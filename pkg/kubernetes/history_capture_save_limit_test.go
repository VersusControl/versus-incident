package kubernetes

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// lockedLog collects log output written from any goroutine.
type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) lines(pattern *regexp.Regexp) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var found []string
	for _, line := range strings.Split(l.buf.String(), "\n") {
		if pattern.MatchString(line) {
			found = append(found, line)
		}
	}
	return found
}

func collectLog(t *testing.T) *lockedLog {
	t.Helper()
	collected := &lockedLog{}
	previous := log.Writer()
	log.SetOutput(collected)
	t.Cleanup(func() { log.SetOutput(previous) })
	return collected
}

var captureTrimLine = regexp.MustCompile(`kubernetes: history capture over the save limit, kept newest offered=(\d+) kept=(\d+) dropped=(\d+) dropped_from=(\S+) dropped_to=(\S+) dropped_changes=(\d+) dropped_pod_churn=(\d+)$`)

func limitCrash(pod kubeindex.Record, kind string, at, observed time.Time, restarts int32) kubechanges.CrashEvent {
	crash := kubechanges.CrashEvent{Cluster: "cluster", Namespace: pod.Namespace, Pod: pod.Name, PodUID: pod.UID, RootUID: pod.UID, RootKind: "Pod", RootName: pod.Name, Container: "app", Type: kind, RestartCount: restarts, At: at, ObservedAt: observed}
	if kind == "termination" {
		crash.Reason, crash.ExitCode, crash.TimeSource = "Error", 1, "finished_at"
	} else {
		crash.WaitingReason, crash.TimeSource = "CrashLoopBackOff", "observed_at"
	}
	crash.ID = kubechanges.CrashID(crash)
	return crash
}

// limitGeneration is one capture of a cluster holding pods Pods named by
// prefix and one crash-looping Pod. Every kind is listed at at, except Pods,
// listed at podAt.
func limitGeneration(prefix string, pods int, at, podAt time.Time, crashes []kubechanges.CrashEvent) inventoryGeneration {
	kinds := map[string]kubeindex.KindStatus{}
	for kind := range indexedResourceIDs {
		kinds[kind] = kubeindex.KindStatus{State: "ready", ObservedAt: at, CompletedAt: at}
	}
	kinds["Pod"] = kubeindex.KindStatus{State: "ready", ObservedAt: podAt, CompletedAt: podAt}
	records := []kubeindex.Record{
		{UID: "physical", Kind: "Namespace", Name: "kube-system"},
		limitLoopingPod(),
	}
	for i := 0; i < pods; i++ {
		records = append(records, memoryPod(fmt.Sprintf("%s-%05d", prefix, i), i))
	}
	return inventoryGeneration{Revision: at.Format(time.RFC3339Nano), View: kubeindex.Snapshot{Kinds: kinds, Records: records}, Crashes: crashes, PodSampledAt: at}
}

func limitLoopingPod() kubeindex.Record {
	return kubeindex.Record{UID: "loop-pod", Kind: "Pod", Namespace: "shop", Name: "api", Containers: []kubeindex.ContainerState{{Name: "app", WaitingReason: "CrashLoopBackOff", RestartCount: 2}}}
}

