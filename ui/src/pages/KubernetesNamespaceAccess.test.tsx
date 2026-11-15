// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { BrowserRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api, ApiError, type KubernetesSyncHealthReport } from "@/lib/api";
import * as apiModule from "@/lib/api";
import { KubernetesPage } from "./KubernetesPage";
import { ToastProvider } from "@/components/Toast";
import { ResourceTimeline } from "./kubernetes-resource-detail/Timeline";
import { KubernetesNamespaceAccessContext } from "@/lib/kubernetesNamespaceAccess";

vi.mock("@/components/TopBar", () => ({ TopBar: ({ title, actions }: { title: string; actions?: React.ReactNode }) => <header><span>{title}</span>{actions}</header> }));
vi.mock("@/lib/api", async (importActual) => { const actual = await importActual<typeof import("@/lib/api")>(); return { ...actual, api: { ...actual.api, listAgentToolsets: vi.fn(), kubernetesOverview: vi.fn(), kubernetesSyncHealth: vi.fn(), kubernetesClusterSyncHealth: vi.fn(), kubernetesOverviewGraph: vi.fn(), kubernetesIssues: vi.fn(), kubernetesChanges: vi.fn(), kubernetesGraph: vi.fn(), kubernetesNamespaces: vi.fn(), kubernetesNeighborhood: vi.fn(), kubernetesCommitStream: vi.fn(), kubernetesTop: vi.fn(), kubernetesReleases: vi.fn(), kubernetesGitOpsApps: vi.fn(), kubernetesRollout: vi.fn(), kubernetesRollouts: vi.fn(), kubernetesTraffic: vi.fn(), kubernetesDiagnose: vi.fn(), kubernetesWorkloadLogs: vi.fn(), kubernetesPodLogs: vi.fn(), kubernetesUsage: vi.fn(), kubernetesWorkloads: vi.fn(), kubernetesWorkload: vi.fn(), kubernetesNodes: vi.fn(), kubernetesNodePods: vi.fn(), kubernetesSearch: vi.fn(), kubernetesEvents: vi.fn(), kubernetesDescribe: vi.fn() } }; });

