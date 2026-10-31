// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, useLocation } from "react-router-dom";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api, ApiError, type KubernetesBreakLogPage, type KubernetesChangesPage, type KubernetesCrashEvent, type KubernetesCrashPage, type KubernetesIssue, type KubernetesIssuePage, type KubernetesTimelineEntry, type KubernetesTimelinePage } from "@/lib/api";
import { KubernetesClusterContext, kubernetesKey, useKubernetesCrashes } from "@/lib/useKubernetesCluster";
import { BreakRow, BreakTimelineLane, CrashJournal, ResourceTimelineChart, TimelineCard } from "./KubernetesBreakTimeline";
import { KubernetesExplorerTabContent } from "./KubernetesExplorerViews";
import { KubernetesWarnings } from "./KubernetesWarnings";
import { KubernetesBreakLogs, KubernetesBreakLogsProvider } from "./KubernetesBreakLogs";
import { ToastProvider } from "@/components/Toast";
import { breakLogWindow } from "@/lib/useKubernetesBreakLogs";

const issue: KubernetesIssue = { root: { kind: "Deployment", namespace: "shop", name: "checkout", uid: "checkout-uid" }, rule: "pod.crash_loop", severity: "critical", count: 7, of: 7, broken: true, broke_at: "2026-10-14T12:04:00Z", history: "complete", evidence: { terminations: [{ reason: "OOMKilled", exit_code: 137, count: 6, last: "2026-10-14T12:05:00Z" }], warning_events: 3, from: "2026-10-14T11:34:00Z", to: "2026-10-14T12:06:00Z" } };
const resource = { resource_id: "apps~v1~deployments", ...issue.root };
const change = { id: "image", cluster: "east", kind: "Deployment", namespace: "shop", name: "checkout", uid: "checkout-uid", type: "image_changed" as const, fields: [{ path: "image", from: "v41", to: "v42" }], at: "2026-10-14T12:00:00Z", source: "reconstructed" as const };
function timelinePage(items: KubernetesTimelineEntry[] = [], overrides: Partial<KubernetesTimelinePage> = {}): KubernetesTimelinePage {
  const counts = { change: 0, crash: 0, break: 0 };
  for (const entry of items) counts[entry.type]++;
  return {
    items, counts, total: items.length, crash_count: counts.crash, gaps: [],
    sync: { state: "ready", age_s: 0, partial: false },
    since: "2026-10-14T11:06:00.000Z", until: "2026-10-14T12:06:00.000Z",
    ...overrides,
  };
}
const changeEntry = (value = change): KubernetesTimelineEntry => ({ type: "change", id: value.id, at: value.at, change: value });
function persistedRootPage(value = issue, sync: KubernetesIssuePage["sync"] = { state: "ready", age_s: 0, partial: false }): KubernetesIssuePage {
  return { items: [], totals: {}, truncated: false, sync, selected_occurrence: value, selection_status: "resolved", root_context: { root: value.root, occurrence: value, retained: true } };
}
function rootIssueResponses(page: KubernetesIssuePage) {
  return async (options: Parameters<typeof api.kubernetesIssues>[0]) => options?.context === "persisted" ? persistedRootPage(page.items[0] ?? issue, page.sync) : page;
}
function Location() { const location = useLocation(); return <output aria-label="Location">{location.pathname}{location.search}</output>; }
function ClickedBreak({ value }: { value: KubernetesIssue }) {
  const location = useLocation();
  return new URLSearchParams(location.search).get("view") === "timeline"
    ? <KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />
    : <BreakRow issue={value} resource={resource} onSelectResource={vi.fn()} />;
}
function mount(element: React.ReactNode, cluster = "east") {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return { client, ...render(<QueryClientProvider client={client}><KubernetesClusterContext.Provider value={{ client: api, cluster, multiple: true, clusters: [], key: (...parts) => kubernetesKey(cluster, ...parts) }}><MemoryRouter initialEntries={[`${window.location.pathname}${window.location.search}`]}>{element}<Location /></MemoryRouter></KubernetesClusterContext.Provider></QueryClientProvider>) };
}
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.useRealTimers(); window.history.replaceState(null, "", "/"); });
beforeEach(() => {
  vi.spyOn(api, "kubernetesNamespaces").mockResolvedValue({ items: ["shop", "other"], sync: { state: "ready", age_s: 0, partial: false } });
});

