package cost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/smithy-go/encoding/cbor"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"

	"github.com/VersusControl/versus-incident/pkg/finops"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	"github.com/VersusControl/versus-incident/tests/fixtures/awsfix"
	"github.com/VersusControl/versus-incident/tests/fixtures/storagefix"
)

type scenarioStep struct {
	From     finops.Date `json:"from"`
	Cost     string      `json:"cost"`
	Quantity string      `json:"quantity"`
}

type scenarioSeries struct {
	Account    string         `json:"account"`
	Service    string         `json:"service"`
	UsageType  string         `json:"usage_type"`
	RecordType string         `json:"record_type"`
	Team       string         `json:"team"`
	Unit       string         `json:"unit"`
	ResourceID string         `json:"resource_id"`
	Steps      []scenarioStep `json:"steps"`
}

type scenario struct {
	Currency        string            `json:"currency"`
	Range           finops.DateRange  `json:"range"`
	PartialFrom     finops.Date       `json:"partial_from"`
	Names           map[string]string `json:"names"`
	Series          []scenarioSeries  `json:"series"`
	Forecast        map[string]string `json:"forecast"`
	Coverage        map[string]string `json:"coverage"`
	TagActive       bool              `json:"tag_active"`
	Budgets         []json.RawMessage `json:"budgets"`
	ExpectedBudgets map[string]string `json:"expected_budgets"`
	ExpectedSkipped map[string]string `json:"expected_skipped_budgets"`
	// ComputeOptimizer answers the Compute Optimizer fallback.
	ComputeOptimizer []struct {
		Account     string  `json:"account"`
		InstanceARN string  `json:"instance_arn"`
		Name        string  `json:"name"`
		Current     string  `json:"current"`
		Target      string  `json:"target"`
		Monthly     float64 `json:"monthly"`
		Finding     string  `json:"finding"`
	} `json:"compute_optimizer"`
	// Inventory, Changes and ChangeCoverage seed the snapshot store.
	Inventory      []model.Resource   `json:"inventory"`
	Changes        []model.Change     `json:"changes"`
	ChangeCoverage []changes.Interval `json:"change_coverage"`
	// PageGroups caps the groups per page the fake returns.
	PageGroups      int               `json:"page_groups"`
	Recommendations []json.RawMessage `json:"recommendations"`
}

// row is one day of one series: the ground truth every grouping is
// aggregated from.
type row struct {
	day                                                           finops.Date
	account, service, usageType, recordType, team, unit, resource string
	cost, qty                                                     int64
}

// loadScenario builds the named awsfix cost data set and decodes it into the
// fake's typed scenario.
func loadScenario(t *testing.T, name string) *scenario {
	t.Helper()
	built, ok := awsfix.CostScenarioNamed(name)
	if !ok {
		t.Fatalf("unknown cost scenario %q", name)
	}
	raw, err := json.Marshal(built)
	if err != nil {
		t.Fatal(err)
	}
	var s scenario
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return &s
}

func (s *scenario) rows() []row {
	var out []row
	s.Range.Each(func(d finops.Date) {
		for _, se := range s.Series {
			var step *scenarioStep
			for i := range se.Steps {
				if !d.Before(se.Steps[i].From) {
					step = &se.Steps[i]
				}
			}
			if step == nil {
				continue
			}
			c, _ := finops.ParseMicros(step.Cost)
			var q int64
			if step.Quantity != "" {
				q, _ = finops.ParseQuantityMicros(step.Quantity)
			}
			if c == 0 && q == 0 {
				continue
			}
			out = append(out, row{d, se.Account, se.Service, se.UsageType, se.RecordType, se.Team, se.Unit, se.ResourceID, c, q})
		}
	})
	return out
}

// fakeFailure makes one call label fail with an AWS error.
type fakeFailure struct {
	Type    string
	Message string
	Status  int
}

