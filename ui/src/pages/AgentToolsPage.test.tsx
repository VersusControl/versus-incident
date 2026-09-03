// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api, type AgentToolsetAvailability } from "@/lib/api";
import { AgentToolsPage } from "./AgentToolsPage";

vi.mock("@/components/TopBar", () => ({ TopBar: ({ title }: { title: string }) => <header>{title}</header> }));
vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, listAgentToolsets: vi.fn(), setAgentToolsetEnabled: vi.fn() } };
});

const rows: AgentToolsetAvailability[] = [
  { id: "kubernetes", section: "connector", display_name: "Kubernetes", description: "Inspect Kubernetes.", icon_key: "kubernetes", docs_url: "https://docs.versusincident.com/#/agent/tools/kubernetes", ui_path: "/agent/kubernetes", visibility: "always", state: "needs_integration", reason: "Kubernetes is not connected.", action: "/settings?tab=agent", action_label: "Connect Kubernetes", enabled: true, child_count: 9, requirement: { kind: "integration", integration: "kubernetes" } },
  { id: "source-control", section: "connector", display_name: "Source control", description: "Read recent changes.", icon_key: "git", docs_url: "https://docs.versusincident.com/#/agent/tools/recent-changes", visibility: "always", state: "needs_integration", reason: "GitHub is not connected.", action: "/settings?tab=agent", action_label: "Connect GitHub", enabled: true, child_count: 1, requirement: { kind: "integration", integration: "github" } },
  { id: "logs", section: "datasource", display_name: "Logs", description: "Read bounded logs.", icon_key: "logs", docs_url: "https://docs.versusincident.com/#/agent/data-sources", ui_path: "/agent/logs", visibility: "always", state: "needs_datasource", reason: "No log data source is connected.", action: "/settings?tab=agent", action_label: "Add a data source", enabled: true, child_count: 1, requirement: { kind: "datasource", signal_kind: "logs" } },
  { id: "metrics", section: "datasource", display_name: "Metrics", description: "Summarize metrics.", icon_key: "metrics", docs_url: "https://docs.versusincident.com/#/agent/data-sources/prometheus", ui_path: "/agent/metrics", visibility: "always", state: "needs_license", reason: "Metric tools need an Enterprise source.", action: "https://versuscontrol.com/enterprise", action_label: "Learn more", enabled: true, child_count: 1, requirement: { kind: "datasource", signal_kind: "metrics" } },
  { id: "traces", section: "datasource", display_name: "Traces", description: "Inspect traces.", icon_key: "traces", docs_url: "https://docs.versusincident.com/#/agent/data-sources/traces", ui_path: "/agent/traces", visibility: "always", state: "needs_datasource", reason: "No trace data source is connected.", action: "/settings?tab=agent", action_label: "Add a data source", enabled: true, child_count: 1, requirement: { kind: "datasource", signal_kind: "traces" } },
  { id: "find_runbook", section: "common", display_name: "Find runbook", description: "Search runbooks.", icon_key: "runbook", docs_url: "https://docs.versusincident.com/#/agent/tools/find-runbook", ui_path: "/agent/runbooks", visibility: "always", state: "needs_capability", reason: "Runbook indexing is not configured.", action: "/admin#agent-ai-settings", action_label: "AI settings", enabled: true, child_count: 1, requirement: { kind: "capability" } },
  { id: "describe_dependencies", section: "common", display_name: "Describe dependencies", description: "Inspect dependencies.", icon_key: "dependencies", docs_url: "https://docs.versusincident.com/#/agent/tools/tools?id=describe_dependencies", visibility: "always", state: "disabled_by_operator", reason: "Not offered to the agent.", action: "", action_label: "", enabled: false, child_count: 1, requirement: { kind: "capability" } },
];

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={client}><MemoryRouter><AgentToolsPage /></MemoryRouter></QueryClientProvider>);
}

beforeEach(() => {
  vi.mocked(api.listAgentToolsets).mockResolvedValue(rows);
  vi.mocked(api.setAgentToolsetEnabled).mockResolvedValue({ agent: "chat", id: "describe_dependencies", enabled: true, changed: true });
});

afterEach(() => { cleanup(); vi.clearAllMocks(); });

