import { createContext, useCallback, useContext, useEffect, useLayoutEffect, useRef, useState, useSyncExternalStore } from "react";
import { useInfiniteQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate, useSearchParams } from "react-router-dom";
import { ApiError, kubernetesAllContainers, type KubernetesAgentSettings, type KubernetesBreakLogQuery, type KubernetesCrashEvent, type KubernetesIssue, type KubernetesResource } from "./api";
import { useKubernetesCluster } from "./useKubernetesCluster";
import { kubernetesAccessLost } from "./kubernetesContinuity";
import { mergeBreakLogData, type BreakLogData } from "./kubernetesBreakLogMerge";

export const maximumLogWindow = 2 * 60 * 60_000;
export const CrashLogNavigationContext = createContext<{ current: ((event: KubernetesCrashEvent) => boolean) | null } | null>(null);

export function timestampNanos(value: string) {
  const match = value.match(/^(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d{1,9}))?(Z|[+-]\d\d:\d\d)$/);
  if (!match) return undefined;
  const seconds = Date.parse(`${match[1]}${match[3]}`);
  return Number.isFinite(seconds) ? BigInt(seconds) * 1_000_000n + BigInt((match[2] ?? "").padEnd(9, "0")) : undefined;
}

function shiftTimestamp(value: string, milliseconds: number) {
  const shifted = new Date(Date.parse(value) + milliseconds).toISOString();
  const fraction = value.match(/\.(\d{1,9})(?:Z|[+-]\d\d:\d\d)$/)?.[1];
  return fraction ? shifted.replace(/\.\d{3}Z$/, `.${fraction}Z`) : shifted;
}

export function breakLogWindow(params: URLSearchParams, brokeAt: string) {
  const from = timestampNanos(params.get("logs_from") ?? "");
  const to = timestampNanos(params.get("logs_to") ?? "");
  return from !== undefined && to !== undefined && to > from && to - from <= BigInt(maximumLogWindow) * 1_000_000n
    ? { from: params.get("logs_from")!, to: params.get("logs_to")! }
    : { from: shiftTimestamp(brokeAt, -5 * 60_000), to: shiftTimestamp(brokeAt, 2 * 60_000) };
}

