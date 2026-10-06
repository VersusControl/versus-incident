export type KubernetesExplorerTab =
  | "overview"
  | "issues"
  | "timeline"
  | "topology"
  | "resources"
  | "releases"
  | "gitops"
  | "traffic";

export const kubernetesExplorerTabs: Array<{ id: KubernetesExplorerTab; label: string }> = [
  { id: "overview", label: "Overview" },
  { id: "issues", label: "Issues" },
  { id: "timeline", label: "Timeline" },
  { id: "topology", label: "Topology" },
  { id: "resources", label: "Resources" },
  { id: "releases", label: "Helm" },
  { id: "gitops", label: "GitOps" },
  { id: "traffic", label: "Traffic" },
];