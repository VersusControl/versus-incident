package cost

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/finops"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// Cost lines, increases with their status history, frozen reports and
// saving decisions live in the FinOps tables (storage.FinOpsStore; see
// lines.go and store.go). Small state stays in blobs, under the
// deployment's organization:
//
//	aws/<org>/finops/meter/<yyyy-mm-dd>       paid requests and dollars of a day
//	aws/<org>/finops/status                   refresh state per payer
//	aws/<org>/finops/budgets/<id>             budget defined in the UI
//	aws/<org>/finops/budget-alerts/<id>       last budget alert sent this month
const (
	linesRetentionMonths = 13
	meterRetentionDays   = 90
	// savingDecisionRetention is how long a decision whose saving is no
	// longer produced is kept for audit before it is deleted.
	savingDecisionRetention = 90 * 24 * time.Hour
	// requestPriceMicros is what one paid Cost Explorer request costs.
	requestPriceMicros = 10_000
)

func finopsPrefix(org string) string { return "aws/" + tenancy.NormalizeOrgID(org) + "/finops/" }

func meterKey(org, day string) string     { return finopsPrefix(org) + "meter/" + day }
func statusKey(org string) string         { return finopsPrefix(org) + "status" }
func itemKey(org, kind, id string) string { return finopsPrefix(org) + kind + "/" + id }

// cache reads across the scope's read organizations, write organization
// first, and writes only to the write organization.
type cache struct {
	provider storage.Provider
	scope    tenancy.OrgScope
}