function renderPage(view?: string) {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	client.setQueryData(["kubernetes-clusters"], { multiple: false, clusters: [] });
	window.history.replaceState(null, "", `/agent/kubernetes${view ? `?view=${view}` : ""}`);
	return render(<QueryClientProvider client={client}><ToastProvider><BrowserRouter><KubernetesPage /></BrowserRouter></ToastProvider></QueryClientProvider>);
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
	vi.mocked(api.kubernetesCommitStream).mockImplementation((_onEvent, signal) => new Promise<void>((resolve) => {
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
afterEach(() => { cleanup(); vi.clearAllMocks(); vi.useRealTimers(); window.history.replaceState(null, "", "/"); });

const published = "2026-11-14T09:00:00Z";
const observed = new Date(Date.now() - 20_000).toISOString();
const scopedKind = { state: "partial" as const, observed_at: observed, records: 4, partial: true, error: "namespace_scope" };
const forbiddenKind = { state: "error" as const, observed_at: "0001-01-01T00:00:00Z", records: 0, partial: true, error: "forbidden" };

/**
 * The report a Role and RoleBinding in shop and payments produces: namespaced
 * kinds are read only inside those namespaces, and cluster-scoped kinds are
 * refused. The server calls the aggregate partial.
 */
function namespaceOnlyReport(overrides: Partial<KubernetesSyncHealthReport> = {}): KubernetesSyncHealthReport {
	return {
		state: "ready", partial: false, age_s: 20, first_ready_at: published,
		kinds: { Pod: scopedKind, Deployment: scopedKind, Node: forbiddenKind, Namespace: forbiddenKind },
		namespace_scoped: [{ kind: "Deployment", state: "ready", class: "namespace_scope", records: 4 }, { kind: "Pod", state: "ready", class: "namespace_scope", records: 4 }], namespace_scoped_kinds: 2,
		unreadable: [{ kind: "Namespace", state: "error", class: "forbidden", records: 0 }, { kind: "Node", state: "error", class: "forbidden", records: 0 }],
		total_kinds: 4, ready_kinds: 0, failing_kinds: 2, unreadable_kinds: 2, pending_kinds: 0, truncated: false,
		cluster_wide: false, readable_namespaces: ["payments", "shop"],
		...overrides,
	};
}

const namespaceOnlyOverview = {
	connector: "kubernetes", cluster_id: "production", observed_at: observed, nodes: 0, ready_nodes: 0, pods: 4, running_pods: 4,
	namespaces: 2, active_namespaces: 2, workloads: 2, warnings: 0, usage_source: "unavailable" as const, metrics_status: "unavailable" as const,
	metrics_fresh: false, truncated: false, sync: { state: "partial", age_s: 20, partial: true }, cluster_wide: false, readable_namespaces: ["payments", "shop"],
	partial_failures: [
		{ resource_id: "core~v1~pods", class: "namespace_scope", kind: "Pod", message: "Pod is read only in the namespaces this connector can access." },
		{ resource_id: "core~v1~nodes", class: "forbidden", kind: "Node", message: "Node is not readable with this connector's permissions." },
	],
};

const historyOffChanges = { items: [], gaps: [], history: "off_namespace_access", sync: { state: "partial", age_s: 0, partial: true } };
const historyOffTimeline = { ...historyOffChanges, total: 0, crash_count: 0, counts: { change: 0, crash: 0, break: 0 }, since: "2026-11-14T08:00:00Z", until: "2026-11-14T09:00:00Z" };

function namespaceOnly() {
	vi.mocked(api.kubernetesSyncHealth).mockResolvedValue(namespaceOnlyReport());
	vi.mocked(api.kubernetesOverview).mockResolvedValue(namespaceOnlyOverview);
	vi.mocked(api.kubernetesIssues).mockResolvedValue({ items: [], totals: {}, broken: { total: 0, by_severity: {} }, truncated: false, sync: { state: "partial", age_s: 0, partial: true }, partial_failures: [{ resource_id: "core~v1~pods", class: "namespace_scope" }] });
	vi.mocked(api.kubernetesChanges).mockResolvedValue(historyOffChanges);
	vi.mocked(api.kubernetesTimeline).mockResolvedValue(historyOffTimeline);
	vi.mocked(api.kubernetesNamespaces).mockRejectedValue(new ApiError(403, "Kubernetes denied access to this resource.", { code: "cluster_permission_denied" }));
}

describe("namespace-only access", () => {
	it("shows a steady namespace status instead of a degraded sync, with a docs link", async () => {
		namespaceOnly();
		const { container } = renderPage();
		const trigger = await screen.findByRole("button", { name: "Namespace access — showing payments, shop" });
		expect(container.querySelector("[data-sync-state]")?.getAttribute("data-sync-state")).toBe("ready");
		expect(screen.queryByText(/Sync degraded/)).toBeNull();
		expect(screen.queryByText(/Sync failed/)).toBeNull();
		expect(container.querySelector("[data-sync-health]")).toBeNull();
		fireEvent.mouseEnter(trigger);
		const details = screen.getByRole("region", { name: "Namespace access details" });
		expect(details.textContent).toContain("payments, shop");
		expect(within(details).getByRole("link", { name: /namespace-only access/ }).getAttribute("href")).toBe("https://docs.versusincident.com/#/agent/kubernetes/configuration?id=least-privilege");
	});

	it("never shows Broken unknown and counts broken workloads in the readable namespaces", async () => {
		namespaceOnly();
		renderPage();
		const health = await screen.findByRole("region", { name: "Cluster health" });
		await waitFor(() => expect(within(health).getByRole("button", { name: "0 broken" })).toBeTruthy());
		expect(within(health).getByText("Namespace access — showing payments, shop")).toBeTruthy();
		expect(screen.queryByText("Broken unknown")).toBeNull();
		expect(screen.queryByText(/Sync degraded/)).toBeNull();
	});

	it("leaves the broken count out rather than calling it unknown when no total is reported", async () => {
		namespaceOnly();
		vi.mocked(api.kubernetesIssues).mockRejectedValue(new ApiError(403, "denied", { code: "cluster_permission_denied" }));
		renderPage();
		const health = await screen.findByRole("region", { name: "Cluster health" });
		await waitFor(() => expect(api.kubernetesIssues).toHaveBeenCalled());
		await act(async () => { await new Promise((resolve) => setTimeout(resolve, 50)); });
		expect(within(health).queryByText("Broken unknown")).toBeNull();
	});

	it("offers the readable namespaces in the workload picker and never lists Namespaces", async () => {
		namespaceOnly();
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [{ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "shop", name: "web" }], counts: { Deployment: 1 }, truncated: false, sync: { state: "partial", age_s: 0, partial: true } });
		renderPage("resources");
		const picker = await screen.findByRole("combobox", { name: "Workload namespace" });
		expect((picker as HTMLInputElement).value).toBe("All readable namespaces");
		await screen.findByText("web");
		// The empty choice reads every readable namespace; the server scopes it.
		expect(vi.mocked(api.kubernetesWorkloads).mock.calls[0]?.[0]?.namespace).toBeUndefined();
		fireEvent.focus(picker);
		const options = within(screen.getByRole("listbox", { name: "Workload namespace options" })).getAllByRole("option").map((option) => option.textContent);
		expect(options).toEqual(["All readable namespaces", "payments", "shop"]);
		expect(api.kubernetesNamespaces).not.toHaveBeenCalled();
		expect(screen.queryByText("Namespaces unavailable.")).toBeNull();
		expect(screen.queryByText(/Inventory unavailable/)).toBeNull();
	});

	it("lists the readable namespaces in topology without reading Namespaces", async () => {
		namespaceOnly();
		renderPage("topology");
		expect(await screen.findByRole("button", { name: "Open namespace shop topology" })).toBeTruthy();
		expect(screen.getByRole("button", { name: "Open namespace payments topology" })).toBeTruthy();
		expect(screen.queryByText(/Namespace list unavailable/)).toBeNull();
		expect(api.kubernetesNamespaces).not.toHaveBeenCalled();
	});

	it("keeps the cluster-wide picker and its Namespace list when cluster_wide is missing", async () => {
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [], counts: {}, truncated: false });
		renderPage("resources");
		const picker = await screen.findByRole("combobox", { name: "Workload namespace" });
		expect((picker as HTMLInputElement).value).toBe("All namespaces");
		fireEvent.focus(picker);
		await waitFor(() => expect(within(screen.getByRole("listbox", { name: "Workload namespace options" })).getAllByRole("option").map((option) => option.textContent)).toEqual(["All namespaces", "shop", "payments"]));
	});
});

