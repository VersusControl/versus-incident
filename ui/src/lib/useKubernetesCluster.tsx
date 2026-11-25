import { createContext, useContext, useEffect, useLayoutEffect, useRef, useState } from "react";
import { useQuery, useQueryClient, type UseQueryOptions } from "@tanstack/react-query";
import { api, ApiError, createKubernetesApi, retainKubernetesHistoryCounts, type KubernetesClusters, type KubernetesCrashQuery } from "./api";
import { useCursorPagination } from "./pagination";
import { hasKubernetesPublication, kubernetesAccessLost, kubernetesCursorExpired, kubernetesFailureCode, retainKubernetesPublication } from "./kubernetesContinuity";
import { focusedCrashes } from "./kubernetesFocusedHistory";
import { historyOff } from "./kubernetesNamespaceAccess";
import { kubernetesFailureBackingOff, kubernetesFailureRefetchInterval } from "./kubernetesFailureBackoff";

export function kubernetesKey(cluster: string | undefined, ...parts: readonly unknown[]) {
  return ["kubernetes", cluster ?? "_", ...parts] as const;
}

export interface ClusterContext {
  multiple: boolean;
  cluster?: string;
  clusters: KubernetesClusters["clusters"];
  client: ReturnType<typeof createKubernetesApi>;
  key: (...parts: readonly unknown[]) => readonly unknown[];
}

export const KubernetesClusterContext = createContext<ClusterContext | null>(null);

/**
 * When this cluster first published a ready generation, once it has. The
 * blocking "preparing" panels belong to a cluster that has never published
 * anything; a per-view refetch must never bring them back after that.
 */
export const KubernetesFirstReadyContext = createContext<string | undefined>(undefined);

export function useKubernetesFirstReady(): string | undefined {
  return useContext(KubernetesFirstReadyContext);
}


const singleContext: ClusterContext = { multiple: false, clusters: [], client: api, key: (...parts) => kubernetesKey(undefined, ...parts) };

export function useKubernetesCluster(): ClusterContext {
  return useContext(KubernetesClusterContext) ?? singleContext;
}

