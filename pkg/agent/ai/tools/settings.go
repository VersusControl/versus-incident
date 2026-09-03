package tools

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/VersusControl/versus-incident/pkg/core"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

const maxCASAttempts = 32

var (
	ErrInvalidAgent       = errors.New("tools: invalid agent")
	ErrUnknownTool        = errors.New("tools: unknown tool")
	ErrUnknownToolset     = errors.New("tools: unknown toolset")
	ErrGroupedChild       = errors.New("tools: grouped child must be changed through its toolset")
	ErrConflict           = errors.New("tools: concurrent update conflict")
	ErrCatalogDrift       = errors.New("tools: runtime tool missing from catalog")
	ErrNewerPolicyVersion = errors.New("tools: newer toolset policy version")
)

// AgentKind identifies an independently configurable model tool list.
type AgentKind string

const (
	AgentChat    AgentKind = "chat"
	AgentAnalyze AgentKind = "analyze"
)

type persistedSettings struct {
	Disabled           map[AgentKind]map[string]bool `json:"disabled"`
	ToolsetTransition  *toolsetTransition            `json:"toolset_transition,omitempty"`
	ToolsetTransitions map[string]toolsetTransition  `json:"toolset_transitions,omitempty"`
}

type toolsetTransition struct {
	Agent   AgentKind `json:"agent"`
	Toolset string    `json:"toolset"`
	Enabled bool      `json:"enabled"`
}

func toolsetTransitionKey(agent AgentKind, id string) string {
	return string(agent) + "/" + id
}

func (settings *persistedSettings) transition(agent AgentKind, id string) (toolsetTransition, bool) {
	key := toolsetTransitionKey(agent, id)
	if transition, ok := settings.ToolsetTransitions[key]; ok {
		return transition, true
	}
	if settings.ToolsetTransition != nil && settings.ToolsetTransition.Agent == agent && settings.ToolsetTransition.Toolset == id {
		return *settings.ToolsetTransition, true
	}
	return toolsetTransition{}, false
}

func (settings *persistedSettings) setTransition(transition toolsetTransition) {
	if settings.ToolsetTransitions == nil {
		settings.ToolsetTransitions = make(map[string]toolsetTransition)
	}
	settings.ToolsetTransitions[toolsetTransitionKey(transition.Agent, transition.Toolset)] = transition
	if settings.ToolsetTransition != nil && settings.ToolsetTransition.Agent == transition.Agent && settings.ToolsetTransition.Toolset == transition.Toolset {
		settings.ToolsetTransition = nil
	}
}

func (settings *persistedSettings) clearTransition(agent AgentKind, id string) {
	delete(settings.ToolsetTransitions, toolsetTransitionKey(agent, id))
	if settings.ToolsetTransition != nil && settings.ToolsetTransition.Agent == agent && settings.ToolsetTransition.Toolset == id {
		settings.ToolsetTransition = nil
	}
}

const toolsetPolicyVersion = 2

type persistedToolsetSettings struct {
	Version          int                           `json:"version"`
	DisabledToolsets map[AgentKind]map[string]bool `json:"disabled_toolsets"`
}

// ToolsetSettingsView is one immutable generation of grouped policy combined
// with the compatibility generation of individual policy.
type ToolsetSettingsView struct {
	settings persistedToolsetSettings
	legacy   persistedSettings
	current  []byte
}

// SettingsView is one validated, immutable settings generation.
type SettingsView struct {
	settings persistedSettings
	current  []byte
}

// Manager stores per-agent tool choices through the configured durable CAS
// provider. A nil provider remains readable with default-enabled state.
type Manager struct{ provider storage.Provider }

func NewManager(provider storage.Provider) *Manager { return &Manager{provider: provider} }

func (manager *Manager) Enabled(scope tenancy.OrgScope, agent AgentKind, name string) (bool, error) {
	view, err := manager.Load(scope)
	if err != nil {
		return false, err
	}
	return view.Enabled(agent, name)
}

// ToolsetEnabled returns fail-closed effective policy for one operator card.
func (manager *Manager) ToolsetEnabled(scope tenancy.OrgScope, agent AgentKind, id string) (bool, error) {
	view, err := manager.LoadToolsets(scope)
	if err != nil {
		return false, err
	}
	return view.Enabled(agent, id)
}

