import { useId, useLayoutEffect, useRef, useState, type ReactNode } from "react";
import { createPortal } from "react-dom";
import { Link } from "react-router-dom";
import { Activity, FileText, Lightbulb, Loader2, Maximize2, MessageSquare, Search } from "lucide-react";
import { Modal } from "@/components/Modal";
import { ApiError, kubernetesAllContainers, type KubernetesBreakLogPage, type KubernetesIssue, type KubernetesResource } from "@/lib/api";
import { useKubernetesCluster } from "@/lib/useKubernetesCluster";
import { BreakLogsContext, maximumLogWindow as maximumWindow, useBreakLogs, useBreakLogsState } from "@/lib/useKubernetesBreakLogs";
import { kubernetesAccessLost } from "@/lib/kubernetesContinuity";

const minimumWindow = 30_000;
const time = (value: string) => new Date(value).toLocaleTimeString([], { hour12: false });

export function KubernetesBreakLogsProvider({ issue, onSelectResource, children, onAccessLost }: { issue?: KubernetesIssue; onSelectResource: (resource: KubernetesResource, tab?: "timeline" | "logs") => void; children: ReactNode; onAccessLost?: (error: Error) => void }) {
  const state = useBreakLogsState(issue, onSelectResource);
  useLayoutEffect(() => {
    if (state.query.error && kubernetesAccessLost(state.query.error)) onAccessLost?.(state.query.error);
  }, [state.query.error, onAccessLost]);
  return <BreakLogsContext.Provider value={state}>{children}</BreakLogsContext.Provider>;
}

export function LogWindowBracket({ since, until, captionViewport }: { since: string; until: string; captionViewport?: { left: number; width: number } }) {
  const state = useBreakLogs();
  const dragging = useRef<"from" | "to" | null>(null);
  const plot = useRef<HTMLDivElement>(null);
  if (!state?.window) return null;
  const start = Date.parse(since), end = Date.parse(until);
  const from = Date.parse(state.window.from), to = Date.parse(state.window.to);
  if (end <= start) return null;
  const position = (value: number) => (value - start) / (end - start) * 100;
  const change = (edge: "from" | "to", value: number) => {
    if (edge === "from") state.setWindow(Math.max(to - maximumWindow, Math.min(to - minimumWindow, value)), to);
    else state.setWindow(from, Math.min(from + maximumWindow, Math.max(from + minimumWindow, value)));
  };
  const relative = (value: number) => { const offset = Math.round((value - Date.parse(state.issue!.broke_at!)) / 30_000) / 2; return `${offset > 0 ? "+" : ""}${offset} m`; };
  const captionWidth = captionViewport ? Math.min(220, Math.max(0, captionViewport.width)) : 220;
  const captionLeft = captionViewport?.left ?? 0;
  const plotMax = `max(0px, calc(100% - ${captionWidth}px))`;
  const captionMin = `min(${captionLeft}px, ${plotMax})`;
  const captionMax = captionViewport ? `min(${captionLeft + Math.max(0, captionViewport.width - captionWidth)}px, ${plotMax})` : plotMax;
  return <div ref={plot} className="break-log-bracket-plot" aria-label="Log window">
    {to >= start && from <= end && <>
      <div className="break-log-bracket" style={{ left: `${Math.max(0, position(from))}%`, right: `${100 - Math.min(100, position(to))}%` }} />
      <span className="break-log-window-caption" style={{ width: captionWidth, left: `clamp(${captionMin}, calc(${(Math.max(0, position(from)) + Math.min(100, position(to))) / 2}% - ${captionWidth / 2}px), ${captionMax})` }}>log window {relative(from)} ... {relative(to)}</span>
    </>}
    {(["from", "to"] as const).map((edge) => { const value = edge === "from" ? from : to; return value >= start && value <= end ? <button key={edge} type="button" role="slider" aria-label={`Log window ${edge === "from" ? "start" : "end"}`} aria-valuemin={edge === "from" ? to - maximumWindow : from + minimumWindow} aria-valuemax={edge === "from" ? to - minimumWindow : from + maximumWindow} aria-valuenow={value} aria-valuetext={time(new Date(value).toISOString())} className="break-log-handle" style={{ left: `${position(value)}%` }} onKeyDown={(event) => {
      const delta = event.key === "ArrowRight" || event.key === "ArrowUp" ? 30_000 : event.key === "ArrowLeft" || event.key === "ArrowDown" ? -30_000 : 0;
      if (delta) { event.preventDefault(); change(edge, value + delta); }
    }} onPointerDown={(event) => { event.preventDefault(); dragging.current = edge; event.currentTarget.focus(); event.currentTarget.setPointerCapture?.(event.pointerId); }} onPointerMove={(event) => { if (dragging.current !== edge) return; const bounds = plot.current?.getBoundingClientRect(); if (bounds?.width) change(edge, Math.round((start + Math.max(0, Math.min(1, (event.clientX - bounds.left) / bounds.width)) * (end - start)) / 1000) * 1000); }} onPointerUp={() => { dragging.current = null; }} onPointerCancel={() => { dragging.current = null; }}><span className="sr-only">{edge}</span></button> : null; })}
  </div>;
}

