import { Link } from "react-router-dom";
import {
  Activity,
  AlertCircle,
  CheckCircle2,
  CircleDashed,
  Clock3,
  FileWarning,
  LockKeyhole,
  RefreshCw,
  Radio,
  ServerCog,
  ShieldAlert,
  Settings2,
} from "lucide-react";
import {
  ApiError,
  type ServiceHealthAvailability,
  type ServiceHealthSnapshot,
  type ServiceHealthState,
} from "@/lib/api";
import { fmtAbs, fmtRel } from "@/lib/format";
import { RetryableError } from "@/components/RetryableError";
import { useEffectiveRole } from "@/lib/useEffectiveRole";
import { useServiceHealthQuery } from "@/lib/useServiceHealth";
import { SERVICE_HEALTH_STATE_COPY } from "@/lib/serviceHealthPresentation";
import { ServiceHealthExplorer } from "@/components/ServiceHealthExplorer";

const STATE_PRESENTATION: Record<
  ServiceHealthState,
  { label: string; detail: string; icon: typeof Activity; tone: string }
> = {
  ready: { ...SERVICE_HEALTH_STATE_COPY.ready, icon: CheckCircle2, tone: "text-sev-ok" },
  partial: { ...SERVICE_HEALTH_STATE_COPY.partial, icon: AlertCircle, tone: "text-sev-warn" },
  not_configured: { ...SERVICE_HEALTH_STATE_COPY.not_configured, icon: ServerCog, tone: "text-ink-300" },
  collecting: { ...SERVICE_HEALTH_STATE_COPY.collecting, icon: CircleDashed, tone: "text-sev-info" },
  no_data: { ...SERVICE_HEALTH_STATE_COPY.no_data, icon: FileWarning, tone: "text-ink-300" },
  unsupported: { ...SERVICE_HEALTH_STATE_COPY.unsupported, icon: ShieldAlert, tone: "text-sev-warn" },
  error: { ...SERVICE_HEALTH_STATE_COPY.error, icon: AlertCircle, tone: "text-sev-critical" },
  stale: { ...SERVICE_HEALTH_STATE_COPY.stale, icon: Clock3, tone: "text-sev-warn" },
  restricted: { ...SERVICE_HEALTH_STATE_COPY.restricted, icon: LockKeyhole, tone: "text-ink-300" },
};

const STATE_PRIORITY: ServiceHealthState[] = [
  "error",
  "stale",
  "partial",
  "ready",
  "collecting",
  "no_data",
  "not_configured",
  "unsupported",
  "restricted",
];

const PREVIEW_STATES = new Set<ServiceHealthState>([
  "not_configured",
  "collecting",
  "no_data",
  "error",
]);

function stateForCapability(snapshot: ServiceHealthSnapshot, family: string): ServiceHealthState {
  const measures = snapshot.capabilities.find((item) => item.family === family)?.measures;
  if (!measures || Object.keys(measures).length === 0) return "unsupported";
  const states = Object.values(measures).map((item) => item.state);
  return STATE_PRIORITY.find((state) => states.includes(state)) ?? "unsupported";
}

function hasUsableEvidence(snapshot: ServiceHealthSnapshot): boolean {
  return snapshot.services.some((service) => {
    const states = Object.values(service.availability ?? {}).map((item) => item.state);
    return (
      service.active_incidents != null ||
      service.logs.matched_logs > 0 ||
      meaningfulTimestamp(service.logs.latest_observation) ||
      states.some((state) => state === "ready")
    );
  });
}

function currentState(snapshot: ServiceHealthSnapshot, hasEvidence: boolean): ServiceHealthState {
  const serviceStates = snapshot.services.flatMap((service) =>
    Object.values(service.availability ?? {}).map((item) => item.state),
  );
  if (hasEvidence) {
    return STATE_PRIORITY.find((state) => serviceStates.includes(state)) ?? "ready";
  }
  return stateForCapability(snapshot, "logs");
}

function meaningfulTimestamp(value?: string): boolean {
  if (!value) return false;
  const date = new Date(value);
  return !Number.isNaN(date.getTime()) && date.getUTCFullYear() > 1970;
}

function HealthStateBadge({ state, prefix }: { state: ServiceHealthState; prefix?: string }) {
  const presentation = STATE_PRESENTATION[state];
  const Icon = presentation.icon;
  return (
    <span className={`inline-flex items-center gap-1.5 text-2xs font-medium ${presentation.tone}`}>
      <Icon size={13} aria-hidden />
      {prefix ? `${prefix}: ` : ""}{presentation.label}
    </span>
  );
}