describe("history off for namespace-only access", () => {
	it("says history is off on the Overview, with a link and no Retry", async () => {
		namespaceOnly();
		renderPage();
		const timeline = await screen.findByRole("region", { name: "Break timeline" });
		const notice = await within(timeline).findByText(/History is off for namespace-only access\./);
		expect(within(notice).getByRole("link", { name: /Learn why/ }).getAttribute("href")).toBe("https://docs.versusincident.com/#/agent/kubernetes/configuration?id=least-privilege");
		expect(screen.queryByText(/Recent change history unavailable/)).toBeNull();
		expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
		expect(within(timeline).getByText("Off")).toBeTruthy();
	});

	it("says history is off on the Timeline tab, with no Retry", async () => {
		namespaceOnly();
		renderPage("timeline");
		const section = await screen.findByRole("region", { name: "Kubernetes change timeline" });
		await within(section).findByText(/History is off for namespace-only access\./);
		expect(screen.queryByRole("button", { name: "Retry Timeline" })).toBeNull();
		expect(screen.queryByText("Timeline unavailable.")).toBeNull();
		expect(screen.queryByText("Some history could not be read")).toBeNull();
	});

	it("treats a refused history read as off once the identity is namespace-only", async () => {
		namespaceOnly();
		const denied = { error: "Authority for this saved evidence is denied or has not been established.", code: "history_authority_denied", action: "Wait for a successful authorized collection.", retryable: false };
		vi.mocked(api.kubernetesTimeline).mockRejectedValue(new ApiError(403, denied.error, denied));
		renderPage("timeline");
		const section = await screen.findByRole("region", { name: "Kubernetes change timeline" });
		await within(section).findByText(/History is off for namespace-only access\./);
		expect(screen.queryByRole("button", { name: "Retry Timeline" })).toBeNull();
	});

	it("stops asking for history every 30 seconds once it is off", async () => {
		vi.useFakeTimers({ shouldAdvanceTime: true });
		namespaceOnly();
		renderPage();
		await screen.findByText(/History is off for namespace-only access\./);
		const changes = vi.mocked(api.kubernetesChanges).mock.calls.length;
		await act(async () => { vi.advanceTimersByTime(95_000); });
		expect(vi.mocked(api.kubernetesChanges).mock.calls.length).toBe(changes);
	});

	it("stops refreshing the Timeline tab every 30 seconds once it is off", async () => {
		vi.useFakeTimers({ shouldAdvanceTime: true });
		namespaceOnly();
		renderPage("timeline");
		await screen.findByText(/History is off for namespace-only access\./);
		const reads = vi.mocked(api.kubernetesTimeline).mock.calls.length;
		await act(async () => { vi.advanceTimersByTime(95_000); });
		expect(vi.mocked(api.kubernetesTimeline).mock.calls.length).toBe(reads);
	});

	it("keeps the 30 second refresh while history is kept", async () => {
		vi.useFakeTimers({ shouldAdvanceTime: true });
		renderPage();
		await screen.findByRole("region", { name: "Break timeline" });
		await waitFor(() => expect(api.kubernetesChanges).toHaveBeenCalled());
		const changes = vi.mocked(api.kubernetesChanges).mock.calls.length;
		await act(async () => { vi.advanceTimersByTime(31_000); });
		await waitFor(() => expect(vi.mocked(api.kubernetesChanges).mock.calls.length).toBeGreaterThan(changes));
		expect(screen.queryByText(/History is off/)).toBeNull();
	});
});

describe("Helm releases without Secret access", () => {
	const needsAccess = {
		items: [], truncated: false, access: "needs_secret_list",
		access_message: "Helm releases need list access to Secrets. Versus reads only their labels, never their contents.",
		docs_url: "https://docs.versusincident.com/#/agent/kubernetes/configuration?id=helm-releases-need-secret-access",
	};

	it("says why on the Helm tab and never claims there are no releases", async () => {
		vi.mocked(api.kubernetesReleases).mockResolvedValue(needsAccess);
		renderPage("releases");
		const section = await screen.findByRole("region", { name: "Helm releases" });
		const notice = await within(section).findByText(/Helm releases need list access to Secrets\./);
		expect(within(notice).getByRole("link").getAttribute("href")).toBe(needsAccess.docs_url);
		expect(screen.queryByText("No Helm release labels were found.")).toBeNull();
	});

	it("says why on the Overview card and never claims there are no releases", async () => {
		vi.mocked(api.kubernetesReleases).mockResolvedValue(needsAccess);
		renderPage();
		await screen.findByText(/Helm releases need list access to Secrets\./);
		expect(screen.queryByText("No Helm release labels were found.")).toBeNull();
		expect(screen.queryByText("Release summary unavailable.")).toBeNull();
	});

	it("says the same for a refused read coded helm_needs_secret_list", async () => {
		const body = { error: needsAccess.access_message, code: "helm_needs_secret_list", action: "Grant the connector identity list on secrets.", retryable: false };
		vi.mocked(api.kubernetesReleases).mockRejectedValue(new ApiError(403, body.error, body));
		renderPage("releases");
		const section = await screen.findByRole("region", { name: "Helm releases" });
		await within(section).findByText(/Helm releases need list access to Secrets\./);
		expect(screen.queryByText("No Helm release labels were found.")).toBeNull();
		expect(screen.queryByText("Release metadata is unavailable.")).toBeNull();
	});

	it("still says there are no releases when Secrets can be listed", async () => {
		renderPage("releases");
		expect(await screen.findByText("No Helm release labels were found.")).toBeTruthy();
	});
});

