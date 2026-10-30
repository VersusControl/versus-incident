import { useState, type ReactNode } from "react";
import { Box, CheckCircle2, ChevronRight } from "lucide-react";
import type { KubernetesResource, KubernetesWorkload } from "@/lib/api";
import { detailText, resourceContainerStates, resourceRestarts, resourceStart } from "@/lib/kubernetesResourceDetail";
import { isKnownKubernetesTime } from "@/lib/kubernetesTime";
import { clock, elapsed, resourceIDs } from "@/lib/kubernetesResourcePresentation";

export function ResourceHeader({
  resource,
  workload,
}: {
  resource: KubernetesResource;
  workload?: KubernetesWorkload;
}) {
  const summary = resource.summary ?? {};
  const logWorkload = ["Deployment", "StatefulSet"].includes(resource.kind);
  const restarts = resourceRestarts(resource, workload);
  const containers = resourceContainerStates(resource).filter(
    (state) => !state.init,
  );
  const ready =
    containers.length &&
    containers.every((state) => typeof state.ready === "boolean")
      ? `${containers.filter((state) => state.ready).length}/${containers.length}`
      : summary.ready !== undefined ? detailText(summary.ready) : "?";
  return (
    <span className="resource-identity block">
      <span className="flex min-w-0 items-center gap-2">
        <span className="resource-kind-icon inline-flex size-11 shrink-0 items-center justify-center rounded-control border border-accent/40 bg-accent-subtle text-link">
          <Box size={24} aria-hidden="true" />
        </span>
        <span className="min-w-0">
          <span className="block text-2xs font-normal uppercase tracking-wide text-ink-400">
            {resource.kind}
          </span>
          <span className="resource-name block break-all">{resource.name}</span>
        </span>
      </span>
      <span className="resource-status flex flex-wrap items-center gap-2 text-2xs font-normal">
        {!logWorkload && <span className={`pill ${summary.phase === "Running" ? "pill-good" : ""}`}>
          {detailText(
            summary.phase,
            workload ? `${workload.ready ?? "?"} ready` : "Status unavailable",
          )}
        </span>}
        <span className="pill" title={resource.kind === "Pod" ? "Regular containers ready" : "Ready replicas"}>
          Ready{" "}
          {resource.kind === "Pod"
            ? ready
            : workload
              ? `${workload.ready ?? "?"}/${workload.desired ?? "?"}`
              : detailText(summary.ready, "?")}
        </span>
        {!logWorkload && <span className={`pill ${restarts !== null && restarts > 0 ? "pill-warn text-sev-warning" : ""}`}>
          {restarts !== null && Number.isFinite(restarts) ? restarts : "?"} restarts
        </span>}
      <span className="resource-location text-xs font-normal text-ink-400">
        {resource.namespace || "Cluster scope"}
        {!logWorkload && <>{summary.node ? ` on ${detailText(summary.node)}` : ""}{"  "}up{" "}{elapsed(resourceStart(resource, workload))}</>}
      </span>
      </span>
    </span>
  );
}

