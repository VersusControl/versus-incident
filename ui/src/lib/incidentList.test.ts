import { describe, expect, it } from "vitest";

import type { IncidentSummary, OriginCounts } from "./api";
import {
  INCIDENT_ORIGIN_VALUES,
  INCIDENT_STATUS_VALUES,
  countByOrigin,
  filterIncidentsByText,
  formatOriginCounts,
  incidentOrigin,
  incidentResetKey,
  matchesOrigin,
  matchesStatus,
  normalizeOrigin,
  originLabel,
  type IncidentStatusFilter,
} from "./incidentList";
import { pageSlice } from "./pagination";

function inc(partial: Partial<IncidentSummary>): IncidentSummary {
  return {
    id: "inc-1",
    resolved: false,
    created_at: "2026-07-01T00:00:00Z",
    ...partial,
  };
}

describe("INCIDENT_STATUS_VALUES", () => {
  it("carries the four segmented-control filters", () => {
    expect(INCIDENT_STATUS_VALUES).toEqual(["open", "acked", "resolved", "all"]);
  });
});

describe("matchesStatus", () => {
  const open = inc({ id: "o", resolved: false, acked_at: null });
  const acked = inc({ id: "a", resolved: false, acked_at: "2026-07-01T01:00:00Z" });
  const resolved = inc({ id: "r", resolved: true, acked_at: "2026-07-01T01:00:00Z" });

  it("buckets open = not resolved and not acked", () => {
    expect(matchesStatus(open, "open")).toBe(true);
    expect(matchesStatus(acked, "open")).toBe(false);
    expect(matchesStatus(resolved, "open")).toBe(false);
  });

  it("buckets acked = acked but not resolved", () => {
    expect(matchesStatus(acked, "acked")).toBe(true);
    expect(matchesStatus(open, "acked")).toBe(false);
    expect(matchesStatus(resolved, "acked")).toBe(false);
  });

  it("buckets resolved and all", () => {
    expect(matchesStatus(resolved, "resolved")).toBe(true);
    expect(matchesStatus(open, "resolved")).toBe(false);
    for (const i of [open, acked, resolved]) {
      expect(matchesStatus(i, "all")).toBe(true);
    }
  });
});

describe("filterIncidentsByText", () => {
  const list = [
    inc({ id: "abc123", title: "Checkout latency", service: "payments" }),
    inc({ id: "def456", title: "Disk full", service: "billing" }),
  ];

  it("returns the whole list for a blank query", () => {
    expect(filterIncidentsByText(list, "", false)).toBe(list);
    expect(filterIncidentsByText(list, "   ", false)).toBe(list);
  });

  it("matches id, title and service case-insensitively", () => {
    expect(filterIncidentsByText(list, "checkout", false)).toHaveLength(1);
    expect(filterIncidentsByText(list, "BILLING", false)).toHaveLength(1);
    expect(filterIncidentsByText(list, "def456", false)).toHaveLength(1);
  });

  it("strips a leading # so a pasted display handle matches the raw id", () => {
    expect(filterIncidentsByText(list, "#abc123", false)[0]?.id).toBe("abc123");
  });

  it("does not re-filter when the server already ran the search", () => {
    // serverSearched === true: the server matched fields the client can't see
    // (e.g. payload body), so the client returns the list untouched.
    expect(filterIncidentsByText(list, "checkout", true)).toBe(list);
  });

  it("tolerates a missing list (query still loading)", () => {
    expect(filterIncidentsByText(undefined, "checkout", false)).toEqual([]);
  });
});

describe("Incidents pagination (filter → 100/page)", () => {
  // The page slices the filtered/sorted list through usePagination AFTER the
  // status + text filters — so 250 open incidents paginate to 3 pages of 100.
  const many: IncidentSummary[] = Array.from({ length: 250 }, (_, n) =>
    inc({ id: `inc-${n}`, resolved: n % 2 === 0 }),
  );

  it("windows an open-status filter into 100-row pages", () => {
    const status: IncidentStatusFilter = "open";
    const filtered = many
      .filter((i) => matchesStatus(i, status))
      .filter(() => true);
    // 125 of the 250 are open (odd n → resolved:false).
    expect(filtered).toHaveLength(125);

    const p1 = pageSlice(filtered.length, 1);
    expect(p1.pageCount).toBe(2);
    expect(filtered.slice(p1.start, p1.end)).toHaveLength(100);
    expect(`${p1.start + 1}–${p1.end} of ${p1.total}`).toBe("1–100 of 125");

    const p2 = pageSlice(filtered.length, 2);
    expect(filtered.slice(p2.start, p2.end)).toHaveLength(25);
    expect(`${p2.start + 1}–${p2.end} of ${p2.total}`).toBe("101–125 of 125");
  });

  it("clamps to the last page when the filter shrinks the list", () => {
    const resolvedOnly = many.filter((i) => matchesStatus(i, "resolved"));
    expect(resolvedOnly).toHaveLength(125);
    // Was on page 2 of the wider list, but resolved-only still has a page 2.
    const s = pageSlice(resolvedOnly.length, 9);
    expect(s.page).toBe(2);
  });
});

describe("origin tabs", () => {
  it("exposes exactly the two feeds, AI-detected first", () => {
    expect(INCIDENT_ORIGIN_VALUES).toEqual(["ai_detect", "webhook"]);
  });

  it("normalizes the URL param, defaulting to the high-signal ai_detect", () => {
    expect(normalizeOrigin("ai_detect")).toBe("ai_detect");
    expect(normalizeOrigin("webhook")).toBe("webhook");
    // Missing or unexpected values fall back to the AI feed so the flood
    // never becomes the default view.
    expect(normalizeOrigin(null)).toBe("ai_detect");
    expect(normalizeOrigin(undefined)).toBe("ai_detect");
    expect(normalizeOrigin("garbage")).toBe("ai_detect");
  });

  it("labels each tab", () => {
    expect(originLabel("ai_detect")).toBe("AI Detected");
    expect(originLabel("webhook")).toBe("Webhook / Alerts");
  });
});

