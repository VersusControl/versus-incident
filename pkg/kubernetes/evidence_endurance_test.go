package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// The API fixture injects credential faults; all durable operations use the
// actual selected provider. This is not a kind or EKS acceptance substitute.
type enduranceStore struct {
	storage.Provider
	storage.EvidenceStore
	storage.BlobCAS
	reads, writes, swaps, commits, failures, bytes, latencyNS atomic.Int64
	fail                                                      atomic.Bool
}

func (p *enduranceStore) ReadBlob(key string) ([]byte, error) {
	p.reads.Add(1)
	return p.Provider.ReadBlob(key)
}
func (p *enduranceStore) WriteBlob(key string, value []byte) error {
	p.writes.Add(1)
	p.bytes.Add(int64(len(value)))
	if p.fail.Load() {
		p.failures.Add(1)
		return errors.New("synthetic storage unavailable")
	}
	return p.Provider.WriteBlob(key, value)
}
func (p *enduranceStore) CompareAndSwapBlob(key string, old, value []byte) (bool, error) {
	p.swaps.Add(1)
	p.bytes.Add(int64(len(value)))
	if p.fail.Load() {
		p.failures.Add(1)
		return false, errors.New("synthetic storage unavailable")
	}
	return p.BlobCAS.CompareAndSwapBlob(key, old, value)
}
func (p *enduranceStore) CommitEvidence(ctx context.Context, scope storage.EvidenceScope, batch storage.EvidenceBatch) error {
	started := time.Now()
	defer func() { p.latencyNS.Add(time.Since(started).Nanoseconds()) }()
	if p.fail.Load() {
		p.failures.Add(1)
		return errors.New("synthetic storage unavailable")
	}
	err := p.EvidenceStore.CommitEvidence(ctx, scope, batch)
	if err != nil {
		p.failures.Add(1)
	} else {
		p.commits.Add(1)
	}
	return err
}

