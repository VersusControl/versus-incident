import { describe, it, expect } from "vitest";
import {
  ADMIN_LEGACY_HASHES,
  ADMIN_SECTIONS,
  SETTINGS_LEGACY_TABS,
  SETTINGS_SECTIONS,
  groupSections,
  resolveSection,
} from "./sections";

describe("resolveSection", () => {
  it("selects a known ?section= value", () => {
    expect(resolveSection(SETTINGS_SECTIONS, { section: "spike" })).toEqual({
      id: "spike",
      fromLegacy: false,
    });
  });

  it("falls back to the first section for an unknown or missing value", () => {
    expect(resolveSection(SETTINGS_SECTIONS, { section: "nope" }).id).toBe(
      SETTINGS_SECTIONS[0].id,
    );
    expect(resolveSection(ADMIN_SECTIONS, {}).id).toBe(ADMIN_SECTIONS[0].id);
  });

  it("maps legacy ?tab= values and flags them for a URL rewrite", () => {
    const legacy = { tabs: SETTINGS_LEGACY_TABS };
    expect(resolveSection(SETTINGS_SECTIONS, { tab: "alerting" }, legacy)).toEqual({
      id: "channels",
      fromLegacy: true,
    });
    expect(resolveSection(SETTINGS_SECTIONS, { tab: "agent" }, legacy).id).toBe(
      "agent-sources",
    );
    expect(resolveSection(SETTINGS_SECTIONS, { tab: "tuning" }, legacy).id).toBe(
      "service-health",
    );
  });

  it("maps legacy #hash anchors on admin", () => {
    const legacy = { hashes: ADMIN_LEGACY_HASHES };
    expect(
      resolveSection(ADMIN_SECTIONS, { hash: "#agent-ai-settings" }, legacy),
    ).toEqual({ id: "ai", fromLegacy: true });
    expect(
      resolveSection(ADMIN_SECTIONS, { hash: "alert-fatigue-settings" }, legacy).id,
    ).toBe("alert-fatigue");
  });

  it("prefers ?section= over a legacy value", () => {
    const r = resolveSection(
      SETTINGS_SECTIONS,
      { section: "reports", tab: "agent" },
      { tabs: SETTINGS_LEGACY_TABS },
    );
    expect(r).toEqual({ id: "reports", fromLegacy: false });
  });

  it("every legacy target is a registered section", () => {
    for (const id of Object.values(SETTINGS_LEGACY_TABS)) {
      expect(SETTINGS_SECTIONS.some((s) => s.id === id)).toBe(true);
    }
    for (const id of Object.values(ADMIN_LEGACY_HASHES)) {
      expect(ADMIN_SECTIONS.some((s) => s.id === id)).toBe(true);
    }
  });
});

describe("section registries", () => {
  it("have unique ids", () => {
    for (const list of [ADMIN_SECTIONS, SETTINGS_SECTIONS]) {
      const ids = list.map((s) => s.id);
      expect(new Set(ids).size).toBe(ids.length);
    }
  });

  it("groupSections keeps registry order within and across groups", () => {
    const groups = groupSections(SETTINGS_SECTIONS);
    expect(groups.map((g) => g.group)).toEqual([
      "General",
      "Alerting",
      "Agent",
      "Reports",
    ]);
    expect(groups.flatMap((g) => g.sections.map((s) => s.id))).toEqual(
      SETTINGS_SECTIONS.map((s) => s.id),
    );
  });
});
