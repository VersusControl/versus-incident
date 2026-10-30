import { useId } from "react";
import { ChevronRight } from "lucide-react";
import type { KubernetesGraph, KubernetesResource } from "@/lib/api";
import { CoverageMarker } from "@/components/CoverageMarker";
import { KubernetesPendingNote } from "@/components/KubernetesPendingNote";
import { KubernetesPreparingPanel } from "@/components/KubernetesPreparingPanel";
import { pendingKindNames } from "@/lib/kubernetesPending";
import { kubernetesAccessLost } from "@/lib/kubernetesContinuity";
import { resourceIDs } from "@/lib/kubernetesResourcePresentation";
import { useKubernetesCluster, useKubernetesFirstReady, useKubernetesQuery } from "@/lib/useKubernetesCluster";
import { failureWarnings } from "../kubernetesWarningItems";

type Related = {
  resource_id?: string;
  kind: string;
  namespace?: string;
  name: string;
  summary?: Record<string, unknown> | null;
};
export function ResourceRelations({
  resource,
  related,
  onOpen,
}: {
  resource: KubernetesResource;
  related?: Related[] | null;
  onOpen: (resource: KubernetesResource) => void;
}) {
  const mapId = useId();
  const firstReadyAt = useKubernetesFirstReady();
  const { client } = useKubernetesCluster();
  const graph = useKubernetesQuery({
    queryKey: [
      "resource-detail-graph",
      resource.uid,
      resource.kind,
      resource.namespace,
      resource.name,
    ],
    queryFn: ({ signal }) =>
      client.kubernetesRelated({
        resource_id: resource.resource_id,
        namespace: resource.namespace,
        name: resource.name,
      }, signal),
    retry: false,
    refetchInterval: (query) => query.state.data?.sync.state === "syncing" ? 1000 : query.state.data?.sync.pending?.length ? 5000 : false,
  });
  if (kubernetesAccessLost(graph.error)) return <section aria-label="Related resources"><p role="status" className="text-xs text-ink-400">Relationship health is unavailable.</p></section>;
  const nodes = graph.data?.nodes ?? [];
  const namespaceFor = (item: Related) => ["Node", "Namespace"].includes(item.kind) ? "" : item.namespace || resource.namespace || "";
  const nodeFor = (item: Related) => nodes.find((node) => node.kind === item.kind && node.name === item.name && namespaceFor(node) === namespaceFor(item));
  const edges = graph.data?.edges ?? [];
  const current = nodes.find(
    (node) =>
      node.kind === resource.kind &&
      node.name === resource.name &&
      namespaceFor(node) === namespaceFor(resource),
  );
  const ancestorDepth = new Map<string, number>(
    current ? [[current.id, 0]] : [],
  );
  const descendants = new Set(current ? [current.id] : []);
  for (let pass = 0; pass < nodes.length; pass++) {
    for (const edge of edges) {
      if (
        (edge.type === "manages" || edge.type === "scales") &&
        ancestorDepth.has(edge.to) &&
        !ancestorDepth.has(edge.from)
      ) {
        ancestorDepth.set(edge.from, ancestorDepth.get(edge.to)! + 1);
      }
      if (edge.type === "manages" && descendants.has(edge.from))
        descendants.add(edge.to);
    }
  }
  const groupFor = (item: Related) => {
    const node = nodeFor(item);
    if (item.kind === "Node") return "Runs on";
    if (
      (node && (ancestorDepth.get(node.id) ?? 0) > 0) ||
      resource.owners?.some(
        (owner) => owner.kind === item.kind && owner.name === item.name,
      )
    )
      return "Owned by";
    if (node && node.id !== current?.id && descendants.has(node.id))
      return "Owns";
    if (
      edges.some(
        (edge) =>
          edge.from === node?.id &&
          edge.to === current?.id &&
          (edge.type === "exposes" || edge.type === "scales"),
      )
    )
      return "Selected by";
    if (
      edges.some(
        (edge) =>
          edge.from === current?.id &&
          edge.to === node?.id &&
          edge.type === "exposes",
      )
    )
      return "Selects";
    if (
      edges.some(
        (edge) =>
          edge.from === current?.id &&
          edge.to === node?.id &&
          edge.type === "scales",
      )
    )
      return "Scales";
    if (edges.some((edge) => edge.from === current?.id && edge.to === node?.id && (edge.type === "uses" || edge.type === "configures"))) return "Uses";
    return "References";
  };
  const pendingRelatedKinds = graph.isError ? new Set<string>() : pendingKindNames(graph.data?.sync.pending);
  const all = new Map<string, Related>();
  [
    // Graph health confirms projected references; without a graph (failed
    // request) or for kinds the graph has not listed yet, keep them as-is.
    ...(related ?? []).filter((item) => graph.isError || nodeFor(item) || pendingRelatedKinds.has(item.kind)),
    ...(resource.owners ?? []).map((owner) => ({
      ...owner,
      namespace:
        owner.namespace ??
        (resourceIDs[owner.kind] && !["Node", "Namespace"].includes(owner.kind)
          ? resource.namespace
          : undefined),
    })),
    ...(typeof resource.summary?.node === "string"
      ? [{ kind: "Node", name: resource.summary.node }]
      : []),
    ...nodes.filter((node) => node.id !== current?.id),
  ].forEach((node) => {
    const normalized = { ...node, namespace: namespaceFor(node) || undefined };
    const key = `${node.kind}:${namespaceFor(node)}:${node.name}`;
    all.set(key, { ...all.get(key), ...normalized });
  });
  const owners = [...all.values()]
    .filter((item) => groupFor(item) === "Owned by")
    .sort((a, b) => {
      const depth = (item: Related) =>
        ancestorDepth.get(
          nodeFor(item)?.id ?? "",
        ) ?? 0;
      return depth(b) - depth(a);
    });
  const open = (item: Related) =>
    onOpen({
      ...item,
      resource_id: item.resource_id ?? resourceIDs[item.kind] ?? "",
    });
  const statusFor = (item: Related) => {
    const summary = item.summary ?? {};
    if (typeof summary.ready === "number" && Number.isSafeInteger(summary.ready) && summary.ready >= 0 && typeof summary.desired === "number" && Number.isSafeInteger(summary.desired) && summary.desired >= 0) return `${summary.ready} of ${summary.desired} ready`;
    if (typeof summary.endpoints === "number" && Number.isSafeInteger(summary.endpoints) && summary.endpoints >= 0) return `${summary.endpoints} endpoints`;
    const node = nodeFor(item);
    const scale = edges.find((edge) => edge.from === node?.id && edge.type === "scales");
    const target = scale && nodes.find((candidate) => candidate.id === scale.to);
    if (target) return `Scales ${target.kind} ${target.name}`;
    if (item.kind === "Secret") return "content not shown";
    if (item.kind === "ConfigMap") return "Referenced configuration";
    if (item.kind === "PersistentVolumeClaim") return "Referenced volume";
    if (item.kind === "Service") {
      const pods = new Set(edges.filter((edge) => edge.from === node?.id && edge.type === "exposes" && nodes.some((candidate) => candidate.id === edge.to && candidate.kind === "Pod")).map((edge) => edge.to));
      if (pods.size) return `Selects ${pods.size} indexed ${pods.size === 1 ? "Pod" : "Pods"}`;
    }
    return node?.health === "healthy" ? item.kind === "Node" ? "Ready" : "Healthy workload" : node?.health === "degraded" || node?.health === "unhealthy" ? "Workload degraded" : "Status unavailable";
  };
  const top = [...owners, resource];
  const dependencies = [...all.values()].filter((item) => groupFor(item) !== "Owned by");
  // Layout: the ownership chain sits on one row; dependencies form a grid
  // below it. Every dependency edge reaches a shared bus line between the two,
  // and edges from lower grid rows climb through the gutter left of their
  // column so they never pass through another node.
  const nodeWidth = 200;
  const nodeHeight = 44;
  const margin = 16;
  const columnGap = 40;
  const rowGap = 72;
  const chainGap = 64;
  const chainY = 16;
  const chainBottom = chainY + nodeHeight;
  const busY = chainBottom + 28;
  const firstRowY = chainBottom + 76;
  const columns = Math.max(1, Math.min(3, dependencies.length));
  const rows = Math.ceil(dependencies.length / columns);
  const width = Math.max(
    760,
    columnGap + columns * nodeWidth + (columns - 1) * columnGap + margin,
    2 * margin + top.length * nodeWidth + (top.length - 1) * chainGap,
  );
  const topX = (index: number) => margin + index * (width - 2 * margin - nodeWidth) / Math.max(1, top.length - 1);
  const columnStep = columns > 1 ? (width - columnGap - margin - nodeWidth) / (columns - 1) : 0;
  const bottomX = (index: number) => columnGap + index % columns * columnStep;
  const bottomY = (index: number) => firstRowY + Math.floor(index / columns) * (nodeHeight + rowGap);
  // Up to four rows get their own lane per gutter; taller maps share one trunk
  // per gutter (every dependency edge ends at the same root anyway).
  const laneStep = rows <= 4 ? 8 : 0;
  const nameLimit = Math.floor((nodeWidth - 24) / 6.2);
  const mapHeight = dependencies.length ? bottomY(dependencies.length - 1) + nodeHeight + margin : chainBottom + 40;
  const mapNode = (item: Related, x: number, y: number, selected = false) => {
    const health = nodeFor(item)?.health;
    const enabled = !selected && Boolean(item.resource_id || resourceIDs[item.kind]);
    const kind = item.kind === "HorizontalPodAutoscaler" ? "HPA" : item.kind === "PersistentVolumeClaim" ? "PVC" : item.kind;
    return <g key={`${item.kind}:${namespaceFor(item)}:${item.name}`} transform={`translate(${x} ${y})`} role={enabled ? "button" : undefined} tabIndex={enabled ? 0 : undefined} aria-label={enabled ? `Open ${item.kind} ${item.name}` : `${item.kind} ${item.name}`} onClick={enabled ? () => open(item) : undefined} onKeyDown={(event) => { if (enabled && (event.key === "Enter" || event.key === " ")) { event.preventDefault(); open(item); } }} className={`relationship-node ${selected ? "relationship-current" : ""}`} data-relationship-node={nodeFor(item)?.id}>
      <title>{item.kind} {item.name}: {statusFor(item)}</title>
      <rect width={nodeWidth} height={nodeHeight} rx="6" />
      <text x="12" y="16" className="relationship-kind">{selected ? `THIS ${kind.toUpperCase()}` : kind.toUpperCase()}</text>
      <text x="12" y="33" className="relationship-name">{item.name.length > nameLimit ? `${item.name.slice(0, nameLimit - 1)}…` : item.name}</text>
      <circle cx={nodeWidth - 10} cy="12" r="3.5" className={health === "healthy" ? "fill-sev-ok" : health === "degraded" || health === "unhealthy" ? "fill-sev-warning" : "fill-ink-400"} />
    </g>;
  };
  // A background resync after the first publication keeps the graph the reader
  // is on; only a cluster that has never published has nothing to keep.
  if (graph.isPending || ((graph.unpublished || graph.data?.sync.state === "syncing") && !firstReadyAt)) return <section aria-label="Related resources">{graph.isPending ? <div role="status" aria-label="Loading related resources" className="space-y-2 p-4"><div className="sk h-12" /><div className="sk h-20" /></div> : <div className="p-3"><KubernetesPreparingPanel progress={graph.data?.sync.progress} /></div>}</section>;
  return (
    <section aria-label="Related resources" className="resource-related space-y-4">
      <CoverageMarker reasons={failureWarnings(graph.data?.partial_failures).map((warning) => warning.content)} />
      {!firstReadyAt && <KubernetesPendingNote kinds={graph.isError ? undefined : [...pendingKindNames(graph.data?.sync.pending)]} />}
      <div
        className="relationship-map min-w-0 rounded-control border border-ink-700 bg-surface-sunken p-4"
        aria-label="Relationship map"
      >
        <header className="mb-4 flex flex-wrap items-center justify-between gap-2"><h3 className="text-xs font-semibold">Relationship map</h3><span className="flex gap-3 text-2xs text-ink-400"><span><span className="text-sev-ok">●</span> Healthy</span><span><span className="text-sev-warning">●</span> Degraded</span><span>● No status</span></span></header>
        <div className="relationship-canvas"><svg viewBox={`0 0 ${width} ${mapHeight}`} style={{ minWidth: width }} aria-label="Directed resource relationships">
          <defs><marker id={mapId} markerWidth="6" markerHeight="6" refX="5" refY="3" orient="auto"><path d="M0 0L6 3L0 6" className="fill-ink-500" /></marker></defs>
          {top.slice(0, -1).map((item, index) => {
            const next = top[index + 1];
            const edge = edges.find((candidate) => candidate.from === nodeFor(item)?.id && candidate.to === nodeFor(next)?.id && ["manages", "scales"].includes(candidate.type));
            const projectedOwner = next === resource && resource.owners?.some((owner) => owner.kind === item.kind && owner.name === item.name);
            return edge || projectedOwner ? <g key={`chain-${index}`} data-relationship-from={edge?.from} data-relationship-to={edge?.to}><path d={`M${topX(index) + nodeWidth} ${chainY + 22}H${topX(index + 1)}`} markerEnd={`url(#${mapId})`} /><text x={(topX(index) + nodeWidth + topX(index + 1)) / 2} y={chainY + 14} textAnchor="middle">{edge?.type === "scales" ? "scales" : "owns"}</text></g> : null;
          })}
          {dependencies.map((item, index) => {
            const rootId = nodeFor(resource)?.id;
            const nodeId = nodeFor(item)?.id;
            const edge = edges.find((candidate) => candidate.from === rootId && candidate.to === nodeId || candidate.to === rootId && candidate.from === nodeId);
            const group = groupFor(item);
            const rootX = topX(top.length - 1) + nodeWidth / 2;
            const x = bottomX(index) + nodeWidth / 2, y = bottomY(index);
            if (!edge && !(group === "Runs on" && resource.summary?.node === item.name)) return null;
            const inbound = edge?.to === rootId;
            const row = Math.floor(index / columns);
            // First-row nodes rise straight to the bus. Lower rows step into the
            // gap just above themselves, cross to their own lane in the column
            // gutter (deeper rows use lanes further from the column), and climb.
            const lane = bottomX(index) - 12 - (row - 1) * laneStep;
            const jogY = y - 16;
            const route = row === 0
              ? [[x, y], [x, busY], [rootX, busY], [rootX, chainBottom]]
              : [[x, y], [x, jogY], [lane, jogY], [lane, busY], [rootX, busY], [rootX, chainBottom]];
            const points = inbound ? route : [...route].reverse();
            const path = points.map(([px, py], step) => `${step ? "L" : "M"}${px} ${py}`).join("");
            const label = row === 0
              ? { x: x + 6, y: y - 20, anchor: "start" as const }
              : { x: (x + lane) / 2, y: jogY - 5, anchor: "middle" as const };
            return <g key={`branch-${index}`} data-relationship-from={edge?.from} data-relationship-to={edge?.to} data-relationship-node-ref={nodeFor(item)?.id}><path d={path} markerEnd={`url(#${mapId})`} /><text x={label.x} y={label.y} textAnchor={label.anchor}>{group.toLowerCase()}</text></g>;
          })}
          {top.map((item, index) => mapNode(item, topX(index), 16, index === top.length - 1))}
          {dependencies.map((item, index) => mapNode(item, bottomX(index), bottomY(index)))}
        </svg></div>
      </div>
      {[
        "Owned by",
        "Owns",
        "Selected by",
        "Selects",
        "Scales",
        "Uses",
        "References",
        "Runs on",
      ].map((group) => {
        const items = [...all.values()].filter(
          (item) => groupFor(item) === group,
        );
        if (group === "Owned by") items.sort((a, b) => (ancestorDepth.get(nodeFor(a)?.id ?? "") ?? 0) - (ancestorDepth.get(nodeFor(b)?.id ?? "") ?? 0));
        if (!items.length && ["Owns", "Selects", "Scales", "References"].includes(group))
          return null;
        return (
          <section key={group}>
            <h3 className="border-b border-ink-700 pb-2 text-2xs uppercase text-ink-400">
              {group}
            </h3>
            {!items.length && (
              <p className="py-2 text-xs text-ink-500">
                No projected relationships.
              </p>
            )}
            {items.map((item) => (
              <div
                key={`${item.kind}:${item.namespace}:${item.name}`}
                className="flex min-w-0 items-center gap-2 border-b border-ink-700 bg-surface-sunken px-3 py-1.5 text-xs"
              >
                <span className="pill shrink-0" title={item.kind}>{item.kind === "HorizontalPodAutoscaler" ? "HPA" : item.kind === "PersistentVolumeClaim" ? "PVC" : item.kind}</span>
                <span className="min-w-0 flex-1 break-all">
                  {item.name}
                </span>
                <span className="relationship-status"><HealthDot graph={graph.isError ? undefined : graph.data} item={item} dotOnly /><span>{statusFor(item)}</span></span>
                <button
                  type="button"
                  disabled={!item.resource_id && !resourceIDs[item.kind]}
                  className="btn text-link"
                  onClick={() => open(item)}
                >
                  Open<ChevronRight size={12} aria-hidden="true" />
                </button>
              </div>
            ))}
          </section>
        );
      })}
      {graph.isError && (
        <p role="status" className="text-xs text-ink-400">
          Relationship health is unavailable; projected references are shown.
        </p>
      )}
    </section>
  );
}
function HealthDot({
  graph,
  item,
  dotOnly = false,
}: {
  graph?: KubernetesGraph;
  item: Related;
  dotOnly?: boolean;
}) {
  const health = graph?.nodes.find(
    (node) =>
      node.kind === item.kind &&
      node.name === item.name &&
      (node.namespace || "") === (item.namespace || ""),
  )?.health;
  return (
    <span
      className={`inline-flex items-center gap-1 text-2xs ${health === "healthy" ? "text-sev-ok" : health === "degraded" || health === "unhealthy" ? "text-sev-warning" : "text-ink-400"}`}
    >
      <span aria-hidden="true">●</span>
      <span className={dotOnly ? "sr-only" : undefined}>{health || "Unknown"}</span>
    </span>
  );
}

