// @vitest-environment jsdom
import { describe, it, expect, afterEach, vi } from "vitest";
import { render, screen, cleanup, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { SidebarContent } from "./Sidebar";

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
