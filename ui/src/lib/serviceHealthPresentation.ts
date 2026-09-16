import type { ServiceHealthState } from "@/lib/api";

export function healthDataLabel(basis: string): string {
  const parts = new Set(basis.split(" + ").map((part) => part.toLowerCase()));
  const known = new Set(["logs", "patterns", "incidents", "incidents only", "internal data only", "metrics", "traces"]);
  if ([...parts].some((part) => !known.has(part))) return "Service records";

  const sources: string[] = [];
  if (parts.has("internal data only")) sources.push("Service records");
  if (parts.has("logs")) sources.push("Logs");
  if (parts.has("patterns") && !parts.has("logs")) sources.push("Log patterns");
  if (parts.has("patterns") && parts.has("logs") && !parts.has("incidents")) sources.push("learned patterns");
  if (parts.has("incidents") || parts.has("incidents only")) sources.push("incident records");
  if (parts.has("metrics")) sources.push("metrics");
  if (parts.has("traces")) sources.push("traces");
  if (sources.length === 0) return "Service records";
  if (sources.length === 1) return sources[0];
  if (sources.length === 2) return `${sources[0]} and ${sources[1]}`;
  return `${sources.slice(0, -1).join(", ")}, and ${sources.at(-1)}`;
}

export const SERVICE_HEALTH_REASON_COPY: Record<string, string> = {
  alert_state_unknown: "Incident state unknown",
  assessment_withheld: "Assessment withheld",
  authentication: "Authentication required",
  baseline_regression: "Outside learned baseline",
  entitlement: "Restricted",
  enterprise_required: "Restricted",
  incident_state_unknown: "Incident state unknown",
  incompatible_baseline: "Learning baseline",
  insufficient_baseline: "Learning baseline",
  low_confidence: "Low confidence",
  missing_distribution: "Unsupported",
  missing_service_attribution: "No service attribution",
  missing_target: "Unsupported",
  no_observations_in_window: "No recent data",
  no_regression: "Within learned baseline",
  permission: "Restricted",
  provider_status_unavailable: "Unavailable",
  sampled_data: "Unsupported",
  score_withheld: "Assessment withheld",
  source_not_configured: "Not connected",
  stale_baseline: "Stale",
  timeout: "Source timeout",
  unsupported_measure: "Unsupported",
  zero_traffic: "No recent data",
};

export function healthReasonLabel(reason: string | undefined, state: ServiceHealthState): string {
  if (reason && SERVICE_HEALTH_REASON_COPY[reason]) return SERVICE_HEALTH_REASON_COPY[reason];
  const stateLabels: Partial<Record<ServiceHealthState, string>> = {
    collecting: "Learning baseline",
    no_data: "No recent data",
    stale: "Stale",
    unsupported: "Unsupported",
    restricted: "Restricted",
  };
  return stateLabels[state] ?? SERVICE_HEALTH_STATE_COPY[state].label;
}

export function healthMeasureLabel(measure: string): string {
  const labels: Record<string, string> = {
    activity: "Log activity",
    anomalies: "Log anomalies",
    incidents: "Incidents",
    latency: "Latency P99",
    patterns: "Log patterns",
    request_context: "Request context",
    request_error_ratio: "Request error rate",
    regression_score: "Regression",
    silent: "No Versus incident",
    throughput: "Throughput",
  };
  return labels[measure] ?? measure.replaceAll("_", " ");
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