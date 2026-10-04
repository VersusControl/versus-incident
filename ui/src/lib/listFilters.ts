// Facet filters for list pages: pure helpers so option counts, matching and
// the URL encoding are unit-testable without React.

export interface FacetDef<T> {
  id: string;
  label: string;
  values: (row: T) => string | readonly string[] | null | undefined;
  format?: (value: string) => string;
}

export interface FacetOption {
  value: string;
  label: string;
  count: number;
}

export type FacetSelection = Readonly<Record<string, readonly string[]>>;

export interface FacetView {
  id: string;
  label: string;
  options: FacetOption[];
  selected: readonly string[];
}

export interface FacetChip {
  facetId: string;
  value: string;
  label: string;
}

const PARAM_PREFIX = "f.";

export function facetParam(id: string): string {
  return `${PARAM_PREFIX}${id}`;
}

function rowValues<T>(facet: FacetDef<T>, row: T): string[] {
  const v = facet.values(row);
  if (v == null) return [];
  return typeof v === "string" ? [v] : Array.from(new Set(v));
}

function formatValue<T>(facet: FacetDef<T>, value: string): string {
  return facet.format ? facet.format(value) : value || "—";
}

// facetOptions lists every value present in rows with how many rows carry it,
// most common first, then alphabetically.
export function facetOptions<T>(
  rows: readonly T[],
  facet: FacetDef<T>,
): FacetOption[] {
  const counts = new Map<string, number>();
  for (const row of rows) {
    for (const v of rowValues(facet, row)) counts.set(v, (counts.get(v) ?? 0) + 1);
  }
  return Array.from(counts, ([value, count]) => ({
    value,
    label: formatValue(facet, value),
    count,
  })).sort((a, b) => b.count - a.count || a.label.localeCompare(b.label));
}

// applyFacets keeps rows matching ANY selected value within a facet and ALL
// facets that have a selection.
export function applyFacets<T>(
  rows: readonly T[],
  facets: readonly FacetDef<T>[],
  selection: FacetSelection,
): T[] {
  const active = facets.filter((f) => (selection[f.id]?.length ?? 0) > 0);
  if (active.length === 0) return [...rows];
  return rows.filter((row) =>
    active.every((f) => {
      const wanted = selection[f.id];
      return rowValues(f, row).some((v) => wanted.includes(v));
    }),
  );
}

export function readFacetSelection(
  params: URLSearchParams,
  facets: readonly { id: string }[],
): FacetSelection {
  const out: Record<string, string[]> = {};
  for (const f of facets) {
    const values = params.getAll(facetParam(f.id));
    if (values.length > 0) out[f.id] = Array.from(new Set(values));
  }
  return out;
}

export function withFacetValues(
  params: URLSearchParams,
  id: string,
  values: readonly string[],
): URLSearchParams {
  const next = new URLSearchParams(params);
  next.delete(facetParam(id));
  for (const v of values) next.append(facetParam(id), v);
  return next;
}

export function withoutFacets(
  params: URLSearchParams,
  facets: readonly { id: string }[],
): URLSearchParams {
  const next = new URLSearchParams(params);
  for (const f of facets) next.delete(facetParam(f.id));
  return next;
}

export function activeFilterCount(selection: FacetSelection): number {
  return Object.values(selection).reduce((n, v) => n + v.length, 0);
}

// selectionSignature is a stable string for pagination / selection reset keys.
export function selectionSignature(selection: FacetSelection): string {
  return Object.keys(selection)
    .sort()
    .map((k) => `${k}=${[...selection[k]].sort().join(",")}`)
    .join("&");
}

export function facetViews<T>(
  rows: readonly T[],
  facets: readonly FacetDef<T>[],
  selection: FacetSelection,
): FacetView[] {
  return facets.map((f) => {
    const selected = selection[f.id] ?? [];
    const options = facetOptions(rows, f);
    // A value deep-linked in the URL stays visible (and removable) even when
    // no loaded row carries it.
    for (const v of selected) {
      if (!options.some((o) => o.value === v)) {
        options.push({ value: v, label: formatValue(f, v), count: 0 });
      }
    }
    return { id: f.id, label: f.label, options, selected };
  });
}

export function facetChips<T>(
  facets: readonly FacetDef<T>[],
  selection: FacetSelection,
): FacetChip[] {
  return facets.flatMap((f) =>
    (selection[f.id] ?? []).map((value) => ({
      facetId: f.id,
      value,
      label: `${f.label}: ${formatValue(f, value)}`,
    })),
  );
}
