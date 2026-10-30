// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, useLocation } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, api, kubernetesAllContainers, type KubernetesBreakLogPage, type KubernetesCrashEvent, type KubernetesCrashPage, type KubernetesIssue, type KubernetesIssuePage, type KubernetesSyncProgress, type KubernetesTimelineEntry, type KubernetesTimelinePage } from "@/lib/api";
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
function persistedRootPage(value = issue, syncState = sync): KubernetesIssuePage {
  return { items: [], totals: {}, truncated: false, sync: syncState, root_context: { root: value.root, occurrence: value, retained: true } };
}
function rootIssueResponses(page: KubernetesIssuePage) {
  return async (options: Parameters<typeof api.kubernetesIssues>[0]) => options?.context === "persisted" ? persistedRootPage(page.items[0] ?? issue, page.sync) : page;
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
    expect(steps.hasAttribute("aria-live")).toBe(false);
    const states = within(steps).getAllByRole("listitem").map((item) => [item.getAttribute("data-step"), item.getAttribute("data-state")]);
    expect(states).toEqual([["connected", "done"], ["listing", "active"], ["building", "waiting"], ["metrics", "waiting"]]);
    expect(within(steps).getByText("Kubernetes v1.30")).toBeTruthy();
    expect(listingCounts().chips).toEqual(["Pods 3,214", "Deployments 412", "Namespaces 27"]);
    expect(listingCounts().text).toBe("Pods 3,214Deployments 412Namespaces 27");
    expect(listingCounts().text).not.toMatch(/[.·•]/);
    // The spacing-only chips read back as one phrase, not as a run-on word.
    expect(screen.getByRole("group", { name: "Pods 3,214, Deployments 412, Namespaces 27" })).toBeTruthy();
    const bar = screen.getByRole("progressbar", { name: "Listing resource types: 3 / 6" });
    expect(bar.getAttribute("aria-valuetext")).toBe("3 / 6 resource types checked");
    expect(screen.queryByText(/Usually under/)).toBeNull();

    rerender(<KubernetesPreparingPanel progress={{ ...progress, step: "building", step_index: 3, steps_done: ["connected", "listing"], counts: { ...progress.counts, Pod: 4100 } }} cluster="prod-eks" />);
    expect(listingCounts().chips).toEqual(["Pods 4,100", "Deployments 412", "Namespaces 27"]);
    expect(listingCounts().text).toBe("Pods 4,100Deployments 412Namespaces 27");
    expect(within(steps).getAllByRole("listitem").map((item) => item.getAttribute("data-state"))).toEqual(["done", "done", "active", "waiting"]);
    expect(bar.getAttribute("aria-valuenow")).toBe("3");
    expect(bar.getAttribute("aria-label")).toBe("Listing resource types: 3 / 6");
  });

  it("retains completed phases and names remaining types after core publication", () => {
    render(<KubernetesPreparingPanel progress={{ ...progress, step: "listing", steps_done: ["connected", "building"], kinds_done: 12, kinds_total: 20, remaining_kinds: ["ConfigMap", "Secret"], kind_status: { ConfigMap: "pending", Secret: "timeout" }, phase_status: { metrics: "unavailable (optional)" } }} />);
    expect(screen.getAllByRole("progressbar")).toHaveLength(1);
    expect(screen.getByRole("progressbar").getAttribute("aria-valuenow")).toBe("12");
    expect(screen.getByText("Remaining: ConfigMap (pending), Secret (timeout)")).toBeTruthy();
    expect(screen.getByText("unavailable (optional)")).toBeTruthy();
    expect([...document.querySelectorAll("[data-state=done]")].map((item) => item.getAttribute("data-step"))).toEqual(["connected", "building"]);
    expect(screen.queryByText(/unknown/i)).toBeNull();
  });

  it("keeps every step label free of separator characters", () => {
    render(<KubernetesPreparingPanel progress={progress} cluster="prod-eks" />);
    const labels = [...document.querySelectorAll<HTMLElement>("[data-first-sync-step-label]")];
    expect(labels).toHaveLength(4);
    for (const label of labels) expect(label.textContent).not.toMatch(/[.·•]/);
  });

  it("keeps all four steps waiting without invented successes without counts or time estimates before worker progress", () => {
    render(<KubernetesPreparingPanel />);
    const steps = within(screen.getByRole("list", { name: "First sync steps" })).getAllByRole("listitem");
    expect(steps.map((item) => item.getAttribute("data-state"))).toEqual(["waiting", "waiting", "waiting", "waiting"]);
    expect(screen.queryByRole("list", { name: "Cluster facts" })).toBeNull();
    expect(screen.queryByText(/unknown/i)).toBeNull();
    expect(document.querySelectorAll("[data-state=done]")).toHaveLength(0);
    expect(screen.queryByText("Usually under a minute for clusters this size")).toBeNull();
    expect(screen.getByRole("progressbar", { name: "Waiting for preparation progress" }).hasAttribute("aria-valuenow")).toBe(false);
  });

  it("stops every animated glyph under prefers-reduced-motion", () => {
    const { container } = render(<KubernetesPreparingPanel progress={progress} />);
    const animated = [...container.querySelectorAll("[data-preparing-motion]")];
    expect(animated.length).toBeGreaterThanOrEqual(3);
    for (const element of animated) expect(/motion-reduce:(animate-none|hidden)/.test(element.getAttribute("class") ?? "")).toBe(true);
  });

  it("gives long listing counts their own mobile row while keeping desktop alignment and every phrase", () => {
    render(<KubernetesPreparingPanel progress={{ ...progress, counts: { Namespace: 27, StatefulSet: 11, DaemonSet: 7, HorizontalPodAutoscaler: 9007199254740991 } }} />);
    const row = document.querySelector<HTMLElement>('[data-step="listing"]')!;
    const label = row.querySelector<HTMLElement>("[data-first-sync-step-label]")!;
    const counts = row.querySelector<HTMLElement>("[data-first-sync-counts]")!;
    const countRow = counts.parentElement!;
    expect(row.className.split(/\s+/)).toEqual(expect.arrayContaining(["flex-wrap", "sm:flex-nowrap"]));
    expect(countRow.className.split(/\s+/)).toEqual(expect.arrayContaining(["basis-full", "pl-9", "sm:basis-auto", "sm:pl-0"]));
    expect(counts.className.split(/\s+/)).toEqual(expect.arrayContaining(["flex-wrap", "justify-start", "sm:justify-end"]));
    expect(label.textContent).toContain("Listing resources");
    const phrases = [...counts.querySelectorAll<HTMLElement>("[data-first-sync-count]")];
    expect(phrases.map(phrase => phrase.textContent)).toEqual(["Namespaces 27", "StatefulSets 11", "DaemonSets 7", "HorizontalPodAutoscalers 9,007,199,254,740,991"]);
    for (const phrase of phrases) {
      expect(phrase.className.split(/\s+/)).toEqual(expect.arrayContaining(["max-w-full", "break-words"]));
      expect(phrase.className).not.toMatch(/truncate|line-clamp|overflow-hidden|whitespace-nowrap/);
      expect(phrase.textContent).not.toMatch(/[.·•]/);
    }
    expect(counts.getAttribute("aria-label")).toBe(phrases.map(phrase => phrase.textContent).join(", "));
    expect(document.querySelectorAll("[data-step]")).toHaveLength(4);
    expect(screen.getAllByRole("progressbar")).toHaveLength(1);
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

  it.each([-120, -5, 0, 0.5])("queries a valid full centered history span near now without future padding (center offset minutes: %s)", async (offset) => {
    const now = Date.parse("2026-10-30T05:30:00Z");
    const clock = vi.spyOn(Date, "now").mockReturnValue(now);
    const center = now + offset * 60_000;
    window.history.replaceState(null, "", `/agent/kubernetes?view=timeline&center=${new Date(center).toISOString()}&namespace=shop&kind=Deployment&breaks=1`);
    const lookup = vi.spyOn(api, "kubernetesTimeline").mockImplementation(async (options) => {
      if (Date.parse(options.until ?? "") > now + 60_000) throw new ApiError(400, "until must not be in the future");
      return timelinePage([breakEntry("checkout", new Date(center).toISOString())], { next: options.cursor ? undefined : "next-window", total: 2 });
    });
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    await waitFor(() => expect(lookup).toHaveBeenCalledTimes(1));
    const initial = lookup.mock.calls[0][0];
    expect(Date.parse(initial.until ?? "")).toBeLessThanOrEqual(now);
    expect(Date.parse(initial.until ?? "") - Date.parse(initial.since ?? "")).toBe(60 * 60_000);
    expect(initial).toMatchObject({ namespace: "shop", kind: "Deployment", breaks: 1, limit: 100 });
    if (offset < -30) expect(Date.parse(initial.until ?? "") - center).toBe(30 * 60_000);
    else expect(initial.until).toBe(new Date(now).toISOString());
    await screen.findByText("checkout");
    clock.mockReturnValue(now + 5 * 60_000);
    fireEvent.click(within(screen.getByRole("navigation", { name: "Timeline pages" })).getByRole("button", { name: "Next page" }));
    await waitFor(() => expect(lookup).toHaveBeenCalledTimes(2));
    expect(lookup.mock.calls[1][0]).toMatchObject({ ...initial, cursor: "next-window" });
  });

  it("keeps an explicitly out-of-range future center invalid instead of silently normalizing it", async () => {
    const now = Date.parse("2026-10-30T05:30:00Z");
    vi.spyOn(Date, "now").mockReturnValue(now);
    window.history.replaceState(null, "", `/agent/kubernetes?view=timeline&center=${new Date(now + 5 * 60_000).toISOString()}`);
    const lookup = vi.spyOn(api, "kubernetesTimeline").mockImplementation(async (options) => {
      if (Date.parse(options.until ?? "") > now + 60_000) throw new ApiError(400, "until must not be in the future");
      return timelinePage([]);
    });
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    await screen.findByRole("button", { name: "Retry Timeline" });
    expect(Date.parse(lookup.mock.calls[0][0].until ?? "")).toBeGreaterThan(now + 60_000);
  });

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
    const rootPage: KubernetesIssuePage = { items: [issue], totals: {}, truncated: false, sync };
    const nextRoot = deferred<KubernetesIssuePage>();
    const historyReads = vi.fn().mockResolvedValueOnce(rootPage).mockReturnValueOnce(nextRoot.promise);
    const rootReads = vi.spyOn(api, "kubernetesIssues").mockImplementation((options) => options?.context === "persisted" ? Promise.resolve(persistedRootPage()) : historyReads(options));
    const graphReads = vi.spyOn(api, "kubernetesRelated").mockResolvedValue({ nodes: [], edges: [], omitted: {}, sync });
    vi.spyOn(api, "kubernetesCrashes").mockResolvedValue(crashPage);
    vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValue(logPage());
    const next = deferred<{ items: typeof change[]; sync: typeof sync }>();
    const changeReads = vi.spyOn(api, "kubernetesChanges").mockResolvedValueOnce({ items: [change], sync }).mockReturnValueOnce(next.promise);
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
    expect(lane.isConnected).toBe(true);
    expect(rootReads.mock.calls.at(-1)?.[0]).toMatchObject({ root_uid: "checkout-uid", namespace: "shop" });
    const requestedWindow = rootReads.mock.calls.at(-1)?.[0];
    expect(Date.parse(requestedWindow?.until ?? "") - Date.parse(requestedWindow?.since ?? "")).toBe(6 * 60 * 60_000);
    await act(async () => { nextRoot.resolve(rootPage); });
    expect(screen.getByRole("region", { name: "Timeline lane checkout" })).toBe(lane);
    expect(within(lane).getByRole("region", { name: "Resource timeline chart" })).toBe(chart);
    expect(screen.getByRole("region", { name: "Logs at break" })).toBe(logs);
    expect(container.querySelector("[data-timeline-chart-area]")?.className).toContain("opacity-60");
    expect(changeReads.mock.calls.every(([options]) => options?.namespace === "shop")).toBe(true);
    expect(graphReads).not.toHaveBeenCalled();
    expect(rootReads.mock.calls.filter(([options]) => options?.context === "persisted")).toHaveLength(1);
    await act(async () => { next.resolve({ items: [change], sync }); });
    await waitFor(() => expect(container.querySelector("[data-timeline-chart-area]")?.className).not.toContain("opacity-60"));
    expect(screen.getByRole("region", { name: "Timeline lane checkout" })).toBe(lane);
    expect(within(lane).getByRole("region", { name: "Resource timeline chart" })).toBe(chart);
    expect(screen.getByRole("region", { name: "Logs at break" })).toBe(logs);
  });

  it("removes the previous root lane when the replacement range is forbidden", async () => {
    window.history.replaceState(null, "", `/agent/kubernetes?view=timeline&root_uid=checkout-uid&namespace=shop&center=${issue.broke_at}`);
    const historyReads = vi.fn().mockResolvedValueOnce({ items: [issue], totals: {}, truncated: false, sync }).mockRejectedValueOnce(new ApiError(403, "Permission reduced"));
    vi.spyOn(api, "kubernetesIssues").mockImplementation((options) => options?.context === "persisted" ? Promise.resolve(persistedRootPage()) : historyReads(options));
    vi.spyOn(api, "kubernetesRelated").mockResolvedValue({ nodes: [], edges: [], omitted: {}, sync });
    vi.spyOn(api, "kubernetesCrashes").mockResolvedValue(crashPage);
    vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValue(logPage());
    vi.spyOn(api, "kubernetesChanges").mockResolvedValue({ items: [change], sync });
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    await screen.findByRole("region", { name: "Timeline lane checkout" });
    fireEvent.click(screen.getByRole("button", { name: "6h" }));
    await screen.findByText("Root history unavailable.");
    expect(screen.queryByRole("region", { name: "Timeline lane checkout" })).toBeNull();
    expect(screen.queryByText("upstream timeout")).toBeNull();
  });
});

