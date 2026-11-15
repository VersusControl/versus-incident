import type { ReactNode } from "react";
import { createPortal } from "react-dom";
import { ExternalLink, Info, Loader2, RefreshCw } from "lucide-react";
import { syncDataTime, syncDuration, syncFailureLabel, syncSince, type KubernetesKindProblem, type KubernetesSyncHealthState, type KubernetesUnreadableSummary } from "@/lib/kubernetesSyncHealth";
import { KubernetesReadCoverage } from "@/components/KubernetesUnreadableNote";
import { useAnchoredPopover } from "@/lib/useAnchoredPopover";
import { namespaceAccessDocsUrl, namespaceAccessLabel } from "@/lib/kubernetesNamespaceAccess";

/** How the index record feed is doing, which only matters once the sync is ready. */
/** "polling": live updates are unavailable by design for this identity; views refresh on a timer. */
export type KubernetesStreamState = "connecting" | "live" | "reconnecting" | "resyncing" | "polling";

/**
 * The cluster sync state in the top bar.
 *
 * One rule for all four states at every width: the state is always written out,
 * so a narrow viewport never leaves an operator with a coloured dot alone. The
 * dot only repeats what the words already say.
 *
 * A failed or degraded sync is the one place that state is reported once a
 * cluster has synced before: the label says how old the data on screen is, and
 * a details popover — on hover, focus or press — names each failing resource
 * type with its reason and carries Retry sync.
 */
export function KubernetesSyncStateIndicator({ state, streamState, ageSeconds, title, firstReadyAt, syncReason, syncState, lastSyncedAt, dataOnScreen = true, problems = [], staleSeconds, onRetry, retrying, readableNamespaces, unreadable = [], notCollected = [], namespaceScoped = [] }: {
  lastSyncedAt?: string;
  syncReason?: string;
  syncState?: string;
  firstReadyAt?: string;
  state: KubernetesSyncHealthState | "loading";
  streamState: KubernetesStreamState;
  ageSeconds?: number;
  title?: string;
  /**
   * Whether any cluster data is actually on screen. A cold reload with a dead
   * credential has nothing to date, so the label must not claim to show data.
   */
  dataOnScreen?: boolean;
  /** The resource types that cannot be used, named in the details popover. */
  problems?: KubernetesKindProblem[];
  /** Age of the last snapshot when the sync is degraded because it is stale. */
  staleSeconds?: number;
  onRetry?: () => void;
  retrying?: boolean;
  /**
   * The namespaces a namespace-only identity reads. A ready sync then says so
   * steadily instead of a freshness line, because reading only those namespaces
   * is the expected state of that role and not a degraded sync.
   */
  readableNamespaces?: string[];
  /** Kinds the connector identity may not read; named in the details, never as a fault. */
  unreadable?: KubernetesUnreadableSummary[];
  /** Kinds the configuration excludes; named in the details in a muted line. */
  notCollected?: string[];
  /** Kinds read only inside some namespaces; named calmly in the details. */
  namespaceScoped?: KubernetesUnreadableSummary[];
}) {
  const coverage = <KubernetesReadCoverage unreadable={unreadable} notCollected={notCollected} namespaceScoped={namespaceScoped} className="mt-3 border-t border-ink-700 pt-3" />;
  const coverageSummary = readCoverageSummary(unreadable, notCollected, namespaceScoped);
  const renewing = (syncState === "refreshing" || syncState === "syncing") && syncReason?.includes("credential");
  const failing = state === "failed" || state === "degraded";
  // The current health read outranks progress cached from an older stream.
  // A feed that is still opening is not a disconnection: the hook reports
  // "reconnecting" once a connection attempt fails or stays silent too long.
  const label = failing ? syncFailureLabel(state, { onScreen: Boolean(firstReadyAt) && dataOnScreen, time: syncDataTime(lastSyncedAt, ageSeconds) })
    : state === "loading" ? "Loading" : renewing ? "Refreshing credentials…"
    : syncState === "refreshing" || syncState === "syncing" && firstReadyAt ? "Syncing"
    : state === "ready"
    ? streamState === "polling" ? "Live updates off — refreshing every 30s"
    : streamState === "live" || streamState === "connecting" ? ageSeconds !== undefined ? `Last synced ${Math.round(ageSeconds)}s ago` : "Last sync time unavailable" : "Updates disconnected — refreshing every 30s"
    : firstReadyAt ? "Syncing" : "Preparing";
  const ink = state === "failed" ? "text-sev-critical-text"
    : state === "degraded" ? "text-sev-warning-text"
      : state === "preparing" ? "text-ink-300"
        : streamState === "live" ? "text-sev-ok" : "text-ink-400";
  const dot = state === "failed" ? "bg-sev-critical"
    : state === "degraded" ? "bg-sev-warning"
      : state === "preparing" ? "bg-accent"
        : streamState === "live" ? "bg-sev-ok" : "bg-ink-400";
  const marker = state === "preparing"
    ? <Loader2 size={13} aria-hidden="true" className="shrink-0 animate-spin motion-reduce:animate-none" />
    : <span aria-hidden="true" className={`size-1.5 shrink-0 rounded-full ${dot}`} />;
  // The live region is the label: a change of state is announced, and the
  // details trigger inside it carries no extra words to read out.
  // A disconnected record feed still reads as such: that line is about
  // freshness, which namespace access does not change.
  const namespaceView = state === "ready" && readableNamespaces !== undefined && !renewing
    && (streamState === "live" || streamState === "connecting" || streamState === "polling") && syncState !== "refreshing";
  return <><span role="status" aria-live="polite" data-sync-state={state} data-namespace-access={namespaceView ? "" : undefined} title={failing || namespaceView ? undefined : title} className={`inline-flex min-w-0 items-center gap-1.5 whitespace-nowrap text-2xs font-medium ${ink}`}>
    {failing
      ? <SyncFailureDetails state={state} label={label} marker={marker} problems={problems} staleSeconds={staleSeconds} onRetry={onRetry} retrying={retrying} coverage={coverageSummary && coverage} />
      : namespaceView
        ? <NamespaceAccessDetails namespaces={readableNamespaces} marker={marker} freshness={streamState === "polling" ? "Live updates are off for namespace access; this view refreshes every 30 seconds." : label} coverage={coverageSummary && coverage} />
        : <>{marker}{label}</>}
  </span>{!failing && !namespaceView && state !== "loading" && coverageSummary && <ReadCoverageDetails summary={coverageSummary}>{coverage}</ReadCoverageDetails>}</>;
}

