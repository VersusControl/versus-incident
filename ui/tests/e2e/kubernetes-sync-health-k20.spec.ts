// K20 honest-sync-failures browser acceptance.
//
// Proves D1 = A (no cluster data while the sync is not complete and healthy),
// the four sync-health states and their copy, honest recovery without a page
// reload, the absence of a misleading "Loading N more resource types" note
// while a kind is failing, and the P8 filter alignment (Timeline namespace vs
// kind, Issues severity vs namespace) at 1440 and 390 in both themes.
//
// Two fixture modes, selected by E2E_K20_MODE:
//   faults — gate/degraded/failed/secret-probe clusters on one versus instance.
//   sync   — one healthy cluster and one held first sync that fails, is
//            released and recovers.
//
// Captures use the shared capturePath helper (HARNESS_IMAGES_DIR).
import { expect, test, type Browser, type BrowserContext, type Page } from "@playwright/test";
import * as fs from "node:fs";
import * as https from "node:https";
import * as path from "node:path";
import { openApp } from "./helpers";
import { capturePath, harnessResultsDir } from "./support/capture";

const mode = process.env.E2E_K20_MODE || "faults";
const apiBaseURL = process.env.E2E_BASE_URL || "http://127.0.0.1:3000";
const gatewaySecret = process.env.E2E_GATEWAY_SECRET || "";
const controlURL = process.env.E2E_K20_CONTROL_URL || "";
const controlCA = process.env.E2E_K20_CONTROL_CA || "";
const preparingControlURL = process.env.E2E_K20_PREPARING_CONTROL_URL || "";
const preparingControlCA = process.env.E2E_K20_PREPARING_CONTROL_CA || "";

const clusters = {
  gate: process.env.E2E_K20_GATE_CLUSTER || "gate",
  degraded: process.env.E2E_K20_DEGRADED_CLUSTER || "degraded",
  failed: process.env.E2E_K20_FAILED_CLUSTER || "noncore-fault",
  secret: process.env.E2E_K20_SECRET_CLUSTER || "secret-probe",
  forbidAll: process.env.E2E_K20_FORBID_ALL_CLUSTER || "forbid-all",
  limitedRbac: process.env.E2E_K20_LIMITED_RBAC_CLUSTER || "limited-rbac",
  healthy: process.env.E2E_K20_HEALTHY_CLUSTER || "healthy",
  preparing: process.env.E2E_K20_PREPARING_CLUSTER || "preparing",
  recovery: process.env.E2E_K20_RECOVERY_CLUSTER || "recovery",
};

const viewports = [
  { name: "desktop", width: 1440, height: 900 },
  { name: "mobile", width: 390, height: 844 },
] as const;
const themes = ["dark", "light"] as const;
type Theme = (typeof themes)[number];
type Viewport = (typeof viewports)[number];

const views = [
  { id: "overview", label: "Overview" },
  { id: "resources", label: "Workloads" },
  { id: "issues", label: "Issues" },
  { id: "timeline", label: "Timeline" },
  { id: "topology", label: "Topology" },
  { id: "nodes", label: "Nodes" },
  { id: "releases", label: "Helm" },
  { id: "gitops", label: "GitOps" },
  { id: "traffic", label: "Traffic" },
  { id: "agent", label: "Agent" },
] as const;

const openContexts: BrowserContext[] = [];
test.afterEach(async () => {
  for (const context of openContexts.splice(0)) await context.close().catch(() => {});
});

async function newPage(browser: Browser, viewport: Viewport, theme: Theme): Promise<Page> {
  const context = await browser.newContext({
    viewport: { width: viewport.width, height: viewport.height },
    colorScheme: theme,
  });
  openContexts.push(context);
  const page = await context.newPage();
  await page.emulateMedia({ colorScheme: theme });
  await page.addInitScript((value) => localStorage.setItem("versus.theme", value), theme);
  return page;
}

async function openCluster(page: Page, cluster: string, extra = "") {
  await openApp(page, `/agent/kubernetes?cluster=${encodeURIComponent(cluster)}${extra}`);
}

