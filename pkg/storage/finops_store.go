package storage

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"time"

	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// ErrInvalidFinOps reports a cost line, record or query the FinOps store
// cannot hold or answer as given.
var ErrInvalidFinOps = errors.New("storage: invalid finops data")

// ErrAmountOverflow reports a sum of stored amounts that does not fit a
// signed 64-bit micro-unit total. The store returns it instead of a dialect
// error, a wrapped number or a clamped one.
var ErrAmountOverflow = errors.New("storage: amount sum out of range")

// FinOpsStore keeps FinOps cost lines, increases, reports and saving
// decisions in indexed tables. It is optional: a provider that does not
// implement it reports FinOps data as not ready, and callers find it by type
// assertion like EvidenceStore.
//
// Writes go to scope.Write. Reads resolve the ordered scope.Read: a payer's
// day is answered by the first organization holding cost lines for it, and
// an increase, report or decision by the first organization holding its ID.
// Days are "YYYY-MM-DD", months "YYYY-MM".
type FinOpsStore interface {
	// ReplaceFinOpsCostDays replaces the payer's lines for every listed day in
	// one transaction: those days are deleted and lines inserted. Every line
	// must fall on a listed day and lines must not repeat a key. A listed day
	// without lines is left empty.
	ReplaceFinOpsCostDays(ctx context.Context, scope tenancy.OrgScope, payer string, days []string, lines []FinOpsCostLine) error
	// DeleteFinOpsPayer deletes every cost line of a payer.
	DeleteFinOpsPayer(ctx context.Context, scope tenancy.OrgScope, payer string) error
	// DeleteFinOpsIncreasesForPayer deletes the payer's increases with their
	// events. It is idempotent and returns the number of increases deleted.
	// The payer's saving decisions are deleted through
	// FinOpsSavingDecisionStore.
	DeleteFinOpsIncreasesForPayer(ctx context.Context, scope tenancy.OrgScope, payer string) (int64, error)
	// PruneFinOpsCostLines deletes cost lines before the day.
	PruneFinOpsCostLines(ctx context.Context, scope tenancy.OrgScope, before string) (int64, error)
	// FinOpsCostLines lists matching lines ordered by day, payer, account,
	// region, service, service name, usage type, record type, team and
	// resource, returning at most query.Limit of them (all when it is 0).
	FinOpsCostLines(ctx context.Context, scope tenancy.OrgScope, query FinOpsCostQuery) ([]FinOpsCostLine, error)
	// FinOpsCostTotals sums matching lines grouped by the dimensions, ordered
	// by them. No dimension returns one grand total (none when nothing
	// matches). It ignores query.Limit and sums every matching line.
	FinOpsCostTotals(ctx context.Context, scope tenancy.OrgScope, query FinOpsCostQuery, groupBy ...FinOpsDimension) ([]FinOpsCostTotal, error)

	// PutFinOpsIncrease inserts or replaces an increase and its events.
	PutFinOpsIncrease(ctx context.Context, scope tenancy.OrgScope, increase FinOpsIncrease) error
	// FinOpsIncreases lists matching increases ordered by start day and ID,
	// each with its events in order.
	FinOpsIncreases(ctx context.Context, scope tenancy.OrgScope, query FinOpsIncreaseQuery) ([]FinOpsIncrease, error)

	// PutFinOpsReport inserts or replaces a month's report.
	PutFinOpsReport(ctx context.Context, scope tenancy.OrgScope, report FinOpsReport) error
	// FinOpsReport returns one month's report.
	FinOpsReport(ctx context.Context, scope tenancy.OrgScope, month string) (FinOpsReport, bool, error)
	// FinOpsReports lists every report without its model, ordered by month.
	FinOpsReports(ctx context.Context, scope tenancy.OrgScope) ([]FinOpsReport, error)

	// PutFinOpsSavingDecision inserts or replaces a saving decision.
	PutFinOpsSavingDecision(ctx context.Context, scope tenancy.OrgScope, decision FinOpsSavingDecision) error
	// FinOpsSavingDecisions lists every decision ordered by saving ID,
	// orphaned ones included, so an audit can still read them.
	FinOpsSavingDecisions(ctx context.Context, scope tenancy.OrgScope) ([]FinOpsSavingDecision, error)
}

