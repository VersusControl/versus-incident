package settings

import (
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/probe"
)

// Source labels where an account came from. A config account is read-only in
// the UI; a wizard account is stored and editable.
type Source string

const (
	// SourceConfig is an account defined in connectors.aws.accounts.
	SourceConfig Source = "config"
	// SourceWizard is an account committed through the connect wizard.
	SourceWizard Source = "wizard"
)

// Health status values. They are a closed vocabulary rendered as a word plus
// an icon, never as colour alone.
const (
	// StatusConnected means the last identity check succeeded.
	StatusConnected = "connected"
	// StatusDisconnected means two consecutive identity checks failed.
	StatusDisconnected = "disconnected"
	// StatusChecking means a check is in flight or has not finished yet.
	StatusChecking = "checking"
	// StatusUnknown means the account has never been checked.
	StatusUnknown = "unknown"
)

// Draft lifecycle values.
const (
	// DraftStarted means the draft exists and no verify has run yet.
	DraftStarted = "started"
	// DraftVerifying means assume and probes are in flight.
	DraftVerifying = "verifying"
	// DraftVerified means assume and probes finished.
	DraftVerified = "verified"
)

// AccountRecord is one wizard account. The external ID is persisted because
// Versus must present it on every assume, but it is never rendered outside the
// audited reveal path.
type AccountRecord struct {
	AccountID   string    `json:"account_id"`
	DisplayName string    `json:"display_name"`
	RoleARN     string    `json:"role_arn"`
	ExternalID  string    `json:"external_id"`
	Regions     []string  `json:"regions"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
	Source      Source    `json:"source"`
}

// AssumeResult is the outcome of one assume-role attempt, in the closed error
// vocabulary. Provider text is never carried.
type AssumeResult struct {
	OK        bool   `json:"ok"`
	LatencyMS int    `json:"latency_ms"`
	ErrorCode string `json:"error_code,omitempty"`
}

// RegionStatus is one region the wizard offers. Enabled regions may be
// selected; a disabled or opt-in region is shown but cannot be connected.
type RegionStatus struct {
	Region         string `json:"region"`
	Enabled        bool   `json:"enabled"`
	OptInStatus    string `json:"opt_in_status,omitempty"`
	ResourcesFound bool   `json:"resources_found"`
}

// Draft is in-progress wizard state. It expires an hour after creation.
type Draft struct {
	ID           string         `json:"draft_id"`
	Method       string         `json:"method"`
	RoleARN      string         `json:"role_arn,omitempty"`
	ExternalID   string         `json:"external_id"`
	PrincipalARN string         `json:"principal_arn,omitempty"`
	AccountID    string         `json:"account_id,omitempty"`
	Status       string         `json:"status"`
	ProbeID      string         `json:"probe_id,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
	ExpiresAt    time.Time      `json:"expires_at"`
	Assume       AssumeResult   `json:"assume"`
	Services     []probe.Result `json:"services,omitempty"`
	FixPolicy    string         `json:"fix_policy,omitempty"`
	Regions      []RegionStatus `json:"regions,omitempty"`
	Counts       ServiceCounts  `json:"counts"`
	ErrorCode    string         `json:"error_code,omitempty"`
	VerifiedAt   time.Time      `json:"verified_at,omitzero"`
}

// HealthRecord is the persisted connection health for one account. The attempt
// and the success are separate timestamps so a long-running check and a stale
// success cannot be confused.
type HealthRecord struct {
	AccountID           string         `json:"account_id"`
	Status              string         `json:"status"`
	AttemptedAt         time.Time      `json:"attempted_at,omitzero"`
	AttemptOK           bool           `json:"attempt_ok"`
	CheckedAt           time.Time      `json:"checked_at,omitzero"`
	DisconnectedSince   time.Time      `json:"disconnected_since,omitzero"`
	FailureSince        time.Time      `json:"failure_since,omitzero"`
	ErrorCode           string         `json:"error_code,omitempty"`
	ConsecutiveFailures int            `json:"consecutive_failures"`
	ProbedAt            time.Time      `json:"probed_at,omitzero"`
	ProbeResults        []probe.Result `json:"probe_results,omitempty"`
	Regions             []RegionStatus `json:"regions,omitempty"`
}

// ServiceCounts summarises the probe grid without forcing a client to walk
// every row. Readable, not used and not checked are numbers; the service names
// behind denied and not-on-plan are lists because each name needs its own fix
// box. Both the short and the "…_services" spellings are emitted for the two
// lists so either client naming reads the same answer.
type ServiceCounts struct {
	Total              int      `json:"total"`
	Readable           int      `json:"readable"`
	NotUsed            int      `json:"not_used"`
	NotChecked         int      `json:"not_checked"`
	Denied             []string `json:"denied"`
	DeniedServices     []string `json:"denied_services"`
	NotOnPlan          []string `json:"not_on_plan"`
	NotOnPlanServices  []string `json:"not_on_plan_services"`
	NotCheckedServices []string `json:"not_checked_services"`
}

// Options holds the runtime overrides a wizard account set can carry. Config
// values are merged over these for display and never written.
type Options struct {
	CostExplorer         *bool `json:"cost_explorer,omitempty"`
	Config               *bool `json:"config,omitempty"`
	SecurityHub          *bool `json:"security_hub,omitempty"`
	TrustedAdvisor       *bool `json:"trusted_advisor,omitempty"`
	MaxRequestsPerSecond *int  `json:"max_requests_per_second,omitempty"`
}

// SelfTestRecord is the stored privacy self-test answer. It is exactly the
// shape the settings API renders.
type SelfTestRecord struct {
	LastRunAt time.Time `json:"last_run_at,omitzero"`
	Passing   bool      `json:"passing"`
	Leaks24h  int       `json:"leaks_24h"`
}

// leakSample is one self-test run counted inside the 24-hour window.
type leakSample struct {
	At    time.Time `json:"at"`
	Leaks int       `json:"leaks"`
}

// privacyRecord is the persisted allowlist plus the samples behind
// SelfTestRecord.Leaks24h. The self-test record itself keeps the exact public
// shape.
type privacyRecord struct {
	TagValueAllowlist []string     `json:"tag_value_allowlist"`
	Samples           []leakSample `json:"samples,omitempty"`
}
