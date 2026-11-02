// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { notifyManager, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, useSearchParams } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { useLayoutEffect, useState } from "react";
import { api, ApiError, kubernetesAllContainers, type KubernetesBreakLogPage, type KubernetesIssue } from "./api";
import { KubernetesClusterContext, kubernetesKey } from "./useKubernetesCluster";
import { breakLogWindow, timestampNanos, useBreakLogsState } from "./useKubernetesBreakLogs";
import { appendBreakLogLines, mergeBreakLogData } from "./kubernetesBreakLogMerge";
import { KubernetesBreakLogs, KubernetesBreakLogsProvider, LogWindowBracket } from "../pages/KubernetesBreakLogs";

const at = "2026-10-31T12:00:00.123456789Z";
const issue: KubernetesIssue = { root: { kind: "Deployment", name: "api", uid: "root", namespace: "shop" }, occurrence_id: "occurrence", broke_at: at, rule: "crash", severity: "critical", broken: true, count: 1, of: 1 };
const line: KubernetesBreakLogPage["lines"][number] = { at, pod: "deleted-pod", pod_uid: "old-pod", container: "api", init: false, slot: "previous", level: "error", text: "failure", source: "linked" };
const page: KubernetesBreakLogPage = {
  occurrence_id: "occurrence", window: { from: "2026-10-31T11:59:00.123456781Z", to: "2026-10-31T12:02:00.123456799Z" },
  lines: [line], pods: [{ name: "deleted-pod", uid: "old-pod", state: "unknown", short: "pod" }], markers: [{ at, type: "break", label: "failed" }],
  sources: [{ kind: "linked", name: "logs", type: "loki", at }], counts: { all: 1, errors: 1, warnings: 0 }, rate: [], patterns: [],
  containers: [{ name: "api", main: true }], main_container: "api", container: "api",
  coverage: { state: "pending", tip: false, reason: "", gaps: [{ from: at, to: "2026-10-31T12:02:00Z", reason: "linked_pending", ready_at: "2026-10-31T12:02:30.123456799Z" }] },
};
function Probe({ value, active }: { value: KubernetesIssue; active: boolean }) {
  const state = useBreakLogsState(value, vi.fn());
  const [, setParams] = useSearchParams();
  // The production panel supplies this mounted/active signal.
  return <><PanelActivity active={active} set={state.setPanelActive} />
    <output aria-label="Lines">{state.lines.map(item => item.text).join("|")}</output>
    <output aria-label="Container">{state.containers.map(item => item.name).join("|")}</output>
    <button onClick={() => state.setSearch("failure")}>Search</button>
    <button onClick={() => state.setContainer("api")}>Container</button>
    <button onClick={() => state.setWindow(Date.parse(page.window.from) - 30_000, Date.parse(page.window.to))}>Window</button>
    <button onClick={() => setParams(current => { const next = new URLSearchParams(current); next.set("center", "2026-10-31T12:00:00.123456780Z"); return next; })}>Center</button>
    <button onClick={() => void state.query.fetchNextPage()}>More</button>
    <output aria-label="Error">{state.query.error?.message}</output></>;
}
function PanelActivity({ active, set }: { active: boolean; set: (value: boolean) => void }) {
  useLayoutEffect(() => { set(active); return () => set(false); }, [active, set]);
  return null;
}
function mount(initialIssue = issue) {
  const cache = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: Infinity } } });
  function App() {
    const [value, setValue] = useState(initialIssue), [cluster, setCluster] = useState("east"), [active, setActive] = useState(true), [, rerender] = useState(0);
    return <KubernetesClusterContext.Provider value={{ client: api, cluster, multiple: true, clusters: [], key: (...parts) => kubernetesKey(cluster, ...parts) }}>
      <button onClick={() => setCluster("west")}>Cluster</button>
      <button onClick={() => setValue({ ...issue, root: { ...issue.root, namespace: "other" } })}>Namespace</button>
      <button onClick={() => setValue({ ...issue, occurrence_id: "next-occurrence" })}>Occurrence</button>
      <button onClick={() => setActive(false)}>Close panel</button>
      <button onClick={() => rerender(count => count + 1)}>Rerender</button>
      <Probe value={value} active={active} />
    </KubernetesClusterContext.Provider>;
  }
  return { cache, ...render(<QueryClientProvider client={cache}><MemoryRouter initialEntries={[`/?center=${at}&logs_from=${page.window.from}&logs_to=${page.window.to}`]}><App /></MemoryRouter></QueryClientProvider>) };
}
async function advance(ms = 0) {
  await act(async () => { await vi.advanceTimersByTimeAsync(ms); });
}
function mountPanel() {
  const cache = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: Infinity } } });
  function App() {
    const [open, setOpen] = useState(true);
    const [selected, setSelected] = useState(issue);
    return <KubernetesBreakLogsProvider issue={selected} onSelectResource={vi.fn()}>
      <button onClick={() => setOpen(value => !value)}>Toggle panel</button>
      <button onClick={() => setSelected({ ...issue, occurrence_id: "next-occurrence" })}>Change occurrence</button>
      <LogWindowBracket since="2026-10-31T11:30:00Z" until="2026-10-31T12:30:00Z" />
      {open && <KubernetesBreakLogs />}
    </KubernetesBreakLogsProvider>;
  }
  return { cache, ...render(<QueryClientProvider client={cache}><KubernetesClusterContext.Provider value={{ client: api, cluster: "east", multiple: true, clusters: [], key: (...parts) => kubernetesKey("east", ...parts) }}><MemoryRouter initialEntries={[`/?center=${at}&logs_from=${page.window.from}&logs_to=${page.window.to}`]}><App /></MemoryRouter></KubernetesClusterContext.Provider></QueryClientProvider>) };
}
beforeEach(() => { notifyManager.setScheduler(queueMicrotask); vi.useFakeTimers(); vi.setSystemTime(new Date(at)); });
afterEach(() => {
  window.getSelection()?.removeAllRanges();
  cleanup();
  notifyManager.setScheduler(callback => setTimeout(callback, 0));
  vi.restoreAllMocks();
  vi.useRealTimers();
});

