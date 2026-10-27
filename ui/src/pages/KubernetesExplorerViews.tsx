import { useKubernetesCluster, useKubernetesCrashes, useKubernetesFirstReady, useKubernetesQuery as useQuery } from "@/lib/useKubernetesCluster";
import { KubernetesIssueAlerts } from "@/pages/KubernetesIssueAlerts";
import { NamespaceSelect } from "@/components/NamespaceSelect";
import { ResourceLogsButton } from "@/components/ResourceLogsButton";
import { KubernetesFilterField, filterFieldControlClass, filterRowClass } from "@/components/KubernetesFilterField";
import { isZeroTime } from "@/lib/kubernetesSyncHealth";
import { counted } from "@/lib/kubernetesEvidence";
import { Activity, AlertTriangle, ArrowDown, ArrowLeft, ArrowRight, ArrowUp, Boxes, Cpu, Eye, EyeOff, GitBranch, History, Layers3, Loader2, Maximize2, Minus, Network, Plus, RotateCcw, Waypoints, X } from "lucide-react";
import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { Link, useLocation, useSearchParams } from "react-router-dom";
import { SegmentedControl } from "@/components/SegmentedControl";
import { KubernetesPreparingPanel } from "@/components/KubernetesPreparingPanel";
import { evidenceLoading, pendingKindNames } from "@/lib/kubernetesPending";
import { KubernetesPendingNote } from "@/components/KubernetesPendingNote";
import {
  ApiError,
  type KubernetesChange,
  type KubernetesChangesPage,
  type KubernetesHelmRelease,
  type KubernetesGraph,
  type KubernetesGraphNode,
  type KubernetesIssue,
  type KubernetesIndexStatus,
  type KubernetesOverview,
  type KubernetesResource,
  type KubernetesTopPage,
  type KubernetesTraffic,
  type KubernetesTrafficEdge,
} from "@/lib/api";
import { CursorPagination } from "@/components/Pagination";
import { useCursorPagination } from "@/lib/pagination";
import { type KubernetesExplorerTab } from "./kubernetesExplorerTabs";
import { BreakRow, BreakTimelineLane, CrashJournal, ResourceTimelineChart, TimelineCard } from "./KubernetesBreakTimeline";
import { KubernetesWarnings } from "./KubernetesWarnings";
import { failureWarnings, issueWarnings } from "./kubernetesWarningItems";
import { KubernetesAgentTab } from "./KubernetesAgentTab";
import { KubernetesBreakLogs, KubernetesBreakLogsProvider } from "./KubernetesBreakLogs";

const virtualViewportHeight = 440;
const resourceRowHeight = 58;
const virtualOverscan = 6;
type ResourceSelection = (resource: KubernetesResource, drawerTab?: "timeline" | "logs") => void;

function resourceIdForKind(kind: string): string | null {
  const ids: Record<string, string> = {
    Pod: "core~v1~pods",
    Node: "core~v1~nodes",
    Namespace: "core~v1~namespaces",
    Service: "core~v1~services",
    ConfigMap: "core~v1~configmaps",
    Secret: "core~v1~secrets",
    PersistentVolumeClaim: "core~v1~persistentvolumeclaims",
    Deployment: "apps~v1~deployments",
    ReplicaSet: "apps~v1~replicasets",
    StatefulSet: "apps~v1~statefulsets",
    DaemonSet: "apps~v1~daemonsets",
    Job: "batch~v1~jobs",
    CronJob: "batch~v1~cronjobs",
    Event: "core~v1~events",
    Ingress: "networking.k8s.io~v1~ingresses",
    HorizontalPodAutoscaler: "autoscaling~v2~horizontalpodautoscalers",
    HTTPRoute: "gateway.networking.k8s.io~v1~httproutes",
    Gateway: "gateway.networking.k8s.io~v1~gateways",
  };
  return ids[kind] ?? null;
}

function issueResource(issue: KubernetesIssue): KubernetesResource | null {
  const resource_id = resourceIdForKind(issue.root.kind);
  if (!resource_id) return null;
  return {
    resource_id,
    api_version: issue.root.api_version,
    kind: issue.root.kind,
    namespace: issue.root.namespace,
    name: issue.root.name,
    uid: issue.root.uid,
  };
}

function asArray<T>(value: T[] | null | undefined): T[] {
  return Array.isArray(value) ? value : [];
}

function Quantity({ value, kind }: { value?: string; kind: "cpu" | "memory" }) {
  if (!value) return <>Unavailable</>;
  const number = quantityValue(value);
  if (number === null) return <>{value}</>;
  if (kind === "cpu") {
    if (Math.abs(number) < 1) return <>{(number * 1000).toLocaleString(undefined, { maximumFractionDigits: 3 })} mCPU</>;
    return <>{number.toLocaleString(undefined, { maximumFractionDigits: 3 })} {Math.abs(number) === 1 ? "core" : "cores"}</>;
  }
  if (kind === "memory") {
    const units = [["TiB", 2 ** 40], ["GiB", 2 ** 30], ["MiB", 2 ** 20], ["KiB", 2 ** 10], ["B", 1]] as const;
    const [unit, divisor] = units.find(([, factor]) => Math.abs(number) >= factor) ?? units[units.length - 1];
    return <>{(number / divisor).toLocaleString(undefined, { maximumFractionDigits: 2 })} {unit}</>;
  }
  return <>Unavailable</>;
}

const overviewTones = {
  accent: { rail: "from-accent/80", chip: "bg-accent/10 text-link" },
  critical: { rail: "from-sev-critical-solid/80", chip: "bg-sev-critical/10 text-sev-critical" },
  warn: { rail: "from-sev-warn-solid/80", chip: "bg-sev-warn/10 text-sev-warning" },
  ok: { rail: "from-sev-ok-solid/80", chip: "bg-sev-ok/10 text-sev-ok" },
} as const;


const quantityFactors: Record<string, number> = { "": 1, n: 1e-9, u: 1e-6, µ: 1e-6, m: 1e-3, k: 1e3, K: 1e3, M: 1e6, G: 1e9, T: 1e12, P: 1e15, E: 1e18, Ki: 2 ** 10, Mi: 2 ** 20, Gi: 2 ** 30, Ti: 2 ** 40, Pi: 2 ** 50, Ei: 2 ** 60 };

function quantityValue(value?: string): number | null {
  const input = value?.trim();
  if (!input) return null;
  const rational = input.match(/^([+-]?\d+)\/([+-]?\d+)$/);
  if (rational) {
    const denominator = Number(rational[2]);
    const result = Number(rational[1]) / denominator;
    return denominator !== 0 && Number.isFinite(result) ? result : null;
  }
  const match = input.match(/^([+-]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][+-]?\d+)?)(n|u|µ|m|k|K|M|G|T|P|E|Ki|Mi|Gi|Ti|Pi|Ei)?$/);
  if (!match) return null;
  const result = Number(match[1]) * quantityFactors[match[2] ?? ""];
  return Number.isFinite(result) ? result : null;
}

function formatUsageWindow(value?: string): string {
  if (!value) return "sample";
  const duration = value.match(/^(?:(\d+(?:\.\d+)?)h)?(?:(\d+(?:\.\d+)?)m)?(?:(\d+(?:\.\d+)?)s)?$/);
  if (!duration || !duration[0]) return value;
  const seconds = Number(duration[1] ?? 0) * 3600 + Number(duration[2] ?? 0) * 60 + Number(duration[3] ?? 0);
  return `${Math.round(seconds)} s`;
}

export function PartialFailuresDisclosure({ failures }: { failures?: Array<{ resource_id?: string; scope?: string; class: string }> | null }) {
  const { cluster } = useKubernetesCluster();
  return <KubernetesWarnings warnings={failureWarnings(failures)} scope={JSON.stringify([cluster])} />;
}


function OverviewCard({ title, icon, tone = "accent", meta, action, className = "", bodyClassName = "", scrollLabel, delay = 0, children }: {
  title: string;
  icon: ReactNode;
  tone?: keyof typeof overviewTones;
  meta?: ReactNode;
  action?: { label: string; onClick: () => void };
  className?: string;
  bodyClassName?: string;
  scrollLabel?: string;
  delay?: number;
  children: ReactNode;
}) {
  const palette = overviewTones[tone];
  return (
    <article className={`rise-in card group relative flex min-w-0 flex-col overflow-hidden transition-[border-color,box-shadow] duration-200 hover:border-ink-400/70 hover:shadow-overlay ${className}`} style={{ animationDelay: `${delay}ms` }}>
      <span aria-hidden="true" className={`pointer-events-none absolute inset-x-0 top-0 h-0.5 bg-gradient-to-r ${palette.rail} to-transparent`} />
      <header className="card-header gap-3">
        <h2 className="card-title flex min-w-0 items-center gap-2">
          <span className={`inline-flex size-7 shrink-0 items-center justify-center rounded-control ${palette.chip}`}>{icon}</span>
          <span className="truncate">{title}</span>
        </h2>
        {meta}
      </header>
      <div role={scrollLabel ? "region" : undefined} aria-label={scrollLabel} tabIndex={scrollLabel ? 0 : undefined} className={`min-h-0 min-w-0 flex-1 ${bodyClassName}`}>{children}</div>
      {action && (
        <button type="button" onClick={action.onClick} className="flex w-full items-center justify-between gap-2 border-t border-ink-500/40 px-4 py-2.5 text-left text-xs font-medium text-link transition-colors duration-150 hover:bg-ink-600/30">
          {action.label}
          <ArrowRight size={14} aria-hidden="true" className="transition-transform duration-200 group-hover:translate-x-0.5" />
        </button>
      )}
    </article>
  );
}

function CardSkeleton({ label, rows = 3 }: { label: string; rows?: number }) {
  return (
    <div role="status" aria-label={label} className="space-y-2.5 p-4">
      {Array.from({ length: rows }, (_, index) => <div key={index} className="sk h-4" style={{ width: `${88 - index * 14}%` }} />)}
    </div>
  );
}

function CardNote({ children, tone = "muted" }: { children: ReactNode; tone?: "muted" | "warn" }) {
  return <p role="status" className={`p-4 text-xs ${tone === "warn" ? "text-sev-warning" : "text-ink-400"}`}>{children}</p>;
}

function ExplorerHeader({ icon, title, description, trailing, controlRow = false }: { icon: ReactNode; title: string; description?: string; trailing?: ReactNode; controlRow?: boolean }) {
  return <header className={`card-header gap-3 ${controlRow ? "flex-col items-stretch sm:flex-row sm:items-center sm:flex-nowrap" : "flex-wrap"}`}>
    <div className="flex min-w-0 items-center gap-3">
      <span aria-hidden="true" className="inline-flex size-8 shrink-0 items-center justify-center rounded-control bg-accent/10 text-link">{icon}</span>
      <div className="min-w-0"><h2 className="card-title">{title}</h2>{description && <p className="mt-1 text-2xs text-ink-400">{description}</p>}</div>
    </div>
    {trailing}
  </header>;
}

function statusTone(value: string): string {
  const normalized = value.toLowerCase().replaceAll("_", " ");
    if (/healthy|ready|synced|fresh|succeeded|completed|available|deployed|current/.test(normalized)) return "pill-good";
    if (/failed|degraded|unhealthy|stale|error|missing|out.?of.?sync/.test(normalized)) return "pill-bad";
  if (/warning|progress|pending|suspend/.test(normalized)) return "pill-warn";
  return "pill-accent";
}

function severityTone(value: string): string {
  return value === "critical" ? "pill-bad" : value === "warning" ? "pill-warn" : "pill-accent";
}

function resourceKindTone(kind: string): string {
  if (["Ingress", "HTTPRoute", "Gateway"].includes(kind)) return "text-link";
  if (kind === "Service") return "text-sev-info";
  if (["Deployment", "ReplicaSet", "StatefulSet", "DaemonSet", "Job", "CronJob", "HorizontalPodAutoscaler"].includes(kind)) return "text-accent-300";
  if (["ConfigMap", "Secret", "PersistentVolumeClaim"].includes(kind)) return "text-sev-warning";
  return "text-sev-ok";
}

