// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { CoverageWarningLine, SummaryCard, SummaryNumber } from "./SummaryCard";

afterEach(cleanup);

const wrap = (ui: React.ReactElement) => render(<MemoryRouter>{ui}</MemoryRouter>);

describe("Shared summary card", () => {
  it("opens coverage from the warning line's Details link", () => {
    wrap(<CoverageWarningLine warning={{ not_read: 2 }} />);
    expect(screen.getByText("Data not complete: 2 sources not read")).toBeTruthy();
    expect(screen.getByRole("link", { name: "Details ›" }).getAttribute("href")).toBe("/aws/coverage");
  });

  it("draws no warning line when nothing is missing", () => {
    const { container } = wrap(<CoverageWarningLine warning={{ not_read: 0 }} />);
    expect(container.querySelector("[data-warning-line]")).toBeNull();
  });

  it("links a number to its page, but never a number that was not read", () => {
    wrap(
      <SummaryCard>
        <dl>
          <SummaryNumber k="a" label="Act now" words={{ value: "3", state: "read" }} to="/aws/issues" />
          <SummaryNumber k="b" label="Worth a look" words={{ value: "—", state: "not_read" }} to="/aws/issues" />
        </dl>
      </SummaryCard>,
    );
    expect(screen.getByRole("link", { name: "3" }).getAttribute("href")).toBe("/aws/issues");
    expect(document.querySelector('[data-number="b"] a')).toBeNull();
    expect(document.querySelector('[data-number="b"] dd')?.className).toContain("text-ink-300");
  });

  it("draws a count with no number to stand on as small grey words, not a headline figure", () => {
    wrap(
      <dl>
        <SummaryNumber k="c" label="Changes by people" words={{ value: "Not all read", state: "capped" }} />
      </dl>,
    );
    const dd = document.querySelector('[data-number="c"] dd') as HTMLElement;
    expect(dd.getAttribute("data-words-only")).toBe("true");
    expect(dd.className).toContain("text-xs");
    expect(dd.className).not.toContain("text-2xl");
  });
});
