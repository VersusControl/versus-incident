import { useEffect, useRef, useState } from "react";
import { api, ApiError, type KubernetesPodLogLine } from "@/lib/api";
import { SSELimitError } from "@/lib/sse";

export const POD_LOG_MAX_LINES = 10_000;
export const POD_LOG_MAX_BYTES = 4 << 20;
export const POD_LOG_MAX_IDENTITIES = POD_LOG_MAX_LINES;
type DisplayLine = Pick<KubernetesPodLogLine, "text" | "container" | "timestamp">;
type BufferedLine = DisplayLine & { bytes: number; key: number };
export interface PodLogBuffer {
  lines: BufferedLine[];
  bytes: number;
  dropped: number;
  nextKey: number;
}
const encoder = new TextEncoder();
export function appendPodLogLines(buffer: PodLogBuffer, incoming: KubernetesPodLogLine[]): PodLogBuffer {
  let bytes = buffer.bytes;
  let nextKey = buffer.nextKey;
  const lines = [...buffer.lines];
  for (const line of incoming) {
    const display = { text: line.text, container: line.container, timestamp: line.timestamp };
    const size = encoder.encode(JSON.stringify(display)).byteLength + 64;
    lines.push({ ...display, bytes: size, key: nextKey++ });
    bytes += size;
  }
  let removed = 0;
  while (lines.length - removed > POD_LOG_MAX_LINES || bytes > POD_LOG_MAX_BYTES) {
    bytes -= lines[removed++].bytes;
  }
  return { lines: lines.slice(removed), bytes, dropped: buffer.dropped + removed, nextKey };
}

const emptyBuffer = (): PodLogBuffer => ({ lines: [], bytes: 0, dropped: 0, nextKey: 0 });
type StreamStatus = "connecting" | "live" | "paused" | "reconnecting" | "ended" | "limited" | "error";
const emptySession = (identity: string) => ({
  identity, buffer: emptyBuffer(), cursor: "", received: false, identities: new Set<string>(),
  replayUncertain: false, stopped: undefined as { status: StreamStatus; message: string } | undefined,
});

