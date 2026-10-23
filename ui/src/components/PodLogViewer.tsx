import { useEffect, useId, useLayoutEffect, useRef, useState } from "react";
import {
  ArrowDownToLine,
  Copy,
  Download,
  Pause,
  Play,
  RefreshCw,
  Clock3,
  Ellipsis,
  Maximize2,
  Search,
  WrapText,
} from "lucide-react";
import type { KubernetesResource, KubernetesWorkloadLogPod } from "@/lib/api";
import { usePodLogStream } from "@/lib/usePodLogStream";
import { createPodPalette, logContainers } from "@/lib/kubernetesResourceDetail";
import { isKnownKubernetesTime } from "@/lib/kubernetesTime";

export function PodLogViewer({
  resource,
  compact = false,
  onOpenFull,
  previousRequest,
}: {
  resource: KubernetesResource;
  compact?: boolean;
  onOpenFull?: () => void;
  previousRequest?: { id: string; container: string; podUID?: string };
}) {
  const workload =
    resource.kind === "Deployment" || resource.kind === "StatefulSet";
  const containers = logContainers(resource);
  const [container, setContainer] = useState(
    workload
      ? ""
      : typeof resource.summary?.main_container === "string" &&
          containers.some(
            (item) => item.name === resource.summary?.main_container,
          )
        ? resource.summary.main_container
        : (containers.find((item) => item.type === "regular")?.name ??
          containers[0]?.name ??
          ""),
  );
  const [podUID, setPodUID] = useState("");
  const [knownPods, setKnownPods] = useState<KubernetesWorkloadLogPod[]>([]);
  const [podColour] = useState(() => createPodPalette());
  const [more, setMore] = useState(false);
  const moreControlsId = useId();
  const [showContainers, setShowContainers] = useState(false);
  const [previous, setPrevious] = useState(false);
  const [paused, setPaused] = useState(false);
  const [follow, setFollow] = useState(true);
  const [timestamps, setTimestamps] = useState(!workload);
  const [wrap, setWrap] = useState(true);
  const [since, setSince] = useState(3600);
  const [filter, setFilter] = useState("");
  const [level, setLevel] = useState("All");
  const [restart, setRestart] = useState(0);
  const [actionStatus, setActionStatus] = useState("");
  const viewport = useRef<HTMLPreElement>(null);
  const nearBottom = useRef(true);
  const scrollAnchor = useRef<{ key: string; offset: number } | null>(null);
  const selectedContainer = container;
  const stream = usePodLogStream({
    namespace: resource.namespace ?? "",
    pod: resource.name,
    uid: resource.uid,
    kind: resource.kind,
    podUID,
    container: selectedContainer,
    previous,
    since: Math.min(since, 86400),
    tail: since >= 86400 ? 5000 : 500,
    paused,
    restart,
  });
  useEffect(() => {
    if (stream.pods !== undefined) setKnownPods(stream.pods);
  }, [stream.pods]);
  useEffect(() => {
    if (!previousRequest) return;
    setPrevious(true);
    setContainer(previousRequest.container);
    setPodUID(previousRequest.podUID ?? "");
    setPaused(false);
  }, [previousRequest]);
  const allContainers = workload
    ? [
        ...new Set(
          knownPods.flatMap((pod) => [
            ...pod.containers,
            ...pod.init_containers,
          ]),
        ),
      ].map((name) => ({
        name,
        type: knownPods.some((pod) => pod.init_containers.includes(name))
          ? "init"
          : "regular",
      }))
    : containers;
  const visible = stream.buffer.lines.filter(
    (line) =>
      line.text.toLowerCase().includes(filter.toLowerCase()) &&
      (level === "All" ||
        line.text.toLowerCase().includes(level.toLowerCase())),
  );
  const levelCounts = {
    Error: stream.buffer.lines.filter((line) => !line.neutral && /\b(error|fatal|panic)\b/i.test(line.text)).length,
    Warn: stream.buffer.lines.filter((line) => !line.neutral && /\bwarn(?:ing)?\b/i.test(line.text)).length,
  };
  const blockingError =
    stream.status === "error" || stream.status === "limited";
  const onlyNeutralOutput = !stream.buffer.lines.some((line) => !line.neutral);
  const shown =
    blockingError && onlyNeutralOutput
      ? []
      : compact
        ? visible.slice(-30)
        : visible;
  const capacityOmissions = new Set(
    knownPods
      .filter(
        (pod) =>
          pod.all_pods_reason === "source_limit" ||
          pod.reason === "source_limit",
      )
      .map((pod) => pod.uid),
  );
  knownPods.forEach((pod) => podColour(pod.name));
  const text = shown
    .map(
      (line) =>
        `${timestamps && line.timestamp ? `${line.timestamp} ` : ""}${workload && line.pod ? `${line.pod}${container === "*" ? `/${line.container}` : ""}  ` : ""}${line.text}`,
    )
    .join("\n");
  const emptyMessage = blockingError
    ? stream.message || "Logs are unavailable. Reopen the resource or retry."
    : filter && stream.buffer.lines.length
      ? "No log lines match this filter."
      : stream.status === "connecting" || stream.status === "reconnecting"
        ? "Connecting to logs..."
        : paused
          ? "Log read paused."
          : "No log lines in the selected time";

  useLayoutEffect(() => {
    const element = viewport.current;
    if (!element) return;
    if (follow && !previous && nearBottom.current)
      element.scrollTop = element.scrollHeight;
    else if (scrollAnchor.current) {
      const anchor = element.querySelector<HTMLElement>(
        `[data-log-key="${scrollAnchor.current.key}"]`,
      );
      if (anchor)
        element.scrollTop +=
          anchor.offsetTop - element.scrollTop - scrollAnchor.current.offset;
    }
  }, [stream.buffer, follow, text, compact, previous]);

  const scrollToBottom = () => {
    nearBottom.current = true;
    scrollAnchor.current = null;
    if (viewport.current)
      viewport.current.scrollTop = viewport.current.scrollHeight;
  };
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text);
      setActionStatus("Scrubbed logs copied.");
    } catch {
      setActionStatus("Clipboard access is unavailable.");
    }
  };
  const download = () => {
    const url = URL.createObjectURL(
      new Blob([text], { type: "text/plain;charset=utf-8" }),
    );
    const link = document.createElement("a");
    link.href = url;
    link.download = `${resource.name}${previous ? "-previous" : ""}-logs.txt`;
    link.click();
    URL.revokeObjectURL(url);
    setActionStatus("Scrubbed log download started.");
  };

  return (
    <section
      aria-label={workload ? "Workload live logs" : "Pod logs"}
      className={`resource-log-viewer min-w-0 space-y-2 ${compact ? "rounded-control border border-ink-700 bg-surface-sunken p-3" : ""}`}
      data-compact={compact}
      data-workload={workload}
    >
      {compact && (
        <div className="log-heading flex items-center gap-2">
          <h3 className="text-xs font-semibold">Live logs</h3>
          {!workload && <span
            className={`pill ${stream.status === "error" ? "text-ink-400" : "text-sev-ok"}`}
          >
            {previous
              ? "Previous"
              : paused
                ? "Paused"
                : stream.status === "error"
                  ? "Unavailable"
                  : stream.status === "ended"
                    ? "Ended"
                    : "Live"}
          </span>}
        </div>
      )}
      <div className="log-toolbar flex flex-wrap items-center gap-2">
        {!compact && !workload && <span className="pill text-sev-ok">{previous ? "Previous" : paused ? "Paused" : stream.status === "error" ? "Unavailable" : stream.status === "ended" ? "Ended" : "Live"}</span>}
        <div className="log-selectors flex w-full items-end gap-2 sm:contents">
          {workload && (
            <label className="log-pod min-w-0 flex-1 text-2xs text-ink-400 sm:max-w-44">
              <span className="sr-only">Pod</span>
              <select
                aria-label="Log pod"
                className="input w-full"
                value={podUID}
                onChange={(event) => setPodUID(event.target.value)}
              >
                <option value="">
                  {capacityOmissions.size > 0
                    ? "All pods (first 20)"
                    : "All pods"}
                </option>
                {knownPods.map((pod) => (
                  <option key={pod.uid} value={pod.uid}>
                    {pod.name}
                    {capacityOmissions.has(pod.uid) ? " (not in all pods)" : ""}
                  </option>
                ))}
              </select>
            </label>
          )}
          <label className="log-container min-w-0 flex-1 text-2xs text-ink-400 sm:max-w-36">
            <span className="sr-only">Container</span>
            <select
              aria-label="Log container"
              className="input w-full"
              value={selectedContainer}
              onChange={(event) => setContainer(event.target.value)}
            >
              {workload && <option value="">{typeof resource.summary?.main_container === "string" ? resource.summary.main_container : "Main container"}</option>}
              {!workload && !containers.length && (
                <option value="">Default container</option>
              )}
              {allContainers.map((item) => (
                <option key={item.name} value={item.name}>
                  {item.type === "init" ? "init:" : ""}
                  {item.name}
                  {item.type === "ephemeral" ? " (ephemeral)" : ""}
                </option>
              ))}
              {workload && <option value="*">All containers</option>}
            </select>
          </label>
          <button
            type="button"
            className="log-more btn sm:hidden"
            aria-expanded={more}
            aria-controls={moreControlsId}
            onClick={() => setMore(!more)}
          >
            More
          </button>
        </div>
        {!workload && (
          <div
            className="log-container-chips flex flex-wrap gap-1"
            role="group"
            aria-label="Log containers"
          >
            {containers.slice(0, showContainers ? undefined : 4).map((item) => (
              <button
                type="button"
                key={item.name}
                className={`btn ${item.name === container ? "text-link" : ""}`}
                aria-pressed={item.name === container}
                onClick={() => setContainer(item.name)}
              >
                {item.type === "init" ? "init:" : ""}
                {item.name}
              </button>
            ))}
            {containers.length > 4 && (
              <button
                type="button"
                className="btn"
                aria-expanded={showContainers}
                onClick={() => setShowContainers(!showContainers)}
              >
                {showContainers ? "Less" : `+${containers.length - 4}`}
              </button>
            )}
          </div>
        )}
        <div
          id={moreControlsId}
          role="group"
          aria-label="Additional log controls"
          data-expanded={more}
          className={`log-additional ${more ? "flex" : "hidden"} flex-wrap items-center gap-x-2 gap-y-2 border-y border-ink-700 py-2 sm:contents`}
        >
          {workload ? <label className="log-toggle log-previous inline-flex items-center gap-2 text-xs text-ink-200">
            <input
              type="checkbox"
              checked={previous}
              onChange={(event) => {
                setPrevious(event.target.checked);
                setPaused(false);
              }}
            />
            Previous logs
          </label> : !compact && <div className="log-instance" role="group" aria-label="Container instance">
            <button type="button" aria-pressed={!previous} onClick={() => { setPrevious(false); setPaused(false); }}>Current</button>
            <button type="button" aria-label="Previous container logs" aria-pressed={previous} onClick={() => { setPrevious(true); setPaused(false); }}>Previous container</button>
          </div>}
          <label className="log-since text-2xs text-ink-400">
            Since{" "}
            <select
              aria-label="Log since seconds"
              className="input"
              value={since}
              onChange={(event) => setSince(Number(event.target.value))}
            >
              {[
                [60, "1m"],
                [300, "5m"],
                [1800, "30m"],
                [3600, "1h"],
                [10800, "3h"],
                [21600, "6h"],
                [86400, "24h"],
                [86401, "All"],
              ].map(([value, label]) => (
                <option key={value} value={value}>
                  {label}
                </option>
              ))}
            </select>
          </label>
          {workload && <label className="log-toggle log-follow inline-flex items-center gap-2 text-xs text-ink-200">
            <input
              type="checkbox"
              checked={follow && !previous}
              disabled={previous}
              onChange={(event) => {
                setFollow(event.target.checked);
                if (event.target.checked) scrollToBottom();
              }}
            />
            Follow
          </label>}
          <div className="log-actions flex shrink-0 gap-1">
          <label className="log-toggle log-timestamps inline-flex items-center gap-2 text-xs text-ink-200">
            <input
              type="checkbox"
              aria-label="Timestamps"
              checked={timestamps}
              onChange={(event) => setTimestamps(event.target.checked)}
            />
            {!workload && <Clock3 size={14} aria-hidden="true" />}<span>Timestamps</span>
          </label>
          <label className="log-toggle log-wrap inline-flex items-center gap-2 text-xs text-ink-200">
            <input
              aria-label="Wrap lines"
              type="checkbox"
              checked={wrap}
              onChange={(event) => setWrap(event.target.checked)}
            />
            {!workload && <WrapText size={14} aria-hidden="true" />}<span>Wrap</span>
          </label>
            <button
              type="button"
              className="log-pause btn-icon"
              aria-label={paused ? "Resume logs" : "Pause logs"}
              title={paused ? "Resume logs" : "Pause logs"}
              onClick={() => setPaused(!paused)}
            >
              {paused ? <Play size={14} /> : <Pause size={14} />}
            </button>
            {!workload && compact && <><button type="button" className="log-search-toggle btn-icon" aria-label="Search log lines" aria-expanded={more} aria-controls={moreControlsId} onClick={() => setMore(!more)}><Search size={14} /></button><button type="button" className="log-expand btn-icon" aria-label="Expand logs" onClick={onOpenFull}><Maximize2 size={14} /></button></>}
            <details className="log-action-menu">
              <summary aria-label="More log actions" title="More log actions"><Ellipsis size={14} /></summary>
              <div>
            <button
              type="button"
              className="log-reload btn-icon"
              aria-label="Reload logs"
              title="Reload logs"
              onClick={() => {
                setRestart((value) => value + 1);
                setPaused(false);
                scrollToBottom();
              }}
            >
              <RefreshCw size={14} />
            </button>
            <button
              type="button"
              className="log-jump btn-icon"
              aria-label="Scroll to latest"
              title="Scroll to latest"
              onClick={() => {
                setFollow(true);
                scrollToBottom();
              }}
            >
              <ArrowDownToLine size={14} />
            </button>
            <button
              type="button"
              className="log-copy btn-icon"
              aria-label="Copy scrubbed logs"
              title="Copy scrubbed logs"
              disabled={!text}
              onClick={() => void copy()}
            >
              <Copy size={14} />
            </button>
            {!workload && <label className="log-toggle log-follow"><input type="checkbox" aria-label="Follow" checked={follow && !previous} disabled={previous} onChange={(event) => { setFollow(event.target.checked); if (event.target.checked) scrollToBottom(); }} />Follow</label>}
              </div>
            </details>
            <button
              type="button"
              className="btn-icon"
              aria-label="Download scrubbed logs"
              title="Download scrubbed logs"
              disabled={!text}
              onClick={download}
            >
              <Download size={14} />
            </button>
          </div>
          <div className="log-filter-group flex w-full flex-wrap items-center gap-2 sm:contents">
            <label className="log-filter-wrap"><Search size={13} aria-hidden="true" />
            <input
              aria-label="Log filter"
              className="log-filter input min-w-0 flex-1"
              placeholder="Filter lines"
              value={filter}
              onChange={(event) => setFilter(event.target.value)}
            />
            </label>
            {!workload &&
              !compact &&
              <div className="log-levels" role="group" aria-label="Log levels">{(["Error", "Warn", "All"] as const).map((value) => (
                <button
                  className={`log-level btn ${value === "Error" ? "log-error-level" : value === "Warn" ? "log-warning-level" : ""}`}
                  type="button"
                  key={value}
                  aria-pressed={level === value}
                  onClick={() => setLevel(value)}
                >
                  {value === "Error" ? `Errors ${levelCounts.Error}` : value === "Warn" ? `Warnings ${levelCounts.Warn}` : "All"}
                </button>
              ))}</div>}
          </div>
        </div>
      </div>
      {previous && !compact && <p className="text-2xs text-ink-400">Previous logs (not followed)</p>}
      <pre
        ref={viewport}
        tabIndex={0}
        aria-label={workload ? "Workload log output" : "Pod log output"}
        className={`log-output relative ${compact ? workload ? "h-32" : "h-52" : "h-[max(24rem,calc(100dvh-260px))]"} min-h-32 overflow-auto rounded-control border border-ink-700 bg-ink-950 p-3 font-mono text-xs leading-5 text-ink-200 ${wrap ? "whitespace-pre-wrap break-words" : "whitespace-pre"}`}
        onScroll={() => {
          const element = viewport.current;
          if (!element) return;
          nearBottom.current =
            element.scrollHeight - element.scrollTop - element.clientHeight <=
            48;
          if (!nearBottom.current) setFollow(false);
          const anchor = Array.from(
            element.querySelectorAll<HTMLElement>("[data-log-key]"),
          ).find(
            (line) => line.offsetTop + line.offsetHeight >= element.scrollTop,
          );
          scrollAnchor.current = anchor
            ? {
                key: anchor.dataset.logKey!,
                offset: anchor.offsetTop - element.scrollTop,
              }
            : null;
        }}
      >
        {shown.length
          ? shown.map((line) => (
              <span
                className={`block ${line.neutral ? line.pod ? "log-source-status text-ink-400" : "log-neutral text-ink-400" : /\b(error|fatal|panic)\b/i.test(line.text) ? "bg-sev-critical/10" : ""}`}
                key={line.key}
                data-log-key={line.key}
              >
                {timestamps && isKnownKubernetesTime(line.timestamp) && <><time dateTime={line.timestamp} title={line.timestamp} className="text-ink-400">{new Date(line.timestamp).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false })}</time>{" "}</>}
                {workload && line.pod && (
                  <>
                    <button
                      type="button"
                      className={`hover:underline ${podColour(line.pod)}`}
                      onClick={() => line.pod_uid && setPodUID(line.pod_uid)}
                    >
                      {line.pod}
                      {container === "*" ? `/${line.container}` : ""}
                    </button>
                    {"  "}
                  </>
                )}
                <span className={line.neutral && !line.pod ? "log-note" : undefined}>{highlightLog(line.neutral && !line.pod ? compactReconnectTime(line.text) : line.text, filter)}</span>
                {"\n"}
              </span>
            ))
          : emptyMessage}
      </pre>
      {stream.status === "error" && shown.length > 0 && (
        <p role="status" className="text-xs text-ink-400">
          {stream.message}
        </p>
      )}
      <div className="log-footer flex flex-wrap items-center gap-2 text-2xs text-ink-400">
        {!workload && (
          <span aria-label="Log connection status">
            {stream.status === "limited" ? "ended" : stream.status}
          </span>
        )}
        {!workload && <span>
          {shown.length} / {stream.buffer.lines.length} retained lines
        </span>}
        {stream.buffer.dropped > 0 && (
          <span>
            {stream.buffer.dropped} evicted · download includes retained lines
            only
          </span>
        )}
        {!follow && (
          <button
            className="text-link"
            type="button"
            onClick={() => {
              setFollow(true);
              scrollToBottom();
            }}
          >
            Jump to latest
          </button>
        )}
        {compact && <button type="button" onClick={onOpenFull} className="log-open-full ml-auto text-xs text-link">Open full logs</button>}
        {!compact && follow && <button type="button" className="btn ml-auto" onClick={scrollToBottom}>Jump to latest</button>}
      </div>
      {actionStatus && (
        <p role="status" className="text-xs text-ink-400">
          {actionStatus}
        </p>
      )}
    </section>
  );
}

