import { useMemo } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import {
  AlertTriangle,
  ArrowUpRight,
  BellOff,
  BellRing,
  Bot,
  Check,
  CheckCircle2,
  EyeOff,
  LineChart,
  Lock,
  RefreshCw,
  ScrollText,
  Sparkles,
  Waypoints,
  X,
} from "lucide-react";
import {
  api,
  ApiError,
  type AgentConfigView,
  type IncidentSummary,
  type OriginCounts,
  type Status,
} from "@/lib/api";
import {
  fmtAbs,
  fmtRel,
  hourlyBuckets,
  incidentTitle,
  truncate,
} from "@/lib/format";
import { TopBar } from "@/components/TopBar";
import { Pill } from "@/components/Pill";
import { SeverityBadge } from "@/components/SeverityBadge";
import { KpiTile } from "@/components/KpiTile";
import { ChannelIcon } from "@/components/ChannelIcon";
import { ClickableRow } from "@/components/DataTable";
import { SegmentedControl } from "@/components/SegmentedControl";
import { FilterBar } from "@/components/FilterBar";
import { useNowTick, useTableKeys } from "@/lib/hooks";
import {
  matchesOrigin,
  normalizeOrigin,
  originLabel,
} from "@/lib/incidentList";
import { SkRows } from "@/components/Skeleton";
import { RetryableError } from "@/components/RetryableError";
import { EmptyState } from "@/components/feedback";
import { countWindowLabel } from "@/lib/countWindow";

