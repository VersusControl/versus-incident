// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, useLocation } from "react-router-dom";
import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api, ApiError, type KubernetesBreakLogPage, type KubernetesChangesPage, type KubernetesCrashEvent, type KubernetesCrashPage, type KubernetesGraph, type KubernetesIssue } from "@/lib/api";
import { KubernetesClusterContext, kubernetesKey, useKubernetesCrashes } from "@/lib/useKubernetesCluster";
import { BreakRow, BreakTimelineLane, CrashJournal, ResourceTimelineChart, TimelineCard } from "./KubernetesBreakTimeline";
import { KubernetesExplorerTabContent } from "./KubernetesExplorerViews";
import { KubernetesWarnings } from "./KubernetesWarnings";
import { KubernetesBreakLogs, KubernetesBreakLogsProvider } from "./KubernetesBreakLogs";
import { breakLogWindow } from "@/lib/useKubernetesBreakLogs";

const issue: KubernetesIssue = { root: { kind: "Deployment", namespace: "shop", name: "checkout", uid: "checkout-uid" }, rule: "pod.crash_loop", severity: "critical", count: 7, of: 7, broken: true, broke_at: "2026-10-14T12:04:00Z", history: "complete", evidence: { terminations: [{ reason: "OOMKilled", exit_code: 137, count: 6, last: "2026-10-14T12:05:00Z" }], warning_events: 3, from: "2026-10-14T11:34:00Z", to: "2026-10-14T12:06:00Z" } };
const resource = { resource_id: "apps~v1~deployments", ...issue.root };
const change = { id: "image", cluster: "east", kind: "Deployment", namespace: "shop", name: "checkout", uid: "checkout-uid", type: "image_changed" as const, fields: [{ path: "image", from: "v41", to: "v42" }], at: "2026-10-14T12:00:00Z", source: "reconstructed" as const };
function Location() { const location = useLocation(); return <output aria-label="Location">{location.pathname}{location.search}</output>; }
function mount(element: React.ReactNode, cluster = "east") {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return { client, ...render(<QueryClientProvider client={client}><KubernetesClusterContext.Provider value={{ client: api, cluster, multiple: true, clusters: [], key: (...parts) => kubernetesKey(cluster, ...parts) }}><MemoryRouter initialEntries={[`${window.location.pathname}${window.location.search}`]}>{element}<Location /></MemoryRouter></KubernetesClusterContext.Provider></QueryClientProvider>) };
}
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.useRealTimers(); window.history.replaceState(null, "", "/"); });

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

  it("keeps complete crash coverage neutral with the backend baseline polling limitations", async () => {
    vi.spyOn(api, "kubernetesCrashes").mockResolvedValue(coveredJournalPage);
    mount(<JournalScenario />);
    const journal = screen.getByRole("region", { name: "Captured crash evidence" });
    await within(journal).findByText("1 observations · Polling may miss crashes between checks.");
    const summary = journal.querySelector("header > span")!;
    expect(summary.className).not.toContain("text-sev-warning");
    expect(summary.textContent).not.toContain("Partial history");
    const disclosure = within(journal).getByText("Capture details").closest("details")!;
    expect(disclosure.open).toBe(false);
    for (const caveat of baselineLimitations) expect(disclosure.textContent).toContain(caveat.replaceAll("_", " "));
  });

  it.each(["journal_not_started_for_full_window", "outside_retention", "journal_commit_failed"])("keeps the window-specific crash limitation %s visible beside the heading", async (limitation) => {
    vi.spyOn(api, "kubernetesCrashes").mockResolvedValue({ ...coveredJournalPage, limitations: [...baselineLimitations, limitation] });
    mount(<JournalScenario />);
    const journal = screen.getByRole("region", { name: "Captured crash evidence" });
    await within(journal).findByText("1 observations · Partial history; crashes may be missing.");
    expect(journal.querySelector("header > span")?.className).toContain("text-sev-warning");
    expect(journal.querySelector("header")?.textContent).not.toContain("Polling may miss crashes between checks.");
    expect(within(journal).getByText("Capture details").closest("details")?.open).toBe(false);
  });

  it.each([
    { ...observation, time_source: "observed_at" as const },
    { ...observation, ancestry_partial: true },
  ])("keeps actual ambiguous crash evidence visible despite otherwise complete coverage ($time_source, ancestry=$ancestry_partial)", async (event) => {
    vi.spyOn(api, "kubernetesCrashes").mockResolvedValue({ ...coveredJournalPage, items: [event] });
    mount(<JournalScenario />);
    const journal = screen.getByRole("region", { name: "Captured crash evidence" });
    await within(journal).findByText("1 observations · Partial history; crashes may be missing.");
    expect(journal.querySelector("header > span")?.className).toContain("text-sev-warning");
  });

  it("keeps uncertainty beside the heading and technical coverage in one closed disclosure", async () => {
    vi.spyOn(api, "kubernetesCrashes").mockResolvedValue({ ...journalPage, items: [], started_at: "0001-01-01T00:00:00Z", retention_from: "0001-01-01T00:00:00Z" });
    mount(<JournalScenario />);
    const journal = screen.getByRole("region", { name: "Captured crash evidence" });
    await within(journal).findByText("No captured observations on this page.");
    const header = journal.querySelector("header")!;
    expect(header.textContent).toContain("0 observations · Partial history; crashes may be missing.");
    const disclosure = within(journal).getByText("Capture details").closest("details")!;
    expect(disclosure.open).toBe(false);
    expect(disclosure.querySelector("details")).toBeNull();
    expect(disclosure.textContent).toContain("Capture gap:");
    expect(journal.textContent).not.toContain("0001-");
    expect(within(journal).getAllByRole("status")).toHaveLength(1);
    fireEvent.click(within(journal).getByText("Capture details"));
    expect(disclosure.open).toBe(true);
  });

  it("pages all journal observations without accumulating rendered rows and resets filters", async () => {
    const lookup = vi.spyOn(api, "kubernetesCrashes").mockImplementation(async (options) => ({ ...journalPage, items: options?.cursor ? [{ ...observation, id: options.cursor, pod: options.cursor === "final-page" ? "final-pod" : "last-pod", init: true, signal: 9 }] : Array.from({ length: 50 }, (_, index) => ({ ...observation, id: `event-${index}`, pod: `old-pod-${index}` })), next: options?.cursor === "final-page" ? undefined : options?.cursor ? "final-page" : "next-page" }));
    const { container } = mount(<JournalScenario />);
    const journal = screen.getByRole("region", { name: "Captured crash evidence" });
    await waitFor(() => expect(within(within(journal).getByRole("list", { name: "Crash observations" })).getAllByRole("listitem")).toHaveLength(50));
    expect(lookup).toHaveBeenCalledTimes(1);
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

  it("keeps safe status observations, actual signals, time provenance and explicit capture gaps", async () => {
    vi.spyOn(api, "kubernetesCrashes").mockResolvedValue({ ...journalPage, items: [observation, { ...observation, id: "status", type: "status", waiting_reason: "CrashLoopBackOff", time_source: "observed_at", ancestry_partial: true }] });
    const { container } = mount(<JournalScenario />);
    await screen.findByText("CrashLoopBackOff", { exact: true });
    const journal = screen.getByRole("region", { name: "Captured crash evidence" });
    expect(container.querySelectorAll('[data-marker="crash"]')).toHaveLength(1);
    expect(journal.textContent).toContain("not proof of SIGKILL");
    expect(journal.textContent).not.toContain(", signal 9");
    expect(journal.textContent).toContain("observation time; finish time unknown");
    expect(journal.textContent).toContain("Capture-time ancestry is incomplete");
    expect(journal.textContent).toContain("Journal retention/capture gap:");
    expect(journal.textContent).toContain("Capture gap:");
    expect(journal.textContent).toContain("Pod capture coverage unavailable");
    expect(journal.textContent).toContain("retention basis: observed_at");
    expect(journal.textContent).toContain("Events between polls may be unseen");
  });

  it("keeps newly observed terminations with finishes 48 hours earlier visible without implying a chart marker", async () => {
    const oldTermination = { ...observation, id: "old-finish", pod: "old-finish-pod", at: "2026-10-12T12:06:00Z" };
    vi.spyOn(api, "kubernetesCrashes").mockResolvedValue({ ...journalPage, items: [oldTermination, observation] });
    const { container } = mount(<JournalScenario />);
    const journal = screen.getByRole("region", { name: "Captured crash evidence" });
    await waitFor(() => expect(within(within(journal).getByRole("list", { name: "Crash observations" })).getAllByRole("listitem")).toHaveLength(2));
    expect(within(journal).getByText(/^Observed window:/)).toBeTruthy();
    const summary = within(journal).getByText("Reported finish outside observed window").closest("summary")!;
    expect(summary.textContent).toContain("shop/old-finish-pod");
    expect(summary.textContent).toContain("Observed:");
    expect(summary.querySelector("time")?.dateTime).toBe(oldTermination.observed_at);
    expect(container.querySelectorAll('[data-marker="crash"]')).toHaveLength(1);
    expect(container.querySelector('[data-event-id="old-finish"]')).toBeNull();
    expect(container.querySelector('[data-event-id="journal-event"]')?.getAttribute("data-at")).toBe(observation.at);
    expect(journal.textContent).toContain("Crash markers show only this page's terminations with event times inside the chart window.");
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

  it.each([true, false])("shows retained journal evidence without current issue or change data, root focus=%s", async (focused) => {
    window.history.replaceState(null, "", `/agent/kubernetes?view=timeline&center=${issue.broke_at}${focused ? "&root_uid=checkout-uid&namespace=shop" : ""}`);
    vi.spyOn(api, "kubernetesCrashes").mockResolvedValue(journalPage);
    vi.spyOn(api, "kubernetesIssues").mockResolvedValue({ items: [], totals: {}, truncated: false, sync: journalPage.sync });
    vi.spyOn(api, "kubernetesChanges").mockRejectedValue(new Error("Unavailable"));
    const graph = vi.spyOn(api, "kubernetesGraph");
    const { container } = mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    await waitFor(() => expect(screen.getByRole("region", { name: "Captured crash evidence" }).textContent).toContain("checkout-old"));
    expect(container.querySelectorAll('[data-marker="crash"]')).toHaveLength(1);
    expect(graph).not.toHaveBeenCalled();
  });

  it("clamps centered journal requests to now without changing the change-chart window", async () => {
    const now = Date.parse("2026-10-16T12:00:00Z");
    vi.spyOn(Date, "now").mockReturnValue(now);
    const center = "2026-10-16T11:55:00Z";
    window.history.replaceState(null, "", `/agent/kubernetes?view=timeline&root_uid=checkout-uid&namespace=shop&center=${center}`);
    const lookup = vi.spyOn(api, "kubernetesCrashes").mockResolvedValue({ ...journalPage, items: [] });
    vi.spyOn(api, "kubernetesIssues").mockResolvedValue({ items: [{ ...issue, broke_at: center }], totals: {}, truncated: false, sync: journalPage.sync });
    vi.spyOn(api, "kubernetesGraph").mockResolvedValue({ nodes: [], edges: [], omitted: {}, sync: journalPage.sync });
    const changes = vi.spyOn(api, "kubernetesChanges").mockResolvedValue({ items: [], sync: journalPage.sync });
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    await waitFor(() => expect(changes).toHaveBeenCalled());
    expect(lookup.mock.calls[0][0]?.until).toBe(new Date(now).toISOString());
    expect(changes.mock.calls[0][0]?.until).toBe("2026-10-16T12:25:00.000Z");
    expect(screen.getByRole("region", { name: "Resource timeline chart" }).getAttribute("data-until")).toBe("2026-10-16T12:25:00.000Z");
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
    const lookup = vi.spyOn(api, "kubernetesIssues").mockResolvedValue({ items: [enriched], totals: {}, truncated: false, sync: { state: "ready", age_s: 0, partial: false } });
    const { container } = mount(<BreakRow issue={enriched} resource={resource} onSelectResource={vi.fn()} collapsed={collapsed} />);
    const toggle = screen.getByRole("button", { name: "+2 more evidence for checkout" });
    const extra = document.getElementById(toggle.getAttribute("aria-controls")!)!;
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    expect(extra.className).toBe("hidden sm:contents");
    expect(screen.getByText("OOMKilled on 6 pods")).toBeTruthy();
    expect(screen.getByText("Init CreateContainerConfigError on 1 pod")).toBeTruthy();
    expect(screen.getByText("Evicted on 2 pods")).toBeTruthy();
    expect(screen.getByText("Partial crash evidence")).toBeTruthy();
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
    const toggle = screen.getByRole("button", { name: "Warnings (2)" });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    expect(document.getElementById(toggle.getAttribute("aria-controls")!)?.hidden).toBe(true);
    expect(screen.getByText("CrashLoopBackOff 7 of 7")).toBeTruthy();
    fireEvent.click(toggle);
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    expect(screen.getByRole("list").textContent).toContain("History incomplete.");
    rerender(<KubernetesWarnings warnings={warnings} scope="west:checkout" />);
    expect(screen.getByRole("button").getAttribute("aria-expanded")).toBe("false");
    rerender(<KubernetesWarnings warnings={warnings} scope="east:checkout" />);
    expect(screen.getByRole("button").getAttribute("aria-expanded")).toBe("false");
    rerender(<KubernetesWarnings warnings={warnings} scope="west:checkout" />);
    fireEvent.click(screen.getByRole("button"));
    screen.getByRole("button").focus();
    const focused = document.activeElement;
    rerender(<KubernetesWarnings warnings={[...warnings, { id: "stale", content: "Snapshot stale." }]} scope="west:checkout" />);
    expect(screen.getByRole("button", { name: "Warnings (3)" }).getAttribute("aria-expanded")).toBe("true");
    expect(document.activeElement).toBe(focused);
  });

  it("reveals all graph-linked changes through more navigation past unrelated newest pages", async () => {
    const linked = Array.from({ length: 24 }, (_, index) => ({ ...change, id: `linked-${index}`, kind: "ConfigMap", name: `settings-${index}`, uid: `config-${index}`, type: "config_changed" as const }));
    const detail = { ...issue, changes: linked.slice(0, 3).map((item) => ({ change: item, relation: "used_by", lead_seconds: 240 })), more_changes: 21 };
    const sync = { state: "ready", age_s: 0, partial: false };
    vi.spyOn(api, "kubernetesIssues").mockResolvedValue({ items: [detail], totals: {}, truncated: false, sync });
    vi.spyOn(api, "kubernetesGraph").mockResolvedValue({ nodes: [{ id: issue.root.uid!, kind: issue.root.kind, namespace: "shop", name: issue.root.name, group: "shop" }, ...linked.map((item) => ({ id: item.uid, kind: item.kind, namespace: item.namespace, name: item.name, group: "shop" }))], edges: linked.map((item) => ({ from: item.uid, to: issue.root.uid!, type: "configures" as const })), omitted: {}, sync });
    const lookup = vi.spyOn(api, "kubernetesChanges").mockImplementation(async (options) => {
      const cursor = options?.cursor;
      if (!cursor || cursor === "unrelated") return { items: Array.from({ length: 20 }, (_, index) => ({ ...change, id: `${cursor}-${index}`, uid: `unrelated-${index}`, name: `unrelated-${index}` })), next: cursor ? "linked" : "unrelated", sync, gaps: !cursor ? [{ from: "2026-10-14T11:50:00Z", to: "2026-10-14T11:51:00Z" }] : [] };
      return { items: cursor === "linked" ? linked.slice(0, 20) : linked.slice(20), next: cursor === "linked" ? "last" : undefined, sync };
    });
    function Scenario() {
      const location = useLocation();
      return new URLSearchParams(location.search).get("view") === "timeline" ? <KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} /> : <BreakRow issue={detail} resource={resource} onSelectResource={vi.fn()} />;
    }
    mount(<Scenario />);
    fireEvent.click(screen.getAllByRole("button", { name: /more changes/ })[0]);
    expect(await screen.findByRole("button", { name: /ConfigMap settings-19:.* at / })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /unrelated-0:/ })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: /Warnings/ }));
    expect(screen.getByRole("region", { name: "Timeline history gaps" })).toBeTruthy();
    expect(api.kubernetesGraph).toHaveBeenCalledWith({ namespace: "shop", complete: true });
    expect(lookup).toHaveBeenCalledTimes(3);
    expect(lookup.mock.calls.every(([options]) => options?.namespace === "shop" && options.limit === 200)).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "Next page" }));
    expect(await screen.findByRole("button", { name: /ConfigMap settings-23:.* at / })).toBeTruthy();
    expect(screen.getByRole("button", { name: /Warnings/ }).getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(screen.getByRole("button", { name: /Warnings/ }));
    expect(screen.getByRole("region", { name: "Timeline history gaps" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /ConfigMap settings-0:.* at / })).toBeNull();
    expect(screen.getByRole("button", { name: "Next page" }).hasAttribute("disabled")).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "Previous page" }));
    expect(await screen.findByRole("button", { name: /ConfigMap settings-0:.* at / })).toBeTruthy();
    const query = new URLSearchParams(screen.getByLabelText("Location").textContent!.split("?")[1]);
    expect(query.get("root_uid")).toBe(issue.root.uid);
    expect(query.get("namespace")).toBe("shop");
    expect(query.get("center")).toBe(issue.broke_at);
  });

  it.each([false, true])("bounds sparse linked history and continues past 1,000 unrelated records with early match=%s", async (earlyMatch) => {
    window.history.replaceState(null, "", `/agent/kubernetes?view=timeline&root_uid=checkout-uid&namespace=shop&center=${issue.broke_at}`);
    const sync = { state: "ready", age_s: 0, partial: false };
    vi.spyOn(api, "kubernetesIssues").mockResolvedValue({ items: [issue], totals: {}, truncated: false, sync });
    vi.spyOn(api, "kubernetesGraph").mockResolvedValue({ nodes: [{ id: "checkout-uid", kind: "Deployment", namespace: "shop", name: "checkout", group: "shop" }], edges: [], omitted: {}, sync });
    const history = Array.from({ length: 1_220 }, (_, index) => ({ ...change, id: `record-${index}`, uid: earlyMatch && index === 450 || index >= 1_200 ? "checkout-uid" : `unrelated-${index}`, name: earlyMatch && index === 450 || index >= 1_200 ? `linked-${index}` : `unrelated-${index}` }));
    const lookup = vi.spyOn(api, "kubernetesChanges").mockImplementation(async (options) => {
      const start = Number(options?.cursor ?? 0);
      const end = Math.min(start + options!.limit!, history.length);
      return { items: history.slice(start, end), next: end < history.length ? String(end) : undefined, sync };
    });
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    expect(await screen.findByText(/Related change scan is incomplete after 5 pages/)).toBeTruthy();
    if (earlyMatch) expect(screen.getByRole("button", { name: /Deployment linked-450:.* at / })).toBeTruthy();
    expect(lookup).toHaveBeenCalledTimes(5);
    expect(lookup.mock.calls.map(([options]) => options?.cursor)).toEqual([undefined, "200", "400", "600", "800"]);
    expect(lookup.mock.calls.every(([options]) => options?.limit === 200)).toBe(true);
    expect(screen.getByText(/Related change scan is incomplete after 5 pages/)).toBeTruthy();
    expect(screen.getByRole("button", { name: /Warnings/ }).getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByText("Related change history is unavailable.")).toBeNull();
    expect(screen.queryByRole("button", { name: /unrelated-0:/ })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Next page" }));
    fireEvent.click(await screen.findByRole("button", { name: /^20 events:/ }));
    expect(within(screen.getByRole("region", { name: "Timeline event details" })).getByText(/Deployment linked-1219:/)).toBeTruthy();
    expect(lookup).toHaveBeenCalledTimes(7);
    expect(lookup.mock.calls.slice(5).map(([options]) => options?.cursor)).toEqual(["1000", "1200"]);
    expect(screen.getByRole("button", { name: "Next page" }).hasAttribute("disabled")).toBe(true);
    expect(screen.queryByText(/Related change scan is incomplete after 5 pages/)).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Previous page" }));
    expect(await screen.findByText(/Related change scan is incomplete after 5 pages/)).toBeTruthy();
    if (earlyMatch) expect(screen.getByRole("button", { name: /Deployment linked-450:.* at / })).toBeTruthy();
  });

  it("isolates namespace and cluster identities and resets pagination when kind changes", async () => {
    window.history.replaceState(null, "", `/agent/kubernetes?view=timeline&root_uid=checkout-uid&namespace=shop&center=${issue.broke_at}`);
    const sync = { state: "ready", age_s: 0, partial: false };
    vi.spyOn(api, "kubernetesIssues").mockImplementation(async (options) => ({ items: options?.namespace === "other" ? [] : [issue], totals: {}, truncated: false, sync }));
    vi.spyOn(api, "kubernetesGraph").mockResolvedValue({ nodes: [{ id: "checkout-uid", kind: "Deployment", namespace: "shop", name: "checkout", group: "shop" }, { id: "pod", kind: "Pod", namespace: "shop", name: "child", group: "shop" }, { id: "other", kind: "ConfigMap", namespace: "other", name: "private", group: "other" }], edges: [{ from: "checkout-uid", to: "pod", type: "manages" }, { from: "other", to: "checkout-uid", type: "configures" }], omitted: {}, sync });
    const lookup = vi.spyOn(api, "kubernetesChanges").mockResolvedValue({ items: [{ ...change, id: "child", uid: "pod", kind: "Pod", name: "child" }, { ...change, id: "other-ns", uid: "other", namespace: "other", name: "private" }, { ...change, id: "other-cluster", cluster: "west", name: "private-cluster" }], sync });
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    expect(await screen.findByRole("button", { name: /Pod child:.* at / })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /private/ })).toBeNull();
    fireEvent.change(screen.getByRole("combobox", { name: "Timeline kind" }), { target: { value: "Deployment" } });
    await waitFor(() => expect(lookup).toHaveBeenLastCalledWith(expect.objectContaining({ kind: "Deployment", cursor: undefined })));
    expect(screen.queryByRole("button", { name: /Pod child:.* at / })).toBeNull();
    fireEvent.change(screen.getByRole("textbox", { name: "Timeline namespace" }), { target: { value: "other" } });
    expect(await screen.findByText("Root not found in this namespace.")).toBeTruthy();
    expect(screen.queryByRole("region", { name: "Timeline lane checkout" })).toBeNull();
  });

  it("keeps namespace and kind controls functional in the unfocused shared chart", async () => {
    window.history.replaceState(null, "", "/agent/kubernetes?view=timeline&namespace=shop");
    const sync = { state: "ready", age_s: 0, partial: false };
    vi.spyOn(api, "kubernetesIssues").mockResolvedValue({ items: [], totals: {}, truncated: false, sync });
    const lookup = vi.spyOn(api, "kubernetesChanges").mockResolvedValue({ items: [change], sync });
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    await screen.findByRole("region", { name: "Resource timeline chart" });
    fireEvent.change(screen.getByRole("combobox", { name: "Timeline kind" }), { target: { value: "Deployment" } });
    await waitFor(() => expect(lookup).toHaveBeenLastCalledWith(expect.objectContaining({ namespace: "shop", kind: "Deployment" })));
    expect((screen.getByRole("combobox", { name: "Timeline kind" }) as HTMLSelectElement).value).toBe("Deployment");
  });

  it("preserves current namespace, kind and cluster when clearing only the root filter", async () => {
    window.history.replaceState(null, "", `/agent/kubernetes?view=timeline&root_uid=checkout-uid&namespace=shop&cluster=east&center=${issue.broke_at}`);
    const sync = { state: "ready", age_s: 0, partial: false };
    vi.spyOn(api, "kubernetesIssues").mockResolvedValue({ items: [issue], totals: {}, truncated: false, sync });
    vi.spyOn(api, "kubernetesGraph").mockResolvedValue({ nodes: [], edges: [], omitted: {}, sync });
    vi.spyOn(api, "kubernetesChanges").mockResolvedValue({ items: [change], sync });
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    await screen.findByRole("region", { name: "Resource timeline chart" });
    fireEvent.change(screen.getByRole("combobox", { name: "Timeline kind" }), { target: { value: "Deployment" } });
    fireEvent.change(screen.getByRole("textbox", { name: "Timeline namespace" }), { target: { value: "other" } });
    const query = new URLSearchParams(screen.getByRole("link", { name: "Clear root filter" }).getAttribute("href")!.split("?")[1]);
    expect(Object.fromEntries(query)).toEqual({ view: "timeline", cluster: "east", namespace: "other", kind: "Deployment" });
  });

  it("discloses graph failures and partial/gap evidence instead of claiming a complete view", async () => {
    window.history.replaceState(null, "", `/agent/kubernetes?view=timeline&root_uid=checkout-uid&namespace=shop&center=${issue.broke_at}`);
    const sync = { state: "ready", age_s: 0, partial: true };
    vi.spyOn(api, "kubernetesIssues").mockResolvedValue({ items: [{ ...issue, changes: [{ change, relation: "same_object", lead_seconds: 240 }] }], totals: {}, truncated: false, sync });
    vi.spyOn(api, "kubernetesGraph").mockRejectedValue(new ApiError(403, "Forbidden"));
    vi.spyOn(api, "kubernetesChanges").mockResolvedValue({ items: [change], truncated: true, sync, gaps: [{ from: change.at, to: issue.broke_at! }], partial_failures: [{ resource_id: "ConfigMap", class: "forbidden" }] });
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    expect(await screen.findByRole("button", { name: /Deployment checkout:.* at / })).toBeTruthy();
    expect(screen.getByText(/Related-resource graph unavailable/)).toBeTruthy();
    expect(screen.getByRole("button", { name: /Warnings/ }).getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(screen.getByRole("button", { name: /Warnings/ }));
    expect(screen.getByText("ConfigMap: forbidden")).toBeTruthy();
    expect(screen.getByRole("region", { name: "Timeline history gaps" })).toBeTruthy();
  });

  it("stops repeated change cursors with an explicit failure", async () => {
    window.history.replaceState(null, "", `/agent/kubernetes?view=timeline&root_uid=checkout-uid&namespace=shop&center=${issue.broke_at}`);
    const sync = { state: "ready", age_s: 0, partial: false };
    vi.spyOn(api, "kubernetesIssues").mockResolvedValue({ items: [issue], totals: {}, truncated: false, sync });
    vi.spyOn(api, "kubernetesGraph").mockResolvedValue({ nodes: [], edges: [], omitted: {}, sync });
    const lookup = vi.spyOn(api, "kubernetesChanges").mockResolvedValue({ items: [], next: "repeat", sync });
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    expect(await screen.findByText("Related change history is unavailable.")).toBeTruthy();
    expect(lookup).toHaveBeenCalledTimes(2);
    expect(screen.queryByText("No recorded markers in this window.")).toBeNull();
  });

  it("caps cluster-wide root graph pagination and explicitly discloses omitted pages", async () => {
    window.history.replaceState(null, "", `/agent/kubernetes?view=timeline&root_uid=checkout-uid&center=${issue.broke_at}`);
    const sync = { state: "ready", age_s: 0, partial: false };
    vi.spyOn(api, "kubernetesIssues").mockResolvedValue({ items: [{ ...issue, root: { ...issue.root, namespace: undefined } }], totals: {}, truncated: false, sync });
    const graph = vi.spyOn(api, "kubernetesGraph").mockImplementation(async (options) => ({ nodes: [{ id: "checkout-uid", kind: "Deployment", name: "checkout", group: "cluster" }], edges: [], omitted: {}, next: String(Number(options?.cursor ?? 0) + 1), sync }));
    vi.spyOn(api, "kubernetesChanges").mockResolvedValue({ items: [change], sync });
    const { client } = mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    expect(await screen.findByText(/additional graph pages were omitted/)).toBeTruthy();
    expect(await screen.findByRole("button", { name: /Deployment checkout:.* at / })).toBeTruthy();
    expect(graph).toHaveBeenCalledTimes(5);
    expect(graph.mock.calls.map(([options]) => options?.cursor)).toEqual([undefined, "1", "2", "3", "4"]);
    expect(client.getQueryData(kubernetesKey("east", "kubernetes-graph", "timeline-root", "checkout-uid", undefined))).toMatchObject({ next: "5", truncated: true, omitted: { continuations: 1 } });
    expect(screen.getByRole("button", { name: /Warnings/ }).getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByText("Related change history is unavailable.")).toBeNull();
  });

  it("stops repeated cluster-wide graph cursors and discloses incomplete links", async () => {
    window.history.replaceState(null, "", `/agent/kubernetes?view=timeline&root_uid=checkout-uid&center=${issue.broke_at}`);
    const sync = { state: "ready", age_s: 0, partial: false };
    vi.spyOn(api, "kubernetesIssues").mockResolvedValue({ items: [{ ...issue, root: { ...issue.root, namespace: undefined } }], totals: {}, truncated: false, sync });
    const graph = vi.spyOn(api, "kubernetesGraph").mockResolvedValue({ nodes: [], edges: [], omitted: {}, next: "repeat", sync });
    vi.spyOn(api, "kubernetesChanges").mockResolvedValue({ items: [change], sync });
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    expect(await screen.findByRole("button", { name: /Deployment checkout:.* at / })).toBeTruthy();
    expect(graph).toHaveBeenCalledTimes(2);
    expect(screen.getByText(/Related-resource graph unavailable/)).toBeTruthy();
    expect(screen.getByRole("button", { name: /Warnings/ }).getAttribute("aria-expanded")).toBe("false");
  });

  it.each(["changes", "graph"] as const)("does not continue or publish an in-flight %s page after unmount cancellation", async (stage) => {
    window.history.replaceState(null, "", `/agent/kubernetes?view=timeline&root_uid=checkout-uid&center=${issue.broke_at}`);
    const sync = { state: "ready", age_s: 0, partial: false };
    vi.spyOn(api, "kubernetesIssues").mockResolvedValue({ items: [{ ...issue, root: { ...issue.root, namespace: undefined } }], totals: {}, truncated: false, sync });
    let finishChanges!: (page: KubernetesChangesPage) => void;
    let finishGraph!: (page: KubernetesGraph) => void;
    const pendingChanges = new Promise<KubernetesChangesPage>((resolve) => { finishChanges = resolve; });
    const pendingGraph = new Promise<KubernetesGraph>((resolve) => { finishGraph = resolve; });
    const graph = vi.spyOn(api, "kubernetesGraph").mockImplementation(() => stage === "graph" ? pendingGraph : Promise.resolve({ nodes: [], edges: [], omitted: {}, sync }));
    const changes = vi.spyOn(api, "kubernetesChanges").mockImplementation(() => pendingChanges);
    const { client, unmount } = mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    const lookup = stage === "graph" ? graph : changes;
    await waitFor(() => expect(lookup).toHaveBeenCalledOnce());
    const query = client.getQueryCache().find({ queryKey: kubernetesKey("east", stage === "graph" ? "kubernetes-graph" : "kubernetes-changes", "timeline-root"), exact: false })!;
    unmount();
    await act(async () => {
      if (stage === "graph") finishGraph({ nodes: [], edges: [], omitted: {}, next: "next", sync });
      else finishChanges({ items: [change], next: "next", sync });
      await (stage === "graph" ? pendingGraph : pendingChanges);
    });
    expect(lookup).toHaveBeenCalledOnce();
    expect(query.state.data).toBeUndefined();
    expect(query.state.fetchStatus).toBe("idle");
    expect(query.state.error).toBeNull();
  });

  it("discloses advertised links that cannot be recovered from current graph history", async () => {
    window.history.replaceState(null, "", `/agent/kubernetes?view=timeline&root_uid=checkout-uid&namespace=shop&center=${issue.broke_at}`);
    const sync = { state: "ready", age_s: 0, partial: false };
    vi.spyOn(api, "kubernetesIssues").mockResolvedValue({ items: [{ ...issue, more_changes: 4 }], totals: {}, truncated: false, sync });
    vi.spyOn(api, "kubernetesGraph").mockResolvedValue({ nodes: [{ id: "checkout-uid", kind: "Deployment", namespace: "shop", name: "checkout", group: "shop" }], edges: [], omitted: {}, sync });
    vi.spyOn(api, "kubernetesChanges").mockResolvedValue({ items: [], sync });
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    expect(await screen.findByText("Break: CrashLoopBackOff")).toBeTruthy();
    expect(await screen.findByText(/Related-resource evidence is incomplete/)).toBeTruthy();
  });

  it.each(["gap", "reconstructed", undefined] as const)("never claims no changes for %s history", (history) => {
    mount(<BreakRow issue={{ ...issue, history, rebuilt: history === "reconstructed" ? ["image"] : undefined }} resource={resource} onSelectResource={vi.fn()} />);
    expect(screen.getByText(history ? /History incomplete/ : "History completeness not yet loaded.")).toBeTruthy();
    expect(screen.queryByText(/No change on it/)).toBeNull();
  });

  it("names untracked dependencies and bounded evidence without provenance pills", () => {
    mount(<BreakRow issue={{ ...issue, untracked: ["Secret"] }} resource={resource} onSelectResource={vi.fn()} />);
    expect(screen.getByText(/Secrets are not tracked/)).toBeTruthy();
    expect(screen.getByText("OOMKilled on 6 pods")).toBeTruthy();
    expect(screen.queryByText("complete")).toBeNull();
  });

  it("does not claim config coverage when ConfigMaps cannot be read", () => {
    mount(<BreakRow issue={{ ...issue, untracked: ["ConfigMap", "Secret"] }} resource={resource} onSelectResource={vi.fn()} sourceSync={{ state: "ready", age_s: 0, partial: false }} />);
    expect(screen.getByText("No recorded change in tracked resources.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Warnings (2)" }));
    expect(screen.getByText("Not tracked: ConfigMap.")).toBeTruthy();
    expect(screen.getByText("Secrets are not tracked.")).toBeTruthy();
    expect(screen.queryByText(/No change on it, its config/)).toBeNull();
  });

  it("preserves generic untracked kinds without claiming temporal coverage for them", () => {
    mount(<BreakRow issue={{ ...issue, untracked: ["PersistentVolumeClaim", "ServiceAccount"] }} resource={resource} onSelectResource={vi.fn()} sourceSync={{ state: "ready", age_s: 0, partial: false }} />);
    expect(screen.getByText("No recorded change in tracked resources.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Warnings (2)" }));
    expect(screen.getByText("Not tracked: PersistentVolumeClaim.")).toBeTruthy();
    expect(screen.getByText("Not tracked: ServiceAccount.")).toBeTruthy();
    expect(screen.queryByText("History incomplete.")).toBeNull();
  });

  it("discloses partial correlation instead of declaring no changes", () => {
    mount(<BreakRow issue={{ ...issue, partial: true }} resource={resource} onSelectResource={vi.fn()} />);
    expect(screen.getByText("Some changes could not be linked.")).toBeTruthy();
    expect(screen.queryByText(/No change on it/)).toBeNull();
  });

  it("does not claim no changes when complete history still reports a gap", () => {
    mount(<BreakRow issue={{ ...issue, gaps: [{ from: change.at, to: issue.broke_at! }] }} resource={resource} onSelectResource={vi.fn()} />);
    expect(screen.queryByText(/No change on it/)).toBeNull();
    const toggle = screen.getByRole("button", { name: "Warnings (1)" });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(toggle);
    expect(screen.getByText(/History incomplete/)).toBeTruthy();
    expect(screen.getByText("OOMKilled on 6 pods")).toBeTruthy();
  });

  it("assigns unique controls to repeated root disclosures", () => {
    mount(<><BreakRow issue={{ ...issue, partial: true }} resource={resource} onSelectResource={vi.fn()} /><BreakRow issue={{ ...issue, partial: true }} resource={resource} onSelectResource={vi.fn()} /></>);
    const toggles = screen.getAllByRole("button", { name: "Warnings (1)" });
    expect(new Set(toggles.map((toggle) => toggle.getAttribute("aria-controls"))).size).toBe(2);
    fireEvent.click(toggles[0]);
    expect(toggles[1].getAttribute("aria-expanded")).toBe("false");
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
    const lookup = vi.spyOn(api, "kubernetesIssues").mockResolvedValue({ items: [issue], totals: {}, broken: { total: 1, by_severity: { critical: 1 } }, truncated: false, sync: { state: "ready", age_s: 0, partial: false } });
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

  it.each([undefined, "reconstructed", "complete"] as const)("advertises only available collapsed history metadata for %s without eager root reads", async (history) => {
    const detail = { ...issue, history: "reconstructed" as const, rebuilt: ["image"], partial: true };
    const lookup = vi.spyOn(api, "kubernetesIssues").mockResolvedValue({ items: [detail], totals: {}, truncated: false, sync: { state: "ready", age_s: 1, partial: false } });
    mount(<BreakRow issue={{ ...issue, history, evidence: undefined }} resource={resource} onSelectResource={vi.fn()} collapsed />);
    expect(lookup).not.toHaveBeenCalled();
    if (history === "complete") expect(screen.queryByRole("button", { name: /Warnings/ })).toBeNull();
    else {
      const toggle = screen.getByRole("button", { name: "Warnings (1)" });
      fireEvent.click(toggle);
      expect(screen.getByRole("button", { name: "Expand checkout" }).getAttribute("aria-expanded")).toBe("false");
      expect(screen.getByText(history ? "History incomplete." : "History completeness not yet loaded.")).toBeTruthy();
      expect(lookup).not.toHaveBeenCalled();
    }
    fireEvent.click(screen.getByRole("button", { name: "Expand checkout" }));
    expect(await screen.findByText("OOMKilled on 6 pods")).toBeTruthy();
    const toggle = screen.getByRole("button", { name: "Warnings (2)" });
    if (toggle.getAttribute("aria-expanded") !== "true") fireEvent.click(toggle);
    expect(screen.getByText(/Image history rebuilt from ReplicaSets/)).toBeTruthy();
    expect(screen.getByText("Some changes could not be linked.")).toBeTruthy();
    expect(lookup).toHaveBeenCalledOnce();
  });

  it("retains open aggregate warnings and focus when sync age and change timestamps refresh", async () => {
    const sync = { state: "stale", age_s: 4, partial: false };
    const page = { items: [issue], totals: {}, truncated: false, sync };
    vi.spyOn(api, "kubernetesIssues").mockResolvedValue(page);
    const { client } = mount(<TimelineCard changes={{ items: [change], sync }} windowEnd={Date.now()} clusterId="east" resourceFor={() => resource} onSelectView={vi.fn()} onSelectResource={vi.fn()} />);
    const toggle = await screen.findByRole("button", { name: "Warnings (2)" });
    fireEvent.click(toggle);
    toggle.focus();
    await act(async () => { client.setQueryData(kubernetesKey("east", "kubernetes-issues", "broken", "east"), { ...page, sync: { ...sync, age_s: 34 } }); });
    expect(screen.getByRole("button", { name: "Warnings (2)" })).toBe(toggle);
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    expect(document.activeElement).toBe(toggle);
  });

  it("preserves rebuilt context and distinct graph incompleteness with one multi-gap region", async () => {
    window.history.replaceState(null, "", `/agent/kubernetes?view=timeline&root_uid=checkout-uid&namespace=shop&center=${issue.broke_at}`);
    const sync = { state: "ready", age_s: 0, partial: false };
    const gaps = [{ from: "2026-10-14T11:50:00Z", to: "2026-10-14T11:51:00Z" }, { from: "2026-10-14T11:52:00Z", to: "2026-10-14T11:53:00Z" }];
    const page = { items: [{ ...issue, history: "reconstructed" as const, rebuilt: ["image"], untracked: ["Secret"], gaps }], totals: {}, truncated: false, sync };
    const lookup = vi.spyOn(api, "kubernetesIssues").mockResolvedValue(page);
    vi.spyOn(api, "kubernetesGraph").mockResolvedValue({ nodes: [{ id: "checkout-uid", kind: "Deployment", namespace: "shop", name: "checkout", group: "shop" }], edges: [], omitted: { ConfigMap: 2 }, sync });
    vi.spyOn(api, "kubernetesChanges").mockResolvedValue({ items: [change], gaps: [...gaps, gaps[0]], sync });
    const { client } = mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    expect(await screen.findByRole("button", { name: /Deployment checkout:.* at / })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Warnings (5)" }));
    expect(screen.getByText(/Image history rebuilt from ReplicaSets/)).toBeTruthy();
    expect(screen.getByText(/Related-resource evidence is incomplete/)).toBeTruthy();
    expect(screen.getAllByRole("region", { name: "Timeline history gaps" })).toHaveLength(1);
    expect(within(screen.getByRole("region", { name: "Timeline history gaps" })).getAllByRole("listitem")).toHaveLength(2);
    const toggle = screen.getByRole("button", { name: "Warnings (5)" });
    toggle.focus();
    await act(async () => { client.setQueryData(kubernetesKey("east", "kubernetes-issues", "timeline-root", "checkout-uid", "shop"), { ...page, sync: { ...sync, age_s: 30 } }); });
    expect(screen.getByRole("button", { name: "Warnings (5)" })).toBe(toggle);
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    expect(document.activeElement).toBe(toggle);
    lookup.mockRejectedValue(new ApiError(403, "Forbidden"));
    await act(async () => { await client.invalidateQueries({ queryKey: kubernetesKey("east", "kubernetes-issues", "timeline-root", "checkout-uid", "shop") }); });
    expect(await screen.findByText("Root history unavailable.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Warnings/ })).toBeNull();
    expect(screen.queryByRole("region", { name: "Timeline history gaps" })).toBeNull();
  });

  it.each([401, 403, 404])("preserves LIST warnings after closing a root denied with %s without leaking cached history across clusters", async (status) => {
    const cached = { ...issue, history: "reconstructed" as const, rebuilt: ["private"], untracked: ["CachedKind"], changes: [{ change: { ...change, name: "cached-east-config" }, relation: "same_object", lead_seconds: 240 }] };
    const page = { items: [cached], totals: {}, truncated: false, sync: { state: "ready", age_s: 0, partial: false } };
    const lookup = vi.spyOn(api, "kubernetesIssues").mockResolvedValue(page);
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
    const toggle = screen.getByRole("button", { name: "Warnings (4)" });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(toggle);
    expect(screen.getByRole("list").textContent).toContain("Root history unavailable");
    expect(screen.getByText("History completeness not yet loaded.")).toBeTruthy();
    expect(screen.getByText("Secrets are not tracked.")).toBeTruthy();
    expect(screen.getByText("Some changes could not be linked.")).toBeTruthy();
    expect(screen.queryByText(/cached-east-config|Private history|CachedKind/)).toBeNull();
    expect(screen.queryByText("OOMKilled on 6 pods")).toBeNull();
    expect(screen.getByText("CrashLoopBackOff")).toBeTruthy();
    expect(lookup).toHaveBeenCalledTimes(2);
    await act(async () => { client.setQueryData(kubernetesKey("west", "kubernetes-issues", "root", "shop", "checkout-uid"), { ...page, items: [{ ...cached, changes: [{ ...cached.changes[0], change: { ...change, cluster: "west", name: "cached-west-config" } }] }] }, { updatedAt: 1 }); });
    fireEvent.click(screen.getByRole("button", { name: "Switch cluster" }));
    expect(screen.getByRole("button", { name: /Warnings/ }).getAttribute("aria-expanded")).toBe("false");
    expect(lookup).toHaveBeenCalledTimes(2);
    fireEvent.click(screen.getByRole("button", { name: "Expand checkout" }));
    expect(await screen.findByText("Root history unavailable.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Collapse checkout" }));
    const westToggle = screen.getByRole("button", { name: "Warnings (4)" });
    expect(westToggle.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(westToggle);
    expect(screen.getByRole("list").textContent).toContain("Root history unavailable");
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
    vi.spyOn(api, "kubernetesIssues").mockResolvedValue({ items: [issue], totals: {}, truncated: false, sync });
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
    vi.spyOn(api, "kubernetesIssues").mockResolvedValue({ items: [], totals: {}, truncated: false, sync: ready });
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
    vi.spyOn(api, "kubernetesIssues").mockResolvedValue({ items: [issue], totals: {}, truncated: false, sync });
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

  it("keeps zero broken workloads distinct from missing totals", async () => {
    vi.spyOn(api, "kubernetesIssues").mockResolvedValue({ items: [], totals: {}, broken: { total: 0, by_severity: {} }, truncated: false, sync: { state: "ready", age_s: 0, partial: false } });
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

  it("keeps the live snapshot now visible after an asynchronous response", async () => {
    const snapshot = Date.parse("2026-10-14T12:06:00Z");
    vi.spyOn(Date, "now").mockReturnValue(snapshot);
    const sync = { state: "ready", age_s: 0, partial: false };
    vi.spyOn(api, "kubernetesIssues").mockResolvedValue({ items: [], totals: {}, truncated: false, sync });
    vi.spyOn(api, "kubernetesChanges").mockImplementation(async () => {
      await Promise.resolve();
      vi.mocked(Date.now).mockReturnValue(snapshot + 1_250);
      return { items: [change], sync };
    });
    const { container } = mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    await screen.findByRole("button", { name: /Deployment checkout:.* at / });
    expect(container.querySelector<HTMLElement>(".resource-timeline-now-line")?.style.left).toBe("100%");
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

  it("retains chart identity and open details through the live thirty second poll", async () => {
    const snapshot = Date.parse("2026-10-14T12:06:00Z");
    vi.spyOn(Date, "now").mockReturnValue(snapshot);
    const sync = { state: "ready", age_s: 0, partial: false };
    vi.spyOn(api, "kubernetesIssues").mockResolvedValue({ items: [], totals: {}, truncated: false, sync });
    const reads = vi.spyOn(api, "kubernetesChanges").mockResolvedValue({ items: [change], sync });
    let poll: TimerHandler | undefined;
    const originalInterval = window.setInterval.bind(window);
    vi.spyOn(window, "setInterval").mockImplementation((handler, delay) => {
      if (String(handler).includes("setWindowEnd")) { poll = handler; return 42; }
      return originalInterval(handler, delay);
    });
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    const marker = await screen.findByRole("button", { name: /Deployment checkout:.* at / });
    const chart = screen.getByRole("region", { name: "Resource timeline chart" });
    marker.focus();
    vi.mocked(Date.now).mockReturnValue(snapshot + 30_000);
    await act(async () => { if (typeof poll === "function") poll(); });
    expect(screen.getByRole("region", { name: "Resource timeline chart" })).toBe(chart);
    expect(document.activeElement).toBe(marker);
    fireEvent.click(marker);
    const heading = screen.getByRole("heading", { name: "Timeline event details" });
    vi.mocked(Date.now).mockReturnValue(snapshot + 60_000);
    await act(async () => { if (typeof poll === "function") poll(); });
    expect(document.activeElement).toBe(heading);
    expect(reads).toHaveBeenCalledTimes(3);
    const kind = screen.getByRole("combobox", { name: "Timeline kind" });
    kind.focus();
    expect(document.activeElement).toBe(kind);
    fireEvent.change(kind, { target: { value: "Deployment" } });
    await waitFor(() => expect(screen.queryByRole("region", { name: "Timeline event details" })).toBeNull());
    expect(document.activeElement).not.toBe(document.body);
    expect(document.activeElement).toBe(kind);
  });

  it("retains namespace typing when filtering removes the selected marker", async () => {
    const user = userEvent.setup();
    vi.spyOn(Date, "now").mockReturnValue(Date.parse("2026-10-14T12:06:00Z"));
    const sync = { state: "ready", age_s: 0, partial: false };
    vi.spyOn(api, "kubernetesIssues").mockResolvedValue({ items: [], totals: {}, truncated: false, sync });
    const reads = vi.spyOn(api, "kubernetesChanges").mockImplementation(async (options) => ({ items: options?.namespace && options.namespace !== "shop" ? [] : [change], sync }));
    mount(<KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} />);
    await user.click(await screen.findByRole("button", { name: /Deployment checkout:.* at / }));
    expect(screen.getByRole("region", { name: "Timeline event details" })).toBeTruthy();
    const namespace = screen.getByRole("textbox", { name: "Timeline namespace" });
    await user.click(namespace);
    for (const letter of "other") {
      await user.keyboard(letter);
      expect(document.activeElement).toBe(namespace);
    }
    await waitFor(() => expect(reads).toHaveBeenLastCalledWith(expect.objectContaining({ namespace: "other" })));
    expect((namespace as HTMLInputElement).value).toBe("other");
    expect(screen.queryByRole("button", { name: /Deployment checkout:.* at / })).toBeNull();
    expect(screen.queryByRole("region", { name: "Timeline event details" })).toBeNull();
    expect(document.activeElement).toBe(namespace);
  });

  it("retains external control focus when a thirty second poll removes the selected marker", async () => {
    const snapshot = Date.parse("2026-10-14T12:06:00Z");
    vi.spyOn(Date, "now").mockReturnValue(snapshot);
    const sync = { state: "ready", age_s: 0, partial: false };
    vi.spyOn(api, "kubernetesIssues").mockResolvedValue({ items: [], totals: {}, truncated: false, sync });
    const reads = vi.spyOn(api, "kubernetesChanges").mockResolvedValue({ items: [change], sync });
    let poll: TimerHandler | undefined;
    const originalInterval = window.setInterval.bind(window);
    vi.spyOn(window, "setInterval").mockImplementation((handler, delay) => {
      if (String(handler).includes("setWindowEnd")) { poll = handler; return 42; }
      return originalInterval(handler, delay);
    });
    mount(<><button type="button">Outside timeline</button><KubernetesExplorerTabContent tab="timeline" onSelectResource={vi.fn()} /></>);
    const chart = await screen.findByRole("region", { name: "Resource timeline chart" });
    fireEvent.click(await screen.findByRole("button", { name: /Deployment checkout:.* at / }));
    const outside = screen.getByRole("button", { name: "Outside timeline" });
    outside.focus();
    const previousUntil = chart.getAttribute("data-until");
    reads.mockResolvedValue({ items: [], sync });
    expect(typeof poll).toBe("function");
    vi.mocked(Date.now).mockReturnValue(snapshot + 30_000);
    await act(async () => { if (typeof poll === "function") poll(); });
    await waitFor(() => expect(screen.queryByRole("button", { name: /Deployment checkout:.* at / })).toBeNull());
    expect(chart.getAttribute("data-until")).not.toBe(previousUntil);
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

  it("qualifies stale zero broken totals as available evidence", async () => {
    vi.spyOn(api, "kubernetesIssues").mockResolvedValue({ items: [], totals: {}, broken: { total: 0, by_severity: {} }, truncated: false, sync: { state: "stale", age_s: 90, partial: false } });
    mount(<TimelineCard windowEnd={Date.now()} clusterId="east" resourceFor={() => resource} onSelectView={vi.fn()} onSelectResource={vi.fn()} />);
    expect(await screen.findByText("No broken workloads in available evidence")).toBeTruthy();
    expect(screen.queryByText("No broken workloads", { exact: true })).toBeNull();
  });

  it("does not interpret missing totals as zero", async () => {
    vi.spyOn(api, "kubernetesIssues").mockResolvedValue({ items: [], totals: {}, truncated: false, sync: { state: "ready", age_s: 0, partial: false } });
    mount(<TimelineCard windowEnd={Date.now()} clusterId="east" resourceFor={() => resource} onSelectView={vi.fn()} onSelectResource={vi.fn()} />);
    await waitFor(() => expect(screen.getByRole("button", { name: /Broken Unknown/ })).toBeTruthy());
    expect(screen.queryByText("No broken workloads")).toBeNull();
  });

  it("deduplicates aggregate failures and discloses stale capped changes without hiding broken evidence", async () => {
    const failures = [{ resource_id: "ConfigMap", class: "forbidden" }, { resource_id: "ConfigMap", class: "forbidden" }];
    const sync = { state: "ready", age_s: 4, partial: true };
    vi.spyOn(api, "kubernetesIssues").mockResolvedValue({ items: [issue], totals: {}, broken: { total: 1, by_severity: { critical: 1 } }, truncated: false, sync, partial_failures: failures });
    mount(<TimelineCard changes={{ items: [], sync: { ...sync, state: "stale" }, truncated: true, partial_failures: failures }} windowEnd={Date.now()} clusterId="east" resourceFor={() => resource} onSelectView={vi.fn()} onSelectResource={vi.fn()} />);
    const toggle = await screen.findByRole("button", { name: "Warnings (3)" });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    expect(screen.getByText("CrashLoopBackOff")).toBeTruthy();
    expect(screen.getByText("OOMKilled on 6 pods")).toBeTruthy();
    fireEvent.click(toggle);
    expect(screen.getAllByText("ConfigMap: forbidden")).toHaveLength(1);
    expect(screen.getByText("Change activity is stale.")).toBeTruthy();
    expect(screen.getByText(/Change history scan is bounded/)).toBeTruthy();
    expect(screen.queryByText("Break evidence is partial.")).toBeNull();
  });
});