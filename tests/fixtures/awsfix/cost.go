// Package awsfix builds the AWS billing worlds that the cost adapter tests and
// the fakeaws server share: daily Cost Explorer series, AWS Budgets, Cost
// Optimization Hub and Compute Optimizer findings, and the inventory and
// change history a cost story is explained with.
//
// It imports no product code, so fakeaws stays independent of the product and
// any test package can use it. The JSON tags follow the shape the cost tests
// decode into their own types, so a consumer converts a scenario with one JSON
// round trip.
package awsfix

import (
	"sort"
	"strconv"
	"time"
)

// CostScenario is one billing world.
type CostScenario struct {
	// Name is the scenario's data-set name ("cost-nat-spike").
	Name     string    `json:"-"`
	Currency string    `json:"currency"`
	Range    DateRange `json:"range"`
	// PartialFrom is the first day Cost Explorer marks Estimated.
	PartialFrom string `json:"partial_from,omitempty"`
	// PageGroups caps the groups per Cost Explorer page (0 = no paging).
	PageGroups int `json:"page_groups,omitempty"`
	// Names are the account display names; a payer named "Management"
	// sees every account's lines.
	Names     map[string]string `json:"names"`
	Series    []CostSeries      `json:"series"`
	Forecast  Forecast          `json:"forecast"`
	Coverage  Coverage          `json:"coverage"`
	TagActive bool              `json:"tag_active"`
	Budgets   []Budget          `json:"budgets"`
	// ExpectedBudgets and ExpectedSkipped are what the budget import must
	// make of Budgets: name to scope, and name to skip reason.
	ExpectedBudgets  map[string]string      `json:"expected_budgets,omitempty"`
	ExpectedSkipped  map[string]string      `json:"expected_skipped_budgets,omitempty"`
	Recommendations  []HubRecommendation    `json:"recommendations"`
	ComputeOptimizer []ComputeOptimizerItem `json:"compute_optimizer,omitempty"`
	// Inventory, Changes and ChangeCoverage seed the snapshot store.
	Inventory      []Resource `json:"inventory,omitempty"`
	Changes        []Change   `json:"changes,omitempty"`
	ChangeCoverage []Interval `json:"change_coverage,omitempty"`
}

// DateRange is a half-open day range: End is the first day not billed.
type DateRange struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// CostSeries is one daily cost line; each step sets the daily cost and
// quantity from its day on. A day before the first step bills nothing.
type CostSeries struct {
	Account    string     `json:"account"`
	Service    string     `json:"service"`
	UsageType  string     `json:"usage_type,omitempty"`
	RecordType string     `json:"record_type"`
	Team       string     `json:"team,omitempty"`
	Unit       string     `json:"unit,omitempty"`
	ResourceID string     `json:"resource_id,omitempty"`
	Steps      []CostStep `json:"steps"`
}

// CostStep is a decimal daily cost and usage quantity from a day on.
type CostStep struct {
	From     string `json:"from"`
	Cost     string `json:"cost"`
	Quantity string `json:"quantity,omitempty"`
}

// Forecast is the Cost Explorer month forecast.
type Forecast struct {
	Mean string `json:"mean"`
	Low  string `json:"low"`
	High string `json:"high"`
}

// Coverage is the Savings Plans coverage answer.
type Coverage struct {
	Covered string `json:"covered"`
	Total   string `json:"total"`
}

// Budget is one AWS Budgets DescribeBudgets entry.
type Budget struct {
	BudgetName       string              `json:"BudgetName"`
	BudgetType       string              `json:"BudgetType"`
	TimeUnit         string              `json:"TimeUnit"`
	BudgetLimit      Spend               `json:"BudgetLimit"`
	Metrics          []string            `json:"Metrics,omitempty"`
	CostFilters      map[string][]string `json:"CostFilters,omitempty"`
	CostTypes        *CostTypes          `json:"CostTypes,omitempty"`
	FilterExpression *Expression         `json:"FilterExpression,omitempty"`
}

// Spend is an amount with its unit.
type Spend struct {
	Amount string `json:"Amount"`
	Unit   string `json:"Unit"`
}

// CostTypes is the legacy budget cost-type block.
type CostTypes struct {
	UseAmortized bool `json:"UseAmortized"`
}

// Expression is a budget filter expression.
type Expression struct {
	Or         []Expression `json:"Or,omitempty"`
	Dimensions *KeyValues   `json:"Dimensions,omitempty"`
	Tags       *KeyValues   `json:"Tags,omitempty"`
}

// KeyValues matches one key against a value list.
type KeyValues struct {
	Key    string   `json:"Key"`
	Values []string `json:"Values"`
}

