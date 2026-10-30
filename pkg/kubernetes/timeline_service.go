package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"math/rand"
	"sort"
	"strconv"
	"time"

	"github.com/VersusControl/versus-incident/pkg/core"
	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/google/uuid"
)

const timelineSnapshotSlots = 128
const timelineSnapshotTTL = 10 * time.Minute
const timelineLocalSnapshots = 8
const timelineRequesterSnapshots = 32

var ErrTimelineContention = errors.New("kubernetes: timeline storage contention")
var errTimelinePublicationPending = errors.New("kubernetes: timeline publication pending")

type TimelineQuery struct {
	Since, Until                                  time.Time
	Namespace, Kind, RootUID, PodUID, ResourceUID string
	Limit                                         int
	Cursor                                        string
	// BreaksOnly keeps only break items, so paging, totals and counts cover
	// breaks alone. Cursors are bound to it.
	BreaksOnly bool
}

type TimelineItem struct {
	Type   string                  `json:"type"`
	ID     string                  `json:"id"`
	At     time.Time               `json:"at"`
	Change *Change                 `json:"change,omitempty"`
	Crash  *kubechanges.CrashEvent `json:"crash,omitempty"`
	Issue  *Issue                  `json:"issue,omitempty"`
}

type TimelinePage struct {
	Items      []TimelineItem    `json:"items"`
	Next       string            `json:"next,omitempty"`
	Sync       SyncStatus        `json:"sync"`
	Partial    []PartialFailure  `json:"partial_failures,omitempty"`
	Gaps       []kubechanges.Gap `json:"gaps"`
	CrashCount int               `json:"crash_count"`
	Total      int               `json:"total"`
	Counts     map[string]int    `json:"counts"`
	Since      time.Time         `json:"since"`
	Until      time.Time         `json:"until"`
	BreaksOnly bool              `json:"breaks_only,omitempty"`
}

type timelineSnapshot struct {
	ID, Binding           string
	Equivalent, Requester string
	Expires               time.Time
	Page                  TimelinePage
}

type timelineCursor struct {
	ID, Binding  string
	Slot, Offset int
}

type timelineReservation struct {
	ID, Binding           string
	Equivalent, Requester string
	Expires               time.Time
	Pinned                bool
	Since, Until          time.Time
}

type timelineManifest struct {
	Slots [timelineSnapshotSlots]timelineReservation
}

