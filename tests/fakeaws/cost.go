package fakeaws

// Cost Explorer, Budgets, Cost Optimization Hub and Compute Optimizer, driven
// by the awsfix cost data sets (the same builders the cost package's unit
// tests use). Nothing here imports product code: amounts are summed in
// micro-units with the same rounding (half away from zero, six decimals) so a
// sum read through the fixture matches the unit fake to the micro.

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/tests/fixtures/awsfix"
)

// Cost scenarios. Each names one awsfix cost data set and a behaviour.
const (
	ScenarioCostNatSpike      = "cost-nat-spike"
	ScenarioCostCredits       = "cost-credits"
	ScenarioCostOneOffFee     = "cost-one-off-fee"
	ScenarioCostOrganization  = "cost-organization"
	ScenarioCostDeniedAccount = "cost-denied-account"
	ScenarioCostThrottled     = "cost-throttled"
	ScenarioCostCapReached    = "cost-cap-reached"
	ScenarioCostUnavailable   = "cost-unavailable"
	// ScenarioCostNatSpikeTwoNATs adds a second NAT gateway behind the
	// inventory app and refuses resource-level cost, so the NAT line cannot
	// be narrowed to one gateway.
	ScenarioCostNatSpikeTwoNATs = "cost-nat-spike-two-nats"
	// ScenarioCostNewUntaggedDB adds an untagged database on Oct 20 after a
	// full September.
	ScenarioCostNewUntaggedDB = "cost-new-untagged-db"

	// CostDeniedAccount is the member account whose cost calls are denied in
	// cost-denied-account.
	CostDeniedAccount = "333333333333"
)

// costScenarioData maps each cost scenario to its awsfix data set (several
// behaviours share one data set).
var costScenarioData = map[string]string{
	ScenarioCostNatSpike:        awsfix.CostNatSpikeName,
	ScenarioCostCredits:         awsfix.SingleAccountName,
	ScenarioCostOneOffFee:       awsfix.OrganizationName,
	ScenarioCostOrganization:    awsfix.OrganizationName,
	ScenarioCostDeniedAccount:   awsfix.OrganizationName,
	ScenarioCostThrottled:       awsfix.CostNatSpikeName,
	ScenarioCostCapReached:      awsfix.LargeOrganizationName,
	ScenarioCostUnavailable:     awsfix.CostNatSpikeName,
	ScenarioCostNatSpikeTwoNATs: awsfix.CostNatSpikeTwoNATsName,
	ScenarioCostNewUntaggedDB:   awsfix.CostNewUntaggedDBName,
}

// costServices are the billing and savings services this file answers.
var costServices = map[string]bool{"ce": true, "budgets": true, "cost-optimization-hub": true, "compute-optimizer": true}

// costFreeActions are Cost Explorer operations the product treats as free.
var costFreeActions = map[string]bool{"ListCostAllocationTags": true}

// CostScenarioData builds the billing world a cost scenario answers from, or
// nil when the scenario is not a cost scenario. Every call returns a fresh
// value.
func CostScenarioData(scenario string) *awsfix.CostScenario {
	name, ok := costScenarioData[scenario]
	if !ok {
		return nil
	}
	data, _ := awsfix.CostScenarioNamed(name)
	return data
}

// costData is a cost data set expanded into daily rows.
type costData struct {
	*awsfix.CostScenario
	rows []costRow
}

type costRow struct {
	day                                                           time.Time
	account, service, usageType, recordType, team, unit, resource string
	cost, qty                                                     int64
}

var (
	costCacheMu sync.Mutex
	costCache   = map[string]*costData{}
)

