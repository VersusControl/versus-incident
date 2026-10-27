import { useId, useState } from "react";
import { Link } from "react-router-dom";
import { AlertTriangle, ChevronRight, RefreshCw } from "lucide-react";
import { type KubernetesClusterSummary, type KubernetesSyncHealthReport, type KubernetesSyncProgress } from "@/lib/api";
import { firstSyncProgress, firstSyncProgressText } from "@/lib/kubernetesFirstSync";
import { isZeroTime, reportSyncHealth, type KubernetesSyncHealthState } from "@/lib/kubernetesSyncHealth";
import { TopBar } from "@/components/TopBar";

// One classification drives the row pill, the row order and the header counts.
// It is the cluster's complete sync report, the same source the explorer gates
// on, so the fleet can never read ready while the cluster view reads failed.
// A cluster with no report yet is "Not ready": the catalog envelope it would
// otherwise fall back on only covers eight core kinds.
type FleetState = KubernetesSyncHealthState | "not-ready" | "syncing";

// Order: a cluster that needs action comes before one that only needs
// patience, so a failing cluster leads and a healthy one trails.
type HeaderTone = "neutral" | "warn" | "ok";
const fleetStates: ReadonlyArray<{ state: FleetState; label: string; count: string; pill: string; headerTone: HeaderTone }> = [
  { state: "failed", label: "Failed", count: "failed", pill: "pill pill-bad", headerTone: "warn" },
  { state: "degraded", label: "Degraded", count: "degraded", pill: "pill pill-warn", headerTone: "warn" },
  { state: "not-ready", label: "Not ready", count: "not ready", pill: "pill", headerTone: "neutral" },
  { state: "preparing", label: "Preparing", count: "preparing", pill: "pill", headerTone: "neutral" },
  { state: "syncing", label: "Syncing", count: "syncing", pill: "pill", headerTone: "neutral" },
  { state: "ready", label: "Ready", count: "ready", pill: "pill pill-good", headerTone: "ok" },
];
// Header counts are small chips so the buckets read one at a time instead of
// as a run-on phrase. Tones come from the severity ramp; the buckets that only
// need patience stay neutral, matching their row pill.
const headerChipTone: Record<HeaderTone, string> = {
  neutral: "border-ink-700 text-ink-300",
  warn: "border-sev-warn/30 text-sev-warn",
  ok: "border-sev-ok/30 text-sev-ok",
};
const headerChip = "inline-flex items-center rounded border px-2 py-0.5 text-2xs tabular-nums";
// A cluster whose complete sync report has not arrived cannot be called ready.
const notReadyLabel = "Not ready";
// The same classifier the explorer uses, so an unreadable or failing kind can
// never put a fleet row and the cluster view into different states.
const rowSync = (cluster: KubernetesClusterSummary, reports: Record<string, KubernetesSyncHealthReport | undefined>) => {
  const report = reports[cluster.id];
  return report ? reportSyncHealth(report) : undefined;
};
const fleetSyncState = (sync: ReturnType<typeof rowSync>): FleetState =>
  sync?.state === "preparing" && sync.firstReadyAt ? "syncing" : sync?.state ?? "not-ready";
const fleetState = (cluster: KubernetesClusterSummary, reports: Record<string, KubernetesSyncHealthReport | undefined>): FleetState =>
  fleetSyncState(rowSync(cluster, reports));
const fleetStateInfo = (state: FleetState) => fleetStates.find((entry) => entry.state === state)!;
const stateRank = (state: FleetState) => fleetStates.findIndex((entry) => entry.state === state);
const compareClusters = (reports: Record<string, KubernetesSyncHealthReport | undefined>) => (left: KubernetesClusterSummary, right: KubernetesClusterSummary) => {
  const leftState = fleetState(left, reports);
  const rightState = fleetState(right, reports);
  return stateRank(leftState) - stateRank(rightState)
    || leftState.localeCompare(rightState)
    || (left.display_name || left.id).localeCompare(right.display_name || right.id)
    || left.id.localeCompare(right.id);
};
const rowColumns = "grid grid-cols-[minmax(180px,1.5fr)_minmax(155px,1.2fr)_110px_120px_115px_140px_160px] items-center gap-5";

