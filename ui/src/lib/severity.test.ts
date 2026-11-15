import { describe, expect, it } from "vitest";
import { incidentSeverity, severityFromContent } from "./severity";

describe("severityFromContent", () => {
  it("falls back to the first alert's labels when the body has no common labels", () => {
    expect(
      severityFromContent({
        status: "firing",
        alerts: [{ labels: { alertname: "HighErrorRate", severity: "critical" } }, { labels: { severity: "info" } }],
      }),
    ).toBe("critical");
  });

  it("keeps common labels ahead of the first alert", () => {
    expect(
      severityFromContent({ commonLabels: { severity: "warning" }, alerts: [{ labels: { severity: "critical" } }] }),
    ).toBe("warn");
  });

  it("does not read a grouped body's severity from its first alert", () => {
    const alerts = [{ labels: { severity: "critical" } }, { labels: { severity: "info" } }];
    expect(severityFromContent({ commonLabels: { env: "prod" }, alerts })).toBeNull();
    expect(severityFromContent({ commonLabels: { env: "prod" }, alerts: alerts.slice(0, 1) })).toBe("critical");
  });

  it("reads the quickstart Alertmanager body's severity from its common labels", () => {
    const labels = { alertname: "HighErrorRate", severity: "critical", instance: "checkout-api:8080", job: "checkout-api" };
    expect(
      severityFromContent({
        receiver: "versus",
        status: "firing",
        alerts: [{ status: "firing", labels, annotations: { summary: "5xx error rate above 5% for 5 minutes" } }],
        groupLabels: { alertname: "HighErrorRate" },
        commonLabels: labels,
        version: "4",
      }),
    ).toBe("critical");
  });

  it("matches severity keys case-insensitively at every level", () => {
    expect(severityFromContent({ SEVERITY: "critical" })).toBe("critical");
    expect(severityFromContent({ Labels: { SEVERITY: "warning" } })).toBe("warn");
    expect(severityFromContent({ CommonLabels: { Level: "info" } })).toBe("info");
    expect(severityFromContent({ Alerts: [{ LABELS: { Severity: "high" } }] })).toBe("high");
  });

  it("prefers an exact key spelling over a case-insensitive match", () => {
    expect(severityFromContent({ severity: "info", SEVERITY: "critical" })).toBe("info");
    expect(severityFromContent({ level: "critical", SEVERITY: "info" })).toBe("critical");
  });

  it("reads commonLabels when labels carries no severity", () => {
    expect(severityFromContent({ labels: { env: "prod" }, commonLabels: { severity: "critical" } })).toBe("critical");
  });

  it("reads a CloudWatch alarm's severity dimension", () => {
    expect(
      severityFromContent({
        AlarmName: "HighCPU",
        Trigger: { Dimensions: [{ name: "InstanceId", value: "i-1" }, { Name: "Severity", Value: "critical" }] },
      }),
    ).toBe("critical");
    expect(severityFromContent({ trigger: { dimensions: [{ name: "level", value: "warning" }] } })).toBe("warn");
  });

  it("keeps top-level and label severity ahead of CloudWatch dimensions", () => {
    expect(
      severityFromContent({ severity: "info", Trigger: { Dimensions: [{ name: "Severity", value: "critical" }] } }),
    ).toBe("info");
  });

  it("resolves nothing from an unusable alerts value", () => {
    expect(severityFromContent({ alerts: [] })).toBeNull();
    expect(severityFromContent({ alerts: "x" })).toBeNull();
  });
});

describe("incidentSeverity", () => {
  it("uses the server-reported raw severity when a list row has no content", () => {
    expect(incidentSeverity(null, undefined, undefined, "critical")).toBe("critical");
    expect(incidentSeverity(null, undefined, undefined, "warning")).toBe("warn");
    expect(incidentSeverity(null, undefined, undefined, "")).toBeNull();
  });

  it("keeps the highest of the typed and reported severities", () => {
    expect(incidentSeverity(null, "warn", undefined, "critical")).toBe("critical");
    expect(incidentSeverity(null, "critical", undefined, "info")).toBe("critical");
  });
});
