import { useId, useState, type ReactNode } from "react";
import { ChevronDown, ChevronRight } from "lucide-react";
type KubernetesWarning = { id: string; content: ReactNode };

export function KubernetesWarnings({ warnings, scope, label = "Warnings" }: { warnings: KubernetesWarning[]; scope: string; label?: string }) {
  const unique = [...new Map(warnings.map((warning) => [warning.id, warning])).values()];
  return <WarningDisclosure key={scope} warnings={unique} label={label} />;
}

function WarningDisclosure({ warnings, label }: { warnings: KubernetesWarning[]; label: string }) {
  const [expanded, setExpanded] = useState(false);
  const id = useId();
  if (!warnings.length) return null;
  return <div className="min-w-0 text-xs">
    <button type="button" aria-expanded={expanded} aria-controls={id} onClick={() => setExpanded((value) => !value)} className="inline-flex min-h-6 items-center gap-1.5 rounded-control px-2 text-sev-warning focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent">
      {expanded ? <ChevronDown size={14} aria-hidden="true" /> : <ChevronRight size={14} aria-hidden="true" />}{label} ({warnings.length})
    </button>
    <ul id={id} hidden={!expanded} className="space-y-1 break-words px-2 pb-2 text-ink-300">{warnings.map((warning) => <li key={warning.id}>{warning.content}</li>)}</ul>
  </div>;
}