// @vitest-environment jsdom
import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router-dom";
import { ToastProvider } from "@/components/Toast";
import { IncidentDetailPage } from "./IncidentDetailPage";
import { api, type IncidentDetail } from "@/lib/api";

// The Run analysis action was extracted into a shared component
// (@/components/RunAnalysisButton) and reused by both this page and the
// incidents list. This pins that the detail page still renders it after the
// extraction — same "Run AI analysis" affordance the list now shares.
vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      getIncident: vi.fn(),
      listAnalyses: vi.fn().mockResolvedValue([]),
      listTeams: vi.fn().mockResolvedValue([]),
      listMembers: vi.fn().mockResolvedValue([]),
      getAgentConfig: vi.fn().mockResolvedValue({ ai: { enable: true } }),
    },
  };
});

afterEach(cleanup);

function detail(overrides: Partial<IncidentDetail> = {}): IncidentDetail {
  return {
    id: "detail-1234567890",
    title: "Checkout latency spike",
    source: "ai_detect",
    origin: "ai_detect",
    service: "checkout",
    resolved: false,
    created_at: new Date().toISOString(),
    content: {},
    ...overrides,
  } as IncidentDetail;
}

function renderDetail() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <ToastProvider>
        <MemoryRouter
          initialEntries={["/incidents/detail-1234567890"]}
          future={{ v7_startTransition: true, v7_relativeSplatPath: true }}
        >
          <Routes>
            <Route
              path="/incidents/:id"
              element={<IncidentDetailPage />}
            />
          </Routes>
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

describe("IncidentDetailPage — shared Run analysis button", () => {
  it("still renders the Run analysis action after the extraction", async () => {
    vi.mocked(api.getIncident).mockResolvedValue(detail());
    renderDetail();

    // The shared RunAnalysisButton renders its accessible label; the detail
    // page mounts it in the sticky header (and mobile footer), so at least one
    // is present once the incident loads.
    const buttons = await screen.findAllByRole("button", {
      name: "Run AI analysis",
    });
    expect(buttons.length).toBeGreaterThanOrEqual(1);
  });
});