func (c cache) read(key func(org string) string, v any) (bool, error) {
	for _, org := range c.scope.Read {
		data, err := c.provider.ReadBlob(key(org))
		if err != nil {
			return false, err
		}
		if len(data) == 0 {
			continue
		}
		if err := json.Unmarshal(data, v); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func (c cache) write(key func(org string) string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.provider.WriteBlob(key(c.scope.Write), data)
}

// remove deletes a key in the write organization through the storage
// delete (an exact-key delete or the blob lifecycle domain). Only a provider
// that cannot delete gets an empty, non-nil blob, which every reader treats
// as absent; a nil payload would break a NOT NULL blob column.
func (c cache) remove(key string) error {
	err := storage.DeleteBlob(c.provider, key)
	if errors.Is(err, storage.ErrUnsupported) {
		return c.provider.WriteBlob(key, []byte{})
	}
	return err
}

// list returns blobs under a sub-prefix, the write organization winning
// over read organizations for the same name.
func (c cache) list(kind string) (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, org := range c.scope.Read {
		prefix := finopsPrefix(org) + kind + "/"
		blobs, err := c.provider.ListBlobs(prefix)
		if err != nil {
			return nil, err
		}
		for _, b := range blobs {
			id := strings.TrimPrefix(b.Name, prefix)
			if _, seen := out[id]; seen || len(b.Data) == 0 || id == "" {
				continue
			}
			out[id] = b.Data
		}
	}
	return out, nil
}

// pruneLines deletes cost lines older than the retention: the first day
// kept is the start of the month linesRetentionMonths before this one.
func (c cache) pruneLines(ctx context.Context, today finops.Date) error {
	store, err := c.tables()
	if err != nil {
		return err
	}
	first := today.MonthStart()
	for i := 0; i < linesRetentionMonths; i++ {
		first = first.AddDays(-1).MonthStart()
	}
	_, err = store.PruneFinOpsCostLines(ctx, c.scope, first.String())
	return err
}

// pruneDecisions deletes saving decisions whose saving has not been produced
// for the whole decision retention. A store without decision upkeep keeps
// none to prune.
func (c cache) pruneDecisions(ctx context.Context, now time.Time) error {
	store, err := c.tables()
	if err != nil {
		return err
	}
	upkeep, ok := store.(storage.FinOpsSavingDecisionStore)
	if !ok {
		return nil
	}
	_, err = upkeep.PruneFinOpsSavingDecisions(ctx, c.scope, now.Add(-savingDecisionRetention))
	return err
}

// deletePayerDecisions deletes the saving decisions of a removed payer:
// those stored with its payer, and those stored without a payer on one of
// accounts, the accounts that belong to it alone. When it is the last payer,
// a decision stored with neither payer nor account (a saving of the whole
// deployment, made while several payers were connected) is its too: nothing
// is left to produce that saving again. Decisions of other payers are never
// touched.
func (c cache) deletePayerDecisions(ctx context.Context, store storage.FinOpsStore, payer string, accounts []string, last bool) error {
	upkeep, ok := store.(storage.FinOpsSavingDecisionStore)
	if !ok {
		return nil
	}
	unfilled, fills := store.(storage.FinOpsSavingDecisionSubjects)
	if last && fills {
		// Claim them for this payer first; the fill only ever changes a
		// decision without a payer.
		decisions, err := store.FinOpsSavingDecisions(ctx, c.scope)
		if err != nil {
			return err
		}
		var claims []storage.FinOpsSavingDecision
		for _, d := range decisions {
			if d.Payer == "" && d.Account == "" {
				claims = append(claims, storage.FinOpsSavingDecision{SavingID: d.SavingID, Payer: payer})
			}
		}
		if len(claims) > 0 {
			if _, err := unfilled.FillFinOpsSavingDecisionSubjects(ctx, c.scope, claims); err != nil {
				return err
			}
		}
	}
	if _, err := upkeep.DeleteFinOpsSavingDecisionsForPayer(ctx, c.scope, payer); err != nil {
		return err
	}
	if !fills {
		return nil
	}
	_, err := unfilled.DeleteFinOpsSavingDecisionsWithoutPayer(ctx, c.scope, accounts)
	return err
}

// accountState is what the status keeps per linked account.
type accountState struct {
	Name      string      `json:"name,omitempty"`
	FirstCost finops.Date `json:"first_cost,omitzero"`
}

// payerState is one payer's refresh state.
type payerState struct {
	Organization    bool        `json:"organization,omitempty"`
	Currency        string      `json:"currency,omitempty"`
	LastAttempt     time.Time   `json:"last_attempt,omitzero"`
	LastSuccess     time.Time   `json:"last_success,omitzero"`
	ErrorCode       string      `json:"error_code,omitempty"`
	Stale           bool        `json:"stale,omitempty"`
	LastCompleteDay finops.Date `json:"last_complete_day,omitzero"`
	HistoryStart    finops.Date `json:"history_start,omitzero"`
	ClosedThrough   string      `json:"closed_through,omitempty"`
	// ClosedMonths are the months whose bill AWS closed: their days are
	// no longer replaced except by the month close itself.
	ClosedMonths    []string                 `json:"closed_months,omitempty"`
	Accounts        map[string]*accountState `json:"accounts,omitempty"`
	Forecast        *finops.ProviderForecast `json:"forecast,omitempty"`
	ForecastOn      finops.Date              `json:"forecast_on,omitzero"`
	Coverage        *coverage                `json:"coverage,omitempty"`
	TagActive       *bool                    `json:"tag_active,omitempty"`
	Budgets         []finops.Budget          `json:"budgets,omitempty"`
	SkippedBudgets  []finops.SkippedBudget   `json:"skipped_budgets,omitempty"`
	Recommendations []finops.Recommendation  `json:"recommendations,omitempty"`
	// IdleFound are idle resources the optimization hub found.
	IdleFound []finops.IdleResource `json:"idle_found,omitempty"`
	// CommitmentMonthly and CommitmentSuggestion are the hub's Savings
	// Plans purchase saving.
	CommitmentMonthly    int64  `json:"commitment_monthly_micros,omitempty"`
	CommitmentSuggestion string `json:"commitment_suggestion,omitempty"`
	// RecommendationSource is the provider that answered: hub or compute
	// optimizer; empty when neither did.
	RecommendationSource string `json:"recommendation_source,omitempty"`
	ResourceData         bool   `json:"resource_data,omitempty"`
	// ResourceFailedOn is the day resource-level cost last failed (off in
	// the payer); it is not asked again for a week.
	ResourceFailedOn finops.Date `json:"resource_failed_on,omitzero"`
	// Drilled records the day each account and service was last drilled.
	Drilled map[string]finops.Date `json:"drilled,omitempty"`
	// PendingDrill holds drill-down detail for days whose tables do not
	// carry the pair yet (day → pair → cells): it is put on the day when a
	// later refresh or backfill writes it, and dropped after the drill
	// window.
	PendingDrill map[string]map[string][]cell `json:"pending_drill,omitempty"`
	// FreeErrors names failures of the free calls (tags, budgets, hub).
	FreeErrors map[string]string `json:"free_errors,omitempty"`
	// Pages are the paid pages each call last used, for its window, so the
	// planner prices the next run from what AWS actually returned.
	Pages map[string]pageStat `json:"pages,omitempty"`
	// ScheduledRefreshedOn and ManualRefreshedOn are the deployment days the
	// payer's scheduled or manual refresh last completed. They are kept
	// apart so a manual run does not satisfy the scheduled plan, and a
	// partial retry resumes only the payers missing from that same kind.
	ScheduledRefreshedOn finops.Date `json:"scheduled_refreshed_on,omitzero"`
	ManualRefreshedOn    finops.Date `json:"manual_refreshed_on,omitzero"`
}

// pageStat is one call's last page use: pages for a window of days. Done
// is false when the call stopped before its last page (the cap), so the
// count is a lower bound.
type pageStat struct {
	Pages int  `json:"pages"`
	Days  int  `json:"days"`
	Done  bool `json:"done"`
}

func (p *payerState) hasData() bool { return !p.HistoryStart.IsZero() }

// statusRecord is the adapter's whole refresh state.
type statusRecord struct {
	UpdatedAt    time.Time              `json:"updated_at,omitzero"`
	Payers       map[string]*payerState `json:"payers,omitempty"`
	SkippedCalls []string               `json:"skipped_calls,omitempty"`
	ManualOn     finops.Date            `json:"manual_on,omitzero"`
	// ScheduledOn is the deployment day the scheduled refresh last ran.
	ScheduledOn finops.Date `json:"scheduled_on,omitzero"`
	// Removed holds, per payer marked removed (MarkPayerRemoved), the
	// account settings revision it was removed at. Tombstones are only
	// added, never dropped.
	Removed map[string]uint64 `json:"removed_payers,omitempty"`
	// PurgePending names payers marked removed whose cost data is not
	// deleted yet. Only the purge that deletes a payer's data clears it.
	PurgePending map[string]bool `json:"purge_pending,omitempty"`

	// purged names the payers purged since this record was read, so its
	// write clears them from PurgePending. It is not stored.
	purged map[string]bool
}

func (c cache) status() (*statusRecord, error) {
	st := &statusRecord{}
	if _, err := c.read(statusKey, st); err != nil {
		return nil, err
	}
	if st.Payers == nil {
		st.Payers = map[string]*payerState{}
	}
	return st, nil
}

func (c cache) putStatus(st *statusRecord) error { return c.write(statusKey, st) }