// Revision returns a stable hash of the durable settings blob for holder
// rebuild signatures. Missing settings have the empty revision.
func (manager *Manager) Revision(scope tenancy.OrgScope) (string, bool) {
	if manager == nil || manager.provider == nil {
		return "", false
	}
	data, err := manager.provider.ReadBlob(settingsBlobName(scope))
	if err != nil {
		return "", false
	}
	if len(data) == 0 {
		return "absent", true
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum[:]), true
}

// Load reads and validates one settings generation for an operation.
func (manager *Manager) Load(scope tenancy.OrgScope) (SettingsView, error) {
	settings, current, err := manager.load(scope)
	if err != nil {
		return SettingsView{}, err
	}
	return SettingsView{settings: settings, current: current}, nil
}

// LoadToolsets reads grouped policy and the one-release legacy overlay.
func (manager *Manager) LoadToolsets(scope tenancy.OrgScope) (ToolsetSettingsView, error) {
	settings, current, err := manager.loadToolsets(scope)
	if err != nil {
		return ToolsetSettingsView{}, err
	}
	legacy, _, err := manager.load(scope)
	if err != nil {
		return ToolsetSettingsView{}, err
	}
	return ToolsetSettingsView{settings: settings, legacy: legacy, current: current}, nil
}

// Enabled returns operator state from this generation without storage I/O.
func (view SettingsView) Enabled(agent AgentKind, name string) (bool, error) {
	if err := validateAgentTool(agent, name); err != nil {
		return false, err
	}
	return !view.settings.Disabled[agent][name], nil
}

// Enabled resolves a group bit together with all current legacy child bits.
func (view ToolsetSettingsView) Enabled(agent AgentKind, id string) (bool, error) {
	toolset, err := validateAgentToolset(agent, id)
	if err != nil {
		return false, err
	}
	if view.NewerPolicy() {
		return false, nil
	}
	if view.settings.DisabledToolsets[agent][id] {
		return false, nil
	}
	for _, name := range toolset.ToolNames {
		if view.legacy.Disabled[agent][name] {
			return false, nil
		}
	}
	return true, nil
}

// NewerPolicy reports a readable policy generation this binary must not edit.
func (view ToolsetSettingsView) NewerPolicy() bool {
	return view.settings.Version > toolsetPolicyVersion
}

// Revision hashes grouped and compatibility policy as one runtime generation.
func (view ToolsetSettingsView) Revision() string {
	encoded, _ := json.Marshal(struct {
		Toolsets persistedToolsetSettings `json:"toolsets"`
		Legacy   persistedSettings        `json:"legacy"`
	}{view.settings, view.legacy})
	sum := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", sum[:])
}

// Filter applies grouped policy, legacy overlay, and runtime capability gates.
func (view ToolsetSettingsView) Filter(agent AgentKind, runtime []core.Tool, snapshot Snapshot) ([]core.Tool, error) {
	legacy := SettingsView{settings: view.legacy}
	return legacy.filter(agent, runtime, snapshot, &view)
}

// Revision returns the stable digest for this already-loaded generation.
func (view SettingsView) Revision() string {
	if len(view.current) == 0 {
		return "absent"
	}
	sum := sha256.Sum256(view.current)
	return fmt.Sprintf("%x", sum[:])
}

// SetEnabled durably updates one agent/tool resource. Repeating the same value
// is an idempotent success and does not rewrite storage.
func (manager *Manager) SetEnabled(scope tenancy.OrgScope, agent AgentKind, name string, enabled bool) (bool, error) {
	if err := validateAgentTool(agent, name); err != nil {
		return false, err
	}
	if owner, ok := toolsetByChild(name); ok && owner.Section != SectionCommon {
		return false, fmt.Errorf("%w: %s", ErrGroupedChild, owner.ID)
	}
	return manager.setLegacyEnabled(scope, agent, name, enabled)
}

