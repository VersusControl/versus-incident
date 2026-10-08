// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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
  { id: "kubernetes", section: "connector", display_name: "Kubernetes", description: "Inspect Kubernetes.", icon_key: "kubernetes", docs_url: "https://docs.versusincident.com/#/agent/connectors/kubernetes", ui_path: "/agent/kubernetes", visibility: "always", state: "needs_integration", reason: "Kubernetes is not connected.", action: "/settings?tab=agent", action_label: "Connect Kubernetes", enabled: true, child_count: 9, requirement: { kind: "integration", integration: "kubernetes" } },
  { id: "source-control", section: "connector", display_name: "Source control", description: "Read recent changes.", icon_key: "git", docs_url: "https://docs.versusincident.com/#/agent/tools/recent-changes", visibility: "always", state: "needs_integration", reason: "GitHub is not connected.", action: "/settings?tab=agent", action_label: "Connect GitHub", enabled: true, child_count: 1, requirement: { kind: "integration", integration: "github" } },
  { id: "logs", section: "datasource", display_name: "Logs", description: "Read bounded logs.", icon_key: "logs", docs_url: "https://docs.versusincident.com/#/agent/data-sources", ui_path: "/agent/logs", visibility: "always", state: "available", reason: "Log tools are available.", action: "/settings?tab=agent", action_label: "Add a data source", enabled: true, child_count: 1, requirement: { kind: "datasource", signal_kind: "logs" } },
  { id: "elasticsearch-logs", section: "datasource", display_name: "Elasticsearch", description: "Discover mappings and shard health, and search bounded data in configured log indices.", icon_key: "elasticsearch", docs_url: "https://docs.versusincident.com/#/agent/data-sources", ui_path: "/agent/logs", visibility: "always", state: "available", reason: "Elasticsearch tools are available.", action: "/settings?tab=agent", action_label: "Add a data source", enabled: true, child_count: 4, requirement: { kind: "datasource", signal_kind: "elasticsearch" } },
  { id: "metrics", section: "datasource", display_name: "Metrics", description: "Summarize metrics.", icon_key: "metrics", docs_url: "https://docs.versusincident.com/#/agent/data-sources/prometheus", ui_path: "/agent/metrics", visibility: "always", state: "needs_license", reason: "Metric tools need an Enterprise source.", action: "https://versuscontrol.com/enterprise", action_label: "Learn more", enabled: true, child_count: 1, requirement: { kind: "datasource", signal_kind: "metrics" } },
  { id: "traces", section: "datasource", display_name: "Traces", description: "Inspect traces.", icon_key: "traces", docs_url: "https://docs.versusincident.com/#/agent/data-sources/traces", ui_path: "/agent/traces", visibility: "always", state: "available", reason: "Trace tools are available.", action: "/settings?tab=agent", action_label: "Add a data source", enabled: true, child_count: 1, requirement: { kind: "datasource", signal_kind: "traces" } },
  { id: "find_runbook", section: "common", display_name: "Find runbook", description: "Search runbooks.", icon_key: "runbook", docs_url: "https://docs.versusincident.com/#/agent/tools/find-runbook", ui_path: "/agent/runbooks", visibility: "always", state: "needs_capability", reason: "Runbook indexing is not configured.", action: "/admin#agent-ai-settings", action_label: "AI settings", enabled: true, child_count: 1, requirement: { kind: "capability" } },
  { id: "describe_dependencies", section: "common", display_name: "Describe dependencies", description: "Inspect dependencies.", icon_key: "dependencies", docs_url: "https://docs.versusincident.com/#/agent/tools/overview?id=describe_dependencies", visibility: "always", state: "disabled_by_operator", reason: "Not offered to the agent.", action: "", action_label: "", enabled: false, child_count: 1, requirement: { kind: "capability" } },
  { id: "describe_baseline", section: "common", display_name: "Describe baseline", description: "Inspect bounded learned expectations for a service signal.", icon_key: "activity", docs_url: "https://docs.versusincident.com/#/agent/tools/overview?id=describe_baseline", visibility: "always", state: "needs_capability", reason: "Baseline provider is not configured.", action: "", action_label: "", enabled: true, child_count: 1, requirement: { kind: "capability", capabilities: ["baseline_provider"] }, permission: "infrastructure:view" },
];

const config = {
  sources: [
    { name: "Application file", type: "file", enable: true },
    { name: "Archive file", type: "file", enable: true },
    { name: "Disabled Elastic", type: "elasticsearch", enable: false },
    { name: "Production Loki", type: "loki", enable: true },
    { name: "CloudWatch application logs", type: "cloudwatchlogs", enable: true },
    { name: "Graylog production", type: "graylog", enable: true },
    { name: "Splunk production", type: "splunk", enable: true },
    { name: "Prometheus primary", type: "prometheus", enable: true },
    { name: "CloudWatch production", type: "cloudwatch_metrics", enable: true },
    { name: "Tempo production", type: "traces", enable: true },
    { name: "Disabled SigNoz traces", type: "signoz_traces", enable: false },
  ],
} as AgentConfigView;

