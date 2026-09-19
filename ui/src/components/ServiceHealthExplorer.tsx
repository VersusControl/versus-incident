import { useCallback, useEffect, useId, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { ArrowUpRight, CheckCircle2, ChevronDown, CircleHelp, LayoutGrid, List, Search, ShieldAlert } from "lucide-react";
import type { ServiceHealthAvailability, ServiceHealthService, ServiceHealthSignalEvidence, ServiceHealthSnapshot } from "@/lib/api";
import { fmtAbs } from "@/lib/format";
import { PeekField, PeekPanel } from "@/components/PeekPanel";
import { healthDataLabel, healthMeasureLabel, healthReasonLabel } from "@/lib/serviceHealthPresentation";
import "./ServiceHeatmap.css";

type EvidenceMode = "combined" | "logs" | "incidents" | "latency" | "request_error_ratio" | "throughput" | "regression";

const MODE_LABELS: Record<EvidenceMode, string> = {
  combined: "All data",
  logs: "Logs",
  incidents: "Incidents",
  latency: "Latency P99",
  request_error_ratio: "Request error rate",
  throughput: "Throughput",
  regression: "Regression",
};

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

function supportsMeasure(snapshot: ServiceHealthSnapshot, measure: string) {
  return snapshot.capabilities.some((capability) =>
    (capability.family === "metrics" || capability.family === "traces") &&
    capability.measures[measure] != null && capability.measures[measure].state !== "restricted");
}

function modesFor(snapshot: ServiceHealthSnapshot): EvidenceMode[] {
  const modes: EvidenceMode[] = ["combined", "logs", "incidents"];
  for (const measure of ["latency", "request_error_ratio", "throughput"] as const) {
    if (supportsMeasure(snapshot, measure)) modes.push(measure);
  }
  const assessment = snapshot.capabilities.find((capability) => capability.family === "assessment");
  if (assessment?.measures.regression_score && assessment.measures.regression_score.state !== "restricted") modes.push("regression");
  return modes;
}

function evidenceFor(service: ServiceHealthService, measure: string) {
  return (service.evidence ?? []).filter((item) =>
    (item.family === "metrics" || item.family === "traces") && item.measure === measure);
}

const availabilityPriority = ["error", "stale", "collecting", "not_configured", "partial", "restricted", "no_data", "unsupported", "ready"] as const;

function preferredAvailability(candidates: Array<ServiceHealthAvailability | undefined>) {
  const available = candidates.filter((item): item is ServiceHealthAvailability => item != null);
  return availabilityPriority
    .map((state) => available.find((candidate) => candidate.state === state))
    .find((candidate) => candidate != null);
}

function availabilityFor(service: ServiceHealthService, snapshot: ServiceHealthSnapshot, measure: string) {
  const serviceAvailability = (["metrics", "traces"] as const)
    .map((family) => service.availability[`${family}.${measure}`])
    .filter((item): item is NonNullable<typeof item> => item != null);
  if (serviceAvailability.length > 0) return preferredAvailability(serviceAvailability);
  return preferredAvailability((["metrics", "traces"] as const).map((family) =>
    snapshot.capabilities.find((capability) => capability.family === family)?.measures[measure]));
}

function formatNumber(value: number) {
  return value.toLocaleString(undefined, { maximumFractionDigits: 2 });
}

function formatEvidenceValue(evidence: ServiceHealthSignalEvidence): string {
  if (evidence.availability.state !== "ready") {
    return healthReasonLabel(evidence.availability.reason_code, evidence.availability.state);
  }
  if (evidence.value == null) return healthReasonLabel(evidence.availability.reason_code, "no_data");
  if (evidence.measure === "request_error_ratio" && evidence.unit === "ratio") {
    return `${(evidence.value * 100).toLocaleString(undefined, { maximumFractionDigits: 2 })}%`;
  }
  if (evidence.measure === "latency" && evidence.unit === "ms") return `${formatNumber(evidence.value)} ms`;
  return evidence.unit ? `${formatNumber(evidence.value)} ${evidence.unit}` : formatNumber(evidence.value);
}

function measureValue(service: ServiceHealthService, snapshot: ServiceHealthSnapshot, measure: string): string {
  const evidence = evidenceFor(service, measure);
  if (new Set(evidence.map((item) => `${item.family}.${item.source_ref}`)).size > 1) return "Multiple sources";
  if (evidence.length > 1) return "Multiple operations";
  if (evidence.length === 1) return formatEvidenceValue(evidence[0]);
  const availability = availabilityFor(service, snapshot, measure);
  if (availability?.state === "ready") return "No recent data";
  return availability ? healthReasonLabel(availability.reason_code, availability.state) : "No recent data";
}

function validRegressionScore(value: number | null | undefined): value is number {
  return typeof value === "number" && Number.isFinite(value) && value >= 0 && value <= 100;
}

function validConfidence(value: number): boolean {
  return Number.isFinite(value) && value > 0 && value <= 1;
}

function assessmentAvailability(service: ServiceHealthService, snapshot: ServiceHealthSnapshot): ServiceHealthAvailability {
  const assessment = service.assessment;
  const availability = service.availability["assessment.regression_score"] ??
    snapshot.capabilities.find((capability) => capability.family === "assessment")?.measures.regression_score;
  const state = availability?.state ?? (assessment?.regression_score == null ? "no_data" : "ready");
  const reasonCode = assessment?.regression_score != null && state !== "ready"
    ? availability?.reason_code
    : assessment?.reason_code ?? availability?.reason_code;
  return { state, reason_code: reasonCode };
}

function regressionValue(service: ServiceHealthService, snapshot: ServiceHealthSnapshot): string {
  const assessment = service.assessment;
  const availability = currentAssessmentAvailability(service, snapshot);
  if (availability.state !== "ready") return healthReasonLabel(availability.reason_code, availability.state);
  if (assessment?.regression_score != null) return `${formatNumber(assessment.regression_score)} / 100`;
  return healthReasonLabel(availability.reason_code, availability.state);
}

function currentAssessmentAvailability(service: ServiceHealthService, snapshot: ServiceHealthSnapshot): ServiceHealthAvailability {
  const availability = assessmentAvailability(service, snapshot);
  if (availability.state !== "ready") return availability;
  if (!validRegressionScore(service.assessment?.regression_score) || !validConfidence(service.assessment.confidence)) {
    return { state: "no_data", reason_code: "provider_status_unavailable" };
  }
  const freshUntil = service.assessment?.fresh_until;
  if (validTime(freshUntil) && validTime(snapshot.generated_at) && Date.parse(freshUntil!) < Date.parse(snapshot.generated_at)) {
    return { state: "stale", reason_code: "stale_baseline" };
  }
  return availability;
}

function sourceLabel(value: string) {
  return value.replaceAll("_", " ").replace(/\b\w/g, (character) => character.toUpperCase());
}

function summaryMeasurements(service: ServiceHealthService, snapshot: ServiceHealthSnapshot) {
  const measurements = [
    { label: "Active incidents", value: service.active_incidents == null ? "Not assessed" : service.active_incidents.toLocaleString() },
    { label: "Log events", value: logValue(service, service.logs.matched_logs) },
  ];
  const premiumMeasure = (["latency", "request_error_ratio", "throughput"] as const)
    .find((measure) => supportsMeasure(snapshot, measure) && evidenceFor(service, measure).length > 0);
  measurements.push(premiumMeasure
    ? { label: healthMeasureLabel(premiumMeasure), value: measureValue(service, snapshot, premiumMeasure) }
    : { label: "Log spikes", value: logValue(service, service.logs.spiking_patterns) });
  return measurements;
}

function AssessmentSummary({ service, snapshot }: { service: ServiceHealthService; snapshot: ServiceHealthSnapshot }) {
  const assessment = service.assessment;
  if (!assessment) return null;
  const availability = currentAssessmentAvailability(service, snapshot);
  const current = availability.state === "ready";
  const score = current && validRegressionScore(assessment.regression_score) ? assessment.regression_score : null;
  const status = healthReasonLabel(availability.reason_code, availability.state);
  return <section aria-labelledby="service-regression-title" className="border-y border-ink-600 py-5">
    <div className="flex flex-wrap items-start justify-between gap-3">
      <div>
        <h3 id="service-regression-title" className="text-sm font-semibold text-ink-50">Regression assessment</h3>
        {!current && <p className="mt-1 text-sm text-ink-300">{status}</p>}
      </div>
      {score != null && <span className="text-xl font-semibold tabular-nums text-ink-50">{formatNumber(score)} <span className="text-xs font-normal text-ink-400">/ 100</span></span>}
    </div>
    {score != null && <div className="mt-4">
      <div role="meter" aria-label="Regression score" aria-valuemin={0} aria-valuemax={100} aria-valuenow={score} className="relative h-2 overflow-hidden rounded-full bg-ink-600">
        <div className="h-full rounded-full bg-sev-critical" style={{ width: `${Math.min(100, Math.max(0, score))}%` }} />
      </div>
    </div>}
    {(assessment.drivers?.length ?? 0) > 0 && <div className="mt-4">
      <h4 className="text-2xs font-medium uppercase text-ink-400">Main contributors</h4>
      <ul className="mt-2 flex flex-wrap gap-2">
        {assessment.drivers!.map((driver, index) => <li key={`${driver.family}-${driver.measure}-${driver.operation ?? "service"}-${index}`} className="rounded-control bg-ink-700 px-2.5 py-1 text-xs text-ink-100">
          {driver.operation ? `${healthMeasureLabel(driver.measure)} (${driver.operation})` : `${healthMeasureLabel(driver.measure)} (service-wide)`}
        </li>)}
      </ul>
    </div>}
    {score != null && <p className="mt-4 text-xs text-ink-300"><span className="inline-flex items-center gap-1" title="Ranking aid, not a probability">Confidence {Math.round(assessment.confidence * 100)}% <CircleHelp size={12} aria-hidden /></span></p>}
  </section>;
}

function EvidenceRows({ evidence }: { evidence: ServiceHealthSignalEvidence[] }) {
  return <div className="divide-y divide-ink-600 border-y border-ink-600">
    {evidence.map((item, index) => <details key={`${item.family}-${item.measure}-${item.operation ?? "service"}-${index}`} className="group">
      <summary className="grid cursor-pointer list-none grid-cols-[minmax(0,1fr)_minmax(5.5rem,auto)_auto] items-center gap-3 py-3 text-sm marker:content-none">
        <span className="min-w-0"><span className="block font-medium text-ink-100">{healthMeasureLabel(item.measure)}</span><span className="mt-0.5 block truncate text-xs text-ink-400">{sourceLabel(item.source_ref)}</span></span>
        <span className="text-right font-semibold tabular-nums text-ink-50">{formatEvidenceValue(item)}</span>
        <ChevronDown size={15} className="text-ink-400 transition-transform group-open:rotate-180" aria-hidden />
      </summary>
      <dl className="grid grid-cols-2 gap-x-5 gap-y-3 pb-4 text-sm sm:grid-cols-3">
        {item.availability.state !== "ready" && <PeekField label="State">{healthReasonLabel(item.availability.reason_code, item.availability.state)}</PeekField>}
        <PeekField label="Operation">{item.operation || "Service-wide"}</PeekField>
        <PeekField label="Observed">{validTime(item.observed_at) ? fmtAbs(item.observed_at) : "Not observed"}</PeekField>
        <PeekField label="Signal">{item.signal_ref || "Not provided"}</PeekField>
        <PeekField label="Fresh until">{validTime(item.fresh_until) ? fmtAbs(item.fresh_until!) : "Not provided"}</PeekField>
        {item.provenance && <PeekField label="Measurement">{sourceLabel(item.provenance)}</PeekField>}
      </dl>
    </details>)}
  </div>;
}

function ServiceHealthDetail({ service, snapshot }: { service: ServiceHealthService; snapshot: ServiceHealthSnapshot }) {
  const hasSignals = (service.evidence?.length ?? 0) > 0;
  const tabs = hasSignals ? ["Overview", "Signals", "Logs"] as const : ["Overview", "Logs"] as const;
  const [tab, setTab] = useState<"Overview" | "Signals" | "Logs">("Overview");
  const activeTab = tabs.includes(tab as "Overview" | "Logs") ? tab : "Overview";
  const tabSetId = useId();
  const assessmentAvailability = currentAssessmentAvailability(service, snapshot);
  const logsState = service.availability.logs?.state ?? "unsupported";
  const logsStatus = coverageLabel(service);
  const incidentsState = service.availability.internal?.state ?? "unsupported";

  useEffect(() => setTab("Overview"), [service.service]);
  useEffect(() => {
    if (!hasSignals && tab === "Signals") setTab("Overview");
  }, [hasSignals, tab]);

  const moveTabFocus = (event: React.KeyboardEvent<HTMLButtonElement>, index: number) => {
    let next = index;
    if (event.key === "ArrowRight") next = (index + 1) % tabs.length;
    else if (event.key === "ArrowLeft") next = (index - 1 + tabs.length) % tabs.length;
    else if (event.key === "Home") next = 0;
    else if (event.key === "End") next = tabs.length - 1;
    else return;
    event.preventDefault();
    setTab(tabs[next]);
    event.currentTarget.parentElement?.querySelectorAll<HTMLButtonElement>('[role="tab"]')[next]?.focus();
  };

  return <div>
    <div className="mb-5">
      <p className="break-words text-sm text-ink-300 [overflow-wrap:anywhere]">{service.domain || "Ungrouped"}</p>
      <div className="mt-3 flex flex-wrap items-center gap-x-4 gap-y-2">
        <Impact service={service} />
        {logsState !== "ready" && <span className="text-xs text-ink-300">Logs: {healthReasonLabel(service.availability.logs?.reason_code, logsState)}</span>}
        <span className="text-xs text-ink-400">Updated {validTime(snapshot.generated_at) ? fmtAbs(snapshot.generated_at) : "not available"}</span>
      </div>
    </div>

    <dl className="grid grid-cols-2 gap-4 pb-5 md:grid-cols-3">
      {summaryMeasurements(service, snapshot).map(({ label, value }) => <div key={label} className="min-w-0">
        <dt className="text-2xs uppercase text-ink-400">{label}</dt>
        <dd className="mt-1 break-words text-lg font-semibold tabular-nums text-ink-50">{value}</dd>
      </div>)}
    </dl>

    <AssessmentSummary service={service} snapshot={snapshot} />

    <div className="sticky top-0 z-10 -mx-5 mt-5 border-b border-ink-600 bg-surface-raised px-5">
      <div role="tablist" aria-label="Service detail views" className="flex gap-5">
        {tabs.map((name, index) => <button key={name} id={`${tabSetId}-${name.toLowerCase()}-tab`} type="button" role="tab" aria-selected={activeTab === name} aria-controls={`${tabSetId}-${name.toLowerCase()}-panel`} tabIndex={activeTab === name ? 0 : -1} className={`border-b-2 py-3 text-sm font-medium ${activeTab === name ? "border-accent text-ink-50" : "border-transparent text-ink-300 hover:text-ink-100"}`} onClick={() => setTab(name)} onKeyDown={(event) => moveTabFocus(event, index)}>{name}</button>)}
      </div>
    </div>

    {activeTab === "Overview" && <div id={`${tabSetId}-overview-panel`} role="tabpanel" aria-labelledby={`${tabSetId}-overview-tab`} tabIndex={0} className="space-y-5 pt-5">
      <section aria-labelledby="service-availability-title">
        <h3 id="service-availability-title" className="text-sm font-semibold text-ink-100">Availability</h3>
        <dl className="mt-3 grid grid-cols-2 gap-4 text-sm sm:grid-cols-3">
          <PeekField label="Impact">{impactLabels[service.severity] ?? "Unknown"}</PeekField>
          {logsStatus && <PeekField label="Logs">{logsStatus}</PeekField>}
          {incidentsState !== "ready" && <PeekField label="Incident data">{healthReasonLabel(service.availability.internal?.reason_code, incidentsState)}</PeekField>}
        </dl>
      </section>
      <details className="border-t border-ink-600 pt-4">
        <summary className="cursor-pointer text-sm font-medium text-ink-200">Measurement diagnostics</summary>
        <dl className="mt-4 grid grid-cols-2 gap-4 text-sm sm:grid-cols-3">
          <PeekField label="Data used">{healthDataLabel(service.assessment_basis)}</PeekField>
          <PeekField label="Last log received">{validTime(service.logs.latest_observation) ? fmtAbs(service.logs.latest_observation!) : "Not observed"}</PeekField>
          {service.assessment && <PeekField label="Assessment">{healthReasonLabel(assessmentAvailability.reason_code, assessmentAvailability.state)}</PeekField>}
          {service.assessment && <PeekField label="Assessed">{validTime(service.assessment.assessed_at) ? fmtAbs(service.assessment.assessed_at) : "Not assessed"}</PeekField>}
        </dl>
      </details>
    </div>}

    {activeTab === "Signals" && hasSignals && <section id={`${tabSetId}-signals-panel`} role="tabpanel" aria-labelledby={`${tabSetId}-signals-tab`} tabIndex={0} className="pt-5">
      <div className="mb-3 grid grid-cols-[minmax(0,1fr)_minmax(5.5rem,auto)_auto] gap-3 text-2xs uppercase text-ink-400"><span>Measure and source</span><span className="text-right">Current</span><span className="sr-only">Details</span></div>
      <EvidenceRows evidence={service.evidence!} />
    </section>}

    {activeTab === "Logs" && <section id={`${tabSetId}-logs-panel`} role="tabpanel" aria-labelledby={`${tabSetId}-logs-tab`} tabIndex={0} className="pt-5">
      <div className="flex flex-wrap items-baseline justify-between gap-2"><h3 className="text-sm font-semibold text-ink-100">Log summary</h3>{logsStatus && <span className="text-xs text-ink-400">{logsStatus}</span>}</div>
      <dl className="mt-4 grid grid-cols-2 gap-4 text-sm sm:grid-cols-4">
        <PeekField label="Events">{logValue(service, service.logs.matched_logs)}</PeekField>
        <PeekField label="Patterns">{logValue(service, service.logs.unique_patterns)}</PeekField>
        <PeekField label="New">{logValue(service, service.logs.new_patterns)}</PeekField>
        <PeekField label="Unrecognized">{logValue(service, service.logs.unknown_patterns)}</PeekField>
        <PeekField label="Spikes">{logValue(service, service.logs.spiking_patterns)}</PeekField>
        <PeekField label="Latest">{validTime(service.logs.latest_observation) ? fmtAbs(service.logs.latest_observation!) : "Not observed"}</PeekField>
      </dl>
    </section>}
  </div>;
}

function evidenceSummary(service: ServiceHealthService, snapshot: ServiceHealthSnapshot, mode: EvidenceMode) {
  const incidents = service.active_incidents == null ? "Not assessed" : service.active_incidents.toLocaleString();
  if (mode === "incidents") return [["Active incidents", incidents]];
  if (mode === "logs") return [
    ["Log events", logValue(service, service.logs.matched_logs)],
    ["Log spikes", logValue(service, service.logs.spiking_patterns)],
  ];
  if (mode === "latency" || mode === "request_error_ratio" || mode === "throughput") {
    return [[MODE_LABELS[mode], measureValue(service, snapshot, mode)]];
  }
  if (mode === "regression") return [["Regression score", regressionValue(service, snapshot)]];
  return [["Log events", logValue(service, service.logs.matched_logs)], ["Active incidents", incidents]];
}

function EvidenceValues({ service, snapshot, mode }: { service: ServiceHealthService; snapshot: ServiceHealthSnapshot; mode: EvidenceMode }) {
  return <dl className="grid grid-cols-2 gap-3 text-left">
    {evidenceSummary(service, snapshot, mode).map(([label, value]) => <div key={label} className="min-w-0">
      <dt className="text-2xs text-ink-400">{label}</dt>
      <dd className="mt-1 break-words text-sm font-semibold tabular-nums text-ink-100">{value}</dd>
    </div>)}
  </dl>;
}

function coverageLabel(service: ServiceHealthService): string | null {
  const state = service.availability.logs?.state ?? "unsupported";
  if (state === "ready" && measured(service)) {
    return service.logs.estimated ? "Estimated counts" : null;
  }
  return healthReasonLabel(service.availability.logs?.reason_code, state);
}

export function ServiceHealthExplorer({
  snapshot,
  selectedService,
  onSelectedServiceChange,
}: {
  snapshot: ServiceHealthSnapshot;
  selectedService?: string | null;
  onSelectedServiceChange?: (service: string | null) => void;
}) {
  const [mode, setMode] = useState<EvidenceMode>("combined");
  const [view, setView] = useState<"grid" | "list">("grid");
  const [search, setSearch] = useState("");
  const [filter, setFilter] = useState("all");
  const [internalSelected, setInternalSelected] = useState<string | null>(null);
  const selected = selectedService === undefined ? internalSelected : selectedService;
  const setSelected = useCallback((service: string | null) => {
    if (selectedService === undefined) setInternalSelected(service);
    onSelectedServiceChange?.(service);
  }, [onSelectedServiceChange, selectedService]);
  const [availabilityNotice, setAvailabilityNotice] = useState("");
  const close = useCallback(() => setSelected(null), [setSelected]);
  const modes = modesFor(snapshot);
  const modeSignature = modes.join("|");
  const premiumStates = snapshot.capabilities
    .filter((capability) => capability.family === "metrics" || capability.family === "traces" || capability.family === "assessment")
    .flatMap((capability) => Object.values(capability.measures).map((measure) => measure.state));
  const hasPremiumAccess = premiumStates.some((state) => state !== "restricted");
  const premiumAccessDenied = premiumStates.length > 0 && premiumStates.every((state) => state === "restricted");
  const hadPremiumAccess = useRef(hasPremiumAccess);
  useEffect(() => {
    const premiumFilterUnavailable =
      filter === "regressing" && snapshot.facets.regressing == null;
    const premiumModeUnavailable = !modeSignature.split("|").includes(mode);
    const genuineAccessLoss = hadPremiumAccess.current && premiumAccessDenied;
    if (premiumModeUnavailable) {
      setMode("combined");
    }
    if (premiumFilterUnavailable || genuineAccessLoss) setFilter("all");
    if (genuineAccessLoss) {
      setSelected(null);
      setAvailabilityNotice("Premium Service Health data is no longer available. Showing All data and all services.");
    }
    if (premiumStates.length > 0) hadPremiumAccess.current = hasPremiumAccess;
  }, [filter, hasPremiumAccess, mode, modeSignature, premiumAccessDenied, premiumStates.length, setSelected, snapshot.facets.regressing]);
  useEffect(() => {
    if (!availabilityNotice) return;
    const timeout = window.setTimeout(() => setAvailabilityNotice(""), 4000);
    return () => window.clearTimeout(timeout);
  }, [availabilityNotice]);
  const ordered = [...snapshot.services].sort((left, right) =>
    impactOrder.indexOf(left.severity) - impactOrder.indexOf(right.severity) || left.service.localeCompare(right.service));
  const visible = ordered.filter((service) => {
    const matches = `${service.service} ${service.domain}`.toLowerCase().includes(search.trim().toLowerCase());
    return matches && (filter === "all" || (filter === "attention" ? affected(service) :
      filter === "regressing" ? service.assessment?.regressing === true :
      filter === "unknown" ? service.severity === "unknown" || Object.values(service.availability).some((item) => item.state === "stale") : service.severity === filter));
  });
  const groups = new Map<string, ServiceHealthService[]>();
  for (const service of visible) {
    const domain = service.domain || "Ungrouped";
    groups.set(domain, [...(groups.get(domain) ?? []), service]);
  }
  const inspecting = snapshot.services.find((service) => service.service === selected);
  useEffect(() => {
    if (selected && !snapshot.services.some((service) => service.service === selected)) setSelected(null);
  }, [selected, setSelected, snapshot.services]);

  return <>
    <p className="sr-only" aria-live="polite">{availabilityNotice}</p>
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
          <option value="attention">Needs attention ({["degraded", "critical", "pressure", "creeping"].reduce((count, key) => count + (snapshot.facets[key] ?? 0), 0)})</option>
          <option value="unknown">Unknown or stale</option>
          {["degraded", "critical", "pressure", "creeping", "nominal"].filter((key) => snapshot.facets[key] != null).map((key) =>
            <option key={key} value={key}>{impactLabels[key]} ({snapshot.facets[key]})</option>)}
          {snapshot.facets.regressing != null && <option value="regressing">Regressing ({snapshot.facets.regressing})</option>}
        </select>
      </label>
      <label className="min-w-0 flex-1 basis-32 sm:max-w-40">
        <span className="sr-only">Show data</span>
        <select className="input" value={mode} onChange={(event) => setMode(event.target.value as EvidenceMode)} title="Changes displayed values, not service status">
          {modes.map((value) => <option key={value} value={value}>{MODE_LABELS[value]}</option>)}
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
              {(mode === "combined" || mode === "logs") && service.availability.logs?.state !== "ready" && <p className="mt-1 text-2xs text-ink-300">{coverageLabel(service)}</p>}
            </div>
            <EvidenceValues service={service} snapshot={snapshot} mode={mode} />
          </button>)}
        </div>
      </section>)}
    </div>

    <PeekPanel open={Boolean(inspecting)} onClose={close} title={inspecting?.service ?? "Service details"} ariaLabel={inspecting?.service ?? "Service details"} size="wide" expandable footer={inspecting && <Link className="btn btn-primary" to={`/agent/services/${encodeURIComponent(inspecting.service)}`}>Open service <ArrowUpRight size={14} aria-hidden /></Link>}>
      {inspecting && <ServiceHealthDetail service={inspecting} snapshot={snapshot} />}
    </PeekPanel>
  </>;
}