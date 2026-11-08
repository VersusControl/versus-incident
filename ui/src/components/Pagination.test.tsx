// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { CursorPagination } from "./Pagination";
import type { CursorPaginationState } from "@/lib/pagination";

afterEach(cleanup);

function state(canPrevious = false): CursorPaginationState {
  return {
    page: canPrevious ? 2 : 1,
    canPrevious,
    previousPage: vi.fn(),
    nextPage: vi.fn(),
  };
}

describe("Cursor pagination", () => {
  it("renders nothing while the list has only one page", () => {
    const { container } = render(<CursorPagination state={state()} />);
    // "Page 1" beside two disabled buttons is a control that cannot act.
    expect(screen.queryByRole("navigation", { name: "Page navigation" })).toBeNull();
    expect(container.textContent).toBe("");
  });

  it("renders the pager once a next page exists and pages forward", () => {
    const paging = state();
    render(<CursorPagination state={paging} next="cursor-2" />);
    fireEvent.click(screen.getByRole("button", { name: "Next page" }));
    expect(paging.nextPage).toHaveBeenCalledWith("cursor-2");
  });

  it("keeps the pager on the last page so the reader can go back", () => {
    const paging = state(true);
    render(<CursorPagination state={paging} />);
    expect(screen.getByText("Page 2")).toBeTruthy();
    const next = screen.getByRole("button", { name: "Next page" }) as HTMLButtonElement;
    expect(next.disabled).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "Previous page" }));
    expect(paging.previousPage).toHaveBeenCalled();
  });

  it("stays out of the way while a page read is busy but a page remains", () => {
    render(<CursorPagination state={state()} next="cursor-2" busy />);
    const next = screen.getByRole("button", { name: "Next page" }) as HTMLButtonElement;
    expect(next.disabled).toBe(true);
  });

  it("renders an exact range and page count from the server total", () => {
    render(<CursorPagination state={state(true)} next="cursor-3" total={312} limit={50} count={50} />);
    expect(screen.getByText("51–100 of 312")).toBeTruthy();
    expect(screen.getByText("Page 2 / 7")).toBeTruthy();
  });

  it("marks a bounded total as capped on the range and page count", () => {
    render(<CursorPagination state={state(true)} next="cursor-3" total={1000} limit={50} count={50} capped />);
    expect(screen.getByText("51–100 of 1,000+")).toBeTruthy();
    expect(screen.getByText("Page 2 / 20+")).toBeTruthy();
  });

  it("renders only the page number when the response has no total", () => {
    render(<CursorPagination state={state(true)} />);
    expect(screen.getByText("Page 2")).toBeTruthy();
    expect(screen.queryByText(/ of /)).toBeNull();
  });

  it("keeps the first page's total while paging when page 2 omits it", () => {
    const { rerender } = render(<CursorPagination state={state()} next="cursor-2" total={312} limit={50} count={50} />);
    expect(screen.getByText("1–50 of 312")).toBeTruthy();
    rerender(<CursorPagination state={state(true)} next="cursor-3" limit={50} />);
    expect(screen.getByText("51–100 of 312")).toBeTruthy();
    expect(screen.getByText("Page 2 / 7")).toBeTruthy();
  });

  it("ends the range at the last item on a short final page", () => {
    const last: CursorPaginationState = { page: 7, canPrevious: true, previousPage: vi.fn(), nextPage: vi.fn() };
    render(<CursorPagination state={last} total={312} limit={50} count={12} />);
    expect(screen.getByText("301–312 of 312")).toBeTruthy();
    expect(screen.getByText("Page 7 / 7")).toBeTruthy();
  });

  it("stays hidden for a single page even when the total is known", () => {
    const { container } = render(<CursorPagination state={state()} total={30} limit={50} count={30} />);
    expect(container.textContent).toBe("");
  });

  it.each([
    ["no page size", { total: 312 }],
    ["a zero page size", { total: 312, limit: 0 }],
    ["a zero total", { total: 0, limit: 50 }],
  ])("falls back to the page number with %s", (_, props) => {
    render(<CursorPagination state={state(true)} next="cursor-3" count={50} {...props} />);
    expect(screen.getByText("Page 2")).toBeTruthy();
    expect(screen.queryByText(/ of /)).toBeNull();
    expect(screen.queryByText(/Page 2 \//)).toBeNull();
  });

  it("replaces the kept total when the first page is read again with a new one", () => {
    const { rerender } = render(<CursorPagination state={state()} next="cursor-2" total={312} limit={20} count={20} />);
    rerender(<CursorPagination state={state()} next="cursor-2" total={40} limit={20} count={20} />);
    expect(screen.getByText("1–20 of 40")).toBeTruthy();
    rerender(<CursorPagination state={state(true)} limit={20} count={20} />);
    expect(screen.getByText("21–40 of 40")).toBeTruthy();
    expect(screen.getByText("Page 2 / 2")).toBeTruthy();
  });

  it("drops the kept total when the first page is read again without one", () => {
    const { rerender } = render(<CursorPagination state={state()} next="cursor-2" total={312} limit={50} count={50} />);
    rerender(<CursorPagination state={state()} next="cursor-2" limit={50} count={50} />);
    rerender(<CursorPagination state={state(true)} next="cursor-3" limit={50} count={50} />);
    expect(screen.getByText("Page 2")).toBeTruthy();
    expect(screen.queryByText(/of 312/)).toBeNull();
  });

  it("keeps a capped first-page total capped on later pages", () => {
    const { rerender } = render(<CursorPagination state={state()} next="cursor-2" total={1000} limit={50} count={50} capped />);
    expect(screen.getByText("1–50 of 1,000+")).toBeTruthy();
    rerender(<CursorPagination state={state(true)} next="cursor-3" limit={50} count={50} />);
    expect(screen.getByText("51–100 of 1,000+")).toBeTruthy();
    expect(screen.getByText("Page 2 / 20+")).toBeTruthy();
  });

  it("starts a later range after the items a trimmed earlier page actually showed", () => {
    const at = (page: number): CursorPaginationState => ({ page, canPrevious: page > 1, previousPage: vi.fn(), nextPage: vi.fn() });
    // Page 1 was trimmed for size: 15 of 20 items, with a next page.
    const { rerender } = render(<CursorPagination state={at(1)} next="15" total={42} limit={20} count={15} />);
    expect(screen.getByText("1–15 of 42")).toBeTruthy();
    rerender(<CursorPagination state={at(2)} next="35" total={42} limit={20} count={20} />);
    expect(screen.getByText("16–35 of 42")).toBeTruthy();
    rerender(<CursorPagination state={at(3)} total={42} limit={20} count={7} />);
    expect(screen.getByText("36–42 of 42")).toBeTruthy();
    rerender(<CursorPagination state={at(2)} next="35" total={42} limit={20} count={20} />);
    expect(screen.getByText("16–35 of 42")).toBeTruthy();
  });
});
