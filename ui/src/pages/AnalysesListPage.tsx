import { useMemo, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { Eye, Loader2, Search, X } from "lucide-react";
import { api, type AnalysisRecord } from "@/lib/api";
import { fmtAbs, fmtRel, formatDuration, incidentTitle } from "@/lib/format";
import { useTableKeys } from "@/lib/hooks";
import { usePagination } from "@/lib/pagination";
import { useBulkSelection } from "@/lib/useBulkSelection";
import { SortHeader } from "@/components/SortHeader";
import { tsValue, useSortableRows } from "@/lib/sortRows";
import { TopBar } from "@/components/TopBar";
import { Pill } from "@/components/Pill";
import { SeverityBadge } from "@/components/SeverityBadge";
import { SegmentedControl } from "@/components/SegmentedControl";
import { Pagination } from "@/components/Pagination";
import { PeekPanel, PeekField } from "@/components/PeekPanel";
import {
  BulkActionBar,
  RowSelectCheckbox,
  SelectAllCheckbox,
} from "@/components/BulkActionBar";
import { SkRows } from "@/components/Skeleton";
import { RetryableError } from "@/components/RetryableError";
import { EmptyState } from "@/components/feedback";

const COLS = 9;

// AnalysesListPage lists every analysis recorded across all incidents,
// newest first. The per-row eye opens a peek slide-out (rows themselves do
// not navigate); the peek footer links to the analysis DETAIL page. The
// Incident column keeps a small secondary link to the parent incident. The
// Post-mortems tab is the explained future feature that used to be a dead
// sidebar item (empty-nav-state rule).
export function AnalysesListPage() {
  const [params, setParams] = useSearchParams();
  const tab = params.get("tab") ?? "analyses";
  const status = params.get("status") ?? "all";
  const incidentFilter = params.get("incident");
  const q = params.get("q") ?? "";

  const analysesQ = useInfiniteQuery({
    queryKey: ["analyses-all"],
    queryFn: ({ pageParam }) =>
      api.listAllAnalysesIndex({ offset: pageParam }),
    initialPageParam: 0,
    // next_offset is the resume cursor; null/undefined means no more rows.
    getNextPageParam: (last) => last.next_offset ?? undefined,
  });
  const {
    isLoading,
    isError,
    error,
    refetch,
    isRefetching,
    fetchNextPage,
    hasNextPage,
    isFetchingNextPage,
  } = analysesQ;

  // Accumulate the loaded pages into a single list; total comes from the first
  // page (the true whole-set count, never data.length). The server ships only
  // the most-recent page, so a large vs_analyses never loads whole up front.
  const data = useMemo<AnalysisRecord[] | undefined>(() => {
    const pages = analysesQ.data?.pages;
    if (!pages || pages.length === 0) return undefined;
    return pages.flatMap((p) => p.analyses);
  }, [analysesQ.data]);
  const total = analysesQ.data?.pages[0]?.total;

  const incidentsQ = useQuery({
    queryKey: ["incidents"],
    queryFn: () => api.listIncidents(),
  });

  const titleByID = useMemo(
    () => new Map((incidentsQ.data ?? []).map((inc) => [inc.id, inc.title])),
    [incidentsQ.data],
  );

  const setParam = (key: string, value: string | null) => {
    const next = new URLSearchParams(params);
    if (value) next.set(key, value);
    else next.delete(key);
    setParams(next, { replace: true });
  };

  const filtered = useMemo(() => {
    if (!data) return [];
    const needle = q.trim().toLowerCase();
    return data.filter((rec) => {
      if (incidentFilter && rec.incident_id !== incidentFilter) return false;
      if (status !== "all" && rec.status !== status) return false;
      if (!needle) return true;
      const title = titleByID.get(rec.incident_id) ?? "";
      return (
        title.toLowerCase().includes(needle) ||
        rec.incident_id.toLowerCase().includes(needle) ||
        (rec.finding?.Title ?? "").toLowerCase().includes(needle) ||
        (rec.finding?.Summary ?? "").toLowerCase().includes(needle) ||
        (rec.model ?? "").toLowerCase().includes(needle)
      );
    });
  }, [data, q, status, incidentFilter, titleByID]);

  // Click-to-sort on When (by the real requested_at timestamp, not the "2m
  // ago" string). Default: newest first, matching the incoming order.
  const sorted = useSortableRows(
    filtered,
    { when: (rec: AnalysisRecord) => tsValue(rec.requested_at) },
    { key: "when", dir: "desc" },
  );

  // Paginate at 100/page AFTER filter/search; reset to page 1 when any filter,
  // the search, or the sort changes so a filter never strands the operator on
  // an empty page.
  const pg = usePagination(sorted.rows, {
    resetKey: `${status}|${incidentFilter ?? ""}|${q}|${sorted.signature}`,
  });

  // Peek + selection. The analyses list is read-only, so the action bar
  // carries no actions — it collapses to the selection count + Clear, matching
  // the learned-signal tables — and the eye opens a peek without leaving the
  // list. Rows do not navigate; the peek footer is the way to the full page.
  const [peekId, setPeekId] = useState<string | null>(null);
  const pageKeys = useMemo(() => pg.pageItems.map((r) => r.id), [pg.pageItems]);
  const bulk = useBulkSelection(
    pageKeys,
    `${status}|${incidentFilter ?? ""}|${q}|${sorted.signature}|${pg.page}`,
  );
  const peek = peekId ? (data ?? []).find((r) => r.id === peekId) : undefined;

  const keys = useTableKeys({
    size: pg.pageItems.length,
    onOpen: (i) => {
      const rec = pg.pageItems[i];
      if (rec) setPeekId(rec.id);
    },
  });

  const hasFilters = Boolean(q || status !== "all" || incidentFilter);
  return (
    <>
      <TopBar
        title="Analyses"
        subtitle={total !== undefined ? `${total} stored` : undefined}
      />

      <main className="flex-1 overflow-auto p-6">
        <div className="mb-3">
          <SegmentedControl
            param="tab"
            defaultValue="analyses"
            aria-label="Analyses view"
            options={[
              { value: "analyses", label: "Analyses" },
              { value: "postmortems", label: "Post-mortems" },
            ]}
          />
        </div>

        {tab === "postmortems" ? (
          <div className="card">
            <EmptyState
              title="Post-mortems are coming"
              hint="They'll be generated from an incident's analyses, evidence and timeline."
            />
          </div>
        ) : (
          <>
            <div className="mb-3 flex flex-wrap items-center gap-2">
              <div className="relative max-w-md flex-1">
                <Search
                  size={12}
                  className="absolute left-2.5 top-1/2 -translate-y-1/2 text-ink-400"
                  aria-hidden
                />
                <input
                  data-page-search
                  className="input pl-7"
                  placeholder="Search by incident, finding or model…"
                  aria-label="Search analyses"
                  value={q}
                  onChange={(e) => setParam("q", e.target.value || null)}
                />
              </div>
              <SegmentedControl
                param="status"
                defaultValue="all"
                aria-label="Call status filter"
                options={[
                  { value: "all", label: "All" },
                  { value: "ok", label: "OK" },
                  { value: "error", label: "Error" },
                ]}
              />
              {incidentFilter && (
                <span className="inline-flex min-h-8 items-center gap-1.5 rounded-full border border-accent/40 bg-accent/10 px-2.5 py-1 text-2xs text-ink-100">
                  <span className="text-ink-300">incident:</span>
                  <span
                    className="max-w-48 truncate font-medium"
                    title={incidentFilter}
                  >
                    {titleByID.get(incidentFilter) ||
                      incidentTitle({ id: incidentFilter })}
                  </span>
                  <button
                    aria-label="Clear incident filter"
                    className="rounded p-0.5 text-ink-300 hover:text-ink-50"
                    onClick={() => setParam("incident", null)}
                  >
                    <X size={11} aria-hidden />
                  </button>
                </span>
              )}
            </div>

            {isError && (
              <div className="mb-3">
                <RetryableError
                  error={error}
                  onRetry={() => refetch()}
                  retrying={isRefetching}
                  context="Couldn't load analyses"
                />
              </div>
            )}
            {incidentsQ.isError && (
              <div className="mb-3">
                <RetryableError
                  error={incidentsQ.error}
                  onRetry={() => incidentsQ.refetch()}
                  retrying={incidentsQ.isRefetching}
                  context="Couldn't load incident titles — rows show raw ids meanwhile"
                />
              </div>
            )}

            <div className="card overflow-hidden">
              {bulk.count > 0 && (
                <BulkActionBar
                  count={bulk.count}
                  actions={[]}
                  onAction={() => {}}
                  onClear={bulk.clear}
                />
              )}
              <div
                className="max-h-[calc(100vh-220px)] overflow-auto"
                aria-label="Analyses table — j/k to move, Enter to open"
                {...keys.containerProps}
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
                      <th className="w-12 text-right">
                        <span className="sr-only">Action</span>
                      </th>
                      <SortHeader
                        className="w-32"
                        label="When"
                        {...sorted.headerProps("when")}
                      />
                      <th>Incident</th>
                      <th>Finding</th>
                      <th className="w-28">Severity</th>
                      <th className="w-32">Model</th>
                      <th className="w-20">Duration</th>
                      <th className="w-20">Status</th>
                    </tr>
                  </thead>
                  <tbody>
                    {isLoading && <SkRows rows={6} cols={COLS} />}
                    {!isLoading && !isError && filtered.length === 0 && (
                      <tr>
                        <td colSpan={COLS}>
                          <EmptyState
                            title="No analyses"
                            hint={
                              hasFilters
                                ? "Try clearing filters."
                                : "Run an analysis from an incident to see it here."
                            }
                            action={
                              hasFilters ? undefined : (
                                <Link to="/incidents" className="btn">
                                  Browse incidents
                                </Link>
                              )
                            }
                          />
                        </td>
                      </tr>
                    )}
                    {pg.pageItems.map((rec, i) => (
                      <tr key={rec.id} {...keys.rowProps(i)}>
                        <td className="w-8">
                          <RowSelectCheckbox
                            checked={bulk.isSelected(rec.id)}
                            onChange={() => bulk.toggle(rec.id)}
                            label={`Select analysis ${rec.id}`}
                          />
                        </td>
                        <td>
                          <div className="flex items-center justify-end gap-1">
                            <button
                              type="button"
                              className="btn p-1"
                              aria-label={`View analysis ${rec.id}`}
                              title="View details"
                              onClick={() => setPeekId(rec.id)}
                            >
                              <Eye size={14} aria-hidden />
                            </button>
                          </div>
                        </td>
                        <td
                          className="text-ink-300"
                          title={fmtAbs(rec.requested_at)}
                        >
                          {fmtRel(rec.requested_at)}
                        </td>
                        <td>
                          <Link
                            to={`/incidents/${rec.incident_id}`}
                            className="text-ink-200 hover:text-link hover:underline"
                            title={`Open incident ${rec.incident_id}`}
                          >
                            {titleByID.get(rec.incident_id) ||
                              incidentTitle({ id: rec.incident_id })}
                          </Link>
                        </td>
                        <td>
                          <div
                            className="max-w-md truncate font-medium text-ink-100"
                            title={rec.finding?.Title || rec.finding?.Summary}
                          >
                            {rec.finding?.Title || rec.finding?.Summary || "—"}
                          </div>
                        </td>
                        <td>
                          <SeverityBadge severity={rec.finding?.Severity} />
                        </td>
                        <td className="text-2xs text-ink-300">
                          {rec.model || "—"}
                        </td>
                        <td className="text-2xs text-ink-300">
                          {rec.duration_ms !== undefined
                            ? formatDuration(rec.duration_ms)
                            : "—"}
                        </td>
                        <td>
                          <Pill
                            tone={rec.status === "ok" ? "good" : "bad"}
                            title="AI call status"
                          >
                            {rec.status}
                          </Pill>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
              <Pagination state={pg} />
              {(isFetchingNextPage || hasNextPage) && (
                <div
                  className="flex items-center justify-center gap-1.5 border-t border-ink-600 px-3 py-2 text-2xs text-ink-400"
                  data-testid="analysis-load-more"
                >
                  {isFetchingNextPage ? (
                    <>
                      <Loader2 size={12} className="animate-spin" />
                      Loading more…
                    </>
                  ) : (
                    <button
                      type="button"
                      className="text-brand-300 hover:underline"
                      onClick={() => fetchNextPage()}
                    >
                      Load more ({total?.toLocaleString() ?? ""} total)
                    </button>
                  )}
                </div>
              )}
            </div>
          </>
        )}
      </main>

      {peek && (
        <PeekPanel
          open
          onClose={() => setPeekId(null)}
          title={peek.finding?.Title || `Analysis ${peek.id.slice(0, 8)}`}
          footer={
            <Link
              to={`/incidents/${peek.incident_id}/analyses/${peek.id}`}
              className="btn"
              onClick={() => setPeekId(null)}
            >
              Open full page ↗
            </Link>
          }
        >
          <AnalysisPeekBody
            rec={peek}
            incidentTitleText={
              titleByID.get(peek.incident_id) ||
              incidentTitle({ id: peek.incident_id })
            }
            onOpenIncident={() => setPeekId(null)}
          />
        </PeekPanel>
      )}
    </>
  );
}

// AnalysisPeekBody — the read-only detail slide-out for one analysis, matching
// the peek shape used across the incident / decision tables.
function AnalysisPeekBody({
  rec,
  incidentTitleText,
  onOpenIncident,
}: {
  rec: AnalysisRecord;
  incidentTitleText: string;
  onOpenIncident: () => void;
}) {
  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-2">
        <Pill tone={rec.status === "ok" ? "good" : "bad"}>{rec.status}</Pill>
        <SeverityBadge severity={rec.finding?.Severity} />
      </div>

      <dl className="grid grid-cols-2 gap-x-4 gap-y-2 text-xs">
        <PeekField label="When">
          <span title={fmtAbs(rec.requested_at)}>
            {fmtRel(rec.requested_at)}
          </span>
        </PeekField>
        <PeekField label="Model">{rec.model || "—"}</PeekField>
        <PeekField label="Duration">
          {rec.duration_ms !== undefined
            ? formatDuration(rec.duration_ms)
            : "—"}
        </PeekField>
        <PeekField label="Incident">
          <Link
            to={`/incidents/${rec.incident_id}`}
            className="text-link hover:underline"
            onClick={onOpenIncident}
          >
            {incidentTitleText}
          </Link>
        </PeekField>
      </dl>

      {rec.finding?.Summary && (
        <div>
          <div className="mb-1 text-2xs uppercase tracking-wide text-ink-400">
            Summary
          </div>
          <p className="text-xs leading-relaxed text-ink-100">
            {rec.finding.Summary}
          </p>
        </div>
      )}

      {rec.error && (
        <div className="rounded-control border border-sev-critical/40 bg-sev-critical/5 p-2 text-2xs text-sev-critical">
          <span className="font-semibold">Error:</span> {rec.error}
        </div>
      )}
    </div>
  );
}
