package agent

import (
	"context"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// TestCatalog_Upsert_RefreshesServiceOnReObservation covers BUG 1: an
// already-learned pattern must adopt the resolved service on re-observation
// (so an operator's "Reassign to service" override re-points it), WITHOUT a
// later empty/"_unknown" tick ever clobbering a good stored service.
func TestCatalog_Upsert_RefreshesServiceOnReObservation(t *testing.T) {
	SetCatalogStore(nil)

	tests := []struct {
		name      string
		initial   string // service stamped when the pattern is first created
		reobserve string // service supplied on the second (existing-path) Upsert
		want      string
	}{
		{"real re-points to a new real service", "billing", "payments", "payments"},
		{"real service fills a previously unknown attribution", "_unknown", "payments", "payments"},
		{"real service fills a previously empty attribution", "", "payments", "payments"},
		{"empty never clobbers a real service", "payments", "", "payments"},
		{"_unknown never clobbers a real service", "payments", "_unknown", "payments"},
		{"empty leaves an unknown attribution untouched", "_unknown", "", "_unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cat, err := LoadCatalog(storage.NewMemory())
			if err != nil {
				t.Fatalf("LoadCatalog: %v", err)
			}
			// First sighting creates the pattern with the initial attribution.
			cat.Upsert("p-1", "tpl <*>", "src", 1, 0.2, "", tt.initial)
			// Re-observation exercises the existing-pattern path.
			cat.Upsert("p-1", "tpl <*>", "src", 1, 0.2, "", tt.reobserve)

			got := cat.Get("p-1")
			if got == nil {
				t.Fatalf("pattern p-1 missing after upsert")
			}
			if got.Service != tt.want {
				t.Fatalf("Service = %q, want %q", got.Service, tt.want)
			}
			// Re-pointing the attribution must never disturb the count folding.
			if got.Count != 2 {
				t.Fatalf("Count = %d, want 2 (both observations folded)", got.Count)
			}
		})
	}
}

// TestLogBrain_ReObservationRepointsServiceViaOverride is the end-to-end-ish
// proof of BUG 1: with a ServiceOverride resolver installed, re-observing an
// already-learned pattern re-points its catalog Service to the override target
// (the exact "Reassign to service" flow: an override is stored, then the log
// brain resolves it on the next tick and Upsert adopts it).
func TestLogBrain_ReObservationRepointsServiceViaOverride(t *testing.T) {
	SetCatalogStore(nil)
	SetServiceOverride(nil) // start with no override wired

	cat, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	miner := NewMiner(0.4, 4, 100)
	// nil matcher (no regex pre-filter) + nil service matcher (Extract → "",
	// so auto-detection yields "_unknown") isolate the override behaviour.
	brain := newLogBrain("src", miner, cat, nil, nil, 0.2, config.AgentCatalogConfig{})
	ctx := context.Background()

	const msg = "checkout failed for order 123"

	// First observation, no override installed: attribution is "_unknown".
	obs, err := brain.Group(ctx, []core.Signal{{Message: msg}})
	if err != nil {
		t.Fatalf("Group: %v", err)
	}
	if len(obs) != 1 {
		t.Fatalf("expected 1 observation, got %d", len(obs))
	}
	if err := brain.Learn(ctx, obs); err != nil {
		t.Fatalf("Learn: %v", err)
	}
	id := obs[0].Key
	if got := cat.Get(id); got == nil || got.Service != "_unknown" {
		t.Fatalf("initial Service = %v, want _unknown", got)
	}

	// Operator reassigns the pattern: install an override that re-points it.
	SetServiceOverride(&stubOverride{service: "payments", ok: true})
	t.Cleanup(func() { SetServiceOverride(nil) })

	// Re-observe the SAME pattern: the brain resolves the override and Upsert's
	// existing-pattern path must adopt it.
	obs2, err := brain.Group(ctx, []core.Signal{{Message: msg}})
	if err != nil {
		t.Fatalf("Group (re-observe): %v", err)
	}
	if err := brain.Learn(ctx, obs2); err != nil {
		t.Fatalf("Learn (re-observe): %v", err)
	}
	if got := cat.Get(id); got == nil || got.Service != "payments" {
		t.Fatalf("re-pointed Service = %v, want payments (override never re-pointed the learned pattern)", got)
	}
}

