// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";

import { ToastProvider } from "@/components/Toast";
import { SLORecommendationsPage } from "./SLORecommendationsPage";
import {
  ApiError,
  type SLOAdoptedSLO,
  type SLOAutodefineConfig,
  type SLORecommendation,
  type SLORecommendationSLI,
  type SLORecommendationsResponse,
} from "@/lib/api";
import { api } from "@/lib/api";
import { fmtAbs } from "@/lib/format";

// The SLI/SLO auto-define page renders one scannable BLOCK per indicator:
// what to measure → what to target → where you are now → what it costs →
// what will page you → how to implement → adopt. Everything past
// {name,type,signal,objective,window_days,rationale,confidence} is optional
// platform enrichment, so these tests pin BOTH the enriched render and the
// degraded render an older server produces.
vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      listSLORecommendations: vi.fn(),
      getSLOAutodefineConfig: vi.fn(),
      setSLOAutodefineConfig: vi.fn(),
      adoptSLORecommendation: vi.fn(),
      unadoptSLORecommendation: vi.fn(),
    },
  };
});

afterEach(cleanup);

beforeEach(() => {
  // The cadence control is admin-only; 403 hides it and leaves the
  // recommendation cards as the only thing under test.
  vi.mocked(api.getSLOAutodefineConfig).mockRejectedValue(
    new ApiError(403, "not an admin"),
  );
  vi.mocked(api.adoptSLORecommendation).mockResolvedValue({ ok: true });
  vi.mocked(api.unadoptSLORecommendation).mockResolvedValue({ ok: true });
});

// legacySLI is what an older server sends: no query, no observation, no
// budget, no burn alerts, no evidence, no adoptable flag.
function legacySLI(over: Partial<SLORecommendationSLI> = {}): SLORecommendationSLI {
  return {
    name: "Checkout availability",
    type: "availability",
    signal: "http_requests_total",
    objective: 0.995,
    window_days: 30,
    rationale: "p99 error ratio held under 0.3% for two weeks",
    confidence: 0.5,
    ...over,
  };
}

// enrichedSLI is the full contract the platform now computes.
function enrichedSLI(over: Partial<SLORecommendationSLI> = {}): SLORecommendationSLI {
  return {
    ...legacySLI(),
    query:
      'sum(rate(http_requests_total{service="checkout",code!~"5.."}[5m]))',
    good_events: "requests that returned a non-5xx status",
    valid_events: "all requests reaching the service",
    observed: 0.9987,
    headroom_pp: 0.37,
    breaching: false,
    error_budget: { ratio: 0.005, minutes: 219, consumed_ratio: 0.62 },
    burn_alerts: [
      {
        name: "fast",
        long_window: "1h0m0s",
        short_window: "5m0s",
        burn_rate: 14.4,
        bad_ratio_threshold: 0.072,
        budget_pct_per_window: 2,
      },
      {
        name: "slow",
        long_window: "6h0m0s",
        short_window: "30m0s",
        burn_rate: 6,
        bad_ratio_threshold: 0.03,
        budget_pct_per_window: 5,
      },
    ],
    evidence: {
      observations: 142,
      confident: true,
      incident_count: 3,
      window_days: 14,
      score: 0.82,
    },
    adoptable: true,
    ...over,
  };
}

function rec(over: Partial<SLORecommendation> = {}): SLORecommendation {
  return {
    service: "checkout",
    generated_at: new Date().toISOString(),
    version: 3,
    summary: "Checkout is the revenue path; start with availability.",
    slis: [enrichedSLI()],
    ...over,
  };
}

// latencySLI is the latency contract: the objective is a COMPLIANCE RATIO
// measured against a discovered millisecond threshold, and `observed` is the
// measured compliance — not a millisecond figure.
function latencySLI(
  over: Partial<SLORecommendationSLI> = {},
): SLORecommendationSLI {
  return enrichedSLI({
    name: "Checkout latency",
    type: "latency",
    signal: "http_request_duration_seconds",
    objective: 0.99,
    objective_ratio: 0.99,
    threshold_ms: 250,
    observed: 0.9912,
    headroom_pp: 0.12,
    ...over,
  });
}

