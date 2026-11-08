package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// Every event type the journal accepts. A record re-observed with only new
// times must keep one stored version whatever its type.
var timeOnlyEventTypes = []string{"change", "crash", "issue_opened", "issue_observed", "issue_resolved", "coverage_gap"}

// partitionedEvidenceDatabase serializes the tests that convert and use the one
// isolated Postgres database: they run in parallel with each other, and the
// conversion, maintenance and physical row checks assume sole use of it.
var partitionedEvidenceDatabase sync.Mutex

// partitionedEvidenceProvider opens the isolated Postgres database with
// maintenance stopped and the events table converted to the partitioned
// layout that long-running deployments use, whose unique key includes the
// event time.
func partitionedEvidenceProvider(t *testing.T, heartbeat string) *postgresProvider {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("isolated Postgres required")
	}
	partitionedEvidenceDatabase.Lock()
	t.Cleanup(partitionedEvidenceDatabase.Unlock)
	provider, err := NewPostgres(PostgresOptions{DSN: dsn, ObservationHeartbeat: heartbeat})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	p := provider.(*postgresProvider)
	p.maintenanceCancel()
	<-p.maintenanceDone
	for i := 0; i < 4096; i++ {
		more, err := p.backfillEvidenceBatch(t.Context(), evidenceBackgroundBackfill)
		if err != nil {
			t.Fatal(err)
		}
		copyMore, err := p.partitionEvidenceBatch(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !more && !copyMore {
			break
		}
	}
	var kind string
	if err := p.db.QueryRow(`SELECT relkind::text FROM pg_class WHERE oid='kubernetes_evidence_events'::regclass`).Scan(&kind); err != nil || kind != "p" {
		t.Fatalf("events table not partitioned: %q %v", kind, err)
	}
	return p
}

func timeOnlyProvider(t *testing.T, backend, heartbeat string) Provider {
	t.Helper()
	if backend == "postgres" {
		return partitionedEvidenceProvider(t, heartbeat)
	}
	provider, _ := localEvidenceTestProvider(t, backend)
	return provider
}

// timeOnlyRecord builds one record of the given type whose content never
// changes; only its detection and observation times vary.
func timeOnlyRecord(eventType string, at, observed time.Time, content string) EvidenceRecord {
	record := EvidenceRecord{ID: eventType + ":stable", Type: eventType, At: at, ObservedAt: observed, Namespace: "shop", Name: "api", Kind: "Deployment", AuthorizationKind: "Deployment", UID: "root-uid", RootUID: "root-uid", Epoch: "stable"}
	fields := map[string]any{"type": eventType, "value": content, "at": at, "observed_at": observed}
	switch eventType {
	case "change":
		record.Priority, record.Rule = 1, "created"
	case "crash":
		record.AuthorizationKind, record.UID, record.PodUID = "Pod", "pod-uid", "pod-uid"
		fields["waiting_reason"], fields["restart_count"] = "CrashLoopBackOff", 4
	case "issue_opened", "issue_observed", "issue_resolved":
		record.Rule = "pod.crashloop"
	case "coverage_gap":
		// The gap bounds are content: the same gap re-reported keeps them.
		record.GapFrom = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
		record.GapTo = record.GapFrom.Add(time.Hour)
		record.Kind, record.Name, record.Namespace, record.UID, record.RootUID = "Pod", "", "", "", ""
	}
	record.Payload, _ = json.Marshal(fields)
	return record
}

// storedRowsByID counts journal rows per event ID, and for Postgres also the
// distinct stored versions and heads.
func storedRowsByID(t *testing.T, provider Provider, scope EvidenceScope) (rows, versions, heads map[string]int) {
	t.Helper()
	rows, versions, heads = map[string]int{}, map[string]int{}, map[string]int{}
	if p, ok := provider.(*postgresProvider); ok {
		key, _ := evidenceScopeKey(scope)
		result, err := p.db.Query(`SELECT e.event_id,COUNT(*),COUNT(DISTINCT e.event_version),
 (SELECT COUNT(*) FROM kubernetes_evidence_event_heads h WHERE h.scope_key=$1 AND h.event_id=e.event_id)
 FROM kubernetes_evidence_events e WHERE e.scope_key=$1 GROUP BY e.event_id`, key)
		if err != nil {
			t.Fatal(err)
		}
		defer result.Close()
		for result.Next() {
			var id string
			var count, distinct, head int
			if err := result.Scan(&id, &count, &distinct, &head); err != nil {
				t.Fatal(err)
			}
			rows[id], versions[id], heads[id] = count, distinct, head
		}
		if err := result.Err(); err != nil {
			t.Fatal(err)
		}
		return rows, versions, heads
	}
	state, err := blobEvidenceRead(t.Context(), provider, scope)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]map[string]bool{}
	for _, record := range state.Records {
		rows[record.ID]++
		if seen[record.ID] == nil {
			seen[record.ID] = map[string]bool{}
		}
		seen[record.ID][evidenceVersion(record)] = true
		versions[record.ID] = len(seen[record.ID])
	}
	return rows, versions, nil
}

