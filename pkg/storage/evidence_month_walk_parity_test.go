package storage

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"
)

// TestPostgresMonthWalkMatchesOneStatement reads random history spread over
// fifteen months two ways: as the store does, a few months per statement
// walking back from the newest, and as one statement over the whole range.
// Every page of every all-time and ranged path must be identical: items in
// order with their payloads, totals and capped counts, the separate gaps,
// and the cursor to the next page, followed to deep pages. The history has
// two scopes read together (the first takes precedence), a denied kind, crash
// statuses refreshed in place for weeks, coverage gaps stored at their start
// and at their end, and a first-scope event changed after the reads were
// pinned.
func TestPostgresMonthWalkMatchesOneStatement(t *testing.T) {
	if testing.Short() {
		t.Skip("writes fifteen months of history")
	}
	snapshotOwnDatabase(t)
	p, _ := headsPartitionedProvider(t)
	random := rand.New(rand.NewSource(20261107))
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	origin := now.AddDate(0, -14, 0)
	main := EvidenceScope{OrgID: "default", ClusterID: "walk-parity", ClusterUID: "physical"}
	licensed := EvidenceScope{OrgID: "acme", ClusterID: "walk-parity", ClusterUID: "physical"}
	reader := licensed
	reader.ReadOrgs = []string{"acme", "default"}
	at := func() time.Time { return origin.Add(time.Duration(random.Int63n(int64(now.Sub(origin))))) }
	record := func(id string, value int) EvidenceRecord {
		when := at()
		ns := fmt.Sprintf("ns-%d", random.Intn(4))
		kind := []string{"Deployment", "Deployment", "Pod", "Secret"}[random.Intn(4)]
		r := EvidenceRecord{ID: id, At: when, ObservedAt: when, Namespace: ns, Name: fmt.Sprintf("obj-%d", random.Intn(6)), Kind: kind, AuthorizationKind: kind, UID: "u-" + id, RootUID: fmt.Sprintf("root-%d", random.Intn(5)), Epoch: "walk"}
		switch roll := random.Intn(100); {
		case roll < 55:
			r.Type = "change"
			r.Payload, _ = json.Marshal(map[string]any{"value": value, "id": id})
		case roll < 80:
			r.Type, r.Kind, r.AuthorizationKind, r.PodUID = "crash", "Deployment", "Pod", "pod-"+id
			subtype := "termination"
			if roll >= 72 {
				subtype = "status"
				r.ObservedAt = when.Add(time.Duration(random.Intn(40*24)) * time.Hour)
				if r.ObservedAt.After(now) {
					r.ObservedAt = now
				}
			}
			r.Payload, _ = json.Marshal(map[string]any{"type": subtype, "value": value})
		case roll < 94:
			r.Type = []string{"issue_opened", "issue_observed", "issue_resolved"}[random.Intn(3)]
			r.Broken, r.Rule, r.Kind, r.AuthorizationKind = random.Intn(2) == 0, "rule-a", "Deployment", "Deployment"
			r.Payload, _ = json.Marshal(map[string]any{"rule": "rule-a", "value": value})
		default:
			length := time.Duration(1+random.Intn(60*24)) * time.Minute
			r.Type, r.Namespace, r.Kind, r.AuthorizationKind, r.Name, r.RootUID, r.UID = "coverage_gap", "", "", "", "", "", ""
			r.GapFrom, r.GapTo = when, when.Add(length)
			if r.GapTo.After(now) {
				r.GapTo = now
			}
			r.ObservedAt = r.GapTo
			if random.Intn(2) == 0 {
				// Stored at its end, as the producer does; otherwise at its start.
				r.At = r.GapTo
			}
			r.Payload, _ = json.Marshal(map[string]any{"from": r.GapFrom, "to": r.GapTo})
		}
		return r
	}
	commit := func(scope EvidenceScope, id string, records []EvidenceRecord) {
		t.Helper()
		cp, err := p.EvidenceCheckpoint(t.Context(), scope)
		if err != nil {
			t.Fatal(err)
		}
		if err = p.CommitEvidence(t.Context(), scope, EvidenceBatch{ID: id, Expected: cp.ID, Generation: cp.Generation, At: now, Epoch: "walk", Records: records}); err != nil {
			t.Fatal(err)
		}
	}
	mainRecords, licensedRecords := []EvidenceRecord{}, []EvidenceRecord{}
	for i := 0; i < 2600; i++ {
		mainRecords = append(mainRecords, record(fmt.Sprintf("e-%d", i), 1))
	}
	for i := 0; i < 400; i++ {
		// Every other licensed event is also in the default scope.
		licensedRecords = append(licensedRecords, record(fmt.Sprintf("e-%d", i*2), 1))
	}
	commit(main, "m0", mainRecords)
	commit(licensed, "l0", licensedRecords)
	// New versions of some events, and crash statuses observed again.
	again := []EvidenceRecord{}
	for i, r := range mainRecords {
		if i%9 == 0 && r.Type != "coverage_gap" {
			changed := r
			changed.Payload, _ = json.Marshal(map[string]any{"value": 2, "id": r.ID})
			again = append(again, changed)
		}
	}
	commit(main, "m1", again)
	authorities := map[string]EvidenceAuthority{"Deployment": {State: "allowed", ProvedAt: now}, "Pod": {State: "allowed", ProvedAt: now}, "Secret": {State: "denied", ProvedAt: now}}
	type path struct {
		name  string
		scope EvidenceScope
		query EvidenceQuery
	}
	all, ranged := now.AddDate(-1, -6, 0), now.AddDate(0, 0, -40)
	query := func(q EvidenceQuery, since time.Time) EvidenceQuery {
		q.Since, q.Until, q.Limit, q.EnforceAuthority, q.Authorities, q.AllowedKinds = since, now, 37, true, authorities, []string{"Deployment", "Pod"}
		return q
	}
	paths := []path{}
	for _, since := range []struct {
		label string
		at    time.Time
	}{{"all-time", all}, {"40 days", ranged}} {
		paths = append(paths,
			path{"changes " + since.label, main, query(EvidenceQuery{Type: "change", GapsApart: true}, since.at)},
			path{"changes in the keyset with gaps " + since.label, main, query(EvidenceQuery{Type: "change", WithGaps: true}, since.at)},
			path{"one object " + since.label, main, query(EvidenceQuery{Type: "change", GapsApart: true, Namespace: "ns-1", Kind: "Deployment", Name: "obj-2"}, since.at)},
			path{"timeline " + since.label, main, query(EvidenceQuery{Type: "timeline", GapsApart: true}, since.at)},
			path{"crashes by observation " + since.label, main, query(EvidenceQuery{Type: "crash", ByObservation: true, GapsApart: true}, since.at)},
			path{"two-scope changes " + since.label, reader, query(EvidenceQuery{Type: "change", GapsApart: true}, since.at)},
			path{"two-scope timeline " + since.label, reader, query(EvidenceQuery{Type: "timeline", GapsApart: true}, since.at)},
		)
	}
	read := func(single bool, scope EvidenceScope, q EvidenceQuery) EvidencePage {
		t.Helper()
		p.evidenceSingleStep = single
		defer func() { p.evidenceSingleStep = false }()
		p.evidenceQueryMu.Lock()
		p.evidenceQueries = nil
		p.evidenceQueryMu.Unlock()
		page, err := p.QueryEvidence(t.Context(), scope, q)
		if err != nil {
			t.Fatalf("read (single=%v): %v", single, err)
		}
		page.Checkpoint = EvidenceCheckpoint{}
		return page
	}
	compared, pages := 0, 0
	run := func(label string) {
		for _, path := range paths {
			walkQuery, singleQuery := path.query, path.query
			for depth := 1; depth <= 12; depth++ {
				walked, single := read(false, path.scope, walkQuery), read(true, path.scope, singleQuery)
				if !reflect.DeepEqual(walked, single) {
					t.Fatalf("%s %q page %d differs:\nwalk:   %d items total=%d counts=%v gaps=%d next=%q\nsingle: %d items total=%d counts=%v gaps=%d next=%q",
						label, path.name, depth, len(walked.Items), walked.Total, walked.Counts, len(walked.Gaps), walked.Next, len(single.Items), single.Total, single.Counts, len(single.Gaps), single.Next)
				}
				compared += len(walked.Items) + len(walked.Gaps)
				pages++
				if depth == 1 && path.scope.ReadOrgs != nil && walked.Next != "" {
					// A first-scope event changes after the read was pinned:
					// later pages must still treat the default scope's copy
					// as shadowed.
					changed := licensedRecords[1]
					changed.Payload, _ = json.Marshal(map[string]any{"value": depth + 10, "id": changed.ID})
					commit(licensed, fmt.Sprintf("l-%s-%s", label, path.name), []EvidenceRecord{changed})
				}
				if walked.Next == "" {
					break
				}
				walkQuery.Cursor, singleQuery.Cursor = walked.Next, single.Next
			}
		}
	}
	run("lag known")
	// Before a scope's observation delay is known, crash reads walk every month.
	if _, err := p.db.Exec(`UPDATE kubernetes_evidence_scopes SET observed_lag_scan='9999-12-31' WHERE cluster_id='walk-parity'`); err != nil {
		t.Fatal(err)
	}
	run("lag unknown")
	if pages < 100 {
		t.Fatalf("only %d pages compared", pages)
	}
	t.Logf("PARITY pages=%d rows_and_gaps=%d paths=%d", pages, compared, len(paths))
}

