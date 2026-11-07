package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	kubechanges "github.com/VersusControl/versus-incident/pkg/kubernetes/changes"
	kubeindex "github.com/VersusControl/versus-incident/pkg/kubernetes/index"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/tests/fakekube"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// worstHistoryDatabase creates a fresh qa_ database for one test on the
// isolated test server and drops it when the test ends.
func worstHistoryDatabase(t *testing.T) string {
	t.Helper()
	server := os.Getenv("TEST_POSTGRES_DSN")
	if server == "" {
		t.Skip("isolated Postgres required")
	}
	target, err := url.Parse(server)
	if err != nil {
		t.Fatal("test server address is not a URL")
	}
	name := fmt.Sprintf("qa_worst_history_%d", time.Now().UnixNano())
	admin, err := pgx.Connect(t.Context(), server)
	if err != nil {
		t.Fatal("cannot reach the test server")
	}
	defer admin.Close(context.Background())
	if _, err = admin.Exec(t.Context(), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		conn, err := pgx.Connect(ctx, server)
		if err != nil {
			t.Errorf("drop %s: cannot reach the test server", name)
			return
		}
		defer conn.Close(ctx)
		if _, err = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})
	target.Path = "/" + name
	return target.String()
}

func worstHistoryStore(t *testing.T, dsn string) storage.Provider {
	t.Helper()
	provider, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn, ObservationHeartbeat: "5m"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	return provider
}

func worstMutate(t *testing.T, fixture *fakekube.Server, body string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	fixture.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/_fake/mutate", strings.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("fixture mutation refused: %d %s", recorder.Code, recorder.Body.String())
	}
}

func worstUpsert(t *testing.T, fixture *fakekube.Server, resource, namespace, name string, object any) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"operation": "upsert", "resource": resource, "namespace": namespace, "name": name, "object": object})
	if err != nil {
		t.Fatal(err)
	}
	worstMutate(t, fixture, string(raw))
}

func worstCache(service *Service) *inventoryCache {
	service.changes.mu.Lock()
	defer service.changes.mu.Unlock()
	return service.changes.inventories[service.indexScope()]
}

func worstCredentialFailures(service *Service) int {
	service.recovery.mu.Lock()
	defer service.recovery.mu.Unlock()
	return service.recovery.failures
}

