package signalsources

import (
	"fmt"
	"sort"
	"sync"

	"github.com/VersusControl/versus-incident/pkg/core"
)

// -----------------------------------------------------------------------------
// Signal-source registration hook.
//
// The OSS build ships a fixed set of log-oriented SignalSource types
// (elasticsearch, file, loki, cloudwatchlogs, graylog, splunk) that the agent
// factory constructs directly. Other source types — notably the standing
// metric (`prometheus`) and trace (`traces`) data sources — live in Versus
// Enterprise. Rather than have the OSS tree import the enterprise module (which
// the one-way import rule forbids), the enterprise module REGISTERS its source
// types here from an init():
//
//	func init() {
//	    signalsources.Register("prometheus", func(name string, opts map[string]any) (core.SignalSource, error) {
//	        // decode opts into the enterprise config, build the source.
//	    })
//	}
//
// The agent factory consults this registry for any type it does not build in
// itself, so a registered enterprise source is wired with zero OSS changes.
// -----------------------------------------------------------------------------

// Factory builds a core.SignalSource for one configured source instance from
// its instance name and the generic per-source options block. The options map
// is the decoded YAML under the source's `options:` key (see
// config.AgentSourceConfig.Options); a Factory is responsible for decoding it
// into whatever concrete config struct it owns (e.g. via mapstructure).
type Factory func(name string, options map[string]any) (core.SignalSource, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// enterpriseSourceTypes lists the source types that used to ship in OSS but now
// live in Versus Enterprise. When one of these is configured on a build where
// the enterprise module is absent (so nothing called Register), the factory
// emits a clear "requires Versus Enterprise" error instead of a generic
// unknown-type error.
var enterpriseSourceTypes = map[string]bool{
	"prometheus": true,
	"traces":     true,
}

// Register makes a source type constructible by the agent factory. It is
// intended to be called from an init() in a module that provides additional
// source types (e.g. Versus Enterprise). Registering the same type name twice,
// or with a nil factory, panics — both indicate a programming error at wiring
// time, not a runtime condition.
func Register(typeName string, factory Factory) {
	if typeName == "" {
		panic("signalsources: Register called with empty type name")
	}
	if factory == nil {
		panic("signalsources: Register called with nil factory for type " + typeName)
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[typeName]; dup {
		panic("signalsources: Register called twice for type " + typeName)
	}
	registry[typeName] = factory
}

// Lookup returns the factory registered for typeName, if any.
func Lookup(typeName string) (Factory, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	f, ok := registry[typeName]
	return f, ok
}

// Registered returns the sorted list of registered source type names. Useful
// for diagnostics and admin surfaces.
func Registered() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// RequiresEnterprise reports whether typeName is a known source type that has
// moved to Versus Enterprise. The factory uses it to turn an unregistered
// enterprise type into an actionable error.
func RequiresEnterprise(typeName string) bool {
	return enterpriseSourceTypes[typeName]
}

// ErrRequiresEnterprise builds the standard error returned when a known
// enterprise source type is configured but no module has registered it.
func ErrRequiresEnterprise(typeName string) error {
	return fmt.Errorf("source type %q requires Versus Enterprise (the %s data source is not part of the open-source build); see https://versuscontrol.com for the enterprise module", typeName, typeName)
}
