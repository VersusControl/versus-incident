import { describe, it, expect } from "vitest";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";

// Source-pinned guards (mounting these pages needs the full react-query +
// router context — see adminUiImprovements.test.ts / tableConsistency.test.tsx),
// for two changes:
//   • Settings groups the incident-delivery config with the spike baseline
//     control under one "Alerting" tab, leaving the agent runtime + report on
//     the "Agent" tab — no empty tab.
//   • The incident / decision / analysis tables reuse the SAME building blocks
//     as the logs / metrics / traces tables: row-select checkboxes feeding a
//     BulkActionBar, an eye that opens a PeekPanel, and pagination.

const here = path.dirname(fileURLToPath(import.meta.url)); // src/lib
const read = (rel: string) => readFileSync(path.resolve(here, rel), "utf8");

describe("Settings — three tabs: Alerting, Agent, Detection & reports", () => {
  const src = read("../pages/SettingsPage.tsx");

  it("defaults to Alerting and offers all three tabs", () => {
    expect(src.includes('defaultValue="alerting"')).toBe(true);
    expect(src.includes('{ value: "alerting", label: "Alerting" }')).toBe(true);
    expect(src.includes('{ value: "agent", label: "Agent" }')).toBe(true);
    expect(
      src.includes('{ value: "tuning", label: "Detection & reports" }'),
    ).toBe(true);
  });

  it("Alerting tab shows the incident-delivery config only (spike moved out)", () => {
    const alerting = src.slice(
      src.indexOf('tab === "alerting"'),
      src.indexOf('tab === "agent"'),
    );
    expect(alerting.includes("<IncidentsConfigPanel />")).toBe(true);
    expect(alerting.includes("<SpikeSettingsControl />")).toBe(false);
  });

  it("Agent tab shows the agent runtime config only (report moved out)", () => {
    const agent = src.slice(
      src.indexOf('tab === "agent"'),
      src.indexOf(") : ("),
    );
    expect(agent.includes("<AgentConfigPanel />")).toBe(true);
    expect(agent.includes("<ReportSettingsControl />")).toBe(false);
  });

  it("Detection & reports tab groups the spike baseline + incident report", () => {
    const tuning = src.slice(src.indexOf(") : ("));
    expect(tuning.includes("<SpikeSettingsControl />")).toBe(true);
    expect(tuning.includes("<ReportSettingsControl />")).toBe(true);
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

// The old /config/* URLs must keep resolving after the Settings reorg: the two
// pre-reorg config pages now live as tabs on /settings, so their legacy paths
// redirect there — incidents config to the default (Alerting) tab, agent config
// to the Agent tab. Source-pinned against the router (mounting App needs the
// whole auth + react-query context) and cross-checked against the tab the
// destination reads, so a redirect can never land on a tab Settings ignores.
describe("Legacy /config/* URLs redirect into the Settings tabs", () => {
  const app = read("../App.tsx");
  const settings = read("../pages/SettingsPage.tsx");

  it("redirects /config/incidents to the default Settings (Alerting) tab", () => {
    expect(
      /path="\/config\/incidents"[\s\S]*?Navigate to="\/settings" replace/.test(
        app,
      ),
    ).toBe(true);
  });

  it("redirects /config/agent to the Agent tab on Settings", () => {
    expect(
      /path="\/config\/agent"[\s\S]*?Navigate to="\/settings\?tab=agent" replace/.test(
        app,
      ),
    ).toBe(true);
  });

  it("Settings selects each tab from its ?tab= value and defaults to Alerting", () => {
    // Every current tab must be reachable via its own ?tab= value, and an
    // unknown/absent value must fall back to the default (Alerting) tab — the
    // /config/agent redirect target (?tab=agent) therefore opens the Agent tab.
    expect(settings.includes('raw === "agent" ? "agent"')).toBe(true);
    expect(settings.includes('raw === "tuning" ? "tuning"')).toBe(true);
    expect(settings.includes(': "alerting"')).toBe(true);
    expect(settings.includes('const raw = params.get("tab")')).toBe(true);
  });
});