// TestCatalog_Label_VerdictTriState covers BUG 2: Label's tri-state *string
// verdict — set, clear (empty string PRESENT), and tags-only (verdict absent)
// must leave the verdict unchanged.
func TestCatalog_Label_VerdictTriState(t *testing.T) {
	SetCatalogStore(nil)

	tests := []struct {
		name        string
		seedVerdict string   // verdict set before the edit ("" = leave uncurated)
		verdict     *string  // the edit's tri-state verdict
		tags        []string // the edit's tags
		wantVerdict string
	}{
		{"set", "", strptr("known"), nil, "known"},
		{"clear (empty string present)", "known", strptr(""), nil, ""},
		{"tags-only leaves the verdict unchanged", "known", nil, []string{"noisy"}, "known"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cat, err := LoadCatalog(storage.NewMemory())
			if err != nil {
				t.Fatalf("LoadCatalog: %v", err)
			}
			cat.Upsert("p-1", "tpl <*>", "src", 1, 0.2, "", "api")
			if tt.seedVerdict != "" {
				if !cat.Label("p-1", strptr(tt.seedVerdict), nil) {
					t.Fatalf("seed Label failed")
				}
			}

			if !cat.Label("p-1", tt.verdict, tt.tags) {
				t.Fatalf("Label returned false for existing pattern")
			}
			got := cat.Get("p-1")
			if got.Verdict != tt.wantVerdict {
				t.Fatalf("Verdict = %q, want %q", got.Verdict, tt.wantVerdict)
			}
			if tt.tags != nil && (len(got.Tags) != len(tt.tags) || got.Tags[0] != tt.tags[0]) {
				t.Fatalf("Tags = %v, want %v", got.Tags, tt.tags)
			}
		})
	}
}

// TestCatalog_RepointService_InMemory covers the RETROACTIVE re-point: setting
// an existing pattern's Service must land IMMEDIATELY on the next read (no
// re-observation), while the guard refuses to blank out or unknown-out a good
// attribution.
func TestCatalog_RepointService_InMemory(t *testing.T) {
	SetCatalogStore(nil)

	tests := []struct {
		name    string
		initial string
		repoint string
		want    string
		wantOK  bool
	}{
		{"re-points to a new real service", "billing", "payments", "payments", true},
		{"fills a previously unknown attribution", "_unknown", "payments", "payments", true},
		{"fills a previously empty attribution", "", "payments", "payments", true},
		{"empty target rejected (never blanks a real service)", "payments", "", "payments", false},
		{"_unknown target rejected (never unknown-outs)", "payments", "_unknown", "payments", false},
		{"no-op when already pointed there", "payments", "payments", "payments", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cat, err := LoadCatalog(storage.NewMemory())
			if err != nil {
				t.Fatalf("LoadCatalog: %v", err)
			}
			cat.Upsert("p-1", "tpl <*>", "src", 1, 0.2, "", tt.initial)

			if got := cat.RepointService("p-1", tt.repoint); got != tt.wantOK {
				t.Fatalf("RepointService ok = %v, want %v", got, tt.wantOK)
			}
			// The change is visible on the very next read — no re-observation.
			if got := cat.Get("p-1"); got == nil || got.Service != tt.want {
				t.Fatalf("Service = %v, want %q", got, tt.want)
			}
		})
	}

	// A re-point of a non-existent pattern (the message-substring override
	// case) is a no-op, leaving the lazy re-observation path to apply it.
	cat, err := LoadCatalog(storage.NewMemory())
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	if cat.RepointService("no-such-id", "payments") {
		t.Fatalf("RepointService of a missing pattern returned true, want false")
	}
}

// TestCatalog_RepointService_RoutesThroughStore proves that with a CatalogStore
// installed a re-point issues exactly ONE Curate(CatalogEditRepointService)
// carrying the pattern id + new target, so the enterprise partition store
// re-points its fleet-wide read view.
func TestCatalog_RepointService_RoutesThroughStore(t *testing.T) {
	fake := &fakeCatalogStore{}
	SetCatalogStore(fake)
	t.Cleanup(func() { SetCatalogStore(nil) })

	cat, err := LoadCatalog(nil)
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	if !cat.RepointService("p-ec7767235887", "cache") {
		t.Fatalf("RepointService returned false, want true (store Curate succeeded)")
	}

	_, _, _, curates := fake.counts()
	if curates != 1 {
		t.Fatalf("store curate calls = %d, want exactly 1", curates)
	}
	edit := fake.curates[0]
	if edit.Kind != CatalogEditRepointService {
		t.Errorf("curate kind = %q, want %q", edit.Kind, CatalogEditRepointService)
	}
	if edit.PatternID != "p-ec7767235887" || edit.Service != "cache" {
		t.Errorf("curate = {PatternID:%q, Service:%q}, want {p-ec7767235887, cache}", edit.PatternID, edit.Service)
	}

	// The guard runs BEFORE the store, so a blank/"_unknown" target never even
	// reaches Curate.
	if cat.RepointService("p-1", "") || cat.RepointService("p-1", "_unknown") {
		t.Fatalf("guarded target reached the store")
	}
	if _, _, _, n := fake.counts(); n != 1 {
		t.Fatalf("store curate calls after guarded re-points = %d, want still 1", n)
	}
}
