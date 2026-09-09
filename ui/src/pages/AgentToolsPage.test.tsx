// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api, type AgentConfigView, type AgentToolsetAvailability } from "@/lib/api";
import { AgentToolsPage } from "./AgentToolsPage";

vi.mock("@/components/TopBar", () => ({ TopBar: ({ title }: { title: string }) => <header>{title}</header> }));
vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, getAgentConfig: vi.fn(), listAgentToolsets: vi.fn(), setAgentToolsetEnabled: vi.fn() } };
});

const rows: AgentToolsetAvailability[] = [
  { id: "kubernetes", section: "connector", display_name: "Kubernetes", description: "Inspect Kubernetes.", icon_key: "kubernetes", docs_url: "https://docs.versusincident.com/#/agent/tools/kubernetes", ui_path: "/agent/kubernetes", visibility: "always", state: "needs_integration", reason: "Kubernetes is not connected.", action: "/settings?tab=agent", action_label: "Connect Kubernetes", enabled: true, child_count: 9, requirement: { kind: "integration", integration: "kubernetes" } },
  { id: "source-control", section: "connector", display_name: "Source control", description: "Read recent changes.", icon_key: "git", docs_url: "https://docs.versusincident.com/#/agent/tools/recent-changes", visibility: "always", state: "needs_integration", reason: "GitHub is not connected.", action: "/settings?tab=agent", action_label: "Connect GitHub", enabled: true, child_count: 1, requirement: { kind: "integration", integration: "github" } },
  { id: "logs", section: "datasource", display_name: "Logs", description: "Read bounded logs.", icon_key: "logs", docs_url: "https://docs.versusincident.com/#/agent/data-sources", ui_path: "/agent/logs", visibility: "always", state: "available", reason: "Log tools are available.", action: "/settings?tab=agent", action_label: "Add a data source", enabled: true, child_count: 1, requirement: { kind: "datasource", signal_kind: "logs" } },
  { id: "elasticsearch-logs", section: "datasource", display_name: "Elasticsearch", description: "Discover mappings and shard health, and search bounded data in configured log indices.", icon_key: "elasticsearch", docs_url: "https://docs.versusincident.com/#/agent/data-sources", ui_path: "/agent/logs", visibility: "always", state: "available", reason: "Elasticsearch tools are available.", action: "/settings?tab=agent", action_label: "Add a data source", enabled: true, child_count: 4, requirement: { kind: "datasource", signal_kind: "elasticsearch" } },
  { id: "metrics", section: "datasource", display_name: "Metrics", description: "Summarize metrics.", icon_key: "metrics", docs_url: "https://docs.versusincident.com/#/agent/data-sources/prometheus", ui_path: "/agent/metrics", visibility: "always", state: "needs_license", reason: "Metric tools need an Enterprise source.", action: "https://versuscontrol.com/enterprise", action_label: "Learn more", enabled: true, child_count: 1, requirement: { kind: "datasource", signal_kind: "metrics" } },
  { id: "traces", section: "datasource", display_name: "Traces", description: "Inspect traces.", icon_key: "traces", docs_url: "https://docs.versusincident.com/#/agent/data-sources/traces", ui_path: "/agent/traces", visibility: "always", state: "available", reason: "Trace tools are available.", action: "/settings?tab=agent", action_label: "Add a data source", enabled: true, child_count: 1, requirement: { kind: "datasource", signal_kind: "traces" } },
  { id: "find_runbook", section: "common", display_name: "Find runbook", description: "Search runbooks.", icon_key: "runbook", docs_url: "https://docs.versusincident.com/#/agent/tools/find-runbook", ui_path: "/agent/runbooks", visibility: "always", state: "needs_capability", reason: "Runbook indexing is not configured.", action: "/admin#agent-ai-settings", action_label: "AI settings", enabled: true, child_count: 1, requirement: { kind: "capability" } },
  { id: "describe_dependencies", section: "common", display_name: "Describe dependencies", description: "Inspect dependencies.", icon_key: "dependencies", docs_url: "https://docs.versusincident.com/#/agent/tools/tools?id=describe_dependencies", visibility: "always", state: "disabled_by_operator", reason: "Not offered to the agent.", action: "", action_label: "", enabled: false, child_count: 1, requirement: { kind: "capability" } },
];

