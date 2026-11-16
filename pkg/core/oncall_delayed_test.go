package core_test

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/services"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/redis/go-redis/v9"
)

// pendingRedis keeps the pending-escalation keys the workflow sets, checks and
// deletes, in memory.
type pendingRedis struct {
	redis.UniversalClient
	mu   sync.Mutex
	keys map[string]bool
}

func (r *pendingRedis) Set(ctx context.Context, key string, _ interface{}, _ time.Duration) *redis.StatusCmd {
	r.mu.Lock()
	r.keys[key] = true
	r.mu.Unlock()
	cmd := redis.NewStatusCmd(ctx)
	cmd.SetVal("OK")
	return cmd
}

func (r *pendingRedis) Exists(ctx context.Context, keys ...string) *redis.IntCmd {
	r.mu.Lock()
	defer r.mu.Unlock()
	var n int64
	for _, k := range keys {
		if r.keys[k] {
			n++
		}
	}
	cmd := redis.NewIntCmd(ctx)
	cmd.SetVal(n)
	return cmd
}

func (r *pendingRedis) Del(ctx context.Context, keys ...string) *redis.IntCmd {
	r.mu.Lock()
	for _, k := range keys {
		delete(r.keys, k)
	}
	r.mu.Unlock()
	cmd := redis.NewIntCmd(ctx)
	cmd.SetVal(1)
	return cmd
}

// switchProvider refuses escalations while fail is set, like PagerDuty
// answering 400, and accepts them otherwise.
type switchProvider struct{ fail atomic.Bool }

func (p *switchProvider) TriggerOnCall(context.Context, string, *config.OnCallConfig) error {
	if p.fail.Load() {
		return errors.New("PagerDuty API returned non-success status: 400")
	}
	return nil
}

func waitForRecord(t *testing.T, store storage.Provider, id string, done func(*storage.IncidentRecord) bool) *storage.IncidentRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec, err := store.GetIncident(id)
		if err != nil {
			t.Fatalf("GetIncident: %v", err)
		}
		if done(rec) {
			return rec
		}
		if time.Now().After(deadline) {
			t.Fatalf("record never reached the expected on-call state: triggered=%v error=%q", rec.OnCallTriggered, rec.OnCallError)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// An escalation that fires after the acknowledgement wait and is refused by
// the provider is written onto the stored incident, as an intake-time failure
// is; a later escalation of the same incident that succeeds clears it again.
// Both a plain webhook incident and a detection incident are covered, since
// they persist through different store paths.
func TestDelayedEscalationOutcomeIsWrittenToTheIncident(t *testing.T) {
	t.Cleanup(core.SetOnCallWaitUnitForTest(5 * time.Millisecond))
	for _, tc := range []struct {
		name string
		rec  *storage.IncidentRecord
	}{
		{"webhook incident", &storage.IncidentRecord{ID: "inc-1", OnCallTriggered: true, NotifyStatus: "failed"}},
		{"detection incident", &storage.IncidentRecord{ID: "inc-2", OnCallTriggered: true, NotifyStatus: "sent", DetectionFingerprint: "fp", DetectionEpisodeID: "ep-1", OccurrenceCount: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := storage.NewMemory()
			tc.rec.CreatedAt = time.Now().UTC()
			if err := store.SaveIncident(tc.rec); err != nil {
				t.Fatalf("SaveIncident: %v", err)
			}
			services.SetStorage(store)
			t.Cleanup(func() { services.SetStorage(nil) })

			provider := &switchProvider{}
			provider.fail.Store(true)
			w := core.NewOnCallWorkflow(&pendingRedis{keys: map[string]bool{}}, provider)
			oc := config.OnCallConfig{WaitMinutes: 1}

			if err := w.StartContext(t.Context(), tc.rec.ID, oc); err != nil {
				t.Fatalf("StartContext: %v (the escalation should only be queued)", err)
			}
			failed := waitForRecord(t, store, tc.rec.ID, func(r *storage.IncidentRecord) bool { return r.OnCallError != "" })
			if failed.OnCallTriggered || failed.OnCallError != "PagerDuty API returned non-success status: 400" {
				t.Fatalf("after the refused escalation: triggered=%v error=%q, want not triggered with the provider error", failed.OnCallTriggered, failed.OnCallError)
			}

			provider.fail.Store(false)
			if err := w.StartContext(t.Context(), tc.rec.ID, oc); err != nil {
				t.Fatalf("StartContext: %v", err)
			}
			waitForRecord(t, store, tc.rec.ID, func(r *storage.IncidentRecord) bool { return r.OnCallTriggered && r.OnCallError == "" })
		})
	}
}

// leakyProvider fails the way an HTTP provider does when the transport errors:
// the error carries the full request URL, credential included.
type leakyProvider struct{}

func (leakyProvider) TriggerOnCall(context.Context, string, *config.OnCallConfig) error {
	return &url.Error{Op: "Post", URL: "https://user:SECRETPASS@acme.service-now.com/api/now/table/incident?token=SECRETTOKEN", Err: errors.New("connection refused")}
}

// A delayed escalation failure is stored on the incident with the request URL
// cut to scheme://host, so no credential reaches the incident or the API.
func TestDelayedEscalationErrorIsStoredWithoutCredentials(t *testing.T) {
	t.Cleanup(core.SetOnCallWaitUnitForTest(5 * time.Millisecond))
	store := storage.NewMemory()
	if err := store.SaveIncident(&storage.IncidentRecord{ID: "inc-3", OnCallTriggered: true, NotifyStatus: "sent", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("SaveIncident: %v", err)
	}
	services.SetStorage(store)
	t.Cleanup(func() { services.SetStorage(nil) })

	w := core.NewOnCallWorkflow(&pendingRedis{keys: map[string]bool{}}, leakyProvider{})
	if err := w.StartContext(t.Context(), "inc-3", config.OnCallConfig{WaitMinutes: 1}); err != nil {
		t.Fatalf("StartContext: %v", err)
	}
	rec := waitForRecord(t, store, "inc-3", func(r *storage.IncidentRecord) bool { return r.OnCallError != "" })
	if strings.Contains(rec.OnCallError, "SECRET") || !strings.Contains(rec.OnCallError, `"https://acme.service-now.com"`) {
		t.Fatalf("OnCallError = %q, want the URL cut to scheme://host", rec.OnCallError)
	}
}