// FinOpsSavingDecisionStore keeps saving decisions in step with the savings
// they refer to. A saving is recomputed from cost and inventory data, so it
// can stop being produced while its decision is still stored. It is optional
// and found by type assertion on a FinOpsStore, so a store that only passes
// FinOpsStore through keeps working without this upkeep.
type FinOpsSavingDecisionStore interface {
	// SyncFinOpsSavingDecisions compares the write organization's decisions
	// with the saving IDs currently produced: a decision whose saving is
	// absent is marked orphaned at the given time (an earlier mark is kept),
	// and the mark is cleared from one whose saving is present again. It
	// returns how many it marked and cleared.
	SyncFinOpsSavingDecisions(ctx context.Context, scope tenancy.OrgScope, present []string, at time.Time) (orphaned, restored int64, err error)
	// DeleteFinOpsSavingDecisionsForPayer deletes the payer's decisions. It
	// is idempotent and returns the number deleted.
	DeleteFinOpsSavingDecisionsForPayer(ctx context.Context, scope tenancy.OrgScope, payer string) (int64, error)
	// PruneFinOpsSavingDecisions deletes decisions orphaned before the time.
	// A decision whose saving is still produced is never pruned.
	PruneFinOpsSavingDecisions(ctx context.Context, scope tenancy.OrgScope, orphanedBefore time.Time) (int64, error)
}

// FinOpsSavingDecisionSubjects fills in what a decision refers to when it
// was stored without it: a decision made before the subject columns existed,
// or one whose payer was not known yet. It is optional and found by type
// assertion, like FinOpsSavingDecisionStore.
type FinOpsSavingDecisionSubjects interface {
	// FillFinOpsSavingDecisionSubjects updates the write organization's
	// decisions whose payer is empty, one per subject's SavingID: the payer
	// is set, and account, kind and title are set where the decision has
	// none, so what was recorded at decision time is kept. Only the subject
	// columns change. A subject without a SavingID or without anything to
	// fill is ignored. It returns how many decisions changed.
	FillFinOpsSavingDecisionSubjects(ctx context.Context, scope tenancy.OrgScope, subjects []FinOpsSavingDecision) (int64, error)
	// DeleteFinOpsSavingDecisionsWithoutPayer deletes the write
	// organization's decisions whose payer is empty and whose account is
	// one of accounts: the decisions of a removed payer that were never
	// filled. A decision with a payer is never touched. It is idempotent and
	// returns the number deleted.
	DeleteFinOpsSavingDecisionsWithoutPayer(ctx context.Context, scope tenancy.OrgScope, accounts []string) (int64, error)
}

// FinOpsRollupStore keeps the cost rollups, which a store may answer totals
// from, in agreement with the cost lines. A process that cannot know what
// wrote the lines before it started (an older version that wrote only the
// lines, an interrupted fill) reads every total from the lines until it has
// reconciled once. It is optional and found by type assertion; a store
// without it never reads totals from a rollup that could disagree.
type FinOpsRollupStore interface {
	// ReconcileFinOpsRollups compares whole daily rollup rows with the lines
	// without a resource ID summed by the rollup key (every sum, count and
	// flag), rebuilds every organization × payer × day that differs, each in
	// its own transaction, then every month whose monthly rows differ from
	// its days. On success this process reads totals from the rollups. The
	// caller serializes it with every other cost-line writer.
	ReconcileFinOpsRollups(ctx context.Context) (FinOpsRollupRepair, error)
	// MarkFinOpsRollupsReconciled lets this process read the rollups without
	// comparing. The cost adapter never calls it: each process reconciles
	// for itself.
	MarkFinOpsRollupsReconciled()
	// FinOpsRollupsReconciled reports whether this process reads totals from
	// the rollups.
	FinOpsRollupsReconciled() bool
}

