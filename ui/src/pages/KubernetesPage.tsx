import {
  useEffect,
  useMemo,
  useRef,
  useState,
  type Ref,
  type ReactNode,
} from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useLocation, useNavigate, useSearchParams } from "react-router-dom";
import { useBrokenIssues, useKubernetesCluster, useKubernetesQuery } from "@/lib/useKubernetesCluster";
import { KubernetesClusterProvider } from "@/components/KubernetesClusterProvider";
import { KubernetesFleet } from "./KubernetesFleet";
import {
  Activity,
  AlertTriangle,
  Boxes,
  ChevronRight,
  ChevronDown,
  Cpu,
  Database,
  ExternalLink,
  GitBranch,
  History,
  Layers3,
  LayoutDashboard,
  Network,
  RefreshCw,
  Search,
  Server,
  ShipWheel,
  Waypoints,
  X,
} from "lucide-react";
import { api, ApiError, type KubernetesCrashEvent, type KubernetesIndexStatus, type KubernetesOverview, type KubernetesResource, type KubernetesUsage } from "@/lib/api";
import { PodLogViewer } from "@/components/PodLogViewer";
import { CursorPagination } from "@/components/Pagination";
import { PeekPanel } from "@/components/PeekPanel";
import { RetryableError } from "@/components/RetryableError";
import { SkCard } from "@/components/Skeleton";
import { TopBar } from "@/components/TopBar";
import { useCursorPagination, type CursorPaginationState } from "@/lib/pagination";
import { ResourceEvents, ResourceFacts, ResourceHeader, ResourceRelations, ResourceTimeline, ResourceYaml } from "./KubernetesResourceDetail";
import { quantity as parseQuantity, resourceDetailTabs, type ResourceDetailTab } from "@/lib/kubernetesResourceDetail";
import { ResourceMetrics } from "./KubernetesResourceMetrics";
import "./KubernetesResourceDetail.css";
import { CrashLogNavigationContext } from "@/lib/useKubernetesBreakLogs";
import {
  KubernetesOverviewInsights,
  KubernetesOverviewTimeline,
  KubernetesExplorerTabContent,
  PartialFailuresDisclosure,
} from "./KubernetesExplorerViews";
import { kubernetesExplorerTabs, type KubernetesExplorerTab } from "./kubernetesExplorerTabs";
import { StateBadge, ToolIcon } from "./AgentToolsPage";
import { KubernetesWarnings } from "./KubernetesWarnings";
import { failureWarnings, type KubernetesWarning } from "./kubernetesWarningItems";

type KubernetesDrawerTab = ResourceDetailTab;

const drawerTabs = resourceDetailTabs;

function parseExplorerTab(value: string | null): KubernetesExplorerTab {
  return kubernetesExplorerTabs.some((item) => item.id === value)
    ? (value as KubernetesExplorerTab)
    : "overview";
}

function parseDrawerTab(value: string | null, resource: KubernetesResource | null): KubernetesDrawerTab {
  if (!resource) return "overview";
  return drawerTabs.some((item) => item.id === value)
    ? (value as KubernetesDrawerTab)
    : "overview";
}

function parseResourceSelection(search: string): KubernetesResource | null {
  const value = new URLSearchParams(search).get("r");
  if (!value) return null;
  const [resource_id, namespace, name] = value.split("/");
  if (!resource_id || !name) return null;
  const kind = ({
    "core~v1~pods": "Pod",
    "core~v1~nodes": "Node",
    "apps~v1~deployments": "Deployment",
    "apps~v1~statefulsets": "StatefulSet",
    "apps~v1~daemonsets": "DaemonSet",
    "batch~v1~jobs": "Job",
    "batch~v1~cronjobs": "CronJob",
  } as Record<string, string>)[resource_id] ?? "Resource";
  return { resource_id, kind, namespace: namespace || undefined, name };
}

function writeExplorerLocation(options: {
  view?: KubernetesExplorerTab;
  resource?: KubernetesResource | null;
  drawerTab?: KubernetesDrawerTab;
}) {
  const query = new URLSearchParams(window.location.search);
  if (options.view) {
    if (options.view !== "timeline") {
      query.delete("logs_from"); query.delete("logs_to");
      query.delete("logs_mode"); query.delete("logs_pod");
    }
    if (options.view !== "timeline" && query.has("root_uid")) {
      query.delete("root_uid");
      query.delete("center");
      query.delete("namespace");
    }
    if (options.view === "overview") query.delete("view");
    else query.set("view", options.view);
  }
  if (options.resource !== undefined) {
    if (options.resource) {
      query.set("r", `${options.resource.resource_id}/${options.resource.namespace ?? ""}/${options.resource.name}`);
    } else {
      query.delete("r");
      query.delete("tab");
    }
  }
  if (options.drawerTab && options.resource) {
    if (options.drawerTab === "overview") query.delete("tab");
    else query.set("tab", options.drawerTab);
  }
  const suffix = query.toString();
  window.history.pushState(null, "", `${window.location.pathname}${suffix ? `?${suffix}` : ""}`);
}

const explorerTabIcons: Record<KubernetesExplorerTab, typeof Cpu> = {
  overview: LayoutDashboard,
  issues: AlertTriangle,
  timeline: History,
  topology: Network,
  resources: Boxes,
  nodes: Server,
  releases: Layers3,
  gitops: GitBranch,
  traffic: Waypoints,
  agent: Activity,
};


const workloadKinds = new Set([
  "Deployment",
  "StatefulSet",
  "DaemonSet",
  "Job",
  "CronJob",
  "Pod",
]);
const workloadKindOptions = ["Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob", "Pod"];
const usageWindowMilliseconds = 15 * 60 * 1000;
const usagePollingMilliseconds = 15_000;
const usageSnapshotLimit = 61;
const usagePodSampleLimit = 500;

function asArray<T>(value: T[] | null | undefined): T[] {
  return Array.isArray(value) ? value : [];
}

function formatAmount(value: number, maximumFractionDigits = 2): string {
  return value.toLocaleString("en-US", { maximumFractionDigits });
}

function formatCPU(value: string | undefined): string {
  const cores = parseQuantity(value);
  if (cores === null) return "Unavailable";
  if (Math.abs(cores) < 1) return `${formatAmount(cores * 1000, 3)} mCPU`;
  const formatted = formatAmount(cores, 3);
  return `${formatted} ${formatted === "1" || formatted === "-1" ? "core" : "cores"}`;
}

function formatMemory(value: string | undefined): string {
  const bytes = parseQuantity(value);
  if (bytes === null) return "Unavailable";
  const units = [
    ["TiB", 2 ** 40],
    ["GiB", 2 ** 30],
    ["MiB", 2 ** 20],
    ["KiB", 2 ** 10],
    ["B", 1],
  ] as const;
  const unit =
    units.find(([, divisor]) => Math.abs(bytes) >= divisor) ??
    units[units.length - 1];
  return `${formatAmount(bytes / unit[1], 2)} ${unit[0]}`;
}

function usageAggregate(usage?: KubernetesUsage, overview?: KubernetesOverview) {
  const source = [
    { name: "node_metrics" as const, metrics: usage?.node_metrics },
    { name: "pod_metrics" as const, metrics: usage?.pod_metrics },
  ].filter(({ metrics }) => metrics?.complete === true && metrics.availability !== "unavailable" && (metrics.cpu !== undefined || metrics.memory !== undefined))
    .sort((left, right) => {
      const quality = (metrics: typeof left.metrics) => metrics?.availability === "available" ? (metrics.fresh ? 2 : 1) : 0;
      const qualityDifference = quality(right.metrics) - quality(left.metrics);
      if (qualityDifference !== 0) return qualityDifference;
      const observedAt = (value?: string) => {
        const timestamp = Date.parse(value ?? "");
        return Number.isFinite(timestamp) ? timestamp : 0;
      };
      const observedDifference = observedAt(right.metrics?.observed_at) - observedAt(left.metrics?.observed_at);
      if (observedDifference !== 0) return observedDifference;
      return left.name === "node_metrics" ? -1 : 1;
    })[0];
  const overviewUsable = !source && overview?.usage_source && overview.usage_source !== "unavailable" && overview.metrics_status && overview.metrics_status !== "unavailable" && (overview.usage_cpu !== undefined || overview.usage_memory !== undefined);
  const availability = source?.metrics?.availability ?? (overviewUsable ? overview.metrics_status! : "unavailable");
  const incomplete = !source && Boolean([usage?.node_metrics, usage?.pod_metrics].some((metrics) => metrics && metrics.complete !== true));
  return {
    source: source?.name ?? (overviewUsable ? overview.usage_source! : undefined),
    availability,
    fresh: source ? Boolean(source.metrics?.fresh && availability === "available") : Boolean(overviewUsable && overview?.metrics_fresh && availability === "available"),
    incomplete,
    fromOverview: Boolean(overviewUsable),
    cpu: source?.metrics?.cpu ?? (overviewUsable ? overview?.usage_cpu : undefined),
    memory: source?.metrics?.memory ?? (overviewUsable ? overview?.usage_memory : undefined),
    observedAt: source?.metrics?.observed_at ?? (overviewUsable ? overview?.metrics_observed_at ?? overview?.observed_at : undefined),
  };
}

