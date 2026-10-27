import { ScrollText } from "lucide-react";
import type { KubernetesResource } from "@/lib/api";
import { canStreamLogs } from "@/lib/kubernetesLogTargets";

// One row action shared by every list that can reach logs: it opens the drawer
// straight on the Logs tab instead of the row's default tab.
export function ResourceLogsButton({
  resource,
  onSelect,
  label,
}: {
  resource: KubernetesResource;
  onSelect: (resource: KubernetesResource, tab: "logs") => void;
  label?: string;
}) {
  if (!canStreamLogs(resource)) return null;
  const name = label ?? resource.name;
  return (
    <button
      type="button"
      data-row-action="logs"
      className="btn shrink-0 justify-center px-2"
      aria-label={`Logs for ${name}`}
      title="Logs"
      onClick={(event) => {
        // The row itself opens its default tab; this action owns its click.
        event.stopPropagation();
        onSelect(resource, "logs");
      }}
    >
      <ScrollText size={14} aria-hidden="true" />
      <span className="sr-only">Logs</span>
    </button>
  );
}
