import { describe, it, expect } from "vitest";
import {
  countExcluded,
  filterByScope,
  isExclusionScope,
} from "@/lib/rowActions";

// Pure-logic tests for the shared scope vocabulary + Active|Ignored partition.
// The per-page action SETS now live entirely in bulkSelect.ts (the checkbox
// action bar is the ONE row-action model — there is no per-row ⋯ menu), so this
// file pins only the id-agnostic contracts rowActions still owns:
//   • the Active|Ignored partition moves excluded rows out of Active and Resume
//     returns them (per-pattern / per-signal grain);
//   • the scope param defaults safely.

describe("Active | Ignored partition (D3)", () => {
  type Row = { id: string; service: string };
  const rows: Row[] = [
    { id: "a", service: "api" },
    { id: "b", service: "web" },
    { id: "c", service: "api" },
  ];
  // Per-pattern grain (E1): the policy excludes by row id, not by service.
  const excludedIds = new Set(["a", "c"]);
  const isExcluded = (r: Row) => excludedIds.has(r.id);

  it("Active shows only non-excluded rows — excluded rows LEAVE the list", () => {
    const active = filterByScope(rows, "active", isExcluded);
    expect(active.map((r) => r.id)).toEqual(["b"]);
  });

  it("Ignored shows only excluded rows", () => {
    const ignored = filterByScope(rows, "ignored", isExcluded);
    expect(ignored.map((r) => r.id)).toEqual(["a", "c"]);
  });

  it("Resume (un-ignore) returns a row to Active", () => {
    // Simulate the policy dropping the excluded ids — the rows partition Active.
    const resumed = (r: Row) => new Set<string>().has(r.id);
    expect(filterByScope(rows, "active", resumed).map((r) => r.id)).toEqual([
      "a",
      "b",
      "c",
    ]);
    expect(filterByScope(rows, "ignored", resumed)).toEqual([]);
  });

  it("countExcluded feeds the Ignored badge", () => {
    expect(countExcluded(rows, isExcluded)).toBe(2);
  });
});

describe("isExclusionScope", () => {
  it("defaults unknown / null to active so a hand-edited link never strands", () => {
    expect(isExclusionScope(null)).toBe("active");
    expect(isExclusionScope("active")).toBe("active");
    expect(isExclusionScope("bogus")).toBe("active");
    expect(isExclusionScope("ignored")).toBe("ignored");
  });
});
