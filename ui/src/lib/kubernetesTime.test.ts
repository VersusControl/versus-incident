import { describe, expect, it } from "vitest";
import { isKnownKubernetesTime, kubernetesTimelineWindow } from "./kubernetesTime";
import { projectedResourceYaml } from "./kubernetesProjection";

describe("Timeline request windows", () => {
  const now = Date.parse("2026-10-30T05:30:00Z");
  it.each([60, 360, 1440])("keeps the full %s minute history span when centering would add future padding", (minutes) => {
    const result = kubernetesTimelineWindow(now - 5 * 60_000, minutes, true, now);
    expect(result.until).toBe(new Date(now).toISOString());
    expect(Date.parse(result.until) - Date.parse(result.since)).toBe(minutes * 60_000);
    expect(Date.parse(result.since)).toBeLessThan(Date.parse(result.until));
  });
  it("retains all-history epoch bounds, noncentered ranges and the root chart's symmetric axis", () => {
    expect(kubernetesTimelineWindow(now + 30_000, 0, true, now)).toEqual({ since: new Date(0).toISOString(), until: new Date(now).toISOString() });
    expect(kubernetesTimelineWindow(now, 60, false, now)).toEqual({ since: new Date(now - 60 * 60_000).toISOString(), until: new Date(now).toISOString() });
    expect(kubernetesTimelineWindow(now - 5 * 60_000, 60, true).until).toBe(new Date(now + 25 * 60_000).toISOString());
  });
  it("does not legalize future centers outside the server's clock-skew allowance", () => {
    const result = kubernetesTimelineWindow(now + 60_001, 60, true, now);
    expect(Date.parse(result.until)).toBeGreaterThan(now + 60_000);
  });
});

describe("Projected resource timestamps", () => {
  it.each([
    undefined,
    null,
    "",
    "invalid",
    "0001-01-01T00:00:00Z",
    "0001-01-01T00:00:00.0000000+00:00",
  ])("treats %s as unknown", (value) => {
    expect(isKnownKubernetesTime(value)).toBe(false);
  });
  it.each([
    "1970-01-01T00:00:00Z",
    "1969-12-31T23:59:59Z",
    "2026-10-21T08:00:00.123456Z",
    "2026-10-21T15:00:00+07:00",
  ])("retains a real timestamp %s", (value) => {
    expect(isKnownKubernetesTime(value)).toBe(true);
  });
  it("omits unknown metadata, condition and nested container times without dropping valid dates or termination evidence", () => {
    const yaml = projectedResourceYaml({
      resource_id: "core~v1~pods",
      kind: "Pod",
      name: "checkout",
      created_at: "0001-01-01T00:00:00Z",
      conditions: [
        {
          type: "Ready",
          status: "False",
          last_transition_at: "0001-01-01T00:00:00Z",
        },
        {
          type: "Scheduled",
          status: "True",
          last_transition_at: "2026-10-21T08:00:00Z",
        },
      ],
      summary: {
        created_at: "invalid",
        container_states: [
          {
            name: "app",
            started_at: "0001-01-01T00:00:00Z",
            last_terminated_at: "0001-01-01T00:00:00Z",
            last_terminated_reason: "OOMKilled",
          },
          { name: "proxy", started_at: "1970-01-01T00:00:00Z" },
        ],
      },
    });
    expect(yaml).not.toContain("0001-01-01");
    expect(yaml).not.toContain("invalid");
    expect(yaml).not.toContain("creationTimestamp:");
    expect(yaml).toContain("OOMKilled");
    expect(yaml).toContain("2026-10-21T08:00:00Z");
    expect(yaml).toContain("1970-01-01T00:00:00Z");
  });
});
