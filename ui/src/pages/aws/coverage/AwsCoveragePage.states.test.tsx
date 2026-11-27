// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, screen } from "@testing-library/react";
import { api, ApiError } from "@/lib/api";
import type { CoverageRow } from "@/lib/awsEstate";
import { AwsCoveragePage } from "./AwsCoveragePage";
import { renderAwsPage } from "../shared/renderAwsPage";
import { awsAccount, awsSettings } from "../awsFixtures";
import { coverageView, DATA, NETWORK, PLATFORM, PROD } from "../shared/estateFixtures";

// Coverage states: only Not read and Capped are gaps and carry a warning
// mark; Not set has no number while the server sends none; each summary
// number appears once; the page reads over the scope the URL names and
// survives empty, failing and very wide answers.

vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, awsCoverage: vi.fn(), awsSettings: vi.fn(), recheckAwsAccount: vi.fn() } };
});

vi.mock("@/components/TopBar", () => ({
  TopBar: ({ title }: { title: string }) => <h1>{title}</h1>,
}));

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
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

const page = (path = "/aws/coverage") => renderAwsPage(<AwsCoveragePage />, path, "/aws/coverage");

const GREEN = /(^|\s)(text|bg|border|fill|stroke|ring)-(sev-ok|ok|good|green|emerald|success)\b/;

describe("Coverage gaps", () => {
  it("marks only the Not read and Capped sections as gaps; Off in AWS and the answers carry no warning mark", async () => {
    vi.mocked(api.awsCoverage).mockResolvedValue(coverageView());
    page();
    await screen.findByText("Off in AWS", { selector: "h2" });
    const marked = [...document.querySelectorAll("section[data-section]")].filter((s) => s.querySelector("h2 svg.text-sev-warn")).map((s) => s.getAttribute("data-section"));
    expect(marked).toEqual(["not-read", "capped"]);
  });

  it("says nothing is missing only when there is no Not read or Capped row, and never in green", async () => {
    vi.mocked(api.awsCoverage).mockResolvedValue(coverageView({ not_read: [], capped: [], off_in_aws: [] }));
    page();
    const done = await screen.findByText("Every source was read");
    expect(GREEN.test(done.className)).toBe(false);
    expect(document.querySelector('[data-section="not-read"]')).toBeNull();
    expect(document.querySelector('[data-section="capped"]')).toBeNull();
  });

  it.each([
    ["null", null],
    ["left out", undefined],
    ["zero", 0],
  ])("shows no Not set number when the server sends it %s", async (_name, notSet) => {
    const view = coverageView({ not_gaps: { not_set: notSet, services_not_offered: 7, regions_not_enabled: 16 } });
    if (notSet === undefined) delete view.not_gaps.not_set;
    vi.mocked(api.awsCoverage).mockResolvedValue(view);
    page();
    const folded = await screen.findByText(/Not gaps/);
    expect(folded.textContent).toBe("Not gaps (2)");
    const section = document.querySelector('[data-section="not-gaps"]') as HTMLElement;
    expect(section.textContent).not.toMatch(/not set/i);
    fireEvent.click(folded);
    expect(section.textContent).not.toMatch(/not set/i);
    expect(section.querySelectorAll("li")).toHaveLength(2);
  });
});

