import { useMemo, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  AlertTriangle,
  Database,
  Eraser,
  EyeOff,
  ScrollText,
  Send,
  Timer,
} from "lucide-react";
import clsx from "clsx";
import { api, type DetectEvent, type ShadowEvent } from "@/lib/api";
import { fmtAbs, fmtRel, truncate } from "@/lib/format";
import { useTableKeys } from "@/lib/hooks";
import { buildSpikeRows, type SpikeRow } from "@/lib/spikeRows";
import { TopBar } from "@/components/TopBar";
import { Pill, VerdictPill } from "@/components/Pill";
import { SeverityBadge } from "@/components/SeverityBadge";
import { SegmentedControl } from "@/components/SegmentedControl";
import { ClickableRow } from "@/components/DataTable";
import { SkRows } from "@/components/Skeleton";
import { RetryableError } from "@/components/RetryableError";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { Pagination } from "@/components/Pagination";
import { usePagination } from "@/lib/pagination";
import { useToast } from "@/components/toastContext";
import { EmptyState, EmptyValue } from "@/components/feedback";
import { SYSTEM_PROMPT_PATH } from "@/lib/systemPromptNav";

type Tab = "detect" | "shadow" | "spike";

// Decisions — Detect + Shadow + Spike as TABS of one page (UX_REDESIGN §2.1).
// One mental model: what the agent decided (detect) / would have decided
// (shadow) / the log templates that surged past baseline (spike). The active
// tab lives in ?tab= so every view is shareable; legacy /detect and /shadow
// land here via App.tsx redirects with ?tab=.
export function DecisionsPage() {
  const [params] = useSearchParams();
  const raw = params.get("tab");
  const tab: Tab =
    raw === "shadow" ? "shadow" : raw === "spike" ? "spike" : "detect";

  const detectStats = useQuery({
    queryKey: ["detect-stats"],
    queryFn: api.detectStats,
  });
  const shadowStats = useQuery({
    queryKey: ["shadow-stats"],
    queryFn: api.shadowStats,
  });

  // Spike count spans BOTH logs: shadow-mode "would have alerted" spikes AND
  // the detect-mode spikes the agent acted on (verdict_spike in detect stats).
  // The badge/subtitle wait until both stat queries settle so they never show
  // a partial (shadow-only) count.
  const spikeReady = shadowStats.data != null && detectStats.data != null;
  const spikeCount =
    (shadowStats.data?.verdicts?.spike ?? 0) +
    (detectStats.data?.["verdict_spike"] ?? 0);
  const subtitle =
    tab === "detect"
      ? detectStats.data
        ? `${detectStats.data.events ?? 0} AI calls audited`
        : undefined
      : tab === "spike"
        ? spikeReady
          ? `${spikeCount} spikes detected`
          : undefined
        : shadowStats.data
          ? `${shadowStats.data.events} entries · ${shadowStats.data.total_signals} signals`
          : undefined;

  return (
    <>
      <TopBar
        title="Decisions"
        subtitle={subtitle}
        actions={
          <Link to={SYSTEM_PROMPT_PATH} className="btn">
            <ScrollText size={12} aria-hidden /> System prompt
          </Link>
        }
      />

      <main className="flex-1 overflow-auto p-6">
        <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
          <SegmentedControl
            param="tab"
            defaultValue="detect"
            aria-label="Decision log"
            options={[
              {
                value: "detect",
                label: "Detect",
                badge: detectStats.data?.events,
              },
              {
                value: "shadow",
                label: "Shadow",
                badge: shadowStats.data?.events,
              },
              {
                value: "spike",
                label: "Spike",
                badge: spikeReady ? spikeCount : undefined,
              },
            ]}
          />
          <div className="flex flex-wrap items-center gap-2">
            {/* key={tab} resets confirm/mutation state when the tab flips so a
                pending Clear dialog can never target the other log. Spike is a
                read-only view of shadow data — no clear there. */}
            {tab !== "spike" && <LogActions key={tab} tab={tab} />}
          </div>
        </div>

        {tab === "detect" ? (
          <DetectTab />
        ) : tab === "spike" ? (
          <SpikeTab />
        ) : (
          <ShadowTab />
        )}
      </main>
    </>
  );
}