function ExamplePreview() {
  return (
    <figure className="mt-5" data-testid="service-health-preview">
      <figcaption className="mb-3 text-xs font-semibold text-ink-100">
        Example preview - sample data
      </figcaption>
      <div className="heatmap-canvas">
        <div className="heatmap-grid">
          {[
            { name: "checkout", state: "nominal", label: "Normal", events: "1,284", incidents: "0" },
            { name: "payments", state: "pressure", label: "Warning", events: "386", incidents: "1" },
            { name: "search", state: "unknown", label: "Unknown", events: "Not measured", incidents: "Not assessed" },
          ].map((sample) => <div key={sample.name} className="heatmap-service" data-impact={sample.state}>
            <div><span className="text-xs text-ink-300">{sample.label}</span><h3 className="mt-2 text-sm font-semibold text-ink-100">{sample.name}</h3></div>
            <dl className="grid grid-cols-2 gap-3 text-xs"><div><dt className="text-ink-300">Log events</dt><dd className="mt-1 font-medium">{sample.events}</dd></div><div><dt className="text-ink-300">Active incidents</dt><dd className="mt-1 font-medium">{sample.incidents}</dd></div></dl>
          </div>)}
        </div>
      </div>
    </figure>
  );
}

function actionFor(snapshot: ServiceHealthSnapshot): ServiceHealthAvailability | undefined {
  const candidates = [
    ...snapshot.capabilities
      .filter((capability) => capability.family === "logs" || capability.family === "internal")
      .flatMap((capability) => Object.values(capability.measures)),
    ...snapshot.services.flatMap((service) => Object.values(service.availability ?? {})),
  ];
  const actionable = candidates.find((item) => item.action_id && item.state === "error")
    ?? candidates.find((item) => item.action_id && item.state === "stale")
    ?? candidates.find((item) => item.action_id && item.state === "partial")
    ?? candidates.find((item) => item.action_id && item.state === "not_configured")
    ?? candidates.find((item) => item.action_id && item.state === "unsupported")
    ?? candidates.find((item) => item.action_id && item.state === "restricted");
  if (actionable || hasUsableEvidence(snapshot)) return actionable;
  return candidates.find((item) => item.state === "collecting")
    ?? candidates.find((item) => item.state === "no_data");
}

function GapAction({ snapshot }: { snapshot: ServiceHealthSnapshot }) {
  const access = useEffectiveRole();
  const gap = actionFor(snapshot);
  if (!gap) return null;
  const canManage = !access.loading && (!access.enterprise || access.isAdmin);

  let title = STATE_PRESENTATION[gap.state].detail;
  let label = "Open settings";
  let href = "/settings?tab=agent";
  if (gap.action_id === "connect_log_source") {
    title = "Logs not connected";
    label = "Connect a log source";
  } else if (gap.action_id === "review_log_source") {
    title = "Log connection needs attention";
    label = "Review connection";
  } else if (gap.state === "collecting") {
    title = meaningfulTimestamp(snapshot.next_collection_at)
      ? `Next update ${fmtRel(snapshot.next_collection_at)}`
      : "Waiting for logs";
    label = "View timing settings";
    href = "/settings?tab=tuning";
  } else if (gap.state === "no_data") {
    title = "No logs in this window";
    label = "Review timing settings";
    href = "/settings?tab=tuning";
  }

  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
      <p className="text-xs text-ink-300">{title}</p>
      {canManage ? (
        <Link className="shrink-0 font-medium text-link hover:underline" to={href}>{label}</Link>
      ) : (
        <span className="inline-flex items-center gap-1.5 text-xs font-medium text-ink-200">
          <LockKeyhole size={13} aria-hidden /> Ask an administrator
        </span>
      )}
    </div>
  );
}

