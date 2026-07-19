// @vitest-environment jsdom
import { describe, it, expect, afterEach } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";
import { EmptyValue } from "@/components/feedback";
import { SeverityBadge } from "@/components/SeverityBadge";

// 2.1 / 2.2 — table consistency: ONE muted empty-value treatment everywhere,
// a Service-first column order on the Incidents + Decisions tables (and the
// Shadow/Spike views that were missing a Service column), and Service-first
// on the Agent overview "Lifetime totals" (Service · Shadow · Detect).
//
// Render tests pin the shared renderer's behaviour; source-pinned tests guard
// the column ORDER against the real files (mounting the full pages needs the
// whole react-query + router context — see adminUiImprovements.test.ts).

afterEach(cleanup);

const here = path.dirname(fileURLToPath(import.meta.url)); // src/lib
const read = (rel: string) => readFileSync(path.resolve(here, rel), "utf8");

describe("EmptyValue — the one shared empty-cell treatment", () => {
  it("renders a muted em dash with an accessible 'none' label", () => {
    render(<EmptyValue />);
    const el = screen.getByLabelText("none");
    expect(el.textContent).toBe("—");
    expect(el.className).toContain("text-ink-400");
  });
});

describe("SeverityBadge — null/empty severity unifies to the muted dash (was a bare dot)", () => {
  it("renders EmptyValue (a muted —), NOT a bare dot, when severity is null", () => {
    const { container } = render(<SeverityBadge severity={null} />);
    expect(screen.getByLabelText("none").textContent).toBe("—");
    // The old bare-dot treatment (a rounded-full filled span) must be gone.
    expect(container.querySelector(".rounded-full")).toBeNull();
  });

  it("renders EmptyValue for an unrecognised severity string", () => {
    render(<SeverityBadge severity="   " />);
    expect(screen.getByLabelText("none").textContent).toBe("—");
  });

  it("still renders a real severity as a labelled badge", () => {
    render(<SeverityBadge severity="critical" />);
    expect(screen.getByText("CRITICAL")).toBeTruthy();
  });
});

describe("Incidents table — Service column is FIRST", () => {
  const src = read("../pages/IncidentsPage.tsx");
  it("puts the Service header before Severity/When", () => {
    const service = src.indexOf(">Service</th>");
    const severity = src.indexOf(">Severity</th>");
    const when = src.indexOf('label="When"');
    expect(service).toBeGreaterThan(-1);
    expect(service).toBeLessThan(severity);
    expect(service).toBeLessThan(when);
  });
  it("routes empty cells through the shared EmptyValue", () => {
    expect(src.includes("EmptyValue")).toBe(true);
    // The old ad-hoc muted-dash spans are gone.
    expect(src.includes('<span className="text-ink-400">—</span>')).toBe(false);
  });
});

describe("Decisions Detect table — Service column is FIRST", () => {
  const src = read("../pages/DecisionsPage.tsx");
  it("puts Service before When/Outcome in the detect header", () => {
    const service = src.indexOf(">Service</th>");
    const when = src.indexOf('label="When"');
    const outcome = src.indexOf(">Outcome</th>");
    expect(service).toBeGreaterThan(-1);
    expect(service).toBeLessThan(when);
    expect(service).toBeLessThan(outcome);
  });
});

describe("Decisions Shadow + Spike — a Service column was ADDED (Service first)", () => {
  const src = read("../pages/DecisionsPage.tsx");
  it("the shared shadow table leads with a Service header", () => {
    // Shadow header: Service then Verdict.
    expect(/>Service<\/th>\s*<th className="w-28">Verdict<\/th>/.test(src)).toBe(
      true,
    );
    // Includes the shared select + eye columns added for table parity.
    expect(src.includes("const SHADOW_COLS = 11")).toBe(true);
  });
  it("the Spike view is a unified detect+shadow table, Service first", () => {
    expect(src.includes("buildSpikeRows")).toBe(true);
    expect(src.includes("SpikeTable")).toBe(true);
    expect(src.includes("SpikeKindPill")).toBe(true);
    // Spike no longer reuses the shadow-only table.
    const spikeTab = src.slice(src.indexOf("function SpikeTab"));
    expect(spikeTab.includes("ShadowEventsTable")).toBe(false);
    expect(spikeTab.includes("api.listDetect")).toBe(true);
    expect(spikeTab.includes("api.listShadow")).toBe(true);
  });
});

describe("Agent overview Lifetime totals — Service · Shadow · Detect order", () => {
  const src = read("../pages/AgentOverviewPage.tsx");
  it("orders the Services / Shadow / Detect tiles Service-first", () => {
    const grid = src.slice(src.indexOf('grid grid-cols-2 gap-3 lg:grid-cols-4"'));
    const services = grid.indexOf('label="Services tracked"');
    const shadow = grid.indexOf('label="Shadow events"');
    const detect = grid.indexOf('label="Detect events"');
    expect(services).toBeGreaterThan(-1);
    expect(services).toBeLessThan(shadow);
    expect(shadow).toBeLessThan(detect);
  });
});
