package eino

import (
	"context"
	"log"
	"sync"

	"github.com/cloudwego/eino/components/model"

	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/core"
)

// holder.go — the runtime model-construction lifecycle.
//
// The model PROVIDER (openai | deepseek | qwen | ollama | claude | gemini) is
// chosen at CONSTRUCTION time: each provider picks a different SDK/builder
// (provider.go), so a provider change cannot hot-reload through a request
// header the way the per-request key override (AuthKeyFunc) does — the model
// object must be REBUILT. A Holder is the rebuild lifecycle: it caches a built
// model keyed by its effective signature and rebuilds only when that signature
// changes, so steady-state has no per-call cost while an operator's runtime
// provider change is picked up on the next Get without a process restart.
//
// The package stays generic: it knows nothing about WHO supplies the runtime
// override. The agent package injects a RuntimeAI backed by its registered
// AISettingsResolver (no import cycle — eino never imports agent), and the
// enterprise slo-advisor injects its own per-org RuntimeAI. With a zero
// RuntimeAI (the OSS default) every override func is nil, so the signature is
// pinned to the configured provider and the model is built exactly once —
// community behaviour is byte-for-byte unchanged.

// RuntimeAI is the optional set of runtime override signals folded into a
// Holder's rebuild signature. Every func MAY be nil; a zero RuntimeAI is the
// community default (no overrides ⇒ the configured provider, built once,
// never rebuilt).
//
// Provider composes with the existing per-request key override: the key VALUE
// still hot-reloads through the AuthKeyFunc transport WITHOUT a rebuild (it is
// NOT part of the signature); only the provider, the model id, and the
// enable/key-presence STATE force a rebuild.
type RuntimeAI struct {
	// Provider returns a runtime provider override for ctx. ok=false ⇒ no
	// opinion (use the configured provider). An unknown/unsupported value
	// FAILS CLOSED: the Holder keeps the configured provider and logs once
	// per distinct bad value — it never crashes and never silently falls
	// back to openai.
	Provider func(ctx context.Context) (provider string, ok bool)

	// Enabled folds the runtime enable state into the signature so a toggle
	// rebuilds. ok=false ⇒ not folded. Optional.
	Enabled func(ctx context.Context) (enabled bool, ok bool)

	// KeySet folds whether a runtime key override is currently present into
	// the signature so a set⟷clear transition rebuilds. ok=false ⇒ not
	// folded. The key VALUE is never part of the signature (it hot-reloads
	// through the transport). Optional.
	KeySet func(ctx context.Context) (set bool, ok bool)
}

// triState captures a folded boolean signal that may also be "unknown" so the
// signature stays a comparable value.
type triState uint8

const (
	triUnknown triState = iota
	triFalse
	triTrue
)

func foldBool(fn func(ctx context.Context) (bool, bool), ctx context.Context) triState {
	if fn == nil {
		return triUnknown
	}
	v, ok := fn(ctx)
	if !ok {
		return triUnknown
	}
	if v {
		return triTrue
	}
	return triFalse
}

// modelSignature is the comparable rebuild key. Two Gets that resolve to the
// same signature reuse the cached model; any difference forces a rebuild.
type modelSignature struct {
	provider string
	model    string
	enabled  triState
	keySet   triState
}

// Holder lazily builds and caches a model artifact of type T (a chat model, a
// tool-calling chat model, an embedder, or a higher-level agent that wraps
// one), rebuilding only when the effective signature changes. It is
// concurrency-safe: agent ticks and an operator's runtime change can race, so
// every Get is mutex-guarded.
type Holder[T any] struct {
	mu    sync.Mutex
	base  config.AgentAIConfig
	opts  Options
	rt    RuntimeAI
	valid func(provider string) bool
	build func(ctx context.Context, cfg config.AgentAIConfig, opts Options) (T, error)

	haveCached bool
	sig        modelSignature
	cached     T

	lastBad string // last unknown runtime provider already logged (de-dup)
}

// NewModelHolder builds a Holder for an arbitrary artifact whose backend is a
// CHAT provider (runtime provider overrides are validated against the chat
// registry). build receives a copy of base with Provider already resolved to
// the effective provider for the current signature; callers route their
// construction (e.g. NewToolCallingChatModel + a ReAct agent) through it.
func NewModelHolder[T any](base config.AgentAIConfig, opts Options, rt RuntimeAI, build func(ctx context.Context, cfg config.AgentAIConfig, opts Options) (T, error)) *Holder[T] {
	return &Holder[T]{base: base, opts: opts, rt: rt, valid: isChatProvider, build: build}
}