export function KubernetesPage() {
  const [params, setParams] = useSearchParams();
  const [notice, setNotice] = useState("");
  const catalog = useQuery({ queryKey: ["kubernetes-clusters"], queryFn: api.kubernetesClusters, retry: false, refetchInterval: (query) => query.state.data?.multiple ? 30_000 : false, refetchIntervalInBackground: false });
  const cluster = params.get("cluster") || undefined;
  const missing = Boolean(catalog.data?.multiple && cluster && !catalog.data.clusters.some((entry) => entry.id === cluster));
  useEffect(() => {
    if (!missing) return;
    setNotice("This cluster is no longer available.");
    setParams({}, { replace: true });
  }, [missing, setParams]);
  if (catalog.isPending) return <main className="min-w-0 flex-1 p-4"><SkCard lines={5} /></main>;
  if (catalog.isError) {
    if (catalog.error instanceof ApiError && catalog.error.status === 503 && catalog.error.message === "Kubernetes connector is not configured") return <KubernetesDashboard />;
    if (catalog.error instanceof ApiError && [401, 403].includes(catalog.error.status)) return <main className="min-w-0 flex-1"><TopBar title="Kubernetes" /><div className="p-4"><RetryableError error={catalog.error} onRetry={() => catalog.refetch()} retrying={catalog.isFetching} context="Couldn't load Kubernetes overview" /></div></main>;
    return <main className="min-w-0 flex-1 p-4"><RetryableError error={catalog.error} onRetry={() => catalog.refetch()} retrying={catalog.isFetching} context="Couldn't load clusters" /></main>;
  }
  if (catalog.data.multiple && (!cluster || missing)) return <KubernetesFleet clusters={catalog.data.clusters} notice={notice || (params.get("notice") === "unavailable" ? "This cluster is no longer available." : undefined)} retry={() => void catalog.refetch()} retrying={catalog.isFetching} />;
  const activeCluster = catalog.data.multiple ? cluster : undefined;
  return <KubernetesClusterProvider catalog={catalog.data} cluster={activeCluster}><KubernetesDashboard key={activeCluster ?? "_"} /></KubernetesClusterProvider>;
}

