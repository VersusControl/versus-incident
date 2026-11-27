// @vitest-environment jsdom
import { describe, it, expect, afterEach, beforeEach, vi } from "vitest";
import { act, render, screen, cleanup, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router-dom";
import { format } from "date-fns";
import { ToastProvider } from "@/components/Toast";
import { NowPage } from "./NowPage";
import {
  api,
  ApiError,
  type IncidentCounts,
  type IncidentStatusCounts,
  type IncidentSummary,
  type OriginCounts,
} from "@/lib/api";

vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      listIncidents: vi.fn(),
      incidentCounts: vi.fn(),
      getAgentConfig: vi.fn().mockResolvedValue({ enable: false }),
      status: vi.fn().mockResolvedValue({ patterns: 0 }),
      listBaselines: vi.fn().mockRejectedValue(new actual.ApiError(404, "community")),
    },
  };
});

afterEach(cleanup);

function oc(ai: number, webhook: number): OriginCounts {
  return { ai_detect: ai, webhook, total: ai + webhook };
}

// open and all deliberately DIFFER on both origins — a badge reading the
// all-status bucket would show 40/900 instead of 1/2.
const byStatus: IncidentStatusCounts = {
  open: oc(1, 2),
  acked: oc(4, 8),
  resolved: oc(35, 890),
  all: oc(40, 900),
};

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

function renderPageAt(entry = "/now") {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return {
    queryClient: qc,
    ...render(
      <QueryClientProvider client={qc}>
        <ToastProvider>
          <MemoryRouter
            initialEntries={[entry]}
            future={{ v7_startTransition: true, v7_relativeSplatPath: true }}
          >
            <Routes>
              <Route path="/now" element={<NowPage />} />
            </Routes>
          </MemoryRouter>
        </ToastProvider>
      </QueryClientProvider>,
    ),
  };
}

function setup(rows: IncidentSummary[] = []) {
  vi.mocked(api.listIncidents).mockResolvedValue(rows);
  vi.mocked(api.incidentCounts).mockResolvedValue({
    count_window: "30d",
    ...oc(byStatus.open.ai_detect + byStatus.acked.ai_detect,
      byStatus.open.webhook + byStatus.acked.webhook),
    by_status: byStatus,
  });
}

describe("NowPage origin badges", () => {
  it("badges both origin tabs with the OPEN count, not the all-status total", async () => {
    setup([
      incident({ id: "ai-open", origin: "ai_detect" }),
      incident({ id: "wh-open", origin: "webhook" }),
    ]);
    renderPageAt();

    // The badge is part of each tab's accessible name once counts land.
    const ai = await screen.findByRole("tab", { name: /^AI Detected 1$/ });
    const webhook = screen.getByRole("tab", { name: /^Webhook \/ Alerts 2$/ });

    // The whole-set numbers must appear on NEITHER tab — one open badge next
    // to one lifetime badge would be worse than the original bug.
    expect(within(ai).queryByText("40")).toBeNull();
    expect(within(webhook).queryByText("900")).toBeNull();
  });

  it("keeps the KPI tiles and the open banner on the same open numbers", async () => {
    setup([incident({ id: "ai-open", origin: "ai_detect" })]);
    renderPageAt();

    // Banner: the server open count for the active (ai_detect) tab.
    const banner = await screen.findByRole("region", {
      name: "Open incidents",
    });
    expect(within(banner).getByText("1 open incident")).toBeTruthy();

    // KPI tiles still read their own status buckets for the active origin.
    expect(screen.getByText("4")).toBeTruthy();
    expect(screen.getByText("35")).toBeTruthy();
    expect(screen.getByText(/counts over last 30d/)).toBeTruthy();
  });
});

