import { useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import clsx from "clsx";
import {
  AlertCircle,
  Eye,
  EyeOff,
  Info,
  Loader2,
  Radio,
  Send,
} from "lucide-react";
import {
  ApiError,
  api,
  type ChannelSettingsMap,
  type ChannelSettingsView,
} from "@/lib/api";
import {
  CHANNELS,
  CHANNEL_SCHEMA,
  buildChannelPut,
  channelLabel,
  fieldSetLabel,
  initialChannelValues,
  noEncryptionKeyMessage,
  type ChannelName,
} from "@/lib/agentChannels";
import { ChannelIcon } from "@/components/ChannelIcon";
import { AdminAccessNotice } from "@/components/AdminAccessNotice";
import { EnterpriseLockedBody } from "@/components/EnterpriseLocked";
import { useEffectiveRole } from "@/lib/useEffectiveRole";
import { adminGateState } from "@/lib/role";
import { useToast } from "@/components/toastContext";

// AgentChannelsSettingsControl — the operator surface for the Enterprise
// runtime notification-channel config override (Item 2, the runtimeai sibling).
// It reads /enterprise/api/agent/channel-settings on mount. The six channels
// are presented TABBED — one segment per channel (icon + name), and only the
// active channel's data-driven form is shown (one field schema drives all six),
// each with an enable toggle, masked write-only secret inputs, and
// Save / Clear / Test.
//
// Like the AI-settings control, every request rides the SSO session cookie and
// is authorized by the caller's RBAC role (runtime:manage). The surface gates
// on useEffectiveRole:
//   not enterprise        → locked Enterprise upsell (no control)
//   no SSO session         → "sign in to manage" notice
//   viewer / responder     → read-only "requires the admin role" notice
//   admin / owner          → the live channel controls
//   422 no_encryption_key  → server master key not set (actionable, admin only)
//
// A secret is NEVER persisted to browser storage. It lives transiently in React
// state, is sent on the single PUT (blank preserves the stored one), then
// cleared. The server never returns a secret — only a masked hint.

// PROXY_FIELD is the one field pulled out of the per-channel field grid onto its
// own line: it is not a value but a switch that routes the channel through the
// server's shared proxy. When ON it REVEALS what "proxy" means (see
// ProxyReference). The runtime channel store carries only this boolean
// (runtimechannels kindBool) pointing at the deployment-level `proxy:` config —
// there are NO per-channel runtime proxy fields — so the reveal is a read-only
// reference to that global config, not editable fields.
const PROXY_FIELD = "use_proxy";

export function AgentChannelsSettingsControl() {
  const qc = useQueryClient();
  const toast = useToast();
  const access = useEffectiveRole();
  // The panel is TABBED — one segment per channel, only the active channel's
  // form is shown (never all six stacked). The active tab is local UI state.
  const [activeChannel, setActiveChannel] = useState<ChannelName>(CHANNELS[0]);
  const gate = adminGateState({
    loading: access.loading,
    enterprise: access.enterprise,
    hasSession: access.hasSession,
    isAdmin: access.isAdmin,
  });

  const settings = useQuery<ChannelSettingsMap>({
    queryKey: ["agent-channel-settings"],
    queryFn: api.getChannelSettings,
    enabled: gate === "admin",
    retry: (count, err) => {
      if (err instanceof ApiError && [401, 403, 404, 503].includes(err.status)) {
        return false;
      }
      return count < 1;
    },
  });

  const status = settings.error instanceof ApiError ? settings.error.status : null;

  if (gate === "loading") {
    return (
      <ChannelsShell>
        <div className="flex items-center gap-2 text-xs text-ink-400">
          <Loader2 size={14} className="animate-spin" />
          Checking access…
        </div>
      </ChannelsShell>
    );
  }
  if (gate === "locked") {
    return <ChannelsShell><LockedBody /></ChannelsShell>;
  }
  if (gate === "sign-in") {
    return <ChannelsShell><AdminAccessNotice reason="sign-in" /></ChannelsShell>;
  }
  if (gate === "read-only") {
    return <ChannelsShell><AdminAccessNotice reason="role" /></ChannelsShell>;
  }
  if (status === 403 || status === 404) {
    return <ChannelsShell><LockedBody /></ChannelsShell>;
  }
  if (settings.isPending) {
    return (
      <ChannelsShell>
        <div className="flex items-center gap-2 text-xs text-ink-400">
          <Loader2 size={14} className="animate-spin" />
          Reading channel settings…
        </div>
      </ChannelsShell>
    );
  }
  if (settings.isError || !settings.data) {
    return (
      <ChannelsShell>
        <div className="flex items-center justify-between gap-3 text-xs">
          <span className="flex items-center gap-1.5 text-sev-critical">
            <AlertCircle size={13} />
            {settings.error instanceof Error
              ? settings.error.message
              : "Couldn't read channel settings."}
          </span>
          <button className="btn" onClick={() => settings.refetch()}>
            Retry
          </button>
        </div>
      </ChannelsShell>
    );
  }

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ["agent-channel-settings"] });
  };

  return (
    <ChannelsShell>
      <div
        role="tablist"
        aria-label="Notification channel"
        className="mb-3 flex flex-wrap gap-1 border-b border-ink-600"
      >
        {CHANNELS.map((channel) => {
          const active = channel === activeChannel;
          const isOverride = settings.data[channel]?.source === "override";
          return (
            <button
              key={channel}
              type="button"
              role="tab"
              aria-selected={active}
              onClick={() => setActiveChannel(channel)}
              className={clsx(
                "inline-flex items-center gap-1.5 rounded-t-control border-b-2 px-3 py-2 text-xs font-medium",
                active
                  ? "border-link text-ink-50"
                  : "border-transparent text-ink-300 hover:text-ink-100",
              )}
            >
              <ChannelIcon id={channel} size={13} />
              {channelLabel(channel)}
              {isOverride && (
                <span
                  className="h-1.5 w-1.5 rounded-full bg-link"
                  aria-label="has a runtime override"
                />
              )}
            </button>
          );
        })}
      </div>
      <ChannelCard
        key={activeChannel}
        channel={activeChannel}
        view={settings.data[activeChannel]}
        refetch={() => settings.refetch()}
        invalidate={invalidate}
        toast={toast}
      />
      <p className="mt-3 text-2xs text-ink-400">
        Changes take effect on the next alert.
      </p>
    </ChannelsShell>
  );
}