func storedChangeIDs(t *testing.T, store storage.EvidenceStore, scope storage.EvidenceScope, since, until time.Time) []string {
	t.Helper()
	var ids []string
	for cursor := ""; ; {
		page, err := store.QueryEvidence(t.Context(), scope, storage.EvidenceQuery{Type: "change", Since: since, Until: until, Limit: 500, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, record := range page.Items {
			ids = append(ids, record.ID)
		}
		if cursor = page.Next; cursor == "" {
			break
		}
	}
	sort.Strings(ids)
	return ids
}

// TestCaptureOverTheSaveLimitKeepsTheNewestRecords replaces 40,000 Pods in one
// capture, 80,000 change records, alongside 2,000 crash terminations spread
// over two hours, a crash status and an open issue. The capture is saved with
// what "now" needs, every termination, and the Pod changes that fit; only Pod
// churn is dropped, with one warning log line as the only signal. Readers see a normal save: no
// gap, no partial history, no save status. The next capture saves normally,
// and the memory and Postgres stores keep the same records.
func TestCaptureOverTheSaveLimitKeepsTheNewestRecords(t *testing.T) {
	if testing.Short() {
		t.Skip("mass churn: 40,000 Pods replaced")
	}
	collected := collectLog(t)
	const pods, terminations = 40000, 2000
	stores := []string{"memory"}
	if os.Getenv("TEST_POSTGRES_DSN") != "" {
		stores = append(stores, "postgres")
	}
	saved := map[string][]string{}
	for _, name := range stores {
		t.Run(name, func(t *testing.T) {
			var provider storage.Provider = storage.NewMemory()
			if name == "postgres" {
				provider = worstHistoryStore(t, worstHistoryDatabase(t))
			}
			service := newTestService(t, inventoryProjectionEndpoint(t), Scope{OrgID: "org", ClusterID: "cluster"})
			service.SetChangeStorage(provider)
			service.changes.evidenceEnabled = true
			store := provider.(storage.EvidenceStore)
			scope := storage.EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}
			first := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
			second, podAt := first.Add(time.Minute), first.Add(30*time.Second)
			third := second.Add(45 * time.Second)

			baseline := limitGeneration("old", pods, first, first, nil)
			if err := service.prepareEvidence(t.Context(), &baseline, baseline); err != nil {
				t.Fatal(err)
			}
			if err := service.commitEvidence(t.Context(), baseline); err != nil {
				t.Fatal(err)
			}

			crashes := []kubechanges.CrashEvent{limitCrash(limitLoopingPod(), "status", second, second, 2)}
			for i := 0; i < terminations; i++ {
				finished := second.Add(-2 * time.Hour).Add(time.Duration(i) * 3600 * time.Millisecond)
				crashes = append(crashes, limitCrash(memoryPod(fmt.Sprintf("new-%05d", i), i), "termination", finished, second, 1))
			}
			replaced := limitGeneration("new", pods, second, podAt, crashes)
			before := len(collected.lines(captureTrimLine))
			if err := service.prepareEvidence(t.Context(), &replaced, baseline); err != nil {
				t.Fatalf("an over-limit capture was refused: %v", err)
			}
			batch := replaced.EvidenceBatch
			if batch == nil {
				t.Fatal("the over-limit capture was not prepared for saving")
			}

			// What "now" needs is kept.
			size, status := 0, false
			kept := map[string]bool{}
			var changes []string
			for _, record := range batch.Records {
				size += storage.EvidencePayloadSize(record.Payload)
				kept[record.ID] = true
				switch {
				case record.Type == "coverage_gap":
					t.Fatalf("a trimmed capture recorded a gap %s..%s", record.GapFrom, record.GapTo)
				case record.Type == "crash" && record.UID == "loop-pod":
					status = true
				case record.Type == "change":
					changes = append(changes, record.ID)
				}
			}
			for _, issue := range batch.Issues {
				size += storage.EvidencePayloadSize(issue.Payload)
			}
			if !status || len(batch.Issues) == 0 {
				t.Fatalf("the trimmed capture lost current state: crash status kept=%t issues=%d", status, len(batch.Issues))
			}
			if total := len(batch.Records) + len(batch.Issues); total > storage.MaxEvidenceBatchRecords || size > storage.MaxEvidenceBatchBytes {
				t.Fatalf("the trimmed capture holds %d records and issues, %d bytes; over the save limits", total, size)
			}

			// Every termination is kept, older than the Pod churn as most
			// are, and of the Pod changes the ones that sort first among
			// equal times.
			for _, crash := range crashes[1:] {
				if !kept["crash:"+crash.ID] {
					t.Fatalf("a termination at %s was dropped while Pod churn was kept", crash.At)
				}
			}
			var offered []string
			byUID := map[string]kubeindex.Record{}
			for _, record := range append(append([]kubeindex.Record(nil), baseline.View.Records...), replaced.View.Records...) {
				byUID[record.UID] = record
			}
			for i := 0; i < pods; i++ {
				old, replacement := byUID[fmt.Sprintf("old-%05d", i)], byUID[fmt.Sprintf("new-%05d", i)]
				for _, delta := range []kubeindex.Delta{{Old: &old}, {New: &replacement}} {
					for _, change := range kubechanges.DetectAll("cluster", delta, podAt) {
						offered = append(offered, "change:"+change.ID)
					}
				}
			}
			sort.Strings(offered)
			sort.Strings(changes)
			if len(changes) == 0 || len(changes) >= len(offered) || !slices.Equal(changes, offered[:len(changes)]) {
				t.Fatalf("kept %d of %d Pod changes, not the first ones in the fixed order", len(changes), len(offered))
			}

			// One warning line, with counts and the dropped span only.
			lines := collected.lines(captureTrimLine)
			if len(lines) != before+1 {
				t.Fatalf("trimming logged %d lines, want exactly one", len(lines)-before)
			}
			match := captureTrimLine.FindStringSubmatch(lines[len(lines)-1])
			wantOffered := 2*pods + terminations + 1 + len(batch.Issues)
			wantKept := len(batch.Records) + len(batch.Issues)
			span := podAt.Format(time.RFC3339Nano)
			if match[1] != fmt.Sprint(wantOffered) || match[2] != fmt.Sprint(wantKept) || match[3] != fmt.Sprint(wantOffered-wantKept) ||
				match[4] != span || match[5] != span || match[6] != "0" || match[7] != match[3] {
				t.Fatalf("trim line %q, want offered=%d kept=%d dropped=%d, all Pod churn, at %s", match[0], wantOffered, wantKept, wantOffered-wantKept, span)
			}
			if strings.Contains(match[0], "checkout-api") || strings.Contains(match[0], "team-") {
				t.Fatal("the trim line carried record text")
			}

			// The same capture always keeps the same records, whatever the
			// order the inventory lists them in.
			reordered := limitGeneration("new", pods, second, podAt, crashes)
			slices.Reverse(reordered.View.Records)
			slices.Reverse(reordered.Crashes)
			if err := service.prepareEvidence(t.Context(), &reordered, baseline); err != nil {
				t.Fatal(err)
			}
			again := map[string]bool{}
			for _, record := range reordered.EvidenceBatch.Records {
				again[record.ID] = true
			}
			if len(again) != len(kept) {
				t.Fatalf("the same capture kept %d records, then %d", len(kept), len(again))
			}
			for id := range kept {
				if !again[id] {
					t.Fatal("the same capture kept different records on a second preparation")
				}
			}

			// Saved like any capture; readers see a normal save.
			if err := service.commitEvidence(t.Context(), replaced); err != nil {
				t.Fatalf("saving the trimmed capture: %v", err)
			}
			done := make(chan struct{})
			close(done)
			service.changes.mu.Lock()
			service.changes.inventories[service.indexScope()] = &inventoryCache{generation: replaced, done: done, cancel: func() {}}
			service.changes.mu.Unlock()
			// The pages read the first ten minutes of the terminations, long
			// before the Pod listing. The in-process store bounds each read to
			// two seconds, so the window stays small and clear of the 25,000
			// kept Pod changes for a race-instrumented run.
			until := time.Now().UTC()
			dropped := TimelineQuery{Since: second.Add(-2 * time.Hour), Until: second.Add(-110 * time.Minute), Limit: 100}
			changePage, err := service.Changes(t.Context(), ChangeQuery{Since: dropped.Since, Until: dropped.Until, Limit: 100})
			if err != nil {
				t.Fatalf("Changes read: %s (%v)", DiagnoseError(err).Code, err)
			}
			crashPage, err := service.Crashes(t.Context(), kubechanges.CrashQuery{Since: dropped.Since, Until: dropped.Until, Limit: 500})
			if err != nil {
				t.Fatalf("Crashes read: %s (%v)", DiagnoseError(err).Code, err)
			}
			timeline, err := service.Timeline(t.Context(), dropped)
			if err != nil {
				t.Fatalf("Timeline read: %s (%v)", DiagnoseError(err).Code, err)
			}
			issues, err := service.historicalIssues(t.Context(), IssueOptions{Since: first, Until: until, Limit: 100})
			if err != nil {
				t.Fatalf("Issues read: %s (%v)", DiagnoseError(err).Code, err)
			}
			if len(changePage.Gaps) != 0 || len(crashPage.Gaps) != 0 || len(crashPage.CaptureGaps) != 0 || len(timeline.Gaps) != 0 {
				t.Fatalf("a trimmed save showed gaps: changes=%v crashes=%v/%v timeline=%v", changePage.Gaps, crashPage.Gaps, crashPage.CaptureGaps, timeline.Gaps)
			}
			if timeline.Sync.Partial || issues.Sync.Partial || len(issues.Partial) != 0 || len(issues.Items) == 0 {
				t.Fatalf("a trimmed save read as partial: timeline=%t issues=%t/%d open issues=%d", timeline.Sync.Partial, issues.Sync.Partial, len(issues.Partial), len(issues.Items))
			}
			for _, capture := range []*CaptureStatus{changePage.Capture, &crashPage.Capture, timeline.Capture} {
				if capture == nil || capture.HistorySave != nil {
					t.Fatalf("a trimmed save reported a history save status: %+v", capture)
				}
			}
			newestCrashes, err := store.QueryEvidence(t.Context(), scope, storage.EvidenceQuery{Type: "crash", Since: podAt.Add(time.Second), Until: second, Limit: 500})
			if err != nil {
				t.Fatal(err)
			}
			if len(newestCrashes.Items) != 9 {
				t.Fatalf("history holds %d crashes after the Pod listing, want the 8 terminations there and the status", len(newestCrashes.Items))
			}

			// The next capture saves normally and logs nothing.
			trims := len(collected.lines(captureTrimLine))
			next := limitGeneration("new", pods, third, podAt.Add(45*time.Second), nil)
			if err := service.prepareEvidence(t.Context(), &next, replaced); err != nil {
				t.Fatal(err)
			}
			if err := service.commitEvidence(t.Context(), next); err != nil {
				t.Fatal(err)
			}
			if len(collected.lines(captureTrimLine)) != trims {
				t.Fatal("a capture within the limits logged a trim")
			}
			checkpoint, err := store.EvidenceCheckpoint(t.Context(), scope)
			if err != nil || checkpoint.ID != next.EvidenceBatch.ID {
				t.Fatalf("the next capture was not saved: checkpoint %q err=%v", checkpoint.ID, err)
			}
			for _, record := range next.EvidenceBatch.Records {
				if record.Type == "coverage_gap" {
					t.Fatalf("the next capture recorded a gap %s..%s", record.GapFrom, record.GapTo)
				}
			}
			saved[name] = storedChangeIDs(t, store, scope, podAt, podAt)
			if !slices.Equal(saved[name], changes) {
				t.Fatalf("the store holds %d change records, the capture kept %d", len(saved[name]), len(changes))
			}
		})
	}
	if len(saved) == 2 && !slices.Equal(saved["memory"], saved["postgres"]) {
		t.Fatalf("memory kept %d change records and Postgres %d, or different ones", len(saved["memory"]), len(saved["postgres"]))
	}
}

func limitChangeRecord(id string, at time.Time, priority int) storage.EvidenceRecord {
	raw, _ := json.Marshal(map[string]string{"id": id})
	return storage.EvidenceRecord{ID: "change:" + id, Type: "change", Priority: priority, At: at, ObservedAt: at, Epoch: "e", Payload: raw}
}

// Trimming keeps current state whole and fills the rest newest first in one
// fixed order: the input order never changes what is kept, records at the
// cut are chosen by their own fields, and the payload byte limit cuts the
// same way as the record limit.
func TestCaptureTrimKeepsTheNewestInOneFixedOrder(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)
	build := func() storage.EvidenceBatch {
		batch := storage.EvidenceBatch{ID: "capture:trim", At: base.Add(3 * time.Hour), Epoch: "e"}
		batch.Issues = []storage.EvidenceIssue{{Key: "k", Kind: "Pod", UID: "p", Rule: "pod.crash_loop", LastSeen: batch.At, Payload: json.RawMessage(`{}`)}}
		batch.Records = append(batch.Records,
			storage.EvidenceRecord{ID: "crash:status", Type: "crash", At: base, ObservedAt: base, Epoch: "e", Payload: json.RawMessage(`{"type":"status"}`)},
			storage.EvidenceRecord{ID: "gap:kind", Type: "coverage_gap", GapFrom: base, GapTo: base, At: base, ObservedAt: base, Epoch: "e", Payload: json.RawMessage(`{}`)},
			storage.EvidenceRecord{ID: "crash:old-termination", Type: "crash", At: base, ObservedAt: base, Epoch: "e", Payload: json.RawMessage(`{"type":"termination"}`)},
		)
		// Three records per second, with both priorities, so the cut falls
		// inside a group of equal times.
		for i := 0; i < 30000; i++ {
			batch.Records = append(batch.Records, limitChangeRecord(fmt.Sprintf("%05d", i), base.Add(time.Duration(i/3)*time.Second), 1+i%2))
		}
		return batch
	}
	terminations := map[string]bool{"crash:old-termination": true}
	var first []string
	for seed := int64(1); seed <= 3; seed++ {
		batch := build()
		rand.New(rand.NewSource(seed)).Shuffle(len(batch.Records), func(i, j int) { batch.Records[i], batch.Records[j] = batch.Records[j], batch.Records[i] })
		trim, trimmed := fitCaptureToSaveLimits(&batch, terminations)
		if !trimmed || trim.offered != 30004 || trim.kept != storage.MaxEvidenceBatchRecords || len(batch.Records)+len(batch.Issues) != storage.MaxEvidenceBatchRecords {
			t.Fatalf("trim %+v kept %d records", trim, len(batch.Records))
		}
		if err := batch.Validate(); err != nil {
			t.Fatal(err)
		}
		ids := []string{}
		for _, record := range batch.Records {
			ids = append(ids, record.ID)
		}
		sort.Strings(ids)
		if !slices.Contains(ids, "crash:status") || !slices.Contains(ids, "gap:kind") || slices.Contains(ids, "crash:old-termination") {
			t.Fatal("trimming dropped current state or kept the oldest termination")
		}
		if first == nil {
			first = ids
		} else if !slices.Equal(first, ids) {
			t.Fatalf("seed %d kept different records", seed)
		}
		// 24,997 changes fit beside the issue, the status and the gap: the
		// newest 8,332 seconds whole, then one of 5,001 to 5,003, which share
		// a second. 5,001 and 5,003 have the higher priority, and 5,001 the
		// lower identity.
		if !slices.Contains(ids, "change:05001") || slices.Contains(ids, "change:05003") || slices.Contains(ids, "change:05002") || !slices.Contains(ids, "change:29999") || slices.Contains(ids, "change:05000") {
			t.Fatal("the cut did not keep the newest records in the fixed order")
		}
		if !trim.from.Equal(base) || !trim.to.Equal(base.Add(1667*time.Second)) {
			t.Fatalf("dropped span %s..%s", trim.from, trim.to)
		}
	}

	// The byte limit: 300 records of 60 KiB, newest first, cut at the first
	// that does not fit.
	batch := storage.EvidenceBatch{ID: "capture:bytes", At: base.Add(time.Hour), Epoch: "e"}
	for i := 0; i < 300; i++ {
		record := limitChangeRecord(fmt.Sprintf("%03d", i), base.Add(time.Duration(i)*time.Second), 1)
		record.Payload = json.RawMessage(`"` + strings.Repeat("x", 60<<10) + `"`)
		batch.Records = append(batch.Records, record)
	}
	trim, trimmed := fitCaptureToSaveLimits(&batch, nil)
	if err := batch.Validate(); !trimmed || err != nil {
		t.Fatalf("trimmed=%t err=%v", trimmed, err)
	}
	fit := storage.MaxEvidenceBatchBytes / (60<<10 + 2)
	if len(batch.Records) != fit || batch.Records[0].ID != fmt.Sprintf("change:%03d", 300-fit) || !trim.to.Equal(base.Add(time.Duration(299-fit)*time.Second)) {
		t.Fatalf("kept %d records from %s, dropped through %s; want the newest %d", len(batch.Records), batch.Records[0].ID, trim.to, fit)
	}

	// Within the limits nothing changes.
	small := build()
	small.Records = small.Records[:100]
	if _, trimmed := fitCaptureToSaveLimits(&small, terminations); trimmed || len(small.Records) != 100 {
		t.Fatal("a capture within the limits was trimmed")
	}

	// Current state alone over the limit is still refused as a whole.
	over := storage.EvidenceBatch{ID: "capture:over", At: base, Epoch: "e"}
	for i := 0; i <= storage.MaxEvidenceBatchRecords; i++ {
		over.Records = append(over.Records, storage.EvidenceRecord{ID: fmt.Sprintf("crash:%05d", i), Type: "crash", At: base, ObservedAt: base, Epoch: "e", Payload: json.RawMessage(`{}`)})
	}
	fitCaptureToSaveLimits(&over, nil)
	if err := over.Validate(); !errors.Is(err, storage.ErrEvidenceLimit) {
		t.Fatalf("current state over the limit answered %v", err)
	}
}

