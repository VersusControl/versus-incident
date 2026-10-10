import { useState } from "react";
import { Link } from "react-router-dom";
import { AlertTriangle, ChevronRight, RefreshCw } from "lucide-react";
import { type KubernetesClusterSummary } from "@/lib/api";
import { TopBar } from "@/components/TopBar";

const healthOrder = { unreachable: 0, attention: 1, partial: 2, unknown: 3, healthy: 4 };
const healthLabels = { unreachable: "Unreachable", attention: "Needs attention", partial: "Partial visibility", unknown: "No node evidence", healthy: "Node checks passing" };
const rowColumns = "grid grid-cols-[minmax(180px,1.5fr)_minmax(155px,1.2fr)_110px_120px_115px_140px_160px] items-center gap-5";

export function KubernetesFleet({ clusters, notice, retry, retrying }: { clusters: KubernetesClusterSummary[]; notice?: string; retry: () => void; retrying: boolean }) {
  const [filter, setFilter] = useState("");
  const sorted = [...clusters].sort((left, right) => healthOrder[left.health] - healthOrder[right.health] || (left.display_name || left.id).localeCompare(right.display_name || right.id));
  const visible = sorted.filter((cluster) => `${cluster.display_name ?? ""} ${cluster.id}`.toLowerCase().includes(filter.toLowerCase()));
  return <main className="min-w-0 flex-1 overflow-auto">
    <TopBar title="Kubernetes" actions={<button type="button" className="btn-icon" aria-label="Refresh clusters" title="Refresh clusters" disabled={retrying} onClick={retry}><RefreshCw size={14} className={retrying ? "animate-spin" : undefined} /></button>} />
    <div className="mx-auto max-w-7xl space-y-4 p-4 sm:p-6">
      {notice && <p role="status" className="text-sm text-sev-warning">{notice}</p>}
      <h1 className="text-base font-semibold text-ink-50">Clusters</h1>
      <div aria-label="Fleet health" className="flex flex-wrap gap-x-5 gap-y-2 border-y border-ink-700 py-3 text-xs text-ink-300">
        <span>{clusters.length} clusters</span>
        {Object.keys(healthOrder).map((health) => <span key={health}>{clusters.filter((cluster) => cluster.health === health).length} {health}</span>)}
      </div>
      {clusters.length > 6 && <label className="block text-xs text-ink-300">Filter clusters<input className="input mt-1 max-w-sm" type="search" value={filter} onChange={(event) => setFilter(event.target.value)} /></label>}
      <div role="region" aria-label="Cluster rows" tabIndex={0} className="overflow-x-auto rounded-card focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent">
        <div className="min-w-[1160px] space-y-2 pb-2">
          <div aria-hidden="true" className={`${rowColumns} px-4 py-2 text-2xs font-medium uppercase text-ink-300`}>
            <span>Cluster</span><span>Health</span><span>Ready nodes</span><span>Running pods</span><span>Signals</span><span>Snapshot</span><span className="text-right">Actions</span>
          </div>
          {visible.map((cluster) => <article key={cluster.id} aria-label={`Cluster ${cluster.id}`} className={`card relative min-w-0 px-4 py-4 ${rowColumns}`}>
            <Link to={`?cluster=${encodeURIComponent(cluster.id)}`} aria-label={`Open cluster ${cluster.display_name || cluster.id}`} className="absolute inset-0 rounded-card focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent" />
            <div className="pointer-events-none relative min-w-0 space-y-1">
              <h2 className="break-words text-sm font-semibold text-ink-50">{cluster.display_name || cluster.id}</h2>
              <p className="break-all font-mono text-2xs text-ink-300">{cluster.id}</p>
              <p className="break-words text-2xs text-ink-300">{cluster.provider} · {cluster.version || "Version unavailable"}</p>
            </div>
            <div className="pointer-events-none relative min-w-0 space-y-2">
              <span className={`pill ${cluster.health === "healthy" ? "pill-good" : cluster.health === "unknown" ? "" : "pill-warn"}`}>{healthLabels[cluster.health]}</span>
              {cluster.health === "unreachable" && <p className="text-2xs text-sev-warning">The cluster did not respond.</p>}
            </div>
            <div className="pointer-events-none relative"><FleetRatio label="Ready nodes" part={cluster.ready_nodes} total={cluster.nodes} unavailable={cluster.health === "unreachable"} /></div>
            <div className="pointer-events-none relative"><FleetRatio label="Running pods" part={cluster.running_pods} total={cluster.pods} unavailable={cluster.health === "unreachable"} /></div>
            <div className="pointer-events-none relative space-y-1 text-xs text-ink-300">
              {cluster.health === "unreachable" ? <span>Unavailable</span> : <><p className={cluster.warnings > 0 ? "text-sev-warning" : undefined}>{cluster.warnings} warnings</p><p>{cluster.issues === undefined ? "Issues unavailable" : `${cluster.issues} issues`}</p></>}
            </div>
            <div className="pointer-events-none relative min-w-0 space-y-1 text-2xs text-ink-300">
              <p className="break-words capitalize">{cluster.sync.state}</p>
              <p>{Number.isFinite(Date.parse(cluster.observed_at)) ? new Date(cluster.observed_at).toLocaleTimeString() : "Snapshot unavailable"}</p>
            </div>
            <div className="relative flex items-center justify-end gap-2">
              {cluster.health === "unreachable" ? <button type="button" className="btn" disabled={retrying} onClick={retry}><RefreshCw size={12} aria-hidden="true" />Retry</button> : <Link className="btn whitespace-nowrap" to={`?cluster=${encodeURIComponent(cluster.id)}&view=issues`}><AlertTriangle size={12} aria-hidden="true" />Review issues</Link>}
              <ChevronRight size={16} aria-hidden="true" className="pointer-events-none shrink-0 text-ink-300" />
            </div>
          </article>)}
        </div>
      </div>
      {!visible.length && <p role="status" className="text-sm text-ink-400">{clusters.length ? "No clusters match." : "No clusters available."}</p>}
    </div>
  </main>;
}

function FleetRatio({ label, part, total, unavailable }: { label: string; part: number; total: number; unavailable: boolean }) {
  return <div className="space-y-2 text-xs"><span className="sr-only">{label}</span>{unavailable ? <><p className="text-ink-300">Unavailable</p><div aria-hidden="true" className="h-1 rounded-full bg-ink-600" /></> : <><p className="font-medium tabular-nums text-ink-50">{part} <span className="font-normal text-ink-300">/ {total}</span></p><meter aria-label={label} min={0} max={Math.max(total, 1)} value={part} className="block h-1 w-full" /></>}</div>;
}