// NowPage — the primary "what's on fire / how bad" surface (UX_REDESIGN
// §2.3a). Top→bottom: open-incident banner (or a quiet "Nothing open"
// strip), KPI tiles that deep-link with their filter, then the latest-10
// incident feed beside the Agent Pulse card. Severity renders "—" until
// the list endpoint carries it (backend ask #1); Ack stays a status, not
// a button, until ask #3; Agent Pulse shows lifetime totals only until
// windowed stats exist (ask #6).
export function NowPage() {
  const navigate = useNavigate();
  const [params] = useSearchParams();
  // Origin tab: AI-detected is the high-signal DEFAULT so a webhook/alert
  // flood can never dominate the live view; webhook is one click away.
  // The URL param IS the state (shareable / restorable), mirroring the
  // Incidents page.
  const origin = normalizeOrigin(params.get("origin"));

  // Shares ["incidents","list"] with the feed elsewhere — the loaded rows back
  // the latest-10 feed and the 24h trend sparklines below. Every NUMBER on the
  // page, though, comes from the server counts query (below), never this
  // paginated collection. 15s auto-refresh, paused while the tab is hidden.
  const incidents = useQuery({
    queryKey: ["incidents", "list"],
    queryFn: () => api.listIncidents(),
    refetchInterval: () => (document.hidden ? false : 15_000),
    staleTime: 15_000,
  });
  // The authoritative per-origin × per-status count — one cheap, rows-free
  // request shared with the header badge (useOpenIncidentCount, same key). The
  // KPI tiles, the origin-tab badges and the open-banner count all read this,
  // so the Now page, the header badge and the Incidents page never disagree.
  const countsQ = useQuery({
    queryKey: ["incidents", "counts"],
    queryFn: () => api.incidentCounts(),
    refetchInterval: () => (document.hidden ? false : 15_000),
    staleTime: 15_000,
  });
  const byStatus = countsQ.data?.by_status;
  const countWindow = countsQ.data
    ? countWindowLabel(countsQ.data.count_window, "short")
    : undefined;
  // Same keys as TopBar's chip queries — one cache entry, zero extra load.
  const config = useQuery({
    queryKey: ["agent-config"],
    queryFn: api.getAgentConfig,
    staleTime: 60_000,
    retry: 1,
  });
  // Skip the agent liveness poll when the agent is disabled — the status
  // route is unmounted in that case, so polling it would 404 every 30s.
  const status = useQuery({
    queryKey: ["status-pulse"],
    queryFn: api.status,
    enabled: config.data?.enable !== false,
    refetchInterval: () => (document.hidden ? false : 30_000),
    retry: 1,
  });

  const sorted = useMemo(() => {
    const list = [...(incidents.data ?? [])];
    list.sort(
      (a, b) =>
        new Date(b.created_at).getTime() - new Date(a.created_at).getTime(),
    );
    return list;
  }, [incidents.data]);

  // OPEN per-origin counts from the server drive the origin-tab badges and
  // the top-bar summary. Now is the live "what needs attention" view, so a
  // lifetime total beside the open banner and the Open KPI tile read as noise
  // and contradicted them. Both feeds still show their own number, so the
  // webhook count never lumps into AI regardless of the active tab.
  const openOriginCounts = byStatus?.open;

  // The active tab scopes the live view (banner PREVIEW rows, feed, trends) to
  // one origin. Rows are split client-side (they share the list cache); the
  // COUNTS are read from the server breakdown for the active origin below.
  const scoped = useMemo(
    () => sorted.filter((i) => matchesOrigin(i, origin)),
    [sorted, origin],
  );

  // Loaded open rows for the banner PREVIEW list only — the open COUNT shown is
  // the server number (counts.open), which may exceed these loaded rows.
  const openIncidents = useMemo(
    () => scoped.filter((i) => !i.resolved && !i.acked_at),
    [scoped],
  );
  // KPI + banner numbers for the active origin, straight from the server's
  // per-origin × per-status breakdown — never a tally of the loaded page.
  const counts = useMemo(() => {
    const pick = (c?: OriginCounts) =>
      origin === "webhook" ? c?.webhook ?? 0 : c?.ai_detect ?? 0;
    return {
      open: pick(byStatus?.open),
      acked: pick(byStatus?.acked),
      resolved: pick(byStatus?.resolved),
    };
  }, [byStatus, origin]);
  // Most recently resolved incident — context for the all-clear banner.
  const lastResolved = useMemo(
    () =>
      scoped
        .filter((i) => i.resolved && i.resolved_at)
        .sort(
          (a, b) =>
            new Date(b.resolved_at!).getTime() -
            new Date(a.resolved_at!).getTime(),
        )[0],
    [scoped],
  );

  // 24h hourly buckets from real incident timestamps — the only windowed
  // series the API already carries (created/acked/resolved stamps). The
  // sums double as honest "· 24h" tile footers. nowTick keeps the window
  // sliding on quiet dashboards where `scoped` never changes identity.
  const nowTick = useNowTick();
  const trends = useMemo(() => {
    const sum = (b: number[]) => b.reduce((a, n) => a + n, 0);
    const created = hourlyBuckets(scoped.map((i) => i.created_at), 24, nowTick);
    const acked = hourlyBuckets(scoped.map((i) => i.acked_at), 24, nowTick);
    const resolved = hourlyBuckets(
      scoped.map((i) => i.resolved_at),
      24,
      nowTick,
    );
    return {
      created,
      acked,
      resolved,
      created24: sum(created),
      acked24: sum(acked),
      resolved24: sum(resolved),
    };
  }, [scoped, nowTick]);

  const feed = scoped.slice(0, 10);
  const keys = useTableKeys({
    size: feed.length,
    onOpen: (i) => navigate(`/incidents/${feed[i].id}`),
  });

  const refreshing =
    incidents.isFetching ||
    countsQ.isFetching ||
    status.isFetching ||
    config.isFetching;
  const refreshAll = () => {
    incidents.refetch();
    countsQ.refetch();
    status.refetch();
    config.refetch();
  };

  // Carry the active origin into every /incidents deep-link so a KPI tile
  // or "view all" click lands on the SAME tab the operator is viewing.
  // ai_detect is the Incidents-page default, so only webhook needs the
  // explicit param.
  const withOrigin = (path: string) => {
    if (origin !== "webhook") return path;
    return path.includes("?")
      ? `${path}&origin=webhook`
      : `${path}?origin=webhook`;
  };

  const agentMode = config.data?.mode;
  const agentValue = config.data
    ? config.data.enable === false
      ? "off"
      : agentMode || "—"
    : undefined;
  const agentTone =
    config.data && config.data.enable !== false
      ? agentMode === "detect"
        ? ("ok" as const)
        : agentMode === "shadow"
          ? ("warn" as const)
          : ("info" as const)
      : undefined;

  return (
    <>
      <TopBar
        title="Now"
        subtitle={`auto-refresh 15s${countWindow ? ` · counts over ${countWindow}` : ""}`}
        showCountWindow={false}
        actions={
          <button
            aria-label="Refresh now"
            title="Refresh"
            className="rounded-control p-1.5 text-ink-300 hover:bg-ink-700 hover:text-ink-100"
            onClick={refreshAll}
          >
            <RefreshCw
              size={15}
              className={refreshing ? "animate-spin" : undefined}
            />
          </button>
        }
      />

      <main className="flex-1 space-y-4 overflow-auto p-4 lg:p-6">
        {/* Origin split — the primary filter for the live view. AI-detected
            (default) vs the inbound webhook/alert firehose, each badged with
            its OPEN count so the tabs agree with the banner and the Open KPI
            tile, and neither feed buries the other. Scopes the banner, KPI
            counts and feed below to the active tab. */}
        <FilterBar
          tabs={
            <SegmentedControl
              param="origin"
              defaultValue="ai_detect"
              aria-label="Filter the Now view by origin — badges show open incidents"
              options={[
                {
                  value: "ai_detect",
                  label: originLabel("ai_detect"),
                  badge: openOriginCounts?.ai_detect,
                },
                {
                  value: "webhook",
                  label: originLabel("webhook"),
                  badge: openOriginCounts?.webhook,
                },
              ]}
            />
          }
        />

        {/* (1) Open-incident banner — the OPEN count is the server number;
            the preview rows are the loaded page. Recency-sorted until backend
            ask #1 ships severity on summaries. */}
        {(incidents.isPending || countsQ.isPending) && (
          <div aria-hidden className="sk h-14 rounded-card" />
        )}
        {(incidents.isError || countsQ.isError) && (
          <RetryableError
            context="Couldn't load incidents"
            error={incidents.error ?? countsQ.error}
            onRetry={() => {
              incidents.refetch();
              countsQ.refetch();
            }}
            retrying={incidents.isFetching || countsQ.isFetching}
          />
        )}
        {byStatus && counts.open === 0 && (
          <div className="flex items-center gap-3 rounded-card border border-sev-ok/30 bg-sev-ok/10 px-4 py-3">
            <CheckCircle2 size={18} className="shrink-0 text-sev-ok" aria-hidden />
            <div className="min-w-0">
              {/* ink-50, not sev-ok: the green text on the light tint
                  measured 3.90:1 (fails AA at 14px). The icon, border and
                  tint carry the tone; the words carry the meaning. */}
              <div className="text-sm font-semibold text-ink-50">
                All clear — nothing open
              </div>
              {lastResolved && (
                <div className="truncate text-2xs text-ink-300">
                  Last incident resolved{" "}
                  <span title={fmtAbs(lastResolved.resolved_at!)}>
                    {fmtRel(lastResolved.resolved_at!)}
                  </span>
                  {" · "}
                  {truncate(incidentTitle(lastResolved), 60)}
                </div>
              )}
            </div>
          </div>
        )}
        {byStatus && counts.open > 0 && (
          <section aria-label="Open incidents" className="card overflow-hidden">
            <div className="card-header py-2">
              <span className="inline-flex items-center gap-2 text-xs font-semibold text-sev-critical">
                <AlertTriangle size={13} aria-hidden />
                {counts.open} open incident{counts.open > 1 ? "s" : ""}
              </span>
              <Link
                to={withOrigin("/incidents")}
                className="text-2xs font-medium text-link hover:underline"
              >
                view all →
              </Link>
            </div>
            <ul className="divide-y divide-ink-500/30">
              {openIncidents.slice(0, 5).map((i) => (
                <li key={i.id}>
                  <Link
                    to={`/incidents/${i.id}`}
                    className="flex min-h-11 items-center gap-3 border-l-2 border-l-sev-critical-solid px-4 py-2 transition-colors hover:bg-ink-600/30"
                  >
                    <div className="min-w-0 flex-1">
                      <span className="truncate text-xs font-medium text-ink-50">
                        {truncate(incidentTitle(i), 96)}
                      </span>
                      <span className="ml-2 text-2xs text-ink-300">
                        {i.service ? `${i.service} · ` : ""}
                        <span title={fmtAbs(i.created_at)}>
                          {fmtRel(i.created_at)}
                        </span>
                      </span>
                    </div>
                    {/* Ack status only — no Ack button until backend ask #3. */}
                    <span className="inline-flex shrink-0 items-center gap-1 text-2xs text-ink-300">
                      <BellOff size={11} aria-hidden />
                      not acked
                      {i.assigned_team_id || i.assigned_member_ids?.length ? (
                        <span className="text-ink-200"> · {assignText(i)}</span>
                      ) : null}
                    </span>
                  </Link>
                </li>
              ))}
            </ul>
          </section>
        )}

        {/* (2) KPI row — each tile deep-links WITH its filter; each tile's
            loading flag covers exactly its own queries (no false zeros). */}
        <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
          <KpiTile
            label="Open"
            value={byStatus ? counts.open : undefined}
            loading={countsQ.isPending}
            to={withOrigin("/incidents")}
            icon={AlertTriangle}
            tone={
              byStatus
                ? counts.open > 0
                  ? "critical"
                  : "ok"
                : undefined
            }
            spark={incidents.data ? trends.created : undefined}
            sparkLabel={`${trends.created24} incidents created in the last 24 hours`}
            foot={
              incidents.data ? `${trends.created24} created · 24h` : undefined
            }
          />
          <KpiTile
            label="Acked"
            value={byStatus ? counts.acked : undefined}
            loading={countsQ.isPending}
            to={withOrigin("/incidents?status=acked")}
            icon={BellRing}
            tone={byStatus && counts.acked > 0 ? "warn" : undefined}
            spark={incidents.data ? trends.acked : undefined}
            sparkLabel={`${trends.acked24} incidents acknowledged in the last 24 hours`}
            foot={incidents.data ? `${trends.acked24} acked · 24h` : undefined}
          />
          <KpiTile
            label="Resolved"
            value={byStatus ? counts.resolved : undefined}
            loading={countsQ.isPending}
            to={withOrigin("/incidents?status=resolved")}
            icon={CheckCircle2}
            spark={incidents.data ? trends.resolved : undefined}
            sparkLabel={`${trends.resolved24} incidents resolved in the last 24 hours`}
            foot={
              incidents.data ? `${trends.resolved24} resolved · 24h` : undefined
            }
          />
          <KpiTile
            label="Agent"
            value={agentValue}
            loading={config.isPending || status.isPending}
            to="/agent"
            icon={Bot}
            tone={agentTone}
            foot={
              status.data ? `${status.data.patterns} log patterns` : undefined
            }
          />
        </div>

        {/* (3) Incident feed — full width */}
        <section>
          <div className="card">
            <div className="card-header">
              <span className="card-title">Incident feed</span>
              <Link
                to={withOrigin("/incidents")}
                className="text-2xs text-link hover:underline"
              >
                View all →
              </Link>
            </div>
            <div
              role="region"
              aria-label="Incident feed — j/k to move, Enter to open"
              className="overflow-x-auto"
              {...keys.containerProps}
            >
              <table className="ddt">
                <thead>
                  <tr>
                    <th className="w-28">Service</th>
                    <th className="w-12">Severity</th>
                    <th className="w-24">When</th>
                    <th>Title</th>
                    <th className="w-28">Notify</th>
                    <th className="w-24">State</th>
                  </tr>
                </thead>
                <tbody>
                  {incidents.isPending && <SkRows rows={6} cols={6} />}
                  {incidents.isSuccess &&
                    feed.map((i, idx) => (
                      <ClickableRow
                        key={i.id}
                        to={`/incidents/${i.id}`}
                        {...keys.rowProps(idx)}
                      >
                        <td className="text-2xs text-ink-200">
                          {i.service || "—"}
                        </td>
                        <td>
                          <SeverityBadge severity={null} />
                        </td>
                        <td
                          className="whitespace-nowrap text-2xs text-ink-300"
                          title={fmtAbs(i.created_at)}
                        >
                          {fmtRel(i.created_at)}
                        </td>
                        <td>
                          <span className="text-ink-50">
                            {truncate(incidentTitle(i), 70)}
                          </span>
                          {/* Failure reason inline, never tooltip-only. */}
                          {i.notify_status === "failed" && i.notify_error && (
                            <div className="mt-0.5 text-2xs text-sev-critical">
                              ✗ {truncate(i.notify_error, 90)}
                            </div>
                          )}
                        </td>
                        <td>
                          <NotifyCell incident={i} />
                        </td>
                        <td>
                          <StatePill incident={i} />
                        </td>
                      </ClickableRow>
                    ))}
                </tbody>
              </table>
              {incidents.isError && (
                <div className="p-4 text-xs text-ink-300">
                  Incident list unavailable — use Retry above.
                </div>
              )}
              {incidents.isSuccess && feed.length === 0 && (
                <EmptyState
                  title="No incidents yet"
                  hint="Once an alert fires it will appear here."
                />
              )}
            </div>
          </div>
        </section>

        {/* (4) Agent Pulse — own row below the feed */}
        <AgentPulse status={status} config={config} />
      </main>
    </>
  );
}

