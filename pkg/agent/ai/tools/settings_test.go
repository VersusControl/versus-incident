package tools

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

type settingsReadProvider struct {
	storage.Provider
	err   error
	reads int
}

func (provider *settingsReadProvider) ReadBlob(name string) ([]byte, error) {
	provider.reads++
	if provider.err != nil {
		return nil, provider.err
	}
	return provider.Provider.ReadBlob(name)
}

type settingsTool string

func (tool settingsTool) Name() string          { return string(tool) }
func (settingsTool) Description() string        { return "test" }
func (settingsTool) ArgsSchema() map[string]any { return map[string]any{"type": "object"} }
func (tool settingsTool) Invoke(context.Context, json.RawMessage) (*core.ToolResult, error) {
	return &core.ToolResult{Tool: tool.Name(), Found: true}, nil
}

func TestManagerRejectsInvalidInputs(t *testing.T) {
	manager := NewManager(storage.NewMemory())
	for _, test := range []struct {
		agent AgentKind
		name  string
		want  error
	}{
		{"detect", "get_incident", ErrInvalidAgent},
		{AgentChat, "unknown", ErrUnknownTool},
		{AgentChat, " get_incident", ErrUnknownTool},
	} {
		if _, err := manager.SetEnabled(tenancy.DefaultOrgScope(), test.agent, test.name, false); !errors.Is(err, test.want) {
			t.Errorf("SetEnabled(%q, %q) error = %v, want %v", test.agent, test.name, err, test.want)
		}
	}
}

func TestManagerPersistsIndependentAgentStateAndIsIdempotent(t *testing.T) {
	provider := storage.NewMemory()
	manager := NewManager(provider)
	scope := tenancy.NewOrgScope("org-a")
	changed, err := manager.SetEnabled(scope, AgentChat, "get_incident", false)
	if err != nil || !changed {
		t.Fatalf("disable = %v, %v", changed, err)
	}
	changed, err = manager.SetEnabled(scope, AgentChat, "get_incident", false)
	if err != nil || changed {
		t.Fatalf("idempotent disable = %v, %v", changed, err)
	}
	chatEnabled, _ := NewManager(provider).Enabled(scope, AgentChat, "get_incident")
	analyzeEnabled, _ := NewManager(provider).Enabled(scope, AgentAnalyze, "get_incident")
	if chatEnabled || !analyzeEnabled {
		t.Fatalf("enabled chat=%v analyze=%v", chatEnabled, analyzeEnabled)
	}
}

func TestManagerConcurrentIndependentUpdatesSurvive(t *testing.T) {
	provider := storage.NewMemory()
	scope := tenancy.NewOrgScope("org-a")
	updates := []struct {
		agent AgentKind
		name  string
	}{{AgentChat, "get_incident"}, {AgentAnalyze, "query_metrics"}, {AgentChat, "list_services"}, {AgentAnalyze, "get_pattern"}}
	var wait sync.WaitGroup
	for _, update := range updates {
		update := update
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := NewManager(provider).SetEnabled(scope, update.agent, update.name, false); err != nil {
				t.Errorf("SetEnabled(%s, %s): %v", update.agent, update.name, err)
			}
		}()
	}
	wait.Wait()
	manager := NewManager(provider)
	for _, update := range updates {
		enabled, err := manager.Enabled(scope, update.agent, update.name)
		if err != nil || enabled {
			t.Errorf("Enabled(%s, %s) = %v, %v", update.agent, update.name, enabled, err)
		}
	}
}

func TestManagerFilterReadsOneGenerationAndRecoversAfterReadError(t *testing.T) {
	inner := storage.NewMemory()
	provider := &settingsReadProvider{Provider: inner}
	manager := NewManager(provider)
	scope := tenancy.NewOrgScope("org-a")
	runtime := []core.Tool{settingsTool("get_incident"), settingsTool("list_services")}

	filtered, err := manager.Filter(scope, AgentChat, runtime, Snapshot{})
	if err != nil || len(filtered) != 2 || provider.reads != 1 {
		t.Fatalf("initial filter = %d tools, %v, reads=%d", len(filtered), err, provider.reads)
	}
	provider.err = errors.New("transient read failure")
	if filtered, err = manager.Filter(scope, AgentChat, runtime, Snapshot{}); err == nil || filtered != nil {
		t.Fatalf("failed filter = %v, %v", filtered, err)
	}
	provider.err = nil
	if filtered, err = manager.Filter(scope, AgentChat, runtime, Snapshot{}); err != nil || len(filtered) != 2 {
		t.Fatalf("recovered filter = %d tools, %v", len(filtered), err)
	}
}

func TestManagerRejectsMalformedSettingsAndCatalogDrift(t *testing.T) {
	provider := storage.NewMemory()
	manager := NewManager(provider)
	scope := tenancy.NewOrgScope("org-a")
	if err := provider.WriteBlob(settingsBlobName(scope), []byte(`{"disabled":`)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Filter(scope, AgentChat, []core.Tool{settingsTool("get_incident")}, Snapshot{}); err == nil {
		t.Fatal("malformed settings did not fail")
	}
	if err := provider.WriteBlob(settingsBlobName(scope), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Filter(scope, AgentChat, []core.Tool{settingsTool("runtime_only")}, Snapshot{}); !errors.Is(err, ErrCatalogDrift) {
		t.Fatalf("catalog drift error = %v", err)
	}
}
