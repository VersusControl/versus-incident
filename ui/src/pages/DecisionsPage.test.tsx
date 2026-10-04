// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { ToastProvider } from "@/components/Toast";
import { api } from "@/lib/api";
import { DecisionsPage } from "./DecisionsPage";

vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      detectStats: vi.fn().mockResolvedValue({ events: 12, outcome_emitted: 7 }),
      shadowStats: vi.fn().mockResolvedValue({
        events: 0,
        total_signals: 0,
        verdicts: {},
        occurrences: 0,
      }),
      listDetect: vi.fn().mockResolvedValue([]),
    },
  };
});

afterEach(cleanup);

describe("DecisionsPage outcome filter panel", () => {
  it("places Filters beside the tabs in a wrapping row", () => {
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <QueryClientProvider client={queryClient}>
        <ToastProvider>
          <MemoryRouter future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
            <DecisionsPage />
          </MemoryRouter>
        </ToastProvider>
      </QueryClientProvider>,
    );

    const filterButton = screen.getByRole("button", { name: /^Filters/ });
    const tabs = screen.getByRole("tab", { name: "Detect" });
    const row = filterButton.closest(".mb-3.flex");

    expect(row).toBeTruthy();
    expect(row?.className).toContain("flex-wrap");
    expect(tabs.closest(".mb-3.flex")).toBe(row);
  });

  it("keeps outcome counts, applies a status, and clears to All", async () => {
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <QueryClientProvider client={queryClient}>
        <ToastProvider>
          <MemoryRouter future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
            <DecisionsPage />
          </MemoryRouter>
        </ToastProvider>
      </QueryClientProvider>,
    );

    fireEvent.click(screen.getByRole("button", { name: /^Filters/ }));
    const panel = screen.getByRole("dialog", { name: "Filters" });
    const emitted = await within(panel).findByRole("button", { name: /Emitted.*7/ });
    fireEvent.click(emitted);
    expect(emitted.getAttribute("aria-pressed")).toBe("true");

    fireEvent.click(within(panel).getByRole("button", { name: "Clear all" }));
    expect(within(panel).getByRole("button", { name: /All.*12/ }).getAttribute("aria-pressed")).toBe("true");
    expect(within(panel).getByRole("button", { name: /Emitted.*7/ }).getAttribute("aria-pressed")).toBe("false");
    expect(api.listDetect).toHaveBeenCalled();
  });
});