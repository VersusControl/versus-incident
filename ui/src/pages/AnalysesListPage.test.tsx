// @vitest-environment jsdom
import { describe, it, expect, afterEach, beforeEach, vi } from "vitest";
import { render, screen, cleanup, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route, useLocation } from "react-router-dom";
import { AnalysesListPage } from "./AnalysesListPage";
import { api, type AnalysisRecord, type AnalysisIndex } from "@/lib/api";

// The analyses table rows do NOT navigate — only the per-row eye opens a peek,
// whose footer button links to the analysis detail page.
vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      listAllAnalysesIndex: vi.fn(),
      listIncidents: vi.fn(),
    },
  };
});

afterEach(cleanup);

function rec(overrides: Partial<AnalysisRecord> = {}): AnalysisRecord {
  return {
    id: "a1",
    incident_id: "inc1",
    requested_at: new Date().toISOString(),
    status: "ok",
    finding: { Title: "Root cause found" },
    ...overrides,
  };
}

// idx builds a single-page AnalysisIndex response (server-shaped), used to
// stub the paged /analyses endpoint the page now calls.
function idx(
  analyses: AnalysisRecord[],
  overrides: Partial<AnalysisIndex> = {},
): AnalysisIndex {
  return {
    analyses,
    total: analyses.length,
    offset: 0,
    next_offset: null,
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
      <MemoryRouter initialEntries={["/analyses"]}>
        <LocationProbe />
        <Routes>
          <Route path="/analyses" element={<AnalysesListPage />} />
          <Route
            path="/incidents/:incidentId/analyses/:analysisId"
            element={<div>analysis detail</div>}
          />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("AnalysesListPage row actions", () => {
  beforeEach(() => {
    vi.mocked(api.listAllAnalysesIndex).mockResolvedValue(idx([rec()]));
    vi.mocked(api.listIncidents).mockResolvedValue([]);
  });

  it("does not navigate on a plain row click", async () => {
    renderPage();
    const eye = await screen.findByLabelText("View analysis a1");
    const row = eye.closest("tr") as HTMLTableRowElement;
    fireEvent.click(row);
    expect(screen.getByTestId("path").textContent).toBe("/analyses");
  });

  it("opens a peek from the eye without navigating", async () => {
    renderPage();
    fireEvent.click(await screen.findByLabelText("View analysis a1"));
    expect(screen.getByTestId("path").textContent).toBe("/analyses");
    expect(
      screen.getByRole("link", { name: /Open full page/ }),
    ).toBeTruthy();
  });
});

describe("AnalysesListPage server pagination", () => {
  beforeEach(() => {
    vi.mocked(api.listIncidents).mockResolvedValue([]);
  });

  it("shows the whole-set total, not just the loaded page, and loads more", async () => {
    // First page: 2 of 5 rows, resume cursor at offset 2. The second page
    // carries the remaining 3 and closes the cursor (next_offset null).
    // Drive the stub off the requested offset so it is robust to how many
    // times react-query invokes the query function.
    vi.mocked(api.listAllAnalysesIndex).mockImplementation((opts) => {
      if ((opts?.offset ?? 0) >= 2) {
        return Promise.resolve(
          idx([rec({ id: "a3" }), rec({ id: "a4" }), rec({ id: "a5" })], {
            total: 5,
            offset: 2,
            next_offset: null,
            page_size: 2,
          }),
        );
      }
      return Promise.resolve(
        idx([rec({ id: "a1" }), rec({ id: "a2" })], {
          total: 5,
          offset: 0,
          next_offset: 2,
          page_size: 2,
        }),
      );
    });

    renderPage();

    // The subtitle reflects the true total (5), not the two rows loaded.
    expect(await screen.findByText("5 stored")).toBeTruthy();
    // Only the first bounded page is on screen up front.
    expect(await screen.findByLabelText("View analysis a1")).toBeTruthy();
    expect(screen.queryByLabelText("View analysis a3")).toBeNull();

    // The load-more control names the total; clicking it pulls the next page.
    const more = await screen.findByTestId("analysis-load-more");
    const btn = more.querySelector("button") as HTMLButtonElement;
    expect(btn.textContent).toContain("5");
    fireEvent.click(btn);

    expect(await screen.findByLabelText("View analysis a5")).toBeTruthy();
    // The second page was requested from the server's resume cursor (offset 2),
    // proving the client asked for a bounded next chunk rather than everything.
    const calls = vi.mocked(api.listAllAnalysesIndex).mock.calls;
    expect(calls[0][0]).toEqual({ offset: 0 });
    expect(calls.some((c) => c[0]?.offset === 2)).toBe(true);
  });
});