// Every capture that is not saved logs one line naming why, from a fixed
// vocabulary, without record or error text.
func TestEveryRefusedCaptureNamesItsReason(t *testing.T) {
	t.Parallel()
	for err, want := range map[error]string{
		capturePreparationRefused(storage.ErrEvidenceLimit):                              "over_save_limit",
		capturePreparationRefused(storage.ErrEvidenceUnstorable):                         "unstorable_text",
		capturePreparationRefused(fmt.Errorf("payload: %w", storage.ErrInvalidEvidence)): "invalid_capture",
		storageStageFailure("journal", storage.ErrUnsupported):                           "store_unsupported",
		unverifiedInventoryAuthority(ErrInventoryNotReady):                               "authority_unverified",
		ErrIncompleteInventory: "incomplete_inventory",
		storageStageFailure("journal", errors.New("connection reset by checkout-api.team-01")): "storage_unavailable",
	} {
		if got := captureRefusalReason(err); got != want {
			t.Errorf("%v: reason %q, want %q", err, got, want)
		}
	}
}

// A page link to Changes, Timeline or Crashes from before the lists kept gaps
// apart is a well-formed link for the same list and filters. It answers
// cursor_expired, so the page goes back to its first page instead of showing
// an error. A cursor for other filters, or a malformed one, is still an
// invalid argument, and a current link still pages.
func TestHistoryLinksFromBeforeTheUpgradeAskForAReload(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	since, until := now.Add(-time.Hour), now
	service := issueHistoryService(t, storage.NewMemory())
	commitInventoryFixtureHistory(t, service, storage.EvidenceBatch{ID: "capture:links-base", At: now.Add(-50 * time.Minute), Epoch: "fixture"})
	records := []storage.EvidenceRecord{}
	for i := 0; i < 4; i++ {
		at := now.Add(-time.Duration(40-i) * time.Minute)
		records = append(records, pagingChangeRecord(t, fmt.Sprintf("link-%d", i), "shop", "root-l", at, now.Add(-time.Minute)))
		crash := limitCrash(kubeindex.Record{UID: fmt.Sprintf("pod-%d", i), Namespace: "shop", Name: fmt.Sprintf("api-%d", i)}, "termination", at, now.Add(-time.Minute), 1)
		raw, err := json.Marshal(crash)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, storage.EvidenceRecord{AuthorizationKind: "Pod", ID: "crash:" + crash.ID, Type: "crash", At: crash.At, ObservedAt: crash.ObservedAt, Namespace: crash.Namespace, Kind: crash.RootKind, UID: crash.PodUID, RootUID: crash.RootUID, PodUID: crash.PodUID, Epoch: "fixture", Payload: raw})
	}
	pagingCommitHistory(t, service, "capture:links", now.Add(-time.Minute), records)
	store := service.evidenceStore()
	scope := storage.EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}
	// Each list as an earlier release queried it: the same filters, gaps in
	// the keyset or left out of it, and no separate gap read.
	lists := map[string]struct {
		earlier storage.EvidenceQuery
		read    func(cursor, namespace string) error
	}{
		"changes": {storage.EvidenceQuery{Since: since, Until: until, Namespace: "shop", Type: "change", Limit: 1}, func(cursor, namespace string) error {
			_, err := service.Changes(t.Context(), ChangeQuery{Since: since, Until: until, Namespace: namespace, Limit: 1, Cursor: cursor})
			return err
		}},
		"crashes": {storage.EvidenceQuery{Since: since, Until: until, Namespace: "shop", Type: "crash", ByObservation: true, Limit: 1}, func(cursor, namespace string) error {
			_, err := service.Crashes(t.Context(), kubechanges.CrashQuery{Since: since, Until: until, Namespace: namespace, Limit: 1, Cursor: cursor})
			return err
		}},
		"timeline": {storage.EvidenceQuery{Since: since, Until: until, Namespace: "shop", Type: "timeline", Limit: 1}, func(cursor, namespace string) error {
			_, err := service.Timeline(t.Context(), TimelineQuery{Since: since, Until: until, Namespace: namespace, Limit: 1, Cursor: cursor})
			return err
		}},
	}
	for name, list := range lists {
		for _, withGaps := range []bool{false, true} {
			query := list.earlier
			query.WithGaps = withGaps
			page, err := store.QueryEvidence(t.Context(), scope, query)
			if err != nil || page.Next == "" {
				t.Fatalf("%s: earlier page 1: next=%q err=%v", name, page.Next, err)
			}
			err = list.read(page.Next, "shop")
			if !errors.Is(err, ErrCursorExpired) || DiagnoseError(err).Code != "cursor_expired" {
				t.Fatalf("%s (gaps in keyset %t): an earlier page link answered %q (%v), want cursor_expired", name, withGaps, DiagnoseError(err).Code, err)
			}
			if err = list.read(page.Next, "ops"); errors.Is(err, ErrCursorExpired) || DiagnoseError(err).Code != "invalid_arguments" {
				t.Fatalf("%s: an earlier link reused on other filters answered %q, want invalid_arguments", name, DiagnoseError(err).Code)
			}
		}
		for _, malformed := range []string{base64.RawURLEncoding.EncodeToString([]byte("{}")), "not a cursor", base64.RawURLEncoding.EncodeToString([]byte(`{"Binding":"x"}`))} {
			if err := list.read(malformed, "shop"); errors.Is(err, ErrCursorExpired) || DiagnoseError(err).Code != "invalid_arguments" {
				t.Fatalf("%s: a malformed cursor answered %q, want invalid_arguments", name, DiagnoseError(err).Code)
			}
		}
	}
	page, err := service.Changes(t.Context(), ChangeQuery{Since: since, Until: until, Namespace: "shop", Limit: 1})
	if err != nil || page.Next == "" {
		t.Fatalf("current page 1: next=%q err=%v", page.Next, err)
	}
	if next, err := service.Changes(t.Context(), ChangeQuery{Since: since, Until: until, Namespace: "shop", Limit: 1, Cursor: page.Next}); err != nil || len(next.Items) != 1 {
		t.Fatalf("a current page link stopped paging: items=%d err=%v", len(next.Items), err)
	}
}

