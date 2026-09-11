// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { api, ApiError, type ServiceHealthSnapshot, type ServiceHealthState } from "@/lib/api";
import { ServiceHealthSection } from "./ServiceHealthSection";
import { SERVICE_HEALTH_STATE_COPY } from "@/lib/serviceHealthPresentation";

vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, getServiceHealth: vi.fn() } };
});

const role = vi.hoisted(() => ({ enterprise: false, isAdmin: false, hasSession: false, loading: false }));
vi.mock("@/lib/useEffectiveRole", () => ({
  useEffectiveRole: () => ({ ...role, org: null, role: null, session: {} }),
}));

afterEach(cleanup);

function availability(state: ServiceHealthState, action_id?: string) {
  return { state, action_id };
}

function snapshot(overrides: Partial<ServiceHealthSnapshot> = {}): ServiceHealthSnapshot {
  return {
    snapshot_id: "snap-1",
    generated_at: "2026-09-10T12:00:00Z",
    latest_attempt: "2026-09-10T12:00:00Z",
    latest_success: "2026-09-10T12:00:00Z",
    next_collection_at: "2026-09-10T12:01:00Z",
    settings_revision: 1,
    window_seconds: 300,
    services: [],
    domains: [],
    facets: {},
    coverage: { total_services: 0, observed_services: 0, partial: false },
    capabilities: [
      { family: "logs", measures: { activity: availability("not_configured", "connect_log_source") } },
      { family: "internal", measures: { incidents: availability("ready") } },
      { family: "metrics", measures: { latency: availability("restricted", "review_enterprise_capability") } },
      { family: "traces", measures: { request_context: availability("restricted", "review_enterprise_capability") } },
    ],
    ...overrides,
  };
}

function service(overrides: Partial<ServiceHealthSnapshot["services"][number]> = {}) {
  return {
    org_id: "default",
    service: "checkout",
    domain: "Commerce",
    kind: "unknown",
    severity: "pressure",
    assessment_basis: "Logs + patterns + incidents",
    logs: {
      service: "checkout",
      source_id: "",
      matched_logs: 1284,
      unique_patterns: 12,
      new_patterns: 2,
      unknown_patterns: 1,
      spiking_patterns: 2,
      severity_counts: { warning: 4 },
      latest_observation: "2026-09-10T11:59:00Z",
      estimated: true,
    },
    availability: { logs: availability("ready"), internal: availability("ready") },
    active_incidents: 1,
    ...overrides,
  };
}

function LocationProbe() {
  return <span data-testid="location">{useLocation().pathname}</span>;
}

