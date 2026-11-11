package cost

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"time"

	"github.com/VersusControl/versus-incident/pkg/finops"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// Store keeps the FinOps decisions. Increases with their status history,
// saving decisions and frozen reports live in the FinOps tables; UI budgets
// and budget-alert levels stay small blobs. It implements finops.Store.
type Store struct {
	cache cache
}

var _ finops.Store = (*Store)(nil)

// ErrInvalidID means an ID is unsafe to use in a storage key.
var ErrInvalidID = errors.New("cost: invalid id")

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,80}$`)

// NewStore builds the decision store for the scope.
func NewStore(provider storage.Provider, scope tenancy.OrgScope) *Store {
	return &Store{cache: cache{provider: provider, scope: scope.Normalized()}}
}

func (s *Store) put(kind, id string, v any) error {
	if !idPattern.MatchString(id) {
		return ErrInvalidID
	}
	return s.cache.write(func(org string) string { return itemKey(org, kind, id) }, v)
}

// increaseRow maps an increase to its table row. The columns carry what
// lists filter on; Detail keeps the whole detection so a read gives back
// exactly what was stored, and the status history is the event rows.
func increaseRow(rec finops.IncreaseRecord) (storage.FinOpsIncrease, error) {
	detail := rec
	detail.History = nil
	raw, err := json.Marshal(detail)
	if err != nil {
		return storage.FinOpsIncrease{}, err
	}
	row := storage.FinOpsIncrease{
		ID: rec.ID, Account: rec.Key.Account, Service: rec.Key.Service, UsageType: rec.Key.UsageType,
		RecordType: rec.Key.RecordType, ResourceID: rec.Key.ResourceID, StartedOn: rec.StartedOn.String(),
		Status: rec.Status, OwnerTeam: rec.Team, OwnerEstimated: rec.TeamEstimated,
		MonthlyImpactMicros: int64(rec.MonthlyImpact), Detail: raw,
	}
	if !rec.ResolvedOn.IsZero() {
		row.ResolvedOn = rec.ResolvedOn.String()
	}
	if rec.Expected != nil {
		row.ExpectedReason = rec.Expected.Reason
		if !rec.Expected.Until.IsZero() {
			row.ExpectedUntil = rec.Expected.Until.String()
		}
	}
	if rec.AlertedAt != nil {
		row.AlertedAt = *rec.AlertedAt
	}
	for _, e := range rec.History {
		ev := storage.FinOpsIncreaseEvent{At: e.At, Actor: e.By, FromStatus: e.From, ToStatus: e.To, Reason: e.Reason}
		if !e.Until.IsZero() {
			ev.Until = e.Until.String()
		}
		row.Events = append(row.Events, ev)
	}
	return row, nil
}

func increaseOf(row storage.FinOpsIncrease) (finops.IncreaseRecord, error) {
	var rec finops.IncreaseRecord
	if err := json.Unmarshal(row.Detail, &rec); err != nil {
		return rec, err
	}
	rec.History = nil
	for _, e := range row.Events {
		ev := finops.StatusEvent{At: e.At, By: e.Actor, From: e.FromStatus, To: e.ToStatus, Reason: e.Reason}
		if e.Until != "" {
			until, err := finops.ParseDate(e.Until)
			if err != nil {
				return rec, err
			}
			ev.Until = until
		}
		rec.History = append(rec.History, ev)
	}
	return rec, nil
}

func (s *Store) ListIncreases(ctx context.Context) ([]finops.IncreaseRecord, error) {
	store, err := s.cache.tables()
	if err != nil {
		return nil, err
	}
	rows, err := store.FinOpsIncreases(ctx, s.cache.scope, storage.FinOpsIncreaseQuery{})
	if err != nil {
		return nil, err
	}
	out := make([]finops.IncreaseRecord, 0, len(rows))
	for _, row := range rows {
		rec, err := increaseOf(row)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *Store) PutIncrease(ctx context.Context, rec finops.IncreaseRecord) error {
	if !idPattern.MatchString(rec.ID) {
		return ErrInvalidID
	}
	store, err := s.cache.tables()
	if err != nil {
		return err
	}
	row, err := increaseRow(rec)
	if err != nil {
		return err
	}
	return store.PutFinOpsIncrease(ctx, s.cache.scope, row)
}

// Saving decision states.
const (
	savingDismissed = "dismissed"
	savingOpen      = "open"
)

// SavingStates returns every stored decision, an orphaned one included: the
// FinOps core applies a decision only to an item it lists, so a decision
// whose item is gone is never shown, and one whose item is back applies again
// before the next sync clears its mark.
func (s *Store) SavingStates(ctx context.Context) (map[string]finops.SavingState, error) {
	store, err := s.cache.tables()
	if err != nil {
		return nil, err
	}
	rows, err := store.FinOpsSavingDecisions(ctx, s.cache.scope)
	if err != nil {
		return nil, err
	}
	out := make(map[string]finops.SavingState, len(rows))
	for _, row := range rows {
		out[row.SavingID] = finops.SavingState{Dismissed: row.State == savingDismissed, Reason: row.Reason, By: row.Actor, At: row.At,
			Account: row.Account, Kind: row.Kind, Title: row.Title}
	}
	return out, nil
}

// PutSavingState stores a decision with what it referred to: the item's
// account, kind and title, and the payer whose cost covers the account, so
// deleting that payer deletes the decision.
func (s *Store) PutSavingState(ctx context.Context, id string, st finops.SavingState) error {
	if !idPattern.MatchString(id) {
		return ErrInvalidID
	}
	store, err := s.cache.tables()
	if err != nil {
		return err
	}
	status, err := s.cache.status()
	if err != nil {
		return err
	}
	state := savingOpen
	if st.Dismissed {
		state = savingDismissed
	}
	return store.PutFinOpsSavingDecision(ctx, s.cache.scope, storage.FinOpsSavingDecision{SavingID: id, State: state, Reason: st.Reason, Actor: st.By, At: st.At,
		Payer: payerOf(status, st.Account), Account: st.Account, Kind: st.Kind, Title: st.Title})
}

// SyncSavingStates marks the decisions whose item is gone as orphaned and
// clears the mark of those whose item is back. First, a decision stored
// without a payer takes its present item's payer, account, kind and title,
// so a decision from before they were kept, or made before its account's
// payer was known, is deleted with that payer. A store without decision
// upkeep has nothing to mark or fill.
func (s *Store) SyncSavingStates(ctx context.Context, present []finops.Saving, at time.Time) error {
	store, err := s.cache.tables()
	if err != nil {
		return err
	}
	if subjects, ok := store.(storage.FinOpsSavingDecisionSubjects); ok && len(present) > 0 {
		status, err := s.cache.status()
		if err != nil {
			return err
		}
		fills := make([]storage.FinOpsSavingDecision, 0, len(present))
		for _, it := range present {
			fills = append(fills, storage.FinOpsSavingDecision{SavingID: it.ID, Payer: payerOf(status, it.Account), Account: it.Account, Kind: it.Kind, Title: it.Title})
		}
		if _, err := subjects.FillFinOpsSavingDecisionSubjects(ctx, s.cache.scope, fills); err != nil {
			return err
		}
	}
	upkeep, ok := store.(storage.FinOpsSavingDecisionStore)
	if !ok {
		return nil
	}
	ids := make([]string, 0, len(present))
	for _, it := range present {
		ids = append(ids, it.ID)
	}
	_, _, err = upkeep.SyncFinOpsSavingDecisions(ctx, s.cache.scope, ids, at)
	return err
}

// payerOf names the payer whose cost covers the account, from the refresh
// state: the account itself when it is a payer, else the payer that billed
// it. It is "" for an account no payer has billed, such as one read only for
// its inventory. A saving without an account, such as the discount coverage
// of the whole deployment, belongs to the deployment's only payer; with
// several payers it is "" and the last payer's removal claims it.
func payerOf(st *statusRecord, account string) string {
	if account == "" {
		if len(st.Payers) == 1 {
			for payer := range st.Payers {
				return payer
			}
		}
		return ""
	}
	if _, ok := st.Payers[account]; ok {
		return account
	}
	for _, payer := range sortedKeys(st.Payers) {
		if ps := st.Payers[payer]; ps != nil && ps.Accounts[account] != nil {
			return payer
		}
	}
	return ""
}

func (s *Store) Budgets(context.Context) ([]finops.Budget, error) {
	blobs, err := s.cache.list("budgets")
	if err != nil {
		return nil, err
	}
	out := make([]finops.Budget, 0, len(blobs))
	for _, id := range sortedNames(blobs) {
		var b finops.Budget
		if err := json.Unmarshal(blobs[id], &b); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// PutBudgets replaces the UI budget set: each budget under its ID, budgets
// no longer in the set removed.
func (s *Store) PutBudgets(ctx context.Context, bs []finops.Budget) error {
	for _, b := range bs {
		if !idPattern.MatchString(b.ID) {
			return ErrInvalidID
		}
	}
	old, err := s.cache.list("budgets")
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, b := range bs {
		keep[b.ID] = true
		if err := s.put("budgets", b.ID, b); err != nil {
			return err
		}
	}
	for id := range old {
		if !keep[id] {
			if err := s.cache.remove(itemKey(s.cache.scope.Write, "budgets", id)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) GetReport(ctx context.Context, month string) (*finops.ReportView, error) {
	if !idPattern.MatchString(month) {
		return nil, ErrInvalidID
	}
	store, err := s.cache.tables()
	if err != nil {
		return nil, err
	}
	row, ok, err := store.FinOpsReport(ctx, s.cache.scope, month)
	if err != nil || !ok {
		return nil, err
	}
	var r finops.ReportView
	if err := json.Unmarshal(row.Model, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *Store) PutReport(ctx context.Context, r finops.ReportView) error {
	if r.Period == nil {
		return ErrInvalidID
	}
	month := r.Period.Calendar.Start.MonthKey()
	if !idPattern.MatchString(month) {
		return ErrInvalidID
	}
	store, err := s.cache.tables()
	if err != nil {
		return err
	}
	model, err := json.Marshal(r)
	if err != nil {
		return err
	}
	row := storage.FinOpsReport{Month: month, TotalMicros: int64(r.Summary.Total), Model: model}
	if r.Frozen != nil {
		row.FrozenAt, row.FrozenBy = r.Frozen.At, r.Frozen.By
	}
	return store.PutFinOpsReport(ctx, s.cache.scope, row)
}

func (s *Store) ListReports(ctx context.Context) ([]finops.ReportRef, error) {
	store, err := s.cache.tables()
	if err != nil {
		return nil, err
	}
	rows, err := store.FinOpsReports(ctx, s.cache.scope)
	if err != nil {
		return nil, err
	}
	out := make([]finops.ReportRef, 0, len(rows))
	for _, row := range rows {
		out = append(out, finops.ReportRef{Month: row.Month, Total: finops.Micros(row.TotalMicros), Frozen: finops.Frozen{At: row.FrozenAt, By: row.FrozenBy}})
	}
	return out, nil
}

func sortedNames(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// BudgetAlertStates returns the last budget alert per budget.
func (s *Store) BudgetAlertStates(context.Context) (map[string]finops.BudgetAlertState, error) {
	blobs, err := s.cache.list("budget-alerts")
	if err != nil {
		return nil, err
	}
	out := make(map[string]finops.BudgetAlertState, len(blobs))
	for id, data := range blobs {
		var st finops.BudgetAlertState
		if err := json.Unmarshal(data, &st); err != nil {
			return nil, err
		}
		out[id] = st
	}
	return out, nil
}

// PutBudgetAlertState records a sent budget alert.
func (s *Store) PutBudgetAlertState(_ context.Context, id string, st finops.BudgetAlertState) error {
	return s.put("budget-alerts", id, st)
}
