package finops

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/tests/fixtures/finopsfix"
)

// fakeSource is an in-memory cost cache.
type fakeSource struct {
	status    SourceStatus
	lines     []CostLine
	resources []CostLine
	savings   SavingsInputs
	calls     int
}

func (f *fakeSource) Status(context.Context) (SourceStatus, error) { return f.status, nil }

func (f *fakeSource) Lines(_ context.Context, r DateRange) ([]CostLine, error) {
	f.calls++
	var out []CostLine
	for _, l := range f.lines {
		if r.Contains(l.Day) {
			out = append(out, l)
		}
	}
	return out, nil
}

func (f *fakeSource) ResourceLines(_ context.Context, r DateRange) ([]CostLine, error) {
	var out []CostLine
	for _, l := range f.resources {
		if r.Contains(l.Day) {
			out = append(out, l)
		}
	}
	return out, nil
}

func (f *fakeSource) SavingsInputs(context.Context) (SavingsInputs, error) { return f.savings, nil }

// memStore is an in-memory Store for tests.
type memStore struct {
	mu        sync.Mutex
	increases map[string]IncreaseRecord
	savings   map[string]SavingState
	orphaned  map[string]time.Time
	budgets   []Budget
	reports   map[string]ReportView
	alerts    map[string]BudgetAlertState
}

func newMemStore() *memStore {
	return &memStore{increases: map[string]IncreaseRecord{}, savings: map[string]SavingState{}, orphaned: map[string]time.Time{}, reports: map[string]ReportView{}, alerts: map[string]BudgetAlertState{}}
}

func (m *memStore) ListIncreases(context.Context) ([]IncreaseRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []IncreaseRecord
	for _, k := range sortedKeys(m.increases) {
		out = append(out, m.increases[k])
	}
	return out, nil
}

func (m *memStore) PutIncrease(_ context.Context, r IncreaseRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.increases[r.ID] = r
	return nil
}

func (m *memStore) SavingStates(context.Context) (map[string]SavingState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]SavingState{}
	for k, v := range m.savings {
		out[k] = v
	}
	return out, nil
}

func (m *memStore) PutSavingState(_ context.Context, id string, st SavingState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.savings[id] = st
	return nil
}

func (m *memStore) SyncSavingStates(_ context.Context, present []Saving, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	keep := map[string]bool{}
	for _, it := range present {
		keep[it.ID] = true
	}
	for id := range m.savings {
		if keep[id] {
			delete(m.orphaned, id)
		} else if _, marked := m.orphaned[id]; !marked {
			m.orphaned[id] = at
		}
	}
	return nil
}

func (m *memStore) Budgets(context.Context) ([]Budget, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Budget(nil), m.budgets...), nil
}

func (m *memStore) PutBudgets(_ context.Context, bs []Budget) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.budgets = append([]Budget(nil), bs...)
	return nil
}

func (m *memStore) GetReport(_ context.Context, month string) (*ReportView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.reports[month]; ok {
		return &r, nil
	}
	return nil, nil
}

func (m *memStore) PutReport(_ context.Context, r ReportView) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reports[r.Period.Calendar.Start.MonthKey()] = r
	return nil
}

func (m *memStore) ListReports(context.Context) ([]ReportRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ReportRef
	for _, k := range sortedKeys(m.reports) {
		r := m.reports[k]
		out = append(out, ReportRef{Month: k, Frozen: *r.Frozen, Total: r.Summary.Total})
	}
	return out, nil
}

// Account IDs of the reference scenario, for assertions.
const (
	acctProd    = "111111111111"
	acctData    = "222222222222"
	acctSandbox = "333333333333"
)

func usd(v int64) Micros { return v * MicrosPerUnit }

// line is a compact constructor for inline rule cases.
func line(day Date, account, service, usage, record, team string, cost Micros, qty int64) CostLine {
	return CostLine{Day: day, Account: account, Region: "us-east-1", Service: service, UsageType: usage,
		RecordType: record, Team: team, NetAmortizedMicros: cost, QuantityMicros: qty}
}

