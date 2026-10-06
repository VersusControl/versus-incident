import { expect, test, type Page } from "@playwright/test";
import * as path from "node:path";
import { fileURLToPath } from "node:url";
import { env, openApp, primaryNav } from "./helpers";

// ui-refresh.spec.ts drives the Settings / Admin one-section layouts, the
// Logs / Metrics / Traces filter panel and Flat / By service views, and the
// heatmap scroll containment against a running app. Run it through
// `plans/harness-run/harness.sh e2e ui ui-refresh`; list cases skip, not pass, when
// the stack has no learned rows yet (seed with `harness.sh gen ...`).

const screenshotDir = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "screenshots",
  "ui-refresh",
);
const MOBILE = { width: 390, height: 844 };

async function shot(page: Page, name: string): Promise<void> {
  // Drop hover state and finish row entrance / nav color transitions first.
  await page.mouse.move(0, 0);
  await page.screenshot({
    path: path.join(screenshotDir, `${name}.png`),
    fullPage: false,
    animations: "disabled",
  });
}

function sectionHeading(page: Page) {
  return page.locator("#settings-section-title");
}

test.describe("Settings — one section at a time", () => {
  test("section nav, deep links, and legacy tab links", async ({ page }) => {
    await openApp(page, "/settings");
    await expect(sectionHeading(page)).toHaveText("Server");
    await expect(page.getByText("Configured in YAML").first()).toBeVisible();

    const nav = page.getByRole("navigation", { name: "Settings sections" });
    await nav.getByRole("link", { name: "Spike baseline" }).click();
    await expect(page).toHaveURL(/[?&]section=spike\b/);
    await expect(sectionHeading(page)).toHaveText("Spike baseline");
    await expect(nav.getByRole("link", { name: "Spike baseline" })).toHaveAttribute("aria-current", "page");
    await shot(page, "settings-spike-desktop");

    await page.goto("/settings?tab=agent");
    await expect(page).toHaveURL(/[?&]section=agent-sources\b/);
    await expect(sectionHeading(page)).toHaveText("Data sources");
    await shot(page, "settings-agent-sources-desktop");
  });

  test("mobile uses the section picker", async ({ page }) => {
    await page.setViewportSize(MOBILE);
    await openApp(page, "/settings?section=count-window");
    await expect(sectionHeading(page)).toHaveText("Incident counts");
    const picker = page.getByLabel("Section", { exact: true });
    await expect(picker).toBeVisible();
    await picker.selectOption("reports");
    await expect(page).toHaveURL(/[?&]section=reports\b/);
    await expect(sectionHeading(page)).toHaveText("Incident report");
    await shot(page, "settings-reports-mobile");
  });
});

test.describe("Admin — Enterprise sections", () => {
  test("section nav and the legacy AI settings anchor", async ({ page }) => {
    test.skip(!env.licensed, "Admin controls need a licensed Enterprise scenario");
    await openApp(page, "/admin");
    await expect(sectionHeading(page)).toHaveText("Runtime mode");
    await shot(page, "admin-mode-desktop");

    await page.goto("/admin#agent-ai-settings");
    await expect(page).toHaveURL(/[?&]section=ai\b/);
    await expect(sectionHeading(page)).toHaveText("AI provider");
    await shot(page, "admin-ai-desktop");

    await page.getByRole("navigation", { name: "Admin sections" })
      .getByRole("link", { name: "Members & roles" }).click();
    await expect(sectionHeading(page)).toHaveText("Members & roles");
    await shot(page, "admin-members-desktop");
  });
});

const LISTS = [
  { name: "logs", path: "/agent/logs", seed: "harness.sh gen logs (oss-loki or enterprise-loki)" },
  { name: "metrics", path: "/agent/metrics", seed: "harness.sh gen metrics (enterprise-prometheus)" },
  { name: "traces", path: "/agent/traces", seed: "harness.sh gen traces (enterprise-tempo)" },
] as const;

// openList waits for the list to settle and reports whether it has rows.
async function openList(page: Page, listPath: string): Promise<"locked" | "empty" | "rows"> {
  await openApp(page, listPath);
  const locked = page.getByText(/is an Enterprise capability/).first();
  const rows = page.getByRole("button", { name: /^View / }).first();
  const empty = page.getByText(/Nothing learned yet|No patterns learned yet/).first();
  return Promise.race([
    locked.waitFor({ timeout: 30_000 }).then(() => "locked" as const),
    rows.waitFor({ timeout: 30_000 }).then(() => "rows" as const),
    empty.waitFor({ timeout: 30_000 }).then(() => "empty" as const),
  ]);
}

