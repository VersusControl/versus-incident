import { describe, expect, it } from "vitest";
import type { AwsGap } from "@/lib/api";
import { gapWords, syncProgress } from "@/lib/awsSettings";

// Counts the server may send for a capped read, including the missing,
// zero and malformed ones a stored unit from an older build can carry.
const CAPPED: AwsGap[] = [
  { reason: "capped" },
  { reason: "capped", seen: 0 },
  { reason: "capped", seen: 0, total: 0 },
  { reason: "capped", seen: 0, total: 140 },
  { reason: "capped", seen: -3 },
  { reason: "capped", seen: Number.NaN },
  { reason: "capped", seen: null as unknown as number },
  { reason: "capped", total: 140 },
];

describe("capped read wording", () => {
  it("never prints a zero, negative or missing count", () => {
    for (const gap of [...CAPPED, "capped"] as Array<string | AwsGap>) {
      const words = gapWords(gap);
      expect(words, JSON.stringify(gap)).toBe("Not all read");
      expect(words).not.toMatch(/more than\s*(0|-|NaN|null|undefined)/);
    }
  });

  it("prints the count read when there is one", () => {
    expect(gapWords({ reason: "capped", seen: 1 })).toBe("more than 1, not all read");
    expect(gapWords({ reason: "capped", seen: 100, total: 0 })).toBe("more than 100, not all read");
    expect(gapWords({ reason: "capped", seen: 100 })).toBe("more than 100, not all read");
    expect(gapWords({ reason: "capped", seen: 100, total: Number.NaN })).toBe("more than 100, not all read");
  });

  it("prints the count read out of the total when both are known", () => {
    expect(gapWords({ reason: "capped", seen: 28, total: 35 })).toBe("28 of 35 read");
    expect(gapWords({ reason: "capped", seen: 1, total: 1 })).toBe("1 of 1 read");
  });

  it("keeps a capped service with no count a neutral Read row, never Ready", () => {
    const progress = syncProgress({
      state: "ok",
      synced_at: null,
      first_sync: true,
      units_done: CAPPED.length,
      units_total: CAPPED.length,
      services: CAPPED.map((gap, index) => ({
        service: `svc${index}`,
        state: "ok" as const,
        units_done: 1,
        units_total: 1,
        last_good_at: null,
        gaps: [gap],
      })),
    });
    for (const row of progress.rows) {
      expect(row).toMatchObject({ kind: "read", word: "Read", gaps: ["Not all read"] });
    }
    expect(progress.label).toBe(`${CAPPED.length} of ${CAPPED.length} ready`);
  });
});