// FinOpsRollupRepair is what a reconcile found and rebuilt.
type FinOpsRollupRepair struct {
	// Days is how many organization × payer × day groups differed.
	Days int
	// RebuiltDays and RebuiltMonths are how many were rebuilt.
	RebuiltDays   int
	RebuiltMonths int
	// Vacuumed is true when the rebuild was large enough that the reconcile
	// then vacuumed and analyzed the rollup tables (Postgres only).
	Vacuumed bool
}

// FinOps returns the provider's FinOps store, if it has one.
func FinOps(provider Provider) (FinOpsStore, bool) {
	store, ok := provider.(FinOpsStore)
	return store, ok
}

// FinOpsCostLine is one stored cost line. The key is payer, day, account,
// region, service, service name, usage type, record type, team and resource
// ID; an empty team or resource ID means none. ServiceName is the provider's
// service, so provider services sharing a neutral Service stay apart.
type FinOpsCostLine struct {
	Payer              string    `json:"payer"`
	Day                string    `json:"day"`
	Account            string    `json:"account"`
	Region             string    `json:"region"`
	Service            string    `json:"service"`
	UsageType          string    `json:"usage_type"`
	RecordType         string    `json:"record_type"`
	Team               string    `json:"team"`
	ResourceID         string    `json:"resource_id"`
	ServiceName        string    `json:"service_name,omitempty"`
	UsageGroup         string    `json:"usage_group,omitempty"`
	App                string    `json:"app,omitempty"`
	Env                string    `json:"env,omitempty"`
	Currency           string    `json:"currency,omitempty"`
	NetAmortizedMicros int64     `json:"net_amortized_micros"`
	QuantityMicros     int64     `json:"quantity_micros"`
	Unit               string    `json:"unit,omitempty"`
	EstimatedTeam      bool      `json:"estimated_team,omitempty"`
	EstimatedRegion    bool      `json:"estimated_region,omitempty"`
	RefreshedAt        time.Time `json:"refreshed_at,omitzero"`
}

// FinOpsCostQuery selects cost lines. Days are inclusive; an empty field
// matches everything. Team and ResourceID are pointers so "none" ("") can be
// selected.
//
// Accounts, Regions and Teams match any listed value and are ignored when
// empty. They narrow the single-value fields and the read scope instead of
// replacing them, so a query holding both requires both. A Teams entry of ""
// selects the lines with no team, like the Team convention; an Accounts or
// Regions entry of "" selects the lines stored without one.
//
// Limit caps the lines a read returns, in the store's order; 0 returns every
// match. A bounded read is a page, not a total: FinOpsCostTotals ignores
// Limit and always sums every match, so an incomplete page can never pass
// for the whole set.
type FinOpsCostQuery struct {
	From        string
	Through     string
	Payer       string
	Account     string
	Region      string
	Service     string
	ServiceName string
	UsageType   string
	RecordType  string
	Team        *string
	ResourceID  *string
	Accounts    []string
	Regions     []string
	Teams       []string
	Limit       int
}

// FinOpsDimension is a column cost totals can be grouped by.
type FinOpsDimension string

// The grouping dimensions.
const (
	FinOpsByDay         FinOpsDimension = "day"
	FinOpsByMonth       FinOpsDimension = "month"
	FinOpsByPayer       FinOpsDimension = "payer"
	FinOpsByAccount     FinOpsDimension = "account"
	FinOpsByRegion      FinOpsDimension = "region"
	FinOpsByService     FinOpsDimension = "service"
	FinOpsByServiceName FinOpsDimension = "service_name"
	FinOpsByUsageType   FinOpsDimension = "usage_type"
	FinOpsByRecordType  FinOpsDimension = "record_type"
	FinOpsByTeam        FinOpsDimension = "team"
	FinOpsByResourceID  FinOpsDimension = "resource_id"
	FinOpsByUsageGroup  FinOpsDimension = "usage_group"
	FinOpsByUnit        FinOpsDimension = "unit"
)

