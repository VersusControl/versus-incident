import { describe, it, expect } from "vitest";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";
import { matchesMetricPattern } from "@/lib/learnExclude";

// learnExcludeParity.test.ts — cross-language parity guard.
//
// The Go server matcher (versus-enterprise/pkg/learnexclude compileMetricPattern)
// is the SOLE authority for the Disable-Learn metric glob/prefix/exact
// semantics; this client matcher (matchesMetricPattern) mirrors it BY HAND.
// This test and its Go sibling
// (versus-enterprise/pkg/learnexclude/metric_glob_parity_test.go) read the SAME
// shared JSON fixture so the two implementations cannot silently drift. The
// server stays authoritative — this only keeps the UI checkbox honest.
//
// Fixture location (stable, documented): the PUBLIC OSS tree at
// versus-incident/testdata/learnexclude/metric-glob-parity.json. Resolved
// relative to this test file (cwd-independent): src/lib -> OSS module root is
// three levels up, then into testdata/.

interface ParityCase {
  name: string;
  entry: string;
  input: string;
  expected: boolean;
}

interface ParityFixture {
  cases: ParityCase[];
}

const here = path.dirname(fileURLToPath(import.meta.url));
const fixturePath = path.resolve(
  here,
  "../../../testdata/learnexclude/metric-glob-parity.json",
);
const fixture = JSON.parse(readFileSync(fixturePath, "utf8")) as ParityFixture;

describe("metric glob matcher cross-language parity (B51)", () => {
  it("loads the shared parity fixture", () => {
    expect(fixture.cases.length).toBeGreaterThan(0);
  });

  for (const c of fixture.cases) {
    it(`${c.name}: entry=${JSON.stringify(c.entry)} input=${JSON.stringify(c.input)}`, () => {
      // matchesMetricPattern signature is (signal, entry); the fixture's
      // `input` is the signal and `entry` is the exclusion pattern.
      expect(matchesMetricPattern(c.input, c.entry)).toBe(c.expected);
    });
  }
});