describe("NowPage notify cell", () => {
  async function notifyRow(title: string): Promise<HTMLElement> {
    const feed = await screen.findByRole("region", { name: /^Incident feed/ });
    const cell = await within(feed).findByText(title);
    return cell.closest<HTMLElement>("tr")!;
  }

  it("says nothing was sent when no channel was enabled", async () => {
    setup([incident({ id: "none-row", title: "No channels row", notify_status: "none" })]);
    renderPageAt();
    const row = await notifyRow("No channels row");
    expect(within(row).getByText("nothing sent")).toBeTruthy();
  });

  it("does not claim a delivery when sent names no channel", async () => {
    setup([incident({ id: "sent-row", title: "Sent nowhere row", notify_status: "sent" })]);
    renderPageAt();
    const row = await notifyRow("Sent nowhere row");
    expect(within(row).getByText("nothing sent")).toBeTruthy();
    expect(within(row).queryByText("sent", { exact: true })).toBeNull();
  });

  it("counts the channels that received a sent alert", async () => {
    setup([incident({
      id: "slack-row",
      title: "Delivered row",
      notify_status: "sent",
      channels_notified: ["slack", "email"],
    })]);
    renderPageAt();
    const row = await notifyRow("Delivered row");
    expect(within(row).getByText("2")).toBeTruthy();
    expect(within(row).queryByText("nothing sent")).toBeNull();
  });
});

describe("NowPage feed severity", () => {
  it("shows the server-reported severity the Incidents list shows", async () => {
    setup([
      incident({ id: "crit-row", title: "Critical row", severity: "critical" }),
      incident({ id: "warn-row", title: "Warning row", severity: "warning" }),
    ]);
    renderPageAt();
    const feed = await screen.findByRole("region", { name: /^Incident feed/ });
    const crit = (await within(feed).findByText("Critical row")).closest<HTMLElement>("tr")!;
    const warn = within(feed).getByText("Warning row").closest<HTMLElement>("tr")!;
    expect(within(crit).getByText("CRITICAL")).toBeTruthy();
    expect(within(warn).getByText("WARN")).toBeTruthy();
  });
});

