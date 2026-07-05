import { describe, it, expect } from "vitest";
import {
  GRACE_ACTION_LABEL,
  graceActionsForSelection,
  graceRemainingLabel,
} from "@/lib/serviceGrace";

// Pure-logic tests for the Services page grace controls. The console has no DOM
// harness, so the two contracts that matter are pinned here:
//   • the CONTEXTUAL action selection — a service IN grace offers only "End
//     grace", one NOT in grace only "Restart grace", never both at once for a
//     single service (the founder's rule); a mixed multi-selection offers each
//     for its subset;
//   • the Grace column label reads the SAME server status the detail page uses.

describe("graceActionsForSelection", () => {
  it("empty selection offers nothing", () => {
    expect(graceActionsForSelection([])).toEqual([]);
  });

  it("a single service IN grace offers ONLY End grace (never both)", () => {
    expect(graceActionsForSelection([true])).toEqual(["end"]);
  });

  it("a single service NOT in grace offers ONLY Restart grace (never both)", () => {
    expect(graceActionsForSelection([false])).toEqual(["restart"]);
  });

  it("a uniform in-grace multi-selection offers only End grace", () => {
    expect(graceActionsForSelection([true, true, true])).toEqual(["end"]);
  });

  it("a uniform tracked multi-selection offers only Restart grace", () => {
    expect(graceActionsForSelection([false, false])).toEqual(["restart"]);
  });

  it("a MIXED selection offers both — each routes to its applicable subset", () => {
    expect(graceActionsForSelection([true, false])).toEqual(["end", "restart"]);
  });

  it("labels the actions for the bar", () => {
    expect(GRACE_ACTION_LABEL.end).toBe("End grace");
    expect(GRACE_ACTION_LABEL.restart).toBe("Restart grace");
  });
});

describe("graceRemainingLabel (Grace column)", () => {
  it("shows the remaining window while in grace", () => {
    // 1800s = 30 minutes → formatDuration renders m/ss.
    expect(graceRemainingLabel(true, 1800)).toBe("30m00s");
  });

  it("shows an em dash '—' once tracked (not in grace)", () => {
    expect(graceRemainingLabel(false, 0)).toBe("—");
    // Even if the server sent a stale positive remaining, "not in grace" wins.
    expect(graceRemainingLabel(false, 999)).toBe("—");
  });

  it("never renders a negative remaining while in grace", () => {
    // A just-expired-but-still-flagged edge clamps to 0, never a negative time.
    expect(graceRemainingLabel(true, -5)).toBe("0ms");
  });
});
