// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider, QueryObserver } from "@tanstack/react-query";
import { BrowserRouter, useLocation } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api, ApiError } from "@/lib/api";
import * as apiModule from "@/lib/api";
import { KubernetesPage } from "./KubernetesPage";
import { ToastProvider } from "@/components/Toast";

vi.mock("@/components/TopBar", () => ({ TopBar: ({ title, actions }: { title: string; actions?: React.ReactNode }) => <header><span>{title}</span>{actions}</header> }));
vi.mock("@/lib/api", async (importActual) => { const actual = await importActual<typeof import("@/lib/api")>(); return { ...actual, api: { ...actual.api, listAgentToolsets: vi.fn(), proposeAgentAction: vi.fn(), approveAgentApproval: vi.fn(), rejectAgentApproval: vi.fn(), kubernetesOverview: vi.fn(), kubernetesSyncHealth: vi.fn(), kubernetesClusterSyncHealth: vi.fn(), kubernetesOverviewGraph: vi.fn(), kubernetesIssues: vi.fn(), kubernetesChanges: vi.fn(), kubernetesGraph: vi.fn(), kubernetesNamespaces: vi.fn(), kubernetesNeighborhood: vi.fn(), kubernetesStream: vi.fn(), kubernetesTop: vi.fn(), kubernetesReleases: vi.fn(), kubernetesGitOpsApps: vi.fn(), kubernetesRollout: vi.fn(), kubernetesRollouts: vi.fn(), kubernetesTraffic: vi.fn(), kubernetesDiagnose: vi.fn(), kubernetesWorkloadLogs: vi.fn(), kubernetesPodLogs: vi.fn(), kubernetesUsage: vi.fn(), kubernetesWorkloads: vi.fn(), kubernetesWorkload: vi.fn(), kubernetesNodes: vi.fn(), kubernetesNodePods: vi.fn(), kubernetesSearch: vi.fn(), kubernetesEvents: vi.fn(), kubernetesDescribe: vi.fn() } }; });

function LocationProbe() { const location = useLocation(); return <output aria-label="Current location">{location.pathname}{location.search}</output>; }
function renderPage(client = new QueryClient({ defaultOptions: { queries: { retry: false } } }), seedCatalog = true) {
	if (seedCatalog && !client.getQueryData(["kubernetes-clusters"])) client.setQueryData(["kubernetes-clusters"], { multiple: false, clusters: [] });
	// The browser router keeps window.location and the router in step, as in the app.
	window.history.replaceState(null, "", `/agent/kubernetes${window.location.search}`);
	return render(<QueryClientProvider client={client}><ToastProvider><BrowserRouter><KubernetesPage /><LocationProbe /></BrowserRouter></ToastProvider></QueryClientProvider>);
}
function renderWorkloads(client?: QueryClient) {
	const query = new URLSearchParams(window.location.search);
	query.set("view", "resources");
	window.history.replaceState(null, "", `/agent/kubernetes?${query}`);
	return renderPage(client);
}

// A deployment that has never had a connector fails both reads that could name
// it: the overview says nothing is configured, and the health report — the one
// answer a configured cluster always gives, whatever its credential is doing —
// says it too. That agreement is what the setup card is owed to.
function mockNeverConfigured() {
	const message = "Kubernetes connector is not configured";
	vi.mocked(api.kubernetesOverview).mockRejectedValue(new ApiError(503, message, { error: message }));
	vi.mocked(api.kubernetesSyncHealth).mockRejectedValue(new ApiError(503, message, { error: message }));
}

function capacityCell(panelName: string, label: string) {
	const panel = screen.getByRole("heading", { name: panelName }).closest("section")!;
	return within(panel).getByText(label).closest("div")!;
}


// Clicks the way a browser does: outside act, so the urgent click render and
// React Router's startTransition navigation commit separately, in order.
async function browserClick(element: HTMLElement) {
	const scope = globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean };
	const previous = scope.IS_REACT_ACT_ENVIRONMENT;
	scope.IS_REACT_ACT_ENVIRONMENT = false;
	try {
		element.click();
		await new Promise((resolve) => setTimeout(resolve, 60));
	} finally {
		scope.IS_REACT_ACT_ENVIRONMENT = previous;
	}
}
async function settle() {
	const scope = globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean };
	const previous = scope.IS_REACT_ACT_ENVIRONMENT;
	scope.IS_REACT_ACT_ENVIRONMENT = false;
	try { await new Promise((resolve) => setTimeout(resolve, 150)); } finally { scope.IS_REACT_ACT_ENVIRONMENT = previous; }
}