describe("Pending linked break logs", () => {
  it("pins raw nanoseconds and retries at 15/30/60 seconds, ending within five minutes", async () => {
    const read = vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValue(page);
    mount(); await advance();
    expect(read.mock.calls[0][0]).toMatchObject({ occurrence_id: "occurrence", center: at, since: page.window.from, until: page.window.to });
    await advance(14_999); expect(read).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByText("Rerender"));
    await advance(1); expect(read).toHaveBeenCalledTimes(2);
    await advance(29_999); expect(read).toHaveBeenCalledTimes(2);
    await advance(1); expect(read).toHaveBeenCalledTimes(3);
    await advance(59_999); expect(read).toHaveBeenCalledTimes(3);
    await advance(1); expect(read).toHaveBeenCalledTimes(4);
    await advance(195_000); expect(read).toHaveBeenCalledTimes(7);
    await advance(600_000); expect(read).toHaveBeenCalledTimes(7);
    for (const [options] of read.mock.calls) expect(options).toMatchObject({ center: at, since: page.window.from, until: page.window.to });
  });

  it("retains duplicates, loaded cursor lines and markers across replay and mature empty reads", async () => {
    const older = { ...line, text: "older", at: page.window.from };
    const read = vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValueOnce({ ...page, lines: [line, line], next: "cursor" })
      .mockResolvedValueOnce({ ...page, lines: [older] })
      .mockResolvedValueOnce({ ...page, lines: [line, line, line], next: "fresh" })
      .mockResolvedValue({ ...page, lines: [], coverage: { state: "partial", tip: false, reason: "no_logs", gaps: [{ from: at, to: at, reason: "no_logs" }] } });
    const { cache } = mount(); await advance();
    fireEvent.click(screen.getByText("More")); await advance();
    expect(read.mock.calls[1][0].cursor).toBe("cursor");
    await advance(15_000);
    expect(screen.getByLabelText("Lines").textContent).toBe("failure|failure|older|failure");
    expect(read.mock.calls[2][0].cursor).toBeUndefined();
    const data = cache.getQueryCache().findAll().find(query => query.queryKey.includes("kubernetes-break-logs"))?.state.data;
    expect(data).toMatchObject({ pages: [{ next: "fresh", markers: page.markers }] });
    await advance(30_000);
    expect(screen.getByLabelText("Lines").textContent).toBe("failure|failure|older|failure");
    await advance(300_000); expect(read).toHaveBeenCalledTimes(4);
  });

  it.each(["no_logs", "unknown_lifetime", "overlap", "budget"])("does not retry a mature %s gap", async reason => {
    const read = vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValue({ ...page, coverage: { state: "partial", tip: true, reason, gaps: [{ from: at, to: at, reason }] } });
    mount(); await advance(); await advance(300_000); expect(read).toHaveBeenCalledTimes(1);
  });

  it.each([401, 403, 404])("cancels and clears lines and container memory after HTTP %s", async status => {
    const read = vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValueOnce(page).mockRejectedValue(new ApiError(status, "denied"));
    mount(); await advance(); await advance(15_000);
    expect(screen.getByLabelText("Lines").textContent).toBe("");
    expect(screen.getByLabelText("Container").textContent).toBe("");
    await advance(300_000); expect(read).toHaveBeenCalledTimes(2);
  });

  it.each(["Cluster", "Namespace", "Occurrence", "Center", "Window", "Search", "Container"])("cancels old reads without placeholders when %s changes", async action => {
    let finish: (value: KubernetesBreakLogPage) => void = () => undefined;
    const read = vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValueOnce(page).mockImplementation(() => new Promise(resolve => { finish = resolve; }));
    mount(); await advance(); await advance(15_000);
    const signal = read.mock.calls[1][1];
    const finishOld = finish;
    fireEvent.click(screen.getByText(action)); await advance();
    expect(signal?.aborted).toBe(true);
    expect(screen.getByLabelText("Lines").textContent).toBe("");
    await act(async () => finishOld({ ...page, lines: [{ ...line, text: "cancelled old scope" }] })); await advance();
    expect(screen.getByLabelText("Lines").textContent).toBe("");
    await act(async () => finish({ ...page, occurrence_id: action === "Occurrence" ? "next-occurrence" : page.occurrence_id, lines: [{ ...line, text: "new scope" }] }));
    await advance();
    expect(screen.getByLabelText("Lines").textContent).not.toContain("failure");
  });

  it("fences a settings source change and does not replay old source lines", async () => {
    const read = vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValueOnce(page).mockImplementation(() => new Promise(() => undefined));
    const { cache } = mount(); await advance(); await advance(15_000);
    const signal = read.mock.calls[1][1];
    await act(async () => { cache.setQueryData(kubernetesKey("east", "kubernetes-agent-settings"), { break_logs: { linked_source: "new-source", fields: {} } }); });
    await advance(); expect(signal?.aborted).toBe(true);
    expect(screen.getByLabelText("Lines").textContent).toBe("");
    expect(screen.getByLabelText("Container").textContent).toBe("");
  });

  it("drops prior evidence rather than combining two linked response sources", async () => {
    vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValueOnce(page).mockResolvedValue({ ...page, sources: [{ ...page.sources[0], name: "new-source" }], lines: [{ ...line, text: "new source" }] });
    mount(); await advance(); await advance(15_000);
    expect(screen.getByLabelText("Lines").textContent).toBe("new source");
  });

  it.each(["explicit", "fallback"])("rejects mismatched %s occurrence confirmation", async selection => {
    const read = vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValueOnce(selection === "explicit" ? { ...page, occurrence_id: "wrong" } : page)
      .mockResolvedValue({ ...page, occurrence_id: "wrong", lines: [{ ...line, text: "wrong occurrence" }] });
    mount(selection === "fallback" ? { ...issue, occurrence_id: undefined } : issue); await advance();
    if (selection === "fallback") await advance(15_000);
    expect(screen.getByLabelText("Lines").textContent).toBe("");
    expect(screen.getByLabelText("Error").textContent).toContain("selected occurrence");
    await advance(300_000); expect(read).toHaveBeenCalledTimes(selection === "explicit" ? 1 : 2);
  });

  it.each(["Close panel", "unmount", "hidden"])("cancels in-flight retries on %s", async action => {
    const visible = vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
    const read = vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValueOnce(page).mockImplementation(() => new Promise(() => undefined));
    const { unmount } = mount(); await advance(); await advance(15_000);
    const signal = read.mock.calls[1][1];
    if (action === "unmount") unmount();
    else if (action === "hidden") { visible.mockReturnValue("hidden"); fireEvent(document, new Event("visibilitychange")); }
    else fireEvent.click(screen.getByText(action));
    await advance(); expect(signal?.aborted).toBe(true);
    await advance(300_000); expect(read).toHaveBeenCalledTimes(2);
  });

  it("aborts an unfinished final automatic read at the five-minute deadline", async () => {
    const read = vi.spyOn(api, "kubernetesBreakLogs").mockImplementation(async () => page);
    mount(); await advance(); await advance(225_000);
    expect(read).toHaveBeenCalledTimes(6);
    read.mockImplementation(() => new Promise(() => undefined));
    await advance(60_000); expect(read).toHaveBeenCalledTimes(7);
    const signal = read.mock.calls[6][1];
    expect(signal?.aborted).toBe(false);
    await advance(14_999); expect(signal?.aborted).toBe(false);
    await advance(1); expect(signal?.aborted).toBe(true);
    await advance(300_000); expect(read).toHaveBeenCalledTimes(7);
  });

  it("cannot republish an aborted epoch/authority read after the parent clears its query", async () => {
    let finish: (value: KubernetesBreakLogPage) => void = () => undefined;
    const read = vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValueOnce(page).mockImplementation(() => new Promise(resolve => { finish = resolve; }));
    const { cache } = mount(); await advance(); await advance(15_000);
    const finishAborted = finish;
    await act(async () => {
      await cache.cancelQueries({ queryKey: kubernetesKey("east", "kubernetes-break-logs") });
      cache.removeQueries({ queryKey: kubernetesKey("east", "kubernetes-break-logs") });
    });
    await act(async () => finishAborted({ ...page, lines: [{ ...line, text: "stale authority" }] })); await advance();
    expect(read.mock.calls[1][1]?.aborted).toBe(true);
    expect(screen.getByLabelText("Lines").textContent).not.toContain("stale authority");
  });
});