describe("NowPage Agent pulse", () => {
  beforeEach(() => {
    setup();
    vi.mocked(api.getAgentConfig).mockResolvedValue({ enable: true } as Awaited<
      ReturnType<typeof api.getAgentConfig>
    >);
    vi.mocked(api.listBaselines).mockReset().mockRejectedValue(new ApiError(404, "community"));
    vi.mocked(api.status).mockResolvedValue({
      patterns: 12,
      shadow_events: 3,
      detect_events: 4,
    } as Awaited<ReturnType<typeof api.status>>);
  });

  it("shows compact current signal activity without runtime or model details", async () => {
    vi.mocked(api.getAgentConfig).mockResolvedValue({
      enable: true, mode: "detect", ai: { enable: true, provider: "openai", model: "o4-mini" },
    } as Awaited<ReturnType<typeof api.getAgentConfig>>);
    vi.mocked(api.listBaselines).mockResolvedValue({
      org: "default",
      count: 4,
      baselines: [
        { type: "metric", confident: true },
        { type: "metric", confident: false },
        { type: "trace", confident: true },
        { type: "trace", confident: false },
      ],
    } as Awaited<ReturnType<typeof api.listBaselines>>);
    renderPageAt();

    const pulse = screen.getByRole("region", { name: "Agent pulse" });
    expect(await within(pulse).findByRole("link", { name: /Logs 12 patterns/ })).toBeTruthy();
    expect(within(pulse).getByRole("heading", { name: "Signal activity" })).toBeTruthy();
    expect(within(pulse).getByText("Current catalog · recorded events")).toBeTruthy();
    expect((await within(pulse).findByRole("link", { name: /Metrics 2 signals 1 ready/ })).getAttribute("href")).toBe("/agent/metrics");
    expect((await within(pulse).findByRole("link", { name: /Traces 2 signals 1 ready/ })).getAttribute("href")).toBe("/agent/traces");
    expect(within(pulse).getByRole("link", { name: /Shadow 3 events/ }).getAttribute("href")).toBe("/agent/decisions?tab=shadow");
    expect(within(pulse).getByRole("link", { name: /Detect 4 events/ }).getAttribute("href")).toBe("/agent/decisions?tab=detect");
    expect(within(pulse).getByRole("link", { name: /Agent overview/ }).getAttribute("href")).toBe("/agent");
    for (const removedContent of ["Runtime mode", "Configured AI model", "o4-mini", "openai"]) {
      expect(within(pulse).queryByText(removedContent, { exact: true })).toBeNull();
    }
  });

  it("keeps zero activity counts navigable", async () => {
    vi.mocked(api.status).mockResolvedValue({
      patterns: 0,
      shadow_events: 0,
      detect_events: 0,
    } as Awaited<ReturnType<typeof api.status>>);
    vi.mocked(api.listBaselines).mockResolvedValue({
      org: "default", count: 0, baselines: [],
    } as Awaited<ReturnType<typeof api.listBaselines>>);
    renderPageAt();

    const pulse = screen.getByRole("region", { name: "Agent pulse" });
    expect(await within(pulse).findByRole("link", { name: /Logs 0 patterns/ })).toBeTruthy();
    expect((await within(pulse).findByRole("link", { name: /Metrics 0 signals/ })).getAttribute("href")).toBe("/agent/metrics");
    expect((await within(pulse).findByRole("link", { name: /Traces 0 signals/ })).getAttribute("href")).toBe("/agent/traces");
    expect(within(pulse).getByRole("link", { name: /Shadow 0 events/ }).getAttribute("href")).toBe("/agent/decisions?tab=shadow");
    expect(within(pulse).getByRole("link", { name: /Detect 0 events/ }).getAttribute("href")).toBe("/agent/decisions?tab=detect");
  });

  it("keeps baseline loading visible until the activity request completes", async () => {
    let resolveBaselines!: (value: Awaited<ReturnType<typeof api.listBaselines>>) => void;
    vi.mocked(api.listBaselines).mockReturnValue(new Promise((resolve) => {
      resolveBaselines = resolve;
    }));
    renderPageAt();

    const pulse = screen.getByRole("region", { name: "Agent pulse" });
    expect(await within(pulse).findByText("Loading metric and trace activity")).toBeTruthy();
    resolveBaselines({ org: "default", count: 0, baselines: [] });
    expect(await within(pulse).findByRole("link", { name: /Metrics 0 signals/ })).toBeTruthy();
  });

  it("keeps baseline authorization failures as an Enterprise lock", async () => {
    renderPageAt();

    const pulse = screen.getByRole("region", { name: "Agent pulse" });
    expect(await within(pulse).findByRole("link", { name: "Enterprise license" })).toBeTruthy();
    expect(within(pulse).getByRole("link", { name: /Logs 12 patterns/ })).toBeTruthy();
    expect(within(pulse).queryByRole("link", { name: /Metrics 0 signals/ })).toBeNull();
    expect(within(pulse).queryByRole("link", { name: /Traces 0 signals/ })).toBeNull();
  });

  it("shows transient baseline failures without presenting them as a license lock", async () => {
    vi.mocked(api.listBaselines).mockRejectedValue(new ApiError(503, "Unavailable"));
    renderPageAt();

    const pulse = screen.getByRole("region", { name: "Agent pulse" });
    expect(await within(pulse).findByText("Couldn't load metric and trace activity", {}, { timeout: 3000 })).toBeTruthy();
    expect(within(pulse).getByRole("link", { name: /Logs 12 patterns/ })).toBeTruthy();
    expect(within(pulse).queryByRole("link", { name: "Enterprise license" })).toBeNull();
  });

  it("retains cached signal and baseline counts after transient refetch failures", async () => {
    const statusData = { patterns: 12, shadow_events: 3, detect_events: 4 } as Awaited<
      ReturnType<typeof api.status>
    >;
    const baselineData = {
      org: "default",
      count: 2,
      baselines: [
        { type: "metric", confident: true },
        { type: "trace", confident: false },
      ],
    } as Awaited<ReturnType<typeof api.listBaselines>>;
    vi.mocked(api.status).mockReset().mockRejectedValue(new ApiError(503, "Unavailable"));
    vi.mocked(api.status).mockResolvedValueOnce(statusData);
    vi.mocked(api.listBaselines).mockReset().mockRejectedValue(new TypeError("Failed to fetch"));
    vi.mocked(api.listBaselines).mockResolvedValueOnce(baselineData);
    const { queryClient } = renderPageAt();
    const pulse = screen.getByRole("region", { name: "Agent pulse" });

    expect(await within(pulse).findByRole("link", { name: /Logs 12 patterns/ })).toBeTruthy();
    expect(await within(pulse).findByRole("link", { name: /Metrics 1 signals 1 ready/ })).toBeTruthy();
    await act(async () => {
      await Promise.all([
        queryClient.refetchQueries({ queryKey: ["status-pulse"], exact: true }),
        queryClient.refetchQueries({ queryKey: ["baselines-pulse"], exact: true }),
      ]);
    });

    expect(within(pulse).getByRole("link", { name: /Logs 12 patterns/ })).toBeTruthy();
    expect(within(pulse).getByRole("link", { name: /Metrics 1 signals 1 ready/ })).toBeTruthy();
    expect(within(pulse).getByRole("link", { name: /Traces 1 signals/ })).toBeTruthy();
    expect(within(pulse).getByText("Couldn't load agent status")).toBeTruthy();
    expect(within(pulse).getByText("Couldn't load metric and trace activity")).toBeTruthy();
    expect(within(pulse).queryByRole("link", { name: "Enterprise license" })).toBeNull();
  });

  it("hides cached signal and premium baseline counts after authorization failures", async () => {
    const statusData = { patterns: 12, shadow_events: 3, detect_events: 4 } as Awaited<
      ReturnType<typeof api.status>
    >;
    const baselineData = {
      org: "default",
      count: 1,
      baselines: [{ type: "metric", confident: true }],
    } as Awaited<ReturnType<typeof api.listBaselines>>;
    vi.mocked(api.status).mockReset().mockRejectedValue(new ApiError(401, "Unauthorized"));
    vi.mocked(api.status).mockResolvedValueOnce(statusData);
    vi.mocked(api.listBaselines).mockReset().mockRejectedValue(new ApiError(401, "Unauthorized"));
    vi.mocked(api.listBaselines).mockResolvedValueOnce(baselineData);
    const { queryClient } = renderPageAt();
    const pulse = screen.getByRole("region", { name: "Agent pulse" });

    expect(await within(pulse).findByRole("link", { name: /Logs 12 patterns/ })).toBeTruthy();
    expect(await within(pulse).findByRole("link", { name: /Metrics 1 signals 1 ready/ })).toBeTruthy();
    await act(async () => {
      await Promise.all([
        queryClient.refetchQueries({ queryKey: ["status-pulse"], exact: true }),
        queryClient.refetchQueries({ queryKey: ["baselines-pulse"], exact: true }),
      ]);
    });

    expect(within(pulse).queryByRole("link", { name: /Logs 12 patterns/ })).toBeNull();
    expect(within(pulse).queryByRole("link", { name: /Metrics 1 signals/ })).toBeNull();
    expect(within(pulse).queryByRole("link", { name: "Enterprise license" })).toBeNull();
    expect(within(pulse).getByText("Couldn't load agent status")).toBeTruthy();
    expect(within(pulse).getByText("Couldn't load metric and trace activity")).toBeTruthy();

    vi.mocked(api.listBaselines).mockRejectedValueOnce(new ApiError(403, "Forbidden"));
    await act(async () => {
      await queryClient.refetchQueries({ queryKey: ["baselines-pulse"], exact: true });
    });
    expect(within(pulse).getByRole("link", { name: "Enterprise license" })).toBeTruthy();
    expect(within(pulse).queryByRole("link", { name: /Metrics 1 signals/ })).toBeNull();
  });

  it("shows the disabled state without requesting signal activity", async () => {
    vi.mocked(api.getAgentConfig).mockResolvedValue({ enable: false } as Awaited<
      ReturnType<typeof api.getAgentConfig>
    >);
    renderPageAt();

    const pulse = screen.getByRole("region", { name: "Agent pulse" });
    expect(await within(pulse).findByText("Agent is disabled")).toBeTruthy();
    expect(within(pulse).getByRole("heading", { name: "Signal activity" })).toBeTruthy();
    expect(within(pulse).queryByRole("link", { name: /Logs|Metrics|Traces|Shadow|Detect/ })).toBeNull();
    expect(api.listBaselines).not.toHaveBeenCalled();
  });
});

