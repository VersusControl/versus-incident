// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ToastProvider } from "./Toast";
import { CopyField } from "./CopyField";

const copyMocks = vi.hoisted(() => ({ copyText: vi.fn() }));

vi.mock("@/lib/clipboard", () => ({ copyText: copyMocks.copyText }));

afterEach(() => {
  cleanup();
  copyMocks.copyText.mockReset();
});

function renderField() {
  return render(
    <ToastProvider>
      <CopyField label="Role ARN" value="arn:aws:iam::123456789012:role/versus-reader" />
    </ToastProvider>,
  );
}

describe("CopyField", () => {
  it("shows the value as read-only mono text with a visible label", () => {
    renderField();
    const input = screen.getByLabelText("Role ARN") as HTMLInputElement;
    expect(input.value).toBe("arn:aws:iam::123456789012:role/versus-reader");
    expect(input.readOnly).toBe(true);
    expect(input.className).toContain("font-mono");
  });

  it("copies the exact value and confirms it", async () => {
    copyMocks.copyText.mockResolvedValue(true);
    renderField();
    fireEvent.click(screen.getByRole("button", { name: "Copy Role ARN" }));
    await waitFor(() => expect(copyMocks.copyText).toHaveBeenCalledWith("arn:aws:iam::123456789012:role/versus-reader"));
    expect(await screen.findByText("Copied")).toBeTruthy();
  });

  it("reports a failed copy instead of pretending it worked", async () => {
    copyMocks.copyText.mockResolvedValue(false);
    renderField();
    fireEvent.click(screen.getByRole("button", { name: "Copy Role ARN" }));
    expect(await screen.findByText("Copy failed")).toBeTruthy();
  });
});
