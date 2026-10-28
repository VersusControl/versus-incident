// @vitest-environment jsdom
import { describe, it, expect, afterEach, beforeEach, vi } from "vitest";
import { act, render, screen, cleanup, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router-dom";
import { ToastProvider } from "@/components/Toast";
import { NowPage } from "./NowPage";
import {
  api,
  ApiError,
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
      listBaselines: vi.fn().mockRejectedValue(new actual.ApiError(404, "community")),
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
  return {
    queryClient: qc,
    ...render(
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
    ),
  };
}

function setup(rows: IncidentSummary[] = []) {
  vi.mocked(api.listIncidents).mockResolvedValue(rows);
  vi.mocked(api.incidentCounts).mockResolvedValue({
    count_window: "30d",
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
    expect(screen.getByText(/counts over last 30d/)).toBeTruthy();
  });
});

describe("NowPage Agent pulse", () => {
  beforeEach(() => {
    setup();
    vi.mocked(api.getAgentConfig).mockResolvedValue({ enable: true } as Awaited<
      ReturnType<typeof api.getAgentConfig>
    >);
    vi.mocked(api.listBaselines).mockReset().mockRejectedValue(new ApiError(404, "community"));
    vi.mocked(api.status).mockResolvedValue({
      patterns: 12,
      shadow_events: 3,
      detect_events: 4,
    } as Awaited<ReturnType<typeof api.status>>);
  });

  it("shows compact current signal activity without runtime or model details", async () => {
    vi.mocked(api.getAgentConfig).mockResolvedValue({
      enable: true, mode: "detect", ai: { enable: true, provider: "openai", model: "o4-mini" },
    } as Awaited<ReturnType<typeof api.getAgentConfig>>);
    vi.mocked(api.listBaselines).mockResolvedValue({
      org: "default",
      count: 4,
      baselines: [
        { type: "metric", confident: true },
        { type: "metric", confident: false },
        { type: "trace", confident: true },
        { type: "trace", confident: false },
      ],
    } as Awaited<ReturnType<typeof api.listBaselines>>);
    renderPageAt();

    const pulse = screen.getByRole("region", { name: "Agent pulse" });
    expect(await within(pulse).findByRole("link", { name: /Logs 12 patterns/ })).toBeTruthy();
    expect(within(pulse).getByRole("heading", { name: "Signal activity" })).toBeTruthy();
    expect(within(pulse).getByText("Current catalog · recorded events")).toBeTruthy();
    expect((await within(pulse).findByRole("link", { name: /Metrics 2 signals 1 ready/ })).getAttribute("href")).toBe("/agent/metrics");
    expect((await within(pulse).findByRole("link", { name: /Traces 2 signals 1 ready/ })).getAttribute("href")).toBe("/agent/traces");
    expect(within(pulse).getByRole("link", { name: /Shadow 3 events/ }).getAttribute("href")).toBe("/agent/decisions?tab=shadow");
    expect(within(pulse).getByRole("link", { name: /Detect 4 events/ }).getAttribute("href")).toBe("/agent/decisions?tab=detect");
    expect(within(pulse).getByRole("link", { name: /Agent overview/ }).getAttribute("href")).toBe("/agent");
    for (const removedContent of ["Runtime mode", "Configured AI model", "o4-mini", "openai"]) {
      expect(within(pulse).queryByText(removedContent, { exact: true })).toBeNull();
    }
  });

  it("keeps zero activity counts navigable", async () => {
    vi.mocked(api.status).mockResolvedValue({
      patterns: 0,
      shadow_events: 0,
      detect_events: 0,
    } as Awaited<ReturnType<typeof api.status>>);
    vi.mocked(api.listBaselines).mockResolvedValue({
      org: "default", count: 0, baselines: [],
    } as Awaited<ReturnType<typeof api.listBaselines>>);
    renderPageAt();

    const pulse = screen.getByRole("region", { name: "Agent pulse" });
    expect(await within(pulse).findByRole("link", { name: /Logs 0 patterns/ })).toBeTruthy();
    expect((await within(pulse).findByRole("link", { name: /Metrics 0 signals/ })).getAttribute("href")).toBe("/agent/metrics");
    expect((await within(pulse).findByRole("link", { name: /Traces 0 signals/ })).getAttribute("href")).toBe("/agent/traces");
    expect(within(pulse).getByRole("link", { name: /Shadow 0 events/ }).getAttribute("href")).toBe("/agent/decisions?tab=shadow");
    expect(within(pulse).getByRole("link", { name: /Detect 0 events/ }).getAttribute("href")).toBe("/agent/decisions?tab=detect");
  });

  it("keeps baseline loading visible until the activity request completes", async () => {
    let resolveBaselines!: (value: Awaited<ReturnType<typeof api.listBaselines>>) => void;
    vi.mocked(api.listBaselines).mockReturnValue(new Promise((resolve) => {
      resolveBaselines = resolve;
    }));
    renderPageAt();

    const pulse = screen.getByRole("region", { name: "Agent pulse" });
    expect(await within(pulse).findByText("Loading metric and trace activity")).toBeTruthy();
    resolveBaselines({ org: "default", count: 0, baselines: [] });
    expect(await within(pulse).findByRole("link", { name: /Metrics 0 signals/ })).toBeTruthy();
  });

  it("keeps baseline authorization failures as an Enterprise lock", async () => {
    renderPageAt();

    const pulse = screen.getByRole("region", { name: "Agent pulse" });
    expect(await within(pulse).findByRole("link", { name: "Enterprise license" })).toBeTruthy();
    expect(within(pulse).getByRole("link", { name: /Logs 12 patterns/ })).toBeTruthy();
    expect(within(pulse).queryByRole("link", { name: /Metrics 0 signals/ })).toBeNull();
    expect(within(pulse).queryByRole("link", { name: /Traces 0 signals/ })).toBeNull();
  });

  it("shows transient baseline failures without presenting them as a license lock", async () => {
    vi.mocked(api.listBaselines).mockRejectedValue(new ApiError(503, "Unavailable"));
    renderPageAt();

    const pulse = screen.getByRole("region", { name: "Agent pulse" });
    expect(await within(pulse).findByText("Couldn't load metric and trace activity", {}, { timeout: 3000 })).toBeTruthy();
    expect(within(pulse).getByRole("link", { name: /Logs 12 patterns/ })).toBeTruthy();
    expect(within(pulse).queryByRole("link", { name: "Enterprise license" })).toBeNull();
  });

  it("retains cached signal and baseline counts after transient refetch failures", async () => {
    const statusData = { patterns: 12, shadow_events: 3, detect_events: 4 } as Awaited<
      ReturnType<typeof api.status>
    >;
    const baselineData = {
      org: "default",
      count: 2,
      baselines: [
        { type: "metric", confident: true },
        { type: "trace", confident: false },
      ],
    } as Awaited<ReturnType<typeof api.listBaselines>>;
    vi.mocked(api.status).mockReset().mockRejectedValue(new ApiError(503, "Unavailable"));
    vi.mocked(api.status).mockResolvedValueOnce(statusData);
    vi.mocked(api.listBaselines).mockReset().mockRejectedValue(new TypeError("Failed to fetch"));
    vi.mocked(api.listBaselines).mockResolvedValueOnce(baselineData);
    const { queryClient } = renderPageAt();
    const pulse = screen.getByRole("region", { name: "Agent pulse" });

    expect(await within(pulse).findByRole("link", { name: /Logs 12 patterns/ })).toBeTruthy();
    expect(await within(pulse).findByRole("link", { name: /Metrics 1 signals 1 ready/ })).toBeTruthy();
    await act(async () => {
      await Promise.all([
        queryClient.refetchQueries({ queryKey: ["status-pulse"], exact: true }),
        queryClient.refetchQueries({ queryKey: ["baselines-pulse"], exact: true }),
      ]);
    });

    expect(within(pulse).getByRole("link", { name: /Logs 12 patterns/ })).toBeTruthy();
    expect(within(pulse).getByRole("link", { name: /Metrics 1 signals 1 ready/ })).toBeTruthy();
    expect(within(pulse).getByRole("link", { name: /Traces 1 signals/ })).toBeTruthy();
    expect(within(pulse).getByText("Couldn't load agent status")).toBeTruthy();
    expect(within(pulse).getByText("Couldn't load metric and trace activity")).toBeTruthy();
    expect(within(pulse).queryByRole("link", { name: "Enterprise license" })).toBeNull();
  });

  it("hides cached signal and premium baseline counts after authorization failures", async () => {
    const statusData = { patterns: 12, shadow_events: 3, detect_events: 4 } as Awaited<
      ReturnType<typeof api.status>
    >;
    const baselineData = {
      org: "default",
      count: 1,
      baselines: [{ type: "metric", confident: true }],
    } as Awaited<ReturnType<typeof api.listBaselines>>;
    vi.mocked(api.status).mockReset().mockRejectedValue(new ApiError(401, "Unauthorized"));
    vi.mocked(api.status).mockResolvedValueOnce(statusData);
    vi.mocked(api.listBaselines).mockReset().mockRejectedValue(new ApiError(401, "Unauthorized"));
    vi.mocked(api.listBaselines).mockResolvedValueOnce(baselineData);
    const { queryClient } = renderPageAt();
    const pulse = screen.getByRole("region", { name: "Agent pulse" });

    expect(await within(pulse).findByRole("link", { name: /Logs 12 patterns/ })).toBeTruthy();
    expect(await within(pulse).findByRole("link", { name: /Metrics 1 signals 1 ready/ })).toBeTruthy();
    await act(async () => {
      await Promise.all([
        queryClient.refetchQueries({ queryKey: ["status-pulse"], exact: true }),
        queryClient.refetchQueries({ queryKey: ["baselines-pulse"], exact: true }),
      ]);
    });

    expect(within(pulse).queryByRole("link", { name: /Logs 12 patterns/ })).toBeNull();
    expect(within(pulse).queryByRole("link", { name: /Metrics 1 signals/ })).toBeNull();
    expect(within(pulse).queryByRole("link", { name: "Enterprise license" })).toBeNull();
    expect(within(pulse).getByText("Couldn't load agent status")).toBeTruthy();
    expect(within(pulse).getByText("Couldn't load metric and trace activity")).toBeTruthy();

    vi.mocked(api.listBaselines).mockRejectedValueOnce(new ApiError(403, "Forbidden"));
    await act(async () => {
      await queryClient.refetchQueries({ queryKey: ["baselines-pulse"], exact: true });
    });
    expect(within(pulse).getByRole("link", { name: "Enterprise license" })).toBeTruthy();
    expect(within(pulse).queryByRole("link", { name: /Metrics 1 signals/ })).toBeNull();
  });

  it("shows the disabled state without requesting signal activity", async () => {
    vi.mocked(api.getAgentConfig).mockResolvedValue({ enable: false } as Awaited<
      ReturnType<typeof api.getAgentConfig>
    >);
    renderPageAt();

    const pulse = screen.getByRole("region", { name: "Agent pulse" });
    expect(await within(pulse).findByText("Agent is disabled")).toBeTruthy();
    expect(within(pulse).getByRole("heading", { name: "Signal activity" })).toBeTruthy();
    expect(within(pulse).queryByRole("link", { name: /Logs|Metrics|Traces|Shadow|Detect/ })).toBeNull();
    expect(api.listBaselines).not.toHaveBeenCalled();
  });
});
