const ruleLabels: Record<string, string> = {
  "pod.crash_loop": "crash looping",
  "pod.image_pull": "unable to pull its image",
  "pod.config_error": "showing a container config error",
  "pod.evicted": "evicted",
  "pod.failed": "failing",
  "pod.unschedulable": "unschedulable",
  "workload.rollout_stalled": "stalled during rollout",
  "workload.unavailable": "unavailable",
  "job.failed": "failing",
  "node.not_ready": "not ready",
};
const resourceKinds: Record<string, string> = {
  pods: "Pod", deployments: "Deployment", statefulsets: "StatefulSet",
  daemonsets: "DaemonSet", replicasets: "ReplicaSet", jobs: "Job",
  cronjobs: "CronJob", nodes: "Node",
};

export function kubernetesIssuePrompt(params: URLSearchParams): string {
  if (params.get("provider") !== "kubernetes" || params.has("session")) return "";
  const label = ruleLabels[params.get("issue") ?? ""];
  const name = params.get("name");
  const resource = params.get("resource_id")?.split("~").at(-1) ?? "";
  const kind = resourceKinds[resource];
  if (!label || !name || !kind) return "";
  return `Why is ${kind} ${name} ${label}?`;
}
