// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { KubernetesSyncHealthSurface } from "@/components/KubernetesSyncHealth";
import { syncHealth } from "@/lib/kubernetesSyncHealth";

const ready = { state: "ready", age_s: 4, partial: false };
const zero = "0001-01-01T00:00:00Z";
const minutesAgo = (value: number) => new Date(Date.now() - value * 60_000).toISOString();

afterEach(() => cleanup());

describe("Sync health surface", () => {
  it("renders nothing while the sync is ready", () => {
    const { container } = render(<KubernetesSyncHealthSurface health={syncHealth({ sync: ready, kinds: { Pod: { state: "ready", observed_at: "2026-10-26T08:30:00Z", records: 4, partial: false } } })} />);
    expect(container.innerHTML).toBe("");
  });

  it("shows the first-sync progress panel while the first sync runs", () => {
    const health = syncHealth({ sync: { state: "syncing", age_s: 0, partial: false, progress: { step: "listing", step_index: 2, step_total: 4, steps_done: ["connected"], counts: { Pod: 12 }, kinds_done: 1, kinds_total: 4, started_at: "2026-10-26T08:00:00Z", updated_at: "2026-10-26T08:00:04Z" } } });
    render(<KubernetesSyncHealthSurface health={health} cluster="production" />);
    expect(screen.getByRole("region", { name: "Preparing your cluster" })).toBeTruthy();
    expect(document.querySelector('[data-sync-health="preparing"]')).toBeTruthy();
  });

  it("says resource types are loading only while the cycle is progressing", () => {
    const health = syncHealth({ sync: { ...ready, pending_kinds: 7 } });
    render(<KubernetesSyncHealthSurface health={health} />);
    const block = document.querySelector('[data-sync-health="preparing"]')!;
    expect(block.textContent).toContain("Preparing");
    expect(block.querySelectorAll("[data-step]")).toHaveLength(4);
    expect(Array.from(block.querySelectorAll("[data-step]"), row => row.getAttribute("data-state"))).toEqual(["waiting", "waiting", "waiting", "waiting"]);
    expect(block.querySelector('[aria-current="step"]')).toBeNull();
    const bar = screen.getByRole("progressbar", { name: "Waiting for preparation progress" });
    expect(block.querySelectorAll("[role=progressbar]")).toHaveLength(1);
    expect(bar.hasAttribute("aria-valuenow")).toBe(false);
    expect(block.textContent).not.toMatch(/under a minute|Step 1|ETA|%/);
    expect(block.querySelector("[data-sync-problem]")).toBeNull();
  });

  it("names a kind that dropped records, with the reason, the skipped count and since when", () => {
    const onRetry = vi.fn();
    const health = syncHealth({
      sync: { state: "partial", age_s: 0, partial: true },
      kinds: {
        Pod: { state: "ready", observed_at: "2026-10-26T08:30:00Z", records: 507, partial: false },
        Node: { state: "partial", records: 12, partial: true, error: "record_rejected", skipped: 3, observed_at: minutesAgo(3) },
      },
    });
    render(<KubernetesSyncHealthSurface health={health} onRetry={onRetry} cluster="vikki-mobile-dev-cluster" />);
    const block = document.querySelector('[data-sync-health="degraded"]')!;
    expect(screen.getByRole("heading", { name: "Cannot finish syncing this cluster" })).toBeTruthy();
    expect(block.textContent).toContain("Degraded");
    expect(block.textContent).toContain("Some resource types did not finish their first sync. Cluster data appears here once they do.");
    expect(block.textContent).not.toContain("stays hidden");
    const problem = block.querySelector('[data-sync-problem]')!;
    expect(problem.getAttribute("data-sync-problem")).toBe("Node");
    expect(problem.textContent).toContain("some objects could not be indexed, everything else is current");
    expect(problem.textContent).toContain("3 objects skipped");
    expect(problem.textContent).toContain("since 3 minutes ago");
    expect(problem.textContent).not.toContain("507");
    expect(block.textContent).not.toMatch(/loading/i);
    fireEvent.click(screen.getByRole("button", { name: "Retry sync" }));
    expect(onRetry).toHaveBeenCalledTimes(1);
  });

  it("says the sync cannot run when a kind's listing errored", () => {
    const health = syncHealth({
      sync: { state: "error", age_s: 0, partial: true },
      kinds: {
        Pod: { state: "error", error: "unauthorized", records: 0, partial: true, observed_at: zero, last_attempt_at: minutesAgo(2) },
        Node: { state: "error", error: "forbidden", records: 0, partial: true, observed_at: zero, last_attempt_at: minutesAgo(2) },
      },
    });
    render(<KubernetesSyncHealthSurface health={health} onRetry={vi.fn()} />);
    const block = document.querySelector('[data-sync-health="failed"]')!;
    expect(screen.getByRole("heading", { name: "Cannot connect to this cluster" })).toBeTruthy();
    expect(block.textContent).toContain("Failed");
    expect(block.textContent).toContain("Versus could not read this cluster yet. Cluster data appears here after the first successful sync.");
    expect(block.textContent).not.toContain("stays hidden");
    expect(screen.getByRole("button", { name: "Retry sync" })).toBeTruthy();
    expect(block.querySelectorAll("[data-sync-problem]")).toHaveLength(2);
    expect(block.textContent).toContain("permission denied");
    // The zero time is never rendered as a date.
    expect(block.textContent).not.toContain("0001");
    expect(block.textContent).not.toMatch(/loading/i);
  });

  it("says how old the snapshot is when the sync is stale", () => {
    const health = syncHealth({ sync: { state: "stale", age_s: 420, partial: true } });
    render(<KubernetesSyncHealthSurface health={health} />);
    const block = document.querySelector('[data-sync-health="degraded"]')!;
    expect(block.textContent).toContain("The last cluster snapshot is 7 minutes old. Cluster data appears here after a fresh sync lands.");
    expect(block.querySelectorAll("[data-sync-problem]")).toHaveLength(0);
  });

  // A cluster that has synced before keeps its pages and their data with no
  // failure section: the top-bar status owns the state, reasons and Retry.
  const published = "2026-10-26T08:00:00Z";
  it.each([
    ["degraded", { state: "partial", age_s: 90, partial: true }],
    ["failed", { state: "error", age_s: 600, partial: true }],
    ["degraded", { state: "stale", age_s: 900, partial: true }],
  ])("renders no in-page section for a published %s sync", (state, sync) => {
    const health = syncHealth({
      sync,
      kinds: state === "failed"
        ? { Secret: { state: "error", error: "forbidden", records: 0, partial: true, observed_at: zero, last_attempt_at: minutesAgo(2) } }
        : { Node: { state: "partial", records: 12, partial: true, error: "record_rejected", skipped: 2, observed_at: minutesAgo(3) } },
      firstReadyAt: published,
    });
    expect(health.state).toBe(state);
    const { container } = render(<KubernetesSyncHealthSurface health={health} onRetry={vi.fn()} />);
    expect(container.innerHTML).toBe("");
    expect(screen.queryByRole("button", { name: "Retry sync" })).toBeNull();
  });

  it("renders the failure panel for a published cluster with nothing on screen", () => {
    const health = syncHealth({
      sync: { state: "error", age_s: 0, partial: true },
      kinds: { Secret: { state: "error", error: "unauthorized", records: 0, partial: true, observed_at: zero, last_attempt_at: minutesAgo(1) } },
      firstReadyAt: published,
    });
    const onRetry = vi.fn();
    render(<KubernetesSyncHealthSurface health={health} onRetry={onRetry} dataOnScreen={false} />);
    expect(screen.getByRole("heading", { name: "Cannot read this cluster right now" })).toBeTruthy();
    const block = document.querySelector('[data-sync-health="failed"]')!;
    expect(block.textContent).toContain("This cluster synced before, but Versus cannot read it now.");
    expect(block.querySelector('[data-sync-problem="Secret"]')?.textContent).toContain("credentials are unavailable or rejected");
    fireEvent.click(screen.getByRole("button", { name: "Retry sync" }));
    expect(onRetry).toHaveBeenCalledTimes(1);
  });
});

