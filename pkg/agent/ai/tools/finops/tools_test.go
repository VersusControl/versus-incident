package finops

import (
	"context"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"net/http"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	aitools "github.com/VersusControl/versus-incident/pkg/agent/ai/tools"
	"github.com/VersusControl/versus-incident/pkg/aws/cost"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	core_finops "github.com/VersusControl/versus-incident/pkg/finops"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

func viewerCtx() context.Context {
	return core.WithCallerAuthorization(context.Background(), core.CallerAuthorization{Authenticated: true,
		Permissions: map[core.Permission]bool{core.PermissionInfrastructureView: true}})
}

// TestFinOpsSpecMatchesRegistry: one spec table drives the tools; the
// registry catalog and the toolset list exactly the same names.
func TestFinOpsSpecMatchesRegistry(t *testing.T) {
	var catalog []string
	for _, m := range aitools.Catalog() {
		if m.Group == aitools.GroupFinOps {
			catalog = append(catalog, m.Name)
			if m.DisplayName != displayNames[m.Name] {
				t.Errorf("%s display %q, spec %q", m.Name, m.DisplayName, displayNames[m.Name])
			}
		}
	}
	if !reflect.DeepEqual(catalog, toolNames) {
		t.Fatalf("catalog %v, spec %v", catalog, toolNames)
	}
	found := false
	for _, ts := range aitools.Toolsets() {
		if ts.ID == "aws-finops" {
			found = true
			if !reflect.DeepEqual(ts.ToolNames, toolNames) || ts.Permission != core.PermissionInfrastructureView {
				t.Fatalf("toolset %+v", ts)
			}
		}
	}
	if !found {
		t.Fatal("no aws-finops toolset")
	}
	for _, tl := range New(func() core_finops.Reader { return nil }) {
		if _, ok := descriptions[tl.Name()]; !ok || tl.Description() == "" || tl.ArgsSchema()["additionalProperties"] != false {
			t.Fatalf("%s schema %+v", tl.Name(), tl.ArgsSchema())
		}
	}
}

// TestFinOpsToolsHaveNoActTools: every FinOps tool reads; none changes state
// or spends money, so the toolset is safe for Analyze and any read-only
// surface (an MCP server exposes the same registry).
func TestFinOpsToolsHaveNoActTools(t *testing.T) {
	for _, name := range toolNames {
		for _, verb := range []string{"set_", "mark_", "dismiss", "refresh", "freeze", "put_", "delete", "propose", "export", "drill"} {
			if strings.Contains(name, verb) {
				t.Errorf("%s looks like a write or paid action", name)
			}
		}
	}
	for _, m := range aitools.Catalog() {
		if m.Group == aitools.GroupFinOps && (m.Requirement.Kind != aitools.RequirementCapability || m.Requirement.Capabilities[0] != "cost_data") {
			t.Errorf("%s requirement %+v", m.Name, m.Requirement)
		}
	}
}

// TestFinOpsToolsetDescriptionBudget keeps the model-visible text small.
func TestFinOpsToolsetDescriptionBudget(t *testing.T) {
	total := 0
	for _, s := range specs {
		if len(s.Description) > 240 {
			t.Errorf("%s description has %d characters", s.Name, len(s.Description))
		}
		total += len(s.Description)
	}
	if total > 1400 {
		t.Fatalf("toolset descriptions total %d characters, budget 1400", total)
	}
}

type countingTransport struct{ n atomic.Int64 }

func (c *countingTransport) Do(*http.Request) (*http.Response, error) {
	c.n.Add(1)
	return nil, fmt.Errorf("no network in tests")
}

// TestToolsNeverCallAWS invokes every tool over the real cost adapter and
// cache: no request reaches AWS and the paid meter stays at zero.
func TestToolsNeverCallAWS(t *testing.T) {
	transport := &countingTransport{}
	provider := storage.NewMemory()
	scope := tenancy.DefaultOrgScope()
	adapter, err := cost.New(provider, scope, cost.Options{HTTPClient: transport})
	if err != nil {
		t.Fatal(err)
	}
	conn := config.AWSConnectorConfig{Enable: true, OptionalData: config.AWSOptionalDataConfig{CostExplorer: true}}
	opts := cost.FromConfig(conn, []cost.Target{{AccountID: "111111111111"}}, nil, "test")
	opts.HTTPClient = transport
	if err := adapter.Update(opts); err != nil {
		t.Fatal(err)
	}
	backend := cost.NewBackend(adapter, cost.NewStore(provider, scope), func() config.AWSConnectorConfig { return conn })
	for _, tl := range New(backend.Reader) {
		args := `{}`
		if tl.Name() == "cost_increase_detail" {
			args = `{"id":"abc"}`
		}
		if _, err := tl.Invoke(viewerCtx(), json.RawMessage(args)); err != nil && !strings.Contains(err.Error(), "invalid") {
			t.Fatalf("%s: %v", tl.Name(), err)
		}
	}
	if transport.n.Load() != 0 || adapter.Meter().Used(context.Background()) != 0 {
		t.Fatalf("tools reached AWS %d times, meter %d", transport.n.Load(), adapter.Meter().Used(context.Background()))
	}
	for _, f := range mustGlob(t, "*.go") {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		pf, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range pf.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if strings.Contains(p, "aws-sdk-go") || strings.Contains(p, "/pkg/aws") {
				t.Errorf("%s imports %s: tools must reach cost through the FinOps service only", f, p)
			}
		}
	}
}