// fakeAWS answers Cost Explorer, Budgets and Cost Optimization Hub requests
// from a scenario. It counts every request by call label.
type fakeAWS struct {
	t        *testing.T
	mu       sync.Mutex
	sc       *scenario
	payers   map[string]*scenario // per payer when several payers are faked
	failures map[string]fakeFailure
	// pageGroups caps the groups per page so pagination is exercised.
	pageGroups int
	calls      map[string]int
	hosts      map[string]int
	bySigner   map[string]int
	paidSigner map[string]int
	total      int
	// denyPayer answers every request signed by that payer with a denial.
	denyPayer string
}

func newFake(t *testing.T, sc *scenario) *fakeAWS {
	return &fakeAWS{t: t, sc: sc, pageGroups: sc.PageGroups, failures: map[string]fakeFailure{}, calls: map[string]int{}, hosts: map[string]int{}, bySigner: map[string]int{}, paidSigner: map[string]int{}}
}

func (f *fakeAWS) count(label string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[label]
}

func (f *fakeAWS) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.total
}

type ceRequest struct {
	TimePeriod    struct{ Start, End string }
	GroupBy       []struct{ Type, Key string }
	Filter        *ceFilter
	NextPageToken *string
	NextToken     *string
}

type ceFilter struct {
	And        []ceFilter
	Dimensions *struct {
		Key    string
		Values []string
	}
}

func (flt *ceFilter) match(r row) bool {
	if flt == nil {
		return true
	}
	for i := range flt.And {
		if !flt.And[i].match(r) {
			return false
		}
	}
	if d := flt.Dimensions; d != nil {
		v := dimValue(r, d.Key)
		ok := false
		for _, want := range d.Values {
			ok = ok || v == want
		}
		return ok
	}
	return true
}

func dimValue(r row, key string) string {
	switch key {
	case "LINKED_ACCOUNT":
		return r.account
	case "SERVICE":
		return r.service
	case "USAGE_TYPE":
		return r.usageType
	case "RECORD_TYPE":
		return r.recordType
	case "RESOURCE_ID":
		return r.resource
	}
	return ""
}

func label(target string, req ceRequest) string {
	switch {
	case target == computeOptimizerTarget:
		return "compute_optimizer"
	case strings.HasSuffix(target, ".GetCostForecast"):
		return callForecast
	case strings.HasSuffix(target, ".GetSavingsPlansCoverage"):
		return callCoverage
	case strings.HasSuffix(target, ".GetCostAndUsageWithResources"):
		return callResources
	case strings.HasSuffix(target, ".ListCostAllocationTags"):
		return "tags"
	case strings.HasSuffix(target, ".DescribeBudgets"):
		return "budgets"
	case strings.HasSuffix(target, ".ListRecommendations"):
		return "hub"
	}
	var keys []string
	for _, g := range req.GroupBy {
		if g.Type == "TAG" {
			keys = append(keys, "TAG")
		} else {
			keys = append(keys, g.Key)
		}
	}
	switch strings.Join(keys, ",") {
	case "LINKED_ACCOUNT,SERVICE":
		return callAccount
	case "SERVICE,USAGE_TYPE":
		return callUsage
	case "TAG,SERVICE", "TAG,LINKED_ACCOUNT":
		return callTeam
	case "RECORD_TYPE,LINKED_ACCOUNT":
		return callRecord
	case "USAGE_TYPE,TAG":
		return callDrill
	}
	return "unknown"
}

