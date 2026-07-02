import type { Readiness } from "./api";

// Pure, UI-only derivations for the shared "learning readiness / time-to-known"
// status cell. The server ships facts (ready / seen / needed / rate_per_min);
// everything the operator sees — remaining evidence, progress fraction, and the
// caveated time estimate — is derived HERE and nowhere else, so the three
// tables (logs / metrics / traces) stay identical. See
// plans/productization/sre/signal-time-to-known-design.md §3.1 and §5.

export type ReadinessState = "ready" | "learning" | "indeterminate" | "stalled";

export interface DerivedReadiness {
  state: ReadinessState;
  ready: boolean;
  indeterminate: boolean; // needed === 0 && !ready → no count gate applies
  stalled: boolean; // !ready, has a target, but no honest rate → no ETA
  remaining: number | null; // max(0, needed - seen); null when indeterminate
  progress: number | null; // seen / needed; null when indeterminate
  etaMinutes: number | null; // remaining / rate; null when no honest estimate
}

// deriveReadiness turns the raw server facts into the values the cell renders.
// The rules are exactly those in the design:
//   remaining   = max(0, needed - seen)            (needed > 0)
//   progress    = seen / needed                    (needed > 0)
//   etaMinutes  = remaining / rate_per_min         (rate > 0, needed > 0, !ready)
// needed === 0 is the indeterminate sentinel; rate_per_min === 0 means no ETA.
export function deriveReadiness(r: Readiness): DerivedReadiness {
  const ready = r.ready;
  const hasTarget = r.needed > 0;
  const indeterminate = !ready && !hasTarget;

  const remaining = hasTarget ? Math.max(0, r.needed - r.seen) : null;
  const progress = hasTarget ? r.seen / r.needed : null;

  const etaMinutes =
    !ready && hasTarget && r.rate_per_min > 0 && remaining !== null && remaining > 0
      ? remaining / r.rate_per_min
      : null;

  const stalled = !ready && hasTarget && etaMinutes === null;

  let state: ReadinessState;
  if (ready) state = "ready";
  else if (indeterminate) state = "indeterminate";
  else if (stalled) state = "stalled";
  else state = "learning";

  return { state, ready, indeterminate, stalled, remaining, progress, etaMinutes };
}
