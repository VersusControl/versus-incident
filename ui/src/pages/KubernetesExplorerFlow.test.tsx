// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, useLocation } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api, kubernetesAllContainers, type KubernetesBreakLogPage, type KubernetesCrashEvent, type KubernetesCrashPage, type KubernetesIssue, type KubernetesSyncProgress, type KubernetesTimelineEntry, type KubernetesTimelinePage } from "@/lib/api";
import { KubernetesClusterContext, kubernetesKey } from "@/lib/useKubernetesCluster";
import { KubernetesPreparingPanel } from "@/components/KubernetesPreparingPanel";
import { KubernetesPendingNote } from "@/components/KubernetesPendingNote";
import { pendingKindName, pendingKindNames, settledSync } from "@/lib/kubernetesPending";
import { filterFieldClass, filterFieldControlClass, filterRowClass } from "@/components/KubernetesFilterField";
import { timelineWarnings } from "./kubernetesWarningItems";
import { BreakRow, ResourceTimelineChart, TimelineCard } from "./KubernetesBreakTimeline";
import { ToastProvider } from "@/components/Toast";
import { KubernetesExplorerTabContent } from "./KubernetesExplorerViews";
import { KubernetesBreakLogs, KubernetesBreakLogsProvider } from "./KubernetesBreakLogs";

const sync = { state: "ready", age_s: 0, partial: false };
const issue: KubernetesIssue = { root: { kind: "Deployment", namespace: "shop", name: "checkout", uid: "checkout-uid" }, rule: "pod.crash_loop", severity: "warning", count: 2, of: 25, broken: true, broke_at: "2026-10-14T12:04:00Z", history: "complete" };
const change = { id: "image", cluster: "east", kind: "Deployment", namespace: "shop", name: "checkout", uid: "checkout-uid", type: "image_changed" as const, fields: [{ path: "image", from: "v41", to: "v42" }], at: "2026-10-14T12:00:00Z", source: "reconstructed" as const };
const crashPage: KubernetesCrashPage = { items: [], since: "2026-10-14T11:00:00Z", until: "2026-10-14T13:00:00Z", retention_from: "2026-10-14T11:00:00Z", retention_basis: "capture", started_at: "2026-10-14T11:00:00Z", gaps: [], capture_gaps: [], coverage: {}, limitations: [], capture: { mode: "continuous", behind: false }, sync };
const logPage = (overrides: Partial<KubernetesBreakLogPage> = {}): KubernetesBreakLogPage => ({
  window: { from: "2026-10-14T11:59:00Z", to: "2026-10-14T12:06:00Z" },
  pods: [{ name: "checkout-a", uid: "pod-a", short: "a", state: "running" }],
  lines: [{ at: "2026-10-14T12:03:00Z", pod: "checkout-a", pod_uid: "pod-a", container: "app", init: false, slot: "current", level: "error", text: "upstream timeout", source: "kubernetes" }],
  markers: [], counts: { all: 1, errors: 1, warnings: 0 }, rate: [],
  patterns: Array.from({ length: 8 }, (_, index) => ({ template: `message ${index} ${"long text ".repeat(index === 0 ? 30 : 1)}`.trim(), level: "error", count: 20 - index })),
  sources: Array.from({ length: 7 }, (_, index) => ({ kind: "linked", name: `source-${index}`, type: "loki", at: "2026-10-14T12:06:00Z" })),
  coverage: { state: "complete", gaps: [], reason: "", tip: false },
  container: "app", main_container: "app",
  containers: [{ name: "app", main: true }, { name: "istio-proxy" }, { name: "migrate", init: true }],
  ...overrides,
});
function timelinePage(items: KubernetesTimelineEntry[], overrides: Partial<KubernetesTimelinePage> = {}): KubernetesTimelinePage {
  const counts = { change: 0, crash: 0, break: 0 };
  for (const entry of items) counts[entry.type]++;
  return { items, counts, total: items.length, crash_count: counts.crash, gaps: [], sync, since: "2026-10-14T11:06:00Z", until: "2026-10-14T12:06:00Z", ...overrides };
}
function Location() { const location = useLocation(); return <output aria-label="Location">{location.search}</output>; }
function mount(element: React.ReactNode) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return { client, ...render(<QueryClientProvider client={client}><KubernetesClusterContext.Provider value={{ client: api, cluster: "east", multiple: true, clusters: [], key: (...parts) => kubernetesKey("east", ...parts) }}><MemoryRouter initialEntries={[`${window.location.pathname}${window.location.search}`]}>{element}<Location /></MemoryRouter></KubernetesClusterContext.Provider></QueryClientProvider>) };
}
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => { resolve = done; });
  return { promise, resolve };
}

afterEach(() => { cleanup(); vi.restoreAllMocks(); window.history.replaceState(null, "", "/"); });
beforeEach(() => {
  vi.spyOn(api, "kubernetesNamespaces").mockResolvedValue({ items: ["shop"], sync });
});