func loadCostData(scenario string) (*costData, error) {
	name, ok := costScenarioData[scenario]
	if !ok {
		return nil, fmt.Errorf("not a cost scenario")
	}
	costCacheMu.Lock()
	defer costCacheMu.Unlock()
	if data := costCache[name]; data != nil {
		return data, nil
	}
	data := costData{CostScenario: CostScenarioData(scenario)}
	start, err1 := time.Parse(time.DateOnly, data.Range.Start)
	end, err2 := time.Parse(time.DateOnly, data.Range.End)
	if err1 != nil || err2 != nil {
		return nil, fmt.Errorf("bad range in %s", name)
	}
	for day := start; day.Before(end); day = day.AddDate(0, 0, 1) {
		for _, series := range data.Series {
			var step *awsfix.CostStep
			for index := range series.Steps {
				from, err := time.Parse(time.DateOnly, series.Steps[index].From)
				if err == nil && !day.Before(from) {
					step = &series.Steps[index]
				}
			}
			if step == nil {
				continue
			}
			cost, _ := parseCostMicros(step.Cost)
			var qty int64
			if step.Quantity != "" {
				qty, _ = parseCostMicros(step.Quantity)
			}
			if cost == 0 && qty == 0 {
				continue
			}
			data.rows = append(data.rows, costRow{day, series.Account, series.Service, series.UsageType, series.RecordType, series.Team, series.Unit, series.ResourceID, cost, qty})
		}
	}
	costCache[name] = &data
	return &data, nil
}

// parseCostMicros parses a decimal amount into millionths, rounding half away
// from zero like the product does.
func parseCostMicros(raw string) (int64, error) {
	value := strings.TrimSpace(raw)
	negative := strings.HasPrefix(value, "-")
	value = strings.TrimPrefix(strings.TrimPrefix(value, "-"), "+")
	whole, fraction, _ := strings.Cut(value, ".")
	digits := whole + fraction
	number, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return 0, fmt.Errorf("bad amount %q", raw)
	}
	shift := 6 - len(fraction)
	ten := big.NewInt(10)
	if shift >= 0 {
		number.Mul(number, new(big.Int).Exp(ten, big.NewInt(int64(shift)), nil))
	} else {
		divisor := new(big.Int).Exp(ten, big.NewInt(int64(-shift)), nil)
		quotient, remainder := new(big.Int).QuoRem(number, divisor, new(big.Int))
		if remainder.Lsh(remainder, 1).Cmp(divisor) >= 0 {
			quotient.Add(quotient, big.NewInt(1))
		}
		number = quotient
	}
	if negative {
		number.Neg(number)
	}
	return number.Int64(), nil
}

func formatCostMicros(micros int64) string {
	negative := micros < 0
	if negative {
		micros = -micros
	}
	out := strconv.FormatInt(micros/1_000_000, 10) + "." + fmt.Sprintf("%06d", micros%1_000_000)
	if negative {
		return "-" + out
	}
	return out
}

// costCounters are the billing calls the fixture answered.
type costCounters struct {
	paid  int64
	free  int64
	calls map[string]int64
}

// PaidCostRequests is how many paid Cost Explorer requests reached the
// fixture (every Cost Explorer operation except the free tag listing).
func (s *Server) PaidCostRequests() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cost.paid
}

// CostCalls is the number of answered billing requests per service:Action.
func (s *Server) CostCalls() map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int64, len(s.cost.calls))
	for key, value := range s.cost.calls {
		out[key] = value
	}
	return out
}

// SetCostThrottled makes every Cost Explorer request answer
// LimitExceededException (on by default in cost-throttled).
func (s *Server) SetCostThrottled(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.costThrottled = on
}

func (s *Server) costThrottledNow() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.costThrottled
}

func (s *Server) countCost(request awsRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cost.calls == nil {
		s.cost.calls = map[string]int64{}
	}
	s.cost.calls[request.operation()]++
	if request.service == "ce" {
		if costFreeActions[request.action] {
			s.cost.free++
		} else {
			s.cost.paid++
		}
	}
}

