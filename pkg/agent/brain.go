package agent

import (
	"log"
	"sort"
	"sync"

	"github.com/VersusControl/versus-incident/pkg/core"
)

// -----------------------------------------------------------------------------
// Typed-brain registration hook.
//
// The worker runs ONE generic lifecycle (training → shadow → detect → analyze)
// and plugs a per-type "brain" — a core.SignalLearner + core.SignalDetector —
// into the key→learn→score triad. OSS ships only the LOG brain (built in by the
// worker as the default for every source). Other source TYPES — notably the
// standing metric (`prometheus`) and trace (`traces`) data sources — get their
// learned brains from Versus Enterprise, which REGISTERS a BrainFactory here
// from an init():
//
//	func init() {
//	    agent.RegisterTypedBrain("prometheus", func(name string, opts map[string]any) (core.SignalLearner, core.SignalDetector, error) {
//	        // decode opts into the enterprise config, build the metric brain.
//	    })
//	}
//
// The worker consults this registry per configured source TYPE; a type with no
// registered brain uses the built-in log brain, so the OSS build behaves
// byte-for-byte as before. This mirrors signalsources.Register and keeps the
// one-way import rule intact: OSS defines the hook, enterprise implements it,
// OSS never imports enterprise.
// -----------------------------------------------------------------------------

// BrainFactory builds the Learner + Detector pair for one configured source
// instance from its instance name and the generic per-source options block
// (the decoded YAML under the source's `options:` key, see
// config.AgentSourceConfig.Options). A factory decodes the map into whatever
// concrete config it owns. Returning an error makes the worker fall back to the
// log brain for that source (logged, non-fatal).
type BrainFactory func(name string, options map[string]any) (core.SignalLearner, core.SignalDetector, error)

var (
	brainRegistryMu sync.RWMutex
	brainRegistry   = map[string]BrainFactory{}
)

// RegisterTypedBrain makes a per-type brain constructible by the worker for a
// given source type. It is intended to be called from an init() in a module
// that provides additional signal types (e.g. Versus Enterprise). Registering
// the same type twice, or with a nil factory, panics — both indicate a wiring
// bug, not a runtime condition.
func RegisterTypedBrain(sourceType string, mk BrainFactory) {
	if sourceType == "" {
		panic("agent: RegisterTypedBrain called with empty source type")
	}
	if mk == nil {
		panic("agent: RegisterTypedBrain called with nil factory for type " + sourceType)
	}
	brainRegistryMu.Lock()
	defer brainRegistryMu.Unlock()
	if _, dup := brainRegistry[sourceType]; dup {
		panic("agent: RegisterTypedBrain called twice for type " + sourceType)
	}
	brainRegistry[sourceType] = mk
}

// lookupTypedBrain returns the brain factory registered for sourceType, if any.
func lookupTypedBrain(sourceType string) (BrainFactory, bool) {
	brainRegistryMu.RLock()
	defer brainRegistryMu.RUnlock()
	f, ok := brainRegistry[sourceType]
	return f, ok
}

// RegisteredTypedBrains returns the sorted list of source types that have a
// registered brain. Useful for diagnostics and admin surfaces. OSS returns an
// empty slice (the log brain is the un-registered default).
func RegisteredTypedBrains() []string {
	brainRegistryMu.RLock()
	defer brainRegistryMu.RUnlock()
	out := make([]string, 0, len(brainRegistry))
	for k := range brainRegistry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// typedBrain pairs a resolved Learner with its Detector for one source. The two
// halves are almost always the same value (the log brain implements both), but
// the seam keeps them separate so a type can supply an independent scoring
// policy without re-implementing the learner.
type typedBrain struct {
	learner  core.SignalLearner
	detector core.SignalDetector
}

// resolveRegisteredBrains builds the externally registered brain (Versus
// Enterprise's metric/trace brains, in practice) for each ENABLED configured
// source whose type has one. Sources with no registered brain are left out of
// the map and fall back to the log brain at tick time, so the OSS build wires
// nothing here. A factory error (or a nil return) is logged and the source
// falls back to the log brain rather than failing the worker.
func (w *Worker) resolveRegisteredBrains() {
	for _, s := range w.cfg.Sources {
		if !s.Enable {
			continue
		}
		mk, ok := lookupTypedBrain(s.Type)
		if !ok {
			continue // no registered brain → log brain (resolved lazily)
		}
		learner, detector, err := mk(s.Name, s.Options)
		if err != nil {
			log.Printf("agent: typed brain for source %s (type=%s) failed to build: %v; falling back to log brain",
				s.Name, s.Type, err)
			continue
		}
		if learner == nil || detector == nil {
			log.Printf("agent: typed brain for source %s (type=%s) returned nil; falling back to log brain",
				s.Name, s.Type)
			continue
		}
		w.brains[s.Name] = typedBrain{learner: learner, detector: detector}
		log.Printf("agent: source %s using %q brain", s.Name, learner.Kind())
	}
}

// brainFor resolves the brain for a source by name. Enterprise brains were
// resolved at construction (keyed by the configured source type); any source
// without one — every source in the OSS build — gets the built-in log brain,
// created lazily and cached. The log brain is handed the source's kind-resolved
// matcher (matcherForSource): logs keep the global default, metrics/traces
// learn all by default, with the optional top-level per-kind override
// (agent.regex.metrics / agent.regex.traces) narrowing that kind when set. The
// lazy creation is mutex-guarded because ticks for different sources run
// concurrently; the shared miner/catalog the log brain wraps are themselves
// synchronised.
func (w *Worker) brainFor(name string) (core.SignalLearner, core.SignalDetector) {
	w.brainMu.Lock()
	defer w.brainMu.Unlock()
	if b, ok := w.brains[name]; ok {
		return b.learner, b.detector
	}
	lb := newLogBrain(name, w.miner, w.catalog, w.matcherForSource(name), w.services, w.ewmaAlpha, w.cfg.Catalog, w.redactor)
	w.brains[name] = typedBrain{learner: lb, detector: lb}
	return lb, lb
}
