package aws

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/catalog"
	"github.com/VersusControl/versus-incident/pkg/aws/settings"
)

// Model-view limits. The view keeps every account it can within
// MaxModelViewBytes and says how many it left out.
const (
	settingsUI        = "/aws/settings"
	connectionRef     = "aws://connection"
	maxDeniedServices = 12
	maxFixActions     = 6
	maxRegions        = 16
	maxOmittedIDs     = 200
)

type deniedService struct {
	Service    string   `json:"service"`
	FixActions []string `json:"fix_actions"`
}

type serviceGap struct {
	Service string   `json:"service"`
	Gaps    []string `json:"gaps"`
}

type accountSync struct {
	State     string       `json:"state"`
	SyncedAt  *time.Time   `json:"synced_at"`
	AgeS      *int64       `json:"age_s"`
	FirstSync bool         `json:"first_sync"`
	Progress  string       `json:"progress"`
	Gaps      []serviceGap `json:"gaps"`
}

type accountView struct {
	AccountID          string          `json:"account_id"`
	Name               string          `json:"name"`
	Status             string          `json:"status"`
	ErrorCode          string          `json:"error_code,omitempty"`
	CheckedAt          *time.Time      `json:"checked_at"`
	Sync               accountSync     `json:"sync"`
	Readable           int             `json:"services_readable"`
	Denied             []deniedService `json:"denied"`
	NotOnPlan          []string        `json:"not_on_plan"`
	NotChecked         []string        `json:"not_checked"`
	RegionsSelected    int             `json:"regions_selected"`
	RegionsNotEnabled  []string        `json:"regions_not_enabled"`
	APIUseRPS          float64         `json:"api_use_rps"`
	APIBudgetRPS       int             `json:"api_budget_rps"`
	Ref                string          `json:"ref"`
	Links              map[string]any  `json:"links"`
	ConfiguredInConfig bool            `json:"from_config"`
}

// ConnectionView compacts the connection view for the model: no external
// ID, role ARN, Versus principal, privacy self-test or draft data. account
// filters to one account when set; offset pages through the accounts.
// Accounts that need attention come first, so when the view is cut to fit
// it is healthy accounts that are left out, and every left-out account is
// named or reachable through next.
func ConnectionView(view settings.SettingsView, account string, offset int, now time.Time) map[string]any {
	out := map[string]any{
		"enabled": view.Enabled,
		"ref":     connectionRef,
		"links":   map[string]any{"ui": settingsUI},
	}
	if !view.Enabled {
		out["accounts"] = []accountView{}
		out["accounts_total"] = 0
		out["sync"] = map[string]any{"state": settings.SyncStateOff, "age_s": nil, "partial": false, "gaps": []string{}}
		out["hint"] = "The AWS connector is off. An administrator can connect accounts on " + settingsUI + "."
		return out
	}
	accounts := make([]accountView, 0, len(view.Accounts))
	for _, item := range view.Accounts {
		if account != "" && item.AccountID != account {
			continue
		}
		accounts = append(accounts, compactAccount(item, now))
	}
	sort.SliceStable(accounts, func(left, right int) bool {
		return attentionRank(accounts[left]) < attentionRank(accounts[right])
	})
	out["accounts_total"] = len(accounts)
	out["sync"] = overallSync(accounts)
	out["budget"] = map[string]any{"used_rps": view.Budget.UsedRPS, "budget_rps": view.Budget.BudgetRPS}
	if account != "" && len(accounts) == 0 {
		out["accounts"] = []accountView{}
		out["hint"] = "Account " + account + " is not connected; see " + settingsUI + "."
		return out
	}
	if offset < 0 || offset > len(accounts) {
		offset = 0
	}
	if offset > 0 {
		out["offset"] = offset
	}
	return fit(out, accounts, offset)
}

// attentionRank orders accounts for the model: failing or disconnected
// first, then accounts with denied, missing or partial data, then healthy.
func attentionRank(account accountView) int {
	switch {
	case account.Status == settings.StatusDisconnected || account.ErrorCode != "" || account.Sync.State == settings.SyncStateFailed:
		return 0
	case len(account.Denied) > 0 || len(account.NotChecked) > 0 || len(account.RegionsNotEnabled) > 0 ||
		account.Sync.State == settings.SyncStatePartial || len(account.Sync.Gaps) > 0 || account.Status != settings.StatusConnected:
		return 1
	default:
		return 2
	}
}