// FinOpsCostTotal is one group of a cost aggregation. Only the grouped
// dimensions are set. Estimated flags are true when any summed line was.
// MinDay is always the earliest day in the group, grouped or not, so a
// run-rate or forecast view can price a group without reading its lines.
type FinOpsCostTotal struct {
	Day                string `json:"day,omitempty"`
	Month              string `json:"month,omitempty"`
	Payer              string `json:"payer,omitempty"`
	Account            string `json:"account,omitempty"`
	Region             string `json:"region,omitempty"`
	Service            string `json:"service,omitempty"`
	ServiceName        string `json:"service_name,omitempty"`
	UsageType          string `json:"usage_type,omitempty"`
	RecordType         string `json:"record_type,omitempty"`
	Team               string `json:"team,omitempty"`
	ResourceID         string `json:"resource_id,omitempty"`
	UsageGroup         string `json:"usage_group,omitempty"`
	Unit               string `json:"unit,omitempty"`
	MinDay             string `json:"min_day,omitempty"`
	NetAmortizedMicros int64  `json:"net_amortized_micros"`
	QuantityMicros     int64  `json:"quantity_micros"`
	Lines              int64  `json:"lines"`
	EstimatedTeam      bool   `json:"estimated_team,omitempty"`
	EstimatedRegion    bool   `json:"estimated_region,omitempty"`
}

// FinOpsIncrease is one cost increase. Suspects and Detail are the caller's
// documents.
type FinOpsIncrease struct {
	ID                  string                `json:"id"`
	Payer               string                `json:"payer,omitempty"`
	Account             string                `json:"account"`
	Service             string                `json:"service"`
	UsageType           string                `json:"usage_type,omitempty"`
	RecordType          string                `json:"record_type,omitempty"`
	ResourceID          string                `json:"resource_id,omitempty"`
	StartedOn           string                `json:"started_on"`
	ResolvedOn          string                `json:"resolved_on,omitempty"`
	Status              string                `json:"status"`
	OwnerTeam           string                `json:"owner_team,omitempty"`
	OwnerRule           string                `json:"owner_rule,omitempty"`
	OwnerEstimated      bool                  `json:"owner_estimated,omitempty"`
	MonthlyImpactMicros int64                 `json:"monthly_impact_micros"`
	ExpectedReason      string                `json:"expected_reason,omitempty"`
	ExpectedUntil       string                `json:"expected_until,omitempty"`
	AlertedAt           time.Time             `json:"alerted_at,omitzero"`
	Suspects            json.RawMessage       `json:"suspects,omitempty"`
	Detail              json.RawMessage       `json:"detail,omitempty"`
	Events              []FinOpsIncreaseEvent `json:"events,omitempty"`
}

// FinOpsIncreaseEvent is one status change of an increase.
type FinOpsIncreaseEvent struct {
	At         time.Time `json:"at"`
	Actor      string    `json:"actor"`
	FromStatus string    `json:"from_status"`
	ToStatus   string    `json:"to_status"`
	Reason     string    `json:"reason,omitempty"`
	Until      string    `json:"until,omitempty"`
}

// FinOpsIncreaseQuery selects increases. Empty fields match everything.
type FinOpsIncreaseQuery struct {
	IDs       []string
	Statuses  []string
	OwnerTeam string
	Service   string
}

// FinOpsReport is one month's report document.
type FinOpsReport struct {
	Month       string          `json:"month"`
	FrozenAt    time.Time       `json:"frozen_at,omitzero"`
	FrozenBy    string          `json:"frozen_by,omitempty"`
	TotalMicros int64           `json:"total_micros"`
	Model       json.RawMessage `json:"model,omitempty"`
}

// FinOpsSavingDecision is a decision on one saving. Payer, Account, Kind and
// Title are what the saving was when the decision was made, so the decision
// still names it after the saving stops being produced. OrphanedAt is set
// while the saving is not produced; a decision is written with it zero.
type FinOpsSavingDecision struct {
	SavingID   string    `json:"saving_id"`
	State      string    `json:"state"`
	Reason     string    `json:"reason,omitempty"`
	Actor      string    `json:"actor,omitempty"`
	At         time.Time `json:"at,omitzero"`
	Payer      string    `json:"payer,omitempty"`
	Account    string    `json:"account,omitempty"`
	Kind       string    `json:"kind,omitempty"`
	Title      string    `json:"title,omitempty"`
	OrphanedAt time.Time `json:"orphaned_at,omitzero"`
}

