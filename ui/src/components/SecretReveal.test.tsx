// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ToastProvider } from "./Toast";
import { SECRET_REVEAL_MS, SecretReveal } from "./SecretReveal";

const clipboardMocks = vi.hoisted(() => ({ copyText: vi.fn() }));

vi.mock("@/lib/clipboard", () => ({ copyText: clipboardMocks.copyText }));

beforeEach(() => {
  clipboardMocks.copyText.mockReset().mockResolvedValue(true);
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

function renderSecret(reveal: () => Promise<string>) {
  return render(
    <ToastProvider>
      <SecretReveal
        label="External ID"
        prefix="vs_"
        suffix="_9f"
        reveal={reveal}
      />
    </ToastProvider>,
  );
}

function shown(): string | null {
  return screen.getByTestId("secret-value").textContent;
}

describe("SecretReveal", () => {
  it("masks the value with only its prefix and suffix visible", () => {
    renderSecret(vi.fn().mockResolvedValue("vs_secretbody_9f"));
    expect(shown()).toBe("vs_••••••••_9f");
    expect((screen.getByRole("button", { name: "Copy External ID" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("reveals through the caller's audited fetcher and copies the exact value", async () => {
    const reveal = vi.fn().mockResolvedValue("vs_secretbody_9f");
    renderSecret(reveal);

    fireEvent.click(screen.getByRole("button", { name: "Reveal External ID" }));
    await waitFor(() => expect(shown()).toBe("vs_secretbody_9f"));
    expect(reveal).toHaveBeenCalledOnce();

    fireEvent.click(screen.getByRole("button", { name: "Copy External ID" }));
    await waitFor(() => expect(clipboardMocks.copyText).toHaveBeenCalledWith("vs_secretbody_9f"));
    expect(await screen.findByText("Copied")).toBeTruthy();
  });

  it("re-masks itself after 30 seconds without any user action", async () => {
    vi.useFakeTimers();
    renderSecret(vi.fn().mockResolvedValue("vs_secretbody_9f"));

    fireEvent.click(screen.getByRole("button", { name: "Reveal External ID" }));
    await act(async () => {
      await Promise.resolve();
    });
    expect(shown()).toBe("vs_secretbody_9f");

    act(() => {
      vi.advanceTimersByTime(SECRET_REVEAL_MS - 1);
    });
    expect(shown()).toBe("vs_secretbody_9f");

    act(() => {
      vi.advanceTimersByTime(1);
    });
    expect(shown()).toBe("vs_••••••••_9f");
    expect(screen.getByRole("button", { name: "Reveal External ID" })).toBeTruthy();
  });

  it("masks immediately when hidden, and the copy button insists on a reveal", async () => {
    renderSecret(vi.fn().mockResolvedValue("vs_secretbody_9f"));
    fireEvent.click(screen.getByRole("button", { name: "Reveal External ID" }));
    await waitFor(() => expect(shown()).toBe("vs_secretbody_9f"));

    fireEvent.click(screen.getByRole("button", { name: "Hide External ID" }));
    expect(shown()).toBe("vs_••••••••_9f");
    expect(clipboardMocks.copyText).not.toHaveBeenCalled();
  });

  it("keeps the value masked when the reveal fails", async () => {
    renderSecret(vi.fn().mockRejectedValue(new Error("denied")));
    fireEvent.click(screen.getByRole("button", { name: "Reveal External ID" }));
    expect(await screen.findByText("Could not reveal")).toBeTruthy();
    expect(shown()).toBe("vs_••••••••_9f");
  });
});
