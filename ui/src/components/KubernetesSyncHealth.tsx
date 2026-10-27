import { useId } from "react";
import { AlertTriangle, Loader2, RefreshCw, ShieldAlert } from "lucide-react";
import { syncHealthStateLabel, type KubernetesSyncHealth as Health } from "@/lib/kubernetesSyncHealth";
import { KubernetesPendingNote } from "@/components/KubernetesPendingNote";
import { KubernetesPreparingPanel } from "@/components/KubernetesPreparingPanel";

const stateTone: Record<Health["state"], string> = {
  ready: "pill-good",
  preparing: "pill",
  degraded: "pill-warn",
  failed: "pill-bad",
};

function minutes(seconds: number) {
  const value = Math.max(0, Math.round(seconds));
  if (value < 60) return `${value}${value === 1 ? " second" : " seconds"}`;
  const count = Math.round(value / 60);
  if (count < 60) return `${count} ${count === 1 ? "minute" : "minutes"}`;
  const hours = Math.round(count / 60);
  return `${hours} ${hours === 1 ? "hour" : "hours"}`;
}

function sinceText(value?: string) {
  if (!value) return undefined;
  const at = new Date(value);
  if (!Number.isFinite(at.getTime()) || at.getUTCFullYear() <= 1) return undefined;
  return { iso: at.toISOString(), age: minutes((Date.now() - at.getTime()) / 1000) };
}

/**
 * The one surface that reports a cluster's sync state.
 *
 * Ready renders nothing, because healthy data speaks for itself. Before this
 * cluster has ever published a generation the other three states replace every
 * data view: there is nothing worth keeping on screen, and each block names its
 * state, the kinds that cannot be used, why in plain words, and when they were
 * last tried. Nothing here presents a count, a chart or a graph: a failing kind
 * is never shown as loading and never as zero.
 *
 * Once a generation has been published (`firstReadyAt`), the same states are a
 * strip instead: the views keep the last data they had, the strip names the
 * state and the reason, and it says how old that data is. Nothing is removed
 * from the screen by a background resync, and nothing on screen claims to be
 * fresher than the strip says it is.
 */
