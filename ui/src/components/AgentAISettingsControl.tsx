import { useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import clsx from "clsx";
import {
  AlertCircle,
  Cpu,
  Eye,
  EyeOff,
  KeyRound,
  Loader2,
  Sparkles,
} from "lucide-react";
import { ApiError, api, AI_PROVIDERS, type AISettingsView } from "@/lib/api";
import { keySetLabel, noEncryptionKeyMessage, providerKeyNotice } from "@/lib/agentAI";
import { AdminAccessNotice } from "@/components/AdminAccessNotice";
import { EnterpriseLockedBody } from "@/components/EnterpriseLocked";
import { useEffectiveRole } from "@/lib/useEffectiveRole";
import { adminGateState } from "@/lib/role";
import { useToast } from "@/components/toastContext";

// AgentAISettingsControl — the operator surface for the Enterprise runtime
// AI-settings override. It reads /enterprise/api/agent/ai-settings on
// mount, shows the EFFECTIVE enable (override or YAML floor), whether a key is
// set (masked — last4 only, NEVER the key), and lets an admin toggle AI and
// rotate the key (PUT) or revert to the YAML floor (DELETE).
//
// Like the mode control, every request rides the SSO session cookie and is
// authorized by the caller's RBAC role (runtime:manage) — NOT a static admin
// token. The surface is gated on the caller's effective role (useEffectiveRole):
//   not enterprise        → locked Enterprise upsell (no control)
//   no SSO session         → "sign in to manage" notice
//   viewer / responder     → read-only "requires the admin role" notice
//   admin / owner          → the live AI-settings control
//   422 no_encryption_key  → server master key not set (actionable, admin only)
//
// The AI key is NEVER persisted to localStorage/sessionStorage. It lives in a
// transient React state field, is sent on the single PUT, then cleared. The
// server never returns it.

// AGENT_AI_SETTINGS_ANCHOR is the DOM id the mode control scrolls to when its
// detect guard reports AI is off (the cross-wire in AgentModeControl).
export const AGENT_AI_SETTINGS_ANCHOR = "agent-ai-settings";

export function AgentAISettingsControl() {
  const qc = useQueryClient();
  const toast = useToast();
  const access = useEffectiveRole();
  const gate = adminGateState({
    loading: access.loading,
    enterprise: access.enterprise,
    hasSession: access.hasSession,
    isAdmin: access.isAdmin,
  });

  const settings = useQuery<AISettingsView>({
    queryKey: ["agent-ai-settings"],
    queryFn: api.getAISettings,
    // Only an admin issues the privileged GET (fail closed for viewers).
    enabled: gate === "admin",
    retry: (count, err) => {
      if (
        err instanceof ApiError &&
        [401, 403, 404, 503].includes(err.status)
      ) {
        return false;
      }
      return count < 1;
    },
  });

  const status = settings.error instanceof ApiError ? settings.error.status : null;

  // ----- role gate (SSO session + RBAC role) ----------
  if (gate === "loading") {
    return (
      <AIShell>
        <div className="flex items-center gap-2 text-xs text-ink-400">
          <Loader2 size={14} className="animate-spin" />
          Checking access…
        </div>
      </AIShell>
    );
  }
  if (gate === "locked") {
    return <AIShell><LockedBody /></AIShell>;
  }
  if (gate === "sign-in") {
    return <AIShell><AdminAccessNotice reason="sign-in" /></AIShell>;
  }
  if (gate === "read-only") {
    return <AIShell><AdminAccessNotice reason="role" /></AIShell>;
  }

  // ----- locked / upsell (defensive: admin whose binary lost the route) -----
  if (status === 403 || status === 404) {
    return <AIShell><LockedBody /></AIShell>;
  }

  // ----- loading ------------------------------------------------------------
  if (settings.isPending) {
    return (
      <AIShell>
        <div className="flex items-center gap-2 text-xs text-ink-400">
          <Loader2 size={14} className="animate-spin" />
          Reading AI settings…
        </div>
      </AIShell>
    );
  }

  // ----- other errors -------------------------------------------------------
  if (settings.isError || !settings.data) {
    return (
      <AIShell>
        <div className="flex items-center justify-between gap-3 text-xs">
          <span className="flex items-center gap-1.5 text-sev-critical">
            <AlertCircle size={13} />
            {settings.error instanceof Error
              ? settings.error.message
              : "Couldn't read AI settings."}
          </span>
          <button className="btn" onClick={() => settings.refetch()}>
            Retry
          </button>
        </div>
      </AIShell>
    );
  }

  return (
    <AIShell>
      <SettingsBody
        view={settings.data}
        refetch={() => settings.refetch()}
        invalidate={() => {
          // Re-read authoritative state and keep the runtime banner's AI chip
          // (sourced from agent-config) in sync, mirroring the mode control.
          qc.invalidateQueries({ queryKey: ["agent-ai-settings"] });
          qc.invalidateQueries({ queryKey: ["agent-config"] });
        }}
        toast={toast}
      />
    </AIShell>
  );
}

// SettingsBody — the populated control. Split out so the local enable/key form
// state lives below the data-loading guards (no conditional hooks).
function SettingsBody({
  view,
  refetch,
  invalidate,
  toast,
}: {
  view: AISettingsView;
  refetch: () => void;
  invalidate: () => void;
  toast: ReturnType<typeof useToast>;
}) {
  // Local enable mirrors the effective enable from GET; it re-syncs whenever a
  // fresh authoritative view arrives. The key field is transient (in-memory
  // only) and never seeded from the (masked) server view.
  const [enabled, setEnabled] = useState(view.enabled);
  const [provider, setProvider] = useState(view.provider ?? "");
  const [keyInput, setKeyInput] = useState("");
  const [showKey, setShowKey] = useState(false);

  useEffect(() => {
    setEnabled(view.enabled);
    setProvider(view.provider ?? "");
  }, [view.enabled, view.source, view.key_set, view.last4, view.provider]);

  const save = useMutation({
    mutationFn: (vars: { enabled: boolean; provider: string; apiKey: string }) =>
      api.setAISettings(vars.enabled, vars.provider, vars.apiKey),
    onSuccess: () => {
      toast.push({ title: "AI settings saved", tone: "ok" });
      setKeyInput(""); // drop the transient key the moment the PUT lands
      setShowKey(false);
      invalidate();
      refetch();
    },
    onError: (err) => {
      // The no_encryption_key denial (422) is surfaced inline as an actionable
      // server-config message, not a generic toast.
      if (noEncryptionKeyMessage(err)) {
        setKeyInput(""); // never keep a key we couldn't store
        return;
      }
      toast.push({
        title: "Couldn't save AI settings",
        description: err instanceof Error ? err.message : String(err),
        tone: "error",
      });
    },
  });

  const clear = useMutation({
    mutationFn: () => api.clearAISettings(),
    onSuccess: () => {
      toast.push({ title: "Reverted to YAML floor", tone: "ok" });
      setKeyInput("");
      setShowKey(false);
      invalidate();
      refetch();
    },
    onError: (err) => {
      toast.push({
        title: "Couldn't revert AI settings",
        description: err instanceof Error ? err.message : String(err),
        tone: "error",
      });
    },
  });

  const busy = save.isPending || clear.isPending;
  const onOverride = view.source === "override";
  const noKeyMsg = noEncryptionKeyMessage(save.error);

  // When the operator stages a provider change
  // without entering the matching key, warn that the previous key would be
  // reused (Bearer providers) or fall back to the YAML key (claude/gemini
  // header-auth). Save then asks for an explicit confirmation.
  const providerNotice = providerKeyNotice(
    view.provider ?? "",
    provider,
    keyInput.trim().length > 0,
  );

  const onSave = () => {
    if (
      providerNotice.requireKey &&
      !window.confirm(
        `${providerNotice.message}\n\nSave anyway and reuse the existing key?`,
      )
    ) {
      return; // operator backed out — let them enter the matching key first
    }
    save.mutate({ enabled, provider, apiKey: keyInput.trim() });
  };

  return (
    <div className="flex flex-col gap-4">
      {/* Effective enable + provenance */}
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
        <div className="flex items-center gap-2">
          <span className="text-2xs uppercase tracking-wider text-ink-400">
            AI
          </span>
          <EnabledBadge enabled={view.enabled} />
        </div>
        <span className="inline-flex items-center gap-1.5 text-2xs text-ink-300">
          <KeyRound size={12} aria-hidden className="text-ink-400" />
          {keySetLabel(view.key_set, view.last4)}
        </span>
        <span className="inline-flex items-center gap-1.5 text-2xs text-ink-300">
          <Cpu size={12} aria-hidden className="text-ink-400" />
          provider: {view.provider ? view.provider : "config default"}
        </span>
      </div>

      {/* Enable toggle */}
      <label className="flex w-fit cursor-pointer items-center gap-2 text-xs text-ink-200">
        <input
          type="checkbox"
          checked={enabled}
          disabled={busy}
          onChange={(e) => setEnabled(e.target.checked)}
          className="h-4 w-4 accent-link"
        />
        Enable AI for this org
      </label>

      {/* Model provider — a change rebuilds the model at runtime (no restart) */}
      <div>
        <label className="field-label" htmlFor="ai-provider">
          Model provider{" "}
          <span className="font-normal text-ink-400">
            (enter the matching key below when you switch)
          </span>
        </label>
        <select
          id="ai-provider"
          value={provider}
          disabled={busy}
          onChange={(e) => setProvider(e.target.value)}
          className="input h-9 max-w-sm text-sm"
        >
          <option value="">Use config default</option>
          {AI_PROVIDERS.map((p) => (
            <option key={p} value={p}>
              {p}
            </option>
          ))}
        </select>
        {providerNotice.show && (
          <div
            role={providerNotice.tone === "warn" ? "alert" : undefined}
            className={clsx(
              "mt-2 flex items-start gap-2 rounded-control border p-2.5 text-2xs",
              providerNotice.tone === "warn"
                ? "border-sev-warn/40 bg-sev-warn/10"
                : "border-link/30 bg-link/10",
            )}
          >
            <AlertCircle
              size={13}
              aria-hidden
              className={clsx(
                "mt-0.5 shrink-0",
                providerNotice.tone === "warn" ? "text-sev-warn" : "text-link",
              )}
            />
            <p className="text-ink-200">{providerNotice.message}</p>
          </div>
        )}
      </div>

      {/* Masked API key (transient — never persisted to browser storage) */}
      <div>
        <label className="field-label" htmlFor="ai-api-key">
          API key{" "}
          <span className="font-normal text-ink-400">
            (leave blank to keep the current key)
          </span>
        </label>
        <div className="relative max-w-sm">
          <input
            id="ai-api-key"
            type={showKey ? "text" : "password"}
            autoComplete="off"
            placeholder={view.key_set ? "•••• stored — blank keeps it" : "sk-…"}
            value={keyInput}
            disabled={busy}
            onChange={(e) => setKeyInput(e.target.value)}
            className="input h-9 pr-9 text-sm"
          />
          <button
            type="button"
            aria-label={showKey ? "Hide key" : "Show key"}
            aria-pressed={showKey}
            className="absolute right-1 top-1/2 -translate-y-1/2 rounded-control p-1.5 text-ink-300 hover:bg-ink-600 hover:text-ink-100"
            onClick={() => setShowKey((s) => !s)}
          >
            {showKey ? <EyeOff size={13} aria-hidden /> : <Eye size={13} aria-hidden />}
          </button>
        </div>
      </div>

      {/* no_encryption_key (422) — actionable server-config message */}
      {noKeyMsg && (
        <div
          role="alert"
          className="flex items-start gap-2 rounded-control border border-sev-warn/40 bg-sev-warn/10 p-3 text-xs"
        >
          <AlertCircle size={14} className="mt-0.5 shrink-0 text-sev-warn" aria-hidden />
          <div>
            <p className="font-medium text-ink-100">
              Server can't store the API key
            </p>
            <p className="mt-0.5 text-ink-300">{noKeyMsg}</p>
          </div>
        </div>
      )}

      {/* Actions */}
      <div className="flex flex-wrap gap-2">
        <button
          type="button"
          disabled={busy}
          onClick={onSave}
          className="btn btn-primary"
        >
          {save.isPending ? "Saving…" : "Save"}
        </button>
        {onOverride && (
          <button
            type="button"
            disabled={busy}
            onClick={() => clear.mutate()}
            className="btn"
            title="Clear the override and follow the YAML floor"
          >
            Clear override
          </button>
        )}
      </div>

      <p className="text-2xs text-ink-400">
        Detect mode requires AI to be enabled.
      </p>
    </div>
  );
}

// AIShell — the consistent card chrome every state renders inside. Carries the
// scroll anchor the mode control's detect cross-wire targets.
function AIShell({ children }: { children: React.ReactNode }) {
  return (
    <div id={AGENT_AI_SETTINGS_ANCHOR} className="card mb-4 scroll-mt-4">
      <div className="card-header">
        <h2 className="card-title">AI settings</h2>
        <span className="text-2xs text-ink-400">Enterprise control</span>
      </div>
      <div className="card-body">{children}</div>
    </div>
  );
}

// EnabledBadge — icon + text chip (state never conveyed by color alone).
function EnabledBadge({ enabled }: { enabled: boolean }) {
  return (
    <span
      className={clsx(
        "inline-flex items-center gap-1 rounded-full border px-2.5 py-1 text-xs font-medium",
        enabled
          ? "border-sev-ok/40 bg-sev-ok/15 text-sev-ok"
          : "border-ink-500/40 bg-ink-600/40 text-ink-300",
      )}
    >
      <Sparkles size={13} aria-hidden />
      {enabled ? "enabled" : "disabled"}
    </span>
  );
}

// LockedBody — the shared Enterprise-only locked upsell, with AI-settings copy.
function LockedBody() {
  return (
    <EnterpriseLockedBody title="AI settings are an Enterprise capability">
      Manage the agent's AI provider key and toggle AI per org at runtime —
      without editing YAML or restarting — and revert instantly. Available on
      Versus Enterprise.
    </EnterpriseLockedBody>
  );
}
