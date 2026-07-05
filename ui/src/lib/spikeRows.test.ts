import { describe, it, expect } from "vitest";
import { buildSpikeRows } from "@/lib/spikeRows";
import type { DetectEvent, ShadowEvent } from "@/lib/api";

// 2.3 — the Decisions "Spike" view merges BOTH decision logs. The bug it fixes:
// a detect-mode spike (the kind that fires an AI-detect incident) lives in the
// DETECT log, never the shadow log, so a shadow-only Spike view hid it — and
// showed nothing at all while the agent ran in detect mode. buildSpikeRows is
// the seam that surfaces every spike regardless of which mode produced it.

function detectEvent(over: Partial<DetectEvent>): DetectEvent {
  return {
    id: "d1",
    timestamp: "2026-07-04T10:00:00Z",
    source: "logs",
    pattern_id: "p-detect",
    template: "tmpl",
    verdict: "spike",
    frequency: 12,
    baseline: 2,
    outcome: "emitted",
    ...over,
  };
}

function shadowEvent(over: Partial<ShadowEvent>): ShadowEvent {
  return {
    pattern_id: "p-shadow",
    template: "tmpl",
    source: "logs",
    verdict: "spike",
    sample_message: "would-have-alerted line",
    count: 7,
    occurrences: 3,
    first_seen: "2026-07-04T09:00:00Z",
    last_seen: "2026-07-04T09:30:00Z",
    ...over,
  };
}

describe("buildSpikeRows — merges detect + shadow spikes", () => {
  it("includes detect-mode spikes (the ones that fire incidents), not just shadow", () => {
    const rows = buildSpikeRows(
      [detectEvent({ id: "d-spike", verdict: "spike", service: "api" })],
      [],
    );
    expect(rows).toHaveLength(1);
    expect(rows[0].kind).toBe("detect");
    expect(rows[0].service).toBe("api");
    expect(rows[0].href).toBe("/agent/decisions/detect/d-spike");
  });

  it("includes shadow-mode spikes too", () => {
    const rows = buildSpikeRows(
      [],
      [shadowEvent({ pattern_id: "p9", verdict: "spike" })],
    );
    expect(rows).toHaveLength(1);
    expect(rows[0].kind).toBe("shadow");
    expect(rows[0].href).toBe("/agent/decisions/shadow/p9");
  });

  it("drops non-spike verdicts from BOTH logs", () => {
    const rows = buildSpikeRows(
      [
        detectEvent({ id: "d-known", verdict: "known" }),
        detectEvent({ id: "d-unknown", verdict: "unknown" }),
        detectEvent({ id: "d-spike", verdict: "spike" }),
      ],
      [
        shadowEvent({ pattern_id: "s-unknown", verdict: "unknown" }),
        shadowEvent({ pattern_id: "s-spike", verdict: "spike" }),
      ],
    );
    expect(rows.map((r) => r.kind === "detect")).toContain(true);
    expect(rows).toHaveLength(2);
    expect(rows.every((r) => r.key.includes("spike"))).toBe(true);
  });

  it("orders newest-first by the row's own timestamp", () => {
    const rows = buildSpikeRows(
      [detectEvent({ id: "d-new", timestamp: "2026-07-04T12:00:00Z" })],
      [shadowEvent({ pattern_id: "s-old", last_seen: "2026-07-04T06:00:00Z" })],
    );
    expect(rows[0].when).toBe("2026-07-04T12:00:00Z");
    expect(rows[1].when).toBe("2026-07-04T06:00:00Z");
  });

  it("uses the AI finding title, then a sample, then the template for the sample text", () => {
    const withFinding = buildSpikeRows(
      [
        detectEvent({
          id: "d1",
          finding: { Title: "Payment API 5xx surge" } as DetectEvent["finding"],
          samples: ["raw line"],
        }),
      ],
      [],
    );
    expect(withFinding[0].sample).toBe("Payment API 5xx surge");

    const withSample = buildSpikeRows(
      [detectEvent({ id: "d2", finding: null, samples: ["raw sample line"] })],
      [],
    );
    expect(withSample[0].sample).toBe("raw sample line");
  });

  it("treats undefined queries (still loading) as empty", () => {
    expect(buildSpikeRows(undefined, undefined)).toEqual([]);
  });
});
