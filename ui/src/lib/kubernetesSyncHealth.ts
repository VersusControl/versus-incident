import type { KubernetesFailingKind, KubernetesKindStatus, KubernetesSync, KubernetesSyncHealthReport, KubernetesSyncProgress, KubernetesUnreadableKind } from "./api";
import { pendingKindNames } from "./kubernetesPending";

/**
 * The four states a cluster sync can be in. Every Kubernetes data view reads
 * this one classification, so the wording can never drift between the top bar,
 * the blocking surface and the cluster list.
 */
export type KubernetesSyncHealthState = "ready" | "preparing" | "degraded" | "failed";

/** One resource kind that cannot be used, with the reason in plain words. */
export interface KubernetesKindProblem {
  kind: string;
  /** Backend failure class, kept for the console and for tests. */
  error: string;
  /** What the failure class means, written for an operator rather than for a developer. */
  reason: string;
  /** Records the index dropped and skipped, when the backend counted them. */
  skipped: number;
  /** Records the kind holds after its last listing. */
  records: number;
  /** When the failure starts being counted, when the backend reported a real time. */
  since?: string;
  /**
   * What `since` measures: when the failing listing was observed, or, for a kind
   * that never produced a listing, the start of its last attempt.
   */
  sinceBasis?: "observed" | "attempt";
}

/** A kind the connector identity may not read, with the reason in plain words. */
export interface KubernetesUnreadableSummary {
  kind: string;
  /** Backend failure class, normally "forbidden". */
  error: string;
  reason: string;
}

export interface KubernetesSyncHealth {
  state: KubernetesSyncHealthState;
  /** Names of the kinds that cannot be used. */
  kinds: string[];
  problems: KubernetesKindProblem[];
  /**
   * Kinds the connector identity may not read. A capability gap, not a sync
   * fault: they never change `state` and never close a view, but they are named
   * wherever the data would otherwise look complete.
   */
  unreadable: KubernetesUnreadableSummary[];
  /** Resource types still to be listed, while the cycle is genuinely progressing. */
  loading: string[];
  /** How many resource types the backend still counts as pending. */
  loadingCount: number;
  /** First-sync progress, present only during a cluster's first sync. */
  progress?: KubernetesSyncProgress;
  /** Age of the snapshot when the data would have been stale. */
  staleSeconds?: number;
  /**
   * When this cluster first published a ready generation, once it has. While it
   * is absent the cluster has nothing to show and the blocking surface owns the
   * screen; once it is set the last data stays on screen and the sync state is
   * reported as a strip.
   */
  firstReadyAt?: string;
  /** Age of the data on screen, from the report's own snapshot age. */
  ageSeconds?: number;
}

const defaultFailureReason = "the last sync attempt at this resource type failed";

/** A partial kind whose class was never reported still says what is wrong with it. */
const partialWithoutClass = "it holds only part of what the cluster returned";

const failureReasons: Record<string, string> = {
  record_rejected: "some objects could not be indexed, everything else is current",
  record_limit: "too many objects for the safety limit",
  incomplete_pages: "the listing could not be paged through",
  commit_conflict: "credentials rotated mid-sync, retrying",
  forbidden: "permission denied",
  unauthorized: "permission denied",
  timeout: "the cluster is slow or rate-limiting reads",
  throttled: "the cluster is slow or rate-limiting reads",
  unavailable: "the cluster read failed",
  read_failed: "the cluster read failed",
  budget_exhausted: "the read budget for this sync ran out",
  namespace_scope: "the namespace scope could not be read",
  unsupported: "the cluster does not support this read",
  syncing: "it is still being listed",
};

/** One failure class as a phrase an operator can act on. */
export function kindFailureReason(failure: { error?: string }): string {
  return failureReasons[failure.error ?? ""] ?? defaultFailureReason;
}

/** The zero time the stream emits for a kind that was never observed. */
export function isZeroTime(value?: string | null): boolean {
  if (!value) return true;
  const at = Date.parse(value);
  return !Number.isFinite(at) || new Date(at).getUTCFullYear() <= 1;
}

/** A kind the cluster serves, as opposed to one it answered with "not found". */
const served = (kind: KubernetesKindStatus) => !kind.absent;