function respond(recs: SLORecommendation[]): SLORecommendationsResponse {
  return {
    org: "acme",
    count: recs.length,
    recommendations: recs,
    status: { enabled: true },
  };
}

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <ToastProvider>
        <MemoryRouter
          future={{ v7_startTransition: true, v7_relativeSplatPath: true }}
        >
          <SLORecommendationsPage />
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

async function firstBlock(): Promise<HTMLElement> {
  const blocks = await screen.findAllByTestId("slo-sli-block");
  return blocks[0];
}

describe("SLORecommendationsPage — enriched SLI block", () => {
  it("renders the objective human-first with the accounting window", async () => {
    vi.mocked(api.listSLORecommendations).mockResolvedValue(respond([rec()]));
    renderPage();
    const block = await firstBlock();
    expect(
      within(block).getByTestId("slo-sli-objective").textContent,
    ).toBe("99.5% over 30 days");
  });

  it("renders a latency objective as a compliance ratio against a threshold", async () => {
    vi.mocked(api.listSLORecommendations).mockResolvedValue(
      respond([rec({ slis: [latencySLI()] })]),
    );
    renderPage();
    const block = await firstBlock();
    expect(
      within(block).getByTestId("slo-sli-objective").textContent,
    ).toBe("99% of requests under 250 ms over 30 days");
  });

  it("renders a latency observation as compliance, never as milliseconds", async () => {
    vi.mocked(api.listSLORecommendations).mockResolvedValue(
      respond([rec({ slis: [latencySLI()] })]),
    );
    renderPage();
    const block = await firstBlock();
    const current = within(block).getByTestId("slo-sli-current");
    expect(current.textContent).toContain("now 99.12%");
    // Regression: `observed` used to be printed as "1 ms" for latency.
    expect(current.textContent).not.toContain("now 1 ms");
    expect(current.textContent).not.toContain("now 0 ms");
  });

  it("keeps the observed p99 as supporting evidence beside the compliance", async () => {
    vi.mocked(api.listSLORecommendations).mockResolvedValue(
      respond([rec({ slis: [latencySLI({ observed_p99_ms: 312 })] })]),
    );
    renderPage();
    const block = await firstBlock();
    expect(within(block).getByTestId("slo-sli-p99").textContent).toContain(
      "observed p99 312 ms",
    );
    // The p99 is evidence, not the objective.
    expect(
      within(block).getByTestId("slo-sli-objective").textContent,
    ).not.toContain("312");
  });

  it("shows the p99 evidence even when no compliance was measured yet", async () => {
    vi.mocked(api.listSLORecommendations).mockResolvedValue(
      respond([
        rec({
          slis: [
            latencySLI({
              observed: undefined,
              headroom_pp: undefined,
              observed_p99_ms: 312,
            }),
          ],
        }),
      ]),
    );
    renderPage();
    const block = await firstBlock();
    const current = within(block).getByTestId("slo-sli-current");
    expect(current.textContent).toContain("observed p99 312 ms");
    expect(current.textContent).not.toContain("now");
  });

  it("reads headroom against the compliance ratio for latency", async () => {
    vi.mocked(api.listSLORecommendations).mockResolvedValue(
      respond([
        rec({
          slis: [
            latencySLI({
              observed: 0.9862,
              headroom_pp: -0.38,
              breaching: true,
            }),
          ],
        }),
      ]),
    );
    renderPage();
    const block = await firstBlock();
    const current = within(block).getByTestId("slo-sli-current");
    expect(current.textContent).toContain("now 98.62%");
    expect(current.textContent).toContain("0.38pp below target");
    expect(within(block).getByTestId("slo-sli-breaching")).toBeTruthy();
  });

  it("shows current attainment with signed headroom", async () => {
    vi.mocked(api.listSLORecommendations).mockResolvedValue(respond([rec()]));
    renderPage();
    const block = await firstBlock();
    const current = within(block).getByTestId("slo-sli-current");
    expect(current.textContent).toContain("now 99.87%");
    expect(current.textContent).toContain("+0.37pp headroom");
  });

  it("flags a breaching objective and says how far below target it is", async () => {
    vi.mocked(api.listSLORecommendations).mockResolvedValue(
      respond([
        rec({
          slis: [
            enrichedSLI({ observed: 0.992, headroom_pp: -0.3, breaching: true }),
          ],
        }),
      ]),
    );
    renderPage();
    const block = await firstBlock();
    expect(within(block).getByTestId("slo-sli-breaching")).toBeTruthy();
    expect(within(block).getByTestId("slo-sli-current").textContent).toContain(
      "0.3pp below target",
    );
  });

  it("translates the error budget into human time plus consumption", async () => {
    vi.mocked(api.listSLORecommendations).mockResolvedValue(respond([rec()]));
    renderPage();
    const block = await firstBlock();
    const budget = within(block).getByTestId("slo-sli-budget");
    expect(budget.textContent).toContain("budget 3h 39m per 30 days");
    expect(budget.textContent).toContain("62% consumed");
  });

  it("spells out the burn-rate alerts that will page you", async () => {
    vi.mocked(api.listSLORecommendations).mockResolvedValue(
      respond([rec({ slis: [enrichedSLI({ adopted: true })] })]),
    );
    renderPage();
    const block = await firstBlock();
    const burn = within(block).getByTestId("slo-sli-burn");
    // Go duration strings are rendered human-first, never echoed as "1h0m0s".
    expect(burn.textContent).toContain(
      "Fast: >14.4× burn over 1h (bad ratio > 7.2%)",
    );
    expect(burn.textContent).toContain(
      "Slow: >6× burn over 6h (bad ratio > 3%)",
    );
    expect(burn.textContent).not.toContain("0m0s");
    expect(
      within(burn).getByTitle(
        "Confirmed over a 5m short window · burns 2% of the budget per window",
      ),
    ).toBeTruthy();
    expect(burn.textContent).toContain("Will page you");
    expect(
      within(burn).getByTestId("slo-sli-burn-note").textContent,
    ).toContain("raise an incident");
  });

  it("conditions the page on adoption while the SLI is only adoptable", async () => {
    vi.mocked(api.listSLORecommendations).mockResolvedValue(respond([rec()]));
    renderPage();
    const block = await firstBlock();
    const note = within(block).getByTestId("slo-sli-burn-note");
    expect(note.getAttribute("data-enforcement")).toBe("on-adopt");
    expect(note.textContent).toContain("once you adopt this objective");
  });

  it("never claims a page on an SLI the platform cannot enforce", async () => {
    vi.mocked(api.listSLORecommendations).mockResolvedValue(
      respond([
        rec({
          slis: [
            enrichedSLI({
              adoptable: false,
              not_adoptable_reason:
                "the error-budget evaluator measures availability only",
            }),
          ],
        }),
      ]),
    );
    renderPage();
    const block = await firstBlock();
    const burn = within(block).getByTestId("slo-sli-burn");
    const note = within(burn).getByTestId("slo-sli-burn-note");
    expect(note.getAttribute("data-enforcement")).toBe("manual");
    expect(burn.textContent).toContain("Alert thresholds");
    expect(burn.textContent).not.toContain("Will page you");
    expect(note.textContent).toContain("your own alerting");
    expect(note.textContent).not.toContain("raise an incident");
  });

  it("honours an explicit enforced=false flag on the burn alerts", async () => {
    const base = enrichedSLI();
    vi.mocked(api.listSLORecommendations).mockResolvedValue(
      respond([
        rec({
          slis: [
            enrichedSLI({
              adopted: true,
              burn_alerts: (base.burn_alerts ?? []).map((a) => ({
                ...a,
                enforced: false,
              })),
            }),
          ],
        }),
      ]),
    );
    renderPage();
    const block = await firstBlock();
    const note = within(block).getByTestId("slo-sli-burn-note");
    expect(note.getAttribute("data-enforcement")).toBe("manual");
    expect(note.textContent).toContain("your own alerting");
  });

  it("shows good/valid events and a copyable query", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", {
      value: { writeText },
      configurable: true,
    });
    vi.mocked(api.listSLORecommendations).mockResolvedValue(respond([rec()]));
    renderPage();
    const block = await firstBlock();
    const measure = within(block).getByTestId("slo-sli-measure");
    expect(measure.textContent).toContain(
      "requests that returned a non-5xx status",
    );
    expect(measure.textContent).toContain("all requests reaching the service");

    const query = within(block).getByTestId("slo-sli-query");
    fireEvent.click(within(block).getByTestId("slo-sli-copy"));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(query.textContent));
    expect(await screen.findByText("Query copied")).toBeTruthy();
  });

  it("prefers the evidence score over the model confidence and shows the backing", async () => {
    vi.mocked(api.listSLORecommendations).mockResolvedValue(respond([rec()]));
    renderPage();
    const block = await firstBlock();
    const conf = within(block).getByTestId("slo-sli-confidence");
    // evidence.score 0.82 wins over the legacy confidence 0.5.
    expect(conf.textContent).toContain("82%");
    expect(conf.getAttribute("title")).toBe(
      "142 observations · confident · 3 incidents/14d",
    );
  });

  it("keeps the rationale as a secondary line", async () => {
    vi.mocked(api.listSLORecommendations).mockResolvedValue(respond([rec()]));
    renderPage();
    const block = await firstBlock();
    expect(
      within(block).getByTestId("slo-sli-rationale").textContent,
    ).toContain("p99 error ratio held under 0.3% for two weeks");
  });
});

