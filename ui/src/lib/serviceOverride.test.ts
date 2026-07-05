import { describe, expect, it } from "vitest";

import type { ServiceOverride } from "./api";
import {
  assignableServices,
  cellOverrideInput,
  logOverrideGate,
  matchSignalGlob,
  overrideMatches,
  resolveOverrideService,
  resolveServiceCell,
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

describe("assignableServices", () => {
  it("returns every known service except the _unknown fallback, sorted", () => {
    expect(
      assignableServices({ payments: {}, _unknown: {}, api: {}, billing: {} }),
    ).toEqual(["api", "billing", "payments"]);
  });

  it("is empty when there are no assignable targets", () => {
    expect(assignableServices({ _unknown: {} })).toEqual([]);
    expect(assignableServices({})).toEqual([]);
  });

  it("tolerates a missing services map (query still loading)", () => {
    expect(assignableServices(undefined)).toEqual([]);
    expect(assignableServices(null)).toEqual([]);
  });
});

describe("cellOverrideInput", () => {
  it("keys a log cell on the pattern id and a signal cell on the name", () => {
    expect(cellOverrideInput("log", "p-1")).toEqual({
      sourceType: "log",
      pattern: "p-1",
    });
    expect(cellOverrideInput("metric", "http_5xx")).toEqual({
      sourceType: "metric",
      signal: "http_5xx",
    });
    expect(cellOverrideInput("trace", "GET /orders")).toEqual({
      sourceType: "trace",
      signal: "GET /orders",
    });
  });
});

describe("resolveServiceCell", () => {
  const logRules = [
    rule({ id: "a", source_type: "log", match: "p-1", service: "payments" }),
  ];

  it("logs: shows the override target with NO pending chip once the read model has re-pointed", () => {
    // The logs patterns reader re-points on write (createServiceOverride →
    // catalog.RepointService), so by the time the cell reads it the pattern's
    // own Service already == the target: instant, settled, no "(pending)".
    expect(
      resolveServiceCell(logRules, cellOverrideInput("log", "p-1"), "payments"),
    ).toEqual({ service: "payments", pending: false });
  });

  it("metrics/traces: shows the override target IMMEDIATELY, with pending while the signal hasn't caught up", () => {
    // The metrics/traces baseline reader does NOT re-point on write, so the
    // signal still reads its old service — yet the cell shows the target at
    // once (instant feedback, same as logs) and only flags "(pending)".
    const signalRules = [
      rule({ id: "m", source_type: "metric", match: "request_rate", service: "checkout" }),
    ];
    expect(
      resolveServiceCell(
        signalRules,
        cellOverrideInput("metric", "request_rate"),
        "prometheus",
      ),
    ).toEqual({ service: "checkout", pending: true });
  });

  it("logs and metrics/traces resolve the SAME (target, pending) for the same lag — one contract", () => {
    // A log pattern that has NOT yet re-pointed (message-substring match, or a
    // reader that lagged) resolves identically to a lagging metric signal: the
    // target is shown and pending is true. The chip means the same thing on
    // every surface.
    const logLagging = resolveServiceCell(
      logRules,
      cellOverrideInput("log", "p-1"),
      "checkout",
    );
    const signalRules = [
      rule({ id: "m", source_type: "metric", match: "request_rate", service: "payments" }),
    ];
    const metricLagging = resolveServiceCell(
      signalRules,
      cellOverrideInput("metric", "request_rate"),
      "checkout",
    );
    expect(logLagging).toEqual({ service: "payments", pending: true });
    expect(metricLagging).toEqual({ service: "payments", pending: true });
  });

  it("treats a blank / _unknown current as still-pending (shows the target)", () => {
    expect(
      resolveServiceCell(logRules, cellOverrideInput("log", "p-1"), ""),
    ).toEqual({ service: "payments", pending: true });
    expect(
      resolveServiceCell(logRules, cellOverrideInput("log", "p-1"), null),
    ).toEqual({ service: "payments", pending: true });
    expect(
      resolveServiceCell(logRules, cellOverrideInput("log", "p-1"), undefined),
    ).toEqual({ service: "payments", pending: true });
  });

  it("no override: passes the signal's own service through, never pending", () => {
    expect(
      resolveServiceCell(logRules, cellOverrideInput("log", "p-other"), "checkout"),
    ).toEqual({ service: "checkout", pending: false });
    expect(
      resolveServiceCell(logRules, cellOverrideInput("log", "p-other"), null),
    ).toEqual({ service: null, pending: false });
  });

  it("drives a metric/trace glob override too", () => {
    const signalRules = [
      rule({ id: "m", source_type: "metric", match: "http_*", service: "api" }),
    ];
    expect(
      resolveServiceCell(
        signalRules,
        cellOverrideInput("metric", "http_5xx"),
        "_unknown",
      ),
    ).toEqual({ service: "api", pending: true });
    expect(
      resolveServiceCell(
        signalRules,
        cellOverrideInput("metric", "http_5xx"),
        "api",
      ),
    ).toEqual({ service: "api", pending: false });
  });

  it("degrades cleanly with no override data (query still loading)", () => {
    expect(
      resolveServiceCell(undefined, cellOverrideInput("log", "p-1"), "x"),
    ).toEqual({ service: "x", pending: false });
  });
});