// worstChurnGenerations is a capture at the 50,000-object inventory limit
// with the most change records a capture can offer: 40,000 Deployments that
// each change image, configuration references and generation and complete
// their rollout (four records each), 10,000 Deployments deleted and 10,000
// ReplicaSets created, each with its rollout start.
func worstChurnGenerations(at time.Time) (previous, current inventoryGeneration) {
	kinds := map[string]kubeindex.KindStatus{}
	for kind := range indexedResourceIDs {
		kinds[kind] = kubeindex.KindStatus{State: "ready", ObservedAt: at, CompletedAt: at}
	}
	earlier := map[string]kubeindex.KindStatus{}
	for kind := range indexedResourceIDs {
		earlier[kind] = kubeindex.KindStatus{State: "ready", ObservedAt: at.Add(-30 * time.Second), CompletedAt: at.Add(-30 * time.Second)}
	}
	namespace := kubeindex.Record{UID: "physical", Kind: "Namespace", Name: "kube-system"}
	deployment := func(i int, version string, generation int64, done bool) kubeindex.Record {
		record := kubeindex.Record{
			APIVersion: "apps/v1", UID: fmt.Sprintf("deployment-%05d", i), Kind: "Deployment", Namespace: fmt.Sprintf("team-%02d", i%60), Name: fmt.Sprintf("checkout-api-%05d", i),
			Labels:     map[string]string{"app.kubernetes.io/name": "checkout-api", "team": fmt.Sprintf("team-%02d", i%60)},
			Generation: generation, Replicas: 3, CreatedAt: at.Add(-24 * time.Hour),
			Images:     []string{"registry.example.com/shop/checkout-api:" + version, "registry.example.com/shop/sidecar-proxy:" + version, "registry.example.com/shop/log-forwarder:" + version},
			References: map[string][]string{"config_maps": {"checkout-" + version}, "secrets": {"checkout-credentials-" + version}},
		}
		if done {
			record.ObservedGeneration, record.Updated, record.Ready = generation, 3, 3
		}
		return record
	}
	previous.View = kubeindex.Snapshot{Kinds: earlier, Records: []kubeindex.Record{namespace}}
	current.View = kubeindex.Snapshot{Kinds: kinds, Records: []kubeindex.Record{namespace}}
	for i := 0; i < 40000; i++ {
		previous.View.Records = append(previous.View.Records, deployment(i, "2026.11.05-1a2b3c4d", 1, false))
		current.View.Records = append(current.View.Records, deployment(i, "2026.11.06-5e6f7a8b", 2, true))
	}
	for i := 40000; i < 50000; i++ {
		previous.View.Records = append(previous.View.Records, deployment(i, "2026.11.05-1a2b3c4d", 1, false))
		owner := deployment(i-40000, "", 2, true)
		current.View.Records = append(current.View.Records, kubeindex.Record{
			APIVersion: "apps/v1", UID: fmt.Sprintf("replicaset-%05d", i), Kind: "ReplicaSet", Namespace: owner.Namespace, Name: owner.Name + "-7d9f8b6c5", Revision: "2", CreatedAt: at.Add(-time.Minute),
			Owners: []kubeindex.OwnerRef{{UID: owner.UID, Kind: "Deployment", Name: owner.Name, Namespace: owner.Namespace, Controller: true, ControllerKnown: true}},
		})
	}
	previous.Revision, current.Revision = "previous", "current"
	current.PodSampledAt = at
	return previous, current
}

