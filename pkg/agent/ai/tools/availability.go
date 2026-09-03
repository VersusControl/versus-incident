package tools

import (
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"
	"unicode"
)

// State is the server-resolved availability state rendered by admin clients.
type State string

const (
	StateNeedsLicense       State = "needs_license"
	StateNeedsDataSource    State = "needs_datasource"
	StateNeedsIntegration   State = "needs_integration"
	StateNeedsCapability    State = "needs_capability"
	StateNeedsPermission    State = "needs_permission"
	StateUnhealthy          State = "unhealthy"
	StateDisabledByOperator State = "disabled_by_operator"
	StateAvailable          State = "available"
)

// DependencyStatus is a provider-neutral, bounded prerequisite observation.
type DependencyStatus struct {
	Configured  bool
	Constructed bool
	Healthy     bool
	Name        string
	Health      string
}

// Snapshot contains the current prerequisite state keyed by catalog values.
type Snapshot struct {
	DataSources  map[string]DependencyStatus
	Integrations map[string]DependencyStatus
	Capabilities map[string]DependencyStatus
}

// EntitlementDecision lets an external module distinguish a purchase gate from
// an ordinary setup step without exposing its license implementation to OSS.
type EntitlementDecision struct {
	Required    bool
	Reason      string
	Action      string
	ActionLabel string
}

// EntitlementResolver derives entitlement from the requirement and the actual
// provider capability observation. Returning Required=false defers to OSS.
type EntitlementResolver func(Requirement, DependencyStatus) EntitlementDecision

type entitlementResolverHolder struct{ resolve EntitlementResolver }

var entitlementResolver atomic.Pointer[entitlementResolverHolder]

// SetEntitlementResolver installs the process-wide entitlement adapter at boot.
func SetEntitlementResolver(resolver EntitlementResolver) {
	if resolver == nil {
		entitlementResolver.Store(nil)
		return
	}
	entitlementResolver.Store(&entitlementResolverHolder{resolve: resolver})
}

// Resolution is safe display copy for one tool card.
type Resolution struct {
	State       State  `json:"state"`
	Reason      string `json:"reason"`
	Action      string `json:"action"`
	ActionLabel string `json:"action_label"`
	Health      string `json:"health,omitempty"`
}

// Resolve evaluates requirements before operator state, so a disabled toggle
// can never mask a missing or unhealthy prerequisite.
func Resolve(requirement Requirement, snapshot Snapshot, enabled bool) Resolution {
	status, satisfied := requirementStatus(requirement, snapshot)
	if holder := entitlementResolver.Load(); holder != nil && holder.resolve != nil {
		if decision := holder.resolve(requirement, status); decision.Required {
			label := boundText(decision.ActionLabel, 40)
			if label == "" {
				label = "Learn more"
			}
			return Resolution{State: StateNeedsLicense, Reason: boundText(decision.Reason, 240), Action: safeAction(decision.Action), ActionLabel: label}
		}
	}
	if !satisfied {
		return missingResolution(requirement, status)
	}
	if !status.Healthy {
		return Resolution{
			State:       StateUnhealthy,
			Reason:      unhealthyReason(requirement, status),
			Action:      setupAction(requirement, true),
			ActionLabel: setupActionLabel(requirement, true),
			Health:      safeHealth(status.Health),
		}
	}
	if !enabled {
		return Resolution{State: StateDisabledByOperator, Reason: "Not offered to the agent.", Action: ""}
	}
	return Resolution{State: StateAvailable, Reason: "Available to the agent.", Action: ""}
}

func requirementStatus(requirement Requirement, snapshot Snapshot) (DependencyStatus, bool) {
	switch requirement.Kind {
	case RequirementNone:
		return DependencyStatus{Configured: true, Healthy: true, Name: "Versus"}, true
	case RequirementDataSource:
		status, ok := snapshot.DataSources[requirement.SignalKind]
		return status, ok && status.Configured
	case RequirementIntegration:
		status, ok := snapshot.Integrations[requirement.Integration]
		return status, ok && status.Configured
	case RequirementCapability:
		combined := DependencyStatus{Configured: true, Healthy: true}
		missing := make([]string, 0, len(requirement.Capabilities))
		unhealthy := make([]string, 0, len(requirement.Capabilities))
		for _, capability := range requirement.Capabilities {
			status, ok := snapshot.Capabilities[capability]
			if !ok || !status.Configured {
				missing = append(missing, displayRequirement(capability))
				continue
			}
			if !status.Healthy {
				combined.Healthy = false
				combined.Health = status.Health
				name := boundText(status.Name, 80)
				if name == "" {
					name = displayRequirement(capability)
				}
				unhealthy = append(unhealthy, name)
			}
		}
		if len(missing) > 0 {
			combined.Configured = false
			combined.Name = strings.Join(missing, " and ")
			return combined, false
		}
		combined.Name = strings.Join(unhealthy, " and ")
		return combined, true
	default:
		return DependencyStatus{}, false
	}
}