export function KubernetesFleet({ clusters, reports = {}, notice, retry, retrying }: { clusters: KubernetesClusterSummary[]; reports?: Record<string, KubernetesSyncHealthReport | undefined>; notice?: string; retry: () => void; retrying: boolean }) {
  const healthLabelID = useId();
  const [filter, setFilter] = useState("");
  const sorted = [...clusters].sort(compareClusters(reports));
  const visible = sorted.filter((cluster) => `${cluster.display_name ?? ""} ${cluster.id}`.toLowerCase().includes(filter.toLowerCase()));
  const buckets = fleetStates.map((state) => ({ ...state, total: clusters.filter((cluster) => fleetState(cluster, reports) === state.state).length }));
  const healthLabel = [`${clusters.length} clusters`, ...buckets.map((bucket) => `${bucket.total} ${bucket.count}`)].join(", ");
  return <main className="min-w-0 flex-1 overflow-auto">
    <TopBar title="Kubernetes" actions={<button type="button" className="btn-icon" aria-label="Refresh clusters" title="Refresh clusters" disabled={retrying} onClick={retry}><RefreshCw size={14} className={retrying ? "animate-spin" : undefined} /></button>} />
    <div className="mx-auto max-w-7xl space-y-4 p-4 sm:p-6">
      {notice && <p role="status" className="text-sm text-sev-warning">{notice}</p>}
      <h1 className="text-base font-semibold text-ink-50">Clusters</h1>
      {/* The visible counts are chips set apart by their borders, so their
          names are read from the label rather than as one run-on word. */}
      <div role="group" aria-labelledby={healthLabelID} className="flex flex-wrap items-center gap-2 border-y border-ink-700 py-3 text-xs text-ink-300">
        <span aria-hidden="true">{clusters.length} clusters</span>
        {buckets.map((bucket) => <span key={bucket.state} data-bucket={bucket.state} aria-hidden="true" className={`${headerChip} ${headerChipTone[bucket.headerTone]}`}>{bucket.total} {bucket.count}</span>)}
        <span id={healthLabelID} className="sr-only">{healthLabel}</span>
      </div>
      {clusters.length > 6 && <label className="block text-xs text-ink-300">Filter clusters<input className="input mt-1 max-w-sm" type="search" value={filter} onChange={(event) => setFilter(event.target.value)} /></label>}
      <div role="region" aria-label="Cluster rows" tabIndex={0} className="overflow-x-auto rounded-card focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent">
        <div className="min-w-[1160px] space-y-2 pb-2">
          <div aria-hidden="true" className={`${rowColumns} px-4 py-2 text-2xs font-medium uppercase text-ink-300`}>
            <span>Cluster</span><span>Health</span><span>Ready nodes</span><span>Running pods</span><span>Signals</span><span>Snapshot</span><span className="text-right">Actions</span>
          </div>
          {visible.map((cluster) => {
            const report = reports[cluster.id];
            const sync = rowSync(cluster, reports);
            const state = fleetSyncState(sync);
            const stateInfo = fleetStateInfo(state);
            const ready = state === "ready" || state === "syncing";
            const preparing = state === "preparing";
            const ratioState = ready ? "measured" : preparing ? "loading" : cluster.health === "unreachable" && !report ? "unavailable" : "not-ready";
            const open = `Open cluster ${cluster.display_name || cluster.id}`;
            return <article key={cluster.id} aria-label={`Cluster ${cluster.id}`} className="card relative min-w-0 px-4 py-4">
              {/* The whole row is one link; its name carries the live progress
                  the narrow line can only truncate. */}
              <Link to={`?cluster=${encodeURIComponent(cluster.id)}`} aria-label={preparing ? `${open}: Preparing first data, ${firstSyncProgressText(cluster.sync?.progress)}` : open} className="absolute inset-0 rounded-card focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent" />
              <div className={`relative ${rowColumns}`}>
                <div className="pointer-events-none relative min-w-0 space-y-1">
                  <h2 className="break-words text-sm font-semibold text-ink-50">{cluster.display_name || cluster.id}</h2>
                  <p className="break-all font-mono text-2xs text-ink-300">{cluster.id}</p>
                  <p className="break-words text-2xs text-ink-300">{cluster.provider} · {cluster.version || "Version unavailable"}</p>
                </div>
                <div data-health-cell className="pointer-events-none relative min-w-0 space-y-2">
                  <span data-sync-state-label className={stateInfo.pill}>{stateInfo.label}</span>
                  {cluster.health === "unreachable" && <p className="text-2xs text-sev-warning">The cluster did not respond.</p>}
                </div>
                <div className="pointer-events-none relative"><FleetRatio label="Ready nodes" part={cluster.ready_nodes} total={cluster.nodes} state={ratioState} /></div>
                <div className="pointer-events-none relative"><FleetRatio label="Running pods" part={cluster.running_pods} total={cluster.pods} state={ratioState} /></div>
                <div className="pointer-events-none relative space-y-1 text-xs text-ink-300">
                  {cluster.health === "unreachable" && !report ? <span>Unavailable</span> : preparing ? <span>Waiting for first data</span> : ready ? <><p className={cluster.warnings > 0 ? "text-sev-warning" : undefined}>{cluster.warnings} warnings</p>{typeof cluster.broken === "number" && cluster.broken > 0 && <p className="text-sev-critical">{cluster.broken} broken</p>}{cluster.last_change_at && Number.isFinite(Date.parse(cluster.last_change_at)) && <p>Last change {Math.max(0, Math.floor((Date.now() - Date.parse(cluster.last_change_at)) / 60_000))} min ago</p>}</> : <span>{notReadyLabel}</span>}
                </div>
                <div className="pointer-events-none relative min-w-0 space-y-1 text-2xs text-ink-300">
                  {preparing ? <p>Preparing first data</p> : <p>{isZeroTime(cluster.observed_at) ? "Snapshot unavailable" : new Date(cluster.observed_at).toLocaleTimeString()}</p>}
                </div>
                <div className="relative flex items-center justify-end gap-2">
                  {cluster.health === "unreachable" ? <button type="button" className="btn" disabled={retrying} onClick={retry}><RefreshCw size={12} aria-hidden="true" />Retry</button> : <Link className="btn whitespace-nowrap" to={`?cluster=${encodeURIComponent(cluster.id)}&view=issues`}><AlertTriangle size={12} aria-hidden="true" />Review issues</Link>}
                  <ChevronRight size={16} aria-hidden="true" className="pointer-events-none shrink-0 text-ink-300" />
                </div>
              </div>
              {preparing && <FirstSyncLine progress={cluster.sync?.progress} />}
            </article>;
          })}
        </div>
      </div>
      {!visible.length && <p role="status" className="text-sm text-ink-400">{clusters.length ? "No clusters match." : "No clusters available."}</p>}
    </div>
  </main>;
}

