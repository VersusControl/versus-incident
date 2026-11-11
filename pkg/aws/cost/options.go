package cost

import (
	"errors"
	"regexp"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/VersusControl/versus-incident/pkg/aws/limit"
	"github.com/VersusControl/versus-incident/pkg/awsauth"
	"github.com/VersusControl/versus-incident/pkg/finops"
)

// UnreachablePayer is a payer whose account cannot be read now: nothing is
// fetched for it, but the cost already stored stays readable, marked stale
// with Cause (a closed code) since Since.
type UnreachablePayer struct {
	AccountID    string
	Organization bool
	Cause        string
	Since        time.Time
}

// Payer is one cost source: an account whose Cost Explorer covers its own
// cost, or the management account of an Organization, whose Cost Explorer
// covers every linked account.
type Payer struct {
	AccountID string
	// Organization marks a management (payer) account. Its lines are kept at
	// account × service level; usage-type detail comes from drill-downs.
	Organization bool
	// Credentials are the payer's assumed reader role.
	Credentials aws.CredentialsProvider
}

// Options configure the adapter for one deployment. Callers build them from
// the cloned request config; the adapter never mutates them.
type Options struct {
	// Deployment names the signal source ("aws-cost:<deployment>").
	Deployment string
	// Enabled reports whether cost data is turned on; nil means on.
	Enabled func() bool
	// DisabledReason is the closed reason shown when Enabled is false.
	DisabledReason string
	// SettingsRevision is the account settings revision read before Payers.
	// The revision only grows, whatever the clocks do: a payer removed at
	// revision r stays out of the plan under options whose revision is not
	// newer than r, even when they still name it. Zero predates every
	// removal.
	SettingsRevision uint64
	// SettingsAccounts are the account IDs the settings held at
	// SettingsRevision, read after it, whether or not cost data is on. A
	// payer whose removal left its purge pending is live again only when it
	// is listed here or in Payers and the revision is newer than its
	// tombstone. Nil means not read.
	SettingsAccounts []string
	Payers           []Payer
	// Unreachable are payers that cannot be read now. They are never
	// fetched, drilled or purged for it; reads still serve their stored cost.
	Unreachable []UnreachablePayer
	// Connected lists accounts with the full reader role (inventory and
	// change history). Other accounts seen in cost data are cost-only.
	Connected []string
	// AccountOwners and AccountNames come from config; names from Cost
	// Explorer are used when config has none.
	AccountOwners map[string]string
	AccountNames  map[string]string
	// TeamTagKey is the one owner tag key (default "team").
	TeamTagKey string
	// TagAllowlisted reports that the team tag's values may be shown. When
	// false the adapter drops tag values before they reach any view.
	TagAllowlisted bool
	// NameAllowlisted reports that the Name tag's values may be shown.
	// When false a recommendation names its resource by ID, never by the
	// Name tag value a provider returns with it.
	NameAllowlisted bool
	// MaxPaidRequests is the daily cap per deployment.
	MaxPaidRequests int
	// Location is the deployment time zone for "today".
	Location *time.Location
	// ResourceServices returns the provider service names that have an open
	// increase; resource-level detail is fetched only for them. Nil skips it.
	ResourceServices func() []string
	// RefreshHour and RefreshMinute are the daily refresh time in Location;
	// RefreshSet says they are configured (for next_refresh_at).
	RefreshHour, RefreshMinute int
	RefreshSet                 bool
	// MaxDrillDowns bounds the daily usage-type drill-downs per
	// Organization payer (default 5).
	MaxDrillDowns int
	// LineBudget bounds the stored cost lines one period read may load.
	// A period holding more is refused with ErrCostLinesBudgetExceeded
	// instead of being summarised from a partial set. Zero uses the
	// default; the totals path ignores it because it returns groups, not
	// lines.
	LineBudget int

	// Inventory is the collected inventory and change history (the
	// snapshot store) for suspect changes and inventory savings; optional.
	Inventory Inventory

	// HTTPClient is the transport for every call; tests inject a fake.
	HTTPClient aws.HTTPClient
	// Limiter paces Cost Explorer through the us-east-1 bucket; optional.
	Limiter *limit.Manager
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

const (
	defaultTeamTagKey    = "team"
	defaultMaxDrillDowns = 5
	// defaultLineBudget is sized from the worst-case probe: a cold read of
	// 201,490 lines cost about 780 B of RSS per line (325 MiB to 481 MiB on
	// the file backend), so 150k lines is about 117 MiB, 23% of the 512 MiB
	// default limit, leaving room for the rest of a request. A period
	// holding more is refused, not truncated.
	defaultLineBudget = 150_000
)

var accountPattern = regexp.MustCompile(`^[0-9]{12}$`)

// ErrInvalidPayer means a payer account ID is not a 12-digit account.
var ErrInvalidPayer = errors.New("cost: invalid payer account")

func (o Options) withDefaults() Options {
	if o.TeamTagKey == "" {
		o.TeamTagKey = defaultTeamTagKey
	}
	if o.MaxPaidRequests <= 0 {
		o.MaxPaidRequests = finops.DefaultMaxPaidRequests
	}
	if o.Location == nil {
		o.Location = time.UTC
	}
	if o.MaxDrillDowns <= 0 {
		o.MaxDrillDowns = defaultMaxDrillDowns
	}
	if o.LineBudget <= 0 {
		o.LineBudget = defaultLineBudget
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.HTTPClient == nil {
		o.HTTPClient = awsauth.NewHTTPClient()
	}
	return o
}

func (o Options) validate() error {
	for _, p := range o.Payers {
		if !accountPattern.MatchString(p.AccountID) {
			return ErrInvalidPayer
		}
	}
	for _, u := range o.Unreachable {
		if !accountPattern.MatchString(u.AccountID) {
			return ErrInvalidPayer
		}
	}
	return nil
}

func (o Options) enabled() bool { return o.Enabled == nil || o.Enabled() }

func (o Options) today() finops.Date { return finops.DateOf(o.Now().In(o.Location)) }

func (o Options) connected(account string) bool {
	for _, c := range o.Connected {
		if c == account {
			return true
		}
	}
	return false
}

// DaySettle is how long after a billing day ends its cost is treated as
// complete. Cost Explorer lags up to 24 hours and marks every day of the
// open month Estimated, so completeness comes from the clock, never from
// that flag.
const DaySettle = 24 * time.Hour

// completeThrough is the last billing day (UTC) whose end plus the settle
// time has passed at now.
func completeThrough(now time.Time, settle time.Duration) finops.Date {
	return finops.DateOf(now.UTC().Add(-settle)).AddDays(-1)
}
