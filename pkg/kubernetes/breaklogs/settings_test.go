package breaklogs

import (
	"github.com/VersusControl/versus-incident/pkg/storage"
	"sync"
	"testing"
	"time"
)

func TestLinkedIngestionDefaultsAndOptionalZeroOverride(t *testing.T) {
	for kind, seconds := range map[string]int{"loki": 30, "elasticsearch": 60, "signoz": 60, "graylog": 60, "splunk": 120, "cloudwatchlogs": 120} {
		if got := (CaptureSettings{}).IngestionDelay(kind); got != time.Duration(seconds)*time.Second {
			t.Fatalf("%s delay=%s", kind, got)
		}
	}
	zero := 0
	if got := (CaptureSettings{IngestDelay: &zero}).IngestionDelay("splunk"); got != 0 {
		t.Fatal("explicit zero became default", got)
	}
	delay := 90
	provider := storage.NewMemory()
	key := SettingsKey("default", "cluster")
	if err := UpdateSettings(provider, key, func(state *SettingsState) error { state.Settings.BreakLogs.IngestDelay = &delay; return nil }); err != nil {
		t.Fatal(err)
	}
	state, err := LoadSettings(provider, key)
	if err != nil || state.Settings.BreakLogs.IngestionDelay("loki") != 90*time.Second {
		t.Fatal("override lost on reload", state, err)
	}
}

func TestSettingsCASDefaultsIsolationAndContention(t *testing.T) {
	provider := storage.NewMemory()
	key := SettingsKey("org", "cluster")
	initial, err := LoadSettings(provider, key)
	if err != nil || initial.Settings.BreakLogs.LinkedSource != "" || initial.Settings.Learning.Scope.Include == nil {
		t.Fatal("defaults", err)
	}
	var workers sync.WaitGroup
	for _, name := range []string{"one", "two", "three", "four"} {
		workers.Add(1)
		go func(name string) {
			defer workers.Done()
			if err := UpdateSettings(provider, key, func(state *SettingsState) error {
				if state.Health == nil {
					state.Health = map[string]Health{}
				}
				state.Health[name] = Health{State: "healthy", At: time.Now()}
				return nil
			}); err != nil {
				t.Error(err)
			}
		}(name)
	}
	workers.Wait()
	state, err := LoadSettings(provider, key)
	if err != nil || len(state.Health) != 4 {
		t.Fatal("lost update", err)
	}
	for _, other := range []string{SettingsKey("other", "cluster"), SettingsKey("org", "other")} {
		state, err := LoadSettings(provider, other)
		if err != nil || len(state.Health) != 0 {
			t.Fatal("scope leak", err)
		}
	}
}

func TestScopeAllowsFollowsTheSettingsPageMeaning(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		scope     Scope
		namespace string
		want      bool
	}{
		{"empty include reads every namespace", Scope{}, "shop", true},
		{"empty lists from storage read every namespace", Scope{Include: []string{}, Exclude: []string{}}, "shop", true},
		{"empty include still honours exclude", Scope{Exclude: []string{"kube-system"}}, "kube-system", false},
		{"empty include lets other namespaces through exclude", Scope{Exclude: []string{"kube-system"}}, "shop", true},
		{"include names the only namespaces", Scope{Include: []string{"shop"}}, "shop", true},
		{"include leaves other namespaces out", Scope{Include: []string{"shop"}}, "billing", false},
		{"exclude wins over include", Scope{Include: []string{"shop"}, Exclude: []string{"shop"}}, "shop", false},
		{"names match exactly", Scope{Include: []string{"shop"}}, "shop-staging", false},
		{"names are case sensitive", Scope{Include: []string{"shop"}}, "Shop", false},
		{"an empty namespace is never allowed", Scope{}, "", false},
		{"an empty namespace is never allowed by include", Scope{Include: []string{""}}, "", false},
	} {
		if got := test.scope.Allows(test.namespace); got != test.want {
			t.Errorf("%s: Allows(%q) = %v, want %v", test.name, test.namespace, got, test.want)
		}
	}
}
