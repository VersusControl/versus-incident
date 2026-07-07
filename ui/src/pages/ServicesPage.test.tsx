// @vitest-environment jsdom
import { describe, it, expect, afterEach, beforeEach, vi } from "vitest";
import { render, screen, cleanup, fireEvent, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route, useLocation } from "react-router-dom";
import { ToastProvider } from "@/components/Toast";
import { ServicesPage } from "./ServicesPage";
import { api, getSsoSession, type ServiceInfo } from "@/lib/api";

// The Services table row has a per-row eye that opens a PEEK slide-out (rows
// never navigate, and the service NAME is not a link). The peek fetches the
// service detail for its pattern/incident counts and its footer button opens
// the full service detail page. The deployment / license probes answer 403
// (community / OSS) so the enterprise Ignore controls stay absent.
vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return {
    ...actual,
    // getSsoSession is a module-level export (not on `api`) that useEffectiveRole
    // reads directly — default it to "no session" so the enterprise surface stays
    // absent unless a test opts in.
    getSsoSession: vi
      .fn()
      .mockRejectedValue(new actual.ApiError(401, "no session")),
    api: {
      ...actual.api,
      listServices: vi.fn(),
      getServiceDetail: vi.fn(),
      listBaselines: vi
        .fn()
        .mockRejectedValue(new actual.ApiError(403, "community")),
      getSSODeployment: vi
        .fn()
        .mockRejectedValue(new actual.ApiError(403, "community")),
      getLearnExclusions: vi
        .fn()
        .mockResolvedValue({ services: [], metrics: [], patterns: [] }),
      setServiceLearnExclusion: vi
        .fn()
        .mockResolvedValue({ services: [], metrics: [], patterns: [] }),
    },
  };
});

afterEach(cleanup);

function svc(overrides: Partial<ServiceInfo> = {}): ServiceInfo {
  return {
    first_seen: new Date().toISOString(),
    manual: false,
    in_grace: false,
    grace_seconds_remaining: 0,
    ...overrides,
  };
}

function LocationProbe() {
  const loc = useLocation();
  return <div data-testid="path">{loc.pathname}</div>;
}

function renderPage() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <ToastProvider>
        <MemoryRouter initialEntries={["/agent/services"]}>
          <LocationProbe />
          <Routes>
            <Route path="/agent/services" element={<ServicesPage />} />
            <Route
              path="/agent/services/:name"
              element={<div>service detail</div>}
            />
          </Routes>
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

describe("ServicesPage row actions", () => {
  beforeEach(() => {
    vi.mocked(api.listServices).mockResolvedValue({ checkout: svc() });
    vi.mocked(api.getServiceDetail).mockResolvedValue({
      service: "checkout",
      first_seen: new Date().toISOString(),
      in_grace: false,
      grace_seconds_remaining: 0,
      patterns: [],
      incidents: {
        window_days: 30,
        count: 0,
        severities: {},
        recent: [],
      },
      counts: { patterns: 3, incidents: 1 },
    });
  });

  it("opens a peek from the per-row eye without navigating", async () => {
    renderPage();
    const eye = await screen.findByLabelText("View service checkout");
    fireEvent.click(eye);
    // The peek opens in place — no navigation happens.
    expect(screen.getByTestId("path").textContent).toBe("/agent/services");
    expect(screen.getByRole("dialog")).toBeTruthy();
    // Footer button links to the full service detail page.
    expect(
      screen.getByRole("link", { name: /Open full page/ }),
    ).toBeTruthy();
  });

  it("navigates to the detail page from the peek footer button", async () => {
    renderPage();
    fireEvent.click(await screen.findByLabelText("View service checkout"));
    fireEvent.click(screen.getByRole("link", { name: /Open full page/ }));
    expect(screen.getByTestId("path").textContent).toBe(
      "/agent/services/checkout",
    );
  });

  it("does not navigate on a plain row click (only the eye opens the peek)", async () => {
    renderPage();
    const eye = await screen.findByLabelText("View service checkout");
    const row = eye.closest("tr") as HTMLTableRowElement;
    // The service name is plain text now — no stray link makes the row navigate.
    expect(
      screen.queryByRole("link", { name: "checkout" }),
    ).toBeNull();
    fireEvent.click(row);
    expect(screen.getByTestId("path").textContent).toBe("/agent/services");
  });
});