const config = {
  sources: [
    { name: "Application file", type: "file", enable: true },
    { name: "Archive file", type: "file", enable: true },
    { name: "Disabled Elastic", type: "elasticsearch", enable: false },
    { name: "Production Loki", type: "loki", enable: true },
    { name: "Prometheus primary", type: "prometheus", enable: true },
    { name: "CloudWatch production", type: "cloudwatch_metrics", enable: true },
    { name: "Tempo production", type: "traces", enable: true },
    { name: "Disabled SigNoz traces", type: "signoz_traces", enable: false },
  ],
} as AgentConfigView;

const providerDocs = {
  File: "https://docs.versusincident.com/#/agent/data-sources/file",
    Loki: "https://docs.versusincident.com/#/agent/data-sources/loki",
  "CloudWatch Logs": "https://docs.versusincident.com/#/agent/data-sources/cloudwatch-logs",
  Graylog: "https://docs.versusincident.com/#/agent/data-sources/graylog",
  Splunk: "https://docs.versusincident.com/#/agent/data-sources/splunk",
  "SigNoz Logs": "https://docs.versusincident.com/#/agent/data-sources/signoz",
  Prometheus: "https://docs.versusincident.com/#/agent/data-sources/prometheus",
  "CloudWatch Metrics": "https://docs.versusincident.com/#/agent/data-sources/cloudwatch-metrics",
  "SigNoz Metrics": "https://docs.versusincident.com/#/enterprise/metrics/signoz",
  "Grafana Tempo": "https://docs.versusincident.com/#/agent/data-sources/traces",
  "SigNoz Traces": "https://docs.versusincident.com/#/agent/data-sources/traces?id=signoz-backend",
};

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={client}><MemoryRouter><AgentToolsPage /></MemoryRouter></QueryClientProvider>);
}

beforeEach(() => {
  vi.mocked(api.getAgentConfig).mockResolvedValue(config);
  vi.mocked(api.listAgentToolsets).mockResolvedValue(rows);
  vi.mocked(api.setAgentToolsetEnabled).mockResolvedValue({ agent: "chat", id: "describe_dependencies", enabled: true, changed: true });
});

afterEach(() => { cleanup(); vi.clearAllMocks(); });