/**
 * A kind that makes the sync unusable. It fails when the index reports an
 * error, or when the kind is partial: partial means the listing did not finish
 * cleanly — records were skipped, pages were not all read, or a cycle was cut
 * short — and the gate must not open on data known to be incomplete. A kind
 * with no failure class is still failing; it is the case where the index
 * cannot say why. A kind the cluster does not serve never fails, and neither
 * does one that is only waiting its turn: a pending or warming kind is on its
 * way, not broken, so it is checked before the partial flag.
 */
export function kindFailed(kind: KubernetesKindStatus): boolean {
  if (!served(kind)) return false;
  if (kind.state === "pending" || kind.state === "warming") return false;
  return kind.state === "error" || kind.state === "partial" || kind.partial;
}

/** The kind has not been listed yet, and nothing about it has failed. */
function kindPreparing(kind: KubernetesKindStatus): boolean {
  if (!served(kind) || kindFailed(kind)) return false;
  return kind.state === "warming" || kind.state === "pending" || isZeroTime(kind.observed_at);
}

/**
 * When a failure starts being counted. A listing that was observed is dated by
 * that observation; a kind that never produced one is dated by its last attempt.
 * The zero time the wire uses for "never observed" is never a date.
 */
function failureTime(kind: KubernetesKindStatus): { since?: string; sinceBasis?: "observed" | "attempt" } {
  if (!isZeroTime(kind.observed_at)) return { since: kind.observed_at, sinceBasis: "observed" };
  if (!isZeroTime(kind.last_attempt_at)) return { since: kind.last_attempt_at, sinceBasis: "attempt" };
  return {};
}

/**
 * Kinds the connector identity may not read, named once each. They are reported
 * separately from the kinds that failed, because nothing is wrong with the sync.
 */
function unreadableSummaries(unreadable: readonly KubernetesUnreadableKind[]): KubernetesUnreadableSummary[] {
  const seen = new Set<string>();
  return unreadable
    .filter((entry) => entry?.kind && !seen.has(entry.kind) && Boolean(seen.add(entry.kind)))
    .map((entry) => ({ kind: entry.kind, error: entry.class || "forbidden", reason: kindFailureReason({ error: entry.class || "forbidden" }) }))
    .sort((left, right) => left.kind.localeCompare(right.kind));
}

/**
 * Rows for the kinds that made the sync unusable. A kind the connector may not
 * read is never one of them, and the failure time comes from the report's own
 * failing row when it has one, because that time is kept from the first failure
 * rather than rewritten by every retry.
 */
function kindProblems(
  kinds: Record<string, KubernetesKindStatus>,
  unreadable: ReadonlySet<string>,
  failing: readonly KubernetesFailingKind[],
): KubernetesKindProblem[] {
  const reported = new Map(failing.filter((row) => row?.kind).map((row) => [row.kind, row]));
  return Object.entries(kinds)
    .filter(([name, kind]) => !unreadable.has(name) && kindFailed(kind))
    .map(([name, kind]) => {
      // The kind carries its own stable first-failure time, so the surface can
      // print "since" even when the bounded failing list dropped this row.
      const row = reported.get(name);
      const since = !isZeroTime(kind.failing_since) ? { since: kind.failing_since, sinceBasis: "observed" as const }
        : !isZeroTime(row?.since) ? { since: row!.since, sinceBasis: "observed" as const }
          : failureTime(kind);
      return {
        kind: name,
        error: kind.error || row?.class || "unavailable",
        reason: !kind.error && kind.state !== "error" ? partialWithoutClass : kindFailureReason({ error: kind.error || row?.class }),
        skipped: Math.max(0, kind.skipped ?? row?.skipped ?? 0),
        records: Math.max(0, kind.records ?? row?.records ?? 0),
        ...since,
      };
    })
    .sort((left, right) => left.kind.localeCompare(right.kind));
}

