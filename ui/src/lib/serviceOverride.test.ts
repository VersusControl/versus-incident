import { describe, expect, it } from "vitest";

import type { ServiceOverride } from "./api";
import {
  logOverrideGate,
  matchSignalGlob,
  overrideMatches,
  resolveOverrideService,
  signalOverrideGate,
} from "./serviceOverride";

function rule(partial: Partial<ServiceOverride>): ServiceOverride {
  return {
    id: "ovr-1",
    source_type: "log",
    match: "",
    service: "payments",
    created_at: "2026-07-01T00:00:00Z",
    ...partial,
  };
}

describe("matchSignalGlob", () => {
  it("matches exact names and rejects mismatches", () => {
    expect(matchSignalGlob("http_5xx", "http_5xx")).toBe(true);
    expect(matchSignalGlob("http_5xx", "http_4xx")).toBe(false);
  });

  it("supports * and ? globs anchored at both ends", () => {
    expect(matchSignalGlob("http_5xx", "http_*")).toBe(true);
    expect(matchSignalGlob("http_5xx", "http_?xx")).toBe(true);
    expect(matchSignalGlob("http_50x", "http_?xx")).toBe(false);
    expect(matchSignalGlob("a.b.c", "a.*.c")).toBe(true);
    expect(matchSignalGlob("a.b.c", "a.b.d")).toBe(false);
  });

  it("never matches blanks", () => {
    expect(matchSignalGlob("", "http_*")).toBe(false);
    expect(matchSignalGlob("http_5xx", "")).toBe(false);
  });
});

describe("overrideMatches", () => {
  it("matches a log rule on the exact pattern id", () => {
    expect(
      overrideMatches(rule({ source_type: "log", match: "p-42" }), {
        sourceType: "log",
        pattern: "p-42",
      }),
    ).toBe(true);
  });

  it("matches a log rule on a message substring", () => {
    expect(
      overrideMatches(rule({ source_type: "log", match: "checkout-svc" }), {
        sourceType: "log",
        pattern: "p-9",
        message: "error in checkout-svc handler",
      }),
    ).toBe(true);
  });

  it("isolates by source type", () => {
    const metricRule = rule({ source_type: "metric", match: "http_5xx" });
    // Same string as a log pattern id must NOT match a metric rule.
    expect(
      overrideMatches(metricRule, { sourceType: "log", pattern: "http_5xx" }),
    ).toBe(false);
    expect(
      overrideMatches(metricRule, { sourceType: "metric", signal: "http_5xx" }),
    ).toBe(true);
  });

  it("matches metric/trace rules by glob", () => {
    expect(
      overrideMatches(rule({ source_type: "trace", match: "GET /orders/*" }), {
        sourceType: "trace",
        signal: "GET /orders/123",
      }),
    ).toBe(true);
  });
});

describe("resolveOverrideService", () => {
  const rules = [
    rule({ id: "a", source_type: "log", match: "p-1", service: "payments" }),
    rule({ id: "b", source_type: "metric", match: "http_*", service: "api" }),
  ];

  it("returns the forced service for the first matching rule", () => {
    expect(
      resolveOverrideService(rules, { sourceType: "log", pattern: "p-1" }),
    ).toBe("payments");
    expect(
      resolveOverrideService(rules, { sourceType: "metric", signal: "http_5xx" }),
    ).toBe("api");
  });

  it("returns undefined when nothing matches", () => {
    expect(
      resolveOverrideService(rules, { sourceType: "log", pattern: "p-99" }),
    ).toBeUndefined();
    expect(resolveOverrideService(undefined, { sourceType: "log" })).toBeUndefined();
  });
});

describe("gates", () => {
  it("logOverrideGate needs only RBAC (OSS capability)", () => {
    expect(logOverrideGate(true)).toBe("editable");
    expect(logOverrideGate(false)).toBe("readonly");
  });

  it("signalOverrideGate is enterprise-gated and fails closed", () => {
    expect(signalOverrideGate({ licensed: false, canManage: true })).toBe("absent");
    expect(signalOverrideGate({ licensed: true, canManage: false })).toBe("readonly");
    expect(signalOverrideGate({ licensed: true, canManage: true })).toBe("editable");
  });
});