for (const list of LISTS) {
  test.describe(`${list.name} list — filters and grouping`, () => {
    test("By service groups rows and expands a group", async ({ page }) => {
      const state = await openList(page, list.path);
      test.skip(state === "locked", `${list.name} learning needs a licensed Enterprise scenario`);
      test.skip(state === "empty", `no learned ${list.name} yet; seed with ${list.seed}`);

      const byService = page.getByRole("tab", { name: "By service" });
      await expect(byService).toHaveAttribute("aria-selected", "true");
      await expect(page.getByRole("tablist", { name: "List view" }).getByRole("tab").first()).toHaveText("By service");
      await expect(page.locator("tr[data-group-header]").first()).toBeVisible();
      await byService.click();
      await expect(page).not.toHaveURL(/[?&]group=/);
      const headers = page.locator("tr[data-group-header]");
      await expect(headers.first()).toBeVisible();

      const expand = page.getByRole("button", { name: /^Expand / }).first();
      if (!(await expand.count())) {
        await page.getByRole("button", { name: /^Collapse / }).first().click();
      }
      await expand.click();
      await expect(page.getByRole("button", { name: /^Collapse / }).first()).toHaveAttribute("aria-expanded", "true");
      await expect(page.getByRole("button", { name: /^View / }).first()).toBeVisible();
      await shot(page, `${list.name}-grouped-desktop`);

      await page.getByRole("tab", { name: "Flat" }).click();
      await expect(page).toHaveURL(/[?&]group=flat\b/);
      await expect(headers).toHaveCount(0);
      await page.reload();
      await expect(page.getByRole("tab", { name: "Flat" })).toHaveAttribute("aria-selected", "true");
      await expect(headers).toHaveCount(0);
    });

    test("filter panel applies a service filter as a chip", async ({ page }) => {
      const state = await openList(page, list.path);
      test.skip(state === "locked", `${list.name} learning needs a licensed Enterprise scenario`);
      test.skip(state === "empty", `no learned ${list.name} yet; seed with ${list.seed}`);

      const button = page.getByRole("button", { name: /^Filters/ });
      await button.click();
      const panel = page.getByRole("dialog", { name: "Filters" });
      await expect(panel).toBeVisible();
      const service = panel.getByRole("group", { name: /Service/ });
      const first = service.getByRole("checkbox").first();
      const selectedService = await first.locator("..").locator("span").first().innerText();
      await first.check();
      await expect(first).toBeChecked();
      await expect(page).toHaveURL(/[?&]f\.service=/);
      await expect(panel.getByText("1 applied")).toBeVisible();
      await shot(page, `${list.name}-filters-open-desktop`);

      await page.keyboard.press("Escape");
      await expect(panel).toBeHidden();
      await expect(button).toBeFocused();
      await page.getByRole("tab", { name: "Flat" }).click();
      const visibleRows = page.locator("tbody tr").filter({ has: page.getByRole("button", { name: /^View / }) });
      await expect(visibleRows.first()).toBeVisible();
      for (const row of await visibleRows.all()) await expect(row).toContainText(selectedService);
      const chip = page.getByRole("button", { name: /^Remove filter Service:/ });
      await expect(chip).toBeVisible();
      await shot(page, `${list.name}-filtered-desktop`);

      await chip.click();
      await expect(page).not.toHaveURL(/[?&]f\.service=/);
      await page.setViewportSize(MOBILE);
      await openApp(page, list.path);
      await button.click();
      await expect(panel).toBeVisible();
      await shot(page, `${list.name}-filters-open-mobile`);
      const bounds = await panel.boundingBox();
      expect(bounds).not.toBeNull();
      expect(bounds!.x).toBeGreaterThanOrEqual(0);
      expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(MOBILE.width);
    });

    test("learning scope is panel-only and clears to Active", async ({ page }) => {
      test.skip(!env.licensed, "Learning scope controls need a licensed Enterprise scenario");
      await openApp(page, list.path);
      await expect(page.getByRole("tablist", { name: "Learning scope" })).toHaveCount(0);
      await page.getByRole("button", { name: /^Filters/ }).click();
      const panel = page.getByRole("dialog", { name: "Filters" });
      const scope = panel.getByRole("tablist", { name: "Learning scope" });
      await expect(page.getByRole("button", { name: "Filters", exact: true })).toHaveText("Filters");
      await expect(scope.getByRole("tab", { name: /^Active\b/ })).toHaveAttribute("aria-selected", "true");
      await scope.getByRole("tab", { name: /^Ignored\b/ }).click();
      await expect(page).toHaveURL(/[?&]scope=ignored\b/);
      await expect(panel.getByText("1 applied", { exact: true })).toBeVisible();
      await shot(page, `${list.name}-scope-filters-desktop`);
      await panel.getByRole("button", { name: "Clear all", exact: true }).click();
      await expect(page).not.toHaveURL(/[?&]scope=/);
      await expect(scope.getByRole("tab", { name: /^Active\b/ })).toHaveAttribute("aria-selected", "true");
      await expect(panel.getByText("No filters applied", { exact: true })).toBeVisible();
      await expect(page.getByRole("button", { name: "Filters", exact: true })).toHaveText("Filters");
      await expect(panel.getByRole("button", { name: "Clear all", exact: true })).toBeDisabled();
      await shot(page, `${list.name}-scope-cleared-desktop`);
      await page.setViewportSize(MOBILE);
      await openApp(page, `${list.path}?scope=ignored`);
      await page.getByRole("button", { name: /^Filters/ }).click();
      await expect(scope.getByRole("tab", { name: /^Ignored\b/ })).toHaveAttribute("aria-selected", "true");
      await shot(page, `${list.name}-scope-filters-mobile`);
      const bounds = await panel.boundingBox();
      expect(bounds).not.toBeNull();
      expect(bounds!.x).toBeGreaterThanOrEqual(0);
      expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(MOBILE.width);
      await panel.getByRole("button", { name: "Clear all", exact: true }).click();
      await expect(page).not.toHaveURL(/[?&]scope=/);
      await expect(scope.getByRole("tab", { name: /^Active\b/ })).toHaveAttribute("aria-selected", "true");
      expect(await panel.innerText()).toContain("No filters applied");
      await expect(page.getByRole("button", { name: "Filters", exact: true })).toHaveText("Filters");
      await expect(panel.getByRole("button", { name: "Clear all", exact: true })).toBeDisabled();
      await shot(page, `${list.name}-scope-cleared-mobile`);
    });

    test("grouping defaults normalize without requiring learned rows", async ({ page }) => {
      const state = await openList(page, `${list.path}?group=unknown`);
      test.skip(state === "locked", `${list.name} learning needs a licensed Enterprise scenario`);
      const views = page.getByRole("tablist", { name: "List view" });
      await expect(views.getByRole("tab").first()).toHaveText("By service");
      await expect(views.getByRole("tab", { name: "By service" })).toHaveAttribute("aria-selected", "true");
      await views.getByRole("tab", { name: "Flat" }).click();
      await expect(page).toHaveURL(/[?&]group=flat\b/);
      await page.reload();
      await expect(views.getByRole("tab", { name: "Flat" })).toHaveAttribute("aria-selected", "true");
      await views.getByRole("tab", { name: "By service" }).click();
      await expect(page).not.toHaveURL(/[?&]group=/);
      await page.reload();
      await expect(views.getByRole("tab", { name: "By service" })).toHaveAttribute("aria-selected", "true");
    });

    test("grouped view on mobile", async ({ page }) => {
      await page.setViewportSize(MOBILE);
      const state = await openList(page, `${list.path}?group=service`);
      test.skip(state === "locked", `${list.name} learning needs a licensed Enterprise scenario`);
      test.skip(state === "empty", `no learned ${list.name} yet; seed with ${list.seed}`);
      await expect(page.locator("tr[data-group-header]").first()).toBeVisible();
      await shot(page, `${list.name}-grouped-mobile`);
    });

    test("unknown grouping falls back to By service", async ({ page }) => {
      const state = await openList(page, `${list.path}?group=unknown`);
      test.skip(state === "locked", `${list.name} learning needs a licensed Enterprise scenario`);
      test.skip(state === "empty", `no learned ${list.name} yet; seed with ${list.seed}`);
      await expect(page.getByRole("tab", { name: "By service" })).toHaveAttribute("aria-selected", "true");
      await expect(page.locator("tr[data-group-header]").first()).toBeVisible();
    });
  });
}

