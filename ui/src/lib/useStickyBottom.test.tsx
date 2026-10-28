// @vitest-environment jsdom
import { fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { describe, expect, it } from "vitest";
import { useStickyBottom } from "./useStickyBottom";

function Harness() {
  const [version, setVersion] = useState(0);
  const sticky = useStickyBottom(version);
  return (
    <>
      <div ref={sticky.scrollRef} onScroll={sticky.onScroll} data-testid="scroll" />
      {!sticky.following && <button onClick={sticky.scrollToBottom}>Bottom</button>}
      <button onClick={() => setVersion((value) => value + 1)}>Append</button>
    </>
  );
}

describe("useStickyBottom", () => {
  it("stops following after user scroll and resumes on explicit request", () => {
    render(<Harness />);
    const element = screen.getByTestId("scroll");
    Object.defineProperties(element, {
      scrollHeight: { value: 1000, configurable: true },
      clientHeight: { value: 300, configurable: true },
      scrollTop: { value: 200, writable: true, configurable: true },
    });
    fireEvent.scroll(element);
    expect(screen.getByRole("button", { name: "Bottom" })).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Append" }));
    expect(element.scrollTop).toBe(200);

    fireEvent.click(screen.getByRole("button", { name: "Bottom" }));
    expect(element.scrollTop).toBe(1000);
    expect(screen.queryByRole("button", { name: "Bottom" })).toBeNull();
  });
});