function assignText(i: IncidentSummary): string {
  const parts: string[] = [];
  if (i.assigned_team_id) parts.push(`team ${i.assigned_team_id}`);
  const n = i.assigned_member_ids?.length ?? 0;
  if (n > 0) parts.push(`${n} assignee${n === 1 ? "" : "s"}`);
  return parts.length ? parts.join(" · ") : "unassigned";
}

// NotifyCell — per-channel icon plus a ✓/✗ outcome. The API exposes one
// notify_status per incident (not per channel), so the glyph + count is
// the honest rendering; the failure reason itself lives inline under the
// title (never tooltip-only).
function NotifyCell({ incident }: { incident: IncidentSummary }) {
  const channels = incident.channels_notified ?? [];
  const st = incident.notify_status;
  if (channels.length === 0 && !st) {
    return <span className="text-2xs text-ink-300">—</span>;
  }
  return (
    <div className="flex items-center gap-1">
      {channels.slice(0, 3).map((c) => (
        <ChannelIcon key={c} id={c} size={11} />
      ))}
      {channels.length > 3 && (
        <span className="text-2xs text-ink-300">+{channels.length - 3}</span>
      )}
      {st === "sent" && (
        <span className="inline-flex items-center gap-0.5 text-2xs font-medium text-sev-ok">
          <Check size={11} aria-hidden />
          {channels.length > 0 ? channels.length : "sent"}
        </span>
      )}
      {st === "failed" && (
        <span className="inline-flex items-center gap-0.5 text-2xs font-medium text-sev-critical">
          <X size={11} aria-hidden />
          failed
        </span>
      )}
      {st === "pending" && (
        <span className="text-2xs text-ink-300">pending…</span>
      )}
    </div>
  );
}

