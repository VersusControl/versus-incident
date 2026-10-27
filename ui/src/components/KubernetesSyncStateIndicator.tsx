import { Loader2 } from "lucide-react";
import type { KubernetesSyncHealthState } from "@/lib/kubernetesSyncHealth";

/** How the index record feed is doing, which only matters once the sync is ready. */
export type KubernetesStreamState = "connecting" | "live" | "reconnecting" | "resyncing";

const syncing: Record<KubernetesStreamState, string> = {
  connecting: "Connecting",
  live: "Live",
  reconnecting: "Reconnecting",
  resyncing: "Resyncing",
};

/**
 * The cluster sync state in the top bar.
 *
 * One rule for all four states at every width: the state is always written out,
 * so a narrow viewport never leaves an operator with a coloured dot alone. The
 * dot only repeats what the words already say.
 */
export function KubernetesSyncStateIndicator({ state, streamState, ageSeconds, title }: {
  state: KubernetesSyncHealthState;
  streamState: KubernetesStreamState;
  ageSeconds?: number;
  title?: string;
}) {
  const label = state === "ready"
    ? `${syncing[streamState]}${streamState === "live" && ageSeconds !== undefined ? ` (${Math.round(ageSeconds)}s)` : ""}`
    : state === "preparing" ? "Preparing" : state === "degraded" ? "Degraded" : "Failed";
  const ink = state === "failed" ? "text-sev-critical"
    : state === "degraded" ? "text-sev-warning"
      : state === "preparing" ? "text-ink-300"
        : streamState === "live" ? "text-sev-ok" : "text-ink-400";
  const dot = state === "failed" ? "bg-sev-critical"
    : state === "degraded" ? "bg-sev-warning"
      : state === "preparing" ? "bg-accent"
        : streamState === "live" ? "bg-sev-ok" : "bg-ink-400";
  return <span role="status" aria-live="polite" data-sync-state={state} title={title} className={`inline-flex min-w-0 items-center gap-1.5 whitespace-nowrap text-2xs font-medium ${ink}`}>
    {state === "preparing"
      ? <Loader2 size={13} aria-hidden="true" className="shrink-0 animate-spin motion-reduce:animate-none" />
      : <span aria-hidden="true" className={`size-1.5 shrink-0 rounded-full ${dot}`} />}
    {label}
  </span>;
}
