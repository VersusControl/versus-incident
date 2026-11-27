package storage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

// occurrenceTestProvider opens the named backend; Postgres needs an isolated
// harness database and skips without one.
func occurrenceTestProvider(t *testing.T, backend string) Provider {
	t.Helper()
	switch backend {
	case "file":
		provider, err := NewFile(FileOptions{DataDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		return provider
	case "postgres":
		dsn := os.Getenv("TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("explicit isolated harness Postgres required")
		}
		provider, err := NewPostgres(PostgresOptions{DSN: dsn})
		if err != nil {
			t.Fatal("isolated database unavailable")
		}
		t.Cleanup(func() { _ = provider.Close() })
		return provider
	}
	return NewMemory()
}

// seedOccurrenceHistory saves thirteen months of captures into two clusters,
// each kept by an enterprise org and the default org. Issues reopen, change
// payload (replacing their version), close and come back, across three roots,
// two namespaces and two kinds.
func seedOccurrenceHistory(t *testing.T, store EvidenceStore, cluster string, start time.Time) {
	t.Helper()
	for _, org := range []string{"ent", "default"} {
		scope := EvidenceScope{OrgID: org, ClusterID: cluster, ClusterUID: "physical"}
		opened := map[string]time.Time{}
		for month := 0; month < 13; month++ {
			for step := 0; step < 2; step++ {
				at := start.AddDate(0, month, step*9)
				cp, err := store.EvidenceCheckpoint(t.Context(), scope)
				if err != nil {
					t.Fatal(err)
				}
				batch := EvidenceBatch{ID: fmt.Sprintf("%s-%d-%d", org, month, step), Expected: cp.ID, Generation: cp.Generation, At: at, Epoch: "seed", CoveredThrough: at}
				for k := 0; k < 6; k++ {
					key := fmt.Sprintf("issue-%d", k)
					if (month+k)%4 == 0 || org == "default" && k == 5 {
						delete(opened, key)
						continue
					}
					if _, ok := opened[key]; !ok {
						opened[key] = at
					}
					kind := []string{"Deployment", "Pod"}[k%2]
					severity := []string{"warning", "critical"}[(month+step+k)%2]
					if k > 2 {
						severity = "info"
					}
					payload := fmt.Sprintf(`{"severity":%q,"root":{"kind":%q,"namespace":"ns-%d"},"month":%d}`, severity, kind, k%2, month)
					batch.Issues = append(batch.Issues, EvidenceIssue{Key: key, Kind: kind, AuthorizationKind: kind, UID: fmt.Sprintf("root-%d", k%3), Namespace: fmt.Sprintf("ns-%d", k%2),
						Rule: "pod.crash_loop", FirstSeen: opened[key], LastSeen: at, Broken: k%2 == 0, Payload: json.RawMessage(payload)})
				}
				if err = store.CommitEvidence(t.Context(), scope, batch); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

// occurrencePageTranscript reads every page of each query and writes what a
// caller sees: totals, counts, and each item's identity, time and payload.
func occurrencePageTranscript(t *testing.T, store EvidenceStore, start, now time.Time) string {
	t.Helper()
	scopes := map[string]EvidenceScope{
		"licensed": {OrgID: "ent", ReadOrgs: []string{"ent", "default"}, ClusterID: "east", ClusterUID: "physical"},
		"default":  {OrgID: "default", ClusterID: "west", ClusterUID: "physical"},
	}
	windows := [][2]time.Time{{start.Add(-time.Hour), now}, {start.AddDate(0, 3, 4), start.AddDate(0, 7, 2)}, {now.AddDate(0, 0, -40), now}, {start.AddDate(0, 12, 1), start.AddDate(0, 12, 3)}}
	filters := []func(*EvidenceQuery){
		func(*EvidenceQuery) {},
		func(q *EvidenceQuery) { q.RootUID, q.Namespace = "root-1", "ns-1" },
		func(q *EvidenceQuery) { q.RootUID = "root-0" },
		func(q *EvidenceQuery) { q.Namespace = "ns-0" },
		func(q *EvidenceQuery) { q.Variant = ":broken" },
		func(q *EvidenceQuery) { q.Variant, q.Namespace = "warning", "ns-0" },
		func(q *EvidenceQuery) { q.EnforceAuthority, q.AllowedKinds = true, []string{"Pod"} },
		func(q *EvidenceQuery) {
			q.EnforceAuthority, q.AllowedKinds, q.RootUID = true, []string{"Deployment"}, "root-2"
		},
	}
	names := []string{"default", "licensed"}
	var out strings.Builder
	for _, name := range names {
		for w, window := range windows {
			for f, filter := range filters {
				for _, itemsOnly := range []bool{false, true} {
					q := EvidenceQuery{Type: "occurrence", Since: window[0], Until: window[1], Limit: 4, ItemsOnly: itemsOnly}
					filter(&q)
					fmt.Fprintf(&out, "%s window=%d filter=%d items-only=%v\n", name, w, f, itemsOnly)
					for pageNumber := 0; pageNumber < 40; pageNumber++ {
						page, err := store.QueryEvidence(t.Context(), scopes[name], q)
						if err != nil {
							fmt.Fprintf(&out, " error %v\n", err)
							break
						}
						if !itemsOnly && pageNumber == 0 {
							kinds := make([]string, 0, len(page.Counts))
							for kind, count := range page.Counts {
								if count != 0 {
									kinds = append(kinds, fmt.Sprintf("%s=%d", kind, count))
								}
							}
							sort.Strings(kinds)
							fmt.Fprintf(&out, " total=%d capped=%v counts=%v\n", page.Total, page.CountCapped, kinds)
						}
						for _, item := range page.Items {
							if item.Type == "coverage_gap" {
								// A denied row shows its identity and time; the
								// memory store also keeps its last sample time.
								fmt.Fprintf(&out, " %s %s %s\n", item.ID, item.Type, item.At.UTC().Format(time.RFC3339Nano))
								continue
							}
							fmt.Fprintf(&out, " %s %s %s %s %x\n", item.ID, item.Type, item.At.UTC().Format(time.RFC3339Nano), item.ObservedAt.UTC().Format(time.RFC3339Nano), sha256.Sum256(utcOccurrencePayload(t, item.Payload)))
						}
						if page.Next == "" {
							break
						}
						q.Cursor = page.Next
					}
				}
			}
		}
	}
	return out.String()
}

// Every occurrence page, its totals and its samples read the same on every
// backend over a long history: the Postgres reads that look spans up per
// scope and subject, reuse totals and skip needless sample lookups answer
// exactly what the plain in-memory reads answer.
func TestOccurrencePagesAgreeAcrossBackendsOverLongHistory(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	start := now.AddDate(0, -13, 0)
	reference := ""
	for _, backend := range []string{"memory", "file", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			provider := occurrenceTestProvider(t, backend)
			store := provider.(EvidenceStore)
			suffix := ""
			if backend == "postgres" {
				// The isolated database outlives one run; fresh clusters keep
				// earlier runs out of these pages.
				suffix = fmt.Sprintf("-%d", now.UnixNano())
			}
			for _, cluster := range []string{"east", "west"} {
				seedOccurrenceHistory(t, store, cluster+suffix, start)
			}
			transcript := occurrencePageTranscript(t, &renamedClusters{EvidenceStore: store, suffix: suffix}, start, now)
			if !strings.Contains(transcript, " issue_") {
				t.Fatalf("the seeded history listed no occurrence:\n%s", transcript)
			}
			if backend == "memory" {
				reference = transcript
				return
			}
			if backend == "file" {
				// The file store keeps fewer resolved occurrences over a long
				// history than the memory store, so it is read for errors only.
				if strings.Contains(transcript, " error ") {
					t.Fatalf("file reads failed:\n%s", transcript)
				}
				return
			}
			if transcript != reference {
				got, want := strings.Split(transcript, "\n"), strings.Split(reference, "\n")
				for i := range min(len(got), len(want)) {
					if got[i] != want[i] {
						t.Fatalf("%s differs from memory at line %d:\n got %s\nwant %s", backend, i+1, got[i], want[i])
					}
				}
				t.Fatalf("%s lists %d lines, memory %d", backend, len(got), len(want))
			}
		})
	}
}

// utcOccurrencePayload writes an occurrence payload with its times in UTC, so
// stores that return the same instant in another zone compare equal.
func utcOccurrencePayload(t *testing.T, raw json.RawMessage) []byte {
	t.Helper()
	var issue EvidenceIssue
	if len(raw) == 0 || json.Unmarshal(raw, &issue) != nil || issue.OccurrenceID == "" {
		return raw
	}
	issue.FirstSeen, issue.LastSeen = issue.FirstSeen.UTC(), issue.LastSeen.UTC()
	if issue.ClosedAt != nil {
		closed := issue.ClosedAt.UTC()
		issue.ClosedAt = &closed
	}
	out, err := json.Marshal(issue)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// renamedClusters reads the clusters a run seeded under its own suffix.
type renamedClusters struct {
	EvidenceStore
	suffix string
}

func (r *renamedClusters) QueryEvidence(ctx context.Context, scope EvidenceScope, q EvidenceQuery) (EvidencePage, error) {
	scope.ClusterID += r.suffix
	return r.EvidenceStore.QueryEvidence(ctx, scope, q)
}

// subjectPageTranscript writes the pages of a subjects read as a caller sees
// them, one block per subject.
func subjectPageTranscript(t *testing.T, pages []EvidencePage, subjects []EvidenceSubject) string {
	t.Helper()
	var out strings.Builder
	for i, page := range pages {
		fmt.Fprintf(&out, "subject %s/%s next=%v items=%d\n", subjects[i].RootUID, subjects[i].Namespace, page.Next != "", len(page.Items))
		for _, item := range page.Items {
			if item.Type == "coverage_gap" {
				fmt.Fprintf(&out, " %s %s %s\n", item.ID, item.Type, item.At.UTC().Format(time.RFC3339Nano))
				continue
			}
			fmt.Fprintf(&out, " %s %s %s %s %x\n", item.ID, item.Type, item.At.UTC().Format(time.RFC3339Nano), item.ObservedAt.UTC().Format(time.RFC3339Nano), sha256.Sum256(utcOccurrencePayload(t, item.Payload)))
		}
	}
	return out.String()
}

// Reading the open occurrences of many roots at once answers, for every root,
// the page its own read answers: the same window, the same per-root limit and
// the same denied rows, on every backend, and the same as the plain in-memory
// reads.
func TestOccurrenceSubjectPagesMatchOneReadPerRoot(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	start := now.AddDate(0, -13, 0)
	last := start.AddDate(0, 12, 9)
	subjects := []EvidenceSubject{{RootUID: "root-0", Namespace: "ns-0"}, {RootUID: "root-1", Namespace: "ns-1"}, {RootUID: "root-2"}, {RootUID: "root-0"}, {RootUID: "root-9", Namespace: "ns-0"}, {RootUID: "root-2", Namespace: "ns-0"}}
	windows := [][2]time.Time{{last.Add(-24 * time.Hour), last}, {start.Add(-time.Hour), now}, {start.AddDate(0, 4, 0), start.AddDate(0, 8, 0)}}
	authorities := []func(*EvidenceQuery){
		func(*EvidenceQuery) {},
		func(q *EvidenceQuery) { q.EnforceAuthority, q.AllowedKinds = true, []string{"Pod"} },
		func(q *EvidenceQuery) {
			q.EnforceAuthority = true
			q.Authorities = map[string]EvidenceAuthority{"Pod": {State: "allowed", ProvedAt: now}, "Deployment@ns-0": {State: "allowed", ProvedAt: now}, "Deployment": {State: "denied", ProvedAt: now.Add(-time.Hour)}}
		},
	}
	reference := map[string]string{}
	for _, backend := range []string{"memory", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			provider := occurrenceTestProvider(t, backend)
			store := provider.(EvidenceStore)
			suffix := ""
			if backend == "postgres" {
				suffix = fmt.Sprintf("-%d", now.UnixNano())
				if _, ok := store.(EvidenceSubjectReader); !ok {
					t.Fatal("the Postgres store does not read subjects together")
				}
			}
			seedOccurrenceHistory(t, store, "east"+suffix, start)
			scope := EvidenceScope{OrgID: "ent", ReadOrgs: []string{"ent", "default"}, ClusterID: "east" + suffix, ClusterUID: "physical"}
			for w, window := range windows {
				for a, authority := range authorities {
					for _, limit := range []int{2, 100} {
						name := fmt.Sprintf("window=%d authority=%d limit=%d", w, a, limit)
						q := EvidenceQuery{Type: "occurrence", Since: window[0], Until: window[1], Limit: limit, ItemsOnly: true}
						authority(&q)
						together, err := QueryEvidenceSubjects(t.Context(), store, scope, q, subjects)
						if err != nil || len(together) != len(subjects) {
							t.Fatalf("%s: subjects read %d pages, %v", name, len(together), err)
						}
						alone := make([]EvidencePage, len(subjects))
						for i, subject := range subjects {
							one := q
							one.RootUID, one.Namespace = subject.RootUID, subject.Namespace
							if alone[i], err = store.QueryEvidence(t.Context(), scope, one); err != nil {
								t.Fatalf("%s: one read: %v", name, err)
							}
						}
						for i := range subjects {
							if together[i].Next != alone[i].Next {
								t.Fatalf("%s: subject %d links to %q together and %q alone", name, i, together[i].Next, alone[i].Next)
							}
						}
						got, want := subjectPageTranscript(t, together, subjects), subjectPageTranscript(t, alone, subjects)
						if got != want {
							t.Fatalf("%s: read together:\n%s\nread alone:\n%s", name, got, want)
						}
						if backend == "memory" {
							reference[name] = want
						} else if want != reference[name] {
							t.Fatalf("%s: Postgres reads:\n%s\nmemory reads:\n%s", name, want, reference[name])
						}
					}
				}
			}
			all := ""
			for _, transcript := range reference {
				all += transcript
			}
			if !strings.Contains(all, " coverage_gap ") || !strings.Contains(all, " issue_") {
				t.Fatal("the reads showed no denied row or no occurrence")
			}
		})
	}
}
