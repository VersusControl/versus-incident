package kubernetes

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/kubernetes/breaklogs"
	kubebreaks "github.com/VersusControl/versus-incident/pkg/kubernetes/breaks"
	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
)

type linkedCacheEntry struct {
	key    string
	until  time.Time
	result breaklogs.Result
}

var linkedBreakCache = struct {
	sync.Mutex
	items map[string]*list.Element
	order *list.List
}{items: map[string]*list.Element{}, order: list.New()}

func clearLinkedCache() {
	linkedBreakCache.Lock()
	defer linkedBreakCache.Unlock()
	linkedBreakCache.items = map[string]*list.Element{}
	linkedBreakCache.order.Init()
}

func cachedLinkedResult(key string, now time.Time) (breaklogs.Result, bool) {
	linkedBreakCache.Lock()
	defer linkedBreakCache.Unlock()
	entry := linkedBreakCache.items[key]
	if entry == nil {
		return breaklogs.Result{}, false
	}
	value := entry.Value.(linkedCacheEntry)
	if !value.until.After(now) {
		linkedBreakCache.order.Remove(entry)
		delete(linkedBreakCache.items, key)
		return breaklogs.Result{}, false
	}
	linkedBreakCache.order.MoveToFront(entry)
	value.result.Lines = append([]breaklogs.LinkedLine(nil), value.result.Lines...)
	value.result.Complete = append([]breaklogs.Gap(nil), value.result.Complete...)
	return value.result, true
}

func cacheLinkedResult(key string, result breaklogs.Result, now, ready time.Time) {
	until := now.Add(5 * time.Minute)
	// A pending result must not suppress the panel's first scheduled retry.
	if ready.After(now) {
		until = minTime(ready, now.Add(15*time.Second))
	}
	result.Lines = append([]breaklogs.LinkedLine(nil), result.Lines...)
	result.Complete = append([]breaklogs.Gap(nil), result.Complete...)
	linkedBreakCache.Lock()
	defer linkedBreakCache.Unlock()
	if entry := linkedBreakCache.items[key]; entry != nil {
		linkedBreakCache.order.Remove(entry)
		delete(linkedBreakCache.items, key)
	}
	entry := linkedBreakCache.order.PushFront(linkedCacheEntry{key, until, result})
	linkedBreakCache.items[key] = entry
	for linkedBreakCache.order.Len() > 256 {
		last := linkedBreakCache.order.Back()
		delete(linkedBreakCache.items, last.Value.(linkedCacheEntry).key)
		linkedBreakCache.order.Remove(last)
	}
}

// verifiedBreakPods resolves ownership while the complete prior graph still
// exists. A later missing object never forces us to guess a workload prefix.
func verifiedBreakPods(snapshot kubeindex.Snapshot, retained ...[]breaklogs.ManifestPod) []breaklogs.ManifestPod {
	podState := snapshot.Kinds["Pod"]
	if podState.State != "ready" || podState.Partial {
		return nil
	}
	byUID := map[string]kubeindex.Record{}
	for _, record := range snapshot.Records {
		byUID[record.UID] = record
	}
	pods := []breaklogs.ManifestPod{}
	known := map[string]bool{}
	for _, history := range retained {
		for _, pod := range history {
			known[pod.UID] = true
		}
	}
	for _, record := range snapshot.Records {
		if record.Kind != "Pod" || record.CreatedAt.IsZero() {
			continue
		}
		root := kubebreaks.Root(record, byUID)
		if root.UID == record.UID {
			relevant := known[record.UID] || kubebreaks.Rule(record, breakPodObservationAt(snapshot)) != ""
			for _, container := range record.Containers {
				relevant = relevant || container.RestartCount > 0
			}
			if !relevant {
				continue
			}
		}
		if _, exists := byUID[root.UID]; !exists {
			continue
		}
		valid := true
		cursor := record
		visited := map[string]bool{}
		for cursor.UID != root.UID {
			if visited[cursor.UID] {
				valid = false
				break
			}
			visited[cursor.UID] = true
			var ownerUID string
			for _, owner := range cursor.Owners {
				if owner.Controller {
					ownerUID = owner.UID
					break
				}
			}
			owner, exists := byUID[ownerUID]
			state := snapshot.Kinds[owner.Kind]
			if !exists || owner.Namespace != record.Namespace || state.State != "ready" || state.Partial {
				valid = false
				break
			}
			cursor = owner
		}
		if !valid {
			continue
		}
		pod := breaklogs.ManifestPod{Namespace: record.Namespace, RootUID: root.UID, Name: record.Name, UID: record.UID, Node: record.NodeName, CreatedAt: record.CreatedAt, Broken: kubebreaks.Rule(record, breakPodObservationAt(snapshot)) != ""}
		containers := record.LogContainers
		if len(containers) == 0 {
			containers = record.Containers
		}
		for _, container := range containers {
			if len(pod.Containers) == 20 {
				break
			}
			pod.Containers = append(pod.Containers, breaklogs.ManifestContainer{Name: container.Name, Init: container.Init, Restarts: container.RestartCount, StartedAt: container.StartedAt, PreviousStartedAt: container.PreviousStartedAt, TerminationAt: container.TerminatedAt, TerminationReason: container.TerminatedReason, TerminationExitCode: container.ExitCode, CurrentTermination: container.CurrentTermination})
		}
		pods = append(pods, pod)
	}
	return pods
}

