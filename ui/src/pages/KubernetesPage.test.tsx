// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "@/lib/api";
import { KubernetesPage } from "./KubernetesPage";

vi.mock("@/components/TopBar", () => ({ TopBar: ({ title }: { title: string }) => <header>{title}</header> }));
vi.mock("@/lib/api", async (importActual) => { const actual = await importActual<typeof import("@/lib/api")>(); return { ...actual, api: { ...actual.api, kubernetesOverview: vi.fn(), kubernetesUsage: vi.fn(), kubernetesWorkloads: vi.fn(), kubernetesWorkload: vi.fn(), kubernetesTopology: vi.fn(), kubernetesSearch: vi.fn(), kubernetesEvents: vi.fn(), kubernetesDescribe: vi.fn() } }; });

function renderPage() { const client = new QueryClient({ defaultOptions: { queries: { retry: false } } }); return render(<QueryClientProvider client={client}><KubernetesPage /></QueryClientProvider>); }

beforeEach(() => {
	vi.mocked(api.kubernetesOverview).mockResolvedValue({ connector: "kubernetes", cluster_id: "production", observed_at: "2026-08-30T12:00:00Z", nodes: 3, ready_nodes: 2, pods: 18, running_pods: 16, namespaces: 5, active_namespaces: 5, workloads: 7, warnings: 2, usage_source: "unavailable", metrics_status: "unavailable", metrics_fresh: false, truncated: false, partial_failures: [{ resource_id: "core~v1~nodes", class: "forbidden" }] });
	vi.mocked(api.kubernetesUsage).mockResolvedValue({ observed_at: "2026-08-30T12:00:00Z", availability: "unavailable", fresh: false, truncated: false });
	vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: [], truncated: false });
	vi.mocked(api.kubernetesWorkload).mockResolvedValue({ resource_id: "core~v1~pods", kind: "Pod", namespace: "default", name: "api", truncated: false });
	vi.mocked(api.kubernetesTopology).mockResolvedValue({ nodes: [{ id: "pod:default:api", kind: "Pod", namespace: "default", name: "api" }], edges: [{ from: "node::a", to: "pod:default:api", type: "scheduled_on" }], node_cap: 200, edge_cap: 400, truncated: false });
	vi.mocked(api.kubernetesSearch).mockResolvedValue({ items: [], truncated: false });
	vi.mocked(api.kubernetesEvents).mockResolvedValue({ items: [], truncated: false });
	vi.mocked(api.kubernetesDescribe).mockResolvedValue({ resource: { resource_id: "core~v1~pods", kind: "Pod", namespace: "default", name: "api" } });
});
afterEach(() => { cleanup(); vi.clearAllMocks(); });

describe("KubernetesPage", () => {
	it("renders health, partial visibility, and keyboard-accessible topology", async () => {
		renderPage();
		expect(screen.getByLabelText("Loading Kubernetes overview")).toBeTruthy();
		await screen.findByText(/production/);
		expect(screen.getByText("Nodes ready 2/3")).toBeTruthy();
		expect(screen.getByText(/source unavailable/)).toBeTruthy();
		expect(screen.getAllByRole("status").some((status) => status.textContent?.includes("forbidden"))).toBe(true);
		const topology = await screen.findByLabelText("Kubernetes topology graph");
		expect(topology.getAttribute("tabindex")).toBe("0");
		expect(screen.getByRole("listitem", { name: /scheduled_on.*observed/ })).toBeTruthy();
	});

	it("searches within the selected namespace and renders empty state", async () => {
		renderPage(); await screen.findByText(/production/);
		fireEvent.change(screen.getByLabelText("Namespace"), { target: { value: "payments" } });
		expect(api.kubernetesUsage).toHaveBeenCalledTimes(1);
		await waitFor(() => expect(api.kubernetesUsage).toHaveBeenCalledWith("payments"));
		fireEvent.change(screen.getByLabelText("Resource name"), { target: { value: "api" } });
		fireEvent.click(screen.getByRole("button", { name: "Search" }));
		await waitFor(() => expect(api.kubernetesSearch).toHaveBeenCalledWith("payments", "api"));
		expect(await screen.findByText("No matching resources.")).toBeTruthy();
	});

	it("renders bounded usage freshness and sample counts", async () => {
		vi.mocked(api.kubernetesUsage).mockResolvedValue({ observed_at: "2026-08-30T12:00:00Z", availability: "stale", fresh: false, pods: [{ kind: "Pod", namespace: "default", name: "api" }], nodes: [{ kind: "Node", name: "node-a" }], truncated: true });
		renderPage();
		expect(await screen.findByText("Usage stale · 1 pod samples · 1 node samples · partial")).toBeTruthy();
	});

	it("renders raw limited-RBAC nullable responses without crashing", async () => {
		vi.mocked(api.kubernetesOverview).mockResolvedValue({ connector: "kubernetes", cluster_id: "limited", observed_at: "2026-08-30T12:00:00Z", nodes: 1, ready_nodes: 1, pods: 1, running_pods: 1, namespaces: 0, active_namespaces: 0, workloads: 1, warnings: 0, usage_source: null, metrics_status: null, metrics_fresh: false, truncated: true, omitted_categories: null, partial_failures: [{ resource_id: "core~v1~events", class: "forbidden" }] });
		vi.mocked(api.kubernetesUsage).mockResolvedValue({ observed_at: "2026-08-30T12:00:00Z", availability: "unavailable", fresh: false, pod_metrics: null as never, node_metrics: null as never, pods: null, nodes: null, truncated: true, omitted_categories: null, partial_failures: [{ class: "forbidden" }] });
		vi.mocked(api.kubernetesWorkloads).mockResolvedValue({ items: null, truncated: true, omitted_categories: null, partial_failures: [{ class: "forbidden" }] });
		vi.mocked(api.kubernetesTopology).mockResolvedValue({ nodes: null, edges: null, node_cap: 20, edge_cap: 20, truncated: true, omitted_categories: ["core~v1~events"], partial_failures: [{ class: "forbidden" }] });
		vi.mocked(api.kubernetesEvents).mockResolvedValue({ items: null, truncated: true, partial_failures: [{ class: "forbidden" }] });
		renderPage();
		expect(await screen.findByText(/kubernetes · limited/)).toBeTruthy();
		expect(screen.getByText("No workloads in this scope.")).toBeTruthy();
		expect(screen.getByText("No recent warning events.")).toBeTruthy();
		expect(screen.getByText("No topology edges in this scope.")).toBeTruthy();
		expect(screen.getAllByRole("status").some((status) => status.textContent?.includes("forbidden"))).toBe(true);
		expect(screen.queryByText("Couldn't render this page")).toBeNull();
	});
});