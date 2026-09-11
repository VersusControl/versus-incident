import { expect, test, type Page, type Route } from "@playwright/test";
import * as path from "node:path";
import { fileURLToPath } from "node:url";

const screenshotDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "screenshots", "service-health");
const fixedTime = "2026-09-10T12:00:00Z";

type Snapshot = Record<string, unknown>;

function emptySnapshot(): Snapshot {
  return {
    snapshot_id: "",
    generated_at: fixedTime,
    latest_attempt: "0001-01-01T00:00:00Z",
    next_collection_at: "0001-01-01T00:00:00Z",
    settings_revision: 0,
    window_seconds: 300,
    services: [],
    domains: [],
    facets: {},
    coverage: { total_services: 0, observed_services: 0, partial: false },
    capabilities: [
      { family: "logs", measures: { activity: { state: "not_configured", reason_code: "source_not_configured", action_id: "connect_log_source" } } },
      { family: "internal", measures: { incidents: { state: "ready" }, patterns: { state: "ready" } } },
      { family: "metrics", measures: { latency: { state: "restricted", reason_code: "enterprise_required" } } },
      { family: "traces", measures: { request_context: { state: "restricted", reason_code: "enterprise_required" } } },
    ],
  };
}

function liveSnapshot(): Snapshot {
  return {
    ...emptySnapshot(),
    snapshot_id: "snap-live",
    latest_attempt: fixedTime,
    latest_success: fixedTime,
    next_collection_at: "2026-09-10T12:01:00Z",
    services: [
      {
        org_id: "default",
        service: "checkout-service-with-a-long-production-name",
        domain: "Customer checkout and payment processing",
        kind: "unknown",
        severity: "pressure",
        assessment_basis: "Logs + patterns + incidents",
        logs: { service: "checkout-service-with-a-long-production-name", source_id: "", matched_logs: 1284, unique_patterns: 12, new_patterns: 2, unknown_patterns: 1, spiking_patterns: 2, severity_counts: { warning: 4 }, latest_observation: fixedTime, estimated: true },
        availability: { logs: { state: "ready" }, internal: { state: "ready" } },
        active_incidents: 1,
      },
      {
        org_id: "default",
        service: "catalog",
        domain: "Ungrouped",
        kind: "unknown",
        severity: "nominal",
        assessment_basis: "Patterns + incidents",
        logs: { service: "catalog", source_id: "", matched_logs: 0, unique_patterns: 0, new_patterns: 0, unknown_patterns: 0, spiking_patterns: 0, severity_counts: {}, estimated: false },
        availability: { logs: { state: "no_data" }, internal: { state: "ready" } },
        active_incidents: 0,
      },
    ],
    domains: [
      { name: "Customer checkout and payment processing", severity: "pressure", service_count: 1, observed_count: 1, affected_count: 1 },
      { name: "Ungrouped", severity: "nominal", service_count: 1, observed_count: 1, affected_count: 0 },
    ],
    facets: { pressure: 1, nominal: 1 },
    coverage: { total_services: 2, observed_services: 2, partial: false },
    capabilities: [
      { family: "logs", measures: { activity: { state: "ready" }, patterns: { state: "ready" }, anomalies: { state: "ready" } } },
      { family: "internal", measures: { incidents: { state: "ready" }, patterns: { state: "ready" } } },
      { family: "metrics", measures: { latency: { state: "restricted", reason_code: "enterprise_required" } } },
      { family: "traces", measures: { request_context: { state: "restricted", reason_code: "enterprise_required" } } },
    ],
  };
}

async function json(route: Route, body: unknown, status = 200) {
  await route.fulfill({ status, contentType: "application/json", body: JSON.stringify(body) });
}