export function BreakLogRateStrip({ since, until }: { since: string; until: string }) {
  const state = useBreakLogs();
  if (!state?.window) return null;
  const start = Date.parse(since), end = Date.parse(until);
  const rate = state.page?.rate ?? [];
  const peak = Math.max(1, ...rate.map((bucket) => bucket.total));
  return <div className="resource-timeline-row break-log-rate"><div className="resource-timeline-label"><strong>Log lines</strong><span>per minute, errors in red</span></div><div className="resource-timeline-plot" role="img" aria-label={state.query.isPending ? "Loading log rates" : rate.length ? "Log lines per minute, errors stacked in red" : "No log rate evidence"}>{rate.filter((bucket) => Date.parse(bucket.minute) >= start && Date.parse(bucket.minute) <= end).map((bucket) => <div title={`${time(bucket.minute)}: ${bucket.total} lines, ${bucket.errors} errors`} key={bucket.minute} className="break-log-rate-bar" style={{ left: `${(Date.parse(bucket.minute) - start) / (end - start) * 100}%`, width: `${Math.min(10, 60_000 / (end - start) * 85)}%`, height: `${bucket.total / peak * 90}%` }}><span style={{ height: `${bucket.total ? bucket.errors / bucket.total * 100 : 0}%` }} /></div>)}</div></div>;
}

const tips: Record<string, string> = {
  restarted_since: "The container restarted more than once since the break; Kubernetes keeps only the last previous container.",
  pod_deleted: "The pod was deleted before Versus captured its logs.",
  capture_late: "Logs before the available capture were not available when Versus captured.",
  rotated: "The node rotated these logs before the window start.",
  forbidden: "The Kubernetes reader cannot read pod logs.",
  budget: "Capture was deferred to stay within the read budget.",
};

function sourceName(source: KubernetesBreakLogPage["sources"][number]) {
  return source.kind === "captured" ? "Captured at break" : source.kind === "kubernetes" ? "Kubernetes container logs" : source.name;
}

const sidePanelLimit = 5;

