// @vitest-environment jsdom
import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createElement, type ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api, ApiError } from "./api";
import { hasKubernetesPublication, kubernetesAccessLost, kubernetesReadNeedsRecovery, kubernetesReportAuthorityLost, kubernetesQueryUsesKinds, kubernetesInventoryNotReady, retainKubernetesPublication } from "./kubernetesContinuity";
import { commitStreamRetryCapMs, commitStreamRetryDelay, useKubernetesCommitEvents, useKubernetesQuery } from "./useKubernetesCluster";

const committed = { items: ["checkout"], broken: { total: 3 }, allocatable_cpu: "8", observed_at: "2026-10-28T00:00:00Z", sync: { state: "ready", age_s: 20, partial: false } };
const warming = { items: [], broken: { total: 0 }, allocatable_cpu: undefined, observed_at: "2026-10-28T00:13:00Z", sync: { state: "syncing", age_s: 800, partial: false } };
afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.useRealTimers(); });

describe("Kubernetes commit refresh", () => {
  const event = (revision: number) => ({ event: "committed", id: String(revision), data: JSON.stringify({ revision, evidence_sequence: revision, kinds: ["Pod"], cluster_id: "east" }) });
  const advance = async (ms = 1) => { await act(async () => { await vi.advanceTimersByTimeAsync(ms); }); };
  function setup() {
    const cache = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const wrapper = ({ children }: { children: ReactNode }) => createElement(QueryClientProvider, { client: cache }, children);
    return { cache, wrapper };
  }

  it("recovers first-open history after collection establishes proof without a reload or poisoning live health", async () => {
    vi.useFakeTimers();
    let collected = false;
    const notEstablished = () => new ApiError(503, "History not established", { code: "history_not_established", retryable: true });
    const stream = vi.spyOn(api, "kubernetesCommitStream").mockImplementation(emit => {
      if (!collected) return Promise.reject(notEstablished());
      emit(event(1));
      return new Promise<void>(() => undefined);
    });
    const { cache, wrapper } = setup();
    const historyRead = vi.fn(async () => { if (!collected) throw notEstablished(); return committed; });
    const { result, unmount } = renderHook(() => ({
      feed: useKubernetesCommitEvents(true),
      overview: useKubernetesQuery({ queryKey: ["kubernetes-overview"], queryFn: async () => ({ pods: 0 }), staleTime: Infinity }),
      health: useKubernetesQuery({ queryKey: ["kubernetes-sync-health"], queryFn: async () => ({ state: "ready" }), staleTime: Infinity }),
      history: useKubernetesQuery({ queryKey: ["kubernetes-timeline"], queryFn: historyRead, staleTime: Infinity }),
    }), { wrapper });
    await advance(1001);
    expect(stream).toHaveBeenCalledTimes(2);
    expect(result.current.feed.streamState).toBe("reconnecting");
    expect(result.current.overview.error).toBeNull();
    expect(result.current.health.error).toBeNull();
    expect(result.current.overview.data).toEqual({ pods: 0 });
    expect(result.current.history.data).toBeUndefined();
    collected = true;
    await advance(2101);
    expect(stream).toHaveBeenCalledTimes(3);
    expect(result.current.feed.streamState).toBe("live");
    expect(result.current.history.data?.items).toEqual(["checkout"]);
    expect(result.current.health.data?.state).toBe("ready");
    expect(result.current.overview.error).toBeNull();
    unmount();
    expect(stream.mock.calls[2][1]?.aborted).toBe(true);
    cache.clear();
  });

  it("fences a real history denial while preserving live recovery and never replays old rows before fresh authorized data", async () => {
    vi.useFakeTimers();
    const denied = () => new ApiError(403, "History denied", { code: "history_authority_denied" });
    let rejectFirst!: (error: Error) => void;
    let handshakeAllowed = false;
    let freshRows = false;
    let calls = 0;
    const stream = vi.spyOn(api, "kubernetesCommitStream").mockImplementation(emit => {
      if (++calls === 1) return new Promise<void>((_resolve, reject) => { rejectFirst = reject; });
      if (!handshakeAllowed) return Promise.reject(denied());
      emit(event(1));
      return new Promise<void>(() => undefined);
    });
    const { cache, wrapper } = setup();
    cache.setQueryData(["kubernetes", "_", "kubernetes-timeline"], committed);
    cache.setQueryData(["kubernetes", "_", "break-seed", "private"], committed);
    const read = vi.fn(async () => {
      if (!freshRows) throw new ApiError(503, "Store unavailable", { code: "history_store_unavailable" });
      return { ...committed, items: ["reauthorized"] };
    });
    const { result, unmount } = renderHook(() => ({
      feed: useKubernetesCommitEvents(true),
      overview: useKubernetesQuery({ queryKey: ["kubernetes-overview"], queryFn: async () => ({ pods: 1 }), staleTime: Infinity }),
      health: useKubernetesQuery({ queryKey: ["kubernetes-sync-health"], queryFn: async () => ({ state: "ready" }), staleTime: Infinity }),
      history: useKubernetesQuery({ queryKey: ["kubernetes-timeline"], queryFn: read, staleTime: Infinity }),
    }), { wrapper });
    await advance();
    expect(result.current.history.data?.items).toEqual(["checkout"]);
    await act(async () => { stream.mock.calls[0][0](event(9)); rejectFirst(denied()); });
    await advance(2);
    expect(result.current.history.data).toBeUndefined();
    expect(cache.getQueryData(["kubernetes", "_", "break-seed", "private"])).toBeUndefined();
    expect(result.current.overview.data).toEqual({ pods: 1 });
    expect(result.current.overview.error).toBeNull();
    expect(result.current.health.error).toBeNull();
    await advance(30100);
    expect(read).not.toHaveBeenCalled();
    expect(stream.mock.calls.length).toBeGreaterThan(1);
    stream.mock.calls[0][0](event(99));
    handshakeAllowed = true;
    await advance(31000);
    expect(read).toHaveBeenCalledTimes(1);
    expect(result.current.history.data).toBeUndefined();
    expect(cache.getQueryData(["kubernetes", "_", "break-seed", "private"])).toBeUndefined();
    freshRows = true;
    act(() => stream.mock.calls.at(-1)![0](event(2)));
    await advance(101);
    expect(result.current.history.data?.items).toEqual(["reauthorized"]);
    expect(result.current.overview.error).toBeNull();
    expect(result.current.health.error).toBeNull();
    unmount();
    cache.clear();
  });

  it("falls back without stream success and aborts connections and timers on unmount", async () => {
    vi.useFakeTimers();
    const stream = vi.spyOn(api, "kubernetesCommitStream").mockRejectedValueOnce(new Error("offline"))
      .mockImplementation(() => new Promise<void>(() => undefined));
    const read = vi.fn(async () => committed);
    const { cache, wrapper } = setup();
    const { result, unmount } = renderHook(() => ({
      feed: useKubernetesCommitEvents(true),
      page: useKubernetesQuery({ queryKey: ["kubernetes-timeline"], queryFn: read, staleTime: Infinity }),
    }), { wrapper });
    await advance();
    expect(result.current.feed.streamState).toBe("reconnecting");
    await advance(30100);
    expect(read).toHaveBeenCalledTimes(2);
    expect(stream).toHaveBeenCalledTimes(2);
    const callback = stream.mock.calls[1][0];
    unmount();
    expect(stream.mock.calls[1][1]?.aborted).toBe(true);
    callback(event(1));
    await advance(60000);
    expect(read).toHaveBeenCalledTimes(2);
    expect(stream).toHaveBeenCalledTimes(2);
    cache.clear();
  });

  it("deduplicates bursts and performs one trailing refresh without aborting slow reads", async () => {
    vi.useFakeTimers();
    const stream = vi.spyOn(api, "kubernetesCommitStream").mockImplementation(() => new Promise<void>(() => undefined));
    const { cache, wrapper } = setup();
    cache.setQueryData(["kubernetes", "_", "kubernetes-timeline"], committed);
    let finish: ((value: typeof committed) => void) | undefined;
    let signal: AbortSignal | undefined;
    let readCount = 0;
    const read = vi.fn(({ signal: nextSignal }: { signal: AbortSignal }) => {
      signal = nextSignal;
      if (++readCount === 1) return new Promise<typeof committed>(resolve => { finish = resolve; });
      return Promise.resolve(committed);
    });
    const { unmount } = renderHook(() => {
      useKubernetesCommitEvents(true);
      return useKubernetesQuery({ queryKey: ["kubernetes-timeline"], queryFn: read, staleTime: Infinity });
    }, { wrapper });
    const callback = stream.mock.calls[0][0];
    act(() => { callback(event(10)); callback(event(10)); callback(event(11)); });
    await advance(101);
    expect(read).toHaveBeenCalledTimes(1);
    act(() => { callback(event(12)); callback(event(13)); });
    await advance(101);
    expect(signal?.aborted).toBe(false);
    expect(read).toHaveBeenCalledTimes(1);
    await act(async () => { finish?.(committed); });
    await advance();
    expect(read).toHaveBeenCalledTimes(2);
    callback(event(13));
    await advance(101);
    expect(read).toHaveBeenCalledTimes(2);
    unmount();
    cache.clear();
  });

  it("resumes with the last ID but invalidates a repeated reconnect snapshot and ignores a retired callback", async () => {
    vi.useFakeTimers();
    let close: (() => void) | undefined;
    const stream = vi.spyOn(api, "kubernetesCommitStream").mockImplementation(() => new Promise<void>(resolve => { close = resolve; }));
    const { cache, wrapper } = setup();
    const read = vi.fn(async () => committed);
    const { result, unmount } = renderHook(() => ({
      feed: useKubernetesCommitEvents(true),
      page: useKubernetesQuery({ queryKey: ["kubernetes-timeline"], queryFn: read, staleTime: Infinity }),
    }), { wrapper });
    await advance();
    const first = stream.mock.calls[0][0];
    act(() => first(event(9)));
    await advance(101);
    expect(read).toHaveBeenCalledTimes(2);
    await act(async () => close?.());
    await advance(1001);
    expect(stream.mock.calls[1][2]).toBe("9");
    first(event(10));
    await advance(101);
    expect(read).toHaveBeenCalledTimes(2);
    act(() => stream.mock.calls[1][0](event(9)));
    await advance(101);
    expect(read).toHaveBeenCalledTimes(3);
    expect(result.current.feed.streamState).toBe("live");
    unmount();
    cache.clear();
  });

  it("wipes protected cache on a real stream denial, without fallback replay", async () => {
    vi.useFakeTimers();
    let deny!: (error: Error) => void;
    const stream = vi.spyOn(api, "kubernetesCommitStream").mockImplementation(() => new Promise<void>((_resolve, reject) => { deny = reject; }));
    const { cache, wrapper } = setup();
    cache.setQueryData(["kubernetes", "_", "break-seed", "private"], committed);
    const read = vi.fn(async () => committed);
    cache.setQueryData(["kubernetes", "_", "kubernetes-timeline"], committed);
    const { result } = renderHook(() => ({
      feed: useKubernetesCommitEvents(true),
      page: useKubernetesQuery({ queryKey: ["kubernetes-timeline"], queryFn: read, staleTime: Infinity }),
    }), { wrapper });
    await act(async () => { stream.mock.calls[0][0](event(1)); deny(new ApiError(403, "denied")); });
    await advance(2);
    expect(result.current.page.data).toBeUndefined();
    expect(cache.getQueryData(["kubernetes", "_", "break-seed", "private"])).toBeUndefined();
    await advance(60100);
    expect(read).not.toHaveBeenCalled();
    cache.clear();
  });
});