// ---------------------------------------------------------------------------
// Clear for the ACTIVE tab's log. The audit found the mutation failing
// silently — every outcome now lands a toast, and Clear keeps its
// confirmation (on the accessible ConfirmDialog, no window.confirm).
// ---------------------------------------------------------------------------
function LogActions({ tab }: { tab: "detect" | "shadow" }) {
  const qc = useQueryClient();
  const toast = useToast();
  const [confirmClear, setConfirmClear] = useState(false);
  const name = tab === "detect" ? "Detect" : "Shadow";

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: [tab] });
    qc.invalidateQueries({ queryKey: [`${tab}-stats`] });
  };

  const clear = useMutation({
    mutationFn: tab === "detect" ? api.clearDetect : api.clearShadow,
    onSuccess: (r) => {
      invalidate();
      setConfirmClear(false);
      toast.push({
        tone: "ok",
        title: `${name} log cleared`,
        description: `${r.cleared} events removed from disk.`,
      });
    },
    onError: (err) => {
      // The dialog stays open with the inline error; the toast makes the
      // failure visible even if the operator already closed it.
      toast.push({
        tone: "error",
        title: `Couldn't clear the ${tab} log`,
        description: err instanceof Error ? err.message : String(err),
      });
    },
  });

  return (
    <>
      <button
        className="btn btn-danger"
        onClick={() => {
          clear.reset();
          setConfirmClear(true);
        }}
        disabled={clear.isPending}
      >
        <Eraser size={12} aria-hidden /> Clear
      </button>
      {confirmClear && (
        <ConfirmDialog
          title={`Clear ${tab} log`}
          message={
            <>
              This permanently removes every {tab} event from disk — the audit
              trail of what the agent{" "}
              {tab === "detect" ? "decided" : "would have decided"} is lost.
              This cannot be undone.
            </>
          }
          confirmLabel="Clear"
          tone="danger"
          busy={clear.isPending}
          error={clear.error instanceof Error ? clear.error : null}
          onConfirm={() => clear.mutate()}
          onClose={() => setConfirmClear(false)}
        />
      )}
    </>
  );
}

// ---------------------------------------------------------------------------
// Detect tab — the existing DetectPage table moved in: columns, outcome
// filter strip (counts double as the stats strip) and empty copy preserved.
// ---------------------------------------------------------------------------

const OUTCOMES = [
  "all",
  "emitted",
  "cached",
  "dry",
  "quota",
  "ai_error",
  "send_error",
] as const;
type OutcomeFilter = (typeof OUTCOMES)[number];

// Filter labels are humanized; the raw snake_case key stays the API value.
const OUTCOME_LABELS: Record<OutcomeFilter, string> = {
  all: "All",
  emitted: "Emitted",
  cached: "Cached",
  dry: "Dry",
  quota: "Quota",
  ai_error: "AI error",
  send_error: "Send error",
};

const DETECT_COLS = 9;

