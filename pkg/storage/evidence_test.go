package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	_ "github.com/VersusControl/versus-incident/pkg/storage/sqlitedriver"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func evidenceTestBatch(id, expected string, at time.Time, present, complete bool) EvidenceBatch {
	batch := EvidenceBatch{ID: id, Expected: expected, At: at, Epoch: "synthetic-epoch"}
	if complete {
		batch.CoveredThrough = at
	}
	if present {
		batch.Issues = []EvidenceIssue{{Key: "resource-rule", Namespace: "shop", Kind: "Pod", UID: "pod-uid", Rule: "pod.crash_loop", Payload: json.RawMessage(`{"rule":"pod.crash_loop"}`)}}
	}
	return batch
}

func TestDriverFreeEmbeddingKeepsOrdinaryStorage(t *testing.T) {
	code := `package main
import("context";"os";"strings";"github.com/VersusControl/versus-incident/pkg/storage")
func main(){p:=storage.NewMemory();if p.WriteBlob("ordinary",[]byte("ok"))!=nil{panic("memory KV")};f,e:=storage.NewFile(storage.FileOptions{DataDir:os.Args[1]});if e!=nil{panic(e)};defer f.Close();if f.WriteBlob("ordinary",[]byte("ok"))!=nil{panic("file KV")};for _,s:=range []storage.Provider{p,f}{_,e=s.(storage.EvidenceStore).EvidenceCheckpoint(context.Background(),storage.EvidenceScope{OrgID:"default",ClusterID:"c",ClusterUID:"u"});if e==nil||!strings.Contains(e.Error(),"storage/sqlitedriver"){panic("missing actionable driver error")}}}`
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	if err := os.WriteFile(path, []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("go", "run", path, filepath.Join(dir, "store")).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
}

func TestClosedOccurrenceReadModelsArePrunedWithHistory(t *testing.T) {
	provider, err := NewFile(FileOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	scope := EvidenceScope{OrgID: "default", ClusterID: "closed", ClusterUID: "physical"}
	at := time.Now().UTC().Add(-48 * time.Hour)
	state := evidenceState{Occurrences: map[string]EvidenceIssue{}, Checkpoint: EvidenceCheckpoint{ID: "old", At: at, OccurrenceRevision: 1}}
	for i := 0; i < 600; i++ {
		id := fmt.Sprint(i)
		payload, _ := json.Marshal(map[string]string{"detail": strings.Repeat("x", 8192), "severity": "warning"})
		state.Occurrences[id] = EvidenceIssue{OccurrenceID: id, Key: id, UID: id, Kind: "Pod", FirstSeen: at, LastSeen: at, ClosedAt: &at, Payload: payload}
	}
	key, _ := evidenceScopeKey(scope)
	raw, _ := json.Marshal(state)
	if err = provider.WriteBlob(key, raw); err != nil {
		t.Fatal(err)
	}
	store := provider.(EvidenceStore)
	batch := EvidenceBatch{ID: "continues", Expected: "old", At: time.Now().UTC(), Epoch: "active"}
	if err = store.CommitEvidence(t.Context(), scope, batch); err != nil {
		t.Fatalf("orphaned closed rows stopped capture: %v", err)
	}
	after, err := blobEvidenceRead(t.Context(), provider, scope)
	if err != nil || len(after.Occurrences) != 0 || after.Checkpoint.PruneRevision == 0 {
		t.Fatalf("closed metadata escaped retention: rows=%d %v", len(after.Occurrences), err)
	}
}

func TestPinnedOccurrenceSampleDoesNotChangeAfterUnchangedCommit(t *testing.T) {
	for _, backend := range []string{"memory", "file"} {
		t.Run(backend, func(t *testing.T) {
			provider, _ := localEvidenceTestProvider(t, backend)
			store := provider.(EvidenceStore)
			scope := EvidenceScope{OrgID: "default", ClusterID: "sample", ClusterUID: "physical"}
			at := time.Now().UTC().Add(-time.Hour)
			first := evidenceTestBatch("first", "", at, true, true)
			for i := 0; i < 4; i++ {
				issue := first.Issues[0]
				issue.Key = fmt.Sprint(i)
				issue.UID = fmt.Sprint(i)
				first.Issues = append(first.Issues, issue)
			}
			if err := store.CommitEvidence(t.Context(), scope, first); err != nil {
				t.Fatal(err)
			}
			q := EvidenceQuery{Since: at.Add(-time.Minute), Until: at.Add(time.Hour), Type: "occurrence", Limit: 1}
			page, err := store.QueryEvidence(t.Context(), scope, q)
			if err != nil || page.Next == "" {
				t.Fatal(err)
			}
			q.Cursor = page.Next
			before, err := store.QueryEvidence(t.Context(), scope, q)
			if err != nil {
				t.Fatal(err)
			}
			next := first
			next.ID, next.Expected, next.At = "later", "first", at.Add(time.Minute)
			if err = store.CommitEvidence(t.Context(), scope, next); err != nil {
				t.Fatal(err)
			}
			after, err := store.QueryEvidence(t.Context(), scope, q)
			if err != nil || !reflect.DeepEqual(before.Items, after.Items) {
				t.Fatalf("genuine cursor changed sample: %v", err)
			}
		})
	}
}

func TestAuthorityBatchCannotBorrowPriorPrincipal(t *testing.T) {
	at := time.Now().UTC()
	state := evidenceState{Checkpoint: EvidenceCheckpoint{ID: "old", At: at, Principal: "verified"}}
	batch := EvidenceBatch{ID: "new", Expected: "old", At: at.Add(time.Second), Epoch: "unknown", Authority: map[string]EvidenceAuthority{"Pod": {State: "allowed", ProvedAt: at}}}
	if _, err := applyEvidence(&state, batch); !errors.Is(err, ErrInvalidEvidence) {
		t.Fatal("unverified producer borrowed principal proofs")
	}
}

func localEvidenceTestProvider(t *testing.T, backend string) (Provider, *localEvidence) {
	t.Helper()
	var provider Provider = NewMemory()
	var err error
	if backend == "file" {
		provider, err = NewFile(FileOptions{DataDir: t.TempDir()})
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	var index *localEvidence
	if p, ok := provider.(*memoryProvider); ok {
		index, err = p.evidenceIndex()
	} else {
		index, err = provider.(*fileProvider).evidenceIndex()
	}
	if err != nil {
		t.Fatal(err)
	}
	return provider, index
}

func TestLocalEvidencePrivateModesAndLegacyDirectoryBoundary(t *testing.T) {
	provider, index := localEvidenceTestProvider(t, "file")
	p := provider.(*fileProvider)
	if _, err := index.db.Exec(`CREATE TABLE mode_probe(value TEXT)`); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(filepath.Join(p.dir, "kubernetes-evidence.sqlite") + suffix)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0077 != 0 {
			t.Fatalf("shared artifact mode %s %o", suffix, info.Mode().Perm())
		}
	}
	root, outside := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "captured-evidence"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "kubernetes")); err != nil {
		t.Fatal(err)
	}
	if opened, err := NewFile(FileOptions{DataDir: root}); err == nil {
		opened.Close()
		t.Fatal("symlinked legacy directory accepted")
	}
	root = t.TempDir()
	if err := os.Symlink(filepath.Join(outside, "captured-evidence"), filepath.Join(root, "kubernetes-evidence.sqlite-journal")); err != nil {
		t.Fatal(err)
	}
	if opened, err := NewFile(FileOptions{DataDir: root}); err == nil {
		_, err = opened.(EvidenceStore).EvidenceCheckpoint(t.Context(), EvidenceScope{OrgID: "default", ClusterID: "journal", ClusterUID: "physical"})
		opened.Close()
		if err == nil {
			t.Fatal("linked rollback journal accepted")
		}
	}
}

