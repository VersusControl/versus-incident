import { useEffect, useRef, useState } from "react";
import { ApiError, type KubernetesPodLogLine, type KubernetesWorkloadLogEvent, type KubernetesWorkloadLogPod } from "@/lib/api";
import { useKubernetesCluster } from "@/lib/useKubernetesCluster";
import { SSELimitError } from "@/lib/sse";

export const POD_LOG_MAX_LINES = 10_000;
export const POD_LOG_MAX_BYTES = 4 << 20;
export const POD_LOG_MAX_IDENTITIES = POD_LOG_MAX_LINES;
type DisplayLine = Pick<KubernetesPodLogLine, "text" | "container" | "timestamp"> & { pod?: string; pod_uid?: string; source_id?: string; neutral?: boolean };
type BufferedLine = DisplayLine & { bytes: number; key: number };
export interface PodLogBuffer {
  lines: BufferedLine[];
  bytes: number;
  dropped: number;
  nextKey: number;
}
const encoder = new TextEncoder();
export function appendPodLogLines(buffer: PodLogBuffer, incoming: DisplayLine[]): PodLogBuffer {
  let bytes = buffer.bytes;
  let nextKey = buffer.nextKey;
  const lines = [...buffer.lines];
  for (const line of incoming) {
    const display = { text: line.text, container: line.container, timestamp: line.timestamp, ...(line.pod ? { pod: line.pod, pod_uid: line.pod_uid, source_id: line.source_id } : {}), ...(line.neutral ? { neutral: true } : {}) };
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
  identity, buffer: emptyBuffer(), cursor: "", received: false, identities: new Set<string>(), paused: false,
  admissionRetries: 0, admissionStartedAt: undefined as number | undefined,
  replayUncertain: false, stopped: undefined as { status: StreamStatus; message: string } | undefined,
});

