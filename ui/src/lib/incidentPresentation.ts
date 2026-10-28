import type { IncidentSummary } from "./api";

export function hasDeliveryError(incident: Pick<IncidentSummary, "notify_status" | "notify_error">): boolean {
  return incident.notify_status === "failed" || incident.notify_status === "partial" || !!incident.notify_error;
}

export function deliveryErrorLabel(status?: string): string {
  return status === "partial" ? "Partial delivery" : "Notify failed";
}

export function safeIncidentHref(value: string): string | undefined {
  if (value.startsWith("/") && !value.startsWith("//") && !/[\\\s]/.test(value)) return value;
  try {
    const url = new URL(value);
    if ((url.protocol === "https:" || url.protocol === "http:") && !url.username && !url.password) return url.href;
  } catch {
    // Malformed payload links are displayed as facts, never navigated.
  }
  return undefined;
}