// The listing detail lays its counts out as separate elements, so its text is
// the parts back to back: any reintroduced separator shows up here.
const listingCounts = () => {
  const counts = document.querySelector<HTMLElement>("[data-first-sync-counts]")!;
  return { chips: [...counts.querySelectorAll("[data-first-sync-count]")].map((chip) => chip.textContent), text: counts.textContent ?? "" };
};

describe("Preparing your cluster panel", () => {
  const progress: KubernetesSyncProgress = { step: "listing", step_index: 2, step_total: 4, steps_done: ["connected"], counts: { Pod: 3214, Deployment: 412, Namespace: 27, Node: 12 }, kinds_done: 3, kinds_total: 6, cluster_version: "v1.30.2", node_count: 12, started_at: "2026-10-23T10:00:00Z", updated_at: "2026-10-23T10:00:05Z" };

  it("shows live steps, ticking counts, early facts and one progress bar", () => {
    const { rerender } = render(<KubernetesPreparingPanel progress={progress} cluster="prod-eks" />);
    const panel = screen.getByRole("region", { name: "Preparing your cluster" });
    expect(within(panel).getByText(/First sync of prod-eks/)).toBeTruthy();
    expect(within(screen.getByRole("list", { name: "Cluster facts" })).getAllByRole("listitem").map((item) => item.textContent)).toEqual(["Kubernetes v1.30", "12 nodes"]);
    const steps = screen.getByRole("list", { name: "First sync steps" });
    expect(steps.getAttribute("aria-live")).toBe("polite");
    const states = within(steps).getAllByRole("listitem").map((item) => [item.getAttribute("data-step"), item.getAttribute("data-state")]);
    expect(states).toEqual([["connected", "done"], ["listing", "active"], ["building", "waiting"], ["metrics", "waiting"]]);
    expect(within(steps).getByText("Kubernetes v1.30")).toBeTruthy();
    expect(listingCounts().chips).toEqual(["Pods 3,214", "Deployments 412", "Namespaces 27"]);
    expect(listingCounts().text).toBe("Pods 3,214Deployments 412Namespaces 27");
    expect(listingCounts().text).not.toMatch(/[.·•]/);
    // The spacing-only chips read back as one phrase, not as a run-on word.
    expect(screen.getByRole("group", { name: "Pods 3,214, Deployments 412, Namespaces 27" })).toBeTruthy();
    const bar = screen.getByRole("progressbar", { name: "First sync progress" });
    expect(bar.getAttribute("aria-valuetext")).toBe("Step 2 of 4");
    expect(screen.getByText("Usually under a minute for clusters this size")).toBeTruthy();

    rerender(<KubernetesPreparingPanel progress={{ ...progress, step: "building", step_index: 3, steps_done: ["connected", "listing"], counts: { ...progress.counts, Pod: 4100 } }} cluster="prod-eks" />);
    expect(listingCounts().chips).toEqual(["Pods 4,100", "Deployments 412", "Namespaces 27"]);
    expect(listingCounts().text).toBe("Pods 4,100Deployments 412Namespaces 27");
    expect(within(steps).getAllByRole("listitem").map((item) => item.getAttribute("data-state"))).toEqual(["done", "done", "active", "waiting"]);
    expect(bar.getAttribute("aria-valuetext")).toBe("Step 3 of 4");
  });

  it("keeps every step label free of separator characters", () => {
    render(<KubernetesPreparingPanel progress={progress} cluster="prod-eks" />);
    const labels = [...document.querySelectorAll<HTMLElement>("[data-first-sync-step-label]")];
    expect(labels).toHaveLength(4);
    for (const label of labels) expect(label.textContent).not.toMatch(/[.·•]/);
  });

  it("starts at the first step with no counts or time hint when progress is absent", () => {
    render(<KubernetesPreparingPanel />);
    const steps = within(screen.getByRole("list", { name: "First sync steps" })).getAllByRole("listitem");
    expect(steps.map((item) => item.getAttribute("data-state"))).toEqual(["active", "waiting", "waiting", "waiting"]);
    expect(screen.queryByRole("list", { name: "Cluster facts" })).toBeNull();
    expect(screen.queryByText("Usually under a minute for clusters this size")).toBeNull();
    expect(screen.getByText("Step 1 of 4")).toBeTruthy();
  });

  it("stops every animated glyph under prefers-reduced-motion", () => {
    const { container } = render(<KubernetesPreparingPanel progress={progress} />);
    const animated = [...container.querySelectorAll("[data-preparing-motion]")];
    expect(animated.length).toBeGreaterThanOrEqual(3);
    for (const element of animated) expect(/motion-reduce:(animate-none|hidden)/.test(element.getAttribute("class") ?? "")).toBe(true);
  });

  it("keeps the title and sentence readable by putting the facts below them on a narrow screen", () => {
    render(<KubernetesPreparingPanel progress={progress} cluster="prod-eks" />);
    const facts = screen.getByRole("list", { name: "Cluster facts" });
    const copy = screen.getByRole("heading", { name: "Preparing your cluster" }).parentElement!;
    // The facts take their own line under the copy until there is room beside it,
    // so the pills can never squeeze the text into one word per line.
    expect(facts.className.split(/\s+/)).toEqual(expect.arrayContaining(["w-full", "sm:w-auto"]));
    expect(copy.className).toContain("basis-56");
    expect(copy.className).toContain("sm:basis-0");
    expect(copy.className).toContain("min-w-0");
  });
});