func commitTimeOnlyCycle(t *testing.T, store EvidenceStore, scope EvidenceScope, id string, at time.Time, records []EvidenceRecord) EvidenceCheckpoint {
	t.Helper()
	cp, err := store.EvidenceCheckpoint(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	batch := EvidenceBatch{ID: id, Expected: cp.ID, Generation: cp.Generation, At: at, Epoch: "stable", Records: records}
	if err = store.CommitEvidence(t.Context(), scope, batch); err != nil {
		t.Fatalf("commit %s: %v", id, err)
	}
	cp, err = store.EvidenceCheckpoint(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	return cp
}

// TestEvidenceKeepsOneVersionPerTypeAcrossAThousandObservationTimes feeds the
// same record of every journal event type with 1,000 different detection and
// observation times. Detection times jump backwards and forwards, as a
// re-detection after a restart or credential renewal can report an earlier or
// later time than the stored one. Every backend must keep exactly one stored
// version per event, and still append a real content change.
func TestEvidenceKeepsOneVersionPerTypeAcrossAThousandObservationTimes(t *testing.T) {
	t.Parallel()
	const cycles = 1000
	for _, backend := range []string{"memory", "file", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()
			provider := timeOnlyProvider(t, backend, "5m")
			store := provider.(EvidenceStore)
			scope := EvidenceScope{OrgID: "default", ClusterID: fmt.Sprintf("time-only-%s-%d", backend, time.Now().UnixNano()), ClusterUID: "physical"}
			origin := time.Now().UTC().Add(-10 * time.Hour).Truncate(time.Microsecond)
			var first EvidenceCheckpoint
			for cycle := 0; cycle < cycles; cycle++ {
				observed := origin.Add(time.Duration(cycle) * 30 * time.Second)
				records := make([]EvidenceRecord, 0, len(timeOnlyEventTypes))
				for position, eventType := range timeOnlyEventTypes {
					// A different, non-monotonic detection time per cycle and type.
					jitter := time.Duration((cycle*7919+position*104729)%3600) * time.Second
					records = append(records, timeOnlyRecord(eventType, observed.Add(-jitter), observed, "1"))
				}
				cp := commitTimeOnlyCycle(t, store, scope, fmt.Sprintf("cycle-%d", cycle), observed, records)
				if cycle == 0 {
					first = cp
					if cp.HighSequence == 0 {
						t.Fatal("the first observation stored nothing")
					}
				} else if cp.HighSequence != first.HighSequence {
					t.Fatalf("cycle %d appended a time-only version: high %d -> %d", cycle, first.HighSequence, cp.HighSequence)
				}
			}
			rows, versions, heads := storedRowsByID(t, provider, scope)
			for _, eventType := range timeOnlyEventTypes {
				id := eventType + ":stable"
				if rows[id] != 1 || versions[id] != 1 {
					t.Errorf("%s: %d timestamps stored rows=%d versions=%d, want 1", eventType, cycles, rows[id], versions[id])
				}
				if heads != nil && heads[id] != 1 {
					t.Errorf("%s: heads=%d, want 1", eventType, heads[id])
				}
			}
			if len(rows) != len(timeOnlyEventTypes) {
				t.Fatalf("stored event ids = %v, want one per type", rows)
			}
			// The guard folds time only: a real content change still appends once.
			changed := origin.Add(cycles * 30 * time.Second)
			cp := commitTimeOnlyCycle(t, store, scope, "content-change", changed, []EvidenceRecord{timeOnlyRecord("change", changed, changed, "2")})
			if cp.HighSequence != first.HighSequence+1 {
				t.Fatalf("a real content change was not appended exactly once: high %d -> %d", first.HighSequence, cp.HighSequence)
			}
		})
	}
}

// crashLoopStatus is a CrashLoopBackOff status record with the identity a
// capture gives it: the restart count is part of the event, the time is not.
func crashLoopStatus(restarts int, at time.Time) EvidenceRecord {
	payload, _ := json.Marshal(map[string]any{"cluster": "crashloop", "namespace": "shop", "pod": "api", "container": "app", "type": "status", "waiting_reason": "CrashLoopBackOff", "restart_count": restarts, "at": at, "observed_at": at, "time_source": "observed_at"})
	return EvidenceRecord{ID: fmt.Sprintf("crash:status-restarts-%d", restarts), Type: "crash", At: at, ObservedAt: at, Namespace: "shop", Name: "api", Kind: "Deployment", AuthorizationKind: "Pod", UID: "pod-uid", RootUID: "root-uid", PodUID: "pod-uid", Epoch: "stable", Payload: payload}
}

