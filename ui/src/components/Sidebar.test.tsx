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

