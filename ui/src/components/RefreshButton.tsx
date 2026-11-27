import { AlertTriangle, RefreshCw } from "lucide-react";
import clsx from "clsx";

export const REFRESH_FAILED_TEXT = "Couldn't refresh · showing the last loaded data";

// RefreshButton is the header's manual refresh. When a background refresh
// fails while data is already on screen, the same button turns into the
// "Couldn't refresh · Retry" notice: an amber warning icon with the message in
// its name and tooltip, announced politely. It keeps its size, so a failed
// poll never moves the page; the full error card is only for a first load
// that returned nothing.
export function RefreshButton({
  onRefresh,
  refreshing,
  failed,
}: {
  onRefresh: () => void;
  refreshing: boolean;
  failed: boolean;
}) {
  const label = failed ? "Couldn't refresh. Retry" : "Refresh now";
  return (
    <>
      <button
        type="button"
        aria-label={label}
        title={failed ? `${REFRESH_FAILED_TEXT}. Retry` : "Refresh"}
        data-refresh-failed={failed || undefined}
        className={clsx(
          "rounded-control p-1.5 hover:bg-ink-700",
          failed ? "text-sev-warn" : "text-ink-300 hover:text-ink-100",
        )}
        onClick={onRefresh}
      >
        {failed && !refreshing ? (
          <AlertTriangle size={15} aria-hidden />
        ) : (
          <RefreshCw
            size={15}
            aria-hidden
            className={refreshing ? "animate-spin" : undefined}
          />
        )}
      </button>
      <span role="status" className="sr-only">
        {failed ? REFRESH_FAILED_TEXT : ""}
      </span>
    </>
  );
}
