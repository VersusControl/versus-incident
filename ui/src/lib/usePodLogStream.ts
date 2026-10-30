import { useEffect, useRef, useState } from "react";
import { ApiError, type KubernetesPodLogLine, type KubernetesWorkloadLogEvent, type KubernetesWorkloadLogPod } from "@/lib/api";
import { useKubernetesCluster } from "@/lib/useKubernetesCluster";
import { SSELimitError } from "@/lib/sse";

export const POD_LOG_MAX_LINES = 10_000;
export const POD_LOG_MAX_BYTES = 4 << 20;
export const POD_LOG_MAX_IDENTITIES = POD_LOG_MAX_LINES;
// Loading earlier history raises the ring's line ceiling: the reader asked for
// that window explicitly, and the byte ceiling still bounds memory.
export const POD_LOG_MAX_LOADED_LINES = 4 * POD_LOG_MAX_LINES;
export const POD_LOG_EARLIER_TAIL = 5_000;
// One workload history page covers every Pod in the workload at once, and the
// backend caps that page at 1,000 lines: a larger request is refused outright,
// which is why a workload "Load earlier" would always fail.
export const POD_LOG_WORKLOAD_EARLIER_TAIL = 1_000;
// Cursors are checkpointed every few lines, so a small lag behind the newest
// line is normal. A cursor this far behind it would re-read a large gap the
// reader already has, and the newest line's own time is then the better resume.
export const POD_LOG_CURSOR_MAX_LAG_MS = 30_000;
// A container clock can run ahead of the browser's. A resume time is clamped to
// wall time plus this tolerance, which stays inside the window the server
// accepts, so a fast container clock cannot turn into a refused read.
export const POD_LOG_CLOCK_SKEW_MS = 5_000;
// A read that ends on a limit and resumes from the position it already held
// makes no progress. Rotating again from there cannot help, so after this many
// rotations in a row that add no line the read stops and says so instead of
// showing a live pill over a frozen output.
export const POD_LOG_MAX_STALLED_ROTATIONS = 3;
type DisplayLine = Pick<KubernetesPodLogLine, "text" | "container" | "timestamp"> & { pod?: string; pod_uid?: string; source_id?: string; neutral?: boolean };
type BufferedLine = DisplayLine & { bytes: number; key: number };
export interface PodLogBuffer {
  lines: BufferedLine[];
  bytes: number;
  dropped: number;
  nextKey: number;
  // Ring capacity in lines; absent means the live tail cap.
  limit?: number;
}
const encoder = new TextEncoder();
const displayLine = (line: DisplayLine): DisplayLine => ({ text: line.text, container: line.container, timestamp: line.timestamp, ...(line.pod ? { pod: line.pod, pod_uid: line.pod_uid, source_id: line.source_id } : {}), ...(line.neutral ? { neutral: true } : {}) });
const lineBytes = (line: DisplayLine) => encoder.encode(JSON.stringify(line)).byteLength + 64;
export function appendPodLogLines(buffer: PodLogBuffer, incoming: DisplayLine[]): PodLogBuffer {
  let bytes = buffer.bytes;
  let nextKey = buffer.nextKey;
  const limit = buffer.limit ?? POD_LOG_MAX_LINES;
  const lines = [...buffer.lines];
  for (const line of incoming) {
    const display = displayLine(line);
    const size = lineBytes(display);
    lines.push({ ...display, bytes: size, key: nextKey++ });
    bytes += size;
  }
  let removed = 0;
  while (lines.length - removed > limit || bytes > POD_LOG_MAX_BYTES) {
    bytes -= lines[removed++].bytes;
  }
  return { lines: lines.slice(removed), bytes, dropped: buffer.dropped + removed, nextKey, ...(buffer.limit !== undefined ? { limit: buffer.limit } : {}) };
}

const lineKey = (line: DisplayLine) => JSON.stringify([line.text, line.container ?? "", line.pod ?? ""]);

