import { useMemo, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Check, Eye, Search, Trash2 } from "lucide-react";
import { api, type Pattern } from "@/lib/api";
import { displayService, fmtAbs, fmtRel } from "@/lib/format";
import { useTableKeys } from "@/lib/hooks";
import { TopBar } from "@/components/TopBar";
import { Pill, VerdictPill } from "@/components/Pill";
import { InfoHint } from "@/components/InfoHint";
import { ReadinessProgress } from "@/components/ReadinessProgress";
import { AutoRefreshControl } from "@/components/AutoRefreshControl";
import { useAutoRefresh } from "@/lib/useAutoRefresh";
import { EmptyState } from "@/components/feedback";
import { SegmentedControl } from "@/components/SegmentedControl";
import { PeekPanel } from "@/components/PeekPanel";
import { SkRows } from "@/components/Skeleton";
import { RetryableError } from "@/components/RetryableError";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { ReassignModal, ServiceCell } from "@/components/ServiceCell";
import {
  BulkActionBar,
  RowSelectCheckbox,
  SelectAllCheckbox,
} from "@/components/BulkActionBar";
import {
  useIntelLicensed,
  useLearnExclusions,
} from "@/lib/useLearnExclusions";
import {
  countExcluded,
  filterByScope,
  isExclusionScope,
  SCOPE_PARAM,
} from "@/lib/rowActions";
import { buildLogsBulkActions } from "@/lib/bulkSelect";
import { useBulkSelection } from "@/lib/useBulkSelection";
import { Pagination } from "@/components/Pagination";
import { usePagination } from "@/lib/pagination";
import { useToast } from "@/components/toastContext";

// Verdict filter is URL-synced via SegmentedControl. "uncurated" is a real
// sentinel value mapping to verdict === "" — never an <option value="">
// duplicate (audit S1: the old select had two empty options fighting).
const VERDICT_PARAM = "verdict";

function matchesVerdict(p: Pattern, v: string): boolean {
  switch (v) {
    case "uncurated":
      return p.verdict === "";
    case "known":
      return p.verdict === "known";
    case "spike":
      return p.verdict === "spike";
    default:
      return true; // "all"
  }
}

type VerdictVars = { id: string; verdict: string };