func TestDurableEvidenceEndurance(t *testing.T) {
	backend := os.Getenv("EVIDENCE_INTEGRATION_BACKEND")
	if backend == "" {
		t.Skip("explicit isolated endurance backend required")
	}
	duration := 2 * time.Hour
	smoke := os.Getenv("EVIDENCE_SOAK_SMOKE") == "1"
	if smoke {
		duration = 90 * time.Second
	}
	data := os.Getenv("HARNESS_DATA_DIR")
	if data == "" {
		t.Fatal("run.sh data directory required")
	}
	var base storage.Provider
	var err error
	switch backend {
	case "file":
		base, err = storage.NewFile(storage.FileOptions{DataDir: filepath.Join(data, "durable-file")})
	case "postgres":
		dsn := os.Getenv("TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Fatal("real isolated Postgres required, not a skip")
		}
		base, err = storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
	default:
		t.Fatal("unknown explicit backend")
	}
	if err != nil {
		t.Fatal("real provider initialization failed")
	}
	defer base.Close()
	observed := &enduranceStore{Provider: base, EvidenceStore: base.(storage.EvidenceStore), BlobCAS: base.(storage.BlobCAS)}
	fixture := newFakeCluster(t, "initially-unaccepted")
	var authorityDeny atomic.Bool
	var authorityMaterialDenials, authorityProofs, authorityCatalogReads atomic.Int64
	var authorityProofTarget atomic.Pointer[string]
	fixture.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api" {
			authorityCatalogReads.Add(1)
		}
		if authorityDeny.Load() {
			if limit := r.URL.Query().Get("limit"); limit != "" && limit != "1" {
				authorityMaterialDenials.Add(1)
			}
			if r.URL.Query().Get("limit") == "1" && authorityMaterialDenials.Load() > 0 {
				if authorityProofTarget.Load() == nil {
					target := r.URL.Path
					authorityProofTarget.CompareAndSwap(nil, &target)
				}
				if target := authorityProofTarget.Load(); target != nil && r.URL.Path == *target {
					authorityProofs.Add(1)
				}
			}
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		fixture.ServeHTTP(w, r)
	})
	healthy := newFakeCluster(t, "synthetic-healthy", "healthy")
	for _, api := range []*fakeCluster{fixture, healthy} {
		api.serve("namespaces", map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "kube-system", "uid": "synthetic-physical"}}, map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "default", "uid": "synthetic-default"}})
	}
	setBroken := func(sequence int) {
		fixture.serve("pods", map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "api", "uid": "broken-pod", "namespace": "default", "resourceVersion": fmt.Sprint(sequence)}, "spec": map[string]any{"containers": []any{map[string]any{"name": "app"}}}, "status": map[string]any{"phase": "Running", "containerStatuses": []any{map[string]any{"name": "app", "restartCount": sequence + 2, "state": map[string]any{"waiting": map[string]any{"reason": "CrashLoopBackOff"}}}}}}, map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": map[string]any{"name": "churn", "uid": fmt.Sprintf("churn-%d", sequence), "namespace": "default"}, "status": map[string]any{"phase": "Running"}})
	}
	setBroken(0)
	role := &renewableRoleClock{now: time.Now, lifetime: 30 * time.Minute}
	var providerFail atomic.Bool
	var refreshes atomic.Int64
	source := newEKSSource("synthetic-cluster", "us-east-1", assumeRoleCredentials(role, "synthetic-role"), time.Now).(*expiringBearerSource)
	mint := source.refresh
	source.refresh = func(ctx context.Context) (string, time.Time, error) {
		refreshes.Add(1)
		if providerFail.Load() {
			return "", time.Time{}, ErrCredentialUnavailable
		}
		token, expires, err := mint(ctx)
		if err == nil {
			fixture.acceptToken(token)
		}
		return token, expires, err
	}
	makeService := func(id string, api *fakeCluster, credentials CredentialSource) *Service {
		client, err := NewClient(api.trustedConfig(credentials))
		if err != nil {
			t.Fatal(err)
		}
		service := NewService(client, Scope{OrgID: "synthetic-endurance", ClusterID: id, CredentialID: id + "-configured"}, time.Hour)
		service.SetChangeStorage(observed)
		return service
	}
	rotating := makeService("rotating", fixture, source)
	stable := makeService("healthy", healthy, staticCredential(t, "synthetic-healthy"))
	registry := NewClusterRegistryWithScope(true, []ClusterEntry{{Info: ClusterInfo{ID: "rotating", Provider: "generic"}, Service: rotating}, {Info: ClusterInfo{ID: "healthy", Provider: "generic"}, Service: stable}}, tenancy.NewOrgScope("synthetic-endurance"))
	defer registry.Close()
	registry.StartCapture(t.Context())
	current := func(id string) *Service {
		registry.mu.Lock()
		defer registry.mu.Unlock()
		for _, entry := range registry.entries {
			if entry.Info.ID == id {
				return entry.Service
			}
		}
		t.Fatal("configured cluster disappeared")
		return nil
	}
	revision := func(service *Service) (string, time.Time, int) {
		service.recovery.mu.Lock()
		defer service.recovery.mu.Unlock()
		return service.recovery.revision, service.recovery.lastCommitted, service.recovery.workers
	}
	waitUntil(t, 45*time.Second, "autonomous nonempty initial publication", func() bool {
		rev, _, _ := revision(current("rotating"))
		healthyRev, _, _ := revision(current("healthy"))
		return rev != "" && healthyRev != "" && observed.commits.Load() > 0
	})
	// Reads occur only after the background publication has already been observed.
	assertBodies := func() {
		service := current("rotating")
		overview, err := service.Overview(t.Context())
		if err != nil || overview.Pods < 1 || overview.Nodes < 1 || overview.AllocatableCPU == "" || overview.AllocatableMemory == "" {
			t.Fatal("nonzero capacity response lost after autonomous publication")
		}
		issues, err := service.Issues(t.Context(), IssueOptions{Broken: true})
		if err != nil || issues.Broken.Total < 1 {
			t.Fatal("nonzero current issues lost")
		}
	}
	assertBodies()
	output, err := os.OpenFile(filepath.Join(os.Getenv("HARNESS_RESULTS_DIR"), "endurance-"+backend+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	encoder := json.NewEncoder(output)
	started := time.Now()
	initialRefresh := refreshes.Load()
	initialRole := role.calls.Load()
	initialCommits := observed.commits.Load()
	initialGoroutines := runtime.NumGoroutine()
	oldRevision, _, _ := revision(current("rotating"))
	firstIdentity := current("rotating").identity
	progress := 0
	epochs := 0
	var lastIdentity = firstIdentity
	phase := "healthy-baseline"
	stopWorkerAt, outageAt, recoverAt, storageAt, storageRecoverAt := 40*time.Minute, 16*time.Minute, 18*time.Minute, 50*time.Minute, 51*time.Minute
	if smoke {
		stopWorkerAt, outageAt, recoverAt, storageAt, storageRecoverAt = 20*time.Second, 30*time.Second, 40*time.Second, 55*time.Second, 65*time.Second
	}
	authorityAt, authorityRecoverAt := 26*time.Minute, 28*time.Minute
	if smoke {
		authorityAt, authorityRecoverAt = 70*time.Second, 110*time.Second
		duration = 140 * time.Second
	}
	var authorityStarted, authorityReleased, authorityObserved, authorityRecovered bool
	var authorityIdentity *credentialEpoch
	var authorityCheckpoint string
	var authorityCatalogBaseline, authorityCommits int64
	var stopped, outage, recovered, storageFailed, storageRecovered bool
	var stoppedDone <-chan struct{}
	autonomousWorkerRestart := false
	baselineRequests := 0
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for time.Since(started) < duration {
		select {
		case <-t.Context().Done():
			t.Fatal("bounded endurance canceled before wall-clock requirement")
		case <-ticker.C:
		}
		elapsed := time.Since(started)
		setBroken(int(elapsed / (20 * time.Second)))
		service := current("rotating")
		rev, at, workers := revision(service)
		healthyRev, healthyAt, healthyWorkers := revision(current("healthy"))
		if rev != "" && rev != oldRevision {
			progress++
			oldRevision = rev
		}
		if service.identity != lastIdentity {
			epochs++
			lastIdentity = service.identity
		}
		if workers > 1 || healthyWorkers > 1 {
			t.Fatalf("unbounded active workers rotating=%d healthy=%d", workers, healthyWorkers)
		}
		if elapsed > time.Minute && (healthyRev == "" || time.Since(healthyAt) > 2*time.Minute) && !observed.fail.Load() {
			t.Fatal("failing cluster stalled healthy cluster")
		}
		if !authorityStarted && elapsed >= authorityAt {
			authorityIdentity = service.identity
			authorityCatalogBaseline = authorityCatalogReads.Load()
			authorityDeny.Store(true)
			authorityStarted = true
			phase = "authority-denial"
		}
		if authorityStarted && !authorityReleased {
			if service.identity != authorityIdentity {
				t.Fatal("whole403 caused a credential epoch replacement instead of same-scope authority handling")
			}
			if authorityMaterialDenials.Load() > 0 {
				report, reportErr := service.SyncHealth(t.Context())
				if carried, ok := SyncHealthReport(reportErr); ok {
					report = carried
				}
				if report.State == "ready" {
					t.Fatal("natural cached material403 still reports ready")
				}
				if report.State == "error" && report.FailingKinds > 0 && report.Kinds["Pod"].Error == "forbidden" && authorityProofs.Load() > 0 {
					checkpoint, checkpointErr := observed.EvidenceStore.EvidenceCheckpoint(t.Context(), storage.EvidenceScope{OrgID: "synthetic-endurance", ClusterID: "rotating", ClusterUID: "synthetic-physical"})
					if checkpointErr != nil {
						t.Fatal(checkpointErr)
					}
					if !authorityObserved {
						authorityCheckpoint = checkpoint.ID
						authorityCommits = observed.commits.Load()
						authorityObserved = true
					} else if checkpoint.ID != authorityCheckpoint {
						t.Fatal("denied authority advanced the committed evidence checkpoint")
					}
				}
			}
		}
		if authorityStarted && !authorityReleased && elapsed >= authorityRecoverAt {
			if !authorityObserved || authorityProofs.Load() < 2 {
				t.Fatal("actual current material denial and producer retry were not observed")
			}
			if authorityCatalogReads.Load() != authorityCatalogBaseline {
				t.Fatal("authority proof relied on catalog refresh rather than the warm1h path")
			}
			authorityDeny.Store(false)
			authorityReleased = true
			phase = "authority-recovery"
		}
		if authorityReleased && !authorityRecovered && workers == 1 && time.Since(at) < 30*time.Second && observed.commits.Load() > authorityCommits {
			report, reportErr := service.SyncHealth(t.Context())
			if reportErr == nil && report.State == "ready" && report.FailingKinds == 0 {
				checkpoint, checkpointErr := observed.EvidenceStore.EvidenceCheckpoint(t.Context(), storage.EvidenceScope{OrgID: "synthetic-endurance", ClusterID: "rotating", ClusterUID: "synthetic-physical"})
				if checkpointErr == nil && checkpoint.ID != authorityCheckpoint {
					authorityRecovered = true
					phase = "authority-recovered"
				}
			}
		}
		if !stopped && elapsed >= stopWorkerAt {
			service.changes.mu.Lock()
			cache := service.changes.inventories[service.indexScope()]
			cancel := cache.cancel
			stoppedDone = cache.done
			service.changes.mu.Unlock()
			cancel()
			stopped = true
			phase = "worker-stopped"
		}
		if stopped && !autonomousWorkerRestart {
			service.changes.mu.Lock()
			cache := service.changes.inventories[service.indexScope()]
			autonomousWorkerRestart = cache != nil && cache.done != stoppedDone
			service.changes.mu.Unlock()
		}
		if !outage && elapsed >= outageAt {
			providerFail.Store(true)
			fixture.mu.Lock()
			clear(fixture.extra)
			fixture.token = "reject-all"
			fixture.mu.Unlock()
			outage = true
			phase = "credential-outage"
		}
		if outage && !recovered && elapsed >= recoverAt {
			providerFail.Store(false)
			recovered = true
			phase = "credential-recovery"
		}
		if !storageFailed && elapsed >= storageAt {
			observed.fail.Store(true)
			storageFailed = true
			phase = "storage-error"
		}
		if storageFailed && !storageRecovered && elapsed >= storageRecoverAt {
			observed.fail.Store(false)
			storageRecovered = true
			phase = "recovered-churn"
		}
		if elapsed >= time.Minute && baselineRequests == 0 {
			baselineRequests = fixture.requestCount() + healthy.requestCount()
		}
		var memory runtime.MemStats
		runtime.ReadMemStats(&memory)
		var usage syscall.Rusage
		_ = syscall.Getrusage(syscall.RUSAGE_SELF, &usage)
		rss := usage.Maxrss
		if runtime.GOOS != "darwin" {
			rss *= 1024
		}
		sample := map[string]any{"wall_seconds": elapsed.Seconds(), "smoke_only": smoke, "phase": phase, "goroutines": runtime.NumGoroutine(), "baseline_goroutines": initialGoroutines, "peak_rss_bytes": rss, "heap_bytes": memory.HeapAlloc, "workers": workers, "healthy_workers": healthyWorkers, "requests": fixture.requestCount() + healthy.requestCount(), "baseline_first_minute_requests": baselineRequests, "refreshes": refreshes.Load(), "role_sessions": role.calls.Load(), "epochs": epochs, "revision_changes": progress, "revision": rev, "last_success_age_seconds": time.Since(at).Seconds(), "blob_reads": observed.reads.Load(), "blob_writes": observed.writes.Load(), "cas_operations": observed.swaps.Load(), "evidence_commits": observed.commits.Load(), "storage_failures": observed.failures.Load(), "encoded_blob_bytes": observed.bytes.Load(), "evidence_commit_total_latency_ns": observed.latencyNS.Load(), "autonomous_worker_restart": autonomousWorkerRestart}
		sample["authority_material_denials"] = authorityMaterialDenials.Load()
		sample["authority_producer_proofs"] = authorityProofs.Load()
		sample["authority_same_scope_denial_observed"] = authorityObserved
		sample["authority_recovered"] = authorityRecovered
		sample["authority_catalog_reads"] = authorityCatalogReads.Load()
		sample["authority_checkpoint_at_denial"] = authorityCheckpoint
		if accessor, ok := base.(storage.SQLAccessor); ok {
			var commits, rollbacks, inserts, updates, deletes int64
			queryCtx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			dbErr := accessor.DB().QueryRowContext(queryCtx, "SELECT xact_commit,xact_rollback,tup_inserted,tup_updated,tup_deleted FROM pg_stat_database WHERE datname=current_database()").Scan(&commits, &rollbacks, &inserts, &updates, &deletes)
			cancel()
			if dbErr != nil {
				t.Fatal("database metrics unavailable")
			}
			sample["database"] = map[string]int64{"transactions": commits, "rollbacks": rollbacks, "inserts": inserts, "updates": updates, "deletes": deletes}
		}
		if err = encoder.Encode(sample); err != nil {
			t.Fatal(err)
		}
		if err = output.Sync(); err != nil {
			t.Fatal(err)
		}
		t.Logf("heartbeat backend=%s seconds=%.0f phase=%s revisions=%d refreshes=%d workers=%d/%d commits=%d", backend, elapsed.Seconds(), phase, progress, refreshes.Load(), workers, healthyWorkers, observed.commits.Load())
	}
	providerFail.Store(false)
	observed.fail.Store(false)
	authorityDeny.Store(false)
	if !authorityObserved || !authorityRecovered || authorityProofs.Load() < 2 {
		t.Fatal("required same-scope current-authority denial/retry/recovery did not complete")
	}
	waitUntil(t, 45*time.Second, "autonomous final recovery", func() bool {
		_, at, workers := revision(current("rotating"))
		return workers == 1 && time.Since(at) < 30*time.Second
	})
	assertBodies()
	if !autonomousWorkerRestart || progress < 2 || observed.commits.Load() <= initialCommits {
		t.Fatal("no autonomous producer/revision/history progress")
	}
	if !smoke && (time.Since(started) < 2*time.Hour || refreshes.Load()-initialRefresh < 5 || role.calls.Load()-initialRole < 2 || epochs < 5) {
		t.Fatal("actual two-hour bearer/role renewal minimum not met")
	}
	t.Logf("completed wall_seconds=%.3f smoke_only=%t epochs=%d revisions=%d refreshes=%d roles=%d requests=%d commits=%d", time.Since(started).Seconds(), smoke, epochs, progress, refreshes.Load(), role.calls.Load(), fixture.requestCount()+healthy.requestCount(), observed.commits.Load())
}
