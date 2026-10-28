package agent

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

// countsettings.go — the runtime setting for how far back the incident COUNTS
// look. Every count surface (the header badge, the Now KPI tiles, the
// Incidents origin/status tabs) reads one authoritative per-origin ×
// per-status tally; this bounds that tally to a recent window so the numbers
// describe current load instead of everything the system has ever seen.
//
// Same shape and contract as spikesettings.go: a non-secret operational choice,
// set at runtime in the admin UI and persisted through the storage blob seam,
// read fresh on each call so there is no process-wide mutable global.

// CountSettingsBlobName is the blob the incident-count window lives under, a
// sibling of the spike and report settings blobs.
const CountSettingsBlobName = "models/settings/incident-count-window"

// ErrCountNoStorage is returned by SaveCountSettings when no backend is
// configured, so the API can map it to a 503.
var ErrCountNoStorage = errors.New("count settings: storage not configured")

// Recognized count windows. "all" keeps the historical whole-set behaviour for
// operators who want it; everything else is a rolling lookback.
const (
	CountWindow24h = "24h"
	CountWindow7d  = "7d"
	CountWindow30d = "30d"
	CountWindow90d = "90d"
	CountWindowAll = "all"
)

// CountSettings is the non-secret runtime configuration for the incident-count
// lookback. It is the JSON shape persisted in the blob and exchanged by the
// admin GET/PUT endpoints.
type CountSettings struct {
	// Window is the rolling lookback the counts cover: 24h, 7d, 30d, 90d, or
	// "all" for no bound.
	Window string `json:"window"`
}

// DefaultCountSettings is the built-in floor: the last 7 days. A fresh install
// therefore reports recent load rather than an all-time total that only ever
// grows.
func DefaultCountSettings() CountSettings {
	return CountSettings{Window: CountWindow7d}
}

// KnownCountWindow reports whether w is exactly one of the recognized windows.
// The API uses it to reject an unknown value with a 400 rather than silently
// folding it to the default, which is the store's job for a hand-edited blob.
func KnownCountWindow(w string) bool {
	switch w {
	case CountWindow24h, CountWindow7d, CountWindow30d, CountWindow90d, CountWindowAll:
		return true
	default:
		return false
	}
}

// sanitize folds an unrecognized or absent window to the built-in default,
// applied on both read and write so a hand-edited or legacy blob can never
// yield a window the count path would have to interpret.
func (s CountSettings) sanitize() CountSettings {
	if !KnownCountWindow(s.Window) {
		s.Window = DefaultCountSettings().Window
	}
	return s
}

// Since converts the window into the earliest created_at a count should
// include. The zero time means "no lower bound" (the "all" window), which is
// what every count path treats as unbounded.
func (s CountSettings) Since(now time.Time) time.Time {
	switch s.sanitize().Window {
	case CountWindow24h:
		return now.Add(-24 * time.Hour)
	case CountWindow7d:
		return now.AddDate(0, 0, -7)
	case CountWindow30d:
		return now.AddDate(0, 0, -30)
	case CountWindow90d:
		return now.AddDate(0, 0, -90)
	default:
		return time.Time{}
	}
}

// LoadCountSettings returns the effective settings: the stored blob merged over
// the built-in default, sanitized. A nil store or an absent/empty/corrupt blob
// yields the default — never an error, mirroring the ReadBlob "fresh start"
// contract.
func LoadCountSettings(st storage.Provider) CountSettings {
	def := DefaultCountSettings()
	if st == nil {
		return def
	}
	data, err := st.ReadBlob(CountSettingsBlobName)
	if err != nil || len(data) == 0 {
		return def
	}
	got := def
	if err := json.Unmarshal(data, &got); err != nil {
		return def
	}
	return got.sanitize()
}

// SaveCountSettings persists the settings blob after sanitizing it. It returns
// ErrCountNoStorage when no backend is configured so the API can map it to 503.
func SaveCountSettings(st storage.Provider, s CountSettings) error {
	if st == nil {
		return ErrCountNoStorage
	}
	data, err := json.Marshal(s.sanitize())
	if err != nil {
		return err
	}
	return st.WriteBlob(CountSettingsBlobName, data)
}
