package storage

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

func tablesPostgres(t *testing.T) *postgresProvider {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set: isolated Postgres required")
	}
	provider, err := NewPostgres(PostgresOptions{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	return provider.(*postgresProvider)
}

// explainTables returns the plan of one store statement. forceIndex turns
// sequential scans off for the transaction, which shows whether an index
// path exists for the statement even on a small table.
func explainTables(t *testing.T, p *postgresProvider, statement string, args []any, forceIndex bool) string {
	t.Helper()
	ctx := context.Background()
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if forceIndex {
		if _, err := tx.ExecContext(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := tx.QueryContext(ctx, "EXPLAIN "+statement, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(plan, "\n")
}

func seedTablesPlanData(t *testing.T, p *postgresProvider, scope tenancy.OrgScope) {
	t.Helper()
	ctx := context.Background()
	for account := 0; account < 6; account++ {
		for _, service := range []string{"ec2", "rds", "lambda"} {
			unit := AWSUnit{fmt.Sprintf("1000000000%02d", account), tablesRegion, service}
			// Three generations per unit: the partial current index covers
			// only the newest rows, so it is materially smaller than the
			// history index and the planner's choice proves which one the
			// current-row statement uses.
			for generation := 0; generation < 3; generation++ {
				resources := make([]AWSResource, 40)
				for index := range resources {
					resources[index] = AWSResource{ARN: fmt.Sprintf("arn:aws:%s:%s:%s:r/%s-%03d", service, unit.Region, unit.Account, service, index), Type: service,
						App: fmt.Sprintf("app-%d", index%5), Name: fmt.Sprintf("web-%02d%03d-g%d", account, index, generation), State: "running"}
				}
				at := tablesBase.Add(time.Duration(generation) * time.Hour)
				must(p.CommitAWSUnit(ctx, scope, unit, AWSCandidate{At: at, Digest: fmt.Sprintf("%s-g%d", unit.Service, generation), Resources: resources}, nil))
			}
		}
		changes := make([]AWSChange, 50)
		for index := range changes {
			changes[index] = AWSChange{EventID: fmt.Sprintf("e-%d-%d", account, index), Account: fmt.Sprintf("1000000000%02d", account), Region: tablesRegion,
				EventTime: tablesBase.Add(time.Duration(index) * time.Hour), ResourceARN: fmt.Sprintf("arn:%d", index%7), App: fmt.Sprintf("app-%d", index%5)}
		}
		mustDo(t, p.AppendAWSChanges(ctx, scope, fmt.Sprintf("1000000000%02d", account), tablesRegion, changes, []AWSInterval{{tablesBase, tablesBase.Add(60 * time.Hour)}}, nil))
	}
	days := make([]string, 0, 30)
	var lines []FinOpsCostLine
	for day := 0; day < 30; day++ {
		date := time.Date(2026, 1, 1+day, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
		days = append(days, date)
		for account := 0; account < 10; account++ {
			// One region per account keeps a region predicate selective
			// enough that the region index is the cheaper path than the day
			// range it narrows.
			region := fmt.Sprintf("region-%d", account)
			for service, team := range map[string]string{"compute": "payments", "storage": "search", "network.nat": "", "database": "platform"} {
				lines = append(lines, FinOpsCostLine{Day: date, Account: fmt.Sprintf("acct-%d", account), Region: region, Service: service, RecordType: "Usage", Team: team, NetAmortizedMicros: 1000})
			}
		}
	}
	mustDo(t, p.ReplaceFinOpsCostDays(ctx, scope, "payer", days, lines))
	for index := 0; index < 40; index++ {
		status := []string{"open", "expected", "resolved", "dismissed"}[index%4]
		mustDo(t, p.PutFinOpsIncrease(ctx, scope, FinOpsIncrease{ID: fmt.Sprintf("inc-%d", index), Service: "compute", StartedOn: days[index%30], Status: status, OwnerTeam: "payments"}))
	}
	for _, table := range []string{"aws_resources", "aws_sync_units", "aws_changes", "aws_change_coverage", "finops_cost_lines", "finops_increases"} {
		if _, err := p.db.Exec(`ANALYZE ` + table); err != nil {
			t.Fatal(err)
		}
	}
}

// TestPostgresTablesMainQueriesUseIndexes proves the statements behind the
// main read paths have an index: resources by unit, name and ARN, changes in
// a window, cost by team, service and day, and open increases. "a|b" accepts
// either of two indexes that lead with the same key.
func TestPostgresTablesMainQueriesUseIndexes(t *testing.T) {
	defer failOnPanic(t)
	p := tablesPostgres(t)
	orgs := newTablesOrgs()
	scope := tenancy.NewOrgScope(orgs.ent)
	enterprise := tenancy.NewOrgScope(orgs.ent, orgs.def)
	seedTablesPlanData(t, p, scope)
	team, from, through := "payments", "2026-01-05", "2026-01-25"
	plans := []struct {
		name, index string
		query       func() (string, []any)
	}{
		{"resources by account, region and service", "aws_resources_current", func() (string, []any) {
			return p.resourcesSQL(scope, AWSResourceQuery{Account: "100000000003", Region: tablesRegion, Service: "rds"})
		}},
		{"resources by unit for an enterprise read", "aws_resources_current|aws_resources_history", func() (string, []any) {
			return p.resourcesSQL(enterprise, AWSResourceQuery{Account: "100000000003", Region: tablesRegion, Service: "rds"})
		}},
		{"search by name", "aws_resources_name", func() (string, []any) { return p.resourcesSQL(scope, AWSResourceQuery{NamePrefix: "WEB-03"}) }},
		{"search by ARN", "aws_resources_arn", func() (string, []any) {
			return p.resourcesSQL(scope, AWSResourceQuery{ARN: "arn:aws:rds:us-east-1:100000000003:r/rds-007"})
		}},
		{"resources by ARN as of a time", "aws_resources_arn", func() (string, []any) {
			return p.resourcesSQL(scope, AWSResourceQuery{ARN: "arn:aws:rds:us-east-1:100000000003:r/rds-007", AsOf: tablesBase.Add(time.Hour)})
		}},
		{"changes in a time window", "aws_changes_time", func() (string, []any) {
			return p.changesSQL(scope, AWSChangeQuery{Since: tablesBase.Add(10 * time.Hour), Until: tablesBase.Add(12 * time.Hour)})
		}},
		{"changes of a resource", "aws_changes_resource", func() (string, []any) {
			return p.changesSQL(scope, AWSChangeQuery{ResourceARN: "arn:3", Since: tablesBase, Until: tablesBase.Add(48 * time.Hour)})
		}},
		{"cost of a team by day", "finops_cost_lines_team", func() (string, []any) {
			return p.costTotalsSQL(scope, FinOpsCostQuery{Team: &team, From: from, Through: through}, []FinOpsDimension{FinOpsByDay})
		}},
		{"cost of a service by day", "finops_cost_lines_service", func() (string, []any) {
			return p.costTotalsSQL(scope, FinOpsCostQuery{Service: "storage", From: from, Through: through}, []FinOpsDimension{FinOpsByDay})
		}},
		{"cost by team over days", "finops_cost_lines_day", func() (string, []any) {
			return p.costTotalsSQL(scope, FinOpsCostQuery{From: from, Through: through}, []FinOpsDimension{FinOpsByTeam})
		}},
		{"cost of a team list by day", "finops_cost_lines_team", func() (string, []any) {
			return p.costTotalsSQL(scope, FinOpsCostQuery{Teams: []string{"payments", "search"}, From: from, Through: through}, []FinOpsDimension{FinOpsByDay})
		}},
		{"cost of an account list by day", "finops_cost_lines_account", func() (string, []any) {
			return p.costTotalsSQL(scope, FinOpsCostQuery{Accounts: []string{"acct-3", "acct-4"}, From: from, Through: through}, []FinOpsDimension{FinOpsByDay})
		}},
		{"cost of a region list by day", "finops_cost_lines_region", func() (string, []any) {
			return p.costTotalsSQL(scope, FinOpsCostQuery{Regions: []string{"region-3"}, From: from, Through: through}, []FinOpsDimension{FinOpsByDay})
		}},
		{"open increases", "finops_increases_status", func() (string, []any) {
			return p.increasesSQL(scope, FinOpsIncreaseQuery{Statuses: []string{"open", "expected"}})
		}},
	}
	for _, item := range plans {
		statement, args := item.query()
		text := explainTables(t, p, statement, args, true)
		t.Logf("%s:\n%s", item.name, text)
		usesIndex := false
		for _, index := range strings.Split(item.index, "|") {
			usesIndex = usesIndex || strings.Contains(text, " "+index+" ")
		}
		if !usesIndex || strings.Contains(text, "Seq Scan") {
			t.Fatalf("%s does not use %s:\n%s", item.name, item.index, text)
		}
	}
}

func tablesLoadMedian(t *testing.T, runs int, fn func()) time.Duration {
	t.Helper()
	durations := make([]time.Duration, runs)
	for index := range durations {
		started := time.Now()
		fn()
		durations[index] = time.Since(started)
	}
	sort.Slice(durations, func(left, right int) bool { return durations[left] < durations[right] })
	return durations[runs/2]
}

// TestPostgresTablesLoadBudgets loads 50k resources with 30 days of history
// and 13 months of cost lines for 150 accounts, then holds the inventory
// search under 200 ms and the FinOps month summary under 300 ms. It takes
// minutes, so it runs only with TEST_TABLES_LOAD=1.
func TestPostgresTablesLoadBudgets(t *testing.T) {
	if os.Getenv("TEST_TABLES_LOAD") != "1" {
		t.Skip("TEST_TABLES_LOAD=1 not set")
	}
	defer failOnPanic(t)
	p := tablesPostgres(t)
	ctx := context.Background()
	scope := tenancy.NewOrgScope(newTablesOrgs().ent)
	const accounts, perUnit, days = 150, 42, 30
	regions := []string{"us-east-1", "eu-west-1"}
	services := []string{"ec2", "rds", "lambda", "s3"}
	kinds := []string{"web", "api", "db", "queue"}
	type unitPlan struct {
		unit      AWSUnit
		resources []AWSResource
	}
	var units []unitPlan
	counter := 0
	for account := 0; account < accounts; account++ {
		for _, region := range regions {
			for _, service := range services {
				unit := AWSUnit{fmt.Sprintf("%012d", 200000000000+account), region, service}
				resources := make([]AWSResource, perUnit)
				for index := range resources {
					resources[index] = AWSResource{ARN: fmt.Sprintf("arn:aws:%s:%s:%s:r/%06d", service, region, unit.Account, counter), Type: service,
						App: fmt.Sprintf("app-%02d", counter%40), Name: fmt.Sprintf("%s-%06d", kinds[counter%4], counter), State: "running",
						Tags: map[string]string{"team": "payments", "env": "[redacted]"}}
					counter++
				}
				units = append(units, unitPlan{unit, resources})
			}
		}
	}
	started := time.Now()
	work := make(chan unitPlan)
	var group sync.WaitGroup
	var failed sync.Once
	var failure error
	for worker := 0; worker < 8; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for item := range work {
				for day := 0; day < days; day++ {
					resources := append([]AWSResource(nil), item.resources...)
					resources[day%perUnit].State = fmt.Sprintf("day-%d", day)
					if _, err := p.CommitAWSUnit(ctx, scope, item.unit, AWSCandidate{At: tablesBase.Add(time.Duration(day) * 24 * time.Hour), Digest: fmt.Sprintf("day-%d", day), Resources: resources}, nil); err != nil {
						failed.Do(func() { failure = err })
						return
					}
				}
			}
		}()
	}
	for _, item := range units {
		work <- item
	}
	close(work)
	group.Wait()
	mustDo(t, failure)
	t.Logf("inventory: %d units, %d current resources, %d daily commits each, loaded in %s", len(units), counter, days, time.Since(started).Round(time.Second))

	started = time.Now()
	type month struct{ days []string }
	var months []month
	for day := time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC); day.Before(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)); day = day.AddDate(0, 0, 1) {
		if day.Day() == 1 {
			months = append(months, month{})
		}
		months[len(months)-1].days = append(months[len(months)-1].days, day.Format(time.DateOnly))
	}
	teams := []string{"payments", "search", "platform", ""}
	lineServices := []string{"compute", "storage", "network.nat", "database"}
	usage := []string{"BoxUsage", "DataTransfer", "Requests"}
	monthWork := make(chan month)
	total := 0
	var totalMu sync.Mutex
	for worker := 0; worker < 4; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for item := range monthWork {
				lines := make([]FinOpsCostLine, 0, len(item.days)*accounts*12)
				for _, day := range item.days {
					for account := 0; account < accounts; account++ {
						for serviceIndex, service := range lineServices {
							for usageIndex, usageType := range usage {
								lines = append(lines, FinOpsCostLine{Day: day, Account: fmt.Sprintf("%012d", 200000000000+account), Region: regions[account%2], Service: service,
									UsageType: usageType, RecordType: "Usage", Team: teams[(account+serviceIndex)%4], NetAmortizedMicros: int64(1000 + account + usageIndex), QuantityMicros: 1_000_000})
							}
						}
					}
				}
				if err := p.ReplaceFinOpsCostDays(ctx, scope, "payer", item.days, lines); err != nil {
					failed.Do(func() { failure = err })
					return
				}
				totalMu.Lock()
				total += len(lines)
				totalMu.Unlock()
			}
		}()
	}
	for _, item := range months {
		monthWork <- item
	}
	close(monthWork)
	group.Wait()
	mustDo(t, failure)
	t.Logf("finops: %d months, %d cost lines, loaded in %s", len(months), total, time.Since(started).Round(time.Second))
	for _, table := range []string{"aws_resources", "aws_unit_generations", "aws_sync_units", "finops_cost_lines"} {
		if _, err := p.db.Exec(`ANALYZE ` + table); err != nil {
			t.Fatal(err)
		}
	}

	search := tablesLoadMedian(t, 7, func() {
		rows := must(p.AWSResources(ctx, scope, AWSResourceQuery{NamePrefix: "WEB-0012"}))
		if len(rows) == 0 {
			t.Fatal("search found nothing")
		}
	})
	byUnit := tablesLoadMedian(t, 7, func() {
		must(p.AWSResources(ctx, scope, AWSResourceQuery{Account: "200000000077", Region: "eu-west-1", Service: "rds"}))
	})
	asOf := tablesLoadMedian(t, 7, func() {
		must(p.AWSResources(ctx, scope, AWSResourceQuery{Account: "200000000077", AsOf: tablesBase.Add(10*24*time.Hour + time.Hour)}))
	})
	summary := tablesLoadMedian(t, 5, func() {
		query := FinOpsCostQuery{From: "2026-01-01", Through: "2026-01-31"}
		must(p.FinOpsCostTotals(ctx, scope, query))
		must(p.FinOpsCostTotals(ctx, scope, query, FinOpsByTeam))
		must(p.FinOpsCostTotals(ctx, scope, query, FinOpsByService))
		must(p.FinOpsCostTotals(ctx, scope, query, FinOpsByDay))
		must(p.FinOpsCostTotals(ctx, scope, query, FinOpsByAccount))
		must(p.FinOpsCostTotals(ctx, scope, FinOpsCostQuery{From: "2025-02-01", Through: "2026-02-28"}, FinOpsByMonth))
	})
	t.Logf("search by name median %s; resources of a unit %s; account as of day 10 %s; FinOps month summary (6 aggregations) %s", search, byUnit, asOf, summary)
	statement, args := p.resourcesSQL(scope, AWSResourceQuery{NamePrefix: "web-0012"})
	t.Logf("search plan at load:\n%s", explainTables(t, p, statement, args, false))
	statement, args = p.costTotalsSQL(scope, FinOpsCostQuery{From: "2026-01-01", Through: "2026-01-31"}, []FinOpsDimension{FinOpsByTeam})
	t.Logf("summary by team plan at load:\n%s", explainTables(t, p, statement, args, false))
	if search > 200*time.Millisecond || byUnit > 200*time.Millisecond {
		t.Fatalf("inventory search over budget: name %s, unit %s", search, byUnit)
	}
	if summary > 300*time.Millisecond {
		t.Fatalf("FinOps summary over budget: %s", summary)
	}
}

