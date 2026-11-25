// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError } from "@/lib/api";
import { RetryableError } from "./RetryableError";

afterEach(() => { cleanup(); vi.useRealTimers(); });

describe("RetryableError for passing Kubernetes answers", () => {
  const answer = (code: string) => new ApiError(503, "server words", { code, error: "server words", action: "Restore or rotate the connector credential, then retry.", retryable: true });

  it.each([
    ["connector_reconnecting", "Reconnecting to the cluster…"],
    ["history_store_unavailable", "Saved history is temporarily unavailable."],
    ["history_store_timeout", "Saved history is temporarily unavailable."],
  ])("reads %s calmly, without the failure lead-in or the server's remedy", (code, text) => {
    vi.useFakeTimers();
    const retry = vi.fn();
    render(<RetryableError error={answer(code)} onRetry={retry} context="Couldn't load issues" />);
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.getByRole("status").textContent).toContain(text);
    expect(document.body.textContent).not.toMatch(/rotate|credential|Couldn't load/i);
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(retry).toHaveBeenCalledOnce();
  });

  it("keeps a real failure as an alert with its remedy", () => {
    vi.useFakeTimers();
    render(<RetryableError error={answer("connector_credential_unresolved")} onRetry={vi.fn()} />);
    expect(screen.getByRole("alert").textContent).toContain("rotate");
  });
});