function graphHealthTone(health?: string): string {
  const normalized = health?.toLowerCase() ?? "";
  if (/healthy|ready|running|available/.test(normalized)) return "bg-sev-ok/10 text-sev-ok";
  if (/failed|unhealthy|error|crash/.test(normalized)) return "bg-sev-critical/10 text-sev-critical";
  if (/warning|degraded|pending|progress/.test(normalized)) return "bg-sev-warn/10 text-sev-warning";
  return "bg-ink-700 text-ink-300";
}

function relationshipTone(type: string): string {
  if (["manages", "scales"].includes(type)) return "text-link";
  if (["exposes", "routes-to"].includes(type)) return "text-sev-info";
  return "text-sev-warning";
}


function OverviewTopology({ overview, indexStatus, unavailable, onFullTopology }: { overview: KubernetesOverview; indexStatus: KubernetesIndexStatus | null; unavailable: boolean; onFullTopology: () => void }) {
  const overviewFields = { Node: "nodes", Pod: "pods", Namespace: "namespaces" } as const;
  const kinds = [...new Set(["Namespace", "Node", "Ingress", "Gateway", "HTTPRoute", "Service", "Deployment", "ReplicaSet", "StatefulSet", "DaemonSet", "Job", "CronJob", "Pod", "HorizontalPodAutoscaler", "ConfigMap", "Secret", "PersistentVolumeClaim", "Event", ...Object.keys(indexStatus?.kinds ?? {})])];
  const missingCount = { display: "0", reported: false };
  const countFor = (kind: string) => {
    if (unavailable) return missingCount;
    const indexed = indexStatus?.kinds?.[kind];
    if (indexed) {
      if (!Number.isSafeInteger(indexed.records) || indexed.records < 0 || !["ready", "stale", "partial"].includes(indexed.state) || indexed.error) return missingCount;
      return { display: indexed.records.toLocaleString(), reported: true };
    }
    const field = overviewFields[kind as keyof typeof overviewFields];
    if (!field) return missingCount;
    const resourceId = resourceIdForKind(kind);
    if (overview.partial_failures?.some((failure) => !failure.resource_id || failure.resource_id === resourceId) || overview.omitted_categories?.some((category) => category === kind || category === resourceId || category === field)) return missingCount;
    const count = overview[field];
    return Number.isSafeInteger(count) && count >= 0 ? { display: count.toLocaleString(), reported: true } : missingCount;
  };
  const workloadKinds = ["Deployment", "ReplicaSet", "StatefulSet", "DaemonSet", "Job", "CronJob"];
  const showWorkloads = workloadKinds.some((kind) => !countFor(kind).reported || /partial|stale/.test(countFor(kind).display));
  const workloadUnavailable = unavailable || !Number.isSafeInteger(overview.workloads) || overview.workloads < 0 || overview.partial_failures?.some((failure) => !failure.resource_id || workloadKinds.some((kind) => failure.resource_id === resourceIdForKind(kind))) || overview.omitted_categories?.some((category) => category === "workloads" || workloadKinds.some((kind) => category === kind || category === resourceIdForKind(kind)));
  const counts = kinds.map((kind) => ({ kind, ...countFor(kind) }));
  if (showWorkloads) counts.push({ kind: "Workloads (aggregate)", display: workloadUnavailable ? "0" : overview.workloads.toLocaleString(), reported: !workloadUnavailable });
  const resourceColor = (kind: string) => kind === "Ingress" || kind === "Gateway" || kind === "HTTPRoute" ? "text-accent" : kind === "Service" || kind === "Node" ? "text-sev-info" : kind === "ConfigMap" || kind === "Secret" || kind === "PersistentVolumeClaim" ? "text-sev-warning" : "text-sev-ok";
  return <section aria-label="Overview topology" className="min-w-0">
    <OverviewCard title="Topology" icon={<Network size={15} aria-hidden="true" />} tone="ok" bodyClassName="dark-scroll max-h-[26rem] overflow-y-auto overscroll-contain" scrollLabel="Topology resource counts" meta={<button type="button" className="btn shrink-0" aria-label="View all relationships" onClick={onFullTopology}><Network size={14} aria-hidden="true" />Full topology</button>}>
      <div className="min-w-0 p-4 sm:p-5">
        <div aria-label="Cluster resource counts" className="min-w-0 space-y-4">
          {[true, false].map((reported) => {
            const entries = counts.filter((count) => count.reported === reported);
            if (!entries.length) return null;
            if (!reported) return <p key="unreported" className="text-2xs text-ink-400">+{entries.length} kinds not reported</p>;
            return <div key={String(reported)} className={reported ? "" : "border-t border-ink-500/40 pt-3"}>
              <h3 className="mb-2 text-2xs font-medium uppercase tracking-wider text-ink-400">{reported ? "Reported resources" : "Not reported"}</h3>
              <dl aria-label="Reported resource counts" className="grid grid-cols-1 gap-x-5 gap-y-1">
                {entries.map(({ kind, display }) => <div key={kind} className="grid min-w-0 grid-cols-[minmax(0,1fr)_auto] items-center gap-3 py-1">
                  <dt title={kind} className="flex min-w-0 items-center gap-2.5 text-xs text-ink-200"><span aria-hidden="true" className={`size-2 shrink-0 rounded-full bg-current ${resourceColor(kind)}`} /><span className="truncate">{kind}</span>{indexStatus?.kinds?.[kind]?.error && <span aria-label={`${kind} could not be read`} className="size-1.5 shrink-0 rounded-full bg-ink-400" />}</dt>
                  <dd title={reported ? undefined : "Count not reported"} aria-label={reported ? undefined : `${kind}: count not reported, displayed as zero`} className={`shrink-0 text-sm font-semibold tabular-nums ${reported ? "text-ink-100" : "text-ink-400"}`}>{display}</dd>
                </div>)}
              </dl>
            </div>;
          })}
        </div>
      </div>
    </OverviewCard>
  </section>;
}

export function KubernetesOverviewTimeline({ overview, onSelectResource, onSelectView }: { overview: KubernetesOverview; onSelectResource: ResourceSelection; onSelectView: (tab: KubernetesExplorerTab) => void }) {
  const { client: api } = useKubernetesCluster();
  const [windowEnd, setWindowEnd] = useState(() => Date.now());
  const changes = useQuery({
    queryKey: ["kubernetes-changes", "overview", overview.cluster_id],
    queryFn: () => {
      const until = Date.now();
      return api.kubernetesChanges({ since: new Date(until - 60 * 60_000).toISOString(), until: new Date(until).toISOString(), limit: 100 });
    },
    retry: false,
  });
  const { refetch: refetchChanges } = changes;
  useEffect(() => {
    const timer = window.setInterval(() => {
      setWindowEnd(Date.now());
      void refetchChanges();
    }, 30_000);
    return () => window.clearInterval(timer);
  }, [refetchChanges]);

  const denied = changes.error instanceof ApiError && [401, 403].includes(changes.error.status);
  return <><TimelineCard changes={denied ? undefined : changes.data} windowEnd={windowEnd} clusterId={overview.cluster_id} onSelectResource={onSelectResource} onSelectView={onSelectView} resourceFor={issueResource} warningsOwned />{changes.isError && <p role="status" className="text-xs text-sev-warning">Recent change history unavailable.<button type="button" className="ml-2 text-link underline" onClick={() => void changes.refetch()}>Retry</button></p>}</>;
}

export function KubernetesOverviewInsights({ overview, indexStatus, unavailable, onSelectView }: { overview: KubernetesOverview; indexStatus: KubernetesIndexStatus | null; unavailable: boolean; onSelectView: (tab: KubernetesExplorerTab) => void }) {
  const { client: api } = useKubernetesCluster();
  const top = useQuery({ queryKey: ["kubernetes-top", "pod", "cpu", 5], queryFn: () => api.kubernetesTop({ kind: "pod", sort: "cpu", limit: 5 }), retry: false });
  const releases = useQuery({ queryKey: ["kubernetes-releases", "overview"], queryFn: () => api.kubernetesReleases({ limit: 5 }), retry: false });
  const traffic = useQuery({ queryKey: ["kubernetes-traffic", "", "15m"], queryFn: () => api.kubernetesTraffic({ window: "15m" }), retry: false });
  const releaseList = asArray(releases.data?.items);
  const unhealthyReleases = releaseList.filter((release) => release.health !== "healthy");
  const trafficEdges = traffic.data?.available ? [...asArray(traffic.data.edges)].sort((left, right) => (right.rate_per_sec ?? 0) - (left.rate_per_sec ?? 0)) : [];

  return (
    <section aria-label="Triage summary" className="grid min-w-0 gap-4 md:grid-cols-2 xl:grid-cols-3">
      <OverviewCard
        title="Top pod usage"
        icon={<Cpu size={14} aria-hidden="true" />}
        meta={top.data && <span className={`pill ${top.data.fresh ? "pill-good" : ""}`}>{top.data.fresh ? "Fresh" : top.data.availability}</span>}
        delay={40}
      >
        {top.isPending && !top.data && <CardSkeleton label="Loading usage ranking" rows={4} />}
        {top.isError && (!top.data || top.error instanceof ApiError && [401, 403].includes(top.error.status)) && <CardNote>Usage ranking unavailable.</CardNote>}
        {top.data && !(top.error instanceof ApiError && [401, 403].includes(top.error.status)) && <TopUsageRows data={top.data} compact />}
      </OverviewCard>

      <OverviewCard
        title="Helm releases"
        icon={<Layers3 size={14} aria-hidden="true" />}
        tone={unhealthyReleases.length > 0 ? "warn" : "accent"}
        meta={releaseList.length > 0 && <span className={`pill ${unhealthyReleases.length > 0 ? "pill-warn" : "pill-good"}`}>{releaseList.length - unhealthyReleases.length}/{releaseList.length} healthy</span>}
        action={{ label: "View all releases", onClick: () => onSelectView("releases") }}
        delay={160}
      >
        {releases.isPending && !releases.data && <CardSkeleton label="Loading release summary" />}
        {releases.isError && (!releases.data || releases.error instanceof ApiError && [401, 403].includes(releases.error.status)) && <CardNote>Release summary unavailable.</CardNote>}
        {releases.data && !(releases.error instanceof ApiError && [401, 403].includes(releases.error.status)) && (releaseList.length === 0 ? <CardNote>No Helm release labels were found.</CardNote> : (
          <ul className="divide-y divide-ink-700">
            {[...unhealthyReleases, ...releaseList.filter((release) => release.health === "healthy")].slice(0, 4).map((release) => (
              <li key={`${release.namespace}:${release.name}`} className="flex items-center gap-3 px-4 py-2.5">
                <span className="min-w-0 flex-1"><span className="block truncate text-xs font-medium text-ink-100">{release.name}</span><span className="block truncate text-2xs text-ink-400">{release.namespace} <span aria-hidden="true">|</span> Revision {release.current.revision}</span></span>
                <span className={`pill ${release.health === "healthy" ? "pill-good" : "pill-warn"}`}>{release.health}</span>
              </li>
            ))}
          </ul>
        ))}
      </OverviewCard>

      <OverviewTopology overview={overview} indexStatus={indexStatus} unavailable={unavailable} onFullTopology={() => onSelectView("topology")} />

      <OverviewCard
        title="Service traffic"
        icon={<Waypoints size={14} aria-hidden="true" />}
        meta={traffic.data?.available ? <span className="pill">{trafficFreshness(traffic.data).label}</span> : traffic.data ? <span className="pill">{trafficUnavailableLabel(traffic.data.reason) === "License required" ? "License required" : "Not connected"}</span> : undefined}
        action={{ label: "View all traffic", onClick: () => onSelectView("traffic") }}
        className="md:col-span-2 xl:col-span-3"
        delay={200}
      >
        {traffic.isPending && !traffic.data && <CardSkeleton label="Loading traffic summary" />}
        {traffic.isError && (!traffic.data || traffic.error instanceof ApiError && [401, 403].includes(traffic.error.status)) && <CardNote>Traffic status not reported.</CardNote>}
        {traffic.data && !(traffic.error instanceof ApiError && [401, 403].includes(traffic.error.status)) && !traffic.data.available && (
          <div className="flex items-start gap-3 p-4">
            <span aria-hidden="true" className="mt-0.5 inline-flex size-8 shrink-0 items-center justify-center rounded-full border border-dashed border-ink-500 text-ink-400"><Waypoints size={14} /></span>
            <p className="text-xs leading-5 text-ink-300">{trafficUnavailableLabel(traffic.data.reason) === "License required" ? "Service-to-service flows need a licensed telemetry source." : "Connect a flow source to see service-to-service request rates, errors, and latency."}</p>
          </div>
        )}
        {traffic.data?.available && !(traffic.error instanceof ApiError && [401, 403].includes(traffic.error.status)) && (trafficEdges.length === 0 ? <CardNote>No service flows were observed in this window.</CardNote> : (
          <ul className="divide-y divide-ink-700">
            {trafficEdges.slice(0, 4).map((edge, index) => (
              <li key={`${trafficWorkloadLabel(edge.from)}:${trafficWorkloadLabel(edge.to)}:${index}`} className="grid grid-cols-[minmax(0,1fr)_auto] items-center gap-3 px-4 py-2.5 text-xs">
                <span className="flex min-w-0 items-center gap-2"><span className="truncate text-ink-100">{edge.from.name}</span><ArrowRight size={12} aria-label="to" className="shrink-0 text-ink-400" /><span className="truncate text-ink-100">{edge.to.name}</span></span>
                <span className="tabular-nums text-ink-300">{trafficMetric(edge.rate_per_sec, 2, " req/s")}</span>
              </li>
            ))}
          </ul>
        ))}
      </OverviewCard>
    </section>
  );
}

