package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/agent"
	"github.com/VersusControl/versus-incident/pkg/incidentcounts"
	"github.com/VersusControl/versus-incident/pkg/logging"
	"github.com/VersusControl/versus-incident/pkg/services"
	"github.com/VersusControl/versus-incident/pkg/storage"

	"github.com/gofiber/fiber/v2"
)

// countingStore counts the window counts that reach storage, and can make
// them fail the way a count that ran out of time does.
type countingStore struct {
	storage.Provider
	counts    atomic.Int32
	timeout   atomic.Bool
	blobReads atomic.Int32
	// listTimeout makes page reads fail the way a read that ran out of time
	// does.
	listTimeout atomic.Bool
}

func (s *countingStore) ReadBlob(name string) ([]byte, error) {
	s.blobReads.Add(1)
	return s.Provider.ReadBlob(name)
}

func (s *countingStore) ListIncidentsPageContext(ctx context.Context, origin string, offset, limit int) ([]*storage.IncidentRecord, error) {
	if s.listTimeout.Load() {
		return nil, fmt.Errorf("list incidents page: %w", storage.ErrListTimeout)
	}
	return s.ListIncidentsPage(origin, offset, limit)
}

func (s *countingStore) CountIncidentsByStatusSinceContext(_ context.Context, since time.Time) (storage.IncidentStatusCounts, error) {
	s.counts.Add(1)
	if s.timeout.Load() {
		return storage.IncidentStatusCounts{}, fmt.Errorf("count incidents: %w", storage.ErrCountTimeout)
	}
	return s.Provider.(storage.IncidentWindowCounter).CountIncidentsByStatusSince(since)
}

func (s *countingStore) CountIncidents() (storage.IncidentCounts, error) {
	return s.Provider.(storage.IncidentPager).CountIncidents()
}

func (s *countingStore) CountIncidentsByStatus() (storage.IncidentStatusCounts, error) {
	return s.Provider.(storage.IncidentPager).CountIncidentsByStatus()
}

