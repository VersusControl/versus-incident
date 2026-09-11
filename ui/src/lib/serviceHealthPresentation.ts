import type { ServiceHealthState } from "@/lib/api";

export function healthDataLabel(basis: string): string {
  const labels: Record<string, string> = {
    "Logs + patterns + incidents": "Logs and incident records",
    "Patterns + incidents": "Log patterns and incident records",
    "Logs + incidents": "Logs and incident records",
    "Logs + patterns": "Logs and learned patterns",
    "Logs": "Logs",
    "Patterns": "Learned log patterns",
    "Incidents": "Incident records",
    "Internal data only": "Service records",
    "Incidents only": "Incident records",
  };
  return labels[basis] ?? "Service records";
}

export const SERVICE_HEALTH_STATE_COPY: Record<
  ServiceHealthState,
  { label: string; detail: string }
> = {
  ready: { label: "Reporting", detail: "Current evidence is available." },
  partial: { label: "Partial", detail: "Some eligible evidence is unavailable." },
  not_configured: { label: "Not connected", detail: "No log source is configured." },
  collecting: { label: "Collecting", detail: "Waiting for the first collection to finish." },
  no_data: { label: "No data", detail: "No observations were found in this window." },
  unsupported: { label: "Unsupported", detail: "This measure is not supported by the current source." },
  error: { label: "Source error", detail: "Collection failed and no current evidence is available." },
  stale: { label: "Stale", detail: "Showing the most recent successful evidence." },
  restricted: { label: "Restricted", detail: "This evidence is unavailable with current access." },
};