export function useKubernetesQuery<T>(options: UseQueryOptions<T, Error, T, readonly unknown[]>, firstReadyAt?: string) {
  const { key } = useKubernetesCluster();
  const cache = useQueryClient();
  const queryFn = options.queryFn;
  const query = useQuery({
    ...options,
    queryFn: typeof queryFn === "function" ? async (context) => {
      const data = await queryFn(context);
      const authority = (data as { authority?: string } | undefined)?.authority;
      if (authority === "denied" || authority === "unknown") throw new ApiError(403, "Stored evidence authority is unavailable.", { code: `authority_${authority}` });
      const parts = [...options.queryKey];
      const cursorIndex = parts[0] === "kubernetes-timeline" && parts[1] === "list" ? parts.length - 1
        : parts[0] === "kubernetes-issues" && parts.length === 6 ? 5
          : parts[0] === "kubernetes-crashes" && parts.length === 3 ? 2 : undefined;
      if (cursorIndex !== undefined && parts[cursorIndex]) {
        const cursor = parts[cursorIndex];
        const predecessor = cache.getQueryCache().findAll({
          queryKey: key(...parts.slice(0, cursorIndex)),
          predicate: query => !kubernetesAccessLost(query.state.error)
            && (query.state.data as { next?: string } | undefined)?.next === cursor,
        })[0];
        const currentKey = key(...parts);
        const prior = predecessor?.state.data ?? (!kubernetesAccessLost(cache.getQueryState(currentKey)?.error) ? cache.getQueryData(currentKey) : undefined);
        return retainKubernetesHistoryCounts(prior, data);
      }
      return data;
    } : queryFn,
    queryKey: key(...options.queryKey),
    // A read that keeps failing backs off instead of asking at its normal
    // cadence; one the server says not to retry waits for the reader.
    refetchInterval: (current) => kubernetesFailureRefetchInterval(current,
      typeof options.refetchInterval === "function" ? options.refetchInterval(current) : options.refetchInterval),
    structuralSharing: (previous, incoming) => retainKubernetesPublication(
      kubernetesAccessLost(cache.getQueryState(key(...options.queryKey))?.error) ? undefined : previous as T | undefined,
      incoming as T,
    ),
  });
  const scopedKey = JSON.stringify(key(...options.queryKey));
  // A read with no data that is asked again goes back to "pending" until it
  // answers. Its failure is still the truth on screen meanwhile: dropping it
  // would swap the error for a loader, unmount whatever the failure gates and
  // mount it again — and every remount asks its own failing reads once more.
  // A reset read (the reader's fresh start after lost access) forgets its
  // failure, so only the failure the cache still records is held.
  const lastFailure = useRef<{ key: string; at: number; error: Error }>();
  if (query.error) lastFailure.current = { key: scopedKey, at: query.errorUpdatedAt, error: query.error };
  else if (!query.isPending || !query.isFetching) lastFailure.current = undefined;
  const heldFailure = query.isPending && query.isFetching && query.errorUpdatedAt > 0
    && lastFailure.current?.key === scopedKey && lastFailure.current.at === query.errorUpdatedAt ? lastFailure.current.error : undefined;
  const settled = heldFailure ? {
    ...query, status: "error" as const, error: heldFailure, isError: true as const, isPending: false as const, isLoading: false as const,
    isLoadingError: true as const, isRefetchError: false as const, isSuccess: false as const, isRefetching: true,
  } : query;
  const accessLost = kubernetesAccessLost(settled.error);
  const authorityFence = cache.getQueryData<symbol>(key("kubernetes-authority-fence"));
  const lastGood = useRef<{ key: string; data: T; at: number; fence: symbol | undefined }>();
  if (lastGood.current && lastGood.current.fence !== authorityFence) lastGood.current = undefined;
  const invalidRequest = settled.error instanceof ApiError && [400, 409, 410, 422].includes(settled.error.status);
  if (accessLost || invalidRequest) lastGood.current = undefined;
  else if (query.data !== undefined) lastGood.current = { key: scopedKey, data: query.data, at: query.dataUpdatedAt || lastGood.current?.at || Date.now(), fence: authorityFence };
  const retained = query.data ?? (settled.isError && lastGood.current?.key === scopedKey ? lastGood.current.data : undefined);
  useEffect(() => {
    if (!accessLost) return;
    // Keep the authoritative error, but discard its now-unauthorized cache.
    cache.getQueryCache().find({ queryKey: JSON.parse(scopedKey) as readonly unknown[], exact: true })?.setState({ data: undefined });
  }, [accessLost, cache, scopedKey]);
  const missingPublication = query.data !== undefined && !hasKubernetesPublication(query.data, firstReadyAt);
  const unpublished = missingPublication && (query.data as { sync?: { state?: string } }).sync?.state === "syncing";
  const hidden = accessLost || missingPublication;
  return { ...settled, unpublished, data: hidden ? undefined : retained, dataUpdatedAt: query.dataUpdatedAt || lastGood.current?.at || 0 };
}

/**
 * How a cursor-paged list answers the server refusing the current page's link
 * because the list it pointed into changed:
 * - "reset": the reader just asked for this page, so the list goes back to
 *   page 1 on its own.
 * - "notice": the page was already on screen and a background or live
 *   refresh was refused. Its rows stay, with an offer to go back to page 1;
 *   moving the reader without asking would lose their place.
 * Either way the view shows no error. The first page sends no cursor, so a
 * refusal there is never read as expiry and cannot loop. A page counts as on
 * screen only once its own read has finished since the reader arrived, so a
 * cached page that is being read again still resets.
 */
export type KubernetesCursorExpiry = "reset" | "notice" | undefined;

