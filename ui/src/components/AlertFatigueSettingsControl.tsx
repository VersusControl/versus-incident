import { useEffect, useMemo, useRef, useState } from "react";
import {
  useInfiniteQuery,
  useMutation,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import clsx from "clsx";
import {
  BellOff,
  ChevronDown,
  ChevronRight,
  Eye,
  EyeOff,
  Info,
  Loader2,
  Plus,
  Trash2,
} from "lucide-react";

import {
  ApiError,
  api,
  type AlertFatigueCorrelationGroup,
  type AlertFatigueCustomChannel,
  type AlertFatigueDependencyEdge,
} from "@/lib/api";
import {
  CUSTOM_CHANNEL_SPECS,
  CUSTOM_CHANNEL_TYPES,
  buildCustomChannelPut,
  canSaveCustomChannel,
  customChannelLabel,
  customFieldSetLabel,
  initialCustomChannelValues,
} from "@/lib/alertFatigueChannel";
import { displayService, fmtAbs, fmtRel } from "@/lib/format";
import { adminGateState } from "@/lib/role";
import { useEffectiveRole } from "@/lib/useEffectiveRole";
import { SeverityBadge } from "@/components/SeverityBadge";
import { ChannelIcon } from "@/components/ChannelIcon";
import { EmptyState } from "@/components/feedback";
import { EnterpriseLockedBody } from "@/components/EnterpriseLocked";
import { AdminAccessNotice } from "@/components/AdminAccessNotice";
import { SkRows } from "@/components/Skeleton";
import { useToast } from "@/components/toastContext";

// AlertFatigueSettingsControl — the Admin-page home for all alert-fatigue
// CONFIGURATION. It consolidates the fatigue-channel form, the same-service
// correlation config, and the dependency-aware suppression config into one
// self-gating card, mirroring the AgentAISettingsControl /
// AgentChannelsSettingsControl siblings.
//
// Gated exactly like those siblings on the caller's effective RBAC role
// (useEffectiveRole → adminGateState): community renders the Enterprise upsell,
// a signed-out operator is asked to sign in, a viewer/responder gets the
// read-only "requires admin" notice, and only admin/owner reach the live
// controls. Every endpoint is enterprise + runtime:manage gated server-side.
//
// The fatigue channel has one model: fatigued ("spam") alerts are SUPPRESSED
// (dropped) by default, and are only sent somewhere when an operator turns on
// "Send fatigued alerts to a channel" and configures the channel here.

const PAGE_SIZE = 50;

// PROXY_FIELD is pulled out of the per-channel field grid onto its own line: it
// is a switch that routes the fatigue channel through the server's shared proxy,
// not a value. When ON it reveals what "proxy" means (see ProxyReference).
const PROXY_FIELD = "use_proxy";

export function AlertFatigueSettingsControl() {
  const access = useEffectiveRole();
  const gate = adminGateState({
    loading: access.loading,
    enterprise: access.enterprise,
    hasSession: access.hasSession,
    isAdmin: access.isAdmin,
  });

  if (gate === "loading") {
    return (
      <Shell>
        <div className="flex items-center gap-2 text-xs text-ink-400">
          <Loader2 size={14} className="animate-spin" />
          Checking access…
        </div>
      </Shell>
    );
  }
  if (gate === "locked") {
    return (
      <Shell>
        <LockedBody />
      </Shell>
    );
  }
  if (gate === "sign-in") {
    return (
      <Shell>
        <AdminAccessNotice reason="sign-in" />
      </Shell>
    );
  }
  if (gate === "read-only") {
    return (
      <Shell>
        <AdminAccessNotice reason="role" />
      </Shell>
    );
  }

  return (
    <Shell>
      <SettingsBody />
    </Shell>
  );
}

// SettingsBody — the live control. The fatigue-channel form, correlation, and
// dependency sections each own their endpoint; there is no shared config read
// here anymore (the master Enable / Require-review toggles live on the
// AlertFatigue page).
function SettingsBody() {
  return (
    <div className="grid gap-4">
      <section data-testid="alert-fatigue-channel-config">
        <FatigueChannelForm />
      </section>

      <CorrelationSection />
      <DependencySection />

      <p className="text-2xs text-ink-400">
        Changes take effect on the next alert.
      </p>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Fatigue channel form
// ---------------------------------------------------------------------------

// FatigueChannelForm is the single "Send fatigued alerts to a channel" surface.
// Fatigued ("spam") alerts are SUPPRESSED (dropped) by default; only when the
// operator turns the toggle on and configures a channel are they sent there.
// The form picks a channel type and fills its fields (secrets masked, showing
// "set" when already stored and only re-sent when a new value is typed). A blank
// secret preserves the stored one — the UI never receives or re-renders a raw
// secret.
function FatigueChannelForm() {
  const qc = useQueryClient();
  const toast = useToast();

  const q = useQuery({
    queryKey: ["alert-fatigue-custom-channel"],
    queryFn: api.getAlertFatigueCustomChannel,
    retry: false,
  });

  if (q.isPending) {
    return (
      <div className="rounded-control border border-ink-600/60 bg-ink-700/30 p-3 text-2xs text-ink-400">
        <span className="inline-flex items-center gap-2">
          <Loader2 size={12} className="animate-spin" />
          Reading fatigue channel…
        </span>
      </div>
    );
  }
  if (q.isError || !q.data) {
    const s = q.error instanceof ApiError ? q.error.status : null;
    // 503: no Postgres backend — a fatigue channel cannot be stored, so fatigued
    // alerts are suppressed. Surface it as an informational note, not an error.
    if (s === 503) {
      return (
        <div
          className="rounded-control border border-ink-600/60 bg-ink-700/30 p-3 text-2xs text-ink-400"
          data-testid="alert-fatigue-custom-unavailable"
        >
          Sending fatigued alerts to a channel requires a Postgres backend.
          Fatigued (“spam”) alerts are currently suppressed and not sent
          anywhere.
        </div>
      );
    }
    return (
      <div className="flex items-center justify-between gap-3 rounded-control border border-ink-600/60 bg-ink-700/30 p-3 text-xs">
        <span className="text-sev-critical">
          {q.error instanceof Error
            ? q.error.message
            : "Couldn't read the fatigue channel."}
        </span>
        <button className="btn" onClick={() => q.refetch()}>
          Retry
        </button>
      </div>
    );
  }

  return (
    <FatigueChannelBody
      view={q.data}
      onSaved={(data) => {
        qc.setQueryData(["alert-fatigue-custom-channel"], data);
        toast.push({ title: "Fatigue channel saved", tone: "ok" });
      }}
      onDisabled={(data) => {
        qc.setQueryData(["alert-fatigue-custom-channel"], data);
        toast.push({ title: "Fatigued alerts suppressed", tone: "ok" });
      }}
      onCleared={() => {
        qc.invalidateQueries({ queryKey: ["alert-fatigue-custom-channel"] });
        toast.push({ title: "Fatigue channel cleared", tone: "ok" });
      }}
      onError={(msg) =>
        toast.push({ title: "Couldn't update fatigue channel", description: msg, tone: "error" })
      }
    />
  );
}

// FatigueChannelBody — the populated form. Split out so the local form state
// sits below the data-loading guard (no conditional hooks). Its secret inputs
// are transient React state, never persisted to browser storage.
//
// The header toggle IS the enable: sending = configured && enabled. Turning it
// OFF while a channel is configured PUTs enabled:false (preserving the saved
// config + secrets), so fatigued alerts are suppressed but the channel can be
// resumed later without re-entering the token. Fully removing the channel is the
// separate "Remove channel" (DELETE) action.
function FatigueChannelBody({
  view,
  onSaved,
  onDisabled,
  onCleared,
  onError,
}: {
  view: AlertFatigueCustomChannel;
  onSaved: (data: AlertFatigueCustomChannel) => void;
  onDisabled: (data: AlertFatigueCustomChannel) => void;
  onCleared: () => void;
  onError: (msg: string) => void;
}) {
  const configured = !!view.configured;
  const sending = configured && (view.enabled ?? false);
  const initialType = (configured && view.channel_type) || "slack";
  const [open, setOpen] = useState<boolean>(sending);
  const [channelType, setChannelType] = useState<string>(initialType);
  const [values, setValues] = useState<Record<string, string>>(() =>
    initialCustomChannelValues(initialType, view),
  );
  const [reveal, setReveal] = useState<Record<string, boolean>>({});

  // Seed the form on initial load and whenever the AUTHORITATIVE stored channel
  // type changes (a fresh save configures one, "Remove channel" clears it, or an
  // out-of-band change swaps it) — but NOT on every `view` update. A background
  // refetch (window refocus, query invalidation) returns an equivalent view with
  // the SAME stored type; re-seeding on that would wipe whatever the operator is
  // typing. The masked-secret "Set" hint reads from `view` at render time, so it
  // keeps tracking the latest stored state independent of this seed.
  const seededTypeRef = useRef<string | null>(null);
  useEffect(() => {
    const storedType = (view.configured && view.channel_type) || null;
    if (seededTypeRef.current === storedType) return;
    seededTypeRef.current = storedType;
    const t = storedType || channelType;
    setChannelType(t);
    setValues(initialCustomChannelValues(t, view));
    setReveal({});
    setOpen(!!view.configured && (view.enabled ?? false));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [view]);

  const save = useMutation({
    mutationFn: () =>
      api.setAlertFatigueCustomChannel(
        buildCustomChannelPut(channelType, true, values),
      ),
    onSuccess: (data) => onSaved(data),
    onError: (err) => onError(err instanceof Error ? err.message : String(err)),
  });

  // disable stops sending without discarding the saved channel: PUT
  // enabled:false with a blank config preserves every stored secret.
  const disable = useMutation({
    mutationFn: () =>
      api.setAlertFatigueCustomChannel({
        channel_type: view.channel_type ?? channelType,
        enabled: false,
        config: {},
      }),
    onSuccess: (data) => onDisabled(data),
    onError: (err) => onError(err instanceof Error ? err.message : String(err)),
  });

  const clear = useMutation({
    mutationFn: () => api.deleteAlertFatigueCustomChannel(),
    onSuccess: () => onCleared(),
    onError: (err) => onError(err instanceof Error ? err.message : String(err)),
  });

  const busy = save.isPending || disable.isPending || clear.isPending;
  const spec = CUSTOM_CHANNEL_SPECS[channelType] ?? [];
  const sameType = configured && view.channel_type === channelType;
  const canSave = !busy && canSaveCustomChannel(channelType, values, view);

  // "Use proxy" renders on its OWN line below the field grid (not inline), and
  // reveals the proxy reference when checked. gridFields is every other field;
  // proxyField is the toggle when this channel type supports it.
  const proxyField = spec.find((f) => f.name === PROXY_FIELD);
  const gridFields = spec.filter((f) => f.name !== PROXY_FIELD);
  const proxyOn = values[PROXY_FIELD] === "true";

  // The header toggle: turning ON reveals the form (and resumes a saved-but-
  // disabled channel by keeping it open for Save); turning OFF suppresses.
  const onToggle = () => {
    if (busy) return;
    const next = !open;
    setOpen(next);
    if (!next && configured) {
      // Was sending → stop sending, preserve the saved config.
      disable.mutate();
    }
  };

  const onTypeChange = (t: string) => {
    setChannelType(t);
    setValues(initialCustomChannelValues(t, view));
    setReveal({});
  };

  return (
    <div
      className="rounded-control border border-ink-600/60 bg-ink-700/30 p-4"
      data-testid="alert-fatigue-custom-channel"
    >
      <div className="flex flex-wrap items-start gap-3">
        <button
          type="button"
          role="switch"
          aria-checked={open}
          aria-label="Send fatigued alerts to a channel"
          disabled={busy}
          data-testid="alert-fatigue-send-toggle"
          onClick={onToggle}
          className={`relative inline-flex h-5 w-9 shrink-0 items-center rounded-full transition ${
            open ? "bg-link" : "bg-ink-600"
          } ${busy ? "opacity-70" : ""}`}
        >
          <span
            className={`inline-block h-4 w-4 transform rounded-full bg-white transition ${
              open ? "translate-x-4" : "translate-x-0.5"
            }`}
          />
        </button>
        <div className="min-w-0 max-w-2xl">
          <div className="flex items-center gap-2 text-xs font-semibold text-ink-100">
            <BellOff size={14} className="text-link" aria-hidden />
            Send fatigued alerts to a channel
          </div>
          {!open && (
            <div
              className="mt-0.5 text-2xs text-ink-400"
              data-testid="alert-fatigue-suppress-note"
            >
              When off, fatigued (“spam”) alerts are silently suppressed and not
              sent anywhere.
            </div>
          )}
        </div>
      </div>

      {open && (
        <div className="mt-4 grid gap-4 border-t border-ink-700 pt-4">
          {configured && (
            <div
              className="inline-flex w-fit items-center gap-1.5 rounded-full border border-link/30 bg-link/10 px-2.5 py-1 text-2xs text-ink-200"
              data-testid="alert-fatigue-custom-status"
            >
              <BellOff size={11} aria-hidden className="text-link" />
              {view.enabled ? "Channel active" : "Channel set (suppressed)"} —{" "}
              {customChannelLabel(view.channel_type ?? "")}
            </div>
          )}

          <div>
            <span className="field-label">Channel type</span>
            <div
              role="tablist"
              aria-label="Fatigue channel type"
              data-testid="alert-fatigue-custom-type"
              className="mt-1 flex flex-wrap gap-1 border-b border-ink-600"
            >
              {CUSTOM_CHANNEL_TYPES.map((t) => {
                const active = t === channelType;
                const isConfigured = configured && view.channel_type === t;
                return (
                  <button
                    key={t}
                    type="button"
                    role="tab"
                    aria-selected={active}
                    disabled={busy}
                    onClick={() => onTypeChange(t)}
                    className={clsx(
                      "inline-flex items-center gap-1.5 rounded-t-control border-b-2 px-3 py-2 text-xs font-medium",
                      active
                        ? "border-link text-ink-50"
                        : "border-transparent text-ink-300 hover:text-ink-100",
                    )}
                  >
                    <ChannelIcon id={t} size={13} />
                    {customChannelLabel(t)}
                    {isConfigured && (
                      <span
                        className="h-1.5 w-1.5 rounded-full bg-link"
                        aria-label="configured channel"
                      />
                    )}
                  </button>
                );
              })}
            </div>
          </div>

          {/* Field grid (one schema drives every type) */}
          <div className="grid gap-2 sm:grid-cols-2">
            {gridFields.map((f) => {
              const inputId = `alert-fatigue-custom-field-${f.name}`;
              if (f.bool) {
                return (
                  <label
                    key={f.name}
                    htmlFor={inputId}
                    className="flex cursor-pointer items-center gap-2 text-xs text-ink-200"
                  >
                    <input
                      id={inputId}
                    data-testid={inputId}
                    type="checkbox"
                    checked={values[f.name] === "true"}
                    disabled={busy}
                    onChange={(e) =>
                      setValues((v) => ({
                        ...v,
                        [f.name]: e.target.checked ? "true" : "false",
                      }))
                    }
                    className="h-4 w-4 accent-link"
                  />
                  {f.label}
                </label>
              );
            }
            const stored = sameType ? view.fields?.[f.name] : undefined;
            const revealed = !!reveal[f.name];
            return (
              <div key={f.name} className="min-w-0">
                <label className="field-label" htmlFor={inputId}>
                  {f.label}
                  {f.required && (
                    <span className="ml-1 text-sev-critical" aria-hidden>
                      *
                    </span>
                  )}
                  {f.secret && (
                    <span className="ml-1 font-normal text-ink-400">
                      ({customFieldSetLabel(!!stored?.set, stored?.hint ?? "")})
                    </span>
                  )}
                </label>
                <div className="relative">
                  <input
                    id={inputId}
                    data-testid={inputId}
                    type={f.secret && !revealed ? "password" : "text"}
                    autoComplete="off"
                    placeholder={
                      f.secret && stored?.set ? "•••• stored — blank keeps it" : ""
                    }
                    value={values[f.name] ?? ""}
                    disabled={busy}
                    aria-label={f.label}
                    onChange={(e) =>
                      setValues((v) => ({ ...v, [f.name]: e.target.value }))
                    }
                    className={clsx("input h-9 text-sm", f.secret && "pr-9")}
                  />
                  {f.secret && (
                    <button
                      type="button"
                      aria-label={revealed ? `Hide ${f.label}` : `Show ${f.label}`}
                      aria-pressed={revealed}
                      className="absolute right-1 top-1/2 -translate-y-1/2 rounded-control p-1.5 text-ink-300 hover:bg-ink-600 hover:text-ink-100"
                      onClick={() =>
                        setReveal((r) => ({ ...r, [f.name]: !r[f.name] }))
                      }
                    >
                      {revealed ? (
                        <EyeOff size={13} aria-hidden />
                      ) : (
                        <Eye size={13} aria-hidden />
                      )}
                    </button>
                  )}
                </div>
              </div>
            );
          })}
        </div>

          {/* Use proxy — on its OWN line, not inline in the grid. When checked
              it reveals the proxy reference (below). Only proxy-capable types
              (telegram/lark/viber) carry the field. */}
          {proxyField && (
            <div className="mt-3 border-t border-ink-600/60 pt-3">
              <label
                htmlFor={`alert-fatigue-custom-field-${PROXY_FIELD}`}
                className="flex w-fit cursor-pointer items-center gap-2 text-xs text-ink-200"
              >
                <input
                  id={`alert-fatigue-custom-field-${PROXY_FIELD}`}
                  data-testid={`alert-fatigue-custom-field-${PROXY_FIELD}`}
                  type="checkbox"
                  checked={proxyOn}
                  disabled={busy}
                  onChange={(e) =>
                    setValues((v) => ({
                      ...v,
                      [PROXY_FIELD]: e.target.checked ? "true" : "false",
                    }))
                  }
                  className="h-4 w-4 accent-link"
                />
                {proxyField.label}
              </label>
              {proxyOn && <ProxyReference />}
            </div>
          )}

          <div className="mt-3 flex flex-wrap gap-2">
            <button
              type="button"
              className="btn btn-primary"
              data-testid="alert-fatigue-custom-save"
              disabled={!canSave}
              onClick={() => save.mutate()}
            >
              {save.isPending ? "Saving…" : "Save channel"}
            </button>
            {configured && (
              <button
                type="button"
                className="btn"
                data-testid="alert-fatigue-custom-clear"
                disabled={busy}
                onClick={() => clear.mutate()}
                title="Remove the fatigue channel — fatigued alerts are suppressed"
              >
                Remove channel
              </button>
            )}
          </div>
        </div>
      )}
    </div>
  );
}

// ProxyReference is the read-only reveal shown when the fatigue channel's "Use
// proxy" is on. Like the notification channels, the fatigue channel carries only
// the use_proxy BOOLEAN — it routes through the deployment-level `proxy:` config
// (URL / username / password), shared by every channel with proxy on and set at
// deploy time. So this reveals WHERE the proxy settings live and that they are
// not editable at runtime, rather than fabricating per-channel proxy fields.
function ProxyReference() {
  return (
    <div className="mt-2 flex items-start gap-2 rounded-control border border-ink-600/60 bg-ink-800/40 p-2.5 text-2xs text-ink-300">
      <Info size={13} className="mt-0.5 shrink-0 text-ink-400" aria-hidden />
      <div className="space-y-1">
        <p className="text-ink-200">
          This channel sends through the server's shared proxy.
        </p>
        <p>
          The proxy endpoint and credentials come from the deployment-level{" "}
          <code className="rounded bg-ink-700/70 px-1 font-mono text-ink-100">
            proxy:
          </code>{" "}
          config (URL, username, password), applied to every channel with proxy
          turned on. It is set at deploy time and is not editable here.
        </p>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Shared section building blocks (moved from AlertFatiguePage)
// ---------------------------------------------------------------------------

// SwitchToggle is the role="switch" control reused by the correlation and
// dependency sections.
function SwitchToggle({
  checked,
  onToggle,
  disabled,
  label,
  testId,
}: {
  checked: boolean;
  onToggle: () => void;
  disabled?: boolean;
  label: string;
  testId?: string;
}) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={label}
      disabled={disabled}
      data-testid={testId}
      onClick={onToggle}
      className={`relative inline-flex h-5 w-9 shrink-0 items-center rounded-full transition ${
        checked ? "bg-link" : "bg-ink-600"
      } ${disabled ? "opacity-70" : ""}`}
    >
      <span
        className={`inline-block h-4 w-4 transform rounded-full bg-white transition ${
          checked ? "translate-x-4" : "translate-x-0.5"
        }`}
      />
    </button>
  );
}

// SecondsSetting is a labelled number input (seconds) with an Apply button and a
// read-only "effective" echo of the value the interceptor actually applies.
function SecondsSetting({
  label,
  help,
  stored,
  effective,
  onApply,
  saving,
  inputTestId,
  applyTestId,
}: {
  label: string;
  help: string;
  stored: number;
  effective: number;
  onApply: (seconds: number) => void;
  saving: boolean;
  inputTestId?: string;
  applyTestId?: string;
}) {
  const [raw, setRaw] = useState(stored ? String(stored) : "");
  const parsed = Number(raw);
  const valid = raw.trim() !== "" && Number.isInteger(parsed) && parsed > 0;
  const changed = valid && parsed !== stored;

  return (
    <div className="flex flex-wrap items-end gap-3">
      <div>
        <label className="field-label" htmlFor={inputTestId}>
          {label}
        </label>
        <div className="max-w-md text-2xs text-ink-400">{help}</div>
      </div>
      <input
        id={inputTestId}
        data-testid={inputTestId}
        type="number"
        min={1}
        className="input h-9 w-32 text-sm"
        value={raw}
        disabled={saving}
        placeholder={String(effective)}
        onChange={(e) => setRaw(e.target.value)}
      />
      <button
        type="button"
        className="btn"
        data-testid={applyTestId}
        disabled={saving || !changed}
        onClick={() => valid && onApply(parsed)}
      >
        Apply
      </button>
      <span className="text-2xs text-ink-400">
        Effective:{" "}
        <span className="tabular-nums text-ink-200">{effective}s</span>
      </span>
    </div>
  );
}

// SectionShell is the card + heading wrapper the correlation / dependency
// sections share. It mirrors the AgentChannelsSettingsControl inner-card
// treatment (subtle bordered panel, medium-weight title) so the two Enterprise
// settings surfaces read consistently.
function SectionShell({
  title,
  icon,
  children,
  testId,
}: {
  title: string;
  icon?: React.ReactNode;
  children: React.ReactNode;
  testId?: string;
}) {
  return (
    <div
      className="rounded-control border border-ink-600/60 bg-ink-700/30 p-4"
      data-testid={testId}
    >
      <h2 className="mb-3 flex items-center gap-2 text-sm font-medium text-ink-100">
        {icon}
        {title}
      </h2>
      {children}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Correlation section (same-service grouping)
// ---------------------------------------------------------------------------

function CorrelationSection() {
  const qc = useQueryClient();
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);

  const corr = useQuery({
    queryKey: ["alert-fatigue-correlation"],
    queryFn: api.getAlertFatigueCorrelation,
    retry: false,
  });

  const save = useMutation({
    mutationFn: (body: {
      correlation_enabled: boolean;
      correlation_window_seconds: number;
    }) => api.setAlertFatigueCorrelation(body),
    onSuccess: (data) => {
      qc.setQueryData(["alert-fatigue-correlation"], data);
      setMsg(null);
    },
    onError: (err: unknown) => {
      setMsg({
        ok: false,
        text:
          err instanceof ApiError ? err.message : "Could not update correlation",
      });
    },
  });

  if (corr.isPending) {
    return (
      <SectionShell
        title="Alert grouping"
        testId="alert-fatigue-correlation"
      >
        <div className="flex items-center gap-2 text-xs text-ink-400">
          <Loader2 size={14} className="animate-spin" />
          Reading correlation settings…
        </div>
      </SectionShell>
    );
  }
  if (corr.isError || !corr.data) {
    return (
      <SectionShell
        title="Alert grouping"
        testId="alert-fatigue-correlation"
      >
        <div className="flex items-center justify-between gap-3 text-xs">
          <span className="text-sev-critical">
            {corr.error instanceof Error
              ? corr.error.message
              : "Couldn't read correlation settings."}
          </span>
          <button className="btn" onClick={() => corr.refetch()}>
            Retry
          </button>
        </div>
      </SectionShell>
    );
  }

  const c = corr.data;
  const busy = save.isPending;

  return (
    <SectionShell
      title="Alert grouping"
      testId="alert-fatigue-correlation"
    >
      <div className="flex flex-wrap items-start gap-3">
        <SwitchToggle
          checked={c.correlation_enabled}
          disabled={busy}
          label="Enable same-service correlation"
          testId="alert-fatigue-correlation-toggle"
          onToggle={() =>
            save.mutate({
              correlation_enabled: !c.correlation_enabled,
              correlation_window_seconds: c.correlation_window_seconds,
            })
          }
        />
        <div className="min-w-0 max-w-2xl">
          <div className="text-xs font-semibold text-ink-100">
            Fold a storm of same-service alerts into one parent
          </div>
          <div className="text-2xs text-ink-400">
            Off by default. The first alert for a service still pages; later
            same-service alerts inside the window fold in as members and do not
            page. Critical/high-priority alerts are never grouped.
          </div>
        </div>
      </div>

      {c.correlation_enabled && (
        <div className="mt-4 grid gap-4 border-t border-ink-700 pt-4">
          <SecondsSetting
            label="Correlation window"
            help="How long after the first same-service alert later alerts fold into the parent group."
            stored={c.correlation_window_seconds}
            effective={c.effective_window_seconds}
            saving={busy}
            inputTestId="alert-fatigue-correlation-window"
            applyTestId="alert-fatigue-correlation-window-apply"
            onApply={(seconds) =>
              save.mutate({
                correlation_enabled: true,
                correlation_window_seconds: seconds,
              })
            }
          />
          <CorrelationGroupsList />
        </div>
      )}

      {msg && (
        <div
          className={`mt-3 text-2xs ${
            msg.ok ? "text-sev-ok" : "text-sev-critical"
          }`}
          role="status"
        >
          {msg.text}
        </div>
      )}
    </SectionShell>
  );
}

function CorrelationGroupsList() {
  const q = useInfiniteQuery({
    queryKey: ["alert-fatigue-correlation-groups"],
    queryFn: ({ pageParam }) =>
      api.listAlertFatigueCorrelationGroups({
        page: pageParam,
        pageSize: PAGE_SIZE,
      }),
    initialPageParam: 1,
    getNextPageParam: (last) =>
      last.page * last.page_size < last.total ? last.page + 1 : undefined,
  });

  const groups = useMemo<AlertFatigueCorrelationGroup[]>(
    () => q.data?.pages.flatMap((p) => p.groups) ?? [],
    [q.data],
  );
  const total = q.data?.pages[0]?.total;

  return (
    <div
      className="overflow-hidden rounded-md border border-ink-700"
      data-testid="alert-fatigue-correlation-groups"
    >
      <div className="border-b border-ink-700 px-3 py-2 text-2xs font-semibold uppercase tracking-wide text-ink-400">
        Correlation groups
        {total !== undefined && (
          <span className="ml-2 font-normal normal-case text-ink-500">
            {total.toLocaleString()} total
          </span>
        )}
      </div>
      {q.isError ? (
        <div className="flex items-center justify-between gap-3 p-3 text-xs">
          <span className="text-sev-critical">
            {q.error instanceof Error ? q.error.message : "Couldn't load groups."}
          </span>
          <button className="btn" onClick={() => q.refetch()}>
            Retry
          </button>
        </div>
      ) : q.isPending ? (
        <table className="ddt">
          <tbody>
            <SkRows rows={3} cols={1} />
          </tbody>
        </table>
      ) : groups.length === 0 ? (
        <EmptyState
          title="No correlation groups yet"
          hint="Same-service storms appear here as the interceptor folds them into a parent."
        />
      ) : (
        <>
          <table className="ddt">
            <thead>
              <tr>
                <th className="w-8" />
                <th>Service</th>
                <th className="w-24">Severity</th>
                <th className="w-20 text-right">Members</th>
                <th className="w-40">Window</th>
              </tr>
            </thead>
            <tbody>
              {groups.map((g) => (
                <CorrelationGroupRow key={g.id} group={g} />
              ))}
            </tbody>
          </table>
          {q.hasNextPage && (
            <div className="border-t border-ink-700 px-3 py-2 text-center">
              <button
                type="button"
                className="btn"
                data-testid="alert-fatigue-correlation-groups-more"
                disabled={q.isFetchingNextPage}
                onClick={() => q.fetchNextPage()}
              >
                {q.isFetchingNextPage ? "Loading…" : "Load more"}
              </button>
            </div>
          )}
        </>
      )}
    </div>
  );
}

function CorrelationGroupRow({ group }: { group: AlertFatigueCorrelationGroup }) {
  const [open, setOpen] = useState(false);
  const members = useQuery({
    queryKey: ["alert-fatigue-correlation-members", group.id],
    queryFn: () => api.listAlertFatigueCorrelationMembers(group.id),
    enabled: open,
    retry: false,
  });

  return (
    <>
      <tr>
        <td>
          <button
            type="button"
            className="text-ink-400 hover:text-link"
            aria-expanded={open}
            aria-label={open ? "Collapse members" : "Expand members"}
            data-testid={`alert-fatigue-group-expand-${group.id}`}
            onClick={() => setOpen((v) => !v)}
          >
            {open ? (
              <ChevronDown size={14} aria-hidden />
            ) : (
              <ChevronRight size={14} aria-hidden />
            )}
          </button>
        </td>
        <td className="font-medium text-ink-100">
          {displayService(group.service)}
        </td>
        <td>
          <SeverityBadge severity={group.parent_severity} />
        </td>
        <td className="text-right tabular-nums text-ink-200">
          {group.member_count}
        </td>
        <td className="text-2xs text-ink-300" title={fmtAbs(group.window_start)}>
          {fmtRel(group.window_start)} → {fmtRel(group.window_end)}
        </td>
      </tr>
      {open && (
        <tr>
          <td colSpan={5} className="bg-ink-950/30">
            {members.isPending ? (
              <div className="flex items-center gap-2 px-3 py-2 text-2xs text-ink-400">
                <Loader2 size={12} className="animate-spin" />
                Loading members…
              </div>
            ) : members.isError ? (
              <div className="px-3 py-2 text-2xs text-sev-critical">
                {members.error instanceof Error
                  ? members.error.message
                  : "Couldn't load members."}
              </div>
            ) : members.data.members.length === 0 ? (
              <div className="px-3 py-2 text-2xs text-ink-400">
                No folded members.
              </div>
            ) : (
              <ul
                className="grid gap-1 px-3 py-2"
                data-testid={`alert-fatigue-group-members-${group.id}`}
              >
                {members.data.members.map((m) => (
                  <li
                    key={m.id}
                    className="flex items-center gap-2 text-2xs text-ink-300"
                  >
                    <SeverityBadge severity={m.child_severity} />
                    <span className="break-all font-mono text-ink-400">
                      {m.child_fingerprint}
                    </span>
                    <span
                      className="ml-auto shrink-0 text-ink-500"
                      title={fmtAbs(m.created_at)}
                    >
                      {fmtRel(m.created_at)}
                    </span>
                  </li>
                ))}
              </ul>
            )}
          </td>
        </tr>
      )}
    </>
  );
}

// ---------------------------------------------------------------------------
// Dependency-suppression section
// ---------------------------------------------------------------------------

function DependencySection() {
  const qc = useQueryClient();
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);

  const dep = useQuery({
    queryKey: ["alert-fatigue-dependency"],
    queryFn: api.getAlertFatigueDependency,
    retry: false,
  });

  const save = useMutation({
    mutationFn: (body: {
      dependency_suppress_enabled: boolean;
      dependency_lookback_seconds: number;
    }) => api.setAlertFatigueDependency(body),
    onSuccess: (data) => {
      qc.setQueryData(["alert-fatigue-dependency"], data);
      setMsg(null);
    },
    onError: (err: unknown) => {
      setMsg({
        ok: false,
        text:
          err instanceof ApiError ? err.message : "Could not update dependency",
      });
    },
  });

  if (dep.isPending) {
    return (
      <SectionShell
        title="Downstream suppression"
        testId="alert-fatigue-dependency"
      >
        <div className="flex items-center gap-2 text-xs text-ink-400">
          <Loader2 size={14} className="animate-spin" />
          Reading dependency settings…
        </div>
      </SectionShell>
    );
  }
  if (dep.isError || !dep.data) {
    return (
      <SectionShell
        title="Downstream suppression"
        testId="alert-fatigue-dependency"
      >
        <div className="flex items-center justify-between gap-3 text-xs">
          <span className="text-sev-critical">
            {dep.error instanceof Error
              ? dep.error.message
              : "Couldn't read dependency settings."}
          </span>
          <button className="btn" onClick={() => dep.refetch()}>
            Retry
          </button>
        </div>
      </SectionShell>
    );
  }

  const d = dep.data;
  const busy = save.isPending;

  return (
    <SectionShell
      title="Downstream suppression"
      testId="alert-fatigue-dependency"
    >
      <div className="flex flex-wrap items-start gap-3">
        <SwitchToggle
          checked={d.dependency_suppress_enabled}
          disabled={busy}
          label="Enable dependency-aware suppression"
          testId="alert-fatigue-dependency-toggle"
          onToggle={() =>
            save.mutate({
              dependency_suppress_enabled: !d.dependency_suppress_enabled,
              dependency_lookback_seconds: d.dependency_lookback_seconds,
            })
          }
        />
        <div className="min-w-0 max-w-2xl">
          <div className="text-xs font-semibold text-ink-100">
            Hold downstream symptoms while an upstream cause is firing
          </div>
          <div className="text-2xs text-ink-400">
            Off by default. When a declared downstream service pages while its
            upstream has an open incident in the lookback window, the symptom is
            held (diverted) and released automatically when the cause clears. A
            cause and any escalation always page.
          </div>
        </div>
      </div>

      {d.dependency_suppress_enabled && (
        <div className="mt-4 grid gap-4 border-t border-ink-700 pt-4">
          <SecondsSetting
            label="Open-incident lookback"
            help="How far back an open upstream incident counts as an active cause for holding downstream symptoms."
            stored={d.dependency_lookback_seconds}
            effective={d.effective_lookback_seconds}
            saving={busy}
            inputTestId="alert-fatigue-dependency-lookback"
            applyTestId="alert-fatigue-dependency-lookback-apply"
            onApply={(seconds) =>
              save.mutate({
                dependency_suppress_enabled: true,
                dependency_lookback_seconds: seconds,
              })
            }
          />
          <DependencyEdgeEditor />
        </div>
      )}

      {msg && (
        <div
          className={`mt-3 text-2xs ${
            msg.ok ? "text-sev-ok" : "text-sev-critical"
          }`}
          role="status"
        >
          {msg.text}
        </div>
      )}
    </SectionShell>
  );
}

function DependencyEdgeEditor() {
  const qc = useQueryClient();
  const [downstream, setDownstream] = useState("");
  const [upstream, setUpstream] = useState("");
  const [msg, setMsg] = useState<string | null>(null);

  const q = useInfiniteQuery({
    queryKey: ["alert-fatigue-dependency-edges"],
    queryFn: ({ pageParam }) =>
      api.listAlertFatigueDependencyEdges({
        page: pageParam,
        pageSize: PAGE_SIZE,
      }),
    initialPageParam: 1,
    getNextPageParam: (last) =>
      last.page * last.page_size < last.total ? last.page + 1 : undefined,
  });

  const edges = useMemo<AlertFatigueDependencyEdge[]>(
    () => q.data?.pages.flatMap((p) => p.edges) ?? [],
    [q.data],
  );
  const total = q.data?.pages[0]?.total;
  const invalidate = () =>
    qc.invalidateQueries({ queryKey: ["alert-fatigue-dependency-edges"] });

  const add = useMutation({
    mutationFn: (body: { downstream: string; upstream: string }) =>
      api.addAlertFatigueDependencyEdge(body),
    onSuccess: () => {
      setDownstream("");
      setUpstream("");
      setMsg(null);
      invalidate();
    },
    onError: (err: unknown) =>
      setMsg(err instanceof ApiError ? err.message : "Could not add edge"),
  });
  const remove = useMutation({
    mutationFn: (id: number) => api.removeAlertFatigueDependencyEdge(id),
    onSuccess: invalidate,
    onError: (err: unknown) =>
      setMsg(err instanceof ApiError ? err.message : "Could not remove edge"),
  });

  const canAdd =
    downstream.trim() !== "" &&
    upstream.trim() !== "" &&
    downstream.trim().toLowerCase() !== upstream.trim().toLowerCase() &&
    !add.isPending;

  const submit = () => {
    if (!canAdd) return;
    add.mutate({ downstream: downstream.trim(), upstream: upstream.trim() });
  };

  return (
    <div
      className="overflow-hidden rounded-md border border-ink-700"
      data-testid="alert-fatigue-dependency-edges"
    >
      <div className="border-b border-ink-700 px-3 py-2 text-2xs font-semibold uppercase tracking-wide text-ink-400">
        Dependency map
        {total !== undefined && (
          <span className="ml-2 font-normal normal-case text-ink-500">
            {total.toLocaleString()} edge{total === 1 ? "" : "s"}
          </span>
        )}
      </div>

      <div className="flex flex-wrap items-end gap-2 border-b border-ink-700 px-3 py-3">
        <div>
          <label className="field-label" htmlFor="alert-fatigue-edge-downstream">
            Downstream
          </label>
          <input
            id="alert-fatigue-edge-downstream"
            data-testid="alert-fatigue-edge-downstream"
            className="input h-8 w-40 text-sm"
            placeholder="e.g. checkout"
            value={downstream}
            disabled={add.isPending}
            onChange={(e) => setDownstream(e.target.value)}
          />
        </div>
        <span className="pb-1.5 text-2xs text-ink-400">depends on</span>
        <div>
          <label className="field-label" htmlFor="alert-fatigue-edge-upstream">
            Upstream
          </label>
          <input
            id="alert-fatigue-edge-upstream"
            data-testid="alert-fatigue-edge-upstream"
            className="input h-8 w-40 text-sm"
            placeholder="e.g. postgres"
            value={upstream}
            disabled={add.isPending}
            onChange={(e) => setUpstream(e.target.value)}
          />
        </div>
        <button
          type="button"
          className="btn inline-flex items-center gap-1"
          data-testid="alert-fatigue-edge-add"
          disabled={!canAdd}
          onClick={submit}
        >
          <Plus size={12} aria-hidden />
          Add edge
        </button>
      </div>

      {msg && (
        <div className="px-3 py-2 text-2xs text-sev-critical" role="alert">
          {msg}
        </div>
      )}

      {q.isError ? (
        <div className="flex items-center justify-between gap-3 p-3 text-xs">
          <span className="text-sev-critical">
            {q.error instanceof Error ? q.error.message : "Couldn't load edges."}
          </span>
          <button className="btn" onClick={() => q.refetch()}>
            Retry
          </button>
        </div>
      ) : q.isPending ? (
        <table className="ddt">
          <tbody>
            <SkRows rows={2} cols={1} />
          </tbody>
        </table>
      ) : edges.length === 0 ? (
        <EmptyState
          title="No dependency edges yet"
          hint="Declare which services depend on which so their symptom pages are held behind the real cause."
        />
      ) : (
        <>
          <table className="ddt">
            <thead>
              <tr>
                <th>Downstream</th>
                <th className="w-8" />
                <th>Upstream</th>
                <th className="w-32">Added</th>
                <th className="w-16 text-right" />
              </tr>
            </thead>
            <tbody>
              {edges.map((e) => (
                <tr key={e.id}>
                  <td className="font-medium text-ink-100">
                    {displayService(e.downstream)}
                  </td>
                  <td className="text-2xs text-ink-500">→</td>
                  <td className="text-ink-200">{displayService(e.upstream)}</td>
                  <td className="text-2xs text-ink-300" title={fmtAbs(e.created_at)}>
                    {fmtRel(e.created_at)}
                  </td>
                  <td className="text-right">
                    <button
                      type="button"
                      className="btn px-2 py-1 text-2xs"
                      data-testid={`alert-fatigue-edge-remove-${e.id}`}
                      aria-label={`Remove edge ${e.downstream} depends on ${e.upstream}`}
                      disabled={remove.isPending}
                      onClick={() => remove.mutate(e.id)}
                    >
                      <Trash2 size={12} aria-hidden />
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {q.hasNextPage && (
            <div className="border-t border-ink-700 px-3 py-2 text-center">
              <button
                type="button"
                className="btn"
                data-testid="alert-fatigue-edges-more"
                disabled={q.isFetchingNextPage}
                onClick={() => q.fetchNextPage()}
              >
                {q.isFetchingNextPage ? "Loading…" : "Load more"}
              </button>
            </div>
          )}
        </>
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Card chrome + locked upsell
// ---------------------------------------------------------------------------

function Shell({ children }: { children: React.ReactNode }) {
  return (
    <div id="alert-fatigue-settings" className="card mb-4 scroll-mt-4">
      <div className="card-header">
        <h2 className="card-title">Alert fatigue settings</h2>
        <span className="text-2xs text-ink-400">Enterprise control</span>
      </div>
      <div className="card-body">{children}</div>
    </div>
  );
}

function LockedBody() {
  return (
    <EnterpriseLockedBody title="Alert fatigue settings are an Enterprise capability">
      Configure where fatigued (&ldquo;spam&rdquo;) alerts are diverted — a named
      channel or a dedicated custom channel that overrides it — plus same-service
      correlation and dependency-aware suppression, all at runtime without
      editing YAML. Available on Versus Enterprise.
    </EnterpriseLockedBody>
  );
}