// TestCapturePreparationMemoryAtTheInventoryLimit measures the transient heap
// of preparing the largest capture the inventory limit allows for saving,
// trimming included. Its change records are folded into the bounded set as
// they are produced, so the history share of preparing it stays within the
// transient budget.
func TestCapturePreparationMemoryAtTheInventoryLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a capture at the inventory limit")
	}
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	service := newTestService(t, inventoryProjectionEndpoint(t), Scope{OrgID: "org", ClusterID: "cluster"})
	service.SetChangeStorage(storage.NewMemory())
	service.changes.evidenceEnabled = true
	previous, current := worstChurnGenerations(at)
	snapshot := transientPeak(func() {
		if _, err := service.issuesSnapshot(pinInventory(t.Context(), current), IssueOptions{unpaged: true, captureOnly: true}); err != nil {
			t.Fatal(err)
		}
	})
	unchanged := transientPeak(func() {
		same := current
		if err := service.prepareEvidence(t.Context(), &same, current); err != nil {
			t.Fatal(err)
		}
	})
	var prepared inventoryGeneration
	peak := transientPeak(func() {
		prepared = current
		if err := service.prepareEvidence(t.Context(), &prepared, previous); err != nil {
			t.Fatal(err)
		}
	})
	if prepared.EvidenceBatch == nil {
		t.Fatal("the capture was not prepared for saving")
	}
	t.Logf("capture at the inventory limit: %d records and %d issues kept; transient heap of preparing it %.1f MiB; preparing the same inventory with no changes %.1f MiB; the issue snapshot alone %.1f MiB",
		len(prepared.EvidenceBatch.Records), len(prepared.EvidenceBatch.Issues), float64(peak)/(1<<20), float64(unchanged)/(1<<20), float64(snapshot)/(1<<20))
	// Preparing any capture of this inventory reads its issues and indexes
	// both listings, change records or not; that share is reported apart.
	// What 190,000 offered change records add on top must fit the budget.
	if peak > unchanged+captureTransientBudget {
		t.Errorf("the change records needed %.1f MiB of transient heap above preparing the unchanged inventory, over the %d MiB budget", float64(peak-unchanged)/(1<<20), captureTransientBudget>>20)
	}
}