describe("SLORecommendationsPage — degrading against an older server", () => {
  it("renders only the header and rationale when no enrichment is present", async () => {
    vi.mocked(api.listSLORecommendations).mockResolvedValue(
      respond([rec({ slis: [legacySLI()] })]),
    );
    renderPage();
    const block = await firstBlock();

    expect(within(block).getByTestId("slo-sli-objective").textContent).toBe(
      "99.5% over 30 days",
    );
    expect(within(block).getByTestId("slo-sli-rationale")).toBeTruthy();
    // No em-dash noise: the rows the server can't back are simply absent.
    expect(within(block).queryByTestId("slo-sli-current")).toBeNull();
    expect(within(block).queryByTestId("slo-sli-budget")).toBeNull();
    expect(within(block).queryByTestId("slo-sli-burn")).toBeNull();
    expect(within(block).queryByTestId("slo-sli-measure")).toBeNull();
    expect(within(block).queryByTestId("slo-sli-copy")).toBeNull();
    // Adoption is unknown, not refused — neither a button nor a note.
    expect(within(block).queryByTestId("slo-adopt-btn")).toBeNull();
    expect(within(block).queryByTestId("slo-adopt-manual")).toBeNull();
    // The legacy confidence still renders.
    expect(within(block).getByTestId("slo-sli-confidence").textContent).toContain(
      "50%",
    );
  });

  it("still reads a latency SLI that only carries a millisecond target", async () => {
    vi.mocked(api.listSLORecommendations).mockResolvedValue(
      respond([
        rec({
          slis: [
            legacySLI({
              name: "Checkout latency",
              type: "latency",
              signal: "http_request_duration_p99",
              objective: 250,
            }),
          ],
        }),
      ]),
    );
    renderPage();
    const block = await firstBlock();
    expect(within(block).getByTestId("slo-sli-objective").textContent).toBe(
      "under 250 ms over 30 days",
    );
    expect(within(block).queryByTestId("slo-sli-p99")).toBeNull();
    expect(within(block).queryByTestId("slo-sli-current")).toBeNull();
  });
});

