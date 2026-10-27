import { useId, useLayoutEffect, useRef, useState, type ReactNode } from "react";
import { createPortal } from "react-dom";
import { useNavigate } from "react-router-dom";
import { Activity, ArrowRight, ChevronDown, ChevronLeft, ChevronRight, Clock, History, TriangleAlert, X } from "lucide-react";
import { CoverageMarker } from "@/components/CoverageMarker";
import { ApiError, type KubernetesChange, type KubernetesChangesPage, type KubernetesCrashEvent, type KubernetesIssue, type KubernetesResource } from "@/lib/api";
import { counted, crashDetail, evidencePills, terminationKey, warningSummary } from "@/lib/kubernetesEvidence";
import { useBrokenIssues, useKubernetesCluster, useKubernetesCrashes, useKubernetesQuery } from "@/lib/useKubernetesCluster";
import { KubernetesWarnings } from "./KubernetesWarnings";
import { issueWarnings, timelineWarnings } from "./kubernetesWarningItems";
import "./KubernetesResourceTimeline.css";
import { BreakLogRateStrip, LogWindowBracket } from "./KubernetesBreakLogs";
import { useBreakLogs } from "@/lib/useKubernetesBreakLogs";
import { evidenceLoading, settledSync } from "@/lib/kubernetesPending";
import { KubernetesPendingNote } from "@/components/KubernetesPendingNote";
import { ResourceLogsButton } from "@/components/ResourceLogsButton";

function breakRule(issue: KubernetesIssue) {
  const names: Record<string, string> = { "pod.crash_loop": "CrashLoopBackOff", "pod.image_pull": "ImagePullBackOff", "pod.config_error": "Container config error", "pod.evicted": "Evicted", "pod.unschedulable": "Unschedulable", "pod.failed": "Pod failed", "workload.rollout_stalled": "Rollout stalled (ProgressDeadlineExceeded)", "workload.unavailable": "Pods unavailable", "job.failed": "Job failed", "node.not_ready": "NotReady" };
  return names[issue.rule] ?? issue.rule.replaceAll(".", " ").replaceAll("_", " ");
}

function clock(value?: string) {
  return value && Number.isFinite(Date.parse(value)) ? new Date(value).toLocaleTimeString([], { hour12: false }) : "Time unavailable";
}

function range(from?: string, to?: string) {
  return from && to ? `${clock(from)} – ${clock(to)}` : "Window unavailable";
}

function changeDescription(change: KubernetesChange) {
  if (change.kind === "ConfigMap" || change.kind === "Secret") return "changed, content not shown";
  return change.fields?.map((field) => `${field.from || "∅"} → ${field.to || "∅"}`).join(", ") || change.type.replaceAll("_", " ");
}

export function BreakTimelineLane({ issue, changes, crashes, since, until, onSelectChange, warningsOwned = false, scope }: { issue: KubernetesIssue; changes: KubernetesChange[]; crashes?: KubernetesCrashEvent[]; since: string; until: string; onSelectChange: (change: KubernetesChange) => void; warningsOwned?: boolean; scope?: string }) {
  const byID = new Map(changes.map((change) => [change.id, change]));
  for (const linked of issue.changes ?? []) byID.set(linked.change.id, linked.change);
  return <section aria-label={`Timeline lane ${issue.root.name}`}>
    <ResourceTimelineChart scope={scope} issue={issue} crashes={crashes} changes={[...byID.values()]} since={since} until={until} onSelectChange={onSelectChange} />
    {!warningsOwned && <KubernetesWarnings warnings={issueWarnings(issue)} scope={JSON.stringify([issue.root.uid, issue.root.kind, issue.root.namespace, issue.root.name, issue.rule, since, until])} />}
  </section>;
}

type TimelineMarker = { id: string; at: string; label: string; detail: string; type: "change" | "crash"; change?: KubernetesChange; crash?: KubernetesCrashEvent };
type TimelineResource = KubernetesIssue["root"];
const timelineIdentity = (resource: TimelineResource) => JSON.stringify([resource.kind, resource.namespace ?? "", resource.uid || resource.name]);
const axisClock = (timestamp: number) => new Date(timestamp).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false });

