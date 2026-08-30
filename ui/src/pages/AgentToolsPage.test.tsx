// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api, type AgentToolAvailability } from "@/lib/api";
import { AgentToolsPage } from "./AgentToolsPage";

vi.mock("@/components/TopBar", () => ({ TopBar: ({ title }: { title: string }) => <header>{title}</header> }));
vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, listAgentTools: vi.fn(), setAgentToolEnabled: vi.fn() } };
});

const rows: AgentToolAvailability[] = [
  { group: "versus", name: "get_incident", display_name: "Incident details", description: "Inspect one incident.", docs_url: "https://docs.versusincident.com/#/agent/tools/tools?id=versus-tools", state: "available", reason: "Available to the agent.", action: "", action_label: "", enabled: true, requirement: { kind: "none" } },
  { group: "versus", name: "list_analyses", display_name: "List analyses", description: "Inspect analyses.", docs_url: "https://docs.versusincident.com/#/agent/tools/tools?id=versus-tools", ui_path: "/analyses", state: "disabled_by_operator", reason: "Not offered to the agent.", action: "", action_label: "", enabled: false, requirement: { kind: "none" } },
  { group: "common", name: "query_metrics", display_name: "Query metrics", description: "Summarize metric series.", docs_url: "https://docs.versusincident.com/#/agent/data-sources/prometheus", ui_path: "/agent/metrics", state: "needs_license", reason: "Metric tools need an Enterprise source.", action: "https://versuscontrol.com/enterprise", action_label: "Learn more", enabled: true, requirement: { kind: "datasource", signal_kind: "metrics" } },
  { group: "common", name: "find_runbook", display_name: "Find runbook", description: "Search runbooks.", docs_url: "https://docs.versusincident.com/#/agent/tools/find-runbook", ui_path: "/agent/runbooks", state: "needs_capability", reason: "Runbook indexing is not configured.", action: "/admin#agent-ai-settings", action_label: "AI settings", enabled: true, requirement: { kind: "capability" } },
  { group: "k8s", name: "get_cluster_overview", display_name: "Cluster overview", description: "Summarize cluster health.", docs_url: "https://docs.versusincident.com/#/agent/tools/tools?id=kubernetes-tools", state: "needs_integration", reason: "Kubernetes is not connected, so this tool cannot inspect Kubernetes resources.", action: "/settings?tab=agent", action_label: "Connect Kubernetes", enabled: true, requirement: { kind: "integration", integration: "kubernetes" } },
];

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={client}><MemoryRouter><AgentToolsPage /></MemoryRouter></QueryClientProvider>);
}

beforeEach(() => {
  vi.mocked(api.listAgentTools).mockResolvedValue(rows);
  vi.mocked(api.setAgentToolEnabled).mockResolvedValue({ agent: "chat", name: "get_incident", enabled: false, changed: true });
});

afterEach(() => { cleanup(); vi.clearAllMocks(); });

