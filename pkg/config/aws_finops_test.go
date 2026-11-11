package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// finopsConnectorsYAML is a connectors.yaml with a full FinOps block, read by
// the loader tests.
const finopsConnectorsYAML = `connectors:
  aws:
    enable: true
    optional_data:
      cost_explorer: true
    accounts:
      - account_id: "111122223333"
        display_name: Production
        owner: platform
        regions: auto
        auth:
          mode: default_chain
    finops:
      team_tag_key: squad
      min_increase_usd_per_month: 250
      min_driver_usd: 75
      target_coverage: 0.7
      max_paid_requests_per_day: 30
      refresh_time: "07:30"
      cost_source_account: "999988887777"
      budgets:
        - {name: AWS total, scope: deployment, monthly_usd: 42000}
        - {name: Checkout, scope: team, value: checkout, monthly_usd: 5000}
      account_groups:
        - {name: Production, accounts: ["111122223333"]}
      report:
        timezone: Europe/Berlin
        slack_summary: true
`

// finopsHelmValuesYAML is Helm values exercising every connectors.aws.finops
// key and an account owner.
const finopsHelmValuesYAML = `connectors:
  aws:
    enable: true
    optionalData:
      costExplorer: true
    accounts:
      - accountId: "111122223333"
        displayName: Production
        owner: platform
        auth:
          mode: default_chain
    finops:
      teamTagKey: squad
      minIncreaseUsdPerMonth: 250
      minDriverUsd: 75
      targetCoverage: 0.7
      maxPaidRequestsPerDay: 30
      refreshTime: "07:30"
      costSourceAccount: "999988887777"
      budgets:
        - {name: AWS total, scope: deployment, monthlyUsd: 42000}
        - {name: Checkout, scope: team, value: checkout, monthlyUsd: 5000}
        - {name: Production, scope: account, value: "111122223333", monthlyUsd: 30000}
      accountGroups:
        - {name: Production, accounts: ["111122223333"]}
      report:
        timezone: Europe/Berlin
        slackSummary: true
`

// writeTestFile writes content to name in a fresh temporary folder and
// returns its path.
func writeTestFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func wantFinOps() AWSFinOpsConfig {
	return AWSFinOpsConfig{
		TeamTagKey: "squad", MinIncreaseUSDPerMonth: 250, MinDriverUSD: 75, TargetCoverage: 0.7,
		MaxPaidRequestsPerDay: 30, RefreshTime: "07:30", CostSourceAccount: "999988887777",
		Budgets: []AWSFinOpsBudgetConfig{
			{Name: "AWS total", Scope: "deployment", MonthlyUSD: 42000},
			{Name: "Checkout", Scope: "team", Value: "checkout", MonthlyUSD: 5000},
		},
		AccountGroups: []AWSFinOpsAccountGroup{{Name: "Production", Accounts: []string{"111122223333"}}},
		Report:        AWSFinOpsReportConfig{Timezone: "Europe/Berlin", SlackSummary: true},
	}
}

func TestAWSFinOpsConfigLoadsEveryKey(t *testing.T) {
	got, err := loadConnectorsFile(writeTestFile(t, "connectors.yaml", finopsConnectorsYAML))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.AWS.FinOps, wantFinOps()) {
		t.Fatalf("finops = %+v", got.AWS.FinOps)
	}
	if got.AWS.Accounts[0].Owner != "platform" || !got.AWS.CostDataEnabled() {
		t.Fatalf("aws = %+v", got.AWS)
	}
	if h, m := got.AWS.FinOps.RefreshClock(); h != 7 || m != 30 || got.AWS.FinOps.Location().String() != "Europe/Berlin" {
		t.Fatal("refresh clock or location")
	}
}

func TestAWSFinOpsDefaults(t *testing.T) {
	got, err := decodeConnectors(map[string]any{"aws": map[string]any{"enable": true}})
	if err != nil {
		t.Fatal(err)
	}
	f := got.AWS.FinOps
	if f.TeamTagKey != "team" || f.MinIncreaseUSDPerMonth != 100 || f.MinDriverUSD != 50 || f.TargetCoverage != 0.8 ||
		f.MaxPaidRequestsPerDay != 20 || f.RefreshTime != "09:00" || f.CostSourceAccount != "" || f.Report.Timezone != "UTC" {
		t.Fatalf("defaults = %+v", f)
	}
	if got.AWS.CostDataEnabled() {
		t.Fatal("cost data is opt-in")
	}
	var zero AWSFinOpsConfig
	if e := zero.Effective(); e.MaxPaidRequestsPerDay != 20 || e.TeamTagKey != "team" {
		t.Fatalf("effective defaults = %+v", e)
	}
}

func finopsDecode(finops map[string]any) error {
	_, err := decodeConnectors(map[string]any{"aws": map[string]any{"enable": true, "finops": finops}})
	return err
}

