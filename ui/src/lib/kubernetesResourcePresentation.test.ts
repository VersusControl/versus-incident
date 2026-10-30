import { afterEach, describe, expect, it, vi } from "vitest";
import { clock, elapsed, resourceIDs } from "./kubernetesResourcePresentation";

afterEach(() => vi.restoreAllMocks());
import { createPodPalette, resourceRestarts } from "./kubernetesResourceDetail";

describe("Resource presentation values", () => {
  it("keeps unknown observation times unavailable rather than current", () => {
    for (const time of [undefined, "", "invalid", "0001-01-01T00:00:00Z"]) {
      expect(elapsed(time)).toBe("Unavailable");
      expect(clock(time ?? "")).toBe("unavailable");
    }
  });

  it("preserves minute, hour and day formatting without negative future ages", () => {
    const now = Date.parse("2026-10-28T00:00:00Z");
    vi.spyOn(Date, "now").mockReturnValue(now);
    expect(elapsed(new Date(now - 9 * 60000).toISOString())).toBe("9m");
    expect(elapsed(new Date(now - 125 * 60000).toISOString())).toBe("2h 5m");
    expect(elapsed(new Date(now - 1500 * 60000).toISOString())).toBe("1d 1h");
    expect(elapsed(new Date(now + 60000).toISOString())).toBe("0m");
    expect(clock(new Date(now).toISOString())).toBe(new Date(now).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false }));
  });

  it("keeps resource navigation identities stable and unknown kinds unsupported", () => {
    expect(resourceIDs.Pod).toBe("core~v1~pods");
    expect(resourceIDs.Deployment).toBe("apps~v1~deployments");
    expect(resourceIDs.StatefulSet).toBe("apps~v1~statefulsets");
    expect(resourceIDs.HorizontalPodAutoscaler).toBe("autoscaling~v2~horizontalpodautoscalers");
    expect(resourceIDs.NotAKubernetesKind).toBeUndefined();
  });
  it("allocates four distinct Pod colours and keeps them stable across admission and reordering", () => {
    const colour = createPodPalette();
    const names = ["old-a", "old-b", "new-a", "new-b"];
    const initial = names.map(colour);
    expect(new Set(initial).size).toBe(4);
    expect([...names].reverse().map(colour)).toEqual([...initial].reverse());
    colour("later");
    expect(names.map(colour)).toEqual(initial);
  });
  it.each([undefined, "", "unknown", NaN, Infinity, -1, 1.2])("keeps invalid restart count %s unknown", (value) => {
    expect(resourceRestarts({ kind: "Pod", resource_id: "pods", name: "app", summary: { restart_count: value } })).toBeNull();
  });
  it("supports the restart alias and uses actual finite Pod totals", () => {
    const resource = { kind: "Deployment", resource_id: "deployments", name: "app", summary: { restarts: 6 } };
    expect(resourceRestarts(resource)).toBe(6);
    expect(resourceRestarts(resource, { ...resource, truncated: false, pods: [{ name: "a", restart_count: 2 }, { name: "b", restart_count: 3 }] })).toBe(5);
  });
});
