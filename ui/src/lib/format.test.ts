import { describe, it, expect } from "vitest";
import {
  formatNormalValue,
  formatWiggle,
  humanSignal,
} from "@/lib/format";

// These tests pin the plain-language value rendering for the Metrics / Traces
// "what the agent knows" views. The two rules that matter to operators:
//   1. a genuinely-idle signal reads "≈ 0 req/s (idle)", a tiny one
//      "< 0.01 req/s" — never a bare, misleading "0".
//   2. the spread never renders "± 0"; below the smallest step it reads
//      "± <0.01 …". Together these kill the old "0 ± 0 looks broken" cell.

describe("formatNormalValue", () => {
  it("renders an everyday value at ~2 significant figures with its unit", () => {
    expect(formatNormalValue(0.4, "req/s")).toBe("≈ 0.4 req/s");
    expect(formatNormalValue(180, "ms")).toBe("≈ 180 ms");
    expect(formatNormalValue(0.456, "%")).toBe("≈ 0.46%");
  });

  it("hugs the percent sign and spaces every other unit", () => {
    expect(formatNormalValue(2, "%")).toBe("≈ 2%");
    expect(formatNormalValue(2, "ms")).toBe("≈ 2 ms");
  });

  it("renders a unit-less raw gauge as a bare number", () => {
    expect(formatNormalValue(0.4, "")).toBe("≈ 0.4");
  });

  it("marks a genuinely-idle (exactly zero) signal as idle, never bare 0", () => {
    expect(formatNormalValue(0, "req/s")).toBe("≈ 0 req/s (idle)");
  });

  it("floors a tiny non-zero value to '< 0.01' instead of rounding to 0", () => {
    expect(formatNormalValue(0.004, "req/s")).toBe("< 0.01 req/s");
    expect(formatNormalValue(0.0001, "%")).toBe("< 0.01%");
  });

  it("renders a non-finite value as an em dash", () => {
    expect(formatNormalValue(NaN, "ms")).toBe("—");
    expect(formatNormalValue(Infinity, "ms")).toBe("—");
  });
});

describe("formatWiggle", () => {
  it("renders the spread with units in detail form", () => {
    expect(formatWiggle(0.1, "req/s", true)).toBe("± 0.1 req/s");
    expect(formatWiggle(12, "ms", true)).toBe("± 12 ms");
    expect(formatWiggle(5, "%", true)).toBe("± 5%");
  });

  it("drops the unit in compact (list) form", () => {
    expect(formatWiggle(0.1, "req/s", false)).toBe("± 0.1");
  });

  it("never renders a bare '± 0' — a sub-step spread reads '± <0.01'", () => {
    expect(formatWiggle(0, "req/s", true)).toBe("± <0.01 req/s");
    expect(formatWiggle(0.004, "req/s", true)).toBe("± <0.01 req/s");
    expect(formatWiggle(0, "", false)).toBe("± <0.01");
    expect(formatWiggle(0, "", false)).not.toBe("± 0");
  });

  it("treats a non-finite spread as sub-step rather than NaN", () => {
    expect(formatWiggle(NaN, "ms", true)).toBe("± <0.01 ms");
  });
});

describe("idle signal never renders the broken '0 ± 0'", () => {
  it("combines to a readable idle value, not '0 ± 0'", () => {
    const value = formatNormalValue(0, "req/s");
    const wiggle = formatWiggle(0, "req/s", true);
    const combined = `${value}, usually ${wiggle}`;
    expect(combined).not.toContain("0 ± 0");
    expect(combined).toBe("≈ 0 req/s (idle), usually ± <0.01 req/s");
  });
});

describe("humanSignal", () => {
  it("maps the golden-signal names to plain words", () => {
    expect(humanSignal("request_rate")).toBe("request rate");
    expect(humanSignal("error_rate")).toBe("error rate");
    expect(humanSignal("latency_p99")).toBe("p99 latency");
  });

  it("strips a namespace prefix and de-snakes the tail", () => {
    expect(humanSignal("saturation:cpu")).toBe("cpu");
    expect(humanSignal("gauge:mem_used")).toBe("mem used");
    expect(humanSignal("custom_thing")).toBe("custom thing");
  });

  it("renders an empty signal as an em dash", () => {
    expect(humanSignal("")).toBe("—");
  });
});
