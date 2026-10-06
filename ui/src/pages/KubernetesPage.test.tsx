// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, useLocation } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api, ApiError } from "@/lib/api";
import { KubernetesPage } from "./KubernetesPage";

vi.mock("@/components/TopBar", () => ({ TopBar: ({ title, actions }: { title: string; actions?: React.ReactNode }) => <header><span>{title}</span>{actions}</header> }));
vi.mock("@/lib/api", async (importActual) => { const actual = await importActual<typeof import("@/lib/api")>(); return { ...actual, api: { ...actual.api, listAgentToolsets: vi.fn(), proposeAgentAction: vi.fn(), approveAgentApproval: vi.fn(), rejectAgentApproval: vi.fn(), kubernetesOverview: vi.fn(), kubernetesOverviewGraph: vi.fn(), kubernetesIssues: vi.fn(), kubernetesChanges: vi.fn(), kubernetesGraph: vi.fn(), kubernetesNamespaces: vi.fn(), kubernetesNeighborhood: vi.fn(), kubernetesStream: vi.fn(), kubernetesTop: vi.fn(), kubernetesReleases: vi.fn(), kubernetesGitOpsApps: vi.fn(), kubernetesRollout: vi.fn(), kubernetesRollouts: vi.fn(), kubernetesTraffic: vi.fn(), kubernetesDiagnose: vi.fn(), kubernetesWorkloadLogs: vi.fn(), kubernetesPodLogs: vi.fn(), kubernetesUsage: vi.fn(), kubernetesWorkloads: vi.fn(), kubernetesWorkload: vi.fn(), kubernetesNodes: vi.fn(), kubernetesNodePods: vi.fn(), kubernetesSearch: vi.fn(), kubernetesEvents: vi.fn(), kubernetesDescribe: vi.fn() } }; });

function LocationProbe() { const location = useLocation(); return <output aria-label="Current location">{location.pathname}{location.search}</output>; }
function renderPage(client = new QueryClient({ defaultOptions: { queries: { retry: false } } })) { return render(<QueryClientProvider client={client}><MemoryRouter initialEntries={["/agent/kubernetes"]}><KubernetesPage /><LocationProbe /></MemoryRouter></QueryClientProvider>); }

function capacityCell(panelName: string, label: string) {
	const panel = screen.getByRole("heading", { name: panelName }).closest("section")!;
	return within(panel).getByText(label).closest("div")!;
}

beforeEach(() => {
	vi.mocked(api.kubernetesOverview).mockResolvedValue({ connector: "kubernetes", cluster_id: "production", observed_at: "2026-08-30T12:00:00Z", nodes: 3, ready_nodes: 2, pods: 18, running_pods: 16, namespaces: 5, active_namespaces: 5, workloads: 7, warnings: 2, usage_source: "unavailable", metrics_status: "unavailable", metrics_fresh: false, truncated: false, partial_failures: [{ resource_id: "core~v1~nodes", class: "forbidden" }] });
	vi.mocked(api.listAgentToolsets).mockResolvedValue([]);
	vi.mocked(api.kubernetesIssues).mockResolvedValue({ items: [], totals: {}, truncated: false, sync: { state: "direct", age_s: 0, partial: false } });
	vi.mocked(api.kubernetesChanges).mockResolvedValue({ items: [], gaps: [], sync: { state: "ready", age_s: 0, partial: false } });
	vi.mocked(api.kubernetesGraph).mockResolvedValue({ nodes: [], edges: [], omitted: {}, sync: { state: "ready", age_s: 0, partial: false } });
	vi.mocked(api.kubernetesOverviewGraph).mockResolvedValue({ nodes: [], edges: [], omitted: {}, sync: { state: "ready", age_s: 0, partial: false } });
	vi.mocked(api.kubernetesNamespaces).mockResolvedValue({ items: [{ resource_id: "core~v1~namespaces", kind: "Namespace", name: "shop" }], truncated: false });
	vi.mocked(api.kubernetesNeighborhood).mockResolvedValue({ nodes: [], edges: [], omitted: {}, sync: { state: "ready", age_s: 0, partial: false } });
	vi.mocked(api.kubernetesStream).mockImplementation((_onEvent, signal) => new Promise<void>((resolve) => {
		if (signal?.aborted) resolve();
		else signal?.addEventListener("abort", () => resolve(), { once: true });
	}));
	vi.mocked(api.kubernetesTop).mockResolvedValue({ items: [], total: 0, truncated: false, availability: "unavailable", fresh: false, sync: { state: "direct", age_s: 0, partial: false } });
	vi.mocked(api.kubernetesReleases).mockResolvedValue({ items: [], truncated: false });
	vi.mocked(api.kubernetesGitOpsApps).mockResolvedValue({ items: [], available: false, reason: "GitOps APIs are not discovered", truncated: false });
	vi.mocked(api.kubernetesRollout).mockResolvedValue({ namespace: "payments", name: "checkout", phase: "Progressing", strategy: "canary", step: 2, total_steps: 5, stable_rs: "checkout-stable", canary_rs: "checkout-canary", weight: 20, message: "Waiting for analysis" });
	vi.mocked(api.kubernetesRollouts).mockResolvedValue({ items: [], available: false, reason: "Argo Rollouts API is not discovered", truncated: false });
	vi.mocked(api.kubernetesTraffic).mockResolvedValue({ available: false, reason: "No flow source connected", window: "15m", unmapped: 0, truncated: false });
	vi.mocked(api.kubernetesDiagnose).mockResolvedValue({ workload: { resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "api", truncated: false }, warning_events: [], changes: [], neighborhood: { nodes: [], edges: [], omitted: {}, sync: { state: "ready", age_s: 0, partial: false } }, truncated: false, sync: { state: "direct", age_s: 0, partial: false } });
	vi.mocked(api.kubernetesWorkloadLogs).mockResolvedValue({ pods: [], lines: [], truncated: false });
	vi.mocked(api.kubernetesPodLogs).mockResolvedValue({ cluster_id: "production", namespace: "default", pod: "api", container: "api", previous: false, since_seconds: 0, tail_lines: 200, text: "", truncated: false });
	vi.mocked(api.kubernetesUsage).mockResolvedValue({ observed_at: "2026-08-30T12:00:00Z", availability: "unavailable", fresh: false, truncated: false });
	vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [], counts: {}, truncated: false });
	vi.mocked(api.kubernetesWorkload).mockResolvedValue({ resource_id: "core~v1~pods", kind: "Pod", namespace: "default", name: "api", truncated: false });
	vi.mocked(api.kubernetesNodes).mockResolvedValue({ items: [{ resource_id: "core~v1~nodes", kind: "Node", name: "node-a", conditions: [{ type: "Ready", status: "True" }], summary: { allocatable_cpu: "4", allocatable_memory: "8Gi" } }], truncated: false });
	vi.mocked(api.kubernetesNodePods).mockResolvedValue({ items: [{ resource_id: "core~v1~pods", kind: "Pod", namespace: "default", name: "api", summary: { phase: "Running", restart_count: 1, node: "node-a" } }], truncated: false });
	vi.mocked(api.kubernetesSearch).mockResolvedValue({ items: [], truncated: false });
	vi.mocked(api.kubernetesEvents).mockResolvedValue({ items: [], truncated: false });
	vi.mocked(api.kubernetesDescribe).mockResolvedValue({ resource: { resource_id: "core~v1~pods", kind: "Pod", namespace: "default", name: "api" } });
});
afterEach(() => { cleanup(); vi.clearAllMocks(); vi.useRealTimers(); window.history.replaceState(null, "", "/"); });