describe("Kubernetes publication continuity", () => {
  it("preserves sampled totals along the returned cursor chain, not an unrelated refreshed first page", async () => {
    const cache = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const wrapper = ({ children }: { children: ReactNode }) => createElement(QueryClientProvider, { client: cache }, children);
    const firstKey = ["kubernetes", "_", "kubernetes-issues", "shop", "", undefined, undefined, undefined];
    cache.setQueryData(firstKey, { ...committed, totals: { critical: 17 }, count_capped: true, next: "a" });
    const { result, rerender } = renderHook(({ cursor, namespace }) => useKubernetesQuery({
      queryKey: ["kubernetes-issues", namespace, "", undefined, undefined, cursor],
      queryFn: async () => ({ ...committed, totals: {}, count_capped: false, next: cursor === "a" ? "b" : undefined }),
    }), { wrapper, initialProps: { cursor: "a", namespace: "shop" } });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data).toMatchObject({ totals: { critical: 17 }, count_capped: true });
    cache.setQueryData(firstKey, { ...committed, totals: { critical: 2 }, count_capped: false, next: "unrelated" });
    rerender({ cursor: "b", namespace: "shop" });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data).toMatchObject({ totals: { critical: 17 }, count_capped: true });
    rerender({ cursor: "b", namespace: "other" });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data).toMatchObject({ totals: {}, count_capped: false });
    cache.clear();
  });
  it.each(["denied", "unknown"] as const)("discards stored %s authority even after a successful read", async authority => {
    const cache = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const wrapper = ({ children }: { children: ReactNode }) => createElement(QueryClientProvider, { client: cache }, children);
    let reply = { ...committed, authority: "current" as string };
    const { result } = renderHook(() => useKubernetesQuery({ queryKey: ["history"], retry: false, queryFn: async () => reply }), { wrapper });
    await waitFor(() => expect(result.current.data?.items).toEqual(["checkout"]));
    reply = { ...reply, authority };
    await act(async () => { await result.current.refetch(); });
    await waitFor(() => expect(result.current.data).toBeUndefined());
    expect(cache.getQueryData(["kubernetes", "_", "history"])).toBeUndefined();
    cache.clear();
  });

  it("keeps a previous range after failed replacement, but never across filters or authority loss", async () => {
    const cache = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const wrapper = ({ children }: { children: ReactNode }) => createElement(QueryClientProvider, { client: cache }, children);
    let failure: Error | undefined;
    const { result, rerender } = renderHook(({ filter, range }) => useKubernetesQuery({
      queryKey: ["history", filter, range], retry: false,
      queryFn: async () => { if (failure) throw failure; return committed; },
      placeholderData: (previous, query) => query?.queryKey[3] === filter ? previous : undefined,
    }), { wrapper, initialProps: { filter: "shop", range: 60 } });
    await waitFor(() => expect(result.current.data?.items).toEqual(["checkout"]));
    failure = new ApiError(503, "store unavailable");
    rerender({ filter: "shop", range: 120 });
    await waitFor(() => expect(result.current.isError).toBe(true));
    expect(result.current.data?.items).toEqual(["checkout"]);
    failure = new ApiError(403, "mismatched occurrence", { code: "authority_mismatch" });
    await act(async () => { await result.current.refetch(); });
    await waitFor(() => expect(result.current.data).toBeUndefined());
    failure = new ApiError(504, "timeout");
    await act(async () => { await result.current.refetch(); });
    expect(result.current.data).toBeUndefined();
    rerender({ filter: "other", range: 120 });
    await waitFor(() => expect(result.current.isFetching).toBe(false));
    expect(result.current.data).toBeUndefined();
    cache.clear();
  });
  it("requires published-cluster and committed projection proof for an initial partial timeout", () => {
    const partial = { items: [{ kind: "Deployment", namespace: "shop", name: "checkout" }], counts: { Deployment: 1, Job: 0 }, sync: { state: "error", partial: true },
      partial_failures: [{ resource_id: "batch~v1~cronjobs", class: "timeout" }] };
    expect(hasKubernetesPublication(partial, committed.observed_at)).toBe(true);
    expect(hasKubernetesPublication({ ...partial, items: [], counts: { Deployment: 0 } }, committed.observed_at)).toBe(true);
    for (const value of [
      partial,
      { ...partial, counts: {} },
      { ...partial, counts: { Unknown: 1 } },
      { ...partial, counts: { Deployment: -1 } },
      { ...partial, items: ["untrusted"] },
      { ...partial, items: [{ kind: "CronJob", name: "unobserved", namespace: "shop" }] },
      { ...partial, sync: { state: "syncing", partial: true }, items: [] },
      { ...partial, sync: { state: "error", partial: false } },
      { ...partial, partial_failures: [] },
      ...["forbidden", "unauthorized", "namespace_scope", "credential_unavailable", "unknown"].map(className =>
        ({ ...partial, partial_failures: [{ resource_id: "batch~v1~cronjobs", class: className }] })),
    ]) expect(hasKubernetesPublication(value, value === partial ? undefined : committed.observed_at)).toBe(false);
    expect(hasKubernetesPublication(partial, "invalid")).toBe(false);
  });

  it("withholds an overview that could read none of its kinds", () => {
    // The server reports a partial denial as ready+partial; error+partial means
    // no requested kind was readable, so the zero counts are not a publication.
    const refused = ["core~v1~nodes", "core~v1~pods", "core~v1~namespaces", "apps~v1~deployments", "apps~v1~statefulsets", "apps~v1~daemonsets", "batch~v1~jobs", "batch~v1~cronjobs"]
      .map((resource_id) => ({ resource_id, class: "forbidden" }));
    const overview = { connector: "kubernetes", cluster_id: "limited", observed_at: committed.observed_at, nodes: 0, pods: 0, namespaces: 0, workloads: 0,
      sync: { state: "error", age_s: 30, partial: true }, partial_failures: refused };
    expect(hasKubernetesPublication(overview, committed.observed_at)).toBe(false);
    expect(hasKubernetesPublication({ ...overview, sync: { state: "ready", age_s: 30, partial: true }, partial_failures: refused.slice(0, 1) }, committed.observed_at)).toBe(true);
  });

  it("admits a published cluster's first overview answer even while its cycle reports an error", () => {
    const overview = { connector: "kubernetes", cluster_id: "east", observed_at: committed.observed_at, nodes: 3, pods: 12, namespaces: 2, workloads: 4,
      sync: { state: "error", age_s: 420, partial: true } };
    expect(hasKubernetesPublication(overview, committed.observed_at)).toBe(true);
    // Without a first publication, zero counts, a zero time or a refusal, it still proves nothing.
    expect(hasKubernetesPublication(overview)).toBe(false);
    expect(hasKubernetesPublication({ ...overview, nodes: 0, pods: 0, workloads: 0 }, committed.observed_at)).toBe(false);
    expect(hasKubernetesPublication({ ...overview, observed_at: "0001-01-01T00:00:00Z" }, committed.observed_at)).toBe(false);
    expect(hasKubernetesPublication({ ...overview, partial_failures: [{ resource_id: "core~v1~pods", class: "forbidden" }] }, committed.observed_at)).toBe(false);
  });

  it("admits only the currently scoped first partial response and hides permission denial", async () => {
    const cache = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const partial = { items: [{ kind: "Deployment", namespace: "shop", name: "checkout" }], counts: { Deployment: 1, Job: 0 }, sync: { state: "error", partial: true },
      partial_failures: [{ resource_id: "batch~v1~cronjobs", class: "timeout" }] };
    let reply: typeof partial | Error = partial;
    const wrapper = ({ children }: { children: ReactNode }) => createElement(QueryClientProvider, { client: cache }, children);
    const { result, rerender } = renderHook(({ scope, proof }: { scope: string; proof?: string }) =>
      useKubernetesQuery({ queryKey: ["kubernetes-workloads", scope], queryFn: async () => {
        if (reply instanceof Error) throw reply;
        return reply;
      } }, proof), { wrapper, initialProps: { scope: "shop", proof: undefined as string | undefined } });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data).toBeUndefined();
    rerender({ scope: "shop", proof: committed.observed_at });
    expect(result.current.data?.counts).toEqual({ Deployment: 1, Job: 0 });
    rerender({ scope: "other", proof: undefined });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data).toBeUndefined();
    rerender({ scope: "shop", proof: committed.observed_at });
    reply = new ApiError(403, "Permission denied");
    await act(async () => { await result.current.refetch(); });
    await waitFor(() => expect(result.current.data).toBeUndefined());
    expect(cache.getQueryData(["kubernetes", "_", "kubernetes-workloads", "shop"])).toBeUndefined();
    cache.clear();
  });

  it("preserves committed rows, capacity and observation time across repeated successful warming and failed publications", () => {
    const refreshing = retainKubernetesPublication(committed, warming);
    const failed = retainKubernetesPublication(refreshing, { ...warming, sync: { state: "error", age_s: 900, partial: true } });
    expect(failed).toMatchObject({ items: ["checkout"], broken: { total: 3 }, allocatable_cpu: "8", observed_at: committed.observed_at, sync: { state: "error", age_s: 900 } });
    expect(hasKubernetesPublication(failed)).toBe(true);
    expect(hasKubernetesPublication(warming)).toBe(false);
  });

  it("keeps unavailable metrics at their committed observation time and marks retained usage stale", () => {
    const metrics = { availability: "available", fresh: true, complete: true, total: 3, cpu: "4", observed_at: committed.observed_at };
    const previous = { availability: "available", fresh: true, observed_at: committed.observed_at, pod_metrics: metrics, node_metrics: metrics };
    const result = retainKubernetesPublication(previous, { ...previous, observed_at: warming.observed_at, availability: "unavailable" });
    expect(result).toMatchObject({ observed_at: committed.observed_at, availability: "stale", fresh: false, pod_metrics: { cpu: "4", availability: "stale", fresh: false } });
  });

  it("replaces prior rows with a genuine complete empty publication", () => {
    const result = retainKubernetesPublication(committed, { ...warming, sync: committed.sync });
    expect(result.items).toEqual([]);
    expect(result.broken.total).toBe(0);
    expect(result.observed_at).toBe(warming.observed_at);
  });

  it("does not merge a successfully permission-reduced projection with old protected values", () => {
    const reduced = { ...warming, sync: { state: "partial", age_s: 1, partial: true }, partial_failures: [{ class: "forbidden" }] };
    expect(retainKubernetesPublication(committed, reduced)).toBe(reduced);
    expect(kubernetesAccessLost(new ApiError(502, "unavailable", { error: { code: "credential_unavailable" } }))).toBe(true);
    expect(kubernetesAccessLost(new ApiError(503, "publication failed"))).toBe(false);
  });

  it("recognizes only structured not-ready responses, never matching error text or other failures", () => {
    expect(kubernetesInventoryNotReady(new ApiError(503, "different wording", { code: "inventory_not_ready", retryable: true }))).toBe(true);
    expect(kubernetesInventoryNotReady(new ApiError(503, "different wording", { error: { code: "inventory_not_ready" } }))).toBe(true);
    for (const error of [
      new ApiError(503, "Kubernetes inventory has not finished syncing"),
      new ApiError(503, "Kubernetes inventory has not finished syncing", { code: "unavailable" }),
      new ApiError(500, "Kubernetes inventory has not finished syncing", { code: "inventory_not_ready" }),
      new ApiError(403, "Kubernetes inventory has not finished syncing", { code: "inventory_not_ready" }),
      new Error("Kubernetes inventory has not finished syncing"),
    ]) expect(kubernetesInventoryNotReady(error)).toBe(false);
  });

  it("recognizes explicit cycle authority failures without confusing isolated gaps or network faults", () => {
    const kind = { state: "error" as const, partial: true, records: 4, error: "forbidden", cycle_failure: true };
    const report = { state: "error", partial: true, age_s: 20, kinds: { Pod: kind }, total_kinds: 1, ready_kinds: 0, failing_kinds: 1, pending_kinds: 0, truncated: false };
    expect(kubernetesReportAuthorityLost(report)).toBe(true);
    expect(kubernetesReportAuthorityLost({ ...report, kinds: { Pod: { ...kind, error: "unauthorized" } } })).toBe(true);
    expect(kubernetesReportAuthorityLost({ ...report, kinds: {}, class: "forbidden" })).toBe(true);
    expect(kubernetesReportAuthorityLost({ ...report, kinds: { Pod: { ...kind, error: "unavailable" } } })).toBe(false);
    expect(kubernetesReportAuthorityLost({ ...report, kinds: { Pod: { ...kind, absent: true } } })).toBe(false);
    expect(kubernetesReportAuthorityLost({ ...report, state: "ready", failing_kinds: 0, kinds: { Secret: { ...kind, cycle_failure: false } }, unreadable: [{ kind: "Secret", class: "forbidden" }] })).toBe(false);
  });

  it("targets denied-kind identities and mixed dependencies, not unrelated rows or coincidental namespace text", () => {
    const denied = new Set(["Secret"]);
    expect(kubernetesQueryUsesKinds(["kubernetes-detail", "core~v1~secrets", "shop", "settings"], undefined, denied)).toBe(true);
    expect(kubernetesQueryUsesKinds(["kubernetes-graph"], { nodes: [{ kind: "Secret" }, { kind: "Pod" }] }, denied)).toBe(true);
    expect(kubernetesQueryUsesKinds(["kubernetes-graph"], undefined, denied)).toBe(true);
    expect(kubernetesQueryUsesKinds(["kubernetes-workloads", "Secret"], { items: [{ kind: "Deployment" }] }, denied)).toBe(false);
    expect(kubernetesQueryUsesKinds(["kubernetes-workloads"], undefined, denied)).toBe(false);
    expect(kubernetesQueryUsesKinds(["kubernetes-graph"], undefined, new Set())).toBe(false);
  });

  it("fences workload counts and unfinished mixed workload projections even when the displayed page has no denied-kind rows", () => {
    const denied = new Set(["Pod"]);
    expect(kubernetesQueryUsesKinds(["kubernetes-workloads", "shop", "", "Deployment"], undefined, denied)).toBe(true);
    expect(kubernetesQueryUsesKinds(["kubernetes-workloads"], { items: [{ kind: "Deployment" }], counts: { Pod: 9, Deployment: 1 } }, denied)).toBe(true);
    expect(kubernetesQueryUsesKinds(["kubernetes-summary"], { counts: { Pod: 0 } }, denied)).toBe(true);
    expect(kubernetesQueryUsesKinds(["kubernetes-workloads"], undefined, new Set(["Secret"]))).toBe(false);
  });

  it("rejects a malformed empty sync envelope without inventing a publication", () => {
    const malformed = { ...warming, sync: { ...warming.sync, state: "" } };
    expect(hasKubernetesPublication(malformed)).toBe(false);
    const retained = retainKubernetesPublication(committed, malformed);
    expect(retained.items).toEqual(committed.items);
    expect(retained.observed_at).toBe(committed.observed_at);
    expect(hasKubernetesPublication(retained)).toBe(true);
    expect(retainKubernetesPublication(retained, { ...malformed, sync: committed.sync }).items).toEqual([]);
  });

  it("retries only unfinished envelopes and explicitly retryable inventory-not-ready errors", () => {
    const malformed = { ...warming, sync: { ...warming.sync, state: "" } };
    expect(kubernetesReadNeedsRecovery(malformed, null)).toBe(true);
    expect(kubernetesReadNeedsRecovery(warming, null)).toBe(true);
    expect(kubernetesReadNeedsRecovery({ ...warming, sync: committed.sync }, null)).toBe(false);
    expect(kubernetesReadNeedsRecovery(undefined, new ApiError(503, "not ready", { code: "inventory_not_ready", retryable: true }))).toBe(true);
    for (const error of [
      new ApiError(503, "not ready", { code: "inventory_not_ready", retryable: false }),
      new ApiError(503, "storage unavailable"),
      new ApiError(503, "credential unavailable", { code: "credential_unavailable", retryable: true }),
      new ApiError(403, "permission reduced", { code: "inventory_not_ready", retryable: true }),
      new ApiError(401, "signed out"),
    ]) expect(kubernetesReadNeedsRecovery(malformed, error)).toBe(false);
    expect(kubernetesReadNeedsRecovery({ ...malformed, partial_failures: [{ class: "forbidden" }] }, null)).toBe(false);
  });

  it.each(["syncing", ""])("uses same-key committed data through faults, but a denial fences subsequent %s envelopes", async (state) => {
    const cache = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const unfinished = { ...warming, sync: { ...warming.sync, state } };
    let reply: typeof committed | typeof warming | Error = committed;
    const wrapper = ({ children }: { children: ReactNode }) => createElement(QueryClientProvider, { client: cache }, children);
    const { result, rerender } = renderHook(({ scope }) => useKubernetesQuery({ queryKey: ["issues", scope], queryFn: async () => { if (reply instanceof Error) throw reply; return reply; }, retry: false }), { wrapper, initialProps: { scope: "shop" } });
    await waitFor(() => expect(result.current.data?.items).toEqual(["checkout"]));
    reply = unfinished;
    await act(async () => { await result.current.refetch(); });
    expect(result.current.data?.broken.total).toBe(3);
    reply = new ApiError(503, "storage unavailable");
    await act(async () => { await result.current.refetch(); });
    await waitFor(() => expect(result.current.isError).toBe(true));
    expect(result.current.data?.observed_at).toBe(committed.observed_at);
    reply = new ApiError(403, "permission reduced");
    await act(async () => { await result.current.refetch(); });
    await waitFor(() => expect(result.current.data).toBeUndefined());
    expect(cache.getQueryData(["kubernetes", "_", "issues", "shop"])).toBeUndefined();
    reply = unfinished;
    await act(async () => { await result.current.refetch(); });
    expect(result.current.data).toBeUndefined();
    reply = committed;
    await act(async () => { await result.current.refetch(); });
    await waitFor(() => expect(result.current.data?.broken.total).toBe(3));
    reply = unfinished;
    rerender({ scope: "other" });
    await waitFor(() => expect(result.current.isFetching).toBe(false));
    expect(result.current.data).toBeUndefined();
    cache.clear();
  });
});

describe("commit stream reconnect delay", () => {
  it("doubles from one second and spreads retries at the cap", () => {
    expect([0, 1, 2, 3, 4, 5].map(failures => commitStreamRetryDelay(failures))).toEqual([1000, 2000, 4000, 8000, 16000, 32000]);
    expect(commitStreamRetryDelay(6, () => 0)).toBe(commitStreamRetryCapMs * 0.8);
    expect(commitStreamRetryDelay(40, () => 1)).toBe(commitStreamRetryCapMs);
    expect(commitStreamRetryDelay(-3)).toBe(1000);
  });
});
