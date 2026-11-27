package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

const (
	ledgerAccount = "123400000001"
	ledgerOther   = "123400000002"
)

// The call ledger behaves the same on every backend: each run adds to its
// hour's row, reads sum the read organizations, a window and an account
// narrow the read, and pruning removes only the write organization's old
// hours.
func TestTablesParityAWSCallCounts(t *testing.T) {
	runTablesParity(t, func(t *testing.T, backend tablesBackend, orgs tablesOrgs, record tablesRecorder) {
		store, ok := AWSCallCountsOf(backend.provider)
		if !ok {
			t.Fatalf("%s provider has no call ledger", backend.name)
		}
		ctx := context.Background()
		archive := tenancy.OrgScope{Write: orgs.def}
		licensed := tenancy.OrgScope{Write: orgs.ent, Read: []string{orgs.ent, orgs.def}}
		hour := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)

		// Two runs in the same hour add up; the minutes are dropped.
		mustDo(t, store.AddAWSCallCounts(ctx, licensed, []AWSCallCount{
			{AccountID: ledgerAccount, Region: "eu-south-2", Source: "elbv2", Hour: hour.Add(12 * time.Minute), Calls: 21, Runs: 1},
			{AccountID: ledgerAccount, Region: "global", Source: "s3-policy", Hour: hour.Add(14 * time.Minute), Calls: 30, Metered: 29, Runs: 1},
		}))
		mustDo(t, store.AddAWSCallCounts(ctx, licensed, []AWSCallCount{
			{AccountID: ledgerAccount, Region: "eu-south-2", Source: "elbv2", Hour: hour.Add(22 * time.Minute), Calls: 22, Runs: 1},
			{AccountID: ledgerAccount, Region: "eu-south-2", Source: "elbv2", Hour: hour.Add(32 * time.Minute), Calls: 0, Runs: 0},
		}))
		// The archive organization counted calls before the upgrade.
		mustDo(t, store.AddAWSCallCounts(ctx, archive, []AWSCallCount{
			{AccountID: ledgerAccount, Region: "eu-south-2", Source: "elbv2", Hour: hour, Calls: 5, Runs: 1},
			{AccountID: ledgerOther, Region: "eu-west-1", Source: "cloudtrail", Hour: hour.Add(-3 * time.Hour), Calls: 7, Runs: 2},
		}))

		all := must(store.AWSCallCounts(ctx, licensed, AWSCallCountQuery{}))
		if len(all) != 3 {
			t.Fatalf("licensed read = %+v", all)
		}
		elb := all[1]
		if elb.Source != "elbv2" || elb.Calls != 48 || elb.Runs != 3 || !elb.Hour.Equal(hour) {
			t.Fatalf("elbv2 hour = %+v", elb)
		}
		record("licensed read", all)
		record("archive read", must(store.AWSCallCounts(ctx, archive, AWSCallCountQuery{})))
		window := must(store.AWSCallCounts(ctx, licensed, AWSCallCountQuery{AccountID: ledgerAccount, Since: hour, Until: hour.Add(time.Hour)}))
		if len(window) != 2 || window[1].Metered != 29 {
			t.Fatalf("account window = %+v", window)
		}
		record("account window", window)
		record("before the window", must(store.AWSCallCounts(ctx, licensed, AWSCallCountQuery{Until: hour})))

		removed := must(store.PruneAWSCallCounts(ctx, licensed, hour.Add(2*time.Hour)))
		record("pruned", removed)
		left := must(store.AWSCallCounts(ctx, licensed, AWSCallCountQuery{}))
		if removed != 2 || len(left) != 2 {
			t.Fatalf("after prune removed %d, left %+v", removed, left)
		}
		record("after prune", left)

		for name, bad := range map[string]AWSCallCount{
			"account":        {AccountID: "12", Region: "eu-south-2", Source: "elbv2", Hour: hour, Calls: 1},
			"region":         {AccountID: ledgerAccount, Region: "", Source: "elbv2", Hour: hour, Calls: 1},
			"source":         {AccountID: ledgerAccount, Region: "eu-south-2", Source: "bad\x00", Hour: hour, Calls: 1},
			"negative":       {AccountID: ledgerAccount, Region: "eu-south-2", Source: "elbv2", Hour: hour, Calls: -1},
			"metered > call": {AccountID: ledgerAccount, Region: "eu-south-2", Source: "kms", Hour: hour, Calls: 1, Metered: 2},
			"no hour":        {AccountID: ledgerAccount, Region: "eu-south-2", Source: "elbv2", Calls: 1},
		} {
			if err := store.AddAWSCallCounts(ctx, licensed, []AWSCallCount{bad}); !errors.Is(err, ErrInvalidAWSCallCount) {
				t.Errorf("%s: add = %v", name, err)
			}
		}
		if _, err := store.AWSCallCounts(ctx, licensed, AWSCallCountQuery{AccountID: "abc"}); !errors.Is(err, ErrInvalidAWSCallCount) {
			t.Errorf("bad account read = %v", err)
		}
	})
}

