import { RefreshCw } from "lucide-react";
import { ApiError } from "@/lib/api";
import { historyStoreDownMessage, kubernetesHistoryStoreDown, kubernetesReconnecting, reconnectingMessage } from "@/lib/kubernetesLiveFailure";

// RetryableError replaces every bare ErrorBox on query failures — the audit
// found zero retry affordances in the entire app. Message + cause + a Retry
// button wired to react-query's refetch.
export function RetryableError({
  error,
  onRetry,
  retrying,
  context,
}: {
  error: unknown;
  onRetry: () => void;
  retrying?: boolean;
  /** Optional "what failed" lead-in, e.g. "Couldn't load incidents". */
  context?: string;
}) {
  // Two Kubernetes answers are passing states, not faults: a reconnect in
  // progress and a history store that is briefly away. They read calmly and
  // never carry the "Couldn't load" lead-in or the server's remedy, which
  // are written for a real failure.
  const calm = kubernetesReconnecting(error) ? reconnectingMessage : kubernetesHistoryStoreDown(error) ? `${historyStoreDownMessage}.` : undefined;
  if (calm) return (
    <div role="status" data-calm-failure className="flex items-center justify-between gap-3 rounded-card border border-ink-600 bg-ink-800/60 p-3 text-xs text-ink-300">
      <div className="min-w-0 break-words">{calm}</div>
      <button className="btn shrink-0" onClick={onRetry} disabled={retrying}>
        <RefreshCw size={12} className={retrying ? "animate-spin" : undefined} />
        {retrying ? "Retrying…" : "Retry"}
      </button>
    </div>
  );
  const msg = error instanceof Error ? error.message : String(error);
  const action = error instanceof ApiError && error.body && typeof error.body === "object" && "action" in error.body && typeof error.body.action === "string"
    ? error.body.action
    : "";
  return (
    <div
      role="alert"
      className="flex items-start justify-between gap-3 rounded-card border border-sev-critical/40 bg-sev-critical/10 p-3"
    >
      <div className="min-w-0 text-xs">
        {context && <div className="font-medium text-ink-50">{context}</div>}
        <div className="mt-0.5 break-words text-sev-critical">{msg}</div>
        {action && <div className="mt-1 break-words text-ink-200">{action}</div>}
      </div>
      <button className="btn shrink-0" onClick={onRetry} disabled={retrying}>
        <RefreshCw size={12} className={retrying ? "animate-spin" : undefined} />
        {retrying ? "Retrying…" : "Retry"}
      </button>
    </div>
  );
}