function BreakLogSidePanel({ page, paneHeight }: { page?: KubernetesBreakLogPage; paneHeight: string }) {
  const [allMessages, setAllMessages] = useState(false);
  const [allSources, setAllSources] = useState(false);
  const patterns = page?.patterns ?? [];
  const sources = page?.sources ?? [];
  const peak = Math.max(1, ...patterns.map((item) => item.count));
  const shownPatterns = allMessages ? patterns : patterns.slice(0, sidePanelLimit);
  const shownSources = allSources ? sources : sources.slice(0, sidePanelLimit);
  return <aside aria-label="Log window summary" aria-busy={!page} data-break-log-side className={`${paneHeight} min-w-0 space-y-4 overflow-y-auto overscroll-contain pr-1 text-xs`}>
    <section aria-label="Top messages"><h4 className="flex items-baseline justify-between gap-2 font-semibold text-ink-100">Top messages{page && <span className="text-2xs font-normal text-ink-400">{patterns.length} unique</span>}</h4>
      {!page ? <SidePanelSkeleton /> : <>
        <ul className="mt-3 space-y-3">{shownPatterns.map((pattern, index) => <li key={index}><div className="flex items-start gap-2"><i aria-hidden="true" className={`mt-1 size-1.5 shrink-0 rounded-full ${pattern.level === "error" ? "bg-sev-critical" : pattern.level === "warning" ? "bg-sev-warn" : "bg-accent"}`} /><span title={pattern.template} className="line-clamp-2 min-w-0 flex-1 break-words text-ink-200">{pattern.template}</span><span className="tabular-nums text-ink-100">{pattern.count}</span></div><div className="ml-3.5 mt-1 h-1 bg-ink-600"><div className={pattern.level === "error" ? "h-full bg-sev-critical" : pattern.level === "warning" ? "h-full bg-sev-warn" : "h-full bg-accent"} style={{ width: `${pattern.count / peak * 100}%` }} /></div></li>)}</ul>
        {patterns.length > sidePanelLimit && <button type="button" aria-expanded={allMessages} className="mt-2 text-link" onClick={() => setAllMessages(!allMessages)}>{allMessages ? `Show top ${sidePanelLimit}` : `Show all (${patterns.length})`}</button>}
        {!patterns.length && <p className="mt-2 text-ink-300">No message patterns in the filtered window.</p>}
      </>}
    </section>
    <section aria-label="Sources" className="border-t border-ink-600 pt-3"><h4 className="flex items-baseline justify-between gap-2 font-semibold text-ink-100">Sources{page && <span className="text-2xs font-normal text-ink-400">{sources.length} {sources.length === 1 ? "source" : "sources"}</span>}</h4>
      {!page ? <SidePanelSkeleton /> : <>
        <ul className="mt-2 space-y-2 text-ink-300">{shownSources.map((source, index) => <li key={index} title={`${sourceName(source)} ${time(source.at)}`} className="line-clamp-2 break-words">{sourceName(source)} {time(source.at)}</li>)}</ul>
        {sources.length > sidePanelLimit && <button type="button" aria-expanded={allSources} className="mt-2 text-link" onClick={() => setAllSources(!allSources)}>{allSources ? `Show top ${sidePanelLimit}` : `Show all (${sources.length})`}</button>}
        {!sources.some((source) => source.kind === "linked") && <p className="mt-2 text-ink-300">No linked-source lines in this window.</p>}
        <p className="mt-2 text-ink-300">Coverage: {page.coverage.state}</p>
        {(page.coverage.gaps ?? []).map((gap, index) => <p key={index} className="mt-1 font-mono text-2xs text-ink-300">Gap {time(gap.from)} - {time(gap.to)}</p>)}
      </>}
    </section>
  </aside>;
}

function SidePanelSkeleton() {
  return <div aria-hidden="true" className="mt-3 space-y-3">{Array.from({ length: 3 }, (_, index) => <div key={index} className="sk h-6" />)}</div>;
}

