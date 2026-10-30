package changes

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

func crashFixture(position int, at time.Time) CrashEvent {
	event := CrashEvent{Cluster: "cluster", Namespace: "shop", Pod: "api", PodUID: "pod-" + strconv.Itoa(position), RootUID: "root", RootKind: "Deployment", RootName: "api", Container: "app", Type: "termination", Reason: "Error", ExitCode: 1, At: at, ObservedAt: at, TimeSource: "finished_at"}
	event.ID = CrashID(event)
	return event
}

func TestChangeValidUsesJournalShape(t *testing.T) {
	change := Change{ID: "synthetic", Cluster: "cluster", Kind: "Secret", Namespace: "shop", Name: "synthetic", UID: "synthetic-uid", Type: Created, Source: "observed", At: time.Now().UTC()}
	if !change.Valid() {
		t.Fatal("complete typed observation was rejected")
	}
	for _, scenario := range []string{"missing-id", "missing-name", "missing-time", "unknown-type", "unknown-source", "invalid-field"} {
		t.Run(scenario, func(t *testing.T) {
			invalid := change
			switch scenario {
			case "missing-id":
				invalid.ID = ""
			case "missing-name":
				invalid.Name = ""
			case "missing-time":
				invalid.At = time.Time{}
			case "unknown-type":
				invalid.Type = "unsupported"
			case "unknown-source":
				invalid.Source = "unsupported"
			case "invalid-field":
				invalid.Fields = []FieldChange{{Path: ""}}
			}
			if invalid.Valid() {
				t.Fatal("incomplete or unsupported journal shape was accepted")
			}
		})
	}
}

func TestCrashJournalPagingDedupAndIsolation(t *testing.T) {
	provider := storage.NewMemory()
	scope := index.Scope{OrgID: "org", ClusterID: "cluster", CredentialID: "principal"}
	store := NewStore(provider, scope)
	now := time.Now().UTC()
	var events []CrashEvent
	for position := range 50 {
		events = append(events, crashFixture(position, now.Add(-time.Minute)))
	}
	coverage := map[string]Gap{"Pod": {From: now.Add(-time.Hour), To: now}}
	if err := store.CommitCapture(nil, nil, coverage, "", now, events, nil, true); err != nil {
		t.Fatal(err)
	}
	store = NewStore(provider, scope)
	query := CrashQuery{RootUID: "root", Namespace: "shop", Limit: 7}
	first, err := store.QueryCrashes(query, now)
	if err != nil || len(first.Items) != 7 || first.Next == "" {
		t.Fatalf("first page: %+v %v", first, err)
	}
	added := crashFixture(99, now.Add(-30*time.Second))
	if err := store.CommitCapture(nil, nil, nil, "", now, append(events, added), nil, true); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	page := first
	for {
		encoded, _ := json.Marshal(page)
		if len(encoded) > maxCrashPageBytes || page.Items == nil || page.Gaps == nil || page.CaptureGaps == nil {
			t.Fatal("unbounded or null page")
		}
		for _, event := range page.Items {
			if ids[event.ID] || event.ID == added.ID {
				t.Fatalf("duplicate or post-snapshot row: %+v", event)
			}
			ids[event.ID] = true
		}
		if page.Next == "" {
			break
		}
		query.Cursor = page.Next
		page, err = store.QueryCrashes(query, now)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(ids) != 50 {
		t.Fatalf("retained %d rows", len(ids))
	}
	for _, changed := range []index.Scope{{OrgID: "other", ClusterID: "cluster", CredentialID: "principal"}, {OrgID: "org", ClusterID: "other", CredentialID: "principal"}, {OrgID: "org", ClusterID: "cluster", CredentialID: "other"}} {
		other := NewStore(provider, changed)
		if _, err := other.QueryCrashes(CrashQuery{Namespace: "shop", RootUID: "root", Cursor: first.Next}, now); !errors.Is(err, ErrInvalidQuery) {
			t.Fatal("cross-scope cursor accepted")
		}
		page, err := other.QueryCrashes(CrashQuery{}, now)
		if err != nil || len(page.Items) != 0 {
			t.Fatal("cross-scope history exposed")
		}
	}
	for _, query := range []CrashQuery{{RootUID: "wrong"}, {Namespace: "wrong"}} {
		page, err := store.QueryCrashes(query, now)
		if err != nil || len(page.Items) != 0 {
			t.Fatal("filter bypass")
		}
	}
	for _, query := range []CrashQuery{{RootUID: "wrong", Namespace: "shop", Cursor: first.Next}, {Cursor: "garbage"}, {Limit: 101}, {Since: now, Until: now.Add(-time.Hour)}, {Since: now.Add(-25 * time.Hour), Until: now}} {
		if _, err := store.QueryCrashes(query, now); !errors.Is(err, ErrInvalidQuery) {
			t.Fatalf("invalid query accepted: %+v %v", query, err)
		}
	}
}

func TestCrashJournalFutureUntilClampsAndPinsCursor(t *testing.T) {
	now := time.Now().UTC()
	store := NewStore(storage.NewMemory(), index.Scope{ClusterID: "cluster"})
	if err := store.CommitCapture(nil, nil, nil, "", now, []CrashEvent{crashFixture(1, now.Add(-time.Minute)), crashFixture(2, now.Add(-2*time.Minute))}, nil, true); err != nil {
		t.Fatal(err)
	}
	future := now.Add(5 * time.Minute)
	first, err := store.QueryCrashes(CrashQuery{Until: future, Limit: 1}, now)
	if err != nil || !first.Until.Equal(now) || !first.Since.Equal(now.Add(-store.retention)) || len(first.Items) != 1 || first.Next == "" {
		t.Fatalf("future until not clamped: %+v %v", first, err)
	}
	if err := store.CommitCapture(nil, nil, nil, "", now, []CrashEvent{crashFixture(3, now.Add(-30*time.Second))}, nil, true); err != nil {
		t.Fatal(err)
	}
	for _, until := range []time.Time{{}, future, first.Until} {
		page, err := store.QueryCrashes(CrashQuery{Until: until, Cursor: first.Next, Limit: 1}, now.Add(time.Minute))
		if err != nil || !page.Until.Equal(first.Until) || !page.Since.Equal(first.Since) || len(page.Items) != 1 || page.Items[0].PodUID != "pod-2" || page.Next != "" {
			t.Fatalf("cursor bounds/snapshot changed: %+v %v", page, err)
		}
	}
	page, err := store.QueryCrashes(CrashQuery{Since: future.Add(-store.retention), Until: future}, now)
	if err != nil || !page.Until.Equal(now) || !page.Since.Equal(future.Add(-store.retention)) {
		t.Fatalf("explicit browser window rejected: %+v %v", page, err)
	}
	for _, query := range []CrashQuery{{Since: now.Add(time.Second), Until: future}, {Since: now.Add(-store.retention - time.Second), Until: future}, {Until: future.Add(time.Second), Cursor: first.Next}} {
		if _, err := store.QueryCrashes(query, now); !errors.Is(err, ErrInvalidQuery) {
			t.Fatalf("invalid clamped window accepted: %+v %v", query, err)
		}
	}
}

func TestCrashJournalObservationWindowAndCursorPreserveEventTime(t *testing.T) {
	now := time.Now().UTC()
	store := NewStore(storage.NewMemory(), index.Scope{ClusterID: "cluster"})
	var events []CrashEvent
	for position := range 3 {
		event := crashFixture(position, now.Add(-48*time.Hour-time.Duration(position)*time.Minute))
		event.ObservedAt = now.Add(time.Duration(position-2) * time.Minute)
		events = append(events, event)
	}
	if err := store.CommitCapture(nil, nil, nil, "", now, events, nil, true); err != nil {
		t.Fatal(err)
	}
	query := CrashQuery{Since: now.Add(-time.Minute), Until: now, Namespace: "shop", RootUID: "root", Limit: 1}
	first, err := store.QueryCrashes(query, now)
	if err != nil || len(first.Items) != 1 || first.Items[0].ID != events[2].ID || !first.Items[0].At.Equal(events[2].At) || first.Items[0].TimeSource != "finished_at" || !first.Items[0].ObservedAt.Equal(now) || first.RetentionBasis != "observed_at" || first.Next == "" {
		t.Fatalf("retained old termination hidden or event time changed: %+v %v", first, err)
	}
	added := crashFixture(99, now.Add(-48*time.Hour))
	added.ObservedAt = now.Add(-30 * time.Second)
	if err := store.CommitCapture(nil, nil, nil, "", now, []CrashEvent{added}, nil, true); err != nil {
		t.Fatal(err)
	}
	query.Cursor = first.Next
	second, err := store.QueryCrashes(query, now.Add(time.Minute))
	if err != nil || len(second.Items) != 1 || second.Items[0].ID != events[1].ID || !second.Items[0].At.Equal(events[1].At) || second.Next != "" || !second.Since.Equal(first.Since) || !second.Until.Equal(first.Until) {
		t.Fatalf("observation cursor lost bounds or admitted post-snapshot row: %+v %v", second, err)
	}
	for _, changed := range []CrashQuery{
		{Since: query.Since.Add(-time.Second), Until: query.Until, Namespace: query.Namespace, RootUID: query.RootUID, Cursor: query.Cursor},
		{Since: query.Since, Until: query.Until.Add(time.Second), Namespace: query.Namespace, RootUID: query.RootUID, Cursor: query.Cursor},
	} {
		if _, err := store.QueryCrashes(changed, now.Add(time.Minute)); !errors.Is(err, ErrInvalidQuery) {
			t.Fatalf("changed observation cursor bounds accepted: %+v %v", changed, err)
		}
	}
	page, err := store.QueryCrashes(CrashQuery{Since: now.Add(-2 * time.Minute), Until: now.Add(-time.Minute - time.Nanosecond)}, now)
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != events[0].ID {
		t.Fatalf("observation since/until filtering failed: %+v %v", page, err)
	}
	page, err = store.QueryCrashes(CrashQuery{}, now.Add(25*time.Hour))
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("expired observation returned: %+v %v", page, err)
	}
	if err := store.CommitCapture(nil, nil, nil, "", now.Add(25*time.Hour), nil, nil, true); err != nil {
		t.Fatal(err)
	}
	current, err := store.read()
	if err != nil || len(current.Crashes) != 0 {
		t.Fatalf("expired observations retained after commit: %+v %v", current, err)
	}
}

