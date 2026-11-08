package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

// gapsApartBackends opens every evidence store a deployment can run on. The
// Postgres store is used only when TEST_POSTGRES_DSN is set.
func gapsApartBackends(t *testing.T) map[string]func(t *testing.T) EvidenceStore {
	t.Helper()
	return map[string]func(t *testing.T) EvidenceStore{
		"memory": func(t *testing.T) EvidenceStore {
			provider, _ := localEvidenceTestProvider(t, "memory")
			return provider.(EvidenceStore)
		},
		"file": func(t *testing.T) EvidenceStore {
			provider, _ := localEvidenceTestProvider(t, "file")
			return provider.(EvidenceStore)
		},
		"postgres": func(t *testing.T) EvidenceStore {
			dsn := os.Getenv("TEST_POSTGRES_DSN")
			if dsn == "" {
				t.Skip("TEST_POSTGRES_DSN not set: Postgres proof did not run")
			}
			provider, err := NewPostgres(PostgresOptions{DSN: dsn, EvidenceRetention: "0"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = provider.Close() })
			return provider.(EvidenceStore)
		},
	}
}

func gapsApartChange(id, authorizationKind string, at, observed time.Time) EvidenceRecord {
	return EvidenceRecord{ID: id, Type: "change", At: at, ObservedAt: observed, Namespace: "shop", Kind: "Deployment", Name: "web", AuthorizationKind: authorizationKind, UID: "web", RootUID: "web", Rule: "image_changed", Epoch: "gaps", Payload: json.RawMessage(`{"secret":"` + id + `"}`)}
}

func gapsApartGap(id string, from time.Time, observed time.Time) EvidenceRecord {
	to := from.Add(10 * time.Second)
	raw, _ := json.Marshal(struct {
		From time.Time `json:"from"`
		To   time.Time `json:"to"`
	}{from, to})
	return EvidenceRecord{ID: id, Type: "coverage_gap", At: from, ObservedAt: observed, Namespace: "shop", GapFrom: from, GapTo: to, Epoch: "gaps", Payload: raw}
}

// With gaps kept apart, the keyset holds only rows the reader may list: the
// first page's total is exactly what the walk shows, every page but the last
// is full, and the gaps (including rows the reader may not see, as bare point
// gaps) still reach the pages whose time range they overlap.
func TestEvidenceGapsApartPagesCountOnlyListableRows(t *testing.T) {
	// Not parallel: it writes to the shared test database, whose journal
	// sequence other tests compare exactly.
	for name, open := range gapsApartBackends(t) {
		t.Run(name, func(t *testing.T) {
			store := open(t)
			now := time.Now().UTC().Truncate(time.Microsecond)
			at := now.Add(-40 * time.Minute)
			scope := EvidenceScope{OrgID: "default", ClusterID: fmt.Sprintf("gaps-apart-%s-%d", name, time.Now().UnixNano()), ClusterUID: "physical"}
			batch := EvidenceBatch{ID: "gaps", At: now, Epoch: "gaps"}
			wantGaps := map[string]bool{}
			for position := 0; position < 6; position++ {
				changeAt := at.Add(time.Duration(position) * time.Minute)
				batch.Records = append(batch.Records, gapsApartChange(fmt.Sprintf("change-%d", position), "Deployment", changeAt, now))
				gap := gapsApartGap(fmt.Sprintf("gap-%d", position), changeAt.Add(20*time.Second), now)
				batch.Records = append(batch.Records, gap)
				wantGaps[gap.ID] = true
			}
			for position := 0; position < 3; position++ {
				denied := gapsApartChange(fmt.Sprintf("denied-%d", position), "Secret", at.Add(time.Duration(position)*time.Minute+40*time.Second), now)
				batch.Records = append(batch.Records, denied)
				wantGaps[denied.ID] = true
			}
			if err := store.CommitEvidence(t.Context(), scope, batch); err != nil {
				t.Fatal(err)
			}
			for _, listType := range []string{"change", "timeline"} {
				query := EvidenceQuery{Type: listType, Namespace: "shop", Since: at.Add(-time.Minute), Until: now, Limit: 3, GapsApart: true, EnforceAuthority: true, AllowedKinds: []string{"Deployment"}}
				total, shown, pages := 0, 0, []int{}
				seenGaps := map[string]bool{}
				for {
					page, err := store.QueryEvidence(t.Context(), scope, query)
					if err != nil {
						t.Fatalf("%s page %d: %v", listType, len(pages)+1, err)
					}
					if len(pages) == 0 {
						total = page.Total
						if page.Counts["coverage_gap"] != 0 {
							t.Fatalf("%s counted gaps as rows: %v", listType, page.Counts)
						}
					}
					pages = append(pages, len(page.Items))
					shown += len(page.Items)
					for _, item := range page.Items {
						if item.Type != "change" || item.AuthorizationKind != "Deployment" {
							t.Fatalf("%s listed %s (%s, %s) as a row", listType, item.ID, item.Type, item.AuthorizationKind)
						}
					}
					for _, gap := range page.Gaps {
						if gap.Type != "coverage_gap" || strings.Contains(string(gap.Payload), "secret") || gap.AuthorizationKind != "" {
							t.Fatalf("%s gap %s carries row content: type=%s kind=%q payload=%s", listType, gap.ID, gap.Type, gap.AuthorizationKind, gap.Payload)
						}
						seenGaps[gap.ID] = true
					}
					if page.Next == "" {
						break
					}
					query.Cursor = page.Next
				}
				if total != 6 || shown != 6 || len(pages) != 2 || pages[0] != 3 {
					t.Fatalf("%s: first-page total %d, walk showed %d rows over pages %v; want 6 rows in two pages of 3", listType, total, shown, pages)
				}
				missing := []string{}
				for id := range wantGaps {
					if !seenGaps[id] {
						missing = append(missing, id)
					}
				}
				sort.Strings(missing)
				if len(missing) > 0 {
					t.Fatalf("%s: gaps never shown on any page: %v", listType, missing)
				}
			}
		})
	}
}

