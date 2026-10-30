// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api, type KubernetesTimelinePage, type KubernetesCrashPage, type KubernetesResource, type KubernetesIssue } from "@/lib/api";
import { CoverageMarker } from "@/components/CoverageMarker";
import { NamespaceSelect } from "@/components/NamespaceSelect";
import { KubernetesExplorerTabContent } from "./KubernetesExplorerViews";
import { ResourceRelations, ResourceTimeline } from "./KubernetesResourceDetail";
import { ResourceMetrics } from "./KubernetesResourceMetrics";
import { kubernetesExplorerTabs } from "./kubernetesExplorerTabs";
import { failureWarnings } from "./kubernetesWarningItems";

const now = "2026-10-23T08:00:00Z";
const sync = { state: "ready", age_s: 0, partial: false };
const emptyTimeline: KubernetesTimelinePage = { items: [], total: 0, crash_count: 0, counts: { change: 0, crash: 0, break: 0 }, since: "2026-10-23T07:00:00Z", until: now, gaps: [], sync };
const deployment: KubernetesResource = { resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "shop", name: "checkout", uid: "deployment" };
const crashPage: KubernetesCrashPage = {
  items: [], crash_count: 0, since: "2026-10-22T08:00:00Z", until: now,
  retention_from: "2026-10-22T08:00:00Z", retention_basis: "capture", started_at: "2026-10-22T08:00:00Z",
  gaps: [], capture_gaps: [], limitations: [], coverage: { Pod: { from: "2026-10-22T08:00:00Z", to: now } },
  capture: { mode: "continuous", behind: false }, sync,
};
function mount(children: React.ReactNode, route = "/agent/kubernetes?view=timeline") {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><MemoryRouter initialEntries={[route]}>{children}</MemoryRouter></QueryClientProvider>);
}
beforeEach(() => {
  vi.spyOn(Date, "now").mockReturnValue(Date.parse(now));
  vi.spyOn(api, "kubernetesNamespaces").mockResolvedValue({ items: ["shop", "platform"], sync });
  vi.spyOn(api, "kubernetesChanges").mockResolvedValue({ items: [], sync });
  vi.spyOn(api, "kubernetesTimeline").mockResolvedValue(emptyTimeline);
  vi.spyOn(api, "kubernetesIssues").mockResolvedValue({ items: [], totals: {}, truncated: false, sync });
  vi.spyOn(api, "kubernetesCrashes").mockResolvedValue(crashPage);
  vi.spyOn(api, "kubernetesRelated").mockResolvedValue({ nodes: [], edges: [], omitted: {}, sync });
  vi.spyOn(api, "kubernetesUsageHistory").mockResolvedValue({ observed_at: now, source: "capture", samples: [] });
});
afterEach(() => { cleanup(); vi.restoreAllMocks(); });