describe("SLORecommendationsPage — adopt", () => {
  it("confirms, POSTs the adoption and shows the adopted state", async () => {
    vi.mocked(api.listSLORecommendations).mockResolvedValue(respond([rec()]));
    renderPage();
    const block = await firstBlock();

    fireEvent.click(within(block).getByTestId("slo-adopt-btn"));
    const dialog = await screen.findByRole("dialog");
    // The confirm explains what adopting actually does.
    expect(dialog.textContent).toContain("raise a burn-rate alert");
    fireEvent.click(within(dialog).getByText("Adopt objective"));

    await waitFor(() =>
      expect(api.adoptSLORecommendation).toHaveBeenCalledWith(
        "checkout",
        "Checkout availability",
      ),
    );
    expect(await screen.findByTestId("slo-adopted")).toBeTruthy();
    expect(screen.queryByTestId("slo-adopt-btn")).toBeNull();
  });

  it("shows a manual-adoption note instead of a dead button when not adoptable", async () => {
    vi.mocked(api.listSLORecommendations).mockResolvedValue(
      respond([rec({ slis: [enrichedSLI({ adoptable: false })] })]),
    );
    renderPage();
    const block = await firstBlock();
    expect(within(block).getByTestId("slo-adopt-manual").textContent).toContain(
      "Manual adoption",
    );
    expect(within(block).queryByTestId("slo-adopt-btn")).toBeNull();
  });

  it("shows the server's reason for refusing adoption", async () => {
    vi.mocked(api.listSLORecommendations).mockResolvedValue(
      respond([
        rec({
          slis: [
            enrichedSLI({
              adoptable: false,
              not_adoptable_reason:
                "saturation SLIs are advisory: the platform enforces availability only",
            }),
          ],
        }),
      ]),
    );
    renderPage();
    const block = await firstBlock();
    expect(within(block).getByTestId("slo-adopt-manual").textContent).toContain(
      "saturation SLIs are advisory: the platform enforces availability only",
    );
  });

  it("renders the adopted state straight from the server", async () => {
    vi.mocked(api.listSLORecommendations).mockResolvedValue(
      respond([rec({ slis: [enrichedSLI({ adopted: true })] })]),
    );
    renderPage();
    expect(await screen.findByTestId("slo-adopted")).toBeTruthy();
  });

  it("reads the adopted objective off the recommendation-level object", async () => {
    vi.mocked(api.listSLORecommendations).mockResolvedValue(
      respond([
        rec({
          adopted: {
            sli: "Checkout availability",
            sli_type: "availability",
            objective: 0.995,
            window_days: 30,
            adopted_at: new Date().toISOString(),
            by: "alice",
          },
        }),
      ]),
    );
    renderPage();
    expect(await screen.findByTestId("slo-adopted")).toBeTruthy();
    expect(
      (await screen.findByTestId("slo-adopted-detail")).textContent,
    ).toContain("by alice");
  });
});

