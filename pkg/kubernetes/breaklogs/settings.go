package breaklogs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

type Scope struct {
	Include []string `json:"include"`
	Exclude []string `json:"exclude"`
}

type Learning struct {
	Logs    bool  `json:"logs"`
	Metrics bool  `json:"metrics"`
	Traces  bool  `json:"traces"`
	Scope   Scope `json:"scope"`
}

type CaptureSettings struct {
	LinkedSource string            `json:"linked_source"`
	Fields       map[string]Fields `json:"fields"`
	IngestDelay  *int              `json:"ingest_delay,omitempty"`
}

func (settings CaptureSettings) IngestionDelay(kind string) time.Duration {
	if settings.IngestDelay != nil {
		return time.Duration(*settings.IngestDelay) * time.Second
	}
	switch kind {
	case "loki":
		return 30 * time.Second
	case "splunk", "cloudwatchlogs":
		return 2 * time.Minute
	default:
		return time.Minute
	}
}

type Settings struct {
	Learning  Learning        `json:"learning"`
	BreakLogs CaptureSettings `json:"break_logs"`
	Alerts    Alerts          `json:"alerts"`
}

type Alerts struct {
	Enabled   bool      `json:"enabled"`
	Level     string    `json:"level"`
	EnabledAt time.Time `json:"enabled_at"`
	UpdatedBy string    `json:"updated_by"`
}

type Health struct {
	State string    `json:"state"`
	At    time.Time `json:"at"`
}

type SettingsState struct {
	Settings Settings          `json:"settings"`
	Health   map[string]Health `json:"health"`
}

func SettingsKey(org, cluster string) string {
	digest := sha256.Sum256([]byte(org + "\x00" + cluster))
	return "kubernetes/agent-settings/" + hex.EncodeToString(digest[:]) + ".json"
}

func LoadSettings(provider storage.Provider, key string) (SettingsState, error) {
	state := SettingsState{Settings: Settings{Learning: Learning{Scope: Scope{Include: []string{}, Exclude: []string{}}}, BreakLogs: CaptureSettings{Fields: map[string]Fields{}}}, Health: map[string]Health{}}
	state.Settings.Alerts.Level = "critical"
	if provider == nil {
		return state, ErrInvalid
	}
	encoded, err := provider.ReadBlob(key)
	if errors.Is(err, storage.ErrNotFound) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if len(encoded) == 0 {
		return state, nil
	}
	if len(encoded) > 64<<10 || json.Unmarshal(encoded, &state) != nil {
		return SettingsState{}, ErrInvalid
	}
	if state.Health == nil {
		state.Health = map[string]Health{}
	}
	return state, nil
}

func UpdateSettings(provider storage.Provider, key string, update func(*SettingsState) error) error {
	cas, ok := provider.(storage.BlobCAS)
	if !ok {
		return ErrInvalid
	}
	for attempt := 0; attempt < 16; attempt++ {
		encoded, err := provider.ReadBlob(key)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		if errors.Is(err, storage.ErrNotFound) {
			encoded = nil
		}
		state := SettingsState{Settings: Settings{Learning: Learning{Scope: Scope{Include: []string{}, Exclude: []string{}}}, BreakLogs: CaptureSettings{Fields: map[string]Fields{}}}, Health: map[string]Health{}}
		state.Settings.Alerts.Level = "critical"
		if len(encoded) > 64<<10 || len(encoded) > 0 && json.Unmarshal(encoded, &state) != nil {
			return ErrInvalid
		}
		if err := update(&state); err != nil {
			return err
		}
		replacement, err := json.Marshal(state)
		if err != nil || len(replacement) > 64<<10 {
			return ErrInvalid
		}
		if bytes.Equal(encoded, replacement) {
			return nil
		}
		swapped, err := cas.CompareAndSwapBlob(key, encoded, replacement)
		if err != nil {
			return err
		}
		if swapped {
			return nil
		}
	}
	return ErrInvalid
}