/**
 * A calm marker beside a healthy status when some kinds are not shown. Hover,
 * focus or press opens the same anchored details as the sync status.
 */
function ReadCoverageDetails({ summary, children }: { summary: { text: string; label: string }; children: ReactNode }) {
  const { open, triggerProps, panelProps } = useAnchoredPopover({ bridgeFocus: true });
  return <>
    <button {...triggerProps} type="button" aria-label={summary.label} data-read-coverage-trigger
      className="inline-flex min-h-8 shrink-0 items-center gap-1 whitespace-nowrap rounded-control px-1.5 text-2xs font-medium text-ink-400 hover:bg-ink-700 hover:text-ink-200 focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent">
      <Info size={12} aria-hidden="true" /><span aria-hidden="true" className="hidden sm:inline">{summary.text}</span>
    </button>
    {open && createPortal(<div {...panelProps} role="region" aria-label="Sync status details" data-sync-details="coverage"
      className="fixed overflow-auto whitespace-normal rounded-control border border-ink-600 bg-surface-raised p-3 text-xs text-ink-200 shadow-overlay">
      <p className="font-medium">Resource types not shown</p>
      {children}
    </div>, document.body)}
  </>;
}

/** How many kinds the top-bar marker counts, and its short words. */
function readCoverageSummary(unreadable: readonly KubernetesUnreadableSummary[], notCollected: readonly string[], namespaceScoped: readonly KubernetesUnreadableSummary[]): { count: number; text: string; label: string } | undefined {
  const excluded = new Set(notCollected);
  const refused = unreadable.filter((kind) => !excluded.has(kind.kind)).length;
  const scoped = namespaceScoped.filter((kind) => !excluded.has(kind.kind)).length;
  const skipped = notCollected.length;
  if (!refused && !skipped && !scoped) return undefined;
  const parts = [refused ? `${refused} not readable` : "", scoped ? `${scoped} readable only in some namespaces` : "", skipped ? `${skipped} not collected` : ""].filter(Boolean);
  const total = refused + scoped + skipped;
  return { count: total, text: parts.length > 1 ? `${total} limited` : scoped ? `${scoped} partly readable` : parts[0], label: `Resource types not fully shown: ${parts.join(", ")}` };
}


/**
 * The steady status of a namespace-only identity: which namespaces are shown,
 * with a popover saying what that role leaves out and linking to the docs.
 */
