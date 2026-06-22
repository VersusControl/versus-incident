import { describe, it, expect } from "vitest";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";

// This test guards the plain-language promise of the three "what the agent
// knows" views (Logs / Metrics / Traces): it reads the real source of the copy
// surfaces and fails if the old, operator-confusing jargon creeps back in.

const here = path.dirname(fileURLToPath(import.meta.url)); // src/lib

function read(rel: string): string {
  return readFileSync(path.resolve(here, rel), "utf8");
}

// The files that carry user-facing copy for the three views plus the shared
// value formatter.
const FILES: Record<string, string> = {
  "pages/LearnedSignalsView.tsx": read("../pages/LearnedSignalsView.tsx"),
  "pages/PatternsPage.tsx": read("../pages/PatternsPage.tsx"),
  "pages/PatternDetailPage.tsx": read("../pages/PatternDetailPage.tsx"),
  "lib/format.ts": read("./format.ts"),
};

// Pure jargon — never a valid code identifier, so banned everywhere in these
// files (comments included).
const HARD_BANNED: Array<[string, RegExp]> = [
  ["seasonality", /seasonality/i],
  ["score against", /score\s+against/i],
  ["Expected now", /expected\s+now/i],
  ["EWMA", /\bEWMA\b/],
  ["σ (sigma glyph)", /σ/],
];

// User-facing labels. Case-sensitive on purpose so the OSS API identifiers
// (BaselineRow, listBaselines, baseline_frequency) don't trip the check — only
// a Title-case standalone "Baseline"/"Baselines" shown to operators fails.
const COPY_BANNED: Array<[string, RegExp]> = [
  ["the word Baseline(s) as a label", /\bBaselines?\b/],
  ["a 'Confidence:' field label", /Confidence:/],
];

describe("agent 'knows' views carry no jargon", () => {
  for (const [name, src] of Object.entries(FILES)) {
    for (const [label, re] of HARD_BANNED) {
      it(`${name} contains no ${label}`, () => {
        expect(re.test(src), `${name} must not contain ${label}`).toBe(false);
      });
    }
  }

  // The Title-case "Baseline" label check applies to the rendered view files,
  // not the formatter (which has no such label).
  const COPY_FILES = [
    "pages/LearnedSignalsView.tsx",
    "pages/PatternsPage.tsx",
    "pages/PatternDetailPage.tsx",
  ];
  for (const name of COPY_FILES) {
    const src = FILES[name];
    for (const [label, re] of COPY_BANNED) {
      it(`${name} contains no ${label}`, () => {
        expect(re.test(src), `${name} must not contain ${label}`).toBe(false);
      });
    }
  }
});
