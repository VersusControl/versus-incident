import {
  useEffect,
  useMemo,
  useRef,
  useState,
  type KeyboardEvent,
  type PointerEvent,
  type Ref,
  type ReactNode,
} from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "react-router-dom";
import {
  Activity,
  AlertTriangle,
  Boxes,
  ChevronRight,
  Copy,
  Cpu,
  Database,
  Download,
  GitBranch,
  History,
  Layers3,
  LayoutDashboard,
  Network,
  RefreshCw,
  Search,
  Server,
  ShieldCheck,
  ShipWheel,
  Waypoints,
  X,
} from "lucide-react";
import { api, type AgentApproval, type AgentProposalResult, type KubernetesChangesPage, type KubernetesDiagnosis, type KubernetesIndexStatus, type KubernetesOverview, type KubernetesPodLogs, type KubernetesResource, type KubernetesUsage, type KubernetesWorkloadLogs } from "@/lib/api";
import { CursorPagination, Pagination } from "@/components/Pagination";
import { PeekPanel, PeekField } from "@/components/PeekPanel";
import { RetryableError } from "@/components/RetryableError";
import { SkCard } from "@/components/Skeleton";
import { TopBar } from "@/components/TopBar";
import { useCursorPagination, usePagination, type CursorPaginationState } from "@/lib/pagination";
import { projectedResourceYaml } from "@/lib/kubernetesProjection";
import {
  KubernetesOverviewInsights,
  KubernetesExplorerTabContent,
  PartialFailuresDisclosure,
} from "./KubernetesExplorerViews";
import { kubernetesExplorerTabs, type KubernetesExplorerTab } from "./kubernetesExplorerTabs";

type KubernetesDrawerTab = "overview" | "events" | "logs" | "yaml" | "related" | "metrics" | "diagnosis" | "timeline" | "actions";

const drawerTabs: Array<{ id: KubernetesDrawerTab; label: string }> = [
  { id: "overview", label: "Overview" },
  { id: "events", label: "Events" },
  { id: "logs", label: "Logs" },
  { id: "yaml", label: "YAML" },
  { id: "related", label: "Related" },
  { id: "metrics", label: "Metrics" },
  { id: "diagnosis", label: "Diagnosis" },
  { id: "timeline", label: "Timeline" },
  { id: "actions", label: "Actions" },
];

function parseExplorerTab(value: string | null): KubernetesExplorerTab {
  return kubernetesExplorerTabs.some((item) => item.id === value)
    ? (value as KubernetesExplorerTab)
    : "overview";
}

function parseDrawerTab(value: string | null): KubernetesDrawerTab {
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
  releases: Layers3,
  gitops: GitBranch,
  traffic: Waypoints,
};

const stats = [
  ["Nodes", "nodes", Server],
  ["Pods", "pods", Boxes],
  ["Workloads", "workloads", ShipWheel],
  ["Namespaces", "namespaces", Network],
] as const;

const statResourceIDs = {
  nodes: ["core~v1~nodes"],
  pods: ["core~v1~pods"],
  workloads: [
    "apps~v1~deployments",
    "apps~v1~statefulsets",
    "apps~v1~daemonsets",
    "batch~v1~jobs",
    "batch~v1~cronjobs",
  ],
  namespaces: ["core~v1~namespaces"],
} satisfies Record<(typeof stats)[number][1], readonly string[]>;

const workloadKinds = new Set([
  "Deployment",
  "StatefulSet",
  "DaemonSet",
  "Job",
  "CronJob",
  "Pod",
]);
const workloadKindOptions = ["Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob", "Pod"];
const kubernetesPageSize = 20;
const usageWindowMilliseconds = 15 * 60 * 1000;
const usagePollingMilliseconds = 15_000;
const usageGapMilliseconds = usagePollingMilliseconds * 2.5;
const usageSnapshotLimit = 61;
const usagePodSampleLimit = 500;

const quantityUnits: Record<string, number> = {
  "": 1,
  n: 1e-9,
  u: 1e-6,
  µ: 1e-6,
  m: 1e-3,
  k: 1e3,
  K: 1e3,
  M: 1e6,
  G: 1e9,
  T: 1e12,
  P: 1e15,
  E: 1e18,
  Ki: 2 ** 10,
  Mi: 2 ** 20,
  Gi: 2 ** 30,
  Ti: 2 ** 40,
  Pi: 2 ** 50,
  Ei: 2 ** 60,
};

function asArray<T>(value: T[] | null | undefined): T[] {
  return Array.isArray(value) ? value : [];
}

function readable(value: string): string {
  return value
    .replaceAll("_", " ")
    .replace(/([a-z0-9])([A-Z])/g, "$1 $2")
    .toLowerCase();
}

