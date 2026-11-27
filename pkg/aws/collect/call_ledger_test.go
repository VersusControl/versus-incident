package collect

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	trailtypes "github.com/aws/aws-sdk-go-v2/service/cloudtrail/types"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	"github.com/VersusControl/versus-incident/tests/fakeaws"
)

// ledgerRecorder keeps every count the collector writes, in a real memory
// ledger so runs add up as they do in storage.
type ledgerRecorder struct {
	mu     sync.Mutex
	store  storage.AWSCallCountStore
	writes int
}

func newLedgerRecorder() *ledgerRecorder {
	store, _ := storage.AWSCallCountsOf(storage.NewMemory())
	return &ledgerRecorder{store: store}
}

func (recorder *ledgerRecorder) record(ctx context.Context, counts []storage.AWSCallCount) error {
	recorder.mu.Lock()
	recorder.writes++
	recorder.mu.Unlock()
	return recorder.store.AddAWSCallCounts(ctx, tenancy.DefaultOrgScope(), counts)
}

func (recorder *ledgerRecorder) rows(t *testing.T) []storage.AWSCallCount {
	t.Helper()
	rows, err := recorder.store.AWSCallCounts(context.Background(), tenancy.DefaultOrgScope(), storage.AWSCallCountQuery{})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func (recorder *ledgerRecorder) total(t *testing.T) int64 {
	t.Helper()
	var total int64
	for _, row := range recorder.rows(t) {
		total += row.Calls
	}
	return total
}

func syncState(t *testing.T, h harness, account, service string) model.SyncState {
	t.Helper()
	statuses, err := h.store.Sync(context.Background(), snapshot.Query{Account: account, Service: service})
	if err != nil || len(statuses) != 1 {
		t.Fatalf("%s statuses = %+v, %v", service, statuses, err)
	}
	return statuses[0].Sync
}

// The ledger counts every request a unit run makes, once per run, under
// its account, Region and unit service, with the metered part named by
// the API: every request that reached AWS is in it, and nothing else.
func TestLedgerCountsEveryCallPerSource(t *testing.T) {
	ledger := newLedgerRecorder()
	h := newHarness(t, fakeaws.ScenarioEstateHubSpoke, WithCallLedger(ledger.record))
	before := h.fake.TotalRequests()
	report := mustSync(t, h, target(fakeaws.EstateData, fakeaws.EstateHomeRegion))
	sent := h.fake.TotalRequests() - before
	if total := ledger.total(t); total != sent || sent == 0 {
		t.Fatalf("ledger counted %d calls, AWS answered %d", total, sent)
	}
	byUnit := map[string]storage.AWSCallCount{}
	for _, row := range ledger.rows(t) {
		byUnit[row.Region+"/"+row.Source] = row
	}
	runs := 0
	for _, unit := range report.Units {
		row, ok := byUnit[unit.Unit.Region+"/"+unit.Unit.Service]
		if unit.Calls == 0 {
			if ok {
				t.Errorf("%s made no call but has a ledger row", unit.Unit.Key())
			}
			continue
		}
		runs++
		if !ok || row.Calls != int64(unit.Calls) || row.Runs != 1 || row.AccountID != fakeaws.EstateData {
			t.Errorf("%s: ledger %+v, run made %d calls", unit.Unit.Key(), row, unit.Calls)
		}
	}
	if ledger.writes != runs {
		t.Errorf("%d ledger writes for %d runs that made calls", ledger.writes, runs)
	}
	for key, row := range byUnit {
		metered := strings.HasSuffix(key, "/s3") || strings.HasSuffix(key, "/s3-policy") || strings.HasSuffix(key, "/kms") || strings.HasSuffix(key, "/sqs")
		switch {
		case metered && row.Metered != row.Calls:
			t.Errorf("%s: %d of %d calls metered, want all", key, row.Metered, row.Calls)
		case !metered && row.Metered != 0:
			t.Errorf("%s: %d calls metered, want none", key, row.Metered)
		}
	}
	if byUnit[model.GlobalRegion+"/s3"].Metered == 0 || byUnit[fakeaws.EstateHomeRegion+"/sqs"].Metered == 0 {
		t.Fatalf("metered rows = %+v", byUnit)
	}

	// A second run in the same hour adds to the same rows.
	mustSync(t, h, target(fakeaws.EstateData, fakeaws.EstateHomeRegion))
	for _, row := range ledger.rows(t) {
		if row.Runs != 2 {
			t.Errorf("%s/%s after two runs: %+v", row.Region, row.Source, row)
		}
	}
}

// Over its per-cycle budget a unit's per-item detail calls stop and are
// named as capped with counts ("N of M"); its lists run to the end, and
// only past five times the budget does the unit stop and keep its last
// good read.
func TestOverBudgetStopsAndRecordsCapped(t *testing.T) {
	t.Run("list past the ceiling", func(t *testing.T) {
		budgets := map[string]int{}
		h := newHarness(t, fakeaws.ScenarioLoad5k, WithOptions(func() Options { return Options{CallBudgets: budgets} }))
		lambda := map[string]bool{model.Unit{Account: productionAccount, Region: testRegion, Service: "lambda"}.Key(): true}
		if _, err := h.collector.sync(context.Background(), target(productionAccount), []Kind{KindInventory}, lambda); err != nil {
			t.Fatal(err)
		}
		good := len(resourcesByARN(t, h, productionAccount))
		budgets[SourceInventory] = 2
		before := h.fake.OperationCount("lambda:ListFunctions")
		report, err := h.collector.sync(context.Background(), target(productionAccount), []Kind{KindInventory}, lambda)
		if err != nil {
			t.Fatal(err)
		}
		if calls := h.fake.OperationCount("lambda:ListFunctions") - before; calls != listCeiling*2 {
			t.Fatalf("a budget of 2 sent %d list calls, want the ceiling of %d", calls, listCeiling*2)
		}
		unit := report.Units[0]
		if unit.Outcome != OutcomeKept || unit.ErrorCode != CodeCapped || unit.Calls != listCeiling*2 {
			t.Fatalf("capped run = %+v", unit)
		}
		if kept := len(resourcesByARN(t, h, productionAccount)); kept != good {
			t.Fatalf("the capped run replaced %d resources with %d", good, kept)
		}
		state := syncState(t, h, productionAccount, "lambda")
		capped := false
		for _, gap := range state.Gaps {
			if gap.Reason == model.GapCapped && gap.Operation == "ListFunctions" && gap.Seen == listCeiling*2 && gap.Outcome() == model.OutcomeCapped {
				capped = true
			}
		}
		if !capped || state.ErrorCode != CodeCapped {
			t.Fatalf("capped state = %+v", state)
		}
	})

	t.Run("per-item fan-out", func(t *testing.T) {
		h := newHarness(t, fakeaws.ScenarioEstateHubSpoke, WithOptions(func() Options {
			return Options{CallBudgets: map[string]int{SourceInventory: 4}}
		}))
		bucket := map[string]bool{model.Unit{Account: fakeaws.EstateData, Region: model.GlobalRegion, Service: "s3"}.Key(): true}
		before := h.fake.TotalRequests()
		report, err := h.collector.sync(context.Background(), target(fakeaws.EstateData, fakeaws.EstateHomeRegion), []Kind{KindInventory}, bucket)
		if err != nil {
			t.Fatal(err)
		}
		// The bucket list and four detail calls: the budget counts detail
		// calls only.
		if sent := h.fake.TotalRequests() - before; sent != 5 || report.Units[0].Calls != 5 {
			t.Fatalf("a budget of 4 sent %d calls (%+v)", sent, report.Units)
		}
		// Nothing was read before, so the partial read commits, with the
		// unread calls named per operation.
		state := syncState(t, h, fakeaws.EstateData, "s3")
		partial := 0
		for _, gap := range state.Gaps {
			if gap.Reason != model.GapCapped {
				t.Errorf("gap %+v is not capped", gap)
				continue
			}
			if gap.Total == 0 || gap.Seen >= gap.Total {
				t.Errorf("capped gap without counts: %+v", gap)
			}
			partial++
		}
		if partial == 0 || report.Units[0].Outcome != OutcomeCommitted {
			t.Fatalf("fan-out run = %+v, gaps %+v", report.Units[0], state.Gaps)
		}
	})
}

// The trails unit learns how many copies of each call the account's
// trails keep: the account's own trail recording reads is yes, a trail of
// writes only is no, and the organization trail another account owns is
// unknown without a gap the member could not fix.
func TestTrailsCountReadEventCopies(t *testing.T) {
	trails := func(t *testing.T, h harness, account string) map[string]model.Resource {
		t.Helper()
		result, err := h.store.Resources(context.Background(), snapshot.Query{Account: account, Service: TrailsService, Trails: true})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]model.Resource{}
		for _, resource := range result.Resources {
			out[resource.Name] = resource
		}
		return out
	}
	only := func(account, region string) map[string]bool {
		return map[string]bool{model.Unit{Account: account, Region: region, Service: TrailsService}.Key(): true}
	}

	world := newHarness(t, fakeaws.ScenarioBadDeploy)
	if _, err := world.collector.sync(context.Background(), target(productionAccount), []Kind{KindInventory}, only(productionAccount, testRegion)); err != nil {
		t.Fatal(err)
	}
	if got := trails(t, world, productionAccount); len(got) != 1 || got[fakeaws.WorldTrailName].Attributes[model.AttrReadEvents] != ReadEventsYes {
		t.Fatalf("world trails = %+v", got)
	}

	estate := newHarness(t, fakeaws.ScenarioEstateHubSpoke)
	for _, account := range []string{fakeaws.EstateWorkloads, fakeaws.EstateData} {
		report, err := estate.collector.sync(context.Background(), target(account, fakeaws.EstateHomeRegion), []Kind{KindInventory}, only(account, fakeaws.EstateHomeRegion))
		if err != nil || len(report.Units) != 1 || report.Units[0].Outcome != OutcomeCommitted {
			t.Fatalf("%s trails run = %+v, %v", account, report, err)
		}
		if gaps := syncState(t, estate, account, TrailsService).Gaps; len(gaps) != 0 {
			t.Errorf("%s: the organization trail made gaps %+v", account, gaps)
		}
	}
	workloads := trails(t, estate, fakeaws.EstateWorkloads)
	org := workloads["org-trail"]
	if org.Attributes[model.AttrReadEvents] != ReadEventsUnknown || org.Attributes[model.AttrOwnerAccount] != fakeaws.EstateManagement ||
		workloads[fakeaws.EstateAuditTrailName].Attributes[model.AttrReadEvents] != ReadEventsYes {
		t.Fatalf("workloads trails = %+v", workloads)
	}
	if data := trails(t, estate, fakeaws.EstateData); data[fakeaws.EstateWritesTrail].Attributes[model.AttrReadEvents] != ReadEventsNo {
		t.Fatalf("data trails = %+v", data)
	}
}

