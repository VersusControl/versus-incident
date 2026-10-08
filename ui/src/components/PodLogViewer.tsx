import { useLayoutEffect, useRef, useState } from "react";
import { ArrowDownToLine, Copy, Download, Pause, Play, RefreshCw } from "lucide-react";
import type { KubernetesResource } from "@/lib/api";
import { usePodLogStream } from "@/lib/usePodLogStream";

function logContainers(resource: KubernetesResource): Array<{ name: string; type: string }> {
  const value = resource.summary?.log_containers;
  if (!Array.isArray(value)) return [];
  return value.filter((item): item is { name: string; type: string } => Boolean(item && typeof item === "object" && typeof item.name === "string" && /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(item.name) && item.name.length <= 63 && ["regular", "init", "ephemeral"].includes(item.type)));
}

export function PodLogViewer({ resource }: { resource: KubernetesResource }) {
  const containers = logContainers(resource);
  const [container, setContainer] = useState(containers[0]?.name ?? "");
  const [previous, setPrevious] = useState(false);
  const [paused, setPaused] = useState(false);
  const [follow, setFollow] = useState(true);
  const [timestamps, setTimestamps] = useState(true);
  const [wrap, setWrap] = useState(true);
  const [since, setSince] = useState(3600);
  const [tail, setTail] = useState(500);
  const [filter, setFilter] = useState("");
  const [restart, setRestart] = useState(0);
  const [actionStatus, setActionStatus] = useState("");
  const viewport = useRef<HTMLPreElement>(null);
  const nearBottom = useRef(true);
  const scrollAnchor = useRef<{ key: string; offset: number } | null>(null);
  const selectedContainer = containers.some((item) => item.name === container) ? container : containers[0]?.name ?? "";
  const stream = usePodLogStream({ namespace: resource.namespace ?? "", pod: resource.name, container: selectedContainer, previous, since, tail, paused, restart });
  const visible = stream.buffer.lines.filter((line) => line.text.includes(filter));
  const text = visible.map((line) => `${timestamps && line.timestamp ? `${line.timestamp} ` : ""}${line.text}`).join("\n");
  const emptyMessage = filter !== "" && stream.buffer.lines.length > 0
    ? "No log lines match this filter."
    : stream.status === "connecting" || stream.status === "reconnecting"
      ? "Connecting to pod logs..."
      : stream.status === "limited"
        ? "Log read reached its safety limit before any lines were received."
        : stream.status === "error"
          ? "No log lines received. See the stream error above."
        : stream.status === "paused"
          ? "Log read paused; no lines received."
          : stream.status === "ended"
            ? previous ? "Previous container log read completed with no lines." : "Log read completed with no lines."
            : "Connected; no log lines received yet.";

  useLayoutEffect(() => {
    const element = viewport.current;
    if (!element) return;
    if (follow && nearBottom.current) element.scrollTop = element.scrollHeight;
    else if (scrollAnchor.current) {
      const anchor = element.querySelector<HTMLElement>(`[data-log-key="${scrollAnchor.current.key}"]`);
      if (anchor) element.scrollTop += anchor.offsetTop - element.scrollTop - scrollAnchor.current.offset;
    }
  }, [stream.buffer, follow, text]);

  const scrollToBottom = () => {
    nearBottom.current = true;
    scrollAnchor.current = null;
    if (viewport.current) viewport.current.scrollTop = viewport.current.scrollHeight;
  };
  const copy = async () => {
    try { await navigator.clipboard.writeText(text); setActionStatus("Scrubbed logs copied."); }
    catch { setActionStatus("Clipboard access is unavailable."); }
  };
  const download = () => {
    const url = URL.createObjectURL(new Blob([text], { type: "text/plain;charset=utf-8" }));
    const link = document.createElement("a");
    link.href = url;
    link.download = `${resource.name}${previous ? "-previous" : ""}-logs.txt`;
    link.click();
    URL.revokeObjectURL(url);
    setActionStatus("Scrubbed log download started.");
  };

  return <section aria-label="Pod logs" className="min-w-0 space-y-3">
    <div className="grid gap-2 sm:grid-cols-2">
      <label className="text-2xs text-ink-400">Container<select aria-label="Log container" className="input mt-1 w-full" value={selectedContainer} onChange={(event) => setContainer(event.target.value)}>
        {!containers.length && <option value="">Default container</option>}
        {containers.map((item) => <option key={item.name} value={item.name}>{item.name} ({item.type})</option>)}
      </select></label>
      <label className="text-2xs text-ink-400">Since<select aria-label="Log since seconds" className="input mt-1 w-full" value={since} onChange={(event) => setSince(Number(event.target.value))}>
        {[60, 300, 900, 3600, 21600, 86400].map((value) => <option key={value} value={value}>{value < 3600 ? `${value / 60} ${value === 60 ? "minute" : "minutes"}` : `${value / 3600} ${value === 3600 ? "hour" : "hours"}`}</option>)}
      </select></label>
      <label className="text-2xs text-ink-400">Tail lines<select aria-label="Log tail lines" className="input mt-1 w-full" value={tail} onChange={(event) => setTail(Number(event.target.value))}>
        {[100, 500, 1000, 2500, 5000].map((value) => <option key={value} value={value}>{value}</option>)}
      </select></label>
      <label className="text-2xs text-ink-400">Literal filter<input aria-label="Log filter" className="input mt-1 w-full" value={filter} onChange={(event) => setFilter(event.target.value)} /></label>
    </div>
    {resource.summary?.log_containers_truncated === true && <p role="status" className="text-xs text-sev-warning">Container list is truncated.</p>}
    <div className="flex flex-wrap items-center gap-x-4 gap-y-2 border-y border-ink-700 py-2">
      <label className="inline-flex items-center gap-2 text-xs text-ink-200"><input type="checkbox" checked={previous} onChange={(event) => { setPrevious(event.target.checked); setPaused(false); }} />Previous container logs</label>
      <label className="inline-flex items-center gap-2 text-xs text-ink-200"><input type="checkbox" checked={follow} onChange={(event) => { setFollow(event.target.checked); if (event.target.checked) scrollToBottom(); }} />Follow</label>
      <label className="inline-flex items-center gap-2 text-xs text-ink-200"><input type="checkbox" checked={timestamps} onChange={(event) => setTimestamps(event.target.checked)} />Timestamps</label>
      <label className="inline-flex items-center gap-2 text-xs text-ink-200"><input type="checkbox" checked={wrap} onChange={(event) => setWrap(event.target.checked)} />Wrap lines</label>
      <div className="flex shrink-0 gap-1">
        <button type="button" className="btn-icon" aria-label={paused ? "Resume logs" : "Pause logs"} title={paused ? "Resume logs" : "Pause logs"} onClick={() => setPaused(!paused)}>{paused ? <Play size={14} /> : <Pause size={14} />}</button>
        <button type="button" className="btn-icon" aria-label="Reload logs" title="Reload logs" onClick={() => { setRestart((value) => value + 1); setPaused(false); scrollToBottom(); }}><RefreshCw size={14} /></button>
        <button type="button" className="btn-icon" aria-label="Scroll logs to bottom" title="Scroll logs to bottom" onClick={scrollToBottom}><ArrowDownToLine size={14} /></button>
        <button type="button" className="btn-icon" aria-label="Copy scrubbed logs" title="Copy scrubbed logs" disabled={!text} onClick={() => void copy()}><Copy size={14} /></button>
        <button type="button" className="btn-icon" aria-label="Download scrubbed logs" title="Download scrubbed logs" disabled={!text} onClick={download}><Download size={14} /></button>
      </div>
    </div>
    <div className="flex flex-wrap items-center gap-2 text-xs text-ink-400"><span role="status" aria-label="Log connection status">{stream.status}</span><span>{visible.length} / {stream.buffer.lines.length} lines</span><span>{Math.ceil(stream.buffer.bytes / 1024)} KiB</span></div>
    {stream.message && <p role={stream.status === "error" ? "alert" : "status"} className="text-xs text-sev-warning">{stream.message}</p>}
    {stream.replayUncertain && <p role="status" className="text-xs text-sev-warning">Log replay is uncertain; gaps or repeated lines may exist. Replay identities may be incomplete or no longer retained.</p>}
    {stream.buffer.dropped > 0 && <p role="status" className="text-xs text-sev-warning">Browser buffer capped; {stream.buffer.dropped} oldest lines dropped.</p>}
    <pre ref={viewport} tabIndex={0} aria-label="Pod log output" className={`relative h-[45vh] min-h-40 max-h-[55vh] overflow-auto rounded-control border border-ink-700 bg-ink-950 p-3 font-mono text-xs leading-5 text-ink-200 ${wrap ? "whitespace-pre-wrap break-words" : "whitespace-pre"}`} onScroll={() => {
      const element = viewport.current;
      if (!element) return;
      nearBottom.current = element.scrollHeight - element.scrollTop - element.clientHeight <= 48;
      const anchor = Array.from(element.querySelectorAll<HTMLElement>("[data-log-key]")).find((line) => line.offsetTop + line.offsetHeight >= element.scrollTop);
      scrollAnchor.current = anchor ? { key: anchor.dataset.logKey!, offset: anchor.offsetTop - element.scrollTop } : null;
    }}>{visible.length ? visible.map((line) => <span className="block" key={line.key} data-log-key={line.key}>{timestamps && line.timestamp ? `${line.timestamp} ` : ""}{line.text}{"\n"}</span>) : emptyMessage}</pre>
    {actionStatus && <p role="status" className="text-xs text-ink-400">{actionStatus}</p>}
  </section>;
}