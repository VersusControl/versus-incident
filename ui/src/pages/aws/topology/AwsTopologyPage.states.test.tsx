// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { api, ApiError } from "@/lib/api";
import type { TopologyNode, TopologyQuery, TopologyView } from "@/lib/awsEstate";
import { AwsTopologyPage } from "./AwsTopologyPage";
import { renderAwsPage } from "../shared/renderAwsPage";
import { awsAccount, awsSettings } from "../awsFixtures";
import { accountsTopology, DATA, focusTopology, groupsTopology, NETWORK, PLATFORM, PROD, TG, ungroupedTopology } from "../shared/estateFixtures";

// Topology states the page must never get wrong: a card whose counts were
// not read never looks like a clear card, a share over a capped total is
// never exact, a view that is too big draws nothing and says how to
// narrow it, the scope (one account, the Region) reaches every read, and
// nothing unread is drawn green.

vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      awsTopology: vi.fn(),
      awsSettings: vi.fn(),
      awsGroupCorrections: vi.fn(),
      saveAwsGroupCorrection: vi.fn(),
      deleteAwsGroupCorrection: vi.fn(),
      saveAwsAccountLabel: vi.fn(),
      awsTopologySearch: vi.fn(),
    },
  };
});

vi.mock("@/components/TopBar", () => ({
  TopBar: ({ title }: { title: string }) => <h1>{title}</h1>,
}));

const FOUR = [
  awsAccount({ account_id: NETWORK, display_name: "network" }),
  awsAccount({ account_id: PROD, display_name: "prod" }),
  awsAccount({ account_id: PLATFORM, display_name: "platform" }),
  awsAccount({ account_id: DATA, display_name: "data" }),
];