function renderSection() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={["/agent/services"]} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
        <LocationProbe />
        <Routes>
          <Route path="/agent/services" element={<ServiceHealthSection />} />
          <Route path="/agent/services/:name" element={<div>service detail destination</div>} />
          <Route path="/settings" element={<div>settings destination</div>} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("ServiceHealthSection", () => {
  beforeEach(() => {
    Object.assign(role, { enterprise: false, isAdmin: false, hasSession: false, loading: false });
  });

  it.each([
    ["ready", "Reporting"], ["partial", "Partial"], ["not_configured", "Not connected"],
    ["collecting", "Collecting"], ["no_data", "No data"], ["unsupported", "Unsupported"],
    ["error", "Source error"], ["stale", "Stale"], ["restricted", "Restricted"],
  ] as Array<[ServiceHealthState, string]>)("gives %s a non-color label", (state, label) => {
    expect(SERVICE_HEALTH_STATE_COPY[state].label).toBe(label);
    expect(SERVICE_HEALTH_STATE_COPY[state].detail.length).toBeGreaterThan(0);
  });

  it.each(["not_configured", "collecting", "no_data", "error"] as ServiceHealthState[])(
    "shows current %s status before a clearly isolated sample preview",
    async (state) => {
      vi.mocked(api.getServiceHealth).mockResolvedValue(snapshot({
        capabilities: snapshot().capabilities.map((capability) => capability.family === "logs"
          ? { ...capability, measures: { activity: availability(state, state === "not_configured" ? "connect_log_source" : undefined) } }
          : capability),
      }));
      renderSection();
      expect(await screen.findByText(`Logs: ${SERVICE_HEALTH_STATE_COPY[state].label}`, { exact: true })).toBeTruthy();
      expect(screen.getByText("Example preview - sample data")).toBeTruthy();
      expect(screen.queryByRole("link", { name: "checkout" })).toBeNull();
      expect(screen.getByTestId("service-health-coverage-summary").textContent).toContain("0 with data");
      expect(screen.getByTestId("service-health-coverage-summary").textContent).not.toContain("1,284");
    },
  );

  it("renders internal-only evidence and never contaminates it with preview data or premium KPI placeholders", async () => {
    vi.mocked(api.getServiceHealth).mockResolvedValue(snapshot({
      services: [service({
        assessment_basis: "Patterns + incidents",
        logs: { ...service().logs, matched_logs: 0, latest_observation: undefined },
        availability: { logs: availability("not_configured", "connect_log_source"), internal: availability("ready") },
      })],
      facets: { pressure: 1 },
      coverage: { total_services: 1, observed_services: 1, partial: false },
    }));
    renderSection();
    const coverage = await screen.findByTestId("service-health-coverage-summary");
    expect(screen.queryByTestId("service-health-preview")).toBeNull();
    expect(coverage.textContent).toContain("1 with data");
    expect(screen.queryByText(/Apdex|latency|error rate/i)).toBeNull();
    expect(screen.queryByText(/silent|regression/i)).toBeNull();
  });

  it("keeps stale real evidence instead of showing an automatic preview", async () => {
    vi.mocked(api.getServiceHealth).mockResolvedValue(snapshot({
      services: [service({ availability: { logs: availability("stale", "review_log_source"), internal: availability("no_data") } })],
      facets: { pressure: 1 },
      coverage: { total_services: 1, observed_services: 1, partial: true },
    }));
    renderSection();
    expect(await screen.findByRole("button", { name: "Inspect checkout" })).toBeTruthy();
    expect(screen.queryByTestId("service-health-preview")).toBeNull();
    expect(screen.getByText("Log connection needs attention")).toBeTruthy();
  });

  it("supports evidence modes, filters, direct service inspection and navigation", async () => {
    vi.mocked(api.getServiceHealth).mockResolvedValue(snapshot({
      services: [service()],
      facets: { pressure: 1, nominal: 0 },
      coverage: { total_services: 1, observed_services: 1, partial: false },
    }));
    renderSection();
    fireEvent.change(await screen.findByLabelText("Health state"), { target: { value: "pressure" } });
    expect(screen.getByText("Log events").parentElement?.textContent).toContain("1,284");
    fireEvent.change(screen.getByLabelText("Show data"), { target: { value: "incidents" } });
    expect(screen.queryByText("Log events")).toBeNull();
    expect(screen.getByText("Active incidents").parentElement?.textContent).toContain("1");
    fireEvent.click(screen.getByRole("button", { name: "Inspect checkout" }));
    expect(within(screen.getByRole("dialog")).getByText("checkout")).toBeTruthy();
    fireEvent.click(screen.getByRole("link", { name: "Open service" }));
    expect(screen.getByTestId("location").textContent).toBe("/agent/services/checkout");
  });

  it("shows one setup action to an OSS administrator and guidance without a failing control to a read-only user", async () => {
    vi.mocked(api.getServiceHealth).mockResolvedValue(snapshot());
    const { unmount } = renderSection();
    expect(await screen.findAllByRole("link", { name: "Connect a log source" })).toHaveLength(1);
    unmount();

    Object.assign(role, { enterprise: true, hasSession: true, isAdmin: false });
    renderSection();
    expect(await screen.findByText("Ask an administrator")).toBeTruthy();
    expect(screen.queryByRole("link", { name: "Connect a log source" })).toBeNull();
  });

  it("uses the auth error flow and never renders preview for unauthorized responses", async () => {
    vi.mocked(api.getServiceHealth).mockRejectedValue(new ApiError(401, "unauthorized"));
    renderSection();
    expect(await screen.findByText(/session is no longer authorized/i)).toBeTruthy();
    expect(screen.queryByTestId("service-health-preview")).toBeNull();
  });

  it("keeps real evidence on refresh failure but hides it when access is revoked", async () => {
    vi.mocked(api.getServiceHealth).mockResolvedValue(snapshot({ services: [service()] }));
    renderSection();
    expect(await screen.findByRole("button", { name: "Inspect checkout" })).toBeTruthy();
    vi.mocked(api.getServiceHealth).mockRejectedValue(new ApiError(503, "unavailable"));
    fireEvent.click(screen.getByRole("button", { name: "Refresh service health" }));
    expect(await screen.findByText("Refresh failed. Showing the last assessment.")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Inspect checkout" })).toBeTruthy();
    expect(screen.queryByTestId("service-health-preview")).toBeNull();
    vi.mocked(api.getServiceHealth).mockRejectedValue(new ApiError(403, "forbidden"));
    fireEvent.click(screen.getByRole("button", { name: "Refresh service health" }));
    await waitFor(() => expect(screen.queryByRole("button", { name: "Inspect checkout" })).toBeNull());
    expect(screen.queryByTestId("service-health-preview")).toBeNull();
  });
});