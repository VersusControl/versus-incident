// reportAnalytics — pure, DOM-free decision logic behind the incidents
// analytics "Reports" action. The UI has no jsdom/testing-library, so (per the
// repo pattern) the behaviours that matter — is the action shown, which window
// and channel are selected by default, whether the picker can send, and how a
// per-channel outcome reads back to the operator — live here as side-effect-
// free helpers and are unit-tested directly. The dialog + settings components
// are thin shells over these.

import type { Capabilities, ReportSendResult } from "@/lib/api";

// ReportWindow is the closed set of windows the report supports.
export type ReportWindow = "today" | "24h" | "7d";

// REPORT_WINDOWS is the ordered picker options with human labels.
export const REPORT_WINDOWS: { value: ReportWindow; label: string }[] = [
  { value: "today", label: "Today" },
  { value: "24h", label: "Last 24h" },
  { value: "7d", label: "Last 7 days" },
];

// isReportWindow narrows an arbitrary string to a valid window.
export function isReportWindow(w: string | undefined): w is ReportWindow {
  return w === "today" || w === "24h" || w === "7d";
}

// canReport gates the whole action: only shown when the server reports the
// feature enabled. A missing report block (older server) hides it.
export function canReport(cap: Capabilities | undefined): boolean {
  return !!cap?.report?.enable;
}

// reportChannels returns the enabled channels to offer in the picker.
export function reportChannels(cap: Capabilities | undefined): string[] {
  return cap?.report?.channels ?? [];
}

// hasReportChannel is the "degrade cleanly" gate: with no enabled channel the
// dialog disables Send and shows guidance instead of a broken picker.
export function hasReportChannel(cap: Capabilities | undefined): boolean {
  return reportChannels(cap).length > 0;
}

// defaultReportChannel picks the initial picker selection: the configured
// default_channel when it is actually enabled, else the first enabled channel,
// else "".
export function defaultReportChannel(cap: Capabilities | undefined): string {
  const channels = reportChannels(cap);
  const preferred = cap?.report?.default_channel ?? "";
  if (preferred && channels.includes(preferred)) return preferred;
  return channels[0] ?? "";
}

// defaultReportWindow picks the initial window: the runtime default_window
// when valid, else today.
export function defaultReportWindow(
  cap: Capabilities | undefined,
): ReportWindow {
  const w = cap?.report?.default_window;
  return isReportWindow(w) ? w : "today";
}

// ReportOutcomeSummary is the operator-facing reading of a send result: a
// toast tone (matching the toast tone union) and a one-line human summary of
// what reached where.
export interface ReportOutcomeSummary {
  tone: "ok" | "info" | "error";
  title: string;
  description: string;
}

// summarizeReportOutcome turns the raw per-channel result into a toast: all
// good → ok; any failure → error (but note what still went through); only
// fallbacks → info (image not supported, summary delivered).
export function summarizeReportOutcome(
  r: ReportSendResult,
): ReportOutcomeSummary {
  const sent = r.sent ?? [];
  const fallback = r.fallback ?? [];
  const failed = Object.keys(r.failed ?? {});

  const parts: string[] = [];
  if (sent.length) parts.push(`image sent to ${sent.join(", ")}`);
  if (fallback.length) parts.push(`text summary to ${fallback.join(", ")}`);
  if (failed.length) parts.push(`failed: ${failed.join(", ")}`);
  const description = parts.join("; ") || "nothing to deliver";

  if (failed.length) {
    return { tone: "error", title: "Report partially delivered", description };
  }
  if (fallback.length && !sent.length) {
    return {
      tone: "info",
      title: "Report shared as text",
      description:
        description +
        " (this channel can't display an image — open Reports in Versus for the dashboard)",
    };
  }
  return { tone: "ok", title: "Report shared", description };
}