// TestMassPodReplacementBeyondTheSaveLimitIsNotSilentlyLost replaces 5,100
// Pods between two captures, as a node pool rotation does. The capture then
// carries more than 10,000 records, more than one save transaction holds. It
// must be prepared for saving, never dropped whole, and saved: history pages
// must not stop silently at the earlier capture. A capture over the capture
// limits is covered by TestCaptureOverTheSaveLimitKeepsTheNewestRecords.
func TestMassPodReplacementBeyondTheSaveLimitIsNotSilentlyLost(t *testing.T) {
	if testing.Short() {
		t.Skip("mass churn: 5,100 Pods replaced")
	}
	fixture, err := fakekube.NewServer(fakekube.Config{Pods: 5100, Namespaces: 60, Seed: 7})
	if err != nil {
		t.Fatal(err)
	}
	worstMutate(t, fixture, `{"operation":"upsert","resource":"namespaces","name":"kube-system","object":{"kind":"Namespace","metadata":{"uid":"mass-churn-physical"}}}`)
	upstream := httptest.NewServer(fixture)
	t.Cleanup(upstream.Close)
	base := storage.NewMemory()
	service := saveDelayReplica(t, upstream.URL, base, "mass-churn", productionPace)
	scope := storage.EvidenceScope{OrgID: "default", ClusterID: "mass-churn", ClusterUID: "mass-churn-physical"}
	store := base.(storage.EvidenceStore)
	waitUntil(t, 120*time.Second, "the first capture of 5,100 Pods saved to history", func() bool {
		checkpoint, err := store.EvidenceCheckpoint(t.Context(), scope)
		return err == nil && checkpoint.ID != "" && historySaveCleared(t, service)
	})
	before, err := store.EvidenceCheckpoint(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}

	var items []map[string]any
	for next := ""; ; {
		listing := httptest.NewRecorder()
		fixture.ServeHTTP(listing, httptest.NewRequest(http.MethodGet, "/api/v1/pods?limit=500&continue="+url.QueryEscape(next), nil))
		var page struct {
			Metadata struct {
				Continue string `json:"continue"`
			} `json:"metadata"`
			Items []map[string]any `json:"items"`
		}
		if err = json.Unmarshal(listing.Body.Bytes(), &page); err != nil {
			t.Fatalf("listing generated Pods: %v", err)
		}
		items = append(items, page.Items...)
		if next = page.Metadata.Continue; next == "" {
			break
		}
	}
	if len(items) < 5100 {
		t.Fatalf("listed %d generated Pods", len(items))
	}
	pods := struct{ Items []map[string]any }{items}
	for _, pod := range pods.Items {
		metadata := pod["metadata"].(map[string]any)
		metadata["uid"] = "replaced-" + metadata["uid"].(string)
		delete(metadata, "resourceVersion")
		worstUpsert(t, fixture, "pods", metadata["namespace"].(string), metadata["name"].(string), pod)
	}
	cache := worstCache(service)
	published := func() (replaced bool, batch *storage.EvidenceBatch, failure string) {
		cache.mu.RLock()
		defer cache.mu.RUnlock()
		for _, record := range cache.generation.View.Records {
			if strings.HasPrefix(record.UID, "replaced-") {
				replaced = true
				break
			}
		}
		return replaced, cache.generation.EvidenceBatch, cache.generation.EvidenceFailure
	}
	var batch *storage.EvidenceBatch
	var failure string
	waitUntil(t, 120*time.Second, "the replaced Pods published", func() bool {
		var replaced bool
		replaced, batch, failure = published()
		return replaced
	})
	if worstCredentialFailures(service) != 0 {
		t.Fatal("mass churn reached the credential recovery gate")
	}
	if batch == nil {
		t.Fatalf("the capture of the replaced Pods was published but never prepared for saving (failure %q)", failure)
	}
	t.Logf("published capture after replacement: prepared records=%d", len(batch.Records))
	waitUntil(t, 60*time.Second, "the replacement capture saved", func() bool {
		latest, err := store.EvidenceCheckpoint(t.Context(), scope)
		// This capture, or a later one, is the saved checkpoint.
		return err == nil && latest.Generation == batch.Generation && !latest.At.Before(batch.At)
	})
	if status := historySaveOf(t, service); status != nil && status.Code == "history_save_rejected" {
		t.Fatalf("a saved capture reads as rejected: %+v", status)
	}
	latest, err := store.EvidenceCheckpoint(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if !latest.At.After(before.At) {
		t.Fatalf("saved history stops at %s, the first capture", latest.At.Format(time.RFC3339))
	}
}

// crashFlapStatus is a kubelet container status for one cycle.
func crashFlapStatus(t *testing.T, waiting string, restarts int, finished time.Time) map[string]any {
	t.Helper()
	state := `{"running":{}}`
	if waiting != "" {
		state = `{"waiting":{"reason":"` + waiting + `"}}`
	}
	raw := fmt.Sprintf(`{"containerStatuses":[{"name":"app","restartCount":%d,"state":%s,"lastState":{"terminated":{"exitCode":1,"reason":"Error","finishedAt":%q}}}]}`, restarts, state, finished.UTC().Format(time.RFC3339))
	var status map[string]any
	if err := json.Unmarshal([]byte(raw), &status); err != nil {
		t.Fatal(err)
	}
	return status
}

// crashFlapRecords runs the crash producer for one cycle and maps its events
// to history records the way a capture does.
func crashFlapRecords(t *testing.T, status map[string]any, observed time.Time) []storage.EvidenceRecord {
	t.Helper()
	pod := kubeindex.Record{UID: "flap-pod", Kind: "Pod", Namespace: "shop", Name: "api-0"}
	batch := &crashCaptureBatch{}
	batch.add("flap-cluster", pod, status, observed)
	events, _, _ := batch.resolve(kubeindex.Snapshot{Records: []kubeindex.Record{pod}, Kinds: map[string]kubeindex.KindStatus{"Pod": {State: "ready"}}}, observed)
	records := make([]storage.EvidenceRecord, 0, len(events))
	for _, crash := range events {
		raw, err := json.Marshal(crash)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, storage.EvidenceRecord{AuthorizationKind: "Pod", ID: "crash:" + crash.ID, Type: "crash", At: crash.At, ObservedAt: crash.ObservedAt, Namespace: crash.Namespace, Kind: crash.RootKind, UID: crash.PodUID, RootUID: crash.RootUID, PodUID: crash.PodUID, Epoch: "stable", Payload: raw})
	}
	return records
}

