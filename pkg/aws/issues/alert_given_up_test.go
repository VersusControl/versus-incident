package issues

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// givingUpStore refuses, while refuse is set, the write that gives an
// alert up.
type givingUpStore struct {
	storage.Provider
	storage.AWSIssueStore
	refuse  atomic.Bool
	refused atomic.Int32
}

func (store *givingUpStore) WriteAWSIssues(ctx context.Context, scope tenancy.OrgScope, batch storage.AWSIssueBatch) error {
	for _, write := range batch.Writes {
		for _, event := range write.Events {
			if event.Kind == storage.AWSIssueEventAlertGivenUp && store.refuse.Load() {
				store.refused.Add(1)
				return errors.New("storage unavailable")
			}
		}
	}
	return store.AWSIssueStore.WriteAWSIssues(ctx, scope, batch)
}

// An alert whose sends fail for a day is given up: the issue then holds
// exactly one alert_given_up event, naming the last channel error, and no
// pending alert. A write that fails to record the give-up leaves the alert
// pending, and the next pump records it once.
func TestGivenUpAlertIsRecordedOnTheIssue(t *testing.T) {
	for _, backend := range flowBackends(t) {
		t.Run(backend.name, func(t *testing.T) {
			ctx := context.Background()
			scope := newScope("alert-given-up")
			raw, _ := storage.AWSIssuesOf(backend.provider)
			store := &givingUpStore{Provider: backend.provider, AWSIssueStore: raw}
			store.refuse.Store(true)
			source := newFakeSource(testAccount)
			source.public(testAccount, "1", "docs-site")
			now := &clock{now: start}
			alerter, err := NewAlerter(store, scope, newFlakySink(-1), WithAlertClock(now.Now), WithAlertLogger(func(string, ...any) {}))
			if err != nil {
				t.Fatal(err)
			}
			evaluator, err := NewEvaluator(store, scope, source, WithRules(severityRules()), WithClock(now.Now),
				WithAfterEvaluate(func(ctx context.Context, report Report) { _ = alerter.Pump(ctx, report) }))
			if err != nil {
				t.Fatal(err)
			}
			for store.refused.Load() == 0 {
				if now.Now().Sub(start) > 2*GiveUpAfter {
					t.Fatal("the alert was never given up")
				}
				if _, err := evaluator.Evaluate(ctx); err != nil {
					t.Fatal(err)
				}
				now.Add(15 * time.Minute)
			}
			id := IssueID(testRuleID, "arn:aws:s3:::docs-site")
			actions, _ := NewActions(store, scope, now.Now)
			issue, events, err := actions.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if issue.Alerted == nil || len(issue.Alerted.Pending) != 1 || givenUpEvents(events) != 0 {
				t.Fatalf("after a failed give-up write: pending %+v, given-up events %d; want the alert still pending", issue.Alerted, givenUpEvents(events))
			}

			store.refuse.Store(false)
			for range 3 {
				if _, err := evaluator.Evaluate(ctx); err != nil {
					t.Fatal(err)
				}
				now.Add(15 * time.Minute)
			}
			issue, events, err = actions.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if issue.Alerted == nil || len(issue.Alerted.Pending) != 0 {
				t.Fatalf("pending after giving up = %+v", issue.Alerted)
			}
			if givenUpEvents(events) != 1 {
				t.Fatalf("events = %+v; want exactly one alert_given_up", events)
			}
			for _, event := range events {
				if event.Kind == EventAlertGivenUp && !strings.Contains(event.Detail, "slack: connection refused") {
					t.Fatalf("given-up event detail = %q; want the last channel error", event.Detail)
				}
			}
		})
	}
}

func givenUpEvents(events []Event) int {
	n := 0
	for _, event := range events {
		if event.Kind == EventAlertGivenUp {
			n++
		}
	}
	return n
}