test.describe("Now — Agent pulse", () => {
  test("shows activity counts and baseline readiness on desktop and mobile", async ({ page }) => {
    await openApp(page, "/now");
    const response = await page.request.get("/api/agent/status");
    expect(response.ok()).toBeTruthy();
    const status = await response.json();
    const pulse = page.getByRole("region", { name: "Agent pulse" });
    await expect(pulse.getByRole("heading", { name: "Signal activity" })).toBeVisible();
    await expect(pulse.getByText("Current catalog · recorded events")).toBeVisible();
    await expect(pulse.getByRole("link", { name: new RegExp(`^Logs ${status.patterns} patterns$`) })).toHaveAttribute("href", "/agent/logs");
    await expect(pulse.getByRole("link", { name: /Metrics 48 signals 36 ready/ })).toHaveAttribute("href", "/agent/metrics");
    await expect(pulse.getByRole("link", { name: /Traces 24 signals 24 ready/ })).toHaveAttribute("href", "/agent/traces");
    await expect(pulse.getByRole("link", { name: new RegExp(`^Shadow ${status.shadow_events ?? 0} events$`) })).toHaveAttribute("href", "/agent/decisions?tab=shadow");
    await expect(pulse.getByRole("link", { name: new RegExp(`^Detect ${status.detect_events ?? 0} events$`) })).toHaveAttribute("href", "/agent/decisions?tab=detect");
    await expect(pulse.getByText("Runtime mode", { exact: true })).toHaveCount(0);
    await expect(pulse.getByText("Configured AI model", { exact: true })).toHaveCount(0);
    await pulse.scrollIntoViewIfNeeded();
    await shot(page, "now-agent-pulse-desktop");

    await page.setViewportSize(MOBILE);
    await expect(pulse.getByRole("heading", { name: "Signal activity" })).toBeVisible();
    await expect(pulse.getByRole("link", { name: /^Logs / })).toBeVisible();
    await expect(pulse.getByRole("link", { name: /^Metrics / })).toBeVisible();
    await expect(pulse.getByRole("link", { name: /^Traces / })).toBeVisible();
    await expect(pulse.getByRole("link", { name: /^Shadow / })).toBeVisible();
    await expect(pulse.getByRole("link", { name: /^Detect / })).toBeVisible();
    await pulse.scrollIntoViewIfNeeded();
    await shot(page, "now-agent-pulse-mobile");
  });
});