// First-sync progress as one full-width line under the row: the running step,
// then every live count as its own element. The chips are set apart by spacing
// only, never by a separator character, and the line truncates on a narrow
// viewport instead of wrapping. The same parts are readable as one phrase,
// which is what the group name and the row link announce.
function FirstSyncLine({ progress }: { progress?: KubernetesSyncProgress }) {
  const labelID = useId();
  const parts = firstSyncProgress(progress);
  const readable = firstSyncProgressText(progress);
  return <div data-first-sync role="group" aria-labelledby={labelID} className="pointer-events-none relative mt-3 flex min-w-0 items-baseline gap-x-2 overflow-hidden whitespace-nowrap text-2xs text-ink-300">
    <span aria-hidden="true" data-first-sync-step className="min-w-0 truncate text-ink-200">{parts?.step ?? "Preparing"}</span>
    {parts?.counts.map((count) => <span key={count} aria-hidden="true" data-first-sync-count className="shrink-0">{count}</span>)}
    <span id={labelID} className="sr-only">{readable}</span>
  </div>;
}

type FleetRatioState = "measured" | "loading" | "unavailable" | "not-ready";

function FleetRatio({ label, part, total, state }: { label: string; part: number; total: number; state: FleetRatioState }) {
  return <div className="space-y-2 text-xs"><span className="sr-only">{label}</span>{state === "measured" ? <><p className="font-medium tabular-nums text-ink-50">{part} <span className="font-normal text-ink-300">/ {total}</span></p><meter aria-label={label} min={0} max={Math.max(total, 1)} value={part} className="block h-1 w-full" /></> : <><p className="text-ink-300">{state === "loading" ? "Loading" : state === "not-ready" ? "Hidden" : "Unavailable"}</p><div aria-hidden="true" className="h-1 rounded-full bg-ink-600" /></>}</div>;
}