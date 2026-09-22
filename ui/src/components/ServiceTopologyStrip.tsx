import { useEffect, useState, type ReactNode } from "react";
import {
  AlertCircle,
  ArrowRight,
  CheckCircle2,
  CircleHelp,
  GitBranch,
  LockKeyhole,
  RefreshCw,
  ShieldAlert,
} from "lucide-react";
import {
  type ServiceHealthService,
  type ServiceHealthSnapshot,
  type ServiceTopologyEdge,
} from "@/lib/api";
import { isServiceHealthAccessDenied, useServiceTopologyQuery } from "@/lib/useServiceHealth";
import { topologyErrorCopy } from "@/lib/serviceTopologyPresentation";

const impactLabels: Record<string, string> = {
  degraded: "Degraded",
  critical: "Critical",
  pressure: "Warning",
  creeping: "Worsening",
  unknown: "Unknown",
  nominal: "Normal",
};

function provenanceLabel(source: string) {
  const normalized = source.toLowerCase();
  if (normalized.startsWith("trace_parent_child:")) return "Traces";
  if (normalized.includes("operator") || normalized.includes("config") || normalized.includes("static")) {
    return "Configured";
  }
  if (normalized.includes("learned")) return "Learned";
  if (normalized.includes("extension")) return "Extension";
  return "Other";
}

function relationshipLabel(edge: ServiceTopologyEdge) {
  const source = provenanceLabel(edge.source);
  return source === "Configured"
    ? `${edge.service} depends on ${edge.depends_on}`
    : `${edge.service} depends on ${edge.depends_on}; ${source.toLowerCase()} source`;
}

function NodeButton({
  name,
  health,
  onSelect,
  onUnavailable,
}: {
  name: string;
  health?: ServiceHealthService;
  onSelect: (service: string) => void;
  onUnavailable: (service: string) => void;
}) {
  const affected = health && ["degraded", "critical", "pressure", "creeping"].includes(health.severity);
  const Icon = affected ? ShieldAlert : health?.severity === "nominal" ? CheckCircle2 : CircleHelp;
  const status = health ? (impactLabels[health.severity] ?? "Unknown") : "No health snapshot";
  return (
    <button
      type="button"
      className="topology-node"
      data-impact={health?.severity ?? "unmatched"}
      aria-label={health ? `Inspect ${name}, health ${status}` : `${name}, no health snapshot available`}
      onClick={() => health ? onSelect(name) : onUnavailable(name)}
    >
      <span className="topology-node-name">{name}</span>
      <span className="topology-node-status"><Icon size={12} aria-hidden />{status}</span>
    </button>
  );
}

function emptyCopy(availability: string) {
  if (availability === "not_configured") return "No service dependencies are configured.";
  if (availability === "no_data") return "No service dependency data is available.";
  if (availability === "restricted") return "Service topology is unavailable for this session.";
  if (availability === "error") return "Service topology is currently unavailable.";
  return "No service dependencies were returned.";
}

function edgeKey(edge: ServiceTopologyEdge) {
  return `${edge.service}\u0000${edge.depends_on}\u0000${edge.source}`;
}

function omittedCopy(nodes: number, edges: number) {
  return [
    nodes > 0 ? `${nodes} ${nodes === 1 ? "service" : "services"}` : "",
    edges > 0 ? `${edges} ${edges === 1 ? "relationship" : "relationships"}` : "",
  ].filter(Boolean).join(" and ");
}