func mustGlob(t *testing.T, pattern string) []string {
	files, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// bigSource is a large deployment: many accounts, services, teams and days.
type bigSource struct{ lines []core_finops.CostLine }

var bigLast = core_finops.MustDate("2026-10-31")

func newBigSource() *bigSource {
	s := &bigSource{}
	start := core_finops.MustDate("2026-08-01")
	for a := range 40 {
		account := fmt.Sprintf("1000000000%02d", a)
		for svc := range 12 {
			for d := start; !d.After(bigLast); d = d.AddDays(1) {
				amt := core_finops.USD(int64(10 + svc))
				if d.Month == 10 && d.Day > 10 && svc%3 == 0 {
					amt *= 6
				}
				s.lines = append(s.lines, core_finops.CostLine{Day: d, Account: account, Region: "us-east-1",
					Service: fmt.Sprintf("service-with-a-rather-long-name-%02d", svc), ServiceName: strings.Repeat("Name ", 20),
					UsageType: fmt.Sprintf("USE1-Usage-%02d", svc), RecordType: core_finops.RecordUsage,
					Team: fmt.Sprintf("team-%02d", (a+svc)%15), NetAmortizedMicros: amt, QuantityMicros: 1_000_000})
			}
		}
	}
	return s
}

func (s *bigSource) Status(context.Context) (core_finops.SourceStatus, error) {
	var accounts []core_finops.Account
	for a := range 40 {
		accounts = append(accounts, core_finops.Account{ID: fmt.Sprintf("1000000000%02d", a), Name: strings.Repeat("Account ", 10), Readable: a != 3, ErrorCode: "ce_denied"})
	}
	return core_finops.SourceStatus{Enabled: true, Currency: "USD", LastCompleteDay: bigLast, HistoryStart: core_finops.MustDate("2026-08-01"),
		Accounts: accounts, TeamTag: core_finops.TagStatus{Key: "team", Active: true, Allowlisted: true}, Stale: true, StaleReason: "throttled",
		SkippedCalls: []string{"forecast"}, UpdatedAt: time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)}, nil
}

func (s *bigSource) Lines(_ context.Context, r core_finops.DateRange) ([]core_finops.CostLine, error) {
	var out []core_finops.CostLine
	for _, l := range s.lines {
		if r.Contains(l.Day) {
			out = append(out, l)
		}
	}
	return out, nil
}

func (s *bigSource) ResourceLines(context.Context, core_finops.DateRange) ([]core_finops.CostLine, error) {
	return nil, nil
}

