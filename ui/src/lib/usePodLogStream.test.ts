// @vitest-environment jsdom
import { act, cleanup, renderHook } from "@testing-library/react";
import { createElement, type ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError, api, type KubernetesPodLogEvent, type KubernetesWorkloadLogEvent, type KubernetesWorkloadLogOptions } from "@/lib/api";
import { SSELimitError } from "@/lib/sse";
import {
  appendPodLogLines, earlierOverlap, POD_LOG_CLOCK_SKEW_MS, POD_LOG_MAX_BYTES, POD_LOG_MAX_STALLED_ROTATIONS,
  POD_LOG_WORKLOAD_EARLIER_TAIL, prependEarlierLines, usePodLogStream,
} from "./usePodLogStream";
import { KubernetesClusterContext, kubernetesKey } from "./useKubernetesCluster";

afterEach(() => { cleanup(); vi.restoreAllMocks(); vi.useRealTimers(); });
const options = { namespace: "shop", pod: "api", container: "app", previous: false, since: 3600, tail: 500, paused: false, restart: 0 };
const busyResponse = () => new Response(JSON.stringify({ error: "The concurrent Pod log stream limit has been reached.", code: "log_stream_busy", retryable: true }), { status: 429, headers: { "Retry-After": "2" } });
const completedResponse = (text = "retained output") => new Response(
  `event: line\ndata: ${JSON.stringify({ text, container: "app", pod: "api-0", pod_uid: "pod-uid", source_id: "source", cursor: "checkpoint" })}\n\nevent: end\ndata: {"cursor":"checkpoint"}\n\n`,
  { headers: { "Content-Type": "text/event-stream" } },
);