function BreakLogViewer({ expanded }: { expanded: boolean }) {
  const state = useBreakLogs()!;
  const { page, query } = state;
  const loading = !page && !query.isError;
  const paneHeight = expanded ? "h-[60vh]" : "h-80";
  const rows = [
    ...state.lines.map((line, index) => ({ at: line.at, key: `line:${index}`, line, marker: undefined })),
    ...(page?.markers ?? []).map((marker, index) => ({ at: marker.at, key: `marker:${index}`, line: undefined, marker })),
  ].sort((left, right) => Date.parse(left.at) - Date.parse(right.at) || Number(Boolean(right.marker)) - Number(Boolean(left.marker)));
  return <div className="min-w-0">
    {/* One fixed-height row that scrolls sideways: pod pills arriving with
        the data must never wrap and push the log pane down. */}
    <div role="group" aria-label="Log filters" data-break-log-filters className="flex h-14 min-w-0 flex-nowrap items-center gap-2 overflow-x-auto overflow-y-hidden border-b border-ink-600 text-xs [&>*]:shrink-0">
      <div role="group" aria-label="Log pods" className="flex flex-nowrap gap-1"><button type="button" className={`pill min-h-8 ${!state.pods.length ? "pill-accent" : ""}`} aria-pressed={!state.pods.length} onClick={() => state.setPods([])}>All pods {page?.pods?.length ?? "?"}</button>{(page?.pods ?? []).map((pod) => { const identity = pod.uid || pod.name; return <button type="button" key={identity} className={`pill min-h-8 max-w-48 shrink-0 gap-1.5 ${state.pods.includes(identity) ? "pill-accent" : ""}`} aria-pressed={state.pods.includes(identity)} title={pod.name} onClick={() => state.setPods(state.pods.includes(identity) ? state.pods.filter((value) => value !== identity) : [...state.pods, identity])}><i aria-hidden="true" className={`size-1.5 shrink-0 rounded-full ${pod.state === "crashed" ? "bg-sev-critical" : pod.state === "running" ? "bg-sev-ok" : pod.state === "not_ready" ? "bg-sev-warn" : "bg-ink-400"}`} /><span className="truncate">{pod.name}</span>{pod.state === "unknown" && <span className="sr-only">unknown Pod state</span>}</button>; })}</div>
      <div role="group" aria-label="Container log mode" className="flex flex-nowrap rounded-control border border-ink-500 p-0.5">{([["merged", "Merged"], ["previous", "Previous container"], ["current", "Current"]] as const).map(([value, label]) => <button type="button" key={value} aria-pressed={state.mode === value} className={`min-h-8 whitespace-nowrap rounded-control px-2 ${state.mode === value ? "bg-accent-subtle text-link" : "text-ink-300"}`} onClick={() => state.setMode(value)}>{label}</button>)}</div>
      <button type="button" aria-pressed={state.level === "error"} className="pill pill-bad min-h-8" onClick={() => state.setLevel(state.level === "error" ? undefined : "error")}>Errors {page?.counts.errors ?? "?"}</button><button type="button" aria-label={`Filter log warnings ${page?.counts.warnings ?? "unavailable"}`} aria-pressed={state.level === "warning"} className="pill pill-warn min-h-8" onClick={() => state.setLevel(state.level === "warning" ? undefined : "warning")}>Warnings {page?.counts.warnings ?? "?"}</button>
      <label className="relative min-w-40 flex-1"><Search size={13} aria-hidden="true" className="absolute left-2 top-2.5 text-ink-300" /><input aria-label="Search in log window" className="input w-full min-w-40 !pl-7" placeholder="Search in window" value={state.search} onChange={(event) => state.setSearch(event.target.value)} /></label>
    </div>
    {query.isError && <p role="alert" className="py-3 text-xs text-sev-warning">{query.error instanceof ApiError && query.error.status === 400 ? "This log page expired or its filters changed. Restart paging." : "Break logs unavailable."}<button type="button" className="btn ml-2" onClick={() => void state.restart()}>Reload logs</button></p>}
    {(page || loading) && <div data-break-log-panes className="grid min-w-0 gap-4 pt-3 lg:grid-cols-[minmax(0,1fr)_17rem]"><div className="min-w-0"><div role="log" aria-label="Break log lines" aria-busy={loading} data-break-log-pane className={`${paneHeight} overflow-auto rounded-control bg-surface-sunken font-mono text-2xs`}>
      {loading ? <><p role="status" className="sr-only">Loading logs at break...</p><div aria-hidden="true" className="space-y-2 p-3">{Array.from({ length: 8 }, (_, index) => <div key={index} className="sk h-3" style={{ width: `${90 - (index % 3) * 18}%` }} />)}</div></> : <>
        {rows.map((row) => row.marker ? <div className="my-2 border-t border-sev-critical px-3 py-1 text-sev-critical" key={row.key}><strong>{row.marker.type}</strong> {time(row.at)} {row.marker.label}</div> : row.line && <LogLine key={row.key} line={row.line} pods={page?.pods ?? []} />)}
        {!state.lines.length && page && <p role="status" className="p-3 text-ink-300">No lines match this window and filters. Coverage: {page.coverage.state}.</p>}
      </>}
    </div><footer className="flex min-h-14 flex-wrap items-center justify-between gap-2 py-3 text-2xs text-ink-300">{page ? <><span>{page.counts.all.toLocaleString()} lines from {page.pods?.length ?? 0} pods in this window; {state.lines.length} matching lines loaded</span>{query.hasNextPage && <button type="button" className="min-h-8 text-link" disabled={query.isFetchingNextPage} onClick={() => void query.fetchNextPage()}>{query.isFetchingNextPage ? "Loading..." : "Load more logs"}</button>}</> : <span aria-hidden="true" className="sk h-3 w-48" />}</footer></div>
      <BreakLogSidePanel key={JSON.stringify(state.window)} page={page} paneHeight={paneHeight} />
    </div>}
  </div>;
}