function DetectTab() {
  const navigate = useNavigate();
  const events = useQuery({ queryKey: ["detect"], queryFn: api.listDetect });
  const stats = useQuery({
    queryKey: ["detect-stats"],
    queryFn: api.detectStats,
  });

  const [filter, setFilter] = useState<OutcomeFilter>("all");

  const list = useMemo(() => {
    if (!events.data) return [];
    if (filter === "all") return events.data;
    return events.data.filter((e) => e.outcome === filter);
  }, [events.data, filter]);

  const pg = usePagination(list, { resetKey: filter });

  const keys = useTableKeys({
    size: pg.pageItems.length,
    onOpen: (i) => {
      const e = pg.pageItems[i];
      if (e) navigate(`/agent/decisions/detect/${encodeURIComponent(e.id)}`);
    },
  });

  return (
    <>
      <div
        role="group"
        aria-label="Outcome filter"
        className="mb-3 inline-flex flex-wrap gap-0.5 rounded-control border border-ink-500 bg-surface-raised p-0.5 text-xs"
      >
        {OUTCOMES.map((f) => {
          const count = stats.data
            ? f === "all"
              ? (stats.data.events ?? 0)
              : (stats.data[`outcome_${f}`] ?? 0)
            : undefined;
          return (
            <button
              key={f}
              onClick={() => setFilter(f)}
              aria-pressed={filter === f}
              className={clsx(
                "rounded px-3 py-1 transition-colors",
                filter === f
                  ? "bg-accent-subtle text-ink-50"
                  : "text-ink-300 hover:text-ink-100",
              )}
            >
              {OUTCOME_LABELS[f]}
              {count !== undefined && (
                <span className="ml-1 text-2xs opacity-70">({count})</span>
              )}
            </button>
          );
        })}
      </div>

      {events.isError && (
        <div className="mb-3">
          <RetryableError
            error={events.error}
            onRetry={() => events.refetch()}
            retrying={events.isRefetching}
            context="Couldn't load detect decisions"
          />
        </div>
      )}
      {stats.isError && (
        <div className="mb-3">
          <RetryableError
            error={stats.error}
            onRetry={() => stats.refetch()}
            retrying={stats.isRefetching}
            context="Couldn't load detect stats — filter counts hidden meanwhile"
          />
        </div>
      )}

      <div className="card overflow-hidden">
        <div
          className="max-h-[calc(100vh-260px)] overflow-auto"
          aria-label="Detect decisions table — j/k to move, Enter to open"
          {...keys.containerProps}
        >
          <table className="ddt">
            <thead>
              <tr>
                <th className="w-32">Service</th>
                <th className="w-32">When</th>
                <th className="w-28">Outcome</th>
                <th className="w-24">Verdict</th>
                <th className="w-24">Severity</th>
                <th className="w-32">Pattern</th>
                <th>Title / Sample</th>
                <th className="w-16 text-right">Freq</th>
                <th className="w-16 text-right">ms</th>
              </tr>
            </thead>
            <tbody>
              {events.isLoading && <SkRows rows={6} cols={DETECT_COLS} />}
              {!events.isLoading && !events.isError && list.length === 0 && (
                <tr>
                  <td colSpan={DETECT_COLS}>
                    {/* Truly-empty vs filtered-empty: the detect-mode hint
                        and the settings CTA would mislead an operator whose
                        agent IS emitting events that just don't match the
                        selected outcome. */}
                    {(events.data?.length ?? 0) === 0 ? (
                      <EmptyState
                        title="No detect events yet"
                        hint="Switch the agent to detect mode and let it call the AI SRE."
                        action={
                          <Link to="/settings?tab=agent" className="btn">
                            View agent settings
                          </Link>
                        }
                      />
                    ) : (
                      <EmptyState
                        title={`No ${OUTCOME_LABELS[filter].toLowerCase()} events`}
                        hint="Try a different outcome filter."
                      />
                    )}
                  </td>
                </tr>
              )}
              {pg.pageItems.map((e, i) => (
                <DetectRow key={e.id} e={e} rowProps={keys.rowProps(i)} />
              ))}
            </tbody>
          </table>
        </div>
        <Pagination state={pg} />
      </div>
    </>
  );
}

function DetectRow({
  e,
  rowProps,
}: {
  e: DetectEvent;
  rowProps: React.HTMLAttributes<HTMLTableRowElement>;
}) {
  const titleOrSample =
    e.finding?.Title ||
    (e.samples && e.samples[0]) ||
    e.template ||
    e.error ||
    "—";
  const href = `/agent/decisions/detect/${encodeURIComponent(e.id)}`;
  return (
    <ClickableRow to={href} {...rowProps}>
      <td className="text-2xs text-ink-200">
        {e.service && e.service !== "_unknown" ? e.service : <EmptyValue />}
      </td>
      <td className="text-2xs text-ink-300" title={fmtAbs(e.timestamp)}>
        {fmtRel(e.timestamp)}
      </td>
      <td>
        <OutcomePill outcome={e.outcome} />
      </td>
      <td>
        <VerdictPill verdict={e.verdict} />
      </td>
      <td>
        <SeverityBadge severity={e.finding?.Severity} />
      </td>
      <td className="font-mono text-2xs">
        <Link
          to={`/agent/logs/${encodeURIComponent(e.pattern_id)}`}
          className="text-link hover:underline"
          title={`Open pattern ${e.pattern_id}`}
        >
          {truncate(e.pattern_id, 14)}
        </Link>
      </td>
      <td className="font-mono text-2xs text-ink-200">
        <Link to={href} className="hover:text-link hover:underline">
          {truncate(titleOrSample, 120)}
        </Link>
      </td>
      <td className="text-right tabular-nums">{e.frequency}</td>
      <td className="text-right tabular-nums text-ink-400">
        {e.duration_ms ?? <EmptyValue />}
      </td>
    </ClickableRow>
  );
}

