import { useMemo, useState } from "react";
import { useSearchParams } from "react-router-dom";
import {
  useInfiniteQuery,
  useMutation,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { BellOff, Info, Loader2, X } from "lucide-react";

import {
  ApiError,
  api,
  type AlertFatigueConfig,
  type AlertFatigueFinding,
  type AlertFatigueSort,
  type AlertFatigueSortDir,
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
import { SegmentedControl } from "@/components/SegmentedControl";
import { SkRows } from "@/components/Skeleton";

// AlertFatiguePage — the operator surface for the Enterprise alert-fatigue
// feature. Modeled on the SLI/SLO auto-define page: a default-OFF master enable
// switch, and everything below it (the "require review before spam" switch, the
// noise-analytics strip, and the fingerprint review table) is hidden until the
// feature is enabled. All alert-fatigue CONFIGURATION — the fatigue channel,
// correlation, and dependency-aware suppression — lives in the Admin page's
// AlertFatigueSettings control; this page keeps only the operator's day-to-day
// review + monitoring surfaces.
//
// Gated exactly like the AI-settings / channel-settings controls on the
// caller's effective RBAC role (useEffectiveRole → adminGateState): a community
// binary renders the Enterprise upsell, a gateway-secret operator is asked to
// sign in, a viewer/responder gets the read-only "requires admin" notice, and
// only admin/owner reach the live controls. Every endpoint is enterprise +
// runtime:manage gated server-side, so the SPA fails closed here before it ever
// issues a privileged request.

const PAGE_TITLE = "Alert fatigue";

const LOCKED_TITLE = "Alert fatigue is an Enterprise capability";
const LOCKED_BODY =
  "Alert fatigue deduplicates repeat alerts and suppresses the noise so your " +
  "on-call channel stays clean — with a reviewable record of every fingerprint " +
  "so you can reclaim anything that was not actually noise. Optionally, send " +
  "the suppressed alerts to a channel of your choice from Admin settings.";

// PENDING_REVIEW_NOTE is the exact operator guidance shown beside the
// "Require review before spam" switch (per the implementation plan §4.1).
const PENDING_REVIEW_NOTE =
  "Alerts are auto-marked as spam by default — some alerts may stop being " +
  "sent. If you notice alerts missing and want to approve them before " +
  "they're marked as spam, enable pending review.";

const PAGE_SIZE = 50;

// FLOORED_SUPPRESS_HINT is the short explanation shown wherever a floored row's
// suppression action is blocked or its "fatigued" status could read as silenced.
const FLOORED_SUPPRESS_HINT =
  "High and critical alerts always page and can't be suppressed.";

// FINGERPRINTS_ANCHOR_ID lets the top-noisy drill-down scroll the Fingerprints
// table into view after it flips the status tab + service filter.
const FINGERPRINTS_ANCHOR_ID = "alert-fatigue-fingerprints";

// STATUS_FILTERS are the review-table filter options. `tracking` lists the
// still-paging rows (recorded, never fatigued) so the operator can find and
// suppress a noisy service; the other tabs list reviewable (non-tracking) rows.
const STATUS_FILTERS: Array<{ value: string; label: string }> = [
  { value: "", label: "All" },
  { value: "tracking", label: "Tracking" },
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

  const save = useMutation({
    mutationFn: (next: AlertFatigueConfig) => api.setAlertFatigueConfig(next),
    onSuccess: (data) => {
      // Share the config key + invalidate so the Admin control reflects an
      // Enable / Require-review change without a reload.
      qc.setQueryData(["alert-fatigue-config"], data);
      qc.invalidateQueries({ queryKey: ["alert-fatigue-config"] });
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
  // Read-modify-write: read the FRESHEST cached config (not the render-time
  // closure), merge only the fields this surface edits, then PUT the whole
  // object so a sibling field is never clobbered. The shared config key is
  // invalidated on success so any other reader stays in sync.
  const patch = (partial: Partial<AlertFatigueConfig>) => {
    setMsg(null);
    const current =
      qc.getQueryData<AlertFatigueConfig>(["alert-fatigue-config"]) ?? config;
    save.mutate({ ...current, ...partial });
  };

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
              When enabled, repeat alerts are auto-marked as spam using agent algorithms.
            </div>
          </div>
        </div>

        {config.enabled && (
          <div className="mt-4 grid gap-4 border-t border-ink-700 pt-4">
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
          matching the pending-review gating. The fatigue channel, correlation,
          and dependency CONFIG live on the Admin page's AlertFatigueSettings
          control; this page keeps the operator's review + monitoring surfaces. */}
      {config.enabled && (
        <>
          <AnalyticsStrip />
          <ReviewTable />
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
  const [params, setParams] = useSearchParams();
  const statusFilter = params.get("status") ?? "";
  const serviceFilter = params.get("service") ?? "";
  const [sort, setSort] = useState<AlertFatigueSort>("last_seen");
  const [dir, setDir] = useState<AlertFatigueSortDir>("desc");
  const [peekId, setPeekId] = useState<string | null>(null);

  // Per-status counts annotate the status tabs as badges; sourced from the SAME
  // analytics read-model (default 7d window) the noise strip reads, so the
  // shared react-query cache serves both with a single fetch. Missing status → 0.
  const analytics = useAlertFatigueAnalytics(DEFAULT_ANALYTICS_WINDOW);
  const byStatus = analytics.data?.by_status;

  // statusOptions carries each reviewable status's count as a badge on its tab
  // (the "All" tab stays badge-less), matching the other list pages' pattern.
  const statusOptions = useMemo(
    () =>
      STATUS_FILTERS.map((f) =>
        f.value === "" ? f : { ...f, badge: byStatus?.[f.value] ?? 0 },
      ),
    [byStatus],
  );

  const q = useInfiniteQuery({
    queryKey: [
      "alert-fatigue-fingerprints",
      statusFilter,
      serviceFilter,
      sort,
      dir,
    ],
    queryFn: ({ pageParam }) =>
      api.listAlertFatigueFingerprints({
        status: statusFilter || undefined,
        service: serviceFilter || undefined,
        sort,
        dir,
        page: pageParam,
        pageSize: PAGE_SIZE,
      }),
    initialPageParam: 1,
    getNextPageParam: (last) =>
      last.page * last.page_size < last.total ? last.page + 1 : undefined,
  });

  // clearService drops the service drill-down param (the top-noisy filter chip)
  // while leaving the active status tab in place.
  const clearService = () => {
    const next = new URLSearchParams(params);
    next.delete("service");
    setParams(next, { replace: true });
  };

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

  // totalNoun labels the Fingerprints header count by the active filter so it
  // never reads as a grand total that contradicts the analytics "Alerts
  // tracked" number. The "All" tab lists REVIEWABLE (non-tracking) rows, so its
  // count is a subset of the analytics total; the per-status pills below spell
  // out the full composition via the per-status tab badges.
  const totalNoun =
    statusFilter === ""
      ? "reviewable"
      : (
          STATUS_FILTERS.find((f) => f.value === statusFilter)?.label ??
          "matching"
        ).toLowerCase();

  return (
    <div id={FINGERPRINTS_ANCHOR_ID} className="card overflow-hidden">
      <div className="flex flex-wrap items-center justify-between gap-2 border-b border-ink-700 px-4 py-3">
        <div>
          <h2 className="text-sm font-semibold text-ink-50">
            Fingerprints
            {total !== undefined && (
              <span className="ml-2 text-2xs font-normal text-ink-400">
                {total.toLocaleString()} {totalNoun}
              </span>
            )}
          </h2>
          {serviceFilter && (
            <button
              type="button"
              data-testid="alert-fatigue-service-chip"
              aria-label={`Clear service filter: ${displayService(serviceFilter)}`}
              onClick={clearService}
              className="mt-2 inline-flex items-center gap-1.5 rounded-full border border-ink-600 bg-ink-950/40 px-2.5 py-1 text-2xs text-ink-200 hover:border-ink-500 hover:text-ink-50"
            >
              <span>
                Service:{" "}
                <span className="font-semibold">
                  {displayService(serviceFilter)}
                </span>
              </span>
              <X size={12} aria-hidden />
            </button>
          )}
        </div>
        <div className="flex items-center gap-2 text-2xs text-ink-400">
          <SegmentedControl
            param="status"
            defaultValue=""
            aria-label="Status filter"
            options={statusOptions}
          />
        </div>
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
            <SkRows rows={4} cols={9} />
          </tbody>
        </table>
      ) : items.length === 0 ? (
        <EmptyState
          title="No fingerprints yet"
          hint={
            statusFilter === "tracking"
              ? "Tracking alerts appear here as the interceptor counts repeat alerts that are still being sent. Suppress a noisy one with “Mark as spam”."
              : "Fatigued and pending-review fingerprints appear here as the interceptor records repeat, low-signal alerts."
          }
        />
      ) : (
        <>
          <table className="ddt w-full table-fixed">
            <thead>
              <tr>
                <th className="w-[16%]">Service</th>
                <th className="w-[10%]">Source</th>
                <th className="w-[9%]">Severity</th>
                <th className="w-[8%]">
                  <SortHeader
                    label="Priority"
                    col="priority"
                    sort={sort}
                    dir={dir}
                    onSort={toggleSort}
                  />
                </th>
                <th className="w-[7%] text-right">
                  <SortHeader
                    label="Repeats"
                    col="repeat_count"
                    sort={sort}
                    dir={dir}
                    onSort={toggleSort}
                    align="right"
                  />
                </th>
                <th className="w-[11%]">
                  <SortHeader
                    label="Last seen"
                    col="last_seen"
                    sort={sort}
                    dir={dir}
                    onSort={toggleSort}
                  />
                </th>
                <th className="w-[11%]">Status</th>
                <th className="w-[12%]">Routed channel</th>
                <th className="w-[16%] text-right">Actions</th>
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
          {(q.isFetchingNextPage || q.hasNextPage) && (
            <div className="flex items-center justify-center gap-1.5 border-t border-ink-600 px-3 py-2 text-2xs text-ink-400">
              {q.isFetchingNextPage ? (
                <>
                  <Loader2 size={12} className="animate-spin" />
                  Loading more…
                </>
              ) : (
                <button
                  type="button"
                  className="text-brand-300 hover:underline"
                  data-testid="alert-fatigue-load-more"
                  onClick={() => q.fetchNextPage()}
                >
                  Load more ({total?.toLocaleString() ?? ""} total)
                </button>
              )}
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
  if (s === "tracking") return <Pill>Tracking</Pill>;
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
  // A floored row is one the interceptor always pages (high/critical or a
  // severity-only floor), so it can never actually be suppressed. The backend
  // reports this authoritatively via `floor`. Its "Mark as spam" action is
  // disabled and, if it somehow already reads "fatigued", the status is
  // annotated so it doesn't look silenced.
  const floored = row.floor === true;

  // MarkAsSpam is the "Mark as spam" (confirm) action, shared by tracking and
  // reclaimed rows. On a floored row it renders disabled with an explanation
  // instead of firing a suppression that would never take effect.
  const markAsSpam = floored ? (
    <span
      data-testid="alert-fatigue-mark-spam-floored"
      title={FLOORED_SUPPRESS_HINT}
      aria-label={`Cannot be suppressed: ${FLOORED_SUPPRESS_HINT}`}
      className="inline-flex max-w-full items-center gap-1 truncate text-2xs text-ink-500"
    >
      <Info size={11} aria-hidden />
      <span className="truncate">Always pages</span>
    </span>
  ) : (
    <button
      type="button"
      className="btn px-2 py-1 text-2xs"
      disabled={acting}
      onClick={onConfirm}
    >
      Mark as spam
    </button>
  );

  return (
    <tr>
      <td className="font-mono text-2xs text-ink-200">
        <button
          type="button"
          className="block w-full truncate text-left hover:text-link hover:underline"
          onClick={onPeek}
          title={displayService(row.service)}
        >
          {displayService(row.service)}
        </button>
      </td>
      <td className="text-2xs text-ink-300">
        <span className="block truncate" title={row.source || undefined}>
          {row.source || "—"}
        </span>
      </td>
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
        <div className="flex min-w-0 flex-col items-start gap-0.5">
          <StatusPill status={row.status} />
          {floored && s === "fatigued" && (
            <span
              data-testid="alert-fatigue-still-pages"
              title={FLOORED_SUPPRESS_HINT}
              className="inline-flex max-w-full items-center gap-1 truncate text-2xs text-sev-warn"
            >
              <Info size={11} aria-hidden />
              still pages (high priority)
            </span>
          )}
        </div>
      </td>
      <td className="text-2xs text-ink-300">
        <span className="block truncate" title={row.routed_channel || undefined}>
          {row.routed_channel || "—"}
        </span>
      </td>
      <td className="text-right">
        <div className="flex flex-wrap justify-end gap-1.5">
          {s === "tracking" && markAsSpam}
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
          {s === "reclaimed" && markAsSpam}
        </div>
      </td>
    </tr>
  );
}

// ---------------------------------------------------------------------------
// Shared section building blocks
// ---------------------------------------------------------------------------

// SectionShell is the card + heading wrapper the analytics section uses so the
// page reads as labelled sections, not a wall of controls. `action` is an
// optional right-aligned slot on the heading row (used for the window tabs).
function SectionShell({
  title,
  icon,
  children,
  testId,
  action,
}: {
  title: string;
  icon?: React.ReactNode;
  children: React.ReactNode;
  testId?: string;
  action?: React.ReactNode;
}) {
  return (
    <div className="card p-4" data-testid={testId}>
      <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
        <h2 className="flex items-center gap-2 text-sm font-semibold text-ink-50">
          {icon}
          {title}
        </h2>
        {action}
      </div>
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

// DEFAULT_ANALYTICS_WINDOW is the window both the analytics strip and the
// Fingerprints per-status counts read by default, so they share one react-query
// cache entry (["alert-fatigue-analytics", "7d"]) and issue a single fetch.
const DEFAULT_ANALYTICS_WINDOW: "7d" | "30d" = "7d";

// useAlertFatigueAnalytics reads the per-org noise read-model over one window.
// Keyed by window so the analytics strip and the Fingerprints per-status counts
// (both default to 7d) reuse the SAME cache entry — one fetch, no backend change.
function useAlertFatigueAnalytics(window: "7d" | "30d") {
  return useQuery({
    queryKey: ["alert-fatigue-analytics", window],
    queryFn: () => api.getAlertFatigueAnalytics(window),
    retry: false,
    staleTime: 30_000,
  });
}

function AnalyticsStrip() {
  const [window, setWindow] = useState<"7d" | "30d">(DEFAULT_ANALYTICS_WINDOW);
  const [params, setParams] = useSearchParams();
  const q = useAlertFatigueAnalytics(window);

  // filterByService drills the Fingerprints table into one noisy service. The
  // high-repeat rows live in `tracking` (still paging, never fatigued), so it
  // switches the status tab there, sets the URL-synced `service` param, and
  // scrolls the table into view.
  const filterByService = (service: string) => {
    const next = new URLSearchParams(params);
    next.set("service", service);
    next.set("status", "tracking");
    setParams(next, { replace: true });
    if (typeof document !== "undefined") {
      requestAnimationFrame(() => {
        const el = document.getElementById(FINGERPRINTS_ANCHOR_ID);
        if (el && typeof el.scrollIntoView === "function") {
          el.scrollIntoView({ behavior: "smooth", block: "start" });
        }
      });
    }
  };

  const windowTabs = (
    <div
      role="tablist"
      aria-label="Analytics window"
      data-testid="alert-fatigue-analytics-window"
      className="inline-flex rounded-control border border-ink-500 bg-surface-raised p-0.5"
    >
      {ANALYTICS_WINDOWS.map((w) => {
        const active = window === w.value;
        return (
          <button
            key={w.value}
            type="button"
            role="tab"
            aria-selected={active}
            aria-label={w.label}
            onClick={() => setWindow(w.value)}
            className={`inline-flex min-h-7 items-center rounded px-2.5 py-1 text-xs font-medium transition-colors ${
              active
                ? "bg-accent-subtle text-ink-50"
                : "text-ink-300 hover:text-ink-100"
            }`}
          >
            {w.label}
          </button>
        );
      })}
    </div>
  );

  return (
    <SectionShell
      title="Noise analytics"
      testId="alert-fatigue-analytics"
      action={windowTabs}
    >
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
          <div className="grid grid-flow-col auto-cols-fr gap-3 overflow-x-auto">
            <Stat
              label="Alerts tracked"
              value={q.data.total.toLocaleString()}
              hint="across all statuses"
              testId="alert-fatigue-stat-total"
            />
            <Stat label="Noise ratio" value={pct(q.data.noise_ratio)} />
            {/* Honest split of the fatigued count: alerts ROUTED to a custom
                fatigue channel vs SILENTLY SUPPRESSED (dropped). Each renders
                only when the read-model exposes it; when neither is present the
                UI falls back to one plainly-labelled "Fatigued" stat rather than
                the old "Diverted" wording that implied every fatigued alert was
                sent somewhere. */}
            {q.data.routed !== undefined && (
              <Stat
                label="Routed to channel"
                value={q.data.routed.toLocaleString()}
                hint="sent to your fatigue channel"
                testId="alert-fatigue-stat-routed"
              />
            )}
            {q.data.suppressed !== undefined && (
              <Stat
                label="Suppressed"
                value={q.data.suppressed.toLocaleString()}
                hint="dropped, not sent anywhere"
                testId="alert-fatigue-stat-suppressed"
              />
            )}
            {q.data.routed === undefined && q.data.suppressed === undefined && (
              <Stat
                label="Fatigued"
                value={q.data.diverted.toLocaleString()}
                hint="kept off on-call"
                testId="alert-fatigue-stat-fatigued"
              />
            )}
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
                  <li key={s.service}>
                    <button
                      type="button"
                      data-testid={`alert-fatigue-top-noisy-${s.service}`}
                      aria-label={`Filter fingerprints by service ${displayService(s.service)}`}
                      onClick={() => filterByService(s.service)}
                      className="flex w-full items-center justify-between gap-3 rounded px-1.5 py-1 text-left text-2xs hover:bg-ink-800/60 focus-visible:outline focus-visible:outline-1 focus-visible:outline-link"
                    >
                      <span className="truncate text-ink-200">
                        {displayService(s.service)}
                      </span>
                      <span className="shrink-0 tabular-nums text-ink-400">
                        {s.repeat_total.toLocaleString()} repeats ·{" "}
                        {s.findings.toLocaleString()} fingerprints
                      </span>
                    </button>
                  </li>
                ))}
              </ul>
              <p className="mt-1.5 text-2xs text-ink-500">
                Click a service to see its still-tracking alerts and mark noise
                as spam.
              </p>
            </div>
          )}
        </>
      )}
    </SectionShell>
  );
}

function Stat({
  label,
  value,
  hint,
  testId,
}: {
  label: string;
  value: string;
  hint?: string;
  testId?: string;
}) {
  return (
    <div
      className="rounded-md border border-ink-700 bg-ink-950/30 p-3"
      data-testid={testId}
    >
      <div className="text-2xs uppercase tracking-wide text-ink-400">
        {label}
      </div>
      <div className="mt-1 text-lg font-semibold tabular-nums text-ink-50">
        {value}
      </div>
      {hint && <div className="mt-0.5 text-2xs text-ink-500">{hint}</div>}
    </div>
  );
}