// storyStep sets a series' daily cost and quantity from a day on.
type storyStep struct {
	From     Date   `json:"from"`
	Cost     Micros `json:"cost_micros"`
	Quantity int64  `json:"quantity_micros"`
}

type storySeries struct {
	Line  CostLine    `json:"line"`
	Steps []storyStep `json:"steps"`
}

type storySuspect struct {
	Change SuspectView  `json:"change"`
	Signal *UsageSignal `json:"signal"`
}

type storyData struct {
	SeriesRange   DateRange     `json:"series_range"`
	Series        []storySeries `json:"series"`
	Lines         []CostLine    `json:"lines"`
	ResourceRange DateRange     `json:"resource_range"`
	Resources     []CostLine    `json:"resources"`
	Status        SourceStatus  `json:"status"`
	Savings       SavingsInputs `json:"savings"`
	Config        struct {
		Budgets       []Budget          `json:"budgets"`
		AccountOwners map[string]string `json:"account_owners"`
		ProviderName  string            `json:"provider_name"`
		UIPath        string            `json:"ui_path"`
		Now           time.Time         `json:"now"`
	} `json:"config"`
	Suspects map[string]storySuspect `json:"suspects"`
}

// loadStory decodes the shared reference story (finopsfix.NATStory: a NAT
// step on Oct 9 after a release, a new untagged database replica, a storage
// price change, a one-off support fee, credits, a steady compute line, a tiny
// logs line, one unreadable account, and a partial day after the last
// complete day) into the core types.
func loadStory() storyData {
	raw, err := json.Marshal(finopsfix.NATStory())
	if err != nil {
		panic(err)
	}
	var d storyData
	if err := json.Unmarshal(raw, &d); err != nil {
		panic(err)
	}
	return d
}

// fakeSuspects names the change behind an increase from the scenario's
// suspects, keyed by neutral service.
type fakeSuspects struct{ byService map[string]storySuspect }

func (f fakeSuspects) Suspect(_ context.Context, d Detected) (SuspectView, *UsageSignal, error) {
	if s, ok := f.byService[d.Key.Service]; ok {
		return s.Change, s.Signal, nil
	}
	return SuspectView{State: "none_found"}, nil, nil
}

// natStory expands the reference scenario into daily lines.
func natStory() (*fakeSource, Config) {
	d := loadStory()
	src := &fakeSource{status: d.Status, savings: d.Savings}
	d.SeriesRange.Each(func(day Date) {
		for _, s := range d.Series {
			var step *storyStep
			for i := range s.Steps {
				if !day.Before(s.Steps[i].From) {
					step = &s.Steps[i]
				}
			}
			if step == nil {
				continue
			}
			l := s.Line
			l.Day, l.NetAmortizedMicros, l.QuantityMicros = day, step.Cost, step.Quantity
			src.lines = append(src.lines, l)
		}
	})
	src.lines = append(src.lines, d.Lines...)
	d.ResourceRange.Each(func(day Date) {
		for _, r := range d.Resources {
			r.Day = day
			src.resources = append(src.resources, r)
		}
	})
	sort.SliceStable(src.lines, func(i, j int) bool { return src.lines[i].Day.Before(src.lines[j].Day) })
	now := d.Config.Now
	cfg := Config{
		Budgets:       d.Config.Budgets,
		AccountOwners: d.Config.AccountOwners,
		ProviderName:  d.Config.ProviderName,
		UIPath:        d.Config.UIPath,
		Now:           func() time.Time { return now },
	}
	return src, cfg
}

// storySuspects is the scenario's suspect finder.
func storySuspects() fakeSuspects { return fakeSuspects{byService: loadStory().Suspects} }

func (m *memStore) BudgetAlertStates(context.Context) (map[string]BudgetAlertState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]BudgetAlertState{}
	for k, v := range m.alerts {
		out[k] = v
	}
	return out, nil
}

func (m *memStore) PutBudgetAlertState(_ context.Context, id string, st BudgetAlertState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.alerts[id] = st
	return nil
}
