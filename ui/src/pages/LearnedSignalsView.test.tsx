// @vitest-environment jsdom
import { describe, it, expect, afterEach, beforeEach, vi } from "vitest";
import {
  render,
  screen,
  cleanup,
  fireEvent,
  within,
} from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import type { ReactElement } from "react";
import { ToastProvider } from "@/components/Toast";
import { MetricsPage, TracesPage } from "./LearnedSignalsView";
import {
  api,
  type BaselineRow,
  type BaselinesResponse,
} from "@/lib/api";

// Item-1 raw-sample-store, OSS UI follow-up. Enterprise adds a capped, redacted
// `latest_sample` to each metric/trace BaselineRow (the metric/trace parity of
// the logs pattern's "Example log line"). This pins that the Eye/peek renders
// that raw example under the RIGHT per-type label ("Example metric" on metrics,
// "Example trace" on traces) when present, and degrades to "No example captured
// yet" when absent — which is the community/OSS case, where the enterprise brain
// never runs and the field is omitted. Kept in the DETAIL/peek only, never a
// list column, exactly like logs.
//
// The row's baselines query is the only source; the peek is driven off it. The
// deployment probe answers 403 (community / not-admin) so the licensed-admin
// bulk column stays absent and does not add extra Eye buttons — the single row
// gives one unambiguous "View details" affordance.
vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      listBaselines: vi.fn(),
      listServiceOverrides: vi.fn().mockResolvedValue([]),
      getSSODeployment: vi
        .fn()
        .mockRejectedValue(new actual.ApiError(403, "community")),
    },
  };
});

afterEach(cleanup);

beforeEach(() => {
  vi.mocked(api.listServiceOverrides).mockResolvedValue([]);
});

function metricRow(overrides: Partial<BaselineRow> = {}): BaselineRow {
  return {
    type: "metric",
    source: "prometheus",
    service: "checkout",
    signal: "latency_p99",
    kind: "latency",
    expected_mean: 0.04,
    expected_std: 0.005,
    unit: "ms",
    display_mean: 40,
    display_std: 5,
    confident: true,
    observations: 22,
    threshold: 20,
    last_updated: new Date().toISOString(),
    readiness: { ready: true, seen: 22, needed: 20, rate_per_min: 1 },
    ...overrides,
  };
}

function respond(rows: BaselineRow[]): BaselinesResponse {
  return { org: "acme", count: rows.length, baselines: rows };
}

function renderPage(page: ReactElement) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <ToastProvider>
        <MemoryRouter>{page}</MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

// openPeek waits for the single learned row to render, clicks its Eye, and
// returns the opened Details panel dialog for scoped assertions.
async function openPeek(): Promise<HTMLElement> {
  const eye = await screen.findByTitle("View details");
  fireEvent.click(eye);
  return screen.getByRole("dialog", { name: "Details panel" });
}

describe("LearnedSignalsView peek — raw-sample-store 'Example metric/trace'", () => {
  it("renders latest_sample under 'Example metric' in the metrics peek", async () => {
    const sample = "latency_p99{service=checkout} = 42ms";
    vi.mocked(api.listBaselines).mockResolvedValue(
      respond([metricRow({ latest_sample: sample })]),
    );

    renderPage(<MetricsPage />);
    const panel = await openPeek();

    expect(within(panel).getByText("Example metric")).toBeTruthy();
    expect(within(panel).getByText(sample)).toBeTruthy();
    // Never the absent fallback when a sample is present.
    expect(within(panel).queryByText("No example captured yet")).toBeNull();
    // The logs page owns "Example log line"; the metric peek must not borrow it.
    expect(within(panel).queryByText("Example log line")).toBeNull();
  });

  it("renders latest_sample under 'Example trace' in the traces peek", async () => {
    const sample = "checkout POST /pay latency_p99 = 42ms";
    vi.mocked(api.listBaselines).mockResolvedValue(
      respond([
        metricRow({
          type: "trace",
          source: "traces",
          operation: "POST /pay",
          latest_sample: sample,
        }),
      ]),
    );

    renderPage(<TracesPage />);
    const panel = await openPeek();

    expect(within(panel).getByText("Example trace")).toBeTruthy();
    expect(within(panel).getByText(sample)).toBeTruthy();
    expect(within(panel).queryByText("Example metric")).toBeNull();
  });

  it("degrades to 'No example captured yet' when latest_sample is absent (community/OSS)", async () => {
    // No latest_sample — the enterprise brain didn't run, so the field is
    // omitted, exactly like the logs 'Example log line' degrade.
    vi.mocked(api.listBaselines).mockResolvedValue(respond([metricRow()]));

    renderPage(<MetricsPage />);
    const panel = await openPeek();

    // The labeled field is still shown, so the absence is explicit, not hidden.
    expect(within(panel).getByText("Example metric")).toBeTruthy();
    expect(within(panel).getByText("No example captured yet")).toBeTruthy();
  });
});