func (service *Service) observeBreakInventory(ctx context.Context, snapshot kubeindex.Snapshot, owner string) {
	if snapshot.Kinds["Pod"].State != "ready" || snapshot.Kinds["Pod"].Partial {
		return
	}
	if store := service.changeStore(); store != nil {
		state, err := store.BreakLogs(breakPodObservationAt(snapshot))
		if err != nil {
			return
		}
		_ = service.commitCredential(ctx, func() error {
			return store.ObserveBreakPods(service.scrubManifestPods(verifiedBreakPods(snapshot, state.PodHistory)), owner, breakPodObservationAt(snapshot))
		})
	}
}

func breakPodObservationAt(snapshot kubeindex.Snapshot) time.Time {
	at := snapshot.Kinds["Pod"].ObservedAt
	if at.IsZero() {
		return time.Now().UTC()
	}
	return at
}

var apiObjectUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func (service *Service) scrubManifestPods(pods []breaklogs.ManifestPod) []breaklogs.ManifestPod {
	if service.scrubber == nil {
		return nil
	}
	result := []breaklogs.ManifestPod{}
	for _, pod := range pods {
		valid := true
		for _, identity := range []string{pod.Namespace, pod.Name} {
			if service.scrubber.Scrub(identity) != identity {
				valid = false
				break
			}
		}
		// API UUIDs are structural identity, not log text. Other identifiers
		// still pass the scrubber so credential-like values cannot be persisted.
		for _, identity := range []string{pod.RootUID, pod.UID} {
			valid = valid && (apiObjectUUID.MatchString(identity) || service.scrubber.Scrub(identity) == identity)
		}
		if !valid {
			continue
		}
		pod.Node = service.scrubber.Scrub(pod.Node)
		containers := []breaklogs.ManifestContainer{}
		for _, container := range pod.Containers {
			if service.scrubber.Scrub(container.Name) == container.Name {
				container.TerminationReason = safeBreakReason(service.scrubber.Scrub(safeBreakReason(container.TerminationReason)))
				containers = append(containers, container)
			}
		}
		pod.Containers = containers
		result = append(result, pod)
	}
	return result
}