const providerDocs = {
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
const providerStates = {
  Loki: "Configured",
  "CloudWatch Logs": "Configured",
  Graylog: "Configured",
  Splunk: "Configured",
  "SigNoz Logs": "Data source needed",
  Prometheus: "Enterprise",
  "CloudWatch Metrics": "Enterprise",
  "SigNoz Metrics": "Enterprise",
  "Grafana Tempo": "Configured",
  "SigNoz Traces": "Data source needed",
} as const;
const providerLogos = {
  Elasticsearch: "/elasticsearch.svg",
  Loki: "/loki.svg",
  Graylog: "/graylog.svg",
  Splunk: "/splunk.svg",
  "SigNoz Logs": "/signoz.svg",
  "SigNoz Metrics": "/signoz.svg",
  "SigNoz Traces": "/signoz.svg",
  Prometheus: "/prometheus.svg",
  "Grafana Tempo": "/tempo.svg",
} as const;

function renderPage(client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } })) {
  return render(<QueryClientProvider client={client}><MemoryRouter><AgentToolsPage /></MemoryRouter></QueryClientProvider>);
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((complete) => { resolve = complete; });
  return { promise, resolve };
}

beforeEach(() => {
  vi.mocked(api.getAgentConfig).mockResolvedValue(config);
  vi.mocked(api.listAgentToolsets).mockResolvedValue(rows);
  vi.mocked(api.setAgentToolsetEnabled).mockResolvedValue({ agent: "chat", id: "describe_dependencies", enabled: true, changed: true });
});

afterEach(() => { cleanup(); vi.clearAllMocks(); });