describe("Logs at break layout", () => {
  const scenario = <KubernetesBreakLogsProvider issue={issue} onSelectResource={vi.fn()}><KubernetesBreakLogs /></KubernetesBreakLogsProvider>;
  const boxes = (container: HTMLElement) => ({
    pane: container.querySelector("[data-break-log-pane]")?.getAttribute("class"),
    filters: container.querySelector("[data-break-log-filters]")?.getAttribute("class"),
    side: container.querySelector("[data-break-log-side]")?.getAttribute("class"),
    spinnerSlot: container.querySelector("[data-break-log-spinner]")?.getAttribute("class"),
  });

  it("reserves the final log pane and side panel boxes while loading and keeps them when data lands", async () => {
    const response = deferred<KubernetesBreakLogPage>();
    vi.spyOn(api, "kubernetesBreakLogs").mockReturnValue(response.promise);
    const { container } = mount(scenario);
    const loading = boxes(container);
    expect(loading.filters).toContain("h-14");
    expect(screen.getByText("Main container:")).toBeTruthy();
    expect(loading.pane).toContain("h-80");
    expect(loading.side).toContain("h-80");
    expect(screen.getByRole("status", { name: "Loading logs at break" })).toBeTruthy();
    expect(screen.getByRole("log", { name: "Break log lines" }).getAttribute("aria-busy")).toBe("true");
    const ask = screen.getAllByRole("button", { name: "Ask chat" })[0];
    expect(ask.previousElementSibling?.hasAttribute("data-break-log-spinner")).toBe(true);
    await act(async () => { response.resolve(logPage()); });
    await screen.findByText("upstream timeout");
    expect(boxes(container)).toEqual(loading);
    // The filter row above the panes keeps one fixed height and never wraps,
    // even after the pod pills arrive with the data.
    const filters = screen.getByRole("group", { name: "Log filters" });
    expect(filters.className.split(/\s+/)).toEqual(expect.arrayContaining(["h-14", "flex-nowrap", "overflow-x-auto", "overflow-y-hidden"]));
    expect(filters.className).not.toMatch(/(^|\s)flex-wrap(\s|$)/);
    expect(within(filters).getByRole("group", { name: "Log pods" }).className).not.toMatch(/(^|\s)flex-wrap(\s|$)/);
    expect(within(filters).getByRole("button", { name: /checkout-a/ })).toBeTruthy();
    expect(screen.queryByRole("status", { name: "Loading logs at break" })).toBeNull();
  });

  it("reserves the logs section while the break itself is still loading", () => {
    const { container } = mount(<KubernetesBreakLogsProvider issue={undefined} onSelectResource={vi.fn()}><KubernetesBreakLogs pending /></KubernetesBreakLogsProvider>);
    expect(screen.getByRole("region", { name: "Logs at break" })).toBeTruthy();
    expect(boxes(container).pane).toContain("h-80");
    expect(boxes(container).side).toContain("h-80");
    expect(screen.getAllByRole("button", { name: "Ask chat" })[0].hasAttribute("disabled")).toBe(true);
  });

  it("caps top messages and sources at five, expands inside the panel and clamps long messages", async () => {
    vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValue(logPage());
    const { container } = mount(scenario);
    await screen.findByText("upstream timeout");
    const messages = screen.getByRole("region", { name: "Top messages" });
    expect(within(messages).getAllByRole("listitem")).toHaveLength(5);
    expect(within(messages).getByText("8 unique")).toBeTruthy();
    const first = within(messages).getByText(/^message 0/);
    expect(first.className).toContain("line-clamp-2");
    expect(first.getAttribute("title")).toBe(first.textContent);
    const sources = screen.getByRole("region", { name: "Sources" });
    expect(within(sources).getAllByRole("listitem")).toHaveLength(5);
    const side = container.querySelector("[data-break-log-side]")!;
    expect(side.className).toContain("overflow-y-auto");
    fireEvent.click(within(messages).getByRole("button", { name: "Show all (8)" }));
    fireEvent.click(within(sources).getByRole("button", { name: "Show all (7)" }));
    expect(within(messages).getAllByRole("listitem")).toHaveLength(8);
    expect(within(sources).getAllByRole("listitem")).toHaveLength(7);
    expect(side.className).toContain("h-80");
    expect(side.contains(messages) && side.contains(sources)).toBe(true);
  });

  it("defaults the container picker to the main container and can switch to one or all containers", async () => {
    const lookup = vi.spyOn(api, "kubernetesBreakLogs").mockImplementation(async (options) => logPage({ container: options.container ?? "app" }));
    mount(scenario);
    await screen.findByText("upstream timeout");
    expect(lookup.mock.calls[0][0].container).toBeUndefined();
    const select = screen.getByRole("combobox", { name: "Log container" }) as HTMLSelectElement;
    expect(select.value).toBe("app");
    expect(screen.getByText("Main container:")).toBeTruthy();
    expect([...select.options].map((option) => option.textContent)).toEqual(["app (main)", "istio-proxy", "init: migrate", "All containers"]);
    fireEvent.change(select, { target: { value: "istio-proxy" } });
    await waitFor(() => expect(lookup.mock.calls.at(-1)?.[0].container).toBe("istio-proxy"));
    expect(screen.getByText("Container:")).toBeTruthy();
    fireEvent.change(select, { target: { value: kubernetesAllContainers } });
    await waitFor(() => expect(lookup.mock.calls.at(-1)?.[0].container).toBe("*"));
  });

  it("falls back to the first non-init container when the server names no main container", async () => {
    vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValue(logPage({ container: undefined, main_container: undefined, containers: [{ name: "migrate", init: true }, { name: "worker" }] }));
    mount(scenario);
    await screen.findByText("upstream timeout");
    expect((screen.getByRole("combobox", { name: "Log container" }) as HTMLSelectElement).value).toBe("worker");
  });
});