func TestTrailReadEventsFromSelectors(t *testing.T) {
	yes, no := true, false
	field := func(name string, equals ...string) trailtypes.AdvancedFieldSelector {
		return trailtypes.AdvancedFieldSelector{Field: &name, Equals: equals}
	}
	for name, tc := range map[string]struct {
		classic  []trailtypes.EventSelector
		advanced []trailtypes.AdvancedEventSelector
		want     string
	}{
		"nothing answered":       {want: ReadEventsUnknown},
		"all management events":  {classic: []trailtypes.EventSelector{{ReadWriteType: trailtypes.ReadWriteTypeAll, IncludeManagementEvents: &yes}}, want: ReadEventsYes},
		"reads only":             {classic: []trailtypes.EventSelector{{ReadWriteType: trailtypes.ReadWriteTypeReadOnly}}, want: ReadEventsYes},
		"writes only":            {classic: []trailtypes.EventSelector{{ReadWriteType: trailtypes.ReadWriteTypeWriteOnly, IncludeManagementEvents: &yes}}, want: ReadEventsNo},
		"data events only":       {classic: []trailtypes.EventSelector{{ReadWriteType: trailtypes.ReadWriteTypeAll, IncludeManagementEvents: &no}}, want: ReadEventsNo},
		"advanced management":    {advanced: []trailtypes.AdvancedEventSelector{{FieldSelectors: []trailtypes.AdvancedFieldSelector{field("eventCategory", "Management")}}}, want: ReadEventsYes},
		"advanced writes only":   {advanced: []trailtypes.AdvancedEventSelector{{FieldSelectors: []trailtypes.AdvancedFieldSelector{field("eventCategory", "Management"), field("readOnly", "false")}}}, want: ReadEventsNo},
		"advanced data only":     {advanced: []trailtypes.AdvancedEventSelector{{FieldSelectors: []trailtypes.AdvancedFieldSelector{field("eventCategory", "Data")}}}, want: ReadEventsNo},
		"advanced reads limited": {advanced: []trailtypes.AdvancedEventSelector{{FieldSelectors: []trailtypes.AdvancedFieldSelector{field("eventCategory", "Management"), field("readOnly", "true")}}}, want: ReadEventsYes},
	} {
		if got := trailReadEvents(tc.classic, tc.advanced); got != tc.want {
			t.Errorf("%s: %s, want %s", name, got, tc.want)
		}
	}
}

