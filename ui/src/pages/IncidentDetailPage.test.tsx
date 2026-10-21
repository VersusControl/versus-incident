// @vitest-environment jsdom
import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, within } from "@testing-library/react";
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
  it("surfaces Kubernetes facts, evidence, changes and safe deep links without a log-pattern link", async () => {
    vi.mocked(api.getIncident).mockResolvedValue(detail({
      source: "kubernetes:east",
      content: {
        AlertKind: "kubernetes_issue", PatternID: "kubernetes:pod.crash_loop",
        Cluster: "east", Namespace: "shop", Workload: "checkout", Affected: "2 of 3 pods",
        Evidence: ["Container api exited with code 137"], Changes: ["Image changed to v42"],
        HistoryIncomplete: true,
        TimelineURL: "/agent/kubernetes?view=timeline&root_uid=checkout",
        AgentURL: "https://console.example.com/agent/chat?issue=pod.crash_loop",
      },
    }));
    renderDetail();
    const evidence = await screen.findByRole("region", { name: "Evidence" });
    expect(within(evidence).getByText("Container api exited with code 137")).toBeTruthy();
    expect(within(screen.getByRole("region", { name: "Changes" })).getByText("Image changed to v42")).toBeTruthy();
    for (const text of ["east", "shop", "2 of 3 pods", "Change history is incomplete.", "kubernetes:pod.crash_loop"]) {
      expect(screen.getByText(text)).toBeTruthy();
    }
    expect(screen.queryByText("No summary provided.")).toBeNull();
    expect(screen.getByRole("link", { name: "Timeline" }).getAttribute("href")).toBe("/agent/kubernetes?view=timeline&root_uid=checkout");
    expect(screen.getByRole("link", { name: "Agent" }).getAttribute("rel")).toBe("noopener noreferrer");
    expect(screen.queryAllByRole("link").some((link) => link.getAttribute("href")?.startsWith("/agent/logs/"))).toBe(false);
  });

  it.each(["javascript:alert(1)", "data:text/html,bad", "//evil.example/agent", "/\\evil.example/agent", "https://user:password@example.com/agent"])("does not navigate unsafe issue links: %s", async (url) => {
    vi.mocked(api.getIncident).mockResolvedValue(detail({
      content: { AlertKind: "kubernetes_issue", TimelineURL: url, AgentURL: url },
    }));
    renderDetail();
    await screen.findByRole("region", { name: "Evidence" });
    expect(screen.queryByRole("link", { name: "Timeline" })).toBeNull();
    expect(screen.queryByRole("link", { name: "Agent" })).toBeNull();
  });

  it("preserves real log-agent pattern navigation", async () => {
    vi.mocked(api.getIncident).mockResolvedValue(detail({
      content: { PatternID: "real-log-pattern-123" },
    }));
    renderDetail();
    expect((await screen.findByRole("link", { name: "real-log-pat" })).getAttribute("href")).toBe("/agent/logs/real-log-pattern-123");
  });

  it("shows partial errors in Timeline and Notified while successful channels are sent", async () => {
    vi.mocked(api.getIncident).mockResolvedValue(detail({
      notify_status: "partial", channels_notified: ["email", "lark"],
      notify_error: "msteams: MS Teams API returned 500 status code",
    }));
    renderDetail();
    expect((await screen.findAllByText("msteams: MS Teams API returned 500 status code")).length).toBe(2);
    const notified = screen.getByText("Notified", { selector: ".card-title" }).closest<HTMLElement>(".card")!;
    expect(within(notified).getAllByText("sent").length).toBe(2);
    expect(within(notified).queryByText("partial")).toBeNull();
    expect(screen.getByText("Partial delivery")).toBeTruthy();
  });

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

  it("shows cumulative episode count and last seen", async () => {
    vi.mocked(api.getIncident).mockResolvedValue(detail({
      detection_episode_id: "episode-1",
      occurrence_count: 500,
      detection_last_seen: new Date().toISOString(),
    }));
    renderDetail();
    expect(await screen.findByText("500")).toBeTruthy();
    expect(screen.getByText("Last seen")).toBeTruthy();
  });

  it("prefers cumulative typed severity over stale content", async () => {
    vi.mocked(api.getIncident).mockResolvedValue(detail({
      highest_observed_severity: "critical",
      highest_notified_severity: "critical",
      content: { Severity: "low" },
    }));
    renderDetail();
    expect(await screen.findByText("CRITICAL")).toBeTruthy();
  });
});