// The Services page unifies its Active | Ignored presentation with the logs and
// metrics/traces pages: ONE table with a SegmentedControl scope toggle (count
// badges) that filters rows by whether the service is held out of learning —
// the toggle appearing only when the enterprise Disable-Learn exclude surface
// is licensed to an admin, and absent otherwise (scope stays "active").
describe("ServicesPage Active/Ignored scope", () => {
  // Render the enterprise exclude surface: a licensed binary (baselines probe
  // succeeds), an admin session (deployment org + admin whoami), and a policy
  // that already ignores one of the two services.
  function renderScoped(initialEntry = "/agent/services") {
    vi.mocked(api.listServices).mockResolvedValue({
      checkout: svc(),
      payments: svc(),
    });
    vi.mocked(api.getServiceDetail).mockResolvedValue({
      service: "checkout",
      first_seen: new Date().toISOString(),
      in_grace: false,
      grace_seconds_remaining: 0,
      patterns: [],
      incidents: { window_days: 30, count: 0, severities: {}, recent: [] },
      counts: { patterns: 0, incidents: 0 },
    });
    vi.mocked(api.listBaselines).mockResolvedValue({
      type: "metric",
      count: 0,
      baselines: [],
    });
    vi.mocked(api.getSSODeployment).mockResolvedValue({ org: "acme" });
    vi.mocked(getSsoSession).mockResolvedValue({
      org: "acme",
      email: "admin@acme.test",
      subject: "admin",
      mfa: false,
      role: "admin",
      issued_at: new Date().toISOString(),
      expires_at: new Date(Date.now() + 3_600_000).toISOString(),
    });
    // "checkout" is held out of learning; "payments" is active.
    vi.mocked(api.getLearnExclusions).mockResolvedValue({
      services: ["checkout"],
      metrics: [],
      patterns: [],
    });

    const qc = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    return render(
      <QueryClientProvider client={qc}>
        <ToastProvider>
          <MemoryRouter initialEntries={[initialEntry]}>
            <LocationProbe />
            <Routes>
              <Route path="/agent/services" element={<ServicesPage />} />
              <Route
                path="/agent/services/:name"
                element={<div>service detail</div>}
              />
            </Routes>
          </MemoryRouter>
        </ToastProvider>
      </QueryClientProvider>,
    );
  }

  it("shows an Active | Ignored scope toggle with count badges when the exclude surface is licensed", async () => {
    renderScoped();
    // Active holds the one non-excluded service; Ignored holds the one excluded
    // — the counts settle once the exclusion policy resolves.
    const active = await screen.findByRole("tab", { name: /Active/ });
    const ignored = screen.getByRole("tab", { name: /Ignored/ });
    await waitFor(() => expect(ignored.textContent).toContain("1"));
    expect(active.textContent).toContain("1");
    // Default scope is Active — the active service shows, the ignored one is out.
    expect(screen.getByText("payments")).toBeTruthy();
    expect(screen.queryByText("checkout")).toBeNull();
  });

  it("filters to the ignored service when the Ignored scope is selected", async () => {
    renderScoped();
    fireEvent.click(await screen.findByRole("tab", { name: /Ignored/ }));
    // The excluded service moves into the single table under the Ignored scope;
    // the active one leaves it.
    expect(await screen.findByText("checkout")).toBeTruthy();
    expect(screen.queryByText("payments")).toBeNull();
  });

  it("shows the empty-state when the Ignored scope has no services", async () => {
    vi.mocked(api.getLearnExclusions).mockResolvedValue({
      services: [],
      metrics: [],
      patterns: [],
    });
    renderScoped("/agent/services?scope=ignored");
    expect(await screen.findByText("No services are ignored")).toBeTruthy();
  });

  it("keeps the scope toggle absent on a community / unlicensed binary", async () => {
    // Community defaults from the module mock: baselines + deployment 403.
    vi.mocked(api.listServices).mockResolvedValue({ checkout: svc() });
    vi.mocked(api.listBaselines).mockRejectedValue(
      new (await import("@/lib/api")).ApiError(403, "community"),
    );
    vi.mocked(api.getSSODeployment).mockRejectedValue(
      new (await import("@/lib/api")).ApiError(403, "community"),
    );
    renderPage();
    await screen.findByLabelText("View service checkout");
    expect(screen.queryByRole("tablist", { name: "Learning scope" })).toBeNull();
  });
});