describe("SLORecommendationsPage — revert to auto-derived", () => {
  const adoptedRec = () => rec({ slis: [enrichedSLI({ adopted: true })] });

  it("confirms and DELETEs the adoption", async () => {
    vi.mocked(api.listSLORecommendations).mockResolvedValue(
      respond([adoptedRec()]),
    );
    renderPage();
    fireEvent.click(await screen.findByTestId("slo-unadopt-btn"));
    const dialog = await screen.findByRole("dialog");
    expect(dialog.textContent).toContain("derives from observed attainment");
    fireEvent.click(within(dialog).getByText("Revert objective"));

    await waitFor(() =>
      expect(api.unadoptSLORecommendation).toHaveBeenCalledWith("checkout"),
    );
  });

  it("surfaces the server's message when there is nothing to revert", async () => {
    vi.mocked(api.listSLORecommendations).mockResolvedValue(
      respond([adoptedRec()]),
    );
    vi.mocked(api.unadoptSLORecommendation).mockRejectedValue(
      new ApiError(409, "no operator-adopted objective for this service", {
        code: "not_adopted",
      }),
    );
    renderPage();
    fireEvent.click(await screen.findByTestId("slo-unadopt-btn"));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByText("Revert objective"));

    expect(
      await screen.findByText(
        "no operator-adopted objective for this service",
      ),
    ).toBeTruthy();
  });

  it("surfaces a 422 cannot_revert instead of a generic failure", async () => {
    vi.mocked(api.listSLORecommendations).mockResolvedValue(
      respond([adoptedRec()]),
    );
    vi.mocked(api.unadoptSLORecommendation).mockRejectedValue(
      new ApiError(
        422,
        "cannot revert: no observed attainment is recorded to re-derive an objective from",
        { code: "cannot_revert" },
      ),
    );
    renderPage();
    fireEvent.click(await screen.findByTestId("slo-unadopt-btn"));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByText("Revert objective"));

    expect(
      await screen.findByText(/no observed attainment is recorded/),
    ).toBeTruthy();
  });

  it("offers no revert action on an SLI that was never adopted", async () => {
    vi.mocked(api.listSLORecommendations).mockResolvedValue(respond([rec()]));
    renderPage();
    const block = await firstBlock();
    expect(within(block).getByTestId("slo-adopt-btn")).toBeTruthy();
    expect(within(block).queryByTestId("slo-unadopt-btn")).toBeNull();
  });
});

