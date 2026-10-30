package storage

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestOccurrenceQueryPinsFreshnessPayloadAndDeletionGeneration(t *testing.T) {
	for _, backend := range []string{"memory", "file", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var provider Provider = NewMemory()
			fileDir := t.TempDir()
			if backend == "file" {
				var err error
				provider, err = NewFile(FileOptions{DataDir: fileDir})
				if err != nil {
					t.Fatal(err)
				}
			}
			if backend == "postgres" {
				dsn := os.Getenv("TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("explicit isolated harness Postgres required")
				}
				var err error
				provider, err = NewPostgres(PostgresOptions{DSN: dsn})
				if err != nil {
					t.Fatal("isolated database unavailable")
				}
				t.Cleanup(func() { _ = provider.Close() })
			}
			store := provider.(EvidenceStore)
			scope := EvidenceScope{OrgID: "synthetic-occurrences", ClusterID: fmt.Sprintf("cluster-%d", time.Now().UnixNano()), ClusterUID: "physical"}
			now := time.Now().UTC().Truncate(time.Microsecond)
			opened := now.Add(-2 * time.Hour)
			recent := now.Add(-time.Minute)
			capture := func(id string, at time.Time, payload string, present bool) EvidenceBatch {
				cp, err := store.EvidenceCheckpoint(t.Context(), scope)
				if err != nil {
					t.Fatal(err)
				}
				b := EvidenceBatch{ID: id, Expected: cp.ID, Generation: cp.Generation, At: at, Epoch: "synthetic", CoveredThrough: at}
				if present {
					for i := 0; i < 13; i++ {
						uid := fmt.Sprintf("pod-%02d", i)
						b.Issues = append(b.Issues, EvidenceIssue{Key: uid, Kind: "Pod", UID: uid, Namespace: "shop", Rule: "pod.crash_loop", FirstSeen: opened, LastSeen: at, Broken: true, Payload: json.RawMessage(payload)})
					}
				}
				if err = store.CommitEvidence(t.Context(), scope, b); err != nil {
					t.Fatal(err)
				}
				if err = store.CommitEvidence(t.Context(), scope, b); err != nil {
					t.Fatal(err)
				}
				return b
			}
			capture("opened", opened, `{"severity":"warning","root":{"kind":"Pod","namespace":"shop"},"count":1}`, true)
			q := EvidenceQuery{Since: opened.Add(-time.Minute), Until: now, Type: "occurrence", Limit: 10, Namespace: "shop"}
			first, err := store.QueryEvidence(t.Context(), scope, q)
			if err != nil || first.Total != 13 || first.Next == "" {
				t.Fatalf("first snapshot: %+v %v", first, err)
			}
			cursorRaw, decodeErr := base64.RawURLEncoding.DecodeString(first.Next)
			if decodeErr != nil {
				t.Fatal(decodeErr)
			}
			var other evidenceCursor
			if err = json.Unmarshal(cursorRaw, &other); err != nil {
				t.Fatal(err)
			}
			other.CaptureScope = "kubernetes/evidence/" + evidenceDigest(scope.OrgID, scope.ClusterID, scope.ClusterUID)
			otherRaw, _ := json.Marshal(other)
			invalid := q
			invalid.Cursor = base64.RawURLEncoding.EncodeToString(otherRaw)
			if _, err = store.QueryEvidence(t.Context(), scope, invalid); !errors.Is(err, ErrInvalidEvidence) {
				t.Fatalf("cursor reinterpreted another capture namespace: %v", err)
			}
			q.Cursor = first.Next
			capture("heartbeat", recent, `{ "count":1, "root": {"namespace":"shop", "kind":"Pod"}, "severity":"warning" }`, true)
			if backend == "file" {
				reopened, err := NewFile(FileOptions{DataDir: fileDir})
				if err != nil {
					t.Fatal(err)
				}
				provider = reopened
				store = reopened.(EvidenceStore)
			}
			if backend == "postgres" {
				reopened, err := NewPostgres(PostgresOptions{DSN: os.Getenv("TEST_POSTGRES_DSN")})
				if err != nil {
					t.Fatal("isolated database reopen failed")
				}
				t.Cleanup(func() { _ = reopened.Close() })
				provider = reopened
				store = reopened.(EvidenceStore)
			}
			next, err := store.QueryEvidence(t.Context(), scope, q)
			if err != nil || len(next.Items) != 3 || next.Total != 13 {
				t.Fatalf("old snapshot continuation: %+v %v", next, err)
			}
			for _, record := range next.Items {
				var issue EvidenceIssue
				_ = json.Unmarshal(record.Payload, &issue)
				if !issue.LastSeen.Equal(opened) {
					t.Fatal("heartbeat leaked into a pinned snapshot")
				}
			}
			q.Cursor = ""
			q.Since = now.Add(-time.Hour)
			fresh, err := store.QueryEvidence(t.Context(), scope, q)
			if err != nil || fresh.Total != 13 || fresh.Next == "" {
				t.Fatalf("fresh recent snapshot: %+v %v", fresh, err)
			}
			for _, record := range fresh.Items {
				var issue EvidenceIssue
				_ = json.Unmarshal(record.Payload, &issue)
				if !issue.LastSeen.Equal(recent) || !issue.FirstSeen.Equal(opened) {
					t.Fatal("fresh occurrence lost observation or onset")
				}
			}
			journal, err := store.QueryEvidence(t.Context(), scope, EvidenceQuery{Since: opened.Add(-time.Minute), Until: now, Type: "issue"})
			if err != nil || journal.Total != 13 {
				t.Fatalf("semantic JSON heartbeat duplicated full journal: %d %v", journal.Total, err)
			}
			if backend == "postgres" {
				key, _ := evidenceScopeKey(scope)
				var versions, observations int
				db := provider.(SQLAccessor).DB()
				if err = db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM kubernetes_evidence_occurrence_versions WHERE scope_key=$1`, key).Scan(&versions); err != nil {
					t.Fatal(err)
				}
				if err = db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM kubernetes_evidence_occurrence_observations WHERE scope_key=$1`, key).Scan(&observations); err != nil {
					t.Fatal(err)
				}
				if versions != 13 || observations != 26 {
					t.Fatalf("payload versions=%d compact observations=%d", versions, observations)
				}
			}
			q.Cursor = fresh.Next
			capture("resolved", now.Add(-30*time.Second), "", false)
			next, err = store.QueryEvidence(t.Context(), scope, q)
			if err != nil || len(next.Items) != 3 || next.Total != 13 {
				t.Fatalf("resolution changed pinned membership: %+v %v", next, err)
			}
			for _, record := range next.Items {
				var issue EvidenceIssue
				_ = json.Unmarshal(record.Payload, &issue)
				if issue.ClosedAt != nil || !issue.LastSeen.Equal(recent) {
					t.Fatal("resolution leaked into pinned payload")
				}
			}
			current := q
			current.Cursor = ""
			current.UID = "pod-00"
			resolved, err := store.QueryEvidence(t.Context(), scope, current)
			if err != nil || len(resolved.Items) != 1 {
				t.Fatalf("fresh resolution: %+v %v", resolved, err)
			}
			var issue EvidenceIssue
			_ = json.Unmarshal(resolved.Items[0].Payload, &issue)
			if issue.ClosedAt == nil {
				t.Fatal("fresh snapshot missed resolution")
			}
			if err = store.DeleteEvidence(t.Context(), scope); err != nil {
				t.Fatal(err)
			}
			stale := capture("after-delete", now.Add(-10*time.Second), `{"severity":"warning"}`, true)
			if _, err = store.QueryEvidence(t.Context(), scope, q); !errors.Is(err, ErrEvidenceConflict) {
				t.Fatalf("pre-delete occurrence cursor survived: %v", err)
			}
			archive := scope
			archive.OrgID = DefaultOrgID
			archiveBatch := EvidenceBatch{ID: "after-delete", At: now.Add(-10 * time.Second), Epoch: "synthetic", Issues: []EvidenceIssue{{Key: "pod-00", Kind: "Pod", UID: "pod-00", Namespace: "shop", Rule: "pod.crash_loop", Broken: true, LastSeen: now.Add(-10 * time.Second), Payload: json.RawMessage(`{"severity":"critical"}`)}}}
			if err = store.CommitEvidence(t.Context(), archive, archiveBatch); err != nil {
				t.Fatal(err)
			}
			readScope := scope
			readScope.ReadOrgs = []string{scope.OrgID, DefaultOrgID}
			hidden, err := store.QueryEvidence(t.Context(), readScope, EvidenceQuery{Since: opened, Until: now, Type: "occurrence", Variant: "critical"})
			if err != nil || hidden.Total != 0 {
				t.Fatalf("filter revived a shadowed archive occurrence: total=%d err=%v", hidden.Total, err)
			}
			stale.Generation--
			stale.Expected = "after-delete"
			if err = store.CommitEvidence(t.Context(), scope, stale); !errors.Is(err, ErrEvidenceConflict) {
				t.Fatalf("idempotence bypassed deletion generation: %v", err)
			}
		})
	}
}

