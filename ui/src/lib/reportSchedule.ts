// reportSchedule — pure, DOM-free helpers behind the "Scheduled delivery"
// group of the incidents report settings. Following the repo pattern the UI
// keeps its logic in lib/*.ts so it stays unit-testable in the node vitest env;
// ReportSettingsControl is a thin shell over these. The one browser-touching
// helper (detectLocalZone) reads Intl and is guarded, so it is safe to call
// under jsdom or node.

import type { ReportWindow } from "@/lib/reportAnalytics";

// TimezoneKind is the AWS-console-style choice the operator makes: UTC or the
// browser's local IANA zone. The stored `timezone` string is always a concrete
// value — either the literal "UTC" or an IANA name (e.g. "Asia/Ho_Chi_Minh");
// the word "local" is never persisted.
export type TimezoneKind = "utc" | "local";

// detectLocalZone returns the browser's resolved IANA timezone (e.g.
// "Asia/Ho_Chi_Minh"), falling back to "UTC" when the runtime cannot resolve
// one.
export function detectLocalZone(): string {
  try {
    const tz = Intl.DateTimeFormat().resolvedOptions().timeZone;
    return tz || "UTC";
  } catch {
    return "UTC";
  }
}

// timezoneKind maps a STORED `timezone` value back to the radio selection: the
// exact string "UTC" is the UTC option, anything else is treated as a local
// IANA zone.
export function timezoneKind(timezone: string): TimezoneKind {
  return timezone === "UTC" ? "utc" : "local";
}

// resolveTimezone maps a radio selection to the value to STORE: picking UTC
// stores "UTC", picking Local stores the detected IANA zone.
export function resolveTimezone(kind: TimezoneKind, localZone: string): string {
  return kind === "utc" ? "UTC" : localZone;
}

// WINDOW_CLAUSE renders the trailing "sends …" phrase for each window.
const WINDOW_CLAUSE: Record<ReportWindow, string> = {
  today: "sends today's incidents",
  "24h": "sends the last 24h",
  "7d": "sends the last 7 days",
};

// windowClause is the human phrase for the currently selected default window,
// falling back to a generic clause for an unknown value.
export function windowClause(window: string): string {
  return WINDOW_CLAUSE[window as ReportWindow] ?? `sends the ${window} window`;
}

// scheduleSummary builds the live one-line preview, e.g.
// "Daily at 17:00 (Asia/Ho_Chi_Minh) — sends the last 24h".
export function scheduleSummary(
  sendTime: string,
  timezone: string,
  window: string,
): string {
  return `Daily at ${sendTime} (${timezone}) — ${windowClause(window)}`;
}
