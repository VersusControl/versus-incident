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
// Usage is optional. The preparing panel stays until the authoritative report
// records a first ready publication, not merely a core-only publication.
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

// Zero-based current worker phase, which can revisit listing while more kinds
// fill in. Completion comes only from steps_done, never from this index.
export function firstSyncStepIndex(progress?: KubernetesSyncProgress) {
  if (!progress) return -1;
  const byIndex = Number.isInteger(progress.step_index) ? progress.step_index - 1 : -1;
  const byStep = firstSyncSteps.findIndex((step) => step.id === progress.step);
  if (byStep < 0 || (byIndex >= 0 && byIndex !== byStep)) return -1;
  return byStep;
}

/** The step currently running, only when the worker identifies a known phase. */
export function firstSyncStep(progress?: KubernetesSyncProgress): FirstSyncStep | undefined {
  return firstSyncSteps[firstSyncStepIndex(progress)];
}

/** Listing counts measure resource types, never a fraction of overall startup. */
export function firstSyncListingProgress(progress?: KubernetesSyncProgress) {
  if (!progress || firstSyncStepIndex(progress) < 0) return undefined;
  const { kinds_done: done, kinds_total: total } = progress;
  if (!Number.isSafeInteger(done) || !Number.isSafeInteger(total) || done < 0 || total <= 0 || done > total) return undefined;
  return { done, total };
}

/** Record counts listed so far, in display order, dropping values that are not real counts. */
export function firstSyncCounts(progress?: KubernetesSyncProgress) {
  return Object.entries(progress?.counts ?? {})
    .filter(([, value]) => Number.isSafeInteger(value) && value >= 0)
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
  const step = firstSyncStep(progress);
  if (!step) return undefined;
  return { step: step.active, counts: firstSyncCountParts(progress, limit) };
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