// A month that retention drops after a read has listed the months, but before
// the read reaches it, makes the read start again with the months that remain
// instead of failing.
func TestPostgresJournalReadRetriesWhenAMonthIsDropped(t *testing.T) {
	if testing.Short() {
		t.Skip("drops a month of history")
	}
	snapshotOwnDatabase(t)
	p, scope := headsPartitionedProvider(t)
	now := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	old := now.AddDate(0, -3, 0)
	headsCommit(t, p, scope, "c0", now, headsChange("change:old", "1", old), headsChange("change:new", "1", now))
	month := "evidence_event_month_" + old.Format("200601")
	listed := 0
	p.evidenceMonthsListed = func() {
		listed++
		if listed == 1 {
			if _, err := p.db.Exec("DROP TABLE " + month); err != nil {
				t.Errorf("drop %s: %v", month, err)
			}
		}
	}
	defer func() { p.evidenceMonthsListed = nil }()
	page, err := p.QueryEvidence(t.Context(), scope, EvidenceQuery{Type: "change", Since: old.AddDate(0, -1, 0), Until: now.Add(time.Minute), Limit: 10})
	if err != nil {
		t.Fatalf("read after the month was dropped: %v", err)
	}
	if listed != 2 {
		t.Fatalf("the read listed the months %d times, want 2", listed)
	}
	if len(page.Items) != 1 || page.Items[0].ID != "change:new" {
		t.Fatalf("read after the drop returned %d items %+v, want only the remaining month's event", len(page.Items), page.Items)
	}
}