describe("AgentToolsPage", () => {
  it("renders one Elasticsearch datasource card with the dedicated asset", async () => {
    renderPage();
    const heading = await screen.findByRole("heading", { name: "Elasticsearch" });
    const card = heading.closest("article");
    const icon = card?.querySelector('img[src="/elasticsearch.svg"]');
    expect(icon?.getAttribute("width")).toBe("24");
    expect(icon?.getAttribute("height")).toBe("24");
    expect(icon?.getAttribute("aria-hidden")).toBe("true");
    expect(card?.querySelector("svg.tool-brand-icon")).toBeNull();
    expect(screen.getAllByRole("heading", { name: /Elasticsearch/ })).toHaveLength(1);

    const fileLogsCard = screen.getByRole("heading", { name: "File" }).closest("article");
    expect(fileLogsCard?.querySelector('img[src="/elasticsearch.svg"]')).toBeNull();
    expect(fileLogsCard?.querySelector("svg.tool-brand-icon")).toBeTruthy();
  });

  it("keeps the Elasticsearch asset off generic file logs", async () => {
    renderPage();
    const elasticsearchHeading = await screen.findByRole("heading", { name: "Elasticsearch" });
    expect(elasticsearchHeading.closest("article")?.querySelector('img[src="/elasticsearch.svg"]')).toBeTruthy();

    const fileLogsCard = screen.getByRole("heading", { name: "File" }).closest("article");
    expect(fileLogsCard?.querySelector('img[src="/elasticsearch.svg"]')).toBeNull();
    expect(fileLogsCard?.querySelector("svg.tool-brand-icon")).toBeTruthy();
  });

  it("renders one Elasticsearch card and hides the generic datasource owners", async () => {
    renderPage();
    expect(screen.getByLabelText("Loading tools")).toBeTruthy();
    await screen.findByText("Kubernetes");
    expect(screen.getAllByRole("article")).toHaveLength(16);
    expect(screen.getAllByRole("heading", { level: 2 }).map((heading) => heading.textContent)).toEqual(["Connectors", "Data Source Tools", "Common"]);
    const datasourceSection = document.querySelector('[aria-labelledby="tools-datasource"]') as HTMLElement;
    expect(within(datasourceSection).getAllByRole("article")).toHaveLength(12);
    expect(within(datasourceSection).queryByRole("heading", { name: "Logs" })).toBeNull();
    expect(within(datasourceSection).queryByRole("heading", { name: "Metrics" })).toBeNull();
    expect(within(datasourceSection).queryByRole("heading", { name: "Traces" })).toBeNull();
    expect(within(datasourceSection).getByRole("heading", { name: "Elasticsearch" })).toBeTruthy();
    for (const provider of Object.keys(providerDocs)) {
      expect(within(datasourceSection).getByRole("heading", { name: provider })).toBeTruthy();
    }
    expect(document.querySelectorAll(".md\\:grid-cols-2")).toHaveLength(3);
    expect(document.querySelectorAll(".xl\\:grid-cols-3")).toHaveLength(3);
    for (const card of screen.getAllByRole("article")) {
      expect(card.className).toContain("bg-transparent");
      expect(card.className).toContain("hover:bg-surface");
    }
    expect(document.querySelector(".tool-brand-kubernetes")).toBeTruthy();
    expect(document.querySelector(".tool-brand-git")).toBeTruthy();
    expect(screen.queryByRole("textbox", { name: "Search tools" })).toBeNull();
    expect(screen.queryByRole("tablist")).toBeNull();
    expect(screen.queryByText(/toolsets?$/)).toBeNull();
    expect(screen.queryByText(/^9 tools$/)).toBeNull();
    expect(screen.queryByText("get_cluster_overview")).toBeNull();
    expect(screen.queryByText("Inspect Kubernetes.")).toBeNull();
    expect(screen.queryByText("Metric tools need an Enterprise source.")).toBeNull();
    expect(screen.getAllByText("Connection needed").length).toBeGreaterThan(0);
    expect(screen.getAllByText("Enterprise")).toHaveLength(3);
    expect(screen.getByText("Off")).toBeTruthy();
    expect(screen.queryByRole("checkbox")).toBeNull();
  });

  it("matches exact enabled source types and gates inherited UI links", async () => {
    renderPage();
    await screen.findByText("Kubernetes");
    expect(screen.getAllByRole("link", { name: /^Open / }).map((link) => link.getAttribute("href"))).toEqual([
      "/agent/kubernetes", "/agent/logs", "/agent/logs", "/agent/logs", "/agent/traces", "/agent/runbooks",
    ]);
    expect(screen.getAllByRole("button", { name: / settings$/ })).toHaveLength(16);
    expect(screen.queryByRole("link", { name: /documentation/i })).toBeNull();
    expect(screen.getByText("Elasticsearch").closest("article")?.textContent).toContain("Ready");
    expect(screen.getByText("CloudWatch Logs").closest("article")?.querySelector('a[href="/agent/logs"]')).toBeNull();
    expect(screen.getByText("Prometheus").closest("article")?.querySelector('a[href="/agent/metrics"]')).toBeNull();
    expect(screen.getByText("SigNoz Traces").closest("article")?.querySelector('a[href="/agent/traces"]')).toBeNull();
  });

  it("keeps canonical Prometheus and Tempo cards usable for tool-only configuration", async () => {
    vi.mocked(api.getAgentConfig).mockResolvedValueOnce({ ...config, sources: config.sources.filter((source) => !["prometheus", "cloudwatch_metrics", "signoz_metrics", "traces", "signoz_traces"].includes(source.type)) });
    vi.mocked(api.listAgentToolsets).mockResolvedValueOnce(rows.map((row) =>
      row.id === "metrics" || row.id === "traces" ? { ...row, state: "available", reason: `${row.display_name} tools are available.` } : row,
    ));
    renderPage();
    await screen.findByText("Prometheus");
    expect(screen.getByText("Prometheus").closest("article")?.textContent).toContain("Configured");
    expect(screen.getByRole("link", { name: "Open Prometheus" }).getAttribute("href")).toBe("/agent/metrics");
    expect(screen.getByRole("link", { name: "Open Grafana Tempo" }).getAttribute("href")).toBe("/agent/traces");
    fireEvent.click(screen.getByRole("button", { name: "Prometheus settings" }));
    const dialog = screen.getByRole("dialog", { name: "Prometheus" });
    expect(dialog.textContent).toContain("configured outside agent sources");
    expect((within(dialog).getByRole("checkbox") as HTMLInputElement).disabled).toBe(false);
  });

  it("does not infer Prometheus from a configured sibling metrics provider", async () => {
    vi.mocked(api.getAgentConfig).mockResolvedValueOnce({ ...config, sources: config.sources.filter((source) => source.type !== "prometheus" && source.type !== "traces") });
    vi.mocked(api.listAgentToolsets).mockResolvedValueOnce(rows.map((row) =>
      row.id === "metrics" || row.id === "traces" ? { ...row, state: "available", reason: `${row.display_name} tools are available.` } : row,
    ));
    renderPage();
    await screen.findByText("Prometheus");
    expect(screen.getByText("Prometheus").closest("article")?.textContent).toContain("Data source needed");
    expect(screen.queryByRole("link", { name: "Open Prometheus" })).toBeNull();
  });

  it("surfaces shared unhealthy state without enabling provider toggles", async () => {
    vi.mocked(api.listAgentToolsets).mockResolvedValueOnce(rows.map((row) =>
      row.id === "logs" ? { ...row, state: "unhealthy", reason: "Log reader is unhealthy.", health: "backend unavailable" } : row,
    ));
    renderPage();
    await screen.findByText("Loki");
    expect(screen.getByText("Loki").closest("article")?.textContent).toContain("Unhealthy");
    fireEvent.click(screen.getByRole("button", { name: "Loki settings" }));
    const dialog = screen.getByRole("dialog", { name: "Loki" });
    expect(dialog.textContent).toContain("shared Logs agent capability is unhealthy");
    expect(dialog.textContent).toContain("backend unavailable");
    expect((within(dialog).getByRole("checkbox") as HTMLInputElement).disabled).toBe(true);
  });

  it("does not infer canonical providers from an unhealthy shared capability", async () => {
    vi.mocked(api.getAgentConfig).mockResolvedValueOnce({ ...config, sources: config.sources.filter((source) => source.type !== "prometheus") });
    vi.mocked(api.listAgentToolsets).mockResolvedValueOnce(rows.map((row) =>
      row.id === "metrics" ? { ...row, state: "unhealthy", reason: "Metric reader configuration is invalid.", health: "configuration" } : row,
    ));
    renderPage();
    await screen.findByText("Prometheus");
    expect(screen.getByText("Prometheus").closest("article")?.textContent).toContain("Data source needed");
    expect(screen.queryByRole("link", { name: "Open Prometheus" })).toBeNull();
  });

  it("names and explains shared policy controls", async () => {
    renderPage();
    await screen.findByText("File");
    fireEvent.click(screen.getByRole("button", { name: "File settings" }));
    const dialog = screen.getByRole("dialog", { name: "File" });
    expect(within(dialog).getByRole("checkbox", { name: "Enable Logs for chat" })).toBeTruthy();
    expect(dialog.textContent).toContain("This setting affects every configured logs provider.");
  });

  it("keeps permission-blocked tools visible without exposing their internal page", async () => {
    vi.mocked(api.listAgentToolsets).mockResolvedValueOnce(rows.map((row) =>
      row.id === "kubernetes"
        ? { ...row, state: "needs_permission", reason: "Kubernetes access is not permitted." }
        : row,
    ));
    renderPage();
    const row = (await screen.findByText("Kubernetes")).closest("article");
    expect(row?.querySelector('a[href="/agent/kubernetes"]')).toBeNull();
    expect(within(row as HTMLElement).getByRole("button", { name: "Kubernetes settings" })).toBeTruthy();
  });

  it("moves availability, docs, setup, and enablement into settings", async () => {
    renderPage();
    await screen.findByText("Kubernetes");
    fireEvent.click(screen.getByRole("button", { name: "Kubernetes settings" }));
    const dialog = screen.getByRole("dialog", { name: "Kubernetes" });
    expect(dialog.textContent).toContain("Connection needed");
    expect(dialog.textContent).toContain("Kubernetes is not connected.");
    expect(dialog.textContent).toContain("Chat agent");
    expect(within(dialog).getByRole("link", { name: /Documentation/ }).getAttribute("href")).toBe("https://docs.versusincident.com/#/agent/tools/kubernetes");
    expect(within(dialog).getByRole("link", { name: "Connect Kubernetes" }).getAttribute("href")).toBe("/settings?tab=agent");
    fireEvent.click(screen.getByRole("button", { name: "Close dialog" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("keeps Enterprise authoritative and allows its external Learn more action", async () => {
    renderPage();
    await screen.findByText("Prometheus");
    fireEvent.click(screen.getByRole("button", { name: "Prometheus settings" }));
    const dialog = screen.getByRole("dialog", { name: "Prometheus" });
    const checkbox = within(dialog).getByRole("checkbox") as HTMLInputElement;
    expect(checkbox.disabled).toBe(true);
    fireEvent.click(checkbox);
    expect(api.setAgentToolsetEnabled).not.toHaveBeenCalled();
    const documentation = within(dialog).getByRole("link", { name: "Documentation" });
    expect(documentation.getAttribute("target")).toBe("_blank");
    expect(documentation.getAttribute("rel")).toBe("noopener noreferrer");
    expect(within(dialog).getByRole("link", { name: /Learn more/ }).getAttribute("href")).toBe("https://versuscontrol.com/enterprise");
  });

  it("shows provider details, configured names, shared capability, and exact docs without internal setup links", async () => {
    renderPage();
    await screen.findByText("File");
    fireEvent.click(screen.getByRole("button", { name: "Source control settings" }));
    let dialog = screen.getByRole("dialog", { name: "Source control" });
    expect(within(dialog).queryByRole("link", { name: "Connect GitHub" })).toBeNull();
    expect(within(dialog).getByRole("link", { name: "Documentation" })).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "Close dialog" }));

    fireEvent.click(screen.getByRole("button", { name: "File settings" }));
    dialog = screen.getByRole("dialog", { name: "File" });
    expect(within(dialog).queryByRole("link", { name: "Add a data source" })).toBeNull();
    expect(dialog.textContent).toContain("Read logs from local files.");
    expect(dialog.textContent).toContain("Uses the shared Logs agent capability.");
    expect(dialog.textContent).toContain("Application file, Archive file");
    expect(within(dialog).getByRole("link", { name: "Documentation" }).getAttribute("href")).toBe(providerDocs.File);
  });

  it("uses provider documentation URLs for every datasource settings dialog", async () => {
    renderPage();
    await screen.findByText("File");
    for (const [provider, docsURL] of Object.entries(providerDocs)) {
      fireEvent.click(screen.getByRole("button", { name: `${provider} settings` }));
      const dialog = screen.getByRole("dialog", { name: provider });
      expect(within(dialog).getByRole("link", { name: "Documentation" }).getAttribute("href")).toBe(docsURL);
      expect(within(dialog).queryByRole("link", { name: "Add a data source" })).toBeNull();
      fireEvent.click(within(dialog).getByRole("button", { name: "Close dialog" }));
      await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    }
  });

  it("toggles configured providers through the shared base policy ID", async () => {
    renderPage();
    await screen.findByText("File");
    fireEvent.click(screen.getByRole("button", { name: "File settings" }));
    fireEvent.click(within(screen.getByRole("dialog", { name: "File" })).getByRole("checkbox"));
    await waitFor(() => expect(api.setAgentToolsetEnabled).toHaveBeenCalledWith("chat", "logs", false));
  });

  it("uses the server-owned Elasticsearch state, reason, and policy toggle", async () => {
    renderPage();
    await screen.findByText("Elasticsearch");
    fireEvent.click(screen.getByRole("button", { name: "Elasticsearch settings" }));
    const dialog = screen.getByRole("dialog", { name: "Elasticsearch" });
    const checkbox = within(dialog).getByRole("checkbox", { name: "Enable Elasticsearch for chat" }) as HTMLInputElement;
    expect(checkbox.disabled).toBe(false);
    expect(dialog.textContent).toContain("Elasticsearch tools are available.");
    expect(dialog.textContent).toContain("Discover mappings and shard health");
    fireEvent.click(checkbox);
    await waitFor(() => expect(api.setAgentToolsetEnabled).toHaveBeenCalledWith("chat", "elasticsearch-logs", false));
  });

  it("uses the single Elasticsearch card for an Elasticsearch-only config", async () => {
    vi.mocked(api.getAgentConfig).mockResolvedValueOnce({
      ...config,
      sources: [{ name: "Production Elasticsearch", type: "elasticsearch", enable: true }],
    });
    renderPage();

    const dedicatedCard = (await screen.findByRole("heading", { name: "Elasticsearch" })).closest("article") as HTMLElement;
    expect(dedicatedCard.textContent).toContain("Ready");

    fireEvent.click(within(dedicatedCard).getByRole("button", { name: "Elasticsearch settings" }));
    const dialog = screen.getByRole("dialog", { name: "Elasticsearch" });
    const dedicatedToggle = within(dialog).getByRole("checkbox", { name: "Enable Elasticsearch for chat" }) as HTMLInputElement;
    expect(dedicatedToggle.disabled).toBe(false);
    expect(dedicatedToggle.checked).toBe(true);
    expect(dialog.textContent).toContain("Discover mappings and shard health");
    fireEvent.click(dedicatedToggle);
    await waitFor(() => expect(api.setAgentToolsetEnabled).toHaveBeenCalledWith("chat", "elasticsearch-logs", false));
  });

  it("keeps the server-owned Elasticsearch permission state authoritative", async () => {
    vi.mocked(api.listAgentToolsets).mockResolvedValueOnce(rows.map((row) =>
      row.id === "elasticsearch-logs"
        ? { ...row, state: "needs_permission", reason: "Elasticsearch access is not permitted." }
        : row,
    ));
    renderPage();
    await screen.findByText("Elasticsearch");
    expect(screen.getByText("Elasticsearch").closest("article")?.textContent).toContain("No access");
    expect(screen.queryByRole("link", { name: "Open Elasticsearch" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Elasticsearch settings" }));
    const dialog = screen.getByRole("dialog", { name: "Elasticsearch" });
    expect(dialog.textContent).toContain("Elasticsearch access is not permitted.");
    expect((within(dialog).getByRole("checkbox") as HTMLInputElement).disabled).toBe(true);
  });

  it("uses neutral shared status when source configuration cannot be queried", async () => {
    vi.mocked(api.getAgentConfig).mockRejectedValueOnce(new Error("config unavailable"));
    renderPage();
    await screen.findByText("File");
    expect(screen.getAllByText("Shared status unknown")).toHaveLength(8);
    expect(screen.getAllByText("Enterprise")).toHaveLength(3);
    expect(screen.getByText("Elasticsearch").closest("article")?.textContent).toContain("Ready");
    expect(screen.getByText("File").closest("article")?.querySelector('a[href="/agent/logs"]')).toBeNull();
    expect(screen.getByText("Grafana Tempo").closest("article")?.querySelector('a[href="/agent/traces"]')).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "File settings" }));
    const dialog = screen.getByRole("dialog", { name: "File" });
    expect(dialog.textContent).toContain("Provider configuration is unavailable.");
    expect((within(dialog).getByRole("checkbox") as HTMLInputElement).disabled).toBe(true);
  });

  it("can re-enable an operator-disabled tool from settings", async () => {
    renderPage();
    await screen.findByText("Kubernetes");
    fireEvent.click(screen.getByRole("button", { name: "Describe dependencies settings" }));
    fireEvent.click(within(screen.getByRole("dialog", { name: "Describe dependencies" })).getByRole("checkbox"));
    await waitFor(() => expect(api.setAgentToolsetEnabled).toHaveBeenCalledWith("chat", "describe_dependencies", true));
  });

  it("toggles independently per selected agent and refetches authoritative state", async () => {
    renderPage();
    await screen.findByText("Kubernetes");
    fireEvent.click(screen.getByRole("button", { name: "Describe dependencies settings" }));
    fireEvent.click(within(screen.getByRole("dialog", { name: "Describe dependencies" })).getByRole("checkbox"));
    await waitFor(() => expect(api.setAgentToolsetEnabled).toHaveBeenCalledWith("chat", "describe_dependencies", true));
    fireEvent.click(screen.getByRole("button", { name: "Close dialog" }));
    fireEvent.click(screen.getByRole("button", { name: "analyze" }));
    await waitFor(() => expect(api.listAgentToolsets).toHaveBeenCalledWith("analyze"));
    fireEvent.click(await screen.findByRole("button", { name: "Describe dependencies settings" }));
    expect(within(screen.getByRole("dialog", { name: "Describe dependencies" })).getByText("Analyze agent")).toBeTruthy();
  });

  it("keeps a rejected mutation actionable and dismissible", async () => {
    vi.mocked(api.setAgentToolsetEnabled).mockRejectedValueOnce(new Error("Requirement is not satisfied"));
    renderPage();
    await screen.findByText("Kubernetes");
    fireEvent.click(screen.getByRole("button", { name: "Describe dependencies settings" }));
    fireEvent.click(within(screen.getByRole("dialog", { name: "Describe dependencies" })).getByRole("checkbox"));
    expect((await screen.findByRole("alert")).textContent).toContain("Requirement is not satisfied");
    fireEvent.click(screen.getByRole("button", { name: /Dismiss/ }));
    await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
  });

  it("renders recoverable error and empty states", async () => {
    vi.mocked(api.listAgentToolsets).mockRejectedValue(new Error("offline"));
    const first = renderPage();
    expect(await screen.findByText(/Couldn't load agent tools/)).toBeTruthy();
    first.unmount();
    vi.mocked(api.listAgentToolsets).mockResolvedValueOnce([]);
    renderPage();
    expect(await screen.findByText("No tools are known to this build.")).toBeTruthy();
  });
});