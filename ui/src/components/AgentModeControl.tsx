import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import clsx from "clsx";
import {
  AlertCircle,
  EyeOff,
  GraduationCap,
  Loader2,
  Radar,
  Sparkles,
  type LucideIcon,
} from "lucide-react";
import {
  ApiError,
  api,
  type AgentMode,
  type AgentModeView,
} from "@/lib/api";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { AdminAccessNotice } from "@/components/AdminAccessNotice";
import { EnterpriseLockedBody } from "@/components/EnterpriseLocked";
import { AGENT_AI_SETTINGS_ANCHOR } from "@/components/AgentAISettingsControl";
import { useEffectiveRole } from "@/lib/useEffectiveRole";
import { adminGateState } from "@/lib/role";
import { detectAiDisabledRemedy } from "@/lib/agentAI";
import { useToast } from "@/components/toastContext";

// AgentModeControl — the operator surface for the Enterprise runtime
// mode-override. It reads /enterprise/api/agent/mode on mount, shows the
// EFFECTIVE mode, and lets an admin switch training / shadow / detect (PUT).
//
// Unlike the read-only learned-signals views (gateway secret), every request
// here rides the SSO session cookie and is authorized by the caller's RBAC
// role (runtime:manage, held by admin/owner).
// Arming `detect` opens real incidents / on-call pages, so it is privileged.
//
// The surface is gated on the caller's effective role (useEffectiveRole):
//   not enterprise        → locked Enterprise upsell (no control)
//   no SSO session         → "sign in to manage" notice
//   viewer / responder     → read-only "requires the admin role" notice
//   admin / owner          → the live mode control

const MODES: Array<{ value: AgentMode; label: string; icon: LucideIcon }> = [
  { value: "training", label: "Training", icon: GraduationCap },
  { value: "shadow", label: "Shadow", icon: EyeOff },
  { value: "detect", label: "Detect", icon: Radar },
];

const MODE_BLURB: Record<AgentMode, string> = {
  training: "Observes only — learns baselines, never alerts.",
  shadow: "Classifies and logs would-have-alerted events, but stays silent.",
  detect: "Calls the AI SRE and opens real incidents / on-call pages.",
};