beforeEach(() => {
	vi.spyOn(api, "kubernetesTimeline").mockResolvedValue({ items: [], total: 0, crash_count: 0, counts: { change: 0, crash: 0, break: 0 }, since: "2026-10-23T07:00:00Z", until: "2026-10-23T08:00:00Z", gaps: [], sync: { state: "ready", age_s: 0, partial: false } });
	vi.spyOn(api, "kubernetesRelated").mockResolvedValue({ nodes: [], edges: [], omitted: {}, sync: { state: "ready", age_s: 0, partial: false } });
	vi.spyOn(api, "kubernetesUsageHistory").mockResolvedValue({ observed_at: new Date().toISOString(), source: "capture", availability: "unavailable", samples: [], retention_seconds: 21600 });
	vi.spyOn(api, "kubernetesCrashes").mockResolvedValue({ items: [], since: "", until: "", started_at: "", retention_from: "", retention_basis: "capture", gaps: [], capture_gaps: [], coverage: {}, limitations: [], capture: { mode: "continuous", behind: false }, sync: { state: "ready", age_s: 0, partial: false } });
	vi.spyOn(api, "kubernetesWorkloadLogStream").mockImplementation(async (_kind, _namespace, _name, _uid, _options, emit) => emit({ event: "end", text: "", container: "" }));
	vi.spyOn(api, "kubernetesIssueAlerts").mockResolvedValue({ enabled: false, level: "critical", can_write: true, available: true, reason: "", links_ready: true, channels_enabled: true });
	vi.spyOn(apiModule, "createKubernetesApi").mockReturnValue(api);
	vi.spyOn(api, "kubernetesClusters").mockResolvedValue({ multiple: false, clusters: [] });
	vi.spyOn(api, "kubernetesPodLogStream").mockImplementation(async (_namespace, _pod, _options, emit) => {
		emit({ event: "end", text: "", container: "app" });
	});
	vi.mocked(api.kubernetesOverview).mockResolvedValue({ connector: "kubernetes", cluster_id: "production", observed_at: "2026-08-30T12:00:00Z", nodes: 3, ready_nodes: 2, pods: 18, running_pods: 16, namespaces: 5, active_namespaces: 5, workloads: 7, warnings: 2, usage_source: "unavailable", metrics_status: "unavailable", metrics_fresh: false, truncated: false, partial_failures: [{ resource_id: "core~v1~nodes", class: "forbidden" }] });
	// The cluster-wide sync report is the authoritative source for every view, so
	// the default fixture is a healthy one; the gate tests block it themselves.
	vi.mocked(api.kubernetesSyncHealth).mockResolvedValue({ state: "ready", partial: false, age_s: 0, kinds: { Pod: { state: "ready", observed_at: "2026-08-30T12:00:00Z", records: 18, partial: false } }, total_kinds: 1, ready_kinds: 1, failing_kinds: 0, pending_kinds: 0, truncated: false });
	vi.mocked(api.listAgentToolsets).mockResolvedValue([]);
	vi.mocked(api.kubernetesIssues).mockResolvedValue({ items: [], totals: {}, truncated: false, sync: { state: "direct", age_s: 0, partial: false } });
	vi.mocked(api.kubernetesChanges).mockResolvedValue({ items: [], gaps: [], sync: { state: "ready", age_s: 0, partial: false } });
	vi.mocked(api.kubernetesGraph).mockResolvedValue({ nodes: [], edges: [], omitted: {}, sync: { state: "ready", age_s: 0, partial: false } });
	vi.mocked(api.kubernetesOverviewGraph).mockResolvedValue({ nodes: [], edges: [], omitted: {}, sync: { state: "ready", age_s: 0, partial: false } });
	vi.mocked(api.kubernetesNamespaces).mockResolvedValue({ items: ["shop", "payments"], sync: { state: "ready", age_s: 0, partial: false } });
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
	it("follows the coverage tip router link into the Agent tab without reloading", async () => {
		window.history.replaceState(null, "", "/agent/kubernetes?view=timeline&root_uid=root&namespace=shop");
		vi.mocked(api.kubernetesIssues).mockResolvedValue({ items: [{ root: { kind: "Deployment", name: "checkout", namespace: "shop", uid: "root" }, rule: "pod.crash_loop", severity: "critical", count: 1, broken: true, broke_at: "2026-10-17T12:00:00Z" }], totals: {}, truncated: false, sync: { state: "ready", age_s: 0, partial: false } });
		vi.spyOn(api, "kubernetesBreakLogs").mockResolvedValue({ window: { from: "2026-10-17T11:55:00Z", to: "2026-10-17T12:02:00Z" }, pods: [], lines: [], markers: [], counts: { all: 0, errors: 0, warnings: 0 }, rate: [], patterns: [], sources: [], coverage: { state: "none", tip: true, reason: "pod_deleted", gaps: [] } });
		const fields = { namespace: "namespace", pod: "pod", container: "container" };
		vi.spyOn(api, "kubernetesAgentSettings").mockResolvedValue({ learning: { logs: false, metrics: false, traces: false, scope: { include: [], exclude: [] } }, break_logs: { linked_source: "", fields: { loki: fields, elasticsearch: fields, cloudwatchlogs: fields, graylog: fields, splunk: fields, signoz: fields } }, can_write: false, linked_source_missing: false, sources: [], detected: { workloads: 1, namespaces: 1, metrics_server: false, traces: null } });
		renderPage();
		// Overview, the break and its logs load in sequence; allow for a busy worker.
		await screen.findByRole("region", { name: "Logs at break" }, { timeout: 5000 });
		fireEvent.click(await screen.findByRole("button", { name: "Log coverage tip" }, { timeout: 5000 }));
		fireEvent.click(screen.getByRole("link", { name: "Link a source in Agent tab" }));
		await screen.findByRole("region", { name: "Kubernetes Agent" });
		expect(screen.getByRole("tab", { name: "Agent", exact: true }).getAttribute("aria-selected")).toBe("true");
	});
	it("aggregates Overview inventory, history and issue reasons into one top-bar marker", async () => {
		vi.mocked(api.kubernetesIssues).mockResolvedValue({
			items: [{ root: { kind: "Deployment", namespace: "shop", name: "checkout", uid: "root" }, rule: "pod.crash_loop", severity: "critical", count: 1, broken: true,
				gaps: [{ from: "2026-10-23T07:00:00Z", to: "2026-10-23T07:01:00Z" }], untracked: ["Secret", "ConfigMap"],
				evidence: { partial: true, warning_events: 0, from: "2026-10-23T07:00:00Z", to: "2026-10-23T08:00:00Z" } }],
			totals: {}, truncated: false, sync: { state: "ready", age_s: 0, partial: false },
		});
		vi.mocked(api.kubernetesChanges).mockResolvedValue({ items: [], gaps: [{ from: "2026-10-23T07:00:00Z", to: "2026-10-23T07:01:00Z" }], sync: { state: "stale", age_s: 400, partial: true } });
		renderPage();
		await screen.findByRole("region", { name: "Break timeline" });
		await waitFor(() => expect(api.kubernetesChanges).toHaveBeenCalled());
		expect(screen.getAllByRole("button", { name: "Resource coverage" })).toHaveLength(1);
		fireEvent.click(screen.getByRole("button", { name: "Resource coverage" }));
		const details = screen.getByRole("region", { name: "Resource coverage details" });
		for (const reason of ["nodes: permission denied", "Change capture does not cover the whole range", "Secret history is not tracked", "ConfigMap history is not tracked", "Some crash evidence could not be captured", "Change activity is stale."]) expect(details.textContent).toContain(reason);
		expect(within(details).getAllByRole("listitem").length).toBeGreaterThan(3);
	});
	it("opens aggregate workload logs directly in the resource drawer without a coverage header", async () => {
		window.history.replaceState(null, "", "/agent/kubernetes?view=timeline&r=apps~v1~deployments/shop/checkout&tab=logs");
		vi.mocked(api.kubernetesDescribe).mockResolvedValue({ resource: { resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "shop", name: "checkout", uid: "checkout-uid" } });
		vi.mocked(api.kubernetesWorkloadLogStream).mockImplementation(async (_kind, _namespace, _name, _uid, _options, emit) => {
			emit({ event: "line", text: "workload log line", container: "api", pod: "api-old", pod_uid: "pod-uid", source_id: "source", cursor: "cursor" });
			emit({ event: "end", text: "", container: "" });
		});
		renderPage();
		await screen.findByText("workload log line");
		expect(api.kubernetesWorkloadLogStream).toHaveBeenCalledWith("Deployment", "shop", "checkout", "checkout-uid", expect.objectContaining({ previous: false, tail_lines: 200 }), expect.any(Function), expect.any(AbortSignal));
		expect(screen.queryByText(/Partial coverage|Omitted pods|Snapshot read/)).toBeNull();
		expect(api.kubernetesWorkloadLogs).not.toHaveBeenCalled();
		fireEvent.click(screen.getByLabelText("Previous logs"));
		await waitFor(() => expect(api.kubernetesWorkloadLogStream).toHaveBeenLastCalledWith("Deployment", "shop", "checkout", "checkout-uid", expect.objectContaining({ previous: true }), expect.any(Function), expect.any(AbortSignal)));
	});
	it("opens the Logs tab from a row before the describe call answers and only for log-capable kinds", async () => {
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [
			{ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout", uid: "checkout-uid" },
			{ resource_id: "v1~configmaps", kind: "ConfigMap", namespace: "default", name: "settings" },
		], truncated: false });
		// Describe never answers, so nothing can come from it.
		vi.mocked(api.kubernetesDescribe).mockImplementation(() => new Promise(() => undefined));
		vi.mocked(api.kubernetesWorkloadLogStream).mockImplementation(async () => undefined);
		renderWorkloads();
		const logs = await screen.findByRole("button", { name: "Logs for checkout" });
		expect(screen.queryByRole("button", { name: "Logs for settings" })).toBeNull();
		fireEvent.click(logs);
		const output = await screen.findByLabelText("Workload log output");
		expect(output.textContent).toBe("Waiting for the first log line…");
		expect(within(screen.getByRole("tablist", { name: "Resource detail views" })).getByRole("tab", { name: "Logs" }).getAttribute("aria-selected")).toBe("true");
		// The row's own tab never opened, and the stream waits for the list-row identity.
		expect(api.kubernetesWorkloadLogStream).toHaveBeenCalledWith("Deployment", "default", "checkout", "checkout-uid", expect.objectContaining({ tail_lines: 200 }), expect.any(Function), expect.any(AbortSignal));
	});

	it("serves the container selector of a Pod opened from a row from the described resource", async () => {
		const longContainer = "payments-checkout-orchestrator-primary-runtime-container-v2026x";
		// The list projection carries the phase and node but no container summary,
		// which is why the row path used to open the Logs tab with no containers.
		vi.mocked(api.kubernetesNodePods).mockResolvedValue({ items: [{ resource_id: "core~v1~pods", kind: "Pod", namespace: "payments", name: "long-container-pod", summary: { phase: "Running", restart_count: 0, node: "node-a" } }], truncated: false });
		vi.mocked(api.kubernetesDescribe).mockResolvedValue({ resource: { resource_id: "core~v1~pods", kind: "Pod", namespace: "payments", name: "long-container-pod", uid: "long-container-uid", summary: { phase: "Running", log_containers: [{ name: longContainer, type: "regular" }], main_container: longContainer } } });
		vi.mocked(api.kubernetesPodLogStream).mockImplementation(async () => undefined);
		renderPage();
		fireEvent.click(screen.getByRole("tab", { name: "Nodes" }));
		const nodes = await screen.findByRole("region", { name: "Nodes" });
		fireEvent.click(await within(nodes).findByRole("button", { name: "View pods on node-a" }));
		const row = (await screen.findByText("long-container-pod")).closest('[role="listitem"]') as HTMLElement;
		fireEvent.click(within(row).getByRole("button", { name: "Logs for long-container-pod" }));
		const select = (await screen.findByLabelText("Log container")) as HTMLSelectElement;
		await waitFor(() => expect([...select.options].map((option) => option.value)).toEqual([longContainer]));
		// The read opens on the described main container, so that is the selection.
		await waitFor(() => expect(select.value).toBe(longContainer));
		expect(api.kubernetesPodLogStream).toHaveBeenCalledWith("payments", "long-container-pod", expect.objectContaining({ container: longContainer }), expect.any(Function), expect.any(AbortSignal));
	});

	it("marks the open drawer without narrowing the list it covers", async () => {
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [{ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout", uid: "checkout-uid" }], truncated: false });
		// Describe never answers: the drawer opens from the row identity alone.
		vi.mocked(api.kubernetesDescribe).mockImplementation(() => new Promise(() => undefined));
		vi.mocked(api.kubernetesWorkloadLogStream).mockImplementation(async () => undefined);
		renderWorkloads();
		const pane = (await screen.findByRole("region", { name: "Workloads" })).closest(".kubernetes-explorer-content") as HTMLElement;
		// Nothing covers the list yet, so the pane keeps the whole width.
		expect(pane.getAttribute("data-drawer")).toBeNull();
		fireEvent.click(await screen.findByRole("button", { name: "Logs for checkout" }));
		await screen.findByLabelText("Workload log output");
		// The drawer is a fixed overlay: the list keeps the width it was laid out
		// for, and the marker records that something covers it. No width is
		// reserved, so no column or scrollbar moves.
		expect(pane.getAttribute("data-drawer")).toBe("open");
		expect(pane.getAttribute("style")).toBeNull();
		expect(pane.className).not.toContain("pr-");
		fireEvent.click(screen.getByRole("button", { name: "Close panel" }));
		await waitFor(() => expect(pane.getAttribute("data-drawer")).toBeNull());
	});

	it("keeps the row's Pod instance when the same name comes back with a new uid", async () => {
		vi.mocked(api.kubernetesNodePods).mockResolvedValue({ items: [{ resource_id: "core~v1~pods", kind: "Pod", namespace: "payments", name: "checkout-7d9", uid: "deleted-instance", summary: { phase: "Running", restart_count: 0, node: "node-a" } }], truncated: false });
		// The row still holds the instance that was deleted; describe answers for
		// the Pod that took the same name after it.
		vi.mocked(api.kubernetesDescribe).mockResolvedValue({ resource: { resource_id: "core~v1~pods", kind: "Pod", namespace: "payments", name: "checkout-7d9", uid: "replacement-instance", summary: { phase: "Running", log_containers: [{ name: "replacement", type: "regular" }], main_container: "replacement" } } });
		vi.mocked(api.kubernetesPodLogStream).mockImplementation(async () => undefined);
		renderPage();
		fireEvent.click(screen.getByRole("tab", { name: "Nodes" }));
		const nodes = await screen.findByRole("region", { name: "Nodes" });
		fireEvent.click(await within(nodes).findByRole("button", { name: "View pods on node-a" }));
		const row = (await screen.findByText("checkout-7d9")).closest('[role="listitem"]') as HTMLElement;
		fireEvent.click(within(row).getByRole("button", { name: "Logs for checkout-7d9" }));
		await screen.findByLabelText("Pod logs");
		await waitFor(() => expect(api.kubernetesDescribe).toHaveBeenCalled());
		await settle();
		// The replacement's container is not evidence about the row's instance.
		expect(vi.mocked(api.kubernetesPodLogStream).mock.calls.every((call) => call[2].container === "")).toBe(true);
		expect(screen.queryByRole("button", { name: "replacement" })).toBeNull();
	});

	it("measures list rows against the card so the open drawer cannot hide their columns", async () => {
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [{ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout", uid: "checkout-uid" }], truncated: false });
		vi.mocked(api.kubernetesNodes).mockResolvedValue({ items: [{ resource_id: "core~v1~nodes", kind: "Node", name: "node-a", conditions: [{ type: "Ready", status: "True" }] }], truncated: false });
		renderWorkloads();
		const workloads = await screen.findByRole("region", { name: "Workloads" });
		// The card is the container the rows re-flow against.
		expect(workloads.className).toContain("kubernetes-list-surface");
		const row = (await within(workloads).findByText("checkout")).closest(".kubernetes-list-row--workload") as HTMLElement;
		expect(row.className).toContain("kubernetes-list-row--workload");
		// A viewport breakpoint would keep all five columns while the drawer narrows the card.
		expect(row.className).not.toContain("sm:grid-cols-");
		expect(row.querySelector(".kubernetes-list-namespace")).toBeTruthy();
		fireEvent.click(screen.getByRole("tab", { name: "Nodes" }));
		const nodes = await screen.findByRole("region", { name: "Nodes" });
		expect(nodes.className).toContain("kubernetes-list-surface");
		const node = within(nodes).getByRole("button", { name: "View pods on node-a" });
		expect(node.className).toContain("kubernetes-list-row--nodes");
		const pod = await (async () => {
			vi.mocked(api.kubernetesNodePods).mockResolvedValue({ items: [{ resource_id: "core~v1~pods", kind: "Pod", namespace: "payments", name: "bulk-app-0", summary: { phase: "Running", restart_count: 0 } }], truncated: false });
			fireEvent.click(node);
			return (await screen.findByText("bulk-app-0")).closest(".kubernetes-list-row--pods");
		})();
		expect(pod).toBeTruthy();
	});

	it("prepares the drawer with the row identity instead of a bare skeleton", async () => {
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [{ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout", uid: "checkout-uid" }], truncated: false });
		vi.mocked(api.kubernetesDescribe).mockImplementation(() => new Promise(() => undefined));
		vi.mocked(api.kubernetesWorkload).mockImplementation(() => new Promise(() => undefined));
		renderWorkloads();
		const logs = await screen.findByRole("button", { name: "Logs for checkout" });
		vi.useFakeTimers();
		fireEvent.click(logs);
		// Nothing flashes before 300ms.
		expect(document.querySelector("[data-resource-preparing]")).toBeNull();
		await act(async () => { await vi.advanceTimersByTimeAsync(300); });
		const panel = document.querySelector("[data-resource-preparing]")!;
		expect(panel.getAttribute("aria-label")).toBe("Preparing Deployment checkout");
		expect(panel.textContent).toContain("checkout");
		expect(panel.textContent).toContain("Deployment in default");
		expect(panel.textContent).toContain("Reading resource");
		await act(async () => { await vi.advanceTimersByTimeAsync(2000); });
		expect(panel.textContent).toContain("2s");
		expect(panel.textContent).not.toMatch(/[·•]/);
	});

	it("preserves inventory alongside unrelated failures and keeps coverage focus on snapshot refresh", async () => {
		const overview = { connector: "kubernetes", cluster_id: "production", observed_at: "2026-10-14T12:00:00Z", nodes: 3, ready_nodes: 2, pods: 18, running_pods: 16, namespaces: 5, active_namespaces: 5, workloads: 7, warnings: 2, truncated: true, partial_failures: [{ resource_id: "ConfigMap", class: "forbidden" }] };
		vi.mocked(api.kubernetesOverview).mockResolvedValue(overview);
		const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
		renderPage(client);
		await screen.findByRole("region", { name: "Cluster health", exact: true });
		const toggle = screen.getByRole("button", { name: "Resource coverage" });
		fireEvent.click(toggle);
		toggle.focus();
		expect(screen.getByText("ConfigMap: permission denied")).toBeTruthy();
		expect(screen.queryByText(/Overview inventory is bounded/)).toBeNull();
		await act(async () => { client.setQueryData(["kubernetes", "_", "kubernetes-overview"], { ...overview, observed_at: "2026-10-14T12:00:30Z" }); });
		expect(screen.getByRole("button", { name: "Resource coverage" })).toBe(toggle);
		expect(toggle.getAttribute("aria-expanded")).toBe("true");
		expect(document.activeElement).toBe(toggle);
	});
	it.each([401, 403])("preserves the overview access-denied presentation for catalog HTTP %s without dashboard reads", async (status) => {
		vi.mocked(api.kubernetesClusters).mockRejectedValue(new ApiError(status, "Access denied", { action: "Check RBAC" }));
		renderPage(new QueryClient({ defaultOptions: { queries: { retry: false } } }), false);
		const alert = await screen.findByRole("alert");
		expect(within(alert).getByText("Couldn't load Kubernetes overview")).toBeTruthy();
		expect(within(alert).getByText("Access denied")).toBeTruthy();
		expect(within(alert).getByText("Check RBAC")).toBeTruthy();
		expect(screen.queryByText("Couldn't load clusters")).toBeNull();
		expect(api.kubernetesOverview).not.toHaveBeenCalled();
		expect(api.kubernetesStream).not.toHaveBeenCalled();
		vi.mocked(api.kubernetesClusters).mockResolvedValue({ multiple: false, clusters: [] });
		fireEvent.click(within(alert).getByRole("button", { name: "Retry" }));
		expect(await screen.findByRole("region", { name: "Cluster health" })).toBeTruthy();
	});

	it.each([false, true])("polls the catalog only in multi mode: multiple=%s", async (multiple) => {
		const catalog = { multiple, clusters: [] };
		vi.mocked(api.kubernetesClusters).mockResolvedValue(catalog);
		const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
		client.setQueryData(["kubernetes-clusters"], catalog);
		vi.useFakeTimers();
		await act(async () => { renderPage(client); });
		expect(api.kubernetesClusters).not.toHaveBeenCalled();
		await act(async () => { await vi.advanceTimersByTimeAsync(30_000); });
		expect(api.kubernetesClusters).toHaveBeenCalledTimes(multiple ? 1 : 0);
		cleanup();
		client.clear();
	});

	it("refreshes the catalog every four seconds while a cluster prepares, then every thirty", async () => {
		const entry = { id: "one", provider: "generic", nodes: 2, ready_nodes: 2, pods: 4, running_pods: 4, warnings: 0, observed_at: "2026-10-09T12:00:00Z" };
		const preparing = { ...entry, health: "syncing" as const, sync: { state: "syncing", age_s: 0, partial: false } };
		const settled = { ...entry, health: "healthy" as const, sync: { state: "live", age_s: 0, partial: false } };
		vi.mocked(api.kubernetesClusters).mockResolvedValue({ multiple: true, clusters: [settled] });
		const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
		client.setQueryData(["kubernetes-clusters"], { multiple: true, clusters: [preparing] });
		vi.useFakeTimers();
		try {
			await act(async () => { renderPage(client); });
			expect(api.kubernetesClusters).not.toHaveBeenCalled();
			await act(async () => { await vi.advanceTimersByTimeAsync(3_900); });
			expect(api.kubernetesClusters).not.toHaveBeenCalled();
			await act(async () => { await vi.advanceTimersByTimeAsync(100); });
			expect(api.kubernetesClusters).toHaveBeenCalledTimes(1);
			// The fresh payload has settled, so the next poll waits for the slow interval.
			await act(async () => { await vi.advanceTimersByTimeAsync(29_000); });
			expect(api.kubernetesClusters).toHaveBeenCalledTimes(1);
			await act(async () => { await vi.advanceTimersByTimeAsync(1_000); });
			expect(api.kubernetesClusters).toHaveBeenCalledTimes(2);
		} finally {
			cleanup();
			vi.useRealTimers();
			client.clear();
		}
	});

	it("does not poll the catalog while the tab is hidden", async () => {
		const preparing = { id: "one", health: "syncing" as const, provider: "generic", nodes: 0, ready_nodes: 0, pods: 0, running_pods: 0, warnings: 0, sync: { state: "syncing", age_s: 0, partial: false }, observed_at: "2026-10-25T08:00:00Z" };
		vi.mocked(api.kubernetesClusters).mockResolvedValue({ multiple: true, clusters: [preparing] });
		const client = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
		client.setQueryData(["kubernetes-clusters"], { multiple: true, clusters: [preparing] });
		const hidden = (value: DocumentVisibilityState) => Object.defineProperty(document, "visibilityState", { configurable: true, get: () => value });
		const original = Object.getOwnPropertyDescriptor(document, "visibilityState");
		vi.useFakeTimers();
		try {
			hidden("hidden");
			await act(async () => { renderPage(client); });
			await act(async () => { await vi.advanceTimersByTimeAsync(12_000); });
			expect(api.kubernetesClusters).not.toHaveBeenCalled();
			hidden("visible");
			await act(async () => { await vi.advanceTimersByTimeAsync(4_000); });
			expect(api.kubernetesClusters).toHaveBeenCalledTimes(1);
		} finally {
			if (original) Object.defineProperty(document, "visibilityState", original);
			else Reflect.deleteProperty(document, "visibilityState");
			cleanup();
			vi.useRealTimers();
			client.clear();
		}
	});

	it("uses fleet mode with one configured cluster and never starts dashboard streams", async () => {
		const catalog = { multiple: true, clusters: [{ id: "one", health: "healthy" as const, provider: "generic", nodes: 1, ready_nodes: 1, pods: 1, running_pods: 1, warnings: 0, sync: { state: "live", age_s: 0, partial: false }, observed_at: "2026-10-09T12:00:00Z" }] };
		vi.mocked(api.kubernetesClusters).mockResolvedValue(catalog);
		const client = new QueryClient();
		client.setQueryData(["kubernetes-clusters"], catalog);
		renderPage(client);
		expect(await screen.findByRole("article", { name: "Cluster one" })).toBeTruthy();
		expect(api.kubernetesOverview).not.toHaveBeenCalled();
		expect(api.kubernetesStream).not.toHaveBeenCalled();
	});

	it("opens a cluster that is still preparing its first data", async () => {
		const syncing = { id: "one", health: "syncing" as const, provider: "generic", nodes: 0, ready_nodes: 0, pods: 0, running_pods: 0, warnings: 0, sync: { state: "syncing", age_s: 0, partial: false }, observed_at: "2026-10-25T08:00:00Z" };
		const catalog = { multiple: true, clusters: [syncing] };
		vi.mocked(api.kubernetesClusters).mockResolvedValue(catalog);
		vi.mocked(api.kubernetesClusterSyncHealth).mockResolvedValue({ state: "syncing", partial: false, age_s: 0, kinds: {}, total_kinds: 20, ready_kinds: 0, failing_kinds: 0, pending_kinds: 20, truncated: false });
		const client = new QueryClient();
		client.setQueryData(["kubernetes-clusters"], catalog);
		renderPage(client);
		const row = await screen.findByRole("article", { name: "Cluster one" });
		// The row's state waits for the cluster's own report.
		await settle();
		expect(within(row).getByText("Preparing", { selector: ".pill" })).toBeTruthy();
		await browserClick(within(row).getByRole("link", { name: /^Open cluster one/ }));
		await waitFor(() => expect(screen.getByLabelText("Current location").textContent).toBe("/agent/kubernetes?cluster=one"));
		await waitFor(() => expect(api.kubernetesOverview).toHaveBeenCalled());
	});

	it("switches cluster caches and aborts the old dashboard stream while retaining the view", async () => {
		const entry = { health: "healthy" as const, provider: "generic", nodes: 1, ready_nodes: 1, pods: 1, running_pods: 1, warnings: 0, sync: { state: "live", age_s: 0, partial: false }, observed_at: "2026-10-09T12:00:00Z" };
		const catalog = { multiple: true, clusters: [{ ...entry, id: "one" }, { ...entry, id: "two" }] };
		vi.mocked(api.kubernetesClusters).mockResolvedValue(catalog);
		const client = new QueryClient();
		client.setQueryData(["kubernetes-clusters"], catalog);
		window.history.replaceState(null, "", "/agent/kubernetes?cluster=one&view=nodes");
		renderPage(client);
		await waitFor(() => expect(api.kubernetesStream).toHaveBeenCalledOnce());
		const signal = vi.mocked(api.kubernetesStream).mock.calls[0][1];
		fireEvent.change(screen.getByRole("combobox", { name: "Cluster", exact: true }), { target: { value: "two" } });
		await waitFor(() => expect(apiModule.createKubernetesApi).toHaveBeenCalledWith("two"));
		expect(signal?.aborted).toBe(true);
		expect(screen.getByLabelText("Current location").textContent).toBe("/agent/kubernetes?cluster=two&view=nodes");
		await waitFor(() => expect(client.getQueryData(["kubernetes", "two", "kubernetes-overview"])).toBeTruthy());
		expect(client.getQueryData(["kubernetes", "one", "kubernetes-overview"])).toBeTruthy();
	});

	it("returns unknown cluster deep links to the fleet with a notice", async () => {
		const catalog = { multiple: true, clusters: [] };
		vi.mocked(api.kubernetesClusters).mockResolvedValue(catalog);
		const client = new QueryClient();
		client.setQueryData(["kubernetes-clusters"], catalog);
		window.history.replaceState(null, "", "/agent/kubernetes?cluster=hidden");
		renderPage(client);
		expect(await screen.findByText("This cluster is no longer available.")).toBeTruthy();
		expect(api.kubernetesOverview).not.toHaveBeenCalled();
		expect(api.kubernetesStream).not.toHaveBeenCalled();
	});

	it("moves workload and node inventories out of Overview into their own tabs", async () => {
		renderPage();
		await screen.findByRole("region", { name: "Overview topology" });
		expect(screen.queryByRole("region", { name: "Workloads" })).toBeNull();
		expect(screen.queryByRole("region", { name: "Nodes" })).toBeNull();
		expect(screen.queryByRole("tab", { name: "Resources" })).toBeNull();
		fireEvent.click(screen.getByRole("tab", { name: "Workloads" }));
		expect(await screen.findByRole("region", { name: "Workloads" })).toBeTruthy();
		expect(screen.queryByRole("region", { name: "Nodes" })).toBeNull();
		expect(new URLSearchParams(window.location.search).get("view")).toBe("resources");
		fireEvent.click(screen.getByRole("tab", { name: "Nodes" }));
		const nodes = await screen.findByRole("region", { name: "Nodes" });
		expect(await within(nodes).findByText("node-a")).toBeTruthy();
		expect(screen.queryByRole("region", { name: "Workloads" })).toBeNull();
		expect(new URLSearchParams(window.location.search).get("view")).toBe("nodes");
	});

	it.each(["resources", "nodes"])("loads the %s inventory tab from its URL", async (view) => {
		window.history.replaceState(null, "", `/agent/kubernetes?view=${view}`);
		renderPage();
		const name = view === "nodes" ? "Nodes" : "Workloads";
		expect(screen.getByRole("tab", { name }).getAttribute("aria-selected")).toBe("true");
		expect(await screen.findByRole("region", { name })).toBeTruthy();
		expect(screen.queryByRole("region", { name: "Overview topology" })).toBeNull();
	});

	it.each([
		"/agent/kubernetes",
		"/agent/kubernetes?view=topology&r=core~v1~pods/default/api&tab=logs",
		"/agent/kubernetes?view=issues&r=apps~v1~deployments/default/api&tab=diagnosis",
		"/agent/kubernetes?view=timeline&r=apps~v1~deployments/default/api&tab=timeline",
	])("shows connector setup without cluster reads for missing configuration at %s", async (location) => {
		window.history.replaceState(null, "", location);
		mockNeverConfigured();
		renderPage();
		const card = await screen.findByRole("article", { name: "Kubernetes connector" });
		expect(within(card).getByText("Connection needed")).toBeTruthy();
		expect(card.querySelector(".tool-brand-kubernetes")).toBeTruthy();
		const docs = within(card).getByRole("link", { name: "Documentation" });
		expect(docs.getAttribute("href")).toBe("https://docs.versusincident.com/#/agent/connectors/kubernetes");
		expect(docs.getAttribute("target")).toBe("_blank");
		expect(docs.getAttribute("rel")).toBe("noopener noreferrer");
		expect(screen.queryByRole("tablist")).toBeNull();
		expect(screen.queryByRole("alert")).toBeNull();
		for (const query of [api.kubernetesUsage, api.kubernetesWorkloads, api.kubernetesNodes, api.kubernetesStream, api.kubernetesOverviewGraph, api.kubernetesNamespaces, api.kubernetesIssues, api.kubernetesChanges, api.kubernetesDescribe, api.kubernetesWorkload, api.kubernetesDiagnose, api.kubernetesPodLogStream, api.listAgentToolsets]) expect(query).not.toHaveBeenCalled();
	});

	it.each([
		new ApiError(401, "Kubernetes connector is not configured"),
		new ApiError(403, "Kubernetes connector is not configured"),
		new ApiError(503, "Kubernetes authentication failed"),
		new ApiError(503, "Kubernetes is temporarily unavailable"),
		new ApiError(500, "Kubernetes connector is not configured"),
		new Error("Network unavailable"),
		new Error("Kubernetes connector is not configured"),
	])("preserves retryable overview errors without inferring missing configuration: %s", async (error) => {
		vi.mocked(api.kubernetesOverview).mockRejectedValue(error);
		renderPage();
		const alert = await screen.findByRole("alert");
		expect(within(alert).getByText(error.message)).toBeTruthy();
		expect(within(alert).getByRole("button", { name: "Retry", exact: true })).toBeTruthy();
		expect(screen.queryByRole("article", { name: "Kubernetes connector" })).toBeNull();
	});

	it("keeps loading distinct from missing setup and recovers after checking the connection", async () => {
		let rejectOverview!: (error: Error) => void;
		// Nothing is configured at first, so the report route says so too. Once a
		// connector exists it answers again, and the page must follow it.
		vi.mocked(api.kubernetesSyncHealth).mockRejectedValueOnce(new ApiError(503, "Kubernetes connector is not configured", { error: "Kubernetes connector is not configured" }));
		vi.mocked(api.kubernetesOverview).mockImplementationOnce(() => new Promise((_resolve, reject) => { rejectOverview = reject; }));
		renderPage();
		expect(screen.getByLabelText("Loading Kubernetes overview")).toBeTruthy();
		expect(screen.queryByRole("article", { name: "Kubernetes connector" })).toBeNull();
		expect(api.kubernetesWorkloads).not.toHaveBeenCalled();
		await act(async () => rejectOverview(new ApiError(503, "Kubernetes connector is not configured")));
		await screen.findByRole("article", { name: "Kubernetes connector" });
		fireEvent.click(screen.getByRole("button", { name: "Check connection" }));
		expect(await screen.findByRole("region", { name: "Cluster health" })).toBeTruthy();
		await waitFor(() => expect(api.kubernetesWorkloads).toHaveBeenCalledOnce());
		expect(screen.queryByRole("article", { name: "Kubernetes connector" })).toBeNull();
	});

	it("hides cached cluster evidence and stops dependent reads when a refresh confirms missing setup", async () => {
		renderPage();
		await screen.findByRole("region", { name: "Cluster health" });
		await waitFor(() => expect(api.kubernetesStream).toHaveBeenCalledOnce());
		const signal = vi.mocked(api.kubernetesStream).mock.calls[0][1];
		vi.mocked(api.kubernetesSyncHealth).mockRejectedValue(new ApiError(503, "Kubernetes connector is not configured", { error: "Kubernetes connector is not configured" }));
		vi.mocked(api.kubernetesOverview).mockRejectedValueOnce(new ApiError(503, "Kubernetes connector is not configured"));
		fireEvent.click(screen.getByRole("button", { name: "Refresh Kubernetes data" }));
		await screen.findByRole("article", { name: "Kubernetes connector" });
		expect(screen.queryByRole("region", { name: "Cluster health" })).toBeNull();
		expect(signal?.aborted).toBe(true);
		const calls = vi.mocked(api.kubernetesWorkloads).mock.calls.length;
		vi.mocked(api.kubernetesOverview).mockRejectedValueOnce(new ApiError(503, "Kubernetes connector is not configured"));
		fireEvent.click(screen.getByRole("button", { name: "Check connection" }));
		await waitFor(() => expect(api.kubernetesOverview).toHaveBeenCalledTimes(3));
		expect(api.kubernetesWorkloads).toHaveBeenCalledTimes(calls);
	});

	it("uses actual overview totals in a scrollable Topology card without requesting any graph", async () => {
		renderPage();
		const topology = await screen.findByRole("region", { name: "Overview topology" });
		const count = (kind: string) => within(topology).queryByText(kind, { exact: true })?.closest("dt")?.parentElement?.querySelector("dd")?.textContent;
		expect(count("Pod")).toBe("18");
		expect(count("Namespace")).toBe("5");
		expect(count("Node")).toBeUndefined();
		expect(count("Deployment")).toBeUndefined();
		expect(count("Workloads (aggregate)")).toBe("7");
		expect(within(topology).queryByRole("img", { name: "Kubernetes relationship schematic" })).toBeNull();
		expect(within(topology).getByRole("region", { name: "Topology resource counts" }).className).toContain("overflow-y-auto");
		expect(topology.querySelector("article.card")).toBeTruthy();
		expect(within(topology).getByText(/\+\d+ kinds not reported/)).toBeTruthy();
		expect(api.kubernetesOverviewGraph).not.toHaveBeenCalled();
		expect(api.kubernetesGraph).not.toHaveBeenCalled();
		expect(api.kubernetesNamespaces).not.toHaveBeenCalled();
		fireEvent.click(within(topology).getByRole("button", { name: "View all relationships" }));
		fireEvent.click(await screen.findByRole("button", { name: "Open namespace shop topology" }));
		await screen.findByText("No connected resources were returned for this namespace.");
		expect(api.kubernetesGraph).toHaveBeenCalledWith({ namespace: "shop", connected_only: true, complete: true });
	});

	it("refreshes overview counts across stream invalidations without fetching topology", async () => {
		const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
		const invalidate = vi.spyOn(client, "invalidateQueries");
		renderPage(client);
		const queries = [api.kubernetesOverview, api.kubernetesWorkloads, api.kubernetesNodes, api.kubernetesIssues, api.kubernetesTop, api.kubernetesChanges];
		await waitFor(() => {
			expect(api.kubernetesOverviewGraph).not.toHaveBeenCalled();
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
				["kubernetes-overview"], ["kubernetes-sync-health"], ["kubernetes-graph"], ["kubernetes-namespaces"], ["kubernetes-workloads"], ["kubernetes-nodes"], ["kubernetes-issues"], ["kubernetes-top"], ["kubernetes-diagnosis"], ["kubernetes-drawer-changes"], ["kubernetes-changes"],
			].map((parts) => ["kubernetes", "_", ...parts]));
			queries.forEach((query) => expect(query).toHaveBeenCalledTimes(index + 2));
			expect(api.kubernetesOverviewGraph).not.toHaveBeenCalled();
		}
		vi.useRealTimers();
		fireEvent.click(screen.getByRole("button", { name: "Refresh Kubernetes data" }));
		await waitFor(() => expect(api.kubernetesOverview).toHaveBeenCalledTimes(4));
		expect(api.kubernetesOverviewGraph).not.toHaveBeenCalled();
		expect(api.kubernetesGraph).not.toHaveBeenCalled();
	});

	it("uses per-kind index records without inventing absent counts or materializing resources", async () => {
		renderPage();
		const topology = await screen.findByRole("region", { name: "Overview topology" });
		await waitFor(() => expect(api.kubernetesStream).toHaveBeenCalledOnce());
		const kinds = {
			Pod: { state: "ready", records: 50000, partial: false },
			Service: { state: "ready", records: 0, partial: false },
			Deployment: { state: "partial", records: 12, partial: true },
			ConfigMap: { state: "stale", records: 9, partial: false },
			Secret: { state: "error", records: 0, partial: true, error: "forbidden" },
			CustomResource: { state: "ready", records: 23, partial: false },
		};
		act(() => vi.mocked(api.kubernetesStream).mock.calls[0][0]({ event: "sync", data: JSON.stringify({ state: "partial", age_s: 0, partial: true, kinds }) }));
		const count = (kind: string) => within(topology).queryByText(kind, { exact: true })?.closest("dt")?.parentElement?.querySelector("dd")?.textContent;
		expect(count("Pod")).toBe((50000).toLocaleString());
		expect(count("Service")).toBe("0");
		expect(count("Deployment")).toBe("12");
		expect(count("ConfigMap")).toBe("9");
		expect(count("Secret")).toBeUndefined();
		expect(count("Ingress")).toBeUndefined();
		expect(count("CustomResource")).toBe("23");
		expect(count("Workloads (aggregate)")).toBe("7");
		expect(within(topology).queryByRole("img", { name: "Kubernetes relationship schematic" })).toBeNull();
		expect(within(topology).getByLabelText("Reported resource counts").textContent).toContain("Service");
		expect(within(topology).getByText(/\+\d+ kinds not reported/)).toBeTruthy();
		expect(api.kubernetesOverviewGraph).not.toHaveBeenCalled();
		expect(api.kubernetesGraph).not.toHaveBeenCalled();
		expect(api.kubernetesNamespaces).not.toHaveBeenCalled();
		expect(api.kubernetesWorkloads).toHaveBeenCalledOnce();
		expect(api.kubernetesWorkloads).toHaveBeenCalledWith(expect.objectContaining({ limit: 20 }));
	});

	it("keeps workloads full width and the sampled timestamp naturally left aligned", async () => {
		vi.mocked(api.kubernetesUsage).mockResolvedValue({ observed_at: "2026-08-30T12:01:00Z", availability: "available", fresh: true, node_metrics: { availability: "available", fresh: true, complete: true, total: 3, cpu: "2", observed_at: "2026-08-30T12:01:00Z" }, truncated: false });
		renderPage();
		const sampled = await screen.findByText(/^Sampled /);
		expect(sampled.classList.contains("ml-auto")).toBe(false);
		expect(sampled.parentElement?.classList.contains("flex-wrap")).toBe(true);
		fireEvent.click(screen.getByRole("tab", { name: "Workloads" }));
		expect(screen.getByRole("region", { name: "Workloads" }).parentElement?.className).toBe("min-w-0 w-full");
	});

	it("hides cached overview counts when a refresh loses access", async () => {
		renderPage();
		const topology = await screen.findByRole("region", { name: "Overview topology" });
		vi.mocked(api.kubernetesOverview).mockRejectedValue(new ApiError(403, "forbidden"));
		fireEvent.click(screen.getByRole("button", { name: "Refresh Kubernetes data" }));
		await screen.findByRole("alert");
		expect([...topology.querySelectorAll("dd")].every((value) => value.textContent === "0")).toBe(true);
		expect(within(topology).queryByLabelText("Reported resource counts")).toBeNull();
		expect(api.kubernetesOverviewGraph).not.toHaveBeenCalled();
	});

	it.each([401, 403])("hides cached workload totals, kind counts, and disclosures after HTTP %s", async (status) => {
		const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [{ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "shop", name: "private-api" }], counts: { Deployment: 137 }, truncated: true, partial_failures: [{ resource_id: "apps~v1~statefulsets", class: "forbidden" }] });
		renderWorkloads(client);
		const inventory = await screen.findByRole("region", { name: "Workloads" });
		await waitFor(() => expect(client.isFetching()).toBe(0));
		expect(within(inventory).getByText("137 total")).toBeTruthy();
		vi.mocked(api.kubernetesWorkloads).mockRejectedValueOnce(new ApiError(status, "Access denied"));
		await act(async () => { await client.invalidateQueries({ queryKey: ["kubernetes", "_", "kubernetes-workloads"] }); });
		expect(await within(inventory).findByText("Inventory unavailable")).toBeTruthy();
		expect(within(inventory).queryByText("137 total")).toBeNull();
		expect(within(inventory).queryByText("137", { exact: true })).toBeNull();
		expect(within(inventory).queryByText("private-api")).toBeNull();
		expect(within(inventory).queryByText(/Some evidence could not be collected/)).toBeNull();
	});

	 it.each([401, 403])("hides the cached change tile count after HTTP %s", async (status) => {
		const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
		vi.mocked(api.kubernetesChanges).mockResolvedValue({ items: [{ id: "private-change", cluster: "production", kind: "Deployment", namespace: "shop", name: "private-api", uid: "private-api", type: "created", at: new Date().toISOString() }], gaps: [], sync: { state: "ready", age_s: 0, partial: false } });
		renderPage(client);
		const card = await screen.findByRole("region", { name: "Break timeline" });
		await waitFor(() => expect(client.isFetching()).toBe(0));
		const tile = within(card).getByRole("button", { name: /Changes in 60 min/ });
		expect(tile.textContent).toContain("1");
		vi.mocked(api.kubernetesChanges).mockRejectedValueOnce(new ApiError(status, "Access denied"));
		await act(async () => { await client.invalidateQueries({ queryKey: ["kubernetes", "_", "kubernetes-changes"] }); });
		await waitFor(() => expect(tile.textContent).toContain("Unknown"));
		expect(tile.textContent).not.toContain("1");
	});

	it("uses grouped eye filters, four default kinds, and resets them for each namespace", async () => {
		vi.mocked(api.kubernetesNamespaces).mockResolvedValue({ items: ["shop", "platform"], sync: { state: "ready", age_s: 0, partial: false } });
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
		await screen.findByRole("img", { name: /Change activity in twelve/ });
		const queries = [api.kubernetesIssues, api.kubernetesTop, api.kubernetesReleases, api.kubernetesTraffic, api.kubernetesChanges];
		const initialCalls = queries.map((query) => vi.mocked(query).mock.calls.length);
		fireEvent.click(screen.getByRole("button", { name: "Refresh Kubernetes data" }));
		await waitFor(() => queries.forEach((query, index) => expect(vi.mocked(query).mock.calls.length).toBeGreaterThan(initialCalls[index])));
	});

	 it("places Timeline before capacity without paired heights and retains rolling counts", async () => {
		const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
		const intervals = vi.spyOn(window, "setInterval");
		const previous = { items: [{ id: "recent", cluster: "production", kind: "Pod", namespace: "shop", name: "recent-api", uid: "recent-api", type: "created" as const, at: new Date().toISOString() }], gaps: [], sync: { state: "ready", age_s: 0, partial: false } };
		vi.mocked(api.kubernetesChanges).mockResolvedValueOnce(previous);
		renderPage(client);
		await screen.findByRole("region", { name: "Break timeline" });
		await waitFor(() => expect(client.isFetching()).toBe(0));
		const card = screen.getByRole("region", { name: "Break timeline" });
		const topologyCard = screen.getByRole("region", { name: "Overview topology" }).querySelector("article")!;
		expect(card.className).not.toContain("h-[26rem]");
		expect(topologyCard.className).not.toContain("h-[26rem]");
		expect(card.compareDocumentPosition(screen.getByRole("heading", { name: "CPU capacity" })) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
		let completeRefresh!: (page: Awaited<ReturnType<typeof api.kubernetesChanges>>) => void;
		vi.mocked(api.kubernetesChanges).mockImplementationOnce(() => new Promise((resolve) => { completeRefresh = resolve; }));
		const refreshWindow = intervals.mock.calls.find(([callback, delay]) => delay === 30_000 && String(callback).includes("refetchChanges"))?.[0];
		intervals.mockRestore();
		if (typeof refreshWindow !== "function") throw new Error("Change activity must register a 30-second refresh interval");
		await act(async () => { refreshWindow(); });
		await waitFor(() => expect(api.kubernetesChanges).toHaveBeenCalledTimes(2));
		const options = vi.mocked(api.kubernetesChanges).mock.calls[1][0]!;
		expect(Date.parse(options.until!) - Date.parse(options.since!)).toBe(60 * 60_000);
		expect(within(card).getByRole("button", { name: /Changes in 60 min/ }).textContent).toContain("1");
		expect(within(card).queryByRole("status", { name: "Loading change history" })).toBeNull();
		expect(client.getQueriesData({ queryKey: ["kubernetes", "_", "kubernetes-changes", "overview"] })).toHaveLength(1);
		await act(async () => {
			completeRefresh({ ...previous, items: [previous.items[0], { ...previous.items[0], id: "updated", name: "updated-api" }] });
		});
		await waitFor(() => expect(within(card).getByRole("button", { name: /Changes in 60 min/ }).textContent).toContain("2"));
	});

	it("keeps loaded Overview sections visible throughout a manual refresh", async () => {
		const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
		renderPage(client);
		await screen.findByRole("region", { name: "Cluster health" });
		await waitFor(() => expect(client.isFetching()).toBe(0));
		const topology = screen.getByRole("region", { name: "Overview topology" });
		const history = screen.getByRole("region", { name: "Break timeline" });
		const queries = [api.kubernetesOverview, api.kubernetesUsage, api.kubernetesWorkloads, api.kubernetesNodes, api.kubernetesIssues, api.kubernetesTop, api.kubernetesChanges, api.kubernetesReleases, api.kubernetesTraffic];
		for (const query of queries) vi.mocked(query).mockImplementationOnce(() => new Promise(() => {}));
		fireEvent.click(screen.getByRole("button", { name: "Refresh Kubernetes data" }));
		await waitFor(() => expect(client.isFetching()).toBeGreaterThan(0));
		expect(screen.getByRole("region", { name: "Overview topology" })).toBe(topology);
		expect(screen.getByRole("region", { name: "Break timeline" })).toBe(history);
		expect(screen.queryByRole("region", { name: "Nodes" })).toBeNull();
		expect(screen.queryByLabelText("Loading Kubernetes overview")).toBeNull();
		for (const label of ["Loading change history", "Loading issue summary", "Loading usage ranking", "Loading release summary", "Loading traffic summary"]) expect(screen.queryByRole("status", { name: label })).toBeNull();
	});

	it("retains loaded Overview evidence after transient refresh failures", async () => {
		const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [{ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "shop", name: "cached-api" }], counts: { Deployment: 1 }, truncated: false });
		renderPage(client);
		await screen.findByRole("region", { name: "Cluster health" });
		await waitFor(() => expect(client.isFetching()).toBe(0));
		const queries = [api.kubernetesOverview, api.kubernetesUsage, api.kubernetesWorkloads, api.kubernetesNodes, api.kubernetesIssues, api.kubernetesTop, api.kubernetesChanges, api.kubernetesReleases, api.kubernetesTraffic];
		for (const query of queries) vi.mocked(query).mockRejectedValueOnce(new ApiError(503, "Temporarily unavailable"));
		fireEvent.click(screen.getByRole("button", { name: "Refresh Kubernetes data" }));
		await waitFor(() => expect(client.isFetching()).toBe(0));
		expect(screen.getByRole("region", { name: "Cluster health" })).toBeTruthy();
		fireEvent.click(screen.getByRole("tab", { name: "Workloads" }));
		expect(within(screen.getByRole("region", { name: "Workloads" })).getByText("cached-api")).toBeTruthy();
		fireEvent.click(screen.getByRole("tab", { name: "Nodes" }));
		expect(within(screen.getByRole("region", { name: "Nodes" })).getByText("node-a")).toBeTruthy();
		expect(screen.queryByRole("alert")).toBeNull();
		expect(screen.queryByLabelText("Loading Kubernetes overview")).toBeNull();
		expect(screen.queryByText("Recent change history unavailable.")).toBeNull();
	});

	it.each([0, 3])("does not infer cluster health from %s ready nodes and no warnings", async (nodes) => {
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ connector: "kubernetes", cluster_id: "production", observed_at: "2026-08-30T12:00:00Z", nodes, ready_nodes: nodes, pods: 4, running_pods: 2, namespaces: 2, active_namespaces: 2, workloads: 2, warnings: 0, metrics_fresh: false, truncated: false });
		renderPage();
		const health = await screen.findByRole("region", { name: "Cluster health" });
		expect(within(health).queryByText("Healthy")).toBeNull();
		expect(within(health).getByText("Sync unknown")).toBeTruthy();
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
		const health = within(screen.getByRole("region", { name: "Cluster health" }));
		expect(health.queryByText("Partial visibility")).toBeNull();
		expect(screen.queryByRole("button", { name: "Resource coverage" })).toBeNull();
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
		expect(await screen.findByText("Usage snapshot unavailable; using overview snapshot")).toBeTruthy();
	});

	it("opens overview card destinations without bounded-activity warnings", async () => {
		vi.mocked(api.kubernetesChanges).mockResolvedValue({ items: [], next: "next-page", sync: { state: "ready", age_s: 0, partial: true } });
		renderPage();
		await screen.findByRole("img", { name: /Change activity in twelve/ });
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
		expect(await screen.findByRole("button", { name: "Open resource checkout-a" })).toBeTruthy();
		expect(screen.queryByText("Issue evidence is partial; the server returned bounded results.")).toBeNull();
		fireEvent.click(screen.getByRole("button", { name: "Next page" }));
		expect(await screen.findByRole("button", { name: "Open resource checkout-b" })).toBeTruthy();
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
		vi.mocked(api.kubernetesNamespaces).mockResolvedValue({ items: ["platform", "shop"], sync: { state: "ready", age_s: 0, partial: false } });
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

	it("lists unreadable workload kinds in a quiet coverage disclosure", async () => {
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [], counts: {}, truncated: true, partial_failures: [{ resource_id: "batch~v1~cronjobs", class: "forbidden" }] });
		renderWorkloads();
		const workloads = await screen.findByRole("region", { name: "Workloads" });
		const disclosure = await within(workloads).findByRole("button", { name: "Resource coverage" });
		expect(disclosure.getAttribute("aria-expanded")).toBe("false");
		fireEvent.click(disclosure);
		expect(screen.getByText("cronjobs: permission denied")).toBeTruthy();
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
		vi.mocked(api.kubernetesPodLogStream).mockImplementation(async (_namespace, _pod, options, emit) => {
			emit({ event: options.previous ? "error" : "end", code: "previous_unavailable", text: "", container: "app" });
		});
		renderWorkloads();
		fireEvent.click(await screen.findByRole("button", { name: "Select Pod default/api" }));
		fireEvent.click(screen.getByRole("tab", { name: "Logs" }));
		fireEvent.click(await screen.findByLabelText("Previous container logs"));
		expect(await screen.findByText(/Previous container logs are unavailable because no previous container instance exists\./)).toBeTruthy();
		expect(screen.queryByText("Couldn't render this page")).toBeNull();
	});

	it("opens grouped issue roots in the shared resource drawer", async () => {
		vi.mocked(api.kubernetesIssues).mockResolvedValue({ items: [{ root: { kind: "Deployment", namespace: "payments", name: "checkout", api_version: "apps/v1" }, rule: "workload.unavailable", severity: "critical", count: 2, examples: [{ kind: "Pod", namespace: "payments", name: "checkout-a" }] }], totals: { critical: 1 }, truncated: false, sync: { state: "direct", age_s: 0, partial: false } });
		renderPage();
		fireEvent.click(screen.getByRole("tab", { name: "Issues" }));
		fireEvent.click(await screen.findByRole("button", { name: "Open resource checkout" }));
		const drawer = await screen.findByRole("dialog", { name: "Details panel" });
		expect(within(drawer).getByRole("heading", { name: "Deployment payments/checkout" })).toBeTruthy();
		expect(new URLSearchParams(window.location.search).get("r")).toBe("apps~v1~deployments/payments/checkout");
	});

	it.each([401, 403])("hides cached Issues roots and counts after HTTP %s", async (status) => {
		window.history.replaceState(null, "", "/agent/kubernetes?view=issues");
		const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
		vi.mocked(api.kubernetesIssues).mockResolvedValue({ items: [{ root: { kind: "Deployment", namespace: "shop", name: "private-root", uid: "private-uid" }, rule: "workload.unavailable", severity: "critical", count: 2 }], totals: { critical: 8 }, truncated: false, sync: { state: "ready", age_s: 0, partial: false } });
		renderPage(client);
		expect(await screen.findByRole("button", { name: "Open resource private-root" })).toBeTruthy();
		await waitFor(() => expect(client.isFetching()).toBe(0));
		vi.mocked(api.kubernetesIssues).mockRejectedValueOnce(new ApiError(status, "Access denied"));
		await act(async () => { await client.invalidateQueries({ queryKey: ["kubernetes", "_", "kubernetes-issues"] }); });
		expect(await screen.findByText("Grouped issues are unavailable.")).toBeTruthy();
		expect(screen.queryByRole("button", { name: "Open resource private-root" })).toBeNull();
		expect(screen.queryByLabelText("critical count")).toBeNull();
	});

	it.each(["Issues", "Overview"] as const)("keeps long issue details on a full-width mobile track in %s", async (view) => {
		const name = "checkout-api-with-a-long-unbroken-resource-name-for-mobile-triage";
		const example = `${name}-replica-pod`;
		const lastSeen = "2026-10-06T07:06:30Z";
		vi.mocked(api.kubernetesIssues).mockResolvedValue({ items: [{ root: { kind: "Deployment", namespace: "payments", name, api_version: "apps/v1" }, rule: "workload.unavailable", severity: "critical", count: 2, last_seen: lastSeen, examples: [{ kind: "Pod", namespace: "payments", name: example }] }], totals: { critical: 1 }, truncated: false, sync: { state: "direct", age_s: 0, partial: false } });
		renderPage();
		fireEvent.click(screen.getByRole("tab", { name: view }));
		const title = await screen.findByRole("button", { name: `Open resource ${name}` });
		const row = title.closest("article")!;
		// A long subject stays on one line and ellipsizes instead of breaking
		// between letters; the full name stays available as a tooltip.
		expect(title.className).toContain("truncate");
		expect(title.className).not.toContain("break-all");
		expect(title.getAttribute("title")).toBe(`payments/${name}`);
		expect(row.className).toContain("min-w-0");
		expect(within(row).getByText("Pods unavailable")).toBeTruthy();
		expect(within(row).getByText("2 affected")).toBeTruthy();
		if (view === "Issues") {
		 expect(within(row).getByRole("button", { name: `Expand ${name}` }).getAttribute("aria-expanded")).toBe("false");
		 expect(within(row).queryByText(`Examples: ${example}`)).toBeNull();
		} else {
		 expect(row.tagName).toBe("ARTICLE");
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

	it("offers six resource views and safe projected YAML without a diagnosis tab", async () => {
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [{ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout" }], truncated: false });
		vi.mocked(api.kubernetesDescribe).mockResolvedValue({ resource: { resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout", summary: { ready: 2, data: "secret-canary", nested: { token: "secret-canary", safe: "projected" } } } });
		vi.mocked(api.kubernetesDiagnose).mockResolvedValue({ workload: { resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout", ready: 2, desired: 3, truncated: false }, warning_events: [], changes: [{ id: "change-diagnosis", cluster: "production", kind: "Deployment", namespace: "default", name: "checkout", uid: "uid-checkout", type: "image_changed", at: "2026-10-05T12:00:00Z" }], neighborhood: { nodes: [{ id: "uid-checkout", kind: "Deployment", namespace: "default", name: "checkout", group: "default" }, { id: "uid-service", kind: "Service", namespace: "default", name: "checkout", group: "default" }], edges: [{ from: "uid-service", to: "uid-checkout", type: "exposes" }], omitted: {}, sync: { state: "ready", age_s: 0, partial: false } }, truncated: false, sync: { state: "direct", age_s: 0, partial: false } });
		renderWorkloads();
		fireEvent.click(await screen.findByRole("button", { name: "Select Deployment default/checkout" }));
		expect(screen.getByRole("tab", { name: "Logs" })).toBeTruthy();
		expect(screen.queryByRole("tab", { name: "Actions" })).toBeNull();
		expect(api.kubernetesWorkloadLogs).not.toHaveBeenCalled();
		fireEvent.click(screen.getByRole("tab", { name: "YAML" }));
		const yamlRegion = await screen.findByRole("region", { name: "Projected YAML" });
		fireEvent.click(screen.getByRole("button", { name: "Unfold all" }));
		const yaml = yamlRegion.textContent!;
		expect(yaml).toContain("name: checkout");
		expect(yaml).toContain("safe: projected");
		expect(yaml).not.toContain("secret-canary");
		const tabs = screen.getByRole("tablist", { name: "Resource detail views" });
		expect(within(tabs).getAllByRole("tab").map((tab) => tab.textContent)).toEqual(["Overview", "Logs", "Events", "YAML", "Related", "Timeline"]);
		expect(within(tabs).queryByRole("tab", { name: "Diagnosis" })).toBeNull();
		expect(api.kubernetesDiagnose).not.toHaveBeenCalled();
	});

	it("renders projected timeline changes and gaps", async () => {
		const change = { id: "change-1", cluster: "production", kind: "Deployment", namespace: "payments", name: "checkout", uid: "uid-1", type: "image_changed" as const, fields: [{ path: "containers.images", from: "checkout:v1", to: "checkout:v2" }], at: new Date(Date.now() - 60_000).toISOString() };
		vi.mocked(api.kubernetesTimeline).mockResolvedValue({ items: [{ type: "change", id: change.id, at: change.at, change }], total: 1, crash_count: 0, counts: { change: 1, crash: 0, break: 0 }, since: "2026-10-05T11:00:00Z", until: change.at, gaps: [{ from: "2026-10-05T11:00:00Z", to: "2026-10-05T11:05:00Z" }], sync: { state: "ready", age_s: 0, partial: false } });
		renderPage();
		fireEvent.click(screen.getByRole("tab", { name: "Timeline" }));
		const marker = await screen.findByRole("button", { name: "Open change details" });
		fireEvent.click(screen.getByRole("button", { name: "Resource coverage" }));
		expect(screen.getByText("History capture does not cover the whole range")).toBeTruthy();
		fireEvent.click(marker);
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

	it("keeps readable topology visible with one quiet permission marker", async () => {
		vi.mocked(api.kubernetesGraph).mockResolvedValue({ nodes: [{ id: "pod", kind: "Pod", namespace: "shop", name: "api", group: "shop" }], edges: [], omitted: {}, partial_failures: [{ resource_id: "core~v1~secrets", class: "forbidden" }], sync: { state: "ready", age_s: 0, partial: true } });
		renderPage();
		fireEvent.click(screen.getByRole("tab", { name: "Topology" }));
		await screen.findByRole("option", { name: "shop" });
		fireEvent.change(screen.getByRole("combobox", { name: "Topology namespace" }), { target: { value: "shop" } });
		expect(await screen.findByLabelText("Scrollable topology graph")).toBeTruthy();
		fireEvent.click(screen.getByRole("button", { name: "Resource coverage" }));
		expect(screen.getByText("secrets: permission denied")).toBeTruthy();
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

	it("keeps the six drawer tabs on one accessible rail with keyboard access to Timeline", async () => {
		window.history.replaceState(null, "", "/agent/kubernetes?r=core~v1~pods/default/api");
		renderPage();
		const tabs = await screen.findByRole("tablist", { name: "Resource detail views" });
		expect(tabs.classList.contains("grid-cols-3")).toBe(false);
		expect(tabs.classList.contains("overflow-x-auto")).toBe(true);
		expect(within(tabs).getAllByRole("tab").map((tab) => tab.textContent)).toEqual(["Overview", "Logs", "Events", "YAML", "Related", "Timeline"]);
		fireEvent.keyDown(within(tabs).getByRole("tab", { name: "Overview" }), { key: "End" });
		expect(within(tabs).getByRole("tab", { name: "Timeline", exact: true }).getAttribute("aria-selected")).toBe("true");
	});
	it("offers keyboard-operable secondary workload views and restores focus", async () => {
		window.history.replaceState(null, "", "/agent/kubernetes?r=apps~v1~deployments/default/checkout");
		renderPage();
		const summary = await screen.findByLabelText("More resource tabs");
		fireEvent.click(summary);
		const items = screen.getAllByRole("menuitem");
		items[0].focus();
		fireEvent.keyDown(items[0], { key: "End" });
		expect(document.activeElement).toBe(items[2]);
		fireEvent.keyDown(items[2], { key: "Escape" });
		expect(summary.closest("details")?.open).toBe(false);
		expect(document.activeElement).toBe(summary);
		expect(screen.getByRole("dialog", { name: "Details panel" })).toBeTruthy();
		fireEvent.click(summary);
		fireEvent.click(screen.getByRole("menuitem", { name: "Timeline" }));
		expect(within(screen.getByRole("tablist", { name: "Resource detail views" })).getByRole("tab", { name: "Timeline", exact: true }).getAttribute("aria-selected")).toBe("true");
		expect(summary.closest("details")?.open).toBe(false);
		expect(document.activeElement).toBe(summary);
	});
	it.each(["actions", "metrics", "diagnosis"])("normalizes obsolete non-Pod %s deep links and popstate", async (tab) => {
		window.history.replaceState(null, "", `/agent/kubernetes?r=apps~v1~deployments/default/checkout&tab=${tab}`);
		renderPage();
		const tabs = await screen.findByRole("tablist", { name: "Resource detail views" });
		expect(within(tabs).getByRole("tab", { name: "Overview" }).getAttribute("aria-selected")).toBe("true");
		expect(within(tabs).getByRole("tab", { name: "Logs" })).toBeTruthy();
		expect(within(tabs).queryByRole("tab", { name: "Actions" })).toBeNull();
		await waitFor(() => expect(new URLSearchParams(window.location.search).has("tab")).toBe(false));
		fireEvent.keyDown(within(tabs).getByRole("tab", { name: "Overview" }), { key: "ArrowRight" });
		expect(within(tabs).getByRole("tab", { name: "Logs" }).getAttribute("aria-selected")).toBe("true");
		act(() => { window.history.pushState(null, "", `/agent/kubernetes?r=apps~v1~deployments/default/checkout&tab=${tab}`); window.dispatchEvent(new PopStateEvent("popstate")); });
		expect(within(tabs).getByRole("tab", { name: "Overview" }).getAttribute("aria-selected")).toBe("true");
		expect(new URLSearchParams(window.location.search).has("tab")).toBe(false);
		expect(api.proposeAgentAction).not.toHaveBeenCalled();
		expect(api.kubernetesWorkloadLogs).not.toHaveBeenCalled();
	});

	it("shares Pod log sessions across Overview and Logs and aborts on drawer close", async () => {
		window.history.replaceState(null, "", "/agent/kubernetes?r=core~v1~pods/default/api&tab=logs");
		vi.mocked(api.kubernetesPodLogStream).mockImplementation((_namespace, _pod, _options, emit, signal) => {
			emit({ event: "line", text: "password=[REDACTED]", container: "app", cursor: "one" });
			return new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
		});
		renderPage();
		const output = await screen.findByLabelText("Pod log output");
		await waitFor(() => expect(output.textContent).toContain("password=[REDACTED]"));
		expect(screen.queryByRole("tab", { name: "Actions" })).toBeNull();
		const first = vi.mocked(api.kubernetesPodLogStream).mock.calls[0][4];
		fireEvent.click(within(screen.getByRole("tablist", { name: "Resource detail views" })).getByRole("tab", { name: "Overview" }));
		expect(first.aborted).toBe(false);
		fireEvent.click(screen.getByRole("tab", { name: "Logs" }));
		expect(vi.mocked(api.kubernetesPodLogStream)).toHaveBeenCalledTimes(1);
		fireEvent.click(screen.getByRole("button", { name: "Close panel" }));
		expect(first.aborted).toBe(true);
		expect(api.kubernetesPodLogs).not.toHaveBeenCalled();
	});

	it("opens shared chat with the selected Kubernetes resource attached", async () => {
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [{ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout", uid: "checkout-uid" }], truncated: false });
		renderWorkloads();
		fireEvent.click(await screen.findByRole("button", { name: "Select Deployment default/checkout" }));
		fireEvent.click(screen.getByRole("button", { name: "Investigate" }));
		const location = await screen.findByLabelText("Current location");
		const search = new URLSearchParams(location.textContent?.split("?")[1]);
		expect(location.textContent).toContain("/agent/chat?");
		expect(search.get("provider")).toBe("kubernetes");
		expect(search.get("cluster")).toBe("production");
		expect(search.get("resource_id")).toBe("apps~v1~deployments");
		expect(search.get("name")).toBe("checkout");
		expect(search.get("uid")).toBe("checkout-uid");
		expect(search.get("include_changes")).toBe("true");
	});

	it("requests a bounded server page for the Workloads inventory", async () => {
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: Array.from({ length: 20 }, (_, index) => ({ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: `service-${index + 1}` })), truncated: false });
		renderWorkloads();
		const inventory = await screen.findByRole("region", { name: "Workloads" });
		expect(await within(inventory).findAllByRole("button", { name: /Select Deployment/ })).toHaveLength(20);
		expect(api.kubernetesWorkloads).toHaveBeenCalledWith(expect.objectContaining({ limit: 20 }));
	});

	it("renders health, partial visibility, and cluster-scoped nodes", async () => {
		const view = renderPage();
		expect(screen.getByLabelText("Loading Kubernetes overview")).toBeTruthy();
		await screen.findByText(/source unavailable/);
		expect(screen.queryByText("Warnings", { exact: true })).toBeNull();
		expect(screen.queryByText("Nodes count unavailable")).toBeNull();
		expect(screen.getByRole("region", { name: "Cluster health" }).querySelector("article")).toBeNull();
		const warningButton = screen.getByRole("button", { name: "Resource coverage" });
		fireEvent.click(warningButton);
		expect(screen.getByRole("region", { name: "Resource coverage details" }).textContent).toContain("nodes: permission denied");
		expect(view.container.querySelector("main")?.className).toContain("overflow-x-hidden");
		expect(screen.queryByText(/kubernetes - production/i)).toBeNull();
		expect(screen.queryByLabelText("Namespace")).toBeNull();
		fireEvent.click(screen.getByRole("tab", { name: "Nodes" }));
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
		renderWorkloads();
		await screen.findByText("checkout-api");
		fireEvent.change(screen.getByLabelText("Resource name"), { target: { value: "nightly" } });
		expect(await screen.findByText("nightly-cleanup")).toBeTruthy();
		expect(screen.queryByText("checkout-api")).toBeNull();
		await waitFor(() => expect(api.kubernetesWorkloads).toHaveBeenCalledWith(expect.objectContaining({ q: "nightly", limit: 20 })));
		fireEvent.focus(screen.getByLabelText("Workload namespace"));
		fireEvent.click(await screen.findByRole("option", { name: "payments" }));
		expect(await screen.findByText("No matching workloads.")).toBeTruthy();
		expect(api.kubernetesWorkloads).toHaveBeenLastCalledWith(expect.objectContaining({ namespace: "payments", q: "nightly", limit: 20 }));
		expect(api.kubernetesSearch).not.toHaveBeenCalled();
	});

	it("always renders All and all six workload kinds with server counts", async () => {
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [], counts: { Deployment: 3, StatefulSet: 2, DaemonSet: 1, Job: 4, CronJob: 5, Pod: 6 }, truncated: false });
		renderWorkloads();
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
		renderWorkloads();
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
		await screen.findByRole("region", { name: "Cluster health" });
		expect(screen.queryByText("Pods count unavailable")).toBeNull();
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
		await screen.findByRole("region", { name: "Cluster health" });
		expect(screen.queryByText("Nodes count unavailable")).toBeNull();
		expect(within(capacityCell("CPU capacity", "Requested")).getByText("1 core")).toBeTruthy();
		expect(within(capacityCell("CPU capacity", "Allocatable")).getByText("Needs node read access (list nodes)")).toBeTruthy();
		expect(within(capacityCell("CPU capacity", "Usage")).getByText("500 mCPU")).toBeTruthy();
		expect(within(capacityCell("Memory capacity", "Requested")).getByText("1 GiB")).toBeTruthy();
		expect(within(capacityCell("Memory capacity", "Allocatable")).getByText("Needs node read access (list nodes)")).toBeTruthy();
		expect(within(capacityCell("Memory capacity", "Usage")).getByText("512 MiB")).toBeTruthy();
	});

	it("shows the workload count as unavailable when any workload category is omitted", async () => {
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ connector: "kubernetes", cluster_id: "production", observed_at: "2026-08-30T12:00:00Z", nodes: 3, ready_nodes: 3, pods: 4, running_pods: 4, namespaces: 2, active_namespaces: 2, workloads: 6, warnings: 0, usage_source: "unavailable", metrics_status: "unavailable", metrics_fresh: false, truncated: true, omitted_categories: ["batch~v1~cronjobs"] });
		renderPage();
		await screen.findByRole("region", { name: "Overview topology" });
		expect(screen.getByText(/\+\d+ kinds not reported/)).toBeTruthy();
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
		renderWorkloads();
		await screen.findByText("deployment-1");
		const workloads = screen.getByRole("region", { name: "Workloads" });
		fireEvent.click(within(workloads).getByRole("button", { name: "Next page" }));
		expect(await within(workloads).findByText("nightly")).toBeTruthy();
		fireEvent.click(within(workloads).getByRole("tab", { name: "Deployment" }));
		expect(await within(workloads).findByText("deployment-1")).toBeTruthy();
		fireEvent.click(within(workloads).getByRole("tab", { name: "Job" }));
		expect(await within(workloads).findByText("nightly")).toBeTruthy();
		// One page has nowhere to go, so the pager is gone rather than disabled.
		expect(within(workloads).queryByRole("button", { name: "Next page" })).toBeNull();
		expect(within(workloads).queryByRole("navigation", { name: "Page navigation" })).toBeNull();
		expect(api.kubernetesWorkloads).toHaveBeenCalledWith(expect.objectContaining({ cursor: "workload-next", limit: 20 }));
		expect(api.kubernetesWorkloads).toHaveBeenLastCalledWith(expect.objectContaining({ kind: "Job", cursor: undefined, limit: 20 }));
	});

	it("fetches only the current server page for workloads and nodes without querying warning events", async () => {
		const workloadItems = Array.from({ length: 20 }, (_, index) => ({ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: `workload-${index + 1}`, summary: { readyReplicas: 1, replicas: 1 } }));
		const nodeItems = Array.from({ length: 20 }, (_, index) => ({ resource_id: "core~v1~nodes", kind: "Node", name: `node-${String(index + 1).padStart(2, "0")}`, conditions: [{ type: "Ready", status: "True" }] }));
		vi.mocked(api.kubernetesWorkloads).mockImplementation(async (options = {}) => options.cursor ? { items: [{ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "workload-21" }], counts: { Deployment: 21 }, truncated: false } : { items: workloadItems, counts: { Deployment: 21 }, next: "workload-next", truncated: false });
		vi.mocked(api.kubernetesNodes).mockImplementation(async (cursor) => cursor ? { items: [{ resource_id: "core~v1~nodes", kind: "Node", name: "node-21" }], truncated: false } : { items: nodeItems, continue: "nodes-next", truncated: true });
		renderWorkloads();
		await screen.findByText("workload-20");
		const workloads = screen.getByRole("region", { name: "Workloads" });
		expect(within(workloads).getByText("workload-20")).toBeTruthy();
		expect(within(workloads).queryByText("workload-21")).toBeNull();
		fireEvent.click(within(workloads).getByRole("button", { name: "Next page" }));
		expect(await within(workloads).findByText("workload-21")).toBeTruthy();
		fireEvent.click(screen.getByRole("tab", { name: "Nodes" }));
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
		fireEvent.click(screen.getByRole("tab", { name: "Nodes" }));
		const nodes = await screen.findByRole("region", { name: "Nodes" });
		fireEvent.click(await within(nodes).findByRole("button", { name: "View pods on node-a" }));
		await waitFor(() => expect(api.kubernetesNodePods).toHaveBeenCalledWith("node-a", undefined));
		expect((await within(nodes).findAllByText("platform")).length).toBeGreaterThan(0);
		expect(within(nodes).getAllByText("payments").length).toBeGreaterThan(0);
		expect(within(nodes).queryByText("pod-21")).toBeNull();
		expect(within(nodes).queryByText(/Scheduled pod inventory is partial/)).toBeNull();
		fireEvent.click(within(nodes).getByRole("button", { name: "Next page" }));
		expect(await within(nodes).findByText("pod-21")).toBeTruthy();
		fireEvent.click(within(nodes).getByRole("button", { name: "All nodes" }));
		expect(within(nodes).getByRole("button", { name: "View pods on node-a" })).toBeTruthy();
		fireEvent.click(screen.getByRole("tab", { name: "Workloads" }));
		fireEvent.focus(screen.getByLabelText("Workload namespace"));
		fireEvent.click(await screen.findByRole("option", { name: "All namespaces" }));
		expect(api.kubernetesUsage).toHaveBeenCalledTimes(1);
		expect(api.kubernetesNodes).toHaveBeenCalledTimes(1);
	});

	it("keeps object-scoped event details available in the resource drawer", async () => {
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [{ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "payments", name: "checkout" }], truncated: false });
		vi.mocked(api.kubernetesDescribe).mockResolvedValue({ resource: { resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "payments", name: "checkout" }, events: [{ resource_id: "core~v1~events", kind: "Event", namespace: "payments", name: "mount-warning", summary: { reason: "FailedMount", message: "Unable to attach the projected volume.", count: 4, lastTimestamp: "2026-08-30T12:00:00Z" } }] });
		renderWorkloads();
		fireEvent.click(await screen.findByRole("button", { name: "Select Deployment payments/checkout" }));
		const drawer = await screen.findByRole("dialog", { name: "Details panel" });
		fireEvent.click(within(drawer).getByRole("tab", { name: "Events" }));
		expect(await within(drawer).findByRole("region", { name: "Object-scoped events" })).toBeTruthy();
		expect(await screen.findByText("Unable to attach the projected volume.")).toBeTruthy();
		expect(within(drawer).getByText("×4", { selector: ".pill" })).toBeTruthy();
		expect(api.kubernetesEvents).not.toHaveBeenCalled();
	});

	it("shows normalized current workload metric tiles with aggregate configured limits", async () => {
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [{ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout", conditions: [{ type: "Ready", status: "True" }] }], truncated: false });
		vi.mocked(api.kubernetesDescribe).mockResolvedValue({ resource: { resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout", summary: { desired: 1, ready: 1, updateStrategy: "RollingUpdate", serviceType: "ClusterIP" } } });
		vi.mocked(api.kubernetesWorkload).mockResolvedValue({ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout", desired: 1, ready: 1, update_strategy: "RollingUpdate", containers: [{ name: "app", limits: { cpu: "750m", memory: "768Mi" } }, { name: "sidecar", limits: { cpu: "250m", memory: "256Mi" } }], usage: [{ kind: "Pod", namespace: "default", name: "checkout-abc", cpu: "1/2", memory: "1073741824", timestamp: "2026-08-30T12:00:00Z" }], pods: [{ name: "checkout-abc", phase: "Running", node: "node-a", restart_count: 2 }], truncated: false });
		renderWorkloads();
		await screen.findByText("checkout");
		fireEvent.click(screen.getByRole("button", { name: "Select Deployment default/checkout" }));
		const metrics = await screen.findByRole("region", { name: "Live workload metrics" });
		expect(await within(metrics).findByLabelText("500 mCPU")).toBeTruthy();
		expect(within(metrics).getByText("1.00 GiB", { selector: "p" })).toBeTruthy();
		expect(within(metrics).getByText("50%")).toBeTruthy();
		expect(within(metrics).getByText("100%")).toBeTruthy();
		expect(within(metrics).getByText("1.00 GiB", { selector: "p" }).className).toContain("text-sev-critical");
		expect(within(metrics).getByLabelText("Restarts metric tile").querySelector("p")?.textContent).toMatch(/^2/);
	});

	it("does not invent a limit or percent when container allocations are incomplete", async () => {
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ connector: "kubernetes", cluster_id: "production", observed_at: "2026-08-30T12:00:00Z", nodes: 1, ready_nodes: 1, pods: 1, running_pods: 1, namespaces: 1, active_namespaces: 1, workloads: 1, warnings: 0, usage_source: "metrics_api", metrics_status: "available", metrics_fresh: true, truncated: false });
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [{ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout" }], truncated: false });
		vi.mocked(api.kubernetesDescribe).mockResolvedValue({ resource: { resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout" } });
		vi.mocked(api.kubernetesWorkload).mockResolvedValue({ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout", containers: [{ name: "app", limits: { cpu: "1" } }, { name: "sidecar" }], usage: [{ kind: "Pod", namespace: "default", name: "checkout-abc", cpu: "250m", memory: "256Mi" }], truncated: false });
		renderWorkloads();
		await screen.findByText("checkout");
		fireEvent.click(screen.getByRole("button", { name: "Select Deployment default/checkout" }));
		const metrics = await screen.findByRole("region", { name: "Live workload metrics" });
		expect(await within(metrics).findByLabelText("250 mCPU")).toBeTruthy();
		expect(within(metrics).getAllByText("of Unavailable")).toHaveLength(2);
		expect(within(metrics).queryByText(/%/)).toBeNull();
	});

	it("shows an explicit metrics unavailable state for a selected workload without samples", async () => {
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [{ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout" }], truncated: false });
		vi.mocked(api.kubernetesDescribe).mockResolvedValue({ resource: { resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout" } });
		vi.mocked(api.kubernetesWorkload).mockResolvedValue({ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout", truncated: false });
		renderWorkloads();
		await screen.findByText("checkout");
		fireEvent.click(screen.getByRole("button", { name: "Select Deployment default/checkout" }));
		expect(await screen.findByText("Metrics unavailable. Waiting for a live sample.")).toBeTruthy();
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
		renderWorkloads();
		await vi.waitFor(() => expect(screen.getByText("checkout")).toBeTruthy());
		fireEvent.click(screen.getByRole("button", { name: "Select Deployment default/checkout" }));
		await vi.waitFor(() => expect(api.kubernetesWorkload).toHaveBeenCalledTimes(1));
		await vi.waitFor(() => expect(screen.getByText("since opened")).toBeTruthy());
		expect(screen.queryByText(/polling every 15 seconds/)).toBeNull();
		await act(async () => { await vi.advanceTimersByTimeAsync(15_000); });
		await vi.waitFor(() => expect(api.kubernetesUsage).toHaveBeenCalledTimes(2));
		await act(async () => { await vi.advanceTimersByTimeAsync(15_000); });
		await vi.waitFor(() => expect(api.kubernetesUsage).toHaveBeenCalledTimes(3));
		expect(api.kubernetesWorkload).toHaveBeenCalledTimes(1);
		const cpuChart = screen.getByRole("img", { name: /Workload CPU and memory collected since/ });
		fireEvent.click(screen.getByRole("button", { name: "Inspect CPU history" }));
		const memoryChart = cpuChart;
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
		expect(screen.getByRole("img", { name: /Workload CPU and memory collected since/ })).toBeTruthy();
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
		renderWorkloads();
		await vi.waitFor(() => expect(screen.getByText("checkout")).toBeTruthy());
		fireEvent.click(screen.getByRole("button", { name: "Select Deployment default/checkout" }));
		await vi.waitFor(() => expect(screen.getByText("since opened")).toBeTruthy());
		await act(async () => { await vi.advanceTimersByTimeAsync(15_000); });
		await vi.waitFor(() => expect(api.kubernetesUsage).toHaveBeenCalledTimes(2));
		expect(screen.getByRole("img", { name: /Workload CPU and memory collected/ }).querySelector('[data-series="cpu"]')?.getAttribute("d")).not.toContain("L");
		expect(screen.getAllByText("Usage samples (1)")).toHaveLength(2);
	});

	it("renders finite metric paths for samples received in the same millisecond", async () => {
		vi.useFakeTimers();
		vi.setSystemTime("2026-08-30T12:00:00Z");
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [{ resource_id: "core~v1~pods", kind: "Pod", namespace: "default", name: "api" }], truncated: false });
		vi.mocked(api.kubernetesUsage)
			.mockResolvedValueOnce({ observed_at: "2026-08-30T12:00:00Z", availability: "available", fresh: true, pods: [{ kind: "Pod", namespace: "default", name: "api", cpu: "100m", memory: "128Mi" }], truncated: false })
			.mockResolvedValue({ observed_at: "2026-08-30T12:00:01Z", availability: "available", fresh: true, pods: [{ kind: "Pod", namespace: "default", name: "api", cpu: "200m", memory: "256Mi" }], truncated: false });
		renderWorkloads();
		await vi.waitFor(() => expect(screen.getByText("api")).toBeTruthy());
		fireEvent.click(screen.getByRole("button", { name: "Select Pod default/api" }));
		await vi.waitFor(() => expect(screen.getByText("since opened")).toBeTruthy());
		await act(async () => { await vi.advanceTimersByTimeAsync(15_000); });
		await vi.waitFor(() => expect(screen.getByRole("img", { name: /Workload CPU and memory collected since/ })).toBeTruthy());
		const chart = screen.getByRole("img", { name: /Workload CPU and memory collected since/ });
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
		expect(await screen.findByRole("button", { name: "Resource coverage" })).toBeTruthy();
		fireEvent.click(screen.getByRole("tab", { name: "Workloads" }));
		expect(screen.getByText("No workloads in this scope.")).toBeTruthy();
		expect(screen.queryByRole("region", { name: "Recent warnings" })).toBeNull();
		fireEvent.click(screen.getByRole("tab", { name: "Nodes" }));
		expect(screen.getByText("No nodes in this cluster.")).toBeTruthy();
		fireEvent.click(screen.getByRole("button", { name: "Resource coverage" }));
		expect(screen.getByText("Kubernetes resources: permission denied")).toBeTruthy();
		expect(api.kubernetesEvents).not.toHaveBeenCalled();
		expect(screen.queryByText("Couldn't render this page")).toBeNull();
	});

	it("keeps every explorer tab renderable when the top-usage list is unavailable", async () => {
		vi.mocked(api.kubernetesTop).mockResolvedValue({ items: [], items_available: false, total: 0, truncated: false, availability: "unavailable", fresh: false, sync: { state: "direct", age_s: 0, partial: true } });
		renderPage();
		expect(await screen.findByText("Usage ranking unavailable; no list was returned.")).toBeTruthy();

		for (const label of ["Overview", "Issues", "Workloads", "Nodes", "Timeline", "Topology", "Helm", "GitOps", "Traffic"]) {
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
			action: "Configure a credential source for connectors.kubernetes.auth.mode and restart Versus.",
			retryable: false,
		}));
		renderPage();
		expect(await screen.findByText("Kubernetes credentials are unavailable.")).toBeTruthy();
		expect(screen.getByText("Configure a credential source for connectors.kubernetes.auth.mode and restart Versus.")).toBeTruthy();
	});

	it.each(["overview", "timeline", "issues"])("shows the Preparing panel instead of a skeleton on the %s tab during the first sync", async (view) => {
		window.history.replaceState(null, "", `/agent/kubernetes?view=${view}`);
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ sync: { state: "syncing", age_s: 0, partial: false, progress: { step: "listing", step_index: 2, step_total: 4, steps_done: ["connected"], counts: { Pod: 3214, Deployment: 412, Namespace: 27 }, kinds_done: 3, kinds_total: 8, cluster_version: "v1.30.2", node_count: 12, started_at: "2026-10-23T10:00:00Z", updated_at: "2026-10-23T10:00:04Z" } }, connector: "kubernetes", cluster_id: "production", observed_at: "", nodes: 0, ready_nodes: 0, pods: 0, running_pods: 0, namespaces: 0, active_namespaces: 0, workloads: 0, warnings: 0 });
		// The cluster-wide report has no published generation yet, so every kind
		// is still to be listed.
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue({ state: "syncing", partial: false, age_s: 0, kinds: {}, total_kinds: 0, ready_kinds: 0, failing_kinds: 0, pending_kinds: 0, truncated: false, progress: (await api.kubernetesOverview()).sync?.progress });
		renderPage();
		const panel = await screen.findByRole("region", { name: "Preparing your cluster" });
		const countsLine = panel.querySelector<HTMLElement>("[data-first-sync-counts]")!;
		expect([...countsLine.querySelectorAll("[data-first-sync-count]")].map((chip) => chip.textContent)).toEqual(["Pods 3,214", "Deployments 412", "Namespaces 27"]);
		expect(countsLine.textContent).toBe("Pods 3,214Deployments 412Namespaces 27");
		expect(countsLine.textContent).not.toMatch(/[.·•]/);
		expect(screen.queryByLabelText("Loading Kubernetes overview")).toBeNull();
		expect(screen.getByText("Preparing")).toBeTruthy();
		expect(screen.queryByText("Syncing cluster")).toBeNull();
		expect(screen.queryByText("Connecting")).toBeNull();
		expect(api.kubernetesTimeline).not.toHaveBeenCalled();
		expect(api.kubernetesIssues).not.toHaveBeenCalled();
	});

	it("drops the Preparing panel once the first generation is ready", async () => {
		const ready = { connector: "kubernetes", cluster_id: "production", observed_at: "2026-08-30T12:00:00Z", nodes: 3, ready_nodes: 3, pods: 4, running_pods: 4, namespaces: 2, active_namespaces: 2, workloads: 2, warnings: 0 };
		const progress = { step: "listing" as const, step_index: 2, step_total: 4, steps_done: ["connected"], counts: { Pod: 4 }, kinds_done: 1, kinds_total: 4, started_at: "2026-10-23T10:00:00Z", updated_at: "2026-10-23T10:00:04Z" };
		vi.mocked(api.kubernetesOverview).mockResolvedValueOnce({ ...ready, sync: { state: "syncing", age_s: 0, partial: false, progress } }).mockResolvedValue({ ...ready, sync: { state: "ready", age_s: 1, partial: false } });
		vi.mocked(api.kubernetesSyncHealth)
			.mockResolvedValueOnce({ state: "syncing", partial: false, age_s: 0, kinds: {}, total_kinds: 0, ready_kinds: 0, failing_kinds: 0, pending_kinds: 0, truncated: false })
			.mockResolvedValue({ state: "ready", partial: false, age_s: 1, kinds: { Pod: { state: "ready", observed_at: "2026-08-30T12:00:00Z", records: 4, partial: false } }, total_kinds: 1, ready_kinds: 1, failing_kinds: 0, pending_kinds: 0, truncated: false });
		renderPage();
		expect(await screen.findByRole("region", { name: "Preparing your cluster" })).toBeTruthy();
		await waitFor(() => expect(screen.queryByRole("region", { name: "Preparing your cluster" })).toBeNull(), { timeout: 3000 });
		expect(await screen.findByRole("region", { name: "Cluster health" })).toBeTruthy();
	});

	it.each(["9181/1000", "9181m", "9181000u", "9181000000n"])("preserves over-capacity percentages with CPU usage %s and rational server quantities", async (cpuUsage) => {
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ connector: "kubernetes", cluster_id: "production", observed_at: "2026-08-30T12:00:00Z", nodes: 1, ready_nodes: 1, pods: 4, running_pods: 4, namespaces: 2, active_namespaces: 2, workloads: 2, warnings: 0, requested_cpu: "18669/200", allocatable_cpu: "196/25", requested_memory: `${31393 * 1024 ** 3}/100`, allocatable_memory: "28.73Gi", truncated: false, sync: { state: "ready", age_s: 0, partial: false } });
		vi.mocked(api.kubernetesUsage).mockResolvedValue({ observed_at: "2026-08-30T12:01:00Z", availability: "available", fresh: true, node_metrics: { availability: "available", fresh: true, complete: true, total: 1, cpu: cpuUsage, memory: `${20055 * 1024 ** 3}/100`, observed_at: "2026-08-30T12:01:00Z" }, pod_metrics: { availability: "unavailable", fresh: false, complete: false, total: 0 }, pods: [], nodes: [], truncated: false });
		renderPage();
		await screen.findByRole("meter", { name: "CPU in use" });
		for (const [name, percent] of [["CPU in use", 117], ["CPU reserved by requests", 1191], ["Memory in use", 698], ["Memory reserved by requests", 1093]] as const) {
			const meter = screen.getByRole("meter", { name });
			const shown = `${percent.toLocaleString()}% of allocatable`;
			expect(meter.getAttribute("aria-valuetext")).toBe(shown);
			expect(screen.getByText(shown)).toBeTruthy();
		}
		expect(within(capacityCell("CPU capacity", "Allocatable")).getByText("7.84 cores")).toBeTruthy();
		expect(within(capacityCell("Memory capacity", "Allocatable")).getByText("28.73 GiB")).toBeTruthy();
	});

	it("shows In use and Reserved percentages once nodes report allocatable capacity", async () => {
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ connector: "kubernetes", cluster_id: "production", observed_at: "2026-08-30T12:00:00Z", nodes: 12, ready_nodes: 12, pods: 300, running_pods: 300, namespaces: 27, active_namespaces: 27, workloads: 412, warnings: 0, requested_cpu: "91300m", limited_cpu: "140500m", allocatable_cpu: "120", requested_memory: "310.9Gi", limited_memory: "402Gi", allocatable_memory: "480Gi", truncated: false, sync: { state: "ready", age_s: 0, partial: false } });
		vi.mocked(api.kubernetesUsage).mockResolvedValue({ observed_at: "2026-08-30T12:01:00Z", availability: "available", fresh: true, node_metrics: { availability: "available", fresh: true, complete: true, total: 12, cpu: "13200m", memory: "197.7Gi", observed_at: "2026-08-30T12:01:00Z" }, pod_metrics: { availability: "unavailable", fresh: false, complete: false, total: 0 }, pods: [], nodes: [], truncated: false });
		renderPage();
		const cpu = await screen.findByRole("meter", { name: "CPU in use" });
		expect(cpu.getAttribute("aria-valuenow")).toBe("11");
		expect(screen.getByRole("meter", { name: "CPU reserved by requests" }).getAttribute("aria-valuenow")).toBe("76");
		expect(screen.getByRole("meter", { name: "Memory in use" }).getAttribute("aria-valuenow")).toBe("41");
		expect(screen.getByRole("meter", { name: "Memory reserved by requests" }).getAttribute("aria-valuenow")).toBe("65");
		expect(within(capacityCell("CPU capacity", "Allocatable")).getByText("120 cores")).toBeTruthy();
		expect(within(capacityCell("Memory capacity", "Allocatable")).getByText("480 GiB")).toBeTruthy();
		expect(screen.queryByText("Needs allocatable capacity")).toBeNull();
	});

	it.each(["forbidden", "unauthorized"])("names missing node read access (%s) instead of missing allocatable capacity", async (failureClass) => {
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ connector: "kubernetes", cluster_id: "production", observed_at: "2026-08-30T12:00:00Z", nodes: 0, ready_nodes: 0, pods: 300, running_pods: 300, namespaces: 27, active_namespaces: 27, workloads: 412, warnings: 0, requested_cpu: "91300m", requested_memory: "310.9Gi", usage_cpu: "13200m", truncated: false, partial_failures: [{ resource_id: "core~v1~nodes", class: failureClass }], sync: { state: "ready", age_s: 0, partial: false } });
		renderPage();
		const panel = (await screen.findByRole("heading", { name: "CPU capacity" })).closest("section")!;
		expect(within(capacityCell("CPU capacity", "Allocatable")).getByText("Needs node read access (list nodes)")).toBeTruthy();
		expect(within(capacityCell("Memory capacity", "Allocatable")).getByText("Needs node read access (list nodes)")).toBeTruthy();
		expect(within(panel).getAllByText("Needs node read access (list nodes)")).toHaveLength(3);
		expect(within(capacityCell("CPU capacity", "Requested")).getByText("91.3 cores")).toBeTruthy();
		expect(screen.queryByText("Needs allocatable capacity")).toBeNull();
	});

	it("keeps the allocatable wording when nodes were listed without allocatable capacity", async () => {
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ connector: "kubernetes", cluster_id: "production", observed_at: "2026-08-30T12:00:00Z", nodes: 3, ready_nodes: 3, pods: 4, running_pods: 4, namespaces: 2, active_namespaces: 2, workloads: 2, warnings: 0, requested_cpu: "2", requested_memory: "2Gi", truncated: false, sync: { state: "ready", age_s: 0, partial: false } });
		renderPage();
		await screen.findByRole("heading", { name: "CPU capacity" });
		expect(screen.getAllByText("Needs allocatable capacity").length).toBeGreaterThanOrEqual(2);
		expect(screen.queryByText("Needs node read access (list nodes)")).toBeNull();
	});

	it.each(["overview", "timeline"])("keeps the four preparation rows until the complete report publishes on the %s tab", async (view) => {
		window.history.replaceState(null, "", `/agent/kubernetes?view=${view}`);
		const core = { connector: "kubernetes", cluster_id: "production", observed_at: "2026-08-30T12:00:00Z", nodes: 3, ready_nodes: 3, pods: 4, running_pods: 4, namespaces: 2, active_namespaces: 2, workloads: 2, warnings: 0 };
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ ...core, sync: { state: "ready", age_s: 0, partial: false, pending_kinds: 7 } });
		// The core kinds are listed; seven more are still on their way.
		const listed = { state: "ready", partial: false, age_s: 0, kinds: { Pod: { state: "ready", observed_at: "2026-08-30T12:00:00Z", records: 4, partial: false } }, total_kinds: 8, ready_kinds: 1, failing_kinds: 0, pending_kinds: 7, truncated: false };
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue(listed);
		const { container } = renderPage();
		await screen.findByRole("region", { name: "Preparing your cluster" });
		expect(container.querySelectorAll("[data-step]")).toHaveLength(4);
		expect(container.querySelectorAll("[role=progressbar]")).toHaveLength(1);
		expect(container.querySelectorAll("[data-pending-note]")).toHaveLength(0);
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ ...core, sync: { state: "ready", age_s: 0, partial: false } });
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue({ ...listed, total_kinds: 1, pending_kinds: 0 });
		await browserClick(screen.getByRole("button", { name: "Refresh Kubernetes data" }));
		await waitFor(() => expect(container.querySelector("[data-preparing-panel]")).toBeNull(), { timeout: 7000 });
	}, 10_000);

	it("shows a pending topology kind as loading, never as zero", async () => {
		vi.mocked(api.kubernetesNamespaces).mockResolvedValue({ items: ["shop"], sync: { state: "ready", age_s: 0, partial: false } });
		vi.mocked(api.kubernetesGraph).mockResolvedValue({ nodes: ["Service", "Deployment", "Pod"].map((kind) => ({ id: kind, kind, namespace: "shop", name: kind.toLowerCase(), group: "shop" })), edges: [], omitted: {}, sync: { state: "partial", age_s: 0, partial: true, pending: ["core~v1~configmaps", "networking.k8s.io~v1~ingresses"] } });
		renderPage();
		fireEvent.click(screen.getByRole("tab", { name: "Topology" }));
		fireEvent.click(await screen.findByRole("button", { name: "Open namespace shop topology" }));
		await screen.findByLabelText("Scrollable topology graph");
		const filters = screen.getByRole("complementary", { name: "Topology filters" });
		for (const kind of ["ConfigMap", "Ingress"]) {
			const row = within(filters).getByText(kind, { selector: "span" }).closest("li")!;
			expect(within(row).getByLabelText(`${kind} loading`).textContent).toBe("loading");
			expect(within(row).queryByText("0")).toBeNull();
		}
		expect(within(within(filters).getByText("Secret", { selector: "span" }).closest("li")!).getByText("0")).toBeTruthy();
		expect(within(within(filters).getByText("Service", { selector: "span" }).closest("li")!).getByText("1")).toBeTruthy();
		expect(screen.queryByText("Relationships are still loading.")).toBeNull();
	});

	it("says relationships are still loading instead of none when only pending kinds would connect them", async () => {
		vi.mocked(api.kubernetesNamespaces).mockResolvedValue({ items: ["shop"], sync: { state: "ready", age_s: 0, partial: false } });
		vi.mocked(api.kubernetesGraph).mockResolvedValue({ nodes: [], edges: [], omitted: {}, sync: { state: "partial", age_s: 0, partial: true, pending: ["core~v1~configmaps"] } });
		renderPage();
		fireEvent.click(screen.getByRole("tab", { name: "Topology" }));
		fireEvent.click(await screen.findByRole("button", { name: "Open namespace shop topology" }));
		expect(await screen.findByText("Relationships are still loading.")).toBeTruthy();
		expect(screen.queryByText("No connected resources were returned for this namespace.")).toBeNull();
	});

	it("blocks the overview and shows how old the snapshot is when a stale sync reports pending kinds", async () => {
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ connector: "kubernetes", cluster_id: "production", observed_at: "2026-08-30T12:00:00Z", nodes: 3, ready_nodes: 3, pods: 4, running_pods: 4, namespaces: 2, active_namespaces: 2, workloads: 2, warnings: 0, sync: { state: "stale", age_s: 420, partial: true, pending: ["core~v1~configmaps"], pending_kinds: 3 } });
		// A snapshot this old stays blocked even while other kinds are loading.
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue({ state: "stale", partial: true, age_s: 420, kinds: { Pod: { state: "ready", observed_at: "2026-08-30T12:00:00Z", records: 4, partial: false } }, total_kinds: 4, ready_kinds: 1, failing_kinds: 0, pending_kinds: 3, truncated: false });
		const { container } = renderPage();
		const block = await waitFor(() => {
			const found = container.querySelector('[data-sync-health="degraded"]');
			expect(found).toBeTruthy();
			return found!;
		});
		expect(block.textContent).toContain("The last cluster snapshot is 7 minutes old, so cluster data stays hidden until a fresh sync lands.");
		expect(screen.queryByText(/Loading \d+ more resource types?/)).toBeNull();
		expect(screen.queryByRole("region", { name: "Cluster health" })).toBeNull();
		expect(screen.queryByRole("heading", { name: "CPU capacity" })).toBeNull();
		expect(container.querySelectorAll("[data-sync-health]")).toHaveLength(1);
	});

	it("keeps the Timeline tab and view=timeline when Breaks only is chosen after a tab-bar switch", async () => {
		renderPage();
		await screen.findByRole("region", { name: "Cluster health" });
		fireEvent.click(screen.getByRole("tab", { name: "Timeline", exact: true }));
		const control = await screen.findByRole("tablist", { name: "Timeline events" });
		fireEvent.click(within(control).getByRole("tab", { name: "Breaks only" }));
		await waitFor(() => expect(new URLSearchParams(window.location.search).get("breaks")).toBe("1"));
		const query = new URLSearchParams(window.location.search);
		expect(query.get("view")).toBe("timeline");
		expect(screen.getByLabelText("Current location").textContent).toContain("view=timeline");
		expect(screen.getByRole("tab", { name: "Timeline", exact: true }).getAttribute("aria-selected")).toBe("true");
		expect(within(screen.getByRole("tablist", { name: "Timeline events" })).getByRole("tab", { name: "Breaks only" }).getAttribute("aria-selected")).toBe("true");
		await waitFor(() => expect(vi.mocked(api.kubernetesTimeline).mock.calls.at(-1)?.[0].breaks).toBe(1));
		act(() => { window.history.back(); });
		await waitFor(() => expect(screen.getByRole("tab", { name: "Overview", exact: true }).getAttribute("aria-selected")).toBe("true"));
		expect(new URLSearchParams(window.location.search).get("view")).toBeNull();
	});

	it.each([["logs", "Logs"], ["events", "Events"], ["yaml", "YAML"], ["related", "Related"], ["timeline", "Timeline"]])("closes the drawer from the %s tab in one URL write and Back reopens it there", async (tab, label) => {
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [{ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout", uid: "checkout-uid" }], truncated: false });
		renderWorkloads();
		await browserClick(await screen.findByRole("button", { name: "Select Deployment default/checkout" }));
		const drawer = await screen.findByRole("dialog", { name: "Details panel" });
		await browserClick(within(within(drawer).getByRole("tablist", { name: "Resource detail views" })).getByRole("tab", { name: label }));
		await waitFor(() => expect(new URLSearchParams(window.location.search).get("tab")).toBe(tab));
		const before = window.history.length;
		await browserClick(screen.getByRole("button", { name: "Close panel" }));
		await settle();
		const closed = new URLSearchParams(window.location.search);
		expect(closed.has("r")).toBe(false);
		expect(closed.has("tab")).toBe(false);
		expect(closed.get("view")).toBe("resources");
		expect(screen.queryByRole("dialog", { name: "Details panel" })).toBeNull();
		expect(window.history.length).toBe(before + 1);
		await settle();
		expect(screen.queryByRole("dialog", { name: "Details panel" })).toBeNull();
		expect(new URLSearchParams(window.location.search).has("r")).toBe(false);
		act(() => { window.history.back(); });
		const reopened = await screen.findByRole("dialog", { name: "Details panel" });
		const query = new URLSearchParams(window.location.search);
		expect(query.get("r")).toBe("apps~v1~deployments/default/checkout");
		expect(query.get("tab")).toBe(tab);
		expect(within(within(reopened).getByRole("tablist", { name: "Resource detail views" })).getByRole("tab", { name: label }).getAttribute("aria-selected")).toBe("true");
	});

	it("keeps view=timeline when Breaks only follows a tab-bar switch with browser-ordered clicks", async () => {
		renderPage();
		await screen.findByRole("region", { name: "Cluster health" });
		await browserClick(screen.getByRole("tab", { name: "Timeline", exact: true }));
		const control = await screen.findByRole("tablist", { name: "Timeline events" });
		await browserClick(within(control).getByRole("tab", { name: "Breaks only" }));
		await settle();
		const query = new URLSearchParams(window.location.search);
		expect(query.get("view")).toBe("timeline");
		expect(query.get("breaks")).toBe("1");
		expect(screen.getByRole("tab", { name: "Timeline", exact: true }).getAttribute("aria-selected")).toBe("true");
	});

	it("opens the focused Timeline from an Issues row Timeline button", async () => {
		vi.mocked(api.kubernetesIssues).mockResolvedValue({ items: [{ root: { kind: "Deployment", namespace: "shop", name: "checkout", uid: "checkout-uid" }, rule: "pod.crash_loop", severity: "critical", count: 1, broken: true, broke_at: "2026-10-23T07:30:00Z" }], totals: {}, truncated: false, sync: { state: "ready", age_s: 0, partial: false } });
		window.history.replaceState(null, "", "/agent/kubernetes?view=issues");
		renderPage();
		await browserClick(await screen.findByRole("button", { name: "Timeline for checkout" }));
		await settle();
		const query = new URLSearchParams(window.location.search);
		expect(Object.fromEntries(query)).toMatchObject({ view: "timeline", root_uid: "checkout-uid", namespace: "shop", center: "2026-10-23T07:30:00Z" });
		expect(screen.getByRole("tab", { name: "Timeline", exact: true }).getAttribute("aria-selected")).toBe("true");
		expect(await screen.findByRole("link", { name: "Clear root filter" })).toBeTruthy();
	});

	it("clears an obsolete drawer tab only from the URL it reads and keeps the resource open", async () => {
		window.history.replaceState(null, "", "/agent/kubernetes?view=resources&r=apps~v1~deployments/default/checkout&tab=actions");
		renderPage();
		expect(await screen.findByRole("dialog", { name: "Details panel" })).toBeTruthy();
		await waitFor(() => expect(new URLSearchParams(window.location.search).has("tab")).toBe(false));
		expect(new URLSearchParams(window.location.search).get("r")).toBe("apps~v1~deployments/default/checkout");
	});
});

describe("KubernetesPage sync gate", () => {
	const healthyOverview = { connector: "kubernetes", cluster_id: "production", observed_at: "2026-08-30T12:00:00Z", nodes: 3, ready_nodes: 3, pods: 4, running_pods: 4, namespaces: 2, active_namespaces: 2, workloads: 2, warnings: 0 };
	const readyKind = { state: "ready" as const, observed_at: "2026-10-26T09:12:00Z", records: 12, partial: false };
	// A report of the whole index: every kind the cluster serves is listed here,
	// so what the gates read is what the cluster-wide route returns.
	const report = (state: string, kinds: Record<string, unknown>, counts: { failing?: number; pending?: number } = {}) => ({
		state, partial: state !== "ready" && state !== "syncing", age_s: 0,
		kinds: kinds as Record<string, never>,
		total_kinds: Object.keys(kinds).length,
		ready_kinds: Object.values(kinds).filter((kind) => (kind as { state?: string }).state === "ready").length,
		failing_kinds: counts.failing ?? 0,
		pending_kinds: counts.pending ?? 0,
		truncated: false,
	});
	// A blocked cluster is set up the way the gate receives it: one aggregate
	// state plus the per-kind rows the surface names.
	function blockWith(state: string, kinds: Record<string, unknown>, counts?: { failing?: number; pending?: number }) {
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue(report(state, kinds, counts));
	}

	it.each(["overview", "resources", "issues", "timeline", "topology", "nodes", "releases", "gitops", "traffic"])("blocks the %s view behind the sync health surface", async (view) => {
		window.history.replaceState(null, "", `/agent/kubernetes${view === "overview" ? "" : `?view=${view}`}`);
		vi.mocked(api.kubernetesOverview).mockResolvedValue(healthyOverview);
		blockWith("error", { Node: { state: "error", observed_at: "0001-01-01T00:00:00Z", last_attempt_at: "2026-10-26T09:12:03Z", records: 0, partial: true, error: "forbidden" } }, { failing: 1 });
		const { container } = renderPage();
		await screen.findByRole("heading", { name: "Cluster sync failed" });
		expect(container.querySelectorAll("[data-sync-health]")).toHaveLength(1);
		expect(container.querySelector('[data-sync-health="failed"]')).toBeTruthy();
		// No cluster data anywhere: no counts, charts or graph for this view.
		for (const name of ["Cluster health", "Kubernetes issues", "Kubernetes change timeline", "Kubernetes traffic flows", "Kubernetes resources", "Nodes"]) expect(screen.queryByRole("region", { name })).toBeNull();
		expect(screen.queryByRole("heading", { name: "CPU capacity" })).toBeNull();
		expect(screen.queryByLabelText("Scrollable topology graph")).toBeNull();
		for (const query of [api.kubernetesWorkloads, api.kubernetesNodes, api.kubernetesIssues, api.kubernetesTimeline, api.kubernetesGraph, api.kubernetesTraffic, api.kubernetesUsage, api.kubernetesReleases, api.kubernetesGitOpsApps]) expect(vi.mocked(query)).not.toHaveBeenCalled();
	});

	// Published failures never take data away from an operator mid-incident.
	const published = "2026-10-26T08:00:00Z";

	it.each(["partial", "stale", "error"])("keeps every view open and reports %s as a strip after the first publication", async (state) => {
		vi.mocked(api.kubernetesOverview).mockResolvedValue(healthyOverview);
		const kinds = state === "error"
			? { Node: { state: "error", observed_at: "0001-01-01T00:00:00Z", last_attempt_at: "2026-10-26T09:12:03Z", records: 0, partial: true, error: "forbidden" } }
			: state === "partial"
				? { Node: { ...readyKind, partial: true, error: "record_rejected", skipped: 2 }, Pod: readyKind }
				: { Node: readyKind, Pod: readyKind };
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue({
			...report(state, kinds, { failing: state === "error" || state === "partial" ? 1 : 0 }),
			// Two minutes of data on screen: the strip has to say so.
			age_s: 120,
			first_ready_at: published,
		});
		const { container } = renderPage();
		const strip = await waitFor(() => {
			const found = container.querySelector("[data-sync-strip]");
			expect(found).toBeTruthy();
			return found!;
		});
		// The blocking surfaces never come back, and the views are open.
		expect(container.querySelector("[data-sync-health]")).toBeNull();
		expect(screen.queryByRole("region", { name: "Preparing your cluster" })).toBeNull();
		expect(await screen.findByRole("region", { name: "Cluster health" })).toBeTruthy();
		expect(screen.queryByRole("heading", { name: "CPU capacity" })).toBeTruthy();
		// The strip names the state and says how old what is on screen is.
		expect(strip.textContent).toMatch(/Cluster sync failed|Cluster sync degraded|Cluster sync running/);
		expect(strip.querySelector("[data-sync-age]")?.textContent).toContain("Showing data from 2 minutes ago.");
		expect(within(strip as HTMLElement).getByRole("button", { name: "Retry sync" })).toBeTruthy();
	});

	it("names a revoked credential as a failure in the strip instead of hiding it", async () => {
		vi.mocked(api.kubernetesOverview).mockResolvedValue(healthyOverview);
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue({
			...report("error", { Secret: { state: "error", observed_at: "0001-01-01T00:00:00Z", last_attempt_at: "2026-10-26T09:12:03Z", records: 0, partial: true, error: "forbidden" } }, { failing: 1 }),
			first_ready_at: published,
		});
		const { container } = renderPage();
		const strip = await waitFor(() => {
			const found = container.querySelector("[data-sync-strip]");
			expect(found).toBeTruthy();
			return found!;
		});
		expect(strip.getAttribute("data-sync-strip")).toBe("failed");
		expect(strip.textContent).toContain("Cluster sync failed");
		expect(strip.querySelector('[data-sync-problem="Secret"]')?.textContent).toContain("permission denied");
		expect(container.querySelector('[data-sync-state="failed"]')?.textContent).toBe("Failed");
		// The data is still readable behind the strip.
		expect(await screen.findByRole("region", { name: "Cluster health" })).toBeTruthy();
	});

	it("shows data and only the existing top-bar indicator when reloaded mid-resync", async () => {
		// A reload is a new mount: the very first report already carries the
		// durable first-publication time.
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ ...healthyOverview, sync: { state: "syncing", age_s: 30, partial: false } });
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue({
			...report("syncing", { Node: readyKind, Pod: { ...readyKind, state: "pending", observed_at: "0001-01-01T00:00:00Z" } }, { pending: 1 }),
			first_ready_at: published,
		});
		const { container } = renderPage();
		expect(await screen.findByRole("region", { name: "Cluster health" })).toBeTruthy();
		await waitFor(() => expect(container.querySelector('[data-sync-state="preparing"]')?.textContent).toBe("Syncing"));
		expect(container.querySelector("[data-sync-strip]")).toBeNull();
		expect(screen.queryByRole("region", { name: "Preparing your cluster" })).toBeNull();
	});

	it("does not fabricate Preparing while the authoritative report is pending on reload", async () => {
		vi.mocked(api.kubernetesOverview).mockResolvedValue(healthyOverview);
		let resolve!: (value: ReturnType<typeof report> & { first_ready_at: string }) => void;
		vi.mocked(api.kubernetesSyncHealth).mockReturnValue(new Promise(done => { resolve = done; }));
		const { container } = renderPage();
		await screen.findByRole("status", { name: "Loading cluster sync status" });
		expect(container.querySelector("[data-preparing-panel], [data-sync-strip]")).toBeNull();
		expect(container.querySelector("[data-sync-state]")?.textContent).toBe("Loading");
		await act(async () => { resolve({ ...report("syncing", { Node: readyKind }), first_ready_at: published }); });
		expect(await screen.findByRole("region", { name: "Cluster health" })).toBeTruthy();
		expect(container.querySelector("[data-preparing-panel], [data-sync-strip]")).toBeNull();
		expect(container.querySelector("[data-sync-state]")?.textContent).toBe("Syncing");
	});

	it("refetches a warming snapshot when a published cluster becomes ready after restart", async () => {
		window.history.replaceState(null, "", "/agent/kubernetes?view=resources");
		vi.mocked(api.kubernetesOverview).mockResolvedValue(healthyOverview);
		const warming = { ...report("syncing", { Pod: { ...readyKind, state: "pending" } }), first_ready_at: published };
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue(warming);
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [], truncated: false, sync: { state: "syncing", age_s: 0, partial: false } });
		const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
		const { container } = renderPage(client);
		await screen.findByText("No workloads in this scope.");
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [{ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "shop", name: "restored-api", uid: "restored-uid" }], truncated: false, sync: { state: "ready", age_s: 0, partial: false } });
		const ready = { ...report("ready", { Pod: readyKind }), first_ready_at: published };
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue(ready);
		await act(async () => { client.setQueryData(["kubernetes", "_", "kubernetes-sync-health"], ready); });
		expect(await screen.findByText("restored-api")).toBeTruthy();
		expect(container.querySelector("[data-preparing-panel], [data-sync-strip]")).toBeNull();
	});

	it("settles late warming responses after readiness without overlapping or cross-scope refreshes", async () => {
		window.history.replaceState(null, "", "/agent/kubernetes?view=resources");
		vi.mocked(api.kubernetesOverview).mockResolvedValue(healthyOverview);
		const warming = { ...report("syncing", { Pod: { ...readyKind, state: "pending" } }), first_ready_at: published };
		const ready = { ...report("ready", { Pod: readyKind }), first_ready_at: published };
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue(warming);
		let finish!: (value: Awaited<ReturnType<typeof api.kubernetesWorkloads>>) => void;
		vi.mocked(api.kubernetesWorkloads).mockReturnValueOnce(new Promise(resolve => { finish = resolve; }));
		const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
		const { container } = renderPage(client);
		await waitFor(() => expect(api.kubernetesWorkloads).toHaveBeenCalledTimes(1));
		const empty = { items: [], truncated: false, sync: { state: "syncing" as const, age_s: 0, partial: false } };
		const otherKey = ["kubernetes", "other", "kubernetes-workloads"];
		const inactiveKey = ["kubernetes", "_", "kubernetes-detail", "inactive"];
		client.setQueryData(otherKey, empty);
		client.setQueryData(inactiveKey, empty);
		const otherFetch = vi.fn().mockResolvedValue(empty);
		const otherObserver = new QueryObserver(client, { queryKey: otherKey, queryFn: otherFetch, staleTime: Infinity });
		const stopOther = otherObserver.subscribe(() => {});
		vi.useFakeTimers();
		try {
			vi.mocked(api.kubernetesSyncHealth).mockResolvedValue(ready);
			await act(async () => { client.setQueryData(["kubernetes", "_", "kubernetes-sync-health"], ready); await vi.advanceTimersByTimeAsync(20); });
			// Ready arrives first, while the initial request still has no data.
			await act(async () => { await vi.advanceTimersByTimeAsync(3_000); });
			expect(api.kubernetesWorkloads).toHaveBeenCalledTimes(1);
			let finishRetry!: typeof finish;
			vi.mocked(api.kubernetesWorkloads).mockResolvedValueOnce(empty).mockReturnValueOnce(new Promise(resolve => { finishRetry = resolve; }));
			await act(async () => { finish(empty); await vi.advanceTimersByTimeAsync(20); });
			await act(async () => { await vi.advanceTimersByTimeAsync(1_000); });
			expect(api.kubernetesWorkloads).toHaveBeenCalledTimes(2);
			await act(async () => { await vi.advanceTimersByTimeAsync(1_000); });
			expect(api.kubernetesWorkloads).toHaveBeenCalledTimes(3);
			// A slow retry is not cancelled/restarted on each poll.
			await act(async () => { await vi.advanceTimersByTimeAsync(3_000); });
			expect(api.kubernetesWorkloads).toHaveBeenCalledTimes(3);
			await act(async () => { finishRetry({ ...empty, items: [{ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "shop", name: "late-api", uid: "late-uid" }], sync: { state: "ready", age_s: 0, partial: false } }); await vi.advanceTimersByTimeAsync(20); });
			expect(screen.getByText("late-api")).toBeTruthy();
			await act(async () => { await vi.advanceTimersByTimeAsync(3_000); });
			expect(api.kubernetesWorkloads).toHaveBeenCalledTimes(3);
			expect(client.getQueryState(otherKey)?.isInvalidated).toBe(false);
			expect(otherFetch).not.toHaveBeenCalled();
			expect(client.getQueryState(inactiveKey)?.isInvalidated).toBe(false);
			expect(container.querySelector("[data-preparing-panel], [data-sync-strip]")).toBeNull();
		} finally {
			stopOther();
			vi.useRealTimers();
		}
	});

	it("never reopens Preparing on a per-view refetch after the first publication", async () => {
		window.history.replaceState(null, "", "/agent/kubernetes?view=issues");
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ ...healthyOverview, sync: { state: "syncing", age_s: 0, partial: false } });
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue({ ...report("syncing", { Node: readyKind }), first_ready_at: published });
		// The issues view's own envelope says syncing while its rows are there.
		vi.mocked(api.kubernetesIssues).mockResolvedValue({ items: [{ rule: "CrashLoopBackOff", severity: "critical", root: { kind: "Pod", namespace: "shop", name: "checkout-abc", uid: "pod-uid" }, evidence: { partial: false, warning_events: 0, from: "2026-10-23T07:00:00Z", to: "2026-10-23T08:00:00Z" } }], totals: { critical: 1 }, truncated: false, sync: { state: "syncing", age_s: 0, partial: false } });
		const { container } = renderPage();
		await screen.findByRole("region", { name: "Kubernetes issues" });
		// The rows stay, and the whole-view Preparing panel never renders.
		expect(await screen.findByText("CrashLoopBackOff")).toBeTruthy();
		expect(screen.queryByRole("region", { name: "Preparing your cluster" })).toBeNull();
		expect(container.querySelector("[data-preparing-panel]")).toBeNull();
	});

	it("still blocks a cluster that has never published, whatever a view reports", async () => {
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ ...healthyOverview, sync: { state: "syncing", age_s: 0, partial: false } });
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue(report("syncing", { Node: { state: "pending", observed_at: "0001-01-01T00:00:00Z", last_attempt_at: "0001-01-01T00:00:00Z", records: 0, partial: false } }, { pending: 1 }));
		const { container } = renderPage();
		await waitFor(() => expect(container.querySelector('[data-sync-health="preparing"]')).toBeTruthy());
		expect(screen.queryByRole("region", { name: "Cluster health" })).toBeNull();
		expect(container.querySelector("[data-sync-strip]")).toBeNull();
		expect(vi.mocked(api.kubernetesWorkloads)).not.toHaveBeenCalled();
	});

	it("names a kind that dropped records, with a plain-words reason and a working Retry", async () => {
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ ...healthyOverview, pods: 507, running_pods: 507, workloads: 295 });
		blockWith("partial", {
			Deployment: readyKind,
			Node: { state: "partial", observed_at: "2026-10-26T07:56:00Z", last_attempt_at: "2026-10-26T07:59:00Z", records: 12, partial: true, error: "record_rejected", skipped: 3 },
			Pod: { ...readyKind, records: 507 },
		}, { failing: 1 });
		const { container } = renderPage();
		const block = await waitFor(() => {
			const found = container.querySelector('[data-sync-health="degraded"]');
			expect(found).toBeTruthy();
			return found!;
		});
		expect(block.textContent).toContain("Cluster sync degraded");
		expect(block.textContent).toContain("Some resource types did not finish syncing, so cluster data stays hidden until they do.");
		const problem = block.querySelector('[data-sync-problem="Node"]')!;
		expect(problem.textContent).toContain("some objects could not be indexed, everything else is current");
		expect(problem.textContent).toContain("3 objects skipped");
		expect(problem.textContent).toContain("since");
		expect(block.textContent).not.toMatch(/loading/i);
		expect(screen.queryByRole("region", { name: "Cluster health" })).toBeNull();
		// The top bar reports the same state instead of reading as connectivity.
		expect(container.querySelector('[data-sync-state="degraded"]')?.textContent).toBe("Degraded");
		expect(screen.queryByText("Live")).toBeNull();
		const before = vi.mocked(api.kubernetesSyncHealth).mock.calls.length;
		const overviewBefore = vi.mocked(api.kubernetesOverview).mock.calls.length;
		await browserClick(screen.getByRole("button", { name: "Retry sync" }));
		await waitFor(() => expect(vi.mocked(api.kubernetesSyncHealth).mock.calls.length).toBeGreaterThan(before));
		// Retry asks the authoritative route again, and the view snapshot with it.
		await waitFor(() => expect(vi.mocked(api.kubernetesOverview).mock.calls.length).toBeGreaterThan(overviewBefore));
	});

	it("reports a kind whose listing errored as a failed sync and names the denial", async () => {
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ ...healthyOverview, pods: 507, running_pods: 507, workloads: 295 });
		blockWith("error", {
			Deployment: readyKind,
			Node: { state: "error", observed_at: "0001-01-01T00:00:00Z", completed_at: "0001-01-01T00:00:00Z", last_attempt_at: "2026-10-26T07:59:00Z", records: 0, partial: true, error: "forbidden" },
			Pod: { ...readyKind, records: 507 },
		}, { failing: 1 });
		const { container } = renderPage();
		const block = await waitFor(() => {
			const found = container.querySelector('[data-sync-health="failed"]');
			expect(found).toBeTruthy();
			return found!;
		});
		expect(block.textContent).toContain("Cluster sync failed");
		expect(block.querySelector('[data-sync-problem="Node"]')?.textContent).toContain("permission denied");
		expect(block.textContent).not.toContain("0001");
		expect(container.querySelector('[data-sync-state="failed"]')?.textContent).toBe("Failed");
		expect(screen.queryByRole("region", { name: "Cluster health" })).toBeNull();
	});

	it("keeps the deep link and opens the resource drawer with the sync health surface", async () => {
		window.history.replaceState(null, "", "/agent/kubernetes?view=resources&r=apps~v1~deployments/default/checkout");
		vi.mocked(api.kubernetesOverview).mockResolvedValue(healthyOverview);
		blockWith("error", { Node: { state: "error", observed_at: "0001-01-01T00:00:00Z", last_attempt_at: "2026-10-26T09:12:03Z", records: 0, partial: true, error: "forbidden" } }, { failing: 1 });
		renderPage();
		const drawer = await screen.findByRole("dialog", { name: "Details panel" });
		expect(within(drawer).getByRole("heading", { name: "Cluster sync failed" })).toBeTruthy();
		expect(within(drawer).queryByRole("tablist", { name: "Resource detail views" })).toBeNull();
		expect(new URLSearchParams(window.location.search).get("r")).toBe("apps~v1~deployments/default/checkout");
		expect(vi.mocked(api.kubernetesDescribe)).not.toHaveBeenCalled();
	});

	it("shows the data again and drops the surface once the sync is ready", async () => {
		vi.mocked(api.kubernetesOverview).mockResolvedValue(healthyOverview);
		vi.mocked(api.kubernetesSyncHealth)
			.mockResolvedValueOnce(report("error", { Node: { state: "error", observed_at: "0001-01-01T00:00:00Z", last_attempt_at: "2026-10-26T09:12:03Z", records: 0, partial: true, error: "forbidden" } }, { failing: 1 }))
			.mockResolvedValue(report("ready", { Node: readyKind, Pod: readyKind }));
		const { container } = renderPage();
		await screen.findByRole("heading", { name: "Cluster sync failed" });
		await waitFor(() => expect(container.querySelector("[data-sync-health]")).toBeNull(), { timeout: 7000 });
		expect(await screen.findByRole("region", { name: "Cluster health" })).toBeTruthy();
	}, 10_000);

	// Every view's own envelope only covers the kinds that view asked for, so a
	// failure outside that set used to render data anyway. The cluster-wide
	// report is what closes the gate now.
	it("blocks every view and names a failing kind the per-view envelope never covered", async () => {
		const ready = { connector: "kubernetes", cluster_id: "production", observed_at: "2026-08-30T12:00:00Z", nodes: 3, ready_nodes: 3, pods: 4, running_pods: 4, namespaces: 2, active_namespaces: 2, workloads: 2, warnings: 0 };
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ ...ready, sync: { state: "ready", age_s: 0, partial: false } });
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue({
			state: "error", partial: true, age_s: 0,
			kinds: {
				Deployment: { state: "ready", observed_at: "2026-10-26T09:12:00Z", records: 295, partial: false },
				Node: { state: "ready", observed_at: "2026-10-26T09:12:00Z", records: 12, partial: false },
				Pod: { state: "ready", observed_at: "2026-10-26T09:12:00Z", records: 507, partial: false },
				Secret: { state: "error", observed_at: "0001-01-01T00:00:00Z", completed_at: "0001-01-01T00:00:00Z", last_attempt_at: "2026-10-26T09:12:03Z", records: 0, partial: true, error: "forbidden" },
			},
			total_kinds: 4, ready_kinds: 3, failing_kinds: 1, pending_kinds: 0, truncated: false,
		});
		const { container } = renderPage();
		const block = await waitFor(() => {
			const found = container.querySelector('[data-sync-health="failed"]');
			expect(found).toBeTruthy();
			return found!;
		});
		const problem = block.querySelector('[data-sync-problem="Secret"]');
		expect(problem?.textContent).toContain("permission denied");
		expect(problem?.textContent).toContain("last tried");
		// The ready envelope of the view does not open the gate.
		expect(screen.queryByRole("region", { name: "Cluster health" })).toBeNull();
		for (const query of [api.kubernetesWorkloads, api.kubernetesNodes, api.kubernetesIssues, api.kubernetesUsage]) expect(vi.mocked(query)).not.toHaveBeenCalled();
		expect(container.querySelector('[data-sync-state="failed"]')?.textContent).toBe("Failed");
	});

	it("names a held kind with the class the index recorded instead of claiming no kind failed", async () => {
		const ready = { connector: "kubernetes", cluster_id: "production", observed_at: "2026-08-30T12:00:00Z", nodes: 3, ready_nodes: 3, pods: 4, running_pods: 4, namespaces: 2, active_namespaces: 2, workloads: 2, warnings: 0 };
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ ...ready, sync: { state: "ready", age_s: 0, partial: false } });
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue({
			state: "error", partial: true, age_s: 0,
			kinds: {
				Deployment: { state: "ready", observed_at: "2026-10-26T09:12:00Z", records: 295, partial: false },
				Node: { state: "ready", observed_at: "2026-10-26T09:12:00Z", records: 12, partial: false },
				Pod: { state: "ready", observed_at: "2026-10-26T09:12:00Z", records: 507, partial: false },
				StatefulSet: { state: "error", observed_at: "0001-01-01T00:00:00Z", completed_at: "0001-01-01T00:00:00Z", last_attempt_at: "2026-10-26T09:12:03Z", records: 0, partial: true, error: "budget_exhausted" },
			},
			total_kinds: 4, ready_kinds: 3, failing_kinds: 1, pending_kinds: 0, truncated: false,
		});
		const { container } = renderPage();
		const block = await waitFor(() => {
			const found = container.querySelector('[data-sync-health="failed"]');
			expect(found).toBeTruthy();
			return found!;
		});
		expect(block.querySelector('[data-sync-problem="StatefulSet"]')?.textContent).toContain("the read budget for this sync ran out");
		expect(block.textContent).not.toContain("did not report which");
	});

	it("reads a kind that is still to be listed as preparing, never as ready", async () => {
		const ready = { connector: "kubernetes", cluster_id: "production", observed_at: "2026-08-30T12:00:00Z", nodes: 3, ready_nodes: 3, pods: 4, running_pods: 4, namespaces: 2, active_namespaces: 2, workloads: 2, warnings: 0 };
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ ...ready, sync: { state: "ready", age_s: 0, partial: false } });
		// The real first-sync wire: a waiting kind is pending with partial:false.
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue({
			state: "syncing", partial: false, age_s: 0,
			kinds: {
				Pod: { state: "ready", observed_at: "2026-10-26T09:12:00Z", records: 507, partial: false },
				Node: { state: "pending", observed_at: "0001-01-01T00:00:00Z", completed_at: "0001-01-01T00:00:00Z", last_attempt_at: "0001-01-01T00:00:00Z", records: 0, partial: false },
			},
			total_kinds: 2, ready_kinds: 1, failing_kinds: 0, pending_kinds: 1, truncated: false,
		});
		const { container } = renderPage();
		await screen.findByRole("progressbar", { name: "Waiting for preparation progress" });
		expect(container.querySelector('[data-sync-health="preparing"]')).toBeTruthy();
		// A kind that is only waiting is never listed as a failure.
		expect(container.querySelectorAll("[data-sync-problem]")).toHaveLength(0);
		expect(screen.queryByRole("region", { name: "Cluster health" })).toBeNull();
		expect(vi.mocked(api.kubernetesWorkloads)).not.toHaveBeenCalled();
	});

	// Before a cluster publishes anything its report lists every known kind as
	// pending, so a full kinds map must still read as "nothing to show yet".
	it("reads a cluster whose kinds are all still to be listed as preparing, not as started", async () => {
		const ready = { connector: "kubernetes", cluster_id: "production", observed_at: "2026-08-30T12:00:00Z", nodes: 3, ready_nodes: 3, pods: 4, running_pods: 4, namespaces: 2, active_namespaces: 2, workloads: 2, warnings: 0 };
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ ...ready, sync: { state: "ready", age_s: 0, partial: false } });
		const known = ["ClusterRole", "ConfigMap", "CronJob", "DaemonSet", "Deployment", "Event", "Gateway", "HTTPRoute", "HorizontalPodAutoscaler", "Ingress", "Job", "Namespace", "Node", "PersistentVolumeClaim", "Pod", "ReplicaSet", "Secret", "Service", "ServiceAccount", "StatefulSet"];
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue({
			state: "syncing", partial: false, age_s: 0,
			kinds: Object.fromEntries(known.map((name) => [name, { state: "pending" as const, observed_at: "0001-01-01T00:00:00Z", completed_at: "0001-01-01T00:00:00Z", last_attempt_at: "0001-01-01T00:00:00Z", records: 0, partial: false }])),
			total_kinds: known.length, ready_kinds: 0, failing_kinds: 0, pending_kinds: known.length, truncated: false,
		});
		const { container } = renderPage();
		await screen.findByRole("progressbar", { name: "Waiting for preparation progress" });
		expect(container.querySelector('[data-sync-health="preparing"]')).toBeTruthy();
		expect(container.querySelector('[data-sync-health="ready"]')).toBeNull();
		// No kind is failing, so nothing is named as a problem.
		expect(container.querySelectorAll("[data-sync-problem]")).toHaveLength(0);
		for (const query of [api.kubernetesWorkloads, api.kubernetesNodes, api.kubernetesIssues, api.kubernetesUsage]) expect(vi.mocked(query)).not.toHaveBeenCalled();
	});

	it("keeps the views closed while the cluster-wide report is still on its way", async () => {
		const ready = { connector: "kubernetes", cluster_id: "production", observed_at: "2026-08-30T12:00:00Z", nodes: 3, ready_nodes: 3, pods: 4, running_pods: 4, namespaces: 2, active_namespaces: 2, workloads: 2, warnings: 0 };
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ ...ready, sync: { state: "ready", age_s: 0, partial: false } });
		vi.mocked(api.kubernetesSyncHealth).mockImplementation(() => new Promise(() => {}));
		const { container } = renderPage();
		await screen.findByRole("status", { name: "Loading cluster sync status" });
		expect(container.querySelector("[data-preparing-panel]")).toBeNull();
		// A ready envelope from the view is not evidence that the cluster is
		// ready, so no cluster query opens before the report answers.
		expect(screen.queryByRole("region", { name: "Cluster health" })).toBeNull();
		for (const query of [api.kubernetesWorkloads, api.kubernetesNodes, api.kubernetesIssues, api.kubernetesUsage]) expect(vi.mocked(query)).not.toHaveBeenCalled();
	});

	it("keeps the cluster closed when the sync health report cannot be read", async () => {
		const ready = { connector: "kubernetes", cluster_id: "production", observed_at: "2026-08-30T12:00:00Z", nodes: 3, ready_nodes: 3, pods: 4, running_pods: 4, namespaces: 2, active_namespaces: 2, workloads: 2, warnings: 0 };
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ ...ready, sync: { state: "ready", age_s: 0, partial: false } });
		vi.mocked(api.kubernetesSyncHealth).mockRejectedValue(new ApiError(500, "index unavailable"));
		const { container } = renderPage();
		const block = await waitFor(() => {
			const found = container.querySelector('[data-sync-health="failed"]');
			expect(found).toBeTruthy();
			return found!;
		});
		// Unreadable health is not health: the views stay closed and the surface
		// says the cause is not known rather than claiming nothing failed.
		expect(block.textContent).toContain("The index did not name a failing resource type, so the cause is not yet known.");
		expect(screen.queryByRole("region", { name: "Cluster health" })).toBeNull();
		expect(vi.mocked(api.kubernetesWorkloads)).not.toHaveBeenCalled();
	});

	// A kind can start failing long after the page settled, and a blocked cluster
	// can recover, so the report is re-read on a timer and whenever the stream
	// says new records landed. Neither needs Retry or a window focus.
	it("re-checks the report so a later failure re-blocks the views and a recovery opens them again", async () => {
		vi.mocked(api.kubernetesOverview).mockResolvedValue(healthyOverview);
		let emit: (event: { event: string; data: string }) => void = () => {};
		vi.mocked(api.kubernetesStream).mockImplementation(async (onEvent, signal) => {
			emit = onEvent as typeof emit;
			await new Promise<void>((resolve) => signal?.addEventListener("abort", () => resolve(), { once: true }));
		});
		const ready = report("ready", { Pod: readyKind });
		const failing = report("error", { Pod: readyKind, Secret: { state: "error", observed_at: "0001-01-01T00:00:00Z", last_attempt_at: "2026-10-26T09:12:03Z", records: 0, partial: true, error: "forbidden" } }, { failing: 1 });
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue(ready);
		const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
		const { container } = renderPage(client);
		expect(await screen.findByRole("region", { name: "Cluster health" })).toBeTruthy();

		// Even a healthy report is scheduled for a bounded recheck.
		const query = client.getQueryCache().getAll().find((entry) => entry.queryKey.includes("kubernetes-sync-health"));
		const interval = query?.options.refetchInterval as number | ((q: unknown) => number | false) | undefined;
		const intervalMs = typeof interval === "function" ? interval(query) : interval;
		expect(typeof intervalMs).toBe("number");
		expect(intervalMs).toBeGreaterThan(0);
		expect(intervalMs as number).toBeLessThanOrEqual(15_000);

		// A kind starts failing while the page is open: the next report blocks every view.
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue(failing);
		act(() => emit({ event: "records", data: "{}" }));
		await waitFor(() => expect(container.querySelector('[data-sync-health="failed"]')).toBeTruthy(), { timeout: 5000 });
		expect(screen.queryByRole("region", { name: "Cluster health" })).toBeNull();

		// And it recovers, again with no user action.
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue(ready);
		act(() => emit({ event: "resync", data: "{}" }));
		await waitFor(() => expect(container.querySelector("[data-sync-health]")).toBeNull(), { timeout: 5000 });
		expect(await screen.findByRole("region", { name: "Cluster health" })).toBeTruthy();
	}, 15_000);

	// F2: a background resync is not a reason to replace rows that are already in
	// hand. Preparing belongs to a cluster that has never published anything.
	it("keeps the Workloads rows through a background resync", async () => {
		vi.mocked(api.kubernetesOverview).mockResolvedValue(healthyOverview);
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue({ ...report("syncing", { Node: readyKind, Pod: readyKind }, { pending: 1 }), first_ready_at: published });
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [{ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout", uid: "checkout-uid" }], truncated: false, sync: { state: "syncing", age_s: 4, partial: false } });
		renderWorkloads();
		const workloads = await screen.findByRole("region", { name: "Workloads" });
		expect(await within(workloads).findByText("checkout")).toBeTruthy();
		expect(within(workloads).queryByRole("region", { name: "Preparing your cluster" })).toBeNull();
		expect(workloads.querySelector(".sk")).toBeNull();
	});

	it("shows Preparing for Workloads on a cluster that has never published", async () => {
		vi.mocked(api.kubernetesOverview).mockResolvedValue(healthyOverview);
		// The report is settled but names no publication: this is a first sync.
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue(report("ready", { Node: readyKind, Pod: readyKind }));
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [{ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "default", name: "checkout", uid: "checkout-uid" }], truncated: false, sync: { state: "syncing", age_s: 0, partial: false } });
		renderWorkloads();
		const workloads = await screen.findByRole("region", { name: "Workloads" });
		expect(await within(workloads).findByRole("region", { name: "Preparing your cluster" })).toBeTruthy();
		expect(within(workloads).queryByText("checkout")).toBeNull();
	});

	// F1: a dead credential must be named on whatever tab the operator is on. The
	// last report that succeeded is never preferred over the failing one, and the
	// failure still says when the cluster first published — but an age is only
	// stated for data that is actually on screen, and this page loaded none.
	it("names a revoked credential on a non-Overview view instead of leaving it blank", async () => {
		window.history.replaceState(null, "", "/agent/kubernetes?view=resources");
		vi.mocked(api.kubernetesOverview).mockRejectedValue(new ApiError(502, "credential revoked", { error: "credential revoked" }));
		vi.mocked(api.kubernetesSyncHealth).mockRejectedValue(new ApiError(502, "credential revoked", { error: "credential revoked", first_ready_at: published, age_s: 300 }));
		vi.mocked(api.kubernetesWorkloads).mockRejectedValue(new ApiError(502, "credential revoked"));
		const { container } = renderPage();
		const strip = await screen.findByRole("region", { name: "Cluster sync status" });
		expect(strip.getAttribute("data-sync-strip")).toBe("failed");
		expect(strip.querySelector("[data-sync-age]")).toBeNull();
		expect(container.querySelector('[data-sync-state="failed"]')?.textContent).toBe("Failed");
		// Nothing is blank: the inventory names its own failure.
		expect(await screen.findByText("Resource inventory is unavailable.")).toBeTruthy();
	});

	it("never keeps the last good report saying ready when the next read fails", async () => {
		vi.mocked(api.kubernetesOverview).mockResolvedValue(healthyOverview);
		let emit: (event: { event: string; data: string }) => void = () => {};
		vi.mocked(api.kubernetesStream).mockImplementation(async (onEvent, signal) => {
			emit = onEvent as typeof emit;
			await new Promise<void>((resolve) => signal?.addEventListener("abort", () => resolve(), { once: true }));
		});
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValueOnce({ ...report("ready", { Node: readyKind, Pod: readyKind }), first_ready_at: published });
		const { container } = renderPage();
		expect(await screen.findByRole("region", { name: "Cluster health" })).toBeTruthy();
		expect(container.querySelector("[data-sync-strip]")).toBeNull();
		expect(container.querySelector('[data-sync-state="ready"]')).toBeTruthy();

		// The credential is revoked while the page is open: the cached report keeps
		// saying ready, and it must not be believed over the failure.
		vi.mocked(api.kubernetesSyncHealth).mockRejectedValue(new ApiError(502, "credential revoked", { error: "credential revoked", first_ready_at: published, age_s: 120 }));
		act(() => emit({ event: "resync", data: "{}" }));
		await waitFor(() => expect(container.querySelector('[data-sync-strip="failed"]')).toBeTruthy(), { timeout: 6000 });
		expect(container.querySelector('[data-sync-state="failed"]')?.textContent).toBe("Failed");
		expect(screen.queryByRole("heading", { name: "Cluster sync failed" })).toBeNull();
		// The data the operator was reading is still there, under the strip.
		expect(screen.getByRole("region", { name: "Cluster health" })).toBeTruthy();
	}, 15_000);

	// QA-211: a published cluster whose credential was removed is not a product
	// nobody configured. The overview words both the same way, so the page leans
	// on the one read a dead credential cannot suppress — the health report.
	const deadCredentialKinds = { Secret: { state: "error", observed_at: "0001-01-01T00:00:00Z", last_attempt_at: "2026-10-26T09:12:03Z", records: 0, partial: true, error: "unauthorized" } };

	it("names a dead credential as a failure on a live session instead of showing connector setup", async () => {
		vi.mocked(api.kubernetesOverview).mockResolvedValue(healthyOverview);
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue({ ...report("ready", { Node: readyKind, Pod: readyKind }), first_ready_at: published, age_s: 180 });
		const { container } = renderPage();
		expect(await screen.findByRole("region", { name: "Cluster health" })).toBeTruthy();

		// The credential dies while the page is open. The overview answers with the
		// same words a never-configured deployment gets, and the report still
		// answers, which is what says this cluster exists.
		vi.mocked(api.kubernetesOverview).mockRejectedValue(new ApiError(503, "Kubernetes connector is not configured"));
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue({ ...report("error", deadCredentialKinds, { failing: 1 }), first_ready_at: published, age_s: 180 });
		fireEvent.click(screen.getByRole("button", { name: "Refresh Kubernetes data" }));

		const strip = await screen.findByRole("region", { name: "Cluster sync status" });
		expect(strip.getAttribute("data-sync-strip")).toBe("failed");
		expect(strip.textContent).toContain("Cluster sync failed");
		expect(within(strip).getByRole("button", { name: "Retry sync" })).toBeTruthy();
		expect(strip.querySelector("[data-sync-age]")?.textContent).toContain("Showing data from 3 minutes ago.");
		expect(container.querySelector('[data-sync-state="failed"]')?.textContent).toBe("Failed");
		expect(screen.queryByRole("article", { name: "Kubernetes connector" })).toBeNull();
		expect(screen.getByRole("tablist", { name: "Kubernetes views" })).toBeTruthy();
		// Every view keeps the data it had.
		expect(screen.getByRole("region", { name: "Cluster health" })).toBeTruthy();
		expect(screen.queryByRole("alert")).toBeNull();
	});

	it("renders the tabs and the failure, never setup or a blank view, on a cold reload with a dead credential", async () => {
		window.history.replaceState(null, "", "/agent/kubernetes?view=resources");
		// The same setup wording, and no cluster read succeeds. Only the report
		// answers, so this is a configured cluster with a dead credential. It even
		// carries a real snapshot age, which still describes data this page never
		// loaded.
		vi.mocked(api.kubernetesOverview).mockRejectedValue(new ApiError(503, "Kubernetes connector is not configured"));
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue({ ...report("error", deadCredentialKinds, { failing: 1 }), first_ready_at: published, age_s: 240 });
		vi.mocked(api.kubernetesWorkloads).mockRejectedValue(new ApiError(502, "credential revoked"));
		const { container } = renderPage();
		const strip = await screen.findByRole("region", { name: "Cluster sync status" });
		expect(strip.getAttribute("data-sync-strip")).toBe("failed");
		expect(container.querySelector('[data-sync-state="failed"]')?.textContent).toBe("Failed");
		expect(screen.queryByRole("article", { name: "Kubernetes connector" })).toBeNull();
		expect(screen.getByRole("tablist", { name: "Kubernetes views" })).toBeTruthy();
		// Nothing is on screen, so the strip claims no age for it: a snapshot age is
		// not a claim about data this page never loaded.
		expect(strip.querySelector("[data-sync-age]")).toBeNull();
		expect(strip.textContent).not.toContain("Showing data");
		// The view names its own failure rather than going blank.
		expect(await screen.findByText("Resource inventory is unavailable.")).toBeTruthy();
	});

	it("treats a coded credential answer as a fault even when it repeats the setup words", async () => {
		const fault = { error: "Kubernetes connector is not configured", code: "credential_unavailable", action: "Configure a credential source.", retryable: false };
		vi.mocked(api.kubernetesOverview).mockRejectedValue(new ApiError(503, fault.error, fault));
		vi.mocked(api.kubernetesSyncHealth).mockRejectedValue(new ApiError(503, fault.error, fault));
		const { container } = renderPage();
		expect(await screen.findByRole("heading", { name: "Cluster sync failed" })).toBeTruthy();
		expect(container.querySelector('[data-sync-health="failed"]')).toBeTruthy();
		expect(container.querySelector('[data-sync-state="failed"]')?.textContent).toBe("Failed");
		expect(screen.queryByRole("article", { name: "Kubernetes connector" })).toBeNull();
	});

	// The cluster list fails with the same credential fault as every other read,
	// so it is not a reason to replace the page with a generic error: the report
	// still answers, and the failure belongs on screen.
	it("names the failure when the cluster list is the read that fails", async () => {
		window.history.replaceState(null, "", "/agent/kubernetes");
		const fault = { error: "Kubernetes connector is not configured", code: "credential_unavailable", retryable: false };
		vi.mocked(api.kubernetesClusters).mockRejectedValue(new ApiError(503, fault.error, fault));
		vi.mocked(api.kubernetesOverview).mockRejectedValue(new ApiError(503, "Kubernetes connector is not configured"));
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue({ ...report("error", deadCredentialKinds, { failing: 1 }), first_ready_at: published, age_s: 90 });
		const { container } = renderPage(undefined, false);
		const strip = await screen.findByRole("region", { name: "Cluster sync status" });
		expect(strip.getAttribute("data-sync-strip")).toBe("failed");
		expect(container.querySelector('[data-sync-state="failed"]')?.textContent).toBe("Failed");
		expect(screen.queryByText(/Couldn't load clusters/)).toBeNull();
		expect(screen.queryByRole("article", { name: "Kubernetes connector" })).toBeNull();
	});
});

