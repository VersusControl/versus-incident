// Package finopsfix builds the cloud-neutral FinOps reference story that the
// FinOps core tests and the FinOps API tests share.
//
// It imports no product code: the FinOps core's own tests live inside the
// core package and cannot import a package that imports it. The types follow
// the JSON shape of the core's cost lines, source status, savings inputs and
// budgets, so a consumer converts the story with one JSON round trip into the
// core types.
package finopsfix

import "time"

// Account IDs of the reference story.
const (
	ProdAccount    = "111111111111"
	DataAccount    = "222222222222"
	SandboxAccount = "333333333333"
)

// Story is the reference cost world before it is expanded into daily lines.
type Story struct {
	// SeriesRange is the inclusive day range Series are expanded over.
	SeriesRange DateRange `json:"series_range"`
	Series      []Series  `json:"series"`
	// Lines are single-day lines added as they are.
	Lines []Line `json:"lines"`
	// ResourceRange is the inclusive day range every Resources line repeats
	// on.
	ResourceRange DateRange          `json:"resource_range"`
	Resources     []Line             `json:"resources"`
	Status        Status             `json:"status"`
	Savings       Savings            `json:"savings"`
	Config        Config             `json:"config"`
	Suspects      map[string]Suspect `json:"suspects"`
}

// DateRange is an inclusive day range.
type DateRange struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// Series is one cost line whose daily cost and quantity step over time.
type Series struct {
	Line  Line   `json:"line"`
	Steps []Step `json:"steps"`
}

// Step sets a series' daily cost and quantity from a day on.
type Step struct {
	From     string `json:"from"`
	Cost     int64  `json:"cost_micros"`
	Quantity int64  `json:"quantity_micros"`
}

// Line is one cost line; amounts are in micro-units.
type Line struct {
	Day         string `json:"day,omitempty"`
	Account     string `json:"account"`
	Region      string `json:"region"`
	Service     string `json:"service"`
	ServiceName string `json:"service_name,omitempty"`
	UsageType   string `json:"usage_type,omitempty"`
	UsageGroup  string `json:"usage_group,omitempty"`
	RecordType  string `json:"record_type"`
	ResourceID  string `json:"resource_id,omitempty"`
	Team        string `json:"team,omitempty"`
	Cost        int64  `json:"net_amortized_micros"`
	Quantity    int64  `json:"quantity_micros"`
}

// Status is the cost source's status. Its field names are the core's, which
// encodes them without JSON tags.
type Status struct {
	Enabled         bool
	Currency        string
	LastCompleteDay string
	HistoryStart    string
	UpdatedAt       time.Time
	Accounts        []Account
	MeterUsed       int
	MeterCap        int
	Forecast        *Forecast
	TeamTag         TagStatus
	ResourceData    bool
}

// Account is one account the source reads.
type Account struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Readable  bool   `json:"readable"`
	FirstCost string `json:"first_cost,omitempty"`
	ErrorCode string `json:"error_code,omitempty"`
}

// Forecast is the provider's month forecast.
type Forecast struct {
	Month string `json:"month"`
	Mean  int64  `json:"mean_micros"`
	Low   int64  `json:"low_micros"`
	High  int64  `json:"high_micros"`
}

// TagStatus is the team tag's state.
type TagStatus struct {
	Key         string `json:"key"`
	Active      bool   `json:"active"`
	Allowlisted bool   `json:"allowlisted"`
}

// Savings are the savings inputs.
type Savings struct {
	Idle            []Idle           `json:"idle,omitempty"`
	Recommendations []Recommendation `json:"recommendations,omitempty"`
	NetworkPaths    []NetworkPath    `json:"network_paths,omitempty"`
	Coverage        *Coverage        `json:"coverage,omitempty"`
	Buckets         []Bucket         `json:"buckets,omitempty"`
}

// Idle is an idle or unattached resource.
type Idle struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Account string `json:"account"`
	Region  string `json:"region"`
	Monthly int64  `json:"monthly_micros"`
	Basis   string `json:"basis,omitempty"`
}

// Recommendation is a provider rightsizing recommendation.
type Recommendation struct {
	ResourceID string `json:"resource_id"`
	Account    string `json:"account"`
	Region     string `json:"region"`
	Current    string `json:"current"`
	Target     string `json:"target"`
	Monthly    int64  `json:"monthly_micros"`
	Source     string `json:"source"`
}