func validFinOpsDay(day string) bool {
	if len(day) != 10 {
		return false
	}
	parsed, err := time.Parse(time.DateOnly, day)
	return err == nil && parsed.Format(time.DateOnly) == day
}

func validFinOpsMonth(month string) bool {
	if len(month) != 7 {
		return false
	}
	parsed, err := time.Parse("2006-01", month)
	return err == nil && parsed.Format("2006-01") == month
}

func validOptionalFinOpsDay(day string) bool { return day == "" || validFinOpsDay(day) }

// finOpsLineKey is the key of a cost line within its organization.
type finOpsLineKey struct {
	payer, day, account, region, service, serviceName, usageType, recordType, team, resourceID string
}

func (line FinOpsCostLine) key() finOpsLineKey {
	return finOpsLineKey{line.Payer, line.Day, line.Account, line.Region, line.Service, line.ServiceName, line.UsageType, line.RecordType, line.Team, line.ResourceID}
}

func lessFinOpsLine(left, right FinOpsCostLine) bool {
	a, b := left.key(), right.key()
	for _, pair := range [][2]string{{a.day, b.day}, {a.payer, b.payer}, {a.account, b.account}, {a.region, b.region}, {a.service, b.service}, {a.serviceName, b.serviceName}, {a.usageType, b.usageType}, {a.recordType, b.recordType}, {a.team, b.team}, {a.resourceID, b.resourceID}} {
		if pair[0] != pair[1] {
			return pair[0] < pair[1]
		}
	}
	return false
}

// prepareFinOpsDays validates a day replacement and returns its days, sorted
// and unique, and its cleaned lines.
func prepareFinOpsDays(payer string, days []string, lines []FinOpsCostLine) ([]string, []FinOpsCostLine, error) {
	if !validTableKey(payer) {
		return nil, nil, ErrInvalidFinOps
	}
	listed := make(map[string]bool, len(days))
	for _, day := range days {
		if !validFinOpsDay(day) {
			return nil, nil, ErrInvalidFinOps
		}
		listed[day] = true
	}
	unique := make([]string, 0, len(listed))
	for day := range listed {
		unique = append(unique, day)
	}
	sort.Strings(unique)
	seen := make(map[finOpsLineKey]bool, len(lines))
	cleaned := make([]FinOpsCostLine, 0, len(lines))
	for _, line := range lines {
		line.Payer = payer
		if !listed[line.Day] || !validTableStrings(line.Account, line.Region, line.Service, line.UsageType, line.RecordType, line.Team, line.ResourceID,
			line.ServiceName, line.UsageGroup, line.App, line.Env, line.Currency, line.Unit) {
			return nil, nil, ErrInvalidFinOps
		}
		key := line.key()
		if seen[key] {
			return nil, nil, ErrInvalidFinOps
		}
		seen[key] = true
		line.RefreshedAt = tableTime(line.RefreshedAt)
		cleaned = append(cleaned, line)
	}
	return unique, cleaned, nil
}

func cleanFinOpsIncrease(increase FinOpsIncrease) (FinOpsIncrease, error) {
	if !validTableKey(increase.ID) || !validFinOpsDay(increase.StartedOn) || !validOptionalFinOpsDay(increase.ResolvedOn) || !validOptionalFinOpsDay(increase.ExpectedUntil) ||
		!validTableStrings(increase.Payer, increase.Account, increase.Service, increase.UsageType, increase.RecordType, increase.ResourceID, increase.Status,
			increase.OwnerTeam, increase.OwnerRule, increase.ExpectedReason) || !validTableJSON(increase.Suspects) || !validTableJSON(increase.Detail) {
		return FinOpsIncrease{}, ErrInvalidFinOps
	}
	increase.AlertedAt = tableTime(increase.AlertedAt)
	increase.Suspects = cloneTableJSON(increase.Suspects)
	increase.Detail = cloneTableJSON(increase.Detail)
	events := make([]FinOpsIncreaseEvent, 0, len(increase.Events))
	for _, event := range increase.Events {
		if !validOptionalFinOpsDay(event.Until) || !validTableStrings(event.Actor, event.FromStatus, event.ToStatus, event.Reason) {
			return FinOpsIncrease{}, ErrInvalidFinOps
		}
		event.At = tableTime(event.At)
		events = append(events, event)
	}
	increase.Events = nil
	if len(events) > 0 {
		increase.Events = events
	}
	return increase, nil
}

