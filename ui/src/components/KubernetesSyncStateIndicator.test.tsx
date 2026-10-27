// @vitest-environment jsdom
import { cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { KubernetesSyncStateIndicator } from "@/components/KubernetesSyncStateIndicator";

afterEach(() => cleanup());

describe("Sync state indicator", () => {
  it.each([
    ["ready", "Live (4s)"],
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
    ["connecting", "Connecting"],
    ["reconnecting", "Reconnecting"],
    ["resyncing", "Resyncing"],
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
