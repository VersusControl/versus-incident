// @vitest-environment jsdom
import { describe, it, expect, afterEach, beforeEach, vi } from "vitest";
import { render, screen, cleanup, fireEvent, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route, useLocation } from "react-router-dom";
import { ToastProvider } from "@/components/Toast";
import { ServicesPage } from "./ServicesPage";
import { api, ApiError, getSsoSession, type ServiceInfo } from "@/lib/api";

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
      listServicesIndex: vi.fn(),
      getServiceDetail: vi.fn(),
      getServiceIntel: vi.fn(),
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
      setServiceLearnExclusions: vi
        .fn()
        .mockResolvedValue({ services: [], metrics: [], patterns: [] }),
      setLearnExclusions: vi
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
        <MemoryRouter
          initialEntries={["/agent/services"]}
          future={{ v7_startTransition: true, v7_relativeSplatPath: true }}
        >
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
    vi.mocked(api.listServicesIndex).mockResolvedValue({
      services: { checkout: svc() },
      total: 1,
      next_offset: null,
    });
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
    vi.mocked(api.getServiceIntel).mockRejectedValue(
      new ApiError(403, "community"),
    );
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

  it("shows metric and trace counts from the shared intel probe", async () => {
    vi.mocked(api.getServiceIntel).mockResolvedValue({
      service: "checkout",
      metrics: [{ signal: "requests" } as never],
      traces: [{ signal: "GET /checkout" } as never, { signal: "POST /checkout" } as never],
    });
    renderPage();
    fireEvent.click(await screen.findByLabelText("View service checkout"));
    const peek = within(screen.getByRole("dialog"));
    expect((await peek.findByText("Metrics")).parentElement?.textContent).toContain("1");
    expect(peek.getByText("Traces").parentElement?.textContent).toContain("2");
  });

  it.each([403, 404])("omits intel fields when the endpoint returns %s", async (status) => {
    vi.mocked(api.getServiceIntel).mockRejectedValue(new ApiError(status, "locked"));
    renderPage();
    fireEvent.click(await screen.findByLabelText("View service checkout"));
    await waitFor(() => expect(api.getServiceIntel).toHaveBeenCalled());
    const peek = within(screen.getByRole("dialog"));
    expect(peek.queryByText("Metrics")).toBeNull();
    expect(peek.queryByText("Traces")).toBeNull();
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
    vi.mocked(api.listServicesIndex).mockResolvedValue({
      services: {
        checkout: svc(),
        payments: svc(),
      },
      total: 2,
      next_offset: null,
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
          <MemoryRouter
            initialEntries={[initialEntry]}
            future={{ v7_startTransition: true, v7_relativeSplatPath: true }}
          >
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
    vi.mocked(api.listServicesIndex).mockResolvedValue({
      services: { checkout: svc() },
      total: 1,
      next_offset: null,
    });
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

// A bulk Ignore/Resume over N selected services must send INTENT — ONE call to
// the batch route carrying only the names + the direction. Sending the WHOLE
// resulting policy instead let a stale page revert a concurrent change, and
// because that body also carries the metric + log-pattern grains, a services
// bulk action could wipe a colleague's metric/log-pattern exclusions.
describe("ServicesPage bulk Ignore/Resume learning", () => {
  // Restore the module mock's community defaults so the licensed-admin surface
  // this block opts into doesn't leak into later describes.
  afterEach(async () => {
    const { ApiError } = await import("@/lib/api");
    vi.mocked(api.listBaselines).mockRejectedValue(
      new ApiError(403, "community"),
    );
    vi.mocked(api.getSSODeployment).mockRejectedValue(
      new ApiError(403, "community"),
    );
    vi.mocked(getSsoSession).mockRejectedValue(new ApiError(401, "no session"));
    vi.mocked(api.getLearnExclusions).mockResolvedValue({
      services: [],
      metrics: [],
      patterns: [],
    });
  });

  function renderBulk(
    excluded: string[],
    otherGrains: { metrics: string[]; patterns: string[] } = {
      metrics: [],
      patterns: [],
    },
  ) {
    vi.mocked(api.listServicesIndex).mockResolvedValue({
      services: { checkout: svc(), payments: svc(), search: svc() },
      total: 3,
      next_offset: null,
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
    vi.mocked(api.getLearnExclusions).mockResolvedValue({
      services: excluded,
      ...otherGrains,
    });
    vi.mocked(api.setLearnExclusions).mockClear();
    vi.mocked(api.setServiceLearnExclusions).mockClear();
    vi.mocked(api.setServiceLearnExclusions).mockResolvedValue({
      services: excluded,
      ...otherGrains,
    });
    vi.mocked(api.setServiceLearnExclusion).mockClear();
    return renderPage();
  }

  async function selectAllThree(scopeTab: RegExp) {
    fireEvent.click(await screen.findByRole("tab", { name: scopeTab }));
    for (const name of ["checkout", "payments", "search"]) {
      fireEvent.click(await screen.findByLabelText(`Select service ${name}`));
    }
  }

  it("ignores every selected service in exactly ONE batch call", async () => {
    renderBulk([]);
    await selectAllThree(/Active/);
    fireEvent.click(
      await screen.findByRole("button", { name: "Ignore learning" }),
    );
    await waitFor(() =>
      expect(vi.mocked(api.setServiceLearnExclusions)).toHaveBeenCalledTimes(1),
    );
    expect(vi.mocked(api.setServiceLearnExclusions).mock.calls[0]).toEqual([
      ["checkout", "payments", "search"],
      true,
    ]);
    // The whole-list PUT and the racing per-service route are both out of the
    // bulk path — the client never sends a resulting policy for this action.
    expect(vi.mocked(api.setLearnExclusions)).not.toHaveBeenCalled();
    expect(vi.mocked(api.setServiceLearnExclusion)).not.toHaveBeenCalled();
  });

  it("resumes every selected service in exactly ONE batch call", async () => {
    renderBulk(["checkout", "payments", "search"]);
    await selectAllThree(/Ignored/);
    fireEvent.click(
      await screen.findByRole("button", { name: "Resume learning" }),
    );
    await waitFor(() =>
      expect(vi.mocked(api.setServiceLearnExclusions)).toHaveBeenCalledTimes(1),
    );
    expect(vi.mocked(api.setServiceLearnExclusions).mock.calls[0]).toEqual([
      ["checkout", "payments", "search"],
      false,
    ]);
    expect(vi.mocked(api.setLearnExclusions)).not.toHaveBeenCalled();
    expect(vi.mocked(api.setServiceLearnExclusion)).not.toHaveBeenCalled();
  });

  // The metric + log-pattern grains are simply not in the request — the client
  // sends the service names and the direction, nothing else. Carrying them (as
  // the old whole-list PUT did) is how a services bulk action could revert a
  // colleague's metric / log-pattern exclusions.
  it("sends ONLY the service names and the exclude flag — no other grain", async () => {
    renderBulk([], {
      metrics: ["go_*", "up"],
      patterns: ["log:checkout:abc123"],
    });
    await selectAllThree(/Active/);
    fireEvent.click(
      await screen.findByRole("button", { name: "Ignore learning" }),
    );
    await waitFor(() =>
      expect(vi.mocked(api.setServiceLearnExclusions)).toHaveBeenCalledTimes(1),
    );
    const [services, exclude] = vi.mocked(api.setServiceLearnExclusions).mock
      .calls[0];
    expect(services).toEqual(["checkout", "payments", "search"]);
    expect(exclude).toBe(true);
    expect(vi.mocked(api.setServiceLearnExclusions).mock.calls[0]).toHaveLength(
      2,
    );
    expect(vi.mocked(api.setLearnExclusions)).not.toHaveBeenCalled();
  });

  // The policy the write returns is adopted into the shared cache, so the scope
  // counts and the partitioned table follow a bulk action with no reload.
  it("moves every bulk-ignored service into the Ignored scope with no reload", async () => {
    renderBulk([]);
    await selectAllThree(/Active/);
    // From the click on, both the write's answer and the post-write refetch
    // report the server's new policy.
    const after = {
      services: ["checkout", "payments", "search"],
      metrics: [],
      patterns: [],
    };
    vi.mocked(api.setServiceLearnExclusions).mockResolvedValue(after);
    vi.mocked(api.getLearnExclusions).mockResolvedValue(after);
    fireEvent.click(
      await screen.findByRole("button", { name: "Ignore learning" }),
    );

    await waitFor(() =>
      expect(
        screen.getByRole("tab", { name: /Ignored/ }).textContent,
      ).toContain("3"),
    );
    expect(screen.getByRole("tab", { name: /Active/ }).textContent).toContain(
      "0",
    );
    expect(screen.queryByText("payments")).toBeNull();

    fireEvent.click(screen.getByRole("tab", { name: /Ignored/ }));
    for (const name of ["checkout", "payments", "search"]) {
      expect(await screen.findByText(name)).toBeTruthy();
    }
  });
});

// The Services list is served as bounded pages (a cheap total + one page),
// keeping the back-compat name→facts map shape. These pin the paged wiring:
// the whole-set total drives the header, and a server-advertised next page
// surfaces a "Load more" control that merges the following page in.
describe("ServicesPage — server-side paging", () => {
  beforeEach(() => {
    vi.mocked(api.getServiceDetail).mockResolvedValue({
      service: "checkout",
      first_seen: new Date().toISOString(),
      in_grace: false,
      grace_seconds_remaining: 0,
      patterns: [],
      incidents: { window_days: 30, count: 0, severities: {}, recent: [] },
      counts: { patterns: 0, incidents: 0 },
    });
  });

  it("renders the whole-set total in the header, not the loaded page size", async () => {
    vi.mocked(api.listServicesIndex).mockResolvedValue({
      services: { checkout: svc() },
      total: 3100,
      next_offset: null,
    });
    renderPage();
    expect(await screen.findByText(/3,100 discovered/)).toBeTruthy();
  });

  it("loads and merges the next page when Load more is clicked", async () => {
    vi.mocked(api.listServicesIndex)
      .mockResolvedValueOnce({
        services: { checkout: svc() },
        total: 2,
        next_offset: 1,
      })
      .mockResolvedValueOnce({
        services: { payments: svc() },
        total: 2,
        next_offset: null,
      });
    renderPage();

    expect(await screen.findByText("checkout")).toBeTruthy();
    const more = await screen.findByTestId("service-load-more");
    fireEvent.click(more.querySelector("button")!);

    // The second page is fetched and merged into the same table.
    expect(await screen.findByText("payments")).toBeTruthy();
    expect(screen.getByText("checkout")).toBeTruthy();
    expect(api.listServicesIndex).toHaveBeenCalledWith({ offset: 1 });
  });
});

