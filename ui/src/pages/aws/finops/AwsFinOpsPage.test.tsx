// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { ToastProvider } from "@/components/Toast";
import { api, type FinOpsQuery } from "@/lib/api";
import { AwsFinOpsPage } from "./AwsFinOpsPage";
import { finopsReportFixture, finopsStates, type FinOpsStateFixture } from "../../../../../tests/ui/finops/fixtures";
import { IncreaseDetailPage } from "./IncreaseDetail";

// Every FinOps state renders from its fixture: one per design page, built in
// code by versus-incident/tests/ui/finops/fixtures.ts (named after the 09e slugs).
type StateFixture = FinOpsStateFixture;
const fixtures = finopsStates();

vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      finopsSummary: vi.fn(),
      finopsBridge: vi.fn(),
      finopsBreakdown: vi.fn(),
      finopsSavings: vi.fn(),
      finopsAccounts: vi.fn(),
      finopsIncrease: vi.fn(),
      finopsSetIncreaseStatus: vi.fn(),
      finopsReport: vi.fn(),
      finopsFreezeReport: vi.fn(),
      finopsDrillDown: vi.fn(),
      finopsDismissSaving: vi.fn(),
    },
  };
});

vi.mock("@/components/TopBar", () => ({
  TopBar: ({ title, subtitle, actions }: { title: string; subtitle?: string; actions?: React.ReactNode }) => (
    <header>
      <span>
        {title} {subtitle}
      </span>
      {actions}
    </header>
  ),
}));

// OSS gateway operator by default; a test can switch to an enterprise viewer.
const OPERATOR = { org: null, enterprise: false, role: null, isAdmin: false, hasSession: false, loading: false, session: {} };
const VIEWER = { org: "acme", enterprise: true, role: "viewer", isAdmin: false, hasSession: true, loading: false, session: {} };
const role = vi.hoisted(() => ({ value: {} as Record<string, unknown> }));

vi.mock("@/lib/useEffectiveRole", () => ({
  useEffectiveRole: () => role.value,
}));

function setViewport(narrow: boolean) {
  Object.defineProperty(window, "matchMedia", {
    value: vi.fn(() => ({ matches: narrow, addEventListener: vi.fn(), removeEventListener: vi.fn() })),
    configurable: true,
  });
}

function serveFixture(f: StateFixture) {
  vi.mocked(api.finopsSummary).mockResolvedValue(f.summary);
  vi.mocked(api.finopsBridge).mockResolvedValue(f.bridge);
  vi.mocked(api.finopsBreakdown).mockImplementation(async (q: FinOpsQuery = {}) => {
    return f.breakdown[q.group_by ?? "service"] ?? f.breakdown.service;
  });
  vi.mocked(api.finopsSavings).mockResolvedValue(f.savings);
  vi.mocked(api.finopsAccounts).mockImplementation(async () => {
    if (!f.accounts) throw new Error("no accounts view in fixture");
    return f.accounts;
  });
  vi.mocked(api.finopsIncrease).mockImplementation(async (id: string) => {
    const d = f.increases?.[id];
    if (!d) throw new Error(`no increase ${id}`);
    return d;
  });
}

function renderState(name: string, route?: string) {
  const f = fixtures.get(name);
  if (!f) throw new Error(`missing fixture ${name}`);
  return renderFixture(f, route);
}