func TestCrashJournalArchivePaginationAndUnknownTerminationIdentity(t *testing.T) {
	provider := storage.NewMemory()
	scope := index.Scope{OrgID: "default", ClusterID: "cluster"}
	archive := NewStore(provider, scope)
	now := time.Now().UTC()
	first, second := crashFixture(0, now.Add(-time.Minute)), crashFixture(1, now.Add(-2*time.Minute))
	if err := archive.CommitCapture(nil, nil, nil, "", now, []CrashEvent{first, second}, nil, true); err != nil {
		t.Fatal(err)
	}
	scope.OrgID = "licensed"
	view, err := NewView(provider, scope, []string{"default"})
	if err != nil {
		t.Fatal(err)
	}
	page, err := view.QueryCrashes(CrashQuery{Limit: 1}, now)
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != first.ID || page.Next == "" {
		t.Fatalf("archive first: %+v %v", page, err)
	}
	if err := view.CommitCapture(nil, nil, nil, "", now, []CrashEvent{second}, nil, true); err != nil {
		t.Fatal(err)
	}
	page, err = view.QueryCrashes(CrashQuery{Limit: 1, Cursor: page.Next}, now)
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != second.ID || page.Next != "" {
		t.Fatalf("new primary shadowed snapshot archive: %+v %v", page, err)
	}
	unknown := crashFixture(99, now)
	unknown.TimeSource, unknown.TerminationSlot = "observed_at", "current"
	currentID := CrashID(unknown)
	unknown.TerminationSlot = "previous"
	if CrashID(unknown) == currentID {
		t.Fatal("unknown current/previous termination conflated")
	}
	unknown.TimeSource = "finished_at"
	previousID := CrashID(unknown)
	unknown.TerminationSlot = "current"
	unknown.RestartCount = 9
	if CrashID(unknown) != previousID {
		t.Fatal("known termination duplicated by slot or lifetime counter")
	}
}