describe("Kubernetes unreadable kinds", () => {
	const healthyOverview = { connector: "kubernetes", cluster_id: "production", observed_at: "2026-08-30T12:00:00Z", nodes: 3, ready_nodes: 3, pods: 4, running_pods: 4, namespaces: 2, active_namespaces: 2, workloads: 2, warnings: 0 };
	const readyKind = { state: "ready" as const, observed_at: "2026-10-26T09:12:00Z", records: 12, partial: false };
	const forbidden = { state: "error" as const, observed_at: "0001-01-01T00:00:00Z", completed_at: "0001-01-01T00:00:00Z", last_attempt_at: "0001-01-01T00:00:00Z", records: 0, partial: true, error: "forbidden" };
	const base = { partial: false, age_s: 0, total_kinds: 4, ready_kinds: 2, failing_kinds: 0, pending_kinds: 0, truncated: false };

	// A least-privilege install lacks secrets and serviceaccounts. That is a
	// capability gap, so the explorer must open and say what it cannot show.
	it("opens every view and names the kinds the connector may not read, exactly once", async () => {
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ ...healthyOverview, sync: { state: "ready", age_s: 0, partial: false } });
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue({
			...base, state: "ready", unreadable_kinds: 2,
			kinds: { Pod: readyKind, Deployment: readyKind, Secret: forbidden, ServiceAccount: forbidden },
			unreadable: [
				{ kind: "Secret", state: "error", class: "forbidden", records: 0, since: "2026-10-26T10:49:24Z" },
				{ kind: "ServiceAccount", state: "error", class: "forbidden", records: 0, since: "2026-10-26T10:49:24Z" },
			],
		});
		const { container } = renderPage();
		expect(await screen.findByRole("region", { name: "Cluster health" })).toBeTruthy();
		// No blocking surface: the sync itself is healthy.
		expect(container.querySelector("[data-sync-health]")).toBeNull();
		expect(container.querySelectorAll("[data-sync-problem]")).toHaveLength(0);
		const note = container.querySelector<HTMLElement>("[data-unreadable-note]")!;
		expect(note.textContent).toBe("Not readable with this connector identity: Secret, ServiceAccount (permission denied). Everything else is shown.");
		// Each kind is named once, and no dot separator sneaks in.
		expect(note.textContent!.match(/Secret/g)).toHaveLength(1);
		expect(note.textContent!.match(/ServiceAccount/g)).toHaveLength(1);
		expect(note.textContent).not.toMatch(/[·•|]/);
		expect(note.className).not.toMatch(/sev-warn|sev-critical|amber/);
		expect(note.getAttribute("role")).toBe("status");
	});

	it("shows the note on a view that is not Overview, and when another kind blocks the sync", async () => {
		window.history.replaceState(null, "", "/agent/kubernetes?view=nodes");
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ ...healthyOverview, sync: { state: "requesting", age_s: 0, partial: false } });
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue({
			...base, state: "error", partial: true, failing_kinds: 1, unreadable_kinds: 1,
			// The kind carries the stable first-failure time; the retry time is newer.
			kinds: { Pod: readyKind, Secret: forbidden, Node: { state: "error", observed_at: "0001-01-01T00:00:00Z", completed_at: "0001-01-01T00:00:00Z", last_attempt_at: "2026-10-26T11:40:00Z", records: 0, partial: true, error: "timeout", failing_since: "2026-10-26T10:49:24Z" } },
			failing: [{ kind: "Node", state: "error", class: "timeout", records: 0, since: "2026-10-26T10:49:24Z" }],
			unreadable: [{ kind: "Secret", state: "error", class: "forbidden", records: 0, since: "2026-10-26T10:49:24Z" }],
		});
		const { container } = renderPage();
		await screen.findByRole("heading", { name: "Cluster sync failed" });
		// The real sync fault still closes the views; the unreadable kind is not it.
		expect(container.querySelectorAll("[data-sync-problem]")).toHaveLength(1);
		expect(container.querySelector('[data-sync-problem="Secret"]')).toBeNull();
		expect(container.querySelector('[data-sync-problem="Node"]')?.textContent).toContain("the cluster is slow or rate-limiting reads");
		// The stable first-failure time is what "since" prints, never a zero time.
		expect(container.querySelector('[data-sync-problem="Node"]')?.textContent).toContain("since");
		expect(container.querySelector('[data-sync-problem="Node"]')?.textContent).not.toContain("0001");
		expect(container.querySelector("[data-unreadable-note]")?.textContent).toContain("Secret");
		expect(screen.queryByRole("region", { name: "Nodes" })).toBeNull();
	});

	it("shows no note at all when every kind is readable", async () => {
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ ...healthyOverview, sync: { state: "ready", age_s: 0, partial: false } });
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue({ ...base, state: "ready", kinds: { Pod: readyKind, Deployment: readyKind } });
		const { container } = renderPage();
		await screen.findByRole("region", { name: "Cluster health" });
		expect(container.querySelector("[data-unreadable-note]")).toBeNull();
	});
});