describe("Pod log stream lifecycle", () => {
  it("discards denied workload-source lines without erasing still-authorized source output", async () => {
    vi.useFakeTimers();
    let emit!: (event: KubernetesWorkloadLogEvent) => void;
    vi.spyOn(api, "kubernetesWorkloadLogStream").mockImplementation(async (_kind, _namespace, _pod, _uid, _options, callback, signal) => {
      emit = callback;
      return new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
    });
    const { result } = renderHook(() => usePodLogStream({ ...options, kind: "Deployment", uid: "root" }));
    act(() => {
      emit({ event: "line", text: "protected", container: "app", pod: "api-0", pod_uid: "denied-pod", source_id: "denied" });
      emit({ event: "line", text: "allowed", container: "app", pod: "api-1", pod_uid: "allowed-pod", source_id: "allowed" });
    });
    await act(async () => { await vi.advanceTimersByTimeAsync(50); });
    act(() => emit({ event: "source_status", text: "", container: "app", code: "forbidden", pod: "api-0", pod_uid: "denied-pod", source_id: "denied" }));
    await act(async () => { await vi.advanceTimersByTimeAsync(50); });
    const lines = result.current.buffer.lines.map((line) => line.text);
    expect(lines).not.toContain("protected");
    expect(lines).toContain("allowed");
  });

  it("rejects callbacks from a retired reconnect attempt without discarding the active read", async () => {
    vi.useFakeTimers();
    let retired!: (event: KubernetesPodLogEvent) => void;
    const stream = vi.spyOn(api, "kubernetesPodLogStream")
      .mockImplementationOnce(async (_namespace, _pod, _options, emit) => { retired = emit; throw new ApiError(502, "interrupted"); })
      .mockImplementationOnce(async (_namespace, _pod, _options, emit, signal) => {
        emit({ event: "line", text: "current", container: "app", cursor: "current-position" });
        return new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
      });
    const { result } = renderHook(() => usePodLogStream(options));
    await act(async () => { await vi.advanceTimersByTimeAsync(1100); });
    act(() => retired({ event: "line", text: "retired", container: "app", cursor: "old-position" }));
    await act(async () => { await vi.advanceTimersByTimeAsync(50); });
    expect(stream).toHaveBeenCalledTimes(2);
    expect(result.current.buffer.lines.map((line) => line.text)).toEqual(["current"]);
  });
  it.each([false, true])("honors permanent credential failure hints without reconnecting (workload: %s)", async (workload) => {
    vi.useFakeTimers();
    const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async () => new Response(JSON.stringify({ error: "Cluster credentials unavailable", code: "credential_unavailable", retryable: false }), { status: 502 }));
    const { result } = renderHook(() => usePodLogStream({ ...options, ...(workload ? { kind: "Deployment", uid: "root" } : {}) }));
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(fetch).toHaveBeenCalledTimes(1);
    expect(result.current.status).toBe("error");
    expect(result.current.message).toContain("credential_unavailable");
  });

  it("recovers transient credential renewal from its resume position within a bounded retry budget", async () => {
    vi.useFakeTimers();
    const fetch = vi.spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(new Response(JSON.stringify({ error: "Cluster credentials unavailable", code: "credential_unavailable", retryable: true }), { status: 502, headers: { "Retry-After": "2" } }))
      .mockResolvedValueOnce(completedResponse("renewed output"));
    const { result } = renderHook(() => usePodLogStream(options));
    await act(async () => { await vi.advanceTimersByTimeAsync(1999); });
    expect(fetch).toHaveBeenCalledTimes(1);
    await act(async () => { await vi.advanceTimersByTimeAsync(100); });
    expect(fetch).toHaveBeenCalledTimes(2);
    expect(result.current.status).toBe("ended");
    expect(result.current.buffer.lines.map((line) => line.text)).toContain("renewed output");
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(fetch).toHaveBeenCalledTimes(2);
  });

  it("bounds repeated short admitted failures even when each connection replays a line", async () => {
    vi.useFakeTimers();
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementation(async (_namespace, _pod, _options, emit) => {
      emit({ event: "line", text: "replayed", container: "app", timestamp: "2026-10-28T00:00:00Z", ordinal: 1 });
      throw new ApiError(502, "renewal unavailable", { code: "credential_unavailable", retryable: true });
    });
    const { result } = renderHook(() => usePodLogStream(options));
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(stream).toHaveBeenCalledTimes(4);
    expect(result.current.status).toBe("error");
    expect(result.current.message).toMatch(/credential_unavailable.*exhausted/);
    expect(result.current.buffer.lines).toEqual([]);
  });
  it("replenishes admission retries after an admitted read later rotates within the same selection", async () => {
    vi.useFakeTimers();
    const encoder = new TextEncoder();
    let liveStream: ReadableStreamDefaultController<Uint8Array> | undefined;
    const admitted = new Response(new ReadableStream<Uint8Array>({
      start(controller) {
        liveStream = controller;
        controller.enqueue(encoder.encode('event: heartbeat\ndata: {"cursor":"admitted"}\n\nevent: line\ndata: {"text":"live output","container":"app","cursor":"line-checkpoint"}\n\n'));
      },
    }), { headers: { "Content-Type": "text/event-stream" } });
    const fetch = vi.spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(busyResponse())
      .mockResolvedValueOnce(admitted)
      .mockResolvedValueOnce(busyResponse())
      .mockResolvedValueOnce(completedResponse("after rotation"));
    const { result } = renderHook(() => usePodLogStream(options));
    await act(async () => { await vi.advanceTimersByTimeAsync(2050); });
    expect(fetch).toHaveBeenCalledTimes(2);
    expect(result.current.status).toBe("live");
    expect(result.current.buffer.lines.map((line) => line.text)).toEqual(["live output"]);
    await act(async () => { await vi.advanceTimersByTimeAsync(11_000); });
    await act(async () => {
      liveStream?.enqueue(encoder.encode('event: limit\ndata: {"reason":"duration","cursor":"rotation-checkpoint"}\n\n'));
      liveStream?.close();
      await vi.advanceTimersByTimeAsync(1);
    });
    expect(fetch).toHaveBeenCalledTimes(3);
    expect(result.current.status).toBe("reconnecting");
    expect(result.current.message).not.toContain("exhausted");
    await act(async () => { await vi.advanceTimersByTimeAsync(2000); });
    expect(fetch).toHaveBeenCalledTimes(4);
    expect(new URL(String(fetch.mock.calls[3][0]), "http://localhost").searchParams.get("cursor")).toBe("rotation-checkpoint");
    expect(result.current.buffer.lines.map((line) => line.text)).toEqual(["live output", "after rotation"]);
    expect(result.current.status).toBe("ended");
  });

  it("does not retry the backend invalid arguments event even if marked retryable", async () => {
    vi.useFakeTimers();
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementation(async (_namespace, _pod, _options, emit) => {
      emit({ event: "error", text: "", container: "app", code: "invalid_arguments", message: "Invalid log arguments.", retryable: true });
    });
    const { result } = renderHook(() => usePodLogStream(options));
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(stream).toHaveBeenCalledTimes(1);
    expect(result.current.status).toBe("error");
    expect(result.current.message).toContain("invalid_arguments");
  });

  it.each([
    { previous: false, workload: false }, { previous: true, workload: false },
    { previous: false, workload: true }, { previous: true, workload: true },
  ])("retries busy admission then retains a finite success for %j", async ({ previous, workload }) => {
    vi.useFakeTimers();
    const fetch = vi.spyOn(globalThis, "fetch").mockResolvedValueOnce(busyResponse()).mockResolvedValueOnce(completedResponse());
    const { result } = renderHook(() => usePodLogStream({ ...options, previous, ...(workload ? { kind: "Deployment", uid: "workload-uid" } : {}) }));
    await act(async () => { await vi.advanceTimersByTimeAsync(1999); });
    expect(fetch).toHaveBeenCalledTimes(1);
    expect(result.current.message).toContain("The concurrent Pod log stream limit");
    expect(result.current.status).toBe("reconnecting");
    await act(async () => { await vi.advanceTimersByTimeAsync(1); });
    expect(fetch).toHaveBeenCalledTimes(2);
    expect(result.current.status).toBe("ended");
    expect(result.current.buffer.lines.map((line) => line.text)).toEqual(["retained output"]);
    expect(result.current.buffer.dropped).toBe(0);
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(fetch).toHaveBeenCalledTimes(2);
  });

  it.each([false, true])("exhausts busy admission across pause/resume and resets only a new selection (previous=%s)", async (previous) => {
    vi.useFakeTimers();
    const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async () => busyResponse());
    const { result, rerender } = renderHook((props) => usePodLogStream(props), { initialProps: { ...options, previous } });
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(fetch).toHaveBeenCalledTimes(4);
    expect(result.current.status).toBe("error");
    expect(result.current.message).toContain("The concurrent Pod log stream limit");
    expect(result.current.message).toContain("Admission retry attempts exhausted");
    rerender({ ...options, previous, paused: true });
    rerender({ ...options, previous });
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(fetch).toHaveBeenCalledTimes(4);
    rerender({ ...options, previous, container: "sidecar" });
    await act(async () => { await vi.advanceTimersByTimeAsync(6000); });
    expect(fetch).toHaveBeenCalledTimes(8);
  });

  it.each([401, 403, 400, 422, 404, 502].flatMap((status) => [false, true].map((previous) => ({ status, previous }))))("does not retry auth, invalid arguments or unavailable Previous (%j)", async ({ status, previous }) => {
    vi.useFakeTimers();
    const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async () => new Response(JSON.stringify({
      error: status >= 404 && status !== 422 ? { code: "previous_unavailable", message: "No previous container exists." } : "Request is not allowed.",
    }), { status }));
    const { result } = renderHook(() => usePodLogStream({ ...options, previous }));
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(fetch).toHaveBeenCalledTimes(1);
    expect(result.current.status).toBe("error");
  });

  it("does not replenish the admission retry window on pause or retry a genuine missing Previous event", async () => {
    vi.useFakeTimers();
    const stream = vi.spyOn(api, "kubernetesPodLogStream")
      .mockRejectedValue(new ApiError(429, "Slots are busy.", { code: "log_stream_busy" }));
    const { result, rerender } = renderHook((props) => usePodLogStream(props), { initialProps: options });
    await act(async () => { await vi.advanceTimersByTimeAsync(100); });
    rerender({ ...options, paused: true });
    await act(async () => { await vi.advanceTimersByTimeAsync(11_000); });
    rerender(options);
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(stream).toHaveBeenCalledTimes(2);
    expect(result.current.message).toContain("Admission retry attempts exhausted");
    stream.mockImplementation(async (_namespace, _pod, _options, emit) => {
      emit({ event: "error", text: "", container: "app", code: "previous_unavailable", retryable: true });
    });
    rerender({ ...options, previous: true });
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(stream).toHaveBeenCalledTimes(3);
    expect(result.current.message).toContain("no previous container instance");
  });

  it("schedules one busy retry when a terminal SSE event is followed by a reader failure", async () => {
    vi.useFakeTimers();
    const stream = vi.spyOn(api, "kubernetesPodLogStream")
      .mockImplementationOnce(async (_namespace, _pod, _options, emit) => {
        emit({ event: "error", text: "", container: "app", code: "log_stream_busy", message: "Slots are busy.", retryable: true });
        throw new ApiError(502, "reader closed");
      })
      .mockImplementation(async (_namespace, _pod, _options, emit) => {
        emit({ event: "end", text: "", container: "app" });
      });
    const { result } = renderHook(() => usePodLogStream({ ...options, previous: true }));
    await act(async () => { await vi.advanceTimersByTimeAsync(499); });
    expect(stream).toHaveBeenCalledTimes(1);
    expect(result.current.message).toContain("Slots are busy.");
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(stream).toHaveBeenCalledTimes(2);
    expect(result.current.status).toBe("ended");
  });

  it("cancels admission timers and fetches on rapid changes and ignores a stale response after switching", async () => {
    vi.useFakeTimers();
    let resolveStale: (response: Response) => void = () => undefined;
    const fetch = vi.spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(busyResponse())
      .mockImplementationOnce(() => new Promise<Response>((resolve) => { resolveStale = resolve; }))
      .mockResolvedValueOnce(completedResponse("new selection"))
      .mockResolvedValueOnce(busyResponse());
    const { result, rerender, unmount } = renderHook((props) => usePodLogStream(props), { initialProps: options });
    await act(async () => { await vi.advanceTimersByTimeAsync(100); });
    rerender({ ...options, previous: true });
    const staleSignal = fetch.mock.calls[1][1]?.signal;
    rerender({ ...options, container: "sidecar" });
    expect(staleSignal?.aborted).toBe(true);
    await act(async () => {
      resolveStale(completedResponse("stale output"));
      await vi.advanceTimersByTimeAsync(60_000);
    });
    expect(fetch).toHaveBeenCalledTimes(3);
    expect(result.current.buffer.lines.map((line) => line.text)).toEqual(["new selection"]);
    expect(result.current.status).toBe("ended");
    rerender({ ...options, container: "other" });
    await act(async () => { await vi.advanceTimersByTimeAsync(100); });
    const lastSignal = fetch.mock.calls[3][1]?.signal;
    unmount();
    expect(lastSignal?.aborted).toBe(true);
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(fetch).toHaveBeenCalledTimes(4);
  });

  it.each(["", "quiet-checkpoint"])("quietly rotates a never-written Pod at its duration limit with cursor %s", async (cursor) => {
    vi.useFakeTimers();
    let secondEmit: (event: KubernetesPodLogEvent) => void = () => undefined;
    const stream = vi.spyOn(api, "kubernetesPodLogStream")
      .mockImplementationOnce(async (_namespace, _pod, _options, emit) => {
        emit({ event: "heartbeat", text: "", container: "app" });
        emit({ event: "limit", text: "", container: "app", reason: "duration", cursor });
        emit({ event: "end", text: "", container: "app", cursor });
      })
      .mockImplementation((_namespace, _pod, _options, emit, signal) => {
        secondEmit = emit;
        return new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
      });
    const { result, unmount } = renderHook(() => usePodLogStream(options));
    await act(async () => { await vi.advanceTimersByTimeAsync(1); });
    expect(stream).toHaveBeenCalledTimes(2);
    expect(stream.mock.calls[1][2]).toEqual(cursor
      ? { container: "app", previous: false, timestamps: false, cursor }
      : { container: "app", previous: false, timestamps: false, since_seconds: 3600, tail_lines: 500 });
    expect(result.current.status).toBe("live");
    expect(result.current.message).toBe("");
    expect(result.current.buffer.lines).toHaveLength(0);
    act(() => secondEmit({ event: "line", text: "first output", container: "app", timestamp: "2026-10-21T08:00:00Z", ordinal: 1, cursor: "first-line" }));
    await act(async () => { await vi.advanceTimersByTimeAsync(50); });
    expect(result.current.buffer.lines.map((line) => line.text)).toEqual(["first output"]);
    unmount();
    expect(stream.mock.calls[1][4].aborted).toBe(true);
  });
  it("aborts and clears buffers and cursors when identical Pod targets switch clusters", async () => {
    let emit: (event: KubernetesPodLogEvent) => void = () => undefined;
    const stream = vi.fn<typeof api.kubernetesPodLogStream>((_namespace, _pod, _options, callback, signal) => {
      emit = callback;
      return new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
    });
    const client = { ...api, kubernetesPodLogStream: stream };
    let cluster = "one";
    const wrapper = ({ children }: { children: ReactNode }) => createElement(KubernetesClusterContext.Provider, { value: { multiple: true, cluster, clusters: [], client, key: (...parts) => kubernetesKey(cluster, ...parts) } }, children);
    const { result, rerender, unmount } = renderHook(() => usePodLogStream(options), { wrapper });
    act(() => emit({ event: "line", text: "from-one", container: "app", cursor: "one-cursor" }));
    await act(async () => undefined);
    const firstSignal = stream.mock.calls[0][4];
    cluster = "two";
    rerender();
    expect(firstSignal.aborted).toBe(true);
    expect(stream).toHaveBeenCalledTimes(2);
    expect(stream.mock.calls[1][2]).not.toHaveProperty("cursor");
    expect(result.current.buffer.lines).toHaveLength(0);
    act(() => emit({ event: "line", text: "from-two", container: "app", cursor: "two-cursor" }));
    unmount();
    expect(stream.mock.calls[1][4].aborted).toBe(true);
  });

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

  it("keeps the backend notice for a stale inventory and offers the reader a retry", async () => {
    vi.useFakeTimers();
    const inventoryNotReady = new ApiError(503, "ReplicaSet inventory is 4 min old; retry once the inventory finishes.", { error: "ReplicaSet inventory is 4 min old; retry once the inventory finishes.", code: "inventory_not_ready", action: "Wait for the Kubernetes inventory to finish syncing and retry.", retryable: true });
    const stream = vi.spyOn(api, "kubernetesWorkloadLogStream").mockRejectedValue(inventoryNotReady);
    const { result } = renderHook(() => usePodLogStream({ ...options, pod: "checkout", kind: "Deployment", uid: "deployment-uid" }));
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(result.current.status).toBe("error");
    expect(result.current.message).toContain("ReplicaSet inventory is 4 min old");
    expect(result.current.notice).toEqual({ action: "Wait for the Kubernetes inventory to finish syncing and retry.", retryable: true });
    // The guidance is the backend's; a reader must never invent permission advice.
    expect(result.current.notice.action).not.toMatch(/RBAC|Grant/);
    expect(stream.mock.calls.length).toBeGreaterThan(1);
  });

  it("keeps a real cluster denial as the only place permission guidance comes from", async () => {
    vi.useFakeTimers();
    const denied = new ApiError(403, "Kubernetes denied get on pods/log in namespace vikki-mobile-dev for user arn:aws:sts::1:assumed-role/admin", { error: "Kubernetes denied get on pods/log in namespace vikki-mobile-dev for user arn:aws:sts::1:assumed-role/admin", code: "cluster_permission_denied", action: "Grant the connector identity the required read-only Kubernetes RBAC permissions.", retryable: false });
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockRejectedValue(denied);
    const { result } = renderHook(() => usePodLogStream(options));
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(result.current.status).toBe("error");
    expect(result.current.message).toContain("pods/log in namespace vikki-mobile-dev");
    expect(result.current.notice).toEqual({ action: "Grant the connector identity the required read-only Kubernetes RBAC permissions.", retryable: false });
    expect(stream).toHaveBeenCalledTimes(1);
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

  it("keeps an ended read ended across pause and resume while a new selection starts clean", async () => {
    vi.useFakeTimers();
    const timestamp = (index: number) => new Date(Date.UTC(2026, 9, 7, 12, 0, index)).toISOString();
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementationOnce(async (_namespace, _pod, _options, emit) => {
      for (let index = 1; index <= 95; index++) emit({ event: "line", text: "same", container: "app", timestamp: timestamp(index), ordinal: 1, sequence: index, ...(index % 32 === 0 ? { cursor: `checkpoint-${index}` } : {}), replay_uncertain: index === 65 });
    }).mockImplementationOnce(async (_namespace, _pod, _options, emit) => {
      for (let index = 65; index <= 95; index++) emit({ event: "line", text: "same", container: "app", timestamp: timestamp(index), ordinal: 1, sequence: index - 64 });
      emit({ event: "line", text: "same", container: "app", timestamp: timestamp(95), ordinal: 2, sequence: 32 });
      emit({ event: "end", text: "", container: "app", cursor: "final", replay_uncertain: false });
    }).mockResolvedValue();
    const { result, rerender } = renderHook((props) => usePodLogStream(props), { initialProps: options });
    await act(async () => { await vi.advanceTimersByTimeAsync(1100); });
    expect(stream.mock.calls[1][2].cursor).toBe("checkpoint-64");
    expect(result.current.buffer.lines.filter((line) => !line.neutral)).toHaveLength(96);
    expect(result.current.buffer.lines.filter((line) => line.neutral)).toHaveLength(1);
    expect(result.current.replayUncertain).toBe(true);
    expect(result.current.status).toBe("ended");
    rerender({ ...options, paused: true });
    rerender(options);
    expect(result.current.replayUncertain).toBe(true);
    // Resuming cannot reopen a container that has stopped writing.
    expect(result.current.status).toBe("ended");
    expect(stream).toHaveBeenCalledTimes(2);
    expect(result.current.buffer.lines.filter((line) => !line.neutral)).toHaveLength(96);
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
    expect(result.current.buffer.lines.filter((line) => !line.neutral)).toHaveLength(1);
  });

  it("keeps an ended stream ended on resume and only a reload starts a new read", async () => {
    vi.useFakeTimers();
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementation(async (_namespace, _pod, _options, emit) => {
      emit({ event: "line", text: "tiny", container: "app", timestamp: "2026-10-07T12:00:00Z", ordinal: 1, cursor: "terminal-checkpoint" });
      if (stream.mock.calls.length > 1) emit({ event: "line", text: "new output", container: "app", timestamp: "2026-10-07T12:00:00Z", ordinal: 2, cursor: "next-checkpoint" });
      emit({ event: "end", text: "", container: "app", cursor: "terminal-checkpoint" });
      emit({ event: "end", text: "", container: "app", cursor: "", replay_uncertain: false });
    });
    const { result, rerender } = renderHook((props) => usePodLogStream(props), { initialProps: options });
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(result.current.status).toBe("ended");
    expect(stream).toHaveBeenCalledTimes(1);
    rerender({ ...options, paused: true });
    expect(result.current.status).toBe("paused");
    rerender(options);
    await act(async () => undefined);
    expect(stream).toHaveBeenCalledTimes(1);
    expect(result.current.status).toBe("ended");
    expect(result.current.buffer.lines.map((line) => line.text)).toEqual(["tiny"]);
    expect(result.current.message).not.toMatch(/cannot resume|gap|uncertain/);
    rerender({ ...options, restart: 1 });
    await act(async () => undefined);
    expect(stream).toHaveBeenCalledTimes(2);
    expect(stream.mock.calls[1][2]).not.toHaveProperty("cursor");
    await act(async () => { await vi.advanceTimersByTimeAsync(50); });
    expect(result.current.buffer.lines.map((line) => line.text)).toEqual(["tiny", "new output"]);
  });
  it.each([1, 31])("resumes after pausing %s lines before a heartbeat without losing or duplicating retained output", async (count) => {
    vi.useFakeTimers();
    const timestamp = "2026-10-21T08:00:00Z";
    const stream = vi.spyOn(api, "kubernetesPodLogStream")
      .mockImplementationOnce((_namespace, _pod, _options, emit, signal) => {
        for (let ordinal = 1; ordinal <= count; ordinal++) emit({ event: "line", text: `line ${ordinal}`, container: "app", timestamp, ordinal, ...(ordinal === 1 ? { cursor: "first-checkpoint" } : {}) });
        return new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
      })
      .mockImplementation((_namespace, _pod, _options, emit, signal) => {
        for (let ordinal = 1; ordinal <= count + 1; ordinal++) emit({ event: "line", text: `line ${ordinal}`, container: "app", timestamp, ordinal, cursor: "progress-checkpoint" });
        return new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
      });
    const { result, rerender, unmount } = renderHook((props) => usePodLogStream(props), { initialProps: options });
    rerender({ ...options, paused: true });
    expect(stream.mock.calls[0][4].aborted).toBe(true);
    expect(result.current.buffer.lines.map((line) => line.text)).toEqual(Array.from({ length: count }, (_, index) => `line ${index + 1}`));
    rerender(options);
    expect(stream.mock.calls[1][2]).toEqual({ container: "app", previous: false, timestamps: false, cursor: "first-checkpoint" });
    await act(async () => { await vi.advanceTimersByTimeAsync(50); });
    expect(result.current.buffer.lines.map((line) => line.text)).toEqual(Array.from({ length: count + 1 }, (_, index) => `line ${index + 1}`));
    expect(result.current.status).toBe("live");
    expect(result.current.message).toBe("");
    expect(result.current.replayUncertain).toBe(false);
    unmount();
    expect(stream.mock.calls[1][4].aborted).toBe(true);
  });
  it("keeps missing Previous terminal on Pause and Resume without inventing a reconnect divider", async () => {
    vi.useFakeTimers();
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementation(async (_namespace, _pod, _options, emit) => {
      emit({ event: "error", text: "", container: "app", code: "previous_unavailable", message: "Previous container logs are unavailable.", action: "Select current container logs.", replay_uncertain: true });
      emit({ event: "end", text: "", container: "app" });
    });
    const previousOptions = { ...options, previous: true };
    const { result, rerender } = renderHook((props) => usePodLogStream(props), { initialProps: previousOptions });
    await act(async () => { await vi.advanceTimersByTimeAsync(50); });
    expect(result.current.status).toBe("error");
    expect(result.current.message).toContain("previous_unavailable Previous container logs are unavailable. Select current container logs.");
    expect(result.current.buffer.lines).toHaveLength(0);
    expect(result.current.replayUncertain).toBe(false);
    rerender({ ...previousOptions, paused: true });
    rerender(previousOptions);
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(stream).toHaveBeenCalledTimes(1);
    expect(result.current.status).toBe("error");
    expect(result.current.buffer.lines).toHaveLength(0);
  });
  it.each(["forbidden", "upstream_unavailable"])("does not describe an initial %s error as a resumed connection", async (code) => {
    vi.useFakeTimers();
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementation(async (_namespace, _pod, _options, emit) => {
      emit({ event: "error", text: "", container: "app", code, message: "Logs are unavailable.", retryable: false, replay_uncertain: true });
      emit({ event: "end", text: "", container: "app" });
    });
    const { result } = renderHook(() => usePodLogStream(options));
    await act(async () => { await vi.advanceTimersByTimeAsync(60_000); });
    expect(stream).toHaveBeenCalledTimes(1);
    expect(result.current.status).toBe("error");
    expect(result.current.message).toContain(`${code} Logs are unavailable.`);
    expect(result.current.replayUncertain).toBe(false);
    expect(result.current.buffer.lines).toHaveLength(0);
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
      return `event: line\ndata: ${JSON.stringify({ text: "tiny", container: "app", timestamp, ordinal, sequence: index + 1, cursor, ...(index === 64 ? { replay_uncertain: true } : {}) })}\n\n`;
    });
    const body = frames.join("") + 'event: end\ndata: {"cursor":"final"}\n\n';
    expect(new TextEncoder().encode(body).byteLength).toBeGreaterThan(1 << 20);
    expect(new TextEncoder().encode(body).byteLength).toBeLessThan(3 << 20);
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(body, { headers: { "Content-Type": "text/event-stream" } }));
    const { result } = renderHook(() => usePodLogStream(options));
    await act(async () => undefined);
    expect(result.current.status).toBe("ended");
    expect(result.current.buffer.lines).toHaveLength(10000);
    expect(result.current.buffer.dropped).toBe(1);
    expect(result.current.buffer.bytes).toBeLessThan(POD_LOG_MAX_BYTES);
    expect(result.current.buffer.lines.every((line) => Object.keys(line).every((key) => ["text", "container", "timestamp", "bytes", "key", "neutral"].includes(key)))).toBe(true);
  });

  it("resumes a 60-Pod workload from a wall-clock time and never puts a cursor in the URL", async () => {
    vi.useFakeTimers();
    const sources = Array.from({ length: 60 }, (_, index) => `pod-${index}`);
    const startedAt = Date.parse("2026-10-26T12:00:00Z");
    let attempt = 0;
    const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async () => {
      attempt++;
      const body = sources.map((pod, index) => {
        const timestamp = new Date(startedAt + attempt * 60_000 + index).toISOString();
        const payload = { text: `${pod} served ${attempt}`, container: "app", pod, pod_uid: `${pod}-uid`, source_id: `${pod}/app`, timestamp, ordinal: 1 };
        return `event: line\ndata: ${JSON.stringify(payload)}\n\n`;
      }).join("");
      // A duration limit rotates the read the way a long follow does.
      return new Response(body + (attempt < 4 ? 'event: limit\ndata: {"reason":"duration"}\n\n' : 'event: end\ndata: {}\n\n'), { headers: { "Content-Type": "text/event-stream" } });
    });
    const { result } = renderHook(() => usePodLogStream({ ...options, pod: "checkout", kind: "Deployment", uid: "workload-uid", previous: false, since: undefined, tail: 200 }));
    await act(async () => { await vi.advanceTimersByTimeAsync(100); });
    expect(fetch).toHaveBeenCalledTimes(4);
    const urls = fetch.mock.calls.map((call) => new URL(String(call[0]), "http://localhost"));
    // A wall-clock resume keeps the request line tiny however wide the workload is.
    for (const url of urls) {
      expect(decodeURIComponent(url.pathname + url.search).length).toBeLessThan(2048);
      expect(url.searchParams.get("cursor")).toBeNull();
    }
    expect(urls[0].searchParams.get("since_time")).toBeNull();
    expect(urls[0].searchParams.get("tail_lines")).toBe("200");
    for (const url of urls.slice(1)) expect(url.searchParams.get("since_time")).toMatch(/^\d{4}-\d{2}-\d{2}T/);
    const texts = result.current.buffer.lines.map((line) => line.text);
    expect(texts).toHaveLength(sources.length * 4);
    expect(texts).toHaveLength(new Set(texts).size);
  });

  it("asks the first reconnect from the laggiest source of a far-apart workload minus one second", async () => {
    vi.useFakeTimers();
    const now = Date.parse("2026-10-26T12:00:00Z");
    vi.spyOn(Date, "now").mockReturnValue(now);
    const newest = new Date(now - 60 * 60 * 1000).toISOString();
    const laggiest = new Date(now - 3 * 60 * 60 * 1000).toISOString();
    let attempt = 0;
    const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async () => {
      attempt++;
      const body = attempt === 1
        ? [
            { text: "fast newest", container: "app", pod: "pod-fast", pod_uid: "uid-fast", source_id: "pod-fast/app", timestamp: newest, ordinal: 1 },
            { text: "slow newest", container: "app", pod: "pod-slow", pod_uid: "uid-slow", source_id: "pod-slow/app", timestamp: laggiest, ordinal: 1 },
          ].map((payload) => `event: line\ndata: ${JSON.stringify(payload)}\n\n`).join("")
          + 'event: limit\ndata: {"reason":"duration"}\n\n'
        : 'event: end\ndata: {}\n\n';
      return new Response(body, { headers: { "Content-Type": "text/event-stream" } });
    });
    renderHook(() => usePodLogStream({ ...options, pod: "checkout", kind: "Deployment", uid: "workload-uid", previous: false, since: undefined, tail: 200 }));
    await act(async () => { await vi.advanceTimersByTimeAsync(100); });
    expect(fetch).toHaveBeenCalledTimes(2);
    // The opening read has no resume point; the reconnect names the laggiest
    // source minus the one-second replay overlap, not the newest source.
    expect(new URL(String(fetch.mock.calls[0][0]), "http://localhost").searchParams.get("since_time")).toBeNull();
    expect(new URL(String(fetch.mock.calls[1][0]), "http://localhost").searchParams.get("since_time"))
      .toBe(new Date(Date.parse(laggiest) - 1000).toISOString());
  });

  it("clamps a workload resume to the readable 23-hour floor when the laggiest source is older", async () => {
    vi.useFakeTimers();
    const now = Date.parse("2026-10-26T12:00:00Z");
    vi.spyOn(Date, "now").mockReturnValue(now);
    const newest = new Date(now - 60 * 60 * 1000).toISOString();
    const laggiest = new Date(now - 30 * 60 * 60 * 1000).toISOString();
    let attempt = 0;
    const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async () => {
      attempt++;
      const body = attempt === 1
        ? [
            { text: "fast newest", container: "app", pod: "pod-fast", pod_uid: "uid-fast", source_id: "pod-fast/app", timestamp: newest, ordinal: 1 },
            { text: "stalled newest", container: "app", pod: "pod-stalled", pod_uid: "uid-stalled", source_id: "pod-stalled/app", timestamp: laggiest, ordinal: 1 },
          ].map((payload) => `event: line\ndata: ${JSON.stringify(payload)}\n\n`).join("")
          + 'event: limit\ndata: {"reason":"duration"}\n\n'
        : 'event: end\ndata: {}\n\n';
      return new Response(body, { headers: { "Content-Type": "text/event-stream" } });
    });
    renderHook(() => usePodLogStream({ ...options, pod: "checkout", kind: "Deployment", uid: "workload-uid", previous: false, since: undefined, tail: 200 }));
    await act(async () => { await vi.advanceTimersByTimeAsync(100); });
    expect(fetch).toHaveBeenCalledTimes(2);
    expect(new URL(String(fetch.mock.calls[1][0]), "http://localhost").searchParams.get("since_time"))
      .toBe(new Date(now - 23 * 60 * 60 * 1000).toISOString());
  });

  it("drops replayed workload lines by identity on every source across a reconnect", async () => {
    vi.useFakeTimers();
    const at = (second: number) => new Date(Date.UTC(2026, 9, 26, 12, 0, second)).toISOString();
    const line = (pod: string, text: string, second: number, ordinal: number) => ({ text, container: "app", pod, pod_uid: `${pod}-uid`, source_id: `${pod}/app`, timestamp: at(second), ordinal });
    let attempt = 0;
    const body = (lines: ReturnType<typeof line>[], tailEvent: string) =>
      lines.map((payload) => `event: line\ndata: ${JSON.stringify(payload)}\n\n`).join("") + tailEvent;
    const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async () => {
      attempt++;
      const response = attempt === 1
        ? body([line("pod-a", "a1", 1, 1), line("pod-b", "b1", 2, 1), line("pod-a", "a2", 3, 2), line("pod-b", "b2", 4, 2)], 'event: limit\ndata: {"reason":"duration"}\n\n')
        // The reconnect replays the since window: both sealed lines arrive again
        // with the same source, timestamp and ordinal, so identity must drop them.
        : body([line("pod-a", "a2", 3, 2), line("pod-b", "b2", 4, 2), line("pod-a", "a3", 5, 3), line("pod-b", "b3", 6, 3)], 'event: end\ndata: {}\n\n');
      return new Response(response, { headers: { "Content-Type": "text/event-stream" } });
    });
    const { result } = renderHook(() => usePodLogStream({ ...options, pod: "checkout", kind: "Deployment", uid: "workload-uid", previous: false, since: undefined, tail: 200 }));
    await act(async () => { await vi.advanceTimersByTimeAsync(100); });
    expect(fetch).toHaveBeenCalledTimes(2);
    const texts = result.current.buffer.lines.filter((item) => !item.neutral).map((item) => item.text);
    expect(texts).toEqual(["a1", "b1", "a2", "b2", "a3", "b3"]);
    expect(texts).toHaveLength(new Set(texts).size);
    expect(result.current.status).toBe("ended");
  });

  it("loads an earlier window without touching the live read", async () => {
    vi.useFakeTimers();
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementation((_namespace, _pod, _options, emit, signal) => {
      emit({ event: "line", text: "newest", container: "app", timestamp: "2026-10-26T12:00:00Z", ordinal: 9, cursor: "live" });
      return new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
    });
    const earlier = vi.spyOn(api, "kubernetesPodLogs").mockResolvedValue({ cluster_id: "production", namespace: "shop", pod: "api", container: "app", previous: false, since_seconds: 86400, tail_lines: 5000, text: "oldest\nolder\nnewest", truncated: false });
    const { result } = renderHook(() => usePodLogStream({ ...options, since: undefined, tail: 200 }));
    await act(async () => { await vi.advanceTimersByTimeAsync(50); });
    expect(result.current.buffer.lines.map((line) => line.text)).toEqual(["newest"]);
    await act(async () => { await result.current.loadEarlier(86400); });
    expect(earlier).toHaveBeenCalledWith("shop", "api", expect.objectContaining({ since_seconds: 86400, tail_lines: 5000 }));
    // The seam line repeats in both reads and is kept once.
    expect(result.current.buffer.lines.map((line) => line.text)).toEqual(["oldest", "older", "newest"]);
    expect(stream).toHaveBeenCalledTimes(1);
  });

  it("keeps the earlier read bound to its own selection", async () => {
    vi.useFakeTimers();
    vi.spyOn(api, "kubernetesPodLogStream").mockImplementation((_namespace, _pod, _options, emit, signal) => {
      emit({ event: "line", text: "live", container: "app" });
      return new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
    });
    type History = Awaited<ReturnType<typeof api.kubernetesPodLogs>>;
    let release: (value: History) => void = () => undefined;
    vi.spyOn(api, "kubernetesPodLogs").mockImplementation(() => new Promise<History>((resolve) => { release = resolve; }));
    const { result, rerender } = renderHook((props) => usePodLogStream(props), { initialProps: { ...options, since: undefined, tail: 200 } });
    await act(async () => { await vi.advanceTimersByTimeAsync(50); });
    let loading: Promise<void> = Promise.resolve();
    act(() => { loading = result.current.loadEarlier(3600); });
    rerender({ ...options, since: undefined, tail: 200, container: "setup" });
    await act(async () => {
      release({ cluster_id: "production", namespace: "shop", pod: "api", container: "app", previous: false, since_seconds: 3600, tail_lines: 5000, text: "from another container", truncated: false });
      await loading;
      await vi.advanceTimersByTimeAsync(50);
    });
    expect(result.current.buffer.lines.map((line) => line.text)).toEqual(["live"]);
  });

  it("lands a burst of lines in one animation frame instead of one render per line", async () => {
    vi.useFakeTimers();
    vi.spyOn(api, "kubernetesPodLogStream").mockImplementation((_namespace, _pod, _options, emit, signal) => {
      for (let ordinal = 1; ordinal <= 20; ordinal++) emit({ event: "line", text: `line ${ordinal}`, container: "app", timestamp: "2026-10-26T12:00:00Z", ordinal });
      return new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
    });
    const { result } = renderHook(() => usePodLogStream({ ...options, since: undefined, tail: 200 }));
    // Nothing renders until the frame.
    expect(result.current.buffer.lines).toHaveLength(0);
    await act(async () => { await vi.advanceTimersByTimeAsync(16); });
    expect(result.current.buffer.lines).toHaveLength(20);
    expect(result.current.status).toBe("live");
  });

  it("rotates a live Pod read from its newest line when the terminal limit carries no cursor", async () => {
    vi.useFakeTimers();
    const at = (second: number) => new Date(Date.UTC(2026, 9, 26, 12, 0, second)).toISOString();
    let attempt = 0;
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementation(async (_namespace, _pod, _options, emit) => {
      attempt++;
      if (attempt === 1) {
        emit({ event: "line", text: "line 1", container: "app", timestamp: at(1), ordinal: 1 });
        emit({ event: "line", text: "line 2", container: "app", timestamp: at(2), ordinal: 2 });
        emit({ event: "line", text: "line 3", container: "app", timestamp: at(3), ordinal: 3 });
        // The resume state outgrew the request budget, so the server hands back
        // no cursor at all and only the lines carry a position.
        emit({ event: "limit", text: "", container: "app", reason: "duration" });
        return;
      }
      emit({ event: "line", text: "line 4", container: "app", timestamp: at(4), ordinal: 4 });
      emit({ event: "end", text: "", container: "app" });
    });
    const { result } = renderHook(() => usePodLogStream(options));
    await act(async () => { await vi.advanceTimersByTimeAsync(50); });
    expect(stream).toHaveBeenCalledTimes(2);
    expect(stream.mock.calls[1][2]).toEqual({
      container: "app", previous: false, timestamps: false,
      since_time: new Date(Date.parse(at(3)) - 1000).toISOString(),
    });
    expect(result.current.buffer.lines.map((line) => line.text)).toEqual(["line 1", "line 2", "line 3", "line 4"]);
  });

  it("prefers the newest line time over a terminal cursor that lags a large gap behind it", async () => {
    vi.useFakeTimers();
    const at = (minute: number) => new Date(Date.UTC(2026, 9, 26, 10, minute, 0)).toISOString();
    const staleCursor = btoa(JSON.stringify({ target: "selected-target", timestamp: at(0), ordinal: 1 }));
    let attempt = 0;
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementation(async (_namespace, _pod, _options, emit) => {
      attempt++;
      if (attempt === 1) {
        emit({ event: "line", text: "served", container: "app", timestamp: at(30), ordinal: 1 });
        emit({ event: "limit", text: "", container: "app", reason: "duration", cursor: staleCursor });
        return;
      }
      emit({ event: "end", text: "", container: "app" });
    });
    renderHook(() => usePodLogStream(options));
    await act(async () => { await vi.advanceTimersByTimeAsync(50); });
    expect(stream).toHaveBeenCalledTimes(2);
    // Resuming from the cursor would replay half an hour the reader already has.
    expect(stream.mock.calls[1][2].since_time).toBe(new Date(Date.parse(at(30)) - 1000).toISOString());
    expect(stream.mock.calls[1][2]).not.toHaveProperty("cursor");
  });

  it("clamps a resume time from a fast container clock to current wall time", async () => {
    vi.useFakeTimers();
    const now = Date.parse("2026-10-26T12:00:00Z");
    vi.spyOn(Date, "now").mockReturnValue(now);
    const ahead = new Date(now + 10 * 60 * 1000).toISOString();
    let attempt = 0;
    const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async () => {
      attempt++;
      const body = attempt === 1
        ? `event: line\ndata: ${JSON.stringify({ text: "ahead", container: "app", pod: "pod-a", pod_uid: "uid-a", source_id: "pod-a/app", timestamp: ahead, ordinal: 1 })}\n\nevent: limit\ndata: {"reason":"duration"}\n\n`
        : 'event: end\ndata: {}\n\n';
      return new Response(body, { headers: { "Content-Type": "text/event-stream" } });
    });
    renderHook(() => usePodLogStream({ ...options, pod: "checkout", kind: "Deployment", uid: "workload-uid", since: undefined, tail: 200 }));
    await act(async () => { await vi.advanceTimersByTimeAsync(100); });
    expect(fetch).toHaveBeenCalledTimes(2);
    const since = new URL(String(fetch.mock.calls[1][0]), "http://localhost").searchParams.get("since_time")!;
    // The read still resumes, from a time the server accepts, instead of being
    // refused for asking about lines that cannot exist yet.
    expect(Date.parse(since)).toBe(now + POD_LOG_CLOCK_SKEW_MS);
  });

  it("reads a workload history page inside the bound the backend enforces", async () => {
    vi.useFakeTimers();
    vi.spyOn(api, "kubernetesWorkloadLogStream").mockImplementation((_kind, _namespace, _pod, _uid, _options, emit, signal) => {
      emit({ event: "line", text: "newest", container: "app", pod: "api-0", pod_uid: "pod-uid-0", source_id: "source-0", timestamp: "2026-10-26T12:00:00Z", ordinal: 1 });
      return new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
    });
    const history = vi.spyOn(api, "kubernetesWorkloadLogs").mockImplementation(async (_kind, _namespace, _pod, request) => {
      // The backend refuses a workload page over one thousand lines outright.
      if ((request.tail_lines ?? 0) > 1_000) {
        throw new ApiError(400, "Kubernetes request is not valid.", { error: { code: "invalid_arguments" } });
      }
      return { pods: ["api-0"], lines: [{ pod: "api-0", container: "app", at: "2026-10-26T11:50:00Z", text: "older" }], truncated: false };
    });
    const { result } = renderHook(() => usePodLogStream({ ...options, pod: "checkout", kind: "Deployment", uid: "workload-uid", since: undefined }));
    await act(async () => { await vi.advanceTimersByTimeAsync(50); });
    await act(async () => { await result.current.loadEarlier(900); });
    expect(history.mock.calls[0][3]).toMatchObject({ since_seconds: 900, tail_lines: POD_LOG_WORKLOAD_EARLIER_TAIL });
    expect(POD_LOG_WORKLOAD_EARLIER_TAIL).toBeLessThanOrEqual(1_000);
    expect(result.current.earlier).toBe("idle");
    expect(result.current.buffer.lines.map((line) => line.text)).toEqual(["older", "newest"]);
  });

  it("drops a departed source from the workload resume instead of replaying its last line", async () => {
    vi.useFakeTimers();
    // The whole first read closes a minute after the lines it delivered, so
    // every source in it is observed up to the moment that connection began.
    const now = Date.parse("2026-10-26T12:01:00Z");
    vi.spyOn(Date, "now").mockReturnValue(now);
    const at = (second: number) => new Date(Date.UTC(2026, 9, 26, 12, 0, second)).toISOString();
    const source = (pod: string) => ({ source_id: `${pod}/app`, pod_uid: `${pod}-uid`, pod, namespace: "shop", container: "app", incarnation: "incarnation-1", admitted: true });
    const frame = (value: Record<string, unknown>) => `event: ${String(value.event)}\ndata: ${JSON.stringify(value)}\n\n`;
    let attempt = 0;
    const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async () => {
      attempt++;
      const body = attempt === 1
        ? [
            frame({ event: "sources", sources: [source("pod-a"), source("pod-b")] }),
            frame({ event: "line", text: "a1", container: "app", pod: "pod-a", pod_uid: "pod-a-uid", source_id: "pod-a/app", timestamp: at(10), ordinal: 1 }),
            frame({ event: "line", text: "b1", container: "app", pod: "pod-b", pod_uid: "pod-b-uid", source_id: "pod-b/app", timestamp: at(20), ordinal: 1 }),
            frame({ event: "limit", reason: "duration" }),
          ].join("")
        : attempt === 2
          // A rollout replaced both Pods with one new source.
          ? [
              frame({ event: "sources", sources: [source("pod-c")] }),
              frame({ event: "line", text: "c1", container: "app", pod: "pod-c", pod_uid: "pod-c-uid", source_id: "pod-c/app", timestamp: at(60), ordinal: 1 }),
              frame({ event: "limit", reason: "duration" }),
            ].join("")
          : frame({ event: "end" });
      return new Response(body, { headers: { "Content-Type": "text/event-stream" } });
    });
    renderHook(() => usePodLogStream({ ...options, pod: "checkout", kind: "Deployment", uid: "workload-uid", since: undefined, tail: 200 }));
    await act(async () => { await vi.advanceTimersByTimeAsync(200); });
    expect(fetch).toHaveBeenCalledTimes(3);
    const since = (call: number) => new URL(String(fetch.mock.calls[call][0]), "http://localhost").searchParams.get("since_time");
    expect(since(1)).toBe(new Date(now - 1000).toISOString());
    // The departed Pods no longer hold the resume back to their last lines;
    // every source the read still follows is observed up to the connection
    // start, including the replacement Pod that wrote one line.
    expect(since(2)).toBe(new Date(now - 1000).toISOString());
  });

  it("holds the workload resume at the connection start for a source that has not written yet", async () => {
    vi.useFakeTimers();
    const startedAt = Date.now();
    // A long follow: the newest line is well after this connection started.
    const at = (second: number) => new Date(startedAt + second * 1000).toISOString();
    const source = (pod: string) => ({ source_id: `${pod}/app`, pod_uid: `${pod}-uid`, pod, namespace: "shop", container: "app", incarnation: "incarnation-1", admitted: true });
    const frame = (value: Record<string, unknown>) => `event: ${String(value.event)}\ndata: ${JSON.stringify(value)}\n\n`;
    let attempt = 0;
    const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async () => {
      attempt++;
      const body = attempt === 1
        ? [
            frame({ event: "sources", sources: [source("pod-a"), source("pod-b")] }),
            frame({ event: "line", text: "a1", container: "app", pod: "pod-a", pod_uid: "pod-a-uid", source_id: "pod-a/app", timestamp: at(30), ordinal: 1 }),
            frame({ event: "limit", reason: "duration" }),
          ].join("")
        : frame({ event: "end" });
      return new Response(body, { headers: { "Content-Type": "text/event-stream" } });
    });
    renderHook(() => usePodLogStream({ ...options, pod: "checkout", kind: "Deployment", uid: "workload-uid", since: undefined, tail: 200 }));
    await act(async () => { await vi.advanceTimersByTimeAsync(100); });
    expect(fetch).toHaveBeenCalledTimes(2);
    const since = new URL(String(fetch.mock.calls[1][0]), "http://localhost").searchParams.get("since_time")!;
    // pod-b has written nothing, so its initial tail starts where the read did.
    expect(Date.parse(since)).toBe(startedAt - 1000);
  });

  it("keeps showing new lines across rotations of a burst that outgrew the checkpoints", async () => {
    vi.useFakeTimers();
    const base = Date.parse("2026-10-26T12:00:00Z");
    // A one-megabyte-per-second burst: 64 KiB every 64 ms fills the read's
    // 16 MB cap in about 16s, well inside the 30s a lagging cursor is forgiven.
    const at = (index: number) => new Date(base + index * 64).toISOString();
    const text = (index: number) => `line ${index} ${"x".repeat(64 << 10)}`;
    const linesPerRead = 256;
    const checkpoint = 64;
    const cursorAt = new Map([[0, "checkpoint-at-0"], [checkpoint, "checkpoint-at-256"]]);
    const reads: Array<{ request: { cursor?: string; since_time?: string }; finish: () => void }> = [];
    // Each read is driven by the test, so one rotation lands per step instead of
    // the follow rotating as fast as the timer queue allows.
    vi.spyOn(api, "kubernetesPodLogStream").mockImplementation((_namespace, _pod, request, emit, signal) => new Promise<void>((resolve) => {
      reads.push({ request, finish: resolve });
      signal.addEventListener("abort", () => resolve(), { once: true });
      // The read is anchored where the client asked: a cursor replays the same
      // window every time it is asked again.
      const fromCursor = [...cursorAt].find(([, value]) => value === request.cursor)?.[0];
      const start = fromCursor ?? (request.since_time ? Math.max(0, Math.ceil((Date.parse(request.since_time) - base) / 64)) : 0);
      for (let index = start; index < start + linesPerRead; index++) {
        emit({ event: "line", text: text(index), container: "app", timestamp: at(index), ordinal: index + 1, ...(cursorAt.has(index) ? { cursor: cursorAt.get(index) } : {}) });
      }
      // The follow stopped checkpointing, so its terminal limit repeats the last
      // cursor it issued — hundreds of lines behind the newest one.
      emit({ event: "limit", text: "", container: "app", reason: "bytes", cursor: cursorAt.get(checkpoint) });
    }));
    const { result } = renderHook(() => usePodLogStream(options));
    const newestShown = () => Math.max(...result.current.buffer.lines.map((line) => Date.parse(line.timestamp ?? "")));
    const positions: number[] = [];
    for (let rotation = 0; rotation < 4; rotation++) {
      await act(async () => { await vi.advanceTimersByTimeAsync(20); });
      positions.push(newestShown());
      if (rotation < 3) await act(async () => { reads[rotation].finish(); await vi.advanceTimersByTimeAsync(1); });
    }
    const requests = reads.map((read) => read.request);
    expect(requests).toHaveLength(4);
    // Replaying from the repeated cursor would re-read the same 16 MB into the
    // same limit for ever; every rotation instead asks from the newest line it
    // has already shown.
    expect(requests.slice(1).every((request) => request.cursor === undefined && request.since_time !== undefined)).toBe(true);
    expect(new Set(positions).size).toBe(4);
    expect(positions).toEqual([...positions].sort((left, right) => left - right));
    expect(result.current.status).toBe("live");
  });

  it("stops a read that keeps rotating from a position it already holds", async () => {
    vi.useFakeTimers();
    const at = (second: number) => new Date(Date.UTC(2026, 9, 26, 12, 0, second)).toISOString();
    const cursor = btoa(JSON.stringify({ target: "shop/api", timestamp: at(1), ordinal: 1 }));
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementation(async (_namespace, _pod, _options, emit) => {
      // The server holds one line and ends every rotation on the same checkpoint.
      emit({ event: "line", text: "only line", container: "app", timestamp: at(1), ordinal: 1, cursor });
      emit({ event: "limit", text: "", container: "app", reason: "bytes", cursor });
    });
    const { result } = renderHook(() => usePodLogStream(options));
    await act(async () => { await vi.advanceTimersByTimeAsync(200); });
    expect(stream).toHaveBeenCalledTimes(POD_LOG_MAX_STALLED_ROTATIONS + 1);
    expect(result.current.status).toBe("limited");
    expect(result.current.message).toContain("same position");
    expect(result.current.buffer.lines.map((line) => line.text)).toEqual(["only line"]);
  });

  it("keeps following a quiet Pod across duration rotations instead of capping the read", async () => {
    vi.useFakeTimers();
    const at = (second: number) => new Date(Date.UTC(2026, 9, 26, 12, 0, second)).toISOString();
    const cursor = btoa(JSON.stringify({ target: "shop/api", timestamp: at(1), ordinal: 1 }));
    let attempt = 0;
    const stream = vi.spyOn(api, "kubernetesPodLogStream").mockImplementation((_namespace, _pod, _options, emit, signal) => {
      attempt++;
      if (attempt === 1) emit({ event: "line", text: "only line", container: "app", timestamp: at(1), ordinal: 1, cursor });
      // Four healthy 30-minute safety cuts over an idle Pod: each rotation
      // resumes from the same line only because the Pod has written nothing
      // since, which is a follow the reader wants to keep. The fifth read keeps
      // following and never ends.
      if (attempt <= 1 + 4) {
        emit({ event: "limit", text: "", container: "app", reason: "duration", cursor });
        return Promise.resolve();
      }
      return new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
    });
    const { result } = renderHook(() => usePodLogStream(options));
    await act(async () => { await vi.advanceTimersByTimeAsync(200); });
    expect(stream).toHaveBeenCalledTimes(1 + 4 + 1);
    expect(result.current.status).toBe("live");
    expect(result.current.message).toBe("");
    expect(result.current.buffer.lines.map((line) => line.text)).toEqual(["only line"]);
  });

  it("advances the workload resume past a silent source on every rotation", async () => {
    vi.useFakeTimers();
    const startedAt = Date.now();
    const source = (pod: string) => ({ source_id: `${pod}/app`, pod_uid: `${pod}-uid`, pod, namespace: "shop", container: "app", incarnation: "incarnation-1", admitted: true });
    const reads: Array<{ request: KubernetesWorkloadLogOptions; emit: (event: KubernetesWorkloadLogEvent) => void; finish: () => void }> = [];
    vi.spyOn(api, "kubernetesWorkloadLogStream").mockImplementation((_kind, _namespace, _pod, _uid, request, emit, signal) => new Promise<void>((resolve) => {
      reads.push({ request, emit, finish: resolve });
      signal.addEventListener("abort", () => resolve(), { once: true });
    }));
    renderHook(() => usePodLogStream({ ...options, pod: "checkout", kind: "Deployment", uid: "workload-uid", since: undefined, tail: 200 }));
    await act(async () => { await vi.advanceTimersByTimeAsync(1); });
    // pod-b is an admitted sidecar that never writes; pod-a follows live.
    for (let rotation = 0; rotation < 3; rotation++) {
      const read = reads[rotation];
      await act(async () => {
        read.emit({ event: "sources", sources: [source("pod-a"), source("pod-b")] });
        read.emit({ event: "line", text: `a${rotation}`, container: "app", pod: "pod-a", pod_uid: "pod-a-uid", source_id: "pod-a/app", timestamp: new Date(Date.now()).toISOString(), ordinal: rotation + 1 });
      });
      await act(async () => { await vi.advanceTimersByTimeAsync(30 * 60 * 1000); });
      await act(async () => {
        read.emit({ event: "limit", reason: "duration" });
        read.finish();
        await vi.advanceTimersByTimeAsync(1);
      });
    }
    expect(reads).toHaveLength(4);
    const resumes = reads.slice(1).map((read) => Date.parse(read.request.since_time!));
    // The first rotation still honours where the silent source joined ...
    expect(resumes[0]).toBe(startedAt - 1000);
    // ... and every later one moves on with the connection instead of
    // resuming from that same moment for the life of the panel.
    for (let index = 1; index < resumes.length; index++) {
      expect(resumes[index] - resumes[index - 1]).toBeGreaterThanOrEqual(30 * 60 * 1000);
    }
  });

  it("advances the workload resume past a source that wrote once and then went quiet", async () => {
    vi.useFakeTimers();
    const startedAt = Date.now();
    const source = (pod: string) => ({ source_id: `${pod}/app`, pod_uid: `${pod}-uid`, pod, namespace: "shop", container: "app", incarnation: "incarnation-1", admitted: true });
    const at = (second: number) => new Date(startedAt + second * 1000).toISOString();
    // Both sources log one line at the start of the follow and stay silent
    // afterwards, the way an init container or a sidecar that logged once does.
    const line = (pod: string) => ({ event: "line" as const, text: `${pod} only`, container: "app", pod, pod_uid: `${pod}-uid`, source_id: `${pod}/app`, timestamp: at(5), ordinal: 1 });
    const reads: Array<{ request: KubernetesWorkloadLogOptions; emit: (event: KubernetesWorkloadLogEvent) => void; finish: () => void }> = [];
    vi.spyOn(api, "kubernetesWorkloadLogStream").mockImplementation((_kind, _namespace, _pod, _uid, request, emit, signal) => new Promise<void>((resolve) => {
      reads.push({ request, emit, finish: resolve });
      signal.addEventListener("abort", () => resolve(), { once: true });
      // A read that still starts before those lines has them replayed.
      if (!request.since_time || Date.parse(request.since_time) <= Date.parse(at(5))) {
        emit(line("pod-a"));
        emit(line("pod-b"));
      }
    }));
    const { result } = renderHook(() => usePodLogStream({ ...options, pod: "checkout", kind: "Deployment", uid: "workload-uid", since: undefined, tail: 200 }));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1);
      reads[0].emit({ event: "sources", sources: [source("pod-a"), source("pod-b")] });
    });
    for (let rotation = 0; rotation < 3; rotation++) {
      await act(async () => { await vi.advanceTimersByTimeAsync(30 * 60 * 1000); });
      await act(async () => {
        reads[rotation].emit({ event: "limit", reason: "duration" });
        reads[rotation].finish();
        await vi.advanceTimersByTimeAsync(1);
      });
    }
    expect(reads).toHaveLength(4);
    const resumes = reads.slice(1).map((read) => Date.parse(read.request.since_time!));
    // The single line must not pin the resume for the life of the panel: every
    // later rotation resumes where the last connection's own coverage began,
    // a second before it, not from the line written at the start.
    for (let index = 1; index < resumes.length; index++) {
      expect(resumes[index] - resumes[index - 1]).toBeGreaterThanOrEqual(30 * 60 * 1000 - 10_000);
    }
    expect(resumes[resumes.length - 1]).toBeGreaterThan(Date.parse(at(5)) + 30 * 60 * 1000);
    expect(result.current.buffer.lines.filter((item) => !item.neutral).map((item) => item.text)).toEqual(["pod-a only", "pod-b only"]);
  });
});