beforeEach(() => {
  Element.prototype.scrollIntoView = vi.fn();
  vi.mocked(api.awsSettings).mockResolvedValue(awsSettings({ accounts: FOUR }));
  vi.mocked(api.awsGroupCorrections).mockResolvedValue({ enabled: true, corrections: [] });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

const page = (path: string) => renderAwsPage(<AwsTopologyPage />, path, "/aws/topology");
const location = () => screen.getByTestId("location").textContent ?? "";

function found(selector: string): Promise<HTMLElement> {
  return waitFor(() => {
    const el = document.querySelector(selector);
    if (!el) throw new Error(`no ${selector}`);
    return el as HTMLElement;
  });
}

const GREEN = /(^|\s)(text|bg|border|fill|stroke|ring)-(sev-ok|ok|good|green|emerald|success)\b/;
function greenElements(root: Element): Element[] {
  return [root, ...root.querySelectorAll("*")].filter((el) => GREEN.test(el.getAttribute("class") ?? ""));
}

/** A node as the server sends it when a count was not read: the field is left out. */
function without(n: TopologyNode, ...keys: Array<keyof TopologyNode>): TopologyNode {
  const out = { ...n };
  for (const k of keys) delete out[k];
  return out;
}

function groupsWith(change: (n: TopologyNode) => TopologyNode): TopologyView {
  const base = groupsTopology();
  return { ...base, nodes: base.nodes.map((n) => (n.kind === "group" ? change(n) : n)) };
}

describe("Topology cards over counts that were not read", () => {
  it("never draws a group whose issue and tier counts were not read like a clear group", async () => {
    const view = groupsWith((n) =>
      n.group === "tag:app=checkout" ? without(n, "open_issues", "act_now", "worth_a_look") : { ...n, open_issues: 0, act_now: 0, worth_a_look: 0 },
    );
    vi.mocked(api.awsTopology).mockResolvedValue(view);
    page(`/aws/topology?level=groups&account=${PROD}`);
    const unread = await found('[data-group="tag:app=checkout"]');
    const clear = document.querySelector('[data-group="tag:service=payments"]') as HTMLElement;
    expect(unread.textContent).toMatch(/not read|not assessed/i);
    expect(clear.textContent).not.toMatch(/not read|not assessed/i);
  });

  it("never draws a connected account whose open issues were not read like a clear account", async () => {
    const base = accountsTopology();
    vi.mocked(api.awsTopology).mockResolvedValue({
      ...base,
      nodes: base.nodes.map((n) => (n.account === DATA ? without(n, "open_issues", "act_now", "worth_a_look") : n)),
    });
    page("/aws/topology?level=accounts");
    const card = await found(`[data-account-card="${DATA}"]`);
    expect(card.textContent).toContain("900 resources");
    expect(card.textContent).toMatch(/issues not read|not assessed/i);
  });

  it("reads a group's changes in 24 h left out by the server as not read, apart from none", async () => {
    const view = groupsWith((n) =>
      n.group === "tag:app=checkout" ? without(n, "changes_24h") : n.group === "tag:service=payments" ? { ...n, changes_24h: 0 } : n,
    );
    vi.mocked(api.awsTopology).mockResolvedValue(view);
    page(`/aws/topology?level=groups&account=${PROD}`);
    const unread = await found('[data-group="tag:app=checkout"]');
    const none = document.querySelector('[data-group="tag:service=payments"]') as HTMLElement;
    expect(unread.textContent).toMatch(/chang\w* not read/i);
    expect(none.textContent).not.toMatch(/not read/i);
  });

  it("reads a capped change count on a group card as more than N, not all read", async () => {
    vi.mocked(api.awsTopology).mockResolvedValue(
      groupsWith((n) => (n.group === "tag:app=checkout" ? { ...n, changes_24h: 50, changes_capped: true } : n)),
    );
    page(`/aws/topology?level=groups&account=${PROD}`);
    const card = await found('[data-group="tag:app=checkout"]');
    expect(card.textContent).toContain("more than 50, not all read changed in 24 h");
  });

  it("never shows Ungrouped's share as exact when another group's count is capped", async () => {
    vi.mocked(api.awsTopology).mockResolvedValue(
      groupsWith((n) => (n.group === "eks:prod-eks" ? { ...n, resources_capped: true } : n)),
    );
    page(`/aws/topology?level=groups&account=${PROD}`);
    const card = await found('[data-group="ungrouped"]');
    expect(card.textContent).not.toMatch(/\d+%/);
  });

  it("never shows Ungrouped's share when another group's count was not read", async () => {
    vi.mocked(api.awsTopology).mockResolvedValue(groupsWith((n) => (n.group === "eks:prod-eks" ? without(n, "resources") : n)));
    page(`/aws/topology?level=groups&account=${PROD}`);
    const card = await found('[data-group="ungrouped"]');
    expect(card.textContent).not.toMatch(/\d+%/);
  });

  it("shows Ungrouped's share when every group was read in full", async () => {
    vi.mocked(api.awsTopology).mockResolvedValue(groupsTopology());
    page(`/aws/topology?level=groups&account=${PROD}`);
    const card = await found('[data-group="ungrouped"]');
    // 468 of 1,200 resources.
    expect(card.textContent).toContain("39%");
  });
});

describe("Topology too many to draw", () => {
  it("draws no card or link at the Accounts level, even when the answer carries nodes, and the account picker opens one account's groups", async () => {
    vi.mocked(api.awsTopology).mockImplementation(async (q: TopologyQuery) =>
      q.level === "groups"
        ? groupsTopology()
        : {
            ...accountsTopology(),
            too_many: true,
            node_count: 412,
            hint: { text: "Too many accounts to draw at once; open one account's groups.", level: "groups" },
          },
    );
    page("/aws/topology?level=accounts");
    const box = await found("[data-too-many]");
    expect(box.textContent).toContain("More than 412 to draw at once");
    expect(box.textContent).toContain("open one account's groups");
    expect(document.querySelector("[data-canvas]")).toBeNull();
    expect(document.querySelector("[data-account-card]")).toBeNull();
    expect(document.querySelector("[data-list]")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Accounts" }));
    fireEvent.mouseDown(await screen.findByRole("option", { name: /^platform/ }));
    await waitFor(() => expect(api.awsTopology).toHaveBeenLastCalledWith(expect.objectContaining({ level: "groups", account: PLATFORM })));
  });

  it("draws no list rows either when the list view is open", async () => {
    vi.mocked(api.awsTopology).mockResolvedValue({ ...groupsTopology(), too_many: true, node_count: 900, hint: { text: "Too many groups.", level: "focus", hops: 1 } });
    page(`/aws/topology?level=groups&account=${PROD}&view=list`);
    await found("[data-too-many]");
    expect(document.querySelector("[data-list]")).toBeNull();
    expect(document.querySelector("[data-group]")).toBeNull();
    // A hop count is no use without a resource: the Groups hint offers none.
    expect(screen.queryByRole("button", { name: /Show \d hops?/ })).toBeNull();
  });

  it("offers the fitting hop count in Focus and asks the server for it", async () => {
    vi.mocked(api.awsTopology).mockImplementation(async (q: TopologyQuery) =>
      q.hops === 3
        ? { ...focusTopology(), hops: 3, too_many: true, node_count: 1200, hint: { text: "Draw fewer hops.", level: "focus", hops: 2 } }
        : focusTopology(),
    );
    page(`/aws/topology?level=focus&account=${PROD}&ref=${encodeURIComponent(TG)}&hops=3`);
    const box = await found("[data-too-many]");
    expect(document.querySelector("[data-canvas]")).toBeNull();
    fireEvent.click(within(box).getByRole("button", { name: "Show 2 hops" }));
    await waitFor(() => expect(api.awsTopology).toHaveBeenLastCalledWith(expect.objectContaining({ level: "focus", hops: 2, account: PROD, ref: TG })));
    expect(await screen.findByText("checkout:42")).toBeTruthy();
  });
});

describe("Topology scope reaches every read", () => {
  it("sends the Region to the group panel's member reads as well as the map", async () => {
    vi.mocked(api.awsTopology).mockImplementation(async (q: TopologyQuery) => {
      if (q.group === "ungrouped") return ungroupedTopology();
      return groupsTopology();
    });
    const sel = encodeURIComponent(`group:${PROD}/tag:app=checkout`);
    page(`/aws/topology?level=groups&account=${PROD}&region=eu-west-1&sel=${sel}`);
    await found("[data-group-panel]");
    await waitFor(() => expect(vi.mocked(api.awsTopology).mock.calls.length).toBeGreaterThanOrEqual(3));
    for (const [q] of vi.mocked(api.awsTopology).mock.calls) {
      expect(q.region, JSON.stringify(q)).toBe("eu-west-1");
    }
  });

  it("sends exactly one account with every Groups and Focus read, from any entry point", async () => {
    vi.mocked(api.awsTopology).mockImplementation(async (q: TopologyQuery) => (q.level === "focus" ? focusTopology() : groupsTopology()));
    for (const path of [
      "/aws/topology?level=groups",
      `/aws/topology?level=focus&ref=${encodeURIComponent(TG)}`,
      "/aws/topology?level=focus&ref=svc",
      `/aws/topology?ref=${encodeURIComponent(TG)}`,
    ]) {
      cleanup();
      vi.mocked(api.awsTopology).mockClear();
      page(path);
      await waitFor(() => expect(api.awsTopology).toHaveBeenCalled());
      for (const [q] of vi.mocked(api.awsTopology).mock.calls) {
        if (q.level === "groups" || q.level === "focus") {
          expect(typeof q.account === "string" && /^\d{12}$/.test(q.account), `${path} ${JSON.stringify(q)}`).toBe(true);
        }
      }
    }
  });

  it("keeps the Region when a search result opens Focus", async () => {
    vi.mocked(api.awsTopology).mockResolvedValue(accountsTopology());
    vi.mocked(api.awsTopologySearch).mockResolvedValue({
      enabled: true,
      capped: false,
      items: [{ ref: TG, name: "checkout-web", type: "elasticloadbalancing:targetgroup", service: "elbv2", account: PROD, region: "eu-west-1" }],
    });
    page("/aws/topology?level=accounts&region=eu-west-1");
    await screen.findByText("Organization");
    fireEvent.change(screen.getByRole("searchbox", { name: "Search resources" }), { target: { value: "checkout" } });
    fireEvent.submit(screen.getByRole("search"));
    expect(api.awsTopologySearch).toHaveBeenCalledWith({ q: "checkout", account: undefined, region: "eu-west-1" });
    fireEvent.click(within(await screen.findByRole("option", { name: /checkout-web/ })).getByRole("button"));
    await waitFor(() => expect(location()).toContain("level=focus"));
    await waitFor(() => expect(api.awsTopology).toHaveBeenLastCalledWith(expect.objectContaining({ level: "focus", account: PROD, ref: TG, region: "eu-west-1" })));
  });

  it("sends nothing for one character, even padded with spaces", async () => {
    vi.mocked(api.awsTopology).mockResolvedValue(accountsTopology());
    page("/aws/topology?level=accounts");
    await screen.findByText("Organization");
    const box = screen.getByRole("searchbox", { name: "Search resources" });
    for (const v of ["c", "  c  ", " "]) {
      fireEvent.change(box, { target: { value: v } });
      fireEvent.submit(screen.getByRole("search"));
    }
    expect(api.awsTopologySearch).not.toHaveBeenCalled();
    expect(document.querySelector("[data-search-results]")).toBeNull();
  });

  it("scopes the search to the account of the Groups view", async () => {
    vi.mocked(api.awsTopology).mockResolvedValue(groupsTopology());
    vi.mocked(api.awsTopologySearch).mockResolvedValue({ enabled: true, capped: false, items: [] });
    page(`/aws/topology?level=groups&account=${PROD}`);
    await screen.findByText("checkout");
    fireEvent.change(screen.getByRole("searchbox", { name: "Search resources" }), { target: { value: "ab" } });
    fireEvent.submit(screen.getByRole("search"));
    expect(await screen.findByText("No resource matches")).toBeTruthy();
    expect(api.awsTopologySearch).toHaveBeenCalledWith({ q: "ab", account: PROD, region: undefined });
  });
});

describe("Topology never green over unread data", () => {
  it("draws a not-read account card grey and dashed with its reason", async () => {
    const base = accountsTopology();
    vi.mocked(api.awsTopology).mockResolvedValue({
      ...base,
      nodes: base.nodes.map((n) =>
        n.account === DATA ? { ...without(n, "resources", "open_issues", "act_now", "worth_a_look"), read: "not_read" as const, not_read_reason: "access denied" } : n,
      ),
    });
    page("/aws/topology?level=accounts");
    const card = await found(`[data-account-card="${DATA}"]`);
    expect(card.getAttribute("data-state")).toBe("not_read");
    expect(card.className).toContain("border-dashed");
    expect(card.textContent).toContain("Not read: access denied");
    expect(greenElements(document.querySelector("main") as HTMLElement)).toEqual([]);
  });

  it("draws a Focus resource with health not read, an unknown last change and unread counts in grey, never green", async () => {
    const base = focusTopology();
    vi.mocked(api.awsTopology).mockResolvedValue({
      ...base,
      nodes: base.nodes.map((n) =>
        n.id === TG
          ? {
              ...without(n, "top_issue", "last_change", "open_issues"),
              health: { state: "not_read" as const, summary: "throttled" },
              last_change_known: false,
            }
          : n,
      ),
    });
    page(`/aws/topology?level=focus&account=${PROD}&ref=${encodeURIComponent(TG)}`);
    const panel = await found("[data-focus-panel]");
    const main = document.querySelector("main") as HTMLElement;
    expect(greenElements(main)).toEqual([]);
    expect(panel.textContent).toContain("Not read: throttled");
    expect(panel.textContent).toContain("Last change not read");
    expect(panel.textContent).not.toContain("No change in the last day");
    expect(panel.textContent).not.toMatch(/Healthy/);
    expect(panel.querySelector("[data-open-issues]")).toBeNull();
    const root = document.querySelector(`[data-root]`) as HTMLElement;
    expect(root.textContent).toContain("not read");
    expect(root.querySelector("[data-problem]")).toBeNull();
  });

  it.each(["ok", "none", "unknown", "warning", "critical"] as const)("paints health %s without green", async (state) => {
    const base = focusTopology();
    vi.mocked(api.awsTopology).mockResolvedValue({
      ...base,
      nodes: base.nodes.map((n) => (n.id === TG ? { ...n, health: { state } } : n)),
    });
    page(`/aws/topology?level=focus&account=${PROD}&ref=${encodeURIComponent(TG)}`);
    await found("[data-focus-panel]");
    expect(greenElements(document.querySelector("main") as HTMLElement)).toEqual([]);
  });

  it("has no warning line: the warning lives in a summary card, and Topology has none", async () => {
    vi.mocked(api.awsTopology).mockResolvedValue(accountsTopology());
    page("/aws/topology?level=accounts");
    await screen.findByText("Organization");
    expect(document.querySelector("[data-warning-line]")).toBeNull();
    expect(document.querySelector("main")?.textContent).not.toContain("Data not complete");
  });
});

describe("Topology robustness", () => {
  it.each([
    ["Groups", `/aws/topology?level=groups&account=${PROD}`],
    ["Focus", `/aws/topology?level=focus&account=${PROD}&ref=${encodeURIComponent(TG)}`],
  ])("shows a retry on a server error at the %s level", async (_level, path) => {
    vi.mocked(api.awsTopology).mockRejectedValue(new ApiError(500, "boom"));
    page(path);
    expect(await screen.findByText(/Couldn't load AWS Topology/)).toBeTruthy();
  });

  it("names the missing access on a refusal", async () => {
    vi.mocked(api.awsTopology).mockRejectedValue(new ApiError(403, "forbidden"));
    page("/aws/topology?level=accounts");
    expect(await screen.findByText("You don't have access to AWS Topology")).toBeTruthy();
  });

  it("leaves the loading card when the account list fails and the link names no account", async () => {
    vi.mocked(api.awsSettings).mockRejectedValue(new ApiError(500, "boom"));
    vi.mocked(api.awsTopology).mockResolvedValue(groupsTopology());
    page("/aws/topology?level=groups");
    // Once the account list has failed, the page must answer: a read of
    // some account, or an error with a retry, never a loading card forever.
    await waitFor(() => expect(api.awsSettings).toHaveBeenCalled());
    await waitFor(() => expect(document.querySelector(".sk")).toBeNull());
  });

  it("draws a removed account's links that point at nodes not in the answer without failing", async () => {
    const base = accountsTopology();
    vi.mocked(api.awsTopology).mockResolvedValue({
      ...base,
      nodes: base.nodes.filter((n) => n.account !== DATA),
      links: [...base.links, { from: "account:000000000000", to: `account:${PROD}`, type: "trusts", kind: "may_access", count: 1, both_sides: 0, seen_from: "one" }],
    });
    page("/aws/topology?level=accounts");
    await screen.findByText("Organization");
    expect(document.querySelector(`[data-account-card="${DATA}"]`)).toBeNull();
    expect(document.querySelector('[data-link^="account:000000000000"]')).toBeNull();
    expect(document.querySelector(`[data-account-card="${PROD}"]`)).not.toBeNull();
  });

  it("draws an empty Focus answer and an empty group without failing", async () => {
    vi.mocked(api.awsTopology).mockResolvedValue({ ...focusTopology(), nodes: [], links: [] });
    page(`/aws/topology?level=focus&account=${PROD}&ref=${encodeURIComponent(TG)}`);
    expect(await screen.findByText("This resource is not in the stored reads")).toBeTruthy();
    cleanup();
    vi.mocked(api.awsTopology).mockResolvedValue({ ...groupsTopology(), group: "tag:app=checkout", nodes: [], links: [] });
    page(`/aws/topology?level=groups&account=${PROD}&group=${encodeURIComponent("tag:app=checkout")}`);
    expect(await screen.findByText("No resources in this group")).toBeTruthy();
  });

  it("keeps 40 groups with long names in their cards", async () => {
    const long = "checkout-payments-and-fulfilment-platform-service-with-a-very-long-name";
    const base = groupsTopology();
    const groups: TopologyNode[] = Array.from({ length: 40 }, (_, i) => ({
      id: `group:${PROD}/g${i}`,
      kind: "group",
      name: i === 0 ? long : `group-${i}`,
      account: PROD,
      group: `g${i}`,
      rule: "tag",
      key: "app",
      resources: i + 1,
      open_issues: 0,
    }));
    vi.mocked(api.awsTopology).mockResolvedValue({ ...base, nodes: groups, links: [] });
    page(`/aws/topology?level=groups&account=${PROD}`);
    await found('[data-group="g39"]');
    expect(document.querySelectorAll("[data-group]")).toHaveLength(40);
    const name = screen.getByRole("button", { name: long });
    expect(name.className).toContain("truncate");
    expect(name.getAttribute("title")).toBe(long);
  });
});
