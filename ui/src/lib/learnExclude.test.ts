import { describe, it, expect } from "vitest";
import {
  learnExcludeGate,
  listExcludeControlVisible,
  matchesMetricPattern,
  metricExcluded,
  patternExcluded,
  serviceExcluded,
  toggleLogPatternExclusion,
  toggleLogPatternExclusions,
  toggleMetricExclusion,
  toggleMetricExclusions,
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

describe("toggleMetricExclusions (bulk fold, one PUT)", () => {
  it("adds every selected signal in a single new list", () => {
    const next = toggleMetricExclusions(["up"], ["go_gc", "http_errors"], true);
    expect(next).toEqual(["up", "go_gc", "http_errors"]);
  });

  it("is idempotent when a glob already covers a selected signal", () => {
    // "go_*" already excludes go_goroutines, so bulk-excluding it is a no-op,
    // while a signal no glob covers is still added.
    const next = toggleMetricExclusions(
      ["go_*"],
      ["go_goroutines", "http_errors"],
      true,
    );
    expect(next).toEqual(["go_*", "http_errors"]);
  });

  it("drops every entry matching each un-excluded signal", () => {
    const next = toggleMetricExclusions(
      ["go_*", "up", "http_errors"],
      ["go_goroutines", "http_errors"],
      false,
    );
    expect(next).toEqual(["up"]);
  });

  it("never mutates the input array", () => {
    const input = ["up"];
    toggleMetricExclusions(input, ["a", "b"], true);
    expect(input).toEqual(["up"]);
  });
});

// The per-log-pattern grain (E1) rides the whole-list PUT — there is no
// per-pattern POST/DELETE route — so these read-modify-write helpers are the
// ONLY way an ignored log pattern lands in the policy's `patterns` list, which
// is exactly what moves the row into the Ignored tab. Log patterns match by
// EXACT key (never glob), so the toggle is a plain add/remove.
describe("toggleLogPatternExclusion (single, exact key)", () => {
  it("adds the pattern key when excluding", () => {
    expect(toggleLogPatternExclusion([], "p-abc", true)).toEqual(["p-abc"]);
    expect(toggleLogPatternExclusion(["p-1"], "p-2", true)).toEqual([
      "p-1",
      "p-2",
    ]);
  });

  it("is idempotent — excluding an already-excluded key is a no-op", () => {
    expect(toggleLogPatternExclusion(["p-1"], "p-1", true)).toEqual(["p-1"]);
  });

  it("drops the pattern key when resuming (un-excluding)", () => {
    expect(toggleLogPatternExclusion(["p-1", "p-2"], "p-1", false)).toEqual([
      "p-2",
    ]);
  });

  it("matches by EXACT key, never as a glob/substring", () => {
    // A key that is a prefix of another must not remove the longer one.
    expect(toggleLogPatternExclusion(["p-10", "p-1"], "p-1", false)).toEqual([
      "p-10",
    ]);
  });

  it("never mutates the input array", () => {
    const input = ["p-1"];
    toggleLogPatternExclusion(input, "p-2", true);
    expect(input).toEqual(["p-1"]);
  });
});

describe("toggleLogPatternExclusions (bulk fold, one PUT)", () => {
  it("adds every selected pattern in a single new list", () => {
    expect(toggleLogPatternExclusions(["p-1"], ["p-2", "p-3"], true)).toEqual([
      "p-1",
      "p-2",
      "p-3",
    ]);
  });

  it("drops every selected pattern when resuming", () => {
    expect(
      toggleLogPatternExclusions(["p-1", "p-2", "p-3"], ["p-1", "p-3"], false),
    ).toEqual(["p-2"]);
  });

  it("never mutates the input array", () => {
    const input = ["p-1"];
    toggleLogPatternExclusions(input, ["p-2", "p-3"], true);
    expect(input).toEqual(["p-1"]);
  });
});

describe("patternExcluded ↔ toggleLogPatternExclusion round-trip (Ignored-tab membership)", () => {
  it("an ignored pattern reads as excluded (so it leaves Active for Ignored)", () => {
    const after = toggleLogPatternExclusion([], "p-noisy", true);
    // patternExcluded is the Ignored-tab membership test the list page uses.
    expect(patternExcluded("p-noisy", after)).toBe(true);
    expect(patternExcluded("p-other", after)).toBe(false);
  });

  it("Resume returns the pattern to Active", () => {
    const excluded = ["p-noisy"];
    const after = toggleLogPatternExclusion(excluded, "p-noisy", false);
    expect(patternExcluded("p-noisy", after)).toBe(false);
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

describe("serviceExcluded", () => {
  const services = ["payments", "checkout"];
  it("is true only for an exact service-name membership match", () => {
    expect(serviceExcluded("payments", services)).toBe(true);
    expect(serviceExcluded("checkout", services)).toBe(true);
  });
  it("is exact — never a prefix or glob match (services are matched by name)", () => {
    expect(serviceExcluded("payment", services)).toBe(false);
    expect(serviceExcluded("payments-api", services)).toBe(false);
    expect(serviceExcluded("Payments", services)).toBe(false); // case-sensitive
  });
  it("is false for a blank name or an absent/empty list", () => {
    expect(serviceExcluded("", services)).toBe(false);
    expect(serviceExcluded("payments", [])).toBe(false);
    expect(serviceExcluded("payments", undefined)).toBe(false);
  });
});

describe("patternExcluded (E1 per-log-pattern grain)", () => {
  const patterns = ["p-5a71ebd3a010", "p-569be0e472f8"];
  it("is true only for an exact pattern-key membership match", () => {
    expect(patternExcluded("p-5a71ebd3a010", patterns)).toBe(true);
    expect(patternExcluded("p-569be0e472f8", patterns)).toBe(true);
  });
  it("is exact — never a prefix or glob match (log pattern ids are exact keys)", () => {
    expect(patternExcluded("p-5a71ebd3a01", patterns)).toBe(false);
    expect(patternExcluded("p-5a71ebd3a010-x", patterns)).toBe(false);
    expect(patternExcluded("P-5A71EBD3A010", patterns)).toBe(false); // case-sensitive
  });
  it("is false for a blank key or an absent/empty list", () => {
    expect(patternExcluded("", patterns)).toBe(false);
    expect(patternExcluded("p-5a71ebd3a010", [])).toBe(false);
    expect(patternExcluded("p-5a71ebd3a010", undefined)).toBe(false);
  });
});

describe("listExcludeControlVisible", () => {
  // The list-page (logs / metrics / traces) per-row control renders ONLY for a
  // licensed admin — the founder-reported ask. It is hidden from a licensed
  // viewer and absent on community / OSS, so the feature never leaks to a
  // non-admin. This is the exact three-way contract the fix must prove.
  it("shows the control for a licensed runtime:manage session (editable)", () => {
    const gate = learnExcludeGate({ licensed: true, canManage: true });
    expect(gate).toBe("editable");
    expect(listExcludeControlVisible(gate)).toBe(true);
  });
  it("hides the control from a licensed viewer (readonly)", () => {
    const gate = learnExcludeGate({ licensed: true, canManage: false });
    expect(gate).toBe("readonly");
    expect(listExcludeControlVisible(gate)).toBe(false);
  });
  it("is absent on a community / OSS surface (absent)", () => {
    expect(
      listExcludeControlVisible(
        learnExcludeGate({ licensed: false, canManage: true }),
      ),
    ).toBe(false);
    expect(
      listExcludeControlVisible(
        learnExcludeGate({ licensed: false, canManage: false }),
      ),
    ).toBe(false);
  });
});