async function installApi(page: Page, health: Snapshot) {
  const state = { settings: { interval_seconds: 60, window_seconds: 300, revision: 0 }, patchBodies: [] as unknown[] };
  await page.route("**/api/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const requestPath = url.pathname;

    if (requestPath === "/api/auth/gateway-session" && request.method() === "POST") {
      return route.fulfill({ status: 204, headers: { "Set-Cookie": "versus_gateway_session=service-health-test; Path=/; HttpOnly; SameSite=Strict" }, body: "" });
    }
    if (requestPath.endsWith("/deployment") || requestPath.includes("/sso/")) return json(route, { error: "community" }, 403);
    if (requestPath === "/api/admin/config/agent") return json(route, { enable: true, mode: "detect", ai: { enable: false }, sources: [] });
    if (requestPath === "/api/agent/status") return json(route, { patterns: 124, dirty: false, shadow_events: 28, detect_events: 42 });
    if (requestPath === "/api/agent/shadow/stats") return json(route, { events: 28, total_signals: 540, verdicts: { spike: 8 }, occurrences: 28 });
    if (requestPath === "/api/agent/detect/stats") return json(route, { outcome_emitted: 12, outcome_cached: 29, outcome_ai_error: 1, verdict_spike: 18, verdict_unknown: 7, verdict_normal: 17, severity_high: 9, severity_medium: 14, severity_low: 19 });
    if (requestPath === "/api/agent/patterns") return json(route, { patterns: [
      { id: "checkout-timeout", template: "checkout request timed out after <*> ms", count: 1240, baseline_frequency: 2.4, verdict: "known" },
      { id: "payment-retry", template: "payment provider retry attempt <*>", count: 386, baseline_frequency: 0.8, verdict: "" },
      { id: "catalog-response", template: "catalog request completed status <*>", count: 2432, baseline_frequency: 8.2, verdict: "known" },
    ] });
    if (requestPath === "/api/agent/shadow") return json(route, { events: [
      { pattern_id: "checkout-timeout", first_seen: fixedTime, last_seen: fixedTime, verdict: "spike", sample_message: "Checkout requests are timing out" },
      { pattern_id: "payment-retry", first_seen: fixedTime, last_seen: fixedTime, verdict: "unknown", sample_message: "Payment provider retry detected" },
    ] });
    if (requestPath === "/api/agent/baselines") return json(route, { error: "community" }, 403);
    if (requestPath === "/api/agent/services" && request.method() === "GET") {
      return json(route, { services: { checkout: { first_seen: fixedTime, manual: false, in_grace: false, grace_seconds_remaining: 0 } }, total: 1, next_offset: null });
    }
    if (requestPath === "/api/agent/service-health") return json(route, health);
    if (requestPath === "/api/agent/service-health/settings" && request.method() === "GET") return json(route, state.settings);
    if (requestPath === "/api/agent/service-health/settings" && request.method() === "PATCH") {
      const body = request.postDataJSON();
      state.patchBodies.push(body);
      state.settings = { interval_seconds: body.interval_seconds, window_seconds: body.window_seconds, revision: state.settings.revision + 1 };
      return json(route, state.settings);
    }
    return json(route, {});
  });
  return state;
}

async function openAuthenticated(page: Page, route: string) {
  await page.goto(route);
  const secret = page.getByLabel("Gateway secret");
  if (await secret.isVisible().catch(() => false)) {
    await secret.fill("service-health-test");
    await page.getByRole("button", { name: "Sign in", exact: true }).click();
  }
  await expect(page.getByTestId("app-authenticated")).toBeVisible();
}

async function expectNoHorizontalOverflow(page: Page) {
  const widths = await page.evaluate(() => ({ viewport: window.innerWidth, body: document.body.scrollWidth, root: document.documentElement.scrollWidth }));
  expect(widths.body).toBeLessThanOrEqual(widths.viewport);
  expect(widths.root).toBeLessThanOrEqual(widths.viewport);
}

test("no-source preview is labeled, isolated, and responsive", async ({ page }) => {
  await installApi(page, emptySnapshot());
  await page.setViewportSize({ width: 1440, height: 1000 });
  await openAuthenticated(page, "/agent");
  await expect(page.getByText("Example preview - sample data")).toBeVisible();
  await expect(page.getByTestId("service-health-coverage-summary")).toContainText("0 with data");
  await expect(page.getByTestId("service-health-preview").getByRole("link")).toHaveCount(0);
  await expectNoHorizontalOverflow(page);
  await page.screenshot({ path: path.join(screenshotDir, "no-source-desktop.png"), fullPage: true });

  await page.setViewportSize({ width: 390, height: 844 });
  await expect(page.getByText("Example preview - sample data")).toBeVisible();
  await expectNoHorizontalOverflow(page);
  await expect(page.getByRole("heading", { name: "Agent Overview", exact: true })).toBeVisible();
  await page.screenshot({ path: path.join(screenshotDir, "no-source-mobile.png"), fullPage: true });
});