func compactAccount(item settings.AccountView, now time.Time) accountView {
	out := accountView{
		AccountID:          item.AccountID,
		Name:               item.DisplayName,
		Status:             item.Status,
		Readable:           item.Services.Readable,
		Denied:             []deniedService{},
		NotOnPlan:          append([]string{}, item.Services.NotOnPlanServices...),
		NotChecked:         append([]string{}, item.Services.NotCheckedServices...),
		RegionsSelected:    len(item.Regions.Selected),
		RegionsNotEnabled:  append([]string{}, item.Regions.NotEnabled...),
		APIUseRPS:          item.APIUse.UsedRPS,
		APIBudgetRPS:       item.APIUse.BudgetRPS,
		Ref:                "aws://" + item.AccountID,
		Links:              map[string]any{"ui": settingsUI},
		ConfiguredInConfig: item.Source == settings.SourceConfig,
	}
	if item.ErrorCode != nil {
		out.ErrorCode = *item.ErrorCode
	}
	if !item.CheckedAt.IsZero() {
		checked := item.CheckedAt
		out.CheckedAt = &checked
	}
	for index, service := range item.Services.DeniedServices {
		if index >= maxDeniedServices {
			break
		}
		denied := deniedService{Service: service, FixActions: []string{}}
		if entry, ok := catalog.ByID(service); ok {
			denied.FixActions = append(denied.FixActions, entry.FixActions...)
			if len(denied.FixActions) > maxFixActions {
				denied.FixActions = denied.FixActions[:maxFixActions]
			}
		}
		out.Denied = append(out.Denied, denied)
	}
	if len(out.RegionsNotEnabled) > maxRegions {
		out.RegionsNotEnabled = out.RegionsNotEnabled[:maxRegions]
	}
	sync := item.Sync
	out.Sync = accountSync{State: sync.State, SyncedAt: sync.SyncedAt, FirstSync: sync.FirstSync, Gaps: []serviceGap{}}
	if sync.SyncedAt != nil {
		age := int64(now.Sub(*sync.SyncedAt) / time.Second)
		if age < 0 {
			age = 0
		}
		out.Sync.AgeS = &age
	}
	if sync.Units > 0 {
		out.Sync.Progress = itoa(sync.UnitsDone) + "/" + itoa(sync.Units)
	}
	for _, service := range sync.Services {
		if len(service.Gaps) > 0 {
			out.Sync.Gaps = append(out.Sync.Gaps, serviceGap{Service: service.Service, Gaps: append([]string(nil), service.Gaps...)})
		}
	}
	return out
}

// overallSync is the worst state across accounts, the oldest freshness and
// the union of named gap reasons. A gap is named, never read as zero.
func overallSync(accounts []accountView) map[string]any {
	rank := map[string]int{settings.SyncStateOK: 0, settings.SyncStateSyncing: 1, settings.SyncStatePending: 2, settings.SyncStatePartial: 3, settings.SyncStateFailed: 4, settings.SyncStateOff: 5}
	state := settings.SyncStateOK
	if len(accounts) == 0 {
		state = settings.SyncStatePending
	}
	var oldest *int64
	reasons := map[string]bool{}
	for _, account := range accounts {
		if rank[account.Sync.State] > rank[state] {
			state = account.Sync.State
		}
		if account.Sync.AgeS != nil && (oldest == nil || *account.Sync.AgeS > *oldest) {
			oldest = account.Sync.AgeS
		}
		for _, gap := range account.Sync.Gaps {
			for _, reason := range gap.Gaps {
				reasons[reason] = true
			}
		}
	}
	gaps := make([]string, 0, len(reasons))
	for reason := range reasons {
		gaps = append(gaps, reason)
	}
	sort.Strings(gaps)
	return map[string]any{"state": state, "age_s": oldest, "partial": state != settings.SyncStateOK, "gaps": gaps}
}

// fit keeps as many accounts from offset as fit in MaxModelViewBytes and
// names the rest: their IDs (bounded) and a next offset that reaches them.
// Listing the omitted IDs wins over showing one more account; only when
// even the IDs cannot fit does the view rely on next alone.
func fit(out map[string]any, accounts []accountView, offset int) map[string]any {
	remaining := accounts[offset:]
	for _, listIDs := range []bool{true, false} {
		for keep := len(remaining); keep >= 0; keep-- {
			shape(out, remaining, offset, keep, listIDs)
			if encoded, err := json.Marshal(out); err == nil && len(encoded) <= MaxModelViewBytes {
				return out
			}
		}
	}
	shape(out, remaining, offset, 0, false)
	return out
}

func shape(out map[string]any, remaining []accountView, offset, keep int, listIDs bool) {
	out["accounts"] = remaining[:keep]
	omitted := remaining[keep:]
	if len(omitted) == 0 {
		delete(out, "truncated")
		delete(out, "omitted_accounts")
		delete(out, "omitted_account_ids")
		delete(out, "next")
		return
	}
	ids := make([]string, 0, len(omitted))
	if listIDs {
		for index, item := range omitted {
			if index >= maxOmittedIDs {
				break
			}
			ids = append(ids, item.AccountID)
		}
	}
	out["truncated"] = true
	out["omitted_accounts"] = len(omitted)
	out["omitted_account_ids"] = ids
	out["next"] = map[string]any{"offset": offset + keep, "hint": "call again with this offset, or with account_id for one account"}
}

func itoa(value int) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
