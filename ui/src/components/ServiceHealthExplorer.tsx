import { useCallback, useState } from "react";
import { Link } from "react-router-dom";
import { ArrowUpRight, CheckCircle2, CircleHelp, LayoutGrid, List, Search, ShieldAlert } from "lucide-react";
import type { ServiceHealthService, ServiceHealthSnapshot } from "@/lib/api";
import { fmtAbs } from "@/lib/format";
import { PeekField, PeekPanel } from "@/components/PeekPanel";
import { healthDataLabel, SERVICE_HEALTH_STATE_COPY } from "@/lib/serviceHealthPresentation";
import "./ServiceHeatmap.css";

type EvidenceMode = "combined" | "logs" | "incidents";

const impactOrder = ["degraded", "critical", "pressure", "creeping", "unknown", "nominal"];
const impactLabels: Record<string, string> = {
  degraded: "Degraded", critical: "Critical", pressure: "Warning",
  creeping: "Worsening", unknown: "Unknown", nominal: "Normal",
};

function affected(service: ServiceHealthService) {
  return ["degraded", "critical", "pressure", "creeping"].includes(service.severity);
}

function measured(service: ServiceHealthService) {
  const state = service.availability.logs?.state;
  return state === "ready" || (["partial", "stale", "error"].includes(state ?? "") &&
    (service.logs.matched_logs > 0 || validTime(service.logs.latest_observation)));
}

function validTime(value?: string) {
  return Boolean(value && Number.isFinite(Date.parse(value)) && new Date(value).getUTCFullYear() > 1970);
}

function logValue(service: ServiceHealthService, value: number) {
  if (!measured(service)) return "Not measured";
  return `${service.logs.estimated ? "~" : ""}${value.toLocaleString()}`;
}

function impactClass(service: ServiceHealthService) {
  if (["critical", "degraded"].includes(service.severity)) return "border-l-sev-critical text-sev-critical";
  if (affected(service)) return "border-l-sev-warn text-sev-warn";
  if (service.severity === "nominal") return "border-l-sev-ok text-sev-ok";
  return "border-l-ink-400 text-ink-300";
}

function Impact({ service }: { service: ServiceHealthService }) {
  const Icon = affected(service) ? ShieldAlert : service.severity === "nominal" ? CheckCircle2 : CircleHelp;
  return <span className={`inline-flex items-center gap-1.5 text-xs font-medium ${impactClass(service)}`}>
    <Icon size={13} aria-hidden />{impactLabels[service.severity] ?? "Unknown"}
  </span>;
}

function evidenceSummary(service: ServiceHealthService, mode: EvidenceMode) {
  const incidents = service.active_incidents == null ? "Not assessed" : service.active_incidents.toLocaleString();
  if (mode === "incidents") return [["Active incidents", incidents]];
  if (mode === "logs") return [
    ["Log events", logValue(service, service.logs.matched_logs)],
    ["Log spikes", logValue(service, service.logs.spiking_patterns)],
  ];
  return [["Log events", logValue(service, service.logs.matched_logs)], ["Active incidents", incidents]];
}

function EvidenceValues({ service, mode }: { service: ServiceHealthService; mode: EvidenceMode }) {
  return <dl className="grid grid-cols-2 gap-3 text-left">
    {evidenceSummary(service, mode).map(([label, value]) => <div key={label} className="min-w-0">
      <dt className="text-2xs text-ink-400">{label}</dt>
      <dd className="mt-1 break-words text-sm font-semibold tabular-nums text-ink-100">{value}</dd>
    </div>)}
  </dl>;
}

function coverageLabel(service: ServiceHealthService) {
  const state = service.availability.logs?.state ?? "unsupported";
  return `Logs: ${SERVICE_HEALTH_STATE_COPY[state].label}${measured(service) && service.logs.estimated ? " · Estimated" : ""}`;
}

