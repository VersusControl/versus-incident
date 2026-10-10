// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api, type KubernetesClusterSummary } from "@/lib/api";
import { kubernetesKey } from "@/lib/useKubernetesCluster";
import { KubernetesFleet } from "./KubernetesFleet";
import { MemberClusterPermissions, MembersPanel } from "./MembersPage";
import { AdminMembersControl } from "@/components/AdminMembersControl";

vi.mock("@/components/TopBar", () => ({ TopBar: () => null }));
vi.mock("@/lib/useEffectiveRole", () => ({ useEffectiveRole: () => ({ enterprise: true, hasSession: true, isAdmin: true, org: "deployment", session: { data: { email: "admin@example.test" } } }) }));
vi.mock("@/components/toastContext", () => ({ useToast: () => ({ push: vi.fn() }) }));
afterEach(() => { cleanup(); vi.restoreAllMocks(); });
const summary = (id: string, health: KubernetesClusterSummary["health"]): KubernetesClusterSummary => ({ id, health, provider: "generic", nodes: 2, ready_nodes: 2, pods: 4, running_pods: 4, warnings: 0, sync: { state: "live" }, observed_at: "2026-10-09T12:00:00Z" });

describe("Kubernetes fleet", () => {
  it("orders unhealthy cards first, preserves missing evidence, and scopes links", () => {
    const retry = vi.fn();
    render(<MemoryRouter><KubernetesFleet clusters={[summary("healthy", "healthy"), summary("offline", "unreachable"), summary("partial", "partial")]} retry={retry} retrying={false} /></MemoryRouter>);
    expect(screen.getAllByRole("article").map((card) => card.getAttribute("aria-label"))).toEqual(["Cluster offline", "Cluster partial", "Cluster healthy"]);
    expect(screen.getByRole("link", { name: "Open cluster healthy" }).getAttribute("href")).toBe("/?cluster=healthy");
    expect(screen.getAllByText(/Version unavailable/)).toHaveLength(3);
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(retry).toHaveBeenCalledOnce();
  });

  it("separates cluster query and invalidation prefixes", () => {
    expect(kubernetesKey("one", "overview")).toEqual(["kubernetes", "one", "overview"]);
    expect(kubernetesKey("two")).toEqual(["kubernetes", "two"]);
    expect(kubernetesKey(undefined)).toEqual(["kubernetes", "_"]);
  });

  it("keeps rows horizontally scrollable and avoids readiness meters for an unreachable cluster", () => {
    render(<MemoryRouter><KubernetesFleet clusters={[summary("offline", "unreachable"), { ...summary("healthy", "healthy"), issues: 0 }]} retry={vi.fn()} retrying /></MemoryRouter>);
    const rows = screen.getByRole("region", { name: "Cluster rows" });
    expect(rows.tabIndex).toBe(0);
    expect(rows.className).toContain("overflow-x-auto");
    expect(screen.getAllByRole("meter")).toHaveLength(2);
    expect(screen.getByRole("button", { name: "Retry" }).hasAttribute("disabled")).toBe(true);
    expect(screen.getByRole("link", { name: "Review issues" }).getAttribute("href")).toBe("/?cluster=healthy&view=issues");
  });
});

