package finops

import (
	"encoding/json"

	core_finops "github.com/VersusControl/versus-incident/pkg/finops"
)

// List bounds of the model views; fit trims further when a view is still
// over MaxModelViewBytes.
const (
	maxRows     = 8
	maxItems    = 10
	maxNotes    = 4
	maxMissing  = 5
	maxCauses   = 5
	maxSentence = 600
)

func headerView(h core_finops.Header) map[string]any {
	out := map[string]any{"measure": h.Measure, "currency": h.Currency, "links": map[string]any{"ui": h.Links.UI}}
	if h.Period != nil {
		out["period"] = map[string]any{"key": h.Period.Key, "label": h.Period.Label, "start": h.Period.Range.Start.String(),
			"end": h.Period.Range.End.String(), "open": h.Period.Open, "complete_days": h.Period.CompleteDays}
	}
	if h.Compare != nil {
		c := map[string]any{"label": h.Compare.Label, "available": h.Compare.Available}
		if !h.Compare.Available {
			c["reason"], c["first_full"] = h.Compare.Reason, h.Compare.FirstFull
		}
		out["compare"] = c
	}
	if s := h.Sync; s != nil {
		sync := map[string]any{
			"last_complete_day": s.LastCompleteDay.String(), "stale": s.Stale, "partial": s.Partial, "learning": s.Learning,
			"accounts_readable": s.AccountsReadable, "accounts_total": s.AccountsTotal, "previous_month_open": s.PreviousMonthOpen,
		}
		if s.StaleReason != "" {
			sync["stale_reason"] = s.StaleReason
		}
		if len(s.SkippedCalls) > 0 {
			sync["skipped_calls"] = s.SkippedCalls
		}
		if len(s.Estimated) > 0 {
			sync["estimated"] = s.Estimated
		}
		var missing []map[string]any
		for i, m := range s.MissingAccounts {
			if i == maxMissing {
				break
			}
			missing = append(missing, map[string]any{"id": m.ID, "name": m.Name, "reason": m.Reason})
		}
		if missing != nil {
			sync["missing_accounts"] = missing
		}
		if len(s.OtherCurrencies) > 0 {
			sync["other_currencies"] = s.OtherCurrencies
		}
		if n := s.Notes; len(n) > 0 {
			sync["notes"] = n[:min(len(n), maxNotes)]
		}
		out["sync"] = sync
	}
	return out
}

func owner(o *core_finops.Owner) map[string]any {
	if o == nil {
		return nil
	}
	m := map[string]any{"name": o.Name, "rule": o.Rule}
	if o.Estimated {
		m["estimated"] = true
	}
	return m
}

func breakdownView(sum core_finops.SummaryView, bd core_finops.BreakdownView) map[string]any {
	out := map[string]any{"total_micros": sum.Total, "answer": clip(sum.Answer), "forecast": sum.Forecast,
		"savings": sum.Savings, "unallocated": sum.Unallocated, "group_by": bd.GroupBy}
	if sum.Change != nil {
		out["change"] = sum.Change
	}
	if sum.Budget != nil {
		out["budget"] = map[string]any{"name": sum.Budget.Name, "monthly_micros": sum.Budget.Monthly, "status": sum.Budget.Status,
			"forecast_diff_micros": sum.Budget.ForecastDiff, "estimated": sum.Budget.Estimated}
	}
	var rows []map[string]any
	for i, r := range bd.Rows {
		if i == maxRows {
			out["rows_omitted"] = len(bd.Rows) - maxRows
			break
		}
		row := map[string]any{"key": r.Key, "label": r.Label, "amount_micros": r.Amount, "share_bp": r.ShareBP}
		if r.Change != nil {
			row["change_micros"] = *r.Change
		}
		if r.Owner != nil {
			row["owner"] = owner(r.Owner)
		}
		if r.Estimated {
			row["estimated"] = true
		}
		rows = append(rows, row)
	}
	out["rows"] = rows
	var opts []string
	for _, o := range bd.Options {
		if o.Available {
			opts = append(opts, o.GroupBy)
		}
	}
	out["group_by_options"] = opts
	return out
}

