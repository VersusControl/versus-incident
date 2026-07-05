// @vitest-environment jsdom
import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, fireEvent } from "@testing-library/react";
import { Modal } from "./Modal";

// Component tests for the shared Modal base. The behaviour that matters (Item 4
// of the founder's fix): the dialog renders as a VIEWPORT overlay via a portal
// to document.body, so it is never clipped to whatever ancestor happens to
// establish a containing block (e.g. the incident-detail PageHeader's
// backdrop-blur, which trapped the old inline fixed modal to the header strip).
afterEach(cleanup);

describe("Modal overlay (Item 4: portal to body, not clipped)", () => {
  it("portals the dialog out to document.body, escaping a clipping ancestor", () => {
    // A wrapper that WOULD clip an inline fixed child (overflow + a transform,
    // which establishes a containing block for fixed descendants).
    const { container } = render(
      <div
        data-testid="clipping-ancestor"
        style={{ overflow: "hidden", transform: "translateZ(0)", height: "40px" }}
      >
        <Modal title="Resolve incident" onClose={() => {}}>
          <p>body</p>
        </Modal>
      </div>,
    );

    const dialog = screen.getByRole("dialog");
    // The dialog is NOT inside the render container / clipping ancestor…
    expect(container.contains(dialog)).toBe(false);
    // …it lives directly under document.body (the portal target).
    expect(document.body.contains(dialog)).toBe(true);
  });

  it("renders a full-viewport fixed backdrop centered on the viewport", () => {
    render(
      <Modal title="Resolve incident" onClose={() => {}}>
        <p>body</p>
      </Modal>,
    );
    const dialog = screen.getByRole("dialog");
    const backdrop = dialog.parentElement as HTMLElement;
    // A viewport-level overlay: fixed + pinned to all edges, centered content.
    expect(backdrop.className).toContain("fixed");
    expect(backdrop.className).toContain("inset-0");
    expect(backdrop.className).toContain("items-center");
    expect(backdrop.className).toContain("justify-center");
  });

  it("is dismissible: Escape and backdrop click both close", () => {
    const onClose = vi.fn();
    render(
      <Modal title="Resolve incident" onClose={onClose}>
        <p>body</p>
      </Modal>,
    );
    fireEvent.keyDown(document, { key: "Escape" });
    expect(onClose).toHaveBeenCalledTimes(1);

    const backdrop = screen.getByRole("dialog").parentElement as HTMLElement;
    fireEvent.click(backdrop);
    expect(onClose).toHaveBeenCalledTimes(2);
  });
});
