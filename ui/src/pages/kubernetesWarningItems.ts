import type { KubernetesIssue } from "@/lib/api";

export type KubernetesWarning = { id: string; content: string };

export function failureWarnings(failures?: Array<{ resource_id?: string; scope?: string; class: string }> | null): KubernetesWarning[] {
  return (failures ?? []).map((failure) => ({ id: `failure:${failure.resource_id ?? ""}:${failure.scope ?? ""}:${failure.class}`, content: `${failure.resource_id || failure.scope || "Kubernetes resources"}: ${failure.class.replaceAll("_", " ")}` }));
}

export function issueWarnings(issue?: KubernetesIssue): KubernetesWarning[] {
  if (!issue) return [];
  const warnings: KubernetesWarning[] = [];
  if (issue.history !== "complete" || issue.gaps?.length) {
    const rebuilt = issue.rebuilt?.length ? ` ${issue.rebuilt.map((value) => value[0].toUpperCase() + value.slice(1)).join(" and ")} history rebuilt from ${issue.root.kind === "Deployment" ? "ReplicaSets" : "ControllerRevisions"}; config and node changes not available.` : "";
    const gaps = (issue.gaps ?? []).map((gap) => `${new Date(gap.from).toLocaleString()} - ${new Date(gap.to).toLocaleString()}`).join(", ");
    warnings.push({ id: "history", content: `${issue.history ? "History incomplete." : "History completeness not yet loaded."}${rebuilt}${gaps ? ` ${gaps}` : ""}` });
  }
  for (const kind of [...new Set(issue.untracked ?? [])].sort()) warnings.push({ id: `untracked:${kind}`, content: kind === "Secret" ? "Secrets are not tracked." : `Not tracked: ${kind}.` });
  if (issue.partial) warnings.push({ id: "unlinked", content: "Some changes could not be linked." });
  return warnings;
}