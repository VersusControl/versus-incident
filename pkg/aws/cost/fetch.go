package cost

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/budgets"
	budgettypes "github.com/aws/aws-sdk-go-v2/service/budgets/types"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
	"github.com/aws/aws-sdk-go-v2/service/costoptimizationhub"

	"github.com/VersusControl/versus-incident/pkg/aws/limit"
	"github.com/VersusControl/versus-incident/pkg/finops"
)

const (
	metricCost     = "NetAmortizedCost"
	metricQuantity = "UsageQuantity"
	// requestTimeout bounds one request independent of the caller.
	requestTimeout = 30 * time.Second
	// forecastInterval is the prediction interval the page shows.
	forecastInterval = 80
)

// Page bounds. A call that reaches its bound with pages still to come
// fails as truncated; a partial answer is never kept as complete.
var (
	// maxPages bounds a paid paginated call; every page is metered anyway.
	maxPages = 50
	// maxFreePages bounds the free list calls.
	maxFreePages = 10
)

// errMalformed means AWS returned an amount or date we cannot read. It is
// reported as Cost Explorer unavailable, never as zero.
var errMalformed = errors.New("cost: malformed response")

// errTruncated means pagination did not finish within its bound.
var errTruncated = fmt.Errorf("%w: pagination did not finish", errMalformed)

// window is an inclusive range of days.
type window struct {
	Start finops.Date
	End   finops.Date
}

// interval is the Cost Explorer interval; its End is exclusive.
func (w window) interval() *cetypes.DateInterval {
	return &cetypes.DateInterval{Start: aws.String(w.Start.String()), End: aws.String(w.End.AddDays(1).String())}
}

// fetcher reads one payer. Paid requests go through the meter one page at a
// time; the cap is decided there.
type fetcher struct {
	payer   string
	env     env
	meter   finops.Meter
	limiter *limit.Manager
	// pages counts the paid requests this fetcher sent.
	pages *int
}

// send paces a request through the us-east-1 limiter and reports throttling
// back to it. It never retries.
func (f fetcher) send(ctx context.Context, call func(context.Context) error) error {
	if f.limiter != nil {
		if err := f.limiter.Wait(ctx, f.payer, billingRegion); err != nil {
			return err
		}
	}
	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	err := call(reqCtx)
	if f.limiter != nil {
		if ErrorCode(err) == CodeThrottled {
			f.limiter.Throttled(f.payer, billingRegion)
		} else {
			f.limiter.Succeeded(f.payer, billingRegion)
		}
	}
	return err
}

// paid takes one request from the meter, then sends it.
func (f fetcher) paid(ctx context.Context, call func(context.Context) error) error {
	if err := f.meter.Take(ctx, 1); err != nil {
		return err
	}
	if f.pages != nil {
		*f.pages++
	}
	return f.send(ctx, call)
}

// dayResult is one day of one grouped request.
type dayResult struct {
	Estimated bool
	Cells     []cell
}

// grouped is a whole grouped request: per-day cells, the linked-account
// names Cost Explorer returned and the currency.
type grouped struct {
	Days     map[finops.Date]*dayResult
	Names    map[string]string
	Currency string
}

func usageFilter() *cetypes.Expression {
	return &cetypes.Expression{Dimensions: &cetypes.DimensionValues{Key: cetypes.DimensionRecordType, Values: usageRecordTypes}}
}

func dimension(key cetypes.Dimension, values ...string) cetypes.Expression {
	return cetypes.Expression{Dimensions: &cetypes.DimensionValues{Key: key, Values: values}}
}

func groupDim(d cetypes.Dimension) cetypes.GroupDefinition {
	return cetypes.GroupDefinition{Type: cetypes.GroupDefinitionTypeDimension, Key: aws.String(string(d))}
}

func groupTag(key string) cetypes.GroupDefinition {
	return cetypes.GroupDefinition{Type: cetypes.GroupDefinitionTypeTag, Key: aws.String(key)}
}