// writeCostOperation answers a billing request in a cost scenario.
func (s *Server) writeCostOperation(writer http.ResponseWriter, request awsRequest) bool {
	if !costServices[request.service] || costScenarioData[request.scenario] == "" {
		return false
	}
	s.countCost(request)
	data, err := loadCostData(request.scenario)
	if err != nil {
		writeErrorFor(writer, request, http.StatusInternalServerError, "InternalFailure", "fixture data unavailable")
		return true
	}
	caller := request.account.ID
	switch {
	case request.scenario == ScenarioCostDeniedAccount && caller == CostDeniedAccount:
		writeErrorFor(writer, request, http.StatusBadRequest, "AccessDeniedException",
			"User: arn:aws:sts::"+caller+":assumed-role/versus-reader is not authorized to perform: "+request.operation())
		return true
	case request.scenario == ScenarioCostUnavailable && request.service == "ce":
		writeErrorFor(writer, request, http.StatusBadRequest, "AccessDeniedException", "User not enabled for cost explorer access")
		return true
	case request.scenario == ScenarioCostNatSpikeTwoNATs && request.action == "GetCostAndUsageWithResources":
		writeErrorFor(writer, request, http.StatusBadRequest, "AccessDeniedException",
			"User: arn:aws:sts::"+caller+":assumed-role/versus-reader is not authorized to perform: ce:GetCostAndUsageWithResources")
		return true
	case request.service == "ce" && s.costThrottledNow():
		writeErrorFor(writer, request, http.StatusBadRequest, "LimitExceededException", "Rate exceeded")
		return true
	}

	switch request.service + ":" + request.action {
	case "ce:GetCostForecast":
		writeJSONBody(writer, mustJSON(map[string]any{
			"Total": map[string]string{"Amount": data.Forecast.Mean, "Unit": data.Currency},
			"ForecastResultsByTime": []map[string]string{{"MeanValue": data.Forecast.Mean,
				"PredictionIntervalLowerBound": data.Forecast.Low, "PredictionIntervalUpperBound": data.Forecast.High}},
		}))
	case "ce:GetSavingsPlansCoverage":
		writeJSONBody(writer, mustJSON(map[string]any{"SavingsPlansCoverages": []map[string]any{{
			"Coverage": map[string]string{"SpendCoveredBySavingsPlans": data.Coverage.Covered, "TotalCost": data.Coverage.Total},
		}}}))
	case "ce:ListCostAllocationTags":
		status := "Inactive"
		if data.TagActive {
			status = "Active"
		}
		writeJSONBody(writer, mustJSON(map[string]any{"CostAllocationTags": []map[string]string{{"TagKey": "team", "Type": "UserDefined", "Status": status}}}))
	case "ce:GetCostAndUsage", "ce:GetCostAndUsageWithResources":
		writeJSONBody(writer, mustJSON(s.costAndUsage(data, request, caller)))
	case "budgets:DescribeBudgets":
		budgets := data.Budgets
		if budgets == nil {
			budgets = []awsfix.Budget{}
		}
		writeJSONBody(writer, mustJSON(map[string]any{"Budgets": budgets}))
	case "cost-optimization-hub:ListRecommendations":
		items := data.Recommendations
		if items == nil {
			items = []awsfix.HubRecommendation{}
		}
		writeBody(writer, "application/x-amz-json-1.0", http.StatusOK, mustJSON(map[string]any{"items": items}))
	case "compute-optimizer:GetEC2InstanceRecommendations":
		recommendations := []any{}
		for _, item := range data.ComputeOptimizer {
			recommendations = append(recommendations, map[string]any{
				"accountId": item.Account, "instanceArn": item.InstanceARN, "instanceName": item.Name,
				"currentInstanceType": item.Current, "finding": item.Finding,
				"recommendationOptions": []any{map[string]any{"instanceType": item.Target, "rank": 1,
					"savingsOpportunity": map[string]any{"estimatedMonthlySavings": map[string]any{"currency": "USD", "value": item.Monthly}}}},
			})
		}
		writeCBORBody(writer, http.StatusOK, map[string]any{"instanceRecommendations": recommendations})
	default:
		writeErrorFor(writer, request, http.StatusBadRequest, "UnknownOperationException", "Unsupported billing operation")
	}
	return true
}

type costFilter struct {
	And        []costFilter
	Dimensions *struct {
		Key    string
		Values []string
	}
}

func (filter *costFilter) match(row costRow) bool {
	if filter == nil {
		return true
	}
	for index := range filter.And {
		if !filter.And[index].match(row) {
			return false
		}
	}
	if dimensions := filter.Dimensions; dimensions != nil {
		value := costDimension(row, dimensions.Key)
		for _, want := range dimensions.Values {
			if value == want {
				return true
			}
		}
		return false
	}
	return true
}

func costDimension(row costRow, key string) string {
	switch key {
	case "LINKED_ACCOUNT":
		return row.account
	case "SERVICE":
		return row.service
	case "USAGE_TYPE":
		return row.usageType
	case "RECORD_TYPE":
		return row.recordType
	case "RESOURCE_ID":
		return row.resource
	}
	return ""
}