export function ServiceHealthSection() {
  const query = useServiceHealthQuery();

  if (query.isPending) {
    return (
      <section className="min-h-40 pt-3" aria-labelledby="service-health-title">
        <h2 id="service-health-title" className="inline-flex items-center gap-2 text-base font-semibold text-ink-50"><Radio size={17} className="text-ink-300" aria-hidden />Service Heatmap</h2>
        <p className="mt-3 inline-flex items-center gap-2 text-xs text-ink-300"><RefreshCw size={14} className="animate-spin" aria-hidden /> Loading current assessment…</p>
      </section>
    );
  }

  const accessDenied = query.error instanceof ApiError && [401, 403].includes(query.error.status);
  if (query.isError && (!query.data || accessDenied)) {
    const unauthorized = query.error instanceof ApiError && query.error.status === 401;
    return (
      <section className="min-h-40 pt-3" aria-labelledby="service-health-title">
        <h2 id="service-health-title" className="inline-flex items-center gap-2 text-base font-semibold text-ink-50"><Radio size={17} className="text-ink-300" aria-hidden />Service Heatmap</h2>
        <div className="mt-3">
          <RetryableError
            error={query.error}
            onRetry={() => query.refetch()}
            retrying={query.isRefetching}
            context={unauthorized ? "Your session is no longer authorized" : "Couldn't load Service Health"}
          />
        </div>
      </section>
    );
  }

  const snapshot = query.data;
  const realEvidence = hasUsableEvidence(snapshot);
  const state = query.isError ? "stale" : currentState(snapshot, realEvidence);
  const stateCopy = STATE_PRESENTATION[state];
  const showPreview = !realEvidence && PREVIEW_STATES.has(state);
  const families = ["logs", "internal", "metrics", "traces"];
  const affected = snapshot.services.filter((service) => ["critical", "degraded", "pressure", "creeping"].includes(service.severity)).length;

  return (
    <section className="heatmap-workspace" aria-labelledby="service-health-title" data-testid="service-health-section">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h2 id="service-health-title" className="inline-flex items-center gap-2 text-lg font-semibold text-ink-50"><Radio size={18} className="text-ink-300" aria-hidden />Service Heatmap</h2>
        <div className="flex flex-wrap items-center gap-3 text-xs text-ink-300">
          <span className="inline-flex items-center gap-1.5"><Clock3 size={13} aria-hidden />Last {Math.round(snapshot.window_seconds / 60)} min</span>
          {meaningfulTimestamp(snapshot.generated_at) && <span title={fmtAbs(snapshot.generated_at)}>Updated {fmtRel(snapshot.generated_at)}</span>}
          <div className="flex items-center gap-1">
            <button type="button" className="btn h-8 w-8 justify-center p-0" aria-label="Refresh service health" title="Refresh service health" disabled={query.isFetching} onClick={() => query.refetch()}><RefreshCw size={14} className={query.isFetching ? "animate-spin" : ""} aria-hidden /></button>
            <Link className="btn h-8 w-8 justify-center p-0" to="/settings?tab=tuning" aria-label="Service Health Timing settings" title="Service Health Timing settings"><Settings2 size={14} aria-hidden /></Link>
          </div>
        </div>
      </div>

      <div className="mt-3 flex flex-wrap items-center gap-x-4 gap-y-2 text-xs text-ink-300" data-testid="service-health-coverage-summary">
        <span>{snapshot.coverage.total_services} services</span>
        <span className={affected ? "font-medium text-sev-warn" : ""}>{realEvidence ? `${affected} need attention` : "Not yet assessed"}</span>
        <span>{snapshot.coverage.observed_services} with data</span>
        {(snapshot.coverage.partial || snapshot.coverage.observed_services < snapshot.coverage.total_services) && <span className="text-sev-warn">Incomplete data</span>}
      </div>
      {query.isError && <p className="mt-3 text-xs text-sev-warn" role="status">Refresh failed. Showing the last assessment.</p>}
      {snapshot.pending_settings && <p className="mt-2 text-xs text-ink-400">New timing applies on the next collection</p>}

      {snapshot.services.length > 0 && <ServiceHealthExplorer snapshot={snapshot} />}
      {!realEvidence && <p className="mt-5 text-sm text-ink-300">{stateCopy.detail}</p>}
      {showPreview && <ExamplePreview />}
      <section className="mt-5 flex flex-wrap items-center justify-between gap-3" aria-label="Data sources">
        <div className="flex flex-wrap items-center gap-x-4 gap-y-2">{families.map((family) => <HealthStateBadge key={family} prefix={family === "internal" ? "Incidents" : family[0].toUpperCase() + family.slice(1)} state={stateForCapability(snapshot, family)} />)}</div>
        <GapAction snapshot={snapshot} />
      </section>
    </section>
  );
}