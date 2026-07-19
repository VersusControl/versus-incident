import { useEffect, useMemo, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import {
  useMutation,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import {
  CheckCircle2,
  ChevronDown,
  ChevronUp,
  Eye,
  Loader2,
  Search,
  UserPlus,
} from "lucide-react";
import { api, type IncidentIndex, type IncidentSummary, type IntakeSettings } from "@/lib/api";
import { fmtAbs, fmtRel, incidentTitle, truncate } from "@/lib/format";
import { useTableKeys } from "@/lib/hooks";
import {
  INCIDENT_STATUS_VALUES,
  filterIncidentsByText,
  formatOriginCounts,
  incidentResetKey,
  matchesStatus,
  normalizeOrigin,
  originLabel,
  type IncidentStatusFilter,
} from "@/lib/incidentList";
import { usePagination } from "@/lib/pagination";
import { useBulkSelection } from "@/lib/useBulkSelection";
import { SortHeader } from "@/components/SortHeader";
import { tsValue, useSortableRows } from "@/lib/sortRows";
import { TopBar } from "@/components/TopBar";
import { Pill, SourceBadge } from "@/components/Pill";
import { EmptyState, EmptyValue } from "@/components/feedback";
import { AssignDialog } from "@/components/AssignDialog";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { Pagination } from "@/components/Pagination";
import { PeekPanel, PeekField } from "@/components/PeekPanel";
import {
  BulkActionBar,
  RowSelectCheckbox,
  SelectAllCheckbox,
} from "@/components/BulkActionBar";
import { SegmentedControl } from "@/components/SegmentedControl";
import { SeverityBadge } from "@/components/SeverityBadge";
import { SkLine, SkRows } from "@/components/Skeleton";
import { RetryableError } from "@/components/RetryableError";
import { ReportsButton } from "@/components/ReportsDialog";
import { useToast } from "@/components/toastContext";

type StatusFilter = IncidentStatusFilter;

const STATUS_VALUES = INCIDENT_STATUS_VALUES;

// Column count for colSpan cells (skeleton / empty / notify-error rows):
// select · service · sev · when · title · channels · assigned · notify · status · id · actions
const COLS = 11;

// useDebounced returns a value that only updates after `delay` ms of no
// change — keeps server-side search from firing on every keystroke.
function useDebounced<T>(value: T, delay = 300): T {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const t = setTimeout(() => setDebounced(value), delay);
    return () => clearTimeout(t);
  }, [value, delay]);
  return debounced;
}

// WebhookAutoResolveToggle is the single interactive control on the webhook
// origin tab: the auto-resolve toggle, backed by GET/PUT
// /api/admin/incidents/intake-settings. It saves on toggle (there is one
// boolean, so no explicit Save button) and mirrors the report settings save
// flow — a toast on success/failure. It is only mounted on the webhook tab, so
// the intake request never fires on the AI-detected tab.
function WebhookAutoResolveToggle() {
  const qc = useQueryClient();
  const toast = useToast();

  const intake = useQuery({
    queryKey: ["intake-settings"],
    queryFn: api.getIntakeSettings,
    staleTime: 30_000,
  });

  const save = useMutation({
    mutationFn: (s: IntakeSettings) => api.updateIntakeSettings(s),
    onSuccess: (saved) => {
      qc.setQueryData(["intake-settings"], saved);
      toast.push({ tone: "ok", title: "Intake settings saved" });
    },
    onError: (err) => {
      toast.push({
        tone: "error",
        title: "Couldn't save intake settings",
        description: err instanceof Error ? err.message : String(err),
      });
    },
  });

  // Defaults ON — the backend default — until the current value loads.
  const value = intake.data?.auto_resolve_webhook ?? true;
  const busy = intake.isLoading || save.isPending;

  return (
    <label
      className="flex items-center gap-1.5 whitespace-nowrap text-sm text-ink-100"
      title="Webhook incidents are stored as resolved on arrival — alerting and on-call still fire. Default on."
    >
      <input
        type="checkbox"
        data-testid="intake-auto-resolve"
        checked={value}
        disabled={busy}
        aria-disabled={busy}
        onChange={(e) => save.mutate({ auto_resolve_webhook: e.target.checked })}
      />
      Auto-resolve
      {save.isPending && (
        <Loader2 size={13} className="animate-spin text-ink-400" />
      )}
    </label>
  );
}

