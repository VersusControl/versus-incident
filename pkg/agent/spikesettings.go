package agent

import (
	"encoding/json"
	"errors"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

// spikesettings.go — the runtime settings store for the log volume-spike
// detector's GLOBAL default baseline mode. It replaces the removed
// `spike_baseline_mode` YAML key: the baseline mode is a NON-SECRET operational
// choice, so it is set at runtime in the admin UI and persisted via the storage
// provider blob seam rather than baked into config at boot.
//
// The store is read-through: callers fetch a fresh value with LoadSpikeSettings
// each time, so there is no process-wide mutable settings global to guard. The
// precedence for a given pattern stays "per-pattern column → this stored global
// → built-in default"; a fresh install (no blob) uses the built-in default
// baseline.

// SpikeSettingsBlobName is the single blob the spike baseline-mode setting
// lives under, in the models/settings/ namespace (a sibling of the report
// settings blob and the model-state artifacts).
const SpikeSettingsBlobName = "models/settings/spike-baseline"

// ErrSpikeNoStorage is returned by SaveSpikeSettings when no backend is
// configured, so the API can map it to a 503.
var ErrSpikeNoStorage = errors.New("spike settings: storage not configured")

// SpikeSettings is the non-secret runtime configuration for the spike
// detector's global default baseline mode. It is the JSON shape persisted in
// the settings blob and the shape the admin GET/PUT endpoints exchange.
type SpikeSettings struct {
	// BaselineMode is the GLOBAL default baseline the spike z-score is measured
	// against for any pattern that does not carry its own per-pattern override:
	// "default" (global rate baseline), "average" (cumulative mean of the rate,
	// never decays), or "time_of_day" (hour-of-day baseline).
	BaselineMode string `json:"baseline_mode"`
}

// DefaultSpikeSettings is the built-in floor applied when the store holds no
// value: the global rate baseline. A fresh install therefore scores spikes
// against the global baseline until an operator picks another mode in the UI.
func DefaultSpikeSettings() SpikeSettings {
	return SpikeSettings{BaselineMode: baselineModeDefault}
}

// KnownBaselineMode reports whether mode is exactly one of the three recognized
// baseline modes. The API uses it to reject an unknown mode with a 400 rather
// than silently folding it to the default, which is the store's job for a
// hand-edited or legacy blob.
func KnownBaselineMode(mode string) bool {
	switch mode {
	case baselineModeDefault, baselineModeAverage, baselineModeTimeOfDay:
		return true
	default:
		return false
	}
}

// sanitize folds an unrecognized or absent mode to the built-in default. It is
// applied on both read and write so a hand-edited or legacy blob can never
// yield an out-of-range setting the detector would have to interpret.
func (s SpikeSettings) sanitize() SpikeSettings {
	s.BaselineMode = normalizeBaselineMode(s.BaselineMode)
	return s
}

// LoadSpikeSettings returns the effective spike settings: the stored blob
// merged over the built-in default, sanitized. A nil store or an
// absent/empty/corrupt blob yields the built-in default — never an error,
// mirroring the ReadBlob "fresh start" contract. Callers get a fresh value each
// time, so there is no shared mutable state to guard.
func LoadSpikeSettings(st storage.Provider) SpikeSettings {
	def := DefaultSpikeSettings()
	if st == nil {
		return def
	}
	data, err := st.ReadBlob(SpikeSettingsBlobName)
	if err != nil || len(data) == 0 {
		return def
	}
	got := def
	if err := json.Unmarshal(data, &got); err != nil {
		return def
	}
	return got.sanitize()
}

// SaveSpikeSettings persists the settings blob after sanitizing it. It returns
// ErrSpikeNoStorage when no backend is configured so the API can map it to 503.
func SaveSpikeSettings(st storage.Provider, s SpikeSettings) error {
	if st == nil {
		return ErrSpikeNoStorage
	}
	data, err := json.Marshal(s.sanitize())
	if err != nil {
		return err
	}
	return st.WriteBlob(SpikeSettingsBlobName, data)
}
