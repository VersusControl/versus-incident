// Single home for severity parsing + presentation. Episode-backed incidents
// expose typed cumulative severity; legacy and webhook details fall back to
// the content blob.

export type Severity = "critical" | "high" | "warn" | "info" | "ok";

const ORDER: Record<Severity, number> = {
  critical: 0,
  high: 1,
  warn: 2,
  info: 3,
  ok: 4,
};

export function normalizeSeverity(raw?: string | null): Severity | null {
  const s = (raw ?? "").trim().toLowerCase();
  if (!s) return null;
  if (["critical", "crit", "fatal", "p1", "sev1"].includes(s)) return "critical";
  if (["high", "error", "err", "major", "p2", "sev2"].includes(s)) return "high";
  if (["medium", "med", "warn", "warning", "p3", "sev3"].includes(s)) return "warn";
  if (["low", "info", "informational", "minor", "p4", "p5"].includes(s)) return "info";
  if (["ok", "good", "resolved", "none", "clear"].includes(s)) return "ok";
  return null;
}

const SEVERITY_KEYS = ["Severity", "severity", "level", "priority"];
const LABEL_PARENTS = ["labels", "commonLabels"];

type Accept = (v: unknown) => boolean;

function isObject(v: unknown): v is Record<string, unknown> {
  return !!v && typeof v === "object" && !Array.isArray(v);
}

const anyValue: Accept = () => true;

// pickKey resolves name against the object's real spellings the way the server
// does: the exact spelling wins, else the lexicographically smallest
// case-insensitive match whose value accept() approves.
function pickKey(obj: Record<string, unknown>, name: string, accept: Accept): string | null {
  if (Object.prototype.hasOwnProperty.call(obj, name) && accept(obj[name])) return name;
  const lowered = name.toLowerCase();
  let best: string | null = null;
  for (const k of Object.keys(obj)) {
    if (k.toLowerCase() !== lowered || !accept(obj[k])) continue;
    if (best === null || k < best) best = k;
  }
  return best;
}

function pick(obj: Record<string, unknown>, name: string, accept: Accept = anyValue): unknown {
  const k = pickKey(obj, name, accept);
  return k === null ? undefined : obj[k];
}

const isKnownSeverity: Accept = (v) => typeof v === "string" && normalizeSeverity(v) !== null;

// keySeverity reads the first recognised severity at any of keys: exact
// spellings across all keys first, then case-insensitive matches, matching the
// server's lookup order.
function keySeverity(obj: unknown, keys: string[]): Severity | null {
  if (!isObject(obj)) return null;
  for (const k of keys) {
    const v = obj[k];
    if (isKnownSeverity(v)) return normalizeSeverity(v as string);
  }
  for (const k of keys) {
    const v = pick(obj, k, isKnownSeverity);
    if (v !== undefined) return normalizeSeverity(v as string);
  }
  return null;
}

// severityFromContent digs through the free-form alert payload in the same
// order the server resolves an incident's severity: top-level keys, then the
// nested label maps, then the first alert's labels, then CloudWatch alarm
// trigger dimensions. Key matching is case-insensitive.
export function severityFromContent(
  content?: Record<string, unknown> | null,
): Severity | null {
  if (!content) return null;
  const top = keySeverity(content, SEVERITY_KEYS);
  if (top) return top;
  for (const parent of LABEL_PARENTS) {
    const fromLabels = keySeverity(pick(content, parent, isObject), SEVERITY_KEYS);
    if (fromLabels) return fromLabels;
  }
  // A minimal Alertmanager body may carry its labels only on each alert. A
  // grouped body (commonLabels with several alerts) is not read from its first
  // alert: the alerts disagree on any label missing from commonLabels.
  const alerts = pick(content, "alerts");
  const grouped = isObject(pick(content, "commonLabels", isObject));
  if (Array.isArray(alerts) && isObject(alerts[0]) && (!grouped || alerts.length === 1)) {
    const fromAlert = keySeverity(pick(alerts[0], "labels", isObject), SEVERITY_KEYS);
    if (fromAlert) return fromAlert;
  }
  return dimensionSeverity(content);
}

// dimensionSeverity reads a severity from a CloudWatch alarm's
// Trigger.Dimensions list ({name, value} entries, any key casing). The first
// entry for a dimension name wins; names are tried in key priority order.
function dimensionSeverity(content: Record<string, unknown>): Severity | null {
  const trigger = pick(content, "Trigger", isObject);
  if (!isObject(trigger)) return null;
  const dims = pick(trigger, "Dimensions");
  if (!Array.isArray(dims)) return null;
  const byName = new Map<string, string>();
  const isNonEmpty: Accept = (v) => typeof v === "string" && v.trim() !== "";
  for (const entry of dims) {
    if (!isObject(entry)) continue;
    const name = pick(entry, "name", isNonEmpty);
    const value = pick(entry, "value", isNonEmpty);
    if (typeof name !== "string" || typeof value !== "string") continue;
    const key = name.trim().toLowerCase();
    if (!byName.has(key)) byName.set(key, value.trim());
  }
  for (const k of SEVERITY_KEYS) {
    const n = normalizeSeverity(byName.get(k.toLowerCase()));
    if (n) return n;
  }
  return null;
}

// severityRank sorts critical-first; unknown severities sink to the bottom.
export function severityRank(sev: Severity | null): number {
  return sev === null ? 99 : ORDER[sev];
}

// incidentSeverity picks the highest of the typed episode severities, the
// content blob (detail pages) and the server-reported raw severity (list rows,
// which carry no content).
export function incidentSeverity(
  content?: Record<string, unknown> | null,
  highestObserved?: string | null,
  highestNotified?: string | null,
  reported?: string | null,
): Severity | null {
  return [
    normalizeSeverity(highestObserved),
    normalizeSeverity(highestNotified),
    severityFromContent(content),
    normalizeSeverity(reported),
  ].reduce<Severity | null>((highest, candidate) =>
    severityRank(candidate) < severityRank(highest) ? candidate : highest,
    null);
}

export const severityLabel: Record<Severity, string> = {
  critical: "CRITICAL",
  high: "HIGH",
  warn: "WARN",
  info: "INFO",
  ok: "OK",
};

// Tailwind class fragments per severity (text / tinted chip / row rail).
export const severityText: Record<Severity, string> = {
  critical: "text-sev-critical",
  high: "text-sev-high",
  warn: "text-sev-warn",
  info: "text-sev-info",
  ok: "text-sev-ok",
};

export const severityChip: Record<Severity, string> = {
  critical: "border-sev-critical/30 bg-sev-critical/15 text-sev-critical",
  high: "border-sev-high/30 bg-sev-high/15 text-sev-high",
  warn: "border-sev-warn/30 bg-sev-warn/15 text-sev-warn",
  info: "border-sev-info/30 bg-sev-info/15 text-sev-info",
  ok: "border-sev-ok/30 bg-sev-ok/15 text-sev-ok",
};

export const severityRail: Record<Severity, string> = {
  critical: "sev-rail-critical",
  high: "sev-rail-high",
  warn: "sev-rail-warn",
  info: "sev-rail-info",
  ok: "sev-rail-ok",
};