func driversView(v core_finops.BridgeView) map[string]any {
	out := map[string]any{"available": v.Available, "base_micros": v.Base, "total_micros": v.Total, "change_micros": v.Change,
		"change_bp": v.ChangeBP, "direction": v.Direction, "steady": v.Steady, "up_micros": v.Up, "down_micros": v.Down,
		"min_driver_micros": v.MinDriver}
	if v.Note != "" {
		out["note"] = v.Note
	}
	var rows []map[string]any
	for i, r := range v.Rows {
		if i == maxRows {
			out["drivers_omitted"] = len(v.Rows) - maxRows
			break
		}
		row := map[string]any{"label": r.Label, "cause": r.Cause, "amount_micros": r.Amount, "service": r.ServiceLabel,
			"account": r.AccountName}
		if r.Owner != nil {
			row["owner"] = owner(r.Owner)
		}
		if r.Why != nil {
			row["why"] = r.Why.Text
		}
		if r.IncreaseID != "" {
			row["increase_id"], row["status"] = r.IncreaseID, r.Status
		}
		rows = append(rows, row)
	}
	out["drivers"] = rows
	if v.Other != nil {
		out["other_micros"] = v.Other.Amount
	}
	return out
}

func increaseItem(i core_finops.IncreaseView) map[string]any {
	o := i.Owner
	m := map[string]any{"id": i.ID, "title": i.Title, "account": i.AccountName, "service": i.ServiceLabel,
		"started_on": i.StartedOn.String(), "daily_before_micros": i.DailyBefore, "daily_now_micros": i.DailyNow,
		"monthly_impact_micros": i.Monthly, "period_change_micros": i.PeriodChange, "cause": i.Cause,
		"owner": owner(&o), "status": i.Status, "suspect": i.Suspect.State}
	if i.Suspect.State == "found" {
		m["suspect_summary"], m["suspect_actor"], m["suspect_confidence"] = i.Suspect.Summary, i.Suspect.Actor, i.Suspect.Confidence
	}
	if i.Expected != nil {
		m["expected_reason"], m["expected_until"] = i.Expected.Reason, i.Expected.Until.String()
	}
	return m
}

func increasesView(v core_finops.IncreasesView) map[string]any {
	out := map[string]any{"status": v.Status, "learning": v.Learning}
	if v.LearningNote != "" {
		out["learning_note"] = v.LearningNote
	}
	var items []map[string]any
	for i, it := range v.Items {
		if i == maxItems {
			out["increases_omitted"] = len(v.Items) - maxItems
			break
		}
		items = append(items, increaseItem(it))
	}
	out["increases"] = items
	return out
}

func increaseDetailView(v core_finops.IncreaseDetailView) map[string]any {
	out := increaseItem(v.Increase)
	out["sentence"] = clip(v.Increase.Sentence)
	out["resource_data"] = v.ResourceData
	if v.ResourceNote != "" {
		out["resource_note"] = v.ResourceNote
	}
	var causes []map[string]any
	for i, c := range v.CauseTree {
		if i == maxCauses {
			break
		}
		node := map[string]any{"label": c.Label, "amount_micros": c.Amount}
		if len(c.Resources) > 0 {
			node["top_resource"] = c.Resources[0]
		}
		causes = append(causes, node)
	}
	out["cause_tree"] = causes
	if v.UsageSignal != nil {
		out["usage_signal"] = v.UsageSignal
	}
	var suspects []map[string]any
	for _, s := range v.Increase.Suspects {
		item := map[string]any{"summary": clip(s.Summary), "actor": s.Actor, "at": s.At, "confidence": s.Confidence, "ref": s.Ref}
		if s.Tied {
			item["tied"], item["note"] = true, s.Note
		}
		suspects = append(suspects, item)
	}
	if suspects != nil {
		out["suspects"] = suspects
	}
	if v.Increase.Suspect.State != "found" && v.Increase.Suspect.Note != "" {
		out["suspect_note"] = v.Increase.Suspect.Note
	}
	var related []map[string]any
	for i, s := range v.RelatedSavings {
		if i == 3 {
			break
		}
		related = append(related, map[string]any{"title": s.Title, "monthly_micros": s.Monthly, "effort": s.Effort, "risk": s.Risk})
	}
	out["related_savings"] = related
	return out
}

