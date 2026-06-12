import { useEffect, useMemo, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import {
  useMutation,
  useQuery,
  useQueryClient,
  type QueryKey,
} from "@tanstack/react-query";
import {
  CheckCircle2,
  ChevronDown,
  ChevronUp,
  Search,
  UserPlus,
} from "lucide-react";
import { api, type IncidentSummary } from "@/lib/api";
import { fmtAbs, fmtRel, incidentTitle, truncate } from "@/lib/format";
import { useTableKeys } from "@/lib/hooks";
import { TopBar } from "@/components/TopBar";
import { Pill, SourceBadge } from "@/components/Pill";
import { EmptyState } from "@/components/feedback";
import { AssignDialog } from "@/components/AssignDialog";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { ClickableRow } from "@/components/DataTable";
import { SegmentedControl } from "@/components/SegmentedControl";
import { SeverityBadge } from "@/components/SeverityBadge";
import { SkLine, SkRows } from "@/components/Skeleton";
import { RetryableError } from "@/components/RetryableError";
import { useToast } from "@/components/Toast";

type StatusFilter = "all" | "open" | "acked" | "resolved";

const STATUS_VALUES: StatusFilter[] = ["open", "acked", "resolved", "all"];

// Column count for colSpan cells (skeleton / empty / notify-error rows):
// sev · when · service · title · channels · assigned · notify · status · id · actions
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

function matchesStatus(i: IncidentSummary, status: StatusFilter): boolean {
  if (status === "open") return !i.resolved && !i.acked_at;
  if (status === "acked") return !!i.acked_at && !i.resolved;
  if (status === "resolved") return i.resolved;
  return true;
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
    queryKey: useServerSearch ? ["incidents", "search", trimmed] : ["incidents"],
    queryFn: () =>
      useServerSearch ? api.searchIncidents(trimmed) : api.listIncidents(),
  });

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
  const textFiltered = useMemo(() => {
    if (!data) return [];
    // When the server already ran the text search, don't re-filter on text
    // (it matches fields the client can't see, e.g. payload body).
    const needle = useServerSearch
      ? ""
      : q.trim().replace(/^#/, "").toLowerCase();
    if (!needle) return data;
    return data.filter(
      (i) =>
        (i.title ?? "").toLowerCase().includes(needle) ||
        (i.service ?? "").toLowerCase().includes(needle) ||
        i.id.toLowerCase().includes(needle),
    );
  }, [data, q, useServerSearch]);

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

  // Resolve is optimistic (§2.4): cache flips immediately, rollback +
  // error toast with Retry on failure, invalidate on settle.
  const resolveMut = useMutation({
    mutationFn: (i: IncidentSummary) => api.resolveIncident(i.id),
    onMutate: async (i) => {
      setResolveFor(null);
      await qc.cancelQueries({ queryKey: ["incidents"] });
      const prev = qc.getQueriesData<IncidentSummary[]>({
        queryKey: ["incidents"],
      });
      qc.setQueriesData<IncidentSummary[]>({ queryKey: ["incidents"] }, (old) =>
        old?.map((x) =>
          x.id === i.id
            ? { ...x, resolved: true, resolved_at: new Date().toISOString() }
            : x,
        ),
      );
      return { prev };
    },
    onError: (err, i, ctx) => {
      ctx?.prev.forEach(([key, snap]: [QueryKey, IncidentSummary[] | undefined]) =>
        qc.setQueryData(key, snap),
      );
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
    },
  });

  // j/k + Enter navigation; a/r act on the active row (modals trap focus,
  // so these can't fire while a dialog is open).
  const { containerProps, rowProps } = useTableKeys({
    size: filtered.length,
    onOpen: (i) => {
      const row = filtered[i];
      if (row) navigate(`/incidents/${row.id}`);
    },
    extra: (key, i) => {
      const row = filtered[i];
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
        subtitle={
          data
            ? `${data.length} ${useServerSearch ? "found" : "stored"}`
            : undefined
        }
      />

      <main className="flex-1 overflow-auto p-4 lg:p-6">
        <div className="mb-3 flex flex-wrap items-center gap-2">
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
                    <th className="w-24">Sev</th>
                    <th className="w-32">When</th>
                    <th className="w-28">Service</th>
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
                    filtered.map((i, idx) => (
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
          {/* Severity stays "—" until the backend ships a severity field on
              list summaries (UX_REDESIGN §3.5 ask #1) — IncidentSummary
              carries no content for the detail page's parser to read. */}
          <SeverityBadge severity={null} />
        </td>
        <td title={fmtAbs(i.created_at)}>{fmtRel(i.created_at)}</td>
        <td className="text-ink-200">{i.service || "—"}</td>
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
            {!i.channels_notified?.length && (
              <span className="text-ink-400">—</span>
            )}
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
            <span className="text-ink-400">—</span>
          )}
        </td>
        <td>
          {!i.notify_status ? (
            <span className="text-ink-400">—</span>
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
