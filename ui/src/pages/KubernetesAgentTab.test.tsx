// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api, ApiError, type KubernetesAgentSettings, type KubernetesLogSourceType } from "@/lib/api";
import { KubernetesClusterContext, kubernetesKey } from "@/lib/useKubernetesCluster";
import { KubernetesAgentTab } from "./KubernetesAgentTab";

const types: KubernetesLogSourceType[] = ["loki", "elasticsearch", "cloudwatchlogs", "graylog", "splunk", "signoz"];
const settings: KubernetesAgentSettings = {
  learning: { logs: true, metrics: false, traces: true, scope: { include: [], exclude: [] } },
  break_logs: { linked_source: "source-loki", fields: {
    loki: { namespace: "namespace", pod: "pod", container: "container" },
    elasticsearch: { namespace: "kubernetes.namespace_name", pod: "kubernetes.pod_name", container: "kubernetes.container_name", time: "@timestamp", message: "message" },
    cloudwatchlogs: { namespace: "kubernetes.namespace_name", pod: "kubernetes.pod_name", container: "kubernetes.container_name", message: "log" },
    graylog: { namespace: "kubernetes_namespace_name", pod: "kubernetes_pod_name", container: "kubernetes_container_name" },
    splunk: { namespace: "namespace", pod: "pod", container: "container_name" },
    signoz: { namespace: "k8s.namespace.name", pod: "k8s.pod.name", container: "k8s.container.name" },
  } },
  sources: types.map((type, index) => ({ name: `source-${type}`, type, scope: `configured ${type} scope`, health: index === 0 ? "healthy" : index === 1 ? "not_checked" : index === 2 ? "failing" : "slow" })),
  detected: { metrics_server: true, traces: { kind: "tempo", namespace: "telemetry" }, workloads: 8, namespaces: 2 },
  can_write: true, linked_source_missing: false,
};

function mount(value = settings) {
  const get = vi.spyOn(api, "kubernetesAgentSettings").mockResolvedValue(value);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(<QueryClientProvider client={client}><KubernetesClusterContext.Provider value={{ client: api, cluster: "east", multiple: true, clusters: [], key: (...parts) => kubernetesKey("east", ...parts) }}><MemoryRouter><KubernetesAgentTab /></MemoryRouter></KubernetesClusterContext.Provider></QueryClientProvider>);
  return { get, client };
}
afterEach(() => { cleanup(); vi.restoreAllMocks(); });

