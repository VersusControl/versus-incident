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