describe("AgentToolsPage", () => {
  it("hides default Versus tools but shows disabled recovery with visible accessible toggles", async () => {
    renderPage();
    expect(screen.getByLabelText("Loading tools")).toBeTruthy();
    await screen.findByText("List analyses");
    const headings = screen.getAllByRole("heading", { level: 2 }).map((node) => node.textContent);
    expect(headings).toEqual(["Versus", "Common", "Kubernetes"]);
    expect(screen.queryByText("Incident details")).toBeNull();
    expect(screen.getByText("Query metrics is unavailable: Metric tools need an Enterprise source.")).toBeTruthy();
    expect(screen.getByRole("link", { name: /Learn more/ }).getAttribute("href")).toBe("https://versuscontrol.com/enterprise");
    expect(screen.getByLabelText("Enable List analyses for chat").hasAttribute("disabled")).toBe(false);
    for (const name of ["Enable Query metrics for chat", "Enable Find runbook for chat", "Enable Cluster overview for chat"]) {
      const checkbox = screen.getByLabelText(name) as HTMLInputElement;
      expect(checkbox.checked).toBe(false);
      expect(checkbox.disabled).toBe(true);
      expect(checkbox.getAttribute("aria-disabled")).toBe("true");
    }
    expect(document.querySelector(".grid-cols-1")).toBeTruthy();
  });

  it("renders safe documentation for every card and only server-mapped internal links", async () => {
    renderPage();
    await screen.findByText("List analyses");
    const cards = screen.getAllByRole("article");
    expect(screen.getAllByRole("link", { name: "Documentation" })).toHaveLength(cards.length);
    const documentationLinks = screen.getAllByRole("link", { name: "Documentation" });
    for (const link of documentationLinks) {
      expect(link.getAttribute("target")).toBe("_blank");
      expect(link.getAttribute("rel")).toBe("noopener noreferrer");
    }
    expect(documentationLinks.map((link) => link.getAttribute("href"))).toEqual([
      "https://docs.versusincident.com/#/agent/tools/tools?id=versus-tools",
      "https://docs.versusincident.com/#/agent/data-sources/prometheus",
      "https://docs.versusincident.com/#/agent/tools/find-runbook",
      "https://docs.versusincident.com/#/agent/tools/tools?id=kubernetes-tools",
    ]);
    expect(screen.getAllByRole("link", { name: "Open tool" }).map((link) => link.getAttribute("href"))).toEqual([
      "/analyses", "/agent/runbooks",
    ]);
    expect(screen.getByText("query_metrics").closest("article")?.querySelector('a[href="/agent/metrics"]')).toBeNull();
    expect(screen.getByText("find_runbook").closest("article")?.querySelector('a[href="/agent/runbooks"]')).not.toBeNull();
  });

  it("does not mutate unavailable tools and can re-enable an operator-disabled Versus tool", async () => {
    renderPage();
    await screen.findByText("List analyses");
    fireEvent.click(screen.getByLabelText("Enable Query metrics for chat"));
    expect(api.setAgentToolEnabled).not.toHaveBeenCalled();
    fireEvent.click(screen.getByLabelText("Enable List analyses for chat"));
    await waitFor(() => expect(api.setAgentToolEnabled).toHaveBeenCalledWith("chat", "list_analyses", true));
  });

  it("toggles independently per selected agent and refetches authoritative state", async () => {
    renderPage();
    await screen.findByText("List analyses");
    fireEvent.click(screen.getByLabelText("Enable List analyses for chat"));
    await waitFor(() => expect(api.setAgentToolEnabled).toHaveBeenCalledWith("chat", "list_analyses", true));
    fireEvent.click(screen.getByRole("button", { name: "analyze" }));
    await waitFor(() => expect(api.listAgentTools).toHaveBeenCalledWith("analyze"));
    expect(await screen.findByLabelText("Enable List analyses for analyze")).toBeTruthy();
  });

  it("keeps a rejected mutation actionable and dismissible", async () => {
    vi.mocked(api.setAgentToolEnabled).mockRejectedValueOnce(new Error("Requirement is not satisfied"));
    renderPage();
    await screen.findByText("List analyses");
    fireEvent.click(screen.getByLabelText("Enable List analyses for chat"));
    expect((await screen.findByRole("alert")).textContent).toContain("Requirement is not satisfied");
    fireEvent.click(screen.getByRole("button", { name: /Dismiss/ }));
    await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
  });

  it("renders recoverable error and empty states", async () => {
    vi.mocked(api.listAgentTools).mockRejectedValue(new Error("offline"));
    const first = renderPage();
    expect(await screen.findByText(/Couldn't load agent tools/)).toBeTruthy();
    first.unmount();
    vi.mocked(api.listAgentTools).mockResolvedValueOnce([]);
    renderPage();
    expect(await screen.findByText("No tools are known to this build.")).toBeTruthy();
  });
});