function LogLine({ line, pods }: { line: KubernetesBreakLogPage["lines"][number]; pods: KubernetesBreakLogPage["pods"] }) {
  const pod = line.pod || pods.find((item) => item.uid && item.uid === line.pod_uid)?.name;
  const short = pod?.split("-").at(-1) || "unknown";
  const identity = `${pod ?? "Pod unavailable"} / ${line.init ? "init " : ""}${line.container} / ${line.slot} / ${line.source}`;
  return <div className={`grid min-w-0 grid-cols-[4.5rem_3.5rem_minmax(0,1fr)] gap-x-2 px-3 py-1 sm:grid-cols-[4.5rem_4.5rem_3.5rem_minmax(0,1fr)] ${line.level === "error" ? "bg-sev-critical/10" : ""}`}>
    <span className="min-w-0 text-ink-300"><time dateTime={line.at}>{time(line.at)}</time><span title={identity} className="block truncate sm:hidden">{short}</span></span>
    <span title={identity} className="hidden truncate text-ink-300 sm:block">{short}</span>
    <span className={line.level === "error" ? "text-sev-critical" : line.level === "warning" ? "text-sev-warning" : "text-ink-300"}>{line.level.toUpperCase()}</span>
    <span className="min-w-0 whitespace-pre-wrap break-words text-ink-100 [overflow-wrap:anywhere]">{line.text}</span>
  </div>;
}

function ContainerSelect() {
  const state = useBreakLogs()!;
  const known = state.containers.length > 0;
  // Before the list arrives the server default is the main container, so the
  // label does not change width (and wrap the header) when data lands.
  const main = !known || state.selectedContainer !== kubernetesAllContainers && state.selectedContainer === state.mainContainer;
  return <label className="inline-flex min-w-0 items-center gap-1.5 text-ink-300"><span className="whitespace-nowrap">{main ? "Main container:" : "Container:"}</span>
    <select aria-label="Log container" disabled={!known} className="input !h-7 w-36 !py-0 text-xs" value={known ? state.selectedContainer : ""} onChange={(event) => state.setContainer(event.target.value)}>
      {!known && <option value="">Loading…</option>}
      {state.containers.map((item) => <option key={item.name} value={item.name}>{item.init ? "init: " : ""}{item.name}{item.name === state.mainContainer ? " (main)" : ""}</option>)}
      {known && <option value={kubernetesAllContainers}>All containers</option>}
    </select>
  </label>;
}

