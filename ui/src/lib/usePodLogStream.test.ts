// @vitest-environment jsdom
import { act, cleanup, renderHook } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api, type KubernetesPodLogEvent } from "@/lib/api";
import { SSELimitError } from "@/lib/sse";
import { appendPodLogLines, POD_LOG_MAX_BYTES, usePodLogStream } from "./usePodLogStream";

afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.useRealTimers(); });
const options = { namespace: "shop", pod: "api", container: "app", previous: false, since: 3600, tail: 500, paused: false, restart: 0 };

describe("Pod log stream lifecycle", () => {
  it("exhausts error-only checkpoints without losing the backend cause or restarting on resume", async () => {
    vi.useFakeTimers();
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementation(async (_namespace, _pod, _options, emit) => {
      emit({ event: "error", text: "", container: "app", cursor: `error-${stream.mock.calls.length}`, retryable: true, code: "upstream_unavailable", message: "Kubelet unreachable", action: "Check node connectivity" });
      emit({ event: "end", text: "", container: "app", cursor: `end-${stream.mock.calls.length}` });
    });
    const { result, rerender } = renderHook((props) => usePodLogStream(props), { initialProps: options });
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(stream).toHaveBeenCalledTimes(4);
    expect(result.current.message).toContain("upstream_unavailable Kubelet unreachable Check node connectivity");
    expect(result.current.message).toContain("exhausted");
    rerender({ ...options, paused: true });
    rerender(options);
    expect(stream).toHaveBeenCalledTimes(4);
    expect(result.current.status).toBe("error");
  });

  it("caps lines and bytes while retaining identical legitimate lines", () => {
    const empty = { lines: [], bytes: 0, dropped: 0, nextKey: 0 };
    const countBound = appendPodLogLines(empty, Array.from({ length: 10005 }, () => ({ text: "same", container: "app" })));
    expect(countBound.lines).toHaveLength(10000);
    expect(countBound.dropped).toBe(5);
    const checkpointLines = appendPodLogLines(empty, Array.from({ length: 10000 }, (_, index) => ({
      text: "x", container: "app", timestamp: "2026-10-07T12:00:00Z", ordinal: index + 1,
      sequence: index + 1, cursor: "checkpoint".repeat(1000),
    })));
    expect(checkpointLines.lines).toHaveLength(10000);
    expect(checkpointLines.bytes).toBeLessThan(POD_LOG_MAX_BYTES);
    expect(checkpointLines.lines[0]).not.toHaveProperty("cursor");
    expect(checkpointLines.lines[0]).not.toHaveProperty("ordinal");
    expect(checkpointLines.lines[0]).not.toHaveProperty("sequence");
    const byteBound = appendPodLogLines(empty, Array.from({ length: 400 }, () => ({ text: "x".repeat(16384), container: "app" })));
    expect(byteBound.bytes).toBeLessThanOrEqual(POD_LOG_MAX_BYTES);
    expect(byteBound.dropped).toBeGreaterThan(0);
  });

  it("aborts on pause, resumes with the cursor, and cancels on unmount", async () => {
    vi.useFakeTimers();
    let emit: (event: KubernetesPodLogEvent) => void = () => undefined;
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementation((_namespace, _pod, _options, callback, signal) => {
      emit = callback;
      return new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
    });
    const { result, rerender, unmount } = renderHook((props) => usePodLogStream(props), { initialProps: options });
    act(() => emit({ event: "line", text: "same", container: "app", cursor: "cursor-two", ordinal: 2 }));
    rerender({ ...options, paused: true });
    expect(stream.mock.calls[0][4].aborted).toBe(true);
    expect(result.current.buffer.lines).toHaveLength(1);
    rerender(options);
    expect(stream.mock.calls[1][2]).toEqual({ container: "app", previous: false, timestamps: false, cursor: "cursor-two" });
    unmount();
    expect(stream.mock.calls[1][4].aborted).toBe(true);
  });

  it("bounds reconnect attempts and never reconnects finite previous reads", async () => {
    vi.useFakeTimers();
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockResolvedValue();
    const { result, rerender } = renderHook((props) => usePodLogStream(props), { initialProps: options });
    await act(async () => { await vi.advanceTimersByTimeAsync(8000); });
    expect(stream).toHaveBeenCalledTimes(4);
    expect(result.current.message).toContain("exhausted");
    rerender({ ...options, previous: true });
    await act(async () => { await vi.advanceTimersByTimeAsync(8000); });
    expect(stream).toHaveBeenCalledTimes(5);
    expect(result.current.status).toBe("error");
    expect(result.current.message).toContain("before completion");
  });

  it("resets target-bound cursors on container changes and preserves server limits", async () => {
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementation(async (_namespace, _pod, _options, emit) => {
      emit({ event: "line", text: "redacted", container: "app", cursor: "old-target" });
      emit({ event: "limit", text: "", container: "app", reason: "byte_limit" });
      emit({ event: "end", text: "", container: "app" });
    });
    const { result, rerender } = renderHook((props) => usePodLogStream(props), { initialProps: options });
    await act(async () => undefined);
    expect(result.current.status).toBe("limited");
    rerender({ ...options, container: "init" });
    await act(async () => undefined);
    expect(stream.mock.calls[1][2]).not.toHaveProperty("cursor");
    expect(result.current.buffer.lines).toHaveLength(1);
  });

  it("reconnects using timestamp ordinals without collapsing repeated text", async () => {
    vi.useFakeTimers();
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementationOnce(async (_namespace, _pod, _options, emit) => {
      emit({ event: "line", text: "same", container: "app", timestamp: "2026-10-07T12:00:00Z", ordinal: 1, cursor: "one" });
      emit({ event: "line", text: "same", container: "app", timestamp: "2026-10-07T12:00:00Z", ordinal: 2, cursor: "two" });
    }).mockImplementationOnce(async (_namespace, _pod, _options, emit) => {
      emit({ event: "line", text: "same", container: "app", timestamp: "2026-10-07T12:00:00Z", ordinal: 3, cursor: "three" });
      emit({ event: "end", text: "", container: "app", reason: "complete" });
    });
    const { result } = renderHook(() => usePodLogStream(options));
    await act(async () => { await vi.advanceTimersByTimeAsync(1100); });
    expect(stream.mock.calls[1][2]).toEqual({ container: "app", previous: false, timestamps: false, cursor: "two" });
    expect(result.current.buffer.lines.map((line) => line.text)).toEqual(["same", "same", "same"]);
  });

  it("stops instead of replaying a stream without resume information", async () => {
    vi.useFakeTimers();
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementation(async (_namespace, _pod, _options, emit) => {
      emit({ event: "line", text: "untimestamped", container: "app" });
    });
    const { result, rerender } = renderHook((props) => usePodLogStream(props), { initialProps: options });
    await act(async () => { await vi.advanceTimersByTimeAsync(8000); });
    expect(stream).toHaveBeenCalledTimes(1);
    expect(result.current.message).toContain("without a resume cursor");
    rerender({ ...options, paused: true });
    rerender(options);
    expect(stream).toHaveBeenCalledTimes(1);
    expect(result.current.message).toContain("cannot resume");
  });

  it("caps consecutive retryable errors despite initial server heartbeats", async () => {
    vi.useFakeTimers();
    const startedAt = Date.now();
    const attemptTimes: number[] = [];
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementation(async (_namespace, _pod, _options, emit) => {
      attemptTimes.push(Date.now() - startedAt);
      emit({ event: "heartbeat", text: "", container: "app" });
      emit({ event: "error", text: "", container: "app", retryable: true, message: "Upstream unavailable" });
    });
    const { result } = renderHook(() => usePodLogStream(options));
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(stream).toHaveBeenCalledTimes(4);
    expect(attemptTimes).toEqual([0, 1000, 3000, 7000]);
    expect(result.current.message).toContain("exhausted");
    expect(stream.mock.calls.map((call) => call[2].timestamps)).toEqual([false, false, false, false]);
  });

  it("recovers after three intermittent drops when cursor progress replenishes retries", async () => {
    vi.useFakeTimers();
    let attempts = 0;
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementation(async (_namespace, _pod, _options, emit) => {
      attempts++;
      emit({ event: "line", text: "progress", container: "app", cursor: `cursor-${attempts}` });
      if (attempts === 4) emit({ event: "end", text: "", container: "app" });
    });
    const { result } = renderHook(() => usePodLogStream(options));
    await act(async () => { await vi.advanceTimersByTimeAsync(3100); });
    expect(stream).toHaveBeenCalledTimes(4);
    expect(stream.mock.calls.slice(1).map((call) => call[2].cursor)).toEqual(["cursor-1", "cursor-2", "cursor-3"]);
    expect(result.current.status).toBe("ended");
    expect(result.current.buffer.lines).toHaveLength(4);
  });

  it("replenishes retries after observed sustained silence but not an initial heartbeat", async () => {
    vi.useFakeTimers();
    let attempts = 0;
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementation((_namespace, _pod, _options, emit) => {
      attempts++;
      emit({ event: "heartbeat", text: "", container: "app" });
      if (attempts === 3) return new Promise<void>((resolve) => setTimeout(() => {
        emit({ event: "heartbeat", text: "", container: "app" });
        resolve();
      }, 15_000));
      return Promise.resolve();
    });
    const { result } = renderHook(() => usePodLogStream(options));
    await act(async () => { await vi.advanceTimersByTimeAsync(18_000); });
    expect(stream).toHaveBeenCalledTimes(3);
    await act(async () => { await vi.advanceTimersByTimeAsync(1000); });
    expect(stream).toHaveBeenCalledTimes(4);
    await act(async () => { await vi.advanceTimersByTimeAsync(6000); });
    expect(stream).toHaveBeenCalledTimes(6);
    expect(result.current.message).toContain("exhausted");
  });

  it("does not treat a delayed initial heartbeat as a stable upstream connection", async () => {
    vi.useFakeTimers();
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementation((_namespace, _pod, _options, emit) => {
      return new Promise<void>((resolve) => setTimeout(() => {
        emit({ event: "heartbeat", text: "", container: "app" });
        emit({ event: "error", text: "", container: "app", retryable: true });
        resolve();
      }, 15_000));
    });
    const { result } = renderHook(() => usePodLogStream(options));
    await act(async () => { await vi.advanceTimersByTimeAsync(120_000); });
    expect(stream).toHaveBeenCalledTimes(4);
    expect(result.current.message).toContain("exhausted");
  });

  it("suppresses only the 31 matching overlap identities after a checkpoint interruption", async () => {
    vi.useFakeTimers();
    const timestamp = "2026-10-07T12:00:00Z";
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementationOnce(async (_namespace, _pod, _options, emit) => {
      for (let ordinal = 1; ordinal <= 63; ordinal++) emit({ event: "line", text: "same", container: "app", timestamp, ordinal, sequence: ordinal, ...(ordinal === 32 ? { cursor: "checkpoint-32" } : {}) });
    }).mockImplementationOnce(async (_namespace, _pod, _options, emit) => {
      for (let ordinal = 33; ordinal <= 64; ordinal++) emit({ event: "line", text: "same", container: "app", timestamp, ordinal, sequence: ordinal - 32 });
      emit({ event: "end", text: "", container: "app", cursor: "checkpoint-64" });
    });
    const { result } = renderHook(() => usePodLogStream(options));
    await act(async () => { await vi.advanceTimersByTimeAsync(1100); });
    expect(stream.mock.calls[1][2].cursor).toBe("checkpoint-32");
    expect(result.current.buffer.lines).toHaveLength(64);
    expect(result.current.buffer.lines.every((line) => line.text === "same")).toBe(true);
    expect(result.current.replayUncertain).toBe(false);
  });

  it("retains overlap across more than 64 timestamps and keeps backend cap uncertainty scoped until reset", async () => {
    vi.useFakeTimers();
    const timestamp = (index: number) => new Date(Date.UTC(2026, 9, 7, 12, 0, index)).toISOString();
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementationOnce(async (_namespace, _pod, _options, emit) => {
      for (let index = 1; index <= 95; index++) emit({ event: "line", text: "same", container: "app", timestamp: timestamp(index), ordinal: 1, sequence: index, ...(index % 32 === 0 ? { cursor: `checkpoint-${index}` } : {}), replay_uncertain: index > 64 });
    }).mockImplementationOnce(async (_namespace, _pod, _options, emit) => {
      for (let index = 65; index <= 95; index++) emit({ event: "line", text: "same", container: "app", timestamp: timestamp(index), ordinal: 1, sequence: index - 64 });
      emit({ event: "line", text: "same", container: "app", timestamp: timestamp(95), ordinal: 2, sequence: 32 });
      emit({ event: "end", text: "", container: "app", cursor: "final", replay_uncertain: false });
    }).mockResolvedValue();
    const { result, rerender } = renderHook((props) => usePodLogStream(props), { initialProps: options });
    await act(async () => { await vi.advanceTimersByTimeAsync(1100); });
    expect(stream.mock.calls[1][2].cursor).toBe("checkpoint-64");
    expect(result.current.buffer.lines).toHaveLength(96);
    expect(result.current.replayUncertain).toBe(true);
    rerender({ ...options, paused: true });
    rerender(options);
    expect(result.current.replayUncertain).toBe(true);
    expect(stream).toHaveBeenCalledTimes(2);
    rerender({ ...options, container: "setup" });
    expect(result.current.buffer.lines).toHaveLength(0);
    expect(result.current.replayUncertain).toBe(false);
    expect(stream.mock.calls[2][2]).not.toHaveProperty("cursor");
  });

  it.each(["heartbeat", "error"] as const)("retains a pending %s checkpoint across empty-cursor events and resumes", async (checkpointEvent) => {
    vi.useFakeTimers();
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementationOnce(async (_namespace, _pod, _options, emit) => {
      emit({ event: "line", text: "same", container: "app", timestamp: "2026-10-07T12:00:00Z", ordinal: 1 });
      emit({ event: checkpointEvent, text: "", container: "app", cursor: "pending-checkpoint", retryable: true, replay_uncertain: checkpointEvent === "error" });
      emit({ event: "heartbeat", text: "", container: "app", cursor: "" });
      if (checkpointEvent === "error") emit({ event: "end", text: "", container: "app", cursor: "read-error-checkpoint", replay_uncertain: true });
    }).mockImplementationOnce(async (_namespace, _pod, _options, emit) => emit({ event: "end", text: "", container: "app" }));
    const { result } = renderHook(() => usePodLogStream(options));
    await act(async () => { await vi.advanceTimersByTimeAsync(1100); });
    expect(stream.mock.calls[1][2].cursor).toBe(checkpointEvent === "error" ? "read-error-checkpoint" : "pending-checkpoint");
    expect(result.current.replayUncertain).toBe(checkpointEvent === "error");
    expect(result.current.buffer.lines).toHaveLength(1);
  });

  it.each(["limit", "end"] as const)("keeps %s terminal across pause/resume and starts fresh only on reload", async (event) => {
    vi.useFakeTimers();
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementation(async (_namespace, _pod, _options, emit) => {
      emit({ event: "line", text: "tiny", container: "app", timestamp: "2026-10-07T12:00:00Z", ordinal: 1 });
      emit({ event, text: "", container: "app", cursor: "terminal-checkpoint", reason: "bytes" });
      emit({ event: "end", text: "", container: "app", cursor: "", replay_uncertain: false });
    });
    const { result, rerender } = renderHook((props) => usePodLogStream(props), { initialProps: options });
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(result.current.status).toBe(event === "limit" ? "limited" : "ended");
    rerender({ ...options, paused: true });
    rerender(options);
    expect(stream).toHaveBeenCalledTimes(1);
    rerender({ ...options, restart: 1 });
    await act(async () => undefined);
    expect(stream).toHaveBeenCalledTimes(2);
    expect(stream.mock.calls[1][2]).not.toHaveProperty("cursor");
  });

  it.each(["total", "duration"] as const)("does not retry a transport %s safety limit", async (code) => {
    vi.useFakeTimers();
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockRejectedValue(new SSELimitError(code));
    const { result } = renderHook(() => usePodLogStream(options));
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(stream).toHaveBeenCalledTimes(1);
    expect(result.current.status).toBe("limited");
    expect(result.current.message).toContain("Reload logs");
  });

  it("bounds identity history without silently dropping unseen or evicted occurrences", async () => {
    vi.useFakeTimers();
    const timestamp = "2026-10-07T12:00:00Z";
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementationOnce(async (_namespace, _pod, _options, emit) => {
      for (let ordinal = 1; ordinal <= 10005; ordinal++) emit({ event: "line", text: "tiny", container: "app", timestamp, ordinal, ...(ordinal % 32 === 0 ? { cursor: `checkpoint-${ordinal}` } : {}) });
    }).mockImplementationOnce(async (_namespace, _pod, _options, emit) => {
      for (const ordinal of [10005, 1, 10006]) emit({ event: "line", text: "tiny", container: "app", timestamp, ordinal });
      emit({ event: "end", text: "", container: "app", cursor: "final" });
    });
    const { result } = renderHook(() => usePodLogStream(options));
    await act(async () => { await vi.advanceTimersByTimeAsync(1100); });
    expect(stream).toHaveBeenCalledTimes(2);
    expect(result.current.buffer.lines).toHaveLength(10000);
    expect(result.current.buffer.nextKey).toBe(10007);
    expect(result.current.buffer.dropped).toBe(7);
    expect(result.current.buffer.bytes).toBeLessThan(POD_LOG_MAX_BYTES);
    expect(result.current.replayUncertain).toBe(true);
  });

  it("fits 10000 tiny checkpointed transport lines in the display buffer without replay tokens", async () => {
    const occurrences: Record<string, number> = {};
    const frames = Array.from({ length: 10000 }, (_, index) => {
      const timestamp = new Date(Date.UTC(2026, 9, 7, 12, 0, 0, index)).toISOString();
      const ordinal = (occurrences[timestamp] ?? 0) + 1;
      occurrences[timestamp] = ordinal;
      if (Object.keys(occurrences).length > 64) delete occurrences[Object.keys(occurrences)[0]];
      const cursor = (index + 1) % 32 === 0 ? btoa(JSON.stringify({ target: "selected-target", timestamp, ordinal, occurrences })) : undefined;
      return `event: line\ndata: ${JSON.stringify({ text: "tiny", container: "app", timestamp, ordinal, sequence: index + 1, cursor, ...(index >= 64 ? { replay_uncertain: true } : {}) })}\n\n`;
    });
    const body = frames.join("") + 'event: end\ndata: {"cursor":"final","replay_uncertain":true}\n\n';
    expect(new TextEncoder().encode(body).byteLength).toBeGreaterThan(2 << 20);
    expect(new TextEncoder().encode(body).byteLength).toBeLessThan(3 << 20);
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(body, { headers: { "Content-Type": "text/event-stream" } }));
    const { result } = renderHook(() => usePodLogStream(options));
    await act(async () => undefined);
    expect(result.current.status).toBe("ended");
    expect(result.current.buffer.lines).toHaveLength(10000);
    expect(result.current.buffer.dropped).toBe(0);
    expect(result.current.buffer.bytes).toBeLessThan(POD_LOG_MAX_BYTES);
    expect(result.current.buffer.lines.every((line) => Object.keys(line).every((key) => ["text", "container", "timestamp", "bytes", "key"].includes(key)))).toBe(true);
  });
});