describe("connector errors in the server's words", () => {
	it("names why the connector could not be built in the cannot-connect panel", async () => {
		const body = { error: "Kubeconfig exec credentials are not supported.", code: "connector_configuration_invalid", action: "Use auth.mode eks, aks or gke.", retryable: false };
		vi.mocked(api.kubernetesOverview).mockRejectedValue(new ApiError(503, body.error, body));
		vi.mocked(api.kubernetesSyncHealth).mockRejectedValue(new ApiError(503, body.error, body));
		const { container } = renderPage();
		await screen.findByRole("heading", { name: "Cannot connect to this cluster" });
		const detail = container.querySelector('[data-connector-detail="connector_configuration_invalid"]');
		expect(detail?.textContent).toContain("Kubeconfig exec credentials are not supported.");
		expect(detail?.textContent).toContain("Use auth.mode eks, aks or gke.");
		expect(screen.queryByText(/did not respond/)).toBeNull();
	});

	it("names the refused address and the setting that allows it", async () => {
		const body = { error: "Versus refused the Kubernetes API address 10.1.2.3 because it is a private address.", code: "connector_endpoint_refused", action: "Set connectors.kubernetes.allow_private_networks: true, then restart Versus.", retryable: false };
		vi.mocked(api.kubernetesOverview).mockRejectedValue(new ApiError(503, body.error, body));
		vi.mocked(api.kubernetesSyncHealth).mockRejectedValue(new ApiError(503, body.error, body));
		const { container } = renderPage();
		await screen.findByRole("heading", { name: "Cannot connect to this cluster" });
		const detail = container.querySelector('[data-connector-detail="connector_endpoint_refused"]');
		expect(detail?.textContent).toContain("10.1.2.3");
		expect(detail?.textContent).toContain("allow_private_networks");
	});

	it("links the not-configured card to the quickstart", async () => {
		const message = "Kubernetes connector is not configured";
		vi.mocked(api.kubernetesOverview).mockRejectedValue(new ApiError(503, message, { error: message }));
		vi.mocked(api.kubernetesSyncHealth).mockRejectedValue(new ApiError(503, message, { error: message }));
		renderPage();
		const card = await screen.findByRole("article", { name: "Kubernetes connector" });
		expect(within(card).getByRole("link", { name: "Quickstart" }).getAttribute("href")).toBe("https://docs.versusincident.com/#/agent/kubernetes/quickstart");
		expect(within(card).getByText("Kubernetes connector is not configured.")).toBeTruthy();
	});
});

describe("coverage popover", () => {
	it("shows the server's sentences in the server's order", async () => {
		namespaceOnly();
		renderPage();
		fireEvent.click(await screen.findByRole("button", { name: "Resource coverage" }));
		const reasons = within(screen.getByRole("region", { name: "Resource coverage details" })).getAllByRole("listitem").map((item) => item.textContent);
		expect(reasons.slice(0, 2)).toEqual([
			"Pod is read only in the namespaces this connector can access.",
			"Node is not readable with this connector's permissions.",
		]);
		expect(reasons.join(" ")).not.toMatch(/\bnodes: permission denied|pods: the namespace scope/);
	});
});

describe("resource history for namespace-only access", () => {
	it("says history is off in the resource drawer instead of offering Retry", async () => {
		vi.mocked(api.kubernetesTimeline).mockResolvedValue(historyOffTimeline);
		const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
		render(<QueryClientProvider client={client}><KubernetesNamespaceAccessContext.Provider value={{ clusterWide: false, namespaces: ["shop"] }}>
			<ResourceTimeline resource={{ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "shop", name: "web", uid: "web-uid" }} onCrash={() => undefined} />
		</KubernetesNamespaceAccessContext.Provider></QueryClientProvider>);
		await screen.findByText(/History is off for namespace-only access\./);
		expect(screen.queryByRole("button", { name: "Retry history" })).toBeNull();
		expect(screen.queryByText("History unavailable.")).toBeNull();
		expect(screen.queryByRole("button", { name: "Resource coverage" })).toBeNull();
	});
});

