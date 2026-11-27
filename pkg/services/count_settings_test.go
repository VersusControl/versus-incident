package services

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// settingsHangGuard is how long a test waits for an event before calling it
// a hang. A passing test never comes near it.
const settingsHangGuard = 60 * time.Second

// settingsBlobStore reads the count window blob from an underlying store,
// and can fail those reads or hold them until the test lets them go.
type settingsBlobStore struct {
	storage.Provider
	reads   atomic.Int32
	fail    atomic.Bool
	hold    atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func newSettingsBlobStore(t *testing.T, window string) *settingsBlobStore {
	t.Helper()
	base := storage.NewMemory()
	if err := agent.SaveCountSettings(base, agent.CountSettings{Window: window}); err != nil {
		t.Fatal(err)
	}
	return &settingsBlobStore{Provider: base, entered: make(chan struct{}, 1), release: make(chan struct{})}
}

func (s *settingsBlobStore) ReadBlob(name string) ([]byte, error) {
	if name == agent.CountSettingsBlobName {
		s.reads.Add(1)
		if s.hold.Load() {
			s.entered <- struct{}{}
			<-s.release
		}
		if s.fail.Load() {
			return nil, errors.New("storage unavailable")
		}
	}
	return s.Provider.ReadBlob(name)
}

type settingsClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *settingsClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *settingsClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// installSettingsStore installs store with an empty count window memo that
// reads the returned clock.
func installSettingsStore(t *testing.T, store storage.Provider) *settingsClock {
	t.Helper()
	previous := Storage()
	SetStorage(store)
	clock := &settingsClock{now: time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)}
	SetCountSettingsClock(clock.Now)
	t.Cleanup(func() {
		SetCountSettingsClock(nil)
		SetStorage(previous)
	})
	return clock
}

// waitSettingsRefreshed waits until no background read of the setting runs.
func waitSettingsRefreshed(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(settingsHangGuard)
	for pause := time.Millisecond; ; pause = min(2*pause, 10*time.Millisecond) {
		m := currentCountSettings()
		m.mu.Lock()
		refreshing := m.refreshing
		m.mu.Unlock()
		if !refreshing {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("hang guard reached: the background read of the count window never ended")
		}
		time.Sleep(pause) // testlint:allow poll pause of the wait; the memo has no notification for a finished read
	}
}

// A background read that fails keeps the window this process holds; it never
// replaces the operator's setting with the default.
func TestCountWindowSurvivesAFailedRefresh(t *testing.T) {
	store := newSettingsBlobStore(t, agent.CountWindow30d)
	clock := installSettingsStore(t, store)
	RememberCountSettings(agent.CountSettings{Window: agent.CountWindow30d})

	clock.Advance(CountSettingsTTL)
	store.fail.Store(true)
	if got := CountSettings(store).Window; got != agent.CountWindow30d {
		t.Fatalf("poll that starts the refresh = %s, want the held 30d", got)
	}
	waitSettingsRefreshed(t)
	if store.reads.Load() != 1 {
		t.Fatalf("background reads = %d, want 1", store.reads.Load())
	}
	if got := CountSettings(store).Window; got != agent.CountWindow30d {
		t.Fatalf("after a failed refresh = %s, want 30d kept", got)
	}
}

// A window saved through this process while a background read is running
// wins over what that read returns: the read started before the save.
func TestSavedCountWindowWinsOverARefreshThatStartedBefore(t *testing.T) {
	store := newSettingsBlobStore(t, agent.CountWindow30d)
	clock := installSettingsStore(t, store)
	RememberCountSettings(agent.CountSettings{Window: agent.CountWindow7d})

	clock.Advance(CountSettingsTTL)
	store.hold.Store(true)
	CountSettings(store)
	timer := time.NewTimer(settingsHangGuard)
	defer timer.Stop()
	select {
	case <-store.entered:
	case <-timer.C:
		t.Fatal("hang guard reached: the background read never started")
	}
	RememberCountSettings(agent.CountSettings{Window: agent.CountWindow90d})
	store.hold.Store(false)
	close(store.release)
	waitSettingsRefreshed(t)
	if got := CountSettings(store).Window; got != agent.CountWindow90d {
		t.Fatalf("after the older read finished with 30d = %s, want the saved 90d", got)
	}
}

// A first read that fails answers the default for that request only; the
// next request reads storage again and gets the operator's setting.
func TestFailedFirstCountWindowReadIsNotRemembered(t *testing.T) {
	store := newSettingsBlobStore(t, agent.CountWindow30d)
	installSettingsStore(t, store)
	store.fail.Store(true)
	if got := CountSettings(store).Window; got != agent.CountWindow7d {
		t.Fatalf("failed first read = %s, want the 7d default", got)
	}
	store.fail.Store(false)
	if got := CountSettings(store).Window; got != agent.CountWindow30d {
		t.Fatalf("request after a failed first read = %s, want 30d read again", got)
	}
	if store.reads.Load() != 2 {
		t.Fatalf("storage reads = %d, want 2", store.reads.Load())
	}
}
