// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { ProportionalBarList } from "./ProportionalBarList";

afterEach(cleanup);

describe("ProportionalBarList", () => {
  it("renders vertical comparative bars with exact accessible values", () => {
    const { container } = render(
      <ProportionalBarList
        label="Signals learned"
        rows={[
          ["Logs", 12],
          ["Metrics", 6],
          ["Traces", 3],
        ]}
      />,
    );

    expect(screen.getByRole("progressbar", { name: "Logs: 12" })).toBeTruthy();
    expect(screen.getByRole("progressbar", { name: "Metrics: 6" })).toBeTruthy();
    expect(screen.getByRole("progressbar", { name: "Traces: 3" })).toBeTruthy();
    expect(screen.getByText("12")).toBeTruthy();
    const fills = container.querySelectorAll('[role="progressbar"]');
    expect((fills[0] as HTMLElement).style.height).toBe("100%");
    expect((fills[1] as HTMLElement).style.height).toBe("50%");
    expect((fills[2] as HTMLElement).style.height).toBe("25%");
    expect((fills[0] as HTMLElement).style.width).toBe("");
    expect(container.querySelectorAll(".border-t")).toHaveLength(4);
  });

  it("lets long labels wrap without widening the card", () => {
    render(
      <ProportionalBarList
        label="Long labels"
        rows={[["an_unusually_long_unbroken_key", 1]]}
      />,
    );
    expect(screen.getByText("an_unusually_long_unbroken_key").className).toContain(
      "[overflow-wrap:anywhere]",
    );
  });
});