func (service *Service) newBreakManifest(ctx context.Context, root kubebreaks.Break, state breaklogs.State, now time.Time) breaklogs.Manifest {
	at := root.BrokeAt
	if at.IsZero() {
		at = now
	}
	manifest := breaklogs.Manifest{ID: breaklogs.ManifestIdentity(root.Root.UID, at), Namespace: root.Root.Namespace, RootUID: root.Root.UID, BreakAt: at, From: at.Add(-5 * time.Minute), To: at.Add(2 * time.Minute), CapturedAt: now, Pods: []breaklogs.ManifestPod{}}
	for _, gap := range state.PodHistoryGaps {
		if gap.Namespace == manifest.Namespace && (gap.RootUID == "" || gap.RootUID == manifest.RootUID) && !gap.To.Before(manifest.From) && !gap.From.After(manifest.To) {
			manifest.Truncated = true
		}
	}
	if service.evidenceStore() != nil {
		lookup, stopLookup := context.WithTimeout(ctx, 500*time.Millisecond)
		contextPage, err := service.historicalIssues(lookup, IssueOptions{RootUID: manifest.RootUID, Namespace: manifest.Namespace, Since: now.Add(-breaklogs.Retention), Until: now, Limit: 100})
		stopLookup()
		if err == nil {
			for _, issue := range contextPage.Items {
				if issue.Rule == root.Rule && issue.Broken && issue.ClosedAt == nil && issue.BrokeAt != nil {
					manifest.OccurrenceID, manifest.ID = issue.OccurrenceID, issue.OccurrenceID
					manifest.BreakAt = *issue.BrokeAt
					manifest.From, manifest.To = manifest.BreakAt.Add(-5*time.Minute), manifest.BreakAt.Add(2*time.Minute)
					break
				}
			}
		}
	}
	for _, pod := range state.PodHistory {
		if pod.Namespace != manifest.Namespace || pod.RootUID != manifest.RootUID || pod.CreatedAt.After(manifest.To) || !pod.DeletedAt.IsZero() && pod.DeletedAt.Before(manifest.From) {
			continue
		}
		if len(manifest.Pods) == breaklogs.MaxManifestPods {
			manifest.Truncated = true
			continue
		}
		manifest.Pods = append(manifest.Pods, pod)
	}
	sort.Slice(manifest.Pods, func(i, j int) bool { return manifest.Pods[i].UID < manifest.Pods[j].UID })
	provider, key := service.logSettingsStore()
	settings, err := breaklogs.LoadSettings(provider, key)
	if err == nil {
		for _, source := range service.configuredLogSources() {
			if source.Name == settings.Settings.BreakLogs.LinkedSource {
				manifest.SettleAt = manifest.To.Add(settings.Settings.BreakLogs.IngestionDelay(source.Type))
				break
			}
		}
	}
	return manifest
}

func manifestQueryPods(state breaklogs.State, query breaklogs.Query) []breaklogs.ManifestPod {
	pods := []breaklogs.ManifestPod{}
	seen := map[string]bool{}
	for _, manifest := range state.Manifests {
		if !matchesBreakManifest(manifest, query) || manifest.To.Before(query.From) || manifest.From.After(query.To) {
			continue
		}
		for _, pod := range manifest.Pods {
			if !seen[pod.UID] {
				pods = append(pods, pod)
				seen[pod.UID] = true
			}
		}
	}
	return pods
}

func matchesBreakManifest(manifest breaklogs.Manifest, query breaklogs.Query) bool {
	if manifest.Namespace != query.Namespace || manifest.RootUID != query.RootUID || !query.Center.IsZero() && !manifest.BreakAt.Equal(query.Center) {
		return false
	}
	return query.OccurrenceID == "" || manifest.OccurrenceID == query.OccurrenceID || manifest.ID == query.OccurrenceID || manifest.OccurrenceID == "" && !query.Center.IsZero()
}

func (service *Service) resolveBreakLogSelection(ctx context.Context, query breaklogs.Query, state breaklogs.State) (breaklogs.Query, error) {
	if query.OccurrenceID == "" && query.Center.IsZero() {
		return query, nil
	}
	var selected *breaklogs.Manifest
	for i := range state.Manifests {
		manifest := &state.Manifests[i]
		if manifest.Namespace != query.Namespace || manifest.RootUID != query.RootUID {
			continue
		}
		match := manifest.BreakAt.Equal(query.Center)
		if query.OccurrenceID != "" {
			match = manifest.OccurrenceID == query.OccurrenceID || manifest.ID == query.OccurrenceID
		}
		if !match {
			continue
		}
		if selected != nil && selected.ID != manifest.ID {
			return query, ErrInvalidArguments
		}
		selected = manifest
	}
	if selected != nil {
		if !query.Center.IsZero() && !query.Center.Equal(selected.BreakAt) {
			return query, ErrInvalidArguments
		}
		if err := service.authorizeEvidenceKind(ctx, query.Namespace, "core~v1~pods"); err != nil {
			return query, err
		}
		query.Center = selected.BreakAt
	} else if service.evidenceStore() != nil {
		resolved, err := service.persistedRootContext(ctx, IssueOptions{Namespace: query.Namespace, RootUID: query.RootUID, OccurrenceID: query.OccurrenceID, Center: query.Center})
		if err != nil {
			return query, err
		}
		if resolved.SelectedOccurrence == nil || resolved.SelectedOccurrence.BrokeAt == nil || !query.Center.IsZero() && !query.Center.Equal(*resolved.SelectedOccurrence.BrokeAt) {
			return query, ErrInvalidArguments
		}
		query.Center = *resolved.SelectedOccurrence.BrokeAt
	} else {
		found := false
		for _, item := range state.Snapshots {
			found = found || query.OccurrenceID == "" && item.RootUID == query.RootUID && item.Namespace == query.Namespace && item.BreakAt.Equal(query.Center)
		}
		if !found {
			return query, ErrInvalidArguments
		}
	}
	if err := query.Validate(); err != nil {
		return query, ErrInvalidArguments
	}
	return query, nil
}