describe("NowPage without the agent", () => {
  it("never asks for agent status while the agent is off or its config is unknown", async () => {
    vi.mocked(api.status).mockClear();
    let finish!: (value: Awaited<ReturnType<typeof api.getAgentConfig>>) => void;
    vi.mocked(api.getAgentConfig).mockReturnValue(new Promise((resolve) => { finish = resolve; }));
    renderPageAt();
    await act(async () => { await new Promise((resolve) => setTimeout(resolve, 20)); });
    expect(api.status).not.toHaveBeenCalled();
    await act(async () => { finish({ enable: false } as Awaited<ReturnType<typeof api.getAgentConfig>>); });
    await act(async () => { await new Promise((resolve) => setTimeout(resolve, 20)); });
    expect(api.status).not.toHaveBeenCalled();
  });

  it("asks for agent status once the config says the agent is on", async () => {
    vi.mocked(api.status).mockClear();
    vi.mocked(api.getAgentConfig).mockResolvedValue({ enable: true } as Awaited<ReturnType<typeof api.getAgentConfig>>);
    renderPageAt();
    await vi.waitFor(() => expect(api.status).toHaveBeenCalled());
  });
});

// The server serves counts from a cache. When it has no count it says so
// (count_unknown) instead of sending zeros, and the page must never turn that,
// or a failed request, into "0 open". A stale count still shows its number,
// with the time it was taken on hover.
describe("NowPage count availability", () => {
  const countedAt = "2026-11-27T09:41:00Z";
  const countedHint = `Counted at ${format(new Date(countedAt), "HH:mm")}`;

  function knownCounts(extra: Partial<IncidentCounts> = {}): IncidentCounts {
    return {
      count_window: "30d",
      ...oc(5, 10),
      by_status: byStatus,
      ...extra,
    };
  }

  async function openTile(): Promise<HTMLElement> {
    return screen.findByRole("link", { name: /^Open .*created · 24h$/ });
  }

  function expectNoZeroCount() {
    // No tile, tab badge or header badge may read 0.
    expect(screen.queryByText("0")).toBeNull();
    expect(screen.queryByText("All clear — nothing open")).toBeNull();
    expect(screen.queryByText(/AI: 0/)).toBeNull();
  }

  it("shows a dash, never 0, when the server has no count", async () => {
    vi.mocked(api.listIncidents).mockResolvedValue([]);
    // The wire shape: number fields omitted, only the unknown flag.
    vi.mocked(api.incidentCounts).mockResolvedValue({
      count_window: "30d",
      count_unknown: true,
    } as unknown as IncidentCounts);
    renderPageAt();

    const header = await screen.findByRole("link", {
      name: "Open incidents: Count unavailable",
    });
    expect(within(header).getByText("—")).toBeTruthy();
    const tile = await openTile();
    expect(within(tile).getByText("—")).toBeTruthy();
    expect(within(tile).getByText("Count unavailable")).toBeTruthy();
    for (const name of [/^Acked /, /^Resolved /]) {
      const t = screen.getByRole("link", { name });
      expect(within(t).getByText("Count unavailable")).toBeTruthy();
    }
    const aiTab = screen.getByRole("tab", { name: /^AI Detected/ });
    expect(within(aiTab).getByText("Count unavailable")).toBeTruthy();
    expectNoZeroCount();
  });

  it("shows a dash when the first counts request fails", async () => {
    vi.mocked(api.listIncidents).mockResolvedValue([]);
    vi.mocked(api.incidentCounts).mockRejectedValue(
      new ApiError(503, "counts unavailable"),
    );
    renderPageAt();

    await screen.findByRole("link", {
      name: "Open incidents: Count unavailable",
    });
    const tile = await openTile();
    expect(within(tile).getByText("Count unavailable")).toBeTruthy();
    expectNoZeroCount();
  });

  it("keeps the last good count with a counted-at hint when a refetch fails", async () => {
    vi.mocked(api.listIncidents).mockResolvedValue([]);
    vi.mocked(api.incidentCounts).mockResolvedValue(
      knownCounts({ counted_at: countedAt, stale: false }),
    );
    const { queryClient } = renderPageAt();

    const tile = await openTile();
    await within(tile).findByText("1");
    expect(screen.queryByTitle(/Counted at/)).toBeNull();

    vi.mocked(api.incidentCounts).mockRejectedValue(
      new ApiError(503, "counts unavailable"),
    );
    await act(() =>
      queryClient.refetchQueries({ queryKey: ["incidents", "counts"] }),
    );

    // The number stays, dated in the browser from counted_at even though the
    // server never said stale.
    const value = await within(await openTile()).findByText("1");
    expect(value.closest("[title]")?.getAttribute("title")).toBe(countedHint);
    const header = screen.getByTitle(new RegExp(`3 open in .* · ${countedHint}$`));
    expect(within(header).getByText("AI: 1 · Webhook: 2")).toBeTruthy();
    expect(screen.queryByText("Count unavailable")).toBeNull();
    expectNoZeroCount();
  });

  it("keeps the page and flags only the refresh button when a refetch fails", async () => {
    vi.mocked(api.listIncidents).mockResolvedValue([
      incident({ id: "kept-row", title: "Kept feed row" }),
    ]);
    vi.mocked(api.incidentCounts).mockResolvedValue(
      knownCounts({ counted_at: countedAt }),
    );
    const { queryClient } = renderPageAt();
    const feed = await screen.findByRole("region", { name: /^Incident feed/ });
    await within(feed).findByText("Kept feed row");
    expect(screen.getByRole("button", { name: "Refresh now" })).toBeTruthy();

    const fail = new ApiError(503, "timed out");
    vi.mocked(api.listIncidents).mockRejectedValue(fail);
    vi.mocked(api.incidentCounts).mockRejectedValue(fail);
    await act(() => queryClient.refetchQueries({ queryKey: ["incidents"] }));

    // The compact notice replaces the refresh button in place...
    await screen.findByRole("button", { name: "Couldn't refresh. Retry" });
    expect(screen.getByRole("status").textContent).toMatch(/Couldn't refresh/);
    // ...and nothing else changes: no error card, the feed and banner stay.
    expect(screen.queryByText("Couldn't load incidents")).toBeNull();
    expect(screen.queryByRole("alert")).toBeNull();
    expect(within(feed).getByText("Kept feed row")).toBeTruthy();
    expect(screen.queryByText(/Incident list unavailable/)).toBeNull();
    expect(screen.getByRole("region", { name: "Open incidents" })).toBeTruthy();

    // The next good poll clears the notice.
    vi.mocked(api.listIncidents).mockResolvedValue([
      incident({ id: "kept-row", title: "Kept feed row" }),
    ]);
    vi.mocked(api.incidentCounts).mockResolvedValue(knownCounts());
    await act(() => queryClient.refetchQueries({ queryKey: ["incidents"] }));
    await screen.findByRole("button", { name: "Refresh now" });
  });

  it("shows the number for a known, fresh count with no counted-at hint", async () => {
    vi.mocked(api.listIncidents).mockResolvedValue([]);
    vi.mocked(api.incidentCounts).mockResolvedValue(
      knownCounts({ counted_at: countedAt, stale: false }),
    );
    renderPageAt();

    const tile = await openTile();
    await within(tile).findByText("1");
    expect(within(tile).queryByText("Count unavailable")).toBeNull();
    expect(screen.queryByTitle(/Counted at/)).toBeNull();
    expect(
      screen.queryByRole("link", { name: "Open incidents: Count unavailable" }),
    ).toBeNull();
  });

  it("shows the number plus a counted-at hint for a stale count", async () => {
    vi.mocked(api.listIncidents).mockResolvedValue([]);
    vi.mocked(api.incidentCounts).mockResolvedValue(
      knownCounts({ counted_at: countedAt, stale: true }),
    );
    renderPageAt();

    const tile = await openTile();
    const value = await within(tile).findByText("1");
    expect(value.closest("[title]")?.getAttribute("title")).toBe(countedHint);
    // The header badge keeps its number and carries the same hint.
    const header = screen.getByTitle(new RegExp(`3 open in .* · ${countedHint}$`));
    expect(within(header).getByText("AI: 1 · Webhook: 2")).toBeTruthy();
    // The origin tab badges carry it too.
    const aiTab = screen.getByRole("tab", { name: /^AI Detected 1$/ });
    expect(within(aiTab).getByTitle(countedHint)).toBeTruthy();
  });

  it("treats a response without the new fields as a known count, as before", async () => {
    vi.mocked(api.listIncidents).mockResolvedValue([]);
    vi.mocked(api.incidentCounts).mockResolvedValue(knownCounts());
    renderPageAt();

    const tile = await openTile();
    await within(tile).findByText("1");
    expect(screen.getByText("AI: 1 · Webhook: 2")).toBeTruthy();
    expect(screen.queryByText("Count unavailable")).toBeNull();
    expect(screen.queryByTitle(/Counted at/)).toBeNull();
  });
});
