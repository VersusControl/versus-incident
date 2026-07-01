import { describe, it, expect } from "vitest";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";

// Guards the split of the combined catalog reset into two scoped, per-page
// clears:
//   1. the redundant "Flush to disk" control stays gone,
//   2. the combined "Clear all" / DELETE /api/agent/catalog is fully removed,
//   3. patterns-only clear is wired end to end (client fn + danger confirm on
//      the Logs page), and
//   4. services-only clear is wired end to end (client fn + danger confirm on
//      the Services page).
// Like agentCopy.test.ts, it reads the real source so a regression that
// re-introduces flush or the combined reset fails here.

const here = path.dirname(fileURLToPath(import.meta.url)); // src/lib

function read(rel: string): string {
  return readFileSync(path.resolve(here, rel), "utf8");
}

const api = read("./api.ts");
const patternsPage = read("../pages/PatternsPage.tsx");
const servicesPage = read("../pages/ServicesPage.tsx");

describe("api client: flush + combined reset removed, scoped clears added", () => {
  it("no longer exposes flushPatterns", () => {
    expect(api.includes("flushPatterns")).toBe(false);
  });

  it("no longer references the removed /api/agent/flush endpoint", () => {
    expect(api.includes("/api/agent/flush")).toBe(false);
  });

  it("no longer exposes the combined resetCatalog / DELETE /api/agent/catalog", () => {
    expect(api.includes("resetCatalog")).toBe(false);
    expect(api.includes("/api/agent/catalog")).toBe(false);
  });

  it("exposes clearPatterns as DELETE /api/agent/patterns", () => {
    expect(api.includes("clearPatterns")).toBe(true);
    expect(/clearPatterns[\s\S]{0,160}\/api\/agent\/patterns/.test(api)).toBe(
      true,
    );
    expect(
      /\/api\/agent\/patterns"[\s\S]{0,80}method:\s*"DELETE"/.test(api),
    ).toBe(true);
  });

  it("exposes clearServices as DELETE /api/agent/services", () => {
    expect(api.includes("clearServices")).toBe(true);
    expect(/clearServices[\s\S]{0,160}\/api\/agent\/services/.test(api)).toBe(
      true,
    );
    expect(
      /\/api\/agent\/services"[\s\S]{0,80}method:\s*"DELETE"/.test(api),
    ).toBe(true);
  });
});

describe("PatternsPage: no flush control, patterns-only Clear all logs", () => {
  it("drops every flush reference", () => {
    expect(patternsPage.includes("flush")).toBe(false);
    expect(patternsPage.includes("Flush")).toBe(false);
    expect(patternsPage.includes("confirmFlush")).toBe(false);
  });

  it("does not reference the removed combined resetCatalog", () => {
    expect(patternsPage.includes("resetCatalog")).toBe(false);
  });

  it("wires Clear all logs through api.clearPatterns", () => {
    expect(patternsPage.includes("api.clearPatterns")).toBe(true);
    expect(patternsPage.includes("Clear all logs")).toBe(true);
  });

  it("confirms the wipe with a danger-toned dialog and log-scoped copy", () => {
    expect(patternsPage.includes('tone="danger"')).toBe(true);
    expect(/relearns log patterns from scratch/i.test(patternsPage)).toBe(true);
  });
});

describe("ServicesPage: services-only Clear all services", () => {
  it("wires Clear all services through api.clearServices", () => {
    expect(servicesPage.includes("api.clearServices")).toBe(true);
    expect(servicesPage.includes("Clear all services")).toBe(true);
  });

  it("confirms the wipe with a danger-toned dialog and service-scoped copy", () => {
    expect(servicesPage.includes('tone="danger"')).toBe(true);
    expect(/re-discovers services from scratch/i.test(servicesPage)).toBe(true);
  });

  it("does not reference the removed combined resetCatalog", () => {
    expect(servicesPage.includes("resetCatalog")).toBe(false);
  });
});
