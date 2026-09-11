import { Link, useSearchParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { Activity, ArrowUpRight, Layers, Radio } from "lucide-react";
import { api, ApiError } from "@/lib/api";
import { fmtAbs, fmtRel } from "@/lib/format";
import { useIsMobileViewport } from "@/lib/hooks";
import { TopBar } from "@/components/TopBar";
import { RetryableError } from "@/components/RetryableError";
import { ServiceHealthSection } from "@/components/ServiceHealthSection";
import "./AgentOverviewPage.css";

const views = [
  { id: "services", label: "Services", icon: Radio },
  { id: "activity", label: "Activity", icon: Activity },
  { id: "learning", label: "Learning", icon: Layers },
] as const;

export function AgentOverviewPage() {
  const [params, setParams] = useSearchParams();
  const isMobile = useIsMobileViewport();
  const view = views.some((item) => item.id === params.get("view")) ? params.get("view")! : "services";

  return <>
    <TopBar title="Agent Overview" />
    <main className="agent-overview flex-1 overflow-auto">
      {isMobile && <div className="agent-overview-navigation">
        <nav className="agent-overview-tabs" aria-label="Overview views">
          {views.map(({ id, label, icon: Icon }) => <button key={id} type="button" aria-current={view === id ? "page" : undefined}
            onClick={() => setParams((previous) => { const next = new URLSearchParams(previous); if (id === "services") next.delete("view"); else next.set("view", id); return next; })}>
            <Icon size={15} aria-hidden />{label}
          </button>)}
        </nav>
      </div>}
      {isMobile ? <div className="agent-overview-content">
        {view === "services" && <ServiceHealthSection />}
        {view === "activity" && <AgentActivity />}
        {view === "learning" && <AgentLearning />}
      </div> : <div className="agent-overview-sections">
        <ServiceHealthSection />
        <AgentActivity />
        <AgentLearning />
      </div>}
    </main>
  </>;
}

function recordedNumber(value: number | undefined, failed = false) {
  return failed ? "Unavailable" : value == null ? "Loading" : value.toLocaleString();
}

function AgentActivity() {
  const detect = useQuery({ queryKey: ["detect-stats"], queryFn: api.detectStats });
  const shadowStats = useQuery({ queryKey: ["shadow-stats"], queryFn: api.shadowStats });
  const shadow = useQuery({ queryKey: ["shadow"], queryFn: api.listShadow });
  const counters = detect.data;
  const groups = (prefix: string) => Object.entries(counters ?? {})
    .filter(([key]) => key.startsWith(prefix))
    .map(([key, value]) => [decisionLabel(key.slice(prefix.length)), value] as [string, number])
    .sort((left, right) => right[1] - left[1]);
  const totals = [
    { label: "Incidents created", value: counters ? counters.outcome_emitted ?? 0 : undefined, failed: detect.isError },
    { label: "Analysis errors", value: counters ? (counters.outcome_ai_error ?? 0) + (counters.outcome_send_error ?? 0) : undefined, failed: detect.isError },
    { label: "Reused analyses", value: counters ? counters.outcome_cached ?? 0 : undefined, failed: detect.isError },
    { label: "Preview events", value: shadowStats.data?.events, failed: shadowStats.isError },
  ];
  return <section aria-labelledby="agent-activity-title">
    <div className="overview-section-heading"><h2 id="agent-activity-title"><Activity size={17} aria-hidden />Agent Activity</h2><Link to="/agent/decisions?tab=detect">All decisions <ArrowUpRight size={13} aria-hidden /></Link></div>
    <p className="mb-4 text-xs text-ink-400">Recorded totals</p>
    <dl className="overview-stat-strip">{totals.map((item) => <div key={item.label}><dt>{item.label}</dt><dd>{recordedNumber(item.value, item.failed)}</dd></div>)}</dl>
    {detect.isError && <RetryableError error={detect.error} onRetry={() => detect.refetch()} retrying={detect.isFetching} context="Couldn't load decisions" />}
    {shadowStats.isError && <RetryableError error={shadowStats.error} onRetry={() => shadowStats.refetch()} retrying={shadowStats.isFetching} context="Couldn't load preview totals" />}
    <div className="overview-analysis-grid">
      <DecisionDistribution title="Detection results" rows={groups("verdict_")} loading={detect.isPending} failed={detect.isError} />
      <DecisionDistribution title="Severity" rows={groups("severity_")} loading={detect.isPending} failed={detect.isError} />
    </div>
    <section className="mt-8" aria-labelledby="recent-preview-title">
      <div className="overview-section-heading"><h3 id="recent-preview-title">Recent preview events</h3><Link to="/agent/decisions?tab=shadow">View all <ArrowUpRight size={13} aria-hidden /></Link></div>
      {shadow.isPending ? <p className="overview-empty">Loading events</p> : shadow.isError ?
        <RetryableError error={shadow.error} onRetry={() => shadow.refetch()} retrying={shadow.isFetching} context="Couldn't load preview events" /> :
        !shadow.data?.length ? <p className="overview-empty">No preview events</p> :
        <ul className="overview-event-list">{shadow.data.slice(0, 6).map((event) => <li key={`${event.pattern_id}-${event.first_seen}`}>
          <span className="overview-event-dot" aria-hidden />
          <div className="min-w-0"><Link to={`/agent/decisions/shadow/${encodeURIComponent(event.pattern_id)}`} className="block truncate text-sm text-ink-100" title={event.sample_message}>{event.sample_message || "Log event"}</Link><span className="text-xs text-ink-300">{decisionLabel(event.verdict)}</span></div>
          <time className="shrink-0 text-xs text-ink-400" title={fmtAbs(event.last_seen)}>{fmtRel(event.last_seen)}</time>
        </li>)}</ul>}
    </section>
  </section>;
}

function decisionLabel(value: string) {
  const labels: Record<string, string> = {
    spike: "Log spike", unknown: "New behavior", known: "Recognized", known_pattern: "Recognized",
    normal: "Normal", critical: "Critical", high: "High", medium: "Medium", low: "Low",
    warning: "Warning", error: "Error", info: "Information",
  };
  return labels[value] ?? value.replaceAll("_", " ");
}

function DecisionDistribution({ title, rows, loading, failed }: { title: string; rows: Array<[string, number]>; loading: boolean; failed: boolean }) {
  const max = Math.max(1, ...rows.map(([, value]) => value));
  return <section aria-label={title}><h3 className="mb-5 text-sm font-semibold text-ink-100">{title}</h3>
    {loading ? <div className="space-y-3" aria-label={`Loading ${title}`}>{[0, 1, 2].map((item) => <div key={item} className="sk h-5 w-full" />)}</div> : failed ?
      <p className="overview-empty">Unavailable</p> : rows.length === 0 ? <p className="overview-empty">No recorded decisions</p> :
      <ul className="space-y-4">{rows.map(([label, count]) => <li key={label}>
        <div className="mb-1.5 flex justify-between gap-2 text-xs"><span className="text-ink-300">{label}</span><span className="font-medium tabular-nums text-ink-100">{count.toLocaleString()}</span></div>
        <div className="h-1.5 rounded-sm bg-ink-600/50" aria-hidden><div className="h-full rounded-sm bg-accent/70" style={{ width: `${count / max * 100}%` }} /></div>
      </li>)}</ul>}
  </section>;
}

function AgentLearning() {
  const status = useQuery({ queryKey: ["status"], queryFn: api.status });
  const patterns = useQuery({ queryKey: ["patterns"], queryFn: api.listPatterns });
  const baselines = useQuery({ queryKey: ["baselines-overview"], queryFn: async () => {
    try { return await api.listBaselines(); } catch (error) {
      if (error instanceof ApiError && [403, 404].includes(error.status)) return null;
      throw error;
    }
  }, retry: false });
  const counts = (type: "metric" | "trace") => {
    if (baselines.isError) return "Unavailable";
    if (baselines.isPending) return "Loading";
    if (baselines.data === null) return "Enterprise";
    return baselines.data?.baselines.filter((row) => row.type === type).length.toLocaleString() ?? "0";
  };
  const top = [...(patterns.data ?? [])].sort((left, right) => right.count - left.count).slice(0, 8);
  return <section aria-labelledby="agent-learning-title">
    <div className="overview-section-heading"><h2 id="agent-learning-title"><Layers size={17} aria-hidden />Learning</h2><Link to="/agent/logs">All log patterns <ArrowUpRight size={13} aria-hidden /></Link></div>
    <dl className="overview-stat-strip overview-stat-strip-three">
      <div><dt>Log patterns</dt><dd>{recordedNumber(status.data?.patterns, status.isError)}</dd></div>
      <div><dt>Metric baselines</dt><dd>{counts("metric")}</dd></div>
      <div><dt>Trace baselines</dt><dd>{counts("trace")}</dd></div>
    </dl>
    {status.isError && <RetryableError error={status.error} onRetry={() => status.refetch()} retrying={status.isFetching} context="Couldn't load learning totals" />}
    {baselines.isError && <RetryableError error={baselines.error} onRetry={() => baselines.refetch()} retrying={baselines.isFetching} context="Couldn't load baselines" />}
    <div className="overview-section-heading mt-7"><h3>Frequent log patterns</h3><span className="text-xs text-ink-400">Recorded sightings</span></div>
    {patterns.isPending ? <p className="overview-empty">Loading patterns</p> : patterns.isError ?
      <RetryableError error={patterns.error} onRetry={() => patterns.refetch()} retrying={patterns.isFetching} context="Couldn't load log patterns" /> :
      top.length === 0 ? <p className="overview-empty">No log patterns learned yet</p> :
      <div className="overflow-x-auto"><table className="overview-pattern-table">
        <thead><tr><th>Log pattern</th><th>Status</th><th className="text-right">Sightings</th><th className="text-right">Usual rate</th></tr></thead>
        <tbody>{top.map((pattern) => <tr key={pattern.id}>
          <td><Link className="block max-w-md truncate text-link" to={`/agent/logs/${encodeURIComponent(pattern.id)}`} title={pattern.template}>{pattern.template}</Link></td>
          <td data-label="Status" className="whitespace-nowrap">{pattern.verdict === "known" ? "Recognized" : "Learning"}</td>
          <td data-label="Sightings" className="text-right tabular-nums">{pattern.count.toLocaleString()}</td>
          <td data-label="Usual rate" className="text-right tabular-nums">{pattern.baseline_frequency.toFixed(1)}/s</td>
        </tr>)}</tbody>
      </table></div>}
  </section>;
}