export function ServiceHealthExplorer({ snapshot }: { snapshot: ServiceHealthSnapshot }) {
  const [mode, setMode] = useState<EvidenceMode>("combined");
  const [view, setView] = useState<"grid" | "list">("grid");
  const [search, setSearch] = useState("");
  const [filter, setFilter] = useState("all");
  const [selected, setSelected] = useState<string | null>(null);
  const close = useCallback(() => setSelected(null), []);
  const ordered = [...snapshot.services].sort((left, right) =>
    impactOrder.indexOf(left.severity) - impactOrder.indexOf(right.severity) || left.service.localeCompare(right.service));
  const visible = ordered.filter((service) => {
    const matches = `${service.service} ${service.domain}`.toLowerCase().includes(search.trim().toLowerCase());
    return matches && (filter === "all" || (filter === "attention" ? affected(service) :
      filter === "unknown" ? service.severity === "unknown" || Object.values(service.availability).some((item) => item.state === "stale") : service.severity === filter));
  });
  const groups = new Map<string, ServiceHealthService[]>();
  for (const service of visible) {
    const domain = service.domain || "Ungrouped";
    groups.set(domain, [...(groups.get(domain) ?? []), service]);
  }
  const inspecting = snapshot.services.find((service) => service.service === selected);

  return <>
    <div className="heatmap-toolbar">
      <label className="min-w-0 flex-1 basis-48">
        <span className="sr-only">Find a service</span>
        <span className="relative block"><Search size={15} className="pointer-events-none absolute left-3 top-2.5 text-ink-400" aria-hidden />
          <input type="search" className="input pl-9" placeholder="Search services" value={search} onChange={(event) => setSearch(event.target.value)} />
        </span>
      </label>
      <label className="min-w-0 flex-1 basis-40 sm:max-w-56">
        <span className="sr-only">Health state</span>
        <select className="input" value={filter} onChange={(event) => setFilter(event.target.value)}>
          <option value="all">All services ({snapshot.services.length})</option>
          <option value="attention">Needs attention ({ordered.filter(affected).length})</option>
          <option value="unknown">Unknown or stale</option>
          {["degraded", "critical", "pressure", "creeping", "nominal"].filter((key) => snapshot.facets[key] != null).map((key) =>
            <option key={key} value={key}>{impactLabels[key]} ({snapshot.facets[key]})</option>)}
        </select>
      </label>
      <label className="min-w-0 flex-1 basis-32 sm:max-w-40">
        <span className="sr-only">Show data</span>
        <select className="input" value={mode} onChange={(event) => setMode(event.target.value as EvidenceMode)} title="Changes displayed values, not service status">
          <option value="combined">All data</option><option value="logs">Logs</option><option value="incidents">Incidents</option>
        </select>
      </label>
      <div className="flex items-center gap-1" role="group" aria-label="Service view">
        {(["grid", "list"] as const).map((value) => {
          const Icon = value === "grid" ? LayoutGrid : List;
          return <button key={value} type="button" className={`btn h-9 w-9 justify-center p-0 ${view === value ? "bg-accent text-white" : ""}`} aria-label={`${value === "grid" ? "Grid" : "List"} view`} title={`${value === "grid" ? "Grid" : "List"} view`} aria-pressed={view === value} onClick={() => setView(value)}><Icon size={16} aria-hidden /></button>;
        })}
      </div>
    </div>

    {visible.length === 0 && <p className="py-8 text-sm text-ink-300" role="status">No services match these filters.</p>}
    <div className="heatmap-canvas" data-view={view} data-testid="service-health-domains">
      {[...groups].map(([domain, services]) => <section key={domain} aria-label={domain}>
        <div className="mb-2 flex flex-wrap items-baseline gap-2"><h3 className="break-words text-xs font-semibold text-ink-200 [overflow-wrap:anywhere]">{domain}</h3><span className="text-2xs text-ink-400">{services.length}</span></div>
        <div className={view === "grid" ? "heatmap-grid" : "heatmap-list"}>
          {services.map((service) => <button key={service.service} type="button" aria-label={`Inspect ${service.service}`} data-impact={service.severity} onClick={() => setSelected(service.service)}
            className={`heatmap-service ${view === "list" ? "heatmap-service-row" : ""}`}>
            <div className="min-w-0"><div className="mb-2 flex items-center justify-between gap-2"><Impact service={service} /><ArrowUpRight size={14} className="shrink-0 text-ink-400" aria-hidden /></div>
              <h4 className="break-words text-sm font-semibold text-ink-50 [overflow-wrap:anywhere]">{service.service}</h4>
              {(service.availability.logs?.state !== "ready" || service.logs.estimated) && <p className="mt-1 text-2xs text-ink-300">{coverageLabel(service)}</p>}
            </div>
            <EvidenceValues service={service} mode={mode} />
          </button>)}
        </div>
      </section>)}
    </div>

    <PeekPanel open={Boolean(inspecting)} onClose={close} title="Service details" footer={inspecting && <Link className="btn btn-primary" to={`/agent/services/${encodeURIComponent(inspecting.service)}`}>Open service <ArrowUpRight size={14} aria-hidden /></Link>}>
      {inspecting && <div className="space-y-6">
        <div><p className="mb-2 text-xs text-ink-400">{inspecting.domain || "Ungrouped"}</p><h3 className="mb-3 break-words text-lg font-semibold text-ink-50 [overflow-wrap:anywhere]">{inspecting.service}</h3><Impact service={inspecting} /></div>
        <EvidenceValues service={inspecting} mode={mode} />
        <dl className="space-y-4 text-sm">
          <PeekField label="Data used">{healthDataLabel(inspecting.assessment_basis)}</PeekField>
          <PeekField label="Logs">{coverageLabel(inspecting)}</PeekField>
          <PeekField label="Incident data">{SERVICE_HEALTH_STATE_COPY[inspecting.availability.internal?.state ?? "unsupported"].label}</PeekField>
          <PeekField label="Last log received">{validTime(inspecting.logs.latest_observation) ? fmtAbs(inspecting.logs.latest_observation!) : "Not observed"}</PeekField>
          <PeekField label="New log patterns">{logValue(inspecting, inspecting.logs.new_patterns)}</PeekField>
          <PeekField label="Unrecognized log patterns">{logValue(inspecting, inspecting.logs.unknown_patterns)}</PeekField>
          <PeekField label="Log spikes">{logValue(inspecting, inspecting.logs.spiking_patterns)}</PeekField>
        </dl>
      </div>}
    </PeekPanel>
  </>;
}