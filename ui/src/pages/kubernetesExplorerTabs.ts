export type KubernetesExplorerTab =
  | "overview"
  | "issues"
  | "timeline"
  | "topology"
  | "resources"
  | "nodes"
  | "releases"
  | "gitops"
  | "traffic";

export const kubernetesExplorerTabs: Array<{ id: KubernetesExplorerTab; label: string }> = [
  { id: "overview", label: "Overview" },
  { id: "issues", label: "Issues" },
  { id: "timeline", label: "Timeline" },
  { id: "topology", label: "Topology" },
  { id: "resources", label: "Workloads" },
  { id: "nodes", label: "Nodes" },
  { id: "releases", label: "Helm" },
  { id: "gitops", label: "GitOps" },
  { id: "traffic", label: "Traffic" },
];