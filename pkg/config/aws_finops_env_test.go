package config

import (
	"os"
	"path/filepath"
	"testing"
)

// emptyConfigDir writes a config with no connectors block.
func emptyConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	main := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(main, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return main
}

// TestFinOpsOverridesLoadWithoutAConnectorsBlock: each override alone on an
// empty config starts with the defaults filled, not a validation error.
func TestFinOpsOverridesLoadWithoutAConnectorsBlock(t *testing.T) {
	main := emptyConfigDir(t)
	for name, value := range map[string]string{
		"TEAM_TAG_KEY": "owner", "MIN_INCREASE_USD_PER_MONTH": "300", "MIN_DRIVER_USD": "60", "TARGET_COVERAGE": "0.9",
		"MAX_PAID_REQUESTS_PER_DAY": "12", "REFRESH_TIME": "06:15", "COST_SOURCE_ACCOUNT": "123456789012",
		"REPORT_TIMEZONE": "Europe/Berlin", "REPORT_SLACK_SUMMARY": "true",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(awsFinOpsEnvironmentPrefix+name, value)
			loaded, err := loadConfigFromPath(main)
			if err != nil {
				t.Fatalf("%s alone: %v", name, err)
			}
			f := loaded.Connectors.AWS.FinOps
			if f.TeamTagKey == "" || f.MaxPaidRequestsPerDay == 0 || f.RefreshTime == "" || f.Report.Timezone == "" {
				t.Fatalf("defaults not filled: %+v", f)
			}
		})
	}
	t.Setenv(awsFinOpsEnvironmentPrefix+"MAX_PAID_REQUESTS_PER_DAY", "12")
	loaded, err := loadConfigFromPath(main)
	if err != nil || loaded.Connectors.AWS.FinOps.MaxPaidRequestsPerDay != 12 || loaded.Connectors.AWS.FinOps.TeamTagKey != DefaultFinOpsTeamTagKey {
		t.Fatalf("override kept, defaults filled: %+v %v", loaded.Connectors.AWS.FinOps, err)
	}
}

// TestCostExplorerSwitchEnvironmentOverride: the FinOps switch follows the
// same env pattern as the other AWS flags.
func TestCostExplorerSwitchEnvironmentOverride(t *testing.T) {
	main := emptyConfigDir(t)
	if err := os.WriteFile(filepath.Join(filepath.Dir(main), "connectors.yaml"),
		[]byte("connectors:\n  aws:\n    enable: true\n    optional_data:\n      cost_explorer: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VERSUS_CONNECTORS_AWS_OPTIONAL_DATA_COST_EXPLORER", "true")
	loaded, err := loadConfigFromPath(main)
	if err != nil || !loaded.Connectors.AWS.CostDataEnabled() {
		t.Fatalf("on: %v", err)
	}
	t.Setenv("VERSUS_CONNECTORS_AWS_OPTIONAL_DATA_COST_EXPLORER", "false")
	loaded, err = loadConfigFromPath(main)
	if err != nil || loaded.Connectors.AWS.CostDataEnabled() {
		t.Fatalf("off: %v", err)
	}
}

// TestHelmChartCostExplorerSwitch renders the chart's switch both ways.
func TestHelmChartCostExplorerSwitch(t *testing.T) {
	requireRenderableChart(t)
	for want, args := range map[bool][]string{
		false: {"--set", "connectors.aws.enable=true"},
		true:  {"--set", "connectors.aws.enable=true", "--set", "connectors.aws.optionalData.costExplorer=true"},
	} {
		files := renderChartFiles(t, chartScenario{name: "aws-cost-switch", args: args})
		dir := t.TempDir()
		for name, body := range files {
			if filepath.Ext(name) == ".yaml" {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		loaded, err := loadConfigFromPath(filepath.Join(dir, "config.yaml"))
		if err != nil || loaded.Connectors.AWS.CostDataEnabled() != want {
			t.Fatalf("chart switch %v: %v", want, err)
		}
	}
}