// rolloutGeneration is one capture of a Deployment and its ReplicaSet with
// pods Pods named by prefix. Deployments and ReplicaSets are listed at
// workloadsAt, every other kind at at.
func rolloutGeneration(prefix string, pods int, version string, revision int64, at, workloadsAt time.Time) inventoryGeneration {
	generation := limitGeneration(prefix, pods, at, at, nil)
	for _, kind := range []string{"Deployment", "ReplicaSet"} {
		generation.View.Kinds[kind] = kubeindex.KindStatus{State: "ready", ObservedAt: workloadsAt, CompletedAt: workloadsAt}
	}
	deployment := kubeindex.Record{APIVersion: "apps/v1", UID: "web", Kind: "Deployment", Namespace: "shop", Name: "web", Generation: revision, Replicas: 3, CreatedAt: at.Add(-24 * time.Hour), Images: []string{"registry.example.com/shop/web:" + version}}
	replicaSet := kubeindex.Record{APIVersion: "apps/v1", UID: fmt.Sprintf("web-rs-%d", revision), Kind: "ReplicaSet", Namespace: "shop", Name: fmt.Sprintf("web-%d", revision), Revision: fmt.Sprint(revision), CreatedAt: workloadsAt.Add(-15 * time.Second), Images: deployment.Images,
		Owners: []kubeindex.OwnerRef{{UID: "web", Kind: "Deployment", Name: "web", Namespace: "shop", Controller: true, ControllerKnown: true}}}
	generation.View.Records = append(generation.View.Records, deployment, replicaSet)
	return generation
}