func (s *bigSource) SavingsInputs(context.Context) (core_finops.SavingsInputs, error) {
	var in core_finops.SavingsInputs
	for i := range 40 {
		in.Idle = append(in.Idle, core_finops.IdleResource{Kind: core_finops.IdleUnattachedVolume, ID: fmt.Sprintf("vol-%d", i),
			Account: fmt.Sprintf("1000000000%02d", i), Region: "us-east-1", Monthly: core_finops.USD(int64(10 + i))})
	}
	return in, nil
}

// TestEveryFinOpsModelViewStaysWithinBudget: each tool's view is at most
// 6 KiB and carries sync, ref and links.ui, naming gaps instead of zeroes.
// Views list the increases a sync stored, as after a deployment's refresh,
// so the fixture has a store and syncs once before reading.
func TestEveryFinOpsModelViewStaysWithinBudget(t *testing.T) {
	src := newBigSource()
	now := time.Date(2026, 11, 6, 9, 30, 0, 0, time.UTC)
	store := cost.NewStore(storage.NewMemory(), tenancy.DefaultOrgScope())
	svc := core_finops.NewService(src, store, core_finops.Config{UIPath: "/aws/finops", Now: func() time.Time { return now }})
	if _, err := svc.SyncIncreases(context.Background()); err != nil {
		t.Fatalf("sync increases: %v", err)
	}
	tools := New(func() core_finops.Reader { return svc })
	incs, err := svc.Increases(context.Background(), core_finops.Query{Period: "2026-10", Status: "all"})
	if err != nil || len(incs.Items) == 0 {
		t.Fatalf("increases %d %v", len(incs.Items), err)
	}
	for _, tl := range tools {
		args := `{"period":"2026-10","group_by":"account"}`
		switch tl.Name() {
		case "cost_increase_detail":
			args = `{"period":"2026-10","id":"` + incs.Items[0].ID + `"}`
		case "cost_increases":
			args = `{"period":"2026-10","status":"all"}`
		case "cost_drivers", "savings_opportunities", "budget_status", "cost_accounts":
			args = `{"period":"2026-10"}`
		}
		res, err := tl.Invoke(viewerCtx(), json.RawMessage(args))
		if err != nil {
			t.Fatalf("%s: %v", tl.Name(), err)
		}
		b, _ := json.Marshal(res)
		if len(b) > MaxModelViewBytes+256 {
			t.Errorf("%s result has %d bytes", tl.Name(), len(b))
		}
		d := res.Data
		sync, _ := d["sync"].(map[string]any)
		links, _ := d["links"].(map[string]any)
		if sync == nil || sync["stale"] != true || sync["accounts_readable"] != 39 || links["ui"] == "" || !strings.HasPrefix(d["ref"].(string), "finops://"+tl.Name()) {
			t.Errorf("%s lacks sync/ref/links: %v", tl.Name(), d)
		}
	}
}

func TestToolsRefuseCallersWithoutInfrastructureView(t *testing.T) {
	svc := core_finops.NewService(newBigSource(), nil, core_finops.Config{})
	for _, tl := range New(func() core_finops.Reader { return svc }) {
		res, err := tl.Invoke(context.Background(), json.RawMessage(`{}`))
		if err != nil || res.IsAvailable() {
			t.Fatalf("%s answered an unauthorized caller", tl.Name())
		}
	}
	tl := New(func() core_finops.Reader { return svc })[0]
	if _, err := tl.Invoke(viewerCtx(), json.RawMessage(`{"period":"2026-10","unknown":1}`)); err == nil {
		t.Fatal("unknown arguments are refused")
	}
	disabled := core_finops.NewService(&disabledSource{}, nil, core_finops.Config{UIPath: "/aws/finops"})
	res, err := New(func() core_finops.Reader { return disabled })[0].Invoke(viewerCtx(), json.RawMessage(`{}`))
	if err != nil || res.IsAvailable() || res.Data["reason"] != "ce_unavailable" {
		t.Fatalf("disabled source %+v %v", res, err)
	}
}

type disabledSource struct{ bigSource }

func (*disabledSource) Status(context.Context) (core_finops.SourceStatus, error) {
	return core_finops.SourceStatus{Enabled: false, DisabledReason: "ce_unavailable"}, nil
}
