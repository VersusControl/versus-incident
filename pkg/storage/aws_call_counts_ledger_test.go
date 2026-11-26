package storage

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// Unit runs on several replicas add to the same hour's row at once: no add
// is lost. A count from the last nanosecond of an hour stays in that hour,
// the first nanosecond of the next starts a new row, and a time in a
// half-hour zone lands in its UTC hour.
func TestTablesCallLedgerAddsConcurrentRunsAndSplitsHours(t *testing.T) {
	for _, backend := range tablesBackends(t) {
		t.Run(backend.name, func(t *testing.T) {
			store, ok := AWSCallCountsOf(backend.provider)
			if !ok {
				t.Fatalf("%s provider has no call ledger", backend.name)
			}
			ctx := context.Background()
			scope := tenancy.OrgScope{Write: newTablesOrgs().ent}
			nine := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
			const writers, adds = 8, 25
			var group sync.WaitGroup
			errs := make(chan error, writers*adds)
			for writer := 0; writer < writers; writer++ {
				group.Add(1)
				go func(writer int) {
					defer group.Done()
					for add := 0; add < adds; add++ {
						at := nine.Add(time.Duration(writer*adds+add) * 17 * time.Second)
						errs <- store.AddAWSCallCounts(ctx, scope, []AWSCallCount{
							{AccountID: ledgerAccount, Region: "eu-south-2", Source: "elbv2", Hour: at, Calls: 3, Metered: 1, Runs: 1},
							{AccountID: ledgerAccount, Region: "eu-south-2", Source: fmt.Sprintf("unit-%d", writer), Hour: at, Calls: 1, Runs: 1},
						})
					}
				}(writer)
			}
			group.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			india := time.FixedZone("UTC+05:30", 5*3600+1800)
			mustDo(t, store.AddAWSCallCounts(ctx, scope, []AWSCallCount{
				{AccountID: ledgerAccount, Region: "eu-south-2", Source: "elbv2", Hour: nine.Add(time.Hour - time.Nanosecond), Calls: 5, Runs: 1},
				{AccountID: ledgerAccount, Region: "eu-south-2", Source: "elbv2", Hour: nine.Add(time.Hour), Calls: 7, Runs: 1},
				{AccountID: ledgerAccount, Region: "eu-south-2", Source: "elbv2", Hour: time.Date(2026, 11, 23, 15, 29, 0, 0, india), Calls: 11, Runs: 1},
			}))
			rows := must(store.AWSCallCounts(ctx, scope, AWSCallCountQuery{}))
			byKey := map[string]AWSCallCount{}
			for _, row := range rows {
				byKey[row.Hour.UTC().Format("15:04")+" "+row.Source] = row
			}
			elb := byKey["09:00 elbv2"]
			if elb.Calls != writers*adds*3+5+11 || elb.Metered != writers*adds || elb.Runs != writers*adds+2 {
				t.Fatalf("09:00 elbv2 = %+v, want %d calls, %d metered, %d runs", elb, writers*adds*3+16, writers*adds, writers*adds+2)
			}
			if next := byKey["10:00 elbv2"]; next.Calls != 7 || next.Runs != 1 {
				t.Fatalf("10:00 elbv2 = %+v", next)
			}
			for writer := 0; writer < writers; writer++ {
				if own := byKey[fmt.Sprintf("09:00 unit-%d", writer)]; own.Calls != adds || own.Runs != adds {
					t.Errorf("unit-%d = %+v, want %d", writer, own, adds)
				}
			}
			if len(rows) != writers+2 {
				t.Fatalf("rows = %d, want %d: %+v", len(rows), writers+2, rows)
			}
		})
	}
}

// The call ledger migration is schema only. Its table keeps one row per
// organization, account, Region, source and hour, refuses negative counts
// and a row without an hour, and keeps each organization's rows apart.
func TestAWSCallCountsMigrationSchema(t *testing.T) {
	text, err := os.ReadFile("migrations/043_aws_call_counts.sql")
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`(?i)\b(INSERT|UPDATE|DELETE)\b`).Match(stripSQLComments(string(text))) {
		t.Fatalf("migration carries a data statement:\n%s", text)
	}
	orgs := newTablesOrgs()
	for _, backend := range tablesBackends(t) {
		if backend.raw == nil {
			continue
		}
		t.Run(backend.name, func(t *testing.T) {
			insert := func(org, hour string, calls int) error {
				_, err := backend.raw.Exec(fmt.Sprintf(`INSERT INTO aws_call_counts (org_id, account_id, region, source, hour, calls, metered, runs)
 VALUES ('%s', '555500006666', 'eu-south-2', 'elbv2', %s, %d, 0, 1)`, org, hour, calls))
				return err
			}
			at := "'2026-11-22T10:00:00Z'"
			if err := insert(orgs.ent, at, 4); err != nil {
				t.Fatal(err)
			}
			if err := insert(orgs.ent, at, 4); err == nil {
				t.Fatal("a second row for the same organization, account, Region, source and hour was stored")
			}
			if err := insert(orgs.def, at, 4); err != nil {
				t.Fatalf("the same hour under another organization: %v", err)
			}
			if err := insert(orgs.ent, "'2026-11-22T11:00:00Z'", -1); err == nil {
				t.Fatal("a negative call count was stored")
			}
			if err := insert(orgs.ent, "NULL", 1); err == nil {
				t.Fatal("a row with no hour was stored")
			}
			if backend.postgres {
				var columns int
				if err := backend.raw.QueryRow(`SELECT count(*) FROM information_schema.key_column_usage k
 JOIN information_schema.table_constraints c ON c.constraint_name = k.constraint_name AND c.table_name = k.table_name
 WHERE k.table_name = 'aws_call_counts' AND c.constraint_type = 'PRIMARY KEY'`).Scan(&columns); err != nil {
					t.Fatal(err)
				}
				if columns != 5 {
					t.Fatalf("primary key columns = %d, want org_id, account_id, region, source and hour", columns)
				}
				var indexes int
				if err := backend.raw.QueryRow(`SELECT count(*) FROM pg_indexes WHERE tablename = 'aws_call_counts'
 AND indexname IN ('aws_call_counts_account_hour', 'aws_call_counts_hour')`).Scan(&indexes); err != nil {
					t.Fatal(err)
				}
				if indexes != 2 {
					t.Fatalf("ledger indexes = %d, want 2", indexes)
				}
			}
		})
	}
}
