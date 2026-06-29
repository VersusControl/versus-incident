import { describe, it, expect } from "vitest";
import {
  learnExcludeGate,
  matchesMetricPattern,
  metricExcluded,
  toggleMetricExclusion,
} from "@/lib/learnExclude";

// Pure-logic tests for the Disable-Learn UI (X30-T8). The console has no DOM
// test harness, so the two contracts that matter are pinned here:
//   1. the client-side metric matcher mirrors the server's exact-name AND
//      glob/prefix (`*`/`?`) semantics, so a checkbox reflects the same
//      exclusion decision the agent makes.
//   2. learnExcludeGate fails closed: no controls on an unlicensed (403/404)
//      surface, read-only without runtime:manage, editable only for a licensed
//      admin/owner session.

describe("matchesMetricPattern", () => {
  it("matches an exact signal name and nothing else", () => {
    expect(matchesMetricPattern("up", "up")).toBe(true);
    expect(matchesMetricPattern("upstream", "up")).toBe(false);
    expect(matchesMetricPattern("up", "down")).toBe(false);
  });

  it("matches a `*` prefix/glob pattern", () => {
    expect(matchesMetricPattern("go_goroutines", "go_*")).toBe(true);
    expect(matchesMetricPattern("go_gc_duration_seconds", "go_*")).toBe(true);
    expect(matchesMetricPattern("prometheus_build_info", "prometheus_*")).toBe(
      true,
    );
    // The prefix must actually match — a different prefix does not.
    expect(matchesMetricPattern("node_cpu", "go_*")).toBe(false);
    // The pattern is anchored at both ends, so a bare prefix without `*` is exact.
    expect(matchesMetricPattern("go_goroutines", "go_")).toBe(false);
  });

  it("supports `*` mid/suffix and `?` single-character globs", () => {
    expect(matchesMetricPattern("http_requests_total", "*_total")).toBe(true);
    expect(matchesMetricPattern("http_requests_count", "*_total")).toBe(false);
    expect(matchesMetricPattern("http_5xx", "http_?xx")).toBe(true);
    expect(matchesMetricPattern("http_50x", "http_?xx")).toBe(false);
  });

  it("is case-sensitive (Prometheus metric names are)", () => {
    expect(matchesMetricPattern("Up", "up")).toBe(false);
    expect(matchesMetricPattern("GO_gc", "go_*")).toBe(false);
  });

  it("trims the entry but never matches a blank entry or blank signal", () => {
    expect(matchesMetricPattern("up", "  up  ")).toBe(true);
    expect(matchesMetricPattern("up", "   ")).toBe(false);
    expect(matchesMetricPattern("", "up")).toBe(false);
    expect(matchesMetricPattern("", "*")).toBe(false);
  });

  it("treats regex metacharacters in a non-glob entry literally", () => {
    // A dot is literal, not 'any char' — only an exact dotted name matches.
    expect(matchesMetricPattern("go.gc", "go.gc")).toBe(true);
    expect(matchesMetricPattern("goxgc", "go.gc")).toBe(false);
  });
});

describe("metricExcluded", () => {
  const list = ["up", "go_*", "prometheus_*"];
  it("is true when any exact name or pattern matches", () => {
    expect(metricExcluded("up", list)).toBe(true);
    expect(metricExcluded("go_goroutines", list)).toBe(true);
    expect(metricExcluded("prometheus_build_info", list)).toBe(true);
  });
  it("is false when nothing matches, or the inputs are empty", () => {
    expect(metricExcluded("node_cpu_seconds", list)).toBe(false);
    expect(metricExcluded("up", [])).toBe(false);
    expect(metricExcluded("up", undefined)).toBe(false);
    expect(metricExcluded("", list)).toBe(false);
  });
});

describe("toggleMetricExclusion", () => {
  it("adds the exact signal name when excluding an un-excluded metric", () => {
    expect(toggleMetricExclusion(["up"], "go_goroutines", true)).toEqual([
      "up",
      "go_goroutines",
    ]);
  });

  it("is a no-op when excluding a metric a glob already covers", () => {
    const next = toggleMetricExclusion(["go_*"], "go_goroutines", true);
    expect(next).toEqual(["go_*"]);
  });

  it("drops the exact entry when un-excluding", () => {
    expect(
      toggleMetricExclusion(["up", "go_goroutines"], "go_goroutines", false),
    ).toEqual(["up"]);
  });

  it("drops every matching glob/prefix entry when un-excluding", () => {
    // Un-excluding a metric removes whatever pattern was excluding it — the only
    // honest read-modify-write when a glob covered it.
    expect(
      toggleMetricExclusion(["go_*", "up"], "go_goroutines", false),
    ).toEqual(["up"]);
  });

  it("never mutates the input array", () => {
    const input = ["up"];
    toggleMetricExclusion(input, "go_gc", true);
    expect(input).toEqual(["up"]);
  });
});

describe("learnExcludeGate", () => {
  it("is 'absent' on an unlicensed surface (the /intel 403/404 degrade)", () => {
    expect(learnExcludeGate({ licensed: false, canManage: true })).toBe(
      "absent",
    );
    expect(learnExcludeGate({ licensed: false, canManage: false })).toBe(
      "absent",
    );
  });

  it("is 'readonly' when licensed but without runtime:manage", () => {
    expect(learnExcludeGate({ licensed: true, canManage: false })).toBe(
      "readonly",
    );
  });

  it("is 'editable' only for a licensed runtime:manage session", () => {
    expect(learnExcludeGate({ licensed: true, canManage: true })).toBe(
      "editable",
    );
  });
});