export function ResourceTimelineChart({ issue, issues = [], changes, crashes, since, until, onSelectChange, now = Date.now(), scope }: { issue?: KubernetesIssue; issues?: KubernetesIssue[]; changes: KubernetesChange[]; crashes?: KubernetesCrashEvent[]; since: string; until: string; onSelectChange: (change: KubernetesChange) => void; now?: number; scope?: string }) {
  const logs = useBreakLogs();
  const [selectedIDs, setSelectedIDs] = useState<string[] | null>(null);
  const plotRef = useRef<HTMLDivElement>(null);
  const headingRef = useRef<HTMLHeadingElement>(null);
  const openerRef = useRef<HTMLButtonElement | null>(null);
  const focusRef = useRef<HTMLElement | null>(null);
  const [viewport, setViewport] = useState({ left: 0, width: 0, plotWidth: 0, mobile: false });
  const detailID = useId();
  const start = Date.parse(since);
  const end = Date.parse(until);
  const valid = Number.isFinite(start) && Number.isFinite(end) && end > start;
  const inWindow = (at: number) => valid && Number.isFinite(at) && at >= start && at <= end;
  const position = (at: number) => (at - start) / (end - start) * 100;
  const rows = new Map<string, { resource: TimelineResource; markers: TimelineMarker[] }>();
  const roots = issue ? [issue] : issues;
  for (const root of roots) rows.set(timelineIdentity(root.root), { resource: root.root, markers: [] });
  for (const change of new Map(changes.map((item) => [item.id, item])).values()) {
    if (!inWindow(Date.parse(change.at))) continue;
    const key = timelineIdentity(change);
    if (!rows.has(key)) rows.set(key, { resource: change, markers: [] });
    const description = changeDescription(change);
    const label = change.kind === "ConfigMap" || change.kind === "Secret" ? "changed (content not shown)" : change.fields?.map((field) => `${field.path.split(".").at(-1)} ${field.from || "∅"} → ${field.to || "∅"}`).join(", ") || description;
    rows.get(key)!.markers.push({ id: change.id, at: change.at, label, detail: `${change.kind} ${change.name}: ${description}`, type: "change", change });
  }
  for (const event of crashes ?? []) {
    if (event.type !== "termination" || !inWindow(Date.parse(event.at))) continue;
    const resource = { kind: event.root_kind, namespace: event.namespace, name: event.root_name, uid: event.root_uid };
    const key = timelineIdentity(resource);
    if (!rows.has(key)) rows.set(key, { resource, markers: [] });
    rows.get(key)!.markers.push({ id: event.id, at: event.at, label: `${event.init ? "Init " : ""}${event.container}: ${event.reason || "Terminated"}`, detail: crashObservationDetail(event), type: "crash", crash: event });
  }
  for (const root of crashes === undefined ? roots : []) for (const termination of root.evidence?.terminations ?? []) {
    if (!inWindow(Date.parse(termination.last))) continue;
    const id = JSON.stringify(["termination", timelineIdentity(root.root), terminationKey(termination), termination.last]);
    const markers = rows.get(timelineIdentity(root.root))!.markers;
    if (!markers.some((marker) => marker.id === id)) markers.push({ id, at: termination.last, label: `${termination.init ? "Init " : ""}${termination.reason} ${counted(termination.count, "pod")}`, detail: crashDetail(termination), type: "crash" });
  }
  const broke = Date.parse(issue?.broke_at ?? "");
  const breakVisible = inWindow(broke);
  const step = [60_000, 5 * 60_000, 10 * 60_000, 30 * 60_000, 60 * 60_000, 4 * 60 * 60_000].find((duration) => valid && (end - start) / duration <= 8) ?? (end - start) / 6;
  const firstTick = Math.ceil(start / step) * step;
  const ticks = valid ? Array.from({ length: Math.min(9, Math.max(0, Math.floor((end - firstTick) / step) + 1)) }, (_, index) => firstTick + index * step) : [];
  const windowLeft = Math.max(start, broke - 30 * 60_000);
  const windowRight = Math.min(end, broke + 2 * 60_000);
  const breakRoots = [...new Map(roots.map((root) => [JSON.stringify([timelineIdentity(root.root), root.broke_at]), root])).values()];
  const markersByID = new Map([...rows.values()].flatMap((row) => row.markers).map((marker) => [marker.id, marker]));
  const selected = selectedIDs?.flatMap((id) => markersByID.has(id) ? [markersByID.get(id)!] : []) ?? null;
  const crashPopover = logs?.validIssue && selected?.some((marker) => marker.crash);
  const anchor = openerRef.current?.getBoundingClientRect();
  const resetScope = scope ?? JSON.stringify([issue && timelineIdentity(issue.root), end - start]);
  const previousScope = useRef(resetScope);
  const framedScope = useRef<string | null>(null);
  const markerIDs = JSON.stringify([...markersByID.keys()]);
  const restoreFocus = () => {
    const opener = openerRef.current;
    (opener?.isConnected ? opener : plotRef.current)?.focus({ preventScroll: true });
  };
  const closeDetails = () => { setSelectedIDs(null); restoreFocus(); };
  const openDetails = (group: TimelineMarker[], opener: HTMLButtonElement) => {
    openerRef.current = opener;
    setSelectedIDs(group.map((marker) => marker.id));
  };
  useLayoutEffect(() => {
    if (selectedIDs?.length && headingRef.current) headingRef.current.focus({ preventScroll: true });
  }, [selectedIDs]);
  useLayoutEffect(() => {
    const active = document.activeElement;
    const canRestore = !active || active === document.body || !active.isConnected || Boolean(plotRef.current?.parentElement?.contains(active)) || Boolean(headingRef.current?.closest(".resource-timeline-detail")?.contains(active));
    if (previousScope.current !== resetScope) {
      previousScope.current = resetScope;
      if (selectedIDs) {
        setSelectedIDs(null);
        if (canRestore && focusRef.current?.closest(".resource-timeline-detail")) restoreFocus();
      }
    } else if (selectedIDs && !selected?.length) {
      setSelectedIDs(null);
      if (canRestore) restoreFocus();
    }
    if (canRestore && focusRef.current && !focusRef.current.isConnected) plotRef.current?.focus({ preventScroll: true });
  }, [resetScope, selectedIDs, selected?.length, markerIDs]);
  useLayoutEffect(() => {
    const element = plotRef.current;
    if (!element) return;
    const mobile = window.matchMedia?.("(max-width: 640px)").matches ?? false;
    const frame = framedScope.current !== resetScope;
    framedScope.current = resetScope;
    if (mobile && frame && Number.isFinite(broke)) {
      const plotWidth = element.querySelector<HTMLElement>(".resource-timeline-overlay")?.getBoundingClientRect().width ?? 0;
      element.scrollLeft = Math.max(0, (broke - start) / (end - start) * plotWidth - (element.clientWidth - 132) / 2);
    }
    const measure = () => setViewport({ left: element.scrollLeft, width: element.clientWidth, plotWidth: element.querySelector<HTMLElement>(".resource-timeline-overlay")?.getBoundingClientRect().width ?? 0, mobile: window.matchMedia?.("(max-width: 640px)").matches ?? false });
    measure();
    const observer = typeof ResizeObserver === "undefined" ? undefined : new ResizeObserver(measure);
    observer?.observe(element);
    return () => observer?.disconnect();
  }, [resetScope, broke, start, end]);
  const mobilePlotWidth = viewport.width - 132;
  const visibleBreak = !viewport.mobile || !viewport.width || position(broke) / 100 * 620 >= viewport.left && position(broke) / 100 * 620 <= viewport.left + mobilePlotWidth;
  const closeCaptions = breakVisible && inWindow(now) && Math.abs(position(now) - position(broke)) * 6.2 < 74;
  return <div className="resource-timeline" onFocusCapture={(event) => { focusRef.current = event.target; }} onBlurCapture={(event) => { if (event.relatedTarget && !event.currentTarget.contains(event.relatedTarget)) focusRef.current = null; }} onKeyDown={(event) => { if (event.key === "Escape" && selectedIDs) { event.preventDefault(); closeDetails(); } }}>
    {!valid ? <p role="status" className="p-4 text-xs text-ink-400">Window unavailable.</p> : <>
      <div ref={plotRef} className="resource-timeline-scroll" tabIndex={0} role="region" aria-label="Resource timeline chart" data-since={since} data-until={until} data-now={now} onScroll={(event) => { const left = event.currentTarget.scrollLeft; setViewport((current) => ({ ...current, left })); }}>
        <div className="resource-timeline-grid">
          <div className="resource-timeline-head" style={{ height: (closeCaptions ? 44 : 34) + (logs?.window ? viewport.mobile ? 56 : 24 : 0) }}><div className="resource-timeline-plot">
            {breakVisible && <span className="resource-timeline-break-pill" style={{ left: viewport.mobile && viewport.width ? `clamp(${viewport.left + 44}px, ${position(broke)}%, ${viewport.left + Math.max(44, mobilePlotWidth - 44)}px)` : `clamp(44px, ${position(broke)}%, calc(100% - 44px))` }} title={`Break: ${breakRule(issue!)} ${clock(issue?.broke_at)}`}>{visibleBreak ? `break ${axisClock(broke)}` : position(broke) / 100 * 620 < viewport.left ? "← break" : "break →"}</span>}
            {inWindow(now) && <span className="resource-timeline-now-label" style={{ left: `clamp(14px, ${position(now)}%, calc(100% - 14px))`, top: closeCaptions ? 18 : 0 }}>now</span>}
          </div></div>
          <div className="resource-timeline-body">
            <div className="resource-timeline-overlay" aria-hidden="true">
              {Number.isFinite(broke) && windowRight > windowLeft && <div className="resource-timeline-window" style={{ left: `${position(windowLeft)}%`, width: `${position(windowRight) - position(windowLeft)}%` }}><span>suspect window (break − 30 m … + 2 m)</span></div>}
              {ticks.map((tick) => <span className="resource-timeline-gridline" key={tick} style={{ left: `${position(tick)}%` }} />)}
              {breakRoots.filter((root) => inWindow(Date.parse(root.broke_at ?? ""))).map((root) => <span key={JSON.stringify([timelineIdentity(root.root), root.broke_at])} className="resource-timeline-break-line" data-at={root.broke_at} data-x={position(Date.parse(root.broke_at!))} style={{ left: `${position(Date.parse(root.broke_at!))}%` }} title={`${root.root.name}: break ${clock(root.broke_at)}`} />)}
              {inWindow(now) && <span className="resource-timeline-now-line" style={{ left: `${position(now)}%` }} />}
            </div>
            {[...rows.entries()].sort(([left, leftRow], [right, rightRow]) => Number(roots.some((root) => timelineIdentity(root.root) === right)) - Number(roots.some((root) => timelineIdentity(root.root) === left)) || leftRow.resource.kind.localeCompare(rightRow.resource.kind) || left.localeCompare(right)).map(([key, row]) => {
              const sorted = row.markers.sort((left, right) => Date.parse(left.at) - Date.parse(right.at) || left.id.localeCompare(right.id));
              const groups: TimelineMarker[][] = [];
              for (const marker of sorted) {
                const last = groups.at(-1);
                if (last && last[0].type === marker.type && position(Date.parse(marker.at)) - position(Date.parse(last[0].at)) < 5) last.push(marker);
                else groups.push([marker]);
              }
              return <div className="resource-timeline-row" key={key} data-resource={key}>
                <div className="resource-timeline-label" title={`${row.resource.kind} ${row.resource.name}, ${row.resource.namespace || "Cluster scope"}`}><strong>{row.resource.kind === "HorizontalPodAutoscaler" ? "HPA" : row.resource.kind} {row.resource.name}</strong><span>{row.resource.namespace || "Cluster scope"}</span></div>
                <div className="resource-timeline-plot">
                  {groups.map((group, groupIndex) => {
                    const crash = group[0].type === "crash";
                    const anchor = position(Date.parse(crash ? group.at(-1)!.at : group[0].at));
                    const previous = groups[groupIndex - 1];
                    const next = groups[groupIndex + 1];
                    const previousAnchor = previous ? position(Date.parse(previous.at(-1)!.at)) : 0;
                    const nextAnchor = next ? position(Date.parse(next[0].at)) : 100;
                    const budget = crash ? (next?.[0].type === "change" ? (nextAnchor - anchor) / 2 : nextAnchor - anchor) : anchor - (previous?.[0].type === "crash" ? (previousAnchor + anchor) / 2 : previousAnchor);
                    return <div key={group[0].id}>
                      {group.map((marker, index) => <button key={marker.id} type="button" data-event-id={marker.id} data-at={marker.at} data-x={position(Date.parse(marker.at))} data-marker={marker.type} className={`resource-timeline-marker resource-timeline-${marker.type}`} style={{ left: `${position(Date.parse(marker.at))}%`, top: `${group.length >= 3 ? Math.min(index, 2) * 9 : 10 + index * 7}px`, display: index > 2 ? "none" : undefined }} aria-label={`${marker.detail} at ${clock(marker.at)}`} title={`${marker.detail} at ${clock(marker.at)}`} aria-expanded={selected?.some((item) => item.id === marker.id) ?? false} aria-controls={selected?.length ? detailID : undefined} onClick={(event) => openDetails(group, event.currentTarget)}><span aria-hidden="true">{marker.type === "crash" ? "×" : ""}</span></button>)}
                      <button type="button" className={`resource-timeline-event-label ${crash ? "resource-timeline-crash-label text-sev-critical" : group[0].change?.kind === "ConfigMap" || group[0].change?.kind === "Secret" ? "resource-timeline-config-label text-ink-200" : "text-ink-200"}`} style={{ left: `${anchor}%`, maxWidth: `min(220px, max(0px, ${budget}% - 18px))` }} onClick={(event) => openDetails(group, event.currentTarget)} title={group.map((marker) => marker.detail).join("; ")} aria-label={group.length > 1 ? `${group.length} events: ${group.map((marker) => marker.detail).join("; ")}` : `Show event details: ${group[0].label}`}>{crash ? group.map((marker) => marker.label).join(", ") : <>{group[0].label}{group.length > 1 && ` +${group.length - 1}`}</>}</button>
                    </div>;
                  })}
                </div>
              </div>;
            })}
            {rows.size === 0 && <p className="p-4 text-xs text-ink-400">No recorded markers in this window.</p>}
            <BreakLogRateStrip since={since} until={until} />
            <LogWindowBracket since={since} until={until} captionViewport={viewport.mobile && viewport.width ? { left: viewport.left, width: Math.min(mobilePlotWidth, Math.max(0, viewport.plotWidth - viewport.left)) } : undefined} />
          </div>
          <div className="resource-timeline-axis"><div className="resource-timeline-plot">{ticks.map((tick) => <time key={tick} dateTime={new Date(tick).toISOString()} style={{ left: `${position(tick)}%` }}>{axisClock(tick)}</time>)}</div></div>
        </div>
      </div>
      <div className="resource-timeline-legend" aria-label="Timeline legend"><span><i className="resource-timeline-dot" />change</span><span><i className="resource-timeline-break-key" />break</span><span><i className="resource-timeline-crash-key">×</i>crash (termination)</span><span><i className="resource-timeline-now-key" />now</span><span><i className="resource-timeline-window-key" />suspect window</span></div>
      {crashes === undefined && roots.some((root) => root.evidence?.partial) && <p role="status" className="flex items-center gap-1 px-3 text-xs text-sev-warning"><TriangleAlert size={12} aria-hidden="true" />Partial crash evidence</p>}
      {breakRoots.map((root) => <span key={JSON.stringify([timelineIdentity(root.root), root.broke_at])} className="sr-only">Break: {breakRule(root)}{root.broke_at && <time dateTime={root.broke_at}>{clock(root.broke_at)}</time>}</span>)}
      {!!selected?.length && !crashPopover && <section id={detailID} aria-label="Timeline event details" className="resource-timeline-detail"><button type="button" className="btn-icon float-right" aria-label="Close event details" title="Close event details" onClick={closeDetails}><X size={14} aria-hidden="true" /></button><h3 ref={headingRef} tabIndex={-1} className="sr-only">Timeline event details</h3>{selected.map((marker) => <div key={marker.id} className="mb-2 break-words"><p>{marker.detail}</p><time dateTime={marker.at} className="font-mono text-ink-400">{clock(marker.at)}</time>{marker.change && <button type="button" className="ml-3 text-link" onClick={() => marker.change && onSelectChange(marker.change)}>Open resource</button>}</div>)}</section>}
      {crashPopover && createPortal(<section id={detailID} aria-label="Timeline event details" className="resource-timeline-detail fixed z-overlay !mt-0 w-80 max-w-[calc(100vw-1.5rem)] shadow-overlay" style={{ left: Math.max(12, Math.min(anchor?.left ?? 12, window.innerWidth - 332)), top: Math.max(12, Math.min((anchor?.bottom ?? 0) + 8, window.innerHeight - 260)) }}><button type="button" className="btn-icon float-right" aria-label="Close event details" title="Close event details" onClick={closeDetails}><X size={14} /></button><h3 ref={headingRef} tabIndex={-1} className="sr-only">Timeline event details</h3>{selected?.map((marker) => marker.crash && <section aria-label="Crash log actions" key={marker.id} className="mb-3 space-y-2 text-xs"><p className="font-semibold text-ink-50">{marker.crash.container} {marker.crash.reason || "Terminated"}, exit {marker.crash.exit_code}</p><span className="pill max-w-full break-all whitespace-normal font-mono">{marker.crash.pod}</span><time dateTime={marker.at} className="block font-mono text-ink-300">{clock(marker.at)}</time><p className="text-ink-300">Previous container logs end at this crash</p><div className="flex flex-wrap gap-3"><button type="button" className="btn btn-primary" onClick={() => { logs.beforeCrash(marker.crash!); closeDetails(); }}>Logs before crash</button><button type="button" className="text-link" onClick={() => logs.openPod(marker.crash!.pod, marker.crash!.pod_uid, undefined, marker.crash!.namespace)}>Open pod</button></div></section>)}</section>, document.body)}
    </>}
  </div>;
}

