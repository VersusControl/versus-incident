import { memo, useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from "react";
import {
  ArrowDownToLine,
  Copy,
  Download,
  Pause,
  Play,
  RefreshCw,
  Clock3,
  Ellipsis,
  Search,
  WrapText,
} from "lucide-react";
import type { KubernetesResource, KubernetesWorkloadLogPod } from "@/lib/api";
import { usePodLogStream } from "@/lib/usePodLogStream";
import { createPodPalette, logContainers } from "@/lib/kubernetesResourceDetail";
import { isKnownKubernetesTime } from "@/lib/kubernetesTime";

// The full tab keeps a bounded window of rows in the document while the buffer
// behind it holds the whole loaded read, so a long follow never grows the DOM.
const FULL_WINDOW = 1_000;
const COMPACT_WINDOW = 30;
// An opening read follows from now, the way kubectl logs -f does.
const OPENING_TAIL = 200;
const EARLIER_WINDOWS = [[900, "15m"], [3600, "1h"], [21600, "6h"], [86400, "24h"]] as const;

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
  const summaryContainers = logContainers(resource);
  const [container, setContainer] = useState(
    workload
      ? ""
      : typeof resource.summary?.main_container === "string" &&
          summaryContainers.some(
            (item) => item.name === resource.summary?.main_container,
          )
        ? resource.summary.main_container
        : (summaryContainers.find((item) => item.type === "regular")?.name ??
          summaryContainers[0]?.name ??
          ""),
  );
  const [podUID, setPodUID] = useState("");
  const [knownPods, setKnownPods] = useState<KubernetesWorkloadLogPod[]>([]);
  const [podColour] = useState(() => createPodPalette());
  const [previous, setPrevious] = useState(false);
  const [paused, setPaused] = useState(false);
  const [follow, setFollow] = useState(true);
  const [timestamps, setTimestamps] = useState(!workload);
  const [wrap, setWrap] = useState(true);
  const [earlierWindow, setEarlierWindow] = useState<number>(3600);
  const [filter, setFilter] = useState("");
  const [level, setLevel] = useState("All");
  const [restart, setRestart] = useState(0);
  const [actionStatus, setActionStatus] = useState("");
  const initialWindow = compact ? COMPACT_WINDOW : FULL_WINDOW;
  const [windowSize, setWindowSize] = useState(initialWindow);
  const [atBottom, setAtBottom] = useState(true);
  const [quiet, setQuiet] = useState(false);
  const viewport = useRef<HTMLPreElement>(null);
  const nearBottom = useRef(true);
  const scrollAnchor = useRef<{ key: string; offset: number } | null>(null);
  // The row the reader is on, held in state so the rendered window can keep it
  // mounted while new lines arrive behind them.
  const [anchorKey, setAnchorKey] = useState<number | undefined>(undefined);  const stream = usePodLogStream({
    namespace: resource.namespace ?? "",
    pod: resource.name,
    uid: resource.uid,
    kind: resource.kind,
    podUID,
    container,
    previous,
    tail: OPENING_TAIL,
    paused,
    restart,
  });
  useEffect(() => {
    if (stream.pods !== undefined) setKnownPods(stream.pods);
  }, [stream.pods]);
  // A Pod opened from a list row arrives with the list projection, which omits
  // the container summary, so its chips would be empty until the describe call
  // lands. The read itself names a container on every line, and a workload's
  // membership event names every container in its Pods, so the chips start from
  // what the stream reports and the summary replaces them once it is known.
  const reportedNames = useMemo(() => {
    const names = new Set<string>();
    knownPods.forEach((pod) => {
      pod.containers.forEach((name) => names.add(name));
      pod.init_containers.forEach((name) => names.add(name));
    });
    stream.buffer.lines.forEach((line) => {
      if (line.container) names.add(line.container);
    });
    return [...names];
  }, [knownPods, stream.buffer]);
  const reportedContainers = useReportedContainers(reportedNames);
  const containers = summaryContainers.length
    ? summaryContainers
    : reportedContainers.map((name) => ({ name, type: "regular" }));
  // A read with no explicit selection opens on the server's default container,
  // so that is the container the chips and the select must show as pressed.
  const openingContainer = useMemo(() => {
    const main = resource.summary?.main_container;
    if (typeof main === "string" && containers.some((item) => item.name === main))
      return main;
    return (
      containers.find((item) => item.type === "regular")?.name ??
      containers[0]?.name ??
      ""
    );
  }, [resource.summary, containers]);
  const shownContainer = workload ? container : container || openingContainer;
  // A row-opened Pod opens its read on the server's default because the list
  // projection carried no container names. Once the describe answer supplies
  // them, the read adopts the container the chips already show — but only while
  // nothing has streamed yet, so no line the reader has seen is thrown away.
  useEffect(() => {
    if (workload || container || stream.received || !summaryContainers.length)
      return;
    if (openingContainer) setContainer(openingContainer);
  }, [workload, container, stream.received, summaryContainers.length, openingContainer]);
  useEffect(() => {
    if (!previousRequest) return;
    setPrevious(true);
    setContainer(previousRequest.container);
    setPodUID(previousRequest.podUID ?? "");
    setPaused(false);
  }, [previousRequest]);
  // A different container, instance or filter is a new reading position, so the
  // window starts over; only a new source restarts the quiet watch.
  useEffect(() => {
    setWindowSize(initialWindow);
  }, [shownContainer, podUID, previous, filter, level, initialWindow]);
  // A fresh read starts before its first line arrives, so it may never claim to
  // be waiting on a container that has already been quiet.
  useEffect(() => {
    setQuiet(false);
  }, [shownContainer, podUID, previous, restart]);
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
  // A rollout can remove or rename the container the reader is on. The selector
  // must never hold a name no option offers, and the live read must stop asking
  // for a container that no longer exists, so the selection falls back to the
  // one the panel opens on — or to the first container still there.
  useEffect(() => {
    if (!container || container === "*") return;
    if (allContainers.some((item) => item.name === container)) return;
    // No container is known yet — a row-opened Pod arrives without a summary —
    // so the selection is not evidence of anything and is left alone.
    if (!allContainers.length) return;
    setContainer(
      allContainers.some((item) => item.name === openingContainer)
        ? openingContainer
        : allContainers.find((item) => item.type === "regular")?.name ?? allContainers[0]?.name ?? "",
    );
  }, [allContainers, container, openingContainer]);
  // Filtering and level counts read the whole loaded window; the window below
  // only decides how many of those rows are in the document.
  const visible = useMemo(
    () =>
      stream.buffer.lines.filter(
        (line) =>
          line.text.toLowerCase().includes(filter.toLowerCase()) &&
          (level === "All" ||
            line.text.toLowerCase().includes(level.toLowerCase())),
      ),
    [stream.buffer, filter, level],
  );
  const levelCounts = useMemo(
    () => ({
      Error: stream.buffer.lines.filter((line) => !line.neutral && /\b(error|fatal|panic)\b/i.test(line.text)).length,
      Warn: stream.buffer.lines.filter((line) => !line.neutral && /\bwarn(?:ing)?\b/i.test(line.text)).length,
    }),
    [stream.buffer],
  );
  const blockingError =
    stream.status === "error" || stream.status === "limited";
  const onlyNeutralOutput = !stream.buffer.lines.some((line) => !line.neutral);
  // While the reader is away from the newest line, a busy read keeps pushing the
  // tail forward and a plain trailing slice trims the row they are reading out of
  // the document, which also strands the scroll compensation that keeps their
  // place. The window then follows the anchored row instead of the tail.
  const shown = useMemo(() => {
    if (blockingError && onlyNeutralOutput) return [];
    if (compact) return visible.slice(-COMPACT_WINDOW);
    const tailStart = Math.max(0, visible.length - windowSize);
    if (atBottom || anchorKey === undefined) return visible.slice(tailStart);
    const anchor = visible.findIndex((line) => line.key === anchorKey);
    if (anchor < 0 || anchor >= tailStart) return visible.slice(tailStart);
    // One row above the anchor keeps an upward nudge on content; the rows below
    // it stay so the view under the anchor does not jump.
    const start = Math.max(0, anchor - 1);
    return visible.slice(start, start + windowSize);
  }, [blockingError, onlyNeutralOutput, compact, visible, windowSize, atBottom, anchorKey]);
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
  // Copy and download cover the loaded window, so a search finds and a download
  // keeps lines that are scrolled out of the rendered window.
  const text = useMemo(
    () =>
      visible
        .map(
          (line) =>
            `${timestamps && line.timestamp ? `${line.timestamp} ` : ""}${workload && line.pod ? `${line.pod}${container === "*" ? `/${line.container}` : ""}  ` : ""}${line.text}`,
        )
        .join("\n"),
    [visible, timestamps, workload, container],
  );
  const reloadLogs = () => {
    setRestart((value) => value + 1);
    setPaused(false);
    scrollToBottom();
  };
  const phase = typeof resource.summary?.phase === "string" ? resource.summary.phase : undefined;
  // A Pod that reached a terminal phase, or a read the stream ended, can never
  // gain lines again: there is nothing left to follow. A read the safety limit
  // stopped is a different state — the container is still running, and only a
  // reload starts a new read — so it is named for what happened instead.
  const limited = stream.status === "limited";
  const stopped =
    stream.status === "ended" ||
    limited ||
    (resource.kind === "Pod" && (phase === "Succeeded" || phase === "Failed"));
  const emptyMessage = blockingError
    ? stream.message || "Logs are unavailable. Reopen the resource or retry."
    : filter && stream.buffer.lines.length
      ? "No log lines match this filter."
      : paused
        ? "Log read paused."
        : stopped
          ? previous
            ? "The previous container read ended. No further lines will arrive."
            : "The log read ended. No further lines will arrive."
          : stream.received || quiet
            ? "No new log lines yet. The container is quiet."
            : "Waiting for the first log line…";

  // A read can be live with heartbeats only; after ten seconds that is a quiet
  // container rather than a read still warming up.
  useEffect(() => {
    if (stream.received || stream.status !== "live") return;
    const timer = setTimeout(() => setQuiet(true), 10_000);
    return () => clearTimeout(timer);
  }, [stream.received, stream.status]);

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
  }, [stream.buffer, follow, text, compact, previous, windowSize]);

  // The anchor is two halves of one position: the ref compensates the scroll
  // offset, and the state keeps the row itself in the rendered window.
  const setAnchor = (anchor: { key: string; offset: number } | null) => {
    scrollAnchor.current = anchor;
    setAnchorKey(anchor ? Number(anchor.key) : undefined);
  };

  const scrollToBottom = () => {
    nearBottom.current = true;
    setAtBottom(true);
    setAnchor(null);
    if (viewport.current)
      viewport.current.scrollTop = viewport.current.scrollHeight;
  };
  // Reaching the top of the window pulls the next older slice out of the
  // buffer; the row under the reader stays where it is.
  const revealEarlier = () => {
    if (compact || visible.length <= windowSize) return;
    const element = viewport.current;
    const first = element?.querySelector<HTMLElement>("[data-log-key]");
    if (element && first)
      setAnchor({
        key: first.dataset.logKey!,
        offset: first.offsetTop - element.scrollTop,
      });
    setWindowSize((value) => value + FULL_WINDOW);
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

  // The container control is the same select in every mode: the workload's
  // all-containers option first, then an option per container, an init: prefix
  // so an init container is never mistaken for a running one.
  const containerOptions = (
    <>
      {workload && <option value="*">All containers</option>}
      {workload && (
        <option value="">
          {typeof resource.summary?.main_container === "string"
            ? resource.summary.main_container
            : "Main container"}
        </option>
      )}
      {!workload && !containers.length && <option value="">Default container</option>}
      {allContainers.map((item) => (
        <option key={item.name} value={item.name}>
          {item.type === "init" ? "init:" : ""}
          {item.name}
          {item.type === "ephemeral" ? " (ephemeral)" : ""}
        </option>
      ))}
    </>
  );

  return (
    <section
      aria-label={workload ? "Workload live logs" : "Pod logs"}
      className={`resource-log-viewer min-w-0 ${compact ? "rounded-control border border-ink-700 bg-surface-sunken p-3" : ""}`}
      data-compact={compact}
      data-workload={workload}
    >
      <div className={compact ? "log-heading" : "log-toolbar"}>
        {compact ? (
          <>
            <h3 className="log-heading-title">Live logs</h3>
            <LogStatus status={stream.status} previous={previous} paused={paused} stopped={stopped} limited={limited} message={stream.message} />
            <label className="log-container">
              <span className="sr-only">Container</span>
              <select
                aria-label="Log container"
                className="input w-full"
                value={shownContainer}
                onChange={(event) => setContainer(event.target.value)}
              >
                {containerOptions}
              </select>
            </label>
          </>
        ) : (
          <>
            <div className="log-primary">
              <LogStatus status={stream.status} previous={previous} paused={paused} stopped={stopped} limited={limited} message={stream.message} />
              {workload && (
                <label className="log-pod">
                  <span className="sr-only">Pod</span>
                  <select
                    aria-label="Log pod"
                    className="input w-full"
                    value={podUID}
                    onChange={(event) => setPodUID(event.target.value)}
                  >
                    <option value="">
                      {capacityOmissions.size > 0 ? "All pods (first 20)" : "All pods"}
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
              <label className="log-container">
                <span className="sr-only">Container</span>
                <select
                  aria-label="Log container"
                  className="input w-full"
                  value={shownContainer}
                  onChange={(event) => setContainer(event.target.value)}
                >
                  {containerOptions}
                </select>
              </label>
              <div className="log-instance" role="group" aria-label="Container instance">
                <button type="button" aria-pressed={!previous} onClick={() => { setPrevious(false); setPaused(false); }}>Current</button>
                <button type="button" aria-label={workload ? "Previous logs" : "Previous container logs"} aria-pressed={previous} onClick={() => { setPrevious(true); setPaused(false); }}>Previous</button>
              </div>
              <div className="log-since" role="group" aria-label="Load earlier logs">
                <label className="log-since-field">
                  <span>Since</span>
                  <select
                    aria-label="Since window"
                    className="input"
                    value={earlierWindow}
                    onChange={(event) => setEarlierWindow(Number(event.target.value))}
                  >
                    {EARLIER_WINDOWS.map(([value, label]) => (
                      <option key={value} value={value}>
                        {label}
                      </option>
                    ))}
                  </select>
                </label>
                <button
                  type="button"
                  className="btn log-load-earlier"
                  aria-label="Load logs since that window"
                  title="Read that window once and add it above the live lines"
                  disabled={stream.earlier === "loading"}
                  onClick={() => void stream.loadEarlier(earlierWindow)}
                >
                  {stream.earlier === "loading" ? "Loading" : "Load"}
                </button>
              </div>
            </div>
            <div className="log-secondary">
              {!workload && (
                <div className="log-levels" role="group" aria-label="Log levels">
                  {(["Error", "Warn", "All"] as const).map((value) => (
                    <button
                      className={`log-level btn ${value === "Error" ? "log-error-level" : value === "Warn" ? "log-warning-level" : ""}`}
                      type="button"
                      key={value}
                      aria-pressed={level === value}
                      onClick={() => setLevel(value)}
                    >
                      {value === "Error" ? `Errors ${levelCounts.Error}` : value === "Warn" ? `Warnings ${levelCounts.Warn}` : "All"}
                    </button>
                  ))}
                </div>
              )}
              <div className="log-filter-group">
                <label className="log-filter-wrap">
                  <Search size={13} aria-hidden="true" />
                  <input
                    aria-label="Log filter"
                    className="log-filter input"
                    placeholder="Filter lines"
                    value={filter}
                    onChange={(event) => setFilter(event.target.value)}
                  />
                </label>
                <div className="log-actions">
                  <label className="log-toggle log-timestamps">
                    <input
                      type="checkbox"
                      aria-label="Timestamps"
                      checked={timestamps}
                      onChange={(event) => setTimestamps(event.target.checked)}
                    />
                    <Clock3 size={14} aria-hidden="true" /><span>Timestamps</span>
                  </label>
                  <label className="log-toggle log-wrap">
                    <input
                      aria-label="Wrap lines"
                      type="checkbox"
                      checked={wrap}
                      onChange={(event) => setWrap(event.target.checked)}
                    />
                    <WrapText size={14} aria-hidden="true" /><span>Wrap</span>
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
                  <details className="log-action-menu">
                    <summary aria-label="More log actions" title="More log actions"><Ellipsis size={14} /></summary>
                    <div>
                      <button
                        type="button"
                        className="log-reload btn-icon"
                        aria-label="Reload logs"
                        title="Reload logs"
                        onClick={reloadLogs}
                      >
                        <RefreshCw size={14} />
                      </button>
                      {/* The same gate as the footer action: there is nowhere to
                          jump at the newest line. */}
                      {!atBottom && <button
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
                      </button>}
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
                    </div>
                  </details>
                  <button
                    type="button"
                    className="btn-icon"
                    aria-label="Download scrubbed logs"
                    title={stream.buffer.dropped > 0 ? `Download includes the newest ${(stream.buffer.limit ?? 10_000).toLocaleString()} lines` : "Download scrubbed logs"}
                    disabled={!text}
                    onClick={download}
                  >
                    <Download size={14} />
                  </button>
                </div>
              </div>
            </div>
          </>
        )}
      </div>

      <pre
        ref={viewport}
        tabIndex={0}
        aria-label={workload ? "Workload log output" : "Pod log output"}
        className={`log-output relative ${compact ? "h-32" : "h-[max(24rem,calc(100dvh-260px))]"} min-w-0 overflow-auto rounded-control border border-ink-700 bg-ink-950 p-3 font-mono text-xs leading-5 text-ink-200 ${wrap ? "whitespace-pre-wrap break-words" : "whitespace-pre"}`}
        onScroll={() => {
          const element = viewport.current;
          if (!element) return;
          const bottom =
            element.scrollHeight - element.scrollTop - element.clientHeight <=
            48;
          nearBottom.current = bottom;
          setAtBottom(bottom);
          // Leaving the newest line stops the follow; coming back to it starts
          // following again, so a reader who scrolls up and then down is never
          // stranded without a way to resume.
          if (!bottom) setFollow(false);
          else setFollow(true);
          // The anchor only matters while reading above the tail; at the bottom
          // the view is pinned to the newest line anyway.
          if (bottom) {
            setAnchor(null);
            return;
          }
          if (element.scrollTop <= 0) revealEarlier();
          const anchor = Array.from(
            element.querySelectorAll<HTMLElement>("[data-log-key]"),
          ).find(
            (line) => line.offsetTop + line.offsetHeight >= element.scrollTop,
          );
          setAnchor(
            anchor
              ? {
                  key: anchor.dataset.logKey!,
                  offset: anchor.offsetTop - element.scrollTop,
                }
              : null,
          );
        }}
      >
        {shown.length
          ? (
              // One wrapper mounts the whole batch off-document and inserts it
              // once; placing thousands of sibling rows one by one is
              // quadratic in React and dominates large log reads.
              <span className="contents" data-log-lines>
                {shown.map((line) => (
                  <LogRow key={line.key} line={line} timestamps={timestamps} workload={workload} allContainers={container === "*"} filter={filter} podColour={podColour} onPod={setPodUID} />
                ))}
              </span>
            )
          : emptyMessage}
      </pre>
      {/* A resume that could not use its own position is a gap in coverage, and
          it is named next to the lines it affects — never as a failed read. */}
      {stream.gap && (
        <p role="status" data-log-gap className="log-gap">
          Gap in the log read: {stream.gap}.
        </p>
      )}
      {blockingError && shown.length > 0 && (
        <p role="status" className="text-xs text-ink-400">
          {stream.message}
        </p>
      )}
      {blockingError && (stream.notice.action || stream.notice.retryable) && (
        <div role="status" data-log-notice className="flex flex-wrap items-center gap-2 text-xs text-ink-300">
          {stream.notice.action && <span className="min-w-0">{stream.notice.action}</span>}
          {stream.notice.retryable && <button type="button" className="btn whitespace-nowrap" onClick={reloadLogs}>Retry logs</button>}
        </div>
      )}
      {stream.earlier === "error" && (
        <p role="status" className="text-2xs text-ink-400">
          Earlier logs are unavailable for that window.
        </p>
      )}
      <div className="log-footer flex flex-wrap items-center gap-2 text-2xs text-ink-400">
        {/* Only while the reader is away from the newest line. */}
        {!atBottom && (
          <button
            className="btn"
            type="button"
            onClick={() => {
              setFollow(true);
              scrollToBottom();
            }}
          >
            Jump to latest
          </button>
        )}
        {/* The compact card has no toolbar, so a read the safety limit stopped
            gets its own Reload here: the message it shows names a control the
            reader must be able to reach. */}
        {compact && limited && <button type="button" className="btn" onClick={reloadLogs}>Reload logs</button>}
        {compact && <button type="button" onClick={onOpenFull} className="log-open-full ml-auto text-xs text-link">Open full logs</button>}
      </div>
      {actionStatus && (
        <p role="status" className="text-xs text-ink-400">
          {actionStatus}
        </p>
      )}
    </section>
  );
}

// Names the read has already reported are kept for the life of the reading
// position: a container that has gone quiet must not lose its chip just because
// its lines aged out of the buffer.
function useReportedContainers(names: string[]): string[] {
  const [reported, setReported] = useState<string[]>([]);
  useEffect(() => {
    setReported((current) => {
      const added = names.filter((name) => !current.includes(name));
      return added.length ? [...current, ...added] : current;
    });
  }, [names]);
  return reported;
}

// toLocaleTimeString builds a new formatter per call, which dominates
// rendering thousands of rows; one shared formatter gives the same text.
let clockFormat: Intl.DateTimeFormat | undefined;
const logClock = () => clockFormat ??= new Intl.DateTimeFormat([], { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false });

// One connection indicator for every mode: the dot carries the animation and
// the word carries the state, so colour is never the only signal. A read the
// safety limit stopped says so: the container is still running, so calling it
// ended would tell the reader the opposite of what happened.
function LogStatus({ status, previous, paused, stopped, limited, message }: { status: string; previous: boolean; paused: boolean; stopped: boolean; limited: boolean; message: string }) {
  const label = previous
    ? "Previous"
    : paused
      ? "Paused"
      : limited
        ? "Stopped"
        : stopped
          ? "Ended"
          : status === "error"
            ? "Unavailable"
            : status === "reconnecting"
              ? "Reconnecting"
              : status === "connecting"
                ? "Connecting"
                : "Live";
  const tone = label === "Live" ? "is-live" : label === "Reconnecting" ? "is-waiting" : label === "Paused" ? "is-paused" : "is-idle";
  return <span aria-label="Log connection status" data-log-status={tone} className="log-status pill" title={limited && message ? message : undefined}>
    <span aria-hidden="true" className="log-dot" />
    {label}
  </span>;
}

type LogRowLine = ReturnType<typeof usePodLogStream>["buffer"]["lines"][number];

// Rows are memoised: status, footer and toolbar updates during a live read
// must not re-render every retained line.
const LogRow = memo(function LogRow({ line, timestamps, workload, allContainers, filter, podColour, onPod }: { line: LogRowLine; timestamps: boolean; workload: boolean; allContainers: boolean; filter: string; podColour: (pod: string) => string; onPod: (uid: string) => void }) {
  const note = line.neutral && !line.pod;
  const content = highlightLog(note ? compactReconnectTime(line.text) : line.text, filter);
  return <span
    className={`block ${line.neutral ? line.pod ? "log-source-status text-ink-400" : "log-neutral text-ink-400" : /\b(error|fatal|panic)\b/i.test(line.text) ? "bg-sev-critical/10" : ""}`}
    data-log-key={line.key}
  >
    {timestamps && isKnownKubernetesTime(line.timestamp) && <><time dateTime={line.timestamp} title={line.timestamp} className="text-ink-400">{logClock().format(new Date(line.timestamp))}</time>{" "}</>}
    {workload && line.pod && <><button type="button" className={`hover:underline ${podColour(line.pod)}`} onClick={() => line.pod_uid && onPod(line.pod_uid)}>{line.pod}{allContainers ? `/${line.container}` : ""}</button>{"  "}</>}
    {note ? <span className="log-note">{content}</span> : content}
    {"\n"}
  </span>;
});

function compactReconnectTime(text: string) {
  const match = /^(.*(?:reconnected|resumed after reconnect) at )(\d{1,2}):(\d{2}):(\d{2})(?:\s*([AP]M))?$/i.exec(text);
  if (!match) return text;
  const hour = Number(match[2]);
  const normalized = match[5] ? hour % 12 + (match[5].toUpperCase() === "PM" ? 12 : 0) : hour;
  return `${match[1]}${String(normalized).padStart(2, "0")}:${match[3]}:${match[4]}`;
}

// Highlighting is pure for a given text and filter, so a live batch only ever
// pays for the lines it adds instead of re-splitting every visible row.
const highlightCache = new Map<string, ReactNode>();
function highlightLog(text: string, filter: string) {
  const cacheKey = `${filter}\u0000${text}`;
  const cached = highlightCache.get(cacheKey);
  if (cached !== undefined) return cached;
  const rendered = buildHighlight(text, filter);
  if (highlightCache.size >= 20_000) highlightCache.clear();
  highlightCache.set(cacheKey, rendered);
  return rendered;
}

function buildHighlight(text: string, filter: string) {
  if (!filter) {
    const level = /\b(INFO|WARN(?:ING)?|ERROR|FATAL|DEBUG|TRACE)\b/i.exec(text);
    if (!level) return text;
    return <>{text.slice(0, level.index)}<span className={/ERROR|FATAL/i.test(level[0]) ? "text-sev-critical" : /WARN/i.test(level[0]) ? "text-sev-warning" : "text-ink-300"}>{level[0]}</span>{text.slice(level.index + level[0].length)}</>;
  }
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