// IncidentsPage shows the persisted incident history pulled from the
// storage backend. Newest first, with a URL-synced status filter
// (?status=, default open — the URL is the state, so tiles and banners can
// deep-link) and a free-text search. When the backend supports server-side
// search (Postgres), the query runs on the server; otherwise it falls back
// to filtering the already-loaded page client-side.
export function IncidentsPage() {
  const qc = useQueryClient();
  const toast = useToast();
  const [q, setQ] = useState("");
  const [params] = useSearchParams();
  const rawStatus = params.get("status") ?? "open";
  const status: StatusFilter = STATUS_VALUES.includes(
    rawStatus as StatusFilter,
  )
    ? (rawStatus as StatusFilter)
    : "open";
  // Origin tab: AI-detected is the high-signal DEFAULT so a flood of
  // webhook incidents never buries it; webhook is one click away. The
  // rows are fetched per-origin so the AI tab never loads the firehose.
  const origin = normalizeOrigin(params.get("origin"));

  // Dialog state lives at page level so the table-level keyboard shortcuts
  // (a = assign, r = resolve) can open them for the active row.
  const [assignFor, setAssignFor] = useState<IncidentSummary | null>(null);
  const [resolveFor, setResolveFor] = useState<IncidentSummary | null>(null);
  const [expandedNotify, setExpandedNotify] = useState<Set<string>>(
    () => new Set(),
  );
  // Peek + bulk state. The eye opens a detail slide-out (peekId); the action
  // bar's Assign/Resolve capture the current selection into these when picked
  // (null = closed), so a single-row action is just a one-row selection.
  const [peekId, setPeekId] = useState<string | null>(null);
  const [bulkAssignIds, setBulkAssignIds] = useState<string[] | null>(null);
  const [bulkResolveIds, setBulkResolveIds] = useState<string[] | null>(null);

  // Probe backend capabilities once; default to no server search until
  // known. The probe degrades silently — when it fails the page simply
  // keeps the client-side filter, which always works.
  const { data: caps } = useQuery({
    queryKey: ["capabilities"],
    queryFn: () => api.capabilities(),
    staleTime: 5 * 60_000,
  });
  const searchSupported = caps?.search ?? false;

  const debouncedQ = useDebounced(q, 300);
  // Untitled incidents DISPLAY as "#f9b0dadc" (incidentTitle), so pasting
  // that handle back into search must work — strip the "#" the backend
  // and the raw id don't have.
  const trimmed = debouncedQ.trim().replace(/^#/, "");
  const useServerSearch = searchSupported && trimmed !== "";

  const { data, isLoading, isError, error, refetch, isRefetching } = useQuery({
    queryKey: useServerSearch
      ? ["incident-index", "search", trimmed, origin]
      : ["incident-index", origin],
    queryFn: () =>
      useServerSearch
        ? api.searchIncidentsIndex(trimmed, { origin })
        : api.listIncidentsIndex({ origin }),
  });
  // rows for the active origin tab; originCounts is whole-set so the
  // top-bar shows both feeds separately regardless of the active tab.
  const originCounts = data?.counts;

  // Roster lookups are shared by every row — resolve them once here.
  const teamsQ = useQuery({ queryKey: ["teams"], queryFn: api.listTeams });
  const membersQ = useQuery({
    queryKey: ["members"],
    queryFn: api.listMembers,
  });
  const teamById = useMemo(() => {
    const m = new Map<string, string>();
    for (const t of teamsQ.data ?? []) m.set(t.id, t.name);
    return m;
  }, [teamsQ.data]);
  const memberById = useMemo(() => {
    const m = new Map<string, string>();
    for (const x of membersQ.data ?? []) m.set(x.id, x.name);
    return m;
  }, [membersQ.data]);
  const rosterLoading = teamsQ.isLoading || membersQ.isLoading;

  // Text filter first (counts per status are computed on this set so the
  // segmented-control badges reflect the current search), then status.
  const textFiltered = useMemo(
    // When the server already ran the text search, don't re-filter on text
    // (it matches fields the client can't see, e.g. payload body).
    () => filterIncidentsByText(data?.incidents, q, useServerSearch),
    [data?.incidents, q, useServerSearch],
  );

  const counts = useMemo(
    () => ({
      open: textFiltered.filter((i) => matchesStatus(i, "open")).length,
      acked: textFiltered.filter((i) => matchesStatus(i, "acked")).length,
      resolved: textFiltered.filter((i) => matchesStatus(i, "resolved")).length,
    }),
    [textFiltered],
  );

  const filtered = useMemo(
    () => textFiltered.filter((i) => matchesStatus(i, status)),
    [textFiltered, status],
  );

  // Click-to-sort on When (by the real created_at timestamp, not the "2m ago"
  // string). Default: newest first, matching the incoming server order.
  const sorted = useSortableRows(
    filtered,
    { when: (i: IncidentSummary) => tsValue(i.created_at) },
    { key: "when", dir: "desc" },
  );

  // Paginate at 100/page AFTER filter/search. Reset to page 1 whenever the
  // origin tab, status filter, search text, or sort changes so a filter never
  // strands the operator on a now-empty page.
  const pg = usePagination(sorted.rows, {
    resetKey: `${incidentResetKey(origin, status, trimmed)}|${sorted.signature}`,
  });

  // ----- selection + action bar -------------------------------------------
  // The SAME checkbox model the learned-signal tables use: a select-all
  // checkbox in the header, a checkbox per row, and a bar that APPEARS on
  // selection surfacing the per-row actions (Assign / Resolve) as bulk actions.
  // Selection resets on origin / status / search / PAGE change.
  const pageKeys = useMemo(() => pg.pageItems.map((i) => i.id), [pg.pageItems]);
  const bulk = useBulkSelection(
    pageKeys,
    `${incidentResetKey(origin, status, trimmed)}|${sorted.signature}|${pg.page}`,
  );
  const bulkActions = [
    { id: "assign", label: "Assign" },
    { id: "resolve", label: "Resolve" },
  ];

  // Resolve is optimistic (§2.4): cache flips immediately, rollback +
  // error toast with Retry on failure, invalidate on settle.
  const resolveMut = useMutation({
    mutationFn: (i: IncidentSummary) => api.resolveIncident(i.id),
    onMutate: async (i) => {
      setResolveFor(null);
      await qc.cancelQueries({ queryKey: ["incidents"] });
      await qc.cancelQueries({ queryKey: ["incident-index"] });
      // Two cache shapes carry incidents: the plain arrays behind
      // ["incidents"] (TopBar/Sidebar/Now badges, analyses lookup) and
      // this page's origin index behind ["incident-index"]. Flip the
      // resolved row in both, snapshotting each for rollback.
      const prevArrays = qc.getQueriesData<IncidentSummary[]>({
        queryKey: ["incidents"],
      });
      const prevIndex = qc.getQueriesData<IncidentIndex>({
        queryKey: ["incident-index"],
      });
      const flip = (x: IncidentSummary) =>
        x.id === i.id
          ? { ...x, resolved: true, resolved_at: new Date().toISOString() }
          : x;
      qc.setQueriesData<IncidentSummary[]>({ queryKey: ["incidents"] }, (old) =>
        old?.map(flip),
      );
      qc.setQueriesData<IncidentIndex>(
        { queryKey: ["incident-index"] },
        (old) => (old ? { ...old, incidents: old.incidents.map(flip) } : old),
      );
      return { prevArrays, prevIndex };
    },
    onError: (err, i, ctx) => {
      ctx?.prevArrays.forEach(([key, snap]) => qc.setQueryData(key, snap));
      ctx?.prevIndex.forEach(([key, snap]) => qc.setQueryData(key, snap));
      toast.push({
        tone: "error",
        title: "Resolve failed",
        description: err.message,
        action: { label: "Retry", onClick: () => resolveMut.mutate(i) },
      });
    },
    onSuccess: (_data, i) => {
      toast.push({
        tone: "ok",
        title: "Incident resolved",
        description: truncate(incidentTitle(i), 60),
      });
      qc.invalidateQueries({ queryKey: ["incident", i.id] });
    },
    onSettled: () => {
      qc.invalidateQueries({ queryKey: ["incidents"] });
      qc.invalidateQueries({ queryKey: ["incident-index"] });
    },
  });

  // j/k move the active row; Enter opens the peek (the eye's action) — the
  // row itself is not a navigation control. a/r act on the active row (modals
  // trap focus, so these can't fire while a dialog is open). Scoped to the
  // rendered page so navigation stays bounded on a multi-thousand-row history.
  const { containerProps, rowProps } = useTableKeys({
    size: pg.pageItems.length,
    onOpen: (i) => {
      const row = pg.pageItems[i];
      if (row) setPeekId(row.id);
    },
    extra: (key, i) => {
      const row = pg.pageItems[i];
      if (!row) return;
      if (key === "a") {
        setAssignFor(row);
        return true;
      }
      if (key === "r" && !row.resolved) {
        setResolveFor(row);
        return true;
      }
    },
  });

  const toggleNotify = (id: string) =>
    setExpandedNotify((cur) => {
      const next = new Set(cur);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });

  // Map an id back to its row for the selection actions + peek. The whole
  // loaded set is the source so the peek survives a page/filter change.
  const byId = useMemo(() => {
    const m = new Map<string, IncidentSummary>();
    for (const i of data?.incidents ?? []) m.set(i.id, i);
    return m;
  }, [data?.incidents]);

  const onBulkAction = (spec: { id: string }) => {
    const ids = bulk.selectedKeys;
    if (ids.length === 0) return;
    if (spec.id === "assign") {
      // Keep the selection until the dialog finishes (onDone clears it).
      setBulkAssignIds([...ids]);
      return;
    }
    if (spec.id === "resolve") {
      // Resolve only the ones not already resolved; a confirm gates the write.
      const unresolved = ids.filter((id) => !byId.get(id)?.resolved);
      setBulkResolveIds(unresolved);
    }
  };

  const peek = peekId ? byId.get(peekId) : undefined;

  return (
    <>
      <TopBar
        title="Incidents"
        subtitle={formatOriginCounts(originCounts)}
      />

      <main className="flex-1 overflow-auto p-4 lg:p-6">
        <div className="mb-3 flex flex-wrap items-center gap-2">
          {/* Origin is the primary split: AI-detected (default) vs the
              inbound webhook/alert firehose, each with its whole-set
              count so neither buries the other. */}
          <SegmentedControl
            param="origin"
            defaultValue="ai_detect"
            aria-label="Filter incidents by origin"
            options={[
              {
                value: "ai_detect",
                label: originLabel("ai_detect"),
                badge: originCounts?.ai_detect,
              },
              {
                value: "webhook",
                label: originLabel("webhook"),
                badge: originCounts?.webhook,
              },
            ]}
          />
          <div className="relative max-w-md flex-1">
            <Search
              size={12}
              className="absolute left-2.5 top-1/2 -translate-y-1/2 text-ink-400"
            />
            <input
              data-page-search
              className="input pl-7"
              placeholder={
                searchSupported
                  ? "Search incidents (title, service, payload)…"
                  : "Filter loaded incidents by id, title or service…"
              }
              value={q}
              onChange={(e) => setQ(e.target.value)}
            />
          </div>
          <SegmentedControl
            param="status"
            defaultValue="open"
            aria-label="Filter incidents by status"
            options={[
              // Badges stay undefined until the query settles — counts
              // computed from [] mid-load would render a false "0 = all
              // quiet" row of badges next to skeleton rows.
              { value: "open", label: "Open", badge: data ? counts.open : undefined },
              { value: "acked", label: "Acked", badge: data ? counts.acked : undefined },
              {
                value: "resolved",
                label: "Resolved",
                badge: data ? counts.resolved : undefined,
              },
              { value: "all", label: "All", badge: data ? textFiltered.length : undefined },
            ]}
          />
          {/* Window-scoped incidents-analytics report — spans both origins, so
              it lives in the toolbar, not on any one incident/tab. Hidden when
              the runtime report setting is disabled (via capabilities). */}
          <div className="ml-auto flex items-center gap-3">
            {/* Auto-resolve is a webhook-origin concept only — it controls
                whether inbound webhook incidents land resolved. Mounted only on
                the webhook tab, so no intake request fires on the AI tab. */}
            {origin === "webhook" && <WebhookAutoResolveToggle />}
            <ReportsButton />
          </div>
        </div>

        {isError ? (
          <RetryableError
            context="Couldn't load incidents"
            error={error}
            onRetry={() => refetch()}
            retrying={isRefetching}
          />
        ) : (
          <div className="card overflow-hidden">
            {bulk.count > 0 && (
              <BulkActionBar
                count={bulk.count}
                actions={bulkActions}
                onAction={onBulkAction}
                onClear={bulk.clear}
                busy={resolveMut.isPending}
              />
            )}
            <div
              {...containerProps}
              role="region"
              aria-label="Incidents table — j/k to move, Enter to open"
              className="max-h-[calc(100vh-240px)] overflow-auto focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
            >
              <table className="ddt">
                <thead>
                  <tr>
                    <th className="w-8">
                      <SelectAllCheckbox
                        state={bulk.headerState}
                        onChange={bulk.toggleAll}
                      />
                    </th>
                    <th className="w-24" />
                    <th className="w-28">Service</th>
                    <th className="w-24">Severity</th>
                    <SortHeader
                      className="w-32"
                      label="When"
                      {...sorted.headerProps("when")}
                    />
                    <th>Title</th>
                    <th className="w-32">Channels</th>
                    <th className="w-32">Assigned</th>
                    <th className="w-24">Notify</th>
                    <th className="w-24">Status</th>
                    <th className="w-28">ID</th>
                  </tr>
                </thead>
                <tbody>
                  {isLoading && <SkRows rows={8} cols={COLS} />}
                  {!isLoading && filtered.length === 0 && (
                    <tr>
                      <td colSpan={COLS}>
                        <EmptyState
                          title={
                            status === "open"
                              ? "No open incidents"
                              : "No incidents"
                          }
                          hint={
                            q
                              ? "Try clearing the search."
                              : status === "open"
                                ? "Nothing needs attention right now."
                                : status !== "all"
                                  ? "Try a different status filter."
                                  : "Once an alert fires, it'll show up here."
                          }
                        />
                      </td>
                    </tr>
                  )}
                  {!isLoading &&
                    pg.pageItems.map((i, idx) => (
                      <IncidentRow
                        key={i.id}
                        i={i}
                        rowProps={rowProps(idx)}
                        teamById={teamById}
                        memberById={memberById}
                        rosterLoading={rosterLoading}
                        selected={bulk.isSelected(i.id)}
                        onToggleSelect={() => bulk.toggle(i.id)}
                        onPeek={() => setPeekId(i.id)}
                        notifyExpanded={expandedNotify.has(i.id)}
                        onToggleNotify={() => toggleNotify(i.id)}
                      />
                    ))}
                </tbody>
              </table>
            </div>
            <Pagination state={pg} />
            <div className="hidden border-t border-ink-600 px-3 py-1.5 text-2xs text-ink-400 md:block">
              j/k navigate · Enter open · a assign · r resolve · / search
            </div>
          </div>
        )}
      </main>

      {assignFor && (
        <AssignDialog
          incidentID={assignFor.id}
          initialTeamID={assignFor.assigned_team_id}
          initialMemberIDs={assignFor.assigned_member_ids}
          onClose={() => setAssignFor(null)}
        />
      )}
      {resolveFor && (
        <ConfirmDialog
          title="Resolve incident"
          message={
            <>
              Mark{" "}
              <span className="font-medium text-ink-50">
                {incidentTitle(resolveFor)}
              </span>{" "}
              as resolved? This stamps a resolved-at timestamp and cannot be
              undone from the UI today.
            </>
          }
          confirmLabel="Resolve"
          busy={resolveMut.isPending}
          onConfirm={() => resolveMut.mutate(resolveFor)}
          onClose={() => {
            if (!resolveMut.isPending) setResolveFor(null);
          }}
        />
      )}

      {/* Bulk assign — one team/member set applied to every selected incident,
          via the shared AssignDialog. onDone clears the selection. */}
      {bulkAssignIds && (
        <AssignDialog
          incidentID={bulkAssignIds}
          onClose={() => setBulkAssignIds(null)}
          onDone={() => bulk.clear()}
        />
      )}

      {/* Bulk resolve — a single confirm gates resolving the selection; each
          not-yet-resolved incident is resolved through the same mutation. */}
      {bulkResolveIds && (
        <ConfirmDialog
          title="Resolve incidents"
          message={
            bulkResolveIds.length === 0 ? (
              <>All selected incidents are already resolved.</>
            ) : (
              <>
                Mark{" "}
                <span className="font-medium text-ink-50">
                  {bulkResolveIds.length}
                </span>{" "}
                {bulkResolveIds.length === 1 ? "incident" : "incidents"} as
                resolved? This stamps a resolved-at timestamp and cannot be
                undone from the UI today.
              </>
            )
          }
          confirmLabel="Resolve"
          busy={resolveMut.isPending}
          onConfirm={() => {
            for (const id of bulkResolveIds) {
              const inc = byId.get(id);
              if (inc && !inc.resolved) resolveMut.mutate(inc);
            }
            setBulkResolveIds(null);
            bulk.clear();
          }}
          onClose={() => {
            if (!resolveMut.isPending) setBulkResolveIds(null);
          }}
        />
      )}

      {/* Peek — inspect an incident without leaving the list; the footer link
          opens the full incident page. */}
      {peek && (
        <PeekPanel
          open
          onClose={() => setPeekId(null)}
          title={truncate(incidentTitle(peek), 60)}
          footer={
            <Link
              to={`/incidents/${peek.id}`}
              className="btn"
              onClick={() => setPeekId(null)}
            >
              Open full page ↗
            </Link>
          }
        >
          <IncidentPeekBody
            i={peek}
            teamById={teamById}
            memberById={memberById}
            onAssign={() => {
              setPeekId(null);
              setAssignFor(peek);
            }}
            onResolve={() => {
              setPeekId(null);
              setResolveFor(peek);
            }}
          />
        </PeekPanel>
      )}
    </>
  );
}

// IncidentPeekBody — the read-only detail shown in the slide-out, plus the same
// Assign / Resolve actions the row offers so the peek is a full stand-in for the
// row.
function IncidentPeekBody({
  i,
  teamById,
  memberById,
  onAssign,
  onResolve,
}: {
  i: IncidentSummary;
  teamById: Map<string, string>;
  memberById: Map<string, string>;
  onAssign: () => void;
  onResolve: () => void;
}) {
  const statusLabel = i.resolved ? "resolved" : i.acked_at ? "acked" : "open";
  const statusTone = i.resolved ? "good" : i.acked_at ? "accent" : "bad";
  const teamName = i.assigned_team_id
    ? teamById.get(i.assigned_team_id)
    : undefined;
  const memberNames = (i.assigned_member_ids ?? []).map(
    (id) => memberById.get(id) ?? id.slice(0, 8),
  );
  const hasAssignment =
    !!i.assigned_team_id || (i.assigned_member_ids ?? []).length > 0;

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-2">
        <Pill tone={statusTone}>{statusLabel}</Pill>
        <SourceBadge source={i.source} />
      </div>

      <dl className="grid grid-cols-2 gap-x-4 gap-y-2 text-xs">
        <PeekField label="Service">
          {i.service && i.service !== "_unknown" ? i.service : "—"}
        </PeekField>
        <PeekField label="When">
          <span title={fmtAbs(i.created_at)}>{fmtRel(i.created_at)}</span>
        </PeekField>
        <PeekField label="Channels">
          {i.channels_notified && i.channels_notified.length > 0 ? (
            <span className="flex flex-wrap gap-1">
              {i.channels_notified.map((c) => (
                <Pill key={c}>{c}</Pill>
              ))}
            </span>
          ) : (
            "—"
          )}
        </PeekField>
        <PeekField label="Assigned">
          {hasAssignment ? (
            <span className="flex flex-wrap gap-1">
              {teamName && <Pill tone="accent">{teamName}</Pill>}
              {memberNames.map((n, idx) => (
                <Pill key={idx}>{n}</Pill>
              ))}
            </span>
          ) : (
            "—"
          )}
        </PeekField>
        <PeekField label="Notify">{i.notify_status || "—"}</PeekField>
        <PeekField label="ID">
          <span className="font-mono text-2xs" title={i.id}>
            {i.id.slice(0, 8)}
          </span>
        </PeekField>
      </dl>

      {i.notify_status === "failed" && i.notify_error && (
        <div className="rounded-control border border-sev-critical/40 bg-sev-critical/5 p-2 text-2xs text-sev-critical">
          <span className="font-semibold">Notify failed:</span> {i.notify_error}
        </div>
      )}

      <div className="flex flex-wrap gap-2 border-t border-ink-600 pt-3">
        <button
          className="btn"
          aria-label="Assign team or member"
          onClick={onAssign}
        >
          <UserPlus size={12} aria-hidden />{" "}
          {hasAssignment ? "Change assignment" : "Assign"}
        </button>
        <button
          className="btn"
          aria-label="Mark incident resolved"
          disabled={i.resolved}
          onClick={onResolve}
        >
          <CheckCircle2 size={12} aria-hidden /> Resolve
        </button>
      </div>
    </div>
  );
}

