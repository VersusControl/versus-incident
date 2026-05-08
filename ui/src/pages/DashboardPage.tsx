import { useMemo } from "react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import {
  AlertTriangle,
  EyeOff,
  Layers,
  Server,
  type LucideIcon,
} from "lucide-react";
import { api } from "@/lib/api";
import { fmtRel, truncate } from "@/lib/format";
import { TopBar } from "@/components/TopBar";
import { Pill, VerdictPill } from "@/components/Pill";
import { ErrorBox, Spinner } from "@/components/feedback";

// DashboardPage is the landing screen — a single-glance overview that
// pulls from the incident store and (when the agent is enabled) the
// agent admin endpoints. Each tile/section deep-links to the focused
// page for that surface.
export function DashboardPage() {
  const incidents = useQuery({
    queryKey: ["incidents"],
    queryFn: () => api.listIncidents(),
  });
  // Agent endpoints may 404/401 if the agent is disabled — keep them
  // optional so the dashboard still renders.
  const status = useQuery({
    queryKey: ["status"],
    queryFn: api.status,
    retry: 0,
  });
  const shadowStats = useQuery({
    queryKey: ["shadow-stats"],
    queryFn: api.shadowStats,
    retry: 0,
  });
  const shadow = useQuery({
    queryKey: ["shadow"],
    queryFn: api.listShadow,
    retry: 0,
  });
  const services = useQuery({
    queryKey: ["services"],
    queryFn: api.listServices,
    retry: 0,
  });
  const patterns = useQuery({
    queryKey: ["patterns"],
    queryFn: api.listPatterns,
    retry: 0,
  });

  const counts = useMemo(() => {
    const list = incidents.data ?? [];
    return {
      total: list.length,
      open: list.filter((i) => !i.resolved && !i.acked_at).length,
      acked: list.filter((i) => !i.resolved && i.acked_at).length,
      resolved: list.filter((i) => i.resolved).length,
    };
  }, [incidents.data]);

  const recentIncidents = useMemo(
    () => (incidents.data ?? []).slice(0, 6),
    [incidents.data],
  );

  const topPatterns = useMemo(() => {
    const list = patterns.data ?? [];
    return [...list].sort((a, b) => b.count - a.count).slice(0, 5);
  }, [patterns.data]);

  const recentShadow = useMemo(
    () => (shadow.data ?? []).slice(0, 5),
    [shadow.data],
  );

  return (
    <>
      <TopBar
        title="Dashboard"
      />

      <main className="flex-1 overflow-auto p-6">
        {incidents.isError && <ErrorBox error={incidents.error} />}

        {/* Metric tiles — two labeled groups in one row on wide screens */}
        <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
          <div>
            <div className="mb-2 text-2xs font-semibold uppercase tracking-wide text-ink-400">
              Incidents
            </div>
            <div className="grid grid-cols-3 gap-3">
              <Tile
                label="Open"
                value={counts.open}
                tone="bad"
                loading={incidents.isLoading}
                to="/incidents"
                Icon={AlertTriangle}
              />
              <Tile
                label="Acknowledged"
                value={counts.acked}
                tone="accent"
                loading={incidents.isLoading}
                to="/incidents"
                Icon={AlertTriangle}
              />
              <Tile
                label="Resolved"
                value={counts.resolved}
                tone="good"
                loading={incidents.isLoading}
                to="/incidents"
                Icon={AlertTriangle}
              />
            </div>
          </div>

          <div>
            <div className="mb-2 text-2xs font-semibold uppercase tracking-wide text-ink-400">
              AI Agent
            </div>
            <div className="grid grid-cols-3 gap-3">
              <Tile
                label="Patterns"
                value={status.data?.patterns ?? patterns.data?.length}
                loading={status.isLoading && patterns.isLoading}
                to="/patterns"
                Icon={Layers}
              />
              <Tile
                label="Shadow events"
                value={
                  status.data?.shadow_events ?? shadowStats.data?.events ?? 0
                }
                loading={status.isLoading && shadowStats.isLoading}
                to="/shadow"
                Icon={EyeOff}
              />
              <Tile
                label="Services"
                value={
                  services.data ? Object.keys(services.data).length : undefined
                }
                loading={services.isLoading}
                to="/services"
                Icon={Server}
              />
            </div>
          </div>
        </div>

        {/* Two-column body */}
        <section className="mt-6 grid grid-cols-1 gap-4 lg:grid-cols-2">
          {/* Recent incidents */}
          <div className="card">
            <div className="card-header flex items-center justify-between">
              <span className="card-title">Recent incidents</span>
              <Link to="/incidents" className="text-2xs text-accent">
                View all →
              </Link>
            </div>
            <div className="card-body">
              {incidents.isLoading && <Spinner />}
              {!incidents.isLoading && recentIncidents.length === 0 && (
                <div className="py-6 text-center text-xs text-ink-400">
                  No incidents yet.
                </div>
              )}
              {recentIncidents.length > 0 && (
                <table className="ddt">
                  <thead>
                    <tr>
                      <th className="w-32">When</th>
                      <th>Title</th>
                      <th className="w-24">Status</th>
                    </tr>
                  </thead>
                  <tbody>
                    {recentIncidents.map((i) => {
                      const status = i.resolved
                        ? { label: "resolved", tone: "good" as const }
                        : i.acked_at
                          ? { label: "acked", tone: "accent" as const }
                          : { label: "open", tone: "bad" as const };
                      return (
                        <tr key={i.id}>
                          <td className="text-2xs text-ink-500">
                            {fmtRel(i.created_at)}
                          </td>
                          <td>
                            <Link
                              to={`/incidents/${i.id}`}
                              className="text-accent hover:underline"
                            >
                              {truncate(i.title || "(untitled)", 60)}
                            </Link>
                            {i.service && (
                              <span className="ml-2 text-2xs text-ink-400">
                                {i.service}
                              </span>
                            )}
                          </td>
                          <td>
                            <Pill tone={status.tone}>{status.label}</Pill>
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              )}
            </div>
          </div>

          {/* Agent runtime */}
          <div className="card">
            <div className="card-header flex items-center justify-between">
              <span className="card-title">Agent runtime</span>
              <Link to="/status" className="text-2xs text-accent">
                Open status →
              </Link>
            </div>
            <div className="card-body">
              {status.isError && (
                <div className="text-xs text-ink-400">
                  Agent endpoints unreachable. The agent may be disabled
                  (`agent.enable: false`).
                </div>
              )}
              {!status.isError && (
                <AgentChart
                  patterns={status.data?.patterns}
                  shadowEvents={
                    status.data?.shadow_events ??
                    shadowStats.data?.events ??
                    0
                  }
                  totalSignals={shadowStats.data?.total_signals}
                  servicesCount={
                    services.data
                      ? Object.keys(services.data).length
                      : undefined
                  }
                  verdicts={shadowStats.data?.verdicts}
                  loading={status.isLoading && shadowStats.isLoading}
                />
              )}
            </div>
          </div>

          {/* Top patterns */}
          <div className="card">
            <div className="card-header flex items-center justify-between">
              <span className="card-title">Top patterns by sightings</span>
              <Link to="/patterns" className="text-2xs text-accent">
                View all →
              </Link>
            </div>
            <div className="card-body">
              {patterns.isLoading && <Spinner />}
              {patterns.isError && (
                <div className="text-xs text-ink-400">
                  Agent endpoints unreachable.
                </div>
              )}
              {!patterns.isLoading && topPatterns.length === 0 && (
                <div className="py-6 text-center text-xs text-ink-400">
                  No patterns yet.
                </div>
              )}
              {topPatterns.length > 0 && (
                <table className="ddt">
                  <thead>
                    <tr>
                      <th className="w-20 text-right">Count</th>
                      <th className="w-24">Verdict</th>
                      <th>Template</th>
                    </tr>
                  </thead>
                  <tbody>
                    {topPatterns.map((p) => (
                      <tr key={p.id}>
                        <td className="text-right tabular-nums">{p.count}</td>
                        <td>
                          <VerdictPill verdict={p.verdict} />
                        </td>
                        <td>
                          <Link
                            to={`/patterns/${p.id}`}
                            className="font-mono text-2xs text-accent hover:underline"
                          >
                            {truncate(p.template, 80)}
                          </Link>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </div>
          </div>

          {/* Recent shadow */}
          <div className="card">
            <div className="card-header flex items-center justify-between">
              <span className="card-title">Recent shadow events</span>
              <Link to="/shadow" className="text-2xs text-accent">
                View all →
              </Link>
            </div>
            <div className="card-body">
              {shadow.isLoading && <Spinner />}
              {shadow.isError && (
                <div className="text-xs text-ink-400">
                  Agent endpoints unreachable.
                </div>
              )}
              {!shadow.isLoading && recentShadow.length === 0 && (
                <div className="py-6 text-center text-xs text-ink-400">
                  No shadow events recorded.
                </div>
              )}
              {recentShadow.length > 0 && (
                <table className="ddt">
                  <thead>
                    <tr>
                      <th className="w-24">Verdict</th>
                      <th>Sample</th>
                      <th className="w-24">Last seen</th>
                    </tr>
                  </thead>
                  <tbody>
                    {recentShadow.map((e) => (
                      <tr key={`${e.pattern_id}-${e.first_seen}`}>
                        <td>
                          <VerdictPill verdict={e.verdict} />
                        </td>
                        <td>
                          <Link
                            to={`/shadow/${encodeURIComponent(e.pattern_id)}`}
                            className="font-mono text-2xs text-ink-700 hover:text-accent hover:underline"
                          >
                            {truncate(e.sample_message, 80)}
                          </Link>
                        </td>
                        <td className="text-2xs text-ink-500">
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

function Tile({
  label,
  value,
  loading,
  tone,
  to,
  Icon,
}: {
  label: string;
  value: number | string | undefined;
  loading?: boolean;
  tone?: "good" | "bad" | "accent";
  to: string;
  Icon: LucideIcon;
}) {
  const accent =
    tone === "bad"
      ? "text-rose-600"
      : tone === "good"
        ? "text-emerald-600"
        : tone === "accent"
          ? "text-accent"
          : "text-ink-700";
  return (
    <Link to={to} className="stat-card hover:border-accent">
      <div className="flex items-center justify-between">
        <div className="stat-label">{label}</div>
        <Icon size={14} className="text-ink-400" />
      </div>
      <div className={"stat-value tabular-nums " + accent}>
        {loading ? <Spinner /> : (value ?? "—")}
      </div>
    </Link>
  );
}

const VERDICT_STROKE: Record<string, string> = {
  unknown: "#f59e0b",
  spike: "#ef4444",
  known: "#10b981",
};

const BAR_COLORS = ["#6366f1", "#f59e0b", "#0ea5e9", "#8b5cf6"];

function AgentChart({
  patterns,
  shadowEvents,
  totalSignals,
  servicesCount,
  verdicts,
  loading,
}: {
  patterns?: number;
  shadowEvents?: number;
  totalSignals?: number;
  servicesCount?: number;
  verdicts?: Record<string, number>;
  loading?: boolean;
}) {
  if (loading) return <Spinner />;

  const bars = [
    { label: "Patterns", value: patterns ?? 0, color: BAR_COLORS[0] },
    { label: "Shadow", value: shadowEvents ?? 0, color: BAR_COLORS[1] },
    { label: "Signals", value: totalSignals ?? 0, color: BAR_COLORS[2] },
    { label: "Services", value: servicesCount ?? 0, color: BAR_COLORS[3] },
  ];

  const verdictEntries = Object.entries(verdicts ?? {}).sort(
    ([, a], [, b]) => b - a,
  );
  const verdictTotal = verdictEntries.reduce((s, [, v]) => s + v, 0) || 1;

  const W = 320;
  const H = 120;
  const PX = 40;
  const PR = 12;
  const PT = 16;
  const PB = 20;
  const chartW = W - PX - PR;
  const chartH = H - PT - PB;
  const max = Math.max(...bars.map((b) => b.value), 1);

  const barCount = bars.length;
  const gap = 12;
  const barW = (chartW - gap * (barCount - 1)) / barCount;

  const gridCount = 3;
  const gridLines = Array.from({ length: gridCount + 1 }, (_, i) => {
    const y = PT + (i / gridCount) * chartH;
    const val = Math.round(max - (i / gridCount) * max);
    return { y, val };
  });

  return (
    <div className="space-y-4">
      <svg viewBox={`0 0 ${W} ${H}`} className="w-full" preserveAspectRatio="xMidYMid meet">
        {/* Grid lines + y-axis labels */}
        {gridLines.map((g) => (
          <g key={g.y}>
            <line
              x1={PX}
              y1={g.y}
              x2={W - PR}
              y2={g.y}
              stroke="currentColor"
              className="text-ink-100"
              strokeWidth={0.5}
            />
            <text
              x={PX - 4}
              y={g.y + 3}
              textAnchor="end"
              className="fill-ink-400"
              fontSize={7}
            >
              {g.val.toLocaleString()}
            </text>
          </g>
        ))}

        {/* Bars */}
        {bars.map((b, i) => {
          const x = PX + i * (barW + gap);
          const barH = (b.value / max) * chartH;
          const y = PT + chartH - barH;
          return (
            <g key={b.label}>
              <rect
                x={x}
                y={y}
                width={barW}
                height={barH}
                rx={3}
                fill={b.color}
                opacity={0.85}
              />
              {/* Value above bar */}
              <text
                x={x + barW / 2}
                y={y - 4}
                textAnchor="middle"
                className="fill-ink-700"
                fontSize={7}
                fontWeight={600}
              >
                {b.value.toLocaleString()}
              </text>
              {/* Label below bar */}
              <text
                x={x + barW / 2}
                y={H - 4}
                textAnchor="middle"
                className="fill-ink-500"
                fontSize={7}
              >
                {b.label}
              </text>
            </g>
          );
        })}
      </svg>

      {/* Verdict breakdown stacked bar */}
      {verdictEntries.length > 0 && (
        <div>
          <div className="mb-1 text-2xs font-medium uppercase tracking-wider text-ink-400">
            Verdicts
          </div>
          <div className="flex h-4 w-full overflow-hidden rounded">
            {verdictEntries.map(([v, count]) => (
              <div
                key={v}
                className="transition-all"
                style={{
                  width: `${(count / verdictTotal) * 100}%`,
                  backgroundColor: VERDICT_STROKE[v] ?? "#94a3b8",
                }}
                title={`${v}: ${count}`}
              />
            ))}
          </div>
          <div className="mt-1.5 flex flex-wrap gap-3 text-2xs text-ink-500">
            {verdictEntries.map(([v, count]) => (
              <span key={v} className="flex items-center gap-1">
                <span
                  className="inline-block h-2 w-2 rounded-full"
                  style={{ backgroundColor: VERDICT_STROKE[v] ?? "#94a3b8" }}
                />
                {v}: {count}
              </span>
            ))}
          </div>
        </div>
      )}
    </div>
  );
}