function NamespaceAccessDetails({ namespaces, marker, freshness, coverage }: { namespaces: string[]; marker: ReactNode; freshness: string; coverage?: ReactNode }) {
  const { open, triggerProps, panelProps } = useAnchoredPopover({ bridgeFocus: true });
  const label = namespaceAccessLabel(namespaces);
  return <>
    <button {...triggerProps} type="button" data-namespace-access-trigger
      className="inline-flex min-h-8 min-w-0 max-w-[14rem] items-center gap-1.5 rounded-control px-1.5 hover:bg-ink-700 focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent sm:max-w-md">
      {marker}<span className="min-w-0 truncate">{label}</span>
    </button>
    {open && createPortal(<div {...panelProps} role="region" aria-label="Namespace access details" data-namespace-access-details
      className="fixed overflow-auto whitespace-normal rounded-control border border-ink-600 bg-surface-raised p-3 text-xs text-ink-200 shadow-overlay">
      <p className="font-medium">Namespace access</p>
      <p className="mt-2">This connector reads only {namespaces.length === 1 ? "the namespace" : "the namespaces"} it was granted{namespaces.length ? <>: <span className="text-ink-100">{namespaces.join(", ")}</span></> : ""}. Cluster-wide resources such as Nodes are not shown, and history is not saved for this role.</p>
      <p data-namespace-freshness className="mt-2 text-ink-300">{freshness}</p>
      <a className="text-link mt-2 inline-flex items-center gap-1" href={namespaceAccessDocsUrl} target="_blank" rel="noopener noreferrer">About namespace-only access <ExternalLink size={12} aria-hidden="true" /></a>
      {coverage}
    </div>, document.body)}
  </>;
}

function SyncFailureDetails({ state, label, marker, problems, staleSeconds, onRetry, retrying, coverage }: {
  state: "failed" | "degraded";
  label: string;
  marker: ReactNode;
  problems: KubernetesKindProblem[];
  staleSeconds?: number;
  onRetry?: () => void;
  retrying?: boolean;
  /** What the connector does not read, added below the failure. */
  coverage?: ReactNode;
}) {
  const { open, triggerProps, panelProps } = useAnchoredPopover({ bridgeFocus: true });
  const failed = state === "failed";
  const summary = failed
    ? "The cluster cannot be read right now."
    : staleSeconds !== undefined
      ? `The last snapshot is ${syncDuration(staleSeconds)} old.`
      : "Some resource types did not finish syncing.";
  return <>
    <button {...triggerProps} type="button" data-sync-details-trigger
      className="inline-flex min-h-8 min-w-0 max-w-full items-center gap-1.5 rounded-control px-1.5 hover:bg-ink-700 focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent">
      {marker}<span className="min-w-0 truncate">{label}</span>
    </button>
    {open && createPortal(<div {...panelProps} role="region" aria-label="Sync status details" data-sync-details={state}
      className="fixed overflow-auto rounded-control border border-ink-600 bg-surface-raised p-3 text-xs text-ink-200 shadow-overlay">
      <p className="font-medium">{failed ? "Sync failed" : "Sync degraded"}</p>
      {problems.length > 0
        ? <ul tabIndex={problems.length > 6 ? 0 : undefined} aria-label="Resource types that could not sync" className="mt-2 max-h-48 space-y-1.5 overflow-y-auto break-words">
          {problems.map((problem) => {
            const seen = syncSince(problem.since);
            return <li key={problem.kind} data-sync-problem={problem.kind}>
              <span className="font-medium text-ink-100">{problem.kind}</span>
              <span>: {problem.reason}{problem.namespaces?.length ? ` in ${problem.namespaces.join(", ")}` : ""}</span>
              {problem.skipped > 0 && <span className="tabular-nums text-ink-300"> ({problem.skipped} {problem.skipped === 1 ? "object" : "objects"} skipped)</span>}
              {seen && <> <time dateTime={seen.iso} title={new Date(seen.iso).toLocaleString()} className="whitespace-nowrap text-ink-300">· {problem.sinceBasis === "attempt" ? "last tried" : "since"} {seen.age} ago</time></>}
            </li>;
          })}
        </ul>
        : <p className="mt-2">{summary}</p>}
      {onRetry && <button type="button" className="btn mt-3" onClick={onRetry} disabled={retrying} data-sync-retry>
        <RefreshCw size={13} aria-hidden="true" className={retrying ? "animate-spin motion-reduce:animate-none" : undefined} />Retry sync
      </button>}
      {coverage}
    </div>, document.body)}
  </>;
}
