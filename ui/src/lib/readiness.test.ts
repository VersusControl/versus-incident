import { describe, it, expect } from "vitest";
import type { Readiness } from "./api";
import { deriveReadiness } from "./readiness";

// base returns a Readiness with sensible defaults so each test tweaks only the
// field under test.
function base(over: Partial<Readiness> = {}): Readiness {
  return { ready: false, seen: 0, needed: 100, rate_per_min: 0, ...over };
}

describe("deriveReadiness", () => {
  it("ready → terminal state, no remaining/eta needed", () => {
    const d = deriveReadiness(base({ ready: true, seen: 100, needed: 100 }));
    expect(d.state).toBe("ready");
    expect(d.ready).toBe(true);
    expect(d.etaMinutes).toBeNull();
    // warm-bucket early-confidence: ready even though seen < needed.
    const warm = deriveReadiness(base({ ready: true, seen: 4, needed: 20 }));
    expect(warm.state).toBe("ready");
  });

  it("learning with a rate → remaining, progress and an ETA", () => {
    const d = deriveReadiness(base({ seen: 40, needed: 100, rate_per_min: 2 }));
    expect(d.state).toBe("learning");
    expect(d.remaining).toBe(60); // max(0, 100 - 40)
    expect(d.progress).toBeCloseTo(0.4); // 40 / 100
    expect(d.etaMinutes).toBeCloseTo(30); // 60 remaining / 2 per min
    expect(d.indeterminate).toBe(false);
    expect(d.stalled).toBe(false);
  });

  it("learning with rate 0 → stalled, progress but no ETA", () => {
    const d = deriveReadiness(base({ seen: 12, needed: 20, rate_per_min: 0 }));
    expect(d.state).toBe("stalled");
    expect(d.stalled).toBe(true);
    expect(d.remaining).toBe(8);
    expect(d.progress).toBeCloseTo(0.6);
    expect(d.etaMinutes).toBeNull();
  });

  // Defensive only: the server always ships a positive `needed` (a non-positive
  // auto_promote_after is normalized to the default upstream), but deriveReadiness
  // must still stay sane if a zero target ever appears rather than dividing by zero.
  it("needed === 0 → stays robust: no remaining/progress/eta", () => {
    const d = deriveReadiness(base({ seen: 5, needed: 0, rate_per_min: 3 }));
    expect(d.indeterminate).toBe(true);
    expect(d.remaining).toBeNull();
    expect(d.progress).toBeNull();
    expect(d.etaMinutes).toBeNull();
  });

  it("needed === 0 but ready (hand-marked known) → ready, not indeterminate", () => {
    const d = deriveReadiness(base({ ready: true, seen: 3, needed: 0 }));
    expect(d.state).toBe("ready");
    expect(d.indeterminate).toBe(false);
  });

  it("remaining clamps at 0 and suppresses the ETA when seen ≥ needed", () => {
    const d = deriveReadiness(base({ seen: 120, needed: 100, rate_per_min: 5 }));
    expect(d.remaining).toBe(0);
    expect(d.etaMinutes).toBeNull(); // no remaining ⇒ no honest ETA
    expect(d.state).toBe("stalled");
  });
});