func timelineBinding(service *Service, query TimelineQuery) string {
	fields := []string{service.cacheKey(), query.Namespace, query.Kind, query.RootUID, query.PodUID, query.ResourceUID}
	if query.BreaksOnly {
		fields = append(fields, "breaks")
	}
	data, _ := json.Marshal(fields)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func (service *Service) timelineKey(slot int) string {
	return "kubernetes/timeline/" + logIdentity(service.cacheKey()) + "/" + strconv.Itoa(slot)
}

func (service *Service) Timeline(ctx context.Context, query TimelineQuery) (TimelinePage, error) {
	if err := service.validateCredential(ctx); err != nil {
		return TimelinePage{}, err
	}
	for _, value := range []string{query.Namespace, query.Kind, query.RootUID, query.PodUID, query.ResourceUID} {
		if value != "" && (!safeSegment(value) || len(value) > 256) {
			return TimelinePage{}, ErrInvalidArguments
		}
	}
	if len(query.Cursor) > 4096 {
		return TimelinePage{}, ErrInvalidArguments
	}

	if service.evidenceStore() != nil {
		now := time.Now().UTC()
		if query.Cursor == "" && query.Until.IsZero() {
			query.Until = now
		}
		if query.Cursor == "" && query.Since.IsZero() {
			query.Since = query.Until.Add(-24 * time.Hour)
		}
		if !query.Since.IsZero() && !query.Until.IsZero() && query.Since.After(query.Until) || query.Until.After(now.Add(time.Minute)) {
			return TimelinePage{}, ErrInvalidArguments
		}
		return service.evidenceTimeline(ctx, query)
	}
	binding := timelineBinding(service, query)
	var snapshot *timelineSnapshot
	cursor := timelineCursor{Binding: binding}
	if query.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(query.Cursor)
		if err != nil || json.Unmarshal(raw, &cursor) != nil || cursor.Binding != binding || cursor.Slot < 0 || cursor.Slot >= timelineSnapshotSlots || !safeSegment(cursor.ID) || cursor.Offset < 0 {
			return TimelinePage{}, ErrInvalidArguments
		}
		if err := service.pinTimeline(ctx, cursor, query); err != nil {
			return TimelinePage{}, err
		}
		service.changes.mu.Lock()
		snapshot = service.changes.timelines[cursor.ID]
		service.changes.mu.Unlock()
		if snapshot == nil {
			provider := service.usageProvider()
			if provider == nil {
				return TimelinePage{}, ErrInvalidArguments
			}
			raw, err := provider.ReadBlob(service.timelineKey(cursor.Slot) + "/" + cursor.ID)
			if err != nil {
				return TimelinePage{}, err
			}
			var stored timelineSnapshot
			if json.Unmarshal(raw, &stored) != nil {
				return TimelinePage{}, ErrInvalidArguments
			}
			snapshot = &stored
		}
		if snapshot.ID != cursor.ID || snapshot.Binding != binding || !snapshot.Expires.After(time.Now()) || !query.Since.IsZero() && !query.Since.Equal(snapshot.Page.Since) || !query.Until.IsZero() && !query.Until.Equal(snapshot.Page.Until) {
			return TimelinePage{}, ErrInvalidArguments
		}
		service.rememberTimeline(snapshot)
	} else {
		now := time.Now().UTC()
		if query.Until.IsZero() {
			query.Until = now
		}
		if query.Since.IsZero() {
			query.Since = query.Until.Add(-24 * time.Hour)
		}
		if query.Since.After(query.Until) || query.Until.Sub(query.Since) > 24*time.Hour || query.Until.After(now.Add(time.Minute)) {
			return TimelinePage{}, ErrInvalidArguments
		}
		page, err := service.buildTimeline(ctx, query)
		if err != nil {
			return page, err
		}
		snapshot = &timelineSnapshot{ID: uuid.NewString(), Binding: binding, Expires: now.Add(timelineSnapshotTTL), Page: page, Requester: logIdentity(core.CallerActor(ctx))}
		if len(page.Items) > normalizePageLimit(query.Limit) {
			generation, err := service.readInventory(ctx)
			if err != nil {
				return TimelinePage{}, err
			}
			stable := page
			stable.Sync.AgeSeconds = 0
			stable.Items = append([]TimelineItem(nil), page.Items...)
			for i, item := range stable.Items {
				if item.Issue != nil {
					copy := *item.Issue
					copy.LastSeen = time.Time{}
					copy.Evidence = nil
					stable.Items[i].Issue = &copy
				}
			}
			version := generation.Revision
			if version == "" {
				raw, _ := json.Marshal(generation.View)
				digest := sha256.Sum256(raw)
				version = hex.EncodeToString(digest[:])
			}
			raw, _ := json.Marshal(struct {
				Binding, Version string
				Page             TimelinePage
			}{binding, version, stable})
			digest := sha256.Sum256(raw)
			snapshot.Equivalent = hex.EncodeToString(digest[:])
			var errStore error
			cursor.Slot, errStore = service.storeTimeline(ctx, snapshot)
			if errStore != nil {
				return TimelinePage{}, errStore
			}
			cursor.ID = snapshot.ID
		} else if err := service.discardTimelinePreview(ctx, binding, snapshot.Requester); err != nil {
			return TimelinePage{}, err
		}
	}
	limit := normalizePageLimit(query.Limit)
	if cursor.Offset > len(snapshot.Page.Items) {
		return TimelinePage{}, ErrInvalidArguments
	}
	end := min(cursor.Offset+limit, len(snapshot.Page.Items))
	page := snapshot.Page
	page.Items = append([]TimelineItem{}, page.Items[cursor.Offset:end]...)
	if end < len(snapshot.Page.Items) && cursor.ID != "" {
		cursor.Offset = end
		raw, _ := json.Marshal(cursor)
		page.Next = base64.RawURLEncoding.EncodeToString(raw)
	}
	if err := service.validateCredential(ctx); err != nil {
		return TimelinePage{}, err
	}
	raw, err := json.Marshal(page)
	if err != nil {
		return TimelinePage{}, err
	}
	var result TimelinePage
	if err := json.Unmarshal(raw, &result); err != nil {
		return TimelinePage{}, err
	}
	return result, nil
}

