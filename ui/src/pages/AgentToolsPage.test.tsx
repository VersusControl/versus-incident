// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api, type AgentToolAvailability } from "@/lib/api";
import { AgentToolsPage } from "./AgentToolsPage";

vi.mock("@/components/TopBar", () => ({ TopBar: ({ title }: { title: string }) => <header>{title}</header> }));
vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return { ...actual, api: { ...actual.api, listAgentTools: vi.fn(), setAgentToolEnabled: vi.fn() } };
});

const rows: AgentToolAvailability[] = [
  { group: "versus", name: "get_incident", display_name: "Incident details", description: "Inspect one incident.", state: "available", reason: "Available to the agent.", action: "", action_label: "", enabled: true, requirement: { kind: "none" } },
  { group: "common", name: "query_metrics", display_name: "Query metrics", description: "Summarize metric series.", state: "needs_license", reason: "Metric tools need an Enterprise source.", action: "https://versuscontrol.com/enterprise", action_label: "Learn more", enabled: true, requirement: { kind: "datasource", signal_kind: "metrics" } },
  { group: "k8s", name: "get_cluster_overview", display_name: "Cluster overview", description: "Summarize cluster health.", state: "needs_integration", reason: "Kubernetes is not connected, so this tool cannot inspect Kubernetes resources.", action: "/settings?tab=agent", action_label: "Connect Kubernetes", enabled: true, requirement: { kind: "integration", integration: "kubernetes" } },
];

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={client}><AgentToolsPage /></QueryClientProvider>);
}

beforeEach(() => {
  vi.mocked(api.listAgentTools).mockResolvedValue(rows);
  vi.mocked(api.setAgentToolEnabled).mockResolvedValue({ agent: "chat", name: "get_incident", enabled: false, changed: true });
});

afterEach(() => { cleanup(); vi.clearAllMocks(); });

describe("AgentToolsPage", () => {
  it("renders server states and required group order with accessible toggles", async () => {
    renderPage();
    expect(screen.getByLabelText("Loading tools")).toBeTruthy();
    await screen.findByText("Incident details");
    const headings = screen.getAllByRole("heading", { level: 2 }).map((node) => node.textContent);
    expect(headings).toEqual(["Versus", "Common", "Kubernetes"]);
    expect(screen.getByText("Metric tools need an Enterprise source.")).toBeTruthy();
    expect(screen.getByRole("link", { name: /Learn more/ }).getAttribute("href")).toBe("https://versuscontrol.com/enterprise");
    expect(screen.getByLabelText("Enable Incident details for chat").hasAttribute("disabled")).toBe(false);
    expect(screen.getByLabelText("Enable Query metrics for chat").hasAttribute("disabled")).toBe(true);
    expect(screen.getByLabelText("Enable Cluster overview for chat").hasAttribute("disabled")).toBe(true);
    expect(document.querySelector(".grid-cols-1")).toBeTruthy();
  });

  it("toggles independently per selected agent and refetches authoritative state", async () => {
    renderPage();
    await screen.findByText("Incident details");
    fireEvent.click(screen.getByLabelText("Enable Incident details for chat"));
    await waitFor(() => expect(api.setAgentToolEnabled).toHaveBeenCalledWith("chat", "get_incident", false));
    fireEvent.click(screen.getByRole("button", { name: "analyze" }));
    await waitFor(() => expect(api.listAgentTools).toHaveBeenCalledWith("analyze"));
    expect(await screen.findByLabelText("Enable Incident details for analyze")).toBeTruthy();
  });

  it("keeps a rejected mutation actionable and dismissible", async () => {
    vi.mocked(api.setAgentToolEnabled).mockRejectedValueOnce(new Error("Requirement is not satisfied"));
    renderPage();
    await screen.findByText("Incident details");
    fireEvent.click(screen.getByLabelText("Enable Incident details for chat"));
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