describe("AgentToolsPage", () => {
  it("renders exactly seven cards in canonical sections with visible accessible toggles", async () => {
    renderPage();
    expect(screen.getByLabelText("Loading tools")).toBeTruthy();
    await screen.findByText("Kubernetes");
    const headings = screen.getAllByRole("heading", { level: 2 }).map((node) => node.textContent);
    expect(headings).toEqual(["Connectors", "Data Source Tools", "Common"]);
    expect(screen.getAllByRole("article")).toHaveLength(7);
    expect(screen.getByText("9 tools")).toBeTruthy();
    expect(screen.queryByText("get_cluster_overview")).toBeNull();
    expect(screen.getByText("Metrics is unavailable: Metric tools need an Enterprise source.")).toBeTruthy();
    expect(screen.getByRole("link", { name: /Learn more/ }).getAttribute("href")).toBe("https://versuscontrol.com/enterprise");
    expect(screen.queryByRole("link", { name: "AI settings" })).toBeNull();
    expect(screen.getByRole("link", { name: "Connect Kubernetes" }).getAttribute("href")).toBe("/settings?tab=agent");
    expect(screen.getByLabelText("Enable Describe dependencies for chat").hasAttribute("disabled")).toBe(false);
    for (const name of ["Enable Metrics for chat", "Enable Find runbook for chat", "Enable Kubernetes for chat"]) {
      const checkbox = screen.getByLabelText(name) as HTMLInputElement;
      expect(checkbox.checked).toBe(false);
      expect(checkbox.disabled).toBe(true);
      expect(checkbox.getAttribute("aria-disabled")).toBe("true");
    }
    expect(document.querySelector(".grid-cols-1")).toBeTruthy();
  });

  it("renders safe documentation for every card and only server-mapped internal links", async () => {
    renderPage();
    await screen.findByText("Kubernetes");
    const cards = screen.getAllByRole("article");
    expect(screen.getAllByRole("link", { name: "Documentation" })).toHaveLength(cards.length);
    const documentationLinks = screen.getAllByRole("link", { name: "Documentation" });
    for (const link of documentationLinks) {
      expect(link.getAttribute("target")).toBe("_blank");
      expect(link.getAttribute("rel")).toBe("noopener noreferrer");
    }
    expect(documentationLinks[0].getAttribute("href")).toBe("https://docs.versusincident.com/#/agent/tools/kubernetes");
    expect(screen.getAllByRole("link", { name: "Open tool" }).map((link) => link.getAttribute("href"))).toEqual([
      "/agent/kubernetes", "/agent/logs", "/agent/traces", "/agent/runbooks",
    ]);
    expect(screen.getByText("Metrics").closest("article")?.querySelector('a[href="/agent/metrics"]')).toBeNull();
  });

  it("keeps permission-blocked tools documented without exposing their internal page", async () => {
    vi.mocked(api.listAgentToolsets).mockResolvedValueOnce(rows.map((row) =>
      row.id === "kubernetes"
        ? { ...row, state: "needs_permission", reason: "Kubernetes access is not permitted." }
        : row,
    ));
    renderPage();
    const card = (await screen.findByText("Kubernetes")).closest("article");
    expect(card?.textContent).toContain("Kubernetes access is not permitted.");
    expect(card?.querySelector('a[href="/agent/kubernetes"]')).toBeNull();
    expect(card?.querySelector('a[href="https://docs.versusincident.com/#/agent/tools/kubernetes"]')).toBeTruthy();
  });

  it("hides internal setup actions from Common cards", async () => {
    vi.mocked(api.listAgentToolsets).mockResolvedValueOnce(rows.map((row) =>
      row.id === "find_runbook"
        ? { ...row, action: "/settings?tab=agent", action_label: "Configure capability" }
        : row,
    ));
    renderPage();
    await screen.findByText("Find runbook");
    expect(screen.queryByRole("link", { name: "Configure capability" })).toBeNull();
    expect(screen.getByRole("link", { name: /Learn more/ })).toBeTruthy();
    expect(screen.getByRole("link", { name: "Connect Kubernetes" })).toBeTruthy();
  });

  it("does not mutate unavailable tools and can re-enable an operator-disabled Versus tool", async () => {
    renderPage();
    await screen.findByText("Kubernetes");
    fireEvent.click(screen.getByLabelText("Enable Metrics for chat"));
    expect(api.setAgentToolsetEnabled).not.toHaveBeenCalled();
    fireEvent.click(screen.getByLabelText("Enable Describe dependencies for chat"));
    await waitFor(() => expect(api.setAgentToolsetEnabled).toHaveBeenCalledWith("chat", "describe_dependencies", true));
  });

  it("toggles independently per selected agent and refetches authoritative state", async () => {
    renderPage();
    await screen.findByText("Kubernetes");
    fireEvent.click(screen.getByLabelText("Enable Describe dependencies for chat"));
    await waitFor(() => expect(api.setAgentToolsetEnabled).toHaveBeenCalledWith("chat", "describe_dependencies", true));
    fireEvent.click(screen.getByRole("button", { name: "analyze" }));
    await waitFor(() => expect(api.listAgentToolsets).toHaveBeenCalledWith("analyze"));
    expect(await screen.findByLabelText("Enable Describe dependencies for analyze")).toBeTruthy();
  });

  it("keeps a rejected mutation actionable and dismissible", async () => {
    vi.mocked(api.setAgentToolsetEnabled).mockRejectedValueOnce(new Error("Requirement is not satisfied"));
    renderPage();
    await screen.findByText("Kubernetes");
    fireEvent.click(screen.getByLabelText("Enable Describe dependencies for chat"));
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