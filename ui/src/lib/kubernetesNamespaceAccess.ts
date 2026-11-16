import { createContext, useContext } from "react";
import { ApiError } from "./api";
import { kubernetesErrorCode } from "./kubernetesConnector";
import { incompleteKubernetesSnapshot } from "./kubernetesContinuity";

/** The setup walkthrough the "not configured" card links to. */
export const kubernetesQuickstartUrl = "https://docs.versusincident.com/#/agent/kubernetes/quickstart";

/** What a namespace-only reader role sees, and why history is not saved for it. */
export const namespaceAccessDocsUrl = "https://docs.versusincident.com/#/agent/kubernetes/configuration?id=least-privilege";

/** Why listing Helm releases needs list access to Secrets. */
export const helmSecretAccessDocsUrl = "https://docs.versusincident.com/#/agent/kubernetes/configuration?id=helm-releases-need-secret-access";

/** The `history` value of a history page for an identity that reads only inside namespaces. */
export const historyOffNamespaceAccess = "off_namespace_access";

/** The one sentence every history surface shows when no history is kept. */
export const historyOffMessage = "History is off for namespace-only access.";

/**
 * Whether the connector identity reads the cluster's namespaced kinds
 * cluster-wide and, when it does not, the namespaces it can read.
 */
export interface KubernetesNamespaceAccess {
  clusterWide: boolean;
  /** Sorted readable namespaces; empty while the identity reads cluster-wide. */
  namespaces: string[];
  /**
   * Some kinds are read only inside namespaces by design: the identity is
   * namespace-only, or the report names namespace-scoped kinds. A partial read
   * caused only by that gap still gives a real count.
   */
  partialByDesign?: boolean;
  /** Kinds the configuration excludes; never counted, not even as 0. */
  notCollected?: string[];
}

export const clusterWideAccess: KubernetesNamespaceAccess = { clusterWide: true, namespaces: [] };

/**
 * Reads namespace access from a sync report or an overview. A missing
 * `cluster_wide`, or `true`, means cluster-wide: only an explicit `false`
 * narrows the views, so an older server never hides data.
 */
export function namespaceAccess(...sources: Array<{ cluster_wide?: boolean | null; readable_namespaces?: string[] | null } | undefined | null>): KubernetesNamespaceAccess {
  const source = sources.find((candidate) => typeof candidate?.cluster_wide === "boolean");
  if (!source || source.cluster_wide !== false) return clusterWideAccess;
  const namespaces = [...new Set((source.readable_namespaces ?? []).filter((name): name is string => typeof name === "string" && name !== ""))].sort();
  return { clusterWide: false, namespaces };
}

export const KubernetesNamespaceAccessContext = createContext<KubernetesNamespaceAccess>(clusterWideAccess);

/** Namespace access for the cluster on screen; cluster-wide outside the explorer. */
export function useKubernetesNamespaceAccess(): KubernetesNamespaceAccess {
  return useContext(KubernetesNamespaceAccessContext);
}

type CountedSnapshot = {
  authority?: string;
  items?: unknown[];
  sync?: { state?: string; partial?: boolean; pending_kinds?: number; pending?: unknown[] };
  partial_failures?: Array<{ class?: string }> | null;
  truncated?: boolean;
};

/**
 * Whether the counts on a list answer are real. A count is unknown only when
 * the data behind it is unavailable: still loading, failed, stale, or partial
 * for a reason other than this identity's by-design namespace gap. A partial
 * answer whose only gaps are refused or namespace-scoped reads counts what it
 * could read, which for a scoped identity is exactly what it shows.
 */
export function countsKnown(access: KubernetesNamespaceAccess, snapshot: unknown, fault = false): boolean {
  if (!snapshot || typeof snapshot !== "object" || fault) return false;
  const page = snapshot as CountedSnapshot;
  if (page.authority === "current" || page.authority === "stale") return true;
  if (!incompleteKubernetesSnapshot(page)) return true;
  if (!access.partialByDesign) return false;
  const sync = page.sync;
  if (sync && (!["ready", "direct", "partial"].includes(sync.state ?? "") || sync.pending_kinds || sync.pending?.length)) return false;
  return (page.partial_failures ?? []).every((failure) => ["forbidden", "namespace_scope"].includes(failure.class ?? ""));
}

/** The namespaces named in a status line, shortened past three. */
export function readableNamespaceList(namespaces: readonly string[]): string {
  if (!namespaces.length) return "no namespaces yet";
  if (namespaces.length <= 3) return namespaces.join(", ");
  return `${namespaces.slice(0, 3).join(", ")} and ${namespaces.length - 3} more`;
}

/** The steady top-bar status for a namespace-only identity. */
export function namespaceAccessLabel(namespaces: readonly string[]): string {
  return `Namespace access — showing ${readableNamespaceList(namespaces)}`;
}

