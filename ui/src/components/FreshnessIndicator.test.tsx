// @vitest-environment jsdom
import { cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { FreshnessIndicator } from "./FreshnessIndicator";

afterEach(cleanup);

function words(container: HTMLElement) {
  return container.querySelector("[data-freshness-state]")?.textContent;
}

describe("FreshnessIndicator", () => {
  it("writes the live state with its age", () => {
    const { container } = render(<FreshnessIndicator state="live" ageSeconds={12} />);
    expect(words(container)).toBe("Live (12s)");
    expect(container.querySelector("[role='status']")).toBeTruthy();
  });

  it("says a stale reading is retrying instead of implying freshness", () => {
    const { container } = render(<FreshnessIndicator state="stale" ageSeconds={360} />);
    expect(words(container)).toBe("Updated 6 min ago, retrying");
  });

  it("names the disconnected state without an age claim", () => {
    const { container } = render(<FreshnessIndicator state="disconnected" ageSeconds={9} />);
    expect(words(container)).toBe("Disconnected");
  });

  it("tones each state with the severity scale and keeps the dot decorative", () => {
    const { container, rerender } = render(<FreshnessIndicator state="live" ageSeconds={3} />);
    expect(container.querySelector("[data-freshness-state]")?.className).toContain("text-sev-ok");
    expect(container.querySelector("[aria-hidden='true']")?.className).toContain("bg-sev-ok");

    rerender(<FreshnessIndicator state="stale" ageSeconds={90} />);
    expect(container.querySelector("[data-freshness-state]")?.className).toContain("text-sev-warn");

    rerender(<FreshnessIndicator state="disconnected" />);
    expect(container.querySelector("[data-freshness-state]")?.className).toContain("text-sev-critical");
  });

  it("never leaves the dot alone when the age is unknown", () => {
    const { container } = render(<FreshnessIndicator state="live" />);
    expect(words(container)).toBe("Live");
    expect(container.querySelector("[data-freshness-state]")?.className).not.toContain("hidden");
  });
});