func (f *fakeAWS) Do(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	target := req.Header.Get("X-Amz-Target")
	if strings.HasSuffix(req.URL.Path, "/operation/GetEC2InstanceRecommendations") {
		target = computeOptimizerTarget
	}
	var in ceRequest
	_ = json.Unmarshal(body, &in)
	lbl := label(target, in)

	id := signer(req)
	f.mu.Lock()
	f.calls[lbl]++
	f.hosts[req.URL.Host]++
	f.bySigner[id]++
	if paidLabel(lbl) {
		f.paidSigner[id]++
	}
	f.total++
	fail, failing := f.failures[lbl]
	if f.denyPayer != "" && signer(req) == f.denyPayer {
		fail, failing = fakeFailure{Type: "AccessDeniedException", Message: "not authorized"}, true
	}
	f.mu.Unlock()

	if failing && lbl == "compute_optimizer" {
		return cborResponse(400, cbor.Map{"__type": cbor.String(fail.Type), "message": cbor.String(fail.Message)}), nil
	}
	if lbl == "compute_optimizer" {
		return f.computeOptimizer(), nil
	}
	if failing {
		status := fail.Status
		if status == 0 {
			status = 400
		}
		return jsonResponse(status, map[string]string{"__type": fail.Type, "message": fail.Message}, fail.Type), nil
	}
	sc := f.sc
	switch lbl {
	case callForecast:
		return jsonResponse(200, map[string]any{
			"Total":                 map[string]string{"Amount": sc.Forecast["mean"], "Unit": sc.Currency},
			"ForecastResultsByTime": []map[string]string{{"MeanValue": sc.Forecast["mean"], "PredictionIntervalLowerBound": sc.Forecast["low"], "PredictionIntervalUpperBound": sc.Forecast["high"]}},
		}, ""), nil
	case callCoverage:
		return jsonResponse(200, map[string]any{"SavingsPlansCoverages": []map[string]any{{
			"Coverage": map[string]string{"SpendCoveredBySavingsPlans": sc.Coverage["covered"], "TotalCost": sc.Coverage["total"]},
		}}}, ""), nil
	case "tags":
		status := "Inactive"
		if sc.TagActive {
			status = "Active"
		}
		return jsonResponse(200, map[string]any{"CostAllocationTags": []map[string]string{{"TagKey": "team", "Type": "UserDefined", "Status": status}}}, ""), nil
	case "budgets":
		return jsonResponse(200, map[string]any{"Budgets": sc.Budgets}, ""), nil
	case "hub":
		return jsonResponse(200, map[string]any{"items": sc.Recommendations}, ""), nil
	}
	return jsonResponse(200, f.costAndUsage(in, lbl == callResources), ""), nil
}

