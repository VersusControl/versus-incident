import { describe, it, expect } from "vitest";
import type { Readiness } from "./api";
import { deriveReadiness, humanizeMinutes } from "./readiness";

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

  it("needed === 0 → indeterminate: no remaining/progress/eta", () => {
    const d = deriveReadiness(base({ seen: 5, needed: 0, rate_per_min: 3 }));
    expect(d.state).toBe("indeterminate");
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

describe("humanizeMinutes", () => {
  it("sub-minute → ~<1m", () => {
    expect(humanizeMinutes(0.4)).toBe("~<1m");
    expect(humanizeMinutes(0)).toBe("~<1m");
    expect(humanizeMinutes(-3)).toBe("~<1m");
  });

  it("minutes under an hour → ~Nm (rounded)", () => {
    expect(humanizeMinutes(12)).toBe("~12m");
    expect(humanizeMinutes(59.4)).toBe("~59m");
  });

  it("hours → ~Hh Mm, dropping a zero minute", () => {
    expect(humanizeMinutes(130)).toBe("~2h 10m");
    expect(humanizeMinutes(90)).toBe("~1h 30m");
    expect(humanizeMinutes(120)).toBe("~2h");
  });

  it("non-finite → ~<1m (never ~NaNm / ~∞)", () => {
    expect(humanizeMinutes(Infinity)).toBe("~<1m");
    expect(humanizeMinutes(NaN)).toBe("~<1m");
  });
});