function IncidentRow({
  i,
  rowProps,
  teamById,
  memberById,
  rosterLoading,
  selected,
  onToggleSelect,
  onPeek,
  notifyExpanded,
  onToggleNotify,
}: {
  i: IncidentSummary;
  rowProps: Record<string, unknown>;
  teamById: Map<string, string>;
  memberById: Map<string, string>;
  rosterLoading: boolean;
  selected: boolean;
  onToggleSelect: () => void;
  onPeek: () => void;
  notifyExpanded: boolean;
  onToggleNotify: () => void;
}) {
  const status = i.resolved
    ? { label: "resolved", tone: "good" as const }
    : i.acked_at
      ? { label: "acked", tone: "accent" as const }
      : { label: "open", tone: "bad" as const };
  const teamName = i.assigned_team_id
    ? teamById.get(i.assigned_team_id)
    : undefined;
  const memberNames = (i.assigned_member_ids ?? []).map(
    (id) => memberById.get(id) ?? id.slice(0, 8),
  );
  const hasAssignment =
    !!i.assigned_team_id || (i.assigned_member_ids ?? []).length > 0;
  const notifyFailed = i.notify_status === "failed";

  return (
    <>
      <tr {...rowProps}>
        <td className="w-8">
          <RowSelectCheckbox
            checked={selected}
            onChange={onToggleSelect}
            label={`Select incident ${i.id.slice(0, 8)}`}
          />
        </td>
        <td>
          <div className="flex justify-end gap-1">
            <button
              className="btn p-2"
              aria-label={`View incident ${i.id.slice(0, 8)}`}
              title="View details"
              onClick={onPeek}
            >
              <Eye size={12} aria-hidden />
            </button>
          </div>
        </td>
        <td>
          {i.service && i.service !== "_unknown" ? (
            <span className="text-ink-200">{i.service}</span>
          ) : (
            <EmptyValue />
          )}
        </td>
        <td>
          {/* Severity stays empty until the backend ships a severity field on
              list summaries (UX_REDESIGN §3.5 ask #1) — IncidentSummary
              carries no content for the detail page's parser to read. */}
          <SeverityBadge severity={null} />
        </td>
        <td title={fmtAbs(i.created_at)}>{fmtRel(i.created_at)}</td>
        <td>
          {/* Origin is already split by the top tabs (AI-detected vs
              Webhook), so the per-row source chip is redundant here — the
              title stands alone. */}
          <div className="flex min-w-0 items-center gap-2">
            <Link
              to={`/incidents/${i.id}`}
              className="truncate font-medium text-ink-50 hover:text-link hover:underline"
            >
              {truncate(incidentTitle(i), 80)}
            </Link>
          </div>
        </td>
        <td>
          <div className="flex flex-wrap gap-1">
            {(i.channels_notified ?? []).map((c) => (
              <Pill key={c}>{c}</Pill>
            ))}
            {!i.channels_notified?.length && <EmptyValue />}
          </div>
        </td>
        <td>
          {hasAssignment ? (
            rosterLoading ? (
              <SkLine className="w-16" />
            ) : (
              <div className="flex flex-wrap gap-1">
                {teamName && <Pill tone="accent">{teamName}</Pill>}
                {memberNames.map((n, idx) => (
                  <Pill key={idx}>{n}</Pill>
                ))}
              </div>
            )
          ) : (
            <EmptyValue />
          )}
        </td>
        <td>
          {!i.notify_status ? (
            <EmptyValue />
          ) : i.notify_status === "sent" ? (
            <Pill tone="good">sent</Pill>
          ) : notifyFailed ? (
            <button
              aria-expanded={notifyExpanded}
              aria-controls={`notify-err-${i.id}`}
              aria-label={
                notifyExpanded
                  ? "Hide notification failure detail"
                  : "Show notification failure detail"
              }
              className="-m-1.5 rounded p-1.5"
              onClick={onToggleNotify}
            >
              <Pill tone="bad" className="gap-0.5">
                failed
                {notifyExpanded ? (
                  <ChevronUp size={10} aria-hidden />
                ) : (
                  <ChevronDown size={10} aria-hidden />
                )}
              </Pill>
            </button>
          ) : (
            <Pill tone="accent">{i.notify_status}</Pill>
          )}
        </td>
        <td>
          <Pill tone={status.tone}>{status.label}</Pill>
        </td>
        <td className="font-mono text-2xs text-ink-400" title={i.id}>
          {i.id.slice(0, 8)}
        </td>
      </tr>
      {notifyFailed && notifyExpanded && (
        <tr id={`notify-err-${i.id}`}>
          <td
            colSpan={COLS}
            className="bg-sev-critical/5 px-3 py-2 text-2xs text-sev-critical"
          >
            <span className="font-semibold">Notify failed:</span>{" "}
            {i.notify_error || "No error detail was recorded for this send."}
          </td>
        </tr>
      )}
    </>
  );
}
