// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { api, clearSecret, createKubernetesApi, kubernetesClusterPath, setSecret } from "./api";
import { focusedChanges } from "./kubernetesFocusedHistory";

afterEach(() => {
  vi.restoreAllMocks();
  clearSecret();
});

describe("Kubernetes resource API", () => {
  it("carries selected occurrence and center separately from date-free retained root identity", async () => {
    const selected = { occurrence_id: "selected", root: { uid: "root" }, broke_at: "2026-10-14T12:00:00Z" };
    const fetch = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify({ items: [], selected_occurrence: selected, selection_status: "resolved" })));
    const result = await createKubernetesApi("east").kubernetesIssues({ context: "persisted", root_uid: "root", namespace: "shop", occurrence_id: "selected", center: selected.broke_at });
    expect(Object.fromEntries(new URL(String(fetch.mock.calls[0][0]), "http://localhost").searchParams)).toEqual({
      context: "persisted", root_uid: "root", namespace: "shop", occurrence_id: "selected", center: selected.broke_at, cluster: "east",
    });
    expect(result.selected_occurrence).toEqual(selected);
    expect(result.selection_status).toBe("resolved");
  });

  it.each(["changes", "crashes"] as const)("returns one raw focused %s page including empty continuations, without hidden paging or cursor restart", async route => {
    const fetch = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify({ items: [], next: "opaque-next", sync: { state: "ready", age_s: 0, partial: false } })));
    const client = createKubernetesApi("east"), signal = new AbortController().signal;
    const result = await client[route === "changes" ? "kubernetesChanges" : "kubernetesCrashes"]({ root_uid: "root", cursor: "position", limit: 100 }, signal, "single");
    expect(fetch).toHaveBeenCalledTimes(1);
    expect(result.next).toBe("opaque-next");
    expect(fetch.mock.calls[0][1]?.signal).toBe(signal);
    fetch.mockReset().mockResolvedValue(new Response(JSON.stringify({ error: "Invalid query" }), { status: 400 }));
    await expect(client[route === "changes" ? "kubernetesChanges" : "kubernetesCrashes"]({ root_uid: "root", cursor: "expired" }, signal, "single")).rejects.toMatchObject({ status: 400 });
    expect(fetch).toHaveBeenCalledTimes(1);
  });

  it("enforces the focused scan's actual HTTP request budget even on an empty history", async () => {
    let page = 0;
    const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async () => new Response(JSON.stringify({ items: [], next: `cursor-${++page}`, sync: { state: "ready", age_s: 0, partial: false } })));
    const signal = new AbortController().signal;
    const client = createKubernetesApi("east");
    const result = await focusedChanges(cursor => client.kubernetesChanges({ root_uid: "root", cursor, limit: 100 }, signal, "single"), signal);
    expect(fetch).toHaveBeenCalledTimes(5);
    expect(result.incomplete_range).toBe(true);
    expect(result.next).toBe("cursor-5");
  });
  it("fetches persisted UID context independently of history dates and keeps its envelope separate", async () => {
    const root = { kind: "Deployment", namespace: "shop", name: "retained-root", uid: "root-uid" };
    const page = { items: [], totals: {}, truncated: false, sync: { state: "ready", age_s: 0, partial: false }, root_context: { root, occurrence: { root, rule: "resource.context", severity: "info", count: 0 }, retained: true } };
    const fetch = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify(page)));
    const controller = new AbortController();
    const result = await createKubernetesApi("east").kubernetesIssues({ root_uid: "root-uid", namespace: "shop", context: "persisted" }, controller.signal);
    const url = new URL(String(fetch.mock.calls[0][0]), "http://localhost");
    expect(url.pathname).toBe("/api/admin/kubernetes/issues");
    expect(Object.fromEntries(url.searchParams)).toEqual({ cluster: "east", root_uid: "root-uid", namespace: "shop", context: "persisted" });
    expect(fetch.mock.calls[0][1]?.signal).toBe(controller.signal);
    expect(result).toEqual(page);
    expect(result.items).toEqual([]);
    expect(result.totals).toEqual({});
  });
  it.each(["issues", "timeline", "changes", "crashes"] as const)("follows empty %s evidence pages without changing opaque scope, filters or windows", async (route) => {
    const fetch = vi.spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(new Response(JSON.stringify({ items: [], next: "opaque+/=position", gaps: [{ from: "start", to: "end" }] })))
      .mockResolvedValueOnce(new Response(JSON.stringify({ items: [{ id: "retained" }], total: 90000, next: "another+position" })));
    const client = createKubernetesApi("east");
    const options = { since: "2020-01-01T00:00:00Z", until: "2026-10-28T00:00:00Z", namespace: "shop", root_uid: "root", limit: 10000 };
    const result = route === "issues" ? await client.kubernetesIssues({ ...options, severity: "critical", broken: true })
      : route === "timeline" ? await client.kubernetesTimeline({ ...options, breaks: 1 })
      : route === "changes" ? await client.kubernetesChanges({ ...options, kind: "Deployment", type: "deleted" })
      : await client.kubernetesCrashes({ ...options, pod_uid: "pod" });
    expect(result.items).toEqual([{ id: "retained" }]);
    expect(fetch).toHaveBeenCalledTimes(2);
    const first = new URL(String(fetch.mock.calls[0][0]), "http://localhost");
    const next = new URL(String(fetch.mock.calls[1][0]), "http://localhost");
    expect(next.searchParams.get("cursor")).toBe("opaque+/=position");
    next.searchParams.delete("cursor");
    expect(next.toString()).toBe(first.toString());
    expect(first.searchParams.get("cluster")).toBe("east");
    expect(first.searchParams.get("since")).toBe(options.since);
    expect(first.searchParams.get("until")).toBe(options.until);
    expect(first.searchParams.get("limit")).toBe("500");
    if (route === "timeline") expect(result).toMatchObject({ gaps: [{ from: "start", to: "end" }], total: 90000 });
  });

  it("restarts an expired file cursor once without mutating history and retains the exact query", async () => {
    const fetch = vi.spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(new Response(JSON.stringify({ error: "Invalid query" }), { status: 400 }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ items: [{ id: "retained" }] })));
    await createKubernetesApi("west").kubernetesChanges({ since: "2020-01-01T00:00:00Z", namespace: "shop", cursor: "expired", type: "deleted" });
    const restarted = new URL(String(fetch.mock.calls[1][0]), "http://localhost");
    expect(restarted.searchParams.has("cursor")).toBe(false);
    expect(restarted.searchParams.get("since")).toBe("2020-01-01T00:00:00Z");
    expect(restarted.searchParams.get("namespace")).toBe("shop");
    expect(restarted.searchParams.get("cluster")).toBe("west");
    expect(fetch.mock.calls.every((call) => !call[1]?.method || call[1]?.method === "GET")).toBe(true);
  });

  it("bounds empty-page work and rejects a nonadvancing continuation", async () => {
    let page = 0;
    const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async () => new Response(JSON.stringify({ items: [], next: `cursor-${++page}` })));
    const result = await api.kubernetesChanges();
    expect(fetch).toHaveBeenCalledTimes(16);
    expect(result.next).toBe("cursor-16");
    fetch.mockReset().mockImplementation(async () => new Response(JSON.stringify({ items: [], next: "repeated" })));
    await expect(api.kubernetesChanges()).rejects.toThrow("History continuation did not advance");
    expect(fetch).toHaveBeenCalledTimes(2);
  });

  it("never follows history continuations after permission or credential denial", async () => {
    const fetch = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify({ error: "denied", code: "credential_unavailable" }), { status: 502 }));
    await expect(api.kubernetesTimeline({ since: "2020-01-01T00:00:00Z", until: "2026-10-28T00:00:00Z", cursor: "position" })).rejects.toThrow();
    expect(fetch).toHaveBeenCalledTimes(1);
  });
  it("scopes issue alert reads and writes and sends only editable fields", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(() => Promise.resolve(new Response("{}")));
    const client = createKubernetesApi("prod/eu+one");
    const signal = new AbortController().signal;
    await client.kubernetesIssueAlerts(signal);
    await client.kubernetesSaveIssueAlerts({ enabled: true, level: "warning", can_write: true, enabled_at: "server-owned" } as Parameters<typeof client.kubernetesSaveIssueAlerts>[0]);
    for (const [input] of fetchMock.mock.calls) {
      const url = new URL(String(input), "http://localhost");
      expect(url.pathname).toBe("/api/admin/kubernetes/issue-alerts");
      expect(url.searchParams.get("cluster")).toBe("prod/eu+one");
    }
    expect(fetchMock.mock.calls[0][1]?.signal).toBe(signal);
    expect(fetchMock.mock.calls[1][1]?.method).toBe("PUT");
    expect(JSON.parse(String(fetchMock.mock.calls[1][1]?.body))).toEqual({ enabled: true, level: "warning" });
  });
  it("scopes Agent settings writes and tests without sending server-owned metadata", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(() => Promise.resolve(new Response("{}")));
    const client = createKubernetesApi("east");
    const fields = { namespace: "namespace", pod: "pod", container: "container" };
    const input = { learning: { logs: true, metrics: false, traces: false, scope: { include: ["shop"], exclude: [] } }, break_logs: { linked_source: "loki-prod", fields: { loki: fields, elasticsearch: fields, cloudwatchlogs: fields, graylog: fields, splunk: fields, signoz: fields } }, can_write: true, sources: [], detected: {} };
    await client.kubernetesAgentSettings();
    await client.kubernetesSaveAgentSettings(input);
    await client.kubernetesTestLink("loki-prod", fields);
    for (const [url] of fetchMock.mock.calls) expect(new URL(String(url), "http://localhost").searchParams.get("cluster")).toBe("east");
    expect(JSON.parse(String(fetchMock.mock.calls[1][1]?.body))).toEqual({ learning: input.learning, break_logs: input.break_logs });
    expect(JSON.parse(String(fetchMock.mock.calls[2][1]?.body))).toEqual({ source: "loki-prod", fields });
  });

  it("preserves break-log filters, deleted Pod names and opaque paging within the cluster", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify({ lines: [{ pod: "deleted-api", pod_uid: "" }], next: "opaque+next" })));
    const signal = new AbortController().signal;
    const result = await createKubernetesApi("prod").kubernetesBreakLogs({ namespace: "shop", root_uid: "root", since: "2026-10-17T10:00:00Z", until: "2026-10-17T10:07:00Z", pods: ["deleted-api", "uid-two"], mode: "previous", level: "error", q: "timeout + retry", cursor: "opaque+next", limit: 500 }, signal);
    const url = new URL(String(fetchMock.mock.calls[0][0]), "http://localhost");
    expect(url.pathname).toBe("/api/admin/kubernetes/break-logs");
    expect(url.searchParams.getAll("pods[]")).toEqual(["deleted-api", "uid-two"]);
    expect(url.searchParams.get("cluster")).toBe("prod");
    expect(url.searchParams.get("cursor")).toBe("opaque+next");
    expect(url.searchParams.get("q")).toBe("timeout + retry");
    expect(fetchMock.mock.calls[0][1]?.signal).toBe(signal);
    expect(result.lines[0]).toEqual({ pod: "deleted-api", pod_uid: "" });
  });

  it("scopes crash journal filters and requests only one retained page", async () => {
    const page = { items: [{ id: "retained" }], next: "next-page", retention_basis: "observed_at" };
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify(page)));
    const result = await createKubernetesApi("prod-eu").kubernetesCrashes({ namespace: "shop", root_uid: "root/one", since: "2026-10-16T10:00:00Z", until: "2026-10-16T11:00:00Z", limit: 100, cursor: "opaque+cursor" });
    const url = new URL(String(fetchMock.mock.calls[0][0]), "http://localhost");
    expect(url.pathname).toBe("/api/admin/kubernetes/crashes");
    expect(Object.fromEntries(url.searchParams)).toEqual({ cluster: "prod-eu", namespace: "shop", root_uid: "root/one", since: "2026-10-16T10:00:00Z", until: "2026-10-16T11:00:00Z", limit: "100", cursor: "opaque+cursor" });
    expect(result).toEqual(page);
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("scopes enriched broken roots and namespace-bound expansion", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(() => Promise.resolve(new Response("{}")));
    const client = createKubernetesApi("prod-eu");
    await client.kubernetesIssues({ broken: true, include: "changes", limit: 5 });
    await client.kubernetesIssues({ root_uid: "root-uid", include: "changes", namespace: "shop" });
    const urls = fetchMock.mock.calls.map(([input]) => new URL(String(input), "http://localhost"));
    expect(Object.fromEntries(urls[0].searchParams)).toEqual({ broken: "true", include: "changes", limit: "5", cluster: "prod-eu" });
    expect(Object.fromEntries(urls[1].searchParams)).toEqual({ root_uid: "root-uid", include: "changes", namespace: "shop", cluster: "prod-eu" });
  });

  it("preserves single URLs and encodes cluster scope without discarding filters", () => {
    expect(kubernetesClusterPath("/api/admin/kubernetes/overview")).toBe("/api/admin/kubernetes/overview");
    const url = new URL(kubernetesClusterPath("/api/admin/kubernetes/search?q=a%2Bb&cluster=old", "prod/eu+one"), "http://localhost");
    expect(url.searchParams.get("q")).toBe("a+b");
    expect(url.searchParams.getAll("cluster")).toEqual(["prod/eu+one"]);
  });

  it("scopes every Kubernetes JSON route including cached namespaces and directed relations", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation((input) => {
      const url = new URL(String(input), "http://localhost");
      return Promise.resolve(new Response(JSON.stringify({ items: [], pods: [], lines: [], truncated: false, ...(url.searchParams.get("resource_id") === "core~v1~namespaces" && !url.searchParams.has("continue") ? { continue: "page-two" } : {}) })));
    });
    const scoped = createKubernetesApi("prod-eu");
    await Promise.all([
      scoped.kubernetesOverview(), scoped.kubernetesOverviewGraph(), scoped.kubernetesIssues(), scoped.kubernetesChanges(), scoped.kubernetesCrashes(), scoped.kubernetesGraph(),
      scoped.kubernetesNeighborhood({ kind: "Pod", name: "api" }), scoped.kubernetesRelated({ resource_id: "core~v1~pods", name: "api" }), scoped.kubernetesTraffic(), scoped.kubernetesTop({ kind: "pod" }),
      scoped.kubernetesNodes(), scoped.kubernetesNamespaces(), scoped.kubernetesNodePods("node-a"), scoped.kubernetesUsage(), scoped.kubernetesWorkloads(),
      scoped.kubernetesWorkload("Pod", "shop", "api"), scoped.kubernetesWorkloadLogs("Pod", "shop", "api"), scoped.kubernetesPodLogs("shop", "api"),
      scoped.kubernetesDiagnose("core~v1~pods", "shop", "api"), scoped.kubernetesReleases(), scoped.kubernetesRelease("shop", "api"), scoped.kubernetesGitOpsApps(),
      scoped.kubernetesRollout("shop", "api"), scoped.kubernetesRollouts(), scoped.kubernetesSearch("shop", "api"), scoped.kubernetesEvents(), scoped.kubernetesDescribe("core~v1~pods", "shop", "api"),
    ]);
    expect(fetchMock).toHaveBeenCalledTimes(27);
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

  it("reads namespace names and honest sync status from the scoped cache endpoint", async () => {
    const payload = { items: ["platform", "payments"], sync: { state: "ready", age_s: 1, partial: true }, partial_failures: [{ class: "forbidden" }] };
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify(payload), { status: 200 }));
    expect(await createKubernetesApi("east").kubernetesNamespaces()).toEqual(payload);
    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual(["/api/admin/kubernetes/namespaces?cluster=east"]);
  });

  it("requests directed resource relationships without a neighborhood depth or node cap", async () => {
    const graph = { nodes: [], edges: [], omitted: {}, sync: { state: "ready", age_s: 0, partial: false } };
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify(graph), { status: 200 }));
    expect(await createKubernetesApi("east").kubernetesRelated({ resource_id: "apps~v1~deployments", namespace: "shop", name: "checkout" })).toEqual(graph);
    expect(String(fetchMock.mock.calls[0][0])).toBe("/api/admin/kubernetes/graph/related?resource_id=apps%7Ev1%7Edeployments&name=checkout&namespace=shop&cluster=east");
  });

  it("preserves root filtering on every changes cursor", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify({ items: [], sync: { state: "ready", age_s: 0, partial: false } }), { status: 200 }));
    await api.kubernetesChanges({ root_uid: "workload", namespace: "shop", cursor: "page-two" });
    const query = new URL(String(fetchMock.mock.calls[0][0]), "http://localhost").searchParams;
    expect(query.get("root_uid")).toBe("workload");
    expect(query.get("cursor")).toBe("page-two");
  });

  it("requests one scoped Timeline snapshot with exact resource identity and an unchanged opaque cursor", async () => {
    const payload = { items: [{ id: "retained" }], next: "snapshot.cursor", total: 2400, crash_count: 600, counts: { change: 1600, crash: 600, break: 200 }, since: "2026-10-23T07:00:00Z", until: "2026-10-23T08:00:00Z", gaps: [], sync: { state: "ready", age_s: 0, partial: false } };
    const fetchMock = vi.spyOn(globalThis, "fetch").mockImplementation(async () => new Response(JSON.stringify(payload), { status: 200 }));
    const client = createKubernetesApi("east");
    const signal = new AbortController().signal;
    const options = { since: payload.since, until: payload.until, namespace: "shop", resource_uid: "pod-uid", limit: 100 };
    expect(await client.kubernetesTimeline(options, signal)).toEqual(payload);
    await client.kubernetesTimeline({ ...options, cursor: payload.next }, signal);
    const urls = fetchMock.mock.calls.map(([input]) => new URL(String(input), "http://localhost"));
    expect(urls.every((url) => url.pathname === "/api/admin/kubernetes/timeline")).toBe(true);
    expect(Object.fromEntries(urls[0].searchParams)).toEqual({ ...options, limit: "100", cluster: "east" });
    expect(Object.fromEntries(urls[1].searchParams)).toEqual({ ...Object.fromEntries(urls[0].searchParams), cursor: "snapshot.cursor" });
    expect(fetchMock.mock.calls[1][1]?.signal).toBe(signal);
  });

  it("scopes Pod crash totals and paging by Pod UID rather than namespace-wide client filtering", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify({ items: [], crash_count: 400 }), { status: 200 }));
    await createKubernetesApi("east").kubernetesCrashes({ namespace: "shop", pod_uid: "pod-uid", cursor: "opaque" });
    const query = new URL(String(fetchMock.mock.calls[0][0]), "http://localhost").searchParams;
    expect(Object.fromEntries(query)).toEqual({ namespace: "shop", pod_uid: "pod-uid", cursor: "opaque", cluster: "east" });
  });
  it("encodes the saved namespace interest without changing cluster authorization scope", async () => {
    const fetchMock = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response(JSON.stringify({ items: [], sync: { state: "syncing", age_s: 0, partial: false } }), { status: 200 }));
    await createKubernetesApi("east").kubernetesNamespaces("payments");
    expect(String(fetchMock.mock.calls[0][0])).toBe("/api/admin/kubernetes/namespaces?namespace=payments&cluster=east");
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