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

// cellOverrideInput builds the source-appropriate OverrideInput a service-cell
// control uses to look itself up in the rule set: a log cell keys on the mined
// pattern id, a metric/trace cell on the signal name. It keeps the ServiceCell
// free of the source-type branching so the match logic stays in one place.
export function cellOverrideInput(
  sourceType: ServiceOverrideSource,
  match: string,
): OverrideInput {
  return sourceType === "log"
    ? { sourceType, pattern: match }
    : { sourceType, signal: match };
}

// ResolvedServiceCell is the effective attribution a service cell renders: the
// service to SHOW plus whether that shown service is a still-settling override.
export interface ResolvedServiceCell {
  // service is the service the cell displays. When a matching override exists it
  // is the override TARGET — so a reassignment is reflected INSTANTLY on every
  // surface (logs, metrics, traces alike), independent of whether the backend
  // read model has re-pointed yet. Otherwise it is the signal's own attributed
  // service.
  service: string | null | undefined;
  // pending is true when an override target is shown but the signal's OWN
  // attribution has not caught up to it (current !== target) — the "saved,
  // awaiting the next re-observation" window. It is false when no override
  // applies or the signal has already adopted the target. The rule is identical
  // for logs and metrics/traces, so the "(pending)" qualifier means the same
  // thing on all three pages. Blank/_unknown current counts as "not yet the
  // target", so a reassign away from _unknown reads as pending too.
  pending: boolean;
}

// resolveServiceCell computes the effective attribution a ServiceCell shows for
// one signal, with a single rule shared across logs, metrics, and traces so the
// reassign affordance is consistent on all three surfaces. A manual override
// wins IMMEDIATELY in the UI: the cell displays the override target the instant
// the override exists (driven off the override list the UI already fetches), so
// the reassign gives instant feedback everywhere — closing the gap where the
// logs patterns reader re-points on write (service flips at once) but the
// metrics/traces baseline reader re-points only on re-observation (service lags),
// which used to make the SAME reassign look instant on logs yet stuck "pending"
// on metrics/traces. The "(pending)" qualifier then rides purely on whether the
// signal's own attribution has caught up to the target, so it never reads as
// "the reassign didn't work" on one page and "instant" on another.
export function resolveServiceCell(
  rules: ServiceOverride[] | undefined,
  input: OverrideInput,
  currentService: string | null | undefined,
): ResolvedServiceCell {
  const target = resolveOverrideService(rules, input);
  if (!target) return { service: currentService, pending: false };
  return { service: target, pending: target !== (currentService ?? "") };
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

// assignableServices turns the known-service map (the ["services"] query) into
// the sorted list of valid reassignment targets: every service except the
// _unknown fallback, which is a display sentinel and never a real target. It is
// the shared option source for the in-column reassign picker (ServiceCell), so
// the picker's contents stay unit-testable without React.
export function assignableServices(
  services: Record<string, unknown> | undefined | null,
): string[] {
  if (!services) return [];
  return Object.keys(services)
    .filter((n) => n !== "_unknown")
    .sort((a, b) => a.localeCompare(b));
}