func (manager *Manager) setLegacyEnabled(scope tenancy.OrgScope, agent AgentKind, name string, enabled bool) (bool, error) {
	if manager == nil || manager.provider == nil {
		return false, storage.ErrUnsupported
	}
	cas, ok := manager.provider.(storage.BlobCAS)
	if !ok {
		return false, storage.ErrUnsupported
	}
	for attempt := 0; attempt < maxCASAttempts; attempt++ {
		view, err := manager.Load(scope)
		if err != nil {
			return false, err
		}
		settings, current := view.settings, view.current
		wasEnabled := !settings.Disabled[agent][name]
		if wasEnabled == enabled {
			return false, nil
		}
		settings.Disabled[agent][name] = !enabled
		replacement, err := json.Marshal(settings)
		if err != nil {
			return false, err
		}
		swapped, err := cas.CompareAndSwapBlob(settingsBlobName(scope), current, replacement)
		if err != nil {
			return false, err
		}
		if swapped {
			return true, nil
		}
	}
	return false, ErrConflict
}

// SetToolsetEnabled changes one group bit through a resumable mixed-version
// transition. Legacy child denies mask every intermediate grouped state.
func (manager *Manager) SetToolsetEnabled(scope tenancy.OrgScope, agent AgentKind, id string, enabled bool) (bool, error) {
	toolset, err := validateAgentToolset(agent, id)
	if err != nil {
		return false, err
	}
	if manager == nil || manager.provider == nil {
		return false, storage.ErrUnsupported
	}
	cas, ok := manager.provider.(storage.BlobCAS)
	if !ok {
		return false, storage.ErrUnsupported
	}
	initialSettings, initialCurrent, err := manager.loadToolsets(scope)
	if err != nil {
		return false, err
	}
	if initialSettings.Version > toolsetPolicyVersion {
		return false, ErrNewerPolicyVersion
	}
	legacy, _, err := manager.load(scope)
	if err != nil {
		return false, err
	}
	wasEnabled := !initialSettings.DisabledToolsets[agent][id]
	if len(initialCurrent) == 0 {
		wasEnabled = !migrateToolsetSettings(legacy).DisabledToolsets[agent][id]
	}
	legacyMatches := true
	for _, name := range toolset.ToolNames {
		if legacy.Disabled[agent][name] == enabled {
			legacyMatches = false
			break
		}
	}
	_, pending := legacy.transition(agent, id)
	if wasEnabled == enabled && legacyMatches && !pending {
		return false, nil
	}
	transitionLegacy, transitionChanged, err := manager.beginToolsetTransition(scope, agent, id, toolset.ToolNames, enabled)
	if err != nil {
		return false, err
	}

	for attempt := 0; attempt < maxCASAttempts; attempt++ {
		settings, current := initialSettings, initialCurrent
		if attempt > 0 {
			settings, current, err = manager.loadToolsets(scope)
			if err != nil {
				return false, err
			}
		}
		if settings.Version > toolsetPolicyVersion {
			return false, ErrNewerPolicyVersion
		}
		if len(current) == 0 {
			settings = migrateToolsetSettings(transitionLegacy)
		}
		groupWasEnabled := !settings.DisabledToolsets[agent][id]
		if groupWasEnabled == enabled && len(current) != 0 {
			legacyChanged, finishErr := manager.finishToolsetTransition(scope, agent, id, toolset.ToolNames, enabled)
			return transitionChanged || legacyChanged, finishErr
		}
		settings.DisabledToolsets[agent][id] = !enabled
		replacement, err := json.Marshal(settings)
		if err != nil {
			return false, err
		}
		swapped, err := cas.CompareAndSwapBlob(toolsetSettingsBlobName(scope), current, replacement)
		if err != nil {
			return false, err
		}
		if swapped {
			legacyChanged, finishErr := manager.finishToolsetTransition(scope, agent, id, toolset.ToolNames, enabled)
			return groupWasEnabled != enabled || transitionChanged || legacyChanged, finishErr
		}
	}
	return false, ErrConflict
}