// costAndUsage aggregates the scenario rows the way Cost Explorer does.
func (f *fakeAWS) costAndUsage(in ceRequest, resources bool) map[string]any {
	start, _ := finops.ParseDate(in.TimePeriod.Start)
	end, _ := finops.ParseDate(in.TimePeriod.End)
	type agg struct {
		keys      []string
		cost, qty int64
		unit      string
	}
	byDay := map[finops.Date]map[string]*agg{}
	accounts := map[string]bool{}
	for _, r := range f.sc.rows() {
		if r.day.Before(start) || !r.day.Before(end) || !in.Filter.match(r) {
			continue
		}
		if resources && r.resource == "" {
			continue
		}
		var keys []string
		for _, g := range in.GroupBy {
			if g.Type == "TAG" {
				keys = append(keys, g.Key+"$"+r.team)
			} else {
				keys = append(keys, dimValue(r, g.Key))
			}
			if g.Key == "LINKED_ACCOUNT" {
				accounts[r.account] = true
			}
		}
		k := strings.Join(keys, "\x00")
		if byDay[r.day] == nil {
			byDay[r.day] = map[string]*agg{}
		}
		a := byDay[r.day][k]
		if a == nil {
			a = &agg{keys: keys}
			byDay[r.day][k] = a
		}
		a.cost += r.cost
		a.qty += r.qty
		// Like Cost Explorer: one unit when the group has one, else N/A.
		switch {
		case a.unit == "":
			a.unit = r.unit
		case a.unit != r.unit:
			a.unit = "N/A"
		}
	}
	type flat struct {
		day   finops.Date
		group map[string]any
	}
	var all []flat
	var days []finops.Date
	for d := start; d.Before(end); d = d.AddDays(1) {
		days = append(days, d)
		var ks []string
		for k := range byDay[d] {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			a := byDay[d][k]
			all = append(all, flat{d, map[string]any{"Keys": a.keys, "Metrics": map[string]any{
				"NetAmortizedCost": map[string]string{"Amount": finops.FormatMicros(a.cost), "Unit": f.sc.Currency},
				"UsageQuantity":    map[string]string{"Amount": finops.FormatMicros(a.qty), "Unit": unitOr(a.unit)},
			}}})
		}
	}
	offset := 0
	if in.NextPageToken != nil {
		offset, _ = strconv.Atoi(*in.NextPageToken)
	}
	page := all[offset:]
	var next any
	if f.pageGroups > 0 && len(page) > f.pageGroups {
		page = page[:f.pageGroups]
		next = strconv.Itoa(offset + f.pageGroups)
	}
	groups := map[finops.Date][]map[string]any{}
	for _, fl := range page {
		groups[fl.day] = append(groups[fl.day], fl.group)
	}
	var results []map[string]any
	for _, d := range days {
		if offset > 0 && len(groups[d]) == 0 {
			continue
		}
		results = append(results, map[string]any{
			"TimePeriod": map[string]string{"Start": d.String(), "End": d.AddDays(1).String()},
			"Groups":     groups[d],
			"Total":      map[string]any{},
			"Estimated":  !f.sc.PartialFrom.IsZero() && !d.Before(f.sc.PartialFrom),
		})
	}
	var attrs []map[string]any
	var ids []string
	for id := range accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if n := f.sc.Names[id]; n != "" {
			attrs = append(attrs, map[string]any{"Value": id, "Attributes": map[string]string{"description": n}})
		}
	}
	out := map[string]any{"ResultsByTime": results, "DimensionValueAttributes": attrs, "GroupDefinitions": in.GroupBy}
	if next != nil {
		out["NextPageToken"] = next
	}
	return out
}

func jsonResponse(status int, v any, errType string) *http.Response {
	data, _ := json.Marshal(v)
	h := http.Header{"Content-Type": []string{"application/x-amz-json-1.1"}}
	if errType != "" {
		h.Set("X-Amzn-Errortype", errType)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(bytes.NewReader(data)), ContentLength: int64(len(data))}
}

// testClock is a settable clock.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

func at(day string, hour int) time.Time {
	d := finops.MustDate(day)
	return time.Date(d.Year, d.Month, d.Day, hour, 0, 0, 0, time.UTC)
}

type harness struct {
	fake     *fakeAWS
	provider storage.Provider
	clock    *testClock
	adapter  *Adapter
	opts     Options
}

func newHarness(t *testing.T, sc *scenario, mutate func(*Options)) *harness {
	t.Helper()
	h := &harness{fake: newFake(t, sc), provider: storagefix.Provider(t), clock: &testClock{now: at("2026-11-09", 10)}}
	h.opts = Options{
		Deployment:     "prod",
		Payers:         []Payer{{AccountID: "111111111111", Credentials: testCreds}},
		Connected:      []string{"111111111111"},
		TagAllowlisted: true,
		HTTPClient:     h.fake,
		Now:            h.clock.Now,
	}
	if mutate != nil {
		mutate(&h.opts)
	}
	a, err := New(h.provider, tenancy.DefaultOrgScope(), h.opts)
	if err != nil {
		t.Fatal(err)
	}
	h.adapter = a
	return h
}

// removePayer marks a payer removed at revision and purges it, as deleting
// its account does when no refresh is running.
func (h *harness) removePayer(payer string, revision uint64) error {
	if err := h.adapter.MarkPayerRemoved(ctx, payer, revision); err != nil {
		return err
	}
	purged, err := h.adapter.PurgePayer(ctx, payer, 5*time.Second)
	if err == nil && !purged {
		return errors.New("the payer purge did not run")
	}
	return err
}

// testCreds is a fixed test credential; it never reaches a real host.
var testCreds = credsFor("AKIDTEST")