function TopUsageRows({ data, compact = false }: { data: KubernetesTopPage; compact?: boolean }) {
  const [sort, setSort] = useState<"cpu" | "memory">("cpu");
  const rows = compact ? data.items.slice(0, 5) : data.items;
  const peak = Math.max(0, ...rows.map((row) => quantityValue(sort === "cpu" ? row.cpu : row.memory) ?? 0));
  return (
    <>
      {!compact && <div className="flex items-center justify-between border-b border-ink-700 px-4 py-2"><p className="text-xs text-ink-400">{data.availability} <span aria-hidden="true">|</span> {data.fresh ? "fresh sample" : "sample may be stale"}</p><div role="group" aria-label="Usage sort" className="flex gap-1">{(["cpu", "memory"] as const).map((field) => <button type="button" aria-pressed={sort === field} onClick={() => setSort(field)} className={`rounded-control px-2 py-1 text-xs ${sort === field ? "bg-accent-subtle text-ink-50" : "text-ink-300 hover:bg-ink-800"}`} key={field}>{field === "cpu" ? "CPU" : "Memory"}</button>)}</div></div>}
      {!data.items_available ? <p role="status" className="p-4 text-xs text-ink-400">Usage ranking unavailable; no list was returned.</p> : rows.length === 0 ? <p role="status" className="p-4 text-xs text-ink-400">Usage metrics are {data.availability}.</p> : <div className="divide-y divide-ink-700">{rows.map((row) => {
        const amount = quantityValue(sort === "cpu" ? row.cpu : row.memory);
        return <div key={`${row.namespace}:${row.name}`} className="grid grid-cols-[minmax(0,1fr)_auto] items-center gap-x-3 gap-y-1.5 px-4 py-3"><span className="min-w-0"><span className="block truncate text-xs font-medium text-ink-100">{row.namespace ? `${row.namespace}/` : ""}{row.name}</span><span className="block truncate text-2xs text-ink-400">{row.owner ? `${row.owner.kind} ` : ""}{formatUsageWindow(row.window)}</span></span><span className="text-right text-xs tabular-nums text-ink-200">{compact ? <><span className="block">CPU <Quantity value={row.cpu} kind="cpu" /></span><span className="block text-2xs text-ink-400">Memory <Quantity value={row.memory} kind="memory" /></span></> : <><span className="block"><Quantity value={sort === "cpu" ? row.cpu : row.memory} kind={sort} /></span><span className="block text-2xs text-ink-400">request <Quantity value={sort === "cpu" ? row.request_cpu : row.request_memory} kind={sort} /></span></> }</span>{compact && amount !== null && peak > 0 && <span aria-hidden="true" className="col-span-2 h-1 overflow-hidden rounded-full bg-ink-600"><span className="block h-full rounded-full bg-gradient-to-r from-accent to-link transition-[width] duration-500" style={{ width: `${Math.max(3, (amount / peak) * 100)}%` }} /></span>}</div>;
      })}</div>}
      {!compact && <PartialFailuresDisclosure failures={data.partial_failures} />}
    </>
  );
}

export function KubernetesExplorerTabContent({ tab, onSelectResource, clusterId }: { tab: KubernetesExplorerTab; onSelectResource: ResourceSelection; clusterId?: string }) {
  switch (tab) {
    case "overview": return null;
    case "nodes": return null;
    case "issues": return <IssuesView onSelectResource={onSelectResource} clusterId={clusterId} />;
    case "resources": return <ResourcesView onSelectResource={onSelectResource} />;
    case "releases": return <ReleasesView />;
    case "gitops": return <GitOpsView />;
    case "timeline": return <TimelineView onSelectResource={onSelectResource} />;
    case "topology": return <KubernetesGraphView onSelectResource={onSelectResource} />;
    case "traffic": return <KubernetesTrafficView />;
    case "agent": return <KubernetesAgentTab clusterName={clusterId} />;
  }
}

const trafficWindowMilliseconds = { "5m": 5 * 60_000, "15m": 15 * 60_000, "1h": 60 * 60_000 } as const;

function trafficFreshness(data: KubernetesTraffic) {
  const observedAt = isZeroTime(data.observed_at) ? Number.NaN : Date.parse(data.observed_at ?? "");
  if (!Number.isFinite(observedAt)) return { label: "Freshness unknown", observed: "Observation time unavailable" };
  const age = Math.max(0, Date.now() - observedAt);
  const stale = age > (trafficWindowMilliseconds[data.window as keyof typeof trafficWindowMilliseconds] ?? trafficWindowMilliseconds["15m"]);
  return {
    label: stale ? "Stale" : "Fresh",
    observed: `Observed ${new Date(observedAt).toLocaleString()}`,
  };
}

function trafficUnavailableLabel(reason?: string) {
  return /licen[cs]e|entitlement/i.test(reason ?? "") ? "License required" : "Unavailable";
}

function trafficWorkloadLabel(workload: KubernetesTrafficEdge["from"]) {
  return `${workload.kind} ${workload.namespace ? `${workload.namespace}/` : ""}${workload.name}`;
}

function trafficMetric(value: number | null | undefined, maximumFractionDigits: number, suffix: string, multiplier = 1) {
  if (value == null || !Number.isFinite(value)) return "Unavailable";
  return `${(value * multiplier).toLocaleString(undefined, { maximumFractionDigits })}${suffix}`;
}

function KubernetesTrafficView() {
  const { client: api } = useKubernetesCluster();
  const [namespace, setNamespace] = useState("");
  const [window, setWindow] = useState<"5m" | "15m" | "1h">("15m");
  const traffic = useQuery({
    queryKey: ["kubernetes-traffic", namespace, window],
    queryFn: () => api.kubernetesTraffic({ namespace: namespace.trim() || undefined, window }),
    retry: false,
    refetchInterval: 30_000,
  });
  const data = traffic.data;
  const freshness = data?.available ? trafficFreshness(data) : null;
  const errorIsAccessGate = traffic.error instanceof ApiError && [402, 403].includes(traffic.error.status);

  return <section className="card min-w-0 overflow-hidden transition-[border-color,box-shadow] duration-200 motion-reduce:transition-none" aria-label="Kubernetes traffic flows">
    <ExplorerHeader icon={<Activity size={15} />} title="Service traffic" description="Observed service-to-service flows" trailing={<div className="flex flex-wrap items-center gap-2">
        <label className="sr-only" htmlFor="kubernetes-traffic-namespace">Traffic namespace</label>
        <NamespaceSelect id="kubernetes-traffic-namespace" label="Traffic namespace" value={namespace} onChange={setNamespace} />
        <label className="sr-only" htmlFor="kubernetes-traffic-window">Traffic window</label>
        <select id="kubernetes-traffic-window" className="input w-28" value={window} onChange={(event) => setWindow(event.target.value as typeof window)}>
          <option value="5m">5 minutes</option><option value="15m">15 minutes</option><option value="1h">1 hour</option>
        </select>
        <button type="button" className="btn-icon" aria-label="Refresh traffic" title="Refresh traffic" onClick={() => void traffic.refetch()}><RotateCcw size={14} /></button>
      </div>} />
    {traffic.isPending && <CardSkeleton label="Loading observed traffic flows" rows={5} />}
    {traffic.isError && <div role="status" className="p-4"><p className="text-sm font-medium text-sev-warning">{errorIsAccessGate ? "License or access required" : "Traffic unavailable"}</p><p className="mt-1 break-words text-xs text-ink-400">{traffic.error instanceof Error ? traffic.error.message : "The traffic source could not be queried."}</p></div>}
    {data && !data.available && <div role="status" className="p-5"><span className="pill">{trafficUnavailableLabel(data.reason)}</span><p className="mt-3 max-w-3xl break-words text-sm leading-6 text-ink-200">{data.reason || "No supported traffic source is connected."}</p></div>}
    {data?.available && <>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2 border-b border-ink-700 px-4 py-3 text-xs">
        <span className={`pill ${statusTone(freshness?.label ?? "unknown")}`}>{freshness?.label}</span>
        <span className="text-ink-300">Source: {data.source || "not reported"}</span>
        <span className="text-ink-400">{data.window} window</span><span className="text-ink-400">{freshness?.observed}</span>
      </div>
      {data.edges?.length ? <div className="max-h-[min(55vh,36rem)] max-w-full overflow-auto">
        <table className="w-full min-w-[760px] text-left text-xs">
          <thead className="border-b border-ink-700 text-2xs uppercase text-ink-400"><tr><th scope="col" className="px-4 py-2">From</th><th scope="col" className="px-4 py-2">To</th><th scope="col" className="px-4 py-2 text-right">Rate</th><th scope="col" className="px-4 py-2 text-right">Errors</th><th scope="col" className="px-4 py-2 text-right">P95</th><th scope="col" className="px-4 py-2 text-right">Bytes/s</th></tr></thead>
          <tbody className="divide-y divide-ink-700">{data.edges.map((edge, index) => <tr key={`${edge.from.namespace}/${edge.from.name}:${edge.to.namespace}/${edge.to.name}:${index}`}><th scope="row" className="max-w-64 truncate px-4 py-3 font-medium text-ink-100">{trafficWorkloadLabel(edge.from)}</th><td className="max-w-64 truncate px-4 py-3 text-ink-200">{trafficWorkloadLabel(edge.to)}</td><td className="px-4 py-3 text-right tabular-nums text-ink-200">{trafficMetric(edge.rate_per_sec, 2, " req/s")}</td><td className="px-4 py-3 text-right tabular-nums text-ink-200">{trafficMetric(edge.error_rate, 2, "%", 100)}</td><td className="px-4 py-3 text-right tabular-nums text-ink-200">{trafficMetric(edge.p95_ms, 1, " ms")}</td><td className="px-4 py-3 text-right tabular-nums text-ink-200">{trafficMetric(edge.bytes_per_sec, 1, "")}</td></tr>)}</tbody>
        </table>
      </div> : <p role="status" className="p-5 text-sm text-ink-400">No service flows were observed in this window.</p>}
      <footer className="flex flex-wrap gap-x-5 gap-y-2 border-t border-ink-700 px-4 py-3 text-2xs text-ink-400">
        <span>{data.unmapped} unmapped flows</span>
        {data.external?.length ? <span>External destinations: {data.external.join(", ")}</span> : null}
        {data.truncated && <span className="text-sev-warning">Flow results are truncated.</span>}
      </footer>
    </>}
  </section>;
}

