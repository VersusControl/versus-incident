import { expect, test, type Page } from "@playwright/test";
import * as fs from "node:fs";
import * as path from "node:path";
import { fileURLToPath } from "node:url";
import { env, openApp, primaryNav } from "./helpers";
import { captureDir, capturePath } from "./support/capture";

// ui-refresh.spec.ts drives the Settings / Admin one-section layouts, the
// Logs / Metrics / Traces filter panel and Flat / By service views, and the
// heatmap scroll containment against a running app. Run it through
// `plans/harness-run/harness.sh e2e ui ui-refresh`; list cases skip, not pass, when
// the stack has no learned rows yet (seed with `harness.sh gen ...`).

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const screenshotDir = captureDir();
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

// Docs quickstart captures for the documentation pages. Each case
// runs only against the matching examples/quickstart stack, selected with
// E2E_DOCS_QUICKSTART=sre-patterns|sre-incident|webhook|alert-fatigue|
// sre-shadow|sre-detect|sre-learning|sre-ai, and writes light and dark 1440-wide captures for versus-incident/src/docs/images/.
const DOCS_QUICKSTART = process.env.E2E_DOCS_QUICKSTART ?? "";
const DOCS_DESKTOP = { width: 1440, height: 1000 };
const DOCS_THEMES = ["light", "dark"] as const;

async function docsTheme(page: Page, theme: (typeof DOCS_THEMES)[number]): Promise<void> {
  await page.emulateMedia({ colorScheme: theme });
  await page.addInitScript((t) => localStorage.setItem("versus.theme", t), theme);
  await page.setViewportSize(DOCS_DESKTOP);
}

async function docsShot(page: Page, surface: string, theme: string, state: string): Promise<void> {
  await expect.poll(() => page.locator("html").getAttribute("data-theme")).toBe(theme === "light" ? "light" : null);
  await page.mouse.move(0, 0);
  await page.waitForTimeout(400);
  await page.screenshot({ path: capturePath(surface, DOCS_DESKTOP.width, theme, state), fullPage: false, animations: "disabled" });
}

async function dismissToasts(page: Page): Promise<void> {
  const dismiss = page.getByRole("button", { name: "Dismiss notification" });
  while (await dismiss.count()) await dismiss.first().click();
  await expect(dismiss).toHaveCount(0);
}

type DocsIncident = { id: string; title?: string; origin?: string; status?: string };

async function findIncident(page: Page, match: (i: DocsIncident) => boolean): Promise<DocsIncident> {
  let found: DocsIncident | undefined;
  await expect.poll(async () => {
    const res = await page.request.get("/api/admin/incidents?limit=200");
    if (!res.ok()) return false;
    const body = await res.json();
    const items: DocsIncident[] = Array.isArray(body) ? body : body.incidents ?? body.items ?? [];
    found = items.find(match);
    return Boolean(found);
  }, { timeout: 30_000 }).toBe(true);
  return found!;
}