describe("Kubernetes fleet sync health", () => {
	const fleetCluster = (id: string) => ({ id, health: "healthy" as const, provider: "generic", nodes: 2, ready_nodes: 2, pods: 4, running_pods: 4, warnings: 0, sync: { state: "ready", age_s: 0, partial: false }, observed_at: "2026-10-09T12:00:00Z" });

	it("gives every fleet row the cluster's own report, so a row cannot read ready while the explorer reads failed", async () => {
		const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
		// The fleet renders when the catalog lists more than one cluster and no
		// single cluster is open, so the catalog itself answers with the fleet.
		vi.mocked(api.kubernetesClusters).mockResolvedValue({ multiple: true, clusters: [fleetCluster("east")] });
		vi.mocked(api.kubernetesClusterSyncHealth).mockResolvedValue({
			state: "error", partial: true, age_s: 0,
			kinds: { Secret: { state: "error", observed_at: "0001-01-01T00:00:00Z", last_attempt_at: "2026-10-26T09:12:03Z", records: 0, partial: true, error: "forbidden" } },
			total_kinds: 1, ready_kinds: 0, failing_kinds: 1, pending_kinds: 0, truncated: false,
		});
		renderPage(client);
		expect(await screen.findByRole("article", { name: "Cluster east" })).toBeTruthy();
		expect(vi.mocked(api.kubernetesClusterSyncHealth)).toHaveBeenCalledWith("east");
		// The report resolves outside React's act scope, so let it land first.
		await settle();
		const synced = screen.getByRole("article", { name: "Cluster east" });
		expect(synced.querySelector("[data-sync-state-label]")?.textContent).toBe("Failed");
		// The catalog envelope still says ready; no count from it is shown.
		expect(within(synced).queryByRole("meter")).toBeNull();
		expect(within(synced).queryByText("0 warnings")).toBeNull();
	});
});

