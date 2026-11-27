package issues

import (
	"context"
	"errors"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// replicas are two processes over one backend. On Postgres each has its
// own pool, so session locks and row locks are real; memory and file
// storage serve one process, so both replicas share it. restart opens the
// backend again, as a process that started over.
type replicas struct {
	name     string
	a, b     storage.Provider
	restart  func(t *testing.T) storage.Provider
	postgres bool
}

func replicaBackends(t *testing.T) []replicas {
	t.Helper()
	var out []replicas
	for name, provider := range singleReplicaBackends(t) {
		out = append(out, replicas{name: name, a: provider, b: provider, restart: func(*testing.T) storage.Provider { return provider }})
	}
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Log("TEST_POSTGRES_DSN not set: the Postgres replicas are skipped")
		return out
	}
	open := func(t *testing.T) storage.Provider {
		provider, err := storage.NewPostgres(storage.PostgresOptions{DSN: dsn})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = provider.Close() })
		return provider
	}
	return append(out, replicas{name: "postgres", a: open(t), b: open(t), restart: open, postgres: true})
}

// uniqueOrg keeps each run's rows apart on a shared database.
func uniqueOrg(prefix string) string {
	return prefix + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// Two replicas evaluate while a third stops mid-evaluation (a restart):
// exactly one issue is opened, with one opened event. While one replica
// evaluates, the other skips.
func TestReplicasAndARestartOpenOneIssue(t *testing.T) {
	for _, set := range replicaBackends(t) {
		t.Run(set.name, func(t *testing.T) {
			scope := tenancy.NewOrgScope(uniqueOrg("issues"))
			source := newFakeSource(testAccount)
			source.public(testAccount, "1", "docs-site")
			now := &clock{now: start}

			// The first process dies after it decided and before it wrote.
			dying, _ := NewEvaluator(set.a, scope, source, WithRules(publicBucketRules()), WithClock(now.Now))
			ctx, cancel := context.WithCancel(context.Background())
			dying.beforeWrite = func(context.Context, string) { cancel() }
			if _, err := dying.Evaluate(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("interrupted evaluation = %v", err)
			}

			restarted, _ := NewEvaluator(set.restart(t), scope, source, WithRules(publicBucketRules()), WithClock(now.Now))
			other, _ := NewEvaluator(set.b, scope, source, WithRules(publicBucketRules()), WithClock(now.Now))
			if set.postgres {
				// While the restarted replica holds the lock, the other one
				// skips instead of evaluating beside it.
				var skipped Report
				var skipErr error
				restarted.beforeWrite = func(ctx context.Context, string string) {
					skipped, skipErr = other.Evaluate(ctx)
				}
				if _, err := restarted.Evaluate(context.Background()); err != nil {
					t.Fatal(err)
				}
				if skipErr != nil || !skipped.Skipped {
					t.Fatalf("the second replica evaluated beside the first: %+v, %v", skipped, skipErr)
				}
				restarted.beforeWrite = nil
			}
			var wg sync.WaitGroup
			errs := make([]error, 2)
			for index, evaluator := range []*Evaluator{restarted, other} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, errs[index] = evaluator.Evaluate(context.Background())
				}()
			}
			wg.Wait()
			if errs[0] != nil || errs[1] != nil {
				t.Fatalf("evaluations = %v", errs)
			}
			store := other.Store()
			list, err := store.AWSIssues(context.Background(), scope, storage.AWSIssueQuery{})
			if err != nil || len(list) != 1 {
				t.Fatalf("issues = %+v, %v", list, err)
			}
			if kinds := eventKinds(t, store, scope, list[0].ID); len(kinds) != 1 || kinds[0] != storage.AWSIssueEventOpened {
				t.Fatalf("events = %v", kinds)
			}
		})
	}
}

// Two evaluators that bypass the lock and race to open the same issue
// still open it once: the second write conflicts and reads it again.
func TestRacingEvaluatorsOpenOneIssue(t *testing.T) {
	for _, set := range replicaBackends(t) {
		t.Run(set.name, func(t *testing.T) {
			scope := tenancy.NewOrgScope(uniqueOrg("race"))
			source := newFakeSource(testAccount)
			source.public(testAccount, "1", "docs-site")
			now := &clock{now: start}
			first, _ := NewEvaluator(set.a, scope, source, WithRules(publicBucketRules()), WithClock(now.Now))
			second, _ := NewEvaluator(set.b, scope, source, WithRules(publicBucketRules()), WithClock(now.Now))
			// The first decides to open the issue, then the second opens it
			// before the first writes.
			first.beforeWrite = func(ctx context.Context, account string) {
				first.beforeWrite = nil
				if _, err := second.evaluateAccount(ctx, account, nil, nil, now.Now(), func(context.Context) error { return nil }); err != nil {
					t.Errorf("second evaluator: %v", err)
				}
			}
			if _, err := first.evaluateAccount(context.Background(), testAccount, nil, nil, now.Now(), func(context.Context) error { return nil }); err != nil {
				t.Fatal(err)
			}
			list, _ := first.Store().AWSIssues(context.Background(), scope, storage.AWSIssueQuery{})
			if len(list) != 1 {
				t.Fatalf("issues = %+v", list)
			}
			if kinds := eventKinds(t, first.Store(), scope, list[0].ID); len(kinds) != 1 {
				t.Fatalf("events = %v", kinds)
			}
		})
	}
}

