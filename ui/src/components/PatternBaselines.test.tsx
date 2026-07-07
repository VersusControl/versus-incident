// @vitest-environment jsdom
import { describe, it, expect, afterEach } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";
import { PatternBaselines } from "@/components/PatternBaselines";
import type { SeasonalBucket } from "@/lib/api";

// PatternBaselines renders the three baselines an operator sees on a pattern's
// detail, its peek, and the service-detail peek. These pin that all three modes
// show with their per-mode labels, the standard deviation is derived from the
// variance, the 24 hour-of-day cells all render, an unwarmed (count 0) or
// missing hour reads "—", and an absent number never fabricates a zero.

afterEach(cleanup);

function seasonalWithOneWarmedHour(): SeasonalBucket[] {
  return Array.from({ length: 24 }, (_, h) =>
    h === 0
      ? { mean: 1.5, variance: 0.25, count: 3 }
      : { mean: 0, variance: 0, count: 0 },
  );
}

describe("PatternBaselines", () => {
  it("shows all three baselines with their per-mode labels and derived σ", () => {
    const { container } = render(
      <PatternBaselines
        frequency={12}
        variance={4}
        avg={5}
        seasonal={seasonalWithOneWarmedHour()}
      />,
    );
    const text = container.textContent ?? "";
    expect(text).toContain("Default (global baseline)");
    expect(text).toContain("≈ 12.0/s");
    // std = sqrt(variance) = sqrt(4) = 2.
    expect(text).toContain("± 2.0/s");
    expect(text).toContain("Average (cumulative mean)");
    expect(text).toContain("≈ 5.0/s");
    expect(text).toContain("Time of day (hour-of-day)");
  });

  it("renders all 24 hour cells, showing the warmed hour's rate and — for the rest", () => {
    render(
      <PatternBaselines
        frequency={12}
        variance={4}
        avg={5}
        seasonal={seasonalWithOneWarmedHour()}
      />,
    );
    for (let h = 0; h < 24; h++) {
      const label = `${String(h).padStart(2, "0")}h`;
      expect(screen.getByText(label)).toBeTruthy();
    }
    // The one warmed hour shows its rate; the other 23 read "—".
    expect(screen.getByText("1.5")).toBeTruthy();
    expect(screen.getAllByText("—").length).toBe(23);
  });

  it("reads — for missing numbers instead of fabricating a zero", () => {
    render(<PatternBaselines />);
    // Default + Average facts (2) plus every one of the 24 hour cells → 26.
    expect(screen.getAllByText("—").length).toBe(26);
  });

  it("stacks Default and Average on separate lines (single-column dl)", () => {
    const { container } = render(
      <PatternBaselines
        frequency={12}
        variance={4}
        avg={5}
        seasonal={seasonalWithOneWarmedHour()}
      />,
    );
    const dl = container.querySelector("dl");
    expect(dl).not.toBeNull();
    // Single column: each baseline gets its own line, never a shared 2-up row.
    expect(dl?.className).toContain("grid-cols-1");
    expect(dl?.className).not.toContain("sm:grid-cols-2");
  });
});