describe("unreadable and not-collected kinds in the top-bar sync details", () => {
	const report = (overrides: Partial<KubernetesSyncHealthReport>): KubernetesSyncHealthReport => ({
		state: "ready", partial: false, age_s: 0, first_ready_at: published,
		kinds: { Pod: { state: "ready", observed_at: observed, records: 4, partial: false } },
		total_kinds: 1, ready_kinds: 1, failing_kinds: 0, pending_kinds: 0, truncated: false, ...overrides,
	});
	const gaps = {
		not_collected: ["Secret"],
		unreadable_kinds: 2,
		// A kind both excluded and refused is named only as excluded.
		unreadable: [{ kind: "Secret", state: "error", class: "forbidden", records: 0 }, { kind: "Node", state: "error", class: "forbidden", records: 0 }],
	};

	it("keeps the page body free of the note and the healthy label in the top bar", async () => {
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue(report(gaps));
		const { container } = renderPage();
		const trigger = await screen.findByRole("button", { name: "Resource types not fully shown: 1 not readable, 1 not collected" });
		expect(container.querySelector("[data-sync-state]")?.textContent).toMatch(/^Last synced/);
		expect(trigger.className).not.toMatch(/sev-(warning|critical)|pill-(warn|bad)/);
		expect(screen.queryByText(/Not readable with this connector identity/)).toBeNull();
		expect(screen.queryByText(/Not collected by configuration/)).toBeNull();
		expect(document.querySelector("[data-read-coverage]")).toBeNull();
	});

	it("lists unreadable kinds with reasons and not-collected kinds muted, with a docs link", async () => {
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue(report(gaps));
		renderPage();
		fireEvent.mouseEnter(await screen.findByRole("button", { name: /^Resource types not fully shown/ }));
		const details = screen.getByRole("region", { name: "Sync status details" });
		const unreadable = details.querySelector("[data-unreadable-note]")!;
		expect(unreadable.textContent).toContain("Not readable with this connector identity:");
		expect(unreadable.textContent).toContain("Node — permission denied");
		expect(unreadable.textContent).toContain("Everything else is shown.");
		expect(unreadable.textContent).not.toContain("Secret");
		const skipped = details.querySelector("[data-not-collected]")!;
		expect(skipped.textContent).toBe("Not collected by configuration: Secret");
		expect(skipped.className).toContain("text-ink-400");
		expect(details.outerHTML).not.toMatch(/sev-(warning|critical)|pill-(warn|bad)/);
		expect(within(details).getByRole("link", { name: /Reader permissions/ }).getAttribute("href")).toBe("https://docs.versusincident.com/#/agent/kubernetes/configuration?id=least-privilege");
	});

	it("opens from the keyboard and closes on Escape", async () => {
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue(report(gaps));
		renderPage();
		const trigger = await screen.findByRole("button", { name: /^Resource types not fully shown/ });
		act(() => trigger.focus());
		expect(screen.getByRole("region", { name: "Sync status details" })).toBeTruthy();
		fireEvent.keyDown(document, { key: "Escape" });
		await waitFor(() => expect(screen.queryByRole("region", { name: "Sync status details" })).toBeNull());
		expect(document.activeElement).toBe(trigger);
	});

	it("lets a failure keep its label and adds the kinds to its details", async () => {
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue(report({ ...gaps, state: "error", partial: true, failing_kinds: 1,
			kinds: { Pod: { state: "ready", observed_at: observed, records: 4, partial: false }, Event: { state: "error", observed_at: observed, records: 0, partial: true, error: "timeout" } } }));
		renderPage();
		const trigger = await screen.findByRole("button", { name: /^Sync failed/ });
		expect(screen.queryByRole("button", { name: /^Resource types not fully shown/ })).toBeNull();
		fireEvent.mouseEnter(trigger);
		const details = screen.getByRole("region", { name: "Sync status details" });
		expect(details.querySelector('[data-sync-problem="Event"]')).toBeTruthy();
		expect(details.querySelector("[data-unreadable-note]")?.textContent).toContain("Node");
		expect(details.querySelector("[data-not-collected]")?.textContent).toContain("Secret");
	});

	it("adds the kinds to the namespace access details", async () => {
		namespaceOnly();
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue(namespaceOnlyReport({ not_collected: ["Secret"] }));
		renderPage();
		fireEvent.mouseEnter(await screen.findByRole("button", { name: "Namespace access — showing payments, shop" }));
		const details = screen.getByRole("region", { name: "Namespace access details" });
		expect(details.querySelector("[data-unreadable-note]")?.textContent).toContain("Namespace");
		expect(details.querySelector("[data-not-collected]")?.textContent).toContain("Secret");
	});

	it("shows no marker when every kind is read and nothing is excluded", async () => {
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue(report({ not_collected: [] }));
		renderPage();
		await screen.findByRole("region", { name: "Cluster health" });
		expect(screen.queryByRole("button", { name: /^Resource types not fully shown/ })).toBeNull();
	});
});

