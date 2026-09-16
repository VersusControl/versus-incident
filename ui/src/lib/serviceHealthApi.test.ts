import { afterEach, describe, expect, it, vi } from "vitest";
import { api, ApiError, type ServiceHealthService } from "./api";

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("Service Health API", () => {
  it("preserves omitted, null, zero, and false enrichment wire values", () => {
    const omitted = {
      org_id: "default", service: "checkout", domain: "Commerce", kind: "service",
      severity: "nominal", assessment_basis: "Internal data only", active_incidents: null,
      logs: { service: "checkout", source_id: "", matched_logs: 0, unique_patterns: 0, new_patterns: 0, unknown_patterns: 0, spiking_patterns: 0, severity_counts: {}, estimated: false },
      availability: {},
    } satisfies ServiceHealthService;
    const explicit = {
      ...omitted,
      base_severity: "nominal",
      evidence: [{
        org_id: "default", service: "checkout", family: "metrics", measure: "request_error_ratio",
        value: null, availability: { state: "collecting" }, source_ref: "signoz_metrics",
        observed_at: "2026-09-15T10:00:00Z", window_start: "2026-09-15T09:55:00Z", window_end: "2026-09-15T10:00:00Z",
      }],
      assessment: {
        org_id: "default", service: "checkout", regression_score: 0, silent: false,
        confidence: 0.8, algorithm_version: "learned-health-v1", assessed_at: "2026-09-15T10:00:00Z",
        drivers: [{ family: "metrics", measure: "latency_p99", weight: 0 }],
      },
    } satisfies ServiceHealthService;
    const withheld = {
      ...explicit,
      assessment: { ...explicit.assessment, regression_score: null, silent: null },
    } satisfies ServiceHealthService;

    expect(omitted.assessment).toBeUndefined();
    expect(explicit.evidence[0].value).toBeNull();
    expect(explicit.assessment.regression_score).toBe(0);
    expect(explicit.assessment.silent).toBe(false);
    expect(withheld.assessment.regression_score).toBeNull();
    expect(withheld.assessment.silent).toBeNull();
  });

  it("uses the snapshot and settings routes with the backend PATCH shape", async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ snapshot_id: "snap-1" }), { status: 200 }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ interval_seconds: 60, window_seconds: 300, revision: 2 }), { status: 200 }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ interval_seconds: 30, window_seconds: 120, revision: 3 }), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);

    await api.getServiceHealth();
    await api.getServiceHealthSettings();
    await api.updateServiceHealthSettings({ interval_seconds: 30, window_seconds: 120, revision: 2 });

    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "/api/agent/service-health",
      "/api/agent/service-health/settings",
      "/api/agent/service-health/settings",
    ]);
    expect(fetchMock.mock.calls[2][1]).toMatchObject({
      method: "PATCH",
      body: JSON.stringify({ interval_seconds: 30, window_seconds: 120, revision: 2 }),
    });
  });

  it("preserves a settings conflict as ApiError status 409", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(
      JSON.stringify({ error: "service health settings changed concurrently; retry" }),
      { status: 409 },
    )));

    await expect(api.updateServiceHealthSettings({
      interval_seconds: 60,
      window_seconds: 300,
      revision: 1,
    })).rejects.toMatchObject<ApiError>({ status: 409 });
  });
});