// NewChatModelHolder is the shared entrypoint for the tool-free, JSON-mode
// chat path (detect, slo-advisor). It rebuilds the model on a provider /
// model / state change and returns model.BaseChatModel.
func NewChatModelHolder(base config.AgentAIConfig, opts Options, rt RuntimeAI) *Holder[model.BaseChatModel] {
	return NewModelHolder(base, opts, rt, func(ctx context.Context, cfg config.AgentAIConfig, o Options) (model.BaseChatModel, error) {
		return NewChatModel(ctx, cfg, o)
	})
}

// NewToolCallingChatModelHolder is the shared entrypoint for the tool-calling
// analyze path. It returns model.ToolCallingChatModel; the caller binds tools.
func NewToolCallingChatModelHolder(base config.AgentAIConfig, opts Options, rt RuntimeAI) *Holder[model.ToolCallingChatModel] {
	return NewModelHolder(base, opts, rt, func(ctx context.Context, cfg config.AgentAIConfig, o Options) (model.ToolCallingChatModel, error) {
		return NewToolCallingChatModel(ctx, cfg, o)
	})
}

// NewEmbedderHolder mirrors the chat holders for the embedding path. Runtime
// provider overrides are validated against the (smaller) embedding registry,
// so a runtime switch to a chat-only provider (deepseek, claude, qwen) fails
// closed to the configured embedding provider.
func NewEmbedderHolder(base config.AgentAIConfig, opts Options, rt RuntimeAI) *Holder[core.Embedder] {
	return &Holder[core.Embedder]{
		base:  base,
		opts:  opts,
		rt:    rt,
		valid: isEmbedderProvider,
		build: func(ctx context.Context, cfg config.AgentAIConfig, o Options) (core.Embedder, error) {
			return NewEmbedder(ctx, cfg, o)
		},
	}
}

// Get returns the model for the current effective signature, rebuilding it
// only when the signature has changed since the last build. A build error is
// returned to the caller and the previously-cached model (if any) is left
// untouched, so a transient failure never poisons a healthy holder.
func (h *Holder[T]) Get(ctx context.Context) (T, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	sig := h.resolveSignature(ctx)
	if h.haveCached && sig == h.sig {
		return h.cached, nil
	}

	cfg := h.base
	cfg.Provider = sig.provider
	v, err := h.build(ctx, cfg, h.opts)
	if err != nil {
		var zero T
		return zero, err
	}
	h.cached = v
	h.sig = sig
	h.haveCached = true
	return v, nil
}

// Current returns the most recently built artifact without consulting the
// runtime signature (no rebuild). It returns the zero value when nothing has
// been built yet. Used by structural guards that need the concrete built
// model, never on the hot path.
func (h *Holder[T]) Current() T {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cached
}

// resolveSignature computes the effective rebuild signature. The configured
// provider is NOT validated here (an explicitly-set unknown provider fails
// fast at build time, as before); only the RUNTIME override is validated and
// fails closed to the configured provider. Caller holds h.mu.
func (h *Holder[T]) resolveSignature(ctx context.Context) modelSignature {
	provider := resolveProvider(h.base.Provider)
	if h.rt.Provider != nil {
		if p, ok := h.rt.Provider(ctx); ok {
			name := resolveProvider(p)
			if h.valid(name) {
				provider = name
			} else if h.lastBad != name {
				log.Printf("eino: runtime provider %q unsupported; keeping configured provider %q", p, provider)
				h.lastBad = name
			}
		}
	}
	return modelSignature{
		provider: provider,
		model:    h.base.Model,
		enabled:  foldBool(h.rt.Enabled, ctx),
		keySet:   foldBool(h.rt.KeySet, ctx),
	}
}

// isChatProvider reports whether name is a registered chat provider.
func isChatProvider(name string) bool {
	_, ok := chatModelBuilders[name]
	return ok
}

// isEmbedderProvider reports whether name is a registered embedding provider.
func isEmbedderProvider(name string) bool {
	_, ok := embedderBuilders[name]
	return ok
}