func TestLocalEvidenceRejectsLinkedDatabaseArtifacts(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.WriteFile(outside, []byte("untouched"), 0600); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(root, "kubernetes-evidence.sqlite")
			var err error
			if kind == "symlink" {
				err = os.Symlink(outside, target)
			} else {
				err = os.Link(outside, target)
			}
			if err != nil {
				t.Fatal(err)
			}
			if provider, err := NewFile(FileOptions{DataDir: root}); err == nil {
				provider.Close()
				t.Fatal("linked database accepted")
			}
			data, err := os.ReadFile(outside)
			if err != nil || string(data) != "untouched" {
				t.Fatal("native index modified a foreign file")
			}
		})
	}
}

func TestLocalEvidenceGrowingCorpusUsesBoundedIndexedReads(t *testing.T) {
	for _, backend := range []string{"memory", "file"} {
		t.Run(backend, func(t *testing.T) {
			provider, index := localEvidenceTestProvider(t, backend)
			store := provider.(EvidenceStore)
			scope := EvidenceScope{OrgID: "default", ClusterID: "growth", ClusterUID: "physical"}
			origin := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
			previous := 0
			for _, size := range []int{128, 2048, 8192} {
				cp, err := store.EvidenceCheckpoint(t.Context(), scope)
				if err != nil {
					t.Fatal(err)
				}
				batch := EvidenceBatch{ID: fmt.Sprint(size), Expected: cp.ID, Generation: cp.Generation, At: origin.Add(time.Duration(size) * time.Millisecond), Epoch: "stable"}
				for i := previous; i < size; i++ {
					kind := "change"
					if i%3 == 0 {
						kind = "crash"
					}
					namespace := "shop"
					if i%2 == 0 {
						namespace = "ops"
					}
					batch.Records = append(batch.Records, EvidenceRecord{ID: fmt.Sprintf("event-%05d", i), Type: kind, At: origin.Add(time.Duration(i) * time.Millisecond),
						ObservedAt: batch.At, Namespace: namespace, Kind: "Deployment", AuthorizationKind: "Pod", RootUID: fmt.Sprintf("root-%d", i%257), UID: fmt.Sprint(i), Epoch: "stable", Payload: json.RawMessage(`{"value":"bounded"}`)})
				}
				if err = store.CommitEvidence(t.Context(), scope, batch); err != nil {
					t.Fatal(err)
				}
				previous = size
				for _, query := range []EvidenceQuery{
					{Type: "timeline"}, {Type: "crash", ByObservation: true}, {Type: "change", Namespace: "shop", RootUID: "root-11"},
					{Type: "change", RootUID: "absent"}, {Type: "change", ResourceUID: "root-11"},
				} {
					query.Since, query.Until, query.Limit = origin.Add(-time.Minute), origin.Add(time.Hour), 7
					expected, err := blobEvidenceQuery(t.Context(), provider, store.EvidencePolicy(), scope, query)
					if err != nil {
						t.Fatal(err)
					}
					rawBefore, payloadBefore, countBefore := index.rawReads.Load(), index.payloadReads.Load(), index.countReads.Load()
					page, err := store.QueryEvidence(t.Context(), scope, query)
					if err != nil {
						t.Fatalf("size=%d query=%+v: %v", size, query, err)
					}
					if !reflect.DeepEqual(page.Items, expected.Items) || page.Next != expected.Next || page.Total != expected.Total {
						t.Fatalf("native page differs at size=%d query=%+v", size, query)
					}
					if index.rawReads.Load() != rawBefore {
						t.Fatal("history read selected the retained CAS image")
					}
					if read := index.payloadReads.Load() - payloadBefore; read > 8 {
						t.Fatalf("decoded %d payloads for page+1=8", read)
					}
					if read := index.countReads.Load() - countBefore; read > 1001 {
						t.Fatalf("count read exceeded cap: %d", read)
					}
					if page.Next != "" {
						query.Cursor = page.Next
						rawBefore, payloadBefore, countBefore = index.rawReads.Load(), index.payloadReads.Load(), index.countReads.Load()
						next, err := store.QueryEvidence(t.Context(), scope, query)
						if err != nil || next.Total != 0 {
							t.Fatalf("continuation: %+v %v", next, err)
						}
						if index.rawReads.Load() != rawBefore || index.payloadReads.Load()-payloadBefore > 8 || index.countReads.Load() != countBefore {
							t.Fatal("continuation read history or recounted")
						}
					}
				}
			}
			key, _ := evidenceScopeKey(scope)
			rows, err := index.db.QueryContext(t.Context(), `EXPLAIN QUERY PLAN SELECT raw FROM evidence_events WHERE key=? AND root=? AND at>=? ORDER BY at DESC,id DESC LIMIT 8`, key, "absent", localEvidenceTime(origin))
			if err != nil {
				t.Fatal(err)
			}
			indexed := false
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				indexed = indexed || strings.Contains(detail, "evidence_event_root")
				if strings.Contains(detail, "SCAN evidence_events") {
					t.Fatal("root query performed an all-history scan")
				}
			}
			rows.Close()
			if !indexed {
				t.Fatal("root lookup did not use its native index")
			}
		})
	}
}