func TestCrashJournalAtomicFencingAndRetention(t *testing.T) {
	provider := storage.NewMemory()
	scope := index.Scope{ClusterID: "cluster"}
	first, second := NewStore(provider, scope), NewStore(provider, scope)
	now := time.Now().UTC()
	if err := first.ClaimLease("leader", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	event := crashFixture(0, now)
	coverage := map[string]Gap{"Pod": {From: now, To: now}}
	if err := second.CommitCapture(nil, nil, coverage, "stale", now, []CrashEvent{event}, nil, true); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale write: %v", err)
	}
	page, err := second.QueryCrashes(CrashQuery{}, now)
	if err != nil || len(page.Items) != 0 || len(page.Coverage) != 0 || !page.StartedAt.IsZero() {
		t.Fatalf("non-atomic stale commit: %+v %v", page, err)
	}
	var workers sync.WaitGroup
	errorsFound := make(chan error, 2)
	for _, writer := range []*Store{first, second} {
		workers.Add(1)
		go func() {
			defer workers.Done()
			errorsFound <- writer.CommitCapture(nil, nil, coverage, "leader", now, []CrashEvent{event}, nil, true)
		}()
	}
	workers.Wait()
	for range 2 {
		if err := <-errorsFound; err != nil {
			t.Fatal(err)
		}
	}
	if err := first.ReleaseLease("leader"); err != nil {
		t.Fatal(err)
	}
	if err := second.ClaimLease("successor", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := second.CommitCapture(nil, nil, coverage, "successor", now, []CrashEvent{event}, nil, true); err != nil {
		t.Fatal(err)
	}
	page, err = second.QueryCrashes(CrashQuery{}, now)
	if err != nil || len(page.Items) != 1 || page.Items[0].Sequence != 1 {
		t.Fatalf("HA duplicate: %+v %v", page, err)
	}
	page, err = second.QueryCrashes(CrashQuery{}, now.Add(25*time.Hour))
	if err != nil || len(page.Items) != 0 {
		t.Fatal("expired observations returned")
	}
}

func TestCrashJournalPageByteBudget(t *testing.T) {
	store := NewStore(storage.NewMemory(), index.Scope{ClusterID: "cluster"})
	now := time.Now().UTC()
	var events []CrashEvent
	for position := range 100 {
		event := crashFixture(position, now)
		event.RootName = strings.Repeat("r", 253)
		event.RootUID = strings.Repeat("u", 256)
		event.Pod = strings.Repeat("p", 253)
		event.Container = strings.Repeat("c", 63)
		event.ID = CrashID(event)
		events = append(events, event)
	}
	if err := store.CommitCapture(nil, nil, nil, "", now, events, nil, true); err != nil {
		t.Fatal(err)
	}
	query := CrashQuery{Limit: 100}
	seen := map[string]bool{}
	for {
		page, err := store.QueryCrashes(query, now)
		encoded, _ := json.Marshal(page)
		if err != nil || len(encoded) > maxCrashPageBytes || len(page.Items) == 0 || len(page.Items) >= 100 {
			t.Fatalf("byte page rows=%d bytes=%d err=%v", len(page.Items), len(encoded), err)
		}
		for _, event := range page.Items {
			if seen[event.ID] {
				t.Fatal("byte paging duplicated row")
			}
			seen[event.ID] = true
		}
		if page.Next == "" {
			break
		}
		query.Cursor = page.Next
	}
	if len(seen) != 100 {
		t.Fatal("byte paging lost rows")
	}
}

func TestCrashJournalBoundsAndSafeFields(t *testing.T) {
	store := NewStore(storage.NewMemory(), index.Scope{ClusterID: "cluster"})
	now := time.Now().UTC()
	var history []Change
	for position := range 2500 {
		history = append(history, Change{ID: "change-" + strconv.Itoa(position), Cluster: "cluster", Kind: "Deployment", Name: "api", Type: ImageChanged, At: now, Fields: []FieldChange{{Path: "image", To: strings.Repeat("i", 1024)}}})
	}
	if err := store.Append(history, now); err != nil {
		t.Fatal(err)
	}
	var events []CrashEvent
	for position := range maxCrashEvents + 1 {
		event := crashFixture(position, now.Add(-48*time.Hour))
		event.ObservedAt = now
		events = append(events, event)
	}
	if err := store.CommitCapture(nil, nil, nil, "", now, events, nil, true); err != nil {
		t.Fatal(err)
	}
	current, err := store.read()
	if err != nil || len(current.Crashes) == 0 || len(current.Crashes) >= len(events) || len(current.CrashGaps) == 0 || encodedSize(current) > maxStoreBytes || len(current.Changes) != len(history) {
		t.Fatalf("missing bounds/disclosure: retained=%d gaps=%+v %v", len(current.Crashes), current.CrashGaps, err)
	}
	page, err := store.QueryCrashes(CrashQuery{Since: now.Add(-time.Minute), Until: now}, now)
	if err != nil || len(page.Items) == 0 || len(page.Gaps) == 0 {
		t.Fatalf("observation-window overflow falsely complete: %+v %v", page, err)
	}
	for _, gap := range page.Gaps {
		if !gap.From.Equal(now) || !gap.To.Equal(now) {
			t.Fatalf("overflow gap used event time instead of observation: %+v", gap)
		}
	}
	for _, mutate := range []func(*CrashEvent){func(event *CrashEvent) { event.Reason = "CANARY raw message" }, func(event *CrashEvent) { event.Signal = 65 }, func(event *CrashEvent) { event.RestartCount = -1 }, func(event *CrashEvent) { event.At = now.Add(time.Second) }} {
		event := crashFixture(99, now)
		mutate(&event)
		event.ID = CrashID(event)
		if err := store.CommitCapture(nil, nil, nil, "", now, []CrashEvent{event}, nil, true); !errors.Is(err, ErrInvalidQuery) {
			t.Fatal("unsafe crash accepted")
		}
	}
}

func TestUpgradeTimelineVisible(t *testing.T) {
	provider := storage.NewMemory()
	now := time.Now().UTC()
	scope := index.Scope{OrgID: "default", ClusterID: "cluster", CredentialID: "credential"}
	archive := NewStore(provider, scope)
	old := Change{ID: "shared", Cluster: "cluster", Kind: "Pod", Name: "before", Type: Created, At: now.Add(-time.Minute)}
	if err := archive.Append([]Change{old}, now); err != nil {
		t.Fatal(err)
	}
	if err := archive.MarkGap(Gap{From: now.Add(-time.Hour), To: now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	before, err := provider.ReadBlob(archive.key)
	if err != nil {
		t.Fatal(err)
	}
	scope.OrgID = "licensed"
	view, err := NewView(provider, scope, []string{"default"})
	if err != nil {
		t.Fatal(err)
	}
	newer := old
	newer.Name = "after"
	added := old
	added.ID = "new"
	added.Name = "new-pod"
	added.At = now
	if err := view.Append([]Change{newer, added}, now); err != nil {
		t.Fatal(err)
	}
	page, err := view.Query(Query{Limit: 1}, now)
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != "new" || page.Next == "" {
		t.Fatalf("page=%+v err=%v", page, err)
	}

	page, err = view.Query(Query{Limit: 1, Cursor: page.Next}, now)
	if err != nil || len(page.Items) != 1 || page.Items[0].Name != "after" || page.Next != "" || len(page.Gaps) != 1 {
		t.Fatalf("deduplicated page=%+v err=%v", page, err)
	}
	page, err = view.Query(Query{Name: "before"}, now)
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("filter before dedup: %+v %v", page, err)
	}
	after, err := provider.ReadBlob(archive.key)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("archive changed")
	}
	for _, changed := range []index.Scope{{OrgID: "licensed", ClusterID: "renamed", CredentialID: "credential"}, {OrgID: "licensed", ClusterID: "cluster", CredentialID: "different"}} {
		isolated, err := NewView(provider, changed, []string{"default"})
		if err != nil {
			t.Fatal(err)
		}
		page, err := isolated.Query(Query{}, now)
		if err != nil || len(page.Items) != 0 {
			t.Fatalf("identity isolation=%+v %v", page, err)
		}
	}
	if _, err := NewView(provider, scope, []string{"default", "foreign"}); !errors.Is(err, ErrStoreLimit) {
		t.Fatal("third scope accepted")
	}
	if err := provider.WriteBlob(archive.key, []byte("malformed")); err != nil {
		t.Fatal(err)
	}
	if _, err := view.Query(Query{}, now); !errors.Is(err, ErrStoreLimit) {
		t.Fatal("malformed archive was hidden")
	}
}

func TestDetectProjectedChangesWithoutSensitiveFields(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	old := index.Record{UID: "uid-a", Kind: "Deployment", Namespace: "shop", Name: "checkout", Images: []string{"checkout:v1"}, Replicas: 2, Labels: map[string]string{"app.kubernetes.io/name": "checkout"}}
	updated := old
	updated.Images = []string{"checkout:v2"}
	updated.Replicas = 3
	change, ok := Detect("cluster-a", index.Delta{Kind: "Deployment", Op: "upsert", Old: &old, New: &updated}, now)
	if !ok || change.Type != ImageChanged || change.Service != "checkout" || len(change.Fields) != 2 || change.Fields[0].From != "checkout:v1" || change.Fields[1].To != "3" {
		t.Fatalf("detected change=%#v ok=%v", change, ok)
	}
	encoded, err := json.Marshal(change)
	if err != nil || strings.Contains(string(encoded), "password") || strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "env") {
		t.Fatalf("change contains non-projected data: %s err=%v", encoded, err)
	}
	if _, ok := Detect("cluster-a", index.Delta{Resync: true}, now); ok {
		t.Fatal("resync marker was converted into a data change")
	}
}

func TestDetectRolloutLifecycleAndConfigVersion(t *testing.T) {
	now := time.Now().UTC()
	old := index.Record{Kind: "Deployment", UID: "root", Name: "api", Generation: 1, Revision: "42", Images: []string{"api:v1"}}
	updated := old
	updated.Generation = 2
	updated.Images = []string{"api:v2"}
	items := DetectAll("cluster", index.Delta{Old: &old, New: &updated}, now)
	seen := map[Type]bool{}
	for _, item := range items {
		seen[item.Type] = true
	}
	if !seen[ImageChanged] || !seen[RolloutStarted] || items[0].ID == items[1].ID {
		t.Fatalf("multi-type delta: %+v", items)
	}
	config := index.Record{Kind: "ConfigMap", UID: "cfg", Name: "cfg", ResourceVersion: "1"}
	newer := config
	newer.ResourceVersion = "2"
	items = DetectAll("cluster", index.Delta{Old: &config, New: &newer}, now)
	if len(items) != 1 || items[0].Type != ConfigChanged || items[0].Fields[0].From != "1" || items[0].Fields[0].To != "2" {
		t.Fatalf("config version: %+v", items)
	}
}

func TestReconstructRevisionTypesAndDecoderCanaries(t *testing.T) {
	now := time.Now().UTC()
	images, err := DecodeRevision(strings.NewReader(`{"spec":{"template":{"metadata":{"annotations":{"private":"CANARY_ANNOTATION"}},"spec":{"containers":[{"name":"app","image":"api:v2","env":[{"value":"CANARY_ENV"}],"args":["CANARY_ARGS"]}],"volumes":[{"secret":{"secretName":"CANARY_VOLUME"}}]}}}}`))
	if err != nil || len(images) != 1 || images[0] != "app=api:v2" {
		t.Fatalf("decoder: %+v %v", images, err)
	}
	root := index.Record{UID: "root", Kind: "StatefulSet", Name: "api", Namespace: "shop"}
	previous := index.Record{UID: "rev1", Kind: "ControllerRevision", Name: "rev1", Namespace: "shop", Revision: "1", CreatedAt: now.Add(-10 * time.Minute), Images: []string{"app=api:v1"}, Owners: []index.OwnerRef{{UID: "root", Kind: "StatefulSet", Name: "api"}}}
	current := previous
	current.UID = "rev2"
	current.Revision = "2"
	current.CreatedAt = now.Add(-5 * time.Minute)
	current.Images = images
	items := Reconstruct("cluster", []index.Record{root, previous, current}, now)
	if len(items) != 2 || items[0].ID == items[1].ID {
		t.Fatalf("revision types collided: %+v", items)
	}
	encoded, _ := json.Marshal(items)
	if strings.Contains(string(encoded), "CANARY") {
		t.Fatal("template values leaked")
	}
	if _, err := DecodeRevision(strings.NewReader(`{"spec":{}}`)); err == nil {
		t.Fatal("empty template accepted")
	}
}

