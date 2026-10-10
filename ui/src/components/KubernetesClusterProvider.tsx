import { useMemo, type ReactNode } from "react";
import { api, createKubernetesApi, type KubernetesClusters } from "@/lib/api";
import { KubernetesClusterContext, kubernetesKey, type ClusterContext } from "@/lib/useKubernetesCluster";

export function KubernetesClusterProvider({ catalog, cluster, children }: { catalog: KubernetesClusters; cluster?: string; children: ReactNode }) {
  const client = useMemo(() => cluster ? createKubernetesApi(cluster) : api, [cluster]);
  const key = useMemo(() => (...parts: readonly unknown[]) => kubernetesKey(cluster, ...parts), [cluster]);
  const value = useMemo<ClusterContext>(() => ({ multiple: catalog.multiple, cluster, clusters: catalog.clusters, client, key }), [catalog, cluster, client, key]);
  return <KubernetesClusterContext.Provider value={value}>{children}</KubernetesClusterContext.Provider>;
}