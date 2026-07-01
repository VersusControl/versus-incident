import { describe, it, expect } from "vitest";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";

// Guards the two-part catalog-admin change:
//   1. the redundant "Flush to disk" control is gone (client fn + endpoint), and
//   2. the destructive "Clear all" reset is wired end to end (client fn +
//      danger-toned confirm on the Logs page).
// Like agentCopy.test.ts, it reads the real source so a regression that
// re-introduces flush or unwires reset fails here.

const here = path.dirname(fileURLToPath(import.meta.url)); // src/lib

function read(rel: string): string {
  return readFileSync(path.resolve(here, rel), "utf8");
}

const api = read("./api.ts");
const patternsPage = read("../pages/PatternsPage.tsx");

describe("api client: flush removed, reset added", () => {
  it("no longer exposes flushPatterns", () => {
    expect(api.includes("flushPatterns")).toBe(false);
  });

  it("no longer references the removed /api/agent/flush endpoint", () => {
    expect(api.includes("/api/agent/flush")).toBe(false);
  });

  it("exposes resetCatalog as DELETE /api/agent/catalog", () => {
    expect(api.includes("resetCatalog")).toBe(true);
    expect(/resetCatalog[\s\S]{0,160}\/api\/agent\/catalog/.test(api)).toBe(
      true,
    );
    expect(/\/api\/agent\/catalog[\s\S]{0,80}method:\s*"DELETE"/.test(api)).toBe(
      true,
    );
  });
});

describe("PatternsPage: no flush control, destructive Clear all", () => {
  it("drops every flush reference", () => {
    expect(patternsPage.includes("flush")).toBe(false);
    expect(patternsPage.includes("Flush")).toBe(false);
    expect(patternsPage.includes("confirmFlush")).toBe(false);
  });

  it("wires the Clear all reset through api.resetCatalog", () => {
    expect(patternsPage.includes("api.resetCatalog")).toBe(true);
    expect(patternsPage.includes("Clear all")).toBe(true);
  });

  it("confirms the wipe with a danger-toned dialog", () => {
    expect(patternsPage.includes('tone="danger"')).toBe(true);
    expect(/relearns from scratch/i.test(patternsPage)).toBe(true);
  });
});