func TestPostgresCaptureDataCoverageAndStaleCASFencing(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	first, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	scope := index.Scope{OrgID: "capture-integration", ClusterID: strconv.FormatInt(time.Now().UnixNano(), 10), CredentialID: "shared"}
	leader, follower := NewStore(first, scope), NewStore(second, scope)
	t.Cleanup(func() {
		current, readErr := first.ReadBlob(leader.key)
		if readErr != nil {
			t.Error(readErr)
			return
		}
		if swapped, cleanupErr := first.(storage.BlobCAS).CompareAndSwapBlob(leader.key, current, nil); cleanupErr != nil || !swapped {
			t.Errorf("cleanup: swapped=%v err=%v", swapped, cleanupErr)
		}
	})
	now := time.Now().UTC()
	if err := leader.ClaimLease("old-owner", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	staleBytes, err := first.ReadBlob(leader.key)
	if err != nil {
		t.Fatal(err)
	}
	if err := follower.ClaimLease("new-owner", now.Add(2*time.Minute), time.Minute); err != nil {
		t.Fatal(err)
	}
	coverage := map[string]Gap{"Pod": {From: now.Add(-time.Minute), To: now}}
	change := Change{ID: "current", Cluster: scope.ClusterID, Kind: "Pod", UID: "pod", Name: "pod", Type: Created, At: now}
	if err := follower.Commit([]Change{change}, nil, coverage, "new-owner", now); err != nil {
		t.Fatal(err)
	}
	before, err := second.ReadBlob(follower.key)
	if err != nil {
		t.Fatal(err)
	}
	stale := change
	stale.ID = "stale"
	staleCoverage := map[string]Gap{"Pod": {From: now.Add(-time.Hour), To: now.Add(time.Hour)}}
	for _, operation := range []struct {
		name string
		run  func() error
	}{
		{"data-and-coverage", func() error { return leader.Commit([]Change{stale}, nil, staleCoverage, "old-owner", now) }},
		{"coverage-only", func() error { return leader.Commit(nil, nil, staleCoverage, "old-owner", now) }},
		{"renewal", func() error { return leader.RenewLease("old-owner", now, time.Minute) }},
		{"foreground-unowned", func() error { return leader.Commit([]Change{stale}, nil, staleCoverage, "", now) }},
	} {
		t.Run(operation.name, func(t *testing.T) {
			if err := operation.run(); !errors.Is(err, ErrLeaseLost) {
				t.Fatalf("stale operation admitted: %v", err)
			}
		})
	}
	if swapped, err := first.(storage.BlobCAS).CompareAndSwapBlob(leader.key, staleBytes, staleBytes); err != nil || swapped {
		t.Fatalf("delayed raw CAS admitted: swapped=%v err=%v", swapped, err)
	}
	after, err := second.ReadBlob(follower.key)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("stale completion changed data or coverage")
	}
	for _, dimension := range []string{"org", "cluster", "credential"} {
		t.Run("isolate-"+dimension, func(t *testing.T) {
			isolatedScope := scope
			switch dimension {
			case "org":
				isolatedScope.OrgID += "-other"
			case "cluster":
				isolatedScope.ClusterID += "-other"
			case "credential":
				isolatedScope.CredentialID += "-restricted"
			}
			isolated := NewStore(second, isolatedScope)
			page, err := isolated.Query(Query{}, now)
			if err != nil || isolated.key == follower.key || len(page.Items) != 0 || len(page.Coverage) != 0 {
				t.Fatalf("scope inherited history or coverage: items=%d coverage=%d err=%v", len(page.Items), len(page.Coverage), err)
			}
		})
	}
	t.Run("reconstruction-observed-restart-dedup", func(t *testing.T) {
		root := index.Record{Kind: "Deployment", UID: "root", Name: "api", Namespace: "shop"}
		previous := index.Record{Kind: "ReplicaSet", UID: "rs1", Name: "rs1", Namespace: "shop", Revision: "1", CreatedAt: now.Add(-time.Hour), Images: []string{"api:v1"}, Owners: []index.OwnerRef{{Kind: "Deployment", UID: "root", Name: "api"}}}
		current := previous
		current.UID, current.Name, current.Revision, current.CreatedAt, current.Images = "rs2", "rs2", "2", now.Add(-time.Minute), []string{"api:v2"}
		reconstructed := Reconstruct(scope.ClusterID, []index.Record{root, previous, current}, now)
		for range 3 {
			if err := follower.Commit(reconstructed, nil, nil, "new-owner", now); err != nil {
				t.Fatal(err)
			}
		}
		beforeObserved, err := follower.Query(Query{Kind: "Deployment"}, now)
		if err != nil || len(beforeObserved.Items) != 2 {
			t.Fatalf("repeated reconstruction: records=%d err=%v", len(beforeObserved.Items), err)
		}
		oldRoot := root
		oldRoot.Images = previous.Images
		root.Images, root.Revision = current.Images, current.Revision
		observed := DetectAll(scope.ClusterID, index.Delta{Old: &oldRoot, New: &root}, now)
		if err := follower.Commit(observed, nil, nil, "new-owner", now); err != nil {
			t.Fatal(err)
		}
		freshProvider, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
		if err != nil {
			t.Fatal(err)
		}
		defer freshProvider.Close()
		fresh := NewStore(freshProvider, scope)
		afterObserved, err := fresh.Query(Query{Kind: "Deployment"}, now)
		if err != nil || len(afterObserved.Items) != 2 {
			t.Fatalf("fresh PG client duplicated reconstruction: records=%d err=%v", len(afterObserved.Items), err)
		}
		originalIDs := make(map[string]bool, len(beforeObserved.Items))
		for _, item := range beforeObserved.Items {
			originalIDs[item.ID] = true
		}
		for _, item := range afterObserved.Items {
			if !originalIDs[item.ID] || (item.Type == ImageChanged && item.Source != "observed") {
				t.Fatal("observed evidence did not replace reconstructed record under the same ID")
			}
		}
	})
}