func (manager *Manager) beginToolsetTransition(scope tenancy.OrgScope, agent AgentKind, id string, names []string, enabled bool) (persistedSettings, bool, error) {
	cas := manager.provider.(storage.BlobCAS)
	for attempt := 0; attempt < maxCASAttempts; attempt++ {
		view, err := manager.Load(scope)
		if err != nil {
			return persistedSettings{}, false, err
		}
		settings, current := view.settings, view.current
		if transition, ok := settings.transition(agent, id); ok {
			if transition.Enabled == enabled {
				return settings, false, nil
			}
		}
		settings.setTransition(toolsetTransition{Agent: agent, Toolset: id, Enabled: enabled})
		if !enabled {
			for _, name := range names {
				if !settings.Disabled[agent][name] {
					settings.Disabled[agent][name] = true
				}
			}
		}
		replacement, err := json.Marshal(settings)
		if err != nil {
			return persistedSettings{}, false, err
		}
		swapped, err := cas.CompareAndSwapBlob(settingsBlobName(scope), current, replacement)
		if err != nil {
			return persistedSettings{}, false, err
		}
		if swapped {
			return settings, true, nil
		}
	}
	return persistedSettings{}, false, ErrConflict
}

func (manager *Manager) finishToolsetTransition(scope tenancy.OrgScope, agent AgentKind, id string, names []string, enabled bool) (bool, error) {
	cas := manager.provider.(storage.BlobCAS)
	for attempt := 0; attempt < maxCASAttempts; attempt++ {
		view, err := manager.Load(scope)
		if err != nil {
			return false, err
		}
		settings, current := view.settings, view.current
		transition, ok := settings.transition(agent, id)
		if !ok {
			return false, nil
		}
		if transition.Enabled != enabled {
			return false, ErrConflict
		}
		changed := false
		if enabled {
			for _, name := range names {
				if settings.Disabled[agent][name] {
					delete(settings.Disabled[agent], name)
					changed = true
				}
			}
		}
		settings.clearTransition(agent, id)
		replacement, err := json.Marshal(settings)
		if err != nil {
			return false, err
		}
		swapped, err := cas.CompareAndSwapBlob(settingsBlobName(scope), current, replacement)
		if err != nil {
			return false, err
		}
		if swapped {
			return changed, nil
		}
	}
	return false, ErrConflict
}

// Filter removes unavailable and operator-disabled tools for one agent without
// changing the order supplied by runtime composition.
func (manager *Manager) Filter(scope tenancy.OrgScope, agent AgentKind, runtime []core.Tool, snapshot Snapshot) ([]core.Tool, error) {
	view, err := manager.Load(scope)
	if err != nil {
		return nil, err
	}
	settings, current, err := manager.loadToolsets(scope)
	if err != nil {
		return nil, err
	}
	toolsets := ToolsetSettingsView{settings: settings, legacy: view.settings, current: current}
	return view.filter(agent, runtime, snapshot, &toolsets)
}

// Filter resolves a runtime tool list from this settings generation.
func (view SettingsView) Filter(agent AgentKind, runtime []core.Tool, snapshot Snapshot) ([]core.Tool, error) {
	return view.filter(agent, runtime, snapshot, nil)
}

func (view SettingsView) filter(agent AgentKind, runtime []core.Tool, snapshot Snapshot, toolsets *ToolsetSettingsView) ([]core.Tool, error) {
	filtered := make([]core.Tool, 0, len(runtime))
	metadata := metadataByName()
	for _, tool := range runtime {
		if tool == nil {
			return nil, fmt.Errorf("%w: nil tool", ErrCatalogDrift)
		}
		entry, ok := metadata[tool.Name()]
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrCatalogDrift, tool.Name())
		}
		enabled, err := view.Enabled(agent, tool.Name())
		if err != nil {
			return nil, err
		}
		if owner, grouped := toolsetByChild(tool.Name()); grouped && toolsets != nil {
			enabled, err = toolsets.Enabled(agent, owner.ID)
			if err != nil {
				return nil, err
			}
		}
		if Resolve(entry.Requirement, snapshot, enabled).State != StateAvailable {
			continue
		}
		filtered = append(filtered, tool)
	}
	return filtered, nil
}

func (manager *Manager) loadToolsets(scope tenancy.OrgScope) (persistedToolsetSettings, []byte, error) {
	settings := emptyToolsetSettings()
	if manager == nil || manager.provider == nil {
		return settings, nil, nil
	}
	data, err := manager.provider.ReadBlob(toolsetSettingsBlobName(scope))
	if err != nil || len(data) == 0 {
		return settings, data, err
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return persistedToolsetSettings{}, data, fmt.Errorf("tools: decode toolset settings: %w", err)
	}
	if settings.Version > toolsetPolicyVersion {
		normalizeToolsetSettings(&settings)
		return settings, data, nil
	}
	if settings.Version != toolsetPolicyVersion {
		return persistedToolsetSettings{}, data, fmt.Errorf("tools: unsupported toolset policy version %d", settings.Version)
	}
	normalizeToolsetSettings(&settings)
	return settings, data, nil
}

