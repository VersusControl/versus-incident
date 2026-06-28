import { ApiError, type SLORecommendationSLI } from "@/lib/api";

// sloAdvisor.ts — pure presentation logic for the SLI/SLO auto-define page
// (epic X29), extracted so it is unit-testable in isolation (the project's
// lib/*.ts + lib/*.test.ts convention).

// isLockedStatus reports whether an error is the terminal Enterprise-locked
// state: 403 (unlicensed) or 404 (OSS binary — endpoint absent). Both render
// the locked upsell, never a retry.
export function isLockedStatus(err: unknown): boolean {
  return err instanceof ApiError && (err.status === 403 || err.status === 404);
}

// formatObjective renders an SLI target in the right units: latency as a
// millisecond target, every other type as a percentage of a ratio in (0,1).
export function formatObjective(s: SLORecommendationSLI): string {
  if (s.type === "latency") {
    return `${Math.round(s.objective)} ms`;
  }
  const pct = s.objective * 100;
  const digits = s.objective >= 0.999 ? 2 : pct % 1 === 0 ? 0 : 1;
  return `${pct.toFixed(digits)}%`;
}

// formatConfidence renders a 0–1 confidence as a whole percentage.
export function formatConfidence(c: number | undefined): string {
  return `${Math.round((c ?? 0) * 100)}%`;
}

// cadenceDirty reports whether the edited cadence differs from the current one,
// so the Save button can stay disabled when nothing changed.
export function cadenceDirty(draft: string, current: string): boolean {
  return draft.trim() !== "" && draft.trim() !== current;
}

// DEFAULT_OFF_REASON is the fallback shown when the AI gate is closed but the
// server supplied no reason (kept in sync with the Go offReasonAI).
export const DEFAULT_OFF_REASON =
  "SLI/SLO auto-define is OFF: enable AI and configure an API key to use it.";

// EnableToggleState is the resolved state of the "Enable SLI/SLO auto-define"
// toggle. `checked` reflects the persisted per-org feature flag; `disabled` is
// true (with a `reason`) when the AI hard gate is closed, so a user cannot turn
// the feature on until AI is enabled and an API key is configured.
export interface EnableToggleState {
  checked: boolean;
  disabled: boolean;
  reason?: string;
}

// enableToggleState gates the feature toggle on the AI hard gate: when AI is OFF
// the toggle is DISABLED and shows the off-reason (the user must configure AI
// first); when AI is ON the toggle is interactive. `checked` always reflects the
// persisted feature flag (which may be true even while AI is off, e.g. AI was
// later turned off) so the UI never misrepresents stored state.
export function enableToggleState(
  aiEnabled: boolean,
  featureEnabled: boolean,
  offReason?: string,
): EnableToggleState {
  if (!aiEnabled) {
    return {
      checked: featureEnabled,
      disabled: true,
      reason: offReason || DEFAULT_OFF_REASON,
    };
  }
  return { checked: featureEnabled, disabled: false };
}