describe("Member cluster permissions", () => {
  it("treats omitted scope as unrestricted and prevents an empty restricted grant", () => {
    const save = vi.fn();
    render(<MemberClusterPermissions memberId="alice" clusters={[{ id: "one" }, { id: "two" }]} canManage pending={false} onSave={save} />);
    fireEvent.click(screen.getByRole("button", { name: "Cluster permissions for alice" }));
    fireEvent.click(screen.getByRole("checkbox", { name: "All clusters" }));
    expect(screen.getByRole("button", { name: "Save" }).hasAttribute("disabled")).toBe(true);
    fireEvent.click(screen.getByRole("checkbox", { name: "two" }));
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(save).toHaveBeenCalledWith(["two"]);
  });

  it("shows restricted scope read-only to non-managers", () => {
    render(<MemberClusterPermissions memberId="alice" clusters={[{ id: "one", display_name: "Production" }]} selected={["one"]} effective={["one"]} canManage={false} pending={false} onSave={vi.fn()} />);
    expect(screen.getByText("Effective access: Production")).toBeTruthy();
    expect(screen.queryByRole("button")).toBeNull();
  });

  it.each([
    { effective: [], label: "No clusters" },
    { effective: null, label: "All clusters" },
    { effective: undefined, label: "Unavailable" },
  ])("distinguishes effective scope from an unrestricted direct assignment: $label", ({ effective, label }) => {
    const save = vi.fn();
    render(<MemberClusterPermissions memberId="alice" clusters={[]} selected={[]} effective={effective} canManage pending={false} onSave={save} />);
    expect(screen.getByText(`Effective access: ${label}`)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Cluster permissions for alice" }));
    expect((screen.getByRole("checkbox", { name: "All clusters" }) as HTMLInputElement).checked).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(save).toHaveBeenCalledWith([]);
  });

  it("preserves unseen direct IDs when saving unchanged or adding a visible cluster", () => {
    const save = vi.fn();
    const unseen = "<private-cluster>";
    render(<MemberClusterPermissions memberId="alice" clusters={[{ id: "one", display_name: "Production" }]} selected={[unseen]} effective={[unseen]} canManage pending={false} onSave={save} />);
    fireEvent.click(screen.getByRole("button", { name: "Cluster permissions for alice" }));
    expect((screen.getByRole("checkbox", { name: `${unseen} (not in visible catalog)` }) as HTMLInputElement).checked).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(save).toHaveBeenLastCalledWith([unseen]);
    fireEvent.click(screen.getByRole("button", { name: "Cluster permissions for alice" }));
    fireEvent.click(screen.getByRole("checkbox", { name: "Production" }));
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(save).toHaveBeenLastCalledWith([unseen, "one"]);
  });

  it("requires an explicit edit to remove an unseen ID or grant all clusters", () => {
    const save = vi.fn();
    render(<MemberClusterPermissions memberId="alice" clusters={[{ id: "one" }]} selected={["hidden"]} effective={[]} canManage pending={false} onSave={save} />);
    fireEvent.click(screen.getByRole("button", { name: "Cluster permissions for alice" }));
    fireEvent.click(screen.getByRole("checkbox", { name: "hidden (not in visible catalog)" }));
    expect(screen.getByRole("button", { name: "Save" }).hasAttribute("disabled")).toBe(true);
    fireEvent.click(screen.getByRole("checkbox", { name: "one" }));
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(save).toHaveBeenLastCalledWith(["one"]);
    fireEvent.click(screen.getByRole("button", { name: "Cluster permissions for alice" }));
    fireEvent.click(screen.getByRole("checkbox", { name: "All clusters" }));
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(save).toHaveBeenLastCalledWith([]);
  });

  it("omits cluster scope for Admin role-only changes even with denied effective access", async () => {
    vi.spyOn(api, "listRbacMembers").mockResolvedValue({ org: "deployment", members: [{ subject: "alice-sub", email: "alice@example.test", role: "responder", clusters: [] }] });
    vi.spyOn(api, "getBootstrapAdmin").mockResolvedValue({ configured: false, enabled: false, can_disable: false });
    const save = vi.spyOn(api, "setMemberRole").mockResolvedValue({ org: "deployment", subject: "alice-sub", role: "admin" });
    render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><AdminMembersControl /></QueryClientProvider>);
    fireEvent.change(await screen.findByRole("combobox", { name: "Role for alice@example.test" }), { target: { value: "admin" } });
    await waitFor(() => expect(save).toHaveBeenCalledWith("deployment", "alice-sub", "admin"));
  });

  it("preserves the direct assignment scope when a role changes despite broader team access", async () => {
    vi.spyOn(api, "listMembers").mockResolvedValue([{ id: "alice", name: "Alice", alias: "alice", meta: { email: "alice@example.test" } }]);
    vi.spyOn(api, "listRbacMembers").mockResolvedValue({ org: "deployment", members: [{ subject: "alice-sub", email: "alice@example.test", role: "admin", clusters: ["one", "two"] }] });
    vi.spyOn(api, "listMemberRoles").mockResolvedValue({ assignments: [{ subject: "alice-sub", role: "responder", clusters: ["one"] }] });
    vi.spyOn(api, "kubernetesClusters").mockResolvedValue({ multiple: true, clusters: [summary("one", "healthy"), summary("two", "healthy")] });
    const save = vi.spyOn(api, "setMemberRole").mockResolvedValue({ org: "deployment", subject: "alice-sub", role: "admin", clusters: ["one"] });
    render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><MembersPanel /></QueryClientProvider>);
    await screen.findByText("Effective access: one, two");
    const role = await screen.findByRole("combobox", { name: "Role for alice-sub" });
    fireEvent.change(role, { target: { value: "admin" } });
    await waitFor(() => expect(save).toHaveBeenCalledWith("deployment", "alice-sub", "admin", ["one"]));
  });

  it("preserves effective restricted IDs when creating a direct role or saving its scope", async () => {
    vi.spyOn(api, "listMembers").mockResolvedValue([{ id: "alice", name: "Alice", alias: "alice", meta: { email: "alice@example.test" } }]);
    vi.spyOn(api, "listRbacMembers").mockResolvedValue({ org: "deployment", members: [{ subject: "alice-sub", email: "alice@example.test", role: "responder", clusters: ["one", "hidden"] }] });
    vi.spyOn(api, "listMemberRoles").mockResolvedValue({ assignments: [] });
    vi.spyOn(api, "kubernetesClusters").mockResolvedValue({ multiple: true, clusters: [summary("one", "healthy")] });
    const save = vi.spyOn(api, "setMemberRole").mockResolvedValue({ org: "deployment", subject: "alice-sub", role: "viewer", clusters: ["one", "hidden"] });
    render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><MembersPanel /></QueryClientProvider>);
    const role = await screen.findByRole("combobox", { name: "Role for alice-sub" });
    fireEvent.change(role, { target: { value: "viewer" } });
    await waitFor(() => expect(save).toHaveBeenCalledWith("deployment", "alice-sub", "viewer", ["one", "hidden"]));
    await waitFor(() => expect(screen.getByRole("button", { name: "Cluster permissions for alice" }).hasAttribute("disabled")).toBe(false));
    fireEvent.click(screen.getByRole("button", { name: "Cluster permissions for alice" }));
    expect((screen.getByRole("checkbox", { name: "All clusters" }) as HTMLInputElement).checked).toBe(false);
    expect((screen.getByRole("checkbox", { name: "hidden (not in visible catalog)" }) as HTMLInputElement).checked).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(save).toHaveBeenLastCalledWith("deployment", "alice-sub", "responder", ["one", "hidden"]));
  });

  it("requires an explicit All choice for a member with no direct role and no effective clusters", async () => {
    vi.spyOn(api, "listMembers").mockResolvedValue([{ id: "alice", name: "Alice", alias: "alice", meta: { email: "alice@example.test" } }]);
    vi.spyOn(api, "listRbacMembers").mockResolvedValue({ org: "deployment", members: [{ subject: "alice-sub", email: "alice@example.test", role: "responder", clusters: [] }] });
    vi.spyOn(api, "listMemberRoles").mockResolvedValue({ assignments: [] });
    vi.spyOn(api, "kubernetesClusters").mockResolvedValue({ multiple: true, clusters: [summary("one", "healthy")] });
    const save = vi.spyOn(api, "setMemberRole").mockResolvedValue({ org: "deployment", subject: "alice-sub", role: "responder", clusters: [] });
    render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><MembersPanel /></QueryClientProvider>);
    fireEvent.click(await screen.findByRole("button", { name: "Cluster permissions for alice" }));
    expect(screen.queryByRole("combobox", { name: "Role for alice-sub" })).toBeNull();
    expect((screen.getByRole("checkbox", { name: "All clusters" }) as HTMLInputElement).checked).toBe(false);
    expect(screen.getByRole("button", { name: "Save" }).hasAttribute("disabled")).toBe(true);
    expect(save).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("checkbox", { name: "All clusters" }));
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(save).toHaveBeenCalledWith("deployment", "alice-sub", "responder", []));
  });

  it("keeps single-cluster role changes scope-free even with no effective clusters", async () => {
    vi.spyOn(api, "listMembers").mockResolvedValue([{ id: "alice", name: "Alice", alias: "alice", meta: { email: "alice@example.test" } }]);
    vi.spyOn(api, "listRbacMembers").mockResolvedValue({ org: "deployment", members: [{ subject: "alice-sub", email: "alice@example.test", role: "responder", clusters: [] }] });
    const assignments = vi.spyOn(api, "listMemberRoles");
    vi.spyOn(api, "kubernetesClusters").mockResolvedValue({ multiple: false, clusters: [summary("one", "healthy")] });
    const save = vi.spyOn(api, "setMemberRole").mockResolvedValue({ org: "deployment", subject: "alice-sub", role: "viewer" });
    render(<QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}><MembersPanel /></QueryClientProvider>);
    fireEvent.change(await screen.findByRole("combobox", { name: "Role for alice-sub" }), { target: { value: "viewer" } });
    await waitFor(() => expect(save).toHaveBeenCalledWith("deployment", "alice-sub", "viewer", undefined));
    expect(assignments).not.toHaveBeenCalled();
    expect(screen.queryByRole("button", { name: "Cluster permissions for alice" })).toBeNull();
  });
});