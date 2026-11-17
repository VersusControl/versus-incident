package cost

import (
	"math"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/finops"
)

// Reasons the page gives when cost cannot be read at all.
const (
	ReasonCostDataOff           = "cost_data_off"
	ReasonCostSourceNotLinked   = "cost_source_not_connected"
	ReasonNoConnectedAccounts   = "no_connected_accounts"
	providerName                = "AWS"
	uiPath                      = "/aws/finops"
	billCloseDayOfNextMonth     = billCloseDay
	coverageBasisPointsPerUnit  = 10_000
	maxConfigTagAllowlistLength = 256
	// nameTagKey is the tag AWS shows as a resource's name.
	nameTagKey = "Name"
)

// Target is a connected account the adapter may read cost through, with
// its assumed reader role.
type Target struct {
	AccountID   string
	Credentials aws.CredentialsProvider
	// Cause is set when the account cannot be read now (for example its
	// credentials stopped working): it is a closed code, Credentials are
	// nil, and Since says from when. Its stored cost stays readable.
	Cause string
	Since time.Time
}

// FromConfig builds the adapter options from the cloned AWS connector
// configuration, the connected accounts and the tag allowlist in effect.
// With cost_source_account set, that account is the Organization payer and
// must be connected; otherwise every connected account reads its own cost.
func FromConfig(cfg config.AWSConnectorConfig, targets []Target, allowlist []string, deployment string) Options {
	f := cfg.FinOps.Effective()
	o := Options{
		Deployment:      deployment,
		TeamTagKey:      f.TeamTagKey,
		MaxPaidRequests: f.MaxPaidRequestsPerDay,
		Location:        f.Location(),
		AccountOwners:   map[string]string{},
		AccountNames:    map[string]string{},
		RefreshSet:      true,
	}
	o.RefreshHour, o.RefreshMinute = f.RefreshClock()
	for _, key := range allowlist {
		if len(allowlist) > maxConfigTagAllowlistLength {
			break
		}
		// AWS tag keys are case-sensitive: an allowlist entry matches a key
		// exactly, as in every other tag-value redaction.
		if strings.TrimSpace(key) == f.TeamTagKey {
			o.TagAllowlisted = true
		}
		if strings.TrimSpace(key) == nameTagKey {
			o.NameAllowlisted = true
		}
	}
	for _, a := range cfg.Accounts {
		if a.Owner != "" {
			o.AccountOwners[a.AccountID] = a.Owner
		}
		if a.DisplayName != "" {
			o.AccountNames[a.AccountID] = a.DisplayName
		}
	}
	reason := ""
	switch {
	case !cfg.CostDataEnabled():
		reason = ReasonCostDataOff
	case f.CostSourceAccount != "":
		for _, t := range targets {
			switch {
			case t.AccountID != f.CostSourceAccount:
			case t.Cause != "":
				o.Unreachable = []UnreachablePayer{{AccountID: t.AccountID, Organization: true, Cause: t.Cause, Since: t.Since}}
			default:
				o.Payers = []Payer{{AccountID: t.AccountID, Organization: true, Credentials: t.Credentials}}
			}
		}
		if len(o.Payers) == 0 {
			reason = ReasonCostSourceNotLinked
		}
	default:
		for _, t := range targets {
			switch {
			case !accountPattern.MatchString(t.AccountID):
			case t.Cause != "":
				o.Unreachable = append(o.Unreachable, UnreachablePayer{AccountID: t.AccountID, Cause: t.Cause, Since: t.Since})
			default:
				o.Payers = append(o.Payers, Payer{AccountID: t.AccountID, Credentials: t.Credentials})
			}
		}
		if len(o.Payers) == 0 {
			reason = ReasonNoConnectedAccounts
		}
	}
	for _, t := range targets {
		o.Connected = append(o.Connected, t.AccountID)
	}
	o.DisabledReason = reason
	o.Enabled = func() bool { return reason == "" }
	return o
}

// FinOpsConfig maps the connector configuration onto the FinOps core.
func FinOpsConfig(cfg config.AWSConnectorConfig) finops.Config {
	f := cfg.FinOps.Effective()
	out := finops.Config{
		MinIncrease:      finops.USD(int64(f.MinIncreaseUSDPerMonth)),
		MinDriver:        finops.USD(int64(f.MinDriverUSD)),
		TargetCoverageBP: int64(math.Round(f.TargetCoverage * coverageBasisPointsPerUnit)),
		MaxPaidRequests:  f.MaxPaidRequestsPerDay,
		AccountOwners:    map[string]string{},
		AccountGroups:    map[string]string{},
		ProviderName:     providerName,
		UIPath:           uiPath,
		BillCloseDay:     billCloseDayOfNextMonth,
		Location:         f.Location(),
	}
	for _, b := range f.Budgets {
		out.Budgets = append(out.Budgets, finops.Budget{
			ID: finops.BudgetID(b.Scope, b.Value), Name: b.Name, Scope: b.Scope, Value: b.Value,
			Monthly: finops.USD(int64(b.MonthlyUSD)), Source: finops.BudgetFromConfig,
		})
	}
	for _, a := range cfg.Accounts {
		if a.Owner != "" {
			out.AccountOwners[a.AccountID] = a.Owner
		}
	}
	for _, g := range f.AccountGroups {
		for _, a := range g.Accounts {
			out.AccountGroups[a] = g.Name
		}
	}
	return out
}