func (service *Service) editTimelineManifest(ctx context.Context, change func(*timelineManifest) (bool, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	provider := service.usageProvider()
	key := service.timelineKey(0) + "/index"
	if provider == nil {
		service.changes.mu.Lock()
		defer service.changes.mu.Unlock()
		manifest := service.changes.timelineManifests[key]
		if service.changes.timelineManifests == nil {
			service.changes.timelineManifests = map[string]timelineManifest{}
		}
		changed, err := change(&manifest)
		if err == nil && changed {
			service.changes.timelineManifests[key] = manifest
		}
		return err
	}
	cas, ok := provider.(storage.BlobCAS)
	if !ok {
		return ErrInvalidArguments
	}
	for attempt := 0; attempt < 32; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw, err := provider.ReadBlob(key)
		if err != nil {
			return err
		}
		var manifest timelineManifest
		if len(raw) > 0 && json.Unmarshal(raw, &manifest) != nil {
			return ErrInvalidArguments
		}
		changed, err := change(&manifest)
		if errors.Is(err, errTimelinePublicationPending) {
			if err := waitTimelineRetry(ctx, attempt); err != nil {
				return err
			}
			continue
		}
		if err != nil || !changed {
			return err
		}
		replacement, err := json.Marshal(manifest)
		if err != nil {
			return err
		}
		won := false
		err = service.commitCredential(ctx, func() error { var err error; won, err = cas.CompareAndSwapBlob(key, raw, replacement); return err })
		if err != nil {
			return err
		}
		if won {
			return nil
		}
		if err := waitTimelineRetry(ctx, attempt); err != nil {
			return err
		}
	}
	return ErrTimelineContention
}