func TestOccurrenceQueriesLeavePreexistingJournalUntouched(t *testing.T) {
	for _, backend := range []string{"memory", "file"} {
		t.Run(backend, func(t *testing.T) {
			var provider Provider = NewMemory()
			if backend == "file" {
				var err error
				provider, err = NewFile(FileOptions{DataDir: t.TempDir()})
				if err != nil {
					t.Fatal(err)
				}
			}
			scope := EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}
			opened := time.Now().UTC().Add(-2 * time.Hour)
			recent := opened.Add(119 * time.Minute)
			issue := EvidenceIssue{Key: "pod-rule", OccurrenceID: "preexisting-occurrence", Kind: "Pod", UID: "pod", Namespace: "shop", FirstSeen: opened, LastSeen: opened, Epoch: "synthetic", Payload: json.RawMessage(`{"severity":"warning"}`)}
			record := issueEvidence(issue, EvidenceBatch{ID: "preexisting", At: opened, Epoch: "synthetic"}, "issue_opened")
			record.Sequence = 1
			issue.LastSeen = recent
			state := evidenceState{Sequence: 1, Records: []EvidenceRecord{record}, Active: map[string]EvidenceIssue{"pod-rule": issue}, Checkpoint: EvidenceCheckpoint{ID: "preexisting", At: recent}}
			raw, _ := json.Marshal(state)
			key := "kubernetes/evidence/" + evidenceDigest(scope.OrgID, scope.ClusterID, scope.ClusterUID)
			if err := provider.WriteBlob(key, raw); err != nil {
				t.Fatal(err)
			}
			store := provider.(EvidenceStore)
			query := EvidenceQuery{Since: opened.Add(-time.Minute), Until: recent.Add(2 * time.Minute), Type: "occurrence"}
			page, err := store.QueryEvidence(t.Context(), scope, query)
			if err != nil || page.Total != 0 {
				t.Errorf("preexisting journal became occurrence history: total=%d err=%v", page.Total, err)
			}
			journalQuery := query
			journalQuery.Type = "issue"
			page, err = store.QueryEvidence(t.Context(), scope, journalQuery)
			if err != nil || page.Total != 0 {
				t.Errorf("preexisting journal became current capture history: total=%d err=%v", page.Total, err)
			}
			checkpoint, err := store.EvidenceCheckpoint(t.Context(), scope)
			if err != nil || checkpoint.ID != "" || checkpoint.OccurrenceRevision != 0 {
				t.Errorf("read initialized a capture checkpoint from preexisting data: %+v %v", checkpoint, err)
			}
			unchanged, err := provider.ReadBlob(key)
			if err != nil || string(unchanged) != string(raw) {
				t.Fatal("read changed preexisting bytes")
			}
			fresh := reviewEvidenceCapture(t, store, scope, "fresh", recent.Add(time.Minute), `{"severity":"warning"}`)
			if err = store.CommitEvidence(t.Context(), scope, fresh); err != nil {
				t.Fatal(err)
			}
			if err = store.CommitEvidence(t.Context(), scope, fresh); err != nil {
				t.Fatal(err)
			}
			page, err = store.QueryEvidence(t.Context(), scope, query)
			if err != nil || page.Total != 1 {
				t.Fatalf("fresh capture not available: total=%d err=%v", page.Total, err)
			}
			var captured EvidenceIssue
			if err = json.Unmarshal(page.Items[0].Payload, &captured); err != nil {
				t.Fatal(err)
			}
			if captured.OccurrenceID == issue.OccurrenceID || !captured.FirstSeen.Equal(fresh.At) || !captured.LastSeen.Equal(fresh.At) {
				t.Errorf("fresh capture inherited preexisting occurrence state: %+v", captured)
			}
			unchanged, err = provider.ReadBlob(key)
			if err != nil || string(unchanged) != string(raw) {
				t.Error("fresh capture altered preexisting bytes")
			}
		})
	}
}

