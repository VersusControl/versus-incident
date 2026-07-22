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
import {
  api,
  type IncidentIndex,
  type IncidentStatusCounts,
  type IncidentSummary,
  type OriginCounts,
} from "@/lib/api";

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

function oc(ai: number, webhook: number): OriginCounts {
  return { ai_detect: ai, webhook, total: ai + webhook };
}

// index builds a list response. by_status is the server's authoritative
// per-origin × per-status breakdown; when omitted it is derived treating every
// loaded row as an open ai_detect incident (the common single-row fixture).
function index(
  rows: IncidentSummary[],
  by_status?: IncidentStatusCounts,
): IncidentIndex {
  const bs =
    by_status ?? {
      open: oc(rows.length, 0),
      acked: oc(0, 0),
      resolved: oc(0, 0),
      all: oc(rows.length, 0),
    };
  return {
    incidents: rows,
    counts: {
      ai_detect: bs.open.ai_detect + bs.acked.ai_detect,
      webhook: bs.open.webhook + bs.acked.webhook,
      total: bs.open.total + bs.acked.total,
      by_status: bs,
    },
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

// The status- and origin-tab counts must be the SERVER's authoritative
// per-origin × per-status totals — never a tally of the bounded loaded page.
// This is the fix for the "three surfaces, three numbers" bug: with a webhook
// history that auto-resolves, the loaded page holds a single OPEN row yet the
// server sees 277 resolved, so the Resolved tab must read 277 (server), not 0
// (loaded page), and origin All must read the whole-set 278.
describe("IncidentsPage — tab counts come from server by_status", () => {
  it("shows server per-status totals, not the loaded page", async () => {
    const loaded = incident({
      id: "wh-open-1",
      origin: "webhook",
      source: "webhook",
      resolved: false,
    });
    const byStatus: IncidentStatusCounts = {
      open: oc(0, 2),
      acked: oc(0, 5),
      resolved: oc(0, 277),
      all: oc(0, 284),
    };
    vi.mocked(api.listIncidentsIndex).mockResolvedValue(
      index([loaded], byStatus),
    );

    renderPageAt("/incidents?origin=webhook&status=resolved");

    // The Resolved status tab shows the server's 277 — the loaded page has zero
    // resolved rows, so a client tally would have shown 0.
    expect(await screen.findByText("277")).toBeTruthy();
    // The Acked tab shows the server's 5 (also absent from the loaded page).
    expect(screen.getByText("5")).toBeTruthy();
    // The webhook feed total (284) reconciles across the origin tab and the
    // "All" status tab — the SAME server number in both places.
    expect(screen.getAllByText("284").length).toBeGreaterThanOrEqual(2);
  });
});
