package cost

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go/parser"
	"go/token"

	"github.com/VersusControl/versus-incident/pkg/awsauth"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/finops"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	"github.com/VersusControl/versus-incident/tests/fixtures/storagefix"
)

func TestSignalSourceAlertsEachIncreaseOnce(t *testing.T) {
	sc := loadScenario(t, "single-account")
	h := newHarness(t, sc, nil)
	if _, err := h.adapter.Refresh(ctx, KindScheduled); err != nil {
		t.Fatal(err)
	}
	store := NewStore(h.provider, h.adapter.cache.scope)
	svc := finops.NewService(h.adapter.Source(), store, finops.Config{Now: h.clock.Now})
	src := NewSignalSource("prod", svc, h.clock.Now)
	if src.Name() != "aws-cost:prod" {
		t.Fatal(src.Name())
	}
	before := h.fake.requests()
	since := at("2026-11-01", 0)
	sigs, cursor, err := src.Pull(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	if h.fake.requests() != before {
		t.Fatal("pulling increases must read the cache only")
	}
	if cursor.After(h.clock.Now()) || cursor.Before(since) {
		t.Fatalf("cursor %v", cursor)
	}
	var nat *core.Signal
	for i := range sigs {
		if sigs[i].Fields[core.FieldService] == "network.nat" {
			nat = &sigs[i]
		}
	}
	if nat == nil || nat.Source != "aws-cost:prod" || nat.Fields[core.FieldSignal] != SignalIncrease || nat.Fields["started_on"] != "2026-10-25" {
		t.Fatalf("signals %+v", sigs)
	}
	if !strings.Contains(nat.Message, "went up about") || nat.Raw != nil {
		t.Fatalf("message %q", nat.Message)
	}
	for _, v := range nat.Fields {
		if s, ok := v.(string); ok && s == "checkout" {
			t.Fatal("tag values never travel in a signal")
		}
	}
	// Not yet committed: a crash before the worker stores it re-delivers.
	again, _, _ := src.Pull(ctx, cursor)
	if len(again) != len(sigs) {
		t.Fatalf("uncommitted increases are re-delivered: %d vs %d", len(again), len(sigs))
	}
	if err := src.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	after, _, err := src.Pull(ctx, cursor)
	if err != nil || len(after) != 0 {
		t.Fatalf("alerted once: %d %v", len(after), err)
	}
}

type failingSyncer struct{ marks int }

func (f *failingSyncer) SyncIncreases(context.Context) ([]finops.IncreaseRecord, error) {
	return []finops.IncreaseRecord{{Detected: finops.Detected{ID: "a"}}, {Detected: finops.Detected{ID: "b"}}}, nil
}

func (f *failingSyncer) MarkIncreaseAlerted(_ context.Context, id string) error {
	f.marks++
	if id == "b" && f.marks < 3 {
		return errors.New("store unavailable")
	}
	return nil
}

func TestSignalCommitKeepsUnrecordedAlerts(t *testing.T) {
	f := &failingSyncer{}
	src := NewSignalSource("", f, func() time.Time { return at("2026-11-09", 10) })
	if _, _, err := src.Pull(ctx, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := src.Commit(ctx); err == nil {
		t.Fatal("commit reports the failure")
	}
	if err := src.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if f.marks != 3 || src.Name() != "aws-cost:default" {
		t.Fatalf("marks %d", f.marks)
	}
}

func TestStoreScopesWritesAndReads(t *testing.T) {
	p := storagefix.Provider(t)
	shared := NewStore(p, tenancy.DefaultOrgScope())
	if err := shared.PutIncrease(ctx, finops.IncreaseRecord{Detected: finops.Detected{ID: "inc1", StartedOn: finops.MustDate("2026-10-09")}, Status: finops.StatusOpen}); err != nil {
		t.Fatal(err)
	}
	ent := NewStore(p, tenancy.NewOrgScope("acme", tenancy.DefaultOrgID))
	if err := ent.PutIncrease(ctx, finops.IncreaseRecord{Detected: finops.Detected{ID: "inc1", StartedOn: finops.MustDate("2026-10-09")}, Status: finops.StatusExpected}); err != nil {
		t.Fatal(err)
	}
	if err := ent.PutIncrease(ctx, finops.IncreaseRecord{Detected: finops.Detected{ID: "inc2", StartedOn: finops.MustDate("2026-10-20")}, Status: finops.StatusOpen}); err != nil {
		t.Fatal(err)
	}
	got, _ := ent.ListIncreases(ctx)
	if len(got) != 2 || got[0].Status != finops.StatusExpected {
		t.Fatalf("write org wins and default is read: %+v", got)
	}
	if def, _ := shared.ListIncreases(ctx); len(def) != 1 || def[0].Status != finops.StatusOpen {
		t.Fatalf("the default org never sees enterprise writes: %+v", def)
	}
	tables, _ := storage.FinOps(p)
	if rows, err := tables.FinOpsIncreases(ctx, tenancy.NewOrgScope("acme"), storage.FinOpsIncreaseQuery{IDs: []string{"inc2"}}); err != nil || len(rows) != 1 {
		t.Fatalf("increases are written to the write organization's table: %+v %v", rows, err)
	}
	b1 := finops.Budget{ID: "b1", Name: "Total", Scope: finops.ScopeDeployment, Monthly: finops.USD(10), Source: finops.BudgetFromUI}
	b2 := finops.Budget{ID: "b2", Name: "Team", Scope: finops.ScopeTeam, Value: "checkout", Monthly: finops.USD(5), Source: finops.BudgetFromUI}
	if err := shared.PutBudgets(ctx, []finops.Budget{b1, b2}); err != nil {
		t.Fatal(err)
	}
	if err := shared.PutBudgets(ctx, []finops.Budget{b2}); err != nil {
		t.Fatal(err)
	}
	if bs, _ := shared.Budgets(ctx); len(bs) != 1 || bs[0].ID != "b2" {
		t.Fatalf("budgets replaced: %+v", bs)
	}
	if err := shared.PutIncrease(ctx, finops.IncreaseRecord{Detected: finops.Detected{ID: "../x"}}); !errors.Is(err, ErrInvalidID) {
		t.Fatal("unsafe ids are refused")
	}
	if r, err := shared.GetReport(ctx, "2026-10"); r != nil || err != nil {
		t.Fatal("missing report is nil")
	}
}

type passThrough struct{ hits int }

func (p *passThrough) Do(*http.Request) (*http.Response, error) {
	p.hits++
	return &http.Response{StatusCode: 200, Body: http.NoBody, ContentLength: maxResponseBytes + 1}, nil
}

func TestPinnedClientStaysOnItsHost(t *testing.T) {
	inner := &passThrough{}
	pinned := pinnedHTTPClient{client: inner, host: costExplorerURL}
	for _, u := range []string{"https://evil.example.com/", "http://" + costExplorerURL + "/", "https://ce.us-west-2.amazonaws.com/"} {
		req, _ := http.NewRequest(http.MethodPost, u, nil)
		if _, err := pinned.Do(req); !errors.Is(err, awsauth.ErrNetwork) {
			t.Fatalf("%s: %v", u, err)
		}
	}
	if inner.hits != 0 {
		t.Fatal("a refused request was sent")
	}
	req, _ := http.NewRequest(http.MethodPost, "https://"+costExplorerURL+"/", nil)
	if _, err := pinned.Do(req); !errors.Is(err, awsauth.ErrNetwork) {
		t.Fatal("an oversized body is refused")
	}
	if cfg := (env{}).config(); cfg.RetryMaxAttempts != 1 || cfg.Region != billingRegion {
		t.Fatalf("config %+v", cfg)
	}
}

func TestMappingIsExact(t *testing.T) {
	for _, c := range []struct{ svc, ut, region string }{
		{"EC2 - Other", "USE2-NatGateway-Bytes", "us-east-2"},
		{"EC2 - Other", "NatGateway-Bytes", "us-east-1"},
		{"Amazon Elastic Compute Cloud - Compute", "APN1-BoxUsage:t3.micro", "ap-northeast-1"},
		{"Amazon Route 53", "HostedZone", finops.GlobalRegion},
		{"AWS Support (Business)", "", finops.GlobalRegion},
		{"Amazon Simple Storage Service", "", finops.UnknownRegion},
	} {
		if got := RegionOf(c.svc, c.ut); got != c.region {
			t.Errorf("%s %s → %s, want %s", c.svc, c.ut, got, c.region)
		}
	}
	if k, g := ServiceKey("EC2 - Other", "USE1-NatGateway-Bytes"); k != "network.nat" || g != "NAT data processing" {
		t.Fatal(k, g)
	}
	if k, _ := ServiceKey("EC2 - Other", "USE1-EBS:VolumeUsage.gp3"); k != "storage.block" {
		t.Fatal(k)
	}
	if k, _ := ServiceKey("Some New Service", ""); k != "Some New Service" {
		t.Fatal(k)
	}
	if TagValue("team$checkout") != "checkout" || TagValue("team$") != "" || TagValue("nokey") != "" {
		t.Fatal("tag value")
	}
	parts := splitWeights(-1_000_001, 7, []weight{{"a", 1}, {"b", 1}, {"c", 1}})
	var sum, qty int64
	for _, p := range parts {
		sum += p.amount
		qty += p.qty
	}
	if sum != -1_000_001 || qty != 7 {
		t.Fatalf("split must sum exactly: %+v", parts)
	}
	// A tag outside the allowlist never reaches a line.
	day := finops.MustDate("2026-11-01")
	tbl := &dayTables{
		Account: []cell{{A: "111111111111", B: "EC2 - Other", M: 100}},
		Usage:   []cell{{A: "EC2 - Other", B: "USE1-NatGateway-Bytes", M: 100, Q: 5}},
		Team:    []cell{{A: "team$secret", B: "EC2 - Other", M: 60}, {A: "team$", B: "EC2 - Other", M: 40}},
	}
	for _, l := range buildLines(day, tbl, synthOptions{payer: "111111111111"}) {
		if l.Team != "" {
			t.Fatalf("hidden tag value leaked: %+v", l)
		}
	}
	ls := buildLines(day, tbl, synthOptions{payer: "111111111111", keepTeams: true})
	if len(ls) != 2 || ls[0].NetAmortizedMicros+ls[1].NetAmortizedMicros != 100 {
		t.Fatalf("team split %+v", ls)
	}
}

// TestPackageUsesOnlyPinnedReadOnlyClients keeps the adapter on the billing
// and optimization clients, without the ambient config loader, and only on read
// operations.
func TestPackageUsesOnlyPinnedReadOnlyClients(t *testing.T) {
	allowed := map[string]bool{
		"github.com/aws/aws-sdk-go-v2/service/costexplorer":              true,
		"github.com/aws/aws-sdk-go-v2/service/costexplorer/types":        true,
		"github.com/aws/aws-sdk-go-v2/service/budgets":                   true,
		"github.com/aws/aws-sdk-go-v2/service/budgets/types":             true,
		"github.com/aws/aws-sdk-go-v2/service/costoptimizationhub":       true,
		"github.com/aws/aws-sdk-go-v2/service/costoptimizationhub/types": true,
		"github.com/aws/aws-sdk-go-v2/service/computeoptimizer":          true,
		"github.com/aws/aws-sdk-go-v2/service/computeoptimizer/types":    true,
	}
	files, _ := filepath.Glob("*.go")
	ops := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		pf, err := parser.ParseFile(token.NewFileSet(), f, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range pf.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if p == "github.com/aws/aws-sdk-go-v2/config" {
				t.Errorf("%s loads ambient config", f)
			}
			if strings.HasPrefix(p, "github.com/aws/aws-sdk-go-v2/service/") && !allowed[p] {
				t.Errorf("%s imports %s", f, p)
			}
		}
		for _, op := range []string{"GetCostAndUsage", "GetCostAndUsageWithResources", "GetCostForecast", "GetSavingsPlansCoverage", "ListCostAllocationTags", "DescribeBudgets", "ListRecommendations", "GetEC2InstanceRecommendations"} {
			if strings.Contains(string(src), "client."+op+"(") {
				ops[op] = true
			}
		}
	}
	if len(ops) != 8 {
		t.Fatalf("operations found %v", ops)
	}
}