export function useBreakLogsState(issue: KubernetesIssue | undefined, onSelectResource: (resource: KubernetesResource, tab?: "timeline" | "logs") => void) {
  const { client, cluster, key } = useKubernetesCluster();
  const [params, setParams] = useSearchParams();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [panelActive, updatePanelActive] = useState(false);
  const openContext = useRef(false);
  const retryBudget = useRef({ scope: "", started: 0, attempts: 0 });
  const setPanelActive = useCallback((active: boolean) => {
    if (active && !openContext.current) {
      retryBudget.current = { scope: "", started: Date.now(), attempts: 0 };
    }
    openContext.current = active;
    updatePanelActive(active);
  }, []);
  const [cacheFence, setCacheFence] = useState(0);
  // Observe existing settings without requiring a settings request or setup.
  const sourceScope = useSyncExternalStore(
    useCallback((notify) => queryClient.getQueryCache().subscribe(notify), [queryClient]),
    () => JSON.stringify(queryClient.getQueryData<KubernetesAgentSettings>(key("kubernetes-agent-settings"))?.break_logs ?? null),
  );
  const navigation = useContext(CrashLogNavigationContext);
  const crashPod = params.get("logs_pod");
  const crashContainer = params.get("logs_container") || undefined;
  const initialMode = params.get("logs_mode") === "previous" ? "previous" : "merged";
  const [pods, setPods] = useState<string[]>(() => crashPod ? [crashPod] : []);
  const [mode, setMode] = useState<KubernetesBreakLogQuery["mode"]>(initialMode);
  const [level, setLevel] = useState<KubernetesBreakLogQuery["level"]>();
  const [search, setSearch] = useState("");
  const [container, setContainer] = useState<string | undefined>(crashContainer);
  const filterIdentity = JSON.stringify([cluster, issue?.root.uid, issue?.root.namespace, sourceScope]);
  const [filterScope, setFilterScope] = useState(filterIdentity);
  if (filterScope !== filterIdentity) {
    setFilterScope(filterIdentity); setPods(crashPod ? [crashPod] : []); setMode(initialMode); setLevel(undefined); setSearch(""); setContainer(crashContainer);
  }
  const validIssue = Boolean(issue?.root.uid && issue.root.namespace && Number.isFinite(Date.parse(issue.broke_at ?? "")));
  const window = validIssue ? breakLogWindow(params, issue!.broke_at!) : undefined;
  const options: KubernetesBreakLogQuery = { namespace: issue?.root.namespace ?? "", root_uid: issue?.root.uid ?? "", occurrence_id: issue?.occurrence_id || params.get("occurrence_id") || undefined, center: params.get("center") || issue?.broke_at || undefined, since: window?.from ?? "", until: window?.to ?? "", pods, mode, level, q: search, limit: 500, container };
  const queryKey = key("kubernetes-break-logs", options, sourceScope, cacheFence);
  const protectedKey = JSON.stringify(queryKey);
  // Parent credential/authority fences remove protected cache entries.
  // A fresh generation cannot merge with the read they just invalidated.
  useEffect(() => queryClient.getQueryCache().subscribe(event => {
    if (JSON.stringify(event.query.queryKey) !== protectedKey) return;
    if (event.type === "removed") setCacheFence(value => value + 1);
  }), [protectedKey, queryClient]);
  const query = useInfiniteQuery({
    queryKey, queryFn: async ({ pageParam, signal }) => {
      const page = await client.kubernetesBreakLogs({ ...options, cursor: pageParam }, signal);
      signal.throwIfAborted();
      const prior = queryClient.getQueryData<BreakLogData>(queryKey)?.pages[0];
      if (options.occurrence_id && page.occurrence_id !== options.occurrence_id
        || prior?.occurrence_id && page.occurrence_id !== prior.occurrence_id) {
        throw new ApiError(502, "Break logs do not match the selected occurrence", { code: "break_log_selection_mismatch" });
      }
      return page;
    },
    structuralSharing: (previous, incoming) => mergeBreakLogData(previous as BreakLogData | undefined, incoming as BreakLogData),
    initialPageParam: undefined as string | undefined, getNextPageParam: (page) => page.next || undefined,
    enabled: validIssue && panelActive, retry: false, staleTime: Infinity, gcTime: 0, refetchInterval: false, refetchOnWindowFocus: false,
  });
  const pendingLinked = Boolean(query.data?.pages[0]?.coverage.gaps.some(gap => gap.reason === "linked_pending" && gap.ready_at));
  if (retryBudget.current.scope !== protectedKey) retryBudget.current = { scope: protectedKey, started: Date.now(), attempts: 0 };
  useEffect(() => {
    const queryKey = JSON.parse(protectedKey) as readonly unknown[];
    if (!panelActive || !validIssue || query.isError || !pendingLinked) return;
    const budget = retryBudget.current;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let automaticInFlight = false;
    const deadline = setTimeout(() => {
      clearTimeout(timer);
      if (automaticInFlight) void queryClient.cancelQueries({ queryKey, exact: true });
    }, Math.max(0, budget.started + 300_000 - Date.now()));
    const schedule = () => {
      clearTimeout(timer);
      const delay = [15_000, 30_000, 60_000][Math.min(2, budget.attempts)];
      if (document.visibilityState === "hidden" || Date.now() + delay > budget.started + 300_000) return;
      timer = setTimeout(refresh, delay);
    };
    const refresh = () => {
      if (Date.now() >= budget.started + 300_000) return;
      if (document.visibilityState !== "hidden" && !queryClient.isFetching({ queryKey, exact: true })) {
        budget.attempts++;
        // The presentation is already collapsed; merge retains transport
        // provenance and the advanced continuation independently.
        automaticInFlight = true;
        void queryClient.refetchQueries({ queryKey, exact: true }).finally(() => { automaticInFlight = false; });
      }
      schedule();
    };
    const visibilityChanged = () => {
      clearTimeout(timer);
      if (document.visibilityState === "hidden") void queryClient.cancelQueries({ queryKey, exact: true });
      else schedule();
    };
    if (document.visibilityState === "hidden") visibilityChanged();
    else schedule();
    document.addEventListener("visibilitychange", visibilityChanged);
    return () => {
      clearTimeout(timer);
      clearTimeout(deadline);
      document.removeEventListener("visibilitychange", visibilityChanged);
    };
  }, [validIssue, panelActive, query.isError, pendingLinked, protectedKey, queryClient]);
  useEffect(() => {
    const queryKey = JSON.parse(protectedKey) as readonly unknown[];
    const hidden = () => {
      if (document.visibilityState === "hidden") void queryClient.cancelQueries({ queryKey, exact: true });
      else if (panelActive && validIssue && !queryClient.getQueryState(queryKey)?.data && !queryClient.getQueryState(queryKey)?.error) void queryClient.refetchQueries({ queryKey, exact: true });
    };
    document.addEventListener("visibilitychange", hidden);
    return () => {
      document.removeEventListener("visibilitychange", hidden);
      void queryClient.cancelQueries({ queryKey, exact: true });
    };
  }, [protectedKey, panelActive, validIssue, queryClient]);
  const denied = kubernetesAccessLost(query.error) || query.error instanceof ApiError && query.error.body && typeof query.error.body === "object" && "code" in query.error.body && query.error.body.code === "break_log_selection_mismatch";
  useLayoutEffect(() => {
    if (!denied) return;
    const queryKey = JSON.parse(protectedKey) as readonly unknown[];
    void queryClient.cancelQueries({ queryKey, exact: true });
    queryClient.getQueryCache().find({ queryKey, exact: true })?.setState({ data: undefined });
  }, [denied, protectedKey, queryClient]);
  const page = denied ? undefined : query.data?.pages[0];
  const lines = denied ? [] : query.data?.pages.flatMap((item) => item.lines ?? []) ?? [];
  const setWindow = (from: number, to: number) => {
    if (!Number.isFinite(from) || !Number.isFinite(to) || to <= from || to - from > maximumLogWindow) return;
    setParams((current) => { const next = new URLSearchParams(current); next.set("logs_from", new Date(from).toISOString()); next.set("logs_to", new Date(to).toISOString()); return next; }, { replace: true });
  };
  const openPod = (pod: string, uid?: string, tab?: "logs", namespace = issue?.root.namespace) => {
    if (!namespace || !pod) return;
    onSelectResource({ resource_id: "core~v1~pods", kind: "Pod", namespace, name: pod, ...(uid ? { uid } : {}) }, tab);
  };
  const beforeCrash = useCallback((event: KubernetesCrashEvent) => {
    // A sidecar or init container crash must show that container's logs,
    // not the main container the picker defaults to.
    setPods([event.pod_uid || event.pod]); setMode("previous"); setLevel(undefined); setSearch(""); setContainer(event.container || undefined);
    setParams((current) => {
      const next = new URLSearchParams(current);
      next.delete("r"); next.delete("tab");
      if (next.get("root_uid") !== event.root_uid) next.set("center", event.at);
      next.set("root_uid", event.root_uid); next.set("namespace", event.namespace);
      next.set("logs_from", shiftTimestamp(event.at, -5 * 60_000)); next.set("logs_to", event.at);
      next.set("logs_mode", "previous"); next.set("logs_pod", event.pod_uid || event.pod);
      if (event.container) next.set("logs_container", event.container); else next.delete("logs_container");
      return next;
    }, { replace: true });
    document.getElementById("kubernetes-break-logs")?.scrollIntoView?.({ block: "nearest", behavior: "smooth" });
  }, [setParams]);
  const rootUID = issue?.root.uid;
  useEffect(() => {
    if (!navigation || !validIssue || !rootUID) return;
    const open = (event: KubernetesCrashEvent) => {
      if (event.root_uid !== rootUID) return false;
      beforeCrash(event);
      return true;
    };
    navigation.current = open;
    return () => { if (navigation.current === open) navigation.current = null; };
  }, [navigation, validIssue, rootUID, beforeCrash]);
  const liveAvailable = Boolean(issue && ["Pod", "Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob"].includes(issue.root.kind));
  const live = () => {
    if (!issue || !liveAvailable) return;
    const ids: Record<string, string> = { Pod: "core~v1~pods", Deployment: "apps~v1~deployments", StatefulSet: "apps~v1~statefulsets", DaemonSet: "apps~v1~daemonsets", Job: "batch~v1~jobs", CronJob: "batch~v1~cronjobs" };
    onSelectResource({ ...issue.root, resource_id: ids[issue.root.kind] }, "logs");
  };
  const ask = () => {
    if (!issue || !window) return;
    const ids: Record<string, string> = { Deployment: "apps~v1~deployments", StatefulSet: "apps~v1~statefulsets", DaemonSet: "apps~v1~daemonsets", ReplicaSet: "apps~v1~replicasets", Pod: "core~v1~pods", Job: "batch~v1~jobs", CronJob: "batch~v1~cronjobs", Node: "core~v1~nodes" };
    const resourceID = ids[issue.root.kind];
    if (!resourceID) return;
    const query = new URLSearchParams({ provider: "kubernetes", cluster: cluster ?? "", resource_id: resourceID, namespace: issue.root.namespace ?? "", name: issue.root.name, uid: issue.root.uid ?? "", include_changes: "true", logs_from: window.from, logs_to: window.to });
    navigate(`/agent/chat?${query}`);
  };
  // The container list does not depend on the filter, so keep the last one
  // while another container's page loads; the picker never empties.
  const containerScope = JSON.stringify([filterIdentity, cacheFence, options.occurrence_id, options.center, window]);
  const [known, setKnown] = useState<{ scope: string; containers: NonNullable<typeof page>["containers"]; main?: string }>({ scope: containerScope, containers: [] });
  if (page?.containers?.length && (known.scope !== containerScope || known.containers !== page.containers)) setKnown({ scope: containerScope, containers: page.containers, main: page.main_container });
  const remembered = !denied && known.scope === containerScope ? known : undefined;
  const containers = page?.containers ?? remembered?.containers ?? [];
  // Main container first, then the first non-init container when the server
  // could not tell which one is the app.
  const mainContainer = (page ? page.main_container : remembered?.main) ?? containers.find((item) => item.main)?.name ?? containers.find((item) => !item.init)?.name;
  const selectedContainer = container ?? page?.container ?? mainContainer ?? kubernetesAllContainers;
  return { issue, window, validIssue, setPanelActive, containers, mainContainer, selectedContainer, setContainer, query: { ...query, fetchNextPage: () => queryClient.isFetching({ queryKey, exact: true }) ? Promise.resolve() : query.fetchNextPage() }, page, lines, pods, setPods, mode, setMode, level, setLevel, search, setSearch, setWindow, beforeCrash, openPod, live, liveAvailable, ask, restart: () => queryClient.resetQueries({ queryKey, exact: true }) };
}

export const BreakLogsContext = createContext<ReturnType<typeof useBreakLogsState> | null>(null);
export const useBreakLogs = () => useContext(BreakLogsContext);