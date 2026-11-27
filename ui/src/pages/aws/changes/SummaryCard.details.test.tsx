// @vitest-environment jsdom
import { afterAll, beforeAll, describe, expect, it, vi } from "vitest";
import { cleanup, render } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { ToastProvider } from "@/components/Toast";
import { ChangesSummaryCard } from "./SummaryCard";
import { amendmentStates, browserPassStates, changesStates, NOW } from "./changesFixtures";

// The Changes warning line links to Coverage like every other warning
// line, except while the first read is still running: that is not a gap.

beforeAll(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date(NOW));
});
afterAll(() => vi.useRealTimers());

const states = { ...changesStates(), ...amendmentStates(), ...browserPassStates() };

describe("Changes warning line", () => {
  it.each(Object.keys(states))("%s: Details opens Coverage on a gap, and only there", (name) => {
    const { container } = render(
      <ToastProvider>
        <MemoryRouter>
          <ChangesSummaryCard view={states[name].view} window="24h" custom={false} locked={false} onWindow={() => {}} onShow={() => {}} />
        </MemoryRouter>
      </ToastProvider>,
    );
    const lines = container.querySelectorAll("[data-warning-line]");
    expect(lines.length).toBeLessThanOrEqual(1);
    const details = container.querySelectorAll('a[href="/aws/coverage"]');
    const line = lines[0];
    if (!line || line.textContent?.includes("Reading")) {
      expect(details).toHaveLength(0);
    } else {
      expect(details).toHaveLength(1);
      expect(line.contains(details[0])).toBe(true);
      expect(details[0].textContent).toBe("Details ›");
    }
    cleanup();
  });

  it("covers both kinds of warning line in the fixtures", () => {
    const kinds = new Set<string>();
    for (const name of Object.keys(states)) {
      const { container } = render(
        <ToastProvider>
          <MemoryRouter>
            <ChangesSummaryCard view={states[name].view} window="24h" custom={false} locked={false} onWindow={() => {}} onShow={() => {}} />
          </MemoryRouter>
        </ToastProvider>,
      );
      const line = container.querySelector("[data-warning-line]");
      if (line) kinds.add(line.textContent?.includes("Reading") ? "reading" : "gap");
      cleanup();
    }
    expect([...kinds].sort()).toEqual(["gap", "reading"]);
  });
});
