// @vitest-environment jsdom
import { describe, it, expect, afterEach, beforeEach, vi } from "vitest";
import {
  render,
  screen,
  cleanup,
  fireEvent,
  waitFor,
  within,
} from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { ApiError } from "@/lib/api";
import { Sidebar, SidebarContent } from "./Sidebar";

// This runner exposes node's experimental global localStorage, whose methods
// aren't callable without a backing file. Install a tiny in-memory Storage so
// the rail's persistence (window.localStorage) is exercised deterministically.
class MemoryStorage {
  private m = new Map<string, string>();
  get length() {
    return this.m.size;
  }
  clear() {
    this.m.clear();
  }
  getItem(key: string) {
    return this.m.has(key) ? (this.m.get(key) as string) : null;
  }
  setItem(key: string, value: string) {
    this.m.set(key, String(value));
  }
  removeItem(key: string) {
    this.m.delete(key);
  }
  key(i: number) {
    return Array.from(this.m.keys())[i] ?? null;
  }
}
const memStore = new MemoryStorage();
Object.defineProperty(window, "localStorage", {
  value: memStore,
  configurable: true,
});
Object.defineProperty(globalThis, "localStorage", {
  value: memStore,
  configurable: true,
});

const apiMocks = vi.hoisted(() => ({
  getAgentConfig: vi.fn().mockResolvedValue({ enable: true }),
  status: vi.fn().mockResolvedValue({ runbooks_available: false }),
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
  apiMocks.status.mockReset().mockResolvedValue({ runbooks_available: false });
  apiMocks.getSSODeployment
    .mockReset()
    .mockRejectedValue(new ApiError(403, "forbidden"));
  apiMocks.listBaselines
    .mockReset()
    .mockRejectedValue(new ApiError(403, "forbidden"));
});

afterEach(cleanup);

afterEach(() => {
  try {
    window.localStorage.clear();
  } catch {
    // no-op — jsdom always has localStorage, this guards non-DOM runs.
  }
});

function renderSidebar() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter
        future={{ v7_startTransition: true, v7_relativeSplatPath: true }}
      >
        <SidebarContent />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

// Render and wait until the baselines probe has resolved to "unlicensed" — the
// enterprise locks (aria-label 'Enterprise') only render once it does, so this
// guarantees the gated state is settled before we assert on it.
async function renderSettled() {
  renderSidebar();
  await screen.findAllByLabelText("Enterprise");
  await screen.findByText("Enterprise");
}

// The primary nav is a flat list: each Zone renders a header <div> followed by
// its <a> links as siblings. Walk it into { sectionTitle -> [hrefs] } so we can
// assert grouping by what actually renders, independent of the source arrays.
function navSections(): Record<string, string[]> {
  const nav = screen.getByRole("navigation", { name: "Primary" });
  const sections: Record<string, string[]> = {};
  let current = "";
  for (const child of Array.from(nav.children)) {
    if (child.tagName === "A") {
      (sections[current] ??= []).push(child.getAttribute("href") ?? "");
    } else {
      current = child.textContent?.trim() ?? "";
      sections[current] ??= [];
    }
  }
  return sections;
}

describe("Sidebar — deployment-aware navigation groups", () => {
  it("moves only enterprise-only items into an OSS Enterprise group", async () => {
    await renderSettled();

    expect(navSections()["Agent"]).toEqual([
      "/agent",
      "/agent/services",
      "/agent/logs",
    ]);
    expect(navSections()["AI"]).toEqual([
      "/agent/chat",
      "/agent/tools",
      "/agent/runbooks",
      "/agent/decisions",
      "/analyses",
    ]);
    expect(navSections()["Enterprise"]).toEqual([
      "/agent/metrics",
      "/agent/traces",
      "/agent/alert-fatigue",
      "/agent/slo",
    ]);
    expect(Object.keys(navSections())).toEqual([
      "Respond",
      "Agent",
      "AI",
      "Enterprise",
      "Manage",
    ]);
  });

  it("partitions every navigation route exactly once without empty entries", async () => {
    await renderSettled();

    const routes = Object.values(navSections()).flat();
    expect(routes).toEqual([
      "/now",
      "/incidents",
      "/agent",
      "/agent/services",
      "/agent/logs",
      "/agent/chat",
      "/agent/tools",
      "/agent/runbooks",
      "/agent/decisions",
      "/analyses",
      "/agent/metrics",
      "/agent/traces",
      "/agent/alert-fatigue",
      "/agent/slo",
      "/people",
      "/admin",
      "/settings",
    ]);
    expect(routes).not.toContain("");
    expect(new Set(routes).size).toBe(routes.length);
  });

  it("keeps the existing grouping and order for Enterprise deployments", async () => {
    apiMocks.getSSODeployment.mockResolvedValue({ org: "acme" });
    renderSidebar();
    await screen.findAllByLabelText("Enterprise");
    await waitFor(() => expect(apiMocks.getSSODeployment).toHaveBeenCalledOnce());

    expect(navSections()).toEqual({
      Respond: ["/now", "/incidents"],
      Agent: [
        "/agent",
        "/agent/services",
        "/agent/logs",
        "/agent/metrics",
        "/agent/traces",
      ],
      AI: [
        "/agent/chat",
        "/agent/tools",
        "/agent/runbooks",
        "/agent/decisions",
        "/analyses",
        "/agent/alert-fatigue",
        "/agent/slo",
      ],
      Manage: ["/people", "/admin", "/settings"],
    });
  });

  it("preserves Enterprise-safe grouping after a transient deployment error", async () => {
    apiMocks.getSSODeployment.mockRejectedValue(new Error("network unavailable"));
    renderSidebar();

    await waitFor(() => expect(apiMocks.getSSODeployment).toHaveBeenCalledOnce());
    expect(navSections()["Enterprise"]).toBeUndefined();
    expect(navSections()["Agent"]).toContain("/agent/metrics");
    expect(navSections()["AI"]).toContain("/agent/alert-fatigue");
  });

  it("keeps Chat, Tool catalog, and available Runbooks reachable when enabled", async () => {
    apiMocks.status.mockResolvedValue({ runbooks_available: true });
    await renderSettled();

    for (const [name, href] of [
      ["Chat", "/agent/chat"],
      ["Tool catalog", "/agent/tools"],
      ["Runbooks", "/agent/runbooks"],
    ] as const) {
      const link = screen.getByRole("link", { name });
      expect(link.getAttribute("href")).toBe(href);
      expect(link.getAttribute("title")).toBeNull();
      expect(within(link).queryByLabelText("Enterprise")).toBeNull();
    }
  });

  it("locks execution pages but keeps Tool catalog readable when the agent is disabled", async () => {
    apiMocks.getAgentConfig.mockResolvedValue({ enable: false });
    await renderSettled();

    for (const name of ["Chat", "Runbooks", "Decisions", "Analyses"]) {
      const link = screen.getByRole("link", {
        name: `${name} Enterprise`,
      });
      expect(link.getAttribute("title")).toContain("AI agent is disabled");
      expect(within(link).getByLabelText("Enterprise")).toBeTruthy();
    }

    const tools = screen.getByRole("link", { name: "Tool catalog" });
    expect(tools.getAttribute("href")).toBe("/agent/tools");
    expect(tools.getAttribute("title")).toBeNull();
    expect(within(tools).queryByLabelText("Enterprise")).toBeNull();
  });

  it("keeps unavailable Runbooks reachable with its setup hint", async () => {
    await renderSettled();
    const runbooks = screen.getByRole("link", { name: /Runbooks/ });
    expect(runbooks.getAttribute("href")).toBe("/agent/runbooks");
    expect(runbooks.getAttribute("title")).toContain("configure an embedding model");
    expect(within(runbooks).getByLabelText("Enterprise")).toBeTruthy();
  });
});

describe("Sidebar — expanded row icons", () => {
  const GROUPS = ["Respond", "Agent", "AI", "Enterprise", "Manage"];
  const ITEMS = [
    "Now",
    "Incidents",
    "Overview",
    "Services",
    "Logs",
    "Metrics",
    "Traces",
    "Chat",
    "Tool catalog",
    "Runbooks",
    "Decisions",
    "Analyses",
    "Alert fatigue",
    "SLIs/SLOs",
    "People",
    "Admin",
    "Settings",
  ];

  it("renders expanded group headers as text only", async () => {
    await renderSettled();
    for (const title of GROUPS) {
      const header = screen.getByText(title).closest("div");
      expect(header).not.toBeNull();
      expect(header?.querySelector("svg")).toBeNull();
    }
  });

  it("renders an aria-hidden leading icon on every item without changing names", async () => {
    await renderSettled();
    for (const label of ITEMS) {
      const link = screen.getByText(label).closest("a");
      expect(link).not.toBeNull();
      expect(link?.querySelector('svg[aria-hidden="true"]')).not.toBeNull();
      expect(link?.getAttribute("aria-label")).toBeNull();
    }
    expect(screen.getByRole("link", { name: "Now" })).toBeTruthy();
  });

  it("keeps both the item icon and Enterprise lock SVG on a locked item", async () => {
    await renderSettled();
    const metrics = screen.getByRole("link", { name: /Metrics/ });
    expect(metrics.querySelectorAll("svg")).toHaveLength(2);
    expect(metrics.querySelector('svg[aria-hidden="true"]')).not.toBeNull();
    within(metrics).getByLabelText("Enterprise");
  });
});

// The desktop rail can collapse to a narrow icon-only strip and expand back.
// The collapsed rail shows only the group icons (as links to each zone's
// primary route); the choice persists in localStorage across reloads. This is
// the desktop (lg) rail — the mobile drawer keeps rendering SidebarContent
// expanded, unaffected.
describe("Sidebar desktop rail — collapse / expand toggle", () => {
  beforeEach(() => {
    try {
      window.localStorage.clear();
    } catch {
      // no-op
    }
  });

  function renderRail() {
    const qc = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    return render(
      <QueryClientProvider client={qc}>
        <MemoryRouter
          future={{ v7_startTransition: true, v7_relativeSplatPath: true }}
        >
          <Sidebar />
        </MemoryRouter>
      </QueryClientProvider>,
    );
  }

  it("collapses to a group-icon rail, updates aria-expanded, and persists", async () => {
    renderRail();
    // Expanded: full labeled item link + a collapse control.
    expect(await screen.findByRole("link", { name: "Now" })).toBeTruthy();
    const collapse = screen.getByRole("button", { name: "Collapse sidebar" });
    expect(collapse.getAttribute("aria-expanded")).toBe("true");

    fireEvent.click(collapse);

    // Collapsed: the labeled item is gone; the zone's group-icon link remains,
    // pointing at the zone's primary route.
    expect(screen.queryByRole("link", { name: "Now" })).toBeNull();
    const respondGroup = screen.getByRole("link", { name: "Respond" });
    expect(respondGroup.getAttribute("href")).toBe("/now");

    // The toggle flips to expand and the collapsed state is persisted.
    const expand = screen.getByRole("button", { name: "Expand sidebar" });
    expect(expand.getAttribute("aria-expanded")).toBe("false");
    expect(window.localStorage.getItem("versus.sidebar.collapsed")).toBe("1");
  });

  it("restores the collapsed rail from localStorage on mount", async () => {
    window.localStorage.setItem("versus.sidebar.collapsed", "1");
    renderRail();
    // Boots collapsed: expand control present, labeled item absent.
    expect(
      await screen.findByRole("button", { name: "Expand sidebar" }),
    ).toBeTruthy();
    expect(screen.queryByRole("link", { name: "Now" })).toBeNull();
    expect(screen.getByRole("link", { name: "Respond" })).toBeTruthy();
  });

  // Without this, a collapsed rail can only reach each zone's FIRST item —
  // every other item in the group is unreachable until the rail is expanded.
  it("reveals a zone's other items on hover while collapsed", async () => {
    window.localStorage.setItem("versus.sidebar.collapsed", "1");
    renderRail();

    const respondGroup = await screen.findByRole("link", { name: "Respond" });
    // Closed by default: only the group icon is reachable.
    expect(screen.queryByRole("link", { name: "Incidents" })).toBeNull();

    fireEvent.mouseEnter(respondGroup.parentElement as HTMLElement);

    // The flyout lists the whole zone, not just its primary route.
    const flyout = screen.getByTestId("nav-flyout-respond");
    expect(within(flyout).getByRole("link", { name: "Now" })).toBeTruthy();
    expect(within(flyout).getByRole("link", { name: "Incidents" })).toBeTruthy();

    // Closing is deferred so a pointer crossing to the panel does not dismiss
    // it mid-travel, so this settles rather than flipping synchronously.
    fireEvent.mouseLeave(respondGroup.parentElement as HTMLElement);
    await waitFor(() =>
      expect(screen.queryByTestId("nav-flyout-respond")).toBeNull(),
    );
  });

  it("keeps the OSS Enterprise group and every gated route reachable", async () => {
    window.localStorage.setItem("versus.sidebar.collapsed", "1");
    renderRail();

    const enterpriseGroup = await screen.findByRole("link", {
      name: "Enterprise",
    });
    expect(enterpriseGroup.getAttribute("href")).toBe("/agent/metrics");
    fireEvent.mouseEnter(enterpriseGroup.parentElement as HTMLElement);

    const flyout = screen.getByTestId("nav-flyout-enterprise");
    for (const label of ["Metrics", "Traces", "Alert fatigue", "SLIs/SLOs"]) {
      expect(within(flyout).getByRole("link", { name: new RegExp(label) })).toBeTruthy();
    }
    expect(within(flyout).getByText("Enterprise").closest("div")?.querySelector("svg")).not.toBeNull();
  });
});