describe("default install polish", () => {
	it("counts warnings the way Issues does, and uses amber only above zero", async () => {
		vi.mocked(api.kubernetesIssues).mockImplementation(async (options) => ({ items: [], totals: options?.broken ? {} : { warning: 3 }, broken: { total: 0, by_severity: {} }, truncated: false, sync: { state: "ready", age_s: 0, partial: false } }));
		renderPage();
		const chip = await screen.findByRole("button", { name: "3 warnings" });
		expect(chip.className).toContain("pill-warn");
		expect(screen.queryByText("2 warnings")).toBeNull();
	});

	it("keeps a zero warning count neutral", async () => {
		vi.mocked(api.kubernetesIssues).mockResolvedValue({ items: [], totals: {}, broken: { total: 0, by_severity: {} }, truncated: false, sync: { state: "ready", age_s: 0, partial: false } });
		renderPage();
		const chip = await screen.findByRole("button", { name: "0 warnings" });
		expect(chip.className).not.toContain("pill-warn");
		expect(screen.getByRole("button", { name: "Review issues" }).querySelector("svg")?.getAttribute("class")).not.toContain("sev-warning");
	});

	it("says CPU and memory need metrics-server instead of a strip of unavailable values", async () => {
		const none = { availability: "unavailable" as const, fresh: false, complete: false, total: 0 };
		vi.mocked(api.kubernetesUsage).mockResolvedValue({ observed_at: observed, metrics: "not_installed", availability: "unavailable", fresh: false, truncated: false, pod_metrics: none, node_metrics: none });
		vi.mocked(api.kubernetesTop).mockResolvedValue({ items: [], items_available: false, total: 0, truncated: false, metrics: "not_installed", availability: "unavailable", fresh: false, sync: { state: "direct", age_s: 0, partial: false } });
		const { container } = renderPage();
		await waitFor(() => expect(container.querySelectorAll("[data-metrics-server-missing]")).toHaveLength(2));
		const line = container.querySelector("[data-metrics-server-missing]")!;
		expect(line.textContent).toContain("CPU and memory need metrics-server.");
		expect(line.querySelector("a")?.getAttribute("href")).toBe("https://github.com/kubernetes-sigs/metrics-server#installation");
		expect(line.className).not.toMatch(/sev-/);
		expect(screen.queryByText(/Aggregate .* CPU/)).toBeNull();
		expect(screen.queryByText("Usage ranking unavailable; no list was returned.")).toBeNull();
	});
});

describe("metrics API states", () => {
	const none = { availability: "unavailable" as const, fresh: false, complete: false, total: 0 };
	it("names refused metrics calmly in the top-bar details, not as missing metrics-server", async () => {
		vi.mocked(api.kubernetesUsage).mockResolvedValue({ observed_at: observed, metrics: "forbidden", availability: "unavailable", fresh: false, truncated: false, pod_metrics: none, node_metrics: none });
		vi.mocked(api.kubernetesTop).mockResolvedValue({ items: [], items_available: false, total: 0, truncated: false, metrics: "forbidden", availability: "unavailable", fresh: false, sync: { state: "direct", age_s: 0, partial: false } });
		const { container } = renderPage();
		fireEvent.mouseEnter(await screen.findByRole("button", { name: "Resource types not fully shown: 1 not readable" }));
		expect(screen.getByRole("region", { name: "Sync status details" }).querySelector("[data-unreadable-note]")?.textContent).toContain("CPU and memory usage — permission denied");
		expect(container.querySelector("[data-metrics-server-missing]")).toBeNull();
		expect(screen.getByText("Usage metrics are not readable with this access.")).toBeTruthy();
	});

	it("shows loading while metrics are syncing", async () => {
		vi.mocked(api.kubernetesUsage).mockResolvedValue({ observed_at: observed, metrics: "syncing", availability: "unavailable", fresh: false, truncated: false, pod_metrics: none, node_metrics: none });
		vi.mocked(api.kubernetesTop).mockResolvedValue({ items: [], items_available: false, total: 0, truncated: false, metrics: "syncing", availability: "unavailable", fresh: false, sync: { state: "direct", age_s: 0, partial: false } });
		const { container } = renderPage();
		expect(await screen.findByText("Loading usage snapshot")).toBeTruthy();
		expect(screen.getByLabelText("Loading usage ranking")).toBeTruthy();
		expect(container.querySelector("[data-metrics-server-missing]")).toBeNull();
		expect(screen.queryByText("Usage ranking unavailable; no list was returned.")).toBeNull();
	});
});

describe("Gateway API kinds in Topology", () => {
	it("shows Gateway and HTTPRoute by default, like Ingress", async () => {
		const node = (kind: string, name: string) => ({ id: `${kind}:${name}`, kind, namespace: "shop", name, resource_id: `gateway.networking.k8s.io~v1~${kind.toLowerCase()}s` });
		vi.mocked(api.kubernetesGraph).mockResolvedValue({
			nodes: [node("Gateway", "qa-gateway"), node("HTTPRoute", "qa-route"), { id: "Service:web", kind: "Service", namespace: "shop", name: "web" }],
			edges: [{ from: "Gateway:qa-gateway", to: "HTTPRoute:qa-route", type: "routes-to" }, { from: "HTTPRoute:qa-route", to: "Service:web", type: "routes-to" }],
			omitted: {}, sync: { state: "ready", age_s: 0, partial: false },
		} as Awaited<ReturnType<typeof api.kubernetesGraph>>);
		renderPage("topology");
		fireEvent.click(await screen.findByRole("button", { name: "Open namespace shop topology" }));
		expect(await screen.findByRole("button", { name: "Open Gateway shop/qa-gateway details" })).toBeTruthy();
		expect(screen.getByRole("button", { name: "Open HTTPRoute shop/qa-route details" })).toBeTruthy();
	});
});

