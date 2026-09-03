package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
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

type settingsCountingProvider struct {
	storage.Provider
	reads map[string]int
	cas   map[string]int
}

type legacyCASFailureProvider struct {
	storage.Provider
	failLegacy bool
}

func (provider *legacyCASFailureProvider) CompareAndSwapBlob(name string, expected, replacement []byte) (bool, error) {
	if provider.failLegacy && strings.Contains(name, "agent-tool-settings") && !strings.Contains(name, "toolsets") {
		return false, errors.New("injected legacy cleanup failure")
	}
	return provider.Provider.(storage.BlobCAS).CompareAndSwapBlob(name, expected, replacement)
}

func (provider *settingsCountingProvider) ReadBlob(name string) ([]byte, error) {
	provider.reads[name]++
	return provider.Provider.ReadBlob(name)
}

func (provider *settingsCountingProvider) CompareAndSwapBlob(name string, expected, replacement []byte) (bool, error) {
	provider.cas[name]++
	return provider.Provider.(storage.BlobCAS).CompareAndSwapBlob(name, expected, replacement)
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
	}{{AgentChat, "get_incident"}, {AgentAnalyze, "get_service"}, {AgentChat, "list_services"}, {AgentAnalyze, "get_pattern"}}
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
	if err != nil || len(filtered) != 2 || provider.reads != 2 {
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

func TestToolsetPolicyMigratesEveryGroupAndPersistsLegacyDenies(t *testing.T) {
	provider := storage.NewMemory()
	manager := NewManager(provider)
	scope := tenancy.NewOrgScope("org-a")
	legacy := []byte(`{"disabled":{"chat":{"query_metrics":true,"get_related_logs":true},"analyze":{"recent_changes":true}}}`)
	if err := provider.WriteBlob(settingsBlobName(scope), legacy); err != nil {
		t.Fatal(err)
	}
	changed, err := manager.SetToolsetEnabled(scope, AgentChat, "kubernetes", false)
	if err != nil || !changed {
		t.Fatalf("disable Kubernetes = %v, %v", changed, err)
	}
	storedLegacy, _, err := manager.load(scope)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range toolsets[0].ToolNames {
		if !storedLegacy.Disabled[AgentChat][name] {
			t.Fatalf("legacy child %q is not denied for old replicas", name)
		}
	}
	data, _ := provider.ReadBlob(toolsetSettingsBlobName(scope))
	var settings persistedToolsetSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	if !settings.DisabledToolsets[AgentChat]["kubernetes"] || !settings.DisabledToolsets[AgentChat]["metrics"] || !settings.DisabledToolsets[AgentChat]["logs"] || !settings.DisabledToolsets[AgentAnalyze]["source-control"] {
		t.Fatalf("whole migration did not preserve disabled groups: %#v", settings.DisabledToolsets)
	}
}

func TestToolsetPolicyOverlayAndEnableAreFailClosedAndAgentIndependent(t *testing.T) {
	provider := storage.NewMemory()
	manager := NewManager(provider)
	scope := tenancy.NewOrgScope("org-a")
	if err := provider.WriteBlob(settingsBlobName(scope), []byte(`{"disabled":{"chat":{"query_metrics":true},"analyze":{}}}`)); err != nil {
		t.Fatal(err)
	}
	if enabled, err := manager.ToolsetEnabled(scope, AgentChat, "metrics"); err != nil || enabled {
		t.Fatalf("legacy overlay enabled=%v err=%v", enabled, err)
	}
	if enabled, err := manager.ToolsetEnabled(scope, AgentAnalyze, "metrics"); err != nil || !enabled {
		t.Fatalf("analyze enabled=%v err=%v", enabled, err)
	}
	if changed, err := manager.SetToolsetEnabled(scope, AgentChat, "metrics", true); err != nil || !changed {
		t.Fatalf("enable legacy-only disabled toolset = %v, %v", changed, err)
	}
	legacy, _, err := manager.load(scope)
	if err != nil || legacy.Disabled[AgentChat]["query_metrics"] {
		t.Fatalf("legacy child was not cleared before enable: %#v, %v", legacy.Disabled, err)
	}
	if enabled, err := manager.ToolsetEnabled(scope, AgentChat, "metrics"); err != nil || !enabled {
		t.Fatalf("chat enabled=%v err=%v", enabled, err)
	}
}

func TestToolsetDisableAndIdempotentRetrySucceedWhenLegacyCleanupFails(t *testing.T) {
	inner := storage.NewMemory()
	provider := &legacyCASFailureProvider{Provider: inner}
	manager := NewManager(provider)
	scope := tenancy.NewOrgScope("org-a")
	grouped, _ := json.Marshal(emptyToolsetSettings())
	if err := inner.WriteBlob(toolsetSettingsBlobName(scope), grouped); err != nil {
		t.Fatal(err)
	}
	if err := inner.WriteBlob(settingsBlobName(scope), []byte(`{"disabled":{"chat":{"get_cluster_overview":true},"analyze":{}}}`)); err != nil {
		t.Fatal(err)
	}
	provider.failLegacy = true
	changed, err := manager.SetToolsetEnabled(scope, AgentChat, "kubernetes", false)
	if err != nil || !changed {
		t.Fatalf("disable after committed group CAS = %v, %v", changed, err)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		view, err := manager.LoadToolsets(scope)
		if err != nil {
			t.Fatal(err)
		}
		enabled, err := view.Enabled(AgentChat, "kubernetes")
		if err != nil || enabled {
			t.Fatalf("attempt %d mixed-version state enabled=%v err=%v", attempt, enabled, err)
		}
		if attempt == 1 {
			changed, err = manager.SetToolsetEnabled(scope, AgentChat, "kubernetes", false)
			if err != nil || changed {
				t.Fatalf("idempotent disable after cleanup failure = %v, %v", changed, err)
			}
		}
	}
}

func TestToolsetPolicyFiltersCommonChildrenIndependently(t *testing.T) {
	provider := storage.NewMemory()
	manager := NewManager(provider)
	scope := tenancy.NewOrgScope("org-a")
	runtime := []core.Tool{settingsTool("describe_dependencies")}
	snapshot := Snapshot{Capabilities: map[string]DependencyStatus{"dependency_graph": {Configured: true, Healthy: true}}}

	if _, err := manager.SetToolsetEnabled(scope, AgentChat, "describe_dependencies", false); err != nil {
		t.Fatal(err)
	}
	chat, err := manager.Filter(scope, AgentChat, runtime, snapshot)
	if err != nil || len(chat) != 0 {
		t.Fatalf("disabled chat common filter = %v, %v", chat, err)
	}
	analyze, err := manager.Filter(scope, AgentAnalyze, runtime, snapshot)
	if err != nil || len(analyze) != 1 {
		t.Fatalf("independent analyze common filter = %v, %v", analyze, err)
	}
	if _, err := manager.SetToolsetEnabled(scope, AgentChat, "describe_dependencies", true); err != nil {
		t.Fatal(err)
	}
	chat, err = manager.Filter(scope, AgentChat, runtime, snapshot)
	if err != nil || len(chat) != 1 {
		t.Fatalf("re-enabled chat common filter = %v, %v", chat, err)
	}
}

func TestToolsetPolicySteadyStateUsesOneReadAndCASAndRejectsNewerVersion(t *testing.T) {
	inner := storage.NewMemory()
	provider := &settingsCountingProvider{Provider: inner, reads: make(map[string]int), cas: make(map[string]int)}
	manager := NewManager(provider)
	scope := tenancy.NewOrgScope("org-a")
	initial, _ := json.Marshal(emptyToolsetSettings())
	if err := inner.WriteBlob(toolsetSettingsBlobName(scope), initial); err != nil {
		t.Fatal(err)
	}
	if changed, err := manager.SetToolsetEnabled(scope, AgentChat, "logs", false); err != nil || !changed {
		t.Fatalf("disable logs = %v, %v", changed, err)
	}
	name := toolsetSettingsBlobName(scope)
	if provider.reads[name] != 1 || provider.cas[name] != 1 {
		t.Fatalf("toolset mutation reads=%d cas=%d, want 1/1", provider.reads[name], provider.cas[name])
	}
	if err := inner.WriteBlob(name, []byte(`{"version":3,"disabled_toolsets":{}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetToolsetEnabled(scope, AgentChat, "logs", true); !errors.Is(err, ErrNewerPolicyVersion) {
		t.Fatalf("newer version error = %v", err)
	}
}

func TestToolsetEnableClearsEveryLegacyChildWithOneLegacyCAS(t *testing.T) {
	inner := storage.NewMemory()
	provider := &settingsCountingProvider{Provider: inner, reads: make(map[string]int), cas: make(map[string]int)}
	manager := NewManager(provider)
	scope := tenancy.NewOrgScope("org-a")
	legacy := persistedSettings{Disabled: map[AgentKind]map[string]bool{AgentChat: {"query_metrics": true}, AgentAnalyze: {}}}
	for _, name := range toolsets[0].ToolNames {
		legacy.Disabled[AgentChat][name] = true
	}
	legacyData, _ := json.Marshal(legacy)
	if err := inner.WriteBlob(settingsBlobName(scope), legacyData); err != nil {
		t.Fatal(err)
	}
	grouped := emptyToolsetSettings()
	grouped.DisabledToolsets[AgentChat]["kubernetes"] = true
	groupedData, _ := json.Marshal(grouped)
	if err := inner.WriteBlob(toolsetSettingsBlobName(scope), groupedData); err != nil {
		t.Fatal(err)
	}
	if changed, err := manager.SetToolsetEnabled(scope, AgentChat, "kubernetes", true); err != nil || !changed {
		t.Fatalf("enable Kubernetes = %v, %v", changed, err)
	}
	legacyName, groupedName := settingsBlobName(scope), toolsetSettingsBlobName(scope)
	if provider.reads[legacyName] != 3 || provider.cas[legacyName] != 2 || provider.reads[groupedName] != 1 || provider.cas[groupedName] != 1 {
		t.Fatalf("I/O legacy=%d/%d grouped=%d/%d, want transition 3/2 and group 1/1", provider.reads[legacyName], provider.cas[legacyName], provider.reads[groupedName], provider.cas[groupedName])
	}
	stored, _, err := manager.load(scope)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range toolsets[0].ToolNames {
		if stored.Disabled[AgentChat][name] {
			t.Fatalf("legacy child %q remained disabled", name)
		}
	}
	if !stored.Disabled[AgentChat]["query_metrics"] {
		t.Fatal("unrelated legacy disable was cleared")
	}
}

func TestToolsetDisableWritesLegacyDeniesAroundOneGroupedCAS(t *testing.T) {
	inner := storage.NewMemory()
	provider := &settingsCountingProvider{Provider: inner, reads: make(map[string]int), cas: make(map[string]int)}
	manager := NewManager(provider)
	scope := tenancy.NewOrgScope("org-a")
	legacy := []byte(`{"disabled":{"chat":{"get_cluster_overview":true},"analyze":{}}}`)
	if err := inner.WriteBlob(settingsBlobName(scope), legacy); err != nil {
		t.Fatal(err)
	}
	grouped, _ := json.Marshal(emptyToolsetSettings())
	if err := inner.WriteBlob(toolsetSettingsBlobName(scope), grouped); err != nil {
		t.Fatal(err)
	}
	if changed, err := manager.SetToolsetEnabled(scope, AgentChat, "kubernetes", false); err != nil || !changed {
		t.Fatalf("disable Kubernetes = %v, %v", changed, err)
	}
	legacyName, groupedName := settingsBlobName(scope), toolsetSettingsBlobName(scope)
	if provider.reads[groupedName] != 1 || provider.cas[groupedName] != 1 || provider.reads[legacyName] != 3 || provider.cas[legacyName] != 2 {
		t.Fatalf("I/O grouped=%d/%d legacy=%d/%d, want group 1/1 and transition 3/2", provider.reads[groupedName], provider.cas[groupedName], provider.reads[legacyName], provider.cas[legacyName])
	}
	stored, _, err := manager.load(scope)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range toolsets[0].ToolNames {
		if !stored.Disabled[AgentChat][name] {
			t.Fatalf("legacy child %q is not denied after disable", name)
		}
	}
	if stored.ToolsetTransition != nil {
		t.Fatalf("completed transition marker = %#v", stored.ToolsetTransition)
	}
}

func TestToolsetTransitionCrashStatesNeverFailOpen(t *testing.T) {
	toolset, err := validateAgentToolset(AgentChat, "kubernetes")
	if err != nil {
		t.Fatal(err)
	}
	legacyEnabled := persistedSettings{Disabled: map[AgentKind]map[string]bool{AgentChat: {}, AgentAnalyze: {}}}
	legacyDenied := persistedSettings{Disabled: map[AgentKind]map[string]bool{AgentChat: {}, AgentAnalyze: {}}}
	for _, name := range toolset.ToolNames {
		legacyDenied.Disabled[AgentChat][name] = true
	}
	disabling := legacyDenied
	disabling.ToolsetTransition = &toolsetTransition{Agent: AgentChat, Toolset: "kubernetes", Enabled: false}
	enabling := legacyDenied
	enabling.ToolsetTransition = &toolsetTransition{Agent: AgentChat, Toolset: "kubernetes", Enabled: true}
	groupEnabled := emptyToolsetSettings()
	groupDisabled := emptyToolsetSettings()
	groupDisabled.DisabledToolsets[AgentChat]["kubernetes"] = true
	for _, state := range []struct {
		name       string
		legacy     persistedSettings
		grouped    persistedToolsetSettings
		wantEnable bool
	}{
		{"initial enabled", legacyEnabled, groupEnabled, true},
		{"disable legacy prepared", disabling, groupEnabled, false},
		{"disable grouped committed", disabling, groupDisabled, false},
		{"disable complete", legacyDenied, groupDisabled, false},
		{"enable legacy prepared", enabling, groupDisabled, false},
		{"enable grouped committed", enabling, groupEnabled, false},
		{"enable complete", legacyEnabled, groupEnabled, true},
	} {
		t.Run(state.name, func(t *testing.T) {
			oldEnabled := true
			for _, name := range toolset.ToolNames {
				oldEnabled = oldEnabled && !state.legacy.Disabled[AgentChat][name]
			}
			view := ToolsetSettingsView{settings: state.grouped, legacy: state.legacy}
			newEnabled, err := view.Enabled(AgentChat, "kubernetes")
			if err != nil || oldEnabled != state.wantEnable || newEnabled != state.wantEnable {
				t.Fatalf("old=%v new=%v want=%v err=%v", oldEnabled, newEnabled, state.wantEnable, err)
			}
		})
	}
}

func TestToolsetTransitionSupersedesOppositeIntent(t *testing.T) {
	provider := storage.NewMemory()
	manager := NewManager(provider)
	scope := tenancy.NewOrgScope("org-a")
	legacy := persistedSettings{Disabled: map[AgentKind]map[string]bool{AgentChat: {}, AgentAnalyze: {}}}
	legacy.ToolsetTransition = &toolsetTransition{Agent: AgentChat, Toolset: "kubernetes", Enabled: false}
	for _, name := range toolsets[0].ToolNames {
		legacy.Disabled[AgentChat][name] = true
	}
	data, _ := json.Marshal(legacy)
	if err := provider.WriteBlob(settingsBlobName(scope), data); err != nil {
		t.Fatal(err)
	}
	if changed, err := manager.SetToolsetEnabled(scope, AgentChat, "kubernetes", true); err != nil || !changed {
		t.Fatalf("superseding enable = %v, %v", changed, err)
	}
	for _, name := range toolsets[0].ToolNames {
		enabled, err := manager.Enabled(scope, AgentChat, name)
		if err != nil || !enabled {
			t.Fatalf("legacy child %q enabled=%v err=%v", name, enabled, err)
		}
	}
}

func TestToolsetTransitionConcurrentOppositeIntentsRemainConsistent(t *testing.T) {
	provider := storage.NewMemory()
	scope := tenancy.NewOrgScope("org-a")
	start := make(chan struct{})
	errorsSeen := make(chan error, 2)
	for _, enabled := range []bool{false, true} {
		enabled := enabled
		go func() {
			<-start
			_, err := NewManager(provider).SetToolsetEnabled(scope, AgentChat, "kubernetes", enabled)
			errorsSeen <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-errorsSeen; err != nil && !errors.Is(err, ErrConflict) {
			t.Fatalf("concurrent mutation error = %v", err)
		}
	}
	manager := NewManager(provider)
	view, err := manager.LoadToolsets(scope)
	if err != nil {
		t.Fatal(err)
	}
	enabled, err := view.Enabled(AgentChat, "kubernetes")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range toolsets[0].ToolNames {
		legacyEnabled, err := manager.Enabled(scope, AgentChat, name)
		if err != nil || legacyEnabled != enabled {
			t.Fatalf("legacy child %q enabled=%v grouped=%v err=%v", name, legacyEnabled, enabled, err)
		}
	}
	stored, _, err := manager.load(scope)
	if err != nil {
		t.Fatal(err)
	}
	if _, pending := stored.transition(AgentChat, "kubernetes"); pending {
		t.Fatal("concurrent completion left a pending transition")
	}
}

func TestToolsetTransitionDoesNotBlockUnrelatedToolset(t *testing.T) {
	provider := storage.NewMemory()
	manager := NewManager(provider)
	scope := tenancy.NewOrgScope("org-a")
	legacy := persistedSettings{Disabled: map[AgentKind]map[string]bool{AgentChat: {}, AgentAnalyze: {}}}
	legacy.setTransition(toolsetTransition{Agent: AgentChat, Toolset: "kubernetes", Enabled: false})
	for _, name := range toolsets[0].ToolNames {
		legacy.Disabled[AgentChat][name] = true
	}
	data, _ := json.Marshal(legacy)
	if err := provider.WriteBlob(settingsBlobName(scope), data); err != nil {
		t.Fatal(err)
	}
	if changed, err := manager.SetToolsetEnabled(scope, AgentChat, "logs", false); err != nil || !changed {
		t.Fatalf("disable unrelated logs = %v, %v", changed, err)
	}
	stored, _, err := manager.load(scope)
	if err != nil {
		t.Fatal(err)
	}
	if transition, ok := stored.transition(AgentChat, "kubernetes"); !ok || transition.Enabled {
		t.Fatalf("stranded Kubernetes transition = %#v, %v", transition, ok)
	}
}

func TestToolsetTransitionRestartsResumeOrSupersedeEveryCrashState(t *testing.T) {
	toolset, err := validateAgentToolset(AgentChat, "kubernetes")
	if err != nil {
		t.Fatal(err)
	}
	denied := func(enabledIntent bool) persistedSettings {
		settings := persistedSettings{Disabled: map[AgentKind]map[string]bool{AgentChat: {}, AgentAnalyze: {}}}
		for _, name := range toolset.ToolNames {
			settings.Disabled[AgentChat][name] = true
		}
		settings.setTransition(toolsetTransition{Agent: AgentChat, Toolset: "kubernetes", Enabled: enabledIntent})
		return settings
	}
	groupEnabled := emptyToolsetSettings()
	groupDisabled := emptyToolsetSettings()
	groupDisabled.DisabledToolsets[AgentChat]["kubernetes"] = true
	for _, crash := range []struct {
		name    string
		legacy  persistedSettings
		grouped persistedToolsetSettings
		intent  bool
	}{
		{"disable after begin", denied(false), groupEnabled, false},
		{"disable after grouped commit", denied(false), groupDisabled, false},
		{"enable after begin", denied(true), groupDisabled, true},
		{"enable after grouped commit", denied(true), groupEnabled, true},
	} {
		for _, sameIntent := range []bool{true, false} {
			name := crash.name + "/supersede"
			requested := !crash.intent
			if sameIntent {
				name = crash.name + "/resume"
				requested = crash.intent
			}
			t.Run(name, func(t *testing.T) {
				provider := storage.NewMemory()
				scope := tenancy.NewOrgScope("org-a")
				legacyData, _ := json.Marshal(crash.legacy)
				groupedData, _ := json.Marshal(crash.grouped)
				if err := provider.WriteBlob(settingsBlobName(scope), legacyData); err != nil {
					t.Fatal(err)
				}
				if err := provider.WriteBlob(toolsetSettingsBlobName(scope), groupedData); err != nil {
					t.Fatal(err)
				}
				manager := NewManager(provider)
				if _, err := manager.SetToolsetEnabled(scope, AgentChat, "kubernetes", requested); err != nil {
					t.Fatalf("restart mutation: %v", err)
				}
				enabled, err := manager.ToolsetEnabled(scope, AgentChat, "kubernetes")
				if err != nil || enabled != requested {
					t.Fatalf("enabled=%v want=%v err=%v", enabled, requested, err)
				}
				stored, _, err := manager.load(scope)
				if err != nil {
					t.Fatal(err)
				}
				if _, pending := stored.transition(AgentChat, "kubernetes"); pending {
					t.Fatal("completed restart left a pending transition")
				}
			})
		}
	}
}

func TestNewerToolsetPolicyIsReadableFailClosedAndWriteRejected(t *testing.T) {
	provider := storage.NewMemory()
	manager := NewManager(provider)
	scope := tenancy.NewOrgScope("org-a")
	if err := provider.WriteBlob(toolsetSettingsBlobName(scope), []byte(`{"version":3,"disabled_toolsets":{},"future_field":{"kept":true}}`)); err != nil {
		t.Fatal(err)
	}
	view, err := manager.LoadToolsets(scope)
	if err != nil || !view.NewerPolicy() {
		t.Fatalf("LoadToolsets newer = newer:%v err:%v", view.NewerPolicy(), err)
	}
	if enabled, err := view.Enabled(AgentChat, "kubernetes"); err != nil || enabled {
		t.Fatalf("newer Kubernetes enabled=%v err=%v", enabled, err)
	}
	filtered, err := view.Filter(AgentChat, []core.Tool{settingsTool("describe_dependencies")}, Snapshot{Capabilities: map[string]DependencyStatus{"dependency_graph": {Configured: true, Healthy: true}}})
	if err != nil || len(filtered) != 0 {
		t.Fatalf("newer policy exposed Common runtime child: %v, %v", filtered, err)
	}
	if _, err := manager.SetToolsetEnabled(scope, AgentChat, "kubernetes", false); !errors.Is(err, ErrNewerPolicyVersion) {
		t.Fatalf("newer write error = %v", err)
	}
	groupedBefore, err := provider.ReadBlob(toolsetSettingsBlobName(scope))
	if err != nil {
		t.Fatal(err)
	}
	legacyBefore := []byte(`{"disabled":{"chat":{"get_cluster_overview":true},"analyze":{}}}`)
	if err := provider.WriteBlob(settingsBlobName(scope), legacyBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetToolsetEnabled(scope, AgentChat, "kubernetes", true); !errors.Is(err, ErrNewerPolicyVersion) {
		t.Fatalf("newer enable error = %v", err)
	}
	groupedAfter, err := provider.ReadBlob(toolsetSettingsBlobName(scope))
	if err != nil || !bytes.Equal(groupedAfter, groupedBefore) {
		t.Fatalf("newer grouped blob changed: %s, %v", groupedAfter, err)
	}
	legacyAfter, err := provider.ReadBlob(settingsBlobName(scope))
	if err != nil || !bytes.Equal(legacyAfter, legacyBefore) {
		t.Fatalf("newer legacy blob changed: %s, %v", legacyAfter, err)
	}
}

func TestGroupedChildIndividualMutationIsRejected(t *testing.T) {
	manager := NewManager(storage.NewMemory())
	if _, err := manager.SetEnabled(tenancy.DefaultOrgScope(), AgentChat, "get_cluster_overview", false); !errors.Is(err, ErrGroupedChild) || !strings.Contains(err.Error(), "kubernetes") {
		t.Fatalf("grouped child error = %v", err)
	}
}
