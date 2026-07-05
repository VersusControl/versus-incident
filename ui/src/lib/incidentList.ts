// incidentList.ts — pure filtering for the Incidents page, kept free of React /
// fetch so the status + text filters (applied BEFORE client-side pagination)
// are unit-testable in the node vitest environment (the lib/*.ts + lib/*.test.ts
// pattern). The page slices the filtered list through usePagination at 100/page.

import type { IncidentSummary, OriginCounts } from "./api";

export type IncidentStatusFilter = "all" | "open" | "acked" | "resolved";

export const INCIDENT_STATUS_VALUES: IncidentStatusFilter[] = [
  "open",
  "acked",
  "resolved",
  "all",
];

// Origin tabs separate the AI-detected feed from the inbound-alert
// firehose. AI-detected is the high-signal DEFAULT so a flood of webhook
// incidents can never bury it; webhook is one click away.
export const INCIDENT_ORIGIN_VALUES = ["ai_detect", "webhook"] as const;
export type IncidentOrigin = (typeof INCIDENT_ORIGIN_VALUES)[number];

// normalizeOrigin coerces the URL param to a known tab, defaulting to
// ai_detect for a missing or unexpected value.
export function normalizeOrigin(
  raw: string | null | undefined,
): IncidentOrigin {
  return raw === "webhook" ? "webhook" : "ai_detect";
}

// originLabel is the human tab label for an origin.
export function originLabel(o: IncidentOrigin): string {
  return o === "webhook" ? "Webhook / Alerts" : "AI Detected";
}

// formatOriginCounts renders the separated top-bar summary so the two
// feeds are never lumped into one total. Returns undefined while the
// counts are still loading so the subtitle stays blank rather than
// flashing "AI: 0 · Webhook: 0".
export function formatOriginCounts(
  counts: OriginCounts | undefined,
): string | undefined {
  if (!counts) return undefined;
  return `AI: ${counts.ai_detect} · Webhook: ${counts.webhook}`;
}

// incidentOrigin classifies a loaded incident into one of the two feeds.
// The backend stamps every fresh row with an explicit origin and
// classifies legacy rows server-side, so the field is normally present;
// client-side we mirror normalizeOrigin's rule — anything that isn't an
// explicit "webhook" is treated as the high-signal ai_detect feed so a
// missing origin can never hide an incident from the default view.
export function incidentOrigin(i: IncidentSummary): IncidentOrigin {
  return i.origin === "webhook" ? "webhook" : "ai_detect";
}

// matchesOrigin filters a loaded incident against the active origin tab.
// Surfaces that fetch the whole set once and split it client-side (e.g.
// the Now feed, which shares its cache with the TopBar/Sidebar badges and
// so cannot refetch per tab) use this instead of the server ?origin=
// param.
export function matchesOrigin(
  i: IncidentSummary,
  origin: IncidentOrigin,
): boolean {
  return incidentOrigin(i) === origin;
}

// countByOrigin tallies a loaded set into the same OriginCounts shape the
// list/search endpoints return, so a client-split surface can feed
// formatOriginCounts and the segmented-control badges without a second
// (server-counted) request. Tolerates a missing list while the query
// loads.
export function countByOrigin(
  list: IncidentSummary[] | undefined,
): OriginCounts {
  let ai_detect = 0;
  let webhook = 0;
  for (const i of list ?? []) {
    if (incidentOrigin(i) === "webhook") webhook++;
    else ai_detect++;
  }
  return { ai_detect, webhook, total: ai_detect + webhook };
}

// incidentResetKey is the usePagination reset signature for the Incidents
// page: changing the origin tab, the status filter, or the search text
// must reset back to page 1. Kept pure so the reset contract is unit
// tested without mounting the page.
export function incidentResetKey(
  origin: string,
  status: string,
  query: string,
): string {
  return `${origin}|${status}|${query}`;
}

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