// TestCrashFlappingWritesOneRowPerRealChange drives the crash producer through
// a container that crashes, waits in back-off, restarts and runs, block after
// block, holding each state for several 30-second cycles. Each block is three
// real changes: the back-off state, the running state after the restart, and
// the termination. That is all the journal may gain, whether the node clock
// agrees with the Versus clock or runs two minutes ahead of it.
func TestCrashFlappingWritesOneRowPerRealChange(t *testing.T) {
	var dsn string
	if os.Getenv("TEST_POSTGRES_DSN") != "" {
		dsn = worstHistoryDatabase(t)
	}
	for _, clock := range []struct {
		name string
		skew time.Duration
	}{{"node clock in step", 0}, {"node clock two minutes ahead", 2 * time.Minute}} {
		t.Run(clock.name, func(t *testing.T) {
			const blocks = 40
			origin := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second)
			ids := map[string]bool{}
			var stores []storage.EvidenceStore
			stores = append(stores, storage.NewMemory().(storage.EvidenceStore))
			if dsn != "" {
				stores = append(stores, worstHistoryStore(t, dsn).(storage.EvidenceStore))
			}
			scope := storage.EvidenceScope{OrgID: "default", ClusterID: "flap-" + strings.ReplaceAll(clock.name, " ", "-"), ClusterUID: "flap-physical"}
			cycle := 0
			for block := 0; block < blocks; block++ {
				start := origin.Add(time.Duration(block*6) * 30 * time.Second)
				finished := start.Add(-10 * time.Second).Add(clock.skew)
				for phase := 0; phase < 6; phase++ {
					observed := start.Add(time.Duration(phase) * 30 * time.Second)
					status := crashFlapStatus(t, "CrashLoopBackOff", block, finished)
					if phase >= 3 {
						status = crashFlapStatus(t, "", block+1, finished)
					}
					records := crashFlapRecords(t, status, observed)
					for _, record := range records {
						ids[record.ID] = true
					}
					for _, store := range stores {
						checkpoint, err := store.EvidenceCheckpoint(t.Context(), scope)
						if err != nil {
							t.Fatal(err)
						}
						batch := storage.EvidenceBatch{ID: fmt.Sprintf("cycle-%d", cycle), Expected: checkpoint.ID, Generation: checkpoint.Generation, At: observed, Epoch: "stable", Records: records}
						if err = store.CommitEvidence(t.Context(), scope, batch); err != nil {
							t.Fatalf("cycle %d: %v", cycle, err)
						}
					}
					cycle++
				}
			}
			want := 3 * blocks
			for i, store := range stores {
				page, err := store.QueryEvidence(t.Context(), scope, storage.EvidenceQuery{Type: "crash", Since: origin.Add(-time.Hour), Until: origin.Add(4 * time.Hour), Limit: 500})
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("store %d: %d crash rows over %d cycles, %d distinct producer identities", i, len(page.Items), cycle, len(ids))
				if len(page.Items) != want {
					t.Errorf("store %d saved %d crash rows for %d real changes", i, len(page.Items), want)
				}
			}
			if len(ids) != want {
				t.Errorf("the producer reported %d crash identities for %d real changes", len(ids), want)
			}
		})
	}
}

// TestPostgresAnswersNeverReachTheCredentialGate feeds the answers a real
// Postgres gives when it dies, restarts, times a statement out or misses the
// lease deadline through the capture layer's classification. Each is a storage
// failure the owner retries; none may count against the credential.
func TestPostgresAnswersNeverReachTheCredentialGate(t *testing.T) {
	for name, answer := range map[string]error{
		"connection killed":     &pgconn.PgError{Severity: "FATAL", Code: "57P01", Message: "terminating connection due to administrator command"},
		"server shutting down":  &pgconn.PgError{Severity: "FATAL", Code: "57P02", Message: "terminating connection due to crash of another server process"},
		"cannot connect now":    &pgconn.PgError{Severity: "FATAL", Code: "57P03", Message: "the database system is starting up"},
		"statement timeout":     &pgconn.PgError{Severity: "ERROR", Code: "57014", Message: "canceling statement due to statement timeout"},
		"lease deadline on I/O": fmt.Errorf("write failed: %w", &timeoutError{}),
		"lease deadline":        fmt.Errorf("timeout: %w", context.DeadlineExceeded),
		"deadlock":              &pgconn.PgError{Severity: "ERROR", Code: "40P01", Message: "deadlock detected"},
	} {
		for _, stage := range []string{"journal", "payload", "cas"} {
			err := storageStageFailure(stage, answer)
			if countsAgainstCredential(err) {
				t.Errorf("%s at %s counts against the credential", name, stage)
			}
			if reason, class := recoveryReason(err, stage), errorClass(err); reason != "storage_unavailable" || class != "storage_unavailable" {
				t.Errorf("%s at %s reported as %q / %q", name, stage, reason, class)
			}
		}
	}
}

type timeoutError struct{}

func (*timeoutError) Error() string   { return "i/o timeout" }
func (*timeoutError) Timeout() bool   { return true }
func (*timeoutError) Temporary() bool { return true }