// credsFor signs as the given key ID, so the fake can tell payers apart.
func credsFor(keyID string) aws.CredentialsProvider {
	return aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: keyID, SecretAccessKey: "secret", Source: "test"}, nil
	})
}

// signer reads the access key ID from the SigV4 Authorization header.
func signer(req *http.Request) string {
	_, rest, ok := strings.Cut(req.Header.Get("Authorization"), "Credential=")
	if !ok {
		return ""
	}
	id, _, _ := strings.Cut(rest, "/")
	return id
}

const computeOptimizerTarget = "ComputeOptimizerService.GetEC2InstanceRecommendations"

// computeOptimizer answers in the RPC v2 CBOR protocol Compute Optimizer
// speaks.
func (f *fakeAWS) computeOptimizer() *http.Response {
	var recs cbor.List
	for _, r := range f.sc.ComputeOptimizer {
		recs = append(recs, cbor.Map{
			"accountId": cbor.String(r.Account), "instanceArn": cbor.String(r.InstanceARN), "instanceName": cbor.String(r.Name),
			"currentInstanceType": cbor.String(r.Current), "finding": cbor.String(r.Finding),
			"recommendationOptions": cbor.List{cbor.Map{"instanceType": cbor.String(r.Target), "rank": cbor.Uint(1),
				"savingsOpportunity": cbor.Map{"estimatedMonthlySavings": cbor.Map{"currency": cbor.String("USD"), "value": cbor.Float64(r.Monthly)}}}},
		})
	}
	if recs == nil {
		recs = cbor.List{}
	}
	return cborResponse(200, cbor.Map{"instanceRecommendations": recs})
}

func cborResponse(status int, v cbor.Value) *http.Response {
	data := cbor.Encode(v)
	h := http.Header{"Content-Type": []string{"application/cbor"}, "Smithy-Protocol": []string{"rpc-v2-cbor"}}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(bytes.NewReader(data)), ContentLength: int64(len(data))}
}

// loadInventory seeds a snapshot store with the scenario's inventory and
// change history.
func loadInventory(t *testing.T, sc *scenario) *snapshot.Store {
	t.Helper()
	// The story's change history is weeks old: pin the store's clock to the
	// story's day and keep two months of changes.
	store := snapshot.New(storage.NewMemory(), tenancy.DefaultOrgScope(),
		snapshot.WithClock(func() time.Time { return at("2026-11-09", 10) }),
		snapshot.WithOptions(func() snapshot.Options { return snapshot.Options{ChangeRetention: 60 * 24 * time.Hour} }))
	units := map[model.Unit][]model.Resource{}
	for _, r := range sc.Inventory {
		u := model.Unit{Account: r.Account, Region: r.Region, Service: r.Service}
		units[u] = append(units[u], r)
	}
	for u, rs := range units {
		if _, err := store.Commit(u, snapshot.Candidate{Resources: rs}); err != nil {
			t.Fatalf("commit %v: %v", u, err)
		}
	}
	byRegion := map[[2]string][]model.Change{}
	for _, c := range sc.Changes {
		k := [2]string{c.Account, c.Region}
		byRegion[k] = append(byRegion[k], c)
	}
	for k, cs := range byRegion {
		if err := store.AppendChanges(k[0], k[1], cs, sc.ChangeCoverage, ""); err != nil {
			t.Fatalf("changes: %v", err)
		}
	}
	return store
}

func unitOr(u string) string {
	if u == "" {
		return "N/A"
	}
	return u
}

// signed is how many requests the given access key ID signed.
func (f *fakeAWS) signed(keyID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bySigner[keyID]
}

// paidSigned is how many paid cost requests the given access key ID signed.
func (f *fakeAWS) paidSigned(keyID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paidSigner[keyID]
}

func paidLabel(lbl string) bool {
	switch lbl {
	case callAccount, callUsage, callTeam, callRecord, callForecast, callCoverage, callDrill, callResources:
		return true
	}
	return false
}
