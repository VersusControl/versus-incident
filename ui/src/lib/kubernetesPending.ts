import type { KubernetesSync } from "./api";

// Resource types the backend has not listed yet. They are loading, not
// empty, so views must never present them as zero.
const kindsByResource: Record<string, string> = {
  configmaps: "ConfigMap",
  secrets: "Secret",
  persistentvolumeclaims: "PersistentVolumeClaim",
  serviceaccounts: "ServiceAccount",
  controllerrevisions: "ControllerRevision",
  ingresses: "Ingress",
  httproutes: "HTTPRoute",
  gateways: "Gateway",
  services: "Service",
  pods: "Pod",
  nodes: "Node",
  namespaces: "Namespace",
  events: "Event",
  deployments: "Deployment",
  replicasets: "ReplicaSet",
  statefulsets: "StatefulSet",
  daemonsets: "DaemonSet",
  jobs: "Job",
  cronjobs: "CronJob",
  horizontalpodautoscalers: "HorizontalPodAutoscaler",
};

/** Kind name for a resource id such as "networking.k8s.io~v1~ingresses"; unknown types keep their resource name. */
export function pendingKindName(resourceID: string) {
  const resource = resourceID.split("~").at(-1) ?? resourceID;
  return kindsByResource[resource] ?? resource;
}

export function pendingKindNames(pending?: string[] | null) {
  return new Set((pending ?? []).filter(Boolean).map(pendingKindName));
}

/**
 * Pending kinds alone are loading, not missing. When a view names pending
 * kinds and reports no real failures, drop the "partial" flag that loading
 * caused. A fresh view becomes ready; a stale view stays stale so old data
 * is never presented as current. Any other state, or any real failure, is
 * returned unchanged.
 */
export function settledSync(sync: KubernetesSync | undefined, failures?: readonly unknown[] | null): KubernetesSync | undefined {
  if (!sync?.pending?.length || failures?.length) return sync;
  if (sync.state === "partial") return { ...sync, state: "ready", partial: false };
  if (sync.state === "stale") return { ...sync, partial: false };
  return sync;
}

export function evidenceLoading(...pending: Array<readonly string[] | null | undefined>) {
  return pending.some((list) => Boolean(list?.length));
}