export function useKubernetesCursorReset(
  paging: { cursor?: string; resetPage: () => void },
  read: { error: unknown; isFetching: boolean; isSuccess: boolean; isPlaceholderData: boolean },
): KubernetesCursorExpiry {
  const { cursor, resetPage } = paging;
  // Which page the reader is on, and whether its own read has finished since
  // they arrived. Leaving a page forgets it, so returning to it counts as a
  // new arrival.
  const [visit, setVisit] = useState<{ cursor?: string; loaded: boolean }>({ cursor, loaded: false });
  if (visit.cursor !== cursor) setVisit({ cursor, loaded: false });
  else if (cursor && !visit.loaded && read.isSuccess && !read.isPlaceholderData && !read.isFetching) setVisit({ cursor, loaded: true });
  const arrived = visit.cursor !== cursor || !visit.loaded;
  // A refusal on a page that has not loaded yet may be a cached answer being
  // read again, so it waits for that read. On a page already on screen the
  // refusal stays in force while later refreshes run, so their wait never
  // turns the notice into an error.
  const expired = Boolean(cursor) && kubernetesCursorExpired(read.error) && (!arrived || !read.isFetching);
  const expiry: KubernetesCursorExpiry = !expired ? undefined : arrived ? "reset" : "notice";
  // Before paint, so the refusal never flashes as an error.
  useLayoutEffect(() => {
    if (expiry === "reset") resetPage();
  }, [expiry, resetPage]);
  return expiry;
}

export function useBrokenIssues(enabled = true) {
  const { client, cluster } = useKubernetesCluster();
  return useKubernetesQuery({ queryKey: ["kubernetes-issues", "broken", cluster], queryFn: ({ signal }) => client.kubernetesIssues({ broken: true, include: "changes", limit: 5 }, signal), enabled, retry: false });
}

/** One commit feed per mounted explorer; missed commits converge by invalidation. */
/**
 * For an identity that reads only inside namespaces the server keeps no
 * history, and the feed's first `committed` event says
 * `history: "off_namespace_access"`. Live updates are then off by design: the
 * state is "polling", never "reconnecting", and the views refresh on the
 * fallback timer. `namespaceOnly` keeps an older server's refusal of the feed
 * (403 `history_authority_denied`) in the same state rather than a lost-access
 * event.
 */
