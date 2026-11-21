package agent

import (
	"context"
	"strings"

	einowrap "github.com/VersusControl/versus-incident/pkg/agent/ai/eino"
	"github.com/VersusControl/versus-incident/pkg/config"
)

// agentName returns the agent identifier passed to the mode resolver.
// The agent config carries no name field today, so this is the empty
// string; it exists as the single seam the resolver keys on, ready for a
// real identifier without touching the call site.
func (w *Worker) agentName() string { return "" }

// effectiveMode resolves the lifecycle mode for one tick. It starts from
// the static YAML floor (cfg.Mode, defaulting to training) and lets a
// registered ModeResolver override it. It fails closed: a nil resolver, an
// ok=false result, or an invalid mode all keep the YAML floor. OSS
// registers no resolver, so this is one nil-check and returns cfg.Mode —
// byte-for-byte unchanged.
func (w *Worker) effectiveMode(ctx context.Context) string {
	yaml := w.cfg.Mode
	if yaml == "" {
		yaml = "training"
	}
	if r := modeResolver(); r != nil {
		if m, ok := r.Mode(ctx, w.agentName()); ok && isValidMode(m) {
			return m
		}
	}
	return yaml
}

// effectiveAIEnabled reports whether the detect path should call the AI on
// this tick. A registered AISettingsResolver decides at runtime and is read
// live, so a hot toggle takes effect on the next call without a restart.
// When the resolver has no answer (ok=false) the static agent config
// decides: AI runs only when agent.ai.enable is on and a credential is
// configured (or the provider needs none), so an unset runtime setting never
// turns a disabled, keyless AI into an outbound model call. With no resolver
// (OSS) the detect agent is only built when AI is enabled, so this returns
// true and the w.ai.Detect nil-check decides.
func (w *Worker) effectiveAIEnabled(ctx context.Context) bool {
	return EffectiveAIEnabled(ctx, w.cfg.AI)
}

// EffectiveAIEnabled is the runtime "may this call reach the model" decision
// shared by the detect worker and the on-demand analyze endpoints: the
// registered AISettingsResolver's answer when it has one, else the static
// config (enabled, plus a usable credential). With no resolver (OSS) the AI
// agents are only built when AI is enabled, so the enable flag is left to the
// caller's nil-agent check, but a configured model without a usable credential
// is still off: it would send incident data to the provider unauthenticated.
// ctx must already carry the request's org (see DecorateAIContext) for a
// per-org resolver to answer.
func EffectiveAIEnabled(ctx context.Context, ai config.AgentAIConfig) bool {
	if r := aiSettingsResolver(); r != nil {
		if enabled, ok := r.EffectiveEnabled(ctx); ok {
			return enabled
		}
		return staticAIEnabled(ai)
	}
	return !ai.Enable || aiCredentialUsable(ai)
}

// staticAIEnabled is the config-only answer to "may the agent call the AI".
func staticAIEnabled(ai config.AgentAIConfig) bool {
	return ai.Enable && aiCredentialUsable(ai)
}

// aiCredentialUsable reports whether a model call would be authenticated: an
// API key is set, the provider needs none (Ollama), or a base_url points the
// call at an endpoint the operator configured (which may authenticate by
// network position) — the same rule the Enterprise runtime settings apply.
func aiCredentialUsable(ai config.AgentAIConfig) bool {
	return strings.TrimSpace(ai.APIKey) != "" ||
		einowrap.IsKeylessProvider(ai.Provider) ||
		strings.TrimSpace(ai.BaseURL) != ""
}