// NetworkPath is one network's NAT processing to managed services.
type NetworkPath struct {
	Network      string            `json:"network"`
	Account      string            `json:"account"`
	Region       string            `json:"region"`
	Team         string            `json:"team,omitempty"`
	Processing   map[string]int64  `json:"processing_micros"`
	HasEndpoint  map[string]bool   `json:"has_endpoint"`
	EndpointCost map[string]int64  `json:"endpoint_cost_micros"`
	TargetNames  map[string]string `json:"target_names,omitempty"`
}

// Coverage is commitment coverage and the purchase that closes the gap.
type Coverage struct {
	CoveredBP        int64  `json:"covered_bp"`
	Suggestion       string `json:"suggestion,omitempty"`
	EstimatedMonthly int64  `json:"estimated_monthly_micros"`
}

// Bucket is object storage metadata.
type Bucket struct {
	Name             string `json:"name"`
	Account          string `json:"account"`
	Region           string `json:"region"`
	HasLifecycle     bool   `json:"has_lifecycle"`
	StandardBytes    int64  `json:"standard_bytes"`
	EstimatedMonthly int64  `json:"estimated_monthly_micros"`
}

// Config is the FinOps service configuration of the story.
type Config struct {
	Budgets       []Budget          `json:"budgets"`
	AccountOwners map[string]string `json:"account_owners"`
	ProviderName  string            `json:"provider_name"`
	UIPath        string            `json:"ui_path"`
	Now           time.Time         `json:"now"`
}

// Budget is a monthly budget.
type Budget struct {
	Name    string `json:"name"`
	Scope   string `json:"scope"`
	Monthly int64  `json:"monthly_micros"`
}

// Suspect is the change named behind an increase, with its usage signal.
type Suspect struct {
	Change SuspectChange `json:"change"`
	Signal *UsageSignal  `json:"signal,omitempty"`
}

// SuspectChange is the suspected change.
type SuspectChange struct {
	State      string    `json:"state"`
	Summary    string    `json:"summary,omitempty"`
	Actor      string    `json:"actor,omitempty"`
	At         time.Time `json:"at"`
	Confidence string    `json:"confidence,omitempty"`
	Ref        string    `json:"ref,omitempty"`
}

// UsageSignal is a usage metric before and after an increase.
type UsageSignal struct {
	Label  string `json:"label"`
	Before int64  `json:"before_micros"`
	After  int64  `json:"after_micros"`
	Unit   string `json:"unit"`
}

// usd is whole dollars in micro-units.
func usd(dollars int64) int64 { return dollars * 1_000_000 }