function StatePill({ incident }: { incident: IncidentSummary }) {
  if (incident.resolved) return <Pill tone="good">resolved</Pill>;
  if (incident.acked_at) return <Pill tone="accent">acked</Pill>;
  return <Pill tone="bad">open</Pill>;
}

function isAuthorizationFailure(error: unknown): boolean {
  return error instanceof ApiError && [401, 403, 404].includes(error.status);
}

// AgentPulse — current signal counts with links to their detail views.
function AgentPulse({
  status,
  config,
}: {
  status: UseQueryResult<Status>;
  config: UseQueryResult<AgentConfigView>;
}) {
  // Probe baselines to show Metrics/Traces learning progress (enterprise-only).
  const baselines = useQuery({
    queryKey: ["baselines-pulse"],
    enabled: config.data?.enable === true,
    queryFn: async () => {
      try {
        return await api.listBaselines();
      } catch (e) {
        if (e instanceof ApiError && (e.status === 403 || e.status === 404)) {
          return null; // locked — OSS or no intelligence license
        }
        throw e;
      }
    },
    staleTime: 30_000,
    retry: 1,
  });
  const enterpriseAvailable = baselines.data != null && !(baselines.isError && isAuthorizationFailure(baselines.error));
  const metricCount = baselines.data?.baselines?.filter((b) => b.type === "metric").length ?? 0;
  const traceCount = baselines.data?.baselines?.filter((b) => b.type === "trace").length ?? 0;
  const metricReady = baselines.data?.baselines?.filter((b) => b.type === "metric" && b.confident).length ?? 0;
  const traceReady = baselines.data?.baselines?.filter((b) => b.type === "trace" && b.confident).length ?? 0;
  const agentOn = config.data?.enable === true;

  return (
    <section aria-labelledby="agent-pulse-title" className="border-y border-ink-600 bg-surface">
      <div className="flex flex-wrap items-center justify-between gap-3 border-b border-ink-600/50 px-4 py-3">
        <div className="flex items-center gap-2">
          <Bot size={16} className="text-ink-300" aria-hidden />
          <h2 id="agent-pulse-title" className="text-sm font-semibold text-ink-100">Agent pulse</h2>
        </div>
        <Link to="/agent" className="inline-flex min-h-8 items-center gap-1 text-xs text-link hover:underline">
          Agent overview <ArrowUpRight size={13} aria-hidden />
        </Link>
      </div>
      <div className="min-w-0 p-4 sm:p-5">
        <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
          <h3 className="text-2xs font-medium uppercase tracking-wide text-ink-400">Signal activity</h3>
          <span className="text-2xs text-ink-400">Current catalog · recorded events</span>
        </div>
        {config.isPending && <p role="status" className="py-6 text-sm text-ink-400">Awaiting runtime configuration</p>}
        {config.isError && (
          <RetryableError
            context="Couldn't load agent config"
            error={config.error}
            onRetry={() => config.refetch()}
            retrying={config.isFetching}
          />
        )}
        {config.isSuccess && !agentOn && <p className="py-6 text-sm text-ink-400">Agent is disabled</p>}
        {agentOn && status.isPending && (
          <div aria-hidden className={`grid gap-px overflow-hidden border border-ink-600/60 ${enterpriseAvailable ? "grid-cols-1 lg:grid-cols-5" : "grid-cols-3"}`}>
            {enterpriseAvailable ? (
              <>
                <div className="grid grid-cols-3 gap-px lg:col-span-3">
                  {Array.from({ length: 3 }, (_, index) => <div key={index} className="sk min-h-24 bg-surface" />)}
                </div>
                <div className="grid grid-cols-2 gap-px self-start lg:col-span-2 lg:self-stretch">
                  {Array.from({ length: 2 }, (_, index) => <div key={index} className="sk min-h-24 bg-surface" />)}
                </div>
              </>
            ) : (
              Array.from({ length: 3 }, (_, index) => <div key={index} className="sk min-h-24 bg-surface" />)
            )}
          </div>
        )}
        {agentOn && status.isError && (
          <RetryableError
            context="Couldn't load agent status"
            error={status.error}
            onRetry={() => status.refetch()}
            retrying={status.isFetching}
          />
        )}

        {agentOn && status.data !== undefined && !(status.isError && isAuthorizationFailure(status.error)) && (
          <div className={`grid gap-px overflow-hidden border border-ink-600/60 ${enterpriseAvailable ? "grid-cols-1 lg:grid-cols-5" : "grid-cols-3"}`}>
            {enterpriseAvailable ? (
              <>
                <div className="grid grid-cols-3 gap-px lg:col-span-3">
                  <PulseSignalStat
                    icon={ScrollText}
                    label="Logs"
                    total={status.data.patterns}
                    unit="patterns"
                    to="/agent/logs"
                  />
                  <PulseSignalStat icon={LineChart} label="Metrics" total={metricCount} ready={metricReady} to="/agent/metrics" />
                  <PulseSignalStat icon={Waypoints} label="Traces" total={traceCount} ready={traceReady} to="/agent/traces" />
                </div>
                <div className="grid grid-cols-2 gap-px self-start lg:col-span-2 lg:self-stretch">
                  <PulseSignalStat
                    icon={EyeOff}
                    label="Shadow"
                    total={status.data.shadow_events ?? 0}
                    unit="events"
                    to="/agent/decisions?tab=shadow"
                    decision
                  />
                  <PulseSignalStat
                    icon={Sparkles}
                    label="Detect"
                    total={status.data.detect_events ?? 0}
                    unit="events"
                    to="/agent/decisions?tab=detect"
                    decision
                  />
                </div>
              </>
            ) : (
              <>
              <PulseSignalStat
                icon={ScrollText}
                label="Logs"
                total={status.data.patterns}
                unit="patterns"
                to="/agent/logs"
              />
              {enterpriseAvailable && <PulseSignalStat
                icon={LineChart}
                label="Metrics"
                total={metricCount}
                ready={metricReady}
                to="/agent/metrics"
              />}
              {enterpriseAvailable && <PulseSignalStat
                icon={Waypoints}
                label="Traces"
                total={traceCount}
                ready={traceReady}
                to="/agent/traces"
              />}
              <PulseSignalStat
                icon={EyeOff}
                label="Shadow"
                total={status.data.shadow_events ?? 0}
                unit="events"
                to="/agent/decisions?tab=shadow"
                decision
              />
              <PulseSignalStat
                icon={Sparkles}
                label="Detect"
                total={status.data.detect_events ?? 0}
                unit="events"
                to="/agent/decisions?tab=detect"
                decision
              />
              </>
            )}
          </div>
        )}
        {agentOn && baselines.isPending && <p role="status" className="pt-3 text-xs text-ink-400">Loading metric and trace activity</p>}
        {agentOn && baselines.isError && (
          <RetryableError context="Couldn't load metric and trace activity" error={baselines.error} onRetry={() => baselines.refetch()} retrying={baselines.isFetching} />
        )}
        {agentOn && baselines.isSuccess && baselines.data === null && (
            <div className="mt-3 flex items-center gap-2 border-t border-ink-600/50 pt-3 text-2xs text-ink-400">
              <Lock size={12} className="shrink-0" />
              <span>
                Metrics &amp; Traces learning requires an{" "}
                <Link to="/agent/metrics" className="text-link hover:underline">Enterprise license</Link>.
              </span>
            </div>
        )}
      </div>
    </section>
  );
}