describe("Persisted root identity independent of historical windows", () => {
  it("keeps persisted identity when the selected window predates capture, without inventing markers or totals", async () => {
    window.history.replaceState(null, "", "/agent/kubernetes?view=timeline&root_uid=checkout-uid&namespace=shop&center=2020-01-01T00:00:00Z");
    const contextPage = persistedRootPage({ ...issue, first_seen: "2020-01-01T00:00:00Z", changes: [{ change, relation: "same_object", lead_seconds: 1 }] });
    const history = { items: [], totals: {}, truncated: false, sync };
    const lookup = vi.spyOn(api, "kubernetesIssues").mockImplementation(async options => options?.context === "persisted" ? contextPage : history);
    const changes = vi.spyOn(api, "kubernetesChanges").mockResolvedValue({ items: [], gaps: [{ from: "2020-01-01T00:00:00Z", to: "2026-10-14T12:00:00Z" }], sync });
    const crashes = vi.spyOn(api, "kubernetesCrashes").mockResolvedValue(crashPage);
    const related = vi.spyOn(api, "kubernetesRelated");
    const { client, container } = mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    await screen.findByText("No captured markers in this range.");
    expect(screen.getByText("Deployment shop/checkout")).toBeTruthy();
    const lane = screen.getByRole("region", { name: "Timeline lane checkout" });
    const chart = within(lane).getByRole("region", { name: "Resource timeline chart" });
    expect(container.querySelectorAll("[data-marker]")).toHaveLength(0);
    expect(screen.queryByText(/Root not found/)).toBeNull();
    expect(related).not.toHaveBeenCalled();
    const metadata = lookup.mock.calls.filter(([options]) => options?.context === "persisted");
    expect(metadata).toHaveLength(1);
    expect(metadata[0][0]).toEqual({ context: "persisted", root_uid: "checkout-uid", namespace: "shop" });
    const contextKey = kubernetesKey("east", "kubernetes-issues", "timeline-root-context", "checkout-uid", "shop");
    expect(client.getQueryData(contextKey)).toMatchObject({ items: [], totals: {}, root_context: { retained: true, root: issue.root } });
    expect(changes.mock.calls[0][0]).toMatchObject({ root_uid: "checkout-uid", namespace: "shop", since: "2019-12-31T23:30:00.000Z", until: "2020-01-01T00:30:00.000Z" });
    expect(crashes.mock.calls[0][0]).toMatchObject({ root_uid: "checkout-uid", namespace: "shop" });
    fireEvent.click(screen.getByRole("button", { name: "6h" }));
    await waitFor(() => expect(changes).toHaveBeenCalledTimes(2));
    expect(lookup.mock.calls.filter(([options]) => options?.context === "persisted")).toHaveLength(1);
    expect(screen.getByRole("region", { name: "Timeline lane checkout" })).toBe(lane);
    expect(within(lane).getByRole("region", { name: "Resource timeline chart" })).toBe(chart);
    expect(history).toEqual({ items: [], totals: {}, truncated: false, sync });
  });

  it.each([false, true])("distinguishes complete context absence from unresolved retained evidence (partial: %s)", async partial => {
    window.history.replaceState(null, "", "/agent/kubernetes?view=timeline&root_uid=checkout-uid&namespace=shop");
    vi.spyOn(api, "kubernetesIssues").mockResolvedValue({ items: [], totals: {}, truncated: false, sync: { state: partial ? "partial" : "ready", age_s: 0, partial }, partial_failures: partial ? [{ class: "unavailable" }] : [] });
    const changes = vi.spyOn(api, "kubernetesChanges");
    const crashes = vi.spyOn(api, "kubernetesCrashes");
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    await screen.findByText(partial ? "Retained root context is incomplete." : "No retained root context for this selection.");
    expect(screen.queryByText(/Root not found/)).toBeNull();
    expect(screen.queryByText("No captured markers in this range.")).toBeNull();
    expect(changes).not.toHaveBeenCalled();
    expect(crashes).not.toHaveBeenCalled();
  });

  it.each(["root UID", "occurrence UID", "namespace", "identity name"])("rejects mismatched persisted context: %s", async mismatch => {
    window.history.replaceState(null, "", "/agent/kubernetes?view=timeline&root_uid=checkout-uid&namespace=shop");
    const value = persistedRootPage().root_context!;
    const context = { ...value, root: { ...value.root }, occurrence: { ...value.occurrence, root: { ...value.occurrence.root } } };
    if (mismatch === "root UID") context.root.uid = "foreign-uid";
    if (mismatch === "occurrence UID") context.occurrence.root.uid = "foreign-uid";
    if (mismatch === "namespace") { context.root.namespace = "other"; context.occurrence.root.namespace = "other"; }
    if (mismatch === "identity name") context.root.name = "foreign-name";
    vi.spyOn(api, "kubernetesIssues").mockResolvedValue({ ...persistedRootPage(), root_context: context });
    const changes = vi.spyOn(api, "kubernetesChanges");
    const crashes = vi.spyOn(api, "kubernetesCrashes");
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    await screen.findByRole("button", { name: "Retry root context" });
    expect(screen.queryByText(/foreign-name|foreign-uid/)).toBeNull();
    expect(screen.queryByRole("region", { name: "Timeline lane checkout" })).toBeNull();
    expect(changes).not.toHaveBeenCalled();
    expect(crashes).not.toHaveBeenCalled();
  });

  it("preserves persisted identity while reporting a separate historical-window error", async () => {
    window.history.replaceState(null, "", "/agent/kubernetes?view=timeline&root_uid=checkout-uid&namespace=shop");
    vi.spyOn(api, "kubernetesIssues").mockImplementation(async options => {
      if (options?.context === "persisted") return persistedRootPage();
      throw new ApiError(503, "history storage unavailable");
    });
    vi.spyOn(api, "kubernetesChanges").mockResolvedValue({ items: [], sync });
    vi.spyOn(api, "kubernetesCrashes").mockResolvedValue(crashPage);
    const related = vi.spyOn(api, "kubernetesRelated");
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    await screen.findByText("Root history unavailable.");
    expect(screen.getByText("Deployment shop/checkout")).toBeTruthy();
    expect(screen.queryByText("Retained root context is unavailable.")).toBeNull();
    expect(screen.queryByText(/Root not found/)).toBeNull();
    expect(related).not.toHaveBeenCalled();
  });

  it.each([["issues", true], ["changes", true], ["crashes", true], ["crashes", false]] as const)("reproves date-free context before recovering a %s-only history denial (explicit namespace: %s)", async (route, explicitNamespace) => {
    window.history.replaceState(null, "", `/agent/kubernetes?view=timeline&root_uid=checkout-uid${explicitNamespace ? "&namespace=shop" : ""}&kind=Deployment&center=${issue.broke_at}`);
    let denied = false;
    let holdContext = false;
    const nextContext = deferred<KubernetesIssuePage>();
    const issues = vi.spyOn(api, "kubernetesIssues").mockImplementation(async options => {
      if (options?.context === "persisted") return holdContext ? nextContext.promise : persistedRootPage();
      if (denied && route === "issues") throw new ApiError(403, "history permission denied");
      return { items: [issue], totals: {}, truncated: false, sync };
    });
    const changes = vi.spyOn(api, "kubernetesChanges").mockImplementation(async options => {
      if (denied && route === "changes") throw new ApiError(403, "history permission denied");
      return { items: [{ ...change, id: options?.cursor ? "older-change" : change.id }], next: options?.cursor ? undefined : "older", sync };
    });
    const crashes = vi.spyOn(api, "kubernetesCrashes").mockImplementation(async () => {
      if (denied && route === "crashes") throw new ApiError(403, "history permission denied");
      return crashPage;
    });
    const related = vi.spyOn(api, "kubernetesRelated");
    const { client } = mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    await screen.findByRole("button", { name: /Deployment checkout:.* at / });
    fireEvent.click(screen.getByRole("button", { name: "Next page" }));
    await waitFor(() => expect(changes.mock.calls.at(-1)?.[0]?.cursor).toBe("older"));
    await screen.findByRole("button", { name: /Deployment checkout:.* at / });
    const selected = changes.mock.calls.at(-1)![0];
    const contextKey = kubernetesKey("east", "kubernetes-issues", "timeline-root-context", "checkout-uid", explicitNamespace ? "shop" : "");
    const otherKey = kubernetesKey("east", "kubernetes-issues", "timeline-root-context", "other-root", "shop");
    client.setQueryData(otherKey, { identity: "unaffected" });
    const target = client.getQueryCache().findAll({ queryKey: kubernetesKey("east", `kubernetes-${route}`), predicate: query => query.isActive() })
      .find(query => route === "crashes" || query.queryKey[3] === "timeline-root")!;
    denied = true;
    await act(async () => { await client.invalidateQueries({ queryKey: target.queryKey, exact: true }); });
    const retry = await screen.findByRole("button", { name: "Retry root context" });
    expect(screen.queryByText("Deployment shop/checkout")).toBeNull();
    expect(screen.queryByRole("region", { name: "Timeline lane checkout" })).toBeNull();
    expect(client.getQueryData(contextKey)).toBeUndefined();
    const contextCalls = () => issues.mock.calls.filter(([options]) => options?.context === "persisted");
    const historyCounts = () => [issues.mock.calls.filter(([options]) => !options?.context).length, changes.mock.calls.length, crashes.mock.calls.length];
    const beforeRetry = historyCounts();
    const beforeContext = contextCalls().length;
    denied = false;
    holdContext = true;
    fireEvent.click(retry);
    await waitFor(() => expect(contextCalls()).toHaveLength(beforeContext + 1));
    expect(historyCounts()).toEqual(beforeRetry);
    expect(screen.queryByText("Deployment shop/checkout")).toBeNull();
    holdContext = false;
    await act(async () => { nextContext.resolve(persistedRootPage()); });
    expect(await screen.findByRole("button", { name: /Deployment checkout:.* at / })).toBeTruthy();
    expect(screen.getByText("Deployment shop/checkout")).toBeTruthy();
    expect(historyCounts().every((count, index) => count > beforeRetry[index])).toBe(true);
    expect(contextCalls().at(-1)?.[0]).toEqual({ context: "persisted", root_uid: "checkout-uid", namespace: explicitNamespace ? "shop" : undefined });
    expect(changes.mock.calls.at(-1)?.[0]).toEqual(selected);
    expect(crashes.mock.calls.every(([options]) => options?.namespace === "shop")).toBe(true);
    expect(client.getQueryData(contextKey)).toMatchObject({ items: [], totals: {}, root_context: { retained: true } });
    expect(client.getQueryData(otherKey)).toEqual({ identity: "unaffected" });
    expect(related).not.toHaveBeenCalled();
  });

  it.each([401, 403])("keeps a permanent history HTTP %s fenced and permits another context-first retry", async status => {
    window.history.replaceState(null, "", `/agent/kubernetes?view=timeline&root_uid=checkout-uid&namespace=shop&center=${issue.broke_at}`);
    let denied = false;
    const issues = vi.spyOn(api, "kubernetesIssues").mockImplementation(rootIssueResponses({ items: [issue], totals: {}, truncated: false, sync }));
    const changes = vi.spyOn(api, "kubernetesChanges").mockImplementation(async () => {
      if (denied) throw new ApiError(status, "history permission denied");
      return { items: [change], sync };
    });
    vi.spyOn(api, "kubernetesCrashes").mockResolvedValue(crashPage);
    const { client } = mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    await screen.findByRole("button", { name: /Deployment checkout:.* at / });
    denied = true;
    await act(async () => { await client.invalidateQueries({ queryKey: kubernetesKey("east", "kubernetes-changes", "timeline-root"), exact: false }); });
    for (let attempt = 0; attempt < 2; attempt++) {
      const retry = await screen.findByRole("button", { name: "Retry root context" });
      fireEvent.click(retry);
      await waitFor(() => expect(changes).toHaveBeenCalledTimes(3 + attempt));
      await waitFor(() => expect((screen.getByRole("button", { name: "Retry root context" }) as HTMLButtonElement).disabled).toBe(false));
      expect(screen.queryByText("Deployment shop/checkout")).toBeNull();
      expect(screen.queryByRole("region", { name: "Timeline lane checkout" })).toBeNull();
      expect(client.getQueryData(kubernetesKey("east", "kubernetes-issues", "timeline-root-context", "checkout-uid", "shop"))).toBeUndefined();
    }
    expect(issues.mock.calls.filter(([options]) => options?.context === "persisted")).toHaveLength(3);
  });

  it("fences cached retained identity and late dependent reads on context403 without touching another root", async () => {
    window.history.replaceState(null, "", `/agent/kubernetes?view=timeline&root_uid=checkout-uid&namespace=shop&center=${issue.broke_at}`);
    let denied = false;
    vi.spyOn(api, "kubernetesIssues").mockImplementation(async options => {
      if (options?.context === "persisted") {
        if (denied) throw new ApiError(403, "permission reduced");
        return persistedRootPage();
      }
      return { items: [issue], totals: {}, truncated: false, sync };
    });
    vi.spyOn(api, "kubernetesChanges").mockResolvedValue({ items: [change], sync });
    vi.spyOn(api, "kubernetesCrashes").mockResolvedValue(crashPage);
    vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValue(logPage());
    const { client } = mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    await screen.findByRole("button", { name: /Deployment checkout:.* at / });
    const contextKey = kubernetesKey("east", "kubernetes-issues", "timeline-root-context", "checkout-uid", "shop");
    const otherKey = kubernetesKey("east", "kubernetes-issues", "timeline-root-context", "different-uid", "shop");
    client.setQueryData(otherKey, { retained: "other-root" });
    const lateKey = kubernetesKey("east", "kubernetes-changes", "timeline-root", "checkout-uid", "shop", "late");
    let finish!: (value: { items: typeof change[]; sync: typeof sync }) => void;
    const late = client.fetchQuery({ queryKey: lateKey, queryFn: () => new Promise<{ items: typeof change[]; sync: typeof sync }>(resolve => { finish = resolve; }) }).catch(() => undefined);
    denied = true;
    await act(async () => { await client.invalidateQueries({ queryKey: contextKey, exact: true }); });
    await screen.findByText("Retained root context is unavailable.");
    expect(screen.queryByText("Deployment shop/checkout")).toBeNull();
    expect(screen.queryByText("upstream timeout")).toBeNull();
    expect(screen.queryByRole("region", { name: "Timeline lane checkout" })).toBeNull();
    expect(client.getQueryData(contextKey)).toBeUndefined();
    expect(client.getQueryData(otherKey)).toEqual({ retained: "other-root" });
    await act(async () => { finish({ items: [change], sync }); await late; });
    expect(client.getQueryData(lateKey)).toBeUndefined();
    denied = false;
    fireEvent.click(screen.getByRole("button", { name: "Retry root context" }));
    expect(await screen.findByText("Deployment shop/checkout")).toBeTruthy();
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
    vi.spyOn(api, "kubernetesIssues").mockImplementation(rootIssueResponses({ items: [{ ...issue, changes: [], pending: pendingIDs }], totals: {}, truncated: false, sync: loadingSync }));
    mount(<BreakRow issue={issue} resource={{ resource_id: "apps~v1~deployments", ...issue.root }} onSelectResource={vi.fn()} collapsed sourceSync={sync} />);
    fireEvent.click(screen.getByRole("button", { name: "Expand checkout" }));
    const note = await screen.findByText("Evidence loading");
    expect(note.closest("[data-pending-note]")?.className).not.toMatch(/warn|amber|sev-/);
    expect(screen.queryByText("No recorded change in available evidence")).toBeNull();
    expect(screen.queryByText(/could not be read|could not be captured/)).toBeNull();
  });

  it("keeps the real no-change wording once evidence has loaded", async () => {
    vi.spyOn(api, "kubernetesIssues").mockImplementation(rootIssueResponses({ items: [{ ...issue, changes: [] }], totals: {}, truncated: false, sync }));
    mount(<BreakRow issue={issue} resource={{ resource_id: "apps~v1~deployments", ...issue.root }} onSelectResource={vi.fn()} collapsed sourceSync={sync} />);
    fireEvent.click(screen.getByRole("button", { name: "Expand checkout" }));
    expect(await screen.findByText("No change on it, its config or its nodes")).toBeTruthy();
    expect(screen.queryByText("Evidence loading")).toBeNull();
  });

  it("does not claim a healthy broken summary while evidence kinds load", async () => {
    vi.spyOn(api, "kubernetesIssues").mockImplementation(rootIssueResponses({ items: [], totals: {}, truncated: false, broken: { total: 0, by_severity: {} }, sync: loadingSync }));
    mount(<TimelineCard windowEnd={Date.parse("2026-10-14T12:06:00Z")} clusterId="east" onSelectView={vi.fn()} onSelectResource={vi.fn()} resourceFor={() => null} />);
    await screen.findAllByText("Unknown");
    const broken = screen.getByRole("button", { name: /^Broken/ });
    expect(within(broken).getAllByText("Unknown")).toHaveLength(2);
    expect(within(broken).queryByText("0")).toBeNull();
    expect(screen.queryByText(/No broken workloads/)).toBeNull();
    expect(screen.queryByRole("button", { name: "Resource coverage" })).toBeNull();
  });

  it("shows Evidence loading in break detail while its evidence kinds load", async () => {
    window.history.replaceState(null, "", `/agent/kubernetes?view=timeline&root_uid=checkout-uid&namespace=shop&center=${issue.broke_at}`);
    vi.spyOn(api, "kubernetesIssues").mockImplementation(rootIssueResponses({ items: [{ ...issue, pending: pendingIDs }], totals: {}, truncated: false, sync: loadingSync }));
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
    vi.spyOn(api, "kubernetesIssues").mockImplementation(rootIssueResponses({ items: [], totals: {}, truncated: false, sync }));
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
    vi.spyOn(api, "kubernetesIssues").mockImplementation(rootIssueResponses({ items: [], totals: {}, truncated: false, sync }));
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
