import type { DetectEvent, ShadowEvent } from "@/lib/api";

// The Decisions "Spike" view merges the two decision logs into ONE lens over
// every spike detection, regardless of which mode produced it:
//
//   * DETECT-mode spikes — the events the agent actually ACTED on (called the
//     AI, emitted an incident). These live in the DETECT log, never the shadow
//     log. In detect mode there are no shadow events at all, so a shadow-only
//     Spike view showed nothing even though the agent fired spike incidents —
//     the bug behind "my spike AI-detect incident isn't on Decisions".
//   * SHADOW-mode spikes — the "would have alerted" surges recorded while the
//     agent runs in shadow mode.
//
// Surfacing both here means a spike detection is findable under "Spike" no
// matter the runtime mode.

export type SpikeKind = "detect" | "shadow";

export interface SpikeRow {
  // Stable React key (unique across both sources).
  key: string;
  // Which decision log the spike came from.
  kind: SpikeKind;
  // Attributed service (may be blank / _unknown → rendered as an empty value).
  service?: string;
  patternId: string;
  source: string;
  // A representative line: the AI finding title / first sample for detect, the
  // coalesced sample message for shadow.
  sample: string;
  // ISO timestamp used for the "When" column and newest-first ordering
  // (detect: decision time; shadow: last seen).
  when: string;
  // Signal count (detect: frequency this tick; shadow: total signals).
  count: number;
  // Detail route for the row.
  href: string;
}

// buildSpikeRows collects every spike-verdict event from the detect + shadow
// logs into a single newest-first list. Non-spike events are dropped. Either
// argument may be undefined (query still loading) and is treated as empty.
export function buildSpikeRows(
  detect: DetectEvent[] | undefined,
  shadow: ShadowEvent[] | undefined,
): SpikeRow[] {
  const rows: SpikeRow[] = [];

  for (const e of detect ?? []) {
    if (e.verdict !== "spike") continue;
    rows.push({
      key: `detect:${e.id}`,
      kind: "detect",
      service: e.service,
      patternId: e.pattern_id,
      source: e.source,
      sample: e.finding?.Title || e.samples?.[0] || e.template || "",
      when: e.timestamp,
      count: e.frequency,
      href: `/agent/decisions/detect/${encodeURIComponent(e.id)}`,
    });
  }

  for (const e of shadow ?? []) {
    if (e.verdict !== "spike") continue;
    rows.push({
      key: `shadow:${e.pattern_id}:${e.first_seen}`,
      kind: "shadow",
      service: e.service,
      patternId: e.pattern_id,
      source: e.source,
      sample: e.sample_message,
      when: e.last_seen,
      count: e.count,
      href: `/agent/decisions/shadow/${encodeURIComponent(e.pattern_id)}`,
    });
  }

  // Newest first by the row's own timestamp (ISO strings sort lexically).
  rows.sort((a, b) => (a.when < b.when ? 1 : a.when > b.when ? -1 : 0));
  return rows;
}