describe("Stable break log merge", () => {
  it("keeps the advanced cursor through first-page replay and appends genuine repeated records on the next page", async () => {
    const a = { ...line, text: "a" }, b = { ...line, text: "b" }, c = { ...line, text: "c" }, d = { ...line, text: "d" };
    const read = vi.spyOn(api, "kubernetesBreakLogs")
      .mockResolvedValueOnce({ ...page, lines: [a, b], next: "p2" })
      .mockResolvedValueOnce({ ...page, lines: [c, d], next: "p3" })
      .mockResolvedValueOnce({ ...page, lines: [a, b], next: "p2" })
      .mockResolvedValueOnce({ ...page, lines: [d, d], next: "p4" })
      .mockResolvedValue({ ...page, lines: [a, b], next: "p2" });
    mount(); await advance();
    fireEvent.click(screen.getByText("More")); await advance();
    expect(screen.getByLabelText("Lines").textContent).toBe("a|b|c|d");
    await advance(15_000);
    expect(screen.getByLabelText("Lines").textContent).toBe("a|b|c|d");
    fireEvent.click(screen.getByText("More")); await advance();
    expect(read.mock.calls[3][0].cursor).toBe("p3");
    expect(screen.getByLabelText("Lines").textContent).toBe("a|b|c|d|d|d");
    await advance(30_000);
    expect(screen.getByLabelText("Lines").textContent).toBe("a|b|c|d|d|d");
  });

  it("compares equivalent offset instants without rounding nanoseconds", () => {
    expect(timestampNanos(at)).toBe(timestampNanos("2026-10-31T14:00:00.123456789+02:00"));
    expect(timestampNanos("2026-10-31T12:00:00.1Z")).toBe(timestampNanos("2026-10-31T12:00:00.100000000Z"));
    expect(timestampNanos(at)).not.toBe(timestampNanos("2026-10-31T12:00:00.123456788Z"));
  });

  it("moves the same pending reader through expansion and Escape without resetting rows or scroll", async () => {
    const earlier = { ...line, at: page.window.from, text: "late earlier" };
    vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValueOnce(page).mockResolvedValue({ ...page, lines: [line, earlier] });
    mountPanel(); await advance();
    const pane = screen.getByRole("log");
    pane.scrollTop = 200;
    await advance(15_000);
    const rows = Array.from(pane.children);
    const range = document.createRange();
    range.selectNodeContents(screen.getByText("failure"));
    window.getSelection()?.removeAllRanges();
    window.getSelection()?.addRange(range);
    fireEvent.click(screen.getByRole("button", { name: "Expand logs" }));
    expect(screen.getByRole("log")).toBe(pane);
    expect(pane.scrollTop).toBe(200);
    expect(Array.from(pane.children)).toEqual(rows);
    expect(window.getSelection()?.toString()).toBe("failure");
    expect(screen.getByRole("dialog").textContent).toContain("Logs update automatically while this panel is open.");
    fireEvent.keyDown(document, { key: "Escape" });
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.getByRole("log")).toBe(pane);
    expect(pane.scrollTop).toBe(200);
    expect(Array.from(pane.children)).toEqual(rows);
    expect(window.getSelection()?.toString()).toBe("failure");
    window.getSelection()?.removeAllRanges();
  });

  it("preserves identical line multiplicity without growing on replay", () => {
    expect(appendBreakLogLines([line, line], [line, line])).toHaveLength(2);
    expect(appendBreakLogLines([line, line], [line, line, line])).toHaveLength(3);
    expect(appendBreakLogLines([line, line], [])).toHaveLength(2);
  });

  describe("Real provider and panel pending lifecycle", () => {
    it("retains an advanced cursor across repeated identical first-page refreshes and counts real identical cursor records", async () => {
      const first = { ...page, lines: [line, line], next: "cursor-a" };
      const read = vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValueOnce(first)
        .mockResolvedValueOnce({ ...page, lines: [line], next: "cursor-b" })
        .mockResolvedValueOnce(first)
        .mockResolvedValueOnce(first)
        .mockResolvedValueOnce({ ...page, lines: [line], next: undefined })
        .mockResolvedValue({ ...page, lines: [], next: undefined, coverage: { state: "partial", tip: false, reason: "no_logs", gaps: [] } });
      const { cache } = mountPanel(); await advance();
      const pane = screen.getByRole("log");
      const originalRows = Array.from(pane.children);
      const continuation = () => (cache.getQueryCache().findAll().find(query => query.queryKey.includes("kubernetes-break-logs"))?.state.data as { pages: KubernetesBreakLogPage[] }).pages[0].next;
      expect(screen.getAllByText("failure")).toHaveLength(2);
      fireEvent.click(screen.getByRole("button", { name: "Load more logs" })); await advance();
      expect(screen.getAllByText("failure")).toHaveLength(3);
      expect(continuation()).toBe("cursor-b");
      for (const delay of [15_000, 30_000]) {
        await advance(delay);
        expect(read.mock.calls.at(-1)?.[0].cursor).toBeUndefined();
        expect(screen.getAllByText("failure")).toHaveLength(3);
        expect(continuation()).toBe("cursor-b");
      }
      fireEvent.click(screen.getByRole("button", { name: "Load more logs" })); await advance();
      expect(read.mock.calls.map(([options]) => options.cursor)).toEqual([undefined, "cursor-a", undefined, undefined, "cursor-b"]);
      expect(screen.getAllByText("failure")).toHaveLength(4);
      expect(screen.queryByRole("button", { name: "Load more logs" })).toBeNull();
      expect(continuation()).toBeUndefined();
      for (const [index, row] of originalRows.entries()) expect(pane.children[index]).toBe(row);
      await advance(60_000);
      expect(screen.getAllByText("failure")).toHaveLength(4);
      await advance(300_000); expect(read).toHaveBeenCalledTimes(6);
    });

    it("moves the same expanded reader with readiness status, append order and scroll intact", async () => {
      const earlier = { ...line, at: page.window.from, text: "earlier arrival" };
      vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValueOnce(page)
        .mockResolvedValue({ ...page, lines: [line, earlier] });
      mountPanel(); await advance();
      const pane = screen.getByRole("log");
      const originalRows = Array.from(pane.children);
      const readiness = screen.getByText(/Waiting for logs to ingest/);
      expect(readiness.textContent).toContain("Logs update automatically while this panel is open.");
      const readySeconds = Math.ceil((Date.parse(page.coverage.gaps[0].ready_at!) - Date.now()) / 1000);
      expect(readiness.textContent).toContain(`about ${readySeconds} s`);
      pane.scrollTop = 200;
      fireEvent.click(screen.getByRole("button", { name: "Expand logs" })); await advance();
      expect(screen.getByRole("dialog").contains(readiness)).toBe(true);
      expect(screen.getByRole("log")).toBe(pane);
      expect(pane.scrollTop).toBe(200);
      await advance(15_000);
      expect(pane.lastElementChild?.textContent).toContain("earlier arrival");
      for (const [index, row] of originalRows.entries()) expect(pane.children[index]).toBe(row);
      fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" }); await advance();
      expect(screen.queryByRole("dialog")).toBeNull();
      expect(screen.getByRole("log")).toBe(pane);
      expect(pane.scrollTop).toBe(200);
      expect(screen.getByText(/Waiting for logs to ingest/)).toBe(readiness);
      expect(pane.lastElementChild?.textContent).toContain("earlier arrival");
    });

    it("keeps a genuine repeated cursor record but does not grow multiplicity on a resumed overlapping replay", async () => {
      const read = vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValueOnce({ ...page, lines: [line, line], next: "more" })
        .mockResolvedValueOnce({ ...page, lines: [line], next: undefined })
        .mockResolvedValueOnce({ ...page, lines: [line, line], next: "resume" })
        .mockResolvedValueOnce({ ...page, lines: [line, line, line, line], next: undefined })
        .mockResolvedValue({ ...page, lines: [], next: undefined, coverage: { state: "partial", tip: false, reason: "no_logs", gaps: [] } });
      mountPanel(); await advance();
      fireEvent.click(screen.getByRole("button", { name: "Load more logs" })); await advance();
      expect(read.mock.calls[1][0].cursor).toBe("more");
      expect(screen.getAllByText("failure")).toHaveLength(3);
      await advance(15_000);
      expect(read.mock.calls[2][0].cursor).toBeUndefined();
      expect(screen.getAllByText("failure")).toHaveLength(3);
      await advance(30_000);
      expect(screen.getAllByText("failure")).toHaveLength(4);
      await advance(60_000);
      expect(screen.getAllByText("failure")).toHaveLength(4);
      await advance(300_000); expect(read).toHaveBeenCalledTimes(5);
    });

    it("does not publish a closed-session callback when the actual panel reopens before its deadline", async () => {
      const completions: Array<(value: KubernetesBreakLogPage) => void> = [];
      const read = vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValueOnce(page)
        .mockImplementation(() => new Promise(resolve => { completions.push(resolve); }));
      mountPanel(); await advance(); await advance(15_000);
      const signal = read.mock.calls[1][1];
      fireEvent.click(screen.getByText("Toggle panel")); await advance();
      expect(signal?.aborted).toBe(true);
      await advance(30_000); expect(read).toHaveBeenCalledTimes(2);
      fireEvent.click(screen.getByText("Toggle panel")); await advance();
      await act(async () => completions[0]({ ...page, lines: [{ ...line, text: "closed callback" }] }));
      await advance(); expect(screen.queryByText("closed callback")).toBeNull();
      await advance(30_000); expect(read).toHaveBeenCalledTimes(3);
      await act(async () => completions[1]({ ...page, lines: [line, { ...line, text: "reopened fresh" }] }));
      await advance();
      expect(screen.getByText("reopened fresh")).toBeTruthy();
      expect(screen.queryByText("closed callback")).toBeNull();
    });

    it.each(["search", "pod", "mode", "level", "container", "window", "occurrence", "source", "epoch"])("fences deferred callbacks after a real panel %s change and publishes only fresh evidence", async action => {
      const completions: Array<(value: KubernetesBreakLogPage) => void> = [];
      const read = vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValueOnce(page)
        .mockImplementation(() => new Promise(resolve => { completions.push(resolve); }));
      const { cache } = mountPanel(); await advance(); await advance(15_000);
      const stale = completions[0], signal = read.mock.calls[1][1];
      if (action === "search") fireEvent.change(screen.getByRole("textbox", { name: "Search in log window" }), { target: { value: "fresh" } });
      else if (action === "pod") fireEvent.click(screen.getByRole("button", { name: /deleted-pod/ }));
      else if (action === "mode") fireEvent.click(screen.getByRole("button", { name: "Previous container" }));
      else if (action === "level") fireEvent.click(screen.getByRole("button", { name: "Errors 1" }));
      else if (action === "container") fireEvent.change(screen.getByRole("combobox", { name: "Log container" }), { target: { value: kubernetesAllContainers } });
      else if (action === "window") fireEvent.keyDown(screen.getByRole("slider", { name: "Log window start" }), { key: "ArrowLeft" });
      else if (action === "occurrence") fireEvent.click(screen.getByText("Change occurrence"));
      else if (action === "source") await act(async () => { cache.setQueryData(kubernetesKey("east", "kubernetes-agent-settings"), { break_logs: { linked_source: "new-source", fields: {} } }); });
      else await act(async () => {
        await cache.cancelQueries({ queryKey: kubernetesKey("east", "kubernetes-break-logs") });
        cache.removeQueries({ queryKey: kubernetesKey("east", "kubernetes-break-logs") });
      });
      await advance();
      expect(signal?.aborted).toBe(true);
      expect(screen.queryByText("failure")).toBeNull();
      await act(async () => stale({ ...page, lines: [{ ...line, text: "stale callback" }] })); await advance();
      expect(screen.queryByText("stale callback")).toBeNull();
      expect(completions).toHaveLength(2);
      const options = read.mock.calls.at(-1)![0];
      await act(async () => completions[1]({ ...page, occurrence_id: options.occurrence_id, window: { from: options.since!, to: options.until! }, lines: [{ ...line, text: "fresh scope" }] }));
      await advance();
      expect(screen.getByText("fresh scope")).toBeTruthy();
      expect(screen.queryByText("stale callback")).toBeNull();
      if (action === "search") expect((screen.getByRole("textbox") as HTMLInputElement).value).toBe("fresh");
    });

    it("polls partial lines through cache with tip false and appends repeat multiplicity without rewriting evidence", async () => {
      const pending = { ...page, lines: [line, line], next: "overlap", coverage: { ...page.coverage, state: "partial" as const } };
      const extra = { ...line, at: page.window.from, text: "arrived earlier" };
      const read = vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValueOnce(pending)
        .mockResolvedValueOnce({ ...pending, lines: [line, line, line, extra] })
        .mockResolvedValue({ ...pending, lines: [], next: undefined, coverage: { state: "partial", tip: false, reason: "no_logs", gaps: [] } });
      const { cache } = mountPanel(); await advance();
      expect(screen.getByText(/Waiting for logs to ingest/)).toBeTruthy();
      expect(screen.queryByRole("button", { name: "Log coverage tip" })).toBeNull();
      const pane = screen.getByRole("log");
      const originalRows = Array.from(pane.children);
      const bracket = screen.getByRole("slider", { name: "Log window start" });
      const boundary = bracket.getAttribute("aria-valuenow");
      pane.scrollTop = 41;
      const range = document.createRange();
      range.selectNodeContents(screen.getAllByText("failure")[0]);
      window.getSelection()?.addRange(range);
      await advance(15_000);
      expect(screen.getAllByText("failure")).toHaveLength(3);
      expect(screen.getByText("arrived earlier")).toBeTruthy();
      expect(Array.from(pane.children).slice(0, originalRows.length)).toEqual(originalRows);
      for (const [index, row] of originalRows.entries()) expect(pane.children[index]).toBe(row);
      expect(pane.lastElementChild?.textContent).toContain("arrived earlier");
      expect(pane.scrollTop).toBe(41);
      expect(window.getSelection()?.toString()).toBe("failure");
      expect(bracket.getAttribute("aria-valuenow")).toBe(boundary);
      const cached = cache.getQueryCache().findAll().find(query => query.queryKey.includes("kubernetes-break-logs"))?.state.data;
      expect(cached).toMatchObject({ pages: [{ lines: [line, line, line, extra], markers: page.markers }] });
      expect(pending.lines).toEqual([line, line]);
      await advance(30_000);
      expect(screen.getAllByText("failure")).toHaveLength(3);
      expect(screen.queryByText(/Waiting for logs/)).toBeNull();
      await advance(300_000); expect(read).toHaveBeenCalledTimes(3);
    });

    it("rejects a deferred final callback exactly at five minutes without rewriting cached or rendered lines", async () => {
      let finish: (value: KubernetesBreakLogPage) => void = () => undefined;
      const read = vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValue(page);
      mountPanel(); await advance(); await advance(225_000);
      read.mockImplementation(() => new Promise(resolve => { finish = resolve; }));
      await advance(60_000);
      const signal = read.mock.calls.at(-1)?.[1];
      await advance(14_999); expect(signal?.aborted).toBe(false);
      await advance(1); expect(signal?.aborted).toBe(true);
      await act(async () => finish({ ...page, lines: [{ ...line, text: "past deadline" }] }));
      await advance();
      expect(screen.queryByText("past deadline")).toBeNull();
      expect(screen.getByText("failure")).toBeTruthy();
      await advance(300_000); expect(read).toHaveBeenCalledTimes(7);
    });

    it("keeps polling in the expanded viewer and pauses while hidden without accepting the hidden callback", async () => {
      const visible = vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
      let finish: (value: KubernetesBreakLogPage) => void = () => undefined;
      const read = vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValueOnce(page)
        .mockImplementation(() => new Promise(resolve => { finish = resolve; }));
      mountPanel(); await advance();
      fireEvent.click(screen.getByRole("button", { name: "Expand logs" })); await advance();
      expect(screen.getByRole("dialog")).toBeTruthy();
      await advance(15_000); expect(read).toHaveBeenCalledTimes(2);
      const stale = finish, signal = read.mock.calls[1][1];
      visible.mockReturnValue("hidden"); fireEvent(document, new Event("visibilitychange")); await advance();
      expect(signal?.aborted).toBe(true);
      await act(async () => stale({ ...page, lines: [{ ...line, text: "hidden stale" }] }));
      await advance(30_000); expect(read).toHaveBeenCalledTimes(2);
      expect(screen.queryByText("hidden stale")).toBeNull();
      visible.mockReturnValue("visible"); fireEvent(document, new Event("visibilitychange"));
      await advance(30_000); expect(read).toHaveBeenCalledTimes(3);
      await act(async () => finish({ ...page, lines: [line, { ...line, text: "visible fresh" }] }));
      await advance();
      expect(screen.getByText("visible fresh")).toBeTruthy();
    });

    it("starts a fresh bounded automatic polling session when the real panel reopens after its first deadline", async () => {
      const read = vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValue(page);
      mountPanel(); await advance(); await advance(300_000);
      expect(read).toHaveBeenCalledTimes(7);
      fireEvent.click(screen.getByText("Toggle panel")); await advance();
      await advance(60_000); expect(read).toHaveBeenCalledTimes(7);
      fireEvent.click(screen.getByText("Toggle panel")); await advance();
      const reopened = read.mock.calls.length;
      await advance(14_999); expect(read).toHaveBeenCalledTimes(reopened);
      await advance(1); expect(read).toHaveBeenCalledTimes(reopened + 1);
    });
  });
  it("keeps settled evidence when an empty mature response omits source and container summaries", () => {
    const result = mergeBreakLogData({ pages: [page], pageParams: [undefined] }, { pages: [{ ...page, sources: [], lines: [], pods: [], containers: [], coverage: { state: "partial", tip: false, reason: "no_logs", gaps: [] } }], pageParams: [undefined] });
    expect(result.pages[0]).toMatchObject({ lines: [line], markers: page.markers, containers: page.containers, sources: page.sources, coverage: { reason: "no_logs" } });
  });
  it("does not carry placeholders across response occurrence, window or source changes", () => {
    const prior = { pages: [page], pageParams: [undefined] };
    for (const next of [{ ...page, occurrence_id: "other" }, { ...page, window: { ...page.window, from: at } }, { ...page, sources: [{ ...page.sources[0], name: "other" }] }]) {
      const incoming = { pages: [{ ...next, lines: [] }], pageParams: [undefined] };
      expect(mergeBreakLogData(prior, incoming)).toBe(incoming);
    }
  });
  it("keeps nano precision in default, explicit and boundary windows", () => {
    expect(breakLogWindow(new URLSearchParams(), at)).toEqual({ from: "2026-10-31T11:55:00.123456789Z", to: "2026-10-31T12:02:00.123456789Z" });
    const raw = { logs_from: "2026-10-31T12:00:00.123456788Z", logs_to: at };
    expect(breakLogWindow(new URLSearchParams(raw), at)).toEqual({ from: raw.logs_from, to: at });
    expect(breakLogWindow(new URLSearchParams({ logs_from: at, logs_to: "2026-10-31T14:00:00.123456790Z" }), at).to).toBe("2026-10-31T12:02:00.123456789Z");
  });
});
