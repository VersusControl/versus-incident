import { format, formatDistanceToNowStrict, parseISO } from "date-fns";

export function fmtAbs(ts?: string) {
  if (!ts) return "—";
  try {
    return format(parseISO(ts), "yyyy-MM-dd HH:mm:ss");
  } catch {
    return ts;
  }
}

export function fmtRel(ts?: string) {
  if (!ts) return "—";
  try {
    return `${formatDistanceToNowStrict(parseISO(ts))} ago`;
  } catch {
    return ts;
  }
}

export function truncate(s: string, n = 80) {
  if (!s) return "";
  return s.length <= n ? s : s.slice(0, n - 1) + "…";
}

// Untitled incidents show their short id ("#f9b0dadc") instead of a
// literal "(untitled)" — operators triaging several unnamed incidents
// need SOMETHING that tells them apart.
export function incidentTitle(i: { title?: string; id: string }) {
  const t = i.title?.trim();
  return t ? t : `#${i.id.slice(0, 8)}`;
}

// "_unknown" is the agent's internal sentinel for unattributed signals
// (pkg/agent service matcher) — never a user-facing service name.
export function displayService(s?: string | null) {
  return s && s !== "_unknown" ? s : "—";
}

// hourlyBuckets — count timestamps into `hours` hourly buckets ending at
// `now`, oldest→newest. Sparkline source: real timestamps only; invalid
// or out-of-window stamps are dropped, never interpolated. Callers inside
// React must pass a ticking `now` (useNowTick) — capturing Date.now()
// inside a data-keyed memo freezes the window when data stops changing.
export function hourlyBuckets(
  timestamps: Array<string | null | undefined>,
  hours = 24,
  now = Date.now(),
): number[] {
  const hour = 3_600_000;
  const buckets = new Array<number>(hours).fill(0);
  for (const ts of timestamps) {
    if (!ts) continue;
    const t = new Date(ts).getTime();
    if (Number.isNaN(t)) continue;
    const age = now - t;
    if (age < 0 || age >= hours * hour) continue;
    buckets[hours - 1 - Math.floor(age / hour)]++;
  }
  return buckets;
}