func TestPostgresOccurrenceSchemaInitializationLeavesExistingDataUntouched(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("explicit isolated harness Postgres required")
	}
	provider, err := NewPostgres(PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatal("isolated database unavailable")
	}
	defer provider.Close()
	tx, err := provider.(SQLAccessor).DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	// A transaction-local schema keeps the shared service and other schemas isolated.
	schema := fmt.Sprintf("occurrence_migration_%d", time.Now().UnixNano())
	if _, err = tx.ExecContext(t.Context(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(t.Context(), "SET LOCAL search_path TO "+schema+",public"); err != nil {
		t.Fatal(err)
	}
	before, err := migrationFiles.ReadFile("migrations/016_kubernetes_evidence.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(t.Context(), string(before)); err != nil {
		t.Fatal(err)
	}
	opened := time.Now().UTC().Truncate(time.Microsecond).Add(-2 * time.Hour)
	recent := opened.Add(119 * time.Minute)
	issue := EvidenceIssue{Key: "key", OccurrenceID: "occurrence", Kind: "Pod", UID: "pod", FirstSeen: opened, LastSeen: opened, Epoch: "synthetic", Payload: json.RawMessage(`{"severity":"warning"}`)}
	payload, _ := json.Marshal(issue)
	if _, err = tx.ExecContext(t.Context(), `INSERT INTO kubernetes_evidence_scopes(scope_key,org_id,cluster_id,cluster_uid,checkpoint) VALUES('legacy','org','cluster','physical','{"id":"before-upgrade"}')`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(t.Context(), `INSERT INTO kubernetes_evidence_events(scope_key,event_id,event_version,event_type,event_at,observed_at,epoch,payload) VALUES('legacy','opened','version','issue_opened',$1,$1,'synthetic',$2)`, opened, payload); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(t.Context(), `INSERT INTO kubernetes_evidence_issues(scope_key,occurrence_id,issue_key,first_seen,last_seen,payload) VALUES('legacy','occurrence','key',$1,$2,$3)`, opened, recent, payload); err != nil {
		t.Fatal(err)
	}
	migration, err := migrationFiles.ReadFile("migrations/017_kubernetes_occurrence_snapshots.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(t.Context(), string(migration)); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(t.Context(), string(migration)); err != nil {
		t.Fatal(err)
	}
	var observations, journal, versions int
	if err = tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM kubernetes_evidence_occurrence_observations WHERE scope_key='legacy'`).Scan(&observations); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM kubernetes_evidence_events`).Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM kubernetes_evidence_occurrence_versions`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if observations != 0 || journal != 1 || versions != 0 {
		t.Fatalf("schema initialization imported rows: observations=%d journal=%d versions=%d", observations, journal, versions)
	}
	var checkpoint string
	if err = tx.QueryRowContext(t.Context(), `SELECT checkpoint->>'id' FROM kubernetes_evidence_scopes WHERE scope_key='legacy' AND NOT checkpoint ? 'occurrence_revision'`).Scan(&checkpoint); err != nil || checkpoint != "before-upgrade" {
		t.Fatalf("schema initialization altered checkpoint: %q %v", checkpoint, err)
	}
	var retained []byte
	var seen time.Time
	var version string
	if err = tx.QueryRowContext(t.Context(), `SELECT payload,last_seen,payload_version FROM kubernetes_evidence_issues WHERE scope_key='legacy'`).Scan(&retained, &seen, &version); err != nil {
		t.Fatal(err)
	}
	var restored EvidenceIssue
	if json.Unmarshal(retained, &restored) != nil || !seen.Equal(recent) || !restored.FirstSeen.Equal(opened) || !restored.LastSeen.Equal(opened) || version != "" {
		t.Fatal("schema initialization changed existing occurrence data")
	}

}

func TestPostgresEvidenceQueriesLeavePreexistingJournalUntouched(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("explicit isolated harness Postgres required")
	}
	provider, err := NewPostgres(PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatal("isolated database unavailable")
	}
	defer provider.Close()
	store := provider.(EvidenceStore)
	db := provider.(SQLAccessor).DB()
	scope := EvidenceScope{OrgID: "synthetic-no-import", ClusterID: fmt.Sprintf("cluster-%d", time.Now().UnixNano()), ClusterUID: "physical"}
	key := "kubernetes/evidence/" + evidenceDigest(scope.OrgID, scope.ClusterID, scope.ClusterUID)
	opened := time.Now().UTC().Truncate(time.Microsecond).Add(-2 * time.Hour)
	recent := opened.Add(119 * time.Minute)
	issue := EvidenceIssue{Key: "pod-rule", OccurrenceID: "preexisting-occurrence", Kind: "Pod", UID: "pod", Namespace: "shop", FirstSeen: opened, LastSeen: opened, Epoch: "synthetic", Payload: json.RawMessage(`{"severity":"warning"}`)}
	payload, _ := json.Marshal(issue)
	checkpoint, _ := json.Marshal(EvidenceCheckpoint{ID: "preexisting", At: recent})
	if _, err = db.ExecContext(t.Context(), `INSERT INTO kubernetes_evidence_scopes(scope_key,org_id,cluster_id,cluster_uid,checkpoint) VALUES($1,$2,$3,$4,$5)`, key, scope.OrgID, scope.ClusterID, scope.ClusterUID, checkpoint); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(t.Context(), `INSERT INTO kubernetes_evidence_issues(scope_key,occurrence_id,issue_key,first_seen,last_seen,payload) VALUES($1,$2,$3,$4,$5,$6)`, key, issue.OccurrenceID, issue.Key, opened, recent, payload); err != nil {
		t.Fatal(err)
	}
	query := EvidenceQuery{Since: opened.Add(-time.Minute), Until: recent.Add(2 * time.Minute), Type: "occurrence"}
	page, err := store.QueryEvidence(t.Context(), scope, query)
	if err != nil || page.Total != 0 {
		t.Errorf("preexisting occurrence visible: total=%d err=%v", page.Total, err)
	}
	current, err := store.EvidenceCheckpoint(t.Context(), scope)
	if err != nil || current.ID != "" {
		t.Errorf("preexisting checkpoint reused: %+v %v", current, err)
	}
	fresh := reviewEvidenceCapture(t, store, scope, "fresh", recent.Add(time.Minute), `{"severity":"warning"}`)
	if err = store.CommitEvidence(t.Context(), scope, fresh); err != nil {
		t.Fatal(err)
	}
	if err = store.CommitEvidence(t.Context(), scope, fresh); err != nil {
		t.Fatal(err)
	}
	page, err = store.QueryEvidence(t.Context(), scope, query)
	if err != nil || page.Total != 1 {
		t.Fatalf("fresh capture: total=%d err=%v", page.Total, err)
	}
	var captured EvidenceIssue
	_ = json.Unmarshal(page.Items[0].Payload, &captured)
	if captured.OccurrenceID == issue.OccurrenceID || !captured.FirstSeen.Equal(fresh.At) {
		t.Error("fresh capture inherited preexisting occurrence")
	}
	var retained []byte
	var seen time.Time
	var version, id string
	if err = db.QueryRowContext(t.Context(), `SELECT payload,last_seen,payload_version FROM kubernetes_evidence_issues WHERE scope_key=$1`, key).Scan(&retained, &seen, &version); err != nil {
		t.Fatal(err)
	}
	var restored EvidenceIssue
	if json.Unmarshal(retained, &restored) != nil || !seen.Equal(recent) || !restored.FirstSeen.Equal(opened) || !restored.LastSeen.Equal(opened) || version != "" {
		t.Error("new capture changed preexisting occurrence data")
	}
	if err = db.QueryRowContext(t.Context(), `SELECT checkpoint->>'id' FROM kubernetes_evidence_scopes WHERE scope_key=$1`, key).Scan(&id); err != nil || id != "preexisting" {
		t.Errorf("new capture changed preexisting checkpoint: %q %v", id, err)
	}
}

func TestCurrentCaptureDoesNotSeedUnversionedOccurrenceState(t *testing.T) {
	for _, backend := range []string{"memory", "file"} {
		t.Run(backend, func(t *testing.T) {
			var provider Provider = NewMemory()
			if backend == "file" {
				var err error
				provider, err = NewFile(FileOptions{DataDir: t.TempDir()})
				if err != nil {
					t.Fatal(err)
				}
			}
			scope := EvidenceScope{OrgID: "org", ClusterID: "cluster", ClusterUID: "physical"}
			at := time.Now().UTC().Add(-time.Hour)
			state := evidenceState{Active: map[string]EvidenceIssue{"pod-rule": {Key: "pod-rule", OccurrenceID: "unversioned", Kind: "Pod", UID: "pod", FirstSeen: at, LastSeen: at, Payload: json.RawMessage(`{"severity":"warning"}`)}}, Checkpoint: EvidenceCheckpoint{ID: "unversioned", At: at}}
			raw, _ := json.Marshal(state)
			key, _ := evidenceScopeKey(scope)
			if err := provider.WriteBlob(key, raw); err != nil {
				t.Fatal(err)
			}
			store := provider.(EvidenceStore)
			batch := reviewEvidenceCapture(t, store, scope, "fresh", at.Add(time.Minute), `{"severity":"warning"}`)
			if err := store.CommitEvidence(t.Context(), scope, batch); !errors.Is(err, ErrInvalidEvidence) {
				t.Fatalf("unversioned state seeded current history: %v", err)
			}
			retained, err := provider.ReadBlob(key)
			if err != nil || string(retained) != string(raw) {
				t.Fatal("invalid state was altered")
			}
		})
	}
}
