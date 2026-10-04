// @vitest-environment jsdom
import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, fireEvent } from "@testing-library/react";
import { within } from "@testing-library/react";
import { ActiveFilterChips, FilterPanel } from "./FilterPanel";
import type { FacetView } from "@/lib/listFilters";

afterEach(cleanup);

const facets: FacetView[] = [
  {
    id: "service",
    label: "Service",
    options: [
      { value: "api", label: "api", count: 4 },
      { value: "web", label: "web", count: 1 },
    ],
    selected: ["web"],
  },
];

describe("FilterPanel", () => {
  it("opens a dialog of facet checkboxes and reports toggles", () => {
    const onToggle = vi.fn();
    render(
      <FilterPanel facets={facets} activeCount={1} onToggle={onToggle} onClear={() => {}} />,
    );
    const button = screen.getByRole("button", { name: /Filters/ });
    expect(button.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(button);
    expect(button.getAttribute("aria-expanded")).toBe("true");

    const dialog = screen.getByRole("dialog", { name: "Filters" });
    const web = screen.getByRole("checkbox", { name: /web/ }) as HTMLInputElement;
    expect(dialog.contains(web)).toBe(true);
    expect(web.checked).toBe(true);

    const api = screen.getByRole("checkbox", { name: /api/ }) as HTMLInputElement;
    fireEvent.click(api);
    expect(onToggle).toHaveBeenCalledWith("service", "api");
    // Shown checked before the URL-backed selection catches up.
    expect(api.checked).toBe(true);
  });

  it("closes on Escape and returns focus to the button", () => {
    render(
      <FilterPanel facets={facets} activeCount={0} onToggle={() => {}} onClear={() => {}} />,
    );
    const button = screen.getByRole("button", { name: /Filters/ });
    fireEvent.click(button);
    fireEvent.keyDown(document, { key: "Escape" });
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.activeElement).toBe(button);
  });

  it("offers a value search for long facets", () => {
    const many: FacetView = {
      id: "signal",
      label: "Signal",
      options: Array.from({ length: 12 }, (_, i) => ({
        value: `s${i}`,
        label: `signal-${i}`,
        count: 1,
      })),
      selected: [],
    };
    render(
      <FilterPanel facets={[many]} activeCount={0} onToggle={() => {}} onClear={() => {}} />,
    );
    fireEvent.click(screen.getByRole("button", { name: /Filters/ }));
    fireEvent.change(screen.getByLabelText("Filter signal values"), {
      target: { value: "signal-11" },
    });
    expect(screen.getAllByRole("checkbox")).toHaveLength(1);
  });

  it("renders controlled filters in the dialog and delegates Clear all", () => {
    const onClear = vi.fn();
    render(
      <FilterPanel
        facets={[]}
        activeCount={1}
        onToggle={() => {}}
        onClear={onClear}
        controls={<div role="group" aria-label="Learning scope">Scope choices</div>}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: /Filters/ }));

    const dialog = screen.getByRole("dialog", { name: "Filters" });
    expect(dialog.contains(screen.getByRole("group", { name: "Learning scope" }))).toBe(true);
    fireEvent.click(within(dialog).getByRole("button", { name: "Clear all" }));
    expect(onClear).toHaveBeenCalledOnce();
  });
});

describe("ActiveFilterChips", () => {
  it("renders nothing without chips", () => {
    const { container } = render(
      <ActiveFilterChips chips={[]} onRemove={() => {}} onClear={() => {}} />,
    );
    expect(container.firstChild).toBeNull();
  });

  it("removes one chip or clears all", () => {
    const onRemove = vi.fn();
    const onClear = vi.fn();
    render(
      <ActiveFilterChips
        chips={[{ facetId: "service", value: "api", label: "Service: api" }]}
        onRemove={onRemove}
        onClear={onClear}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Remove filter Service: api" }));
    expect(onRemove).toHaveBeenCalledWith("service", "api");
    fireEvent.click(screen.getByRole("button", { name: "Clear all" }));
    expect(onClear).toHaveBeenCalled();
  });
});
