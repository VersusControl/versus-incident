export type KubernetesExplorerTab =
  | "overview"
  | "issues"
  | "timeline"
  | "topology"
  | "resources"
  | "nodes"
  | "releases"
  | "gitops"
  | "traffic"
  | "agent";

export const kubernetesExplorerTabs: Array<{ id: KubernetesExplorerTab; label: string }> = [
  { id: "overview", label: "Overview" },
  { id: "resources", label: "Workloads" },
  { id: "issues", label: "Issues" },
  { id: "timeline", label: "Timeline" },
  { id: "topology", label: "Topology" },
  { id: "nodes", label: "Nodes" },
  { id: "releases", label: "Helm" },
  { id: "gitops", label: "GitOps" },
  { id: "traffic", label: "Traffic" },
  { id: "agent", label: "Agent" },
];