describe("KubernetesPage", () => {
	it("loads real overview topology on first visit and isolates it from namespace and kind filters", async () => {
		const nodes = [
			{ id: "overview-service", kind: "Service", namespace: "platform", name: "overview-service", group: "platform" },
			{ id: "overview-pod", kind: "Pod", namespace: "shop", name: "overview-pod", group: "shop" },
		];
		vi.mocked(api.kubernetesOverviewGraph).mockResolvedValue({ nodes, edges: [{ from: nodes[0].id, to: nodes[1].id, type: "exposes" }], omitted: {}, sync: { state: "ready", age_s: 0, partial: false } });
		renderPage();
		const preview = await screen.findByLabelText("Scrollable overview topology graph");
		expect(api.kubernetesOverviewGraph).toHaveBeenCalledTimes(1);
		expect(api.kubernetesOverviewGraph).toHaveBeenCalledWith();
		expect(api.kubernetesGraph).not.toHaveBeenCalled();
		expect(preview.querySelector('[data-edge-from="overview-service"]')).toBeTruthy();
		fireEvent.click(within(preview).getByRole("button", { name: "Open Pod shop/overview-pod details" }));
		expect(await screen.findByRole("dialog", { name: "Details panel" })).toBeTruthy();
		fireEvent.click(screen.getByRole("button", { name: "Close panel" }));
		fireEvent.click(screen.getByRole("button", { name: "View all relationships", exact: true }));
		fireEvent.click(await screen.findByRole("button", { name: "Open namespace shop topology" }));
		fireEvent.click(await screen.findByRole("button", { name: "Hide Pod", exact: true }));
		fireEvent.click(screen.getByRole("tab", { name: "Overview", exact: true }));
		const restored = await screen.findByLabelText("Scrollable overview topology graph");
		expect(within(restored).getAllByRole("button")).toHaveLength(2);
		expect(api.kubernetesOverviewGraph).toHaveBeenCalledTimes(1);
	});

	it("keeps overview topology cached across stream invalidations until manual refresh", async () => {
		const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
		const invalidate = vi.spyOn(client, "invalidateQueries");
		renderPage(client);
		const queries = [api.kubernetesOverview, api.kubernetesWorkloads, api.kubernetesNodes, api.kubernetesIssues, api.kubernetesTop, api.kubernetesChanges];
		await waitFor(() => {
			expect(api.kubernetesOverviewGraph).toHaveBeenCalledTimes(1);
			expect(client.isFetching()).toBe(0);
			queries.forEach((query) => expect(query).toHaveBeenCalledTimes(1));
		});
		const onEvent = vi.mocked(api.kubernetesStream).mock.calls[0][0];
		vi.useFakeTimers();
		for (const [index, event] of ["records", "resync"].entries()) {
			invalidate.mockClear();
			act(() => onEvent({ event, data: "{}" }));
			await act(async () => { await vi.advanceTimersByTimeAsync(749); });
			expect(invalidate).not.toHaveBeenCalled();
			await act(async () => { await vi.advanceTimersByTimeAsync(1); });
			expect(invalidate.mock.calls.map(([options]) => options?.queryKey)).toEqual([
				["kubernetes-overview"], ["kubernetes-graph"], ["kubernetes-namespaces"], ["kubernetes-workloads"], ["kubernetes-nodes"], ["kubernetes-issues"], ["kubernetes-top"], ["kubernetes-diagnosis"], ["kubernetes-drawer-changes"], ["kubernetes-changes"],
			]);
			queries.forEach((query) => expect(query).toHaveBeenCalledTimes(index + 2));
			expect(api.kubernetesOverviewGraph).toHaveBeenCalledTimes(1);
		}
		vi.useRealTimers();
		fireEvent.click(screen.getByRole("button", { name: "Refresh Kubernetes data" }));
		await waitFor(() => expect(api.kubernetesOverviewGraph).toHaveBeenCalledTimes(2));
	});

	it.each(["partial", "paged", "omitted", "error", "dangling", "malformed"])("fails closed for %s overview topology", async (failure) => {
		const node = { id: "pod", kind: "Pod", namespace: "shop", name: "incomplete", group: "shop" };
		if (failure === "error") vi.mocked(api.kubernetesOverviewGraph).mockRejectedValue(new ApiError(403, "forbidden"));
		else vi.mocked(api.kubernetesOverviewGraph).mockResolvedValue({ nodes: failure === "malformed" ? null as never : [node], edges: failure === "dangling" ? [{ from: "pod", to: "missing", type: "uses" }] : [], next: failure === "paged" ? "next-page" : undefined, omitted: failure === "omitted" ? { Pod: 1 } : {}, sync: { state: "ready", age_s: 0, partial: failure === "partial" } });
		renderPage();
		await screen.findByRole("alert");
		expect(screen.queryByLabelText("Scrollable overview topology graph")).toBeNull();
		expect(screen.queryByRole("button", { name: "Open Pod shop/incomplete details" })).toBeNull();
	});

	it("keeps workloads full width and the sampled timestamp naturally left aligned", async () => {
		vi.mocked(api.kubernetesUsage).mockResolvedValue({ observed_at: "2026-08-30T12:01:00Z", availability: "available", fresh: true, node_metrics: { availability: "available", fresh: true, complete: true, total: 3, cpu: "2", observed_at: "2026-08-30T12:01:00Z" }, truncated: false });
		renderPage();
		const sampled = await screen.findByText(/^Sampled /);
		expect(sampled.classList.contains("ml-auto")).toBe(false);
		expect(sampled.parentElement?.classList.contains("flex-wrap")).toBe(true);
		expect(screen.getByRole("region", { name: "Workloads" }).parentElement?.className).toBe("min-w-0 w-full");
	});

	it("hides cached overview topology when a refresh loses access", async () => {
		vi.mocked(api.kubernetesOverviewGraph)
			.mockResolvedValueOnce({ nodes: [{ id: "cached", kind: "Pod", namespace: "shop", name: "cached-overview", group: "shop" }], edges: [], omitted: {}, sync: { state: "ready", age_s: 0, partial: false } })
			.mockRejectedValue(new ApiError(403, "forbidden"));
		renderPage();
		await screen.findByRole("button", { name: "Open Pod shop/cached-overview details" });
		fireEvent.click(screen.getByRole("button", { name: "Refresh Kubernetes data" }));
		await screen.findByRole("alert");
		expect(screen.queryByLabelText("Scrollable overview topology graph")).toBeNull();
		expect(screen.queryByRole("button", { name: "Open Pod shop/cached-overview details" })).toBeNull();
	});

	it("retains the entire large overview graph while culling only its viewport", async () => {
		const nodes = Array.from({ length: 1400 }, (_, index) => ({ id: `overview-${index}`, kind: "Pod", namespace: index % 2 ? "shop" : "platform", name: `pod-${index}`, group: "all" }));
		const edges = nodes.slice(1).map((node, index) => ({ from: nodes[index].id, to: node.id, type: "manages" as const }));
		vi.mocked(api.kubernetesOverviewGraph).mockResolvedValue({ nodes, edges, omitted: {}, sync: { state: "ready", age_s: 0, partial: false } });
		renderPage();
		const viewport = await screen.findByLabelText("Scrollable overview topology graph");
		expect(screen.getByText(/1400 \/ 1400 connected resources visible/).textContent).toContain("1399 / 1399 relationships");
		expect(within(viewport).getAllByRole("button").length).toBeLessThan(100);
		expect(viewport.querySelectorAll("svg g[data-edge-from]").length).toBeLessThan(100);
		expect(viewport.querySelectorAll("svg g rect")).toHaveLength(0);
		expect(api.kubernetesGraph).not.toHaveBeenCalled();
	});

	it("uses grouped eye filters, four default kinds, and resets them for each namespace", async () => {
		vi.mocked(api.kubernetesNamespaces).mockResolvedValue({ items: ["shop", "platform"].map((name) => ({ resource_id: "core~v1~namespaces", kind: "Namespace", name })), truncated: false });
		vi.mocked(api.kubernetesGraph).mockImplementation(async (options) => ({ nodes: ["Ingress", "Service", "Deployment", "Pod", "ConfigMap", "ReplicaSet"].map((kind) => ({ id: kind, kind, namespace: options?.namespace, name: kind.toLowerCase(), group: options?.namespace ?? "" })), edges: [{ from: "Deployment", to: "Pod", type: "manages" }, { from: "ConfigMap", to: "Pod", type: "configures" }], omitted: {}, sync: { state: "ready", age_s: 0, partial: false } }));
		renderPage();
		fireEvent.click(screen.getByRole("tab", { name: "Topology" }));
		const blocks = await screen.findByRole("group", { name: "Namespace blocks" });
		expect(within(blocks).getAllByRole("button")).toHaveLength(2);
		fireEvent.click(within(blocks).getByRole("button", { name: "Open namespace shop topology" }));
		await screen.findByLabelText("Scrollable topology graph");
		const filters = screen.getByRole("complementary", { name: "Topology filters" });
		for (const group of ["Networking", "Workloads", "Configuration"]) expect(within(filters).getByText(group)).toBeTruthy();
		for (const kind of ["Ingress", "Service", "Deployment", "Pod"]) expect(within(filters).getByRole("button", { name: `Hide ${kind}`, exact: true }).getAttribute("aria-pressed")).toBe("true");
		const hidden = within(filters).getByRole("button", { name: "Show ConfigMap", exact: true });
		expect(hidden.getAttribute("aria-pressed")).toBe("false");
		expect(hidden.getAttribute("title")).toBe("Show ConfigMap");
		expect(within(filters).queryByRole("checkbox")).toBeNull();
		expect(within(filters).getByRole("button", { name: "Refresh topology" })).toBeTruthy();
		expect(screen.getByText(/connected resources visible/).textContent).toContain("4 / 6 connected resources visible");
		expect(screen.getByText(/connected resources visible/).textContent).toContain("1 / 2 relationships");
		fireEvent.click(hidden);
		expect(within(filters).getByRole("button", { name: "Hide ConfigMap" }).getAttribute("aria-pressed")).toBe("true");
		fireEvent.click(screen.getByRole("button", { name: "Namespaces", exact: true }));
		fireEvent.click(screen.getByRole("button", { name: "Open namespace platform topology" }));
		await screen.findByLabelText("Scrollable topology graph");
		expect(screen.getByRole("button", { name: "Show ConfigMap", exact: true }).getAttribute("aria-pressed")).toBe("false");
	});
	it("refreshes all overview summaries with the cluster snapshot", async () => {
		renderPage();
		await screen.findByRole("img", { name: /changes in the last 60 minutes/ });
		const queries = [api.kubernetesIssues, api.kubernetesTop, api.kubernetesOverviewGraph, api.kubernetesReleases, api.kubernetesTraffic, api.kubernetesChanges];
		const initialCalls = queries.map((query) => vi.mocked(query).mock.calls.length);
		fireEvent.click(screen.getByRole("button", { name: "Refresh Kubernetes data" }));
		await waitFor(() => queries.forEach((query, index) => expect(vi.mocked(query).mock.calls.length).toBeGreaterThan(initialCalls[index])));
	});

	it.each([0, 3])("does not infer cluster health from %s ready nodes and no warnings", async (nodes) => {
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ connector: "kubernetes", cluster_id: "production", observed_at: "2026-08-30T12:00:00Z", nodes, ready_nodes: nodes, pods: 4, running_pods: 2, namespaces: 2, active_namespaces: 2, workloads: 2, warnings: 0, metrics_fresh: false, truncated: false });
		renderPage();
		const health = await screen.findByRole("region", { name: "Cluster health" });
		expect(within(health).queryByText("Healthy")).toBeNull();
		expect(within(health).getByText(nodes > 0 ? "Node checks passing" : "No node evidence")).toBeTruthy();
	});

	it("does not present partial ranking metadata as an overview warning", async () => {
		vi.mocked(api.kubernetesTop).mockResolvedValue({ items: [], items_available: true, total: 0, truncated: false, availability: "available", fresh: true, sync: { state: "ready", age_s: 0, partial: true } });
		renderPage();
		await screen.findByText("Top pod usage");
		expect(screen.queryByText("Usage ranking is partial.")).toBeNull();
	});
	it("shows accurate capacity meters and keeps unavailable metrics distinct from zero", async () => {
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ connector: "kubernetes", cluster_id: "production", observed_at: "2026-08-30T12:00:00Z", nodes: 3, ready_nodes: 3, pods: 4, running_pods: 4, namespaces: 2, active_namespaces: 2, workloads: 2, warnings: 0, requested_cpu: "6", allocatable_cpu: "4", usage_cpu: "0", requested_memory: "2Gi", allocatable_memory: "8Gi", metrics_fresh: false, truncated: true });
		vi.mocked(api.kubernetesUsage).mockResolvedValue({ observed_at: "2026-08-30T12:01:00Z", availability: "available", fresh: true, node_metrics: { availability: "available", fresh: true, complete: true, total: 3, cpu: "0", observed_at: "2026-08-30T12:01:00Z" }, pod_metrics: { availability: "unavailable", fresh: false, complete: false, total: 0 }, pods: [], nodes: [], truncated: false });
		renderPage();
		const cpu = await screen.findByRole("meter", { name: "CPU in use" });
		expect(cpu.getAttribute("aria-valuenow")).toBe("0");
		const reserved = screen.getByRole("meter", { name: "CPU reserved by requests" });
		expect(reserved.getAttribute("aria-valuenow")).toBe("100");
		expect(reserved.getAttribute("aria-valuetext")).toBe("150% of allocatable");
		expect(screen.getByRole("meter", { name: "Memory reserved by requests" }).getAttribute("aria-valuenow")).toBe("25");
		expect(screen.queryByRole("meter", { name: "Memory in use" })).toBeNull();
		expect(within(screen.getByRole("region", { name: "Cluster health" })).getByText("Partial visibility")).toBeTruthy();
	});

	it("uses fresh usage aggregates when overview inventory metrics are unavailable", async () => {
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ connector: "kubernetes", cluster_id: "production", observed_at: "2026-08-30T12:00:00Z", nodes: 0, ready_nodes: 0, pods: 0, running_pods: 0, namespaces: 0, active_namespaces: 0, workloads: 0, warnings: 0, usage_source: "unavailable", metrics_status: "unavailable", metrics_fresh: false, truncated: true, partial_failures: [{ resource_id: "core~v1~pods", class: "forbidden" }] });
		vi.mocked(api.kubernetesUsage).mockResolvedValue({ observed_at: "2026-08-30T12:01:00Z", availability: "available", fresh: true, node_metrics: { availability: "available", fresh: true, complete: true, total: 3, cpu: "2", memory: "4Gi", observed_at: "2026-08-30T12:01:00Z" }, pod_metrics: { availability: "unavailable", fresh: false, complete: false, total: 0 }, pods: [], nodes: [], truncated: false });
		renderPage();

		expect(await screen.findByText("Metrics available")).toBeTruthy();
		expect(screen.getByText("source node metrics")).toBeTruthy();
		expect(screen.getByText("Aggregate 2 cores CPU, 4 GiB memory")).toBeTruthy();
		expect(screen.getByText(/Usage fresh/)).toBeTruthy();
	});

	it("prefers one fresh aggregate usage snapshot over stale overview metrics", async () => {
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ connector: "kubernetes", cluster_id: "production", observed_at: "2026-08-30T11:00:00Z", nodes: 3, ready_nodes: 3, pods: 20, running_pods: 20, namespaces: 2, active_namespaces: 2, workloads: 4, warnings: 0, usage_cpu: "99", usage_memory: "999Gi", usage_source: "pod_metrics", metrics_status: "stale", metrics_observed_at: "2026-08-30T11:00:00Z", metrics_fresh: false, truncated: false });
		vi.mocked(api.kubernetesUsage).mockResolvedValue({ observed_at: "2026-08-30T12:00:00Z", availability: "available", fresh: true, node_metrics: { availability: "stale", fresh: false, complete: true, total: 5, cpu: "500m", memory: "512Mi", observed_at: "2026-08-30T11:59:00Z" }, pod_metrics: { availability: "available", fresh: true, complete: true, total: 13, cpu: "750m", memory: "2Gi", observed_at: "2026-08-30T12:00:00Z" }, pods: [{ kind: "Pod", namespace: "shop", name: "sample-only", cpu: "1", memory: "4Gi" }], nodes: [], truncated: true });
		renderPage();
		expect(await screen.findByText("Metrics available")).toBeTruthy();
		expect(screen.getByText("source pod metrics")).toBeTruthy();
		expect(screen.getByText("Aggregate 750 mCPU CPU, 2 GiB memory")).toBeTruthy();
		expect(screen.getByText("Usage fresh; 13 pod metrics; 5 node metrics; sample list partial")).toBeTruthy();
		expect(within(capacityCell("CPU capacity", "Usage")).getByText("750 mCPU")).toBeTruthy();
		expect(within(capacityCell("Memory capacity", "Usage")).getByText("2 GiB")).toBeTruthy();
		expect(screen.queryByText("99 cores")).toBeNull();
		expect(screen.queryByText("999 GiB")).toBeNull();
	});

	it("keeps a fresh complete node aggregate fresh when pod metrics are stale and partial", async () => {
		vi.mocked(api.kubernetesTop).mockResolvedValue({ items: [], items_available: true, total: 0, truncated: false, availability: "stale", fresh: false, sync: { state: "ready", age_s: 0, partial: true } });
		vi.mocked(api.kubernetesUsage).mockResolvedValue({ observed_at: "2026-08-30T12:01:00Z", availability: "available", fresh: false, node_metrics: { availability: "available", fresh: true, complete: true, total: 3, cpu: "2", memory: "4Gi", observed_at: "2026-08-30T12:01:00Z" }, pod_metrics: { availability: "stale", fresh: false, complete: false, total: 13 }, pods: [], nodes: [], truncated: false });
		renderPage();
		expect(await screen.findByText("Metrics available")).toBeTruthy();
		expect(screen.getByText("source node metrics")).toBeTruthy();
		expect(screen.getByText("Aggregate 2 cores CPU, 4 GiB memory")).toBeTruthy();
		expect(screen.getByText("Usage fresh; Unavailable pod metrics; 3 node metrics")).toBeTruthy();
		expect(within(capacityCell("CPU capacity", "Usage")).getByText("2 cores")).toBeTruthy();
		expect(within(capacityCell("Memory capacity", "Usage")).getByText("4 GiB")).toBeTruthy();
	});

	it("rejects fresh but incomplete metrics aggregates", async () => {
		vi.mocked(api.kubernetesUsage).mockResolvedValue({ observed_at: "2026-08-30T12:01:00Z", availability: "available", fresh: true, node_metrics: { availability: "available", fresh: true, complete: false, total: 8, cpu: "500m", memory: "512Mi" }, pod_metrics: { availability: "available", fresh: true, complete: false, total: 97, cpu: "2", memory: "4Gi" }, truncated: true });
		renderPage();
		expect(await screen.findByText("Aggregate Unavailable CPU, Unavailable memory")).toBeTruthy();
		expect(screen.getByText(/metrics source incomplete; aggregate unavailable/)).toBeTruthy();
		expect(screen.getByText(/Unavailable pod metrics; Unavailable node metrics/)).toBeTruthy();
		expect(screen.queryByText("500 mCPU")).toBeNull();
		expect(screen.queryByText("2 cores")).toBeNull();
	});

	it("falls back as one snapshot to trustworthy overview metrics when usage is incomplete", async () => {
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ connector: "kubernetes", cluster_id: "production", observed_at: "2026-08-30T12:00:00Z", nodes: 3, ready_nodes: 3, pods: 4, running_pods: 4, namespaces: 2, active_namespaces: 2, workloads: 2, warnings: 0, usage_cpu: "600m", usage_memory: "1Gi", usage_source: "node_metrics", metrics_status: "stale", metrics_observed_at: "2026-08-30T11:59:00Z", metrics_fresh: false, truncated: false });
		vi.mocked(api.kubernetesUsage).mockResolvedValue({ observed_at: "2026-08-30T12:01:00Z", availability: "available", fresh: true, node_metrics: { availability: "available", fresh: true, complete: false, total: 3, cpu: "99", memory: "99Gi" }, truncated: false });
		renderPage();
		expect(await screen.findByText("Metrics stale")).toBeTruthy();
		expect(screen.getByText("Aggregate 600 mCPU CPU, 1 GiB memory")).toBeTruthy();
		expect(screen.getByText(/latest metrics source incomplete; using overview snapshot/)).toBeTruthy();
		expect(screen.queryByText("99 cores")).toBeNull();
	});

	it("uses one trustworthy overview snapshot when the usage read fails", async () => {
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ connector: "kubernetes", cluster_id: "production", observed_at: "2026-08-30T12:00:00Z", nodes: 3, ready_nodes: 3, pods: 4, running_pods: 4, namespaces: 2, active_namespaces: 2, workloads: 2, warnings: 0, usage_cpu: "700m", usage_memory: "1Gi", usage_source: "node_metrics", metrics_status: "available", metrics_observed_at: "2026-08-30T12:00:00Z", metrics_fresh: true, truncated: false });
		vi.mocked(api.kubernetesUsage).mockRejectedValueOnce(new Error("usage endpoint unavailable"));
		renderPage();
		expect(await screen.findByText("Metrics available")).toBeTruthy();
		expect(screen.getByText("Aggregate 700 mCPU CPU, 1 GiB memory")).toBeTruthy();
		expect(screen.getByText("Usage snapshot unavailable; using overview snapshot")).toBeTruthy();
	});

	it("opens overview card destinations without bounded-activity warnings", async () => {
		vi.mocked(api.kubernetesChanges).mockResolvedValue({ items: [], next: "next-page", sync: { state: "ready", age_s: 0, partial: true } });
		renderPage();
		await screen.findByRole("img", { name: /changes in the last 60 minutes/ });
		expect(screen.queryByText("Change activity reflects bounded history; view the timeline for more.")).toBeNull();
		expect(screen.queryByText("The issue summary is bounded; view all issues for more results.")).toBeNull();
		expect(screen.queryByText("Relationship summary is bounded or partially synchronized; view all relationships for details.")).toBeNull();
		fireEvent.click(screen.getByRole("button", { name: "View all relationships", exact: true }));
		expect(screen.getByRole("tab", { name: "Topology" }).getAttribute("aria-selected")).toBe("true");
		fireEvent.click(screen.getByRole("tab", { name: "Overview" }));
		fireEvent.click(await screen.findByRole("button", { name: "View all releases", exact: true }));
		expect(screen.getByRole("tab", { name: "Helm" }).getAttribute("aria-selected")).toBe("true");
	});
	it("automatically lists Rollouts and shows the backend unavailable reason", async () => {
		renderPage();
		fireEvent.click(screen.getByRole("tab", { name: "GitOps" }));
		expect(await screen.findByText("Argo Rollouts API is not discovered")).toBeTruthy();
		expect(api.kubernetesRollouts).toHaveBeenCalledWith({ limit: 20, cursor: undefined });
		expect(screen.queryByRole("textbox", { name: "Rollout namespace" })).toBeNull();
		expect(screen.queryByRole("textbox", { name: "Rollout name" })).toBeNull();
	});

	it("pages Issues with the returned cursor and does not label next-page evidence partial", async () => {
		const issue = (name: string) => ({ root: { kind: "Deployment", namespace: "payments", name, api_version: "apps/v1" }, rule: "workload.unavailable", severity: "warning" as const, count: 1 });
		vi.mocked(api.kubernetesIssues).mockImplementation(async (options = {}) => options.cursor
			? { items: [issue("checkout-b")], totals: { warning: 1 }, truncated: false, sync: { state: "direct", age_s: 0, partial: false } }
			: { items: [issue("checkout-a")], totals: { warning: 1 }, next: "issues-next", truncated: false, sync: { state: "direct", age_s: 0, partial: false } });
		const view = renderPage();
		fireEvent.click(screen.getByRole("tab", { name: "Issues" }));
		expect(await screen.findByText("Deployment payments/checkout-a")).toBeTruthy();
		expect(screen.queryByText("Issue evidence is partial; the server returned bounded results.")).toBeNull();
		fireEvent.click(screen.getByRole("button", { name: "Next page" }));
		expect(await screen.findByText("Deployment payments/checkout-b")).toBeTruthy();
		expect(api.kubernetesIssues).toHaveBeenCalledWith({ namespace: "", severity: "", limit: 20, cursor: "issues-next" });
		expect(view.container.textContent).not.toContain("·");
	});

	it("shows an accessible skeleton while an Issues filter request is pending", async () => {
		vi.mocked(api.kubernetesIssues).mockImplementation((options = {}) => options.severity
			? new Promise(() => {})
			: Promise.resolve({ items: [], totals: {}, truncated: false, sync: { state: "direct", age_s: 0, partial: false } }));
		renderPage();
		fireEvent.click(screen.getByRole("tab", { name: "Issues" }));
		await screen.findByText("No issues match these filters.");
		fireEvent.change(screen.getByLabelText("Issue severity"), { target: { value: "warning" } });
		expect(await screen.findByRole("status", { name: "Loading grouped issues" })).toBeTruthy();
	});

	it("loads namespace choices before requesting a complete connected graph", async () => {
		vi.mocked(api.kubernetesNamespaces).mockResolvedValue({ items: [
			{ resource_id: "core~v1~namespaces", kind: "Namespace", name: "platform" },
			{ resource_id: "core~v1~namespaces", kind: "Namespace", name: "shop" },
		], truncated: false });
		vi.mocked(api.kubernetesGraph).mockResolvedValue({ nodes: [], edges: [], omitted: {}, sync: { state: "ready", age_s: 0, partial: false } });
		renderPage();
		fireEvent.click(screen.getByRole("tab", { name: "Topology" }));
		expect(await screen.findByText("Select one of 2 listed namespaces to load its complete connected graph.")).toBeTruthy();
		expect([...screen.getByRole("combobox", { name: "Topology namespace" }).querySelectorAll("option")].map((option) => option.textContent)).toEqual(["Select a namespace", "platform", "shop"]);
		const completeCallsBeforeSelection = vi.mocked(api.kubernetesGraph).mock.calls.filter(([options]) => options?.complete).length;
		expect(completeCallsBeforeSelection).toBe(0);
		fireEvent.change(screen.getByRole("combobox", { name: "Topology namespace" }), { target: { value: "shop" } });
		expect(await screen.findByText("No connected resources were returned for this namespace.")).toBeTruthy();
		expect(api.kubernetesNamespaces).toHaveBeenCalledTimes(1);
		expect(api.kubernetesGraph).toHaveBeenCalledWith({ namespace: "shop", connected_only: true, complete: true });
		expect(screen.getByRole("heading", { name: "Projected relationships", exact: true })).toBeTruthy();
		expect(screen.queryByText("Projected relationships", { selector: "p" })).toBeNull();
	});

	it("lists failed workload kinds in an expandable partial-evidence notice", async () => {
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [], counts: {}, truncated: true, partial_failures: [{ resource_id: "batch~v1~cronjobs", class: "forbidden" }] });
		renderPage();
		const workloads = await screen.findByRole("region", { name: "Workloads" });
		const disclosure = await within(workloads).findByRole("button", { name: "Some evidence could not be collected (1 categories)" });
		expect(disclosure.getAttribute("aria-expanded")).toBe("false");
		fireEvent.click(disclosure);
		expect(within(workloads).getByText("batch~v1~cronjobs: forbidden")).toBeTruthy();
	});

	it("formats rational pod CPU and fractional windows for the top usage summary", async () => {
		vi.mocked(api.kubernetesTop).mockResolvedValue({ items: [{ kind: "Pod", namespace: "payments", name: "checkout", cpu: "150920827/250000000", memory: "1048576", window: "19.082s" }], items_available: true, total: 1, truncated: false, availability: "available", fresh: true, sync: { state: "ready", age_s: 0, partial: false } });
		renderPage();
		expect(await screen.findByText("CPU 603.683 mCPU")).toBeTruthy();
		expect(screen.getByText("Memory 1 MiB")).toBeTruthy();
		expect(screen.getByText("19 s")).toBeTruthy();
	});

	it("explains previous-container unavailability without crashing the log drawer", async () => {
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [{ resource_id: "core~v1~pods", kind: "Pod", namespace: "default", name: "api" }], counts: { Pod: 1 }, truncated: false });
		vi.mocked(api.kubernetesPodLogs).mockResolvedValue({ cluster_id: "production", namespace: "default", pod: "api", container: "api", previous: true, since_seconds: 0, tail_lines: 200, text: "", truncated: false, partial_failures: [{ scope: "logs", class: "previous_unavailable" }] });
		renderPage();
		fireEvent.click(await screen.findByRole("button", { name: "Select Pod default/api" }));
		fireEvent.click(screen.getByRole("tab", { name: "Logs" }));
		fireEvent.click(screen.getByLabelText("Previous container logs"));
		expect(await screen.findByText("Previous container logs are unavailable because no previous container instance exists.")).toBeTruthy();
		expect(screen.queryByText("Couldn't render this page")).toBeNull();
	});

	it("opens grouped issue roots in the shared resource drawer", async () => {
		vi.mocked(api.kubernetesIssues).mockResolvedValue({ items: [{ root: { kind: "Deployment", namespace: "payments", name: "checkout", api_version: "apps/v1" }, rule: "workload.unavailable", severity: "critical", count: 2, examples: [{ kind: "Pod", namespace: "payments", name: "checkout-a" }] }], totals: { critical: 1 }, truncated: false, sync: { state: "direct", age_s: 0, partial: false } });
		renderPage();
		fireEvent.click(screen.getByRole("tab", { name: "Issues" }));
		await screen.findByText("Deployment payments/checkout");
		const openResourceButtons = await screen.findAllByRole("button", { name: "Open resource" });
		fireEvent.click(openResourceButtons.at(-1)!);
		const drawer = await screen.findByRole("dialog", { name: "Details panel" });
		expect(within(drawer).getByRole("heading", { name: "Deployment payments/checkout" })).toBeTruthy();
		expect(new URLSearchParams(window.location.search).get("r")).toBe("apps~v1~deployments/payments/checkout");
	});

	it.each(["Issues", "Overview"] as const)("keeps long issue details on a full-width mobile track in %s", async (view) => {
		const name = "checkout-api-with-a-long-unbroken-resource-name-for-mobile-triage";
		const example = `${name}-replica-pod`;
		const lastSeen = "2026-10-06T07:06:30Z";
		vi.mocked(api.kubernetesIssues).mockResolvedValue({ items: [{ root: { kind: "Deployment", namespace: "payments", name, api_version: "apps/v1" }, rule: "workload.unavailable", severity: "critical", count: 2, last_seen: lastSeen, examples: [{ kind: "Pod", namespace: "payments", name: example }] }], totals: { critical: 1 }, truncated: false, sync: { state: "direct", age_s: 0, partial: false } });
		renderPage();
		fireEvent.click(screen.getByRole("tab", { name: view }));
		const title = await screen.findByText(`Deployment payments/${name}`);
		const details = title.parentElement!;
		const row = details.parentElement!;
		expect(row.classList.contains("grid")).toBe(true);
		expect(row.classList.contains("grid-cols-[minmax(0,1fr)]")).toBe(true);
		expect(details.classList.contains("min-w-0")).toBe(true);
		expect(details.classList.contains("[overflow-wrap:anywhere]")).toBe(true);
		expect(within(details).getByText("workload unavailable; 2 affected")).toBeTruthy();
		expect(details.querySelector(".truncate, .overflow-hidden, .line-clamp-1")).toBeNull();
		if (view === "Issues") {
			expect(within(details).getByText(`Examples: ${example}`)).toBeTruthy();
			const timestamp = within(row).getByText(new Date(lastSeen).toLocaleString());
			const metadata = timestamp.parentElement!;
			expect(metadata.parentElement).toBe(row);
			expect(metadata).not.toBe(details);
			expect(details.contains(timestamp)).toBe(false);
			expect(metadata.classList.contains("flex-wrap")).toBe(true);
			expect(within(metadata).getByRole("button", { name: "Open resource" })).toBeTruthy();
			expect(row.classList.contains("lg:grid-cols-[auto_minmax(0,1fr)_auto]")).toBe(true);
		} else {
			expect(row.tagName).toBe("BUTTON");
			expect(row.classList.contains("sm:grid-cols-[auto_minmax(0,1fr)]")).toBe(true);
		}
	});

	it("uses bounded resource search and opens the URL-addressable drawer", async () => {
		vi.mocked(api.kubernetesSearch).mockResolvedValue({ items: [{ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "payments", name: "checkout-api" }], truncated: false });
		renderPage();
		fireEvent.keyDown(window, { key: "k", metaKey: true });
		const palette = screen.getByRole("dialog", { name: "Kubernetes command palette" });
		fireEvent.change(within(palette).getByRole("combobox"), { target: { value: "checkout" } });
		await waitFor(() => expect(api.kubernetesSearch).toHaveBeenCalledWith("", "checkout", 20), { timeout: 1000 });
		fireEvent.click(await within(palette).findByRole("option", { name: /checkout-api/ }));
		expect(await screen.findByRole("heading", { name: "Deployment payments/checkout-api" })).toBeTruthy();
		expect(new URLSearchParams(window.location.search).get("r")).toBe("apps~v1~deployments/payments/checkout-api");
	});

	it("shows scrubbed logs and safe projected YAML in the shared drawer", async () => {
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [{ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout" }], truncated: false });
		vi.mocked(api.kubernetesDescribe).mockResolvedValue({ resource: { resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout", summary: { ready: 2, data: "secret-canary", nested: { token: "secret-canary", safe: "projected" } } } });
		vi.mocked(api.kubernetesWorkloadLogs).mockResolvedValue({ pods: ["checkout-a"], lines: [{ pod: "checkout-a", container: "app", text: "password=REDACTED" }], truncated: false });
		vi.mocked(api.kubernetesDiagnose).mockResolvedValue({ workload: { resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout", ready: 2, desired: 3, truncated: false }, warning_events: [], changes: [{ id: "change-diagnosis", cluster: "production", kind: "Deployment", namespace: "default", name: "checkout", uid: "uid-checkout", type: "image_changed", at: "2026-10-05T12:00:00Z" }], neighborhood: { nodes: [{ id: "uid-checkout", kind: "Deployment", namespace: "default", name: "checkout", group: "default" }, { id: "uid-service", kind: "Service", namespace: "default", name: "checkout", group: "default" }], edges: [{ from: "uid-service", to: "uid-checkout", type: "exposes" }], omitted: {}, sync: { state: "ready", age_s: 0, partial: false } }, truncated: false, sync: { state: "direct", age_s: 0, partial: false } });
		renderPage();
		fireEvent.click(await screen.findByRole("button", { name: "Select Deployment default/checkout" }));
		fireEvent.click(screen.getByRole("tab", { name: "Logs" }));
		await waitFor(() => expect(api.kubernetesWorkloadLogs).toHaveBeenCalled());
		const logPromise = api.kubernetesWorkloadLogs.mock.results.at(-1)?.value as Promise<{ lines: Array<{ text: string }> }>;
		await act(async () => { expect((await logPromise).lines[0].text).toBe("password=REDACTED"); });
		await waitFor(() => expect(screen.getByRole("region", { name: "Resource logs" }).textContent).toContain("password=REDACTED"));
		expect(api.kubernetesWorkloadLogs).toHaveBeenCalledWith("Deployment", "default", "checkout", expect.objectContaining({ tail_lines: 200 }));
		fireEvent.click(screen.getByRole("tab", { name: "YAML" }));
		const yaml = screen.getByRole("region", { name: "Projected YAML" }).querySelector("pre")!.textContent!;
		expect(yaml).toContain('name: "checkout"');
		expect(yaml).toContain('safe: "projected"');
		expect(yaml).not.toContain("secret-canary");
		fireEvent.click(screen.getByRole("tab", { name: "Diagnosis" }));
		expect(await screen.findByRole("region", { name: "Diagnosis evidence" })).toBeTruthy();
		expect(await screen.findByText("2 / 3")).toBeTruthy();
		expect(screen.getByRole("region", { name: "Diagnosis change citations" })).toBeTruthy();
		expect(screen.getByRole("region", { name: "Diagnosis neighborhood citations" })).toBeTruthy();
	});

	it("renders projected timeline changes and gaps", async () => {
		vi.mocked(api.kubernetesChanges).mockResolvedValue({ items: [{ id: "change-1", cluster: "production", kind: "Deployment", namespace: "payments", name: "checkout", uid: "uid-1", type: "image_changed", fields: [{ path: "containers.images", from: "checkout:v1", to: "checkout:v2" }], at: "2026-10-05T12:00:00Z" }], gaps: [{ from: "2026-10-05T11:00:00Z", to: "2026-10-05T11:05:00Z" }], sync: { state: "ready", age_s: 0, partial: false } });
		renderPage();
		fireEvent.click(screen.getByRole("tab", { name: "Timeline" }));
		expect(await screen.findByText(/containers.images: checkout:v1/)).toBeTruthy();
		expect(screen.getByRole("region", { name: "Timeline history gaps" })).toBeTruthy();
		fireEvent.click(screen.getByRole("button", { name: /Deployment checkout/ }));
		const drawer = await screen.findByRole("dialog", { name: "Details panel" });
		expect(within(drawer).getByRole("tab", { name: "Timeline" }).getAttribute("aria-selected")).toBe("true");
		expect(await within(drawer).findByRole("region", { name: "Resource change history" })).toBeTruthy();
	});

	it("renders the complete graph and keeps it mounted when opening a resource", async () => {
		const sync = { state: "ready", age_s: 0, partial: false };
		vi.mocked(api.kubernetesGraph).mockResolvedValue({ nodes: [{ id: "deployment-uid", kind: "Deployment", namespace: "shop", name: "checkout", group: "shop" }, { id: "pod-uid", kind: "Pod", namespace: "shop", name: "checkout-7", group: "shop" }], edges: [{ from: "deployment-uid", to: "pod-uid", type: "manages" }], omitted: {}, sync });
		renderPage();
		fireEvent.click(screen.getByRole("tab", { name: "Topology" }));
		await screen.findByRole("option", { name: "shop" });
		fireEvent.change(screen.getByRole("combobox", { name: "Topology namespace" }), { target: { value: "shop" } });
		await screen.findByRole("img", { name: "shop connected Kubernetes relationships" });
		const graph = screen.getByLabelText("Scrollable topology graph");
		expect(within(graph).queryByText("Routes")).toBeNull();
		expect(within(graph).queryByText("Services")).toBeNull();
		const deploymentNode = within(graph).getByRole("button", { name: /Deployment.*checkout/ });
		expect(deploymentNode.style.left).toBe("18px");
		const nodeGeometry = { left: deploymentNode.style.left, top: deploymentNode.style.top };
		const edgeGeometry = graph.querySelector("svg g path")?.getAttribute("d");
		fireEvent.click(screen.getByRole("button", { name: "Zoom in topology" }));
		expect({ left: deploymentNode.style.left, top: deploymentNode.style.top }).toEqual(nodeGeometry);
		expect(graph.querySelector("svg g path")?.getAttribute("d")).toBe(edgeGeometry);
		expect(screen.getByText(/connected resources visible/).textContent).toContain("2 / 2 connected resources visible");
		expect(screen.getByText(/connected resources visible/).textContent).toContain("1 / 1 relationships");
		expect(screen.queryByText(/Bounded graph omitted|projected index is partial|Next page/)).toBeNull();
		expect(api.kubernetesGraph).toHaveBeenCalledWith({ namespace: "shop", connected_only: true, complete: true });
		fireEvent.click(screen.getByRole("button", { name: "Hide Pod", exact: true }));
		expect(screen.getByText(/connected resources visible/).textContent).toContain("1 / 2 connected resources visible");
		expect(screen.getByText(/connected resources visible/).textContent).toContain("0 / 1 relationships");
		fireEvent.click(screen.getByRole("button", { name: "Show all" }));
		fireEvent.click(within(graph).getByRole("button", { name: "Open Pod shop/checkout-7 details" }));
		expect(await screen.findByRole("dialog", { name: "Details panel" })).toBeTruthy();
		expect(api.kubernetesNeighborhood).not.toHaveBeenCalled();
		for (const label of ["Pan topology left", "Zoom in topology", "Zoom out topology", "Fit topology to view"]) expect(screen.getByRole("button", { name: label })).toBeTruthy();
	});

	it("refuses to render a complete topology from partial graph data", async () => {
		vi.mocked(api.kubernetesGraph).mockResolvedValue({ nodes: [{ id: "pod", kind: "Pod", namespace: "shop", name: "api", group: "shop" }], edges: [], omitted: {}, sync: { state: "ready", age_s: 0, partial: true } });
		renderPage();
		fireEvent.click(screen.getByRole("tab", { name: "Topology" }));
		await screen.findByRole("option", { name: "shop" });
		fireEvent.change(screen.getByRole("combobox", { name: "Topology namespace" }), { target: { value: "shop" } });
		expect((await screen.findByRole("alert")).textContent).toContain("The complete connected graph could not be verified");
		expect(screen.queryByLabelText("Scrollable topology graph")).toBeNull();
	});

	it("hides cached graph data after a forbidden complete read", async () => {
		const sync = { state: "ready", age_s: 0, partial: false };
		let completeReads = 0;
		vi.mocked(api.kubernetesGraph).mockImplementation((options) => {
			if (!options?.complete) return Promise.resolve({ nodes: [], edges: [], omitted: {}, sync });
			completeReads += 1;
			return completeReads === 1
				? Promise.resolve({ nodes: [{ id: "pod", kind: "Pod", namespace: "shop", name: "cached" }], edges: [], omitted: {}, sync })
				: Promise.reject(new ApiError(403, "forbidden"));
		});
		renderPage();
		fireEvent.click(screen.getByRole("tab", { name: "Topology" }));
		await screen.findByRole("option", { name: "shop" });
		fireEvent.change(screen.getByRole("combobox", { name: "Topology namespace" }), { target: { value: "shop" } });
		await screen.findByRole("button", { name: "Open Pod shop/cached details" });
		fireEvent.click(screen.getByRole("button", { name: "Refresh topology" }));
		expect((await screen.findByRole("alert")).textContent).toContain("Check sign-in, license, and Kubernetes graph permissions");
		expect(screen.queryByRole("button", { name: "Open Pod shop/cached details" })).toBeNull();
		expect(screen.queryByRole("button", { name: "Retry topology" })).toBeNull();
		expect(screen.queryByLabelText("Scrollable topology graph")).toBeNull();
	});

	it("keeps large graph data intact while culling offscreen DOM and labels", async () => {
		const nodes = Array.from({ length: 1200 }, (_, index) => ({ id: `pod-${index}`, kind: "Pod", namespace: "shop", name: `pod-${index}` }));
		const edges = nodes.slice(1).map((node, index) => ({ from: nodes[index].id, to: node.id, type: "manages" }));
		vi.mocked(api.kubernetesGraph).mockResolvedValue({ nodes, edges, omitted: {}, sync: { state: "ready", age_s: 0, partial: false } });
		renderPage();
		fireEvent.click(screen.getByRole("tab", { name: "Topology" }));
		await screen.findByRole("option", { name: "shop" });
		fireEvent.change(screen.getByRole("combobox", { name: "Topology namespace" }), { target: { value: "shop" } });
		const graph = await screen.findByLabelText("Scrollable topology graph");
		const counts = screen.getByText(/1200 \/ 1200 connected resources visible/).textContent;
		expect(counts).toContain("1199 / 1199 relationships");
		expect(within(graph).getAllByRole("button").length).toBeLessThan(100);
		expect(graph.querySelectorAll("svg g[data-edge-from]").length).toBeLessThan(100);
		expect(graph.querySelectorAll("svg g rect")).toHaveLength(0);
		expect(Number.parseFloat((graph.firstElementChild as HTMLElement).style.height)).toBeGreaterThan(20000);
	});

	it("resets the culled graph viewport after hiding and showing all kinds", async () => {
		const nodes = Array.from({ length: 1200 }, (_, index) => ({ id: `pod-${index}`, kind: "Pod", namespace: "shop", name: `pod-${index}` }));
		vi.mocked(api.kubernetesGraph).mockResolvedValue({ nodes, edges: [], omitted: {}, sync: { state: "ready", age_s: 0, partial: false } });
		renderPage();
		fireEvent.click(screen.getByRole("tab", { name: "Topology" }));
		await screen.findByRole("option", { name: "shop" });
		fireEvent.change(screen.getByRole("combobox", { name: "Topology namespace" }), { target: { value: "shop" } });
		const initialGraph = await screen.findByLabelText("Scrollable topology graph");
		const firstNodeName = "Open Pod shop/pod-0 details";
		expect(within(initialGraph).getByRole("button", { name: firstNodeName })).toBeTruthy();
		initialGraph.scrollTop = 20000;
		fireEvent.scroll(initialGraph);
		expect(within(initialGraph).queryByRole("button", { name: firstNodeName })).toBeNull();
		fireEvent.click(screen.getByRole("button", { name: "Hide all" }));
		expect(screen.queryByLabelText("Scrollable topology graph")).toBeNull();
		fireEvent.click(screen.getByRole("button", { name: "Show all" }));
		const restoredGraph = screen.getByLabelText("Scrollable topology graph");
		expect(restoredGraph.scrollTop).toBe(0);
		expect(within(restoredGraph).getByRole("button", { name: firstNodeName })).toBeTruthy();
	});

	it("routes edge labels around intermediate nodes and into same-column targets", async () => {
		const sync = { state: "ready", age_s: 0, partial: false };
		vi.mocked(api.kubernetesGraph).mockResolvedValue({ nodes: [
			{ id: "service", kind: "Service", namespace: "shop", name: "api", group: "shop" },
			{ id: "deployment", kind: "Deployment", namespace: "shop", name: "api", group: "shop" },
			{ id: "replicaset", kind: "ReplicaSet", namespace: "shop", name: "api-rs", group: "shop" },
			{ id: "config", kind: "ConfigMap", namespace: "shop", name: "settings", group: "shop" },
		], edges: [
			{ from: "service", to: "config", type: "routes-to" },
			{ from: "deployment", to: "replicaset", type: "manages" },
		], omitted: {}, sync });
		renderPage();
		fireEvent.click(screen.getByRole("tab", { name: "Topology" }));
		await screen.findByRole("option", { name: "shop" });
		fireEvent.change(screen.getByRole("combobox", { name: "Topology namespace" }), { target: { value: "shop" } });
		const graph = await screen.findByLabelText("Scrollable topology graph");
		fireEvent.click(screen.getByRole("button", { name: "Show all", exact: true }));
		const nodes = within(graph).getAllByRole("button").map((button) => ({
			left: Number.parseFloat(button.style.left),
			top: Number.parseFloat(button.style.top),
			right: Number.parseFloat(button.style.left) + Number.parseFloat(button.style.width),
			bottom: Number.parseFloat(button.style.top) + 72,
		}));
		const labels = [...graph.querySelectorAll("svg g rect")];
		expect(labels).toHaveLength(2);
		for (const label of labels) {
			const left = Number(label.getAttribute("x"));
			const top = Number(label.getAttribute("y"));
			const right = left + Number(label.getAttribute("width"));
			const bottom = top + Number(label.getAttribute("height"));
			expect(nodes.some((node) => left < node.right && right > node.left && top < node.bottom && bottom > node.top)).toBe(false);
		}
		const sameColumnEdge = [...graph.querySelectorAll("svg g")].find((group) => group.textContent?.includes("manages"))!;
		const target = within(graph).getByRole("button", { name: /ReplicaSet.*api-rs/ });
		const targetRight = Number.parseFloat(target.style.left) + 184;
		const path = sameColumnEdge.querySelector("path")!;
		expect(path.getAttribute("d")).toMatch(new RegExp(` ${targetRight} [\\d.]+$`));
		expect(path.getAttribute("marker-end")).toBe("url(#kubernetes-graph-arrow)");
	});

	it("renders observed traffic edges with source and freshness", async () => {
		vi.mocked(api.kubernetesTraffic).mockResolvedValue({ available: true, source: "otel", window: "15m", observed_at: new Date(Date.now() - 60_000).toISOString(), edges: [{ from: { namespace: "shop", kind: "Deployment", name: "web" }, to: { namespace: "shop", kind: "Deployment", name: "api" }, rate_per_sec: 12.5, error_rate: 0.025, p95_ms: 84, bytes_per_sec: 2048 }], unmapped: 2, external: ["payments.example"], truncated: false });
		renderPage();
		fireEvent.click(screen.getByRole("tab", { name: "Traffic" }));
		const traffic = await screen.findByRole("region", { name: "Kubernetes traffic flows" });
		expect(await within(traffic).findByText("Deployment shop/web")).toBeTruthy();
		expect(within(traffic).getByText("Deployment shop/api")).toBeTruthy();
		expect(within(traffic).getByText("12.5 req/s")).toBeTruthy();
		expect(within(traffic).getByText("2.5%")).toBeTruthy();
		expect(within(traffic).getByText("Source: otel")).toBeTruthy();
		expect(within(traffic).getByText("Fresh")).toBeTruthy();
		expect(within(traffic).getByText("2 unmapped flows")).toBeTruthy();
		expect(within(traffic).getByText("External destinations: payments.example")).toBeTruthy();
		expect(api.kubernetesTraffic).toHaveBeenCalledWith({ namespace: undefined, window: "15m" });
	});

	it("shows stale and license-unavailable traffic states without synthetic edges", async () => {
		vi.mocked(api.kubernetesTraffic).mockResolvedValue({ available: true, source: "otel", window: "5m", observed_at: new Date(Date.now() - 10 * 60_000).toISOString(), edges: [], unmapped: 0, truncated: false });
		renderPage();
		fireEvent.click(screen.getByRole("tab", { name: "Traffic" }));
		const traffic = await screen.findByRole("region", { name: "Kubernetes traffic flows" });
		expect(await within(traffic).findByText("Stale")).toBeTruthy();

		vi.mocked(api.kubernetesTraffic).mockResolvedValue({ available: false, reason: "Telemetry license is required", window: "15m", unmapped: 0, truncated: false });
		fireEvent.change(within(traffic).getByRole("combobox", { name: "Traffic window" }), { target: { value: "1h" } });
		expect(await within(traffic).findByText("License required")).toBeTruthy();
		expect(within(traffic).getByText("Telemetry license is required")).toBeTruthy();
		expect(within(traffic).queryByText(/req\/s/)).toBeNull();
	});

	it("renders null traffic metrics as unavailable while retaining provider window and freshness", async () => {
		vi.mocked(api.kubernetesTraffic).mockResolvedValue({ available: true, source: "otel", window: "5m", observed_at: new Date(Date.now() - 30_000).toISOString(), edges: [{ from: { namespace: "shop", kind: "Deployment", name: "web" }, to: { namespace: "shop", kind: "Deployment", name: "api" }, rate_per_sec: null, error_rate: null, p95_ms: null, bytes_per_sec: null }], unmapped: 0, truncated: false });
		renderPage();
		fireEvent.click(screen.getByRole("tab", { name: "Traffic" }));
		const traffic = await screen.findByRole("region", { name: "Kubernetes traffic flows" });
		expect(await within(traffic).findByText("Fresh")).toBeTruthy();
		expect(within(traffic).getByText(/5m window/)).toBeTruthy();
		expect(within(traffic).getAllByText("Unavailable")).toHaveLength(4);
		expect(within(traffic).queryByText("0 req/s")).toBeNull();
	});

	it("hides Kubernetes proposal controls when the separate actor is not advertised", async () => {
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [{ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout" }], truncated: false });
		vi.mocked(api.listAgentToolsets).mockResolvedValue([{ id: "kubernetes-actions", section: "connector", display_name: "Kubernetes actions", description: "", icon_key: "kubernetes", visibility: "always", state: "needs_capability", reason: "A separate Kubernetes actor is not configured.", action: "", action_label: "", enabled: true, child_count: 1, requirement: { kind: "capability", capabilities: ["kubernetes_actions"] } }]);
		renderPage();
		fireEvent.click(await screen.findByRole("button", { name: "Select Deployment default/checkout" }));
		fireEvent.click(screen.getByRole("tab", { name: "Actions" }));
		const panel = await screen.findByRole("region", { name: "Kubernetes actions" });
		expect(await within(panel).findByText("A separate Kubernetes actor is not configured.")).toBeTruthy();
		expect(within(panel).queryByRole("button", { name: "Restart rollout" })).toBeNull();
		expect(api.proposeAgentAction).not.toHaveBeenCalled();
	});

	it("submits a dry-run proposal and displays server approval and verification", async () => {
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [{ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout" }], truncated: false });
		vi.mocked(api.listAgentToolsets).mockResolvedValue([{ id: "kubernetes-actions", section: "connector", display_name: "Kubernetes actions", description: "", icon_key: "kubernetes", visibility: "always", state: "available", reason: "Available to the agent.", action: "", action_label: "", enabled: true, child_count: 1, requirement: { kind: "capability", capabilities: ["kubernetes_actions"] } }]);
		vi.mocked(api.proposeAgentAction).mockResolvedValue({ proposal: { id: "proposal-1", run_id: "run-1", type: "k8s.rollout_restart", target: { cluster: "production", kind: "Deployment", namespace: "default", name: "checkout" }, params: {}, params_hash: "hash", binding_hash: "binding", dry_run: "Restart Deployment checkout", risk: "medium", reason: "Recover service", proposed_by: "admin", created_at: "2026-10-05T12:00:00Z", expires_at: "2026-10-05T12:10:00Z" }, approval: { id: "approval-1", proposal: { id: "proposal-1", run_id: "run-1", type: "k8s.rollout_restart", target: { cluster: "production", kind: "Deployment", namespace: "default", name: "checkout" }, params: {}, params_hash: "hash", binding_hash: "binding", dry_run: "Restart Deployment checkout", risk: "medium", reason: "Recover service", proposed_by: "admin", created_at: "2026-10-05T12:00:00Z", expires_at: "2026-10-05T12:10:00Z" }, state: "pending", expires_at: "2026-10-05T12:10:00Z" }, nonce: "nonce-1" });
		vi.mocked(api.approveAgentApproval).mockResolvedValue({ id: "approval-1", proposal: { id: "proposal-1", run_id: "run-1", type: "k8s.rollout_restart", target: { cluster: "production", kind: "Deployment", namespace: "default", name: "checkout" }, params: {}, params_hash: "hash", binding_hash: "binding", dry_run: "Restart Deployment checkout", risk: "medium", reason: "Recover service", proposed_by: "admin", created_at: "2026-10-05T12:00:00Z", expires_at: "2026-10-05T12:10:00Z" }, state: "verified", expires_at: "2026-10-05T12:10:00Z", result: { summary: "Restart completed" }, verification: { verified: true, summary: "Kubernetes reports the requested state" } });
		vi.mocked(api.rejectAgentApproval).mockResolvedValue({ id: "approval-1", proposal: { id: "proposal-1", run_id: "run-1", type: "k8s.rollout_restart", target: { cluster: "production", kind: "Deployment", namespace: "default", name: "checkout" }, params: {}, params_hash: "hash", binding_hash: "binding", dry_run: "Restart Deployment checkout", risk: "medium", reason: "Recover service", proposed_by: "admin", created_at: "2026-10-05T12:00:00Z", expires_at: "2026-10-05T12:10:00Z" }, state: "rejected", expires_at: "2026-10-05T12:10:00Z" });
		renderPage();
		fireEvent.click(await screen.findByRole("button", { name: "Select Deployment default/checkout" }));
		fireEvent.click(screen.getByRole("tab", { name: "Actions" }));
		const panel = await screen.findByRole("region", { name: "Kubernetes actions" });
		fireEvent.click(await within(panel).findByRole("button", { name: "Restart rollout" }));
		fireEvent.change(within(panel).getByRole("textbox", { name: "Action reason" }), { target: { value: "Recover service" } });
		fireEvent.click(within(panel).getByRole("button", { name: "Create dry-run proposal" }));
		const details = await within(panel).findByRole("region", { name: "Action proposal details" });
		expect(await within(details).findByText("Restart Deployment checkout")).toBeTruthy();
		expect(api.proposeAgentAction).toHaveBeenCalledWith({ type: "k8s.rollout_restart", target: { cluster: "production", namespace: "default", kind: "Deployment", name: "checkout" }, params: {}, reason: "Recover service" });
		fireEvent.click(within(details).getByRole("button", { name: "Approve and execute" }));
		expect(await within(details).findByText("Verification passed: Kubernetes reports the requested state")).toBeTruthy();
		expect(api.approveAgentApproval).toHaveBeenCalledWith("approval-1", "nonce-1");
		fireEvent.click(within(panel).getByRole("button", { name: "Create dry-run proposal" }));
		const rejectionDetails = await within(panel).findByRole("region", { name: "Action proposal details" });
		fireEvent.change(within(rejectionDetails).getByRole("textbox", { name: "Proposal rejection reason" }), { target: { value: "Change window closed" } });
		fireEvent.click(within(rejectionDetails).getByRole("button", { name: "Reject proposal" }));
		expect(await within(rejectionDetails).findByText("Dry-run proposal: rejected")).toBeTruthy();
		expect(api.rejectAgentApproval).toHaveBeenCalledWith("approval-1", "Change window closed");
	});

	it("opens shared chat with the selected Kubernetes resource attached", async () => {
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [{ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout" }], truncated: false });
		renderPage();
		fireEvent.click(await screen.findByRole("button", { name: "Select Deployment default/checkout" }));
		fireEvent.click(screen.getByRole("button", { name: "Investigate" }));
		const location = await screen.findByLabelText("Current location");
		const search = new URLSearchParams(location.textContent?.split("?")[1]);
		expect(location.textContent).toContain("/agent/chat?");
		expect(search.get("provider")).toBe("kubernetes");
		expect(search.get("cluster")).toBe("production");
		expect(search.get("resource_id")).toBe("apps~v1~deployments");
		expect(search.get("name")).toBe("checkout");
	});

	it("bounds rendered resource rows in the virtualized inventory", async () => {
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: Array.from({ length: 300 }, (_, index) => ({ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: `service-${index + 1}` })), truncated: false });
		renderPage();
		fireEvent.click(screen.getByRole("tab", { name: "Resources" }));
		const list = await screen.findByRole("list", { name: "Virtualized Kubernetes resources" });
		expect(list.querySelectorAll('[role="listitem"]').length).toBeLessThanOrEqual(20);
	});

	it("renders health, partial visibility, and cluster-scoped nodes", async () => {
		const view = renderPage();
		expect(screen.getByLabelText("Loading Kubernetes overview")).toBeTruthy();
		await screen.findByText(/source unavailable/);
		expect(screen.queryByText("Warnings", { exact: true })).toBeNull();
		const nodeCount = screen.getByText("Nodes count unavailable");
		expect(within(nodeCount.closest("article")!).getByText("Unavailable")).toBeTruthy();
		const warningButton = screen.getByRole("button", { name: "1 Kubernetes overview warning" });
		expect(warningButton.getAttribute("aria-describedby")).toBe("kubernetes-overview-warnings");
		expect(screen.getByRole("tooltip").textContent).toContain("Partial cluster visibility: forbidden");
		expect(view.container.querySelector("main")?.className).toContain("overflow-x-hidden");
		expect(screen.queryByText(/kubernetes - production/i)).toBeNull();
		expect(screen.queryByLabelText("Namespace")).toBeNull();
		const nodes = await screen.findByRole("region", { name: "Nodes" });
		expect(within(nodes).getByText("node-a")).toBeTruthy();
		expect(within(nodes).getByText("Ready")).toBeTruthy();
		expect(within(nodes).getByText("4 cores")).toBeTruthy();
		expect(within(nodes).getByText("8 GiB")).toBeTruthy();
		expect(screen.getByRole("tab", { name: "Topology" })).toBeTruthy();
		expect(api.kubernetesNodes).toHaveBeenCalledTimes(1);
	});

	it("sends workload name and namespace filters to the paged endpoint", async () => {
		const checkout = { resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "payments", name: "checkout-api" };
		const nightly = { resource_id: "batch~v1~jobs", kind: "Job", namespace: "platform", name: "nightly-cleanup" };
		vi.mocked(api.kubernetesWorkloads).mockImplementation(async (options = {}) => {
			if (options.namespace === "payments") return { items: [], counts: {}, truncated: false };
			if (options.q === "nightly") return { items: [nightly], counts: { Job: 1 }, truncated: false };
			return { items: [checkout, nightly], counts: { Deployment: 1, Job: 1 }, truncated: false };
		});
		renderPage();
		await screen.findByText("checkout-api");
		fireEvent.change(screen.getByLabelText("Resource name"), { target: { value: "nightly" } });
		expect(await screen.findByText("nightly-cleanup")).toBeTruthy();
		expect(screen.queryByText("checkout-api")).toBeNull();
		await waitFor(() => expect(api.kubernetesWorkloads).toHaveBeenCalledWith(expect.objectContaining({ q: "nightly", limit: 20 })));
		fireEvent.change(screen.getByLabelText("Workload namespace"), { target: { value: "payments" } });
		expect(await screen.findByText("No matching workloads.")).toBeTruthy();
		expect(api.kubernetesWorkloads).toHaveBeenLastCalledWith(expect.objectContaining({ namespace: "payments", q: "nightly", limit: 20 }));
		expect(api.kubernetesSearch).not.toHaveBeenCalled();
	});

	it("always renders All and all six workload kinds with server counts", async () => {
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [], counts: { Deployment: 3, StatefulSet: 2, DaemonSet: 1, Job: 4, CronJob: 5, Pod: 6 }, truncated: false });
		renderPage();
		const tabs = await screen.findByRole("tablist", { name: "Workload kind" });
		await within(tabs).findByText("3", { exact: true });
		for (const kind of ["All", "Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob", "Pod"]) {
			expect(within(tabs).getByRole("tab", { name: kind, exact: true })).toBeTruthy();
		}
		expect(within(tabs).getByText("21", { exact: true })).toBeTruthy();
	});

	it("shows replica readiness from indexed snake_case and string summaries", async () => {
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [
			{ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "shop", name: "web", summary: { ready_replicas: "1", desired_replicas: "3" } },
			{ resource_id: "apps~v1~statefulsets", kind: "StatefulSet", namespace: "shop", name: "db", summary: { readyReplicas: 2, replicas: 2 } },
		], counts: { Deployment: 1, StatefulSet: 1 }, truncated: false });
		renderPage();
		const workloads = await screen.findByRole("region", { name: "Workloads" });
		expect(await within(workloads).findByText("1/3 ready")).toBeTruthy();
		expect(within(workloads).getByText("2/2 ready")).toBeTruthy();
		expect(within(workloads).queryByText("Unknown")).toBeNull();
	});

	it("renders aggregate-source totals instead of capped sample counts", async () => {
		vi.mocked(api.kubernetesUsage).mockResolvedValue({ observed_at: "2026-08-30T12:00:00Z", availability: "stale", fresh: false, pod_metrics: { availability: "stale", fresh: false, complete: true, total: 97, cpu: "2", memory: "4Gi" }, node_metrics: { availability: "stale", fresh: false, complete: true, total: 8, cpu: "1", memory: "2Gi" }, pods: [{ kind: "Pod", namespace: "default", name: "api" }], nodes: [{ kind: "Node", name: "node-a" }], truncated: true });
		renderPage();
		expect(await screen.findByText("Usage stale; 97 pod metrics; 8 node metrics; sample list partial")).toBeTruthy();
	});

	it("formats CPU and memory capacity with explicit human units", async () => {
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ connector: "kubernetes", cluster_id: "production", observed_at: "2026-08-30T12:00:00Z", nodes: 3, ready_nodes: 3, pods: 4, running_pods: 4, namespaces: 2, active_namespaces: 2, workloads: 2, warnings: 0, requested_cpu: "1/2", limited_cpu: "2", allocatable_cpu: "250m", usage_cpu: "1.0004", requested_memory: "1024", limited_memory: "1048576", allocatable_memory: "1Gi", usage_source: "unavailable", metrics_status: "unavailable", metrics_fresh: false, truncated: false });
		vi.mocked(api.kubernetesUsage).mockResolvedValue({ observed_at: "2026-08-30T12:01:00Z", availability: "available", fresh: true, node_metrics: { availability: "available", fresh: true, complete: true, total: 3, cpu: "1.0004", memory: "1024", observed_at: "2026-08-30T12:01:00Z" }, pod_metrics: { availability: "unavailable", fresh: false, complete: false, total: 0 }, truncated: false });
		renderPage();
		await screen.findByText("250 mCPU");
		expect(screen.getAllByText("500 mCPU").length).toBeGreaterThan(0);
		expect(screen.getByText("2 cores")).toBeTruthy();
		expect(screen.getByText("1 core")).toBeTruthy();
		expect(screen.getByText("250 mCPU")).toBeTruthy();
		expect(within(capacityCell("Memory capacity", "Usage")).getByText("1 KiB")).toBeTruthy();
		expect(screen.getByText("1 MiB")).toBeTruthy();
		expect(screen.getByText("1 GiB")).toBeTruthy();
	});

	it("shows pod count and running ratio as unavailable when pod collection is partial", async () => {
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ connector: "kubernetes", cluster_id: "production", observed_at: "2026-08-30T12:00:00Z", nodes: 3, ready_nodes: 3, pods: 0, running_pods: 0, namespaces: 2, active_namespaces: 2, workloads: 2, warnings: 0, requested_cpu: "1", limited_cpu: "2", allocatable_cpu: "4", usage_cpu: "500m", requested_memory: "1Gi", limited_memory: "2Gi", allocatable_memory: "8Gi", usage_memory: "512Mi", usage_source: "metrics_api", metrics_status: "available", metrics_fresh: true, truncated: true, omitted_categories: ["core~v1~pods"], partial_failures: [{ resource_id: "core~v1~pods", class: "response_too_large" }] });
		vi.mocked(api.kubernetesUsage).mockResolvedValue({ observed_at: "2026-08-30T12:01:00Z", availability: "available", fresh: true, node_metrics: { availability: "available", fresh: true, complete: true, total: 3, cpu: "500m", memory: "512Mi" }, pod_metrics: { availability: "unavailable", fresh: false, complete: false, total: 0 }, truncated: false });
		renderPage();
		const podCount = await screen.findByText("Pods count unavailable");
		expect(within(podCount.closest("article")!).getByText("Unavailable")).toBeTruthy();
		expect(screen.queryByText("Pods running 0/0")).toBeNull();
		expect(within(capacityCell("CPU capacity", "Requested")).getByText("Unavailable")).toBeTruthy();
		expect(within(capacityCell("CPU capacity", "Limited")).getByText("Unavailable")).toBeTruthy();
		expect(within(capacityCell("CPU capacity", "Allocatable")).getByText("4 cores")).toBeTruthy();
		expect(within(capacityCell("CPU capacity", "Usage")).getByText("500 mCPU")).toBeTruthy();
		expect(within(capacityCell("Memory capacity", "Requested")).getByText("Unavailable")).toBeTruthy();
		expect(within(capacityCell("Memory capacity", "Limited")).getByText("Unavailable")).toBeTruthy();
		expect(within(capacityCell("Memory capacity", "Allocatable")).getByText("8 GiB")).toBeTruthy();
		expect(within(capacityCell("Memory capacity", "Usage")).getByText("512 MiB")).toBeTruthy();
	});

	it("masks node-owned capacity when node collection fails", async () => {
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ connector: "kubernetes", cluster_id: "production", observed_at: "2026-08-30T12:00:00Z", nodes: 1, ready_nodes: 1, pods: 2, running_pods: 2, namespaces: 1, active_namespaces: 1, workloads: 1, warnings: 0, requested_cpu: "1", limited_cpu: "2", allocatable_cpu: "4", usage_cpu: "500m", requested_memory: "1Gi", limited_memory: "2Gi", allocatable_memory: "8Gi", usage_memory: "512Mi", usage_source: "metrics_api", metrics_status: "available", metrics_fresh: true, truncated: false, partial_failures: [{ resource_id: "core~v1~nodes", class: "forbidden" }] });
		vi.mocked(api.kubernetesUsage).mockResolvedValue({ observed_at: "2026-08-30T12:01:00Z", availability: "available", fresh: true, node_metrics: { availability: "available", fresh: true, complete: true, total: 1, cpu: "500m", memory: "512Mi" }, pod_metrics: { availability: "unavailable", fresh: false, complete: false, total: 0 }, truncated: false });
		renderPage();
		const nodeCount = await screen.findByText("Nodes count unavailable");
		expect(within(nodeCount.closest("article")!).getByText("Unavailable")).toBeTruthy();
		expect(within(capacityCell("CPU capacity", "Requested")).getByText("1 core")).toBeTruthy();
		expect(within(capacityCell("CPU capacity", "Allocatable")).getByText("Unavailable")).toBeTruthy();
		expect(within(capacityCell("CPU capacity", "Usage")).getByText("500 mCPU")).toBeTruthy();
		expect(within(capacityCell("Memory capacity", "Requested")).getByText("1 GiB")).toBeTruthy();
		expect(within(capacityCell("Memory capacity", "Allocatable")).getByText("Unavailable")).toBeTruthy();
		expect(within(capacityCell("Memory capacity", "Usage")).getByText("512 MiB")).toBeTruthy();
	});

	it("shows the workload count as unavailable when any workload category is omitted", async () => {
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ connector: "kubernetes", cluster_id: "production", observed_at: "2026-08-30T12:00:00Z", nodes: 3, ready_nodes: 3, pods: 4, running_pods: 4, namespaces: 2, active_namespaces: 2, workloads: 6, warnings: 0, usage_source: "unavailable", metrics_status: "unavailable", metrics_fresh: false, truncated: true, omitted_categories: ["batch~v1~cronjobs"] });
		renderPage();
		const workloadCount = await screen.findByText("Workloads count unavailable");
		expect(within(workloadCount.closest("article")!).getByText("Unavailable")).toBeTruthy();
		expect(screen.queryByText("discovered resources")).toBeNull();
	});

	it("uses workload cursors and resets to the first page when kind changes", async () => {
		const deployments = Array.from({ length: 20 }, (_, index) => ({ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: `deployment-${index + 1}` }));
		const nightly = { resource_id: "batch~v1~jobs", kind: "Job", namespace: "default", name: "nightly" };
		vi.mocked(api.kubernetesWorkloads).mockImplementation(async (options = {}) => {
			if (options.kind === "Job") return { items: [nightly], counts: { Job: 1 }, truncated: false };
			if (options.cursor === "workload-next") return { items: [nightly], counts: { Deployment: 21, Job: 1 }, truncated: false };
			return { items: deployments, counts: { Deployment: 21, Job: 1 }, next: "workload-next", truncated: false };
		});
		renderPage();
		await screen.findByText("deployment-1");
		const workloads = screen.getByRole("region", { name: "Workloads" });
		fireEvent.click(within(workloads).getByRole("button", { name: "Next page" }));
		expect(await within(workloads).findByText("nightly")).toBeTruthy();
		fireEvent.click(within(workloads).getByRole("tab", { name: "Deployment" }));
		expect(await within(workloads).findByText("deployment-1")).toBeTruthy();
		fireEvent.click(within(workloads).getByRole("tab", { name: "Job" }));
		expect(await within(workloads).findByText("nightly")).toBeTruthy();
		expect(within(workloads).getByRole("button", { name: "Next page" }).hasAttribute("disabled")).toBe(true);
		expect(api.kubernetesWorkloads).toHaveBeenCalledWith(expect.objectContaining({ cursor: "workload-next", limit: 20 }));
		expect(api.kubernetesWorkloads).toHaveBeenLastCalledWith(expect.objectContaining({ kind: "Job", cursor: undefined, limit: 20 }));
	});

	it("fetches only the current server page for workloads and nodes without querying warning events", async () => {
		const workloadItems = Array.from({ length: 20 }, (_, index) => ({ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: `workload-${index + 1}`, summary: { readyReplicas: 1, replicas: 1 } }));
		const nodeItems = Array.from({ length: 20 }, (_, index) => ({ resource_id: "core~v1~nodes", kind: "Node", name: `node-${String(index + 1).padStart(2, "0")}`, conditions: [{ type: "Ready", status: "True" }] }));
		vi.mocked(api.kubernetesWorkloads).mockImplementation(async (options = {}) => options.cursor ? { items: [{ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "workload-21" }], counts: { Deployment: 21 }, truncated: false } : { items: workloadItems, counts: { Deployment: 21 }, next: "workload-next", truncated: false });
		vi.mocked(api.kubernetesNodes).mockImplementation(async (cursor) => cursor ? { items: [{ resource_id: "core~v1~nodes", kind: "Node", name: "node-21" }], truncated: false } : { items: nodeItems, continue: "nodes-next", truncated: true });
		renderPage();
		await screen.findByText("workload-20");
		const workloads = screen.getByRole("region", { name: "Workloads" });
		expect(within(workloads).getByText("workload-20")).toBeTruthy();
		expect(within(workloads).queryByText("workload-21")).toBeNull();
		fireEvent.click(within(workloads).getByRole("button", { name: "Next page" }));
		expect(await within(workloads).findByText("workload-21")).toBeTruthy();
		const nodes = screen.getByRole("region", { name: "Nodes" });
		expect(within(nodes).getByRole("button", { name: "View pods on node-20" })).toBeTruthy();
		expect(within(nodes).queryByRole("button", { name: "View pods on node-21" })).toBeNull();
		fireEvent.click(within(nodes).getByRole("button", { name: "Next page" }));
		expect(await within(nodes).findByRole("button", { name: "View pods on node-21" })).toBeTruthy();
		expect(api.kubernetesEvents).not.toHaveBeenCalled();
	});

	it("shows and cursor-pages all namespaces of pods for a selected node", async () => {
		const firstPods = Array.from({ length: 20 }, (_, index) => ({ resource_id: "core~v1~pods", kind: "Pod", namespace: index % 2 ? "payments" : "platform", name: `pod-${index + 1}`, summary: { phase: index === 0 ? "Pending" : "Running", restart_count: index } }));
		vi.mocked(api.kubernetesNodePods).mockImplementation(async (_node, cursor) => cursor ? { items: [{ resource_id: "core~v1~pods", kind: "Pod", namespace: "payments", name: "pod-21", summary: { phase: "Running", restart_count: 20 } }], truncated: false } : { items: firstPods, continue: "pods-next", truncated: true, partial_failures: [{ class: "response_too_large" }] });
		renderPage();
		const nodes = screen.getByRole("region", { name: "Nodes" });
		fireEvent.click(await within(nodes).findByRole("button", { name: "View pods on node-a" }));
		await waitFor(() => expect(api.kubernetesNodePods).toHaveBeenCalledWith("node-a", undefined));
		expect((await within(nodes).findAllByText("platform")).length).toBeGreaterThan(0);
		expect(within(nodes).getAllByText("payments").length).toBeGreaterThan(0);
		expect(within(nodes).queryByText("pod-21")).toBeNull();
		expect(within(nodes).getByText(/Scheduled pod inventory is partial/)).toBeTruthy();
		fireEvent.click(within(nodes).getByRole("button", { name: "Next page" }));
		expect(await within(nodes).findByText("pod-21")).toBeTruthy();
		fireEvent.click(within(nodes).getByRole("button", { name: "All nodes" }));
		expect(within(nodes).getByRole("button", { name: "View pods on node-a" })).toBeTruthy();
		fireEvent.change(screen.getByLabelText("Workload namespace"), { target: { value: "All" } });
		expect(api.kubernetesUsage).toHaveBeenCalledTimes(1);
		expect(api.kubernetesNodes).toHaveBeenCalledTimes(1);
	});

	it("keeps object-scoped event details available in the resource drawer", async () => {
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [{ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "payments", name: "checkout" }], truncated: false });
		vi.mocked(api.kubernetesDescribe).mockResolvedValue({ resource: { resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "payments", name: "checkout" }, events: [{ resource_id: "core~v1~events", kind: "Event", namespace: "payments", name: "mount-warning", summary: { reason: "FailedMount", message: "Unable to attach the projected volume.", count: 4, lastTimestamp: "2026-08-30T12:00:00Z" } }] });
		renderPage();
		fireEvent.click(await screen.findByRole("button", { name: "Select Deployment payments/checkout" }));
		const drawer = await screen.findByRole("dialog", { name: "Details panel" });
		fireEvent.click(within(drawer).getByRole("tab", { name: "Events" }));
		expect(await within(drawer).findByRole("region", { name: "Object-scoped events" })).toBeTruthy();
		expect(await screen.findByText("Unable to attach the projected volume.")).toBeTruthy();
		expect(within(drawer).getByText(/Count 4/)).toBeTruthy();
		expect(api.kubernetesEvents).not.toHaveBeenCalled();
	});

	it("shows normalized current workload metrics and related pod status", async () => {
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [{ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout", conditions: [{ type: "Ready", status: "True" }] }], truncated: false });
		vi.mocked(api.kubernetesDescribe).mockResolvedValue({ resource: { resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout", summary: { desired: 1, ready: 1, updateStrategy: "RollingUpdate", serviceType: "ClusterIP" } } });
		vi.mocked(api.kubernetesWorkload).mockResolvedValue({ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout", desired: 1, ready: 1, update_strategy: "RollingUpdate", containers: [{ name: "app", limits: { cpu: "750m", memory: "768Mi" } }, { name: "sidecar", limits: { cpu: "250m", memory: "256Mi" } }], usage: [{ kind: "Pod", namespace: "default", name: "checkout-abc", cpu: "1/2", memory: "1073741824", timestamp: "2026-08-30T12:00:00Z" }], pods: [{ name: "checkout-abc", phase: "Running", node: "node-a", restart_count: 2 }], truncated: false });
		renderPage();
		await screen.findByText("checkout");
		fireEvent.click(screen.getByRole("button", { name: "Select Deployment default/checkout" }));
		const metrics = await screen.findByRole("region", { name: /Current workload metrics snapshot/ });
		expect(within(metrics).getByText("Current metrics snapshot")).toBeTruthy();
		expect(metrics.getAttribute("aria-label")).toMatch(/CPU normalized against configured per-pod limits; memory normalized against configured per-pod limits/i);
		expect(within(metrics).queryByText(/CPU normalized/i)).toBeNull();
		expect(within(metrics).getByText("500 mCPU")).toBeTruthy();
		expect(within(metrics).getByText("1 GiB")).toBeTruthy();
		const cpu = within(metrics).getByRole("progressbar", { name: "checkout-abc cpu usage" });
		expect(cpu.getAttribute("aria-valuemax")).toBe("1");
		expect(cpu.getAttribute("aria-valuetext")).toBe("500 mCPU");
		expect(within(metrics).getByRole("progressbar", { name: "checkout-abc memory usage" }).getAttribute("aria-valuetext")).toBe("1 GiB");
		const pods = screen.getByRole("region", { name: "Related pods" });
		expect(within(pods).getByText("Running")).toBeTruthy();
		expect(within(pods).getByText("node-a")).toBeTruthy();
		expect(within(pods).getByText("Pod").closest("div.mt-3")?.getAttribute("tabindex")).toBe("0");
		const detail = screen.getByRole("dialog", { name: "Details panel" });
		expect(within(detail).getByText("Kind").closest("dl")).toBeTruthy();
		expect(within(detail).queryByText("update strategy")).toBeNull();
		expect(within(detail).getByText("service type")).toBeTruthy();
	});

	it("labels within-workload metric fallback when container limits are incomplete", async () => {
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ connector: "kubernetes", cluster_id: "production", observed_at: "2026-08-30T12:00:00Z", nodes: 1, ready_nodes: 1, pods: 1, running_pods: 1, namespaces: 1, active_namespaces: 1, workloads: 1, warnings: 0, usage_source: "metrics_api", metrics_status: "available", metrics_fresh: true, truncated: false });
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [{ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout" }], truncated: false });
		vi.mocked(api.kubernetesDescribe).mockResolvedValue({ resource: { resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout" } });
		vi.mocked(api.kubernetesWorkload).mockResolvedValue({ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout", containers: [{ name: "app", limits: { cpu: "1" } }, { name: "sidecar" }], usage: [{ kind: "Pod", namespace: "default", name: "checkout-abc", cpu: "250m", memory: "256Mi" }], truncated: false });
		renderPage();
		await screen.findByText("checkout");
		fireEvent.click(screen.getByRole("button", { name: "Select Deployment default/checkout" }));
		const metrics = await screen.findByRole("region", { name: /Current workload metrics snapshot/ });
		expect(metrics.getAttribute("aria-label")).toMatch(/CPU normalized within this workload; memory normalized within this workload/i);
		expect(within(metrics).queryByText(/CPU normalized/i)).toBeNull();
	});

	it("shows an explicit metrics unavailable state for a selected workload without samples", async () => {
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [{ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout" }], truncated: false });
		vi.mocked(api.kubernetesDescribe).mockResolvedValue({ resource: { resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout" } });
		vi.mocked(api.kubernetesWorkload).mockResolvedValue({ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout", truncated: false });
		renderPage();
		await screen.findByText("checkout");
		fireEvent.click(screen.getByRole("button", { name: "Select Deployment default/checkout" }));
		expect(await screen.findByText("Metrics API unavailable.")).toBeTruthy();
	});

	it("uses receive time for an actual-span chart, breaks paths across polling gaps, and retains history", async () => {
		vi.useFakeTimers();
		vi.setSystemTime("2026-08-30T12:00:00Z");
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [{ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout" }], truncated: false });
		vi.mocked(api.kubernetesDescribe).mockResolvedValue({ resource: { resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout" } });
		vi.mocked(api.kubernetesWorkload).mockResolvedValue({ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout", pods: [{ name: "checkout-abc", restart_count: 0 }], truncated: false });
		vi.mocked(api.kubernetesUsage)
			.mockResolvedValueOnce({ observed_at: "2026-08-30T09:00:00Z", availability: "available", fresh: true, pods: [{ kind: "Pod", namespace: "default", name: "checkout-abc", cpu: "100m", memory: "128Mi" }], truncated: false })
			.mockResolvedValueOnce({ observed_at: "2026-08-30T14:00:00Z", availability: "available", fresh: true, pods: [{ kind: "Pod", namespace: "default", name: "checkout-abc", cpu: "250m", memory: "256Mi" }], truncated: false })
			.mockResolvedValueOnce({ observed_at: "2026-08-30T12:00:05Z", availability: "available", fresh: true, pods: [{ kind: "Pod", namespace: "default", name: "checkout-abc", cpu: "500m", memory: "512Mi" }], truncated: false })
			.mockRejectedValueOnce(new Error("temporary metrics failure"))
			.mockRejectedValueOnce(new Error("temporary metrics failure"))
			.mockResolvedValue({ observed_at: "2026-08-30T12:01:15Z", availability: "available", fresh: true, pods: [{ kind: "Pod", namespace: "default", name: "checkout-abc", cpu: "750m", memory: "768Mi" }], truncated: false });
		renderPage();
		await vi.waitFor(() => expect(screen.getByText("checkout")).toBeTruthy());
		fireEvent.click(screen.getByRole("button", { name: "Select Deployment default/checkout" }));
		await vi.waitFor(() => expect(api.kubernetesWorkload).toHaveBeenCalledTimes(1));
		await vi.waitFor(() => expect(screen.getAllByText("Collecting 15-minute history.")).toHaveLength(2));
		expect(screen.queryByText(/polling every 15 seconds/)).toBeNull();
		await act(async () => { await vi.advanceTimersByTimeAsync(15_000); });
		await vi.waitFor(() => expect(api.kubernetesUsage).toHaveBeenCalledTimes(2));
		await act(async () => { await vi.advanceTimersByTimeAsync(15_000); });
		await vi.waitFor(() => expect(api.kubernetesUsage).toHaveBeenCalledTimes(3));
		expect(api.kubernetesWorkload).toHaveBeenCalledTimes(1);
		const cpuChart = screen.getByRole("img", { name: /Workload CPU collected since/ });
		const memoryChart = screen.getByRole("img", { name: /Workload Memory collected since/ });
		expect(cpuChart.querySelector('[data-series="cpu"]')?.getAttribute("d")).toContain("L");
		expect(memoryChart.querySelector('[data-series="memory"]')?.getAttribute("d")).toContain("L");
		expect(cpuChart.parentElement?.getAttribute("tabindex")).toBe("0");
		expect(memoryChart.parentElement?.getAttribute("tabindex")).toBe("0");
		fireEvent.pointerMove(cpuChart, { clientX: 0 });
		expect(within(cpuChart).getByTestId("cpu-tooltip").textContent).toContain("100 mCPU");
		fireEvent.pointerLeave(cpuChart);
		expect(within(cpuChart).queryByTestId("cpu-tooltip")).toBeNull();
		fireEvent.focus(cpuChart.parentElement!);
		fireEvent.keyDown(cpuChart.parentElement!, { key: "End" });
		expect(cpuChart.parentElement?.querySelector("#live-cpu-metric-value")?.textContent).toContain("500 mCPU");
		expect(screen.queryByText(/15m ago/)).toBeNull();
		expect(screen.queryByText("CPU, separate scale")).toBeNull();
		expect(screen.queryByText("Memory, separate scale")).toBeNull();
		expect(screen.queryByText("15-minute range")).toBeNull();
		await act(async () => { await vi.advanceTimersByTimeAsync(45_000); });
		await vi.waitFor(() => expect(api.kubernetesUsage).toHaveBeenCalledTimes(6));
		const gappedPath = cpuChart.querySelector('[data-series="cpu"]')?.getAttribute("d") ?? "";
		expect(gappedPath).toMatch(/L .*M /);
		fireEvent.click(screen.getByRole("button", { name: "Close panel" }));
		fireEvent.click(screen.getByRole("button", { name: "Select Deployment default/checkout" }));
		expect(screen.getByRole("img", { name: /Workload CPU collected since/ })).toBeTruthy();
		expect(screen.getByRole("img", { name: /Workload Memory collected since/ })).toBeTruthy();
	});

	it("omits historical snapshots missing a current workload pod", async () => {
		vi.useFakeTimers();
		vi.setSystemTime("2026-08-30T12:00:00Z");
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [{ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout" }], truncated: false });
		vi.mocked(api.kubernetesDescribe).mockResolvedValue({ resource: { resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout" } });
		vi.mocked(api.kubernetesWorkload).mockResolvedValue({ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout", pods: [{ name: "checkout-a", restart_count: 0 }, { name: "checkout-b", restart_count: 0 }], truncated: false });
		vi.mocked(api.kubernetesUsage)
			.mockResolvedValueOnce({ observed_at: "2026-08-30T12:00:00Z", availability: "available", fresh: true, pods: [{ kind: "Pod", namespace: "default", name: "checkout-a", cpu: "100m", memory: "128Mi" }], nodes: [{ kind: "Node", name: "node-a", cpu: "8", memory: "64Gi", extra: "not retained" } as never], truncated: false })
			.mockResolvedValue({ observed_at: "2026-08-30T12:00:15Z", availability: "available", fresh: true, pods: [{ kind: "Pod", namespace: "default", name: "checkout-a", cpu: "200m", memory: "256Mi" }, { kind: "Pod", namespace: "default", name: "checkout-b", cpu: "300m", memory: "384Mi" }], truncated: false });
		renderPage();
		await vi.waitFor(() => expect(screen.getByText("checkout")).toBeTruthy());
		fireEvent.click(screen.getByRole("button", { name: "Select Deployment default/checkout" }));
		await vi.waitFor(() => expect(screen.getAllByText("Collecting 15-minute history.")).toHaveLength(2));
		await act(async () => { await vi.advanceTimersByTimeAsync(15_000); });
		await vi.waitFor(() => expect(api.kubernetesUsage).toHaveBeenCalledTimes(2));
		expect(screen.queryByRole("img", { name: /Workload (CPU|Memory) collected/ })).toBeNull();
		expect(screen.getAllByText("Collecting 15-minute history.")).toHaveLength(2);
	});

	it("renders finite metric paths for samples received in the same millisecond", async () => {
		vi.useFakeTimers();
		vi.setSystemTime("2026-08-30T12:00:00Z");
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [{ resource_id: "core~v1~pods", kind: "Pod", namespace: "default", name: "api" }], truncated: false });
		vi.mocked(api.kubernetesUsage)
			.mockResolvedValueOnce({ observed_at: "2026-08-30T12:00:00Z", availability: "available", fresh: true, pods: [{ kind: "Pod", namespace: "default", name: "api", cpu: "100m", memory: "128Mi" }], truncated: false })
			.mockResolvedValue({ observed_at: "2026-08-30T12:00:01Z", availability: "available", fresh: true, pods: [{ kind: "Pod", namespace: "default", name: "api", cpu: "200m", memory: "256Mi" }], truncated: false });
		renderPage();
		await vi.waitFor(() => expect(screen.getByText("api")).toBeTruthy());
		fireEvent.click(screen.getByRole("button", { name: "Select Pod default/api" }));
		await vi.waitFor(() => expect(screen.getAllByText("Collecting 15-minute history.")).toHaveLength(2));
		await act(async () => { await vi.advanceTimersByTimeAsync(15_000); });
		await vi.waitFor(() => expect(screen.getByRole("img", { name: /Workload CPU collected since/ })).toBeTruthy());
		const chart = screen.getByRole("img", { name: /Workload CPU collected since/ });
		const path = chart.querySelector('[data-series="cpu"]')?.getAttribute("d") ?? "";
		expect(path).toContain("L");
		expect(path).not.toMatch(/NaN|Infinity/);
	});

	it("renders raw limited-RBAC nullable responses without crashing", async () => {
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ connector: "kubernetes", cluster_id: "limited", observed_at: "2026-08-30T12:00:00Z", nodes: 1, ready_nodes: 1, pods: 1, running_pods: 1, namespaces: 0, active_namespaces: 0, workloads: 1, warnings: 0, usage_source: null, metrics_status: null, metrics_fresh: false, truncated: true, omitted_categories: null, partial_failures: [{ resource_id: "core~v1~events", class: "forbidden" }] });
		vi.mocked(api.kubernetesUsage).mockResolvedValue({ observed_at: "2026-08-30T12:00:00Z", availability: "unavailable", fresh: false, pod_metrics: null as never, node_metrics: null as never, pods: null, nodes: null, truncated: true, omitted_categories: null, partial_failures: [{ class: "forbidden" }] });
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [], counts: {}, truncated: true, omitted_categories: null, partial_failures: [{ class: "forbidden" }] });
		vi.mocked(api.kubernetesNodes).mockResolvedValue({ items: null, truncated: true, omitted_categories: ["core~v1~nodes"], partial_failures: [{ class: "forbidden" }] });
		renderPage();
		expect(await screen.findByRole("button", { name: "2 Kubernetes overview warnings" })).toBeTruthy();
		expect(screen.getByText("No workloads in this scope.")).toBeTruthy();
		expect(screen.queryByRole("region", { name: "Recent warnings" })).toBeNull();
		expect(screen.getByText("No nodes in this cluster.")).toBeTruthy();
		expect(screen.getAllByRole("status").some((status) => status.textContent?.includes("forbidden"))).toBe(true);
		expect(api.kubernetesEvents).not.toHaveBeenCalled();
		expect(screen.queryByText("Couldn't render this page")).toBeNull();
	});

	it("keeps every explorer tab renderable when the top-usage list is unavailable", async () => {
		vi.mocked(api.kubernetesTop).mockResolvedValue({ items: [], items_available: false, total: 0, truncated: false, availability: "unavailable", fresh: false, sync: { state: "direct", age_s: 0, partial: true } });
		renderPage();
		expect(await screen.findByText("Usage ranking unavailable; no list was returned.")).toBeTruthy();

		for (const label of ["Overview", "Issues", "Resources", "Timeline", "Topology", "Helm", "GitOps", "Traffic"]) {
			const tab = screen.getByRole("tab", { name: label });
			fireEvent.click(tab);
			expect(tab.getAttribute("aria-selected")).toBe("true");
			expect(screen.queryByRole("heading", { name: "Couldn't render this page" })).toBeNull();
		}
	});

	it("shows actionable connector diagnostics", async () => {
		vi.mocked(api.kubernetesOverview).mockRejectedValue(new ApiError(502, "Kubernetes credentials are unavailable.", {
			error: "Kubernetes credentials are unavailable.",
			code: "credential_unavailable",
			action: "Configure a credential source for tools.kubernetes.auth.mode and restart Versus.",
			retryable: false,
		}));
		renderPage();
		expect(await screen.findByText("Kubernetes credentials are unavailable.")).toBeTruthy();
		expect(screen.getByText("Configure a credential source for tools.kubernetes.auth.mode and restart Versus.")).toBeTruthy();
	});
});