export function useKubernetesCommitEvents(enabled: boolean, namespaceOnly = false) {
  const { client, key, cluster } = useKubernetesCluster();
  const cache = useQueryClient();
  const [state, setState] = useState<"connecting" | "live" | "reconnecting" | "polling">("connecting");
  const [sync, setSync] = useState<{ state: string; reason?: string; age: number }>();
  useEffect(() => {
    if (!enabled) return;
    let disposed = false;
    let connected = false;
    let lastID: string | undefined;
    let retry: ReturnType<typeof setTimeout> | undefined;
    let flush: ReturnType<typeof setTimeout> | undefined;
    let failures = 0;
    let denied = false;
    let recoveryOnly = false;
    // The server says live updates are off for this identity: the feed stays
    // open, and the periodic refresh is the source of truth.
    let liveOff = false;
    let controller: AbortController | undefined;
    // The visible feed state follows the stream only through `show`. While the
    // stream is between connections a grace timer holds the current state; it
    // turns into "reconnecting" only when no event has arrived before it fires.
    let grace: ReturnType<typeof setTimeout> | undefined;
    const hold = () => {
      if (grace) return;
      grace = setTimeout(() => {
        grace = undefined;
        if (!disposed && !connected) setState("reconnecting");
      }, commitStreamGraceMs);
    };
    const release = () => { clearTimeout(grace); grace = undefined; };
    const families = new Set([
      "kubernetes-overview", "kubernetes-sync-health", "kubernetes-graph", "kubernetes-overview-topology",
      "kubernetes-namespaces", "kubernetes-workloads", "kubernetes-nodes", "kubernetes-node-pods",
      "kubernetes-issues", "kubernetes-top", "kubernetes-diagnosis", "kubernetes-drawer-changes",
      "kubernetes-changes", "kubernetes-crashes", "kubernetes-timeline", "kubernetes-break-logs",
    ]);
    // Reads that were already in flight when a commit was noticed, asked once
    // more when that read settles, since its answer may predate the commit.
    const pending = new Set<string>();
    const recoveryFamilies = new Set(["kubernetes-overview", "kubernetes-sync-health"]);
    const recover = () => {
      for (const family of recoveryFamilies) {
        void cache.invalidateQueries({ queryKey: key(family) }, { cancelRefetch: false });
      }
    };
    // Change notices and read starts are numbered in the order they happen,
    // so a refresh can tell whether a read started after the latest change,
    // even within the same millisecond.
    let order = 0;
    const fetchStarts = new WeakMap<object, number>();
    // The latest change not yet refreshed for, and when the last refresh ran.
    let latestChange: number | undefined;
    let lastFlushAt = Number.NEGATIVE_INFINITY;
    // The last committed generation and sync report the feed announced. They
    // outlive a single connection: each reconnect opens with the current
    // generation, and one that has not moved changes nothing on screen.
    let generation: string | undefined;
    let syncSignature: string | undefined;
    const unsubscribe = cache.getQueryCache().subscribe(event => {
      if (disposed || event.type !== "updated") return;
      if (event.action.type === "fetch") fetchStarts.set(event.query, ++order);
      if (event.query.state.fetchStatus !== "idle" || !pending.delete(event.query.queryHash)) return;
      if (denied || kubernetesAccessLost(event.query.state.error)) return;
      void cache.invalidateQueries({ queryKey: event.query.queryKey, exact: true }, { cancelRefetch: false });
    });
    const refresh = () => {
      flush = undefined;
      if (disposed || denied || latestChange === undefined) return;
      const change = latestChange;
      latestChange = undefined;
      lastFlushAt = Date.now();
      // A read that started after the latest change already answers for every
      // change gathered here, whether it is still running or has settled;
      // asking again would only repeat it. A read that started between two of
      // them may hold the earlier state, so it is asked again.
      const covered = (query: object) => (fetchStarts.get(query) ?? 0) > change;
      for (const query of cache.getQueryCache().findAll({ queryKey: key() })) {
        if (families.has(String(query.queryKey[2])) && query.isActive() && query.state.fetchStatus === "fetching" && !covered(query)) pending.add(query.queryHash);
      }
      // Do not cancel a slow persisted read on every commit. Mark it stale
      // and schedule one trailing refresh when its current request settles.
      // A history page that says history is off for this identity cannot
      // change with a commit; the page asks again when the access changes.
      // A read waiting out its failure backoff is asked again when the
      // backoff ends, not on every notice.
      void cache.invalidateQueries({ queryKey: key(), predicate: query =>
        families.has(String(query.queryKey[2])) && !covered(query) && !historyOff(query.state.data) && !kubernetesFailureBackingOff(query),
      }, { cancelRefetch: false });
    };
    // Every change notice lands here. Notices are gathered into one refresh:
    // the first after a quiet spell runs shortly after it arrives, and any that
    // follow wait until the refresh interval has passed, so a burst of commits
    // (a backlog after a recovery, a busy cluster) asks each read once per
    // interval rather than once per commit.
    const invalidate = () => {
      if (disposed || denied) return;
      latestChange = ++order;
      if (flush) return;
      flush = setTimeout(refresh, Math.max(commitRefreshDelayMs, lastFlushAt + commitRefreshIntervalMs - Date.now()));
    };
    const run = async () => {
      const currentController = new AbortController();
      controller = currentController;
      let active = true;
      let delivered = false;
      const openedAt = Date.now();
      connected = false;
      if (failures) {
        release();
        setState("reconnecting");
      } else {
        hold();
      }
      const seen = new Set<string>();
      let cleanEnd = true;
      try {
        await client.kubernetesCommitStream(event => {
          if (disposed || !active || currentController.signal.aborted || !["committed", "sync"].includes(event.event)) return;
          let payload: Record<string, unknown>;
          try { payload = JSON.parse(event.data) as Record<string, unknown>; } catch { throw new Error("Invalid commit event"); }
          if (!payload || typeof payload !== "object" || typeof payload.cluster_id !== "string"
            || cluster && payload.cluster_id !== cluster) throw new Error("Commit event cluster mismatch");
          if (event.event === "committed" && (!Number.isSafeInteger(payload.revision) || !Array.isArray(payload.kinds)
            || !payload.kinds.every(kind => typeof kind === "string"))) throw new Error("Invalid commit revision");
          if (event.event === "sync" && (typeof payload.state !== "string" || typeof payload.age !== "number")) throw new Error("Invalid sync event");
          const reason = typeof payload.reason === "string" ? payload.reason : undefined;
          if (event.event === "sync") setSync({ state: String(payload.state), reason, age: Number(payload.age) });
          denied = false;
          recoveryOnly = false;
          connected = true;
          delivered = true;
          failures = 0;
          release();
          if (event.event === "committed" && payload.history === "off_namespace_access") liveOff = true;
          setState(liveOff ? "polling" : "live");
          const identity = event.id || (event.event === "committed" ? String(payload.revision) : undefined);
          if (event.event === "committed") {
            if (identity && seen.has(identity)) return;
            if (identity) { seen.add(identity); lastID = identity; }
            if (seen.size > 1024) seen.delete(seen.values().next().value!);
            // Each connection opens with the current generation. One that has
            // not moved since the last connection has nothing new to show; any
            // other value, lower ones included (revisions restart with the
            // server), is a change.
            const next = `${String(payload.revision)}:${String(payload.evidence_sequence ?? "")}`;
            if (next === generation) return;
            generation = next;
            invalidate();
            return;
          }
          // The sync event repeats the same state every second while the
          // cluster is healthy; the report is asked again only when the state
          // or its cause changes; a new generation reaches it through the
          // commit refresh. The report keeps its own poll for everything else.
          // The first event is measured against the report already on screen.
          const signature = JSON.stringify([payload.state, reason ?? ""]);
          const changed = syncSignature === undefined
            ? cache.getQueryData<{ state?: unknown }>(key("kubernetes-sync-health"))?.state !== payload.state
            : signature !== syncSignature;
          syncSignature = signature;
          if (changed) void cache.invalidateQueries({ queryKey: key("kubernetes-sync-health") }, { cancelRefetch: false });
        }, currentController.signal, lastID);
      } catch (error) {
        cleanEnd = false;
        if (disposed) return;
        if (namespaceOnly && error instanceof ApiError && error.status === 403 && kubernetesFailureCode(error) === "history_authority_denied") {
          release();
          setState("polling");
          return;
        }
        if (kubernetesAccessLost(error)) {
          const historyDenied = error instanceof ApiError && error.status === 403
            && kubernetesFailureCode(error) === "history_authority_denied";
          if (!denied) cache.setQueryData(key("kubernetes-authority-fence"), Symbol("persisted evidence authority"));
          denied = true;
          recoveryOnly = historyDenied;
          pending.clear();
          clearTimeout(flush);
          flush = undefined;
          latestChange = undefined;
          // Access came back means every read is asked afresh: the first
          // generation after the fence is a change even if it has not moved.
          generation = undefined;
          syncSignature = undefined;
          lastFlushAt = Number.NEGATIVE_INFINITY;
          const filters = {
            queryKey: key(),
            predicate: (query: { queryKey: readonly unknown[] }) => query.queryKey[2] !== "kubernetes-authority-fence"
              && (!historyDenied || !recoveryFamilies.has(String(query.queryKey[2]))
                && !["kubernetes-agent-settings", "issue-alerts"].includes(String(query.queryKey[2]))),
          };
          await cache.cancelQueries(filters);
          if (disposed) return;
          cache.removeQueries({ queryKey: key("break-seed") });
          for (const query of cache.getQueryCache().findAll(filters)) {
            query.setState({ data: undefined, error: error as Error, status: "error", fetchStatus: "idle" });
          }
          release();
          setState("reconnecting");
          if (!historyDenied) return;
          // Persisted evidence denial is not an application/session denial.
          // Only fresh authorized reads may restore protected data; live health
          // and overview remain reachable while the commit handshake retries.
          recover();
        }
      } finally { active = false; }
      if (disposed) return;
      connected = false;
      if (delivered && !denied) {
        // The stream was live and has ended: the server closes every stream
        // after a short lease by design. Keep the current state through the
        // grace period while the next connection opens; only a reconnect that
        // fails or stays silent past the grace period shows the notice.
        hold();
        const planned = cleanEnd && Date.now() - openedAt >= commitStreamGraceMs;
        retry = setTimeout(() => void run(), planned ? 0 : commitStreamRetryDelay(0));
        return;
      }
      release();
      setState("reconnecting");
      retry = setTimeout(() => void run(), commitStreamRetryDelay(failures++));
    };
    const fallback = setInterval(() => {
      if (denied && recoveryOnly) recover();
      else if ((!connected || liveOff) && !denied) invalidate();
    }, 30000);
    void run();
    return () => {
      disposed = true;
      unsubscribe();
      controller?.abort();
      clearTimeout(retry);
      clearTimeout(flush);
      release();
      clearInterval(fallback);
    };
  }, [enabled, client, key, cluster, cache, namespaceOnly]);
  return { streamState: state, sync };
}

