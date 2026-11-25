// @vitest-environment jsdom
import { act, cleanup, renderHook } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createElement, type ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api, ApiError } from "./api";
import type { ServerSentEvent } from "./sse";
import { commitRefreshIntervalMs, useKubernetesCommitEvents, useKubernetesQuery } from "./useKubernetesCluster";

// How many requests the open Kubernetes page makes when the commit feed
// recovers, repeats an unchanged sync state, or reconnects at the end of its
// lease. Each read is counted by family; the feed is a scripted fake.

afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.useRealTimers(); });

const pageFamilies = [
  "kubernetes-overview", "kubernetes-sync-health", "kubernetes-issues", "kubernetes-changes",
  "kubernetes-workloads", "kubernetes-top", "kubernetes-graph", "kubernetes-namespaces",
] as const;
type Family = typeof pageFamilies[number];

const committed = (revision: number): ServerSentEvent => ({
  event: "committed", id: String(revision),
  data: JSON.stringify({ revision, evidence_sequence: revision, kinds: ["Pod"], cluster_id: "east" }),
});
const sync = (state: string, reason = ""): ServerSentEvent => ({
  event: "sync", data: JSON.stringify({ state, reason, age: 1, cluster_id: "east" }),
});

const advance = async (ms: number) => { await act(async () => { await vi.advanceTimersByTimeAsync(ms); }); };

/** One fake commit feed connection: what the test sends, and how it ends. */
interface Connection {
  emit: (event: ServerSentEvent) => void;
  close: () => void;
}

function mountPage(options: { frozen?: () => boolean } = {}) {
  const frozen = options.frozen ?? (() => false);
  const connections: Connection[] = [];
  vi.spyOn(api, "kubernetesCommitStream").mockImplementation(async (emit) => {
    if (frozen()) throw new ApiError(503, "Kubernetes commit stream unavailable.", { code: "history_store_unavailable", retryable: true });
    await new Promise<void>((resolve) => { connections.push({ emit, close: resolve }); });
  });
  const counts = Object.fromEntries(pageFamilies.map((family) => [family, 0])) as Record<Family, number>;
  const read = (family: Family) => async () => {
    counts[family]++;
    if (frozen()) throw new ApiError(503, "Store unavailable", { code: "history_store_unavailable", retryable: true });
    return { family, sync: { state: "ready", age_s: 1, partial: false } };
  };
  const cache = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrapper = ({ children }: { children: ReactNode }) => createElement(QueryClientProvider, { client: cache }, children);
  const view = renderHook(() => {
    const feed = useKubernetesCommitEvents(true);
    useKubernetesQuery({ queryKey: [pageFamilies[0]], queryFn: read(pageFamilies[0]) });
    useKubernetesQuery({ queryKey: [pageFamilies[1]], queryFn: read(pageFamilies[1]) });
    useKubernetesQuery({ queryKey: [pageFamilies[2]], queryFn: read(pageFamilies[2]) });
    useKubernetesQuery({ queryKey: [pageFamilies[3]], queryFn: read(pageFamilies[3]) });
    useKubernetesQuery({ queryKey: [pageFamilies[4]], queryFn: read(pageFamilies[4]) });
    useKubernetesQuery({ queryKey: [pageFamilies[5]], queryFn: read(pageFamilies[5]) });
    useKubernetesQuery({ queryKey: [pageFamilies[6]], queryFn: read(pageFamilies[6]) });
    useKubernetesQuery({ queryKey: [pageFamilies[7]], queryFn: read(pageFamilies[7]) });
    return feed;
  }, { wrapper });
  const reset = () => { for (const family of pageFamilies) counts[family] = 0; };
  const send = (event: ServerSentEvent) => act(() => connections.at(-1)!.emit(event));
  return { ...view, cache, connections, counts, reset, send };
}

