// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { MemoryRouter } from "react-router-dom";
import { Breadcrumb } from "./Breadcrumb";

afterEach(cleanup);

function renderCrumbs() {
  return render(
    <MemoryRouter>
      <Breadcrumb items={[{ label: "AWS", to: "/aws" }, { label: "Settings" }]} />
    </MemoryRouter>,
  );
}

describe("Breadcrumb", () => {
  it("links every crumb but the current page", () => {
    renderCrumbs();
    const nav = screen.getByRole("navigation", { name: "Breadcrumb" });
    expect(nav).toBeTruthy();
    expect(screen.getByRole("link", { name: "AWS" }).getAttribute("href")).toBe("/aws");
    expect(screen.queryByRole("link", { name: "Settings" })).toBeNull();
    expect(screen.getByText("Settings").getAttribute("aria-current")).toBe("page");
  });

  it("separates the crumbs with a decoration screen readers ignore", () => {
    const { container } = renderCrumbs();
    const separators = container.querySelectorAll("svg[aria-hidden='true']");
    expect(separators).toHaveLength(1);
  });

  it("renders nothing when there is no trail", () => {
    const { container } = render(
      <MemoryRouter>
        <Breadcrumb items={[]} />
      </MemoryRouter>,
    );
    expect(container.querySelector("nav")).toBeNull();
  });
});
