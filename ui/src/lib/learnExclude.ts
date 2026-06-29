// learnExclude.ts — pure logic for the Disable-Learn UI (X30-T8), kept free of
// React / fetch so the client-side metric matcher and the render gate are
// unit-testable in isolation (the project's lib/*.ts + lib/*.test.ts pattern).
//
// The enterprise policy (/enterprise/api/agent/learn-exclusions) is two lists:
// `services` are exact service names, `metrics` are exact signal names AND
// glob/prefix patterns (e.g. "up", "go_*", "prometheus_*"). The control surface
// mirrors the server's match semantics client-side so a checkbox reflects the
// exact same exclusion decision the agent makes on its next tick.

// LearnExclusions is the GET view AND the PUT input shape of the policy
// endpoint — one org's Disable-Learn lists. The two halves are independent:
// `services` are fully-excluded service names, `metrics` are signal entries
// (exact OR glob/prefix). Both are always present (possibly empty).
export interface LearnExclusions {
  services: string[];
  metrics: string[];
}

// escapeRegExp escapes every regex metacharacter in a literal run so a glob
// entry's non-`*`/`?` characters match literally — the JS mirror of Go's
// regexp.QuoteMeta.
function escapeRegExp(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

// compileGlob compiles a metric entry that contains a `*` or `?` metacharacter
// into an anchored RegExp, mirroring the server's compileMetricPattern exactly:
// `*` → any run, `?` → one character, every other character literal, anchored
// `^…$`, case-sensitive. An entry with no metacharacter is an exact name and
// returns null (the caller compares it literally). A malformed pattern returns
// null too, so it degrades to an exact compare rather than throwing.
function compileGlob(entry: string): RegExp | null {
  if (!/[*?]/.test(entry)) return null;
  let out = "^";
  for (const ch of entry) {
    if (ch === "*") out += ".*";
    else if (ch === "?") out += ".";
    else out += escapeRegExp(ch);
  }
  out += "$";
  try {
    return new RegExp(out);
  } catch {
    return null;
  }
}

// matchesMetricPattern reports whether one exclusion entry matches a metric
// signal name. The entry is trimmed (the server trims on write); a blank entry
// or a blank signal never matches. An entry with `*`/`?` is a glob/prefix
// pattern; otherwise it is an exact name.
export function matchesMetricPattern(signal: string, entry: string): boolean {
  const p = entry.trim();
  if (p === "" || signal === "") return false;
  const re = compileGlob(p);
  return re ? re.test(signal) : signal === p;
}

// metricExcluded reports whether a metric signal is excluded from learning by
// ANY entry in the metrics list — an exact name OR a glob/prefix pattern. It is
// the checkbox's checked-state source.
export function metricExcluded(
  signal: string,
  metrics: string[] | undefined,
): boolean {
  if (!signal || !metrics) return false;
  return metrics.some((m) => matchesMetricPattern(signal, m));
}

// toggleMetricExclusion computes the new metrics list for a read-modify-write
// PUT when one metric row's checkbox is toggled:
//   • exclude=true  → add the exact signal name (no-op if already matched, so a
//                     glob like "go_*" is left intact when it already covers it).
//   • exclude=false → drop EVERY entry (exact or glob/prefix) that matches the
//                     signal, so the signal is no longer excluded by anything.
// The returned list is a new array; the input is never mutated.
export function toggleMetricExclusion(
  metrics: string[],
  signal: string,
  exclude: boolean,
): string[] {
  if (exclude) {
    if (metricExcluded(signal, metrics)) return metrics.slice();
    return [...metrics, signal];
  }
  return metrics.filter((m) => !matchesMetricPattern(signal, m));
}

// LearnExcludeGate is how the Disable-Learn controls route themselves:
//   "absent"   — the binary is not licensed for this surface (the /intel probe
//                returned 403/404): render NO controls at all, exactly like the
//                Metrics card's locked degrade.
//   "readonly" — licensed but the session lacks RBAC runtime:manage: show the
//                exclusion state with the toggle/checkbox DISABLED, never hidden.
//   "editable" — licensed AND runtime:manage: the live, interactive controls.
export type LearnExcludeGate = "absent" | "readonly" | "editable";

// learnExcludeGate is the pure decision the controls share. It fails closed:
// without a licensed surface there are no controls, and without runtime:manage
// the controls are read-only — a non-licensed / non-admin caller can never
// reach "editable".
export function learnExcludeGate(input: {
  // licensed is true only when the enterprise /intel endpoint returned 200 (the
  // same HTTP-status signal the Metrics & Traces card degrades on); 403/404 is
  // false.
  licensed: boolean;
  // canManage is true when the SSO session holds RBAC runtime:manage
  // (admin/owner) — the permission the policy write endpoints require.
  canManage: boolean;
}): LearnExcludeGate {
  if (!input.licensed) return "absent";
  if (!input.canManage) return "readonly";
  return "editable";
}