describe("Kubernetes Agent settings", () => {
  it("locks learning and scope while preserving persisted values on log-source saves", async () => {
    const persisted = { ...settings, learning: { ...settings.learning, scope: { include: ["shop"], exclude: ["sandbox"] } } };
    const save = vi.spyOn(api, "kubernetesSaveAgentSettings").mockImplementation(async (input) => ({ ...settings, ...input }));
    mount(persisted);
    await screen.findByText("8 workloads in 2 namespaces");
    expect(screen.getByText("Not available yet.")).toBeTruthy();
    expect(screen.getByText("metrics-server found")).toBeTruthy();
    expect(screen.getByText("tempo found in telemetry")).toBeTruthy();
    for (const control of screen.getAllByRole("switch")) expect((control as HTMLInputElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Edit scope" }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Save settings" }) as HTMLButtonElement).disabled).toBe(true);
    const learning = screen.getByRole("region", { name: "Agent learning" });
    const capture = screen.getByRole("region", { name: "Log capture at break" });
    expect(capture.compareDocumentPosition(learning) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(capture.textContent).not.toMatch(/KiB|MiB|500 lines|containers per pod/);
    fireEvent.change(screen.getByLabelText("Namespace label"), { target: { value: "custom_namespace" } });
    fireEvent.click(screen.getByRole("button", { name: "Save settings" }));
    await screen.findByText("Agent settings saved.");
    expect(save.mock.calls[0][0].learning).toEqual(persisted.learning);
    expect(save.mock.calls[0][0]).not.toHaveProperty("sources");
    expect(save.mock.calls[0][0].break_logs.fields.loki.namespace).toBe("custom_namespace");
  });

  it("shows saved values without inputs or write/test requests for a read-only caller", async () => {
    const save = vi.spyOn(api, "kubernetesSaveAgentSettings");
    const test = vi.spyOn(api, "kubernetesTestLink");
    mount({ ...settings, can_write: false });
    await screen.findByText("Read only. An administrator can change these settings.");
    expect(screen.getAllByRole("switch").every((control) => (control as HTMLInputElement).disabled)).toBe(true);
    expect(screen.queryByRole("textbox")).toBeNull();
    expect(screen.queryByRole("button", { name: "Test link" })).toBeNull();
    expect(save).not.toHaveBeenCalled(); expect(test).not.toHaveBeenCalled();
  });

  it.each(types)("renders and saves the %s mapping keyed by type", async (type) => {
    const save = vi.spyOn(api, "kubernetesSaveAgentSettings").mockImplementation(async (input) => ({ ...settings, ...input }));
    const test = vi.spyOn(api, "kubernetesTestLink").mockResolvedValue({ lines: 12, namespace: "shop", workload: "api", duration_ms: 4800, preview: JSON.stringify([{ type, scope: "configured", query: "actual bounded query", from: "2026-10-17T12:00:00Z", to: "2026-10-17T12:15:00Z", limit: 500 }]), truncated: true });
    mount();
    await screen.findByRole("button", { name: "source-loki" });
    fireEvent.click(screen.getByRole("button", { name: "source-loki" }));
    const picker = screen.getByLabelText("Configured log sources");
    expect(within(picker).getAllByRole("region")).toHaveLength(6);
    expect(within(picker).getByText("not checked yet")).toBeTruthy();
    expect(within(picker).getAllByText("slow")).toHaveLength(3);
    fireEvent.click(within(picker).getByRole("button", { name: new RegExp(`^source-${type}`) }));
    const form = screen.getByRole("region", { name: new RegExp(" mapping$") });
    expect(within(form).getAllByRole("textbox")).toHaveLength(type === "elasticsearch" ? 5 : type === "cloudwatchlogs" ? 4 : 3);
    if (type === "cloudwatchlogs") expect(within(form).getByText(/FilterLogEvents preserves/)).toBeTruthy();
    fireEvent.click(within(form).getByRole("button", { name: "Test link" }));
    await within(form).findByText("Found 12 lines");
    expect(test).toHaveBeenCalledWith(`source-${type}`, settings.break_logs.fields[type]);
    expect(form.textContent).toContain("actual bounded query");
    expect(form.textContent).toContain("results truncated, coverage remains incomplete");
    fireEvent.change(within(form).getAllByRole("textbox")[0], { target: { value: "custom_namespace" } });
    await waitFor(() => expect(form.textContent).not.toContain("actual bounded query"));
    fireEvent.click(within(form).getByRole("button", { name: type === "loki" ? "Save" : "Use this source" }));
    await waitFor(() => expect(save).toHaveBeenCalledTimes(1));
    expect(save.mock.calls[0][0].break_logs.linked_source).toBe(`source-${type}`);
    expect(save.mock.calls[0][0].break_logs.fields[type].namespace).toBe("custom_namespace");
  });

  it.each(["unauthorized", "not_found", "timeout", "invalid_mapping"])("renders a safe %s test-link result and Retry", async (code) => {
    vi.spyOn(api, "kubernetesTestLink").mockRejectedValue(new ApiError(502, "private provider response", { error: code }));
    mount(); await screen.findByRole("button", { name: "Test link" });
    fireEvent.click(screen.getByRole("button", { name: "Test link" }));
    await screen.findByRole("alert");
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
    expect(document.body.textContent).not.toContain("private provider response");
  });

  it("keeps zero lines neutral and permits unlinking a missing source", async () => {
    const test = vi.spyOn(api, "kubernetesTestLink").mockResolvedValue({ lines: 0, namespace: "shop", workload: "api", duration_ms: 20, preview: "[]", truncated: false });
    const save = vi.spyOn(api, "kubernetesSaveAgentSettings").mockResolvedValue(settings);
    mount({ ...settings, linked_source_missing: true });
    fireEvent.click(await screen.findByRole("button", { name: "Test link" }));
    await screen.findByText("Found 0 lines. Check the field names.");
    expect(test).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByRole("button", { name: "source-loki" }));
    fireEvent.click(screen.getByRole("button", { name: /None Kubernetes container logs only/ }));
    fireEvent.click(screen.getByRole("button", { name: "Save settings" }));
    await waitFor(() => expect(save.mock.calls[0][0].break_logs.linked_source).toBe(""));
  });

  it("renders the no-source state without exposing an editable learning scope", async () => {
    mount({ ...settings, sources: [], break_logs: { ...settings.break_logs, linked_source: "" } });
    await screen.findByText("No log data source is configured");
    expect(screen.getByRole("button", { name: "None available" }).hasAttribute("disabled")).toBe(true);
    expect(screen.getByRole("link", { name: "Read the data sources guide" }).getAttribute("target")).toBe("_blank");
    fireEvent.click(screen.getByRole("button", { name: "Edit scope" }));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.queryByLabelText("Include namespaces")).toBeNull();
  });
});