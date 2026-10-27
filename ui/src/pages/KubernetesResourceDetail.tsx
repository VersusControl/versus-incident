import { useId, useMemo, useState, type ReactNode } from "react";
import { Box, CheckCircle2, ChevronRight, Copy, Download, Search } from "lucide-react";
import type {
  KubernetesChange,
  KubernetesCrashEvent,
  KubernetesGraph,
  KubernetesResource,
  KubernetesWorkload,
} from "@/lib/api";
import { projectedResourceYaml, yamlKeyEnd } from "@/lib/kubernetesProjection";
import { CoverageMarker } from "@/components/CoverageMarker";
import { KubernetesPendingNote } from "@/components/KubernetesPendingNote";
import { KubernetesPreparingPanel } from "@/components/KubernetesPreparingPanel";
import { pendingKindNames } from "@/lib/kubernetesPending";
import { failureWarnings } from "./kubernetesWarningItems";
import { CursorPagination } from "@/components/Pagination";
import { useCursorPagination } from "@/lib/pagination";
import { isKnownKubernetesTime } from "@/lib/kubernetesTime";
import {
  useKubernetesCluster,
  useKubernetesFirstReady,
  useKubernetesQuery,
} from "@/lib/useKubernetesCluster";
import {
  detailText,
  downloadText,
  resourceContainerStates,
  resourceStart,
  resourceRestarts,
} from "@/lib/kubernetesResourceDetail";

function elapsed(at: unknown) {
  if (!isKnownKubernetesTime(at)) return "Unavailable";
  const minutes = Math.max(
    0,
    Math.floor((Date.now() - Date.parse(at)) / 60000),
  );
  return minutes < 60
    ? `${minutes}m`
    : minutes < 1440
      ? `${Math.floor(minutes / 60)}h ${minutes % 60}m`
      : `${Math.floor(minutes / 1440)}d ${Math.floor((minutes % 1440) / 60)}h`;
}
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

