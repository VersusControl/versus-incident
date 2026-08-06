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
      // Analysis is enabled by default so the peek's Run analysis button is
      // active; runAnalysis is a spy the row-action test asserts against.
      getAgentConfig: vi.fn().mockResolvedValue({ ai: { enable: true } }),
      runAnalysis: vi.fn().mockResolvedValue({}),
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
        <MemoryRouter
          initialEntries={["/incidents"]}
          future={{ v7_startTransition: true, v7_relativeSplatPath: true }}
        >
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
        <MemoryRouter
          initialEntries={[entry]}
          future={{ v7_startTransition: true, v7_relativeSplatPath: true }}
        >
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

// The auto load-more effect must be BOUNDED by the server-authoritative match
// count for the active (origin, status). Before the fix, a status the loaded
// page can't satisfy (webhook + open, everything auto-resolved) walked the
// whole history one page at a time and rendered a blank list. The guard stops
// paging once the server says there are no (more) matching rows.
describe("IncidentsPage — bounded auto load-more", () => {
  // Call counts accumulate across tests (no global mock reset), so clear the
  // list-client spy before each case to count only this test's own fetches.
  beforeEach(() => {
    vi.mocked(api.listIncidentsIndex).mockReset();
  });

  it("does NOT page past the first server chunk when the active status has zero server matches (webhook + open)", async () => {
    // The server sees ZERO webhook+open rows (all auto-resolved); the loaded
    // first page holds only resolved rows, and there is MORE history to fetch
    // (next_offset present). The default status filter is `open`.
    const byStatus: IncidentStatusCounts = {
      open: oc(0, 0),
      acked: oc(0, 0),
      resolved: oc(0, 200),
      all: oc(0, 200),
    };
    vi.mocked(api.listIncidentsIndex).mockImplementation((opts) =>
      Promise.resolve({
        ...index(
          [
            incident({
              id: "wh-resolved-1",
              origin: "webhook",
              source: "webhook",
              resolved: true,
            }),
          ],
          byStatus,
        ),
        // More rows exist on the server — without the guard the effect would
        // keep fetching offset 100, 200, … chasing matches that never come.
        next_offset: opts?.offset ? null : 100,
      }),
    );

    renderPageAt("/incidents?origin=webhook");

    // The empty state renders after the FIRST page — no history walk.
    expect(await screen.findByText("No open incidents")).toBeTruthy();
    // The list client was called exactly once (offset 0). A settle window makes
    // a runaway second/third fetch observable if the guard regressed.
    await waitFor(() =>
      expect(api.listIncidentsIndex).toHaveBeenCalledTimes(1),
    );
    // Belt-and-suspenders: still one call after the microtask queue drains.
    await new Promise((r) => setTimeout(r, 30));
    expect(api.listIncidentsIndex).toHaveBeenCalledTimes(1);
  });

  it("STILL loads later chunks when the active status has matches deeper in the history", async () => {
    // The server reports 2 webhook+open rows, but they live on the SECOND
    // chunk; the first chunk holds only a resolved row. The guard uses `<`, so
    // it keeps fetching until filtered.length reaches the known match count.
    const byStatus: IncidentStatusCounts = {
      open: oc(0, 2),
      acked: oc(0, 0),
      resolved: oc(0, 1),
      all: oc(0, 3),
    };
    vi.mocked(api.listIncidentsIndex).mockImplementation((opts) => {
      if (!opts?.offset) {
        return Promise.resolve({
          ...index(
            [
              incident({
                id: "wh-resolved-1",
                origin: "webhook",
                source: "webhook",
                resolved: true,
              }),
            ],
            byStatus,
          ),
          next_offset: 100,
        });
      }
      return Promise.resolve({
        ...index(
          [
            incident({
              id: "wh-open-1",
              title: "Deep open A",
              origin: "webhook",
              source: "webhook",
              resolved: false,
            }),
            incident({
              id: "wh-open-2",
              title: "Deep open B",
              origin: "webhook",
              source: "webhook",
              resolved: false,
            }),
          ],
          byStatus,
        ),
        next_offset: null,
      });
    });

    renderPageAt("/incidents?origin=webhook");

    // The deep open rows are found and rendered…
    expect(await screen.findByText("Deep open A")).toBeTruthy();
    expect(screen.getByText("Deep open B")).toBeTruthy();
    // …which required exactly TWO chunks (offset 0 then 100) — and no more,
    // because filtered.length (2) then equals the known match count (2).
    await waitFor(() =>
      expect(api.listIncidentsIndex).toHaveBeenCalledTimes(2),
    );
    await new Promise((r) => setTimeout(r, 30));
    expect(api.listIncidentsIndex).toHaveBeenCalledTimes(2);
  });
});

// The incidents LIST exposes the same Run analysis action as the detail page,
// via the per-row peek slide-out (where Assign / Resolve also live), so an
// operator can trigger analysis without leaving the list.
describe("IncidentsPage — per-row Run analysis", () => {
  beforeEach(() => {
    vi.mocked(api.runAnalysis).mockClear();
    vi.mocked(api.listIncidentsIndex).mockResolvedValue(
      index([incident({ id: "row-analyze-1" })]),
    );
  });

  it("triggers api.runAnalysis with the row id from the peek", async () => {
    renderPage();

    // Open the row's action surface (the peek) — the same place Assign /
    // Resolve are exposed per row.
    const eye = await screen.findByLabelText(/View incident/);
    fireEvent.click(eye);
    const panel = screen.getByRole("dialog", { name: "Details panel" });

    const runBtn = await within(panel).findByRole("button", {
      name: "Run AI analysis",
    });
    fireEvent.click(runBtn);

    await waitFor(() =>
      expect(api.runAnalysis).toHaveBeenCalledWith("row-analyze-1"),
    );
  });
});