// The gap read of one page is bounded. A page range holding more gaps than the
// bound is reported as one gap over the whole range, never as fully covered.
func TestEvidenceGapsApartBoundsThePageGapRead(t *testing.T) {
	// Not parallel: it writes to the shared test database, whose journal
	// sequence other tests compare exactly.
	for name, open := range gapsApartBackends(t) {
		t.Run(name, func(t *testing.T) {
			store := open(t)
			now := time.Now().UTC().Truncate(time.Microsecond)
			at := now.Add(-50 * time.Minute)
			scope := EvidenceScope{OrgID: "default", ClusterID: fmt.Sprintf("gaps-bound-%s-%d", name, time.Now().UnixNano()), ClusterUID: "physical"}
			batch := EvidenceBatch{ID: "gaps", At: now, Epoch: "gaps", Records: []EvidenceRecord{gapsApartChange("only-change", "Deployment", at, now)}}
			for position := 0; position < evidencePageGapLimit+5; position++ {
				batch.Records = append(batch.Records, gapsApartGap(fmt.Sprintf("dense-%03d", position), at.Add(time.Duration(position)*time.Second), now))
			}
			if err := store.CommitEvidence(t.Context(), scope, batch); err != nil {
				t.Fatal(err)
			}
			since := at.Add(-time.Minute)
			page, err := store.QueryEvidence(t.Context(), scope, EvidenceQuery{Type: "change", Since: since, Until: now, Limit: 10, GapsApart: true})
			if err != nil {
				t.Fatal(err)
			}
			if page.Total != 1 || len(page.Items) != 1 || page.Next != "" {
				t.Fatalf("total=%d items=%d next=%q", page.Total, len(page.Items), page.Next)
			}
			if len(page.Gaps) != 1 || !page.Gaps[0].GapFrom.Equal(since) || !page.Gaps[0].GapTo.Equal(now) {
				t.Fatalf("an overflowing gap read returned %d gaps, want one gap over [%s, %s]: %+v", len(page.Gaps), since, now, page.Gaps)
			}
		})
	}
}

