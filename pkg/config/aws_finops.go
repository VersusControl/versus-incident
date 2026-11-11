package config

import (
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// AWSFinOpsConfig tunes the FinOps page for the deployment's AWS cost. Cost
// data is read only when connectors.aws.optional_data.cost_explorer is on.
// One deployment has one budget set; there are no per-organization budgets.
type AWSFinOpsConfig struct {
	TeamTagKey             string                  `mapstructure:"team_tag_key"`
	MinIncreaseUSDPerMonth int                     `mapstructure:"min_increase_usd_per_month"`
	MinDriverUSD           int                     `mapstructure:"min_driver_usd"`
	TargetCoverage         float64                 `mapstructure:"target_coverage"`
	MaxPaidRequestsPerDay  int                     `mapstructure:"max_paid_requests_per_day"`
	RefreshTime            string                  `mapstructure:"refresh_time"`
	CostSourceAccount      string                  `mapstructure:"cost_source_account"`
	Budgets                []AWSFinOpsBudgetConfig `mapstructure:"budgets"`
	AccountGroups          []AWSFinOpsAccountGroup `mapstructure:"account_groups"`
	Report                 AWSFinOpsReportConfig   `mapstructure:"report"`
}

// AWSFinOpsBudgetConfig is one monthly budget for the deployment, one
// account (value = account ID) or one team (value = team tag value).
type AWSFinOpsBudgetConfig struct {
	Name       string `mapstructure:"name"`
	Scope      string `mapstructure:"scope"`
	Value      string `mapstructure:"value"`
	MonthlyUSD int    `mapstructure:"monthly_usd"`
}

// AWSFinOpsAccountGroup names a group of accounts ("Production").
type AWSFinOpsAccountGroup struct {
	Name     string   `mapstructure:"name"`
	Accounts []string `mapstructure:"accounts"`
}

// AWSFinOpsReportConfig controls the monthly report.
type AWSFinOpsReportConfig struct {
	Timezone     string `mapstructure:"timezone"`
	SlackSummary bool   `mapstructure:"slack_summary"`
}

// FinOps defaults.
const (
	DefaultFinOpsTeamTagKey      = "team"
	DefaultFinOpsMinIncreaseUSD  = 100
	DefaultFinOpsMinDriverUSD    = 50
	DefaultFinOpsTargetCoverage  = 0.8
	DefaultFinOpsMaxPaidRequests = 20
	DefaultFinOpsRefreshTime     = "09:00"
	DefaultFinOpsTimezone        = "UTC"
	maxFinOpsPaidRequests        = 1000
	maxFinOpsBudgets             = 50
	maxFinOpsAccountGroups       = 64
	maxFinOpsUSD                 = 1_000_000_000
	awsFinOpsEnvironmentPrefix   = "VERSUS_CONNECTORS_AWS_FINOPS_"
)

var (
	finopsRefreshPattern = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
	finopsTagKeyPattern  = regexp.MustCompile(`^[A-Za-z0-9 _.:/=+\-@]{1,128}$`)
)

// normalizeAWSFinOpsDefaults fills unset values.
func normalizeAWSFinOpsDefaults(f *AWSFinOpsConfig) {
	if strings.TrimSpace(f.TeamTagKey) == "" {
		f.TeamTagKey = DefaultFinOpsTeamTagKey
	}
	if f.MinIncreaseUSDPerMonth == 0 {
		f.MinIncreaseUSDPerMonth = DefaultFinOpsMinIncreaseUSD
	}
	if f.MinDriverUSD == 0 {
		f.MinDriverUSD = DefaultFinOpsMinDriverUSD
	}
	if f.TargetCoverage == 0 {
		f.TargetCoverage = DefaultFinOpsTargetCoverage
	}
	if f.MaxPaidRequestsPerDay == 0 {
		f.MaxPaidRequestsPerDay = DefaultFinOpsMaxPaidRequests
	}
	if strings.TrimSpace(f.RefreshTime) == "" {
		f.RefreshTime = DefaultFinOpsRefreshTime
	}
	if strings.TrimSpace(f.Report.Timezone) == "" {
		f.Report.Timezone = DefaultFinOpsTimezone
	}
}

// validateAWSFinOps checks the block. Messages name the field, never echo
// the value.
func validateAWSFinOps(f AWSFinOpsConfig) error {
	const p = "connectors.aws.finops."
	if !finopsTagKeyPattern.MatchString(f.TeamTagKey) {
		return errors.New(p + "team_tag_key is not a valid tag key")
	}
	if f.MinIncreaseUSDPerMonth < 1 || f.MinIncreaseUSDPerMonth > maxFinOpsUSD {
		return errors.New(p + "min_increase_usd_per_month must be a positive whole amount")
	}
	if f.MinDriverUSD < 1 || f.MinDriverUSD > maxFinOpsUSD {
		return errors.New(p + "min_driver_usd must be a positive whole amount")
	}
	if math.IsNaN(f.TargetCoverage) || f.TargetCoverage <= 0 || f.TargetCoverage > 1 {
		return errors.New(p + "target_coverage must be above 0 and at most 1")
	}
	if f.MaxPaidRequestsPerDay < 1 || f.MaxPaidRequestsPerDay > maxFinOpsPaidRequests {
		return errors.New(p + "max_paid_requests_per_day must be between 1 and 1000")
	}
	if !finopsRefreshPattern.MatchString(f.RefreshTime) {
		return errors.New(p + "refresh_time must be HH:MM")
	}
	if f.CostSourceAccount != "" && (len(f.CostSourceAccount) != 12 || !digitsOnly(f.CostSourceAccount)) {
		return errors.New(p + "cost_source_account must be a 12-digit account ID")
	}
	if _, err := time.LoadLocation(f.Report.Timezone); err != nil {
		return errors.New(p + "report.timezone is not a known time zone")
	}
	if len(f.Budgets) > maxFinOpsBudgets {
		return errors.New(p + "budgets must contain at most 50 entries")
	}
	seen := map[string]bool{}
	for i, b := range f.Budgets {
		fail := func(reason string) error { return fmt.Errorf("%sbudgets[%d]: %s", p, i, reason) }
		if strings.TrimSpace(b.Name) == "" || len(b.Name) > 128 {
			return fail("name is required (at most 128 characters)")
		}
		switch b.Scope {
		case "deployment":
			if b.Value != "" {
				return fail("deployment scope takes no value")
			}
		case "account":
			if len(b.Value) != 12 || !digitsOnly(b.Value) {
				return fail("account scope needs a 12-digit account ID as value")
			}
		case "team":
			if strings.TrimSpace(b.Value) == "" || len(b.Value) > 256 {
				return fail("team scope needs a team tag value")
			}
		default:
			// One deployment, one budget set: no organization scope.
			return fail("scope must be deployment, account or team")
		}
		if b.MonthlyUSD < 1 || b.MonthlyUSD > maxFinOpsUSD {
			return fail("monthly_usd must be a positive whole amount")
		}
		key := b.Scope + "|" + b.Value
		if seen[key] {
			return fail("duplicates another budget scope")
		}
		seen[key] = true
	}
	if len(f.AccountGroups) > maxFinOpsAccountGroups {
		return errors.New(p + "account_groups must contain at most 64 entries")
	}
	grouped := map[string]bool{}
	for i, g := range f.AccountGroups {
		if strings.TrimSpace(g.Name) == "" || len(g.Name) > 128 {
			return fmt.Errorf("%saccount_groups[%d]: name is required", p, i)
		}
		for _, a := range g.Accounts {
			if len(a) != 12 || !digitsOnly(a) {
				return fmt.Errorf("%saccount_groups[%d]: accounts must be 12-digit account IDs", p, i)
			}
			if grouped[a] {
				return fmt.Errorf("%saccount_groups[%d]: an account belongs to one group", p, i)
			}
			grouped[a] = true
		}
	}
	return nil
}

// applyAWSFinOpsEnvironment applies VERSUS_CONNECTORS_AWS_FINOPS_* scalar
// overrides on top of the file and validates the result. An invalid
// override fails the load without echoing the value.
func applyAWSFinOpsEnvironment(f *AWSFinOpsConfig) error {
	env := func(name string) (string, bool) {
		v := strings.TrimSpace(os.Getenv(awsFinOpsEnvironmentPrefix + name))
		return v, v != ""
	}
	// Without a connectors file the block was never decoded: fill the
	// defaults first, so an override changes one value and an invalid
	// override (such as 0) is still refused.
	for _, name := range []string{"TEAM_TAG_KEY", "REFRESH_TIME", "COST_SOURCE_ACCOUNT", "REPORT_TIMEZONE",
		"MIN_INCREASE_USD_PER_MONTH", "MIN_DRIVER_USD", "MAX_PAID_REQUESTS_PER_DAY", "TARGET_COVERAGE", "REPORT_SLACK_SUMMARY"} {
		if _, ok := env(name); ok {
			normalizeAWSFinOpsDefaults(f)
			break
		}
	}
	set := false
	for name, target := range map[string]*string{
		"TEAM_TAG_KEY":        &f.TeamTagKey,
		"REFRESH_TIME":        &f.RefreshTime,
		"COST_SOURCE_ACCOUNT": &f.CostSourceAccount,
		"REPORT_TIMEZONE":     &f.Report.Timezone,
	} {
		if v, ok := env(name); ok {
			*target, set = v, true
		}
	}
	for name, target := range map[string]*int{
		"MIN_INCREASE_USD_PER_MONTH": &f.MinIncreaseUSDPerMonth,
		"MIN_DRIVER_USD":             &f.MinDriverUSD,
		"MAX_PAID_REQUESTS_PER_DAY":  &f.MaxPaidRequestsPerDay,
	} {
		if v, ok := env(name); ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("%s%s must be a whole number", awsFinOpsEnvironmentPrefix, name)
			}
			*target, set = n, true
		}
	}
	if v, ok := env("TARGET_COVERAGE"); ok {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("%sTARGET_COVERAGE must be a number", awsFinOpsEnvironmentPrefix)
		}
		f.TargetCoverage, set = n, true
	}
	if v, ok := env("REPORT_SLACK_SUMMARY"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("%sREPORT_SLACK_SUMMARY must be true or false", awsFinOpsEnvironmentPrefix)
		}
		f.Report.SlackSummary, set = b, true
	}
	if !set {
		return nil
	}
	return validateAWSFinOps(*f)
}

