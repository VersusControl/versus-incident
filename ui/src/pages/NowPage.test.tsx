// @vitest-environment jsdom
import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router-dom";
import { ToastProvider } from "@/components/Toast";
import { NowPage } from "./NowPage";
import {
  api,
  type IncidentStatusCounts,
  type IncidentSummary,
  type OriginCounts,
} from "@/lib/api";

vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      listIncidents: vi.fn(),
      incidentCounts: vi.fn(),
      getAgentConfig: vi.fn().mockResolvedValue({ enable: false }),
      status: vi.fn().mockResolvedValue({ patterns: 0 }),
    },
  };
});

afterEach(cleanup);

function oc(ai: number, webhook: number): OriginCounts {
  return { ai_detect: ai, webhook, total: ai + webhook };
}

// open and all deliberately DIFFER on both origins — a badge reading the
// all-status bucket would show 40/900 instead of 1/2.
const byStatus: IncidentStatusCounts = {
  open: oc(1, 2),
  acked: oc(4, 8),
  resolved: oc(35, 890),
  all: oc(40, 900),
};

function incident(overrides: Partial<IncidentSummary> = {}): IncidentSummary {
  return {
    id: "abcdef1234567890",
    title: "Checkout latency spike",
    source: "ai_detect",
    origin: "ai_detect",
    service: "checkout",
    resolved: false,
    created_at: new Date().toISOString(),
    ...overrides,
  };
}

function renderPageAt(entry = "/now") {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <ToastProvider>
        <MemoryRouter
          initialEntries={[entry]}
          future={{ v7_startTransition: true, v7_relativeSplatPath: true }}
        >
          <Routes>
            <Route path="/now" element={<NowPage />} />
          </Routes>
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

function setup(rows: IncidentSummary[] = []) {
  vi.mocked(api.listIncidents).mockResolvedValue(rows);
  vi.mocked(api.incidentCounts).mockResolvedValue({
    ...oc(byStatus.open.ai_detect + byStatus.acked.ai_detect,
      byStatus.open.webhook + byStatus.acked.webhook),
    by_status: byStatus,
  });
}

describe("NowPage origin badges", () => {
  it("badges both origin tabs with the OPEN count, not the all-status total", async () => {
    setup([
      incident({ id: "ai-open", origin: "ai_detect" }),
      incident({ id: "wh-open", origin: "webhook" }),
    ]);
    renderPageAt();

    // The badge is part of each tab's accessible name once counts land.
    const ai = await screen.findByRole("tab", { name: /^AI Detected 1$/ });
    const webhook = screen.getByRole("tab", { name: /^Webhook \/ Alerts 2$/ });

    // The whole-set numbers must appear on NEITHER tab — one open badge next
    // to one lifetime badge would be worse than the original bug.
    expect(within(ai).queryByText("40")).toBeNull();
    expect(within(webhook).queryByText("900")).toBeNull();
  });

  it("keeps the KPI tiles and the open banner on the same open numbers", async () => {
    setup([incident({ id: "ai-open", origin: "ai_detect" })]);
    renderPageAt();

    // Banner: the server open count for the active (ai_detect) tab.
    const banner = await screen.findByRole("region", {
      name: "Open incidents",
    });
    expect(within(banner).getByText("1 open incident")).toBeTruthy();

    // KPI tiles still read their own status buckets for the active origin.
    expect(screen.getByText("4")).toBeTruthy();
    expect(screen.getByText("35")).toBeTruthy();
  });

  it("labels the top-bar summary as the open split", async () => {
    setup([incident({ id: "ai-open", origin: "ai_detect" })]);
    renderPageAt();

    expect(
      await screen.findByText("Open — AI: 1 · Webhook: 2"),
    ).toBeTruthy();
  });
});
