import { describe, it, expect } from "vitest";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";
import {
  SETTINGS_LEGACY_TABS,
  SETTINGS_SECTIONS,
  resolveSection,
} from "@/components/settings/sections";

// Source-pinned guards (mounting these pages needs the full react-query +
// router context — see adminUiImprovements.test.ts / tableConsistency.test.tsx),
// for two changes:
//   • Settings renders one section at a time from the shared section registry,
//     so every registered section has a matching control.
//   • The incident / decision / analysis tables reuse the SAME building blocks
//     as the logs / metrics / traces tables: row-select checkboxes feeding a
//     BulkActionBar, an eye that opens a PeekPanel, and pagination.

const here = path.dirname(fileURLToPath(import.meta.url)); // src/lib
const read = (rel: string) => readFileSync(path.resolve(here, rel), "utf8");

describe("Settings — one section at a time", () => {
  const src = read("../pages/SettingsPage.tsx");

  it("uses the shared layout with the settings registry and legacy tabs", () => {
    expect(src.includes("<SettingsLayout")).toBe(true);
    expect(src.includes("sections={SETTINGS_SECTIONS}")).toBe(true);
    expect(src.includes("legacy={{ tabs: SETTINGS_LEGACY_TABS }}")).toBe(true);
  });

  it("renders a control for every registered section", () => {
    for (const s of SETTINGS_SECTIONS) {
      if (s.id === SETTINGS_SECTIONS[0].id) continue; // the default branch
      expect(src.includes(`case "${s.id}":`)).toBe(true);
    }
  });

  it("keeps the tuning controls on their own sections", () => {
    expect(src.includes("<CountSettingsControl />")).toBe(true);
    expect(src.includes("<ServiceHealthSettingsControl />")).toBe(true);
    expect(src.includes("<SpikeSettingsControl />")).toBe(true);
    expect(src.includes("<ReportSettingsControl />")).toBe(true);
  });
});

// The shared row-selection + peek building blocks every parity table must wire.
const PARITY_PAGES: Array<[string, string]> = [
  ["IncidentsPage.tsx", read("../pages/IncidentsPage.tsx")],
  ["DecisionsPage.tsx", read("../pages/DecisionsPage.tsx")],
  ["AnalysesListPage.tsx", read("../pages/AnalysesListPage.tsx")],
];

describe("Incident / decision / analysis tables reuse the shared table behavior", () => {
  for (const [name, src] of PARITY_PAGES) {
    it(`${name} wires the shared selection + bulk bar`, () => {
      expect(src.includes("useBulkSelection")).toBe(true);
      expect(src.includes("BulkActionBar")).toBe(true);
      expect(src.includes("SelectAllCheckbox")).toBe(true);
      expect(src.includes("RowSelectCheckbox")).toBe(true);
    });
    it(`${name} adds an eye that opens a PeekPanel`, () => {
      expect(src.includes("PeekPanel")).toBe(true);
      expect(src.includes("<Eye")).toBe(true);
      expect(src.includes('title="View details"')).toBe(true);
    });
  }
});

describe("Incidents bulk bar surfaces Assign + Resolve as selection actions", () => {
  const src = read("../pages/IncidentsPage.tsx");
  it("offers Assign and Resolve as bulk actions", () => {
    expect(src.includes('{ id: "assign", label: "Assign" }')).toBe(true);
    expect(src.includes('{ id: "resolve", label: "Resolve" }')).toBe(true);
  });
});

describe("Service detail — the pattern peek shows samples + baselines", () => {
  const src = read("../pages/ServiceDetailPage.tsx");
  it("adds an eye + PeekPanel with the shared PatternBaselines", () => {
    expect(src.includes("PeekPanel")).toBe(true);
    expect(src.includes("PatternBaselines")).toBe(true);
    expect(src.includes("<Eye")).toBe(true);
  });
  it("renders the redacted sample log lines in the peek", () => {
    expect(src.includes("peekPattern.samples")).toBe(true);
    expect(src.includes("Sample log lines")).toBe(true);
  });
  it("keeps the row link to the full pattern page", () => {
    expect(src.includes("`/agent/logs/${p.id}`")).toBe(true);
  });
});

// The old /config/* URLs must keep resolving: incidents config redirects to the
// default Settings section, agent config to the agent runtime section. Source-
// pinned against the router (mounting App needs the whole auth + react-query
// context) and cross-checked against the registry, so a redirect can never
// land on a section Settings doesn't know.
describe("Legacy /config/* URLs redirect into Settings sections", () => {
  const app = read("../App.tsx");

  it("redirects /config/incidents to the default Settings section", () => {
    expect(
      /path="\/config\/incidents"[\s\S]*?Navigate to="\/settings" replace/.test(
        app,
      ),
    ).toBe(true);
  });

  it("redirects /config/agent to the agent runtime section", () => {
    expect(
      /path="\/config\/agent"[\s\S]*?Navigate to="\/settings\?section=agent-runtime" replace/.test(
        app,
      ),
    ).toBe(true);
    expect(
      resolveSection(SETTINGS_SECTIONS, { section: "agent-runtime" }).id,
    ).toBe("agent-runtime");
  });

  it("maps every pre-section ?tab= value to a known section", () => {
    for (const tab of ["alerting", "agent", "tuning"]) {
      const r = resolveSection(SETTINGS_SECTIONS, { tab }, { tabs: SETTINGS_LEGACY_TABS });
      expect(r.fromLegacy).toBe(true);
      expect(SETTINGS_SECTIONS.some((s) => s.id === r.id)).toBe(true);
    }
  });
});
