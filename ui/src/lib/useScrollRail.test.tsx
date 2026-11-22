// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { scrollRailMask, useScrollRail } from "./useScrollRail";

const TAB_WIDTH = 80;
const RAIL_WIDTH = 390;
const tabs = ["Overview", "Workloads", "Issues", "Timeline", "Topology", "Nodes", "Helm", "GitOps", "Traffic", "Agent"];

/**
 * jsdom lays nothing out, so the rail is given a phone-width box and each tab
 * a fixed slot; positions follow the rail's own scrollLeft like a browser.
 */
function layOut(rail: HTMLElement) {
  Object.defineProperty(rail, "clientWidth", { configurable: true, get: () => RAIL_WIDTH });
  Object.defineProperty(rail, "scrollWidth", { configurable: true, get: () => tabs.length * TAB_WIDTH });
  rail.getBoundingClientRect = () => ({ left: 0, right: RAIL_WIDTH, top: 0, bottom: 40, width: RAIL_WIDTH, height: 40, x: 0, y: 0, toJSON: () => ({}) });
  rail.querySelectorAll<HTMLElement>("[role=tab]").forEach((tab, index) => {
    tab.getBoundingClientRect = () => {
      const left = index * TAB_WIDTH - rail.scrollLeft;
      return { left, right: left + TAB_WIDTH, top: 0, bottom: 40, width: TAB_WIDTH, height: 40, x: left, y: 0, toJSON: () => ({}) };
    };
  });
  rail.scrollTo = ((options: ScrollToOptions) => { rail.scrollLeft = options.left ?? rail.scrollLeft; fireEvent.scroll(rail); }) as typeof rail.scrollTo;
}

function Rail({ active }: { active: string }) {
  const rail = useScrollRail<HTMLDivElement>(active);
  return <div ref={(node) => { if (node && !node.dataset.laidOut) { node.dataset.laidOut = "1"; layOut(node); } rail.ref.current = node; }} role="tablist" aria-label="Views" data-start={String(rail.edges.start)} data-end={String(rail.edges.end)}>
    {tabs.map((tab) => <button key={tab} type="button" role="tab" aria-selected={tab === active}>{tab}</button>)}
  </div>;
}

afterEach(() => { cleanup(); vi.restoreAllMocks(); });

describe("scrolling tab rail", () => {
  it("fades only the end that still hides tabs", () => {
    render(<Rail active="Overview" />);
    const rail = screen.getByRole("tablist");
    expect(rail.scrollLeft).toBe(0);
    expect(rail.dataset.start).toBe("false");
    expect(rail.dataset.end).toBe("true");
  });

  it("scrolls the active tab into view and fades both ends in the middle", () => {
    const { rerender } = render(<Rail active="Overview" />);
    const rail = screen.getByRole("tablist");
    act(() => { rerender(<Rail active="Topology" />); });
    const topology = screen.getByRole("tab", { name: "Topology" }).getBoundingClientRect();
    // Clear of the 32px edge fade on both sides, not merely inside the rail.
    expect(topology.left).toBeGreaterThanOrEqual(32);
    expect(topology.right).toBeLessThanOrEqual(RAIL_WIDTH - 32);
    expect(rail.scrollLeft).toBeGreaterThan(0);
    expect(rail.dataset.start).toBe("true");
    expect(rail.dataset.end).toBe("true");
  });

  it("drops the end fade once the last tab is fully shown", () => {
    const { rerender } = render(<Rail active="Overview" />);
    const rail = screen.getByRole("tablist");
    act(() => { rerender(<Rail active="Agent" />); });
    act(() => { rail.scrollLeft = tabs.length * TAB_WIDTH - RAIL_WIDTH; fireEvent.scroll(rail); });
    expect(screen.getByRole("tab", { name: "Agent" }).getBoundingClientRect().right).toBeLessThanOrEqual(RAIL_WIDTH);
    expect(rail.dataset.start).toBe("true");
    expect(rail.dataset.end).toBe("false");
  });

  it("masks with alpha only, so the fade works on any theme background", () => {
    expect(scrollRailMask({ start: false, end: false })).toBeUndefined();
    const both = scrollRailMask({ start: true, end: true })!;
    expect(both).toContain("transparent 0");
    expect(both).toContain("transparent 100%");
    expect(both).not.toMatch(/rgb|#[0-9a-f]{6}|var\(/i);
    expect(scrollRailMask({ start: false, end: true })).not.toContain("transparent 0");
    expect(both).toContain("#000 32px");
    expect(both).toContain("calc(100% - 32px)");
  });
});
