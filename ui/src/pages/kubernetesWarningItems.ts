import type { KubernetesChangesPage, KubernetesIssue, KubernetesIssuePage } from "@/lib/api";
import { settledSync } from "@/lib/kubernetesPending";
import { historyOff } from "@/lib/kubernetesNamespaceAccess";

/** Display names for resource types, keyed by the plural in a resource id. */
const kindNames: Record<string, string> = {
  pods: "Pod", nodes: "Node", namespaces: "Namespace", events: "Event", services: "Service",
  deployments: "Deployment", statefulsets: "StatefulSet", daemonsets: "DaemonSet", replicasets: "ReplicaSet",
  controllerrevisions: "ControllerRevision", jobs: "Job", cronjobs: "CronJob", ingresses: "Ingress",
  horizontalpodautoscalers: "HorizontalPodAutoscaler", persistentvolumeclaims: "PersistentVolumeClaim",
  configmaps: "ConfigMap", secrets: "Secret", serviceaccounts: "ServiceAccount", httproutes: "HTTPRoute",
  gateways: "Gateway", applications: "Application", rollouts: "Rollout", kustomizations: "Kustomization",
  helmreleases: "HelmRelease",
};

/** Readable names for failure sources that are not Kubernetes resource ids. */
const failureSources: Record<string, string> = {
  "kubernetes.history": "Change history",
  "kubernetes.discovery": "API discovery",
  "kubernetes.inventory": "Inventory",
  "metrics.k8s.io": "Usage metrics",
  "metrics.k8s.io~v1beta1~pods": "Pod usage metrics",
  "metrics.k8s.io~v1beta1~nodes": "Node usage metrics",
};

export type KubernetesWarning = { id: string; content: string };

type PartialFailure = { resource_id?: string; scope?: string; class: string; kind?: string; message?: string };

/** What a partial failure is about, as a display name rather than a resource id. */
function failureName(failure: PartialFailure): string {
  if (failure.kind?.trim()) return failure.kind.trim();
  const id = failure.resource_id ?? "";
  if (failureSources[id]) return failureSources[id];
  const plural = id.split("~").at(-1) ?? "";
  if (plural) return kindNames[plural.toLowerCase()] ?? plural;
  return failure.scope ? kindNames[failure.scope.toLowerCase()] ?? failure.scope : "A Kubernetes resource";
}

/** One read in plain words, worded like the server's own sentences. */
function failureSentence(name: string, failureClass: string): string {
  switch (failureClass) {
    case "forbidden": return `${name} is not readable with this connector's permissions.`;
    case "namespace_scope": return `${name} is read only in the namespaces this connector can access.`;
    case "unauthorized":
    case "credential_unavailable": return `${name} could not be read because the cluster rejected the connector's credential.`;
    case "timeout": return `${name} took too long to read; the next sync tries again.`;
    case "throttled": return `The cluster throttled the ${name} read; the next sync tries again.`;
    case "unsupported": return `${name} is not served by this cluster.`;
    case "not_found": return `${name} is not installed or unavailable.`;
    case "budget_exhausted": case "record_limit": case "incomplete_pages": case "encoded_result_size": case "cursor_limit":
      return `${name} was read only in part because it is larger than one read allows.`;
  }
  return `${name} could not be read; the next sync tries again.`;
}

/**
 * One warning per partial failure, in the order the server sent them. A failure
 * that carries the server's plain sentence (`message`) is shown as written; a
 * failure from a read that sends none is worded the same way here.
 */
export function failureWarnings(failures?: Array<PartialFailure> | null): KubernetesWarning[] {
  return (failures ?? []).filter((failure) => !["item_limit", "request_limit", "budget", "page_limit", "syncing"].includes(failure.class)).map((failure) => ({
    id: `failure:${failure.resource_id ?? ""}:${failure.scope ?? ""}:${failure.class}`,
    content: typeof failure.message === "string" && failure.message.trim() ? failure.message.trim()
      : !failure.resource_id && failure.scope === "discovery" ? discoveryWarning(failure.class)
        : failureSentence(failureName(failure), failure.class),
  }));
}

/**
 * The server reports a failed API discovery under the scope "discovery". That
 * is an internal stage name, so it is written out as a sentence instead.
 */
function discoveryWarning(failureClass: string): string {
  switch (failureClass) {
    case "timeout": return "The cluster did not list its resource types in time";
    case "forbidden":
    case "unauthorized": return "The cluster's resource types could not be listed: access denied";
    default: return "The cluster's resource types could not be listed";
  }
}

export function issueWarnings(issue?: KubernetesIssue): KubernetesWarning[] {
  if (!issue) return [];
  const warnings: KubernetesWarning[] = [];
  if (issue.gaps?.length) {
    warnings.push({ id: "history", content: "Change capture does not cover the whole range" });
  }

  for (const kind of [...new Set(issue.untracked ?? [])].sort()) warnings.push({ id: `untracked:${kind}`, content: `${kind} history is not tracked` });
  if (issue.history_truncated) warnings.push({ id: "history-truncated", content: "Some changes before this break were not loaded" });
  return warnings;
}

export function timelineWarnings(issues?: KubernetesIssuePage, history?: KubernetesChangesPage): KubernetesWarning[] {
  // A page that says history is off is empty by design, not incomplete.
  const changes = historyOff(history) ? undefined : history;
  const warnings = [
    ...failureWarnings(issues?.partial_failures),
    ...failureWarnings(changes?.partial_failures),
    ...(issues?.items ?? []).flatMap(issueWarnings),
  ];
  if (issues?.items.some((issue) => issue.evidence?.partial)) warnings.push({ id: "crash-capture", content: "Some crash evidence could not be captured" });
  if (issues?.sync.state === "stale") warnings.push({ id: "break-stale", content: "Break evidence is stale." });
  if (settledSync(issues?.sync, issues?.partial_failures)?.partial && !issues?.partial_failures?.length && !issues?.truncated) warnings.push({ id: "break-partial", content: "Some issue evidence could not be read" });
  if (changes?.gaps?.length) warnings.push({ id: "changes-gaps", content: "Change capture does not cover the whole range" });
  if (changes?.sync.state === "stale") warnings.push({ id: "changes-stale", content: "Change activity is stale." });
  if (changes?.sync.partial && !changes.gaps?.length && !changes.partial_failures?.length) warnings.push({ id: "changes-partial", content: "Change activity reflects incomplete history." });
  return warnings;
}