describe("Earlier log window merge", () => {
  const line = (text: string, extra: Record<string, unknown> = {}) => ({ text, container: "app", ...extra });
  it("drops only the overlap at the seam and keeps repeated text elsewhere", () => {
    const buffer = appendPodLogLines({ lines: [], bytes: 0, dropped: 0, nextKey: 0 }, [line("repeat"), line("b"), line("repeat")]);
    const merged = prependEarlierLines(buffer, [line("a"), line("repeat"), line("b"), line("repeat")]);
    expect(merged.lines.map((entry) => entry.text)).toEqual(["a", "repeat", "b", "repeat"]);
    expect(new Set(merged.lines.map((entry) => entry.key)).size).toBe(merged.lines.length);
  });
  it("keeps the whole earlier read when the two windows do not touch", () => {
    const buffer = appendPodLogLines({ lines: [], bytes: 0, dropped: 0, nextKey: 0 }, [line("live")]);
    expect(earlierOverlap(buffer, [line("old")])).toBe(0);
    const merged = prependEarlierLines(buffer, [line("old")]);
    expect(merged.lines.map((entry) => entry.text)).toEqual(["old", "live"]);
  });
  it("finds the seam inside a long live read instead of duplicating a short window out of order", () => {
    const base = Date.UTC(2026, 9, 26, 11, 0, 0);
    const at = (seconds: number) => new Date(base + seconds * 1000).toISOString();
    // Thirty minutes of live follow, one line every ten seconds.
    const live = Array.from({ length: 180 }, (_, index) => line(`live-${index}`, { timestamp: at(index * 10) }));
    const buffer = appendPodLogLines({ lines: [], bytes: 0, dropped: 0, nextKey: 0 }, live);
    // A fifteen-minute window read afterwards re-reads the newest half of what
    // the live follow already shows, plus lines older than its first one.
    const window = live.slice(90);
    const older = Array.from({ length: 20 }, (_, index) => line(`older-${index}`, { timestamp: at(-20 + index) }));
    const merged = prependEarlierLines(buffer, [...older, ...window]);
    expect(merged.lines).toHaveLength(200);
    expect(new Set(merged.lines.map((entry) => entry.text)).size).toBe(200);
    expect(merged.lines.slice(0, 20).map((entry) => entry.text)).toEqual(older.map((entry) => entry.text));
    // The merge is ordered: no stale line lands above a newer one.
    const times = merged.lines.map((entry) => Date.parse(entry.timestamp!));
    expect(times).toEqual([...times].sort((left, right) => left - right));
  });
  it("splits the overlap on timestamps so identical text from different seconds survives", () => {
    const buffer = appendPodLogLines({ lines: [], bytes: 0, dropped: 0, nextKey: 0 }, [line("served", { timestamp: "2026-10-26T12:00:01Z" })]);
    const merged = prependEarlierLines(buffer, [line("served", { timestamp: "2026-10-26T11:59:59Z" }), line("served", { timestamp: "2026-10-26T12:00:01Z" })]);
    expect(merged.lines.map((entry) => entry.timestamp)).toEqual(["2026-10-26T11:59:59Z", "2026-10-26T12:00:01Z"]);
  });
});