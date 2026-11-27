package services

import (
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// CountSettingsTTL is how long this process uses the count window setting it
// holds before reading it again, so a change saved on another replica shows
// within it. A change saved through this process shows at once.
const CountSettingsTTL = 30 * time.Second

// countSettingsMemo keeps the count window setting in memory. Every count
// request needs the window, and reading it from storage each time made a
// cached count wait for a database connection whenever the pool was busy.
// After the first read, requests never wait on storage: a value older than
// CountSettingsTTL is served while one background read replaces it.
type countSettingsMemo struct {
	now func() time.Time

	mu         sync.Mutex
	value      agent.CountSettings
	readAt     time.Time
	has        bool
	refreshing bool
	// generation changes whenever the held value is replaced, so a background
	// read that started before a save never overwrites the saved value.
	generation uint64
}

var (
	countSettingsMu  sync.Mutex
	countSettingsNow = time.Now
	countSettings    = &countSettingsMemo{now: time.Now}
)

func currentCountSettings() *countSettingsMemo {
	countSettingsMu.Lock()
	defer countSettingsMu.Unlock()
	return countSettings
}

// resetCountSettings starts an empty memo for a newly installed store.
func resetCountSettings() {
	countSettingsMu.Lock()
	countSettings = &countSettingsMemo{now: countSettingsNow}
	countSettingsMu.Unlock()
}

// SetCountSettingsClock makes the count window memo read the time from now and
// starts it empty, so a test can move past CountSettingsTTL. A nil now
// restores the real clock.
func SetCountSettingsClock(now func() time.Time) {
	if now == nil {
		now = time.Now
	}
	countSettingsMu.Lock()
	countSettingsNow = now
	countSettingsMu.Unlock()
	resetCountSettings()
}

// CountSettings returns the count window setting for st, the installed store.
// Only the first call, and calls after a failed first read, read storage on
// the request path.
func CountSettings(st storage.Provider) agent.CountSettings {
	if st == nil {
		return agent.DefaultCountSettings()
	}
	m := currentCountSettings()
	m.mu.Lock()
	if !m.has {
		m.mu.Unlock()
		value, err := agent.ReadCountSettings(st)
		if err != nil {
			// Not remembered: the next request reads again rather than
			// holding the default in place of the operator's setting.
			return value
		}
		m.mu.Lock()
		if !m.has {
			m.value, m.readAt, m.has = value, m.now(), true
			m.generation++
		}
		value = m.value
		m.mu.Unlock()
		return value
	}
	value := m.value
	if !m.refreshing && m.now().Sub(m.readAt) >= CountSettingsTTL {
		m.refreshing = true
		go m.refresh(st, m.generation)
	}
	m.mu.Unlock()
	return value
}

func (m *countSettingsMemo) refresh(st storage.Provider, generation uint64) {
	value, err := agent.ReadCountSettings(st)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshing = false
	if m.generation != generation {
		return
	}
	if err != nil {
		// Keep the held value; try again after another TTL.
		m.readAt = m.now()
		return
	}
	m.value, m.readAt = value, m.now()
	m.generation++
}

// RememberCountSettings records a count window setting this process just
// saved, so its counts use it from the next request.
func RememberCountSettings(s agent.CountSettings) {
	m := currentCountSettings()
	m.mu.Lock()
	m.value, m.readAt, m.has = s, m.now(), true
	m.generation++
	m.mu.Unlock()
}
