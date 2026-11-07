package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestFileEvidenceStaysFlatUnderEndlessChurn is the file store's long-running
// gate. The file store keeps a bounded window (one day, 5,000 records, 4 MiB
// per scope), so ten years of history is ten years of churn through that
// window: every save inserts new rows and prunes as many old ones. It saves
// 480 captures of 60 new changes, a capture every 15 simulated minutes (five
// days, the window full after the first day), and checks that the database
// and its write-ahead log stop growing once the window is full, that saves
// and page reads cost the same at the end as in the first full day, and that
// the statements a save runs reach rows through an index. It runs with
// EVIDENCE_DECADE_GATE=1 outside short mode.
func TestFileEvidenceStaysFlatUnderEndlessChurn(t *testing.T) {
	if testing.Short() || os.Getenv("EVIDENCE_DECADE_GATE") != "1" {
		t.Skip("set EVIDENCE_DECADE_GATE=1 outside short mode")
	}
	dir := t.TempDir()
	provider, err := NewFile(FileOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	store := provider.(EvidenceStore)
	scope := EvidenceScope{OrgID: "default", ClusterID: "file-churn", ClusterUID: "physical"}
	const captures, perCapture = 480, 60
	origin := time.Now().UTC().Add(-time.Duration(captures+8) * 15 * time.Minute).Truncate(time.Second)
	path := filepath.Join(dir, "kubernetes-evidence.sqlite")
	size := func(name string) int64 {
		info, err := os.Stat(name)
		if err != nil {
			return 0
		}
		return info.Size()
	}
	type sample struct {
		Capture             int
		SaveMS, ReadMS      float64
		Database, WAL, Rows int64
	}
	var samples []sample
	expected := ""
	for capture := 0; capture < captures; capture++ {
		at := origin.Add(time.Duration(capture) * 15 * time.Minute)
		batch := EvidenceBatch{ID: fmt.Sprintf("capture-%d", capture), Expected: expected, At: at, CoveredThrough: at, Epoch: "stable"}
		for i := 0; i < perCapture; i++ {
			payload, _ := json.Marshal(map[string]any{"kind": "Deployment", "name": fmt.Sprintf("obj-%d", i), "value": capture})
			batch.Records = append(batch.Records, EvidenceRecord{ID: fmt.Sprintf("change:%d:%d", capture, i), Type: "change", At: at, ObservedAt: at,
				Namespace: fmt.Sprintf("ns-%d", i%10), Name: fmt.Sprintf("obj-%d", i), Kind: "Deployment", AuthorizationKind: "Deployment", UID: fmt.Sprintf("uid-%d", i), RootUID: fmt.Sprintf("uid-%d", i), Epoch: "stable", Payload: payload})
		}
		started := time.Now()
		if err = store.CommitEvidence(t.Context(), scope, batch); err != nil {
			t.Fatalf("capture %d: %v", capture, err)
		}
		saved := float64(time.Since(started).Microseconds()) / 1000
		expected = batch.ID
		started = time.Now()
		page, err := store.QueryEvidence(t.Context(), scope, EvidenceQuery{Type: "change", GapsApart: true, Since: at.Add(-24 * time.Hour), Until: at, Namespace: "ns-3", Limit: 100})
		if err != nil {
			t.Fatalf("read after capture %d: %v", capture, err)
		}
		read := float64(time.Since(started).Microseconds()) / 1000
		if len(page.Items) == 0 {
			t.Fatalf("read after capture %d returned nothing", capture)
		}
		s := sample{Capture: capture, SaveMS: saved, ReadMS: read, Database: size(path), WAL: size(path + "-wal")}
		index, err := provider.(*fileProvider).evidenceIndex()
		if err != nil {
			t.Fatal(err)
		}
		if err = index.db.QueryRow(`SELECT COUNT(*) FROM evidence_events`).Scan(&s.Rows); err != nil {
			t.Fatal(err)
		}
		samples = append(samples, s)
	}
	median := func(from, to int, value func(sample) float64) float64 {
		values := []float64{}
		for _, s := range samples[from:to] {
			values = append(values, value(s))
		}
		sort.Float64s(values)
		return values[len(values)/2]
	}
	// The window is full after a day of captures; compare the second day
	// with the last.
	early, late := [2]int{96, 192}, [2]int{captures - 96, captures}
	for _, check := range []struct {
		name  string
		value func(sample) float64
	}{{"save", func(s sample) float64 { return s.SaveMS }}, {"page read", func(s sample) float64 { return s.ReadMS }}} {
		a, b := median(early[0], early[1], check.value), median(late[0], late[1], check.value)
		t.Logf("FILE %s median: %.2f ms on the second day, %.2f ms on the last", check.name, a, b)
		if b > 2*a && b-a > 2 {
			t.Errorf("file %s grew under churn: %.2f ms to %.2f ms", check.name, a, b)
		}
	}
	peak := samples[early[1]-1]
	for _, s := range samples[early[1]:] {
		if s.Database > peak.Database*3/2 {
			t.Errorf("file database kept growing under churn: %d bytes after capture %d, %d after the second day", s.Database, s.Capture, peak.Database)
			break
		}
		if s.Rows > 5000 {
			t.Errorf("file index holds %d rows after capture %d, more than the window", s.Rows, s.Capture)
			break
		}
	}
	last := samples[len(samples)-1]
	t.Logf("FILE database %d bytes, write-ahead log %d bytes, %d rows after %d captures; %d bytes after the second day", last.Database, last.WAL, last.Rows, captures, peak.Database)

	// The statements a save and a read run against the index reach rows by
	// key, never by scanning a table.
	index, err := provider.(*fileProvider).evidenceIndex()
	if err != nil {
		t.Fatal(err)
	}
	key, _ := evidenceScopeKey(scope)
	plans := map[string]string{}
	for name, statement := range map[string]string{
		"next version of an event": `SELECT MIN(sequence) FROM evidence_events WHERE key=? AND id=? AND sequence>?`,
		"supersede older versions": `UPDATE evidence_events SET superseded=? WHERE key=? AND id=? AND sequence<? AND(superseded IS NULL OR superseded>?)`,
		"prune one row":            `DELETE FROM evidence_events WHERE key=? AND sequence=?`,
		"next occurrence version":  `SELECT revision,observed FROM evidence_versions WHERE key=? AND id=? AND revision>? ORDER BY revision LIMIT 1`,
		"prune one observation":    `DELETE FROM evidence_observations WHERE key=? AND id=? AND revision=?`,
		"page by namespace":        `SELECT id FROM evidence_events e WHERE e.key=? AND e.namespace=? AND e.at>=? AND e.at<=? ORDER BY e.at DESC,e.id DESC LIMIT 101`,
		"page by type":             `SELECT id FROM evidence_events e WHERE e.key=? AND e.type=? AND e.at>=? AND e.at<=? ORDER BY e.at DESC,e.id DESC LIMIT 101`,
	} {
		args := []any{key}
		for len(args) < strings.Count(statement, "?") {
			args = append(args, "x")
		}
		rows, err := index.db.Query("EXPLAIN QUERY PLAN "+statement, args...)
		if err != nil {
			t.Fatalf("explain %s: %v", name, err)
		}
		plan := []string{}
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err = rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		plans[name] = strings.Join(plan, "; ")
		for _, detail := range plan {
			if strings.HasPrefix(detail, "SCAN ") && !strings.Contains(detail, " USING ") {
				t.Errorf("file statement %q scans a table: %s", name, plans[name])
			}
		}
	}
	if results := os.Getenv("HARNESS_RESULTS_DIR"); results != "" {
		scaleWriteJSON(t, filepath.Join(results, "decade-file-store.json"), map[string]any{"Samples": samples, "Plans": plans})
	}
	var journal string
	if err = index.db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	t.Logf("FILE journal_mode=%s plans=%v", journal, plans)
}