// The batched retention sweep removes call ledger hours older than its
// cutoff in the write organization and keeps the rest.
func TestTablesAWSRetentionSweepRemovesOldCallCounts(t *testing.T) {
	for _, backend := range tablesBackends(t) {
		sweeper, ok := backend.provider.(AWSRetentionSweeper)
		if !ok {
			continue
		}
		t.Run(backend.name, func(t *testing.T) {
			store, _ := AWSCallCountsOf(backend.provider)
			ctx := context.Background()
			orgs := newTablesOrgs()
			scope := tenancy.OrgScope{Write: orgs.ent}
			other := tenancy.OrgScope{Write: orgs.def}
			now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
			var counts []AWSCallCount
			for day := 0; day < 20; day++ {
				counts = append(counts, AWSCallCount{AccountID: ledgerAccount, Region: "eu-south-2", Source: "ec2", Hour: now.Add(-time.Duration(day) * 24 * time.Hour), Calls: 10, Runs: 1})
			}
			mustDo(t, store.AddAWSCallCounts(ctx, scope, counts))
			mustDo(t, store.AddAWSCallCounts(ctx, other, counts))
			result, err := sweeper.SweepAWSRetention(ctx, AWSRetentionSweep{Scope: scope, KeepGenerations: 1, CallCountsBefore: now.Add(-DefaultAWSCallCountRetention), Batch: 2})
			if err != nil {
				t.Fatal(err)
			}
			if result.CallCounts != 5 || result.More {
				t.Fatalf("sweep = %+v", result)
			}
			if kept := must(store.AWSCallCounts(ctx, scope, AWSCallCountQuery{})); len(kept) != 15 {
				t.Fatalf("kept %d hours", len(kept))
			}
			if untouched := must(store.AWSCallCounts(ctx, other, AWSCallCountQuery{})); len(untouched) != 20 {
				t.Fatalf("other organization kept %d hours", len(untouched))
			}
		})
	}
}

// The last 24 hours of one account and of every account have an index
// path, so the cost card never scans the whole ledger. On a small table
// the planner may pick either hour index; neither read is a sequential
// scan.
func TestPostgresCallCountQueriesUseIndexes(t *testing.T) {
	p := tablesPostgres(t)
	since := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		statement string
		args      []any
	}{
		"one account": {`SELECT account_id, region, source, hour, calls FROM aws_call_counts WHERE org_id IN ($1) AND account_id = $2 AND hour >= $3`,
			[]any{"call-plan", ledgerAccount, since}},
		"every account": {`SELECT account_id, region, source, hour, calls FROM aws_call_counts WHERE org_id IN ($1) AND hour >= $2`,
			[]any{"call-plan", since}},
	} {
		plan := explainTables(t, p, tc.statement, tc.args, true)
		if strings.Contains(plan, "Seq Scan") || !strings.Contains(plan, "aws_call_counts_") {
			t.Errorf("%s plan has no ledger index path:\n%s", name, plan)
		}
	}
}

// Each hour keeps the largest single run beside the sum, on every backend
// and across the organizations a read merges: what a per-run budget is
// measured against.
func TestTablesParityAWSCallCountPeak(t *testing.T) {
	runTablesParity(t, func(t *testing.T, backend tablesBackend, orgs tablesOrgs, record tablesRecorder) {
		store, _ := AWSCallCountsOf(backend.provider)
		ctx := context.Background()
		licensed := tenancy.OrgScope{Write: orgs.ent, Read: []string{orgs.ent, orgs.def}}
		hour := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
		for _, run := range []int64{12, 30, 7} {
			mustDo(t, store.AddAWSCallCounts(ctx, licensed, []AWSCallCount{{AccountID: ledgerAccount, Region: "eu-south-2", Source: "elbv2", Hour: hour, Calls: run, Runs: 1, Peak: run}}))
		}
		mustDo(t, store.AddAWSCallCounts(ctx, tenancy.OrgScope{Write: orgs.def}, []AWSCallCount{{AccountID: ledgerAccount, Region: "eu-south-2", Source: "elbv2", Hour: hour, Calls: 41, Runs: 1, Peak: 41}}))
		rows := must(store.AWSCallCounts(ctx, licensed, AWSCallCountQuery{}))
		if len(rows) != 1 || rows[0].Calls != 90 || rows[0].Runs != 4 || rows[0].Peak != 41 {
			t.Fatalf("rows = %+v", rows)
		}
		record("rows", rows)
		if err := store.AddAWSCallCounts(ctx, licensed, []AWSCallCount{{AccountID: ledgerAccount, Region: "eu-south-2", Source: "elbv2", Hour: hour, Calls: 3, Runs: 1, Peak: 4}}); !errors.Is(err, ErrInvalidAWSCallCount) {
			t.Fatalf("a peak above the calls = %v", err)
		}
	})
}