function parseQuantity(value: string | undefined): number | null {
  const input = value?.trim();
  if (!input) return null;

  const rational = input.match(/^([+-]?\d+)\/([+-]?\d+)$/);
  if (rational) {
    const denominator = Number(rational[2]);
    const result = Number(rational[1]) / denominator;
    return denominator !== 0 && Number.isFinite(result) ? result : null;
  }

  const quantity = input.match(
    /^([+-]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][+-]?\d+)?)(n|u|µ|m|k|K|M|G|T|P|E|Ki|Mi|Gi|Ti|Pi|Ei)?$/,
  );
  if (!quantity) return null;
  const result = Number(quantity[1]) * quantityUnits[quantity[2] ?? ""];
  return Number.isFinite(result) ? result : null;
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
  const navigate = useNavigate();
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
    parseDrawerTab(new URLSearchParams(window.location.search).get("tab")),
  );
  const [paletteOpen, setPaletteOpen] = useState(false);
  const [paletteQuery, setPaletteQuery] = useState("");
  const [debouncedPaletteQuery, setDebouncedPaletteQuery] = useState("");
  const [recentResources, setRecentResources] = useState<string[]>(() => {
    try {
      const value: unknown = JSON.parse(localStorage.getItem("versus.k8s.recent") ?? "[]");
      return Array.isArray(value) ? value.filter((item): item is string => typeof item === "string").slice(0, 8) : [];
    } catch {
      return [];
    }
  });
  const [logContainer, setLogContainer] = useState("");
  const [logPrevious, setLogPrevious] = useState(false);
  const [logSinceSeconds, setLogSinceSeconds] = useState("0");
  const [logTailLines, setLogTailLines] = useState("200");
  const [logGrep, setLogGrep] = useState("");
  const [wrapLogs, setWrapLogs] = useState(true);
  const [logActionStatus, setLogActionStatus] = useState("");
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

  const usage = useQuery({
    queryKey: ["kubernetes-usage"],
    queryFn: () => api.kubernetesUsage(),
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
    retry: false,
  });
  const nodePaging = useCursorPagination("nodes");
  const nodes = useQuery({
    queryKey: ["kubernetes-nodes", nodePaging.cursor],
    queryFn: () => api.kubernetesNodes(nodePaging.cursor),
    retry: false,
  });
  const nodePodPaging = useCursorPagination(`node-pods:${selectedNode?.name ?? ""}`);
  const nodePods = useQuery({
    queryKey: ["kubernetes-node-pods", selectedNode?.name, nodePodPaging.cursor],
    queryFn: () => api.kubernetesNodePods(selectedNode!.name, nodePodPaging.cursor),
    enabled: selectedNode !== null,
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
    enabled: selected !== null,
    retry: false,
  });
  const workloadDetailEnabled =
    selected !== null && workloadKinds.has(selected.kind);
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
  const drawerLogs = useQuery<KubernetesPodLogs | KubernetesWorkloadLogs>({
    queryKey: ["kubernetes-drawer-logs", selected?.kind, selected?.namespace, selected?.name, logContainer, logPrevious, logSinceSeconds, logTailLines, logGrep],
    queryFn: () => selected!.kind === "Pod"
      ? api.kubernetesPodLogs(selected!.namespace ?? "", selected!.name, {
          container: logContainer,
          previous: logPrevious,
          since_seconds: Number(logSinceSeconds) || 0,
          tail_lines: Number(logTailLines) || 200,
        })
      : api.kubernetesWorkloadLogs(selected!.kind, selected!.namespace ?? "", selected!.name, {
          container: logContainer,
          previous: logPrevious,
          since_seconds: Number(logSinceSeconds) || 0,
          tail_lines: Number(logTailLines) || 200,
          grep: logGrep,
        }),
    enabled: selected !== null && drawerTab === "logs" && workloadKinds.has(selected.kind),
    retry: false,
  });
  const diagnosis = useQuery({
    queryKey: ["kubernetes-diagnosis", selected?.resource_id, selected?.namespace, selected?.name],
    queryFn: () => api.kubernetesDiagnose(selected!.resource_id, selected!.namespace ?? "", selected!.name),
    enabled: selected !== null && drawerTab === "diagnosis" && workloadKinds.has(selected.kind),
    retry: false,
  });
  const drawerChanges = useQuery({
    queryKey: ["kubernetes-drawer-changes", selected?.kind, selected?.namespace, selected?.name],
    queryFn: () => api.kubernetesChanges({
      since: new Date(Date.now() - 24 * 60 * 60_000).toISOString(),
      until: new Date().toISOString(),
      namespace: selected!.namespace,
      kind: selected!.kind,
      name: selected!.name,
      limit: 100,
    }),
    enabled: selected !== null && drawerTab === "timeline",
    retry: false,
  });
  const paletteResults = useQuery({
    queryKey: ["kubernetes-palette", debouncedPaletteQuery],
    queryFn: () => api.kubernetesSearch("", debouncedPaletteQuery, 20),
    enabled: paletteOpen && debouncedPaletteQuery.trim().length > 0,
    retry: false,
  });

  useEffect(() => {
    const timer = window.setTimeout(() => setDebouncedPaletteQuery(paletteQuery.trim()), 200);
    return () => window.clearTimeout(timer);
  }, [paletteQuery]);

  useEffect(() => {
    const onPopState = () => {
      const query = new URLSearchParams(window.location.search);
      setView(parseExplorerTab(query.get("view")));
      setSelected(parseResourceSelection(window.location.search));
      setDrawerTab(parseDrawerTab(query.get("tab")));
    };
    window.addEventListener("popstate", onPopState);
    return () => window.removeEventListener("popstate", onPopState);
  }, []);

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
        resourceFilterRef.current?.focus();
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
          void queryClient.invalidateQueries({ queryKey });
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
        } catch {
          if (disposed) return;
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
  }, [queryClient]);

  const selectResource = (resource: KubernetesResource, initialTab?: "timeline") => {
    setSelected(resource);
    setDrawerTab(initialTab ?? "overview");
    setLogActionStatus("");
    writeExplorerLocation({ resource, drawerTab: initialTab ?? "overview" });
    const recent = `${resource.kind}:${resource.namespace ? `${resource.namespace}/` : ""}${resource.name}`;
    setRecentResources((current) => {
      const next = [recent, ...current.filter((item) => item !== recent)].slice(0, 8);
      try { localStorage.setItem("versus.k8s.recent", JSON.stringify(next)); } catch { /* storage is optional */ }
      return next;
    });
    setPaletteOpen(false);
  };
  const closeResource = () => {
    setSelected(null);
    setDrawerTab("overview");
    writeExplorerLocation({ resource: null });
  };
  const selectView = (next: KubernetesExplorerTab) => {
    setView(next);
    writeExplorerLocation({ view: next });
  };
  const selectDrawerTab = (next: KubernetesDrawerTab) => {
    setDrawerTab(next);
    if (selected) writeExplorerLocation({ resource: selected, drawerTab: next });
  };
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
    usage.refetch();
    workloads.refetch();
    nodes.refetch();
    if (selectedNode) nodePods.refetch();
    for (const queryKey of ["kubernetes-issues", "kubernetes-top", "kubernetes-overview-topology", "kubernetes-graph", "kubernetes-namespaces", "kubernetes-releases", "kubernetes-traffic", "kubernetes-changes"]) {
      void queryClient.invalidateQueries({ queryKey: [queryKey] });
    }
  };
  const podsUnavailable = overview.data
    ? categoryUnavailable(overview.data, statResourceIDs.pods)
    : false;
  const nodesUnavailable = overview.data
    ? categoryUnavailable(overview.data, statResourceIDs.nodes)
    : false;
  const aggregateUsage = usageAggregate(usage.isError ? undefined : usage.data, overview.data);
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
  const drawerLogText = drawerLogs.data
    ? "text" in drawerLogs.data
      ? drawerLogs.data.text
      : drawerLogs.data.lines.map((line) => `${line.at ? `${line.at} ` : ""}[${line.pod}${line.container ? `/${line.container}` : ""}] ${line.text}`).join("\n")
    : "";
  const visibleDrawerLogText = selected?.kind === "Pod" && logGrep.trim()
    ? drawerLogText.split("\n").filter((line) => line.toLocaleLowerCase().includes(logGrep.toLocaleLowerCase())).join("\n")
    : drawerLogText;
  const logResponseTruncated = drawerLogs.data?.truncated ?? false;
  const previousLogsUnavailable = Boolean(logPrevious && drawerLogs.data && "partial_failures" in drawerLogs.data && drawerLogs.data.partial_failures?.some((failure) => failure.class === "previous_unavailable"));

  const copyDrawerLogs = async () => {
    try {
      await navigator.clipboard.writeText(visibleDrawerLogText);
      setLogActionStatus("Scrubbed logs copied.");
    } catch {
      setLogActionStatus("Clipboard access is unavailable.");
    }
  };
  const downloadDrawerLogs = () => {
    const url = URL.createObjectURL(new Blob([visibleDrawerLogText], { type: "text/plain;charset=utf-8" }));
    const link = document.createElement("a");
    link.href = url;
    link.download = `${selected?.name ?? "kubernetes"}-logs.txt`;
    link.click();
    URL.revokeObjectURL(url);
    setLogActionStatus("Scrubbed log download started.");
  };

  return (
    <main className="relative min-h-0 min-w-0 flex-1 overflow-x-hidden overflow-y-auto">
      <TopBar
        title="Kubernetes"
        actions={
          <div className="flex items-center gap-1">
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
      <div className="mx-auto min-w-0 max-w-7xl space-y-5 overflow-x-hidden p-4 sm:p-6">
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
        <div role="tabpanel" id="kubernetes-view-panel" aria-labelledby={`kubernetes-tab-${view}`} className="space-y-5">
        {view === "overview" && overview.isPending && (
          <div aria-label="Loading Kubernetes overview">
            <SkCard lines={4} />
          </div>
        )}
        {view === "overview" && overview.isError && (
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
                onOpenIssues={() => selectView("issues")}
              />
              <div className="relative grid grid-cols-2 gap-3 p-4 md:grid-cols-3 sm:p-5 xl:grid-cols-4">
              {stats.map(([label, key, Icon]) => {
                const unavailable = categoryUnavailable(
                  overview.data,
                  statResourceIDs[key],
                );
                const ratio =
                  key === "nodes"
                    ? { part: overview.data.ready_nodes, noun: "nodes ready" }
                    : key === "pods"
                      ? { part: overview.data.running_pods, noun: "pods running" }
                      : key === "namespaces"
                        ? { part: overview.data.active_namespaces, noun: "namespaces active" }
                        : null;
                return (
                  <article
                    className="flex min-h-32 min-w-0 flex-col rounded-control border border-ink-500/50 bg-surface-raised/60 p-3.5 transition-colors duration-200 hover:border-ink-400/60"
                    key={key}
                  >
                    <div className="flex items-center justify-between gap-2">
                      <span className="text-2xs font-medium uppercase tracking-wider text-ink-300">
                        {label}
                      </span>
                      <span className="inline-flex size-6 items-center justify-center rounded-control bg-accent/10 text-link">
                        <Icon size={13} aria-hidden="true" />
                      </span>
                    </div>
                    <div className="mt-3 flex min-w-0 flex-1 items-center gap-3">
                      {!unavailable && ratio && (
                        <RatioRing
                          part={ratio.part}
                          total={overview.data[key]}
                          label={`${ratio.part} of ${overview.data[key]} ${ratio.noun}`}
                        />
                      )}
                      <div
                        className={`${unavailable ? "text-lg" : "text-3xl"} min-w-0 font-semibold tabular-nums text-ink-50`}
                      >
                        {unavailable ? "Unavailable" : overview.data[key]}
                      </div>
                    </div>
                    <p className="mt-2 text-2xs text-ink-400">
                      {unavailable
                        ? `${label} count unavailable`
                        : key === "nodes"
                          ? `Nodes ready ${overview.data.ready_nodes}/${overview.data.nodes}`
                          : key === "pods"
                            ? `Pods running ${overview.data.running_pods}/${overview.data.pods}`
                            : key === "namespaces"
                              ? `Namespaces active ${overview.data.active_namespaces}/${overview.data.namespaces}`
                              : key === "workloads"
                                ? "discovered resources"
                                : "active namespaces"}
                    </p>
                  </article>
                );
              })}
              </div>
            </section>

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

        {view === "overview" && <>
        <KubernetesOverviewInsights onSelectResource={selectResource} onSelectView={selectView} />
        <div className="min-w-0 w-full">
          <SectionFrame
            title="Workloads"
            icon={<Boxes size={16} />}
            trailing={
              <span className="text-xs text-ink-400">
                {workloadTotal} total
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
            <KindTabs label="Workload kind" kinds={workloadKindOptions} counts={workloadCounts} selected={workloadKind} onSelect={setWorkloadKind} />
            {workloads.isPending && (
              <div className="p-4">
                <SkCard lines={4} />
              </div>
            )}
            {workloads.isError && (
              <p role="status" className="p-4 text-sm text-sev-warning">
                Resource inventory is unavailable.
              </p>
            )}
            {!workloads.isPending && !workloads.isError && (
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
            {workloads.data?.truncated ||
            workloads.data?.partial_failures?.length ? (
              <p
                role="status"
                className="min-w-0 break-words border-t border-ink-700 px-4 py-2 text-xs text-sev-warning"
              >
                Inventory is bounded or partially available.
              </p>
            ) : null}
            <PartialFailuresDisclosure failures={workloads.data?.partial_failures} />
          </SectionFrame>

        </div>

        <SectionFrame
          title="Nodes"
          icon={<Server size={16} />}
          trailing={
            nodes.data ? (
              <span className="text-xs text-ink-400">
                {nodeItems.length} nodes
              </span>
            ) : undefined
          }
        >
          {selectedNode ? (
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
              {nodes.isPending && (
                <div className="p-4">
                  <SkCard lines={3} />
                </div>
              )}
              {nodes.isError && (
                <p role="status" className="p-4 text-sm text-sev-warning">
                  Nodes are unavailable.
                </p>
              )}
              {nodes.data && (
                <div className="min-w-0 overflow-hidden">
                  <div role="list" className="max-h-[min(45vh,32rem)] divide-y divide-ink-700 overflow-y-auto">
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
              {(nodes.data?.truncated ||
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
              <PartialFailuresDisclosure failures={asArray(nodes.data?.partial_failures)} />
            </>
          )}
        </SectionFrame>
        </>}
        {view !== "overview" && (
          <KubernetesExplorerTabContent tab={view} onSelectResource={selectResource} />
        )}
        </div>
      </div>

      <PeekPanel
        open={selected !== null}
        onClose={closeResource}
        size="wide"
        expandable
        footer={selected && (
          <button type="button" className="btn btn-primary" onClick={() => {
            if (!selected) return;
            const query = new URLSearchParams({ provider: "kubernetes", cluster: overview.data?.cluster_id ?? "", resource_id: selected.resource_id, name: selected.name });
            if (selected.namespace) query.set("namespace", selected.namespace);
            navigate(`/agent/chat?${query}`);
          }}>
            <Activity size={14} aria-hidden="true" /> Investigate
          </button>
        )}
        title={
          selected
            ? `${selected.kind} ${selected.namespace ? `${selected.namespace}/` : ""}${selected.name}`
            : "Resource detail"
        }
      >
        {selected && <div className="space-y-4">
          <div role="tablist" aria-label="Resource detail views" className="flex gap-1 overflow-x-auto border-b border-ink-700" onKeyDown={(event) => {
            if (event.key !== "ArrowRight" && event.key !== "ArrowLeft") return;
            event.preventDefault();
            const current = drawerTabs.findIndex((item) => item.id === drawerTab);
            const offset = event.key === "ArrowRight" ? 1 : -1;
            const next = drawerTabs[(current + offset + drawerTabs.length) % drawerTabs.length];
            selectDrawerTab(next.id);
            window.requestAnimationFrame(() => document.getElementById(`kubernetes-drawer-tab-${next.id}`)?.focus());
          }}>
            {drawerTabs.map((item) => <button type="button" id={`kubernetes-drawer-tab-${item.id}`} role="tab" aria-controls="kubernetes-drawer-panel" aria-selected={drawerTab === item.id} tabIndex={drawerTab === item.id ? 0 : -1} onClick={() => selectDrawerTab(item.id)} className={`shrink-0 border-b-2 px-2.5 py-2 text-xs ${drawerTab === item.id ? "border-accent text-ink-50" : "border-transparent text-ink-300 hover:text-ink-100"}`} key={item.id}>{item.label}</button>)}
          </div>
          <div role="tabpanel" id="kubernetes-drawer-panel" aria-labelledby={`kubernetes-drawer-tab-${drawerTab}`}>
          {detail.isPending && <SkCard lines={5} />}
          {detail.isError && <RetryableError error={detail.error} onRetry={() => detail.refetch()} retrying={detail.isRefetching} context="Couldn't load resource detail" />}
          {detail.data && drawerTab === "overview" && <>
            <ResourceDetail resource={detail.data.resource} related={detail.data.related_resources} eventCount={detail.data.events?.length ?? 0} workload={workloadDetail.data} />
            {workloadDetail.data && <><LiveMetricsSection samples={liveMetrics}/><WorkloadSnapshot workload={workloadDetail.data} metricsStatus={overview.data?.metrics_status}/></>}
          </>}
          {drawerTab === "events" && <DrawerEvents events={detail.data?.events ?? []} loading={detail.isPending} />}
          {drawerTab === "logs" && <section aria-label="Resource logs" className="space-y-3">
            {!workloadKinds.has(selected.kind) ? <p role="status" className="text-sm text-ink-400">Logs are not supported for this resource kind.</p> : <>
              <div className="grid gap-2 sm:grid-cols-2">
                <label className="text-2xs text-ink-400">Container<input aria-label="Log container" className="input mt-1 w-full" value={logContainer} onChange={(event) => setLogContainer(event.target.value)} /></label>
                <label className="text-2xs text-ink-400">Since seconds<input aria-label="Log since seconds" type="number" min="0" max="86400" className="input mt-1 w-full" value={logSinceSeconds} onChange={(event) => setLogSinceSeconds(event.target.value)} /></label>
                <label className="text-2xs text-ink-400">Tail lines<input aria-label="Log tail lines" type="number" min="1" max="1000" className="input mt-1 w-full" value={logTailLines} onChange={(event) => setLogTailLines(event.target.value)} /></label>
                <label className="text-2xs text-ink-400">Literal filter<input aria-label="Log filter" className="input mt-1 w-full" value={logGrep} onChange={(event) => setLogGrep(event.target.value)} placeholder="Filter scrubbed lines" /></label>
              </div>
              <div className="flex flex-wrap items-center justify-between gap-3 border-y border-ink-700 py-2">
                <label className="inline-flex items-center gap-2 text-xs text-ink-200"><input type="checkbox" checked={logPrevious} onChange={(event) => setLogPrevious(event.target.checked)} />Previous container logs</label>
                <label className="inline-flex items-center gap-2 text-xs text-ink-200"><input type="checkbox" checked={wrapLogs} onChange={(event) => setWrapLogs(event.target.checked)} />Wrap lines</label>
                <div className="flex gap-1"><button type="button" className="btn-icon" aria-label="Copy scrubbed logs" title="Copy scrubbed logs" onClick={() => void copyDrawerLogs()} disabled={!drawerLogText}><Copy size={14}/></button><button type="button" className="btn-icon" aria-label="Download scrubbed logs" title="Download scrubbed logs" onClick={downloadDrawerLogs} disabled={!drawerLogText}><Download size={14}/></button></div>
              </div>
              {drawerLogs.isPending && <p role="status" className="text-xs text-ink-400">Loading scrubbed logs.</p>}
              {drawerLogs.isError && <p role="status" className="text-xs text-sev-warning">Logs are unavailable for this resource.</p>}
              {drawerLogs.data && <>
                {previousLogsUnavailable && <p role="status" className="text-xs text-sev-warning">Previous container logs are unavailable because no previous container instance exists.</p>}
                <pre className={`max-h-[55vh] overflow-auto rounded-control border border-ink-700 bg-ink-950 p-3 font-mono text-xs leading-5 text-ink-200 ${wrapLogs ? "whitespace-pre-wrap break-words" : "whitespace-pre"}`}>{visibleDrawerLogText || "No log lines match this filter."}</pre>
                {logResponseTruncated && <p role="status" className="text-xs text-sev-warning">Log output is truncated{drawerLogs.data && "omitted_pods" in drawerLogs.data && drawerLogs.data.omitted_pods?.length ? `; omitted pods: ${drawerLogs.data.omitted_pods.join(", ")}` : ""}.</p>}
                {logActionStatus && <p role="status" className="text-xs text-ink-400">{logActionStatus}</p>}
              </>}
            </>}
          </section>}
          {drawerTab === "yaml" && detail.data && <section aria-label="Projected YAML"><p className="mb-2 text-2xs uppercase text-ink-400">Projected view, read-only</p><pre className="max-h-[65vh] overflow-auto whitespace-pre-wrap break-words rounded-control border border-ink-700 bg-ink-950 p-3 font-mono text-xs leading-5 text-ink-200">{projectedResourceYaml(detail.data.resource)}</pre></section>}
          {drawerTab === "related" && detail.data && <ResourceRelated related={detail.data.related_resources} />}
          {drawerTab === "metrics" && (workloadDetail.data ? <><LiveMetricsSection samples={liveMetrics}/><WorkloadSnapshot workload={workloadDetail.data} metricsStatus={overview.data?.metrics_status}/></> : <p role="status" className="text-sm text-ink-400">Metrics snapshots are not available for this resource kind.</p>)}
          {drawerTab === "diagnosis" && <DiagnosisPanel data={diagnosis.data} pending={diagnosis.isPending} failed={diagnosis.isError} supported={workloadKinds.has(selected.kind)} onSelectResource={selectResource} />}
          {drawerTab === "timeline" && <DrawerTimelinePanel data={drawerChanges.data} pending={drawerChanges.isPending} failed={drawerChanges.isError} />}
          {drawerTab === "actions" && <KubernetesActionsPanel key={`${selected.resource_id}/${selected.namespace ?? ""}/${selected.name}`} resource={selected} cluster={overview.data?.cluster_id} />}
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
  );
}

type KubernetesAction = { type: string; label: string; params: unknown };

function actionsForResource(resource: KubernetesResource): KubernetesAction[] {
  if (["Deployment", "StatefulSet"].includes(resource.kind)) {
    return [
      { type: "k8s.rollout_restart", label: "Restart rollout", params: {} },
      { type: "k8s.scale", label: "Scale replicas", params: {} },
    ];
  }
  if (resource.kind === "DaemonSet") return [{ type: "k8s.rollout_restart", label: "Restart rollout", params: {} }];
  if (resource.kind === "CronJob") return [
    { type: "k8s.cronjob_suspend", label: "Suspend CronJob", params: {} },
    { type: "k8s.cronjob_resume", label: "Resume CronJob", params: {} },
    { type: "k8s.cronjob_trigger", label: "Trigger CronJob", params: {} },
  ];
  if (resource.kind === "Node") return [
    { type: "k8s.node_cordon", label: "Cordon node", params: {} },
    { type: "k8s.node_uncordon", label: "Uncordon node", params: {} },
  ];
  return [];
}

function KubernetesActionsPanel({ resource, cluster }: { resource: KubernetesResource; cluster?: string }) {
  const availability = useQuery({
    queryKey: ["agent-toolsets", "chat"],
    queryFn: () => api.listAgentToolsets("chat"),
    retry: false,
  });
  const [selectedAction, setSelectedAction] = useState("");
  const [replicas, setReplicas] = useState("1");
  const [reason, setReason] = useState("");
  const [rejectionReason, setRejectionReason] = useState("");
  const [result, setResult] = useState<AgentProposalResult | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const actions = actionsForResource(resource);
  const toolset = availability.data?.find((item) => item.id === "kubernetes-actions");
  const available = availability.isSuccess && !availability.isFetching && toolset?.enabled === true && toolset.state === "available";
  const action = actions.find((item) => item.type === selectedAction);
  const replicasValue = Number(replicas);
  const params = action?.type === "k8s.scale" ? { replicas: replicasValue } : action?.params;
  const approval = result?.approval;
  const proposal = result?.proposal ?? approval?.proposal;
  const targetLabel = `${resource.kind} ${resource.namespace ? `${resource.namespace}/` : ""}${resource.name}`;

  const propose = async () => {
    if (!action || !reason.trim() || busy) return;
    if (action.type === "k8s.scale" && (!Number.isSafeInteger(replicasValue) || replicasValue < 1)) return;
    setBusy(true);
    setError("");
    setResult(null);
    try {
      setResult(await api.proposeAgentAction({
        type: action.type,
        target: { cluster, namespace: resource.namespace, kind: resource.kind, name: resource.name },
        params,
        reason: reason.trim(),
      }));
    } catch (failure) {
      setError(failure instanceof Error ? failure.message : "The proposal could not be submitted.");
    } finally {
      setBusy(false);
    }
  };

  const decide = async (decision: "approve" | "reject") => {
    if (!approval || busy || (decision === "approve" && !result?.nonce)) return;
    setBusy(true);
    setError("");
    try {
      const updated = decision === "approve"
        ? await api.approveAgentApproval(approval.id, result.nonce!)
        : await api.rejectAgentApproval(approval.id, rejectionReason.trim());
      setResult((current) => current ? { ...current, approval: updated, nonce: undefined } : current);
    } catch (failure) {
      setError(failure instanceof Error ? failure.message : `The ${decision} request failed.`);
    } finally {
      setBusy(false);
    }
  };

  return <section aria-label="Kubernetes actions" className="space-y-4">
    {(availability.isPending || availability.isFetching) && <p role="status" className="text-sm text-ink-400">Checking action availability.</p>}
    {availability.isError && <p role="status" className="text-sm text-ink-400">Action availability could not be confirmed. Actions are unavailable.</p>}
    {availability.isSuccess && !availability.isFetching && !available && <p role="status" className="text-sm text-ink-400">{toolset?.reason || "Kubernetes actions are unavailable because the server has not advertised an available actor."}</p>}
    {availability.isSuccess && available && actions.length === 0 && <p role="status" className="text-sm text-ink-400">No allow-listed actions apply to this resource kind.</p>}
    {available && actions.length > 0 && <>
      <p className="text-xs text-ink-400">Submitting creates a server-side dry-run proposal only. Approval sends a nonce-backed decision to the server-side actor, which executes and verifies the change.</p>
      <div className="flex flex-wrap gap-2" aria-label="Available Kubernetes actions">
        {actions.map((item) => <button type="button" key={item.type} aria-pressed={selectedAction === item.type} className={`btn ${selectedAction === item.type ? "btn-primary" : ""}`} onClick={() => { setSelectedAction(item.type); setResult(null); setError(""); }}><ShieldCheck size={14} aria-hidden="true" />{item.label}</button>)}
      </div>
      {action && <div className="max-w-xl space-y-3 border-y border-ink-700 py-3">
        <h3 className="text-sm font-medium text-ink-100">Propose {action.label.toLowerCase()}</h3>
        <p className="text-xs text-ink-300">Target: {targetLabel}{cluster ? `, cluster ${cluster}` : ""}</p>
        {action.type === "k8s.scale" && <label className="block text-xs text-ink-300">Replicas<input aria-label="Desired replicas" className="input mt-1 w-full" type="number" min="1" step="1" value={replicas} onChange={(event) => setReplicas(event.target.value)} /></label>}
        <label className="block text-xs text-ink-300">Reason<input aria-label="Action reason" className="input mt-1 w-full" value={reason} maxLength={512} onChange={(event) => setReason(event.target.value)} /></label>
        <button type="button" className="btn btn-primary" onClick={() => void propose()} disabled={busy || !reason.trim() || (action.type === "k8s.scale" && (!Number.isSafeInteger(replicasValue) || replicasValue < 1))}>{busy ? "Submitting…" : "Create dry-run proposal"}</button>
      </div>}
    </>}
    {result?.guide && <section aria-label="Action guide" className="space-y-2 border border-ink-700 p-3"><h3 className="text-sm font-medium text-ink-100">Guide only: {result.guide.risk} risk</h3><p className="text-xs text-ink-300">{result.guide.action}</p><pre className="overflow-auto whitespace-pre-wrap break-words rounded-control bg-ink-950 p-3 font-mono text-xs text-ink-200">{result.guide.command}</pre></section>}
    {proposal && <ProposalDetails proposal={proposal} approval={approval} nonce={result?.nonce} busy={busy} rejectionReason={rejectionReason} onRejectionReasonChange={setRejectionReason} onApprove={() => void decide("approve")} onReject={() => void decide("reject")} />}
    {error && <p role="alert" className="text-xs text-sev-critical">{error}</p>}
  </section>;
}

function ProposalDetails({ proposal, approval, nonce, busy, rejectionReason, onRejectionReasonChange, onApprove, onReject }: {
  proposal: NonNullable<AgentProposalResult["proposal"]>;
  approval?: AgentApproval;
  nonce?: string;
  busy: boolean;
  rejectionReason: string;
  onRejectionReasonChange: (value: string) => void;
  onApprove: () => void;
  onReject: () => void;
}) {
  const expiry = Date.parse(approval?.expires_at ?? proposal.expires_at);
  const pending = approval?.state === "pending";
  return <section aria-label="Action proposal details" className="space-y-3 border border-ink-700 p-3">
    <h3 className="text-sm font-semibold text-ink-100">Dry-run proposal: {approval?.state ?? "created"}</h3>
    <dl className="grid gap-x-4 gap-y-2 text-xs sm:grid-cols-[minmax(100px,140px)_1fr]">
      <dt className="text-ink-400">Action</dt><dd className="break-words text-ink-100">{proposal.type}</dd>
      <dt className="text-ink-400">Target</dt><dd className="break-words text-ink-100">{proposal.target.kind} {proposal.target.namespace ? `${proposal.target.namespace}/` : ""}{proposal.target.name}</dd>
      <dt className="text-ink-400">Risk</dt><dd className="text-ink-100">{proposal.risk}</dd>
      <dt className="text-ink-400">Reason</dt><dd className="break-words text-ink-100">{proposal.reason}</dd>
      <dt className="text-ink-400">Dry run</dt><dd className="whitespace-pre-wrap break-words text-ink-100">{proposal.dry_run || "No dry-run details returned."}</dd>
      {Number.isFinite(expiry) && <><dt className="text-ink-400">Expires</dt><dd><time dateTime={approval?.expires_at ?? proposal.expires_at} className="text-ink-200">{new Date(expiry).toLocaleString()}</time></dd></>}
    </dl>
    <details><summary className="cursor-pointer text-xs text-ink-300">Proposal parameters</summary><pre className="mt-2 max-h-40 overflow-auto whitespace-pre-wrap break-words rounded-control bg-ink-950 p-2 font-mono text-xs text-ink-200">{JSON.stringify(proposal.params, null, 2)}</pre></details>
    {approval?.result?.summary && <p className="text-xs text-ink-200">Execution: {approval.result.summary}</p>}
    {approval?.verification && <p role="status" className="text-xs text-ink-200">Verification {approval.verification.verified ? "passed" : "failed"}: {approval.verification.summary}</p>}
    {pending && <div className="space-y-2 border-t border-ink-700 pt-3">
      <label className="block text-xs text-ink-300">Rejection reason<input aria-label="Proposal rejection reason" className="input mt-1 w-full" value={rejectionReason} maxLength={512} onChange={(event) => onRejectionReasonChange(event.target.value)} /></label>
      <div className="flex flex-wrap gap-2"><button type="button" className="btn btn-primary" onClick={onApprove} disabled={busy || !nonce}>{busy ? "Submitting…" : "Approve and execute"}</button><button type="button" className="btn" onClick={onReject} disabled={busy}>{busy ? "Submitting…" : "Reject proposal"}</button></div>
      {!nonce && <p role="status" className="text-xs text-ink-400">Approval is unavailable because this proposal has no decision nonce.</p>}
    </div>}
  </section>;
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

  function DrawerEvents({ events, loading }: { events: KubernetesResource[]; loading: boolean }) {
    if (loading) return <p role="status" className="text-xs text-ink-400">Loading object-scoped events.</p>;
    if (events.length === 0) return <p role="status" className="text-xs text-ink-400">No object-scoped events were returned.</p>;
    return <section aria-label="Object-scoped events" className="divide-y divide-ink-700">{events.map((event) => <article className="py-3" key={`${event.uid ?? event.name}:${event.name}`}><h3 className="text-xs font-medium text-ink-100">{String(event.summary?.reason ?? event.name)}</h3><p className="mt-1 break-words text-xs leading-5 text-ink-300">{String(event.summary?.message ?? "Event message unavailable")}</p><p className="mt-1 text-2xs text-ink-400">{event.summary?.count ? `Count ${String(event.summary.count)}; ` : ""}{String(event.summary?.lastTimestamp ?? "Time unavailable")}</p></article>)}</section>;
  }

  function ResourceRelated({ related }: { related?: Array<{ resource_id?: string; kind: string; namespace?: string; name: string }> | null }) {
    const items = asArray(related);
    return <section aria-label="Related resources"><h3 className="text-xs font-semibold text-ink-100">Projected relationships</h3>{items.length === 0 ? <p role="status" className="mt-3 text-xs text-ink-400">No related resource references were projected.</p> : <div className="mt-3 divide-y divide-ink-700">{items.map((item) => <div className="flex min-w-0 items-center justify-between gap-3 py-3" key={`${item.resource_id}:${item.namespace}:${item.kind}:${item.name}`}><span className="min-w-0"><span className="block truncate text-xs text-ink-100">{item.name}</span><span className="block truncate text-2xs text-ink-400">{item.namespace || "Cluster scope"}</span></span><span className="pill">{item.kind}</span></div>)}</div>}</section>;
  }

  function DiagnosisPanel({ data, pending, failed, supported, onSelectResource }: { data?: KubernetesDiagnosis; pending: boolean; failed: boolean; supported: boolean; onSelectResource: (resource: KubernetesResource, drawerTab?: "timeline") => void }) {
    if (!supported) return <DrawerUnavailable title="Diagnosis" contract="GET /api/admin/kubernetes/diagnose/:resourceId/:name (workload kinds only)" />;
    if (pending) return <p role="status" className="text-xs text-ink-400">Loading diagnosis evidence.</p>;
    if (failed) return <p role="status" className="text-xs text-sev-warning">Diagnosis evidence is unavailable.</p>;
    if (!data) return <p role="status" className="text-xs text-ink-400">No diagnosis evidence was returned.</p>;
    return <section aria-label="Diagnosis evidence" className="space-y-4">
      <p className="text-xs text-ink-300">Evidence only: {data.sync.state}{data.sync.partial ? ", partial" : ""}</p>
      <dl className="grid grid-cols-2 gap-3"><PeekField label="Workload">{data.workload.kind} {data.workload.name}</PeekField><PeekField label="Ready">{data.workload.ready ?? "Unavailable"} / {data.workload.desired ?? "Unavailable"}</PeekField><PeekField label="Generation">{data.workload.generation ?? "Unavailable"}</PeekField><PeekField label="Observed generation">{data.workload.observed_generation ?? "Unavailable"}</PeekField></dl>
      <section aria-label="Diagnosis change citations"><div className="flex items-center justify-between gap-3"><h3 className="text-xs font-semibold text-ink-100">Recent changes (1h)</h3><button type="button" className="btn" onClick={() => onSelectResource({ resource_id: data.workload.resource_id, kind: data.workload.kind, namespace: data.workload.namespace, name: data.workload.name }, "timeline")}>Open timeline</button></div>{data.changes.length === 0 ? <p className="mt-2 text-xs text-ink-400">No projected changes were recorded for this workload.</p> : <ol className="mt-2 divide-y divide-ink-700">{data.changes.map((change) => <li key={change.id}><button type="button" className="flex w-full items-center justify-between gap-3 py-2 text-left hover:bg-ink-800/50" onClick={() => onSelectResource({ resource_id: data.workload.resource_id, kind: data.workload.kind, namespace: data.workload.namespace, name: data.workload.name }, "timeline")}><span className="min-w-0"><span className="block truncate text-xs text-ink-100">{change.type.replaceAll("_", " ")}</span><span className="block truncate text-2xs text-ink-400">{change.fields?.map((field) => field.path).join(", ") || "Object changed"}</span></span><time className="shrink-0 text-2xs text-ink-400">{new Date(change.at).toLocaleString()}</time></button></li>)}</ol>}</section>
      <section aria-label="Diagnosis neighborhood citations"><h3 className="text-xs font-semibold text-ink-100">Related resources</h3>{data.neighborhood.nodes.filter((node) => node.kind !== data.workload.kind || node.namespace !== data.workload.namespace || node.name !== data.workload.name).length === 0 ? <p className="mt-2 text-xs text-ink-400">No adjacent resources were projected.</p> : <ul className="mt-2 divide-y divide-ink-700">{data.neighborhood.nodes.filter((node) => node.kind !== data.workload.kind || node.namespace !== data.workload.namespace || node.name !== data.workload.name).map((node) => {
        const resource_id = ({ Pod: "core~v1~pods", Node: "core~v1~nodes", Namespace: "core~v1~namespaces", Service: "core~v1~services", ConfigMap: "core~v1~configmaps", Secret: "core~v1~secrets", PersistentVolumeClaim: "core~v1~persistentvolumeclaims", Deployment: "apps~v1~deployments", ReplicaSet: "apps~v1~replicasets", StatefulSet: "apps~v1~statefulsets", DaemonSet: "apps~v1~daemonsets", Job: "batch~v1~jobs", CronJob: "batch~v1~cronjobs", Ingress: "networking.k8s.io~v1~ingresses", HorizontalPodAutoscaler: "autoscaling~v2~horizontalpodautoscalers", HTTPRoute: "gateway.networking.k8s.io~v1~httproutes", Gateway: "gateway.networking.k8s.io~v1~gateways" } as Record<string, string>)[node.kind];
        return <li key={node.id}><button type="button" disabled={!resource_id} className="flex w-full items-center justify-between gap-3 py-2 text-left hover:bg-ink-800/50 disabled:cursor-default" onClick={() => resource_id && onSelectResource({ resource_id, kind: node.kind, namespace: node.namespace, name: node.name })}><span className="min-w-0"><span className="block truncate text-xs text-ink-100">{node.namespace ? `${node.namespace}/` : ""}{node.name}</span><span className="block text-2xs text-ink-400">{node.kind}</span></span><span className="text-2xs text-link">Open resource</span></button></li>;
      })}</ul>}{Object.values(data.neighborhood.omitted).some((count) => count > 0) && <p role="status" className="mt-2 text-2xs text-sev-warning">The neighborhood response omitted projected resources.</p>}</section>
      {data.warning_events?.length ? <div><h3 className="text-xs font-semibold text-ink-100">Warning events</h3><ul className="mt-2 divide-y divide-ink-700">{data.warning_events.map((event) => <li className="py-2 text-xs" key={`${event.uid}:${event.name}`}><span className="font-medium text-ink-100">{String(event.summary?.reason ?? event.name)}</span><span className="ml-2 text-ink-400">{String(event.summary?.count ?? 1)} occurrences</span></li>)}</ul></div> : <p className="text-xs text-ink-400">No warning events were returned.</p>}
      {data.worst_pod_logs && <div><h3 className="text-xs font-semibold text-ink-100">Scrubbed pod log evidence for {data.worst_pod_logs.pod}</h3><pre className="mt-2 max-h-64 overflow-auto whitespace-pre-wrap break-words border border-ink-700 bg-ink-950 p-3 font-mono text-2xs leading-5 text-ink-200">{data.worst_pod_logs.text}</pre></div>}
      {(data.omitted_categories?.length || data.partial_failures?.length || data.truncated) ? <p role="status" className="text-xs text-sev-warning">Partial evidence: {[...asArray(data.omitted_categories), ...asArray(data.partial_failures).map((item) => item.resource_id ? `${item.resource_id} (${item.class})` : item.class)].join(", ") || "response truncated"}</p> : null}
    </section>;
  }

  function DrawerUnavailable({ title, contract }: { title: string; contract: string }) {
    return <section aria-label={`${title} unavailable`} className="space-y-2"><h3 className="text-sm font-semibold text-ink-100">{title}</h3><p role="status" className="text-xs text-ink-400">This view is unavailable until its backend contract is registered.</p><p className="break-words font-mono text-2xs text-ink-500">{contract}</p></section>;
  }

  function DrawerTimelinePanel({ data, pending, failed }: { data?: KubernetesChangesPage; pending: boolean; failed: boolean }) {
    if (pending) return <p role="status" className="text-xs text-ink-400">Loading resource change history.</p>;
    if (failed) return <p role="status" className="text-xs text-sev-warning">Resource change history is unavailable.</p>;
    if (!data || data.items.length === 0) return <p role="status" className="text-xs text-ink-400">No projected changes were recorded for this resource.</p>;
    return <section aria-label="Resource change history" className="space-y-3"><ol className="divide-y divide-ink-700">{data.items.map((change) => <li className="py-3" key={change.id}><div className="flex flex-wrap items-center justify-between gap-2"><span className="pill pill-accent">{change.type.replaceAll("_", " ")}</span><time className="text-2xs text-ink-400">{new Date(change.at).toLocaleString()}</time></div>{change.fields?.map((field) => <p className="mt-2 break-words font-mono text-2xs text-ink-300" key={`${change.id}:${field.path}`}>{field.path}: {field.from || "∅"} → {field.to || "∅"}</p>)}</li>)}</ol>{data.gaps?.length ? <p role="status" className="text-xs text-sev-warning">History has {data.gaps.length} recorded gap(s).</p> : null}</section>;
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

function RatioRing({ part, total, label }: { part: number; total: number; label: string }) {
  const radius = 18;
  const circumference = 2 * Math.PI * radius;
  const ratio = total > 0 ? Math.min(1, Math.max(0, part / total)) : 0;
  const tone =
    total === 0
      ? "text-ink-500"
      : ratio >= 1
        ? "text-sev-ok"
        : ratio >= 0.8
          ? "text-sev-warning"
          : "text-sev-critical";
  return (
    <div className="relative size-12 shrink-0">
      <svg viewBox="0 0 44 44" className="size-12 -rotate-90" role="img" aria-label={label}>
        <circle cx="22" cy="22" r={radius} fill="none" stroke="currentColor" strokeWidth="4" className="text-ink-600" />
        {ratio > 0 && (
          <circle
            cx="22"
            cy="22"
            r={radius}
            fill="none"
            stroke="currentColor"
            strokeWidth="4"
            strokeLinecap="round"
            strokeDasharray={circumference}
            strokeDashoffset={circumference * (1 - ratio)}
            className={`${tone} transition-[stroke-dashoffset] duration-700 ease-out`}
          />
        )}
      </svg>
      <span aria-hidden="true" className="absolute inset-0 grid place-items-center text-2xs font-semibold tabular-nums text-ink-100">
        {total > 0 ? `${Math.round(ratio * 100)}%` : "–"}
      </span>
    </div>
  );
}

function ClusterHealthHeader({
  overview,
  onOpenIssues,
}: {
  overview: {
    observed_at: string;
    nodes: number;
    ready_nodes: number;
    warnings: number;
    truncated: boolean;
    omitted_categories?: string[] | null;
    partial_failures?: Array<{ resource_id?: string }> | null;
  };
  onOpenIssues: () => void;
}) {
  const partial = overview.truncated || Boolean(overview.omitted_categories?.length) || Boolean(overview.partial_failures?.length);
  const attention = overview.ready_nodes < overview.nodes || overview.warnings > 0;
  const status = partial
    ? { label: "Partial visibility", tone: "pill-warn", dot: "bg-sev-warn-solid" }
    : attention
      ? { label: "Needs attention", tone: "pill-warn", dot: "bg-sev-warn-solid" }
      : overview.nodes > 0
        ? { label: "Node checks passing", tone: "pill-good", dot: "bg-sev-ok-solid" }
        : { label: "No node evidence", tone: "", dot: "bg-ink-400" };
  const observed = Date.parse(overview.observed_at);
  return (
    <header className="relative flex flex-wrap items-center justify-between gap-3 px-4 pt-4 sm:px-5 sm:pt-5">
      <div className="flex min-w-0 items-center gap-3">
        <span className="inline-flex size-9 shrink-0 items-center justify-center rounded-card bg-accent/15 text-link ring-1 ring-inset ring-accent/30">
          <ShipWheel size={18} aria-hidden="true" />
        </span>
        <div className="min-w-0">
          <h2 className="flex flex-wrap items-center gap-2 text-sm font-semibold text-ink-50">
            Cluster health
            <span className={`pill ${status.tone}`}>
              <span aria-hidden="true" className={`size-1.5 rounded-full ${status.dot}`} />
              {status.label}
            </span>
          </h2>
          <p className="mt-0.5 text-2xs text-ink-400">
            {Number.isFinite(observed)
              ? `Snapshot ${new Date(observed).toLocaleTimeString()}`
              : "Snapshot time not reported"}
          </p>
        </div>
      </div>
      <button type="button" className="btn" onClick={onOpenIssues}>
        <AlertTriangle size={13} aria-hidden="true" className="text-sev-warning" />
        Review issues
        <ChevronRight size={13} aria-hidden="true" />
      </button>
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
type WorkloadUsage = NonNullable<Workload["usage"]>[number];
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
      let hasCPU = false;
      let hasMemory = false;
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
          hasCPU = true;
        }
        if (podMemory !== null) {
          memory += podMemory;
          hasMemory = true;
        }
      }
      if (matchedPodNames.size !== podNames.size) return [];
      const sample = {
        observedAt: snapshot.observedAt,
        cpu: hasCPU ? cpu : null,
        memory: hasMemory ? memory : null,
      };
      return sample.cpu !== null || sample.memory !== null ? [sample] : [];
    });
}

function metricPath(
  samples: LiveMetricSample[],
  field: "cpu" | "memory",
  windowStart: number,
  now: number,
  maximum: number,
): string {
  const values = samples.map((sample) => sample[field]);
  let previousIndex: number | null = null;
  return values
    .map((value, index) => {
      if (value === null) {
        previousIndex = null;
        return "";
      }
      const x =
        40 +
        Math.max(
          0,
          Math.min(
            1,
            (samples[index].observedAt - windowStart) /
              Math.max(1, now - windowStart),
          ),
        ) *
          490;
      const y = 120 - (value / maximum) * 95;
      const contiguous =
        previousIndex !== null &&
        samples[index].observedAt - samples[previousIndex].observedAt <=
          usageGapMilliseconds;
      const command = contiguous ? "L" : "M";
      previousIndex = index;
      return `${command} ${x.toFixed(1)} ${y.toFixed(1)}`;
    })
    .filter(Boolean)
    .join(" ");
}

function hasRenderableMetricPath(
  samples: LiveMetricSample[],
  field: "cpu" | "memory",
): boolean {
  return samples.some(
    (sample, index) =>
      index > 0 &&
      sample[field] !== null &&
      samples[index - 1][field] !== null &&
      sample.observedAt - samples[index - 1].observedAt <=
        usageGapMilliseconds,
  );
}

function LiveMetricsChart({
  samples,
  field,
}: {
  samples: LiveMetricSample[];
  field: "cpu" | "memory";
}) {
  const validSamples = samples.filter((sample) => sample[field] !== null);
  const [activeObservedAt, setActiveObservedAt] = useState<number | null>(null);
  const now = samples.at(-1)!.observedAt;
  const windowStart = Math.max(
    now - usageWindowMilliseconds,
    samples[0].observedAt,
  );
  const maximum = Math.max(...validSamples.map((sample) => sample[field] ?? 0));
  const scaleMaximum = Math.max(1, maximum);
  const first = new Date(windowStart).toLocaleTimeString();
  const last = new Date(now).toLocaleTimeString();
  const span = now - windowStart;
  const spanMinutes = Math.max(0, span / 60_000);
  const title = field === "cpu" ? "CPU" : "Memory";
  const formatter = field === "cpu" ? formatCPU : formatMemory;
  const label =
    span >= usageWindowMilliseconds
      ? `Workload ${title} collected over the rolling 15-minute window`
      : `Workload ${title} collected since ${first}`;
  const activeIndex =
    activeObservedAt === null
      ? -1
      : validSamples.findIndex(
          (sample) => sample.observedAt === activeObservedAt,
        );
  const active = activeIndex < 0 ? null : validSamples[activeIndex];
  const activeX =
    active === null ? 40 : 40 +
    Math.max(
      0,
      Math.min(
        1,
        (active.observedAt - windowStart) / Math.max(1, now - windowStart),
      ),
    ) *
      490;
  const activeValue = active?.[field] ?? null;
  const activeY = activeValue === null ? 120 : 120 - (activeValue / scaleMaximum) * 95;
  const setNearest = (clientX: number, currentTarget: SVGSVGElement) => {
    const bounds = currentTarget.getBoundingClientRect();
    const viewBoxX = (clientX - bounds.left) / Math.max(1, bounds.width) * 560;
    const ratio = Math.max(0, Math.min(1, (viewBoxX - 40) / 490));
    const targetTime = windowStart + ratio * (now - windowStart);
    let nearest = 0;
    for (let index = 1; index < validSamples.length; index++) {
      if (Math.abs(validSamples[index].observedAt - targetTime) < Math.abs(validSamples[nearest].observedAt - targetTime)) nearest = index;
    }
    setActiveObservedAt(validSamples[nearest].observedAt);
  };
  const moveSelection = (event: KeyboardEvent<HTMLDivElement>) => {
    if (event.key !== "ArrowLeft" && event.key !== "ArrowRight" && event.key !== "Home" && event.key !== "End") return;
    event.preventDefault();
    if (event.key === "Home") setActiveObservedAt(validSamples[0].observedAt);
    else if (event.key === "End")
      setActiveObservedAt(validSamples.at(-1)!.observedAt);
    else {
      const currentIndex = Math.max(0, activeIndex);
      const nextIndex = Math.max(
        0,
        Math.min(
          validSamples.length - 1,
          currentIndex + (event.key === "ArrowRight" ? 1 : -1),
        ),
      );
      setActiveObservedAt(validSamples[nextIndex].observedAt);
    }
  };
  const activeDescription = active && activeValue !== null
    ? `${new Date(active.observedAt).toLocaleTimeString()}: ${formatter(String(activeValue))}`
    : "Use arrow keys or hover over the chart to inspect samples.";
  return (
    <section className="min-w-0" aria-label={`${title} usage chart`}>
      <h4 className="text-2xs font-medium uppercase text-ink-400">{title}</h4>
      <div className="mt-2 min-w-0 overflow-x-auto" tabIndex={0} onFocus={() => setActiveObservedAt((current) => current ?? validSamples.at(-1)!.observedAt)} onBlur={() => setActiveObservedAt(null)} onKeyDown={moveSelection} aria-label={`${label}. ${validSamples.length} samples over ${formatAmount(spanMinutes, 1)} minutes.`} aria-describedby={`live-${field}-metric-value`}>
      <span id={`live-${field}-metric-value`} className="sr-only" aria-live="polite">{activeDescription}</span>
      <svg
        role="img"
        aria-label={label}
        viewBox="0 0 560 155"
        className="h-auto min-w-[28rem] text-ink-400"
        onPointerMove={(event: PointerEvent<SVGSVGElement>) => setNearest(event.clientX, event.currentTarget)}
        onPointerLeave={() => setActiveObservedAt(null)}
      >
        <title>{label}</title>
        <g stroke="currentColor" strokeOpacity="0.25">
          <line x1="40" y1="25" x2="530" y2="25" />
          <line x1="40" y1="72.5" x2="530" y2="72.5" />
          <line x1="40" y1="120" x2="530" y2="120" />
        </g>
        <g fill="currentColor" fontSize="10">
          <text x="40" y="145">
            {first}
          </text>
          <text x="530" y="145" textAnchor="end">
            Now: {last}
          </text>
          <text x="526" y="19" textAnchor="end">
            {formatter(String(maximum))}
          </text>
        </g>
        <path
          data-series={field}
          d={metricPath(samples, field, windowStart, now, scaleMaximum)}
          fill="none"
          stroke={field === "cpu" ? "rgb(56 189 248)" : "rgb(74 222 128)"}
          strokeWidth="2"
        />
        {validSamples.map((sample) => {
          const x =
            40 +
            Math.max(
              0,
              Math.min(
                1,
                (sample.observedAt - windowStart) /
                  Math.max(1, now - windowStart),
              ),
            ) *
              490;
          return (
            <g key={sample.observedAt}>
              <circle cx={x} cy={120 - (sample[field]! / scaleMaximum) * 95} r="2.5" fill={field === "cpu" ? "rgb(56 189 248)" : "rgb(74 222 128)"} />
            </g>
          );
        })}
        {active && activeValue !== null && <><line data-testid={`${field}-crosshair`} x1={activeX} y1="25" x2={activeX} y2="120" stroke="currentColor" strokeDasharray="3 3" />
        <circle data-testid={`${field}-active-point`} cx={activeX} cy={activeY} r="5" fill={field === "cpu" ? "rgb(56 189 248)" : "rgb(74 222 128)"} stroke="rgb(15 23 42)" strokeWidth="2" />
        <g data-testid={`${field}-tooltip`}>
          <rect x={Math.max(42, Math.min(390, activeX - 65))} y="2" width="138" height="20" rx="3" fill="rgb(15 23 42)" />
          <text x={Math.max(48, Math.min(396, activeX - 59))} y="16" fill="rgb(226 232 240)" fontSize="10">{activeDescription}</text>
        </g></>}
      </svg>
      </div>
    </section>
  );
}

function LiveMetricsSection({ samples }: { samples: LiveMetricSample[] }) {
  return (
    <section
      className="mt-5 border-t border-ink-700 pt-4"
      aria-label="Live workload metrics"
    >
      <h3 className="text-xs font-semibold text-ink-100">
        Live workload usage
      </h3>
      <div className="mt-3 grid min-w-0 gap-5">
        {(["cpu", "memory"] as const).map((field) => hasRenderableMetricPath(samples, field) ? <LiveMetricsChart samples={samples} field={field} key={field} /> : <section aria-label={`${field === "cpu" ? "CPU" : "Memory"} usage chart`} key={field}><h4 className="text-2xs font-medium uppercase text-ink-400">{field === "cpu" ? "CPU" : "Memory"}</h4><p role="status" className="mt-2 text-xs text-ink-400">Collecting 15-minute history.</p></section>)}
      </div>
    </section>
  );
}

function metricMaximum(rows: WorkloadUsage[], field: "cpu" | "memory"): number {
  return rows.reduce(
    (maximum, row) => Math.max(maximum, parseQuantity(row[field]) ?? 0),
    0,
  );
}

function configuredLimit(
  workload: Workload,
  field: "cpu" | "memory",
): number | null {
  const containers = asArray(workload.containers);
  if (containers.length === 0) return null;
  const limits = containers.map((container) =>
    parseQuantity(container.limits?.[field]),
  );
  if (limits.some((limit) => limit === null)) return null;
  const total = limits.reduce<number>((sum, limit) => sum + (limit ?? 0), 0);
  return total > 0 ? total : null;
}

function MetricBar({
  row,
  field,
  maximum,
}: {
  row: WorkloadUsage;
  field: "cpu" | "memory";
  maximum: number;
}) {
  const amount = parseQuantity(row[field]);
  const formatted =
    field === "cpu" ? formatCPU(row.cpu) : formatMemory(row.memory);
  const width =
    amount !== null && maximum > 0
      ? Math.max(0, Math.min(100, (amount / maximum) * 100))
      : 0;
  return (
    <div className="grid grid-cols-[minmax(5rem,0.8fr)_minmax(6rem,1.5fr)_auto] items-center gap-2 py-1.5">
      <span className="truncate text-xs text-ink-200" title={row.name}>
        {row.name}
      </span>
      <div className="h-2 overflow-hidden rounded-control bg-ink-700">
        {amount !== null && (
          <div
            role="progressbar"
            aria-label={`${row.name} ${field} usage`}
            aria-valuemin={0}
            aria-valuemax={maximum}
            aria-valuenow={Math.max(0, Math.min(amount, maximum))}
            aria-valuetext={formatted}
            className="h-full rounded-control bg-accent"
            style={{ width: `${width}%` }}
          />
        )}
      </div>
      <span className="min-w-16 text-right text-2xs tabular-nums text-ink-300">
        {formatted}
      </span>
    </div>
  );
}

function WorkloadSnapshot({
  workload,
  metricsStatus,
}: {
  workload: Workload;
  metricsStatus?: "available" | "stale" | "unavailable" | null;
}) {
  const usage = asArray(workload.usage);
  const pods = asArray(workload.pods);
  const resetKey = `${workload.kind}:${workload.namespace ?? ""}:${workload.name}`;
  const usagePagination = usePagination(usage, {
    pageSize: kubernetesPageSize,
    resetKey,
  });
  const podPagination = usePagination(pods, {
    pageSize: kubernetesPageSize,
    resetKey,
  });
  const cpuLimit = configuredLimit(workload, "cpu");
  const memoryLimit = configuredLimit(workload, "memory");
  const cpuMaximum = cpuLimit ?? metricMaximum(usage, "cpu");
  const memoryMaximum = memoryLimit ?? metricMaximum(usage, "memory");
  const normalization = `CPU normalized ${cpuLimit === null ? "within this workload" : "against configured per-pod limits"}; memory normalized ${memoryLimit === null ? "within this workload" : "against configured per-pod limits"}.`;

  return (
    <div className="mt-5 space-y-5">
      <section
        className="border-t border-ink-700 pt-4"
        aria-label={
          usage.length > 0
            ? `Current workload metrics snapshot. ${normalization}`
            : "Current workload metrics snapshot"
        }
      >
        <h3 className="text-xs font-semibold text-ink-100">
          Current metrics snapshot
        </h3>
        {usage.length > 0 ? (
          <div className="mt-3 grid gap-4 lg:grid-cols-2">
            <div>
              <h4 className="text-2xs font-medium uppercase text-ink-400">
                CPU
              </h4>
              <div className="mt-1 divide-y divide-ink-700">
                {usagePagination.pageItems.map((row) => (
                  <MetricBar
                    row={row}
                    field="cpu"
                    maximum={cpuMaximum}
                    key={`cpu:${row.namespace}:${row.name}`}
                  />
                ))}
              </div>
            </div>
            <div>
              <h4 className="text-2xs font-medium uppercase text-ink-400">
                Memory
              </h4>
              <div className="mt-1 divide-y divide-ink-700">
                {usagePagination.pageItems.map((row) => (
                  <MetricBar
                    row={row}
                    field="memory"
                    maximum={memoryMaximum}
                    key={`memory:${row.namespace}:${row.name}`}
                  />
                ))}
              </div>
            </div>
          </div>
        ) : (
          <p role="status" className="mt-3 text-xs text-ink-400">
            {metricsStatus === "available" || metricsStatus === "stale"
              ? "No current pod samples for this workload."
              : "Metrics API unavailable."}
          </p>
        )}
        <Pagination state={usagePagination} />
      </section>
      {pods.length > 0 && (
        <section
          className="border-t border-ink-700 pt-4"
          aria-label="Related pods"
        >
          <h3 className="text-xs font-semibold text-ink-100">Related pods</h3>
          <div className="mt-3 overflow-x-auto" tabIndex={0}>
            <div className="min-w-[28rem]">
              <div className="grid grid-cols-[minmax(0,1.4fr)_minmax(5rem,0.7fr)_minmax(7rem,1fr)_5rem] gap-3 border-b border-ink-700 pb-2 text-2xs font-medium uppercase text-ink-400">
                <span>Pod</span>
                <span>Status</span>
                <span>Node</span>
                <span className="text-right">Restarts</span>
              </div>
              <div className="divide-y divide-ink-700">
                {podPagination.pageItems.map((pod) => (
                  <div
                    className="grid grid-cols-[minmax(0,1.4fr)_minmax(5rem,0.7fr)_minmax(7rem,1fr)_5rem] gap-3 py-2 text-xs"
                    key={pod.name}
                  >
                    <span
                      className="truncate font-medium text-ink-100"
                      title={pod.name}
                    >
                      {pod.name}
                    </span>
                    <span className="truncate text-ink-300">
                      {pod.phase || "Unknown"}
                    </span>
                    <span className="truncate text-ink-300" title={pod.node}>
                      {pod.node || "Unassigned"}
                    </span>
                    <span className="text-right tabular-nums text-ink-300">
                      {pod.restart_count}
                    </span>
                  </div>
                ))}
              </div>
            </div>
          </div>
          <Pagination state={podPagination} />
        </section>
      )}
    </div>
  );
}

const workloadSummaryKeys = new Set([
  "desired",
  "ready",
  "available",
  "unavailable",
  "updatestrategy",
  "generation",
]);

function ResourceDetail({
  resource,
  related,
  eventCount,
  workload,
}: {
  resource: KubernetesResource;
  related?: Array<{
    resource_id?: string;
    kind: string;
    namespace?: string;
    name: string;
  }> | null;
  eventCount: number;
  workload?: Awaited<ReturnType<typeof api.kubernetesWorkload>>;
}) {
  const summary = Object.entries(resource.summary ?? {}).filter(
    ([key, value]) =>
      (value === null ||
        ["string", "number", "boolean"].includes(typeof value)) &&
      (!workload ||
        !workloadSummaryKeys.has(key.replaceAll("_", "").toLowerCase())),
  );
  return (
    <div className="space-y-5">
      <dl className="grid grid-cols-2 gap-3">
        <PeekField label="Kind">{resource.kind}</PeekField>
        <PeekField label="Namespace">
          {resource.namespace || "Cluster scope"}
        </PeekField>
        <PeekField label="API version">
          {resource.api_version || "Unavailable"}
        </PeekField>
        <PeekField label="Events">{eventCount}</PeekField>
      </dl>
      {workload && (
        <section className="border-y border-ink-700 py-4">
          <h3 className="text-xs font-semibold text-ink-100">
            Workload status
          </h3>
          <dl className="mt-3 grid grid-cols-2 gap-3">
            <PeekField label="Desired">{workload.desired ?? "n/a"}</PeekField>
            <PeekField label="Ready">{workload.ready ?? "n/a"}</PeekField>
            <PeekField label="Available">
              {workload.available ?? "n/a"}
            </PeekField>
            <PeekField label="Unavailable">
              {workload.unavailable ?? "n/a"}
            </PeekField>
            <PeekField label="Strategy">
              {workload.update_strategy ?? "n/a"}
            </PeekField>
            <PeekField label="Generation">
              {workload.generation ?? "n/a"}
            </PeekField>
          </dl>
        </section>
      )}
      {summary.length > 0 && (
        <section>
          <h3 className="text-xs font-semibold text-ink-100">Information</h3>
          <dl className="mt-3 grid grid-cols-2 gap-x-4 gap-y-3">
            {summary.map(([key, value]) => (
              <PeekField label={readable(key)} key={key}>
                {value === null ? "n/a" : String(value)}
              </PeekField>
            ))}
          </dl>
        </section>
      )}
      {asArray(resource.conditions).length > 0 && (
        <section className="border-t border-ink-700 pt-4">
          <h3 className="text-xs font-semibold text-ink-100">Conditions</h3>
          <div className="mt-2 divide-y divide-ink-700">
            {asArray(resource.conditions).map((condition) => (
              <div
                className="flex items-center justify-between gap-3 py-2 text-xs"
                key={condition.type}
              >
                <span className="text-ink-200">{condition.type}</span>
                <span
                  className={
                    condition.status === "True"
                      ? "text-sev-ok"
                      : "text-sev-warning"
                  }
                >
                  {condition.status}
                  {condition.reason ? `: ${condition.reason}` : ""}
                </span>
              </div>
            ))}
          </div>
        </section>
      )}
      {asArray(related).length > 0 && (
        <section className="border-t border-ink-700 pt-4">
          <h3 className="text-xs font-semibold text-ink-100">
            Related resources
          </h3>
          <div className="mt-2 space-y-2">
            {asArray(related).map((item) => (
              <div
                className="flex items-center justify-between gap-3 text-xs"
                key={`${item.resource_id}:${item.namespace}:${item.name}`}
              >
                <span className="truncate text-ink-200">{item.name}</span>
                <span className="pill">{item.kind}</span>
              </div>
            ))}
          </div>
        </section>
      )}
      {resource.projection_truncated?.length ? (
        <p role="status" className="text-xs text-sev-warning">
          Projection truncated: {resource.projection_truncated.join(", ")}
        </p>
      ) : null}
    </div>
  );
}