function crashObservationDetail(event: KubernetesCrashEvent) {
  const status = event.type === "termination"
    ? `${event.reason || "Terminated"}, exit ${event.exit_code}${event.signal ? `, signal ${event.signal}` : ""}${event.exit_code === 137 ? ", exit hint: 137 conventionally means 128 + 9; not proof of SIGKILL" : ""}, ${event.termination_slot || "unknown"} termination`
    : event.waiting_reason || "Container status observed";
  return `${event.cluster} / ${event.namespace} / ${event.root_kind} ${event.root_name} / Pod ${event.pod} / ${event.init ? "Init " : ""}container ${event.container}: ${status}; ${event.restart_count} lifetime restarts`;
}

const journalTime = (value: string) => Number.isFinite(Date.parse(value)) && !value.startsWith("0001-") ? new Date(value).toLocaleString() : "unavailable";

export function CrashJournal({ journal, reasons = [] }: { journal: ReturnType<typeof useKubernetesCrashes>; reasons?: ReactNode[] }) {
  const page = journal.isError ? undefined : journal.data;
  const podCoverage = page?.coverage?.Pod;
  const windowLimited = page?.limitations?.some((limitation) =>
    ["journal_not_started_for_full_window", "outside_retention", "journal_commit_failed"].includes(limitation),
  );
  // Baseline polling caveats apply to every page; ambiguity needs an actual observation.
  const ambiguousEvidence = page?.items.some((event) =>
    event.ancestry_partial || event.type === "termination" && event.time_source !== "finished_at",
  );
  const incomplete = page && (!podCoverage || !Number.isFinite(Date.parse(podCoverage.from)) || !Number.isFinite(Date.parse(podCoverage.to)) || Date.parse(podCoverage.from) > Date.parse(page.since) || Date.parse(podCoverage.to) < Date.parse(page.until) || page.sync.state !== "ready" || page.sync.partial || page.capture.mode !== "continuous" || page.capture.behind || page.gaps?.length || page.capture_gaps?.length || windowLimited || ambiguousEvidence);
  return <section aria-label="Captured crash evidence" className="min-w-0 border-t border-ink-700 text-xs">
    <header className="flex flex-wrap items-center gap-x-3 gap-y-1 px-4 py-3"><h3 className="font-semibold text-ink-100">Captured crash evidence</h3>{page && <span className="text-ink-300">{page.crash_count ?? page.items.filter((event) => event.type === "termination").length} crashes</span>}<CoverageMarker reasons={[...reasons, ...(incomplete ? ["Crash capture does not cover the whole range"] : [])]} /></header>
    {journal.isPending && <p role="status" className="px-4 pb-3">Loading captured crash evidence...</p>}
    {journal.isError && <p role="alert" className="px-4 pb-3 text-sev-warning">Crash journal unavailable.<button type="button" className="ml-2 text-link underline" onClick={() => void journal.refetch()}>Retry crash journal</button></p>}
    {page && <>
      <details className="px-4 pb-3 text-ink-400 [overflow-wrap:anywhere]"><summary className="cursor-pointer text-link">Capture details</summary>
      <div className="space-y-1 pt-2" aria-label="Crash capture coverage">
        <p>Window: {journalTime(page.since)} – {journalTime(page.until)}</p>
        <p>Capture started: {journalTime(page.started_at)}</p>
      </div></details>
      {!page.items.length && <p role="status" className="px-4 pb-3 text-ink-300">No captured observations on this page.</p>}
      <ul className="divide-y divide-ink-700" aria-label="Crash observations">{page.items.map((event) => <li key={event.id} className="min-w-0 px-4 py-2 [overflow-wrap:anywhere]">
        <details><summary className="cursor-pointer py-1 text-ink-200"><span className="font-medium">{event.type === "termination" ? event.reason || "Terminated" : event.waiting_reason || "Status observation"}</span>{" - "}{event.namespace}/{event.pod}{" / "}{event.init ? "Init " : ""}{event.container}{" / "}Observed: <time dateTime={event.observed_at}>{journalTime(event.observed_at)}</time>{event.time_source === "finished_at" && (Date.parse(event.at) < Date.parse(page.since) || Date.parse(event.at) > Date.parse(page.until)) && <span className="ml-2 text-sev-warning">Reported finish outside observed window</span>}</summary>
          <div className="space-y-1 py-2 text-ink-300"><p>{crashObservationDetail(event)}</p><p>Root UID: {event.root_uid}; Pod UID: {event.pod_uid}</p><p>Event time: <time dateTime={event.at}>{journalTime(event.at)}</time> ({event.time_source === "finished_at" ? "reported finish time" : "observation time; finish time unknown"})</p><p>Observed: <time dateTime={event.observed_at}>{journalTime(event.observed_at)}</time></p>{event.ancestry_partial && <p className="text-sev-warning">Capture-time ancestry is incomplete.</p>}<p className="text-ink-400">Observation ID: {event.id}</p></div>
        </details>
      </li>)}</ul>
    </>}
    <nav aria-label="Crash journal pages" className="flex items-center justify-between gap-2 border-t border-ink-700 px-4 py-2 text-ink-300"><span>Page {journal.paging.page}</span><div className="flex gap-2"><button type="button" className="btn" aria-label="Previous crash page" disabled={!journal.paging.canPrevious || journal.isFetching} onClick={journal.paging.previousPage}><ChevronLeft size={14} aria-hidden="true" />Previous</button><button type="button" className="btn" aria-label="Next crash page" disabled={journal.isFetching || !page?.next || page.next === journal.paging.cursor} onClick={() => journal.paging.nextPage(page?.next)}>Next<ChevronRight size={14} aria-hidden="true" /></button></div></nav>
  </section>;
}