describe("Preparation and snapshot age regressions", () => {
 it("uses authoritative progress after a core-only overview becomes ready", async () => {
  const report = await api.kubernetesSyncHealth();
  vi.mocked(api.kubernetesSyncHealth).mockResolvedValue({ ...report, state: "syncing", pending_kinds: 8, first_ready_at: undefined, progress: { step: "listing", step_index: 2, step_total: 4, steps_done: ["connected", "building"], counts: { Pod: 18 }, kinds_done: 12, kinds_total: 20, remaining_kinds: ["ConfigMap"], kind_status: { ConfigMap: "pending" }, started_at: "2026-10-27T12:00:00Z", updated_at: "2026-10-27T12:00:16Z" } });
  renderPage();
  const panel = await screen.findByRole("region", { name: "Preparing your cluster" });
  expect(within(panel).getByRole("progressbar").getAttribute("aria-valuenow")).toBe("12");
  expect(within(panel).getByText("Remaining: ConfigMap (pending)")).toBeTruthy();
  expect(within(panel).queryByText(/unknown/i)).toBeNull();
 });
 it("labels the snapshot age rather than suggesting a sync duration", async () => {
  const overview = await api.kubernetesOverview();
  vi.mocked(api.kubernetesOverview).mockResolvedValue({ ...overview, partial_failures: [], sync: { state: "ready", age_s: 261, partial: false } });
  const issues = await api.kubernetesIssues();
  vi.mocked(api.kubernetesIssues).mockResolvedValue({ ...issues, sync: { state: "ready", age_s: 261, partial: false } });
  renderPage();
  expect(await screen.findByText("Snapshot 261s ago")).toBeTruthy();
  expect(screen.queryByText(/Synced 261s/)).toBeNull();
 });
});
