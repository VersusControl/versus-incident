import { useMemo, useState } from "react";
import {
  useInfiniteQuery,
  useMutation,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import {
  AlertTriangle,
  BellOff,
  ChevronDown,
  ChevronRight,
  Info,
  Loader2,
  Plus,
  Trash2,
} from "lucide-react";

import {
  ApiError,
  api,
  type AlertFatigueConfig,
  type AlertFatigueFinding,
  type AlertFatigueSort,
  type AlertFatigueSortDir,
  type AlertFatigueCorrelationGroup,
  type AlertFatigueDependencyEdge,
  type AlertFatigueDependencyHold,
} from "@/lib/api";
import { displayService, fmtAbs, fmtRel } from "@/lib/format";
import { adminGateState } from "@/lib/role";
import { useEffectiveRole } from "@/lib/useEffectiveRole";
import { TopBar } from "@/components/TopBar";
import { Pill } from "@/components/Pill";
import { SeverityBadge } from "@/components/SeverityBadge";
import { EmptyState } from "@/components/feedback";
import { EnterpriseLockedBody } from "@/components/EnterpriseLocked";
import { AdminAccessNotice } from "@/components/AdminAccessNotice";
import { PeekPanel, PeekField } from "@/components/PeekPanel";
import { SkRows } from "@/components/Skeleton";

// AlertFatiguePage — the operator surface for the Enterprise alert-fatigue
// feature. Modeled on the SLI/SLO auto-define page: a default-OFF master enable
// switch, and everything below it (the fatigue-channel picker, the "require
// review before spam" switch, and the fingerprint review table) is hidden until
// the feature is enabled.
//
// Gated exactly like the AI-settings / channel-settings controls on the
// caller's effective RBAC role (useEffectiveRole → adminGateState): a community
// binary renders the Enterprise upsell, a gateway-secret operator is asked to
// sign in, a viewer/responder gets the read-only "requires admin" notice, and
// only admin/owner reach the live controls. Every endpoint is enterprise +
// runtime:manage gated server-side, so the SPA fails closed here before it ever
// issues a privileged request.

const PAGE_TITLE = "Alert fatigue";
const SUBTITLE =
  "Deterministically divert repeat, low-signal alerts to a fatigue channel so the on-call channel stays clean — every suppression stays visible and reversible.";

const LOCKED_TITLE = "Alert fatigue is an Enterprise capability";
const LOCKED_BODY =
  "Alert fatigue deduplicates repeat alerts and diverts the noise to a channel you choose, with a reviewable record of every fingerprint so you can reclaim anything that was not actually noise.";

// PENDING_REVIEW_NOTE is the exact operator guidance shown beside the
// "Require review before spam" switch (per the implementation plan §4.1).
const PENDING_REVIEW_NOTE =
  "Alerts are auto-marked as spam by default — some alerts may stop being " +
  "sent. If you notice alerts missing and want to approve them before " +
  "they're marked as spam, enable pending review.";

// KNOWN_CHANNELS is the static fallback list of notification channels used when
// the runtime channel-settings map is unavailable. The picker prefers the real
// configured channels (api.getChannelSettings) and only falls back to these.
const KNOWN_CHANNELS = [
  "slack",
  "telegram",
  "viber",
  "email",
  "msteams",
  "lark",
];

const PAGE_SIZE = 50;

// STATUS_FILTERS are the review-table filter options. The server's internal
// `tracking` state is intentionally absent — requesting it is a 400.
const STATUS_FILTERS: Array<{ value: string; label: string }> = [
  { value: "", label: "All" },
  { value: "fatigued", label: "Fatigued" },
  { value: "pending_review", label: "Pending review" },
  { value: "reclaimed", label: "Reclaimed" },
];

function AlertFatigueShell({ children }: { children: React.ReactNode }) {
  return (
    <>
      <TopBar title={PAGE_TITLE} />
      <main className="flex-1 overflow-auto p-4 lg:p-6">{children}</main>
    </>
  );
}

export function AlertFatiguePage() {
  const access = useEffectiveRole();
  const gate = adminGateState({
    loading: access.loading,
    enterprise: access.enterprise,
    hasSession: access.hasSession,
    isAdmin: access.isAdmin,
  });

  if (gate === "loading") {
    return (
      <AlertFatigueShell>
        <div className="card flex items-center gap-2 p-4 text-xs text-ink-400">
          <Loader2 size={14} className="animate-spin" />
          Checking access…
        </div>
      </AlertFatigueShell>
    );
  }
  if (gate === "locked") {
    return (
      <AlertFatigueShell>
        <div className="card p-8">
          <EnterpriseLockedBody title={LOCKED_TITLE}>
            {LOCKED_BODY}
          </EnterpriseLockedBody>
        </div>
      </AlertFatigueShell>
    );
  }
  if (gate === "sign-in") {
    return (
      <AlertFatigueShell>
        <div className="card p-4">
          <AdminAccessNotice reason="sign-in" />
        </div>
      </AlertFatigueShell>
    );
  }
  if (gate === "read-only") {
    return (
      <AlertFatigueShell>
        <div className="card p-4">
          <AdminAccessNotice reason="role" />
        </div>
      </AlertFatigueShell>
    );
  }

  return (
    <AlertFatigueShell>
      <p className="mb-3 max-w-3xl text-xs text-ink-300">{SUBTITLE}</p>
      <AdminBody />
    </AlertFatigueShell>
  );
}

// AdminBody is the live control, split out so its data-loading hooks sit below
// the role gate (no conditional hooks).
function AdminBody() {
  const qc = useQueryClient();
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);

  const cfg = useQuery({
    queryKey: ["alert-fatigue-config"],
    queryFn: () => api.getAlertFatigueConfig(),
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

  // The configured notification channels for the fatigue-channel picker. Reads
  // the dedicated alert-fatigue channels endpoint (name + effective enabled),
  // so the picker knows which channels are still live and can warn when the
  // diverted channel was later disabled. Falls back to the static known names
  // when it is unavailable.
  const channels = useQuery({
    queryKey: ["alert-fatigue-channels"],
    queryFn: api.listAlertFatigueChannels,
    retry: false,
    staleTime: 60_000,
  });

  const save = useMutation({
    mutationFn: (next: AlertFatigueConfig) => api.setAlertFatigueConfig(next),
    onSuccess: (data) => {
      qc.setQueryData(["alert-fatigue-config"], data);
    },
    onError: (err: unknown) => {
      setMsg({
        ok: false,
        text:
          err instanceof ApiError ? err.message : "Could not update settings",
      });
    },
  });

  if (cfg.isPending) {
    return (
      <div className="card flex items-center gap-2 p-4 text-xs text-ink-400">
        <Loader2 size={14} className="animate-spin" />
        Reading alert-fatigue settings…
      </div>
    );
  }
  if (cfg.isError || !cfg.data) {
    // A late 403/404 (binary lost the route) still resolves to the upsell.
    const s = cfg.error instanceof ApiError ? cfg.error.status : null;
    if (s === 403 || s === 404) {
      return (
        <div className="card p-8">
          <EnterpriseLockedBody title={LOCKED_TITLE}>
            {LOCKED_BODY}
          </EnterpriseLockedBody>
        </div>
      );
    }
    return (
      <div className="card flex items-center justify-between gap-3 p-4 text-xs">
        <span className="text-sev-critical">
          {cfg.error instanceof Error
            ? cfg.error.message
            : "Couldn't read alert-fatigue settings."}
        </span>
        <button className="btn" onClick={() => cfg.refetch()}>
          Retry
        </button>
      </div>
    );
  }

  const config = cfg.data;
  const patch = (partial: Partial<AlertFatigueConfig>) => {
    setMsg(null);
    save.mutate({ ...config, ...partial });
  };

  const channelList = channels.data ?? [];
  const channelNames = ((): string[] => {
    const names = channelList.map((c) => c.name);
    const base = names.length ? names : KNOWN_CHANNELS;
    // Keep the currently-selected channel selectable even if it is not (or no
    // longer) in the configured set, so the picker never silently drops it.
    if (config.fatigue_channel && !base.includes(config.fatigue_channel)) {
      return [config.fatigue_channel, ...base];
    }
    return base;
  })();
  // The diverted channel is broken when the server's derived echo says so, or
  // when the channels list explicitly reports the selected channel disabled.
  const selectedDisabled = channelList.some(
    (c) => c.name === config.fatigue_channel && !c.enabled,
  );
  const channelInvalid =
    Boolean(config.fatigue_channel) &&
    (config.fatigue_channel_valid === false || selectedDisabled);

  return (
    <div className="grid gap-4">
      {/* Enable + config card */}
      <div className="card p-4" data-testid="alert-fatigue-config">
        <div className="flex flex-wrap items-start gap-3">
          <button
            type="button"
            role="switch"
            aria-checked={config.enabled}
            aria-label="Enable alert fatigue"
            disabled={save.isPending}
            data-testid="alert-fatigue-enable-toggle"
            onClick={() => patch({ enabled: !config.enabled })}
            className={`relative inline-flex h-5 w-9 shrink-0 items-center rounded-full transition ${
              config.enabled ? "bg-link" : "bg-ink-600"
            } ${save.isPending ? "opacity-70" : ""}`}
          >
            <span
              className={`inline-block h-4 w-4 transform rounded-full bg-white transition ${
                config.enabled ? "translate-x-4" : "translate-x-0.5"
              }`}
            />
          </button>
          <div className="min-w-0">
            <div className="flex items-center gap-2 text-xs font-semibold text-ink-100">
              <BellOff size={14} className="text-link" aria-hidden />
              Enable alert fatigue
            </div>
            <div className="text-2xs text-ink-400">
              Turn the deterministic dedup/divert interceptor on or off for this
              org. Off by default — nothing is suppressed until it is on.
            </div>
          </div>
        </div>

        {config.enabled && (
          <div className="mt-4 grid gap-4 border-t border-ink-700 pt-4">
            {/* Fatigue channel picker */}
            <div className="flex flex-wrap items-end gap-3">
              <div>
                <label
                  className="field-label"
                  htmlFor="alert-fatigue-channel"
                >
                  Fatigue channel
                </label>
                <div className="text-2xs text-ink-400">
                  Where fatigued (&ldquo;spam&rdquo;) alerts are diverted so the
                  on-call channel stays clean.
                </div>
              </div>
              <select
                id="alert-fatigue-channel"
                data-testid="alert-fatigue-channel-select"
                className="input h-9 max-w-xs text-sm"
                value={config.fatigue_channel}
                disabled={save.isPending}
                onChange={(e) => patch({ fatigue_channel: e.target.value })}
              >
                <option value="">Select a channel…</option>
                {channelNames.map((c) => {
                  const disabled = channelList.some(
                    (ch) => ch.name === c && !ch.enabled,
                  );
                  return (
                    <option key={c} value={c}>
                      {c}
                      {disabled ? " (disabled)" : ""}
                    </option>
                  );
                })}
              </select>
            </div>

            {channelInvalid && (
              <div
                className="flex items-start gap-1.5 text-2xs text-sev-warn"
                role="alert"
                data-testid="alert-fatigue-channel-warning"
              >
                <AlertTriangle size={12} className="mt-0.5 shrink-0" aria-hidden />
                <span>
                  The fatigue channel{" "}
                  <span className="font-medium">
                    &ldquo;{config.fatigue_channel}&rdquo;
                  </span>{" "}
                  is no longer an enabled channel. Diverted alerts may not be
                  delivered — pick an active channel below.
                </span>
              </div>
            )}

            {/* Require review before spam */}
            <div
              className="flex flex-wrap items-start gap-3"
              data-testid="alert-fatigue-pending-control"
            >
              <button
                type="button"
                role="switch"
                aria-checked={config.pending_review}
                aria-label="Require review before spam"
                disabled={save.isPending}
                data-testid="alert-fatigue-pending-toggle"
                onClick={() =>
                  patch({ pending_review: !config.pending_review })
                }
                className={`relative inline-flex h-5 w-9 shrink-0 items-center rounded-full transition ${
                  config.pending_review ? "bg-link" : "bg-ink-600"
                } ${save.isPending ? "opacity-70" : ""}`}
              >
                <span
                  className={`inline-block h-4 w-4 transform rounded-full bg-white transition ${
                    config.pending_review ? "translate-x-4" : "translate-x-0.5"
                  }`}
                />
              </button>
              <div className="min-w-0 max-w-2xl">
                <div className="text-xs font-semibold text-ink-100">
                  Require review before spam
                </div>
                <div
                  className="mt-0.5 flex items-start gap-1.5 text-2xs text-ink-400"
                  data-testid="alert-fatigue-pending-note"
                >
                  <Info
                    size={12}
                    className="mt-0.5 shrink-0 text-ink-500"
                    aria-hidden
                  />
                  <span>{PENDING_REVIEW_NOTE}</span>
                </div>
              </div>
            </div>
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
      </div>

      {/* Everything below the master switch is scoped to the feature being on,
          matching the pending-review gating. */}
      {config.enabled && (
        <>
          <AnalyticsStrip />
          <ReviewTable />
          <CorrelationSection saving={save.isPending} />
          <DependencySection saving={save.isPending} />
        </>
      )}
    </div>
  );
}

// ReviewTable lists reviewable fingerprints, filterable by status and paged via
// a load-more cursor (page/page_size + whole-set total, like the analyses list).
// Row actions are RBAC-gated by the same admin-only mount as the rest of the
// page; on success the list is invalidated so the moved row reflects its new
// state.
function ReviewTable() {
  const qc = useQueryClient();
  const [statusFilter, setStatusFilter] = useState("");
  const [sort, setSort] = useState<AlertFatigueSort>("last_seen");
  const [dir, setDir] = useState<AlertFatigueSortDir>("desc");
  const [peekId, setPeekId] = useState<string | null>(null);

  const q = useInfiniteQuery({
    queryKey: ["alert-fatigue-fingerprints", statusFilter, sort, dir],
    queryFn: ({ pageParam }) =>
      api.listAlertFatigueFingerprints({
        status: statusFilter || undefined,
        sort,
        dir,
        page: pageParam,
        pageSize: PAGE_SIZE,
      }),
    initialPageParam: 1,
    getNextPageParam: (last) =>
      last.page * last.page_size < last.total ? last.page + 1 : undefined,
  });

  // toggleSort drives the SERVER sort param: clicking a new column selects it
  // (defaulting to descending — biggest/most-recent first); clicking the active
  // column flips the direction. Changing either resets the infinite query to
  // page 1 via the queryKey, so the load-more cursor never spans two orderings.
  const toggleSort = (col: AlertFatigueSort) => {
    if (col === sort) {
      setDir((d) => (d === "desc" ? "asc" : "desc"));
    } else {
      setSort(col);
      setDir("desc");
    }
  };

  const items = useMemo<AlertFatigueFinding[]>(
    () => q.data?.pages.flatMap((p) => p.fingerprints) ?? [],
    [q.data],
  );
  const total = q.data?.pages[0]?.total;

  const invalidate = () =>
    qc.invalidateQueries({ queryKey: ["alert-fatigue-fingerprints"] });

  const confirm = useMutation({
    mutationFn: (id: string) => api.confirmAlertFatigueFingerprint(id),
    onSuccess: invalidate,
  });
  const reclaim = useMutation({
    mutationFn: (id: string) => api.reclaimAlertFatigueFingerprint(id),
    onSuccess: invalidate,
  });
  const acting = confirm.isPending || reclaim.isPending;

  const peek = peekId ? items.find((r) => r.id === peekId) : undefined;

  return (
    <div className="card overflow-hidden">
      <div className="flex flex-wrap items-center justify-between gap-2 border-b border-ink-700 px-4 py-3">
        <h2 className="text-sm font-semibold text-ink-50">
          Fingerprints
          {total !== undefined && (
            <span className="ml-2 text-2xs font-normal text-ink-400">
              {total.toLocaleString()} total
            </span>
          )}
        </h2>
        <label className="flex items-center gap-2 text-2xs text-ink-400">
          <span>Status</span>
          <select
            className="input h-8 text-xs"
            data-testid="alert-fatigue-status-filter"
            value={statusFilter}
            onChange={(e) => setStatusFilter(e.target.value)}
          >
            {STATUS_FILTERS.map((f) => (
              <option key={f.value || "all"} value={f.value}>
                {f.label}
              </option>
            ))}
          </select>
        </label>
      </div>

      {q.isError ? (
        <div className="flex items-center justify-between gap-3 p-4 text-xs">
          <span className="text-sev-critical">
            {q.error instanceof Error
              ? q.error.message
              : "Couldn't load fingerprints."}
          </span>
          <button className="btn" onClick={() => q.refetch()}>
            Retry
          </button>
        </div>
      ) : q.isPending ? (
        <table className="ddt">
          <tbody>
            <SkRows rows={4} cols={1} />
          </tbody>
        </table>
      ) : items.length === 0 ? (
        <EmptyState
          title="No fingerprints yet"
          hint="Fatigued and pending-review fingerprints appear here as the interceptor records repeat, low-signal alerts."
        />
      ) : (
        <>
          <table className="ddt">
            <thead>
              <tr>
                <th>Service</th>
                <th>Source</th>
                <th className="w-24">Severity</th>
                <th className="w-24">
                  <SortHeader
                    label="Priority"
                    col="priority"
                    sort={sort}
                    dir={dir}
                    onSort={toggleSort}
                  />
                </th>
                <th className="w-20 text-right">
                  <SortHeader
                    label="Repeats"
                    col="repeat_count"
                    sort={sort}
                    dir={dir}
                    onSort={toggleSort}
                    align="right"
                  />
                </th>
                <th className="w-32">
                  <SortHeader
                    label="Last seen"
                    col="last_seen"
                    sort={sort}
                    dir={dir}
                    onSort={toggleSort}
                  />
                </th>
                <th className="w-32">Status</th>
                <th>Routed channel</th>
                <th className="w-48 text-right">Actions</th>
              </tr>
            </thead>
            <tbody>
              {items.map((r) => (
                <FingerprintRow
                  key={r.id}
                  row={r}
                  acting={acting}
                  onPeek={() => setPeekId(r.id)}
                  onConfirm={() => confirm.mutate(r.id)}
                  onReclaim={() => reclaim.mutate(r.id)}
                />
              ))}
            </tbody>
          </table>
          {q.hasNextPage && (
            <div className="border-t border-ink-700 px-4 py-3 text-center">
              <button
                type="button"
                className="btn"
                data-testid="alert-fatigue-load-more"
                disabled={q.isFetchingNextPage}
                onClick={() => q.fetchNextPage()}
              >
                {q.isFetchingNextPage ? "Loading…" : "Load more"}
              </button>
            </div>
          )}
        </>
      )}

      <PeekPanel
        open={Boolean(peek)}
        onClose={() => setPeekId(null)}
        title="Fingerprint detail"
      >
        {peek && (
          <dl className="grid gap-3">
            <PeekField label="Service">{displayService(peek.service)}</PeekField>
            <PeekField label="Source">{peek.source || "—"}</PeekField>
            <PeekField label="Fingerprint">
              <span className="break-all font-mono text-2xs">
                {peek.fingerprint}
              </span>
            </PeekField>
            <PeekField label="Repeat count">{peek.repeat_count}</PeekField>
            <PeekField label="First seen">
              <span title={fmtAbs(peek.first_seen)}>
                {fmtRel(peek.first_seen)}
              </span>
            </PeekField>
            <PeekField label="Last seen">
              <span title={fmtAbs(peek.last_seen)}>
                {fmtRel(peek.last_seen)}
              </span>
            </PeekField>
            <PeekField label="Routed channel">
              {peek.routed_channel || "—"}
            </PeekField>
            {peek.priority_score !== undefined && (
              <PeekField label="Priority">
                <div className="flex flex-col gap-1">
                  <PriorityBadge
                    score={peek.priority_score}
                    reason={peek.priority_reason}
                  />
                  {peek.priority_reason && (
                    <span className="text-2xs text-ink-400">
                      {peek.priority_reason}
                    </span>
                  )}
                </div>
              </PeekField>
            )}
            <PeekField label="Alert content (redacted)">
              <pre className="max-h-72 overflow-auto whitespace-pre-wrap break-all rounded-md border border-ink-600 bg-ink-950/40 p-2 font-mono text-2xs text-ink-200">
                {peek.alert_content
                  ? JSON.stringify(peek.alert_content, null, 2)
                  : "—"}
              </pre>
            </PeekField>
          </dl>
        )}
      </PeekPanel>
    </div>
  );
}

// SortHeader is a clickable column header that drives the SERVER sort. Clicking
// selects the column (descending by default); clicking the active column flips
// direction. The active column shows an ↑/↓ indicator and aria-sort for
// assistive tech.
function SortHeader({
  label,
  col,
  sort,
  dir,
  onSort,
  align,
}: {
  label: string;
  col: AlertFatigueSort;
  sort: AlertFatigueSort;
  dir: AlertFatigueSortDir;
  onSort: (col: AlertFatigueSort) => void;
  align?: "right";
}) {
  const active = sort === col;
  return (
    <button
      type="button"
      data-testid={`alert-fatigue-sort-${col}`}
      aria-sort={active ? (dir === "asc" ? "ascending" : "descending") : "none"}
      onClick={() => onSort(col)}
      className={`flex items-center gap-1 hover:text-ink-100 ${
        align === "right" ? "ml-auto justify-end" : ""
      } ${active ? "text-ink-100" : ""}`}
      title={`Sort by ${label.toLowerCase()}`}
    >
      <span>{label}</span>
      <span className="text-2xs text-ink-400" aria-hidden="true">
        {active ? (dir === "asc" ? "↑" : "↓") : ""}
      </span>
    </button>
  );
}

function StatusPill({ status }: { status: string }) {
  const s = status.toLowerCase();
  if (s === "fatigued") return <Pill tone="bad">Fatigued</Pill>;
  if (s === "pending_review") return <Pill tone="warn">Pending review</Pill>;
  if (s === "reclaimed") return <Pill tone="good">Reclaimed</Pill>;
  return <Pill>{status || "—"}</Pill>;
}

// PriorityBadge renders the deterministic priority scorecard as a compact,
// color-graded badge. High scores (>= 0.80 = the interceptor's page-now floor)
// read "bad"/urgent; mid "warn"; low "default". Absent score → an em dash (the
// scorer had no signal for that row).
function PriorityBadge({
  score,
  reason,
}: {
  score?: number;
  reason?: string;
}) {
  if (score === undefined || score === null) {
    return <span className="text-2xs text-ink-500">—</span>;
  }
  const pct = Math.round(score * 100);
  const tone = score >= 0.8 ? "bad" : score >= 0.5 ? "warn" : "default";
  return (
    <Pill tone={tone} title={reason || undefined}>
      {pct}
    </Pill>
  );
}

function FingerprintRow({
  row,
  acting,
  onPeek,
  onConfirm,
  onReclaim,
}: {
  row: AlertFatigueFinding;
  acting: boolean;
  onPeek: () => void;
  onConfirm: () => void;
  onReclaim: () => void;
}) {
  const s = row.status.toLowerCase();
  return (
    <tr>
      <td className="font-medium text-ink-100">
        <button
          type="button"
          className="text-left hover:text-link hover:underline"
          onClick={onPeek}
          title="View fingerprint detail"
        >
          {displayService(row.service)}
        </button>
      </td>
      <td className="text-2xs text-ink-300">{row.source || "—"}</td>
      <td>
        <SeverityBadge severity={row.severity} />
      </td>
      <td>
        <PriorityBadge score={row.priority_score} reason={row.priority_reason} />
      </td>
      <td className="text-right tabular-nums text-ink-200">
        {row.repeat_count}
      </td>
      <td className="text-2xs text-ink-300" title={fmtAbs(row.last_seen)}>
        {fmtRel(row.last_seen)}
      </td>
      <td>
        <StatusPill status={row.status} />
      </td>
      <td className="text-2xs text-ink-300">{row.routed_channel || "—"}</td>
      <td className="text-right">
        <div className="flex justify-end gap-1.5">
          {s === "pending_review" && (
            <button
              type="button"
              className="btn px-2 py-1 text-2xs"
              disabled={acting}
              onClick={onConfirm}
            >
              Confirm spam
            </button>
          )}
          {(s === "fatigued" || s === "pending_review") && (
            <button
              type="button"
              className="btn px-2 py-1 text-2xs"
              disabled={acting}
              onClick={onReclaim}
            >
              Not spam
            </button>
          )}
          {s === "reclaimed" && (
            <button
              type="button"
              className="btn px-2 py-1 text-2xs"
              disabled={acting}
              onClick={onConfirm}
            >
              Mark as spam
            </button>
          )}
        </div>
      </td>
    </tr>
  );
}

// ---------------------------------------------------------------------------
// Shared section building blocks
// ---------------------------------------------------------------------------

// SwitchToggle is the role="switch" control reused by the correlation and
// dependency sections (same look/behaviour as the master enable switch).
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

// SecondsSetting is a labelled number input (seconds) with an Apply button and
// a read-only "effective" echo of the value the interceptor actually applies
// after its default/clamp. Apply is disabled while saving, when the value is
// unchanged, or when it is not a positive integer.
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

// SectionShell is the card + heading wrapper the analytics / correlation /
// dependency sections share so the page reads as labelled sections, not a wall
// of controls.
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
    <div className="card p-4" data-testid={testId}>
      <h2 className="mb-3 flex items-center gap-2 text-sm font-semibold text-ink-50">
        {icon}
        {title}
      </h2>
      {children}
    </div>
  );
}

function pct(v: number): string {
  if (!Number.isFinite(v)) return "—";
  return `${Math.round(v * 100)}%`;
}

// ---------------------------------------------------------------------------
// Analytics strip (read-only noise read-model)
// ---------------------------------------------------------------------------

const ANALYTICS_WINDOWS: Array<{ value: "7d" | "30d"; label: string }> = [
  { value: "7d", label: "7 days" },
  { value: "30d", label: "30 days" },
];

function AnalyticsStrip() {
  const [window, setWindow] = useState<"7d" | "30d">("7d");
  const q = useQuery({
    queryKey: ["alert-fatigue-analytics", window],
    queryFn: () => api.getAlertFatigueAnalytics(window),
    retry: false,
    staleTime: 30_000,
  });

  return (
    <SectionShell title="Noise analytics" testId="alert-fatigue-analytics">
      <div className="mb-3 flex items-center justify-between gap-2">
        <span className="text-2xs text-ink-400">
          Deterministic, org-scoped counts over the selected window.
        </span>
        <label className="flex items-center gap-2 text-2xs text-ink-400">
          <span>Window</span>
          <select
            className="input h-8 text-xs"
            data-testid="alert-fatigue-analytics-window"
            value={window}
            onChange={(e) => setWindow(e.target.value as "7d" | "30d")}
          >
            {ANALYTICS_WINDOWS.map((w) => (
              <option key={w.value} value={w.value}>
                {w.label}
              </option>
            ))}
          </select>
        </label>
      </div>

      {q.isError ? (
        <div className="flex items-center justify-between gap-3 text-xs">
          <span className="text-sev-critical">
            {q.error instanceof Error
              ? q.error.message
              : "Couldn't load analytics."}
          </span>
          <button className="btn" onClick={() => q.refetch()}>
            Retry
          </button>
        </div>
      ) : q.isPending ? (
        <div className="flex items-center gap-2 text-xs text-ink-400">
          <Loader2 size={14} className="animate-spin" />
          Reading analytics…
        </div>
      ) : (
        <>
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-5">
            <Stat label="Total alerts" value={q.data.total.toLocaleString()} />
            <Stat label="Noise ratio" value={pct(q.data.noise_ratio)} />
            <Stat label="Diverted" value={q.data.diverted.toLocaleString()} />
            <Stat
              label="Reclaimed"
              value={q.data.reclaim_count.toLocaleString()}
            />
            <Stat label="Reclaim rate" value={pct(q.data.reclaim_rate)} />
          </div>
          {q.data.top_noisy.length > 0 && (
            <div className="mt-4">
              <div className="mb-1.5 text-2xs font-semibold uppercase tracking-wide text-ink-400">
                Top-noisy services
              </div>
              <ul
                className="grid gap-1"
                data-testid="alert-fatigue-top-noisy"
              >
                {q.data.top_noisy.map((s) => (
                  <li
                    key={s.service}
                    className="flex items-center justify-between gap-3 text-2xs"
                  >
                    <span className="truncate text-ink-200">
                      {displayService(s.service)}
                    </span>
                    <span className="shrink-0 tabular-nums text-ink-400">
                      {s.repeat_total.toLocaleString()} repeats ·{" "}
                      {s.findings.toLocaleString()} fingerprints
                    </span>
                  </li>
                ))}
              </ul>
            </div>
          )}
        </>
      )}
    </SectionShell>
  );
}

function Stat({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded-md border border-ink-700 bg-ink-950/30 p-3">
      <div className="text-2xs uppercase tracking-wide text-ink-400">
        {label}
      </div>
      <div className="mt-1 text-lg font-semibold tabular-nums text-ink-50">
        {value}
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Correlation section (same-service grouping)
// ---------------------------------------------------------------------------

function CorrelationSection({ saving }: { saving: boolean }) {
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
        title="Correlation (same-service grouping)"
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
        title="Correlation (same-service grouping)"
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
  const busy = saving || save.isPending;

  return (
    <SectionShell
      title="Correlation (same-service grouping)"
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
            {q.error instanceof Error
              ? q.error.message
              : "Couldn't load groups."}
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

function CorrelationGroupRow({
  group,
}: {
  group: AlertFatigueCorrelationGroup;
}) {
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

function DependencySection({ saving }: { saving: boolean }) {
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
        title="Dependency-aware suppression"
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
        title="Dependency-aware suppression"
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
  const busy = saving || save.isPending;

  return (
    <SectionShell
      title="Dependency-aware suppression"
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
          <HoldsList />
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

function HoldsList() {
  const qc = useQueryClient();
  const [peekId, setPeekId] = useState<number | null>(null);

  const q = useInfiniteQuery({
    queryKey: ["alert-fatigue-dependency-holds"],
    queryFn: ({ pageParam }) =>
      api.listAlertFatigueDependencyHolds({
        page: pageParam,
        pageSize: PAGE_SIZE,
      }),
    initialPageParam: 1,
    getNextPageParam: (last) =>
      last.page * last.page_size < last.total ? last.page + 1 : undefined,
  });

  // reclaim marks a held symptom "should page" and releases it; on success the
  // holds list is invalidated so the reclaimed row drops out on refetch.
  const reclaim = useMutation({
    mutationFn: (id: number) => api.reclaimAlertFatigueDependencyHold(id),
    onSuccess: () =>
      qc.invalidateQueries({ queryKey: ["alert-fatigue-dependency-holds"] }),
  });

  const holds = useMemo<AlertFatigueDependencyHold[]>(
    () => q.data?.pages.flatMap((p) => p.holds) ?? [],
    [q.data],
  );
  const total = q.data?.pages[0]?.total;
  const peek = peekId !== null ? holds.find((h) => h.id === peekId) : undefined;

  return (
    <div
      className="overflow-hidden rounded-md border border-ink-700"
      data-testid="alert-fatigue-holds"
    >
      <div className="border-b border-ink-700 px-3 py-2 text-2xs font-semibold uppercase tracking-wide text-ink-400">
        Held symptoms
        {total !== undefined && (
          <span className="ml-2 font-normal normal-case text-ink-500">
            {total.toLocaleString()} total
          </span>
        )}
      </div>
      {q.isError ? (
        <div className="flex items-center justify-between gap-3 p-3 text-xs">
          <span className="text-sev-critical">
            {q.error instanceof Error ? q.error.message : "Couldn't load holds."}
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
      ) : holds.length === 0 ? (
        <EmptyState
          title="No held symptoms yet"
          hint="Downstream symptom pages held behind a firing upstream cause appear here, reviewable and reversible."
        />
      ) : (
        <>
          <table className="ddt">
            <thead>
              <tr>
                <th>Downstream</th>
                <th>Upstream</th>
                <th className="w-24">Severity</th>
                <th className="w-16 text-right">Holds</th>
                <th className="w-32">Last seen</th>
                <th className="w-28 text-right">Actions</th>
              </tr>
            </thead>
            <tbody>
              {holds.map((h) => (
                <tr key={h.id}>
                  <td className="font-medium text-ink-100">
                    <button
                      type="button"
                      className="text-left hover:text-link hover:underline"
                      onClick={() => setPeekId(h.id)}
                      title="View held symptom detail"
                    >
                      {displayService(h.downstream)}
                    </button>
                  </td>
                  <td className="text-ink-200">{displayService(h.upstream)}</td>
                  <td>
                    <SeverityBadge severity={h.severity} />
                  </td>
                  <td className="text-right tabular-nums text-ink-200">
                    {h.hold_count}
                  </td>
                  <td className="text-2xs text-ink-300" title={fmtAbs(h.last_seen)}>
                    {fmtRel(h.last_seen)}
                  </td>
                  <td className="text-right">
                    <button
                      type="button"
                      className="btn px-2 py-1 text-2xs"
                      data-testid={`alert-fatigue-hold-reclaim-${h.id}`}
                      disabled={reclaim.isPending}
                      onClick={() => reclaim.mutate(h.id)}
                      title="Release this symptom to the on-call channel — it should page"
                    >
                      Reclaim
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
                data-testid="alert-fatigue-holds-more"
                disabled={q.isFetchingNextPage}
                onClick={() => q.fetchNextPage()}
              >
                {q.isFetchingNextPage ? "Loading…" : "Load more"}
              </button>
            </div>
          )}
        </>
      )}

      <PeekPanel
        open={Boolean(peek)}
        onClose={() => setPeekId(null)}
        title="Held symptom detail"
      >
        {peek && (
          <dl className="grid gap-3">
            <PeekField label="Downstream">
              {displayService(peek.downstream)}
            </PeekField>
            <PeekField label="Upstream (cause)">
              {displayService(peek.upstream)}
            </PeekField>
            <PeekField label="Upstream incident">
              {peek.incident_id || "—"}
            </PeekField>
            <PeekField label="Severity">
              <SeverityBadge severity={peek.severity} />
            </PeekField>
            <PeekField label="Source">{peek.source || "—"}</PeekField>
            <PeekField label="Fingerprint">
              <span className="break-all font-mono text-2xs">
                {peek.fingerprint}
              </span>
            </PeekField>
            <PeekField label="Hold count">{peek.hold_count}</PeekField>
            <PeekField label="Routed channel">
              {peek.routed_channel || "—"}
            </PeekField>
            <PeekField label="First seen">
              <span title={fmtAbs(peek.first_seen)}>
                {fmtRel(peek.first_seen)}
              </span>
            </PeekField>
            <PeekField label="Last seen">
              <span title={fmtAbs(peek.last_seen)}>
                {fmtRel(peek.last_seen)}
              </span>
            </PeekField>
            <PeekField label="Alert content (redacted)">
              <pre className="max-h-72 overflow-auto whitespace-pre-wrap break-all rounded-md border border-ink-600 bg-ink-950/40 p-2 font-mono text-2xs text-ink-200">
                {peek.alert_content
                  ? JSON.stringify(peek.alert_content, null, 2)
                  : "—"}
              </pre>
            </PeekField>
          </dl>
        )}
      </PeekPanel>
    </div>
  );
}