// NATStory is the reference scenario: a NAT step on Oct 9 after a release, a
// new untagged database replica, a storage price change, a one-off support
// fee, credits, a steady compute line, a tiny logs line, one unreadable
// account, and a partial day after the last complete day. The clock reads
// Nov 6, 2026; October is the last closed month.
func NATStory() Story {
	usEast1 := func(account, service, usageType, team string) Line {
		return Line{Account: account, Region: "us-east-1", Service: service, UsageType: usageType, RecordType: "usage", Team: team}
	}
	nat := usEast1(ProdAccount, "network.nat", "USE1-NatGateway-Bytes", "checkout")
	natSeries := nat
	natSeries.ServiceName, natSeries.UsageGroup = "EC2 - Other", "NAT data processing"
	compute := usEast1(ProdAccount, "compute.vm", "USE1-BoxUsage:m6i.large", "checkout")
	compute.UsageGroup = "instances"
	storage := usEast1(ProdAccount, "storage.object", "TimedStorage-ByteHrs", "platform")
	storage.Region = "global"
	day := func(day string, l Line, cost, quantity int64) Line {
		l.Day, l.Cost, l.Quantity = day, cost, quantity
		return l
	}
	resource := func(id string, cost int64) Line {
		l := nat
		l.ResourceID, l.Cost = id, cost
		return l
	}
	charge := func(service, record string) Line {
		return Line{Account: ProdAccount, Region: "us-east-1", Service: service, RecordType: record}
	}
	unattached := func(id string) Idle {
		return Idle{Kind: "unattached_volume", ID: id, Account: ProdAccount, Region: "us-east-1", Monthly: 53_000_000}
	}
	return Story{
		SeriesRange: DateRange{Start: "2026-08-01", End: "2026-10-31"},
		Series: []Series{
			{Line: natSeries, Steps: []Step{
				{From: "2026-08-01", Cost: usd(20), Quantity: 1_000_000_000},
				{From: "2026-10-09", Cost: usd(85), Quantity: 4_250_000_000},
			}},
			{Line: compute, Steps: []Step{{From: "2026-08-01", Cost: usd(300), Quantity: 2_400_000_000}}},
			{Line: storage, Steps: []Step{
				{From: "2026-08-01", Cost: usd(50), Quantity: 2_000_000_000},
				{From: "2026-10-01", Cost: usd(55), Quantity: 2_000_000_000},
			}},
			{Line: usEast1(ProdAccount, "observability.logs", "USE1-DataProcessing-Bytes", "platform"),
				Steps: []Step{{From: "2026-08-01", Cost: usd(1), Quantity: 1_000_000}}},
			{Line: usEast1(DataAccount, "database.relational", "USE1-RDS:StorageUsage", ""),
				Steps: []Step{{From: "2026-08-01", Cost: usd(10), Quantity: 100_000_000}}},
			{Line: usEast1(DataAccount, "database.relational", "USE1-InstanceUsage:db.r6g.xlarge", ""),
				Steps: []Step{{From: "2026-10-20", Cost: usd(40), Quantity: 24_000_000}}},
		},
		Lines: []Line{
			day("2026-10-01", charge("billing.support", "fee"), usd(1_200), 0),
			day("2026-10-15", charge("billing.credit", "credit"), -usd(500), 0),
			day("2026-11-01", nat, usd(500), 25_000_000_000),
		},
		ResourceRange: DateRange{Start: "2026-10-18", End: "2026-10-31"},
		Resources:     []Line{resource("nat-1a", usd(84)), resource("nat-1b", usd(1))},
		Status: Status{
			Enabled:         true,
			Currency:        "USD",
			LastCompleteDay: "2026-10-31",
			HistoryStart:    "2026-08-01",
			UpdatedAt:       time.Date(2026, time.November, 6, 8, 0, 0, 0, time.UTC),
			Accounts: []Account{
				{ID: ProdAccount, Name: "Production", Kind: "connected", Readable: true, FirstCost: "2026-08-01"},
				{ID: DataAccount, Name: "Data", Kind: "connected", Readable: true, FirstCost: "2026-08-01"},
				{ID: SandboxAccount, Name: "Sandbox", Kind: "cost_only", Readable: false, ErrorCode: "denied"},
			},
			MeterUsed:    7,
			MeterCap:     20,
			Forecast:     &Forecast{Month: "2026-11", Mean: usd(15_000), Low: usd(14_200), High: usd(15_900)},
			TeamTag:      TagStatus{Key: "team", Active: true, Allowlisted: true},
			ResourceData: true,
		},
		Savings: Savings{
			Idle: []Idle{unattached("vol-1"), unattached("vol-2"), unattached("vol-3"), unattached("vol-4")},
			NetworkPaths: []NetworkPath{{
				Network: "vpc-prod", Account: ProdAccount, Region: "us-east-1", Team: "checkout",
				Processing:   map[string]int64{"object_storage": usd(1_200), "container_registry": usd(400)},
				HasEndpoint:  map[string]bool{},
				EndpointCost: map[string]int64{"container_registry": usd(80)},
				TargetNames:  map[string]string{"object_storage": "S3", "container_registry": "ECR"},
			}},
			Coverage: &Coverage{CoveredBP: 7_100, Suggestion: "a 1-year compute commitment of $4/h", EstimatedMonthly: usd(610)},
			Buckets: []Bucket{{Name: "data-lake-raw", Account: DataAccount, Region: "us-east-1", StandardBytes: 5_497_558_138_880,
				EstimatedMonthly: usd(300)}},
		},
		Config: Config{
			Budgets:       []Budget{{Name: "Cloud total", Scope: "deployment", Monthly: usd(14_000)}},
			AccountOwners: map[string]string{DataAccount: "data platform"},
			ProviderName:  "AWS",
			UIPath:        "/aws/finops",
			Now:           time.Date(2026, time.November, 6, 9, 30, 0, 0, time.UTC),
		},
		Suspects: map[string]Suspect{
			"network.nat": {
				Change: SuspectChange{State: "found", Summary: "the checkout-api release", Actor: "github-actions-prod",
					At: time.Date(2026, time.October, 9, 14, 2, 0, 0, time.UTC), Confidence: "likely", Ref: "change:ecs-checkout-api-42"},
				Signal: &UsageSignal{Label: "NAT bytes processed", Before: 600_000, After: 2_500_000, Unit: "TB/day"},
			},
		},
	}
}