// TestPoisonObjectsDoNotStopPostgresHistorySaves publishes objects a Postgres
// jsonb column cannot hold if copied verbatim: NUL bytes and invalid UTF-8
// in an image, an annotation of several hundred KiB, a termination and waiting
// message, an event reason and message. Captures of that cluster must keep
// saving to Postgres, including after the poisoned workload really changes.
func TestPoisonObjectsDoNotStopPostgresHistorySaves(t *testing.T) {
	dsn := worstHistoryDatabase(t)
	store := worstHistoryStore(t, dsn)
	fixture, err := fakekube.NewServer(fakekube.Config{Scenario: "crash-evidence"})
	if err != nil {
		t.Fatal(err)
	}
	worstMutate(t, fixture, `{"operation":"upsert","resource":"namespaces","name":"kube-system","object":{"kind":"Namespace","metadata":{"uid":"poison-physical"}}}`)
	upstream := httptest.NewServer(fixture)
	t.Cleanup(upstream.Close)
	service := saveDelayReplica(t, upstream.URL, store, "poison", fixturePace)
	scope := storage.EvidenceScope{OrgID: "default", ClusterID: "poison", ClusterUID: "poison-physical"}
	evidence := store.(storage.EvidenceStore)
	waitUntil(t, 90*time.Second, "a first capture saved to Postgres", func() bool {
		checkpoint, err := evidence.EvidenceCheckpoint(t.Context(), scope)
		return err == nil && checkpoint.ID != ""
	})

	poison := "bad\u0000\xff"
	huge := strings.Repeat("annotation\u0000", 60000)
	finished := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	deployment := func(image string, generation int) map[string]any {
		labels := map[string]string{"app": "poison"}
		return map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "poison", "namespace": "batch", "uid": "poison-deployment", "generation": generation, "labels": labels, "annotations": map[string]string{"note": huge}},
			"spec":   map[string]any{"replicas": 1, "selector": map[string]any{"matchLabels": labels}, "template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "app", "image": image}}}}},
			"status": map[string]any{"replicas": 1, "observedGeneration": generation}}
	}
	worstUpsert(t, fixture, "deployments", "batch", "poison", deployment("registry.invalid/app:"+poison, 1))
	worstUpsert(t, fixture, "pods", "batch", "poison-0", map[string]any{"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": "poison-0", "namespace": "batch", "uid": "poison-pod", "labels": map[string]string{"app": "poison"}, "annotations": map[string]string{"note": huge}},
		"spec":     map[string]any{"containers": []any{map[string]any{"name": "app", "image": "registry.invalid/app:" + poison}}},
		"status": map[string]any{"phase": "Running", "message": poison, "containerStatuses": []any{map[string]any{"name": "app", "image": "registry.invalid/app:" + poison, "restartCount": 3,
			"state":     map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff", "message": poison}},
			"lastState": map[string]any{"terminated": map[string]any{"exitCode": 1, "reason": "Error", "message": poison, "finishedAt": finished}}}}}})
	worstUpsert(t, fixture, "events", "batch", "poison.1", map[string]any{"apiVersion": "v1", "kind": "Event",
		"metadata":       map[string]any{"name": "poison.1", "namespace": "batch", "uid": "poison-event"},
		"involvedObject": map[string]any{"kind": "Pod", "name": "poison-0", "namespace": "batch", "uid": "poison-pod"},
		"type":           "Warning", "reason": "Failed" + poison, "message": "pull " + poison, "count": 5, "firstTimestamp": finished, "lastTimestamp": finished})

	advanced := func(from string, saves int, describe string) string {
		t.Helper()
		seen := map[string]bool{}
		current := from
		waitUntil(t, 120*time.Second, describe, func() bool {
			checkpoint, err := evidence.EvidenceCheckpoint(t.Context(), scope)
			if err == nil && checkpoint.ID != current {
				seen[checkpoint.ID] = true
				current = checkpoint.ID
			}
			return len(seen) >= saves
		})
		return current
	}
	start, _ := evidence.EvidenceCheckpoint(t.Context(), scope)
	latest := advanced(start.ID, 3, "three captures with the poisoned objects saved")
	worstUpsert(t, fixture, "deployments", "batch", "poison", deployment("registry.invalid/app:v2"+poison, 2))
	advanced(latest, 3, "three more captures saved after the poisoned workload changed")
	service.recovery.mu.Lock()
	journal := service.recovery.stages["journal"]
	service.recovery.mu.Unlock()
	if journal.reason != "ready" || worstCredentialFailures(service) != 0 {
		t.Fatalf("journal stage %q, credential failures %d", journal.reason, worstCredentialFailures(service))
	}
	if _, err := service.Crashes(t.Context(), kubechanges.CrashQuery{}); err != nil {
		t.Fatalf("crash history over the poisoned Pod: %v", err)
	}
	if _, err := service.Changes(t.Context(), ChangeQuery{}); err != nil {
		t.Fatalf("change history over the poisoned workload: %v", err)
	}
}
