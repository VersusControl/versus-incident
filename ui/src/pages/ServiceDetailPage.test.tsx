// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { ToastProvider } from "@/components/Toast";
import { api, ApiError, getSsoSession, type BaselineRow } from "@/lib/api";
import { ServiceDetailPage } from "./ServiceDetailPage";

vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return {
    ...actual,
    getSsoSession: vi.fn().mockRejectedValue(new actual.ApiError(401, "no session")),
    api: {
      ...actual.api,
      getServiceDetail: vi.fn(),
      getServiceIntel: vi.fn(),
      getLearnExclusions: vi.fn(),
      setLearnExclusions: vi.fn(),
      setServiceLearnExclusion: vi.fn(),
    },
  };
});

afterEach(() => {
  document.body.innerHTML = "";
});

function signal(type: "metric" | "trace", name: string): BaselineRow {
  return {
    type,
    source: type === "metric" ? "prometheus" : "traces",
    service: "checkout",
    signal: name,
    kind: "traffic",
    expected_mean: 1,
    expected_std: 0,
    unit: "",
    display_mean: 1,
    display_std: 0,
    confident: true,
    observations: 10,
    threshold: 5,
    last_updated: new Date().toISOString(),
    readiness: { ready: true, observations: 10, threshold: 5 },
  };
}

function renderPage() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <ToastProvider>
        <MemoryRouter
          initialEntries={["/agent/services/checkout"]}
          future={{ v7_startTransition: true, v7_relativeSplatPath: true }}
        >
          <Routes>
            <Route path="/agent/services/:name" element={<ServiceDetailPage />} />
          </Routes>
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

describe("ServiceDetailPage intel cards", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.mocked(getSsoSession).mockRejectedValue(new ApiError(401, "no session"));
    vi.mocked(api.getServiceDetail).mockResolvedValue({
      service: "checkout",
      first_seen: new Date().toISOString(),
      in_grace: false,
      grace_seconds_remaining: 0,
      patterns: [],
      incidents: { window_days: 30, count: 0, severities: {}, recent: [] },
      counts: { patterns: 0, incidents: 0 },
    });
    vi.mocked(api.getLearnExclusions).mockResolvedValue({ services: [], metrics: [], patterns: [] });
  });

  it("renders sibling cards from one intel request and keeps exclusions in Metrics", async () => {
    vi.mocked(api.getServiceIntel).mockResolvedValue({
      service: "checkout",
      metrics: [signal("metric", "requests_total")],
      traces: [signal("trace", "GET /checkout")],
    });
    renderPage();

    await screen.findByText("requests_total");
    const metrics = screen.getByRole("heading", { name: "Metrics" });
    const traces = screen.getByRole("heading", { name: "Traces" });
    expect(within(metrics.closest("section")!).getByText("requests_total")).toBeTruthy();
    expect(within(metrics.closest("section")!).getByLabelText("Ignore requests_total")).toBeTruthy();
    expect(within(traces.closest("section")!).getByText("GET /checkout")).toBeTruthy();
    expect(within(traces.closest("section")!).queryByRole("checkbox")).toBeNull();
    await waitFor(() => expect(api.getServiceIntel).toHaveBeenCalledTimes(1));
  });

  it("renders an independent empty state in each card", async () => {
    vi.mocked(api.getServiceIntel).mockResolvedValue({ service: "checkout", metrics: [], traces: [] });
    renderPage();
    expect(await screen.findByText("No learned metrics for this service yet.")).toBeTruthy();
    expect(screen.getByText("No learned traces for this service yet.")).toBeTruthy();
  });

  it("orders severity pills for triage and discloses the recent cap", async () => {
    vi.mocked(api.getServiceDetail).mockResolvedValue({
      service: "checkout",
      first_seen: new Date().toISOString(),
      in_grace: false,
      grace_seconds_remaining: 0,
      patterns: [],
      incidents: {
        window_days: 30,
        count: 12,
        severities: { low: 3, critical: 2, high: 4, unknown: 1, medium: 2 },
        recent: Array.from({ length: 10 }, (_, index) => ({
          id: `incident-${index}`,
          title: `Incident ${index}`,
          severity: "high",
          created_at: new Date(Date.now() - index * 1000).toISOString(),
        })),
      },
      counts: { patterns: 0, incidents: 12 },
    });
    vi.mocked(api.getServiceIntel).mockResolvedValue({ service: "checkout", metrics: [], traces: [] });
    renderPage();
    expect(await screen.findByText("showing latest 10")).toBeTruthy();
    const labels = ["critical 2", "high 4", "medium 2", "low 3", "unknown 1"];
    for (let index = 0; index < labels.length - 1; index++) {
      const position = screen.getByText(labels[index]).compareDocumentPosition(screen.getByText(labels[index + 1]));
      expect(position & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    }
  });

  it("renders an independent error state in each card", async () => {
    vi.mocked(api.getServiceIntel).mockRejectedValue(new ApiError(500, "failed"));
    renderPage();
    expect(await screen.findByText("Couldn't load metrics", {}, { timeout: 3000 })).toBeTruthy();
    expect(screen.getByText("Couldn't load traces")).toBeTruthy();
  });

  it.each([403, 404])("preserves locked behavior for status %s", async (status) => {
    vi.mocked(api.getServiceIntel).mockRejectedValue(new ApiError(status, "locked"));
    renderPage();
    expect(await screen.findByText("Metrics learning is an Enterprise capability")).toBeTruthy();
    expect(screen.getByText("Traces learning is an Enterprise capability")).toBeTruthy();
  });
});