// A user rejects an issue on one replica while the other replica's
// evaluation is about to write it: the issue ends rejected, never open,
// and later evaluations keep it rejected.
func TestRejectOnOneReplicaWhileTheOtherEvaluates(t *testing.T) {
	for _, set := range replicaBackends(t) {
		t.Run(set.name, func(t *testing.T) {
			ctx := context.Background()
			scope := tenancy.NewOrgScope(uniqueOrg("reject"))
			source := newFakeSource(testAccount)
			source.public(testAccount, "1", "docs-site")
			now := &clock{now: start}
			evaluator, _ := NewEvaluator(set.a, scope, source, WithRules(publicBucketRules()), WithClock(now.Now))
			actions, _ := NewActions(set.b, scope, now.Now)
			if _, err := evaluator.Evaluate(ctx); err != nil {
				t.Fatal(err)
			}
			id := IssueID(testRuleID, "arn:aws:s3:::docs-site")
			source.public(testAccount, "2", "docs-site")
			evaluator.beforeWrite = func(ctx context.Context, string string) {
				evaluator.beforeWrite = nil
				if _, err := actions.Reject(ctx, id, "ana@example.com", "public on purpose: docs site", ""); err != nil {
					t.Errorf("reject: %v", err)
				}
			}
			for range 3 {
				now.Add(time.Minute)
				source.public(testAccount, now.Now().Format(time.TimeOnly), "docs-site")
				if _, err := evaluator.Evaluate(ctx); err != nil {
					t.Fatal(err)
				}
			}
			issue, events, err := actions.Get(ctx, id)
			if err != nil || issue.State != StateRejected {
				t.Fatalf("issue = %+v, %v", issue, err)
			}
			if len(events) != 2 || events[0].Kind != EventOpened || events[1].Kind != EventRejected {
				t.Fatalf("events = %+v", events)
			}
		})
	}
}

// Retention deletes resolved issues and their events, and never a
// rejected issue, an issue still open or acknowledged, or any event of
// one, however old.
func TestRetentionKeepsRejectedIssues(t *testing.T) {
	for _, set := range replicaBackends(t) {
		t.Run(set.name, func(t *testing.T) {
			ctx := context.Background()
			scope := tenancy.NewOrgScope(uniqueOrg("retain"))
			source := newFakeSource(testAccount)
			source.public(testAccount, "1", "docs-site", "uploads", "logs")
			now := &clock{now: start}
			evaluator, _ := NewEvaluator(set.a, scope, source, WithRules(publicBucketRules()), WithClock(now.Now))
			actions, _ := NewActions(set.b, scope, now.Now)
			if _, err := evaluator.Evaluate(ctx); err != nil {
				t.Fatal(err)
			}
			docs, uploads := IssueID(testRuleID, "arn:aws:s3:::docs-site"), IssueID(testRuleID, "arn:aws:s3:::uploads")
			logs := IssueID(testRuleID, "arn:aws:s3:::logs")
			if _, err := actions.Reject(ctx, docs, "ana@example.com", "public on purpose: docs site", ""); err != nil {
				t.Fatal(err)
			}
			if _, err := actions.Acknowledge(ctx, logs, "bo@example.com"); err != nil {
				t.Fatal(err)
			}
			now.Add(time.Minute)
			source.public(testAccount, "1", "logs")
			if _, err := evaluator.Evaluate(ctx); err != nil {
				t.Fatal(err)
			}
			now.Add(storage.DefaultAWSIssueRetention + 24*time.Hour)
			report, err := evaluator.Evaluate(ctx)
			if err != nil || report.Pruned != 3 {
				t.Fatalf("retention pass = %+v, %v; want the resolved issue and its two events pruned", report, err)
			}
			list, _ := actions.List(ctx, storage.AWSIssueQuery{})
			if len(list) != 2 {
				t.Fatalf("after retention = %+v", list)
			}
			if issue, _, _ := actions.Get(ctx, docs); issue.Rejection == nil || issue.State != StateResolved {
				t.Fatalf("the rejected issue after retention = %+v", issue)
			}
			if kinds := eventKinds(t, evaluator.Store(), scope, logs); len(kinds) != 2 || kinds[1] != storage.AWSIssueEventAcknowledged {
				t.Fatalf("events of the acknowledged issue after %s = %v", storage.DefaultAWSIssueRetention+24*time.Hour, kinds)
			}
			if kinds := eventKinds(t, evaluator.Store(), scope, docs); len(kinds) != 3 || kinds[1] != storage.AWSIssueEventRejected {
				t.Fatalf("events of the rejected issue = %v", kinds)
			}
			if _, _, err := actions.Get(ctx, uploads); !errors.Is(err, ErrNotFound) {
				t.Fatalf("the resolved issue was kept: %v", err)
			}
			// The bucket comes back with the evidence it was rejected on: it
			// is rejected again, not opened.
			source.public(testAccount, "2", "docs-site")
			if _, err := evaluator.Evaluate(ctx); err != nil {
				t.Fatal(err)
			}
			if issue, _, _ := actions.Get(ctx, docs); issue.State != StateRejected {
				t.Fatalf("after it came back = %+v", issue)
			}
		})
	}
}