const graphColumnWidth = 292;
const graphNodeWidth = 184;
const graphNodeHeight = 72;
const graphRowHeight = 120;
const graphEdgeLabelLimit = 400;
const graphViewportOverscanX = graphColumnWidth;
const graphViewportOverscanY = graphRowHeight * 2;
const graphKindColumn: Record<string, number> = {
  Ingress: 0,
  HTTPRoute: 0,
  Gateway: 0,
  Service: 1,
  HorizontalPodAutoscaler: 2,
  Deployment: 2,
  ReplicaSet: 2,
  StatefulSet: 2,
  DaemonSet: 2,
  Job: 2,
  CronJob: 2,
  ConfigMap: 3,
  Secret: 3,
  PersistentVolumeClaim: 3,
  Pod: 4,
  Node: 4,
  Namespace: 4,
};
const graphColumnLabels = ["Routes", "Services", "Workloads", "Configuration", "Runtime"];

function resourceFromGraphNode(node: KubernetesGraphNode): KubernetesResource | null {
  const resource_id = resourceIdForKind(node.kind);
  return resource_id ? { resource_id, kind: node.kind, namespace: node.namespace, name: node.name } : null;
}

function graphLayout(graph: KubernetesGraph) {
  const allColumns = graphColumnLabels.map((label) => ({ label, nodes: [] as KubernetesGraphNode[] }));
  for (const node of graph.nodes) allColumns[graphKindColumn[node.kind] ?? 2].nodes.push(node);
  const columns = allColumns.filter((column) => column.nodes.length > 0);
  for (const column of columns) {
    column.nodes.sort((left, right) => `${left.namespace ?? ""}/${left.kind}/${left.name}`.localeCompare(`${right.namespace ?? ""}/${right.kind}/${right.name}`));
  }
  const positions = new Map<string, { x: number; y: number }>();
  columns.forEach((column, columnIndex) => column.nodes.forEach((node, rowIndex) => {
    positions.set(node.id, { x: columnIndex * graphColumnWidth + 18, y: 54 + rowIndex * graphRowHeight });
  }));
  const rowCount = Math.max(0, ...columns.map((column) => column.nodes.length));
  return { positions, width: Math.max(1, columns.length) * graphColumnWidth + 20, height: Math.max(450, 90 + rowCount * graphRowHeight), columns };
}

const defaultGraphKinds = ["Ingress", "Service", "Deployment", "Pod"];
const graphFilterGroups = [
  { label: "Networking", kinds: ["Ingress", "HTTPRoute", "Gateway", "Service"] },
  { label: "Workloads", kinds: ["Deployment", "ReplicaSet", "StatefulSet", "DaemonSet", "Job", "CronJob", "HorizontalPodAutoscaler", "Pod"] },
  { label: "Configuration", kinds: ["ConfigMap", "Secret", "PersistentVolumeClaim"] },
];

