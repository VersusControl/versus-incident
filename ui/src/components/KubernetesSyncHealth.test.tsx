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
    expect(block.textContent).toContain("Listing the resource types this cluster serves");
    expect(screen.getByText("Loading 7 more resource types")).toBeTruthy();
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
    expect(block.textContent).toContain("Cluster sync degraded");
    expect(block.textContent).toContain("Degraded");
    expect(block.textContent).toContain("Some resource types did not finish syncing, so cluster data stays hidden until they do.");
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
    expect(block.textContent).toContain("Cluster sync failed");
    expect(block.textContent).toContain("Failed");
    expect(block.textContent).toContain("The cluster sync cannot run, so cluster data stays hidden until it can.");
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
    expect(block.textContent).toContain("The last cluster snapshot is 7 minutes old, so cluster data stays hidden until a fresh sync lands.");
    expect(block.querySelectorAll("[data-sync-problem]")).toHaveLength(0);
  });

  // K22 S4: from the first publication on, the same states are a strip. The data
  // on screen stays, the state is named, and its age is stated.
  const published = "2026-10-26T08:00:00Z";
  it.each([
    ["preparing", { state: "syncing", age_s: 30, partial: false }, "Cluster sync running"],
    ["degraded", { state: "partial", age_s: 90, partial: true }, "Cluster sync degraded"],
    ["failed", { state: "error", age_s: 600, partial: true }, "Cluster sync failed"],
  ])("reports %s as a strip once the cluster has published", (state, sync, headline) => {
    const health = syncHealth({
      sync,
      kinds: state === "failed"
        ? { Secret: { state: "error", error: "forbidden", records: 0, partial: true, observed_at: zero, last_attempt_at: minutesAgo(2) } }
        : state === "degraded"
          ? { Node: { state: "partial", records: 12, partial: true, error: "record_rejected", skipped: 2, observed_at: minutesAgo(3) } }
          : undefined,
      firstReadyAt: published,
    });
    expect(health.firstReadyAt).toBe(published);
    render(<KubernetesSyncHealthSurface health={health} onRetry={vi.fn()} />);
    // No blocking block, no Preparing panel: a strip instead.
    expect(document.querySelector("[data-sync-health]")).toBeNull();
    expect(screen.queryByRole("region", { name: "Preparing your cluster" })).toBeNull();
    const strip = document.querySelector(`[data-sync-strip="${state}"]`)!;
    expect(strip.textContent).toContain(headline);
    // The age of what is on screen is stated in plain words.
    expect(strip.querySelector("[data-sync-age]")!.textContent).toContain("Showing data from");
    expect(screen.getByRole("button", { name: "Retry sync" })).toBeTruthy();
  });

  it("names a revoked credential in the strip rather than dropping it", () => {
    const health = syncHealth({
      sync: { state: "error", age_s: 120, partial: true },
      kinds: { Secret: { state: "error", error: "forbidden", records: 0, partial: true, observed_at: zero, last_attempt_at: minutesAgo(1) } },
      firstReadyAt: published,
    });
    render(<KubernetesSyncHealthSurface health={health} />);
    const strip = document.querySelector('[data-sync-strip="failed"]')!;
    expect(strip.querySelector('[data-sync-problem="Secret"]')!.textContent).toContain("permission denied");
    expect(strip.textContent).toContain("Showing data from 2 minutes ago.");
    // Nothing in the strip claims the cluster is loading.
    expect(strip.textContent).not.toMatch(/Preparing/);
  });

  it("calls a snapshot from the last few seconds just now, not zero seconds ago", () => {
    const health = syncHealth({ sync: { state: "partial", age_s: 2, partial: true }, kinds: { Node: { state: "partial", records: 1, partial: true, error: "record_rejected", observed_at: minutesAgo(1) } }, firstReadyAt: published });
    render(<KubernetesSyncHealthSurface health={health} />);
    expect(document.querySelector("[data-sync-age]")!.textContent).toContain("Showing data loaded just now.");
  });

  it("counts the resource types a strip leaves out instead of hiding them", () => {
    const health = syncHealth({
      sync: { state: "partial", age_s: 60, partial: true },
      kinds: Object.fromEntries(["A", "B", "C", "D", "E"].map((kind) => [kind, { state: "partial", records: 1, partial: true, error: "record_rejected", observed_at: minutesAgo(1) }])),
      firstReadyAt: published,
    });
    render(<KubernetesSyncHealthSurface health={health} />);
    const strip = document.querySelector("[data-sync-strip]")!;
    expect(strip.querySelectorAll("[data-sync-problem]")).toHaveLength(3);
    expect(strip.textContent).toContain("2 more resource types");
  });
});
