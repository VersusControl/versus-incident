// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, screen } from "@testing-library/react";
import { api, ApiError } from "@/lib/api";
import { CollectionCostCard } from "./CollectionCostCard";
import { renderAwsPage } from "./shared/renderAwsPage";
import { awsAccount } from "./awsFixtures";
import { costView, coverageView, NETWORK, PROD } from "./shared/estateFixtures";

// Collection cost states: the month's estimate is never shown as complete
// when a part of it was not read or not known, a failing read shows an
// error rather than a blank card, and wide or empty answers still draw.

vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, awsCollectionCost: vi.fn(), awsCoverage: vi.fn() } };
});

beforeEach(() => {
  vi.mocked(api.awsCoverage).mockResolvedValue(coverageView());
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

const accounts = [awsAccount({ account_id: NETWORK, display_name: "network" }), awsAccount({ account_id: PROD, display_name: "prod" })];
const card = () => renderAwsPage(<CollectionCostCard accounts={accounts} />, "/aws/settings");
const headline = () => document.querySelector('[data-number="Estimate a month"] dd')?.textContent ?? "";

describe("Collection cost estimate over parts not read", () => {
  it("keeps the exact estimate when every part is known", async () => {
    vi.mocked(api.awsCollectionCost).mockResolvedValue(costView());
    card();
    await screen.findByText("Calls 24 h");
    expect(headline()).toBe("~$46");
  });

  it("never shows the month's estimate as complete when the trail copies were not read", async () => {
    const base = costView();
    vi.mocked(api.awsCollectionCost).mockResolvedValue(
      costView({ estimate_month: { ...base.estimate_month, extra_trail_copies_usd: null, extra_trail_copies: null } }),
    );
    card();
    await screen.findByText("Calls 24 h");
    expect(document.querySelector('[data-estimate="trail"] dd')?.textContent).toBe("not read");
    expect(headline()).toMatch(/more than|at least|not all read|not read|not known/i);
  });

  it("never shows the month's estimate as complete when the GuardDuty part is not known", async () => {
    const base = costView();
    vi.mocked(api.awsCollectionCost).mockResolvedValue(
      costView({ estimate_month: { ...base.estimate_month, guardduty_usd: null, guardduty_known: false } }),
    );
    card();
    await screen.findByText("Calls 24 h");
    expect(document.querySelector('[data-estimate="guardduty"] dd')?.textContent).toBe("not known");
    expect(headline()).toMatch(/more than|at least|not all read|not read|not known/i);
  });

  it("carries the trail line's lower bound to the headline when some accounts were not read", async () => {
    const base = costView();
    vi.mocked(api.awsCollectionCost).mockResolvedValue(costView({ estimate_month: { ...base.estimate_month, unknown_accounts: [PROD] } }));
    card();
    await screen.findByText("Calls 24 h");
    expect(document.querySelector('[data-estimate="trail"] dd')?.textContent).toBe("more than $38");
    expect(headline()).toMatch(/more than|at least|not all read/i);
  });

  it("never paints a cost green", async () => {
    vi.mocked(api.awsCollectionCost).mockResolvedValue(costView());
    card();
    await screen.findByText("Calls 24 h");
    const root = document.querySelector("[data-collection-cost]") as HTMLElement;
    const green = [...root.querySelectorAll("*")].filter((el) => /(^|\s)(text|bg|border)-(sev-ok|ok|good|green|emerald)\b/.test(el.getAttribute("class") ?? ""));
    expect(green).toEqual([]);
  });
});

describe("Collection cost robustness", () => {
  it("offers a retry when the cost read fails", async () => {
    vi.mocked(api.awsCollectionCost).mockRejectedValue(new ApiError(500, "boom"));
    card();
    expect(await screen.findByText(/Couldn't load the collection cost/)).toBeTruthy();
  });

  it("still draws the budgets when the coverage read fails, with no reached line it cannot name", async () => {
    vi.mocked(api.awsCoverage).mockRejectedValue(new ApiError(500, "boom"));
    vi.mocked(api.awsCollectionCost).mockResolvedValue(costView());
    card();
    await screen.findByText("Calls 24 h");
    expect(document.querySelector('[data-budget="findings"]')?.getAttribute("data-reached")).toBe("true");
    expect(document.querySelector("[data-reached-line]")).toBeNull();
  });

  it("draws an empty answer: no table, no budgets, no optional sources", async () => {
    vi.mocked(api.awsCollectionCost).mockResolvedValue(
      costView({ calls_24h: 0, by_account_source: [], budgets: [], optional_off: [], stretched: [], stretched_accounts: 0, sources_capped: 0 }),
    );
    card();
    await screen.findByText("Calls 24 h");
    expect(document.querySelector("[data-by-source]")).toBeNull();
    expect(document.querySelectorAll("[data-budget]")).toHaveLength(0);
    expect(document.querySelector("[data-optional]")).toBeNull();
  });

  it("draws a budget with no limit at zero, never as a broken bar", async () => {
    vi.mocked(api.awsCollectionCost).mockResolvedValue(
      costView({ budgets: [{ source: "inventory", limit: 0, used: 12, unit: "calls", reached: false, coverage_sources: [] }] }),
    );
    card();
    await screen.findByText("Calls 24 h");
    const bar = document.querySelector('[data-budget="inventory"] span[aria-hidden] span') as HTMLElement;
    expect(bar.style.width).toBe("0%");
  });

  it("lists 40 accounts with long names in the calls table", async () => {
    const ids = Array.from({ length: 40 }, (_, i) => String(100000000000 + i));
    const long = "production-workloads-eu-west-1-payments-and-checkout-platform-account";
    vi.mocked(api.awsCollectionCost).mockResolvedValue(
      costView({ by_account_source: ids.map((account) => ({ account, source: "inventory", calls: 10, metered: 0 })) }),
    );
    renderAwsPage(<CollectionCostCard accounts={ids.map((id, i) => awsAccount({ account_id: id, display_name: i === 0 ? long : `a-${i}` }))} />, "/aws/settings");
    await screen.findByText("Calls 24 h");
    expect(document.querySelectorAll("[data-by-source] tbody tr")).toHaveLength(40);
    const name = screen.getByText(long);
    expect(name.className).toContain("truncate");
  });
});