// Every store returns the same coverage gaps for a range, whether a gap is
// stored at its start, at its end or in between, and whether it starts before
// the range, ends after it, or covers it whole, even when it is stored a month
// before the range (outside the file store's one-day window): both with gaps
// in the keyset and with gaps kept apart.
func TestEvidenceGapsOverlappingARangeAreTheSameInEveryStore(t *testing.T) {
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	since, until := now.Add(-2*time.Hour), now.Add(-time.Hour)
	gap := func(id string, from, to, at time.Time) EvidenceRecord {
		raw, _ := json.Marshal(map[string]any{"from": from, "to": to})
		return EvidenceRecord{ID: id, Type: "coverage_gap", At: at, ObservedAt: now, Namespace: "shop", GapFrom: from, GapTo: to, Epoch: "gaps", Payload: raw}
	}
	long := now.Add(-40 * 24 * time.Hour)
	records := []EvidenceRecord{
		gap("gap:starts-before-stored-at-start", since.Add(-30*time.Minute), since.Add(10*time.Minute), since.Add(-30*time.Minute)),
		gap("gap:starts-before-stored-at-end", since.Add(-30*time.Minute), since.Add(20*time.Minute), since.Add(20*time.Minute)),
		gap("gap:covers-stored-at-start-a-month-back", long, until.Add(time.Hour), long),
		gap("gap:inside-stored-between", since.Add(10*time.Minute), since.Add(40*time.Minute), since.Add(25*time.Minute)),
		gap("gap:ends-after-stored-at-end", until.Add(-10*time.Minute), until.Add(30*time.Minute), until.Add(30*time.Minute)),
		gap("gap:before-range", since.Add(-3*time.Hour), since.Add(-2*time.Hour), since.Add(-3*time.Hour)),
		gap("gap:after-range", until.Add(5*time.Minute), until.Add(15*time.Minute), until.Add(5*time.Minute)),
		gapsApartChange("change:inside", "Deployment", since.Add(30*time.Minute), since.Add(30*time.Minute)),
	}
	ids := func(items []EvidenceRecord) []string {
		found := []string{}
		for _, item := range items {
			if item.Type == "coverage_gap" {
				found = append(found, item.ID)
			}
		}
		sort.Strings(found)
		return found
	}
	results := map[string][2][]string{}
	for name, open := range gapsApartBackends(t) {
		t.Run(name, func(t *testing.T) {
			store := open(t)
			scope := EvidenceScope{OrgID: "default", ClusterID: fmt.Sprintf("gap-parity-%s-%d", name, now.UnixNano()), ClusterUID: "physical"}
			cp, err := store.EvidenceCheckpoint(t.Context(), scope)
			if err != nil {
				t.Fatal(err)
			}
			saved := records
			if name == "file" {
				// The file store keeps one day; the month-old gap is past it.
				saved = append([]EvidenceRecord{}, records[:2]...)
				saved = append(saved, records[3:]...)
			}
			if err = store.CommitEvidence(t.Context(), scope, EvidenceBatch{ID: "gaps", Expected: cp.ID, Generation: cp.Generation, At: now, Epoch: "gaps", Records: saved}); err != nil {
				t.Fatal(err)
			}
			inline, err := store.QueryEvidence(t.Context(), scope, EvidenceQuery{Type: "change", WithGaps: true, Since: since, Until: until, Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			apart, err := store.QueryEvidence(t.Context(), scope, EvidenceQuery{Type: "change", GapsApart: true, Since: since, Until: until, Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			results[name] = [2][]string{ids(inline.Items), ids(apart.Gaps)}
		})
	}
	want := []string{"gap:covers-stored-at-start-a-month-back", "gap:ends-after-stored-at-end", "gap:inside-stored-between", "gap:starts-before-stored-at-end", "gap:starts-before-stored-at-start"}
	for name, got := range results {
		want := want
		if name == "file" {
			want = want[1:]
		}
		if strings.Join(got[0], ",") != strings.Join(want, ",") {
			t.Errorf("%s with gaps in the keyset: %v, want %v", name, got[0], want)
		}
		if strings.Join(got[1], ",") != strings.Join(want, ",") {
			t.Errorf("%s with gaps kept apart: %v, want %v", name, got[1], want)
		}
	}
}
