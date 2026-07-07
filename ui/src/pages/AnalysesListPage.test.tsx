// @vitest-environment jsdom
import { describe, it, expect, afterEach, beforeEach, vi } from "vitest";
import { render, screen, cleanup, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route, useLocation } from "react-router-dom";
import { AnalysesListPage } from "./AnalysesListPage";
import { api, type AnalysisRecord } from "@/lib/api";

// The analyses table rows do NOT navigate — only the per-row eye opens a peek,
// whose footer button links to the analysis detail page.
vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      listAllAnalyses: vi.fn(),
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
    vi.mocked(api.listAllAnalyses).mockResolvedValue([rec()]);
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
