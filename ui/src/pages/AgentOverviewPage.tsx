import { useMemo } from "react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import clsx from "clsx";
import {
  Activity,
  AlertTriangle,
  CircleDot,
  EyeOff,
  GraduationCap,
  Layers,
  Lock,
  Power,
  PowerOff,
  Radar,
  Server,
  Sparkles,
  Zap,
  type LucideIcon,
} from "lucide-react";
import { api, ApiError, type AgentConfigView, type BaselineRow } from "@/lib/api";
import { fmtAbs, fmtRel, hourlyBuckets } from "@/lib/format";
import { useNowTick } from "@/lib/hooks";
import { TopBar } from "@/components/TopBar";
import { Pill, VerdictPill } from "@/components/Pill";
import { KpiTile } from "@/components/KpiTile";
import { SkRows } from "@/components/Skeleton";
import { RetryableError } from "@/components/RetryableError";
import { EmptyState } from "@/components/feedback";
import { ProportionalBarList } from "@/components/ProportionalBarList";

// Agent Overview (/agent) — StatusPage merged with the old Dashboard's
// agent cards. The runtime banner implements the runtime truth table:
//   config query loading  → skeleton banner (never the disabled treatment)
//   config query error    → RetryableError (the agent may be fine — WE
//                           couldn't ask)
//   success + enable:false → the one and only disabled state
//   success + enable:true  → mode chip (icon + text, never color alone)
//                            and "N/M enabled" sources from real data.
export function AgentOverviewPage() {
  const agentCfg = useQuery({
    queryKey: ["agent-config"],
    queryFn: api.getAgentConfig,
    retry: 2,
  });
  const status = useQuery({ queryKey: ["status"], queryFn: api.status, retry: 2 });
  const shadowStats = useQuery({
    queryKey: ["shadow-stats"],
    queryFn: api.shadowStats,
    retry: 2,
  });
  const detectStats = useQuery({
    queryKey: ["detect-stats"],
    queryFn: api.detectStats,
    retry: 2,
  });
  const patterns = useQuery({
    queryKey: ["patterns"],
    queryFn: api.listPatterns,
    retry: 2,
  });
  const shadow = useQuery({ queryKey: ["shadow"], queryFn: api.listShadow, retry: 2 });
  const services = useQuery({
    queryKey: ["services"],
    queryFn: api.listServices,
    retry: 2,
  });

  // Enterprise baselines probe — shows Metrics/Traces learning section.
  // Returns null when locked (403/404 = OSS or no intelligence license).
  const baselines = useQuery({
    queryKey: ["baselines-overview"],
    queryFn: async () => {
      try {
        return await api.listBaselines();
      } catch (e) {
        if (e instanceof ApiError && (e.status === 403 || e.status === 404)) {
          return null;
        }
        throw e;
      }
    },
    staleTime: 30_000,
    retry: 1,
  });

  // Only a SUCCESSFUL config response with enable:false means "disabled".
  const agentDisabled = agentCfg.isSuccess && !agentCfg.data.enable;

  // Enterprise (intelligence license) available when the baselines probe
  // returned a payload (null = locked OSS / no license). When available, Logs
  // joins Metrics & Traces in one signals line, so it leaves Lifetime totals.
  const enterpriseAvailable = baselines.data != null;

  // Services breakdown for the wide enterprise Services tile's ring: what
  // fraction of discovered services have passed the new-service grace window
  // (tracked) vs are still in grace (learning, not alerting yet).
  const svcTotal = services.data ? Object.keys(services.data).length : 0;
  const svcInGrace = services.data
    ? Object.values(services.data).filter((s) => s.in_grace).length
    : 0;
  const svcTracked = svcTotal - svcInGrace;
  const svcTrackedFrac = svcTotal > 0 ? svcTracked / svcTotal : 0;

  const topPatterns = useMemo(() => {
    const list = patterns.data ?? [];
    return [...list].sort((a, b) => b.count - a.count).slice(0, 5);
  }, [patterns.data]);

  const recentShadow = useMemo(
    () => (shadow.data ?? []).slice(0, 5),
    [shadow.data],
  );

  // Shadow activity over 24h from event last_seen stamps — the only
  // windowed series this page's queries already carry. Enhancement only:
  // the tile's number/loading still come from status/shadowStats. nowTick
  // keeps the window sliding while the data reference stays unchanged.
  const nowTick = useNowTick();
  const shadowTrend = useMemo(
    () =>
      shadow.data
        ? hourlyBuckets(
            shadow.data.map((e) => e.last_seen),
            24,
            nowTick,
          )
        : undefined,
    [shadow.data, nowTick],
  );
  const shadowTrend24 = useMemo(
    () => (shadowTrend ?? []).reduce((a, n) => a + n, 0),
    [shadowTrend],
  );

  // Flatten DetectStats: keys like outcome_emitted, verdict_spike, severity_high.
  const detectGroup = (prefix: string): Array<[string, number]> => {
    if (!detectStats.data) return [];
    return Object.entries(detectStats.data)
      .filter(([k]) => k.startsWith(prefix))
      .map(([k, v]) => [k.slice(prefix.length), v] as [string, number])
      .sort((a, b) => b[1] - a[1]);
  };
  const detectVerdicts = detectGroup("verdict_");
  const severities = detectGroup("severity_");

  // Derived detect counters — undefined (→ "—") until the query settles;
  // a missing key on a SETTLED response genuinely means zero.
  const dd = detectStats.data;
  const emitted = dd ? (dd["outcome_emitted"] ?? 0) : undefined;
  const cached = dd ? (dd["outcome_cached"] ?? 0) : undefined;
  const aiErrors = dd
    ? (dd["outcome_ai_error"] ?? 0) + (dd["outcome_send_error"] ?? 0)
    : undefined;

  // One consolidated RetryableError for the stat queries (they fail
  // together when the agent admin endpoints are down).
  type RetryHandle = {
    isError: boolean;
    error: unknown;
    isRefetching: boolean;
    refetch: () => unknown;
  };
  const statSources: Array<{ q: RetryHandle; what: string }> = [
    { q: status, what: "agent status" },
    { q: shadowStats, what: "shadow stats" },
    { q: detectStats, what: "detect stats" },
    { q: services, what: "the service list" },
  ];
  const failedStats = statSources.filter((s) => s.q.isError);

  return (
    <>
      <TopBar
        title="Agent"
      />

      <main className="flex-1 overflow-auto p-6">
        {/* 1 — Runtime banner (truth table) */}
        {agentCfg.isPending ? (
          <div className="card mb-4" aria-hidden>
            <div className="card-body flex flex-wrap items-center gap-6">
              <div className="sk h-4 w-28" />
              <div className="sk h-4 w-20" />
              <div className="sk h-4 w-32" />
              <div className="sk h-4 w-24" />
            </div>
          </div>
        ) : agentCfg.isError ? (
          <div className="mb-4">
            <RetryableError
              error={agentCfg.error}
              context="Couldn't read the agent config — the agent itself may be fine; we just couldn't ask."
              onRetry={() => agentCfg.refetch()}
              retrying={agentCfg.isRefetching}
            />
          </div>
        ) : (
          <RuntimeBanner cfg={agentCfg.data} />
        )}

        {/* 2 — Lifetime stat tiles (no 24h windowed stats yet — §3.5 ask #6) */}
        <section aria-labelledby="lifetime-totals-heading">
          <h2
            id="lifetime-totals-heading"
            className="mb-2 text-2xs font-semibold uppercase tracking-wide text-ink-400"
          >
            Lifetime totals
          </h2>
          <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
          {/* Service first (Service · Shadow · Detect), then Logs (OSS only)
              and the rest. Shadow/Detect are enterprise-fed once a license is
              present, but they render from the same status query in BOTH the
              OSS and enterprise paths, so the ordering is a pure ui/ change
              that holds for both — no separate enterprise component. */}
          <KpiTile
            label="Services tracked"
            value={
              services.data ? Object.keys(services.data).length : undefined
            }
            loading={services.isPending}
            to="/agent/services"
            icon={Server}
            className={enterpriseAvailable ? "lg:col-span-2" : undefined}
            chart={
              enterpriseAvailable && svcTotal > 0 ? (
                <ProgressRing progress={svcTrackedFrac} />
              ) : undefined
            }
            foot={
              enterpriseAvailable && svcTotal > 0
                ? `${svcTracked} tracked · ${svcInGrace} in grace`
                : "Discovered from logs"
            }
          />
          <KpiTile
            label="Shadow events"
            value={status.data?.shadow_events ?? shadowStats.data?.events}
            loading={status.isPending || shadowStats.isPending}
            to="/agent/decisions?tab=shadow"
            icon={EyeOff}
            spark={shadowTrend}
            sparkLabel={`${shadowTrend24} shadow events active in the last 24 hours`}
            foot={
              status.data
                ? status.data.shadow_dirty
                  ? "Unsaved changes"
                  : "Disk in sync"
                : undefined
            }
          />
          <KpiTile
            label="Detect events"
            value={status.data?.detect_events ?? dd?.["events"]}
            loading={status.isPending || detectStats.isPending}
            to="/agent/decisions?tab=detect"
            icon={Sparkles}
            foot={
              status.data
                ? status.data.detect_dirty
                  ? "Unsaved changes"
                  : "Disk in sync"
                : undefined
            }
          />
          {!enterpriseAvailable && (
            <KpiTile
              label="Log patterns"
              value={status.data?.patterns}
              loading={status.isPending}
              to="/agent/logs"
              icon={Layers}
              foot={
                status.data
                  ? status.data.dirty
                    ? "Unsaved changes"
                    : "Persisted"
                  : undefined
              }
            />
          )}
          <KpiTile
            label="Incidents emitted"
            value={emitted}
            loading={detectStats.isPending}
            to="/agent/decisions?tab=detect"
            icon={AlertTriangle}
            tone={emitted != null && emitted > 0 ? "critical" : undefined}
            foot="From detect mode"
          />
          <KpiTile
            label="AI cache hits"
            value={cached}
            loading={detectStats.isPending}
            to="/agent/decisions?tab=detect"
            icon={Zap}
            tone={cached != null && cached > 0 ? "ok" : undefined}
            foot="No model call needed"
          />
          <KpiTile
            label="AI / send errors"
            value={aiErrors}
            loading={detectStats.isPending}
            to="/agent/decisions?tab=detect"
            icon={AlertTriangle}
            tone={aiErrors != null && aiErrors > 0 ? "warn" : undefined}
            foot="Failed analyses + sends"
          />
          <KpiTile
            label="Total signals (shadow)"
            value={shadowStats.data?.total_signals}
            loading={shadowStats.isPending}
            to="/agent/decisions?tab=shadow"
            icon={Activity}
            foot="Across every shadow tick"
          />
          </div>

          {failedStats.length > 0 && (
            <div className="mt-3">
              {agentDisabled ? (
                <p className="text-xs text-ink-400">
                  Agent statistics are unavailable while the agent is disabled.
                </p>
              ) : (
                <RetryableError
                  error={failedStats[0].q.error}
                  context={`Couldn't load ${failedStats
                    .map((s) => s.what)
                    .join(", ")}`}
                  onRetry={() => failedStats.forEach((s) => s.q.refetch())}
                  retrying={failedStats.some((s) => s.q.isRefetching)}
                />
              )}
            </div>
          )}
        </section>

        {/* 3 — Logs, Metrics & Traces learning (Logs always; Metrics/Traces Enterprise-only) */}
        <section className="mt-6" aria-labelledby="signal-learning-heading">
          <h2
            id="signal-learning-heading"
            className="mb-2 text-2xs font-semibold uppercase tracking-wide text-ink-400"
          >
            Logs, Metrics &amp; Traces
          </h2>
          <div className="grid grid-cols-1 items-stretch gap-3 lg:grid-cols-2">
            <EnterpriseLearningSummary
              baselines={baselines.data}
              loading={baselines.isPending}
              locked={baselines.data === null}
              logPatterns={status.data?.patterns}
            />
            <DetectBreakdownCard
              isPending={detectStats.isPending}
              isError={detectStats.isError}
              agentDisabled={agentDisabled}
              verdicts={detectVerdicts}
              severities={severities}
            />
          </div>
        </section>

        {/* 4 — The old Dashboard agent cards */}
        <section className="mt-6 grid grid-cols-1 gap-6 lg:grid-cols-2">
          <div className="card">
            <div className="card-header">
              <h2 className="card-title">Top patterns by sightings</h2>
              <Link
                to="/agent/logs"
                className="text-2xs text-link hover:underline"
              >
                View all →
              </Link>
            </div>
            <div className="card-body">
              {patterns.isPending ? (
                <table className="ddt">
                  <thead>
                    <tr>
                      <th className="w-16 text-right">Count</th>
                      <th className="w-20 text-right">Normal</th>
                      <th className="w-24">Verdict</th>
                      <th>Template</th>
                    </tr>
                  </thead>
                  <tbody>
                    <SkRows rows={5} cols={4} />
                  </tbody>
                </table>
              ) : patterns.isError ? (
                agentDisabled ? (
                  <DisabledNote />
                ) : (
                  <RetryableError
                    error={patterns.error}
                    context="Couldn't load patterns"
                    onRetry={() => patterns.refetch()}
                    retrying={patterns.isRefetching}
                  />
                )
              ) : topPatterns.length === 0 ? (
                <EmptyState
                  title="No patterns yet"
                  hint="The miner learns templates as logs flow in."
                />
              ) : (
                <table className="ddt">
                  <thead>
                    <tr>
                      <th className="w-16 text-right">Count</th>
                      <th className="w-20 text-right">Normal</th>
                      <th className="w-24">Verdict</th>
                      <th>Template</th>
                    </tr>
                  </thead>
                  <tbody>
                    {topPatterns.map((p) => (
                      <tr key={p.id}>
                        <td className="text-right tabular-nums">
                          {p.count.toLocaleString()}
                        </td>
                        <td
                          className="text-right font-mono text-2xs text-ink-300"
                          title="Learned normal match rate (per second)"
                        >
                          ≈{p.baseline_frequency.toFixed(1)}/s
                        </td>
                        <td>
                          <VerdictPill verdict={p.verdict} />
                        </td>
                        <td className="max-w-0">
                          <Link
                            to={`/agent/logs/${p.id}`}
                            title={p.template}
                            className="block truncate font-mono text-2xs text-link hover:underline"
                          >
                            {p.template}
                          </Link>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </div>
          </div>

          <div className="card">
            <div className="card-header">
              <h2 className="card-title">Recent shadow events</h2>
              <Link
                to="/agent/decisions?tab=shadow"
                className="text-2xs text-link hover:underline"
              >
                View all →
              </Link>
            </div>
            <div className="card-body">
              {shadow.isPending ? (
                <table className="ddt">
                  <thead>
                    <tr>
                      <th className="w-24">Verdict</th>
                      <th>Sample</th>
                      <th className="w-28">Last seen</th>
                    </tr>
                  </thead>
                  <tbody>
                    <SkRows rows={5} cols={3} />
                  </tbody>
                </table>
              ) : shadow.isError ? (
                agentDisabled ? (
                  <DisabledNote />
                ) : (
                  <RetryableError
                    error={shadow.error}
                    context="Couldn't load shadow events"
                    onRetry={() => shadow.refetch()}
                    retrying={shadow.isRefetching}
                  />
                )
              ) : recentShadow.length === 0 ? (
                <EmptyState
                  title="No shadow events recorded"
                  hint="Shadow mode logs would-have-alerted decisions here."
                />
              ) : (
                <table className="ddt">
                  <thead>
                    <tr>
                      <th className="w-24">Verdict</th>
                      <th>Sample</th>
                      <th className="w-28">Last seen</th>
                    </tr>
                  </thead>
                  <tbody>
                    {recentShadow.map((e) => (
                      <tr key={`${e.pattern_id}-${e.first_seen}`}>
                        <td>
                          <VerdictPill verdict={e.verdict} />
                        </td>
                        <td className="max-w-0">
                          <Link
                            to={`/agent/decisions/shadow/${encodeURIComponent(
                              e.pattern_id,
                            )}`}
                            title={e.sample_message}
                            className="block truncate font-mono text-2xs text-ink-100 hover:text-link hover:underline"
                          >
                            {e.sample_message}
                          </Link>
                        </td>
                        <td
                          className="text-2xs text-ink-300"
                          title={fmtAbs(e.last_seen)}
                        >
                          {fmtRel(e.last_seen)}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </div>
          </div>
        </section>

      </main>
    </>
  );
}

function DisabledNote() {
  return (
    <p className="py-2 text-xs text-ink-400">
      Unavailable while the agent is disabled.
    </p>
  );
}

// ---------------------------------------------------------------------------
// Runtime banner — only reached with a SUCCESSFUL config response.
// ---------------------------------------------------------------------------
function RuntimeBanner({ cfg }: { cfg: AgentConfigView }) {
  if (!cfg.enable) {
    return (
      <div className="card mb-4">
        <div className="card-body flex flex-wrap items-center gap-x-4 gap-y-2 text-xs">
          <div className="flex items-center gap-2">
            <PowerOff size={14} className="text-ink-400" aria-hidden />
            <span className="font-medium text-ink-50">Agent</span>
            <Pill>disabled</Pill>
          </div>
          <p className="text-ink-300">
            The agent loop is off (<code>agent.enable: false</code>). The data
            below reflects its last run.
          </p>
        </div>
      </div>
    );
  }

  const aiEnabled = !!cfg.ai?.enable;

  return (
    <div className="card mb-4">
      <div className="card-body flex flex-wrap items-center gap-x-6 gap-y-3 text-xs">
        <div className="flex items-center gap-2">
          <Power size={14} className="text-sev-ok" aria-hidden />
          <span className="font-medium text-ink-50">Agent</span>
          <Pill tone="good">enabled</Pill>
        </div>
        {cfg.mode && (
          <div className="flex items-center gap-2">
            <span className="text-ink-300">Mode</span>
            <ModeChip mode={cfg.mode} />
          </div>
        )}
        <div className="flex items-center gap-2">
          <span className="text-ink-300">AI</span>
          <Pill tone={aiEnabled ? "accent" : undefined}>
            {aiEnabled ? cfg.ai.model || "on" : "off"}
          </Pill>
        </div>
      </div>
    </div>
  );
}

// Mode chip: training=info, shadow=warn, detect=ok — icon + text, so the
// state is never conveyed by color alone.
const MODE_CHIP: Record<string, { cls: string; icon: LucideIcon }> = {
  detect: { cls: "border-sev-ok/40 bg-sev-ok/15 text-sev-ok", icon: Radar },
  shadow: { cls: "border-sev-warn/40 bg-sev-warn/15 text-sev-warn", icon: EyeOff },
  training: {
    cls: "border-sev-info/40 bg-sev-info/15 text-sev-info",
    icon: GraduationCap,
  },
};

function ModeChip({ mode }: { mode: string }) {
  const m = MODE_CHIP[mode.toLowerCase()] ?? {
    cls: "border-ink-500 bg-ink-700 text-ink-200",
    icon: CircleDot,
  };
  const Icon = m.icon;
  return (
    <span
      className={clsx(
        "inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-2xs font-medium",
        m.cls,
      )}
    >
      <Icon size={11} aria-hidden />
      {mode}
    </span>
  );
}

// ---------------------------------------------------------------------------
// Errors point at the consolidated RetryableError above the cards.
// ---------------------------------------------------------------------------
function DetectBreakdownCard({
  verdicts,
  severities,
  isPending,
  isError,
  agentDisabled,
}: {
  verdicts: Array<[string, number]>;
  severities: Array<[string, number]>;
  isPending: boolean;
  isError: boolean;
  agentDisabled: boolean;
}) {
  const groups = [
    ["AI Detect", verdicts],
    ["AI severity", severities],
  ] as const;
  const empty = groups.every(([, rows]) => rows.length === 0);
  return (
    <div className="card">
      <div className="card-header">
        <h2 className="card-title">Detect breakdown</h2>
        <Link
          to="/agent/decisions?tab=detect"
          className="text-2xs text-link hover:underline"
        >
          View detect →
        </Link>
      </div>
      <div className="card-body">
        {isPending ? (
          <div aria-hidden className="grid gap-5 md:grid-cols-2">
            {Array.from({ length: 2 }).map((_, group) => (
              <div key={group} className="space-y-3">
                <div className="sk h-3 w-20" />
                {Array.from({ length: 3 }).map((__, row) => (
                  <div key={row} className="space-y-1">
                    <div className="sk h-3 w-full" />
                    <div className="sk h-2 w-full rounded-full" />
                  </div>
                ))}
              </div>
            ))}
          </div>
        ) : isError ? (
          agentDisabled ? (
            <DisabledNote />
          ) : (
            <p className="py-2 text-xs text-ink-300">
              Couldn't load — use Retry in the error above.
            </p>
          )
        ) : empty ? (
          <EmptyState title="No detect-mode calls yet" />
        ) : (
          <div className="grid gap-5 md:grid-cols-2">
            {groups.map(([title, rows]) => (
              <div key={title} className="min-w-0">
                <h3 className="mb-3 text-2xs font-semibold uppercase tracking-wide text-ink-400 text-center">
                  {title}
                </h3>
                {rows.length > 0 ? (
                  <ProportionalBarList rows={rows} label={`Detect ${title}`} />
                ) : (
                  <p className="text-xs text-ink-400">No data</p>
                )}
              </div>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Enterprise Metrics/Traces learning summary — shows signal counts, ready-to-
// detect progress, and links to the detail pages. When locked (OSS / no
// intelligence license) shows a subtle locked banner. Renders nothing while
// loading (skeleton is optional; this section is non-critical).
// ---------------------------------------------------------------------------
function EnterpriseLearningSummary({
  baselines,
  loading,
  locked,
  logPatterns,
}: {
  baselines: { baselines: BaselineRow[] } | null | undefined;
  loading: boolean;
  locked: boolean;
  logPatterns: number | undefined;
}) {
  if (loading) {
    return (
      <div className="sk h-52 rounded-card" />
    );
  }

  if (locked) {
    return (
      <div className="card min-w-0">
        <div className="card-header">
          <h3 className="card-title">Signals learned</h3>
        </div>
        <div className="card-body space-y-4">
          <ProportionalBarList
            rows={[["Logs", logPatterns ?? 0]]}
            label="Signals learned"
          />
          <div className="flex items-center gap-2 rounded-control border border-ink-700 bg-surface-raised px-3 py-2 text-xs text-ink-400">
          <Lock size={14} className="shrink-0" />
          <span>
            Metrics and Traces learning is an Enterprise feature.{" "}
            <Link to="/agent/metrics" className="text-link hover:underline">
              Learn more →
            </Link>
          </span>
          </div>
        </div>
      </div>
    );
  }

  if (!baselines) return null;

  const rows = baselines.baselines ?? [];
  const metrics = rows.filter((r) => r.type === "metric");
  const traces = rows.filter((r) => r.type === "trace");
  const logCount = logPatterns ?? 0;

  return (
    <div className="card min-w-0">
      <div className="card-header gap-2">
        <h3 className="card-title">Signals learned</h3>
        <div className="flex items-center gap-3 text-2xs">
          <Link to="/agent/logs" className="text-link hover:underline">Logs</Link>
          <Link to="/agent/metrics" className="text-link hover:underline">Metrics</Link>
          <Link to="/agent/traces" className="text-link hover:underline">Traces</Link>
        </div>
      </div>
      <div className="card-body">
        <ProportionalBarList
          rows={[
            ["Logs", logCount],
            ["Metrics", metrics.length],
            ["Traces", traces.length],
          ]}
          label="Signals learned"
        />
      </div>
    </div>
  );
}

// Tiny SVG progress ring for the enterprise learning cards.
function ProgressRing({ progress }: { progress: number }) {
  const r = 14;
  const c = 2 * Math.PI * r;
  const offset = c * (1 - Math.min(1, Math.max(0, progress)));
  return (
    <svg width={36} height={36} className="shrink-0">
      <circle
        cx={18}
        cy={18}
        r={r}
        fill="none"
        stroke="currentColor"
        className="text-ink-700"
        strokeWidth={3}
      />
      <circle
        cx={18}
        cy={18}
        r={r}
        fill="none"
        stroke="currentColor"
        className="text-accent"
        strokeWidth={3}
        strokeDasharray={c}
        strokeDashoffset={offset}
        strokeLinecap="round"
        transform="rotate(-90 18 18)"
      />
      <text
        x={18}
        y={18}
        textAnchor="middle"
        dominantBaseline="central"
        className="fill-ink-100 text-[9px] font-semibold"
      >
        {Math.round(progress * 100)}%
      </text>
    </svg>
  );
}