function KubernetesGraphView({ onSelectResource, preview = false, onFullTopology }: { onSelectResource: ResourceSelection; preview?: boolean; onFullTopology?: () => void }) {
  const { client: api } = useKubernetesCluster();
  const [namespace, setNamespace] = useState("");
  const [namespaceSearch, setNamespaceSearch] = useState("");
  const [selectedKinds, setSelectedKinds] = useState<string[] | null>(defaultGraphKinds);
  const [zoom, setZoom] = useState(1);
  const [scrollOffset, setScrollOffset] = useState({ left: 0, top: 0 });
  const [viewportSize, setViewportSize] = useState({ width: 1200, height: 700 });
  const viewportRef = useRef<HTMLDivElement>(null);
  const namespaces = useQuery({ queryKey: ["kubernetes-namespaces"], queryFn: () => api.kubernetesNamespaces(), enabled: !preview, retry: false });
  const graph = useQuery({
    queryKey: preview ? ["kubernetes-overview-topology"] : ["kubernetes-graph", namespace, "complete"],
    queryFn: () => preview ? api.kubernetesOverviewGraph() : api.kubernetesGraph({ namespace, connected_only: true, complete: true }),
    enabled: preview || namespace !== "",
    retry: false,
    staleTime: preview ? Infinity : 0,
    refetchInterval: (query) => query.state.data?.sync?.pending?.length ? 5000 : false,
  });
  const graphMalformed = Boolean(graph.data && (!Array.isArray(graph.data.nodes) || !Array.isArray(graph.data.edges)));
  const graphData = graph.isError || graphMalformed ? undefined : graph.data;
  const graphAccessDenied = graph.error instanceof ApiError && [401, 402, 403].includes(graph.error.status);
  const namespaceNames = useMemo(() => [...new Set(namespaces.data?.items ?? [])].sort(), [namespaces.data]);
  const availableKinds = useMemo(() => [...new Set((graphData?.nodes ?? []).map((node) => node.kind))].sort(), [graphData]);
  const pendingKinds = useMemo(() => pendingKindNames(graphData?.sync?.pending), [graphData]);
  const kindCounts = useMemo(() => {
    const counts = new Map<string, number>();
    for (const node of graphData?.nodes ?? []) counts.set(node.kind, (counts.get(node.kind) ?? 0) + 1);
    return counts;
  }, [graphData]);
  const filterGroups = useMemo(() => {
    const grouped = new Set(graphFilterGroups.flatMap((group) => group.kinds));
    return [...graphFilterGroups, { label: "Other", kinds: availableKinds.filter((kind) => !grouped.has(kind)) }];
  }, [availableKinds]);
  const visibleNodes = useMemo(() => (graphData?.nodes ?? []).filter((node) => preview || selectedKinds === null || selectedKinds.includes(node.kind)), [graphData, selectedKinds, preview]);
  const visibleNodeIds = useMemo(() => new Set(visibleNodes.map((node) => node.id)), [visibleNodes]);
  const visibleEdges = useMemo(() => (graphData?.edges ?? []).filter((edge) => visibleNodeIds.has(edge.from) && visibleNodeIds.has(edge.to)), [graphData, visibleNodeIds]);
  const visibleGraph = useMemo(() => graphData ? { ...graphData, nodes: visibleNodes, edges: visibleEdges } : null, [graphData, visibleNodes, visibleEdges]);
  const layout = useMemo(() => visibleGraph ? graphLayout(visibleGraph) : null, [visibleGraph]);
  const edgeGeometry = useMemo(() => {
    if (!layout) return [];
    return visibleEdges.flatMap((edge) => {
      const from = layout.positions.get(edge.from);
      const to = layout.positions.get(edge.to);
      if (!from || !to) return [];
      const sameColumn = from.x === to.x;
      const forward = from.x <= to.x;
      const startX = sameColumn || forward ? from.x + graphNodeWidth : from.x;
      const endX = sameColumn ? to.x + graphNodeWidth : forward ? to.x : to.x + graphNodeWidth;
      const startY = from.y + graphNodeHeight / 2;
      const endY = to.y + graphNodeHeight / 2;
      const channelX = sameColumn ? from.x + graphNodeWidth + 54 : (from.x + to.x + graphNodeWidth) / 2;
      const labelWidth = Math.min(100, Math.max(48, edge.type.length * 6 + 12));
      const label = visibleEdges.length <= graphEdgeLabelLimit
        ? { x: sameColumn || forward ? from.x + graphNodeWidth + 54 : from.x - 54, y: (startY + endY) / 2 }
        : undefined;
      return [{ edge, startX, startY, endX, endY, channelX, labelWidth, label, minX: Math.min(startX, endX, channelX), maxX: Math.max(startX, endX, channelX), minY: Math.min(startY, endY), maxY: Math.max(startY, endY) }];
    });
  }, [layout, visibleEdges]);
  const viewportBounds = useMemo(() => {
    const width = viewportSize.width / zoom;
    const height = viewportSize.height / zoom;
    return {
      left: scrollOffset.left / zoom - graphViewportOverscanX,
      top: scrollOffset.top / zoom - graphViewportOverscanY,
      right: scrollOffset.left / zoom + width + graphViewportOverscanX,
      bottom: scrollOffset.top / zoom + height + graphViewportOverscanY,
    };
  }, [scrollOffset, viewportSize, zoom]);
  const viewportNodes = useMemo(() => layout ? visibleNodes.filter((node) => {
    const position = layout.positions.get(node.id);
    return position && position.x < viewportBounds.right && position.x + graphNodeWidth > viewportBounds.left && position.y < viewportBounds.bottom && position.y + graphNodeHeight > viewportBounds.top;
  }) : [], [layout, visibleNodes, viewportBounds]);
  const viewportEdges = useMemo(() => edgeGeometry.filter(({ minX, maxX, minY, maxY }) => minX < viewportBounds.right && maxX > viewportBounds.left && minY < viewportBounds.bottom && maxY > viewportBounds.top), [edgeGeometry, viewportBounds]);

  useEffect(() => {
    if (!layout || !viewportRef.current) return;
    const viewport = viewportRef.current;
    const fit = Math.min(1, (viewport.clientWidth - 32) / layout.width, (viewport.clientHeight - 32) / layout.height);
    setZoom(Math.max(0.2, fit));
    viewport.scrollLeft = 0;
    viewport.scrollTop = 0;
    setScrollOffset({ left: 0, top: 0 });
    setViewportSize({ width: viewport.clientWidth || 1200, height: viewport.clientHeight || 700 });
  }, [namespace, layout]);

  useEffect(() => {
    const viewport = viewportRef.current;
    if (!viewport) return;
    const measure = () => setViewportSize({ width: viewport.clientWidth || 1200, height: viewport.clientHeight || 700 });
    window.addEventListener("resize", measure);
    return () => window.removeEventListener("resize", measure);
  }, [graphData]);

  const resetGraphScroll = () => {
    if (viewportRef.current) {
      viewportRef.current.scrollLeft = 0;
      viewportRef.current.scrollTop = 0;
    }
    setScrollOffset({ left: 0, top: 0 });
  };
  const setKindVisible = (kind: string, visible: boolean) => {
    resetGraphScroll();
    const current = new Set(selectedKinds ?? availableKinds);
    if (visible) current.add(kind);
    else current.delete(kind);
    setSelectedKinds([...current]);
  };
  const fitGraph = () => {
    if (!layout || !viewportRef.current) return;
    const viewport = viewportRef.current;
    const fit = Math.min(1, (viewport.clientWidth - 32) / layout.width, (viewport.clientHeight - 32) / layout.height);
    setZoom(Math.max(0.2, fit));
    viewport.scrollLeft = 0;
    viewport.scrollTop = 0;
    setScrollOffset({ left: 0, top: 0 });
  };
  const pan = (left: number, top: number) => {
    if (!viewportRef.current) return;
    viewportRef.current.scrollLeft += left;
    viewportRef.current.scrollTop += top;
    setScrollOffset({ left: viewportRef.current.scrollLeft, top: viewportRef.current.scrollTop });
  };
  const selectNamespace = (value: string) => {
    resetGraphScroll();
    setSelectedKinds(defaultGraphKinds);
    setNamespace(value);
  };

  return <section className={`min-w-0 max-w-full overflow-hidden border-y border-ink-700 ${preview ? "md:col-span-2 xl:col-span-3" : ""}`} aria-label={preview ? "Overview topology" : "Kubernetes topology"}>
    <ExplorerHeader controlRow={!preview} icon={<Network size={15} />} title={preview ? "Topology" : "Projected relationships"} trailing={preview ? <button type="button" className="btn" aria-label="View all relationships" onClick={onFullTopology}><Network size={14} aria-hidden="true" />Full topology</button> : <div role="group" aria-label="Topology namespace controls" className="flex w-full min-w-0 flex-nowrap items-center gap-2 sm:w-auto sm:flex-1 sm:justify-end">
      {namespace && <button type="button" className="btn shrink-0" onClick={() => selectNamespace("")}><ArrowLeft size={14} aria-hidden="true" />Namespaces</button>}
        <label className="sr-only" htmlFor="kubernetes-graph-namespace">Select topology namespace</label>
        <select id="kubernetes-graph-namespace" aria-label="Topology namespace" className="input min-w-0 flex-1 sm:max-w-56" value={namespace} onChange={(event) => selectNamespace(event.target.value)}>
          <option value="">Select a namespace</option>{namespaceNames.map((value) => <option key={value} value={value}>{value}</option>)}
        </select>
        <button type="button" className="btn-icon shrink-0" aria-label={namespace ? "Refresh namespace topology" : "Refresh namespaces"} title={namespace ? "Refresh namespace topology" : "Refresh namespaces"} onClick={() => void (namespace ? graph.refetch() : namespaces.refetch())}><RotateCcw size={14} /></button>
      </div>} />
    {!preview && namespaces.isPending && <CardSkeleton label="Loading Kubernetes namespaces" rows={4} />}
    {!preview && namespaces.isError && <div role="alert" className="p-4 text-sm text-sev-warning"><p>Namespace list unavailable. Check Kubernetes resource-list permission and retry.</p><p className="mt-1 break-words text-xs">{namespaces.error.message}</p><button type="button" className="btn mt-3" onClick={() => void namespaces.refetch()}>Retry namespace list</button></div>}
    {!preview && namespaces.data && namespaceNames.length === 0 && <p role="status" className="p-5 text-sm text-ink-400">No namespaces are available to display.</p>}
    {!preview && namespaces.data && namespaceNames.length > 0 && !namespace && <p role="status" className="p-5 text-sm text-ink-400">Select one of {namespaceNames.length} listed namespaces to load its complete connected graph.</p>}
    {!preview && !namespace && namespaceNames.length > 0 && <div className="p-4">
      <input className="input mb-3 w-full" aria-label="Search topology namespaces" placeholder="Search namespaces" value={namespaceSearch} onChange={(event) => setNamespaceSearch(event.target.value)} />
      <div role="group" aria-label="Namespace blocks" className="grid grid-cols-[repeat(auto-fill,minmax(min(100%,12rem),1fr))] gap-3">
        {namespaceNames.filter((value) => value.toLowerCase().includes(namespaceSearch.toLowerCase())).map((value) => <button type="button" key={value} aria-label={`Open namespace ${value} topology`} onClick={() => selectNamespace(value)} className="flex min-h-28 min-w-0 flex-col items-start justify-center gap-3 rounded-control border border-ink-600 bg-surface-raised p-4 text-left hover:border-accent focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent"><Network size={20} className="text-link" aria-hidden="true" /><span className="w-full break-words text-sm font-semibold text-ink-100 [overflow-wrap:anywhere]">{value}</span></button>)}
      </div>
    </div>}
    {!preview && <PartialFailuresDisclosure failures={[...(namespaces.data?.partial_failures ?? []), ...(graphData?.partial_failures ?? [])]} />}
    {(preview || namespace) && graph.isPending && <CardSkeleton label={`Loading complete topology for ${preview ? "all namespaces" : namespace}`} rows={5} />}
    {(preview || namespace) && graph.isError && <div role="alert" className="p-4 text-sm text-sev-warning"><p>Complete topology for {preview ? "all namespaces" : namespace} is unavailable. {graphAccessDenied ? "Check sign-in, license, and Kubernetes graph permissions, then refresh." : "Check connector access and retry."}</p><p className="mt-1 break-words text-xs">{graph.error.message}</p>{!graphAccessDenied && <button type="button" className="btn mt-3" onClick={() => void graph.refetch()}>Retry topology</button>}</div>}
    {graphMalformed && <p role="status" className="p-4 text-sm text-ink-400">Topology could not be read.</p>}
    {graphData && !graphMalformed && visibleGraph && layout && <>
      <div className="flex flex-wrap items-center justify-between gap-3 border-y border-ink-700 px-4 py-3">
        <p className="text-xs tabular-nums text-ink-300">{visibleNodes.length} / {graphData.nodes.length} connected resources visible <span aria-hidden="true">|</span> {visibleEdges.length} / {graphData.edges.length} relationships</p>
        <div className="flex flex-wrap items-center gap-1" role="group" aria-label="Topology view controls">
          <button type="button" className="btn-icon" aria-label="Pan topology left" title="Pan left" onClick={() => pan(-240, 0)}><ArrowLeft size={14} /></button>
          <button type="button" className="btn-icon" aria-label="Pan topology up" title="Pan up" onClick={() => pan(0, -180)}><ArrowUp size={14} /></button>
          <button type="button" className="btn-icon" aria-label="Pan topology down" title="Pan down" onClick={() => pan(0, 180)}><ArrowDown size={14} /></button>
          <button type="button" className="btn-icon" aria-label="Pan topology right" title="Pan right" onClick={() => pan(240, 0)}><ArrowRight size={14} /></button>
          <span className="mx-1 h-5 border-l border-ink-600" aria-hidden="true" />
          <button type="button" className="btn-icon" aria-label="Zoom out topology" title="Zoom out" onClick={() => setZoom((current) => Math.max(0.2, current - 0.1))}><Minus size={14} /></button>
          <button type="button" className="btn-icon" aria-label="Zoom in topology" title="Zoom in" onClick={() => setZoom((current) => Math.min(1.5, current + 0.1))}><Plus size={14} /></button>
          <button type="button" className="btn-icon" aria-label="Fit topology to view" title="Fit to view" onClick={fitGraph}><Maximize2 size={14} /></button>
        </div>
      </div>
      <div className={preview ? "min-w-0" : "grid min-w-0 grid-cols-[minmax(0,1fr)] md:grid-cols-[13rem_minmax(0,1fr)]"}>
      {!preview && <aside className="min-w-0 border-b border-ink-700 md:border-b-0 md:border-r" aria-label="Topology filters">
        <div className="flex items-center justify-between border-b border-ink-700 p-3"><span className="text-xs font-semibold text-ink-100">Filters</span><div className="flex gap-1">
          <button type="button" className="btn-icon" aria-label="Show all" title="Show all kinds" aria-pressed={selectedKinds === null} onClick={() => { resetGraphScroll(); setSelectedKinds(null); }}><Eye size={14} /></button>
          <button type="button" className="btn-icon" aria-label="Hide all" title="Hide all kinds" aria-pressed={selectedKinds?.length === 0} onClick={() => { resetGraphScroll(); setSelectedKinds([]); }}><EyeOff size={14} /></button>
        </div></div>
        <div className="flex items-center gap-2 border-b border-ink-700 p-3 text-xs text-ink-300"><span className="flex-1">Refresh</span><button type="button" className="btn-icon" aria-label="Refresh topology" title="Refresh topology" onClick={() => void graph.refetch()}><RotateCcw size={14} /></button></div>
        {filterGroups.map((group) => <details key={group.label} open className="border-b border-ink-700 last:border-b-0"><summary className="cursor-pointer px-3 py-2 text-xs font-medium text-ink-200">{group.label}</summary><ul className="px-2 pb-2">{group.kinds.map((kind) => {
          const visible = selectedKinds === null || selectedKinds.includes(kind);
          return <li key={kind} className="flex min-w-0 items-center gap-2 text-2xs"><span className={`min-w-0 flex-1 break-words ${resourceKindTone(kind)}`}>{kind}</span>{pendingKinds.has(kind) && !kindCounts.get(kind) ? <span className="text-ink-400" aria-label={`${kind} loading`}>loading</span> : <span className="tabular-nums text-ink-400">{kindCounts.get(kind) ?? 0}</span>}<button type="button" className="btn-icon shrink-0" aria-label={`${visible ? "Hide" : "Show"} ${kind}`} title={`${visible ? "Hide" : "Show"} ${kind}`} aria-pressed={visible} onClick={() => setKindVisible(kind, !visible)}>{visible ? <Eye size={14} /> : <EyeOff size={14} />}</button></li>;
        })}</ul></details>)}
      </aside>}
      {graphData.nodes.length === 0 ? <p role="status" className="p-6 text-sm text-ink-400">{pendingKinds.size ? "Relationships are still loading." : preview ? "No projected relationships yet." : "No connected resources were returned for this namespace."}</p> : visibleNodes.length === 0 ? <p role="status" className="p-6 text-sm text-ink-400">No resources match these kind filters.</p> : <div ref={viewportRef} onScroll={(event) => setScrollOffset({ left: event.currentTarget.scrollLeft, top: event.currentTarget.scrollTop })} className={`min-w-0 max-w-full overflow-auto border-b border-ink-700 ${preview ? "h-[min(50vh,32rem)]" : "max-h-[min(65vh,48rem)]"}`} tabIndex={0} aria-label={preview ? "Scrollable overview topology graph" : "Scrollable topology graph"}>
        <div className="relative" style={{ width: layout.width * zoom, height: layout.height * zoom }}>
          <div className="absolute left-0 top-0 origin-top-left" style={{ width: layout.width, height: layout.height, transform: `scale(${zoom})` }}>
          <div className="absolute left-0 top-3 grid" style={{ gridTemplateColumns: `repeat(${layout.columns.length}, ${graphColumnWidth}px)` }} aria-hidden="true">{layout.columns.map((column) => <p className="text-center text-2xs font-medium uppercase text-ink-400" key={column.label}>{column.label}</p>)}</div>
          <svg className="absolute inset-0" width={layout.width} height={layout.height} viewBox={`0 0 ${layout.width} ${layout.height}`} role="img" aria-label={`${preview ? "All namespaces" : namespace} connected Kubernetes relationships`}>
            <defs><marker id="kubernetes-graph-arrow" markerWidth="7" markerHeight="7" refX="6" refY="3.5" orient="auto"><path d="M0,0 L7,3.5 L0,7 z" fill="currentColor" /></marker></defs>
            {viewportEdges.map(({ edge, startX, startY, endX, endY, channelX, labelWidth, label }) => <g key={`${edge.from}:${edge.to}:${edge.type}`} className={relationshipTone(edge.type)} data-edge-from={edge.from} data-edge-to={edge.to}><path d={`M ${startX} ${startY} C ${channelX} ${startY}, ${channelX} ${endY}, ${endX} ${endY}`} fill="none" stroke="currentColor" strokeWidth="1.5" markerEnd="url(#kubernetes-graph-arrow)"/>{label && <><rect x={label.x - labelWidth / 2} y={label.y - 9} width={labelWidth} height="18" rx="4" className="fill-surface stroke-ink-600"/><text x={label.x} y={label.y + 3} textAnchor="middle" className="fill-ink-100" fontSize="9">{edge.type}</text></>}</g>)}
          </svg>
          {viewportNodes.map((node) => {
            const position = layout.positions.get(node.id);
            if (!position) return null;
            const resource = resourceFromGraphNode(node);
            return <button type="button" key={node.id} disabled={!resource} data-node-id={node.id} title={`${node.kind} ${node.namespace ? `${node.namespace}/` : ""}${node.name}`} aria-label={`Open ${node.kind} ${node.namespace ? `${node.namespace}/` : ""}${node.name} details`} onClick={() => resource && onSelectResource(resource)} className="absolute flex h-[72px] flex-col justify-center overflow-hidden rounded-control border border-ink-500/70 bg-surface-raised px-3 text-left shadow-card transition-[background-color,border-color,box-shadow] duration-150 hover:border-accent/60 hover:bg-surface motion-reduce:transition-none focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent" style={{ left: position.x, top: position.y, width: graphNodeWidth }}>
              <span className="flex min-w-0 items-center gap-2"><span className={`truncate text-2xs font-semibold ${resourceKindTone(node.kind)}`}>{node.kind}</span>{node.health && <span className={`truncate rounded px-1.5 py-0.5 text-2xs ${graphHealthTone(node.health)}`}>{node.health}</span>}</span>
              <span className="mt-1 truncate text-xs font-semibold text-ink-50">{node.name}</span>
              <span className="truncate text-2xs text-ink-400">{node.namespace || "Cluster scope"}</span>
            </button>;
          })}
          </div>
        </div>
      </div>}
      </div>
      <footer className="flex flex-wrap gap-2 border-t border-ink-700/70 px-4 py-2 text-2xs" aria-label="Relationship key">{["manages", "exposes", "routes-to", "uses", "configures", "scales"].map((type) => <span key={type} className={`rounded-control bg-ink-800/70 px-2 py-1 ${relationshipTone(type)}`}>{type}</span>)}</footer>
    </>}
  </section>;
}

