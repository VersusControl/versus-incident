// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { api, ApiError } from "@/lib/api";
import { AwsOverviewPage } from "./AwsOverviewPage";
import { renderAwsPage } from "../shared/renderAwsPage";
import { DATA, NETWORK, overviewView, PLATFORM, PROD } from "../shared/estateFixtures";
import { awsAccount, awsSettings } from "../awsFixtures";

vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, awsOverview: vi.fn(), awsSettings: vi.fn(), awsTopologySearch: vi.fn() } };
});

vi.mock("@/components/TopBar", () => ({
  TopBar: ({ title, subtitle }: { title: string; subtitle?: string }) => (
    <h1>
      {title} {subtitle}
    </h1>
  ),
}));

beforeEach(() => {
  vi.mocked(api.awsSettings).mockRejectedValue(new ApiError(404, "not found"));
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

const page = (path = "/aws/overview") => renderAwsPage(<AwsOverviewPage />, path, "/aws/overview");

function number(k: string): HTMLElement {
  const el = document.querySelector(`[data-number="${k}"]`);
  if (!el) throw new Error(`no number ${k}`);
  return el as HTMLElement;
}

describe("AWS Overview", () => {
  it("shows each number once in the summary card, each linking to the page that owns it", async () => {
    vi.mocked(api.awsOverview).mockResolvedValue(overviewView());
    page();
    await screen.findByText("Needs attention");
    expect(number("accounts").textContent).toContain("4/4");
    expect(number("act_now").querySelector("a")?.getAttribute("href")).toBe("/aws/issues?tier=act_now");
    expect(number("worth_a_look").querySelector("a")?.getAttribute("href")).toBe("/aws/issues?tier=worth_a_look");
    expect(number("resources").textContent).toContain("3,000");
    expect(number("resources").querySelector("[data-count-prefix]")).toBeNull();
    expect(number("grouped").textContent).toContain("62%");
    expect(number("people_changes").textContent).toContain("4");
    // The act-now count is not repeated in a heading, chip or sentence.
    const main = document.querySelector("main") as HTMLElement;
    expect(main.textContent?.match(/\b3 act now\b/i)).toBeNull();
  });

  it("puts the one warning line in the summary card, with Details opening coverage", async () => {
    vi.mocked(api.awsOverview).mockResolvedValue(overviewView());
    page();
    await screen.findByText("Needs attention");
    const lines = document.querySelectorAll("[data-warning-line]");
    expect(lines).toHaveLength(1);
    expect(lines[0].closest("[data-summary-card]")).not.toBeNull();
    expect(lines[0].textContent).toContain("Data not complete: 3 sources not read");
    expect(within(lines[0] as HTMLElement).getByRole("link", { name: "Details ›" }).getAttribute("href")).toBe("/aws/coverage");
  });

  it("shows nothing about coverage when the data is complete", async () => {
    vi.mocked(api.awsOverview).mockResolvedValue(overviewView({ warning: { not_read: 0 } }));
    page();
    await screen.findByText("Needs attention");
    expect(document.querySelector("[data-warning-line]")).toBeNull();
  });

  it("lists at most five issues and three changes by people", async () => {
    const base = overviewView();
    vi.mocked(api.awsOverview).mockResolvedValue(
      overviewView({
        needs_attention: [...base.needs_attention, { ...base.needs_attention[0], id: "iss-6" }],
        people_changes: [...base.people_changes, { ...base.people_changes[0], id: "ch-4" }],
      }),
    );
    page();
    await screen.findByText("Needs attention");
    expect(document.querySelectorAll("[data-issue]")).toHaveLength(5);
    expect(document.querySelectorAll("[data-change]")).toHaveLength(3);
    expect(screen.getByText("sg-checkout").tagName).toBe("STRONG");
    // The issue start shows only when it is known.
    expect(document.querySelector('[data-issue="iss-2"]')?.textContent).not.toContain("since");
    expect(document.querySelector('[data-issue="iss-3"]')?.textContent).toContain("since");
  });

  it("shows a loading card first", () => {
    vi.mocked(api.awsOverview).mockReturnValue(new Promise(() => {}));
    page();
    expect(document.querySelector("[data-loading]")).not.toBeNull();
    expect(document.querySelector("[data-summary-card]")).toBeNull();
  });

  it("offers to connect an account when none is configured", async () => {
    vi.mocked(api.awsOverview).mockResolvedValue(
      overviewView({ summary: { ...overviewView().summary, accounts_total: 0, accounts_read: 0 }, accounts: [], needs_attention: [], people_changes: [] }),
    );
    page();
    expect(await screen.findByText("No AWS account is connected.")).toBeTruthy();
    expect(document.querySelector("[data-summary-card]")).toBeNull();
  });

  it("names the connector off rather than drawing empty numbers", async () => {
    vi.mocked(api.awsOverview).mockResolvedValue({ ...overviewView(), enabled: false });
    page();
    expect(await screen.findByText(/The AWS connector is off/)).toBeTruthy();
  });

  it("reads an account whose inventory was never read as not read, never as 0 resources", async () => {
    const base = overviewView();
    vi.mocked(api.awsOverview).mockResolvedValue(
      overviewView({
        summary: { ...base.summary, accounts_read: 3, resources_capped: true },
        accounts: base.accounts.map((a) =>
          a.id === DATA ? { ...a, read: "not_read", not_read_reason: "access denied", resources: null, open_issues: null } : a,
        ),
      }),
    );
    page();
    await screen.findByText("Needs attention");
    const row = document.querySelector(`[data-account="${DATA}"]`) as HTMLElement;
    expect(row.getAttribute("data-state")).toBe("not_read");
    expect(row.textContent).toContain("Not read: access denied");
    expect(row.textContent).not.toMatch(/\b0\b/);
    expect(number("resources").textContent).toContain("more than");
    expect(number("resources").textContent).toContain("not all read");
  });

  it("reads a partly read account's resources as more than N, not all read", async () => {
    const base = overviewView();
    vi.mocked(api.awsOverview).mockResolvedValue(
      overviewView({ accounts: base.accounts.map((a) => (a.id === PROD ? { ...a, read: "partial" } : a)) }),
    );
    page();
    await screen.findByText("Needs attention");
    const cell = document.querySelector(`[data-account="${PROD}"] [data-resources]`) as HTMLElement;
    expect(cell.textContent).toBe("more than 1,200not all read");
    // Two lines: the bound, then the words, so the cell never spills.
    expect(cell.querySelectorAll("span.block")).toHaveLength(2);
  });

  it("shows a dash, not zero, on every count the server did not read", async () => {
    const base = overviewView();
    vi.mocked(api.awsOverview).mockResolvedValue(
      overviewView({
        summary: { ...base.summary, accounts_read: 0, act_now: null, worth_a_look: null, resources: null, grouped_share: null, people_changes_24h: null },
        needs_attention: [],
        accounts: base.accounts.map((a) => ({ ...a, read: "not_read", resources: null, open_issues: null })),
        people_changes: [],
      }),
    );
    page();
    await screen.findByText("Needs attention");
    for (const k of ["act_now", "worth_a_look", "resources", "grouped", "people_changes"]) {
      expect(number(k).getAttribute("data-state"), k).toBe("not_read");
      expect(number(k).textContent, k).toContain("—");
      expect(number(k).querySelector("a"), k).toBeNull();
    }
    expect(screen.queryByText("Nothing needs attention")).toBeNull();
    expect(screen.getByText("Not assessed: no account was read")).toBeTruthy();
    expect(screen.getByText("Not read: the change history could not be read")).toBeTruthy();
  });

  it("reads capped counts as more than N, not all read", async () => {
    vi.mocked(api.awsOverview).mockResolvedValue(
      overviewView({ summary: { ...overviewView().summary, resources_capped: true, people_changes_capped: true } }),
    );
    page();
    await screen.findByText("Needs attention");
    for (const k of ["resources", "people_changes"]) {
      expect(number(k).getAttribute("data-state")).toBe("capped");
      expect(number(k).querySelector("[data-count-prefix]")?.textContent).toBe("more than");
      expect(number(k).querySelector("[data-count-suffix]")?.textContent).toBe("not all read");
    }
  });

  it("says nothing needs attention without colouring it as fine", async () => {
    vi.mocked(api.awsOverview).mockResolvedValue(overviewView({ needs_attention: [] }));
    page();
    const clear = await screen.findByText("Nothing needs attention");
    expect(clear.className).not.toMatch(/sev-ok|green/);
  });

  it("offers a retry when the read fails", async () => {
    vi.mocked(api.awsOverview).mockRejectedValue(new ApiError(500, "boom"));
    page();
    expect(await screen.findByText(/Couldn't load AWS Overview/)).toBeTruthy();
  });

  it("names the missing access on a refusal", async () => {
    vi.mocked(api.awsOverview).mockRejectedValue(new ApiError(403, "forbidden"));
    page();
    expect(await screen.findByText("You don't have access to AWS Overview")).toBeTruthy();
  });

  it("keeps long names in their row and lists 40 accounts", async () => {
    const long = "production-workloads-eu-west-1-payments-and-checkout-platform-account";
    vi.mocked(api.awsOverview).mockResolvedValue(
      overviewView({
        accounts: Array.from({ length: 40 }, (_, i) => ({
          id: String(100000000000 + i),
          name: i === 0 ? long : `account-${i}`,
          role: i === 0 ? ("hub" as const) : ("spoke" as const),
          read: "read" as const,
          resources: 10 * i,
          resources_capped: false,
          open_issues: i % 3,
        })),
        other_accounts: { count: 12, unknown: 5 },
      }),
    );
    page();
    await screen.findByText("Needs attention");
    expect(document.querySelectorAll("[data-account]")).toHaveLength(40);
    const name = screen.getByText(long);
    // A long name wraps rather than cutting the row's badge or counts.
    expect(name.className).toContain("break-words");
    expect(name.getAttribute("title")).toBe(`${long} (100000000000)`);
    expect(document.querySelector("[data-other-accounts]")?.textContent).toContain("Other accounts: 12 (5 unknown)");
  });

  it("names accounts in the issue rows by their account name", async () => {
    vi.mocked(api.awsOverview).mockResolvedValue(overviewView());
    page();
    await screen.findByText("Needs attention");
    const row = document.querySelector('[data-issue="iss-3"]') as HTMLElement;
    expect(row.textContent).toContain("prod · eu-west-1");
    expect(row.textContent).not.toContain(PROD);
    expect(document.querySelector(`[data-account="${NETWORK}"]`)?.textContent).toContain("Hub");
  });
});

describe("AWS Overview scope and places", () => {
  beforeEach(() => {
    Element.prototype.scrollIntoView = vi.fn();
    vi.mocked(api.awsSettings).mockResolvedValue(
      awsSettings({
        accounts: [
          awsAccount({ account_id: NETWORK, display_name: "network" }),
          awsAccount({ account_id: PROD, display_name: "prod", regions: { selected: ["eu-west-1", "us-east-1"], enabled: 2, not_enabled: [] } }),
        ],
      }),
    );
  });

  it("reads the Overview over the account and Region the URL names", async () => {
    vi.mocked(api.awsOverview).mockResolvedValue(overviewView());
    page(`/aws/overview?account=${PROD}&region=eu-west-1`);
    await screen.findByText("Needs attention");
    expect(api.awsOverview).toHaveBeenCalledWith({ account: PROD, region: "eu-west-1" });
  });

  it("narrows to one account from the Accounts picker", async () => {
    vi.mocked(api.awsOverview).mockResolvedValue(overviewView());
    page();
    await screen.findByText("Needs attention");
    expect(api.awsOverview).toHaveBeenCalledWith({ account: undefined, region: undefined });
    await waitFor(() => expect(screen.getByRole("button", { name: "Accounts" }).textContent).toContain("All 2"));
    fireEvent.click(screen.getByRole("button", { name: "Accounts" }));
    fireEvent.mouseDown(await screen.findByRole("option", { name: /^prod/ }));
    await waitFor(() => expect(api.awsOverview).toHaveBeenLastCalledWith({ account: PROD, region: undefined }));
    fireEvent.click(screen.getByRole("button", { name: "Regions" }));
    fireEvent.mouseDown(await screen.findByRole("option", { name: "eu-west-1" }));
    await waitFor(() => expect(api.awsOverview).toHaveBeenLastCalledWith({ account: PROD, region: "eu-west-1" }));
  });

  it("shows the issue's place line and the account's role detail", async () => {
    vi.mocked(api.awsOverview).mockResolvedValue(overviewView());
    page();
    await screen.findByText("Needs attention");
    expect(document.querySelector('[data-issue="iss-1"]')?.textContent).toContain("platform · cluster ci-eks · since");
    expect(document.querySelector('[data-issue="iss-4"]')?.textContent).toContain("data · $1,840 a month");
    expect(document.querySelector(`[data-account="${NETWORK}"]`)?.textContent).toContain("Hub, transit gateway");
    expect(document.querySelector(`[data-account="${PLATFORM}"]`)?.textContent).toContain("Spoke");
  });

  it("searches resources over the scope", async () => {
    vi.mocked(api.awsOverview).mockResolvedValue(overviewView());
    vi.mocked(api.awsTopologySearch).mockResolvedValue({ enabled: true, items: [], capped: false });
    page(`/aws/overview?account=${PROD}`);
    await screen.findByText("Needs attention");
    fireEvent.change(screen.getByRole("searchbox", { name: "Search resources" }), { target: { value: "checkout" } });
    fireEvent.submit(screen.getByRole("search"));
    expect(await screen.findByText("No resource matches")).toBeTruthy();
    expect(api.awsTopologySearch).toHaveBeenCalledWith({ q: "checkout", account: PROD, region: undefined });
  });
});

describe("AWS Overview grouped share and not-read reasons", () => {
  it("shows a plain dash for the grouped share when no resource exists, apart from not read", async () => {
    const base = overviewView();
    vi.mocked(api.awsOverview).mockResolvedValue(overviewView({ summary: { ...base.summary, resources: 0, grouped_share: null } }));
    page();
    await screen.findByText("Needs attention");
    const grouped = document.querySelector('[data-number="grouped"]') as HTMLElement;
    expect(grouped.getAttribute("data-state")).toBe("read");
    expect(grouped.querySelector("dd")?.textContent).toBe("—");
    expect(grouped.querySelector("a")).toBeNull();
  });

  it("reads the grouped share as not read when the resource count is not known", async () => {
    const base = overviewView();
    vi.mocked(api.awsOverview).mockResolvedValue(overviewView({ summary: { ...base.summary, resources: null, grouped_share: null } }));
    page();
    await screen.findByText("Needs attention");
    const grouped = document.querySelector('[data-number="grouped"]') as HTMLElement;
    expect(grouped.getAttribute("data-state")).toBe("not_read");
    expect(grouped.querySelector("a")).toBeNull();
  });

  it.each([
    ["not_read_yet", "Not read yet"],
    ["denied", "Access denied"],
    ["throttled", "AWS throttled the read"],
    ["credentials_invalid", "Credentials not accepted"],
    ["not_checked", "Not checked"],
    ["something_new", "Not read (see Settings)"],
  ])("names the not-read reason %s in plain words", async (reason, words) => {
    const base = overviewView();
    vi.mocked(api.awsOverview).mockResolvedValue(
      overviewView({ accounts: base.accounts.map((a) => (a.id === DATA ? { ...a, read: "not_read", not_read_reason: reason, resources: null, open_issues: null } : a)) }),
    );
    page();
    await screen.findByText("Needs attention");
    expect(document.querySelector(`[data-account="${DATA}"] [data-not-read]`)?.textContent).toBe(words);
  });
});

describe("AWS Overview counts over accounts not all read", () => {
  it("reads capped tier counts and a capped share as more than N, not all read, in the grey style", async () => {
    const base = overviewView();
    vi.mocked(api.awsOverview).mockResolvedValue(
      overviewView({
        summary: { ...base.summary, act_now: 0, act_now_capped: true, worth_a_look: 2, worth_a_look_capped: true, grouped_share_capped: true },
      }),
    );
    page();
    await screen.findByText("Needs attention");
    const n = (k: string) => document.querySelector(`[data-number="${k}"]`) as HTMLElement;
    expect(n("act_now").getAttribute("data-state")).toBe("capped");
    expect(n("act_now").textContent).not.toMatch(/^Act now0$/);
    expect(n("act_now").querySelector("dd")?.className).toContain("text-ink-300");
    expect(n("worth_a_look").textContent).toContain("more than");
    expect(n("worth_a_look").textContent).toContain("not all read");
    expect(n("grouped").getAttribute("data-state")).toBe("capped");
    expect(n("grouped").textContent).toContain("62%");
    expect(n("grouped").textContent).toContain("not all read");
  });

  it("keeps an exact 0 when nothing is capped", async () => {
    const base = overviewView();
    vi.mocked(api.awsOverview).mockResolvedValue(overviewView({ summary: { ...base.summary, act_now: 0, worth_a_look: 0 } }));
    page();
    await screen.findByText("Needs attention");
    const act = document.querySelector('[data-number="act_now"]') as HTMLElement;
    expect(act.getAttribute("data-state")).toBe("read");
    expect(act.querySelector("dd")?.textContent).toBe("0");
  });

  it("shows an account in its first read with lower-bound counts and the reason", async () => {
    const base = overviewView();
    vi.mocked(api.awsOverview).mockResolvedValue(
      overviewView({ accounts: base.accounts.map((a) => (a.id === DATA ? { ...a, read: "partial", not_read_reason: "first_read_in_progress" } : a)) }),
    );
    page();
    await screen.findByText("Needs attention");
    const row = document.querySelector(`[data-account="${DATA}"]`) as HTMLElement;
    expect(row.querySelector("[data-partial-reason]")?.textContent).toBe("First read in progress");
    expect(row.querySelector("[data-resources]")?.textContent).toBe("more than 900not all read");
  });

  it("counts the Regions the scope reads", async () => {
    vi.mocked(api.awsOverview).mockResolvedValue(overviewView({ regions: ["eu-west-1", "us-east-1", "ap-south-1"] }));
    page();
    await screen.findByText("Needs attention");
    expect(screen.getByRole("button", { name: "Regions" }).textContent).toContain("3 regions");
  });
});

describe("AWS Overview at phone width", () => {
  it("lists every account at 390, the unread one included, with no cap on the card's height", async () => {
    const base = overviewView();
    vi.mocked(api.awsOverview).mockResolvedValue(
      overviewView({
        accounts: [
          ...base.accounts,
          { id: "123400000005", name: "sandbox-shared-services-experiments-eu", role: "", read: "not_read", not_read_reason: "assume_role_failed", resources: null, resources_capped: false, open_issues: null },
        ],
      }),
    );
    page();
    await screen.findByText("Needs attention");
    expect(document.querySelectorAll("[data-account]")).toHaveLength(5);
    // Below the sm breakpoint (390 px) nothing caps the list: the height cap and inner scroll apply from sm up only.
    const scroll = document.querySelector("[data-accounts-scroll]") as HTMLElement;
    const classes = scroll.className.split(/\s+/);
    expect(classes.filter((c) => c.startsWith("max-h-") || c === "overflow-auto")).toEqual([]);
    expect(classes).toContain("sm:max-h-[26rem]");
    expect(document.querySelector('[data-account="123400000005"]')?.textContent).toContain("Read role could not be assumed");
  });

  it("keeps 'not all read' on one line under its bound", async () => {
    vi.mocked(api.awsOverview).mockResolvedValue(overviewView({ accounts: overviewView().accounts.map((a) => ({ ...a, read: "partial" })) }));
    page();
    await screen.findByText("Needs attention");
    const suffix = document.querySelector(`[data-account="${PROD}"] [data-resources] span:last-child`) as HTMLElement;
    expect(suffix.textContent).toBe("not all read");
    expect(suffix.className).toContain("whitespace-nowrap");
  });
});

