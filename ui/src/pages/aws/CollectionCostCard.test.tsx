// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, screen, waitFor } from "@testing-library/react";
import { api, ApiError } from "@/lib/api";
import { CollectionCostCard } from "./CollectionCostCard";
import { renderAwsPage } from "./shared/renderAwsPage";
import { awsAccount } from "./awsFixtures";
import { costView, coverageView, NETWORK, PLATFORM, PROD } from "./shared/estateFixtures";

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

const accounts = [
  awsAccount({ account_id: NETWORK, display_name: "network" }),
  awsAccount({ account_id: PROD, display_name: "prod" }),
  awsAccount({ account_id: PLATFORM, display_name: "platform" }),
];
const card = () => renderAwsPage(<CollectionCostCard accounts={accounts} />, "/aws/settings");

describe("Collection cost card", () => {
  it("shows the calls, the month's estimate, capped sources and stretched accounts", async () => {
    vi.mocked(api.awsCollectionCost).mockResolvedValue(costView());
    card();
    await screen.findByText("Calls 24 h");
    const n = (k: string) => document.querySelector(`[data-number="${k}"] dd`)?.textContent;
    expect(n("Calls 24 h")).toBe("62,840");
    expect(n("Estimate a month")).toBe("~$46");
    expect(n("Sources capped")).toBe("1");
    expect(n("Stretched")).toBe("1");
    expect(document.querySelector('[data-estimate="trail"]')?.textContent).toContain("(1 found)");
  });

  it("lists calls by account name and source, with a total", async () => {
    vi.mocked(api.awsCollectionCost).mockResolvedValue(costView());
    card();
    await screen.findByText("Calls by source");
    const row = document.querySelector(`[data-account="${NETWORK}"]`) as HTMLElement;
    expect(row.textContent).toContain("network");
    expect(row.textContent).toContain("10,260");
    // No account called the credential report: no column.
    expect(document.querySelector("[data-by-source] thead")?.textContent).not.toContain("Credential report");
  });

  it("takes the stretched accounts from the server", async () => {
    vi.mocked(api.awsCollectionCost).mockResolvedValue({ ...costView(), stretched_accounts: 3 });
    card();
    await screen.findByText("Calls 24 h");
    expect(document.querySelector('[data-number="Stretched"] dd')?.textContent).toBe("3");
  });

  it("names what a reached budget left unread, from its Capped coverage row", async () => {
    vi.mocked(api.awsCollectionCost).mockResolvedValue(costView());
    card();
    const line = await waitFor(() => {
      const el = document.querySelector('[data-budget="findings"] [data-reached-line]');
      if (!el) throw new Error("not yet");
      return el;
    });
    expect(line.textContent).toBe("Inspector in platform: more than 3,000, not all read, known exploited ones first");
  });

  it("marks only the budget that was reached", async () => {
    vi.mocked(api.awsCollectionCost).mockResolvedValue(costView());
    card();
    await screen.findByText("Budget per cycle");
    expect(document.querySelector('[data-budget="findings"]')?.getAttribute("data-reached")).toBe("true");
    expect(document.querySelector('[data-budget="findings"]')?.textContent).toContain("50 of 50 pages");
    expect(document.querySelector('[data-budget="health"]')?.getAttribute("data-reached")).toBeNull();
  });

  it("says an unknown multiplier is not known, never $0", async () => {
    const v = costView();
    vi.mocked(api.awsCollectionCost).mockResolvedValue({
      ...v,
      estimate_month: { ...v.estimate_month, guardduty_usd: null, extra_trail_copies_usd: null, extra_trail_copies: null },
    });
    card();
    await screen.findByText("Calls 24 h");
    expect(document.querySelector('[data-estimate="guardduty"]')?.textContent).toContain("not known");
    expect(document.querySelector('[data-estimate="trail"]')?.textContent).toContain("not read");
  });

  it("stays off the page when the connector is off", async () => {
    vi.mocked(api.awsCollectionCost).mockResolvedValue({ ...costView(), enabled: false });
    card();
    // The loading card shows first; the answer removes the card entirely.
    await waitFor(() => expect(document.querySelector("[data-collection-cost]")).toBeNull());
    expect(api.awsCollectionCost).toHaveBeenCalled();
  });

  it("stays off the page when the server has no cost read", async () => {
    vi.mocked(api.awsCollectionCost).mockRejectedValue(new ApiError(404, "not found"));
    card();
    await waitFor(() => expect(document.querySelector("[data-collection-cost]")).toBeNull());
  });

  it("reads a lower-bound estimate as at least $N, with one qualifier", async () => {
    const v = costView();
    vi.mocked(api.awsCollectionCost).mockResolvedValue({ ...v, estimate_month: { ...v.estimate_month, extra_trail_copies_usd: null } });
    card();
    await screen.findByText("Calls 24 h");
    expect(document.querySelector('[data-number="Estimate a month"] dd')?.textContent).toBe("at least$7.70");
    expect(document.querySelector('[data-number="Estimate a month"] dd')?.textContent).not.toContain("~");
  });

  it("leaves out the second-trail line when no second trail was found", async () => {
    const v = costView();
    vi.mocked(api.awsCollectionCost).mockResolvedValue({ ...v, estimate_month: { ...v.estimate_month, extra_trail_copies: 0, extra_trail_copies_usd: 0 } });
    card();
    await screen.findByText("Calls 24 h");
    expect(document.querySelector('[data-estimate="trail"]')).toBeNull();
    expect(document.querySelector('[data-number="Estimate a month"] dd')?.textContent).toBe("~$7.70");
  });

  it("leaves out the second-trail line when none was found, even with some accounts' trails not read", async () => {
    const v = costView();
    vi.mocked(api.awsCollectionCost).mockResolvedValue({ ...v, estimate_month: { ...v.estimate_month, extra_trail_copies: 0, extra_trail_copies_usd: 0, unknown_accounts: [PROD] } });
    card();
    await screen.findByText("Calls 24 h");
    expect(document.querySelector('[data-estimate="trail"]')).toBeNull();
    expect(document.querySelector("[data-collection-cost]")?.textContent).not.toContain("$0.00");
    // The unread trails still make the headline a lower bound.
    expect(document.querySelector('[data-number="Estimate a month"] dd')?.textContent).toContain("at least");
  });
});