function IssuesView({ onSelectResource, clusterId }: { onSelectResource: ResourceSelection; clusterId?: string }) {
  const { client: api } = useKubernetesCluster();
  const firstReadyAt = useKubernetesFirstReady();
  const [severity, setSeverity] = useState("");
  const [namespace, setNamespace] = useState("");
  const paging = useCursorPagination(`issues:${namespace}:${severity}`);
  const issues = useQuery({ queryKey: ["kubernetes-issues", namespace, severity, paging.cursor], queryFn: () => api.kubernetesIssues({ namespace, severity, limit: 20, cursor: paging.cursor }), retry: false });
  const denied = issues.error instanceof ApiError && [401, 403].includes(issues.error.status);
  const totals = issues.data?.totals ?? {};
  return (
    <section className="card overflow-hidden transition-[border-color,box-shadow] duration-200 motion-reduce:transition-none" aria-label="Kubernetes issues">
      <ExplorerHeader icon={<AlertTriangle size={15} />} title="Issues" trailing={!denied && issues.data && <span className={`pill ${statusTone(issues.data.sync.state)}`}>{issues.data.sync.state}</span>} />
      <KubernetesIssueAlerts />
      <div className={`${filterRowClass} border-b border-ink-700 p-3`}>
        <KubernetesFilterField label="Severity"><select aria-label="Issue severity" className={filterFieldControlClass} value={severity} onChange={(event) => setSeverity(event.target.value)}><option value="">All severities</option><option value="critical">Critical</option><option value="warning">Warning</option><option value="info">Info</option></select></KubernetesFilterField>
        <KubernetesFilterField label="Namespace"><NamespaceSelect label="Issue namespace" value={namespace} onChange={setNamespace} className={filterFieldControlClass} /></KubernetesFilterField>
      </div>
      {issues.isPending && <CardSkeleton label="Loading grouped issues" rows={5} />}
      {issues.data?.sync.state === "syncing" && !firstReadyAt && <div className="p-3"><KubernetesPreparingPanel progress={issues.data.sync.progress} /></div>}
      {issues.isError && <p role="status" className="p-4 text-sm text-ink-400">Grouped issues are unavailable.</p>}
      {/* A resync after the first publication keeps the grouped issues on
          screen; only a cluster that has never published has nothing to keep. */}
      {!denied && issues.data && (Boolean(firstReadyAt) || issues.data.sync.state !== "syncing") && <>
        <div className="flex flex-wrap gap-2 border-b border-ink-700 px-4 py-3">{["critical", "warning", "info"].map((key) => <span className={`pill inline-flex gap-1.5 ${severityTone(key)}`} key={key}><span>{key}</span><span aria-label={`${key} count`}>{totals[key] ?? 0}</span></span>)}</div>
        <div className="max-h-[min(55vh,36rem)] divide-y divide-ink-700 overflow-y-auto">{issues.data.items.map((issue, index) => <BreakRow key={`${issue.rule}:${issue.root.uid ?? issue.root.name}:${index}`} issue={issue} resource={issueResource(issue)} onSelectResource={onSelectResource} clusterId={clusterId} warningScope={JSON.stringify([namespace, severity, paging.cursor])} sourceSync={issues.isError ? undefined : issues.data.sync} sourceFailures={issues.data.partial_failures} collapsed />)}</div>
        {issues.data.items.length === 0 && <p className="p-6 text-center text-sm text-ink-400">No issues match these filters.</p>}
        <PartialFailuresDisclosure failures={issues.data.partial_failures} />
        <CursorPagination state={paging} next={issues.data.next} />
      </>}
    </section>
  );
}

function ResourcesView({ onSelectResource }: { onSelectResource: ResourceSelection }) {
  const { client: api } = useKubernetesCluster();
  const [query, setQuery] = useState("");
  const [kind, setKind] = useState("All");
  const paging = useCursorPagination(`resources:${query}:${kind}`);
  const inventory = useQuery({ queryKey: ["kubernetes-resources-view", query, kind, paging.cursor], queryFn: () => api.kubernetesWorkloads({ q: query.trim() || undefined, kind: kind === "All" ? undefined : kind, limit: 20, cursor: paging.cursor }), retry: false });
  const resources = asArray(inventory.data?.items);
  const kinds = ["Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob", "Pod"];
  return <section className="card overflow-hidden transition-[border-color,box-shadow] duration-200 motion-reduce:transition-none" aria-label="Kubernetes resources">
    <ExplorerHeader icon={<Boxes size={15} />} title="Resource inventory" />
    <div className="grid gap-2 border-b border-ink-700 p-3 sm:grid-cols-[minmax(0,1fr)_14rem]"><input aria-label="Filter resources" className="input w-full" value={query} onChange={(event) => setQuery(event.target.value)} placeholder="Filter resource names"/><select aria-label="Resource kind" className="input w-full" value={kind} onChange={(event) => setKind(event.target.value)}><option>All</option>{kinds.map((value) => <option key={value}>{value}</option>)}</select></div>
    {inventory.isPending && <CardSkeleton label="Loading Kubernetes resources" rows={5} />}
    {inventory.isError && <CardNote>Resource inventory is unavailable.</CardNote>}
    {inventory.data && <><VirtualList ariaLabel="Virtualized Kubernetes resources" items={resources} itemHeight={resourceRowHeight} renderItem={(item) => <div className="flex h-full min-w-0 items-center border-b border-ink-700"><button type="button" onClick={() => onSelectResource(item)} className="grid h-full w-full min-w-0 flex-1 grid-cols-[minmax(0,1fr)_auto] items-center gap-3 px-4 text-left transition-colors duration-150 motion-reduce:transition-none hover:bg-ink-800/60 sm:grid-cols-[minmax(0,1fr)_minmax(0,0.7fr)_9rem]"><span className="min-w-0"><span className="block truncate text-xs font-medium text-ink-100">{item.name}</span><span className="block truncate text-2xs text-ink-400 sm:hidden">{item.namespace || "Cluster scope"}</span></span><span className="hidden truncate text-xs text-ink-300 sm:block">{item.namespace || "Cluster scope"}</span><span className={`pill ${resourceKindTone(item.kind)}`}>{item.kind}</span></button><div className="px-2"><ResourceLogsButton resource={item} onSelect={onSelectResource} /></div></div>} />
    {resources.length === 0 && <p role="status" className="p-6 text-center text-sm text-ink-400">No resources match these filters.</p>}
    <CursorPagination state={paging} next={inventory.data.next} />
    <PartialFailuresDisclosure failures={inventory.data.partial_failures} />
    </>}
  </section>;
}

function VirtualList<T>({ ariaLabel, items, itemHeight, renderItem, itemKey }: { ariaLabel: string; items: T[]; itemHeight: number; renderItem: (item: T, index: number) => ReactNode; itemKey?: (item: T) => string }) {
  const [scrollTop, setScrollTop] = useState(0);
  const visibleCount = Math.ceil(virtualViewportHeight / itemHeight) + virtualOverscan * 2;
  const start = Math.max(0, Math.min(Math.floor(scrollTop / itemHeight) - virtualOverscan, items.length - visibleCount));
  const end = Math.min(items.length, start + visibleCount);
  return <div role="list" aria-label={ariaLabel} className="max-h-[27.5rem] overflow-y-auto" onScroll={(event) => setScrollTop(event.currentTarget.scrollTop)}>
    <div style={{ height: start * itemHeight }} aria-hidden="true" />
    {items.slice(start, end).map((item, offset) => <div role="listitem" style={{ height: itemHeight }} key={itemKey ? itemKey(item) : offset + start}>{renderItem(item, offset + start)}</div>)}
    <div style={{ height: Math.max(0, (items.length - end) * itemHeight) }} aria-hidden="true" />
  </div>;
}

function ReleasesView() {
  const { client: api } = useKubernetesCluster();
  const paging = useCursorPagination("helm-releases");
  const releases = useQuery({ queryKey: ["kubernetes-releases", paging.cursor], queryFn: () => api.kubernetesReleases({ limit: 20, cursor: paging.cursor }), retry: false });
  return <section className="card overflow-hidden transition-[border-color,box-shadow] duration-200 motion-reduce:transition-none" aria-label="Helm releases"><ExplorerHeader icon={<Layers3 size={15} />} title="Helm releases" trailing={<span className="text-xs text-ink-400">Label metadata only</span>} />
    {releases.isPending && <CardSkeleton label="Loading release metadata" rows={5} />}{releases.isError && <p role="status" className="p-4 text-sm text-ink-400">Release metadata is unavailable.</p>}
    {releases.data && <><div className="max-h-[min(55vh,36rem)] divide-y divide-ink-700 overflow-y-auto">{releases.data.items.map((release) => <ReleaseRow release={release} key={`${release.namespace}:${release.name}`} />)}</div>{releases.data.items.length === 0 && <p className="p-6 text-center text-sm text-ink-400">No Helm release labels were found.</p>}<CursorPagination state={paging} next={releases.data.next}/><PartialFailuresDisclosure failures={releases.data.partial_failures}/></>}
  </section>;
}

function ReleaseRow({ release }: { release: KubernetesHelmRelease }) {
  const [expanded, setExpanded] = useState(false);
  return <article className="border-b border-ink-700/70"><button type="button" aria-expanded={expanded} onClick={() => setExpanded((value) => !value)} className="grid w-full grid-cols-[minmax(0,1fr)_auto] items-center gap-3 px-4 py-3 text-left transition-colors duration-150 motion-reduce:transition-none hover:bg-ink-800/60 sm:grid-cols-[minmax(0,1fr)_minmax(0,0.7fr)_8rem_7rem]"><span className="min-w-0"><span className="block truncate text-sm font-medium text-ink-100">{release.name}</span><span className="block text-2xs text-ink-400">{release.namespace}</span></span><span className={`pill ${statusTone(release.current.status || "unknown")}`}>{release.current.status || "unknown"}</span><span className="hidden text-xs text-ink-300 sm:block">Revision {release.current.revision}</span><span className={`pill ${statusTone(release.health)}`}>{release.health}</span></button>{expanded && <ol className="divide-y divide-ink-700 border-t border-ink-700 bg-ink-900/30 px-4">{release.history.map((revision) => <li className="flex justify-between gap-3 py-2 text-xs" key={revision.revision}><span className="text-ink-200">Revision {revision.revision}</span><span className={`pill ${statusTone(revision.status)}`}>{revision.status}</span><time className="text-ink-400">{revision.created ? new Date(revision.created).toLocaleString() : "Time unavailable"}</time></li>)}</ol>}</article>;
}