func waitTimelineRetry(ctx context.Context, attempt int) error {
	delay := min(time.Millisecond*time.Duration(1<<min(attempt, 5)), 25*time.Millisecond)
	timer := time.NewTimer(delay + time.Duration(rand.Int63n(int64(delay))))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (service *Service) pinTimeline(ctx context.Context, cursor timelineCursor, query TimelineQuery) error {
	requester := logIdentity(core.CallerActor(ctx))
	return service.editTimelineManifest(ctx, func(manifest *timelineManifest) (bool, error) {
		entry := manifest.Slots[cursor.Slot]
		if entry.ID != cursor.ID || entry.Binding != cursor.Binding || entry.Requester != requester || !entry.Expires.After(time.Now()) ||
			!query.Since.IsZero() && !query.Since.Equal(entry.Since) || !query.Until.IsZero() && !query.Until.Equal(entry.Until) {
			return false, ErrInvalidArguments
		}
		if entry.Pinned {
			return false, nil
		}
		entry.Pinned = true
		manifest.Slots[cursor.Slot] = entry
		return true, nil
	})
}

type retiredTimeline struct {
	Slot int
	ID   string
}

func (service *Service) cleanupTimeline(retired []retiredTimeline) {
	provider := service.usageProvider()
	for _, old := range retired {
		service.changes.mu.Lock()
		delete(service.changes.timelines, old.ID)
		service.changes.mu.Unlock()
		if provider == nil || old.ID == "" || !safeSegment(old.ID) {
			continue
		}
		key := service.timelineKey(old.Slot) + "/" + old.ID
		var err error
		if deleter, ok := provider.(storage.BlobDeleter); ok {
			err = deleter.DeleteBlob(key)
		} else {
			raw, readErr := provider.ReadBlob(key)
			err = readErr
			if err == nil && len(raw) > 0 {
				_, err = provider.(storage.BlobCAS).CompareAndSwapBlob(key, raw, nil)
			}
		}
		if err != nil {
			log.Printf("kubernetes: obsolete timeline cleanup deferred")
		}
	}
}

func (service *Service) discardTimelinePreview(ctx context.Context, binding, requester string) error {
	retired := []retiredTimeline{}
	err := service.editTimelineManifest(ctx, func(manifest *timelineManifest) (bool, error) {
		retired = nil
		for i, entry := range manifest.Slots {
			if entry.ID != "" && !entry.Pinned && entry.Binding == binding && entry.Requester == requester {
				retired = append(retired, retiredTimeline{i, entry.ID})
				manifest.Slots[i] = timelineReservation{}
			}
		}
		return len(retired) > 0, nil
	})
	if err == nil {
		service.cleanupTimeline(retired)
	}
	return err
}

func (service *Service) storeTimeline(ctx context.Context, snapshot *timelineSnapshot) (int, error) {
	provider := service.usageProvider()
	slot := -1
	retired := []retiredTimeline{}
	var reuse *timelineSnapshot
	err := service.editTimelineManifest(ctx, func(manifest *timelineManifest) (bool, error) {
		slot = -1
		retired = nil
		reuse = nil
		free, preview, equivalent, requesterSlots := -1, -1, -1, 0
		for i, entry := range manifest.Slots {
			if entry.ID == "" || !entry.Expires.After(time.Now()) {
				if free < 0 {
					free = i
				}
				continue
			}
			if entry.Requester != snapshot.Requester {
				continue
			}
			requesterSlots++
			if entry.Binding != snapshot.Binding {
				continue
			}
			if !entry.Pinned && preview < 0 {
				preview = i
			}
			if entry.Equivalent == snapshot.Equivalent {
				equivalent = i
			}
		}
		if equivalent >= 0 {
			entry := manifest.Slots[equivalent]
			if provider == nil {
				reuse = service.changes.timelines[entry.ID]
			} else {
				raw, err := provider.ReadBlob(service.timelineKey(equivalent) + "/" + entry.ID)
				if err != nil {
					return false, err
				}
				var stored timelineSnapshot
				// The reservation precedes its immutable payload. An identical
				// first-page reader waits for publication, never spends another
				// slot or misreports pending IO as pagination capacity exhaustion.
				if len(raw) == 0 {
					return false, errTimelinePublicationPending
				}
				if json.Unmarshal(raw, &stored) != nil || stored.ID != entry.ID || stored.Equivalent != snapshot.Equivalent {
					return false, ErrInvalidArguments
				}
				reuse = &stored
			}
			if reuse == nil {
				return false, ErrIndexStreamBusy
			}
			slot = equivalent
		} else {
			slot = preview
			if slot < 0 {
				slot = free
			}
			if slot < 0 || preview < 0 && requesterSlots >= timelineRequesterSnapshots {
				return false, ErrIndexStreamBusy
			}
		}
		changed := false
		for i, entry := range manifest.Slots {
			if i != slot && entry.ID != "" && !entry.Pinned && entry.Binding == snapshot.Binding && entry.Requester == snapshot.Requester {
				retired = append(retired, retiredTimeline{i, entry.ID})
				manifest.Slots[i] = timelineReservation{}
				changed = true
			}
		}
		if reuse != nil {
			return changed, nil
		}
		if old := manifest.Slots[slot]; old.ID != "" {
			retired = append(retired, retiredTimeline{slot, old.ID})
		}
		manifest.Slots[slot] = timelineReservation{ID: snapshot.ID, Binding: snapshot.Binding, Equivalent: snapshot.Equivalent, Requester: snapshot.Requester, Expires: snapshot.Expires, Since: snapshot.Page.Since, Until: snapshot.Page.Until}
		if provider == nil {
			service.changes.timelines[snapshot.ID] = snapshot
		}
		return true, nil
	})
	if err != nil {
		return 0, err
	}
	if reuse != nil {
		*snapshot = *reuse
	} else if provider != nil {
		data, err := json.Marshal(snapshot)
		if err != nil {
			return 0, err
		}
		if err := service.commitCredential(ctx, func() error { return provider.WriteBlob(service.timelineKey(slot)+"/"+snapshot.ID, data) }); err != nil {
			return 0, err
		}
		current := false
		if err := service.editTimelineManifest(ctx, func(manifest *timelineManifest) (bool, error) {
			current = manifest.Slots[slot].ID == snapshot.ID
			return false, nil
		}); err != nil {
			return 0, err
		}
		if !current {
			service.cleanupTimeline([]retiredTimeline{{slot, snapshot.ID}})
		}
	}
	service.cleanupTimeline(retired)
	service.rememberTimeline(snapshot)
	return slot, nil
}

func (service *Service) rememberTimeline(snapshot *timelineSnapshot) {
	service.changes.mu.Lock()
	defer service.changes.mu.Unlock()
	for id, old := range service.changes.timelines {
		if !old.Expires.After(time.Now()) {
			delete(service.changes.timelines, id)
		}
	}
	limit := timelineLocalSnapshots
	if service.changes.provider == nil {
		limit = timelineSnapshotSlots
	}
	if _, exists := service.changes.timelines[snapshot.ID]; exists {
		return
	}
	if len(service.changes.timelines) >= limit {
		oldestID := ""
		oldest := time.Time{}
		for id, entry := range service.changes.timelines {
			if oldest.IsZero() || entry.Expires.Before(oldest) {
				oldestID, oldest = id, entry.Expires
			}
		}
		delete(service.changes.timelines, oldestID)
	}
	service.changes.timelines[snapshot.ID] = snapshot
}

func (service *Service) buildTimeline(ctx context.Context, query TimelineQuery) (TimelinePage, error) {
	page := TimelinePage{Items: []TimelineItem{}, Gaps: []kubechanges.Gap{}, Counts: map[string]int{"change": 0, "crash": 0, "break": 0}, Since: query.Since, Until: query.Until, BreaksOnly: query.BreaksOnly}
	issues, err := service.issuesSnapshot(ctx, IssueOptions{Namespace: query.Namespace, Broken: true, unpaged: true})
	if err != nil {
		return page, err
	}
	page.Sync, page.Partial = issues.Sync, issues.Partial
	related := map[string]bool{}
	if query.RootUID != "" {
		view, _, err := service.CachedNamespaceSnapshot(ctx, query.Namespace, graphIndexKinds...)
		if err != nil {
			return page, err
		}
		related[query.RootUID] = true
		for _, record := range view.Records {
			if record.UID == query.RootUID {
				for _, node := range directedRelated(view, record.Kind, record.Namespace, record.Name).Nodes {
					related[node.ID] = true
				}
				break
			}
		}
	}
	appendItem := func(item TimelineItem, namespace, kind, uid, root, pod string) {
		if query.BreaksOnly && item.Type != "break" {
			return
		}
		if query.Namespace != "" && namespace != query.Namespace || query.Kind != "" && kind != query.Kind || query.RootUID != "" && root != query.RootUID && !related[uid] || query.PodUID != "" && pod != query.PodUID && !(item.Type == "change" && uid == query.PodUID) || query.ResourceUID != "" && uid != query.ResourceUID && !(item.Type == "crash" && (root == query.ResourceUID || pod == query.ResourceUID)) {
			return
		}
		if item.At.IsZero() || item.At.Before(query.Since) || item.At.After(query.Until) {
			return
		}
		page.Items = append(page.Items, item)
		page.Counts[item.Type]++
	}
	for _, issue := range issues.Items {
		at := issue.FirstSeen
		if issue.BrokeAt != nil {
			at = *issue.BrokeAt
		}
		copy := issue
		pod := ""
		if issue.Root.Kind == "Pod" {
			pod = issue.Root.UID
		}
		appendItem(TimelineItem{Type: "break", ID: "break:" + issue.Root.UID + ":" + issue.Rule, At: at, Issue: &copy}, issue.Root.Namespace, issue.Root.Kind, issue.Root.UID, issue.Root.UID, pod)
	}
	store := service.changeStore()
	if store != nil {
		if len(service.readOrgs) > 1 {
			store, err = kubechanges.NewView(service.usageProvider(), service.indexScope(), service.readOrgs[1:])
			if err != nil {
				return page, err
			}
		}
		changes, crashes, gaps, err := store.TimelineWindow(query.Since, query.Until)
		if err != nil {
			return page, err
		}
		page.Gaps = gaps
		for _, change := range changes {
			copy := change
			appendItem(TimelineItem{Type: "change", ID: "change:" + change.ID, At: change.At, Change: &copy}, change.Namespace, change.Kind, change.UID, change.RootUID, "")
		}
		for _, crash := range crashes {
			copy := crash
			kind := crash.RootKind
			if kind == "" || query.PodUID != "" || query.ResourceUID == crash.PodUID {
				kind = "Pod"
			}
			appendItem(TimelineItem{Type: "crash", ID: "crash:" + crash.ID, At: crash.At, Crash: &copy}, crash.Namespace, kind, crash.PodUID, crash.RootUID, crash.PodUID)
		}
	}
	sort.Slice(page.Items, func(i, j int) bool {
		if page.Items[i].At.Equal(page.Items[j].At) {
			return page.Items[i].ID > page.Items[j].ID
		}
		return page.Items[i].At.After(page.Items[j].At)
	})
	page.Total, page.CrashCount = len(page.Items), page.Counts["crash"]
	return page, nil
}
