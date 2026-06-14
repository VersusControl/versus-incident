// Single home for severity parsing + presentation. The incident LIST
// endpoint does not carry severity today (backend ask #1 in
// ui/UX_REDESIGN.md §3.5) — callers must degrade to "—" when normalize
// returns null. The DETAIL page parses it out of the content blob; that
// logic lives here so every surface ranks/labels severity identically.

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

// severityFromContent digs through the free-form alert payload the same way
// the detail page historically did: common keys first, then nested labels.
export function severityFromContent(
  content?: Record<string, unknown> | null,
): Severity | null {
  if (!content) return null;
  const tryKeys = ["Severity", "severity", "level", "priority"];
  for (const k of tryKeys) {
    const v = content[k];
    if (typeof v === "string") {
      const n = normalizeSeverity(v);
      if (n) return n;
    }
  }
  const labels = content["labels"] ?? content["commonLabels"];
  if (labels && typeof labels === "object") {
    const v = (labels as Record<string, unknown>)["severity"];
    if (typeof v === "string") {
      const n = normalizeSeverity(v);
      if (n) return n;
    }
  }
  return null;
}

// severityRank sorts critical-first; unknown severities sink to the bottom.
export function severityRank(sev: Severity | null): number {
  return sev === null ? 99 : ORDER[sev];
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