export function KubernetesSyncHealthSurface({ health, onRetry, retrying, cluster }: {
  health: Health;
  onRetry?: () => void;
  retrying?: boolean;
  cluster?: string;
}) {
  const titleID = useId();
  if (health.state === "ready") return null;
  if (health.firstReadyAt) return <SyncStrip health={health} onRetry={onRetry} retrying={retrying} />;

  if (health.state === "preparing") {
    if (health.progress) return <div data-sync-health="preparing"><KubernetesPreparingPanel progress={health.progress} cluster={cluster} /></div>;
    return <section aria-labelledby={titleID} data-sync-health="preparing" className="card rise-in overflow-hidden">
      <div className="flex flex-wrap items-start gap-3 p-4 sm:p-6">
        <span aria-hidden="true" className="inline-flex size-9 shrink-0 items-center justify-center rounded-full bg-accent-subtle text-link"><Loader2 size={16} className="animate-spin motion-reduce:animate-none" /></span>
        <div className="min-w-0 flex-1 space-y-1">
          <div className="flex flex-wrap items-center gap-2">
            <h2 id={titleID} className="text-sm font-semibold text-ink-50">Preparing the cluster</h2>
            <span className={`${stateTone[health.state]} self-center`}>{syncHealthStateLabel(health.state)}</span>
          </div>
          <p className="text-xs text-ink-300">Listing the resource types this cluster serves{cluster ? ` on ${cluster}` : ""}. Resource data appears once every type is ready.</p>
        </div>
      </div>
      {health.loadingCount > 0 || health.loading.length > 0 ? <div className="border-t border-ink-700 px-4 py-3 sm:px-6"><KubernetesPendingNote count={health.loadingCount} kinds={health.loading} /></div> : null}
    </section>;
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
          <h2 id={titleID} className="text-sm font-semibold text-ink-50">{failed ? "Cluster sync failed" : "Cluster sync degraded"}</h2>
          <span className={stateTone[health.state]}>{syncHealthStateLabel(health.state)}</span>
        </div>
        <p className="text-xs text-ink-300">
          {failed
            ? "The cluster sync cannot run, so cluster data stays hidden until it can."
            : stale
              ? `The last cluster snapshot is ${minutes(health.staleSeconds!)} old, so cluster data stays hidden until a fresh sync lands.`
              : "Some resource types did not finish syncing, so cluster data stays hidden until they do."}
        </p>
      </div>
      {onRetry && <button type="button" className="btn" onClick={onRetry} disabled={retrying} data-sync-retry>
        <RefreshCw size={13} aria-hidden="true" className={retrying ? "animate-spin motion-reduce:animate-none" : undefined} />Retry sync
      </button>}
    </div>
    {health.problems.length > 0
      ? <ul aria-label="Resource types that could not sync" className="divide-y divide-ink-700">
        {health.problems.map((problem) => {
          const seen = sinceText(problem.since);
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

/**
 * The same sync state as a non-blocking strip, for a cluster that has published
 * before. It names the state and the first kinds that cannot be used, says how
 * old the data on screen is, and carries the same Retry. It never covers the
 * views and never removes their data: an operator mid-incident keeps the screen
 * they were reading and is told, in words, what is in front of them.
 */
function SyncStrip({ health, onRetry, retrying }: {
  health: Health;
  onRetry?: () => void;
  retrying?: boolean;
}) {
  const failed = health.state === "failed";
  const preparing = health.state === "preparing";
  const stale = health.staleSeconds !== undefined;
  const headline = failed ? "Cluster sync failed" : preparing ? "Cluster sync running" : "Cluster sync degraded";
  const reason = failed
    ? "The cluster cannot be read right now."
    : preparing
      ? "A background sync is running."
      : stale
        ? `The last snapshot is ${minutes(health.staleSeconds!)} old.`
        : "Some resource types did not finish syncing.";
  const ageSeconds = health.ageSeconds ?? health.staleSeconds;
  // "0 seconds ago" reads like a clock error; a snapshot this fresh is just now.
  const age = ageSeconds === undefined ? undefined : ageSeconds < 5 ? "just now" : `${minutes(ageSeconds)} ago`;
  const problems = health.problems;
  return <section
    aria-label="Cluster sync status"
    data-sync-strip={health.state}
    className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-2 rounded-card border border-ink-600 bg-surface px-3 py-2 text-xs"
  >
    <span aria-hidden="true" className={`inline-flex size-5 shrink-0 items-center justify-center rounded-full ${failed ? "bg-sev-critical/15 text-sev-critical" : "bg-sev-warn/15 text-sev-warning"}`}>
      {failed ? <ShieldAlert size={12} /> : <AlertTriangle size={12} />}
    </span>
    {/* The live region is the sentence, not the whole strip: Retry must be
        reachable without being announced as new text. */}
    <p role="status" data-sync-strip-summary className="min-w-0 flex-1">
      <span className="font-medium text-ink-100">{headline}</span>
      <span className="text-ink-300"> {reason}</span>
      {age && <span data-sync-age className="text-ink-400"> {age === "just now" ? "Showing data loaded just now." : `Showing data from ${age}.`}</span>}
    </p>
    {problems.length > 0 && <ul aria-label="Resource types that could not sync" className="flex min-w-0 basis-full flex-wrap gap-x-3 gap-y-1 text-ink-300">
      {problems.slice(0, 3).map((problem) => <li key={problem.kind} data-sync-problem={problem.kind} className="min-w-0">
        <span className="font-medium text-ink-100">{problem.kind}</span>
        <span> {problem.reason}</span>
        {problem.skipped > 0 && <span className="tabular-nums text-ink-400"> ({problem.skipped} skipped)</span>}
      </li>)}
      {problems.length > 3 && <li className="text-ink-400">{problems.length - 3} more resource types</li>}
    </ul>}
    {onRetry && <button type="button" className="btn" onClick={onRetry} disabled={retrying} data-sync-retry>
      <RefreshCw size={13} aria-hidden="true" className={retrying ? "animate-spin motion-reduce:animate-none" : undefined} />Retry sync
    </button>}
  </section>;
}