// cloneAWSFinOpsConfig deep-copies the block for a per-request config.
func cloneAWSFinOpsConfig(src AWSFinOpsConfig) AWSFinOpsConfig {
	out := src
	out.Budgets = append([]AWSFinOpsBudgetConfig(nil), src.Budgets...)
	if src.AccountGroups != nil {
		out.AccountGroups = make([]AWSFinOpsAccountGroup, len(src.AccountGroups))
		for i, g := range src.AccountGroups {
			out.AccountGroups[i] = AWSFinOpsAccountGroup{Name: g.Name, Accounts: append([]string(nil), g.Accounts...)}
		}
	}
	return out
}

// CostDataEnabled reports whether FinOps reads cost: the AWS connector and
// its Cost Explorer opt-in are both on.
func (connector AWSConnectorConfig) CostDataEnabled() bool {
	return connector.Enable && connector.OptionalData.CostExplorer
}

// RefreshClock returns the daily refresh hour and minute.
func (f AWSFinOpsConfig) RefreshClock() (int, int) {
	h, m := 9, 0
	if finopsRefreshPattern.MatchString(f.RefreshTime) {
		h, _ = strconv.Atoi(f.RefreshTime[:2])
		m, _ = strconv.Atoi(f.RefreshTime[3:])
	}
	return h, m
}

// Location is the report time zone, UTC when unknown.
func (f AWSFinOpsConfig) Location() *time.Location {
	if loc, err := time.LoadLocation(f.Report.Timezone); err == nil {
		return loc
	}
	return time.UTC
}

// Effective returns the block with defaults filled, for callers reading a
// config that was never decoded from a file.
func (f AWSFinOpsConfig) Effective() AWSFinOpsConfig {
	normalizeAWSFinOpsDefaults(&f)
	return f
}