func (s *countingStore) ListIncidentsPage(origin string, offset, limit int) ([]*storage.IncidentRecord, error) {
	return s.Provider.(storage.IncidentPager).ListIncidentsPage(origin, offset, limit)
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// installCountingStore installs a counting store over a memory store holding
// open webhook incidents, and a count cache that reads the test's clock.
func installCountingStore(t *testing.T, open int) (*countingStore, *testClock) {
	t.Helper()
	mem := storage.NewMemory()
	now := time.Now().UTC()
	for i := range open {
		if err := mem.SaveIncident(&storage.IncidentRecord{
			ID: fmt.Sprintf("open-%d", i), Origin: storage.OriginWebhook, Source: "webhook", CreatedAt: now.Add(-time.Minute),
		}); err != nil {
			t.Fatalf("SaveIncident: %v", err)
		}
	}
	store := &countingStore{Provider: mem}
	services.SetStorage(store)
	clock := &testClock{now: now}
	services.SetIncidentCounts(incidentcounts.New(incidentcounts.Options{Now: clock.Now, Throttle: logging.NewThrottle(0, clock.Now)}))
	t.Cleanup(func() { services.SetStorage(nil) })
	return store, clock
}

func countsApp(ctrl *IncidentAdminController) *fiber.App {
	app := fiber.New()
	app.Get("/counts", ctrl.counts)
	app.Get("/list", ctrl.list)
	app.Get("/search", ctrl.search)
	app.Put("/count-settings", NewCountSettingsController().putSettings)
	app.Post("/:id/resolve", ctrl.resolve)
	return app
}

func putCountWindow(t *testing.T, app *fiber.App, window string) {
	t.Helper()
	req := httptest.NewRequest("PUT", "/count-settings", strings.NewReader(`{"window":"`+window+`"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("PUT count settings: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT count settings status = %d body %s", resp.StatusCode, body)
	}
}

func countsCall(t *testing.T, app *fiber.App, method, path string) map[string]any {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest(method, path, nil), -1)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("%s %s status = %d body %s", method, path, resp.StatusCode, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}
	return out
}

func openTotal(t *testing.T, counts map[string]any) float64 {
	t.Helper()
	byStatus, ok := counts["by_status"].(map[string]any)
	if !ok {
		t.Fatalf("counts carry no by_status: %v", counts)
	}
	return byStatus["open"].(map[string]any)["total"].(float64)
}

// Polls inside FreshFor are served from the cache even while a burst of new
// incidents arrives; the operator's own resolve makes the next poll recount
// once and show it. Every served count carries the time it was counted.
func TestCountsPollsShareTheCacheAndResolveRecounts(t *testing.T) {
	store, clock := installCountingStore(t, 2)
	app := countsApp(NewIncidentAdminController())

	first := countsCall(t, app, "GET", "/counts")
	if openTotal(t, first) != 2 || first["counted_at"] != clock.Now().UTC().Format(time.RFC3339) {
		t.Fatalf("first poll = %v, want 2 open counted now", first)
	}
	if _, ok := first["count_unknown"]; ok {
		t.Fatalf("a good count carries count_unknown: %v", first)
	}
	if _, ok := first["stale"]; ok {
		t.Fatalf("a fresh count carries stale: %v", first)
	}

	now := time.Now().UTC()
	for i := range 1000 {
		if err := store.SaveIncident(&storage.IncidentRecord{
			ID: fmt.Sprintf("burst-%d", i), Origin: storage.OriginWebhook, Source: "fluent-bit", CreatedAt: now,
		}); err != nil {
			t.Fatalf("SaveIncident: %v", err)
		}
	}
	clock.Advance(incidentcounts.FreshFor - time.Second)
	for range 5 {
		if got := countsCall(t, app, "GET", "/counts"); openTotal(t, got) != 2 {
			t.Fatalf("poll inside FreshFor after 1000 inserts = %v, want the cached 2", got)
		}
		list := countsCall(t, app, "GET", "/list?page_size=1")
		if openTotal(t, list["counts"].(map[string]any)) != 2 || list["total"].(float64) != 2 {
			t.Fatalf("list inside FreshFor = counts %v total %v, want the cached 2", list["counts"], list["total"])
		}
	}
	if got := store.counts.Load(); got != 1 {
		t.Fatalf("counts reaching storage after 1000 inserts and 10 polls inside FreshFor = %d, want 1", got)
	}

	countsCall(t, app, "POST", "/open-0/resolve")
	after := countsCall(t, app, "GET", "/counts")
	if openTotal(t, after) != 1001 {
		t.Fatalf("poll after resolve = %v, want a recount showing 1001 open", after)
	}
	countsCall(t, app, "GET", "/counts")
	if got := store.counts.Load(); got != 2 {
		t.Fatalf("counts reaching storage after one resolve and two polls = %d, want 2", got)
	}
}

// A changed count window is counted again under its own name, and the old
// window's numbers are never served for it.
func TestCountsWindowChangeCountsAgain(t *testing.T) {
	store, _ := installCountingStore(t, 1)
	if err := store.SaveIncident(&storage.IncidentRecord{
		ID: "older", Origin: storage.OriginWebhook, Source: "webhook", CreatedAt: time.Now().UTC().Add(-10 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("SaveIncident: %v", err)
	}
	app := countsApp(NewIncidentAdminController())
	if got := countsCall(t, app, "GET", "/counts"); openTotal(t, got) != 1 || got["count_window"] != agent.CountWindow7d {
		t.Fatalf("7d poll = %v, want 1 open in 7d", got)
	}
	// Saved through the settings API, the new window applies to this
	// process's very next count.
	putCountWindow(t, app, agent.CountWindow30d)
	if got := countsCall(t, app, "GET", "/counts"); openTotal(t, got) != 2 || got["count_window"] != agent.CountWindow30d {
		t.Fatalf("30d poll right after the settings save = %v, want 2 open in 30d", got)
	}
	if got := store.counts.Load(); got != 2 {
		t.Fatalf("counts reaching storage = %d, want one per window", got)
	}
}

// With no good count the response says so: every number is null, never
// zero, and the list's total is null too. A last good count that ages past
// StaleAfter while recounts fail is served with stale: true.
func TestCountsUnknownAndStale(t *testing.T) {
	store, clock := installCountingStore(t, 3)
	store.timeout.Store(true)
	app := countsApp(NewIncidentAdminController())

	unknown := countsCall(t, app, "GET", "/counts")
	if unknown["count_unknown"] != true {
		t.Fatalf("timed-out first count = %v, want count_unknown", unknown)
	}
	for _, field := range []string{"ai_detect", "webhook", "total", "by_status"} {
		value, ok := unknown[field]
		if !ok || value != nil {
			t.Fatalf("unknown count field %s = %v (present %v), want null", field, value, ok)
		}
	}
	if _, ok := unknown["counted_at"]; ok {
		t.Fatalf("unknown count carries counted_at: %v", unknown)
	}
	list := countsCall(t, app, "GET", "/list")
	if total, ok := list["total"]; !ok || total != nil {
		t.Fatalf("list total with an unknown count = %v (present %v), want null", total, ok)
	}
	if counts := list["counts"].(map[string]any); counts["count_unknown"] != true || counts["total"] != nil {
		t.Fatalf("list counts with an unknown count = %v", counts)
	}

	store.timeout.Store(false)
	clock.Advance(incidentcounts.FreshFor)
	good := countsCall(t, app, "GET", "/counts")
	countedAt := good["counted_at"]
	if openTotal(t, good) != 3 || countedAt == nil {
		t.Fatalf("recovered count = %v, want 3 with counted_at", good)
	}

	store.timeout.Store(true)
	clock.Advance(incidentcounts.FreshFor)
	countsCall(t, app, "GET", "/counts")
	clock.Advance(incidentcounts.StaleAfter)
	stale := countsCall(t, app, "GET", "/counts")
	if openTotal(t, stale) != 3 || stale["stale"] != true || stale["counted_at"] != countedAt {
		t.Fatalf("aged count = %v, want the last good 3 counted at %v with stale", stale, countedAt)
	}
}

// Once the count and the window setting are held, a poll inside FreshFor
// touches storage not at all: no count, and no read of the setting. During a
// stall on the incident table, a poll must not wait for a pool connection.
func TestFreshCountPollMakesNoStorageReads(t *testing.T) {
	store, clock := installCountingStore(t, 2)
	app := countsApp(NewIncidentAdminController())
	countsCall(t, app, "GET", "/counts")
	counts, reads := store.counts.Load(), store.blobReads.Load()
	if counts != 1 || reads == 0 {
		t.Fatalf("first poll: counts %d, setting reads %d; want 1 count and the setting read", counts, reads)
	}
	clock.Advance(incidentcounts.FreshFor - time.Second)
	for range 10 {
		if got := countsCall(t, app, "GET", "/counts"); openTotal(t, got) != 2 {
			t.Fatalf("fresh poll = %v", got)
		}
	}
	if store.counts.Load() != counts || store.blobReads.Load() != reads {
		t.Fatalf("ten fresh polls reached storage: counts %d -> %d, setting reads %d -> %d; want none",
			counts, store.counts.Load(), reads, store.blobReads.Load())
	}
}

// A window saved on another replica reaches this process within
// CountSettingsTTL: the poll that finds the setting old still answers at once
// with the held window, and a read behind it brings the new one.
func TestCountWindowFromAnotherReplicaShowsWithinTheTTL(t *testing.T) {
	store, _ := installCountingStore(t, 1)
	settingsClock := &testClock{now: time.Now().UTC()}
	services.SetCountSettingsClock(settingsClock.Now)
	t.Cleanup(func() { services.SetCountSettingsClock(nil) })
	app := countsApp(NewIncidentAdminController())
	if got := countsCall(t, app, "GET", "/counts"); got["count_window"] != agent.CountWindow7d {
		t.Fatalf("first poll window = %v, want 7d", got["count_window"])
	}
	// Another replica saves 30d straight to the shared store.
	if err := agent.SaveCountSettings(store, agent.CountSettings{Window: agent.CountWindow30d}); err != nil {
		t.Fatal(err)
	}
	settingsClock.Advance(services.CountSettingsTTL - time.Second)
	if got := countsCall(t, app, "GET", "/counts"); got["count_window"] != agent.CountWindow7d {
		t.Fatalf("poll inside the TTL window = %v, want the held 7d", got["count_window"])
	}
	settingsClock.Advance(time.Second)
	reads := store.blobReads.Load()
	if got := countsCall(t, app, "GET", "/counts"); got["count_window"] != agent.CountWindow7d {
		t.Fatalf("poll that finds the setting old = %v, want the held 7d at once", got["count_window"])
	}
	eventually(t, "the background read of the setting", func() bool { return store.blobReads.Load() > reads })
	eventually(t, "the other replica's window", func() bool {
		return countsCall(t, app, "GET", "/counts")["count_window"] == agent.CountWindow30d
	})
}

// A list page read that runs out of time answers a retryable 503 with no page,
// never an empty or partial list shown as complete.
func TestListReadPastTheLimitAnswersRetryable503(t *testing.T) {
	store, _ := installCountingStore(t, 3)
	store.listTimeout.Store(true)
	app := countsApp(NewIncidentAdminController())
	resp, err := app.Test(httptest.NewRequest("GET", "/list", nil), -1)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != fiber.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("timed-out list = %d (Retry-After %q) %s; want a retryable 503", resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["incidents"]; ok || out["retryable"] != true {
		t.Fatalf("timed-out list body = %v; want no incidents and retryable", out)
	}
}

// eventually waits until cond holds, failing only at a hang guard a passing
// test never comes near.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for pause := time.Millisecond; !cond(); pause = min(2*pause, 10*time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("hang guard reached: %s", what)
		}
		time.Sleep(pause) // testlint:allow poll pause of the wait helper; a background settings read has no notification
	}
}