export function ResourceFacts({
  resource,
  workload,
  onOpen,
}: {
  resource: KubernetesResource;
  workload?: KubernetesWorkload;
  onOpen: (resource: KubernetesResource) => void;
}) {
  const [expanded, setExpanded] = useState(false);
  const summary = resource.summary ?? {};
  const owner = resource.owners?.[0];
  const terminated = resourceContainerStates(resource)
    .filter((state) => state.last_terminated_reason)
    .sort(
      (a, b) =>
        (isKnownKubernetesTime(b.last_terminated_at)
          ? Date.parse(b.last_terminated_at)
          : -Infinity) -
        (isKnownKubernetesTime(a.last_terminated_at)
          ? Date.parse(a.last_terminated_at)
          : -Infinity),
    )[0];
  const conditions = resource.conditions ?? workload?.conditions ?? [];
  const facts: Array<[string, ReactNode]> = [
    ["Image", workload?.containers?.map((item) => item.image).filter(Boolean).join(", ") || detailText(summary.image)],
    ["QoS", detailText(summary.qos_class)],
    ["Node", detailText(summary.node ?? workload?.nodes?.join(", "))],
    ["Pod IP", detailText(summary.pod_ip)],
    ["Owner", owner ? <button className="resource-owner text-link" type="button" title={`${owner.kind} ${owner.name}`} disabled={!resourceIDs[owner.kind]} onClick={() => onOpen({ resource_id: resourceIDs[owner.kind], kind: owner.kind, name: owner.name, namespace: owner.namespace || resource.namespace, uid: owner.uid })}><span>{owner.kind} {owner.name}</span><ChevronRight size={12} aria-hidden="true" /></button> : "Unavailable"],
    ["Age", elapsed(isKnownKubernetesTime(resource.created_at) ? resource.created_at : summary.created_at)],
  ];
  return (
    <>
    <button type="button" className="resource-facts-toggle text-xs text-link sm:hidden" aria-expanded={expanded} aria-controls="resource-facts" onClick={() => setExpanded(!expanded)}>Details and conditions {expanded ? "−" : "+"}</button>
    <div id="resource-facts" data-mobile-open={expanded} className="resource-facts grid gap-3 sm:grid-cols-2">
      <section className="relative rounded-control border border-ink-700 bg-surface-sunken p-3">
        <h3 className="mb-3 text-xs font-semibold">Details</h3>
        <dl className="grid grid-cols-1 gap-x-3 gap-y-2 text-2xs sm:grid-cols-2">
          {facts.map(([label, value]) => (
            <div key={label} className="grid grid-cols-[4rem_1fr] gap-2">
              <dt className="text-ink-400">{label}</dt>
              <dd className="break-all">{value}</dd>
            </div>
          ))}
        </dl>
        {resource.projection_truncated?.length ? (
          <p className="mt-2 text-2xs text-ink-400">
            Some projected fields are unavailable.
          </p>
        ) : null}
      </section>
      <section className="rounded-control border border-ink-700 bg-surface-sunken p-3">
        <h3 className="mb-3 text-xs font-semibold">Conditions</h3>
        {!conditions.length && (
          <p className="text-xs text-ink-400">No projected conditions.</p>
        )}
        <div className="condition-grid grid grid-cols-1 gap-x-3 sm:grid-cols-2" style={{ gridTemplateRows: `repeat(${Math.max(1, Math.ceil(conditions.length / 2))}, auto)` }}>{conditions.map((condition) => (
          <div
            key={condition.type}
            className="mb-2 flex items-start gap-1.5 text-2xs"
          >
            <CheckCircle2
              size={13}
              className={
                condition.status === "True" ? "text-sev-ok" : condition.status === "False" ? "text-sev-warning" : "text-ink-400"
              }
              aria-hidden="true"
            />
            <span className="flex-1">
              <span title={condition.reason}>{condition.type === "ContainersReady" ? "Containers ready" : condition.type === "PodScheduled" ? "Scheduled" : condition.type}</span>
              {isKnownKubernetesTime(condition.last_transition_at) && (
                <time dateTime={condition.last_transition_at} title={new Date(condition.last_transition_at).toLocaleString()} className="float-right ml-1 text-2xs text-ink-400">
                  {clock(condition.last_transition_at)}
                </time>
              )}
            </span>
            <span className="sr-only">{condition.status}</span>
          </div>
        ))}</div>
        <p className="mt-2 text-2xs text-ink-400">
          Last state{" "}
          {terminated
            ? `${terminated.name}: ${terminated.last_terminated_reason} exit ${terminated.last_terminated_exit_code ?? "?"}${isKnownKubernetesTime(terminated.last_terminated_at) ? ` at ${clock(terminated.last_terminated_at)}` : ""}`
            : "unavailable"}
        </p>
      </section>
    </div>
    </>
  );
}