describe("Kubernetes commit feed refresh budget", () => {
  it("asks each page read at most once when the feed recovers with a backlog of commits", async () => {
    vi.useFakeTimers();
    let isFrozen = false;
    const page = mountPage({ frozen: () => isFrozen });
    await advance(10);
    page.send(committed(1));
    page.send(sync("ready"));
    await advance(commitRefreshIntervalMs * 2);
    expect(page.result.current.streamState).toBe("live");

    // The database freezes: the open stream ends and every reconnect fails.
    isFrozen = true;
    await act(async () => page.connections.at(-1)!.close());
    await advance(25_000);
    expect(page.result.current.streamState).toBe("reconnecting");
    const opened = page.connections.length;

    // It thaws. The next reconnect opens on a newer generation while the
    // collector catches up: a commit and a changing sync state every second.
    isFrozen = false;
    for (let waited = 0; page.connections.length === opened && waited < 120_000; waited += 100) await advance(100);
    expect(page.connections.length).toBe(opened + 1);
    page.reset();
    page.send(committed(2));
    page.send(sync("refreshing", "collection_stale"));
    let revision = 2;
    for (let second = 1; second < 5; second++) {
      await advance(1_000);
      page.send(committed(++revision));
      page.send(sync(second < 3 ? "refreshing" : "ready", second < 3 ? "collection_stale" : ""));
    }
    await advance(900);
    // Within the first refresh interval after the recovery each read is asked
    // once; the sync report once more only for its own change of state.
    for (const family of pageFamilies) {
      expect(page.counts[family], family).toBeLessThanOrEqual(family === "kubernetes-sync-health" ? 2 : 1);
    }
    for (let second = 5; second < 7; second++) {
      page.send(committed(++revision));
      page.send(sync("ready"));
      await advance(1_000);
    }
    // Over the whole 7 s: at most one more refresh for the commits that kept
    // arriving, never one per commit.
    for (const family of pageFamilies) {
      expect(page.counts[family], family).toBeGreaterThanOrEqual(1);
      expect(page.counts[family], family).toBeLessThanOrEqual(family === "kubernetes-sync-health" ? 3 : 2);
    }
    const total = pageFamilies.reduce((sum, family) => sum + page.counts[family], 0);
    expect(total).toBeLessThanOrEqual(pageFamilies.length * 2 + 1);
    page.unmount();
    page.cache.clear();
  });

  it("does not read the sync report again while the feed repeats an unchanged state", async () => {
    vi.useFakeTimers();
    const page = mountPage();
    await advance(10);
    page.send(committed(1));
    page.send(sync("ready"));
    await advance(commitRefreshIntervalMs * 2);
    page.reset();
    for (let second = 0; second < 20; second++) {
      page.send(sync("ready"));
      await advance(1_000);
    }
    expect(page.counts["kubernetes-sync-health"]).toBe(0);
    // A change of state, or of its cause, is read once.
    page.send(sync("refreshing", "collection_stale"));
    await advance(1_000);
    expect(page.counts["kubernetes-sync-health"]).toBe(1);
    for (let second = 0; second < 5; second++) {
      page.send(sync("refreshing", "collection_stale"));
      await advance(1_000);
    }
    expect(page.counts["kubernetes-sync-health"]).toBe(1);
    page.send(sync("refreshing", "collection_failed"));
    await advance(1_000);
    expect(page.counts["kubernetes-sync-health"]).toBe(2);
    for (const family of pageFamilies) if (family !== "kubernetes-sync-health") expect(page.counts[family], family).toBe(0);
    page.unmount();
    page.cache.clear();
  });

  it("refetches nothing when a lease reconnect opens on the same generation", async () => {
    vi.useFakeTimers();
    const page = mountPage();
    await advance(10);
    page.send(committed(5));
    page.send(sync("ready"));
    await advance(commitRefreshIntervalMs * 2);
    page.reset();
    for (let lease = 0; lease < 3; lease++) {
      await advance(30_000);
      await act(async () => page.connections.at(-1)!.close());
      await advance(10);
      expect(page.connections).toHaveLength(lease + 2);
      // The server opens every connection with the current generation.
      page.send(committed(5));
      page.send(sync("ready"));
      await advance(1_000);
      page.send(sync("ready"));
      await advance(commitRefreshIntervalMs);
    }
    for (const family of pageFamilies) expect(page.counts[family], family).toBe(0);
    expect(page.result.current.streamState).toBe("live");
    // A reconnect on a newer generation is refreshed for, once.
    await act(async () => page.connections.at(-1)!.close());
    await advance(10);
    page.send(committed(6));
    page.send(sync("ready"));
    await advance(commitRefreshIntervalMs);
    for (const family of pageFamilies) expect(page.counts[family], family).toBe(1);
    page.unmount();
    page.cache.clear();
  });
  it("brings a read that started in the middle of a burst up to the burst's last commit", async () => {
    vi.useFakeTimers();
    const emits: Array<(event: ServerSentEvent) => void> = [];
    vi.spyOn(api, "kubernetesCommitStream").mockImplementation(async (emit) => {
      emits.push(emit);
      await new Promise<void>(() => undefined);
    });
    // The server answers with the revision it had when the read started, and
    // takes 1.5 s to answer.
    let serverRevision = 1;
    let reads = 0;
    const cache = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const wrapper = ({ children }: { children: ReactNode }) => createElement(QueryClientProvider, { client: cache }, children);
    const view = renderHook(() => {
      useKubernetesCommitEvents(true);
      return useKubernetesQuery({ queryKey: ["kubernetes-issues"], queryFn: async () => {
        reads++;
        const revision = serverRevision;
        await new Promise((resolve) => setTimeout(resolve, 1_500));
        return { revision };
      } });
    }, { wrapper });
    const send = (event: ServerSentEvent) => act(() => emits.at(-1)!(event));
    await advance(10);
    // The first commit lands while the mount read is in flight, so that read
    // is asked again when it settles.
    send(committed(1));
    await advance(500);
    serverRevision = 2;
    send(committed(2));
    // The mount read settles and is asked again: that read starts after
    // commit 2 and answers revision 2.
    await advance(1_200);
    serverRevision = 3;
    // Commit 3 lands while that read is in flight, inside the same spacing
    // window.
    send(committed(3));
    await advance(commitRefreshIntervalMs * 4);
    expect((view.result.current.data as { revision: number }).revision).toBe(3);
    // Mount, the re-ask after the first commit, and one refresh for the rest.
    expect(reads).toBeLessThanOrEqual(3);
    view.unmount();
    cache.clear();
  });
});
