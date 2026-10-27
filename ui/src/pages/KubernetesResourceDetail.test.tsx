// @vitest-environment jsdom
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  api,
  ApiError,
  type KubernetesCrashEvent,
  type KubernetesResource,
  type KubernetesTimelineEntry,
  type KubernetesTimelinePage,
  type KubernetesWorkload,
} from "@/lib/api";
import {
  ResourceEvents,
  ResourceFacts,
  ResourceHeader,
  ResourceRelations,
  ResourceTimeline,
  ResourceYaml,
} from "./KubernetesResourceDetail";
import { ResourceMetrics } from "./KubernetesResourceMetrics";
import {
  mergeUsageHistory,
  metricThreshold,
  quantity,
  resourceDetailTabs,
} from "@/lib/kubernetesResourceDetail";
import { projectedResourceYaml } from "@/lib/kubernetesProjection";
import { KubernetesFirstReadyContext } from "@/lib/useKubernetesCluster";

const now = "2026-10-21T08:00:00Z";
const pod: KubernetesResource = {
  resource_id: "core~v1~pods",
  kind: "Pod",
  namespace: "shop",
  name: "checkout-0",
  uid: "pod-uid",
  created_at: "2026-10-20T08:00:00Z",
  conditions: [
    {
      type: "Ready",
      status: "True",
      last_transition_at: "2026-10-21T07:55:00Z",
    },
  ],
  summary: {
    phase: "Running",
    node: "node-a",
    restart_count: 2,
    main_container: "app",
    log_container_states: [{ name: "app", ready: true, started_at: "2026-10-21T07:00:00Z" }],
  },
};
const crash: KubernetesCrashEvent = {
  id: "crash-a",
  sequence: 1,
  cluster: "production",
  namespace: "shop",
  pod: pod.name,
  pod_uid: pod.uid!,
  root_uid: "workload-uid",
  root_kind: "StatefulSet",
  root_name: "checkout",
  container: "app",
  type: "termination",
  termination_slot: "previous",
  reason: "OOMKilled",
  exit_code: 137,
  restart_count: 2,
  at: "2026-10-21T07:50:00Z",
  time_source: "finished_at",
  observed_at: "2026-10-21T07:50:02Z",
};
function timelinePage(items: KubernetesTimelineEntry[], extra: Partial<KubernetesTimelinePage> = {}): KubernetesTimelinePage {
  return {
    items, total: items.length, crash_count: items.filter((entry) => entry.type === "crash").length,
    counts: { change: items.filter((entry) => entry.type === "change").length, crash: items.filter((entry) => entry.type === "crash").length, break: items.filter((entry) => entry.type === "break").length },
    since: "2026-10-21T02:00:00Z", until: now, gaps: [], sync: { state: "ready", age_s: 0, partial: false }, ...extra,
  };
}
function mount(element: React.ReactNode) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return { client, ...render(
    <QueryClientProvider client={client}>
      {element}
    </QueryClientProvider>,
  ) };
}
beforeEach(() => {
  const change = { id: "change-a", at: "2026-10-21T07:40:00Z", type: "spec_changed" as const, kind: "Pod", namespace: "shop", name: pod.name, uid: pod.uid!, cluster: "production", fields: [{ path: "image", from: "old", to: "new" }] };
  vi.spyOn(api, "kubernetesTimeline").mockResolvedValue(timelinePage([
    { type: "crash", id: crash.id, at: crash.at, crash },
    { type: "change", id: change.id, at: change.at, change },
  ]));
  vi.spyOn(api, "kubernetesIssues").mockResolvedValue({
    items: [],
    totals: {},
    truncated: false,
    sync: { state: "ready", age_s: 0, partial: false },
  });
  vi.spyOn(Date, "now").mockReturnValue(Date.parse(now));
  vi.spyOn(api, "kubernetesUsageHistory").mockResolvedValue({
    observed_at: now,
    source: "capture",
    availability: "available",
    samples: [
      {
        timestamp: "2026-10-21T07:59:00Z",
        cpu_milli: 950,
        memory_bytes: 100 * 1024 ** 2,
      },
    ],
    retention_seconds: 21600,
  });
  vi.spyOn(api, "kubernetesCrashes").mockResolvedValue({
    items: [crash],
    crash_count: 1,
    since: "2026-10-20T08:00:00Z",
    until: now,
    started_at: "2026-10-20T08:00:00Z",
    retention_from: "2026-10-20T08:00:00Z",
    retention_basis: "capture",
    gaps: [],
    capture_gaps: [],
    coverage: {},
    limitations: [],
    capture: { mode: "continuous", behind: false },
    sync: { state: "ready", age_s: 0, partial: false },
  });
  vi.spyOn(api, "kubernetesChanges").mockResolvedValue({
    items: [
      {
        id: "change-a",
        at: "2026-10-21T07:40:00Z",
        type: "spec_changed",
        kind: "Pod",
        namespace: "shop",
        name: pod.name,
        fields: [{ path: "image", from: "old", to: "new" }],
      },
    ],
    sync: { state: "ready", age_s: 0, partial: false },
  });
  vi.spyOn(api, "kubernetesRelated").mockResolvedValue({
    nodes: [
      {
        id: "pod",
        kind: "Pod",
        namespace: "shop",
        name: pod.name,
        group: "workload",
        health: "healthy",
      },
      {
        id: "owner",
        kind: "StatefulSet",
        namespace: "shop",
        name: "checkout",
        group: "workload",
        health: "healthy",
      },
      {
        id: "secret",
        kind: "Secret",
        namespace: "shop",
        name: "credentials",
        group: "configuration",
      },
    ],
    edges: [
      { from: "owner", to: "pod", type: "manages" },
      { from: "pod", to: "secret", type: "uses" },
    ],
    omitted: {},
    sync: { state: "ready", age_s: 0, partial: false },
  });
});
afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("Resource detail surfaces", () => {
  it("normalizes CPU and memory independently, fills only contiguous memory, and keeps the now marker finite", async () => {
    vi.mocked(api.kubernetesUsageHistory).mockResolvedValue({
      observed_at: now, source: "capture", availability: "available", retention_seconds: 21600,
      samples: [
        { timestamp: "2026-10-21T07:58:00Z", cpu_milli: 250, memory_bytes: 128 * 1024 ** 2 },
        { timestamp: "2026-10-21T07:59:00Z", cpu_milli: 500, memory_bytes: 256 * 1024 ** 2 },
      ],
    });
    const { container } = mount(<ResourceMetrics resource={pod} workload={{ ...pod, truncated: false, containers: [{ name: "app", requests: { cpu: "250m", memory: "256Mi" }, limits: { cpu: "1", memory: "512Mi" } }] }} liveSamples={[]} onCrash={vi.fn()} />);
    await screen.findByLabelText("500 mCPU");
    const cpu = container.querySelector('[data-series="cpu"]')!;
    const memory = container.querySelector('[data-series="memory"]')!;
    expect(cpu.getAttribute("d")).toBe(memory.getAttribute("d"));
    expect(container.querySelectorAll("[data-memory-area]")).toHaveLength(1);
    expect(container.querySelector("[data-memory-area]")?.getAttribute("d")).toMatch(/Z$/);
    expect(container.querySelector("[data-now-marker]")?.getAttribute("x1")).toBe("768");
    expect(container.querySelector("svg[aria-label^='Workload CPU']")?.outerHTML).not.toMatch(/NaN|Infinity/);
    expect(screen.getByText("512 Mi", { selector: "text" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Inspect Memory history" }).getAttribute("aria-pressed")).toBe("true");
  });
  it("leaves sparse historical samples separated instead of drawing invented memory area", async () => {
    vi.mocked(api.kubernetesUsageHistory).mockResolvedValue({
      observed_at: now, source: "capture", availability: "available", retention_seconds: 21600,
      samples: [
        { timestamp: "2026-10-21T07:30:00Z", cpu_milli: 250, memory_bytes: 128 * 1024 ** 2 },
        { timestamp: "2026-10-21T07:59:00Z", cpu_milli: 500, memory_bytes: 256 * 1024 ** 2 },
      ],
    });
    const { container } = mount(<ResourceMetrics resource={pod} liveSamples={[]} onCrash={vi.fn()} />);
    await screen.findByLabelText("500 mCPU");
    expect(container.querySelectorAll("[data-memory-area]")).toHaveLength(0);
    expect(container.querySelector('[data-series="memory"]')?.getAttribute("d")?.match(/M/g)).toHaveLength(2);
    expect(container.querySelectorAll('[data-reference="request"],[data-reference="limit"]')).toHaveLength(0);
  });
  it("uses the restart alias consistently in header and metric tile and never renders invalid counts", async () => {
    const alias = { ...pod, summary: { ...pod.summary, restart_count: undefined, restarts: "7" } };
    const { unmount } = mount(<><ResourceHeader resource={alias} /><ResourceMetrics resource={alias} liveSamples={[]} onCrash={vi.fn()} /></>);
    expect(screen.getByText("7 restarts")).toBeTruthy();
    expect(screen.getByLabelText("Restarts metric tile").querySelector("p")?.textContent).toMatch(/^7/);
    unmount();
    const invalid = { ...alias, summary: { restart_count: "not a count" } };
    mount(<><ResourceHeader resource={invalid} /><ResourceMetrics resource={invalid} liveSamples={[]} onCrash={vi.fn()} /></>);
    expect(screen.getByText("? restarts")).toBeTruthy();
    expect(screen.getByLabelText("Restarts metric tile").querySelector("p")?.textContent).toMatch(/^Unavailable/);
    expect(document.body.textContent).not.toContain("NaN");
  });
  it("puts a scaler in the directed ancestry chain and deduplicates empty namespace references", async () => {
    vi.mocked(api.kubernetesRelated).mockResolvedValue({
      nodes: [
        { id: "p", kind: "Pod", namespace: "shop", name: pod.name, group: "workload", health: "healthy" },
        { id: "rs", kind: "ReplicaSet", namespace: "shop", name: "checkout-rs", group: "workload", health: "healthy" },
        { id: "d", kind: "Deployment", namespace: "", name: "checkout", group: "workload", health: "degraded" },
        { id: "h", kind: "HorizontalPodAutoscaler", namespace: "shop", name: "checkout-hpa", group: "workload" },
        { id: "s", kind: "Service", namespace: "shop", name: "checkout-svc", group: "network" },
      ],
      edges: [{ from: "h", to: "d", type: "scales" }, { from: "d", to: "rs", type: "manages" }, { from: "rs", to: "p", type: "manages" }, { from: "s", to: "p", type: "exposes" }],
      omitted: {}, sync: { state: "ready", age_s: 0, partial: false },
    });
    const onOpen = vi.fn();
    mount(<ResourceRelations resource={pod} related={[{ kind: "Deployment", name: "checkout" }, { kind: "Deployment", name: "checkout", namespace: "", summary: null }, { kind: "ConfigMap", name: "unknown-reference" }]} onOpen={onOpen} />);
    const hpa = await screen.findByRole("button", { name: "Open HorizontalPodAutoscaler checkout-hpa" });
    const owned = screen.getByRole("heading", { name: "Owned by" }).closest("section")!;
    expect(within(owned).getAllByText("checkout")).toHaveLength(1);
    expect(within(owned).getByText("checkout-hpa")).toBeTruthy();
    expect(document.querySelector('[data-relationship-from="h"][data-relationship-to="d"]')?.textContent).toContain("scales");
    expect(document.querySelector('[data-relationship-from="s"][data-relationship-to="p"]')?.textContent).toContain("selected by");
    expect(document.body.textContent).not.toContain("2 of 2 ready");
    expect(screen.queryByText("unknown-reference")).toBeNull();
    expect(screen.getByRole("heading", { name: "Uses", exact: true }).closest("section")?.textContent).not.toContain("unknown-reference");
    fireEvent.click(hpa);
    expect(onOpen).toHaveBeenCalledWith(expect.objectContaining({ kind: "HorizontalPodAutoscaler", namespace: "shop" }));
  });
  it("keeps projected references when the relationship graph request fails", async () => {
    vi.mocked(api.kubernetesRelated).mockRejectedValue(new ApiError(503, "unavailable"));
    mount(<ResourceRelations resource={pod} related={[{ kind: "ConfigMap", name: "checkout-config" }, { kind: "Service", name: "checkout-svc" }]} onOpen={vi.fn()} />);
    expect(await screen.findByText("Relationship health is unavailable; projected references are shown.")).toBeTruthy();
    const section = screen.getByRole("region", { name: "Related resources" });
    const references = within(section).getByRole("heading", { name: "References" }).closest("section")!;
    expect(within(references).getByText("checkout-config")).toBeTruthy();
    expect(within(references).getByText("checkout-svc")).toBeTruthy();
    expect(within(references).queryByText("No projected relationships.")).toBeNull();
  });
  it("keeps references to kinds the graph is still loading", async () => {
    vi.mocked(api.kubernetesRelated).mockResolvedValue({
      nodes: [{ id: "p", kind: "Pod", namespace: "shop", name: pod.name, group: "workload", health: "healthy" }],
      edges: [], omitted: {}, sync: { state: "partial", age_s: 0, partial: true, pending: ["core~v1~configmaps"] },
    });
    mount(<ResourceRelations resource={pod} related={[{ kind: "ConfigMap", name: "checkout-config" }, { kind: "Secret", name: "stale-secret" }]} onOpen={vi.fn()} />);
    await screen.findByText("Loading ConfigMap");
    expect(screen.getAllByText("checkout-config").length).toBeGreaterThan(0);
    expect(screen.queryByText("stale-secret")).toBeNull();
  });
  it("names related kinds that are still loading above the map without changing it", async () => {
    vi.mocked(api.kubernetesRelated).mockResolvedValue({
      nodes: [{ id: "p", kind: "Pod", namespace: "shop", name: pod.name, group: "workload", health: "healthy" }],
      edges: [], omitted: {}, sync: { state: "partial", age_s: 0, partial: true, pending: ["core~v1~configmaps", "core~v1~serviceaccounts"] },
    });
    mount(<ResourceRelations resource={pod} related={[]} onOpen={vi.fn()} />);
    const note = await screen.findByText("Loading ConfigMap, ServiceAccount");
    const section = screen.getByRole("region", { name: "Related resources" });
    expect(section.contains(note)).toBe(true);
    expect(within(section).getByLabelText("Relationship map")).toBeTruthy();
    expect(note.closest("[data-pending-note]")?.className).not.toMatch(/warn|amber|sev-/);
  });
  it("lays out a busy relationship map with spaced nodes and edges that never cross unrelated nodes", async () => {
    const deployment: KubernetesResource = { ...pod, kind: "Deployment", name: "checkout-api-with-a-long-deployment-name", uid: "deployment", resource_id: "apps~v1~deployments", summary: {} };
    const dependency = (id: string, kind: string, name: string, group: string) => ({ id, kind, namespace: "shop", name, group });
    vi.mocked(api.kubernetesRelated).mockResolvedValue({
      nodes: [
        { id: "d", kind: "Deployment", namespace: "shop", name: deployment.name, group: "workload", health: "healthy" },
        { id: "h", kind: "HorizontalPodAutoscaler", namespace: "shop", name: "checkout-hpa", group: "workload" },
        ...[0, 1, 2].map((n) => dependency(`rs${n}`, "ReplicaSet", `checkout-api-rs-${n}`, "workload")),
        ...[0, 1, 2].map((n) => dependency(`cm${n}`, "ConfigMap", `checkout-config-number-${n}`, "configuration")),
        ...[0, 1].map((n) => dependency(`sec${n}`, "Secret", `checkout-credentials-${n}`, "configuration")),
        dependency("svc", "Service", "checkout-svc", "network"),
        dependency("sa", "ServiceAccount", "checkout-runtime-identity", "configuration"),
        dependency("pvc", "PersistentVolumeClaim", "checkout-data", "storage"),
      ],
      edges: [
        { from: "h", to: "d", type: "scales" },
        ...[0, 1, 2].map((n) => ({ from: "d", to: `rs${n}`, type: "manages" as const })),
        ...[0, 1, 2].map((n) => ({ from: "d", to: `cm${n}`, type: "uses" as const })),
        ...[0, 1].map((n) => ({ from: "d", to: `sec${n}`, type: "uses" as const })),
        { from: "svc", to: "d", type: "exposes" },
        { from: "d", to: "sa", type: "uses" },
        { from: "d", to: "pvc", type: "uses" },
      ],
      omitted: {}, sync: { state: "ready", age_s: 0, partial: false },
    });
    const { container } = mount(<ResourceRelations resource={deployment} onOpen={vi.fn()} />);
    await screen.findByRole("button", { name: "Open ConfigMap checkout-config-number-2" });

    type Rect = { id: string; x: number; y: number; w: number; h: number };
    const rects: Rect[] = [...container.querySelectorAll<SVGGElement>("g.relationship-node")].map((node) => {
      const [, x, y] = node.getAttribute("transform")!.match(/translate\(([-\d.]+) ([-\d.]+)\)/)!.map(Number);
      const rect = node.querySelector("rect")!;
      return { id: node.getAttribute("data-relationship-node")!, x, y, w: Number(rect.getAttribute("width")), h: Number(rect.getAttribute("height")) };
    });
    expect(rects).toHaveLength(13);
    const chainY = Math.min(...rects.map((rect) => rect.y));
    for (const [i, a] of rects.entries()) {
      for (const b of rects.slice(i + 1)) {
        const horizontal = Math.max(b.x - (a.x + a.w), a.x - (b.x + b.w));
        const vertical = Math.max(b.y - (a.y + a.h), a.y - (b.y + b.h));
        const minVertical = a.y === chainY || b.y === chainY ? 56 : 72;
        expect(horizontal >= 32 || vertical >= minVertical, `${a.id} and ${b.id} are too close`).toBe(true);
      }
    }
    // Long names keep more than the old 20 characters visible and the full name in the tooltip.
    const sa = container.querySelector('g[data-relationship-node="sa"]')!;
    expect(sa.querySelector(".relationship-name")?.textContent).toBe("checkout-runtime-identity");
    expect(sa.querySelector("title")?.textContent).toContain("checkout-runtime-identity");

    type Segment = [number, number, number, number];
    const edgeGroups = [...container.querySelectorAll<SVGGElement>("g[data-relationship-node-ref]")];
    expect(edgeGroups).toHaveLength(11);
    const segmentsOf = (group: SVGGElement): Segment[] => {
      const points = [...group.querySelector("path")!.getAttribute("d")!.matchAll(/[ML]([-\d.]+) ([-\d.]+)/g)].map((match) => [Number(match[1]), Number(match[2])]);
      return points.slice(1).map((point, index) => [points[index][0], points[index][1], point[0], point[1]]);
    };
    const crosses = ([x1, y1, x2, y2]: Segment, rect: Rect) =>
      Math.max(Math.min(x1, x2), rect.x) < Math.min(Math.max(x1, x2), rect.x + rect.w) - 0.01 && Math.max(Math.min(y1, y2), rect.y) < Math.min(Math.max(y1, y2), rect.y + rect.h) - 0.01
      || (x1 === x2 && x1 > rect.x && x1 < rect.x + rect.w && Math.min(y1, y2) < rect.y + rect.h && Math.max(y1, y2) > rect.y)
      || (y1 === y2 && y1 > rect.y && y1 < rect.y + rect.h && Math.min(x1, x2) < rect.x + rect.w && Math.max(x1, x2) > rect.x);
    const distance = ([x1, y1, x2, y2]: Segment, box: { x0: number; x1: number; y0: number; y1: number }) => {
      const dx = Math.max(0, box.x0 - Math.max(x1, x2), Math.min(x1, x2) - box.x1);
      const dy = Math.max(0, box.y0 - Math.max(y1, y2), Math.min(y1, y2) - box.y1);
      return Math.hypot(dx, dy);
    };
    for (const group of edgeGroups) {
      const from = group.getAttribute("data-relationship-from");
      const to = group.getAttribute("data-relationship-to");
      const own = group.getAttribute("data-relationship-node-ref")!;
      expect([from, to].sort()).toEqual(["d", own].sort());
      const segments = segmentsOf(group);
      for (const segment of segments) {
        expect(segment[0] === segment[2] || segment[1] === segment[3], `${own} has a diagonal segment`).toBe(true);
        for (const rect of rects.filter((candidate) => candidate.id !== from && candidate.id !== to)) {
          expect(crosses(segment, rect), `${own} edge crosses ${rect.id}`).toBe(false);
        }
      }
      // The label sits directly above its own node, beside its own edge, and away from every node.
      const label = group.querySelector("text")!;
      const lx = Number(label.getAttribute("x")), ly = Number(label.getAttribute("y"));
      const labelWidth = (label.textContent ?? "").length * 5.5;
      const box = label.getAttribute("text-anchor") === "middle"
        ? { x0: lx - labelWidth / 2, x1: lx + labelWidth / 2, y0: ly - 9, y1: ly }
        : { x0: lx, x1: lx + labelWidth, y0: ly - 9, y1: ly };
      const node = rects.find((rect) => rect.id === own)!;
      expect(node.y - box.y1).toBeGreaterThanOrEqual(0);
      expect(node.y - box.y1).toBeLessThanOrEqual(24);
      expect(box.x0 >= node.x - 32 && box.x1 <= node.x + node.w, `${own} label leaves its column`).toBe(true);
      for (const rect of rects) {
        expect(box.x1 <= rect.x || box.x0 >= rect.x + rect.w || box.y1 <= rect.y || box.y0 >= rect.y + rect.h, `${own} label overlaps ${rect.id}`).toBe(true);
      }
      const ownDistance = Math.min(...segments.map((segment) => distance(segment, box)));
      expect(ownDistance).toBeLessThanOrEqual(8);
      for (const other of edgeGroups.filter((candidate) => candidate !== group)) {
        const unshared = segmentsOf(other).filter((segment) => !segments.some((mine) => mine.join() === segment.join() || [mine[2], mine[3], mine[0], mine[1]].join() === segment.join()));
        for (const segment of unshared) expect(distance(segment, box), `${own} label is nearer another edge`).toBeGreaterThan(ownDistance);
      }
    }
    expect(container.querySelector('g[data-relationship-from="svc"][data-relationship-to="d"]')?.textContent).toBe("selected by");
  });
  it.each(["next", "gap", "permission"])("keeps resource Timeline coverage quiet for %s without extra caveats", async (kind) => {
    vi.mocked(api.kubernetesTimeline).mockResolvedValue(timelinePage([], {
      next: kind === "next" ? "more" : undefined,
      gaps: kind === "gap" ? [{ from: "2026-10-21T04:00:00Z", to: now }] : [],
      partial_failures: kind === "permission" ? [{ resource_id: "core~v1~pods", class: "forbidden" }] : [],
    }));
    mount(<ResourceTimeline resource={pod} onCrash={vi.fn()} />);
    await screen.findByText("No changes or crashes in this range.");
    expect(screen.queryByText(/Partial history; capture gaps or bounded results/)).toBeNull();
    expect(screen.queryAllByRole("button", { name: "Resource coverage" })).toHaveLength(kind === "next" ? 0 : 1);
    expect(screen.queryByText(/^No history before/)).toBeNull();
  });
  it("defines only the six approved views", () => {
    expect(resourceDetailTabs.map((tab) => tab.label)).toEqual([
      "Overview",
      "Logs",
      "Events",
      "YAML",
      "Related",
      "Timeline",
    ]);
  });
  it("shows header identity, readiness, restart count and running uptime", () => {
    mount(
      <h2>
        <ResourceHeader resource={pod} />
      </h2>,
    );
    expect(screen.getByText("checkout-0")).toBeTruthy();
    expect(screen.getByText("Ready 1/1")).toBeTruthy();
    expect(screen.getByText("2 restarts").className).toContain(
      "text-sev-warning",
    );
    expect(screen.getByText(/shop on node-a/).textContent).toContain(
      "up 1h 0m",
    );
  });
  it("keeps unknown resource status neutral and makes mobile facts available on demand", () => {
    const unknown = { ...pod, summary: {}, conditions: [] };
    const { unmount } = mount(<ResourceHeader resource={unknown} />);
    expect(screen.getByText("Status unavailable")).toBeTruthy();
    expect(screen.getByText("Ready ?")).toBeTruthy();
    expect(screen.getByText("? restarts")).toBeTruthy();
    expect(screen.queryByText("0 restarts")).toBeNull();
    unmount();
    mount(<ResourceFacts resource={pod} onOpen={vi.fn()} />);
    const control = screen.getByRole("button", { name: /Details and conditions/ });
    expect(control.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(control);
    expect(control.getAttribute("aria-expanded")).toBe("true");
    expect(document.getElementById(control.getAttribute("aria-controls")!)?.dataset.mobileOpen).toBe("true");
  });
  it.each([
    [75, "text-ink-50"],
    [75.1, "text-sev-warning"],
    [90, "text-sev-warning"],
    [90.1, "text-sev-critical"],
    [null, "text-ink-400"],
  ] as const)("uses precise tile threshold %s", (percent, expected) => {
    expect(metricThreshold(percent)).toBe(expected);
  });
  it("aggregates available aligned usage samples and parses projected rational quantities", () => {
    expect(quantity("1/2")).toBe(0.5);
    expect(quantity("256Mi")).toBe(256 * 1024 ** 2);
    expect(quantity("invalid")).toBeNull();
    const sample = { observedAt: Date.parse(now), cpu: 0.1, memory: 100 };
    expect(
      mergeUsageHistory([
        [sample],
        [{ ...sample, observedAt: sample.observedAt + 5000 }],
      ]),
    ).toEqual([
      {
        ...sample,
        cpu: 0.2,
        memory: 200,
        coverage: { total: 2, cpu: 2, memory: 2 },
      },
    ]);
    expect(mergeUsageHistory([[sample], []])).toEqual([
      { ...sample, coverage: { total: 2, cpu: 1, memory: 1 } },
    ]);
  });
  it("retains disjoint history buckets and reports missing fields rather than inventing zero usage", () => {
    const first = {
      observedAt: Date.parse(now) - 120000,
      cpu: 0,
      memory: null,
    };
    const later = {
      observedAt: Date.parse(now) - 60000,
      cpu: 0.4,
      memory: 128,
    };
    expect(mergeUsageHistory([[first], [later], []])).toEqual([
      { ...first, coverage: { total: 3, cpu: 1, memory: 0 } },
      { ...later, coverage: { total: 3, cpu: 1, memory: 1 } },
    ]);
    expect(mergeUsageHistory([[], []])).toEqual([]);
  });
  it("retains rollout history with an unsampled or departed Pod and labels partial subtotals without percentages", async () => {
    let joined = false;
    vi.mocked(api.kubernetesUsageHistory).mockImplementation(
      async (_namespace, name) => ({
        observed_at: now,
        source: "capture",
        availability: "available",
        retention_seconds: 21600,
        samples:
          name === "old-pod"
            ? [
                {
                  timestamp: "2026-10-21T07:58:00Z",
                  cpu_milli: 250,
                  memory_bytes: 64 * 1024 ** 2,
                },
              ]
            : joined
              ? [
                  {
                    timestamp: "2026-10-21T07:59:00Z",
                    cpu_milli: 400,
                    memory_bytes: 128 * 1024 ** 2,
                  },
                ]
              : [],
      }),
    );
    const resource = {
      ...pod,
      kind: "Deployment",
      name: "checkout",
      uid: "workload-uid",
      resource_id: "apps~v1~deployments",
    };
    const workload: KubernetesWorkload = {
      ...resource,
      containers: [{ name: "app", limits: { cpu: "1", memory: "256Mi" } }],
      pods: [{ name: "old-pod", restart_count: 0 }],
      truncated: false,
    };
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    const view = (selected: KubernetesResource, detail: KubernetesWorkload) => (
      <QueryClientProvider client={client}>
        <ResourceMetrics
          resource={selected}
          workload={detail}
          liveSamples={[]}
          onCrash={vi.fn()}
        />
      </QueryClientProvider>
    );
    const { rerender } = render(view(resource, workload));
    await screen.findByLabelText("250 mCPU");
    const rollout = {
      ...workload,
      pods: [...workload.pods!, { name: "new-pod", restart_count: 0 }],
    };
    rerender(view(resource, rollout));
    expect(await screen.findAllByText("Available samples 1 of 2 tracked Pods")).toHaveLength(2);
    expect(screen.getByLabelText("250 mCPU")).toBeTruthy();
    expect(screen.queryByText(/%/)).toBeNull();
    expect(screen.queryByText("since opened")).toBeNull();
    expect(
      screen.getAllByText("Available samples 1 of 2 tracked Pods"),
    ).toHaveLength(2);
    joined = true;
    await act(async () => {
      await client.invalidateQueries({
        queryKey: ["kubernetes", "_", "resource-usage-history"],
      });
    });
    await waitFor(() =>
      expect(screen.getAllByText("Usage samples (2)")).toHaveLength(2),
    );
    expect(screen.getByLabelText("400 mCPU")).toBeTruthy();
    rerender(
      view(resource, {
        ...workload,
        pods: [{ name: "new-pod", restart_count: 0 }],
      }),
    );
    expect(screen.getAllByText("Usage samples (2)")).toHaveLength(2);
    expect(screen.queryByText(/tracked Pod series/)).toBeNull();
    expect(screen.queryByText(/%/)).toBeNull();
    rerender(
      view(
        { ...resource, name: "other", uid: "other-uid" },
        {
          ...workload,
          name: "other",
          pods: [{ name: "new-pod", restart_count: 0 }],
        },
      ),
    );
    await waitFor(() =>
      expect(screen.getAllByText("Usage samples (1)")).toHaveLength(2),
    );
    expect(screen.queryByLabelText("250 mCPU")).toBeNull();
    expect(screen.queryByText(/Partial history/)).toBeNull();
  });
  it("renders history, allocations, threshold colour, uptime and clickable journal crashes", async () => {
    const onCrash = vi.fn();
    const workload = {
      resource_id: pod.resource_id,
      kind: "Pod",
      namespace: "shop",
      name: pod.name,
      containers: [
        {
          name: "app",
          limits: { cpu: "1", memory: "128Mi" },
          requests: { cpu: "250m", memory: "64Mi" },
        },
      ],
      truncated: false,
    };
    const { container } = mount(
      <ResourceMetrics
        resource={pod}
        workload={workload}
        liveSamples={[]}
        onCrash={onCrash}
      />,
    );
    expect(await screen.findByLabelText("950 mCPU")).toBeTruthy();
    expect(screen.getByLabelText("950 mCPU").className).toContain(
      "text-sev-critical",
    );
    expect(within(screen.getByLabelText("CPU metric tile")).getByText("95%")).toBeTruthy();
    expect(screen.getByText("of 1 core")).toBeTruthy();
    expect(container.querySelectorAll('[data-reference="limit"]')).toHaveLength(
      2,
    );
    expect(
      container.querySelectorAll('[data-reference="request"]'),
    ).toHaveLength(2);
    expect(container.querySelectorAll("[data-crash-marker]")).toHaveLength(1);
    const chart = screen.getByRole("figure", { name: "CPU and memory usage chart" });
    expect(chart.querySelectorAll("svg")).toHaveLength(1);
    expect(chart.querySelectorAll("[data-series]")).toHaveLength(2);
    fireEvent.click(
      screen.getByRole("button", { name: `Crash at ${crash.at}` }),
    );
    expect(onCrash).toHaveBeenCalledWith(crash);
    fireEvent.click(screen.getByRole("button", { name: "6h" }));
    expect(
      screen.getByRole("button", { name: "6h" }).getAttribute("aria-pressed"),
    ).toBe("true");
  });
  it("falls back to since-opened live samples instead of inventing empty history", async () => {
    vi.mocked(api.kubernetesUsageHistory).mockRejectedValue(
      new Error("unavailable"),
    );
    mount(
      <ResourceMetrics
        resource={pod}
        liveSamples={[
          {
            observedAt: Date.parse(now) - 1000,
            cpu: 0.2,
            memory: 128 * 1024 ** 2,
          },
        ]}
        onCrash={vi.fn()}
      />,
    );
    expect(await screen.findByLabelText("200 mCPU")).toBeTruthy();
    expect(screen.getByText("since opened")).toBeTruthy();
  });
  it("groups events by reason, filters severity and search, and shows source and histogram", () => {
    const events: KubernetesResource[] = [
      {
        ...pod,
        kind: "Event",
        uid: "one",
        name: "one",
        summary: {
          type: "Warning",
          reason: "BackOff",
          count: 3,
          message: "Back-off restarting failed container",
          firstTimestamp: "2026-10-21T07:50:00Z",
          lastTimestamp: "2026-10-21T07:59:00Z",
          source_component: "kubelet",
          source_host: "node-a",
        },
      },
      {
        ...pod,
        kind: "Event",
        uid: "two",
        name: "two",
        summary: {
          type: "Normal",
          reason: "Started",
          message: "Started container app",
          lastTimestamp: "2026-10-21T07:58:00Z",
        },
      },
    ];
    mount(<ResourceEvents events={events} />);
    expect(screen.getByRole("img", { name: /Event histogram/ })).toBeTruthy();
    expect(screen.getByText("kubelet node-a")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Warnings 3" }));
    expect(screen.queryByRole("heading", { name: "Started" })).toBeNull();
    expect(screen.getByRole("heading", { name: "BackOff" })).toBeTruthy();
    fireEvent.change(screen.getByLabelText("Search events"), {
      target: { value: "absent" },
    });
    expect(screen.getByText("No events match this filter.")).toBeTruthy();
  });
  it("folds projected YAML, unfolds search targets and outlines blocks without leaking values", () => {
    mount(
      <ResourceYaml
        resource={{
          ...pod,
          summary: {
            safe: { image: "checkout:v2" },
            data: { password: "never-show-this" },
          },
        }}
      />,
    );
    expect(screen.getByText("Projected, read-only")).toBeTruthy();
    expect(
      screen.getByRole("region", { name: "Projected YAML" }).textContent,
    ).not.toContain("never-show-this");
    fireEvent.click(screen.getByRole("button", { name: "Fold all" }));
    expect(screen.queryByText(/checkout:v2/)).toBeNull();
    fireEvent.change(screen.getByLabelText("Find in YAML"), {
      target: { value: "checkout:v2" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Next match" }));
    expect(screen.getByText(/checkout:v2/)).toBeTruthy();
    expect(
      screen.getByRole("navigation", { name: "YAML outline" }),
    ).toBeTruthy();
    expect(
      projectedResourceYaml({ ...pod, summary: { token: "never-show-this" } }),
    ).not.toContain("never-show-this");
  });
  it("opens typed relationships and labels Secrets without content", async () => {
    const onOpen = vi.fn();
    mount(<ResourceRelations resource={pod} related={[]} onOpen={onOpen} />);
    await waitFor(() =>
      expect(screen.getAllByText("content not shown")).toHaveLength(1),
    );
    const ownerGroup = screen
      .getByRole("heading", { name: "Owned by" })
      .closest("section")!;
    fireEvent.click(within(ownerGroup).getByRole("button", { name: "Open" }));
    expect(onOpen).toHaveBeenCalledWith(
      expect.objectContaining({
        kind: "StatefulSet",
        resource_id: "apps~v1~statefulsets",
        name: "checkout",
      }),
    );
  });
  it("shows changes and crashes on the resource lane and opens previous logs", async () => {
    const onCrash = vi.fn();
    mount(<ResourceTimeline resource={pod} onCrash={onCrash} />);
    fireEvent.click(
      await screen.findByRole("button", { name: "View logs before crash" }),
    );
    expect(onCrash).toHaveBeenCalledWith(crash);
    expect(screen.getByText(/^image/, { selector: "p" }).textContent).toContain(
      "old",
    );
    expect(screen.queryByText(/Crash history starts/)).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "24h" }));
    await waitFor(() =>
      expect(api.kubernetesTimeline).toHaveBeenLastCalledWith(
        expect.objectContaining({ resource_uid: pod.uid, since: "2026-10-20T08:00:00.000Z" }),
        expect.any(AbortSignal),
      ),
    );
  });
  it("does not show crashes from same-name replacement Pods", async () => {
    vi.mocked(api.kubernetesTimeline).mockResolvedValue(timelinePage([]));
    mount(
      <ResourceTimeline
        resource={{ ...pod, uid: "replacement" }}
        onCrash={vi.fn()}
      />,
    );
    await waitFor(() => expect(api.kubernetesTimeline).toHaveBeenCalledWith(expect.objectContaining({ resource_uid: "replacement" }), expect.any(AbortSignal)));
    expect(
      screen.queryByRole("button", { name: "View logs before crash" }),
    ).toBeNull();
  });
  it("keeps a syncing resource snapshot loading instead of claiming an empty range", async () => {
    vi.mocked(api.kubernetesTimeline).mockResolvedValue(timelinePage([], { sync: { state: "syncing", age_s: 0, partial: false } }));
    const { client } = mount(<ResourceTimeline resource={pod} onCrash={vi.fn()} />);
    await waitFor(() => expect(client.getQueryCache().getAll().find((query) => query.queryKey.includes("resource-detail-timeline"))?.state.status).toBe("success"));
    expect(screen.getByText("Loading resource history.")).toBeTruthy();
    expect(screen.queryByText("No changes or crashes in this range.")).toBeNull();
  });
  it("restarts an expired resource snapshot without reusing a continuation or changing its filters", async () => {
    const first = timelinePage([{ type: "crash", id: crash.id, at: crash.at, crash }], { next: "expired-snapshot" });
    vi.mocked(api.kubernetesTimeline).mockImplementation(async (options) => {
      if (options.cursor) throw new ApiError(400, "invalid arguments");
      return first;
    });
    mount(<ResourceTimeline resource={pod} onCrash={vi.fn()} />);
    await screen.findByRole("button", { name: "View logs before crash" });
    const original = vi.mocked(api.kubernetesTimeline).mock.calls[0][0];
    fireEvent.click(screen.getByRole("button", { name: "Next page" }));
    fireEvent.click(await screen.findByRole("button", { name: "Restart history" }));
    await screen.findByRole("button", { name: "View logs before crash" });
    expect(vi.mocked(api.kubernetesTimeline).mock.calls.map(([options]) => options.cursor)).toEqual([undefined, "expired-snapshot", undefined]);
    expect(vi.mocked(api.kubernetesTimeline).mock.calls[2][0]).toEqual(original);
    expect(api.kubernetesCrashes).not.toHaveBeenCalled();
    expect(api.kubernetesChanges).not.toHaveBeenCalled();
  });
  it("loads only a selected crash page, keeps its window frozen and uses the aggregate termination count", async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date(now));
    const first = { items: [crash], crash_count: 2400, since: "2026-10-20T08:00:00Z", until: now, started_at: now, retention_from: now, retention_basis: "capture", gaps: [], capture_gaps: [], coverage: {}, limitations: [], capture: { mode: "continuous", behind: false }, sync: { state: "ready", age_s: 0, partial: false }, next: "opaque-next" };
    vi.mocked(api.kubernetesCrashes).mockImplementation(async (options) => ({ ...first, items: options?.cursor ? [{ ...crash, id: "older-crash" }] : [crash], next: options?.cursor ? undefined : first.next }));
    mount(<ResourceMetrics resource={pod} liveSamples={[]} onCrash={vi.fn()} />);
    await act(async () => { await vi.advanceTimersByTimeAsync(0); });
    expect(api.kubernetesCrashes).toHaveBeenCalledTimes(1);
    const request = vi.mocked(api.kubernetesCrashes).mock.calls[0][0]!;
    expect(request).toMatchObject({ pod_uid: pod.uid, limit: 50, cursor: undefined });
    expect(screen.getByText("2400 captured crashes in the last 24h")).toBeTruthy();
    const pager = screen.getByRole("navigation", { name: "Captured crash pages" });
    fireEvent.click(within(pager).getByRole("button", { name: "Next page" }));
    await act(async () => { await vi.advanceTimersByTimeAsync(0); });
    expect(api.kubernetesCrashes).toHaveBeenCalledTimes(2);
    expect(vi.mocked(api.kubernetesCrashes).mock.calls[1][0]).toEqual({ ...request, cursor: "opaque-next" });
    await act(async () => { await vi.advanceTimersByTimeAsync(30000); });
    expect(api.kubernetesCrashes).toHaveBeenCalledTimes(2);
    expect(screen.getByText("2400 captured crashes in the last 24h")).toBeTruthy();
    fireEvent.click(within(pager).getByRole("button", { name: "Previous page" }));
    await act(async () => { await vi.advanceTimersByTimeAsync(0); });
    await act(async () => { await vi.advanceTimersByTimeAsync(30000); });
    expect(api.kubernetesCrashes).toHaveBeenCalledTimes(3);
    expect(vi.mocked(api.kubernetesCrashes).mock.calls[2][0]?.cursor).toBeUndefined();
  });
  it("hides the crash section when nothing crashed and pages only when there is another page", async () => {
    const page = { items: [], crash_count: 0, since: "2026-10-20T08:00:00Z", until: now, started_at: now, retention_from: now, retention_basis: "capture", gaps: [], capture_gaps: [], coverage: {}, limitations: [], capture: { mode: "continuous", behind: false }, sync: { state: "ready", age_s: 0, partial: false } };
    vi.mocked(api.kubernetesCrashes).mockResolvedValue(page as never);
    const quiet = mount(<ResourceMetrics resource={pod} liveSamples={[]} onCrash={vi.fn()} />);
    await waitFor(() => expect(screen.queryByText("Loading captured crashes")).toBeNull());
    expect(screen.queryByRole("navigation", { name: "Captured crash pages" })).toBeNull();
    expect(screen.queryByText(/captured crashes/)).toBeNull();
    quiet.unmount();
    vi.mocked(api.kubernetesCrashes).mockResolvedValue({ ...page, items: [crash], crash_count: 3 } as never);
    mount(<ResourceMetrics resource={pod} liveSamples={[]} onCrash={vi.fn()} />);
    expect(await screen.findByText("3 captured crashes in the last 24h")).toBeTruthy();
    // One page of crashes needs no pager.
    expect(screen.queryByRole("button", { name: "Next page" })).toBeNull();
  });

  it("keeps missing projected facts honest", () => {
    mount(<ResourceFacts resource={pod} onOpen={vi.fn()} />);
    expect(screen.getByText("Details")).toBeTruthy();
    expect(screen.getByText("Conditions")).toBeTruthy();
  });
  it("keeps zero or invalid projected dates unknown while retaining real ages and transition dates", () => {
    const zero = "0001-01-01T00:00:00Z";
    const unknown: KubernetesResource = {
      ...pod,
      created_at: zero,
      conditions: [
        { type: "Ready", status: "False", last_transition_at: zero },
      ],
      summary: {
        log_container_states: [
          {
            name: "app",
            started_at: zero,
            last_terminated_at: zero,
            last_terminated_reason: "OOMKilled",
            last_terminated_exit_code: 137,
          },
        ],
      },
    };
    const { unmount } = mount(
      <>
        <ResourceHeader resource={unknown} />
        <ResourceFacts resource={unknown} onOpen={vi.fn()} />
      </>,
    );
    expect(screen.getByText("Age").nextElementSibling?.textContent).toBe(
      "Unavailable",
    );
    expect(screen.getByText(/up Unavailable/)).toBeTruthy();
    expect(screen.getByText(/Last state/).textContent).toContain(
      "OOMKilled exit 137",
    );
    expect(screen.getByText(/Last state/).textContent).not.toContain(" at ");
    expect(document.querySelector("time")).toBeNull();
    expect(document.body.textContent).not.toMatch(
      /739\d+d|1\/1\/1|Invalid Date/,
    );
    unmount();
    const real = "2026-10-21T07:00:00Z";
    mount(
      <ResourceFacts
        resource={{
          ...unknown,
          created_at: real,
          conditions: [
            { type: "Ready", status: "True", last_transition_at: real },
          ],
          summary: {
            log_container_states: [
              {
                name: "app",
                last_terminated_reason: "Error",
                last_terminated_at: real,
                last_terminated_exit_code: 1,
              },
            ],
          },
        }}
        onOpen={vi.fn()}
      />,
    );
    expect(screen.getByText("Age").nextElementSibling?.textContent).toBe(
      "1h 0m",
    );
    expect(document.querySelector("time")?.title).toBe(
      new Date(real).toLocaleString(),
    );
    expect(screen.getByText(/Last state/).textContent).toContain(
      `at ${new Date(real).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false })}`,
    );
  });
  it("uses muted unknown values and neutral normal values instead of healthy green", async () => {
    vi.mocked(api.kubernetesUsageHistory).mockResolvedValue({
      observed_at: now,
      source: "capture",
      availability: "unavailable",
      samples: [],
      retention_seconds: 21600,
    });
    const { unmount } = mount(
      <ResourceMetrics resource={pod} liveSamples={[]} onCrash={vi.fn()} />,
    );
    expect(
      screen.getByLabelText("CPU metric tile").querySelector("p")?.className,
    ).toContain("text-ink-400");
    expect(
      screen.getByLabelText("Memory metric tile").querySelector("p")?.className,
    ).toContain("text-ink-400");
    unmount();
    mount(
      <ResourceMetrics
        resource={pod}
        workload={{
          resource_id: pod.resource_id,
          kind: "Pod",
          name: pod.name,
          containers: [{ name: "app", limits: { cpu: "1", memory: "256Mi" } }],
          truncated: false,
        }}
        liveSamples={[
          {
            observedAt: Date.parse(now) - 1000,
            cpu: 0.2,
            memory: 64 * 1024 ** 2,
          },
        ]}
        onCrash={vi.fn()}
      />,
    );
    expect(screen.getByLabelText("200 mCPU").className).toContain(
      "text-ink-50",
    );
    expect(screen.getByText(/20%/, { selector: "span" }).className).toContain(
      "text-link",
    );
  });
  it("classifies owned and selected resources by direction relative to the current resource", async () => {
    const deployment = {
      ...pod,
      kind: "Deployment",
      name: "checkout",
      uid: "deployment",
      resource_id: "apps~v1~deployments",
    };
    vi.mocked(api.kubernetesRelated).mockResolvedValue({
      nodes: [
        {
          id: "deployment",
          kind: "Deployment",
          namespace: "shop",
          name: "checkout",
          group: "workload",
        },
        {
          id: "replicaset",
          kind: "ReplicaSet",
          namespace: "shop",
          name: "checkout-rs",
          group: "workload",
        },
        {
          id: "pod",
          kind: "Pod",
          namespace: "shop",
          name: "checkout-pod",
          group: "workload",
        },
        {
          id: "service",
          kind: "Service",
          namespace: "shop",
          name: "checkout-svc",
          group: "network",
        },
        {
          id: "scaler",
          kind: "HorizontalPodAutoscaler",
          namespace: "shop",
          name: "checkout-hpa",
          group: "workload",
        },
      ],
      edges: [
        { from: "deployment", to: "replicaset", type: "manages" },
        { from: "replicaset", to: "pod", type: "manages" },
        { from: "service", to: "pod", type: "exposes" },
        { from: "scaler", to: "deployment", type: "scales" },
      ],
      omitted: {},
      sync: { state: "ready", age_s: 0, partial: false },
    });
    const { unmount } = mount(
      <ResourceRelations resource={deployment} onOpen={vi.fn()} />,
    );
    const owns = (
      await screen.findByRole("heading", { name: "Owns", exact: true })
    ).closest("section")!;
    expect(within(owns).getByText("checkout-pod")).toBeTruthy();
    expect(within(owns).getByText("checkout-rs")).toBeTruthy();
    const selected = screen
      .getByRole("heading", { name: "Selected by" })
      .closest("section")!;
    expect(within(selected).queryByText("checkout-hpa")).toBeNull();
    expect(within(screen.getByRole("heading", { name: "Owned by" }).closest("section")!).getByText("checkout-hpa")).toBeTruthy();
    expect(within(selected).queryByText("checkout-pod")).toBeNull();
    expect(within(selected).queryByText("checkout-svc")).toBeNull();
    unmount();
    mount(
      <ResourceRelations
        resource={{
          ...deployment,
          kind: "Service",
          name: "checkout-svc",
          resource_id: "core~v1~services",
        }}
        onOpen={vi.fn()}
      />,
    );
    const selects = (
      await screen.findByRole("heading", { name: "Selects", exact: true })
    ).closest("section")!;
    expect(within(selects).getByText("checkout-pod")).toBeTruthy();
    expect(
      within(
        screen
          .getByRole("heading", { name: "Selected by" })
          .closest("section")!,
      ).queryByText("checkout-pod"),
    ).toBeNull();
  });
  it("uses occurrence totals consistently for chips, reason badges and last-observed histogram buckets", () => {
    const at = "2026-10-21T07:59:00Z";
    mount(
      <ResourceEvents
        events={[
          {
            ...pod,
            kind: "Event",
            name: "warning",
            uid: "warning",
            summary: {
              type: "Warning",
              reason: "BackOff",
              count: 9,
              lastTimestamp: at,
            },
          },
          {
            ...pod,
            kind: "Event",
            name: "normal",
            uid: "normal",
            summary: {
              type: "Normal",
              reason: "Started",
              count: 4,
              lastTimestamp: at,
            },
          },
        ]}
      />,
    );
    expect(screen.getByRole("button", { name: "All 13" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Warnings 9" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Normal 4" })).toBeTruthy();
    expect(
      screen
        .getByRole("heading", { name: "BackOff" })
        .parentElement?.querySelector(".pill")?.textContent,
    ).toBe("×9");
    expect(screen.getByTitle("9 warnings, 4 normal")).toBeTruthy();
    expect(screen.getByText(/Counts are occurrences/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Warnings 9" }));
    expect(screen.getByTitle("9 warnings, 0 normal")).toBeTruthy();
  });
});

describe("Resource detail sync gates", () => {
  const published = "2026-10-26T08:00:00Z";
  const podNode = { id: "pod", kind: "Pod", namespace: "shop", name: pod.name, group: "workload", health: "healthy" };

  // A background resync is not a reason to take the graph away from an operator
  // who is on it: the last generation is still the best answer they have.
  it("keeps the relationship map through a background resync", async () => {
    vi.mocked(api.kubernetesRelated).mockResolvedValue({ nodes: [podNode], edges: [], omitted: {}, sync: { state: "syncing", age_s: 4, partial: false } });
    mount(
      <KubernetesFirstReadyContext.Provider value={published}>
        <ResourceRelations resource={pod} related={[{ kind: "ConfigMap", name: "checkout-config" }]} onOpen={vi.fn()} />
      </KubernetesFirstReadyContext.Provider>,
    );
    const section = await screen.findByRole("region", { name: "Related resources" });
    await waitFor(() => expect(section.querySelector('[data-relationship-node="pod"]')).toBeTruthy());
    expect(within(section).queryByLabelText("Loading related resources")).toBeNull();
    expect(within(section).queryByRole("region", { name: "Preparing your cluster" })).toBeNull();
  });

  // Before this cluster has published anything there is nothing to keep, so the
  // first sync says what it is doing instead of showing an empty map.
  it("shows Preparing in Related while the cluster has never published", async () => {
    vi.mocked(api.kubernetesRelated).mockResolvedValue({ nodes: [podNode], edges: [], omitted: {}, sync: { state: "syncing", age_s: 0, partial: false } });
    const { container } = mount(<ResourceRelations resource={pod} related={[]} onOpen={vi.fn()} />);
    expect(await screen.findByRole("region", { name: "Preparing your cluster" })).toBeTruthy();
    expect(container.querySelector("[data-relationship-node]")).toBeNull();
    expect(screen.queryByLabelText("Loading related resources")).toBeNull();
  });

  it("counts events and crashes in words rather than with a dot", async () => {
    mount(<ResourceTimeline resource={pod} onCrash={vi.fn()} />);
    const summary = await screen.findByText(/^\d+ events and \d+ crashes$/);
    expect(summary.textContent).toBe("2 events and 1 crashes");
    expect(summary.textContent).not.toMatch(/[·•]/);
  });
});
