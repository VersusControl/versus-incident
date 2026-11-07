package storage

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Worst-case keyset paging checks over every evidence backend: memory and
// file share the local index, Postgres has its own read path. Postgres runs
// only with TEST_POSTGRES_DSN and then in its own throwaway qa_ database.

type pagingBackend struct {
	name     string
	provider Provider
	store    EvidenceStore
	postgres *postgresProvider
}

// pagingOwnDatabase creates a fresh qa_ database for this test alone and drops
// it when the test ends. It refuses any database it did not just create.
func pagingOwnDatabase(t *testing.T) string {
	t.Helper()
	server := os.Getenv("TEST_POSTGRES_DSN")
	target, err := url.Parse(server)
	if err != nil {
		t.Fatal("TEST_POSTGRES_DSN is not a URL")
	}
	name := fmt.Sprintf("qa_paging_%d", time.Now().UnixNano())
	admin, err := pgx.Connect(t.Context(), server)
	if err != nil {
		t.Fatalf("connect to the test server: %T", err)
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
			t.Errorf("drop %s: %T", name, err)
			return
		}
		defer conn.Close(ctx)
		if _, err = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})
	target.Path = "/" + name
	own := target.String()
	conn, err := pgx.Connect(t.Context(), own)
	if err != nil {
		t.Fatalf("connect to %s: %T", name, err)
	}
	defer conn.Close(context.Background())
	var current string
	var tables int
	if err = conn.QueryRow(t.Context(), `SELECT current_database(),(SELECT COUNT(*) FROM pg_tables WHERE schemaname='public')`).Scan(&current, &tables); err != nil {
		t.Fatal(err)
	}
	if current != name || !strings.HasPrefix(current, "qa_") || tables != 0 {
		t.Fatalf("refusing %q: not the fresh qa_ database %q", current, name)
	}
	return own
}

