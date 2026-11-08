import { describe, expect, it } from "vitest";
import type { KubernetesIssue, KubernetesIssuePage } from "@/lib/api";
import { knownFailureReason } from "@/lib/kubernetesSyncHealth";
import { failureWarnings, issueWarnings, timelineWarnings } from "./kubernetesWarningItems";

// Failure classes share kubernetesSyncHealth's phrases, the history source is
// named, and a capped per-issue window has its own warning.
const issue: KubernetesIssue = { root: { kind: "Deployment", name: "api" }, rule: "workload.unavailable", severity: "critical", count: 1 };
const capWarning = "Some changes before this break were not loaded";

describe("Kubernetes warning wording", () => {
  it("words budget and storage failures with the shared reason phrases", () => {
    const warnings = failureWarnings([
      { resource_id: "kubernetes.history", class: "budget_exhausted" },
      { resource_id: "kubernetes.history", class: "storage_unavailable" },
    ]);
    expect(warnings.map((warning) => warning.content)).toEqual([
      `Change history: ${knownFailureReason("budget_exhausted")}`,
      `Change history: ${knownFailureReason("storage_unavailable")}`,
    ]);
    expect(warnings[0]?.content).toBe("Change history: the read budget for this sync ran out");
    expect(warnings.some((warning) => warning.content.includes("could not be read"))).toBe(false);
  });

  it.each(["record_limit", "incomplete_pages", "storage_rejected", "throttled", "read_failed", "unsupported", "credential_unavailable"])(
    "words the known %s class with its shared phrase instead of the fallback",
    (failureClass) => {
      const reason = knownFailureReason(failureClass);
      expect(reason).toBeTruthy();
      expect(failureWarnings([{ resource_id: "kubernetes.history", class: failureClass }])[0]?.content).toBe(`Change history: ${reason}`);
    },
  );

  it("labels the history source and keeps unknown classes honest", () => {
    expect(failureWarnings([{ resource_id: "kubernetes.history", class: "unavailable" }])[0]?.content).toBe("Change history: not available");
    expect(failureWarnings([{ resource_id: "kubernetes.history", class: "not_a_known_class" }])[0]?.content).toBe("Change history: could not be read");
    expect(failureWarnings([{ resource_id: "kubernetes.history", class: "" }])[0]?.content).toBe("Change history: could not be read");
  });

  it("keeps the resource name for sources other than change history", () => {
    expect(failureWarnings([{ resource_id: "apps~v1~deployments", class: "budget_exhausted" }])[0]?.content).toBe("deployments: the read budget for this sync ran out");
    expect(failureWarnings([{ class: "budget_exhausted" }])[0]?.content).toBe("Kubernetes resources: the read budget for this sync ran out");
  });

  it("returns no warnings for missing or empty failure lists", () => {
    expect(failureWarnings(undefined)).toEqual([]);
    expect(failureWarnings(null)).toEqual([]);
    expect(failureWarnings([])).toEqual([]);
  });

  it("adds the per-issue warning when a break's window was capped", () => {
    expect(issueWarnings({ ...issue, history_truncated: true })).toContainEqual({ id: "history-truncated", content: capWarning });
    expect(issueWarnings(issue)).toEqual([]);
    expect(issueWarnings({ ...issue, history_truncated: false })).toEqual([]);
    expect(issueWarnings(undefined)).toEqual([]);
  });

  it("lists the cap warning beside the gap and untracked warnings, once", () => {
    const warnings = issueWarnings({ ...issue, history_truncated: true, gaps: [{ from: "2026-11-05T10:00:00Z", to: "2026-11-05T10:05:00Z" }], untracked: ["Secret", "Secret"] });
    expect(warnings.map((warning) => warning.content)).toEqual([
      "Change capture does not cover the whole range",
      "Secret history is not tracked",
      capWarning,
    ]);
  });

  it("keeps a capped break's warning on its own issue, never as a list-level read failure", () => {
    const page: KubernetesIssuePage = {
      items: [{ ...issue, history_truncated: true, partial: true }, { ...issue, root: { kind: "Deployment", name: "web" } }],
      totals: { critical: 2 },
      truncated: false,
      sync: { state: "ready", age_s: 0, partial: false },
    };
    const contents = timelineWarnings(page).map((warning) => warning.content);
    expect(contents).toEqual([capWarning]);
    expect(contents.some((content) => content.includes("could not be read"))).toBe(false);
  });
});
