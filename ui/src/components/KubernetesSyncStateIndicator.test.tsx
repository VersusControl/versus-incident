// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { KubernetesSyncStateIndicator } from "@/components/KubernetesSyncStateIndicator";
import type { KubernetesKindProblem } from "@/lib/kubernetesSyncHealth";

afterEach(() => { cleanup(); vi.useRealTimers(); });

const published = "2026-10-27T08:00:00Z";
const hhmm = (iso: string) => new Date(iso).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hourCycle: "h23" });
const minutesAgo = (value: number) => new Date(Date.now() - value * 60_000).toISOString();
const problem = (kind: string, reason: string, since?: string): KubernetesKindProblem => ({ kind, error: "forbidden", reason, skipped: 0, records: 0, since, sinceBasis: "observed" });
const label = () => document.querySelector("[data-sync-state]")?.textContent;

describe("Sync state indicator", () => {
  it.each([
    ["ready", "Last synced 4s ago"],
    ["preparing", "Preparing"],
    ["degraded", "Sync degraded"],
    ["failed", "Sync failed"],
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
    expect(indicator.getAttribute("aria-live")).toBe("polite");
  });
});

it.each([
  ["failed", "Sync failed"],
  ["degraded", "Sync degraded"],
] as const)("dates the data on screen when a published cluster's sync is %s", (state, head) => {
  const lastSyncedAt = "2026-11-02T09:00:00Z";
  render(<KubernetesSyncStateIndicator state={state} firstReadyAt={published} streamState="live" lastSyncedAt={lastSyncedAt} />);
  expect(label()).toBe(`${head} — showing data from ${hhmm(lastSyncedAt)}`);
  expect(document.querySelector("[data-sync-state]")?.className).toContain(state === "failed" ? "text-sev-critical-text" : "text-sev-warning-text");
});

it("says last saved data when a published cluster's data has no known time", () => {
  render(<KubernetesSyncStateIndicator state="failed" firstReadyAt={published} streamState="live" />);
  expect(label()).toBe("Sync failed — showing last saved data");
});

it("does not claim to show data when nothing is on screen", () => {
  render(<KubernetesSyncStateIndicator state="failed" firstReadyAt={published} streamState="live" ageSeconds={60} dataOnScreen={false} />);
  expect(label()).toBe("Sync failed");
});

it("names published preparing as Syncing", () => {
  render(<KubernetesSyncStateIndicator state="preparing" firstReadyAt={published} streamState="live" />);
  expect(label()).toBe("Syncing");
});

it("names credential renewal without claiming a successful live refresh", () => {
  render(<KubernetesSyncStateIndicator state="ready" streamState="live" syncState="refreshing" syncReason="credential_refresh" />);
  expect(label()).toBe("Refreshing credentials…");
});

it("does not describe a credential denial as an active renewal", () => {
  render(<KubernetesSyncStateIndicator state="failed" streamState="live" syncState="error" syncReason="credential_unavailable" />);
  expect(label()).toBe("Sync failed");
});

it("words an unreachable cluster the same way as any other failed sync", () => {
  render(<KubernetesSyncStateIndicator state="degraded" streamState="live" firstReadyAt={published} syncReason="cluster_unreachable" ageSeconds={60} />);
  expect(label()).toMatch(/^Sync degraded — showing data from \d{2}:\d{2}/);
  expect(label()).not.toContain("unreachable");
});

it.each(["syncing", "refreshing"])("does not let stale %s progress conceal a current failure or advance its saved timestamp", syncState => {
  vi.useFakeTimers();
  vi.setSystemTime(new Date("2026-11-02T09:10:00Z"));
  const lastSyncedAt = "2026-11-02T09:00:00Z";
  const props = { state: "failed" as const, streamState: "live" as const, firstReadyAt: lastSyncedAt,
    syncState, syncReason: "request_timeout", lastSyncedAt, ageSeconds: 0 };
  const { container, rerender } = render(<KubernetesSyncStateIndicator {...props} />);
  const expected = `Sync failed — showing data from ${hhmm(lastSyncedAt)}`;
  expect(container.querySelector("[data-sync-state]")?.textContent).toBe(expected);
  vi.advanceTimersByTime(600000);
  rerender(<KubernetesSyncStateIndicator {...props} />);
  expect(container.querySelector("[data-sync-state]")?.textContent).toBe(expected);
});