func TestEvidenceTimelineCrashMarkerKeepsLegacyAndRejectsStatusObservations(t *testing.T) {
	// A crash record that declares no subtype is a marker written before crash
	// observations carried one, so it stays on the timeline. A declared status
	// observation is not a marker, and a declared subtype that is not a string is
	// not one either. A declared termination keeps the marker it has always had, and
	// a subtype declared under another key is not a declared subtype. Every backend
	// must agree and must still return every record from the crash history itself.
	for _, backend := range []string{"memory", "file", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var provider Provider
			var index *localEvidence
			scope := EvidenceScope{OrgID: "default", ClusterID: "crash-marker", ClusterUID: "physical"}
			if backend == "postgres" {
				dsn := os.Getenv("TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("isolated Postgres required")
				}
				var err error
				provider, err = NewPostgres(PostgresOptions{DSN: dsn})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = provider.Close() })
				scope.ClusterID = fmt.Sprintf("crash-marker-%d", time.Now().UnixNano())
			} else {
				provider, index = localEvidenceTestProvider(t, backend)
			}
			store := provider.(EvidenceStore)
			origin := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
			cp, err := store.EvidenceCheckpoint(t.Context(), scope)
			if err != nil {
				t.Fatal(err)
			}
			batch := EvidenceBatch{ID: "crash-marker", Expected: cp.ID, Generation: cp.Generation, At: origin.Add(10 * time.Second), Epoch: "stable"}
			crash := func(id string, at time.Time, payload string) EvidenceRecord {
				return EvidenceRecord{ID: id, Type: "crash", At: at, ObservedAt: batch.At, Namespace: "shop", Name: "api", Kind: "Deployment", AuthorizationKind: "Pod", UID: "pod-uid", RootUID: "root-uid", PodUID: "pod-uid", Epoch: batch.Epoch, Payload: json.RawMessage(payload)}
			}
			legacy := crash("crash:legacy", origin.Add(2*time.Second), `{"cluster":"crash-marker","namespace":"shop","pod":"api","container":"app","reason":"Error","exit_code":1,"restart_count":2}`)
			status := crash("crash:status", origin.Add(3*time.Second), `{"cluster":"crash-marker","namespace":"shop","pod":"api","container":"app","type":"status","waiting_reason":"CrashLoopBackOff","restart_count":2}`)
			termination := crash("crash:termination", origin.Add(4*time.Second), `{"cluster":"crash-marker","namespace":"shop","pod":"api","container":"app","type":"termination","reason":"Error","exit_code":1}`)
			numeric := crash("crash:numeric", origin.Add(5*time.Second), `{"cluster":"crash-marker","namespace":"shop","pod":"api","container":"app","type":1,"restart_count":2}`)
			capitalized := crash("crash:capitalized", origin.Add(6*time.Second), `{"cluster":"crash-marker","namespace":"shop","pod":"api","container":"app","Type":"status","restart_count":2}`)
			nullSubtype := crash("crash:null-subtype", origin.Add(7*time.Second), `{"cluster":"crash-marker","namespace":"shop","pod":"api","container":"app","type":null,"restart_count":2}`)
			changePayload, err := json.Marshal(map[string]any{"cluster": "crash-marker", "kind": "Deployment", "name": "api", "type": "created"})
			if err != nil {
				t.Fatal(err)
			}
			change := EvidenceRecord{ID: "change:created", Type: "change", At: origin.Add(time.Second), ObservedAt: batch.At, Namespace: "shop", Name: "api", Kind: "Deployment", AuthorizationKind: "Deployment", UID: "root-uid", RootUID: "root-uid", Epoch: batch.Epoch, Payload: changePayload}
			batch.Records = []EvidenceRecord{change, legacy, status, termination, numeric, capitalized, nullSubtype}
			if err = store.CommitEvidence(t.Context(), scope, batch); err != nil {
				t.Fatal(err)
			}
			query := EvidenceQuery{Type: "timeline", Since: origin.Add(-time.Second), Until: batch.At, Limit: 10}
			page, err := store.QueryEvidence(t.Context(), scope, query)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, len(page.Items))
			for i, item := range page.Items {
				got[i] = item.ID
			}
			want := []string{nullSubtype.ID, capitalized.ID, termination.ID, legacy.ID, change.ID}
			if !reflect.DeepEqual(got, want) || page.Total != 5 || page.Counts["crash"] != 4 || page.Counts["change"] != 1 {
				t.Fatalf("%s timeline=%v total=%d counts=%v", backend, got, page.Total, page.Counts)
			}
			if index != nil {
				retained, err := blobEvidenceQuery(t.Context(), provider, store.EvidencePolicy(), scope, query)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(retained.Items, page.Items) || retained.Total != page.Total || !reflect.DeepEqual(retained.Counts, page.Counts) {
					t.Fatalf("%s retained history disagrees with the index: %+v vs %+v", backend, retained, page)
				}
			}
			history, err := store.QueryEvidence(t.Context(), scope, EvidenceQuery{Type: "crash", Since: origin.Add(-time.Second), Until: batch.At, Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if history.Total != 6 || len(history.Items) != 6 {
				t.Fatalf("%s crash history dropped a recorded observation: total=%d items=%d", backend, history.Total, len(history.Items))
			}
		})
	}
}

