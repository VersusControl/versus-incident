// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api, type KubernetesClusterSummary, type KubernetesSyncHealthReport, type KubernetesSyncProgress } from "@/lib/api";
import { kubernetesKey } from "@/lib/useKubernetesCluster";
import { KubernetesFleet } from "./KubernetesFleet";
import { MemberClusterPermissions, MembersPanel } from "./MembersPage";
import { AdminMembersControl } from "@/components/AdminMembersControl";

vi.mock("@/components/TopBar", () => ({ TopBar: () => null }));
vi.mock("@/lib/useEffectiveRole", () => ({ useEffectiveRole: () => ({ enterprise: true, hasSession: true, isAdmin: true, org: "deployment", session: { data: { email: "admin@example.test" } } }) }));
vi.mock("@/components/toastContext", () => ({ useToast: () => ({ push: vi.fn() }) }));
afterEach(() => { cleanup(); vi.restoreAllMocks(); });
const summary = (id: string, health: KubernetesClusterSummary["health"]): KubernetesClusterSummary => ({ id, health, provider: "generic", nodes: 2, ready_nodes: 2, pods: 4, running_pods: 4, warnings: 0, sync: { state: "live", age_s: 0, partial: false }, observed_at: "2026-10-09T12:00:00Z" });

// A cluster the server reports mid first sync: its numbers are placeholders.
const syncing = (id: string, progress?: KubernetesSyncProgress): KubernetesClusterSummary => ({
  ...summary(id, "syncing"),
  nodes: 0,
  ready_nodes: 0,
  pods: 0,
  running_pods: 0,
  sync: { state: "syncing", age_s: 0, partial: false, progress },
});

const firstProgress: KubernetesSyncProgress = {
  step: "listing",
  step_index: 2,
  step_total: 4,
  steps_done: ["connected"],
  counts: { Pod: 3214, Deployment: 412, Namespace: 27, Node: 12 },
  kinds_done: 4,
  kinds_total: 20,
  started_at: "2026-10-25T08:00:00Z",
  updated_at: "2026-10-25T08:00:30Z",
};

// Every row reads its sync state from the cluster's complete report, so a test
// cluster is given the report its envelope implies unless it says otherwise.
const reportFor = (cluster: KubernetesClusterSummary): KubernetesSyncHealthReport => ({
  state: cluster.sync.state,
  partial: cluster.sync.partial,
  age_s: cluster.sync.age_s,
  kinds: { Pod: { state: cluster.sync.state === "ready" || cluster.sync.state === "live" ? "ready" : "pending", observed_at: "2026-10-09T12:00:00Z", records: cluster.pods, partial: false } },
  total_kinds: 1,
  ready_kinds: 1,
  failing_kinds: 0,
  pending_kinds: 0,
  truncated: false,
});
const readyReport = (records = 4): KubernetesSyncHealthReport => ({ state: "ready", partial: false, age_s: 0, kinds: { Pod: { state: "ready", observed_at: "2026-10-09T12:00:00Z", records, partial: false } }, total_kinds: 1, ready_kinds: 1, failing_kinds: 0, pending_kinds: 0, truncated: false });
const failedReport: KubernetesSyncHealthReport = { state: "error", partial: true, age_s: 0, kinds: { Pod: { state: "error", observed_at: "0001-01-01T00:00:00Z", last_attempt_at: "2026-10-26T09:12:03Z", records: 0, partial: true, error: "forbidden" } }, total_kinds: 1, ready_kinds: 0, failing_kinds: 1, pending_kinds: 0, truncated: false };
// An unreachable cluster cannot answer the report, so it has none: the row must
// still never read ready for it.
const reportsFor = (clusters: KubernetesClusterSummary[]) => Object.fromEntries(clusters.filter((cluster) => cluster.health !== "unreachable").map((cluster) => [cluster.id, reportFor(cluster)]));
const mount = (clusters: KubernetesClusterSummary[], reports = reportsFor(clusters), retrying = false) => {
  const retry = vi.fn();
  return { ...render(<MemoryRouter><KubernetesFleet clusters={clusters} reports={reports} retry={retry} retrying={retrying} /></MemoryRouter>), retry };
};
const rendered = (clusters: KubernetesClusterSummary[], reports = reportsFor(clusters)) => mount(clusters, reports);
const orderIn = () => screen.getAllByRole("article").map((card) => card.getAttribute("aria-label"));
const fleetHeader = () => screen.getByRole("group", { name: /clusters, / });
const headerChips = (header: HTMLElement) => [...header.querySelectorAll<HTMLElement>("[data-bucket]")];
// Only the chips are visible; the sentence that names every bucket is sr-only.
const visibleHeaderText = (header: HTMLElement) => [...header.querySelectorAll<HTMLElement>('[aria-hidden="true"]')].map((element) => element.textContent).join(" ");

