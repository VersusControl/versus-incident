// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError, createKubernetesApi } from "./api";

const frame = (event: string, payload: unknown) =>
  `event: ${event}\ndata: ${JSON.stringify(payload)}\n\n`;
function serve(body: string) {
  return vi
    .spyOn(globalThis, "fetch")
    .mockResolvedValue(
      new Response(body, { headers: { "Content-Type": "text/event-stream" } }),
    );
}
afterEach(() => vi.restoreAllMocks());

describe("Resource detail API transport", () => {
  it.each(["pod", "workload"] as const)("preserves flat and nested %s stream admission errors and bounded retry hints", async (target) => {
    const fetch = vi.spyOn(globalThis, "fetch");
    for (const status of [429, 502]) {
      for (const body of [
        { error: "The concurrent Pod log stream limit has been reached.", code: "log_stream_busy", retryable: true },
        { error: { code: "log_stream_busy", message: "The concurrent Pod log stream limit has been reached." } },
      ]) {
        fetch.mockResolvedValueOnce(new Response(JSON.stringify(body), { status, headers: { "Retry-After": "2" } }));
        const client = createKubernetesApi();
        const signal = new AbortController().signal;
        const read = target === "pod"
          ? client.kubernetesPodLogStream("shop", "api", {}, vi.fn(), signal)
          : client.kubernetesWorkloadLogStream("Deployment", "shop", "api", "uid", {}, vi.fn(), signal);
        await expect(read).rejects.toMatchObject({
          status, body, message: "The concurrent Pod log stream limit has been reached.", retryAfterMs: 2000,
        });
      }
    }
  });

  it.each(["pod", "workload"] as const)("keeps the %s HTTP status on non-JSON errors and notifies only expired authentication", async (target) => {
    const notify = vi.spyOn(window, "dispatchEvent");
    const fetch = vi.spyOn(globalThis, "fetch");
    for (const status of [400, 401, 403, 429, 503]) {
      fetch.mockResolvedValueOnce(new Response("<html>proxy error</html>", { status }));
      const client = createKubernetesApi();
      const signal = new AbortController().signal;
      const read = target === "pod"
        ? client.kubernetesPodLogStream("shop", "api", {}, vi.fn(), signal)
        : client.kubernetesWorkloadLogStream("Deployment", "shop", "api", "uid", {}, vi.fn(), signal);
      await expect(read).rejects.toMatchObject({ status, message: `${target === "pod" ? "Pod" : "Workload"} log stream failed (HTTP ${status}).` });
    }
    expect(notify).toHaveBeenCalledTimes(1);
  });

  it("uses the cluster-scoped history endpoint and ambient session", async () => {
    const response = {
      observed_at: "2026-10-21T08:00:00Z",
      source: "capture",
      availability: "unavailable",
      samples: [],
      retention_seconds: 21600,
    };
    const fetch = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(JSON.stringify(response), {
        headers: { "Content-Type": "application/json" },
      }),
    );
    const signal = new AbortController().signal;
    expect(
      await createKubernetesApi("west").kubernetesUsageHistory(
        "shop",
        "checkout-0",
        signal,
      ),
    ).toEqual(response);
    const url = new URL(String(fetch.mock.calls[0][0]), "http://localhost");
    expect(url.pathname).toBe("/api/admin/kubernetes/usage-history");
    expect(Object.fromEntries(url.searchParams)).toEqual({
      cluster: "west",
      namespace: "shop",
      pod: "checkout-0",
    });
    expect(fetch.mock.calls[0][1]).toMatchObject({
      credentials: "same-origin",
      signal,
    });
  });
  it("opens one authenticated workload read with repeated Pod filters and an opaque cursor", async () => {
    const fetch = serve(
      frame("end", { workload_uid: "workload-uid", cursor: "checkpoint" }),
    );
    const onEvent = vi.fn();
    const signal = new AbortController().signal;
    await createKubernetesApi("west").kubernetesWorkloadLogStream(
      "Deployment",
      "shop",
      "checkout",
      "workload-uid",
      {
        pod_uid: ["pod-a", "pod-b"],
        container: "*",
        previous: true,
        timestamps: false,
        since_seconds: 3600,
        tail_lines: 500,
        since_time: "2026-10-26T12:00:00.000Z",
      },
      onEvent,
      signal,
    );
    const url = new URL(String(fetch.mock.calls[0][0]), "http://localhost");
    expect(url.pathname).toBe(
      "/api/admin/kubernetes/workloads/Deployment/checkout/logs/stream",
    );
    expect(url.searchParams.getAll("pod_uid")).toEqual(["pod-a", "pod-b"]);
    expect(url.searchParams.get("cluster")).toBe("west");
    expect(url.searchParams.get("uid")).toBe("workload-uid");
    expect(url.searchParams.get("timestamps")).toBe("false");
    expect(url.searchParams.get("container")).toBe("*");
    // The workload resume is a wall clock, so no cursor travels in the URL.
    expect(url.searchParams.has("cursor")).toBe(false);
    expect(url.searchParams.get("since_time")).toBe("2026-10-26T12:00:00.000Z");
    expect(url.searchParams.has("since_seconds")).toBe(false);
    expect(url.searchParams.has("tail_lines")).toBe(false);
    expect(url.searchParams.has("token")).toBe(false);
    expect(fetch.mock.calls[0][1]).toMatchObject({
      credentials: "same-origin",
      cache: "no-store",
      signal,
    });
    expect(new Headers(fetch.mock.calls[0][1]?.headers).get("Accept")).toBe(
      "text/event-stream",
    );
    expect(onEvent).toHaveBeenCalledWith(
      expect.objectContaining({ event: "end" }),
    );
    expect(onEvent.mock.calls[0][0]).not.toHaveProperty("cursor");
  });
  it("normalizes omitted empty membership from the bounded Go projection", async () => {
    serve(frame("sources", { workload_uid: "workload-uid" }));
    const onEvent = vi.fn();
    await createKubernetesApi().kubernetesWorkloadLogStream(
      "StatefulSet",
      "shop",
      "checkout",
      "uid",
      {},
      onEvent,
      new AbortController().signal,
    );
    expect(onEvent).toHaveBeenCalledWith(
      expect.objectContaining({ sources: [], pods: [] }),
    );
  });
  it("accepts the complete bounded membership catalog and source-status identity", async () => {
    const pod = {
      uid: "pod-uid",
      name: "checkout-0",
      containers: ["app"],
      init_containers: [],
      main_container: "app",
      admitted: true,
      all_pods_admitted: false,
      all_pods_reason: "source_limit",
    };
    serve(
      frame("sources", { workload_uid: "uid", sources: [], pods: [pod] }) +
        frame("source_status", {
          workload_uid: "uid",
          pod_uid: pod.uid,
          pod: pod.name,
          code: "forbidden",
          message: "forbidden",
        }),
    );
    const onEvent = vi.fn();
    await createKubernetesApi().kubernetesWorkloadLogStream(
      "StatefulSet",
      "shop",
      "checkout",
      "uid",
      {},
      onEvent,
      new AbortController().signal,
    );
    expect(onEvent).toHaveBeenCalledTimes(2);
    expect(onEvent.mock.calls[0][0].pods).toEqual([pod]);
    expect(onEvent.mock.calls[1][0]).toMatchObject({
      event: "source_status",
      pod_uid: "pod-uid",
    });
  });
  it("accepts an aggregate limit frame and keeps no resume token in the event", async () => {
    const cursor = "x".repeat(128 << 10);
    serve(frame("limit", { cursor, reason: "bytes" }));
    const onEvent = vi.fn();
    await createKubernetesApi().kubernetesWorkloadLogStream(
      "Deployment",
      "shop",
      "checkout",
      "uid",
      {},
      onEvent,
      new AbortController().signal,
    );
    expect(onEvent.mock.calls[0][0]).toMatchObject({ event: "limit", reason: "bytes" });
    expect(onEvent.mock.calls[0][0]).not.toHaveProperty("cursor");
  });
  it.each([
    ["line", { text: "message", container: "app" }],
    [
      "line",
      {
        text: "x".repeat(16385),
        container: "app",
        pod: "p",
        pod_uid: "u",
        source_id: "s",
      },
    ],
    [
      "sources",
      {
        sources: [],
        pods: [
          {
            uid: "u",
            name: "p",
            admitted: true,
            containers: ["app"],
            init_containers: "not-an-array",
          },
        ],
      },
    ],
    ["sources", { sources: Array.from({ length: 41 }, () => ({})), pods: [] }],
    [
      "sources",
      {
        sources: [],
        pods: [
          {
            uid: "u",
            name: "p",
            admitted: true,
            containers: ["app"],
            init_containers: [],
            all_pods_admitted: "false",
          },
        ],
      },
    ],
    [
      "sources",
      {
        sources: [],
        pods: [
          {
            uid: "u",
            name: "p",
            admitted: true,
            containers: ["app"],
            init_containers: [],
            all_pods_reason: 42,
          },
        ],
      },
    ],
  ])("rejects malformed or over-bound %s frames", async (event, payload) => {
    serve(frame(event as string, payload));
    await expect(
      createKubernetesApi().kubernetesWorkloadLogStream(
        "Deployment",
        "shop",
        "checkout",
        "uid",
        {},
        vi.fn(),
        new AbortController().signal,
      ),
    ).rejects.toBeInstanceOf(ApiError);
  });
  it("fails closed on revoked access before consuming any SSE payload", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response("private response", { status: 403 }),
    );
    const onEvent = vi.fn();
    await expect(
      createKubernetesApi().kubernetesWorkloadLogStream(
        "Deployment",
        "shop",
        "checkout",
        "uid",
        {},
        onEvent,
        new AbortController().signal,
      ),
    ).rejects.toMatchObject({ status: 403 });
    expect(onEvent).not.toHaveBeenCalled();
  });
});
