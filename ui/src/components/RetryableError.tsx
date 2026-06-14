import { RefreshCw } from "lucide-react";

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
  const msg = error instanceof Error ? error.message : String(error);
  return (
    <div
      role="alert"
      className="flex items-start justify-between gap-3 rounded-card border border-sev-critical/40 bg-sev-critical/10 p-3"
    >
      <div className="min-w-0 text-xs">
        {context && <div className="font-medium text-ink-50">{context}</div>}
        <div className="mt-0.5 break-words text-sev-critical">{msg}</div>
      </div>
      <button className="btn shrink-0" onClick={onRetry} disabled={retrying}>
        <RefreshCw size={12} className={retrying ? "animate-spin" : undefined} />
        {retrying ? "Retrying…" : "Retry"}
      </button>
    </div>
  );
}