test.describe("Docs quickstart captures", () => {
  for (const theme of DOCS_THEMES) {
    test(`sre patterns learned in training ${theme}`, async ({ page }) => {
      test.skip(DOCS_QUICKSTART !== "sre-patterns", "needs the sre-agent quickstart stack in training");
      await docsTheme(page, theme);
      const state = await openList(page, "/agent/logs");
      expect(state).toBe("rows");
      await expect(page.getByRole("button", { name: /^View / }).nth(4)).toBeVisible();
      await docsShot(page, "quickstart-sre-patterns", theme, "training");
    });

    test(`sre detected incident ${theme}`, async ({ page }) => {
      test.skip(DOCS_QUICKSTART !== "sre-incident", "needs the sre-agent quickstart stack after the error burst");
      await docsTheme(page, theme);
      await openApp(page, "/incidents");
      const incident = await findIncident(page, (i) => i.origin === "ai_detect" && /acme-pay/.test(i.title ?? ""));
      await page.goto(`/incidents/${incident.id}`);
      await expect(page.getByText(/acme-pay/).first()).toBeVisible();
      await docsShot(page, "quickstart-sre-incident", theme, "ai-detect");
    });

    test(`webhook HighErrorRate incident ${theme}`, async ({ page }) => {
      test.skip(DOCS_QUICKSTART !== "webhook", "needs the webhook quickstart stack after the sample alert");
      await docsTheme(page, theme);
      await openApp(page, "/incidents?origin=webhook&status=all");
      await expect(page.getByText("HighErrorRate").first()).toBeVisible();
      await docsShot(page, "quickstart-webhook-incident", theme, "list");
      const incident = await findIncident(page, (i) => /HighErrorRate/.test(i.title ?? ""));
      await page.goto(`/incidents/${incident.id}`);
      await expect(page.getByText("HighErrorRate").first()).toBeVisible();
      await docsShot(page, "quickstart-webhook-incident", theme, "detail");
    });

    test(`webhook alert fatigue fingerprint ${theme}`, async ({ page }) => {
      test.skip(DOCS_QUICKSTART !== "alert-fatigue", "needs the webhook Enterprise overlay after the warning alerts");
      await docsTheme(page, theme);
      await openApp(page, "/agent/alert-fatigue");
      const locked = page.getByText(/is an Enterprise capability/).first();
      const config = page.getByTestId("alert-fatigue-config");
      await expect(locked.or(config).first()).toBeVisible({ timeout: 30_000 });
      if (await config.isVisible()) {
        // README step 7: turn Alert fatigue on, then send the warning alert five times.
        const enable = page.getByTestId("alert-fatigue-enable-toggle");
        if ((await enable.getAttribute("aria-checked")) !== "true") await enable.click();
        await expect(enable).toHaveAttribute("aria-checked", "true");
        const rows = page.locator("table.ddt tbody tr");
        const empty = page.getByText("No fingerprints yet");
        await expect(rows.first().or(empty).first()).toBeVisible();
        if (await empty.isVisible()) {
          const payload = fs.readFileSync(path.resolve(__dirname, "../../../examples/quickstart/webhook/payloads/alertmanager-warning.json"), "utf8");
          for (let i = 0; i < 5; i++) {
            const res = await page.request.post("/api/incidents", { headers: { "Content-Type": "application/json" }, data: payload });
            expect(res.status()).toBe(201);
          }
          await page.reload();
        }
        const fatigued = rows.filter({ hasText: "Fatigued" }).first();
        await expect(fatigued).toBeVisible();
        await expect(fatigued.locator("td").filter({ hasText: /^5$/ })).toHaveCount(1);
        await docsShot(page, "quickstart-webhook-alert-fatigue", theme, "fatigued");
        await fatigued.locator("td").first().locator("button").click();
        await expect(page.getByText("Fingerprint detail").first()).toBeVisible();
        await docsShot(page, "quickstart-webhook-alert-fatigue", theme, "fingerprint-detail");
      } else {
        await docsShot(page, "quickstart-webhook-alert-fatigue", theme, "locked");
      }
    });

    test(`sre shadow decisions ${theme}`, async ({ page }) => {
      test.skip(DOCS_QUICKSTART !== "sre-shadow", "needs the sre-agent stack in shadow after the checkout burst and a known-pattern spike");
      await docsTheme(page, theme);
      await openApp(page, "/agent/decisions?tab=shadow");
      const table = page.locator("table.ddt tbody");
      await expect(table.locator("tr").filter({ hasText: "checkout-api" }).first()).toBeVisible({ timeout: 30_000 });
      await expect(table.getByText("unknown", { exact: true }).first()).toBeVisible();
      await expect(table.getByText("spike", { exact: true }).first()).toBeVisible();
      await docsShot(page, "sre-shadow", theme, "decisions");
    });

    test(`sre detect decisions with side panel ${theme}`, async ({ page }) => {
      test.skip(DOCS_QUICKSTART !== "sre-detect", "needs the sre-agent stack in detect after the checkout burst");
      await docsTheme(page, theme);
      await openApp(page, "/agent/decisions?tab=detect");
      const row = page.locator("table.ddt tbody tr").filter({ hasText: "checkout-api" }).filter({ hasText: "unknown" }).first();
      await expect(row).toBeVisible({ timeout: 30_000 });
      await row.getByRole("button", { name: /^View detect event / }).click();
      await expect(page.getByText("Why it tripped").or(page.getByText("Sample", { exact: true })).first()).toBeVisible();
      await expect(page.getByRole("link", { name: /Open full page/ })).toBeVisible();
      await docsShot(page, "sre-detect", theme, "decisions-peek");
    });

    test(`sre spike decisions ${theme}`, async ({ page }) => {
      test.skip(DOCS_QUICKSTART !== "sre-detect", "needs shadow and detect spikes on the sre-agent stack");
      await docsTheme(page, theme);
      await openApp(page, "/agent/decisions?tab=spike");
      const table = page.locator("table.ddt tbody");
      await expect(table.getByText("AI-detect", { exact: true }).first()).toBeVisible({ timeout: 30_000 });
      await expect(table.getByText("Shadow", { exact: true }).first()).toBeVisible();
      await docsShot(page, "sre-spike", theme, "decisions");
    });

    test(`sre spike pattern what's normal ${theme}`, async ({ page }) => {
      test.skip(DOCS_QUICKSTART !== "sre-detect", "needs a well-trained pattern on the sre-agent stack");
      await docsTheme(page, theme);
      await openApp(page, "/agent/logs");
      let id = "";
      await expect.poll(async () => {
        const res = await page.request.get("/api/agent/patterns?limit=500");
        if (!res.ok()) return "";
        const body = await res.json();
        const items: Array<{ id: string; template?: string; count?: number }> = body.patterns ?? [];
        id = items.find((p) => /failed to enqueue job/.test(p.template ?? "") && (p.count ?? 0) >= 20)?.id ?? "";
        return id;
      }, { timeout: 30_000 }).not.toBe("");
      await page.goto(`/agent/logs/${encodeURIComponent(id)}`);
      const normal = page.getByText("What's normal", { exact: true });
      await expect(normal).toBeVisible({ timeout: 30_000 });
      await expect(page.getByLabel("Learned rate by hour of day")).toBeVisible();
      await normal.evaluate((el) => el.scrollIntoView({ block: "start" }));
      await docsShot(page, "sre-spike-normal", theme, "pattern");
    });

    test(`sre services learning ${theme}`, async ({ page }) => {
      test.skip(DOCS_QUICKSTART !== "sre-learning", "needs the sre-agent stack with checkout-api, orders and payments-api");
      await docsTheme(page, theme);
      await openApp(page, "/agent/services");
      // payments-api is added by hand; orders is auto-discovered and its grace
      // is restarted so the capture shows one service still in grace.
      // Same-origin fetch from the app, exactly as the Services page sends it.
      const post = (url: string, body: unknown) => page.evaluate(async ([u, b]) => {
        const res = await fetch(u as string, { method: "POST", credentials: "same-origin", headers: { "Content-Type": "application/json" }, body: JSON.stringify(b) });
        return res.status;
      }, [url, body] as const);
      expect([200, 201, 409]).toContain(await post("/api/agent/services", { name: "payments-api" }));
      expect(await post("/api/agent/services/orders/grace", { action: "restart" })).toBeLessThan(300);
      await page.reload();
      const table = page.locator("table tbody");
      for (const name of ["checkout-api", "orders", "payments-api"]) {
        await expect(table.locator("tr").filter({ has: page.getByText(name, { exact: true }) }).first()).toBeVisible({ timeout: 30_000 });
      }
      const orders = table.locator("tr").filter({ has: page.getByText("orders", { exact: true }) }).first();
      await expect(orders.getByText("in grace")).toBeVisible();
      const payments = table.locator("tr").filter({ has: page.getByText("payments-api", { exact: true }) }).first();
      await expect(payments.getByText("Manual")).toBeVisible();
      await expect(table.getByText("in grace", { exact: true })).toHaveCount(1);
      await docsShot(page, "sre-services", theme, "list");
    });

    test(`sre service heatmap ${theme}`, async ({ page }) => {
      test.skip(DOCS_QUICKSTART !== "sre-learning", "needs the sre-agent stack with one active checkout-api incident");
      test.setTimeout(180_000);
      await docsTheme(page, theme);
      await openApp(page, "/agent");
      const canvas = page.locator(".heatmap-canvas").first();
      await expect(canvas).toBeVisible({ timeout: 30_000 });
      await expect(page.getByTestId("service-health-preview")).toHaveCount(0);
      // A service with an active incident reads Degraded; the rest stay Normal.
      const checkout = canvas.getByRole("button", { name: "Inspect checkout-api" });
      await expect(checkout).toBeVisible({ timeout: 30_000 });
      await expect(checkout.getByText("Degraded")).toBeVisible({ timeout: 30_000 });
      await expect(canvas.getByText("Normal").first()).toBeVisible();
      await expect.poll(async () => {
        await page.getByRole("button", { name: "Refresh service health" }).click();
        return canvas.locator('[data-impact="degraded"]').count();
      }, { timeout: 120_000, intervals: [5_000] }).toBe(1);
      await page.locator("#service-health-title").evaluate((el) => el.scrollIntoView({ block: "start" }));
      await docsShot(page, "sre-heatmap", theme, "overview");
    });

    // Data-source pages (src/agent/data-sources/*.md). Select with a comma
    // list such as E2E_DOCS_QUICKSTART=ds-elasticsearch or ds-prometheus,ds-settings.
    const dsModes = new Set(DOCS_QUICKSTART.split(","));
    const dsLogs = ["ds-file", "ds-elasticsearch", "ds-graylog", "ds-splunk", "ds-signoz"].find((m) => dsModes.has(m));

    test(`data source learned log patterns ${theme}`, async ({ page }) => {
      test.skip(!dsLogs, "needs one examples/docker-compose log stack after training");
      await docsTheme(page, theme);
      expect(await openList(page, "/agent/logs")).toBe("rows");
      await expect(page.getByRole("button", { name: /^View / }).nth(2)).toBeVisible();
      await docsShot(page, dsLogs!, theme, "logs");
    });

    // Enterprise getting-started and SSO pages (src/enterprise/). Needs a
    // licensed stack with AI on in config (the harness mock model) so detect
    // can be armed; the key typed below is a placeholder and is never saved.
    test(`enterprise add an AI key ${theme}`, async ({ page }) => {
      test.skip(!dsModes.has("ent-admin"), "needs a licensed Enterprise stack");
      await docsTheme(page, theme);
      await openApp(page, "/admin?section=ai");
      await expect(sectionHeading(page)).toHaveText("AI provider");
      const enable = page.getByLabel("Enable AI for this org");
      if (!(await enable.isChecked())) await enable.check();
      await page.locator("#ai-provider").selectOption("openai");
      await expect(page.locator("#ai-base-url")).toHaveCount(0);
      await page.locator("#ai-api-key").fill("placeholder-key-1234");
      await expect(page.locator("#ai-api-key")).toHaveAttribute("type", "password");
      await expect(page.getByRole("button", { name: "Save", exact: true })).toBeEnabled();
      await docsShot(page, "enterprise-activate-ai-key", theme, "form");
    });

    test(`enterprise arm detect confirm ${theme}`, async ({ page }) => {
      test.skip(!dsModes.has("ent-admin"), "needs a licensed Enterprise stack with AI on");
      await docsTheme(page, theme);
      await openApp(page, "/admin?section=mode");
      const modes = page.getByRole("group", { name: "Runtime mode" });
      const detect = modes.getByRole("button", { name: "Detect" });
      if ((await detect.getAttribute("aria-pressed")) === "true") {
        await modes.getByRole("button", { name: "Shadow" }).click();
        await expect(modes.getByRole("button", { name: "Shadow" })).toHaveAttribute("aria-pressed", "true");
      }
      await detect.click();
      const dialog = page.getByRole("dialog");
      await expect(dialog.getByText("Arm detect mode?")).toBeVisible();
      await expect(dialog.getByRole("button", { name: "Arm detect" })).toBeEnabled();
      await dismissToasts(page);
      await docsShot(page, "enterprise-arm-detect", theme, "confirm");
      await dialog.getByRole("button", { name: "Arm detect" }).click();
      await expect(detect).toHaveAttribute("aria-pressed", "true");
    });

    test(`intro incidents hero ${theme}`, async ({ page }) => {
      test.skip(!dsModes.has("intro-incidents"), "needs the sre-agent quickstart in detect after the checkout burst");
      await docsTheme(page, theme);
      await page.setViewportSize({ width: 1440, height: 900 });
      await openApp(page, "/incidents");
      const row = page.locator("table tbody tr").filter({ hasText: "checkout-api" }).first();
      await expect(row).toBeVisible({ timeout: 30_000 });
      const incident = await findIncident(page, (i) => i.origin === "ai_detect" && /checkout-api/.test(i.title ?? ""));
      await page.goto(`/incidents/${incident.id}`);
      await expect(page.getByText(/acme-pay/).first()).toBeVisible({ timeout: 30_000 });
      await page.waitForLoadState("networkidle");
      await expect.poll(() => page.locator("html").getAttribute("data-theme")).toBe(theme === "light" ? "light" : null);
      await page.mouse.move(0, 0);
      await page.waitForTimeout(400);
      await page.screenshot({ path: capturePath("intro-incidents", 1440, theme, "detail"), animations: "disabled" });
    });

    test(`sre top bar detect chip 2x ${theme}`, async ({ browser }) => {
      test.skip(!dsModes.has("sre-mode-detect-2x"), "needs the sre-agent quickstart in detect mode");
      const context = await browser.newContext({ deviceScaleFactor: 2, viewport: { width: 1440, height: 900 }, colorScheme: theme });
      const page = await context.newPage();
      await page.addInitScript((t) => localStorage.setItem("versus.theme", t), theme);
      await openApp(page, "/now");
      await expect(page.locator("header").getByText("detect", { exact: true }).first()).toBeVisible({ timeout: 30_000 });
      await expect.poll(() => page.locator("html").getAttribute("data-theme")).toBe(theme === "light" ? "light" : null);
      await page.mouse.move(0, 0);
      await page.waitForTimeout(400);
      await page.screenshot({ path: capturePath("quickstart-sre-mode-detect-2x", 1440, theme, "chip"), clip: { x: 840, y: 0, width: 600, height: 56 }, animations: "disabled" });
      await context.close();
    });

    test(`agent tool catalog ${theme}`, async ({ page }) => {
      test.skip(!dsModes.has("tool-catalog"), "needs a running OSS stack");
      await docsTheme(page, theme);
      await page.setViewportSize({ width: 1280, height: 720 });
      await openApp(page, "/agent/tools");
      await expect(page.getByRole("heading", { name: "Connectors & Tools" }).last()).toBeVisible({ timeout: 30_000 });
      await expect(page.getByText("Kubernetes").first()).toBeVisible();
      await page.waitForLoadState("networkidle");
      await expect.poll(() => page.locator("html").getAttribute("data-theme")).toBe(theme === "light" ? "light" : null);
      await page.mouse.move(0, 0);
      await page.waitForTimeout(400);
      await page.screenshot({ path: capturePath("agent-tool-catalog", 1280, theme, "catalog"), animations: "disabled" });
    });

    test(`ha docker sign-in page ${theme}`, async ({ page }) => {
      test.skip(!dsModes.has("ha-signin"), "needs examples/high-availability/docker behind its load balancer");
      await docsTheme(page, theme);
      await page.goto("/");
      await expect(page.getByTestId("local-login-form")).toBeVisible({ timeout: 30_000 });
      await expect(page.getByText(/Sign in with/)).toHaveCount(0);
      await docsShot(page, "ha-docker-sign-in", theme, "login");
    });

    test(`sre top bar detect chip ${theme}`, async ({ page }) => {
      test.skip(!dsModes.has("sre-mode-detect"), "needs the sre-agent quickstart in detect mode");
      await docsTheme(page, theme);
      await openApp(page, "/now");
      const chip = page.locator("header").getByText("detect", { exact: true }).first();
      await expect(chip).toBeVisible({ timeout: 30_000 });
      await expect.poll(() => page.locator("html").getAttribute("data-theme")).toBe(theme === "light" ? "light" : null);
      await page.mouse.move(0, 0);
      await page.waitForTimeout(400);
      await page.screenshot({
        path: capturePath("quickstart-sre-mode-detect", DOCS_DESKTOP.width, theme, "topbar"),
        clip: { x: 0, y: 0, width: DOCS_DESKTOP.width, height: 120 },
        animations: "disabled",
      });
    });

    // ent-slo turns auto-define on; ent-slo-shot captures the first proposals
    // once the advisor's first run (about 15 minutes after start) has landed.
    test(`enterprise slo proposals ${theme}`, async ({ page }) => {
      test.skip(!dsModes.has("ent-slo") && !dsModes.has("ent-slo-shot"), "needs a licensed stack with AI on");
      await docsTheme(page, theme);
      await openApp(page, "/agent/slo");
      const toggle = page.getByTestId("slo-enable-toggle");
      await expect(toggle).toBeEnabled({ timeout: 30_000 });
      if ((await toggle.getAttribute("aria-checked")) !== "true") await toggle.click();
      await expect(toggle).toHaveAttribute("aria-checked", "true");
      if (!dsModes.has("ent-slo-shot")) return;
      await expect(page.getByTestId("slo-adopt-btn").first()).toBeVisible({ timeout: 30_000 });
      await expect(page.getByText("Adopt first", { exact: true }).first()).toBeVisible();
      await dismissToasts(page);
      await docsShot(page, "slo", theme, "proposals");
    });

    test(`enterprise google identity provider ${theme}`, async ({ page }) => {
      test.skip(!dsModes.has("ent-admin"), "needs a licensed Enterprise stack");
      await docsTheme(page, theme);
      await openApp(page, "/admin?section=sso");
      await expect(sectionHeading(page)).toHaveText("Identity providers");
      const addButton = page.getByRole("button", { name: "Add identity provider" });
      await expect(addButton).toBeVisible();
      await page.waitForLoadState("networkidle");
      if (!(await page.getByRole("button", { name: "Edit Google" }).count())) {
        await page.getByRole("button", { name: "Add identity provider" }).click();
        await page.locator("#conn-type").selectOption("google");
        await page.locator("#conn-id").fill("google");
        await page.locator("#conn-name").fill("Google");
        await page.locator("#conn-client-id").fill("000000000000-docs-example.apps.googleusercontent.com");
        await page.locator("#conn-secret").fill("docs-example-secret");
        await page.locator("#conn-domains").fill("acme.com");
        await page.getByRole("button", { name: "Add provider" }).click();
        await expect(page.getByText("Identity provider saved")).toBeVisible();
      }
      await expect(page.getByRole("button", { name: "Edit Google" })).toBeVisible();
      await dismissToasts(page);
      await docsShot(page, "admin-identity-providers", theme, "google");
      await page.context().clearCookies();
      await page.goto("/");
      const google = page.getByRole("button", { name: "Sign in with Google" }).or(page.getByRole("link", { name: "Sign in with Google" }));
      await expect(google.first()).toBeVisible({ timeout: 30_000 });
      await docsShot(page, "sso-login", theme, "google");
    });

    for (const [mode, listPath] of [["ds-prometheus", "/agent/metrics"], ["ds-traces", "/agent/traces"], ["ent-metrics", "/agent/metrics"], ["ent-traces", "/agent/traces"]] as const) {
      test(`data source ${mode} signals ${theme}`, async ({ page }) => {
        test.skip(!dsModes.has(mode), "needs the licensed metrics-source example after training");
        await docsTheme(page, theme);
        expect(await openList(page, listPath)).toBe("rows");
        await docsShot(page, mode, theme, "signals");
      });
    }

    test(`data source settings cards ${theme}`, async ({ page }) => {
      test.skip(!dsModes.has("ds-settings"), "needs a stack with two or more data sources");
      await docsTheme(page, theme);
      await openApp(page, "/settings?section=agent-sources");
      await expect(sectionHeading(page)).toHaveText("Data sources");
      await page.waitForLoadState("networkidle");
      await docsShot(page, "ds-settings", theme, "cards");
    });

    test(`sre run analysis on the detected incident ${theme}`, async ({ page }) => {
      test.skip(DOCS_QUICKSTART !== "sre-ai", "needs the sre-agent stack with an AI model and an AI-detect incident");
      test.setTimeout(120_000);
      await docsTheme(page, theme);
      await openApp(page, "/incidents");
      const incident = await findIncident(page, (i) => i.origin === "ai_detect" && /acme-pay/.test(i.title ?? ""));
      await page.goto(`/incidents/${incident.id}`);
      await page.getByRole("button", { name: "Run AI analysis" }).first().click();
      const dialog = page.getByRole("dialog");
      await expect(dialog.getByText("Analysis complete")).toBeVisible({ timeout: 90_000 });
      await expect(dialog.getByText("Root cause hypotheses")).toBeVisible({ timeout: 30_000 });
      await expect(dialog.getByText("Next steps")).toBeVisible();
      await expect(dialog.getByText(/^\d+% conf$/)).toBeVisible();
      const toolCalls = dialog.locator("summary").filter({ hasText: /^Tool calls \(\d+\)$/ });
      await toolCalls.click();
      await expect(toolCalls.locator("xpath=..")).toHaveAttribute("open", "");
      // The "Analysis complete" toast covers the modal footer; dismiss it.
      const dismiss = page.getByRole("button", { name: "Dismiss notification" });
      while (await dismiss.count()) await dismiss.first().click();
      await expect(dismiss).toHaveCount(0);
      await docsShot(page, "sre-analyze", theme, "analysis");
      // Second frame: the tool-call audit trail, every call with args and output.
      const calls = toolCalls.locator("xpath=..").locator("li");
      await expect(calls).toHaveCount(2);
      await toolCalls.evaluate((el) => el.scrollIntoView({ block: "start" }));
      for (const call of await calls.all()) {
        await expect(call.getByText(/^args:/)).toBeInViewport();
        await expect(call.getByText(/^output:/)).toBeInViewport();
      }
      await docsShot(page, "sre-analyze-tools", theme, "analysis");
    });

    test(`sre chat summary with evidence ${theme}`, async ({ page }) => {
      test.skip(DOCS_QUICKSTART !== "sre-ai", "needs the sre-agent stack with an AI model");
      test.setTimeout(120_000);
      await docsTheme(page, theme);
      await openApp(page, "/agent/chat");
      await page.getByRole("button", { name: "Summarize unresolved incidents from the last 24 hours." }).click();
      await page.getByRole("button", { name: "Send" }).click();
      const log = page.getByRole("log", { name: "Conversation" });
      const sources = log.getByLabel("Sources").last();
      await expect(sources).toBeVisible({ timeout: 90_000 });
      await expect(sources.getByRole("button").first()).toBeVisible();
      const tool = log.locator('button[aria-expanded="false"]').filter({ has: page.getByLabel("Success") }).first();
      await tool.click();
      await expect(log.getByText("Arguments", { exact: true }).first()).toBeVisible();
      await docsShot(page, "sre-chat", theme, "answer");
    });
  }
});
