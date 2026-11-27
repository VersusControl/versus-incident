// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { api, ApiError } from "@/lib/api";
import { AwsTopologyPage } from "./AwsTopologyPage";
import { renderAwsPage } from "../shared/renderAwsPage";
import { awsAccount, awsSettings } from "../awsFixtures";
import { accountsTopology, correction, DATA, focusTopology, groupsTopology, NETWORK, PLATFORM, PROD, TG, ungroupedTopology, UNKNOWN } from "../shared/estateFixtures";
import type { TopologyQuery } from "@/lib/awsEstate";
import { fitView, layoutFocus, MIN_SCALE } from "./layout";
import { mapHeight } from "./useMapView";

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

beforeEach(() => {
  // jsdom has no scrolling; the dropdown scrolls its active option into view.
  Element.prototype.scrollIntoView = vi.fn();
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
  vi.mocked(api.awsGroupCorrections).mockResolvedValue({ enabled: true, corrections: [] });
  vi.mocked(api.saveAwsGroupCorrection).mockResolvedValue({ correction: correction() });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

const page = (path = "/aws/topology") => renderAwsPage(<AwsTopologyPage />, path, "/aws/topology");
const location = () => screen.getByTestId("location").textContent ?? "";

/** Waits for an element that matches `selector`. */
function found(selector: string): Promise<HTMLElement> {
  return waitFor(() => {
    const el = document.querySelector(selector);
    if (!el) throw new Error(`no ${selector}`);
    return el as HTMLElement;
  });
}

/** Answers the Groups level, and the Ungrouped group the panel reads its Move list from. */
function groupsAnswer(view = groupsTopology()) {
  return async (q: TopologyQuery) => {
    if (q.group === "ungrouped") return ungroupedTopology();
    if (q.group) {
      // An opened group: its members.
      return {
        ...view,
        group: q.group,
        nodes: [{ id: "arn:aws:s3:::checkout-assets", kind: "resource" as const, name: "checkout-assets", account: PROD, type: "s3:bucket", group: q.group }],
        links: [],
      };
    }
    return view;
  };
}

const SEL = encodeURIComponent(`group:${PROD}/tag:app=checkout`);

describe("AWS Topology, Accounts level", () => {
  it("draws every account and marks links seen from one side in amber dashes", async () => {
    vi.mocked(api.awsTopology).mockResolvedValue(accountsTopology());
    page();
    await screen.findByText("Organization");
    expect(api.awsTopology).toHaveBeenCalledWith(expect.objectContaining({ level: undefined, lens: ["network", "identity", "data"] }));
    // data's one-side trust to an outside account is one line to the Other accounts column.
    const trust = document.querySelector(`[data-link="account:${DATA}>column:other-accounts"]`) as SVGGElement;
    expect(trust.getAttribute("data-sides")).toBe("one");
    expect(trust.getAttribute("class")).toContain("text-sev-warn");
    expect(trust.querySelector("path")?.getAttribute("stroke-dasharray")).toBe("6 4");
    expect(trust.querySelector("title")?.textContent).toContain("Role trust 2");
    const both = [...document.querySelectorAll("[data-link]")].find((e) =>
      [`account:${PLATFORM}>account:${PROD}`, `account:${PROD}>account:${PLATFORM}`].includes(e.getAttribute("data-link") ?? ""),
    ) as SVGGElement;
    expect(both.getAttribute("data-sides")).toBe("both");
    expect(both.querySelector("path")?.getAttribute("stroke-dasharray")).toBeNull();
    expect(both.querySelector("title")?.textContent).toContain("DNS 12");
    const policy = [...document.querySelectorAll("[data-link]")].find((e) =>
      [`account:${PROD}>account:${DATA}`, `account:${DATA}>account:${PROD}`].includes(e.getAttribute("data-link") ?? ""),
    ) as SVGGElement;
    expect(policy.getAttribute("data-kind")).toBe("may_access");
    expect(policy.querySelector("path")?.getAttribute("stroke-dasharray")).toBe("2 4");
    expect(document.querySelector(`[data-account-card="${NETWORK}"]`)?.textContent).toContain("Hub");
  });

  it("shows other accounts, the far end and the same-organization account apart from the connected ones", async () => {
    vi.mocked(api.awsTopology).mockResolvedValue(accountsTopology());
    page();
    await screen.findByText("Organization");
    expect(document.querySelector("[data-far]")?.textContent).toContain("Load balancers we cannot read");
    const same = document.querySelector('[data-label="same_org"]') as HTMLElement;
    expect(within(same).getByRole("link", { name: "Connect ›" })).toBeTruthy();
    const unknown = document.querySelector(`[data-account-card="${UNKNOWN}"]`) as HTMLElement;
    expect(unknown.textContent).toContain("Unknown");
    expect(within(unknown).getByRole("button", { name: "Label this account" })).toBeTruthy();
    // Topology has no summary card and says nothing about coverage.
    expect(document.querySelector("[data-summary-card]")).toBeNull();
    expect(document.querySelector("[data-warning-line]")).toBeNull();
  });

  it("labels an unknown account as known", async () => {
    vi.mocked(api.awsTopology).mockResolvedValue(accountsTopology());
    vi.mocked(api.saveAwsAccountLabel).mockResolvedValue({ label: { account_id: UNKNOWN, label: "known", name: "Vendor X", created_at: "" } });
    page();
    await screen.findByText("Organization");
    const card = document.querySelector(`[data-account-card="${UNKNOWN}"]`) as HTMLElement;
    await waitFor(() => expect((within(card).getByRole("button", { name: "Label this account" }) as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(within(card).getByRole("button", { name: "Label this account" }));
    fireEvent.change(within(card).getByRole("textbox"), { target: { value: "Vendor X" } });
    fireEvent.click(within(card).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(api.saveAwsAccountLabel).toHaveBeenCalledWith(UNKNOWN, { label: "known", name: "Vendor X" }));
  });

  it("opens an account's groups from its card", async () => {
    vi.mocked(api.awsTopology).mockResolvedValue(accountsTopology());
    page();
    await screen.findByText("Organization");
    fireEvent.click(document.querySelector(`[data-account-card="${PROD}"]`) as HTMLElement);
    await waitFor(() => expect(location()).toContain(`level=groups&account=${PROD}`));
  });

  it("draws the level the server picked for one account", async () => {
    vi.mocked(api.awsTopology).mockImplementation(groupsAnswer());
    page();
    expect(await screen.findByText("checkout")).toBeTruthy();
    expect(document.querySelector('[data-level="groups"]')).not.toBeNull();
  });

  it("reads an account that could not be read as not read, never as 0 resources", async () => {
    const v = accountsTopology();
    v.nodes = v.nodes.map((n) => (n.account === DATA ? { ...n, read: "not_read", not_read_reason: "access denied", resources: undefined, open_issues: undefined } : n));
    vi.mocked(api.awsTopology).mockResolvedValue(v);
    page();
    await screen.findByText("Organization");
    const card = document.querySelector(`[data-account-card="${DATA}"]`) as HTMLElement;
    expect(card.getAttribute("data-state")).toBe("not_read");
    expect(card.className).toContain("border-dashed");
    expect(card.textContent).toContain("Not read: access denied");
    expect(card.textContent).not.toContain("0 resources");
  });

  it("reads a capped or partial account count as more than N, not all read", async () => {
    const v = accountsTopology();
    v.nodes = v.nodes.map((n) => (n.account === PROD ? { ...n, resources_capped: true } : n.account === DATA ? { ...n, read: "partial" } : n));
    vi.mocked(api.awsTopology).mockResolvedValue(v);
    page();
    await screen.findByText("Organization");
    expect(document.querySelector(`[data-account-card="${PROD}"]`)?.textContent).toContain("more than 1,200 resources, not all read");
    expect(document.querySelector(`[data-account-card="${DATA}"]`)?.textContent).toContain("more than 900 resources, not all read");
  });

  it("keeps 40 accounts with long names in their cards", async () => {
    const long = "production-workloads-eu-west-1-payments-and-checkout-platform";
    const nodes = Array.from({ length: 40 }, (_, i) => {
      const id = String(100000000000 + i);
      return { id: `account:${id}`, kind: "account" as const, name: i === 0 ? long : `account-${i}`, account: id, label: "connected" as const, read: "read" as const, resources: i };
    });
    vi.mocked(api.awsTopology).mockResolvedValue({ ...accountsTopology(), nodes, links: [] });
    page();
    await screen.findByText("Connected accounts");
    expect(document.querySelectorAll("[data-account-card]")).toHaveLength(40);
    expect(screen.getByText(long).className).toContain("truncate");
  });
});

describe("AWS Topology, Groups level", () => {
  it("draws the groups with the rule that made each, and Ungrouped as its own card", async () => {
    vi.mocked(api.awsTopology).mockImplementation(groupsAnswer());
    page(`/aws/topology?level=groups&account=${PROD}`);
    await screen.findByText("checkout");
    expect(document.querySelector('[data-group="tag:app=checkout"] [data-rule]')?.textContent).toBe("tag app");
    expect(document.querySelector('[data-group="eks:prod-eks/ns:search"] [data-rule]')?.textContent).toBe("namespace search");
    const ungrouped = document.querySelector('[data-group="ungrouped"]') as HTMLElement;
    expect(ungrouped.textContent).toContain("39%");
    expect(ungrouped.textContent).toContain("volumes 212");
    expect(document.querySelector(`[data-pill="account:${PLATFORM}"]`)?.textContent).toContain("platform");
  });

  it("opens one group's resources", async () => {
    vi.mocked(api.awsTopology).mockImplementation(groupsAnswer());
    page(`/aws/topology?level=groups&account=${PROD}`);
    await screen.findByText("checkout");
    fireEvent.click(within(document.querySelector('[data-group="tag:app=checkout"]') as HTMLElement).getByRole("button", { name: "Open ›" }));
    await waitFor(() => expect(location()).toContain("group=tag%3Aapp%3Dcheckout"));
    await waitFor(() => expect(api.awsTopology).toHaveBeenLastCalledWith(expect.objectContaining({ level: "groups", account: PROD, group: "tag:app=checkout" })));
  });

  it("opens the side panel of a group", async () => {
    vi.mocked(api.awsTopology).mockImplementation(groupsAnswer());
    page(`/aws/topology?level=groups&account=${PROD}`);
    await screen.findByText("checkout");
    fireEvent.click(screen.getByRole("button", { name: "checkout" }));
    const panel = await found('[data-group-panel="tag:app=checkout"]');
    expect(within(panel).getByRole("button", { name: "Rename" })).toBeTruthy();
    expect(within(panel).getByRole("button", { name: "Merge with" })).toBeTruthy();
  });

  async function renameTo(name: string) {
    vi.mocked(api.awsTopology).mockImplementation(groupsAnswer());
    page(`/aws/topology?level=groups&account=${PROD}&sel=${SEL}`);
    const panel = await found("[data-group-panel]");
    const rename = within(panel).getByRole("button", { name: "Rename" }) as HTMLButtonElement;
    await waitFor(() => expect(rename.disabled).toBe(false));
    fireEvent.click(rename);
    fireEvent.change(within(panel).getByRole("textbox", { name: "Group name" }), { target: { value: name } });
    fireEvent.click(within(panel).getByRole("button", { name: "Save" }));
    return panel;
  }

  it("saves a rename from the correction it started from", async () => {
    await renameTo("Checkout");
    await waitFor(() =>
      expect(api.saveAwsGroupCorrection).toHaveBeenCalledWith({
        account_id: PROD,
        kind: "rename",
        group_id: "tag:app=checkout",
        name: "Checkout",
        target_group_id: undefined,
      }),
    );
  });

  it("sends the created_at it read, and maps the server's 409 to a reload", async () => {
    const read = correction({ name: "Shop", created_at: "2026-11-25T10:00:00Z" });
    vi.mocked(api.awsGroupCorrections).mockResolvedValue({ enabled: true, corrections: [read] });
    vi.mocked(api.saveAwsGroupCorrection).mockRejectedValue(new ApiError(409, "changed by someone else; reload", { code: "correction_conflict" }));
    const panel = await renameTo("Checkout");
    await waitFor(() =>
      expect(api.saveAwsGroupCorrection).toHaveBeenCalledWith(expect.objectContaining({ kind: "rename", expected_created_at: "2026-11-25T10:00:00Z" })),
    );
    const alert = await within(panel).findByRole("alert");
    expect(alert.textContent).toContain("Changed by someone else");
    expect(within(alert).getByRole("button", { name: "Reload" })).toBeTruthy();
  });

  it("names another 409 as a failure, not a conflict", async () => {
    vi.mocked(api.saveAwsGroupCorrection).mockRejectedValue(new ApiError(409, "inherited", { code: "correction_inherited" }));
    const panel = await renameTo("Checkout");
    await within(panel).findByRole("alert");
    expect(panel.querySelector("[data-conflict]")).toBeNull();
    expect(panel.querySelector("[data-failure]")?.textContent).toBe("inherited");
  });

  it("refuses a rename when someone else changed the group since, and asks for a reload", async () => {
    vi.mocked(api.awsGroupCorrections)
      .mockResolvedValueOnce({ enabled: true, corrections: [] })
      .mockResolvedValue({ enabled: true, corrections: [correction({ name: "Shop", created_at: "2026-11-26T09:00:00Z" })] });
    const panel = await renameTo("Checkout");
    const alert = await within(panel).findByRole("alert");
    expect(alert.textContent).toContain("Changed by someone else");
    expect(api.saveAwsGroupCorrection).not.toHaveBeenCalled();
    fireEvent.click(within(alert).getByRole("button", { name: "Reload" }));
    await waitFor(() => expect(panel.querySelector("[data-conflict]")).toBeNull());
  });

  it("shows the same reload when the server refuses the write as a conflict", async () => {
    vi.mocked(api.saveAwsGroupCorrection).mockRejectedValue(new ApiError(409, "conflict", { code: "correction_conflict" }));
    const panel = await renameTo("Checkout");
    expect((await within(panel).findByRole("alert")).textContent).toContain("Changed by someone else");
  });

  it("names the correction limit as a failure, not a conflict", async () => {
    vi.mocked(api.saveAwsGroupCorrection).mockRejectedValue(
      new ApiError(409, "this account holds the most corrections allowed; delete one first", { code: "correction_limit" }),
    );
    const panel = await renameTo("Checkout");
    const alert = await within(panel).findByRole("alert");
    expect(alert.textContent).toContain("most corrections allowed");
    expect(panel.querySelector("[data-conflict]")).toBeNull();
  });

  it("lists corrections that no longer match, never applied to another group", async () => {
    vi.mocked(api.awsGroupCorrections).mockResolvedValue({
      enabled: true,
      corrections: [correction({ group_id: "prefix:shop-", name: "Shop", status: "no_longer_matches" })],
    });
    vi.mocked(api.awsTopology).mockImplementation(groupsAnswer());
    page(`/aws/topology?level=groups&account=${PROD}&sel=${SEL}`);
    const list = await found("[data-no-longer-matches]");
    expect(list.textContent).toContain("Rename prefix:shop- to Shop");
    expect(list.textContent).toContain("No longer matches");
  });

  it("moves an ungrouped resource into the selected group", async () => {
    vi.mocked(api.awsTopology).mockImplementation(groupsAnswer());
    page(`/aws/topology?level=groups&account=${PROD}&sel=${SEL}`);
    const panel = await found("[data-group-panel]");
    const picker = within(panel).getByRole("button", { name: "Resource to move" }) as HTMLButtonElement;
    await waitFor(() => expect(picker.disabled).toBe(false));
    fireEvent.click(picker);
    fireEvent.mouseDown(await screen.findByRole("option", { name: /checkout-receipts/ }));
    fireEvent.click(within(panel).getByRole("button", { name: "Move" }));
    await waitFor(() =>
      expect(api.saveAwsGroupCorrection).toHaveBeenCalledWith(
        expect.objectContaining({ kind: "move", member_arn: "arn:aws:s3:::checkout-receipts", target_group_id: "tag:app=checkout" }),
      ),
    );
  });

  it("reads an account whose resources were not read as not read", async () => {
    vi.mocked(api.awsTopology).mockResolvedValue({
      ...groupsTopology(),
      nodes: [{ id: `account:${PROD}`, kind: "account", name: "prod", account: PROD, label: "connected", read: "not_read", not_read_reason: "access denied" }],
      links: [],
    });
    page(`/aws/topology?level=groups&account=${PROD}`);
    const note = await screen.findByText("Not read: access denied");
    expect(note.getAttribute("data-not-read")).toBe("true");
  });

  it("reads a group whose count was not read as not read", async () => {
    const v = groupsTopology();
    v.nodes = v.nodes.map((n) => (n.group === "stack:baseline" ? { ...n, resources: undefined } : n));
    vi.mocked(api.awsTopology).mockImplementation(groupsAnswer(v));
    page(`/aws/topology?level=groups&account=${PROD}`);
    await screen.findByText("checkout");
    expect(document.querySelector('[data-group="stack:baseline"]')?.textContent).toContain("Resources not read");
  });

  it("says an account holds nothing only when it was read", async () => {
    vi.mocked(api.awsTopology).mockResolvedValue({ ...groupsTopology(), nodes: [], links: [] });
    page(`/aws/topology?level=groups&account=${PROD}`);
    expect(await screen.findByText("No resources in this account")).toBeTruthy();
  });

  it("lists the groups in the list view", async () => {
    vi.mocked(api.awsTopology).mockImplementation(groupsAnswer());
    page(`/aws/topology?level=groups&account=${PROD}&view=list`);
    await screen.findByText("checkout");
    expect(document.querySelector("[data-list]")).not.toBeNull();
    expect(document.querySelector("[data-canvas]")).toBeNull();
  });

  it("opens the first account's groups from an old link without one", async () => {
    vi.mocked(api.awsTopology).mockImplementation(groupsAnswer());
    page("/aws/topology?level=groups");
    await screen.findByText("checkout");
    expect(api.awsTopology).toHaveBeenCalledWith(expect.objectContaining({ level: "groups", account: NETWORK }));
  });
});

describe("AWS Topology, Focus level", () => {
  it("draws the resource in the middle with its hops and the far end", async () => {
    vi.mocked(api.awsTopology).mockResolvedValue(focusTopology());
    page(`/aws/topology?level=focus&ref=${encodeURIComponent(TG)}`);
    await screen.findByText("checkout:42");
    expect(document.querySelector("[data-root]")?.textContent).toContain("checkout-web");
    expect(document.querySelector("[data-dangling]")?.className).toContain("border-dashed");
    expect(document.querySelector('[data-link="far-lb>dns"]')?.getAttribute("data-sides")).toBe("one");
    expect(document.querySelector('[data-link="cdn>lb"] path')?.getAttribute("stroke-dasharray")).toBe("6 4");
    const panel = document.querySelector("[data-focus-panel]") as HTMLElement;
    expect(panel.querySelector('[data-health="critical"]')?.textContent).toContain("0 of 4 targets healthy");
    // A neighbour the fan-out cap left out is counted, never dropped silently.
    expect(document.querySelector("[data-hidden]")?.textContent).toBe("2 more resources not drawn");
  });

  it("asks for more hops from the server", async () => {
    vi.mocked(api.awsTopology).mockResolvedValue(focusTopology());
    page(`/aws/topology?level=focus&ref=${encodeURIComponent(TG)}`);
    await screen.findByText("checkout:42");
    fireEvent.click(within(screen.getByRole("group", { name: "Hops" })).getByRole("button", { name: "3" }));
    await waitFor(() => expect(api.awsTopology).toHaveBeenLastCalledWith(expect.objectContaining({ level: "focus", hops: 3, ref: TG })));
  });

  it("marks a resource whose policy was not read, never as clear", async () => {
    vi.mocked(api.awsTopology).mockResolvedValue(focusTopology());
    page(`/aws/topology?level=focus&ref=${encodeURIComponent(TG)}`);
    await screen.findByText("checkout:42");
    const ecr = document.querySelector('[data-node="ecr"]') as HTMLElement;
    expect(ecr.className).toContain("border-dashed");
    expect(ecr.textContent).toContain("not read");
    fireEvent.click(ecr);
    const panel = await found('[data-focus-panel="ecr"]');
    const notRead = [...panel.querySelectorAll("[data-not-read]")].map((e) => e.textContent);
    expect(notRead).toEqual(["Not read: access denied", "policy", "Not assessed"]);
    expect(panel.querySelector("[data-health]")).toBeNull();
  });

  it("asks for a resource before drawing anything", async () => {
    page("/aws/topology?level=focus");
    expect(await screen.findByText("Open a resource from a group or an issue to see what it connects to.")).toBeTruthy();
    expect(api.awsTopology).not.toHaveBeenCalled();
  });

  it("names a resource that is gone from the stored reads", async () => {
    vi.mocked(api.awsTopology).mockRejectedValue(new ApiError(404, "not in the stored reads", { code: "not_found" }));
    page(`/aws/topology?level=focus&ref=${encodeURIComponent(TG)}`);
    expect(await screen.findByText(/Not in the stored reads/)).toBeTruthy();
  });
});

describe("AWS Topology states", () => {
  it("shows the narrower-scope hint and draws nothing when there are too many nodes", async () => {
    vi.mocked(api.awsTopology).mockResolvedValue({
      ...groupsTopology(),
      nodes: [],
      links: [],
      too_many: true,
      node_count: 1840,
      hint: { text: "Too many groups to draw at once; search for a resource and focus on it.", level: "focus", hops: 1 },
    });
    page(`/aws/topology?level=groups&account=${PROD}`);
    const box = await found("[data-too-many]");
    expect(box.textContent).toContain("More than 1,840 to draw at once");
    expect(box.textContent).toContain("Too many groups to draw at once");
    expect(document.querySelector("[data-canvas]")).toBeNull();
  });

  it("offers the largest hop count that fits a focus view", async () => {
    vi.mocked(api.awsTopology).mockResolvedValue({ ...focusTopology(), hops: 3, nodes: [], links: [], too_many: true, node_count: 300, hint: { text: "Fewer hops fit.", level: "focus", hops: 1 } });
    page(`/aws/topology?level=focus&ref=${encodeURIComponent(TG)}&hops=3`);
    const box = await found("[data-too-many]");
    expect(document.querySelector("[data-canvas]")).toBeNull();
    fireEvent.click(within(box).getByRole("button", { name: "Show 1 hop" }));
    await waitFor(() => expect(location()).toContain("hops=1"));
  });

  it("shows a loading card first, then an error with a retry", async () => {
    vi.mocked(api.awsTopology).mockRejectedValue(new ApiError(500, "boom"));
    page();
    expect(document.querySelector(".sk")).not.toBeNull();
    expect(await screen.findByText(/Couldn't load AWS Topology/)).toBeTruthy();
  });

  it("offers to connect an account when none is connected", async () => {
    vi.mocked(api.awsTopology).mockResolvedValue({ ...accountsTopology(), nodes: [], links: [] });
    page("/aws/topology?level=accounts");
    expect(await screen.findByText("No AWS account is connected.")).toBeTruthy();
  });
});

describe("AWS Topology, wired fields", () => {
  it("colours account cards by tier: red only with an Act-now issue", async () => {
    vi.mocked(api.awsTopology).mockResolvedValue(accountsTopology());
    page();
    await screen.findByText("Organization");
    const platform = document.querySelector(`[data-account-card="${PLATFORM}"] [data-tier]`) as HTMLElement;
    expect(platform.getAttribute("data-tier")).toBe("act_now");
    expect(platform.className).toContain("text-sev-critical");
    const network = document.querySelector(`[data-account-card="${NETWORK}"] [data-tier]`) as HTMLElement;
    expect(network.getAttribute("data-tier")).toBe("worth_a_look");
    expect(network.className).not.toContain("sev-");
    expect(document.querySelector(`[data-account-card="${NETWORK}"]`)?.textContent).toContain("transit gateway");
  });

  it("colours group cards by tier and reads a capped tier as more than N", async () => {
    const v = groupsTopology();
    v.nodes = v.nodes.map((n) => (n.group === "tag:service=payments" ? { ...n, act_now: 0, worth_a_look: 3, worth_a_look_capped: true } : n));
    vi.mocked(api.awsTopology).mockImplementation(groupsAnswer(v));
    page(`/aws/topology?level=groups&account=${PROD}`);
    await screen.findByText("checkout");
    const act = document.querySelector('[data-group="tag:app=checkout"] [data-tier]') as HTMLElement;
    expect(act.textContent).toBe("1 act now");
    expect(act.className).toContain("text-sev-critical");
    const worth = document.querySelector('[data-group="eks:prod-eks"] [data-tier]') as HTMLElement;
    expect(worth.textContent).toBe("1 worth a look");
    expect(worth.className).toContain("text-ink-300");
    expect(document.querySelector('[data-group="tag:service=payments"] [data-tier]')?.textContent).toBe("more than 3, not all read worth a look");
  });

  it("shows the health states, never green, with not read dashed", async () => {
    const states = [
      ["critical", "text-sev-critical"],
      ["warning", "text-sev-warn"],
      ["ok", "text-ink-100"],
      ["none", "text-ink-200"],
      ["unknown", "text-ink-300"],
    ] as const;
    for (const [state, tone] of states) {
      const v = focusTopology();
      v.nodes = v.nodes.map((n) => (n.id === TG ? { ...n, health: { state, summary: `health ${state}` } } : n));
      vi.mocked(api.awsTopology).mockResolvedValue(v);
      page(`/aws/topology?level=focus&account=${PROD}&ref=${encodeURIComponent(TG)}`);
      const health = await found(`[data-health="${state}"]`);
      expect(health.querySelector("span")?.className, state).toBe(tone);
      expect(health.innerHTML, state).not.toMatch(/sev-ok|green/);
      cleanup();
    }
    const v = focusTopology();
    v.nodes = v.nodes.map((n) => (n.id === TG ? { ...n, health: { state: "not_read", summary: "access denied" } } : n));
    vi.mocked(api.awsTopology).mockResolvedValue(v);
    page(`/aws/topology?level=focus&account=${PROD}&ref=${encodeURIComponent(TG)}`);
    const panel = await found("[data-focus-panel]");
    expect(panel.querySelector("[data-health]")).toBeNull();
    const dashed = panel.querySelector("[data-not-read]") as HTMLElement;
    expect(dashed.textContent).toBe("Not read: access denied");
    expect(dashed.className).toContain("border-dashed");
  });

  it("names a clear resource with nothing to check without a number or colour", async () => {
    const v = focusTopology();
    v.nodes = v.nodes.map((n) => (n.id === TG ? { ...n, health: { state: "none" }, top_issue: undefined, open_issues: 0 } : n));
    vi.mocked(api.awsTopology).mockResolvedValue(v);
    page(`/aws/topology?level=focus&account=${PROD}&ref=${encodeURIComponent(TG)}`);
    expect((await found('[data-health="none"]')).textContent).toBe("Nothing to check");
    expect(document.querySelector("[data-open-issues]")?.textContent).toBe("0");
  });

  it("shows the top issue and its likely cause", async () => {
    vi.mocked(api.awsTopology).mockResolvedValue(focusTopology());
    page(`/aws/topology?level=focus&account=${PROD}&ref=${encodeURIComponent(TG)}`);
    const panel = await found("[data-focus-panel]");
    expect(panel.querySelector("[data-top-issue]")?.textContent).toBe("checkout-web target group has no healthy targets");
    expect(panel.textContent).toContain("High, act now");
    expect(within(panel).getByRole("link", { name: "Open ›" }).getAttribute("href")).toBe("/aws/issues?id=iss-3");
    expect(panel.querySelector("[data-likely-cause]")?.textContent).toContain("task definition :42");
    const root = [...document.querySelectorAll("[data-node]")].find((e) => e.getAttribute("data-node") === TG) as HTMLElement;
    expect(root.querySelector("[data-problem]")?.className).toContain("bg-sev-critical");
  });
});

describe("AWS Topology scope", () => {
  it("asks for the Accounts level over every account, never Groups over two or more", async () => {
    vi.mocked(api.awsTopology).mockResolvedValue(accountsTopology());
    page("/aws/topology?level=accounts");
    await screen.findByText("Organization");
    for (const [q] of vi.mocked(api.awsTopology).mock.calls) {
      expect(q.level === "groups" && !q.account).toBe(false);
    }
    expect(api.awsTopology).toHaveBeenCalledWith(expect.objectContaining({ level: "accounts", account: undefined }));
  });

  it("opens one account's groups from the Accounts picker, and every account again from All", async () => {
    vi.mocked(api.awsTopology).mockImplementation(async (q) => (q.level === "accounts" || !q.level ? accountsTopology() : groupsAnswer()(q)));
    page("/aws/topology?level=accounts");
    await screen.findByText("Organization");
    fireEvent.click(screen.getByRole("button", { name: "Accounts" }));
    fireEvent.mouseDown(await screen.findByRole("option", { name: /^prod/ }));
    await waitFor(() => expect(location()).toContain(`level=groups&account=${PROD}`));
    await waitFor(() => expect(api.awsTopology).toHaveBeenLastCalledWith(expect.objectContaining({ level: "groups", account: PROD })));
    fireEvent.click(screen.getByRole("button", { name: "Accounts" }));
    fireEvent.mouseDown(await screen.findByRole("option", { name: /^All/ }));
    await waitFor(() => expect(api.awsTopology).toHaveBeenLastCalledWith(expect.objectContaining({ level: "accounts", account: undefined })));
  });

  it("passes the Region to the topology", async () => {
    vi.mocked(api.awsTopology).mockResolvedValue(accountsTopology());
    page("/aws/topology?level=accounts&region=eu-west-1");
    await screen.findByText("Organization");
    expect(api.awsTopology).toHaveBeenCalledWith(expect.objectContaining({ region: "eu-west-1" }));
  });

  it("opens Focus with the resource's own account when the link has none", async () => {
    vi.mocked(api.awsTopology).mockResolvedValue(focusTopology());
    page(`/aws/topology?level=focus&ref=${encodeURIComponent(TG)}`);
    await screen.findByText("checkout:42");
    expect(api.awsTopology).toHaveBeenCalledWith(expect.objectContaining({ level: "focus", account: PROD, ref: TG }));
  });
});

describe("AWS resource search", () => {
  it("opens Focus on the chosen resource", async () => {
    vi.mocked(api.awsTopology).mockResolvedValue(accountsTopology());
    vi.mocked(api.awsTopologySearch).mockResolvedValue({
      enabled: true,
      capped: false,
      items: [{ ref: TG, name: "checkout-web", type: "elasticloadbalancing:targetgroup", service: "elbv2", account: PROD, region: "eu-west-1" }],
    });
    page("/aws/topology?level=accounts");
    await screen.findByText("Organization");
    fireEvent.change(screen.getByRole("searchbox", { name: "Search resources" }), { target: { value: "checkout" } });
    fireEvent.submit(screen.getByRole("search"));
    const option = await screen.findByRole("option", { name: /checkout-web/ });
    expect(api.awsTopologySearch).toHaveBeenCalledWith({ q: "checkout", account: undefined, region: undefined });
    expect(document.querySelector("[data-search-capped]")).toBeNull();
    fireEvent.click(within(option).getByRole("button"));
    await waitFor(() => expect(location()).toContain(`level=focus&account=${PROD}&ref=${encodeURIComponent(TG)}`));
  });

  it("says more than 20, refine when the server capped the results", async () => {
    vi.mocked(api.awsTopology).mockResolvedValue(accountsTopology());
    vi.mocked(api.awsTopologySearch).mockResolvedValue({
      enabled: true,
      capped: true,
      items: Array.from({ length: 20 }, (_, i) => ({ ref: `r${i}`, name: `checkout-${i}`, type: "s3:bucket", service: "s3", account: PROD, region: "eu-west-1" })),
    });
    page("/aws/topology?level=accounts");
    await screen.findByText("Organization");
    fireEvent.change(screen.getByRole("searchbox", { name: "Search resources" }), { target: { value: "checkout" } });
    fireEvent.submit(screen.getByRole("search"));
    expect((await found("[data-search-capped]")).textContent).toBe("More than 20, refine");
    expect(screen.getAllByRole("option")).toHaveLength(20);
  });

  it("does not search with fewer than two characters", async () => {
    vi.mocked(api.awsTopology).mockResolvedValue(accountsTopology());
    page("/aws/topology?level=accounts");
    await screen.findByText("Organization");
    fireEvent.change(screen.getByRole("searchbox", { name: "Search resources" }), { target: { value: "c" } });
    fireEvent.submit(screen.getByRole("search"));
    expect(api.awsTopologySearch).not.toHaveBeenCalled();
  });
});

describe("AWS Topology Focus details", () => {
  it("marks the node the likely cause changed, and shows the last change", async () => {
    vi.mocked(api.awsTopology).mockResolvedValue(focusTopology());
    page(`/aws/topology?level=focus&account=${PROD}&ref=${encodeURIComponent(TG)}`);
    await screen.findByText("checkout:42");
    const cause = document.querySelectorAll("[data-likely-cause]");
    const marked = [...cause].filter((e) => e.hasAttribute("data-node"));
    expect(marked.map((e) => e.getAttribute("data-node"))).toEqual(["td"]);
    expect(marked[0].textContent).toContain("Likely cause");
    const last = document.querySelector("[data-focus-panel] [data-last-change]") as HTMLElement;
    expect(last.textContent).toContain("Service checkout updated to task definition :42");
    expect(last.textContent).toContain("by github-actions-deploy");
  });

  it("says no change in the last day only when that is certain", async () => {
    const v = focusTopology();
    v.nodes = v.nodes.map((n) => (n.id === TG ? { ...n, last_change: undefined, last_change_known: true } : n));
    vi.mocked(api.awsTopology).mockResolvedValue(v);
    page(`/aws/topology?level=focus&account=${PROD}&ref=${encodeURIComponent(TG)}`);
    expect((await found('[data-last-change="none"]')).textContent).toBe("No change in the last day");
  });

  it("reads an unknown last change as not read, never as no change", async () => {
    const v = focusTopology();
    v.nodes = v.nodes.map((n) => (n.id === TG ? { ...n, last_change: undefined, last_change_known: false } : n));
    vi.mocked(api.awsTopology).mockResolvedValue(v);
    page(`/aws/topology?level=focus&account=${PROD}&ref=${encodeURIComponent(TG)}`);
    const panel = await found("[data-focus-panel]");
    expect([...panel.querySelectorAll("[data-not-read]")].map((e) => e.textContent)).toContain("Last change not read");
    expect(panel.textContent).not.toContain("No change");
    expect(panel.querySelector("[data-last-change]")).toBeNull();
  });

  it("names the accounts a link was seen from", async () => {
    vi.mocked(api.awsTopology).mockResolvedValue(focusTopology());
    page(`/aws/topology?level=focus&account=${PROD}&ref=${encodeURIComponent(TG)}`);
    expect((await found("[data-seen-from]")).textContent).toBe("prod, platform, both sides read");
  });

  it("labels Focus links in the mock-up words", async () => {
    vi.mocked(api.awsTopology).mockResolvedValue(focusTopology());
    page(`/aws/topology?level=focus&account=${PROD}&ref=${encodeURIComponent(TG)}`);
    await screen.findByText("checkout:42");
    const label = (from: string, to: string) =>
      [...document.querySelectorAll("[data-link]")].find((e) => e.getAttribute("data-link") === `${from}>${to}`)?.querySelector("text")?.textContent;
    expect(label("dns", TG)).toBe("Alias record");
    expect(label("lb", TG)).toBe("Listener rule");
    expect(label("cdn", "lb")).toBe("Origin hostname matches");
  });
});

describe("AWS Topology, Ungrouped moves and causes not drawn", () => {
  it("moves a grouped resource to Ungrouped", async () => {
    vi.mocked(api.awsTopology).mockImplementation(groupsAnswer());
    page(`/aws/topology?level=groups&account=${PROD}&sel=${SEL}`);
    const section = await found("[data-move-to-ungrouped]");
    const picker = within(section).getByRole("button", { name: "Resource to move to Ungrouped" }) as HTMLButtonElement;
    await waitFor(() => expect(picker.disabled).toBe(false));
    fireEvent.click(picker);
    fireEvent.mouseDown(await screen.findByRole("option", { name: "checkout-assets" }));
    fireEvent.click(within(section).getByRole("button", { name: "Move to Ungrouped" }));
    await waitFor(() =>
      expect(api.saveAwsGroupCorrection).toHaveBeenCalledWith(
        expect.objectContaining({ kind: "move", member_arn: "arn:aws:s3:::checkout-assets", target_group_id: "ungrouped" }),
      ),
    );
  });

  it("moves an ungrouped resource to a chosen group from the Ungrouped panel, which offers no rename or merge", async () => {
    vi.mocked(api.awsTopology).mockImplementation(groupsAnswer());
    page(`/aws/topology?level=groups&account=${PROD}&sel=${encodeURIComponent(`group:${PROD}/ungrouped`)}`);
    const panel = await found('[data-group-panel="ungrouped"]');
    expect(within(panel).queryByRole("button", { name: "Rename" })).toBeNull();
    expect(within(panel).queryByRole("button", { name: "Merge with" })).toBeNull();
    const picker = within(panel).getByRole("button", { name: "Resource to move" }) as HTMLButtonElement;
    await waitFor(() => expect(picker.disabled).toBe(false));
    fireEvent.click(picker);
    fireEvent.mouseDown(await screen.findByRole("option", { name: "checkout-receipts" }));
    fireEvent.click(within(panel).getByRole("button", { name: "Group to move to" }));
    fireEvent.mouseDown(await screen.findByRole("option", { name: "payments" }));
    fireEvent.click(within(panel).getByRole("button", { name: "Move" }));
    await waitFor(() =>
      expect(api.saveAwsGroupCorrection).toHaveBeenCalledWith(
        expect.objectContaining({ kind: "move", member_arn: "arn:aws:s3:::checkout-receipts", target_group_id: "tag:service=payments" }),
      ),
    );
  });

  it("keeps the likely cause in the panel only when its node is not drawn", async () => {
    const v = focusTopology();
    v.nodes = v.nodes.map((n) =>
      n.id === TG && n.top_issue ? { ...n, top_issue: { ...n.top_issue, cause_ref: "arn:aws:ecs:eu-west-1:333344445555:task-definition/other:7" } } : n,
    );
    vi.mocked(api.awsTopology).mockResolvedValue(v);
    page(`/aws/topology?level=focus&account=${PROD}&ref=${encodeURIComponent(TG)}`);
    await screen.findByText("checkout:42");
    expect([...document.querySelectorAll("[data-node][data-likely-cause]")]).toHaveLength(0);
    expect(document.querySelector("[data-focus-panel] [data-likely-cause]")?.textContent).toContain("task definition :42");
  });

  it("draws no chip for an issue stored before causes had a ref", async () => {
    const v = focusTopology();
    v.nodes = v.nodes.map((n) => (n.id === TG && n.top_issue ? { ...n, top_issue: { ...n.top_issue, cause_ref: undefined } } : n));
    vi.mocked(api.awsTopology).mockResolvedValue(v);
    page(`/aws/topology?level=focus&account=${PROD}&ref=${encodeURIComponent(TG)}`);
    await screen.findByText("checkout:42");
    expect([...document.querySelectorAll("[data-node][data-likely-cause]")]).toHaveLength(0);
  });
});

describe("AWS Topology group link words", () => {
  it("names a group link read by two calls with each count", async () => {
    const v = groupsTopology();
    v.links = v.links.map((l) =>
      l.from === `group:${PROD}/tag:app=checkout` && l.to === `group:${PROD}/stack:baseline`
        ? { ...l, count: 3, sources: { "kms:GetKeyPolicy": 2, "ecr:GetRepositoryPolicy": 1 } }
        : l,
    );
    vi.mocked(api.awsTopology).mockImplementation(groupsAnswer(v));
    page(`/aws/topology?level=groups&account=${PROD}`);
    await screen.findByText("checkout");
    const link = [...document.querySelectorAll("[data-link]")].find(
      (e) => e.getAttribute("data-link") === `group:${PROD}/tag:app=checkout>group:${PROD}/stack:baseline`,
    ) as Element;
    expect(link.querySelector("text")?.textContent).toBe("KMS key 2, ECR repository 1");
  });
});

describe("AWS Topology view placement", () => {
  it("opens a Focus map too wide for its view centred on the resource, and again after a hops change", async () => {
    const v = focusTopology();
    const plan = layoutFocus(v.nodes, v.links, TG);
    // A view narrower than the map can show at the smallest readable scale.
    const viewWidth = Math.round(plan.width * MIN_SCALE * 0.5);
    const width = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientWidth");
    Object.defineProperty(HTMLElement.prototype, "clientWidth", { configurable: true, get: () => viewWidth });
    try {
      vi.mocked(api.awsTopology).mockResolvedValue(v);
      page(`/aws/topology?level=focus&account=${PROD}&ref=${encodeURIComponent(TG)}`);
      await screen.findByText("checkout:42");
      const at = fitView(plan, { width: viewWidth, height: mapHeight() });
      expect(at.target.id).toBe(TG);
      const canvas = document.querySelector("[data-canvas]") as HTMLElement;
      expect(canvas.dataset.fit).toBe(TG);
      expect(canvas.scrollLeft).toBe(at.left);
      expect(at.left).toBeGreaterThan(0);
      // The resource sits fully in view with the margin: never cut at the left edge.
      const root = plan.boxes.get(TG)!;
      expect(root.x * at.scale - canvas.scrollLeft).toBeGreaterThanOrEqual(0);
      canvas.scrollLeft = 0;
      vi.mocked(api.awsTopology).mockResolvedValue({ ...v, hops: 3 });
      fireEvent.click(within(screen.getByRole("group", { name: "Hops" })).getByRole("button", { name: "3" }));
      await waitFor(() => expect(api.awsTopology).toHaveBeenLastCalledWith(expect.objectContaining({ hops: 3 })));
      await waitFor(() => expect((document.querySelector("[data-canvas]") as HTMLElement).scrollLeft).toBe(at.left));
    } finally {
      if (width) Object.defineProperty(HTMLElement.prototype, "clientWidth", width);
    }
  });

  it("zooms by hand and fits again from the Fit button", async () => {
    vi.mocked(api.awsTopology).mockResolvedValue(focusTopology());
    page(`/aws/topology?level=focus&account=${PROD}&ref=${encodeURIComponent(TG)}`);
    await screen.findByText("checkout:42");
    const canvas = () => document.querySelector("[data-canvas]") as HTMLElement;
    const zoom = screen.getByRole("group", { name: "Zoom" });
    const before = Number(canvas().dataset.scale);
    fireEvent.click(within(zoom).getByRole("button", { name: "Zoom out" }));
    await waitFor(() => expect(Number(canvas().dataset.scale)).toBeCloseTo(before - 0.1, 5));
    fireEvent.click(within(zoom).getByRole("button", { name: "Fit the map to the view" }));
    await waitFor(() => expect(Number(canvas().dataset.scale)).toBe(before));
  });

  it("opens in the List view at phone width unless the Map is asked for", async () => {
    const before = window.matchMedia;
    Object.defineProperty(window, "matchMedia", { configurable: true, value: (q: string) => ({ matches: q.includes("max-width: 639px"), addEventListener: () => {}, removeEventListener: () => {} }) });
    try {
      vi.mocked(api.awsTopology).mockImplementation(groupsAnswer());
      page(`/aws/topology?level=groups&account=${PROD}`);
      await screen.findByText("checkout");
      expect(document.querySelector("[data-list]")).not.toBeNull();
      expect(document.querySelector("[data-canvas]")).toBeNull();
      fireEvent.click(within(screen.getByRole("group", { name: "View" })).getByRole("button", { name: "Map" }));
      await waitFor(() => expect(location()).toContain("view=map"));
      expect(await found("[data-canvas]")).toBeTruthy();
    } finally {
      Object.defineProperty(window, "matchMedia", { configurable: true, value: before });
    }
  });

  it("names a not-read account in plain words on the Accounts map", async () => {
    const v = accountsTopology();
    v.nodes = v.nodes.map((n) => (n.account === DATA ? { ...n, read: "not_read", not_read_reason: "assume_role_failed" } : n));
    vi.mocked(api.awsTopology).mockResolvedValue(v);
    page("/aws/topology?level=accounts");
    await screen.findByText("Organization");
    const card = document.querySelector(`[data-account-card="${DATA}"]`) as HTMLElement;
    expect(card.textContent).toContain("Read role could not be assumed");
    expect(card.textContent).not.toContain("assume_role_failed");
  });

  it("shows an unknown account's whole ID, so two with one prefix stay apart", async () => {
    const v = accountsTopology();
    v.nodes = [...v.nodes, { id: "account:889900009999", kind: "account", name: "889900009999", account: "889900009999", label: "unknown" }];
    vi.mocked(api.awsTopology).mockResolvedValue(v);
    page("/aws/topology?level=accounts");
    await screen.findByText("Organization");
    expect(document.querySelector(`[data-account-card="${UNKNOWN}"]`)?.textContent).toContain("8899-0000-1234");
    expect(document.querySelector('[data-account-card="889900009999"]')?.textContent).toContain("8899-0000-9999");
  });
});

describe("AWS Topology first read", () => {
  it("shows an account in its first read with lower-bound counts and the reason", async () => {
    const v = accountsTopology();
    v.nodes = v.nodes.map((n) => (n.account === DATA ? { ...n, read: "partial", not_read_reason: "first_read_in_progress" } : n));
    vi.mocked(api.awsTopology).mockResolvedValue({ ...v, regions: ["eu-west-1", "us-east-1"] });
    page("/aws/topology?level=accounts");
    await screen.findByText("Organization");
    const card = document.querySelector(`[data-account-card="${DATA}"]`) as HTMLElement;
    expect(card.textContent).toContain("more than 900 resources, not all read");
    expect(card.textContent).toContain("First read in progress");
    expect(screen.getByRole("button", { name: "Regions" }).textContent).toContain("2 regions");
  });
});

describe("AWS Topology opening scroll and zoom control", () => {
  /** The settled estate as the server answers it. */
  function settledAnswer() {
    const ids = ["network", "prod", "platform", "data", "sandbox"];
    const node = (n: string, i: number) => ({
      id: `account:12340000000${i}`,
      kind: "account" as const,
      name: n,
      account: `12340000000${i}`,
      label: "connected" as const,
      role: n === "network" ? ("hub" as const) : ("spoke" as const),
      read: n === "sandbox" ? ("not_read" as const) : ("read" as const),
      resources: n === "sandbox" ? undefined : 60 + i,
    });
    const id = (n: string) => `account:12340000000${ids.indexOf(n)}`;
    const others = Array.from({ length: 9 }, (_, i) => ({ id: `account:99990000000${i}`, kind: "account" as const, name: `99990000000${i}`, account: `99990000000${i}`, label: "unknown" as const }));
    const link = (from: string, to: string, type: string, seen: "both" | "one") => ({ from, to, type, kind: "configured" as const, count: 2, both_sides: seen === "both" ? 2 : 0, seen_from: seen });
    return {
      ...accountsTopology(),
      nodes: [...ids.map(node), { id: "account:111100001111", kind: "account" as const, name: "Management", account: "111100001111", label: "same_org" as const }, ...others],
      links: [
        ...["prod", "platform", "data"].map((s) => link(id(s), id("network"), "attached_to_transit", "both")),
        link(id("platform"), id("prod"), "alias_of", "both"),
        ...["network", "prod", "platform", "data"].flatMap((s, si) => [0, 1, 2, 3, 4].map((k) => link(id(s), others[(si * 2 + k) % 9].id, "trusts", "one"))),
      ],
    };
  }

  it.each(["dark", "light"])("opens the settled estate at 342 px (%s) with the Organization heading in view", async (theme) => {
    document.documentElement.dataset.theme = theme;
    const width = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientWidth");
    const innerHeight = window.innerHeight;
    Object.defineProperty(HTMLElement.prototype, "clientWidth", { configurable: true, get: () => 342 });
    Object.defineProperty(window, "innerHeight", { configurable: true, value: 800 });
    try {
      vi.mocked(api.awsTopology).mockResolvedValue(settledAnswer());
      page("/aws/topology?level=accounts&view=map");
      const heading = await found('[data-heading="organization"]');
      const canvas = document.querySelector("[data-canvas]") as HTMLElement;
      await waitFor(() => expect(canvas.dataset.scale).toBeDefined());
      const scale = Number(canvas.dataset.scale);
      const left = parseFloat(heading.style.left) * scale - canvas.scrollLeft;
      const top = parseFloat(heading.style.top) * scale - canvas.scrollTop;
      expect(left).toBeGreaterThanOrEqual(0);
      expect(top).toBeGreaterThanOrEqual(0);
      expect(left + 120 * scale).toBeLessThanOrEqual(342);
      expect(top + 12 * scale).toBeLessThanOrEqual(600);
    } finally {
      if (width) Object.defineProperty(HTMLElement.prototype, "clientWidth", width);
      Object.defineProperty(window, "innerHeight", { configurable: true, value: innerHeight });
      delete document.documentElement.dataset.theme;
    }
  });

  it("keeps the zoom control out of the drawing area, so no card sits under it", async () => {
    vi.mocked(api.awsTopology).mockResolvedValue(focusTopology());
    page(`/aws/topology?level=focus&account=${PROD}&ref=${encodeURIComponent(TG)}`);
    await screen.findByText("checkout:42");
    const zoom = screen.getByRole("group", { name: "Zoom" });
    const canvas = document.querySelector("[data-canvas]") as HTMLElement;
    expect(canvas.contains(zoom)).toBe(false);
    // The control's row comes before the map and is laid out in the flow, not over it.
    const row = zoom.closest("[data-zoom-row]") as HTMLElement;
    expect(row.compareDocumentPosition(canvas) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(row.className).not.toMatch(/\babsolute\b/);
    expect(zoom.className).not.toMatch(/\babsolute\b/);
  });
});