func TestLocalEvidenceLegacyImportReopenAndDeletionStayIndexed(t *testing.T) {
	source := NewMemory()
	defer source.Close()
	scope := EvidenceScope{OrgID: "default", ClusterID: "legacy", ClusterUID: "physical"}
	at := time.Now().UTC().Add(-time.Minute)
	if err := source.(EvidenceStore).CommitEvidence(t.Context(), scope, evidenceTestBatch("open", "", at, true, true)); err != nil {
		t.Fatal(err)
	}
	key, _ := evidenceScopeKey(scope)
	raw, err := source.ReadBlob(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, filepath.FromSlash(key)+".json")
	if err = os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	provider, err := NewFile(FileOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	store := provider.(EvidenceStore)
	query := EvidenceQuery{Since: at.Add(-time.Minute), Until: at.Add(time.Hour), Type: "occurrence", Limit: 1}
	page, err := store.QueryEvidence(t.Context(), scope, query)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("legacy import: %+v %v", page, err)
	}
	if err = provider.Close(); err != nil {
		t.Fatal(err)
	}
	// A stale legacy image is no longer authoritative after migration.
	if err = os.WriteFile(path, []byte("stale legacy image"), 0600); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewFile(FileOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	index, err := reopened.(*fileProvider).evidenceIndex()
	if err != nil {
		t.Fatal(err)
	}
	page, err = reopened.(EvidenceStore).QueryEvidence(t.Context(), scope, query)
	if err != nil || len(page.Items) != 1 || index.rawReads.Load() != 0 {
		t.Fatalf("cold read traversed history: %+v %v", page, err)
	}
	if err = reopened.(EvidenceStore).DeleteEvidence(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	if err = reopened.Close(); err != nil {
		t.Fatal(err)
	}
	final, err := NewFile(FileOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer final.Close()
	empty, err := final.(EvidenceStore).QueryEvidence(t.Context(), scope, query)
	if err != nil || len(empty.Items) != 0 {
		t.Fatalf("deleted evidence resurrected on reopen: %+v %v", empty, err)
	}
}

func TestLocalEvidencePinsFreshnessAndCASPublication(t *testing.T) {
	for _, backend := range []string{"memory", "file"} {
		t.Run(backend, func(t *testing.T) {
			provider, index := localEvidenceTestProvider(t, backend)
			store := provider.(EvidenceStore)
			scope := EvidenceScope{OrgID: "default", ClusterID: "pins", ClusterUID: "physical"}
			at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
			open := evidenceTestBatch("open", "", at, true, true)
			if err := store.CommitEvidence(t.Context(), scope, open); err != nil {
				t.Fatal(err)
			}
			current, err := store.QueryEvidence(t.Context(), scope, EvidenceQuery{Since: at.Add(-time.Minute), Until: at.Add(time.Hour), Type: "occurrence"})
			if err != nil || len(current.Items) != 1 {
				t.Fatal(err)
			}
			var issue EvidenceIssue
			if err = json.Unmarshal(current.Items[0].Payload, &issue); err != nil {
				t.Fatal(err)
			}
			fresh := evidenceTestBatch("fresh", "open", at.Add(2*time.Minute), true, true)
			if err = store.CommitEvidence(t.Context(), scope, fresh); err != nil {
				t.Fatal(err)
			}
			before := index.rawReads.Load()
			pinned, err := store.QueryEvidence(t.Context(), scope, EvidenceQuery{Since: at.Add(-time.Minute), Until: at.Add(time.Minute), Type: "occurrence", OccurrenceID: issue.OccurrenceID, PointInTime: true})
			if err != nil || len(pinned.Items) != 1 {
				t.Fatalf("PIT=%+v %v", pinned, err)
			}
			var snapshot EvidenceIssue
			if err = json.Unmarshal(pinned.Items[0].Payload, &snapshot); err != nil || !snapshot.LastSeen.Equal(at) {
				t.Fatal("PIT invented freshness between observations")
			}
			if index.rawReads.Load() != before {
				t.Fatal("PIT read the CAS image")
			}
			guard := "kubernetes/guard"
			if err = provider.WriteBlob(guard, []byte("current")); err != nil {
				t.Fatal(err)
			}
			bad := evidenceTestBatch("bad", "fresh", at.Add(3*time.Minute), false, true)
			bad.GuardKey, bad.GuardExpected = guard, []byte("retired")
			if err = store.CommitEvidence(t.Context(), scope, bad); !errors.Is(err, ErrEvidenceConflict) {
				t.Fatalf("guard accepted: %v", err)
			}
			cp, err := store.EvidenceCheckpoint(t.Context(), scope)
			if err != nil || cp.ID != "fresh" {
				t.Fatal("failed CAS published metadata")
			}
			query := EvidenceQuery{Since: at.Add(-time.Minute), Until: at.Add(time.Hour), Type: "issue", Limit: 1}
			journal, err := store.QueryEvidence(t.Context(), scope, query)
			if err != nil || len(journal.Items) != 1 || journal.Items[0].Type != "issue_opened" {
				t.Fatal("failed CAS published rows")
			}
		})
	}
}

func TestEvidenceNamespaceAuthorityAndSourceKindParity(t *testing.T) {
	for _, backend := range []string{"memory", "file", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var provider Provider = NewMemory()
			var err error
			if backend == "file" {
				provider, err = NewFile(FileOptions{DataDir: t.TempDir()})
			}
			if backend == "postgres" {
				dsn := os.Getenv("TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("isolated Postgres required")
				}
				provider, err = NewPostgres(PostgresOptions{DSN: dsn})
			}
			if err != nil {
				t.Fatal(err)
			}
			defer provider.Close()
			store := provider.(EvidenceStore)
			at := time.Now().UTC().Truncate(time.Microsecond)
			scope := EvidenceScope{OrgID: "namespace-proof", ClusterID: fmt.Sprint(time.Now().UnixNano()), ClusterUID: "physical"}
			proofs := map[string]EvidenceAuthority{
				"Pod":      {State: "allowed", ProvedAt: at.Add(-time.Minute)},
				"Pod@shop": {State: "denied", Namespace: "shop", ProvedAt: at},
				"Pod@ops":  {State: "allowed", Namespace: "ops", ProvedAt: at},
			}
			batch := EvidenceBatch{ID: "scope", At: at, Epoch: "stable", Principal: "verified", Authority: proofs}
			for _, namespace := range []string{"shop", "ops"} {
				batch.Records = append(batch.Records, EvidenceRecord{ID: namespace, Type: "change", Namespace: namespace, Kind: "Deployment", AuthorizationKind: "Pod", RootUID: "root-" + namespace, At: at, ObservedAt: at, Epoch: "stable", Payload: json.RawMessage(`{}`)})
			}
			if err = store.CommitEvidence(t.Context(), scope, batch); err != nil {
				t.Fatal(err)
			}
			cp, err := store.EvidenceCheckpoint(t.Context(), scope)
			if err != nil || cp.Authority["Pod@shop"].State != "denied" {
				t.Fatalf("namespace proof not persisted: %+v %v", cp, err)
			}
			query := EvidenceQuery{Since: at.Add(-time.Minute), Until: at.Add(time.Minute), Type: "change", Kind: "Deployment", EnforceAuthority: true, Authorities: cp.Authority}
			page, err := store.QueryEvidence(t.Context(), scope, query)
			if err != nil || len(page.Items) != 2 {
				t.Fatalf("scoped pages=%+v %v", page, err)
			}
			allowed, gaps := 0, 0
			for _, record := range page.Items {
				if record.Type == "coverage_gap" {
					gaps++
					if record.RootUID != "" || record.Namespace != "" {
						t.Fatal("denied namespace leaked identity")
					}
				} else {
					allowed++
					if record.Namespace != "ops" || record.AuthorizationKind != "Pod" {
						t.Fatal("display kind replaced source authority")
					}
				}
			}
			if allowed != 1 || gaps != 1 {
				t.Fatalf("allowed=%d gaps=%d", allowed, gaps)
			}
			// A namespaced grant cannot defeat an equally recent denial.
			query.Authorities = map[string]EvidenceAuthority{"Pod": {State: "denied", ProvedAt: at}, "Pod@ops": {State: "allowed", Namespace: "ops", ProvedAt: at}}
			page, err = store.QueryEvidence(t.Context(), scope, query)
			if err != nil {
				t.Fatal(err)
			}
			for _, record := range page.Items {
				if record.Type != "coverage_gap" {
					t.Fatal("equal-time grant overrode denial")
				}
			}
		})
	}
}

func TestEvidenceSparseFreshnessAndPersistedAuthorityParity(t *testing.T) {
	for _, backend := range []string{"memory", "file", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var provider Provider = NewMemory()
			var err error
			switch backend {
			case "file":
				provider, err = NewFile(FileOptions{DataDir: t.TempDir()})
			case "postgres":
				dsn := os.Getenv("TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("isolated Postgres required")
				}
				provider, err = NewPostgres(PostgresOptions{DSN: dsn})
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = provider.Close() })
			store := provider.(EvidenceStore)
			scope := EvidenceScope{OrgID: "sparse", ClusterID: fmt.Sprint(time.Now().UnixNano()), ClusterUID: "physical"}
			origin := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)
			for i := 0; i <= 15; i++ {
				at := origin.Add(time.Duration(i) * time.Minute)
				cp, err := store.EvidenceCheckpoint(t.Context(), scope)
				if err != nil {
					t.Fatal(err)
				}
				payload, _ := json.Marshal(map[string]any{"root": map[string]string{"uid": "root", "kind": "Deployment", "namespace": "shop"}, "rule": "pod.crash_loop", "severity": "warning", "count": i + 1, "evidence": map[string]int{"restarts": i + 100}})
				batch := EvidenceBatch{ScopeBinding: "binding", ID: fmt.Sprintf("poll-%d", i), Expected: cp.ID, Generation: cp.Generation, At: at, Epoch: "stable", Principal: "verified", Authority: map[string]EvidenceAuthority{"Pod": {State: "allowed", ProvedAt: at}}, Issues: []EvidenceIssue{{Key: "root-rule", Kind: "Deployment", AuthorizationKind: "Pod", Namespace: "shop", UID: "root", Rule: "pod.crash_loop", Broken: true, Payload: payload}}}
				if err = store.CommitEvidence(t.Context(), scope, batch); err != nil {
					t.Fatal(err)
				}
			}
			query := EvidenceQuery{Since: origin.Add(-time.Minute), Until: origin.Add(time.Hour), Type: "occurrence", EnforceAuthority: true, AllowedKinds: []string{"Pod"}}
			if lookup, ok := provider.(EvidenceScopeLookup); ok {
				resolved, err := lookup.FindEvidenceScope(t.Context(), scope.OrgID, scope.ClusterID, "binding")
				if err != nil || resolved.ClusterUID != scope.ClusterUID {
					t.Fatalf("persisted physical identity lookup=%+v %v", resolved, err)
				}
			}
			current, err := store.QueryEvidence(t.Context(), scope, query)
			if err != nil || len(current.Items) != 1 {
				t.Fatalf("current=%+v err=%v", current, err)
			}
			var issue EvidenceIssue
			if err = json.Unmarshal(current.Items[0].Payload, &issue); err != nil {
				t.Fatal(err)
			}
			if !issue.LastSeen.Equal(origin.Add(15*time.Minute)) || !strings.Contains(string(issue.Payload), `"count":16`) {
				t.Fatalf("mutable state lost: %+v", issue)
			}
			journal, err := store.QueryEvidence(t.Context(), scope, EvidenceQuery{Since: query.Since, Until: query.Until, Type: "issue"})
			if err != nil || journal.Total != 1 {
				t.Fatalf("volatile measurements appended journal: %+v %v", journal, err)
			}
			if backend != "postgres" {
				state, err := blobEvidenceRead(t.Context(), provider, scope)
				if err != nil {
					t.Fatal(err)
				}
				if len(state.Observations) != 4 || len(state.OccurrenceVersions) != 1 {
					t.Fatalf("observations=%d versions=%d", len(state.Observations), len(state.OccurrenceVersions))
				}
			} else {
				var observations, versions int
				key, _ := evidenceScopeKey(scope)
				db := provider.(SQLAccessor).DB()
				if err = db.QueryRow(`SELECT (SELECT COUNT(*) FROM kubernetes_evidence_occurrence_observations WHERE scope_key=$1),(SELECT COUNT(*) FROM kubernetes_evidence_occurrence_versions WHERE scope_key=$1)`, key).Scan(&observations, &versions); err != nil {
					t.Fatal(err)
				}
				if observations != 4 || versions != 1 {
					t.Fatalf("observations=%d versions=%d", observations, versions)
				}
			}
			query.PointInTime, query.OccurrenceID, query.Until = true, issue.OccurrenceID, origin.Add(2*time.Minute)
			pinned, err := store.QueryEvidence(t.Context(), scope, query)
			if err != nil || len(pinned.Items) != 1 {
				t.Fatalf("snapshot: %+v %v", pinned, err)
			}
			var historical EvidenceIssue
			if err = json.Unmarshal(pinned.Items[0].Payload, &historical); err != nil {
				t.Fatal(err)
			}
			if !historical.LastSeen.Equal(origin) {
				t.Fatalf("fabricated historic freshness: %s", historical.LastSeen)
			}
			query.PointInTime, query.Until, query.AllowedKinds = false, origin.Add(time.Hour), nil
			denied, err := store.QueryEvidence(t.Context(), scope, query)
			if err != nil || len(denied.Items) != 1 || denied.Items[0].Type != "coverage_gap" {
				t.Fatalf("denied slot: %+v %v", denied, err)
			}
			if denied.Items[0].RootUID != "" || denied.Items[0].Namespace != "" || strings.Contains(string(denied.Items[0].Payload), "root") {
				t.Fatal("denied slot leaked protected identity")
			}
			authority := provider.(EvidenceAuthorityStore)
			revoke := map[string]EvidenceAuthority{"Pod": {State: "denied", ProvedAt: origin.Add(20 * time.Minute)}}
			if err = authority.SetEvidenceAuthority(t.Context(), scope, "verified", revoke); err != nil {
				t.Fatal(err)
			}
			if err = authority.SetEvidenceAuthority(t.Context(), scope, "other-principal", map[string]EvidenceAuthority{"Pod": {State: "allowed", ProvedAt: origin.Add(30 * time.Minute)}}); !errors.Is(err, ErrEvidenceConflict) {
				t.Fatalf("foreign principal authority write: %v", err)
			}
			cp, err := store.EvidenceCheckpoint(t.Context(), scope)
			if err != nil || cp.Authority["Pod"].State != "denied" {
				t.Fatalf("denial not durable: %+v %v", cp, err)
			}
		})
	}
}

