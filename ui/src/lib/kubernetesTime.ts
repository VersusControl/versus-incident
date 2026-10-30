/** Center old evidence normally; shift valid recent history to now without shortening its span. */
export function kubernetesTimelineWindow(end: number, minutes: number, centered: boolean, historyCeiling?: number) {
  const span = minutes * 60_000;
  let since = minutes === 0 ? 0 : end - span * (centered ? 0.5 : 1);
  let until = end + (centered ? span * 0.5 : 0);
  // The ceiling corrects UI-generated padding, not an explicitly invalid future
  // center. Preserve the server's one-minute clock-skew/future-input boundary.
  if (centered && historyCeiling !== undefined && end <= historyCeiling + 60_000 && until > historyCeiling) {
    until = historyCeiling;
    since = minutes === 0 ? 0 : until - span;
  }
  return { since: new Date(since).toISOString(), until: new Date(until).toISOString() };
}

// Go's zero time is parseable by JavaScript, but does not represent captured evidence.
export function isKnownKubernetesTime(value: unknown): value is string {
  if (typeof value !== "string" || !value.trim()) return false;
  const time = Date.parse(value);
  return Number.isFinite(time) && time !== -62135596800000;
}
