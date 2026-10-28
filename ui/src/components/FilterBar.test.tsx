// @vitest-environment jsdom
import { describe, it, expect, afterEach } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";
import { FilterBar } from "./FilterBar";

// Component tests for the shared FilterBar. Its whole job is to enforce ONE
// canonical left-to-right order on every list page — tabs → search → actions —
// so those invariants are pinned here.
afterEach(cleanup);

describe("FilterBar", () => {
  it("renders slots in the canonical order tabs → search → actions", () => {
    const { container } = render(
      <FilterBar
        tabs={<span data-testid="tabs">tabs</span>}
        search={<span data-testid="search">search</span>}
        actions={<span data-testid="actions">actions</span>}
      />,
    );
    const order = Array.from(
      container.querySelectorAll("[data-testid]"),
    ).map((el) => el.getAttribute("data-testid"));
    expect(order).toEqual(["tabs", "search", "actions"]);
  });

  it("pushes actions to the right in their own container", () => {
    render(
      <FilterBar actions={<span data-testid="actions">actions</span>} />,
    );
    const wrapper = screen.getByTestId("actions").parentElement as HTMLElement;
    expect(wrapper.className).toContain("ml-auto");
  });

  it("omits any slot that is not provided", () => {
    render(<FilterBar search={<span data-testid="search">search</span>} />);
    expect(screen.getByTestId("search")).toBeTruthy();
    expect(screen.queryByTestId("tabs")).toBeNull();
    expect(screen.queryByTestId("actions")).toBeNull();
  });
});
