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
});
