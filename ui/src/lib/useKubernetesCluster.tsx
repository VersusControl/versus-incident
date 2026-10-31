import { createContext, useContext, useEffect } from "react";
import { useQuery, useQueryClient, type UseQueryOptions } from "@tanstack/react-query";
import { api, createKubernetesApi, type KubernetesClusters, type KubernetesCrashQuery } from "./api";
import { useCursorPagination } from "./pagination";
import { hasKubernetesPublication, kubernetesAccessLost, retainKubernetesPublication } from "./kubernetesContinuity";
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
  const query = useQuery({
    ...options,
    queryKey: key(...options.queryKey),
    structuralSharing: (previous, incoming) => retainKubernetesPublication(
      kubernetesAccessLost(cache.getQueryState(key(...options.queryKey))?.error) ? undefined : previous as T | undefined,
      incoming as T,
    ),
  });
  const accessLost = kubernetesAccessLost(query.error);
  const scopedKey = JSON.stringify(key(...options.queryKey));
  useEffect(() => {
    if (!accessLost) return;
    // Keep the authoritative error, but discard its now-unauthorized cache.
    cache.getQueryCache().find({ queryKey: JSON.parse(scopedKey) as readonly unknown[], exact: true })?.setState({ data: undefined });
  }, [accessLost, cache, scopedKey]);
  const missingPublication = query.data !== undefined && !hasKubernetesPublication(query.data, firstReadyAt);
  const unpublished = missingPublication && (query.data as { sync?: { state?: string } }).sync?.state === "syncing";
  const hidden = accessLost || missingPublication;
  return { ...query, unpublished, data: hidden ? undefined : query.data };
}

export function useBrokenIssues(enabled = true) {
  const { client, cluster } = useKubernetesCluster();
  return useKubernetesQuery({ queryKey: ["kubernetes-issues", "broken", cluster], queryFn: () => client.kubernetesIssues({ broken: true, include: "changes", limit: 5 }), enabled, retry: false, refetchInterval: 30_000 });
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