function eventTime(event: KubernetesResource, first = false): string {
  const summary = event.summary ?? {};
  const time = detailText(
    first
      ? (summary.first_timestamp ?? summary.firstTimestamp)
      : (summary.last_timestamp ?? summary.lastTimestamp),
    "",
  );
  return isKnownKubernetesTime(time) ? time : "";
}
function clock(time: string) {
  return isKnownKubernetesTime(time)
    ? new Date(time).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false })
    : "unavailable";
}
function eventBoundary(events: KubernetesResource[], first = false) {
  const times = events.map((event) => eventTime(event, first)).filter(Boolean).sort((a, b) => Date.parse(a) - Date.parse(b));
  return (first ? times[0] : times.at(-1)) ?? "";
}
function eventOccurrences(event: KubernetesResource): number {
  const count = Number(event.summary?.count ?? 1);
  return Number.isSafeInteger(count) && count > 0 ? count : 1;
}
export function ResourceEvents({ events }: { events: KubernetesResource[] }) {
  const [kind, setKind] = useState("All");
  const [search, setSearch] = useState("");
  const now = Date.now();
  const warning = (event: KubernetesResource) =>
    event.summary?.type === "Warning";
  const counts = {
    All: events.reduce((sum, event) => sum + eventOccurrences(event), 0),
    Warnings: events
      .filter(warning)
      .reduce((sum, event) => sum + eventOccurrences(event), 0),
    Normal: events
      .filter((event) => !warning(event))
      .reduce((sum, event) => sum + eventOccurrences(event), 0),
  };
  const visible = events.filter(
    (event) =>
      (kind === "All" || (kind === "Warnings") === warning(event)) &&
      `${detailText(event.summary?.reason, event.name)} ${detailText(event.summary?.message, "")}`
        .toLowerCase()
        .includes(search.toLowerCase()),
  );
  const groups = new Map<string, KubernetesResource[]>();
  visible.forEach((event) => {
    const key = `${warning(event) ? "Warning" : "Normal"}:${detailText(event.summary?.reason, event.name)}`;
    groups.set(key, [...(groups.get(key) ?? []), event]);
  });
  const bins = Array.from({ length: 12 }, (_, index) => {
    const members = visible.filter((event) => {
      const at = Date.parse(eventTime(event));
      return (
        at >= now - (12 - index) * 300000 && at < now - (11 - index) * 300000
      );
    });
    return {
      normal: members
        .filter((event) => !warning(event))
        .reduce((sum, event) => sum + eventOccurrences(event), 0),
      warnings: members
        .filter(warning)
        .reduce((sum, event) => sum + eventOccurrences(event), 0),
    };
  });
  const maximum = Math.max(1, ...bins.map((bin) => bin.normal + bin.warnings));
  return (
    <section aria-label="Object-scoped events" className="resource-events space-y-3">
      <p className="sr-only">
        Counts are occurrences; histogram buckets use each Event’s last observed
        time.
      </p>
      <div className="flex flex-wrap items-center gap-2">
        {Object.entries(counts).map(([label, count]) => (
          <button
            type="button"
            key={label}
            className={`btn ${label === "Warnings" ? "event-warning-filter" : ""} ${label === kind ? "bg-accent/10 text-link" : ""}`}
            aria-pressed={label === kind}
            onClick={() => setKind(label)}
          >
            {label} <span className="text-ink-400">{count}</span>
          </button>
        ))}
        <label className="flex min-w-0 flex-1 items-center gap-2">
          <Search size={14} aria-hidden="true" />
          <input
            aria-label="Search events"
            placeholder="Search events"
            className="input w-full"
            value={search}
            onChange={(event) => setSearch(event.target.value)}
          />
        </label>
      </div>
      <figure className="event-histogram rounded-control border border-ink-700 bg-surface-sunken p-3">
        <figcaption className="mb-2 flex items-center justify-between text-xs text-ink-300">
          <span>Events per 5 minutes, last hour</span><span className="flex gap-3 text-2xs"><span><span className="text-sev-critical">■</span> Warnings</span><span><span className="text-ink-500">■</span> Normal</span></span>
        </figcaption>
        <div
          className="flex h-20 items-end gap-2"
          role="img"
          aria-label="Event histogram: warnings red, normal gray"
        >
          {bins.map((bin, index) => (
            <div
              key={index}
              className="flex h-full flex-1 flex-col justify-end"
              title={`${bin.warnings} warnings, ${bin.normal} normal`}
            >
              <div
                className="bg-sev-critical"
                style={{ height: `${(bin.warnings / maximum) * 100}%` }}
              />
              <div
                className="bg-ink-500"
                style={{ height: `${(bin.normal / maximum) * 100}%` }}
              />
            </div>
          ))}
        </div>
        <div className="mt-2 flex justify-between text-2xs text-ink-400">
          {[60, 45, 30, 15, 0].map((minutes) => <span key={minutes}>{minutes === 0 ? "now" : clock(new Date(now - minutes * 60000).toISOString()).slice(0, 5)}</span>)}
        </div>
      </figure>
      {!visible.length && (
        <p role="status" className="py-8 text-center text-xs text-ink-400">
          {events.length
            ? "No events match this filter."
            : "No object-scoped events were returned."}
        </p>
      )}
      {[...groups].map(([key, items]) => (
        <article
          key={key}
          className={`rounded-control border border-ink-700 border-l-[3px] bg-surface-sunken p-3 ${warning(items[0]) ? "border-l-sev-warning" : "border-l-ink-500"}`}
        >
          <div className="flex items-center gap-2">
            <h3 className="text-xs font-semibold">
              {detailText(items[0].summary?.reason, items[0].name)}
            </h3>
            <span className="pill">
              ×{items.reduce((sum, item) => sum + eventOccurrences(item), 0)}
            </span>
            <span className="ml-auto flex flex-wrap justify-end gap-x-4 text-2xs text-ink-400">
              <span>first <time dateTime={eventBoundary(items, true) || undefined} title={eventBoundary(items, true)}>{clock(eventBoundary(items, true))}</time></span>
              <span>last <time dateTime={eventBoundary(items) || undefined} title={eventBoundary(items)}>{clock(eventBoundary(items))}</time></span>
            </span>
          </div>
          {items.map((event) => (
            <div className="mt-2 space-y-1" key={event.uid ?? event.name}>
              <p
                className="truncate text-xs text-ink-200"
                title={detailText(event.summary?.message)}
              >
                {detailText(
                  event.summary?.message,
                  "Event message unavailable",
                )}
              </p>
              <div className="flex flex-wrap gap-x-4 gap-y-1 text-2xs text-ink-400">
                <span>
                  {detailText(event.summary?.source_component, "unknown")}{" "}
                  {detailText(event.summary?.source_host, "")}
                </span>
              </div>
            </div>
          ))}
        </article>
      ))}
    </section>
  );
}