// Availability and latency are two INDEPENDENT objectives on one service, so
// every adopt/revert action has to name the one it means.
describe("SLORecommendationsPage — the two independent objectives", () => {
  it("reverts a latency objective with type=latency", async () => {
    vi.mocked(api.listSLORecommendations).mockResolvedValue(
      respond([rec({ slis: [latencySLI({ adopted: true })] })]),
    );
    renderPage();
    fireEvent.click(await screen.findByTestId("slo-unadopt-btn"));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByText("Revert objective"));

    await waitFor(() =>
      expect(api.unadoptSLORecommendation).toHaveBeenCalledWith(
        "checkout",
        "latency",
      ),
    );
  });

  it("adopts a latency SLI and reports the discovered threshold honestly", async () => {
    vi.mocked(api.listSLORecommendations).mockResolvedValue(
      respond([rec({ slis: [latencySLI()] })]),
    );
    vi.mocked(api.adoptSLORecommendation).mockResolvedValue({
      ok: true,
      adjusted: {
        threshold_ms: {
          from: 300,
          to: 250,
          reason: "moved to the histogram bucket boundary",
        },
      },
    });
    renderPage();
    const block = await firstBlock();
    fireEvent.click(within(block).getByTestId("slo-adopt-btn"));
    const dialog = await screen.findByRole("dialog");
    expect(dialog.textContent).toContain(
      "99% of requests under 250 ms over 30 days",
    );
    fireEvent.click(within(dialog).getByText("Adopt objective"));

    await waitFor(() =>
      expect(api.adoptSLORecommendation).toHaveBeenCalledWith(
        "checkout",
        "Checkout latency",
      ),
    );
    expect(
      await screen.findByText("Adopted at the discovered threshold 250 ms."),
    ).toBeTruthy();
  });

  it("shows both objectives adopted on one service, from their own slots", async () => {
    vi.mocked(api.listSLORecommendations).mockResolvedValue(
      respond([
        rec({
          slis: [enrichedSLI(), latencySLI()],
          adopted: {
            sli: "Checkout availability",
            sli_type: "availability",
            objective: 0.995,
            window_days: 30,
            adopted_at: new Date().toISOString(),
            by: "alice",
          },
          latency_adopted: {
            sli: "Checkout latency",
            sli_type: "latency",
            objective: 0.99,
            window_days: 30,
            threshold_ms: 250,
            adopted_at: new Date().toISOString(),
            by: "bob",
          },
        }),
      ]),
    );
    renderPage();
    const blocks = await screen.findAllByTestId("slo-sli-block");
    expect(blocks).toHaveLength(2);
    expect(within(blocks[0]).getByTestId("slo-adopted")).toBeTruthy();
    expect(
      within(blocks[0]).getByTestId("slo-adopted-detail").textContent,
    ).toContain("by alice");
    expect(within(blocks[1]).getByTestId("slo-adopted")).toBeTruthy();
    expect(
      within(blocks[1]).getByTestId("slo-adopted-detail").textContent,
    ).toContain("by bob");
  });

  it("never reads the availability slot as a latency adoption", async () => {
    vi.mocked(api.listSLORecommendations).mockResolvedValue(
      respond([
        rec({
          slis: [latencySLI()],
          adopted: {
            sli: "Checkout latency",
            sli_type: "availability",
            objective: 0.995,
            window_days: 30,
            adopted_at: new Date().toISOString(),
          },
        }),
      ]),
    );
    renderPage();
    const block = await firstBlock();
    expect(within(block).getByTestId("slo-adopt-btn")).toBeTruthy();
    expect(within(block).queryByTestId("slo-adopted")).toBeNull();
  });
});