// Earlier history arrives from its own bounded read that ends now, so the two
// reads meet inside the buffer rather than at its head: a short window sits
// wholly inside a long live read, and nothing about the buffer's first line
// says where the seam is. Instead every line the buffer already holds is
// claimed once, from the newest end where both reads agree, and the seam is
// where an earlier line has no claim left. That drops exactly the repeated
// occurrences and keeps every genuinely older line in order.
export function earlierOverlap(buffer: PodLogBuffer, earlier: DisplayLine[]): number {
  if (!earlier.length || !buffer.lines.length) return 0;
  const occurrences = new Map<string, { timed: string[]; untimed: number }>();
  for (const line of buffer.lines) {
    const key = lineKey(line);
    let entry = occurrences.get(key);
    if (!entry) { entry = { timed: [], untimed: 0 }; occurrences.set(key, entry); }
    if (line.timestamp) entry.timed.push(line.timestamp);
    else entry.untimed++;
  }
  let overlap = 0;
  for (let index = earlier.length - 1; index >= 0; index--) {
    const line = earlier[index];
    const entry = occurrences.get(lineKey(line));
    if (!entry) break;
    if (line.timestamp) {
      // Two timed lines name the same occurrence only when their times agree.
      let matched = -1;
      for (let at = entry.timed.length - 1; at >= 0; at--) {
        if (entry.timed[at] === line.timestamp) { matched = at; break; }
      }
      if (matched >= 0) entry.timed.splice(matched, 1);
      else if (entry.untimed > 0) entry.untimed--;
      else break;
    } else if (entry.timed.length) {
      entry.timed.pop();
    } else if (entry.untimed > 0) {
      entry.untimed--;
    } else break;
    overlap++;
  }
  return overlap;
}
export function prependEarlierLines(buffer: PodLogBuffer, earlier: DisplayLine[]): PodLogBuffer {
  const fresh = earlier.slice(0, earlier.length - earlierOverlap(buffer, earlier));
  if (!fresh.length) return buffer;
  let nextKey = buffer.nextKey;
  const prepared = fresh.map((line) => {
    const display = displayLine(line);
    return { ...display, bytes: lineBytes(display), key: nextKey++ };
  });
  let bytes = buffer.bytes + prepared.reduce((total, line) => total + line.bytes, 0);
  const lines = [...prepared, ...buffer.lines];
  let removed = 0;
  while (bytes > POD_LOG_MAX_BYTES && lines.length - removed > 1) bytes -= lines[removed++].bytes;
  const kept = lines.slice(removed);
  return { lines: kept, bytes, dropped: buffer.dropped + removed, nextKey, limit: Math.min(POD_LOG_MAX_LOADED_LINES, Math.max(buffer.limit ?? POD_LOG_MAX_LINES, kept.length)) };
}

const emptyBuffer = (): PodLogBuffer => ({ lines: [], bytes: 0, dropped: 0, nextKey: 0 });

const lineTime = (timestamp?: string): number | undefined => {
  if (!timestamp) return undefined;
  const epoch = Date.parse(timestamp);
  return Number.isFinite(epoch) ? epoch : undefined;
};

// A cursor names the exact line it was issued for, so its own time is the
// position the server will replay from.
export function cursorLineTime(cursor: string): number | undefined {
  if (!cursor) return undefined;
  try {
    const standard = cursor.replace(/-/g, "+").replace(/_/g, "/");
    const padded = standard.padEnd(standard.length + (4 - standard.length % 4) % 4, "=");
    const payload: unknown = JSON.parse(atob(padded));
    if (!payload || typeof payload !== "object" || !("timestamp" in payload)) return undefined;
    return typeof payload.timestamp === "string" ? lineTime(payload.timestamp) : undefined;
  } catch {
    return undefined;
  }
}

// A fast container clock can hand back a line time that is still in the future
// for the browser; asking the server to read from there would be refused.
export function clampSinceTime(epoch: number, now = Date.now()): number {
  return Math.min(epoch, now + POD_LOG_CLOCK_SKEW_MS);
}