func savingsView(v core_finops.SavingsView) map[string]any {
	out := map[string]any{"total": v.Total}
	var items []map[string]any
	for i, s := range v.Items {
		if i == maxItems {
			out["savings_omitted"] = len(v.Items) - maxItems
			break
		}
		o := s.Owner
		item := map[string]any{"id": s.ID, "kind": s.Kind, "title": s.Title, "monthly_micros": s.Monthly,
			"effort": s.Effort, "risk": s.Risk, "account": s.Account, "region": s.Region, "owner": owner(&o), "status": s.Status,
			"basis": s.Basis}
		if s.UpTo {
			item["up_to"] = true
		}
		if s.BeforeOtherSavings {
			item["before_other_savings"] = true
		}
		for i, a := range s.Alternatives {
			if i == 2 {
				break
			}
			item["alternatives"] = append(asList(item["alternatives"]), map[string]any{"title": a.Title, "monthly_micros": a.Monthly})
		}
		items = append(items, item)
	}
	out["savings"] = items
	if len(v.Gaps) > 0 {
		out["checks_not_run"] = v.Gaps
	}
	return out
}

func budgetView(v core_finops.BudgetView) map[string]any {
	out := map[string]any{"forecast": v.Forecast}
	var items []map[string]any
	for i, b := range v.Budgets {
		if i == maxItems {
			break
		}
		items = append(items, map[string]any{"name": b.Name, "scope": b.Scope, "scope_value": b.Value, "monthly_micros": b.Monthly,
			"actual_micros": b.Actual, "forecast_micros": b.Forecast, "forecast_source": b.ForecastSource, "status": b.Status,
			"source": b.Source, "estimated": b.Estimated})
	}
	out["budgets"] = items
	if len(v.Skipped) > 0 {
		out["skipped_budgets"] = v.Skipped
	}
	return out
}

func accountsView(v core_finops.AccountsView) map[string]any {
	out := map[string]any{"total_micros": v.Total}
	var rows []map[string]any
	for i, a := range v.Accounts {
		if i == maxItems {
			out["accounts_omitted"] = len(v.Accounts) - maxItems
			break
		}
		row := map[string]any{"id": a.ID, "name": a.Name, "kind": a.Kind, "status": a.Status, "cost_micros": a.Cost}
		if a.ErrorCode != "" {
			row["error_code"] = a.ErrorCode
		}
		if a.Group != "" {
			row["group"] = a.Group
		}
		if a.Change != nil {
			row["change_micros"] = *a.Change
		}
		if a.TopDriver != nil {
			row["top_driver"] = a.TopDriver.Label
		}
		if a.TopTeam != "" {
			row["top_team"], row["top_team_estimated"] = a.TopTeam, a.TopTeamEstimated
		}
		rows = append(rows, row)
	}
	out["accounts"] = rows
	return out
}

func clip(s string) string {
	if len(s) > maxSentence {
		return s[:maxSentence]
	}
	return s
}

// listKeys are the lists fit may shorten, in trimming order.
var listKeys = []string{"rows", "drivers", "increases", "savings", "accounts", "budgets", "cause_tree", "related_savings"}

// fit keeps a view under MaxModelViewBytes by halving its lists, and says
// so; a gap is named, never silently dropped.
func fit(view map[string]any) map[string]any {
	for range 8 {
		b, err := json.Marshal(view)
		if err == nil && len(b) <= MaxModelViewBytes {
			return view
		}
		trimmed := false
		for _, k := range listKeys {
			if items, ok := view[k].([]map[string]any); ok && len(items) > 1 {
				view[k] = items[:len(items)/2]
				trimmed = true
			}
		}
		view["truncated"] = true
		if !trimmed {
			if s, ok := view["sync"].(map[string]any); ok {
				delete(s, "notes")
			}
			delete(view, "answer")
			delete(view, "sentence")
		}
	}
	return view
}

func asList(v any) []map[string]any {
	if l, ok := v.([]map[string]any); ok {
		return l
	}
	return nil
}