const STATUS_PANELS = [
  { name: "incidents", path: "/incidents", group: "Filter incidents by status", selected: "Resolved", value: "resolved", initial: "Open" },
  { name: "analyses", path: "/analyses", group: "Call status filter", selected: "Error", value: "error", initial: "All" },
] as const;

for (const surface of STATUS_PANELS) {
  test(`${surface.name} status is panel-only, URL-backed, and clearable`, async ({ page }) => {
    await openApp(page, surface.path);
    await expect(page.getByRole("tablist", { name: surface.group })).toHaveCount(0);
    const trigger = page.getByRole("button", { name: /^Filters/ });
    await trigger.click();
    const panel = page.getByRole("dialog", { name: "Filters" });
    const tabs = panel.getByRole("tablist", { name: surface.group });
    await expect(tabs.getByRole("tab", { name: new RegExp(`^${surface.initial}\\b`) })).toHaveAttribute("aria-selected", "true");
    await tabs.getByRole("tab", { name: new RegExp(`^${surface.selected}\\b`) }).click();
    await expect(page).toHaveURL(new RegExp(`[?&]status=${surface.value}\\b`));
    await expect(panel.getByText("1 applied", { exact: true })).toBeVisible();
    await shot(page, `${surface.name}-status-filters-desktop`);
    await page.keyboard.press("Escape");
    await expect(panel).toBeHidden();
    await expect(trigger).toBeFocused();
    await page.reload();
    await trigger.click();
    await expect(tabs.getByRole("tab", { name: new RegExp(`^${surface.selected}\\b`) })).toHaveAttribute("aria-selected", "true");
    await page.setViewportSize(MOBILE);
    await openApp(page, `${surface.path}?status=${surface.value}`);
    await trigger.click();
    await shot(page, `${surface.name}-status-filters-mobile`);
    const bounds = await panel.boundingBox();
    expect(bounds).not.toBeNull();
    expect(bounds!.x).toBeGreaterThanOrEqual(0);
    expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(MOBILE.width);
    if (surface.name === "analyses") {
      for (const control of [...await tabs.getByRole("tab").all(), panel.getByRole("button", { name: "Clear all", exact: true })]) {
        const controlBounds = await control.boundingBox();
        expect(controlBounds).not.toBeNull();
        expect(controlBounds!.x).toBeGreaterThanOrEqual(0);
        expect(controlBounds!.x + controlBounds!.width).toBeLessThanOrEqual(MOBILE.width);
      }
    }
    expect(await page.evaluate(() => window.scrollX)).toBe(0);
    console.log(`${surface.name} mobile filter bounds: ${JSON.stringify(bounds)}`);
    await panel.getByRole("button", { name: "Clear all", exact: true }).click();
    await expect(page).not.toHaveURL(/[?&]status=/);
    await expect(tabs.getByRole("tab", { name: new RegExp(`^${surface.initial}\\b`) })).toHaveAttribute("aria-selected", "true");
    await openApp(page, `${surface.path}?status=unknown`);
    await trigger.click();
    await expect(tabs.getByRole("tab", { name: new RegExp(`^${surface.initial}\\b`) })).toHaveAttribute("aria-selected", "true");
    await expect(panel.getByText("No filters applied", { exact: true })).toBeVisible();
    await expect(panel.getByRole("button", { name: "Clear all", exact: true })).toBeDisabled();
  });
}