export function ResourceYaml({ resource }: { resource: KubernetesResource }) {
  const yaml = useMemo(() => projectedResourceYaml(resource), [resource]);
  const lines = yaml.trimEnd().split("\n");
  const blocks = lines.flatMap((line, index) => {
    if (!/^\s*[\w.-]+:\s*$/.test(line) && !/^\s*-\s+name:\s*.+$/.test(line)) return [];
    const indent = line.search(/\S/);
    let end = index + 1;
    while (end < lines.length && lines[end].search(/\S/) > indent) end++;
    return end > index + 1
      ? [{ start: index, end, name: line.trim().replace(/:$/, ""), indent }]
      : [];
  });
  const containerBlock = blocks.find((block) => block.name === "containers");
  const containerEntries = blocks.filter((block) => containerBlock && block.indent === containerBlock.indent + 2 && block.start > containerBlock.start && block.end <= containerBlock.end);
  const [folded, setFolded] = useState<Set<number>>(() => new Set(blocks.filter((block) => ["ownerReferences", "status", "observations"].includes(block.name) || containerEntries.slice(1).some((entry) => entry.start === block.start)).map((block) => block.start)));
  const [activeLine, setActiveLine] = useState(blocks.find((block) => block.name === "spec")?.start ?? 0);
  const [find, setFind] = useState("");
  const [match, setMatch] = useState(0);
  const [status, setStatus] = useState("");
  const matches = find
    ? lines.flatMap((line, index) =>
        line.toLowerCase().includes(find.toLowerCase()) ? [index] : [],
      )
    : [];
  const goTo = (line: number) => {
    setActiveLine(line);
    setFolded(
      (current) =>
        new Set(
          [...current].filter(
            (start) =>
              !blocks.some(
                (block) =>
                  block.start === start && line > start && line < block.end,
              ),
          ),
        ),
    );
    requestAnimationFrame(() =>
      document
        .getElementById(`resource-yaml-${line}`)
        ?.scrollIntoView?.({ block: "center" }),
    );
  };
  return (
    <section aria-label="Projected YAML" className="resource-yaml space-y-3">
      <div className="yaml-toolbar flex flex-wrap items-center gap-2">
        <span className="pill">Projected, read-only</span>
        <div className="yaml-find flex items-center gap-1"><Search size={13} aria-hidden="true" />
          <input className="input min-w-0 flex-1" aria-label="Find in YAML" placeholder="Find in YAML" value={find} onChange={(event) => { setFind(event.target.value); setMatch(0); }} onKeyDown={(event) => { if (event.key === "Enter" && matches.length) { goTo(matches[match % matches.length]); setMatch((value) => value + 1); } }} />
          <span aria-live="polite" className="yaml-find-count">{find ? `${matches.length}` : ""}</span>
          <button className="btn" type="button" aria-label="Next match" title="Next match" disabled={!matches.length} onClick={() => { goTo(matches[match % matches.length]); setMatch((value) => value + 1); }}>↓</button>
        </div>
        <button
          type="button"
          className="btn"
          onClick={() => setFolded(new Set(blocks.map((block) => block.start)))}
        >
          Fold all
        </button>
        <button
          type="button"
          className="btn"
          onClick={() => setFolded(new Set())}
        >
          Unfold all
        </button>
        <button
          type="button"
          className="btn"
          onClick={() =>
            void navigator.clipboard.writeText(yaml).then(
              () => setStatus("YAML copied."),
              () => setStatus("Clipboard unavailable."),
            )
          }
        >
          <Copy size={13} />
          Copy
        </button>
        <button
          type="button"
          className="btn"
          onClick={() =>
            downloadText(yaml, `${resource.name}.yaml`, "text/yaml")
          }
        >
          <Download size={13} />
          Download
        </button>
      </div>
      <div className="yaml-workspace flex min-w-0 flex-col gap-3 sm:flex-row">
        <div className="yaml-editor min-w-0 flex-1 overflow-auto rounded-control border border-ink-700 bg-ink-950 py-2 font-mono text-xs leading-[19px]" onScroll={(event) => { const top = event.currentTarget.getBoundingClientRect().top; const first = Array.from(event.currentTarget.querySelectorAll<HTMLElement>("[data-line]")).find((row) => row.getBoundingClientRect().bottom >= top); if (first) setActiveLine(Number(first.dataset.line)); }}>
          {lines.map((line, index) => {
            if (
              blocks.some(
                (block) =>
                  folded.has(block.start) &&
                  index > block.start &&
                  index < block.end,
              )
            )
              return null;
            const block = blocks.find((item) => item.start === index);
            const colon = yamlKeyEnd(line);
            return (
              <div
                id={`resource-yaml-${index}`}
                key={index}
                data-line={index}
                onFocus={() => setActiveLine(index)}
                className={`flex min-w-max ${matches.includes(index) ? "bg-accent/10" : ""}`}
              >
                <span
                  className="inline-block w-10 shrink-0 pr-2 text-right text-ink-500"
                  aria-hidden="true"
                >
                  {index + 1}
                </span>
                {folded.has(index) && block ? <button className="yaml-fold-pill" type="button" aria-label={`Unfold ${block.name} at line ${index + 1}`} aria-expanded={false} onClick={() => setFolded((current) => new Set([...current].filter((start) => start !== index)))}>▸ {block.name} ({block.end - block.start - 1} lines)</button> : <><span className="inline-block w-5 shrink-0">
                  {block && (
                    <button
                      type="button"
                      aria-label={`${folded.has(index) ? "Unfold" : "Fold"} ${block.name} at line ${index + 1}`}
                      aria-expanded={!folded.has(index)}
                      onClick={() =>
                        setFolded((current) => {
                          const next = new Set(current);
                          if (next.has(index)) next.delete(index);
                          else next.add(index);
                          return next;
                        })
                      }
                    >
                      {folded.has(index) ? "▸" : "▾"}
                    </button>
                  )}
                </span>
                <code className="whitespace-pre pr-3">
                  {colon >= 0 ? (
                    <>
                      <span className="text-link">
                        {line.slice(0, colon + 1)}
                      </span>
                      <span className={line.slice(colon + 1).includes("‹redacted›") ? "yaml-redacted" : /^\s*-?\d+(?:\.\d+)?\s*$/.test(line.slice(colon + 1)) ? "text-sev-warning" : "text-sev-ok"}>
                        {line.slice(colon + 1)}
                      </span>
                    </>
                  ) : (
                    line
                  )}
                </code>
                </>}
              </div>
            );
          })}
        </div>
        <nav aria-label="YAML outline" className="h-fit shrink-0 rounded-control border border-ink-700 bg-surface-sunken p-3 text-xs sm:w-32">
          <h3 className="mb-2 text-2xs uppercase text-ink-400">Outline</h3>
          {blocks
            .filter((block, index, all) => (block.indent === 0 || block.name === "containers") && all.findIndex((candidate) => candidate.name === block.name) === index)
            .map((block) => (
              <button
                key={block.start}
                type="button"
                className="yaml-outline-item block w-full py-1 text-left"
                aria-current={activeLine >= block.start && activeLine < block.end && !blocks.some((child) => child.name === "containers" && child.start > block.start && activeLine >= child.start && activeLine < child.end) ? "location" : undefined}
                onClick={() => goTo(block.start)}
              >
                {block.name}{" "}
                <span className="text-ink-400">{block.start + 1}</span>
              </button>
            ))}
        </nav>
      </div>
      <p role="status" className="text-2xs text-ink-400">
        {status}
      </p>
    </section>
  );
}