type StreamStatus = "connecting" | "live" | "paused" | "reconnecting" | "ended" | "limited" | "error";
const emptySession = (identity: string) => ({
  identity, buffer: emptyBuffer(), cursor: "", received: false, identities: new Set<string>(), paused: false,
  admissionRetries: 0, admissionStartedAt: undefined as number | undefined,
  replayUncertain: false, stopped: undefined as { status: StreamStatus; message: string } | undefined,
  // Newest line time per source Pod, so a reconnect can ask for everything
  // after it without a cursor that grows with the workload.
  newestBySource: new Map<string, number>(),
  // How far a source that has gone quiet has been observed: a source that has
  // not written yet, or whose last line is older than the connection that
  // followed it. It holds the group back only to the last read that covered it,
  // never to where it joined or to that one older line.
  unwrittenSince: new Map<string, number>(),
  // The cursor issued with the newest line shown. A cursor that names any other
  // position is behind the reader and is not a resume point.
  cursorAtNewestLine: "",
  // Lines this reading position has shown, and how many rotations in a row
  // arrived without one.
  linesAccepted: 0, stalledRotations: 0,
  // The newest line this reading position has shown. Every line carries its
  // time, so it is a resume point for a single Pod read without a cursor.
  newestLineAt: undefined as number | undefined,
  // The sources the current read still follows, and when this reading position
  // started: a source that has joined but not written yet is resumed from the
  // start of the read instead of being skipped.
  liveSources: new Set<string>(), connectionStart: undefined as number | undefined,
  loadingEarlier: false,
  // A resume that could not use its cursor and started from a wall-clock time
  // instead. The read is continuous from that time, not from where the reader
  // left off, which is a gap in coverage rather than a failed read.
  clampedResume: false,
});