function lead(seconds: number) {
  const duration = Math.abs(seconds) >= 60 ? `${Math.round(Math.abs(seconds) / 60)} m` : `${Math.abs(seconds)} s`;
  return seconds === 0 ? "at break" : `${duration} ${seconds > 0 ? "before" : "after"}`;
}

export function BreakRow({ issue, resource, onSelectResource, collapsed = false, clusterId, warningScope = "", sourceSync, sourceFailures, warningsOwned = false }: { issue: KubernetesIssue; resource: KubernetesResource | null; onSelectResource: (resource: KubernetesResource, drawerTab?: "timeline" | "logs") => void; collapsed?: boolean; clusterId?: string; warningScope?: string; sourceSync?: KubernetesChangesPage["sync"]; sourceFailures?: readonly unknown[] | null; warningsOwned?: boolean }) {
  const { client, cluster } = useKubernetesCluster();
  const navigate = useNavigate();
  const [expanded, setExpanded] = useState(!collapsed);
  const [evidenceScope, setEvidenceScope] = useState<string | null>(null);
  const enrichment = useKubernetesQuery({ queryKey: ["kubernetes-issues", "root", issue.root.namespace ?? "", issue.root.uid], queryFn: () => client.kubernetesIssues({ root_uid: issue.root.uid, include: "changes", namespace: issue.root.namespace }), enabled: collapsed && expanded && Boolean(issue.root.uid), staleTime: 30_000, retry: false, refetchInterval: (query) => query.state.data?.sync.pending?.length ? 5000 : false });
  const denied = enrichment.error instanceof ApiError && [401, 403, 404].includes(enrichment.error.status);
  const detail = collapsed ? denied ? undefined : enrichment.data?.items.find((item) => item.root.uid === issue.root.uid && item.rule === issue.rule) ?? enrichment.data?.items.find((item) => item.root.uid === issue.root.uid) : issue;
  const changes = [...(detail?.changes ?? [])].sort((left, right) => Date.parse(right.change.at) - Date.parse(left.change.at));
  const timeline = () => {
    const query = new URLSearchParams(window.location.search);
    query.set("view", "timeline");
    query.delete("logs_from");
    query.delete("logs_to");
    query.delete("logs_mode");
    query.delete("logs_pod");
    query.delete("logs_container");
    query.delete("r");
    query.delete("tab");
    if (issue.root.uid) query.set("root_uid", issue.root.uid);
    if (issue.root.namespace) query.set("namespace", issue.root.namespace);
    else query.delete("namespace");
    if (issue.broke_at) query.set("center", issue.broke_at);
    else query.delete("center");
    navigate(`/agent/kubernetes?${query}`);
  };
  const investigate = () => {
    if (!resource) return;
    const query = new URLSearchParams({ provider: "kubernetes", cluster: cluster ?? clusterId ?? changes[0]?.change.cluster ?? "", resource_id: resource.resource_id, name: resource.name, include_changes: "true" });
    if (resource.namespace) query.set("namespace", resource.namespace);
    if (issue.root.uid) query.set("uid", issue.root.uid);
    navigate(`/agent/chat?${query}`);
  };
  const id = useId();
  const evidenceID = useId();
  const scope = JSON.stringify([cluster ?? clusterId, issue.root.uid, issue.root.kind, issue.root.namespace, issue.root.name, issue.rule, warningScope]);
  if (evidenceScope !== null && (evidenceScope !== scope || denied)) setEvidenceScope(null);
  const evidence = denied ? undefined : detail?.evidence ?? issue.evidence;
  const pills = evidence ? evidencePills(evidence) : [];
  const allEvidence = expanded && evidenceScope === scope;
  const warnings = issueWarnings(denied ? issue : detail ?? issue);
  if (evidence?.partial) warnings.push({ id: "crash-capture", content: "Some crash evidence could not be captured" });
  if (denied) warnings.push({ id: "root-history-unavailable", content: "Root history unavailable" });
  const settledSource = settledSync(sourceSync, sourceFailures);
  const sync = settledSync(collapsed ? enrichment.data?.sync : sourceSync, collapsed ? enrichment.data?.partial_failures : undefined);
  // Evidence kinds still loading leave history incomplete; that is not a
  // coverage failure, so say so instead of hedging about available evidence.
  const loadingEvidence = !denied && evidenceLoading(detail?.pending, collapsed ? enrichment.data?.sync.pending : undefined);
  const complete = settledSource?.state === "ready" && !settledSource.partial && sync?.state === "ready" && !sync.partial && !enrichment.isError && !enrichment.data?.truncated && !enrichment.data?.partial_failures?.length;
  const more = Math.max(0, changes.length - 1) + (detail?.more_changes ?? 0);
  const rowButton = "btn h-8 w-8 shrink-0 justify-center px-0 py-0 sm:w-auto sm:px-2.5";
  return <article aria-label={`${issue.root.kind} ${issue.root.name}`} className={`relative min-w-0 border-l-2 px-3 py-1 sm:px-4 sm:py-3 ${issue.severity === "critical" ? "border-l-sev-critical-solid" : "border-l-sev-warn-solid"}`}>
    <div data-issue-row className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1 text-xs lg:flex-nowrap">
      {collapsed && <button type="button" aria-expanded={expanded} aria-controls={id} aria-label={`${expanded ? "Collapse" : "Expand"} ${issue.root.name}`} title={`${expanded ? "Collapse" : "Expand"} issue`} className="btn-icon shrink-0" onClick={() => { setExpanded((value) => !value); setEvidenceScope(null); }}>{expanded ? <ChevronDown size={14} aria-hidden="true" /> : <ChevronRight size={14} aria-hidden="true" />}</button>}
      <div className="flex min-w-0 flex-1 flex-wrap items-center gap-x-1 gap-y-1 sm:gap-x-2">
        <span className={`pill ${issue.severity === "critical" ? "pill-bad" : issue.severity === "warning" ? "pill-warn" : "pill-accent"}`}>{issue.severity}</span>
        <span className="pill text-2xs text-ink-300">{issue.root.kind}</span>
        <button type="button" data-issue-subject disabled={!resource} onClick={() => resource && onSelectResource(resource)} className="min-w-0 max-w-full truncate text-left text-xs font-semibold text-link" title={`${issue.root.namespace ? `${issue.root.namespace}/` : ""}${issue.root.name}`} aria-label={`Open resource ${issue.root.name}`}>{issue.root.namespace ? `${issue.root.namespace}/` : ""}{issue.root.name}</button>
      </div>
      <div className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1 text-ink-300"><span title={breakRule(issue)} aria-label={breakRule(issue)}>{issue.rule === "workload.rollout_stalled" ? <><span className="sm:hidden">Rollout stalled</span><span className="hidden sm:inline">{breakRule(issue)}</span></> : breakRule(issue)}</span><span>{issue.count}{issue.of ? ` of ${issue.of}` : ""} {issue.broken ? `${issue.root.kind === "Node" ? "node" : "pod"}${(issue.of || issue.count) === 1 ? "" : "s"} affected` : "affected"}</span><span className="inline-flex items-center gap-1 whitespace-nowrap font-mono text-ink-400"><Clock size={12} aria-hidden="true" />{issue.broken ? "Broke" : "First seen"} {clock(issue.broken ? issue.broke_at : issue.first_seen)}</span></div>
      <div className="ml-auto flex shrink-0 items-center gap-1">
        {resource && <ResourceLogsButton resource={resource} onSelect={onSelectResource} label={issue.root.name} />}
        <button type="button" data-row-action className={rowButton} aria-label={`Timeline for ${issue.root.name}`} title="Timeline" onClick={timeline}><History size={14} aria-hidden="true" /><span className="hidden sm:inline">Timeline</span></button>
        <button type="button" data-row-action className={`${rowButton} btn-primary`} aria-label={`Investigate ${issue.root.name}`} title="Investigate" disabled={!resource} onClick={investigate}><Activity size={14} aria-hidden="true" /><span className="hidden sm:inline">Investigate</span></button>
      </div>
    </div>
    <div id={id} hidden={!expanded} className="mt-1 space-y-1 text-xs sm:mt-2 sm:pl-[4.5rem]">
      {expanded && <>
      {collapsed && issue.root.uid && enrichment.isPending && <p role="status" className="text-ink-400">Loading root history…</p>}
      {collapsed && enrichment.isError && <p role="status" className="text-sev-warning">Root history unavailable.<button type="button" className="ml-2 text-link underline" onClick={() => void enrichment.refetch()}>Retry</button></p>}
      {collapsed && !issue.root.uid && <p className="text-ink-400">Root history unavailable.</p>}
      {changes.map((item, index) => <div key={item.change.id} className={`grid min-w-0 grid-cols-[minmax(0,1fr)_auto] items-start gap-x-3 sm:grid-cols-[minmax(0,1fr)_6rem_5rem] ${index > 0 ? "hidden sm:grid" : ""}`}><span className="min-w-0 break-words text-ink-200"><span className="text-ink-400">{item.change.kind} </span>{item.change.name} <span className="font-mono">{changeDescription(item.change)}</span></span><span className="col-span-2 col-start-1 row-start-2 flex flex-wrap items-center gap-x-3 text-ink-400 sm:col-span-1 sm:col-start-2 sm:row-start-1">{lead(item.lead_seconds)}{index === 0 && more > 0 && <button type="button" className="min-h-6 text-link sm:hidden" onClick={timeline}>+{more} more changes</button>}</span><time className="col-start-2 row-start-1 whitespace-nowrap text-right font-mono text-ink-400 sm:col-start-3" dateTime={item.change.at}>{clock(item.change.at)}</time></div>)}
      {(detail?.more_changes ?? 0) > 0 && <button type="button" onClick={timeline} className={`${changes.length ? "hidden sm:block" : ""} text-link`}>+{detail?.more_changes} more changes</button>}
      {loadingEvidence && <KubernetesPendingNote label="Evidence loading" />}
      {detail && changes.length === 0 && !loadingEvidence && <p className="text-ink-400">{complete && detail.history === "complete" && !detail.gaps?.length && !detail.partial ? detail.untracked?.length ? "No recorded change in tracked resources." : "No change on it, its config or its nodes" : "No recorded change in available evidence"}</p>}
      {collapsed && issue.examples?.length ? <p className="text-ink-400">Examples: {issue.examples.map((item) => item.name).join(", ")}</p> : null}
      </>}
    </div>
    {evidence && <div className={`mt-1 flex min-w-0 flex-wrap items-center gap-1.5 text-xs ${collapsed ? "pl-7" : "sm:pl-[4.5rem]"} ${expanded ? "" : "sm:hidden"}`} aria-label="Crash evidence">
      {pills.slice(0, 3).map((pill) => <span className={`pill pill-${pill.tone} min-w-0 max-w-full whitespace-normal [overflow-wrap:anywhere]`} key={pill.key}>{pill.text}</span>)}
      <div id={evidenceID} className={allEvidence ? "contents" : "hidden sm:contents"}>{pills.slice(3).map((pill) => <span className={`pill pill-${pill.tone} min-w-0 max-w-full whitespace-normal [overflow-wrap:anywhere]`} key={pill.key}>{pill.text}</span>)}</div>
      {pills.length > 3 && <button type="button" className="inline-flex min-h-8 items-center gap-1 text-link sm:hidden" aria-expanded={allEvidence} aria-controls={evidenceID} aria-label={allEvidence ? `Show less evidence for ${issue.root.name}` : `+${pills.length - 3} more evidence for ${issue.root.name}`} onClick={() => { setExpanded(true); setEvidenceScope(allEvidence ? null : scope); }}><ChevronDown size={14} aria-hidden="true" className={allEvidence ? "rotate-180" : ""} />{allEvidence ? "Show less" : `+${pills.length - 3} more`}</button>}
      {evidence.restarts !== undefined && <span className="text-ink-400">{counted(evidence.restarts, "lifetime restart")}</span>}
      <span className="min-w-0 [overflow-wrap:anywhere] text-ink-400">{warningSummary(evidence)}</span>
      {expanded && <span className="ml-auto hidden whitespace-nowrap text-right font-mono text-ink-400 sm:inline">{range(evidence.from, evidence.to)}</span>}
    </div>}
    <div className={`flex flex-wrap items-center justify-between gap-x-2 text-xs ${warnings.length || expanded && detail?.evidence ? "mt-1" : ""} ${warnings.length ? "" : "sm:hidden"} ${collapsed ? "pl-7" : "sm:pl-[4.5rem]"}`}>{!collapsed && !warningsOwned && <KubernetesWarnings warnings={warnings} scope={JSON.stringify([cluster ?? clusterId, issue.root.uid, issue.root.kind, issue.root.namespace, issue.root.name, issue.rule, warningScope, denied])} />}{expanded && detail?.evidence && <span className="ml-auto whitespace-nowrap text-right font-mono text-ink-400 sm:hidden">{range(detail.evidence.from, detail.evidence.to)}</span>}</div>
  </article>;
}