export function ServiceTopologyStrip({
  snapshot,
  onSelectService,
}: {
  snapshot?: ServiceHealthSnapshot;
  onSelectService: (service: string) => void;
}) {
  const query = useServiceTopologyQuery();
  const [unavailableActivation, setUnavailableActivation] = useState<{ service: string; sequence: number } | null>(null);
  const [announcement, setAnnouncement] = useState("");
  const healthByName = new Map(snapshot?.services.map((service) => [service.service, service]) ?? []);

  useEffect(() => {
    if (!unavailableActivation) return;
    setAnnouncement("");
    const announceTimer = window.setTimeout(() => {
      setAnnouncement(`${unavailableActivation.service} is not represented in the current health snapshot.`);
    }, 0);
    const clearTimer = window.setTimeout(() => setAnnouncement(""), 5_000);
    return () => {
      window.clearTimeout(announceTimer);
      window.clearTimeout(clearTimer);
    };
  }, [unavailableActivation]);

  const announceUnavailable = (service: string) => {
    setUnavailableActivation((current) => ({ service, sequence: (current?.sequence ?? 0) + 1 }));
  };

  let body: ReactNode;
  const accessDenied = isServiceHealthAccessDenied(query.error);
  if (accessDenied) {
    body = <p className="topology-state" role="alert"><LockKeyhole size={13} aria-hidden />{topologyErrorCopy(query.error)}</p>;
  } else if (query.isPending) {
    body = <p className="topology-state"><RefreshCw size={13} className="animate-spin" aria-hidden />Loading dependencies...</p>;
  } else if (query.isError && !query.data) {
    body = <div className="flex flex-wrap items-center gap-2">
      <p className="topology-state"><AlertCircle size={13} aria-hidden />{topologyErrorCopy(query.error)}</p>
      <button type="button" className="btn h-7 px-2 text-2xs" disabled={query.isRefetching} onClick={() => query.refetch()}>Retry</button>
    </div>;
  } else {
    const topology = query.data;
    const nodes = [...topology.nodes].sort((left, right) => left.service.localeCompare(right.service));
    const nodeNames = new Set(nodes.map((node) => node.service));
    const edges = [...topology.edges]
      .filter((edge) => nodeNames.has(edge.service) && nodeNames.has(edge.depends_on))
      .sort((left, right) => edgeKey(left).localeCompare(edgeKey(right)));
    const clientOmittedEdges = topology.edges.length - edges.length;
    const connected = new Set(edges.flatMap((edge) => [edge.service, edge.depends_on]));
    const isolated = nodes.filter((node) => !connected.has(node.service));
    const hasGraph = nodes.length > 0;
    const provenance = [...new Set(
      [...topology.provenance, ...edges.map((edge) => edge.source)].map(provenanceLabel),
    )].filter((label) => label !== "Configured").sort();
    const omittedNodes = topology.omitted_nodes ?? 0;
    const omittedEdges = (topology.omitted_edges ?? 0) + clientOmittedEdges;

    const refreshWarning = query.isError && <div className="mb-2 flex flex-wrap items-center gap-2" role="status">
      <p className="topology-state"><AlertCircle size={13} aria-hidden />Showing saved topology. Refresh failed.</p>
      <button type="button" className="btn h-7 px-2 text-2xs" disabled={query.isRefetching} onClick={() => query.refetch()}>Retry</button>
    </div>;

    body = hasGraph ? <>
      {refreshWarning}
      <div className="mb-2 flex flex-wrap items-center gap-2 text-2xs text-ink-400">
        {provenance.map((label) => <span key={label} className="topology-provenance">{label}</span>)}
        {topology.availability === "partial" && <span className="inline-flex items-center gap-1 text-sev-warn"><AlertCircle size={12} aria-hidden />Partial coverage</span>}
      </div>
      <div className="topology-scroll" data-testid="service-topology-strip" tabIndex={0} aria-label="Service dependency relationships; scroll horizontally for more">
        <div className="topology-track" role="list">
          {edges.map((edge) => <div key={edgeKey(edge)} className="topology-relationship" role="listitem" aria-label={relationshipLabel(edge)}>
            <NodeButton name={edge.service} health={healthByName.get(edge.service)} onSelect={onSelectService} onUnavailable={announceUnavailable} />
            <span className="topology-edge" aria-hidden><ArrowRight size={17} />{provenanceLabel(edge.source) !== "Configured" && <span>{provenanceLabel(edge.source)}</span>}</span>
            <NodeButton name={edge.depends_on} health={healthByName.get(edge.depends_on)} onSelect={onSelectService} onUnavailable={announceUnavailable} />
          </div>)}
          {isolated.map((node) => <div key={node.service} role="listitem">
            <NodeButton name={node.service} health={healthByName.get(node.service)} onSelect={onSelectService} onUnavailable={announceUnavailable} />
          </div>)}
        </div>
      </div>
      {(omittedNodes > 0 || omittedEdges > 0) && <p className="mt-2 text-2xs text-ink-400" data-testid="service-topology-omitted">
        {omittedCopy(omittedNodes, omittedEdges)} omitted.
      </p>}
    </> : <>
      {refreshWarning}
      <p className="topology-state">{emptyCopy(topology.availability)}</p>
      {(omittedNodes > 0 || omittedEdges > 0) && <p className="mt-2 text-2xs text-ink-400" data-testid="service-topology-omitted">{omittedCopy(omittedNodes, omittedEdges)} omitted.</p>}
    </>;
  }

  return (
    <section className="topology-section" aria-labelledby="service-topology-title">
      <div className="flex flex-wrap items-center gap-2">
        <h3 id="service-topology-title" className="inline-flex items-center gap-2 text-sm font-semibold text-ink-100"><GitBranch size={15} className="text-ink-400" aria-hidden />Service dependencies</h3>
      </div>
      <div className="mt-2">{body}</div>
      {!accessDenied && <p className="sr-only" role="status" aria-live="polite" data-testid="service-topology-announcement">{announcement}</p>}
    </section>
  );
}