// HubRecommendation is one Cost Optimization Hub ListRecommendations item.
type HubRecommendation struct {
	AccountID                  string   `json:"accountId"`
	ActionType                 string   `json:"actionType"`
	CurrentResourceType        string   `json:"currentResourceType,omitempty"`
	ResourceID                 string   `json:"resourceId"`
	Region                     string   `json:"region"`
	CurrentResourceSummary     string   `json:"currentResourceSummary,omitempty"`
	RecommendedResourceSummary string   `json:"recommendedResourceSummary,omitempty"`
	EstimatedMonthlySavings    float64  `json:"estimatedMonthlySavings"`
	Tags                       []HubTag `json:"tags,omitempty"`
}

// HubTag is a resource tag on a hub recommendation.
type HubTag struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// ComputeOptimizerItem is one Compute Optimizer EC2 recommendation.
type ComputeOptimizerItem struct {
	Account     string  `json:"account"`
	InstanceARN string  `json:"instance_arn"`
	Name        string  `json:"name"`
	Current     string  `json:"current"`
	Target      string  `json:"target"`
	Monthly     float64 `json:"monthly"`
	Finding     string  `json:"finding"`
}

// Resource is one inventory resource in the collectors' normalized shape.
type Resource struct {
	ARN        string            `json:"arn"`
	Service    string            `json:"service"`
	Type       string            `json:"type"`
	Account    string            `json:"account"`
	Region     string            `json:"region"`
	App        string            `json:"app,omitempty"`
	Name       string            `json:"name"`
	State      string            `json:"state,omitempty"`
	Tags       map[string]string `json:"tags,omitempty"`
	Relations  []Relation        `json:"relations,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

// Relation links a resource to another by ARN.
type Relation struct {
	Type   string `json:"type"`
	Target string `json:"target"`
}

// Change is one recorded write event.
type Change struct {
	ID          string    `json:"id"`
	Time        time.Time `json:"time"`
	Account     string    `json:"account"`
	Region      string    `json:"region"`
	Service     string    `json:"service"`
	EventSource string    `json:"event_source"`
	EventName   string    `json:"event_name"`
	Template    string    `json:"template"`
	Summary     string    `json:"summary"`
	Resources   []string  `json:"resources,omitempty"`
	Actor       Actor     `json:"actor"`
	ErrorCode   string    `json:"error_code,omitempty"`
}

// Actor is who made a change.
type Actor struct {
	Class         string `json:"class"`
	PrincipalType string `json:"principal_type,omitempty"`
	PrincipalARN  string `json:"principal_arn,omitempty"`
}

// Interval is a span the change history covers.
type Interval struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// Cost data-set names.
const (
	CostNatSpikeName        = "cost-nat-spike"
	CostNatSpikeTwoNATsName = "cost-nat-spike-two-nats"
	CostNewUntaggedDBName   = "cost-new-untagged-db"
	SingleAccountName       = "single-account"
	OrganizationName        = "organization"
	LargeOrganizationName   = "large-organization"
)

var costScenarios = map[string]func() *CostScenario{
	CostNatSpikeName:        CostNatSpike,
	CostNatSpikeTwoNATsName: CostNatSpikeTwoNATs,
	CostNewUntaggedDBName:   CostNewUntaggedDB,
	SingleAccountName:       SingleAccount,
	OrganizationName:        Organization,
	LargeOrganizationName:   LargeOrganization,
}

// CostScenarioNamed builds the named data set; ok is false for an unknown
// name. Every call returns a fresh value the caller may change.
func CostScenarioNamed(name string) (scenario *CostScenario, ok bool) {
	build, ok := costScenarios[name]
	if !ok {
		return nil, false
	}
	return build(), true
}

// CostScenarioNames lists every data set, sorted.
func CostScenarioNames() []string {
	names := make([]string, 0, len(costScenarios))
	for name := range costScenarios {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// series is a compact constructor for one cost series.
func series(account, service, usageType, recordType, team, unit string, steps ...CostStep) CostSeries {
	return CostSeries{Account: account, Service: service, UsageType: usageType, RecordType: recordType,
		Team: team, Unit: unit, Steps: steps}
}

// step sets a daily cost and quantity from a day on.
func step(from, cost, quantity string) CostStep {
	return CostStep{From: from, Cost: cost, Quantity: quantity}
}

// fee is a one-day charge on day that drops back to zero the next day.
func fee(day, cost string) []CostStep {
	next := mustDay(day).AddDate(0, 0, 1).Format(time.DateOnly)
	return []CostStep{{From: day, Cost: cost}, {From: next, Cost: "0"}}
}

func mustDay(value string) time.Time {
	day, err := time.Parse(time.DateOnly, value)
	if err != nil {
		panic(err)
	}
	return day
}

func mustTime(value string) time.Time {
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return at
}

// linkedAccounts returns count consecutive twelve-digit account IDs from
// first.
func linkedAccounts(first int64, count int) []string {
	out := make([]string, count)
	for index := range out {
		out[index] = strconv.FormatInt(first+int64(index), 10)
	}
	return out
}