export function KubernetesBreakLogs({ pending = false, mounted = false }: { pending?: boolean; mounted?: boolean }) {
  const state = useBreakLogs();
  const [expanded, setExpanded] = useState(false);
  const [tipOpen, setTipOpen] = useState(false);
  const tipID = useId();
  const { cluster } = useKubernetesCluster();
  const ready = Boolean(state?.validIssue && state.window);
  if (!state || !ready && !pending && !mounted) return null;
  const { page } = state;
  const params = new URLSearchParams({ view: "agent" });
  if (cluster) params.set("cluster", cluster);
  const showTip = page?.coverage?.state !== "complete" && page?.coverage?.tip;
  const loading = pending || ready && state.query.isFetching && !state.query.isFetchingNextPage;
  const spinner = (announce: boolean) => <span data-break-log-spinner className="inline-flex size-4 shrink-0 items-center justify-center text-ink-300">{loading && <Loader2 size={14} {...announce ? { role: "status", "aria-label": "Loading logs at break" } : { "aria-hidden": true }} className="animate-spin motion-reduce:animate-none" />}</span>;
  const actions = <><button type="button" className="btn" disabled={!ready} onClick={state.ask}><MessageSquare size={14} />Ask chat</button><button type="button" className="btn" disabled={!ready || !state.liveAvailable} title={state.liveAvailable ? `Open ${state.issue?.root.name}` : "Live logs are unavailable for this resource kind"} onClick={state.live}><Activity size={14} />Open live logs</button></>;
  return <section id="kubernetes-break-logs" aria-label="Logs at break" aria-busy={loading} className="min-w-0 border-t border-ink-600 px-3 pb-16 pt-3 sm:px-4 sm:pb-3">
    <header className="flex min-w-0 flex-wrap items-center gap-2 text-xs"><FileText size={16} aria-hidden="true" /><h3 className="text-sm font-semibold text-ink-50">Logs at break</h3>{state.window ? <span className="break-log-window-chip font-mono">{time(state.window.from)} - {time(state.window.to)}</span> : pending ? <span aria-hidden="true" className="sk h-5 w-36" /> : <span className="text-ink-400">Time unavailable</span>}
      {state.issue?.broke_at_source === "observed_at" && ready && <span className="text-ink-400">Observation time; finish time unknown</span>}
      <ContainerSelect />
      {showTip && <div className="relative" onKeyDown={(event) => { if (event.key === "Escape") setTipOpen(false); }}><button type="button" aria-label="Log coverage tip" title="Log coverage tip" aria-expanded={tipOpen} aria-controls={tipID} className="btn-icon" onClick={() => setTipOpen(!tipOpen)}><Lightbulb size={14} /></button>{tipOpen && <section id={tipID} aria-label="Log coverage tip" className="fixed inset-x-4 bottom-20 z-20 max-w-[calc(100vw-2rem)] space-y-3 rounded-control border border-ink-500 bg-surface-raised p-4 shadow-overlay sm:absolute sm:inset-x-auto sm:bottom-auto sm:left-0 sm:top-full sm:w-72"><h4 className="font-medium text-ink-100">Some logs are not available from Kubernetes</h4><p className="text-ink-200">{page.coverage.reason === "capture_late" && page.coverage.gaps?.[0] ? `Logs before ${time(page.coverage.gaps[0].to)} were not available when Versus captured.` : tips[page.coverage.reason] ?? "Kubernetes could not cover the full log window."}</p><Link to={`/agent/kubernetes?${params}`} className="block text-link">Link a source in Agent tab</Link></section>}</div>}
      <div className="ml-auto hidden flex-wrap items-center gap-2 sm:flex">{spinner(true)}{actions}</div><button type="button" className="btn-icon" title="Expand logs" aria-label="Expand logs" disabled={!ready} onClick={() => setExpanded(true)}><Maximize2 size={14} /></button>
    </header>
    {!ready && !pending && <p role="status" className="py-4 text-xs text-ink-300">Log time range is unavailable for this observation.</p>}
    {!expanded && (ready || pending) && <BreakLogViewer expanded={false} />}
    {!expanded && ready && createPortal(<div role="toolbar" aria-label="Break log actions" className="fixed inset-x-0 bottom-0 z-overlay flex flex-wrap items-center justify-center gap-2 border-t border-ink-600 bg-surface-raised px-2 py-2 pb-[max(0.5rem,env(safe-area-inset-bottom))] sm:hidden">{spinner(false)}{actions}</div>, document.body)}
    {expanded && <Modal title="Logs at break" size="2xl" onClose={() => setExpanded(false)} footer={<>{spinner(false)}{actions}</>}><BreakLogViewer expanded /></Modal>}
  </section>;
}
