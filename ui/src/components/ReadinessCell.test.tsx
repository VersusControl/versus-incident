import { describe, it, expect } from "vitest";
import { renderToStaticMarkup } from "react-dom/server";
import type { Readiness } from "@/lib/api";
import { ReadinessCell } from "./ReadinessCell";

// The cell is pure and stateless, so react-dom/server's static render (no jsdom
// needed) is enough to assert what each Readiness state produces. The cell now
// shows QUANTITATIVE PROGRESS, not a status label: no "Learning" / "Ready" pills
// (those duplicated the Verdict column). We check the visible count, the "how
// much longer" ETA, the honest title caveats, and that the retired labels are
// gone.

function html(readiness?: Readiness): string {
  return renderToStaticMarkup(<ReadinessCell readiness={readiness} />);
}

describe("ReadinessCell", () => {
  it("ready → progress complete (filled bar + check), no 'Ready' label", () => {
    const out = html({ ready: true, seen: 100, needed: 100, rate_per_min: 0 });
    // No status word that mirrors Verdict=known.
    expect(out).not.toContain("Ready");
    expect(out).not.toContain("Learning");
    // Full progress bar + a "learned/reached its target" tooltip.
    expect(out).toContain("width:100%");
    expect(out).toContain("Learned");
    expect(out).toContain("reached its target");
    // The subtle check icon (lucide renders an <svg class="lucide-check">).
    expect(out).toContain("lucide-check");
  });

  it("learning with an ETA → '40 / 100 · ~30m' progress, no 'Learning' prefix", () => {
    const out = html({ ready: false, seen: 40, needed: 100, rate_per_min: 2 });
    // The number IS the message — no "Learning —" prefix.
    expect(out).not.toContain("Learning");
    expect(out).toContain("40 / 100");
    // "how much longer" ETA appended.
    expect(out).toContain("~30m");
    // ETA is caveated as an estimate, not a promise, and reads "to known".
    expect(out).toContain("estimate");
    expect(out).toContain("to known");
    // Progress bar reflects 40%.
    expect(out).toContain("width:40%");
  });

  it("learning with no rate → count only, no ETA, 'no new data' title", () => {
    const out = html({ ready: false, seen: 12, needed: 20, rate_per_min: 0 });
    expect(out).not.toContain("Learning");
    expect(out).toContain("12 / 20");
    expect(out).not.toContain("~"); // no ETA chip
    expect(out).toContain("No new data");
    expect(out).toContain("width:60%");
  });

  it("indeterminate (needed = 0) → seen count + 'no target', no 'Learning' word", () => {
    const out = html({ ready: false, seen: 5, needed: 0, rate_per_min: 0 });
    expect(out).not.toContain("Learning");
    expect(out).toContain("no target");
    expect(out).toContain("5");
    expect(out).not.toContain(" / "); // no "X / N" target shown
    expect(out).toContain("Auto-promotion is off");
  });

  it("absent readiness → degrades to a quiet dash", () => {
    const out = html(undefined);
    expect(out).toContain("—");
    expect(out).not.toContain("pill");
  });
});