export interface ResourceLogOptions {
  namespace: string; pod: string; container: string; previous: boolean; since: number;
  tail: number; paused: boolean; restart: number; uid?: string; kind?: string; podUID?: string; enabled?: boolean;
}
export function usePodLogStream(options: ResourceLogOptions) {
  const { client: api, cluster } = useKubernetesCluster();
  const { namespace, pod, container, previous, since, tail, paused, restart, uid, kind, podUID, enabled = true } = options;
  const workload = kind === "Deployment" || kind === "StatefulSet";
  const identity = JSON.stringify([cluster, namespace, pod, uid, kind, podUID, container, previous, since, tail, restart]);
  const session = useRef(emptySession(""));
  const [buffer, setBuffer] = useState(emptyBuffer);
  const [status, setStatus] = useState<StreamStatus>("connecting");
  const [message, setMessage] = useState("");
  const [replayUncertain, setReplayUncertain] = useState(false);
  const [pods, setPods] = useState<KubernetesWorkloadLogPod[]>();
  const [truncated, setTruncated] = useState(false);

  useEffect(() => {
    if (session.current.identity !== identity) {
      session.current = emptySession(identity);
      setBuffer(session.current.buffer);
      setMessage("");
      setReplayUncertain(false);
      setPods(undefined);
      setTruncated(false);
    }
    if (!enabled) return;
    if (workload && !uid) { setStatus("error"); setMessage("Resource identity is unavailable. Reopen the resource to retry."); return; }
    const explicitlyResumed = session.current.paused && !paused;
    session.current.paused = paused;
    if (paused) { setStatus("paused"); return; }
    if (explicitlyResumed && session.current.stopped?.status === "ended") session.current.stopped = undefined;
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
    const sourceStatuses = new Set<string>();
    const flush = () => {
      clearTimeout(flushTimer);
      flushTimer = undefined;
      if (!pending.length) return;
      session.current.buffer = appendPodLogLines(session.current.buffer, pending);
      pending = [];
      setBuffer(session.current.buffer);
    };
    const run = async (rotatingRead = false) => {
      if (disposed) return;
      let terminal = false;
      let retryable = false;
      let admissionBusy = false;
      let retryAfterMs: number | undefined;
      let failureMessage = "";
      let firstHeartbeatAt: number | undefined;
      let rotating = false;
      let admitted = false;
      const uncertaintyIdentities = new Set<string>();
      setMessage("");
      setStatus(rotatingRead && !previous ? "live" : retries ? "reconnecting" : "connecting");
      try {
        const resuming = Boolean(session.current.cursor);
        const streamOptions = {
          container, previous, timestamps: false,
          ...(session.current.cursor ? { cursor: session.current.cursor } : { since_seconds: since, tail_lines: tail }),
        };
        const onEvent = (event: KubernetesWorkloadLogEvent) => {
          if (disposed) return;
          if (!terminal && !admitted && (event.event === "heartbeat" || event.event === "line" || event.event === "sources")) {
            admitted = true;
            session.current.admissionRetries = 0;
            session.current.admissionStartedAt = undefined;
          }
          if (event.cursor) {
            if (!terminal && event.event === "line" && event.cursor !== session.current.cursor) retries = 0;
            session.current.cursor = event.cursor;
          }
          if (event.replay_uncertain && !(event.event === "error" && (event.code === "previous_unavailable" || !resuming && !session.current.received))) {
            session.current.replayUncertain = true;
            setReplayUncertain(true);
            const uncertaintyIdentity = event.sequence !== undefined || event.timestamp || event.cursor
              ? JSON.stringify([event.event, event.source_id, event.pod_uid, event.incarnation, event.sequence, event.timestamp, event.ordinal, event.cursor, event.code, event.reason])
              : undefined;
            if (!uncertaintyIdentity || !uncertaintyIdentities.has(uncertaintyIdentity)) {
              if (uncertaintyIdentity) {
                uncertaintyIdentities.add(uncertaintyIdentity);
                if (uncertaintyIdentities.size > 1000) uncertaintyIdentities.delete(uncertaintyIdentities.values().next().value!);
              }
              pending.push({ text: `${workload ? "reconnected" : "resumed after reconnect"} at ${new Date().toLocaleTimeString()}`, container: "", neutral: true });
              if (!flushTimer) flushTimer = setTimeout(flush, 50);
            }
          }
          if (event.event === "sources") {
            setPods(event.pods ?? []);
            setTruncated(event.truncated === true);
          } else if (event.event === "source_status") {
            if (event.code === "source_limit" || event.reason === "source_limit") return;
            const key = JSON.stringify([event.source_id, event.pod_uid, event.pod, event.container, event.code, event.message]);
            if (!sourceStatuses.has(key)) {
              sourceStatuses.add(key);
              // Bound notices independently from the display ring.
              if (sourceStatuses.size > 1000) sourceStatuses.delete(sourceStatuses.values().next().value!);
              pending.push({ text: `logs not available: ${(event.message || event.code || event.reason || "unavailable").replace(/^logs not available:\s*/i, "")}`, container: event.container, pod: event.pod, pod_uid: event.pod_uid, source_id: event.source_id, neutral: true });
              if (!flushTimer) flushTimer = setTimeout(flush, 50);
            }
          } else if (event.event === "line") {
            if (terminal) return;
            session.current.received = true;
            if (event.timestamp && event.ordinal) {
              const lineIdentity = JSON.stringify([event.source_id, event.pod_uid, event.incarnation, event.container, event.timestamp, event.ordinal]);
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
            pending.push({ text: event.text, container: event.container, timestamp: event.timestamp, pod: event.pod, pod_uid: event.pod_uid, source_id: event.source_id });
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
            if (event.event === "limit" && (event.cursor || !previous && !session.current.received && event.reason === "duration")) {
              rotating = true;
              retryable = true;
              setStatus(previous ? "connecting" : "live");
              return;
            }
            admissionBusy = event.event === "error" && event.code === "log_stream_busy";
            retryable = event.event === "error" && event.code !== "previous_unavailable" && event.code !== "forbidden" && event.code !== "unauthorized" && event.code !== "invalid_arguments" && (admissionBusy || event.retryable === true);
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
        };
        if (workload) await api.kubernetesWorkloadLogStream(kind!, namespace, pod, uid!, { ...streamOptions, ...(podUID ? { pod_uid: [podUID] } : {}) }, onEvent, controller.signal);
        else await api.kubernetesPodLogStream(namespace, pod, streamOptions, onEvent, controller.signal);
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
        } else if (terminal) {
          // A terminal SSE event owns the outcome even if the reader subsequently throws.
        } else {
          const body = failure instanceof ApiError ? failure.body : undefined;
          const payload = body && typeof body === "object" && "error" in body && body.error && typeof body.error === "object" ? body.error : body;
          const code = payload && typeof payload === "object" && "code" in payload ? payload.code : undefined;
          admissionBusy = code === "log_stream_busy" && failure instanceof ApiError && (failure.status === 429 || failure.status >= 500);
          retryAfterMs = failure instanceof ApiError ? failure.retryAfterMs : undefined;
          retryable = code !== "previous_unavailable" && (!(failure instanceof ApiError) || failure.status === 429 || failure.status >= 500);
          failureMessage = failure instanceof ApiError ? failure.message : "Pod log connection interrupted.";
          setStatus("error");
          setMessage(failureMessage);
        }
      }
      flush();
      if (disposed || terminal && !retryable) return;
      if (rotating) {
        retryTimer = setTimeout(() => void run(true), 0);
        return;
      }
      if (!retryable || previous && !admissionBusy) return;
      if (session.current.received && !session.current.cursor) {
        setStatus("error");
        setMessage("Connection interrupted without a resume cursor. Reload logs to start a new read; a gap may exist.");
        return;
      }
      if (admissionBusy) {
        const current = session.current;
        current.admissionStartedAt ??= Date.now();
        const delay = Math.max(500 * 2 ** current.admissionRetries, Math.min(retryAfterMs ?? 0, 2000));
        if (current.admissionRetries >= 3 || Date.now() - current.admissionStartedAt + delay > 10_000) {
          const exhaustedMessage = `${failureMessage} Admission retry attempts exhausted. Reload logs to retry.`;
          current.stopped = { status: "error", message: exhaustedMessage };
          setStatus("error");
          setMessage(exhaustedMessage);
          return;
        }
        current.admissionRetries++;
        setStatus("reconnecting");
        setMessage(`${failureMessage} Retrying log stream admission.`);
        retryTimer = setTimeout(() => void run(), delay);
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
  }, [identity, namespace, pod, uid, kind, podUID, workload, container, previous, since, tail, paused, enabled, api]);
  return { buffer, status, message, replayUncertain, pods, truncated };
}