describe("Logs at break", () => {
  const page: KubernetesBreakLogPage = {
    window: { from: "2026-10-14T11:59:00Z", to: "2026-10-14T12:06:00Z" },
    pods: [{ name: "checkout-deleted", uid: "", short: "deleted", state: "unknown" }, { name: "checkout-live", uid: "pod-live", short: "live", state: "running" }],
    lines: [{ at: "2026-10-14T12:03:00Z", pod: "checkout-deleted", pod_uid: "", container: "api", init: false, slot: "previous", level: "error", text: "timeout <script>not markup</script>", source: "linked" }],
    markers: [{ at: issue.broke_at!, type: "break", label: "Deployment checkout unavailable" }, { at: "2026-10-14T12:05:00Z", type: "crash", label: "api Error exit 1" }],
    counts: { all: 3, errors: 2, warnings: 1 }, rate: [{ minute: "2026-10-14T12:03:00Z", total: 3, errors: 2 }],
    patterns: [{ template: "timeout <value>", level: "error", count: 2 }], sources: [{ kind: "linked", name: "loki-prod", type: "loki", at: "2026-10-14T12:06:00Z" }],
    coverage: { state: "partial", gaps: [{ from: "2026-10-14T11:59:00Z", to: "2026-10-14T12:01:00Z" }], reason: "restarted_since", tip: true }, next: "opaque-next",
  };
  function scenario(onSelect = vi.fn(), crashes: KubernetesCrashEvent[] = []) {
    return <KubernetesBreakLogsProvider issue={issue} onSelectResource={onSelect}><ResourceTimelineChart issue={issue} changes={[]} crashes={crashes} since="2026-10-14T11:30:00Z" until="2026-10-14T12:30:00Z" onSelectChange={vi.fn()} /><KubernetesBreakLogs /></KubernetesBreakLogsProvider>;
  }

  async function advance(milliseconds = 0) {
    let remaining = milliseconds;
    do {
      const step = Math.min(1_000, remaining);
      await act(async () => { await vi.advanceTimersByTimeAsync(step); });
      remaining -= step;
    } while (remaining > 0);
  }

  it("refreshes a future window from page one with fresh cursors and stops after settlement", async () => {
    vi.useFakeTimers(); vi.setSystemTime(new Date(issue.broke_at!));
    let revision = 0;
    const lookup = vi.spyOn(api, "kubernetesBreakLogs").mockImplementation(async (options) => {
      if (!options.cursor) revision++;
      return { ...page, lines: [{ ...page.lines[0], text: options.cursor ? `older page ${revision}` : `capture ${revision}` }], next: options.cursor ? undefined : `cursor-${revision}` };
    });
    const { client } = mount(scenario()); await advance();
    fireEvent.click(screen.getByRole("button", { name: "Load more logs" })); await advance();
    expect(lookup.mock.calls.at(-1)?.[0].cursor).toBe("cursor-1");
    expect(screen.getByText("older page 1")).toBeTruthy();
    await advance(10_000);
    expect(lookup.mock.calls.at(-1)?.[0].cursor).toBeUndefined();
    expect(client.getQueryCache().findAll().find((query) => query.queryKey.includes("kubernetes-break-logs"))?.state.data).toMatchObject({ pages: [{ lines: [{ text: "capture 2" }] }], pageParams: [undefined] });
    await advance(1);
    expect(screen.queryByText("older page 1")).toBeNull();
    expect(screen.getByText("capture 2")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Load more logs" })); await advance();
    expect(lookup.mock.calls.at(-1)?.[0].cursor).toBe("cursor-2");
    await advance(180_000);
    const settledCalls = lookup.mock.calls.length;
    expect(settledCalls).toBeGreaterThan(4);
    await advance(60_000);
    expect(lookup).toHaveBeenCalledTimes(settledCalls);
  });

  it("collects the scheduled capture for an active break even when the requested end is past", async () => {
    vi.useFakeTimers(); vi.setSystemTime(new Date("2026-10-14T12:05:10Z"));
    window.history.replaceState(null, "", "/?logs_from=2026-10-14T11:59:00Z&logs_to=2026-10-14T12:04:00Z");
    const lookup = vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValue(page);
    mount(scenario()); await advance();
    await advance(10_000);
    expect(lookup).toHaveBeenCalledTimes(2);
    await advance(120_000);
    const settledCalls = lookup.mock.calls.length;
    await advance(60_000);
    expect(lookup).toHaveBeenCalledTimes(settledCalls);
  });

  it("keeps the first page and scroll position visible while refreshing from its original page parameter", async () => {
    vi.useFakeTimers(); vi.setSystemTime(new Date(issue.broke_at!));
    let finishRefresh: (value: KubernetesBreakLogPage) => void = () => undefined;
    const lookup = vi.spyOn(api, "kubernetesBreakLogs")
      .mockResolvedValueOnce(page)
      .mockResolvedValueOnce({ ...page, lines: [{ ...page.lines[0], text: "older cursor line" }], next: undefined })
      .mockImplementationOnce(() => new Promise<KubernetesBreakLogPage>((resolve) => { finishRefresh = resolve; }))
      .mockResolvedValue({ ...page, next: undefined });
    const { client } = mount(scenario()); await advance();
    fireEvent.click(screen.getByRole("button", { name: "Load more logs" })); await advance();
    const viewer = screen.getByRole("log");
    viewer.scrollTop = 80;
    await advance(10_000);
    expect(lookup).toHaveBeenCalledTimes(3);
    expect(lookup.mock.calls[2][0].cursor).toBeUndefined();
    expect(client.getQueryCache().findAll().find((query) => query.queryKey.includes("kubernetes-break-logs"))?.state.data).toMatchObject({ pages: [page], pageParams: [undefined] });
    await advance(1);
    expect(screen.getByText("timeout <script>not markup</script>")).toBeTruthy();
    expect(screen.queryByText("older cursor line")).toBeNull();
    expect(screen.getByRole("log")).toBe(viewer);
    expect(viewer.scrollTop).toBe(80);
    expect(client.getQueryCache().findAll().find((query) => query.queryKey.includes("kubernetes-break-logs"))?.state.data).toMatchObject({ pageParams: [undefined] });
    await act(async () => finishRefresh({ ...page, lines: [{ ...page.lines[0], text: "refreshed first page" }], next: "fresh-cursor" }));
    await advance();
    expect(screen.getByText("refreshed first page")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Load more logs" })); await advance();
    expect(lookup.mock.calls.at(-1)?.[0].cursor).toBe("fresh-cursor");
  });

  it("does not poll a historical settled window", async () => {
    vi.useFakeTimers(); vi.setSystemTime(new Date("2026-10-14T13:00:00Z"));
    const lookup = vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValue(page);
    mount(scenario()); await advance();
    await advance(180_000);
    expect(lookup).toHaveBeenCalledTimes(1);
  });

  it("resumes a hidden historical initial load immediately without periodic polling", async () => {
    vi.useFakeTimers(); vi.setSystemTime(new Date("2026-10-14T13:00:00Z"));
    const timers = vi.spyOn(globalThis, "setTimeout");
    const visibility = vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
    let finishLoad: (value: KubernetesBreakLogPage) => void = () => undefined;
    const lookup = vi.spyOn(api, "kubernetesBreakLogs").mockImplementation(() => new Promise<KubernetesBreakLogPage>((resolve) => { finishLoad = resolve; }));
    mount(scenario()); await advance();
    await advance(30_000);
    expect(lookup).toHaveBeenCalledTimes(1);
    expect(timers).not.toHaveBeenCalledWith(expect.any(Function), 10_000);
    const initialSignal = lookup.mock.calls[0][1];
    visibility.mockReturnValue("hidden"); fireEvent(document, new Event("visibilitychange")); await advance();
    expect(initialSignal?.aborted).toBe(true);
    await advance(60_000);
    expect(lookup).toHaveBeenCalledTimes(1);
    visibility.mockReturnValue("visible"); fireEvent(document, new Event("visibilitychange")); await advance();
    expect(lookup).toHaveBeenCalledTimes(2);
    expect(lookup.mock.calls[1][0].cursor).toBeUndefined();
    await advance(60_000);
    expect(lookup).toHaveBeenCalledTimes(2);
    expect(timers).not.toHaveBeenCalledWith(expect.any(Function), 10_000);
    await act(async () => finishLoad(page)); await advance();
    expect(screen.getByText("timeout <script>not markup</script>")).toBeTruthy();
    await advance(60_000);
    expect(lookup).toHaveBeenCalledTimes(2);
  });

  it("does not postpone polling when the parent rerenders with the same query scope", async () => {
    vi.useFakeTimers(); vi.setSystemTime(new Date(issue.broke_at!));
    const lookup = vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValue(page);
    function Parent() {
      const [, update] = useState(0);
      return <><button onClick={() => update((count) => count + 1)}>Refresh parent</button>{scenario()}</>;
    }
    mount(<Parent />); await advance(); await advance(9_000);
    fireEvent.click(screen.getByRole("button", { name: "Refresh parent" }));
    await advance(1_000);
    expect(lookup).toHaveBeenCalledTimes(2);
  });

  it("waits for paging and still refreshes page one when a cursor response crosses settlement", async () => {
    vi.useFakeTimers(); vi.setSystemTime(new Date(issue.broke_at!));
    let finishPage: (value: KubernetesBreakLogPage) => void = () => undefined;
    const lookup = vi.spyOn(api, "kubernetesBreakLogs").mockImplementation(async (options) => {
      if (options.cursor) return new Promise<KubernetesBreakLogPage>((resolve) => { finishPage = resolve; });
      return page;
    });
    mount(scenario()); await advance(); await advance(170_000);
    fireEvent.click(screen.getByRole("button", { name: "Load more logs" })); await advance();
    const pagingCalls = lookup.mock.calls.length;
    await advance(20_000);
    expect(lookup).toHaveBeenCalledTimes(pagingCalls);
    await act(async () => finishPage({ ...page, lines: [{ ...page.lines[0], text: "old cursor page" }], next: undefined }));
    await advance(20_000);
    expect(lookup).toHaveBeenCalledTimes(pagingCalls + 1);
    expect(lookup.mock.calls.at(-1)?.[0].cursor).toBeUndefined();
    expect(screen.queryByText("old cursor page")).toBeNull();
    await advance(30_000);
    expect(lookup).toHaveBeenCalledTimes(pagingCalls + 1);
  });

  it("pauses while hidden, cancels in-flight reads and settles on return without stale pages", async () => {
    vi.useFakeTimers(); vi.setSystemTime(new Date(issue.broke_at!));
    const visibility = vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
    const lookup = vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValueOnce(page).mockImplementation((_options, signal) => new Promise((resolve) => {
      signal?.addEventListener("abort", () => resolve({ ...page, lines: [{ ...page.lines[0], text: "stale cancelled result" }] }), { once: true });
    }));
    mount(scenario()); await advance();
    await advance(10_000);
    const signal = lookup.mock.calls.at(-1)?.[1];
    expect(signal?.aborted).toBe(false);
    expect(screen.getByText("timeout <script>not markup</script>")).toBeTruthy();
    visibility.mockReturnValue("hidden"); fireEvent(document, new Event("visibilitychange")); await advance();
    expect(signal?.aborted).toBe(true);
    expect(screen.getByText("timeout <script>not markup</script>")).toBeTruthy();
    await advance(240_000);
    expect(lookup).toHaveBeenCalledTimes(2);
    lookup.mockResolvedValue({ ...page, lines: [{ ...page.lines[0], text: "settled capture" }] });
    visibility.mockReturnValue("visible"); fireEvent(document, new Event("visibilitychange"));
    await advance();
    expect(lookup).toHaveBeenCalledTimes(3);
    expect(screen.getByText("settled capture")).toBeTruthy();
    expect(screen.queryByText("stale cancelled result")).toBeNull();
    await advance(60_000);
    expect(lookup).toHaveBeenCalledTimes(3);
  });

  it("cancels an in-flight refresh and its polling timer on unmount", async () => {
    vi.useFakeTimers(); vi.setSystemTime(new Date(issue.broke_at!));
    const lookup = vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValueOnce(page).mockImplementation((_options, signal) => new Promise((resolve) => {
      signal?.addEventListener("abort", () => resolve(page), { once: true });
    }));
    const { unmount } = mount(scenario()); await advance(); await advance(10_000);
    const signal = lookup.mock.calls.at(-1)?.[1];
    unmount(); await advance(60_000);
    expect(signal?.aborted).toBe(true);
    expect(lookup).toHaveBeenCalledTimes(2);
  });

  it.each([401, 403, 404])("stops polling and removes stale evidence after cursor HTTP %s", async (status) => {
    vi.useFakeTimers(); vi.setSystemTime(new Date(issue.broke_at!));
    const lookup = vi.spyOn(api, "kubernetesBreakLogs").mockImplementation(async (options) => {
      if (options.cursor) throw new ApiError(status, "private provider text");
      return page;
    });
    mount(scenario()); await advance();
    fireEvent.click(screen.getByRole("button", { name: "Load more logs" })); await advance();
    expect(screen.getByRole("alert")).toBeTruthy();
    expect(screen.queryByText("timeout <script>not markup</script>")).toBeNull();
    await advance(180_000);
    expect(lookup).toHaveBeenCalledTimes(2);
  });

  it.each(["", "logs_from=bad&logs_to=bad", "logs_from=2026-10-14T12:06:00Z&logs_to=2026-10-14T11:59:00Z", "logs_from=2026-10-14T08:00:00Z&logs_to=2026-10-14T12:00:00Z"])("falls back to the default bounded window for %s", (query) => {
    expect(breakLogWindow(new URLSearchParams(query), issue.broke_at!)).toEqual({ from: "2026-10-14T11:59:00.000Z", to: "2026-10-14T12:06:00.000Z" });
  });

  it("moves the URL-backed window by keyboard and pointer without remounting handles", async () => {
    const lookup = vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValue(page);
    const { container } = mount(scenario());
    await screen.findByText("timeout <value>");
    const handle = screen.getByRole("slider", { name: "Log window start" });
    handle.focus(); fireEvent.keyDown(handle, { key: "ArrowRight" });
    await waitFor(() => expect(screen.getByLabelText("Location").textContent).toContain("logs_from=2026-10-14T11%3A59%3A30.000Z"));
    expect(screen.getByRole("slider", { name: "Log window start" })).toBe(handle);
    expect(document.activeElement).toBe(handle);
    expect(lookup.mock.calls.at(-1)?.[0].since).toBe("2026-10-14T11:59:30.000Z");
    const plot = container.querySelector<HTMLElement>(".break-log-bracket-plot")!;
    vi.spyOn(plot, "getBoundingClientRect").mockReturnValue({ left: 0, width: 600, right: 600, top: 0, bottom: 100, height: 100, x: 0, y: 0, toJSON: () => ({}) });
    const original = window.PointerEvent;
    window.PointerEvent = MouseEvent as typeof PointerEvent;
    fireEvent.pointerDown(handle, { pointerId: 1, clientX: 295 });
    fireEvent.pointerMove(handle, { pointerId: 1, clientX: 300 });
    fireEvent.pointerUp(handle, { pointerId: 1 });
    window.PointerEvent = original;
    await waitFor(() => expect(lookup.mock.calls.at(-1)?.[0].since).toBe("2026-10-14T12:00:00.000Z"));
  });

  it("renders literal lines, dividers, rates and patterns and preserves original filters while paging", async () => {
    const lookup = vi.spyOn(api, "kubernetesBreakLogs").mockImplementation(async (options) => ({ ...page, lines: options.cursor ? [{ ...page.lines[0], text: "later page line" }] : page.lines, next: options.cursor ? undefined : page.next }));
    const { container } = mount(scenario());
    await screen.findByText("timeout <script>not markup</script>");
    expect(container.querySelector("script")).toBeNull();
    expect(screen.getByRole("log").textContent).toContain("Deployment checkout unavailable");
    expect(screen.getByRole("log").textContent).toContain("api Error exit 1");
    expect(screen.getByRole("img", { name: "Log lines per minute, errors stacked in red" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /checkout-deleted/ }));
    await waitFor(() => expect(lookup.mock.calls.at(-1)?.[0].pods).toEqual(["checkout-deleted"]));
    fireEvent.click(screen.getByRole("button", { name: "Previous container" }));
    fireEvent.click(await screen.findByRole("button", { name: "Errors 2" }));
    fireEvent.change(screen.getByRole("textbox", { name: "Search in log window" }), { target: { value: "timeout + retry" } });
    await waitFor(() => expect(lookup.mock.calls.at(-1)?.[0]).toMatchObject({ mode: "previous", level: "error", q: "timeout + retry", cursor: undefined }));
    fireEvent.click(await screen.findByRole("button", { name: "Load more logs" }));
    await screen.findByText("later page line");
    expect(lookup.mock.calls.at(-1)?.[0]).toMatchObject({ pods: ["checkout-deleted"], mode: "previous", level: "error", q: "timeout + retry", cursor: "opaque-next" });
    fireEvent.click(screen.getByRole("button", { name: "Current" }));
    await waitFor(() => expect(lookup.mock.calls.at(-1)?.[0]).toMatchObject({ mode: "current", cursor: undefined }));
    expect(screen.queryByText("later page line")).toBeNull();
  });

  it.each([{ state: "complete" as const, tip: true, visible: false }, { state: "partial" as const, tip: false, visible: false }, { state: "none" as const, tip: true, visible: true }])("honors coverage $state with backend tip=$tip", async ({ state, tip, visible }) => {
    vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValue({ ...page, coverage: { ...page.coverage, state, tip } });
    mount(scenario()); await screen.findByText("timeout <value>");
    expect(Boolean(screen.queryByRole("button", { name: "Log coverage tip" }))).toBe(visible);
    if (visible) {
      fireEvent.click(screen.getByRole("button", { name: "Log coverage tip" }));
      expect(screen.getByText(/The container restarted more than once/)).toBeTruthy();
      expect(screen.getByRole("link", { name: "Link a source in Agent tab" }).getAttribute("href")).toBe("/agent/kubernetes?view=agent&cluster=east");
    }
  });

  it("selects previous Pod logs ending at the crash and opens the real Pod/workload", async () => {
    const lookup = vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValue(page);
    const select = vi.fn();
    const crash: KubernetesCrashEvent = { id: "crash", sequence: 1, cluster: "east", namespace: "shop", pod: "checkout-old", pod_uid: "old-uid", root_uid: "checkout-uid", root_kind: "Deployment", root_name: "checkout", container: "api", type: "termination", termination_slot: "previous", reason: "OOMKilled", exit_code: 137, restart_count: 2, at: "2026-10-14T12:05:00Z", time_source: "finished_at", observed_at: "2026-10-14T12:05:05Z" };
    const { container } = mount(scenario(select, [crash]));
    await screen.findByText("timeout <value>");
    fireEvent.click(container.querySelector('[data-event-id="crash"]')!);
    fireEvent.click(screen.getByRole("button", { name: "Open pod" }));
    expect(select).toHaveBeenLastCalledWith(expect.objectContaining({ name: "checkout-old", uid: "old-uid", kind: "Pod" }), undefined);
    fireEvent.click(screen.getByRole("button", { name: "Logs before crash" }));
    await waitFor(() => expect(lookup.mock.calls.at(-1)?.[0]).toMatchObject({ pods: ["old-uid"], mode: "previous", since: "2026-10-14T12:00:00.000Z", until: "2026-10-14T12:05:00.000Z" }));
    fireEvent.click(screen.getAllByRole("button", { name: "Open live logs" })[0]);
    expect(select).toHaveBeenLastCalledWith(expect.objectContaining({ name: "checkout", resource_id: "apps~v1~deployments" }), "logs");
    fireEvent.click(screen.getAllByRole("button", { name: "Ask chat" })[0]);
    expect(screen.getByLabelText("Location").textContent).toContain("/agent/chat?");
    expect(screen.getByLabelText("Location").textContent).toContain("logs_to=2026-10-14T12%3A05%3A00.000Z");
  });

  it.each([400, 403])("handles cursor HTTP %s without exposing provider text", async (status) => {
    const lookup = vi.spyOn(api, "kubernetesBreakLogs").mockImplementation(async (options) => {
      if (options.cursor) throw new ApiError(status, "private provider text");
      return page;
    });
    mount(scenario()); await screen.findByText("timeout <value>");
    fireEvent.click(screen.getByRole("button", { name: "Load more logs" }));
    await screen.findByRole("alert");
    expect(document.body.textContent).not.toContain("private provider text");
    if (status === 403) expect(screen.queryByText("timeout <script>not markup</script>")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Reload logs" }));
    await waitFor(() => expect(lookup.mock.calls.at(-1)?.[0].cursor).toBeUndefined());
  });

  it("opens another root's crash with that root, namespace and actual Pod identity", async () => {
    vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValue(page);
    const crash: KubernetesCrashEvent = { id: "other-crash", sequence: 1, cluster: "east", namespace: "payments", pod: "ledger-old", pod_uid: "ledger-pod", root_uid: "ledger-root", root_kind: "Deployment", root_name: "ledger", container: "api", type: "termination", termination_slot: "previous", reason: "Error", exit_code: 1, restart_count: 1, at: "2026-10-14T12:05:00Z", time_source: "finished_at", observed_at: "2026-10-14T12:05:05Z" };
    const { container } = mount(scenario(vi.fn(), [crash]));
    await screen.findByText("timeout <value>");
    fireEvent.click(container.querySelector('[data-event-id="other-crash"]')!);
    fireEvent.click(screen.getByRole("button", { name: "Logs before crash" }));
    const location = screen.getByLabelText("Location").textContent!;
    expect(location).toContain("root_uid=ledger-root");
    expect(location).toContain("namespace=payments");
    expect(location).toContain("logs_pod=ledger-pod");
    expect(location).toContain("logs_mode=previous");
  });
});

describe("Break timeline", () => {
  const observation: KubernetesCrashEvent = { id: "journal-event", sequence: 1, cluster: "east", namespace: "shop", pod: "checkout-old", pod_uid: "old-pod-uid", root_uid: "checkout-uid", root_kind: "Deployment", root_name: "checkout", container: "api", type: "termination", termination_slot: "previous", reason: "Error", exit_code: 137, restart_count: 41, at: "2026-10-14T12:05:00Z", time_source: "finished_at", observed_at: "2026-10-14T12:06:00Z" };
  const journalPage: KubernetesCrashPage = { items: [observation], since: "2026-10-14T11:30:00Z", until: "2026-10-14T12:30:00Z", retention_from: "2026-10-13T12:30:00Z", retention_basis: "observed_at", started_at: "2026-10-14T11:45:00Z", gaps: [{ from: "2026-10-14T11:30:00Z", to: "2026-10-14T11:45:00Z" }], capture_gaps: [{ from: "2026-10-14T12:00:00Z", to: "2026-10-14T12:01:00Z" }], coverage: {}, limitations: ["journal_not_started_for_full_window"], capture: { mode: "continuous", behind: true }, sync: { state: "ready", age_s: 0, partial: true } };

  function JournalScenario() {
    const [namespace, setNamespace] = useState("shop");
    const journal = useKubernetesCrashes({ namespace, root_uid: "checkout-uid", since: journalPage.since, until: journalPage.until });
    return <><button onClick={() => setNamespace("other")}>Other namespace</button><ResourceTimelineChart issues={[{ ...issue, evidence: { ...issue.evidence!, partial: true } }]} changes={[]} crashes={journal.isError ? [] : journal.data?.items ?? []} since={journalPage.since} until={journalPage.until} onSelectChange={vi.fn()} /><CrashJournal journal={journal} /></>;
  }

  const baselineLimitations = [
    "polling_observations_only",
    "restart_counts_are_lifetime_not_occurrences",
    "summary_caps_do_not_limit_journal",
    "ancestry_is_capture_time_uid_scoped",
    "unknown_finished_at_uses_observation_time",
    "duplicate_status_states_are_coalesced",
    "unknown_termination_identity_is_ambiguous",
  ];
  const coveredJournalPage: KubernetesCrashPage = {
    ...journalPage,
    started_at: journalPage.since,
    gaps: [],
    capture_gaps: [],
    coverage: { Pod: { from: journalPage.since, to: journalPage.until } },
    limitations: baselineLimitations,
    capture: { mode: "continuous", behind: false },
    sync: { state: "ready", age_s: 0, partial: false },
  };
  const emptyLogs: KubernetesBreakLogPage = {
    window: { from: journalPage.since, to: journalPage.until },
    pods: [], lines: [], markers: [], counts: { all: 0, errors: 0, warnings: 0 },
    rate: [], patterns: [], sources: [],
    coverage: { state: "complete", gaps: [], reason: "", tip: false },
  };

  it("keeps complete crash coverage neutral and capture details limited to the window and start", async () => {
    vi.spyOn(api, "kubernetesCrashes").mockResolvedValue(coveredJournalPage);
    mount(<JournalScenario />);
    const journal = screen.getByRole("region", { name: "Captured crash evidence" });
    await within(journal).findByText("1 crash");
    const summary = journal.querySelector("header > span")!;
    expect(summary.className).not.toContain("text-sev-warning");
    expect(summary.textContent).not.toContain("Partial history");
    const disclosure = within(journal).getByText("Capture details").closest("details")!;
    expect(disclosure.open).toBe(false);
    expect(within(journal).queryByRole("button", { name: "Resource coverage" })).toBeNull();
    const lines = disclosure.querySelectorAll("p");
    expect(lines).toHaveLength(2);
    expect(lines[0].textContent).toMatch(/^Window:/);
    expect(lines[1].textContent).toMatch(/^Capture started:/);
    for (const caveat of baselineLimitations) expect(disclosure.textContent).not.toContain(caveat.replaceAll("_", " "));
  });

  it.each(["journal_not_started_for_full_window", "outside_retention", "journal_commit_failed"])("discloses window-specific crash limitation %s through one neutral coverage marker", async (limitation) => {
    vi.spyOn(api, "kubernetesCrashes").mockResolvedValue({ ...coveredJournalPage, limitations: [...baselineLimitations, limitation] });
    mount(<JournalScenario />);
    const journal = screen.getByRole("region", { name: "Captured crash evidence" });
    await within(journal).findByText("1 crash");
    const marker = within(journal).getByRole("button", { name: "Resource coverage" });
    expect(within(journal).getAllByRole("button", { name: "Resource coverage" })).toHaveLength(1);
    expect(marker.className).toContain("text-ink-400");
    expect(marker.className).not.toContain("text-sev-warning");
    expect(screen.queryByText("Crash capture does not cover the whole range")).toBeNull();
    fireEvent.click(marker);
    expect(screen.getByRole("region", { name: "Resource coverage details" }).textContent).toContain("Crash capture does not cover the whole range");
    expect(within(journal).getByText("Capture details").closest("details")?.open).toBe(false);
  });

  it.each([
    { ...observation, time_source: "observed_at" as const },
    { ...observation, ancestry_partial: true },
  ])("keeps actual ambiguous crash evidence visible despite otherwise complete coverage ($time_source, ancestry=$ancestry_partial)", async (event) => {
    vi.spyOn(api, "kubernetesCrashes").mockResolvedValue({ ...coveredJournalPage, items: [event] });
    mount(<JournalScenario />);
    const journal = screen.getByRole("region", { name: "Captured crash evidence" });
    await within(journal).findByText("1 crash");
    fireEvent.click(within(journal).getByRole("button", { name: "Resource coverage" }));
    expect(screen.getByText("Crash capture does not cover the whole range")).toBeTruthy();
  });

  it("keeps incomplete coverage in a closed portal and excludes invalid capture timestamps", async () => {
    vi.spyOn(api, "kubernetesCrashes").mockResolvedValue({ ...journalPage, items: [], started_at: "0001-01-01T00:00:00Z", retention_from: "0001-01-01T00:00:00Z" });
    mount(<JournalScenario />);
    const journal = screen.getByRole("region", { name: "Captured crash evidence" });
    await within(journal).findByText("No captured observations on this page.");
    const header = journal.querySelector("header")!;
    expect(header.textContent).toContain("0 crashes");
    const marker = within(header).getByRole("button", { name: "Resource coverage" });
    expect(marker.getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByRole("region", { name: "Resource coverage details" })).toBeNull();
    const disclosure = within(journal).getByText("Capture details").closest("details")!;
    expect(disclosure.open).toBe(false);
    expect(disclosure.querySelector("details")).toBeNull();
    expect(disclosure.querySelectorAll("p")).toHaveLength(2);
    expect(journal.textContent).not.toContain("0001-");
    expect(within(journal).getAllByRole("status")).toHaveLength(1);
    fireEvent.click(within(journal).getByText("Capture details"));
    expect(disclosure.open).toBe(true);
    fireEvent.click(marker);
    expect(screen.getByText("Crash capture does not cover the whole range")).toBeTruthy();
  });

  it("pages all journal observations without accumulating rendered rows and resets filters", async () => {
    const lookup = vi.spyOn(api, "kubernetesCrashes").mockImplementation(async (options) => ({ ...journalPage, crash_count: 73, items: options?.cursor ? [{ ...observation, id: options.cursor, pod: options.cursor === "final-page" ? "final-pod" : "last-pod", init: true, signal: 9 }] : Array.from({ length: 50 }, (_, index) => ({ ...observation, id: `event-${index}`, pod: `old-pod-${index}` })), next: options?.cursor === "final-page" ? undefined : options?.cursor ? "final-page" : "next-page" }));
    const { container } = mount(<JournalScenario />);
    const journal = screen.getByRole("region", { name: "Captured crash evidence" });
    await waitFor(() => expect(within(within(journal).getByRole("list", { name: "Crash observations" })).getAllByRole("listitem")).toHaveLength(50));
    expect(lookup).toHaveBeenCalledTimes(1);
    expect(within(journal).getByText("73 crashes")).toBeTruthy();
    expect(lookup.mock.calls[0][0]).toMatchObject({ namespace: "shop", root_uid: "checkout-uid", limit: 50, since: journalPage.since, until: journalPage.until });
    expect(lookup.mock.calls[0][1]).toBeInstanceOf(AbortSignal);
    expect(container.querySelectorAll('[data-marker="crash"]')).toHaveLength(50);
    expect(screen.queryByText("Partial crash evidence")).toBeNull();
    const opener = screen.getByRole("button", { name: /^50 events:/ });
    fireEvent.click(opener);
    expect(screen.getByRole("region", { name: "Timeline event details" }).textContent).toContain("old-pod-49");
    fireEvent.keyDown(opener, { key: "Escape" });
    fireEvent.click(within(journal).getByRole("button", { name: "Next crash page" }));
    await waitFor(() => expect(journal.textContent).toContain("last-pod"));
    expect(within(journal).getByText("73 crashes")).toBeTruthy();
    expect(within(journal).getByRole("button", { name: "Next crash page" }).hasAttribute("disabled")).toBe(false);
    expect(container.querySelectorAll('[data-marker="crash"]')).toHaveLength(1);
    const summary = within(journal).getByRole("list", { name: "Crash observations" }).querySelector("summary")!;
    fireEvent.click(summary);
    expect(journal.textContent).toContain("signal 9");
    expect(journal.textContent).toContain("41 lifetime restarts");
    expect(journal.textContent).toContain("Root UID: checkout-uid; Pod UID: old-pod-uid");
    fireEvent.click(within(journal).getByRole("button", { name: "Previous crash page" }));
    await waitFor(() => expect(journal.textContent).toContain("old-pod-49"));
    expect(lookup).toHaveBeenCalledTimes(2);
    fireEvent.click(within(journal).getByRole("button", { name: "Next crash page" }));
    await waitFor(() => expect(journal.textContent).toContain("last-pod"));
    fireEvent.click(within(journal).getByRole("button", { name: "Next crash page" }));
    await waitFor(() => expect(journal.textContent).toContain("final-pod"));
    expect(within(journal).getByRole("button", { name: "Next crash page" }).hasAttribute("disabled")).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "Other namespace" }));
    await waitFor(() => expect(lookup).toHaveBeenCalledTimes(4));
    expect(lookup.mock.calls[3][0]).toMatchObject({ namespace: "other", cursor: undefined });
  });

  it("counts server termination totals rather than status rows and preserves safe time and signal provenance", async () => {
    vi.spyOn(api, "kubernetesCrashes").mockResolvedValue({ ...journalPage, crash_count: 73, items: [observation, { ...observation, id: "status", type: "status", waiting_reason: "CrashLoopBackOff", time_source: "observed_at", ancestry_partial: true }] });
    const { container } = mount(<JournalScenario />);
    await screen.findByText("CrashLoopBackOff", { exact: true });
    const journal = screen.getByRole("region", { name: "Captured crash evidence" });
    expect(container.querySelectorAll('[data-marker="crash"]')).toHaveLength(1);
    expect(journal.textContent).toContain("not proof of SIGKILL");
    expect(journal.textContent).not.toContain(", signal 9");
    expect(journal.textContent).toContain("observation time; finish time unknown");
    expect(journal.textContent).toContain("Capture-time ancestry is incomplete");
    expect(within(journal).getByText("73 crashes")).toBeTruthy();
    expect(within(journal).queryByText("2 crashes")).toBeNull();
    fireEvent.click(within(journal).getByRole("button", { name: "Resource coverage" }));
    expect(screen.getByText("Crash capture does not cover the whole range")).toBeTruthy();
  });

  it.each([0, 1])("counts only %s termination rows when the server total is absent", async (terminations) => {
    const status: KubernetesCrashEvent = { ...observation, id: "status", type: "status", waiting_reason: "CrashLoopBackOff", time_source: "observed_at" };
    vi.spyOn(api, "kubernetesCrashes").mockResolvedValue({ ...coveredJournalPage, items: [...(terminations ? [observation] : []), status] });
    const { container } = mount(<JournalScenario />);
    const journal = screen.getByRole("region", { name: "Captured crash evidence" });
    await within(journal).findByText(`${terminations} ${terminations === 1 ? "crash" : "crashes"}`);
    expect(within(within(journal).getByRole("list", { name: "Crash observations" })).getAllByRole("listitem")).toHaveLength(terminations + 1);
    expect(container.querySelectorAll('[data-marker="crash"]')).toHaveLength(terminations);
  });

  it("keeps newly observed terminations with finishes 48 hours earlier visible without implying a chart marker", async () => {
    const oldTermination = { ...observation, id: "old-finish", pod: "old-finish-pod", at: "2026-10-12T12:06:00Z" };
    vi.spyOn(api, "kubernetesCrashes").mockResolvedValue({ ...journalPage, items: [oldTermination, observation] });
    const { container } = mount(<JournalScenario />);
    const journal = screen.getByRole("region", { name: "Captured crash evidence" });
    await waitFor(() => expect(within(within(journal).getByRole("list", { name: "Crash observations" })).getAllByRole("listitem")).toHaveLength(2));
    expect(within(journal).getByText(/^Window:/)).toBeTruthy();
    const summary = within(journal).getByText("Reported finish outside observed window").closest("summary")!;
    expect(summary.textContent).toContain("shop/old-finish-pod");
    expect(summary.textContent).toContain("Observed:");
    expect(summary.querySelector("time")?.dateTime).toBe(oldTermination.observed_at);
    expect(container.querySelectorAll('[data-marker="crash"]')).toHaveLength(1);
    expect(container.querySelector('[data-event-id="old-finish"]')).toBeNull();
    expect(container.querySelector('[data-event-id="journal-event"]')?.getAttribute("data-at")).toBe(observation.at);
    fireEvent.click(summary);
    const details = summary.closest("details")!;
    expect(within(details).getByText(/^Event time:/).querySelector("time")?.dateTime).toBe(oldTermination.at);
    expect(within(details).getByText(/^Event time:/).textContent).toContain("reported finish time");
    expect(within(details).getByText(/^Observed:/, { selector: "p" }).querySelector("time")?.dateTime).toBe(oldTermination.observed_at);
  });

  it("offers retry without aggregate fallback or stale journal rows on failure", async () => {
    const lookup = vi.spyOn(api, "kubernetesCrashes").mockRejectedValueOnce(new ApiError(403, "private upstream body")).mockResolvedValue(journalPage);
    const { container } = mount(<JournalScenario />);
    await screen.findByRole("alert");
    expect(container.querySelectorAll('[data-marker="crash"]')).toHaveLength(0);
    expect(screen.queryByText(/private upstream body/)).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Retry crash journal" }));
    await waitFor(() => expect(screen.getByRole("region", { name: "Captured crash evidence" }).textContent).toContain("shop/checkout-old"));
    expect(lookup).toHaveBeenCalledTimes(2);
  });

  it("shows retained journal evidence without current issue or change data for a selected root", async () => {
    window.history.replaceState(null, "", `/agent/kubernetes?view=timeline&center=${issue.broke_at}&root_uid=checkout-uid&namespace=shop`);
    vi.spyOn(api, "kubernetesCrashes").mockResolvedValue(journalPage);
    vi.spyOn(api, "kubernetesIssues").mockImplementation(rootIssueResponses({ items: [], totals: {}, truncated: false, sync: journalPage.sync }));
    vi.spyOn(api, "kubernetesChanges").mockRejectedValue(new Error("Unavailable"));
    const graph = vi.spyOn(api, "kubernetesGraph");
    const related = vi.spyOn(api, "kubernetesRelated").mockResolvedValue({ nodes: [], edges: [], omitted: {}, sync: journalPage.sync });
    const { container } = mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    await waitFor(() => expect(screen.getByRole("region", { name: "Captured crash evidence" }).textContent).toContain("checkout-old"));
    expect(container.querySelectorAll('[data-marker="crash"]')).toHaveLength(1);
    expect(graph).not.toHaveBeenCalled();
    expect(related).not.toHaveBeenCalled();
  });

  it("clamps centered journal requests to now without changing the change-chart window", async () => {
    const now = Date.parse("2026-10-16T12:00:00Z");
    vi.spyOn(Date, "now").mockReturnValue(now);
    const center = "2026-10-16T11:55:00Z";
    window.history.replaceState(null, "", `/agent/kubernetes?view=timeline&root_uid=checkout-uid&namespace=shop&center=${center}`);
    const lookup = vi.spyOn(api, "kubernetesCrashes").mockResolvedValue({ ...journalPage, items: [] });
    vi.spyOn(api, "kubernetesIssues").mockImplementation(rootIssueResponses({ items: [{ ...issue, broke_at: center }], totals: {}, truncated: false, sync: journalPage.sync }));
    vi.spyOn(api, "kubernetesRelated").mockResolvedValue({ nodes: [], edges: [], omitted: {}, sync: journalPage.sync });
    const changes = vi.spyOn(api, "kubernetesChanges").mockResolvedValue({ items: [], sync: journalPage.sync });
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    await waitFor(() => expect(changes).toHaveBeenCalled());
    expect(lookup.mock.calls[0][0]?.until).toBe(new Date(now).toISOString());
    expect(changes.mock.calls[0][0]?.until).toBe("2026-10-16T12:25:00.000Z");
    expect(screen.getByRole("region", { name: "Resource timeline chart" }).getAttribute("data-until")).toBe("2026-10-16T12:25:00.000Z");
  });

  it.each(["resolved", "not_found"] as const)("preserves a clicked nanosecond break center and only enables logs after server resolution: %s", async (status) => {
    const precise = { ...issue, broke_at: "2026-10-14T12:04:00.123456789Z" };
    const lookup = vi.spyOn(api, "kubernetesIssues").mockImplementation(async options => {
      if (options?.context !== "persisted") return { items: [precise], totals: {}, truncated: false, sync: journalPage.sync };
      const resolved = options.center === precise.broke_at && status === "resolved";
      return { ...persistedRootPage(issue), selection_status: resolved ? "resolved" : "not_found", selected_occurrence: resolved ? precise : undefined };
    });
    vi.spyOn(api, "kubernetesCrashes").mockResolvedValue(coveredJournalPage);
    vi.spyOn(api, "kubernetesChanges").mockResolvedValue({ items: [], sync: journalPage.sync });
    const logs = vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValue({ window: { from: "2026-10-14T11:59:00Z", to: "2026-10-14T12:09:00Z" }, pods: [], lines: [], markers: [], counts: { all: 0, errors: 0, warnings: 0 }, rate: [], patterns: [], sources: [], coverage: { state: "complete", gaps: [] } });
    mount(<ClickedBreak value={precise} />);
    fireEvent.click(screen.getByRole("button", { name: "Timeline for checkout" }));
    await waitFor(() => expect(lookup.mock.calls.find(([options]) => options?.context === "persisted")?.[0]).toMatchObject({ center: precise.broke_at, occurrence_id: undefined }));
    const location = screen.getByLabelText("Location").textContent!;
    expect(new URLSearchParams(location.slice(location.indexOf("?"))).get("center")).toBe(precise.broke_at);
    if (status === "resolved") {
      await waitFor(() => expect(logs).toHaveBeenCalled());
      expect(logs.mock.calls[0][0]).toMatchObject({ since: "2026-10-14T11:59:00.123Z", until: "2026-10-14T12:06:00.123Z" });
      expect(screen.queryByText("Log time range is unavailable.")).toBeNull();
    } else {
      await screen.findByText(/Log time range is unavailable/);
      expect(logs).not.toHaveBeenCalled();
    }
  });

  it.each([["pod.config_error", "Container config error"], ["pod.evicted", "Evicted"], ["pod.unschedulable", "Unschedulable"]])("names rule %s and singular affected Pods", (rule, label) => {
    mount(<BreakRow issue={{ ...issue, rule, count: 1, of: 1, evidence: undefined }} resource={resource} onSelectResource={vi.fn()} />);
    expect(screen.getByLabelText(label)).toBeTruthy();
    expect(screen.getByText("1 of 1 pod affected")).toBeTruthy();
    expect(screen.queryByLabelText("Crash evidence")).toBeNull();
  });

  it.each([false, true])("reveals mobile evidence with keyboard and expands the existing row, collapsed=%s", async (collapsed) => {
    const user = userEvent.setup();
    const enriched: KubernetesIssue = { ...issue, evidence: { ...issue.evidence!, restarts: 44, partial: true,
      terminations: [{ ...issue.evidence!.terminations![0], restarts: 41 }],
      waiting: [{ reason: "CreateContainerConfigError", count: 1, init: true }],
      pod_reasons: [{ reason: "Evicted", count: 2 }], conditions: [{ type: "Failed", reason: "BackoffLimitExceeded" }],
      nodes: [{ reason: "MemoryPressure", count: 3 }], warning_events: 12,
      warning_reasons: [{ reason: "FailedScheduling", count: 8 }, { reason: "Other", count: 4 }],
    } };
    const lookup = vi.spyOn(api, "kubernetesIssues").mockImplementation(rootIssueResponses({ items: [enriched], totals: {}, truncated: false, sync: { state: "ready", age_s: 0, partial: false } }));
    const { container } = mount(<BreakRow issue={enriched} resource={resource} onSelectResource={vi.fn()} collapsed={collapsed} />);
    const toggle = screen.getByRole("button", { name: "+2 more evidence for checkout" });
    const extra = document.getElementById(toggle.getAttribute("aria-controls")!)!;
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    expect(extra.className).toBe("hidden sm:contents");
    expect(screen.getByText("OOMKilled on 6 pods")).toBeTruthy();
    expect(screen.getByText("Init CreateContainerConfigError on 1 pod")).toBeTruthy();
    expect(screen.getByText("Evicted on 2 pods")).toBeTruthy();
    expect(screen.queryByText("Partial crash evidence")).toBeNull();
    if (collapsed) expect(screen.queryByRole("button", { name: "Resource coverage" })).toBeNull();
    else {
      const marker = screen.getByRole("button", { name: "Resource coverage" });
      fireEvent.click(marker);
      expect(screen.getByText("Some crash evidence could not be captured")).toBeTruthy();
      fireEvent.keyDown(marker, { key: "Escape" });
    }
    expect(screen.getByText("44 lifetime restarts")).toBeTruthy();
    expect(screen.getByText("12 warning events: FailedScheduling 8, Other 4")).toBeTruthy();
    expect(lookup).not.toHaveBeenCalled();
    toggle.focus();
    await user.keyboard("{Enter}");
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    expect(extra.className).toBe("contents");
    expect(extra.textContent).toContain("BackoffLimitExceeded");
    expect(extra.textContent).toContain("Node MemoryPressure on 3 pods");
    expect(document.activeElement).toBe(toggle);
    if (collapsed) {
      expect(screen.getByRole("button", { name: "Collapse checkout" }).getAttribute("aria-expanded")).toBe("true");
      await waitFor(() => expect(lookup).toHaveBeenCalledOnce());
    }
    await user.keyboard("{Enter}");
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    expect(extra.className).toBe("hidden sm:contents");
    expect(container.textContent).not.toContain("·");
  });

  it("resets evidence disclosure when scope changes and does not restore a previous scope", () => {
    const enriched = { ...issue, evidence: { ...issue.evidence!, waiting: ["CreateContainerConfigError", "CrashLoopBackOff", "ImagePullBackOff"].map((reason) => ({ reason, count: 1 })) } };
    function Scenario() {
      const [scope, setScope] = useState("east");
      return <><button onClick={() => setScope(scope === "east" ? "west" : "east")}>Change evidence scope</button><BreakRow issue={enriched} resource={resource} onSelectResource={vi.fn()} warningScope={scope} /></>;
    }
    mount(<Scenario />);
    fireEvent.click(screen.getByRole("button", { name: "+1 more evidence for checkout" }));
    expect(screen.getByRole("button", { name: "Show less evidence for checkout" }).getAttribute("aria-expanded")).toBe("true");
    fireEvent.click(screen.getByRole("button", { name: "Change evidence scope" }));
    expect(screen.getByRole("button", { name: "+1 more evidence for checkout" }).getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(screen.getByRole("button", { name: "Change evidence scope" }));
    expect(screen.getByRole("button", { name: "+1 more evidence for checkout" }).getAttribute("aria-expanded")).toBe("false");
  });

  it("keeps marker buckets and roots distinct with actual timestamps and keyboard details", async () => {
    const user = userEvent.setup();
    const termination = { reason: "Error", exit_code: 137, count: 1, last: "2026-10-14T12:05:00Z" };
    const enriched = { ...issue, evidence: { ...issue.evidence!, partial: true, terminations: [termination, { ...termination, init: true }, { ...termination, signal: "SIGKILL", restarts: 1 }, { ...termination, exit_hint: "SIGKILL" }] } };
    const second = { ...issue, root: { ...issue.root, name: "other", uid: "other" }, evidence: { ...issue.evidence!, terminations: [termination] } };
    const { container } = mount(<ResourceTimelineChart issues={[enriched, second]} changes={[]} since="2026-10-14T11:30:00Z" until="2026-10-14T12:30:00Z" onSelectChange={vi.fn()} />);
    const markers = [...container.querySelectorAll<HTMLElement>('[data-marker="crash"]')];
    expect(markers).toHaveLength(5);
    expect(new Set(markers.map((marker) => marker.dataset.eventId)).size).toBe(5);
    expect(markers.every((marker) => marker.dataset.at === termination.last)).toBe(true);
    expect(screen.getByText("Partial crash evidence")).toBeTruthy();
    const opener = screen.getByRole("button", { name: /^4 events:/ });
    opener.focus();
    await user.keyboard("{Enter}");
    const details = screen.getByRole("region", { name: "Timeline event details" });
    expect(details.textContent).toContain("signal SIGKILL on 1 pod, 1 lifetime restart (last occurrence)");
    expect(details.textContent).toContain("exit hint: SIGKILL");
    expect(details.textContent).toContain("Init Error exit 137 on 1 pod");
    expect(details.textContent).not.toContain("·");
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("region", { name: "Timeline event details" })).toBeNull();
    expect(document.activeElement).toBe(opener);
  });

  it("omits unknown lifetime restarts while keeping singular warning events", () => {
    mount(<BreakRow issue={{ ...issue, evidence: { ...issue.evidence!, warning_events: 1 } }} resource={resource} onSelectResource={vi.fn()} />);
    expect(screen.getByText("1 warning event")).toBeTruthy();
    expect(screen.queryByText(/lifetime restart/)).toBeNull();
    expect(screen.queryByText("Partial crash evidence")).toBeNull();
  });

  it("collapses deduplicated warnings without hiding primary break evidence and resets scope", () => {
    const warnings = [{ id: "history:gap", content: "History incomplete." }, { id: "history:gap", content: "History incomplete." }, { id: "untracked:Secret", content: "Secrets are not tracked." }];
    const { rerender } = render(<><strong>CrashLoopBackOff 7 of 7</strong><KubernetesWarnings warnings={warnings} scope="east:checkout" /></>);
    const toggle = screen.getByRole("button", { name: "Resource coverage" });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    expect(toggle.hasAttribute("aria-controls")).toBe(false);
    expect(screen.queryByText("History incomplete.")).toBeNull();
    expect(screen.queryByRole("region", { name: "Resource coverage details" })).toBeNull();
    expect(screen.getByText("CrashLoopBackOff 7 of 7")).toBeTruthy();
    fireEvent.click(toggle);
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    const panel = screen.getByRole("region", { name: "Resource coverage details" });
    expect(panel.parentElement).toBe(document.body);
    expect(panel.id).toBe(toggle.getAttribute("aria-controls"));
    expect(screen.getByRole("list").textContent).toContain("History incomplete.");
    expect(screen.getAllByText("History incomplete.")).toHaveLength(1);
    expect(within(screen.getByRole("region", { name: "Resource coverage details" })).getAllByRole("listitem")).toHaveLength(2);
    rerender(<KubernetesWarnings warnings={warnings} scope="west:checkout" />);
    expect(screen.getByRole("button").getAttribute("aria-expanded")).toBe("false");
    rerender(<KubernetesWarnings warnings={warnings} scope="east:checkout" />);
    expect(screen.getByRole("button").getAttribute("aria-expanded")).toBe("false");
    rerender(<KubernetesWarnings warnings={warnings} scope="west:checkout" />);
    fireEvent.click(screen.getByRole("button"));
    screen.getByRole("button").focus();
    const focused = document.activeElement;
    rerender(<KubernetesWarnings warnings={[...warnings, { id: "stale", content: "Snapshot stale." }]} scope="west:checkout" />);
    expect(screen.getByRole("button", { name: "Resource coverage" }).getAttribute("aria-expanded")).toBe("true");
    expect(within(screen.getByRole("region", { name: "Resource coverage details" })).getAllByRole("listitem")).toHaveLength(3);
    expect(document.activeElement).toBe(focused);
  });

  it("opens root-filtered server pages from more changes without scanning a generic graph", async () => {
    const linked = Array.from({ length: 24 }, (_, index) => ({ ...change, id: `linked-${index}`, kind: "ConfigMap", name: `settings-${index}`, uid: `config-${index}`, type: "config_changed" as const }));
    const detail = { ...issue, changes: linked.slice(0, 3).map((item) => ({ change: item, relation: "used_by", lead_seconds: 240 })), more_changes: 21 };
    const sync = { state: "ready", age_s: 0, partial: false };
    vi.spyOn(api, "kubernetesIssues").mockImplementation(rootIssueResponses({ items: [detail], totals: {}, truncated: false, sync }));
    const graph = vi.spyOn(api, "kubernetesGraph");
    const related = vi.spyOn(api, "kubernetesRelated").mockResolvedValue({ nodes: [], edges: [], omitted: {}, sync });
    vi.spyOn(api, "kubernetesCrashes").mockResolvedValue(coveredJournalPage);
    vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValue(emptyLogs);
    const lookup = vi.spyOn(api, "kubernetesChanges").mockImplementation(async (options) => {
      const cursor = options?.cursor;
      return { items: cursor ? linked.slice(20) : linked.slice(0, 20), next: cursor ? undefined : "opaque+/=next", sync };
    });
    function Scenario() {
      const location = useLocation();
      return new URLSearchParams(location.search).get("view") === "timeline" ? <KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} /> : <BreakRow issue={detail} resource={resource} onSelectResource={vi.fn()} />;
    }
    mount(<Scenario />);
    fireEvent.click(screen.getAllByRole("button", { name: /more changes/ })[0]);
    expect(await screen.findByRole("button", { name: /ConfigMap settings-19:.* at / })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /unrelated-0:/ })).toBeNull();
    expect(graph).not.toHaveBeenCalled();
    expect(related).not.toHaveBeenCalled();
    expect(lookup).toHaveBeenCalledTimes(2);
    expect(lookup.mock.calls[0][0]).toMatchObject({ namespace: "shop", root_uid: "checkout-uid", limit: 100, cursor: undefined });
    expect(await screen.findByRole("button", { name: /ConfigMap settings-23:.* at / })).toBeTruthy();
    expect(lookup.mock.calls[1][0]).toMatchObject({ root_uid: "checkout-uid", cursor: "opaque+/=next" });
    expect(lookup).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole("button", { name: "Next page" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Previous page" })).toBeNull();
    expect(await screen.findByRole("button", { name: /ConfigMap settings-0:.* at / })).toBeTruthy();
    const query = new URLSearchParams(screen.getByLabelText("Location").textContent!.split("?")[1]);
    expect(query.get("root_uid")).toBe(issue.root.uid);
    expect(query.get("namespace")).toBe("shop");
    expect(query.get("center")).toBe(issue.broke_at);
  });

  it("bounds internal root continuation to five pages with loaded markers and explicit incomplete coverage", async () => {
    window.history.replaceState(null, "", `/agent/kubernetes?view=timeline&root_uid=checkout-uid&namespace=shop&center=${issue.broke_at}`);
    const sync = { state: "ready", age_s: 0, partial: false };
    vi.spyOn(api, "kubernetesIssues").mockImplementation(rootIssueResponses({ items: [issue], totals: {}, truncated: false, sync }));
    vi.spyOn(api, "kubernetesRelated").mockResolvedValue({ nodes: [], edges: [], omitted: {}, sync });
    vi.spyOn(api, "kubernetesCrashes").mockResolvedValue(coveredJournalPage);
    const cursors = [undefined, "a+/=", "b+/=", "c+/=", "d+/=", "e+/=", "f+/="];
    const lookup = vi.spyOn(api, "kubernetesChanges").mockImplementation(async (options) => {
      const index = cursors.indexOf(options?.cursor);
      return { items: [{ ...change, id: `record-${index}`, uid: `server-linked-${index}`, name: `linked-${index}` }], next: cursors[index + 1], sync };
    });
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    await screen.findByRole("button", { name: /Deployment linked-0:.* at / });
    expect(lookup).toHaveBeenCalledTimes(5);
    for (let index = 1; index < 5; index++) expect(screen.getByRole("button", { name: new RegExp(`Deployment linked-${index}:.* at `) })).toBeTruthy();
    expect(lookup.mock.calls.map(([options]) => options?.cursor)).toEqual(cursors.slice(0, 5));
    expect(lookup.mock.calls.every(([options]) => options?.root_uid === "checkout-uid" && options.namespace === "shop" && options.limit === 100)).toBe(true);
    expect(screen.queryByRole("button", { name: "Next page" })).toBeNull();
    expect(screen.getByText(/Change evidence is incomplete for this range/)).toBeTruthy();
  });

  it("delegates root and URL kind filtering to the server and hides namespace and kind selectors in break detail", async () => {
    window.history.replaceState(null, "", `/agent/kubernetes?view=timeline&root_uid=checkout-uid&namespace=shop&kind=Deployment&center=${issue.broke_at}`);
    const sync = { state: "ready", age_s: 0, partial: false };
    vi.spyOn(api, "kubernetesIssues").mockImplementation(rootIssueResponses({ items: [issue], totals: {}, truncated: false, sync }));
    vi.spyOn(api, "kubernetesRelated").mockResolvedValue({ nodes: [], edges: [], omitted: {}, sync });
    vi.spyOn(api, "kubernetesCrashes").mockResolvedValue(coveredJournalPage);
    const lookup = vi.spyOn(api, "kubernetesChanges").mockImplementation(async (options) => ({ items: [{ ...change, id: options?.cursor ? "older" : "newer", name: options?.cursor ? "older-checkout" : "checkout" }], next: options?.cursor ? undefined : "older", sync }));
    const { client } = mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    expect(await screen.findByRole("button", { name: /Deployment checkout:.* at / })).toBeTruthy();
    expect(lookup.mock.calls.at(-1)?.[0]).toMatchObject({ root_uid: "checkout-uid", namespace: "shop", kind: "Deployment", limit: 100 });
    await screen.findByRole("button", { name: /Deployment older-checkout:.* at / });
    expect(client.getQueryCache().findAll().filter((query) => query.queryKey.includes("kubernetes-changes")).every((query) => query.queryKey[0] === "kubernetes" && query.queryKey[1] === "east")).toBe(true);
    expect(screen.queryByRole("combobox", { name: "Timeline kind" })).toBeNull();
    expect(screen.queryByRole("combobox", { name: "Timeline namespace" })).toBeNull();
    expect(screen.getByRole("link", { name: "Clear root filter" })).toBeTruthy();
    expect(screen.getByText(/^centred /)).toBeTruthy();
    expect(screen.getByRole("group", { name: "Timeline window" })).toBeTruthy();
  });

  it("keeps namespace and kind controls functional in the default virtualized list", async () => {
    window.history.replaceState(null, "", "/agent/kubernetes?view=timeline&namespace=shop");
    const issues = vi.spyOn(api, "kubernetesIssues");
    const crashes = vi.spyOn(api, "kubernetesCrashes");
    const changes = vi.spyOn(api, "kubernetesChanges");
    const lookup = vi.spyOn(api, "kubernetesTimeline").mockResolvedValue(timelinePage([changeEntry()]));
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    await screen.findByRole("button", { name: "Open change details" });
    expect(screen.getByRole("list", { name: "Breaks and changes" })).toBeTruthy();
    expect(screen.queryByRole("region", { name: "Resource timeline chart" })).toBeNull();
    expect(screen.queryByRole("region", { name: "Captured crash evidence" })).toBeNull();
    expect(screen.queryByRole("log")).toBeNull();
    fireEvent.change(screen.getByRole("combobox", { name: "Timeline kind" }), { target: { value: "Deployment" } });
    await waitFor(() => expect(lookup).toHaveBeenLastCalledWith(expect.objectContaining({ namespace: "shop", kind: "Deployment" }), expect.any(AbortSignal)));
    expect((screen.getByRole("combobox", { name: "Timeline kind" }) as HTMLSelectElement).value).toBe("Deployment");
    fireEvent.click(screen.getByRole("combobox", { name: "Timeline namespace" }));
    fireEvent.click(await screen.findByRole("option", { name: "other" }));
    await waitFor(() => expect(lookup).toHaveBeenLastCalledWith(expect.objectContaining({ namespace: "other", kind: "Deployment", cursor: undefined }), expect.any(AbortSignal)));
    expect(vi.mocked(api.kubernetesNamespaces).mock.calls.map(([namespace]) => namespace)).toEqual(["shop", "other"]);
    expect(issues).not.toHaveBeenCalled();
    expect(crashes).not.toHaveBeenCalled();
    expect(changes).not.toHaveBeenCalled();
  });

  it("virtualizes a full server page and exposes later rows by scrolling without mounting a chart or logs", async () => {
    vi.spyOn(Date, "now").mockReturnValue(Date.parse("2026-10-14T12:06:00Z"));
    const changes = Array.from({ length: 100 }, (_, index) => ({ ...change, id: `change-${index}`, name: `workload-${index}`, uid: `uid-${index}`, at: new Date(Date.parse(change.at) - index * 1_000).toISOString() }));
    const lookup = vi.spyOn(api, "kubernetesTimeline").mockResolvedValue(timelinePage(changes.map(changeEntry)));
    const logs = vi.spyOn(api, "kubernetesBreakLogs");
    const select = vi.fn();
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={select} />);
    await screen.findByText("workload-0", { exact: false });
    const list = screen.getByRole("list", { name: "Breaks and changes" });
    expect(within(list).getAllByRole("listitem")).toHaveLength(20);
    expect(within(list).queryByText("workload-99", { exact: false })).toBeNull();
    fireEvent.scroll(list, { target: { scrollTop: 5_800 } });
    const last = within(list).getByText("workload-99", { exact: false }).closest("article")!;
    expect(within(list).getAllByRole("listitem")).toHaveLength(20);
    expect(within(list).queryByText("workload-0", { exact: false })).toBeNull();
    fireEvent.click(within(last).getByRole("button", { name: "Open change details" }));
    expect(select).toHaveBeenCalledWith(expect.objectContaining({ uid: "uid-99", name: "workload-99" }), "timeline");
    expect(lookup).toHaveBeenCalledOnce();
    expect(screen.queryByRole("region", { name: "Resource timeline chart" })).toBeNull();
    expect(screen.queryByRole("region", { name: "Captured crash evidence" })).toBeNull();
    expect(logs).not.toHaveBeenCalled();
  });

  it("pages one globally ordered mixed-source snapshot without changing cursor filters or merging live updates", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-10-14T12:06:00Z"));
    window.history.replaceState(null, "", "/agent/kubernetes?view=timeline&namespace=shop&kind=Deployment");
    const at = (minute: number) => `2026-10-14T12:0${minute}:00.000Z`;
    const first: KubernetesTimelineEntry[] = [
      changeEntry({ ...change, id: "new-change", at: at(5) }),
      { type: "crash", id: "middle-crash", at: at(4), crash: { ...observation, id: "middle-crash", at: at(4) } },
      { type: "break", id: "middle-break", at: at(3), issue: { ...issue, broke_at: at(3) } },
    ];
    const second: KubernetesTimelineEntry[] = [
      { type: "crash", id: "older-crash", at: at(2), crash: { ...observation, id: "older-crash", at: at(2) } },
      changeEntry({ ...change, id: "older-change", at: at(1) }),
      { type: "break", id: "older-break", at: at(0), issue: { ...issue, broke_at: at(0) } },
    ];
    const totals = { total: 6, crash_count: 2, counts: { change: 2, crash: 2, break: 2 } };
    const cursor = "immutable+/=snapshot-page-2";
    const lookup = vi.spyOn(api, "kubernetesTimeline").mockImplementation(async (options) => timelinePage(options.cursor ? second : first, { ...totals, next: options.cursor ? undefined : cursor }));
    const changes = vi.spyOn(api, "kubernetesChanges");
    const crashes = vi.spyOn(api, "kubernetesCrashes");
    const issues = vi.spyOn(api, "kubernetesIssues");
    const related = vi.spyOn(api, "kubernetesRelated");
    const logs = vi.spyOn(api, "kubernetesBreakLogs");
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    await act(async () => { await vi.advanceTimersByTimeAsync(1); });
    const times = () => [...screen.getByRole("list", { name: "Breaks and changes" }).querySelectorAll("time")].map((time) => time.dateTime);
    expect(times()).toEqual(first.map((entry) => entry.at));
    expect(screen.getByText("6 events and 2 crashes")).toBeTruthy();
    expect(screen.getAllByRole("navigation", { name: "Timeline pages" })).toHaveLength(1);
    expect(screen.getAllByRole("button", { name: "Next page" })).toHaveLength(1);
    expect(lookup).toHaveBeenCalledExactlyOnceWith({
      since: "2026-10-14T11:06:00.000Z", until: at(6), namespace: "shop", kind: "Deployment", limit: 100, cursor: undefined,
    }, expect.any(AbortSignal));
    const firstFilters = lookup.mock.calls[0][0];
    fireEvent.click(screen.getByRole("button", { name: "Next page" }));
    await act(async () => { await vi.advanceTimersByTimeAsync(1); });
    expect(times()).toEqual(second.map((entry) => entry.at));
    expect([...first, ...second].map((entry) => entry.at)).toEqual([5, 4, 3, 2, 1, 0].map(at));
    expect(lookup.mock.calls[1][0]).toEqual({ ...firstFilters, cursor });
    expect(lookup.mock.calls[1][1]).toBeInstanceOf(AbortSignal);
    expect(screen.getByRole("button", { name: "Next page" }).hasAttribute("disabled")).toBe(true);
    expect(screen.getByRole("button", { name: "Previous page" }).hasAttribute("disabled")).toBe(false);
    lookup.mockResolvedValue(timelinePage([changeEntry({ ...change, id: "arrived-after-snapshot", at: at(6) })]));
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(lookup).toHaveBeenCalledTimes(2);
    expect(times()).toEqual(second.map((entry) => entry.at));
    expect(screen.getByText("6 events and 2 crashes")).toBeTruthy();
    for (const legacy of [changes, crashes, issues, related, logs]) expect(legacy).not.toHaveBeenCalled();
  });

  it("retains first-page rows and focused actions while a fresh thirty-second snapshot is pending", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date("2026-10-14T12:06:00Z"));
    let resolveRefresh: (page: KubernetesTimelinePage) => void = () => { throw new Error("refresh not started"); };
    const lookup = vi.spyOn(api, "kubernetesTimeline")
      .mockResolvedValueOnce(timelinePage([changeEntry()]))
      .mockImplementationOnce(() => new Promise<KubernetesTimelinePage>((resolve) => { resolveRefresh = resolve; }));
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    await act(async () => { await vi.advanceTimersByTimeAsync(1); });
    const list = screen.getByRole("list", { name: "Breaks and changes" });
    const action = screen.getByRole("button", { name: "Open change details" });
    action.focus();
    await act(async () => { await vi.advanceTimersByTimeAsync(30_000); });
    expect(lookup).toHaveBeenCalledTimes(2);
    expect(lookup.mock.calls[1][0]).toMatchObject({
      cursor: undefined, since: "2026-10-14T11:06:30.000Z", until: "2026-10-14T12:06:30.000Z",
    });
    expect(screen.getByRole("list", { name: "Breaks and changes" })).toBe(list);
    expect(screen.getByRole("button", { name: "Open change details" })).toBe(action);
    expect(document.activeElement).toBe(action);
    await act(async () => { resolveRefresh(timelinePage([changeEntry()], { until: "2026-10-14T12:06:30.000Z" })); await vi.advanceTimersByTimeAsync(1); });
    expect(screen.getByRole("button", { name: "Open change details" })).toBe(action);
    expect(document.activeElement).toBe(action);
  });

  it("offers an explicit snapshot restart for an expired cursor instead of retrying it or merging stale rows", async () => {
    vi.spyOn(Date, "now").mockReturnValue(Date.parse("2026-10-14T12:06:00Z"));
    const lookup = vi.spyOn(api, "kubernetesTimeline")
      .mockResolvedValueOnce(timelinePage([changeEntry()], { next: "expired-snapshot" }))
      .mockRejectedValueOnce(new ApiError(400, "Timeline snapshot expired"))
      .mockResolvedValueOnce(timelinePage([changeEntry({ ...change, id: "replacement", name: "replacement-workload" })]));
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    await screen.findByRole("button", { name: "Open change details" });
    fireEvent.click(screen.getByRole("button", { name: "Next page" }));
    const restart = await screen.findByRole("button", { name: "Restart Timeline" });
    expect(lookup).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole("button", { name: "Open change details" })).toBeNull();
    fireEvent.click(restart);
    await screen.findByText("replacement-workload", { exact: false });
    expect(lookup).toHaveBeenCalledTimes(3);
    expect(lookup.mock.calls[2][0]).toEqual({ ...lookup.mock.calls[0][0], cursor: undefined });
    expect(screen.queryByRole("button", { name: "Restart Timeline" })).toBeNull();
    // A single page renders no pager at all, so nothing invites a first-page move.
    expect(screen.queryByRole("button", { name: "Previous page" })).toBeNull();
  });

  it.each([401, 403])("does not expose cached Timeline rows or source fallbacks after a cursor read is denied with %s", async (status) => {
    const lookup = vi.spyOn(api, "kubernetesTimeline")
      .mockResolvedValueOnce(timelinePage([changeEntry()], { next: "protected-snapshot" }))
      .mockRejectedValueOnce(new ApiError(status, "Access denied"));
    const changes = vi.spyOn(api, "kubernetesChanges");
    const crashes = vi.spyOn(api, "kubernetesCrashes");
    const issues = vi.spyOn(api, "kubernetesIssues");
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    await screen.findByRole("button", { name: "Open change details" });
    fireEvent.click(screen.getByRole("button", { name: "Next page" }));
    await screen.findByRole("button", { name: "Retry Timeline" });
    expect(lookup).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole("button", { name: "Open change details" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Restart Timeline" })).toBeNull();
    for (const legacy of [changes, crashes, issues]) expect(legacy).not.toHaveBeenCalled();
  });

  it.each(["syncing", "ready"])("distinguishes a %s empty Timeline from incomplete or unhealthy history", async (state) => {
    vi.spyOn(api, "kubernetesTimeline").mockResolvedValue(timelinePage([], { sync: { state, age_s: 0, partial: false } }));
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    if (state === "syncing") {
      await screen.findByRole("region", { name: "Preparing your cluster" });
      expect(screen.queryByText("0 events and 0 crashes")).toBeNull();
      expect(screen.queryByText("No changes or crashes in this range.")).toBeNull();
    } else {
      await screen.findByText("0 events and 0 crashes");
      await screen.findByText("No changes or crashes in this range.");
      expect(screen.queryByRole("region", { name: "Preparing your cluster" })).toBeNull();
    }
    expect(screen.queryByRole("button", { name: "Resource coverage" })).toBeNull();
    expect(screen.queryByText(/partial history/i)).toBeNull();
    // One page of nothing is still one page: no pager to offer.
    expect(screen.queryByRole("button", { name: "Next page" })).toBeNull();
  });

  it("preserves namespace, kind and cluster from the URL when clearing only the root filter", async () => {
    window.history.replaceState(null, "", `/agent/kubernetes?view=timeline&root_uid=checkout-uid&namespace=shop&kind=Deployment&cluster=east&center=${issue.broke_at}`);
    const sync = { state: "ready", age_s: 0, partial: false };
    vi.spyOn(api, "kubernetesIssues").mockImplementation(rootIssueResponses({ items: [issue], totals: {}, truncated: false, sync }));
    vi.spyOn(api, "kubernetesRelated").mockResolvedValue({ nodes: [], edges: [], omitted: {}, sync });
    vi.spyOn(api, "kubernetesCrashes").mockResolvedValue(coveredJournalPage);
    vi.spyOn(api, "kubernetesChanges").mockResolvedValue({ items: [change], sync });
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    await screen.findByRole("region", { name: "Resource timeline chart" });
    const query = new URLSearchParams(screen.getByRole("link", { name: "Clear root filter" }).getAttribute("href")!.split("?")[1]);
    expect(Object.fromEntries(query)).toEqual({ view: "timeline", cluster: "east", namespace: "shop", kind: "Deployment" });
  });

  it("discloses partial/gap history without making name-only graph reads for retained roots", async () => {
    window.history.replaceState(null, "", `/agent/kubernetes?view=timeline&root_uid=checkout-uid&namespace=shop&center=${issue.broke_at}`);
    const sync = { state: "ready", age_s: 0, partial: true };
    vi.spyOn(api, "kubernetesIssues").mockImplementation(rootIssueResponses({ items: [{ ...issue, changes: [{ change, relation: "same_object", lead_seconds: 240 }] }], totals: {}, truncated: false, sync }));
    vi.spyOn(api, "kubernetesRelated").mockRejectedValue(new ApiError(403, "Forbidden"));
    vi.spyOn(api, "kubernetesCrashes").mockResolvedValue(coveredJournalPage);
    vi.spyOn(api, "kubernetesChanges").mockResolvedValue({ items: [change], truncated: true, sync, gaps: [{ from: change.at, to: issue.broke_at! }], partial_failures: [{ resource_id: "ConfigMap", class: "forbidden" }] });
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    expect(await screen.findByRole("button", { name: /Deployment checkout:.* at / })).toBeTruthy();
    expect(screen.queryByText(/Related-resource graph unavailable/)).toBeNull();
    const marker = screen.getByRole("button", { name: "Resource coverage" });
    expect(marker.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(marker);
    expect(screen.queryByText(/Related-resource graph unavailable/)).toBeNull();
    expect(api.kubernetesRelated).not.toHaveBeenCalled();
    expect(screen.getByText("ConfigMap: permission denied")).toBeTruthy();
    expect(screen.getByText("Change capture does not cover the whole range")).toBeTruthy();
  });

  it("stops repeated internal root cursors and reports incomplete evidence without a pager", async () => {
    window.history.replaceState(null, "", `/agent/kubernetes?view=timeline&root_uid=checkout-uid&namespace=shop&center=${issue.broke_at}`);
    const sync = { state: "ready", age_s: 0, partial: false };
    vi.spyOn(api, "kubernetesIssues").mockImplementation(rootIssueResponses({ items: [issue], totals: {}, truncated: false, sync }));
    vi.spyOn(api, "kubernetesRelated").mockResolvedValue({ nodes: [], edges: [], omitted: {}, sync });
    vi.spyOn(api, "kubernetesCrashes").mockResolvedValue(coveredJournalPage);
    const lookup = vi.spyOn(api, "kubernetesChanges").mockResolvedValue({ items: [], next: "repeat", sync });
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    await screen.findByText(/Change evidence is incomplete for this range/);
    expect(screen.queryByRole("button", { name: "Next page" })).toBeNull();
    expect(lookup).toHaveBeenCalledTimes(2);
    expect(screen.queryByText("Related change history is unavailable.")).toBeNull();
  });

  it("keeps a retained cluster-scoped root independent of name-only related-resource lookups", async () => {
    window.history.replaceState(null, "", `/agent/kubernetes?view=timeline&root_uid=checkout-uid&center=${issue.broke_at}`);
    const sync = { state: "ready", age_s: 0, partial: false };
    vi.spyOn(api, "kubernetesIssues").mockImplementation(rootIssueResponses({ items: [{ ...issue, root: { ...issue.root, namespace: undefined } }], totals: {}, truncated: false, sync }));
    const graph = vi.spyOn(api, "kubernetesGraph");
    const related = vi.spyOn(api, "kubernetesRelated").mockResolvedValue({ nodes: [{ id: "checkout-uid", kind: "Deployment", name: "checkout", group: "cluster" }], edges: [], omitted: {}, sync });
    vi.spyOn(api, "kubernetesCrashes").mockResolvedValue(coveredJournalPage);
    vi.spyOn(api, "kubernetesChanges").mockResolvedValue({ items: [change], sync });
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    expect(await screen.findByRole("button", { name: /Deployment checkout:.* at / })).toBeTruthy();
    expect(graph).not.toHaveBeenCalled();
    expect(related).not.toHaveBeenCalled();
    expect(screen.queryByText("Related change history is unavailable.")).toBeNull();
  });

  it("retries a failed persisted context read without any live graph fallback", async () => {
    window.history.replaceState(null, "", `/agent/kubernetes?view=timeline&root_uid=checkout-uid&center=${issue.broke_at}`);
    const sync = { state: "ready", age_s: 0, partial: false };
    let failed = false;
    const issues = vi.spyOn(api, "kubernetesIssues").mockImplementation(async (options) => {
      if (options?.context === "persisted") {
        if (!failed) { failed = true; throw new ApiError(503, "storage unavailable"); }
        return persistedRootPage(issue, sync);
      }
      return { items: [issue], totals: {}, truncated: false, sync };
    });
    const graph = vi.spyOn(api, "kubernetesGraph");
    const related = vi.spyOn(api, "kubernetesRelated");
    vi.spyOn(api, "kubernetesCrashes").mockResolvedValue(coveredJournalPage);
    vi.spyOn(api, "kubernetesChanges").mockResolvedValue({ items: [change], sync });
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    fireEvent.click(await screen.findByRole("button", { name: "Retry root context" }));
    expect(await screen.findByRole("button", { name: /Deployment checkout:.* at / })).toBeTruthy();
    expect(issues.mock.calls.filter(([options]) => options?.context === "persisted")).toHaveLength(2);
    expect(graph).not.toHaveBeenCalled();
    expect(related).not.toHaveBeenCalled();
  });

  it.each(["changes", "context"] as const)("does not start continuation reads when an in-flight %s request finishes after unmount", async (stage) => {
    window.history.replaceState(null, "", `/agent/kubernetes?view=timeline&root_uid=checkout-uid&center=${issue.broke_at}`);
    const sync = { state: "ready", age_s: 0, partial: false };
    let finishChanges!: (page: KubernetesChangesPage) => void;
    let finishContext!: (page: KubernetesIssuePage) => void;
    const pendingChanges = new Promise<KubernetesChangesPage>((resolve) => { finishChanges = resolve; });
    const pendingContext = new Promise<KubernetesIssuePage>((resolve) => { finishContext = resolve; });
    const issues = vi.spyOn(api, "kubernetesIssues").mockImplementation(async (options) => options?.context === "persisted"
      ? stage === "context" ? pendingContext : persistedRootPage(issue, sync)
      : { items: [issue], totals: {}, truncated: false, sync });
    vi.spyOn(api, "kubernetesCrashes").mockResolvedValue(coveredJournalPage);
    const changes = vi.spyOn(api, "kubernetesChanges").mockImplementation(() => pendingChanges);
    const { client, unmount } = mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    const lookup = stage === "context" ? issues : changes;
    await waitFor(() => expect(lookup).toHaveBeenCalledOnce());
    const query = client.getQueryCache().find({ queryKey: kubernetesKey("east", stage === "context" ? "kubernetes-issues" : "kubernetes-changes", stage === "context" ? "timeline-root-context" : "timeline-root"), exact: false })!;
    unmount();
    await act(async () => {
      if (stage === "context") finishContext(persistedRootPage(issue, sync));
      else finishChanges({ items: [change], next: "next", sync });
      await (stage === "context" ? pendingContext : pendingChanges);
    });
    expect(lookup).toHaveBeenCalledOnce();
    expect(query.state.data).toBeUndefined();
    expect(lookup.mock.calls[0][1]?.aborted).toBe(true);
    expect(query.state.fetchStatus).toBe("idle");
    expect(query.state.error).toBeNull();
  });

  it("does not invent graph-based missing-link warnings for server-filtered empty history", async () => {
    window.history.replaceState(null, "", `/agent/kubernetes?view=timeline&root_uid=checkout-uid&namespace=shop&center=${issue.broke_at}`);
    const sync = { state: "ready", age_s: 0, partial: false };
    vi.spyOn(api, "kubernetesIssues").mockImplementation(rootIssueResponses({ items: [{ ...issue, more_changes: 4 }], totals: {}, truncated: false, sync }));
    vi.spyOn(api, "kubernetesRelated").mockResolvedValue({ nodes: [{ id: "checkout-uid", kind: "Deployment", namespace: "shop", name: "checkout", group: "shop" }], edges: [], omitted: {}, sync });
    vi.spyOn(api, "kubernetesCrashes").mockResolvedValue(coveredJournalPage);
    vi.spyOn(api, "kubernetesChanges").mockResolvedValue({ items: [], sync });
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    expect(await screen.findByText("Break: CrashLoopBackOff")).toBeTruthy();
    expect(screen.queryByText(/Related-resource evidence is incomplete/)).toBeNull();
    expect(screen.queryByRole("button", { name: "Resource coverage" })).toBeNull();
  });

  it.each(["gap", "reconstructed", undefined] as const)("never claims no changes for %s history", (history) => {
    mount(<BreakRow issue={{ ...issue, history, rebuilt: history === "reconstructed" ? ["image"] : undefined }} resource={resource} onSelectResource={vi.fn()} />);
    expect(screen.getByText("No recorded change in available evidence")).toBeTruthy();
    expect(screen.queryByText(/No change on it/)).toBeNull();
  });

  it("names untracked dependencies and bounded evidence without provenance pills", () => {
    mount(<BreakRow issue={{ ...issue, untracked: ["Secret"] }} resource={resource} onSelectResource={vi.fn()} />);
    expect(screen.queryByText("Secret history is not tracked")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Resource coverage" }));
    expect(screen.getByText("Secret history is not tracked")).toBeTruthy();
    expect(screen.getByText("OOMKilled on 6 pods")).toBeTruthy();
    expect(screen.queryByText("complete")).toBeNull();
  });

  it("does not claim config coverage when ConfigMaps cannot be read", () => {
    mount(<BreakRow issue={{ ...issue, untracked: ["ConfigMap", "Secret"] }} resource={resource} onSelectResource={vi.fn()} sourceSync={{ state: "ready", age_s: 0, partial: false }} />);
    expect(screen.getByText("No recorded change in tracked resources.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Resource coverage" }));
    expect(screen.getByText("ConfigMap history is not tracked")).toBeTruthy();
    expect(screen.getByText("Secret history is not tracked")).toBeTruthy();
    expect(screen.queryByText(/No change on it, its config/)).toBeNull();
  });

  it("preserves generic untracked kinds without claiming temporal coverage for them", () => {
    mount(<BreakRow issue={{ ...issue, untracked: ["PersistentVolumeClaim", "ServiceAccount"] }} resource={resource} onSelectResource={vi.fn()} sourceSync={{ state: "ready", age_s: 0, partial: false }} />);
    expect(screen.getByText("No recorded change in tracked resources.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Resource coverage" }));
    expect(screen.getByText("PersistentVolumeClaim history is not tracked")).toBeTruthy();
    expect(screen.getByText("ServiceAccount history is not tracked")).toBeTruthy();
    expect(screen.queryByText("History incomplete.")).toBeNull();
  });

  it("qualifies partial correlation instead of declaring complete no-change evidence", () => {
    mount(<BreakRow issue={{ ...issue, partial: true }} resource={resource} onSelectResource={vi.fn()} sourceSync={{ state: "ready", age_s: 0, partial: false }} />);
    expect(screen.getByText("No recorded change in available evidence")).toBeTruthy();
    expect(screen.queryByText(/No change on it/)).toBeNull();
  });

  it("does not claim no changes when complete history still reports a gap", () => {
    mount(<BreakRow issue={{ ...issue, gaps: [{ from: change.at, to: issue.broke_at! }] }} resource={resource} onSelectResource={vi.fn()} />);
    expect(screen.queryByText(/No change on it/)).toBeNull();
    const toggle = screen.getByRole("button", { name: "Resource coverage" });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(toggle);
    expect(screen.getByText("Change capture does not cover the whole range")).toBeTruthy();
    expect(screen.getByText("OOMKilled on 6 pods")).toBeTruthy();
  });

  it("assigns unique controls to repeated root disclosures", () => {
    const gaps = [{ from: change.at, to: issue.broke_at! }];
    mount(<><BreakRow issue={{ ...issue, gaps }} resource={resource} onSelectResource={vi.fn()} /><BreakRow issue={{ ...issue, gaps }} resource={resource} onSelectResource={vi.fn()} /></>);
    const toggles = screen.getAllByRole("button", { name: "Resource coverage" });
    fireEvent.click(toggles[0]);
    expect(toggles[1].getAttribute("aria-expanded")).toBe("false");
    const firstID = toggles[0].getAttribute("aria-controls");
    fireEvent.click(toggles[1]);
    expect(toggles[1].getAttribute("aria-controls")).not.toBe(firstID);
  });

  it("renders from/to, lead, timestamp and root-focused navigation", () => {
    mount(<BreakRow issue={{ ...issue, changes: [{ change, relation: "same_object", lead_seconds: 240 }] }} resource={resource} onSelectResource={vi.fn()} />);
    expect(screen.getByText("v41 → v42")).toBeTruthy();
    expect(screen.getByText("4 m before")).toBeTruthy();
    expect(screen.queryByText("same_object")).toBeNull();
    expect(screen.queryByText("reconstructed")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Timeline for checkout" }));
    const query = new URLSearchParams(screen.getByLabelText("Location").textContent!.split("?")[1]);
    expect(query.get("root_uid")).toBe("checkout-uid");
    expect(query.get("namespace")).toBe("shop");
    expect(query.get("center")).toBe(issue.broke_at);
  });

  it("requests server-computed investigation context using identity only", () => {
    mount(<BreakRow issue={issue} resource={resource} onSelectResource={vi.fn()} />);
    fireEvent.click(screen.getByRole("button", { name: "Investigate checkout" }));
    const query = new URLSearchParams(screen.getByLabelText("Location").textContent!.split("?")[1]);
    expect(query.get("uid")).toBe("checkout-uid");
    expect(query.get("include_changes")).toBe("true");
    expect(query.get("cluster")).toBe("east");
    expect(query.has("changes")).toBe(false);
  });

  it("expands one scoped root only and reuses its cached lookup", async () => {
    const lookup = vi.spyOn(api, "kubernetesIssues").mockImplementation(rootIssueResponses({ items: [issue], totals: {}, broken: { total: 1, by_severity: { critical: 1 } }, truncated: false, sync: { state: "ready", age_s: 0, partial: false } }));
    const { client } = mount(<BreakRow issue={issue} resource={resource} onSelectResource={vi.fn()} collapsed />);
    expect(lookup).not.toHaveBeenCalled();
    expect(screen.getByText("OOMKilled on 6 pods")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Expand checkout" }));
    await waitFor(() => expect(client.getQueryData(kubernetesKey("east", "kubernetes-issues", "root", "shop", "checkout-uid"))).toBeTruthy());
    expect(screen.getByText("OOMKilled on 6 pods")).toBeTruthy();
    expect(lookup).toHaveBeenCalledWith({ root_uid: "checkout-uid", include: "changes", namespace: "shop" });
    expect(client.getQueryData(kubernetesKey("east", "kubernetes-issues", "root", "shop", "checkout-uid"))).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Collapse checkout" }));
    fireEvent.click(screen.getByRole("button", { name: "Expand checkout" }));
    expect(lookup).toHaveBeenCalledTimes(1);
  });

  it.each([undefined, "reconstructed", "complete"] as const)("keeps collapsed %s history free of per-row coverage markers and eager root reads", async (history) => {
    const detail = { ...issue, history: "reconstructed" as const, rebuilt: ["image"], partial: true };
    const lookup = vi.spyOn(api, "kubernetesIssues").mockImplementation(rootIssueResponses({ items: [detail], totals: {}, truncated: false, sync: { state: "ready", age_s: 1, partial: false } }));
    mount(<BreakRow issue={{ ...issue, history, evidence: undefined }} resource={resource} onSelectResource={vi.fn()} collapsed />);
    expect(lookup).not.toHaveBeenCalled();
    expect(screen.queryByRole("button", { name: "Resource coverage" })).toBeNull();
    expect(screen.getByRole("button", { name: "Expand checkout" }).getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(screen.getByRole("button", { name: "Expand checkout" }));
    expect(await screen.findByText("OOMKilled on 6 pods")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Resource coverage" })).toBeNull();
    expect(screen.getByText("No recorded change in available evidence")).toBeTruthy();
    expect(screen.queryByText("No change on it, its config or its nodes")).toBeNull();
    expect(lookup).toHaveBeenCalledOnce();
  });

  it("retains open aggregate warnings and focus when sync age and change timestamps refresh", async () => {
    const sync = { state: "stale", age_s: 4, partial: false };
    const page = { items: [issue], totals: {}, truncated: false, sync };
    vi.spyOn(api, "kubernetesIssues").mockImplementation(rootIssueResponses(page));
    const { client } = mount(<TimelineCard changes={{ items: [change], sync }} windowEnd={Date.now()} clusterId="east" resourceFor={() => resource} onSelectView={vi.fn()} onSelectResource={vi.fn()} />);
    const toggle = await screen.findByRole("button", { name: "Resource coverage" });
    fireEvent.click(toggle);
    toggle.focus();
    await act(async () => { client.setQueryData(kubernetesKey("east", "kubernetes-issues", "broken", "east"), { ...page, sync: { ...sync, age_s: 34 } }); });
    expect(screen.getByRole("button", { name: "Resource coverage" })).toBe(toggle);
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    expect(document.activeElement).toBe(toggle);
  });

  it("aggregates child issue coverage into one Timeline footer marker without hiding primary evidence", async () => {
    const sync = { state: "ready", age_s: 0, partial: false };
    const enriched = { ...issue, untracked: ["Secret"], gaps: [{ from: "2026-10-14T11:50:00Z", to: "2026-10-14T11:51:00Z" }], evidence: { ...issue.evidence!, partial: true } };
    vi.spyOn(api, "kubernetesIssues").mockImplementation(rootIssueResponses({ items: [enriched], totals: {}, truncated: false, sync }));
    mount(<TimelineCard changes={{ items: [], sync }} windowEnd={Date.now()} clusterId="east" resourceFor={() => resource} onSelectView={vi.fn()} onSelectResource={vi.fn()} />);
    await screen.findByText("OOMKilled on 6 pods");
    const card = screen.getByRole("region", { name: "Break timeline" });
    expect(within(card).getAllByRole("button", { name: "Resource coverage" })).toHaveLength(1);
    const marker = within(card).getByRole("button", { name: "Resource coverage" });
    expect(marker.closest("article")).toBeNull();
    fireEvent.click(marker);
    const reasons = screen.getByRole("region", { name: "Resource coverage details" });
    for (const text of ["Secret history is not tracked", "Change capture does not cover the whole range", "Some crash evidence could not be captured"]) {
      expect(within(reasons).getAllByText(text)).toHaveLength(1);
    }
    expect(screen.getByText("CrashLoopBackOff")).toBeTruthy();
  });

  it("delegates Timeline coverage entirely to its parent when warnings are parent-owned", async () => {
    const sync = { state: "ready", age_s: 0, partial: true };
    vi.spyOn(api, "kubernetesIssues").mockResolvedValue({
      items: [{ ...issue, untracked: ["Secret"], evidence: { ...issue.evidence!, partial: true } }],
      totals: {}, truncated: false, sync, partial_failures: [{ resource_id: "ConfigMap", class: "forbidden" }],
    });
    mount(<TimelineCard warningsOwned changes={{ items: [], sync }} windowEnd={Date.now()} clusterId="east" resourceFor={() => resource} onSelectView={vi.fn()} onSelectResource={vi.fn()} />);
    await screen.findByText("OOMKilled on 6 pods");
    expect(screen.queryByRole("button", { name: "Resource coverage" })).toBeNull();
    expect(screen.queryByText("Secret history is not tracked")).toBeNull();
    expect(screen.getByText("CrashLoopBackOff")).toBeTruthy();
  });

  it("keeps rebuilt provenance out of coverage while preserving unreadable resources and one change-gap reason", async () => {
    window.history.replaceState(null, "", `/agent/kubernetes?view=timeline&root_uid=checkout-uid&namespace=shop&center=${issue.broke_at}`);
    const sync = { state: "ready", age_s: 0, partial: false };
    const gaps = [{ from: "2026-10-14T11:50:00Z", to: "2026-10-14T11:51:00Z" }, { from: "2026-10-14T11:52:00Z", to: "2026-10-14T11:53:00Z" }];
    const page = { items: [{ ...issue, history: "reconstructed" as const, rebuilt: ["image"], untracked: ["Secret"], gaps }], totals: {}, truncated: false, sync };
    const lookup = vi.spyOn(api, "kubernetesIssues").mockImplementation(rootIssueResponses(page));
    vi.spyOn(api, "kubernetesRelated").mockResolvedValue({ nodes: [{ id: "checkout-uid", kind: "Deployment", namespace: "shop", name: "checkout", group: "shop" }], edges: [], omitted: {}, partial_failures: [{ resource_id: "ConfigMap", class: "forbidden" }], sync });
    vi.spyOn(api, "kubernetesCrashes").mockResolvedValue(coveredJournalPage);
    vi.spyOn(api, "kubernetesChanges").mockResolvedValue({ items: [change], gaps: [...gaps, gaps[0]], partial_failures: [{ resource_id: "ConfigMap", class: "forbidden" }], sync });
    const { client } = mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    expect(await screen.findByRole("button", { name: /Deployment checkout:.* at / })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Resource coverage" }));
    expect(screen.queryByText(/Image history rebuilt from ReplicaSets/)).toBeNull();
    expect(screen.getByText("Secret history is not tracked")).toBeTruthy();
    expect(screen.getByText("ConfigMap: permission denied")).toBeTruthy();
    expect(screen.getAllByText("Change capture does not cover the whole range")).toHaveLength(1);
    const toggle = screen.getByRole("button", { name: "Resource coverage" });
    toggle.focus();
    await act(async () => { client.setQueryData(kubernetesKey("east", "kubernetes-issues", "timeline-root", "checkout-uid", "shop"), { ...page, sync: { ...sync, age_s: 30 } }); });
    expect(screen.getByRole("button", { name: "Resource coverage" })).toBe(toggle);
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    expect(document.activeElement).toBe(toggle);
    lookup.mockRejectedValue(new ApiError(403, "Forbidden"));
    await act(async () => { await client.invalidateQueries({ queryKey: kubernetesKey("east", "kubernetes-issues", "timeline-root", "checkout-uid", "shop") }); });
    expect(await screen.findByText("Root history unavailable.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Resource coverage" })).toBeNull();
    expect(screen.queryByRole("region", { name: "Resource coverage details" })).toBeNull();
  });

  it.each([401, 403, 404])("keeps denied %s root history out of collapsed rows and isolates cached history across clusters", async (status) => {
    const cached = { ...issue, history: "reconstructed" as const, rebuilt: ["private"], untracked: ["CachedKind"], changes: [{ change: { ...change, name: "cached-east-config" }, relation: "same_object", lead_seconds: 240 }] };
    const page = { items: [cached], totals: {}, truncated: false, sync: { state: "ready", age_s: 0, partial: false } };
    const lookup = vi.spyOn(api, "kubernetesIssues").mockImplementation(rootIssueResponses(page));
    function Scenario() {
      const [cluster, setCluster] = useState("east");
      return <KubernetesClusterContext.Provider value={{ client: api, cluster, multiple: true, clusters: [], key: (...parts) => kubernetesKey(cluster, ...parts) }}><button onClick={() => setCluster("west")}>Switch cluster</button><BreakRow issue={{ ...issue, history: undefined, evidence: undefined, partial: true, untracked: ["Secret"] }} resource={resource} onSelectResource={vi.fn()} collapsed /></KubernetesClusterContext.Provider>;
    }
    const { client } = mount(<Scenario />);
    expect(lookup).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Expand checkout" }));
    expect(await screen.findByText("OOMKilled on 6 pods")).toBeTruthy();
    expect(screen.getByText(/cached-east-config/)).toBeTruthy();
    lookup.mockRejectedValue(new ApiError(status, "Denied"));
    await act(async () => { await client.invalidateQueries({ queryKey: kubernetesKey("east", "kubernetes-issues", "root", "shop", "checkout-uid") }); });
    expect(await screen.findByText("Root history unavailable.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Collapse checkout" }));
    expect(screen.queryByRole("button", { name: "Resource coverage" })).toBeNull();
    expect(screen.queryByText(/cached-east-config|Private history|CachedKind/)).toBeNull();
    expect(screen.queryByText("OOMKilled on 6 pods")).toBeNull();
    expect(screen.getByText("CrashLoopBackOff")).toBeTruthy();
    expect(lookup).toHaveBeenCalledTimes(2);
    await act(async () => { client.setQueryData(kubernetesKey("west", "kubernetes-issues", "root", "shop", "checkout-uid"), { ...page, items: [{ ...cached, changes: [{ ...cached.changes[0], change: { ...change, cluster: "west", name: "cached-west-config" } }] }] }, { updatedAt: 1 }); });
    fireEvent.click(screen.getByRole("button", { name: "Switch cluster" }));
    expect(screen.queryByRole("button", { name: "Resource coverage" })).toBeNull();
    expect(lookup).toHaveBeenCalledTimes(2);
    fireEvent.click(screen.getByRole("button", { name: "Expand checkout" }));
    expect(await screen.findByText("Root history unavailable.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Collapse checkout" }));
    expect(screen.queryByRole("button", { name: "Resource coverage" })).toBeNull();
    expect(screen.queryByText(/cached-east-config|cached-west-config|Private history|CachedKind|OOMKilled/)).toBeNull();
    expect(lookup).toHaveBeenCalledTimes(3);
  });

  it.each([
    { state: "ready", age_s: 0, partial: false },
    { state: "stale", age_s: 90, partial: false },
    { state: "ready", age_s: 0, partial: true },
    { state: "unknown", age_s: 0, partial: false },
  ])("qualifies empty root history using enrichment sync $state partial=$partial", async (sync) => {
    const ready = { state: "ready", age_s: 0, partial: false };
    vi.spyOn(api, "kubernetesIssues").mockImplementation(rootIssueResponses({ items: [issue], totals: {}, truncated: false, sync }));
    mount(<BreakRow issue={issue} resource={resource} onSelectResource={vi.fn()} sourceSync={ready} collapsed />);
    fireEvent.click(screen.getByRole("button", { name: "Expand checkout" }));
    await waitFor(() => expect(screen.queryByText("Loading root history…")).toBeNull());
    expect(screen.getByText("OOMKilled on 6 pods")).toBeTruthy();
    const complete = sync.state === "ready" && !sync.partial;
    expect(screen.getByText(complete ? "No change on it, its config or its nodes" : "No recorded change in available evidence")).toBeTruthy();
    if (!complete) expect(screen.queryByText("No change on it, its config or its nodes")).toBeNull();
  });

  it("does not infer complete history when enrichment has no matching root", async () => {
    const ready = { state: "ready", age_s: 0, partial: false };
    vi.spyOn(api, "kubernetesIssues").mockImplementation(rootIssueResponses({ items: [], totals: {}, truncated: false, sync: ready }));
    mount(<BreakRow issue={issue} resource={resource} onSelectResource={vi.fn()} sourceSync={ready} collapsed />);
    fireEvent.click(screen.getByRole("button", { name: "Expand checkout" }));
    await waitFor(() => expect(screen.queryByText("Loading root history…")).toBeNull());
    expect(screen.queryByText("No change on it, its config or its nodes")).toBeNull();
  });

  it.each([
    { state: "ready", age_s: 0, partial: false },
    { state: "stale", age_s: 90, partial: false },
    { state: "ready", age_s: 0, partial: true },
    { state: "unknown", age_s: 0, partial: false },
  ])("qualifies overview row history using parent sync $state partial=$partial", async (sync) => {
    vi.spyOn(api, "kubernetesIssues").mockImplementation(rootIssueResponses({ items: [issue], totals: {}, truncated: false, sync }));
    mount(<TimelineCard windowEnd={Date.now()} clusterId="east" resourceFor={() => resource} onSelectView={vi.fn()} onSelectResource={vi.fn()} />);
    const row = await screen.findByRole("article", { name: "Deployment checkout" });
    const complete = sync.state === "ready" && !sync.partial;
    expect(within(row).getByText(complete ? "No change on it, its config or its nodes" : "No recorded change in available evidence")).toBeTruthy();
    if (!complete) expect(within(row).queryByText("No change on it, its config or its nodes")).toBeNull();
  });

  it("qualifies complete detail without source sync", () => {
    mount(<BreakRow issue={issue} resource={resource} onSelectResource={vi.fn()} />);
    expect(screen.getByText("No recorded change in available evidence")).toBeTruthy();
    expect(screen.queryByText("No change on it, its config or its nodes")).toBeNull();
  });

  it("aligns resource lanes on one proportional time axis and retains actual crash Last", () => {
    const select = vi.fn();
    const { container } = mount(<BreakTimelineLane issue={issue} changes={[change, { ...change, id: "config", kind: "ConfigMap", name: "cfg", uid: "cfg" }, { ...change, id: "scale", kind: "HorizontalPodAutoscaler", name: "checkout", uid: "hpa", at: "2026-10-14T12:01:00Z" }]} since="2026-10-14T11:30:00Z" until="2026-10-14T12:30:00Z" onSelectChange={select} />);
    expect(container.querySelectorAll("[data-resource]")).toHaveLength(3);
    const markers = [...container.querySelectorAll<HTMLElement>('[data-marker="change"]')];
    expect(markers.map((marker) => Number(marker.dataset.x))).toEqual([50, 50, 51.66666666666667]);
    const crashes = container.querySelectorAll('[data-marker="crash"]');
    expect(crashes).toHaveLength(1);
    expect(crashes[0].getAttribute("data-at")).toBe(issue.evidence!.terminations![0].last);
    fireEvent.click(screen.getByRole("button", { name: /Crash: OOMKilled exit 137 on 6 pods.*last occurrence.*at/ }));
    expect(screen.getByRole("region", { name: "Timeline event details" }).textContent).toContain("last occurrence");
    fireEvent.click(screen.getByRole("button", { name: /Deployment checkout: v41.*at/ }));
    fireEvent.click(screen.getByRole("button", { name: "Open resource" }));
    expect(select).toHaveBeenCalledWith(change);
  });

  it("does not claim zero broken or recovery from a successful warming empty response", async () => {
    vi.spyOn(api, "kubernetesIssues").mockImplementation(rootIssueResponses({ items: [], totals: {}, broken: { total: 0, by_severity: {} }, truncated: false, sync: { state: "syncing", age_s: 0, partial: false } }));
    mount(<TimelineCard windowEnd={Date.now()} clusterId="east" resourceFor={() => resource} onSelectView={vi.fn()} onSelectResource={vi.fn()} />);
    await screen.findByText("Break timeline unavailable.");
    expect(screen.queryByText("No broken workloads")).toBeNull();
    expect(screen.queryByText("0")).toBeNull();
  });

  it("reads retained Issues with their pinned window without using historical totals as current counts", async () => {
    window.history.replaceState(null, "", "/agent/kubernetes?view=issues&since=2020-01-01T00:00:00Z&until=2026-10-28T00:00:00Z");
    const lookup = vi.spyOn(api, "kubernetesIssues").mockImplementation(async (options) => ({
      items: options?.since ? [{ ...issue, state: "resolved", occurrence_id: "old-occurrence", closed_at: "2026-10-15T00:00:00Z" }] : [],
      totals: { critical: options?.since ? 90000 : 2 }, broken: { total: options?.since ? 90000 : 2, by_severity: {} },
      truncated: false, next: options?.since && !options.cursor ? "opaque-position" : undefined, sync: { state: "ready", age_s: 0, partial: false },
    }));
    vi.spyOn(api, "kubernetesIssueAlerts").mockResolvedValue({ enabled: false, level: "critical", available: false, can_write: false, reason: "read_only", links_ready: false, channels_enabled: false });
    mount(<ToastProvider><KubernetesExplorerTabContent tab="issues" onSelectResource={vi.fn()} /></ToastProvider>);
    expect(await screen.findByLabelText("critical count")).toHaveProperty("textContent", "2");
    expect(screen.queryByText("90000")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Next page" }));
    await waitFor(() => expect(lookup.mock.calls.some(([options]) => options?.cursor === "opaque-position" && options.since === "2020-01-01T00:00:00Z" && options.until === "2026-10-28T00:00:00Z")).toBe(true));
  });

  it("keeps zero broken workloads distinct from missing totals", async () => {
    vi.spyOn(api, "kubernetesIssues").mockImplementation(rootIssueResponses({ items: [], totals: {}, broken: { total: 0, by_severity: {} }, truncated: false, sync: { state: "ready", age_s: 0, partial: false } }));
    mount(<TimelineCard windowEnd={Date.now()} clusterId="east" resourceFor={() => resource} onSelectView={vi.fn()} onSelectResource={vi.fn()} />);
    expect(await screen.findByText("No broken workloads")).toBeTruthy();
    await waitFor(() => expect(screen.queryByRole("article")).toBeNull());
  });

  it.each([["invalid", "invalid"], [change.at, change.at]])("handles unavailable and zero-length chart windows without invalid coordinates", (since, until) => {
    const { container } = render(<ResourceTimelineChart changes={[change]} since={since} until={until} onSelectChange={vi.fn()} />);
    expect(screen.getByRole("status").textContent).toBe("Window unavailable.");
    expect(container.querySelector("[data-x]")).toBeNull();
  });

  it("excludes invalid and out-of-window markers and clips the suspect window", () => {
    const { container } = render(<ResourceTimelineChart issue={issue} changes={[change, { ...change, id: "bad", at: "invalid" }, { ...change, id: "old", at: "2026-10-14T10:00:00Z" }]} since="2026-10-14T12:00:00Z" until="2026-10-14T12:10:00Z" now={Date.parse("2026-10-14T12:20:00Z")} onSelectChange={vi.fn()} />);
    expect(container.querySelectorAll('[data-marker="change"]')).toHaveLength(1);
    expect(container.querySelector<HTMLElement>(".resource-timeline-window")?.style.left).toBe("0%");
    expect(container.querySelector<HTMLElement>(".resource-timeline-window")?.style.width).toBe("60%");
    expect(container.querySelector(".resource-timeline-now-line")).toBeNull();
  });

  it("keeps asynchronously loaded changes in the default list and opens only a selected root's chart", async () => {
    const snapshot = Date.parse("2026-10-14T12:06:00Z");
    vi.spyOn(Date, "now").mockReturnValue(snapshot);
    const sync = { state: "ready", age_s: 0, partial: false };
    vi.spyOn(api, "kubernetesIssues").mockImplementation(rootIssueResponses({ items: [issue], totals: {}, truncated: false, sync }));
    vi.spyOn(api, "kubernetesCrashes").mockResolvedValue(coveredJournalPage);
    const related = vi.spyOn(api, "kubernetesRelated").mockResolvedValue({ nodes: [], edges: [], omitted: {}, sync });
    const logs = vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValue(emptyLogs);
    vi.spyOn(api, "kubernetesChanges").mockResolvedValue({ items: [change], sync });
    vi.spyOn(api, "kubernetesTimeline").mockImplementation(async () => {
      await Promise.resolve();
      vi.mocked(Date.now).mockReturnValue(snapshot + 1_250);
      return timelinePage([
        { type: "break", id: "break-checkout", at: issue.broke_at!, issue },
        changeEntry(),
      ]);
    });
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    await screen.findByRole("button", { name: "Open change details" });
    expect(screen.queryByRole("region", { name: "Resource timeline chart" })).toBeNull();
    expect(screen.queryByRole("region", { name: "Captured crash evidence" })).toBeNull();
    expect(related).not.toHaveBeenCalled();
    expect(logs).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("link", { name: "Open break details" }));
    const chart = await screen.findByRole("region", { name: "Resource timeline chart" });
    expect(chart.getAttribute("data-since")).toBe("2026-10-14T11:34:00.000Z");
    expect(chart.getAttribute("data-until")).toBe("2026-10-14T12:34:00.000Z");
    expect(screen.getByRole("region", { name: "Captured crash evidence" })).toBeTruthy();
    await waitFor(() => expect(logs).toHaveBeenCalled());
    expect(related).not.toHaveBeenCalled();
  });

  it("renders now only at its real coordinate when inside the shared axis", () => {
    const { container } = render(<ResourceTimelineChart changes={[]} since="2026-10-14T12:00:00Z" until="2026-10-14T13:00:00Z" now={Date.parse("2026-10-14T12:15:00Z")} onSelectChange={vi.fn()} />);
    expect(container.querySelector<HTMLElement>(".resource-timeline-now-line")?.style.left).toBe("25%");
    expect(screen.getByText("No recorded markers in this window.")).toBeTruthy();
  });

  it("preserves focused markers, selected IDs and pan across moving windows, then resets explicit filters", () => {
    const props = { changes: [change], since: "2026-10-14T11:30:00Z", until: "2026-10-14T12:30:00Z", onSelectChange: vi.fn(), scope: "east:all" };
    const { rerender } = render(<ResourceTimelineChart {...props} />);
    const marker = screen.getByRole("button", { name: /Deployment checkout:.* at / });
    const plot = screen.getByRole("region", { name: "Resource timeline chart" });
    plot.scrollLeft = 140;
    marker.focus();
    rerender(<ResourceTimelineChart {...props} since="2026-10-14T11:30:30Z" until="2026-10-14T12:30:30Z" />);
    expect(document.activeElement).toBe(marker);
    expect(plot.scrollLeft).toBe(140);
    fireEvent.click(marker);
    const heading = screen.getByRole("heading", { name: "Timeline event details" });
    expect(document.activeElement).toBe(heading);
    rerender(<ResourceTimelineChart {...props} changes={[{ ...change, fields: [{ path: "image", from: "v42", to: "v43" }] }]} since="2026-10-14T11:31:00Z" until="2026-10-14T12:31:00Z" />);
    expect(screen.getByRole("region", { name: "Timeline event details" }).textContent).toContain("v43");
    expect(document.activeElement).toBe(heading);
    fireEvent.keyDown(heading, { key: "Escape" });
    expect(document.activeElement).toBe(marker);
    fireEvent.click(marker);
    fireEvent.click(screen.getByRole("button", { name: "Close event details" }));
    expect(document.activeElement).toBe(marker);
    fireEvent.click(marker);
    rerender(<ResourceTimelineChart {...props} changes={[]} />);
    expect(screen.queryByRole("region", { name: "Timeline event details" })).toBeNull();
    expect(document.activeElement).toBe(plot);
    rerender(<ResourceTimelineChart {...props} />);
    fireEvent.click(screen.getByRole("button", { name: /Deployment checkout:.* at / }));
    rerender(<ResourceTimelineChart {...props} scope="east:Deployment" />);
    expect(screen.queryByRole("region", { name: "Timeline event details" })).toBeNull();
    expect(document.activeElement).not.toBe(document.body);
  });

  it("retains list identity and focused change actions through the live thirty second poll", async () => {
    const snapshot = Date.parse("2026-10-14T12:06:00Z");
    vi.spyOn(Date, "now").mockReturnValue(snapshot);
    const reads = vi.spyOn(api, "kubernetesTimeline").mockResolvedValue(timelinePage([changeEntry()]));
    let poll: TimerHandler | undefined;
    const originalInterval = window.setInterval.bind(window);
    vi.spyOn(window, "setInterval").mockImplementation((handler, delay) => {
      if (String(handler).includes("setWindowEnd")) { poll = handler; return 42; }
      return originalInterval(handler, delay);
    });
    const select = vi.fn();
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={select} />);
    const marker = await screen.findByRole("button", { name: "Open change details" });
    const list = screen.getByRole("list", { name: "Breaks and changes" });
    marker.focus();
    vi.mocked(Date.now).mockReturnValue(snapshot + 30_000);
    await act(async () => { if (typeof poll === "function") poll(); });
    await waitFor(() => expect(reads).toHaveBeenCalledTimes(2));
    expect(screen.getByRole("list", { name: "Breaks and changes" })).toBe(list);
    expect(document.activeElement).toBe(marker);
    fireEvent.click(marker);
    expect(select).toHaveBeenCalledWith(expect.objectContaining({ resource_id: resource.resource_id, uid: "checkout-uid" }), "timeline");
    vi.mocked(Date.now).mockReturnValue(snapshot + 60_000);
    await act(async () => { if (typeof poll === "function") poll(); });
    await waitFor(() => expect(reads).toHaveBeenCalledTimes(3));
    expect(document.activeElement).toBe(marker);
    const kind = screen.getByRole("combobox", { name: "Timeline kind" });
    kind.focus();
    expect(document.activeElement).toBe(kind);
    fireEvent.change(kind, { target: { value: "Deployment" } });
    await waitFor(() => expect(reads).toHaveBeenLastCalledWith(expect.objectContaining({ kind: "Deployment" }), expect.any(AbortSignal)));
    expect(document.activeElement).not.toBe(document.body);
    expect(document.activeElement).toBe(kind);
  });

  it("retains namespace search focus without issuing per-keystroke reads and commits the selected option", async () => {
    const user = userEvent.setup();
    vi.spyOn(Date, "now").mockReturnValue(Date.parse("2026-10-14T12:06:00Z"));
    const reads = vi.spyOn(api, "kubernetesTimeline").mockImplementation(async (options) => timelinePage(options.namespace && options.namespace !== "shop" ? [] : [changeEntry()]));
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    await screen.findByRole("button", { name: "Open change details" });
    const namespace = screen.getByRole("combobox", { name: "Timeline namespace" });
    await user.click(namespace);
    for (const letter of "other") {
      await user.keyboard(letter);
      expect(document.activeElement).toBe(namespace);
    }
    expect(reads).toHaveBeenCalledOnce();
    expect(screen.getByRole("option", { name: "other" })).toBeTruthy();
    await user.keyboard("{Enter}");
    await waitFor(() => expect(reads).toHaveBeenLastCalledWith(expect.objectContaining({ namespace: "other" }), expect.any(AbortSignal)));
    expect((namespace as HTMLInputElement).value).toBe("other");
    expect(screen.queryByRole("button", { name: "Open change details" })).toBeNull();
    expect(screen.queryByRole("region", { name: "Timeline event details" })).toBeNull();
    expect(document.activeElement).toBe(namespace);
  });

  it("retains external control focus when a thirty second poll removes a change row", async () => {
    const snapshot = Date.parse("2026-10-14T12:06:00Z");
    vi.spyOn(Date, "now").mockReturnValue(snapshot);
    const reads = vi.spyOn(api, "kubernetesTimeline").mockResolvedValue(timelinePage([changeEntry()]));
    let poll: TimerHandler | undefined;
    const originalInterval = window.setInterval.bind(window);
    vi.spyOn(window, "setInterval").mockImplementation((handler, delay) => {
      if (String(handler).includes("setWindowEnd")) { poll = handler; return 42; }
      return originalInterval(handler, delay);
    });
    mount(<><button type="button">Outside timeline</button><KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} /></>);
    const list = screen.getByRole("list", { name: "Breaks and changes" });
    await screen.findByRole("button", { name: "Open change details" });
    const outside = screen.getByRole("button", { name: "Outside timeline" });
    outside.focus();
    reads.mockResolvedValue(timelinePage());
    expect(typeof poll).toBe("function");
    vi.mocked(Date.now).mockReturnValue(snapshot + 30_000);
    await act(async () => { if (typeof poll === "function") poll(); });
    await waitFor(() => expect(screen.queryByRole("button", { name: "Open change details" })).toBeNull());
    expect(screen.getByRole("list", { name: "Breaks and changes" })).toBe(list);
    expect(screen.queryByRole("region", { name: "Timeline event details" })).toBeNull();
    expect(reads).toHaveBeenCalledTimes(2);
    expect(document.activeElement).toBe(outside);
  });

  it("shares crash and rollback label space and fits three hit targets inside a compact row", () => {
    const events = [0, 1, 2].map((index) => ({ ...change, id: `stack-${index}` }));
    const rollback = { ...change, id: "rollback", at: "2026-10-14T12:06:00Z", fields: [{ path: "image", from: "v42", to: "v41" }] };
    const { container } = render(<ResourceTimelineChart issue={issue} changes={[...events, rollback]} since="2026-10-14T11:30:00Z" until="2026-10-14T12:30:00Z" onSelectChange={vi.fn()} />);
    expect([...container.querySelectorAll<HTMLElement>('[data-event-id^="stack-"]')].map((marker) => Number.parseFloat(marker.style.top))).toEqual([0, 9, 18]);
    const crashLabel = container.querySelector<HTMLElement>(".resource-timeline-crash-label")!;
    const rollbackLabel = screen.getByRole("button", { name: /Show event details: image v42/ });
    expect(crashLabel.style.maxWidth).toContain("0.83333");
    expect(rollbackLabel.style.maxWidth).toContain("0.83333");
    expect(crashLabel.title).toContain("on 6 pods (last occurrence)");
    expect(container.querySelector('[data-marker="crash"]')?.getAttribute("data-at")).toBe(issue.evidence!.terminations![0].last);
  });

  it("deduplicates IDs and retains a hundred crowded events in bounded details with long names", () => {
    const events = Array.from({ length: 100 }, (_, index) => ({ ...change, id: `crowded-${index}`, name: "long-resource-name-".repeat(30) }));
    const { container } = render(<ResourceTimelineChart changes={[...events, events[0]]} since="2026-10-14T11:30:00Z" until="2026-10-14T12:30:00Z" onSelectChange={vi.fn()} />);
    expect(container.querySelectorAll("[data-resource]")).toHaveLength(1);
    expect(container.querySelectorAll("[data-marker]")).toHaveLength(100);
    fireEvent.click(screen.getByRole("button", { name: /^100 events:/ }));
    expect(within(screen.getByRole("region", { name: "Timeline event details" })).getAllByRole("button", { name: "Open resource" })).toHaveLength(100);
    fireEvent.keyDown(screen.getByRole("region", { name: "Timeline event details" }), { key: "Escape" });
    expect(screen.queryByRole("region", { name: "Timeline event details" })).toBeNull();
  });

  it("uses a single unfocused chart with independent resource lanes and unique detail IDs", () => {
    const { container } = render(<><ResourceTimelineChart changes={[change]} since="2026-10-14T11:30:00Z" until="2026-10-14T12:30:00Z" onSelectChange={vi.fn()} /><ResourceTimelineChart changes={[change]} since="2026-10-14T11:30:00Z" until="2026-10-14T12:30:00Z" onSelectChange={vi.fn()} /></>);
    for (const marker of screen.getAllByRole("button", { name: /Deployment checkout:.* at / })) fireEvent.click(marker);
    expect(new Set([...container.querySelectorAll("[aria-controls]")].map((element) => element.getAttribute("aria-controls"))).size).toBe(2);
  });

  it("does not duplicate break or termination markers when issue rules share a root", () => {
    const { container } = render(<ResourceTimelineChart issues={[issue, { ...issue, rule: "workload.unavailable" }]} changes={[change]} since="2026-10-14T11:30:00Z" until="2026-10-14T12:30:00Z" onSelectChange={vi.fn()} />);
    expect(container.querySelectorAll("[data-resource]")).toHaveLength(1);
    expect(container.querySelectorAll('[data-marker="crash"]')).toHaveLength(1);
    expect(container.querySelectorAll(".resource-timeline-break-line")).toHaveLength(1);
  });

  it("does not interpret a stale zero as confirmed recovery", async () => {
    vi.spyOn(api, "kubernetesIssues").mockImplementation(rootIssueResponses({ items: [], totals: {}, broken: { total: 0, by_severity: {} }, truncated: false, sync: { state: "stale", age_s: 90, partial: false } }));
    mount(<TimelineCard windowEnd={Date.now()} clusterId="east" resourceFor={() => resource} onSelectView={vi.fn()} onSelectResource={vi.fn()} />);
    expect(await screen.findByRole("button", { name: /^Broken Unknown/ })).toBeTruthy();
    expect(screen.queryByText(/No broken workloads/)).toBeNull();
  });

  it("does not interpret missing totals as zero", async () => {
    vi.spyOn(api, "kubernetesIssues").mockImplementation(rootIssueResponses({ items: [], totals: {}, truncated: false, sync: { state: "ready", age_s: 0, partial: false } }));
    mount(<TimelineCard windowEnd={Date.now()} clusterId="east" resourceFor={() => resource} onSelectView={vi.fn()} onSelectResource={vi.fn()} />);
    await waitFor(() => expect(screen.getByRole("button", { name: /Broken Unknown/ })).toBeTruthy());
    expect(screen.queryByText("No broken workloads")).toBeNull();
  });

  it("deduplicates aggregate failures and discloses stale changes while retaining the capped count and broken evidence", async () => {
    const failures = [{ resource_id: "ConfigMap", class: "forbidden" }, { resource_id: "ConfigMap", class: "forbidden" }];
    const sync = { state: "ready", age_s: 4, partial: true };
    vi.spyOn(api, "kubernetesIssues").mockImplementation(rootIssueResponses({ items: [issue], totals: {}, broken: { total: 1, by_severity: { critical: 1 } }, truncated: false, sync, partial_failures: failures }));
    mount(<TimelineCard changes={{ items: [], sync: { ...sync, state: "stale" }, truncated: true, partial_failures: failures }} windowEnd={Date.now()} clusterId="east" resourceFor={() => resource} onSelectView={vi.fn()} onSelectResource={vi.fn()} />);
    const toggle = await screen.findByRole("button", { name: "Resource coverage" });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    expect(screen.getByText("CrashLoopBackOff")).toBeTruthy();
    expect(screen.getByText("OOMKilled on 6 pods")).toBeTruthy();
    fireEvent.click(toggle);
    expect(screen.getAllByText("ConfigMap: permission denied")).toHaveLength(1);
    expect(screen.getByText("Change activity is stale.")).toBeTruthy();
    expect(screen.getByRole("button", { name: /Changes in 60 min/ }).textContent).toContain("0+");
    expect(screen.queryByText("Break evidence is partial.")).toBeNull();
  });
});