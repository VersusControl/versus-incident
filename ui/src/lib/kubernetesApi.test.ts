// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { api, clearSecret, createKubernetesApi, kubernetesClusterPath, setSecret } from "./api";

afterEach(() => {
  vi.restoreAllMocks();
  clearSecret();
});

describe("Kubernetes resource API", () => {
  it("preserves single URLs and encodes cluster scope without discarding filters", () => {
    expect(kubernetesClusterPath("/api/admin/kubernetes/overview")).toBe("/api/admin/kubernetes/overview");
    const url = new URL(kubernetesClusterPath("/api/admin/kubernetes/search?q=a%2Bb&cluster=old", "prod/eu+one"), "http://localhost");
    expect(url.searchParams.get("q")).toBe("a+b");
    expect(url.searchParams.getAll("cluster")).toEqual(["prod/eu+one"]);
  });

  it("scopes every Kubernetes JSON route including all namespace continuation pages", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
      const url = new URL(String(input), "http://localhost");
      return Promise.resolve(new Response(JSON.stringify({ items: [], pods: [], lines: [], truncated: false, ...(url.searchParams.get("resource_id") === "core~v1~namespaces" && !url.searchParams.has("continue") ? { continue: "page-two" } : {}) })));
    });
    const scoped = createKubernetesApi("prod-eu");
    await Promise.all([
      scoped.kubernetesOverview(), scoped.kubernetesOverviewGraph(), scoped.kubernetesIssues(), scoped.kubernetesChanges(), scoped.kubernetesGraph(),
      scoped.kubernetesNeighborhood({ kind: "Pod", name: "api" }), scoped.kubernetesTraffic(), scoped.kubernetesTop({ kind: "pod" }),
      scoped.kubernetesNodes(), scoped.kubernetesNamespaces(), scoped.kubernetesNodePods("node-a"), scoped.kubernetesUsage(), scoped.kubernetesWorkloads(),
      scoped.kubernetesWorkload("Pod", "shop", "api"), scoped.kubernetesWorkloadLogs("Pod", "shop", "api"), scoped.kubernetesPodLogs("shop", "api"),
      scoped.kubernetesDiagnose("core~v1~pods", "shop", "api"), scoped.kubernetesReleases(), scoped.kubernetesRelease("shop", "api"), scoped.kubernetesGitOpsApps(),
      scoped.kubernetesRollout("shop", "api"), scoped.kubernetesRollouts(), scoped.kubernetesSearch("shop", "api"), scoped.kubernetesEvents(), scoped.kubernetesDescribe("core~v1~pods", "shop", "api"),
    ]);
    expect(fetchMock).toHaveBeenCalledTimes(26);
    for (const [input] of fetchMock.mock.calls) expect(new URL(String(input), "http://localhost").searchParams.get("cluster")).toBe("prod-eu");
  });

  it("scopes both streams and retains each caller's abort signal", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(() => Promise.resolve(new Response('event: end\ndata: {}\n\n', { headers: { "Content-Type": "text/event-stream" } })));
    const signal = new AbortController().signal;
    const scoped = createKubernetesApi("staging");
    await scoped.kubernetesStream(vi.fn(), signal);
    await scoped.kubernetesPodLogStream("shop", "api", { cursor: "resume" }, vi.fn(), signal);
    for (const [input, options] of fetchMock.mock.calls) {
      expect(new URL(String(input), "http://localhost").searchParams.get("cluster")).toBe("staging");
      expect(options?.signal).toBe(signal);
    }
  });

  it("writes optional member and team scope while preserving legacy role payloads", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(() => Promise.resolve(new Response("{}")));
    await api.setMemberRole("deployment", "alice", "responder", ["staging"]);
    await api.setTeamRole("deployment", "operators", "admin", []);
    await api.setMemberRole("deployment", "bob", "viewer");
    await api.setMemberRole("deployment", "alice", "admin", []);
    expect(fetchMock.mock.calls.map(([, options]) => JSON.parse(String(options?.body)))).toEqual([{ role: "responder", clusters: ["staging"] }, { role: "admin", clusters: [] }, { role: "viewer" }, { role: "admin", clusters: [] }]);
  });

  it("streams Pod lines with gateway headers, cancellation, and cursor-only resume options", async () => {
    setSecret("test-gateway");
    const signal = new AbortController().signal;
    const onEvent = vi.fn();
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response('event: line\ndata: {"text":"repeated","container":"app","ordinal":2,"cursor":"resume"}\n\nevent: end\ndata: {"reason":"complete"}\n\n', { headers: { "Content-Type": "text/event-stream" } }));
    await api.kubernetesPodLogStream("payments", "checkout", { container: "app", cursor: "resume", since_seconds: 3600, tail_lines: 500 }, onEvent, signal);
    expect(fetchMock.mock.calls[0][0]).toBe("/api/admin/kubernetes/pods/payments/checkout/logs/stream?timestamps=true&container=app&cursor=resume");
    const init = fetchMock.mock.calls[0][1];
    expect(new Headers(init?.headers).get("X-Gateway-Secret")).toBe("test-gateway");
    expect(init?.signal).toBe(signal);
    expect(init?.credentials).toBe("same-origin");
    expect(onEvent).toHaveBeenCalledWith(expect.objectContaining({ event: "line", text: "repeated", ordinal: 2 }));
    expect(onEvent).toHaveBeenCalledWith(expect.objectContaining({ event: "end", reason: "complete" }));
  });

  it("never exposes upstream HTTP bodies as Pod stream errors", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response("private upstream credentials", { status: 403 }));
    await expect(api.kubernetesPodLogStream("payments", "checkout", {}, vi.fn(), new AbortController().signal)).rejects.toThrow("Pod log stream failed (HTTP 403).");
  });

  it("reads JSON checkpoints on every event without using SSE ids and defaults missing uncertainty to false", async () => {
    const cursor = "checkpoint".repeat(400);
    const events = ["line", "heartbeat", "limit", "error", "end"];
    const body = events.map((event) => `id: not-a-cursor\nevent: ${event}\ndata: ${JSON.stringify({ text: "same", container: "app", timestamp: "2026-10-07T12:00:00Z", ordinal: 1, sequence: 1, cursor, ...(event === "limit" ? { replay_uncertain: true } : {}) })}\n\n`).join("") + 'id: ignored\nevent: heartbeat\ndata: {}\n\n';
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(body, { headers: { "Content-Type": "text/event-stream" } }));
    const onEvent = vi.fn();
    await api.kubernetesPodLogStream("shop", "api", {}, onEvent, new AbortController().signal);
    expect(onEvent.mock.calls.slice(0, 5).map(([event]) => event.cursor)).toEqual(events.map(() => cursor));
    expect(onEvent.mock.calls.map(([event]) => event.replay_uncertain)).toEqual([false, false, true, false, false, false]);
    expect(onEvent.mock.calls[5][0].cursor).toBeUndefined();
  });

  it.each([
    'event: line\ndata: {"text":42,"container":"app"}\n\n',
    'event: line\ndata: {"text":"safe","container":"app","cursor":42}\n\n',
    'event: line\ndata: {"text":"safe","container":"app","ordinal":-1}\n\n',
    'event: heartbeat\ndata: {"replay_uncertain":"false"}\n\n',
    `event: heartbeat\ndata: ${JSON.stringify({ cursor: "x".repeat((16 << 10) + 1) })}\n\n`,
    'event: heartbeat\ndata: invalid-json\n\n',
  ])("rejects malformed Pod stream events", async (body) => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(body, { headers: { "Content-Type": "text/event-stream" } }));
    await expect(api.kubernetesPodLogStream("shop", "api", {}, vi.fn(), new AbortController().signal)).rejects.toThrow(/Invalid Pod log/);
  });

  it("passes abort failures through without exposing a body", async () => {
    vi.spyOn(globalThis, "fetch").mockRejectedValue(new DOMException("Aborted", "AbortError"));
    await expect(api.kubernetesPodLogStream("shop", "api", {}, vi.fn(), new AbortController().signal)).rejects.toMatchObject({ name: "AbortError" });
  });

  it("reads the complete all-namespace overview graph without pagination or namespace filters", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify({ nodes: [], edges: [], omitted: {}, sync: { state: "ready", age_s: 0, partial: false } }), { status: 200 }));
    await api.kubernetesOverviewGraph();
    expect(fetchMock).toHaveBeenCalledWith("/api/admin/kubernetes/graph/overview", expect.any(Object));
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });
  it("lists cluster nodes through the generic resource endpoint", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(() => Promise.resolve(new Response(JSON.stringify({ items: [], truncated: false }), { status: 200 })));

    await api.kubernetesNodes();

    expect(fetchMock).toHaveBeenCalledWith("/api/admin/kubernetes/resources?resource_id=core%7Ev1%7Enodes&limit=20", expect.any(Object));
  });

  it("encodes a selected node in the generic pod field selector", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(() => Promise.resolve(new Response(JSON.stringify({ items: [], truncated: false }), { status: 200 })));

    await api.kubernetesNodePods("worker/a+b");

    expect(fetchMock).toHaveBeenCalledWith("/api/admin/kubernetes/resources?resource_id=core%7Ev1%7Epods&limit=20&fields=spec.nodeName%3Dworker%2Fa%2Bb", expect.any(Object));
  });

  it("requests one Kubernetes continuation page at a time", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(() => Promise.resolve(new Response(JSON.stringify({ items: [{ resource_id: "core~v1~nodes", kind: "Node", name: "node-next" }], continue: "next-token", truncated: true }), { status: 200 })));

    const result = await api.kubernetesNodes("next-token");

    expect(result.items).toHaveLength(1);
    expect(result.continue).toBe("next-token");
    expect(fetchMock).toHaveBeenCalledWith("/api/admin/kubernetes/resources?resource_id=core%7Ev1%7Enodes&limit=20&continue=next-token", expect.any(Object));
  });

  it("collects every namespace continuation page and retains partial failures", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
      const url = String(input);
      const page = url.includes("continue=")
        ? { items: [{ resource_id: "core~v1~namespaces", kind: "Namespace", name: "payments" }], truncated: false, partial_failures: [{ class: "forbidden" }] }
        : { items: [{ resource_id: "core~v1~namespaces", kind: "Namespace", name: "platform" }], continue: "namespace-next", truncated: true };
      return Promise.resolve(new Response(JSON.stringify(page), { status: 200 }));
    });

    const result = await api.kubernetesNamespaces();

    expect(result.items?.map((item) => item.name)).toEqual(["platform", "payments"]);
    expect(result.truncated).toBe(false);
    expect(result.partial_failures).toEqual([{ class: "forbidden" }]);
    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "/api/admin/kubernetes/resources?resource_id=core%7Ev1%7Enamespaces&limit=20",
      "/api/admin/kubernetes/resources?resource_id=core%7Ev1%7Enamespaces&limit=20&continue=namespace-next",
    ]);
  });

  it("serializes workload filters and cursor without fetching a client-side 500-row inventory", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(() => Promise.resolve(new Response(JSON.stringify({ items: [], counts: { Deployment: 3 }, truncated: false }), { status: 200 })));

    await api.kubernetesWorkloads({ namespace: "payments", kind: "Pod", q: "checkout", limit: 20, cursor: "page-two" });

    expect(fetchMock).toHaveBeenCalledWith("/api/admin/kubernetes/workloads?namespace=payments&kind=Pod&q=checkout&limit=20&cursor=page-two", expect.any(Object));
  });

  it("serializes bounded timeline filters to the registered changes route", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(() => Promise.resolve(new Response(JSON.stringify({ items: [], gaps: [], sync: { state: "ready", age_s: 0, partial: false } }), { status: 200 })));

    await api.kubernetesChanges({ since: "2026-10-05T11:00:00Z", until: "2026-10-05T12:00:00Z", namespace: "payments", kind: "Deployment", limit: 50, cursor: "10" });

    expect(fetchMock.mock.calls[0][0]).toBe("/api/admin/kubernetes/changes?since=2026-10-05T11%3A00%3A00Z&until=2026-10-05T12%3A00%3A00Z&namespace=payments&kind=Deployment&limit=50&cursor=10");
  });

  it("uses the registered bounded graph and neighborhood query contracts", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(() => Promise.resolve(new Response(JSON.stringify({ nodes: [], edges: [], omitted: {}, sync: { state: "ready", age_s: 0, partial: false } }), { status: 200 })));

    await api.kubernetesGraph({ namespace: "payments", connected_only: true, limit: 20, cursor: "graph-next" });
    await api.kubernetesGraph({ namespace: "payments", connected_only: true, complete: true });
    await api.kubernetesNeighborhood({ kind: "Deployment", namespace: "payments", name: "checkout", hops: 1, max_nodes: 30 });

    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "/api/admin/kubernetes/graph?namespace=payments&connected_only=true&limit=20&cursor=graph-next",
      "/api/admin/kubernetes/graph?namespace=payments&connected_only=true&complete=true",
      "/api/admin/kubernetes/graph/neighborhood?kind=Deployment&namespace=payments&name=checkout&hops=1&max_nodes=30",
    ]);
  });

  it("reads paged release and rollout objects", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(() => Promise.resolve(new Response(JSON.stringify({ items: [], available: true, truncated: false }), { status: 200 })));

    await api.kubernetesReleases({ limit: 20, cursor: "release-next" });
    await api.kubernetesRollouts({ namespace: "payments", limit: 20, cursor: "rollout-next" });

    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      "/api/admin/kubernetes/releases?limit=20&cursor=release-next",
      "/api/admin/kubernetes/rollouts?namespace=payments&limit=20&cursor=rollout-next",
    ]);
  });

  it("normalizes missing workload log arrays and preserves previous-container partial failures", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify({ pods: null, lines: null, truncated: false, partial_failures: [{ scope: "logs", class: "previous_unavailable" }] }), { status: 200 }));

    const result = await api.kubernetesWorkloadLogs("Deployment", "payments", "checkout", { previous: true });

    expect(result.pods).toEqual([]);
    expect(result.lines).toEqual([]);
    expect(result.partial_failures?.[0].class).toBe("previous_unavailable");
  });

  it("queries real Kubernetes traffic with the selected namespace and window", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(() => Promise.resolve(new Response(JSON.stringify({ available: false, reason: "no flow source connected", window: "5m", unmapped: 0, truncated: false }), { status: 200 })));

    await api.kubernetesTraffic({ namespace: "payments", window: "5m" });

    expect(fetchMock).toHaveBeenCalledWith("/api/admin/kubernetes/traffic?window=5m&namespace=payments", expect.any(Object));
  });

  it("normalizes the live unavailable-metrics response without treating its null list as empty evidence", async () => {
    const liveUnavailableMetricsResponse = {
      items: null,
      total: 0,
      truncated: false,
      availability: "unavailable",
      fresh: false,
      sync: { state: "direct", age_s: 0, partial: true },
    };
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify(liveUnavailableMetricsResponse), { status: 200 }));

    const result = await api.kubernetesTop({ kind: "pod", sort: "cpu", limit: 5 });

    expect(result.items).toEqual([]);
    expect(result.items_available).toBe(false);
    expect(result.availability).toBe("unavailable");
    expect(result.sync.partial).toBe(true);
  });

  it("submits a Kubernetes action proposal to the shared agent route", async () => {
    const proposal = { type: "k8s.rollout_restart", target: { cluster: "production", kind: "Deployment", namespace: "payments", name: "checkout" }, params: {}, reason: "Restore service" };
    const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(() => Promise.resolve(new Response(JSON.stringify({ proposal: { id: "proposal-1" }, approval: { id: "approval-1" }, nonce: "decision-nonce" }), { status: 201 })));

    await api.proposeAgentAction(proposal);

    expect(fetchMock).toHaveBeenCalledWith("/api/v1/agent/proposals", expect.objectContaining({ method: "POST", body: JSON.stringify(proposal) }));
  });
});