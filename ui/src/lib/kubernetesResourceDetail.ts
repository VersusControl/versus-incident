import type {
  KubernetesCrashEvent,
  KubernetesCrashPage,
  KubernetesResource,
  KubernetesWorkload,
} from "./api";
import {
  useKubernetesCluster,
  useKubernetesQuery,
} from "./useKubernetesCluster";
import { isKnownKubernetesTime } from "./kubernetesTime";

export type ResourceDetailTab =
  | "overview"
  | "logs"
  | "events"
  | "yaml"
  | "related"
  | "timeline";
export const resourceDetailTabs: Array<{
  id: ResourceDetailTab;
  label: string;
}> = [
  { id: "overview", label: "Overview" },
  { id: "logs", label: "Logs" },
  { id: "events", label: "Events" },
  { id: "yaml", label: "YAML" },
  { id: "related", label: "Related" },
  { id: "timeline", label: "Timeline" },
];
export const detailText = (value: unknown, fallback = "Unavailable"): string =>
  typeof value === "string" || typeof value === "number"
    ? String(value)
    : fallback;
const quantityUnits: Record<string, number> = {
  "": 1,
  n: 1e-9,
  u: 1e-6,
  µ: 1e-6,
  m: 1e-3,
  k: 1e3,
  K: 1e3,
  M: 1e6,
  G: 1e9,
  T: 1e12,
  P: 1e15,
  E: 1e18,
  Ki: 2 ** 10,
  Mi: 2 ** 20,
  Gi: 2 ** 30,
  Ti: 2 ** 40,
  Pi: 2 ** 50,
  Ei: 2 ** 60,
};
export function quantity(value: unknown): number | null {
  if (typeof value === "number") return Number.isFinite(value) ? value : null;
  if (typeof value !== "string") return null;
  const input = value.trim();
  const rational = /^([+-]?\d+)\/([+-]?\d+)$/.exec(input);
  if (rational) {
    const result = Number(rational[1]) / Number(rational[2]);
    return Number(rational[2]) !== 0 && Number.isFinite(result) ? result : null;
  }
  const match =
    /^([+-]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][+-]?\d+)?)(n|u|µ|m|k|K|M|G|T|P|E|Ki|Mi|Gi|Ti|Pi|Ei)?$/.exec(
      input,
    );
  if (!match) return null;
  const result = Number(match[1]) * quantityUnits[match[2] ?? ""];
  return Number.isFinite(result) ? result : null;
}
export interface ProjectedContainerState {
  name: string;
  init?: boolean;
  ready?: boolean;
  running?: boolean;
  started_at?: string;
  last_terminated_reason?: string;
  last_terminated_at?: string;
  last_terminated_exit_code?: number;
}
export function resourceContainerStates(
  resource: KubernetesResource,
): ProjectedContainerState[] {
  const states =
    resource.summary?.log_container_states ??
    resource.summary?.container_states;
  return (Array.isArray(states) ? states : []).filter(
    (state): state is ProjectedContainerState =>
      Boolean(
        state && typeof state === "object" && typeof state.name === "string",
      ),
  );
}
export function resourceStart(
  resource: KubernetesResource,
  workload?: KubernetesWorkload,
): string | undefined {
  if (resource.kind !== "Pod")
    return workload?.pods
      ?.map((pod) => pod.started_at)
      .filter((at): at is string => isKnownKubernetesTime(at))
      .sort()
      .at(-1);
  const states = resourceContainerStates(resource).filter((state) =>
    resource.summary?.main_container
      ? state.name === resource.summary.main_container
      : !state.init,
  );
  const started = states.flatMap((state) =>
    isKnownKubernetesTime(state.started_at) ? [state.started_at] : [],
  );
  return started.sort().at(-1);
}
export function downloadText(text: string, name: string, type = "text/plain") {
  const url = URL.createObjectURL(
    new Blob([text], { type: `${type};charset=utf-8` }),
  );
  const link = document.createElement("a");
  link.href = url;
  link.download = name;
  link.click();
  URL.revokeObjectURL(url);
}
export function useResourceCrashes(resource: KubernetesResource) {
  const { client } = useKubernetesCluster();
  return useKubernetesQuery({
    queryKey: [
      "resource-detail-crashes",
      resource.uid,
      resource.namespace,
      resource.name,
    ],
    queryFn: () =>
      client.kubernetesCrashes({
        namespace: resource.namespace,
        ...(resource.kind !== "Pod" && resource.uid
          ? { root_uid: resource.uid }
          : {}),
        since: new Date(Date.now() - 86400000).toISOString(),
        until: new Date().toISOString(),
        limit: 200,
      }),
    enabled: Boolean(resource.uid),
    retry: false,
    refetchInterval: 30000,
  });
}
export function scopedCrashes(
  page: KubernetesCrashPage | undefined,
  resource: KubernetesResource,
): KubernetesCrashEvent[] {
  return (page?.items ?? []).filter((item) =>
    resource.kind === "Pod"
      ? item.pod_uid === resource.uid
      : item.root_uid === resource.uid,
  );
}
export interface ResourceMetricSample {
  observedAt: number;
  cpu: number | null;
  memory: number | null;
  coverage?: { total: number; cpu: number; memory: number };
}
export function metricThreshold(percent: number | null) {
  return percent !== null && percent > 90
    ? "text-sev-critical"
    : percent !== null && percent > 75
      ? "text-sev-warning"
      : percent === null
        ? "text-ink-400"
        : "text-ink-50";
}
export function metricAllocation(
  workload: KubernetesWorkload | undefined,
  field: "cpu" | "memory",
  kind: "limits" | "requests",
  resource: KubernetesResource,
): number | null {
  const containers = workload?.containers ?? [];
  const amounts = containers.map((container) =>
    quantity(container[kind]?.[field]),
  );
  if (!amounts.length || amounts.some((amount) => amount === null)) return null;
  const perPod = amounts.reduce<number>(
    (sum, amount) => sum + (amount ?? 0),
    0,
  );
  const pods = resource.kind === "Pod" ? 1 : (workload?.pods?.length ?? 0);
  return pods > 0 ? perPod * pods : null;
}
export function mergeUsageHistory(
  histories: ResourceMetricSample[][],
): ResourceMetricSample[] {
  if (!histories.length) return [];
  const buckets = new Map<number, Map<number, ResourceMetricSample>>();
  histories.forEach((samples, podIndex) =>
    samples.forEach((sample) => {
      if (!Number.isFinite(sample.observedAt)) return;
      const key = Math.floor(sample.observedAt / 60000);
      const members =
        buckets.get(key) ?? new Map<number, ResourceMetricSample>();
      const existing = members.get(podIndex);
      if (!existing || sample.observedAt >= existing.observedAt)
        members.set(podIndex, sample);
      buckets.set(key, members);
    }),
  );
  return [...buckets]
    .map(([minute, members]) => {
      const samples = [...members.values()];
      const cpu = samples.flatMap((sample) =>
        sample.cpu !== null && Number.isFinite(sample.cpu) ? [sample.cpu] : [],
      );
      const memory = samples.flatMap((sample) =>
        sample.memory !== null && Number.isFinite(sample.memory)
          ? [sample.memory]
          : [],
      );
      return {
        observedAt: minute * 60000,
        cpu: cpu.length ? cpu.reduce((sum, value) => sum + value, 0) : null,
        memory: memory.length
          ? memory.reduce((sum, value) => sum + value, 0)
          : null,
        coverage: {
          total: histories.length,
          cpu: cpu.length,
          memory: memory.length,
        },
      };
    })
    .sort((a, b) => a.observedAt - b.observedAt);
}
export function logContainers(
  resource: KubernetesResource,
): Array<{ name: string; type: string }> {
  const value = resource.summary?.log_containers;
  const listed = Array.isArray(value)
    ? value.filter((item): item is { name: string; type: string } =>
        Boolean(
          item &&
            typeof item === "object" &&
            typeof item.name === "string" &&
            /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(item.name) &&
            item.name.length <= 63 &&
            ["regular", "init", "ephemeral"].includes(item.type),
        ),
      )
    : [];
  const states = resource.summary?.log_container_states;
  if (!Array.isArray(states)) return listed;
  const all = new Map(listed.map((item) => [item.name, item]));
  states.forEach((item: unknown) => {
    if (
      !item ||
      typeof item !== "object" ||
      !("name" in item) ||
      typeof item.name !== "string" ||
      item.name.length > 63 ||
      !/^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(item.name)
    )
      return;
    all.set(item.name, {
      name: item.name,
      type: "init" in item && item.init === true ? "init" : "regular",
    });
  });
  return [...all.values()];
}
export function podColour(index: number): string {
  return `log-pod-tone-${Math.abs(index) % 4}`;
}

/** Each viewer owns one palette; admission order, not a hash, keeps the first four Pods distinct. */
export function createPodPalette() {
  const indices = new Map<string, number>();
  return (name: string) => {
    if (!indices.has(name)) indices.set(name, indices.size);
    return podColour(indices.get(name)!);
  };
}

export function resourceRestarts(resource: KubernetesResource, workload?: KubernetesWorkload): number | null {
  const count = (value: unknown) => {
    if (typeof value !== "number" && !(typeof value === "string" && /^\d+$/.test(value))) return null;
    const number = Number(value);
    return Number.isSafeInteger(number) && number >= 0 ? number : null;
  };
  if (workload?.pods?.length) {
    const counts = workload.pods.map((pod) => count(pod.restart_count));
    return counts.every((value) => value !== null) ? counts.reduce<number>((sum, value) => sum + (value ?? 0), 0) : null;
  }
  return count(resource.summary?.restart_count ?? resource.summary?.restarts);
}
