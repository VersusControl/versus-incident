// @vitest-environment jsdom
import { describe, it, expect, afterEach, beforeEach, vi } from "vitest";
import {
  render,
  screen,
  cleanup,
  fireEvent,
  within,
} from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
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

// Nav regrouping — the agent's reasoning surfaces (Decisions, Analyses,
// SLIs/SLOs) live in their own "AI" nav section, distinct from the raw
// learned-catalog "Agent" views, and the SLIs/SLOs entry stays enterprise-
// locked. This is pinned by RENDERED BEHAVIOUR (the section a user sees and
// its links) rather than the internal source array name — a source-string
// pin re-broke when the array was renamed `const aiZone` → `const ai`, so we
// assert what renders instead of how the file happens to be written.
//
// The agent is enabled (nothing dimmed) and the baselines probe answers 403
// (OSS / unlicensed) so the enterprise lock on SLIs/SLOs is exercised.
vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      getAgentConfig: vi.fn().mockResolvedValue({ enable: true }),
      status: vi.fn().mockResolvedValue({ runbooks_available: false }),
      // 403 → unlicensed → enterpriseLocked === true → SLIs/SLOs shows a lock.
      listBaselines: vi.fn().mockRejectedValue(new actual.ApiError(403, "forbidden")),
    },
  };
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
      <MemoryRouter>
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

describe("Sidebar — the AI nav section groups the agent's reasoning surfaces", () => {
  it("renders an 'AI' section holding Decisions, Analyses and SLIs/SLOs in order", async () => {
    await renderSettled();
    // The SLIs/SLOs entry carries the enterprise lock (probe answered 403).
    const slo = screen.getByRole("link", { name: /SLIs\/SLOs/ });
    within(slo).getByLabelText("Enterprise");

    expect(navSections()["AI"]).toEqual([
      "/agent/decisions",
      "/analyses",
      "/agent/slo",
    ]);
  });

  it("moved Decisions and SLIs/SLOs OUT of the Agent section", async () => {
    await renderSettled();

    const sections = navSections();
    expect(sections["Agent"]).not.toContain("/agent/decisions");
    expect(sections["Agent"]).not.toContain("/agent/slo");
    // The Agent section still holds the raw learned-catalog views.
    expect(sections["Agent"]).toContain("/agent/logs");
  });

  it("keeps SLIs/SLOs enterprise-locked while Decisions stays ungated", async () => {
    await renderSettled();
    const slo = screen.getByRole("link", { name: /SLIs\/SLOs/ });
    // The lock icon (aria-label 'Enterprise') is the gated affordance.
    expect(within(slo).getByLabelText("Enterprise")).toBeTruthy();
    // Decisions is ungated — no lock.
    const decisions = screen.getByRole("link", { name: /Decisions/ });
    expect(within(decisions).queryByLabelText("Enterprise")).toBeNull();
  });
});

// The AI section also carries greenlit-but-unbuilt capabilities (secret
// scanning, fraud detection, alert fatigue). They render as in-development
// placeholders: non-navigable rows (not router links), aria-disabled, with a
// "Dev" badge and an "In development" tooltip. They must never become
// clickable — even though the AI zone is wrapped in applyAgentOff, SideLink
// short-circuits on inDev before any dim/lock logic runs.
describe("Sidebar — in-development AI placeholders", () => {
  const PLACEHOLDERS: Array<{ label: string; testid: string }> = [
    { label: "Secret scanning", testid: "nav-indev-secret-scanning" },
    { label: "Fraud detection", testid: "nav-indev-fraud-detection" },
    { label: "Alert fatigue", testid: "nav-indev-alert-fatigue" },
  ];

  it("renders all placeholders as non-clickable, aria-disabled rows with a Dev badge and stable testid", async () => {
    renderSidebar();
    for (const { label, testid } of PLACEHOLDERS) {
      const text = await screen.findByText(label);
      // Not a router link — no link role for these placeholders.
      expect(screen.queryByRole("link", { name: label })).toBeNull();
      // The row is a disabled, non-navigable element (a div, not an <a>).
      const row = text.closest("[aria-disabled='true']");
      expect(row).not.toBeNull();
      expect(row?.tagName).toBe("DIV");
      // Carries its stable nav-indev-* testid.
      expect(row?.getAttribute("data-testid")).toBe(testid);
      // Carries the in-development tooltip and a visible "Dev" indicator.
      expect(row?.getAttribute("title")).toBe("In development — coming soon");
      expect(within(row as HTMLElement).getByText("Dev")).toBeTruthy();
    }
  });

  it("groups the placeholders under the 'AI' nav section and contributes no navigable href", async () => {
    renderSidebar();
    await screen.findByText("Secret scanning");
    // The AI section's navigable hrefs are only the real routes — the in-dev
    // placeholders add no <a>, so the AI href list is unchanged.
    expect(navSections()["AI"]).toEqual([
      "/agent/decisions",
      "/analyses",
      "/agent/slo",
    ]);
    // No section holds an empty ("") href from a placeholder rendered as a link.
    for (const hrefs of Object.values(navSections())) {
      expect(hrefs).not.toContain("");
    }
    // The removed Security section no longer renders.
    const nav = screen.getByRole("navigation", { name: "Primary" });
    expect(within(nav).queryByText("Security")).toBeNull();
  });
});

// Icons live on the GROUP headers, not on individual items. Each zone header
// (Respond / Agent / AI / Tools / Manage) carries a representative Lucide icon
// beside its title, while individual nav rows are text-only — the leading
// per-item icon was removed. The active accent bar, the enterprise Lock badge,
// the dim styling and the in-dev "Dev" chip all stay intact.
describe("Sidebar — icons on group headers, not on items", () => {
  const GROUPS = ["Respond", "Agent", "AI", "Tools", "Manage"];

  it("renders an icon beside every group header title", async () => {
    await renderSettled();
    for (const title of GROUPS) {
      const header = screen.getByText(title).closest("div");
      expect(header).not.toBeNull();
      // The header carries exactly one leading icon (an <svg>) next to its text.
      expect(header?.querySelector("svg")).not.toBeNull();
    }
  });

  it("renders plain nav items with no leading icon", async () => {
    await renderSettled();
    // An ordinary, ungated item is text-only — no svg at all.
    const now = screen.getByRole("link", { name: "Now" });
    expect(now.querySelector("svg")).toBeNull();
  });

  it("keeps the enterprise Lock badge on a locked item (its only svg)", async () => {
    await renderSettled();
    // A locked item drops its leading icon but KEEPS the Lock badge — so the
    // single remaining svg is the lock, not a per-item nav icon.
    const metrics = screen.getByRole("link", { name: /Metrics/ });
    expect(metrics.querySelectorAll("svg")).toHaveLength(1);
    within(metrics).getByLabelText("Enterprise");
  });

  it("keeps the in-dev 'Dev' chip on placeholder rows and no leading icon", async () => {
    renderSidebar();
    const label = await screen.findByText("Secret scanning");
    const row = label.closest("[aria-disabled='true']") as HTMLElement;
    // The Dev chip is still there…
    within(row).getByText("Dev");
    // …and the row has no leading nav icon (the accent bar span is not an svg).
    expect(row.querySelector("svg")).toBeNull();
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
        <MemoryRouter>
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
});


