import { useId } from "react";
import { AlertTriangle, RefreshCw, ShieldAlert } from "lucide-react";
import { syncDuration, syncHealthStateLabel, syncSince, type KubernetesSyncHealth as Health } from "@/lib/kubernetesSyncHealth";
import { KubernetesPreparingPanel } from "@/components/KubernetesPreparingPanel";

const stateTone: Record<Health["state"], string> = {
  ready: "pill pill-good",
  preparing: "pill",
  degraded: "pill pill-warn",
  failed: "pill pill-bad",
};

/**
 * The in-page surface for a cluster's sync state before it has ever synced.
 *
 * Ready renders nothing, because healthy data speaks for itself. Before this
 * cluster has ever published a generation the other three states replace every
 * data view: there is nothing worth keeping on screen, and each block names its
 * state, the kinds that cannot be used, why in plain words, and when they were
 * last tried. Nothing here presents a count, a chart or a graph: a failing kind
 * is never shown as loading and never as zero.
 *
 * Once a generation has been published, a failure with data on screen renders
 * nothing here: the pages keep their data, and the top-bar status owns the
 * state, its reasons and Retry. A published cluster whose data is gone — access
 * was lost, or no read answered — has nothing to keep on screen, so the same
 * panel names the failure instead of leaving an empty or loading page.
 */
export function KubernetesSyncHealthSurface({ health, onRetry, retrying, cluster, dataOnScreen = true }: {
  health: Health;
  onRetry?: () => void;
  retrying?: boolean;
  cluster?: string;
  /** Whether the page has cluster data on screen for the top-bar status to describe. */
  dataOnScreen?: boolean;
}) {
  const titleID = useId();
  const published = health.firstReadyAt !== undefined;
  if (health.state === "ready" || published && (health.state === "preparing" || dataOnScreen)) return null;

  if (health.state === "preparing") {
    return <div data-sync-health="preparing"><KubernetesPreparingPanel progress={health.progress} cluster={cluster} /></div>;
  }

  const failed = health.state === "failed";
  const stale = health.staleSeconds !== undefined;
  return <section aria-labelledby={titleID} data-sync-health={health.state} className="card rise-in overflow-hidden">
    <div className="flex flex-wrap items-start gap-3 border-b border-ink-700 p-4 sm:p-6">
      <span aria-hidden="true" className={`inline-flex size-9 shrink-0 items-center justify-center rounded-full ${failed ? "bg-sev-critical/15 text-sev-critical" : "bg-sev-warn/15 text-sev-warning"}`}>
        {failed ? <ShieldAlert size={16} /> : <AlertTriangle size={16} />}
      </span>
      <div className="min-w-0 flex-1 space-y-1">
        <div className="flex flex-wrap items-center gap-2">
          <h2 id={titleID} className="text-sm font-semibold text-ink-50">{published
            ? failed ? "Cannot read this cluster right now" : "Cannot show this cluster right now"
            : failed ? "Cannot connect to this cluster" : "Cannot finish syncing this cluster"}</h2>
          <span className={stateTone[health.state]}>{syncHealthStateLabel(health.state)}</span>
        </div>
        <p className="text-xs text-ink-300">
          {published
            ? failed
              ? "This cluster synced before, but Versus cannot read it now. Cluster data appears again after the next successful sync."
              : "This cluster synced before, but its data cannot be shown now. Cluster data appears again after the next successful sync."
            : failed
            ? "Versus could not read this cluster yet. Cluster data appears here after the first successful sync."
            : stale
              ? `The last cluster snapshot is ${syncDuration(health.staleSeconds!)} old. Cluster data appears here after a fresh sync lands.`
              : "Some resource types did not finish their first sync. Cluster data appears here once they do."}
        </p>
      </div>
      {onRetry && <button type="button" className="btn" onClick={onRetry} disabled={retrying} data-sync-retry>
        <RefreshCw size={13} aria-hidden="true" className={retrying ? "animate-spin motion-reduce:animate-none" : undefined} />Retry sync
      </button>}
    </div>
    {health.problems.length > 0
      ? <ul aria-label="Resource types that could not sync" className="divide-y divide-ink-700">
        {health.problems.map((problem) => {
          const seen = syncSince(problem.since);
          return <li key={problem.kind} data-sync-problem={problem.kind} className="flex min-w-0 flex-wrap items-baseline gap-x-2 gap-y-1 px-4 py-2.5 text-xs sm:px-6">
            <span className="font-medium text-ink-100">{problem.kind}</span>
            <span className="min-w-0 text-ink-300">{problem.reason}</span>
            {problem.skipped > 0 && <span className="tabular-nums text-ink-400">{problem.skipped} {problem.skipped === 1 ? "object" : "objects"} skipped</span>}
            {seen && <time dateTime={seen.iso} title={new Date(seen.iso).toLocaleString()} className="text-ink-400">{problem.sinceBasis === "attempt" ? "last tried" : "since"} {seen.age} ago</time>}
          </li>;
        })}
      </ul>
      : failed ? <p className="px-4 py-3 text-xs text-ink-300 sm:px-6">The index did not name a failing resource type, so the cause is not yet known.</p> : null}
  </section>;
}