function renderFixture(f: StateFixture, route?: string) {
  serveFixture(f);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <MemoryRouter initialEntries={[route ?? f.route]}>
          <Routes>
            <Route path="/aws/finops" element={<AwsFinOpsPage />} />
            <Route path="/aws/finops/increases/:id" element={<IncreaseDetailPage />} />
          </Routes>
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

/** Waits for the page to finish its first load (no skeletons left). */
async function settle() {
  await waitFor(() => expect(document.querySelector(".sk")).toBeNull());
}

const text = () => document.body.textContent ?? "";

afterEach(cleanup);
beforeEach(() => {
  vi.clearAllMocks();
  setViewport(false);
  role.value = OPERATOR;
});

describe("every state renders from its fixture", () => {
  const expectations: Record<string, { has: string[]; not?: string[] }> = {
    "09-finops": {
      has: ["up to $610 a month, before other savings", "or: Stop reporting-db outside working hours $90", "list price", "AWS estimate", "What moved the cost", "$43,520", "+$3,640", "+9.1%", "Up", "$44,900", "Over budget by $2,900", "Network traffic", "Spend by service", "Savings found", "$1,890/mo"],
    },
    "09e-01-cost-off": { has: ["What FinOps answers", "What it reads", "≤ $0.20 a day", "Copy policy", "Cost Explorer reads: off", "Turn on"], not: ["$0 ", "Export"] },
    "09e-02-account-not-readable": { has: ["2 of 3 accounts", "Staging: access denied", "Copy fix", "September, 2 accounts", "October, 2 accounts", "$38,140"] },
    "09e-03-refresh-failed": { has: ["Stale since Nov 1", "Not loaded", "$1,480"] },
    "09e-04-request-cap": { has: ["Request cap reached: forecast and coverage skipped", "Raise cap in Settings ›", "Skipped today", "November to date"] },
    "09e-05-no-team-tags": { has: ["Owner: no team tag", "Set up tags ›", "no tag"] },
    "09e-06-cost-down": { has: ["Down", "−$2,310", "−5.8%", "Smaller functions"], not: ["Over budget"] },
    "09e-07-new-account": { has: ["Not checked: old snapshots — not collected", "Not checked: discount coverage — AWS recommendations off", "No previous month", "7 days", "Estimate", "Needs a full previous month", "Production", "none yet", "Learning until Nov 8", "Daily cost"] },
    "09e-08-no-change-found": { has: ["No change found", "No collector", "Collectors ›", "Change history incomplete"] },
    "09e-10-credits-fees": { has: ["Includes $2,400 one-time", "One-time charge", "Credits applied", "−$500"] },
    "09e-11-bill-not-closed": { has: ["Not final", "$43,520"] },
    "09e-12-partial-day": { has: ["November to date", "Partial", "$4,440"] },
    "09e-13-cost-flat": { has: ["About the same", "+0.4%", "Ups and downs offset (+$2,230 / −$2,060)"] },
    "09e-14-same-tag": { has: ["Owner hidden: all cost is team=platform", "platform"] },
    "09e-15-forecast-unavailable": { has: ["No full October", "$44,100", "Estimate", "Compare from December (first full month)", "Daily cost"] },
    "09e-16-single-account": { has: ["Production", "us-east-1", "Global"], not: ["1 of 1", "Over budget"] },
    "09e-17-cost-explorer-unavailable": { has: ["Cost Explorer is not available for Production (111122223333)", "ce_unavailable", "No cost data", "Open console ›", "Copy request", "Connect account ›"], not: ["$0", "Savings found"] },
    "09e-18-single-region": { has: ["No team tag", "Spend by service", "instances $8,700, EBS $1,700, IPs $500", "us-east-1", "no tag"] },
    "09e-19-single-region-tagged": { has: ["Spend by team", "Find untagged ›", "Unallocated", "us-east-1"] },
    "09e-20-organization": { has: ["Overview", "Accounts", "48", "Cost only", "Not connected", "Connect ›", "Unused Savings Plan", "Over budget by $12,000", "Cost source: management", "Not checked: rightsizing — needs usage detail (Organization)"] },
    "09e-21-organization-accounts": { has: ["Connected", "Cost only", "New", "Closed", "billed until Nov 30", "Cost not readable", "6 accounts", "Search accounts or IDs", "Export CSV", "Cost source"] },
    "09b-finops-why": { has: ["Network traffic in Production went up $1,980 in October", "Started Oct 9", "Owner: checkout", "Likely", "Oct 9 14:02", "github-actions-prod", "nat-1a", "All of it", "~$1,520/mo", "Mark expected…", "Rule: team=checkout tag on nat-1a", "Sent to alerts, Oct 12", "Read exact detail from AWS ›", "Possible", "Route table rtb-prod-private-1a changed", "platform-terraform"], not: ["acknowledged"] },
    "09b-finops-why-tied": {
      has: ["Network traffic in Production went up $1,980 in October", "Several changes fit;", "turn on resource-level data in Cost Explorer to narrow it ›", "Route table rtb-prod-private-1a changed"],
      not: ["Likely"],
    },
    "09e-09-resource-data-off": { has: ["Resource data off", "Turn on ›", "NAT data processing", "USE1-NatGateway-Bytes"], not: ["All of it"] },
  };

  it("covers every fixture", () => {
    expect(Object.keys(expectations).sort()).toEqual([...fixtures.keys()].sort());
  });

  for (const [name, exp] of Object.entries(expectations)) {
    it(name, async () => {
      renderState(name);
      await waitFor(() => expect(text()).toContain(exp.has[0]));
      await settle();
      for (const s of exp.has) expect(text(), s).toContain(s);
      for (const s of exp.not ?? []) expect(text(), s).not.toContain(s);
      // Versus never runs anything in AWS from this page.
      expect(text()).not.toMatch(/\b(Approve|Run)\b/);
      expect(text()).not.toMatch(/NaN|undefined|null/);
    });
  }
});

describe("hero columns", () => {
  it("shows Owner only when tags name two or more owners", async () => {
    renderState("09-finops");
    await screen.findByRole("columnheader", { name: "Owner" });
    cleanup();
    for (const name of ["09e-05-no-team-tags", "09e-14-same-tag", "09e-18-single-region"]) {
      renderState(name);
      await screen.findByRole("columnheader", { name: "Service" });
      expect(screen.queryByRole("columnheader", { name: "Owner" }), name).toBeNull();
      cleanup();
    }
  });

  it("shows Account only in the organization view", async () => {
    renderState("09e-20-organization");
    await screen.findByRole("columnheader", { name: "Account" });
    cleanup();
    renderState("09-finops");
    await screen.findByRole("columnheader", { name: "Service" });
    expect(screen.queryByRole("columnheader", { name: "Account" })).toBeNull();
  });

  it("gives every bar a spoken label and a keyboard path to its detail", async () => {
    renderState("09-finops");
    const link = await screen.findByRole("link", { name: "Network traffic, checkout, up $1,980" });
    expect(link.getAttribute("href")).toBe("/aws/finops/increases/inc-nat");
    expect(screen.getAllByRole("img", { name: /September total, \$39,880/ }).length).toBeGreaterThan(0);
  });

  it("becomes a ranked list on a phone", async () => {
    setViewport(true);
    renderState("09-finops");
    const list = await screen.findByRole("list", { name: "What moved the cost" });
    expect(within(list).getAllByRole("listitem")).toHaveLength(9);
    expect(screen.queryByRole("table", { name: /one row per driver/ })).toBeNull();
  });
});

describe("spend by", () => {
  it("offers only qualifying options; Team locked with its reason", async () => {
    renderState("09e-05-no-team-tags");
    const group = await screen.findByRole("group", { name: "Spend by" });
    const buttons = within(group).getAllByRole("button");
    expect(buttons.map((b) => b.textContent?.trim())).toEqual(["Account", "Team"]);
    expect(within(group).getByRole("button", { name: /Team/ }).getAttribute("aria-disabled")).toBe("true");
  });

  it("removes Account with one account and opens on Region", async () => {
    renderState("09e-16-single-account");
    const group = await screen.findByRole("group", { name: "Spend by" });
    expect(within(group).queryByRole("button", { name: "Account" })).toBeNull();
    expect(within(group).getByRole("button", { name: "Region" }).getAttribute("aria-pressed")).toBe("true");
  });

  it("removes the card when nothing splits the cost", async () => {
    renderState("09e-18-single-region");
    await screen.findByRole("heading", { name: "Spend by service" });
    expect(screen.queryByRole("group", { name: "Spend by" })).toBeNull();
    expect(screen.queryByRole("heading", { name: /^Spend by( account| region| team)?$/ })).toBeNull();
  });

  it("titles the card with the only option", async () => {
    renderState("09e-19-single-region-tagged");
    await screen.findByRole("heading", { name: "Spend by team" });
    expect(screen.queryByRole("group", { name: "Spend by" })).toBeNull();
  });

  it("marks estimated team figures with ≈ and an ⓘ", async () => {
    renderState("09-finops");
    const group = await screen.findByRole("group", { name: "Spend by" });
    fireEvent.click(within(group).getByRole("button", { name: "Team" }));
    await screen.findByRole("button", { name: "Estimated split" });
    expect(text()).toContain("≈");
  });

  it("names a missing account where its row would be, never $0", async () => {
    renderState("09e-02-account-not-readable");
    await screen.findByText("Staging: access denied");
    expect(text()).toContain("access denied");
    expect(text()).not.toMatch(/Staging\$0/);
  });
});

describe("header", () => {
  it("switches Summary | Details with labels only and asks the server for the depth", async () => {
    renderState("09-finops");
    const depth = await screen.findByRole("group", { name: "Depth" });
    expect(within(depth).getAllByRole("button").map((b) => b.textContent)).toEqual(["Summary", "Details"]);
    const before = text().match(/\$[\d,]+/g);
    fireEvent.click(within(depth).getByRole("button", { name: "Details" }));
    await waitFor(() =>
      expect(vi.mocked(api.finopsSummary).mock.calls.some(([q]) => q?.depth === "details")).toBe(true),
    );
    await screen.findByText("USE1-NatGateway-Bytes");
    expect(text().match(/\$[\d,]+/g)).toEqual(before);
  });

  it("renders every export entry and locks Save while the bill is open", async () => {
    const user = userEvent.setup();
    renderState("09e-11-bill-not-closed");
    const button = await screen.findByRole("button", { name: "Export" });
    button.focus();
    await user.keyboard("{ArrowDown}");
    const menu = await screen.findByRole("menu", { name: "Export" });
    const items = within(menu).getAllByRole("menuitem");
    expect(items.map((i) => i.textContent)).toEqual([
      "PDFNot final",
      "CSVNot final",
      "Copy summary",
      "Save as monthly reportAvailable after Nov 5",
    ]);
    expect(items[3].getAttribute("aria-disabled")).toBe("true");
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("menu")).toBeNull();
    expect(document.activeElement).toBe(button);
  });

  it("calls the export handlers it is given", async () => {
    const f = fixtures.get("09-finops");
    if (!f) throw new Error("fixture");
    serveFixture(f);
    const pdf = vi.fn();
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <ToastProvider>
          <MemoryRouter initialEntries={["/aws/finops"]}>
            <AwsFinOpsPage handlers={{ pdf, csv: vi.fn(), copy_summary: vi.fn(), save_monthly_report: vi.fn() }} />
          </MemoryRouter>
        </ToastProvider>
      </QueryClientProvider>,
    );
    fireEvent.click(await screen.findByRole("button", { name: "Export" }));
    fireEvent.click(await screen.findByRole("menuitem", { name: "PDF" }));
    expect(pdf).toHaveBeenCalledOnce();
  });

  it("disables Export when there is no cost data", async () => {
    renderState("09e-17-cost-explorer-unavailable");
    expect((await screen.findByRole("button", { name: "Export" })).hasAttribute("disabled")).toBe(true);
  });

  it("shows the stale cause in the freshness popover", async () => {
    renderState("09e-03-refresh-failed");
    fireEvent.click(await screen.findByRole("button", { name: "Stale since Nov 1" }));
    expect(await screen.findByText(/AWS throttled the refresh/)).toBeTruthy();
  });
});

