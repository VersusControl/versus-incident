import { describe, it, expect } from "vitest";
import {
  tsValue,
  compareValues,
  sortRows,
  nextSort,
  sortSignature,
} from "./sortRows";

describe("tsValue", () => {
  it("parses an ISO timestamp to its epoch millis", () => {
    expect(tsValue("2026-07-19T00:00:00.000Z")).toBe(
      Date.parse("2026-07-19T00:00:00.000Z"),
    );
  });

  it("returns NaN for a missing stamp", () => {
    expect(Number.isNaN(tsValue(undefined))).toBe(true);
    expect(Number.isNaN(tsValue(null))).toBe(true);
    expect(Number.isNaN(tsValue(""))).toBe(true);
  });

  it("returns NaN for an unparseable stamp", () => {
    expect(Number.isNaN(tsValue("not-a-date"))).toBe(true);
  });
});

describe("compareValues", () => {
  it("orders numbers numerically", () => {
    expect(compareValues(1, 2)).toBeLessThan(0);
    expect(compareValues(2, 1)).toBeGreaterThan(0);
    expect(compareValues(5, 5)).toBe(0);
  });

  it("sinks NaN below real numbers (blanks go last in a desc sort)", () => {
    expect(compareValues(NaN, 0)).toBeLessThan(0);
    expect(compareValues(0, NaN)).toBeGreaterThan(0);
  });

  it("orders strings case-insensitively and numerically", () => {
    expect(compareValues("apple", "Banana")).toBeLessThan(0);
    expect(compareValues("item-2", "item-10")).toBeLessThan(0);
  });
});

describe("sortRows", () => {
  const rows = [
    { id: "a", n: 3 },
    { id: "b", n: 1 },
    { id: "c", n: 2 },
  ];
  const byN = (r: { n: number }) => r.n;

  it("sorts ascending without mutating the input", () => {
    const out = sortRows(rows, byN, "asc");
    expect(out.map((r) => r.id)).toEqual(["b", "c", "a"]);
    expect(rows.map((r) => r.id)).toEqual(["a", "b", "c"]);
  });

  it("sorts descending", () => {
    const out = sortRows(rows, byN, "desc");
    expect(out.map((r) => r.id)).toEqual(["a", "c", "b"]);
  });

  it("is stable for ties", () => {
    const tied = [
      { id: "x", n: 1 },
      { id: "y", n: 1 },
      { id: "z", n: 1 },
    ];
    expect(sortRows(tied, byN, "asc").map((r) => r.id)).toEqual([
      "x",
      "y",
      "z",
    ]);
  });

  it("sinks blank timestamps to the bottom of a descending sort", () => {
    const stamped = [
      { id: "old", ts: "2026-01-01T00:00:00Z" },
      { id: "blank", ts: null as string | null },
      { id: "new", ts: "2026-07-01T00:00:00Z" },
    ];
    const out = sortRows(stamped, (r) => tsValue(r.ts), "desc");
    expect(out.map((r) => r.id)).toEqual(["new", "old", "blank"]);
  });
});

describe("nextSort", () => {
  it("starts a fresh column descending", () => {
    expect(nextSort(null, "when")).toEqual({ key: "when", dir: "desc" });
    expect(nextSort({ key: "other", dir: "asc" }, "when")).toEqual({
      key: "when",
      dir: "desc",
    });
  });

  it("flips direction when the active column is re-clicked", () => {
    expect(nextSort({ key: "when", dir: "desc" }, "when")).toEqual({
      key: "when",
      dir: "asc",
    });
    expect(nextSort({ key: "when", dir: "asc" }, "when")).toEqual({
      key: "when",
      dir: "desc",
    });
  });
});

describe("sortSignature", () => {
  it("is 'none' when no sort is active", () => {
    expect(sortSignature(null)).toBe("none");
  });

  it("encodes the active key and direction", () => {
    expect(sortSignature({ key: "when", dir: "desc" })).toBe("when:desc");
    expect(sortSignature({ key: "last_seen", dir: "asc" })).toBe(
      "last_seen:asc",
    );
  });
});