// A rejection made under the open-source default organization holds after
// a licensed upgrade: the licensed evaluator matches the issue by ID
// across its read scope, keeps it rejected, does not baseline the account
// again, and reopens it only on a material change.
func TestDefaultRejectionHoldsAfterLicence(t *testing.T) {
	for _, set := range replicaBackends(t) {
		t.Run(set.name, func(t *testing.T) {
			ctx := context.Background()
			def := uniqueOrg("default")
			community := tenancy.NewOrgScope(def)
			licensed := tenancy.OrgScope{Write: uniqueOrg("ent"), Read: []string{"", def}}
			licensed.Read[0] = licensed.Write
			source := newFakeSource(testAccount)
			source.public(testAccount, "1", "docs-site")
			now := &clock{now: start}
			before, _ := NewEvaluator(set.a, community, source, WithRules(publicBucketRules()), WithClock(now.Now))
			if _, err := before.Evaluate(ctx); err != nil {
				t.Fatal(err)
			}
			id := IssueID(testRuleID, "arn:aws:s3:::docs-site")
			oss, _ := NewActions(set.a, community, now.Now)
			if _, err := oss.Reject(ctx, id, "ana@example.com", "public on purpose: docs site", ""); err != nil {
				t.Fatal(err)
			}

			now.Add(time.Hour)
			source.public(testAccount, "2", "docs-site")
			after, _ := NewEvaluator(set.b, licensed, source, WithRules(publicBucketRules()), WithClock(now.Now))
			report, err := after.Evaluate(ctx)
			if err != nil || len(report.Accounts) != 1 || len(report.Accounts[0].Baselines) != 0 {
				t.Fatalf("licensed evaluation = %+v, %v", report, err)
			}
			actions, _ := NewActions(set.b, licensed, now.Now)
			issue, events, err := actions.Get(ctx, id)
			if err != nil || issue.State != StateRejected || issue.Rejection == nil || issue.Rejection.Reason != "public on purpose: docs site" {
				t.Fatalf("after the licence = %+v, %v", issue, err)
			}
			for _, event := range events {
				if event.Kind == EventOpened && event.At.After(start) || event.Kind == EventRegressed {
					t.Fatalf("the licensed evaluator opened it again: %+v", events)
				}
			}
			if list, _ := actions.List(ctx, storage.AWSIssueQuery{State: string(StateOpen)}); len(list) != 0 {
				t.Fatalf("open issues after the licence = %+v", list)
			}

			now.Add(time.Minute)
			source.mu.Lock()
			source.buckets[testAccount][0].Tags["principals"] = "*, arn:aws:iam::999900000000:root"
			source.mu.Unlock()
			for range 3 {
				if _, err := after.Evaluate(ctx); err != nil {
					t.Fatal(err)
				}
				now.Add(time.Minute)
			}
			issue, events, _ = actions.Get(ctx, id)
			regressed := 0
			for _, event := range events {
				if event.Kind == EventRegressed {
					regressed++
				}
			}
			if issue.State != StateRegressed || regressed != 1 || issue.Org != licensed.Write {
				t.Fatalf("after a material change = %+v, %d regressed", issue, regressed)
			}
		})
	}
}
