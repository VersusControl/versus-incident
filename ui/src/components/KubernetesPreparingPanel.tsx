import { useId } from "react";
import { Check, Loader2, Radar } from "lucide-react";
import type { KubernetesSyncProgress } from "@/lib/api";
import { firstSyncCount, firstSyncCountLabel, firstSyncCounts, firstSyncListingProgress, firstSyncStepIndex, firstSyncSteps } from "@/lib/kubernetesFirstSync";

function version(value?: string) {
  if (!value) return undefined;
  const match = /^v?(\d+)\.(\d+)/.exec(value);
  return match ? `v${match[1]}.${match[2]}` : value;
}

// Shown in place of every Kubernetes view until the cluster's first
// cluster-wide ready generation is published. It reports progress only; nothing here is
// presented as complete cluster data.
export function KubernetesPreparingPanel({ progress, cluster }: { progress?: KubernetesSyncProgress; cluster?: string }) {
  const titleID = useId();
  const current = firstSyncStepIndex(progress);
  const counts = firstSyncCounts(progress);
  const serverVersion = version(progress?.cluster_version);
  const nodes = progress?.node_count;
  const facts = [serverVersion && `Kubernetes ${serverVersion}`, nodes !== undefined && Number.isSafeInteger(nodes) && nodes >= 0 && `${firstSyncCount(nodes)} ${nodes === 1 ? "node" : "nodes"}`].filter((fact): fact is string => Boolean(fact));
  const listing = firstSyncListingProgress(progress);
  const progressLabel = listing ? `Listing resource types: ${listing.done} / ${listing.total}` : "Waiting for preparation progress";
  return <section aria-labelledby={titleID} data-preparing-panel className="card rise-in overflow-hidden">
    <div className="flex flex-wrap items-start gap-4 border-b border-ink-700 p-4 sm:p-6">
      <span aria-hidden="true" className="relative inline-flex size-11 shrink-0 items-center justify-center rounded-full bg-accent-subtle text-link">
        <span data-preparing-motion className="absolute inset-0 animate-ping rounded-full bg-accent/20 motion-reduce:hidden" />
        <Radar size={22} data-preparing-motion className="relative animate-spin [animation-duration:3s] motion-reduce:animate-none" />
      </span>
      <div className="min-w-0 flex-1 basis-56 sm:basis-0">
        <h2 id={titleID} className="text-lg font-semibold text-ink-50">Preparing your cluster</h2>
        <p className="mt-1 text-xs text-ink-300">First sync{cluster ? ` of ${cluster}` : ""}: every tab opens with data once this finishes.</p>
      </div>
      {facts.length > 0 && <ul aria-label="Cluster facts" className="flex w-full flex-wrap gap-2 sm:w-auto">{facts.map((fact) => <li key={fact} className="pill">{fact}</li>)}</ul>}
    </div>
    <ol aria-label="First sync steps" className="divide-y divide-ink-700">
      {firstSyncSteps.map((step, index) => {
        const done = current >= 0 && Boolean(progress?.steps_done?.includes(step.id));
        const active = !done && index === current && !progress?.phase_status?.[step.id];
        const state = done ? "done" : active ? "active" : "waiting";
        // Listed counts are separate elements, never one joined string: the
        // panel line separates them with spacing alone.
        const listed = step.id === "listing" && (active || done) && counts.length
          ? counts.filter(([kind]) => kind !== "Node").slice(0, 4).map(([kind, value]) => firstSyncCountLabel(kind, value))
          : [];
        const detail = step.id === "connected" && done && serverVersion ? `Kubernetes ${serverVersion}`
          : listed.length ? undefined
            : progress?.phase_status?.[step.id] ?? (active ? step.id === "listing" ? "counting" : "in progress" : done ? "done" : "waiting");
        return <li key={step.id} data-step={step.id} data-state={state} aria-current={active ? "step" : undefined} className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1 px-4 py-3 text-sm sm:flex-nowrap sm:gap-3 sm:px-6">
          <span aria-hidden="true" className={`inline-flex size-6 shrink-0 items-center justify-center rounded-full ${done ? "bg-sev-ok/15 text-sev-ok" : active ? "bg-accent-subtle text-link" : "bg-ink-700 text-ink-400"}`}>
            {done ? <Check size={14} /> : active ? <Loader2 size={14} data-preparing-motion className="animate-spin motion-reduce:animate-none" /> : <span className="size-1.5 rounded-full bg-current" />}
          </span>
          <span className={`min-w-0 flex-1 ${done ? "text-ink-100" : active ? "font-medium text-ink-50" : "text-ink-400"}`} data-first-sync-step-label>{done ? step.label : active ? step.active : step.label}<span className="sr-only">: {state === "done" ? "done" : state === "active" ? "in progress" : progress?.phase_status?.[step.id] ?? state}</span></span>
          <span className={`min-w-0 text-right text-xs tabular-nums ${listed.length ? "basis-full pl-9 sm:basis-auto sm:pl-0" : ""} ${active ? "text-ink-200" : "text-ink-400"}`}>
            {/* Spacing alone sets the counts apart, so their name is one phrase
                read with commas rather than one run-on word. */}
            {listed.length ? <span data-first-sync-counts role="group" aria-label={listed.join(", ")} className="flex flex-wrap justify-start gap-x-2 gap-y-1 sm:justify-end">{listed.map((count) => <span key={count} aria-hidden="true" data-first-sync-count className="max-w-full break-words">{count}</span>)}</span> : detail}
          </span>
        </li>;
      })}
    </ol>
    <div className="space-y-2 border-t border-ink-700 px-4 py-4 sm:px-6">
      <div role="progressbar" aria-label={progressLabel} aria-valuemin={listing ? 0 : undefined} aria-valuemax={listing?.total} aria-valuenow={listing?.done} aria-valuetext={listing ? `${listing.done} / ${listing.total} resource types checked` : undefined} className="h-1.5 overflow-hidden rounded-full bg-ink-700">
        <div className={`h-full rounded-full bg-accent ${listing ? "transition-[width] duration-500 motion-reduce:transition-none" : "w-1/3 animate-pulse motion-reduce:animate-none"}`} style={listing ? { width: `${listing.done / listing.total * 100}%` } : undefined} />
      </div>
      <p className="text-2xs text-ink-400">{progressLabel}</p>
      {Boolean(progress?.remaining_kinds?.length) && <p className="text-2xs text-ink-400">Remaining: {progress!.remaining_kinds!.map((kind) => `${kind} (${progress?.kind_status?.[kind] ?? "pending"})`).join(", ")}</p>}
    </div>
  </section>;
}