func emptyToolsetSettings() persistedToolsetSettings {
	return persistedToolsetSettings{Version: toolsetPolicyVersion, DisabledToolsets: map[AgentKind]map[string]bool{AgentChat: {}, AgentAnalyze: {}}}
}

func normalizeToolsetSettings(settings *persistedToolsetSettings) {
	if settings.DisabledToolsets == nil {
		settings.DisabledToolsets = make(map[AgentKind]map[string]bool)
	}
	for _, agent := range []AgentKind{AgentChat, AgentAnalyze} {
		if settings.DisabledToolsets[agent] == nil {
			settings.DisabledToolsets[agent] = make(map[string]bool)
		}
	}
}

func migrateToolsetSettings(legacy persistedSettings) persistedToolsetSettings {
	settings := emptyToolsetSettings()
	for _, agent := range []AgentKind{AgentChat, AgentAnalyze} {
		for _, toolset := range toolsets {
			for _, name := range toolset.ToolNames {
				if legacy.Disabled[agent][name] {
					settings.DisabledToolsets[agent][toolset.ID] = true
					break
				}
			}
		}
	}
	return settings
}

func (manager *Manager) load(scope tenancy.OrgScope) (persistedSettings, []byte, error) {
	settings := persistedSettings{Disabled: map[AgentKind]map[string]bool{AgentChat: {}, AgentAnalyze: {}}}
	if manager == nil || manager.provider == nil {
		return settings, nil, nil
	}
	data, err := manager.provider.ReadBlob(settingsBlobName(scope))
	if err != nil || len(data) == 0 {
		return settings, data, err
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return persistedSettings{}, data, fmt.Errorf("tools: decode settings: %w", err)
	}
	if settings.Disabled == nil {
		settings.Disabled = make(map[AgentKind]map[string]bool)
	}
	for _, agent := range []AgentKind{AgentChat, AgentAnalyze} {
		if settings.Disabled[agent] == nil {
			settings.Disabled[agent] = make(map[string]bool)
		}
	}
	return settings, data, nil
}

func settingsBlobName(scope tenancy.OrgScope) string {
	return "agent-tools/" + tenancy.NormalizeOrgID(scope.Normalized().Write) + "/settings.json"
}

func toolsetSettingsBlobName(scope tenancy.OrgScope) string {
	return "agent-tools/" + tenancy.NormalizeOrgID(scope.Normalized().Write) + "/toolsets-v2.json"
}

func validateAgentTool(agent AgentKind, name string) error {
	if agent != AgentChat && agent != AgentAnalyze {
		return ErrInvalidAgent
	}
	if strings.TrimSpace(name) != name || len(name) == 0 || len(name) > 80 {
		return ErrUnknownTool
	}
	if _, ok := metadataByName()[name]; !ok {
		return ErrUnknownTool
	}
	return nil
}

func validateAgentToolset(agent AgentKind, id string) (ToolsetMetadata, error) {
	if agent != AgentChat && agent != AgentAnalyze {
		return ToolsetMetadata{}, ErrInvalidAgent
	}
	if strings.TrimSpace(id) != id || len(id) == 0 || len(id) > 80 {
		return ToolsetMetadata{}, ErrUnknownToolset
	}
	for _, toolset := range toolsets {
		if toolset.ID == id {
			return toolset, nil
		}
	}
	return ToolsetMetadata{}, ErrUnknownToolset
}

func toolsetByChild(name string) (ToolsetMetadata, bool) {
	for _, toolset := range toolsets {
		for _, child := range toolset.ToolNames {
			if child == name {
				return toolset, true
			}
		}
	}
	return ToolsetMetadata{}, false
}

func metadataByName() map[string]Metadata {
	entries := make(map[string]Metadata, len(catalog))
	for _, entry := range catalog {
		entries[entry.Name] = entry
	}
	return entries
}
