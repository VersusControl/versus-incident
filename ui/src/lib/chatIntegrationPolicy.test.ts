// @vitest-environment jsdom
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createElement } from "react";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { SidebarContent } from "@/components/Sidebar";
import { ApiError } from "@/lib/api";

const appSource = readFileSync(resolve(process.cwd(), "src/App.tsx"), "utf8");

const apiMocks = vi.hoisted(() => ({
  getAgentConfig: vi.fn(),
  status: vi.fn(),
  getSSODeployment: vi.fn(),
  listBaselines: vi.fn(),
}));

vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      getAgentConfig: apiMocks.getAgentConfig,
      status: apiMocks.status,
      getSSODeployment: apiMocks.getSSODeployment,
      listBaselines: apiMocks.listBaselines,
    },
  };
});

beforeEach(() => {
  apiMocks.getAgentConfig.mockReset().mockResolvedValue({ enable: true });
  apiMocks.status.mockReset().mockResolvedValue({ runbooks_available: true });
  apiMocks.getSSODeployment.mockReset().mockRejectedValue(new ApiError(403, "forbidden"));
  apiMocks.listBaselines.mockReset().mockRejectedValue(new ApiError(403, "forbidden"));
});

afterEach(cleanup);

async function renderSidebar() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    createElement(
      QueryClientProvider,
      { client },
      createElement(
        MemoryRouter,
        { future: { v7_startTransition: true, v7_relativeSplatPath: true } },
        createElement(SidebarContent),
      ),
    ),
  );
  await waitFor(() => expect(apiMocks.getSSODeployment).toHaveBeenCalledOnce());
}

function sectionLinks(title: string) {
  const header = screen.getByText(title);
  const links: string[] = [];
  for (let node = header.nextElementSibling; node?.tagName === "A"; node = node.nextElementSibling) {
    links.push(node.getAttribute("href") ?? "");
  }
  return links;
}

describe("chat integration policy", () => {
  it("keeps the cold chat page lazy and mounts every canonical DC5 route", () => {
    expect(appSource).toMatch(/lazyPage\(\(\) => import\("\.\/pages\/ChatPage"\)/);
    expect(appSource).toContain('<Route path="/agent/chat" element={<ChatPage />} />');
    expect(appSource).toContain('<Route path="/agent/tools" element={<AgentToolsPage />} />');
    expect(appSource).toContain('<Route path="/agent/runbooks" element={<RunbooksPage />} />');
  });

  it("renders Chat, Tool catalog, and Runbooks first without a generic Tools zone", async () => {
    await renderSidebar();

    expect(sectionLinks("AI").slice(0, 4)).toEqual([
      "/agent/chat",
      "/agent/tools",
      "/agent/runbooks",
      "/agent/decisions",
    ]);
    expect(screen.queryByText("Tools", { selector: "div" })).toBeNull();
  });

  it("keeps Tool catalog readable while agent-backed pages are disabled", async () => {
    apiMocks.getAgentConfig.mockResolvedValue({ enable: false });
    await renderSidebar();

    const tools = screen.getByRole("link", { name: "Tool catalog" });
    expect(tools.getAttribute("href")).toBe("/agent/tools");
    expect(tools.getAttribute("title")).toBeNull();
    await waitFor(() =>
      expect(screen.getByRole("link", { name: /Chat/ }).getAttribute("title")).toContain(
        "AI agent is disabled",
      ),
    );
  });

  it("renders the Runbooks setup hint when its capability is unavailable", async () => {
    apiMocks.status.mockResolvedValue({ runbooks_available: false });
    await renderSidebar();

    const runbooks = screen.getByRole("link", { name: /Runbooks/ });
    expect(runbooks.getAttribute("href")).toBe("/agent/runbooks");
    await waitFor(() =>
      expect(runbooks.getAttribute("title")).toContain("configure an embedding model"),
    );
    expect(within(runbooks).getByLabelText("Enterprise")).toBeTruthy();
  });
});