test("detect outcomes are panel-only and clearable on desktop and mobile", async ({ page }) => {
  await openApp(page, "/agent/decisions?tab=detect");
  await expect(page.getByRole("group", { name: "Outcome filter" })).toHaveCount(0);
  await page.getByRole("button", { name: /^Filters/ }).click();
  const panel = page.getByRole("dialog", { name: "Filters" });
  const outcomes = panel.getByRole("group", { name: "Outcome filter" });
  const alternative = outcomes.getByRole("button").nth(1);
  await alternative.click();
  await expect(alternative).toHaveAttribute("aria-pressed", "true");
  await expect(panel.getByText("1 applied", { exact: true })).toBeVisible();
  await shot(page, "decisions-outcome-filters-desktop");
  await page.setViewportSize(MOBILE);
  await panel.scrollIntoViewIfNeeded();
  const bounds = await panel.boundingBox();
  expect(bounds).not.toBeNull();
  expect(bounds!.x).toBeGreaterThanOrEqual(0);
  expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(MOBILE.width);
  await shot(page, "decisions-outcome-filters-mobile");
  await panel.getByRole("button", { name: "Clear all", exact: true }).click();
  await expect(outcomes.getByRole("button").first()).toHaveAttribute("aria-pressed", "true");
  await expect(panel.getByText("No filters applied", { exact: true })).toBeVisible();
});

test.describe("Agent overview — heatmap scroll containment", () => {
  test("scrolling the heatmap never scrolls the app frame", async ({ page }) => {
    await openApp(page, "/agent");
    const canvas = page.locator(".heatmap-canvas").first();
    const present = await canvas.waitFor({ timeout: 20_000 }).then(() => true, () => false);
    const tiles = canvas.locator("button, a");
    test.skip(!present || (await tiles.count()) === 0, "no service heatmap tiles on this stack; seed services first");

    const lastTile = tiles.last();
    await lastTile.scrollIntoViewIfNeeded();
    await lastTile.focus();
    const frameScroll = await page.evaluate(() => ({
      html: document.documentElement.scrollTop,
      root: document.getElementById("root")?.scrollTop ?? 0,
    }));
    expect(frameScroll).toEqual({ html: 0, root: 0 });
    const navBox = await primaryNav(page).boundingBox();
    expect(navBox?.y ?? 0).toBeGreaterThanOrEqual(0);
    await shot(page, "agent-heatmap-scrolled-desktop");
  });
});
