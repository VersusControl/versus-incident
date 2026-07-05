package config

import (
	"context"
	"log"
	"sync"
)

// alertresolver.go — the single-slot runtime notification-channel config
// resolver seam.
//
// It lets a consumer (the enterprise hooks.Register) override the effective
// notification-channel configuration (`alert.*`: credentials + enable, per
// channel) at RUNTIME, so the incident emission path re-resolves the effective
// channel config on every incident instead of being pinned to the static YAML
// `alert.*` config for the life of the process.
//
// It is the direct sibling of the pkg/agent AISettingsResolver /
// SetAISettingsResolver seam (X27): one process-wide slot, registered once at
// boot, mutex-guarded. OSS registers nothing, so alertConfigResolver() returns
// nil and the emission path uses the static YAML `alert.*` config unchanged —
// community behaviour is byte-for-byte unchanged (one nil-check, no clone, no
// allocations, no goroutines).
//
// ============================================================================
// CONTROL-API CONTRACT for the Enterprise consumer (design C1 / tasks E1–E4)
// ============================================================================
//
// The enterprise module (versus-enterprise/pkg/runtimechannels) fills this
// seam with a store-backed, license-gated, per-org Resolver that seals channel
// secrets at rest (reusing runtimeai.Crypto). It exposes a control API that the
// admin UI calls. Enterprise MUST implement EXACTLY the following contract so
// the OSS seam, the store, and the UI compose:
//
//	:channel ∈ {slack, telegram, viber, email, msteams, lark}  (unknown ⇒ 400)
//
//	GET  /enterprise/api/agent/channel-settings
//	  → 200 masked view of ALL SIX channels. NEVER returns a raw secret.
//	    {
//	      "channels": {
//	        "slack": {
//	          "enabled":      true,          // effective enable (override or yaml)
//	          "configured":   true,          // a runtime override exists for this channel
//	          "source":       "override",    // "override" | "yaml"
//	          "yaml_enabled": false,         // the YAML floor's enable, for provenance
//	          "fields": {
//	            "token":      { "set": true,  "hint": "…x9f2" },   // last-4 for token-like
//	            "channel_id": { "set": true,  "hint": "C0123…" }   // non-secret echoed/masked
//	          }
//	        },
//	        "telegram": { … }, "viber": { … }, "email": { … },
//	        "msteams":  { … }, "lark":  { … }
//	      }
//	    }
//	    Masking rule (write-only): token-like secrets (Slack token, Telegram /
//	    Viber bot tokens, SMTP password/username) → last-4 hint only; URL-like
//	    secrets (Teams / Lark webhooks) → scheme+host only, path/query masked.
//	    No read path EVER returns the plaintext secret.
//
//	PUT  /enterprise/api/agent/channel-settings/:channel
//	  ← per-channel config body. A PRESENT secret field is sealed; a BLANK or
//	    OMITTED secret field PRESERVES the existing sealed value (so enable can
//	    be toggled or a channel id changed without resubmitting the token).
//	    Shape-validated against the factory's required-field rules; malformed ⇒
//	    400 (audited denial, never persisted). If no master key is configured a
//	    secret write is refused ⇒ 422 no_encryption_key (plaintext never
//	    persisted). Write-through updates the resolver's per-org cache
//	    synchronously so the NEXT incident uses it (no restart).
//	  → 200 masked response (same shape as one GET channel entry).
//
//	DELETE /enterprise/api/agent/channel-settings/:channel
//	  → clears that channel's override (writes a cleared marker) ⇒ resolver
//	    returns "no opinion" for it ⇒ reverts to the YAML floor on next send.
//
//	POST /enterprise/api/agent/channel-settings/:channel/test   (optional)
//	  → test-send with the candidate/effective config (optional pre-save
//	    validate) without persisting; rate-limited per org; audited.
//
//	Gating (all routes): licenseGate (403 community) → rbac.Require
//	(runtime:manage) (401 no session, 403 wrong role, 503 nil guard). Org is
//	taken from middleware.OrgFromContext, NEVER a caller-supplied body/header.
//	Every PUT/DELETE/test is audited (success AND denial) with a real subject
//	and a NON-SECRET target summary.
//
// The single ResolveAlert method below is the ONE seam Enterprise implements to
// feed the send path; the masked Get / per-channel Set / Clear used by the
// control API live entirely inside the enterprise resolver and never touch OSS.

