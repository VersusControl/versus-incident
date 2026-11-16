// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { ToastProvider } from "@/components/Toast";
import { api, type DetectEvent, type ShadowEvent } from "@/lib/api";
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
        total_occurrences: 0,
        verdict_spike: 0,
        verdict_unknown: 0,
      }),
      listDetect: vi.fn().mockResolvedValue([]),
      listShadow: vi.fn().mockResolvedValue([]),
    },
  };
});

afterEach(cleanup);

function renderAt(path: string) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <ToastProvider>
        <MemoryRouter
          initialEntries={[path]}
          future={{ v7_startTransition: true, v7_relativeSplatPath: true }}
        >
          <DecisionsPage />
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

const detectSpike: DetectEvent = {
  id: "d1",
  timestamp: "2026-07-04T10:00:00Z",
  source: "logs",
  pattern_id: "p-detect",
  template: "tmpl",
  verdict: "spike",
  frequency: 12,
  baseline: 2,
  outcome: "emitted",
  samples: ["detect spike line"],
};

function shadowEvent(over: Partial<ShadowEvent>): ShadowEvent {
  return {
    pattern_id: "p-shadow",
    template: "tmpl",
    source: "logs",
    verdict: "spike",
    sample_message: "shadow spike line",
    count: 7,
    occurrences: 3,
    first_seen: "2026-07-04T09:00:00Z",
    last_seen: "2026-07-04T09:30:00Z",
    ...over,
  };
}

// The stats endpoints return a flat map (verdict_spike / verdict_unknown at
// the top level). These fixtures use exactly that shape, so a page reading a
// nested `verdicts` object shows zeros and fails here.
function mockOneSpikePerLog() {
  vi.mocked(api.detectStats).mockResolvedValue({
    events: 1,
    outcome_emitted: 1,
    verdict_spike: 1,
  });
  vi.mocked(api.shadowStats).mockResolvedValue({
    events: 2,
    total_signals: 9,
    total_occurrences: 4,
    verdict_spike: 1,
    verdict_unknown: 1,
  });
  vi.mocked(api.listDetect).mockResolvedValue([detectSpike]);
  vi.mocked(api.listShadow).mockResolvedValue([
    shadowEvent({}),
    shadowEvent({
      pattern_id: "p-unknown",
      verdict: "unknown",
      sample_message: "unknown line",
    }),
  ]);
}

describe("DecisionsPage verdict counts", () => {
  // Restore the module-level defaults so later suites see the shared mocks.
  afterEach(() => {
    vi.mocked(api.detectStats).mockResolvedValue({ events: 12, outcome_emitted: 7 });
    vi.mocked(api.shadowStats).mockResolvedValue({
      events: 0,
      total_signals: 0,
      total_occurrences: 0,
      verdict_spike: 0,
      verdict_unknown: 0,
    });
    vi.mocked(api.listDetect).mockResolvedValue([]);
    vi.mocked(api.listShadow).mockResolvedValue([]);
  });

  it("shows shadow Spike/Unknown filter counts from the flat stats fields", async () => {
    mockOneSpikePerLog();
    renderAt("/agent/decisions?tab=shadow");

    expect(
      await screen.findByRole("button", { name: "Spikes (1)" }),
    ).toBeTruthy();
    expect(screen.getByRole("button", { name: "Unknown (1)" })).toBeTruthy();
  });

  it("Spike tab badge and subtitle count spikes across both logs and match the rows", async () => {
    mockOneSpikePerLog();
    renderAt("/agent/decisions?tab=spike");

    expect(await screen.findByText("2 spikes detected")).toBeTruthy();
    expect(screen.getByRole("tab", { name: /^Spike\s*2$/ })).toBeTruthy();

    await screen.findByText("shadow spike line");
    expect(screen.getByText("detect spike line")).toBeTruthy();
    expect(screen.queryByText("unknown line")).toBeNull();
  });
});

describe("DecisionsPage detect subtitle", () => {
  it("counts decisions without claiming AI calls", async () => {
    renderAt("/agent/decisions?tab=detect");
    expect(await screen.findByText("12 decisions audited")).toBeTruthy();
    expect(screen.queryByText(/AI calls/)).toBeNull();
  });
});

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