it("renders no additional loading surface during a published routine refresh", () => {
  const health = syncHealth({ sync: { state: "syncing", age_s: 30, partial: false }, firstReadyAt: "2026-10-27T08:00:00Z" });
  const { container } = render(<KubernetesSyncHealthSurface health={health} onRetry={vi.fn()} />);
  expect(container.innerHTML).toBe("");
});

it("uses one listing bar measured in types, not overall startup percent", () => {
  const health = syncHealth({ sync: { state: "syncing", age_s: 0, partial: false, progress: { step: "listing", step_index: 2, step_total: 4, steps_done: ["connected"], counts: {}, kinds_done: 12, kinds_total: 20, started_at: "2026-10-27T08:00:00Z", updated_at: "2026-10-27T08:00:01Z" } } });
  const { container } = render(<KubernetesSyncHealthSurface health={health} />);
  expect(container.querySelectorAll("[data-step]")).toHaveLength(4);
  const bar = screen.getByRole("progressbar", { name: "Listing resource types: 12 / 20" });
  expect(container.querySelectorAll("[role=progressbar]")).toHaveLength(1);
  expect(bar.getAttribute("aria-valuenow")).toBe("12");
  expect(bar.getAttribute("aria-valuemax")).toBe("20");
  expect(bar.getAttribute("aria-valuetext")).toBe("12 / 20 resource types checked");
  expect(bar.getAttribute("aria-valuetext")).not.toContain("types listed");
  expect(container.textContent).not.toMatch(/60%|Usually under|ETA/);
  expect(container.querySelector("ol")?.hasAttribute("aria-live")).toBe(false);
});