// AlertConfigResolver resolves the effective notification-channel config at
// runtime. Implementations overlay this org's runtime channel overrides onto
// base — a per-request CLONE the caller owns — IN PLACE, per channel, and
// report whether any override was applied.
//
// Contract:
//   - Only channels the resolver holds an override for are overlaid; every
//     other channel is left exactly as base (the YAML floor) — partial override.
//   - It MUST NOT mutate global config; base is always a caller-owned clone.
//   - It MUST be fail-closed per channel: if a persisted override for a channel
//     fails to decrypt or is malformed, the resolver leaves THAT channel at its
//     base (YAML) value rather than breaking it. A corrupt override reverts to
//     YAML, it never silently mutes a channel.
//   - ctx carries the effective org for a future multi-tenant build; the
//     single-org resolver uses its boot-pinned org and may ignore ctx.
//   - It returns applied=true if it changed base at all, false if it had no
//     opinion for any channel (base untouched).
type AlertConfigResolver interface {
	ResolveAlert(ctx context.Context, base *AlertConfig) (applied bool)
}

// Process-wide single slot. A consumer registers a resolver at boot; the
// emission path reads it once per incident. Mutex-guarded so a boot-time
// registration is safely visible to the HTTP handler and worker goroutines.
var (
	alertResolverMu   sync.Mutex
	alertResolverSlot AlertConfigResolver
)

// SetAlertConfigResolver registers the resolver used to override the effective
// notification-channel config at runtime. Last-wins: a second call replaces the
// first. Passing nil clears the slot (back to the YAML floor). OSS ships none,
// so the emission path uses the static `alert.*` config unchanged. This is the
// entry point the enterprise hooks.Register attaches to (mirror of
// SetAISettingsResolver). Call at boot.
func SetAlertConfigResolver(r AlertConfigResolver) {
	alertResolverMu.Lock()
	defer alertResolverMu.Unlock()
	alertResolverSlot = r
}

// alertConfigResolver returns the registered resolver, or nil when none is set
// (community mode).
func alertConfigResolver() AlertConfigResolver {
	alertResolverMu.Lock()
	defer alertResolverMu.Unlock()
	return alertResolverSlot
}

// applyAlertResolver overlays the registered runtime channel override onto the
// cloned config's Alert section. It is a no-op when no resolver is registered
// (community). It is FAIL-SAFE: the resolver is run against a scratch copy of
// the Alert config, and the result is swapped into the clone only if the
// resolver applied an override AND did not panic. So a resolver that panics or
// misbehaves mid-overlay can never leave the clone half-mutated — the clone
// keeps its YAML floor and alerting is never silently broken by a bad override.
// (Per-channel fail-closed — a single corrupt channel reverting to YAML — is
// the resolver's own responsibility per the AlertConfigResolver contract.)
func applyAlertResolver(ctx context.Context, cloned *Config) {
	r := alertConfigResolver()
	if r == nil || cloned == nil {
		return
	}
	candidate := cloneAlertConfig(cloned.Alert)
	if safeResolveAlert(ctx, r, &candidate) {
		cloned.Alert = candidate
	}
}

// safeResolveAlert calls the resolver under a recover so a panicking resolver
// falls back to the YAML floor (applied=false) instead of crashing the
// emission path. Secrets are never logged — only the recovered value's presence
// is noted.
func safeResolveAlert(ctx context.Context, r AlertConfigResolver, base *AlertConfig) (applied bool) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("config: alert config resolver panicked; falling back to YAML channel config: %v", rec)
			applied = false
		}
	}()
	return r.ResolveAlert(ctx, base)
}
