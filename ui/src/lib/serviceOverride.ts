// serviceOverride.ts — pure logic for the manual-attribution override UI, kept
// free of React / fetch so the client-side matcher and the render gate are
// unit-testable in isolation (the project's lib/*.ts + lib/*.test.ts pattern).
//
// A manual override re-labels a mis-attributed signal's service. The match key
// is source-appropriate and mirrors the Go pkg/agent overrideMatches /
// matchSignalGlob EXACTLY so a client control reflects the same decision the
// agent makes on its next tick: logs match on the mined pattern id (exact) OR a
// message substring; metrics/traces match on the signal name (exact or `*`/`?`
// glob).

import type { ServiceOverride, ServiceOverrideSource } from "./api";

// escapeRegExp escapes every regex metacharacter in a literal run — the JS
// mirror of Go's regexp.QuoteMeta.
function escapeRegExp(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

// matchSignalGlob reports whether a metric/trace signal name matches a rule
// entry: an exact name, or a `*`/`?` glob anchored at both ends, case-sensitive.
// It mirrors the Go matchSignalGlob so a client badge reflects the exact server
// decision. A blank entry or blank signal never matches; a malformed pattern
// degrades to an exact compare rather than throwing.
export function matchSignalGlob(signal: string, entry: string): boolean {
  const p = entry.trim();
  if (p === "" || signal === "") return false;
  if (!/[*?]/.test(p)) return signal === p;
  let out = "^";
  for (const ch of p) {
    if (ch === "*") out += ".*";
    else if (ch === "?") out += ".";
    else out += escapeRegExp(ch);
  }
  out += "$";
  try {
    return new RegExp(out).test(signal);
  } catch {
    return signal === p;
  }
}

// OverrideInput is the source-appropriate match context for one signal.
export interface OverrideInput {
  sourceType: ServiceOverrideSource;
  // signal is the metric/trace signal (series) name. Empty for logs.
  signal?: string;
  // pattern is the mined log pattern id. Empty for metrics/traces.
  pattern?: string;
  // message is the raw log message, for substring matching. Empty otherwise.
  message?: string;
}

// overrideMatches reports whether a rule applies to an input — the JS mirror of
// Go's overrideMatches. A rule only ever matches an input of the SAME source
// type.
export function overrideMatches(
  rule: ServiceOverride,
  input: OverrideInput,
): boolean {
  if (
    rule.source_type !== input.sourceType ||
    !rule.match ||
    !rule.service
  ) {
    return false;
  }
  if (input.sourceType === "log") {
    if (input.pattern && rule.match === input.pattern) return true;
    return !!input.message && input.message.includes(rule.match);
  }
  return matchSignalGlob(input.signal ?? "", rule.match);
}

// resolveOverrideService returns the service the FIRST matching override rule
// forces for an input, or undefined when none applies (keep auto-detection). It
// is the source of a "reassigned to X" badge on a signal row.
export function resolveOverrideService(
  rules: ServiceOverride[] | undefined,
  input: OverrideInput,
): string | undefined {
  if (!rules) return undefined;
  const hit = rules.find((r) => overrideMatches(r, input));
  return hit ? hit.service : undefined;
}

// ServiceOverrideGate is how the reassign controls route themselves, matching
// the learnExcludeGate tri-state:
//   "absent"   — the surface is not available (e.g. metric/trace reassign on an
//                unlicensed binary): render NO controls.
//   "readonly" — available but the session lacks RBAC runtime:manage: show the
//                current override, disable the control.
//   "editable" — available AND runtime:manage: the live, interactive control.
export type ServiceOverrideGate = "absent" | "readonly" | "editable";

// logOverrideGate gates the LOGS reassign control. Logs override is an OSS
// capability — it needs only RBAC runtime:manage, never a license.
export function logOverrideGate(canManage: boolean): ServiceOverrideGate {
  return canManage ? "editable" : "readonly";
}

// signalOverrideGate gates the METRIC/TRACE reassign control. Detection for
// those types is enterprise-only, so the control is "absent" without a license,
// "readonly" without runtime:manage, and "editable" only with both. It fails
// closed exactly like learnExcludeGate.
export function signalOverrideGate(input: {
  licensed: boolean;
  canManage: boolean;
}): ServiceOverrideGate {
  if (!input.licensed) return "absent";
  if (!input.canManage) return "readonly";
  return "editable";
}
