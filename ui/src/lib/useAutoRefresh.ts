import { useState } from "react";

// AUTO_REFRESH_SECONDS — the intervals an operator can pick. Default is the
// first (5s); 30s is the slowest.
export const AUTO_REFRESH_SECONDS = [5, 15, 30] as const;

export interface AutoRefreshState {
  enabled: boolean;
  setEnabled: (v: boolean) => void;
  seconds: number;
  setSeconds: (v: number) => void;
  // refetchInterval feeds straight into a TanStack useQuery: a number of ms
  // while on, or false to stop polling.
  refetchInterval: number | false;
}

// useAutoRefresh holds the on/off + interval state for a page's auto-refresh
// control and derives the refetchInterval to hand to useQuery. Off by default —
// polling only starts once the operator opts in.
export function useAutoRefresh(defaultSeconds = 5): AutoRefreshState {
  const [enabled, setEnabled] = useState(false);
  const [seconds, setSeconds] = useState(defaultSeconds);
  return {
    enabled,
    setEnabled,
    seconds,
    setSeconds,
    refetchInterval: enabled ? seconds * 1000 : false,
  };
}