func supplementLinkedPods(manifest []breaklogs.ManifestPod, snapshots []breaklogs.Snapshot, live []kubeindex.Record, observations ...time.Time) []breaklogs.ManifestPod {
	pods := append([]breaklogs.ManifestPod(nil), manifest...)
	positions := map[string]int{}
	liveUIDs := map[string]bool{}
	for _, record := range live {
		liveUIDs[record.UID] = true
	}
	for i := range pods {
		positions[pods[i].UID] = i
		if !liveUIDs[pods[i].UID] && pods[i].DeletedAt.IsZero() {
			pods[i].DeletedAt = pods[i].LastSeen
		}
	}
	for _, item := range snapshots {
		if item.PodCreatedAt.IsZero() {
			continue
		}
		position, exists := positions[item.PodUID]
		if !exists {
			position = len(pods)
			positions[item.PodUID] = position
			pods = append(pods, breaklogs.ManifestPod{Namespace: item.Namespace, RootUID: item.RootUID, Name: item.Pod, UID: item.PodUID, CreatedAt: item.PodCreatedAt, DeletedAt: item.To, LastSeen: item.To})
		}
		pod := &pods[position]
		if pod.LastSeen.Before(item.To) && !liveUIDs[pod.UID] {
			pod.LastSeen, pod.DeletedAt = item.To, item.To
		}
		known := false
		for _, container := range pod.Containers {
			known = known || container.Name == item.Container
		}
		if !known {
			pod.Containers = append(pod.Containers, breaklogs.ManifestContainer{Name: item.Container, Init: item.Init, Restarts: item.Restarts, TerminationAt: item.TerminationAt, TerminationReason: item.TerminationReason, TerminationExitCode: item.TerminationExitCode})
		}
	}
	for _, record := range live {
		position, exists := positions[record.UID]
		if !exists {
			position = len(pods)
			positions[record.UID] = position
			pods = append(pods, breaklogs.ManifestPod{Namespace: record.Namespace, RootUID: "", Name: record.Name, UID: record.UID, CreatedAt: record.CreatedAt})
		}
		pod := &pods[position]
		pod.DeletedAt = time.Time{}
		for _, current := range record.Containers {
			known := false
			for i := range pod.Containers {
				container := &pod.Containers[i]
				known = known || container.Name == current.Name
				// A fresh status can fill missing bounds for the pinned incarnation,
				// but must never replace it with a later restart.
				if container.Name == current.Name && container.Init == current.Init && current.RestartCount == container.Restarts && current.TerminatedAt.Equal(container.TerminationAt) && current.CurrentTermination == container.CurrentTermination {
					if container.StartedAt.IsZero() {
						container.StartedAt = current.StartedAt
					}
					if container.PreviousStartedAt.IsZero() {
						container.PreviousStartedAt = current.PreviousStartedAt
					}
				}
				if container.Name == current.Name && current.RestartCount > container.Restarts {
					end := pod.LastSeen
					if current.RestartCount == container.Restarts+1 && !current.TerminatedAt.IsZero() && !current.TerminatedAt.Before(container.StartedAt) {
						end = current.TerminatedAt
					}
					container.CurrentEndedAt = end
				} else if container.Name == current.Name && len(observations) > 0 && !observations[0].IsZero() && current.RestartCount == container.Restarts {
					container.CurrentEndedAt = observations[0]
				}
			}
			if !known {
				container := breaklogs.ManifestContainer{Name: current.Name, Init: current.Init, Restarts: current.RestartCount, StartedAt: current.StartedAt, PreviousStartedAt: current.PreviousStartedAt, TerminationAt: current.TerminatedAt, CurrentTermination: current.CurrentTermination}
				if len(observations) > 0 {
					container.CurrentEndedAt = observations[0]
				}
				pod.Containers = append(pod.Containers, container)
			}
		}
	}
	return pods
}

