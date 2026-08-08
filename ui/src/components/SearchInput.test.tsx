// @vitest-environment jsdom
import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, fireEvent } from "@testing-library/react";
import { SearchInput } from "./SearchInput";

// Component tests for the shared SearchInput. It replaces every hand-rolled
// search block across the list pages, so the invariants that pages (and the
// global "/"-to-focus shortcut) rely on are pinned here.
afterEach(cleanup);

describe("SearchInput", () => {
  it("renders the placeholder and mirrors the controlled value", () => {
    render(
      <SearchInput value="abc" onChange={() => {}} placeholder="Search things…" />,
    );
    const input = screen.getByPlaceholderText("Search things…") as HTMLInputElement;
    expect(input.value).toBe("abc");
  });

  it("labels the input for screen readers, defaulting to the placeholder", () => {
    const { rerender } = render(
      <SearchInput value="" onChange={() => {}} placeholder="Search things…" />,
    );
    expect(screen.getByLabelText("Search things…")).toBeTruthy();

    rerender(
      <SearchInput
        value=""
        onChange={() => {}}
        placeholder="Search things…"
        ariaLabel="Search widgets"
      />,
    );
    expect(screen.getByLabelText("Search widgets")).toBeTruthy();
  });

  it("calls onChange with the typed value", () => {
    const onChange = vi.fn();
    render(
      <SearchInput value="" onChange={onChange} placeholder="Search things…" />,
    );
    fireEvent.change(screen.getByPlaceholderText("Search things…"), {
      target: { value: "hello" },
    });
    expect(onChange).toHaveBeenCalledWith("hello");
  });

  it("shows a clear button only when there is a value; clearing empties and refocuses", () => {
    const onChange = vi.fn();
    const { rerender } = render(
      <SearchInput value="" onChange={onChange} placeholder="Search…" />,
    );
    // Empty: no clear button.
    expect(screen.queryByLabelText("Clear search")).toBeNull();

    rerender(<SearchInput value="q" onChange={onChange} placeholder="Search…" />);
    const clear = screen.getByLabelText("Clear search");
    fireEvent.click(clear);
    expect(onChange).toHaveBeenCalledWith("");
    // Focus returns to the input so the operator keeps typing.
    expect(document.activeElement).toBe(screen.getByPlaceholderText("Search…"));
  });

  it("wires the global '/'-focus hook (data-page-search) when shortcut is on and drops it when off", () => {
    const { rerender } = render(
      <SearchInput value="" onChange={() => {}} placeholder="Search…" />,
    );
    // Default: shortcut on → the marker + kbd hint are present.
    expect(document.querySelector("[data-page-search]")).not.toBeNull();
    expect(screen.getByText("/")).toBeTruthy();

    rerender(
      <SearchInput
        value=""
        onChange={() => {}}
        placeholder="Search…"
        shortcut={false}
      />,
    );
    expect(document.querySelector("[data-page-search]")).toBeNull();
    expect(screen.queryByText("/")).toBeNull();
  });

  it("passes through a data-testid", () => {
    render(
      <SearchInput
        value=""
        onChange={() => {}}
        placeholder="Search…"
        data-testid="my-search"
      />,
    );
    expect(screen.getByTestId("my-search")).toBeTruthy();
  });
});
