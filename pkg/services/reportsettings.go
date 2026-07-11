package services

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

// reportsettings.go — the OSS runtime settings store for the incidents
// analytics report. It replaces the removed `report:` YAML block: report
// settings are NON-SECRET operational toggles (enable, default channel,
// include-charts, rate-limit, default window), so they are set at runtime in
// the admin UI and persisted via the existing storage.Provider blob seam —
// NOT the enterprise encrypted per-org channel store (that store exists to
// seal channel credentials; these carry none).
//
// The store is read-through on every request: callers fetch a fresh copy with
// LoadReportSettings, so there is no process-wide mutable settings global to
// clone (golden rule #4 — no global config mutation). Precedence is simply
// "stored value → built-in default"; there is no YAML layer anymore, so a
// fresh install has the feature off.

// ReportSettingsBlobName is the single blob the report settings live under,
// in the models/settings/ namespace (a sibling of the model-state artifacts,
// deliberately NOT the encrypted channel store).
const ReportSettingsBlobName = "models/settings/report-settings"

// reportWindowToday / reportWindow24h / reportWindow7d are the only valid
// windows. An unknown/absent window defaults to today.
const (
	reportWindowToday = "today"
	reportWindow24h   = "24h"
	reportWindow7d    = "7d"
)

// defaultSendTime / defaultTimezone are the scheduler defaults applied when
// the store holds no value: 09:00 wall-clock in UTC. A fresh install has the
// daily schedule OFF (ScheduleEnabled false), so these only take effect once
// an operator enables it.
const (
	defaultSendTime = "09:00"
	defaultTimezone = "UTC"
)

// sendTimeRe matches a 24-hour "HH:MM" wall-clock time (00:00–23:59). It is
// the single source of truth for the scheduler send-time format, shared by the
// admin PUT validator (ValidSendTime) and the due-check parser (parseSendTime).
var sendTimeRe = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

// ReportSettings is the non-secret runtime configuration for the incidents
// analytics report. It is the JSON shape persisted in the settings blob and
// the shape the admin GET/PUT endpoints exchange.
type ReportSettings struct {
	Enable         bool   `json:"enable"`
	DefaultChannel string `json:"default_channel"`
	IncludeChart   bool   `json:"include_chart"`
	RatePerMinute  int    `json:"rate_per_minute"`
	DefaultWindow  string `json:"default_window"`
	// ScheduleEnabled turns on the recurring daily digest: when Enable AND
	// ScheduleEnabled are both true, the report is sent once per local day at
	// SendTime in Timezone, over DefaultWindow to DefaultChannel. Default
	// false — a fresh install never sends on a schedule until an operator
	// opts in.
	ScheduleEnabled bool `json:"schedule_enabled"`
	// SendTime is the wall-clock "HH:MM" (24-hour) at which the daily digest
	// fires, interpreted in Timezone. Default "09:00".
	SendTime string `json:"send_time"`
	// Timezone is the IANA location name (e.g. "UTC" or "Asia/Ho_Chi_Minh")
	// that BOTH schedules the daily send AND controls the report's printed
	// timestamps and window bounds. Default "UTC" — which keeps the rendered
	// report byte-for-byte identical to the pre-timezone behaviour.
	Timezone string `json:"timezone"`
}

// DefaultReportSettings is the built-in floor applied when the store holds no
// value: the feature is OFF, charts on, a 6/min render cap, today as the
// default window, and the daily schedule OFF (09:00 UTC once enabled). A fresh
// install therefore has the report disabled until an operator enables it in
// the UI.
func DefaultReportSettings() ReportSettings {
	return ReportSettings{
		Enable:          false,
		DefaultChannel:  "",
		IncludeChart:    true,
		RatePerMinute:   6,
		DefaultWindow:   reportWindowToday,
		ScheduleEnabled: false,
		SendTime:        defaultSendTime,
		Timezone:        defaultTimezone,
	}
}

// normalizeReportWindow returns w when it is a recognized window, else the
// default "today". It is the single boundary that maps an unknown/absent
// window to the safe default.
func normalizeReportWindow(w string) string {
	switch strings.TrimSpace(w) {
	case reportWindow24h:
		return reportWindow24h
	case reportWindow7d:
		return reportWindow7d
	default:
		return reportWindowToday
	}
}

// sanitize clamps a settings value into a valid shape: a recognized default
// window, a non-negative rate, and a non-empty send-time/timezone (falling
// back to the built-in defaults). It is applied on both read and write so a
// hand-edited or legacy blob can never yield an out-of-range setting.
func (s ReportSettings) sanitize() ReportSettings {
	s.DefaultChannel = strings.TrimSpace(s.DefaultChannel)
	s.DefaultWindow = normalizeReportWindow(s.DefaultWindow)
	if s.RatePerMinute < 0 {
		s.RatePerMinute = 0
	}
	s.SendTime = strings.TrimSpace(s.SendTime)
	if s.SendTime == "" {
		s.SendTime = defaultSendTime
	}
	s.Timezone = strings.TrimSpace(s.Timezone)
	if s.Timezone == "" {
		s.Timezone = defaultTimezone
	}
	return s
}

// Location resolves the settings Timezone to a *time.Location. An empty or
// unloadable value falls back to UTC so rendering and scheduling never fail on
// a bad blob (the PUT boundary rejects invalid zones, so this is defence in
// depth for a legacy/hand-edited blob).
func (s ReportSettings) Location() *time.Location {
	tz := strings.TrimSpace(s.Timezone)
	if tz == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.UTC
	}
	return loc
}

// ValidSendTime reports whether v is a valid "HH:MM" 24-hour wall-clock time.
// It is the boundary check the admin PUT handler applies before persisting.
func ValidSendTime(v string) bool {
	return sendTimeRe.MatchString(strings.TrimSpace(v))
}

// ValidTimezone reports whether v is "UTC" or a loadable IANA location name.
// It is the boundary check the admin PUT handler applies before persisting.
func ValidTimezone(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	_, err := time.LoadLocation(v)
	return err == nil
}

// LoadReportSettings returns the effective report settings: the stored blob
// merged over the built-in defaults, sanitized. A nil store or an
// absent/empty/corrupt blob yields the built-in defaults (feature off) —
// never an error, mirroring the ReadBlob "fresh start" contract. Callers get
// a fresh value each time, so there is no shared mutable state to guard.
func LoadReportSettings(st storage.Provider) ReportSettings {
	def := DefaultReportSettings()
	if st == nil {
		return def
	}
	data, err := st.ReadBlob(ReportSettingsBlobName)
	if err != nil || len(data) == 0 {
		return def
	}
	// Unmarshal onto the defaults so a partial blob keeps the default for
	// any omitted field.
	got := def
	if err := json.Unmarshal(data, &got); err != nil {
		return def
	}
	return got.sanitize()
}

// SaveReportSettings persists the settings blob after sanitizing it. It
// returns ErrReportNoStorage when no backend is configured so the API can map
// it to 503, consistent with the render/send paths.
func SaveReportSettings(st storage.Provider, s ReportSettings) error {
	if st == nil {
		return ErrReportNoStorage
	}
	data, err := json.Marshal(s.sanitize())
	if err != nil {
		return err
	}
	return st.WriteBlob(ReportSettingsBlobName, data)
}
