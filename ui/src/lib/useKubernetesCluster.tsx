import { createContext, useContext } from "react";
import { useQuery, type UseQueryOptions } from "@tanstack/react-query";
import { api, createKubernetesApi, type KubernetesClusters, type KubernetesCrashQuery } from "./api";
import { useCursorPagination } from "./pagination";

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
const singleContext: ClusterContext = { multiple: false, clusters: [], client: api, key: (...parts) => kubernetesKey(undefined, ...parts) };

export function useKubernetesCluster(): ClusterContext {
  return useContext(KubernetesClusterContext) ?? singleContext;
}

export function useKubernetesQuery<T>(options: UseQueryOptions<T, Error, T, readonly unknown[]>) {
  const { key } = useKubernetesCluster();
  return useQuery({ ...options, queryKey: key(...options.queryKey) });
}

export function useBrokenIssues(enabled = true) {
  const { client, cluster } = useKubernetesCluster();
  return useKubernetesQuery({ queryKey: ["kubernetes-issues", "broken", cluster], queryFn: () => client.kubernetesIssues({ broken: true, include: "changes", limit: 5 }), enabled, retry: false, refetchInterval: 30_000 });
}

export function useKubernetesCrashes(options: Omit<KubernetesCrashQuery, "limit" | "cursor">) {
  const { client, cluster } = useKubernetesCluster();
  const paging = useCursorPagination(JSON.stringify([cluster, options]));
  const query = useKubernetesQuery({
    queryKey: ["kubernetes-crashes", options, paging.cursor],
    queryFn: ({ signal }) => client.kubernetesCrashes({ ...options, limit: 50, cursor: paging.cursor }, signal),
    retry: false,
    staleTime: Infinity,
    gcTime: 60_000,
    refetchOnWindowFocus: false,
  });
  return { ...query, paging };
}