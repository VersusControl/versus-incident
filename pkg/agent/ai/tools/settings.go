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
	ErrInvalidAgent = errors.New("tools: invalid agent")
	ErrUnknownTool  = errors.New("tools: unknown tool")
	ErrConflict     = errors.New("tools: concurrent update conflict")
	ErrCatalogDrift = errors.New("tools: runtime tool missing from catalog")
)

// AgentKind identifies an independently configurable model tool list.
type AgentKind string

const (
	AgentChat    AgentKind = "chat"
	AgentAnalyze AgentKind = "analyze"
)

type persistedSettings struct {
	Disabled map[AgentKind]map[string]bool `json:"disabled"`
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

// Enabled returns operator state from this generation without storage I/O.
func (view SettingsView) Enabled(agent AgentKind, name string) (bool, error) {
	if err := validateAgentTool(agent, name); err != nil {
		return false, err
	}
	return !view.settings.Disabled[agent][name], nil
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

// Filter removes unavailable and operator-disabled tools for one agent without
// changing the order supplied by runtime composition.
func (manager *Manager) Filter(scope tenancy.OrgScope, agent AgentKind, runtime []core.Tool, snapshot Snapshot) ([]core.Tool, error) {
	view, err := manager.Load(scope)
	if err != nil {
		return nil, err
	}
	return view.Filter(agent, runtime, snapshot)
}

// Filter resolves a runtime tool list from this settings generation.
func (view SettingsView) Filter(agent AgentKind, runtime []core.Tool, snapshot Snapshot) ([]core.Tool, error) {
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
		if Resolve(entry.Requirement, snapshot, enabled).State != StateAvailable {
			continue
		}
		filtered = append(filtered, tool)
	}
	return filtered, nil
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

func metadataByName() map[string]Metadata {
	entries := make(map[string]Metadata, len(catalog))
	for _, entry := range catalog {
		entries[entry.Name] = entry
	}
	return entries
}
