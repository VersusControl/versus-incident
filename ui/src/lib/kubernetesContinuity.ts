import { ApiError, type KubernetesSyncHealthReport } from "./api";

const published = Symbol("published Kubernetes observation");
type Snapshot = { sync?: { state?: string; partial?: boolean; pending_kinds?: number; pending?: unknown[] }; partial_failures?: Array<{ class?: string }>; [published]?: boolean };

function object(value: unknown): Record<string, unknown> | undefined {
  return value !== null && typeof value === "object" ? value as Record<string, unknown> : undefined;
}

export function kubernetesFailureCode(error: unknown): string | undefined {
  if (!(error instanceof ApiError)) return undefined;
  const body = object(error.body);
  const payload = object(body?.error) ?? body;
  return typeof payload?.code === "string" ? payload.code : undefined;
}

/** Only the structured startup response is warming; its message alone proves nothing. */
export function kubernetesInventoryNotReady(error: unknown): boolean {
  return error instanceof ApiError && error.status === 503 && kubernetesFailureCode(error) === "inventory_not_ready";
}

/** These failures cannot authorize replay of a previously successful response. */
export function kubernetesAccessLost(error: unknown): boolean {
  return error instanceof ApiError && ([401, 403, 404].includes(error.status)
    || ["credential_unavailable", "forbidden", "unauthorized", "namespace_scope", "cluster_identity_mismatch"].includes(kubernetesFailureCode(error) ?? ""));
}

export function kubernetesReportAuthorityLost(report: KubernetesSyncHealthReport | undefined): boolean {
  if (report?.state !== "error") return false;
  if (["unauthorized", "forbidden", "credential_unavailable", "namespace_scope"].includes(report.class ?? "")) return true;
  return Object.values(report.kinds ?? {}).some(kind => !kind.absent && kind.cycle_failure === true
    && ["forbidden", "unauthorized"].includes(kind.error ?? ""));
}

/** Mixed projections are indivisible: a denied reference makes the whole cached result unusable. */
export function kubernetesQueryUsesKinds(parts: readonly unknown[], value: unknown, kinds: ReadonlySet<string>): boolean {
  if (!kinds.size) return false;
  // Unfinished mixed projections have not yet disclosed their dependencies.
  if (value === undefined && ["kubernetes-graph", "kubernetes-overview-graph", "kubernetes-neighborhood", "kubernetes-related", "kubernetes-detail", "kubernetes-issues", "kubernetes-palette", "kubernetes-timeline"].includes(String(parts[0]))) return true;
  const matches = (value: unknown) => typeof value === "string" && [...kinds].some(kind => {
    const name = kind.toLowerCase(), candidate = value.toLowerCase();
    return candidate === name || candidate.endsWith(`~${name}s`) || candidate.endsWith(`~${name}es`);
  });
  const seen = new WeakSet<object>();
  const visit = (value: unknown): boolean => {
    if (!value || typeof value !== "object" || seen.has(value)) return false;
    seen.add(value);
    if (Array.isArray(value)) return value.some(visit);
    const row = value as Record<string, unknown>;
    return [row.kind, row.root_kind, row.resource_id].some(matches) || Object.values(row).some(visit);
  };
  return parts.some(part => typeof part === "string" && part.includes("~") && matches(part) || visit(part)) || visit(value);
}

export function incompleteKubernetesSnapshot(value: unknown): boolean {
  const snapshot = value as Snapshot | undefined;
  return Boolean(snapshot?.sync && (!["ready", "direct"].includes(snapshot.sync.state ?? "") || snapshot.sync.partial
    || snapshot.sync.pending_kinds || snapshot.sync.pending?.length) || snapshot?.partial_failures?.length);
}

/** Query keys already fence cluster, namespace, filters, window and cursor. */
export function retainKubernetesPublication<T>(previous: T | undefined, incoming: T): T {
  const nextRecord = object(incoming);
  const next = nextRecord as Snapshot | undefined;
  // A successful permission-reduced response is just as authoritative as a denial.
  if (next?.partial_failures?.some((failure) => ["forbidden", "unauthorized", "namespace_scope", "credential_unavailable"].includes(failure.class ?? ""))) return incoming;
  const priorRecord = object(previous);
  if (nextRecord?.availability === "unavailable" && "pod_metrics" in nextRecord && priorRecord && ["available", "stale"].includes(String(priorRecord.availability))) {
    const stale = (source: unknown) => object(source) ? { ...object(source), availability: "stale", fresh: false } : source;
    return { ...priorRecord, availability: "stale", fresh: false, pod_metrics: stale(priorRecord.pod_metrics), node_metrics: stale(priorRecord.node_metrics) } as T;
  }
  if (!next?.sync) return incoming;
  const prior = priorRecord as Snapshot | undefined;
  if (incompleteKubernetesSnapshot(next)) {
    if (prior && (prior[published] || !incompleteKubernetesSnapshot(prior) || hasProjectedObservation(prior))) {
      return { ...prior, sync: next.sync, partial_failures: next.partial_failures, [published]: true } as T;
    }
    return incoming;
  }
  // A complete empty publication is replacement data, not a reason to keep rows.
  return { ...next, [published]: true } as T;
}

function hasProjectedObservation(value: unknown): boolean {
  const projection = object(value);
  return Boolean(projection && ([projection.items, projection.nodes, projection.edges].some((rows) => Array.isArray(rows) && rows.length > 0)
    || typeof projection.observed_at === "string" && Number.isFinite(Date.parse(projection.observed_at))
      && [projection.nodes, projection.pods, projection.workloads].some((count) => typeof count === "number" && count > 0)));
}

export function hasKubernetesPublication(value: unknown): boolean {
  const snapshot = value as Snapshot | undefined;
  if (snapshot?.[published]) return true;
  if (snapshot?.sync && (!snapshot.sync.state || snapshot.sync.state === "syncing")) return hasProjectedObservation(value);
  return snapshot?.sync?.state !== "error";
}

/** Only unfinished reads need a readiness-triggered retry, never arbitrary faults or denials. */
export function kubernetesReadNeedsRecovery(value: unknown, error: unknown): boolean {
  if (error) return error instanceof ApiError && error.status === 503
    && kubernetesFailureCode(error) === "inventory_not_ready" && object(error.body)?.retryable === true;
  const snapshot = value as Snapshot | undefined;
  if (snapshot?.partial_failures?.some((failure) => ["forbidden", "unauthorized", "namespace_scope", "credential_unavailable"].includes(failure.class ?? ""))) return false;
  return Boolean(snapshot?.sync && (!snapshot.sync.state || snapshot.sync.state === "syncing"));
}