describe("Timeline range and filters", () => {
  const breakEntry = (id: string, at: string): KubernetesTimelineEntry => ({ type: "break", id, at, issue: { ...issue, root: { ...issue.root, uid: id, name: id } } });

  it("keeps the previous rows on screen, dimmed, with a spinner on the range control while a new range loads", async () => {
    window.history.replaceState(null, "", "/agent/kubernetes?view=timeline");
    const next = deferred<KubernetesTimelinePage>();
    const lookup = vi.spyOn(api, "kubernetesTimeline").mockResolvedValueOnce(timelinePage([{ type: "change", id: change.id, at: change.at, change }])).mockReturnValueOnce(next.promise);
    const { container } = mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    const list = await screen.findByRole("list", { name: "Breaks and changes" });
    fireEvent.click(screen.getByRole("button", { name: "6h" }));
    await waitFor(() => expect(lookup).toHaveBeenCalledTimes(2));
    expect(screen.getByRole("list", { name: "Breaks and changes" })).toBe(list);
    expect(screen.getByRole("button", { name: "Open change details" })).toBeTruthy();
    expect(screen.queryByLabelText("Loading projected change history")).toBeNull();
    expect(container.querySelector("[data-timeline-list]")?.className).toContain("opacity-60");
    const range = screen.getByRole("group", { name: "Timeline window" });
    expect(within(range).getByRole("status", { name: "Loading timeline range" })).toBeTruthy();
    await act(async () => { next.resolve(timelinePage([breakEntry("ledger", "2026-10-14T12:01:00Z")])); });
    await screen.findByRole("link", { name: "Open break details" });
    expect(screen.getByRole("list", { name: "Breaks and changes" })).toBe(list);
    expect(container.querySelector("[data-timeline-list]")?.className).not.toContain("opacity-60");
    expect(within(range).queryByRole("status")).toBeNull();
  });

  it("filters to breaks only through the server and keeps the choice in the URL", async () => {
    window.history.replaceState(null, "", "/agent/kubernetes?view=timeline");
    const lookup = vi.spyOn(api, "kubernetesTimeline").mockImplementation(async (options) => options.breaks ? timelinePage([breakEntry("ledger", "2026-10-14T12:01:00Z")], { breaks_only: true, total: 11, counts: { change: 0, crash: 0, break: 11 }, next: "page-2" }) : timelinePage([{ type: "change", id: change.id, at: change.at, change }]));
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    const control = await screen.findByRole("tablist", { name: "Timeline events" });
    expect(within(control).getByRole("tab", { name: "All" }).getAttribute("aria-selected")).toBe("true");
    expect(lookup.mock.calls[0][0].breaks).toBeUndefined();
    fireEvent.click(within(control).getByRole("tab", { name: "Breaks only" }));
    await waitFor(() => expect(lookup.mock.calls.at(-1)?.[0]).toMatchObject({ breaks: 1, cursor: undefined }));
    expect(screen.getByLabelText("Location").textContent).toContain("breaks=1");
    expect(await screen.findByText("11 breaks in 1h")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Open change details" })).toBeNull();
    fireEvent.click(within(control).getByRole("tab", { name: "All" }));
    await waitFor(() => expect(screen.getByLabelText("Location").textContent).not.toContain("breaks="));
    await waitFor(() => expect(lookup.mock.calls.at(-1)?.[0].breaks).toBeUndefined());
  });

  it("keeps break detail mounted and dimmed while its range changes, with no namespace or kind selectors", async () => {
    window.history.replaceState(null, "", `/agent/kubernetes?view=timeline&root_uid=checkout-uid&namespace=shop&center=${issue.broke_at}`);
    vi.spyOn(api, "kubernetesIssues").mockResolvedValue({ items: [issue], totals: {}, truncated: false, sync });
    vi.spyOn(api, "kubernetesRelated").mockResolvedValue({ nodes: [], edges: [], omitted: {}, sync });
    vi.spyOn(api, "kubernetesCrashes").mockResolvedValue(crashPage);
    vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValue(logPage());
    const next = deferred<{ items: typeof change[]; sync: typeof sync }>();
    vi.spyOn(api, "kubernetesChanges").mockResolvedValueOnce({ items: [change], sync }).mockReturnValueOnce(next.promise);
    const { container } = mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    const lane = await screen.findByRole("region", { name: "Timeline lane checkout" });
    const chart = within(lane).getByRole("region", { name: "Resource timeline chart" });
    const logs = screen.getByRole("region", { name: "Logs at break" });
    expect(screen.queryByRole("combobox", { name: "Timeline namespace" })).toBeNull();
    expect(screen.queryByRole("combobox", { name: "Timeline kind" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "6h" }));
    await waitFor(() => expect(within(screen.getByRole("group", { name: "Timeline window" })).getByRole("status", { name: "Loading timeline range" })).toBeTruthy());
    expect(screen.getByRole("region", { name: "Timeline lane checkout" })).toBe(lane);
    expect(within(lane).getByRole("region", { name: "Resource timeline chart" })).toBe(chart);
    expect(screen.getByRole("region", { name: "Logs at break" })).toBe(logs);
    expect(container.querySelector("[data-timeline-chart-area]")?.className).toContain("opacity-60");
    expect(screen.queryByLabelText("Loading related root change history")).toBeNull();
    await act(async () => { next.resolve({ items: [change], sync }); });
    await waitFor(() => expect(container.querySelector("[data-timeline-chart-area]")?.className).not.toContain("opacity-60"));
    expect(within(lane).getByRole("region", { name: "Resource timeline chart" })).toBe(chart);
  });
});

describe("Issues row", () => {
  it("puts the whole row on one centred flex line with same-size Timeline and Investigate buttons", () => {
    const { container } = mount(<BreakRow issue={issue} resource={{ resource_id: "apps~v1~deployments", ...issue.root }} onSelectResource={vi.fn()} collapsed />);
    const row = container.querySelector("[data-issue-row]")!;
    expect(row.className).toMatch(/\bflex\b/);
    expect(row.className).toContain("items-center");
    expect(row.className).not.toContain("items-start");
    const timeline = screen.getByRole("button", { name: "Timeline for checkout" });
    const investigate = screen.getByRole("button", { name: "Investigate checkout" });
    const size = (element: HTMLElement) => element.className.split(/\s+/).filter((name) => /^(sm:)?(h|w|px|py|size)-/.test(name)).sort();
    expect(size(timeline)).toEqual(size(investigate));
    expect(investigate.className).toContain("btn-primary");
    for (const element of [screen.getByRole("button", { name: "Expand checkout" }), screen.getByRole("button", { name: "Open resource checkout" }), screen.getByText("2 of 25 pods affected"), timeline, investigate]) expect(row.contains(element)).toBe(true);
  });
});

describe("Pending resource types", () => {
  it("names pending resource ids as kinds and keeps unknown types readable", () => {
    expect(pendingKindName("core~v1~configmaps")).toBe("ConfigMap");
    expect(pendingKindName("networking.k8s.io~v1~ingresses")).toBe("Ingress");
    expect(pendingKindName("gateway.networking.k8s.io~v1~httproutes")).toBe("HTTPRoute");
    expect(pendingKindName("example.com~v1alpha1~widgets")).toBe("widgets");
    expect([...pendingKindNames(["core~v1~secrets", "core~v1~secrets", "core~v1~serviceaccounts"])]).toEqual(["Secret", "ServiceAccount"]);
    expect(pendingKindNames(undefined).size).toBe(0);
  });

  it("renders a quiet status note only while something is pending", () => {
    const { rerender, container } = render(<KubernetesPendingNote count={3} />);
    const note = screen.getByRole("status");
    expect(note.textContent).toBe("Loading 3 more resource types");
    expect(note.className).toContain("text-ink-400");
    expect(note.className).not.toMatch(/warn|amber|sev-/);
    expect(container.querySelector(".rounded-full.bg-current")).toBeTruthy();
    rerender(<KubernetesPendingNote count={1} />);
    expect(screen.getByRole("status").textContent).toBe("Loading 1 more resource type");
    rerender(<KubernetesPendingNote kinds={["ConfigMap", "Secret"]} />);
    expect(screen.getByRole("status").textContent).toBe("Loading ConfigMap, Secret");
    rerender(<KubernetesPendingNote count={0} />);
    expect(screen.queryByRole("status")).toBeNull();
    rerender(<KubernetesPendingNote kinds={[]} />);
    expect(screen.queryByRole("status")).toBeNull();
  });
});

describe("Review fixes", () => {
  it("opens a sidecar crash on that sidecar's previous logs, not the main container", async () => {
    window.history.replaceState(null, "", "/agent/kubernetes?view=timeline&root_uid=checkout-uid");
    const lookup = vi.spyOn(api, "kubernetesBreakLogs").mockImplementation(async (options) => logPage({ container: options.container ?? "app" }));
    const crash: KubernetesCrashEvent = { id: "sidecar-crash", sequence: 1, cluster: "east", namespace: "shop", pod: "checkout-a", pod_uid: "pod-a", root_uid: "checkout-uid", root_kind: "Deployment", root_name: "checkout", container: "istio-proxy", type: "termination", termination_slot: "previous", reason: "Error", exit_code: 1, restart_count: 3, at: "2026-10-14T12:05:00Z", time_source: "finished_at", observed_at: "2026-10-14T12:05:01Z" };
    const { container } = mount(<KubernetesBreakLogsProvider issue={issue} onSelectResource={vi.fn()}><ResourceTimelineChart issue={issue} changes={[]} crashes={[crash]} since="2026-10-14T11:30:00Z" until="2026-10-14T12:30:00Z" onSelectChange={vi.fn()} /><KubernetesBreakLogs /></KubernetesBreakLogsProvider>);
    await screen.findByText("upstream timeout");
    expect((screen.getByRole("combobox", { name: "Log container" }) as HTMLSelectElement).value).toBe("app");
    fireEvent.click(container.querySelector('[data-event-id="sidecar-crash"]')!);
    fireEvent.click(screen.getByRole("button", { name: "Logs before crash" }));
    await waitFor(() => expect(lookup.mock.calls.at(-1)?.[0]).toMatchObject({ container: "istio-proxy", pods: ["pod-a"], mode: "previous" }));
    expect(screen.getByLabelText("Location").textContent).toContain("logs_container=istio-proxy");
    await waitFor(() => expect((screen.getByRole("combobox", { name: "Log container" }) as HTMLSelectElement).value).toBe("istio-proxy"));
  });

  it("restores the crashed container from the URL on reload", async () => {
    window.history.replaceState(null, "", "/agent/kubernetes?view=timeline&root_uid=checkout-uid&logs_pod=pod-a&logs_mode=previous&logs_container=istio-proxy");
    const lookup = vi.spyOn(api, "kubernetesBreakLogs").mockImplementation(async (options) => logPage({ container: options.container ?? "app" }));
    mount(<KubernetesBreakLogsProvider issue={issue} onSelectResource={vi.fn()}><KubernetesBreakLogs /></KubernetesBreakLogsProvider>);
    await screen.findByText("upstream timeout");
    expect(lookup.mock.calls[0][0]).toMatchObject({ container: "istio-proxy", pods: ["pod-a"], mode: "previous" });
  });

  it("does not dim the list or spin the range control when the window advances on its own", async () => {
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval", "Date"] });
    window.history.replaceState(null, "", "/agent/kubernetes?view=timeline");
    const next = deferred<KubernetesTimelinePage>();
    const lookup = vi.spyOn(api, "kubernetesTimeline").mockResolvedValueOnce(timelinePage([{ type: "change", id: change.id, at: change.at, change }])).mockReturnValueOnce(next.promise);
    const { container } = mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    await screen.findByRole("button", { name: "Open change details" });
    await act(async () => { vi.advanceTimersByTime(30_000); });
    await waitFor(() => expect(lookup).toHaveBeenCalledTimes(2));
    expect(lookup.mock.calls[1][0].since).not.toBe(lookup.mock.calls[0][0].since);
    expect(screen.getByRole("button", { name: "Open change details" })).toBeTruthy();
    expect(container.querySelector("[data-timeline-list]")?.className).not.toContain("opacity-60");
    expect(within(screen.getByRole("group", { name: "Timeline window" })).queryByRole("status")).toBeNull();
    await act(async () => { next.resolve(timelinePage([{ type: "change", id: change.id, at: change.at, change }])); });
    vi.useRealTimers();
  });
});

describe("Issue evidence still loading", () => {
  const pendingIDs = ["core~v1~configmaps", "core~v1~secrets"];
  const loadingSync = { state: "partial", age_s: 0, partial: true, pending: pendingIDs };

  it("treats a sync that is partial only because evidence is loading as settled", () => {
    expect(settledSync(loadingSync)).toMatchObject({ state: "ready", partial: false, pending: pendingIDs });
    expect(settledSync(loadingSync, [{ resource_id: "core~v1~secrets", class: "forbidden" }])).toMatchObject({ state: "partial", partial: true });
    expect(settledSync({ state: "partial", age_s: 0, partial: true })).toMatchObject({ state: "partial", partial: true });
    expect(settledSync({ ...loadingSync, state: "stale" })?.state).toBe("stale");
    const page = { items: [], totals: {}, truncated: false };
    expect(timelineWarnings({ ...page, sync: loadingSync }).map((warning) => warning.content)).not.toContain("Some issue evidence could not be read");
    expect(timelineWarnings({ ...page, sync: { state: "partial", age_s: 0, partial: true } }).map((warning) => warning.content)).toContain("Some issue evidence could not be read");
  });

  it("says Evidence loading on an Issues row instead of hedging about available evidence", async () => {
    vi.spyOn(api, "kubernetesIssues").mockResolvedValue({ items: [{ ...issue, changes: [], pending: pendingIDs }], totals: {}, truncated: false, sync: loadingSync });
    mount(<BreakRow issue={issue} resource={{ resource_id: "apps~v1~deployments", ...issue.root }} onSelectResource={vi.fn()} collapsed sourceSync={sync} />);
    fireEvent.click(screen.getByRole("button", { name: "Expand checkout" }));
    const note = await screen.findByText("Evidence loading");
    expect(note.closest("[data-pending-note]")?.className).not.toMatch(/warn|amber|sev-/);
    expect(screen.queryByText("No recorded change in available evidence")).toBeNull();
    expect(screen.queryByText(/could not be read|could not be captured/)).toBeNull();
  });

  it("keeps the real no-change wording once evidence has loaded", async () => {
    vi.spyOn(api, "kubernetesIssues").mockResolvedValue({ items: [{ ...issue, changes: [] }], totals: {}, truncated: false, sync });
    mount(<BreakRow issue={issue} resource={{ resource_id: "apps~v1~deployments", ...issue.root }} onSelectResource={vi.fn()} collapsed sourceSync={sync} />);
    fireEvent.click(screen.getByRole("button", { name: "Expand checkout" }));
    expect(await screen.findByText("No change on it, its config or its nodes")).toBeTruthy();
    expect(screen.queryByText("Evidence loading")).toBeNull();
  });

  it("does not downgrade the broken summary while evidence kinds load", async () => {
    vi.spyOn(api, "kubernetesIssues").mockResolvedValue({ items: [], totals: {}, truncated: false, broken: { total: 0, by_severity: {} }, sync: loadingSync });
    mount(<TimelineCard windowEnd={Date.parse("2026-10-14T12:06:00Z")} clusterId="east" onSelectView={vi.fn()} onSelectResource={vi.fn()} resourceFor={() => null} />);
    expect(await screen.findByText("No broken workloads")).toBeTruthy();
    expect(screen.queryByText("No broken workloads in available evidence")).toBeNull();
    expect(screen.queryByRole("button", { name: "Resource coverage" })).toBeNull();
  });

  it("shows Evidence loading in break detail while its evidence kinds load", async () => {
    window.history.replaceState(null, "", `/agent/kubernetes?view=timeline&root_uid=checkout-uid&namespace=shop&center=${issue.broke_at}`);
    vi.spyOn(api, "kubernetesIssues").mockResolvedValue({ items: [{ ...issue, pending: pendingIDs }], totals: {}, truncated: false, sync: loadingSync });
    vi.spyOn(api, "kubernetesRelated").mockResolvedValue({ nodes: [], edges: [], omitted: {}, sync });
    vi.spyOn(api, "kubernetesCrashes").mockResolvedValue(crashPage);
    vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValue(logPage());
    vi.spyOn(api, "kubernetesChanges").mockResolvedValue({ items: [change], sync });
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    expect(await screen.findByText("Evidence loading")).toBeTruthy();
    await screen.findByRole("region", { name: "Timeline lane checkout" });
    expect(screen.queryByText(/could not be read/)).toBeNull();
  });

  it("never presents stale data with loading kinds as current", () => {
    const staleLoading = { state: "stale", age_s: 420, partial: true, pending: pendingIDs, pending_kinds: 4 };
    expect(settledSync(staleLoading)).toMatchObject({ state: "stale", partial: false, age_s: 420 });
    expect(settledSync(staleLoading, [{ resource_id: "core~v1~pods", class: "forbidden" }])).toEqual(staleLoading);
    expect(settledSync({ state: "error", age_s: 0, partial: true, pending: pendingIDs })?.state).toBe("error");
    const contents = timelineWarnings({ items: [], totals: {}, truncated: false, sync: staleLoading }).map((warning) => warning.content);
    expect(contents).toContain("Break evidence is stale.");
    expect(contents).not.toContain("Some issue evidence could not be read");
  });

  it.each([
    ["only loading", undefined, "No change on it, its config or its nodes"],
    ["loading plus a real failure", [{ resource_id: "core~v1~configmaps", class: "forbidden" }], "No recorded change in available evidence"],
  ])("settles an Issues row from its page sync when evidence is %s", (_case, failures, wording) => {
    mount(<BreakRow issue={{ ...issue, changes: [] }} resource={{ resource_id: "apps~v1~deployments", ...issue.root }} onSelectResource={vi.fn()} sourceSync={loadingSync} sourceFailures={failures} />);
    expect(screen.getByText(wording)).toBeTruthy();
  });
});

describe("Filter rows", () => {
  const namespaceInput = (name: string) => screen.findByRole("combobox", { name }) as Promise<HTMLInputElement>;

  it.each([
    ["timeline", "Timeline namespace", "Timeline kind"],
    ["issues", "Issue namespace", "Issue severity"],
  ])("gives the %s row one label baseline, control top, height and border box", async (tab, namespaceLabel, pairedLabel) => {
    vi.spyOn(api, "kubernetesTimeline").mockResolvedValue(timelinePage([]));
    vi.spyOn(api, "kubernetesIssues").mockResolvedValue({ items: [], totals: {}, truncated: false, sync });
    vi.spyOn(api, "kubernetesIssueAlerts").mockResolvedValue({ enabled: false, level: "critical", can_write: true, available: true, reason: "", links_ready: true, channels_enabled: true });
    mount(<ToastProvider><KubernetesExplorerTabContent tab={tab as "timeline" | "issues"} onSelectResource={vi.fn()} /></ToastProvider>);
    const namespace = await namespaceInput(namespaceLabel);
    const paired = screen.getByRole("combobox", { name: pairedLabel }) as HTMLInputElement;
    // One control contract for both columns: same top margin, width and border box.
    expect(namespace.className).toBe(paired.className);
    expect(namespace.className).toBe(filterFieldControlClass);
    // One field wrapper for both columns: same label baseline and block layout.
    const namespaceField = namespace.closest("label")!;
    const pairedField = paired.closest("label")!;
    expect(namespaceField.className).toBe(pairedField.className);
    expect(namespaceField.className).toContain(filterFieldClass);
    expect(namespaceField.querySelector("span")?.className).toBe("block");
    expect(pairedField.querySelector("span")?.className).toBe("block");
    // Both sit in the same row, so the two columns share a baseline.
    expect(namespaceField.parentElement).toBe(pairedField.parentElement);
  });

  it("gives the Issues and Timeline filter rows one grid contract at all widths", async () => {
    vi.spyOn(api, "kubernetesTimeline").mockResolvedValue(timelinePage([]));
    vi.spyOn(api, "kubernetesIssues").mockResolvedValue({ items: [], totals: {}, truncated: false, sync });
    vi.spyOn(api, "kubernetesIssueAlerts").mockResolvedValue({ enabled: false, level: "critical", can_write: true, available: true, reason: "", links_ready: true, channels_enabled: true });
    mount(<ToastProvider>
      <KubernetesExplorerTabContent tab="issues" onSelectResource={vi.fn()} />
      <KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />
    </ToastProvider>);
    const issuesRow = (await namespaceInput("Issue namespace")).closest("label")!.parentElement!;
    const timelineRow = (await namespaceInput("Timeline namespace")).closest("label")!.parentElement!;
    // Both rows use the one contract, token for token.
    for (const row of [issuesRow, timelineRow]) for (const token of filterRowClass.split(" ")) expect(row.className.split(/\s+/)).toContain(token);
    const layout = (row: HTMLElement) => row.className.split(/\s+/).filter((token) => /^(grid|grid-cols-\d+|min-w-0|gap-\d+)$/.test(token)).sort();
    expect(layout(issuesRow)).toEqual(layout(timelineRow));
    // Two columns at every width, so neither row stacks its second control.
    expect(layout(issuesRow)).toEqual(["gap-2", "grid", "grid-cols-2", "min-w-0"]);
    for (const row of [issuesRow, timelineRow]) expect(row.className).not.toMatch(/(^|\s)(sm|md|lg):grid-cols-/);
  });

  it("leaves the inline Traffic header control as it was", async () => {
    vi.spyOn(api, "kubernetesTraffic").mockResolvedValue({ available: false, reason: "No flow source connected", window: "15m", unmapped: 0, truncated: false });
    mount(<KubernetesExplorerTabContent tab="traffic" onSelectResource={vi.fn()} />);
    const traffic = await namespaceInput("Traffic namespace");
    expect(traffic.className).toBe("input w-full min-w-0");
    expect(traffic.closest("label")).toBeNull();
  });
});
