import { describe, expect, it } from "vitest";
import type { KubernetesClusterSummary, KubernetesClusters, KubernetesSyncProgress } from "./api";
import {
  firstSyncListingProgress,
  firstSyncStepIndex,
  firstSyncProgress,
  firstSyncProgressText,
  firstSyncStep,
  firstSyncSteps,
  isPreparing,
  kubernetesCatalogRefreshInterval,
  preparingCatalogRefreshMs,
  settledCatalogRefreshMs,
} from "./kubernetesFirstSync";

const progress = (overrides: Partial<KubernetesSyncProgress> = {}): KubernetesSyncProgress => ({
  step: "listing",
  step_index: 2,
  step_total: 4,
  steps_done: ["connected"],
  counts: { Pod: 3214, Deployment: 412, Namespace: 27, Node: 12, Widget: 3 },
  kinds_done: 4,
  kinds_total: 20,
  started_at: "2026-10-25T08:00:00Z",
  updated_at: "2026-10-25T08:00:30Z",
  ...overrides,
});

const cluster = (health: KubernetesClusterSummary["health"], state = health === "syncing" ? "syncing" : "live"): KubernetesClusterSummary => ({
  id: health,
  health,
  provider: "generic",
  nodes: 3,
  ready_nodes: 3,
  pods: 8,
  running_pods: 8,
  warnings: 0,
  sync: { state, age_s: 0, partial: false },
  observed_at: "2026-10-25T08:00:00Z",
});

const catalog = (clusters: KubernetesClusterSummary[], multiple = true): KubernetesClusters => ({ multiple, clusters });

describe("first sync wording", () => {
  it("runs the four steps in worker order", () => {
    expect(firstSyncSteps.map((step) => step.id)).toEqual(["connected", "listing", "building", "metrics"]);
  });

  it("follows the server step index and falls back to the step id", () => {
    expect(firstSyncStep(progress({ step: "building", step_index: 3 }))?.active).toBe("Building issues and timeline");
    expect(firstSyncStep(progress({ step: "metrics", step_index: 0 }))?.active).toBe("Reading CPU and memory usage");
    expect(firstSyncStep(undefined)).toBeUndefined();
  });

  it("returns progress as parts, not as a joined string", () => {
    expect(firstSyncProgress(progress())).toEqual({
      step: "Listing resources",
      counts: ["Pods 3,214", "Deployments 412", "Namespaces 27"],
    });
  });

  it("names no dot or bullet inside the step or a count", () => {
    const parts = firstSyncProgress(progress())!;
    for (const part of [parts.step, ...parts.counts]) expect(part).not.toMatch(/[.·•]/);
  });

  it("reports nothing before the first progress payload", () => {
    expect(firstSyncProgress(undefined)).toBeUndefined();
  });

  it("reads the same parts as one phrase for assistive tech", () => {
    expect(firstSyncProgressText(progress())).toBe("Listing resources, Pods 3,214, Deployments 412, Namespaces 27");
    expect(firstSyncProgressText(progress(), 2)).toBe("Listing resources, Pods 3,214, Deployments 412");
    expect(firstSyncProgressText(undefined)).toBe("Preparing");
  });

  it("drops a node count and keeps the most telling resource kinds", () => {
    expect(firstSyncProgress(progress(), 2)?.counts).toEqual(["Pods 3,214", "Deployments 412"]);
    expect(firstSyncProgress(progress({ counts: { Node: 12, Widget: 3 } }))?.counts).toEqual(["Widgets 3"]);
  });
});

describe("preparing classification", () => {
  it("reads either first-sync signal as preparing", () => {
    expect(isPreparing({ health: "syncing" })).toBe(true);
    expect(isPreparing({ health: "unknown", sync: { state: "syncing" } })).toBe(true);
    expect(isPreparing({ health: "healthy", sync: { state: "live" } })).toBe(false);
    expect(isPreparing({ health: "unreachable" })).toBe(false);
  });
});

describe("catalog refresh while preparing", () => {
  it("follows a preparing cluster closely and settles to the slow interval", () => {
    expect(kubernetesCatalogRefreshInterval(catalog([cluster("healthy"), cluster("syncing")]))).toBe(preparingCatalogRefreshMs);
    expect(kubernetesCatalogRefreshInterval(catalog([cluster("healthy"), cluster("unreachable")]))).toBe(settledCatalogRefreshMs);
    expect(preparingCatalogRefreshMs).toBe(4_000);
    expect(settledCatalogRefreshMs).toBe(30_000);
  });

  it("treats a stale health with a syncing state as preparing", () => {
    expect(kubernetesCatalogRefreshInterval(catalog([cluster("unknown", "syncing")]))).toBe(preparingCatalogRefreshMs);
  });

  it("does not poll a single-cluster deployment or an unloaded list", () => {
    expect(kubernetesCatalogRefreshInterval(catalog([cluster("syncing")], false))).toBe(false);
    expect(kubernetesCatalogRefreshInterval(catalog([], false))).toBe(false);
    expect(kubernetesCatalogRefreshInterval(undefined)).toBe(false);
  });
});

describe("honest listing progress", () => {
  it("counts resource types without claiming overall startup completion", () => {
    expect(firstSyncListingProgress(progress({ kinds_done: 12, kinds_total: 20 }))).toEqual({ done: 12, total: 20 });
  });
  it.each([
    undefined,
    progress({ step: "unknown" }),
    progress({ kinds_done: 21 }),
    progress({ kinds_done: -1 }),
    progress({ kinds_done: 1.5 }),
    progress({ kinds_total: 0 }),
    progress({ kinds_total: Number.NaN }),
  ])("rejects unknown or incoherent listing counts", (value) => {
    expect(firstSyncListingProgress(value)).toBeUndefined();
  });
  it("retains listing counts during projection and metrics phases", () => {
    expect(firstSyncListingProgress(progress({ step: "building", step_index: 3 }))).toEqual({ done: 4, total: 20 });
    expect(firstSyncListingProgress(progress({ step: "metrics", step_index: 4 }))).toEqual({ done: 4, total: 20 });
  });
  it("does not invent a phase from missing or contradictory worker data", () => {
    expect(firstSyncStepIndex()).toBe(-1);
    expect(firstSyncStepIndex(progress({ step: "unknown" }))).toBe(-1);
    expect(firstSyncStepIndex(progress({ step_index: 3 }))).toBe(-1);
    expect(firstSyncProgress(progress({ step: "unknown" }))).toBeUndefined();
  });
});