describe("export actions", () => {
  async function openMenu() {
    fireEvent.click(await screen.findByRole("button", { name: "Export" }));
    return screen.findByRole("menu", { name: "Export" });
  }

  it("hides Save as monthly report from viewers", async () => {
    role.value = VIEWER;
    renderState("09-finops");
    const menu = await openMenu();
    expect(within(menu).getAllByRole("menuitem").map((i) => i.textContent)).toEqual(["PDF", "CSV", "Copy summary"]);
  });

  it("locks Save while the month is still running", async () => {
    renderState("09e-12-partial-day");
    const menu = await openMenu();
    const save = within(menu).getByRole("menuitem", { name: /Save as monthly report/ });
    expect(save.getAttribute("aria-disabled")).toBe("true");
    expect(save.textContent).toContain("Available when the month ends");
    fireEvent.click(save);
    expect(api.finopsFreezeReport).not.toHaveBeenCalled();
  });

  it("saves a closed month", async () => {
    vi.mocked(api.finopsFreezeReport).mockResolvedValue({} as never);
    renderState("09-finops");
    const menu = await openMenu();
    fireEvent.click(within(menu).getByRole("menuitem", { name: "Save as monthly report" }));
    await waitFor(() => expect(api.finopsFreezeReport).toHaveBeenCalledWith("2026-10"));
    await screen.findByText("Monthly report saved");
  });

  it("words a save-report refusal in the toast, never the raw code", async () => {
    const { ApiError } = await import("@/lib/api");
    vi.mocked(api.finopsFreezeReport).mockRejectedValue(
      new ApiError(409, "bill_not_closed", { error: "bill_not_closed" }),
    );
    renderState("09-finops");
    const menu = await openMenu();
    fireEvent.click(within(menu).getByRole("menuitem", { name: "Save as monthly report" }));
    expect(await screen.findByText("The bill isn't final yet")).toBeTruthy();
    expect(screen.getByText("Save the report after the 5th.")).toBeTruthy();
    expect(text()).not.toContain("bill_not_closed");
  });

  it("downloads the CSV with the active filters", async () => {
    const hrefs: string[] = [];
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
      hrefs.push(this.getAttribute("href") ?? "");
    });
    renderState("09-finops", "/aws/finops?period=2026-10&accounts=111122223333&teams=checkout,platform");
    const menu = await openMenu();
    fireEvent.click(within(menu).getByRole("menuitem", { name: "CSV" }));
    expect(hrefs).toHaveLength(1);
    const url = new URL(hrefs[0], "http://x");
    expect(url.pathname).toBe("/api/admin/aws/finops/export.csv");
    expect(url.searchParams.get("period")).toBe("2026-10");
    expect(url.searchParams.get("accounts")).toBe("111122223333");
    expect(url.searchParams.get("teams")).toBe("checkout,platform");
    expect(url.searchParams.get("table")).toBe("breakdown");
    click.mockRestore();
  });

  it("opens the print route for the PDF", async () => {
    const open = vi.spyOn(window, "open").mockReturnValue(null);
    renderState("09-finops");
    const menu = await openMenu();
    fireEvent.click(within(menu).getByRole("menuitem", { name: "PDF" }));
    expect(open).toHaveBeenCalledWith("/aws/finops/report?period=2026-10&print=1", "_blank", "noopener");
    open.mockRestore();
  });

  it("copies the server's plain-text summary", async () => {
    const report = finopsReportFixture();
    vi.mocked(api.finopsReport).mockResolvedValue(report.report);
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    renderState("09-finops");
    const menu = await openMenu();
    fireEvent.click(within(menu).getByRole("menuitem", { name: "Copy summary" }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(report.report.copy_summary));
    expect(vi.mocked(api.finopsReport).mock.calls[0][0]).toMatchObject({ period: "2026-10" });
    await screen.findByText("Summary copied");
  });
});