describe("AgentToolsPage", () => {
  it("names the page Connectors & Tools and remains readable with the agent disabled", async () => {
    vi.mocked(api.getAgentConfig).mockResolvedValueOnce({ ...config, enable: false });
    renderPage();
    expect(screen.getByRole("heading", { level: 1, name: "Connectors & Tools" })).toBeTruthy();
    expect(screen.getByRole("banner").textContent).toBe("Connectors & Tools");
    const kubernetes = await screen.findByRole("heading", { name: "Kubernetes" });
    expect(within(kubernetes.closest("article")!).getByRole("link", { name: "Open Kubernetes" }).getAttribute("href")).toBe("/agent/kubernetes");
  });

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
  });

  it("omits the File provider card even when file sources are enabled", async () => {
    renderPage();
    await screen.findByRole("heading", { name: "Elasticsearch" });
    expect(screen.queryByRole("heading", { name: "File" })).toBeNull();
    expect(screen.queryByText("Application file")).toBeNull();
    expect(screen.queryByText("Archive file")).toBeNull();
    expect(screen.getByRole("heading", { name: "Logs tools" })).toBeTruthy();
  });

  it("renders one clearly named shared policy card for every datasource group", async () => {
    renderPage();
    expect(screen.getByLabelText("Loading tools")).toBeTruthy();
    await screen.findByText("Kubernetes");
    expect(screen.getAllByRole("article")).toHaveLength(19);
    expect(screen.getAllByRole("heading", { level: 2 }).map((heading) => heading.textContent)).toEqual(["Connectors", "Data Source Tools", "Common"]);
    const connectorSection = document.querySelector('[aria-labelledby="tools-connector"]') as HTMLElement;
    const datasourceSection = document.querySelector('[aria-labelledby="tools-datasource"]') as HTMLElement;
    const commonSection = document.querySelector('[aria-labelledby="tools-common"]') as HTMLElement;
    expect(within(connectorSection).getAllByRole("article")).toHaveLength(2);
    expect(within(datasourceSection).getAllByRole("article")).toHaveLength(14);
    expect(within(commonSection).getAllByRole("article")).toHaveLength(3);
    for (const name of ["Logs tools", "Metrics tools", "Trace tools"]) {
      expect(within(datasourceSection).getAllByRole("heading", { name })).toHaveLength(1);
    }
    const sharedIcons = {
      "Logs tools": "lucide-file-text",
      "Metrics tools": "lucide-activity",
      "Trace tools": "lucide-network",
    } as const;
    for (const [name, iconClass] of Object.entries(sharedIcons)) {
      const card = within(datasourceSection).getByRole("heading", { name }).closest("article");
      expect(card?.querySelector(`svg.${iconClass}`)?.getAttribute("aria-hidden")).toBe("true");
      expect(card?.querySelector("svg.lucide-wrench")).toBeNull();
    }
    const baselineCard = screen.getByRole("heading", { name: "Describe baseline" }).closest("article");
    expect(baselineCard?.querySelector("svg.lucide-activity")?.getAttribute("aria-hidden")).toBe("true");
    expect(baselineCard?.querySelector("svg.lucide-wrench")).toBeNull();
    expect(within(datasourceSection).queryByRole("heading", { name: /^(Logs|Metrics|Traces)$/ })).toBeNull();
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
    expect(screen.getAllByText("Enterprise")).toHaveLength(4);
    expect(screen.queryAllByText("Development")).toHaveLength(0);
    expect(screen.getByText("Off")).toBeTruthy();
    expect(screen.queryByRole("switch")).toBeNull();
  });

  it("matches exact enabled source types and gates inherited UI links", async () => {
    renderPage();
    await screen.findByText("Kubernetes");
    expect(screen.getAllByRole("link", { name: /^Open / }).map((link) => link.getAttribute("href"))).toEqual([
      "/agent/kubernetes", "/agent/logs", "/agent/logs", "/agent/logs", "/agent/logs", "/agent/logs", "/agent/traces", "/agent/runbooks",
    ]);
    expect(screen.getAllByRole("button", { name: / settings$/ })).toHaveLength(19);
    expect(screen.queryAllByRole("button", { name: / details$/ })).toHaveLength(0);
    expect(screen.queryByRole("link", { name: /documentation/i })).toBeNull();
    expect(screen.getByText("Elasticsearch").closest("article")?.textContent).toContain("Ready");
    expect(screen.getByText("CloudWatch Logs").closest("article")?.querySelector('a[href="/agent/logs"]')).toBeTruthy();
    expect(screen.getByText("SigNoz Logs").closest("article")?.querySelector('a[href="/agent/logs"]')).toBeNull();
    expect(screen.getByText("Prometheus").closest("article")?.querySelector('a[href="/agent/metrics"]')).toBeNull();
    expect(screen.getByText("SigNoz Traces").closest("article")?.querySelector('a[href="/agent/traces"]')).toBeNull();
  });

  it("shows the exact provider availability matrix", async () => {
    renderPage();
    await screen.findByText("Elasticsearch");
    expect(screen.getByText("Elasticsearch").closest("article")?.textContent).toContain("Ready");
    for (const [provider, state] of Object.entries(providerStates)) {
      expect(screen.getByText(provider).closest("article")?.textContent).toContain(state);
    }
    expect(screen.getAllByRole("heading", { name: "Elasticsearch" })).toHaveLength(1);
    expect(screen.queryByRole("heading", { name: "SigNoz" })).toBeNull();
    expect(screen.getAllByRole("heading", { name: /^SigNoz (Logs|Metrics|Traces)$/ })).toHaveLength(3);
  });

  it("uses public provider logos with decorative accessible presentation", async () => {
    renderPage();
    await screen.findByText("Elasticsearch");
    for (const [provider, src] of Object.entries(providerLogos)) {
      const icon = screen.getByText(provider).closest("article")?.querySelector(`img[src="${src}"]`);
      expect(icon?.getAttribute("alt")).toBe("");
      expect(icon?.getAttribute("aria-hidden")).toBe("true");
      expect(icon?.getAttribute("width")).toBe("24");
      expect(icon?.getAttribute("height")).toBe("24");
    }
    expect(screen.queryByText("File")).toBeNull();
    expect(screen.getByText("CloudWatch Logs").closest("article")?.querySelector("img")).toBeNull();
  });

  it("shows configured log providers with source names and open links", async () => {
    renderPage();
    await screen.findByText("Loki");
    const configuredProviders = {
      Loki: ["Production Loki"],
      "CloudWatch Logs": ["CloudWatch application logs"],
      Graylog: ["Graylog production"],
      Splunk: ["Splunk production"],
    } as const;
    for (const [provider, sourceNames] of Object.entries(configuredProviders)) {
      const card = screen.getByText(provider).closest("article") as HTMLElement;
      expect(card.textContent).toContain("Configured");
      expect(card.textContent).not.toContain("Development");
      expect(within(card).getByRole("link", { name: `Open ${provider}` }).getAttribute("href")).toBe("/agent/logs");
      fireEvent.click(screen.getByRole("button", { name: `${provider} settings` }));
      const dialog = screen.getByRole("dialog", { name: provider });
      expect(dialog.textContent).toContain(`${provider} is configured.`);
      expect(dialog.textContent).not.toContain("Provider-native agent tools are in development.");
      expect(dialog.textContent).not.toContain("Disabled");
      expect(dialog.textContent).not.toContain("Setup required");
      expect(dialog.textContent).not.toMatch(/shared (Logs|Metrics|Trace) agent capability/i);
      expect(within(dialog).getByText("Configured sources", { exact: true })).toBeTruthy();
      for (const sourceName of sourceNames) expect(dialog.textContent).toContain(sourceName);
      expect(within(dialog).queryByRole("switch")).toBeNull();
      expect(within(dialog).getByRole("link", { name: /Open tool/ }).getAttribute("href")).toBe("/agent/logs");
      expect(within(dialog).queryByRole("link", { name: /Add a data source|Learn more/ })).toBeNull();
      expect(within(dialog).getByRole("link", { name: "Documentation" })).toBeTruthy();
      fireEvent.click(within(dialog).getByRole("button", { name: "Close dialog" }));
      await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    }
  });

  it("keeps shared permission state authoritative for every log provider", async () => {
    vi.mocked(api.listAgentToolsets).mockResolvedValueOnce(rows.map((row) =>
      row.id === "logs" ? { ...row, state: "needs_permission", reason: "Log access is not permitted." } : row,
    ));
    renderPage();
    await screen.findByText("Loki");
    for (const provider of ["Loki", "CloudWatch Logs", "Graylog", "Splunk", "SigNoz Logs"]) {
      const card = screen.getByText(provider).closest("article") as HTMLElement;
      expect(card.textContent).toContain("No access");
      expect(card.textContent).not.toContain("Development");
      expect(within(card).queryByRole("link", { name: `Open ${provider}` })).toBeNull();
    }
  });

  it("requires an enabled source even when shared metrics and traces are available", async () => {
    vi.mocked(api.getAgentConfig).mockResolvedValueOnce({ ...config, sources: config.sources.filter((source) => !["prometheus", "cloudwatch_metrics", "signoz_metrics", "traces", "signoz_traces"].includes(source.type)) });
    vi.mocked(api.listAgentToolsets).mockResolvedValueOnce(rows.map((row) =>
      row.id === "metrics" || row.id === "traces" ? { ...row, state: "available", reason: `${row.display_name} tools are available.` } : row,
    ));
    renderPage();
    await screen.findByText("Prometheus");
    for (const provider of ["Prometheus", "Grafana Tempo"]) {
      const card = screen.getByText(provider).closest("article");
      expect(card?.textContent).toContain("Data source needed");
      expect(card?.textContent).not.toContain("Development");
    }
    expect(screen.queryByRole("link", { name: "Open Prometheus" })).toBeNull();
    expect(screen.queryByRole("link", { name: "Open Grafana Tempo" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Prometheus settings" }));
    const dialog = screen.getByRole("dialog", { name: "Prometheus" });
    expect(dialog.textContent).toContain("Prometheus is not configured.");
    expect(dialog.textContent).not.toContain("configured outside agent sources");
    expect(within(dialog).queryByRole("switch")).toBeNull();
  });

  it("marks a licensed provider configured when its enabled source is present", async () => {
    vi.mocked(api.listAgentToolsets).mockResolvedValueOnce(rows.map((row) =>
      row.id === "metrics" ? { ...row, state: "available", reason: "Metric tools are available." } : row,
    ));
    renderPage();
    await screen.findByText("Prometheus");
    expect(screen.getByText("Prometheus").closest("article")?.textContent).toContain("Configured");
    expect(screen.getByRole("link", { name: "Open Prometheus" }).getAttribute("href")).toBe("/agent/metrics");
    fireEvent.click(screen.getByRole("button", { name: "Prometheus settings" }));
    const dialog = screen.getByRole("dialog", { name: "Prometheus" });
    expect(dialog.textContent).toContain("Prometheus is configured.");
    expect(dialog.textContent).toContain("Prometheus primary");
    expect(within(dialog).queryByRole("switch")).toBeNull();
  });

  it("keeps needs_license authoritative when a Prometheus source is configured", async () => {
    renderPage();
    await screen.findByText("Prometheus");
    const card = screen.getByText("Prometheus").closest("article");
    expect(card?.textContent).toContain("Enterprise");
    expect(card?.textContent).not.toContain("Development");
    expect(screen.queryByRole("link", { name: "Open Prometheus" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Prometheus settings" }));
    const dialog = screen.getByRole("dialog", { name: "Prometheus" });
    expect(dialog.textContent).toContain("Metric tools need an Enterprise source.");
    expect(dialog.textContent).not.toContain("Prometheus is configured.");
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

  it("surfaces shared health on the base card without enabling provider toggles", async () => {
    vi.mocked(api.getAgentConfig).mockResolvedValueOnce({ ...config, sources: [...config.sources, { name: "SigNoz production", type: "signoz", enable: true }] });
    vi.mocked(api.listAgentToolsets).mockResolvedValueOnce(rows.map((row) =>
      row.id === "logs" ? { ...row, state: "unhealthy", reason: "Log reader is unhealthy.", health: "backend unavailable" } : row,
    ));
    renderPage();
    await screen.findByText("SigNoz Logs");
    expect(screen.getByText("SigNoz Logs").closest("article")?.textContent).toContain("Unhealthy");
    fireEvent.click(screen.getByRole("button", { name: "SigNoz Logs settings" }));
    const dialog = screen.getByRole("dialog", { name: "SigNoz Logs" });
    expect(dialog.textContent).toContain("shared Logs agent capability is unhealthy");
    expect(dialog.textContent).not.toContain("backend unavailable");
    expect(within(dialog).queryByRole("switch")).toBeNull();
    fireEvent.click(within(dialog).getByRole("button", { name: "Close dialog" }));
    fireEvent.click(screen.getByRole("button", { name: "Logs tools settings" }));
    const baseDialog = screen.getByRole("dialog", { name: "Logs tools" });
    expect(baseDialog.textContent).toContain("backend unavailable");
    expect((within(baseDialog).getByRole("switch") as HTMLButtonElement).disabled).toBe(true);
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

  it("keeps shared policy controls on the base card only", async () => {
    vi.mocked(api.getAgentConfig).mockResolvedValueOnce({ ...config, sources: [...config.sources, { name: "SigNoz production", type: "signoz", enable: true }] });
    renderPage();
    await screen.findByText("SigNoz Logs");
    fireEvent.click(screen.getByRole("button", { name: "SigNoz Logs settings" }));
    let dialog = screen.getByRole("dialog", { name: "SigNoz Logs" });
    expect(within(dialog).queryByRole("switch")).toBeNull();
    expect(dialog.textContent).not.toContain("shared Logs agent capability");
    fireEvent.click(within(dialog).getByRole("button", { name: "Close dialog" }));
    fireEvent.click(screen.getByRole("button", { name: "Logs tools settings" }));
    dialog = screen.getByRole("dialog", { name: "Logs tools" });
    expect(within(dialog).getByRole("switch", { name: "Enable Logs tools for chat" })).toBeTruthy();
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
    expect(within(dialog).getByRole("link", { name: /Documentation/ }).getAttribute("href")).toBe("https://docs.versusincident.com/#/agent/connectors/kubernetes");
    expect(within(dialog).getByRole("link", { name: "Connect Kubernetes" }).getAttribute("href")).toBe("/settings?tab=agent");
    fireEvent.click(screen.getByRole("button", { name: "Close dialog" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  });

  it("keeps Enterprise authoritative and allows its external Learn more action", async () => {
    renderPage();
    await screen.findByText("Prometheus");
    fireEvent.click(screen.getByRole("button", { name: "Prometheus settings" }));
    let dialog = screen.getByRole("dialog", { name: "Prometheus" });
    expect(within(dialog).queryByRole("switch")).toBeNull();
    const documentation = within(dialog).getByRole("link", { name: "Documentation" });
    expect(documentation.getAttribute("target")).toBe("_blank");
    expect(documentation.getAttribute("rel")).toBe("noopener noreferrer");
    expect(within(dialog).getByRole("link", { name: /Learn more/ }).getAttribute("href")).toBe("https://versuscontrol.com/enterprise");
    fireEvent.click(within(dialog).getByRole("button", { name: "Close dialog" }));
    fireEvent.click(screen.getByRole("button", { name: "Metrics tools settings" }));
    dialog = screen.getByRole("dialog", { name: "Metrics tools" });
    const checkbox = within(dialog).getByRole("switch") as HTMLButtonElement;
    expect(checkbox.disabled).toBe(true);
    fireEvent.click(checkbox);
    expect(api.setAgentToolsetEnabled).not.toHaveBeenCalled();
    expect(within(dialog).getByRole("link", { name: /Learn more/ }).getAttribute("href")).toBe("https://versuscontrol.com/enterprise");
  });

  it("shows provider details, configured names, shared capability, and exact docs without internal setup links", async () => {
    vi.mocked(api.getAgentConfig).mockResolvedValueOnce({ ...config, sources: [...config.sources, { name: "SigNoz production", type: "signoz", enable: true }] });
    renderPage();
    await screen.findByText("SigNoz Logs");
    fireEvent.click(screen.getByRole("button", { name: "Source control settings" }));
    let dialog = screen.getByRole("dialog", { name: "Source control" });
    expect(within(dialog).queryByRole("link", { name: "Connect GitHub" })).toBeNull();
    expect(within(dialog).getByRole("link", { name: "Documentation" })).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "Close dialog" }));

    fireEvent.click(screen.getByRole("button", { name: "SigNoz Logs settings" }));
    dialog = screen.getByRole("dialog", { name: "SigNoz Logs" });
    expect(within(dialog).queryByRole("link", { name: "Add a data source" })).toBeNull();
    expect(dialog.textContent).toContain("Query logs stored in SigNoz.");
    expect(dialog.textContent).not.toContain("Uses the shared Logs agent capability.");
    expect(dialog.textContent).toContain("SigNoz production");
    expect(within(dialog).getByRole("link", { name: "Documentation" }).getAttribute("href")).toBe(providerDocs["SigNoz Logs"]);
  });

  it("uses provider documentation URLs for every datasource settings dialog", async () => {
    renderPage();
    await screen.findByText("Loki");
    for (const [provider, docsURL] of Object.entries(providerDocs)) {
      fireEvent.click(screen.getByRole("button", { name: `${provider} settings` }));
      const dialog = screen.getByRole("dialog", { name: provider });
      expect(within(dialog).getByRole("link", { name: "Documentation" }).getAttribute("href")).toBe(docsURL);
      expect(within(dialog).queryByRole("link", { name: "Add a data source" })).toBeNull();
      fireEvent.click(within(dialog).getByRole("button", { name: "Close dialog" }));
      await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    }
  });

  it("toggles each shared base policy ID for both agents", async () => {
    vi.mocked(api.listAgentToolsets).mockResolvedValue(rows.map((row) =>
      ["logs", "metrics", "traces"].includes(row.id) ? { ...row, state: "available", reason: `${row.display_name} tools are available.` } : row,
    ));
    renderPage();
    await screen.findByText("Logs tools");
    for (const [name, id] of [["Logs tools", "logs"], ["Metrics tools", "metrics"], ["Trace tools", "traces"]] as const) {
      fireEvent.click(screen.getByRole("button", { name: `${name} settings` }));
      fireEvent.click(within(screen.getByRole("dialog", { name })).getByRole("switch"));
      await waitFor(() => expect(api.setAgentToolsetEnabled).toHaveBeenCalledWith("chat", id, false));
      fireEvent.click(within(screen.getByRole("dialog", { name })).getByRole("button", { name: "Close dialog" }));
    }
    fireEvent.click(screen.getByRole("button", { name: "analyze" }));
    await waitFor(() => expect(api.listAgentToolsets).toHaveBeenCalledWith("analyze"));
    for (const [name, id] of [["Logs tools", "logs"], ["Metrics tools", "metrics"], ["Trace tools", "traces"]] as const) {
      fireEvent.click(await screen.findByRole("button", { name: `${name} settings` }));
      fireEvent.click(within(screen.getByRole("dialog", { name })).getByRole("switch"));
      await waitFor(() => expect(api.setAgentToolsetEnabled).toHaveBeenCalledWith("analyze", id, false));
      fireEvent.click(within(screen.getByRole("dialog", { name })).getByRole("button", { name: "Close dialog" }));
    }
  });

  it("uses the server-owned Elasticsearch state, reason, and policy toggle", async () => {
    renderPage();
    await screen.findByText("Elasticsearch");
    fireEvent.click(screen.getByRole("button", { name: "Elasticsearch settings" }));
    const dialog = screen.getByRole("dialog", { name: "Elasticsearch" });
    const checkbox = within(dialog).getByRole("switch", { name: "Enable Elasticsearch for chat" }) as HTMLButtonElement;
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
    const dedicatedToggle = within(dialog).getByRole("switch", { name: "Enable Elasticsearch for chat" }) as HTMLButtonElement;
    expect(dedicatedToggle.disabled).toBe(false);
    expect(dedicatedToggle.getAttribute("aria-checked")).toBe("true");
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
    expect((within(dialog).getByRole("switch") as HTMLButtonElement).disabled).toBe(true);
  });

  it("uses neutral shared status when source configuration cannot be queried", async () => {
    vi.mocked(api.getAgentConfig).mockRejectedValueOnce(new Error("config unavailable"));
    renderPage();
    await screen.findByText("SigNoz Logs");
    expect(screen.getAllByText("Shared status unknown")).toHaveLength(7);
    expect(screen.getAllByText("Enterprise")).toHaveLength(4);
    expect(screen.queryAllByText("Development")).toHaveLength(0);
    expect(screen.getByText("Elasticsearch").closest("article")?.textContent).toContain("Ready");
    expect(screen.queryByText("File")).toBeNull();
    expect(screen.getByText("Grafana Tempo").closest("article")?.querySelector('a[href="/agent/traces"]')).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "SigNoz Logs settings" }));
    const dialog = screen.getByRole("dialog", { name: "SigNoz Logs" });
    expect(dialog.textContent).toContain("Provider configuration is unavailable.");
    expect(within(dialog).queryByRole("switch")).toBeNull();
  });

  it("uses a stable sanitized setup description ID for provider IDs", async () => {
    renderPage();
    await screen.findByText("SigNoz Logs");
    fireEvent.click(screen.getByRole("button", { name: "SigNoz Logs settings" }));
    const dialog = screen.getByRole("dialog", { name: "SigNoz Logs" });
    const setup = within(dialog).getByText("Setup required.").parentElement;
    expect(setup?.id).toBe("toolset-provider-signoz-setup-required");
    expect(setup?.id).not.toContain(":");
  });

  it("can re-enable an operator-disabled tool from settings", async () => {
    renderPage();
    await screen.findByText("Kubernetes");
    fireEvent.click(screen.getByRole("button", { name: "Describe dependencies settings" }));
    fireEvent.click(within(screen.getByRole("dialog", { name: "Describe dependencies" })).getByRole("switch"));
    await waitFor(() => expect(api.setAgentToolsetEnabled).toHaveBeenCalledWith("chat", "describe_dependencies", true));
  });

  it("shows Kubernetes action setup reason inside Kubernetes settings without a separate card", async () => {
    vi.mocked(api.listAgentToolsets).mockResolvedValueOnce([...rows, {
      id: "kubernetes-actions", section: "connector", display_name: "Kubernetes actions", description: "Propose Kubernetes actions.", icon_key: "kubernetes", visibility: "always", state: "needs_capability", reason: "A separate Kubernetes actor credential is not configured.", action: "", action_label: "", enabled: true, child_count: 1, requirement: { kind: "capability", capabilities: ["kubernetes_actions"] },
    }]);
    renderPage();
    await screen.findByText("Kubernetes");
    expect(screen.queryByRole("heading", { name: "Kubernetes actions" })).toBeNull();
    expect(screen.getAllByRole("article")).toHaveLength(19);
    fireEvent.click(screen.getByRole("button", { name: "Kubernetes settings" }));
    const dialog = screen.getByRole("dialog", { name: "Kubernetes" });
    const actions = within(dialog).getByRole("region", { name: "Actions" });
    expect(within(actions).getByText("Setup required. A separate Kubernetes actor credential is not configured.")).toBeTruthy();
    const toggle = within(actions).getByRole("switch", { name: "Enable Kubernetes actions for chat" }) as HTMLButtonElement;
    expect(toggle.getAttribute("aria-checked")).toBe("false");
    expect(toggle.disabled).toBe(true);
    expect(api.setAgentToolsetEnabled).not.toHaveBeenCalled();
  });

  it("toggles Kubernetes actions through its own policy ID when available", async () => {
    vi.mocked(api.listAgentToolsets).mockResolvedValueOnce([...rows, {
      id: "kubernetes-actions", section: "connector", display_name: "Kubernetes actions", description: "Propose Kubernetes actions.", icon_key: "kubernetes", visibility: "always", state: "available", reason: "A separate Kubernetes actor is configured.", action: "", action_label: "", enabled: true, child_count: 1, requirement: { kind: "capability", capabilities: ["kubernetes_actions"] },
    }]);
    renderPage();
    await screen.findByText("Kubernetes");
    fireEvent.click(screen.getByRole("button", { name: "Kubernetes settings" }));
    const dialog = screen.getByRole("dialog", { name: "Kubernetes" });
    const toggle = within(within(dialog).getByRole("region", { name: "Actions" })).getByRole("switch", { name: "Enable Kubernetes actions for chat" });
    expect(toggle.getAttribute("aria-checked")).toBe("true");
    expect((within(dialog).getByRole("switch", { name: "Enable Kubernetes for chat" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(toggle);
    await waitFor(() => expect(api.setAgentToolsetEnabled).toHaveBeenCalledWith("chat", "kubernetes-actions", false));
    expect(api.setAgentToolsetEnabled).not.toHaveBeenCalledWith("chat", "kubernetes", false);
  });

  it("toggles independently per selected agent and refetches authoritative state", async () => {
    renderPage();
    await screen.findByText("Kubernetes");
    fireEvent.click(screen.getByRole("button", { name: "Describe dependencies settings" }));
    fireEvent.click(within(screen.getByRole("dialog", { name: "Describe dependencies" })).getByRole("switch"));
    await waitFor(() => expect(api.setAgentToolsetEnabled).toHaveBeenCalledWith("chat", "describe_dependencies", true));
    fireEvent.click(screen.getByRole("button", { name: "Close dialog" }));
    fireEvent.click(screen.getByRole("button", { name: "analyze" }));
    await waitFor(() => expect(api.listAgentToolsets).toHaveBeenCalledWith("analyze"));
    fireEvent.click(await screen.findByRole("button", { name: "Describe dependencies settings" }));
    expect(within(screen.getByRole("dialog", { name: "Describe dependencies" })).getByRole("switch", { name: "Enable Describe dependencies for analyze" })).toBeTruthy();
  });

  it("keeps a rejected mutation actionable and dismissible", async () => {
    vi.mocked(api.setAgentToolsetEnabled).mockRejectedValueOnce(new Error("Requirement is not satisfied"));
    renderPage();
    await screen.findByText("Kubernetes");
    fireEvent.click(screen.getByRole("button", { name: "Describe dependencies settings" }));
    const dialog = screen.getByRole("dialog", { name: "Describe dependencies" });
    const policy = within(dialog).getByRole("switch");
    fireEvent.click(policy);
    const alert = await within(dialog).findByRole("alert");
    expect(alert.textContent).toContain("Requirement is not satisfied");
    expect(within(dialog).queryByRole("alert")).toBe(alert);
    expect((policy as HTMLButtonElement).getAttribute("aria-checked")).toBe("false");
    expect(screen.queryByRole("alert", { name: /Requirement is not satisfied/ })).toBeNull();
    fireEvent.click(within(alert).getByRole("button", { name: /Dismiss/ }));
    await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
  });

  it("shows a rejected read-policy denial only for its matching agent and tool", async () => {
    vi.mocked(api.setAgentToolsetEnabled).mockRejectedValueOnce(new Error("Read access is not permitted"));
    renderPage();
    await screen.findByText("Kubernetes");
    fireEvent.click(screen.getByRole("button", { name: "Describe dependencies settings" }));
    let dialog = screen.getByRole("dialog", { name: "Describe dependencies" });
    fireEvent.click(within(dialog).getByRole("switch"));
    expect((await within(dialog).findByRole("alert")).textContent).toContain("Read access is not permitted");

    fireEvent.click(within(dialog).getByRole("button", { name: "Close dialog" }));
    fireEvent.click(screen.getByRole("button", { name: "Source control settings" }));
    dialog = screen.getByRole("dialog", { name: "Source control" });
    expect(within(dialog).queryByRole("alert")).toBeNull();
    fireEvent.click(within(dialog).getByRole("button", { name: "Close dialog" }));

    fireEvent.click(screen.getByRole("button", { name: "analyze" }));
    await waitFor(() => expect(api.listAgentToolsets).toHaveBeenCalledWith("analyze"));
    fireEvent.click(await screen.findByRole("button", { name: "Describe dependencies settings" }));
    dialog = screen.getByRole("dialog", { name: "Describe dependencies" });
    expect(within(dialog).queryByRole("alert")).toBeNull();
    fireEvent.click(within(dialog).getByRole("button", { name: "Close dialog" }));

    fireEvent.click(screen.getByRole("button", { name: "chat" }));
    await waitFor(() => expect(api.listAgentToolsets).toHaveBeenCalledWith("chat"));
    fireEvent.click(await screen.findByRole("button", { name: "Describe dependencies settings" }));
    dialog = screen.getByRole("dialog", { name: "Describe dependencies" });
    expect((await within(dialog).findByRole("alert")).textContent).toContain("Read access is not permitted");
  });

  it("shows a rejected Kubernetes-actions denial only in its matching Kubernetes dialog", async () => {
    const actions: AgentToolsetAvailability = {
      id: "kubernetes-actions", section: "connector", display_name: "Kubernetes actions", description: "Propose Kubernetes actions.", icon_key: "kubernetes", visibility: "always", state: "available", reason: "A separate Kubernetes actor is configured.", action: "", action_label: "", enabled: true, child_count: 1, requirement: { kind: "capability", capabilities: ["kubernetes_actions"] },
    };
    vi.mocked(api.listAgentToolsets).mockResolvedValue([
      ...rows.map((row) => row.id === "kubernetes" ? { ...row, state: "available", reason: "Kubernetes is connected." } : row),
      actions,
    ]);
    vi.mocked(api.setAgentToolsetEnabled).mockRejectedValueOnce(new Error("Kubernetes actions are not permitted"));
    renderPage();
    await screen.findByText("Kubernetes");
    fireEvent.click(screen.getByRole("button", { name: "Kubernetes settings" }));
    let dialog = screen.getByRole("dialog", { name: "Kubernetes" });
    const actionToggle = within(within(dialog).getByRole("region", { name: "Actions" })).getByRole("switch");
    fireEvent.click(actionToggle);
    expect((await within(dialog).findByRole("alert")).textContent).toContain("Kubernetes actions are not permitted");

    fireEvent.click(within(dialog).getByRole("button", { name: "Close dialog" }));
    fireEvent.click(screen.getByRole("button", { name: "Source control settings" }));
    dialog = screen.getByRole("dialog", { name: "Source control" });
    expect(within(dialog).queryByRole("alert")).toBeNull();
    fireEvent.click(within(dialog).getByRole("button", { name: "Close dialog" }));

    fireEvent.click(screen.getByRole("button", { name: "analyze" }));
    await waitFor(() => expect(api.listAgentToolsets).toHaveBeenCalledWith("analyze"));
    fireEvent.click(await screen.findByRole("button", { name: "Kubernetes settings" }));
    dialog = screen.getByRole("dialog", { name: "Kubernetes" });
    expect(within(dialog).queryByRole("alert")).toBeNull();
    fireEvent.click(within(dialog).getByRole("button", { name: "Close dialog" }));

    fireEvent.click(screen.getByRole("button", { name: "chat" }));
    await waitFor(() => expect(api.listAgentToolsets).toHaveBeenCalledWith("chat"));
    fireEvent.click(await screen.findByRole("button", { name: "Kubernetes settings" }));
    dialog = screen.getByRole("dialog", { name: "Kubernetes" });
    expect((await within(dialog).findByRole("alert")).textContent).toContain("Kubernetes actions are not permitted");
  });

  it("locks both Kubernetes policy switches across read and action saves", async () => {
    const readSave = deferred<Awaited<ReturnType<typeof api.setAgentToolsetEnabled>>>();
    const actionSave = deferred<Awaited<ReturnType<typeof api.setAgentToolsetEnabled>>>();
    let saveIndex = 0;
    vi.mocked(api.listAgentToolsets).mockResolvedValue([...rows.map((row) => row.id === "kubernetes"
      ? { ...row, state: "available", reason: "Kubernetes is connected." }
      : row), {
      id: "kubernetes-actions", section: "connector", display_name: "Kubernetes actions", description: "Propose Kubernetes actions.", icon_key: "kubernetes", visibility: "always", state: "available", reason: "A separate Kubernetes actor is configured.", action: "", action_label: "", enabled: true, child_count: 1, requirement: { kind: "capability", capabilities: ["kubernetes_actions"] },
    }]);
    vi.mocked(api.setAgentToolsetEnabled).mockImplementation(() => [readSave.promise, actionSave.promise][saveIndex++]);
    renderPage();
    await screen.findByText("Kubernetes");
    fireEvent.click(screen.getByRole("button", { name: "Kubernetes settings" }));
    const dialog = screen.getByRole("dialog", { name: "Kubernetes" });
    const readPolicy = within(dialog).getByRole("switch", { name: "Enable Kubernetes for chat" }) as HTMLButtonElement;
    const actionPolicy = within(dialog).getByRole("switch", { name: "Enable Kubernetes actions for chat" }) as HTMLButtonElement;

    fireEvent.click(readPolicy);
    await waitFor(() => {
      expect(readPolicy.disabled).toBe(true);
      expect(actionPolicy.disabled).toBe(true);
    });
    await act(async () => readSave.resolve({ agent: "chat", id: "kubernetes", enabled: false, changed: true }));
    await waitFor(() => expect(readPolicy.disabled).toBe(false));
    expect(actionPolicy.disabled).toBe(false);

    fireEvent.click(actionPolicy);
    await waitFor(() => {
      expect(readPolicy.disabled).toBe(true);
      expect(actionPolicy.disabled).toBe(true);
    });
    await act(async () => actionSave.resolve({ agent: "chat", id: "kubernetes-actions", enabled: false, changed: true }));
    await waitFor(() => expect(actionPolicy.disabled).toBe(false));
    expect(readPolicy.disabled).toBe(false);
  });

  it("keeps a pending save attached to its original agent", async () => {
    const save = deferred<Awaited<ReturnType<typeof api.setAgentToolsetEnabled>>>();
    vi.mocked(api.setAgentToolsetEnabled).mockReturnValue(save.promise);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    renderPage(client);
    await screen.findByText("Kubernetes");
    fireEvent.click(screen.getByRole("button", { name: "Describe dependencies settings" }));
    let dialog = screen.getByRole("dialog", { name: "Describe dependencies" });
    fireEvent.click(within(dialog).getByRole("switch", { name: "Enable Describe dependencies for chat" }));
    await waitFor(() => expect(within(dialog).getByLabelText("Saving")).toBeTruthy());
    fireEvent.click(within(dialog).getByRole("button", { name: "Close dialog" }));
    fireEvent.click(screen.getByRole("button", { name: "analyze" }));
    await waitFor(() => expect(api.listAgentToolsets).toHaveBeenCalledWith("analyze"));
    fireEvent.click(await screen.findByRole("button", { name: "Describe dependencies settings" }));
    dialog = screen.getByRole("dialog", { name: "Describe dependencies" });
    const analyzePolicy = await within(dialog).findByRole("switch", { name: "Enable Describe dependencies for analyze" });
    expect((analyzePolicy as HTMLButtonElement).disabled).toBe(true);
    expect(within(dialog).queryByLabelText("Saving")).toBeNull();

    await act(async () => save.resolve({ agent: "chat", id: "describe_dependencies", enabled: true, changed: true }));
    await waitFor(() => expect(client.getQueryState(["agent-toolsets", "chat"])?.isInvalidated).toBe(true));
    expect(client.getQueryState(["agent-toolsets", "analyze"])?.isInvalidated).not.toBe(true);
    expect(api.setAgentToolsetEnabled).toHaveBeenCalledWith("chat", "describe_dependencies", true);
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