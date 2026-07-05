// serviceGrace.ts — pure logic for the Services page's grace controls, kept
// free of React / fetch so the contextual action selection and the column
// labels are unit-testable in the project's lib/*.ts + lib/*.test.ts pattern.
//
// Grace is the new-service window during which a service is learned but not
// alerted on. The server computes it (in_grace + grace_seconds_remaining) with
// the SAME helper the service-detail endpoint uses, so the Services LIST and
// the service DETAIL page report the same status — this module only turns that
// status into the operator-facing action set + labels.

import { formatDuration } from "@/lib/format";

// GraceAction is the wire action the controlGrace endpoint accepts: "end"
// closes the grace window now; "restart" resets/re-enters it.
export type GraceAction = "end" | "restart";

// GRACE_ACTION_LABEL is the button copy for each grace action.
export const GRACE_ACTION_LABEL: Record<GraceAction, string> = {
  end: "End grace",
  restart: "Restart grace",
};

// graceActionsForSelection returns the CONTEXTUALLY-VALID grace actions for a
// selection of services, given whether each is currently in its grace window:
//   • a service IN grace   → "End grace",
//   • a service NOT in grace → "Restart grace" (re-enter).
// For a single service (or a uniform multi-selection) EXACTLY ONE action is
// offered — never both End and Restart at once (the founder's rule). A mixed
// multi-selection offers each action, since each applies to a different subset;
// the page routes "end" to the in-grace rows and "restart" to the rest. An
// empty selection offers nothing.
export function graceActionsForSelection(
  inGraceFlags: boolean[],
): GraceAction[] {
  if (inGraceFlags.length === 0) return [];
  const actions: GraceAction[] = [];
  if (inGraceFlags.some((g) => g)) actions.push("end");
  if (inGraceFlags.some((g) => !g)) actions.push("restart");
  return actions;
}

// graceRemainingLabel renders the Services table's Grace column: the remaining
// window while in grace, or "—" when the service is tracked (not in grace). It
// reads the SAME server-computed status the detail page shows, so the two
// surfaces never disagree.
export function graceRemainingLabel(
  inGrace: boolean,
  secondsRemaining: number,
): string {
  if (!inGrace) return "—";
  return formatDuration(Math.max(0, secondsRemaining) * 1000);
}
