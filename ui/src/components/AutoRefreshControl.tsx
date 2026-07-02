import { RefreshCw } from "lucide-react";
import clsx from "clsx";
import { Dropdown } from "./Dropdown";
import {
  AUTO_REFRESH_SECONDS,
  type AutoRefreshState,
} from "@/lib/useAutoRefresh";

// AutoRefreshControl — a checkbox that turns polling on, plus an interval
// picker (5 / 15 / 30s) shown only while it's on. Pair it with useAutoRefresh
// and pass state.refetchInterval to the page's query.
export function AutoRefreshControl({ state }: { state: AutoRefreshState }) {
  return (
    <div className="flex items-center gap-2">
      <label className="flex cursor-pointer select-none items-center gap-1.5 text-xs text-ink-200">
        <input
          type="checkbox"
          className="h-3.5 w-3.5 accent-accent"
          checked={state.enabled}
          onChange={(e) => state.setEnabled(e.target.checked)}
        />
        <RefreshCw
          size={12}
          className={clsx(state.enabled ? "text-accent" : "text-ink-400")}
          aria-hidden
        />
        Auto-refresh
      </label>
      {state.enabled && (
        <Dropdown
          aria-label="Auto-refresh interval"
          className="w-28"
          value={String(state.seconds)}
          onChange={(v) => state.setSeconds(Number(v))}
          options={AUTO_REFRESH_SECONDS.map((s) => ({
            value: String(s),
            label: `every ${s}s`,
          }))}
        />
      )}
    </div>
  );
}
