import { createContext, useContext, useEffect, useRef, useState } from "react";
import { useQuery, useQueryClient, type UseQueryOptions } from "@tanstack/react-query";
import { api, ApiError, createKubernetesApi, retainKubernetesHistoryCounts, type KubernetesClusters, type KubernetesCrashQuery } from "./api";
import { useCursorPagination } from "./pagination";
import { hasKubernetesPublication, kubernetesAccessLost, kubernetesFailureCode, retainKubernetesPublication } from "./kubernetesContinuity";
import { focusedCrashes } from "./kubernetesFocusedHistory";

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
    structuralSharing: (previous, incoming) => retainKubernetesPublication(
      kubernetesAccessLost(cache.getQueryState(key(...options.queryKey))?.error) ? undefined : previous as T | undefined,
      incoming as T,
    ),
  });
  const accessLost = kubernetesAccessLost(query.error);
  const scopedKey = JSON.stringify(key(...options.queryKey));
  const authorityFence = cache.getQueryData<symbol>(key("kubernetes-authority-fence"));
  const lastGood = useRef<{ key: string; data: T; at: number; fence: symbol | undefined }>();
  if (lastGood.current && lastGood.current.fence !== authorityFence) lastGood.current = undefined;
  const invalidRequest = query.error instanceof ApiError && [400, 409, 410, 422].includes(query.error.status);
  if (accessLost || invalidRequest) lastGood.current = undefined;
  else if (query.data !== undefined) lastGood.current = { key: scopedKey, data: query.data, at: query.dataUpdatedAt || lastGood.current?.at || Date.now(), fence: authorityFence };
  const retained = query.data ?? (query.isError && lastGood.current?.key === scopedKey ? lastGood.current.data : undefined);
  useEffect(() => {
    if (!accessLost) return;
    // Keep the authoritative error, but discard its now-unauthorized cache.
    cache.getQueryCache().find({ queryKey: JSON.parse(scopedKey) as readonly unknown[], exact: true })?.setState({ data: undefined });
  }, [accessLost, cache, scopedKey]);
  const missingPublication = query.data !== undefined && !hasKubernetesPublication(query.data, firstReadyAt);
  const unpublished = missingPublication && (query.data as { sync?: { state?: string } }).sync?.state === "syncing";
  const hidden = accessLost || missingPublication;
  return { ...query, unpublished, data: hidden ? undefined : retained, dataUpdatedAt: query.dataUpdatedAt || lastGood.current?.at || 0 };
}

export function useBrokenIssues(enabled = true) {
  const { client, cluster } = useKubernetesCluster();
  return useKubernetesQuery({ queryKey: ["kubernetes-issues", "broken", cluster], queryFn: ({ signal }) => client.kubernetesIssues({ broken: true, include: "changes", limit: 5 }, signal), enabled, retry: false });
}

/** One commit feed per mounted explorer; missed commits converge by invalidation. */
export function useKubernetesCommitEvents(enabled: boolean) {
  const { client, key, cluster } = useKubernetesCluster();
  const cache = useQueryClient();
  const [state, setState] = useState<"connecting" | "live" | "reconnecting">("connecting");
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
    let controller: AbortController | undefined;
    const families = new Set([
      "kubernetes-overview", "kubernetes-sync-health", "kubernetes-graph", "kubernetes-overview-topology",
      "kubernetes-namespaces", "kubernetes-workloads", "kubernetes-nodes", "kubernetes-node-pods",
      "kubernetes-issues", "kubernetes-top", "kubernetes-diagnosis", "kubernetes-drawer-changes",
      "kubernetes-changes", "kubernetes-crashes", "kubernetes-timeline", "kubernetes-break-logs",
    ]);
    const pending = new Set<string>();
    const recoveryFamilies = new Set(["kubernetes-overview", "kubernetes-sync-health"]);
    const recover = () => {
      for (const family of recoveryFamilies) {
        void cache.invalidateQueries({ queryKey: key(family) }, { cancelRefetch: false });
      }
    };
    const unsubscribe = cache.getQueryCache().subscribe(event => {
      if (disposed || event.type !== "updated" || event.query.state.fetchStatus !== "idle"
        || !pending.delete(event.query.queryHash)) return;
      if (denied || kubernetesAccessLost(event.query.state.error)) return;
      void cache.invalidateQueries({ queryKey: event.query.queryKey, exact: true }, { cancelRefetch: false });
    });
    const invalidate = () => {
      if (disposed || denied || flush) return;
      flush = setTimeout(() => {
        flush = undefined;
        if (disposed || denied) return;
        for (const query of cache.getQueryCache().findAll({ queryKey: key() })) {
          if (families.has(String(query.queryKey[2])) && query.isActive() && query.state.fetchStatus === "fetching") pending.add(query.queryHash);
        }
        // Do not cancel a slow persisted read on every commit. Mark it stale
        // and schedule one trailing refresh when its current request settles.
        void cache.invalidateQueries({ queryKey: key(), predicate: query =>
          families.has(String(query.queryKey[2])),
        }, { cancelRefetch: false });
      }, 100);
    };
    const run = async () => {
      const currentController = new AbortController();
      controller = currentController;
      let active = true;
      connected = false;
      setState(failures ? "reconnecting" : "connecting");
      const seen = new Set<string>();
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
          if (event.event === "sync") setSync({ state: String(payload.state), reason: typeof payload.reason === "string" ? payload.reason : undefined, age: Number(payload.age) });
          denied = false;
          recoveryOnly = false;
          connected = true;
          failures = 0;
          setState("live");
          const identity = event.id || (event.event === "committed" ? String(payload.revision) : undefined);
          if (event.event === "committed") {
            if (identity && seen.has(identity)) return;
            if (identity) { seen.add(identity); lastID = identity; }
            if (seen.size > 1024) seen.delete(seen.values().next().value!);
          }
          // A new connection's initial snapshot is deliberately not deduped
          // against the old process: revisions can reset after a restart.
          if (event.event === "committed") invalidate();
          else void cache.invalidateQueries({ queryKey: key("kubernetes-sync-health") }, { cancelRefetch: false });
        }, currentController.signal, lastID);
      } catch (error) {
        if (disposed) return;
        if (kubernetesAccessLost(error)) {
          const historyDenied = error instanceof ApiError && error.status === 403
            && kubernetesFailureCode(error) === "history_authority_denied";
          if (!denied) cache.setQueryData(key("kubernetes-authority-fence"), Symbol("persisted evidence authority"));
          denied = true;
          recoveryOnly = historyDenied;
          pending.clear();
          clearTimeout(flush);
          flush = undefined;
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
      setState("reconnecting");
      retry = setTimeout(() => void run(), Math.min(1000 * 2 ** failures++, 30000));
    };
    const fallback = setInterval(() => {
      if (denied && recoveryOnly) recover();
      else if (!connected && !denied) invalidate();
    }, 30000);
    void run();
    return () => {
      disposed = true;
      unsubscribe();
      controller?.abort();
      clearTimeout(retry);
      clearTimeout(flush);
      clearInterval(fallback);
    };
  }, [enabled, client, key, cluster, cache]);
  return { streamState: state, sync };
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
  return { ...query, paging, focused };
}