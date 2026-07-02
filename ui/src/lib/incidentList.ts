// incidentList.ts — pure filtering for the Incidents page, kept free of React /
// fetch so the status + text filters (applied BEFORE client-side pagination)
// are unit-testable in the node vitest environment (the lib/*.ts + lib/*.test.ts
// pattern). The page slices the filtered list through usePagination at 100/page.

import type { IncidentSummary } from "./api";

export type IncidentStatusFilter = "all" | "open" | "acked" | "resolved";

export const INCIDENT_STATUS_VALUES: IncidentStatusFilter[] = [
  "open",
  "acked",
  "resolved",
  "all",
];

// matchesStatus mirrors the page's status buckets: open = not resolved and not
// acked; acked = acked but not resolved; resolved = resolved; all = everything.
export function matchesStatus(
  i: IncidentSummary,
  status: IncidentStatusFilter,
): boolean {
  if (status === "open") return !i.resolved && !i.acked_at;
  if (status === "acked") return !!i.acked_at && !i.resolved;
  if (status === "resolved") return i.resolved;
  return true;
}

// filterIncidentsByText runs the client-side free-text filter over id, title
// and service. A leading "#" (how untitled incidents DISPLAY) is stripped so a
// pasted handle still matches. When the server already ran the search
// (serverSearched), the client does NOT re-filter — the server matched fields
// the client can't see (e.g. payload body) — so the list is returned as-is.
export function filterIncidentsByText(
  list: IncidentSummary[] | undefined,
  query: string,
  serverSearched: boolean,
): IncidentSummary[] {
  if (!list) return [];
  const needle = serverSearched ? "" : query.trim().replace(/^#/, "").toLowerCase();
  if (!needle) return list;
  return list.filter(
    (i) =>
      (i.title ?? "").toLowerCase().includes(needle) ||
      (i.service ?? "").toLowerCase().includes(needle) ||
      i.id.toLowerCase().includes(needle),
  );
}
