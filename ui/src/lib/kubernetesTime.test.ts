import { describe, expect, it } from "vitest";
import { isKnownKubernetesTime } from "./kubernetesTime";
import { projectedResourceYaml } from "./kubernetesProjection";

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