func missingResolution(requirement Requirement, status DependencyStatus) Resolution {
	switch requirement.Kind {
	case RequirementDataSource:
		kind := singular(requirement.SignalKind)
		return Resolution{State: StateNeedsDataSource, Reason: fmt.Sprintf("No %s data source is connected, so this tool cannot read %s data.", kind, kind), Action: "/settings?tab=agent", ActionLabel: "Add a data source"}
	case RequirementIntegration:
		name := displayRequirement(requirement.Integration)
		return Resolution{State: StateNeedsIntegration, Reason: fmt.Sprintf("%s is not connected, so this tool cannot %s.", name, integrationPurpose(requirement.Integration)), Action: integrationAction(requirement.Integration), ActionLabel: "Connect " + name}
	case RequirementCapability:
		name := status.Name
		if name == "" {
			name = "Required capability"
		}
		verb := "is"
		if strings.Contains(name, " and ") {
			verb = "are"
		}
		action := "/settings?tab=agent"
		label := "Configure capability"
		purpose := "use the required capability"
		if contains(requirement.Capabilities, "runbook_index") || contains(requirement.Capabilities, "ai_embedder") {
			action = "/admin#agent-ai-settings"
			label = "AI settings"
			purpose = "search runbooks"
		} else if contains(requirement.Capabilities, "dependency_graph") {
			purpose = "inspect service dependencies"
		}
		return Resolution{State: StateNeedsCapability, Reason: fmt.Sprintf("%s %s not configured, so this tool cannot %s.", name, verb, purpose), Action: action, ActionLabel: label}
	default:
		return Resolution{State: StateNeedsCapability, Reason: "The required capability is not configured, so this tool cannot run.", Action: "/settings?tab=agent", ActionLabel: "Configure capability"}
	}
}

func unhealthyReason(requirement Requirement, status DependencyStatus) string {
	name := boundText(status.Name, 80)
	if name == "" {
		name = displayRequirement(requirement.SignalKind)
		if name == "" {
			name = displayRequirement(requirement.Integration)
		}
	}
	verb := "is"
	if strings.Contains(name, " and ") {
		verb = "are"
	}
	return fmt.Sprintf("%s %s unhealthy (%s), so this tool cannot %s.", name, verb, safeHealth(status.Health), requirementPurpose(requirement))
}

func setupAction(requirement Requirement, unhealthy bool) string {
	if requirement.Kind == RequirementIntegration {
		return integrationAction(requirement.Integration)
	}
	if requirement.Kind == RequirementCapability && (contains(requirement.Capabilities, "ai_embedder") || contains(requirement.Capabilities, "runbook_index")) {
		return "/admin#agent-ai-settings"
	}
	if unhealthy {
		return "/settings?tab=agent"
	}
	return ""
}

func setupActionLabel(requirement Requirement, unhealthy bool) string {
	if !unhealthy {
		return ""
	}
	if requirement.Kind == RequirementIntegration {
		return "Check " + displayRequirement(requirement.Integration)
	}
	if requirement.Kind == RequirementCapability {
		if contains(requirement.Capabilities, "ai_embedder") || contains(requirement.Capabilities, "runbook_index") {
			return "AI settings"
		}
		return "Configure capability"
	}
	return "Configure data source"
}

func integrationAction(integration string) string {
	return "/settings?tab=agent"
}

func integrationPurpose(integration string) string {
	if integration == "github" {
		return "read recent changes"
	}
	if integration == "kubernetes" {
		return "inspect Kubernetes resources"
	}
	return "use the integration"
}

func requirementPurpose(requirement Requirement) string {
	switch requirement.Kind {
	case RequirementDataSource:
		return "read " + singular(requirement.SignalKind) + " data"
	case RequirementIntegration:
		return integrationPurpose(requirement.Integration)
	case RequirementCapability:
		return "use the required capability"
	default:
		return "run"
	}
}

func singular(value string) string { return strings.TrimSuffix(value, "s") }

func displayRequirement(value string) string {
	value = strings.ReplaceAll(value, "_", " ")
	if value == "" {
		return ""
	}
	if strings.EqualFold(value, "github") {
		return "GitHub"
	}
	if strings.EqualFold(value, "ai embedder") {
		return "AI embedder"
	}
	return strings.ToUpper(value[:1]) + value[1:]
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func safeHealth(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "authentication", "authorization", "connection", "timeout", "configuration", "unknown":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "unknown"
	}
}

func safeAction(value string) string {
	if internal := safeInternalPath(value, true); internal != "" {
		return internal
	}
	value = strings.TrimSpace(value)
	if value == "" || hasUnsafeURLText(value) {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return ""
	}
	if parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil {
		return boundText(value, 240)
	}
	return ""
}

func safeDocsURL(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || hasUnsafeURLText(value) || !strings.HasPrefix(value, "https://docs.versusincident.com/#/") {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "docs.versusincident.com" || parsed.User != nil || parsed.Path != "/" || parsed.RawQuery != "" || !strings.HasPrefix(parsed.Fragment, "/") || strings.HasPrefix(parsed.Fragment, "//") || hasUnsafeURLText(parsed.Fragment) {
		return ""
	}
	return value
}

func safeUIPath(value string) string {
	return safeInternalPath(value, false)
}

func safeInternalPath(value string, allowNavigationState bool) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if hasUnsafeURLText(value) {
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "" || parsed.Host != "" || parsed.User != nil || parsed.Opaque != "" {
		return ""
	}
	if !allowNavigationState && (parsed.RawQuery != "" || parsed.Fragment != "" || parsed.ForceQuery) {
		return ""
	}
	if !strings.HasPrefix(parsed.Path, "/") || strings.HasPrefix(parsed.Path, "//") || hasUnsafeURLText(parsed.Path) {
		return ""
	}
	return boundText(value, 240)
}

func hasUnsafeURLText(value string) bool {
	return strings.Contains(value, "\\") || strings.IndexFunc(value, unicode.IsControl) >= 0
}

func boundText(value string, limit int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) > limit {
		runes = runes[:limit]
	}
	return string(runes)
}
