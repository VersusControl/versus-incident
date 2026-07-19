// Shared client-side row sorting for the dense agent tables (.ddt). Every table
// that carries a time column wires the SAME helper so a header click sorts by
// the real underlying value (a Date/epoch, never the humanized "31 minutes ago"
// string) and toggles descending ↔ ascending with one active column at a time.
//
// The pure math (tsValue / compareValues / sortRows / nextSort) stays React-free
// so it is unit-testable in the node vitest environment (like pagination.ts);
// the useSortableRows hook wraps it with the active-column state. Sorting is
// applied AFTER search/filter/scope and BEFORE pagination — pass the already
// filtered rows in and slice the returned order through usePagination.

import { useMemo, useRef, useState } from "react";

export type SortDir = "asc" | "desc";

export interface SortState<K extends string> {
  key: K;
  dir: SortDir;
}

// A column accessor returns the comparable value behind a cell — a number for
// timestamps/counts (via tsValue for time columns) or a string for text.
export type SortAccessor<T> = (row: T) => number | string;

// tsValue turns a timestamp (ISO string / epoch-ish) into a number for
// comparison. Missing or unparseable stamps become NaN, which compareValues
// sinks to the bottom of a descending (most-recent-first) sort.
export function tsValue(ts?: string | null): number {
  if (!ts) return NaN;
  const t = Date.parse(ts);
  return Number.isNaN(t) ? NaN : t;
}

// compareValues orders two accessor values ascending: numbers numerically (with
// NaN treated as the smallest so blanks sink), everything else lexically with a
// locale-aware, case-insensitive compare.
export function compareValues(
  a: number | string,
  b: number | string,
): number {
  if (typeof a === "number" && typeof b === "number") {
    const an = Number.isNaN(a) ? -Infinity : a;
    const bn = Number.isNaN(b) ? -Infinity : b;
    return an < bn ? -1 : an > bn ? 1 : 0;
  }
  return String(a).localeCompare(String(b), undefined, {
    numeric: true,
    sensitivity: "base",
  });
}

// sortRows returns a NEW array ordered by the accessor. Array.sort is stable
// (ES2019+), so rows that tie keep their incoming relative order.
export function sortRows<T>(
  rows: readonly T[],
  accessor: SortAccessor<T>,
  dir: SortDir,
): T[] {
  const copy = rows.slice();
  copy.sort((a, b) => {
    const cmp = compareValues(accessor(a), accessor(b));
    return dir === "asc" ? cmp : -cmp;
  });
  return copy;
}

// nextSort computes the state after clicking a header: a fresh column starts
// descending (most-recent-first for time, highest-first for counts); clicking
// the already-active column flips the direction.
export function nextSort<K extends string>(
  current: SortState<K> | null,
  key: K,
): SortState<K> {
  if (current && current.key === key) {
    return { key, dir: current.dir === "asc" ? "desc" : "asc" };
  }
  return { key, dir: "desc" };
}

// sortSignature is a stable string for the active sort — wire it into the
// usePagination / useBulkSelection resetKey so re-sorting lands the operator on
// page 1 with a clean selection, matching the filter/search reset behavior.
export function sortSignature<K extends string>(
  sort: SortState<K> | null,
): string {
  return sort ? `${sort.key}:${sort.dir}` : "none";
}

export interface SortableRows<T, K extends string> {
  // rows in the active sort order (or the incoming order when no sort is set).
  rows: T[];
  sort: SortState<K> | null;
  signature: string;
  // toggle wires a SortHeader's onSort — flip/activate the column.
  toggle: (key: K) => void;
  // headerProps spreads the active/dir/onSort a SortHeader needs.
  headerProps: (key: K) => {
    active: boolean;
    dir: SortDir;
    onSort: () => void;
  };
}

// useSortableRows holds the single active sort column and returns the rows in
// that order. `accessors` maps each sortable column key to the real value
// behind it; `initial` sets the default sort (e.g. the primary time column,
// descending) or null to preserve the incoming order until the operator clicks.
// The column key set K is inferred from the accessors object, so `initial` and
// headerProps are type-checked against the real column names.
export function useSortableRows<
  T,
  A extends Record<string, SortAccessor<T>>,
>(
  rows: readonly T[],
  accessors: A,
  // NoInfer keeps `initial` from being an inference site for A: the column-key
  // set K is inferred ONLY from `accessors`, so a table with >1 sortable column
  // plus a default sort (e.g. last_seen + observations) type-checks correctly
  // instead of pinning K to just the initial key.
  initial: SortState<NoInfer<keyof A & string>> | null = null,
): SortableRows<T, keyof A & string> {
  type K = keyof A & string;
  const [sort, setSort] = useState<SortState<K> | null>(initial);

  // The accessors object is a fresh literal each render but its behavior is
  // stable; keep the latest in a ref so the sort memo depends only on the rows
  // and the active column, never on the object's identity.
  const accessorsRef = useRef(accessors);
  accessorsRef.current = accessors;

  const sorted = useMemo(() => {
    if (!sort) return rows.slice();
    return sortRows(rows, accessorsRef.current[sort.key], sort.dir);
  }, [rows, sort]);

  const toggle = (key: K) => setSort((cur) => nextSort(cur, key));

  return {
    rows: sorted,
    sort,
    signature: sortSignature(sort),
    toggle,
    headerProps: (key) => ({
      active: sort?.key === key,
      dir: sort?.key === key ? sort.dir : "desc",
      onSort: () => toggle(key),
    }),
  };
}
