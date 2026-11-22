import { describe, expect, it } from "vitest";
import { failureWarnings } from "./kubernetesWarningItems";

describe("failure warnings", () => {
  it("writes a failed API discovery as a sentence, not the internal stage name", () => {
    const text = failureWarnings([
      { scope: "discovery", class: "timeout" },
      { scope: "discovery", class: "forbidden" },
      { scope: "discovery", class: "source_error" },
    ]).map((warning) => warning.content);
    expect(text).toEqual([
      "The cluster did not list its resource types in time",
      "The cluster's resource types could not be listed: access denied",
      "The cluster's resource types could not be listed",
    ]);
    expect(text.join(" ")).not.toContain("discovery");
  });

  it("says issues stay live while saved break history is unavailable, without blaming change capture", () => {
    const [warning] = failureWarnings([{ resource_id: "kubernetes.history", class: "historical_evidence_unavailable" }]);
    expect(warning.content).toBe("Issues are live, but saved break history is temporarily unavailable, so break times and occurrence links are hidden. It is retried on the next refresh.");
    expect(warning.content).not.toMatch(/change history|change capture|next sync/i);
  });

  it("keeps naming resource types by their kind", () => {
    expect(failureWarnings([{ resource_id: "argoproj.io~v1alpha1~rollouts", class: "timeout" }])[0].content).toBe("Rollout took too long to read; the next sync tries again.");
  });
});