describe("formatOriginCounts", () => {
  it("renders the two feeds separately, never one lumped total", () => {
    const counts: OriginCounts = { ai_detect: 7, webhook: 12043, total: 12050 };
    expect(formatOriginCounts(counts)).toBe("AI: 7 · Webhook: 12043");
  });

  it("stays blank while counts are still loading", () => {
    expect(formatOriginCounts(undefined)).toBeUndefined();
  });
});

describe("incidentOrigin / matchesOrigin (client-side split)", () => {
  it("classifies an explicit webhook row as webhook", () => {
    expect(incidentOrigin(inc({ origin: "webhook" }))).toBe("webhook");
  });

  it("treats ai_detect and anything else as the high-signal ai_detect feed", () => {
    expect(incidentOrigin(inc({ origin: "ai_detect" }))).toBe("ai_detect");
    // Missing or unexpected origin must never hide a row from the default
    // AI view — mirrors normalizeOrigin's fallback.
    expect(incidentOrigin(inc({ origin: undefined }))).toBe("ai_detect");
    expect(incidentOrigin(inc({ origin: "garbage" }))).toBe("ai_detect");
  });

  it("filters a loaded set to the active tab", () => {
    const ai = inc({ id: "a", origin: "ai_detect" });
    const wh = inc({ id: "w", origin: "webhook" });
    expect(matchesOrigin(ai, "ai_detect")).toBe(true);
    expect(matchesOrigin(ai, "webhook")).toBe(false);
    expect(matchesOrigin(wh, "webhook")).toBe(true);
    expect(matchesOrigin(wh, "ai_detect")).toBe(false);
  });
});

describe("countByOrigin (client-split whole-set tally)", () => {
  it("tallies both feeds separately so a webhook flood never lumps into AI", () => {
    const list = [
      inc({ id: "a1", origin: "ai_detect" }),
      inc({ id: "a2", origin: "ai_detect" }),
      ...Array.from({ length: 500 }, (_, n) =>
        inc({ id: `w${n}`, origin: "webhook" }),
      ),
    ];
    expect(countByOrigin(list)).toEqual({
      ai_detect: 2,
      webhook: 500,
      total: 502,
    });
  });

  it("counts a missing origin as ai_detect and tolerates an empty/undefined list", () => {
    expect(countByOrigin([inc({ origin: undefined })])).toEqual({
      ai_detect: 1,
      webhook: 0,
      total: 1,
    });
    expect(countByOrigin(undefined)).toEqual({
      ai_detect: 0,
      webhook: 0,
      total: 0,
    });
  });

  it("feeds formatOriginCounts so the Now top-bar shows both feeds", () => {
    const counts = countByOrigin([
      inc({ origin: "ai_detect" }),
      inc({ origin: "webhook" }),
      inc({ origin: "webhook" }),
    ]);
    expect(formatOriginCounts(counts)).toBe("AI: 1 · Webhook: 2");
  });
});

describe("Now page origin split (default-to-AI, client-side scope)", () => {
  // The Now feed fetches the whole set once (shared with the TopBar/Sidebar
  // badges) and splits it client-side: the active tab scopes the feed while
  // the whole-set counts keep both feeds visible.
  const sorted: IncidentSummary[] = [
    inc({ id: "ai-open", origin: "ai_detect", resolved: false, acked_at: null }),
    inc({ id: "wh-1", origin: "webhook", resolved: false, acked_at: null }),
    inc({ id: "wh-2", origin: "webhook", resolved: false, acked_at: null }),
    inc({ id: "wh-3", origin: "webhook", resolved: true }),
  ];

  it("defaults to the AI-detected tab so the webhook flood never dominates", () => {
    // Missing ?origin= param → ai_detect.
    const origin = normalizeOrigin(null);
    expect(origin).toBe("ai_detect");
    const scoped = sorted.filter((i) => matchesOrigin(i, origin));
    expect(scoped.map((i) => i.id)).toEqual(["ai-open"]);
  });

  it("scopes the feed to the webhook tab when selected", () => {
    const origin = normalizeOrigin("webhook");
    const scoped = sorted.filter((i) => matchesOrigin(i, origin));
    expect(scoped.map((i) => i.id)).toEqual(["wh-1", "wh-2", "wh-3"]);
  });

  it("shows separate per-origin counts regardless of the active tab", () => {
    expect(countByOrigin(sorted)).toEqual({
      ai_detect: 1,
      webhook: 3,
      total: 4,
    });
  });
});

describe("incidentResetKey (pagination reset on tab/filter change)", () => {
  it("changes when the origin tab changes so page resets to 1", () => {
    const ai = incidentResetKey("ai_detect", "open", "");
    const webhook = incidentResetKey("webhook", "open", "");
    expect(ai).not.toBe(webhook);
  });

  it("changes when the status filter or search text changes", () => {
    const base = incidentResetKey("ai_detect", "open", "");
    expect(incidentResetKey("ai_detect", "resolved", "")).not.toBe(base);
    expect(incidentResetKey("ai_detect", "open", "db")).not.toBe(base);
  });

  it("is stable for the same origin/status/query", () => {
    expect(incidentResetKey("webhook", "all", "latency")).toBe(
      incidentResetKey("webhook", "all", "latency"),
    );
  });
});