func TestAWSFinOpsStrictShapeRejectsPerOrgAndBadValues(t *testing.T) {
	cases := map[string]map[string]any{
		"unknown key":              {"org_budgets": []any{}},
		"per-org budget scope":     {"budgets": []any{map[string]any{"name": "Acme", "scope": "org", "monthly_usd": 10}}},
		"per-org budget key":       {"budgets": []any{map[string]any{"name": "Acme", "scope": "deployment", "org": "acme", "monthly_usd": 10}}},
		"budgets keyed by org":     {"budgets": map[string]any{"acme": []any{}}},
		"deployment with value":    {"budgets": []any{map[string]any{"name": "x", "scope": "deployment", "value": "a", "monthly_usd": 10}}},
		"account without id":       {"budgets": []any{map[string]any{"name": "x", "scope": "account", "value": "abc", "monthly_usd": 10}}},
		"duplicate scope":          {"budgets": []any{map[string]any{"name": "a", "scope": "deployment", "monthly_usd": 1}, map[string]any{"name": "b", "scope": "deployment", "monthly_usd": 2}}},
		"zero budget":              {"budgets": []any{map[string]any{"name": "a", "scope": "deployment", "monthly_usd": -1}}},
		"refresh time":             {"refresh_time": "9am"},
		"cost source":              {"cost_source_account": "12345"},
		"coverage above one":       {"target_coverage": 1.5},
		"cap":                      {"max_paid_requests_per_day": 5000},
		"time zone":                {"report": map[string]any{"timezone": "Mars/Base"}},
		"account in two groups":    {"account_groups": []any{map[string]any{"name": "a", "accounts": []any{"111122223333"}}, map[string]any{"name": "b", "accounts": []any{"111122223333"}}}},
		"invalid account in group": {"account_groups": []any{map[string]any{"name": "a", "accounts": []any{"x"}}}},
	}
	for name, block := range cases {
		if err := finopsDecode(block); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := finopsDecode(map[string]any{"target_coverage": 1, "max_paid_requests_per_day": "25"}); err != nil {
		t.Fatalf("whole-number coverage and string ints decode: %v", err)
	}
}

func TestAWSFinOpsEnvironmentOverrides(t *testing.T) {
	f := wantFinOps()
	for k, v := range map[string]string{
		"TEAM_TAG_KEY": "owner", "MIN_INCREASE_USD_PER_MONTH": "300", "MIN_DRIVER_USD": "60", "TARGET_COVERAGE": "0.9",
		"MAX_PAID_REQUESTS_PER_DAY": "12", "REFRESH_TIME": "06:15", "COST_SOURCE_ACCOUNT": "123456789012",
		"REPORT_TIMEZONE": "UTC", "REPORT_SLACK_SUMMARY": "false",
	} {
		t.Setenv(awsFinOpsEnvironmentPrefix+k, v)
	}
	if err := applyAWSFinOpsEnvironment(&f); err != nil {
		t.Fatal(err)
	}
	if f.TeamTagKey != "owner" || f.MinIncreaseUSDPerMonth != 300 || f.MinDriverUSD != 60 || f.TargetCoverage != 0.9 ||
		f.MaxPaidRequestsPerDay != 12 || f.RefreshTime != "06:15" || f.CostSourceAccount != "123456789012" ||
		f.Report.Timezone != "UTC" || f.Report.SlackSummary {
		t.Fatalf("env = %+v", f)
	}
	t.Setenv(awsFinOpsEnvironmentPrefix+"MAX_PAID_REQUESTS_PER_DAY", "lots-secret")
	err := applyAWSFinOpsEnvironment(&f)
	if err == nil || strings.Contains(err.Error(), "lots-secret") {
		t.Fatalf("invalid override must fail without echo: %v", err)
	}
	t.Setenv(awsFinOpsEnvironmentPrefix+"MAX_PAID_REQUESTS_PER_DAY", "0")
	if err := applyAWSFinOpsEnvironment(&f); err == nil {
		t.Fatal("an out-of-range override is validated")
	}
}

func TestAWSFinOpsCloneIsDeep(t *testing.T) {
	src := ConnectorsConfig{AWS: AWSConnectorConfig{FinOps: wantFinOps()}}
	out := cloneConnectorsConfig(src)
	out.AWS.FinOps.Budgets[0].Name = "changed"
	out.AWS.FinOps.AccountGroups[0].Accounts[0] = "000000000000"
	if src.AWS.FinOps.Budgets[0].Name != "AWS total" || src.AWS.FinOps.AccountGroups[0].Accounts[0] != "111122223333" {
		t.Fatal("clone shares FinOps slices with the source")
	}
}

// TestHelmChartRendersAWSFinOps proves the chart renders every FinOps key
// into a config the loader accepts unchanged, and that its defaults are the
// loader's defaults.
func TestHelmChartRendersAWSFinOps(t *testing.T) {
	requireRenderableChart(t)
	load := func(name string, args ...string) *Config {
		files := renderChartFiles(t, chartScenario{name: name, args: args})
		dir := t.TempDir()
		for file, body := range files {
			if strings.HasSuffix(file, ".yaml") {
				if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		loaded, err := loadConfigFromPath(filepath.Join(dir, "config.yaml"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return loaded
	}
	full := load("aws-finops", "-f", writeTestFile(t, "values.yaml", finopsHelmValuesYAML))
	want := wantFinOps()
	want.Budgets = append(want.Budgets, AWSFinOpsBudgetConfig{Name: "Production", Scope: "account", Value: "111122223333", MonthlyUSD: 30000})
	if !reflect.DeepEqual(full.Connectors.AWS.FinOps, want) {
		t.Fatalf("chart finops = %+v", full.Connectors.AWS.FinOps)
	}
	if full.Connectors.AWS.Accounts[0].Owner != "platform" || !full.Connectors.AWS.CostDataEnabled() {
		t.Fatalf("chart aws = %+v", full.Connectors.AWS)
	}
	defaults := load("aws-finops-defaults", "--set", "connectors.aws.enable=true")
	var zero AWSFinOpsConfig
	got := defaults.Connectors.AWS.FinOps
	if len(got.Budgets) == 0 && len(got.AccountGroups) == 0 {
		got.Budgets, got.AccountGroups = nil, nil
	}
	if !reflect.DeepEqual(got, zero.Effective()) {
		t.Fatalf("chart defaults %+v differ from the loader's %+v", defaults.Connectors.AWS.FinOps, zero.Effective())
	}
}
