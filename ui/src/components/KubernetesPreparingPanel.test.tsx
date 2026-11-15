// @vitest-environment jsdom
import { cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import type { KubernetesSyncProgress } from "@/lib/api";
import { KubernetesPreparingPanel } from "./KubernetesPreparingPanel";

afterEach(cleanup);

const base: KubernetesSyncProgress = {
  step: "metrics", step_index: 4, step_total: 4, steps_done: ["connected", "building"],
  counts: { Pod: 12 }, kinds_done: 20, kinds_total: 20, started_at: "2026-11-14T10:00:00Z", updated_at: "2026-11-14T10:00:30Z",
};
const states = (container: HTMLElement) => [...container.querySelectorAll("[data-step]")].map((step) => [step.getAttribute("data-step"), step.getAttribute("data-state")]);

describe("Preparing your cluster steps", () => {
  it("shows every step before a done or active one as done, even when its own status lags", () => {
    const { container } = render(<KubernetesPreparingPanel progress={{ ...base, phase_status: { listing: "waiting" } }} />);
    expect(states(container)).toEqual([["connected", "done"], ["listing", "done"], ["building", "done"], ["metrics", "active"]]);
    expect(container.querySelector('[data-step="listing"]')?.textContent).not.toContain("waiting");
  });

  it("leaves later steps waiting while an earlier one is active", () => {
    const { container } = render(<KubernetesPreparingPanel progress={{ ...base, step: "listing", step_index: 2, steps_done: ["connected"], kinds_done: 4 }} />);
    expect(states(container)).toEqual([["connected", "done"], ["listing", "active"], ["building", "waiting"], ["metrics", "waiting"]]);
  });
});

describe("Preparing your cluster steps, current step", () => {
  it("keeps the step the worker names as current active even when a later step is done", () => {
    const { container } = render(<KubernetesPreparingPanel progress={{ ...base, step: "listing", step_index: 2, steps_done: ["connected", "building"], kinds_done: 12 }} />);
    expect(states(container)).toEqual([["connected", "done"], ["listing", "active"], ["building", "done"], ["metrics", "waiting"]]);
  });
});