describe("namespace-only steady view", () => {
	const denied = { error: "Authority for this saved evidence is denied or has not been established.", code: "history_authority_denied", action: "Wait.", retryable: false };

	it("says live updates are off, calmly, when the feed's first commit says history is off", async () => {
		namespaceOnly();
		vi.useFakeTimers({ shouldAdvanceTime: true });
		vi.mocked(api.kubernetesCommitStream).mockImplementation((onEvent, signal) => new Promise<void>((resolve) => {
			onEvent({ event: "committed", id: "1", data: JSON.stringify({ revision: 1, kinds: ["Pod"], cluster_id: "production", history: "off_namespace_access" }) });
			signal?.addEventListener("abort", () => resolve(), { once: true });
		}));
		renderPage();
		const trigger = await screen.findByRole("button", { name: "Namespace access — showing payments, shop" });
		await waitFor(() => expect(api.kubernetesCommitStream).toHaveBeenCalled());
		expect(screen.queryByText(/Updates disconnected/)).toBeNull();
		fireEvent.mouseEnter(trigger);
		expect(screen.getByRole("region", { name: "Namespace access details" }).querySelector("[data-namespace-freshness]")?.textContent).toBe("Live updates are off for namespace access; this view refreshes every 30 seconds.");
		// The periodic refresh stays the source of truth while the feed is open.
		const reads = vi.mocked(api.kubernetesOverview).mock.calls.length;
		await act(async () => { vi.advanceTimersByTime(31_000); });
		await waitFor(() => expect(vi.mocked(api.kubernetesOverview).mock.calls.length).toBeGreaterThan(reads));
		// The namespace-scoped kinds are this role's normal state, not named as a gap.
		expect(screen.queryByRole("button", { name: /^Resource types not fully shown/ })).toBeNull();
	});

	it("keeps the namespace status and every panel when an older server refuses the feed", async () => {
		namespaceOnly();
		vi.mocked(api.kubernetesTop).mockResolvedValue({ items: [{ kind: "Pod", namespace: "shop", name: "web-1", cpu: "10m", memory: "20Mi" }], items_available: true, total: 1, truncated: false, availability: "available", fresh: true, sync: { state: "direct", age_s: 0, partial: false } });
		vi.mocked(api.kubernetesCommitStream).mockRejectedValue(new ApiError(403, denied.error, denied));
		renderPage();
		await screen.findByText("shop/web-1");
		await waitFor(() => expect(api.kubernetesCommitStream).toHaveBeenCalled());
		await act(async () => { await new Promise((resolve) => setTimeout(resolve, 200)); });
		expect(screen.getByRole("button", { name: "Namespace access — showing payments, shop" })).toBeTruthy();
		expect(screen.queryByText(/Updates disconnected/)).toBeNull();
		expect(screen.getByText("shop/web-1")).toBeTruthy();
		expect(screen.queryByText("Usage ranking unavailable.")).toBeNull();
		expect(screen.queryByText(/Break timeline unavailable/)).toBeNull();
		// The refusal is final for this identity, so the feed is not retried.
		await act(async () => { await new Promise((resolve) => setTimeout(resolve, 1200)); });
		expect(api.kubernetesCommitStream).toHaveBeenCalledTimes(1);
	});

	it("offers no Retry on the Overview timeline card while history is off", async () => {
		namespaceOnly();
		vi.mocked(api.kubernetesIssues).mockRejectedValue(new ApiError(503, "busy", { code: "history_store_timeout" }));
		renderPage();
		const card = await screen.findByRole("region", { name: "Break timeline" });
		await within(card).findByText(/Break timeline unavailable/);
		expect(within(card).queryByRole("button", { name: "Retry" })).toBeNull();
	});

	it("says GitOps kinds are not readable rather than none returned", async () => {
		namespaceOnly();
		vi.mocked(api.kubernetesGitOpsApps).mockResolvedValue({ items: [], available: true, truncated: false, access: "not_readable", access_message: "Argo CD and Flux objects are installed but not readable with this connector's access." });
		vi.mocked(api.kubernetesRollouts).mockResolvedValue({ items: [], available: true, truncated: false, access: "not_readable", access_message: "Argo Rollouts are installed but not readable with this connector's access." });
		renderPage("gitops");
		expect(await screen.findByText("Argo CD and Flux objects are installed but not readable with this connector's access.")).toBeTruthy();
		expect(screen.getByText("Argo Rollouts are installed but not readable with this connector's access.")).toBeTruthy();
		expect(screen.queryByText("No GitOps applications were returned.")).toBeNull();
		expect(screen.queryByText("No Rollouts were returned.")).toBeNull();
		expect(screen.queryByText("Detected")).toBeNull();
	});
});

describe("mixed role", () => {
	it("shows the data with the gap in the top-bar details instead of blocking every tab", async () => {
		const ready = { state: "ready" as const, observed_at: observed, records: 4, partial: false };
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue({
			state: "ready", partial: false, age_s: 5, first_ready_at: published, kinds: { Pod: ready, Deployment: ready, Secret: { ...ready, records: 2 } },
			total_kinds: 3, ready_kinds: 3, failing_kinds: 0, pending_kinds: 0, truncated: false, cluster_wide: true,
			namespace_scoped: [{ kind: "Secret", state: "ready", class: "namespace_scope", records: 2 }], namespace_scoped_kinds: 1,
		});
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [{ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "shop", name: "web" }], counts: { Deployment: 1 }, truncated: false });
		const { container } = renderPage();
		expect(await screen.findByRole("region", { name: "Cluster health" })).toBeTruthy();
		expect(screen.queryByRole("heading", { name: /Cannot finish syncing this cluster/ })).toBeNull();
		expect(container.querySelector("[data-sync-health]")).toBeNull();
		expect(screen.queryByText(/Sync degraded/)).toBeNull();
		fireEvent.mouseEnter(await screen.findByRole("button", { name: "Resource types not fully shown: 1 readable only in some namespaces" }));
		const details = screen.getByRole("region", { name: "Sync status details" });
		expect(details.querySelector("[data-namespace-scoped]")?.textContent).toBe("Readable only in some namespaces: Secret");
		expect(details.querySelector("[data-unreadable-note]")).toBeNull();
		expect(details.outerHTML).not.toMatch(/sev-(warning|critical)/);
		fireEvent.click(screen.getByRole("tab", { name: "Workloads" }));
		expect(await screen.findByText("web")).toBeTruthy();
	});
});