func TestEvidencePagesBoundProjectionAndCounts(t *testing.T) {
	provider := NewMemory()
	store := provider.(EvidenceStore)
	scope := EvidenceScope{OrgID: "default", ClusterID: "bounded", ClusterUID: "physical"}
	at := time.Now().UTC()
	batch := EvidenceBatch{ID: "many", At: at, Epoch: "stable"}
	for i := 0; i < 1100; i++ {
		batch.Records = append(batch.Records, EvidenceRecord{ID: fmt.Sprintf("row-%04d", i), Type: "change", At: at, ObservedAt: at, Epoch: "stable", AuthorizationKind: "Pod", Payload: json.RawMessage(`{}`)})
	}
	if err := store.CommitEvidence(t.Context(), scope, batch); err != nil {
		t.Fatal(err)
	}
	query := EvidenceQuery{Since: at.Add(-time.Minute), Until: at.Add(time.Minute), Type: "change", Limit: 10}
	page, err := store.QueryEvidence(t.Context(), scope, query)
	if err != nil || len(page.Items) != 10 || !page.CountCapped || page.Total != 1001 || page.Next == "" {
		t.Fatalf("bounded counts=%+v err=%v", page, err)
	}
	query.Cursor = page.Next
	next, err := store.QueryEvidence(t.Context(), scope, query)
	if err != nil || next.Total != 0 || len(next.Items) != 10 {
		t.Fatalf("continuation counts=%+v %v", next, err)
	}
}
func TestEvidenceLifecycleIsAtomicIdempotentAndCoverageSafe(t *testing.T) {
	for _, backend := range []string{"memory", "file"} {
		t.Run(backend, func(t *testing.T) {
			var provider Provider = NewMemory()
			var err error
			dir := t.TempDir()
			if backend == "file" {
				provider, err = NewFile(FileOptions{DataDir: dir})
				if err != nil {
					t.Fatal(err)
				}
			}
			store := provider.(EvidenceStore)
			scope := EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical-a"}
			origin := time.Now().UTC().Add(-time.Hour)
			batches := []EvidenceBatch{evidenceTestBatch("open", "", origin, true, true), evidenceTestBatch("partial", "open", origin.Add(time.Minute), false, false), evidenceTestBatch("seen", "partial", origin.Add(2*time.Minute), true, true), evidenceTestBatch("resolve", "seen", origin.Add(3*time.Minute), false, true), evidenceTestBatch("recur", "resolve", origin.Add(4*time.Minute), true, true)}
			for _, batch := range batches {
				if err = store.CommitEvidence(t.Context(), scope, batch); err != nil {
					t.Fatal(err)
				}
				if err = store.CommitEvidence(t.Context(), scope, batch); err != nil {
					t.Fatalf("duplicate failed: %v", err)
				}
			}
			if backend == "file" {
				provider, err = NewFile(FileOptions{DataDir: dir})
				if err != nil {
					t.Fatal(err)
				}
				store = provider.(EvidenceStore)
			}
			if err = store.CommitEvidence(t.Context(), scope, batches[0]); err != nil {
				t.Fatalf("retained duplicate delivery failed: %v", err)
			}
			page, err := store.QueryEvidence(t.Context(), scope, EvidenceQuery{Since: origin.Add(-time.Minute), Until: origin.Add(time.Hour), Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			counts := map[string]int{}
			occurrences := map[string]bool{}
			for _, record := range page.Items {
				counts[record.Type]++
				var issue EvidenceIssue
				if err = json.Unmarshal(record.Payload, &issue); err != nil {
					t.Fatal(err)
				}
				occurrences[issue.OccurrenceID] = true
			}
			if counts["issue_opened"] != 2 || counts["issue_resolved"] != 1 || counts["issue_observed"] != 0 || len(occurrences) != 2 {
				t.Fatalf("lifecycle=%v occurrences=%d", counts, len(occurrences))
			}
			for _, foreign := range []EvidenceScope{{OrgID: "other", ClusterID: "cluster", ClusterUID: "physical-a"}, {OrgID: "org", ClusterID: "other", ClusterUID: "physical-a"}, {OrgID: "org", ClusterID: "cluster", ClusterUID: "physical-b"}} {
				foreignPage, err := store.QueryEvidence(t.Context(), foreign, EvidenceQuery{Since: origin, Until: origin.Add(time.Hour)})
				if err != nil || len(foreignPage.Items) != 0 {
					t.Fatal("scope leaked evidence")
				}
			}
			checkpoint, err := store.EvidenceCheckpoint(t.Context(), scope)
			if err != nil {
				t.Fatal(err)
			}
			bad := evidenceTestBatch("bad", checkpoint.ID, origin.Add(5*time.Minute), false, true)
			bad.Records = []EvidenceRecord{{ID: "invalid"}}
			if err = store.CommitEvidence(t.Context(), scope, bad); err == nil {
				t.Fatal("invalid event committed")
			}
			after, _ := store.EvidenceCheckpoint(t.Context(), scope)
			if after.ID != checkpoint.ID {
				t.Fatal("failed write advanced checkpoint")
			}
		})
	}
}

func TestEvidencePaginationWatermarkExcludesBackdatedConcurrentInserts(t *testing.T) {
	store := NewMemory().(EvidenceStore)
	scope := EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}
	origin := time.Now().UTC().Add(-time.Hour)
	batch := evidenceTestBatch("first", "", origin.Add(time.Minute), false, false)
	for i := 0; i < 12; i++ {
		batch.Records = append(batch.Records, EvidenceRecord{ID: fmt.Sprintf("event-%02d", i), Type: "change", At: origin.Add(time.Duration(i) * time.Second), ObservedAt: batch.At, Epoch: batch.Epoch, Payload: json.RawMessage(`{}`)})
	}
	if err := store.CommitEvidence(t.Context(), scope, batch); err != nil {
		t.Fatal(err)
	}
	query := EvidenceQuery{Since: origin, Until: origin.Add(time.Hour), Limit: 3}
	first, err := store.QueryEvidence(t.Context(), scope, query)
	if err != nil || first.Next == "" {
		t.Fatal("first page missing")
	}
	concurrent := evidenceTestBatch("concurrent", "first", origin.Add(2*time.Minute), false, false)
	concurrent.Records = []EvidenceRecord{{ID: "backdated", Type: "change", At: origin.Add(5 * time.Second), ObservedAt: concurrent.At, Epoch: concurrent.Epoch, Payload: json.RawMessage(`{}`)}}
	if err := store.CommitEvidence(t.Context(), scope, concurrent); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	page := first
	for {
		for _, record := range page.Items {
			if record.ID == "backdated" || seen[record.ID] {
				t.Fatal("paging included insert or duplicate")
			}
			seen[record.ID] = true
		}
		if page.Next == "" {
			break
		}
		query.Cursor = page.Next
		page, err = store.QueryEvidence(t.Context(), scope, query)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 12 {
		t.Fatalf("lost history: %d", len(seen))
	}
	query.Namespace = "other"
	if _, err = store.QueryEvidence(t.Context(), scope, query); err == nil {
		t.Fatal("cursor crossed authorized namespace")
	}
}

func TestEvidenceConcurrentWritersDoNotAdvanceOneCheckpointTwice(t *testing.T) {
	store := NewMemory().(EvidenceStore)
	scope := EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}
	origin := time.Now().UTC()
	var wait sync.WaitGroup
	results := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			results <- store.CommitEvidence(t.Context(), scope, evidenceTestBatch(fmt.Sprint(i), "", origin, true, true))
		}(i)
	}
	wait.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrEvidenceConflict) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatalf("checkpoint winners=%d", success)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := store.CommitEvidence(ctx, scope, evidenceTestBatch("cancelled", "", origin, false, false)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
}

