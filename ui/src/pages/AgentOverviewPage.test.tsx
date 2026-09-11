// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { AgentOverviewPage } from "./AgentOverviewPage";
import { ApiError, api, type AgentConfigView, type BaselineRow } from "@/lib/api";

vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, getAgentConfig: vi.fn(), status: vi.fn(), shadowStats: vi.fn(),
    detectStats: vi.fn(), listPatterns: vi.fn(), listShadow: vi.fn(), listServices: vi.fn(), listBaselines: vi.fn(), getServiceHealth: vi.fn() } };
});
afterEach(cleanup);

function setViewport(mobile: boolean) {
  Object.defineProperty(window, "matchMedia", {
    value: vi.fn(() => ({ matches: mobile, addEventListener: vi.fn(), removeEventListener: vi.fn() })),
    configurable: true,
  });
}

function renderPage(view = "services", mobile = false) {
  setViewport(mobile);
  return render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
    <MemoryRouter initialEntries={[`/agent?view=${view}`]}><AgentOverviewPage /></MemoryRouter>
  </QueryClientProvider>);
}

beforeEach(() => {
  vi.clearAllMocks();
  setViewport(false);
  vi.mocked(api.getAgentConfig).mockResolvedValue({ enable: true, mode: "detect", ai: { enable: false } } as AgentConfigView);
  vi.mocked(api.status).mockResolvedValue({ patterns: 7, dirty: false });
  vi.mocked(api.shadowStats).mockResolvedValue({ events: 8, total_signals: 20, verdicts: { spike: 8 }, occurrences: 8 });
  vi.mocked(api.detectStats).mockResolvedValue({ outcome_emitted: 6, outcome_cached: 2, verdict_spike: 3, severity_high: 4 });
  vi.mocked(api.listPatterns).mockResolvedValue([]);
  vi.mocked(api.listShadow).mockResolvedValue([]);
  vi.mocked(api.listBaselines).mockRejectedValue(new ApiError(403, "community"));
  vi.mocked(api.getServiceHealth).mockResolvedValue({ snapshot_id: "", generated_at: "2026-09-10T12:00:00Z", latest_attempt: "", next_collection_at: "", settings_revision: 0, window_seconds: 300, services: [], domains: [], facets: {}, coverage: { total_services: 0, observed_services: 0, partial: false }, capabilities: [{ family: "logs", measures: { activity: { state: "not_configured" } } }] });
});

describe("Agent Overview workspace", () => {
  it("stacks every section on desktop instead of tabs", async () => {
    renderPage();
    expect(await screen.findByRole("heading", { name: "Service Heatmap" })).toBeTruthy();
    expect(await screen.findByRole("heading", { name: "Agent Activity" })).toBeTruthy();
    expect(await screen.findByRole("heading", { name: "Learning" })).toBeTruthy();
    expect(await screen.findByText("Log spike")).toBeTruthy();
    expect(screen.queryByRole("navigation", { name: "Overview views" })).toBeNull();
    expect(screen.queryByText("Learning and activity history")).toBeNull();
  });

  it("leads each section title with its own icon", async () => {
    renderPage();
    for (const name of ["Service Heatmap", "Agent Activity", "Learning"]) {
      expect((await screen.findByRole("heading", { name })).querySelector("svg")).toBeTruthy();
    }
  });

  it("uses tabs on mobile and loads only the selected section", async () => {
    renderPage("services", true);
    expect(await screen.findByRole("heading", { name: "Service Heatmap" })).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Agent Activity" })).toBeNull();
    expect(api.detectStats).not.toHaveBeenCalled();
    expect(api.listPatterns).not.toHaveBeenCalled();

    fireEvent.click(within(screen.getByRole("navigation", { name: "Overview views" })).getByRole("button", { name: "Activity" }));
    expect(await screen.findByRole("heading", { name: "Agent Activity" })).toBeTruthy();
    expect(await screen.findByText("Log spike")).toBeTruthy();
    expect(screen.getByText("Recorded totals")).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Service Heatmap" })).toBeNull();
    expect(screen.queryByText("Disk in sync")).toBeNull();
  });

  it("shows independent learning counts without comparing different signal units", async () => {
    vi.mocked(api.listBaselines).mockResolvedValue({ baselines: [{ type: "metric", confident: true } as BaselineRow, { type: "trace", confident: false } as BaselineRow] });
    renderPage("learning");
    expect(await screen.findByText("7")).toBeTruthy();
    expect(await screen.findByText("No log patterns learned yet")).toBeTruthy();
    expect(screen.getByText("Metric baselines")).toBeTruthy();
    expect(screen.getByText("Trace baselines")).toBeTruthy();
    expect(screen.queryByRole("progressbar")).toBeNull();
  });

  it("keeps Enterprise restrictions distinct from failed baseline reads", async () => {
    const { unmount } = renderPage("learning");
    expect(await screen.findAllByText("Enterprise")).toHaveLength(2);
    unmount();
    vi.mocked(api.listBaselines).mockRejectedValue(new ApiError(503, "unavailable"));
    renderPage("learning");
    expect(await screen.findByText("Couldn't load baselines")).toBeTruthy();
    expect(screen.queryByText("Enterprise")).toBeNull();
  });

  it("leaves agent status to the top bar without a duplicate settings action", async () => {
    vi.mocked(api.getAgentConfig).mockResolvedValue({ enable: false } as AgentConfigView);
    renderPage();
    expect(await screen.findByText("agent off")).toBeTruthy();
    expect(await screen.findByRole("heading", { name: "Service Heatmap" })).toBeTruthy();
    expect(screen.queryByRole("link", { name: "Agent settings" })).toBeNull();
    expect(screen.getAllByRole("link", { name: "Service Health Timing settings" })).toHaveLength(1);
  });

  it("renders activity loading and failure without a false zero", async () => {
    vi.mocked(api.detectStats).mockReturnValue(new Promise(() => {}));
    const { unmount } = renderPage("activity");
    expect(screen.getByLabelText("Loading Detection results")).toBeTruthy();
    unmount();
    vi.mocked(api.detectStats).mockRejectedValue(new ApiError(503, "unavailable"));
    renderPage("activity");
    expect(await screen.findByText("Couldn't load decisions")).toBeTruthy();
    expect(screen.getAllByText("Unavailable").length).toBeGreaterThan(0);
  });
});