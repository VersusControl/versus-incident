import { createContext, useContext, useEffect, useState } from "react";
import { useInfiniteQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate, useSearchParams } from "react-router-dom";
import { ApiError, type KubernetesBreakLogQuery, type KubernetesCrashEvent, type KubernetesIssue, type KubernetesResource } from "./api";
import { useKubernetesCluster } from "./useKubernetesCluster";

export const maximumLogWindow = 2 * 60 * 60_000;

export function breakLogWindow(params: URLSearchParams, brokeAt: string) {
  const broke = Date.parse(brokeAt);
  const from = Date.parse(params.get("logs_from") ?? "");
  const to = Date.parse(params.get("logs_to") ?? "");
  return Number.isFinite(from) && Number.isFinite(to) && to > from && to - from <= maximumLogWindow
    ? { from: new Date(from).toISOString(), to: new Date(to).toISOString() }
    : { from: new Date(broke - 5 * 60_000).toISOString(), to: new Date(broke + 2 * 60_000).toISOString() };
}

export function useBreakLogsState(issue: KubernetesIssue | undefined, onSelectResource: (resource: KubernetesResource, tab?: "timeline" | "logs") => void) {
  const { client, cluster, key } = useKubernetesCluster();
  const [params, setParams] = useSearchParams();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const crashPod = params.get("logs_pod");
  const initialMode = params.get("logs_mode") === "previous" ? "previous" : "merged";
  const [pods, setPods] = useState<string[]>(() => crashPod ? [crashPod] : []);
  const [mode, setMode] = useState<KubernetesBreakLogQuery["mode"]>(initialMode);
  const [level, setLevel] = useState<KubernetesBreakLogQuery["level"]>();
  const [search, setSearch] = useState("");
  const filterIdentity = JSON.stringify([cluster, issue?.root.uid, issue?.root.namespace]);
  const [filterScope, setFilterScope] = useState(filterIdentity);
  if (filterScope !== filterIdentity) {
    setFilterScope(filterIdentity); setPods(crashPod ? [crashPod] : []); setMode(initialMode); setLevel(undefined); setSearch("");
  }
  const validIssue = Boolean(issue?.root.uid && issue.root.namespace && Number.isFinite(Date.parse(issue.broke_at ?? "")));
  const window = validIssue ? breakLogWindow(params, issue!.broke_at!) : undefined;
  const options: KubernetesBreakLogQuery = { namespace: issue?.root.namespace ?? "", root_uid: issue?.root.uid ?? "", since: window?.from ?? "", until: window?.to ?? "", pods, mode, level, q: search, limit: 500 };
  const queryKey = key("kubernetes-break-logs", options);
  const query = useInfiniteQuery({ queryKey, queryFn: async ({ pageParam, signal }) => ({ ...await client.kubernetesBreakLogs({ ...options, cursor: pageParam }, signal), fetchedAt: Date.now() }), initialPageParam: undefined as string | undefined, getNextPageParam: (page) => page.next || undefined, enabled: validIssue, retry: false, staleTime: Infinity, refetchInterval: false, refetchOnWindowFocus: false });
  const pollingQuery = queryClient.getQueryCache().find({ queryKey, exact: true });
  const firstPageFetchedAt = query.data?.pages[0]?.fetchedAt;
  const broke = Date.parse(issue?.broke_at ?? "");
  const activeCapture = issue?.broken && window && Date.parse(window.from) <= broke + 2 * 60_000 && Date.parse(window.to) >= broke;
  const settleAt = window ? Math.max(Date.parse(window.to), activeCapture ? broke + 2 * 60_000 : 0) + 60_000 : 0;
  useEffect(() => {
    if (!pollingQuery || !validIssue || query.isError || (firstPageFetchedAt !== undefined && firstPageFetchedAt >= settleAt)) return;
    const queryKey = pollingQuery.queryKey;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const schedule = () => {
      clearTimeout(timer);
      if (document.visibilityState === "hidden" || (firstPageFetchedAt === undefined && Date.now() >= settleAt)) return;
      timer = setTimeout(refresh, 10_000);
    };
    const refresh = () => {
      if (document.visibilityState !== "hidden" && !queryClient.isFetching({ queryKey, exact: true })) {
        queryClient.setQueryData<typeof query.data>(queryKey, (data) => data && ({ ...data, pages: data.pages.slice(0, 1), pageParams: data.pageParams.slice(0, 1) }));
        void queryClient.refetchQueries({ queryKey, exact: true });
      }
      schedule();
    };
    const visibilityChanged = () => {
      clearTimeout(timer);
      if (document.visibilityState === "hidden") void queryClient.cancelQueries({ queryKey, exact: true });
      else refresh();
    };
    if (document.visibilityState === "hidden") visibilityChanged();
    else schedule();
    document.addEventListener("visibilitychange", visibilityChanged);
    return () => {
      clearTimeout(timer);
      document.removeEventListener("visibilitychange", visibilityChanged);
    };
  }, [validIssue, query.isError, firstPageFetchedAt, settleAt, queryClient, pollingQuery]);
  const denied = query.error instanceof ApiError && [401, 403, 404].includes(query.error.status);
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
  const beforeCrash = (event: KubernetesCrashEvent) => {
    const end = Date.parse(event.at);
    setPods([event.pod_uid || event.pod]); setMode("previous"); setLevel(undefined); setSearch("");
    setParams((current) => {
      const next = new URLSearchParams(current);
      if (next.get("root_uid") !== event.root_uid) next.set("center", event.at);
      next.set("root_uid", event.root_uid); next.set("namespace", event.namespace);
      next.set("logs_from", new Date(end - 5 * 60_000).toISOString()); next.set("logs_to", new Date(end).toISOString());
      next.set("logs_mode", "previous"); next.set("logs_pod", event.pod_uid || event.pod);
      return next;
    }, { replace: true });
    document.getElementById("kubernetes-break-logs")?.scrollIntoView?.({ block: "nearest", behavior: "smooth" });
  };
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
  return { issue, window, validIssue, query: { ...query, fetchNextPage: () => queryClient.isFetching({ queryKey, exact: true }) ? Promise.resolve() : query.fetchNextPage() }, page, lines, pods, setPods, mode, setMode, level, setLevel, search, setSearch, setWindow, beforeCrash, openPod, live, liveAvailable, ask, restart: () => queryClient.resetQueries({ queryKey, exact: true }) };
}

export const BreakLogsContext = createContext<ReturnType<typeof useBreakLogsState> | null>(null);
export const useBreakLogs = () => useContext(BreakLogsContext);