// Re-discovery can move an adopted latency threshold on its own. The operator
// is told, quietly, rather than left comparing the number they adopted against
// a different one being enforced.
describe("SLORecommendationsPage — automatic threshold re-sync", () => {
  const latencyAdopted = (over: Partial<SLOAdoptedSLO> = {}) =>
    rec({
      slis: [latencySLI()],
      latency_adopted: {
        sli: "Checkout latency",
        sli_type: "latency",
        objective: 0.99,
        window_days: 30,
        threshold_ms: 500,
        adopted_at: new Date().toISOString(),
        ...over,
      },
    });

  it("notes the move when the server records a re-sync", async () => {
    vi.mocked(api.listSLORecommendations).mockResolvedValue(
      respond([
        latencyAdopted({
          threshold_resync: {
            from_ms: 250,
            to_ms: 500,
            at: "2026-08-16T09:00:00Z",
          },
        }),
      ]),
    );
    renderPage();
    const note = await screen.findByTestId("slo-threshold-resync");
    expect(note.textContent).toBe("threshold re-synced 250 ms → 500 ms");
    expect(note.getAttribute("title")).toBe(fmtAbs("2026-08-16T09:00:00Z"));
  });

  it("renders nothing against a server that doesn't record re-syncs", async () => {
    vi.mocked(api.listSLORecommendations).mockResolvedValue(
      respond([latencyAdopted()]),
    );
    renderPage();
    expect(await screen.findByTestId("slo-adopted")).toBeTruthy();
    expect(screen.queryByTestId("slo-threshold-resync")).toBeNull();
  });
});

describe("SLORecommendationsPage — ordering and preserved states", () => {
  it("puts the HIGHEST priority service first", async () => {
    vi.mocked(api.listSLORecommendations).mockResolvedValue(
      respond([
        rec({ service: "legacy-cron", priority: 0.05 }),
        rec({ service: "checkout", priority: 0.36 }),
        rec({ service: "payments-api", priority: 0.92 }),
        rec({ service: "search" }),
      ]),
    );
    renderPage();
    const cards = await screen.findAllByTestId("slo-service-card");
    expect(cards.map((c) => c.querySelector("h3")?.textContent)).toEqual([
      "payments-api",
      "checkout",
      "legacy-cron",
      "search",
    ]);
  });

  it("labels priority by rank and band, never as a raw score", async () => {
    vi.mocked(api.listSLORecommendations).mockResolvedValue(
      respond([
        rec({ service: "legacy-cron", priority: 0.05 }),
        rec({ service: "payments-api", priority: 0.92 }),
      ]),
    );
    renderPage();
    const cards = await screen.findAllByTestId("slo-service-card");
    expect(
      cards.map((c) => within(c).getByTestId("slo-priority").textContent),
    ).toEqual(["Adopt first", "Low priority"]);
    expect(screen.queryByText(/Priority 0\./)).toBeNull();
    expect(screen.queryByText(/Lower numbers/)).toBeNull();
  });

  it("still renders the AI-off banner", async () => {
    vi.mocked(api.listSLORecommendations).mockResolvedValue({
      ...respond([rec()]),
      status: { enabled: false, off_reason: "AI is off: no API key" },
    });
    renderPage();
    expect(await screen.findByTestId("slo-ai-off-banner")).toBeTruthy();
  });

  it("still renders the Enterprise-locked upsell on 403", async () => {
    vi.mocked(api.listSLORecommendations).mockRejectedValue(
      new ApiError(403, "unlicensed"),
    );
    renderPage();
    expect(
      await screen.findByText(
        "SLI/SLO auto-define is an Enterprise capability",
      ),
    ).toBeTruthy();
    expect(screen.queryByTestId("slo-service-card")).toBeNull();
  });
});

