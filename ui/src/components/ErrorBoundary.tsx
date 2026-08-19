import { Component } from "react";
import type { ErrorInfo, ReactNode } from "react";
import { QueryErrorResetBoundary, useQueryClient } from "@tanstack/react-query";
import { AlertTriangle, RefreshCw } from "lucide-react";

// ErrorBoundary contains a render-time throw to the subtree it wraps instead of
// letting React unmount the whole app to a blank document. Callers layer it:
// once at the root (shell-level throws) and once around the routed outlet, keyed
// by pathname so navigating away resets the boundary.
interface Props {
  children: ReactNode;
  /** Optional lead-in, e.g. "Couldn't render this page". */
  context?: string;
  // onReset runs before the boundary clears its own error, and again if the
  // boundary unmounts while still showing one. Clearing boundary state alone
  // cannot recover a throw that came from cached data: the subtree re-renders
  // from the same poisoned value and throws again.
  onReset?: () => void;
}

interface State {
  error: Error | null;
}

export class ErrorBoundary extends Component<Props, State> {
  state: State = { error: null };

  static getDerivedStateFromError(error: unknown): State {
    return { error: error instanceof Error ? error : new Error(String(error)) };
  }

  componentDidCatch(error: unknown, info: ErrorInfo) {
    console.error("Unhandled render error", error, info.componentStack);
  }

  // Navigating away from a crashed page unmounts this boundary (the caller keys
  // it by pathname), so the same cleanup has to run there — otherwise coming
  // back replays the throw off the untouched cache.
  componentWillUnmount() {
    if (this.state.error) this.props.onReset?.();
  }

  private reset = () => {
    this.props.onReset?.();
    this.setState({ error: null });
  };

  render() {
    const { error } = this.state;
    if (!error) return this.props.children;

    return (
      <div className="flex-1 overflow-auto p-6" data-testid="error-boundary">
        <div className="card max-w-2xl p-4" role="alert">
          <div className="flex items-start gap-2">
            <AlertTriangle size={16} className="mt-0.5 shrink-0 text-sev-critical" />
            <div className="min-w-0">
              <h2 className="text-sm font-semibold text-ink-50">
                {this.props.context ?? "Something went wrong on this page"}
              </h2>
              <p className="mt-1 break-words text-xs text-ink-300">
                The rest of the app is still running — you can retry, or move to
                another page from the sidebar.
              </p>
            </div>
          </div>
          <div
            className="mt-3 break-words rounded-md border border-sev-critical/40 bg-sev-critical/10 p-3 text-xs text-sev-critical"
            data-testid="error-boundary-message"
          >
            {error.message || "Unknown error"}
          </div>
          <div className="mt-3 flex flex-wrap gap-2">
            <button
              className="btn btn-primary"
              onClick={this.reset}
              data-testid="error-boundary-retry"
            >
              <RefreshCw size={12} />
              Try again
            </button>
            <button
              className="btn"
              onClick={() => window.location.reload()}
              data-testid="error-boundary-reload"
            >
              Reload
            </button>
          </div>
        </div>
      </div>
    );
  }
}

// QueryErrorBoundary is the boundary as it must be used inside a
// QueryClientProvider. The commonest render throw in this app is a malformed
// payload sitting in the query cache, which the subtree re-reads synchronously
// on the next render — and because the throw happens during render, the query
// observer never mounts long enough to schedule a refetch. Recovery therefore
// resets the cache as well: QueryErrorResetBoundary clears errors thrown to the
// boundary, and resetQueries drops the cached data so the remounted subtree
// fetches again.
export function QueryErrorBoundary({
  children,
  context,
}: Omit<Props, "onReset">) {
  const qc = useQueryClient();
  return (
    <QueryErrorResetBoundary>
      {({ reset }) => (
        <ErrorBoundary
          context={context}
          onReset={() => {
            reset();
            void qc.resetQueries();
          }}
        >
          {children}
        </ErrorBoundary>
      )}
    </QueryErrorResetBoundary>
  );
}