// ChannelCard — one channel's data-driven form. Local form state lives here so
// each card manages its own enable/field values (no conditional hooks).
function ChannelCard({
  channel,
  view,
  refetch,
  invalidate,
  toast,
}: {
  channel: string;
  view: ChannelSettingsView | undefined;
  refetch: () => void;
  invalidate: () => void;
  toast: ReturnType<typeof useToast>;
}) {
  const schema = CHANNEL_SCHEMA[channel] ?? [];
  const [enabled, setEnabled] = useState<boolean>(view?.enabled ?? false);
  const [values, setValues] = useState<Record<string, string>>(() =>
    initialChannelValues(channel, view),
  );
  const [showSecret, setShowSecret] = useState<Record<string, boolean>>({});

  useEffect(() => {
    setEnabled(view?.enabled ?? false);
    setValues(initialChannelValues(channel, view));
    setShowSecret({});
  }, [channel, view]);

  const save = useMutation({
    mutationFn: () =>
      api.setChannelSettings(channel, buildChannelPut(channel, enabled, values)),
    onSuccess: () => {
      toast.push({ title: `${channelLabel(channel)} saved`, tone: "ok" });
      // Drop the transient secrets the moment the PUT lands.
      setValues((v) => {
        const next = { ...v };
        for (const f of schema) if (f.secret) next[f.name] = "";
        return next;
      });
      setShowSecret({});
      invalidate();
      refetch();
    },
    onError: (err) => {
      if (noEncryptionKeyMessage(err)) {
        setValues((v) => {
          const next = { ...v };
          for (const f of schema) if (f.secret) next[f.name] = "";
          return next;
        });
        return; // surfaced inline below
      }
      toast.push({
        title: `Couldn't save ${channelLabel(channel)}`,
        description: err instanceof Error ? err.message : String(err),
        tone: "error",
      });
    },
  });

  const clear = useMutation({
    mutationFn: () => api.clearChannelSettings(channel),
    onSuccess: () => {
      toast.push({ title: `${channelLabel(channel)} reverted to default`, tone: "ok" });
      invalidate();
      refetch();
    },
    onError: (err) => {
      toast.push({
        title: `Couldn't revert ${channelLabel(channel)}`,
        description: err instanceof Error ? err.message : String(err),
        tone: "error",
      });
    },
  });

  const test = useMutation({
    mutationFn: () => api.testChannel(channel),
    onSuccess: () => toast.push({ title: `${channelLabel(channel)} test sent`, tone: "ok" }),
    onError: (err) =>
      toast.push({
        title: `${channelLabel(channel)} test failed`,
        description: err instanceof Error ? err.message : String(err),
        tone: "error",
      }),
  });

  const busy = save.isPending || clear.isPending || test.isPending;
  const onOverride = view?.source === "override";
  const noKeyMsg = noEncryptionKeyMessage(save.error);

  // "Use proxy" is rendered on its OWN line below the field grid (not inline),
  // and REVEALS the proxy reference when checked. gridFields is every other
  // field; proxyField is the toggle when this channel supports it.
  const proxyField = schema.find((f) => f.name === PROXY_FIELD);
  const gridFields = schema.filter((f) => f.name !== PROXY_FIELD);
  const proxyOn = values[PROXY_FIELD] === "true";

  return (
    <div className="rounded-control border border-ink-600/60 bg-ink-700/30 p-3">
      <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          <ChannelIcon id={channel} size={14} />
          <span className="text-sm font-medium text-ink-100">
            {channelLabel(channel)}
          </span>
          <ProvenanceChip source={view?.source ?? "yaml"} />
        </div>
        <label className="flex cursor-pointer items-center gap-2 text-xs text-ink-200">
          <input
            type="checkbox"
            checked={enabled}
            disabled={busy}
            onChange={(e) => setEnabled(e.target.checked)}
            className="h-4 w-4 accent-link"
          />
          Enable
        </label>
      </div>

      <div className="grid gap-2 sm:grid-cols-2">
        {gridFields.map((f) => {
          const mf = view?.fields?.[f.name];
          if (f.bool) {
            return (
              <label
                key={f.name}
                className="flex items-center gap-2 text-xs text-ink-200"
              >
                <input
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
          return (
            <div key={f.name} className="min-w-0">
              <label className="field-label" htmlFor={`${channel}-${f.name}`}>
                {f.label}
                {f.secret && (
                  <span className="ml-1 font-normal text-ink-400">
                    ({fieldSetLabel(mf?.set ?? false, mf?.hint ?? "")} — blank keeps it)
                  </span>
                )}
              </label>
              <div className="relative">
                <input
                  id={`${channel}-${f.name}`}
                  type={f.secret && !showSecret[f.name] ? "password" : "text"}
                  autoComplete="off"
                  placeholder={f.secret && mf?.set ? "•••• stored — blank keeps it" : ""}
                  value={values[f.name] ?? ""}
                  disabled={busy}
                  onChange={(e) =>
                    setValues((v) => ({ ...v, [f.name]: e.target.value }))
                  }
                  className={clsx("input h-9 text-sm", f.secret && "pr-9")}
                />
                {f.secret && (
                  <button
                    type="button"
                    aria-label={showSecret[f.name] ? "Hide value" : "Show value"}
                    aria-pressed={!!showSecret[f.name]}
                    className="absolute right-1 top-1/2 -translate-y-1/2 rounded-control p-1.5 text-ink-300 hover:bg-ink-600 hover:text-ink-100"
                    onClick={() =>
                      setShowSecret((s) => ({ ...s, [f.name]: !s[f.name] }))
                    }
                  >
                    {showSecret[f.name] ? (
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

      {/* Use proxy — on its OWN line, not inline in the grid. When checked it
          reveals the proxy reference (below). */}
      {proxyField && (
        <div className="mt-3 border-t border-ink-600/60 pt-3">
          <label className="flex w-fit cursor-pointer items-center gap-2 text-xs text-ink-200">
            <input
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

      {noKeyMsg && (
        <div
          role="alert"
          className="mt-2 flex items-start gap-2 rounded-control border border-sev-warn/40 bg-sev-warn/10 p-2.5 text-xs"
        >
          <AlertCircle size={13} className="mt-0.5 shrink-0 text-sev-warn" aria-hidden />
          <p className="text-ink-200">{noKeyMsg}</p>
        </div>
      )}

      <div className="mt-3 flex flex-wrap gap-2">
        <button
          type="button"
          disabled={busy}
          onClick={() => save.mutate()}
          className="btn btn-primary"
        >
          {save.isPending ? "Saving…" : "Save"}
        </button>
        {onOverride && (
          <>
            <button
              type="button"
              disabled={busy}
              onClick={() => test.mutate()}
              className="btn inline-flex items-center gap-1.5"
              title="Send a synthetic test alert to this channel"
            >
              <Send size={13} aria-hidden />
              Test
            </button>
            <button
              type="button"
              disabled={busy}
              onClick={() => clear.mutate()}
              className="btn"
              title="Clear the runtime override and follow the deployed default"
            >
              Clear override
            </button>
          </>
        )}
      </div>
    </div>
  );
}

// ProxyReference is the read-only reveal shown when a channel's "Use proxy" is
// on. The runtime channel store carries only the use_proxy BOOLEAN — it points
// at the deployment-level `proxy:` config (URL / username / password), shared by
// every channel that turns proxy on and set at deploy time. So this reveals
// WHERE the proxy settings live and that they are not editable at runtime,
// rather than fabricating per-channel proxy fields the runtime store lacks.
// (Making per-channel proxy fields editable at runtime would require adding them
// to the enterprise runtimechannels field schema.)
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

// ProvenanceChip — override vs default, never color alone.
function ProvenanceChip({ source }: { source: "override" | "yaml" }) {
  const override = source === "override";
  return (
    <span
      className={clsx(
        "inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-2xs font-medium",
        override
          ? "border-link/40 bg-link/15 text-link"
          : "border-ink-500/40 bg-ink-600/40 text-ink-300",
      )}
    >
      {override ? "runtime override" : "default"}
    </span>
  );
}

// ChannelsShell — the consistent card chrome every state renders inside.
function ChannelsShell({ children }: { children: React.ReactNode }) {
  return (
    <div className="card mb-4">
      <div className="card-header">
        <h2 className="card-title inline-flex items-center gap-2">
          <Radio size={15} aria-hidden className="text-ink-400" />
          Notification channels
        </h2>
        <span className="text-2xs text-ink-400">Enterprise control</span>
      </div>
      <div className="card-body">{children}</div>
    </div>
  );
}

// LockedBody — the shared Enterprise-only locked upsell, channels copy.
function LockedBody() {
  return (
    <EnterpriseLockedBody title="Runtime channel config is an Enterprise capability">
      Configure notification channels — rotate a Slack/Telegram/Teams/Lark/Viber
      credential or SMTP login and toggle a channel — at runtime, without editing
      config files or restarting, and revert instantly. Available on Versus
      Enterprise.
    </EnterpriseLockedBody>
  );
}