export function usePodLogStream(options: { namespace: string; pod: string; container: string; previous: boolean; since: number; tail: number; paused: boolean; restart: number }) {
  const { namespace, pod, container, previous, since, tail, paused, restart } = options;
  const identity = JSON.stringify([namespace, pod, container, previous, since, tail, restart]);
  const session = useRef(emptySession(""));
  const [buffer, setBuffer] = useState(emptyBuffer);
  const [status, setStatus] = useState<StreamStatus>("connecting");
  const [message, setMessage] = useState("");
  const [replayUncertain, setReplayUncertain] = useState(false);

  useEffect(() => {
    if (session.current.identity !== identity) {
      session.current = emptySession(identity);
      setBuffer(session.current.buffer);
      setMessage("");
      setReplayUncertain(false);
    }
    if (paused) { setStatus("paused"); return; }
    if (session.current.stopped) {
      setStatus(session.current.stopped.status);
      setMessage(session.current.stopped.message);
      return;
    }
    if (session.current.received && !session.current.cursor) {
      setStatus("error");
      setMessage("Logs cannot resume without a cursor. Reload logs to start a new read; a gap may exist.");
      return;
    }
    let disposed = false;
    const controller = new AbortController();
    let retryTimer: ReturnType<typeof setTimeout> | undefined;
    let flushTimer: ReturnType<typeof setTimeout> | undefined;
    let pending: DisplayLine[] = [];
    let retries = 0;
    const flush = () => {
      clearTimeout(flushTimer);
      flushTimer = undefined;
      if (!pending.length) return;
      session.current.buffer = appendPodLogLines(session.current.buffer, pending);
      pending = [];
      setBuffer(session.current.buffer);
    };
    const run = async () => {
      let terminal = false;
      let retryable = false;
      let failureMessage = "";
      let firstHeartbeatAt: number | undefined;
      setMessage("");
      setStatus(retries ? "reconnecting" : "connecting");
      try {
        await api.kubernetesPodLogStream(namespace, pod, {
          container, previous, timestamps: false,
          ...(session.current.cursor ? { cursor: session.current.cursor } : { since_seconds: since, tail_lines: tail }),
        }, (event) => {
          if (disposed) return;
          if (event.cursor) {
            if (!terminal && event.event === "line" && event.cursor !== session.current.cursor) retries = 0;
            session.current.cursor = event.cursor;
          }
          if (event.replay_uncertain) {
            session.current.replayUncertain = true;
            setReplayUncertain(true);
          }
          if (event.event === "line") {
            if (terminal) return;
            session.current.received = true;
            if (event.timestamp && event.ordinal) {
              const lineIdentity = JSON.stringify([event.container, event.timestamp, event.ordinal]);
              if (session.current.identities.has(lineIdentity)) return;
              session.current.identities.add(lineIdentity);
              if (session.current.identities.size > POD_LOG_MAX_IDENTITIES) {
                session.current.identities.delete(session.current.identities.values().next().value!);
                session.current.replayUncertain = true;
                setReplayUncertain(true);
              }
            } else {
              session.current.replayUncertain = true;
              setReplayUncertain(true);
            }
            pending.push({ text: event.text, container: event.container, timestamp: event.timestamp });
            if (pending.length >= 100) flush();
            else if (!flushTimer) flushTimer = setTimeout(flush, 50);
            if (!terminal) setStatus("live");
          } else if (event.event === "heartbeat" && !terminal) {
            const now = Date.now();
            if (firstHeartbeatAt !== undefined && now - firstHeartbeatAt >= 10_000) retries = 0;
            firstHeartbeatAt ??= now;
            setStatus("live");
          } else if (event.event === "limit" || event.event === "error") {
            terminal = true;
            retryable = event.event === "error" && event.retryable === true;
            const stoppedStatus = event.event === "limit" ? "limited" : "error";
            const stoppedMessage = event.event === "limit"
              ? `Log read reached its ${event.reason === "duration" ? "duration safety limit" : event.reason === "bytes" || event.reason === "byte_limit" ? "byte safety limit" : "safety limit"}. Reload logs to start a new read.`
              : [event.code, event.message, event.action, event.reason].filter(Boolean).join(" ") || "Pod log stream stopped.";
            failureMessage = event.code === "previous_unavailable" && !event.message ? `${stoppedMessage} Previous container logs are unavailable because no previous container instance exists.` : stoppedMessage;
            if (!retryable) session.current.stopped = { status: stoppedStatus, message: failureMessage };
            setStatus(stoppedStatus);
            setMessage(failureMessage);
          } else if (event.event === "end") {
            if (!terminal) {
              terminal = true;
              const stoppedMessage = previous ? "Previous container log read completed." : "Pod log stream ended.";
              session.current.stopped = { status: "ended", message: stoppedMessage };
              setStatus("ended");
              setMessage(stoppedMessage);
            }
          }
        }, controller.signal);
        if (disposed) return;
        if (!terminal) {
          retryable = !previous;
          if (previous) {
            setStatus("error");
            setMessage("Previous container log read was interrupted before completion. Reload logs to retry.");
          }
        }
      } catch (failure) {
        if (disposed) return;
        if (failure instanceof SSELimitError) {
          terminal = true;
          retryable = false;
          const stoppedMessage = `Pod log stream exceeded its ${failure.code} safety limit. Reload logs to start a new read.`;
          session.current.stopped = { status: "limited", message: stoppedMessage };
          setStatus("limited");
          setMessage(stoppedMessage);
        } else {
          retryable = !(failure instanceof ApiError) || failure.status === 429 || failure.status >= 500;
          failureMessage = failure instanceof ApiError ? failure.message : "Pod log connection interrupted.";
          setStatus("error");
          setMessage(failureMessage);
        }
      }
      flush();
      if (disposed || terminal && !retryable) return;
      if (previous || !retryable) return;
      if (session.current.received && !session.current.cursor) {
        setStatus("error");
        setMessage("Connection interrupted without a resume cursor. Reload logs to start a new read; a gap may exist.");
        return;
      }
      if (retries >= 3) {
        const exhaustedMessage = `${failureMessage ? `${failureMessage} ` : ""}Reconnect attempts exhausted. Reload logs to retry from a new read.`;
        session.current.stopped = { status: "error", message: exhaustedMessage };
        setStatus("error");
        setMessage(exhaustedMessage);
        return;
      }
      setStatus("reconnecting");
      setMessage(`${failureMessage ? `${failureMessage} ` : ""}Connection interrupted. ${session.current.cursor ? "Resuming from the last cursor." : "Retrying the log connection."}`);
      retryTimer = setTimeout(() => { retries++; void run(); }, 1000 * 2 ** retries);
    };
    void run();
    return () => {
      disposed = true;
      controller.abort();
      clearTimeout(retryTimer);
      flush();
    };
  }, [identity, namespace, pod, container, previous, since, tail, paused]);
  return { buffer, status, message, replayUncertain };
}