export interface KubernetesSyncSignals {
  /** Aggregate of the cluster-wide sync report, covering every indexed kind. */
  sync?: KubernetesSync;
  /** Per-kind state for every kind the index knows. */
  kinds?: Record<string, KubernetesKindStatus> | null;
  /** Kinds the connector identity may not read; never a sync fault. */
  unreadable?: readonly KubernetesUnreadableKind[] | null;
  /** The report's own failing rows, which carry the stable failure time. */
  failing?: readonly KubernetesFailingKind[] | null;
  /** When this cluster first published a ready generation, if it ever has. */
  firstReadyAt?: string;
}

/**
 * The aggregate state of the cluster-wide sync report, in the envelope shape
 * every freshness consumer already reads. Its state, age and partial flag
 * describe every kind the index knows for the cluster, so one report settles
 * the gate for all views.
 */
export function clusterSyncEnvelope(report?: KubernetesSyncHealthReport): KubernetesSync | undefined {
  if (!report) return undefined;
  return { state: report.state, age_s: report.age_s, partial: report.partial, pending_kinds: report.pending_kinds || undefined };
}

const ready: KubernetesSyncHealth = { state: "ready", kinds: [], problems: [], unreadable: [], loading: [], loadingCount: 0 };

/**
 * The cluster's durable first-publication time, once it has one. The wire
 * sends the zero time for "never", which is not a date and must not be shown as
 * one.
 */
export function firstReadyAt(report?: { first_ready_at?: string }): string | undefined {
  const value = report?.first_ready_at;
  return value && !isZeroTime(value) ? value : undefined;
}

/** The durable cluster facts a failed sync-health read still carries. */
export interface KubernetesSyncFailure {
  firstReadyAt?: string;
  ageSeconds?: number;
}

const noFailure: KubernetesSyncFailure = {};

function record(value: unknown): Record<string, unknown> | undefined {
  return value && typeof value === "object" ? value as Record<string, unknown> : undefined;
}

/**
 * Reads the durable facts out of a failed sync-health answer.
 *
 * A revoked credential makes the report itself unreadable, and the last report
 * that succeeded keeps claiming the cluster is ready while every read fails.
 * The failure body still names when this cluster first published and how old
 * the snapshot on screen is — properties of the cluster, not of the request —
 * so the failure can be named over the data instead of the screen going blank.
 * A body is not trusted to be well formed: a zero time is not a publication and
 * a value that is not a number of seconds is not an age.
 */
export function syncFailureReport(body: unknown): KubernetesSyncFailure {
  const flat = record(body);
  // A handler may nest the payload under `error`; both shapes name the same facts.
  const nested = record(flat?.error);
  const field = (name: string) => nested?.[name] ?? flat?.[name];
  const first = field("first_ready_at");
  const age = field("age_s");
  const failure: KubernetesSyncFailure = {};
  if (typeof first === "string" && !isZeroTime(first)) failure.firstReadyAt = first;
  if (typeof age === "number" && Number.isFinite(age) && age >= 0) failure.ageSeconds = age;
  return Object.keys(failure).length ? failure : noFailure;
}

/**
 * Classifies the cluster-wide report. The blocking group is `failing`; kinds
 * the connector identity may not read are carried alongside it and never move
 * the state, so a least-privilege install opens normally and still says what it
 * cannot show.
 */
export function reportSyncHealth(report?: KubernetesSyncHealthReport, progress?: KubernetesSyncProgress): KubernetesSyncHealth {
  return syncHealth({
    sync: report ? { ...clusterSyncEnvelope(report)!, progress } : undefined,
    kinds: report?.kinds,
    unreadable: report?.unreadable,
    failing: report?.failing,
    firstReadyAt: firstReadyAt(report),
  });
}

/**
 * Classifies a cluster's sync from the freshness envelope and the per-kind
 * index state, with the precedence failed, then degraded, then preparing:
 *
 * - failed: a kind's listing errored, or the envelope reports an error that no
 *   per-kind state explains. Nothing can be trusted, so nothing is shown.
 * - degraded: a kind is partial and names a failure class, or the snapshot is
 *   too old to show. Views stay blocked and the failing kinds are named.
 * - preparing: nothing failed but some kinds are still warming or were never
 *   listed. Only this state may say something is loading.
 * - ready: every served kind is ready and clean, so views show data.
 *
 * A response without the envelope cannot be classified, so it is left as it is
 * rather than blocked on a guess; the backend always sends one.
 */