describe("a kind failing in one namespace", () => {
	it("names the namespace in the degraded sync details", async () => {
		const ready = { state: "ready" as const, observed_at: observed, records: 4, partial: false };
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue({
			state: "partial", partial: true, age_s: 5, first_ready_at: published,
			kinds: { Deployment: ready, Pod: { ...ready, state: "partial", partial: true, error: "unavailable" } },
			failing: [{ kind: "Pod", state: "partial", class: "unavailable", records: 4, namespace: "payments" }],
			total_kinds: 2, ready_kinds: 1, failing_kinds: 1, pending_kinds: 0, truncated: false, cluster_wide: true,
		});
		renderPage();
		fireEvent.mouseEnter(await screen.findByRole("button", { name: /^Sync degraded/ }));
		const row = screen.getByRole("region", { name: "Sync status details" }).querySelector('[data-sync-problem="Pod"]');
		expect(row?.textContent).toContain("Pod: the cluster read failed in payments");
	});
});

describe("counts that are known for scoped readers", () => {
	const ready = { state: "ready" as const, observed_at: observed, records: 4, partial: false };
	const scopedIssues = { items: [], totals: {}, broken: { total: 0, by_severity: {} }, truncated: false, sync: { state: "partial", age_s: 0, partial: true }, partial_failures: [{ resource_id: "core~v1~secrets", class: "namespace_scope" }] };

	it("gives a mixed role a real broken count instead of Broken unknown", async () => {
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue({
			state: "ready", partial: false, age_s: 5, first_ready_at: published, kinds: { Pod: ready, Secret: { ...ready, records: 2 } },
			total_kinds: 2, ready_kinds: 2, failing_kinds: 0, pending_kinds: 0, truncated: false, cluster_wide: true,
			namespace_scoped: [{ kind: "Secret", state: "ready", class: "namespace_scope", records: 2 }], namespace_scoped_kinds: 1,
		});
		vi.mocked(api.kubernetesIssues).mockResolvedValue(scopedIssues);
		renderPage();
		const health = await screen.findByRole("region", { name: "Cluster health" });
		await waitFor(() => expect(within(health).getByRole("button", { name: "0 broken" })).toBeTruthy());
		expect(screen.queryByText("Broken unknown")).toBeNull();
		const card = screen.getByRole("region", { name: "Break timeline" });
		expect(within(card).queryByText("Unknown")).toBeNull();
	});

	it("keeps Broken unknown for a cluster-wide reader whose answer is genuinely partial", async () => {
		vi.mocked(api.kubernetesIssues).mockResolvedValue({ ...scopedIssues, partial_failures: [{ resource_id: "core~v1~pods", class: "timeout" }] });
		renderPage();
		const health = await screen.findByRole("region", { name: "Cluster health" });
		await waitFor(() => expect(within(health).getByRole("button", { name: "Broken unknown" })).toBeTruthy());
	});

	it("gives a namespace-only role real counts in the Broken tile and on Issues", async () => {
		namespaceOnly();
		vi.mocked(api.kubernetesIssues).mockResolvedValue({ ...scopedIssues, totals: { warning: 2 }, partial_failures: [{ resource_id: "core~v1~pods", class: "namespace_scope" }] });
		renderPage();
		const card = await screen.findByRole("region", { name: "Break timeline" });
		await waitFor(() => expect(within(card).getAllByText("0").length).toBeGreaterThan(0));
		expect(within(card).queryByText("Unknown")).toBeNull();
		cleanup();
		namespaceOnly();
		vi.mocked(api.kubernetesIssues).mockResolvedValue({ ...scopedIssues, totals: { warning: 2 }, partial_failures: [{ resource_id: "core~v1~pods", class: "namespace_scope" }] });
		renderPage("issues");
		expect(await screen.findByLabelText("warning count")).toBeTruthy();
		await waitFor(() => expect(screen.getByLabelText("warning count").textContent).toBe("2"));
		expect(screen.getByLabelText("critical count").textContent).toBe("0");
	});

	it("leaves a kind that is not collected out of the Topology counts", async () => {
		vi.mocked(api.kubernetesSyncHealth).mockResolvedValue({
			state: "ready", partial: false, age_s: 5, first_ready_at: published, kinds: { Pod: ready, ConfigMap: ready },
			total_kinds: 2, ready_kinds: 2, failing_kinds: 0, pending_kinds: 0, truncated: false, not_collected: ["Secret"],
		});
		renderPage();
		const topology = await screen.findByRole("region", { name: "Overview topology" });
		await waitFor(() => expect(within(topology).getByText("ConfigMap")).toBeTruthy());
		expect(within(topology).queryByText("Secret", { exact: true })).toBeNull();
	});
});
