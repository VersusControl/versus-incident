import { useEffect, useMemo, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import {
  useMutation,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import {
  CheckCircle2,
  ChevronDown,
  ChevronUp,
  Search,
  UserPlus,
} from "lucide-react";
import { api, type IncidentIndex, type IncidentSummary } from "@/lib/api";
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
import { TopBar } from "@/components/TopBar";
import { Pill, SourceBadge } from "@/components/Pill";
import { EmptyState, EmptyValue } from "@/components/feedback";
import { AssignDialog } from "@/components/AssignDialog";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { ClickableRow } from "@/components/DataTable";
import { Pagination } from "@/components/Pagination";
import { SegmentedControl } from "@/components/SegmentedControl";
import { SeverityBadge } from "@/components/SeverityBadge";
import { SkLine, SkRows } from "@/components/Skeleton";
import { RetryableError } from "@/components/RetryableError";
import { ReportsButton } from "@/components/ReportsDialog";
import { useToast } from "@/components/toastContext";

type StatusFilter = IncidentStatusFilter;

const STATUS_VALUES = INCIDENT_STATUS_VALUES;

// Column count for colSpan cells (skeleton / empty / notify-error rows):
// service · sev · when · title · channels · assigned · notify · status · id · actions
const COLS = 10;

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

// IncidentsPage shows the persisted incident history pulled from the
// storage backend. Newest first, with a URL-synced status filter
// (?status=, default open — the URL is the state, so tiles and banners can
// deep-link) and a free-text search. When the backend supports server-side
// search (Postgres), the query runs on the server; otherwise it falls back
// to filtering the already-loaded page client-side.
export function IncidentsPage() {
  const navigate = useNavigate();
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

  // Paginate at 100/page AFTER filter/search. Reset to page 1 whenever the
  // origin tab, status filter, or search text changes so a filter never
  // strands the operator on a now-empty page.
  const pg = usePagination(filtered, {
    resetKey: incidentResetKey(origin, status, trimmed),
  });

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

  // j/k + Enter navigation; a/r act on the active row (modals trap focus,
  // so these can't fire while a dialog is open). Scoped to the rendered page
  // so navigation stays bounded on a multi-thousand-row history.
  const { containerProps, rowProps } = useTableKeys({
    size: pg.pageItems.length,
    onOpen: (i) => {
      const row = pg.pageItems[i];
      if (row) navigate(`/incidents/${row.id}`);
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
          <ReportsButton className="ml-auto" />
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
            <div
              {...containerProps}
              role="region"
              aria-label="Incidents table — j/k to move, Enter to open"
              className="max-h-[calc(100vh-240px)] overflow-auto focus:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
            >
              <table className="ddt">
                <thead>
                  <tr>
                    <th className="w-28">Service</th>
                    <th className="w-24">Severity</th>
                    <th className="w-32">When</th>
                    <th>Title</th>
                    <th className="w-32">Channels</th>
                    <th className="w-32">Assigned</th>
                    <th className="w-24">Notify</th>
                    <th className="w-24">Status</th>
                    <th className="w-28">ID</th>
                    <th className="w-24" />
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
                        notifyExpanded={expandedNotify.has(i.id)}
                        onToggleNotify={() => toggleNotify(i.id)}
                        onAssign={() => setAssignFor(i)}
                        onResolve={() => setResolveFor(i)}
                        resolvePending={
                          resolveMut.isPending &&
                          resolveMut.variables?.id === i.id
                        }
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
    </>
  );
}

function IncidentRow({
  i,
  rowProps,
  teamById,
  memberById,
  rosterLoading,
  notifyExpanded,
  onToggleNotify,
  onAssign,
  onResolve,
  resolvePending,
}: {
  i: IncidentSummary;
  rowProps: Record<string, unknown>;
  teamById: Map<string, string>;
  memberById: Map<string, string>;
  rosterLoading: boolean;
  notifyExpanded: boolean;
  onToggleNotify: () => void;
  onAssign: () => void;
  onResolve: () => void;
  resolvePending: boolean;
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
      <ClickableRow to={`/incidents/${i.id}`} {...rowProps}>
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
          {/* Single-line flex — inline content made the source chip wrap
              under long titles but sit beside short ones, so the column
              read as two different layouts. nowrap keeps title + chip on
              one line in one consistent order. */}
          <div className="flex min-w-0 items-center gap-2">
            <Link
              to={`/incidents/${i.id}`}
              className="truncate font-medium text-ink-50 hover:text-link hover:underline"
            >
              {truncate(incidentTitle(i), 80)}
            </Link>
            <span className="shrink-0">
              <SourceBadge source={i.source} />
            </span>
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
        <td>
          <div className="flex justify-end gap-1">
            <button
              className="btn p-2"
              aria-label="Assign team or member"
              title={
                hasAssignment ? "Change assignment" : "Assign team or member"
              }
              onClick={onAssign}
            >
              <UserPlus size={12} />
            </button>
            <button
              className="btn p-2"
              aria-label="Mark incident resolved"
              title={
                i.resolved
                  ? "Already resolved"
                  : "Mark this incident as resolved"
              }
              disabled={i.resolved || resolvePending}
              onClick={onResolve}
            >
              <CheckCircle2 size={12} />
            </button>
          </div>
        </td>
      </ClickableRow>
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
