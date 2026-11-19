// @vitest-environment jsdom
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { Dropdown } from "./Dropdown";

const OPTIONS = [
  { value: "", label: "All 3" },
  { value: "prod", label: "Prod — complete" },
  { value: "shared", label: "Shared — complete" },
  { value: "staging", label: "Staging — as of 09:20" },
];

function Harness({ onChange }: { onChange: (v: string) => void }) {
  const [value, setValue] = useState("");
  return (
    <>
      <Dropdown
        value={value}
        options={OPTIONS}
        aria-label="Accounts"
        onChange={(v) => {
          setValue(v);
          onChange(v);
        }}
      />
      <button type="button">after</button>
    </>
  );
}

function setup() {
  const onChange = vi.fn();
  render(<Harness onChange={onChange} />);
  const button = screen.getByRole("button", { name: "Accounts" });
  return { onChange, button };
}

function activeOption(): string | null {
  const list = screen.getByRole("listbox");
  const id = list.getAttribute("aria-activedescendant");
  return id ? (document.getElementById(id)?.textContent ?? null) : null;
}

beforeEach(() => {
  Element.prototype.scrollIntoView = vi.fn();
});

afterEach(() => cleanup());

describe("Dropdown — keyboard", () => {
  it("opening from the keyboard moves focus to the list", () => {
    const { button } = setup();
    button.focus();
    fireEvent.keyDown(button, { key: "Enter" });
    expect(button.getAttribute("aria-expanded")).toBe("true");
    expect(document.activeElement).toBe(screen.getByRole("listbox"));
    expect(button.getAttribute("aria-controls")).toBe(screen.getByRole("listbox").id);
  });

  it("opening with the mouse also moves focus to the list", () => {
    const { button } = setup();
    fireEvent.click(button);
    expect(document.activeElement).toBe(screen.getByRole("listbox"));
  });

  it("Arrow keys, Home and End move the highlighted option", () => {
    const { button } = setup();
    fireEvent.keyDown(button, { key: "ArrowDown" });
    const list = screen.getByRole("listbox");
    expect(activeOption()).toBe("All 3");
    fireEvent.keyDown(list, { key: "ArrowDown" });
    expect(activeOption()).toBe("Prod — complete");
    fireEvent.keyDown(list, { key: "End" });
    expect(activeOption()).toBe("Staging — as of 09:20");
    fireEvent.keyDown(list, { key: "ArrowDown" });
    expect(activeOption()).toBe("Staging — as of 09:20");
    fireEvent.keyDown(list, { key: "Home" });
    expect(activeOption()).toBe("All 3");
    fireEvent.keyDown(list, { key: "ArrowUp" });
    expect(activeOption()).toBe("All 3");
  });

  it("Enter chooses the highlighted option, closes the list and returns focus to the button", () => {
    const { button, onChange } = setup();
    fireEvent.keyDown(button, { key: "Enter" });
    const list = screen.getByRole("listbox");
    fireEvent.keyDown(list, { key: "ArrowDown" });
    fireEvent.keyDown(list, { key: "Enter" });
    expect(onChange).toHaveBeenCalledWith("prod");
    expect(screen.queryByRole("listbox")).toBeNull();
    expect(document.activeElement).toBe(button);
    expect(button.textContent).toContain("Prod — complete");
  });

  it("Space chooses the highlighted option too", () => {
    const { button, onChange } = setup();
    fireEvent.keyDown(button, { key: " " });
    const list = screen.getByRole("listbox");
    fireEvent.keyDown(list, { key: "End" });
    fireEvent.keyDown(list, { key: " " });
    expect(onChange).toHaveBeenCalledWith("staging");
    expect(document.activeElement).toBe(button);
  });

  it("Escape closes without choosing and returns focus to the button, after a keyboard or a mouse open", () => {
    const { button, onChange } = setup();
    fireEvent.keyDown(button, { key: "Enter" });
    fireEvent.keyDown(screen.getByRole("listbox"), { key: "ArrowDown" });
    fireEvent.keyDown(screen.getByRole("listbox"), { key: "Escape" });
    expect(screen.queryByRole("listbox")).toBeNull();
    expect(button.getAttribute("aria-expanded")).toBe("false");
    expect(document.activeElement).toBe(button);
    fireEvent.click(button);
    fireEvent.keyDown(document.activeElement as Element, { key: "Escape" });
    expect(screen.queryByRole("listbox")).toBeNull();
    expect(document.activeElement).toBe(button);
    expect(onChange).not.toHaveBeenCalled();
  });

  it("opening highlights the current choice", () => {
    const { button } = setup();
    fireEvent.keyDown(button, { key: "Enter" });
    fireEvent.keyDown(screen.getByRole("listbox"), { key: "End" });
    fireEvent.keyDown(screen.getByRole("listbox"), { key: "Enter" });
    fireEvent.keyDown(button, { key: "ArrowDown" });
    expect(activeOption()).toBe("Staging — as of 09:20");
  });
});