const resourceIDs: Record<string, string> = {
  Pod: "core~v1~pods",
  Node: "core~v1~nodes",
  Namespace: "core~v1~namespaces",
  Service: "core~v1~services",
  Secret: "core~v1~secrets",
  ConfigMap: "core~v1~configmaps",
  PersistentVolumeClaim: "core~v1~persistentvolumeclaims",
  Deployment: "apps~v1~deployments",
  ReplicaSet: "apps~v1~replicasets",
  StatefulSet: "apps~v1~statefulsets",
  DaemonSet: "apps~v1~daemonsets",
  Job: "batch~v1~jobs",
  CronJob: "batch~v1~cronjobs",
  HorizontalPodAutoscaler: "autoscaling~v2~horizontalpodautoscalers",
};
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
  const nodes = graph.isError ? [] : (graph.data?.nodes ?? []);
  const namespaceFor = (item: Related) => ["Node", "Namespace"].includes(item.kind) ? "" : item.namespace || resource.namespace || "";
  const nodeFor = (item: Related) => nodes.find((node) => node.kind === item.kind && node.name === item.name && namespaceFor(node) === namespaceFor(item));
  const edges = graph.isError ? [] : graph.data?.edges ?? [];
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
  if (graph.isPending || (graph.data?.sync.state === "syncing" && !firstReadyAt)) return <section aria-label="Related resources">{graph.isPending ? <div role="status" aria-label="Loading related resources" className="space-y-2 p-4"><div className="sk h-12" /><div className="sk h-20" /></div> : <div className="p-3"><KubernetesPreparingPanel progress={graph.data?.sync.progress} /></div>}</section>;
  return (
    <section aria-label="Related resources" className="resource-related space-y-4">
      <CoverageMarker reasons={failureWarnings(graph.data?.partial_failures).map((warning) => warning.content)} />
      <KubernetesPendingNote kinds={graph.isError ? undefined : [...pendingKindNames(graph.data?.sync.pending)]} />
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

export function ResourceTimeline({
  resource,
  onCrash,
}: {
  resource: KubernetesResource;
  onCrash: (crash: KubernetesCrashEvent) => void;
}) {
  const { client, key } = useKubernetesCluster();
  const [hours, setHours] = useState(6);
  const [now] = useState(Date.now);
  const [generation, setGeneration] = useState(0);
  const paging = useCursorPagination(JSON.stringify([resource.uid, resource.kind, resource.namespace, resource.name, hours, generation]));
  const timeline = useKubernetesQuery({
    queryKey: [
      "resource-detail-timeline",
      resource.uid,
      resource.kind,
      resource.namespace,
      resource.name,
      hours,
      generation,
      paging.cursor,
    ],
    queryFn: ({ signal }) =>
      client.kubernetesTimeline({
        namespace: resource.namespace,
        resource_uid: resource.uid,
        since: new Date(now - hours * 3600000).toISOString(),
        until: new Date(now).toISOString(),
        limit: 100,
        cursor: paging.cursor,
      }, signal),
    placeholderData: (previous, query) => JSON.stringify(query?.queryKey.slice(0, -1)) === JSON.stringify(key("resource-detail-timeline", resource.uid, resource.kind, resource.namespace, resource.name, hours, generation)) ? previous : undefined,
    enabled: Boolean(resource.uid),
    retry: false,
    refetchInterval: (query) => query.state.data?.sync.state === "syncing" ? 1000 : false,
  });
  const data = timeline.isError ? undefined : timeline.data;
  const history = (data?.items ?? []).map<{ id: string; at: string; type: string; fields?: KubernetesChange["fields"]; crash?: KubernetesCrashEvent }>((entry) =>
    entry.type === "change" ? { id: entry.id, at: entry.at, type: entry.change.type, fields: entry.change.fields }
      : entry.type === "crash" ? { id: entry.id, at: entry.at, type: "crash", crash: entry.crash }
        : { id: entry.id, at: entry.at, type: "break" },
  );
  const coverageReasons = [
    ...failureWarnings(data?.partial_failures).map((warning) => warning.content),
    ...(data?.gaps?.length ? ["History capture does not cover the whole range"] : []),
    ...(["stale", "error", "partial"].includes(data?.sync.state ?? "") ? ["Some history could not be read"] : []),
    ...(!resource.uid ? ["Resource identity could not be read"] : []),
    ...(timeline.isError ? ["History could not be read"] : []),
  ];
  const syncing = timeline.isPending && timeline.isFetching || data?.sync.state === "syncing";
  return (
    <section aria-label="Resource change history" className="resource-history space-y-3">
      <div className="flex items-center gap-2">
        <div className="history-range" role="group" aria-label="History range">
        {[1, 6, 24].map((value) => (
          <button
            type="button"
            className={`btn ${value === hours ? "bg-accent-subtle text-link" : ""}`}
            key={value}
            aria-pressed={value === hours}
            onClick={() => setHours(value)}
          >
            {value}h
          </button>
        ))}
        </div>
        <CoverageMarker reasons={coverageReasons} />
        <span className="sr-only">
          {resource.name}
        </span>
      </div>
      {history.length > 0 && <><div className="flex flex-wrap justify-end gap-3 text-2xs text-ink-400" aria-label="History legend"><span><span className="text-link">●</span> change</span><span><span className="text-sev-critical">│</span> break</span><span><span className="text-sev-critical">×</span> crash</span></div>
      <div
        className="relative h-[86px] rounded-control border border-ink-700 bg-surface-sunken px-4"
        aria-label="Resource timeline lane"
      >
        <span className="absolute left-4 top-3 text-2xs uppercase tracking-wide text-ink-400">This {resource.kind}</span>
        <div className="absolute inset-x-4 top-12 border-t border-ink-600" />
        {history.map((item) => (
          <button
            key={item.id}
            type="button"
            aria-label={`${item.type} at ${item.at}`}
            title={`${item.type} at ${item.at}`}
            className={`absolute top-10 flex size-4 items-center justify-center ${item.crash ? "text-sev-critical" : item.type === "break" ? "text-sev-critical" : "text-link"}`}
            style={{
              left: `${Math.max(2, Math.min(96, (100 * (Date.parse(item.at) - (now - hours * 3600000))) / (hours * 3600000)))}%`,
            }}
            onClick={() =>
              document
                .getElementById(`resource-history-${item.id}`)
                ?.scrollIntoView?.({ block: "center" })
            }
          >{item.crash ? "×" : item.type === "break" ? "│" : "●"}</button>
        ))}
        <div className="absolute inset-x-4 bottom-2 flex justify-between text-2xs text-ink-400">{[0, 1, 2, 3].map((tick) => <span key={tick}>{tick === 3 ? "now" : new Date(now - hours * 3600000 * (1 - tick / 3)).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}</span>)}</div>
      </div></>}
      {syncing && (
        <p role="status" className="text-xs text-ink-400">
          Loading resource history.
        </p>
      )}
      <ol className="space-y-3">
        {history.map((item) => (
          <li
            id={`resource-history-${item.id}`}
            className="space-y-2 rounded-control border border-ink-700 bg-surface-sunken p-3"
            key={item.id}
          >
            <div className="flex flex-wrap items-center gap-2">
              <span
                className={`pill ${item.crash ? "text-sev-critical" : "pill-accent"}`}
              >
                {item.crash ? `${item.crash.type === "termination" ? "Terminated" : "Observed"}: ${item.crash.reason || item.crash.waiting_reason || "Container stopped"}` : item.fields?.some((field) => /(?:^|[./])image$/.test(field.path)) ? "Image changed" : item.type.replaceAll("_", " ")}
              </span>
              <time dateTime={item.at} title={new Date(item.at).toLocaleString()} className="ml-auto text-2xs text-ink-400">
                {clock(item.at)}
              </time>
            </div>
            {item.fields?.map((field) => (
              <p className="break-words font-mono text-2xs" key={field.path}>
                {field.path}{" "}
                <span className="history-diff-from">{field.from || "∅"}</span> →{" "}
                <span className="history-diff-to">{field.to || "∅"}</span>
              </p>
            ))}
            {item.crash && (
              <>
                <p className="text-xs text-ink-300">
                  Container {item.crash.container}, exit {item.crash.exit_code}
                </p>
                <button
                  type="button"
                  className="btn text-link"
                  onClick={() => onCrash(item.crash!)}
                >
                  View logs before crash
                </button>
              </>
            )}
          </li>
        ))}
      </ol>
      {!history.length && !timeline.isFetching && !syncing && (
        <p className="py-4 text-xs text-ink-400">
          No changes or crashes in this range.
        </p>
      )}
      {data && data.total > 0 && <p className="text-2xs text-ink-400">{data.total} events and {data.crash_count} crashes</p>}
      {timeline.isError && <button type="button" className="btn text-link" onClick={() => { paging.resetPage(); setGeneration((value) => value + 1); }}>{paging.cursor ? "Restart history" : "Retry history"}</button>}
      <CursorPagination state={paging} busy={timeline.isFetching} next={data?.next} />
    </section>
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
