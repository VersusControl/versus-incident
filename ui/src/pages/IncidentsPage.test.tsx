// @vitest-environment jsdom
import { describe, it, expect, afterEach, beforeEach, vi } from "vitest";
import {
  render,
  screen,
  cleanup,
  fireEvent,
  waitFor,
  within,
} from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route, useLocation } from "react-router-dom";
import { ToastProvider } from "@/components/Toast";
import { IncidentsPage } from "./IncidentsPage";
import { api, type IncidentIndex, type IncidentSummary } from "@/lib/api";

// The Incidents table row exposes ONLY the eye (Assign / Resolve moved to the
// bulk-action bar), and the row itself is no longer a navigation control —
// clicking a row must NOT navigate; only the eye opens the peek. These pin both.
vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      listIncidentsIndex: vi.fn(),
      searchIncidentsIndex: vi.fn(),
      capabilities: vi.fn().mockResolvedValue({ search: false }),
      listTeams: vi.fn().mockResolvedValue([]),
      listMembers: vi.fn().mockResolvedValue([]),
      getIntakeSettings: vi.fn(),
      updateIntakeSettings: vi.fn(),
    },
  };
});

afterEach(cleanup);

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

function index(rows: IncidentSummary[]): IncidentIndex {
  return {
    incidents: rows,
    counts: { ai_detect: rows.length, webhook: 0, total: rows.length },
    total: rows.length,
  };
}

// LocationProbe surfaces the current path so a click can be asserted to NOT
// navigate.
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
        <MemoryRouter initialEntries={["/incidents"]}>
          <LocationProbe />
          <Routes>
            <Route path="/incidents" element={<IncidentsPage />} />
            <Route
              path="/incidents/:id"
              element={<div>incident detail</div>}
            />
          </Routes>
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

// renderPageAt renders the page at a specific URL so the origin tab under test
// (?origin=webhook vs the default ai_detect) is active from first paint.
function renderPageAt(entry: string) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <ToastProvider>
        <MemoryRouter initialEntries={[entry]}>
          <LocationProbe />
          <Routes>
            <Route path="/incidents" element={<IncidentsPage />} />
            <Route
              path="/incidents/:id"
              element={<div>incident detail</div>}
            />
          </Routes>
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

describe("IncidentsPage row actions", () => {
  beforeEach(() => {
    vi.mocked(api.listIncidentsIndex).mockResolvedValue(index([incident()]));
  });

  it("shows only the eye action per row — no Assign / Resolve buttons", async () => {
    renderPage();
    // The single row's eye is present…
    expect(await screen.findByLabelText(/View incident/)).toBeTruthy();
    // …and the per-row Assign / Resolve icon buttons are gone (they live in the
    // bulk-action bar now, which only appears on selection).
    expect(
      screen.queryByRole("button", { name: "Assign team or member" }),
    ).toBeNull();
    expect(
      screen.queryByRole("button", { name: "Mark incident resolved" }),
    ).toBeNull();
  });

  it("does not navigate when the row is clicked (only the eye acts)", async () => {
    renderPage();
    const eye = await screen.findByLabelText(/View incident/);
    const row = eye.closest("tr") as HTMLTableRowElement;
    // A plain row click is inert — the row carries no navigation affordance.
    fireEvent.click(row);
    expect(screen.getByTestId("path").textContent).toBe("/incidents");
    // The eye opens the in-place peek, still without a route change.
    fireEvent.click(eye);
    const panel = screen.getByRole("dialog", { name: "Details panel" });
    expect(within(panel).getByText("checkout")).toBeTruthy();
    expect(screen.getByTestId("path").textContent).toBe("/incidents");
  });
});

// The webhook auto-resolve toggle lives in the Incidents toolbar and is scoped
// to the webhook origin tab — it is the toggle's meaning ("Auto-resolve"), not
// an "Incident intake" settings card. It must be absent on the AI-detected tab.
describe("IncidentsPage — webhook auto-resolve toggle", () => {
  beforeEach(() => {
    vi.mocked(api.listIncidentsIndex).mockResolvedValue(index([incident()]));
    vi.mocked(api.getIntakeSettings).mockResolvedValue({
      auto_resolve_webhook: true,
    });
    vi.mocked(api.updateIntakeSettings).mockImplementation((s) =>
      Promise.resolve(s),
    );
  });

  it("is absent on the AI-detected (default) tab", async () => {
    renderPage();
    // Wait for the page to settle so a late mount can't be mistaken for absence.
    await screen.findByLabelText(/View incident/);
    expect(screen.queryByTestId("intake-auto-resolve")).toBeNull();
    expect(api.getIntakeSettings).not.toHaveBeenCalled();
  });

  it("renders on the webhook tab, defaults ON, and PUTs on toggle", async () => {
    renderPageAt("/incidents?origin=webhook");

    const toggle = (await screen.findByTestId(
      "intake-auto-resolve",
    )) as HTMLInputElement;
    // Default ON — mirrors the backend default.
    await waitFor(() => expect(toggle.checked).toBe(true));
    // Short label only — no "Incident intake" wording.
    expect(screen.getByText("Auto-resolve")).toBeTruthy();

    fireEvent.click(toggle);
    await waitFor(() =>
      expect(api.updateIntakeSettings).toHaveBeenCalledWith({
        auto_resolve_webhook: false,
      }),
    );
  });
});
