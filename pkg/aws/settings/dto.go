package settings

import "time"

// SettingsView is the GET /api/admin/aws/settings answer.
type SettingsView struct {
	Enabled      bool             `json:"enabled"`
	PrincipalARN string           `json:"principal_arn,omitempty"`
	Accounts     []AccountView    `json:"accounts"`
	Budget       BudgetView       `json:"budget"`
	OptionalData OptionalDataView `json:"optional_data"`
	Privacy      PrivacyView      `json:"privacy"`
	Capabilities CapabilitiesView `json:"capabilities"`
}

// AccountView is one row of the accounts table plus everything the row's
// sub-lines need.
type AccountView struct {
	AccountID         string        `json:"account_id"`
	DisplayName       string        `json:"display_name"`
	Source            Source        `json:"source"`
	Status            string        `json:"status"`
	CheckedAt         time.Time     `json:"checked_at,omitzero"`
	DisconnectedSince *time.Time    `json:"disconnected_since"`
	ErrorCode         *string       `json:"error_code"`
	Services          ServiceCounts `json:"services"`
	Regions           RegionsView   `json:"regions"`
	APIUse            APIUseView    `json:"api_use"`
	RoleARN           string        `json:"role_arn,omitempty"`
	Conflict          bool          `json:"conflict,omitempty"`
	// Sync is collection freshness and first-sync progress. It is always
	// present; State is "off" when collection is turned off.
	Sync AccountSyncView `json:"sync"`
}

// RegionsView reports the account's selection against what AWS said is
// available.
type RegionsView struct {
	Selected   []string `json:"selected"`
	Enabled    int      `json:"enabled"`
	NotEnabled []string `json:"not_enabled"`
}

// APIUseView is the shared limiter's meter for one account.
type APIUseView struct {
	UsedRPS   float64 `json:"used_rps"`
	BudgetRPS int     `json:"budget_rps"`
}

// BudgetView is the deployment's request budget: the configured rate, the
// grant rate over the trailing window, and whether the value came from config
// or from a saved wizard setting.
type BudgetView struct {
	UsedRPS   float64 `json:"used_rps"`
	BudgetRPS int     `json:"budget_rps"`
	Source    string  `json:"source"`
}

// OptionalDataView counts how many accounts have each optional source on.
type OptionalDataView struct {
	CostExplorer   OptionalCount `json:"cost_explorer"`
	Config         OptionalCount `json:"config"`
	SecurityHub    OptionalCount `json:"security_hub"`
	TrustedAdvisor OptionalCount `json:"trusted_advisor"`
}

// OptionalCount is the number of accounts with an optional source enabled and
// where that setting came from.
type OptionalCount struct {
	On     int    `json:"on"`
	Source string `json:"source"`
}

// PrivacyView describes exactly what leaves the deployment for the model.
type PrivacyView struct {
	Sent                  []string       `json:"sent"`
	NeverSent             []string       `json:"never_sent"`
	TagValueAllowlist     []string       `json:"tag_value_allowlist"`
	TagValueAllowlistFrom string         `json:"tag_value_allowlist_source"`
	AI                    AIView         `json:"ai"`
	SelfTest              SelfTestRecord `json:"self_test"`
}

// AIView is the configured model destination. An empty provider means no
// model is configured.
type AIView struct {
	Enabled  bool   `json:"enabled"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
}

// CapabilitiesView reports what this deployment can do. Organizations is
// false in OSS: the enterprise module installs the seam.
type CapabilitiesView struct {
	Organizations bool `json:"organizations"`
	CanWrite      bool `json:"can_write"`
}

// DraftView is the GET /api/admin/aws/connect/drafts/{id} answer. The full
// external ID is never part of it.
type DraftView struct {
	DraftID          string         `json:"draft_id"`
	Method           string         `json:"method"`
	Status           string         `json:"status"`
	RoleARN          string         `json:"role_arn,omitempty"`
	ExternalIDMasked string         `json:"external_id_masked"`
	PrincipalARN     string         `json:"principal_arn,omitempty"`
	AccountID        string         `json:"account_id,omitempty"`
	ProbeID          string         `json:"probe_id,omitempty"`
	Assume           AssumeResult   `json:"assume"`
	Services         []ServiceRow   `json:"services"`
	Counts           ServiceCounts  `json:"counts"`
	FixPolicy        string         `json:"fix_policy,omitempty"`
	Regions          []RegionStatus `json:"regions,omitempty"`
	ErrorCode        string         `json:"error_code,omitempty"`
	ExpiresAt        time.Time      `json:"expires_at,omitzero"`
}

// ServiceRow is one service in the verify grid.
type ServiceRow struct {
	Service    string   `json:"service"`
	Status     string   `json:"status"`
	Items      int      `json:"items"`
	ErrorCode  string   `json:"error_code,omitempty"`
	FixActions []string `json:"fix_actions,omitempty"`
	Retryable  bool     `json:"retryable"`
}

// DraftStartView is the POST /connect/drafts answer.
type DraftStartView struct {
	DraftID          string `json:"draft_id"`
	ExternalIDMasked string `json:"external_id_masked"`
	PrincipalARN     string `json:"principal_arn,omitempty"`
	TemplateURL      string `json:"template_url"`
	TerraformSnippet string `json:"terraform_snippet"`
}

// View converts a stored draft into its API shape.
func (draft Draft) View() DraftView {
	view := DraftView{
		DraftID:          draft.ID,
		Method:           draft.Method,
		Status:           draft.Status,
		RoleARN:          draft.RoleARN,
		ExternalIDMasked: MaskExternalID(draft.ExternalID),
		PrincipalARN:     draft.PrincipalARN,
		AccountID:        draft.AccountID,
		ProbeID:          draft.ProbeID,
		Assume:           draft.Assume,
		Counts:           draft.Counts,
		FixPolicy:        draft.FixPolicy,
		Regions:          draft.Regions,
		ErrorCode:        draft.ErrorCode,
		ExpiresAt:        draft.ExpiresAt,
	}
	view.Services = make([]ServiceRow, 0, len(draft.Services))
	for _, result := range draft.Services {
		view.Services = append(view.Services, ServiceRow{
			Service:    result.Service,
			Status:     string(result.Status),
			Items:      result.Items,
			ErrorCode:  result.ErrorCode,
			FixActions: result.FixActions,
			Retryable:  result.Retryable,
		})
	}
	return view
}
