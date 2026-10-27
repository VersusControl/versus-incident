import { describe, expect, it } from "vitest";
import type { KubernetesIssue } from "@/lib/api";
import { counted, crashDetail, evidencePills, terminationKey, warningSummary } from "./kubernetesEvidence";

const evidence: NonNullable<KubernetesIssue["evidence"]> = { warning_events: 0, from: "", to: "" };
const termination = { reason: "Error", exit_code: 137, count: 1, last: "2026-10-16T12:00:00Z" };

describe("Kubernetes evidence", () => {
  it("orders all evidence kinds with distinct Pod counts and init identity", () => {
    expect(evidencePills({ ...evidence,
      terminations: [{ ...termination, reason: "OOMKilled", count: 6 }, { ...termination, exit_code: 1, init: true }],
      waiting: [{ reason: "CreateContainerConfigError", count: 3, init: true }],
      pod_reasons: [{ reason: "Evicted", count: 4 }, { reason: "Unschedulable", count: 1 }],
      conditions: [{ type: "Failed", reason: "BackoffLimitExceeded" }],
      nodes: [{ reason: "MemoryPressure", count: 2 }],
    }).map(({ text, tone }) => [text, tone])).toEqual([
      ["OOMKilled on 6 pods", "bad"], ["Init Error exit 1 on 1 pod", "bad"],
      ["Init CreateContainerConfigError on 3 pods", "warn"], ["Evicted on 4 pods", "bad"],
      ["Unschedulable on 1 pod", "warn"], ["BackoffLimitExceeded", "bad"], ["Node MemoryPressure on 2 pods", "warn"],
    ]);
  });

  it.each(["SIGABRT", "SIGKILL", "SIGSEGV", "SIGTERM", "31"])("shows only the observed signal %s as a signal", (signal) => {
    const text = evidencePills({ ...evidence, terminations: [{ ...termination, signal, exit_hint: "SIGKILL" }] })[0].text;
    expect(text).toBe(`Error exit 137 signal ${signal} on 1 pod`);
  });

  it.each([["SIGKILL", "SIGKILL"], ["not_executable", "not executable"], ["not_found", "command not found"]])("qualifies conventional exit hint %s", (exit_hint, label) => {
    expect(evidencePills({ ...evidence, terminations: [{ ...termination, exit_hint }] })[0].text).toBe(`Error exit 137 (exit hint: ${label}) on 1 pod`);
  });

  it("does not invent a signal, hint, or unknown restart count from exit codes", () => {
    expect(crashDetail(termination)).toBe("Crash: Error exit 137 on 1 pod (last occurrence)");
    expect(crashDetail({ ...termination, restarts: 41 })).toBe("Crash: Error exit 137 on 1 pod, 41 lifetime restarts (last occurrence)");
    expect(crashDetail({ ...termination, init: true, restarts: 1 })).toContain("Init Error exit 137 on 1 pod, 1 lifetime restart");
  });

  it("keeps same-reason buckets separate by exit, init, actual signal and hint", () => {
    const items = [termination, { ...termination, exit_code: 1 }, { ...termination, init: true }, { ...termination, signal: "SIGKILL" }, { ...termination, signal: "31" }, { ...termination, exit_hint: "SIGKILL" }];
    expect(new Set(items.map(terminationKey)).size).toBe(items.length);
    expect(new Set(evidencePills({ ...evidence, terminations: items }).map((pill) => pill.key)).size).toBe(items.length);
  });

  it("preserves warning totals and every provided reason including Other", () => {
    expect(warningSummary({ ...evidence, warning_events: 12, partial: true, warning_reasons: [{ reason: "FailedScheduling", count: 8 }, { reason: "Unhealthy", count: 2 }, { reason: "FailedMount", count: 1 }, { reason: "Other", count: 1 }] })).toBe("12 warning events: FailedScheduling 8, Unhealthy 2, FailedMount 1, Other 1");
    expect(warningSummary({ ...evidence, warning_events: 1 })).toBe("1 warning event");
    expect(warningSummary(evidence)).toBe("0 warning events");
    expect(evidencePills(evidence)).toEqual([]);
  });

  it("pluralizes a count the way English does, including the sibilants", () => {
    expect(counted(1, "event")).toBe("1 event");
    expect(counted(2, "event")).toBe("2 events");
    expect(counted(0, "crash")).toBe("0 crashes");
    expect(counted(1, "crash")).toBe("1 crash");
    expect(counted(2, "crash")).toBe("2 crashes");
    expect(counted(1, "captured crash")).toBe("1 captured crash");
    expect(counted(2400, "captured crash")).toBe("2400 captured crashes");
    expect(counted(1, "pod")).toBe("1 pod");
  });
});
