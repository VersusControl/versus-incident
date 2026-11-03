// @vitest-environment jsdom
import { cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { KubernetesSyncStateIndicator } from "@/components/KubernetesSyncStateIndicator";

afterEach(() => { cleanup(); vi.useRealTimers(); });

describe("Sync state indicator", () => {
  it.each([
    ["ready", "Last synced 4s ago"],
    ["preparing", "Preparing"],
    ["degraded", "Degraded"],
    ["failed", "Failed"],
  ] as const)("writes out the %s state at every width", (state, expected) => {
    const { container } = render(<KubernetesSyncStateIndicator state={state} streamState="live" ageSeconds={4} />);
    const indicator = container.querySelector("[data-sync-state]")!;
    expect(indicator.getAttribute("data-sync-state")).toBe(state);
    expect(indicator.textContent).toBe(expected);
    // One responsive rule for all four states: no width-scoped hiding, so a
    // narrow viewport never leaves the dot alone.
    const tokens = indicator.className.split(/\s+/);
    expect(tokens).not.toContain("hidden");
    expect(tokens.some((token) => /^(sm|md|lg):(inline|block|flex)$/.test(token))).toBe(false);
  });

  it.each([
    ["connecting", "Updates disconnected — refreshing every 30s"],
    ["reconnecting", "Updates disconnected — refreshing every 30s"],
    ["resyncing", "Updates disconnected — refreshing every 30s"],
  ] as const)("names the %s record feed while the sync is ready", (streamState, expected) => {
    const { container } = render(<KubernetesSyncStateIndicator state="ready" streamState={streamState} ageSeconds={4} />);
    expect(container.querySelector("[data-sync-state]")?.textContent).toBe(expected);
  });

  it("keeps the dot decorative so state is never carried by colour alone", () => {
    const { container } = render(<KubernetesSyncStateIndicator state="degraded" streamState="live" />);
    const indicator = container.querySelector("[data-sync-state]")!;
    const decoration = indicator.querySelector("[aria-hidden='true']");
    expect(decoration).toBeTruthy();
    expect(decoration?.textContent).toBe("");
    expect(indicator.getAttribute("role")).toBe("status");
  });
});

it.each(["preparing", "degraded", "failed"] as const)("names published %s without hiding faults", (state) => {
  render(<KubernetesSyncStateIndicator state={state} firstReadyAt="2026-10-27T08:00:00Z" streamState="live" />);
  expect(document.querySelector("[data-sync-state]")?.textContent).toBe(state === "preparing" ? "Syncing" : state === "degraded" ? "Degraded" : "Failed");
});

it("names credential renewal without claiming a successful live refresh", () => {
  render(<KubernetesSyncStateIndicator state="ready" streamState="live" syncState="refreshing" syncReason="credential_refresh" />);
  expect(document.querySelector("[data-sync-state]")?.textContent).toBe("Refreshing credentials…");
});

it("does not describe a credential denial as an active renewal", () => {
  render(<KubernetesSyncStateIndicator state="failed" streamState="live" syncState="error" syncReason="credential_unavailable" />);
  expect(document.querySelector("[data-sync-state]")?.textContent).toBe("Failed");
});

it("keeps the last committed timestamp visible when the cluster is unreachable", () => {
  render(<KubernetesSyncStateIndicator state="degraded" streamState="live" firstReadyAt="2026-10-27T08:00:00Z" syncReason="cluster_unreachable" ageSeconds={60} />);
  expect(document.querySelector("[data-sync-state]")?.textContent).toContain("Cluster unreachable — showing data from");
});

it.each(["syncing", "refreshing"])("does not let stale %s progress conceal a current unreachable failure or advance its saved timestamp", syncState => {
  vi.useFakeTimers();
  vi.setSystemTime(new Date("2026-11-02T09:10:00Z"));
  const lastSyncedAt = "2026-11-02T09:00:00Z";
  const props = { state: "failed" as const, streamState: "live" as const, firstReadyAt: lastSyncedAt,
    syncState, syncReason: "request_timeout", lastSyncedAt, ageSeconds: 0 };
  const { container, rerender } = render(<KubernetesSyncStateIndicator {...props} />);
  const expected = `Cluster unreachable — showing data from ${new Date(lastSyncedAt).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}`;
  expect(container.querySelector("[data-sync-state]")?.textContent).toBe(expected);
  vi.advanceTimersByTime(600000);
  rerender(<KubernetesSyncStateIndicator {...props} />);
  expect(container.querySelector("[data-sync-state]")?.textContent).toBe(expected);
});

it("does not mask a current credential denial with stale credential-renewal progress", () => {
  render(<KubernetesSyncStateIndicator state="failed" streamState="live" syncState="refreshing" syncReason="credential_refresh" />);
  expect(document.querySelector("[data-sync-state]")?.textContent).toBe("Failed");
});
