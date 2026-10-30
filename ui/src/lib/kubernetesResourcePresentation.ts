import { isKnownKubernetesTime } from "./kubernetesTime";

export function elapsed(at: unknown) {
  if (!isKnownKubernetesTime(at)) return "Unavailable";
  const minutes = Math.max(
    0,
    Math.floor((Date.now() - Date.parse(at)) / 60000),
  );
  return minutes < 60
    ? `${minutes}m`
    : minutes < 1440
      ? `${Math.floor(minutes / 60)}h ${minutes % 60}m`
      : `${Math.floor(minutes / 1440)}d ${Math.floor((minutes % 1440) / 60)}h`;
}
export function clock(time: string) {
  return isKnownKubernetesTime(time)
    ? new Date(time).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false })
    : "unavailable";
}
export const resourceIDs: Record<string, string> = {
  Pod: "core~v1~pods",
  Node: "core~v1~nodes",
  Namespace: "core~v1~namespaces",
  Service: "core~v1~services",
  Secret: "core~v1~secrets",
  ConfigMap: "core~v1~configmaps",
  PersistentVolumeClaim: "core~v1~persistentvolumeclaims",
  Deployment: "apps~v1~deployments",
  ReplicaSet: "apps~v1~replicasets",
  StatefulSet: "apps~v1~statefulsets",
  DaemonSet: "apps~v1~daemonsets",
  Job: "batch~v1~jobs",
  CronJob: "batch~v1~cronjobs",
  HorizontalPodAutoscaler: "autoscaling~v2~horizontalpodautoscalers",
};