// A Deployment rollout that replaces 15,000 Pods offers 30,000 Pod creations
// and deletions, listed after the Deployment's own changes. The rollout start
// (the ReplicaSet's creation time) and the image change are older than all
// of that churn, yet they are what explains it: they are kept, and only Pod
// churn is dropped, on the memory and Postgres stores alike.
func TestCaptureOverTheSaveLimitKeepsWorkloadChangesOverPodChurn(t *testing.T) {
	if testing.Short() {
		t.Skip("mass churn: 15,000 Pods replaced")
	}
	collected := collectLog(t)
	const pods = 15000
	stores := []string{"memory"}
	if os.Getenv("TEST_POSTGRES_DSN") != "" {
		stores = append(stores, "postgres")
	}
	saved := map[string][]string{}
	for _, name := range stores {
		t.Run(name, func(t *testing.T) {
			var provider storage.Provider = storage.NewMemory()
			if name == "postgres" {
				provider = worstHistoryStore(t, worstHistoryDatabase(t))
			}
			service := newTestService(t, inventoryProjectionEndpoint(t), Scope{OrgID: "org", ClusterID: "cluster"})
			service.SetChangeStorage(provider)
			service.changes.evidenceEnabled = true
			store := provider.(storage.EvidenceStore)
			scope := storage.EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}
			first := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)
			workloadsAt, podsAt := first.Add(20*time.Second), first.Add(50*time.Second)

			baseline := rolloutGeneration("old", pods, "v1", 1, first, first)
			if err := service.prepareEvidence(t.Context(), &baseline, baseline); err != nil {
				t.Fatal(err)
			}
			if err := service.commitEvidence(t.Context(), baseline); err != nil {
				t.Fatal(err)
			}
			rollout := rolloutGeneration("new", pods, "v2", 2, podsAt, workloadsAt)
			before := len(collected.lines(captureTrimLine))
			if err := service.prepareEvidence(t.Context(), &rollout, baseline); err != nil {
				t.Fatalf("the rollout capture was refused: %v", err)
			}
			workload := map[string]bool{}
			churn := 0
			for _, record := range rollout.EvidenceBatch.Records {
				if record.Type != "change" {
					continue
				}
				if record.Kind == "Pod" {
					churn++
					if !record.At.Equal(podsAt) {
						t.Fatalf("a Pod change at %s, want the Pod listing time", record.At)
					}
					continue
				}
				if !record.At.Before(podsAt) {
					t.Fatalf("the %s %s change at %s is not older than the Pod churn", record.Kind, record.Rule, record.At)
				}
				workload[record.Kind+" "+record.Rule] = true
			}
			if !workload["Deployment rollout_started"] || !workload["Deployment image_changed"] {
				t.Fatalf("the rollout lost its cause: kept %v", workload)
			}
			if churn == 0 || churn >= 2*pods {
				t.Fatalf("kept %d of %d Pod changes, want a trimmed share", churn, 2*pods)
			}
			lines := collected.lines(captureTrimLine)
			if len(lines) != before+1 {
				t.Fatalf("trimming logged %d lines, want exactly one", len(lines)-before)
			}
			match := captureTrimLine.FindStringSubmatch(lines[len(lines)-1])
			if match[6] != "0" || match[7] != match[3] || match[3] != fmt.Sprint(2*pods-churn) {
				t.Fatalf("trim line %q, want only the %d dropped Pod changes", match[0], 2*pods-churn)
			}
			if err := service.commitEvidence(t.Context(), rollout); err != nil {
				t.Fatalf("saving the trimmed rollout: %v", err)
			}
			page, err := store.QueryEvidence(t.Context(), scope, storage.EvidenceQuery{Type: "change", Kind: "Deployment", Since: first, Until: podsAt, Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			var rules []string
			for _, record := range page.Items {
				rules = append(rules, record.Rule)
			}
			sort.Strings(rules)
			saved[name] = rules
			if !slices.Contains(rules, "rollout_started") || !slices.Contains(rules, "image_changed") {
				t.Fatalf("history holds Deployment changes %v, want the rollout start and the image change", rules)
			}
		})
	}
	if len(saved) == 2 && !slices.Equal(saved["memory"], saved["postgres"]) {
		t.Fatalf("memory holds Deployment changes %v, Postgres %v", saved["memory"], saved["postgres"])
	}
}

