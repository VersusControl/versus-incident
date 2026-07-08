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
import { ToastProvider } from "@/components/Toast";
import { PatternsPage } from "./PatternsPage";
import {
  api,
  type Pattern,
  type SeasonalBucket,
} from "@/lib/api";

// The logs LIST endpoint strips `samples` and (on the Postgres backend) can
// carry a leaner baseline set than the full record. These pin that opening the
// peek FETCHES the pattern DETAIL (the same read the full page uses) and
// renders the complete baselines — incl. the hour-of-day grid — and the
// redacted sample example from THAT detail, not the thin list row.
//
// The deployment / license probes answer 403 (community / OSS) so the
// licensed-admin bulk column stays absent and each row shows exactly one
// unambiguous "View details" eye.
vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      listPatterns: vi.fn(),
      getPattern: vi.fn(),
      listBaselines: vi
        .fn()
        .mockRejectedValue(new actual.ApiError(403, "community")),
      listServiceOverrides: vi.fn().mockResolvedValue([]),
      getSSODeployment: vi
        .fn()
        .mockRejectedValue(new actual.ApiError(403, "community")),
    },
  };
});

afterEach(cleanup);

function seasonalOneWarmedHour(): SeasonalBucket[] {
  return Array.from({ length: 24 }, (_, h) =>
    h === 0
      ? { mean: 7.5, variance: 0.25, count: 4 }
      : { mean: 0, variance: 0, count: 0 },
  );
}

// listRow is what the LIST endpoint returns: NO samples, and no seasonal /
// cumulative baselines (the leaner Postgres list shape).
function listRow(overrides: Partial<Pattern> = {}): Pattern {
  return {
    id: "p-checkout-1",
    template: "payment <*> failed",
    first_seen: new Date().toISOString(),
    last_seen: new Date().toISOString(),
    count: 1200,
    baseline_frequency: 1.3,
    verdict: "",
    rule_name: "checkout",
    source: "logs",
    service: "checkout",
    readiness: { ready: false, seen: 40, needed: 100, rate_per_min: 2 },
    ...overrides,
  };
}

// detail is the DETAIL read: full baselines + the redacted sample ring.
function detail(overrides: Partial<Pattern> = {}): Pattern {
  return {
    ...listRow(),
    baseline_variance: 0.25,
    baseline_avg: 1.1,
    seasonal: seasonalOneWarmedHour(),
    samples: ["payment 8471 failed", "payment 22 failed"],
    ...overrides,
  };
}

function renderPage() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <ToastProvider>
        <MemoryRouter>
          <PatternsPage />
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

async function openPeek(): Promise<HTMLElement> {
  const eye = await screen.findByTitle("View details");
  fireEvent.click(eye);
  return screen.getByRole("dialog", { name: "Details panel" });
}

describe("PatternsPage peek — fetches the pattern DETAIL", () => {
  beforeEach(() => {
    vi.mocked(api.listServiceOverrides).mockResolvedValue([]);
    vi.mocked(api.listPatterns).mockResolvedValue([listRow()]);
    vi.mocked(api.getPattern).mockResolvedValue(detail());
  });

  it("calls getPattern for the opened row (same read as the full page)", async () => {
    renderPage();
    await openPeek();
    expect(api.getPattern).toHaveBeenCalledWith("p-checkout-1");
  });

  it("renders the redacted sample example from the fetched detail", async () => {
    renderPage();
    const panel = await openPeek();
    // The list row carries NO samples — the example can only come from detail.
    expect(await within(panel).findByText("payment 22 failed")).toBeTruthy();
    expect(within(panel).getByText("Example log line")).toBeTruthy();
    expect(
      within(panel).queryByText("No example captured yet"),
    ).toBeNull();
  });

  it("renders the detail's baselines incl. the warmed hour-of-day cell", async () => {
    renderPage();
    const panel = await openPeek();
    // seasonal[0] warmed to 7.5/s — only present on the detail read.
    expect(await within(panel).findByText("7.5")).toBeTruthy();
    // The cumulative-mean baseline is likewise a detail-only number.
    expect(within(panel).getByText(/≈ 1\.1\/s/)).toBeTruthy();
  });

  it("wraps the pattern template instead of scrolling it sideways", async () => {
    renderPage();
    const panel = await openPeek();
    const pre = within(panel).getByText("payment <*> failed");
    expect(pre.tagName).toBe("PRE");
    // The whole template is visible — it wraps rather than scrolling left/right.
    expect(pre.className).toContain("whitespace-pre-wrap");
    expect(pre.className).toContain("break-words");
    expect(pre.className).not.toContain("overflow-auto");
  });
});

// The Logs Verdict cell renders a learning hint next to the "Still learning"
// pill. Which hint it shows depends on whether count-based auto-promotion is
// enabled (readiness.needed):
//   needed > 0  → a seen/needed progress meter ("40 / 100").
//   needed === 0 → auto-promotion is disabled, so the pattern never becomes
//     known by count; the cell must say so and point the operator at the manual
//     path, instead of showing a bare "Still learning" with no progress and no
//     reason (the operator's "learning without progress" report).
describe("PatternsPage — Verdict cell learning hint", () => {
  beforeEach(() => {
    vi.mocked(api.listServiceOverrides).mockResolvedValue([]);
    vi.mocked(api.getPattern).mockResolvedValue(detail());
  });

  it("shows the seen/needed progress meter when a count target exists", async () => {
    vi.mocked(api.listPatterns).mockResolvedValue([
      listRow({
        readiness: { ready: false, seen: 40, needed: 100, rate_per_min: 2 },
      }),
    ]);
    renderPage();
    const row = await screen.findByText("payment <*> failed");
    const cell = row.closest("tr")!;
    expect(within(cell).getByRole("progressbar")).toBeTruthy();
    expect(cell.textContent).toContain("40");
    expect(cell.textContent).toContain("100");
    expect(within(cell).queryByText(/auto-promotion off/)).toBeNull();
  });

  it("explains auto-promotion is off when there is no count target (needed === 0)", async () => {
    vi.mocked(api.listPatterns).mockResolvedValue([
      listRow({
        count: 3304,
        readiness: { ready: false, seen: 3304, needed: 0, rate_per_min: 12.6 },
      }),
    ]);
    renderPage();
    const row = await screen.findByText("payment <*> failed");
    const cell = row.closest("tr")!;
    expect(within(cell).getByText(/auto-promotion off/)).toBeTruthy();
    expect(within(cell).getByText(/mark known by hand/)).toBeTruthy();
    // No progressbar meter for the indeterminate state.
    expect(within(cell).queryByRole("progressbar")).toBeNull();
  });
});