// OutcomePill — icon + tone pairs so the outcome is never conveyed by color
// alone (§2.1): emitted=accent/Send, cached=neutral/Database, dry=neutral,
// noise_filtered=warn/EyeOff, rate_limited|quota=warn/Timer,
// ai_error|send_error=bad/AlertTriangle. Also consumed by DetectDetailPage.
// Labels share PILL_LABELS with the filter strip — a row chipped
// "ai_error" under a filter reading "AI error" is the same enum twice.
const PILL_LABELS: Record<string, string> = {
  emitted: "Emitted",
  cached: "Cached",
  dry: "Dry",
  quota: "Quota",
  noise_filtered: "Noise filtered",
  rate_limited: "Rate limited",
  ai_error: "AI error",
  send_error: "Send error",
};

export function OutcomePill({ outcome }: { outcome: string }) {
  const o = (outcome || "").toLowerCase();
  const label = PILL_LABELS[o] ?? outcome;
  if (o === "emitted")
    return (
      <Pill tone="accent">
        <Send size={11} aria-hidden /> {label}
      </Pill>
    );
  if (o === "cached")
    return (
      <Pill>
        <Database size={11} aria-hidden /> {label}
      </Pill>
    );
  if (o === "dry") return <Pill>{label}</Pill>;
  if (o === "noise_filtered")
    return (
      <Pill tone="warn">
        <EyeOff size={11} aria-hidden /> {label}
      </Pill>
    );
  if (o === "rate_limited" || o === "quota")
    return (
      <Pill tone="warn">
        <Timer size={11} aria-hidden /> {label}
      </Pill>
    );
  if (o === "ai_error" || o === "send_error")
    return (
      <Pill tone="bad">
        <AlertTriangle size={11} aria-hidden /> {label}
      </Pill>
    );
  return <Pill>{outcome || "—"}</Pill>;
}

// ---------------------------------------------------------------------------
// Shadow tab — the existing ShadowPage table moved in: columns, verdict
// filter strip and empty copy preserved.
// ---------------------------------------------------------------------------

const VERDICT_FILTERS = ["all", "spike", "unknown"] as const;
type VerdictFilter = (typeof VERDICT_FILTERS)[number];

const SHADOW_COLS = 9;

function ShadowEventsTable({
  list,
  isLoading,
  resetKey,
  empty,
}: {
  list: ShadowEvent[];
  isLoading: boolean;
  resetKey: string;
  empty: React.ReactNode;
}) {
  const navigate = useNavigate();
  const pg = usePagination(list, { resetKey });

  const keys = useTableKeys({
    size: pg.pageItems.length,
    onOpen: (i) => {
      const e = pg.pageItems[i];
      if (e)
        navigate(
          `/agent/decisions/shadow/${encodeURIComponent(e.pattern_id)}`,
        );
    },
  });

  return (
    <div className="card overflow-hidden">
      <div
        className="max-h-[calc(100vh-260px)] overflow-auto"
        aria-label="Shadow decisions table — j/k to move, Enter to open"
        {...keys.containerProps}
      >
        <table className="ddt">
          <thead>
            <tr>
              <th className="w-28">Service</th>
              <th className="w-28">Verdict</th>
              <th className="w-32">Pattern</th>
              <th className="w-28">Source</th>
              <th className="w-24">Rule</th>
              <th className="w-20 text-right">Signals</th>
              <th className="w-20 text-right">Ticks</th>
              <th>Sample</th>
              <th className="w-32">Last seen</th>
            </tr>
          </thead>
          <tbody>
            {isLoading && <SkRows rows={6} cols={SHADOW_COLS} />}
            {!isLoading && list.length === 0 && (
              <tr>
                <td colSpan={SHADOW_COLS}>{empty}</td>
              </tr>
            )}
            {pg.pageItems.map((e, i) => {
              const href = `/agent/decisions/shadow/${encodeURIComponent(e.pattern_id)}`;
              return (
                <ClickableRow
                  key={`${e.pattern_id}-${e.first_seen}`}
                  to={href}
                  {...keys.rowProps(i)}
                >
                  <td className="text-2xs text-ink-200">
                    {e.service && e.service !== "_unknown" ? (
                      e.service
                    ) : (
                      <EmptyValue />
                    )}
                  </td>
                  <td>
                    <VerdictPill verdict={e.verdict} />
                  </td>
                  <td className="font-mono text-2xs">
                    <Link
                      to={`/agent/logs/${encodeURIComponent(e.pattern_id)}`}
                      className="text-link hover:underline"
                      title={`Open pattern ${e.pattern_id}`}
                    >
                      {e.pattern_id}
                    </Link>
                  </td>
                  <td className="text-2xs text-ink-200">{e.source}</td>
                  <td className="text-2xs text-ink-200">
                    {e.rule_name ? e.rule_name : <EmptyValue />}
                  </td>
                  <td className="text-right tabular-nums">{e.count}</td>
                  <td className="text-right tabular-nums">{e.occurrences}</td>
                  <td className="font-mono text-2xs text-ink-200">
                    <Link
                      to={href}
                      className="hover:text-link hover:underline"
                    >
                      {truncate(e.sample_message, 120)}
                    </Link>
                  </td>
                  <td
                    className="text-2xs text-ink-300"
                    title={fmtAbs(e.last_seen)}
                  >
                    {fmtRel(e.last_seen)}
                  </td>
                </ClickableRow>
              );
            })}
          </tbody>
        </table>
      </div>
      <Pagination state={pg} />
    </div>
  );
}