describe("C6 fields", () => {
  it("fills the scope bar from scope_options and sends lists", async () => {
    // jsdom has no layout; the listbox scrolls its active option into view.
    Element.prototype.scrollIntoView = vi.fn();
    renderState("09-finops");
    fireEvent.click(await screen.findByRole("button", { name: "Accounts" }));
    const list = await screen.findByRole("listbox");
    expect(within(list).getAllByRole("option").map((o) => o.textContent)).toEqual(["All 3", "Production", "Shared services", "Staging"]);
    fireEvent.mouseDown(within(list).getByRole("option", { name: "Staging" }));
    await waitFor(() =>
      expect(vi.mocked(api.finopsSummary).mock.calls.some(([q]) => q?.accounts?.join(",") === "444455556666")).toBe(true),
    );
  });

  it("shows only one region as a label", async () => {
    renderState("09e-19-single-region-tagged");
    await screen.findByText("Spend by team");
    expect(screen.queryByRole("button", { name: "Regions" })).toBeNull();
    expect(text()).toContain("Region: us-east-1");
  });

  it("marks cost-only rows from account_kind", async () => {
    renderState("09e-20-organization");
    await screen.findByRole("columnheader", { name: "Account" });
    expect(screen.getAllByText("Cost only")).toHaveLength(3);
  });

  it("shows the request cost before the paid drill-down and sends the AWS service name", async () => {
    const f = fixtures.get("09b-finops-why");
    if (!f?.increases) throw new Error("fixture");
    const detailsView = structuredClone(f.increases["inc-nat"]);
    detailsView.increase.service_label = "Amazon Virtual Private Cloud";
    renderState("09b-finops-why");
    vi.mocked(api.finopsIncrease).mockImplementation(async (_id, q) => (q?.depth === "details" ? detailsView : f.increases!["inc-nat"]));
    vi.mocked(api.finopsDrillDown).mockResolvedValue({ drilled: true });
    fireEvent.click(await screen.findByRole("button", { name: "Read exact detail from AWS ›" }));
    expect(await screen.findByText("1 paid Cost Explorer request ($0.01), more for long results")).toBeTruthy();
    expect(api.finopsDrillDown).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Read for about $0.01" }));
    await waitFor(() =>
      expect(api.finopsDrillDown).toHaveBeenCalledWith({ account: "111122223333", service: "Amazon Virtual Private Cloud" }),
    );
  });

  it("words a drill-down refusal in the toast, never the raw code", async () => {
    const { ApiError } = await import("@/lib/api");
    renderState("09b-finops-why");
    vi.mocked(api.finopsDrillDown).mockRejectedValue(
      new ApiError(429, "paid_request_cap_reached", { error: "paid_request_cap_reached" }),
    );
    fireEvent.click(await screen.findByRole("button", { name: "Read exact detail from AWS ›" }));
    fireEvent.click(screen.getByRole("button", { name: "Read for about $0.01" }));
    expect(await screen.findByText("Daily AWS request limit reached")).toBeTruthy();
    expect(screen.getByText("Try again tomorrow.")).toBeTruthy();
    expect(text()).not.toContain("paid_request_cap_reached");
  });

  it("words a mark-expected refusal in the toast, never the raw code", async () => {
    const { ApiError } = await import("@/lib/api");
    vi.mocked(api.finopsSetIncreaseStatus).mockRejectedValue(
      new ApiError(400, "until_invalid", { error: "until_invalid" }),
    );
    renderState("09b-finops-why");
    fireEvent.click(await screen.findByRole("button", { name: "Mark expected…" }));
    const dialog = screen.getByRole("dialog");
    fireEvent.change(within(dialog).getByLabelText("Reason"), { target: { value: "Launch week" } });
    const until = new Date(Date.now() + 7 * 86_400_000).toISOString().slice(0, 10);
    fireEvent.change(within(dialog).getByLabelText("Until"), { target: { value: until } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Mark expected" }));
    expect(await screen.findByText("Pick a valid end date")).toBeTruthy();
    expect(text()).not.toContain("until_invalid");
  });

  it("gives a viewer no write or paid action on the increase", async () => {
    role.value = VIEWER;
    renderState("09b-finops-why");
    await screen.findByText("Sent to alerts, Oct 12");
    expect(screen.queryByRole("button", { name: /Read exact detail|Mark expected|Reopen/ })).toBeNull();
  });
});

describe("estimates and finality", () => {
  it("marks the total and change ≈ exactly when the filter rests on split cells", async () => {
    const f = structuredClone(fixtures.get("09-finops")!);
    f.summary.sync!.estimated = ["team"];
    fixtures.set("09-finops-estimated", { ...f, state: "09-finops-estimated" });
    renderState("09-finops-estimated");
    for (const name of ["October cost", "Change vs September"]) {
      const tile = await screen.findByRole("region", { name });
      expect(tile.textContent, name).toContain("≈");
      expect(within(tile).getByRole("button", { name: "Estimated split" })).toBeTruthy();
    }
    cleanup();
    renderState("09-finops");
    for (const name of ["October cost", "Change vs September"]) {
      const tile = await screen.findByRole("region", { name });
      expect(tile.textContent, name).not.toContain("≈");
    }
  });

  it("shows no Not final badge for the month to date", async () => {
    renderState("09e-12-partial-day");
    await screen.findByText("November to date");
    expect(text()).not.toContain("Not final");
  });

  it("names the cost source account when Cost Explorer is unavailable", async () => {
    const f = structuredClone(fixtures.get("09e-17-cost-explorer-unavailable")!);
    f.summary.cost_source = { account_id: "111122223333", account_name: "Production", kind: "account" };
    fixtures.set("09e-17-named", { ...f, state: "09e-17-named" });
    renderState("09e-17-named");
    expect(await screen.findByText("Cost Explorer is not available for Production (111122223333)")).toBeTruthy();
    cleanup();
    f.summary.cost_source = { account_id: "111122223333", kind: "account" };
    fixtures.set("09e-17-id", { ...f, state: "09e-17-id" });
    renderState("09e-17-id");
    expect(await screen.findByText("Cost Explorer is not available for account 111122223333")).toBeTruthy();
  });
});

describe("C4/C5 fields", () => {
  it("ranks suspects: the first highlighted, the rest as Possible rows", async () => {
    renderState("09b-finops-why");
    await screen.findByText("ECS service checkout-api updated to task definition :42");
    expect(screen.getAllByText("Possible")).toHaveLength(2);
    expect(screen.getByText("Likely").className).toContain("pill-accent");
  });

  it("says Change history incomplete, never No change found, when history has a gap", async () => {
    const f = structuredClone(fixtures.get("09b-finops-why")!);
    const inc = f.increases!["inc-nat"].increase;
    inc.suspect_change = { state: "history_incomplete", note: "Change history incomplete for that window" };
    inc.suspects = [];
    fixtures.set("09b-incomplete", { ...f, state: "09b-incomplete" });
    renderState("09b-incomplete");
    await screen.findByText("Change history incomplete");
    expect(screen.getByRole("button", { name: "About change history" })).toBeTruthy();
    expect(text()).not.toContain("No change found");
  });

  it("names savings gaps and never shows $0 when nothing could be checked", async () => {
    const f = structuredClone(fixtures.get("09-finops")!);
    f.savings = { ...f.savings, items: [], total: { monthly_micros: 0, count: 0, commitment_monthly_micros: 0, commitment_count: 0 }, gaps: [{ check: "unattached_volume", reason: "not_collected" }] };
    fixtures.set("09-no-savings", { ...f, state: "09-no-savings" });
    renderState("09-no-savings");
    const card = await screen.findByRole("region", { name: "Savings found" });
    expect(card.textContent).toContain("Not checked: unattached volumes — not collected");
    expect(card.textContent).not.toMatch(/\$0/);
    expect(card.textContent).not.toContain("None found");
  });
});

describe("tied suspects", () => {
  it("shows every tied change as Possible with none highlighted", async () => {
    renderState("09b-finops-why-tied");
    await screen.findByText("Several changes fit;", { exact: false });
    const pills = screen.getAllByText("Possible");
    expect(pills).toHaveLength(2);
    for (const p of pills) expect(p.className).not.toContain("pill-accent");
    expect(screen.getByRole("link", { name: /turn on resource-level data/ }).getAttribute("href")).toContain("cost-management");
  });
});

describe("savings totals", () => {
  it("takes the total from the API, never adding alternatives or the before-other-savings item", async () => {
    renderState("09-finops");
    const card = await screen.findByRole("region", { name: "Savings found" });
    // Items add to $2,500 and the alternative is $90; the API total excludes both.
    expect(card.textContent).toContain("$1,890/mo");
    expect(card.textContent).not.toContain("$2,500");
    expect(card.textContent).not.toContain("$2,590");
  });
});

describe("dismiss a saving", () => {
  // One open saving built here; the second response is the list after the
  // dismissal, with the item and its amount gone from the total.
  function oneSaving() {
    const f = fixtures.get("09-finops")!;
    const item = { ...(f.savings.items ?? [])[0], id: "sav-idle-db", title: "Stop idle staging-db", before_other_savings: false, alternatives: [] };
    const open = { ...f.savings, items: [item], total: { ...f.savings.total, monthly_micros: 120_000_000, count: 1 } };
    const after = { ...f.savings, items: [], gaps: [], total: { ...f.savings.total, monthly_micros: 0, count: 0, commitment_monthly_micros: 0, commitment_count: 0 } };
    return { f, item, open, after };
  }

  async function openDialog(user: ReturnType<typeof userEvent.setup>) {
    const card = await screen.findByRole("region", { name: "Savings found" });
    const button = within(card).getByRole("button", { name: "Dismiss Stop idle staging-db" });
    await user.click(button);
    return { card, button, dialog: await screen.findByRole("dialog", { name: "Dismiss saving" }) };
  }

  it("opens a labelled form that Escape closes, returning focus to Dismiss", async () => {
    const { f, open } = oneSaving();
    renderFixture({ ...f, savings: open });
    const user = userEvent.setup();
    const { button, dialog } = await openDialog(user);
    expect(within(dialog).getByLabelText("Reason")).toBeTruthy();
    expect(within(dialog).queryByRole("button", { name: /approve|run/i })).toBeNull();
    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(document.activeElement).toBe(button);
    expect(api.finopsDismissSaving).not.toHaveBeenCalled();
  });

  it("requires a reason before it sends", async () => {
    const { f, open } = oneSaving();
    renderFixture({ ...f, savings: open });
    const user = userEvent.setup();
    const { dialog } = await openDialog(user);
    const submit = within(dialog).getByRole("button", { name: "Dismiss" });
    expect((submit as HTMLButtonElement).disabled).toBe(true);
    await user.type(within(dialog).getByLabelText("Reason"), "   ");
    expect((submit as HTMLButtonElement).disabled).toBe(true);
    await user.type(within(dialog).getByLabelText("Reason"), "{Enter}");
    expect(api.finopsDismissSaving).not.toHaveBeenCalled();
  });

  it("sends the reason, then the item leaves the list and the total", async () => {
    const { f, open, after } = oneSaving();
    renderFixture({ ...f, savings: open });
    vi.mocked(api.finopsSavings).mockResolvedValueOnce(open).mockResolvedValue(after);
    vi.mocked(api.finopsDismissSaving).mockResolvedValue({ dismissed: true });
    const user = userEvent.setup();
    const { card, dialog } = await openDialog(user);
    expect(card.textContent).toContain("$120/mo");
    await user.type(within(dialog).getByLabelText("Reason"), "  Staging is being retired  ");
    await user.click(within(dialog).getByRole("button", { name: "Dismiss" }));
    expect(api.finopsDismissSaving).toHaveBeenCalledWith("sav-idle-db", "Staging is being retired");
    expect(await screen.findByText("Saving dismissed")).toBeTruthy();
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await waitFor(() => expect(card.textContent).not.toContain("Stop idle staging-db"));
    expect(card.textContent).not.toContain("$120/mo");
    expect(card.contains(document.activeElement)).toBe(true);
  });

  it("words a refused dismissal and keeps the form open", async () => {
    const { ApiError } = await import("@/lib/api");
    const { f, open } = oneSaving();
    renderFixture({ ...f, savings: open });
    vi.mocked(api.finopsDismissSaving).mockRejectedValue(new ApiError(503, "store_unavailable", { error: "store_unavailable" }));
    const user = userEvent.setup();
    const { dialog } = await openDialog(user);
    await user.type(within(dialog).getByLabelText("Reason"), "Not ours{Enter}");
    expect(await screen.findByText("Storage is unavailable")).toBeTruthy();
    expect(screen.getByText("Try again shortly.")).toBeTruthy();
    expect(text()).not.toContain("store_unavailable");
    expect(text()).not.toMatch(/AWS request failed/);
    expect(screen.getByRole("dialog", { name: "Dismiss saving" })).toBeTruthy();
  });

  it("offers no Dismiss to a viewer", async () => {
    role.value = VIEWER;
    const { f, open } = oneSaving();
    renderFixture({ ...f, savings: open });
    const card = await screen.findByRole("region", { name: "Savings found" });
    await within(card).findByText("Stop idle staging-db");
    expect(within(card).queryByRole("button", { name: /^Dismiss/ })).toBeNull();
  });
});

describe("resource detail too large", () => {
  const NOTE = "Resource detail is too large to read for this period, so resources are not named; the amounts are complete";

  // Neither "no resources" nor a zero amount may stand in for names the
  // server could not read.
  function expectNoEmptyOrZero(card: HTMLElement) {
    const t = card.textContent ?? "";
    expect(t).not.toMatch(/no resources?/i);
    expect(t).not.toMatch(/\$0(?![\d,.])/);
    expect(t).not.toMatch(/All of it/);
  }

  it("bridge without a resource note shows no marker", async () => {
    const f = fixtures.get("09-finops")!;
    renderFixture({ ...f, bridge: { ...f.bridge, resource_note: undefined } });
    await screen.findByText("What moved the cost");
    await settle();
    expect(text()).toContain("+$3,640");
    expect(text()).not.toContain("Resources not named");
    expect(screen.queryByRole("button", { name: "About resource data" })).toBeNull();
  });

  it("bridge with too-large resource detail shows a status word and the note behind an info tip", async () => {
    const f = fixtures.get("09-finops")!;
    renderFixture({ ...f, bridge: { ...f.bridge, resource_note: NOTE } });
    await screen.findByText("What moved the cost");
    await settle();
    const card = screen.getByRole("region", { name: "What moved the cost" });
    expect(within(card).getByText("Resources not named")).toBeTruthy();
    // The note is behind the info tip, not a sentence on the card.
    expect(text()).not.toContain(NOTE);
    // The drivers and amounts still render: the amounts are complete.
    expect(text()).toContain("+$3,640");
    expect(text()).toContain("Network traffic");
    expectNoEmptyOrZero(card);
    await userEvent.click(within(card).getByRole("button", { name: "About resource data" }));
    expect(within(card).getByRole("note").textContent).toBe(NOTE);
  });

  it("bridge marks only the row whose resource detail was too large", async () => {
    const f = fixtures.get("09-finops")!;
    const rows = (f.bridge.rows ?? []).map((r) => (r.id === "drv-nat" ? { ...r, resources: "too_large" } : r));
    renderFixture({ ...f, bridge: { ...f.bridge, rows, resource_note: NOTE } });
    await screen.findByText("What moved the cost");
    await settle();
    const card = screen.getByRole("region", { name: "What moved the cost" });
    const marked = within(card).getByText("Network traffic").closest("tr")!;
    const normal = within(card).getByText("More servers").closest("tr")!;
    expect(marked.textContent).toContain("Resources not named");
    expect(normal.textContent).not.toContain("Resources not named");
    // The row word carries no ⓘ of its own: the header keeps the single one.
    expect(within(marked).queryByRole("button", { name: "About resource data" })).toBeNull();
    expect(within(card).getAllByRole("button", { name: "About resource data" })).toHaveLength(1);
    for (const tr of [marked, normal]) {
      const t = tr.textContent ?? "";
      expect(t).not.toMatch(/no resources?/i);
      expect(t).not.toMatch(/\$0(?![\d,.])/);
    }
    expect(marked.textContent).toContain("+$1,980");
    expect(normal.textContent).toContain("+$1,240");
  });

  it("increase detail with too-large resource detail reads the same as the bridge", async () => {
    const f = fixtures.get("09b-finops-why")!;
    const d = f.increases!["inc-nat"];
    const tree = (d.cause_tree ?? []).map((n) => ({ ...n, resources: [] }));
    renderFixture({ ...f, increases: { "inc-nat": { ...d, resource_data: "too_large", resource_note: NOTE, cause_tree: tree } } });
    await screen.findByText(/Network traffic in Production went up/);
    await settle();
    const card = screen.getByRole("region", { name: "Where the increase came from" });
    expect(within(card).getAllByText("Resources not named").length).toBe(tree.length);
    expect(text()).not.toContain(NOTE);
    expect(text()).not.toContain("Resource data off");
    expectNoEmptyOrZero(card);
    await userEvent.click(within(card).getAllByRole("button", { name: "About resource data" })[0]);
    expect(within(card).getByRole("note").textContent).toBe(NOTE);
  });
});

describe("cost data off", () => {
  it("asks a viewer to find an admin instead of offering Turn on", async () => {
    role.value = VIEWER;
    renderState("09e-01-cost-off");
    await screen.findByText("Ask an admin to turn on Cost Explorer reads");
    expect(screen.queryByRole("link", { name: "Turn on" })).toBeNull();
  });
});

describe("organization", () => {
  it("opens the Accounts tab and expands a group", async () => {
    renderState("09e-21-organization-accounts");
    const tab = await screen.findByRole("tab", { name: /Accounts/ });
    expect(tab.getAttribute("aria-selected")).toBe("true");
    await screen.findByText("data-prod");
    expect(screen.queryByText("ml-prod")).toBeNull();
    const more = screen.getByRole("button", { name: "3 more ›" });
    fireEvent.click(more);
    await screen.findByText("ml-prod");
    expect(screen.getAllByRole("button", { name: "Estimated split" }).length).toBeGreaterThan(0);
  });

  it("filters cost-only accounts and searches", async () => {
    renderState("09e-21-organization-accounts");
    await screen.findByText("Shared services");
    fireEvent.click(screen.getByRole("button", { name: /Cost only/ }));
    expect(screen.queryByText("Shared services")).toBeNull();
    fireEvent.change(screen.getByRole("searchbox", { name: "Search accounts or IDs" }), { target: { value: "9999000" } });
    await waitFor(() => expect(screen.queryByText("data-prod")).toBeNull());
    expect(screen.getByText("Log Archive")).toBeTruthy();
  });
});

// Shapes seen on a real account: a month to date compared with the same days
// of last month, a phone-sized bridge with a change-history gap, and an
// increase that resolved before this month.
/** Makes every truncated line measure wider than its box, as on a phone. */
function measureAsCut() {
  vi.spyOn(HTMLElement.prototype, "scrollWidth", "get").mockReturnValue(300);
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(120);
}

describe("real account shapes", () => {
  /** Month to date (Nov 1–9) against Oct 1–9, named "October" by the server. */
  function monthToDate(): StateFixture {
    const f = structuredClone(fixtures.get("09-finops")!);
    const period = {
      key: "mtd", label: "November 2026, to date", short: "November",
      range: { start: "2026-11-01", end: "2026-11-09" }, calendar: { start: "2026-11-01", end: "2026-11-30" },
      open: true, complete_days: 9,
    };
    const compare = { key: "prev_period", label: "October", short: "October", range: { start: "2026-10-01", end: "2026-10-09" }, available: true };
    f.summary = { ...f.summary, period, compare };
    f.bridge = { ...f.bridge, period, compare };
    return f;
  }

  it("names a partial base by its days in the bridge, never as the month's total", async () => {
    renderFixture(monthToDate());
    await settle();
    expect(screen.getAllByRole("img", { name: /^Oct 1–9, \$39,880/ }).length).toBeGreaterThan(0);
    expect(text()).not.toContain("October total");
    expect(screen.getAllByRole("img", { name: /^November to date, / }).length).toBeGreaterThan(0);
  });

  it("names the year of a base from last year in the bridge and the Change note", async () => {
    const f = monthToDate();
    const compare = { key: "same_period_last_year", label: "November 2025", short: "November", range: { start: "2025-11-01", end: "2025-11-09" }, available: true };
    f.summary = { ...f.summary, compare };
    f.bridge = { ...f.bridge, compare };
    renderFixture(f);
    await settle();
    expect(screen.getAllByRole("img", { name: /^Nov 1–9, 2025, \$39,880/ }).length).toBeGreaterThan(0);
    await userEvent.click(screen.getByRole("button", { name: "About Change vs November" }));
    expect(screen.getByRole("note").textContent).toContain("Nov 1–9, 2025: $39,880");
  });

  it("names a partial base by its days in the Change note", async () => {
    renderFixture(monthToDate());
    await settle();
    await userEvent.click(screen.getByRole("button", { name: "About Change vs October" }));
    const note = screen.getByRole("note").textContent ?? "";
    expect(note).toContain("Oct 1–9: $39,880");
    expect(note).not.toContain("October total");
  });

  it("keeps the change-history note out of the row link on a phone, and opening it stays on the page", async () => {
    setViewport(true);
    const f = structuredClone(fixtures.get("09-finops")!);
    const rows = f.bridge.rows ?? [];
    rows[0].why = { state: "history_incomplete", text: "Change history incomplete" };
    renderFixture(f);
    const list = await screen.findByRole("list", { name: "What moved the cost" });
    const info = within(list).getByRole("button", { name: "About change history" });
    expect(info.closest("a")).toBeNull();
    for (const a of within(list).getAllByRole("link")) {
      expect(a.querySelector("button, a")).toBeNull();
    }
    info.focus();
    await userEvent.keyboard("{Enter}");
    expect(screen.getByRole("note").textContent).toContain("Some change history");
    expect(screen.queryByText(/went up/)).toBeNull();
    expect(screen.getByRole("list", { name: "What moved the cost" })).toBeTruthy();
  });

  it("shows no total over only a commitment saving, never a green $0", async () => {
    const f = structuredClone(fixtures.get("09-finops")!);
    const item = (f.savings.items ?? [])[0];
    f.savings = {
      ...f.savings,
      total: { monthly_micros: 0, count: 0, commitment_monthly_micros: 441_000_000, commitment_count: 1 },
      items: [{ ...item, id: "sav-commit", kind: "commitment", title: "Raise discount coverage toward 80%", monthly_micros: 441_000_000, before_other_savings: true, alternatives: [] }],
      gaps: [],
    };
    renderFixture(f);
    const card = await screen.findByRole("region", { name: "Savings found" });
    expect(card.textContent).toContain("up to $441 a month, before other savings");
    expect(card.textContent).not.toMatch(/\$0\/mo/);
  });

  it("shows the commitment line alone when only commitments were found and none is listed", async () => {
    const f = structuredClone(fixtures.get("09-finops")!);
    f.savings = { ...f.savings, items: [], gaps: [], total: { monthly_micros: 0, count: 0, commitment_monthly_micros: 441_000_000, commitment_count: 1 } };
    renderFixture(f);
    const card = await screen.findByRole("region", { name: "Savings found" });
    expect(card.textContent).toContain("Up to $441 a month, before other savings");
    expect(card.textContent).not.toContain("None found");
    expect(card.textContent).not.toMatch(/\$0(?![\d,.])/);
  });

  it("names a missing account check instead of a bare unavailable", async () => {
    const f = structuredClone(fixtures.get("09e-01-cost-off")!);
    f.summary = { ...f.summary, enabled: false, reason: "no_connected_accounts" };
    renderFixture(f);
    await screen.findByText("No account ready to read cost yet");
    expect(text()).not.toContain("Cost data unavailable");
    expect(screen.getByRole("link", { name: "AWS settings ›" }).getAttribute("href")).toBe("/aws/settings");
  });

  it("keeps stored cost on the page when the only account loses access, marked stale with the cause and the fix", async () => {
    const f = structuredClone(fixtures.get("09-finops")!);
    const sync = { ...f.summary.sync!, stale: true, stale_reason: "no_server_identity", stale_detail: { since: "2026-11-11T05:18:00Z", days: [] } };
    f.summary = { ...f.summary, sync };
    f.bridge = { ...f.bridge, sync };
    renderFixture(f);
    await settle();
    const line = document.querySelector('[data-gap="stale-access"]') as HTMLElement;
    expect(line.textContent).toContain("Stale since Nov 11 05:18 UTC");
    expect(within(line).getByRole("link", { name: "AWS settings ›" }).getAttribute("href")).toBe("/aws/settings");
    // The cause sits behind the ⓘ, not on the page.
    expect(line.textContent).not.toContain("AWS identity");
    await userEvent.click(within(line).getByRole("button", { name: "About stale cost" }));
    expect(screen.getByRole("note").textContent).toContain("The Versus server has no AWS identity");
    // The stored numbers stay.
    expect(screen.getByRole("region", { name: "What moved the cost" })).toBeTruthy();
    expect(text()).not.toContain("No account ready to read cost yet");
  });

  it("points the freshness note to AWS settings for lost access and keeps the retry wording for throttling", async () => {
    for (const [reason, want, not] of [
      ["no_server_identity", "until access is fixed in AWS settings", "next scheduled refresh retries"],
      ["throttled", "next scheduled refresh retries", "until access is fixed"],
    ] as const) {
      const f = structuredClone(fixtures.get("09-finops")!);
      const sync = { ...f.summary.sync!, stale: true, stale_reason: reason, stale_detail: { since: "2026-11-11T05:18:00Z", days: [] } };
      f.summary = { ...f.summary, sync };
      f.bridge = { ...f.bridge, sync };
      renderFixture(f);
      await settle();
      await userEvent.click(screen.getByRole("button", { name: /^Stale since/ }));
      const note = screen.getAllByRole("note").map((n) => n.textContent ?? "").join(" ");
      expect(note).toContain(want);
      expect(note).not.toContain(not);
      cleanup();
    }
  });

  it("keeps the freshness note inside a 390 px phone", async () => {
    setViewport(true);
    const width = vi.spyOn(window, "innerWidth", "get").mockReturnValue(390);
    const height = vi.spyOn(window, "innerHeight", "get").mockReturnValue(844);
    const rect = vi.spyOn(HTMLElement.prototype, "getBoundingClientRect").mockImplementation(function (this: HTMLElement) {
      // The freshness button sits at the right of the header on a phone.
      if (this.tagName === "BUTTON") return DOMRect.fromRect({ x: 290, y: 60, width: 92, height: 24 });
      return DOMRect.fromRect({ x: 0, y: 0, width: 288, height: 120 });
    });
    const f = structuredClone(fixtures.get("09-finops")!);
    const sync = { ...f.summary.sync!, stale: true, stale_reason: "no_server_identity", stale_detail: { since: "2026-11-11T05:18:00Z", days: [] } };
    f.summary = { ...f.summary, sync };
    f.bridge = { ...f.bridge, sync };
    renderFixture(f);
    await settle();
    await userEvent.click(screen.getByRole("button", { name: /^Stale since/ }));
    const note = screen.getAllByRole("note").find((n) => (n.textContent ?? "").includes("AWS settings"))!;
    expect(note.style.position).toBe("fixed");
    const left = parseFloat(note.style.left);
    expect(left).toBeGreaterThanOrEqual(8);
    expect(left + parseFloat(note.style.width)).toBeLessThanOrEqual(390 - 8);
    expect(note.style.visibility).not.toBe("hidden");
    rect.mockRestore();
    width.mockRestore();
    height.mockRestore();
  });

  it("names lost access, not a first setup, when cost was stored but no account can read it", async () => {
    const f = structuredClone(fixtures.get("09e-01-cost-off")!);
    f.summary = {
      ...f.summary,
      enabled: false,
      reason: "no_connected_accounts",
      sync: { ...fixtures.get("09-finops")!.summary.sync!, stale: true, stale_reason: "no_server_identity", stale_detail: { since: "2026-11-11T05:18:00Z", days: [] } },
    };
    renderFixture(f);
    await screen.findByText("No account can read cost");
    expect(text()).not.toContain("yet");
    expect(screen.getByRole("link", { name: "AWS settings ›" })).toBeTruthy();
    await userEvent.click(screen.getByRole("button", { name: "About cost access" }));
    expect(screen.getByRole("note").textContent).toContain("The Versus server has no AWS identity");
    expect(screen.getByRole("note").textContent).toContain("stale since Nov 11 05:18 UTC");
  });

  it("lets both cost-off cards shrink to a phone's width", async () => {
    renderState("09e-01-cost-off");
    const answers = await screen.findByRole("region", { name: "What FinOps answers" });
    const reads = screen.getByRole("region", { name: "What it reads" });
    expect(answers.className).toContain("min-w-0");
    expect(reads.className).toContain("min-w-0");
  });

  describe("increase detail", () => {
    function detail(patch: (d: NonNullable<StateFixture["increases"]>[string]) => void): StateFixture {
      const f = structuredClone(fixtures.get("09b-finops-why")!);
      patch(f.increases!["inc-nat"]);
      return f;
    }

    it("highlights the amount the sentence states", async () => {
      renderFixture(detail(() => {}));
      const h1 = await screen.findByRole("heading", { level: 1 });
      expect(h1.querySelector("span.font-mono")?.textContent).toBe("$1,980");
    });

    it("highlights the whole stated amount, cents included", async () => {
      renderFixture(detail((d) => {
        d.increase.stated_change_micros = 69_330_000;
        d.increase.sentence = "Virtual machines in Harness account went up $69.33 from Oct 1–9";
      }));
      const h1 = await screen.findByRole("heading", { level: 1 });
      expect(h1.querySelector("span.font-mono")?.textContent).toBe("$69.33");
      expect(h1.textContent).toBe("Virtual machines in Harness account went up $69.33 from Oct 1–9");
    });

    it("keeps the last day label inside the chart", async () => {
      const start = Date.UTC(2026, 8, 18);
      renderFixture(detail((d) => {
        d.series = Array.from({ length: 43 }, (_, i) => ({
          day: new Date(start + i * 86_400_000).toISOString().slice(0, 10),
          amount_micros: 30_000_000,
        }));
        d.increase.started_on = "2026-09-20";
      }));
      const chart = await screen.findByRole("img", { name: /^Daily cost, / });
      const labels = Array.from(chart.querySelectorAll("text")).filter((t) => /^[A-Z][a-z]{2} \d+$/.test(t.textContent ?? ""));
      const last = labels[labels.length - 1];
      expect(last.textContent).toBe("Oct 30");
      expect(last.getAttribute("text-anchor")).toBe("end");
      expect(Number(last.getAttribute("x"))).toBeLessThanOrEqual(560);
    });

    it("highlights nothing and shows no $0 when the sentence states no amount", async () => {
      renderFixture(detail((d) => {
        d.increase.stated_change_micros = 0;
        d.increase.period_change_micros = 0;
        d.increase.sentence = "Virtual machines in Harness account went up from Oct 20 to Oct 27, resolved";
      }));
      const h1 = await screen.findByRole("heading", { level: 1 });
      expect(h1.textContent).toBe("Virtual machines in Harness account went up from Oct 20 to Oct 27, resolved");
      expect(h1.querySelector("span.font-mono")).toBeNull();
      expect(h1.textContent).not.toContain("$0");
    });

    it("shows no per-month amount from now on a resolved increase", async () => {
      renderFixture(detail((d) => {
        d.increase.status = "resolved";
        d.increase.monthly_impact_micros = 43_930_950;
      }));
      await screen.findByText(/Network traffic in Production went up/);
      await settle();
      const card = screen.getByRole("region", { name: "Daily cost before and after" });
      expect(card.textContent).not.toContain("/mo from now");
    });

    it("keeps the per-month amount on an open increase", async () => {
      renderFixture(detail(() => {}));
      await screen.findByText(/Network traffic in Production went up/);
      await settle();
      expect(screen.getByRole("region", { name: "Daily cost before and after" }).textContent).toContain("+$2,600/mo from now");
    });

    it("says resource data is off once under the tree, drops $0.00 rows and keeps full usage types", async () => {
      const long = "APS7-SpotUsage:m7i-flex.2xlarge";
      renderFixture(detail((d) => {
        d.resource_data = "off";
        d.resource_note = "Turn on resource-level data in Cost Explorer to see resources";
        d.cause_tree = [
          { usage_type: long, label: "Spot instances", amount_micros: 70_000_000 },
          { usage_type: "APS7-EBS:VolumeUsage", label: "Disks", amount_micros: 3_000 },
          { usage_type: "APS7-DataTransfer-Out-Bytes", label: "Data transfer out", amount_micros: -2_000 },
        ];
      }));
      await screen.findByText(/Network traffic in Production went up/);
      await settle();
      const card = screen.getByRole("region", { name: "Where the increase came from" });
      expect(within(card).getAllByText("Resource data off")).toHaveLength(1);
      expect(card.textContent).not.toContain("$0.00");
      expect(card.textContent).not.toContain("Disks");
      expect(within(card).getByText(long)).toBeTruthy();
    });

    it("shows a cut usage type in full on tap or Enter", async () => {
      measureAsCut();
      const long = "APS7-SpotUsage:m7i-flex.2xlarge-with-a-long-tail";
      renderFixture(detail((d) => {
        d.cause_tree = [{ usage_type: long, label: "Spot instances", amount_micros: 70_000_000 }];
      }));
      await screen.findByText(/Network traffic in Production went up/);
      await settle();
      const cut = screen.getByRole("button", { name: long });
      expect(cut.getAttribute("aria-expanded")).toBe("false");
      expect(cut.className).toContain("truncate");
      cut.focus();
      await userEvent.keyboard("{Enter}");
      expect(cut.getAttribute("aria-expanded")).toBe("true");
      expect(cut.className).not.toContain("truncate");
      expect(cut.className).toContain("whitespace-normal");
      vi.restoreAllMocks();
    });

    it("leaves a usage type that fits as plain text with no tab stop", async () => {
      const short = "APS7-EBS:VolumeUsage";
      renderFixture(detail((d) => {
        d.cause_tree = [{ usage_type: short, label: "Disks", amount_micros: 70_000_000 }];
      }));
      await screen.findByText(/Network traffic in Production went up/);
      await settle();
      const el = screen.getByText(short);
      expect(el.tagName).toBe("SPAN");
      expect(el.hasAttribute("aria-expanded")).toBe(false);
      expect(el.hasAttribute("tabindex")).toBe(false);
      expect(screen.queryByRole("button", { name: short })).toBeNull();
    });

    it("copies a summary with one full stop after a sentence that has one", async () => {
      const writeText = vi.fn().mockResolvedValue(undefined);
      Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
      renderFixture(detail((d) => {
        d.increase.sentence = "Virtual machines in Harness account went up $44 a month from Oct 20 to Oct 27, resolved.";
      }));
      await screen.findByText(/Virtual machines in Harness account went up/);
      await settle();
      await userEvent.click(screen.getByRole("button", { name: "Copy summary" }));
      await waitFor(() => expect(writeText).toHaveBeenCalled());
      const copied = String(writeText.mock.calls[0][0]);
      expect(copied.split("\n")[0]).toBe("Virtual machines in Harness account went up $44 a month from Oct 20 to Oct 27, resolved.");
      expect(copied).not.toContain("..");
    });

    it("puts the usage unit in the label so the label and the figures agree", async () => {
      renderFixture(detail((d) => {
        d.usage_signal = { label: "instances billed per day", before_micros: 93_000_000, after_micros: 122_000_000, unit: "Hrs/day" };
      }));
      await screen.findByText(/Network traffic in Production went up/);
      await settle();
      expect(text()).toContain("instances, hours per day");
      expect(text()).toContain("93 → 122");
      expect(text()).not.toContain("Hrs/day");
    });

    it("draws the daily chart at a phone's scale with no fixed height", async () => {
      setViewport(true);
      renderFixture(detail(() => {}));
      const chart = await screen.findByRole("img", { name: /^Daily cost, / });
      expect(chart.getAttribute("viewBox")).toBe("0 0 360 200");
      expect(chart.getAttribute("class")).not.toContain("h-64");
      expect(chart.querySelector("text")?.getAttribute("class")).toContain("text-[11px]");
    });
  });
});
