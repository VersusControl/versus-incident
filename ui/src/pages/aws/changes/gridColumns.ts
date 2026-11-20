import type { CSSProperties } from "react";
import { bucketIndexFor, type ChangesBucket } from "@/lib/awsChanges";

/**
 * One column's box: a fixed share of the grid with its padding and borders
 * inside it, so a column with a count is exactly as wide as an empty one.
 * Cells, ticks and axis slots all use it, so every row shares one column grid.
 */
export function columnStyle(columns: number, minPx: number): CSSProperties {
  return { flex: `0 0 ${100 / Math.max(1, columns)}%`, minWidth: `${minPx}px`, boxSizing: "border-box" };
}

/**
 * Where a time sits on the column grid, in percent of the grid's width: its
 * column's start plus how far into that column it falls. The overlays use this
 * rather than the window's time span, so a band, an alarm line or a day label
 * lands on the same x as the column that holds that time, even when the first
 * or last bucket is shorter than the others.
 */
export function columnPct(buckets: ChangesBucket[], ends: string[], iso: string): number {
  const n = buckets.length;
  const t = Date.parse(iso);
  if (n === 0 || Number.isNaN(t)) return 0;
  if (t <= Date.parse(buckets[0].start)) return 0;
  if (t >= Date.parse(ends[n - 1])) return 100;
  const i = Math.max(0, bucketIndexFor(buckets, iso));
  const a = Date.parse(buckets[i].start);
  const b = Date.parse(ends[i]);
  const frac = b > a ? Math.min(1, Math.max(0, (t - a) / (b - a))) : 0;
  return ((i + frac) / n) * 100;
}
