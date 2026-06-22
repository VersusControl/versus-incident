package signalsources

import "sync"

// -----------------------------------------------------------------------------
// Data-source KIND taxonomy.
//
// Every signal-source TYPE belongs to a KIND — logs, metrics, or traces today,
// extensible to events/profiles later. The kind is the single source of truth
// that downstream behaviour keys off without re-deriving it: notably the
// agent's regex pre-filter default (logs keep the log-tuned global default;
// metrics/traces learn-all by default), and future per-kind UI grouping or
// routing.
//
// This registry mirrors Register / RegisterTypedBrain: OSS registers the six
// built-in LOG types from init() below; the enterprise module registers its
// metric (`prometheus`) and trace (`traces`) types through the SAME seam from
// its own init() — the established one-way direction (enterprise → OSS). OSS
// never imports enterprise. Any unregistered/unknown type defaults to KindLogs
// so behaviour is identical to before this taxonomy existed.
// -----------------------------------------------------------------------------

// Kind is the family a signal-source type belongs to. The string values match
// the seam Kind() the typed brains report ("logs"/"metrics"/"traces"), so a
// registered kind can be compared against a brain's Kind() in a drift test.
type Kind string

const (
	KindLogs    Kind = "logs"
	KindMetrics Kind = "metrics"
	KindTraces  Kind = "traces"
)

var (
	kindRegistryMu sync.RWMutex
	kindRegistry   = map[string]Kind{}
)

// RegisterKind records the KIND a source type belongs to. It is intended to be
// called from an init() (OSS for the built-in log types; the enterprise module
// for prometheus/traces). Registering with an empty type or empty kind, or
// registering the same type twice, panics — each indicates a wiring bug, not a
// runtime condition, exactly like Register.
func RegisterKind(sourceType string, kind Kind) {
	if sourceType == "" {
		panic("signalsources: RegisterKind called with empty source type")
	}
	if kind == "" {
		panic("signalsources: RegisterKind called with empty kind for type " + sourceType)
	}
	kindRegistryMu.Lock()
	defer kindRegistryMu.Unlock()
	if _, dup := kindRegistry[sourceType]; dup {
		panic("signalsources: RegisterKind called twice for type " + sourceType)
	}
	kindRegistry[sourceType] = kind
}

// KindOf returns the registered KIND for a source type, or KindLogs when the
// type is unknown/unregistered. The log default keeps any unrecognised type
// behaving exactly as it did before the taxonomy existed.
func KindOf(sourceType string) Kind {
	kindRegistryMu.RLock()
	defer kindRegistryMu.RUnlock()
	if k, ok := kindRegistry[sourceType]; ok {
		return k
	}
	return KindLogs
}

// init registers the six built-in OSS log source types. The type strings match
// exactly the ones the agent factory builds (pkg/agent.BuildSources) and the
// config schema documents (config.AgentSourceConfig.Type).
func init() {
	for _, t := range []string{
		"elasticsearch",
		"file",
		"loki",
		"cloudwatchlogs",
		"graylog",
		"splunk",
	} {
		RegisterKind(t, KindLogs)
	}
}
