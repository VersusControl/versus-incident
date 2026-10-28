// Go's zero time is parseable by JavaScript, but does not represent captured evidence.
export function isKnownKubernetesTime(value: unknown): value is string {
  if (typeof value !== "string" || !value.trim()) return false;
  const time = Date.parse(value);
  return Number.isFinite(time) && time !== -62135596800000;
}
