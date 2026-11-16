import { describe, it, expect } from "vitest";
import { existsSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";
import { matchesMetricPattern } from "@/lib/learnExclude";
import { METRIC_GLOB_CASES, type MetricGlobCase } from "../../../tests/ui/learnexclude/metricGlobParity";

// learnExcludeParity.test.ts — cross-language parity guard.
//
// The Go server matcher (versus-enterprise/pkg/learnexclude compileMetricPattern)
// is the SOLE authority for the Disable-Learn metric glob/prefix/exact
// semantics; this client matcher (matchesMetricPattern) mirrors it BY HAND.
// The committed cases live in code (tests/ui/learnexclude/metricGlobParity.ts)
// and the Go parity test carries the same table, so the two implementations
// cannot silently drift. The server stays authoritative — this only keeps the
// UI checkbox honest.
//
// A local copy of the shared cases may also exist at
// versus-incident/testdata/learnexclude/metric-glob-parity.json (gitignored).
// When it is present it is checked too; when it is missing that check is
// skipped, never failed.

function checkCases(cases: readonly MetricGlobCase[]) {
  for (const c of cases) {
    it(`${c.name}: entry=${JSON.stringify(c.entry)} input=${JSON.stringify(c.input)}`, () => {
      // matchesMetricPattern signature is (signal, entry); the case's `input`
      // is the signal and `entry` is the exclusion pattern.
      expect(matchesMetricPattern(c.input, c.entry)).toBe(c.expected);
    });
  }
}

describe("metric glob matcher cross-language parity", () => {
  it("has cases", () => {
    expect(METRIC_GLOB_CASES.length).toBeGreaterThan(0);
  });
  checkCases(METRIC_GLOB_CASES);
});

const here = path.dirname(fileURLToPath(import.meta.url));
const localCopy = path.resolve(here, "../../../testdata/learnexclude/metric-glob-parity.json");
const hasLocalCopy = existsSync(localCopy);

describe.skipIf(!hasLocalCopy)("metric glob parity against the local testdata copy (optional)", () => {
  const local = hasLocalCopy
    ? (JSON.parse(readFileSync(localCopy, "utf8")) as { cases: MetricGlobCase[] }).cases
    : [];
  it("has cases", () => {
    expect(local.length).toBeGreaterThan(0);
  });
  checkCases(local);
});