// readLinkedBreak returns only scrubbed, lifetime-verified lines. The same
// planner is used by detail and settle; neither path bypasses namespace proof.
func (service *Service) readLinkedBreak(ctx context.Context, query breaklogs.Query, pods []breaklogs.ManifestPod, settings breaklogs.CaptureSettings, source breaklogs.Source, cache bool, versions ...string) ([]BreakLogLine, breaklogs.Result, error) {
	if err := service.validateCredential(ctx); err != nil {
		clearLinkedCache()
		return nil, breaklogs.Result{}, err
	}
	if err := service.authorizeEvidenceKind(ctx, query.Namespace, "core~v1~pods"); err != nil {
		clearLinkedCache()
		return nil, breaklogs.Result{}, err
	}
	linkedQuery := breaklogs.LinkedQuery{Namespace: query.Namespace, From: query.From, To: minTime(query.To, service.observationNow()), Limit: 2000}
	if query.Container != "" && query.Container != breaklogs.AllContainers {
		linkedQuery.Container = query.Container
	}
	names := map[string]bool{}
	prioritized := append([]breaklogs.ManifestPod(nil), pods...)
	sort.Slice(prioritized, func(i, j int) bool {
		left, right := brokenManifestPod(prioritized[i]), brokenManifestPod(prioritized[j])
		if left != right {
			return left
		}
		return prioritized[i].UID < prioritized[j].UID
	})
	for _, pod := range prioritized {
		selected := len(query.Pods) == 0
		for _, name := range query.Pods {
			selected = selected || name == pod.Name || name == pod.UID
		}
		if selected && !names[pod.Name] {
			names[pod.Name] = true
			linkedQuery.Pods = append(linkedQuery.Pods, pod.Name)
		}
	}
	if len(linkedQuery.Pods) == 0 || !linkedQuery.To.After(linkedQuery.From) {
		return nil, breaklogs.Result{}, nil
	}
	// Do not let older, unobserved restarts consume a bounded provider sample.
	// This is a conservative envelope; ResolveLinkedLine still verifies each
	// row against its exact Pod and container incarnation after retrieval.
	var first, last time.Time
	terminatedOnly := true
	for _, pod := range pods {
		if !names[pod.Name] || pod.CreatedAt.IsZero() {
			continue
		}
		for _, container := range pod.Containers {
			if linkedQuery.Container != "" && linkedQuery.Container != container.Name {
				continue
			}
			if !container.CurrentTermination || container.StartedAt.IsZero() || container.TerminationAt.IsZero() {
				terminatedOnly = false
				continue
			}
			add := func(start, end time.Time) {
				if start.IsZero() {
					return
				}
				start = maxTime(start, maxTime(pod.CreatedAt, linkedQuery.From))
				end = minTime(end, linkedQuery.To)
				if !pod.DeletedAt.IsZero() {
					end = minTime(end, pod.DeletedAt)
				}
				if !end.After(start) {
					return
				}
				if first.IsZero() || start.Before(first) {
					first = start
				}
				if end.After(last) {
					last = end
				}
			}
			add(container.StartedAt, breaklogs.TerminationEnd(container.TerminationAt))
		}
	}
	if terminatedOnly && !first.IsZero() {
		linkedQuery.From, linkedQuery.To = first, last
	}
	ready := query.To.Add(settings.IngestionDelay(source.Type))
	raw, _ := json.Marshal([]any{service.scope, service.readOrgs, fmt.Sprintf("%p", service.identity), source.CacheKey, settings, query, pods, versions})
	digest := sha256.Sum256(raw)
	key := hex.EncodeToString(digest[:])
	// Custom readers without a stable configuration fingerprint are not cached.
	cache = cache && source.CacheKey != ""
	result, found := breaklogs.Result{}, false
	if cache {
		result, found = cachedLinkedResult(key, service.observationNow())
	}
	if !found {
		reader, err := source.Build(settings.Fields[source.Type])
		if err != nil {
			return nil, result, err
		}
		started := service.observationNow()
		result, err = breaklogs.ReadWindow(ctx, reader, linkedQuery)
		if healthErr := service.recordLogHealth(ctx, source.Name, started, err); healthErr != nil {
			return nil, result, healthErr
		}
		if err != nil {
			return nil, result, err
		}
		safeLines := []breaklogs.LinkedLine{}
		invalid := false
		for _, line := range result.Lines {
			if !names[line.Pod] || service.scrubber.Scrub(line.Pod) != line.Pod || service.scrubber.Scrub(line.Container) != line.Container {
				invalid = true
				continue
			}
			if _, _, verified := breaklogs.ResolveLinkedLine(pods, line); !verified {
				invalid = true
				continue
			}
			line.Text = service.scrubber.Scrub(line.Text)
			if len(line.Text) > 4096 {
				line.Text = strings.ToValidUTF8(line.Text[:4096], "")
				invalid, result.Truncated = true, true
			}
			safeLines = append(safeLines, line)
		}
		result.Lines = safeLines
		if invalid {
			result.Rejected, result.Complete = true, nil
		}
		if err := service.validateCredential(ctx); err != nil {
			clearLinkedCache()
			return nil, breaklogs.Result{}, err
		}
		if cache {
			cacheLinkedResult(key, result, started, ready)
		}
	}
	lines := []BreakLogLine{}
	seen := map[string]bool{}
	for _, line := range result.Lines {
		if line.At.Before(query.From) || line.At.After(query.To) || !names[line.Pod] || query.Container != "" && query.Container != breaklogs.AllContainers && line.Container != query.Container {
			continue
		}
		pod, container, ok := breaklogs.ResolveLinkedLine(pods, line)
		if !ok {
			continue
		}
		slot := "current"
		if !container.TerminationAt.IsZero() && !line.At.After(breaklogs.TerminationEnd(container.TerminationAt)) {
			slot = "previous"
		}
		if query.Mode != "" && query.Mode != "merged" && query.Mode != slot {
			continue
		}
		text := service.scrubber.Scrub(line.Text)
		id, _ := json.Marshal([]any{pod.UID, container.Name, container.Init, slot, line.At, text})
		if seen[string(id)] {
			continue
		}
		seen[string(id)] = true
		lines = append(lines, BreakLogLine{At: line.At, Pod: pod.Name, PodUID: pod.UID, Container: container.Name, Init: container.Init, Slot: slot, Level: breaklogs.Level(text), Text: text, Source: "linked"})
	}
	if err := service.authorizeEvidenceKind(ctx, query.Namespace, "core~v1~pods"); err != nil {
		clearLinkedCache()
		return nil, breaklogs.Result{}, err
	}
	return lines, result, service.validateCredential(ctx)
}

