// @vitest-environment jsdom
import { useState } from "react";
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { ConfirmDialog } from "./ConfirmDialog";
import { PeekPanel } from "./PeekPanel";
import { isTabbableCandidate } from "./peekPanelFocus";

afterEach(() => {
  cleanup();
  delete document.body.dataset.modalDepth;
});

function OverlayHarness({ revision = 0 }: { revision?: number }) {
  const [peekOpen, setPeekOpen] = useState(true);
  const [confirmOpen, setConfirmOpen] = useState(false);

  return <>
    <main data-testid="page">Page {revision}</main>
    <PeekPanel
      open={peekOpen}
      onClose={() => setPeekOpen(false)}
      title="Service details"
      expandable
    >
      <label>Notes<input aria-label="Notes" defaultValue="keep me" /></label>
      <button type="button" onClick={() => setConfirmOpen(true)}>Open confirmation</button>
    </PeekPanel>
    {confirmOpen && <ConfirmDialog
      title="Confirm action"
      message="Continue?"
      onConfirm={() => setConfirmOpen(false)}
      onClose={() => setConfirmOpen(false)}
    />}
  </>;
}

describe("PeekPanel", () => {
  it("excludes semantically hidden focus candidates without relying on layout", () => {
    const container = document.createElement("div");
    container.innerHTML = `
      <button data-case="visible">Visible</button>
      <button data-case="negative" tabindex="-1">Negative</button>
      <fieldset disabled><button data-case="disabled">Disabled</button></fieldset>
      <div inert><a data-case="inert" href="#">Inert</a></div>
      <div hidden><button data-case="hidden">Hidden</button></div>
      <details><summary>Summary</summary><button data-case="collapsed">Collapsed</button></details>
      <section role="tabpanel" tabindex="0" data-case="tabpanel">Panel</section>
    `;
    document.body.appendChild(container);
    const candidate = (name: string) => container.querySelector<HTMLElement>(`[data-case="${name}"]`)!;

    expect(isTabbableCandidate(candidate("visible"))).toBe(true);
    expect(isTabbableCandidate(candidate("negative"))).toBe(false);
    expect(isTabbableCandidate(candidate("disabled"))).toBe(false);
    expect(isTabbableCandidate(candidate("inert"))).toBe(false);
    expect(isTabbableCandidate(candidate("hidden"))).toBe(false);
    expect(isTabbableCandidate(candidate("collapsed"))).toBe(false);
    expect(isTabbableCandidate(candidate("tabpanel"))).toBe(true);
    container.remove();
  });

  it("preserves expansion and focused input when an inline onClose callback rerenders", () => {
    const view = render(<OverlayHarness />);
    const dialog = screen.getByRole("dialog", { name: "Details panel" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Expand panel" }));
    const input = within(dialog).getByRole("textbox", { name: "Notes" });
    input.focus();

    view.rerender(<OverlayHarness revision={1} />);

    expect(within(dialog).getByRole("button", { name: "Collapse panel" })).toBeTruthy();
    expect(document.activeElement).toBe(input);
  });

  it("leaves a modal above the peek operable and dismisses only the top layer on Escape", () => {
    render(<OverlayHarness />);
    const page = screen.getByTestId("page") as HTMLElement;
    expect(page.closest("[inert]")).not.toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Open confirmation" }));

    const modal = screen.getByRole("dialog", { name: "Confirm action" });
    expect(modal.closest("[inert]" )).toBeNull();
    expect(document.activeElement).toBe(within(modal).getByRole("button", { name: "Close dialog" }));
    fireEvent.keyDown(document, { key: "Escape" });

    expect(screen.queryByRole("dialog", { name: "Confirm action" })).toBeNull();
    expect(screen.getByRole("dialog", { name: "Details panel" })).toBeTruthy();
    fireEvent.keyDown(document, { key: "Escape" });
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(page.closest("[inert]")).toBeNull();
  });

  it("restores each sibling's original inert state after close", () => {
    const preserved = document.createElement("section");
    preserved.setAttribute("inert", "preserve");
    document.body.appendChild(preserved);
    const view = render(<OverlayHarness />);
    fireEvent.keyDown(document, { key: "Escape" });

    expect(preserved.getAttribute("inert")).toBe("preserve");
    expect(screen.getByTestId("page").closest("[inert]")).toBeNull();
    view.unmount();
    preserved.remove();
  });
});