function ShadowTab() {
  const events = useQuery({ queryKey: ["shadow"], queryFn: api.listShadow });
  const stats = useQuery({
    queryKey: ["shadow-stats"],
    queryFn: api.shadowStats,
  });

  const [filter, setFilter] = useState<VerdictFilter>("all");

  const list = useMemo(() => {
    if (!events.data) return [];
    if (filter === "all") return events.data;
    return events.data.filter((e) => e.verdict === filter);
  }, [events.data, filter]);

  const verdictCount = (v: "spike" | "unknown") =>
    stats.data ? ` (${stats.data.verdicts?.[v] ?? 0})` : "";

  return (
    <>
      <div
        role="group"
        aria-label="Verdict filter"
        className="mb-3 inline-flex flex-wrap gap-0.5 rounded-control border border-ink-500 bg-surface-raised p-0.5 text-xs"
      >
        {VERDICT_FILTERS.map((f) => (
          <button
            key={f}
            onClick={() => setFilter(f)}
            aria-pressed={filter === f}
            className={clsx(
              "rounded px-3 py-1 transition-colors",
              filter === f
                ? "bg-accent-subtle text-ink-50"
                : "text-ink-300 hover:text-ink-100",
            )}
          >
            {f === "all"
              ? "All"
              : f === "spike"
                ? `Spikes${verdictCount("spike")}`
                : `Unknown${verdictCount("unknown")}`}
          </button>
        ))}
      </div>

      {events.isError && (
        <div className="mb-3">
          <RetryableError
            error={events.error}
            onRetry={() => events.refetch()}
            retrying={events.isRefetching}
            context="Couldn't load shadow decisions"
          />
        </div>
      )}
      {stats.isError && (
        <div className="mb-3">
          <RetryableError
            error={stats.error}
            onRetry={() => stats.refetch()}
            retrying={stats.isRefetching}
            context="Couldn't load shadow stats — filter counts hidden meanwhile"
          />
        </div>
      )}

      <ShadowEventsTable
        list={list}
        isLoading={events.isLoading}
        resetKey={filter}
        empty={
          <EmptyState
            title="No shadow events match this filter."
            hint="Switch to shadow mode and inject some traffic."
          />
        }
      />
    </>
  );
}

