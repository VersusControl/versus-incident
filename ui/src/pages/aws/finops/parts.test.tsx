// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { FinOpsBreakdownRow } from "@/lib/api";
import { ClippedText, InfoTip } from "./parts";
import { placeNote } from "./notePlacement";
import { SpendByService } from "./SpendByService";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("ⓘ note placement", () => {
  it("keeps the note inside a 1440 px window when the ⓘ sits near the right edge", () => {
    const p = placeNote({ left: 1193, top: 300, bottom: 316 }, 80, { width: 1440, height: 900 });
    expect(p.left + p.width).toBeLessThanOrEqual(1440 - 8);
    expect(p.top).toBe(320);
  });

  it("keeps the note inside a 390 px phone", () => {
    for (const left of [138, 146, 148, 370]) {
      const p = placeNote({ left, top: 200, bottom: 216 }, 80, { width: 390, height: 844 });
      expect(p.left).toBeGreaterThanOrEqual(8);
      expect(p.left + p.width).toBeLessThanOrEqual(390 - 8);
    }
  });

  it("narrows the note on a screen smaller than its width", () => {
    const p = placeNote({ left: 10, top: 10, bottom: 26 }, 40, { width: 240, height: 600 });
    expect(p.width).toBe(240 - 16);
    expect(p.left).toBe(8);
  });

  it("opens above the ⓘ when there is no room below", () => {
    const p = placeNote({ left: 20, top: 800, bottom: 816 }, 100, { width: 390, height: 844 });
    expect(p.top).toBe(800 - 4 - 100);
  });

  it("draws the open note at fixed coordinates inside the viewport", async () => {
    vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (this: HTMLElement) {
      if (this.tagName === "BUTTON") return DOMRect.fromRect({ x: 1000, y: 100, width: 16, height: 16 });
      return DOMRect.fromRect({ x: 0, y: 0, width: 256, height: 60 });
    });
    render(<InfoTip label="About change history">Some change history is missing.</InfoTip>);
    await userEvent.click(screen.getByRole("button", { name: "About change history" }));
    const note = screen.getByRole("note");
    expect(note.style.position).toBe("fixed");
    const left = parseFloat(note.style.left);
    const width = parseFloat(note.style.width);
    expect(left + width).toBeLessThanOrEqual(window.innerWidth - 8);
    expect(note.style.visibility).not.toBe("hidden");
  });

  it("does not let a surrounding link act on the ⓘ", async () => {
    render(
      <a href="/aws/finops/increases/x">
        Row <InfoTip label="About change history">Note</InfoTip>
      </a>,
    );
    const button = screen.getByRole("button", { name: "About change history" });
    const event = new MouseEvent("click", { bubbles: true, cancelable: true });
    button.dispatchEvent(event);
    expect(event.defaultPrevented).toBe(true);
  });
});

describe("cut-off text", () => {
  it("is plain text with no tab stop when it fits", () => {
    render(<ClippedText text="instances $667" />);
    const el = screen.getByText("instances $667");
    expect(el.tagName).toBe("SPAN");
    expect(el.hasAttribute("aria-expanded")).toBe(false);
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("becomes a collapsed button only when the text is cut, and expands on Enter", async () => {
    vi.spyOn(HTMLElement.prototype, "scrollWidth", "get").mockReturnValue(300);
    vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(120);
    render(<ClippedText text="APS7-SpotUsage:m7i-flex.2xlarge" />);
    const button = screen.getByRole("button", { name: "APS7-SpotUsage:m7i-flex.2xlarge" });
    expect(button.getAttribute("aria-expanded")).toBe("false");
    button.focus();
    await userEvent.keyboard("{Enter}");
    expect(button.getAttribute("aria-expanded")).toBe("true");
    await userEvent.keyboard("{Enter}");
    expect(screen.getByRole("button").getAttribute("aria-expanded")).toBe("false");
  });

  it("re-checks on resize: text that comes to fit loses the button", () => {
    const wide = vi.spyOn(HTMLElement.prototype, "scrollWidth", "get").mockReturnValue(300);
    vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(120);
    render(<ClippedText text="data transfer $12" />);
    expect(screen.getByRole("button")).toBeTruthy();
    wide.mockReturnValue(100);
    act(() => {
      window.dispatchEvent(new Event("resize"));
    });
    expect(screen.queryByRole("button")).toBeNull();
    expect(screen.getByText("data transfer $12").tagName).toBe("SPAN");
  });
});

describe("spend by service, what is inside", () => {
  const usd = (n: number) => Math.round(n * 1_000_000);
  function row(key: string, label: string, amount: number, inside: Array<[string, string, number]>): FinOpsBreakdownRow {
    return {
      key,
      label,
      amount_micros: usd(amount),
      share_bp: 0,
      inside: inside.map(([k, l, a]) => ({ key: k, label: l, amount_micros: usd(a), share_bp: 0 })),
    };
  }

  it("shows a cut inside line in full on tap", async () => {
    vi.spyOn(HTMLElement.prototype, "scrollWidth", "get").mockReturnValue(300);
    vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(120);
    const line = "instances $667, data transfer $12, storage $9.50";
    render(
      <SpendByService
        showInside
        currency="USD"
        rows={[row("compute.vm", "Virtual machines", 690, [["instances", "instances", 667], ["data transfer", "data transfer", 12], ["storage", "storage", 9.5]])]}
      />,
    );
    const cut = screen.getByRole("button", { name: line });
    expect(cut.className).toContain("truncate");
    await userEvent.click(cut);
    expect(cut.getAttribute("aria-expanded")).toBe("true");
    expect(cut.className).toContain("whitespace-normal");
  });

  it("drops groups that round to $0.00 and a line that only says Other", () => {
    render(
      <SpendByService
        showInside
        currency="USD"
        rows={[
          row("compute.vm", "Virtual machines", 667, [["instances", "instances", 667], ["", "Other", 0.001]]),
          row("network.transfer", "Data transfer", 370, [["", "Other", 370], ["data transfer", "data transfer", 0.002]]),
        ]}
      />,
    );
    const card = screen.getByRole("region", { name: "Spend by service" });
    expect(card.textContent).not.toContain("$0.00");
    expect(within(card).getByText("instances $667")).toBeTruthy();
    expect(within(card).queryByText(/^Other \$370/)).toBeNull();
  });
});
