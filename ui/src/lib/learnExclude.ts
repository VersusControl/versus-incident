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
// endpoint — one org's Disable-Learn lists. The three halves are independent:
// `services` are fully-excluded service names, `metrics` are signal entries
// (exact OR glob/prefix), and `patterns` are excluded LOG PATTERN keys/ids (the
// stable miner id shown in the patterns list — the per-log-pattern grain, E1).
// All three are always present (possibly empty).
export interface LearnExclusions {
  services: string[];
  metrics: string[];
  patterns: string[];
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

// serviceExcluded reports whether a whole service is held out of learning — an
// exact-name membership test against the policy's `services` list (the server
// matches services by exact name, never by glob). It is the checked-state
// source for the LOGS-page per-row "Ignore service" control and mirrors the
// ServiceDetailPage overview toggle: an excluded service is fully ignored
// across training, shadow and detect in EVERY telemetry type (logs, metrics,
// traces). A blank name, or an absent list, is never excluded.
export function serviceExcluded(
  service: string,
  services: string[] | undefined,
): boolean {
  if (!service || !services) return false;
  return services.includes(service);
}

// patternExcluded reports whether one LOG PATTERN is held out of learning — an
// exact-name membership test against the policy's `patterns` list, keyed on the
// pattern's stable Key/id (the miner cluster id shown in the patterns list and
// used by relabel/reassign). It mirrors the Go seam's ExcludeLogPattern
// (matched by exact key, never by glob) and is the checked-state source for the
// LOGS-page per-row "Ignore this pattern" action — the log analogue of
// metricExcluded for the per-signal metric/trace grain. A blank key, or an
// absent list, is never excluded.
export function patternExcluded(
  patternKey: string,
  patterns: string[] | undefined,
): boolean {
  if (!patternKey || !patterns) return false;
  return patterns.includes(patternKey);
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

// toggleMetricExclusions folds MANY signal toggles into one new metrics list —
// the read-modify-write basis for a BULK Ignore/Resume of metric/trace rows in
// a SINGLE PUT (firing one PUT per signal would race: each reads the same stale
// list and the last write wins, dropping the rest). It applies each signal in
// turn through toggleMetricExclusion, so the exact-add / match-drop semantics
// are identical to the single-row path. The input list is never mutated.
export function toggleMetricExclusions(
  metrics: string[],
  signals: string[],
  exclude: boolean,
): string[] {
  return signals.reduce(
    (acc, signal) => toggleMetricExclusion(acc, signal, exclude),
    metrics.slice(),
  );
}

// toggleLogPatternExclusion computes the new patterns list for a
// read-modify-write PUT when one LOG PATTERN is toggled. Log patterns match by
// EXACT Key/id (never glob — mirrors the Go seam's ExcludeLogPattern), so the
// toggle is a plain add (idempotent) / remove:
//   • exclude=true  → add the pattern key (no-op if already present),
//   • exclude=false → drop the pattern key.
// The returned list is a new array; the input is never mutated. This is the
// log analogue of toggleMetricExclusion — the whole-list PUT is the ONLY write
// path for the log-pattern grain (there is no per-pattern POST/DELETE route),
// so this is how an Ignore/Resume of a log pattern is persisted.
export function toggleLogPatternExclusion(
  patterns: string[],
  patternKey: string,
  exclude: boolean,
): string[] {
  if (exclude) {
    if (patterns.includes(patternKey)) return patterns.slice();
    return [...patterns, patternKey];
  }
  return patterns.filter((p) => p !== patternKey);
}

// toggleLogPatternExclusions folds MANY log-pattern toggles into one new
// patterns list — the read-modify-write basis for a BULK Ignore/Resume of log
// rows in a SINGLE PUT (firing one PUT per pattern would race on the same stale
// list). It applies each key in turn through toggleLogPatternExclusion, so the
// add/remove semantics match the single-row path. The input is never mutated.
export function toggleLogPatternExclusions(
  patterns: string[],
  patternKeys: string[],
  exclude: boolean,
): string[] {
  return patternKeys.reduce(
    (acc, key) => toggleLogPatternExclusion(acc, key, exclude),
    patterns.slice(),
  );
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

// listExcludeControlVisible decides whether the per-row exclude control renders
// on a LIST page (logs / metrics / traces). Unlike the ServiceDetailPage detail
// surface — which shows the state read-only to a viewer — a dense list row
// carries NO control unless the caller can actually act on it: the control is
// shown ONLY for a licensed runtime:manage session ("editable"). It is entirely
// absent on community / OSS ("absent") and for a licensed viewer ("readonly"),
// so the feature never leaks a header or an inert widget to a non-admin. This
// is the boolean the list pages gate both the column header AND the cell on.
export function listExcludeControlVisible(gate: LearnExcludeGate): boolean {
  return gate === "editable";
}