function KubernetesDashboard() {
  const { client: api, key, cluster, multiple, clusters } = useKubernetesCluster();
  const useQuery = useKubernetesQuery;
  const navigate = useNavigate();
  const location = useLocation();
  const queryClient = useQueryClient();
  const [view, setView] = useState<KubernetesExplorerTab>(() =>
    parseExplorerTab(new URLSearchParams(window.location.search).get("view")),
  );
  const [name, setName] = useState("");
  const [workloadNamespace, setWorkloadNamespace] = useState("All");
  const [selected, setSelected] = useState<KubernetesResource | null>(() =>
    parseResourceSelection(window.location.search),
  );
  const [drawerTab, setDrawerTab] = useState<KubernetesDrawerTab>(() =>
    parseDrawerTab(new URLSearchParams(window.location.search).get("tab"), parseResourceSelection(window.location.search)),
  );
  const [paletteOpen, setPaletteOpen] = useState(false);
  const [paletteQuery, setPaletteQuery] = useState("");
  const [debouncedPaletteQuery, setDebouncedPaletteQuery] = useState("");
  const [recentResources, setRecentResources] = useState<string[]>(() => {
    try {
      const value: unknown = JSON.parse(localStorage.getItem(cluster ? `versus.k8s.recent.${cluster}` : "versus.k8s.recent") ?? "[]");
      return Array.isArray(value) ? value.filter((item): item is string => typeof item === "string").slice(0, 8) : [];
    } catch {
      return [];
    }
  });
  const [streamState, setStreamState] = useState<"connecting" | "live" | "reconnecting" | "resyncing">("connecting");
  const [indexStatus, setIndexStatus] = useState<KubernetesIndexStatus | null>(null);
  const resourceFilterRef = useRef<HTMLInputElement>(null);
  const paletteInputRef = useRef<HTMLInputElement>(null);
  const gShortcutPending = useRef(false);
  const [selectedNode, setSelectedNode] = useState<KubernetesResource | null>(
    null,
  );
  const [workloadKind, setWorkloadKind] = useState("All");
  const [usageHistory, setUsageHistory] = useState<{
    namespace: string;
    snapshots: UsageSnapshot[];
  }>({ namespace: "", snapshots: [] });
  const overview = useQuery({
    queryKey: ["kubernetes-overview"],
    queryFn: api.kubernetesOverview,
    retry: false,
  });
  const connectorMissing = overview.error instanceof ApiError
    && overview.error.status === 503
    && overview.error.message === "Kubernetes connector is not configured";
  const overviewAccessDenied = overview.error instanceof ApiError && [401, 403].includes(overview.error.status);
  const clusterUnavailable = multiple && overview.error instanceof ApiError && overview.error.status === 404;
  const clusterQueriesEnabled = !overview.isPending && !connectorMissing && (!multiple || !overviewAccessDenied) && !clusterUnavailable;
  const brokenIssues = useBrokenIssues(view === "overview" && clusterQueriesEnabled && !overviewAccessDenied);

  useEffect(() => {
    if (!clusterUnavailable) return;
    void queryClient.cancelQueries({ queryKey: key() });
    queryClient.removeQueries({ queryKey: key() });
    void queryClient.invalidateQueries({ queryKey: ["kubernetes-clusters"] });
    navigate("/agent/kubernetes?notice=unavailable", { replace: true });
  }, [clusterUnavailable, navigate, queryClient, key]);

  const usage = useQuery({
    queryKey: ["kubernetes-usage"],
    queryFn: () => api.kubernetesUsage(),
    enabled: clusterQueriesEnabled,
    retry: false,
    refetchInterval: usagePollingMilliseconds,
    refetchIntervalInBackground: false,
  });
  const workloadPaging = useCursorPagination(`workloads:${workloadNamespace}:${name.trim()}:${workloadKind}`);
  const workloads = useQuery({
    queryKey: ["kubernetes-workloads", workloadNamespace, name.trim(), workloadKind, workloadPaging.cursor],
    queryFn: () => api.kubernetesWorkloads({
      namespace: workloadNamespace === "All" ? undefined : workloadNamespace,
      kind: workloadKind === "All" ? undefined : workloadKind,
      q: name.trim() || undefined,
      limit: 20,
      cursor: workloadPaging.cursor,
    }),
    enabled: clusterQueriesEnabled,
    retry: false,
  });
  const nodePaging = useCursorPagination("nodes");
  const nodes = useQuery({
    queryKey: ["kubernetes-nodes", nodePaging.cursor],
    queryFn: () => api.kubernetesNodes(nodePaging.cursor),
    enabled: clusterQueriesEnabled,
    retry: false,
  });
  const nodePodPaging = useCursorPagination(`node-pods:${selectedNode?.name ?? ""}`);
  const nodePods = useQuery({
    queryKey: ["kubernetes-node-pods", selectedNode?.name, nodePodPaging.cursor],
    queryFn: () => api.kubernetesNodePods(selectedNode!.name, nodePodPaging.cursor),
    enabled: clusterQueriesEnabled && selectedNode !== null,
    retry: false,
  });
  const detail = useQuery({
    queryKey: [
      "kubernetes-detail",
      selected?.resource_id,
      selected?.namespace,
      selected?.name,
    ],
    queryFn: () =>
      api.kubernetesDescribe(
        selected!.resource_id,
        selected?.namespace ?? "",
        selected!.name,
      ),
    enabled: clusterQueriesEnabled && selected !== null,
    retry: false,
  });
  const workloadDetailEnabled =
    clusterQueriesEnabled && selected !== null && workloadKinds.has(selected.kind);
  const workloadDetail = useQuery({
    queryKey: [
      "kubernetes-workload",
      selected?.kind,
      selected?.namespace,
      selected?.name,
    ],
    queryFn: () =>
      api.kubernetesWorkload(
        selected!.kind,
        selected?.namespace ?? "",
        selected!.name,
      ),
    enabled: workloadDetailEnabled,
    retry: false,
  });
  const [previousLogRequest, setPreviousLogRequest] = useState<{ id: string; container: string; podUID?: string }>();
  const crashLogNavigation = useRef<((event: KubernetesCrashEvent) => boolean) | null>(null);
  const openCrashLogs = (crash: KubernetesCrashEvent) => {
    if (crashLogNavigation.current?.(crash)) { setPreviousLogRequest(undefined); setSelected(null); setDrawerTab("overview"); return; }
    if (selected?.kind !== "Pod" && !["Deployment", "StatefulSet"].includes(selected?.kind ?? "")) {
      selectResource({ resource_id: "core~v1~pods", kind: "Pod", namespace: crash.namespace, name: crash.pod, uid: crash.pod_uid }, "logs");
      setPreviousLogRequest({ id: crash.id, container: crash.container, podUID: crash.pod_uid });
      return;
    }
    setPreviousLogRequest({ id: crash.id, container: crash.container, podUID: crash.pod_uid });
    selectDrawerTab("logs");
  };
  const paletteResults = useQuery({
    queryKey: ["kubernetes-palette", debouncedPaletteQuery],
    queryFn: () => api.kubernetesSearch("", debouncedPaletteQuery, 20),
    enabled: clusterQueriesEnabled && paletteOpen && debouncedPaletteQuery.trim().length > 0,
    retry: false,
  });

  useEffect(() => {
    const timer = window.setTimeout(() => setDebouncedPaletteQuery(paletteQuery.trim()), 200);
    return () => window.clearTimeout(timer);
  }, [paletteQuery]);

  useEffect(() => {
    const onPopState = () => {
      const query = new URLSearchParams(window.location.search);
      const resource = parseResourceSelection(window.location.search);
      setView(parseExplorerTab(query.get("view")));
      setSelected(resource);
      setDrawerTab(parseDrawerTab(query.get("tab"), resource));
    };
    window.addEventListener("popstate", onPopState);
    return () => window.removeEventListener("popstate", onPopState);
  }, []);

  useEffect(() => {
    const query = new URLSearchParams(location.search);
    const resource = parseResourceSelection(location.search);
    setView(parseExplorerTab(query.get("view")));
    setSelected(resource);
    setDrawerTab(parseDrawerTab(query.get("tab"), resource));
  }, [location.search]);

  useEffect(() => {
    const query = new URLSearchParams(window.location.search);
    const requested = query.get("tab");
    if (requested && parseDrawerTab(requested, selected) === "overview") {
      query.delete("tab");
      const suffix = query.toString();
      window.history.replaceState(null, "", `${window.location.pathname}${suffix ? `?${suffix}` : ""}${window.location.hash}`);
    }
  }, [selected, drawerTab]);

  useEffect(() => {
    const onKeyDown = (event: globalThis.KeyboardEvent) => {
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") {
        event.preventDefault();
        setPaletteOpen(true);
        return;
      }
      const target = event.target;
      const editing = target instanceof HTMLElement && (target.isContentEditable || ["INPUT", "SELECT", "TEXTAREA"].includes(target.tagName));
      if (editing) return;
      if (event.key === "/") {
        event.preventDefault();
        setView("resources");
        writeExplorerLocation({ view: "resources" });
        window.requestAnimationFrame(() => resourceFilterRef.current?.focus());
        return;
      }
      if (gShortcutPending.current) {
        const tabs: Record<string, KubernetesExplorerTab> = { o: "overview", i: "issues", t: "timeline", p: "topology" };
        const next = tabs[event.key.toLowerCase()];
        if (next) {
          event.preventDefault();
          setView(next);
          writeExplorerLocation({ view: next });
        }
        gShortcutPending.current = false;
      } else if (event.key.toLowerCase() === "g") {
        gShortcutPending.current = true;
        window.setTimeout(() => { gShortcutPending.current = false; }, 800);
      }
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, []);

  useEffect(() => {
    if (paletteOpen) window.requestAnimationFrame(() => paletteInputRef.current?.focus());
  }, [paletteOpen]);

  useEffect(() => {
    if (!paletteOpen) return;
    const dismiss = (event: globalThis.KeyboardEvent) => {
      if (event.key === "Escape") {
        event.stopPropagation();
        setPaletteOpen(false);
      }
    };
    window.addEventListener("keydown", dismiss, true);
    return () => window.removeEventListener("keydown", dismiss, true);
  }, [paletteOpen]);

  useEffect(() => {
    if (!clusterQueriesEnabled) return;
    const controller = new AbortController();
    let disposed = false;
    let reconnectTimer: number | undefined;
    let invalidateTimer: number | undefined;
    let backoffMilliseconds = 1000;
    const invalidateSnapshots = () => {
      if (invalidateTimer) return;
      invalidateTimer = window.setTimeout(() => {
        invalidateTimer = undefined;
        for (const queryKey of [["kubernetes-overview"], ["kubernetes-graph"], ["kubernetes-namespaces"], ["kubernetes-workloads"], ["kubernetes-nodes"], ["kubernetes-issues"], ["kubernetes-top"], ["kubernetes-diagnosis"], ["kubernetes-drawer-changes"], ["kubernetes-changes"]]) {
          void queryClient.invalidateQueries({ queryKey: key(...queryKey) });
        }
      }, 750);
    };
    const run = async () => {
      while (!disposed) {
        try {
          setStreamState("connecting");
          await api.kubernetesStream(({ event, data }) => {
            if (event === "sync") {
              try {
                setIndexStatus(JSON.parse(data) as KubernetesIndexStatus);
                setStreamState("live");
                backoffMilliseconds = 1000;
              } catch {
                setStreamState("resyncing");
              }
            } else if (event === "records") {
              setStreamState("live");
              invalidateSnapshots();
            } else if (event === "resync") {
              setStreamState("resyncing");
              invalidateSnapshots();
            }
          }, controller.signal);
          if (!disposed) setStreamState("reconnecting");
        } catch (failure) {
          if (disposed) return;
          if (multiple && failure instanceof ApiError && failure.status === 404) {
            void queryClient.cancelQueries({ queryKey: key() });
            queryClient.removeQueries({ queryKey: key() });
            void queryClient.invalidateQueries({ queryKey: ["kubernetes-clusters"] });
            navigate("/agent/kubernetes?notice=unavailable", { replace: true });
            return;
          }
          setStreamState("reconnecting");
        }
        if (disposed) return;
        await new Promise<void>((resolve) => {
          reconnectTimer = window.setTimeout(resolve, backoffMilliseconds);
        });
        backoffMilliseconds = Math.min(backoffMilliseconds * 2, 30_000);
      }
    };
    void run();
    return () => {
      disposed = true;
      controller.abort();
      if (reconnectTimer) window.clearTimeout(reconnectTimer);
      if (invalidateTimer) window.clearTimeout(invalidateTimer);
    };
  }, [queryClient, clusterQueriesEnabled, api, key, multiple, navigate]);

  const selectResource = (resource: KubernetesResource, initialTab?: "timeline" | "logs") => {
    setPreviousLogRequest(undefined);
    setSelected(resource);
    setDrawerTab(initialTab ?? "overview");
    writeExplorerLocation({ resource, drawerTab: initialTab ?? "overview" });
    const recent = `${resource.kind}:${resource.namespace ? `${resource.namespace}/` : ""}${resource.name}`;
    setRecentResources((current) => {
      const next = [recent, ...current.filter((item) => item !== recent)].slice(0, 8);
      try { localStorage.setItem(cluster ? `versus.k8s.recent.${cluster}` : "versus.k8s.recent", JSON.stringify(next)); } catch { /* storage is optional */ }
      return next;
    });
    setPaletteOpen(false);
  };
  const closeResource = () => {
    setPreviousLogRequest(undefined);
    setSelected(null);
    setDrawerTab("overview");
    writeExplorerLocation({ resource: null });
  };
  const selectView = (next: KubernetesExplorerTab) => {
    setView(next);
    writeExplorerLocation({ view: next });
  };
  const selectDrawerTab = (next: KubernetesDrawerTab) => {
    const available = parseDrawerTab(next, selected);
    setDrawerTab(available);
    if (selected) writeExplorerLocation({ resource: selected, drawerTab: available });
  };
  const visibleDrawerTabs = drawerTabs;
  const visibleResources = asArray(workloads.data?.items);
  const nodeItems = asArray(nodes.data?.items);
  const selectedNodePods = asArray(nodePods.data?.items);
  const workloadCounts = workloads.data?.counts ?? {};
  const workloadTotal = workloadKindOptions.reduce((sum, kind) => sum + (workloadCounts[kind] ?? 0), 0);

  useEffect(() => {
    if (!usage.data) return;
    const parsedObservedAt = Date.parse(usage.data.observed_at);
    const snapshot: UsageSnapshot = {
      key: Number.isFinite(parsedObservedAt)
        ? usage.data.observed_at
        : String(usage.dataUpdatedAt),
      observedAt: usage.dataUpdatedAt,
      pods: asArray(usage.data.pods)
        .slice(0, usagePodSampleLimit)
        .map((pod) => ({
          namespace: pod.namespace,
          name: pod.name,
          cpu: pod.cpu,
          memory: pod.memory,
        })),
    };
    setUsageHistory((current) => {
      const snapshots = current.namespace === "" ? current.snapshots : [];
      const cutoff = usage.dataUpdatedAt - usageWindowMilliseconds;
      const retained = snapshots.filter(
        (item) => item.observedAt >= cutoff && item.key !== snapshot.key,
      );
      const next =
        snapshot.observedAt >= cutoff ? [...retained, snapshot] : retained;
      return {
        namespace: "",
        snapshots: next
          .sort((left, right) => left.observedAt - right.observedAt)
          .slice(-usageSnapshotLimit),
      };
    });
  }, [usage.data, usage.dataUpdatedAt]);

  const historyNow = usage.dataUpdatedAt || Date.now();
  const liveMetrics = useMemo(
    () =>
      workloadDetail.data
        ? workloadMetricSamples(
            usageHistory.namespace === "" ? usageHistory.snapshots : [],
            workloadDetail.data,
            selected,
            historyNow,
          )
        : [],
    [
      historyNow,
      selected,
      usageHistory,
      workloadDetail.data,
    ],
  );
  const refreshAll = () => {
    overview.refetch();
    if (!clusterQueriesEnabled) return;
    usage.refetch();
    workloads.refetch();
    nodes.refetch();
    if (selectedNode) nodePods.refetch();
    for (const queryKey of ["kubernetes-issues", "kubernetes-top", "kubernetes-graph", "kubernetes-namespaces", "kubernetes-releases", "kubernetes-traffic", "kubernetes-changes"]) {
      void queryClient.invalidateQueries({ queryKey: key(queryKey) });
    }
  };
  const podsUnavailable = overview.data
    ? categoryUnavailable(overview.data, ["core~v1~pods"])
    : false;
  const nodesUnavailable = overview.data
    ? categoryUnavailable(overview.data, ["core~v1~nodes"])
    : false;
  const usageAccessDenied = usage.error instanceof ApiError && [401, 403].includes(usage.error.status);
  const workloadsAccessDenied = overviewAccessDenied || workloads.error instanceof ApiError && [401, 403].includes(workloads.error.status);
  const nodesAccessDenied = overviewAccessDenied || nodes.error instanceof ApiError && [401, 403].includes(nodes.error.status);
  const aggregateUsage = usageAggregate(usageAccessDenied ? undefined : usage.data, overview.data);
  const overviewWarningMessages = overview.data
    ? [
        ...(overview.data.partial_failures?.length
          ? [
              `Partial cluster visibility: ${[
                ...new Set(
                  overview.data.partial_failures.map((failure) => failure.class),
                ),
              ].join(", ")}`,
            ]
          : []),
        ...(overview.data.truncated
          ? ["Overview reached its collection bound. Some categories are omitted."]
          : []),
      ]
    : [];
  if (clusterUnavailable) return <p role="status" className="p-4 text-sm text-sev-warning">This cluster is no longer available.</p>;
  if (connectorMissing) {
    return (
      <main className="min-w-0 flex-1 overflow-auto">
        <TopBar title="Kubernetes" />
        <div className="mx-auto max-w-7xl p-4 sm:p-6">
          <article aria-labelledby="kubernetes-connector-title" className="card flex max-w-xl flex-wrap items-start gap-4 p-4 sm:p-6">
            <div className="flex size-12 shrink-0 items-center justify-center rounded-control border border-ink-600 bg-ink-800"><ToolIcon iconKey="kubernetes" /></div>
            <div className="min-w-0 flex-1">
              <h1 id="kubernetes-connector-title" className="text-base font-semibold text-ink-50">Kubernetes connector</h1>
              <StateBadge state="needs_integration" />
              <p className="mt-3 text-sm text-ink-300">Kubernetes connector is not configured.</p>
              <div className="mt-4 flex flex-wrap gap-2">
                <a className="btn" href="https://docs.versusincident.com/#/agent/connectors/kubernetes" target="_blank" rel="noopener noreferrer">Documentation <ExternalLink size={13} aria-hidden="true" /></a>
                <button type="button" className="btn" onClick={() => overview.refetch()} disabled={overview.isFetching}><RefreshCw size={13} aria-hidden="true" className={overview.isFetching ? "animate-spin" : undefined} />Check connection</button>
              </div>
            </div>
          </article>
        </div>
      </main>
    );
  }
  return (
    <CrashLogNavigationContext.Provider value={crashLogNavigation}>
    <main className="relative min-h-0 min-w-0 flex-1 overflow-x-hidden overflow-y-auto">
      <TopBar
        title="Kubernetes"
        actions={
          <div className="flex items-center gap-1">
            {multiple && <><Link to="/agent/kubernetes" className="btn">Clusters</Link><select aria-label="Cluster" className="input h-8 w-32 min-w-0 py-0 text-xs sm:w-48" value={cluster} onChange={(event) => {
              const next = new URLSearchParams(window.location.search);
              next.set("cluster", event.target.value);
              for (const parameter of ["r", "tab", "namespace", "root_uid", "center"]) next.delete(parameter);
              navigate(`/agent/kubernetes?${next}`);
            }}>{clusters.map((entry) => <option key={entry.id} value={entry.id}>{entry.display_name || entry.id}</option>)}</select></>}
            <span role="status" aria-live="polite" title={indexStatus ? `Index ${indexStatus.state}${indexStatus.partial ? ", partial" : ""}` : "Kubernetes index status unavailable"} className={`hidden text-2xs sm:inline ${streamState === "live" ? "text-sev-ok" : "text-ink-400"}`}>
              {streamState === "live" ? `Live${indexStatus ? ` (${Math.round(indexStatus.age_s)}s)` : ""}` : streamState === "resyncing" ? "Resyncing" : streamState === "connecting" ? "Connecting" : "Reconnecting"}
            </span>
            {overviewWarningMessages.length > 0 && (
              <OverviewWarningIndicator messages={overviewWarningMessages} />
            )}
            <button
              type="button"
              onClick={refreshAll}
              disabled={overview.isRefetching}
              aria-label="Refresh Kubernetes data"
              title="Refresh Kubernetes data"
              className="inline-flex size-9 items-center justify-center rounded-control text-ink-300 hover:bg-ink-700 hover:text-ink-100 disabled:opacity-50"
            >
              <RefreshCw
                size={15}
                className={overview.isRefetching ? "animate-spin" : undefined}
              />
            </button>
          </div>
        }
      />
      <div className="mx-auto min-w-0 max-w-7xl space-y-3 overflow-x-hidden p-3 sm:space-y-5 sm:p-6">
        {multiple && <nav aria-label="Cluster breadcrumb" className="flex min-w-0 items-center gap-2 text-xs text-ink-400"><Link to="/agent/kubernetes" className="text-link">Clusters</Link><ChevronRight size={12} className="shrink-0" /><span className="truncate text-ink-200">{clusters.find((entry) => entry.id === cluster)?.display_name || cluster}</span></nav>}
        <nav role="tablist" aria-label="Kubernetes views" className="flex min-w-0 gap-1 overflow-x-auto rounded-card border border-ink-500/60 bg-surface p-1 shadow-card" onKeyDown={(event) => {
          if (event.key !== "ArrowRight" && event.key !== "ArrowLeft") return;
          event.preventDefault();
          const current = kubernetesExplorerTabs.findIndex((item) => item.id === view);
          const offset = event.key === "ArrowRight" ? 1 : -1;
          const next = kubernetesExplorerTabs[(current + offset + kubernetesExplorerTabs.length) % kubernetesExplorerTabs.length];
          selectView(next.id);
          window.requestAnimationFrame(() => document.getElementById(`kubernetes-tab-${next.id}`)?.focus());
        }}>
          {kubernetesExplorerTabs.map((item) => {
            const TabIcon = explorerTabIcons[item.id];
            const active = view === item.id;
            return (
              <button type="button" id={`kubernetes-tab-${item.id}`} role="tab" aria-controls="kubernetes-view-panel" aria-selected={active} tabIndex={active ? 0 : -1} onClick={() => selectView(item.id)} key={item.id} className={`inline-flex shrink-0 items-center gap-1.5 rounded-control px-3 py-1.5 text-xs font-medium transition-colors duration-150 ${active ? "bg-accent-subtle text-ink-50 shadow-card ring-1 ring-inset ring-accent/40" : "text-ink-300 hover:bg-ink-600/50 hover:text-ink-100"}`}>
                <TabIcon size={13} aria-hidden="true" className={active ? "text-link" : "text-ink-400"} />
                {item.label}
              </button>
            );
          })}
        </nav>
        <div role="tabpanel" id="kubernetes-view-panel" aria-labelledby={`kubernetes-tab-${view}`} className="space-y-3 sm:space-y-5">
        {view === "overview" && overview.isPending && !overview.data && (
          <div aria-label="Loading Kubernetes overview">
            <SkCard lines={4} />
          </div>
        )}
        {view === "overview" && overview.isError && (!overview.data || overviewAccessDenied) && (
          <RetryableError
            error={overview.error}
            onRetry={() => overview.refetch()}
            retrying={overview.isRefetching}
            context="Couldn't load Kubernetes overview"
          />
        )}

        {view === "overview" && overview.data && (
          <>
            <section
              aria-label="Cluster health"
              className="rise-in relative overflow-hidden rounded-card border border-ink-500/60 bg-surface shadow-card"
            >
              <span aria-hidden="true" className="pointer-events-none absolute inset-x-0 top-0 h-0.5 bg-gradient-to-r from-accent via-accent/40 to-transparent" />
              <ClusterHealthHeader
                overview={overview.data}
                broken={brokenIssues.error instanceof ApiError && [401, 403].includes(brokenIssues.error.status) ? undefined : brokenIssues.data?.broken?.total}
                sync={brokenIssues.error instanceof ApiError && [401, 403].includes(brokenIssues.error.status) ? undefined : brokenIssues.data?.sync}
                onOpenIssues={() => selectView("issues")}
              />
            </section>

            {clusterQueriesEnabled && <KubernetesOverviewTimeline overview={overview.data} onSelectResource={selectResource} onSelectView={selectView} />}

            <div className="grid gap-4 lg:grid-cols-2">
              <CapacityPanel
                icon={Cpu}
                title="CPU capacity"
                formatter={formatCPU}
                meters={capacityMeters(
                  "CPU",
                  aggregateUsage.cpu,
                  podsUnavailable ? undefined : overview.data.requested_cpu,
                  nodesUnavailable ? undefined : overview.data.allocatable_cpu,
                )}
                values={[
                  [
                    "Requested",
                    podsUnavailable ? undefined : overview.data.requested_cpu,
                  ],
                  [
                    "Limited",
                    podsUnavailable ? undefined : overview.data.limited_cpu,
                  ],
                  [
                    "Allocatable",
                    nodesUnavailable ? undefined : overview.data.allocatable_cpu,
                  ],
                  ["Usage", aggregateUsage.cpu],
                ]}
              />
              <CapacityPanel
                icon={Database}
                title="Memory capacity"
                formatter={formatMemory}
                meters={capacityMeters(
                  "Memory",
                  aggregateUsage.memory,
                  podsUnavailable ? undefined : overview.data.requested_memory,
                  nodesUnavailable ? undefined : overview.data.allocatable_memory,
                )}
                values={[
                  [
                    "Requested",
                    podsUnavailable
                      ? undefined
                      : overview.data.requested_memory,
                  ],
                  [
                    "Limited",
                    podsUnavailable ? undefined : overview.data.limited_memory,
                  ],
                  [
                    "Allocatable",
                    nodesUnavailable
                      ? undefined
                      : overview.data.allocatable_memory,
                  ],
                  ["Usage", aggregateUsage.memory],
                ]}
              />
            </div>

            <div className="flex flex-wrap items-center gap-x-4 gap-y-2 rounded-card border border-ink-500/50 bg-surface/70 px-4 py-2.5 text-xs text-ink-300">
              <span className="inline-flex items-center gap-1.5">
                <Activity size={13} className={aggregateUsage.fresh ? "text-sev-ok" : "text-ink-400"} aria-hidden="true" />
                Metrics {aggregateUsage.availability}
              </span>
              <span>
                source{" "}
                {aggregateUsage.source?.replace("_", " ") ?? "unavailable"}
              </span>
              <span>Aggregate {formatCPU(aggregateUsage.cpu)} CPU, {formatMemory(aggregateUsage.memory)} memory</span>
              <span role="status">
                {usage.isPending
                  ? "Loading usage snapshot"
                  : usage.isError
                    ? aggregateUsage.fromOverview ? "Usage snapshot unavailable; using overview snapshot" : "Usage snapshot unavailable"
                    : `Usage ${aggregateUsage.availability === "unavailable" ? "unavailable" : aggregateUsage.fresh ? "fresh" : "stale"}; ${usage.data?.pod_metrics?.complete === true ? usage.data.pod_metrics.total : "Unavailable"} pod metrics; ${usage.data?.node_metrics?.complete === true ? usage.data.node_metrics.total : "Unavailable"} node metrics${usage.data?.truncated ? "; sample list partial" : ""}${aggregateUsage.incomplete ? aggregateUsage.fromOverview ? "; latest metrics source incomplete; using overview snapshot" : "; metrics source incomplete; aggregate unavailable" : ""}`}
              </span>
              {aggregateUsage.observedAt && (
                <span>
                  Sampled{" "}
                  {new Date(aggregateUsage.observedAt).toLocaleString()}
                </span>
              )}
            </div>

          </>
        )}

        {view === "overview" && clusterQueriesEnabled && <>
        {overview.data && <KubernetesOverviewInsights overview={overview.data} indexStatus={indexStatus} indexLive={streamState === "live" && !overview.isError} unavailable={overviewAccessDenied} onSelectView={selectView} />}
        </>}
        {view === "resources" && clusterQueriesEnabled && (
        <div className="min-w-0 w-full">
          <SectionFrame
            title="Workloads"
            icon={<Boxes size={16} />}
            trailing={
              <span className="text-xs text-ink-400">
                {workloadsAccessDenied ? "Inventory unavailable" : `${workloadTotal} total`}
              </span>
            }
          >
            <div className="grid gap-2 border-b border-ink-700 p-3 sm:grid-cols-[minmax(0,1fr)_14rem]">
              <div className="relative min-w-0 flex-1">
                <Search
                  size={14}
                  className="pointer-events-none absolute left-3 top-2.5 text-ink-400"
                  aria-hidden="true"
                />
                <input
                  ref={resourceFilterRef}
                  aria-label="Resource name"
                  className="w-full rounded-control border border-ink-600 bg-ink-900 py-2 pl-9 pr-3 text-sm text-ink-50"
                  value={name}
                  onChange={(event) => setName(event.target.value)}
                  placeholder="Search resource names"
                />
              </div>
              <label className="text-2xs text-ink-400">
                <span className="sr-only">Workload namespace</span>
                <input
                  aria-label="Workload namespace"
                  className="input h-full w-full"
                  value={workloadNamespace === "All" ? "" : workloadNamespace}
                  onChange={(event) => setWorkloadNamespace(event.target.value || "All")}
                  placeholder="All namespaces"
                />
              </label>
            </div>
            <KindTabs label="Workload kind" kinds={workloadKindOptions} counts={workloadsAccessDenied ? {} : workloadCounts} selected={workloadKind} onSelect={setWorkloadKind} />
            {workloads.isPending && !workloads.data && (
              <div className="p-4">
                <SkCard lines={4} />
              </div>
            )}
            {(workloadsAccessDenied || workloads.isError && !workloads.data) && (
              <p role="status" className="p-4 text-sm text-sev-warning">
                Resource inventory is unavailable.
              </p>
            )}
            {workloads.data && !workloadsAccessDenied && (
                <div className="overflow-hidden">
                  {visibleResources.length > 0 && (
                    <div className="hidden grid-cols-[minmax(0,1.4fr)_minmax(0,1fr)_7rem_minmax(7rem,1fr)_1.5rem] gap-3 border-b border-ink-700 bg-ink-900/40 px-4 py-2 text-2xs font-medium uppercase text-ink-400 sm:grid">
                      <span>Name</span>
                      <span>Namespace</span>
                      <span>Kind</span>
                      <span>Readiness / status</span>
                      <span className="sr-only">Select</span>
                    </div>
                  )}
                  <div
                    role="list"
                    className="max-h-[min(45vh,32rem)] divide-y divide-ink-700 overflow-y-auto"
                  >
                    {visibleResources.map((item) => (
                      <ResourceRow
                        key={`${item.resource_id}:${item.namespace}:${item.name}`}
                        item={item}
                        selected={
                          selected?.resource_id === item.resource_id &&
                          selected?.namespace === item.namespace &&
                          selected?.name === item.name
                        }
                        onSelect={() => selectResource(item)}
                      />
                    ))}
                  </div>
                  {visibleResources.length === 0 && (
                    <EmptyState>
                      {name.trim() ? "No matching workloads." : workloadNamespace !== "All" ? "No workloads in this namespace." : workloadKind !== "All" ? `No ${workloadKind} resources in this scope.` : "No workloads in this scope."}
                    </EmptyState>
                  )}
                  <CursorPagination state={workloadPaging} next={workloads.data?.next} />
                </div>
              )}
            {!workloadsAccessDenied && (workloads.data?.truncated ||
            workloads.data?.partial_failures?.length) ? (
              <p
                role="status"
                className="min-w-0 break-words border-t border-ink-700 px-4 py-2 text-xs text-sev-warning"
              >
                Inventory is bounded or partially available.
              </p>
            ) : null}
            <PartialFailuresDisclosure failures={workloadsAccessDenied ? undefined : workloads.data?.partial_failures} />
          </SectionFrame>

        </div>
        )}
        {view === "nodes" && clusterQueriesEnabled && (
        <SectionFrame
          title="Nodes"
          icon={<Server size={16} />}
          trailing={
            nodes.data && !nodesAccessDenied ? (
              <span className="text-xs text-ink-400">
                {nodeItems.length} nodes
              </span>
            ) : undefined
          }
        >
          {selectedNode && !nodesAccessDenied ? (
            <NodePods
              node={selectedNode}
              isPending={nodePods.isPending}
              isError={nodePods.isError}
              truncated={Boolean(nodePods.data?.truncated)}
              partialFailures={nodePods.data?.partial_failures}
              items={selectedNodePods}
              pagination={nodePodPaging}
              next={nodePods.data?.continue}
              onClose={() => setSelectedNode(null)}
            />
          ) : (
            <>
              {nodes.isPending && !nodes.data && (
                <div className="p-4">
                  <SkCard lines={3} />
                </div>
              )}
              {(nodesAccessDenied || nodes.isError && !nodes.data) && (
                <p role="status" className="p-4 text-sm text-sev-warning">
                  Nodes are unavailable.
                </p>
              )}
              {nodes.data && !nodesAccessDenied && (
                <div className="min-w-0 overflow-hidden">
                  <div role="list" className="max-h-[min(55vh,32rem)] divide-y divide-ink-700 overflow-y-auto">
                    {nodeItems.map((node) => (
                      <NodeRow
                        node={node}
                        onSelect={() => setSelectedNode(node)}
                        key={node.name}
                      />
                    ))}
                  </div>
                  {nodeItems.length === 0 && (
                    <EmptyState>No nodes in this cluster.</EmptyState>
                  )}
                  <CursorPagination state={nodePaging} next={nodes.data.continue} />
                </div>
              )}
              {!nodesAccessDenied && (nodes.data?.truncated ||
                nodes.data?.partial_failures?.length) && (
                <p
                  role="status"
                  className="min-w-0 break-words border-t border-ink-700 px-4 py-2 text-xs text-sev-warning"
                >
                  Node inventory is partial
                  {nodes.data.partial_failures?.some(
                    (failure) => failure.class === "forbidden",
                  )
                    ? " because nodes are forbidden"
                    : ""}
                  .
                </p>
              )}
              <PartialFailuresDisclosure failures={nodesAccessDenied ? undefined : asArray(nodes.data?.partial_failures)} />
            </>
          )}
        </SectionFrame>
        )}
        {!["overview", "resources", "nodes"].includes(view) && clusterQueriesEnabled && (
          <KubernetesExplorerTabContent tab={view} onSelectResource={selectResource} clusterId={overview.data?.cluster_id} />
        )}
        </div>
      </div>

      <PeekPanel
        open={clusterQueriesEnabled && selected !== null}
        onClose={closeResource}
        size="wide"
        appearance="resource"
        expandable
        headingLabel={selected ? `${selected.kind} ${selected.namespace ? `${selected.namespace}/` : ""}${selected.name}` : undefined}
        headerActions={selected && (
          <button type="button" aria-label="Investigate" title="Investigate" className="btn btn-primary" onClick={() => {
            if (!selected) return;
            const query = new URLSearchParams({ provider: "kubernetes", cluster: cluster ?? overview.data?.cluster_id ?? "", resource_id: selected.resource_id, name: selected.name, include_changes: "true" });
            if (selected.namespace) query.set("namespace", selected.namespace);
            const uid = detail.data?.resource.uid ?? selected.uid;
            if (uid) query.set("uid", uid);
            navigate(`/agent/chat?${query}`);
          }}>
            <Activity size={14} aria-hidden="true" /><span className="hidden sm:inline">Investigate</span>
          </button>
        )}
        title={
          selected
            ? <ResourceHeader resource={!detail.isError && detail.data?.resource || selected} workload={!workloadDetail.isError ? workloadDetail.data : undefined} />
            : "Resource detail"
        }
      >
        {clusterQueriesEnabled && selected && <div className="resource-detail-content" data-workload={["Deployment", "StatefulSet"].includes(selected.kind)}>
          <div className="resource-navigation">
          <div role="tablist" aria-label="Resource detail views" className="resource-tabs flex min-w-0 gap-0 overflow-x-auto border-b border-ink-700" onKeyDown={(event) => {
            if (!["ArrowRight", "ArrowLeft", "Home", "End"].includes(event.key)) return;
            event.preventDefault();
            const focused = event.target instanceof HTMLElement ? event.target.closest('[role="tab"]')?.id : undefined;
            const keyboardTabs = window.matchMedia?.("(max-width: 639px)").matches && ["Deployment", "StatefulSet"].includes(selected.kind) ? visibleDrawerTabs.slice(0, 3) : visibleDrawerTabs;
            const current = keyboardTabs.findIndex((item) => focused ? focused === `kubernetes-drawer-tab-${item.id}` : item.id === drawerTab);
            const offset = event.key === "ArrowRight" ? 1 : -1;
            const next = keyboardTabs[event.key === "Home" ? 0 : event.key === "End" ? keyboardTabs.length - 1 : (current + offset + keyboardTabs.length) % keyboardTabs.length];
            selectDrawerTab(next.id);
            window.requestAnimationFrame(() => document.getElementById(`kubernetes-drawer-tab-${next.id}`)?.focus());
          }}>
            {visibleDrawerTabs.map((item) => <button type="button" id={`kubernetes-drawer-tab-${item.id}`} role="tab" aria-controls="kubernetes-drawer-panel" aria-selected={drawerTab === item.id} tabIndex={drawerTab === item.id ? 0 : -1} onClick={() => selectDrawerTab(item.id)} className={`min-w-0 border-b-2 px-2 py-2 text-xs sm:shrink-0 sm:px-2.5 ${drawerTab === item.id ? "border-accent text-ink-50" : "border-transparent text-ink-300 hover:text-ink-100"}`} key={item.id}>{item.label}</button>)}
          </div>
          {["Deployment", "StatefulSet"].includes(selected.kind) && <details className="resource-more-tabs"><summary aria-label="More resource tabs" aria-haspopup="menu" className={["yaml", "related", "timeline"].includes(drawerTab) ? "text-link" : ""}>More tabs <ChevronDown size={12} aria-hidden="true" /></summary><div role="menu" aria-label="More resource views" onKeyDown={(event) => {
            if (event.key === "Escape") { event.preventDefault(); const details = event.currentTarget.closest("details"); details?.removeAttribute("open"); details?.querySelector("summary")?.focus(); return; }
            if (!["ArrowDown", "ArrowUp", "Home", "End"].includes(event.key)) return;
            event.preventDefault();
            const items = [...event.currentTarget.querySelectorAll<HTMLButtonElement>('[role="menuitem"]')];
            const index = items.findIndex((item) => item === document.activeElement);
            const next = event.key === "Home" ? 0 : event.key === "End" ? items.length - 1 : (index + (event.key === "ArrowUp" ? -1 : 1) + items.length) % items.length;
            items[next]?.focus();
          }}>{visibleDrawerTabs.slice(3).map((item) => <button type="button" role="menuitem" key={item.id} onClick={(event) => { selectDrawerTab(item.id); const details = event.currentTarget.closest("details"); details?.removeAttribute("open"); details?.querySelector("summary")?.focus(); }}>{item.label}</button>)}</div></details>}
          </div>
          <div role="tabpanel" className="resource-tabpanel" data-view={drawerTab} id="kubernetes-drawer-panel" aria-labelledby={`kubernetes-drawer-tab-${drawerTab}`}>
          {detail.isPending && <SkCard lines={5} />}
          {detail.isError && <RetryableError error={detail.error} onRetry={() => detail.refetch()} retrying={detail.isRefetching} context="Couldn't load resource detail" />}
          {detail.data && !detail.isError && <>
            <div hidden={drawerTab !== "overview"} className="resource-overview space-y-4">
              <ResourceMetrics resource={detail.data.resource} workload={workloadDetail.isError ? undefined : workloadDetail.data} liveSamples={liveMetrics} onCrash={openCrashLogs} />
              <ResourceFacts resource={detail.data.resource} workload={workloadDetail.isError ? undefined : workloadDetail.data} onOpen={selectResource} />
            </div>
            {["Pod", "Deployment", "StatefulSet"].includes(selected.kind) ? <div hidden={drawerTab !== "overview" && drawerTab !== "logs"} className="resource-log-slot" data-full={drawerTab === "logs"}><PodLogViewer key={`${cluster}:${selected.resource_id}/${selected.namespace}/${selected.name}/${detail.data.resource.uid}`} resource={detail.data.resource} compact={drawerTab !== "logs"} onOpenFull={() => selectDrawerTab("logs")} previousRequest={previousLogRequest} /></div> : drawerTab === "logs" && <p className="text-xs text-ink-400">Live logs are available for Pods, Deployments and StatefulSets.</p>}
            {drawerTab === "events" && <ResourceEvents events={detail.data.events ?? []} />}
            {drawerTab === "yaml" && <ResourceYaml resource={detail.data.resource} />}
            {drawerTab === "related" && <ResourceRelations resource={detail.data.resource} related={detail.data.related_resources} onOpen={selectResource} />}
            {drawerTab === "timeline" && <ResourceTimeline resource={detail.data.resource} onCrash={openCrashLogs} />}
          </>}
          </div>
        </div>}
      </PeekPanel>
      {paletteOpen && <CommandPalette
        inputRef={paletteInputRef}
        query={paletteQuery}
        onQueryChange={setPaletteQuery}
        results={asArray(paletteResults.data?.items)}
        loading={paletteResults.isPending && debouncedPaletteQuery.length > 0}
        partial={Boolean(paletteResults.data?.truncated || paletteResults.data?.partial_failures?.length)}
        recent={recentResources}
        onRecent={(item) => setPaletteQuery(item.slice(item.lastIndexOf("/") + 1))}
        onSelectResource={selectResource}
        onSelectView={(next) => { selectView(next); setPaletteOpen(false); }}
        onClose={() => setPaletteOpen(false)}
      />}
    </main>
    </CrashLogNavigationContext.Provider>
  );
}

function CommandPalette({
  inputRef,
  query,
  onQueryChange,
  results,
  loading,
  partial,
  recent,
  onRecent,
  onSelectResource,
  onSelectView,
  onClose,
}: {
  inputRef: Ref<HTMLInputElement>;
  query: string;
  onQueryChange: (value: string) => void;
  results: KubernetesResource[];
  loading: boolean;
  partial: boolean;
  recent: string[];
  onRecent: (value: string) => void;
  onSelectResource: (resource: KubernetesResource) => void;
  onSelectView: (tab: KubernetesExplorerTab) => void;
  onClose: () => void;
}) {
  const matchingTabs = kubernetesExplorerTabs.filter((tab) => tab.label.toLocaleLowerCase().includes(query.trim().toLocaleLowerCase()));
  return <div className="fixed inset-0 z-overlay flex items-start justify-center bg-black/60 px-3 pt-[10vh]" role="dialog" aria-modal="true" aria-label="Kubernetes command palette" onMouseDown={(event) => { if (event.target === event.currentTarget) onClose(); }}>
    <section className="card w-full max-w-2xl overflow-hidden shadow-overlay">
      <div className="flex items-center gap-3 border-b border-ink-700 px-4 py-3"><Search size={16} className="shrink-0 text-ink-400"/><input ref={inputRef} role="combobox" aria-label="Search Kubernetes resources and views" aria-expanded="true" aria-controls="kubernetes-palette-results" className="min-w-0 flex-1 bg-transparent text-sm text-ink-50 outline-none placeholder:text-ink-400" value={query} onChange={(event) => onQueryChange(event.target.value)} placeholder="Search resources or views"/><button type="button" className="btn-icon" aria-label="Close command palette" title="Close command palette" onClick={onClose}><X size={14}/></button></div>
      <div id="kubernetes-palette-results" role="listbox" className="max-h-[65vh] overflow-y-auto">
        {matchingTabs.length > 0 && <div className="border-b border-ink-700 p-2"><p className="px-2 py-1 text-2xs uppercase text-ink-400">Views</p>{matchingTabs.map((tab) => <button type="button" role="option" aria-selected="false" className="block w-full rounded-control px-3 py-2 text-left text-sm text-ink-200 hover:bg-ink-800" onClick={() => onSelectView(tab.id)} key={tab.id}>{tab.label}</button>)}</div>}
        {!query.trim() && recent.length > 0 && <div className="border-b border-ink-700 p-2"><p className="px-2 py-1 text-2xs uppercase text-ink-400">Recent resources</p>{recent.map((item) => <button type="button" role="option" aria-selected="false" className="block w-full truncate rounded-control px-3 py-2 text-left text-xs text-ink-200 hover:bg-ink-800" onClick={() => onRecent(item)} key={item}>{item}</button>)}</div>}
        {loading && <p role="status" className="p-4 text-xs text-ink-400">Searching discovered resources.</p>}
        {results.map((resource) => <button type="button" role="option" aria-selected="false" className="flex w-full items-center justify-between gap-3 border-b border-ink-700 px-4 py-3 text-left hover:bg-ink-800" onClick={() => onSelectResource(resource)} key={`${resource.resource_id}:${resource.namespace}:${resource.name}`}><span className="min-w-0"><span className="block truncate text-sm text-ink-100">{resource.name}</span><span className="block truncate text-2xs text-ink-400">{resource.kind} {resource.namespace || "Cluster scope"}</span></span><span className="pill">Open</span></button>)}
        {query.trim() && !loading && results.length === 0 && <p role="status" className="p-4 text-xs text-ink-400">No matching resources found.</p>}
        {partial && <p role="status" className="border-t border-ink-700 px-4 py-2 text-2xs text-sev-warning">Search results are bounded or partial.</p>}
      </div>
      <p className="border-t border-ink-700 px-4 py-2 text-2xs text-ink-400">Search uses projected names and metadata only.</p>
    </section>
  </div>;
}

  function SectionFrame({
  title,
  icon,
  trailing,
  children,
}: {
  title: string;
  icon: ReactNode;
  trailing?: ReactNode;
  children: ReactNode;
}) {
  return (
    <section className="rise-in card min-w-0 overflow-hidden" aria-label={title}>
      <header className="card-header min-w-0">
        <h2 className="card-title flex min-w-0 items-center gap-2">
          <span className="inline-flex size-7 shrink-0 items-center justify-center rounded-control bg-accent/10 text-link [&>svg]:size-3.5">
            {icon}
          </span>
          <span>{title}</span>
        </h2>
        {trailing}
      </header>
      {children}
    </section>
  );
}

function KindTabs({
  label,
  kinds,
  counts = {},
  selected,
  onSelect,
}: {
  label: string;
  kinds: string[];
  counts?: Record<string, number>;
  selected: string;
  onSelect: (kind: string) => void;
}) {
  const options = ["All", ...kinds];
  return (
    <div className="border-b border-ink-700 px-3 py-2">
      <label className="block text-2xs text-ink-400 sm:hidden">
        {label}
        <select
          aria-label={label}
          className="input mt-1 w-full"
          value={selected}
          onChange={(event) => onSelect(event.target.value)}
        >
          {options.map((kind) => (
            <option value={kind} key={kind}>
              {kind}{counts[kind] === undefined ? "" : ` (${counts[kind]})`}
            </option>
          ))}
        </select>
      </label>
      <div
        role="tablist"
        aria-label={label}
        className="hidden flex-wrap gap-1 sm:flex"
      >
        {options.map((kind) => {
          const active = selected === kind;
          return (
            <button
              type="button"
              role="tab"
              aria-selected={active}
              className={`rounded-control px-3 py-1.5 text-xs font-medium ${active ? "bg-accent-subtle text-ink-50" : "text-ink-300 hover:bg-ink-800 hover:text-ink-100"}`}
              onClick={() => onSelect(kind)}
              key={kind}
            >
              {kind}
              <span aria-hidden="true" className="ml-1 tabular-nums text-ink-400">{kind === "All" ? kinds.reduce((sum, item) => sum + (counts[item] ?? 0), 0) : counts[kind] ?? 0}</span>
            </button>
          );
        })}
      </div>
    </div>
  );
}

function categoryUnavailable(
  overview: {
    omitted_categories?: string[] | null;
    partial_failures?: Array<{ resource_id?: string }> | null;
  },
  resourceIDs: readonly string[],
): boolean {
  return (
    resourceIDs.some((resourceID) =>
      asArray(overview.omitted_categories).includes(resourceID),
    ) ||
    asArray(overview.partial_failures).some(
      (failure) =>
        failure.resource_id !== undefined &&
        resourceIDs.includes(failure.resource_id),
    )
  );
}

type CapacityMeterValue = {
  label: string;
  name: string;
  percent: number | null;
  missing: string;
};

function capacityPercent(
  value: string | undefined,
  allocatable: string | undefined,
): number | null {
  const amount = parseQuantity(value);
  const total = parseQuantity(allocatable);
  if (amount === null || amount < 0 || total === null || total <= 0) return null;
  const percent = (amount / total) * 100;
  return Number.isFinite(percent) ? percent : null;
}

function capacityMeters(
  resource: "CPU" | "Memory",
  usage: string | undefined,
  requested: string | undefined,
  allocatable: string | undefined,
): CapacityMeterValue[] {
  const total = parseQuantity(allocatable);
  const allocatableMissing = total === null || total <= 0;
  return [
    {
      label: "In use",
      name: `${resource} in use`,
      percent: capacityPercent(usage, allocatable),
      missing: allocatableMissing ? "Needs allocatable capacity" : "Needs usage metrics",
    },
    {
      label: "Reserved by requests",
      name: `${resource} reserved by requests`,
      percent: capacityPercent(requested, allocatable),
      missing: allocatableMissing ? "Needs allocatable capacity" : "Needs pod requests",
    },
  ];
}

function CapacityMeter({ meter }: { meter: CapacityMeterValue }) {
  const { percent } = meter;
  const shown = percent === null ? null : `${formatAmount(percent, percent < 10 ? 1 : 0)}% of allocatable`;
  const tone =
    percent === null
      ? ""
      : percent >= 90
        ? "bg-sev-critical-solid"
        : percent >= 70
          ? "bg-sev-warn-solid"
          : "bg-gradient-to-r from-accent to-link";
  return (
    <div className="min-w-0">
      <div className="flex items-baseline justify-between gap-3 text-2xs">
        <span className="text-ink-300">{meter.label}</span>
        <span className={percent === null ? "text-ink-400" : "font-semibold tabular-nums text-ink-100"}>
          {shown ?? meter.missing}
        </span>
      </div>
      {percent === null ? (
        <div aria-hidden="true" className="mt-1.5 h-2 rounded-full bg-ink-600/70 [background-image:repeating-linear-gradient(135deg,transparent_0_6px,rgb(var(--ink-500)/0.5)_6px_8px)]" />
      ) : (
        <div
          role="meter"
          aria-label={meter.name}
          aria-valuemin={0}
          aria-valuemax={100}
          aria-valuenow={Math.min(100, Math.round(percent))}
          aria-valuetext={shown ?? undefined}
          className="mt-1.5 h-2 overflow-hidden rounded-full bg-ink-600"
        >
          <div
            className={`h-full rounded-full transition-[width] duration-700 ease-out ${tone}`}
            style={{ width: `${Math.min(100, Math.max(percent > 0 ? 2 : 0, percent))}%` }}
          />
        </div>
      )}
    </div>
  );
}

function ClusterHealthHeader({
  overview,
  broken,
  sync,
  onOpenIssues,
}: {
  overview: {
    observed_at: string;
    nodes: number;
    ready_nodes: number;
    warnings: number;
    truncated: boolean;
    omitted_categories?: string[] | null;
    partial_failures?: Array<{ resource_id?: string; scope?: string; class: string }> | null;
  };
  onOpenIssues: () => void;
  broken?: number;
  sync?: { state: string; age_s: number; partial: boolean };
}) {
  const { cluster } = useKubernetesCluster();
  const warnings: KubernetesWarning[] = failureWarnings(overview.partial_failures);
  for (const category of [...new Set(overview.omitted_categories ?? [])].sort()) {
    if (!overview.partial_failures?.some((failure) => failure.resource_id === category)) warnings.push({ id: `omitted:${category}`, content: `Coverage unavailable: ${category}.` });
  }
  if (overview.truncated) warnings.push({ id: "overview-cap", content: "Overview inventory is bounded; coverage is incomplete." });
  const partial = overview.truncated || Boolean(warnings.length) || Boolean(sync?.partial);
  const syncLabel = !partial && sync?.state === "ready" ? `Synced ${Math.round(sync.age_s)}s` : sync?.state === "ready" || sync?.state === "stale" ? `Snapshot ${Math.round(sync.age_s)}s ago` : "Sync unknown";
  const observed = Date.parse(overview.observed_at);
  return (
    <header className="relative grid grid-cols-[minmax(0,1fr)_auto] items-center gap-2 px-3 py-2 sm:flex sm:flex-wrap sm:justify-between sm:px-4 sm:py-3">
      <div className="flex min-w-0 items-center gap-3">
        <span className="hidden size-9 shrink-0 items-center justify-center rounded-card bg-accent/15 text-link ring-1 ring-inset ring-accent/30 sm:inline-flex">
          <ShipWheel size={18} aria-hidden="true" />
        </span>
        <div className="min-w-0">
          <h2 className="flex flex-wrap items-center gap-1 text-xs font-semibold text-ink-50 sm:gap-2 sm:text-sm">
            Cluster health
            <span className={`pill ${!partial && sync?.state === "ready" ? "pill-good" : "text-ink-300"}`}>
              {syncLabel}
            </span>
          </h2>
          <p className="mt-0.5 text-2xs text-ink-400">
            {Number.isFinite(observed)
              ? `Snapshot ${new Date(observed).toLocaleTimeString([], { hour12: false })}`
              : "Snapshot time not reported"}
          </p>
        </div>
      </div>
      <div className="grid grid-cols-[auto_auto] items-center gap-x-2 gap-y-1 sm:flex sm:flex-wrap sm:gap-2">
      <button type="button" className={`pill ${broken ? "pill-bad" : ""}`} onClick={onOpenIssues}>{broken === undefined ? "Broken unknown" : `${broken} broken`}</button>
      <button type="button" className="pill pill-warn col-start-1 row-start-2" onClick={onOpenIssues}>{overview.warnings} warnings</button>
      <button type="button" className="col-start-2 row-span-2 row-start-1 inline-flex size-8 items-center justify-center rounded-control hover:bg-ink-600 focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent sm:btn sm:w-auto" aria-label="Review issues" title="Review issues" onClick={onOpenIssues}>
        <AlertTriangle size={13} aria-hidden="true" className="text-sev-warning" />
        <span className="hidden sm:inline">Review issues</span>
        <ChevronRight size={13} aria-hidden="true" className="hidden sm:block" />
      </button>
      </div>
      {warnings.length > 0 && <div className="col-span-2"><KubernetesWarnings warnings={warnings} scope={JSON.stringify([cluster])} /></div>}
    </header>
  );
}

function CapacityPanel({
  icon: Icon,
  title,
  values,
  meters,
  formatter,
}: {
  icon: typeof Cpu;
  title: string;
  values: Array<[string, string | undefined]>;
  meters: CapacityMeterValue[];
  formatter: (value: string | undefined) => string;
}) {
  return (
    <section className="rise-in card relative overflow-hidden transition-colors duration-200 hover:border-ink-400/60">
      <span aria-hidden="true" className="pointer-events-none absolute inset-x-0 top-0 h-0.5 bg-gradient-to-r from-accent/80 to-transparent" />
      <header className="card-header">
        <h2 className="card-title flex items-center gap-2">
          <span className="inline-flex size-7 items-center justify-center rounded-control bg-accent/10 text-link">
            <Icon size={14} aria-hidden="true" />
          </span>
          {title}
        </h2>
      </header>
      <div className="space-y-3 border-b border-ink-500/40 px-4 py-3.5">
        {meters.map((meter) => (
          <CapacityMeter meter={meter} key={meter.label} />
        ))}
      </div>
      <dl className="grid grid-cols-2 divide-x divide-y divide-ink-700 sm:grid-cols-4 sm:divide-y-0">
        {values.map(([label, value]) => {
          const formatted = formatter(value);
          return (
            <div className="min-w-0 p-3" key={label}>
              <dt className="text-2xs uppercase tracking-wider text-ink-400">{label}</dt>
              <dd
                className={`mt-1 truncate text-sm font-semibold tabular-nums ${formatted === "Unavailable" ? "text-ink-400" : "text-ink-100"}`}
                title={formatted}
              >
                {formatted}
              </dd>
            </div>
          );
        })}
      </dl>
    </section>
  );
}

function ResourceRow({
  item,
  selected,
  onSelect,
}: {
  item: KubernetesResource;
  selected: boolean;
  onSelect: () => void;
}) {
  const condition = asArray(item.conditions).find(
    (entry) => entry.type === "Ready" || entry.status === "False",
  );
  const unhealthy = condition?.status === "False";
  const status = workloadStatus(item);
  return (
    <div role="listitem">
      <button
        type="button"
        onClick={onSelect}
        aria-label={`Select ${item.kind} ${item.namespace ? `${item.namespace}/` : ""}${item.name}`}
        className={`grid w-full grid-cols-[minmax(0,1fr)_auto] items-center gap-3 px-4 py-3 text-left hover:bg-ink-800/60 sm:grid-cols-[minmax(0,1.4fr)_minmax(0,1fr)_7rem_minmax(7rem,1fr)_1.5rem] ${selected ? "bg-ink-800" : ""}`}
      >
        <div className="min-w-0">
          <span className="block truncate text-sm font-medium text-ink-50">
            {item.name}
          </span>
          <p className="mt-1 truncate text-2xs text-ink-400 sm:hidden">
            {item.namespace || "cluster scope"}
          </p>
        </div>
        <span className="hidden truncate text-xs text-ink-300 sm:block">
          {item.namespace || "cluster scope"}
        </span>
        <span className="pill w-fit">{item.kind}</span>
        <span
          className={`col-span-2 flex min-w-0 items-center gap-1.5 text-xs sm:col-span-1 ${unhealthy ? "text-sev-warning" : "text-ink-300"}`}
        >
          {unhealthy && (
            <AlertTriangle size={13} className="shrink-0" aria-hidden="true" />
          )}
          <span className="truncate">{status}</span>
        </span>
        <ChevronRight
          size={14}
          className="hidden text-ink-500 sm:block"
          aria-hidden="true"
        />
      </button>
    </div>
  );
}

function OverviewWarningIndicator({ messages }: { messages: string[] }) {
  return (
    <div className="group relative">
      <button
        type="button"
        aria-label={`${messages.length} Kubernetes overview ${messages.length === 1 ? "warning" : "warnings"}`}
        aria-describedby="kubernetes-overview-warnings"
        className="inline-flex size-9 items-center justify-center rounded-control text-sev-warning hover:bg-sev-warning/10 focus-visible:bg-sev-warning/10"
      >
        <AlertTriangle size={16} />
      </button>
      <div
        id="kubernetes-overview-warnings"
        role="tooltip"
        className="pointer-events-none absolute right-0 top-full z-tooltip mt-1 hidden w-72 max-w-[calc(100vw-1rem)] border border-ink-600 bg-ink-900 p-3 text-xs text-ink-100 shadow-xl group-hover:block group-focus-within:block"
      >
        <p className="font-semibold text-sev-warning">Cluster visibility warning</p>
        <ul className="mt-2 space-y-1.5">
          {messages.map((message) => (
            <li className="break-words" key={message}>
              {message}
            </li>
          ))}
        </ul>
      </div>
    </div>
  );
}

function workloadStatus(item: KubernetesResource): string {
  const ready = asArray(item.conditions).find(
    (entry) => entry.type === "Ready",
  );
  if (ready)
    return ready.status === "True" ? "Ready" : ready.reason || "Not ready";
  const failed = asArray(item.conditions).find(
    (entry) => entry.status === "False",
  );
  if (failed) return failed.reason || `${failed.type} false`;
  const summary = item.summary ?? {};
  if (typeof summary.phase === "string" && summary.phase) return summary.phase;
  const count = (...keys: string[]) => {
    for (const key of keys) {
      const value = Number(summary[key]);
      if (summary[key] !== undefined && summary[key] !== "" && Number.isFinite(value)) return value;
    }
    return null;
  };
  const readyCount = count("ready_replicas", "readyReplicas", "numberReady");
  const desiredCount = count("desired_replicas", "replicas", "desiredNumberScheduled");
  if (readyCount !== null && desiredCount !== null)
    return `${readyCount}/${desiredCount} ready`;
  return "Unknown";
}

function EmptyState({
  icon,
  children,
}: {
  icon?: ReactNode;
  children: ReactNode;
}) {
  return (
    <div className="flex min-h-28 flex-col items-center justify-center gap-2 p-5 text-center text-sm text-ink-400">
      {icon}
      {children}
    </div>
  );
}

function NodeRow({
  node,
  onSelect,
}: {
  node: KubernetesResource;
  onSelect: () => void;
}) {
  const summary = node.summary ?? {};
  const ready = asArray(node.conditions).find(
    (condition) => condition.type === "Ready",
  );
  const status =
    ready?.status === "True" ? "Ready" : ready?.reason || "Not ready";
  return (
    <div role="listitem">
      <button
        type="button"
        aria-label={`View pods on ${node.name}`}
        onClick={onSelect}
        className="grid w-full min-w-0 grid-cols-[minmax(0,1fr)_auto] items-center gap-3 px-4 py-3 text-left hover:bg-ink-800/60 sm:grid-cols-[minmax(0,1.4fr)_minmax(7rem,0.7fr)_minmax(8rem,1fr)_minmax(8rem,1fr)_1rem]"
      >
        <span
          className="truncate text-sm font-medium text-ink-50"
          title={node.name}
        >
          {node.name}
        </span>
        <span
          className={
            ready?.status === "True"
              ? "text-xs text-sev-ok"
              : "text-xs text-sev-warning"
          }
        >
          {status}
        </span>
        <span className="hidden truncate text-xs text-ink-300 sm:block">
          {formatCPU(
            typeof summary.allocatable_cpu === "string"
              ? summary.allocatable_cpu
              : undefined,
          )}
        </span>
        <span className="hidden truncate text-xs text-ink-300 sm:block">
          {formatMemory(
            typeof summary.allocatable_memory === "string"
              ? summary.allocatable_memory
              : undefined,
          )}
        </span>
        <ChevronRight size={14} className="text-ink-500" aria-hidden="true" />
      </button>
    </div>
  );
}

function NodePods({
  node,
  isPending,
  isError,
  truncated,
  partialFailures,
  items,
  pagination,
  next,
  onClose,
}: {
  node: KubernetesResource;
  isPending: boolean;
  isError: boolean;
  truncated: boolean;
  partialFailures?: Array<{ resource_id?: string; class: string }> | null;
  items: KubernetesResource[];
  pagination: CursorPaginationState;
  next?: string;
  onClose: () => void;
}) {
  return (
    <div className="min-w-0">
      <div className="flex min-w-0 items-center justify-between gap-3 border-b border-ink-700 px-4 py-3">
        <div className="min-w-0">
          <p
            className="truncate text-sm font-medium text-ink-100"
            title={node.name}
          >
            {node.name}
          </p>
          <p className="text-2xs text-ink-400">
            Scheduled pods across all namespaces
          </p>
        </div>
        <button type="button" className="btn shrink-0" onClick={onClose}>
          All nodes
        </button>
      </div>
      {isPending && (
        <div className="p-4">
          <SkCard lines={3} />
        </div>
      )}
      {isError && (
        <p role="status" className="p-4 text-sm text-sev-warning">
          Scheduled pods are unavailable.
        </p>
      )}
      {!isPending && !isError && (
        <>
          <div
            role="list"
            aria-label={`Pods on ${node.name}`}
            className="max-h-[min(45vh,32rem)] divide-y divide-ink-700 overflow-y-auto"
          >
            {items.map((pod) => (
              <NodePodRow
                pod={pod}
                key={`${pod.namespace ?? ""}:${pod.name}`}
              />
            ))}
          </div>
          {items.length === 0 && (
            <EmptyState>No pods are scheduled to this node.</EmptyState>
          )}
          <CursorPagination state={pagination} next={next} />
        </>
      )}
      {truncated || partialFailures?.length ? (
        <p
          role="status"
          className="min-w-0 break-words border-t border-ink-700 px-4 py-2 text-xs text-sev-warning"
        >
          Scheduled pod inventory is partial
          {partialFailures?.some((failure) => failure.class === "forbidden")
            ? " because some pods are forbidden"
            : ""}
          .
        </p>
      ) : null}
      <PartialFailuresDisclosure failures={asArray(partialFailures)} />
    </div>
  );
}

function NodePodRow({ pod }: { pod: KubernetesResource }) {
  const summary = pod.summary ?? {};
  const phase =
    typeof summary.phase === "string" ? summary.phase : workloadStatus(pod);
  const restarts =
    typeof summary.restart_count === "number"
      ? summary.restart_count
      : "Unavailable";
  return (
    <div
      role="listitem"
      className="grid min-w-0 grid-cols-[minmax(0,1fr)_auto] gap-3 px-4 py-3 text-xs sm:grid-cols-[minmax(0,1fr)_minmax(0,1.4fr)_7rem_6rem]"
    >
      <span
        className="truncate text-ink-300"
        title={pod.namespace || "cluster scope"}
      >
        {pod.namespace || "cluster scope"}
      </span>
      <span className="truncate font-medium text-ink-100" title={pod.name}>
        {pod.name}
      </span>
      <span className="text-ink-300">{phase}</span>
      <span className="text-right tabular-nums text-ink-300">{restarts}</span>
    </div>
  );
}

type Workload = Awaited<ReturnType<typeof api.kubernetesWorkload>>;
type LiveMetricSample = {
  observedAt: number;
  cpu: number | null;
  memory: number | null;
};
type UsagePodSample = {
  namespace?: string;
  name: string;
  cpu?: string;
  memory?: string;
};
type UsageSnapshot = {
  key: string;
  observedAt: number;
  pods: UsagePodSample[];
};

function workloadMetricSamples(
  snapshots: UsageSnapshot[],
  workload: Workload,
  selected: KubernetesResource | null,
  now: number,
): LiveMetricSample[] {
  const podNames = new Set(asArray(workload.pods).map((pod) => pod.name));
  if (selected?.kind === "Pod") podNames.add(selected.name);
  if (podNames.size === 0) return [];
  return snapshots
    .filter(
      (snapshot) =>
        snapshot.observedAt >= now - usageWindowMilliseconds &&
        snapshot.observedAt <= now,
    )
    .flatMap((snapshot) => {
      let cpu = 0;
      let memory = 0;
      let cpuSamples = 0;
      let memorySamples = 0;
      const matchedPodNames = new Set<string>();
      for (const pod of snapshot.pods) {
        if (
          !podNames.has(pod.name) ||
          (workload.namespace ?? "") !== (pod.namespace ?? "")
        )
          continue;
        matchedPodNames.add(pod.name);
        const podCPU = parseQuantity(pod.cpu);
        const podMemory = parseQuantity(pod.memory);
        if (podCPU !== null) {
          cpu += podCPU;
          cpuSamples++;
        }
        if (podMemory !== null) {
          memory += podMemory;
          memorySamples++;
        }
      }
      if (matchedPodNames.size !== podNames.size) return [];
      const sample = {
        observedAt: snapshot.observedAt,
        cpu: cpuSamples === podNames.size ? cpu : null,
        memory: memorySamples === podNames.size ? memory : null,
      };
      return sample.cpu !== null || sample.memory !== null ? [sample] : [];
    });
}
