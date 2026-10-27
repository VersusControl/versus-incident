import type { KubernetesChangesPage, KubernetesIssue, KubernetesIssuePage } from "@/lib/api";
import { settledSync } from "@/lib/kubernetesPending";

export type KubernetesWarning = { id: string; content: string };

export function failureWarnings(failures?: Array<{ resource_id?: string; scope?: string; class: string }> | null): KubernetesWarning[] {
  const reasons: Record<string, string> = {
    forbidden: "permission denied",
    unauthorized: "access denied",
    unavailable: "not available",
    not_found: "not installed or unavailable",
    timeout: "source did not respond",
    source_error: "source could not be read",
  };
  return (failures ?? []).filter((failure) => !["item_limit", "request_limit", "budget", "page_limit", "syncing"].includes(failure.class)).map((failure) => ({
    id: `failure:${failure.resource_id ?? ""}:${failure.scope ?? ""}:${failure.class}`,
    content: `${failure.resource_id?.split("~").at(-1) || failure.scope || "Kubernetes resources"}: ${reasons[failure.class] ?? "could not be read"}`,
  }));
}

export function issueWarnings(issue?: KubernetesIssue): KubernetesWarning[] {
  if (!issue) return [];
  const warnings: KubernetesWarning[] = [];
  if (issue.gaps?.length) {
    warnings.push({ id: "history", content: "Change capture does not cover the whole range" });
  }

  for (const kind of [...new Set(issue.untracked ?? [])].sort()) warnings.push({ id: `untracked:${kind}`, content: `${kind} history is not tracked` });
  return warnings;
}

export function timelineWarnings(issues?: KubernetesIssuePage, changes?: KubernetesChangesPage): KubernetesWarning[] {
  const warnings = [
    ...failureWarnings(issues?.partial_failures),
    ...failureWarnings(changes?.partial_failures),
    ...(issues?.items ?? []).flatMap(issueWarnings),
  ];
  if (issues?.items.some((issue) => issue.evidence?.partial)) warnings.push({ id: "crash-capture", content: "Some crash evidence could not be captured" });
  if (issues?.sync.state === "stale") warnings.push({ id: "break-stale", content: "Break evidence is stale." });
  if (settledSync(issues?.sync, issues?.partial_failures)?.partial && !issues?.partial_failures?.length && !issues?.truncated) warnings.push({ id: "break-partial", content: "Some issue evidence could not be read" });
  if (changes?.gaps?.length) warnings.push({ id: "changes-gaps", content: "Change capture does not cover the whole range" });
  if (changes?.sync.state === "stale") warnings.push({ id: "changes-stale", content: "Change activity is stale." });
  if (changes?.sync.partial && !changes.gaps?.length && !changes.partial_failures?.length) warnings.push({ id: "changes-partial", content: "Change activity reflects incomplete history." });
  return warnings;
}