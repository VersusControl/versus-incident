import { ExternalLink } from "lucide-react";
import { historyOffMessage, metricsServerInstallUrl, namespaceAccessDocsUrl, type KubernetesHelmAccess } from "@/lib/kubernetesNamespaceAccess";

/**
 * History is not kept for a connector identity that reads only inside
 * namespaces. That is a property of the role, not a failure, so the notice is
 * one quiet sentence with a docs link and never offers Retry.
 */
export function KubernetesHistoryOffNotice({ className = "p-4" }: { className?: string }) {
  return <p role="status" data-history-off className={`text-sm text-ink-300 ${className}`}>
    {historyOffMessage}{" "}
    <a className="text-link inline-flex items-center gap-1" href={namespaceAccessDocsUrl} target="_blank" rel="noopener noreferrer">Learn why <ExternalLink size={12} aria-hidden="true" /></a>
  </p>;
}

/**
 * Helm keeps releases in Secrets, so a connector that may not list Secrets
 * cannot see any. The server's sentence says so in place of an empty list.
 */
export function KubernetesHelmAccessNotice({ access, className = "p-4" }: { access: KubernetesHelmAccess; className?: string }) {
  return <p role="status" data-helm-access="needs_secret_list" className={`text-sm text-ink-300 ${className}`}>
    {access.message}{" "}
    <a className="text-link inline-flex items-center gap-1" href={access.docsUrl} target="_blank" rel="noopener noreferrer">How to allow it <ExternalLink size={12} aria-hidden="true" /></a>
  </p>;
}

/** CPU and memory usage need the Metrics API; a calm setup line, not a failure. */
export function KubernetesMetricsServerNotice({ className = "" }: { className?: string }) {
  return <p role="status" data-metrics-server-missing className={`text-xs text-ink-300 ${className}`}>
    CPU and memory need metrics-server.{" "}
    <a className="text-link inline-flex items-center gap-1" href={metricsServerInstallUrl} target="_blank" rel="noopener noreferrer">How to install it <ExternalLink size={12} aria-hidden="true" /></a>
  </p>;
}
