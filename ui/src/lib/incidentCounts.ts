import { format } from "date-fns";
import type { IncidentCountBreakdown } from "./api";

type CountMeta = Pick<
  IncidentCountBreakdown,
  "counted_at" | "count_unknown" | "stale"
>;

// countsUnavailable is true when a count surface must show "—" instead of a
// number: the server said it has no count, or the request failed with no
// earlier count to fall back on. A failed refetch keeps the last good count
// (staleCountHint then dates it). A missing count_unknown field (an older
// server) reads as a known count, as before.
export function countsUnavailable(
  data: CountMeta | undefined,
  isError: boolean,
): boolean {
  return data?.count_unknown === true || (isError && !data);
}

// staleCountHint is the tooltip "Counted at HH:MM" (local time) for a count
// that may be out of date: the server marked it stale, or the latest refetch
// failed and the cached count is shown instead (refetchFailed). Undefined for
// a fresh count, an unknown one, or a missing or unreadable counted_at.
export function staleCountHint(
  data: CountMeta | undefined,
  refetchFailed = false,
): string | undefined {
  if (!data || data.count_unknown || !data.counted_at) return undefined;
  if (!data.stale && !refetchFailed) return undefined;
  const at = new Date(data.counted_at);
  if (Number.isNaN(at.getTime())) return undefined;
  return `Counted at ${format(at, "HH:mm")}`;
}

export const COUNT_UNAVAILABLE_LABEL = "Count unavailable";