export function AgentModeControl() {
  const qc = useQueryClient();
  const toast = useToast();
  const access = useEffectiveRole();
  const gate = adminGateState({
    loading: access.loading,
    enterprise: access.enterprise,
    hasSession: access.hasSession,
    isAdmin: access.isAdmin,
  });
  const [pendingDetect, setPendingDetect] = useState(false);

  const mode = useQuery<AgentModeView>({
    queryKey: ["agent-mode"],
    queryFn: api.getAgentMode,
    // Only the admin-gated endpoint is hit, and only once we know the caller is
    // an admin — a viewer never issues a privileged GET (fail closed).
    enabled: gate === "admin",
    retry: (count, err) => {
      // Locked / token states are terminal, not transient — never retry them.
      if (
        err instanceof ApiError &&
        [401, 403, 404, 503].includes(err.status)
      ) {
        return false;
      }
      return count < 1;
    },
  });

  // setMode is optimistic-but-VERIFIED: the mutation fires, then we
  // invalidate so the authoritative effective+source is re-read from GET. We
  // also refresh agent-config so the Runtime banner's mode chip stays in sync.
  const refreshAuthoritative = () => {
    qc.invalidateQueries({ queryKey: ["agent-mode"] });
    qc.invalidateQueries({ queryKey: ["agent-config"] });
  };

  const setMode = useMutation({
    mutationFn: (vars: { mode: AgentMode; confirm?: boolean }) =>
      api.setAgentMode(vars.mode, vars.confirm),
    onSuccess: (_data, vars) => {
      toast.push({
        title: `Mode set to ${vars.mode}`,
        tone: vars.mode === "detect" ? "info" : "ok",
      });
      refreshAuthoritative();
    },
    onError: (err) => {
      // The detect AI-guard (422 ai_disabled) is NOT a generic failure — it is
      // an actionable block surfaced inline on the detect confirm path (see the
      // ConfirmDialog below), so suppress the error toast for it.
      if (detectAiDisabledRemedy(err)) return;
      toast.push({
        title: "Couldn't change mode",
        description: err instanceof Error ? err.message : String(err),
        tone: "error",
      });
    },
  });

  const status = mode.error instanceof ApiError ? mode.error.status : null;

  // ----- role gate (SSO session + RBAC role) ----------
  if (gate === "loading") {
    return (
      <ModeShell>
        <div className="flex items-center gap-2 text-xs text-ink-400">
          <Loader2 size={14} className="animate-spin" />
          Checking access…
        </div>
      </ModeShell>
    );
  }
  if (gate === "locked") {
    return <LockedCard />;
  }
  if (gate === "sign-in") {
    return (
      <ModeShell>
        <AdminAccessNotice reason="sign-in" />
      </ModeShell>
    );
  }
  if (gate === "read-only") {
    return (
      <ModeShell>
        <AdminAccessNotice reason="role" />
      </ModeShell>
    );
  }

  // ----- locked / upsell (defensive: admin whose binary lost the route) -----
  if (status === 403 || status === 404) {
    return <LockedCard />;
  }

  // ----- loading ------------------------------------------------------------
  if (mode.isPending) {
    return (
      <ModeShell>
        <div className="flex items-center gap-2 text-xs text-ink-400">
          <Loader2 size={14} className="animate-spin" />
          Reading runtime mode…
        </div>
      </ModeShell>
    );
  }

  // ----- other errors -------------------------------------------------------
  if (mode.isError || !mode.data) {
    return (
      <ModeShell>
        <div className="flex items-center justify-between gap-3 text-xs">
          <span className="flex items-center gap-1.5 text-sev-critical">
            <AlertCircle size={13} />
            {mode.error instanceof Error
              ? mode.error.message
              : "Couldn't read runtime mode."}
          </span>
          <button className="btn" onClick={() => mode.refetch()}>
            Retry
          </button>
        </div>
      </ModeShell>
    );
  }

  const view = mode.data;
  const busy = setMode.isPending;
  // When the last detect PUT was blocked by the AI-guard (422 ai_disabled),
  // this carries the server remedy to surface inline on the confirm path.
  const detectRemedy = detectAiDisabledRemedy(setMode.error);

  const choose = (next: AgentMode) => {
    if (next === view.effective) return;
    if (next === "detect") {
      setPendingDetect(true);
      return;
    }
    setMode.mutate({ mode: next });
  };

  return (
    <ModeShell>
      <div className="flex flex-col gap-4">
        {/* Effective mode */}
        <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
          <div className="flex items-center gap-2">
            <span className="text-2xs uppercase tracking-wider text-ink-400">
              Effective
            </span>
            <ModeBadge mode={view.effective} large />
          </div>
        </div>

        {/* Mode switcher */}
        <div
          role="group"
          aria-label="Runtime mode"
          className="flex flex-wrap gap-2"
        >
          {MODES.map((m) => {
            const active = view.effective === m.value;
            const Icon = m.icon;
            return (
              <button
                key={m.value}
                type="button"
                disabled={busy}
                aria-pressed={active}
                title={MODE_BLURB[m.value]}
                onClick={() => choose(m.value)}
                className={clsx("btn", active && "btn-primary")}
              >
                <Icon size={13} aria-hidden />
                {m.label}
              </button>
            );
          })}
        </div>

        <p className="text-2xs text-ink-400">{MODE_BLURB[view.effective]}</p>
      </div>

      {pendingDetect && (
        <ConfirmDialog
          title="Arm detect mode?"
          tone="danger"
          confirmLabel="Arm detect"
          cancelLabel="Cancel"
          busy={setMode.isPending}
          error={detectRemedy ? null : setMode.error instanceof Error ? setMode.error : null}
          onClose={() => {
            if (!setMode.isPending) {
              setPendingDetect(false);
              setMode.reset();
            }
          }}
          onConfirm={() => {
            setMode.mutate(
              { mode: "detect", confirm: true },
              { onSuccess: () => setPendingDetect(false) },
            );
          }}
          message={
            detectRemedy ? (
              <div className="flex items-start gap-3 rounded-control border border-sev-warn/40 bg-sev-warn/10 p-3">
                <Sparkles
                  size={16}
                  className="mt-0.5 shrink-0 text-sev-warn"
                  aria-hidden
                />
                <div className="space-y-1.5">
                  <p className="font-medium text-ink-100">
                    AI is off — detect is blocked
                  </p>
                  <p className="text-ink-300">{detectRemedy}</p>
                  <button
                    type="button"
                    className="text-xs text-link hover:underline"
                    onClick={() => {
                      setPendingDetect(false);
                      setMode.reset();
                      scrollToAISettings();
                    }}
                  >
                    Go to AI settings
                  </button>
                </div>
              </div>
            ) : (
              <div className="space-y-2">
                <p>
                  <strong className="text-ink-100">Detect</strong> calls the AI
                  SRE and opens <strong className="text-ink-100">real
                  incidents and on-call pages</strong>. This is not a drill —
                  operators will be paged for what the agent flags.
                </p>
                <p>
                  Confirm only if you intend to arm live alerting now. You can
                  switch back to training or shadow at any time.
                </p>
              </div>
            )
          }
        />
      )}
    </ModeShell>
  );
}

