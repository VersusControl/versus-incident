import { describe, expect, it } from "vitest";
import { kubernetesIssuePrompt } from "@/lib/kubernetesIssuePrompt";

describe("Kubernetes issue investigation prompt", () => {
  it.each([
    ["apps~v1~deployments", "pod.crash_loop", "Deployment checkout crash looping"],
    ["batch~v1~jobs", "job.failed", "Job checkout failing"],
    ["core~v1~nodes", "node.not_ready", "Node checkout not ready"],
    ["apps~v1~statefulsets", "workload.rollout_stalled", "StatefulSet checkout stalled during rollout"],
  ])("uses the workload kind and issue label for %s", (resource, issue, expected) => {
    expect(kubernetesIssuePrompt(new URLSearchParams({ provider: "kubernetes", resource_id: resource, name: "checkout", issue }))).toBe(`Why is ${expected}?`);
  });
  it.each([
    "provider=kubernetes&resource_id=apps~v1~deployments&name=checkout",
    "provider=kubernetes&resource_id=apps~v1~deployments&name=checkout&issue=unknown",
    "provider=kubernetes&resource_id=apps~v1~deployments&issue=pod.crash_loop",
    "provider=other&resource_id=apps~v1~deployments&name=checkout&issue=pod.crash_loop",
    "provider=kubernetes&resource_id=unknown&name=checkout&issue=pod.crash_loop",
    "provider=kubernetes&resource_id=apps~v1~deployments&name=checkout&issue=pod.crash_loop&session=saved",
  ])("leaves other composer entry paths unchanged: %s", (query) => {
    expect(kubernetesIssuePrompt(new URLSearchParams(query))).toBe("");
  });
});