export function TimelineCard({ changes, windowEnd, clusterId, onSelectView, onSelectResource, resourceFor, warningsOwned = false }: { changes?: KubernetesChangesPage; windowEnd: number; clusterId: string; onSelectView: (view: "issues" | "timeline") => void; onSelectResource: (resource: KubernetesResource) => void; resourceFor: (issue: KubernetesIssue) => KubernetesResource | null; warningsOwned?: boolean }) {
  const issues = useBrokenIssues();
  const blocked = issues.error instanceof ApiError && [401, 403].includes(issues.error.status);
  const data = blocked || !issues.data ? undefined : { ...issues.data, sync: settledSync(issues.data.sync, issues.data.partial_failures) ?? issues.data.sync };
  const warnings = timelineWarnings(data, changes);
  if (data && (issues.isError || data.sync.state === "stale")) warnings.push({ id: "break-stale", content: "Break evidence is stale." });
  const buckets = Array.from({ length: 12 }, () => 0);
  for (const change of changes?.items ?? []) {
    const offset = windowEnd - Date.parse(change.at);
    const index = 11 - Math.floor(offset / 300_000);
    if (offset >= 0 && index >= 0 && index < 12) buckets[index]++;
  }
  const peak = Math.max(1, ...buckets);
  return <section aria-label="Break timeline" className="card min-w-0 overflow-hidden">
    <header className="card-header !py-2 sm:!py-3"><h2 className="card-title flex items-center gap-2"><History size={15} aria-hidden="true" />Timeline</h2><button type="button" className="flex items-center gap-2 text-xs text-link" onClick={() => onSelectView("timeline")}>Full timeline<ArrowRight size={13} aria-hidden="true" /></button></header>
    {issues.isPending && <div role="status" aria-label="Loading break timeline" className="space-y-2 p-4"><div className="sk h-14" /><div className="sk h-20" /></div>}
    {issues.isError && <p role="status" className="p-4 text-xs text-ink-400">Break timeline unavailable.{data && " Showing earlier evidence."}<button type="button" className="ml-2 text-link underline" onClick={() => void issues.refetch()}>Retry</button></p>}
    {data && <><div className="grid grid-cols-2 gap-2 border-b border-ink-700 px-2 py-1 sm:p-3 [&>button]:py-1.5 sm:[&>button]:py-2 [&_strong]:leading-none sm:[&_strong]:leading-7 [&_button>span:nth-child(2)]:mt-0 sm:[&_button>span:nth-child(2)]:mt-1">
      <button type="button" className="min-w-0 rounded-control border border-ink-500/50 bg-surface-raised/60 px-3 py-2 text-left" onClick={() => onSelectView("issues")}><span className="flex items-center justify-between text-2xs text-ink-400 sm:block">Broken<strong className="text-xl tabular-nums text-ink-50 sm:hidden">{data.broken?.total ?? "Unknown"}</strong></span><span className="mt-1 flex flex-wrap items-center gap-1 sm:gap-2"><strong className="hidden text-xl tabular-nums text-ink-50 sm:inline">{data.broken?.total ?? "Unknown"}</strong>{Object.entries(data.broken?.by_severity ?? {}).filter(([, count]) => count > 0).map(([severity, count]) => <span key={severity} className={`pill ${severity === "critical" ? "pill-bad" : "pill-warn"}`}>{count} {severity}</span>)}</span>{data.broken?.total === 0 && <span className="text-xs text-ink-400">{data.sync.state === "stale" || data.sync.partial || data.truncated || data.partial_failures?.length || issues.isError ? "No broken workloads in available evidence" : "No broken workloads"}</span>}</button>
      <button type="button" className="min-w-0 rounded-control border border-ink-500/50 bg-surface-raised/60 px-3 py-2 text-left" onClick={() => onSelectView("timeline")}><span className="block text-2xs text-ink-400">Changes in 60 min</span><span className="mt-1 flex items-end justify-between gap-2"><strong className="text-xl tabular-nums text-ink-50">{changes ? `${changes.items.length}${changes.next || changes.truncated ? "+" : ""}` : "Unknown"}</strong><span role="img" aria-label="Change activity in twelve five-minute intervals" className="flex h-7 w-1/2 items-end gap-1">{buckets.map((count, index) => <span key={index} className={`min-w-0 flex-1 rounded-t-sm ${count ? "bg-accent" : "bg-ink-600"}`} style={{ height: `${count ? Math.max(12, count / peak * 100) : 8}%` }} />)}</span></span></button>
    </div><div className="divide-y divide-ink-700">{data.items.slice(0, 5).map((issue) => <BreakRow key={issue.root.uid ?? issue.root.name} issue={issue} resource={resourceFor(issue)} onSelectResource={onSelectResource} clusterId={clusterId} sourceSync={issues.isError ? undefined : data.sync} sourceFailures={data.partial_failures} warningsOwned />)}</div>{(data.broken?.total ?? 0) > data.items.length && <button type="button" className="flex w-full items-center gap-2 border-t border-ink-700 px-4 py-2 text-xs text-link" onClick={() => onSelectView("issues")}>{data.broken!.total - data.items.length} more<ArrowRight size={12} aria-hidden="true" />Issues</button>}
    </>}
    {data && !warningsOwned && <div className="px-2"><KubernetesWarnings warnings={warnings} scope={JSON.stringify([clusterId, blocked])} /></div>}
  </section>;
}