// costAndUsage runs one daily grouped request, every page metered.
func (f fetcher) costAndUsage(ctx context.Context, w window, groups []cetypes.GroupDefinition, filter *cetypes.Expression, withQuantity bool) (*grouped, error) {
	client := f.env.costExplorer()
	metrics := []string{metricCost}
	if withQuantity {
		metrics = append(metrics, metricQuantity)
	}
	out := &grouped{Days: map[finops.Date]*dayResult{}, Names: map[string]string{}}
	var token *string
	for page := 0; page < maxPages; page++ {
		var resp *costexplorer.GetCostAndUsageOutput
		err := f.paid(ctx, func(ctx context.Context) error {
			var err error
			resp, err = client.GetCostAndUsage(ctx, &costexplorer.GetCostAndUsageInput{
				Granularity: cetypes.GranularityDaily, Metrics: metrics, TimePeriod: w.interval(),
				GroupBy: groups, Filter: filter, NextPageToken: token,
			})
			return err
		})
		if err != nil {
			return nil, err
		}
		if err := out.add(resp.ResultsByTime, resp.DimensionValueAttributes); err != nil {
			return nil, err
		}
		token = resp.NextPageToken
		if token == nil || *token == "" {
			return out, nil
		}
	}
	return nil, errTruncated
}

func (g *grouped) add(results []cetypes.ResultByTime, attrs []cetypes.DimensionValuesWithAttributes) error {
	for _, a := range attrs {
		if a.Value != nil && a.Attributes["description"] != "" {
			g.Names[*a.Value] = a.Attributes["description"]
		}
	}
	for _, r := range results {
		if r.TimePeriod == nil || r.TimePeriod.Start == nil {
			return errMalformed
		}
		day, err := finops.ParseDate(*r.TimePeriod.Start)
		if err != nil {
			return errMalformed
		}
		d := g.Days[day]
		if d == nil {
			d = &dayResult{}
			g.Days[day] = d
		}
		d.Estimated = d.Estimated || r.Estimated
		for _, grp := range r.Groups {
			c := cell{}
			if len(grp.Keys) > 0 {
				c.A = grp.Keys[0]
			}
			if len(grp.Keys) > 1 {
				c.B = grp.Keys[1]
			}
			if m, ok := grp.Metrics[metricCost]; ok && m.Amount != nil {
				v, err := finops.ParseMicros(*m.Amount)
				if err != nil {
					return errMalformed
				}
				c.M = v
				if m.Unit != nil && g.Currency == "" {
					g.Currency = *m.Unit
				}
			}
			if m, ok := grp.Metrics[metricQuantity]; ok && m.Amount != nil {
				v, err := finops.ParseQuantityMicros(*m.Amount)
				if err != nil {
					return errMalformed
				}
				c.Q = v
				if m.Unit != nil && *m.Unit != "N/A" {
					c.U = *m.Unit
				}
			}
			if c.M != 0 || c.Q != 0 {
				d.Cells = append(d.Cells, c)
			}
		}
	}
	return nil
}

// The four daily tables.
func (f fetcher) byAccount(ctx context.Context, w window) (*grouped, error) {
	return f.costAndUsage(ctx, w, []cetypes.GroupDefinition{groupDim(cetypes.DimensionLinkedAccount), groupDim(cetypes.DimensionService)}, usageFilter(), false)
}

func (f fetcher) byUsage(ctx context.Context, w window) (*grouped, error) {
	return f.costAndUsage(ctx, w, []cetypes.GroupDefinition{groupDim(cetypes.DimensionService), groupDim(cetypes.DimensionUsageType)}, usageFilter(), true)
}

// byTeam reads the team tag by linked account when the payer covers
// several accounts (account × team is then exact), else by service.
func (f fetcher) byTeam(ctx context.Context, w window, tagKey string, byAccount bool) (*grouped, error) {
	second := cetypes.DimensionService
	if byAccount {
		second = cetypes.DimensionLinkedAccount
	}
	return f.costAndUsage(ctx, w, []cetypes.GroupDefinition{groupTag(tagKey), groupDim(second)}, usageFilter(), false)
}

