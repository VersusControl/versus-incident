import { describe, expect, it } from "vitest";
import { retainKubernetesHistoryCounts } from "./api";

describe("counts carried onto later history pages", () => {
  const later = { items: [{ id: "c" }], count_unknown: true, next: "page-3" };
  it("carries a counted first page's count and clears the later page's unknown flag", () => {
    const carried = retainKubernetesHistoryCounts({ items: [], total: 18, crash_count: 3, counts: { change: 15, crash: 3, break: 0 } }, later) as Record<string, unknown>;
    expect(carried.total).toBe(18);
    expect(carried.crash_count).toBe(3);
    expect("count_unknown" in carried).toBe(false);
  });

  it("keeps an uncounted first page's window unknown", () => {
    const carried = retainKubernetesHistoryCounts({ items: [], count_unknown: true }, later) as Record<string, unknown>;
    expect(carried.count_unknown).toBe(true);
    expect(carried.total).toBeUndefined();
  });
});