describe("Kubernetes fleet", () => {
  it("shows only positive broken counts and observed change times", () => {
    mount([summary("unknown", "healthy"), { ...summary("known", "attention"), broken: 3, last_change_at: new Date(Date.now() - 240_000).toISOString() }, { ...summary("quiet", "healthy"), broken: 0 }]);
    expect(screen.queryByText("Broken unknown")).toBeNull();
    expect(screen.queryByText("0 broken")).toBeNull();
    expect(screen.getByText("3 broken").className).toContain("text-sev-critical");
    expect(screen.getByText("Last change 4 min ago")).toBeTruthy();
    expect(screen.queryByText("Last change unknown")).toBeNull();
    expect(screen.getAllByText(/^Last change/)).toHaveLength(1);
  });

  it.each([undefined, "", "not-a-date"])("hides unobserved change time %s", (lastChangeAt) => {
    mount([{ ...summary("cluster", "healthy"), last_change_at: lastChangeAt }]);
    expect(screen.queryByText(/^Last change/)).toBeNull();
    expect(screen.getByText("0 warnings")).toBeTruthy();
  });

  it("orders unhealthy cards first, preserves missing evidence, and scopes links", () => {
    const { retry } = mount([summary("healthy", "healthy"), summary("offline", "unreachable"), summary("partial", "partial")]);
    // A cluster with no report leads; the rest read ready and sort by name.
    expect(screen.getAllByRole("article").map((card) => card.getAttribute("aria-label"))).toEqual(["Cluster offline", "Cluster healthy", "Cluster partial"]);
    expect(screen.getByRole("link", { name: "Open cluster healthy" }).getAttribute("href")).toBe("/?cluster=healthy");
    expect(screen.getAllByText(/Version unavailable/)).toHaveLength(3);
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(retry).toHaveBeenCalledOnce();
  });

  // The pill is the row's one state word, so it comes from the sync report.
  // Node health alone can read green while the cluster's sync is failing.
  it.each([
    ["healthy", "Ready", "pill pill-good"],
    ["attention", "Ready", "pill pill-good"],
    ["partial", "Ready", "pill pill-good"],
    ["unknown", "Ready", "pill pill-good"],
    ["syncing", "Preparing", "pill"],
    ["unreachable", "Not ready", "pill"],
  ] as const)("labels a %s cluster from its sync report, not its node health", (health, label, pillClass) => {
    rendered([health === "syncing" ? syncing("one") : summary("one", health)]);
    const pill = screen.getByRole("article", { name: "Cluster one" }).querySelector(".pill")!;
    expect(pill.getAttribute("data-sync-state-label")).not.toBeNull();
    expect(pill.textContent).toBe(label);
    expect(pill.className).toBe(pillClass);
  });

  it("treats a stale health with a syncing envelope as preparing everywhere on the row", () => {
    // The server can report an older health while the sync envelope already
    // says the first sync is running; both signals must agree.
    const divergent: KubernetesClusterSummary = { ...syncing("divergent", firstProgress), health: "unknown" };
    rendered([summary("healthy", "healthy"), divergent]);
    const row = screen.getByRole("article", { name: "Cluster divergent" });
    expect(row.querySelector(".pill")!.textContent).toBe("Preparing");
    expect(row.querySelector(".pill")!.className).toBe("pill");
    expect(within(row).queryByText("No node evidence")).toBeNull();
    expect(within(row).queryByText("0 warnings")).toBeNull();
    expect(within(row).queryByText("Syncing")).toBeNull();
    expect(within(row).getByText("Waiting for first data")).toBeTruthy();
    expect(within(row).getByText("Preparing first data")).toBeTruthy();
    const line = row.querySelector<HTMLElement>("[data-first-sync]")!;
    expect(line.querySelector("[data-first-sync-step]")!.textContent).toBe("Listing resources");
    // A preparing row sorts after unreachable and before attention, whatever its health says.
    expect(orderIn()).toEqual(["Cluster divergent", "Cluster healthy"]);
  });

  it("renders first-sync progress as one full-width line and reports no false zero", () => {
    rendered([syncing("prep", firstProgress)]);
    const row = screen.getByRole("article", { name: "Cluster prep" });
    expect(row.textContent).not.toMatch(/0\s*\/\s*0/);
    expect(screen.queryByRole("meter")).toBeNull();
    expect(within(row).getAllByText("Loading")).toHaveLength(2);
    expect(within(row).getByText("Waiting for first data")).toBeTruthy();
    expect(within(row).getByText("Preparing first data")).toBeTruthy();
    // The zeros of a first sync are never presented as facts.
    expect(within(row).queryByText("0 warnings")).toBeNull();
    expect(within(row).queryByText(/\d{1,2}:\d{2}/)).toBeNull();
    const line = row.querySelector<HTMLElement>("[data-first-sync]")!;
    const step = line.querySelector<HTMLElement>("[data-first-sync-step]")!;
    const chips = [...line.querySelectorAll<HTMLElement>("[data-first-sync-count]")].map((chip) => chip.textContent);
    expect(step.textContent).toBe("Listing resources");
    expect(chips).toEqual(["Pods 3,214", "Deployments 412", "Namespaces 27"]);
    // Spacing only: each visible part carries no separator of its own.
    for (const part of [step, ...line.querySelectorAll<HTMLElement>("[data-first-sync-count]")]) expect(part.textContent).not.toMatch(/[.·•]/);
    // The parts read back as one phrase through the group name, and the same
    // full text is the row link's name, so nothing depends on a hover tooltip.
    expect(screen.getByRole("group", { name: "Listing resources, Pods 3,214, Deployments 412, Namespaces 27" })).toBe(line);
    expect(line.hasAttribute("title")).toBe(false);
    expect(screen.getByRole("link", { name: "Open cluster prep: Preparing first data, Listing resources, Pods 3,214, Deployments 412, Namespaces 27" }).getAttribute("href")).toBe("/?cluster=prep");
  });

  it("keeps the progress line on its own full-width sub-row instead of the health cell", () => {
    const narrow = window.innerWidth;
    window.innerWidth = 390;
    try {
      rendered([syncing("prep", firstProgress)]);
      const row = screen.getByRole("article", { name: "Cluster prep" });
      const line = row.querySelector<HTMLElement>("[data-first-sync]")!;
      // A direct child of the row, outside the grid cells, so it spans the card.
      expect(line.parentElement).toBe(row);
      expect(line.closest("[data-health-cell]")).toBeNull();
      expect(row.querySelector("[data-health-cell] [data-first-sync]")).toBeNull();
      // One visual line on a narrow screen: it truncates rather than wrapping.
      expect(line.className).not.toContain("flex-wrap");
      expect(line.className).toContain("whitespace-nowrap");
      expect(line.className).toContain("overflow-hidden");
      expect(line.querySelector<HTMLElement>("[data-first-sync-step]")!.className).toContain("truncate");
      for (const chip of line.querySelectorAll<HTMLElement>("[data-first-sync-count]")) expect(chip.className).toContain("shrink-0");
      expect(screen.getByRole("region", { name: "Cluster rows" }).className).toContain("overflow-x-auto");
    } finally {
      window.innerWidth = narrow;
    }
  });

  it("falls back to Preparing before the first progress payload arrives", () => {
    rendered([syncing("prep")]);
    const row = screen.getByRole("article", { name: "Cluster prep" });
    const line = row.querySelector<HTMLElement>("[data-first-sync]")!;
    expect(line.querySelector("[data-first-sync-step]")?.textContent).toBe("Preparing");
    expect(line.querySelectorAll("[data-first-sync-count]")).toHaveLength(0);
    expect(screen.getByRole("group", { name: "Preparing" })).toBe(line);
    expect(line.hasAttribute("title")).toBe(false);
    expect(screen.getByRole("link", { name: "Open cluster prep: Preparing first data, Preparing" })).toBeTruthy();
  });

  it("orders a mixed fleet by sync state, with no report before a preparing cluster and both before ready", () => {
    rendered([summary("healthy", "healthy"), summary("unknown", "unknown"), summary("partial", "partial"), summary("attention", "attention"), syncing("preparing"), summary("unreachable", "unreachable")]);
    expect(orderIn()).toEqual(["Cluster unreachable", "Cluster preparing", "Cluster attention", "Cluster healthy", "Cluster partial", "Cluster unknown"]);
  });

  it("orders a failing report before a degraded one, and both before a ready one", () => {
    const degraded: KubernetesSyncHealthReport = { ...readyReport(4), state: "partial", partial: true, kinds: { Pod: { state: "partial", observed_at: "2026-10-09T12:00:00Z", records: 4, partial: true } }, failing_kinds: 1 };
    mount([summary("east", "attention"), summary("west", "healthy"), summary("north", "healthy")], { east: readyReport(), west: degraded, north: failedReport });
    expect(orderIn()).toEqual(["Cluster north", "Cluster west", "Cluster east"]);
  });

  it("counts every cluster in one sync bucket, so the header cannot call a failing cluster healthy", () => {
    mount([syncing("preparing"), summary("healthy", "healthy"), summary("unreachable", "unreachable"), summary("broken", "healthy")], { preparing: reportFor(syncing("preparing")), healthy: readyReport(), broken: failedReport });
    // Every bucket is its own chip, so the counts never run together into words.
    const header = fleetHeader();
    const chips = headerChips(header);
    expect(chips.map((chip) => chip.getAttribute("data-bucket"))).toEqual(["failed", "degraded", "not-ready", "preparing", "syncing", "ready"]);
    expect(chips.map((chip) => chip.textContent)).toEqual(["1 failed", "0 degraded", "1 not ready", "1 preparing", "0 syncing", "1 ready"]);
    expect(chips.every((chip) => chip.getAttribute("aria-hidden") === "true")).toBe(true);
    expect(chips[0].className).toContain("rounded");
    expect(chips[0].className).toContain("border");
    expect(screen.getByText("4 clusters")).toBeTruthy();
    // Nothing separates the chips visually except their own borders.
    expect(visibleHeaderText(header)).not.toMatch(/[·•|,;/]/);
    // The buckets add up to the fleet size, and the sentence names each one.
    const shown = chips.map((element) => Number.parseInt(element.textContent ?? "", 10));
    expect(shown.reduce((sum, value) => sum + value, 0)).toBe(4);
    expect(screen.getByRole("group", { name: "4 clusters, 1 failed, 0 degraded, 1 not ready, 1 preparing, 0 syncing, 1 ready" })).toBeTruthy();
  });

  // The contradiction the review found: a green node-health field must not
  // outrank the cluster's own failing report, in the pill or in the header.
  it("never shows a green pill or a healthy header count for a cluster whose report fails", () => {
    mount([{ ...summary("divergent", "healthy"), warnings: 0 }], { divergent: failedReport });
    const row = screen.getByRole("article", { name: "Cluster divergent" });
    const pill = row.querySelector(".pill")!;
    expect(pill.textContent).toBe("Failed");
    expect(pill.className).toBe("pill pill-bad");
    expect(pill.className).not.toContain("pill-good");
    expect(headerChips(fleetHeader()).map((chip) => chip.textContent)).toEqual(["1 failed", "0 degraded", "0 not ready", "0 preparing", "0 syncing", "0 ready"]);
  });

  it("keeps a preparing cluster openable with its usual row actions", () => {
    rendered([syncing("prep", firstProgress)]);
    expect(screen.getByRole("link", { name: /^Open cluster prep/ }).getAttribute("href")).toBe("/?cluster=prep");
    expect(screen.getByRole("link", { name: "Review issues" }).getAttribute("href")).toBe("/?cluster=prep&view=issues");
  });

  // The catalog envelope only covers eight core kinds, so on its own it can say
  // a cluster is fine while the explorer is blocked. The row must read the same
  // complete report the explorer does.
  it("never reads ready, and never shows counts, when the cluster's own report is failing", () => {
    const lying = { ...summary("divergent", "healthy"), sync: { state: "ready", age_s: 0, partial: false } };
    mount([lying], { divergent: failedReport });
    const row = screen.getByRole("article", { name: "Cluster divergent" });
    expect(row.querySelector("[data-sync-state-label]")?.textContent).toBe("Failed");
    expect(row.querySelector("[data-sync-state-label]")?.className).toContain("pill-bad");
    // The counts and signals behind a failing sync are not shown at all.
    expect(within(row).queryByRole("meter")).toBeNull();
    expect(within(row).queryByText(/^2\s*\/\s*2$/)).toBeNull();
    expect(within(row).queryByText("0 warnings")).toBeNull();
    expect(within(row).getAllByText("Hidden")).toHaveLength(2);
    // The signals column says the row cannot be trusted yet, and the row offers
    // no warning or broken counts to read as facts.
    expect(within(row).getAllByText("Not ready")).toHaveLength(1);
  });

  it("reads degraded and failed from the report, and shows counts again only once it is ready", () => {
    const cluster = summary("recovering", "healthy");
    const degraded: KubernetesSyncHealthReport = { ...readyReport(4), state: "partial", partial: true, kinds: { Pod: { state: "partial", observed_at: "2026-10-09T12:00:00Z", records: 4, partial: true } }, failing_kinds: 1 };
    const { rerender } = mount([cluster], { recovering: degraded });
    const row = () => screen.getByRole("article", { name: "Cluster recovering" });
    expect(row().querySelector("[data-sync-state-label]")?.textContent).toBe("Degraded");
    expect(within(row()).queryByRole("meter")).toBeNull();
    // The same report recovering needs no user action from this component.
    rerender(<MemoryRouter><KubernetesFleet clusters={[cluster]} reports={{ recovering: readyReport(4) }} retry={vi.fn()} retrying={false} /></MemoryRouter>);
    expect(row().querySelector("[data-sync-state-label]")?.textContent).toBe("Ready");
    expect(within(row()).getAllByRole("meter")).toHaveLength(2);
    expect(within(row()).getByText("0 warnings")).toBeTruthy();
  });

  it("keeps a cluster ready when the only trouble is a kind the connector may not read", () => {
    const unreadable: KubernetesSyncHealthReport = {
      ...readyReport(4), state: "ready", unreadable_kinds: 1,
      kinds: {
        Pod: { state: "ready", observed_at: "2026-10-09T12:00:00Z", records: 4, partial: false },
        Secret: { state: "error", observed_at: "0001-01-01T00:00:00Z", records: 0, partial: true, error: "forbidden" },
      },
      unreadable: [{ kind: "Secret", state: "error", class: "forbidden", records: 0, since: "2026-10-26T10:49:24Z" }],
    };
    mount([summary("least-privilege", "healthy")], { "least-privilege": unreadable });
    const row = screen.getByRole("article", { name: "Cluster least-privilege" });
    // A permission gap is not a sync fault: the row is ready and shows its counts.
    expect(row.querySelector("[data-sync-state-label]")?.textContent).toBe("Ready");
    expect(within(row).getAllByRole("meter")).toHaveLength(2);
    expect(headerChips(fleetHeader()).map((chip) => chip.textContent)).toEqual(["0 failed", "0 degraded", "0 not ready", "0 preparing", "0 syncing", "1 ready"]);
  });

  it("never renders a zero snapshot time as a clock reading", () => {
    mount([{ ...summary("empty", "healthy"), observed_at: "0001-01-01T00:00:00Z" }]);
    const row = screen.getByRole("article", { name: "Cluster empty" });
    expect(within(row).getByText("Snapshot unavailable")).toBeTruthy();
    expect(within(row).queryByText(/^\d{1,2}:\d{2}:\d{2}/)).toBeNull();
  });

  it("shows a cluster it has no report for as not ready instead of trusting the envelope", () => {
    mount([summary("unreported", "healthy")], {});
    const row = screen.getByRole("article", { name: "Cluster unreported" });
    expect(row.querySelector("[data-sync-state-label]")?.textContent).toBe("Not ready");
    expect(within(row).queryByRole("meter")).toBeNull();
    expect(within(row).queryByText("0 warnings")).toBeNull();
  });

  it("separates cluster query and invalidation prefixes", () => {
    expect(kubernetesKey("one", "overview")).toEqual(["kubernetes", "one", "overview"]);
    expect(kubernetesKey("two")).toEqual(["kubernetes", "two"]);
    expect(kubernetesKey(undefined)).toEqual(["kubernetes", "_"]);
  });

  it("keeps rows horizontally scrollable and avoids readiness meters for an unreachable cluster", () => {
    mount([summary("offline", "unreachable"), { ...summary("healthy", "healthy"), issues: 0 }], undefined, true);
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
it("isolates first publication and quiet refresh classifications per fleet cluster", () => {
  const fresh = syncing("new", firstProgress);
  const used = { ...summary("used", "healthy"), sync: { state: "syncing", age_s: 20, partial: false } };
  mount([fresh, used], {
    new: reportFor(fresh),
    used: { ...reportFor(used), first_ready_at: "2026-10-27T08:00:00Z" },
  });
  const newRow = screen.getByRole("article", { name: "Cluster new" });
  const usedRow = screen.getByRole("article", { name: "Cluster used" });
  expect(within(newRow).getByText("Preparing", { exact: true })).toBeTruthy();
  expect(within(usedRow).getByText("Syncing", { exact: true })).toBeTruthy();
  expect(usedRow.textContent).not.toMatch(/Preparing|Waiting for first data/);
  expect(usedRow.querySelector("[data-first-sync]")).toBeNull();
  expect(usedRow.textContent).toContain("2 / 2");
});
