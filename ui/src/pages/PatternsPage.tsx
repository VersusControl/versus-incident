import { useMemo, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Check, Save, Search, Zap } from "lucide-react";
import { api, type Pattern } from "@/lib/api";
import { displayService, fmtAbs, fmtRel } from "@/lib/format";
import { useTableKeys } from "@/lib/hooks";
import { TopBar } from "@/components/TopBar";
import { Pill, VerdictPill } from "@/components/Pill";
import { EmptyState } from "@/components/feedback";
import { SegmentedControl } from "@/components/SegmentedControl";
import { ClickableRow } from "@/components/DataTable";
import { PeekPanel } from "@/components/PeekPanel";
import { SkRows } from "@/components/Skeleton";
import { RetryableError } from "@/components/RetryableError";
import { ConfirmDialog } from "@/components/ConfirmDialog";
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
  const verdictFilter = params.get(VERDICT_PARAM) ?? "uncurated";

  const { data, isLoading, isError, error, refetch, isRefetching } = useQuery({
    queryKey: ["patterns"],
    queryFn: api.listPatterns,
  });

  const [q, setQ] = useState("");
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [peekId, setPeekId] = useState<string | null>(null);
  const [confirmFlush, setConfirmFlush] = useState(false);
  const [bulkBusy, setBulkBusy] = useState(false);

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

  // ----- bulk labeling: sequential PATCHes with progress + retry ----------
  async function runBulk(ids: string[], verdict: string) {
    setBulkBusy(true);
    toast.push({ tone: "info", title: `Labeling ${ids.length} patterns…` });
    const failed: string[] = [];
    let lastError = "";
    for (const id of ids) {
      try {
        await api.updatePattern(id, { verdict });
      } catch (e) {
        failed.push(id);
        lastError = e instanceof Error ? e.message : String(e);
      }
    }
    await qc.invalidateQueries({ queryKey: ["patterns"] });
    setBulkBusy(false);
    if (failed.length === 0) {
      toast.push({ tone: "ok", title: `${ids.length}/${ids.length} labeled` });
      setSelected(new Set());
    } else {
      toast.push({
        tone: "error",
        title: `${ids.length - failed.length}/${ids.length} labeled`,
        description: `${failed.length} failed${lastError ? ` — ${lastError}` : ""}`,
        action: {
          label: "Retry failed",
          onClick: () => void runBulk(failed, verdict),
        },
      });
      setSelected(new Set(failed));
    }
  }

  // ----- flush: ConfirmDialog kept + success/error toasts (audit S3) ------
  const flush = useMutation({
    mutationFn: api.flushPatterns,
    onSuccess: (res) => {
      qc.invalidateQueries({ queryKey: ["patterns"] });
      setConfirmFlush(false);
      toast.push({
        tone: "ok",
        title: "Catalog flushed",
        description: `${res.patterns} patterns written to disk`,
      });
    },
    onError: (err) => {
      toast.push({
        tone: "error",
        title: "Flush failed",
        description: err.message,
        action: { label: "Retry", onClick: () => flush.mutate() },
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

  // ----- selection ---------------------------------------------------------
  const toggleOne = (id: string) =>
    setSelected((cur) => {
      const next = new Set(cur);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });

  const visibleIds = filtered.map((p) => p.id);
  const allSelected =
    visibleIds.length > 0 && visibleIds.every((id) => selected.has(id));
  const someSelected = visibleIds.some((id) => selected.has(id));
  const toggleAll = () =>
    setSelected((cur) => {
      const next = new Set(cur);
      if (allSelected) visibleIds.forEach((id) => next.delete(id));
      else visibleIds.forEach((id) => next.add(id));
      return next;
    });

  // ----- keyboard: j/k rows · Enter peek · x select · K known · S spike ----
  const keys = useTableKeys({
    size: filtered.length,
    onOpen: (i) => {
      const row = filtered[i];
      if (row) setPeekId(row.id);
    },
    extra: (key, index) => {
      const row = filtered[index];
      if (!row) return false;
      if (key === "x") {
        toggleOne(row.id);
        return true;
      }
      if (key === "K") {
        verdictMutation.mutate({ id: row.id, verdict: "known" });
        return true;
      }
      if (key === "S") {
        verdictMutation.mutate({ id: row.id, verdict: "spike" });
        return true;
      }
      return false;
    },
  });

  // Esc clears the bulk selection too (the hook only resets the active row).
  // The PeekPanel owns Escape while open (document capture + stopPropagation).
  const onTableKeyDown = (e: React.KeyboardEvent) => {
    if (
      e.key === "Escape" &&
      !(e.target as HTMLElement).hasAttribute("data-page-search") &&
      selected.size > 0
    ) {
      setSelected(new Set());
    }
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
            className="btn"
            disabled={flush.isPending}
            onClick={() => setConfirmFlush(true)}
          >
            <Save size={12} />
            Flush to disk
          </button>
        }
      />

      <main className="flex-1 overflow-auto p-4 lg:p-6">
        <p className="mb-3 max-w-3xl text-xs text-ink-300">
          The recurring log messages the agent has learned for each service —
          and how often each normally shows up — so it can spot a new or surging
          one.
        </p>
        <div className="mb-3 flex flex-wrap items-center gap-2">
          <SegmentedControl
            param={VERDICT_PARAM}
            defaultValue="uncurated"
            aria-label="Verdict filter"
            options={[
              {
                value: "uncurated",
                label: "Uncurated",
                badge: counts?.uncurated,
              },
              { value: "known", label: "Known", badge: counts?.known },
              { value: "spike", label: "Spike", badge: counts?.spike },
              { value: "all", label: "All", badge: counts?.all },
            ]}
          />
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
            <div
              tabIndex={0}
              onKeyDown={onTableKeyDown}
              className="max-h-[calc(100vh-230px)] overflow-auto"
            >
              <table className="ddt">
                <thead>
                  <tr>
                    <th className="w-10">
                      <input
                        type="checkbox"
                        aria-label="Select all visible patterns"
                        className="h-3.5 w-3.5 accent-accent"
                        checked={allSelected}
                        ref={(el) => {
                          if (el) el.indeterminate = !allSelected && someSelected;
                        }}
                        onChange={toggleAll}
                      />
                    </th>
                    <th className="w-20 text-right">Count</th>
                    <th className="w-24 text-right">Normal</th>
                    <th>Template</th>
                    <th className="w-28">Service</th>
                    <th className="w-24">Verdict</th>
                    <th className="w-44">Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {isLoading && <SkRows rows={8} cols={7} />}
                  {!isLoading && filtered.length === 0 && (
                    <tr>
                      <td colSpan={7}>
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
                        ) : verdictFilter === "uncurated" && !q.trim() ? (
                          <EmptyState title="No uncurated patterns — the catalog is fully labeled." />
                        ) : (
                          <EmptyState
                            title="No patterns match your filters"
                            hint="Try clearing the search or switching the verdict filter."
                          />
                        )}
                      </td>
                    </tr>
                  )}
                  {filtered.map((p, i) => (
                    <ClickableRow
                      key={p.id}
                      onOpen={() => setPeekId(p.id)}
                      {...keys.rowProps(i)}
                    >
                      <td>
                        <input
                          type="checkbox"
                          aria-label={`Select pattern ${p.id}`}
                          className="h-3.5 w-3.5 accent-accent"
                          checked={selected.has(p.id)}
                          onChange={() => toggleOne(p.id)}
                        />
                      </td>
                      <td className="text-right tabular-nums text-ink-100">
                        {p.count}
                      </td>
                      <td className="text-right tabular-nums text-ink-300">
                        ≈ {p.baseline_frequency.toFixed(1)}
                      </td>
                      <td className="max-w-0">
                        <div
                          className="truncate font-mono text-2xs text-ink-200"
                          title={p.template}
                        >
                          {p.template}
                        </div>
                      </td>
                      <td className="font-mono text-2xs text-ink-300">
                        {displayService(p.service)}
                      </td>
                      <td>
                        <VerdictPill verdict={p.verdict} />
                      </td>
                      <td>
                        <div className="flex items-center gap-1.5">
                          <button
                            aria-label={`Mark pattern ${p.id} as known`}
                            className="btn px-2 py-1 text-2xs"
                            disabled={p.verdict === "known"}
                            onClick={() =>
                              verdictMutation.mutate({
                                id: p.id,
                                verdict: "known",
                              })
                            }
                          >
                            <Check size={11} /> known
                          </button>
                          <button
                            aria-label={`Mark pattern ${p.id} as spike`}
                            className="btn px-2 py-1 text-2xs"
                            disabled={p.verdict === "spike"}
                            onClick={() =>
                              verdictMutation.mutate({
                                id: p.id,
                                verdict: "spike",
                              })
                            }
                          >
                            <Zap size={11} /> spike
                          </button>
                        </div>
                      </td>
                    </ClickableRow>
                  ))}
                </tbody>
              </table>
            </div>
          </div>
        )}
      </main>

      {/* Bulk action bar — appears on selection; sequential PATCHes. */}
      {selected.size > 0 && (
        <div
          role="toolbar"
          aria-label="Bulk pattern actions"
          className="fixed bottom-4 left-1/2 z-overlay flex -translate-x-1/2 flex-wrap items-center
                     gap-2 rounded-card border border-ink-500 bg-surface-raised px-4 py-2.5 shadow-overlay"
        >
          <span className="text-xs font-medium tabular-nums text-ink-50">
            {selected.size} selected
          </span>
          <button
            className="btn"
            disabled={bulkBusy}
            onClick={() => void runBulk([...selected], "known")}
          >
            <Check size={12} /> Mark known
          </button>
          <button
            className="btn"
            disabled={bulkBusy}
            onClick={() => void runBulk([...selected], "spike")}
          >
            <Zap size={12} /> Mark spike
          </button>
          <button
            className="btn"
            disabled={bulkBusy}
            onClick={() => void runBulk([...selected], "")}
          >
            Clear verdict
          </button>
          <button
            aria-label="Clear selection"
            className="text-2xs text-ink-300 hover:text-ink-100"
            disabled={bulkBusy}
            onClick={() => setSelected(new Set())}
          >
            Esc
          </button>
        </div>
      )}

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
                aria-label={`Mark pattern ${peek.id} as spike`}
                className="btn"
                disabled={peek.verdict === "spike"}
                onClick={() =>
                  verdictMutation.mutate({ id: peek.id, verdict: "spike" })
                }
              >
                <Zap size={12} /> Mark spike
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

      {confirmFlush && (
        <ConfirmDialog
          title="Flush catalog to disk"
          message="Write the in-memory pattern catalog to persists storage now."
          confirmLabel="Flush"
          busy={flush.isPending}
          error={flush.error}
          onConfirm={() => flush.mutate()}
          onClose={() => setConfirmFlush(false)}
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