// Spike tab — every spike the agent flagged, from BOTH decision logs: the
// detect-mode spikes it acted on (which fire incidents) and the shadow-mode
// "would have alerted" surges. Merging them here is the fix for a spike
// AI-detect incident being invisible on Decisions: detect spikes live in the
// detect log, so a shadow-only Spike view hid them (and showed nothing at all
// while the agent ran in detect mode). Read-only — no clear action.
function SpikeTab() {
  const detect = useQuery({ queryKey: ["detect"], queryFn: api.listDetect });
  const shadow = useQuery({ queryKey: ["shadow"], queryFn: api.listShadow });

  const rows = useMemo(
    () => buildSpikeRows(detect.data, shadow.data),
    [detect.data, shadow.data],
  );

  const isLoading = detect.isLoading || shadow.isLoading;
  const failed = detect.isError ? detect : shadow.isError ? shadow : null;

  return (
    <>
      <p className="mb-3 max-w-3xl text-xs text-ink-300">
        Every spike the agent flagged — the AI-detect events it acted on and the
        shadow-mode “would have alerted” surges, in one place. Open one to see
        the pattern and why it tripped.
      </p>

      {failed && (
        <div className="mb-3">
          <RetryableError
            error={failed.error}
            onRetry={() => failed.refetch()}
            retrying={failed.isRefetching}
            context="Couldn't load spike signals"
          />
        </div>
      )}

      <SpikeTable rows={rows} isLoading={isLoading} />
    </>
  );
}

const SPIKE_COLS = 7;

// SpikeKindPill labels which decision log a spike came from — AI-detect (the
// agent acted) vs Shadow (would have alerted). Text, never color alone.
function SpikeKindPill({ kind }: { kind: SpikeRow["kind"] }) {
  return kind === "detect" ? (
    <Pill tone="accent">AI-detect</Pill>
  ) : (
    <Pill tone="warn">Shadow</Pill>
  );
}

function SpikeTable({
  rows,
  isLoading,
}: {
  rows: SpikeRow[];
  isLoading: boolean;
}) {
  const navigate = useNavigate();
  const pg = usePagination(rows, { resetKey: "spike" });

  const keys = useTableKeys({
    size: pg.pageItems.length,
    onOpen: (i) => {
      const r = pg.pageItems[i];
      if (r) navigate(r.href);
    },
  });

  return (
    <div className="card overflow-hidden">
      <div
        className="max-h-[calc(100vh-260px)] overflow-auto"
        aria-label="Spike signals table — j/k to move, Enter to open"
        {...keys.containerProps}
      >
        <table className="ddt">
          <thead>
            <tr>
              <th className="w-28">Service</th>
              <th className="w-24">Kind</th>
              <th className="w-32">Pattern</th>
              <th className="w-28">Source</th>
              <th>Sample</th>
              <th className="w-20 text-right">Signals</th>
              <th className="w-32">When</th>
            </tr>
          </thead>
          <tbody>
            {isLoading && <SkRows rows={6} cols={SPIKE_COLS} />}
            {!isLoading && rows.length === 0 && (
              <tr>
                <td colSpan={SPIKE_COLS}>
                  <EmptyState
                    title="No spikes yet"
                    hint="When a pattern surges past its baseline in shadow or detect mode, it shows up here."
                  />
                </td>
              </tr>
            )}
            {pg.pageItems.map((r, i) => (
              <ClickableRow key={r.key} to={r.href} {...keys.rowProps(i)}>
                <td className="text-2xs text-ink-200">
                  {r.service && r.service !== "_unknown" ? (
                    r.service
                  ) : (
                    <EmptyValue />
                  )}
                </td>
                <td>
                  <SpikeKindPill kind={r.kind} />
                </td>
                <td className="font-mono text-2xs">
                  <Link
                    to={`/agent/logs/${encodeURIComponent(r.patternId)}`}
                    className="text-link hover:underline"
                    title={`Open pattern ${r.patternId}`}
                  >
                    {truncate(r.patternId, 14)}
                  </Link>
                </td>
                <td className="text-2xs text-ink-200">{r.source}</td>
                <td className="font-mono text-2xs text-ink-200">
                  <Link to={r.href} className="hover:text-link hover:underline">
                    {r.sample ? truncate(r.sample, 120) : <EmptyValue />}
                  </Link>
                </td>
                <td className="text-right tabular-nums">{r.count}</td>
                <td className="text-2xs text-ink-300" title={fmtAbs(r.when)}>
                  {fmtRel(r.when)}
                </td>
              </ClickableRow>
            ))}
          </tbody>
        </table>
      </div>
      <Pagination state={pg} />
    </div>
  );
}
