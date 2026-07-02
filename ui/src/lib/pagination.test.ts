import { describe, expect, it } from "vitest";

import { PAGE_SIZE, pageSlice } from "./pagination";

describe("PAGE_SIZE", () => {
  it("is 100 — the fixed page size for every agent admin table", () => {
    expect(PAGE_SIZE).toBe(100);
  });
});

describe("pageSlice", () => {
  it("windows the first page of a large list at 100 rows", () => {
    const s = pageSlice(2314, 1);
    expect(s).toEqual({
      page: 1,
      pageCount: 24,
      pageSize: 100,
      total: 2314,
      start: 0,
      end: 100,
    });
  });

  it("windows a middle page", () => {
    const s = pageSlice(2314, 3);
    expect(s.start).toBe(200);
    expect(s.end).toBe(300);
    expect(s.page).toBe(3);
  });

  it("clamps the tail page to the real remainder", () => {
    const s = pageSlice(2314, 24);
    expect(s.start).toBe(2300);
    expect(s.end).toBe(2314); // 14 rows, not 100
  });

  it("clamps an out-of-range page down to the last page", () => {
    // The list shrank under the cursor after a filter change.
    const s = pageSlice(150, 99);
    expect(s.page).toBe(2);
    expect(s.pageCount).toBe(2);
    expect(s.start).toBe(100);
    expect(s.end).toBe(150);
  });

  it("never returns a page below 1", () => {
    const s = pageSlice(150, 0);
    expect(s.page).toBe(1);
    expect(s.start).toBe(0);
  });

  it("treats a non-finite page as page 1", () => {
    const s = pageSlice(150, Number.NaN);
    expect(s.page).toBe(1);
  });

  it("handles an empty list as a single empty page", () => {
    const s = pageSlice(0, 1);
    expect(s).toEqual({
      page: 1,
      pageCount: 1,
      pageSize: 100,
      total: 0,
      start: 0,
      end: 0,
    });
  });

  it("exact multiples do not add a phantom trailing page", () => {
    const s = pageSlice(200, 2);
    expect(s.pageCount).toBe(2);
    expect(s.start).toBe(100);
    expect(s.end).toBe(200);
  });

  it("respects a custom page size", () => {
    const s = pageSlice(45, 2, 20);
    expect(s.pageCount).toBe(3);
    expect(s.start).toBe(20);
    expect(s.end).toBe(40);
  });
});
