package issues

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// racingStore runs race before every issue write, so a test lands another
// writer's change between the action's read and its write.
type racingStore struct {
	storage.Provider
	storage.AWSIssueStore
	race func(ctx context.Context, scope tenancy.OrgScope, batch storage.AWSIssueBatch)
}

func (store racingStore) WriteAWSIssues(ctx context.Context, scope tenancy.OrgScope, batch storage.AWSIssueBatch) error {
	if store.race != nil {
		store.race(ctx, scope, batch)
	}
	return store.AWSIssueStore.WriteAWSIssues(ctx, scope, batch)
}

// bump writes an issue again unchanged at the next version, as an
// evaluation that refreshed a volatile fact does.
func bump(t *testing.T, ctx context.Context, store storage.AWSIssueStore, scope tenancy.OrgScope, id string) {
	t.Helper()
	records, err := store.AWSIssues(ctx, scope, storage.AWSIssueQuery{IDs: []string{id}})
	if err != nil || len(records) != 1 {
		t.Fatalf("bump read: %+v, %v", records, err)
	}
	record := records[0]
	read, org := record.Version, record.Org
	record.Version++
	if err := store.WriteAWSIssues(ctx, scope, storage.AWSIssueBatch{AccountID: record.AccountID,
		Writes: []storage.AWSIssueWrite{{Issue: record, Read: read, ReadOrg: org}}}); err != nil {
		t.Fatalf("bump write: %v", err)
	}
}

// Bulk reject gives every distinct ID its own result: rejected,
// already_rejected, not_found or conflict. Duplicates are answered once,
// unknown and malformed IDs are not found, an issue another user rejected
// mid-call keeps that user's reason, one an evaluation touched mid-call is
// still rejected without losing that write, and one that keeps changing is
// a conflict that writes nothing. A repeat writes nothing.
func TestBulkRejectResultPerID(t *testing.T) {
	for _, backend := range flowBackends(t) {
		t.Run(backend.name, func(t *testing.T) {
			ctx := context.Background()
			rule := bucketRule()
			source := newScriptedSource(flowAccount)
			source.read(flowAccount, "s3", "eu-west-1", ReadOK)
			scope := newScope("bulk")
			f := newFlow(t, backend.provider, scope, source, nil, rule)
			owner := func(name string) string { return "arn:aws:s3:::bulk-" + name }
			names := []string{"plain", "rejected", "touched", "taken", "churning", "resolved"}
			var findings []Finding
			for _, name := range names {
				findings = append(findings, publicFinding(owner(name), "eu-west-1", SeverityHigh, "*", "1"))
			}
			rule.set(findings...)
			f.evaluate()
			// "resolved" is gone in a complete read.
			f.clock.Add(time.Minute)
			rule.set(findings[:5]...)
			f.evaluate()
			id := map[string]string{}
			for _, name := range names {
				id[name] = IssueID(rule.id, owner(name))
			}
			if _, err := f.actions.Reject(ctx, id["rejected"], "bo@example.com", "rejected before the bulk call", ""); err != nil {
				t.Fatal(err)
			}

			raw, _ := storage.AWSIssuesOf(backend.provider)
			other, _ := NewActions(backend.provider, scope, f.clock.Now)
			raced := map[string]int{}
			racing := racingStore{Provider: backend.provider, AWSIssueStore: raw, race: func(ctx context.Context, scope tenancy.OrgScope, batch storage.AWSIssueBatch) {
				target := batch.Writes[0].Issue.ID
				raced[target]++
				switch {
				case target == id["touched"] && raced[target] == 1:
					if _, err := other.Acknowledge(ctx, target, "cy@example.com"); err != nil {
						t.Errorf("concurrent acknowledge: %v", err)
					}
				case target == id["taken"] && raced[target] == 1:
					if _, err := other.Reject(ctx, target, "cy@example.com", "cy rejected it first", ""); err != nil {
						t.Errorf("concurrent reject: %v", err)
					}
				case target == id["churning"]:
					bump(t, ctx, raw, scope, target)
				}
			}}
			bulk, err := NewActions(racing, scope, f.clock.Now)
			if err != nil {
				t.Fatal(err)
			}
			unknown := IssueID(rule.id, owner("never-seen"))
			request := []string{id["plain"], unknown, id["plain"], id["rejected"], id["touched"], id["taken"], id["churning"], id["resolved"], "not-an-issue-id", ""}
			want := []BulkResult{
				{id["plain"], BulkRejected}, {unknown, BulkNotFound}, {id["rejected"], BulkAlreadyRejected},
				{id["touched"], BulkRejected}, {id["taken"], BulkAlreadyRejected}, {id["churning"], BulkConflict},
				{id["resolved"], BulkConflict}, {"not-an-issue-id", BulkNotFound}, {"", BulkNotFound},
			}
			f.clock.Add(time.Minute)
			got, err := bulk.RejectMany(ctx, request, "ana@example.com", "public on purpose: static sites", "")
			if err != nil {
				t.Fatal(err)
			}
			assertBulk(t, got, want)
			if raced[id["touched"]] != 2 || raced[id["taken"]] != 1 || raced[id["churning"]] != maxActionRetries {
				t.Fatalf("the mid-call writes did not land as planned: %v", raced)
			}

			if issue, events := f.get(id["plain"]); issue.State != StateRejected || countKind(events, EventRejected) != 1 {
				t.Fatalf("duplicate ID rejected twice: %+v", events)
			}
			if issue, events := f.get(id["touched"]); issue.State != StateRejected || countKind(events, EventAcknowledged) != 1 ||
				issue.Rejection.By != "ana@example.com" {
				t.Fatalf("touched mid-call: %+v, %+v", issue, events)
			}
			if issue, _ := f.get(id["taken"]); issue.Rejection == nil || issue.Rejection.By != "cy@example.com" || issue.Rejection.Reason != "cy rejected it first" {
				t.Fatalf("the other user's rejection was overwritten: %+v", issue.Rejection)
			}
			if issue, events := f.get(id["churning"]); issue.State != StateOpen || issue.Rejection != nil || countKind(events, EventRejected) != 0 {
				t.Fatalf("a conflicting ID was rejected: %+v", issue)
			}
			if issue, _ := f.get(id["resolved"]); issue.State != StateResolved || issue.Rejection != nil {
				t.Fatalf("a resolved issue was rejected: %+v", issue)
			}

			// A repeat is a no-op: same answers, no event, no version.
			before := snapshotIssues(t, f, id)
			f.clock.Add(time.Minute)
			again, err := bulk.RejectMany(ctx, request, "ana@example.com", "public on purpose: static sites", "")
			if err != nil {
				t.Fatal(err)
			}
			want[0].Result, want[3].Result = BulkAlreadyRejected, BulkAlreadyRejected
			assertBulk(t, again, want)
			after := snapshotIssues(t, f, id)
			for name, was := range before {
				if name == "churning" {
					continue // the racer bumps it on purpose
				}
				if now := after[name]; now != was {
					t.Fatalf("the repeat wrote %s: %+v -> %+v", name, was, now)
				}
			}
		})
	}
}