func (f fetcher) byRecord(ctx context.Context, w window) (*grouped, error) {
	return f.costAndUsage(ctx, w, []cetypes.GroupDefinition{groupDim(cetypes.DimensionRecordType), groupDim(cetypes.DimensionLinkedAccount)}, nil, false)
}

// drill reads one account and service by usage type and team tag, so the
// usage type and owner of a drilled line are exact.
func (f fetcher) drill(ctx context.Context, w window, account, service, tagKey string) (*grouped, error) {
	filter := &cetypes.Expression{And: []cetypes.Expression{
		*usageFilter(),
		dimension(cetypes.DimensionLinkedAccount, account),
		dimension(cetypes.DimensionService, service),
	}}
	return f.costAndUsage(ctx, w, []cetypes.GroupDefinition{groupDim(cetypes.DimensionUsageType), groupTag(tagKey)}, filter, true)
}

// resources reads resource-level cost of the given services by usage type.
func (f fetcher) resources(ctx context.Context, w window, services []string) (*grouped, error) {
	client := f.env.costExplorer()
	filter := &cetypes.Expression{And: []cetypes.Expression{*usageFilter(), dimension(cetypes.DimensionService, services...)}}
	groups := []cetypes.GroupDefinition{groupDim(cetypes.DimensionUsageType), groupDim(cetypes.DimensionResourceId)}
	out := &grouped{Days: map[finops.Date]*dayResult{}, Names: map[string]string{}}
	var token *string
	for page := 0; page < maxPages; page++ {
		var resp *costexplorer.GetCostAndUsageWithResourcesOutput
		err := f.paid(ctx, func(ctx context.Context) error {
			var err error
			resp, err = client.GetCostAndUsageWithResources(ctx, &costexplorer.GetCostAndUsageWithResourcesInput{
				Granularity: cetypes.GranularityDaily, Metrics: []string{metricCost, metricQuantity},
				TimePeriod: w.interval(), GroupBy: groups, Filter: filter, NextPageToken: token,
			})
			return err
		})
		if err != nil {
			return nil, err
		}
		if err := out.add(resp.ResultsByTime, resp.DimensionValueAttributes); err != nil {
			return nil, err
		}
		token = resp.NextPageToken
		if token == nil || *token == "" {
			return out, nil
		}
	}
	return nil, errTruncated
}

// forecastRest is the forecast for the rest of the month from today.
type forecastRest struct {
	Month string
	Mean  int64
	Low   int64
	High  int64
}

