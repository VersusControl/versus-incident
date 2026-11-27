// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, screen } from "@testing-library/react";
import { api, ApiError } from "@/lib/api";
import type { OverviewAccount, OverviewView } from "@/lib/awsEstate";
import { AwsOverviewPage } from "./AwsOverviewPage";
import { renderAwsPage } from "../shared/renderAwsPage";
import { DATA, NETWORK, overviewView, PLATFORM, PROD } from "../shared/estateFixtures";

// Overview states the page must never get wrong: a count the server left
// out or could not read is never a number or a clear answer, each number
// is shown once, and nothing unread is ever drawn green.

vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, awsOverview: vi.fn(), awsSettings: vi.fn(), awsTopologySearch: vi.fn() } };
});

vi.mock("@/components/TopBar", () => ({
  TopBar: ({ title }: { title: string }) => <h1>{title}</h1>,
}));

beforeEach(() => {
  vi.mocked(api.awsSettings).mockRejectedValue(new ApiError(404, "not found"));
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

const page = () => renderAwsPage(<AwsOverviewPage />, "/aws/overview", "/aws/overview");

function number(k: string): HTMLElement {
  const el = document.querySelector(`[data-number="${k}"]`);
  if (!el) throw new Error(`no number ${k}`);
  return el as HTMLElement;
}

/** Any class that paints an element green or "ok". */
const GREEN = /(^|\s)(text|bg|border|fill|stroke|ring|from|to)-(sev-ok|ok|good|green|emerald|success)\b/;

function greenElements(root: Element): Element[] {
  return [root, ...root.querySelectorAll("*")].filter((el) => GREEN.test(el.getAttribute("class") ?? ""));
}

/** The server's JSON with fields left out, as a partly read estate can send it. */
function withOmitted<T extends object>(obj: T, ...keys: string[]): T {
  const out = { ...obj } as Record<string, unknown>;
  for (const k of keys) delete out[k];
  return out as T;
}

describe("AWS Overview, counts that were not read", () => {
  it("reads an account's open issues left out by the server as not read, and still draws the page", async () => {
    const base = overviewView();
    vi.mocked(api.awsOverview).mockResolvedValue(
      overviewView({
        accounts: base.accounts.map((a) => (a.id === PROD ? withOmitted<OverviewAccount>(a, "open_issues") : a)),
      }),
    );
    page();
    await screen.findByText("Needs attention");
    const row = document.querySelector(`[data-account="${PROD}"]`) as HTMLElement;
    const cells = row.querySelectorAll("td");
    const issues = cells[cells.length - 1];
    expect(issues.textContent).not.toMatch(/\d/);
    expect(issues.querySelector("a")).toBeNull();
  });

  it("reads an account's resources left out by the server as not read", async () => {
    const base = overviewView();
    vi.mocked(api.awsOverview).mockResolvedValue(
      overviewView({
        accounts: base.accounts.map((a) => (a.id === DATA ? withOmitted<OverviewAccount>(a, "resources") : a)),
      }),
    );
    page();
    await screen.findByText("Needs attention");
    const row = document.querySelector(`[data-account="${DATA}"]`) as HTMLElement;
    expect(row.textContent).toMatch(/not read/i);
    expect(row.textContent).not.toMatch(/\b0\b/);
  });

  it("reads a grouped share left out by the server as not read, with no link", async () => {
    const base = overviewView();
    vi.mocked(api.awsOverview).mockResolvedValue(
      overviewView({ summary: withOmitted(base.summary, "grouped_share") }),
    );
    page();
    await screen.findByText("Needs attention");
    expect(number("grouped").getAttribute("data-state")).toBe("not_read");
    expect(number("grouped").querySelector("dd")?.textContent).not.toMatch(/\d/);
    expect(number("grouped").querySelector("a")).toBeNull();
  });

  it.each([
    ["act now", { act_now: null, worth_a_look: 0 }],
    ["worth a look", { act_now: 0, worth_a_look: null }],
  ])("never says nothing needs attention while the %s count was not read", async (_label, counts) => {
    const base = overviewView();
    vi.mocked(api.awsOverview).mockResolvedValue(
      overviewView({ summary: { ...base.summary, ...counts }, needs_attention: [] }),
    );
    page();
    await screen.findByText("Needs attention");
    expect(screen.queryByText("Nothing needs attention")).toBeNull();
    const section = document.querySelector('[data-section="needs-attention"]') as HTMLElement;
    expect(section.querySelector("[data-clear]")).toBeNull();
    expect(section.querySelector("[data-not-read]")).not.toBeNull();
  });

  it("reads an omitted act-now count as not read in the summary", async () => {
    const base = overviewView();
    vi.mocked(api.awsOverview).mockResolvedValue(overviewView({ summary: withOmitted(base.summary, "act_now", "people_changes_24h") }));
    page();
    await screen.findByText("Needs attention");
    for (const k of ["act_now", "people_changes"]) {
      expect(number(k).getAttribute("data-state"), k).toBe("not_read");
      expect(number(k).querySelector("dd")?.textContent, k).not.toMatch(/\d/);
    }
  });
});

describe("AWS Overview, each number once", () => {
  /** Distinct values, so a repeat of any summary number elsewhere is visible. */
  function distinct(): OverviewView {
    const base = overviewView();
    return overviewView({
      summary: {
        accounts_read: 3,
        accounts_total: 5,
        act_now: 17,
        worth_a_look: 23,
        resources: 4321,
        resources_capped: false,
        grouped_share: 61,
        people_changes_24h: 29,
        people_changes_capped: false,
      },
      accounts: [
        { id: NETWORK, name: "network", role: "hub", read: "read", resources: 1001, resources_capped: false, open_issues: 2 },
        { id: PROD, name: "prod", role: "spoke", read: "read", resources: 1100, resources_capped: false, open_issues: 5 },
        { id: PLATFORM, name: "platform", role: "spoke", read: "read", resources: 1200, resources_capped: false, open_issues: 6 },
        { id: DATA, name: "data", role: "spoke", read: "not_read", not_read_reason: "access denied", resources: null, resources_capped: false, open_issues: null },
      ],
      other_accounts: { count: 8, unknown: 1 },
      warning: { not_read: 9 },
      needs_attention: base.needs_attention,
      people_changes: base.people_changes,
    });
  }

  it("shows each summary number once on the page and nowhere outside the summary card", async () => {
    vi.mocked(api.awsOverview).mockResolvedValue(distinct());
    page();
    await screen.findByText("Needs attention");
    const main = document.querySelector("main") as HTMLElement;
    const card = main.querySelector("[data-summary-card]") as HTMLElement;
    const outside = main.cloneNode(true) as HTMLElement;
    outside.querySelector("[data-summary-card]")?.remove();
    // A number standing alone: no digit, comma or slash on either side.
    const alone = (n: string) => new RegExp(`(?<![\\d,./:])${n.replace(/[/%]/g, "\\$&")}(?![\\d,/:])`);
    const shown: Array<[string, RegExp]> = [
      ["accounts", alone("3/5")],
      ["act_now", alone("17")],
      ["worth_a_look", alone("23")],
      ["resources", alone("4,321")],
      ["grouped", alone("61%")],
      ["people_changes", alone("29")],
    ];
    for (const [k, re] of shown) {
      expect(number(k).querySelector("dd")?.textContent, k).toMatch(re);
      const global = new RegExp(re.source, "g");
      expect(card.textContent?.match(global)?.length ?? 0, `${k} in the summary card`).toBe(1);
      expect(outside.textContent?.match(global) ?? [], `${k} outside the summary card`).toEqual([]);
    }
  });

  it("puts the warning and its count in the summary card only", async () => {
    vi.mocked(api.awsOverview).mockResolvedValue(distinct());
    page();
    await screen.findByText("Needs attention");
    const main = document.querySelector("main") as HTMLElement;
    expect(main.querySelectorAll("[data-warning-line]")).toHaveLength(1);
    expect(main.textContent?.match(/Data not complete/g)).toHaveLength(1);
    expect(main.textContent?.match(/\b9 sources\b/g)).toHaveLength(1);
  });
});

describe("AWS Overview, never green over unread data", () => {
  it("paints nothing green when no account was read", async () => {
    const base = overviewView();
    vi.mocked(api.awsOverview).mockResolvedValue(
      overviewView({
        summary: { ...base.summary, accounts_read: 0, act_now: null, worth_a_look: null, resources: null, grouped_share: null, people_changes_24h: null },
        needs_attention: [],
        accounts: base.accounts.map((a) => ({ ...a, read: "not_read", not_read_reason: "credentials not valid", resources: null, open_issues: null })),
        people_changes: [],
      }),
    );
    page();
    await screen.findByText("Needs attention");
    const main = document.querySelector("main") as HTMLElement;
    expect(greenElements(main)).toEqual([]);
    for (const row of main.querySelectorAll("[data-account]")) {
      expect(row.getAttribute("data-state")).toBe("not_read");
      const badge = row.querySelector("[data-not-read]") as HTMLElement;
      expect(badge.className).toContain("border-dashed");
      expect(badge.textContent).toBe("Not read: credentials not valid");
    }
  });

  it("paints nothing green on a fully read, clear estate either", async () => {
    vi.mocked(api.awsOverview).mockResolvedValue(
      overviewView({ warning: null, needs_attention: [], summary: { ...overviewView().summary, act_now: 0, worth_a_look: 0 } }),
    );
    page();
    await screen.findByText("Nothing needs attention");
    expect(greenElements(document.querySelector("main") as HTMLElement)).toEqual([]);
  });
});

describe("AWS Overview robustness", () => {
  it("draws the page from an empty but enabled answer", async () => {
    vi.mocked(api.awsOverview).mockResolvedValue(
      overviewView({ needs_attention: [], people_changes: [], accounts: [], other_accounts: { count: 0, unknown: 0 }, warning: null }),
    );
    page();
    await screen.findByText("Needs attention");
    expect(document.querySelector("[data-summary-card]")).not.toBeNull();
    expect(document.querySelectorAll("[data-account]")).toHaveLength(0);
    expect(document.querySelector("[data-other-accounts]")).toBeNull();
  });

  it("names a server that does not serve the Overview yet instead of a blank page", async () => {
    vi.mocked(api.awsOverview).mockRejectedValue(new ApiError(404, "not found"));
    page();
    expect(await screen.findByText(/does not serve AWS Overview yet/)).toBeTruthy();
  });

  it("offers a retry on a network failure that is not an API error", async () => {
    vi.mocked(api.awsOverview).mockRejectedValue(new TypeError("Failed to fetch"));
    page();
    expect(await screen.findByText(/Couldn't load AWS Overview/)).toBeTruthy();
  });

  it("keeps a very long issue title, place and change summary in their rows", async () => {
    const long = "x".repeat(400);
    const base = overviewView();
    vi.mocked(api.awsOverview).mockResolvedValue(
      overviewView({
        needs_attention: [{ ...base.needs_attention[0], title: long, where: long }],
        people_changes: [{ ...base.people_changes[0], summary: `${long} sg-checkout`, actor: long }],
      }),
    );
    page();
    await screen.findByText("Needs attention");
    const title = screen.getByTitle(long);
    expect(title.className).toContain("truncate");
    expect(document.querySelectorAll("[data-issue]")).toHaveLength(1);
    expect(document.querySelectorAll("[data-change]")).toHaveLength(1);
  });
});