// A lasting refusal is logged once, not on every publish: again only when its
// reason or class changes, and once when captures are prepared again.
func TestCaptureRefusalIsLoggedOnlyWhenItChanges(t *testing.T) {
	collected := collectLog(t)
	lines := regexp.MustCompile(`kubernetes: history capture(s are prepared for saving again| refused before saving reason=\S+ class=\S+)$`)
	cache := &inventoryCache{}
	incomplete := "reason=incomplete_inventory class=incomplete_pages"
	unverified := "reason=authority_unverified class=unavailable"
	for _, refusal := range []string{"", "", incomplete, incomplete, incomplete, unverified, unverified, "", "", incomplete} {
		noteCaptureRefusal(cache, refusal)
	}
	want := []string{
		"kubernetes: history capture refused before saving " + incomplete,
		"kubernetes: history capture refused before saving " + unverified,
		"kubernetes: history captures are prepared for saving again",
		"kubernetes: history capture refused before saving " + incomplete,
	}
	got := collected.lines(lines)
	if len(got) != len(want) {
		t.Fatalf("logged %d lines, want %d: %q", len(got), len(want), got)
	}
	for i := range want {
		if !strings.HasSuffix(got[i], want[i]) {
			t.Fatalf("line %d is %q, want %q", i, got[i], want[i])
		}
	}
	// Before a scope has a cache, every refusal is logged and a prepared
	// capture logs nothing.
	noteCaptureRefusal(nil, "")
	noteCaptureRefusal(nil, incomplete)
	noteCaptureRefusal(nil, incomplete)
	if extra := len(collected.lines(lines)) - len(want); extra != 2 {
		t.Fatalf("without a cache, logged %d lines for two refusals", extra)
	}
}

// Under the byte limit the kept set is the longest run of the best records
// that fits, whatever order they are offered in: once a large record has
// been dropped, a smaller, lower-ranked record that would fit in the space
// left is dropped too. 255 workload changes of 60 KiB and one older 62 KiB
// configuration change fill the bytes; small Pod churn and smaller changes of
// mixed sizes are offered around them in many orders.
func TestCaptureTrimUnderTheByteLimitIsTheSameInEveryOrder(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)
	record := func(id, kind, rule string, at time.Time, size int) storage.EvidenceRecord {
		head := `"` + id
		return storage.EvidenceRecord{ID: "change:" + id, Type: "change", Kind: kind, Rule: rule, Priority: 2, At: at, ObservedAt: base.Add(time.Hour), Epoch: "e", Payload: json.RawMessage(head + strings.Repeat("x", size-len(head)-1) + `"`)}
	}
	var offered []storage.EvidenceRecord
	for i := 0; i < 255; i++ {
		offered = append(offered, record(fmt.Sprintf("image-%03d", i), "Deployment", "image_changed", base.Add(30*time.Minute+time.Duration(i)*time.Second), 60<<10))
	}
	offered = append(offered, record("config", "ConfigMap", "config_changed", base.Add(10*time.Minute), 62<<10))
	sizes := rand.New(rand.NewSource(7))
	for i := 0; i < 60; i++ {
		offered = append(offered, record(fmt.Sprintf("rollout-%03d", i), "Deployment", "rollout_started", base.Add(time.Duration(sizes.Intn(3600))*time.Second), 100+sizes.Intn(4<<10)))
	}
	for i := 0; i < 50; i++ {
		offered = append(offered, record(fmt.Sprintf("pod-%03d", i), "Pod", "created", base.Add(50*time.Minute), 200))
		offered = append(offered, record(fmt.Sprintf("gone-%03d", i), "Pod", "deleted", base.Add(time.Duration(sizes.Intn(3600))*time.Second), 150+sizes.Intn(400)))
	}

	// Sort, then cut at the first record that does not fit.
	ranked := slices.Clone(offered)
	slices.SortFunc(ranked, func(a, b storage.EvidenceRecord) int {
		if order := captureRecordTier(a) - captureRecordTier(b); order != 0 {
			return order
		}
		return newestCaptureRecordFirst(a, b)
	})
	want, used := []string{}, 0
	for _, candidate := range ranked {
		used += storage.EvidencePayloadSize(candidate.Payload)
		if len(want)+1 > storage.MaxEvidenceBatchRecords || used > storage.MaxEvidenceBatchBytes {
			break
		}
		want = append(want, candidate.ID)
	}
	sort.Strings(want)
	if slices.Contains(want, "change:config") || len(want) >= len(offered) {
		t.Fatalf("the probe does not reach the byte limit at the configuration change: %d of %d kept", len(want), len(offered))
	}

	orders := map[string][]storage.EvidenceRecord{
		"churn last":  offered,
		"churn first": append(slices.Clone(offered[256:]), offered[:256]...),
	}
	for seed := int64(1); seed <= 6; seed++ {
		shuffled := slices.Clone(offered)
		rand.New(rand.NewSource(seed)).Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		orders[fmt.Sprintf("shuffle %d", seed)] = shuffled
	}
	for name, order := range orders {
		batch := storage.EvidenceBatch{ID: "capture:bytes", At: base.Add(time.Hour), Epoch: "e", Records: slices.Clone(order)}
		trim, trimmed := fitCaptureToSaveLimits(&batch, nil)
		if !trimmed {
			t.Fatalf("%s: nothing was dropped", name)
		}
		if err := batch.Validate(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		kept := []string{}
		churnKept := false
		for _, record := range batch.Records {
			kept = append(kept, record.ID)
			churnKept = churnKept || captureRecordTier(record) == captureTierChurn
		}
		sort.Strings(kept)
		if !slices.Equal(kept, want) {
			t.Fatalf("%s: kept %d records, sort-then-cut keeps %d, or different ones", name, len(kept), len(want))
		}
		if churnKept && trim.dropped[captureTierChanges] > 0 {
			t.Fatalf("%s: Pod churn was kept while %d other changes were dropped", name, trim.dropped[captureTierChanges])
		}
	}
}
