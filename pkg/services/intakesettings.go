package services

import (
	"encoding/json"
	"errors"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

// intakesettings.go — the OSS runtime settings store for incident INTAKE
// behaviour, persisted via the same storage.Provider blob seam the report
// settings use. These are non-secret operational toggles set at runtime in the
// admin UI, NOT the encrypted per-org channel store.
//
// The store is read-through on every request (LoadIntakeSettings returns a
// fresh copy each call), so there is no process-wide mutable settings global to
// clone. Precedence is simply "stored value → built-in default".

// IntakeSettingsBlobName is the blob the intake settings live under, a sibling
// of the report settings in the models/settings/ namespace.
const IntakeSettingsBlobName = "models/settings/intake-settings"

// ErrIntakeNoStorage is returned by SaveIntakeSettings when no storage backend
// is configured, so the admin API can map it to 503.
var ErrIntakeNoStorage = errors.New("intake: storage not configured")

// IntakeSettings is the non-secret runtime configuration for incident intake.
// It is the JSON shape persisted in the settings blob and exchanged by the
// admin GET/PUT endpoints.
type IntakeSettings struct {
	// AutoResolveWebhook makes an incident that arrives via the PUBLIC
	// webhook intake run the full normal flow — alert fan-out, ack URL, and
	// on-call escalation, exactly as an ordinary incident — and then stamps
	// the stored record resolved so it does not sit in the open list. The only
	// difference from a normal webhook incident is the persisted resolved /
	// resolved_at; alerting, ack URL, and on-call are unchanged. DEFAULT true:
	// an install with no stored blob auto-resolves webhook incidents. It is
	// scoped strictly to the webhook origin, so SNS/SQS-transported and
	// agent-emitted incidents are never affected.
	AutoResolveWebhook bool `json:"auto_resolve_webhook"`
}

// DefaultIntakeSettings is the built-in floor applied when the store holds no
// value: webhook auto-resolve is ON. This is a deliberate default-on that
// changes public-webhook behaviour out of the box — an operator disables it in
// the UI when they want webhook incidents to stay open and escalate.
func DefaultIntakeSettings() IntakeSettings {
	return IntakeSettings{
		AutoResolveWebhook: true,
	}
}

// LoadIntakeSettings returns the effective intake settings: the stored blob
// merged over the built-in defaults. A nil store or an absent/empty/corrupt
// blob yields the built-in defaults (auto-resolve ON) — never an error,
// mirroring the ReadBlob "fresh start" contract. Callers get a fresh value
// each time, so there is no shared mutable state to guard.
func LoadIntakeSettings(st storage.Provider) IntakeSettings {
	def := DefaultIntakeSettings()
	if st == nil {
		return def
	}
	data, err := st.ReadBlob(IntakeSettingsBlobName)
	if err != nil || len(data) == 0 {
		return def
	}
	// Unmarshal onto the defaults so a partial blob keeps the default for any
	// omitted field. The bool carries no omitempty, so an explicit `false`
	// round-trips faithfully.
	got := def
	if err := json.Unmarshal(data, &got); err != nil {
		return def
	}
	return got
}

// SaveIntakeSettings persists the intake settings blob. It returns
// ErrIntakeNoStorage when no backend is configured so the API can map it to
// 503, consistent with the report settings path.
func SaveIntakeSettings(st storage.Provider, s IntakeSettings) error {
	if st == nil {
		return ErrIntakeNoStorage
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return st.WriteBlob(IntakeSettingsBlobName, data)
}