it("does not mask a current credential denial with stale credential-renewal progress", () => {
  render(<KubernetesSyncStateIndicator state="failed" streamState="live" syncState="refreshing" syncReason="credential_refresh" />);
  expect(label()).toBe("Sync failed");
});

describe("sync failure details", () => {
  it("opens on hover with one line per failing kind and Retry sync", () => {
    const onRetry = vi.fn();
    render(<KubernetesSyncStateIndicator state="failed" firstReadyAt={published} streamState="live"
      problems={[problem("Secret", "permission denied", minutesAgo(5)), problem("Node", "the cluster read failed")]} onRetry={onRetry} />);
    expect(screen.queryByRole("region", { name: "Sync status details" })).toBeNull();
    fireEvent.mouseEnter(screen.getByRole("button", { name: /^Sync failed/ }));
    const details = screen.getByRole("region", { name: "Sync status details" });
    expect(within(details).getByText("Sync failed")).toBeTruthy();
    const rows = details.querySelectorAll("[data-sync-problem]");
    expect(rows).toHaveLength(2);
    expect(rows[0].textContent).toBe("Secret: permission denied · since 5 minutes ago");
    expect(rows[1].textContent).toBe("Node: the cluster read failed");
    fireEvent.click(within(details).getByRole("button", { name: "Retry sync" }));
    expect(onRetry).toHaveBeenCalledTimes(1);
    // The label itself stays the live region; the panel lives outside it.
    expect(document.querySelector("[role=status][data-sync-state]")?.contains(details)).toBe(false);
  });

  it("opens on keyboard focus, lets Tab reach Retry, and closes on Escape back to the trigger", () => {
    render(<>
      <KubernetesSyncStateIndicator state="degraded" firstReadyAt={published} streamState="live" staleSeconds={420} onRetry={vi.fn()} />
      <button type="button">Next control</button>
    </>);
    const trigger = screen.getByRole("button", { name: /^Sync degraded/ });
    act(() => trigger.focus());
    expect(trigger.getAttribute("aria-expanded")).toBe("true");
    const details = screen.getByRole("region", { name: "Sync status details" });
    expect(trigger.getAttribute("aria-controls")).toBe(details.id);
    expect(within(details).getByText("Sync degraded")).toBeTruthy();
    expect(details.textContent).toContain("The last snapshot is 7 minutes old.");
    fireEvent.keyDown(trigger, { key: "Tab" });
    const retry = within(details).getByRole("button", { name: "Retry sync" });
    expect(document.activeElement).toBe(retry);
    fireEvent.keyDown(document, { key: "Escape" });
    expect(screen.queryByRole("region", { name: "Sync status details" })).toBeNull();
    expect(document.activeElement).toBe(trigger);
  });

  it("moves on past the panel to the control after the trigger", () => {
    render(<>
      <KubernetesSyncStateIndicator state="failed" firstReadyAt={published} streamState="live" onRetry={vi.fn()} />
      <button type="button">Next control</button>
    </>);
    const trigger = screen.getByRole("button", { name: /^Sync failed/ });
    act(() => trigger.focus());
    fireEvent.keyDown(trigger, { key: "Tab" });
    const retry = screen.getByRole("button", { name: "Retry sync" });
    fireEvent.keyDown(retry, { key: "Tab" });
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Next control" }));
    expect(screen.queryByRole("region", { name: "Sync status details" })).toBeNull();
  });

  it("scrolls a long list of failing kinds instead of growing past the viewport", () => {
    const many = Array.from({ length: 9 }, (_, index) => problem(`Kind${index}`, "permission denied"));
    render(<KubernetesSyncStateIndicator state="failed" firstReadyAt={published} streamState="live" problems={many} />);
    fireEvent.click(screen.getByRole("button", { name: /^Sync failed/ }));
    const list = screen.getByRole("list", { name: "Resource types that could not sync" });
    expect(list.querySelectorAll("li")).toHaveLength(9);
    expect(list.className).toContain("overflow-y-auto");
    expect(list.getAttribute("tabindex")).toBe("0");
    expect(screen.getByRole("region", { name: "Sync status details" }).className).toContain("overflow-auto");
  });

  it("offers no details trigger while the sync is healthy", () => {
    render(<KubernetesSyncStateIndicator state="ready" streamState="live" ageSeconds={4} />);
    expect(screen.queryByRole("button")).toBeNull();
  });
});