describe("Coverage, each number once", () => {
  it("shows each summary number in the summary card only, with no count in the section headings", async () => {
    vi.mocked(api.awsCoverage).mockResolvedValue(
      coverageView({ summary: { accounts_read: 7, accounts_total: 9, not_read: 13, capped: 11, off_in_aws: 12 } }),
    );
    page();
    await screen.findByText("Off in AWS", { selector: "h2" });
    const main = document.querySelector("main") as HTMLElement;
    const outside = main.cloneNode(true) as HTMLElement;
    outside.querySelector("[data-summary-card]")?.remove();
    const card = main.querySelector("[data-summary-card]") as HTMLElement;
    const alone = (n: string) => new RegExp(`(?<![\\d,./:])${n.replace("/", "\\/")}(?![\\d,/:])`, "g");
    for (const n of ["7/9", "13", "11", "12"]) {
      expect(card.textContent?.match(alone(n))?.length ?? 0, `${n} in the summary card`).toBe(1);
      expect(outside.textContent?.match(alone(n)) ?? [], `${n} outside the summary card`).toEqual([]);
    }
    for (const h of main.querySelectorAll("section[data-section] h2")) {
      expect(h.textContent, "section heading").not.toMatch(/\d/);
    }
  });

  it("draws no warning line: Coverage is where the warning line's Details lead", async () => {
    vi.mocked(api.awsCoverage).mockResolvedValue(coverageView());
    page();
    await screen.findByText("Off in AWS", { selector: "h2" });
    expect(document.querySelector("[data-warning-line]")).toBeNull();
    expect(document.querySelector('a[href="/aws/coverage"]')).toBeNull();
  });
});

describe("Coverage scope and robustness", () => {
  it("reads coverage over the account and Region the URL names", async () => {
    vi.mocked(api.awsCoverage).mockResolvedValue(coverageView());
    page(`/aws/coverage?account=${PROD}&region=eu-west-1`);
    await screen.findByText("Off in AWS", { selector: "h2" });
    expect(api.awsCoverage).toHaveBeenCalledWith({ account: PROD, region: "eu-west-1" });
  });

  it("names a server that does not serve Coverage yet", async () => {
    vi.mocked(api.awsCoverage).mockRejectedValue(new ApiError(404, "not found"));
    page();
    expect(await screen.findByText(/does not serve AWS Coverage yet/)).toBeTruthy();
  });

  it("names the missing access on a refusal", async () => {
    vi.mocked(api.awsCoverage).mockRejectedValue(new ApiError(403, "forbidden"));
    page();
    expect(await screen.findByText("You don't have access to AWS Coverage")).toBeTruthy();
  });

  it("names the connector off", async () => {
    vi.mocked(api.awsCoverage).mockResolvedValue({ ...coverageView(), enabled: false });
    page();
    expect(await screen.findByText(/The AWS connector is off/)).toBeTruthy();
  });

  it("draws 40 accounts with long names as one row per gap", async () => {
    const ids = Array.from({ length: 40 }, (_, i) => String(100000000000 + i));
    const long = "production-workloads-eu-west-1-payments-and-checkout-platform-account";
    vi.mocked(api.awsSettings).mockResolvedValue(
      awsSettings({ accounts: ids.map((id, i) => awsAccount({ account_id: id, display_name: i === 0 ? long : `account-${i}` })) }),
    );
    const denied: CoverageRow = { source: "lambda", operation: "ListFunctionUrlConfigs", outcome: "not_read", reason: "denied", accounts: ids, fix: ["lambda:ListFunctionUrlConfigs"] };
    const oneAccount: CoverageRow = { source: "account", outcome: "not_read", reason: "denied", detail: "credentials_invalid", accounts: [ids[0]] };
    vi.mocked(api.awsCoverage).mockResolvedValue(
      coverageView({
        summary: { accounts_read: 39, accounts_total: 40, not_read: 2, capped: 1, off_in_aws: 0 },
        not_read: [denied, oneAccount],
        capped: [{ source: "inspector2", outcome: "capped", reason: "capped", accounts: ids, seen: 3000 }],
        off_in_aws: [],
      }),
    );
    page();
    await screen.findByText("Capped", { selector: "h2" });
    expect(document.querySelectorAll('[data-outcome="not_read"]')).toHaveLength(2);
    expect(document.querySelector('[data-row="lambda"] [data-line]')?.textContent).toBe("denied in all 40 accounts");
    const title = await screen.findByTitle(long);
    expect(title.className).toContain("truncate");
    // A capped row over many accounts names none of them in its title.
    expect(document.querySelector('[data-outcome="capped"] p')?.textContent).toBe("Inspector");
  });
});