function PulseSignalStat({
  icon: Icon,
  label,
  total,
  ready,
  unit = "signals",
  to,
  decision = false,
}: {
  icon: typeof LineChart;
  label: string;
  total: number;
  ready?: number;
  unit?: string;
  to: string;
  decision?: boolean;
}) {
  return (
    <Link
      to={to}
      className={`group flex min-h-24 min-w-0 flex-col justify-between gap-2 bg-surface p-3 transition-colors hover:bg-surface-raised focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-link ${decision ? "bg-surface-raised/40" : ""}`}
    >
      <div className="flex min-w-0 items-center justify-between gap-2">
        <span className="flex min-w-0 items-center gap-2 text-xs font-medium text-ink-200">
          <Icon size={14} className={`shrink-0 ${decision ? "text-ink-300" : "text-ink-400"}`} aria-hidden />
          <span className="truncate">{label}</span>
        </span>
        <ArrowUpRight size={13} className="hidden shrink-0 text-ink-400 group-hover:text-link sm:block" aria-hidden />
      </div>
      <div className="min-w-0">
        <div className="flex flex-wrap items-baseline gap-x-1.5">
          <span className="text-xl font-semibold tabular-nums text-ink-50">{total.toLocaleString()}</span>
          <span className="text-2xs text-ink-400">{unit}</span>
        </div>
        {ready !== undefined && total > 0 && (
          <span className="block text-2xs text-ink-400">{ready.toLocaleString()} ready</span>
        )}
      </div>
    </Link>
  );
}