function compactReconnectTime(text: string) {
  const match = /^(.*(?:reconnected|resumed after reconnect) at )(\d{1,2}):(\d{2}):(\d{2})(?:\s*([AP]M))?$/i.exec(text);
  if (!match) return text;
  const hour = Number(match[2]);
  const normalized = match[5] ? hour % 12 + (match[5].toUpperCase() === "PM" ? 12 : 0) : hour;
  return `${match[1]}${String(normalized).padStart(2, "0")}:${match[3]}:${match[4]}`;
}

function highlightLog(text: string, filter: string) {
  if (!filter) {
    const level = /\b(INFO|WARN(?:ING)?|ERROR|FATAL|DEBUG|TRACE)\b/i.exec(text);
    if (!level) return text;
    return <>{text.slice(0, level.index)}<span className={/ERROR|FATAL/i.test(level[0]) ? "text-sev-critical" : /WARN/i.test(level[0]) ? "text-sev-warning" : "text-ink-300"}>{level[0]}</span>{text.slice(level.index + level[0].length)}</>;
  }
  if (!filter) return text;
  const parts = [];
  let start = 0;
  let found = text.toLowerCase().indexOf(filter.toLowerCase());
  while (found >= 0) {
    parts.push(
      text.slice(start, found),
      <mark key={found} className="bg-accent/20 text-ink-50">
        {text.slice(found, found + filter.length)}
      </mark>,
    );
    start = found + filter.length;
    found = text.toLowerCase().indexOf(filter.toLowerCase(), start);
  }
  parts.push(text.slice(start));
  return parts;
}