export function syncHealth({ sync, kinds, unreadable, failing, firstReadyAt: published }: KubernetesSyncSignals): KubernetesSyncHealth {
  const unreadableKinds = unreadableSummaries(unreadable ?? []);
  const withUnreadable = (health: KubernetesSyncHealth): KubernetesSyncHealth => unreadableKinds.length ? { ...health, unreadable: unreadableKinds } : health;
  if (!sync) return withUnreadable(ready);
  const entries = Object.values(kinds ?? {});
  const problems = kindProblems(kinds ?? {}, new Set(unreadableKinds.map((entry) => entry.kind)), failing ?? []);
  // Loading names and the pending count belong to a cycle that is progressing.
  // A failing or stalled cycle has no loading kinds, only failed ones, so the
  // blocking surface can never present a failure as something still on its way.
  const progress = sync.state === "syncing" ? sync.progress : undefined;
  // Having published once is remembered whatever the current state says, and
  // the age travels with it: a strip can only label the data it keeps on screen.
  const publishedAt = published ? { firstReadyAt: published } : {};
  const envelope: Omit<KubernetesSyncHealth, "state"> = { kinds: problems.map((problem) => problem.kind), problems, unreadable: unreadableKinds, loading: [], loadingCount: 0, progress, ageSeconds: sync.age_s, ...publishedAt };
  const progressing = { loading: [...pendingKindNames(sync.pending)], loadingCount: sync.pending_kinds ?? 0 };
  // A snapshot the backend calls stale is shown for what it is, even when the
  // kinds that failed are the reason it aged.
  const stale = sync.state === "stale" ? { staleSeconds: sync.age_s } : {};

  if (problems.length > 0) {
    // A kind whose listing errored is the strongest evidence there is, so it
    // outranks a kind that only dropped records.
    const errored = problems.some((problem) => kinds?.[problem.kind]?.state === "error");
    return { ...envelope, state: errored ? "failed" : "degraded", ...stale };
  }
  if (sync.state === "error") return { ...envelope, state: "failed", ...stale };
  if (sync.state === "stale") return { ...envelope, state: "degraded", ...stale };
  // A report that calls itself partial without naming a kind still cannot be
  // shown: the data is incomplete, so the views stay blocked and say why.
  if (sync.state === "partial" && entries.length === 0) return { ...envelope, state: "degraded", ...stale };
  if (progress || sync.state === "syncing" || entries.some(kindPreparing)) return { ...envelope, ...progressing, state: "preparing" };
  if (progressing.loadingCount > 0 || progressing.loading.length > 0) return { ...envelope, ...progressing, state: "preparing" };
  return withUnreadable({ ...ready, ageSeconds: sync.age_s, ...publishedAt });
}

/** How often a view behind the blocking surface should ask again. */
export function syncPollIntervalMs(sync?: KubernetesSync): number | false {
  if (!sync) return false;
  if (sync.state === "syncing") return 1000;
  if (sync.state === "error" || sync.state === "stale" || sync.state === "partial") return 5000;
  if ((sync.pending_kinds ?? 0) > 0 || sync.pending?.length) return 5000;
  return false;
}

/** The slowest a sync health report is ever re-read once the cluster looks settled. */
export const syncReportIdlePollMs = 15_000;

/**
 * How often to re-read the cluster-wide report. Sync health changes without a
 * user action — a kind can start failing after it was ready, and a blocked
 * cluster can recover — so this is never off: a running sync is followed
 * closely, a blocked one is rechecked within five seconds, and a settled or
 * unreadable one is rechecked within {@link syncReportIdlePollMs}.
 */
export function syncReportPollIntervalMs(sync?: KubernetesSync): number {
  const interval = syncPollIntervalMs(sync);
  return typeof interval === "number" ? interval : syncReportIdlePollMs;
}

/** The four words every surface uses for the one classification. */
export function syncHealthStateLabel(state: KubernetesSyncHealthState): string {
  switch (state) {
    case "ready":
      return "Ready";
    case "preparing":
      return "Preparing";
    case "degraded":
      return "Degraded";
    case "failed":
      return "Failed";
  }
}