export function PatternsPage() {
  const qc = useQueryClient();
  const toast = useToast();
  const [params] = useSearchParams();
  const verdictFilter = params.get(VERDICT_PARAM) ?? "all";
  const scope = isExclusionScope(params.get(SCOPE_PARAM));

  const refresh = useAutoRefresh();
  const { data, isLoading, isError, error, refetch, isRefetching } = useQuery({
    queryKey: ["patterns"],
    queryFn: api.listPatterns,
    refetchInterval: refresh.refetchInterval,
  });

  const [q, setQ] = useState("");
  const [peekId, setPeekId] = useState<string | null>(null);
  // Bulk reassign: the selected pattern ids captured when "Assign to service"
  // is picked in the action bar (null = modal closed). Reassign is one flow for
  // one row or many — a single-row correction is just a one-row selection.
  const [reassignMatches, setReassignMatches] = useState<string[] | null>(null);
  const [confirmReset, setConfirmReset] = useState(false);

  // Disable-Learn (X30 / E1) control. Log learning is OSS, so listPatterns
  // succeeds regardless of license — an explicit enterprise probe decides
  // whether the surface is licensed. Exclusion here is at the per-PATTERN grain
  // (E1): ignoring one log pattern holds out ONLY that pattern, keyed on its
  // stable id — the log analogue of the metric/trace per-signal exclusion
  // (whole-service exclusion still lives on the service detail page). The action
  // lives in the checkbox action bar (Ignore/Resume) and renders only for a
  // licensed admin (runtime:manage); it is absent on community / OSS and hidden
  // from a viewer (excl.visible).
  const licensed = useIntelLicensed();
  const excl = useLearnExclusions(licensed);

  // ----- inline verdict mutation: optimistic with rollback + Undo ---------
  const verdictMutation = useMutation<
    Pattern,
    Error,
    VerdictVars,
    { prev?: Pattern[] }
  >({
    mutationFn: ({ id, verdict }) => api.updatePattern(id, { verdict }),
    onMutate: async ({ id, verdict }) => {
      await qc.cancelQueries({ queryKey: ["patterns"] });
      const prev = qc.getQueryData<Pattern[]>(["patterns"]);
      qc.setQueryData<Pattern[]>(["patterns"], (old) =>
        (old ?? []).map((p) => (p.id === id ? { ...p, verdict } : p)),
      );
      return { prev };
    },
    onError: (err, vars, ctx) => {
      if (ctx?.prev) qc.setQueryData(["patterns"], ctx.prev);
      toast.push({
        tone: "error",
        title: "Verdict update failed",
        description: err.message,
        action: { label: "Retry", onClick: () => verdictMutation.mutate(vars) },
      });
    },
    onSuccess: (_data, vars, ctx) => {
      const prevVerdict =
        ctx?.prev?.find((p) => p.id === vars.id)?.verdict ?? "";
      toast.push({
        tone: "ok",
        title:
          vars.verdict === "known"
            ? "Marked known"
            : vars.verdict === "spike"
              ? "Marked spike"
              : "Verdict cleared",
        action: {
          label: "Undo",
          onClick: () =>
            verdictMutation.mutate({ id: vars.id, verdict: prevVerdict }),
        },
      });
    },
    onSettled: () => qc.invalidateQueries({ queryKey: ["patterns"] }),
  });

  // ----- clear all logs: destructive reset of every learned log pattern -----
  const reset = useMutation({
    mutationFn: api.clearPatterns,
    onSuccess: (res) => {
      qc.invalidateQueries({ queryKey: ["patterns"] });
      setConfirmReset(false);
      toast.push({
        tone: "ok",
        title: "Learned log patterns cleared",
        description: `${res.patterns} patterns removed — the agent relearns log patterns from scratch`,
      });
    },
    onError: (err) => {
      toast.push({
        tone: "error",
        title: "Clear all logs failed",
        description: err.message,
        action: { label: "Retry", onClick: () => reset.mutate() },
      });
    },
  });

  // ----- filtering ---------------------------------------------------------
  const filtered = useMemo(() => {
    if (!data) return [];
    const needle = q.trim().toLowerCase();
    return data.filter((p) => {
      if (!matchesVerdict(p, verdictFilter)) return false;
      if (!needle) return true;
      return (
        p.template.toLowerCase().includes(needle) ||
        (p.service ?? "").toLowerCase().includes(needle) ||
        p.id.toLowerCase().includes(needle) ||
        (p.rule_name ?? "").toLowerCase().includes(needle)
      );
    });
  }, [data, q, verdictFilter]);

  const counts = useMemo(() => {
    if (!data) return null;
    return {
      uncurated: data.filter((p) => p.verdict === "").length,
      known: data.filter((p) => p.verdict === "known").length,
      spike: data.filter((p) => p.verdict === "spike").length,
      all: data.length,
    };
  }, [data]);

  // ----- Active | Ignored scope (D3) --------------------------------------
  // A log row is "ignored" when its PATTERN is held out of learning (E1, keyed
  // on the pattern's stable id). The scope control is gated on excl.visible —
  // absent for community / viewers, so scope stays "active" and nothing is
  // partitioned out. The server stays authority: this only re-partitions what
  // the loaded policy already reports.
  const isRowExcluded = (p: Pattern) => excl.isPatternExcluded(p.id);
  const scopeCounts = useMemo(
    () => ({
      active: filtered.length - countExcluded(filtered, isRowExcluded),
      ignored: countExcluded(filtered, isRowExcluded),
    }),
    // isRowExcluded closes over excl; recompute when the policy or list changes.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [filtered, excl],
  );
  const scoped = useMemo(
    () =>
      excl.visible ? filterByScope(filtered, scope, isRowExcluded) : filtered,
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [filtered, scope, excl],
  );

  // ----- pagination (100/page) — resets to page 1 when the verdict filter,
  // scope tab, or search changes so a filter never strands the operator on an
  // empty page.
  const pg = usePagination(scoped, {
    resetKey: `${verdictFilter}|${scope}|${q}`,
  });

  // ----- selection + action bar -------------------------------------------
  // The checkbox selection is the ONE row-action model: a select-all checkbox
  // in the header, a checkbox per row, and an action bar that APPEARS when rows
  // are selected (no per-row ⋯ menu). Logs actions = relabel (Mark known / Clear
  // verdict, ungated) + Ignore/Resume (gated on the exclude surface) +
  // Assign-to-service (always — log attribution override is OSS). Selection
  // resets on verdict / scope / search / PAGE change.
  const bulkActions = buildLogsBulkActions({ scope, excludeVisible: excl.visible });
  const bulkEnabled = bulkActions.length > 0;
  const pageKeys = useMemo(() => pg.pageItems.map((p) => p.id), [pg.pageItems]);
  const bulk = useBulkSelection(
    pageKeys,
    `${verdictFilter}|${scope}|${q}|${pg.page}`,
  );

  const onBulkAction = (spec: { id: string }) => {
    const ids = bulk.selectedKeys;
    switch (spec.id) {
      case "reassign":
        // Open the picker for the selection; keep the selection until the modal
        // finishes (onDone clears it). Every other action fires immediately.
        setReassignMatches([...ids]);
        return;
      case "mark-known":
        ids.forEach((id) => verdictMutation.mutate({ id, verdict: "known" }));
        break;
      case "clear-verdict":
        ids.forEach((id) => verdictMutation.mutate({ id, verdict: "" }));
        break;
      case "ignore":
        excl.togglePatterns(ids, true);
        break;
      case "resume":
        excl.togglePatterns(ids, false);
        break;
    }
    bulk.clear();
  };

  // Columns: Service, Template, Count, Normal, Verdict, Action (+ checkbox).
  const cols = 6 + (bulkEnabled ? 1 : 0);

  // ----- keyboard: j/k rows · Enter view · K known -------------------------
  const keys = useTableKeys({
    size: pg.pageItems.length,
    onOpen: (i) => {
      const row = pg.pageItems[i];
      if (row) setPeekId(row.id);
    },
    extra: (key, index) => {
      const row = pg.pageItems[index];
      if (!row) return false;
      if (key === "K") {
        verdictMutation.mutate({ id: row.id, verdict: "known" });
        return true;
      }
      return false;
    },
  });

  // The PeekPanel owns Escape while open (document capture + stopPropagation).
  const onTableKeyDown = (e: React.KeyboardEvent) => {
    keys.containerProps.onKeyDown(e);
  };

  const peek = peekId ? (data ?? []).find((p) => p.id === peekId) : undefined;

  return (
    <>
      <TopBar
        title="What the agent knows right now"
        subtitle={data ? `${data.length} log templates learned` : "The agent learns recurring message templates from your logs so it can spot new or unusual ones."}
        actions={
          <button
            className="btn btn-danger"
            disabled={reset.isPending || !data?.length}
            onClick={() => setConfirmReset(true)}
          >
            <Trash2 size={12} />
            Clear all logs
          </button>
        }
      />

      <main className="flex-1 overflow-auto p-4 lg:p-6">
        <p className="mb-4 max-w-4xl text-xs text-ink-400">
          The recurring log messages the agent has learned for each service —
          and how often each normally shows up.
        </p>
        <div className="mb-3 flex flex-wrap items-center gap-2">
          <SegmentedControl
            param={VERDICT_PARAM}
            defaultValue="all"
            aria-label="Verdict filter"
            options={[
              { value: "all", label: "All", badge: counts?.all },
              {
                value: "uncurated",
                label: "Still learning",
                badge: counts?.uncurated,
              },
              { value: "known", label: "Known", badge: counts?.known },
            ]}
          />
          {excl.visible && (
            <SegmentedControl
              param={SCOPE_PARAM}
              defaultValue="active"
              aria-label="Learning scope"
              options={[
                { value: "active", label: "Active", badge: scopeCounts.active },
                {
                  value: "ignored",
                  label: "Ignored",
                  badge: scopeCounts.ignored,
                },
              ]}
            />
          )}
          <div className="relative w-full max-w-md sm:w-auto sm:flex-1">
            <Search
              size={12}
              className="absolute left-2.5 top-1/2 -translate-y-1/2 text-ink-400"
            />
            <input
              data-page-search
              className="input pl-7"
              placeholder="Search template, service, id, or rule…  ( / )"
              value={q}
              onChange={(e) => setQ(e.target.value)}
            />
          </div>

          <AutoRefreshControl state={refresh} />
        </div>

        {isError ? (
          <RetryableError
            error={error}
            onRetry={() => refetch()}
            retrying={isRefetching}
            context="Couldn't load the pattern catalog"
          />
        ) : (
          <div className="card overflow-hidden">
            {bulkEnabled && bulk.count > 0 && (
              <BulkActionBar
                count={bulk.count}
                actions={bulkActions}
                onAction={onBulkAction}
                onClear={bulk.clear}
                busy={excl.busy || verdictMutation.isPending}
              />
            )}
            <div
              tabIndex={0}
              onKeyDown={onTableKeyDown}
              className="max-h-[calc(100vh-230px)] overflow-auto"
            >
              <table className="ddt">
                <thead>
                  <tr>
                    {bulkEnabled && (
                      <th className="w-8">
                        <SelectAllCheckbox
                          state={bulk.headerState}
                          onChange={bulk.toggleAll}
                        />
                      </th>
                    )}
                    <th className="w-36">Service</th>
                    <th className="whitespace-nowrap">
                      Template
                      <InfoHint
                        label="About the Template"
                        text="A recurring log message the agent has learned. The parts that change from line to line — numbers, IDs, timestamps — are blanked out, so many similar lines group into one template instead of thousands of separate messages."
                        example="The lines 'user 8471 login failed' and 'user 22 login failed' both become the template 'user <*> login failed'."
                      />
                    </th>
                    <th className="w-20 whitespace-nowrap text-right">
                      Count
                      <InfoHint
                        label="About the Count"
                        text="The total number of times the agent has seen this exact message shape since it started learning — a running lifetime total, not a per-check number."
                        example="12,480 means this template has matched 12,480 log lines so far."
                      />
                    </th>
                    <th className="w-24 whitespace-nowrap text-right">
                      Normal
                      <InfoHint
                        label="About the Normal"
                        text="How often this message normally appears each time the agent checks (it polls every ~30s). The agent learns this baseline from history, so 'normal' is a small range, not an exact number. A 'big jump' means far more sightings than usual in one check — that's what gets flagged as a possible problem."
                        example="'payment failed' normally appears ~2 times per check; if it suddenly appears 25 times, the agent flags it as a spike."
                      />
                    </th>
                    <th className="w-24 whitespace-nowrap">
                      Verdict
                      <InfoHint
                        label="About the Verdict"
                        text="The agent's current label for this template. 'Still learning' = not reviewed yet, the agent is still working out what's normal — the count next to it (e.g. 40 / 100) shows how close it is to being treated as known automatically. 'Known' = an operator marked it as normal, so it won't raise an alert. 'Spike' = it recently showed up far more often than its usual range."
                        example="A login-error template shows 'Still learning 40 / 100' — 40 of the ~100 sightings it needs; once you're sure it's harmless you mark it 'Known' and it stops alerting, but if it later floods in it flips to 'Spike'."
                      />
                    </th>
                    <th className="w-12 text-right">
                      <span className="sr-only">Action</span>
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {isLoading && <SkRows rows={8} cols={cols} />}
                  {!isLoading && scoped.length === 0 && (
                    <tr>
                      <td colSpan={cols}>
                        {data && data.length === 0 ? (
                          <EmptyState
                            title="No patterns learned yet"
                            hint="The agent builds the catalog as logs flow in."
                            action={
                              <Link className="btn" to="/agent/metrics">
                                Learning metrics or traces? See Metrics
                              </Link>
                            }
                          />
                        ) : scope === "ignored" ? (
                          <EmptyState
                            title="No patterns ignored"
                            hint="Select a noisy pattern and choose Ignore from the action bar and it moves here, held out of learning until you resume it."
                          />
                        ) : verdictFilter === "uncurated" && !q.trim() ? (
                          <EmptyState title="No patterns still learning — the catalog is fully labeled." />
                        ) : (
                          <EmptyState
                            title="No patterns match your filters"
                            hint="Try clearing the search or switching the verdict filter."
                          />
                        )}
                      </td>
                    </tr>
                  )}
                  {pg.pageItems.map((p, i) => (
                    <tr key={p.id} {...keys.rowProps(i)}>
                      {bulkEnabled && (
                        <td className="w-8">
                          <RowSelectCheckbox
                            checked={bulk.isSelected(p.id)}
                            onChange={() => bulk.toggle(p.id)}
                            label={`Select pattern ${p.id}`}
                          />
                        </td>
                      )}
                      <td className="font-mono text-2xs text-ink-300">
                        <ServiceCell
                          service={p.service}
                          sourceType="log"
                          match={p.id}
                        />
                      </td>
                      <td className="max-w-0">
                        <div
                          className="truncate font-mono text-2xs text-ink-200"
                          title={p.template}
                        >
                          {p.template}
                        </div>
                      </td>
                      <td className="text-right tabular-nums text-ink-100">
                        {p.count}
                      </td>
                      <td className="text-right tabular-nums text-ink-300">
                        ≈ {p.baseline_frequency.toFixed(1)}
                      </td>
                      <td>
                        <div className="flex items-center gap-2">
                          <VerdictPill verdict={p.verdict} />
                          {p.verdict === "" &&
                            p.readiness &&
                            !p.readiness.ready &&
                            p.readiness.needed > 0 && (
                              <span
                                className="inline-flex items-center gap-1.5"
                                title={`Seen ${p.readiness.seen} of ${p.readiness.needed} sightings needed before the agent treats this pattern as known`}
                              >
                                <span
                                  className="h-1 w-12 overflow-hidden rounded-full bg-ink-700"
                                  role="progressbar"
                                  aria-valuenow={p.readiness.seen}
                                  aria-valuemin={0}
                                  aria-valuemax={p.readiness.needed}
                                >
                                  <span
                                    className="block h-full rounded-full bg-accent transition-[width]"
                                    style={{
                                      width: `${Math.min(100, Math.round((p.readiness.seen / p.readiness.needed) * 100))}%`,
                                    }}
                                  />
                                </span>
                                <span className="text-2xs tabular-nums text-ink-400">
                                  {p.readiness.seen}
                                  <span className="text-ink-600">/</span>
                                  {p.readiness.needed}
                                </span>
                              </span>
                            )}
                        </div>
                      </td>
                      <td>
                        <div className="flex items-center justify-end gap-1">
                          <button
                            type="button"
                            className="btn p-1"
                            aria-label={`View pattern ${p.id}`}
                            title="View details"
                            onClick={() => setPeekId(p.id)}
                          >
                            <Eye size={14} aria-hidden />
                          </button>
                        </div>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            <Pagination state={pg} />
          </div>
        )}
      </main>

      {/* Peek panel — inspect without losing list position. No sparkline:
          the API exposes no bucketed counts (UX_REDESIGN §3.5 ask #6), so the
          current count vs the learned-normal count tells the story. */}
      {peek && (
        <PeekPanel
          open
          onClose={() => setPeekId(null)}
          title={<span className="font-mono">{peek.id}</span>}
          footer={
            <Link
              to={`/agent/logs/${peek.id}`}
              className="btn"
              onClick={() => setPeekId(null)}
            >
              Open full page ↗
            </Link>
          }
        >
          <div className="space-y-4">
            <div className="flex items-center gap-2">
              <VerdictPill verdict={peek.verdict} />
              <span className="text-2xs text-ink-400">{peek.rule_name || "no rule"}</span>
            </div>

            <pre className="overflow-auto rounded-md border border-ink-600 bg-surface-sunken p-3 font-mono text-2xs leading-relaxed text-ink-100">
              {peek.template}
            </pre>

            <dl className="grid grid-cols-2 gap-x-4 gap-y-2 text-xs">
              <PeekFact label="Count">
                <span className="tabular-nums">{peek.count}</span>
              </PeekFact>
              <PeekFact label="Normal">
                <span className="tabular-nums">
                  ≈ {peek.baseline_frequency.toFixed(1)}
                </span>
              </PeekFact>
              <PeekFact label="To known">
                <ReadinessProgress readiness={peek.readiness} />
              </PeekFact>
              <PeekFact label="First seen">
                <span title={fmtAbs(peek.first_seen)}>
                  {fmtRel(peek.first_seen)}
                </span>
              </PeekFact>
              <PeekFact label="Last seen">
                <span title={fmtAbs(peek.last_seen)}>
                  {fmtRel(peek.last_seen)}
                </span>
              </PeekFact>
              <PeekFact label="Service">{displayService(peek.service)}</PeekFact>
              <PeekFact label="Rule">{peek.rule_name || "—"}</PeekFact>
              <PeekFact label="Source">{peek.source || "—"}</PeekFact>
              <PeekFact label="Tags">
                {peek.tags && peek.tags.length > 0 ? (
                  <span className="flex flex-wrap gap-1">
                    {peek.tags.map((t) => (
                      <Pill key={t} tone="accent">
                        {t}
                      </Pill>
                    ))}
                  </span>
                ) : (
                  "—"
                )}
              </PeekFact>
            </dl>

            <div className="flex flex-wrap gap-2 border-t border-ink-600 pt-3">
              <button
                aria-label={`Mark pattern ${peek.id} as known`}
                className="btn"
                disabled={peek.verdict === "known"}
                onClick={() =>
                  verdictMutation.mutate({ id: peek.id, verdict: "known" })
                }
              >
                <Check size={12} /> Mark known
              </button>
              <button
                aria-label={`Clear verdict for pattern ${peek.id}`}
                className="btn"
                disabled={peek.verdict === ""}
                onClick={() =>
                  verdictMutation.mutate({ id: peek.id, verdict: "" })
                }
              >
                Clear verdict
              </button>
            </div>
          </div>
        </PeekPanel>
      )}

      {confirmReset && (
        <ConfirmDialog
          title="Clear all learned log patterns"
          message="This removes ALL learned log patterns and resets the miner, so the agent relearns log patterns from scratch on the next tick. Discovered services are left untouched. This cannot be undone."
          confirmLabel="Clear all logs"
          tone="danger"
          busy={reset.isPending}
          error={reset.error}
          onConfirm={() => reset.mutate()}
          onClose={() => setConfirmReset(false)}
        />
      )}

      {/* Reassign picker — opened from the "Assign to service" action in the
          checkbox action bar; reassigns the selected pattern(s). */}
      {reassignMatches && (
        <ReassignModal
          sourceType="log"
          matches={reassignMatches}
          invalidateKeys={[["patterns"]]}
          onClose={() => setReassignMatches(null)}
          onDone={() => bulk.clear()}
        />
      )}
    </>
  );
}

function PeekFact({
  label,
  children,
}: {
  label: string;
  children: React.ReactNode;
}) {
  return (
    <div>
      <dt className="text-2xs uppercase tracking-wider text-ink-400">
        {label}
      </dt>
      <dd className="mt-0.5 font-mono text-xs text-ink-100">{children}</dd>
    </div>
  );
}
