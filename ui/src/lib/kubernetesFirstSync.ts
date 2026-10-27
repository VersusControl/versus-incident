import type { KubernetesClusters, KubernetesSyncProgress } from "./api";

/**
 * Anything that reports the two first-sync signals. The cluster summary sets
 * `health` to "syncing" while the first sync runs, but a cached health can lag
 * behind the freshness envelope, so the sync state counts as well. Every
 * surface asks this one predicate so the two signals can never disagree.
 */
export interface PreparingSignals {
  health: string;
  sync?: { state: string };
}

export function isPreparing(cluster: PreparingSignals): boolean {
  return cluster.health === "syncing" || cluster.sync?.state === "syncing";
}

// The four first-sync steps, in the order the background worker runs them.
// Usage is read after the first generation publishes, so the preparing panel
// usually disappears while "building" is still the current step.
export const firstSyncSteps = [
  { id: "connected", label: "Connected to the cluster", active: "Connecting to the cluster" },
  { id: "listing", label: "Listing resources", active: "Listing resources" },
  { id: "building", label: "Building issues and timeline", active: "Building issues and timeline" },
  { id: "metrics", label: "Reading CPU and memory usage", active: "Reading CPU and memory usage" },
] as const;

export type FirstSyncStep = (typeof firstSyncSteps)[number];

// Kinds worth naming in a live count, most telling first. Untracked kinds keep
// their alphabetical place after these.
const countOrder = ["Pod", "Deployment", "Namespace", "StatefulSet", "DaemonSet", "Job", "Node"];

export const firstSyncCount = (value: number) => value.toLocaleString("en-US");

const plural = (kind: string) => (kind.endsWith("s") ? `${kind}es` : kind.endsWith("y") ? `${kind.slice(0, -1)}ies` : `${kind}s`);

/** One count as a phrase, e.g. "Pods 3,214". */
export const firstSyncCountLabel = (kind: string, value: number) => `${plural(kind)} ${firstSyncCount(value)}`;

// Zero-based index of the step currently running. The server index never
// decreases; fall back to the step id when an older payload lacks it.
export function firstSyncStepIndex(progress?: KubernetesSyncProgress) {
  if (!progress) return 0;
  const byIndex = Number.isInteger(progress.step_index) ? progress.step_index - 1 : -1;
  const index = byIndex >= 0 && byIndex < firstSyncSteps.length ? byIndex : firstSyncSteps.findIndex((step) => step.id === progress.step);
  return index < 0 ? 0 : index;
}

/** The step currently running, carrying both its finished and in-progress wording. */
export function firstSyncStep(progress?: KubernetesSyncProgress): FirstSyncStep {
  return firstSyncSteps[firstSyncStepIndex(progress)];
}

/** Record counts listed so far, in display order, dropping values that are not real counts. */
export function firstSyncCounts(progress?: KubernetesSyncProgress) {
  return Object.entries(progress?.counts ?? {})
    .filter(([, value]) => Number.isFinite(value) && value >= 0)
    .map(([kind, value]) => [kind, value] as const)
    .sort(([left], [right]) => {
      const a = countOrder.indexOf(left), b = countOrder.indexOf(right);
      return (a < 0 ? countOrder.length : a) - (b < 0 ? countOrder.length : b) || left.localeCompare(right);
    });
}

/**
 * Counts worth showing on a compact surface, most telling first. Node counts
 * are a cluster fact rather than a listed resource type, so they are dropped
 * here and reported separately by the preparing panel.
 */
export function firstSyncCountParts(progress?: KubernetesSyncProgress, limit = 3) {
  return firstSyncCounts(progress)
    .filter(([kind]) => kind !== "Node")
    .slice(0, limit)
    .map(([kind, value]) => firstSyncCountLabel(kind, value));
}

/**
 * Progress as parts rather than prose: the running step and its live counts.
 * Surfaces lay these out as separate elements, so nothing has to be split back
 * apart and no separator character is rendered between them.
 */
export interface FirstSyncProgressParts {
  step: string;
  counts: string[];
}

export function firstSyncProgress(progress?: KubernetesSyncProgress, limit = 3): FirstSyncProgressParts | undefined {
  if (!progress) return undefined;
  return { step: firstSyncStep(progress).active, counts: firstSyncCountParts(progress, limit) };
}

/**
 * The same parts as one phrase for assistive tech. A visual line separates the
 * chips with spacing alone, which would read back as one run-on word, so the
 * accessible name uses commas as pauses instead.
 */
export function firstSyncProgressText(progress?: KubernetesSyncProgress, limit = 3): string {
  const parts = firstSyncProgress(progress, limit);
  return parts ? [parts.step, ...parts.counts].join(", ") : "Preparing";
}

// The cluster list is cached for half a minute once every cluster has settled,
// but a first sync reports new counts every few seconds, so the list follows it
// closely until the cluster it is waiting on stops preparing.
export const preparingCatalogRefreshMs = 4_000;
export const settledCatalogRefreshMs = 30_000;

/** How often the cluster list should refresh, given what it currently shows. */
export function kubernetesCatalogRefreshInterval(data?: KubernetesClusters): number | false {
  if (!data?.multiple) return false;
  return data.clusters.some(isPreparing) ? preparingCatalogRefreshMs : settledCatalogRefreshMs;
}