export interface ResourceLogOptions {
  namespace: string; pod: string; container: string; previous: boolean; since?: number;
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
  // The backend's own guidance for the stop, with whether asking again can
  // help. A reader must never replace it with advice of its own: only a real
  // API-server denial carries permission guidance.
  const [notice, setNotice] = useState<{ action?: string; retryable: boolean }>({ retryable: false });
  const [replayUncertain, setReplayUncertain] = useState(false);
  // Where a clamped resume left the read: named in the output so a gap in
  // coverage is never mistaken for a failed read.
  const [gap, setGap] = useState("");
  const [pods, setPods] = useState<KubernetesWorkloadLogPod[]>();
  const [truncated, setTruncated] = useState(false);
  const [received, setReceived] = useState(false);
  const [earlier, setEarlier] = useState<"idle" | "loading" | "error">("idle");
  useEffect(() => {
    const sinceTime = (epoch: number) => new Date(clampSinceTime(epoch - 1000)).toISOString();
    // A resume point is a cursor for one Pod and a wall-clock time for a
    // workload, so both live behind one question.
    const podResume = (): { cursor?: string; since_time?: string } | undefined => {
      const newest = session.current.newestLineAt;
      const cursor = session.current.cursor;
      const cursorTime = cursor ? cursorLineTime(cursor) : undefined;
      // The server hands back the last cursor it issued, which can name a
      // position well behind the lines already on screen. Replaying a little of
      // what the reader has is cheaper than the request, but replaying a large
      // gap means the cursor is no longer a resume point: the newest line's own
      // time is.
      if (!cursor) {
        // Without a cursor the read restarts from the newest line's own time,
        // which is continuous from where the reader is but not an exact
        // position: the same wall-clock restart the clamp below produces.
        if (newest !== undefined) session.current.clampedResume = true;
        return newest !== undefined ? { since_time: sinceTime(newest) } : undefined;
      }
      if (newest !== undefined && cursorTime !== undefined && newest - cursorTime > POD_LOG_CURSOR_MAX_LAG_MS) {
        session.current.clampedResume = true;
        setGap(`resumed at ${new Date(newest).toLocaleTimeString()}; lines before that time may be missing`);
        return { since_time: sinceTime(newest) };
      }
      return { cursor };
    };
    const workloadResume = (): { since_time: string } | undefined => {
      const current = session.current;
      // Each source sits where the furthest of its own newest line and the last
      // read that observed it left it. A source that has joined but not written
      // yet must not be skipped by a resume that starts after it did, so it
      // holds the resume back to the last read that covered it — or, before any
      // read has, to when this reading position began. A source that wrote one
      // line and then went quiet must not pin the resume to that line either.
      const positions = new Map(current.newestBySource);
      if (current.connectionStart !== undefined) {
        for (const source of current.liveSources) {
          const newest = current.newestBySource.get(source);
          const observed = current.unwrittenSince.get(source);
          if (newest === undefined) positions.set(source, observed ?? current.connectionStart);
          else positions.set(source, observed === undefined ? newest : Math.max(newest, observed));
        }
        if (!positions.size && current.received) positions.set("", current.connectionStart);
      }
      const times = [...positions.values()];
      if (!times.length) return undefined;
      // The laggiest source decides the resume time: asking from the newest of
      // the whole workload would skip lines a slower Pod had not written yet.
      const laggiest = Math.min(...times) - 1000;
      const floor = Date.now() - 23 * 60 * 60 * 1000;
      return { since_time: new Date(clampSinceTime(Math.max(laggiest, floor))).toISOString() };
    };
    const resumeOptions = () => (workload ? workloadResume() : podResume());
    const resumePoint = () => resumeOptions() !== undefined;
    if (session.current.identity !== identity) {
      session.current = emptySession(identity);
      setBuffer(session.current.buffer);
      setMessage("");
      setNotice({ retryable: false });
      setReplayUncertain(false);
      setGap("");
      setPods(undefined);
      setTruncated(false);
      setReceived(false);
      setEarlier("idle");
    }
    if (!enabled) return;
    // A workload row read from a link can arrive before the describe call fills
    // in its uid; the read then waits instead of claiming a broken identity.
    if (workload && !uid) { setStatus("connecting"); setMessage(""); return; }
    session.current.paused = paused;
    if (paused) { setStatus("paused"); return; }
    if (session.current.stopped) {
      setStatus(session.current.stopped.status);
      setMessage(session.current.stopped.message);
      return;
    }
    if (session.current.received && !resumePoint()) {
      setStatus("error");
      setMessage(workload
        ? "Logs cannot resume without a line time. Reload logs to start a new read; a gap may exist."
        : "Logs cannot resume without a cursor. Reload logs to start a new read; a gap may exist.");
      return;
    }
    session.current.connectionStart ??= Date.now();
    let disposed = false;
    const controller = new AbortController();
    let retryTimer: ReturnType<typeof setTimeout> | undefined;
    let frame: ReturnType<typeof setTimeout> | number | undefined;
    let frameIsAnimation = false;
    let pending: DisplayLine[] = [];
    let retries = 0;
    let attempt = 0;
    const sourceStatuses = new Set<string>();
    const flush = () => {
      frame = undefined;
      if (!pending.length) return;
      session.current.buffer = appendPodLogLines(session.current.buffer, pending);
      pending = [];
      setBuffer(session.current.buffer);
    };
    // One flush per animation frame: a burst of lines in one frame lands as a
    // single render instead of one render per line.
    const scheduleFlush = () => {
      if (frame !== undefined) return;
      if (typeof requestAnimationFrame === "function") {
        frameIsAnimation = true;
        frame = requestAnimationFrame(flush);
      } else {
        frameIsAnimation = false;
        frame = setTimeout(flush, 16);
      }
    };
    const clearFlush = () => {
      if (frame === undefined) return;
      if (frameIsAnimation && typeof cancelAnimationFrame === "function") cancelAnimationFrame(frame as number);
      else clearTimeout(frame as ReturnType<typeof setTimeout>);
      frame = undefined;
    };
    const discardProtectedLines = (source?: { source_id?: string; pod_uid?: string; pod?: string }) => {
      // Scoped denials remove raw output, not the connector's safe failure notices.
      // Otherwise a duplicate denial erases its notice before deduplication can keep it.
      const allowed = (line: DisplayLine) => Boolean(source && line.neutral) || (source?.source_id ? Boolean(line.source_id && line.source_id !== source.source_id)
        : source?.pod_uid ? Boolean(line.pod_uid && line.pod_uid !== source.pod_uid) : source?.pod ? Boolean(line.pod && line.pod !== source.pod) : false);
      pending = pending.filter(allowed);
      clearFlush();
      const lines = session.current.buffer.lines.filter(allowed);
      const buffer = { ...session.current.buffer, lines, bytes: lines.reduce((total, line) => total + line.bytes, 0) };
      // Replacing the session also fences any earlier-history read in flight.
      session.current = { ...session.current, buffer };
      setBuffer(buffer);
      setReceived(lines.some((line) => !line.neutral));
      if (pending.length) scheduleFlush();
    };
    const run = async (rotatingRead = false) => {
      if (disposed) return;
      const currentAttempt = ++attempt;
      let terminal = false;
      // The position this connection started from, and the lines already shown
      // when it began: a rotation that adds nothing is measured against both.
      const readStartedAt = Date.now();
      const acceptedAtStart = session.current.linesAccepted;
      // The resume point this connection asked from; a rotation that asks from
      // the same place again can only replay what the reader already has.
      let startedFrom = "null";
      let retryable = false;
      let admissionBusy = false;
      let retryAfterMs: number | undefined;
      let failureMessage = "";
      let firstHeartbeatAt: number | undefined;
      let rotating = false;
      // Why this connection rotated. A duration cut is the server ending a
      // follow the client asked to keep, so it cannot be a stall on its own.
      let rotationReason: string | undefined;
      let admitted = false;
      const uncertaintyIdentities = new Set<string>();
      setMessage("");
      setStatus(rotatingRead && !previous ? "live" : retries ? "reconnecting" : "connecting");
      try {
        // Each attempt asks for its own resume point, so a clamp recorded by
        // the previous one never colours this read.
        session.current.clampedResume = false;
        const resuming = resumePoint();
        const resume = resumeOptions();
        startedFrom = JSON.stringify(resume ?? null);
        // Every connection carries its own floor. Keeping the first one for the
        // life of the panel made each rotation resume from the moment it
        // mounted, so the replay window grew with the follow.
        session.current.connectionStart = readStartedAt;
        const streamOptions = {
          container, previous, timestamps: false,
          ...(resume ?? { ...(since !== undefined ? { since_seconds: since } : {}), tail_lines: tail }),
        };
        const onEvent = (event: KubernetesWorkloadLogEvent) => {
          if (disposed || currentAttempt !== attempt || terminal && event.event !== "end") return;
          const positionAdvanced = Boolean(event.cursor && event.cursor !== session.current.cursor)
            || Boolean(event.timestamp && Date.parse(event.timestamp) > (session.current.newestLineAt ?? -Infinity));
          if (!terminal && !admitted && (event.event === "heartbeat" || event.event === "line" || event.event === "sources")) {
            admitted = true;
            session.current.admissionRetries = 0;
            session.current.admissionStartedAt = undefined;
          }
          if (event.cursor && !workload) {
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
              scheduleFlush();
            }
          }
          if (event.event === "sources") {
            setPods(event.pods ?? []);
            setTruncated(event.truncated === true);
            // A resume point for a source that left the workload would pin every
            // later rotation to a Pod that no longer writes, so the resume keeps
            // only the sources this read still follows.
            const live = new Set((event.sources ?? []).map((source) => source.source_id));
            for (const source of [...session.current.newestBySource.keys()]) {
              if (!live.has(source)) session.current.newestBySource.delete(source);
            }
            for (const source of [...session.current.unwrittenSince.keys()]) {
              if (!live.has(source)) session.current.unwrittenSince.delete(source);
            }
            session.current.liveSources = live;
          } else if (event.event === "source_status") {
            if (["forbidden", "unauthorized", "credential_unavailable"].includes(event.code ?? "")) discardProtectedLines(event);
            if (event.code === "source_limit" || event.reason === "source_limit") return;
            const key = JSON.stringify([event.source_id, event.pod_uid, event.pod, event.container, event.code, event.message]);
            if (!sourceStatuses.has(key)) {
              sourceStatuses.add(key);
              // Bound notices independently from the display ring.
              if (sourceStatuses.size > 1000) sourceStatuses.delete(sourceStatuses.values().next().value!);
              pending.push({ text: `logs not available: ${(event.message || event.code || event.reason || "unavailable").replace(/^logs not available:\s*/i, "")}`, container: event.container, pod: event.pod, pod_uid: event.pod_uid, source_id: event.source_id, neutral: true });
              scheduleFlush();
            }
          } else if (event.event === "line") {
            if (terminal) return;
            session.current.received = true;
            setReceived(true);
            // A line is evidence the read works, for a Pod and for a workload
            // alike; a workload has no cursor to signal that.
            if (event.timestamp) {
              const epoch = Date.parse(event.timestamp);
              if (Number.isFinite(epoch)) {
                const source = event.source_id ?? "";
                const newest = session.current.newestBySource.get(source);
                if (newest === undefined || epoch > newest) session.current.newestBySource.set(source, epoch);
                if (session.current.newestLineAt === undefined || epoch > session.current.newestLineAt) session.current.newestLineAt = epoch;
              }
            }
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
            if (positionAdvanced || Date.now() - readStartedAt >= 10_000) retries = 0;
            pending.push({ text: event.text, container: event.container, timestamp: event.timestamp, pod: event.pod, pod_uid: event.pod_uid, source_id: event.source_id });
            session.current.linesAccepted++;
            session.current.cursorAtNewestLine = event.cursor ?? "";
            if (pending.length >= 100) flush();
            else scheduleFlush();
            if (!terminal) setStatus("live");
          } else if (event.event === "heartbeat" && !terminal) {
            const now = Date.now();
            if (firstHeartbeatAt !== undefined && now - firstHeartbeatAt >= 10_000) retries = 0;
            firstHeartbeatAt ??= now;
            setStatus("live");
          } else if (event.event === "limit" || event.event === "error") {
            // A read that resumed from a wall-clock time cannot promise an exact
            // continuation, and the server says so with replay_uncertain. An
            // event that carries no failure class is that uncertainty and
            // nothing worse: it names the gap and the read keeps following.
            if (event.event === "error" && event.replay_uncertain && !event.code && session.current.clampedResume) {
              session.current.replayUncertain = true;
              setReplayUncertain(true);
              setGap((current) => current || `resumed at ${new Date().toLocaleTimeString()}; lines before that time may be missing`);
              return;
            }
            terminal = true;
            if (["forbidden", "unauthorized", "credential_unavailable"].includes(event.code ?? "")) discardProtectedLines();
            // A terminal limit can arrive without a usable cursor, and a live
            // Pod read then strands on a stopped state the reader cannot leave.
            // Every line carries its time, so the newest retained line rotates
            // the read the way a cursor would.
            const newestLine = session.current.newestLineAt;
            // A follow checkpoints its first lines and then stops issuing
            // cursors, repeating the last one on the terminal limit. That
            // cursor names a line many checkpoints behind the newest one, so
            // rotating from it replays the same window into the same limit for
            // ever: the pill says live and the output never grows. Rotate from
            // the newest retained line instead whenever the server says the
            // cursor lags, or the cursor is not the one issued with that line.
            if (!workload && newestLine !== undefined
              && (event.cursor_lagging === true || session.current.cursor !== session.current.cursorAtNewestLine)) {
              session.current.cursor = "";
            }
            const fromNewestLine = !previous && (workload ? resumePoint() : newestLine !== undefined);
            if (event.event === "limit" && (event.cursor !== undefined || fromNewestLine || (!session.current.received && event.reason === "duration"))) {
              rotating = true;
              rotationReason = event.reason;
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
          if (failure instanceof ApiError && [401, 403].includes(failure.status) || ["forbidden", "unauthorized", "credential_unavailable"].includes(String(code))) discardProtectedLines();
          admissionBusy = code === "log_stream_busy" && failure instanceof ApiError && (failure.status === 429 || failure.status >= 500);
          retryAfterMs = failure instanceof ApiError ? failure.retryAfterMs : undefined;
          retryable = code !== "previous_unavailable" && (!(failure instanceof ApiError) || failure.status === 429 || failure.status >= 500);
          failureMessage = failure instanceof ApiError ? failure.message : "Pod log connection interrupted.";
          const action = payload && typeof payload === "object" && "action" in payload && typeof payload.action === "string" ? payload.action : undefined;
          const retryableHint = payload && typeof payload === "object" && "retryable" in payload && typeof payload.retryable === "boolean" ? payload.retryable : undefined;
          retryable = retryable && retryableHint !== false && !["forbidden", "unauthorized", "invalid_arguments"].includes(String(code));
          failureMessage = [code, failureMessage].filter(Boolean).join(" ");
          if (!retryable) session.current.stopped = { status: "error", message: failureMessage };
          setNotice({ action, retryable });
          setStatus("error");
          setMessage(failureMessage);
        }
      }
      flush();
      // A connection that ended normally covered every source from its resume
      // point to its own cut, so every source it followed has been observed up
      // to the moment this connection began — whether it never wrote or wrote
      // once and then went quiet. Advancing that watermark per connection keeps
      // a silent sidecar, an init container or a source that logged a single
      // line from pinning every later resume behind it, which would replay the
      // whole window on each rotation. A connection that failed advances
      // nothing, so no line can fall into the gap.
      if (!failureMessage) {
        for (const source of session.current.liveSources) {
          const newest = session.current.newestBySource.get(source);
          if (newest === undefined || newest < readStartedAt) session.current.unwrittenSince.set(source, readStartedAt);
        }
      }
      if (disposed || terminal && !retryable) return;
      if (rotating) {
        // A rotation that resumes from the position it just read makes no
        // progress: the reader would sit on a live pill over frozen output.
        // New lines are the obvious progress; for a quiet source the resume
        // point itself moving forward is enough, because the next rotation
        // then covers a window the reader has not read yet.
        const resumesAhead = JSON.stringify(resumeOptions() ?? null) !== startedFrom;
        const stalled = session.current.received && session.current.linesAccepted <= acceptedAtStart && !resumesAhead;
        session.current.stalledRotations = stalled ? session.current.stalledRotations + 1 : 0;
        // Only a rotation that froze on a limit can spin for ever. A duration
        // cut over an idle Pod resumes from the same line every time precisely
        // because the Pod has written nothing since, and the follow should keep
        // reading instead of stopping on a read that was never stuck.
        if (rotationReason !== "duration" && session.current.stalledRotations >= POD_LOG_MAX_STALLED_ROTATIONS) {
          const stalledMessage = "Log read keeps restarting from the same position without new lines. Reload logs to start a new read; a gap may exist.";
          session.current.stopped = { status: "limited", message: stalledMessage };
          setStatus("limited");
          setMessage(stalledMessage);
          return;
        }
        retryTimer = setTimeout(() => void run(true), 0);
        return;
      }
      if (!retryable || previous && !admissionBusy) return;
      if (session.current.received && !resumePoint()) {
        setStatus("error");
        setMessage(workload
          ? "Connection interrupted without a line time. Reload logs to start a new read; a gap may exist."
          : "Connection interrupted without a resume cursor. Reload logs to start a new read; a gap may exist.");
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
      setMessage(`${failureMessage ? `${failureMessage} ` : ""}Connection interrupted. ${resumePoint() ? "Resuming from the last line read." : "Retrying the log connection."}`);
      const delay = Math.max(1000 * 2 ** retries, retryAfterMs ?? 0);
      if (delay > 60_000) {
        const stoppedMessage = `${failureMessage} Retry delay exceeds this log read budget. Reload logs to retry.`;
        session.current.stopped = { status: "error", message: stoppedMessage };
        setStatus("error");
        setMessage(stoppedMessage);
        return;
      }
      retryTimer = setTimeout(() => { retries++; void run(); }, delay);
    };
    void run();
    return () => {
      disposed = true;
      controller.abort();
      clearTimeout(retryTimer);
      clearFlush();
      flush();
    };
  }, [identity, namespace, pod, uid, kind, podUID, workload, container, previous, since, tail, paused, enabled, api]);

  // One bounded history read, independent of the live stream: it prepends what
  // it finds and never restarts the follow.
  const loadEarlier = async (windowSeconds: number) => {
    if (session.current.loadingEarlier) return;
    const target = session.current;
    target.loadingEarlier = true;
    setEarlier("loading");
    try {
      let lines: DisplayLine[];
      if (workload) {
        const page = await api.kubernetesWorkloadLogs(kind!, namespace, pod, { ...(container ? { container } : {}), previous, since_seconds: windowSeconds, tail_lines: POD_LOG_WORKLOAD_EARLIER_TAIL });
        lines = page.lines.map((line) => ({ text: line.text, container: line.container ?? "", timestamp: line.at, pod: line.pod }));
      } else {
        const page = await api.kubernetesPodLogs(namespace, pod, { ...(container ? { container } : {}), previous, since_seconds: windowSeconds, tail_lines: POD_LOG_EARLIER_TAIL });
        lines = page.text.split("\n").filter((text) => text.length > 0).map((text) => ({ text, container }));
      }
      if (session.current !== target) return;
      target.buffer = prependEarlierLines(target.buffer, lines);
      setBuffer(target.buffer);
      setEarlier("idle");
    } catch {
      if (session.current === target) setEarlier("error");
    } finally {
      target.loadingEarlier = false;
    }
  };
  const current = session.current.identity === identity;
  return {
    buffer: current ? buffer : emptyBuffer(), status: current ? status : "connecting" as const,
    message: current ? message : "", notice: current ? notice : { retryable: false },
    replayUncertain: current && replayUncertain, gap: current ? gap : "", pods: current ? pods : undefined,
    truncated: current && truncated, received: current && received, earlier: current ? earlier : "idle" as const, loadEarlier,
  };
}