test("live OSS evidence supports mode, facet, domain drill-down, and service navigation", async ({ page }) => {
  await installApi(page, liveSnapshot());
  await openAuthenticated(page, "/agent");
  await expect(page.getByRole("button", { name: "Inspect catalog" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Service Heatmap", exact: true })).toBeVisible();
  await expect(page.getByText("Impact based on combined evidence")).toHaveCount(0);
  await expect(page.getByText("Patterns + incidents")).toHaveCount(0);
  const firstTile = await page.getByRole("button", { name: "Inspect catalog" }).boundingBox();
  expect(firstTile?.y).toBeLessThan(620);
  await expect(page.getByRole("heading", { name: "Lifetime totals" })).not.toBeVisible();
  await page.screenshot({ path: path.join(screenshotDir, "live-grid-desktop.png"), fullPage: true });
  await page.setViewportSize({ width: 390, height: 844 });
  await expectNoHorizontalOverflow(page);
  await page.screenshot({ path: path.join(screenshotDir, "live-grid-mobile.png"), fullPage: true });
  await page.setViewportSize({ width: 1280, height: 900 });
  const lightSwitch = page.getByRole("button", { name: "Switch to light theme" });
  if (await lightSwitch.isVisible()) await lightSwitch.click();
  await expect(page.getByTestId("service-health-domains")).toHaveCSS("background-color", "rgb(255, 255, 255)");
  await expect.poll(async () => page.getByRole("button", { name: "Inspect catalog" }).evaluate((element) => {
    const canvas = document.createElement("canvas");
    canvas.width = 1; canvas.height = 1;
    const context = canvas.getContext("2d")!;
    context.fillStyle = getComputedStyle(element).backgroundColor;
    context.fillRect(0, 0, 1, 1);
    return Math.min(...Array.from(context.getImageData(0, 0, 1, 1).data).slice(0, 3));
  })).toBeGreaterThan(220);
  await page.screenshot({ path: path.join(screenshotDir, "live-grid-light.png"), fullPage: true });
  await expect(page.getByTestId("service-health-preview")).toHaveCount(0);
  await page.getByLabel("Health state").selectOption("pressure");
  await expect(page.getByRole("button", { name: "Inspect checkout-service-with-a-long-production-name" })).toBeVisible();
  await expect(page.getByText("Log events")).toBeVisible();
  await page.getByLabel("Show data").selectOption("incidents");
  await expect(page.getByText("Active incidents")).toBeVisible();
  await expect(page.getByText("Log events")).toHaveCount(0);
  await page.getByRole("button", { name: "List view" }).click();
  await page.getByRole("searchbox", { name: "Find a service" }).fill("checkout");
  await page.getByRole("button", { name: "Inspect checkout-service-with-a-long-production-name" }).click();
  await expect(page.getByRole("dialog")).toBeVisible();
  await expect(page.getByText("Logs and incident records")).toBeVisible();
  await page.screenshot({ path: path.join(screenshotDir, "live-drilldown-desktop.png"), fullPage: true });
  await page.setViewportSize({ width: 390, height: 844 });
  const panel = page.getByRole("dialog").locator("aside");
  await expect.poll(async () => {
    const box = await panel.boundingBox();
    return box ? Math.abs(box.x) : Infinity;
  }).toBeLessThan(1);
  await expect.poll(async () => {
    const box = await panel.boundingBox();
    return box ? box.x + box.width : Infinity;
  }).toBeLessThanOrEqual(391);
  await expect(page.getByRole("button", { name: "Close panel" })).toBeInViewport();
  await expectNoHorizontalOverflow(page);
  await page.screenshot({ path: path.join(screenshotDir, "live-drilldown-mobile.png"), fullPage: true });
  await page.getByRole("link", { name: "Open service" }).click();
  await expect(page).toHaveURL(/\/agent\/services\/checkout-service-with-a-long-production-name$/);
});

test("desktop stacks every section and mobile falls back to tabs", async ({ page }) => {
  await installApi(page, liveSnapshot());
  await openAuthenticated(page, "/agent?view=activity");
  for (const name of ["Service Heatmap", "Agent Activity", "Learning"]) {
    await expect(page.getByRole("heading", { name, exact: true })).toBeVisible();
    await expect(page.getByRole("heading", { name, exact: true }).locator("svg")).toBeVisible();
  }
  await expect(page.getByRole("navigation", { name: "Overview views" })).toHaveCount(0);
  await expect(page.getByText("Checkout requests are timing out")).toBeVisible();
  await expect(page.getByText("Learning and activity history")).toHaveCount(0);
  await expect(page.locator("main .card")).toHaveCount(0);
  await page.screenshot({ path: path.join(screenshotDir, "activity-desktop.png"), fullPage: true });
  await page.screenshot({ path: path.join(screenshotDir, "learning-desktop.png"), fullPage: true });

  await page.setViewportSize({ width: 390, height: 844 });
  const tabs = page.getByRole("navigation", { name: "Overview views" });
  await expect(tabs).toBeVisible();
  await expect(page.getByRole("heading", { name: "Agent Activity", exact: true })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Service Heatmap", exact: true })).toHaveCount(0);
  await expectNoHorizontalOverflow(page);
  await page.screenshot({ path: path.join(screenshotDir, "activity-mobile.png"), fullPage: true });

  await tabs.getByRole("button", { name: "Learning" }).click();
  await expect(page).toHaveURL(/view=learning/);
  await expect(page.getByRole("heading", { name: "Frequent log patterns" })).toBeVisible();
  await expect(page.getByRole("link", { name: "checkout request timed out after <*> ms", exact: true })).toBeVisible();
  await expectNoHorizontalOverflow(page);
  await expect(page.locator("td[data-label='Sightings']").first()).toBeInViewport();
  await page.screenshot({ path: path.join(screenshotDir, "learning-mobile.png"), fullPage: true });

  await tabs.getByRole("button", { name: "Services", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Service Heatmap", exact: true })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Agent Activity", exact: true })).toHaveCount(0);
});

test("Service Health timing saves through the settings API", async ({ page }) => {
  const state = await installApi(page, liveSnapshot());
  await openAuthenticated(page, "/settings?tab=tuning");
  const panel = page.getByRole("region", { name: "Service Health timing" });
  const inputs = panel.getByRole("spinbutton");
  await expect(inputs).toHaveCount(2);
  await inputs.nth(0).fill("90");
  await inputs.nth(1).fill("600");
  await panel.getByRole("button", { name: "Save", exact: true }).click();
  await expect(page.getByText("Service Health timing saved")).toBeVisible();
  expect(state.patchBodies).toEqual([{ interval_seconds: 90, window_seconds: 600, revision: 0 }]);
});