// The cadence control speaks Go durations to the API but human durations to
// the operator: "24h0m0s" is a wire value, "24h" is what a person reads.
describe("SLORecommendationsPage — cadence control", () => {
  function config(over: Partial<SLOAutodefineConfig> = {}): SLOAutodefineConfig {
    return {
      cadence: "24h0m0s",
      enabled: true,
      min_cadence: "24h0m0s",
      status: { enabled: true },
      ...over,
    };
  }

  beforeEach(() => {
    vi.mocked(api.listSLORecommendations).mockResolvedValue(respond([rec()]));
    vi.mocked(api.getSLOAutodefineConfig).mockResolvedValue(config());
    vi.mocked(api.setSLOAutodefineConfig).mockResolvedValue(
      config({ cadence: "48h0m0s" }),
    );
  });

  async function cadenceInput(): Promise<HTMLInputElement> {
    await screen.findByTestId("slo-cadence-control");
    return screen.getByLabelText("Cadence") as HTMLInputElement;
  }

  it("renders the minimum and the current cadence human-first", async () => {
    renderPage();
    const input = await cadenceInput();
    expect(input.value).toBe("24h");
    expect(screen.getByText(/Minimum 24h\./)).toBeTruthy();
  });

  it("keeps Save disabled while the untouched cadence is only reformatted", async () => {
    renderPage();
    await cadenceInput();
    const save = screen.getByRole("button", { name: "Save" }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
  });

  it("submits the operator's raw Go duration, not the formatted display", async () => {
    renderPage();
    const input = await cadenceInput();
    fireEvent.change(input, { target: { value: "48h" } });
    const save = screen.getByRole("button", { name: "Save" }) as HTMLButtonElement;
    expect(save.disabled).toBe(false);
    fireEvent.click(save);
    await waitFor(() =>
      expect(api.setSLOAutodefineConfig).toHaveBeenCalledWith("48h"),
    );
    expect(await screen.findByText("Cadence set to 48h")).toBeTruthy();
  });

  it("compounds read as spoken", async () => {
    vi.mocked(api.getSLOAutodefineConfig).mockResolvedValue(
      config({ cadence: "36h30m0s", min_cadence: "1h30m0s" }),
    );
    renderPage();
    const input = await cadenceInput();
    expect(input.value).toBe("36h 30m");
    expect(screen.getByText(/Minimum 1h 30m\./)).toBeTruthy();
  });

  // Editing the humanized compound in place keeps the space; Go's parser
  // rejects it, so the submit strips whitespace and the display stays readable.
  it("strips the display whitespace out of a compound cadence on submit", async () => {
    vi.mocked(api.getSLOAutodefineConfig).mockResolvedValue(
      config({ cadence: "24h0m0s", min_cadence: "1h0m0s" }),
    );
    renderPage();
    const input = await cadenceInput();
    fireEvent.change(input, { target: { value: "36h 30m" } });
    expect(input.value).toBe("36h 30m");
    const save = screen.getByRole("button", { name: "Save" }) as HTMLButtonElement;
    expect(save.disabled).toBe(false);
    fireEvent.click(save);
    await waitFor(() =>
      expect(api.setSLOAutodefineConfig).toHaveBeenCalledWith("36h30m"),
    );
  });

  it("keeps Save disabled when the edit only reformats the current value", async () => {
    vi.mocked(api.getSLOAutodefineConfig).mockResolvedValue(
      config({ cadence: "36h30m0s", min_cadence: "1h0m0s" }),
    );
    renderPage();
    const input = await cadenceInput();
    fireEvent.change(input, { target: { value: "36h 30m" } });
    const save = screen.getByRole("button", { name: "Save" }) as HTMLButtonElement;
    expect(save.disabled).toBe(true);
  });

  // A partial config response must degrade to a minimal control rather than
  // leaning on the error boundary above it.
  it("still renders the page when the config response is partial", async () => {
    vi.mocked(api.getSLOAutodefineConfig).mockResolvedValue(
      {} as SLOAutodefineConfig,
    );
    renderPage();
    expect(await screen.findByTestId("slo-cadence-control")).toBeTruthy();
    expect(await screen.findByTestId("slo-service-card")).toBeTruthy();
    const input = screen.getByLabelText("Cadence") as HTMLInputElement;
    expect(input.value).toBe("");
    expect(screen.queryByText(/Minimum/)).toBeNull();
    // The AI gate is unknown, so the toggle stays off and locked.
    const toggle = screen.getByTestId("slo-enable-toggle") as HTMLButtonElement;
    expect(toggle.getAttribute("aria-checked")).toBe("false");
    expect(toggle.disabled).toBe(true);
  });
});
