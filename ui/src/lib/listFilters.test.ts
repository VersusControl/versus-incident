import { describe, it, expect } from "vitest";
import {
  activeFilterCount,
  applyFacets,
  facetChips,
  facetOptions,
  facetViews,
  readFacetSelection,
  selectionSignature,
  withFacetValues,
  withoutFacets,
  type FacetDef,
} from "./listFilters";

type Row = { service: string; signal: string; tags?: string[] };

const SERVICE: FacetDef<Row> = {
  id: "service",
  label: "Service",
  values: (r) => r.service,
};
const SIGNAL: FacetDef<Row> = {
  id: "signal",
  label: "Signal",
  values: (r) => r.signal,
  format: (v) => v.toUpperCase(),
};
const TAG: FacetDef<Row> = { id: "tag", label: "Tag", values: (r) => r.tags };
const FACETS = [SERVICE, SIGNAL, TAG];

const rows: Row[] = [
  { service: "api", signal: "latency", tags: ["a", "a", "b"] },
  { service: "api", signal: "errors" },
  { service: "web", signal: "latency", tags: ["b"] },
  { service: "db", signal: "latency" },
];

describe("facetOptions", () => {
  it("counts each value once per row, most common first", () => {
    expect(facetOptions(rows, SERVICE)).toEqual([
      { value: "api", label: "api", count: 2 },
      { value: "db", label: "db", count: 1 },
      { value: "web", label: "web", count: 1 },
    ]);
    expect(facetOptions(rows, TAG)).toEqual([
      { value: "b", label: "b", count: 2 },
      { value: "a", label: "a", count: 1 },
    ]);
  });

  it("uses the facet's formatter for labels", () => {
    expect(facetOptions(rows, SIGNAL)[0]).toEqual({
      value: "latency",
      label: "LATENCY",
      count: 3,
    });
  });
});

describe("applyFacets", () => {
  it("returns every row without a selection", () => {
    expect(applyFacets(rows, FACETS, {})).toHaveLength(4);
  });

  it("ORs values within a facet and ANDs across facets", () => {
    const sel = { service: ["api", "web"], signal: ["latency"] };
    expect(applyFacets(rows, FACETS, sel)).toEqual([rows[0], rows[2]]);
  });

  it("matches array-valued facets on any value", () => {
    expect(applyFacets(rows, FACETS, { tag: ["a"] })).toEqual([rows[0]]);
  });
});

describe("URL encoding", () => {
  it("round-trips repeated params and ignores unknown facets", () => {
    let p = new URLSearchParams("status=ready&f.other=x");
    p = withFacetValues(p, "service", ["api", "web"]);
    p = withFacetValues(p, "signal", ["latency"]);
    expect(readFacetSelection(p, FACETS)).toEqual({
      service: ["api", "web"],
      signal: ["latency"],
    });
    expect(p.get("status")).toBe("ready");

    const cleared = withoutFacets(p, FACETS);
    expect(readFacetSelection(cleared, FACETS)).toEqual({});
    expect(cleared.get("status")).toBe("ready");
    expect(cleared.get("f.other")).toBe("x");
  });

  it("removes a facet when given no values", () => {
    const p = withFacetValues(new URLSearchParams("f.service=api"), "service", []);
    expect(p.has("f.service")).toBe(false);
  });
});

describe("summaries", () => {
  it("counts applied values and builds an order-independent signature", () => {
    expect(activeFilterCount({ service: ["a", "b"], signal: ["c"] })).toBe(3);
    expect(selectionSignature({ b: ["2", "1"], a: ["x"] })).toBe(
      selectionSignature({ a: ["x"], b: ["1", "2"] }),
    );
  });

  it("keeps a deep-linked value visible even when no row carries it", () => {
    const [service] = facetViews(rows, [SERVICE], { service: ["gone"] });
    expect(service.options).toContainEqual({ value: "gone", label: "gone", count: 0 });
    expect(service.selected).toEqual(["gone"]);
  });

  it("labels chips with the facet name and formatted value", () => {
    expect(facetChips(FACETS, { signal: ["errors"] })).toEqual([
      { facetId: "signal", value: "errors", label: "Signal: ERRORS" },
    ]);
  });
});
