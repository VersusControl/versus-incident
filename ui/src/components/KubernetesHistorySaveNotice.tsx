import { Clock, TriangleAlert } from "lucide-react";
import { historySaveDelay, historySaveLabel } from "@/lib/kubernetesSyncHealth";

// A note that saved history is behind while the cluster is still being read.
// The data beside it stays on screen; this is never an access or connector
// failure. A delay that clears by itself stays quiet. A save the history store
// refused for good needs an operator, so it takes the warning tone and carries
// the server's next step.
export function KubernetesHistorySaveNotice({ pages, className = "" }: { pages: readonly unknown[]; className?: string }) {
  const delay = historySaveDelay(...pages);
  if (!delay) return null;
  const Icon = delay.rejected ? TriangleAlert : Clock;
  return <div role="status" data-history-save-notice={delay.rejected ? "rejected" : "delayed"} className={`flex min-w-0 items-start gap-2 text-xs ${delay.rejected ? "text-sev-warning" : "text-ink-300"} ${className}`}>
    <Icon size={12} aria-hidden="true" className={`mt-0.5 shrink-0 ${delay.rejected ? "" : "text-ink-400"}`} />
    <div className="min-w-0">
      <p title={delay.savedThrough ? new Date(delay.savedThrough).toLocaleString() : undefined}>{historySaveLabel(delay)}</p>
      {delay.rejected && delay.action && <p className="mt-0.5 text-ink-300">{delay.action}</p>}
    </div>
  </div>;
}