describe("Kubernetes presentation", () => {
  it("keeps Workloads second without changing deep-link identifiers", () => {
    expect(kubernetesExplorerTabs.map((tab) => tab.id)).toEqual(["overview", "resources", "issues", "timeline", "topology", "nodes", "releases", "gitops", "traffic", "agent"]);
  });
  it("hides size and paging reasons and keeps one neutral keyboard disclosure", () => {
    const warnings = failureWarnings([{ class: "item_limit" }, { class: "page_limit" }, { resource_id: "core~v1~secrets", class: "forbidden" }]);
    mount(<><nav aria-label="Kubernetes views" /><CoverageMarker reasons={warnings.map((warning) => warning.content)} /></>);
    const trigger = screen.getByRole("button", { name: "Resource coverage" });
    fireEvent.focus(trigger);
    expect(screen.getByRole("region", { name: "Resource coverage details" }).textContent).toContain("secrets: permission denied");
    expect(document.body.textContent).not.toContain("item_limit");
    fireEvent.keyDown(document, { key: "Escape" });
    expect(trigger.getAttribute("aria-expanded")).toBe("false");
  });
  it("accepts only readable namespace options through search and keyboard", async () => {
    const change = vi.fn();
    mount(<div data-testid="namespace-card" className="overflow-hidden"><NamespaceSelect label="Issue namespace" value="" onChange={change} /></div>);
    const input = screen.getByRole("combobox", { name: "Issue namespace" });
    fireEvent.focus(input);
    await screen.findByRole("option", { name: "platform" });
    expect(screen.getByTestId("namespace-card").contains(screen.getByRole("listbox"))).toBe(false);
    expect(screen.getAllByRole("option")[0].textContent).toBe("All namespaces");
    fireEvent.change(input, { target: { value: "plat" } });
    fireEvent.keyDown(input, { key: "Enter" });
    expect(change).toHaveBeenLastCalledWith("platform");
    fireEvent.focus(input);
    fireEvent.change(input, { target: { value: "not-readable" } });
    fireEvent.keyDown(input, { key: "Enter" });
    expect(change).toHaveBeenCalledTimes(1);
    fireEvent.keyDown(input, { key: "Escape" });
    expect(input.getAttribute("aria-expanded")).toBe("false");
  });
  it("renders only the directed Related set, not describe siblings using the same Secret", async () => {
    const nodes = [
      { id: "deployment", kind: "Deployment", name: "checkout", namespace: "shop", group: "workload" },
      { id: "secret", kind: "Secret", name: "shared", namespace: "shop", group: "config" },
      { id: "pod", kind: "Pod", name: "checkout-pod", namespace: "shop", group: "workload" },
      { id: "hpa", kind: "HorizontalPodAutoscaler", name: "checkout-hpa", namespace: "shop", group: "workload" },
      { id: "rs", kind: "ReplicaSet", name: "checkout-rs", namespace: "shop", group: "workload" },
      { id: "service", kind: "Service", name: "checkout-svc", namespace: "shop", group: "network" },
      { id: "config", kind: "ConfigMap", name: "checkout-config", namespace: "shop", group: "config" },
      { id: "node", kind: "Node", name: "node-a", group: "cluster" },
    ];
    vi.mocked(api.kubernetesRelated).mockResolvedValue({ nodes, edges: [{ from: "deployment", to: "secret", type: "uses" }, { from: "deployment", to: "rs", type: "manages" }, { from: "rs", to: "pod", type: "manages" }, { from: "service", to: "pod", type: "exposes" }, { from: "pod", to: "node", type: "uses" }, { from: "deployment", to: "config", type: "uses" }, { from: "hpa", to: "deployment", type: "scales" }], omitted: {}, sync });
    mount(<ResourceRelations resource={deployment} related={Array.from({ length: 10 }, (_, index) => ({ kind: "Deployment", namespace: "shop", name: `unrelated-${index}` }))} onOpen={vi.fn()} />);
    await screen.findByRole("button", { name: "Open Secret shared" });
    expect(api.kubernetesRelated).toHaveBeenCalledWith({ resource_id: deployment.resource_id, namespace: "shop", name: "checkout" }, expect.any(AbortSignal));
    expect(screen.getByRole("button", { name: "Open Pod checkout-pod" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Open HorizontalPodAutoscaler checkout-hpa" })).toBeTruthy();
    for (const name of ["Open ReplicaSet checkout-rs", "Open Service checkout-svc", "Open ConfigMap checkout-config", "Open Node node-a"]) expect(screen.getByRole("button", { name })).toBeTruthy();
    expect(document.body.textContent).not.toContain("unrelated-");
  });
  it("sends only the saved namespace as a discovery hint, never arbitrary search text", async () => {
    mount(<NamespaceSelect label="Timeline namespace" value="shop" onChange={vi.fn()} />);
    await waitFor(() => expect(api.kubernetesNamespaces).toHaveBeenCalledWith("shop"));
    const input = screen.getByRole("combobox", { name: "Timeline namespace" });
    fireEvent.focus(input);
    fireEvent.change(input, { target: { value: "unlisted" } });
    fireEvent.keyDown(input, { key: "Enter" });
    expect(api.kubernetesNamespaces).toHaveBeenCalledTimes(1);
  });
  it("keeps the empty resource Timeline to one short line with coverage by the range", async () => {
    vi.mocked(api.kubernetesTimeline).mockResolvedValue(emptyTimeline);
    mount(<ResourceTimeline resource={deployment} onCrash={vi.fn()} />);
    await screen.findByText("No changes or crashes in this range.");
    expect(document.body.textContent).not.toMatch(/Partial history|Some resource history|Crash history starts|No recorded/);
    expect(screen.queryByRole("button", { name: "Resource coverage" })).toBeNull();
  });
  it("shows a capture-gap marker without another resource Timeline caveat", async () => {
    vi.mocked(api.kubernetesTimeline).mockResolvedValue({ ...emptyTimeline, gaps: [{ from: "2026-10-23T07:00:00Z", to: now }] });
    mount(<ResourceTimeline resource={deployment} onCrash={vi.fn()} />);
    const marker = await screen.findByRole("button", { name: "Resource coverage" });
    expect(screen.queryByText("No changes or crashes in this range.")).toBeNull();
    expect(marker.parentElement?.querySelector('[aria-label="History range"]')).toBeTruthy();
    expect(screen.getAllByRole("button", { name: "Resource coverage" })).toHaveLength(1);
  });
  it("omits workload-name uptime badges and separates chart ranges from the legend", async () => {
    mount(<ResourceMetrics resource={deployment} liveSamples={[]} onCrash={vi.fn()} />);
    expect(screen.getByLabelText("Uptime metric tile").querySelector(".metric-container-label")).toBeNull();
    const chart = screen.getByLabelText("CPU and memory usage chart");
    expect(within(chart).getByText("CPU and memory").closest("figcaption")).toBeTruthy();
    expect(within(chart).getByRole("group", { name: "Usage history range" }).closest("figcaption")).toBeNull();
    expect(within(chart).getByRole("group", { name: "Usage history range" }).textContent).toContain("since opened");
  });
  it("virtualizes the default break/change list and retains rows while fetching a page", async () => {
    const items = Array.from({ length: 120 }, (_, index) => ({ id: `change-${index}`, cluster: "east", kind: "Deployment", namespace: "shop", name: `checkout-${index}`, uid: `uid-${index}`, type: "image_changed" as const, at: "2026-10-23T07:50:00Z" }));
    let resolvePage!: (page: KubernetesTimelinePage) => void;
    vi.mocked(api.kubernetesTimeline).mockImplementation((options) => options?.cursor ? new Promise((resolve) => { resolvePage = resolve; }) : Promise.resolve({ ...emptyTimeline, items: items.map((change) => ({ type: "change" as const, id: change.id, at: change.at, change })), total: 121, counts: { change: 121, crash: 0, break: 0 }, next: "page-two" }));
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    const list = await screen.findByRole("list", { name: "Breaks and changes" });
    await within(list).findByText("checkout-0");
    expect(within(list).getAllByRole("listitem").length).toBeLessThan(30);
    expect(screen.queryByLabelText("Resource timeline")).toBeNull();
    expect(screen.queryByLabelText("Logs at break")).toBeNull();
    const pager = screen.getByRole("navigation", { name: "Timeline pages" });
    fireEvent.click(within(pager).getByRole("button", { name: "Next page" }));
    await waitFor(() => expect(api.kubernetesTimeline).toHaveBeenLastCalledWith(expect.objectContaining({ cursor: "page-two" }), expect.any(AbortSignal)));
    expect(within(list).getByText("checkout-0")).toBeTruthy();
    expect(within(pager).getByRole("button", { name: "Next page" }).hasAttribute("disabled")).toBe(true);
    await act(async () => resolvePage({ ...emptyTimeline, total: 121, counts: { change: 121, crash: 0, break: 0 }, items: [{ type: "change", id: "next", at: items[0].at, change: { ...items[0], name: "next-page" } }] }));
    await within(list).findByText("next-page");
  });
  it("aggregates both GitOps cards into one coverage marker without dropping reasons", async () => {
    vi.spyOn(api, "kubernetesGitOpsApps").mockResolvedValue({ items: [], available: true, truncated: false, partial_failures: [{ resource_id: "argoproj.io~v1alpha1~applications", class: "forbidden" }] });
    vi.spyOn(api, "kubernetesRollouts").mockResolvedValue({ items: [], available: true, truncated: false, partial_failures: [{ resource_id: "argoproj.io~v1alpha1~rollouts", class: "timeout" }] });
    mount(<KubernetesExplorerTabContent tab="gitops" onSelectResource={vi.fn()} />);
    await waitFor(() => expect(screen.getAllByRole("button", { name: "Resource coverage" })).toHaveLength(1));
    fireEvent.click(screen.getByRole("button", { name: "Resource coverage" }));
    expect(screen.getByText("applications: permission denied")).toBeTruthy();
    expect(screen.getByText("rollouts: source did not respond")).toBeTruthy();
  });
  it("preserves root deep links and requests server-filtered related changes", async () => {
    const retained: KubernetesIssue = { root: deployment, broke_at: "2026-10-23T07:55:00Z", rule: "pod.crash_loop", severity: "critical", count: 1 };
    vi.mocked(api.kubernetesIssues).mockImplementation(async options => ({ items: options?.context === "persisted" ? [] : [retained], totals: {}, truncated: false, sync, ...(options?.context === "persisted" ? { root_context: { root: retained.root, occurrence: retained, retained: true as const } } : {}) }));
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />, "/agent/kubernetes?view=timeline&root_uid=deployment&namespace=shop");
    await waitFor(() => expect(api.kubernetesChanges).toHaveBeenCalledWith(expect.objectContaining({ root_uid: "deployment", namespace: "shop" }), expect.any(AbortSignal)));
    expect(screen.getByRole("link", { name: "Clear root filter" })).toBeTruthy();
    expect(screen.queryByRole("list", { name: "Breaks and changes" })).toBeNull();
  });
});
