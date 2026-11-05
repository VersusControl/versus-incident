// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { Stepper } from "./Stepper";

afterEach(cleanup);

const STEPS = [
  { label: "Method" },
  { label: "Check access" },
  { label: "Regions" },
];

describe("Stepper", () => {
  it("marks done, current and upcoming steps and the current one is aria-current", () => {
    render(<Stepper steps={STEPS} current={1} label="Connect account" />);
    const list = screen.getByRole("list", { name: "Connect account" });
    const items = within(list).getAllByRole("listitem");
    expect(items).toHaveLength(3);
    expect(items[0].getAttribute("aria-current")).toBeNull();
    expect(items[1].getAttribute("aria-current")).toBe("step");
    expect(items[2].getAttribute("aria-current")).toBeNull();
    // The completed step reads as done in words too, not by colour alone.
    expect(items[0].textContent).toContain("Method");
    expect(items[1].textContent).toContain("Check access");
    expect(items[2].textContent).toContain("Regions");
  });

  it("makes reachable steps real buttons when selection is wired", () => {
    const onSelect = vi.fn();
    render(<Stepper steps={STEPS} current={1} onSelect={onSelect} />);
    const done = screen.getByRole("button", { name: /Method/ });
    fireEvent.click(done);
    expect(onSelect).toHaveBeenCalledWith(0);

    const upcoming = screen.getByRole("button", { name: /Regions/ });
    expect((upcoming as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(upcoming);
    expect(onSelect).toHaveBeenCalledTimes(1);
  });

  it("renders no buttons when the flow cannot jump backwards", () => {
    render(<Stepper steps={STEPS} current={0} />);
    expect(screen.queryAllByRole("button")).toHaveLength(0);
  });
});
