// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { AgentOverviewPage } from "./AgentOverviewPage";
import {
  ApiError,
  api,
  type AgentConfigView,
  type BaselineRow,
} from "@/lib/api";

vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      getAgentConfig: vi.fn(),
      status: vi.fn(),
      shadowStats: vi.fn(),
      detectStats: vi.fn(),
      listPatterns: vi.fn(),
      listShadow: vi.fn(),
      listServices: vi.fn(),
      listBaselines: vi.fn(),
    },
  };
});

afterEach(cleanup);

const enabledConfig = { enable: true, mode: "detect", ai: { enable: true, model: "test" } } as AgentConfigView;
const disabledConfig = { ...enabledConfig, enable: false };

function renderPage() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
        <AgentOverviewPage />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

function setupBase(config: AgentConfigView = enabledConfig) {
  vi.mocked(api.getAgentConfig).mockResolvedValue(config);
  vi.mocked(api.status).mockResolvedValue({ patterns: 0, dirty: false });
  vi.mocked(api.shadowStats).mockResolvedValue({
    events: 8,
    total_signals: 20,
    verdicts: { spike: 8 },
    occurrences: 8,
  });
  vi.mocked(api.detectStats).mockResolvedValue({});
  vi.mocked(api.listPatterns).mockResolvedValue([]);
  vi.mocked(api.listShadow).mockResolvedValue([]);
  vi.mocked(api.listServices).mockResolvedValue({});
  vi.mocked(api.listBaselines).mockRejectedValue(new ApiError(403, "community"));
}

function card(title: string): HTMLElement {
  const heading = screen.getByRole("heading", { name: title });
  const element = heading.closest(".card");
  if (!element) throw new Error(`Card not found: ${title}`);
  return element as HTMLElement;
}

describe("AgentOverviewPage lifetime breakdowns", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    setupBase();
  });

  it("renders the grouped detect breakdown with its values and no shadow verdict card", async () => {
    vi.mocked(api.detectStats).mockResolvedValue({
      outcome_emitted: 6,
      outcome_cached: 2,
      verdict_spike: 3,
      verdict_normal: 1,
      severity_high: 4,
      severity_low: 1,
    });
    renderPage();

    const detect = card("Detect breakdown");
    expect(await within(detect).findByText("spike")).toBeTruthy();
    expect(within(detect).getByText("3")).toBeTruthy();
    expect(within(detect).getByText("high")).toBeTruthy();
    expect(within(detect).getByText("4")).toBeTruthy();
    expect(within(detect).queryByText("emitted")).toBeNull();
    expect(within(detect).queryByText("Outcomes")).toBeNull();
    expect(screen.queryByText("Verdict breakdown (shadow)")).toBeNull();
  });

  it("renders exactly two bar-chart cards under Logs, Metrics & Traces", async () => {
    vi.mocked(api.status).mockResolvedValue({ patterns: 7, dirty: false });
    vi.mocked(api.listBaselines).mockResolvedValue({
      baselines: [
        { type: "metric", confident: true } as BaselineRow,
        { type: "metric", confident: false } as BaselineRow,
        { type: "trace", confident: true } as BaselineRow,
      ],
    });
    vi.mocked(api.detectStats).mockResolvedValue({
      outcome_emitted: 4,
      verdict_spike: 2,
      severity_high: 1,
    });
    renderPage();

    const heading = await screen.findByRole("heading", {
      name: "Logs, Metrics & Traces",
    });
    const section = heading.closest("section");
    if (!section) throw new Error("Signals section not found");

    const signals = await within(section).findByRole("heading", {
      name: "Signals learned",
    });
    expect(signals).toBeTruthy();
    expect(within(section).getByRole("heading", { name: "Detect breakdown" })).toBeTruthy();
    expect(section.querySelectorAll(".card")).toHaveLength(2);
    const cards = card("Signals learned").parentElement;
    expect(cards?.className).toContain("lg:grid-cols-2");
    expect(card("Detect breakdown").parentElement).toBe(cards);
    expect(
      within(card("Signals learned")).getByRole("progressbar", { name: "Logs: 7" }),
    ).toBeTruthy();
    expect(
      within(card("Signals learned")).getByRole("progressbar", { name: "Metrics: 2" }),
    ).toBeTruthy();
    expect(
      within(card("Signals learned")).getByRole("progressbar", { name: "Traces: 1" }),
    ).toBeTruthy();
    expect(within(card("Detect breakdown")).getByText("AI Detect")).toBeTruthy();
    expect(within(card("Detect breakdown")).getByText("AI severity")).toBeTruthy();
    expect(within(card("Detect breakdown")).queryByText("Outcomes")).toBeNull();
    expect(within(section).getAllByRole("progressbar")).toHaveLength(5);

    const lifetime = screen
      .getByRole("heading", { name: "Lifetime totals" })
      .closest("section");
    expect(lifetime && within(lifetime).queryByText("Detect breakdown")).toBeNull();
  });

  it("renders chart-shaped skeletons while detect stats are pending", async () => {
    vi.mocked(api.detectStats).mockReturnValue(new Promise(() => {}));
    renderPage();

    const detect = card("Detect breakdown");
    expect(detect.querySelectorAll(".sk")).toHaveLength(14);
    expect(within(detect).queryByRole("progressbar")).toBeNull();
  });

  it("renders an error distinct from the disabled state", async () => {
    vi.mocked(api.detectStats).mockRejectedValue(new Error("offline"));
    renderPage();

    expect(
      await within(card("Detect breakdown")).findByText(
        "Couldn't load — use Retry in the error above.",
        {},
        { timeout: 5_000 },
      ),
    ).toBeTruthy();
    expect(within(card("Detect breakdown")).queryByText(/disabled/i)).toBeNull();
  });

  it("renders the disabled note when the agent is off and stats fail", async () => {
    setupBase(disabledConfig);
    vi.mocked(api.detectStats).mockRejectedValue(new Error("agent off"));
    renderPage();

    expect(
      await within(card("Detect breakdown")).findByText(
        "Unavailable while the agent is disabled.",
        {},
        { timeout: 5_000 },
      ),
    ).toBeTruthy();
    expect(within(card("Detect breakdown")).queryByText(/use Retry/i)).toBeNull();
  });

  it("renders the configured empty states when no groups have values", async () => {
    renderPage();

    expect(
      await within(card("Detect breakdown")).findByText("No detect-mode calls yet"),
    ).toBeTruthy();
  });
});