// TestPostgresAWSRetentionSweep proves Postgres retention: old generations
// and the rows that stopped being current before the age cutoff are deleted
// in bounded batches that resume, while current rows (valid_to IS NULL) are
// never touched.
func TestPostgresAWSRetentionSweep(t *testing.T) {
	p := tablesPostgres(t)
	ctx := context.Background()
	scope := tenancy.NewOrgScope(newTablesOrgs().ent)
	unit := AWSUnit{"100000000099", tablesRegion, "ec2"}
	for _, table := range []string{"aws_sync_units", "aws_unit_generations", "aws_resources"} {
		if _, err := p.db.Exec(`DELETE FROM `+table+` WHERE org_id = $1`, scope.Write); err != nil {
			t.Fatal(err)
		}
	}
	count := func(statement string) int64 {
		t.Helper()
		var rows int64
		if err := p.db.QueryRow(statement, scope.Write).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	rowsOf := func(current bool) string {
		where := `valid_to IS NOT NULL`
		if current {
			where = `valid_to IS NULL`
		}
		return `SELECT count(*) FROM aws_resources WHERE org_id = $1 AND ` + where
	}
	generations := func() int64 {
		return count(`SELECT count(*) FROM aws_unit_generations WHERE org_id = $1`)
	}

	resources := func(prefix string) []AWSResource {
		rows := make([]AWSResource, 3)
		for index := range rows {
			rows[index] = AWSResource{ARN: fmt.Sprintf("arn:aws:ec2:%s:%s:instance/%s-%d", tablesRegion, unit.Account, prefix, index), Type: "instance"}
		}
		return rows
	}
	for index, prefix := range []string{"a", "b", "c"} {
		must(p.CommitAWSUnit(ctx, scope, unit, AWSCandidate{At: tablesBase.Add(time.Duration(index) * time.Hour), Digest: prefix, Resources: resources(prefix)}, nil))
	}
	if got := count(rowsOf(true)); got != 3 {
		t.Fatalf("current rows before the sweep = %d, want 3", got)
	}
	if got := count(rowsOf(false)); got != 6 {
		t.Fatalf("expired rows before the sweep = %d, want 6", got)
	}
	t.Logf("before sweep: current=%d expired=%d generations=%d", count(rowsOf(true)), count(rowsOf(false)), generations())

	// A deadline in the past stops the pass before it deletes anything, so
	// the caller can resume it later instead of blocking a writer.
	result := must(p.SweepAWSRetention(ctx, AWSRetentionSweep{
		Scope: scope, HistoryBefore: tablesBase.Add(90 * time.Minute), ChangeBefore: tablesBase,
		KeepGenerations: 2, Batch: 1, Deadline: time.Now().Add(-time.Second),
	}))
	if !result.More {
		t.Fatalf("a pass past its deadline reported no more work: %+v", result)
	}

	// The deferred pass removed nothing, so it can resume from its cursor
	// without having held a lock or dropped a row early.
	if got := count(rowsOf(false)); got != 6 {
		t.Fatalf("the deadline pass still deleted rows: %d expired left, want 6", got)
	}
	// One row per statement walks the backlog in several short statements.
	for result.More {
		result = must(p.SweepAWSRetention(ctx, AWSRetentionSweep{
			Scope: scope, HistoryBefore: tablesBase.Add(90 * time.Minute), ChangeBefore: tablesBase,
			KeepGenerations: 2, Batch: 1, Cursor: result.Cursor,
		}))
	}
	if got := generations(); got != 2 {
		t.Fatalf("generations after keep=2 = %d, want the kept and current generation", got)
	}
	if got := count(rowsOf(false)); got != 3 {
		t.Fatalf("expired rows after the sweep = %d, want only the kept generation's", got)
	}
	if got := count(rowsOf(true)); got != 3 {
		t.Fatalf("current rows after the sweep = %d, want 3", got)
	}
	t.Logf("after count+age sweep: current=%d expired=%d generations=%d", count(rowsOf(true)), count(rowsOf(false)), generations())
	current := must(p.AWSResources(ctx, scope, AWSResourceQuery{Service: "ec2"}))
	if len(current) != 3 || current[0].ARN != resources("c")[0].ARN {
		t.Fatalf("current resources read back as %+v", current)
	}
	if old := must(p.AWSResources(ctx, scope, AWSResourceQuery{Service: "ec2", AsOf: tablesBase.Add(30 * time.Minute)})); len(old) != 0 {
		t.Fatalf("a pruned generation still answers: %+v", old)
	}

	// Even a cutoff far in the future never deletes a current row.
	must(p.SweepAWSRetention(ctx, AWSRetentionSweep{
		Scope: scope, HistoryBefore: time.Now().Add(100 * 365 * 24 * time.Hour), ChangeBefore: time.Now().Add(100 * 365 * 24 * time.Hour),
		KeepGenerations: 2, Batch: 1,
	}))
	if got := count(rowsOf(true)); got != 3 {
		t.Fatalf("a far-future cutoff deleted current rows: %d", got)
	}
	t.Logf("after far-future cutoff: current=%d expired=%d generations=%d", count(rowsOf(true)), count(rowsOf(false)), generations())
}

// TestPostgresFinOpsTotalsPlanUsesCoveringIndex proves the page totals
// aggregation is answered from the totals index on Postgres: the key is in
// the query's grouping order and the included columns carry the sums, so the
// plan is an index-only scan instead of a table scan.
func TestPostgresFinOpsTotalsPlanUsesCoveringIndex(t *testing.T) {
	p := tablesPostgres(t)
	ctx := context.Background()
	scope := tenancy.NewOrgScope(newTablesOrgs().ent)
	// A month and a half of history, so the queried window is a small part
	// of the table and the planner weighs the index against a full scan the
	// way it does at the acceptance scale.
	days := make([]string, 0, 45)
	for day := 0; day < 45; day++ {
		days = append(days, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, day).Format(time.DateOnly))
	}
	lines := make([]FinOpsCostLine, 0, 45000)
	for _, day := range days {
		for index := 0; index < 1000; index++ {
			lines = append(lines, FinOpsCostLine{Day: day, Payer: "payer", Account: fmt.Sprintf("acct-%d", index%9), Region: "us-east-1",
				Service: []string{"compute", "storage"}[index%2], ServiceName: fmt.Sprintf("service-%d", index%3), UsageGroup: "group",
				UsageType: fmt.Sprintf("usage-%04d", index), RecordType: "Usage", Team: []string{"payments", "search", ""}[index%3],
				NetAmortizedMicros: int64(1000 + index), QuantityMicros: int64(index)})
		}
	}
	mustDo(t, p.ReplaceFinOpsCostDays(ctx, scope, "payer", days, lines))
	for _, statement := range []string{"ANALYZE finops_cost_lines", "VACUUM finops_cost_lines"} {
		if _, err := p.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	noResource := ""
	statement, args := p.costTotalsSQL(inventoryScope(scope), FinOpsCostQuery{From: days[0], Through: days[1], ResourceID: &noResource},
		[]FinOpsDimension{FinOpsByDay, FinOpsByAccount, FinOpsByRegion, FinOpsByService, FinOpsByServiceName, FinOpsByUsageGroup, FinOpsByRecordType, FinOpsByTeam, FinOpsByPayer})
	plan := explainTables(t, p, statement, args, false)
	if strings.Contains(plan, "Seq Scan on finops_cost_lines") || strings.Contains(plan, "Bitmap Heap Scan on finops_cost_lines") {
		t.Fatalf("totals aggregation reads the cost-line table:\n%s", plan)
	}
	if !strings.Contains(plan, "finops_cost_lines_totals") {
		t.Fatalf("totals aggregation does not use the covering index:\n%s", plan)
	}
	if !strings.Contains(plan, "Index Only Scan") {
		t.Fatalf("totals aggregation is not an index-only scan:\n%s", plan)
	}
	t.Logf("totals plan:\n%s", plan)
}
