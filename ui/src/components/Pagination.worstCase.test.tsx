// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { CursorPagination } from "./Pagination";
import type { CursorPaginationState } from "@/lib/pagination";

// Worst-case pager inputs, using the values the server actually sends: a
// bounded history count is the number of rows read, at most 1,001, with
// count_capped set above 1,000.

afterEach(cleanup);

function at(page: number): CursorPaginationState {
  return { page, canPrevious: page > 1, previousPage: vi.fn(), nextPage: vi.fn() };
}

describe("Cursor pagination worst cases", () => {
  it("shows a capped server count of 1,001 as 1,000+ with 20+ pages", () => {
    render(<CursorPagination state={at(1)} next="cursor-2" total={1001} limit={50} count={50} capped />);
    const text = screen.getByRole("navigation", { name: "Page navigation" }).textContent ?? "";
    expect(text).toContain("1–50 of 1,000+");
    expect(text).toContain("Page 1 / 20+");
  });

  it("keeps 1,000+ on the last page of a capped list", () => {
    const { rerender } = render(<CursorPagination state={at(1)} next="cursor-2" total={1001} limit={50} count={50} capped />);
    rerender(<CursorPagination state={at(21)} limit={50} count={1} />);
    expect(screen.queryByText(/1,001\+/)).toBeNull();
    expect(screen.getByText(/of 1,000\+$/)).toBeTruthy();
  });

  it("shows only the page number for a zero total that still has a next page", () => {
    render(<CursorPagination state={at(1)} next="cursor-2" total={0} limit={50} count={0} />);
    expect(screen.getByText("Page 1")).toBeTruthy();
    expect(screen.queryByText(/ of /)).toBeNull();
  });

  it("stays hidden when the total equals one full page", () => {
    const { container } = render(<CursorPagination state={at(1)} total={50} limit={50} count={50} />);
    expect(container.textContent).toBe("");
  });

  it("ends exactly on a one-item last page when the total is one more than a page", () => {
    render(<CursorPagination state={at(2)} total={51} limit={50} count={1} />);
    expect(screen.getByText("51–51 of 51")).toBeTruthy();
    expect(screen.getByText("Page 2 / 2")).toBeTruthy();
  });

  it("never claims an item that is not on an empty page beyond the total", () => {
    // A snapshot that shrank under the reader answers the old offset with an
    // empty page and the new total.
    render(<CursorPagination state={at(9)} total={312} limit={50} count={0} />);
    const text = screen.getByRole("navigation", { name: "Page navigation" }).textContent ?? "";
    expect(text).not.toMatch(/401–401/);
    const range = text.match(/([\d,]+)–([\d,]+) of/);
    if (range) {
      const start = Number(range[1].replace(/,/g, ""));
      const end = Number(range[2].replace(/,/g, ""));
      expect(start).toBeLessThanOrEqual(end);
      expect(end).toBeLessThanOrEqual(312);
    }
  });

  it("follows a snapshot total that changed on a later page", () => {
    const { rerender } = render(<CursorPagination state={at(1)} next="2" total={60} limit={20} count={20} />);
    rerender(<CursorPagination state={at(2)} next="4" total={41} limit={20} count={20} />);
    expect(screen.getByText("21–40 of 41")).toBeTruthy();
    expect(screen.getByText("Page 2 / 3")).toBeTruthy();
  });
});