function GitOpsView() {
  const { client: api } = useKubernetesCluster();
  const appsPaging = useCursorPagination("gitops-apps");
  const apps = useQuery({ queryKey: ["kubernetes-gitops-apps", appsPaging.cursor], queryFn: () => api.kubernetesGitOpsApps({ limit: 20, cursor: appsPaging.cursor }), retry: false });
  const rolloutsPaging = useCursorPagination("gitops-rollouts");
  const rollouts = useQuery({ queryKey: ["kubernetes-rollouts", rolloutsPaging.cursor], queryFn: () => api.kubernetesRollouts({ limit: 20, cursor: rolloutsPaging.cursor }), retry: false });
  return <div className="space-y-4"><PartialFailuresDisclosure failures={[...(apps.data?.partial_failures ?? []), ...(rollouts.data?.partial_failures ?? [])]} /><section className="card overflow-hidden transition-[border-color,box-shadow] duration-200 motion-reduce:transition-none" aria-label="GitOps applications"><ExplorerHeader icon={<GitBranch size={15} />} title="GitOps applications" trailing={apps.data && <span className={`pill ${apps.data.available ? "pill-good" : "pill-warn"}`}>{apps.data.available ? "Detected" : "Not detected"}</span>} />
    {apps.isPending && <CardSkeleton label="Loading GitOps applications" rows={5} />}{apps.isError && <p role="status" className="p-4 text-sm text-sev-warning">GitOps status is unavailable.</p>}{apps.data && <>{!apps.data.available && <p role="status" className="p-4 text-sm text-ink-400">{apps.data.reason || "No GitOps APIs detected."}</p>}<div className="max-h-[min(55vh,36rem)] divide-y divide-ink-700 overflow-y-auto">{apps.data.items.map((app) => <article className="grid gap-2 px-4 py-3 sm:grid-cols-[minmax(0,1fr)_8rem_8rem_8rem]" key={`${app.tool}:${app.namespace}:${app.name}`}><span className="min-w-0"><span className="block truncate text-sm font-medium text-ink-100">{app.name}</span><span className="flex flex-wrap gap-x-2 text-2xs text-ink-400"><span>{app.namespace}</span><span>{app.tool}</span><span>{app.kind}</span></span></span><span className="pill w-fit">{app.sync}</span><span className="pill w-fit">{app.health}</span><span className="truncate text-xs text-ink-400" title={app.revision}>{app.revision || "Revision unavailable"}</span>{app.message && <p className="break-words text-xs text-ink-300 sm:col-span-4">{app.message}</p>}</article>)}</div>{apps.data.items.length === 0 && apps.data.available && <p role="status" className="p-6 text-center text-sm text-ink-400">No GitOps applications were returned.</p>}<CursorPagination state={appsPaging} next={apps.data.next}/></>}</section>
    <section className="card overflow-hidden transition-[border-color,box-shadow] duration-200 motion-reduce:transition-none" aria-label="Argo Rollouts"><ExplorerHeader icon={<RotateCcw size={15} />} title="Argo Rollouts" />
      {rollouts.isPending && <CardSkeleton label="Loading Argo Rollouts" rows={5} />}{rollouts.isError && <p role="status" className="p-4 text-sm text-sev-warning">Rollout inventory is unavailable.</p>}{rollouts.data && <>{!rollouts.data.available && <p role="status" className="p-4 text-sm text-ink-400">{rollouts.data.reason || "Argo Rollouts are not available."}</p>}<div className="max-h-[min(55vh,36rem)] divide-y divide-ink-700 overflow-y-auto">{rollouts.data.items.map((rollout) => <article className="space-y-2 px-4 py-3" key={`${rollout.namespace}:${rollout.name}`}><div className="flex flex-wrap items-center gap-2"><h3 className="min-w-0 flex-1 break-all text-sm font-medium text-ink-100">{rollout.namespace}/{rollout.name}</h3><span className="pill">{rollout.phase}</span></div><dl className="grid gap-2 text-xs sm:grid-cols-2"><div><dt className="text-ink-400">Strategy</dt><dd className="mt-1 text-ink-200">{rollout.strategy || "Unavailable"}</dd></div><div><dt className="text-ink-400">Step</dt><dd className="mt-1 text-ink-200">{rollout.total_steps > 0 ? `${rollout.step} of ${rollout.total_steps}` : "Unavailable"}</dd></div><div><dt className="text-ink-400">Canary weight</dt><dd className="mt-1 text-ink-200">{rollout.weight === undefined ? "Unavailable" : `${rollout.weight}%`}</dd></div><div><dt className="text-ink-400">Stable and canary ReplicaSets</dt><dd className="mt-1 break-words text-ink-200">{rollout.stable_rs || "Unavailable"} / {rollout.canary_rs || "Unavailable"}</dd></div></dl>{rollout.message && <p className="break-words text-xs text-ink-300">{rollout.message}</p>}</article>)}</div>{rollouts.data.items.length === 0 && rollouts.data.available && <p role="status" className="p-6 text-center text-sm text-ink-400">No Rollouts were returned.</p>}<CursorPagination state={rolloutsPaging} next={rollouts.data.next}/></>}
    </section>
  </div>;
}

type TimelineMinutes = 15 | 60 | 360 | 1440;
const timelineRanges: ReadonlyArray<readonly [TimelineMinutes, string]> = [[15, "15m"], [60, "1h"], [360, "6h"], [1440, "24h"]];

function TimelineRangeControl({ minutes, onChange, loading, compact = false }: { minutes: TimelineMinutes; onChange: (minutes: TimelineMinutes) => void; loading: boolean; compact?: boolean }) {
  return <div role="group" aria-label="Timeline window" aria-busy={loading} className={`flex items-center ${compact ? "rounded-control border border-ink-500 bg-surface" : "flex-wrap gap-1"}`}>
    {timelineRanges.map(([value, label]) => <button type="button" aria-pressed={minutes === value} onClick={() => onChange(value)} className={`rounded-control px-3 text-xs transition-colors duration-150 motion-reduce:transition-none ${compact ? "py-1" : "py-1.5"} ${minutes === value ? "bg-accent-subtle text-ink-50" : "text-ink-300 hover:bg-ink-800"}`} key={value}>{label}</button>)}
    <span data-range-spinner className="inline-flex size-6 shrink-0 items-center justify-center text-ink-300">{loading && <Loader2 size={13} role="status" aria-label="Loading timeline range" className="animate-spin motion-reduce:animate-none" />}</span>
  </div>;
}

function TimelineView({ onSelectResource }: { onSelectResource: ResourceSelection }) {
  const { cluster } = useKubernetesCluster();
  const { search } = useLocation();
  const focus = new URLSearchParams(search);
  const rootUID = focus.get("root_uid");
  return rootUID ? <RootTimelineView key={JSON.stringify([cluster, rootUID, focus.get("center"), focus.get("namespace"), focus.get("kind")])} rootUID={rootUID} focus={focus} onSelectResource={onSelectResource} /> : <UnfocusedTimelineView onSelectResource={onSelectResource} />;
}

function RootTimelineView({ rootUID, focus, onSelectResource }: { rootUID: string; focus: URLSearchParams; onSelectResource: ResourceSelection }) {
  const { client: api, cluster, key } = useKubernetesCluster();
  const center = focus.get("center");
  const centerTime = center ? Date.parse(center) : NaN;
  const centered = Number.isFinite(centerTime);
  const [minutes, setMinutes] = useState<15 | 60 | 360 | 1440>(60);
  const namespace = focus.get("namespace") ?? "";
  const kind = focus.get("kind") ?? "";
  const [windowEnd] = useState(() => centered ? centerTime : Date.now());
  const [journalCeiling] = useState(Date.now);
  const since = new Date(windowEnd - minutes * 60_000 * (centered ? 0.5 : 1)).toISOString();
  const until = new Date(windowEnd + (centered ? minutes * 30_000 : 0)).toISOString();
  const crashes = useKubernetesCrashes({ root_uid: rootUID, namespace: namespace || undefined, since, until: new Date(Math.min(journalCeiling, Date.parse(until))).toISOString() }, true, true);
  const breaks = useQuery({ queryKey: ["kubernetes-issues", "timeline-root", rootUID, namespace], queryFn: () => api.kubernetesIssues({ root_uid: rootUID, include: "changes", namespace: namespace || undefined }), retry: false, refetchInterval: (query) => query.state.data?.sync.pending?.length ? 5000 : false });
  const capturedRoot = crashes.data?.items.find((event) => event.root_uid === rootUID);
  const issue = !breaks.isError ? breaks.data?.items.find((item) => item.root.uid === rootUID && (!namespace || item.root.namespace === namespace)) ?? (capturedRoot ? {
    root: { uid: rootUID, kind: capturedRoot.root_kind, namespace: capturedRoot.namespace, name: capturedRoot.root_name },
    broke_at: capturedRoot.at, rule: "container_terminated", severity: "info" as const, count: 1,
  } : undefined) : undefined;
  const rootNamespace = issue?.root.namespace;
  const graph = useQuery({
    queryKey: ["kubernetes-graph", "timeline-root", rootUID, rootNamespace],
    enabled: Boolean(issue),
    retry: false,
    queryFn: ({ signal }) => api.kubernetesRelated({ resource_id: resourceIdForKind(issue!.root.kind) ?? "", namespace: rootNamespace, name: issue!.root.name }, signal),
  });
  const topology = !graph.isError ? graph.data : undefined;
  const paging = useCursorPagination(`root-timeline:${rootUID}:${since}:${until}:${namespace}:${kind}`);
  const changes = useQuery<KubernetesChangesPage>({
    queryKey: ["kubernetes-changes", "timeline-root", rootUID, rootNamespace, since, until, kind, paging.cursor],
    enabled: Boolean(issue),
    retry: false,
    // Keep the previous range on screen while a new range or page loads, so
    // the chart dims and swaps in place instead of collapsing to a skeleton.
    placeholderData: (previous, query) => {
      const prior = query?.queryKey;
      const scope = key("kubernetes-changes", "timeline-root", rootUID, rootNamespace);
      return prior && JSON.stringify(prior.slice(0, scope.length)) === JSON.stringify(scope) && prior[scope.length + 2] === kind ? previous : undefined;
    },
    queryFn: ({ signal }) => api.kubernetesChanges({ root_uid: rootUID, since, until, namespace: rootNamespace, kind: kind || undefined, limit: 100, cursor: paging.cursor }, signal),
  });
  const data = !breaks.isError && !changes.isError && changes.data ? { ...changes.data, items: changes.data.items.filter((change) => (!cluster || change.cluster === cluster) && (!rootNamespace || change.namespace === rootNamespace || change.kind === "Node")) } : undefined;
  const gaps = [...(data?.gaps ?? []), ...(issue?.gaps ?? [])];
  const failures = breaks.isError ? [] : [...(breaks.data?.partial_failures ?? []), ...(topology?.partial_failures ?? []), ...(data?.partial_failures ?? [])];
  const warnings: Array<{ id: string; content: ReactNode }> = [...issueWarnings(issue ? { ...issue, gaps: [] } : undefined), ...failureWarnings(failures)];
  if ([breaks.data?.sync.state, topology?.sync.state, data?.sync.state].includes("stale")) warnings.push({ id: "snapshot-stale", content: "Timeline evidence includes stale snapshots." });
  if (graph.isError) warnings.push({ id: "graph-unavailable", content: <>Related-resource graph unavailable; linked evidence is incomplete.<button type="button" className="ml-2 text-link underline" onClick={() => void graph.refetch()}>Retry</button></> });
  if (gaps.length) warnings.push({ id: "history-gaps", content: "Change capture does not cover the whole range" });
  const unfocusedQuery = new URLSearchParams(focus);
  for (const name of ["root_uid", "center", "namespace", "kind"]) unfocusedQuery.delete(name);
  if (namespace) unfocusedQuery.set("namespace", namespace);
  if (kind) unfocusedQuery.set("kind", kind);
  for (const name of ["logs_from", "logs_to", "logs_mode", "logs_pod", "logs_container"]) unfocusedQuery.delete(name);
  const rangeLoading = Boolean(data && (changes.isPlaceholderData || crashes.isPlaceholderData));
  const loadingHistory = breaks.isPending || Boolean(issue) && (graph.isPending || changes.isPending);
  return <KubernetesBreakLogsProvider issue={issue} onSelectResource={onSelectResource}><section className="card overflow-clip" aria-label="Kubernetes change timeline">
    <div className="flex min-w-0 flex-wrap items-center justify-between gap-2 border-b border-ink-700 px-4 py-2">
      <div className="flex min-w-0 flex-wrap items-center gap-2 text-xs"><span className="text-ink-300">Filtered</span><span className="pill max-w-full"><span className="truncate" title={rootUID}>{issue ? `${issue.root.kind} ${issue.root.namespace ? `${issue.root.namespace}/` : ""}${issue.root.name}` : rootUID}</span><Link to={`?${unfocusedQuery}`} className="ml-2" aria-label="Clear root filter" title="Clear root filter"><X size={12} aria-hidden="true" /></Link></span>{centered && <time className="pill pill-accent font-mono" dateTime={new Date(centerTime).toISOString()}>centred {new Date(centerTime).toLocaleTimeString([], { hour12: false })}</time>}</div>
      <TimelineRangeControl minutes={minutes} onChange={setMinutes} loading={rangeLoading} compact />
    </div>
    {!breaks.isError && evidenceLoading(issue?.pending, breaks.data?.sync.pending) && <div className="border-b border-ink-700 px-2"><KubernetesPendingNote label="Evidence loading" /></div>}
    <div data-timeline-chart-area aria-busy={loadingHistory || rangeLoading} className={`min-h-60 transition-opacity duration-150 motion-reduce:transition-none ${rangeLoading ? "opacity-60" : ""}`}>
    {loadingHistory && <CardSkeleton label="Loading related root change history" rows={5} />}
    {breaks.isError && <p role="status" className="p-4 text-sm text-sev-warning">Root history unavailable.</p>}
    {breaks.isSuccess && !issue && <p role="status" className="p-4 text-sm text-ink-400">Root not found in this namespace.</p>}
    {changes.isError && <p role="status" className="p-4 text-sm text-sev-warning">Related change history is unavailable.<button type="button" className="ml-2 text-link" onClick={() => void changes.refetch()}>Retry</button></p>}
    {data && issue && <>
      <BreakTimelineLane scope={JSON.stringify([cluster, rootUID, minutes, namespace, kind, paging.cursor, crashes.paging.cursor])} warningsOwned issue={{ ...issue, changes: [] }} crashes={crashes.isError ? [] : crashes.data?.items ?? []} changes={data.items} since={since} until={until} onSelectChange={(change) => { const resource_id = resourceIdForKind(change.kind); if (resource_id) onSelectResource({ resource_id, kind: change.kind, namespace: change.namespace, name: change.name, uid: change.uid }, "timeline"); }} />
      <CursorPagination state={paging} busy={changes.isFetching} next={data.next} />
    </>}
    {(!data || !issue) && crashes.data && !crashes.isError && <ResourceTimelineChart crashes={crashes.data.items} changes={[]} since={since} until={until} onSelectChange={() => undefined} />}
    </div>
    <KubernetesBreakLogs pending={breaks.isPending} />
    <CrashJournal journal={crashes} reasons={warnings.map((warning) => warning.content)} />
  </section></KubernetesBreakLogsProvider>;
}

