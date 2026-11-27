// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { api, ApiError } from "@/lib/api";
import type { TopologyNode, TopologyQuery } from "@/lib/awsEstate";
import { GroupPanel } from "./GroupPanel";
import { renderAwsPage } from "../shared/renderAwsPage";
import { correction, groupsTopology, PROD, ungroupedTopology } from "../shared/estateFixtures";

// The group side panel's writes: every save starts from the correction it
// read; only a 409 correction_conflict asks for a reload, any other
// refusal is named as it is; moves go both ways between a group and
// Ungrouped; Ungrouped itself is never renamed, merged into, or merged.

vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      awsTopology: vi.fn(),
      awsGroupCorrections: vi.fn(),
      saveAwsGroupCorrection: vi.fn(),
      deleteAwsGroupCorrection: vi.fn(),
    },
  };
});

const groups = groupsTopology().nodes.filter((n) => n.kind === "group");
const group = (id: string) => groups.find((g) => g.group === id) as TopologyNode;
const CHECKOUT_MEMBER = "arn:aws:s3:::checkout-assets";

beforeEach(() => {
  Element.prototype.scrollIntoView = vi.fn();
  vi.mocked(api.awsGroupCorrections).mockResolvedValue({ enabled: true, corrections: [] });
  vi.mocked(api.saveAwsGroupCorrection).mockResolvedValue({ correction: correction() });
  vi.mocked(api.awsTopology).mockImplementation(async (q: TopologyQuery) => {
    if (q.group === "ungrouped") return ungroupedTopology();
    return {
      ...groupsTopology(),
      group: q.group,
      nodes: [{ id: CHECKOUT_MEMBER, kind: "resource" as const, name: "checkout-assets", account: PROD, type: "s3:bucket", group: q.group }],
      links: [],
    };
  });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function panel(id: string, canWrite = true) {
  renderAwsPage(<GroupPanel account={PROD} group={group(id)} groups={groups} canWrite={canWrite} />, "/aws/topology");
  return waitFor(() => {
    const el = document.querySelector(`[data-group-panel="${id}"]`);
    if (!el) throw new Error("no panel");
    return el as HTMLElement;
  });
}

async function choose(root: HTMLElement, picker: string, option: string | RegExp) {
  const button = within(root).getByRole("button", { name: picker }) as HTMLButtonElement;
  await waitFor(() => expect(button.disabled).toBe(false));
  fireEvent.click(button);
  fireEvent.mouseDown(await screen.findByRole("option", { name: option }));
}

async function mergeIntoPayments(root: HTMLElement) {
  await choose(root, "Merge with", "payments");
  fireEvent.click(within(root).getByRole("button", { name: "Merge" }));
}

describe("Group corrections, conflicts", () => {
  it("asks for a reload when the server refuses a merge with correction_conflict", async () => {
    const read = correction({ kind: "merge", group_id: "tag:app=checkout", target_group_id: "stack:baseline", name: undefined, created_at: "2026-11-25T08:00:00Z" });
    vi.mocked(api.awsGroupCorrections).mockResolvedValue({ enabled: true, corrections: [read] });
    vi.mocked(api.saveAwsGroupCorrection).mockRejectedValue(new ApiError(409, "changed by someone else; reload", { code: "correction_conflict", error: "changed by someone else; reload" }));
    const root = await panel("tag:app=checkout");
    await mergeIntoPayments(root);
    await waitFor(() =>
      expect(api.saveAwsGroupCorrection).toHaveBeenCalledWith(
        expect.objectContaining({ kind: "merge", group_id: "tag:app=checkout", target_group_id: "tag:service=payments", expected_created_at: "2026-11-25T08:00:00Z" }),
      ),
    );
    const alert = await within(root).findByRole("alert");
    expect(alert.textContent).toMatch(/changed by someone else/i);
    expect(within(alert).getByRole("button", { name: "Reload" })).toBeTruthy();
    expect(root.querySelector("[data-failure]")).toBeNull();
  });

  it("asks for a reload when the server refuses a move with correction_conflict", async () => {
    vi.mocked(api.saveAwsGroupCorrection).mockRejectedValue(new ApiError(409, "changed", { code: "correction_conflict" }));
    const root = await panel("tag:app=checkout");
    await choose(root, "Resource to move", "checkout-receipts");
    fireEvent.click(within(root).getByRole("button", { name: "Move" }));
    expect((await within(root).findByRole("alert")).textContent).toMatch(/changed by someone else/i);
    expect(root.querySelector("[data-conflict]")).not.toBeNull();
  });

  it.each([
    ["a 409 with no code", new ApiError(409, "conflict")],
    ["a 409 with another code", new ApiError(409, "group is inherited", { code: "correction_inherited" })],
    ["a 409 whose body is text", new ApiError(409, "proxy said no", "correction_conflict")],
    ["a server error", new ApiError(500, "aws group correction could not be saved")],
    ["a network failure", new TypeError("Failed to fetch")],
  ])("names %s as a failure, never as changed by someone else", async (_name, error) => {
    vi.mocked(api.saveAwsGroupCorrection).mockRejectedValue(error);
    const root = await panel("tag:app=checkout");
    await mergeIntoPayments(root);
    const alert = await within(root).findByRole("alert");
    expect(root.querySelector("[data-conflict]")).toBeNull();
    expect(alert.textContent).not.toMatch(/someone else/i);
    expect(root.querySelector("[data-failure]")).not.toBeNull();
  });

  it("clears the conflict and reads the corrections again on Reload", async () => {
    vi.mocked(api.saveAwsGroupCorrection).mockRejectedValue(new ApiError(409, "changed", { code: "correction_conflict" }));
    const root = await panel("tag:app=checkout");
    await mergeIntoPayments(root);
    const alert = await within(root).findByRole("alert");
    const reads = vi.mocked(api.awsGroupCorrections).mock.calls.length;
    fireEvent.click(within(alert).getByRole("button", { name: "Reload" }));
    await waitFor(() => expect(root.querySelector("[data-conflict]")).toBeNull());
    await waitFor(() => expect(vi.mocked(api.awsGroupCorrections).mock.calls.length).toBeGreaterThan(reads));
  });
});

describe("Group corrections, Ungrouped", () => {
  it("offers no rename or merge on Ungrouped, and never offers Ungrouped as a merge or move target", async () => {
    const root = await panel("ungrouped");
    expect(within(root).queryByRole("button", { name: "Rename" })).toBeNull();
    expect(within(root).queryByRole("button", { name: "Merge with" })).toBeNull();
    expect(within(root).queryByRole("textbox", { name: "Group name" })).toBeNull();
    await choose(root, "Resource to move", "checkout-receipts");
    fireEvent.click(within(root).getByRole("button", { name: "Group to move to" }));
    const targets = (await screen.findAllByRole("option")).map((o) => o.textContent);
    expect(targets).not.toContain("Ungrouped");
    expect(targets).toContain("checkout");
  });

  it("never offers Ungrouped or the group itself as a merge target", async () => {
    const root = await panel("tag:app=checkout");
    const button = within(root).getByRole("button", { name: "Merge with" }) as HTMLButtonElement;
    await waitFor(() => expect(button.disabled).toBe(false));
    fireEvent.click(button);
    const options = (await screen.findAllByRole("option")).map((o) => o.textContent);
    expect(options).not.toContain("Ungrouped");
    expect(options).not.toContain("checkout");
    expect(options).toContain("payments");
  });

  it("moves a resource out of a group to Ungrouped with the target ungrouped", async () => {
    const root = await panel("tag:app=checkout");
    const section = root.querySelector("[data-move-to-ungrouped]") as HTMLElement;
    await choose(section, "Resource to move to Ungrouped", "checkout-assets");
    fireEvent.click(within(section).getByRole("button", { name: "Move to Ungrouped" }));
    await waitFor(() =>
      expect(api.saveAwsGroupCorrection).toHaveBeenCalledWith(
        expect.objectContaining({ account_id: PROD, kind: "move", member_arn: CHECKOUT_MEMBER, target_group_id: "ungrouped" }),
      ),
    );
  });

  it("moves a resource out of Ungrouped into the chosen group", async () => {
    const root = await panel("ungrouped");
    await choose(root, "Resource to move", "checkout-receipts");
    await choose(root, "Group to move to", "baseline");
    fireEvent.click(within(root).getByRole("button", { name: "Move" }));
    await waitFor(() =>
      expect(api.saveAwsGroupCorrection).toHaveBeenCalledWith(
        expect.objectContaining({ account_id: PROD, kind: "move", member_arn: "arn:aws:s3:::checkout-receipts", target_group_id: "stack:baseline" }),
      ),
    );
  });

  it("sends the move's created_at when the member was moved before", async () => {
    vi.mocked(api.awsGroupCorrections).mockResolvedValue({
      enabled: true,
      corrections: [correction({ kind: "move", group_id: undefined, name: undefined, member_arn: "arn:aws:s3:::checkout-receipts", target_group_id: "ungrouped", created_at: "2026-11-24T07:00:00Z" })],
    });
    const root = await panel("tag:app=checkout");
    await choose(root, "Resource to move", "checkout-receipts");
    fireEvent.click(within(root).getByRole("button", { name: "Move" }));
    await waitFor(() =>
      expect(api.saveAwsGroupCorrection).toHaveBeenCalledWith(expect.objectContaining({ kind: "move", expected_created_at: "2026-11-24T07:00:00Z" })),
    );
  });

  it("disables every write for a role that cannot change groups", async () => {
    const root = await panel("tag:app=checkout", false);
    await waitFor(() => expect(api.awsGroupCorrections).toHaveBeenCalled());
    for (const name of ["Rename", "Merge with", "Resource to move", "Resource to move to Ungrouped"]) {
      expect((within(root).getByRole("button", { name }) as HTMLButtonElement).disabled, name).toBe(true);
    }
  });
});