func cleanFinOpsReport(report FinOpsReport) (FinOpsReport, error) {
	if !validFinOpsMonth(report.Month) || emptyJSON(report.Model) || !validTableJSON(report.Model) || !validTableText(report.FrozenBy) {
		return FinOpsReport{}, ErrInvalidFinOps
	}
	report.FrozenAt = tableTime(report.FrozenAt)
	report.Model = cloneTableJSON(report.Model)
	return report, nil
}

func cleanFinOpsDecision(decision FinOpsSavingDecision) (FinOpsSavingDecision, error) {
	if !validTableKey(decision.SavingID) || !validTableStrings(decision.State, decision.Reason, decision.Actor, decision.Payer, decision.Account, decision.Kind, decision.Title) {
		return FinOpsSavingDecision{}, ErrInvalidFinOps
	}
	decision.At = tableTime(decision.At)
	decision.OrphanedAt = tableTime(decision.OrphanedAt)
	return decision, nil
}

// decisionSubjects validates the subjects a fill names and keeps, per saving
// ID, the last one that has something to fill.
func decisionSubjects(subjects []FinOpsSavingDecision) (map[string]FinOpsSavingDecision, error) {
	out := make(map[string]FinOpsSavingDecision, len(subjects))
	for _, subject := range subjects {
		if !validTableKey(subject.SavingID) || !validTableStrings(subject.Payer, subject.Account, subject.Kind, subject.Title) {
			return nil, ErrInvalidFinOps
		}
		if subject.Payer == "" && subject.Account == "" && subject.Kind == "" && subject.Title == "" {
			continue
		}
		out[subject.SavingID] = FinOpsSavingDecision{SavingID: subject.SavingID, Payer: subject.Payer, Account: subject.Account, Kind: subject.Kind, Title: subject.Title}
	}
	return out, nil
}

// decisionAccounts validates the accounts of a delete of decisions without a
// payer. An empty account never names a decision.
func decisionAccounts(accounts []string) (map[string]bool, error) {
	out := make(map[string]bool, len(accounts))
	for _, account := range accounts {
		if account == "" {
			continue
		}
		if !validTableKey(account) {
			return nil, ErrInvalidFinOps
		}
		out[account] = true
	}
	return out, nil
}

// fillDecisionSubject applies a subject to a decision without a payer and
// reports whether anything changed: the payer is taken, and account, kind and
// title only where the decision has none.
func fillDecisionSubject(decision *FinOpsSavingDecision, subject FinOpsSavingDecision) bool {
	if decision.Payer != "" {
		return false
	}
	changed := false
	set := func(field *string, value string) {
		if *field == "" && value != "" {
			*field = value
			changed = true
		}
	}
	set(&decision.Payer, subject.Payer)
	set(&decision.Account, subject.Account)
	set(&decision.Kind, subject.Kind)
	set(&decision.Title, subject.Title)
	return changed
}

// presentSavings validates the saving IDs a decision sync compares against.
func presentSavings(present []string, at time.Time) (map[string]bool, error) {
	if at.IsZero() {
		return nil, ErrInvalidFinOps
	}
	out := make(map[string]bool, len(present))
	for _, id := range present {
		if !validTableKey(id) {
			return nil, ErrInvalidFinOps
		}
		out[id] = true
	}
	return out, nil
}