// TestCrashLoopFreshnessStaysOneRowPerStateOverAThousandCycles replays 1,000
// sync cycles of a Pod stuck in CrashLoopBackOff, with a restart every 250
// cycles. Each state is one journal row, each restart adds exactly one row,
// and on Postgres "still crashing" is refreshed in place at most once per
// heartbeat while staying no older than one heartbeat.
func TestCrashLoopFreshnessStaysOneRowPerStateOverAThousandCycles(t *testing.T) {
	t.Parallel()
	const (
		cycles       = 1000
		interval     = 30 * time.Second
		heartbeat    = 5 * time.Minute
		restartEvery = 250
	)
	for _, backend := range []string{"memory", "file", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			t.Parallel()
			provider := timeOnlyProvider(t, backend, heartbeat.String())
			store := provider.(EvidenceStore)
			scope := EvidenceScope{OrgID: "default", ClusterID: fmt.Sprintf("crashloop-%s-%d", backend, time.Now().UnixNano()), ClusterUID: "physical"}
			p, isPostgres := provider.(*postgresProvider)
			key, _ := evidenceScopeKey(scope)
			type physical struct{ journal, head string }
			version := func(id string) (physical, time.Time, time.Time) {
				t.Helper()
				var v physical
				var observed, at time.Time
				if err := p.db.QueryRow(`SELECT e.xmin::text,COALESCE((SELECT h.xmin::text FROM kubernetes_evidence_event_heads h WHERE h.scope_key=$1 AND h.event_id=$2),''),e.observed_at,e.event_at
 FROM kubernetes_evidence_events e WHERE e.scope_key=$1 AND e.event_id=$2`, key, id).Scan(&v.journal, &v.head, &observed, &at); err != nil {
					t.Fatal(err)
				}
				return v, observed, at
			}
			origin := time.Now().UTC().Add(-10 * time.Hour).Truncate(time.Microsecond)
			restarts, stateStart := 3, origin
			var high uint64
			var last physical
			journalUpdates, headUpdates := 0, 0
			for cycle := 0; cycle < cycles; cycle++ {
				now := origin.Add(time.Duration(cycle) * interval)
				restarted := cycle > 0 && cycle%restartEvery == 0
				if restarted {
					restarts++
					stateStart = now
				}
				cp := commitTimeOnlyCycle(t, store, scope, fmt.Sprintf("cycle-%d", cycle), now, []EvidenceRecord{crashLoopStatus(restarts, now)})
				switch {
				case cycle == 0 && cp.HighSequence == 0:
					t.Fatal("the first crash observation stored nothing")
				case cycle > 0 && restarted && cp.HighSequence != high+1:
					t.Fatalf("cycle %d: a restart appended %d rows, want 1", cycle, cp.HighSequence-high)
				case cycle > 0 && !restarted && cp.HighSequence != high:
					t.Fatalf("cycle %d: an unchanged crash appended %d rows, want 0", cycle, cp.HighSequence-high)
				}
				high = cp.HighSequence
				if !isPostgres {
					continue
				}
				id := fmt.Sprintf("crash:status-restarts-%d", restarts)
				current, observed, at := version(id)
				if !at.Equal(stateStart) {
					t.Fatalf("cycle %d: event time moved from the first observation %s to %s", cycle, stateStart, at)
				}
				if stale := now.Sub(observed); stale >= heartbeat {
					t.Fatalf("cycle %d: still-crashing freshness is %s old, want under the %s heartbeat", cycle, stale, heartbeat)
				}
				if !restarted && cycle > 0 {
					if current.journal != last.journal {
						journalUpdates++
					}
					if current.head != last.head {
						headUpdates++
					}
				}
				last = current
			}
			rows, _, _ := storedRowsByID(t, provider, scope)
			if want := 1 + (cycles-1)/restartEvery; len(rows) != want {
				t.Fatalf("stored crash events = %d, want one per state (%d): %v", len(rows), want, rows)
			}
			for id, count := range rows {
				if count != 1 {
					t.Fatalf("%s stored %d rows, want 1", id, count)
				}
			}
			if !isPostgres {
				return
			}
			bound := int(cycles * interval / heartbeat)
			t.Logf("%d cycles: %d in-place journal updates, %d head updates (bound %d)", cycles, journalUpdates, headUpdates, bound)
			if journalUpdates > bound || headUpdates > bound {
				t.Fatalf("in-place updates journal=%d head=%d exceed cycles x interval / heartbeat = %d", journalUpdates, headUpdates, bound)
			}
			if journalUpdates == 0 {
				t.Fatal("a long-running crash was never refreshed in place")
			}
		})
	}
}