/** The label for the empty namespace choice: every namespace this identity can read. */
export function allNamespacesLabel(access: KubernetesNamespaceAccess): string {
  return access.clusterWide ? "All namespaces" : "All readable namespaces";
}

/** A history page the server answered as off for namespace-only access. */
export function historyOff(page: unknown): boolean {
  return Boolean(page && typeof page === "object" && (page as { history?: unknown }).history === historyOffNamespaceAccess);
}

/**
 * Whether a history surface should say history is off: the server said so, or
 * the identity is namespace-only and the history read was refused because no
 * authority was ever established for it.
 */
export function historyOffFor(access: KubernetesNamespaceAccess, page: unknown, error?: unknown): boolean {
  if (historyOff(page)) return true;
  if (access.clusterWide || !(error instanceof ApiError)) return false;
  const code = kubernetesErrorCode(error.body);
  return code === "history_authority_denied" || code === "history_not_established" || code === "authority_denied" || code === "authority_unknown";
}

/** Why Helm releases cannot be listed, in the server's words. */
export interface KubernetesHelmAccess {
  message: string;
  docsUrl: string;
}

const helmAccessFallback = "Helm releases need list access to Secrets. Versus reads only their labels, never their contents.";

function safeDocsUrl(value: unknown): string {
  if (typeof value !== "string") return helmSecretAccessDocsUrl;
  try {
    const url = new URL(value);
    return url.protocol === "https:" ? url.toString() : helmSecretAccessDocsUrl;
  } catch {
    return helmSecretAccessDocsUrl;
  }
}

/**
 * The reason Helm releases could not be read, from a release page that says
 * `access: "needs_secret_list"` or a refused read coded
 * `helm_needs_secret_list`. An empty list is never presented as "no releases"
 * while this answers.
 */
export function helmAccessDenial(page: unknown, error?: unknown): KubernetesHelmAccess | undefined {
  const record = page && typeof page === "object" ? page as { access?: unknown; access_message?: unknown; docs_url?: unknown } : undefined;
  if (record?.access === "needs_secret_list") {
    const message = typeof record.access_message === "string" && record.access_message.trim() ? record.access_message.trim() : helmAccessFallback;
    return { message, docsUrl: safeDocsUrl(record.docs_url) };
  }
  if (error instanceof ApiError && kubernetesErrorCode(error.body) === "helm_needs_secret_list") {
    return { message: error.message && !error.message.startsWith("HTTP ") ? error.message : helmAccessFallback, docsUrl: helmSecretAccessDocsUrl };
  }
  return undefined;
}

/** A connector fault the server explained with a message and an action. */
export interface KubernetesConnectorDetail {
  code: string;
  message: string;
  action?: string;
}

const explainedConnectorCodes = new Set(["connector_configuration_invalid", "connector_endpoint_refused"]);

/**
 * The first error that carries the server's explanation of why the connector
 * could not be built or why its address was refused. Its message and action
 * are written for an operator and never carry a secret.
 */
export function connectorSetupDetail(...errors: unknown[]): KubernetesConnectorDetail | undefined {
  for (const error of errors) {
    if (!(error instanceof ApiError)) continue;
    const code = kubernetesErrorCode(error.body);
    if (!code || !explainedConnectorCodes.has(code)) continue;
    const body = error.body as { action?: unknown };
    const action = typeof body.action === "string" && body.action.trim() ? body.action.trim() : undefined;
    return { code, message: error.message && !error.message.startsWith("HTTP ") ? error.message : "The Kubernetes connector could not be started from its configuration.", action };
  }
  return undefined;
}

/** Kind names the server reported as excluded by configuration, once each and sorted. */
export function notCollectedKinds(value?: readonly unknown[] | null): string[] {
  return [...new Set((value ?? []).filter((kind): kind is string => typeof kind === "string" && kind.trim() !== "").map((kind) => kind.trim()))].sort();
}

/** Where to install the Kubernetes Metrics API that CPU and memory usage come from. */
export const metricsServerInstallUrl = "https://github.com/kubernetes-sigs/metrics-server#installation";

/** The server's word on the Metrics API behind CPU and memory usage. */
export type KubernetesMetricsState = "not_installed" | "forbidden" | "unavailable" | "syncing" | "available";

/** Reads `metrics` from a usage or top answer; undefined when it is not sent. */
export function metricsState(page: unknown): KubernetesMetricsState | undefined {
  const value = page && typeof page === "object" ? (page as { metrics?: unknown }).metrics : undefined;
  return value === "not_installed" || value === "forbidden" || value === "unavailable" || value === "syncing" || value === "available" ? value : undefined;
}

/** The cluster has no Metrics API, so usage is a setup step rather than a fault. */
export function metricsApiMissing(page: unknown): boolean {
  return metricsState(page) === "not_installed";
}