func pagingBackends(t *testing.T, retention string) []pagingBackend {
	t.Helper()
	memory := NewMemory()
	t.Cleanup(func() { _ = memory.Close() })
	file, err := NewFile(FileOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	backends := []pagingBackend{{name: "memory", provider: memory, store: memory.(EvidenceStore)}, {name: "file", provider: file, store: file.(EvidenceStore)}}
	if os.Getenv("TEST_POSTGRES_DSN") == "" {
		t.Log("Postgres variant skipped: TEST_POSTGRES_DSN not set")
		return backends
	}
	dsn := pagingOwnDatabase(t)
	provider, err := NewPostgres(PostgresOptions{DSN: dsn, EvidenceRetention: retention})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	return append(backends, pagingBackend{name: "postgres", provider: provider, store: provider.(EvidenceStore), postgres: provider.(*postgresProvider)})
}

func pagingChange(id, namespace string, at time.Time, observed time.Time, payload string) EvidenceRecord {
	return EvidenceRecord{ID: id, Type: "change", At: at, ObservedAt: observed, Namespace: namespace, Name: "root", Kind: "Deployment", AuthorizationKind: "Deployment",
		UID: "root", RootUID: "root", Epoch: "paging", Payload: json.RawMessage(fmt.Sprintf(`{"value":%q}`, payload))}
}

func pagingIssue(key, namespace string, seen time.Time) EvidenceIssue {
	return EvidenceIssue{Key: key, Namespace: namespace, Kind: "Pod", UID: key, Rule: "pod.crash_loop", AuthorizationKind: "Pod", LastSeen: seen,
		Payload: json.RawMessage(`{"rule":"pod.crash_loop","severity":"warning"}`)}
}

func pagingCommit(t *testing.T, store EvidenceStore, scope EvidenceScope, id string, at time.Time, records []EvidenceRecord, issues []EvidenceIssue) {
	t.Helper()
	checkpoint, err := store.EvidenceCheckpoint(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	batch := EvidenceBatch{ID: id, Expected: checkpoint.ID, Generation: checkpoint.Generation, At: at, Epoch: "paging", Records: records, Issues: issues}
	if err := store.CommitEvidence(t.Context(), scope, batch); err != nil {
		t.Fatalf("commit %s: %v", id, err)
	}
}

func pagingScope(name string) EvidenceScope {
	return EvidenceScope{OrgID: "default", ClusterID: fmt.Sprintf("%s-%d", name, time.Now().UnixNano()), ClusterUID: "physical"}
}

// pagingWalk reads every page and calls between(page) before each next read.
func pagingWalk(t *testing.T, store EvidenceStore, scope EvidenceScope, query EvidenceQuery, between func(EvidencePage)) ([]EvidenceRecord, []int) {
	t.Helper()
	items, sizes := []EvidenceRecord{}, []int{}
	for pages := 0; ; pages++ {
		if pages > 5000 {
			t.Fatal("the walk never ended")
		}
		page, err := store.QueryEvidence(t.Context(), scope, query)
		if err != nil {
			t.Fatalf("page %d: %v", pages+1, err)
		}
		items = append(items, page.Items...)
		sizes = append(sizes, len(page.Items))
		if page.Next == "" {
			return items, sizes
		}
		if between != nil {
			between(page)
		}
		query.Cursor = page.Next
	}
}

func pagingIDs(records []EvidenceRecord) []string {
	ids := make([]string, len(records))
	for i, record := range records {
		ids[i] = record.ID
	}
	return ids
}

// A keyset walk over many rows with identical timestamps straddling every
// page boundary returns each row present at page 1 exactly once, in a
// deterministic (time, id) order, while a writer keeps inserting newer rows,
// rows at the cursor's own timestamp on both sides of its id, and new
// versions of rows already listed.
func TestHistoryKeysetWalkUnderConcurrentWritesAndTimestampTies(t *testing.T) {
	if testing.Short() {
		t.Skip("commits a batch between every page on every backend")
	}
	for _, backend := range pagingBackends(t, "0") {
		t.Run(backend.name+"/journal", func(t *testing.T) {
			scope := pagingScope("ties")
			now := time.Now().UTC().Truncate(time.Microsecond)
			stamps := []time.Time{now.Add(-50 * time.Minute), now.Add(-30 * time.Minute), now.Add(-10 * time.Minute)}
			seed := []EvidenceRecord{}
			for i := 0; i < 120; i++ {
				// Ids are not in time order, so the tie-break, not insertion,
				// decides the order.
				id := fmt.Sprintf("c-%03d", (i*37)%120)
				seed = append(seed, pagingChange(id, "shop", stamps[i%3], now, "v1"))
			}
			pagingCommit(t, backend.store, scope, "seed", now, seed, nil)
			query := EvidenceQuery{Type: "change", Since: now.Add(-time.Hour), Until: now.Add(time.Minute), Limit: 7}
			want := append([]EvidenceRecord(nil), seed...)
			sort.Slice(want, func(i, j int) bool {
				return want[i].At.After(want[j].At) || want[i].At.Equal(want[j].At) && want[i].ID > want[j].ID
			})
			quiet, _ := pagingWalk(t, backend.store, scope, query, nil)
			if strings.Join(pagingIDs(quiet), ",") != strings.Join(pagingIDs(want), ",") {
				t.Fatalf("a quiet walk is not in (time desc, id desc) order:\n got  %v\n want %v", pagingIDs(quiet), pagingIDs(want))
			}
			writes := 0
			busy, _ := pagingWalk(t, backend.store, scope, query, func(page EvidencePage) {
				last := page.Items[len(page.Items)-1]
				writes++
				at := now.Add(time.Duration(writes) * time.Millisecond)
				pagingCommit(t, backend.store, scope, fmt.Sprintf("writer-%d", writes), at, []EvidenceRecord{
					pagingChange(fmt.Sprintf("a-%03d", writes), "shop", last.At, at, "tie-low"),
					pagingChange(fmt.Sprintf("z-%03d", writes), "shop", last.At, at, "tie-high"),
					pagingChange(fmt.Sprintf("n-%03d", writes), "shop", now.Add(-time.Second), at, "newer"),
					pagingChange(want[(writes*11)%len(want)].ID, "shop", want[(writes*11)%len(want)].At, at, fmt.Sprintf("v%d", writes+1)),
				}, nil)
			})
			if writes == 0 {
				t.Fatal("the walk had a single page")
			}
			seen := map[string]int{}
			for _, record := range busy {
				seen[record.ID]++
			}
			for _, record := range want {
				if seen[record.ID] != 1 {
					t.Fatalf("pre-existing row %s was returned %d times during a busy walk", record.ID, seen[record.ID])
				}
			}
			if len(busy) != len(want) {
				t.Fatalf("a busy walk returned %d rows, want the %d present at page 1 (ids %v)", len(busy), len(want), pagingIDs(busy))
			}
		})
		t.Run(backend.name+"/occurrence", func(t *testing.T) {
			scope := pagingScope("occurrence-ties")
			now := time.Now().UTC().Truncate(time.Microsecond)
			first, second := now.Add(-20*time.Minute), now.Add(-10*time.Minute)
			issues := []EvidenceIssue{}
			for i := 0; i < 30; i++ {
				issues = append(issues, pagingIssue(fmt.Sprintf("o-%02d", (i*7)%30), "shop", first))
			}
			pagingCommit(t, backend.store, scope, "seed-1", first, nil, issues)
			for i := 30; i < 60; i++ {
				issues = append(issues, pagingIssue(fmt.Sprintf("o-%02d", i), "shop", second))
			}
			for i := range issues {
				issues[i].LastSeen = second
			}
			pagingCommit(t, backend.store, scope, "seed-2", second, nil, issues)
			query := EvidenceQuery{Type: "occurrence", Since: now.Add(-time.Hour), Until: now, Limit: 4}
			quiet, _ := pagingWalk(t, backend.store, scope, query, nil)
			if len(quiet) != 60 {
				t.Fatalf("a quiet walk returned %d occurrences, want 60", len(quiet))
			}
			writes := 0
			busy, _ := pagingWalk(t, backend.store, scope, query, func(EvidencePage) {
				writes++
				at := second.Add(time.Duration(writes) * time.Second)
				next := append([]EvidenceIssue(nil), issues...)
				next = append(next, pagingIssue(fmt.Sprintf("late-%03d", writes), "shop", at))
				for i := range next {
					next[i].LastSeen = at
				}
				pagingCommit(t, backend.store, scope, fmt.Sprintf("occurrence-writer-%d", writes), at, nil, next)
			})
			if strings.Join(pagingIDs(busy), ",") != strings.Join(pagingIDs(quiet), ",") {
				t.Fatalf("a busy occurrence walk differs from the quiet one:\n busy  %d %v\n quiet %d %v", len(busy), pagingIDs(busy), len(quiet), pagingIDs(quiet))
			}
		})
	}
}

// Count cap boundaries: 0, 1,000, 1,001 and 1,002 rows, and a filter that
// lands exactly on 1,000. The first page counts at most 1,001 rows and marks
// the count capped above 1,000; the walk has no trailing empty page.
func TestHistoryCountCapBoundaries(t *testing.T) {
	if testing.Short() {
		t.Skip("commits up to 1,002 rows per case on every backend")
	}
	for _, backend := range pagingBackends(t, "0") {
		for _, rows := range []int{0, 1000, 1001, 1002} {
			t.Run(fmt.Sprintf("%s/%d", backend.name, rows), func(t *testing.T) {
				scope := pagingScope(fmt.Sprintf("cap-%d", rows))
				now := time.Now().UTC().Truncate(time.Microsecond)
				records := []EvidenceRecord{}
				for i := 0; i < rows; i++ {
					namespace := "a"
					if i >= 1000 {
						namespace = "b"
					}
					records = append(records, pagingChange(fmt.Sprintf("cap-%04d", i), namespace, now.Add(-time.Duration(i)*time.Millisecond), now, "x"))
				}
				pagingCommit(t, backend.store, scope, "cap", now, records, nil)
				check := func(query EvidenceQuery, matching int) {
					t.Helper()
					page, err := backend.store.QueryEvidence(t.Context(), scope, query)
					if err != nil {
						t.Fatal(err)
					}
					if page.Total != min(matching, 1001) || page.CountCapped != (matching > 1000) || page.Items == nil {
						t.Fatalf("namespace %q, %d rows: total=%d capped=%t items-nil=%t", query.Namespace, matching, page.Total, page.CountCapped, page.Items == nil)
					}
					items, sizes := pagingWalk(t, backend.store, scope, query, nil)
					wantPages := max(1, (matching+query.Limit-1)/query.Limit)
					if len(items) != matching || len(sizes) != wantPages {
						t.Fatalf("namespace %q, %d rows: walked %d rows over %d pages %v, want %d pages", query.Namespace, matching, len(items), len(sizes), sizes, wantPages)
					}
				}
				// Product reads always enforce authority, which keeps every
				// backend on the bounded first-page count.
				base := EvidenceQuery{Type: "change", Since: now.Add(-time.Hour), Until: now, Limit: 200, EnforceAuthority: true, AllowedKinds: []string{"Deployment"}}
				check(base, rows)
				// Without authority, an unfiltered Postgres read over all retained
				// history uses the exact per-type counters instead.
				exact, err := backend.store.QueryEvidence(t.Context(), scope, EvidenceQuery{Type: "change", Since: now.Add(-time.Hour), Until: now, Limit: 200})
				if err != nil {
					t.Fatal(err)
				}
				if backend.postgres != nil && (exact.Total != rows || exact.CountCapped) || backend.postgres == nil && (exact.Total != min(rows, 1001) || exact.CountCapped != (rows > 1000)) {
					t.Fatalf("unenforced unfiltered count: total=%d capped=%t for %d rows", exact.Total, exact.CountCapped, rows)
				}
				filtered := base
				filtered.Namespace = "a"
				check(filtered, min(rows, 1000))
			})
		}
		t.Run(backend.name+"/occurrences-1001", func(t *testing.T) {
			scope := pagingScope("occurrence-cap")
			now := time.Now().UTC().Truncate(time.Microsecond)
			issues := []EvidenceIssue{}
			for i := 0; i < 1001; i++ {
				issues = append(issues, pagingIssue(fmt.Sprintf("cap-%04d", i), "shop", now))
			}
			pagingCommit(t, backend.store, scope, "occurrence-cap", now, nil, issues)
			page, err := backend.store.QueryEvidence(t.Context(), scope, EvidenceQuery{Type: "occurrence", Since: now.Add(-time.Hour), Until: now, Limit: 200})
			if err != nil || page.Total != 1001 || !page.CountCapped || len(page.Items) != 200 || page.Next == "" {
				t.Fatalf("1,001 occurrences: total=%d capped=%t items=%d next=%t err=%v", page.Total, page.CountCapped, len(page.Items), page.Next != "", err)
			}
		})
	}
}

// Hostile limits and cursors on every backend: limits are normalized, and a
// malformed, tampered or reused cursor is rejected without rows or a panic.
func TestHistoryHostileCursorsOnEveryBackend(t *testing.T) {
	for _, backend := range pagingBackends(t, "0") {
		t.Run(backend.name, func(t *testing.T) {
			scope := pagingScope("hostile")
			other := pagingScope("hostile-other")
			now := time.Now().UTC().Truncate(time.Microsecond)
			records := []EvidenceRecord{}
			for i := 0; i < 700; i++ {
				records = append(records, pagingChange(fmt.Sprintf("h-%04d", i), []string{"shop", "ops"}[i%2], now.Add(-time.Duration(i)*time.Millisecond), now, "x"))
			}
			pagingCommit(t, backend.store, scope, "hostile", now, records, nil)
			pagingCommit(t, backend.store, other, "hostile-other", now, records, nil)
			base := EvidenceQuery{Type: "change", Namespace: "shop", Since: now.Add(-time.Hour), Until: now}
			for limit, want := range map[int]int{0: 100, -1: 100, -1 << 40: 100, 1 << 40: 350, 501: 350} {
				query := base
				query.Limit = limit
				page, err := backend.store.QueryEvidence(t.Context(), scope, query)
				if err != nil || len(page.Items) != want {
					t.Fatalf("limit %d: items=%d err=%v", limit, len(page.Items), err)
				}
			}
			query := base
			query.Limit = 5
			first, err := backend.store.QueryEvidence(t.Context(), scope, query)
			if err != nil || first.Next == "" {
				t.Fatal(err)
			}
			raw, err := base64.RawURLEncoding.DecodeString(first.Next)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err = json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			rewrite := func(key string, value any) string {
				copied := map[string]any{}
				for name, field := range fields {
					copied[name] = field
				}
				copied[key] = value
				encoded, _ := json.Marshal(copied)
				return base64.RawURLEncoding.EncodeToString(encoded)
			}
			flipped := []byte(first.Next)
			flipped[len(flipped)/3] ^= 0x02
			cursors := map[string]string{
				"flipped": string(flipped), "truncated": first.Next[:len(first.Next)-5], "garbage": "!!!!", "not json": base64.RawURLEncoding.EncodeToString([]byte("[1,2]")),
				"binding": rewrite("Binding", "x"), "scope": rewrite("CaptureScope", "x"), "no id": rewrite("ID", ""), "no time": rewrite("At", time.Time{}),
				"short pins": rewrite("Highs", []any{}), "huge pin": rewrite("Highs", []any{json.Number("18446744073709551615")}), "generation": rewrite("Generations", []any{12345}),
				"prune": rewrite("PruneRevisions", []any{77}), "oversized": strings.Repeat("A", 4097),
			}
			for name, cursor := range cursors {
				query := base
				query.Limit, query.Cursor = 5, cursor
				page, err := backend.store.QueryEvidence(t.Context(), scope, query)
				if err == nil {
					t.Fatalf("%s cursor served %d rows", name, len(page.Items))
				}
				if !errors.Is(err, ErrInvalidEvidence) && !errors.Is(err, ErrEvidenceConflict) {
					t.Fatalf("%s cursor answered %v, want invalid or conflict", name, err)
				}
			}
			reuse := map[string]func(*EvidenceQuery){
				"namespace": func(q *EvidenceQuery) { q.Namespace = "ops" },
				"kind":      func(q *EvidenceQuery) { q.Kind = "Pod" },
				"type":      func(q *EvidenceQuery) { q.Type = "crash" },
				"window":    func(q *EvidenceQuery) { q.Since = q.Since.Add(time.Second) },
				"gaps":      func(q *EvidenceQuery) { q.WithGaps = true },
				"variant":   func(q *EvidenceQuery) { q.Variant = "critical" },
			}
			for name, change := range reuse {
				query := base
				query.Limit, query.Cursor = 5, first.Next
				change(&query)
				if page, err := backend.store.QueryEvidence(t.Context(), scope, query); !errors.Is(err, ErrInvalidEvidence) {
					t.Fatalf("cursor reused with another %s answered %d rows, err %v", name, len(page.Items), err)
				}
			}
			query.Cursor = first.Next
			if page, err := backend.store.QueryEvidence(t.Context(), other, query); !errors.Is(err, ErrInvalidEvidence) {
				t.Fatalf("cursor reused on another cluster answered %d rows, err %v", len(page.Items), err)
			}
			if page, err := backend.store.QueryEvidence(t.Context(), scope, query); err != nil || len(page.Items) != 5 {
				t.Fatalf("the issued cursor stopped working: items=%d err=%v", len(page.Items), err)
			}
		})
	}
}

// An occurrence page link that pages in another order than its list, or that
// predates links naming their order, restarts the list from its first page; a
// malformed or foreign link is still rejected as invalid.
func TestOccurrenceLinkInAnotherOrderRestartsTheList(t *testing.T) {
	for _, backend := range pagingBackends(t, "0") {
		t.Run(backend.name, func(t *testing.T) {
			scope := pagingScope("ordering")
			now := time.Now().UTC().Truncate(time.Microsecond)
			issues := []EvidenceIssue{}
			for i := 0; i < 6; i++ {
				issues = append(issues, pagingIssue(fmt.Sprintf("ord-%02d", i), "shop", now))
			}
			pagingCommit(t, backend.store, scope, "ordering", now, nil, issues)
			list := EvidenceQuery{Type: "occurrence", Since: now.Add(-time.Hour), Until: now, Limit: 2}
			first, err := backend.store.QueryEvidence(t.Context(), scope, list)
			if err != nil || first.Next == "" || len(first.Items) == 0 {
				t.Fatalf("first occurrence page: next=%t err=%v", first.Next != "", err)
			}
			raw, err := base64.RawURLEncoding.DecodeString(first.Next)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]any
			if err = json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			if fields["Ordering"] != "first_seen" {
				t.Fatalf("the occurrence list issued a link ordered by %v, want first_seen", fields["Ordering"])
			}
			rewrite := func(changes map[string]any, drop ...string) string {
				copied := map[string]any{}
				for name, field := range fields {
					copied[name] = field
				}
				for name, value := range changes {
					copied[name] = value
				}
				for _, name := range drop {
					delete(copied, name)
				}
				encoded, _ := json.Marshal(copied)
				return base64.RawURLEncoding.EncodeToString(encoded)
			}
			expectRestart := func(what string, query EvidenceQuery) {
				t.Helper()
				if page, err := backend.store.QueryEvidence(t.Context(), scope, query); !errors.Is(err, ErrEvidenceCursorExpired) {
					t.Fatalf("%s answered %d rows, err %v, want the list restarted", what, len(page.Items), err)
				}
			}
			expectInvalid := func(what string, query EvidenceQuery) {
				t.Helper()
				if page, err := backend.store.QueryEvidence(t.Context(), scope, query); !errors.Is(err, ErrInvalidEvidence) || errors.Is(err, ErrEvidenceCursorExpired) {
					t.Fatalf("%s answered %d rows, err %v, want invalid and not expired", what, len(page.Items), err)
				}
			}

			query := list
			query.Cursor = rewrite(map[string]any{"Ordering": "last_seen"})
			expectRestart("a last_seen link on the occurrence list", query)
			query.Cursor = rewrite(nil, "Ordering")
			expectRestart("an occurrence list link without an order", query)
			query.Cursor = rewrite(map[string]any{"Ordering": ""})
			expectRestart("an occurrence list link with an empty order", query)

			// The single-occurrence history pages by last_seen on Postgres and
			// by first_seen on the local index; send it a link of the other order.
			history := list
			history.PointInTime, history.OccurrenceID = true, first.Items[0].ID
			_, bound, err := prepareEvidenceQuery(scope, history)
			if err != nil {
				t.Fatal(err)
			}
			other := "last_seen"
			if backend.postgres != nil {
				other = "first_seen"
			}
			query = history
			query.Cursor = rewrite(map[string]any{"Binding": bound.Binding, "Ordering": other})
			expectRestart("a "+other+" link on the occurrence history", query)
			query.Cursor = rewrite(map[string]any{"Binding": bound.Binding}, "Ordering")
			expectRestart("an occurrence history link without an order", query)

			for name, cursor := range map[string]string{
				"garbage": "!!!!", "not json": base64.RawURLEncoding.EncodeToString([]byte("[1,2]")),
				"foreign scope": rewrite(map[string]any{"CaptureScope": "x", "Ordering": "last_seen"}),
				"no id":         rewrite(map[string]any{"ID": ""}, "Ordering"),
				"other list":    rewrite(map[string]any{"Binding": "x"}),
			} {
				query := list
				query.Cursor = cursor
				expectInvalid("a malformed occurrence link ("+name+")", query)
			}
			query = list
			query.Cursor = first.Next
			if page, err := backend.store.QueryEvidence(t.Context(), scope, query); err != nil || len(page.Items) != 2 {
				t.Fatalf("the issued occurrence link stopped working: items=%d err=%v", len(page.Items), err)
			}
		})
	}
}

// A cursor must not outlive the history it pins: after the scope is reset or
// rows are pruned, the next page is refused, never a silent skip.
func TestHistoryCursorIsRefusedAfterResetOrPrune(t *testing.T) {
	if testing.Short() {
		t.Skip("fills file history to its record bound")
	}
	for _, backend := range pagingBackends(t, "720h") {
		t.Run(backend.name+"/reset", func(t *testing.T) {
			scope := pagingScope("reset")
			now := time.Now().UTC().Truncate(time.Microsecond)
			records := []EvidenceRecord{}
			for i := 0; i < 20; i++ {
				records = append(records, pagingChange(fmt.Sprintf("r-%02d", i), "shop", now.Add(-time.Duration(i)*time.Second), now, "x"))
			}
			pagingCommit(t, backend.store, scope, "reset-seed", now, records, nil)
			query := EvidenceQuery{Type: "change", Since: now.Add(-time.Hour), Until: now, Limit: 5}
			page, err := backend.store.QueryEvidence(t.Context(), scope, query)
			if err != nil || page.Next == "" {
				t.Fatal(err)
			}
			if err = backend.store.DeleteEvidence(t.Context(), scope); err != nil {
				t.Fatal(err)
			}
			pagingCommit(t, backend.store, scope, "reset-again", now, records[:3], nil)
			query.Cursor = page.Next
			if next, err := backend.store.QueryEvidence(t.Context(), scope, query); err == nil {
				t.Fatalf("a cursor over deleted history was served %d rows", len(next.Items))
			}
		})
		if backend.name == "file" {
			t.Run("file/record-bound-prune", func(t *testing.T) {
				scope := pagingScope("prune")
				now := time.Now().UTC().Truncate(time.Microsecond)
				records := []EvidenceRecord{}
				for i := 0; i < 4990; i++ {
					records = append(records, pagingChange(fmt.Sprintf("p-%04d", i), "shop", now.Add(-time.Duration(i)*time.Millisecond), now, "x"))
				}
				pagingCommit(t, backend.store, scope, "prune-seed", now, records, nil)
				query := EvidenceQuery{Type: "change", Since: now.Add(-time.Hour), Until: now, Limit: 100}
				page, err := backend.store.QueryEvidence(t.Context(), scope, query)
				if err != nil || page.Next == "" {
					t.Fatal(err)
				}
				more := []EvidenceRecord{}
				for i := 0; i < 20; i++ {
					more = append(more, pagingChange(fmt.Sprintf("q-%02d", i), "shop", now, now.Add(time.Millisecond), "x"))
				}
				pagingCommit(t, backend.store, scope, "prune-more", now.Add(time.Millisecond), more, nil)
				query.Cursor = page.Next
				if _, err := backend.store.QueryEvidence(t.Context(), scope, query); !errors.Is(err, ErrEvidenceCursorExpired) {
					t.Fatalf("a cursor over pruned file history answered %v, want it expired", err)
				}
			})
		}
		if backend.postgres != nil {
			t.Run("postgres/month-drop-journal", func(t *testing.T) {
				p := backend.postgres
				p.maintenanceCancel()
				<-p.maintenanceDone
				settlePartitionCopy(t, p)
				now := time.Now().UTC()
				expired := time.Date(now.Year(), now.Month()-3, 10, 12, 0, 0, 0, time.UTC)
				scope := pagingScope("month-drop")
				records := []EvidenceRecord{}
				for i := 0; i < 20; i++ {
					records = append(records, pagingChange(fmt.Sprintf("m-%02d", i), "shop", expired.Add(-time.Duration(i)*time.Second), expired, "x"))
				}
				// A store with retention never writes into a month retention has
				// passed, so the month is seeded as a deployment that enables
				// retention later holds it: saved while retention was off.
				retention := p.evidenceRetention
				p.evidenceRetention = 0
				pagingCommit(t, p, scope, "month-seed", expired, records, nil)
				p.evidenceRetention = retention
				query := EvidenceQuery{Type: "change", Since: expired.Add(-time.Hour), Until: expired, Limit: 5}
				page, err := p.QueryEvidence(t.Context(), scope, query)
				if err != nil || page.Next == "" {
					t.Fatal(err)
				}
				for i := 0; ; i++ {
					if i == 1000 {
						t.Fatal("retention never settled")
					}
					more, err := p.compactEvidenceBatch(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					if !more {
						break
					}
				}
				query.Cursor = page.Next
				if next, err := p.QueryEvidence(t.Context(), scope, query); !errors.Is(err, ErrEvidenceCursorExpired) {
					t.Fatalf("a change cursor over a dropped month answered %d rows, err %v", len(next.Items), err)
				}
			})
		}
	}
}