// The ID list is bounded: none is an empty answer, 200 is served, 201 is
// refused before anything is written, and a bad reason rejects nothing.
func TestBulkRejectBounds(t *testing.T) {
	ctx := context.Background()
	rule := bucketRule()
	source := newScriptedSource(flowAccount)
	source.read(flowAccount, "s3", "eu-west-1", ReadOK)
	f := newFlow(t, storage.NewMemory(), newScope("bulk-bounds"), source, nil, rule)
	rule.set(publicFinding(docsBucket, "eu-west-1", SeverityHigh, "*", "1"))
	f.evaluate()
	real := IssueID(rule.id, docsBucket)

	none, err := f.actions.RejectMany(ctx, nil, "ana@example.com", "public on purpose", "")
	if err != nil || none == nil || len(none) != 0 {
		t.Fatalf("no IDs = %#v, %v; want an empty list", none, err)
	}
	ids := []string{real}
	for index := 1; len(ids) < MaxBulkReject+1; index++ {
		ids = append(ids, IssueID(rule.id, "arn:aws:s3:::unknown-"+strconv.Itoa(index)))
	}
	if results, err := f.actions.RejectMany(ctx, ids, "ana@example.com", "public on purpose", ""); !errors.Is(err, ErrTooMany) || results != nil {
		t.Fatalf("201 IDs = %d results, %v; want refused", len(results), err)
	}
	if issue, _ := f.get(real); issue.State != StateOpen {
		t.Fatalf("a refused bulk call wrote: %+v", issue)
	}
	for _, bad := range []string{"no", "   ", strings.Repeat("x", 501)} {
		if _, err := f.actions.RejectMany(ctx, ids[:2], "ana@example.com", bad, ""); !errors.Is(err, ErrInvalidReason) {
			t.Fatalf("reason %q = %v", bad, err)
		}
	}
	if _, err := f.actions.RejectMany(ctx, ids[:2], "ana@example.com", "public on purpose", day(start)); !errors.Is(err, ErrInvalidReviewDate) {
		t.Fatalf("a review date of today = %v", err)
	}
	if issue, _ := f.get(real); issue.State != StateOpen {
		t.Fatalf("a refused bulk call wrote: %+v", issue)
	}
	results, err := f.actions.RejectMany(ctx, ids[:MaxBulkReject], "ana@example.com", "public on purpose", "")
	if err != nil || len(results) != MaxBulkReject || results[0] != (BulkResult{real, BulkRejected}) {
		t.Fatalf("200 IDs = %d results, %v", len(results), err)
	}
	for _, result := range results[1:] {
		if result.Result != BulkNotFound {
			t.Fatalf("unknown ID = %+v", result)
		}
	}
}

func assertBulk(t *testing.T, got, want []BulkResult) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("results = %+v, want %+v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("result %d = %+v, want %+v", index, got[index], want[index])
		}
	}
}

func countKind(events []Event, kind EventKind) int {
	n := 0
	for _, event := range events {
		if event.Kind == kind {
			n++
		}
	}
	return n
}

type issueMark struct {
	state   State
	version int64
	events  int
}

func snapshotIssues(t *testing.T, f *flow, ids map[string]string) map[string]issueMark {
	t.Helper()
	out := map[string]issueMark{}
	for name, id := range ids {
		issue, events := f.get(id)
		out[name] = issueMark{issue.State, issue.Version, len(events)}
	}
	return out
}
