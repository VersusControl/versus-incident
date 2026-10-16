import type { KubernetesIssue } from "@/lib/api";

type Evidence = NonNullable<KubernetesIssue["evidence"]>;
type Termination = NonNullable<Evidence["terminations"]>[number];
export type EvidencePill = { key: string; text: string; tone: "bad" | "warn" };

export const counted = (count: number, noun: string) => `${count} ${noun}${count === 1 ? "" : "s"}`;
const pods = (count: number) => `on ${counted(count, "pod")}`;
const hintLabels: Record<string, string> = { not_executable: "not executable", not_found: "command not found" };

export function terminationKey(item: Termination): string {
  return JSON.stringify([item.reason, item.exit_code, Boolean(item.init), item.signal ?? "", item.exit_hint ?? ""]);
}

export function terminationDescription(item: Termination, includeExit = false): string {
  const exit = includeExit || item.reason !== "OOMKilled" ? ` exit ${item.exit_code}` : "";
  const signal = item.signal ? ` signal ${item.signal}` : "";
  const hint = !item.signal && item.exit_hint ? ` (exit hint: ${hintLabels[item.exit_hint] ?? item.exit_hint})` : "";
  return `${item.init ? "Init " : ""}${item.reason}${exit}${signal}${hint} ${pods(item.count)}`;
}

export function crashDetail(item: Termination): string {
  const restarts = item.restarts === undefined ? "" : `, ${counted(item.restarts, "lifetime restart")}`;
  return `Crash: ${terminationDescription(item, true)}${restarts} (last occurrence)`;
}

export function evidencePills(evidence: Evidence): EvidencePill[] {
  return [
    ...(evidence.terminations ?? []).map((item): EvidencePill => ({ key: `t:${terminationKey(item)}`, text: terminationDescription(item), tone: "bad" })),
    ...(evidence.waiting ?? []).map((item): EvidencePill => ({ key: `w:${JSON.stringify([item.reason, Boolean(item.init)])}`, text: `${item.init ? "Init " : ""}${item.reason} ${pods(item.count)}`, tone: "warn" })),
    ...(evidence.pod_reasons ?? []).map((item): EvidencePill => ({ key: `p:${item.reason}`, text: `${item.reason} ${pods(item.count)}`, tone: item.reason === "Unschedulable" ? "warn" : "bad" })),
    ...(evidence.conditions ?? []).map((item): EvidencePill => ({ key: `c:${JSON.stringify([item.type, item.reason])}`, text: item.reason, tone: "bad" })),
    ...(evidence.nodes ?? []).map((item): EvidencePill => ({ key: `n:${item.reason}`, text: `Node ${item.reason} ${pods(item.count)}`, tone: "warn" })),
  ];
}

export function warningSummary(evidence: Evidence): string {
  const reasons = (evidence.warning_reasons ?? []).map((item) => `${item.reason} ${item.count}`).join(", ");
  return `${counted(evidence.warning_events, "warning event")}${reasons ? `: ${reasons}` : ""}`;
}