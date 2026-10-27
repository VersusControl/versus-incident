import type { KubernetesResource } from "@/lib/api";

// Kinds the log stream can serve. A Deployment or StatefulSet is read as one
// aggregate across its Pods; a DaemonSet or Job has no aggregate read, so its
// rows offer no Logs action rather than opening a stream that cannot serve them.
const streamableKinds = new Set(["Pod", "Deployment", "StatefulSet"]);

export function canStreamLogs(resource: KubernetesResource): boolean {
  return streamableKinds.has(resource.kind);
}
