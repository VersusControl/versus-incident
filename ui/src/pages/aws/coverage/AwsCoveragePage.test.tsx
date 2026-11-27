// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { api, ApiError } from "@/lib/api";
import { AwsCoveragePage } from "./AwsCoveragePage";
import { renderAwsPage } from "../shared/renderAwsPage";
import { awsAccount, awsSettings } from "../awsFixtures";
import { coverageView, DATA, NETWORK, PLATFORM, PROD } from "../shared/estateFixtures";

vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, awsCoverage: vi.fn(), awsSettings: vi.fn(), recheckAwsAccount: vi.fn() } };
});

vi.mock("@/components/TopBar", () => ({
  TopBar: ({ title }: { title: string }) => <h1>{title}</h1>,
}));

vi.mock("@/lib/clipboard", () => ({ copyText: vi.fn(async () => true) }));

beforeEach(() => {
  vi.mocked(api.awsSettings).mockResolvedValue(
    awsSettings({
      accounts: [
        awsAccount({ account_id: NETWORK, display_name: "network" }),
        awsAccount({ account_id: PROD, display_name: "prod" }),
        awsAccount({ account_id: PLATFORM, display_name: "platform" }),
        awsAccount({ account_id: DATA, display_name: "data" }),
      ],
    }),
  );
  vi.mocked(api.recheckAwsAccount).mockResolvedValue(undefined);
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

const page = () => renderAwsPage(<AwsCoveragePage />, "/aws/coverage");

describe("AWS Coverage", () => {
  it("lists the gaps by outcome: only Not read and Capped carry a warning mark", async () => {
    vi.mocked(api.awsCoverage).mockResolvedValue(coverageView());
    page();
    await screen.findByText("Function URL auth");
    const notRead = document.querySelector('[data-section="not-read"]') as HTMLElement;
    const capped = document.querySelector('[data-section="capped"]') as HTMLElement;
    const off = document.querySelector('[data-section="off-in-aws"]') as HTMLElement;
    expect(notRead.querySelector("h2 svg")).not.toBeNull();
    expect(capped.querySelector("h2 svg")).not.toBeNull();
    expect(off.querySelector("h2 svg")).toBeNull();
    expect(notRead.querySelectorAll('[data-outcome="not_read"]')).toHaveLength(3);
    expect(capped.querySelectorAll('[data-outcome="capped"]')).toHaveLength(1);
    expect(off.querySelectorAll('[data-outcome="off_in_aws"]')).toHaveLength(4);
    // No coverage warning line on the coverage page itself.
    expect(document.querySelector("[data-warning-line]")).toBeNull();
  });

  it("counts a denial across four accounts as one row, with how much was read", async () => {
    vi.mocked(api.awsCoverage).mockResolvedValue(coverageView());
    page();
    const title = await screen.findByText("Function URL auth");
    const row = title.closest("li") as HTMLElement;
    expect(row.querySelector("[data-line]")?.textContent).toBe("28 of 35 read, denied in all 4 accounts");
    expect(row.querySelector("[data-hides]")?.textContent).toBe("Hides: whether those functions are public");
  });

  it("reads a capped row as more than N, not all read, with the budget link", async () => {
    vi.mocked(api.awsCoverage).mockResolvedValue(coverageView());
    page();
    const title = await screen.findByText("Inspector in platform");
    const row = title.closest("li") as HTMLElement;
    expect(row.textContent).toContain("More than 3,000, not all read, known exploited ones first");
    expect(within(row).getByRole("link", { name: "Budget in Settings ›" }).getAttribute("href")).toBe("/aws/settings#collection-cost");
  });

  it("names the check Off in AWS can't run, never as a warning", async () => {
    vi.mocked(api.awsCoverage).mockResolvedValue(coverageView());
    page();
    await screen.findByText("Function URL auth");
    const off = document.querySelector('[data-section="off-in-aws"]') as HTMLElement;
    expect(off.textContent).toContain("Can't run: image and package findings");
    expect(off.textContent).toContain("network and prod");
    expect(off.textContent).toContain("10 of 19, all 4 accounts");
    expect(off.className).not.toMatch(/sev-/);
  });

  it("folds the answers that are not gaps, with no Not set count while the server has none", async () => {
    vi.mocked(api.awsCoverage).mockResolvedValue(coverageView());
    page();
    await screen.findByText("Function URL auth");
    const fold = screen.getByRole("button", { name: /Not gaps \(2\)/ });
    expect(fold.getAttribute("aria-expanded")).toBe("false");
    expect(fold.textContent).toContain("7 services not offered, 16 Regions not turned on");
    // Not set is not counted yet: no number, no placeholder, no zero.
    expect(fold.textContent).not.toMatch(/not set|\b0\b/);
  });

  it("shows the fix and copies it", async () => {
    vi.mocked(api.awsCoverage).mockResolvedValue(coverageView());
    const { copyText } = await import("@/lib/clipboard");
    page();
    await screen.findByText("Function URL auth");
    fireEvent.click(screen.getByRole("button", { name: "Show the fix for Function URL auth" }));
    const fix = document.querySelector("[data-fix]") as HTMLElement;
    expect(fix.textContent).toContain('"Action": "lambda:ListFunctionUrlConfigs"');
    fireEvent.click(within(fix).getByRole("button", { name: /Copy fix/ }));
    await waitFor(() => expect(copyText).toHaveBeenCalledWith(expect.stringContaining("lambda:ListFunctionUrlConfigs")));
  });

  it("names an account that could not be read at all", async () => {
    vi.mocked(api.awsCoverage).mockResolvedValue(
      coverageView({
        summary: { ...coverageView().summary, accounts_read: 3 },
        not_read: [{ source: "account", outcome: "not_read", reason: "denied", detail: "assume_role_failed", accounts: [DATA] }],
      }),
    );
    page();
    const title = await screen.findByText("data");
    expect(title.closest("li")?.textContent).toContain("Read role could not be assumed");
    expect(document.querySelector('[data-number="accounts"]')?.textContent).toContain("3/4");
  });

  it("says every source was read when there is no gap", async () => {
    vi.mocked(api.awsCoverage).mockResolvedValue(
      coverageView({
        summary: { accounts_read: 4, accounts_total: 4, not_read: 0, capped: 0, off_in_aws: 0 },
        not_read: [],
        capped: [],
        off_in_aws: [],
        not_gaps: { services_not_offered: 0, regions_not_enabled: 0 },
      }),
    );
    page();
    expect(await screen.findByText("Every source was read")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Not gaps/ })).toBeNull();
  });

  it("reads every account again from Read again", async () => {
    vi.mocked(api.awsCoverage).mockResolvedValue(coverageView());
    page();
    await screen.findByText("Function URL auth");
    const button = screen.getByRole("button", { name: "Read again" });
    await waitFor(() => expect((button as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(button);
    await waitFor(() => expect(api.recheckAwsAccount).toHaveBeenCalledTimes(4));
  });

  it("shows loading, then the error with a retry", async () => {
    vi.mocked(api.awsCoverage).mockRejectedValue(new ApiError(500, "boom"));
    page();
    expect(document.querySelector(".sk")).not.toBeNull();
    expect(await screen.findByText(/Couldn't load AWS Coverage/)).toBeTruthy();
  });

  it("offers to connect an account when none is configured", async () => {
    vi.mocked(api.awsCoverage).mockResolvedValue(
      coverageView({ summary: { accounts_read: 0, accounts_total: 0, not_read: 0, capped: 0, off_in_aws: 0 }, not_read: [], capped: [], off_in_aws: [] }),
    );
    page();
    expect(await screen.findByText("No AWS account is connected.")).toBeTruthy();
  });
});