func brokenManifestPod(pod breaklogs.ManifestPod) bool {
	if !pod.DeletedAt.IsZero() {
		return true
	}
	for _, container := range pod.Containers {
		if container.Restarts > 0 || !container.TerminationAt.IsZero() {
			return true
		}
	}
	return false
}

var breakSettleSlots = make(chan struct{}, 2)

// Demand capture still settles retained breaks. It takes a short capture lease
// only for due jobs; authorized foreground captures can share that local owner.
func (service *Service) runDemandBreakSettles(loop *captureLoop) {
	defer close(loop.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		store := service.changeStore()
		now := time.Now().UTC()
		if store != nil && loop.ctx.Err() == nil {
			due, err := service.dueBreakSettles(now)
			if err == nil && len(due) > 0 {
				err = service.commitCredential(loop.ctx, func() error { return store.ClaimLease(loop.owner, now, 45*time.Second) })
				if err == nil {
					owned, cancel := context.WithTimeout(loop.ctx, 45*time.Second)
					owned = captureReadContext(owned, loop.owner)
					loop.mu.Lock()
					loop.owned, loop.ownedContext = true, owned
					loop.mu.Unlock()
					var running sync.WaitGroup
					service.scheduleBreakSettles(owned, loop.owner, &running)
					running.Wait()
					cancel()
					loop.mu.Lock()
					loop.owned, loop.ownedContext = false, nil
					loop.mu.Unlock()
					_ = store.ReleaseLease(loop.owner)
				}
			}
		}
		select {
		case <-loop.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (service *Service) runBreakSettles(ctx context.Context, owner string, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var running sync.WaitGroup
	defer running.Wait()
	for {
		service.scheduleBreakSettles(ctx, owner, &running)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (service *Service) scheduleBreakSettles(ctx context.Context, owner string, running *sync.WaitGroup) {
	store := service.changeStore()
	if store == nil || service.scrubber == nil || ctx.Err() != nil {
		return
	}
	now := time.Now().UTC()
	due, err := service.dueBreakSettles(now)
	if err != nil {
		return
	}
	for _, item := range due {
		select {
		case breakSettleSlots <- struct{}{}:
		default:
			return
		}
		raw := fmt.Sprintf("%v/%s/%s/%d", service.indexScope(), owner, item.ID, item.Attempts+1)
		digest := sha256.Sum256([]byte(raw))
		token := hex.EncodeToString(digest[:])
		if err := store.AcquireBreakSettleSlot(token, now); err != nil {
			<-breakSettleSlots
			continue
		}
		var claimed breaklogs.Manifest
		err := service.commitCredential(ctx, func() error {
			var claimErr error
			claimed, claimErr = store.ClaimBreakSettle(item.ID, owner, now)
			return claimErr
		})
		if err != nil {
			_ = store.ReleaseBreakSettleSlot(token)
			<-breakSettleSlots
			continue
		}
		running.Add(1)
		go func(manifest breaklogs.Manifest, token string) {
			defer running.Done()
			defer func() { <-breakSettleSlots }()
			defer store.ReleaseBreakSettleSlot(token)
			service.settleBreakLogs(ctx, store, manifest, owner)
		}(claimed, token)
	}
}

// Eligibility is shared by demand admission and the owned scheduler. Checking
// current source/delay before claiming ownership avoids idle lease churn.
func (service *Service) dueBreakSettles(now time.Time) ([]breaklogs.Manifest, error) {
	if service.changeStore() == nil || service.scrubber == nil {
		return nil, nil
	}
	provider, key := service.logSettingsStore()
	settings, settingsErr := breaklogs.LoadSettings(provider, key)
	if settingsErr != nil {
		return nil, settingsErr
	}
	var sourceType string
	for _, source := range service.configuredLogSources() {
		if source.Name == settings.Settings.BreakLogs.LinkedSource {
			sourceType = source.Type
			break
		}
	}
	if sourceType == "" {
		return nil, nil
	}
	state, err := service.changeStore().BreakLogs(now)
	if err != nil {
		return nil, err
	}
	due := []breaklogs.Manifest{}
	for _, item := range state.Manifests {
		if item.Done || item.Attempts >= 2 || item.SettleAt.IsZero() || item.SettleAt.After(now) || item.SettlingUntil.After(now) {
			continue
		}
		if item.To.Add(settings.Settings.BreakLogs.IngestionDelay(sourceType)).After(now) {
			continue
		}
		due = append(due, item)
	}
	return due, nil
}

func (service *Service) settleBreakLogs(parent context.Context, store *kubechanges.Store, manifest breaklogs.Manifest, owner string) {
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, credentialEpochContextKey{}, service)
	provider, key := service.logSettingsStore()
	settings, err := breaklogs.LoadSettings(provider, key)
	if err != nil {
		return
	}
	items := []breaklogs.Snapshot{}
	retry := false
	for _, source := range service.configuredLogSources() {
		if source.Name != settings.Settings.BreakLogs.LinkedSource {
			continue
		}
		query := breaklogs.Query{Namespace: manifest.Namespace, RootUID: manifest.RootUID, From: manifest.From, To: manifest.To, Container: breaklogs.AllContainers, Mode: "merged", OccurrenceID: manifest.OccurrenceID, Center: manifest.BreakAt}
		// Current inventory may tighten a captured lifetime, but cannot invent
		// unseen intervening restarts or resurrect a disappeared UID.
		// Inventory refresh has its own owner/limiter. A settle must not spend
		// its linked-query admission waiting for first-sync LISTs.
		snapshot := kubeindex.Snapshot{}
		if index, indexErr := service.indexes.ForScope(service.indexScope()); indexErr == nil {
			snapshot = index.Snapshot()
		}
		pods := supplementLinkedPods(manifest.Pods, nil, breakLogPods(snapshot.Records, manifest.Namespace, manifest.RootUID), snapshot.Kinds["Pod"].ObservedAt)
		if len(pods) > breaklogs.MaxManifestPods {
			pods = pods[:breaklogs.MaxManifestPods]
			manifest.Truncated = true
		}
		manifest.Pods = pods
		lines, result, readErr := service.readLinkedBreak(ctx, query, pods, settings.Settings.BreakLogs, source, false)
		retry = readErr != nil || result.Truncated || result.Rejected || manifest.Truncated || time.Now().Before(manifest.To.Add(settings.Settings.BreakLogs.IngestionDelay(source.Type)))
		if readErr == nil {
			items = service.linkedBreakSnapshots(manifest, lines, result.Truncated || result.Rejected || manifest.Truncated)
		}
		break
	}
	if err := service.commitCredential(ctx, func() error { return store.CommitBreakSettle(manifest, items, retry, owner, time.Now().UTC()) }); err == nil {
		clearLinkedCache()
	}
}

func (service *Service) linkedBreakSnapshots(manifest breaklogs.Manifest, lines []BreakLogLine, truncated bool) []breaklogs.Snapshot {
	items := map[string]*breaklogs.Snapshot{}
	for _, line := range lines {
		var pod breaklogs.ManifestPod
		var container breaklogs.ManifestContainer
		for _, candidate := range manifest.Pods {
			if candidate.UID != line.PodUID {
				continue
			}
			pod = candidate
			for _, current := range candidate.Containers {
				if current.Name == line.Container {
					container = current
					break
				}
			}
		}
		item := breaklogs.Snapshot{Source: "linked", OccurrenceID: manifest.ID, BreakAt: manifest.BreakAt, Cluster: service.scope.ClusterID, Namespace: manifest.Namespace, RootUID: manifest.RootUID, Pod: line.Pod, PodUID: line.PodUID, PodCreatedAt: pod.CreatedAt, Container: line.Container, Init: line.Init, Slot: line.Slot, Restarts: container.Restarts, TerminationAt: container.TerminationAt, TerminationReason: safeBreakReason(service.scrubber.Scrub(safeBreakReason(container.TerminationReason))), TerminationExitCode: container.TerminationExitCode, From: manifest.From, To: manifest.To, CapturedAt: time.Now().UTC(), Truncated: truncated}
		item.From = maxTime(item.From, pod.CreatedAt)
		if !pod.DeletedAt.IsZero() {
			item.To = minTime(item.To, pod.DeletedAt)
		}
		if line.Slot == "previous" {
			item.To = minTime(item.To, breaklogs.TerminationEnd(container.TerminationAt))
			start := container.PreviousStartedAt
			if container.CurrentTermination {
				start = container.StartedAt
			}
			if !start.IsZero() {
				item.From = maxTime(item.From, start)
			}
		} else if !container.StartedAt.IsZero() {
			item.From = maxTime(item.From, container.StartedAt)
		}
		if line.Slot == "current" && !container.CurrentEndedAt.IsZero() {
			item.To = minTime(item.To, container.CurrentEndedAt)
		}
		item.ID = breaklogs.Identity(item)
		if items[item.ID] == nil {
			items[item.ID] = &item
		}
		target := items[item.ID]
		used := 0
		for _, stored := range target.Lines {
			used += len(stored.Text)
		}
		text := service.scrubber.Scrub(line.Text)
		if len(text) > 4096 || len(target.Lines) == 500 || used+len(text) > 64<<10 {
			target.Truncated = true
			continue
		}
		target.Lines = append(target.Lines, breaklogs.Line{At: line.At, Text: text})
	}
	result := []breaklogs.Snapshot{}
	for _, item := range items {
		result = append(result, *item)
	}
	return result
}
