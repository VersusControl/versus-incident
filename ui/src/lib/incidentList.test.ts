import { describe, expect, it } from "vitest";

import type { IncidentSummary } from "./api";
import {
  INCIDENT_STATUS_VALUES,
  filterIncidentsByText,
  matchesStatus,
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