// ModeShell — the consistent card chrome every state renders inside.
function ModeShell({ children }: { children: React.ReactNode }) {
  return (
    <div className="card mb-4">
      <div className="card-header">
        <h2 className="card-title">Runtime mode</h2>
        <span className="text-2xs text-ink-400">Enterprise control</span>
      </div>
      <div className="card-body">{children}</div>
    </div>
  );
}

// ModeBadge — icon + text chip (state never conveyed by color alone).
const MODE_TONE: Record<AgentMode, string> = {
  detect: "border-sev-ok/40 bg-sev-ok/15 text-sev-ok",
  shadow: "border-sev-warn/40 bg-sev-warn/15 text-sev-warn",
  training: "border-sev-info/40 bg-sev-info/15 text-sev-info",
};

function ModeBadge({ mode, large }: { mode: AgentMode; large?: boolean }) {
  const icon = MODES.find((m) => m.value === mode)?.icon ?? Radar;
  const Icon = icon;
  return (
    <span
      className={clsx(
        "inline-flex items-center gap-1 rounded-full border font-medium",
        large ? "px-2.5 py-1 text-xs" : "px-2 py-0.5 text-2xs",
        MODE_TONE[mode],
      )}
    >
      <Icon size={large ? 13 : 11} aria-hidden />
      {mode}
    </span>
  );
}

// LockedCard — the same Enterprise-only locked state the learned-signals
// views use (Lock glyph, upsell copy, Learn-more CTA), sized for an embedded
// card rather than a full page.
function LockedCard() {
  return (
    <ModeShell>
      <EnterpriseLockedBody title="Runtime mode control is an Enterprise capability">
        Switch the agent between training, shadow and detect at runtime —
        without editing YAML or restarting — and revert instantly. Available on
        Versus Enterprise.
      </EnterpriseLockedBody>
    </ModeShell>
  );
}

// scrollToAISettings brings the AI-settings control (rendered on the same
// /agent page) into view when the operator follows the detect-blocked remedy.
function scrollToAISettings() {
  const el = document.getElementById(AGENT_AI_SETTINGS_ANCHOR);
  if (el) {
    el.scrollIntoView({ behavior: "smooth", block: "center" });
  }
}

