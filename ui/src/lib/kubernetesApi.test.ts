// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "./api";

afterEach(() => {
  vi.restoreAllMocks();
});

describe("Kubernetes resource API", () => {
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