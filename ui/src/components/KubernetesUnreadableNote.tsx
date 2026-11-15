import { ExternalLink } from "lucide-react";
import type { KubernetesUnreadableSummary } from "@/lib/kubernetesSyncHealth";
import { namespaceAccessDocsUrl } from "@/lib/kubernetesNamespaceAccess";

/**
 * What this connector does not read, for the top-bar sync details: the kinds
 * its identity may not read, and the kinds the configuration excludes.
 *
 * Neither is a sync fault, so nothing here uses warning colours. A kind both
 * excluded and refused is named only as excluded, because the configuration is
 * the reason it is missing. Renders nothing when every kind is read.
 */
export function KubernetesReadCoverage({ unreadable, notCollected, namespaceScoped = [], className = "" }: { unreadable: readonly KubernetesUnreadableSummary[]; notCollected: readonly string[]; namespaceScoped?: readonly KubernetesUnreadableSummary[]; className?: string }) {
  const excluded = new Set(notCollected);
  const refused = unreadable.filter((kind) => !excluded.has(kind.kind));
  const scoped = namespaceScoped.filter((kind) => !excluded.has(kind.kind));
  if (!refused.length && !notCollected.length && !scoped.length) return null;
  return <div data-read-coverage className={`space-y-2 text-xs text-ink-300 ${className}`}>
    {refused.length > 0 && <div data-unreadable-note>
      <p className="font-medium text-ink-200">Not readable with this connector identity:</p>
      <ul aria-label="Kinds not readable" className="mt-1 space-y-0.5">
        {refused.map((kind) => <li key={kind.kind} data-unreadable-kind={kind.kind}><span className="text-ink-100">{kind.kind}</span><span className="text-ink-400"> — {kind.reason}</span></li>)}
      </ul>
      <p className="mt-1 text-ink-400">Everything else is shown.</p>
    </div>}
    {scoped.length > 0 && <p data-namespace-scoped className="text-ink-400">Readable only in some namespaces: <span className="text-ink-300">{scoped.map((kind) => kind.kind).join(", ")}</span></p>}
    {notCollected.length > 0 && <p data-not-collected className="text-ink-400">Not collected by configuration: <span className="text-ink-300">{notCollected.join(", ")}</span></p>}
    <a className="text-link inline-flex items-center gap-1" href={namespaceAccessDocsUrl} target="_blank" rel="noopener noreferrer">Reader permissions and exclusions <ExternalLink size={12} aria-hidden="true" /></a>
  </div>;
}
