import { describe, expect, it } from "vitest";
import { createPodPalette, resourceRestarts } from "./kubernetesResourceDetail";

describe("Resource presentation values", () => {
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