function UnfocusedTimelineView({ onSelectResource }: { onSelectResource: ResourceSelection }) {
  const { client: api, cluster, key } = useKubernetesCluster();
  const firstReadyAt = useKubernetesFirstReady();
  const focus = new URLSearchParams(window.location.search);
  const center = focus.get("center");
  const centerTime = center ? Date.parse(center) : NaN;
  const [minutes, setMinutes] = useState<TimelineMinutes>(60);
  const [namespace, setNamespace] = useState(focus.get("namespace") ?? "");
  const [kind, setKind] = useState(focus.get("kind") ?? "");
  const [params] = useSearchParams();
  const breaksOnly = params.get("breaks") === "1";
  const centered = Number.isFinite(centerTime);
  const [windowEnd, setWindowEnd] = useState(() => centered ? centerTime : Date.now());
  const [generation, setGeneration] = useState(0);
  const since = new Date(windowEnd - minutes * 60_000 * (centered ? 0.5 : 1)).toISOString();
  const until = new Date(windowEnd + (centered ? minutes * 30_000 : 0)).toISOString();
  const paging = useCursorPagination(`timeline:${since}:${namespace}:${kind}:${breaksOnly}:${generation}`);
  const timeline = useQuery({
    queryKey: ["kubernetes-timeline", "list", namespace, kind, breaksOnly, generation, minutes, since, until, paging.cursor],
    queryFn: ({ signal }) => api.kubernetesTimeline({ since, until, namespace, kind, breaks: breaksOnly ? 1 : undefined, limit: 100, cursor: paging.cursor }, signal),
    // A new range or page keeps the current rows on screen, dimmed, until the
    // next page lands; a different filter starts from a clean list.
    placeholderData: (previous, query) => JSON.stringify(query?.queryKey.slice(0, -4)) === JSON.stringify(key("kubernetes-timeline", "list", namespace, kind, breaksOnly, generation)) ? previous : undefined,
    retry: false,
    refetchInterval: (query) => query.state.data?.sync.state === "syncing" ? 1000 : false,
  });
  useEffect(() => {
    if (centered || paging.page > 1) return;
    const timer = window.setInterval(() => setWindowEnd(Date.now()), 30_000);
    return () => window.clearInterval(timer);
  }, [centered, paging.page]);
  const data = timeline.isError ? undefined : timeline.data;
  // The window slides forward every 30 s on its own; only a range the user
  // picked dims the list and spins the range control.
  const [shownMinutes, setShownMinutes] = useState(minutes);
  if (data && !timeline.isPlaceholderData && shownMinutes !== minutes) setShownMinutes(minutes);
  const rangeLoading = Boolean(data) && timeline.isPlaceholderData && shownMinutes !== minutes;
  const warnings = failureWarnings(data?.partial_failures);
  if (data?.sync.state === "stale") warnings.push({ id: "timeline-stale", content: "Timeline evidence includes stale snapshots." });
  if (data?.gaps?.length) warnings.push({ id: "history-gaps", content: "History capture does not cover the whole range" });
  if (["error", "partial"].includes(data?.sync.state ?? "")) warnings.push({ id: "history-unavailable", content: "Some history could not be read" });
  const openChange = (change: KubernetesChange) => {
    const resource_id = resourceIdForKind(change.kind);
    if (!resource_id) return;
    onSelectResource({ resource_id, kind: change.kind, namespace: change.namespace, name: change.name, uid: change.uid }, "timeline");
  };
  const focusedLink = (uid: string, at: string, ns?: string) => {
    const query = new URLSearchParams(focus);
    query.set("view", "timeline");
    query.set("root_uid", uid);
    query.set("center", at);
    if (ns) query.set("namespace", ns); else query.delete("namespace");
    return `?${query}`;
  };
  const rows = (data?.items ?? []).map((entry) => {
    const description = `timeline-row-${entry.id}`;
    if (entry.type === "change") {
      const change = entry.change;
      return { ...entry, kind: change.kind, name: change.name, namespace: change.namespace, summary: change.type.replaceAll("_", " "),
        content: <button type="button" aria-describedby={description} className="block h-full w-full text-left" onClick={() => openChange(change)}>Open change details</button> };
    }
    if (entry.type === "break") {
      const issue = entry.issue;
      return { ...entry, kind: issue.root.kind, name: issue.root.name, namespace: issue.root.namespace, summary: issue.rule.replaceAll(/[._]/g, " "),
        content: <Link aria-describedby={description} className="block h-full w-full" to={focusedLink(issue.root.uid ?? "", entry.at, issue.root.namespace)}>Open break details</Link> };
    }
    const crash = entry.crash;
    return { ...entry, kind: crash.root_kind || "Pod", name: crash.root_name || crash.pod, namespace: crash.namespace, summary: `${crash.container}: ${crash.reason || "Terminated"}, exit ${crash.exit_code}`,
      content: <Link aria-describedby={description} className="block h-full w-full" to={focusedLink(crash.root_uid || crash.pod_uid, entry.at, crash.namespace)}>Open crash details</Link> };
  });
  return <section className="card overflow-clip transition-[border-color,box-shadow] duration-200 motion-reduce:transition-none" aria-label="Kubernetes change timeline">
    <ExplorerHeader icon={<History size={15} />} title="Breaks and changes" trailing={<span className="flex flex-wrap items-center gap-2">{data && <span className="text-2xs text-ink-400">{breaksOnly ? `${data.counts?.break ?? data.total} ${(data.counts?.break ?? data.total) === 1 ? "break" : "breaks"} in ${timelineRanges.find(([value]) => value === minutes)?.[1]}` : `${counted(data.total, "event")} and ${counted(data.crash_count, "crash")}`}</span>}<SegmentedControl param="breaks" aria-label="Timeline events" defaultValue="all" options={[{ value: "all", label: "All" }, { value: "1", label: "Breaks only" }]} /></span>} />
    <div className="flex flex-wrap items-end justify-between gap-3 border-b border-ink-700 p-3">
      <TimelineRangeControl minutes={minutes} onChange={setMinutes} loading={rangeLoading} />
      <div className={filterRowClass}><KubernetesFilterField label="Namespace"><NamespaceSelect label="Timeline namespace" value={namespace} onChange={setNamespace} className={filterFieldControlClass} /></KubernetesFilterField><KubernetesFilterField label="Kind"><select aria-label="Timeline kind" className={filterFieldControlClass} value={kind} onChange={(event) => setKind(event.target.value)}><option value="">All kinds</option>{["Pod", "Deployment", "StatefulSet", "DaemonSet", "Job", "Node"].map((value) => <option key={value}>{value}</option>)}</select></KubernetesFilterField></div>
    </div>
    {timeline.isPending && <CardSkeleton label="Loading projected change history" rows={5} />}
    {data?.sync.state === "syncing" && !firstReadyAt && <div className="p-3"><KubernetesPreparingPanel progress={data.sync.progress} /></div>}
    {timeline.isError && <p role="status" className="p-4 text-sm text-ink-400">Timeline unavailable. <button type="button" className="text-link" onClick={() => { paging.resetPage(); setGeneration((value) => value + 1); }}>{paging.cursor && timeline.error instanceof ApiError && timeline.error.status === 400 ? "Restart Timeline" : "Retry Timeline"}</button></p>}
    <div aria-busy={timeline.isFetching} data-timeline-list className={`transition-opacity duration-150 motion-reduce:transition-none ${rangeLoading ? "opacity-60" : ""}`}>
      <VirtualList ariaLabel="Breaks and changes" items={rows} itemKey={(row) => row.id} itemHeight={resourceRowHeight} renderItem={(row) => <article className="relative grid h-full grid-cols-[5rem_minmax(0,1fr)] items-center gap-3 border-b border-ink-700 px-4 text-xs">
        <time dateTime={row.at} title={new Date(row.at).toLocaleString()} className="tabular-nums text-ink-400">{new Date(row.at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}</time>
        <div id={`timeline-row-${row.id}`} className="min-w-0"><p className="truncate" title={`${row.kind} ${row.namespace ? `${row.namespace}/` : ""}${row.name}`}><span className="text-ink-400">{row.kind}</span> {row.name}</p><p className="truncate text-ink-400">{row.summary}</p></div>
        <div className="absolute inset-0 rounded-control text-transparent hover:bg-ink-500/10 focus-within:ring-2 focus-within:ring-inset focus-within:ring-accent">{row.content}</div>
      </article>} />
      {!rows.length && data && (Boolean(firstReadyAt) || data.sync.state !== "syncing") && <p className="p-4 text-xs text-ink-400">{breaksOnly ? "No breaks in this range." : "No changes or crashes in this range."}</p>}
      <nav aria-label="Timeline pages"><CursorPagination state={paging} busy={timeline.isFetching} next={data?.next} /></nav>
    </div>
    <div className="px-2"><KubernetesWarnings warnings={warnings} scope={JSON.stringify([cluster, namespace, kind, breaksOnly, since, until, paging.cursor])} /></div>
  </section>;
}
