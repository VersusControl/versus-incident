import { describe, expect, it } from "vitest";

import { ApiError } from "@/lib/api";
import type { SLORecommendationSLI } from "@/lib/api";
import {
  cadenceDirty,
  DEFAULT_OFF_REASON,
  enableToggleState,
  formatConfidence,
  formatObjective,
  isLockedStatus,
} from "@/lib/sloAdvisor";

function sli(over: Partial<SLORecommendationSLI>): SLORecommendationSLI {
  return {
    name: "x",
    type: "availability",
    signal: "s",
    objective: 0.99,
    window_days: 30,
    rationale: "r",
    confidence: 0.5,
    ...over,
  };
}

describe("isLockedStatus", () => {
  it("is true for 403 and 404 (locked / OSS)", () => {
    expect(isLockedStatus(new ApiError(403, "no"))).toBe(true);
    expect(isLockedStatus(new ApiError(404, "absent"))).toBe(true);
  });
  it("is false for other errors", () => {
    expect(isLockedStatus(new ApiError(500, "boom"))).toBe(false);
    expect(isLockedStatus(new ApiError(401, "no session"))).toBe(false);
    expect(isLockedStatus(new Error("net"))).toBe(false);
  });
});

describe("formatObjective", () => {
  it("renders latency as a ms target", () => {
    expect(formatObjective(sli({ type: "latency", objective: 250.4 }))).toBe(
      "250 ms",
    );
  });
  it("renders availability nines as a percentage", () => {
    expect(formatObjective(sli({ type: "availability", objective: 0.999 }))).toBe(
      "99.90%",
    );
    expect(formatObjective(sli({ type: "availability", objective: 0.99 }))).toBe(
      "99%",
    );
    expect(formatObjective(sli({ type: "availability", objective: 0.995 }))).toBe(
      "99.5%",
    );
  });
  it("renders error_rate ratio as a percentage", () => {
    expect(formatObjective(sli({ type: "error_rate", objective: 0.95 }))).toBe(
      "95%",
    );
  });
});

describe("formatConfidence", () => {
  it("renders a 0-1 value as whole percent", () => {
    expect(formatConfidence(0.8)).toBe("80%");
    expect(formatConfidence(undefined)).toBe("0%");
  });
});

describe("cadenceDirty", () => {
  it("is true only when a non-empty draft differs from current", () => {
    expect(cadenceDirty("12h", "24h0m0s")).toBe(true);
    expect(cadenceDirty("24h0m0s", "24h0m0s")).toBe(false);
    expect(cadenceDirty("", "24h0m0s")).toBe(false);
    expect(cadenceDirty("  ", "24h0m0s")).toBe(false);
  });
});

describe("enableToggleState", () => {
  it("disables the toggle and shows the off-reason when AI is OFF", () => {
    const st = enableToggleState(false, false, "configure AI first");
    expect(st.disabled).toBe(true);
    expect(st.reason).toBe("configure AI first");
    expect(st.checked).toBe(false);
  });
  it("falls back to the default off-reason when none is supplied", () => {
    const st = enableToggleState(false, false);
    expect(st.disabled).toBe(true);
    expect(st.reason).toBe(DEFAULT_OFF_REASON);
  });
  it("still reflects a persisted-on feature even while AI is OFF (disabled)", () => {
    const st = enableToggleState(false, true, "off");
    expect(st.disabled).toBe(true);
    expect(st.checked).toBe(true);
  });
  it("is interactive (not disabled) when AI is ON, reflecting the feature flag", () => {
    expect(enableToggleState(true, false)).toEqual({
      checked: false,
      disabled: false,
    });
    expect(enableToggleState(true, true)).toEqual({
      checked: true,
      disabled: false,
    });
  });
});
