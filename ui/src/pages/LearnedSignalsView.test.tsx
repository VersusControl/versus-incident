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

describe("LearnedSignalsView — Source label is driven off row.source, not the type", () => {
  it("renders 'CloudWatch' as the Source and shows the row's real unit for a cloudwatch_metrics row", async () => {
    vi.mocked(api.listBaselines).mockResolvedValue(
      respond([
        metricRow({
          source: "cloudwatch_metrics",
          kind: "saturation",
          unit: "Bytes/Second",
          display_mean: 1200,
          display_std: 80,
        }),
      ]),
    );

    renderPage(<MetricsPage />);
    const panel = await openPeek();

    const source = within(panel).getByText("Source").parentElement as HTMLElement;
    expect(within(source).getByText("CloudWatch")).toBeTruthy();
    // The Prometheus label must NOT be borrowed for a CloudWatch row.
    expect(within(source).queryByText("Prometheus")).toBeNull();

    // The real backend unit surfaces in the dedicated Unit field...
    const unit = within(panel).getByText("Unit").parentElement as HTMLElement;
    expect(within(unit).getByText("Bytes/Second")).toBeTruthy();
    // ...and flows through to the "What's normal now" value line unchanged.
    expect(within(panel).getAllByText(/Bytes\/Second/).length).toBeGreaterThan(1);
  });

  it("still renders 'Prometheus' and its unit for an existing prometheus metric row (no regression)", async () => {
    vi.mocked(api.listBaselines).mockResolvedValue(
      respond([metricRow({ source: "prometheus", unit: "ms" })]),
    );

    renderPage(<MetricsPage />);
    const panel = await openPeek();

    const source = within(panel).getByText("Source").parentElement as HTMLElement;
    expect(within(source).getByText("Prometheus")).toBeTruthy();

    const unit = within(panel).getByText("Unit").parentElement as HTMLElement;
    expect(within(unit).getByText("ms")).toBeTruthy();
  });

  it("still renders 'Traces' as the Source for a trace row", async () => {
    vi.mocked(api.listBaselines).mockResolvedValue(
      respond([
        metricRow({
          type: "trace",
          source: "traces",
          operation: "POST /pay",
          unit: "ms",
        }),
      ]),
    );

    renderPage(<TracesPage />);
    const panel = await openPeek();

    const source = within(panel).getByText("Source").parentElement as HTMLElement;
    expect(within(source).getByText("Traces")).toBeTruthy();
  });

  it("renders the '—' placeholder for a raw gauge with an empty unit, without breaking", async () => {
    vi.mocked(api.listBaselines).mockResolvedValue(
      respond([
        metricRow({
          source: "cloudwatch_metrics",
          kind: "other",
          unit: "",
          display_mean: 3,
          display_std: 0.5,
        }),
      ]),
    );

    renderPage(<MetricsPage />);
    const panel = await openPeek();

    // Source still resolves off row.source even with no unit.
    const source = within(panel).getByText("Source").parentElement as HTMLElement;
    expect(within(source).getByText("CloudWatch")).toBeTruthy();

    // Empty unit renders the neutral placeholder — never "undefined" or a
    // dangling separator.
    const unit = within(panel).getByText("Unit").parentElement as HTMLElement;
    expect(within(unit).getByText("—")).toBeTruthy();
    expect(within(panel).queryByText(/undefined/)).toBeNull();
  });
});

// A long Service value (a full ARN / target-group / cluster name) must not
// widen the table and push the right-hand columns behind a horizontal scroll.
// The table is fixed-layout and the Service cell truncates with the full value
// exposed via a title tooltip, so every other column stays rendered.
describe("LearnedSignalsView — long Service value truncates, columns stay visible", () => {
  const LONG_SERVICE =
    "arn:aws:elasticloadbalancing:us-east-1:123456789012:targetgroup/checkout-very-long-target-group-name-that-would-overflow/1a2b3c4d5e6f7890";

  it("truncates the Service cell and exposes the full value via a title tooltip", async () => {
    vi.mocked(api.listBaselines).mockResolvedValue(
      respond([metricRow({ service: LONG_SERVICE, unit: "Bytes/Second" })]),
    );

    renderPage(<MetricsPage />);
    // Wait for the row (its Eye action) to render.
    await screen.findByTitle("View details");

    // The Service cell exposes the FULL value via title and clips overflow with
    // the `truncate` utility so one long cell can't dictate the table width.
    const serviceCell = screen.getByTitle(LONG_SERVICE);
    expect(serviceCell.className).toContain("truncate");

    // The right-hand columns are still rendered — the Unit value survives with
    // its own truncating tooltip, proving nothing got pushed off-screen.
    expect(screen.getByTitle("Bytes/Second")).toBeTruthy();
  });
});

// Click-to-sort on the Last seen column sorts by the real last_updated
// timestamp (never the humanized "31 minutes ago" string). The column defaults
// to most-recent-first (descending); clicking its header button flips the
// direction and toggles the th's aria-sort so screen readers announce it.
describe("LearnedSignalsView — Last seen click-to-sort", () => {
  const OLD = "2026-01-01T00:00:00Z";
  const MID = "2026-04-01T00:00:00Z";
  const NEW = "2026-07-01T00:00:00Z";

  function threeRows() {
    return respond([
      metricRow({ signal: "alpha", last_updated: OLD }),
      metricRow({ signal: "charlie", last_updated: NEW }),
      metricRow({ signal: "bravo", last_updated: MID }),
    ]);
  }

  // order reads the rendered signal per data row, top-to-bottom.
  function order(): string[] {
    const labels = ["alpha", "bravo", "charlie"];
    return screen
      .getAllByRole("row")
      .slice(1)
      .map((r) => labels.find((l) => (r.textContent ?? "").includes(l)))
      .filter((l): l is string => Boolean(l));
  }

  it("defaults to most-recent-first and flips to oldest-first on header click", async () => {
    vi.mocked(api.listBaselines).mockResolvedValue(threeRows());

    renderPage(<MetricsPage />);
    await screen.findAllByTitle("View details");

    // Default sort: newest last_updated first, regardless of the incoming order.
    expect(order()).toEqual(["charlie", "bravo", "alpha"]);

    const th = screen.getByRole("columnheader", { name: /Last seen/i });
    expect(th.getAttribute("aria-sort")).toBe("descending");

    // Clicking the header button flips to ascending (oldest first).
    fireEvent.click(screen.getByRole("button", { name: "Last seen" }));

    expect(order()).toEqual(["alpha", "bravo", "charlie"]);
    expect(
      screen
        .getByRole("columnheader", { name: /Last seen/i })
        .getAttribute("aria-sort"),
    ).toBe("ascending");

    // Clicking again returns to descending.
    fireEvent.click(screen.getByRole("button", { name: "Last seen" }));
    expect(order()).toEqual(["charlie", "bravo", "alpha"]);
  });
});