async function verifyThemeAndQuietPage(page: Page, theme: Theme) {
  await expect.poll(() => page.locator("html").getAttribute("data-theme")).toBe(theme === "light" ? "light" : null);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  const bodyText = await page.locator("body").innerText();
  expect(bodyText).not.toMatch(/Session expired|Sign in to continue|Unhandled Runtime Error|Couldn't load Kubernetes overview/i);
  expect(bodyText).not.toMatch(/\bundefined\b/);
}

async function saveCapture(page: Page, surface: string, viewport: Viewport, theme: Theme, state: string) {
  await verifyThemeAndQuietPage(page, theme);
  const out = capturePath(surface, viewport.width, theme, state);
  await page.screenshot({ path: out, animations: "disabled" });
  return out;
}

function viewTabs(page: Page) {
  return page.getByRole("tablist", { name: "Kubernetes views", exact: true });
}

async function selectView(page: Page, id: (typeof views)[number]["id"], label: string) {
  const tab = viewTabs(page).getByRole("tab", { name: label, exact: true });
  await tab.click();
  await expect(tab).toHaveAttribute("aria-selected", "true");
  await expect(page.locator("#kubernetes-view-panel")).toBeVisible();
}

function surface(page: Page) {
  return page.locator("[data-sync-health]").first();
}

async function waitSurface(page: Page, state: string, timeout = 90_000) {
  await expect(surface(page)).toHaveAttribute("data-sync-health", state, { timeout });
}

function marker(page: Page, id: (typeof views)[number]["id"]) {
  switch (id) {
    case "overview": return page.locator('[aria-label="Cluster health"]');
    case "resources": return page.getByRole("heading", { name: "Workloads", exact: true });
    case "issues": return page.locator('[aria-label="Kubernetes issues"]');
    case "timeline": return page.locator('[aria-label="Kubernetes change timeline"]');
    case "topology": return page.locator('[aria-label="Kubernetes topology"]');
    case "nodes": return page.getByRole("heading", { name: "Nodes", exact: true });
    case "releases": return page.locator('[aria-label="Helm releases"]');
    case "gitops": return page.locator('[aria-label="GitOps applications"]');
    case "traffic": return page.locator('[aria-label="Kubernetes traffic flows"]');
    case "agent": return page.locator('[aria-label="Kubernetes Agent"]');
  }
}

/** No cluster data of any shape: no view card, no rows, no chart, no counts. */
async function expectNoClusterData(page: Page, id: (typeof views)[number]["id"]) {
  const panel = page.locator("#kubernetes-view-panel");
  await expect(panel).toBeVisible();
  await expect(marker(page, id)).toHaveCount(0);
  await expect(panel.locator('table, svg[role="img"], [role="list"], .recharts-wrapper')).toHaveCount(0);
  const text = await panel.innerText();
  expect(text).not.toMatch(/CPU capacity|Memory capacity|checkout|payments|load-0000|node-0\b|\b\d+\s+(pods|nodes|resources|events|issues)\b/i);
  expect(text).not.toMatch(/No issues match these filters|No workloads in this scope|No nodes in this cluster/i);
}

/** A preparing surface: no view card, rows, chart or topology graph. The
 * first-sync panel's listing counts are progress, not cluster data. */
async function expectNoDataViews(page: Page, id: (typeof views)[number]["id"]) {
  const panel = page.locator("#kubernetes-view-panel");
  await expect(panel).toBeVisible();
  await expect(marker(page, id)).toHaveCount(0);
  await expect(panel.locator('table, svg[role="img"], [role="list"], .recharts-wrapper')).toHaveCount(0);
  await expect(panel.locator("[data-sync-health]")).toHaveCount(1);
}

/** A failing kind is never presented as loading. */
async function expectNoLoadingCopy(page: Page) {
  const panel = page.locator("#kubernetes-view-panel");
  const text = await panel.innerText();
  expect(text).not.toMatch(/Loading/i);
  expect(text).not.toMatch(/more resource type/i);
  await expect(page.locator("[data-pending-note]")).toHaveCount(0);
}

async function expectTopState(page: Page, text: RegExp) {
  const state = page.locator("[data-sync-state]");
  await expect(state).toHaveCount(1);
  await expect(state).toHaveText(text);
  const box = await state.boundingBox();
  expect(box, "top-bar sync state word must be visibly rendered at every viewport width").not.toBeNull();
  expect(box!.width).toBeGreaterThanOrEqual(38);
  expect(box!.height).toBeGreaterThanOrEqual(12);
}

async function expectTopReady(page: Page) {
  // QA-195: the state is always written out at every width, Ready included, so
  // Ready is one [data-sync-state="ready"] reading "Live (Ns)", never absent.
  await expect(page.locator("[data-sync-state]")).toHaveCount(1);
  const state = page.locator('[data-sync-state="ready"]');
  await expect(state).toBeVisible();
  await expect(state).toHaveText(/^Live/);
  const box = await state.boundingBox();
  expect(box, "top-bar Ready indicator must be visibly rendered at every viewport width").not.toBeNull();
  expect(box!.width).toBeGreaterThanOrEqual(38);
  expect(box!.height).toBeGreaterThanOrEqual(12);
}

interface BackendSyncReport {
  state?: string;
  partial?: boolean;
  ready_kinds?: number;
  failing_kinds?: number;
  pending_kinds?: number;
  kinds?: Record<string, { state?: string; error?: string; partial?: boolean }>;
  failing?: Array<{ kind?: string; state?: string; class?: string }>;
}

async function fetchBackendSync(cluster: string): Promise<BackendSyncReport> {
  const headers: Record<string, string> = gatewaySecret ? { "X-Gateway-Secret": gatewaySecret } : {};
  const res = await fetch(`${apiBaseURL}/api/admin/kubernetes/sync?cluster=${encodeURIComponent(cluster)}`, {
    headers,
    signal: AbortSignal.timeout(10_000),
  });
  if (!res.ok) throw new Error(`sync ${cluster} HTTP ${res.status}`);
  return (await res.json()) as BackendSyncReport;
}

async function fetchBackendOverview(cluster: string): Promise<{ sync?: { state?: string; partial?: boolean } }> {
  const headers: Record<string, string> = gatewaySecret ? { "X-Gateway-Secret": gatewaySecret } : {};
  const res = await fetch(`${apiBaseURL}/api/admin/kubernetes/overview?cluster=${encodeURIComponent(cluster)}`, {
    headers,
    signal: AbortSignal.timeout(10_000),
  });
  if (!res.ok) throw new Error(`overview ${cluster} HTTP ${res.status}`);
  return (await res.json()) as { sync?: { state?: string; partial?: boolean } };
}

async function waitBackendSync(cluster: string, predicate: (body: BackendSyncReport) => boolean, timeoutMs = 90_000): Promise<BackendSyncReport> {
  const deadline = Date.now() + timeoutMs;
  let last = "";
  while (Date.now() < deadline) {
    try {
      const body = await fetchBackendSync(cluster);
      last = JSON.stringify({ state: body.state, partial: body.partial, failing: body.failing });
      if (predicate(body)) return body;
    } catch (err) {
      last = String(err);
    }
    await new Promise((resolve) => setTimeout(resolve, 250));
  }
  throw new Error(`Timeout waiting for backend sync (${cluster}): ${last}`);
}

async function problemRow(page: Page, kind: string) {
  const row = page.locator(`[data-sync-problem="${kind}"]`).first();
  await expect(row).toBeVisible();
  return row;
}

async function expectSince(row: import("@playwright/test").Locator) {
  const time = row.locator("time");
  await expect(time).toHaveCount(1);
  await expect(time).toHaveText(/(since|last tried) \d+ (second|seconds|minute|minutes|hour|hours) ago/);
  const iso = await time.getAttribute("datetime");
  expect(iso).toBeTruthy();
  expect(iso).not.toContain("0001");
}

function controlClient(url = controlURL, caFile = controlCA) {
  if (!url || !caFile) throw new Error("fakekube control URL/CA required");
  const ca = fs.readFileSync(caFile);
  const agent = new https.Agent({ ca, keepAlive: true });
  return (endpoint: string, body: unknown) => new Promise<unknown>((resolve, reject) => {
    const req = https.request(`${url}${endpoint}`, {
      agent,
      method: "POST",
      headers: { "Content-Type": "application/json" },
      timeout: 10000,
    }, (res) => {
      let data = "";
      res.on("data", (chunk) => { data += chunk; });
      res.on("end", () => {
        if (res.statusCode !== 200) return reject(new Error(`fakekube ${endpoint} HTTP ${res.statusCode}: ${data}`));
        try { resolve(JSON.parse(data)); } catch (err) { reject(err); }
      });
    });
    req.on("error", reject);
    req.end(JSON.stringify(body));
  });
}

// ---------------------------------------------------------------------------
// P8 geometry
// ---------------------------------------------------------------------------

interface FieldBox {
  label: string;
  labelText: { left: number; right: number; top: number; bottom: number; height: number };
  labelBaseline: number;
  control: {
    left: number; right: number; top: number; bottom: number; width: number; height: number;
    borderTop: number; borderLeft: number; borderRight: number; borderBottom: number;
    paddingTop: number; paddingBottom: number; fontSize: number; lineHeight: number;
  };
}

async function measureField(page: Page, scope: string, label: string, role: "combobox"): Promise<FieldBox> {
  const root = page.locator(scope).first();
  const control = root.getByRole(role, { name: label, exact: true }).first();
  await expect(control).toBeVisible();
  return control.evaluate((element) => {
    const labelElement = element.closest("label");
    if (!labelElement) throw new Error(`control ${element.getAttribute("aria-label")} has no label wrapper`);
    const labelText = labelElement.querySelector(":scope > span");
    if (!labelText) throw new Error("filter label text missing");
    const range = document.createRange();
    range.selectNodeContents(labelText);
    const textRect = range.getBoundingClientRect();
    const textStyle = getComputedStyle(labelText);
    const rect = element.getBoundingClientRect();
    const style = getComputedStyle(element);
    const px = (value: string) => Number.parseFloat(value) || 0;
    return {
      label: (labelText.textContent || "").trim(),
      labelText: {
        left: textRect.left, right: textRect.right, top: textRect.top, bottom: textRect.bottom, height: textRect.height,
      },
      // Both fields use one font and line-height, so the text rect bottom is a
      // stable baseline proxy; the measured font metrics are recorded too.
      labelBaseline: textRect.bottom - px(textStyle.fontSize) * 0.2,
      control: {
        left: rect.left, right: rect.right, top: rect.top, bottom: rect.bottom, width: rect.width, height: rect.height,
        borderTop: px(style.borderTopWidth), borderLeft: px(style.borderLeftWidth),
        borderRight: px(style.borderRightWidth), borderBottom: px(style.borderBottomWidth),
        paddingTop: px(style.paddingTop), paddingBottom: px(style.paddingBottom),
        fontSize: px(style.fontSize), lineHeight: px(style.lineHeight),
      },
    };
  });
}

async function measureControl(page: Page, scope: string, label: string): Promise<FieldBox["control"]> {
  const control = page.locator(scope).first().getByRole("combobox", { name: label, exact: true }).first();
  await expect(control).toBeVisible();
  return control.evaluate((element) => {
    const rect = element.getBoundingClientRect();
    const style = getComputedStyle(element);
    const px = (value: string) => Number.parseFloat(value) || 0;
    return {
      left: rect.left, right: rect.right, top: rect.top, bottom: rect.bottom, width: rect.width, height: rect.height,
      borderTop: px(style.borderTopWidth), borderLeft: px(style.borderLeftWidth),
      borderRight: px(style.borderRightWidth), borderBottom: px(style.borderBottomWidth),
      paddingTop: px(style.paddingTop), paddingBottom: px(style.paddingBottom),
      fontSize: px(style.fontSize), lineHeight: px(style.lineHeight),
    };
  });
}

function compareFields(leftName: string, left: FieldBox, rightName: string, right: FieldBox) {
  // The two fields sit in different columns, so each column's left is
  // expected to differ. What must be identical is the label baseline/text box
  // and the control's top, height and border box. Both rows share one
  // two-column grid at every width (QA-193), so every delta must be 0.00px —
  // a 1px tolerance would hide the stacking this test exists to catch.
  const pairs: Array<[string, number, number]> = [
    [`${leftName} vs ${rightName} label baseline`, left.labelBaseline, right.labelBaseline],
    [`${leftName} vs ${rightName} label text top`, left.labelText.top, right.labelText.top],
    [`${leftName} vs ${rightName} label text bottom`, left.labelText.bottom, right.labelText.bottom],
    [`${leftName} vs ${rightName} label text height`, left.labelText.height, right.labelText.height],
    [`${leftName} vs ${rightName} control top`, left.control.top, right.control.top],
    [`${leftName} vs ${rightName} control bottom`, left.control.bottom, right.control.bottom],
    [`${leftName} vs ${rightName} control width`, left.control.width, right.control.width],
    [`${leftName} vs ${rightName} control height`, left.control.height, right.control.height],
    [`${leftName} vs ${rightName} border top`, left.control.borderTop, right.control.borderTop],
    [`${leftName} vs ${rightName} border bottom`, left.control.borderBottom, right.control.borderBottom],
    [`${leftName} vs ${rightName} border left`, left.control.borderLeft, right.control.borderLeft],
    [`${leftName} vs ${rightName} border right`, left.control.borderRight, right.control.borderRight],
    [`${leftName} vs ${rightName} padding top`, left.control.paddingTop, right.control.paddingTop],
    [`${leftName} vs ${rightName} font size`, left.control.fontSize, right.control.fontSize],
  ];
  const deltas = Object.fromEntries(pairs.map(([label, a, b]) => [label, Number(Math.abs(a - b).toFixed(2))]));
  for (const [label, delta] of Object.entries(deltas)) expect(delta, `${label} must be 0.00px`).toBe(0);
  return deltas;
}

// ---------------------------------------------------------------------------
// faults mode
// ---------------------------------------------------------------------------

test.describe("faults", () => {
  test.skip(mode !== "faults", "faults fixture only");

  test("D1=A: one failing kind blocks all ten views (Overview, Workloads, Issues, Timeline, Topology, Nodes, Helm, GitOps, Traffic and Agent)", async ({ browser }) => {
    test.setTimeout(240_000);
    for (const viewport of viewports) {
      for (const theme of themes) {
        const page = await newPage(browser, viewport, theme);
        await openCluster(page, clusters.gate);
        await waitSurface(page, "failed", 120_000);
        await expectTopState(page, /Failed/);
        const pod = await problemRow(page, "Pod");
        await expect(pod).toContainText("the cluster read failed");
        await expectSince(pod);

        for (const view of views) {
          await selectView(page, view.id, view.label);
          await expect(surface(page)).toBeVisible();
          await expectNoClusterData(page, view.id);
          await expectNoLoadingCopy(page);
          const capture = viewport.name === "desktop" && theme === "dark" || viewport.name === "mobile" && theme === "light" || view.id === "overview";
          if (capture) await saveCapture(page, `k20-gate-${view.id}`, viewport, theme, "blocked");
        }
        await expect(page.locator("[data-sync-health]")).toHaveCount(1);
      }
    }
  });

  test("D1=A: the gated resource drawer shows the sync surface instead of resource data", async ({ browser }) => {
    test.setTimeout(120_000);
    for (const viewport of viewports) {
      const theme: Theme = viewport.name === "desktop" ? "dark" : "light";
      const page = await newPage(browser, viewport, theme);
      await openCluster(page, clusters.gate, "&view=resources&r=apps~v1~deployments/load-0000/app-00000&tab=overview");
      await expect(page.getByRole("dialog")).toBeVisible();
      const dialog = page.getByRole("dialog");
      await expect(dialog.locator('[data-sync-health="failed"]')).toBeVisible();
      await expect(dialog.locator(".resource-detail-content")).toHaveCount(0);
      await expect(dialog.locator('[role="tablist"][aria-label="Resource detail views"]')).toHaveCount(0);
      await expect(dialog.locator("table, svg[role=img]")).toHaveCount(0);
      await saveCapture(page, "k20-gate-drawer", viewport, theme, "blocked");
    }
  });

  test("Degraded names the kind that dropped records, its reason and since when, with a working Retry", async ({ browser }) => {
    test.setTimeout(240_000);
    for (const viewport of viewports) {
      for (const theme of themes) {
        const page = await newPage(browser, viewport, theme);
        await openCluster(page, clusters.degraded);
        await waitSurface(page, "degraded", 120_000);
        await expectTopState(page, /Degraded/);
        await expect(page.getByRole("heading", { name: "Cluster sync degraded" })).toBeVisible();
        const pod = await problemRow(page, "Pod");
        await expect(pod).toContainText("some objects could not be indexed");
        await expect(pod).toContainText("1 object skipped");
        await expectSince(pod);
        await expectNoClusterData(page, "overview");
        await expectNoLoadingCopy(page);
        const requestedSync = page.waitForResponse((response) => response.url().includes("/api/admin/kubernetes/sync") && response.request().method() === "GET");
        const requestedOverview = page.waitForResponse((response) => response.url().includes("/api/admin/kubernetes/overview") && response.request().method() === "GET");
        const retry = page.getByRole("button", { name: "Retry sync", exact: true });
        await retry.click();
        await Promise.all([requestedSync, requestedOverview]);
        await expect(retry).toBeEnabled();
        await waitSurface(page, "degraded");
        await saveCapture(page, "k20-degraded", viewport, theme, "surface");
      }
    }
  });

  test("Failed surface and non-core kind failure (ConfigMap 503): blocks all ten views, names ConfigMap, and stays honest while Retry runs", async ({ browser }) => {
    test.setTimeout(240_000);
    for (const viewport of viewports) {
      for (const theme of themes) {
        const page = await newPage(browser, viewport, theme);
        await openCluster(page, clusters.failed);
        await waitSurface(page, "failed", 120_000);
        await expectTopState(page, /Failed/);
        await expect(page.getByRole("heading", { name: "Cluster sync failed" })).toBeVisible();
        const cm = await problemRow(page, "ConfigMap");
        await expect(cm).toContainText("the cluster read failed");
        await expectSince(cm);
        await expectNoClusterData(page, "overview");
        await expectNoLoadingCopy(page);
        const requestedSync = page.waitForResponse((response) => response.url().includes("/api/admin/kubernetes/sync") && response.request().method() === "GET");
        const requestedOverview = page.waitForResponse((response) => response.url().includes("/api/admin/kubernetes/overview") && response.request().method() === "GET");
        const retry = page.getByRole("button", { name: "Retry sync", exact: true });
        await retry.click();
        await Promise.all([requestedSync, requestedOverview]);
        await expect(retry).toBeEnabled();
        await waitSurface(page, "failed");
        await saveCapture(page, "k20-failed", viewport, theme, "surface");

        if (viewport.name === "desktop" && theme === "dark") {
          for (const view of views) {
            await selectView(page, view.id, view.label);
            await expect(surface(page)).toBeVisible();
            await expectNoClusterData(page, view.id);
            await expectNoLoadingCopy(page);
          }
        }
      }
    }
  });

  test("Permission-gap probes: Secret-only 403 unreadable note, forbid-all (100% 403), and limited-rbac (Node 403)", async ({ browser }) => {
    test.setTimeout(180_000);
    // 1. Secret-only 403 (QA-197): explorer opens with [data-unreadable-note] naming Secret.
    const secretPage = await newPage(browser, viewports[0], "dark");
    await openCluster(secretPage, clusters.secret);
    await expectTopReady(secretPage);
    await expect(secretPage.locator("[data-sync-health]")).toHaveCount(0);
    const unreadableNote = secretPage.locator("[data-unreadable-note]");
    await expect(unreadableNote).toBeVisible();
    await expect(unreadableNote).toContainText("Not readable with this connector identity: Secret (permission denied). Everything else is shown.");
    await saveCapture(secretPage, "k20-secret-probe", viewports[0], "dark", "overview");
    await selectView(secretPage, "releases", "Helm");
    await expect(unreadableNote).toBeVisible();
    await saveCapture(secretPage, "k20-secret-probe", viewports[0], "dark", "helm");

    // 2. forbid-all (-forbid-lists: every LIST returns 403 Forbidden, ready_kinds=0, unreadable_kinds=18).
    const forbidAllSync = await fetchBackendSync(clusters.forbidAll);
    const forbidAllOverview = await fetchBackendOverview(clusters.forbidAll);
    const forbidPage = await newPage(browser, viewports[0], "dark");
    await openCluster(forbidPage, clusters.forbidAll);
    await expect(surface(forbidPage)).toBeVisible({ timeout: 30_000 });
    const forbidSurfaceState = await surface(forbidPage).getAttribute("data-sync-health");
    const forbidTopText = await forbidPage.locator("[data-sync-state]").innerText();
    const forbidHeading = await forbidPage.locator("#kubernetes-view-panel h2").first().innerText();
    await saveCapture(forbidPage, "k20-forbid-all", viewports[0], "dark", "surface");

    const forbidMobile = await newPage(browser, viewports[1], "light");
    await openCluster(forbidMobile, clusters.forbidAll);
    await expect(surface(forbidMobile)).toBeVisible({ timeout: 30_000 });
    await saveCapture(forbidMobile, "k20-forbid-all", viewports[1], "light", "surface");

    // 3. limited-rbac (Node and Secret return 403 Forbidden).
    const limitedSync = await fetchBackendSync(clusters.limitedRbac);
    const limitedOverview = await fetchBackendOverview(clusters.limitedRbac);
    const limitedPage = await newPage(browser, viewports[0], "dark");
    await openCluster(limitedPage, clusters.limitedRbac);
    await expect(limitedPage.locator("[data-unreadable-note]")).toBeVisible({ timeout: 30_000 });
    const limitedUnreadableText = await limitedPage.locator("[data-unreadable-note]").innerText();
    const limitedBodyText = await limitedPage.locator("#kubernetes-view-panel").innerText();
    await saveCapture(limitedPage, "k20-limited-rbac", viewports[0], "dark", "overview");
    await selectView(limitedPage, "nodes", "Nodes");
    await limitedPage.waitForTimeout(1500);
    await saveCapture(limitedPage, "k20-limited-rbac", viewports[0], "dark", "nodes");
    await selectView(limitedPage, "topology", "Topology");
    await limitedPage.waitForTimeout(1500);
    await saveCapture(limitedPage, "k20-limited-rbac", viewports[0], "dark", "topology");

    fs.mkdirSync(harnessResultsDir(), { recursive: true });
    fs.writeFileSync(
      path.join(harnessResultsDir(), "k20-permission-probes.json"),
      `${JSON.stringify({
        forbidAll: {
          overviewSync: forbidAllOverview.sync,
          syncReport: {
            state: forbidAllSync.state,
            partial: forbidAllSync.partial,
            ready_kinds: forbidAllSync.ready_kinds,
            failing_kinds: forbidAllSync.failing_kinds,
            pending_kinds: forbidAllSync.pending_kinds,
            unreadable_kinds: (forbidAllSync as Record<string, unknown>).unreadable_kinds,
          },
          renderedSurfaceState: forbidSurfaceState,
          renderedTopBar: forbidTopText,
          renderedHeading: forbidHeading,
        },
        limitedRbac: {
          overviewSync: limitedOverview.sync,
          syncReport: {
            state: limitedSync.state,
            partial: limitedSync.partial,
            ready_kinds: limitedSync.ready_kinds,
            failing_kinds: limitedSync.failing_kinds,
            pending_kinds: limitedSync.pending_kinds,
            unreadable_kinds: (limitedSync as Record<string, unknown>).unreadable_kinds,
          },
          unreadableNote: limitedUnreadableText,
          overviewExcerpt: limitedBodyText.slice(0, 600),
        },
      }, null, 2)}\n`,
    );
  });

  test("Fleet page: overview envelope says ready while sync report says failed -> row shows Failed with no counts", async ({ browser }) => {
    test.setTimeout(120_000);
    for (const viewport of [viewports[0], viewports[1]]) {
      const theme: Theme = viewport.name === "desktop" ? "dark" : "light";
      const page = await newPage(browser, viewport, theme);
      const catalogClusters: Record<string, { sync?: { state?: string; partial?: boolean }; nodes?: number; ready_nodes?: number; pods?: number; running_pods?: number }> = {};
      const syncReports: Record<string, BackendSyncReport> = {};

      page.on("response", async (response) => {
        try {
          const url = new URL(response.url());
          if (url.pathname === "/api/admin/kubernetes/clusters" && response.status() === 200) {
            const body = (await response.json()) as { clusters?: Array<{ id: string; sync?: { state?: string; partial?: boolean }; nodes?: number; ready_nodes?: number; pods?: number; running_pods?: number }> };
            for (const item of body.clusters ?? []) catalogClusters[item.id] = item;
          } else if (url.pathname === "/api/admin/kubernetes/sync" && response.status() === 200) {
            const clusterId = url.searchParams.get("cluster") || "";
            if (clusterId) syncReports[clusterId] = (await response.json()) as BackendSyncReport;
          }
        } catch {
          // Ignore body parse errors on cancelled navigations.
        }
      });

      await openApp(page, "/agent/kubernetes");
      await expect(page.getByRole("heading", { name: "Clusters", exact: true })).toBeVisible();
      const rows = page.locator('[aria-label="Cluster rows"] article');
      await expect(rows).toHaveCount(6);

      // Wait until one /api/admin/kubernetes/sync report per listed cluster has arrived.
      await expect.poll(() => Object.keys(syncReports).sort().join(","), { timeout: 30_000 }).toBe(
        [clusters.degraded, clusters.failed, clusters.forbidAll, clusters.gate, clusters.limitedRbac, clusters.secret].sort().join(","),
      );

      // Verify the exact discrepancy on noncore-fault (clusters.failed):
      // - /api/admin/kubernetes/clusters (8-core-kind overview envelope) says ready with node/pod counts
      // - /api/admin/kubernetes/sync?cluster=noncore-fault (authoritative 23-kind report) says error (ConfigMap 503)
      expect(catalogClusters[clusters.failed]?.sync?.state).toBe("ready");
      expect(catalogClusters[clusters.failed]?.sync?.partial).toBe(false);
      expect(catalogClusters[clusters.failed]?.nodes).toBe(2);
      expect(catalogClusters[clusters.failed]?.ready_nodes).toBeGreaterThanOrEqual(1);
      expect(catalogClusters[clusters.failed]?.pods).toBe(32);
      expect(catalogClusters[clusters.failed]?.running_pods).toBeGreaterThanOrEqual(28);
      expect(syncReports[clusters.failed]?.state).toBe("error");
      expect(syncReports[clusters.failed]?.partial).toBe(true);
      expect(syncReports[clusters.failed]?.failing?.some((item) => item.kind === "ConfigMap")).toBe(true);

      // Assert the rendered noncore-fault row shows Failed with no counts (Hidden, Not ready, 0 meters).
      const noncoreRow = page.locator(`article[aria-label="Cluster ${clusters.failed}"]`);
      await expect(noncoreRow).toBeVisible();
      await expect(noncoreRow.locator("[data-sync-state-label]")).toHaveText("Failed");
      await expect(noncoreRow.locator("meter")).toHaveCount(0);
      await expect(noncoreRow.getByText("Hidden", { exact: true })).toHaveCount(2);
      await expect(noncoreRow.getByText("Not ready", { exact: true })).toHaveCount(1);
      await expect(noncoreRow.getByText(/\d+\s*\/\s*2/)).toHaveCount(0);
      await expect(noncoreRow.getByText(/\d+\s*\/\s*32/)).toHaveCount(0);

      // Also verify gate (Failed) and degraded (Degraded) show authoritative states and no counts.
      for (const [id, expectedLabel] of [
        [clusters.gate, "Failed"],
        [clusters.degraded, "Degraded"],
      ] as const) {
        const row = page.locator(`article[aria-label="Cluster ${id}"]`);
        await expect(row.locator("[data-sync-state-label]")).toHaveText(expectedLabel);
        await expect(row.locator("meter")).toHaveCount(0);
        await expect(row.getByText("Hidden", { exact: true })).toHaveCount(2);
        await expect(row.getByText("Not ready", { exact: true })).toHaveCount(1);
      }

      await saveCapture(page, "k20-fleet-faults", viewport, theme, "blocked-rows");
    }
  });
});

// ---------------------------------------------------------------------------
// sync mode
// ---------------------------------------------------------------------------

test.describe("sync", () => {
  test.skip(mode !== "sync", "sync fixture only");

  test("Fleet page: the Preparing row still behaves", async ({ browser }) => {
    test.setTimeout(120_000);
    for (const viewport of [viewports[0], viewports[1]]) {
      const theme: Theme = viewport.name === "desktop" ? "dark" : "light";
      const page = await newPage(browser, viewport, theme);
      await openApp(page, "/agent/kubernetes");
      await expect(page.getByRole("heading", { name: "Clusters", exact: true })).toBeVisible();
      const rows = page.locator('[aria-label="Cluster rows"] article');
      await expect(rows).toHaveCount(3);
      const preparing = page.locator(`article[aria-label="Cluster ${clusters.preparing}"]`);
      await expect(preparing).toBeVisible();
      await expect(preparing.locator("[data-sync-state-label]")).toHaveText("Preparing");
      await expect(preparing.locator("[data-first-sync]")).toBeVisible();
      const healthyRow = page.locator(`article[aria-label="Cluster ${clusters.healthy}"]`);
      await expect(healthyRow).toBeVisible();
      await expect(healthyRow.locator("[data-sync-state-label]")).toHaveText("Ready");
      await expect(healthyRow.locator("meter")).toHaveCount(2);
      const recoveryRow = page.locator(`article[aria-label="Cluster ${clusters.recovery}"]`);
      await expect(recoveryRow).toBeVisible();
      await expect(recoveryRow.locator("[data-sync-state-label]")).toHaveText("Failed");
      await expect(recoveryRow.locator("meter")).toHaveCount(0);
      await expect(recoveryRow.getByText("Hidden", { exact: true })).toHaveCount(2);
      await saveCapture(page, "k20-fleet", viewport, theme, "preparing-row");
    }
  });

  test("Preparing copy and the only loading line, then release to Ready without a reload", async ({ browser }) => {
    test.setTimeout(240_000);
    const fake = controlClient(preparingControlURL, preparingControlCA);

    // 1. First-sync progress: copy is explicit and no data view renders. The
    //    progress panel reports listing counts, which are progress, not data.
    for (const viewport of viewports) {
      for (const theme of themes) {
        const page = await newPage(browser, viewport, theme);
        await openCluster(page, clusters.preparing);
        await waitSurface(page, "preparing", 90_000);
        await expect(page.getByRole("heading", { name: "Preparing your cluster" })).toBeVisible();
        await expect(page.getByText(`First sync of ${clusters.preparing}: every tab opens with data once this finishes.`)).toBeVisible();
        await expect(page.locator("[data-preparing-panel]")).toBeVisible();
        await expectNoDataViews(page, "overview");
        await expect(page.locator("[data-pending-note]")).toHaveCount(0);
        await saveCapture(page, "k20-preparing", viewport, theme, "first-sync");
      }
    }

    // 2. Release the last core kind: the progress panel closes and the one
    //    legitimate loading line appears while a non-core kind is pending.
    const page = await newPage(browser, viewports[0], "dark");
    await openCluster(page, clusters.preparing);
    const origin = await page.evaluate(() => performance.timeOrigin);
    await fake("/_fake/list-holds", { resource: "statefulsets", hold: false });
    await waitSurface(page, "preparing", 90_000);
    const note = page.locator("[data-pending-note]");
    await expect(note).toBeVisible({ timeout: 60_000 });
    await expect(note).toHaveText(/^Loading .*(more resource type|ConfigMap)/);
    const panelText = await page.locator("#kubernetes-view-panel").innerText();
    expect(panelText.match(/Loading/g)?.length).toBe(1);
    await expectNoClusterData(page, "overview");
    await saveCapture(page, "k20-pending", viewports[0], "dark", "only-loading");

    const mobileNote = await newPage(browser, viewports[1], "light");
    await openCluster(mobileNote, clusters.preparing);
    await waitSurface(mobileNote, "preparing", 60_000);
    await expect(mobileNote.locator("[data-pending-note]")).toBeVisible({ timeout: 30_000 });
    await saveCapture(mobileNote, "k20-pending", viewports[1], "light", "only-loading");

    // 3. Release the last held kind: surface gone, data rendered, top bar Live,
    //    same page, same timeOrigin.
    await fake("/_fake/list-holds", { resource: "configmaps", hold: false });
    for (const current of [page, mobileNote]) {
      await expect(current.locator("[data-sync-health]")).toHaveCount(0, { timeout: 120_000 });
      await expectTopReady(current);
      await expect(marker(current, "overview")).toBeVisible({ timeout: 30_000 });
    }
    expect(await page.evaluate(() => performance.timeOrigin)).toBe(origin);
    await saveCapture(page, "k20-recovered", viewports[0], "dark", "ready");
    await saveCapture(mobileNote, "k20-recovered", viewports[1], "light", "ready");
  });

  test("A failing held kind is released; the surface clears and the views render without a reload, and on the fleet page the row flips from Failed to Ready with counts while the overview envelope still says failed", async ({ browser }) => {
    test.setTimeout(300_000);
    const fake = controlClient();
    const desktop = await newPage(browser, viewports[0], "dark");
    const mobile = await newPage(browser, viewports[1], "light");
    const pages = [
      { page: desktop, viewport: viewports[0], theme: "dark" as Theme },
      { page: mobile, viewport: viewports[1], theme: "light" as Theme },
    ];
    for (const entry of pages) await openCluster(entry.page, clusters.recovery);
    const origins = await Promise.all(pages.map((entry) => entry.page.evaluate(() => performance.timeOrigin)));

    for (const entry of pages) {
      await waitSurface(entry.page, "failed", 120_000);
      await expectTopState(entry.page, /Failed/);
      await expectNoClusterData(entry.page, "overview");
      await expectNoLoadingCopy(entry.page);
      await saveCapture(entry.page, "k20-held", entry.viewport, entry.theme, "failed");
    }

    // Open fleet pages (1440 dark and 390 light) right before releasing the held
    // StatefulSet kind. Each fleet page fetches /api/admin/kubernetes/clusters
    // on mount and receives recovery.sync.state === "error" (and because no
    // cluster is syncing, the catalog refresh interval is 30s while /sync polls
    // every 5s).
    const fleetDesktop = await newPage(browser, viewports[0], "dark");
    const fleetMobile = await newPage(browser, viewports[1], "light");
    const fleetPages = [
      { page: fleetDesktop, viewport: viewports[0], theme: "dark" as Theme, catalogStates: [] as string[], syncStates: [] as string[] },
      { page: fleetMobile, viewport: viewports[1], theme: "light" as Theme, catalogStates: [] as string[], syncStates: [] as string[] },
    ];
    for (const entry of fleetPages) {
      entry.page.on("response", async (response) => {
        try {
          const url = new URL(response.url());
          if (url.pathname === "/api/admin/kubernetes/clusters" && response.status() === 200) {
            const body = (await response.json()) as { clusters?: Array<{ id: string; sync?: { state?: string } }> };
            const found = body.clusters?.find((item) => item.id === clusters.recovery);
            if (found?.sync?.state) entry.catalogStates.push(found.sync.state);
          } else if (url.pathname === "/api/admin/kubernetes/sync" && url.searchParams.get("cluster") === clusters.recovery && response.status() === 200) {
            const body = (await response.json()) as BackendSyncReport;
            if (body.state) entry.syncStates.push(body.state);
          }
        } catch {
          // Ignore parse errors on teardown.
        }
      });
      await openApp(entry.page, "/agent/kubernetes");
      const recoveryRow = entry.page.locator(`article[aria-label="Cluster ${clusters.recovery}"]`);
      await expect(recoveryRow.locator("[data-sync-state-label]")).toHaveText("Failed", { timeout: 30_000 });
      await expect(recoveryRow.locator("meter")).toHaveCount(0);
      await expect(recoveryRow.getByText("Hidden", { exact: true })).toHaveCount(2);
      await expect.poll(() => entry.catalogStates.at(-1), { timeout: 10_000 }).toBe("error");
      await expect.poll(() => entry.syncStates.at(-1), { timeout: 10_000 }).toBe("error");
    }

    await fake("/_fake/list-holds", { resource: "statefulsets", hold: false });

    for (const [index, entry] of pages.entries()) {
      await expect(entry.page.locator("[data-sync-health]")).toHaveCount(0, { timeout: 120_000 });
      await expectTopReady(entry.page);
      await expect(marker(entry.page, "overview")).toBeVisible({ timeout: 30_000 });
      expect(await entry.page.evaluate(() => performance.timeOrigin)).toBe(origins[index]);
      await saveCapture(entry.page, "k20-recovered-held", entry.viewport, entry.theme, "ready");
      await selectView(entry.page, "resources", "Workloads");
      await expect(marker(entry.page, "resources")).toBeVisible();
      await expect(entry.page.locator("[data-sync-health]")).toHaveCount(0);
    }

    // Inverse fleet check: on both fleet pages, /api/admin/kubernetes/sync?cluster=recovery
    // flips to "ready" while the /api/admin/kubernetes/clusters overview envelope
    // still says "error" (failed), and the row renders Ready with node/pod counts and meters.
    for (const entry of fleetPages) {
      const recoveryRow = entry.page.locator(`article[aria-label="Cluster ${clusters.recovery}"]`);
      await expect(recoveryRow.locator("[data-sync-state-label]")).toHaveText("Ready", { timeout: 30_000 });
      await expect(recoveryRow.locator("meter")).toHaveCount(2);
      await expect(recoveryRow.getByText(/1\s*\/\s*2/)).toBeVisible();
      await expect(recoveryRow.getByText(/\d+\s*\/\s*32/)).toBeVisible();
      await expect(recoveryRow.getByText("Hidden", { exact: true })).toHaveCount(0);
      await expect(recoveryRow.getByText("Not ready", { exact: true })).toHaveCount(0);
      await expect.poll(() => entry.syncStates.at(-1), { timeout: 10_000 }).toBe("ready");
      expect(entry.catalogStates[0]).toBe("error");
      await saveCapture(entry.page, "k20-fleet-recovered", entry.viewport, entry.theme, "ready-row");
    }
  });

  test("Continuous /sync polling: a healthy kind that fails later re-blocks every view within the polling window with no interaction, and releasing it re-opens views with no Retry or focus", async ({ browser }) => {
    test.setTimeout(300_000);
    const fake = controlClient();

    // Ensure recovery starts healthy and Ready on the backend.
    await waitBackendSync(clusters.recovery, (body) => body.state === "ready" && body.pending_kinds === 0, 60_000);

    const desktop = await newPage(browser, viewports[0], "dark");
    const mobile = await newPage(browser, viewports[1], "light");
    const pages = [
      { page: desktop, viewport: viewports[0], theme: "dark" as Theme },
      { page: mobile, viewport: viewports[1], theme: "light" as Theme },
    ];
    for (const entry of pages) {
      await openCluster(entry.page, clusters.recovery);
      await expect(entry.page.locator("[data-sync-health]")).toHaveCount(0, { timeout: 30_000 });
      await expectTopReady(entry.page);
      await expect(marker(entry.page, "overview")).toBeVisible();
      await entry.page.evaluate(() => {
        (document.activeElement as HTMLElement | null)?.blur();
        (window as unknown as { __k20UserInteractions: number }).__k20UserInteractions = 0;
        for (const eventName of ["click", "keydown", "focus"]) {
          window.addEventListener(eventName, () => {
            (window as unknown as { __k20UserInteractions: number }).__k20UserInteractions += 1;
          });
        }
      });
    }
    const origins = await Promise.all(pages.map((entry) => entry.page.evaluate(() => performance.timeOrigin)));

    // 1. Hold ControllerRevision (30s capture cadence, non-core kind so overview.sync
    //    stays "ready" while /sync flips to "error"). Wait on the backend for the
    //    sync report to record the failure, then verify both open pages re-block
    //    within the 15s polling window with zero user interaction.
    await fake("/_fake/list-holds", { resource: "controllerrevisions", hold: true, remaining: 10 });
    await waitBackendSync(
      clusters.recovery,
      (body) => body.state === "error" && body.kinds?.ControllerRevision?.state === "error",
      90_000,
    );
    const backendFailedAt = Date.now();
    const overviewDuringFailure = await fetchBackendOverview(clusters.recovery);
    expect(overviewDuringFailure.sync?.state).toBe("ready");

    for (const [index, entry] of pages.entries()) {
      await waitSurface(entry.page, "failed", 20_000);
      const elapsedAfterBackendFail = Date.now() - backendFailedAt;
      expect(elapsedAfterBackendFail, "view must re-block within the 15s /sync polling window").toBeLessThanOrEqual(18_000);
      await expectTopState(entry.page, /Failed/);
      const row = await problemRow(entry.page, "ControllerRevision");
      await expect(row).toContainText("the cluster is slow or rate-limiting reads");
      await expectSince(row);
      await expectNoClusterData(entry.page, "overview");
      await expectNoLoadingCopy(entry.page);
      expect(await entry.page.evaluate(() => (window as unknown as { __k20UserInteractions: number }).__k20UserInteractions)).toBe(0);
      expect(await entry.page.evaluate(() => performance.timeOrigin)).toBe(origins[index]);
      await saveCapture(entry.page, "k20-reblocked", entry.viewport, entry.theme, "failed");
    }

    // Verify all ten explorer views are re-blocked by the late-failing kind.
    for (const view of views) {
      await selectView(desktop, view.id, view.label);
      await expect(surface(desktop)).toBeVisible();
      await expectNoClusterData(desktop, view.id);
      await expectNoLoadingCopy(desktop);
    }
    await selectView(desktop, "overview", "Overview");

    // 2. Reset the interaction counters, blur both pages, and release the hold.
    //    Verify both pages re-open within the 5s error polling window after backend
    //    recovery with no Retry click, no window focus, and no page reload.
    for (const entry of pages) {
      await entry.page.evaluate(() => {
        (document.activeElement as HTMLElement | null)?.blur();
        (window as unknown as { __k20UserInteractions: number }).__k20UserInteractions = 0;
      });
    }
    await fake("/_fake/list-holds", { resource: "controllerrevisions", hold: false });
    await waitBackendSync(clusters.recovery, (body) => body.state === "ready" && body.pending_kinds === 0, 60_000);
    const backendReadyAt = Date.now();

    for (const [index, entry] of pages.entries()) {
      await expect(entry.page.locator("[data-sync-health]")).toHaveCount(0, { timeout: 10_000 });
      const elapsedAfterBackendReady = Date.now() - backendReadyAt;
      expect(elapsedAfterBackendReady, "view must re-open within the 5s error /sync polling window").toBeLessThanOrEqual(8_000);
      await expectTopReady(entry.page);
      await expect(marker(entry.page, "overview")).toBeVisible({ timeout: 10_000 });
      expect(await entry.page.evaluate(() => (window as unknown as { __k20UserInteractions: number }).__k20UserInteractions)).toBe(0);
      expect(await entry.page.evaluate(() => performance.timeOrigin)).toBe(origins[index]);
      await saveCapture(entry.page, "k20-reopened", entry.viewport, entry.theme, "ready");
    }
  });

  test("Healthy cluster renders every view normally, and deep links and the drawer work", async ({ browser }) => {
    test.setTimeout(240_000);
    const page = await newPage(browser, viewports[0], "dark");
    await openCluster(page, clusters.healthy);
    await expectTopReady(page);
    await expect(page.locator("[data-sync-health]")).toHaveCount(0);
    for (const view of views) {
      await selectView(page, view.id, view.label);
      await expect(marker(page, view.id)).toBeVisible();
      await saveCapture(page, `k20-healthy-${view.id}`, viewports[0], "dark", "data");
    }

    const mobile = await newPage(browser, viewports[1], "light");
    await openCluster(mobile, clusters.healthy);
    await expectTopReady(mobile);
    for (const view of views.filter((entry) => ["overview", "resources", "issues", "timeline", "topology"].includes(entry.id))) {
      await selectView(mobile, view.id, view.label);
      await expect(marker(mobile, view.id)).toBeVisible();
      await saveCapture(mobile, `k20-healthy-${view.id}`, viewports[1], "light", "data");
    }

    // Deep link to a resource drawer: it opens on the named resource.
    const deep = await newPage(browser, viewports[0], "dark");
    await openCluster(deep, clusters.healthy, "&view=resources&r=apps~v1~deployments/load-0000/app-00000&tab=overview");
    const dialog = deep.getByRole("dialog");
    await expect(dialog).toBeVisible();
    await expect(dialog).toContainText("app-00000");
    await expect(dialog.locator(".resource-detail-content")).toHaveCount(1);
    await expect(dialog.locator("#kubernetes-drawer-tab-overview")).toBeVisible();
    await saveCapture(deep, "k20-healthy-drawer", viewports[0], "dark", "deeplink");

    // Deep link to a Timeline filter state.
    const timeline = await newPage(browser, viewports[0], "dark");
    await openCluster(timeline, clusters.healthy, "&view=timeline&breaks=1");
    await expect(viewTabs(timeline).getByRole("tab", { name: "Timeline", exact: true })).toHaveAttribute("aria-selected", "true");
    await expect(timeline.getByRole("tablist", { name: "Timeline events", exact: true }).getByRole("tab", { name: "Breaks only", exact: true })).toHaveAttribute("aria-selected", "true");
  });

  test("P8: Timeline namespace/kind and Issues severity/namespace are pixel-identical; Traffic header is the reference", async ({ browser }) => {
    test.setTimeout(180_000);
    const report: Record<string, unknown> = {};
    const alignmentFailures: string[] = [];
    for (const viewport of viewports) {
      for (const theme of themes) {
        const page = await newPage(browser, viewport, theme);
        await openCluster(page, clusters.healthy);
        await expectTopReady(page);

        await selectView(page, "timeline", "Timeline");
        await expect(marker(page, "timeline")).toBeVisible();
        const tlNamespace = await measureField(page, '[aria-label="Kubernetes change timeline"]', "Timeline namespace", "combobox");
        const tlKind = await measureField(page, '[aria-label="Kubernetes change timeline"]', "Timeline kind", "combobox");
        const timelineDeltas = compareFields("namespace", tlNamespace, "kind", tlKind);
        await saveCapture(page, "k20-p8-timeline", viewport, theme, "aligned");

        await selectView(page, "issues", "Issues");
        await expect(marker(page, "issues")).toBeVisible();
        const issueSeverity = await measureField(page, '[aria-label="Kubernetes issues"]', "Issue severity", "combobox");
        const issueNamespace = await measureField(page, '[aria-label="Kubernetes issues"]', "Issue namespace", "combobox");
        console.log(`[k20-p8] ${viewport.name}-${theme} issues severity=${JSON.stringify(issueSeverity)} namespace=${JSON.stringify(issueNamespace)}`);
        let issuesDeltas: Record<string, number> = {};
        try {
          issuesDeltas = compareFields("severity", issueSeverity, "namespace", issueNamespace);
        } catch (err) {
          alignmentFailures.push(`${viewport.name}-${theme} issues: ${String(err)}`);
          issuesDeltas = { labelBaseline: Number(Math.abs(issueSeverity.labelBaseline - issueNamespace.labelBaseline).toFixed(2)), controlTop: Number(Math.abs(issueSeverity.control.top - issueNamespace.control.top).toFixed(2)), controlHeight: Number(Math.abs(issueSeverity.control.height - issueNamespace.control.height).toFixed(2)) };
        }
        await saveCapture(page, "k20-p8-issues", viewport, theme, "issues-row");

        await selectView(page, "traffic", "Traffic");
        await expect(marker(page, "traffic")).toBeVisible();
        const trafficNamespace = await measureControl(page, '[aria-label="Kubernetes traffic flows"]', "Traffic namespace");
        const trafficWindow = await measureControl(page, '[aria-label="Kubernetes traffic flows"]', "Traffic window");
        // The Traffic row is inline: its two controls share one top and height.
        // This is the unchanged reference geometry.
        expect(Math.abs(trafficNamespace.top - trafficWindow.top), "traffic control top").toBeLessThanOrEqual(1);
        expect(Math.abs(trafficNamespace.height - trafficWindow.height), "traffic control height").toBeLessThanOrEqual(1);
        await saveCapture(page, "k20-traffic-reference", viewport, theme, "header");

        report[`${viewport.name}-${theme}`] = { timeline: { namespace: tlNamespace, kind: tlKind, deltas: timelineDeltas }, issues: { severity: issueSeverity, namespace: issueNamespace, deltas: issuesDeltas }, traffic: { namespace: trafficNamespace, window: trafficWindow } };
      }
    }
    fs.mkdirSync(harnessResultsDir(), { recursive: true });
    fs.writeFileSync(path.join(harnessResultsDir(), "k20-p8-measurements.json"), `${JSON.stringify({ report, alignmentFailures }, null, 2)}\n`);
    console.log(`[k20-p8] measurements ${JSON.stringify(report)}`);
    expect(alignmentFailures, alignmentFailures.join("\n")).toEqual([]);
  });
});
