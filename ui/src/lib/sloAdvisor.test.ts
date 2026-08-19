import { describe, expect, it } from "vitest";

import { ApiError } from "@/lib/api";
import type {
  SLOBurnAlert,
  SLORecommendation,
  SLORecommendationSLI,
} from "@/lib/api";
import {
  adoptedDetailFor,
  adoptionTypeOf,
  budgetTone,
  burnAlertFraming,
  cadenceDirty,
  clampPercent,
  DEFAULT_OFF_REASON,
  enableToggleState,
  formatAdoptAdjustment,
  formatBudgetMinutes,
  formatBurnAlert,
  formatBurnAlertDetail,
  formatConfidence,
  formatConsumed,
  formatErrorBudget,
  formatEvidence,
  formatGoDuration,
  formatHeadroom,
  formatNotAdoptable,
  formatObjective,
  formatObjectiveHuman,
  formatObserved,
  formatObservedP99,
  formatRatioPct,
  formatThresholdResync,
  formatWindowDays,
  isLockedStatus,
  latencyObjectiveRatio,
  latencyThresholdMs,
  MANUAL_ADOPTION_NOTE,
  normalizeCadence,
  pickConfidence,
  priorityBand,
  priorityLabel,
  sortByPriority,
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

// latencySLI is the contract the platform now sends for a latency indicator:
// a compliance RATIO measured against a discovered millisecond threshold.
function latencySLI(
  over: Partial<SLORecommendationSLI> = {},
): SLORecommendationSLI {
  return sli({
    name: "Checkout latency",
    type: "latency",
    signal: "http_request_duration_p99",
    objective: 0.99,
    objective_ratio: 0.99,
    threshold_ms: 250,
    window_days: 30,
    ...over,
  });
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
  it("renders a latency objective as its compliance percentage", () => {
    expect(formatObjective(latencySLI())).toBe("99%");
    expect(formatObjective(latencySLI({ objective_ratio: 0.995 }))).toBe(
      "99.5%",
    );
  });
  it("still renders an older server's bare millisecond target as ms", () => {
    expect(
      formatObjective(
        sli({
          type: "latency",
          objective: 250.4,
          objective_ratio: undefined,
          threshold_ms: undefined,
        }),
      ),
    ).toBe("250 ms");
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

describe("latencyThresholdMs / latencyObjectiveRatio", () => {
  it("reads the discovered threshold and the compliance ratio", () => {
    expect(latencyThresholdMs(latencySLI())).toBe(250);
    expect(latencyObjectiveRatio(latencySLI())).toBe(0.99);
  });
  it("falls back to `objective` when it carries the ratio itself", () => {
    const s = latencySLI({ objective: 0.995, objective_ratio: undefined });
    expect(latencyObjectiveRatio(s)).toBe(0.995);
  });
  it("reads an older server's millisecond target out of `objective`", () => {
    const s = sli({ type: "latency", objective: 250, threshold_ms: undefined });
    expect(latencyThresholdMs(s)).toBe(250);
    expect(latencyObjectiveRatio(s)).toBeNull();
  });
  it("is null when the field is absent or unusable", () => {
    expect(latencyThresholdMs(latencySLI({ threshold_ms: 0 }))).toBeNull();
    expect(
      latencyObjectiveRatio(
        latencySLI({ objective: 250, objective_ratio: undefined }),
      ),
    ).toBeNull();
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

  it("treats a merely reformatted value as unchanged", () => {
    expect(cadenceDirty("36h 30m", "36h30m0s")).toBe(false);
    expect(cadenceDirty("36h30m", "36h30m0s")).toBe(false);
    expect(cadenceDirty("24h", "24h0m0s")).toBe(false);
    expect(cadenceDirty("36h 31m", "36h30m0s")).toBe(true);
  });
});

describe("normalizeCadence", () => {
  it("strips the display whitespace Go's parser rejects", () => {
    expect(normalizeCadence("36h 30m")).toBe("36h30m");
    expect(normalizeCadence("  2h 15m 30s ")).toBe("2h15m30s");
    expect(normalizeCadence("24h")).toBe("24h");
    expect(normalizeCadence("")).toBe("");
    expect(normalizeCadence(undefined as unknown as string)).toBe("");
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

// --- enriched-SLI presentation ---------------------------------------------
// Each helper below backs one line of the redesigned SLI block. The absent
// cases matter as much as the present ones: an older server sends none of
// these fields and every helper must answer null so the page drops the line
// instead of rendering an em dash.

describe("formatRatioPct", () => {
  it("trims trailing zeros without eating integer zeros", () => {
    expect(formatRatioPct(0.9987)).toBe("99.87%");
    expect(formatRatioPct(0.9)).toBe("90%");
    expect(formatRatioPct(0.072)).toBe("7.2%");
    expect(formatRatioPct(0.03)).toBe("3%");
    expect(formatRatioPct(1)).toBe("100%");
  });
});

describe("formatWindowDays", () => {
  it("pluralises the accounting window", () => {
    expect(formatWindowDays(30)).toBe("30 days");
    expect(formatWindowDays(1)).toBe("1 day");
  });
});

describe("formatObjectiveHuman", () => {
  it("reads a ratio target the way an SRE says it", () => {
    expect(
      formatObjectiveHuman(sli({ objective: 0.995, window_days: 30 })),
    ).toBe("99.5% over 30 days");
  });
  it("reads a latency target as a compliance ratio against a threshold", () => {
    expect(formatObjectiveHuman(latencySLI())).toBe(
      "99% of requests under 250 ms over 30 days",
    );
  });
  it("never states the objective in milliseconds when a ratio is known", () => {
    const out = formatObjectiveHuman(latencySLI({ objective: 250 }));
    expect(out).toBe("99% of requests under 250 ms over 30 days");
    expect(out).not.toContain("≤ 250 ms");
  });
  it("ignores a percentile in the signal now the threshold states the target", () => {
    expect(
      formatObjectiveHuman(latencySLI({ signal: "latency_p99" })),
    ).not.toContain("p99");
  });
  it("degrades to the threshold alone when no ratio was sent", () => {
    expect(
      formatObjectiveHuman(
        sli({ type: "latency", objective: 250, window_days: 30 }),
      ),
    ).toBe("under 250 ms over 30 days");
  });
  it("keeps sub-10 ms thresholds off the rounding floor", () => {
    expect(formatObjectiveHuman(latencySLI({ threshold_ms: 2.5 }))).toBe(
      "99% of requests under 2.5 ms over 30 days",
    );
  });
});

describe("formatObserved", () => {
  it("renders current attainment as a percentage", () => {
    expect(formatObserved(sli({ observed: 0.9987 }))).toBe("99.87%");
  });
  it("renders a latency observation as compliance, NOT as milliseconds", () => {
    const out = formatObserved(latencySLI({ observed: 0.9912 }));
    expect(out).toBe("99.12%");
    expect(out).not.toContain("ms");
  });
  it("still renders an older server's millisecond attainment as ms", () => {
    expect(
      formatObserved(sli({ type: "latency", observed: 412.6 })),
    ).toBe("413 ms");
  });
  it("is null when the server sent no observation", () => {
    expect(formatObserved(sli({}))).toBeNull();
    expect(formatObserved(sli({ observed: Number.NaN }))).toBeNull();
  });
});

describe("formatObservedP99", () => {
  it("renders the p99 as supporting evidence", () => {
    expect(formatObservedP99(latencySLI({ observed_p99_ms: 312 }))).toBe(
      "observed p99 312 ms",
    );
  });
  it("is null when the server sent no p99", () => {
    expect(formatObservedP99(latencySLI())).toBeNull();
    expect(formatObservedP99(latencySLI({ observed_p99_ms: 0 }))).toBeNull();
    expect(
      formatObservedP99(latencySLI({ observed_p99_ms: Number.NaN })),
    ).toBeNull();
  });
});

describe("adoptionTypeOf / adoptedDetailFor", () => {
  it("routes each SLI to its own objective slot", () => {
    expect(adoptionTypeOf(latencySLI())).toBe("latency");
    expect(adoptionTypeOf(sli({ type: "availability" }))).toBe("availability");
    expect(adoptionTypeOf(sli({ type: "error_rate" }))).toBe("availability");
  });

  it("keeps the two adoptions independent on one service", () => {
    const avail = sli({ name: "Checkout availability" });
    const lat = latencySLI();
    const rec = {
      adopted: {
        sli: "Checkout availability",
        sli_type: "availability",
        objective: 0.995,
        window_days: 30,
      },
      latency_adopted: {
        sli: "Checkout latency",
        sli_type: "latency",
        objective: 0.99,
        window_days: 30,
        threshold_ms: 250,
      },
    };
    expect(adoptedDetailFor(avail, rec)?.sli_type).toBe("availability");
    expect(adoptedDetailFor(lat, rec)?.sli_type).toBe("latency");
  });

  it("never reads the availability slot for a latency SLI", () => {
    const rec = {
      adopted: {
        sli: "Checkout latency",
        sli_type: "availability",
        objective: 0.995,
        window_days: 30,
      },
    };
    expect(adoptedDetailFor(latencySLI(), rec)).toBeUndefined();
  });

  it("is undefined when the slot names a different indicator or is absent", () => {
    const rec = {
      adopted: {
        sli: "Something else",
        sli_type: "availability",
        objective: 0.995,
        window_days: 30,
      },
    };
    expect(adoptedDetailFor(sli({ name: "x" }), rec)).toBeUndefined();
    expect(adoptedDetailFor(sli({ name: "x" }), undefined)).toBeUndefined();
  });
});

describe("formatAdoptAdjustment", () => {
  it("names the discovered threshold the objective was adopted at", () => {
    expect(
      formatAdoptAdjustment({
        adjusted: { threshold_ms: { from: 300, to: 250, reason: "bucket" } },
      }),
    ).toBe("Adopted at the discovered threshold 250 ms.");
  });
  it("is null when nothing moved", () => {
    expect(formatAdoptAdjustment({ ok: true })).toBeNull();
    expect(formatAdoptAdjustment(undefined)).toBeNull();
    expect(
      formatAdoptAdjustment({ adjusted: { window_days: { to: 30 } } }),
    ).toBeNull();
  });
});

describe("formatThresholdResync", () => {
  const adopted = {
    sli: "Checkout latency",
    sli_type: "latency",
    objective: 0.99,
    window_days: 30,
    threshold_ms: 500,
  };

  it("names the move the platform made", () => {
    expect(
      formatThresholdResync({
        ...adopted,
        threshold_resync: {
          from_ms: 250,
          to_ms: 500,
          at: "2026-08-16T09:00:00Z",
        },
      }),
    ).toBe("threshold re-synced 250 ms → 500 ms");
  });

  it("names only the destination when the origin is missing", () => {
    expect(
      formatThresholdResync({
        ...adopted,
        threshold_resync: {
          from_ms: 0,
          to_ms: 500,
          at: "2026-08-16T09:00:00Z",
        },
      }),
    ).toBe("threshold re-synced to 500 ms");
  });

  it("is null when the server records no re-sync", () => {
    expect(formatThresholdResync(adopted)).toBeNull();
    expect(formatThresholdResync(undefined)).toBeNull();
  });

  it("is null when the re-sync names no threshold moved to", () => {
    expect(
      formatThresholdResync({
        ...adopted,
        threshold_resync: {
          from_ms: 250,
          to_ms: 0,
          at: "2026-08-16T09:00:00Z",
        },
      }),
    ).toBeNull();
  });
});

describe("formatHeadroom", () => {
  it("signs the distance from the objective", () => {
    expect(formatHeadroom(0.37)).toBe("+0.37pp headroom");
    expect(formatHeadroom(-0.3)).toBe("0.3pp below target");
    expect(formatHeadroom(0)).toBe("at target");
  });
  it("is null when absent", () => {
    expect(formatHeadroom(undefined)).toBeNull();
  });
});

describe("formatBudgetMinutes", () => {
  it("turns minutes into human time", () => {
    expect(formatBudgetMinutes(219)).toBe("3h 39m");
    expect(formatBudgetMinutes(45)).toBe("45m");
    expect(formatBudgetMinutes(60)).toBe("1h");
    expect(formatBudgetMinutes(2880)).toBe("2d");
    expect(formatBudgetMinutes(3000)).toBe("2d 2h");
    expect(formatBudgetMinutes(0)).toBe("0m");
  });
  it("is null when absent", () => {
    expect(formatBudgetMinutes(undefined)).toBeNull();
  });
});

describe("formatErrorBudget", () => {
  it("states the allowance per window", () => {
    expect(formatErrorBudget({ ratio: 0.005, minutes: 219 }, 30)).toBe(
      "3h 39m per 30 days",
    );
  });
  it("is null without a budget", () => {
    expect(formatErrorBudget(undefined, 30)).toBeNull();
  });
});

describe("formatConsumed", () => {
  it("never rounds a live burn down to 0%", () => {
    expect(formatConsumed(0.62)).toBe("62% consumed");
    expect(formatConsumed(0.004)).toBe("<1% consumed");
    expect(formatConsumed(0)).toBe("0% consumed");
  });
  it("is null when absent", () => {
    expect(formatConsumed(undefined)).toBeNull();
  });
});

describe("budgetTone / clampPercent", () => {
  it("escalates the tone as the budget drains", () => {
    expect(budgetTone(0.2)).toBe("ok");
    expect(budgetTone(0.5)).toBe("warn");
    expect(budgetTone(0.95)).toBe("bad");
    expect(budgetTone(undefined)).toBe("ok");
  });
  it("clamps the bar width to 0..100", () => {
    expect(clampPercent(0.62)).toBe(62);
    expect(clampPercent(1.5)).toBe(100);
    expect(clampPercent(-1)).toBe(0);
    expect(clampPercent(undefined)).toBe(0);
  });
});

describe("formatGoDuration", () => {
  it("drops the zero components Go always prints", () => {
    expect(formatGoDuration("1h0m0s")).toBe("1h");
    expect(formatGoDuration("5m0s")).toBe("5m");
    expect(formatGoDuration("6h0m0s")).toBe("6h");
    expect(formatGoDuration("30m0s")).toBe("30m");
  });
  it("keeps every non-zero component", () => {
    expect(formatGoDuration("1h30m0s")).toBe("1h 30m");
    expect(formatGoDuration("2h15m30s")).toBe("2h 15m 30s");
    expect(formatGoDuration("1h0m30s")).toBe("1h 30s");
  });
  it("passes an already-clean value through", () => {
    expect(formatGoDuration("1h")).toBe("1h");
    expect(formatGoDuration("5m")).toBe("5m");
    expect(formatGoDuration("500ms")).toBe("500ms");
    expect(formatGoDuration("1.5s")).toBe("1.5s");
  });
  it("returns anything it cannot parse unchanged", () => {
    expect(formatGoDuration("")).toBe("");
    expect(formatGoDuration("soon")).toBe("soon");
    expect(formatGoDuration("1 hour")).toBe("1 hour");
    expect(formatGoDuration("-1h0m0s")).toBe("-1h0m0s");
  });
  it("survives a missing value from a partial payload", () => {
    expect(
      formatGoDuration(undefined as unknown as string),
    ).toBe("");
    expect(formatGoDuration(null as unknown as string)).toBe("");
  });
  it("keeps an all-zero duration legible", () => {
    expect(formatGoDuration("0s")).toBe("0s");
    expect(formatGoDuration("0h0m0s")).toBe("0s");
  });
  it("renders the cadence values the config endpoint returns", () => {
    expect(formatGoDuration("24h0m0s")).toBe("24h");
    expect(formatGoDuration("48h0m0s")).toBe("48h");
    expect(formatGoDuration("168h0m0s")).toBe("168h");
  });
});

describe("formatBurnAlert", () => {
  const alert: SLOBurnAlert = {
    name: "fast",
    long_window: "1h0m0s",
    short_window: "5m0s",
    burn_rate: 14.4,
    bad_ratio_threshold: 0.072,
    budget_pct_per_window: 2,
  };
  it("reads as the sentence an on-call needs", () => {
    expect(formatBurnAlert(alert)).toBe(
      "Fast: >14.4× burn over 1h (bad ratio > 7.2%)",
    );
  });
  it("trims a whole burn rate", () => {
    expect(
      formatBurnAlert({
        ...alert,
        name: "slow",
        long_window: "6h0m0s",
        burn_rate: 6,
        bad_ratio_threshold: 0.03,
      }),
    ).toBe("Slow: >6× burn over 6h (bad ratio > 3%)");
  });
  it("puts the short window and budget slice in the detail", () => {
    expect(formatBurnAlertDetail(alert)).toBe(
      "Confirmed over a 5m short window · burns 2% of the budget per window",
    );
    expect(
      formatBurnAlertDetail({ ...alert, short_window: "30m0s" }),
    ).toContain("a 30m short window");
  });
});

describe("formatNotAdoptable", () => {
  it("prefers the server's plain-language reason", () => {
    expect(
      formatNotAdoptable(
        sli({
          adoptable: false,
          not_adoptable_reason:
            "latency SLIs need a histogram the platform has not discovered yet",
        }),
      ),
    ).toBe("latency SLIs need a histogram the platform has not discovered yet");
  });
  it("falls back to the generic note when the server sent none", () => {
    expect(formatNotAdoptable(sli({ adoptable: false }))).toBe(
      MANUAL_ADOPTION_NOTE,
    );
    expect(
      formatNotAdoptable(sli({ adoptable: false, not_adoptable_reason: "  " })),
    ).toBe(MANUAL_ADOPTION_NOTE);
  });
});

describe("pickConfidence", () => {
  it("prefers the platform's evidence score over the model's confidence", () => {
    expect(
      pickConfidence(
        sli({
          confidence: 0.5,
          evidence: {
            observations: 142,
            confident: true,
            incident_count: 3,
            window_days: 14,
            score: 0.82,
          },
        }),
      ),
    ).toBe(0.82);
  });
  it("falls back to the legacy confidence without evidence", () => {
    expect(pickConfidence(sli({ confidence: 0.5 }))).toBe(0.5);
  });
});

describe("formatEvidence", () => {
  it("summarises what the score is built on", () => {
    expect(
      formatEvidence({
        observations: 142,
        confident: true,
        incident_count: 3,
        window_days: 14,
        score: 0.82,
      }),
    ).toBe("142 observations · confident · 3 incidents/14d");
  });
  it("says so when the signal is still thin", () => {
    expect(
      formatEvidence({
        observations: 9,
        confident: false,
        incident_count: 1,
        window_days: 7,
        score: 0.2,
      }),
    ).toBe("9 observations · still learning · 1 incident/7d");
  });
  it("is null when absent", () => {
    expect(formatEvidence(undefined)).toBeNull();
  });
});

describe("sortByPriority", () => {
  function rec(service: string, priority?: number): SLORecommendation {
    return {
      service,
      generated_at: "2026-08-15T00:00:00Z",
      version: 1,
      summary: "",
      slis: [],
      ...(priority === undefined ? {} : { priority }),
    };
  }
  it("floats the HIGHEST priority to the top and keeps unranked services last", () => {
    const out = sortByPriority([
      rec("c"),
      rec("b", 2),
      rec("a", 3),
      rec("d"),
    ]);
    expect(out.map((r) => r.service)).toEqual(["a", "b", "c", "d"]);
  });
  it("puts the breaching, incident-heavy service first, not last", () => {
    // The server's score is 0.45·incidents + 0.35·breaching + 0.20·traffic, so
    // payments-api leads. Ascending order buries it at the bottom.
    const out = sortByPriority([
      rec("legacy-cron", 0.05),
      rec("checkout", 0.36),
      rec("payments-api", 0.92),
    ]);
    expect(out.map((r) => r.service)).toEqual([
      "payments-api",
      "checkout",
      "legacy-cron",
    ]);
  });
  it("is stable and does not mutate the input (no priority at all)", () => {
    const input = [rec("x"), rec("y"), rec("z")];
    const out = sortByPriority(input);
    expect(out.map((r) => r.service)).toEqual(["x", "y", "z"]);
    expect(input.map((r) => r.service)).toEqual(["x", "y", "z"]);
    expect(out).not.toBe(input);
  });
  it("keeps ties in server order", () => {
    const out = sortByPriority([rec("p", 0.5), rec("q", 0.5), rec("r", 0.9)]);
    expect(out.map((r) => r.service)).toEqual(["r", "p", "q"]);
  });
});

describe("priorityBand / priorityLabel", () => {
  it("bands the raw score", () => {
    expect(priorityBand(0.92)).toBe("high");
    expect(priorityBand(0.6)).toBe("high");
    expect(priorityBand(0.36)).toBe("medium");
    expect(priorityBand(0.05)).toBe("low");
  });
  it("is null without a usable score", () => {
    expect(priorityBand(undefined)).toBeNull();
    expect(priorityBand(Number.NaN)).toBeNull();
  });
  it("calls out the top-ranked service instead of printing a number", () => {
    const top = priorityLabel(0.92, true);
    expect(top?.text).toBe("Adopt first");
    expect(top?.tone).toBe("accent");
    expect(top?.title).toContain("adopt first");
  });
  it("labels the rest by band, never by raw score", () => {
    expect(priorityLabel(0.92, false)?.text).toBe("High priority");
    expect(priorityLabel(0.36, false)?.text).toBe("Medium priority");
    expect(priorityLabel(0.05, false)?.text).toBe("Low priority");
    expect(priorityLabel(0.05, false)?.tone).toBe("default");
  });
  it("renders nothing when the server sent no priority", () => {
    expect(priorityLabel(undefined, false)).toBeNull();
    expect(priorityLabel(undefined, true)).toBeNull();
  });
});

describe("burnAlertFraming", () => {
  const alert: SLOBurnAlert = {
    name: "fast",
    long_window: "1h0m0s",
    short_window: "5m0s",
    burn_rate: 14.4,
    bad_ratio_threshold: 0.072,
    budget_pct_per_window: 2,
  };
  it("claims a page only for an adopted objective", () => {
    const f = burnAlertFraming(
      sli({ burn_alerts: [alert], adoptable: true, adopted: true }),
      false,
    );
    expect(f.mode).toBe("enforced");
    expect(f.label).toBe("Will page you");
    expect(f.note).toContain("raise an incident");
  });
  it("reads the adoption off the recommendation-level object too", () => {
    expect(
      burnAlertFraming(sli({ burn_alerts: [alert], adoptable: true }), true).mode,
    ).toBe("enforced");
  });
  it("conditions the page on adoption when the SLI is only adoptable", () => {
    const f = burnAlertFraming(
      sli({ burn_alerts: [alert], adoptable: true }),
      false,
    );
    expect(f.mode).toBe("on-adopt");
    expect(f.note).toContain("once you adopt");
  });
  it("never claims a page for an SLI the platform cannot enforce", () => {
    const f = burnAlertFraming(
      sli({ burn_alerts: [alert], adoptable: false }),
      false,
    );
    expect(f.mode).toBe("manual");
    expect(f.label).toBe("Alert thresholds");
    expect(f.note).toContain("your own alerting");
    expect(f.note).not.toContain("raise an incident");
  });
  it("honours an explicit enforced flag over the adoption state", () => {
    expect(
      burnAlertFraming(
        sli({
          burn_alerts: [{ ...alert, enforced: false }],
          adoptable: true,
          adopted: true,
        }),
        true,
      ).mode,
    ).toBe("manual");
    expect(
      burnAlertFraming(
        sli({ burn_alerts: [{ ...alert, enforced: true }], adoptable: false }),
        false,
      ).mode,
    ).toBe("enforced");
  });
  it("stays manual against an older server that says nothing at all", () => {
    expect(burnAlertFraming(sli({ burn_alerts: [alert] }), false).mode).toBe(
      "manual",
    );
  });
});