func validFinOpsQuery(query FinOpsCostQuery) bool {
	return validOptionalFinOpsDay(query.From) && validOptionalFinOpsDay(query.Through) && query.Limit >= 0
}

func validFinOpsDimensions(groupBy []FinOpsDimension) bool {
	seen := make(map[FinOpsDimension]bool, len(groupBy))
	for _, dimension := range groupBy {
		if _, ok := finOpsDimensionColumns[dimension]; !ok || seen[dimension] {
			return false
		}
		seen[dimension] = true
	}
	return true
}

// finOpsDimensionColumns maps each dimension to its column. Month is
// computed by the dialect.
var finOpsDimensionColumns = map[FinOpsDimension]string{
	FinOpsByDay: "day", FinOpsByMonth: "", FinOpsByPayer: "payer", FinOpsByAccount: "account",
	FinOpsByRegion: "region", FinOpsByService: "service", FinOpsByServiceName: "service_name", FinOpsByUsageType: "usage_type",
	FinOpsByRecordType: "record_type", FinOpsByTeam: "team", FinOpsByResourceID: "resource_id",
	FinOpsByUsageGroup: "usage_group", FinOpsByUnit: "unit",
}

func setFinOpsDimension(total *FinOpsCostTotal, dimension FinOpsDimension, value string) {
	switch dimension {
	case FinOpsByDay:
		total.Day = value
	case FinOpsByMonth:
		total.Month = value
	case FinOpsByPayer:
		total.Payer = value
	case FinOpsByAccount:
		total.Account = value
	case FinOpsByRegion:
		total.Region = value
	case FinOpsByService:
		total.Service = value
	case FinOpsByServiceName:
		total.ServiceName = value
	case FinOpsByUsageType:
		total.UsageType = value
	case FinOpsByRecordType:
		total.RecordType = value
	case FinOpsByTeam:
		total.Team = value
	case FinOpsByResourceID:
		total.ResourceID = value
	case FinOpsByUsageGroup:
		total.UsageGroup = value
	case FinOpsByUnit:
		total.Unit = value
	}
}

func finOpsDimensionValue(line FinOpsCostLine, dimension FinOpsDimension) string {
	switch dimension {
	case FinOpsByDay:
		return line.Day
	case FinOpsByMonth:
		return line.Day[:7]
	case FinOpsByPayer:
		return line.Payer
	case FinOpsByAccount:
		return line.Account
	case FinOpsByRegion:
		return line.Region
	case FinOpsByService:
		return line.Service
	case FinOpsByServiceName:
		return line.ServiceName
	case FinOpsByUsageType:
		return line.UsageType
	case FinOpsByRecordType:
		return line.RecordType
	case FinOpsByTeam:
		return line.Team
	case FinOpsByResourceID:
		return line.ResourceID
	case FinOpsByUsageGroup:
		return line.UsageGroup
	case FinOpsByUnit:
		return line.Unit
	}
	return ""
}

// finOpsListed reports whether value passes a list filter: an empty list
// selects everything.
func finOpsListed(values []string, value string) bool {
	return len(values) == 0 || slices.Contains(values, value)
}

func matchFinOpsLine(query FinOpsCostQuery, line FinOpsCostLine) bool {
	switch {
	case query.From != "" && line.Day < query.From,
		query.Through != "" && line.Day > query.Through,
		query.Payer != "" && line.Payer != query.Payer,
		query.Account != "" && line.Account != query.Account,
		query.Region != "" && line.Region != query.Region,
		query.Service != "" && line.Service != query.Service,
		query.ServiceName != "" && line.ServiceName != query.ServiceName,
		query.UsageType != "" && line.UsageType != query.UsageType,
		query.RecordType != "" && line.RecordType != query.RecordType,
		query.Team != nil && line.Team != *query.Team,
		query.ResourceID != nil && line.ResourceID != *query.ResourceID,
		!finOpsListed(query.Accounts, line.Account),
		!finOpsListed(query.Regions, line.Region),
		!finOpsListed(query.Teams, line.Team):
		return false
	}
	return true
}