func TestFileEvidencePruningIsExplicitAndPreservesActiveOccurrence(t *testing.T) {
	provider, err := NewFile(FileOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	store := provider.(EvidenceStore)
	scope := EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}
	origin := time.Now().UTC().Add(-48 * time.Hour)
	if err = store.CommitEvidence(t.Context(), scope, evidenceTestBatch("first", "", origin, true, true)); err != nil {
		t.Fatal(err)
	}
	batch := evidenceTestBatch("later", "first", origin.Add(30*time.Hour), true, false)
	if err = store.CommitEvidence(t.Context(), scope, batch); err != nil {
		t.Fatal(err)
	}
	page, err := store.QueryEvidence(t.Context(), scope, EvidenceQuery{Since: origin.Add(-time.Minute), Until: origin.Add(48 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if page.Policy.Mode != "bounded_file" || page.Checkpoint.PrunedThrough.IsZero() || len(page.Items) != 0 {
		t.Fatalf("file policy=%+v checkpoint=%+v items=%d", page.Policy, page.Checkpoint, len(page.Items))
	}
	state, err := blobEvidenceRead(t.Context(), provider, scope)
	if err != nil {
		t.Fatal(err)
	}
	issue, exists := state.Active["resource-rule"]
	if !exists || !issue.FirstSeen.Equal(origin) || !issue.LastSeen.Equal(batch.At) || issue.ClosedAt != nil {
		t.Fatal("pruning lost active lifecycle")
	}
	if p := (&postgresProvider{}).EvidencePolicy(); p.Mode != "unbounded_database" || p.Retention != 0 || p.MaxRecords != 0 || p.MaxBytes != 0 {
		t.Fatal("database policy imposes retention")
	}
}

func TestPostgresEvidenceRetainsLargeHistoryWithoutApplicationPruning(t *testing.T) {
	dsn := os.Getenv("KUBERNETES_EVIDENCE_TEST_POSTGRES_DSN")
	if dsn == "" {
		dsn = os.Getenv("TEST_POSTGRES_DSN")
	}
	if dsn == "" {
		t.Skip("isolated synthetic Postgres DSN not supplied")
	}
	provider, err := NewPostgres(PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatal("isolated Postgres setup failed")
	}
	defer provider.Close()
	store := provider.(EvidenceStore)
	scope := EvidenceScope{OrgID: "synthetic-evidence", ClusterID: fmt.Sprintf("capture-%d", time.Now().UnixNano()), ClusterUID: "synthetic-physical"}
	origin := time.Now().UTC().Add(-60 * 24 * time.Hour)
	expected := ""
	var first EvidenceBatch
	const total = 100500
	for offset := 0; offset < total; offset += 1000 {
		batch := EvidenceBatch{ID: fmt.Sprintf("batch-%d", offset), Expected: expected, At: time.Now().UTC(), Epoch: "synthetic-epoch"}
		for i := offset; i < min(offset+1000, total); i++ {
			payload, _ := json.Marshal(map[string]string{"summary": fmt.Sprintf("safe event %d %s", i, strings.Repeat("x", 1024))})
			batch.Records = append(batch.Records, EvidenceRecord{ID: fmt.Sprintf("event-%06d", i), Type: "change", At: origin.Add(time.Duration(i) * 40 * time.Second), ObservedAt: batch.At, Namespace: "shop", Kind: "Pod", UID: "synthetic-pod", Epoch: batch.Epoch, Payload: payload})
		}
		if err = store.CommitEvidence(t.Context(), scope, batch); err != nil {
			t.Fatal(err)
		}
		if offset == 0 {
			first = batch
		}
		expected = batch.ID
	}
	if err = store.CommitEvidence(t.Context(), scope, first); err != nil {
		t.Fatal("old capture replay was not idempotent")
	}
	// Exercise bounded filtered counts rather than the unfiltered checkpoint
	// summary. A lower-bound count must never be confused with retained size.
	query := EvidenceQuery{Since: origin.Add(-time.Hour), Until: time.Now().UTC(), Namespace: "shop", Type: "change", Limit: 500}
	count := 0
	seen := map[string]bool{}
	for {
		page, err := store.QueryEvidence(t.Context(), scope, query)
		if err != nil {
			t.Fatal(err)
		}
		if page.Policy.Mode != "unbounded_database" || page.Policy.Retention != 0 || page.Policy.MaxRecords != 0 || page.Policy.MaxBytes != 0 || !page.Checkpoint.PrunedThrough.IsZero() || page.Checkpoint.PruneRevision != 0 {
			t.Fatal("database history acquired a retention limit or pruning marker")
		}
		if query.Cursor == "" {
			if !page.CountCapped || page.Total != 1001 || page.Counts["change"] != 1001 {
				t.Fatalf("first-page filtered count is not an explicit lower bound: total=%d counts=%v capped=%v", page.Total, page.Counts, page.CountCapped)
			}
		} else if page.Total != 0 || len(page.Counts) != 0 || page.CountCapped {
			t.Fatal("continuation page repeated first-page counts")
		}
		for _, record := range page.Items {
			if seen[record.ID] {
				t.Fatal("duplicate evidence while paging")
			}
			if count >= total || record.ID != fmt.Sprintf("event-%06d", total-1-count) || !record.At.Equal(origin.Add(time.Duration(total-1-count)*40*time.Second)) {
				t.Fatalf("retained keyset order or membership changed at position %d: id=%s", count, record.ID)
			}
			seen[record.ID] = true
			count++
		}
		if page.Next == "" {
			break
		}
		if len(page.Items) == 0 || page.Next == query.Cursor {
			t.Fatal("keyset continuation made no progress")
		}
		query.Cursor = page.Next
	}
	if count != total {
		t.Fatalf("retained rows=%d want=%d", count, total)
	}
}

func TestEvidenceGapOverlapsWindowAndTrustedArchivesPageWithoutDuplicates(t *testing.T) {
	provider := NewMemory()
	store := provider.(EvidenceStore)
	now := time.Now().UTC()
	scope := EvidenceScope{OrgID: "default", ClusterID: "cluster", ClusterUID: "physical"}
	batch := EvidenceBatch{ID: "archive", At: now, Epoch: "synthetic"}
	for i := 0; i < 8; i++ {
		batch.Records = append(batch.Records, EvidenceRecord{ID: fmt.Sprintf("event-%d", i), Type: "change", At: now.Add(time.Duration(i-10) * time.Hour), ObservedAt: now, Namespace: "shop", Kind: "Pod", UID: "pod", Epoch: "synthetic", Payload: json.RawMessage(`{}`)})
	}
	if err := store.CommitEvidence(t.Context(), scope, batch); err != nil {
		t.Fatal(err)
	}
	scope.OrgID = "licensed"
	gapPayload, _ := json.Marshal(map[string]time.Time{"from": now.Add(-20 * time.Hour), "to": now.Add(-time.Hour)})
	current := EvidenceBatch{ID: "current", At: now, Epoch: "synthetic", Records: []EvidenceRecord{batch.Records[0], {ID: "gap", Type: "coverage_gap", GapFrom: now.Add(-20 * time.Hour), GapTo: now.Add(-time.Hour), At: now.Add(-time.Hour), ObservedAt: now, Epoch: "synthetic", Payload: gapPayload}}}
	if err := store.CommitEvidence(t.Context(), scope, current); err != nil {
		t.Fatal(err)
	}
	scope.ReadOrgs = []string{"licensed", "default"}
	query := EvidenceQuery{Since: now.Add(-12 * time.Hour), Until: now.Add(-2 * time.Hour), Namespace: "shop", Type: "change", WithGaps: true, Limit: 2}
	count := 0
	seen := map[string]bool{}
	gaps := 0
	for {
		page, err := store.QueryEvidence(t.Context(), scope, query)
		if err != nil {
			t.Fatal(err)
		}
		expectedTotal := 9
		if query.Cursor != "" {
			expectedTotal = 0
		}
		if page.Total != expectedTotal {
			t.Fatalf("trusted merge total=%d", page.Total)
		}
		for _, record := range page.Items {
			if seen[record.ID] {
				t.Fatal("archive duplicate")
			}
			seen[record.ID] = true
			if record.Type == "coverage_gap" {
				gaps++
			}
			count++
		}
		if page.Next == "" {
			break
		}
		query.Cursor = page.Next
	}
	if count != 9 || gaps != 1 {
		t.Fatalf("merged rows=%d gaps=%d", count, gaps)
	}
}

func TestFileEvidenceCountAndBytePruningInvalidatesPagingWithoutClosingIssues(t *testing.T) {
	provider, err := NewFile(FileOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	store := provider.(EvidenceStore)
	scope := EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}
	now := time.Now().UTC()
	first := evidenceTestBatch("first", "", now.Add(-4*time.Hour), true, true)
	for i := 0; i < 12; i++ {
		first.Records = append(first.Records, EvidenceRecord{ID: fmt.Sprintf("initial-%d", i), Type: "change", At: first.At, ObservedAt: first.At, Epoch: first.Epoch, Payload: json.RawMessage(`{}`)})
	}
	if err = store.CommitEvidence(t.Context(), scope, first); err != nil {
		t.Fatal(err)
	}
	query := EvidenceQuery{Since: now.Add(-5 * time.Hour), Until: now, Limit: 2}
	page, err := store.QueryEvidence(t.Context(), scope, query)
	if err != nil || page.Next == "" {
		t.Fatal("missing initial cursor")
	}
	cursor := page.Next
	later := evidenceTestBatch("later", "first", now, true, false)
	payload, _ := json.Marshal(map[string]string{"summary": strings.Repeat("x", 1000)})
	for i := 0; i < 6000; i++ {
		later.Records = append(later.Records, EvidenceRecord{ID: fmt.Sprintf("large-%d", i), Type: "change", At: now.Add(-2 * time.Hour).Add(time.Duration(i) * time.Second), ObservedAt: now, Epoch: later.Epoch, Payload: payload})
	}
	if err = store.CommitEvidence(t.Context(), scope, later); err != nil {
		t.Fatal(err)
	}
	state, err := blobEvidenceRead(t.Context(), provider, scope)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(state)
	if len(raw) > 4<<20 || len(state.Records) > 5000 || state.Checkpoint.PruneRevision == 0 || len(state.Active) != 1 {
		t.Fatalf("unsafe file budget: bytes=%d rows=%d active=%d", len(raw), len(state.Records), len(state.Active))
	}
	query.Cursor = cursor
	if _, err = store.QueryEvidence(t.Context(), scope, query); !errors.Is(err, ErrEvidenceConflict) {
		t.Fatal("pruned cursor silently skipped evidence")
	}
}

func TestEvidenceRefinementsPreservePinnedPagesAndPreferObservedFacts(t *testing.T) {
	store := NewMemory().(EvidenceStore)
	scope := EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}
	now := time.Now().UTC().Add(-time.Minute)
	initial := EvidenceBatch{ID: "initial", At: now, Epoch: "synthetic", Records: []EvidenceRecord{{ID: "target", Type: "change", At: now.Add(-time.Second), ObservedAt: now, Epoch: "synthetic", Priority: 1, Payload: json.RawMessage(`{"count":1}`)}, {ID: "other", Type: "change", At: now, ObservedAt: now, Epoch: "synthetic", Payload: json.RawMessage(`{}`)}}}
	if err := store.CommitEvidence(t.Context(), scope, initial); err != nil {
		t.Fatal(err)
	}
	query := EvidenceQuery{Since: now.Add(-time.Hour), Until: now.Add(time.Hour), Limit: 1}
	first, err := store.QueryEvidence(t.Context(), scope, query)
	if err != nil || first.Next == "" {
		t.Fatal("initial paging unavailable")
	}
	refined := initial.Records[0]
	refined.At = now.Add(time.Second)
	refined.ObservedAt = now.Add(time.Second)
	refined.Priority = 2
	refined.Payload = json.RawMessage(`{"count":3}`)
	if err = store.CommitEvidence(t.Context(), scope, EvidenceBatch{ID: "refined", Expected: "initial", At: now.Add(time.Second), Epoch: "synthetic", Records: []EvidenceRecord{refined}}); err != nil {
		t.Fatal(err)
	}
	query.Cursor = first.Next
	old, err := store.QueryEvidence(t.Context(), scope, query)
	if err != nil || len(old.Items) != 1 || string(old.Items[0].Payload) != `{"count":1}` {
		t.Fatal("refinement moved or erased an already pinned row")
	}
	query.Cursor = ""
	latest, err := store.QueryEvidence(t.Context(), scope, query)
	if err != nil || latest.Total != 2 || latest.Items[0].ID != "target" || string(latest.Items[0].Payload) != `{"count":3}` {
		t.Fatal("new query did not see strongest facts")
	}
	weak := refined
	weak.Priority = 1
	weak.Payload = json.RawMessage(`{"count":2}`)
	if err = store.CommitEvidence(t.Context(), scope, EvidenceBatch{ID: "weak", Expected: "refined", At: now.Add(2 * time.Second), Epoch: "synthetic", Records: []EvidenceRecord{weak}}); err != nil {
		t.Fatal(err)
	}
	latest, err = store.QueryEvidence(t.Context(), scope, query)
	if err != nil || string(latest.Items[0].Payload) != `{"count":3}` {
		t.Fatal("reconstruction replaced stronger observed evidence")
	}
}

func TestHistoricalObservationCannotReopenAResolvedIssue(t *testing.T) {
	provider := NewMemory()
	store := provider.(EvidenceStore)
	scope := EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}
	now := time.Now().UTC()
	if err := store.CommitEvidence(t.Context(), scope, evidenceTestBatch("open", "", now, true, true)); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitEvidence(t.Context(), scope, evidenceTestBatch("resolved", "open", now.Add(time.Minute), false, true)); err != nil {
		t.Fatal(err)
	}
	late := evidenceTestBatch("late", "resolved", now.Add(time.Minute), true, false)
	late.HistoricalOnly = true
	late.Issues[0].LastSeen = now
	if err := store.CommitEvidence(t.Context(), scope, late); err != nil {
		t.Fatal(err)
	}
	state, err := blobEvidenceRead(t.Context(), provider, scope)
	if err != nil || len(state.Active) != 0 {
		t.Fatal("older observation reopened a resolved occurrence")
	}
	if !state.Checkpoint.CoveredThrough.Equal(now.Add(time.Minute)) {
		t.Fatal("late observation changed committed coverage")
	}
	found := false
	for _, record := range state.Records {
		if record.Type != "issue_observed" {
			continue
		}
		var issue EvidenceIssue
		if err = json.Unmarshal(record.Payload, &issue); err != nil {
			t.Fatal(err)
		}
		found = found || issue.ObservationOnly
	}
	if !found {
		t.Fatal("older safe evidence was silently dropped")
	}
}

func TestEvidenceCountCacheBoundsQueryWorkWithoutRetainingCallerMaps(t *testing.T) {
	provider := &postgresProvider{}
	original := map[string]int{"change": 7}
	provider.rememberEvidenceCounts("query", 7, original)
	original["change"] = 0
	total, counts, ok := provider.evidenceCounts("query")
	if !ok || total != 7 || counts["change"] != 7 {
		t.Fatal("query count cache retained mutable caller data")
	}
	counts["change"] = 100
	_, again, _ := provider.evidenceCounts("query")
	if again["change"] != 7 {
		t.Fatal("query response mutated cached counts")
	}
	for i := 0; i < 200; i++ {
		provider.rememberEvidenceCounts(fmt.Sprint(i), i, map[string]int{"change": i})
	}
	if len(provider.evidenceQueries) > 128 {
		t.Fatal("query metadata cache grew without bound")
	}
	provider.evidenceQueries["expired"] = evidenceCountSnapshot{Expires: time.Now().Add(-time.Second)}
	if _, _, ok = provider.evidenceCounts("expired"); ok {
		t.Fatal("expired query metadata was reused")
	}
	generation := provider.evidenceCountGeneration()
	finish := provider.beginEvidenceDeletion()
	provider.rememberEvidenceCounts("stale", 9, map[string]int{"change": 9}, generation)
	if _, _, ok := provider.evidenceCounts("stale"); ok {
		t.Fatal("delete allowed an in-flight old count publication")
	}
	finish()
	provider.rememberEvidenceCounts("stale", 9, map[string]int{"change": 9}, generation)
	if _, _, ok := provider.evidenceCounts("stale"); ok {
		t.Fatal("old query repopulated count metadata after deletion")
	}
	if policy := provider.EvidencePolicy(); policy.MaxRecords != 0 || policy.MaxBytes != 0 || policy.Retention != 0 {
		t.Fatal("query cache bounds became history retention limits")
	}
}
