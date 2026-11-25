import { useEffect, useId, useState, type ReactNode } from "react";
import { createPortal } from "react-dom";
import { CircleAlert, ExternalLink, Info, Loader2, RefreshCw } from "lucide-react";
import { syncDataTime, syncDuration, syncFailureLabel, syncSince, type KubernetesKindProblem, type KubernetesSyncHealthState, type KubernetesUnreadableSummary } from "@/lib/kubernetesSyncHealth";
import { KubernetesReadCoverage } from "@/components/KubernetesUnreadableNote";
import { useAnchoredPopover } from "@/lib/useAnchoredPopover";
import { namespaceAccessDocsUrl, namespaceAccessLabel } from "@/lib/kubernetesNamespaceAccess";
import { retryCountdown } from "@/lib/kubernetesLiveFailure";

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
export function KubernetesSyncStateIndicator({ refreshNote, refreshRetryAt, statusStaleSeconds, statusUnavailable = false, state, streamState, ageSeconds, title, firstReadyAt, syncReason, syncState, lastSyncedAt, dataOnScreen = true, problems = [], staleSeconds, onRetry, retrying, readableNamespaces, unreadable = [], notCollected = [], namespaceScoped = [] }: {
  lastSyncedAt?: string;
  /**
   * Why the sync state shown is not newer, when the status read itself did not
   * answer. A neutral line beside the last known state, never a failure.
   */
  refreshNote?: string;
  /** When the status is read again; the note counts down to it. */
  refreshRetryAt?: number;
  /**
   * Status reads keep failing and the last report's snapshot has grown past
   * the server's stale limit: the state shows as degraded with that age.
   */
  statusStaleSeconds?: number;
  /** Status reads keep failing and there was never a report to fall back on. */
  statusUnavailable?: boolean;
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
  // A status that could not be read for longer than the stale limit. These
  // only ever describe the read, never a failed cluster sync.
  const readStale = !failing && statusStaleSeconds !== undefined;
  const readUnavailable = !failing && !readStale && statusUnavailable;
  // The current health read outranks progress cached from an older stream.
  // A feed that is still opening is not a disconnection: the hook reports
  // "reconnecting" once a connection attempt fails or stays silent too long.
  // The status is a live region, announced on a change of state only. A label
  // that carries a ticking age is shown as is but hidden from assistive
  // technology, and a stable spoken form stands in for it.
  const staleLine = readStale ? `Status may be out of date · last snapshot ${syncDuration(statusStaleSeconds!)} ago` : undefined;
  const ticking = !failing && !readStale && !readUnavailable && state === "ready" && !renewing
    && !(syncState === "refreshing" || syncState === "syncing" && firstReadyAt)
    && (streamState === "live" || streamState === "connecting") && ageSeconds !== undefined;
  const showNote = Boolean(refreshNote) && !failing && !readUnavailable;
  // While status reads fail, the state shown is the last one known, not a
  // fresh "synced".
  const spoken = readStale ? "Status may be out of date" : ticking ? showNote ? "Last status known" : "Synced" : undefined;
  const describedBy = useId();
  const label = staleLine ? staleLine
    : readUnavailable ? "Couldn't load sync status — try again"
    : failing ? syncFailureLabel(state, { onScreen: Boolean(firstReadyAt) && dataOnScreen, time: syncDataTime(lastSyncedAt, ageSeconds) })
    : state === "loading" ? "Loading" : renewing ? "Refreshing credentials…"
    : syncState === "refreshing" || syncState === "syncing" && firstReadyAt ? "Syncing"
    : state === "ready"
    ? streamState === "polling" ? "Live updates off — refreshing every 30s"
    : streamState === "live" || streamState === "connecting" ? ageSeconds !== undefined ? `Last synced ${Math.round(ageSeconds)}s ago` : "Last sync time unavailable" : "Updates disconnected — refreshing every 30s"
    : firstReadyAt ? "Syncing" : "Preparing";
  const ink = readStale ? "text-sev-warning-text" : readUnavailable ? "text-ink-300"
    : state === "failed" ? "text-sev-critical-text"
    : state === "degraded" ? "text-sev-warning-text"
      : state === "preparing" ? "text-ink-300"
        : streamState === "live" ? "text-sev-ok-text" : "text-ink-300";
  const dot = readStale ? "bg-sev-warning" : readUnavailable ? "bg-ink-400"
    : state === "failed" ? "bg-sev-critical"
    : state === "degraded" ? "bg-sev-warning"
      : state === "preparing" ? "bg-accent"
        : streamState === "live" ? "bg-sev-ok" : "bg-ink-400";
  const marker = state === "preparing" && !readUnavailable
    ? <Loader2 size={13} aria-hidden="true" className="shrink-0 animate-spin motion-reduce:animate-none" />
    : <span aria-hidden="true" className={`size-1.5 shrink-0 rounded-full ${dot}`} />;
  // The live region is the label: a change of state is announced, and the
  // details trigger inside it carries no extra words to read out.
  // A disconnected record feed still reads as such: that line is about
  // freshness, which namespace access does not change.
  const namespaceView = state === "ready" && !readStale && readableNamespaces !== undefined && !renewing
    && (streamState === "live" || streamState === "connecting" || streamState === "polling") && syncState !== "refreshing";
  // The live region holds only words that change with the state. The ticking
  // label is read on demand through its description, which sits outside the
  // region, and the phone buttons that open details sit beside it, not in it.
  return <><span role="status" aria-live="polite" aria-describedby={spoken ? describedBy : undefined} data-sync-state={readStale ? "degraded" : state} data-namespace-access={namespaceView ? "" : undefined} title={failing || namespaceView ? undefined : title} className={`inline-flex min-w-0 items-center gap-1.5 whitespace-nowrap text-2xs font-medium ${ink}`}>
    {failing
      ? <SyncFailureDetails state={state} label={label} marker={marker} problems={problems} staleSeconds={staleSeconds} onRetry={onRetry} retrying={retrying} coverage={coverageSummary && coverage} />
      : namespaceView
        ? <NamespaceAccessDetails namespaces={readableNamespaces} marker={marker} freshness={streamState === "polling" ? "Live updates are off for namespace access; this view refreshes every 30 seconds." : label} coverage={coverageSummary && coverage} />
        : staleLine
          ? <>{marker}<span data-sync-label title={staleLine} className="min-w-0 truncate">
            <span aria-hidden="true" className="hidden sm:inline">{staleLine}</span>
            <span aria-hidden="true" data-sync-label-short className="sm:hidden">May be out of date</span>
            <span className="sr-only">{spoken}</span>
          </span></>
          : <>{marker}<span data-sync-label title={label} className="min-w-0 truncate">{spoken ? <><span aria-hidden="true">{label}</span><span className="sr-only">{spoken}</span></> : label}</span></>}
    {showNote && <RefreshNoteText note={refreshNote!} retryAt={refreshRetryAt} />}
  </span>
  {spoken && <span id={describedBy} hidden>{label}</span>}
  {staleLine && <StaleDetails line={staleLine} />}
  {showNote && <RefreshNoteDetails note={refreshNote!} retryAt={refreshRetryAt} />}
  {!failing && !namespaceView && state !== "loading" && coverageSummary && <ReadCoverageDetails summary={coverageSummary}>{coverage}</ReadCoverageDetails>}</>;
}

/**
 * The full stale line on a phone, where the status shows only "May be out of
 * date": a button beside the status opens it on hover, focus or tap. It says
 * the status may be out of date, never that the data is too old.
 */
function StaleDetails({ line }: { line: string }) {
  const { open, triggerProps, panelProps } = useAnchoredPopover({ bridgeFocus: true });
  return <>
    <button {...triggerProps} type="button" aria-label="Status details" title={line} data-sync-stale-trigger
      className="inline-flex size-8 shrink-0 items-center justify-center rounded-control hover:bg-ink-700 focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent sm:hidden">
      <Info size={13} aria-hidden="true" />
    </button>
    {open && createPortal(<div {...panelProps} role="region" aria-label="Status details" data-sync-stale-details
      className="fixed overflow-auto whitespace-normal rounded-control border border-ink-600 bg-surface-raised p-3 text-xs text-ink-200 shadow-overlay">
      {line}
    </div>, document.body)}
  </>;
}

/** The reader's clock, ticking each second while `active`. */
function useSecondClock(active: boolean): number {
  const [now, setNow] = useState(Date.now);
  useEffect(() => {
    if (!active) return;
    setNow(Date.now());
    const timer = window.setInterval(() => setNow(Date.now()), 1_000);
    return () => window.clearInterval(timer);
  }, [active]);
  return active ? now : Date.now();
}

/**
 * Why the status shown is not newer, inside the live region: the cause is
 * spoken after a separator at every width; the visible words (from sm up)
 * carry the ticking countdown and are hidden from assistive technology.
 */
function RefreshNoteText({ note: cause, retryAt }: { note: string; retryAt?: number }) {
  const now = useSecondClock(retryAt !== undefined);
  const countdown = retryAt === undefined ? undefined : retryCountdown(retryAt, now);
  return <>
    <span className="sr-only">; {cause}</span>
    <span aria-hidden="true" data-sync-refresh-note title={cause} className="hidden min-w-0 truncate font-normal text-ink-300 sm:inline">· {cause}{countdown && <span data-sync-refresh-countdown> — {countdown}</span>}</span>
  </>;
}

/**
 * Below sm the status already fills the row, so an icon beside the status,
 * outside the live region, names the cause and opens the full note with its
 * countdown on hover, focus or tap. Neither form wraps the top bar.
 */
function RefreshNoteDetails({ note: cause, retryAt }: { note: string; retryAt?: number }) {
  const { open, triggerProps, panelProps } = useAnchoredPopover({ bridgeFocus: true });
  const now = useSecondClock(retryAt !== undefined && open);
  const countdown = retryAt === undefined ? undefined : retryCountdown(retryAt, now);
  return <>
    <button {...triggerProps} type="button" aria-label={cause} title={cause} data-sync-refresh-note-trigger
      className="inline-flex size-8 shrink-0 items-center justify-center rounded-control text-ink-300 hover:bg-ink-700 focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent sm:hidden">
      <CircleAlert size={13} aria-hidden="true" />
    </button>
    {open && createPortal(<div {...panelProps} role="region" aria-label="Status refresh" data-sync-refresh-details
      className="fixed overflow-auto whitespace-normal rounded-control border border-ink-600 bg-surface-raised p-3 text-xs text-ink-200 shadow-overlay">
      {countdown ? `${cause} — ${countdown}` : cause}
    </div>, document.body)}
  </>;
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