/**
 * How long the commit feed may be between connections before the top bar says
 * updates are disconnected. A healthy reconnect sends its first event within
 * about a second, so a planned stream end never reaches this; a stream that is
 * still silent after five seconds is treated as down.
 */
export const commitStreamGraceMs = 5_000;

/** How soon after a first change notice the views are asked again. */
export const commitRefreshDelayMs = 100;

/**
 * The shortest time between two refreshes driven by the commit feed. Notices
 * that arrive sooner are gathered into the next refresh, so each read is asked
 * at most once per interval however many commits land.
 */
export const commitRefreshIntervalMs = 5_000;

/** Longest wait between commit-stream reconnects while the server keeps failing. */
export const commitStreamRetryCapMs = 60_000;

/**
 * Exponential reconnect delay. Once it reaches the cap it is spread over the
 * last fifth of the cap, so open tabs do not reconnect in lockstep.
 */
export function commitStreamRetryDelay(failures: number, random: () => number = Math.random): number {
  const delay = 1000 * 2 ** Math.max(0, failures);
  if (delay < commitStreamRetryCapMs) return delay;
  return Math.round(commitStreamRetryCapMs * (0.8 + 0.2 * random()));
}

export function useKubernetesCrashes(options: Omit<KubernetesCrashQuery, "limit" | "cursor">, enabled = true, keepAcrossRanges = false, focused = false) {
  const { client, cluster } = useKubernetesCluster();
  const paging = useCursorPagination(JSON.stringify([cluster, options]));
  const query = useKubernetesQuery({
    queryKey: ["kubernetes-crashes", options, paging.cursor, ...(focused ? ["focused"] : [])],
    queryFn: ({ signal }) => focused
      ? focusedCrashes(cursor => client.kubernetesCrashes({ ...options, limit: 50, cursor }, signal, "single"), signal)
      : client.kubernetesCrashes({ ...options, limit: 50, cursor: paging.cursor }, signal),
    enabled,
    placeholderData: (previous, query) => {
      if (query?.queryKey[1] !== (cluster ?? "_")) return undefined;
      if ((query.queryKey[5] === "focused") !== focused) return undefined;
      const prior = query.queryKey[3] as typeof options | undefined;
      if (!prior || prior.namespace !== options.namespace || prior.root_uid !== options.root_uid || prior.pod_uid !== options.pod_uid) return undefined;
      if (keepAcrossRanges || prior.since === options.since && prior.until === options.until) return previous;
      const start = Date.parse(options.since ?? ""), end = Date.parse(options.until ?? "");
      const priorStart = Date.parse(prior.since ?? ""), priorEnd = Date.parse(prior.until ?? "");
      return end - start === priorEnd - priorStart && end >= priorEnd && end - priorEnd <= 60000 ? previous : undefined;
    },
    retry: false,
    staleTime: Infinity,
    gcTime: 60_000,
    refetchOnWindowFocus: false,
  });
  // An expired crash page link comes back from the reader as an error. On
  // arrival the pager returns to page 1, so page 1's crashes are never labelled
  // as a later page; a page already on screen keeps its rows and offers
  // Restart crashes.
  useKubernetesCursorReset(paging, query);
  return { ...query, paging, focused };
}