// alwaysBilled are the operations and APIs AWS bills on every call, which
// the collectors must never use: CloudWatch metric data and Contributor
// Insights reports, Logs Insights and CloudTrail Lake queries, and Athena.
var alwaysBilled = map[string]bool{
	"GetMetricData":               true,
	"NewGetMetricDataPaginator":   true,
	"GetMetricWidgetImage":        true,
	"GetInsightRuleReport":        true,
	"StartQuery":                  true,
	"GetQueryResults":             true,
	"StartQueryExecution":         true,
	"GetQueryExecution":           true,
	"StartEventDataStoreIngest":   true,
	"NewGetQueryResultsPaginator": true,
}

// alwaysBilledAPIs are client packages whose read calls are all billed
// queries.
var alwaysBilledAPIs = []string{"/service/athena", "/service/cloudwatchlogs", "/service/cloudtraildata"}

// No collector source names an always-billed operation or imports an API
// made of them, in code or in a string.
func TestCollectorsNeverCallAlwaysBilledOperations(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	positions := token.NewFileSet()
	seen := map[string]bool{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(positions, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range file.Imports {
			path, _ := strconv.Unquote(spec.Path.Value)
			for _, api := range alwaysBilledAPIs {
				if strings.HasSuffix(path, api) {
					t.Errorf("%s imports %s, whose queries are always billed", name, path)
				}
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch value := node.(type) {
			case *ast.Ident:
				seen[value.Name] = true
				if alwaysBilled[value.Name] {
					t.Errorf("%s: %s is always billed", positions.Position(value.Pos()), value.Name)
				}
			case *ast.BasicLit:
				if value.Kind != token.STRING {
					return true
				}
				text, _ := strconv.Unquote(value.Value)
				for operation := range alwaysBilled {
					if strings.Contains(text, operation) {
						t.Errorf("%s: %q names %s, which is always billed", positions.Position(value.Pos()), text, operation)
					}
				}
			}
			return true
		})
	}
	for _, called := range []string{"DescribeAlarms", "LookupEvents", "DescribeTrails"} {
		if !seen[called] {
			t.Fatalf("the scan did not see %s; it is not reading the collector sources", called)
		}
	}
}

// The default budgets are the configuration's, and each covers the widest
// unit of its source at its per-item caps, so a unit within its caps never
// stops at its budget.
func TestDefaultBudgetsCoverTheFanOutCaps(t *testing.T) {
	if got, want := DefaultCallBudgets(), config.DefaultAWSCycleBudgets(); !reflect.DeepEqual(got, want) {
		t.Fatalf("collector budgets %v, configuration budgets %v", got, want)
	}
	budgets := DefaultCallBudgets()
	for name, need := range map[string]struct {
		source string
		calls  int
	}{
		"target group health and stacks": {SourceHealth, maxTargetGroupHealth + maxStacksDescribed + 2},
		"ECS clusters":                   {SourceHealth, 2 * maxClusters},
		"bucket details":                 {SourceInventory, 3*maxBucketDetails + 1},
		"queue attributes":               {SourceInventory, maxQueueDetails + 10},
		"repository policies":            {SourcePolicies, maxRepositories + 1},
		"key policies":                   {SourcePolicies, maxKeys + 1},
	} {
		if budgets[need.source] < need.calls {
			t.Errorf("%s need %d calls, the %s budget is %d", name, need.calls, need.source, budgets[need.source])
		}
	}
}