func TestConcurrentStoresAndCaptureLeaseFencing(t *testing.T) {
	provider := storage.NewMemory()
	scope := index.Scope{ClusterID: "cluster"}
	now := time.Now().UTC()
	var workers sync.WaitGroup
	for position := range 20 {
		workers.Add(1)
		go func(position int) {
			defer workers.Done()
			store := NewStore(provider, scope)
			change := Change{ID: strconv.Itoa(position), Kind: "Pod", Name: "pod", Cluster: "cluster", Type: Created, At: now}
			if err := store.Append([]Change{change}, now); err != nil {
				t.Errorf("concurrent append: %v", err)
			}
		}(position)
	}
	workers.Wait()
	first, second := NewStore(provider, scope), NewStore(provider, scope)
	page, err := first.Query(Query{Limit: 100}, now)
	if err != nil || len(page.Items) != 20 {
		t.Fatalf("lost multi-instance changes: %d %v", len(page.Items), err)
	}
	if err := first.ClaimLease("first", now, 90*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := second.ClaimLease("second", now, 90*time.Second); !errors.Is(err, ErrLeaseLost) {
		t.Fatal("second owner acquired live lease")
	}
	if err := first.RenewLease("first", now.Add(30*time.Second), 90*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := second.ClaimLease("second", now.Add(3*time.Minute), 90*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := first.Commit(nil, nil, map[string]Gap{"Pod": {From: now, To: now}}, "first", now); !errors.Is(err, ErrLeaseLost) {
		t.Fatal("stale owner write was not fenced")
	}
	if err := first.RenewLease("first", now.Add(3*time.Minute), 90*time.Second); !errors.Is(err, ErrLeaseLost) {
		t.Fatal("old owner renewed after takeover")
	}
	if err := first.Commit(nil, nil, map[string]Gap{"Pod": {From: now, To: now}}, "", now); !errors.Is(err, ErrLeaseLost) {
		t.Fatal("unowned write bypassed capture lease")
	}
}

func TestDemandCommitClearsExpiredLease(t *testing.T) {
	provider := storage.NewMemory()
	testDemandCommitClearsExpiredLease(t, provider)
}

func testDemandCommitClearsExpiredLease(t *testing.T, provider storage.Provider) {
	t.Helper()
	store := NewStore(provider, index.Scope{ClusterID: "cluster"})
	now := time.Now().UTC()
	if err := store.ClaimLease("expired", now.Add(-2*time.Minute), time.Minute); err != nil {
		t.Fatal(err)
	}
	change := Change{ID: "demand", Kind: "Pod", Name: "api", Cluster: "cluster", Type: Created, At: now}
	if err := store.Append([]Change{change}, now); err != nil {
		t.Fatalf("expired lease blocked demand commit: %v", err)
	}
	data, err := provider.ReadBlob(store.key)
	current, decodeErr := decodeSnapshot(data)
	if err != nil || decodeErr != nil || current.Lease != nil || len(current.Changes) != 1 {
		t.Fatal("demand commit did not atomically clear expired lease")
	}
	if err := store.Commit(nil, nil, nil, "expired", now); !errors.Is(err, ErrLeaseLost) {
		t.Fatal("expired owner was not fenced")
	}
	if err := NewStore(provider, index.Scope{ClusterID: "cluster"}).ClaimLease("active", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := store.Append([]Change{change}, now); !errors.Is(err, ErrLeaseLost) {
		t.Fatal("active takeover did not fence demand commit")
	}
	t.Log("expired_owner=expired demand_owner=null rows=1 active_takeover=active stale_demand=rejected")
}

type takeoverCASProvider struct {
	storage.Provider
	storage.BlobCAS
	once     sync.Once
	takeover func()
}

func (provider *takeoverCASProvider) CompareAndSwapBlob(key string, expected, replacement []byte) (bool, error) {
	provider.once.Do(provider.takeover)
	return provider.BlobCAS.CompareAndSwapBlob(key, expected, replacement)
}

func TestDemandExpiredLeaseCASRetryFencesTakeover(t *testing.T) {
	base := storage.NewMemory()
	testDemandExpiredLeaseCASRetryFencesTakeover(t, base)
}

func testDemandExpiredLeaseCASRetryFencesTakeover(t *testing.T, base storage.Provider) {
	t.Helper()
	scope := index.Scope{ClusterID: "cluster"}
	takeover := NewStore(base, scope)
	now := time.Now().UTC()
	if err := takeover.ClaimLease("expired", now.Add(-2*time.Minute), time.Minute); err != nil {
		t.Fatal(err)
	}
	var successorBytes []byte
	provider := &takeoverCASProvider{Provider: base, BlobCAS: base.(storage.BlobCAS), takeover: func() {
		if err := takeover.ClaimLease("active", time.Now().UTC(), time.Minute); err != nil {
			t.Fatal(err)
		}
		var readErr error
		successorBytes, readErr = base.ReadBlob(takeover.key)
		if readErr != nil {
			t.Fatal(readErr)
		}
	}}
	store := NewStore(provider, scope)
	change := Change{ID: "must-not-commit", Cluster: "cluster", Kind: "Pod", Name: "api", Type: Created, At: now}
	if err := store.Commit([]Change{change}, nil, map[string]Gap{"Pod": {From: now.Add(-time.Hour), To: now.Add(time.Hour)}}, "", now); !errors.Is(err, ErrLeaseLost) {
		t.Fatal("CAS retry erased an active takeover")
	}
	data, err := base.ReadBlob(store.key)
	current, decodeErr := decodeSnapshot(data)
	if err != nil || decodeErr != nil || current.Lease == nil || current.Lease.Owner != "active" || len(current.Changes) != 0 {
		t.Fatal("demand write escaped takeover fence")
	}
	if !bytes.Equal(successorBytes, data) {
		t.Fatal("stale CAS changed successor data or coverage")
	}
	t.Log("expired_owner=expired successor_owner=active stale_data_and_coverage=rejected successor_bytes_unchanged=true rows=0")
}

func TestReplayChangeIdentityAndObservedUpsert(t *testing.T) {
	now := time.Now().UTC()
	provider := storage.NewMemory()
	scope := index.Scope{ClusterID: "cluster"}
	first, restarted := NewStore(provider, scope), NewStore(provider, scope)
	for _, delta := range []index.Delta{
		{New: &index.Record{Kind: "Pod", UID: "pod", Name: "api", CreatedAt: now.Add(-time.Hour), Owners: []index.OwnerRef{{UID: "owner", Kind: "ReplicaSet"}}}},
		{Old: &index.Record{Kind: "Pod", UID: "deleted", Name: "old"}},
		{Old: &index.Record{Kind: "ConfigMap", UID: "cfg", Name: "cfg", ResourceVersion: "1"}, New: &index.Record{Kind: "ConfigMap", UID: "cfg", Name: "cfg", ResourceVersion: "2"}},
		{Old: &index.Record{Kind: "HorizontalPodAutoscaler", UID: "hpa", Name: "hpa", Generation: 1}, New: &index.Record{Kind: "HorizontalPodAutoscaler", UID: "hpa", Name: "hpa", Generation: 2}},
	} {
		initial := DetectContinuous("cluster", delta, now.Add(-time.Minute))
		replayed := DetectContinuous("cluster", delta, now)
		if len(initial) == 0 || len(initial) != len(replayed) {
			t.Fatal("missing replayed changes")
		}
		for position := range initial {
			if initial[position].ID != replayed[position].ID {
				t.Fatal("replayed upstream evidence changed identity")
			}
		}
		if err := first.Append(initial, now); err != nil {
			t.Fatal(err)
		}
		if err := restarted.Append(replayed, now); err != nil {
			t.Fatal(err)
		}
	}
	page, err := restarted.Query(Query{}, now)
	if err != nil || len(page.Items) != 4 {
		t.Fatalf("duplicate replay history: items=%d err=%v", len(page.Items), err)
	}
	root := index.Record{Kind: "Deployment", UID: "root", Name: "api", Namespace: "shop"}
	previous := index.Record{Kind: "ReplicaSet", UID: "rs1", Name: "rs1", Namespace: "shop", Revision: "1", CreatedAt: now.Add(-time.Hour), Images: []string{"api:v1"}, Owners: []index.OwnerRef{{Kind: "Deployment", UID: "root", Name: "api"}}}
	current := previous
	current.UID, current.Name, current.Revision, current.CreatedAt, current.Images = "rs2", "rs2", "2", now.Add(-time.Minute), []string{"api:v2"}
	reconstructed := Reconstruct("cluster", []index.Record{root, previous, current}, now)
	if err := first.Append(reconstructed, now); err != nil {
		t.Fatal(err)
	}
	oldRoot := root
	oldRoot.Images = previous.Images
	root.Images, root.Revision = current.Images, current.Revision
	observed := DetectAll("cluster", index.Delta{Old: &oldRoot, New: &root}, now)
	if len(observed) != 1 || observed[0].Type != ImageChanged {
		t.Fatal("missing observed rollout")
	}
	if err := restarted.Append(observed, now); err != nil {
		t.Fatal(err)
	}
	page, err = restarted.Query(Query{Kind: "Deployment"}, now)
	if err != nil || len(page.Items) != 2 {
		t.Fatal("reconstruction duplicated observed rollout")
	}
	for _, item := range page.Items {
		if item.Type == ImageChanged && item.Source != "observed" {
			t.Fatal("observed evidence did not replace reconstruction")
		}
	}
}

func TestReconstructRolloutFallbackRequiresEvidenceTime(t *testing.T) {
	now := time.Now().UTC()
	root := index.Record{UID: "root", Kind: "StatefulSet", Name: "api", Namespace: "shop", CurrentRevision: "old", UpdateRevision: "new"}
	if items := Reconstruct("cluster", []index.Record{root}, now); len(items) != 0 {
		t.Fatalf("invented rollout time: %+v", items)
	}
	root.ProgressingTransitionAt = now.Add(-time.Minute)
	items := Reconstruct("cluster", []index.Record{root}, now)
	if len(items) != 1 || !items[0].At.Equal(root.ProgressingTransitionAt) {
		t.Fatalf("fallback evidence time: %+v", items)
	}
	previous := index.Record{UID: "old", Kind: "ControllerRevision", Name: "old", Namespace: "shop", Revision: "1", CreatedAt: now.Add(-10 * time.Minute), Images: []string{"api:v1"}, Owners: []index.OwnerRef{{UID: "root", Kind: "StatefulSet", Name: "api"}}}
	current := previous
	current.UID, current.Name, current.Revision, current.CreatedAt, current.Images = "new", "new", "2", now.Add(-5*time.Minute), []string{"api:v2"}
	items = Reconstruct("cluster", []index.Record{root, previous, current}, now)
	if len(items) != 2 {
		t.Fatalf("duplicate fallback with revision history: %+v", items)
	}
	for _, item := range items {
		if item.Source != "reconstructed" || !item.At.Equal(current.CreatedAt) {
			t.Fatalf("incorrect revision evidence: %+v", item)
		}
	}
	current.Summary = map[string]string{"revision_history_partial": "true"}
	root.ProgressingTransitionAt = time.Time{}
	if items = Reconstruct("cluster", []index.Record{root, previous, current}, now); len(items) != 0 {
		t.Fatalf("omitted revision became a change: %+v", items)
	}
}

func TestWarningBurstAcrossObjectsAndBoundedImages(t *testing.T) {
	now := time.Now().UTC().Truncate(5 * time.Minute).Add(time.Minute)
	records := []index.Record{}
	for position := range 5 {
		records = append(records, index.Record{Kind: "Event", UID: strconv.Itoa(position), Name: "warning", EventType: "Warning", EventCount: 1, EventFirstAt: now.Add(-time.Minute), EventLastAt: now, Target: &index.OwnerRef{Kind: "Pod", Name: "api", UID: "pod"}})
	}
	items := Reconstruct("cluster", records, now)
	if len(items) != 1 || items[0].Type != WarningBurst {
		t.Fatalf("distinct warning burst lost: %+v", items)
	}
	previous := index.Record{Kind: "Deployment", Name: "api", UID: "root", Images: []string{strings.Repeat("a", 512), strings.Repeat("b", 512), strings.Repeat("c", 512)}}
	current := previous
	current.Images = []string{"api:v2"}
	change, ok := Detect("cluster", index.Delta{Old: &previous, New: &current}, now)
	if !ok || !change.FieldsTruncated || !validChange(change) || len(change.Fields[0].From) > 1024 {
		t.Fatalf("image field poisoned store: %+v", change)
	}
}

func TestContinuousLifecycleCoalescesWithoutEvictingSubstantiveChanges(t *testing.T) {
	now := time.Now().UTC().Truncate(5 * time.Minute).Add(time.Minute)
	store := NewStore(storage.NewMemory(), index.Scope{ClusterID: "cluster"})
	config := index.Record{Kind: "ConfigMap", UID: "config", Name: "config", ResourceVersion: "1"}
	updated := config
	updated.ResourceVersion = "2"
	items := DetectAll("cluster", index.Delta{Old: &config, New: &updated}, now.Add(-time.Minute))
	hpa := index.Record{Kind: "HorizontalPodAutoscaler", UID: "hpa", Name: "hpa", Generation: 1}
	newHPA := hpa
	newHPA.Generation = 2
	items = append(items, DetectAll("cluster", index.Delta{Old: &hpa, New: &newHPA}, now.Add(-time.Minute))...)
	for position := range maxChanges + 1 {
		pod := index.Record{Kind: "Pod", UID: "pod-" + strconv.Itoa(position), Name: "pod", Owners: []index.OwnerRef{{UID: "controller-" + strconv.Itoa(position/2), Kind: "ReplicaSet", Name: "rs"}}}
		delta := index.Delta{New: &pod}
		demand := DetectAll("cluster", delta, now)
		continuous := DetectContinuous("cluster", delta, now)
		if len(demand) != 1 || demand[0].Coalesced || len(continuous) != 1 || !continuous[0].Coalesced {
			t.Fatal("demand lifecycle compatibility or continuous coalescing lost")
		}
		items = append(items, continuous...)
	}
	if err := store.Append(items, now); err != nil {
		t.Fatal(err)
	}
	page, err := store.Query(Query{Limit: 500}, now)
	if err != nil || len(page.Items) != maxLifecycleSamples+2 || len(page.Gaps) != 0 {
		t.Fatalf("lifecycle sample cap: items=%d gaps=%v err=%v", len(page.Items), page.Gaps, err)
	}
	seen := map[Type]bool{}
	for _, item := range page.Items {
		seen[item.Type] = true
	}
	if !seen[ConfigChanged] || !seen[SpecChanged] {
		t.Fatal("lifecycle churn displaced config or HPA changes")
	}
	event := index.Record{Kind: "Event", UID: "event", Name: "event", EventType: "Warning", EventCount: 1}
	if items := DetectAll("cluster", index.Delta{New: &event}, now); len(items) != 0 {
		t.Fatalf("warning event lifecycle noise: %+v", items)
	}
}

func TestWarningBurstStableIdentityAndCountUpdates(t *testing.T) {
	now := time.Now().UTC().Truncate(5 * time.Minute).Add(time.Minute)
	store := NewStore(storage.NewMemory(), index.Scope{ClusterID: "cluster"})
	event := index.Record{Kind: "Event", UID: "event", Name: "warning", EventType: "Warning", EventCount: 5, EventFirstAt: now.Add(-time.Minute), EventLastAt: now, Target: &index.OwnerRef{UID: "pod", Kind: "Pod", Name: "api"}}
	initial := DetectAll("cluster", index.Delta{New: &event}, now)
	if len(initial) != 1 || initial[0].Type != WarningBurst {
		t.Fatalf("initial burst: %+v", initial)
	}
	for position := range 50 {
		previous := event
		event.EventCount += 5
		event.EventLastAt = now.Add(time.Duration(position+1) * time.Second)
		items := DetectAll("cluster", index.Delta{Old: &previous, New: &event}, event.EventLastAt)
		if len(items) != 1 || items[0].ID != initial[0].ID {
			t.Fatalf("growing burst changed identity: %+v", items)
		}
		items = append(items, Reconstruct("cluster", []index.Record{event}, event.EventLastAt)...)
		if err := store.Append(items, event.EventLastAt); err != nil {
			t.Fatal(err)
		}
	}
	page, err := store.Query(Query{}, event.EventLastAt)
	if err != nil || len(page.Items) != 1 || warningCount(page.Items[0]) != 255 || !page.Items[0].At.Equal(event.EventLastAt) || page.Items[0].Source != "observed" {
		t.Fatalf("burst dedup/count update: %+v %v", page, err)
	}
	if err := store.Append(initial, event.EventLastAt); err != nil {
		t.Fatal(err)
	}
	page, _ = store.Query(Query{}, event.EventLastAt)
	if warningCount(page.Items[0]) != 255 {
		t.Fatal("stale burst lowered count")
	}
}

func TestWarningBurstOutOfOrderObservationsPreserveCountAndTime(t *testing.T) {
	testWarningBurstOutOfOrderObservationsPreserveCountAndTime(t, storage.NewMemory())
}

func testWarningBurstOutOfOrderObservationsPreserveCountAndTime(t *testing.T, provider storage.Provider) {
	t.Helper()
	now := time.Now().UTC().Truncate(5 * time.Minute).Add(2 * time.Minute)
	store := NewStore(provider, index.Scope{ClusterID: "cluster"})
	event := index.Record{Kind: "Event", UID: "newer-event", Name: "warning", EventType: "Warning", EventCount: 5, EventFirstAt: now.Add(-time.Minute), EventLastAt: now, Target: &index.OwnerRef{UID: "pod", Kind: "Pod", Name: "api"}}
	initial := DetectAll("cluster", index.Delta{New: &event}, now)
	if err := store.Append(initial, now); err != nil {
		t.Fatal(err)
	}
	event.UID, event.EventLastAt = "earlier-event", now.Add(-30*time.Second)
	earlier := DetectAll("cluster", index.Delta{New: &event}, now)
	if err := store.Append(earlier, now); err != nil {
		t.Fatal(err)
	}
	page, err := store.Query(Query{}, now)
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != initial[0].ID || warningCount(page.Items[0]) != 10 || !page.Items[0].At.Equal(now) {
		t.Fatalf("out-of-order aggregate lost count, time or identity: %+v %v", page, err)
	}
	if err := store.Append(earlier, now); err != nil {
		t.Fatal(err)
	}
	page, err = store.Query(Query{}, now)
	if err != nil || len(page.Items) != 1 || warningCount(page.Items[0]) != 10 || !page.Items[0].At.Equal(now) {
		t.Fatalf("replay changed aggregate: %+v %v", page, err)
	}
}

func TestWarningBurstSlowAccumulatedDeltasAndLaterWindow(t *testing.T) {
	testWarningBurstSlowAccumulatedDeltasAndLaterWindow(t, storage.NewMemory(), time.Minute)
}

func testWarningBurstSlowAccumulatedDeltasAndLaterWindow(t *testing.T, provider storage.Provider, interval time.Duration) {
	t.Helper()
	start := time.Now().UTC().Truncate(5 * time.Minute).Add(-10 * time.Minute)
	scope := index.Scope{ClusterID: "cluster"}
	event := index.Record{Kind: "Event", UID: "same-event", Name: "warning", EventType: "Warning", EventCount: 1, EventFirstAt: start, EventLastAt: start, Target: &index.OwnerRef{UID: "pod", Kind: "Pod", Name: "api"}}
	for position := range 5 {
		previous := event
		event.EventCount = int32(position + 1)
		event.EventLastAt = start.Add(time.Duration(position) * interval)
		delta := index.Delta{Old: &previous, New: &event}
		if position == 0 {
			delta.Old = nil
		}
		store := NewStore(provider, scope)
		if err := store.Append(DetectAll("cluster", delta, event.EventLastAt), event.EventLastAt); err != nil {
			t.Fatal(err)
		}
		page, err := store.Query(Query{}, event.EventLastAt)
		if err != nil || position < 4 && len(page.Items) != 0 || position == 4 && (len(page.Items) != 1 || warningCount(page.Items[0]) != 5) {
			t.Fatalf("slow warning count at %d: %+v %v", position, page, err)
		}
	}
	store := NewStore(provider, scope)
	oldPage, _ := store.Query(Query{}, event.EventLastAt)
	first := oldPage.Items[0]
	for position := range 5 {
		previous := event
		event.EventCount++
		event.EventLastAt = start.Add(5*time.Minute + time.Duration(position)*interval)
		items := DetectAll("cluster", index.Delta{Old: &previous, New: &event}, event.EventLastAt)
		items = append(items, Reconstruct("cluster", []index.Record{event}, event.EventLastAt)...)
		if err := NewStore(provider, scope).Append(items, event.EventLastAt); err != nil {
			t.Fatal(err)
		}
	}
	page, err := store.Query(Query{}, event.EventLastAt)
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("later window lost: %+v %v", page, err)
	}
	for _, item := range page.Items {
		if warningCount(item) != 5 {
			t.Fatal("lifetime count leaked into window")
		}
		if item.ID == first.ID && !item.At.Equal(first.At) {
			t.Fatal("later window overwrote earlier burst")
		}
	}
	if page.Items[0].ID == page.Items[1].ID {
		t.Fatal("different windows reused ID")
	}
	if err := NewStore(provider, scope).Append(DetectAll("cluster", index.Delta{New: &event}, event.EventLastAt), event.EventLastAt); err != nil {
		t.Fatal(err)
	}
	page, err = NewStore(provider, scope).Query(Query{}, event.EventLastAt)
	if err != nil || len(page.Items) != 2 {
		t.Fatal("replay duplicated warning windows")
	}
	retained := false
	for _, item := range page.Items {
		if item.ID == first.ID {
			retained = item.At.Equal(first.At) && warningCount(item) == 5
		}
		if warningCount(item) != 5 {
			t.Fatal("replay inflated bucket count")
		}
	}
	if !retained {
		t.Fatal("first warning bucket was not retained")
	}
	t.Logf("interval=%s rows_before=1 rows_after=2 first_id=%s later_id=%s counts=5,5", interval, first.ID, page.Items[0].ID)
}

func TestPostgresDemandLeaseAndWarningWindows(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set")
	}
	for _, test := range []struct {
		name string
		run  func(*testing.T, storage.Provider)
	}{
		{"expired-clear", testDemandCommitClearsExpiredLease},
		{"cas-takeover", testDemandExpiredLeaseCASRetryFencesTakeover},
		{"out-of-order", testWarningBurstOutOfOrderObservationsPreserveCountAndTime},
		{"five-in-five-minutes", func(t *testing.T, provider storage.Provider) {
			testWarningBurstSlowAccumulatedDeltasAndLaterWindow(t, provider, time.Minute)
		}},
		{"five-in-thirty-seconds", func(t *testing.T, provider storage.Provider) {
			testWarningBurstSlowAccumulatedDeltasAndLaterWindow(t, provider, 7500*time.Millisecond)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
			if err != nil {
				t.Fatal(err)
			}
			defer provider.Close()
			store := NewStore(provider, index.Scope{ClusterID: "cluster"})
			defer func() {
				data, readErr := provider.ReadBlob(store.key)
				if readErr != nil {
					t.Error(readErr)
					return
				}
				if swapped, cleanupErr := provider.(storage.BlobCAS).CompareAndSwapBlob(store.key, data, nil); cleanupErr != nil || !swapped {
					t.Errorf("cleanup swapped=%v err=%v", swapped, cleanupErr)
				}
			}()
			test.run(t, provider)
			if strings.HasPrefix(test.name, "five-") {
				second, openErr := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
				if openErr != nil {
					t.Fatal(openErr)
				}
				defer second.Close()
				page, queryErr := NewStore(second, index.Scope{ClusterID: "cluster"}).Query(Query{}, time.Now().UTC())
				if queryErr != nil || len(page.Items) != 2 || warningCount(page.Items[0]) != 5 || warningCount(page.Items[1]) != 5 {
					t.Fatal("fresh PG reader lost warning bucket checkpoints")
				}
				t.Logf("fresh_pg_reader_rows=2 first_id=%s later_id=%s counts=5,5", page.Items[1].ID, page.Items[0].ID)
			}
		})
	}
}

func TestWarningBurstBoundaryReconstructionAndReplay(t *testing.T) {
	boundary := time.Now().UTC().Truncate(5 * time.Minute)
	store := NewStore(storage.NewMemory(), index.Scope{ClusterID: "cluster"})
	event := index.Record{Kind: "Event", UID: "event", Name: "warning", EventType: "Warning", EventCount: 5, EventFirstAt: boundary.Add(-time.Minute), EventLastAt: boundary.Add(-time.Second), Target: &index.OwnerRef{UID: "pod", Kind: "Pod", Name: "api"}}
	initial := DetectAll("cluster", index.Delta{New: &event}, event.EventLastAt)
	initial = append(initial, Reconstruct("cluster", []index.Record{event}, event.EventLastAt)...)
	if len(initial) != 2 || initial[0].ID != initial[1].ID {
		t.Fatal("observed/reconstructed IDs differ")
	}
	if err := store.Append(initial, event.EventLastAt); err != nil {
		t.Fatal(err)
	}
	previous := event
	event.EventCount++
	event.EventLastAt = boundary
	atBoundary := DetectAll("cluster", index.Delta{Old: &previous, New: &event}, boundary)
	if err := store.Append(atBoundary, boundary); err != nil {
		t.Fatal(err)
	}
	page, _ := store.Query(Query{}, boundary)
	if len(page.Items) != 1 || page.Items[0].ID != initial[0].ID || warningCount(page.Items[0]) != 5 {
		t.Fatal("boundary sample changed older burst")
	}
	previous = event
	event.EventCount += 4
	event.EventLastAt = boundary.Add(time.Minute)
	second := DetectAll("cluster", index.Delta{Old: &previous, New: &event}, event.EventLastAt)
	if len(Reconstruct("cluster", []index.Record{event}, event.EventLastAt)) != 0 {
		t.Fatal("cross-window lifetime count reconstructed as a burst")
	}
	for range 2 {
		if err := store.Append(append(second, initial...), event.EventLastAt); err != nil {
			t.Fatal(err)
		}
	}
	page, err := store.Query(Query{}, event.EventLastAt)
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("replayed window duplicated: %+v %v", page, err)
	}
	for _, item := range page.Items {
		if warningCount(item) != 5 {
			t.Fatal("replay inflated window count")
		}
	}
}

func TestStoreAppendFilterCursorScopeAndRetention(t *testing.T) {
	provider := storage.NewMemory()
	scope := index.Scope{OrgID: "org-a", ClusterID: "cluster-a", CredentialID: "cred-a"}
	store := NewStore(provider, scope)
	other := NewStore(provider, index.Scope{OrgID: "org-b", ClusterID: "cluster-a", CredentialID: "cred-a"})
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	for sequence := 0; sequence < 3; sequence++ {
		change := Change{ID: strconv.Itoa(sequence), Cluster: "cluster-a", Kind: "Deployment", Namespace: "shop", Name: "checkout", UID: "uid-a", Type: SpecChanged, At: now.Add(time.Duration(sequence) * time.Minute)}
		if sequence == 1 {
			change.Kind = "Pod"
		}
		if err := store.Append([]Change{change}, now.Add(time.Duration(sequence)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	page, err := store.Query(Query{Kind: "Deployment", Limit: 1}, now.Add(3*time.Minute))
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != "2" || page.Next == "" {
		t.Fatalf("filtered page=%#v err=%v", page, err)
	}
	page, err = store.Query(Query{Kind: "Deployment", Limit: 1, Cursor: page.Next}, now.Add(3*time.Minute))
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != "0" {
		t.Fatalf("cursor page=%#v err=%v", page, err)
	}
	if page, err := other.Query(Query{}, now.Add(3*time.Minute)); err != nil || len(page.Items) != 0 {
		t.Fatalf("other organization read changes: page=%#v err=%v", page, err)
	}
	store.retention = time.Hour
	page, err = store.Query(Query{}, now.Add(3*time.Hour))
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("expired timeline page=%#v err=%v", page, err)
	}
	if err := store.Append([]Change{{ID: "bad", Cluster: "c", Kind: "Pod", Name: "p", Type: "unknown", At: now}}, now); err != ErrInvalidQuery {
		t.Fatalf("invalid change error=%v", err)
	}
}

type writeCountingProvider struct {
	storage.Provider
	writes int
}

func (provider *writeCountingProvider) WriteBlob(name string, data []byte) error {
	provider.writes++
	return provider.Provider.WriteBlob(name, data)
}

func TestStoreAppendSkipsWritesWithoutChangesOrPruning(t *testing.T) {
	provider := &writeCountingProvider{Provider: storage.NewMemory()}
	store := NewStore(provider, index.Scope{OrgID: "org", ClusterID: "cluster"})
	now := time.Now().UTC()
	change := Change{ID: "one", Cluster: "cluster", Kind: "Pod", Name: "pod", Type: Created, At: now}
	if err := store.Append([]Change{change}, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Append([]Change{change}, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(nil, now); err != nil {
		t.Fatal(err)
	}
	if provider.writes != 1 {
		t.Fatalf("writes=%d, want one initial snapshot write", provider.writes)
	}
	if err := store.Append(nil, now.Add(25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if provider.writes != 2 {
		t.Fatalf("writes=%d after retention pruning, want 2", provider.writes)
	}
	gap := Gap{From: now.Add(26 * time.Hour), To: now.Add(26*time.Hour + time.Second)}
	if err := store.MarkGap(gap); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkGap(Gap{From: gap.From.Add(time.Millisecond), To: gap.To.Add(-time.Millisecond)}); err != nil {
		t.Fatal(err)
	}
	if provider.writes != 3 {
		t.Fatalf("writes=%d after duplicate gap, want 3", provider.writes)
	}
}

func TestStoreCapsRecordCountAndReportsEvictionGap(t *testing.T) {
	store := NewStore(storage.NewMemory(), index.Scope{OrgID: "org", ClusterID: "cluster"})
	now := time.Now().UTC()
	changes := make([]Change, maxChanges+1)
	for sequence := range changes {
		changes[sequence] = Change{ID: strconv.Itoa(sequence), Cluster: "cluster", Kind: "Pod", Name: "pod", UID: "uid", Type: Created, At: now.Add(time.Duration(sequence) * time.Millisecond)}
	}
	if err := store.Append(changes, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	page, err := store.Query(Query{Limit: 500}, now.Add(time.Hour))
	if err != nil || len(page.Items) != 500 || len(page.Gaps) == 0 || page.Next == "" {
		t.Fatalf("bounded page=%#v err=%v", page, err)
	}
}

func TestStoreMarksTimelineGap(t *testing.T) {
	store := NewStore(storage.NewMemory(), index.Scope{OrgID: "org", ClusterID: "cluster"})
	now := time.Now().UTC()
	gap := Gap{From: now, To: now.Add(time.Minute)}
	if err := store.MarkGap(gap); err != nil {
		t.Fatal(err)
	}
	page, err := store.Query(Query{Since: now, Until: now.Add(time.Minute)}, now.Add(time.Minute))
	if err != nil || len(page.Gaps) != 1 || !page.Gaps[0].From.Equal(gap.From) {
		t.Fatalf("gap page=%#v err=%v", page, err)
	}
	if err := store.MarkGap(Gap{From: now.Add(time.Minute), To: now}); err != ErrInvalidQuery {
		t.Fatalf("invalid gap error=%v", err)
	}
}

func TestCaptureStateDoesNotReadOrRewriteLegacyTimelineEvidence(t *testing.T) {
	provider := storage.NewMemory()
	scope := index.Scope{OrgID: "org", ClusterID: "cluster", CredentialID: "credential"}
	now := time.Now().UTC()
	legacy := NewStore(provider, scope)
	change := Change{ID: "old", Cluster: "cluster", Kind: "Pod", Namespace: "shop", UID: "pod", Name: "pod", Type: Created, At: now}
	if err := legacy.Append([]Change{change}, now); err != nil {
		t.Fatal(err)
	}
	before, err := provider.ReadBlob(legacy.key)
	if err != nil {
		t.Fatal(err)
	}
	current := NewCaptureState(provider, scope)
	if current == nil || current.key == legacy.key {
		t.Fatal("capture controls reused the old history key")
	}
	if err = current.ClaimLease("owner", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	page, err := current.ReadWindow(now.Add(-time.Hour), now.Add(time.Hour), now)
	if err != nil || len(page.Items) != 0 {
		t.Fatal("capture control state imported old history")
	}
	after, err := provider.ReadBlob(legacy.key)
	if err != nil || string(before) != string(after) {
		t.Fatal("new capture controls rewrote legacy history")
	}
	if NewCaptureState(nil, scope) != nil || NewCaptureState(provider, index.Scope{}) != nil {
		t.Fatal("invalid capture control scope accepted")
	}
}