func (f fetcher) forecast(ctx context.Context, today finops.Date) (*forecastRest, error) {
	client := f.env.costExplorer()
	end := today.MonthEnd()
	var resp *costexplorer.GetCostForecastOutput
	err := f.paid(ctx, func(ctx context.Context) error {
		var err error
		resp, err = client.GetCostForecast(ctx, &costexplorer.GetCostForecastInput{
			Granularity: cetypes.GranularityMonthly, Metric: cetypes.MetricNetAmortizedCost,
			TimePeriod:              &cetypes.DateInterval{Start: aws.String(today.String()), End: aws.String(end.AddDays(1).String())},
			PredictionIntervalLevel: aws.Int32(forecastInterval),
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	out := &forecastRest{Month: today.MonthKey()}
	if resp.Total == nil || resp.Total.Amount == nil {
		return nil, errMalformed
	}
	if out.Mean, err = finops.ParseMicros(*resp.Total.Amount); err != nil {
		return nil, errMalformed
	}
	out.Low, out.High = out.Mean, out.Mean
	if len(resp.ForecastResultsByTime) > 0 {
		var low, high int64
		for _, r := range resp.ForecastResultsByTime {
			l, err1 := parseOptional(r.PredictionIntervalLowerBound)
			h, err2 := parseOptional(r.PredictionIntervalUpperBound)
			if err1 != nil || err2 != nil {
				return nil, errMalformed
			}
			low, high = low+l, high+h
		}
		if low != 0 || high != 0 {
			out.Low, out.High = low, high
		}
	}
	return out, nil
}

func parseOptional(s *string) (int64, error) {
	if s == nil || *s == "" {
		return 0, nil
	}
	return finops.ParseMicros(*s)
}

// coverage is Savings Plans coverage over a window, as exact sums.
type coverage struct {
	Covered int64 `json:"covered_micros"`
	Total   int64 `json:"total_micros"`
}

func (f fetcher) coverage(ctx context.Context, w window) (*coverage, error) {
	client := f.env.costExplorer()
	out := &coverage{}
	var token *string
	for page := 0; page < maxPages; page++ {
		var resp *costexplorer.GetSavingsPlansCoverageOutput
		err := f.paid(ctx, func(ctx context.Context) error {
			var err error
			resp, err = client.GetSavingsPlansCoverage(ctx, &costexplorer.GetSavingsPlansCoverageInput{
				TimePeriod: w.interval(), Granularity: cetypes.GranularityMonthly, NextToken: token,
			})
			return err
		})
		if err != nil {
			return nil, err
		}
		for _, c := range resp.SavingsPlansCoverages {
			if c.Coverage == nil {
				continue
			}
			cv, err1 := parseOptional(c.Coverage.SpendCoveredBySavingsPlans)
			tot, err2 := parseOptional(c.Coverage.TotalCost)
			if err1 != nil || err2 != nil {
				return nil, errMalformed
			}
			out.Covered += cv
			out.Total += tot
		}
		token = resp.NextToken
		if token == nil || *token == "" {
			return out, nil
		}
	}
	return nil, errTruncated
}

// tagActive reports whether the team tag is an active cost allocation tag.
// The call is free.
func (f fetcher) tagActive(ctx context.Context, key string) (bool, error) {
	client := f.env.costExplorer()
	var resp *costexplorer.ListCostAllocationTagsOutput
	err := f.send(ctx, func(ctx context.Context) error {
		var err error
		resp, err = client.ListCostAllocationTags(ctx, &costexplorer.ListCostAllocationTagsInput{TagKeys: []string{key}})
		return err
	})
	if err != nil {
		return false, err
	}
	for _, t := range resp.CostAllocationTags {
		if t.TagKey != nil && *t.TagKey == key && t.Status == cetypes.CostAllocationTagStatusActive {
			return true, nil
		}
	}
	return false, nil
}

// budgets reads the payer's monthly cost budgets (free). A budget in
// another measure, or with a filter the neutral model cannot express, is
// left out and named in skipped.
func (f fetcher) budgets(ctx context.Context, tagKey string) (out []finops.Budget, skipped []finops.SkippedBudget, err error) {
	client := f.env.budgets()
	var token *string
	for page := 0; page < maxFreePages; page++ {
		var resp *budgets.DescribeBudgetsOutput
		err := f.send(ctx, func(ctx context.Context) error {
			var err error
			resp, err = client.DescribeBudgets(ctx, &budgets.DescribeBudgetsInput{AccountId: aws.String(f.payer), NextToken: token})
			return err
		})
		if err != nil {
			return nil, nil, err
		}
		for _, b := range resp.Budgets {
			nb, reason := mapBudget(b, tagKey)
			if reason != "" {
				skipped = append(skipped, finops.SkippedBudget{Name: aws.ToString(b.BudgetName), Reason: reason})
				continue
			}
			out = append(out, nb)
		}
		token = resp.NextToken
		if token == nil || *token == "" {
			return out, skipped, nil
		}
	}
	return nil, nil, errTruncated
}

func mapBudget(b budgettypes.Budget, tagKey string) (finops.Budget, string) {
	if b.BudgetType != budgettypes.BudgetTypeCost || b.TimeUnit != budgettypes.TimeUnitMonthly || b.BudgetLimit == nil || b.BudgetLimit.Amount == nil || b.BudgetName == nil {
		return finops.Budget{}, finops.SkipBudgetNotMonthlyCost
	}
	if !netAmortized(b.Metrics) {
		return finops.Budget{}, finops.SkipBudgetMeasure
	}
	amount, err := finops.ParseMicros(*b.BudgetLimit.Amount)
	if err != nil || amount <= 0 {
		return finops.Budget{}, finops.SkipBudgetNotMonthlyCost
	}
	out := finops.Budget{Name: *b.BudgetName, Scope: finops.ScopeDeployment, Monthly: amount, Source: finops.BudgetFromProvider}
	var ok bool
	if b.FilterExpression != nil {
		out.Scope, out.Value, ok = scopeOfExpression(*b.FilterExpression, tagKey)
	} else {
		out.Scope, out.Value, ok = scopeOfCostFilters(b.CostFilters, tagKey)
	}
	if !ok {
		return finops.Budget{}, finops.SkipBudgetFilter
	}
	out.ID = "aws-" + finops.BudgetID(out.Scope, out.Value+"|"+out.Name)
	return out, ""
}

// netAmortized reports a budget measured in net amortized cost, the one
// measure the page uses. A budget without a metric list uses the legacy
// cost types, which cannot express net amortized cost.
func netAmortized(metrics []budgettypes.Metric) bool {
	for _, m := range metrics {
		if m == budgettypes.MetricNetAmortizedCost {
			return true
		}
	}
	return false
}

// scopeOfExpression maps a filter expression of exactly one linked account
// or one team tag value; anything else is not expressible.
func scopeOfExpression(e budgettypes.Expression, tagKey string) (string, string, bool) {
	if len(e.And) > 0 || len(e.Or) > 0 || e.Not != nil || e.CostCategories != nil || e.ProductAttributes != nil {
		return "", "", false
	}
	switch {
	case e.Dimensions != nil && e.Tags == nil:
		d := e.Dimensions
		if d.Key == budgettypes.DimensionLinkedAccount && len(d.Values) == 1 && equalsOnly(d.MatchOptions) {
			return finops.ScopeAccount, d.Values[0], true
		}
	case e.Tags != nil && e.Dimensions == nil:
		tg := e.Tags
		key := strings.TrimPrefix(aws.ToString(tg.Key), "user:")
		if key == tagKey && len(tg.Values) == 1 && equalsOnly(tg.MatchOptions) {
			v := tg.Values[0]
			if k, val, ok := cutTagFilter(v); ok {
				if k != tagKey {
					return "", "", false
				}
				v = val
			}
			if v != "" {
				return finops.ScopeTeam, v, true
			}
		}
	case e.Dimensions == nil && e.Tags == nil:
		return finops.ScopeDeployment, "", true
	}
	return "", "", false
}

func equalsOnly(opts []budgettypes.MatchOption) bool {
	for _, o := range opts {
		if o != budgettypes.MatchOptionEquals {
			return false
		}
	}
	return true
}

// scopeOfCostFilters maps the legacy cost filters of older budgets.
func scopeOfCostFilters(f map[string][]string, tagKey string) (string, string, bool) {
	switch {
	case len(f) == 0:
		return finops.ScopeDeployment, "", true
	case len(f) == 1 && len(f["LinkedAccount"]) == 1:
		return finops.ScopeAccount, f["LinkedAccount"][0], true
	case len(f) == 1 && len(f["TagKeyValue"]) == 1:
		k, v, ok := cutTagFilter(f["TagKeyValue"][0])
		if ok && k == tagKey && v != "" {
			return finops.ScopeTeam, v, true
		}
	}
	return "", "", false
}

// rightsizeActions are the hub actions that map to a rightsizing
// recommendation.
var rightsizeActions = map[string]bool{"Rightsize": true, "Upgrade": true, "MigrateToGraviton": true}

// hubResult is what Cost Optimization Hub contributes to the savings list.
type hubResult struct {
	Recommendations []finops.Recommendation
	// Idle holds unattached volumes (Delete) and idle machines (Stop).
	Idle []finops.IdleResource
	// Commitment is the hub's Savings Plans purchase saving and summary.
	CommitmentMonthly    int64
	CommitmentSuggestion string
}

// recommendations reads Cost Optimization Hub (free). Rightsizing actions
// become recommendations, Delete on a volume an unattached volume, Stop an
// idle machine, and Savings Plans purchases the commitment suggestion.
func (f fetcher) recommendations(ctx context.Context, tagKey string, keepTeams bool) (*hubResult, error) {
	client := f.env.optimizationHub()
	out := &hubResult{}
	var token *string
	for page := 0; page < maxFreePages; page++ {
		var resp *costoptimizationhub.ListRecommendationsOutput
		err := f.send(ctx, func(ctx context.Context) error {
			var err error
			resp, err = client.ListRecommendations(ctx, &costoptimizationhub.ListRecommendationsInput{MaxResults: aws.Int32(100), NextToken: token})
			return err
		})
		if err != nil {
			return nil, err
		}
		for _, r := range resp.Items {
			if r.ActionType == nil || r.EstimatedMonthlySavings == nil {
				continue
			}
			monthly, err := finops.ParseMicros(strconv.FormatFloat(*r.EstimatedMonthlySavings, 'f', -1, 64))
			if err != nil || monthly <= 0 {
				continue
			}
			team := ""
			if keepTeams {
				for _, t := range r.Tags {
					if aws.ToString(t.Key) == tagKey {
						team = aws.ToString(t.Value)
					}
				}
			}
			resourceType := aws.ToString(r.CurrentResourceType)
			switch action := *r.ActionType; {
			case rightsizeActions[action]:
				out.Recommendations = append(out.Recommendations, finops.Recommendation{
					ResourceID: resourcePart(aws.ToString(r.ResourceId)), Account: aws.ToString(r.AccountId), Region: aws.ToString(r.Region),
					Current: aws.ToString(r.CurrentResourceSummary), Target: aws.ToString(r.RecommendedResourceSummary),
					Monthly: monthly, Source: "cost_optimization_hub", Team: team, Guide: hubGuide(),
				})
			case action == "Delete" && resourceType == "EbsVolume":
				out.Idle = append(out.Idle, finops.IdleResource{Kind: finops.IdleUnattachedVolume, ID: resourcePart(aws.ToString(r.ResourceId)),
					Account: aws.ToString(r.AccountId), Region: aws.ToString(r.Region), Team: team, Monthly: monthly, Basis: finops.BasisProvider})
			case action == "Stop":
				out.Idle = append(out.Idle, finops.IdleResource{Kind: finops.IdleIdleInstance, ID: resourcePart(aws.ToString(r.ResourceId)),
					Account: aws.ToString(r.AccountId), Region: aws.ToString(r.Region), Team: team, Monthly: monthly, Basis: finops.BasisProvider})
			case action == "PurchaseSavingsPlans":
				if monthly > out.CommitmentMonthly {
					out.CommitmentMonthly = monthly
					out.CommitmentSuggestion = commitmentSuggestion(cmp.Or(aws.ToString(r.RecommendedResourceType), resourceType), aws.ToString(r.RecommendedResourceSummary))
				}
			}
		}
		token = resp.NextToken
		if token == nil || *token == "" {
			return out, nil
		}
	}
	return nil, errTruncated
}

// commitmentSuggestion rewords the hub's Savings Plans purchase summary in
// plain words (finops.PlainCommitmentSuggestion), naming the plan kind.
func commitmentSuggestion(resourceType, summary string) string {
	kind := ""
	switch resourceType {
	case "ComputeSavingsPlans":
		kind = "compute"
	case "Ec2InstanceSavingsPlans":
		kind = "instance family"
	case "SageMakerSavingsPlans":
		kind = "machine learning"
	}
	return finops.PlainCommitmentSuggestion(kind, summary)
}

// cutTagFilter reads a tag filter value ("user:team$checkout").
func cutTagFilter(s string) (string, string, bool) {
	s = strings.TrimPrefix(s, "user:")
	k, v, ok := strings.Cut(s, "$")
	return k, v, ok
}