// costAndUsage aggregates the rows the caller may see the way Cost Explorer
// does. A management account (named "Management" in the data set) sees every
// account; any other caller sees only its own lines. Every day of the open
// month, by the fixture clock, is marked Estimated, like real AWS.
func (s *Server) costAndUsage(data *costData, request awsRequest, caller string) map[string]any {
	var input struct {
		TimePeriod    struct{ Start, End string }
		GroupBy       []struct{ Type, Key string }
		Filter        *costFilter
		NextPageToken *string
	}
	_ = json.Unmarshal(request.body, &input)
	start, _ := time.Parse(time.DateOnly, input.TimePeriod.Start)
	end, _ := time.Parse(time.DateOnly, input.TimePeriod.End)
	resources := request.action == "GetCostAndUsageWithResources"
	management := data.Names[caller] == "Management"
	now := s.clock().UTC()
	openMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)

	type aggregate struct {
		keys      []string
		cost, qty int64
		unit      string
	}
	byDay := map[time.Time]map[string]*aggregate{}
	accounts := map[string]bool{}
	for _, row := range data.rows {
		if row.day.Before(start) || !row.day.Before(end) || !input.Filter.match(row) {
			continue
		}
		if !management && row.account != caller {
			continue
		}
		if resources && row.resource == "" {
			continue
		}
		var keys []string
		for _, group := range input.GroupBy {
			if group.Type == "TAG" {
				keys = append(keys, group.Key+"$"+row.team)
			} else {
				keys = append(keys, costDimension(row, group.Key))
			}
			if group.Key == "LINKED_ACCOUNT" {
				accounts[row.account] = true
			}
		}
		key := strings.Join(keys, "\x00")
		if byDay[row.day] == nil {
			byDay[row.day] = map[string]*aggregate{}
		}
		entry := byDay[row.day][key]
		if entry == nil {
			entry = &aggregate{keys: keys}
			byDay[row.day][key] = entry
		}
		entry.cost += row.cost
		entry.qty += row.qty
		switch {
		case entry.unit == "":
			entry.unit = row.unit
		case entry.unit != row.unit:
			entry.unit = "N/A"
		}
	}
	type flat struct {
		day   time.Time
		group map[string]any
	}
	var all []flat
	var days []time.Time
	for day := start; day.Before(end); day = day.AddDate(0, 0, 1) {
		days = append(days, day)
		keys := make([]string, 0, len(byDay[day]))
		for key := range byDay[day] {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			entry := byDay[day][key]
			unit := entry.unit
			if unit == "" {
				unit = "N/A"
			}
			all = append(all, flat{day, map[string]any{"Keys": entry.keys, "Metrics": map[string]any{
				"NetAmortizedCost": map[string]string{"Amount": formatCostMicros(entry.cost), "Unit": data.Currency},
				"UsageQuantity":    map[string]string{"Amount": formatCostMicros(entry.qty), "Unit": unit},
			}}})
		}
	}
	offset := 0
	if input.NextPageToken != nil {
		offset, _ = strconv.Atoi(*input.NextPageToken)
	}
	if offset > len(all) {
		offset = len(all)
	}
	page := all[offset:]
	next := ""
	if data.PageGroups > 0 && len(page) > data.PageGroups {
		page = page[:data.PageGroups]
		next = strconv.Itoa(offset + data.PageGroups)
	}
	groups := map[time.Time][]map[string]any{}
	for _, item := range page {
		groups[item.day] = append(groups[item.day], item.group)
	}
	results := []map[string]any{}
	for _, day := range days {
		if offset > 0 && len(groups[day]) == 0 {
			continue
		}
		dayGroups := groups[day]
		if dayGroups == nil {
			dayGroups = []map[string]any{}
		}
		results = append(results, map[string]any{
			"TimePeriod": map[string]string{"Start": day.Format(time.DateOnly), "End": day.AddDate(0, 0, 1).Format(time.DateOnly)},
			"Groups":     dayGroups,
			"Total":      map[string]any{},
			"Estimated":  !day.Before(openMonth),
		})
	}
	ids := make([]string, 0, len(accounts))
	for id := range accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	attributes := []map[string]any{}
	for _, id := range ids {
		if name := data.Names[id]; name != "" {
			attributes = append(attributes, map[string]any{"Value": id, "Attributes": map[string]string{"description": name}})
		}
	}
	out := map[string]any{"ResultsByTime": results, "DimensionValueAttributes": attributes, "GroupDefinitions": input.GroupBy}
	if next != "" {
		out["NextPageToken"] = next
	}
	return out
}
