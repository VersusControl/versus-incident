import { createContext, useContext } from "react";
import { useQuery, type UseQueryOptions } from "@tanstack/react-query";
import { api, createKubernetesApi, type KubernetesClusters } from "./api";

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