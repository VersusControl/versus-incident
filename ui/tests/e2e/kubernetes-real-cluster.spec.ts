import { expect, test, type Page } from "@playwright/test";
import * as fs from "node:fs";
import * as path from "node:path";
import { fileURLToPath } from "node:url";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { createHash } from "node:crypto";
import { env, openApp } from "./helpers";
import { captureDir, capturePath } from "./support/capture";
import type { AgentApproval, ChatSession, ChatSessionSummary, KubernetesChangesPage } from "../../src/lib/api";

// Runs against the harness kind cluster:
//   plans/harness-run/harness.sh up oss-kind
//   plans/harness-run/harness.sh e2e ui kubernetes-real-cluster.spec.ts --project=chromium
// Fixtures live in plans/harness-run/config/k8s/fixtures.yaml.

const screenshotDir = process.env.E2E_KUBERNETES_CAPTURE_DIR
  ? path.resolve(process.env.E2E_KUBERNETES_CAPTURE_DIR)
  : captureDir();
const canary = "harness-canary-7f3a";
const cluster = process.env.E2E_KIND_CLUSTER ?? "versus-harness";
const workerNode = `${cluster}-worker`;
const backend = process.env.E2E_KUBERNETES_BACKEND;
const execute = promisify(execFile);
const workspace = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../../..");

async function fakeRuntimeJSON(endpoint: string, body?: unknown) {
  const state = process.env.HARNESS_STATE_DIR;
  if (!state) throw new Error("Owned harness state required");
  const upstream = `https://localhost:${process.env.HARNESS_FAKEKUBE_PORT || 19443}`;
  const args = ["--silent", "--show-error", "--fail", "--max-time", "10", "--cacert", path.join(state, "fakekube/ca.crt")];
  if (body) args.push("--header", "Content-Type: application/json", "--data-binary", JSON.stringify(body));
  const result = await execute("curl", [...args, `${upstream}${endpoint}`], { timeout: 15_000 });
  return JSON.parse(result.stdout);
}

interface UpgradeReceipt {
  session_id: string;
  change_id: string;
  pending_approval_id: string;
  executed_approval_id: string;
  cluster_id: string;
  credential_id: string;
  archive_digest: string;
  read_only: boolean;
  status: string;
}

interface UpgradeFixture {
  seed: UpgradeReceipt;
  enterpriseURL: string;
  upgrade: () => Promise<void>;
  verify: () => Promise<UpgradeReceipt>;
  continueHistory: () => Promise<UpgradeReceipt>;
  assertArchive: () => Promise<void>;
  assertPublic: (page: Page, payloads: unknown[]) => Promise<void>;
}

function upgradeReceipt(stateDir: string, mode: "seed" | "verify" | "continue"): UpgradeReceipt {
  const value: unknown = JSON.parse(fs.readFileSync(path.join(stateDir, `upgrade-${mode}.json`), "utf8"));
  if (typeof value !== "object" || value === null) throw new Error("Core upgrade receipt is incomplete");
  const fields = value as Record<string, unknown>;
  if (!["session_id", "change_id", "pending_approval_id", "executed_approval_id", "cluster_id", "credential_id", "archive_digest"].every((key) => typeof fields[key] === "string" && fields[key] !== "") || fields.status !== "ok" || typeof fields.read_only !== "boolean") {
    throw new Error("Core upgrade receipt is incomplete");
  }
  return value as UpgradeReceipt;
}

test.describe("Live bad-rollout break timeline", () => {
  test.skip(process.env.E2E_BAD_ROLLOUT !== "1", "requires the OSS bad-rollout harness scenario");
  test.setTimeout(180_000);

  for (const viewport of [{ name: "desktop", width: 1440, height: 900 }, { name: "mobile", width: 390, height: 844 }]) {
    test(`unmocked proposal horizontal plot dark ${viewport.name}`, async ({ page }, testInfo) => {
      await page.setViewportSize(viewport);
      await page.emulateMedia({ colorScheme: "dark" });
      await page.addInitScript(() => localStorage.setItem("versus.theme", "dark"));
      const errors: string[] = [];
      const reads: URL[] = [];
      const completedReads: Array<{ url: URL; body: KubernetesChangesPage }> = [];
      const captures: unknown[] = [];
      const plots: unknown[] = [];
      const limitations: string[] = [];
      page.on("pageerror", (error) => errors.push(error.message));
      page.on("request", (request) => {
        const url = new URL(request.url());
        if (url.pathname.endsWith("/kubernetes/changes")) reads.push(url);
      });
      page.on("response", async (response) => {
        const url = new URL(response.url());
        if (url.pathname.endsWith("/kubernetes/changes") && response.ok() && await response.finished() === null) completedReads.push({ url, body: await response.json() });
      });
      const capture = async (state: string) => {
        expect(await page.evaluate(() => localStorage.getItem("versus.theme"))).toBe("dark");
        const background = await page.locator("body").evaluate((body) => getComputedStyle(body).backgroundColor);
        expect(background).toBe("rgb(13, 17, 24)");
        await page.locator("html").evaluate((element) => element.setAttribute("data-theme", "dark"));
        await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
        expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
        fs.mkdirSync(screenshotDir, { recursive: true });
        const filename = `runtime-${state}-${viewport.name}-dark.png`;
        const image = await page.screenshot({ path: path.join(screenshotDir, filename), animations: "disabled" });
        captures.push({ filename, classification: "L", viewport, theme: { html_data_theme: "dark", local_storage: "dark", background }, sha256: createHash("sha256").update(image).digest("hex") });
        fs.writeFileSync(path.join(screenshotDir, `captures-${viewport.name}.json`), JSON.stringify({ captures, plots }, null, 2));
      };
      const verifyPlot = async (focused: boolean) => {
        const chart = page.getByRole("region", { name: "Resource timeline chart", exact: true });
        await expect(chart).toBeVisible();
        await expect(chart.locator("[data-marker=change]").first()).toBeAttached();
        const snapshot = () => chart.evaluate((element) => {
          const plot = element.querySelector<HTMLElement>(".resource-timeline-overlay")!;
          const bounds = plot.getBoundingClientRect();
          const points = [...element.querySelectorAll<HTMLElement>("[data-at][data-x]")].map((marker) => {
            const rect = marker.getBoundingClientRect();
            return { id: marker.dataset.eventId, at: marker.dataset.at!, x: Number(marker.dataset.x), actual: marker.classList.contains("resource-timeline-marker") ? rect.left + rect.width / 2 - bounds.left : rect.left - bounds.left, type: marker.dataset.marker ?? "break", display: getComputedStyle(marker).display };
          });
          return { since: element.getAttribute("data-since"), until: element.getAttribute("data-until"), asOf: Number(element.getAttribute("data-now")), width: bounds.width, font: getComputedStyle(element.querySelector(".resource-timeline-label")!).fontSize, rows: [...element.querySelectorAll<HTMLElement>("[data-resource]")].map((row) => row.dataset.resource), points, ticks: [...element.querySelectorAll<HTMLTimeElement>(".resource-timeline-axis time")].map((tick) => ({ at: tick.dateTime, x: parseFloat(tick.style.left), text: tick.textContent, expected: new Date(tick.dateTime).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false }) })), gridlines: element.querySelectorAll(".resource-timeline-gridline").length, now: element.querySelector(".resource-timeline-now-line") ? getComputedStyle(element.querySelector(".resource-timeline-now-line")!).borderLeftStyle : null, window: element.querySelector<HTMLElement>(".resource-timeline-window")?.style.cssText };
        });
        let geometry = await snapshot();
        let responseIDs = new Set<string>();
        await expect.poll(async () => {
          const period = await page.getByRole("group", { name: "Timeline window", exact: true }).locator('[aria-pressed="true"]').innerText();
          const minutes = { "15m": 15, "1h": 60, "6h": 360, "24h": 1440 }[period];
          const namespace = await page.getByRole("textbox", { name: "Timeline namespace", exact: true }).inputValue();
          const kind = await page.getByRole("combobox", { name: "Timeline kind", exact: true }).inputValue();
          geometry = await snapshot();
          const matching = completedReads.filter(({ url }) => url.searchParams.get("since") === geometry.since && url.searchParams.get("until") === geometry.until && (url.searchParams.get("namespace") || "") === namespace && (url.searchParams.get("kind") || "") === kind && Number.isFinite(minutes) && Date.parse(geometry.until!) - Date.parse(geometry.since!) === minutes! * 60_000).at(-1);
          responseIDs = new Set(matching?.body.items.map((item) => item.id));
          return Boolean(matching) && geometry.points.filter((point) => point.type === "change").every((point) => responseIDs.has(point.id!));
        }, { timeout: 15_000 }).toBe(true);
        const since = Date.parse(geometry.since!);
        const until = Date.parse(geometry.until!);
        expect(geometry.width).toBeGreaterThanOrEqual(619);
        expect(geometry.font).toBe("12px");
        expect(geometry.rows.length).toBeGreaterThanOrEqual(1);
        expect(geometry.points.length).toBeGreaterThan(0);
        for (const point of geometry.points) {
          if (point.type === "change") expect(responseIDs.has(point.id!)).toBe(true);
          expect(point.x).toBeCloseTo((Date.parse(point.at) - since) / (until - since) * 100, 5);
          if (point.display !== "none") expect(Math.abs(point.actual - geometry.width * point.x / 100)).toBeLessThan(1.1);
        }
        expect(geometry.ticks.length).toBeGreaterThan(1);
        expect(geometry.gridlines).toBe(geometry.ticks.length);
        for (const tick of geometry.ticks) {
          expect(Math.abs(tick.x - (Date.parse(tick.at) - since) / (until - since) * 100) * geometry.width / 100).toBeLessThan(0.1);
          expect(tick.text).toBe(tick.expected);
        }
        expect(geometry.now).toBe(geometry.asOf >= since && geometry.asOf <= until ? "dashed" : null);
        if (focused) {
          expect(geometry.points.some((point) => point.type === "break")).toBe(true);
          expect(geometry.window).toBeTruthy();
        }
        plots.push({ focused, ...geometry, since: new Date(since).toISOString(), until: new Date(until).toISOString(), response_ids: [...responseIDs] });
        return chart;
      };
      await openApp(page, "/agent/kubernetes");
      const overview = page.getByRole("region", { name: "Break timeline", exact: true });
      await expect(overview.getByRole("article")).toHaveCount(3, { timeout: 90_000 });
      const checkout = overview.getByRole("article", { name: "Deployment checkout", exact: true });
      await expect(checkout).toContainText("OOMKilled ×6");
      await expect(checkout.getByRole("button", { name: /^Warnings \(\d+\)$/ })).toHaveAttribute("aria-expanded", "false");
      await capture("overview");
      await page.getByRole("tab", { name: "Issues", exact: true }).click();
      const issue = page.getByRole("region", { name: "Kubernetes issues", exact: true }).getByRole("article", { name: "Deployment checkout", exact: true }).filter({ hasText: "CrashLoopBackOff" });
      await issue.getByRole("button", { name: "Expand checkout", exact: true }).click();
      await expect(issue).toContainText("OOMKilled ×6");
      await capture("issues");
      await issue.getByRole("button", { name: "Timeline for checkout", exact: true }).click();
      await expect(page).toHaveURL(/root_uid=deployment-checkout/);
      const focused = page.getByRole("region", { name: "Kubernetes change timeline", exact: true });
      let chart = await verifyPlot(true);
      await expect(chart).toContainText("ConfigMap checkout-cfg");
      const crash = chart.locator('[data-marker="crash"][title*="OOMKilled"]').first();
      await expect(crash).toHaveAttribute("title", /OOMKilled exit 137 ×6.*last occurrence/);
      const response = await apiText(page, "/api/admin/kubernetes/issues?root_uid=deployment-checkout&include=changes&namespace=shop");
      expect(response.status).toBe(200);
      const rootPage = JSON.parse(response.body);
      const root = rootPage.items[0];
      if (rootPage.partial_failures?.some((failure: { resource_id: string; class: string }) => failure.resource_id === "autoscaling~v2~horizontalpodautoscalers" && failure.class === "unsupported")) limitations.push("Actual HPA row BLOCKED: live synthetic upstream advertises autoscaling v1 rather than required v2; HPA rendering remains class F evidence.");
      else await expect(chart).toContainText("HPA checkout");
      expect(await crash.getAttribute("data-at")).toBe(root.evidence.terminations.find((item: { reason: string }) => item.reason === "OOMKilled").last);
      await capture("timeline-root-labels");
      await capture("timeline-root-plot");
      const initialViewport = await chart.evaluate((element) => {
        const bounds = element.getBoundingClientRect();
        const labels = [...element.querySelectorAll<HTMLElement>(".resource-timeline-label")].map((label) => ({ width: label.getBoundingClientRect().width, position: getComputedStyle(label).position }));
        const visible = [...element.querySelectorAll<HTMLElement>("[data-marker], .resource-timeline-break-line")].filter((marker) => {
          const rect = marker.getBoundingClientRect();
          return rect.right >= bounds.left + 132 && rect.left < bounds.right && getComputedStyle(marker).display !== "none";
        }).map((marker) => marker.dataset.marker ?? "break");
        return { scrollLeft: element.scrollLeft, labels, visible };
      });
      plots.push({ initialViewport });
      if (viewport.name === "desktop") expect(initialViewport.scrollLeft).toBe(0);
      else expect(initialViewport.scrollLeft).toBeGreaterThanOrEqual(0);
      expect(initialViewport.labels.every((label) => label.width === (viewport.name === "mobile" ? 132 : 200) && label.position === "sticky")).toBe(true);
      expect(initialViewport.visible).toContain("break");
      expect(initialViewport.visible.some((type) => type === "change" || type === "crash")).toBe(true);
      const marker = chart.locator('[data-marker="change"]').first();
      await marker.focus();
      await page.keyboard.press("Enter");
      await expect(marker).toHaveAttribute("aria-expanded", "true");
      const details = page.getByRole("region", { name: "Timeline event details", exact: true });
      await expect(details).toBeVisible();
      await expect(details.getByRole("heading", { name: "Timeline event details", exact: true })).toBeFocused();
      await page.keyboard.press("Escape");
      await expect(details).toHaveCount(0);
      await expect(marker).toBeFocused();
      await marker.focus();
      await page.keyboard.press("Space");
      await expect(details).toBeVisible();
      const selectedEvent = await marker.getAttribute("data-event-id");
      await details.getByRole("button", { name: "Close event details", exact: true }).click();
      await expect(details).toHaveCount(0);
      await expect(chart.locator(`[data-event-id="${selectedEvent}"]`)).toBeFocused();
      await page.keyboard.press("Enter");
      await details.getByRole("button", { name: "Open resource", exact: true }).first().click();
      await expect(page).toHaveURL(/r=/);
      await page.goBack();
      for (const period of ["15m", "1h", "6h", "24h"]) {
        await focused.getByRole("group", { name: "Timeline window", exact: true }).getByRole("button", { name: period, exact: true }).click();
        await expect(focused.getByRole("button", { name: period, exact: true })).toHaveAttribute("aria-pressed", "true");
        chart = await verifyPlot(true);
      }
      await focused.getByRole("button", { name: "1h", exact: true }).click();
      await verifyPlot(true);
      await focused.getByRole("combobox", { name: "Timeline kind", exact: true }).selectOption("Deployment");
      await expect(focused.getByRole("button", { name: /^Warnings \(\d+\)$/ })).toHaveAttribute("aria-expanded", "false");
      await expect(focused.locator("[data-resource]")).toHaveCount(1);
      await focused.getByRole("link", { name: "Clear root filter", exact: true }).click();
      await expect(page).not.toHaveURL(/root_uid=/);
      chart = await verifyPlot(false);
      await expect(focused.getByRole("combobox", { name: "Timeline kind", exact: true })).toHaveValue("Deployment");
      await expect(focused.getByRole("textbox", { name: "Timeline namespace", exact: true })).toHaveValue("shop");
      await focused.getByRole("combobox", { name: "Timeline kind", exact: true }).selectOption("");
      await focused.getByRole("textbox", { name: "Timeline namespace", exact: true }).fill("");
      chart = await verifyPlot(false);
      await capture("timeline-general-labels");
      await capture("timeline-general-initial");
      const liveMarker = chart.locator('[data-marker="change"]').first();
      const liveID = await liveMarker.getAttribute("data-event-id");
      await liveMarker.focus();
      await page.keyboard.press("Enter");
      await expect(details.getByRole("heading", { name: "Timeline event details", exact: true })).toBeFocused();
      for (let refresh = 0; refresh < 2; refresh++) {
        const priorWindow = await chart.getAttribute("data-until");
        await page.waitForResponse(async (response) => new URL(response.url()).pathname.endsWith("/kubernetes/changes") && response.ok() && await response.finished() === null, { timeout: 45_000 });
        await expect(chart).not.toHaveAttribute("data-until", priorWindow!);
        await expect(details).toBeVisible();
        await expect(chart.locator(`[data-event-id="${liveID}"]`)).toHaveAttribute("aria-expanded", "true");
        await expect(details.getByRole("heading", { name: "Timeline event details", exact: true })).toBeFocused();
      }
      await page.keyboard.press("Escape");
      await expect(chart.locator(`[data-event-id="${liveID}"]`)).toBeFocused();
      await chart.evaluate((element) => { element.scrollLeft = element.scrollWidth; });
      await capture("timeline-general-plot");
      const nowEdge = await chart.evaluate((element) => {
        const line = element.querySelector<HTMLElement>(".resource-timeline-now-line")!;
        const plot = element.querySelector<HTMLElement>(".resource-timeline-overlay")!.getBoundingClientRect();
        const bounds = line.getBoundingClientRect();
        return { now: Number(element.getAttribute("data-now")), until: Date.parse(element.getAttribute("data-until")!), left: bounds.left, right: bounds.right, plotRight: plot.right, clipRight: element.getBoundingClientRect().right };
      });
      fs.writeFileSync(path.join(screenshotDir, `now-edge-${viewport.name}.json`), JSON.stringify(nowEdge, null, 2));
      expect(nowEdge.now).toBe(nowEdge.until);
      expect(nowEdge.left).toBeLessThanOrEqual(Math.min(nowEdge.plotRight, nowEdge.clipRight));
      await page.getByRole("tab", { name: "Overview", exact: true }).click();
      await overview.getByRole("button", { name: "Full timeline", exact: true }).click();
      await verifyPlot(false);
      expect(errors).toEqual([]);
      const receipt = { classification: "L", backend: "built OSS / live synthetic fakekube; not native", captures, plots, errors, limitations, completed_windows: completedReads.map(({ url, body }) => ({ since: url.searchParams.get("since"), until: url.searchParams.get("until"), ids: body.items.map((item) => item.id) })), change_requests: reads.map((url) => ({ path: url.pathname, limit: url.searchParams.get("limit"), namespace: url.searchParams.get("namespace"), kind: url.searchParams.get("kind"), has_cursor: url.searchParams.has("cursor") })) };
      fs.writeFileSync(path.join(screenshotDir, `receipt-${viewport.name}.json`), JSON.stringify(receipt, null, 2));
      await testInfo.attach("proposal-runtime-observables", { body: JSON.stringify(receipt), contentType: "application/json" });
    });
  }

  for (const viewport of [{ name: "desktop", width: 1440, height: 900 }, { name: "mobile", width: 390, height: 844 }]) {
    test(`unmocked external timeline filters retain focus ${viewport.name}`, async ({ page }, testInfo) => {
      await page.setViewportSize(viewport);
      await page.addInitScript(() => localStorage.setItem("versus.theme", "dark"));
      await openApp(page, "/agent/kubernetes?view=timeline");
      const chart = page.getByRole("region", { name: "Resource timeline chart", exact: true });
      const details = page.getByRole("region", { name: "Timeline event details", exact: true });
      const namespace = page.getByRole("textbox", { name: "Timeline namespace", exact: true });
      const kind = page.getByRole("combobox", { name: "Timeline kind", exact: true });
      const observations: unknown[] = [];
      for (const control of ["Namespace", "Kind"] as const) {
        const marker = chart.locator('[data-marker="change"][title*="Deployment checkout:"]').first();
        await expect(marker).toBeVisible();
        const selected = await marker.getAttribute("data-event-id");
        await marker.focus();
        await page.keyboard.press("Enter");
        await expect(details.getByRole("heading", { name: "Timeline event details", exact: true })).toBeFocused();
        const field = control === "Namespace" ? namespace : kind;
        await field.focus();
        const filtered = page.waitForResponse(async (response) => {
          const url = new URL(response.url());
          return url.pathname.endsWith("/kubernetes/changes") && url.searchParams.get(control === "Namespace" ? "namespace" : "kind") === (control === "Namespace" ? "payments" : "ConfigMap") && response.ok() && await response.finished() === null;
        });
        if (control === "Namespace") await namespace.fill("payments");
        else await kind.selectOption("ConfigMap");
        await filtered;
        await expect(chart.locator(`[data-event-id="${selected}"]`)).toHaveCount(0);
        await expect(details).toHaveCount(0);
        await expect(field).toBeFocused();
        observations.push({ control, selected, removed: true, external_focus_retained: true });
        if (control === "Namespace") await namespace.fill("");
        else await kind.selectOption("");
      }
      const marker = chart.locator('[data-marker="change"]').first();
      await marker.focus();
      await page.keyboard.press("Enter");
      await expect(details).toBeVisible();
      const selected = await marker.getAttribute("data-event-id");
      await namespace.focus();
      const priorWindow = await chart.getAttribute("data-until");
      await page.waitForResponse(async (response) => new URL(response.url()).pathname.endsWith("/kubernetes/changes") && response.ok() && await response.finished() === null, { timeout: 45_000 });
      await expect(chart).not.toHaveAttribute("data-until", priorWindow!);
      await expect(namespace).toBeFocused();
      await expect(details).toBeVisible();
      await expect(chart.locator(`[data-event-id="${selected}"]`)).toHaveAttribute("aria-expanded", "true");
      observations.push({ control: "Namespace", natural_poll_advanced: true, external_focus_retained: true, selected_retained: true });
      await testInfo.attach("actual-external-focus", { body: JSON.stringify({ classification: "L", viewport, observations }), contentType: "application/json" });
      fs.mkdirSync(screenshotDir, { recursive: true });
      fs.writeFileSync(path.join(screenshotDir, `external-focus-${viewport.name}.json`), JSON.stringify({ classification: "L", viewport, observations }, null, 2));
    });

    test(`unmocked full timeline entry and scoped issue filters ${viewport.name}`, async ({ page }) => {
      await page.setViewportSize(viewport);
      await page.addInitScript(() => localStorage.setItem("versus.theme", "dark"));
      await openApp(page, "/agent/kubernetes");
      await page.getByRole("region", { name: "Break timeline", exact: true }).getByRole("button", { name: "Full timeline", exact: true }).click();
      const chart = page.getByRole("region", { name: "Resource timeline chart", exact: true });
      await expect(chart).toContainText("ConfigMap checkout-cfg");
      await expect(chart).toContainText("HPA checkout");
      await expect(chart).toContainText("Deployment checkout");
      await page.getByRole("tab", { name: "Issues", exact: true }).click();
      const issues = page.getByRole("region", { name: "Kubernetes issues", exact: true });
      await issues.getByRole("textbox", { name: "Issue namespace", exact: true }).fill("shop");
      await issues.getByRole("combobox", { name: "Issue severity", exact: true }).selectOption("critical");
      await expect(issues.getByRole("article").first()).toContainText("checkout");
      await expect(issues).not.toContainText("ledger-api");
      await expect(issues).not.toContainText("node-agent");
      expect(await page.locator("body").evaluate((body) => getComputedStyle(body).backgroundColor)).toBe("rgb(13, 17, 24)");
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
    });
  }

  test("unmocked collapsed issue advertises incomplete history before expansion", async ({ page }) => {
    const rootReads: URL[] = [];
    page.on("request", (request) => {
      const url = new URL(request.url());
      if (url.pathname.endsWith("/kubernetes/issues") && url.searchParams.has("root_uid")) rootReads.push(url);
    });
    await openApp(page, "/agent/kubernetes?view=issues");
    const issues = page.getByRole("region", { name: "Kubernetes issues", exact: true });
    const row = issues.getByRole("article", { name: "Deployment checkout", exact: true }).filter({ hasText: "CrashLoopBackOff" });
    await expect(row).toBeVisible();
    await expect(row.getByRole("button", { name: "Expand checkout", exact: true })).toHaveAttribute("aria-expanded", "false");
    await expect(row.getByRole("button", { name: /^Warnings \(\d+\)$/ })).toBeVisible();
    await expect(row).not.toContainText("No change on it, its config or its nodes");
    expect(rootReads).toHaveLength(0);
    const warnings = row.getByRole("button", { name: /^Warnings \(\d+\)$/ });
    await warnings.click();
    await expect(warnings).toHaveAttribute("aria-expanded", "true");
    await expect(row.getByText("History completeness not yet loaded.", { exact: true })).toBeVisible();
    await expect(row).not.toContainText(/History incomplete\.|History gap:/);
    await expect(row.getByRole("button", { name: "Expand checkout", exact: true })).toHaveAttribute("aria-expanded", "false");
    expect(rootReads).toHaveLength(0);
    await row.getByRole("button", { name: "Expand checkout", exact: true }).click();
    await expect(row.getByText(/History incomplete/)).toBeVisible();
    await expect(row.getByText("Some changes could not be linked.", { exact: true })).toBeVisible();
    expect(rootReads).toHaveLength(1);
  });

  test("unmocked denied refresh hides cached root warnings", async ({ page }) => {
    await openApp(page, "/agent/kubernetes?view=issues");
    const row = page.getByRole("region", { name: "Kubernetes issues", exact: true }).getByRole("article", { name: "Deployment checkout", exact: true }).filter({ hasText: "CrashLoopBackOff" });
    await row.getByRole("button", { name: "Expand checkout", exact: true }).click();
    const warnings = row.getByRole("button", { name: /^Warnings \(\d+\)$/ });
    await warnings.click();
    await expect(row.getByText(/History incomplete/)).toBeVisible();
    await page.context().clearCookies();
    const denied = page.waitForResponse((response) => new URL(response.url()).pathname.endsWith("/kubernetes/issues") && response.status() === 401);
    await page.getByRole("button", { name: "Refresh Kubernetes data", exact: true }).click();
    await denied;
    await expect(page.getByText(/History incomplete|Some changes could not be linked/)).toHaveCount(0);
    await expect(warnings).toHaveCount(0);
  });

  test("unmocked record stream preserves warning disclosure and focus", async ({ page }) => {
    const original = await fakeRuntimeJSON("/api/v1/nodes/node-a") as { status: { conditions: Array<{ type: string; status: string }> } };
    try {
      await openApp(page, "/agent/kubernetes");
      const checkout = page.getByRole("region", { name: "Break timeline", exact: true }).getByRole("article", { name: "Deployment checkout", exact: true });
      const warnings = checkout.getByRole("button", { name: /^Warnings \(\d+\)$/ });
      await warnings.focus();
      await page.keyboard.press("Enter");
      await expect(warnings).toHaveAttribute("aria-expanded", "true");
      const records = page.evaluate(async () => {
        const controller = new AbortController();
        const timer = window.setTimeout(() => controller.abort(), 75_000);
        try {
          const response = await fetch("/api/admin/kubernetes/stream?kinds=Node", { signal: controller.signal });
          if (!response.ok || !response.body) throw new Error("Runtime record stream unavailable");
          const reader = response.body.getReader();
          (window as unknown as { runtimeRecordObserverReady: boolean }).runtimeRecordObserverReady = true;
          let text = "";
          const decoder = new TextDecoder();
          try {
            while (true) {
              const chunk = await reader.read();
              if (chunk.done) return false;
              text += decoder.decode(chunk.value, { stream: true });
              if (/^event: records\r?$/m.test(text)) return true;
            }
          } finally { await reader.cancel(); }
        } finally { window.clearTimeout(timer); controller.abort(); }
      });
      await expect.poll(() => page.evaluate(() => Boolean((window as unknown as { runtimeRecordObserverReady?: boolean }).runtimeRecordObserverReady))).toBe(true);
      const changed = { ...original, status: { ...original.status, conditions: original.status.conditions.map((condition) => condition.type === "Ready" ? { ...condition, status: "False" } : condition) } };
      await fakeRuntimeJSON("/_fake/mutate", { operation: "upsert", resource: "nodes", name: "node-a", object: changed });
      expect(await records).toBe(true);
      await expect(warnings).toHaveAttribute("aria-expanded", "true");
      await expect(warnings).toBeFocused();
    } finally {
      await fakeRuntimeJSON("/_fake/mutate", { operation: "upsert", resource: "nodes", name: "node-a", object: original });
    }
  });

  for (const theme of ["light", "dark"] as const) {
    test(`unmocked mobile focused gap disclosure ${theme}`, async ({ page }) => {
      await page.setViewportSize({ width: 390, height: 844 });
      await page.emulateMedia({ colorScheme: theme });
      await page.addInitScript((colorMode) => localStorage.setItem("versus.theme", colorMode), theme);
      await openApp(page, "/agent/kubernetes?view=timeline&root_uid=deployment-checkout&namespace=shop");
      const focused = page.getByRole("region", { name: "Kubernetes change timeline", exact: true });
      const warnings = focused.getByRole("button", { name: /^Warnings \(\d+\)$/ });
      await warnings.click();
      const gaps = focused.getByRole("region", { name: "Timeline history gaps", exact: true });
      await expect(gaps).toHaveCount(1);
      await expect(gaps).toContainText("History gap:");
      await gaps.scrollIntoViewIfNeeded();
      await expect(gaps).toBeInViewport();
      await expect(warnings).toHaveAttribute("aria-expanded", "true");
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
      await shot(page, `live-break-focus-gap-mobile-${theme}.png`);
    });
  }

  for (const viewport of [{ name: "desktop", width: 1440, height: 900 }, { name: "mobile", width: 390, height: 844 }].flatMap((size) =>
    (["light", "dark"] as const).map((theme) => ({ ...size, theme })))) {
    const theme = viewport.theme;
    test(`unmocked final edge captures ${viewport.name} ${theme}`, async ({ page }, testInfo) => {
      await page.setViewportSize(viewport);
      await page.emulateMedia({ colorScheme: theme });
      await page.addInitScript((colorMode) => localStorage.setItem("versus.theme", colorMode), theme);
      const errors: string[] = [];
      const rootReads: URL[] = [];
      page.on("pageerror", (error) => errors.push(error.message));
      page.on("request", (request) => { const url = new URL(request.url()); if (url.pathname.endsWith("/kubernetes/issues") && url.searchParams.has("root_uid")) rootReads.push(url); });
      const capture = async (state: string) => {
        expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
        fs.mkdirSync(screenshotDir, { recursive: true });
        await page.screenshot({ path: path.join(screenshotDir, `edge-${state}-${viewport.name}-${theme}.png`), animations: "disabled" });
      };
      await openApp(page, "/agent/kubernetes");
      const timeline = page.getByRole("region", { name: "Break timeline", exact: true });
      const overviewRow = timeline.getByRole("article", { name: "Deployment checkout", exact: true });
      await expect(overviewRow).toContainText("OOMKilled ×6");
      const overviewWarnings = overviewRow.getByRole("button", { name: /^Warnings \(\d+\)$/ });
      await expect(overviewWarnings).toHaveAttribute("aria-expanded", "false");
      const geometry = await overviewRow.locator("time").evaluateAll((elements) => elements.filter((element) => element.getBoundingClientRect().height > 0).map((element) => {
        const bounds = element.getBoundingClientRect();
        const row = element.closest("article")!.getBoundingClientRect();
        return { at: element.getAttribute("datetime"), left: bounds.left, right: bounds.right, top: bounds.top, bottom: bounds.bottom, rowLeft: row.left, rowRight: row.right, rowTop: row.top, rowBottom: row.bottom };
      }));
      expect(geometry.length).toBeGreaterThan(0);
      for (const timestamp of geometry) {
        expect(Number.isFinite(Date.parse(timestamp.at!))).toBe(true);
        expect(timestamp.left).toBeGreaterThanOrEqual(timestamp.rowLeft);
        expect(timestamp.right).toBeLessThanOrEqual(timestamp.rowRight);
        expect(timestamp.top).toBeGreaterThanOrEqual(timestamp.rowTop);
        expect(timestamp.bottom).toBeLessThanOrEqual(timestamp.rowBottom);
      }
      if (viewport.name === "desktop") expect(Math.max(...geometry.map((item) => item.right)) - Math.min(...geometry.map((item) => item.right))).toBeLessThan(1);
      await capture("overview-collapsed");
      await overviewWarnings.click();
      await expect(overviewRow.getByText(/History incomplete/)).toBeVisible();
      await capture("overview-expanded");
      await page.getByRole("tab", { name: "Issues", exact: true }).click();
      const row = page.getByRole("region", { name: "Kubernetes issues", exact: true }).getByRole("article", { name: "Deployment checkout", exact: true }).filter({ hasText: "CrashLoopBackOff" });
      const warnings = row.getByRole("button", { name: /^Warnings \(\d+\)$/ });
      await expect(warnings).toHaveAttribute("aria-expanded", "false");
      expect(rootReads).toHaveLength(0);
      await row.scrollIntoViewIfNeeded();
      await capture("issues-collapsed");
      await warnings.click();
      await expect(row.getByText("History completeness not yet loaded.", { exact: true })).toBeVisible();
      expect(rootReads).toHaveLength(0);
      await row.getByRole("button", { name: "Expand checkout", exact: true }).click();
      await expect(row.getByText(/History incomplete/)).toBeVisible();
      await expect(warnings).toHaveAttribute("aria-expanded", "true");
      expect(rootReads).toHaveLength(1);
      await row.getByRole("button", { name: "Collapse checkout", exact: true }).click();
      await expect(warnings).toHaveAttribute("aria-expanded", "true");
      const texts = await row.locator("ul[id] > li").allTextContents();
      await expect(warnings).toHaveText(`Warnings (${new Set(texts).size})`);
      await row.locator("ul[id]").scrollIntoViewIfNeeded();
      await capture("issues-expanded");
      await warnings.click();
      await expect(row.getByRole("button", { name: "Expand checkout", exact: true })).toHaveAttribute("aria-expanded", "false");
      expect(errors).toEqual([]);
      await testInfo.attach("current-runtime-edge-observables", { body: JSON.stringify({ classification: "L", viewport, geometry, root_reads: rootReads.length, warning_items: texts, time_axis_svg_count: await timeline.locator("svg[viewBox]").count(), svg_alignment: "UNVERIFIED: current timeline has no time-axis SVG" }), contentType: "application/json" });
    });
    test(`unmocked break evidence and scoped history ${viewport.name} ${theme}`, async ({ page }) => {
      await page.setViewportSize(viewport);
      await page.emulateMedia({ colorScheme: theme });
      await page.addInitScript((colorMode) => localStorage.setItem("versus.theme", colorMode), theme);
      const errors: string[] = [];
      page.on("pageerror", (error) => errors.push(error.message));
      await openApp(page, "/agent/kubernetes");
      await expect.poll(() => page.locator("html").getAttribute("data-theme")).toBe(theme === "light" ? "light" : null);
      const timeline = page.getByRole("region", { name: "Break timeline", exact: true });
      await expect(timeline.getByRole("article")).toHaveCount(3, { timeout: 90_000 });
      const checkout = timeline.getByRole("article", { name: "Deployment checkout", exact: true });
      await expect(checkout).toContainText("OOMKilled ×6");
      const warnings = checkout.getByRole("button", { name: /^Warnings \(\d+\)$/ });
      await expect(warnings).toHaveAttribute("aria-expanded", "false");
      await expect(checkout.getByText(/History incomplete/)).not.toBeVisible();
      await warnings.focus();
      await page.keyboard.press("Enter");
      await expect(warnings).toHaveAttribute("aria-expanded", "true");
      const warningList = checkout.locator("ul[id]");
      const warningTexts = await warningList.locator("li").allTextContents();
      expect(new Set(warningTexts).size).toBe(warningTexts.length);
      await expect(warnings).toHaveText(`Warnings (${warningTexts.length})`);
      await expect(checkout.getByText(/History incomplete/)).toBeVisible();
      const refreshed = page.waitForResponse((response) => new URL(response.url()).pathname.endsWith("/kubernetes/issues") && !new URL(response.url()).searchParams.has("root_uid"), { timeout: 45_000 });
      await refreshed;
      await expect(warnings).toHaveAttribute("aria-expanded", "true");
      await expect(warnings).toBeFocused();
      await shot(page, `live-break-warnings-expanded-${viewport.name}-${theme}.png`);
      await page.keyboard.press("Space");
      await expect(warnings).toHaveAttribute("aria-expanded", "false");
      if (viewport.name === "desktop") await expect(checkout).toContainText("example.invalid/checkout:v41 → example.invalid/checkout:v42");
      await expect(timeline).not.toContainText(/CANARY|same_object|owned_by|used_by|scaled_by|reconstructed|·/);
      for (const row of await timeline.getByRole("article").all()) {
        const bounds = await row.boundingBox();
        expect(bounds).not.toBeNull();
        expect(bounds!.y + bounds!.height, "three root rows fit above the fold").toBeLessThanOrEqual(viewport.height);
      }
      await expect(checkout.locator("time").first()).toBeVisible();
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
      await shot(page, `live-break-timeline-${viewport.name}-${theme}.png`);

      const root = await apiText(page, "/api/admin/kubernetes/issues?root_uid=deployment-checkout&include=changes&namespace=shop");
      expect(root.status).toBe(200);
      const rootPage = JSON.parse(root.body) as { items: Array<{ root: { uid: string }; changes?: unknown[]; history?: string; gaps?: unknown[]; partial?: boolean }> };
      expect(rootPage.items).toHaveLength(1);
      expect(rootPage.items[0].root.uid).toBe("deployment-checkout");
      expect(rootPage.items[0].changes?.length).toBeGreaterThan(0);
      if (rootPage.items[0].history !== "complete" || rootPage.items[0].gaps?.length || rootPage.items[0].partial) await expect(checkout).not.toContainText("No change on it, its config or its nodes");
      expect(root.body).not.toContain("CANARY");
      for (const [query, status] of [
        ["root_uid=deployment-checkout&include=changes&namespace=payments", 404],
        ["root_uid=deployment-checkout", 400],
        ["include=suspects", 400],
      ] as const) expect((await apiText(page, `/api/admin/kubernetes/issues?${query}`)).status).toBe(status);

      await page.getByRole("tab", { name: "Issues", exact: true }).click();
      const issues = page.getByRole("region", { name: "Kubernetes issues", exact: true });
      const row = issues.getByRole("article", { name: "Deployment checkout", exact: true }).filter({ hasText: "CrashLoopBackOff" });
      await expect(row.getByRole("button", { name: "Expand checkout", exact: true })).toHaveAttribute("aria-expanded", "false");
      const issueWarnings = row.getByRole("button", { name: /^Warnings \(\d+\)$/ });
      await expect(issueWarnings).toHaveAttribute("aria-expanded", "false");
      await issueWarnings.click();
      await expect(row.getByText("History completeness not yet loaded.", { exact: true })).toBeVisible();
      await shot(page, `live-break-issues-before-${viewport.name}-${theme}.png`);
      await row.getByRole("button", { name: "Expand checkout", exact: true }).click();
      await expect(row).toContainText("OOMKilled ×6");
      await expect(issueWarnings).toHaveAttribute("aria-expanded", "true");
      await expect(row.getByText(/History incomplete/)).toBeVisible();
      await shot(page, `live-break-issues-${viewport.name}-${theme}.png`);
      const issueTexts = await row.locator("ul[id] > li").allTextContents();
      expect(new Set(issueTexts).size).toBe(issueTexts.length);
      await expect(issueWarnings).toHaveText(`Warnings (${issueTexts.length})`);
      await issues.getByRole("combobox", { name: "Issue severity", exact: true }).selectOption("critical");
      await expect(issueWarnings).toHaveAttribute("aria-expanded", "false");
      await issues.getByRole("combobox", { name: "Issue severity", exact: true }).selectOption("");
      await expect(issueWarnings).toHaveAttribute("aria-expanded", "false");
      await row.getByRole("button", { name: "Timeline for checkout", exact: true }).click();
      await expect(page).toHaveURL(/root_uid=deployment-checkout/);
      const lane = page.getByRole("region", { name: "Timeline lane checkout", exact: true });
      await expect(lane).toContainText("Break: CrashLoopBackOff");
      await expect(lane).toContainText("Crash: OOMKilled exit 137 ×6");
      await expect(lane).not.toContainText("CANARY");
      for (const toggle of await page.getByRole("button", { name: /^Warnings \(\d+\)$/ }).all()) await expect(toggle).toHaveAttribute("aria-expanded", "false");
      const focused = page.getByRole("region", { name: "Kubernetes change timeline", exact: true });
      const focusedWarnings = focused.getByRole("button", { name: /^Warnings \(\d+\)$/ });
      await focusedWarnings.focus();
      await page.keyboard.press("Enter");
      await expect(focusedWarnings).toHaveAttribute("aria-expanded", "true");
      await expect(focused.getByRole("region", { name: "Timeline history gaps", exact: true })).toHaveCount(1);
      await expect(lane.getByRole("region", { name: "Timeline history gaps", exact: true })).toHaveCount(0);
      await expect(focused).toContainText("history rebuilt from ReplicaSets");
      await expect(focused).toContainText("Node and removed-resource links may be incomplete.");
      await shot(page, `live-break-focus-warnings-${viewport.name}-${theme}.png`);
      await focused.getByRole("combobox", { name: "Timeline kind", exact: true }).selectOption("Deployment");
      await expect(focused.getByRole("button", { name: /^Warnings \(\d+\)$/ })).toHaveAttribute("aria-expanded", "false");
      await expect(lane).toBeVisible();
      await shot(page, `live-break-focus-${viewport.name}-${theme}.png`);
      await page.getByRole("tab", { name: "Overview", exact: true }).click();
      await timeline.getByRole("article", { name: "DaemonSet node-agent", exact: true }).getByRole("button", { name: "Timeline for node-agent", exact: true }).click();
      await expect(page.getByRole("region", { name: "Timeline lane node-agent", exact: true })).toBeVisible();
      await expect(page.getByRole("region", { name: "Timeline lane checkout", exact: true })).toHaveCount(0);
      for (const toggle of await page.getByRole("button", { name: /^Warnings \(\d+\)$/ }).all()) await expect(toggle).toHaveAttribute("aria-expanded", "false");
      expect(errors).toEqual([]);
    });
  }
});

test.describe("Live partial Kubernetes warnings", () => {
  test.skip(process.env.E2E_KUBERNETES_PARTIAL !== "1", "requires actual upstream resource failures");
  test("unmocked incomplete zero broken count stays qualified", async ({ page }) => {
    await openApp(page, "/agent/kubernetes");
    const response = await apiText(page, "/api/admin/kubernetes/issues?broken=true&include=changes&limit=5");
    expect(response.status).toBe(200);
    const result = JSON.parse(response.body) as { broken: { total: number }; sync: { partial: boolean }; partial_failures?: unknown[] };
    expect(result.broken.total).toBe(0);
    expect(result.sync.partial || Boolean(result.partial_failures?.length)).toBe(true);
    const timeline = page.getByRole("region", { name: "Break timeline", exact: true });
    await expect(timeline.getByText("No broken workloads in available evidence", { exact: true })).toBeVisible();
    await expect(timeline.getByText("No broken workloads", { exact: true })).toHaveCount(0);
  });
  for (const viewport of [{ name: "desktop", width: 1440, height: 900 }, { name: "mobile", width: 390, height: 844 }]) {
    test(`truthful collapsed disclosure ${viewport.name}`, async ({ page }) => {
      await page.setViewportSize(viewport);
      await openApp(page, "/agent/kubernetes");
      const overview = await apiText(page, "/api/admin/kubernetes/overview");
      expect(overview.status).toBe(200);
      const body = JSON.parse(overview.body) as { partial_failures?: unknown[] };
      expect(body.partial_failures?.length).toBeGreaterThan(0);
      const health = page.getByRole("region", { name: "Cluster health", exact: true });
      const warnings = health.getByRole("button", { name: /^Warnings \(\d+\)$/ });
      await expect(warnings).toHaveAttribute("aria-expanded", "false");
      await expect(health).not.toContainText(/Synced|Healthy|All healthy/);
      await expect(page.getByRole("region", { name: "Break timeline", exact: true }).getByRole("button", { name: /^Broken / })).toBeVisible();
      await shot(page, `runtime-partial-collapsed-${viewport.name}.png`);
      await warnings.focus();
      await page.keyboard.press("Enter");
      await expect(warnings).toHaveAttribute("aria-expanded", "true");
      const texts = await health.locator("ul[id] li").allTextContents();
      expect(new Set(texts).size).toBe(texts.length);
      await expect(warnings).toHaveText(`Warnings (${texts.length})`);
      await expect(health.getByText(/forbidden/)).toBeVisible();
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
      await shot(page, `runtime-partial-expanded-${viewport.name}.png`);
    });
  }
});

test.describe("Live untracked Kubernetes warnings", () => {
  test.skip(process.env.E2E_KUBERNETES_UNTRACKED !== "1", "requires the triage upstream fixture");
  test("unmocked untracked kind stays distinct from history completeness", async ({ page }) => {
    const original = await fakeRuntimeJSON("/api/v1/namespaces/payments/pods/checkout-api-0") as { spec: Record<string, unknown> };
    try {
      const changed = { ...original, spec: { ...original.spec, serviceAccountName: "untracked-account", volumes: [{ name: "data", persistentVolumeClaim: { claimName: "database" } }] } };
      await fakeRuntimeJSON("/_fake/mutate", { operation: "upsert", resource: "pods", namespace: "payments", name: "checkout-api-0", object: changed });
      await openApp(page, "/agent/kubernetes?view=issues");
      await expect.poll(async () => {
        const response = await page.request.get("/api/admin/kubernetes/issues?root_uid=deployment-checkout&include=changes&namespace=payments");
        expect(response.status()).toBe(200);
        const body = await response.json() as { items: Array<{ untracked?: string[] }> };
        return body.items[0]?.untracked?.slice().sort();
      }, { timeout: 45_000 }).toEqual(["PersistentVolumeClaim", "ServiceAccount"]);
      const row = page.getByRole("region", { name: "Kubernetes issues", exact: true }).getByRole("article", { name: "Deployment checkout-api", exact: true }).filter({ hasText: "CrashLoopBackOff" });
      await row.getByRole("button", { name: "Expand checkout-api", exact: true }).click();
      const warnings = row.getByRole("button", { name: /^Warnings \(\d+\)$/ });
      await warnings.click();
      await expect(row.getByText("Not tracked: PersistentVolumeClaim.", { exact: true })).toBeVisible();
      await expect(row.getByText("Not tracked: ServiceAccount.", { exact: true })).toBeVisible();
      await expect(row.getByText(/History incomplete/)).toBeVisible();
      await expect(row).not.toContainText("No change on it, its config or its nodes");
      const texts = await row.locator("ul[id] > li").allTextContents();
      expect(new Set(texts).size).toBe(texts.length);
      await expect(warnings).toHaveText(`Warnings (${texts.length})`);
    } finally {
      await fakeRuntimeJSON("/_fake/mutate", { operation: "upsert", resource: "pods", namespace: "payments", name: "checkout-api-0", object: original });
    }
  });
});

test.describe("Live Kubernetes model answer", () => {
  test.skip(process.env.E2E_REAL_KUBERNETES_MODEL !== "1", "requires configured real model credentials and break data");
  test("authenticated web answer retains one scoped broken-issues tool call", async ({ page }, testInfo) => {
    test.setTimeout(180_000);
    let session = "";
    try {
      await openApp(page, "/agent/chat");
      await page.getByRole("textbox", { name: "Message", exact: true }).fill("In Kubernetes cluster fakekube, shop/checkout just broke. What changed? Use one list_k8s_issues call with broken=true and include_changes=true to get the break evidence and changes. Report observed versus reconstructed or incomplete history, not a fabricated cause. Do not execute actions.");
      await page.getByRole("button", { name: "Send", exact: true }).click();
      await expect(page).toHaveURL(/session=/);
      session = new URL(page.url()).searchParams.get("session")!;
      let result: ChatSession | undefined;
      await expect.poll(async () => {
        const response = await page.request.get(`/api/admin/chat/sessions/${encodeURIComponent(session)}`);
        expect(response.status()).toBe(200);
        result = await response.json() as ChatSession;
        return result.status;
      }, { timeout: 150_000, intervals: [500, 1000, 2000] }).toBe("idle");
      const turns = result!.turns.filter((turn) => turn.role === "assistant");
      const answer = turns.map((turn) => turn.content).join("\n");
      const calls = turns.flatMap((turn) => turn.tool_calls ?? []);
      expect(answer).toMatch(/checkout/i);
      expect(answer).toMatch(/history|reconstruct|incomplete/i);
      expect(answer).not.toMatch(/CANARY|harness-canary/);
      expect(calls).toHaveLength(1);
      expect(calls[0].Name).toBe("list_k8s_issues");
      expect(JSON.parse(calls[0].Args).broken).toBe(true);
      expect(calls[0].Error).toBeFalsy();
      expect(Buffer.byteLength(calls[0].Output)).toBeLessThanOrEqual(6144);
      expect(turns.flatMap((turn) => turn.events ?? []).some((event) => event.kind === "run_failed")).toBe(false);
      await expect(page.getByRole("log", { name: "Conversation", exact: true })).toContainText(/checkout/i);
      await testInfo.attach("actual-web-model-projection", { body: JSON.stringify({ session, status: result!.status, answer_bytes: Buffer.byteLength(answer), calls: calls.map((call) => ({ name: call.Name, broken: JSON.parse(call.Args).broken, output_bytes: Buffer.byteLength(call.Output), error: Boolean(call.Error) })) }), contentType: "application/json" });
    } finally {
      if (session) {
        const response = await page.request.delete(`/api/admin/chat/sessions/${encodeURIComponent(session)}`, { headers: { Origin: new URL(page.url()).origin } });
        expect(response.ok()).toBe(true);
      }
    }
  });
});

const upgradeTest = test.extend<{ upgradeFixture: UpgradeFixture }>({
  upgradeFixture: [async ({ page }, use) => {
    void page;
    const stateDir = process.env.E2E_UPGRADE_STATE_DIR;
    const project = process.env.E2E_UPGRADE_PROJECT;
    if (!stateDir || !path.isAbsolute(stateDir) || !project?.includes("upgrade")) throw new Error("Isolated upgrade state directory and project required");
    const script = path.join(workspace, "plans/harness-run/tests/upgrade/oss-to-enterprise-upgrade.sh");
    const harnessEnv: Record<string, string | undefined> = { ...process.env, HARNESS_STATE_DIR: stateDir, HARNESS_PROJECT: project };
    const enterprisePort = harnessEnv.HARNESS_ENTERPRISE_PORT;
    if (!enterprisePort || !/^\d+$/.test(enterprisePort) || Number(enterprisePort) < 1 || Number(enterprisePort) > 65535) throw new Error("Harness Enterprise published port required");
    const enterpriseURL = `http://127.0.0.1:${enterprisePort}`;
    const run = async (mode: "prepare" | "seed" | "upgrade" | "verify" | "continue" | "digest" | "stop") => {
      try {
        await execute("bash", [script, mode], { cwd: workspace, env: harnessEnv, timeout: 1_200_000, maxBuffer: 4 * 1024 * 1024 });
      } catch (error) {
        if (typeof error === "object" && error !== null) {
          const output = error as { stdout?: string; stderr?: string };
          fs.writeFileSync(path.join(stateDir, `upgrade-${mode}-failure.log`), `${output.stdout ?? ""}\n${output.stderr ?? ""}`, { mode: 0o600 });
        }
        throw new Error(`Core upgrade lifecycle failed during ${mode}; inspect the private harness output`);
      }
    };
    const originalAuth = { ...env };
    let licenseOrg = "";
    try {
      await run("prepare");
      await run("seed");
      const seed = upgradeReceipt(stateDir, "seed");
      await use({
        seed,
        enterpriseURL,
        upgrade: async () => {
          await run("upgrade");
          let credentials: string;
          try {
            const result = await execute("bash", ["-c", 'set +x; source "$1"; printf "%s\\0%s" "$HARNESS_ADMIN_USERNAME" "$HARNESS_ADMIN_PASSWORD"', "upgrade-auth", path.join(stateDir, "admin.env")], { env: harnessEnv });
            credentials = result.stdout;
          } catch {
            throw new Error("Retained licensed authentication fixture unavailable");
          }
          const [username, password] = credentials.split("\0");
          if (!username || !password) throw new Error("Retained licensed authentication fixture incomplete");
          env.adminUsername = username;
          env.adminPassword = password;
          env.licensed = true;
          const licensed: unknown = JSON.parse(fs.readFileSync(path.join(stateDir, "licensed-kubernetes.json"), "utf8"));
          if (typeof licensed !== "object" || licensed === null || !("org" in licensed) || typeof licensed.org !== "string" || !licensed.org) throw new Error("Signed deployment scope fixture unavailable");
          licenseOrg = licensed.org;
        },
        verify: async () => { await run("verify"); return upgradeReceipt(stateDir, "verify"); },
        continueHistory: async () => { await run("continue"); return upgradeReceipt(stateDir, "continue"); },
        assertArchive: async () => { await run("digest"); },
        assertPublic: async (publicPage, payloads) => {
          expect(Boolean(licenseOrg)).toBe(true);
          for (const text of [await publicPage.locator("body").innerText(), publicPage.url(), ...payloads.map((payload) => JSON.stringify(payload))]) {
            expect(text.includes(licenseOrg), "licensed scope must remain internal").toBe(false);
          }
        },
      });
    } finally {
      Object.assign(env, originalAuth);
      await run("stop");
    }
  }, { timeout: 1_800_000 }],
});

async function shot(page: Page, name: string) {
  if (process.env.E2E_KUBERNETES_SKIP_CAPTURE === "1") return;
  fs.mkdirSync(screenshotDir, { recursive: true });
  await page.screenshot({ path: path.join(screenshotDir, name), fullPage: true, animations: "disabled" });
}

async function openKubernetes(page: Page) {
  await openApp(page, "/agent/kubernetes");
  await expect(page.getByRole("heading", { name: "Kubernetes" })).toBeVisible();
  const response = await page.request.get("/api/admin/kubernetes/overview");
  expect(response.status()).toBe(200);
  const overview = await response.json();
  expect(overview.nodes).toBe(3);
  expect(overview.ready_nodes).toBe(3);
  await expect(page.getByRole("region", { name: "Cluster health", exact: true })).toBeVisible();
}

async function apiText(page: Page, url: string) {
  return page.evaluate(async (target) => {
    const response = await fetch(target, { credentials: "same-origin" });
    return { status: response.status, body: await response.text() };
  }, url);
}

test.describe("Kubernetes on the harness kind cluster", () => {
  test.skip(backend !== "kind" && backend !== "real", "requires plans/harness-run/harness.sh up oss-kind (or enterprise-kind)");
  test.setTimeout(180_000);

  for (const workload of [
    { kind: "Deployment", name: "deploy-multi", message: "steady-line" },
    { kind: "StatefulSet", name: "sts-ordinals", message: "ordinal-line" },
  ]) {
    for (const viewport of [{ width: 1440, height: 1000 }, { width: 390, height: 844 }]) {
      test(`real arriving workload logs ${workload.kind} ${viewport.width}`, async ({ page }, testInfo) => {
        const namespace = process.env.HARNESS_K8S_NAMESPACE;
        expect(namespace, "run.sh exec --k8s kind namespace is required").toBeTruthy();
        await page.setViewportSize(viewport);
        await page.emulateMedia({ colorScheme: "dark" });
        await page.addInitScript(() => localStorage.setItem("versus.theme", "dark"));
        const errors: string[] = [];
        const streamRequests: string[] = [];
        page.on("pageerror", (error) => errors.push(error.message));
        page.on("request", (request) => {
          const url = new URL(request.url());
          if (url.pathname.includes("/logs/stream")) streamRequests.push(url.pathname + url.search);
        });
        await openApp(page, "/agent/kubernetes");
        await expect(page.getByRole("heading", { name: "Kubernetes", exact: true })).toBeVisible();
        await expect(page.getByRole("region", { name: "Cluster health", exact: true })).toBeVisible();
        await page.getByRole("tab", { name: "Workloads", exact: true }).click();
        await page.getByLabel("Workload namespace").fill(namespace!);
        await page.getByLabel("Resource name", { exact: true }).fill(workload.name);
        await page.getByRole("button", { name: `Select ${workload.kind} ${namespace}/${workload.name}`, exact: true }).click();
        const detail = page.getByRole("dialog", { name: "Details panel" });
        await expect(detail.getByRole("heading", { name: `${workload.kind} ${namespace}/${workload.name}`, exact: true })).toBeVisible();
        await detail.getByRole("tab", { name: "Logs", exact: true }).click();
        if (viewport.width === 1440) await detail.getByRole("button", { name: "Expand panel", exact: true }).click();
        const logs = detail.getByRole("region", { name: "Workload live logs" });
        const output = logs.getByLabel("Workload log output", { exact: true });
        const podSelect = logs.getByLabel("Log pod", { exact: true });
        await expect(podSelect).toHaveValue("");
        await expect(podSelect.locator("option").first()).toHaveText("All pods");
        await expect(podSelect.locator("option")).toHaveCount(4);
        const pods = (await podSelect.locator("option").allTextContents()).slice(1);
        if (workload.kind === "StatefulSet") expect(pods).toEqual(["sts-ordinals-0", "sts-ordinals-1", "sts-ordinals-2"]);
        const sequences = async () => {
          const text = await output.innerText();
          return Object.fromEntries(pods.map((pod) => {
            const matches = [...text.matchAll(/pod=([a-z0-9-]+) seq=(\d+)/g)].filter((match) => match[1] === pod);
            return [pod, Math.max(0, ...matches.map((match) => Number(match[2])))];
          }));
        };
        await expect.poll(async () => Object.values(await sequences()).every((seq) => seq > 0)).toBe(true);
        expect(streamRequests).toHaveLength(1);
        const before = await sequences();
        await expect.poll(async () => {
          const current = await sequences();
          return pods.every((pod) => current[pod] > before[pod]);
        }, { timeout: 30_000, intervals: [1_000] }).toBe(true);
        const after = await sequences();
        expect(streamRequests).toHaveLength(1);
        for (const pod of pods) await expect(output.getByRole("button", { name: pod, exact: true }).first()).toBeAttached();
        await expect(output).toContainText(`msg=${workload.message}`);
        await expect(logs).not.toContainText(/Connecting to logs|Logs are unavailable|No log lines/);
        if (viewport.width === 390) {
          await logs.getByRole("button", { name: "More", exact: true }).click();
          await expect(logs.getByRole("group", { name: "Additional log controls" })).toBeVisible();
        }
        await output.getByRole("button", { name: pods[0], exact: true }).last().click();
        const selectedUID = await podSelect.inputValue();
        expect(selectedUID).not.toBe("");
        await expect.poll(async () => {
          const prefixes = await output.getByRole("button").allTextContents();
          return prefixes.length > 0 && prefixes.every((prefix) => prefix === pods[0]);
        }).toBe(true);
        const filter = logs.getByLabel("Log filter", { exact: true });
        await filter.fill("no-such-synthetic-message");
        await expect(output).toContainText("No log lines match this filter.");
        await filter.fill(workload.message);
        await expect(output).toContainText(`msg=${workload.message}`);
        await filter.fill("");
        await podSelect.selectOption("");
        await expect.poll(async () => Object.values(await sequences()).every((seq) => seq > 0)).toBe(true);
        const restoredBefore = await sequences();
        await expect.poll(async () => {
          const current = await sequences();
          return pods.every((pod) => current[pod] > restoredBefore[pod]);
        }, { timeout: 30_000, intervals: [1_000] }).toBe(true);
        const restoredAfter = await sequences();
        // Automatic stream retries are allowed; no Refresh or Reload control is used.
        expect(streamRequests.length).toBeGreaterThanOrEqual(3);
        await logs.getByRole("button", { name: "Jump to latest", exact: true }).click();
        if (viewport.width === 390) await logs.getByRole("button", { name: "More", exact: true }).click();
        await podSelect.blur();
        await expect(podSelect).toHaveValue("");
        expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
        const bounds = await logs.evaluate((element) => {
          const rect = element.getBoundingClientRect();
          return { left: rect.left, right: rect.right, width: innerWidth };
        });
        expect(bounds.left).toBeGreaterThanOrEqual(0);
        expect(bounds.right).toBeLessThanOrEqual(bounds.width);
        await expect(page.getByText(canary)).toHaveCount(0);
        expect(await detail.innerText()).not.toMatch(/Bearer |BEGIN PRIVATE KEY|password=|token=/i);
        expect(errors).toEqual([]);
        const image = capturePath(`kubernetes-${workload.kind.toLowerCase()}-live-logs`, String(viewport.width), "dark", "all-pods");
        await page.screenshot({ path: image, animations: "disabled" });
        const receipt = {
          kind: workload.kind, namespace, viewport, pods, before, after, restoredBefore, restoredAfter, selectedUID,
          allPodsDefault: true, podPrefixFilter: true, textFilter: true, refreshOrReloadUsed: false,
          streamRequests, pageErrors: errors, bounds, image,
          sha256: createHash("sha256").update(fs.readFileSync(image)).digest("hex"),
          qualification: "Real harness kind; synthetic repository fixture logs only; no browser routes or API mocks.",
        };
        await testInfo.attach("live-log-assertions", { body: JSON.stringify(receipt, null, 2), contentType: "application/json" });
        fs.writeFileSync(path.join(process.env.HARNESS_RESULTS_DIR!, `${workload.kind.toLowerCase()}-${viewport.width}-assertions.json`), JSON.stringify(receipt, null, 2));
        await detail.getByRole("button", { name: "Close panel", exact: true }).click();
        await expect(detail).toHaveCount(0);
      });
    }
  }

  test("rapid workload log stream switching recovers from busy admission and settles finite previous logs", async ({ page }, testInfo) => {
    test.setTimeout(240_000);
    const namespace = process.env.HARNESS_K8S_NAMESPACE;
    expect(namespace, "run.sh exec --k8s kind namespace is required").toBeTruthy();
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.emulateMedia({ colorScheme: "dark" });
    await page.addInitScript(() => localStorage.setItem("versus.theme", "dark"));

    const pageErrors: string[] = [];
    interface StreamObservation {
      id: number;
      url: string;
      pathname: string;
      search: string;
      previous: boolean;
      podUID: string;
      sinceSeconds: string;
      startedAt: number;
      status?: number;
      retryAfter?: string | null;
      errorBody?: Record<string, unknown> | null;
      finished?: boolean;
      aborted?: boolean;
    }
    const streamObservations: StreamObservation[] = [];
    const byRequest = new Map<object, StreamObservation>();
    let nextStreamId = 1;

    page.on("pageerror", (error) => pageErrors.push(error.message));
    page.on("request", (request) => {
      const url = new URL(request.url());
      if (!url.pathname.includes("/logs/stream")) return;
      const obs: StreamObservation = {
        id: nextStreamId++,
        url: url.pathname + url.search,
        pathname: url.pathname,
        search: url.search,
        previous: url.searchParams.get("previous") === "true",
        podUID: url.searchParams.get("pod_uid") ?? "",
        sinceSeconds: url.searchParams.get("since_seconds") ?? "",
        startedAt: Date.now(),
      };
      streamObservations.push(obs);
      byRequest.set(request, obs);
    });
    page.on("response", async (response) => {
      const obs = byRequest.get(response.request());
      if (!obs) return;
      obs.status = response.status();
      obs.retryAfter = response.headers()["retry-after"] ?? null;
      if (response.status() >= 400) {
        try {
          obs.errorBody = (await response.json()) as Record<string, unknown>;
        } catch {
          obs.errorBody = null;
        }
      }
    });
    page.on("requestfinished", (request) => {
      const obs = byRequest.get(request);
      if (obs) obs.finished = true;
    });
    page.on("requestfailed", (request) => {
      const obs = byRequest.get(request);
      if (obs) obs.aborted = true;
    });

    const capturedImages: Array<{ name: string; path: string; viewport: string; state: string; sha256: string }> = [];
    const recordCapture = async (surface: string, width: number, height: number, state: string) => {
      await page.setViewportSize({ width, height });
      const logsRegion = page.getByRole("dialog", { name: "Details panel" }).locator(`section[aria-label="Workload live logs"], section[aria-label="Pod logs"]`).first();
      if (width === 390) {
        const moreBtn = logsRegion.getByRole("button", { name: "More", exact: true });
        if ((await moreBtn.count()) > 0 && (await moreBtn.getAttribute("aria-expanded")) !== "true") {
          await moreBtn.click();
          await expect(logsRegion.getByRole("group", { name: "Additional log controls" })).toBeVisible();
        }
      }
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
      const target = capturePath(surface, String(width), "dark", state);
      const buf = await page.screenshot({ path: target, animations: "disabled" });
      capturedImages.push({
        name: path.basename(target),
        path: target,
        viewport: `${width}x${height}`,
        state,
        sha256: createHash("sha256").update(buf).digest("hex"),
      });
    };

    const openSlotHolders = async (podName: string, count = 4) => {
      const controllers: AbortController[] = [];
      const pumps: Promise<void>[] = [];
      try {
        for (let i = 0; i < count; i++) {
          let admitted = false;
          const deadline = Date.now() + 10_000;
          while (!admitted && Date.now() < deadline) {
            const ctrl = new AbortController();
            const res = await fetch(
              `${env.baseURL}/api/admin/kubernetes/pods/${encodeURIComponent(namespace!)}/${encodeURIComponent(podName)}/logs/stream?timestamps=true&since_seconds=60&tail_lines=5`,
              {
                headers: {
                  Accept: "text/event-stream",
                  ...(env.gatewaySecret ? { "X-Gateway-Secret": env.gatewaySecret } : {}),
                },
                signal: ctrl.signal,
              },
            );
            if (res.status === 200 && res.body) {
              const reader = res.body.getReader();
              const first = await reader.read();
              if (!first.done) {
                controllers.push(ctrl);
                pumps.push(
                  (async () => {
                    try {
                      while (!(await reader.read()).done) {
                        // hold slot until aborted
                      }
                    } catch {
                      // expected when aborted
                    }
                  })(),
                );
                admitted = true;
                break;
              }
            }
            ctrl.abort();
            await new Promise((r) => setTimeout(r, 350));
          }
          expect(admitted, `holder stream ${i + 1} must acquire org stream slot`).toBe(true);
        }
      } catch (err) {
        for (const ctrl of controllers) ctrl.abort();
        await Promise.allSettled(pumps);
        throw err;
      }
      return async () => {
        for (const ctrl of controllers) ctrl.abort();
        await Promise.allSettled(pumps);
      };
    };

    await openApp(page, "/agent/kubernetes");
    await expect(page.getByRole("heading", { name: "Kubernetes", exact: true })).toBeVisible();
    await expect(page.getByRole("region", { name: "Cluster health", exact: true })).toBeVisible();

    const workloadResults: Record<string, unknown> = {};
    let samplePodName = "";

    for (const workload of [
      { kind: "Deployment", name: "deploy-multi", message: "steady-line", surface: "kubernetes-deployment-stream-switching" },
      { kind: "StatefulSet", name: "sts-ordinals", message: "ordinal-line", surface: "kubernetes-statefulset-stream-switching" },
    ]) {
      await page.setViewportSize({ width: 1440, height: 1000 });
      await page.getByRole("tab", { name: "Workloads", exact: true }).click();
      await page
        .getByRole("tablist", { name: "Workload kind", exact: true })
        .getByRole("tab", { name: new RegExp(`^${workload.kind}`), exact: false })
        .click();
      await page.getByLabel("Workload namespace").fill(namespace!);
      await page.getByLabel("Resource name", { exact: true }).fill(workload.name);
      const selectBtn = page.getByRole("button", { name: `Select ${workload.kind} ${namespace}/${workload.name}`, exact: true });
      await expect
        .poll(
          async () => {
            if (await selectBtn.isVisible()) return true;
            const refreshBtn = page.getByRole("button", { name: "Refresh Kubernetes data" });
            if (await refreshBtn.isVisible()) await refreshBtn.click();
            return await selectBtn.isVisible();
          },
          { timeout: 60_000, intervals: [1_000, 2_000] },
        )
        .toBe(true);
      await selectBtn.click();

      const detail = page.getByRole("dialog", { name: "Details panel" });
      await expect(detail.getByRole("heading", { name: `${workload.kind} ${namespace}/${workload.name}`, exact: true })).toBeVisible();
      await detail.getByRole("tab", { name: "Logs", exact: true }).click();
      const expandBtn = detail.getByRole("button", { name: "Expand panel", exact: true });
      if (await expandBtn.isVisible()) await expandBtn.click();

      const logs = detail.getByRole("region", { name: "Workload live logs", exact: true });
      const output = logs.getByLabel("Workload log output", { exact: true });
      const podSelect = logs.getByLabel("Log pod", { exact: true });
      const sinceSelect = logs.getByLabel("Log since seconds", { exact: true });
      const previousCheckbox = logs.getByLabel("Previous logs", { exact: true });
      const logStatusPill = logs.getByLabel("Log connection status", { exact: true });

      await expect(podSelect).toHaveValue("");
      await expect(podSelect.locator("option")).toHaveCount(4, { timeout: 60_000 });
      const pods = (await podSelect.locator("option").allTextContents()).slice(1);
      const podUIDs: string[] = [];
      for (let i = 1; i <= 3; i++) {
        podUIDs.push((await podSelect.locator("option").nth(i).getAttribute("value")) ?? "");
      }
      expect(podUIDs.every((uid) => uid.length > 0)).toBe(true);
      if (!samplePodName) samplePodName = pods[0];

      const sequences = async () => {
        const text = await output.innerText();
        return Object.fromEntries(
          pods.map((pod) => {
            const matches = [...text.matchAll(/pod=([a-z0-9-]+) seq=(\d+)/g)].filter((m) => m[1] === pod);
            return [pod, Math.max(0, ...matches.map((m) => Number(m[2])))];
          }),
        );
      };

      await expect.poll(async () => Object.values(await sequences()).every((seq) => seq > 0), { timeout: 60_000 }).toBe(true);
      // Allow any prior aborted stream keepalives to settle before starting the <2s rapid burst.
      await page.waitForTimeout(2_200);

      const waitStreamResponse = async (action: () => Promise<unknown>) => {
        const responsePromise = page.waitForResponse(
          (res) => res.url().includes("/logs/stream"),
          { timeout: 5_000 },
        );
        await action();
        return responsePromise;
      };

      const burstStartIndex = streamObservations.length;
      const burstStartMs = Date.now();

      // 5 rapid stream-reopening control changes after the initial live stream (>=6 total stream selections) within <2s ending on Previous logs
      await waitStreamResponse(() => podSelect.selectOption(podUIDs[0]));
      await waitStreamResponse(() => podSelect.selectOption(""));
      await waitStreamResponse(() => sinceSelect.selectOption("1800"));
      await waitStreamResponse(() => sinceSelect.selectOption("300"));
      await waitStreamResponse(() => previousCheckbox.check());

      const burstElapsedMs = Date.now() - burstStartMs;
      expect(burstElapsedMs, "rapid control switching burst must complete within <2s").toBeLessThan(2_000);

      const burstStreams = streamObservations.slice(burstStartIndex);
      expect(burstStreams.length).toBeGreaterThanOrEqual(5);
      expect(burstStreams.at(-1)?.previous).toBe(true);

      // Wait for automatic recovery of the Previous logs stream without any manual toggle or Reload click.
      await expect
        .poll(
          async () => {
            const sinceBurst = streamObservations.slice(burstStartIndex);
            const prevStreams = sinceBurst.filter((s) => s.previous);
            const succeededPrev = prevStreams.some((s) => s.status === 200);
            const lineCount = await output.locator("[data-log-key]").count();
            const txt = (await output.textContent()) ?? "";
            const settledDom =
              lineCount >= 3 &&
              !txt.includes("Connecting to logs...") &&
              !txt.includes("HTTP 502") &&
              !txt.includes("Admission retry attempts exhausted");
            return succeededPrev && settledDom;
          },
          { timeout: 20_000, intervals: [200, 500] },
        )
        .toBe(true);

      let postBurstStreams = streamObservations.slice(burstStartIndex);
      let busy429Streams = postBurstStreams.filter((s) => s.status === 429);
      if (busy429Streams.length === 0) {
        // When historical tail frames are actively flushing, the server detects socket close immediately and frees slots before the 5th switch.
        // Induce a real transient 429 on Previous logs using short-lived org slot holders and verify automatic recovery without manual toggle.
        await previousCheckbox.uncheck();
        await logs.getByRole("button", { name: "Pause logs", exact: true }).click();
        await page.waitForTimeout(2_200);
        const releaseTransientHolders = await openSlotHolders(pods[0], 4);
        try {
          const transientResp = await waitStreamResponse(() => previousCheckbox.check());
          expect(transientResp.status()).toBe(429);
        } finally {
          await releaseTransientHolders();
        }
        await expect
          .poll(
            async () => {
              const sinceBurst = streamObservations.slice(burstStartIndex);
              const prevStreams = sinceBurst.filter((s) => s.previous);
              const lastPrev = prevStreams.at(-1);
              const lineCount = await output.locator("[data-log-key]").count();
              const txt = (await output.textContent()) ?? "";
              return (
                lastPrev?.status === 200 &&
                lineCount >= 3 &&
                !txt.includes("Connecting to logs...") &&
                !txt.includes("HTTP 502") &&
                !txt.includes("Admission retry attempts exhausted")
              );
            },
            { timeout: 20_000, intervals: [200, 500] },
          )
          .toBe(true);
        postBurstStreams = streamObservations.slice(burstStartIndex);
        busy429Streams = postBurstStreams.filter((s) => s.status === 429);
      }
      expect(busy429Streams.length, "workload must observe structured HTTP 429 busy response and recover automatically").toBeGreaterThanOrEqual(1);
      for (const busy of busy429Streams) {
        expect(busy.retryAfter).toBe("2");
        expect(busy.errorBody).toMatchObject({
          code: "log_stream_busy",
          retryable: true,
          error: "The concurrent Pod log stream limit has been reached.",
        });
      }
      expect(streamObservations.some((s) => s.status === 502)).toBe(false);
      expect(await previousCheckbox.isChecked()).toBe(true);
      // Previous logs is one bounded read, and the status pill names it: there is
      // no live follow to disable.
      await expect(logStatusPill).toHaveText("Previous");

      // Confirm finite Previous logs stays settled and does not reconnect.
      const countAfterPreviousSettled = streamObservations.length;
      await page.waitForTimeout(1_500);
      expect(streamObservations.length).toBe(countAfterPreviousSettled);
      const previousSettledText = (await output.innerText()).trim();
      expect(previousSettledText.length).toBeGreaterThan(0);
      expect(previousSettledText).not.toContain("HTTP 502");

      await recordCapture(workload.surface, 1440, 1000, "previous-settled");
      await recordCapture(workload.surface, 390, 844, "previous-settled");
      await page.setViewportSize({ width: 1440, height: 1000 });

      // Uncheck Previous logs and verify current healthy live logs return and advance.
      await sinceSelect.selectOption("3600");
      await previousCheckbox.uncheck();
      expect(await previousCheckbox.isChecked()).toBe(false);
      await expect(logStatusPill).toHaveText("Live", { timeout: 30_000 });

      await expect.poll(async () => Object.values(await sequences()).every((seq) => seq > 0), { timeout: 30_000 }).toBe(true);
      const recoveredBefore = await sequences();
      await expect
        .poll(
          async () => {
            const current = await sequences();
            return pods.every((pod) => current[pod] > recoveredBefore[pod]);
          },
          { timeout: 30_000, intervals: [1_000] },
        )
        .toBe(true);
      const recoveredAfter = await sequences();
      await expect(output).toContainText(`msg=${workload.message}`);

      await recordCapture(workload.surface, 1440, 1000, "live-recovered");
      await recordCapture(workload.surface, 390, 844, "live-recovered");
      await page.setViewportSize({ width: 1440, height: 1000 });

      let switchCancelAndSaturation: Record<string, unknown> | undefined;
      if (workload.kind === "Deployment") {
        // 1. Rapid pod selection switch (pod 0 -> pod 1) aborts stale stream and prevents stale pod lines.
        const switchPodIdx = streamObservations.length;
        const firstPodReq = page.waitForRequest((req) => req.url().includes("/logs/stream") && req.url().includes(podUIDs[0]));
        await podSelect.selectOption(podUIDs[0]);
        await firstPodReq;
        await podSelect.selectOption(podUIDs[1]);
        await expect
          .poll(async () => {
            const prefixes = await output.getByRole("button").allTextContents();
            return prefixes.length > 0 && prefixes.every((p) => p === pods[1]);
          }, { timeout: 20_000 })
          .toBe(true);
        const podSwitchStreams = streamObservations.slice(switchPodIdx);
        expect(podSwitchStreams.some((s) => s.podUID === podUIDs[0])).toBe(true);
        expect(podSwitchStreams.some((s) => s.podUID === podUIDs[1])).toBe(true);

        await podSelect.selectOption("");
        await expect.poll(async () => Object.values(await sequences()).every((seq) => seq > 0), { timeout: 20_000 }).toBe(true);

        // 2. Pause during busy-retry backoff cancels the retry timer and makes zero requests while paused.
        await logs.getByRole("button", { name: "Pause logs", exact: true }).click();
        await expect(logs.getByRole("button", { name: "Resume logs", exact: true })).toBeVisible();
        await page.waitForTimeout(2_200);

        const releaseHoldersForPause = await openSlotHolders(pods[0], 4);
        let pausedAtCount = 0;
        let countAfterPauseWait = 0;
        try {
          const busyReloadResp = waitStreamResponse(() => logs.getByRole("button", { name: "Reload logs", exact: true }).click());
          const resp = await busyReloadResp;
          expect(resp.status()).toBe(429);
          await logs.getByRole("button", { name: "Pause logs", exact: true }).click();
          await expect(logs.getByRole("button", { name: "Resume logs", exact: true })).toBeVisible();
          pausedAtCount = streamObservations.length;
          await page.waitForTimeout(2_600);
          countAfterPauseWait = streamObservations.length;
          expect(countAfterPauseWait, "pausing must cancel pending admission retry timer").toBe(pausedAtCount);
        } finally {
          await releaseHoldersForPause();
        }
        await page.waitForTimeout(2_200);
        await logs.getByRole("button", { name: "Resume logs", exact: true }).click();
        await expect.poll(async () => Object.values(await sequences()).every((seq) => seq > 0), { timeout: 20_000 }).toBe(true);

        // 3. Legitimate sustained capacity saturation exhausts bounded retries (4 attempts) with explicit terminal message and does not loop or replenish on pause/resume.
        await logs.getByRole("button", { name: "Pause logs", exact: true }).click();
        await page.waitForTimeout(2_200);
        const releaseHoldersForExhaustion = await openSlotHolders(pods[0], 4);
        let saturationAttempts = 0;
        let saturationCountAfterWait = 0;
        let saturationCountAfterPauseResume = 0;
        try {
          const saturationStartIdx = streamObservations.length;
          await logs.getByRole("button", { name: "Reload logs", exact: true }).click();
          await expect
            .poll(
              async () => (await output.textContent()) ?? "",
              { timeout: 20_000, intervals: [250, 500] },
            )
            .toContain("The concurrent Pod log stream limit has been reached. Admission retry attempts exhausted. Reload logs to retry.");

          saturationAttempts = streamObservations.slice(saturationStartIdx).length;
          expect(saturationAttempts, "1 initial attempt + 3 bounded retries = 4 total attempts").toBe(4);
          expect(streamObservations.slice(saturationStartIdx).every((s) => s.status === 429 && s.retryAfter === "2")).toBe(true);

          // Confirm no 5th retry occurs (not an endless loop).
          await page.waitForTimeout(3_000);
          saturationCountAfterWait = streamObservations.slice(saturationStartIdx).length;
          expect(saturationCountAfterWait).toBe(4);

          // Confirm pause/resume does not replenish the exhausted admission budget.
          await logs.getByRole("button", { name: "Pause logs", exact: true }).click();
          await logs.getByRole("button", { name: "Resume logs", exact: true }).click();
          await page.waitForTimeout(2_500);
          saturationCountAfterPauseResume = streamObservations.slice(saturationStartIdx).length;
          expect(saturationCountAfterPauseResume).toBe(4);
          await expect(output).toContainText(
            "The concurrent Pod log stream limit has been reached. Admission retry attempts exhausted. Reload logs to retry.",
          );

          await recordCapture(workload.surface, 1440, 1000, "admission-exhausted");
          await recordCapture(workload.surface, 390, 844, "admission-exhausted");
          await page.setViewportSize({ width: 1440, height: 1000 });
        } finally {
          await releaseHoldersForExhaustion();
        }

        // Once capacity is freed, clicking Reload logs recovers live streaming cleanly.
        await page.waitForTimeout(2_200);
        await logs.getByRole("button", { name: "Reload logs", exact: true }).click();
        await expect.poll(async () => Object.values(await sequences()).every((seq) => seq > 0), { timeout: 25_000 }).toBe(true);

        switchCancelAndSaturation = {
          rapidPodSwitchAbortedStale: true,
          pauseCancelledPendingRetry: countAfterPauseWait === pausedAtCount,
          saturationAttempts,
          saturationCountAfterWait,
          saturationCountAfterPauseResume,
          recoveredAfterReload: true,
        };
      }

      // Close panel and verify no further stream requests fire after close.
      await detail.getByRole("button", { name: "Close panel", exact: true }).click();
      await expect(detail).toHaveCount(0);
      const countAtClose = streamObservations.length;
      await page.waitForTimeout(1_000);
      expect(streamObservations.length).toBe(countAtClose);

      workloadResults[workload.kind] = {
        namespace,
        name: workload.name,
        pods,
        burstElapsedMs,
        burstSelectionsCount: burstStreams.length,
        busy429ObservedCount: busy429Streams.length,
        busy429Sample: busy429Streams[0] ?? null,
        previousSettledWithoutManualToggle: true,
        previousSettledTextPreview: previousSettledText.slice(0, 240),
        recoveredBefore,
        recoveredAfter,
        switchCancelAndSaturation,
      };
    }

    // Direct Pod previous_unavailable terminal check on real kind cluster (0 restarts -> terminal error, 1 request, 0 retries).
    await page
      .getByRole("tablist", { name: "Workload kind", exact: true })
      .getByRole("tab", { name: /^Pod/, exact: false })
      .click();
    await page.getByLabel("Workload namespace").fill(namespace!);
    await page.getByLabel("Resource name", { exact: true }).fill(samplePodName);
    const podBtn = page.getByRole("button", { name: `Select Pod ${namespace}/${samplePodName}`, exact: true });
    await expect(podBtn).toBeVisible({ timeout: 30_000 });
    await podBtn.click();

    const podDetail = page.getByRole("dialog", { name: "Details panel" });
    await podDetail.getByRole("tab", { name: "Logs", exact: true }).click();
    const podLogs = podDetail.getByRole("region", { name: "Pod logs", exact: true });
    const podOutput = podLogs.getByLabel("Pod log output", { exact: true });
    await expect(podOutput).toContainText("msg=steady-line", { timeout: 20_000 });

    const podPrevStartIdx = streamObservations.length;
    await podLogs.getByLabel("Previous container logs", { exact: true }).check();
    await expect(podOutput).toContainText("previous_unavailable Previous container logs are unavailable.", {
      timeout: 15_000,
    });
    await expect(podLogs.getByLabel("Log connection status", { exact: true })).toHaveText("error");
    await page.waitForTimeout(2_500);
    const podPrevStreams = streamObservations.slice(podPrevStartIdx);
    expect(podPrevStreams.length, "previous_unavailable on Pod must stay terminal with 1 request and 0 retries").toBe(1);

    await recordCapture("kubernetes-pod-stream-switching", 1440, 1000, "previous-unavailable");
    await podDetail.getByRole("button", { name: "Close panel", exact: true }).click();
    await expect(podDetail).toHaveCount(0);

    expect(pageErrors).toEqual([]);
    expect(streamObservations.some((s) => s.status === 502)).toBe(false);

    const receipt = {
      qualification: "live — Real harness kind cluster and rebuilt local OSS image; synthetic repository workloads in per-run namespace; zero route mocks.",
      namespace,
      workloadResults,
      podPreviousUnavailableTerminal: {
        pod: samplePodName,
        requestsCount: podPrevStreams.length,
        terminalStatus: "error",
      },
      totalStreamRequests: streamObservations.length,
      any502: streamObservations.some((s) => s.status === 502),
      pageErrors,
      capturedImages,
    };
    await testInfo.attach("stream-switching-live-acceptance", {
      body: JSON.stringify(receipt, null, 2),
      contentType: "application/json",
    });
    if (process.env.HARNESS_RESULTS_DIR) {
      fs.writeFileSync(
        path.join(process.env.HARNESS_RESULTS_DIR, "stream-switching-live-acceptance.json"),
        JSON.stringify(receipt, null, 2),
      );
    }
  });

  test("post-live rotation busy retry budget resets after admission and auth denied stays terminal", async ({ page }, testInfo) => {
    test.setTimeout(120_000);
    const namespace = process.env.HARNESS_K8S_NAMESPACE;
    expect(namespace, "run.sh exec --k8s kind namespace is required").toBeTruthy();
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.emulateMedia({ colorScheme: "dark" });
    await page.addInitScript(() => localStorage.setItem("versus.theme", "dark"));

    const pageErrors: string[] = [];
    page.on("pageerror", (error) => pageErrors.push(error.message));

    const distDir = path.resolve(workspace, "versus-incident/ui/dist");
    const distIndex = path.join(distDir, "index.html");
    if (fs.existsSync(distIndex)) {
      await page.route("**/*", async (route) => {
        const reqUrl = new URL(route.request().url());
        if (reqUrl.pathname.startsWith("/api/") || reqUrl.pathname.startsWith("/enterprise/")) {
          await route.fallback();
          return;
        }
        if (reqUrl.pathname.startsWith("/assets/")) {
          const assetFile = path.join(distDir, reqUrl.pathname.slice(1));
          if (fs.existsSync(assetFile)) {
            await route.fulfill({
              status: 200,
              contentType: assetFile.endsWith(".css") ? "text/css" : "application/javascript",
              body: fs.readFileSync(assetFile),
            });
            return;
          }
        }
        if (route.request().isNavigationRequest() || reqUrl.pathname === "/" || reqUrl.pathname.startsWith("/agent/")) {
          await route.fulfill({
            status: 200,
            contentType: "text/html",
            body: fs.readFileSync(distIndex),
          });
          return;
        }
        await route.fallback();
      });
    }

    const attemptLog: Array<{ attempt: number; atMs: number; cursor: string; container: string; outcome: string }> = [];
    let t0 = 0;

    await page.route("**/api/admin/kubernetes/workloads/Deployment/deploy-multi/logs/stream**", async (route) => {
      const url = new URL(route.request().url());
      const cursor = url.searchParams.get("cursor") ?? "";
      const container = url.searchParams.get("container") ?? "";
      const attempt = attemptLog.length + 1;
      if (attempt === 1) t0 = Date.now();
      const atMs = Date.now() - t0;

      if (container === "app") {
        attemptLog.push({ attempt, atMs, cursor, container, outcome: "403-forbidden" });
        await route.fulfill({
          status: 403,
          contentType: "application/json",
          body: JSON.stringify({
            error: "Pod logs are forbidden by cluster RBAC.",
            code: "forbidden",
            action: "Grant pods/log get permission to the reader Role.",
            retryable: false,
          }),
        });
        return;
      }

      // Attempts 1, 2, 3 consume all 3 initial admission retries before Attempt 4 admits live.
      if (attempt <= 3) {
        attemptLog.push({ attempt, atMs, cursor, container, outcome: "429-initial-busy" });
        await route.fulfill({
          status: 429,
          headers: { "Retry-After": "1" },
          contentType: "application/json",
          body: JSON.stringify({
            error: "The concurrent Pod log stream limit has been reached.",
            code: "log_stream_busy",
            action: "Close another open log stream or wait briefly and retry.",
            retryable: true,
          }),
        });
        return;
      }

      // Attempt 4 admits live (resets admissionRetries=0 and admissionStartedAt=undefined), emits a line with cursor, and rotates on duration limit.
      if (attempt === 4) {
        attemptLog.push({ attempt, atMs, cursor, container, outcome: "200-admitted-then-limit-rotate" });
        const podsPayload = [
          { uid: "pod-uid-1", name: "deploy-multi-a", containers: ["app"], init_containers: [], admitted: true },
        ];
        await route.fulfill({
          status: 200,
          contentType: "text/event-stream",
          body: [
            `event: sources\ndata: ${JSON.stringify({ workload_uid: "wl-uid", pods: podsPayload, truncated: false })}\n\n`,
            `event: heartbeat\ndata: {"cursor":"rot-cursor-1"}\n\n`,
            `event: line\ndata: ${JSON.stringify({
              workload_uid: "wl-uid",
              pod_uid: "pod-uid-1",
              pod: "deploy-multi-a",
              namespace,
              container: "app",
              incarnation: "1",
              source_id: "src-1",
              text: "level=info msg=pre-rotation-live-line seq=1",
              timestamp: "2026-10-22T07:50:00Z",
              sequence: 1,
              ordinal: 1,
              cursor: "rot-cursor-1",
            })}\n\n`,
            `event: limit\ndata: ${JSON.stringify({ reason: "duration", cursor: "rot-cursor-1" })}\n\n`,
            `event: end\ndata: ${JSON.stringify({ reason: "limit", cursor: "rot-cursor-1" })}\n\n`,
          ].join(""),
        });
        return;
      }

      // Attempt 5 (rotation reconnect with cursor=rot-cursor-1) arrives >10s after Attempt 1 and hits HTTP 429 busy.
      if (attempt === 5) {
        const waitMs = Math.max(0, 10_500 - (Date.now() - t0));
        if (waitMs > 0) await new Promise((r) => setTimeout(r, waitMs));
        const postWaitMs = Date.now() - t0;
        attemptLog.push({ attempt, atMs: postWaitMs, cursor, container, outcome: "429-post-live-rotation-busy" });
        await route.fulfill({
          status: 429,
          headers: { "Retry-After": "1" },
          contentType: "application/json",
          body: JSON.stringify({
            error: "The concurrent Pod log stream limit has been reached.",
            code: "log_stream_busy",
            action: "Close another open log stream or wait briefly and retry.",
            retryable: true,
          }),
        });
        return;
      }

      // Attempt 6 succeeds after the post-rotation busy retry and delivers the post-rotation line.
      attemptLog.push({ attempt, atMs, cursor, container, outcome: "200-post-rotation-recovered" });
      await route.fulfill({
        status: 200,
        contentType: "text/event-stream",
        body: [
          `event: heartbeat\ndata: {"cursor":"rot-cursor-2"}\n\n`,
          `event: line\ndata: ${JSON.stringify({
            workload_uid: "wl-uid",
            pod_uid: "pod-uid-1",
            pod: "deploy-multi-a",
            namespace,
            container: "app",
            incarnation: "1",
            source_id: "src-1",
            text: "level=info msg=post-rotation-recovered-line seq=2",
            timestamp: "2026-10-22T07:50:12Z",
            sequence: 2,
            ordinal: 2,
            cursor: "rot-cursor-2",
          })}\n\n`,
          `event: end\ndata: ${JSON.stringify({ reason: "complete", cursor: "rot-cursor-2" })}\n\n`,
        ].join(""),
      });
    });

    await openApp(page, "/agent/kubernetes");
    await page.getByRole("tab", { name: "Workloads", exact: true }).click();
    await page.getByLabel("Workload namespace").fill(namespace!);
    await page.getByLabel("Resource name", { exact: true }).fill("deploy-multi");
    await page.getByRole("button", { name: `Select Deployment ${namespace}/deploy-multi`, exact: true }).click();

    const detail = page.getByRole("dialog", { name: "Details panel" });
    await detail.getByRole("tab", { name: "Logs", exact: true }).click();
    const logs = detail.getByRole("region", { name: "Workload live logs", exact: true });
    const output = logs.getByLabel("Workload log output", { exact: true });

    await expect(output).toContainText("msg=pre-rotation-live-line seq=1", { timeout: 20_000 });
    await expect(output).toContainText("msg=post-rotation-recovered-line seq=2", { timeout: 25_000 });
    await expect(output).not.toContainText("Admission retry attempts exhausted");

    expect(attemptLog).toHaveLength(6);
    expect(attemptLog[4].outcome).toBe("429-post-live-rotation-busy");
    expect(attemptLog[4].cursor).toBe("rot-cursor-1");
    expect(attemptLog[4].atMs).toBeGreaterThan(10_000);
    expect(attemptLog[5].outcome).toBe("200-post-rotation-recovered");
    expect(attemptLog[5].cursor).toBe("rot-cursor-1");

    // Verify 403 forbidden auth denied is terminal with 1 request and 0 retries.
    await logs.getByLabel("Log container", { exact: true }).selectOption("app");
    await expect(page.getByText("Pod logs are forbidden by cluster RBAC.")).toBeVisible({ timeout: 10_000 });
    await page.waitForTimeout(2_500);
    const forbiddenAttempts = attemptLog.filter((a) => a.container === "app");
    expect(forbiddenAttempts).toHaveLength(1);

    await detail.getByRole("button", { name: "Close panel", exact: true }).click();
    expect(pageErrors).toEqual([]);

    const fixtureReceipt = {
      qualification: "fixture — Route-intercepted workload log stream on real kind workload drawer to deterministically verify >10s post-admission rotation busy retry budget reset and HTTP 403 forbidden terminal non-retry.",
      attemptLog,
      forbiddenAttemptsCount: forbiddenAttempts.length,
      pageErrors,
    };
    await testInfo.attach("stream-switching-fixture-acceptance", {
      body: JSON.stringify(fixtureReceipt, null, 2),
      contentType: "application/json",
    });
    if (process.env.HARNESS_RESULTS_DIR) {
      fs.writeFileSync(
        path.join(process.env.HARNESS_RESULTS_DIR, "stream-switching-fixture-acceptance.json"),
        JSON.stringify(fixtureReceipt, null, 2),
      );
    }
  });

  test("catalog opens the explorer with live cluster facts", async ({ page }) => {
    const pageErrors: Error[] = [];
    page.on("pageerror", (error) => pageErrors.push(error));
    await openApp(page, "/agent/tools");
    const card = page.locator("main article").filter({ has: page.getByRole("heading", { name: "Kubernetes", exact: true }) });
    await card.getByRole("link", { name: "Open Kubernetes" }).click();
    await expect(page).toHaveURL(/\/agent\/kubernetes$/);
    await expect(page.getByRole("region", { name: "Cluster health" })).toBeVisible();
    await expect(page.getByRole("tab", { name: "Workloads", exact: true })).toBeVisible();
    await shot(page, "overview-desktop.png");
    expect(pageErrors).toEqual([]);
  });

  test("healthy workload detail and node drill-down", async ({ page }) => {
    await openKubernetes(page);
    await page.getByRole("tab", { name: "Workloads", exact: true }).click();
    await page.getByLabel("Workload namespace").fill("shop");
    await page.getByLabel("Resource name").fill("qa-web");
    const workloads = page.getByRole("region", { name: "Workloads" });
    const row = workloads.getByRole("button", { name: "Select Deployment shop/qa-web", exact: true });
    await expect(row).toBeVisible({ timeout: 60_000 });
    await row.click();
    const detail = page.getByRole("dialog", { name: "Details panel" });
    await expect(detail.getByRole("heading", { name: "Deployment · shop/qa-web" })).toBeVisible();
    await expect(page.getByText(canary)).toHaveCount(0);
    await shot(page, "workload-detail-desktop.png");
    await page.keyboard.press("Escape");

    await page.getByRole("tab", { name: "Nodes", exact: true }).click();
    const nodes = page.getByRole("region", { name: "Nodes" });
    await nodes.getByRole("button", { name: `View pods on ${workerNode}`, exact: true }).click();
    await expect(nodes.getByRole("list", { name: `Pods on ${workerNode}`, exact: true })).toContainText("node-agent");
    await shot(page, "node-pods-desktop.png");
  });

  test("unhealthy fixtures stay visible and secrets never cross the API", async ({ page }) => {
    await openKubernetes(page);
    await page.getByRole("tab", { name: "Workloads", exact: true }).click();
    await page.getByLabel("Workload namespace").fill("payments");
    await expect(page.getByText("Unhealthy", { exact: true }).first()).toBeVisible({ timeout: 90_000 });
    await shot(page, "unhealthy-desktop.png");

    const pods = await apiText(page, "/api/admin/kubernetes/resources?resource_id=core~v1~pods&namespace=payments&labels=app.kubernetes.io%2Fname%3Dpayments-api");
    expect(pods.status).toBe(200);
    const podName = (JSON.parse(pods.body) as { items: Array<{ name: string }> | null }).items?.[0]?.name;
    expect(podName, "payments-api pod").toBeTruthy();
    // A crash-looping container's previous or current log can be briefly unavailable between restarts.
    let logBody = "";
    await expect.poll(async () => {
      for (const previous of ["true", "false"]) {
        const result = await apiText(page, `/api/admin/kubernetes/pods/payments/${podName}/logs?container=api&tail_lines=50&previous=${previous}`);
        if (result.status === 200 && result.body.includes("panic: database connection refused")) {
          logBody = result.body;
          return true;
        }
      }
      return false;
    }, { timeout: 90_000, intervals: [2_000] }).toBe(true);
    expect(logBody).not.toContain(canary);

    // The chart's reader ClusterRole grants no Secret access, so the Secret read must be refused.
    const secret = await apiText(page, "/api/admin/kubernetes/resources/core~v1~secrets/qa-web-db/describe?namespace=shop");
    expect(secret.status).toBe(403);
    expect(secret.body).not.toContain(canary);

    for (const url of [
      "/api/admin/kubernetes/resources/core~v1~configmaps/qa-web-config/describe?namespace=shop",
      "/api/admin/kubernetes/workloads/Deployment/qa-web?namespace=shop",
    ]) {
      const result = await apiText(page, url);
      expect(result.status, url).toBe(200);
      expect(result.body, url).not.toContain(canary);
    }
  });

  test("explorer stays usable on mobile", async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await openKubernetes(page);
    await expect(page.getByRole("region", { name: "Cluster health", exact: true })).toBeVisible();
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth);
    expect(overflow).toBe(false);
    await shot(page, "overview-mobile.png");
  });
});

upgradeTest.describe("OSS to Enterprise persistent history", () => {
  upgradeTest.skip(process.env.E2E_UPGRADE_LIVE !== "1", "requires the isolated signed-license upgrade harness and real OIDC fixture");
  upgradeTest.setTimeout(1_800_000);

  upgradeTest("archive approvals and chat survive a same-volume licensed upgrade", async ({ page, upgradeFixture }) => {
    const ossURL = process.env.E2E_UPGRADE_OSS_URL;
    if (!ossURL || !["localhost", "127.0.0.1", "[::1]"].includes(new URL(ossURL).hostname)) throw new Error("Loopback OSS upgrade URL required");
    const seed = upgradeFixture.seed;
    await openApp(page, `${ossURL}/agent/chat?session=${encodeURIComponent(seed.session_id)}`);
    await expect(page.getByText("Pre-upgrade investigation retained.", { exact: true })).toBeVisible();
    await upgradeFixture.upgrade();
    await page.context().clearCookies();
    await openApp(page, `${upgradeFixture.enterpriseURL}/agent/chat?session=${encodeURIComponent(seed.session_id)}`);
    const verified = await upgradeFixture.verify();
    expect(verified.archive_digest === seed.archive_digest).toBe(true);
    expect(verified.read_only).toBe(true);

    const approvals = await page.evaluate(async () => {
      const response = await fetch("/api/v1/agent/approvals?state=all", { credentials: "same-origin" });
      return { status: response.status, body: await response.json() as { approvals: AgentApproval[] } };
    });
    expect(approvals.status, "archive listing must work even when actor adapters are disabled").toBe(200);
    for (const id of [seed.pending_approval_id, seed.executed_approval_id]) {
      const matches = approvals.body.approvals.filter((approval) => approval.id === id);
      expect(matches).toHaveLength(1);
      expect(matches[0].read_only).toBe(true);
      for (const decision of ["approve", "reject"]) {
        const result = await page.evaluate(async ({ id, decision }) => {
          const response = await fetch(`/api/v1/agent/approvals/${encodeURIComponent(id)}/${decision}`, { method: "POST", credentials: "same-origin", headers: { "Content-Type": "application/json" }, body: JSON.stringify(decision === "approve" ? { nonce: "archive-probe" } : { reason: "Archive must remain frozen" }) });
          const body: { code?: string } = await response.json();
          return { status: response.status, code: body.code };
        }, { id, decision });
        expect(result).toEqual({ status: 409, code: "approval_read_only" });
      }
    }
    for (const [name, viewport] of [["desktop", { width: 1440, height: 1000 }], ["mobile", { width: 390, height: 844 }]] as const) {
      await page.setViewportSize(viewport);
      await page.getByRole("button", { name: "Open action approvals" }).click();
      const dialog = page.getByRole("dialog", { name: "Action approvals" });
      await expect(dialog.getByText("Before upgrade", { exact: true })).toHaveCount(2);
      await expect(dialog.getByRole("button", { name: "Approve action" })).toHaveCount(0);
      await expect(dialog.getByRole("button", { name: "Reject action" })).toHaveCount(0);
      await upgradeFixture.assertPublic(page, [approvals.body]);
      expect(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)).toBe(false);
      await shot(page, `upgrade-approvals-${name}.png`);
      await page.keyboard.press("Escape");
    }

    await openApp(page, `${upgradeFixture.enterpriseURL}/agent/kubernetes?cluster=${encodeURIComponent(seed.cluster_id)}&view=timeline`);
    await page.getByRole("group", { name: "Timeline window" }).getByRole("button", { name: "24h", exact: true }).click();
    const timeline = await page.evaluate(async (cluster) => {
      const response = await fetch(`/api/admin/kubernetes/changes?cluster=${encodeURIComponent(cluster)}`, { credentials: "same-origin" });
      const body = await response.json();
      return { status: response.status, code: typeof body.code === "string" ? body.code : undefined, body: body as KubernetesChangesPage };
    }, seed.cluster_id);
    expect(timeline.status, `timeline error code: ${timeline.code ?? "none"}`).toBe(200);
    expect(timeline.body.items.filter((change) => change.id === seed.change_id)).toHaveLength(1);
    await expect(page.getByText("checkout:v1", { exact: false }).first()).toBeVisible();
    await upgradeFixture.assertPublic(page, [timeline.body]);
    await shot(page, "upgrade-timeline-mobile.png");
    await page.setViewportSize({ width: 1440, height: 1000 });
    await expect(page.getByText("checkout:v1", { exact: false }).first()).toBeVisible();
    await shot(page, "upgrade-timeline-desktop.png");

    const continued = await upgradeFixture.continueHistory();
    expect(continued.archive_digest === seed.archive_digest).toBe(true);
    const mergedTimeline = await page.evaluate(async (cluster) => {
      const response = await fetch(`/api/admin/kubernetes/changes?cluster=${encodeURIComponent(cluster)}`, { credentials: "same-origin" });
      const body = await response.json();
      return { status: response.status, code: typeof body.code === "string" ? body.code : undefined, body: body as KubernetesChangesPage };
    }, seed.cluster_id);
    expect(mergedTimeline.status, `timeline error code: ${mergedTimeline.code ?? "none"}`).toBe(200);
    expect(mergedTimeline.body.items.filter((change) => change.id === seed.change_id)).toHaveLength(1);
    expect(mergedTimeline.body.items.find((change) => change.id === seed.change_id)?.fields).toEqual([{ path: "images", from: "checkout:v2", to: "checkout:v3" }]);
    expect(mergedTimeline.body.items.filter((change) => change.id === "upgrade-new-change")).toHaveLength(1);
    await openApp(page, `${upgradeFixture.enterpriseURL}/agent/chat?session=${encodeURIComponent(seed.session_id)}`);
    await expect(page.getByText("Pre-upgrade investigation retained.", { exact: true })).toHaveCount(1);
    await expect(page.getByText("Continue after upgrading.", { exact: true })).toHaveCount(1);
    await page.getByLabel("Message", { exact: true }).fill("Summarize the retained investigation in one sentence without tools.");
    const message = page.waitForResponse((response) => response.url().endsWith(`/sessions/${seed.session_id}/messages`) && response.request().method() === "POST");
    await page.getByRole("button", { name: "Send", exact: true }).click();
    expect((await message).status()).toBe(200);
    await expect(page.getByRole("button", { name: "Stop", exact: true })).toHaveCount(0, { timeout: 180_000 });
    const session = await page.evaluate(async (id) => {
      const response = await fetch(`/api/admin/chat/sessions/${encodeURIComponent(id)}`, { credentials: "same-origin" });
      return { status: response.status, body: await response.json() as ChatSession };
    }, seed.session_id);
    expect(session.status).toBe(200);
    expect(session.body.status).toBe("idle");
    expect(session.body.turns.filter((turn) => turn.role === "assistant").length).toBeGreaterThan(1);
    expect(new Set(session.body.turns.map((turn) => turn.id)).size).toBe(session.body.turns.length);
    await upgradeFixture.assertPublic(page, [session.body]);
    await page.reload();
    await expect(page.getByText("Pre-upgrade investigation retained.", { exact: true })).toHaveCount(1);
    await expect(page.getByText("Continue after upgrading.", { exact: true })).toHaveCount(1);
    await shot(page, "upgrade-chat-desktop.png");
    await page.setViewportSize({ width: 390, height: 844 });
    expect(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)).toBe(false);
    await shot(page, "upgrade-chat-mobile.png");
    const finalReceipt = await upgradeFixture.verify();
    expect(finalReceipt.archive_digest === seed.archive_digest).toBe(true);
    expect(await page.locator("body").innerText()).not.toMatch(/\b(?:org_id|write_org|read_orgs)\b/);
    expect(new URL(page.url()).searchParams.has("org")).toBe(false);

    await page.getByRole("button", { name: "Open chat history" }).click();
    const row = page.getByRole("dialog", { name: "Thread history" }).locator(".group").filter({ has: page.getByRole("button", { name: /Investigate the deployment before upgrading/ }) });
    await row.getByRole("button", { name: "Delete thread" }).click();
    await expect(page.getByText("Pre-upgrade investigation retained.", { exact: true })).toHaveCount(0);
    const deleted = await page.evaluate(async (id) => {
      const response = await fetch(`/api/admin/chat/sessions/${encodeURIComponent(id)}`, { credentials: "same-origin" });
      const list = await fetch("/api/admin/chat/sessions", { credentials: "same-origin" });
      const body: { sessions: ChatSessionSummary[] } = await list.json();
      return { status: response.status, listed: body.sessions.some((session) => session.id === id) };
    }, seed.session_id);
    expect(deleted).toEqual({ status: 404, listed: false });
    await upgradeFixture.assertArchive();
  });
});


async function waitForExternalOverview(page: Page) {
  const timeline = page.getByRole("region", { name: "Break timeline", exact: true });
  await expect(timeline).toBeVisible();
  await expect.poll(() => timeline.evaluate((element) => {
    const text = element.textContent ?? "";
    return !element.querySelector('[aria-label="Loading break timeline"], .sk')
      && !text.includes("Break timeline unavailable.")
      && (element.querySelectorAll("[data-issue-row]").length > 0 || text.includes("No broken workloads"));
  }), { timeout: 45_000 }).toBe(true);
  await expect(timeline.getByRole("status", { name: "Loading break timeline", exact: true })).toHaveCount(0);
}

// Only numeric geometry leaves the browser: no text, attributes, names or IDs.
// Measure before the identity-redaction helper can alter text widths/reflow.
async function measureExternalGeometry(page: Page, surface: "preparing" | "issues") {
  return page.evaluate((kind) => {
    type Box = { x: number; y: number; width: number; height: number };
    const box = (rect: DOMRect): Box => ({ x: rect.x, y: rect.y, width: rect.width, height: rect.height });
    const inside = (inner: Box, outer: Box) => inner.x >= outer.x - 1 && inner.y >= outer.y - 1 && inner.x + inner.width <= outer.x + outer.width + 1 && inner.y + inner.height <= outer.y + outer.height + 1;
    const area = (a: Box, b: Box) => Math.max(0, Math.min(a.x + a.width, b.x + b.width) - Math.max(a.x, b.x)) * Math.max(0, Math.min(a.y + a.height, b.y + b.height) - Math.max(a.y, b.y));
    const glyphs = (element: Element): Box[] => {
      const walker = document.createTreeWalker(element, NodeFilter.SHOW_TEXT);
      const result: Box[] = [];
      while (walker.nextNode()) {
        const node = walker.currentNode;
        if (!node.textContent?.trim() || node.parentElement?.closest(".sr-only")) continue;
        const range = document.createRange();
        range.selectNodeContents(node);
        result.push(...[...range.getClientRects()].filter((rect) => rect.width > 0 && rect.height > 0).map(box));
      }
      return result;
    };
    const rows = kind === "preparing"
      ? [...document.querySelectorAll('[data-preparing-panel] [data-step="listing"]')]
      : [...document.querySelectorAll('[aria-label="Kubernetes issues"] [data-issue-row]')].slice(0, 10);
    return {
      surface: kind, width: innerWidth, preRedaction: true,
      rows: rows.map((row, index) => {
        const groups = row.querySelectorAll(":scope > div");
        const label = kind === "preparing" ? row.querySelector("[data-first-sync-step-label]") : groups[0];
        const counts = kind === "preparing" ? row.querySelector("[data-first-sync-counts]") : groups[1];
        if (!label || !counts) return { index, measured: false, label: null, counts: null, overlaps: 0, overflowedPhrases: 0, phraseCount: 0, centerDelta: 0 };
        const labelBoxes = kind === "preparing" ? glyphs(label) : [...label.querySelectorAll(".pill")].flatMap(glyphs);
        const countBoxes = glyphs(counts);
        const countPhrases = kind === "preparing" ? [...counts.querySelectorAll("[data-first-sync-count]")] : [...counts.querySelectorAll(":scope > span")];
        const bounds = box(row.getBoundingClientRect());
        const labelBounds = box(label.getBoundingClientRect());
        const countBounds = box(counts.getBoundingClientRect());
        return {
          index, measured: true, label: labelBounds, counts: countBounds,
          overlaps: labelBoxes.reduce((total, left) => total + countBoxes.filter((right) => area(left, right) > 1).length, 0),
          overflowedPhrases: countPhrases.filter((phrase) => {
            const own = box(phrase.getBoundingClientRect());
            return glyphs(phrase).some((rect) => !inside(rect, own) || !inside(rect, countBounds) || !inside(rect, bounds));
          }).length + (kind === "preparing" && labelBoxes.some((rect) => !inside(rect, labelBounds) || !inside(rect, bounds)) ? 1 : 0),
          phraseCount: countPhrases.length,
          centerDelta: Math.abs(labelBounds.y + labelBounds.height / 2 - countBounds.y - countBounds.height / 2),
        };
      }),
    };
  }, surface);
}

async function captureExternalRedacted(page: Page, capturedImages: Array<{ name: string; viewport: string; sha256: string }>, surface: string, viewportWidth: number, state: string) {
        const targetPath = capturePath(surface, String(viewportWidth), "dark", state);
        await page.evaluate(() => {
          const win = window as unknown as {
            __redactTokens?: Set<string>;
            __restoreCallbacks?: Array<() => void>;
          };
          const restore: Array<() => void> = [];
          win.__restoreCallbacks = restore;
          const UI_SAFE_WORDS = new Set([
            "all", "pod", "pods", "deployment", "deployments", "statefulset", "statefulsets",
            "daemonset", "daemonsets", "job", "jobs", "cronjob", "cronjobs", "node", "nodes",
            "service", "services", "configmap", "configmaps", "secret", "secrets", "namespace",
            "namespaces", "replicaset", "replicasets", "ingress", "event", "events", "ready",
            "running", "pending", "succeeded", "failed", "unknown", "true", "false", "normal",
            "warning", "warnings", "critical", "info", "live", "paused", "ended", "unavailable",
            "overview", "issues", "timeline", "topology", "workloads", "helm", "gitops", "traffic",
            "agent", "logs", "yaml", "related", "details", "conditions", "restarts", "uptime",
            "memory", "cpu", "metrics-server", "always", "preview", "none", "healthy", "degraded",
            "unhealthy", "fresh", "stale", "complete", "partial", "burstable", "guaranteed", "besteffort",
          ]);
          const tokens = [...(win.__redactTokens ?? [])]
            .filter((t) => t.length >= 3 && !UI_SAFE_WORDS.has(t.toLowerCase()))
            .sort((a, b) => b.length - a.length);
          const tokenMap = new Map<string, string>();
          tokens.forEach((tok, idx) => {
            tokenMap.set(tok, `redacted-${idx + 1}`);
          });

          const scrubString = (input: string): string => {
            let out = input;
            for (const [tok, replacement] of tokenMap.entries()) {
              if (out.includes(tok)) {
                out = out.split(tok).join(replacement);
              }
            }
            out = out.replace(/arn:aws[a-zA-Z0-9-]*:[a-z0-9-]+:[a-z0-9-]*:\d*:[^\s"']+/gi, "arn:aws:eks:region:000000000000:cluster/redacted");
            out = out.replace(/\b(?:\d{1,3}\.){3}\d{1,3}\b/g, "10.0.0.1");
            out = out.replace(/https?:\/\/[^\s"']+/gi, "https://redacted.example");
            // Redact any remaining namespace/resource-name pairs
            out = out.replace(/\b([a-z][a-z0-9-]*)\/([a-z0-9][a-z0-9-._]*)\b/g, (match, left) => {
              if (left === "ns-redacted" || left.startsWith("redacted-")) return `${left}/resource-redacted`;
              return "ns-redacted/resource-redacted";
            });
            // Redact multi-segment kebab-case cluster/resource identifiers (preserving metrics-server)
            out = out.replace(/\b[a-z0-9]+(?:-[a-z0-9]+){2,}\b/g, (match) => {
              if (match === "metrics-server" || match.startsWith("redacted-") || match.startsWith("pod-redacted-") || match.startsWith("ns-redacted") || match.startsWith("resource-redacted")) return match;
              return "id-redacted";
            });
            out = out.replace(/\b[a-z0-9]+-(?:secret|svc|env|config|db|redis|postgres|api|web|app|worker|node|pod|deploy|sts|cron|job|test|realtime|service|server)\b/gi, (match) => {
              if (match.toLowerCase() === "metrics-server" || match.startsWith("redacted-") || match.startsWith("resource-redacted")) return match;
              return "id-redacted";
            });
            return out;
          };

          // Scrub log lines inside <pre> and break log viewers
          for (const pre of document.querySelectorAll<HTMLElement>(
            'pre[aria-label="Workload log output"], pre[aria-label="Pod log output"]',
          )) {
            const spans = pre.querySelectorAll<HTMLElement>("[data-log-key]");
            spans.forEach((span, idx) => {
              const buttons = span.querySelectorAll<HTMLElement>("button");
              buttons.forEach((btn) => {
                const origBtn = btn.textContent ?? "";
                restore.push(() => { btn.textContent = origBtn; });
                const hasContainer = origBtn.includes("/");
                btn.textContent = `pod-redacted-${(idx % 2) + 1}${hasContainer ? "/container-1" : ""}`;
              });
              for (const node of Array.from(span.childNodes)) {
                if (node.nodeType === Node.TEXT_NODE && (node.textContent ?? "").trim().length > 0) {
                  const origText = node.textContent ?? "";
                  restore.push(() => { node.textContent = origText; });
                  node.textContent = `  [scrubbed telemetry log line #${idx + 1}]\n`;
                }
              }
              for (const mark of span.querySelectorAll<HTMLElement>("mark")) {
                const origMark = mark.textContent ?? "";
                restore.push(() => { mark.textContent = origMark; });
                mark.textContent = "match";
              }
            });
          }

          for (const breakLogContainer of document.querySelectorAll<HTMLElement>('div[aria-label="Break log lines"]')) {
            const rows = Array.from(breakLogContainer.children) as HTMLElement[];
            rows.forEach((row, idx) => {
              const spans = row.querySelectorAll<HTMLElement>("span");
              if (spans.length >= 4) {
                const msgSpan = spans[spans.length - 1];
                const origMsg = msgSpan.textContent ?? "";
                restore.push(() => { msgSpan.textContent = origMsg; });
                msgSpan.textContent = `[scrubbed break log line #${idx + 1}]`;
                const podSpan = spans[1];
                const origPod = podSpan.textContent ?? "";
                restore.push(() => { podSpan.textContent = origPod; });
                podSpan.textContent = `p${(idx % 2) + 1}`;
              }
            });
          }

          // Scrub Top messages in window patterns
          for (const patSpan of document.querySelectorAll<HTMLElement>('#kubernetes-break-logs aside ul li span.break-words')) {
            const origPat = patSpan.textContent ?? "";
            restore.push(() => { patSpan.textContent = origPat; });
            patSpan.textContent = "[scrubbed log message pattern]";
          }

          // Scrub YAML values
          for (const code of document.querySelectorAll<HTMLElement>('section[aria-label="Projected YAML"] code')) {
            const spans = code.querySelectorAll<HTMLElement>("span");
            if (spans.length >= 2) {
              const valSpan = spans[1];
              const origVal = valSpan.textContent ?? "";
              if (origVal.trim().length > 0 && !/^(true|false|\d+|\[\]|\{\})$/i.test(origVal.trim())) {
                restore.push(() => { valSpan.textContent = origVal; });
                valSpan.textContent = " redacted-value";
              }
            } else {
              const origCode = code.textContent ?? "";
              if (origCode.trim().startsWith("- ")) {
                restore.push(() => { code.textContent = origCode; });
                code.textContent = "  - redacted-item";
              }
            }
          }

          // Scrub Timeline chart resource labels directly so even 0/1-hyphen names are always redacted
          const timelineLabels = Array.from(document.querySelectorAll<HTMLElement>(".resource-timeline-row:not(.break-log-rate) .resource-timeline-label"));
          timelineLabels.forEach((lbl, idx) => {
            const strong = lbl.querySelector<HTMLElement>("strong");
            const span = lbl.querySelector<HTMLElement>("span");
            if (strong) {
              const origStrong = strong.textContent ?? "";
              const kindPrefix = origStrong.split(" ")[0] || "Resource";
              restore.push(() => { strong.textContent = origStrong; });
              strong.textContent = `${kindPrefix} workload-redacted-${idx + 1}`;
            }
            if (span) {
              const origSpan = span.textContent ?? "";
              restore.push(() => { span.textContent = origSpan; });
              span.textContent = `ns-redacted-${(idx % 3) + 1}`;
            }
          });

          // Scrub Workloads list rows directly
          const workloadRows = Array.from(document.querySelectorAll<HTMLElement>(`[aria-label="Workloads"] [role="listitem"] button`));
          workloadRows.forEach((btn, idx) => {
            const spans = btn.querySelectorAll<HTMLElement>("span");
            if (spans.length >= 2) {
              const origName = spans[0].textContent ?? "";
              const origNs = spans[1].textContent ?? "";
              restore.push(() => { spans[0].textContent = origName; spans[1].textContent = origNs; });
              spans[0].textContent = `workload-redacted-${idx + 1}`;
              spans[1].textContent = `ns-redacted-${(idx % 4) + 1}`;
            }
          });

          // Scrub Details panel fields and Related resources names directly
          for (const dd of document.querySelectorAll<HTMLElement>('div[role="dialog"][aria-label="Details panel"] dd')) {
            const origDd = dd.textContent ?? "";
            const scrubbed = scrubString(origDd);
            if (scrubbed !== origDd) {
              restore.push(() => { dd.textContent = origDd; });
              dd.textContent = scrubbed;
            }
          }

          // Scrub inputs and selects
          for (const input of document.querySelectorAll<HTMLInputElement>("input")) {
            if (input.value) {
              const origVal = input.value;
              restore.push(() => { input.value = origVal; });
              input.value = "redacted-filter";
            }
          }
          for (const option of document.querySelectorAll<HTMLOptionElement>("select option")) {
            const origOpt = option.textContent ?? "";
            const scrubbed = scrubString(origOpt);
            if (scrubbed !== origOpt) {
              restore.push(() => { option.textContent = origOpt; });
              option.textContent = scrubbed;
            }
          }

          // Scrub all remaining text nodes
          const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
          let current: Node | null = walker.nextNode();
          while (current) {
            const textNode = current;
            const orig = textNode.textContent ?? "";
            if (orig.trim().length > 0) {
              const scrubbed = scrubString(orig);
              if (scrubbed !== orig) {
                restore.push(() => { textNode.textContent = orig; });
                textNode.textContent = scrubbed;
              }
            }
            current = walker.nextNode();
          }
        });

        try {
          const buf = await page.screenshot({ path: targetPath, animations: "disabled" });
          capturedImages.push({
            name: path.basename(targetPath),
            viewport: String(viewportWidth),
            sha256: createHash("sha256").update(buf).digest("hex"),
          });
        } finally {
          await page.evaluate(() => {
            const win = window as unknown as { __restoreCallbacks?: Array<() => void> };
            for (const fn of (win.__restoreCallbacks ?? []).reverse()) {
              try { fn(); } catch { /* ignore */ }
            }
            win.__restoreCallbacks = [];
          });
        }
}

test.describe("Read-only external cluster acceptance", () => {
  test.skip(
    process.env.E2E_KUBERNETES_READONLY_ACCEPTANCE !== "1",
    "requires an authorized read-only external cluster viewer",
  );
  test.setTimeout(300_000);

  test("verifies read-only resource detail drawer live logs break logs and agent settings without cluster mutations", async ({ browser }, testInfo) => {
    const context = await browser.newContext({
      baseURL: env.baseURL,
      viewport: { width: 1440, height: 900 },
      colorScheme: "dark",
    });
    const page = await context.newPage();
    try {
      const pageErrors: string[] = [];
      const mutatingRequests: Array<{ method: string; pathname: string }> = [];
      const streamLifecycle = {
        started: 0,
        completed: 0,
        failedOrAborted: 0,
        active: 0,
        workloadStreams: 0,
        podStreams: 0,
      };
      const activeStreamSet = new Set<object>();

      page.on("pageerror", (error) => {
        pageErrors.push(error.name || "PageError");
      });

      page.on("request", (request) => {
        const url = new URL(request.url());
        const method = request.method().toUpperCase();
        if (
          (url.pathname.startsWith("/api/") || url.pathname.startsWith("/enterprise/api/")) &&
          !["GET", "HEAD", "OPTIONS"].includes(method)
        ) {
          const isInitialSignIn =
            method === "POST" &&
            (url.pathname === "/api/auth/gateway-session" ||
              url.pathname === "/api/admin/auth/session" ||
              url.pathname === "/api/v1/auth/session" ||
              url.pathname.endsWith("/auth/login") ||
              url.pathname.endsWith("/session"));
          if (!isInitialSignIn) {
            mutatingRequests.push({ method, pathname: url.pathname });
          }
        }
        if (url.pathname.includes("/logs/stream")) {
          streamLifecycle.started += 1;
          if (url.pathname.includes("/workloads/")) streamLifecycle.workloadStreams += 1;
          if (url.pathname.includes("/pods/")) streamLifecycle.podStreams += 1;
          activeStreamSet.add(request);
          streamLifecycle.active = activeStreamSet.size;
        }
      });

      page.on("requestfinished", (request) => {
        if (activeStreamSet.delete(request)) {
          streamLifecycle.completed += 1;
          streamLifecycle.active = activeStreamSet.size;
        }
      });

      page.on("requestfailed", (request) => {
        if (activeStreamSet.delete(request)) {
          streamLifecycle.failedOrAborted += 1;
          streamLifecycle.active = activeStreamSet.size;
        }
      });

      const capturedImages: Array<{ name: string; viewport: string; sha256: string }> = [];

      const captureRedacted = (surface: string, viewportWidth: number, state: string) =>
        captureExternalRedacted(page, capturedImages, surface, viewportWidth, state);

      await page.addInitScript(() => localStorage.setItem("versus.theme", "dark"));

      // 1. Open Kubernetes Explorer and authenticate via gateway secret
      await openApp(page, "/agent/kubernetes");
      await expect(page.getByRole("heading", { name: "Kubernetes", exact: true })).toBeVisible();

      const viewTabs = page.getByRole("tablist", { name: "Kubernetes views", exact: true });
      await expect(viewTabs).toBeVisible();
      const viewTabLabels = await viewTabs.getByRole("tab").allTextContents();
      expect(viewTabLabels.map((t) => t.trim())).toEqual([
        "Overview",
        "Workloads",
        "Issues",
        "Timeline",
        "Topology",
        "Nodes",
        "Helm",
        "GitOps",
        "Traffic",
        "Agent",
      ]);

      // Probe overview, inventory, issues, changes, crashes, and active log emitters
      // inside page context without returning any raw names or identifiers to Node/logs.
      const clusterProbe = await page.evaluate(async () => {
        const safeFetch = async (url: string) => {
          const res = await fetch(url, { credentials: "same-origin" });
          let json: Record<string, unknown> | null = null;
          try {
            json = (await res.json()) as Record<string, unknown>;
          } catch {
            json = null;
          }
          return { status: res.status, json };
        };

        const win = window as unknown as {
          __redactTokens?: Set<string>;
          __discoveredTargets?: Record<string, { resource_id: string; kind: string; namespace: string; name: string; uid: string; podCount: number }>;
          __brokenIssueTarget?: { root_uid: string; namespace: string; kind: string } | null;
        };
        if (!win.__redactTokens) win.__redactTokens = new Set<string>();
        const addToken = (val: unknown) => {
          if (typeof val === "string" && val.trim().length >= 3) {
            win.__redactTokens!.add(val.trim());
          }
        };

        const ov = await safeFetch("/api/admin/kubernetes/overview");
        const ovBody = ov.json ?? {};
        addToken(ovBody.cluster_id);
        const partialFailures = Array.isArray(ovBody.partial_failures)
          ? (ovBody.partial_failures as Array<Record<string, unknown>>).map((f) => ({
              resource_id: String(f.resource_id ?? ""),
              class: String(f.class ?? "unknown"),
            }))
          : [];

        // Pre-populate redaction tokens from namespaces, nodes, issues, 24h changes, 24h crashes, top
        const since24h = encodeURIComponent(new Date(Date.now() - 24 * 3600_000).toISOString());
        const untilNow = encodeURIComponent(new Date().toISOString());
        const [nsRes, nodesRes, issAllRes, issBrokenRes, chgRes, crshRes, topPodsRes] = await Promise.all([
          safeFetch("/api/admin/kubernetes/resources?resource_id=core~v1~namespaces&limit=100"),
          safeFetch("/api/admin/kubernetes/resources?resource_id=core~v1~nodes&limit=100"),
          safeFetch("/api/admin/kubernetes/issues?limit=100"),
          safeFetch("/api/admin/kubernetes/issues?broken=true&limit=100"),
          safeFetch(`/api/admin/kubernetes/changes?since=${since24h}&until=${untilNow}&limit=200`),
          safeFetch(`/api/admin/kubernetes/crashes?since=${since24h}&until=${untilNow}&limit=200`),
          safeFetch("/api/admin/kubernetes/top?kind=Pod&sort=cpu&limit=30"),
        ]);

        if (Array.isArray(nsRes.json?.items)) {
          for (const item of nsRes.json.items as Array<Record<string, unknown>>) addToken(item.name);
        }
        if (Array.isArray(nodesRes.json?.items)) {
          for (const item of nodesRes.json.items as Array<Record<string, unknown>>) addToken(item.name);
        }
        for (const list of [issAllRes.json?.items, issBrokenRes.json?.items]) {
          if (Array.isArray(list)) {
            for (const item of list as Array<Record<string, unknown>>) {
              const root = (item.root as Record<string, unknown> | undefined) ?? {};
              addToken(root.name);
              addToken(root.namespace);
              addToken(root.uid);
            }
          }
        }
        if (Array.isArray(chgRes.json?.items)) {
          for (const item of chgRes.json.items as Array<Record<string, unknown>>) {
            addToken(item.name);
            addToken(item.namespace);
            addToken(item.uid);
          }
        }
        if (Array.isArray(crshRes.json?.items)) {
          for (const item of crshRes.json.items as Array<Record<string, unknown>>) {
            addToken(item.pod);
            addToken(item.namespace);
            addToken(item.container);
            addToken(item.node);
            addToken(item.root_uid);
          }
        }
        const topPods = Array.isArray(topPodsRes.json?.items)
          ? (topPodsRes.json!.items as Array<Record<string, unknown>>)
          : [];
        for (const item of topPods) {
          addToken(item.name);
          addToken(item.namespace);
          const owner = (item.owner as Record<string, unknown> | undefined) ?? {};
          addToken(owner.name);
        }

        // Identify a broken issue with valid root_uid, namespace, and broke_at for Timeline break-logs testing
        const brokenItems = Array.isArray(issBrokenRes.json?.items)
          ? (issBrokenRes.json!.items as Array<Record<string, unknown>>)
          : [];
        win.__brokenIssueTarget = null;
        const preferredBroken = brokenItems.find((item) => {
          const root = (item.root as Record<string, unknown> | undefined) ?? {};
          return Boolean(
            root.uid &&
              root.namespace &&
              typeof item.broke_at === "string" &&
              ["Deployment", "StatefulSet", "Pod", "DaemonSet", "Job", "CronJob"].includes(String(root.kind ?? "")),
          );
        }) ?? brokenItems.find((item) => {
          const root = (item.root as Record<string, unknown> | undefined) ?? {};
          return Boolean(root.uid && root.namespace && typeof item.broke_at === "string");
        });
        if (preferredBroken) {
          const root = (preferredBroken.root as Record<string, unknown>) ?? {};
          win.__brokenIssueTarget = {
            root_uid: String(root.uid),
            namespace: String(root.namespace),
            kind: String(root.kind ?? ""),
          };
        }

        const wlAll = await safeFetch("/api/admin/kubernetes/workloads?limit=100");
        const wlAllBody = wlAll.json ?? {};
        const counts = (wlAllBody.counts as Record<string, number> | undefined) ?? {};

        const classifyInventory = (status: number, items: unknown[], failures: Array<Record<string, unknown>>) => {
          if (status === 401 || status === 403 || failures.some((f) => f.class === "forbidden")) return "forbidden";
          if (status !== 200) return `error_${status}`;
          return items.length > 0 ? "populated" : "empty";
        };

        const kindSummary: Record<string, { status: number; classification: string; count: number; multiPodCount: number }> = {};
        win.__discoveredTargets = {};

        for (const kind of ["Deployment", "StatefulSet", "Pod", "DaemonSet", "Job", "CronJob"]) {
          const res = await safeFetch(`/api/admin/kubernetes/workloads?kind=${encodeURIComponent(kind)}&limit=60`);
          const items = Array.isArray(res.json?.items) ? (res.json!.items as Array<Record<string, unknown>>) : [];
          const failures = Array.isArray(res.json?.partial_failures)
            ? (res.json!.partial_failures as Array<Record<string, unknown>>)
            : [];
          for (const item of items) {
            addToken(item.name);
            addToken(item.namespace);
            addToken(item.uid);
            const summary = (item.summary as Record<string, unknown> | undefined) ?? {};
            addToken(summary.node);
            addToken(summary.pod_ip);
            addToken(summary.image);
          }

          let multiPodCount = 0;
          if ((kind === "Deployment" || kind === "StatefulSet") && items.length > 0) {
            const sorted = [...items].sort((a, b) => {
              const sa = (a.summary as Record<string, unknown> | undefined) ?? {};
              const sb = (b.summary as Record<string, unknown> | undefined) ?? {};
              const ra = Number(sa.ready_replicas ?? sa.readyReplicas ?? sa.replicas ?? 0);
              const rb = Number(sb.ready_replicas ?? sb.readyReplicas ?? sb.replicas ?? 0);
              return rb - ra;
            });

            let bestScore = -1;
            let chosen: { resource_id: string; kind: string; namespace: string; name: string; uid: string; podCount: number } | null = null;
            for (const candidate of sorted.slice(0, 15)) {
              const ns = String(candidate.namespace ?? "");
              const name = String(candidate.name ?? "");
              const wDetail = await safeFetch(
                `/api/admin/kubernetes/workloads/${encodeURIComponent(kind)}/${encodeURIComponent(name)}?namespace=${encodeURIComponent(ns)}`,
              );
              const pods = Array.isArray(wDetail.json?.pods) ? (wDetail.json!.pods as Array<Record<string, unknown>>) : [];
              for (const p of pods) {
                addToken(p.name);
                addToken(p.uid);
                addToken(p.node);
              }
              const containers = Array.isArray(wDetail.json?.containers)
                ? (wDetail.json!.containers as Array<Record<string, unknown>>)
                : [];
              for (const c of containers) {
                addToken(c.name);
                addToken(c.image);
              }
              if (pods.length >= 2) multiPodCount += 1;
              let recentLines = 0;
              if (pods.length >= 1) {
                const logProbe = await safeFetch(
                  `/api/admin/kubernetes/workloads/${encodeURIComponent(kind)}/${encodeURIComponent(name)}/logs?namespace=${encodeURIComponent(ns)}&since_seconds=3600&tail_lines=30`,
                );
                const lines = Array.isArray(logProbe.json?.lines) ? logProbe.json!.lines : [];
                recentLines = lines.length;
              }
              const score = (pods.length >= 2 ? 1000 : 0) + (recentLines > 0 ? 500 + recentLines : 0) + pods.length;
              if (score > bestScore) {
                bestScore = score;
                chosen = {
                  resource_id: String(candidate.resource_id ?? (kind === "Deployment" ? "apps~v1~deployments" : "apps~v1~statefulsets")),
                  kind,
                  namespace: ns,
                  name,
                  uid: String(candidate.uid ?? ""),
                  podCount: pods.length,
                };
              }
            }
            if (chosen) win.__discoveredTargets[kind] = chosen;
          } else if (kind === "Pod" && items.length > 0) {
            const running = items.filter((item) => {
              const summary = (item.summary as Record<string, unknown> | undefined) ?? {};
              return summary.phase === "Running";
            });
            const pick = running[0] ?? items[0];
            win.__discoveredTargets.Pod = {
              resource_id: String(pick.resource_id ?? "core~v1~pods"),
              kind: "Pod",
              namespace: String(pick.namespace ?? ""),
              name: String(pick.name ?? ""),
              uid: String(pick.uid ?? ""),
              podCount: 1,
            };
          }

          kindSummary[kind] = {
            status: res.status,
            classification: classifyInventory(res.status, items, failures),
            count: counts[kind] ?? items.length,
            multiPodCount,
          };
        }

        // Also probe top CPU pods to find the freshest active log emitter in the cluster
        for (const tp of topPods.slice(0, 15)) {
          const ns = String(tp.namespace ?? "");
          const name = String(tp.name ?? "");
          if (!ns || !name) continue;
          const pLog = await safeFetch(
            `/api/admin/kubernetes/pods/${encodeURIComponent(ns)}/${encodeURIComponent(name)}/logs?since_seconds=60&tail_lines=20`,
          );
          const lines = Array.isArray(pLog.json?.lines) ? pLog.json!.lines : [];
          if (lines.length > 0) {
            const pRes = await safeFetch(
              `/api/admin/kubernetes/resources/core~v1~pods/${encodeURIComponent(name)}?namespace=${encodeURIComponent(ns)}`,
            );
            const uid = String(pRes.json?.uid ?? "");
            if (uid) {
              win.__discoveredTargets.ActivePod = {
                resource_id: "core~v1~pods",
                kind: "Pod",
                namespace: ns,
                name,
                uid,
                podCount: 1,
              };
              break;
            }
          }
        }

        return {
          overview: {
            status: ov.status,
            classification: ov.status === 200 ? "ok" : ov.status === 401 || ov.status === 403 ? "forbidden" : `error_${ov.status}`,
            clusterIdPresent: Boolean(ovBody.cluster_id),
            nodes: Number(ovBody.nodes ?? 0),
            readyNodes: Number(ovBody.ready_nodes ?? 0),
            namespaces: Number(ovBody.namespaces ?? 0),
            workloads: Number(ovBody.workloads ?? 0),
            pods: Number(ovBody.pods ?? 0),
            metricsStatus: String(ovBody.metrics_status ?? "unavailable"),
            usageSource: String(ovBody.usage_source ?? "unavailable"),
            partialFailures,
            omittedCategories: Array.isArray(ovBody.omitted_categories) ? (ovBody.omitted_categories as string[]) : [],
          },
          workloadsInventory: {
            status: wlAll.status,
            counts,
            kinds: kindSummary,
            discoveredKinds: Object.keys(win.__discoveredTargets).map((k) => ({
              kind: k,
              podCount: win.__discoveredTargets![k].podCount,
            })),
          },
        };
      });

      expect(clusterProbe.overview.status).toBe(200);
      await expect(page.getByRole("region", { name: "Cluster health", exact: true })).toBeVisible();
      await waitForExternalOverview(page);
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
      await captureRedacted("eks-overview", 1440, "populated");

      // 2. Switch to Workloads tab and verify inventory UI
      await page.getByRole("tab", { name: "Workloads", exact: true }).click();
      const workloadsRegion = page.getByRole("region", { name: "Workloads", exact: true });
      await expect(workloadsRegion).toBeVisible();
      await expect(page.getByRole("tablist", { name: "Workload kind", exact: true })).toBeVisible();
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
      await captureRedacted("eks-workloads", 1440, "populated");

      const openTargetInDrawer = async (targetKind: string) => {
        await page.getByRole("tab", { name: "Workloads", exact: true }).click();
        await expect(workloadsRegion).toBeVisible();
        const hasTarget = await page.evaluate((k) => {
          const win = window as unknown as {
            __discoveredTargets?: Record<string, { resource_id: string; kind: string; namespace: string; name: string; uid: string; podCount: number }>;
          };
          return Boolean(win.__discoveredTargets?.[k]);
        }, targetKind);
        if (!hasTarget) return false;

        const uiKind = targetKind === "ActivePod" ? "Pod" : targetKind;
        const viewportSize = page.viewportSize();
        if ((viewportSize?.width ?? 1440) >= 640) {
          await page
            .getByRole("tablist", { name: "Workload kind", exact: true })
            .getByRole("tab", { name: new RegExp(`^${uiKind}`), exact: false })
            .click();
        } else {
          await page.locator(`select[aria-label="Workload kind"]`).selectOption(uiKind);
        }

        await page.evaluate((k) => {
          const win = window as unknown as {
            __discoveredTargets?: Record<string, { resource_id: string; kind: string; namespace: string; name: string; uid: string; podCount: number }>;
          };
          const target = win.__discoveredTargets![k];
          const setReactInput = (el: HTMLInputElement | null, value: string) => {
            if (!el) return;
            const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")?.set;
            setter?.call(el, value);
            el.dispatchEvent(new Event("input", { bubbles: true }));
            el.dispatchEvent(new Event("change", { bubbles: true }));
          };
          setReactInput(document.querySelector<HTMLInputElement>(`input[aria-label="Workload namespace"]`), target.namespace);
          setReactInput(document.querySelector<HTMLInputElement>(`input[aria-label="Resource name"]`), target.name);
        }, targetKind);

        const firstRowButton = workloadsRegion.getByRole("listitem").first().locator(`button[aria-label^="Select "]`);
        await expect(firstRowButton).toBeVisible({ timeout: 30_000 });
        await firstRowButton.click();
        const detail = page.getByRole("dialog", { name: "Details panel" });
        await expect(detail).toBeVisible({ timeout: 30_000 });
        return true;
      };

      // 3. Test deep-link normalization for legacy tabs (metrics, diagnosis, actions -> overview)
      const deepLinkNormalized = await page.evaluate(async () => {
        const win = window as unknown as {
          __discoveredTargets?: Record<string, { resource_id: string; kind: string; namespace: string; name: string; uid: string; podCount: number }>;
        };
        const target = win.__discoveredTargets?.Deployment ?? win.__discoveredTargets?.Pod ?? win.__discoveredTargets?.StatefulSet;
        if (!target) return { tested: false, results: {} };
        const rParam = `${target.resource_id}/${target.namespace}/${target.name}`;
        const results: Record<string, boolean> = {};
        for (const legacyTab of ["metrics", "diagnosis", "actions"]) {
          window.history.pushState(null, "", `/agent/kubernetes?r=${encodeURIComponent(rParam)}&tab=${legacyTab}`);
          window.dispatchEvent(new PopStateEvent("popstate"));
          await new Promise((r) => setTimeout(r, 150));
          const params = new URLSearchParams(window.location.search);
          const overviewTab = document.getElementById("kubernetes-drawer-tab-overview");
          results[legacyTab] = !params.has("tab") && overviewTab?.getAttribute("aria-selected") === "true";
        }
        return { tested: true, results };
      });
      if (deepLinkNormalized.tested) {
        expect(deepLinkNormalized.results).toEqual({ metrics: true, diagnosis: true, actions: true });
        const closeBtn = page.getByRole("dialog", { name: "Details panel" }).getByRole("button", { name: "Close panel", exact: true });
        if (await closeBtn.count()) await closeBtn.click();
        await expect(page.getByRole("dialog", { name: "Details panel" })).toHaveCount(0);
        await page.waitForTimeout(2_200);
      }

      // 4. Exercise Resource Detail Drawer across present kinds (Deployment, StatefulSet, Pod)
      const drawerVerification: Record<string, unknown> = {};

      for (const kind of ["Deployment", "StatefulSet", "Pod"]) {
        const opened = await openTargetInDrawer(kind);
        if (!opened) {
          drawerVerification[kind] = {
            exercised: false,
            reason: clusterProbe.workloadsInventory.kinds[kind]?.classification ?? "absent",
          };
          continue;
        }

        const detail = page.getByRole("dialog", { name: "Details panel" });
        const drawerTablist = detail.getByRole("tablist", { name: "Resource detail views", exact: true });
        await expect(drawerTablist).toBeVisible();
        const tabLabels = (await drawerTablist.getByRole("tab").allTextContents()).map((t) => t.trim());
        expect(tabLabels).toEqual(["Overview", "Logs", "Events", "YAML", "Related", "Timeline"]);
        await expect(drawerTablist.getByRole("tab", { name: /Actions|Metrics|Diagnosis/i })).toHaveCount(0);

        await expect(detail.getByRole("region", { name: "Live workload metrics", exact: true })).toBeVisible({ timeout: 30_000 });

        // Keyboard navigation check on drawer tabs
        const overviewTabBtn = drawerTablist.getByRole("tab", { name: "Overview", exact: true });
        const logsTabBtn = drawerTablist.getByRole("tab", { name: "Logs", exact: true });
        await overviewTabBtn.focus();
        await expect(overviewTabBtn).toBeFocused();
        await page.keyboard.press("ArrowRight");
        await expect(logsTabBtn).toHaveAttribute("aria-selected", "true");
        await expect(logsTabBtn).toBeFocused();
        await page.keyboard.press("ArrowLeft");
        await expect(overviewTabBtn).toHaveAttribute("aria-selected", "true");
        await expect(overviewTabBtn).toBeFocused();

        // Overview tab checks
        const overviewCheck = await detail.evaluate((dialogEl) => {
          const text = dialogEl.textContent ?? "";
          const cpuTile = Boolean(dialogEl.querySelector(`[aria-label="CPU metric tile"]`));
          const memTile = Boolean(dialogEl.querySelector(`[aria-label="Memory metric tile"]`));
          const crashTicks = Boolean(dialogEl.querySelector(`[aria-label="24 hour crash ticks"]`));
          const detailsHeading = Array.from(dialogEl.querySelectorAll("h3")).some((h) => h.textContent?.trim() === "Details");
          const conditionsHeading = Array.from(dialogEl.querySelectorAll("h3")).some((h) => h.textContent?.trim() === "Conditions");
          const compactLogs = Boolean(
            dialogEl.querySelector(`section[aria-label="Workload live logs"], section[aria-label="Pod logs"]`),
          );
          const investigateBtn = Boolean(dialogEl.querySelector(`button[aria-label="Investigate"]`));
          return {
            cpuTile,
            memTile,
            crashTicks,
            detailsHeading,
            conditionsHeading,
            compactLogs,
            investigateBtn,
            hasReadyPill: /Ready\s+\d+\/\d+|Ready\s+\?/.test(text),
            hasRestartsPill: /\d+\s+restarts/.test(text),
          };
        });
        expect(overviewCheck.cpuTile).toBe(true);
        expect(overviewCheck.memTile).toBe(true);
        expect(overviewCheck.detailsHeading).toBe(true);
        expect(overviewCheck.conditionsHeading).toBe(true);
        expect(overviewCheck.compactLogs).toBe(true);

        if (kind === "Deployment") {
          await page.waitForTimeout(1_000);
          await captureRedacted("eks-drawer-overview", 1440, "populated");
        }

        // Switch from Overview compact live logs to full Logs tab via "Open full logs ->"
        const openFullLogsBtn = detail.getByRole("button", { name: /Open full logs/ });
        await expect(openFullLogsBtn).toBeVisible();
        await openFullLogsBtn.click();
        await expect(drawerTablist.getByRole("tab", { name: "Logs", exact: true })).toHaveAttribute("aria-selected", "true");

        const expandBtn = detail.getByRole("button", { name: "Expand panel", exact: true });
        if (await expandBtn.isVisible()) {
          await expandBtn.click();
        }

        const isWorkload = kind === "Deployment" || kind === "StatefulSet";
        const logsRegion = detail.getByRole("region", { name: isWorkload ? "Workload live logs" : "Pod logs", exact: true });
        await expect(logsRegion).toBeVisible();
        const logOutput = logsRegion.getByLabel(isWorkload ? "Workload log output" : "Pod log output", { exact: true });
        await expect(logOutput).toBeVisible();

        const waitForLogsSettled = async () => {
          await page.waitForTimeout(350);
          await expect
            .poll(
              async () => {
                const txt = (await logOutput.textContent()) ?? "";
                return !txt.includes("Connecting to logs...");
              },
              { timeout: 25_000, intervals: [500, 1000] },
            )
            .toBe(true);
        };

        await waitForLogsSettled();

        // Since chooses a finite window; only the explicit Load starts its
        // read. Keep that behavior assertion even when the live tail is quiet.
        const sinceWindow = logsRegion.getByRole("combobox", { name: "Since window", exact: true });
        const loadEarlier = logsRegion.getByRole("button", { name: "Load logs since that window", exact: true });
        await expect(sinceWindow).toBeVisible();
        await expect(loadEarlier).toBeVisible();
        await sinceWindow.selectOption("86400");
        await expect(sinceWindow).toHaveValue("86400");
        await page.waitForTimeout(2_200);
        const earlierRead = page.waitForResponse((response) => {
          const url = new URL(response.url());
          return url.pathname.includes("/kubernetes/") && url.pathname.endsWith("/logs") && url.searchParams.get("since_seconds") === "86400";
        });
        await loadEarlier.click();
        expect((await earlierRead).status()).toBe(200);
        await expect(loadEarlier).toBeEnabled();
        await waitForLogsSettled();

        // Verify no yellow warning banner inside log viewer (excluding per-Pod prefix buttons inside pre)
        const warningBannersInLogs = await logsRegion
          .locator(`p.text-sev-warning, div.text-sev-warning, [role="alert"], [role="status"].text-sev-warning`)
          .count();
        expect(warningBannersInLogs).toBe(0);

        const initialLogMetrics = await logsRegion.evaluate((sectionEl, wl) => {
          const pre = sectionEl.querySelector("pre");
          const lineSpans = pre ? Array.from(pre.querySelectorAll<HTMLElement>("[data-log-key]")) : [];
          const neutralSpans = lineSpans.filter((s) => s.classList.contains("text-ink-400"));
          const prefixBtns = pre ? Array.from(pre.querySelectorAll<HTMLButtonElement>("button")) : [];
          const distinctPrefixes = new Set(prefixBtns.map((b) => (b.textContent ?? "").trim()).filter(Boolean));
          const podSelect = sectionEl.querySelector<HTMLSelectElement>(`select[aria-label="Log pod"]`);
          const containerSelect = sectionEl.querySelector<HTMLSelectElement>(`select[aria-label="Log container"]`);
          const podOptions = podSelect ? Array.from(podSelect.options).map((o) => ({ value: o.value, isAll: o.value === "" })) : [];
          const containerOptions = containerSelect ? Array.from(containerSelect.options).map((o) => o.value) : [];
          const win = window as unknown as { __redactTokens?: Set<string> };
          if (podSelect) {
            for (const opt of Array.from(podSelect.options)) {
              if (opt.value) win.__redactTokens?.add(opt.textContent?.trim() ?? "");
            }
          }
          for (const p of distinctPrefixes) win.__redactTokens?.add(p);
          return {
            workloadMode: wl,
            allPodsDefault: podSelect ? podSelect.value === "" : null,
            allPodsOptionLabel: podSelect?.options[0]?.textContent?.trim() ?? null,
            podOptionCount: podOptions.filter((o) => !o.isAll).length,
            containerOptionCount: containerOptions.length,
            hasAllContainersOption: containerOptions.includes("*"),
            retainedLineSpans: lineSpans.length,
            nonNeutralLineSpans: lineSpans.length - neutralSpans.length,
            neutralLineSpans: neutralSpans.length,
            distinctPodPrefixesInStream: distinctPrefixes.size,
            lastLineKey: lineSpans.at(-1)?.dataset.logKey ?? null,
            emptyOrStatusText: lineSpans.length === 0 ? (pre?.textContent?.trim() ?? "") : null,
          };
        }, isWorkload);

        if (kind === "Deployment") {
          await captureRedacted("eks-drawer-logs", 1440, "populated");
        }

        // Observe for arriving new lines without clicking Reload/Refresh (poll up to 15s)
        let arrivedNewLines = false;
        let afterLineSpans = initialLogMetrics.retainedLineSpans;
        if (initialLogMetrics.retainedLineSpans > 0) {
          const beforeCount = initialLogMetrics.retainedLineSpans;
          const beforeKey = initialLogMetrics.lastLineKey;
          try {
            await expect
              .poll(
                async () => {
                  const state = await logsRegion.evaluate((sectionEl) => {
                    const pre = sectionEl.querySelector("pre");
                    const spans = pre ? Array.from(pre.querySelectorAll<HTMLElement>("[data-log-key]")) : [];
                    return {
                      count: spans.length,
                      lastKey: spans.at(-1)?.dataset.logKey ?? null,
                    };
                  });
                  afterLineSpans = state.count;
                  return state.count > beforeCount || (state.lastKey !== null && state.lastKey !== beforeKey);
                },
                { timeout: 30_000, intervals: [1_000] },
              )
              .toBe(true);
            arrivedNewLines = true;
          } catch {
            arrivedNewLines = false;
          }
        }

        // Test Pod selection (dropdown & prefix button) for Deployment / StatefulSet
        let podSelectionCheck: Record<string, unknown> = { applicable: isWorkload };
        if (isWorkload && initialLogMetrics.podOptionCount >= 1) {
          const podSelect = logsRegion.getByLabel("Log pod", { exact: true });
          await expect(podSelect).toHaveValue("");
          await page.waitForTimeout(2_200);
          if (initialLogMetrics.distinctPodPrefixesInStream >= 1) {
            await logOutput.locator("button").last().click();
            const selectedVal = await podSelect.inputValue();
            expect(selectedVal).not.toBe("");
            let finalUniqueCount = 0;
            await expect
              .poll(
                async () => {
                  const prefixesMatch = await logsRegion.evaluate((sectionEl) => {
                    const pre = sectionEl.querySelector("pre");
                    const btns = pre ? Array.from(pre.querySelectorAll<HTMLButtonElement>("button")) : [];
                    const unique = new Set(btns.map((b) => (b.textContent ?? "").trim()));
                    return { buttonCount: btns.length, uniqueCount: unique.size };
                  });
                  finalUniqueCount = prefixesMatch.uniqueCount;
                  return prefixesMatch.uniqueCount <= 1;
                },
                { timeout: 20_000, intervals: [500, 1000] },
              )
              .toBe(true);
            await page.waitForTimeout(2_200);
            await podSelect.selectOption("");
            await expect(podSelect).toHaveValue("");
            await waitForLogsSettled();
            podSelectionCheck = {
              applicable: true,
              exercisedViaPrefixClick: true,
              selectedPodUIDNonEmpty: selectedVal !== "",
              filteredUniquePodPrefixes: finalUniqueCount,
              restoredAllPods: true,
            };
          } else {
            const firstPodUid = await podSelect.locator("option").nth(1).getAttribute("value");
            if (firstPodUid) {
              await podSelect.selectOption(firstPodUid);
              expect(await podSelect.inputValue()).toBe(firstPodUid);
              await page.waitForTimeout(2_200);
              await podSelect.selectOption("");
              await expect(podSelect).toHaveValue("");
              await waitForLogsSettled();
              podSelectionCheck = {
                applicable: true,
                exercisedViaDropdown: true,
                selectedPodUIDNonEmpty: true,
                restoredAllPods: true,
              };
            }
          }
        }

        // Test Container selection
        const containerSelect = logsRegion.getByLabel("Log container", { exact: true });
        const initialContainerVal = await containerSelect.inputValue();
        if (isWorkload && initialLogMetrics.hasAllContainersOption) {
          await page.waitForTimeout(2_200);
          await containerSelect.selectOption("*");
          expect(await containerSelect.inputValue()).toBe("*");
          await waitForLogsSettled();
          await page.waitForTimeout(2_200);
          await containerSelect.selectOption(initialContainerVal);
          expect(await containerSelect.inputValue()).toBe(initialContainerVal);
          await waitForLogsSettled();
        }

        // Current defaults: workload prefixes omit timestamps; Pod lines show
        // them. Exercise both states and restore the mode-specific default.
        const timestampsCheckbox = logsRegion.getByLabel("Timestamps", { exact: true });
        expect(await timestampsCheckbox.isChecked()).toBe(!isWorkload);
        await timestampsCheckbox.uncheck();
        expect(await timestampsCheckbox.isChecked()).toBe(false);
        await timestampsCheckbox.check();
        expect(await timestampsCheckbox.isChecked()).toBe(true);
        const hasTimestampTextWhenChecked = await logsRegion.evaluate((sectionEl) => {
          const pre = sectionEl.querySelector("pre");
          const firstSpan = pre?.querySelector<HTMLElement>("[data-log-key]");
          if (!firstSpan) return null;
          return /\d{4}-\d{2}-\d{2}T|\d{2}:\d{2}:\d{2}/.test(firstSpan.textContent ?? "");
        });
        if (hasTimestampTextWhenChecked !== null) expect(hasTimestampTextWhenChecked).toBe(true);
        await timestampsCheckbox.setChecked(!isWorkload);
        expect(await timestampsCheckbox.isChecked()).toBe(!isWorkload);

        // Test Wrap lines toggle
        const wrapCheckbox = logsRegion.getByLabel("Wrap lines", { exact: true });
        expect(await wrapCheckbox.isChecked()).toBe(true);
        await expect(logOutput).toHaveClass(/whitespace-pre-wrap/);
        await wrapCheckbox.uncheck();
        expect(await wrapCheckbox.isChecked()).toBe(false);
        await expect(logOutput).toHaveClass(/whitespace-pre/);
        await wrapCheckbox.check();
        await expect(logOutput).toHaveClass(/whitespace-pre-wrap/);

        // Test the live read, Pause/Resume, and Jump to latest. Following is
        // automatic: the status pill names the read, and Jump exists only while
        // the reader is away from the newest line.
        const liveStatusPill = logsRegion.getByLabel("Log connection status", { exact: true });
        await expect(liveStatusPill).toHaveText("Live");
        await expect(logsRegion.getByRole("checkbox", { name: "Follow", exact: true })).toHaveCount(0);
        await logOutput.evaluate((element) => { element.scrollTop = element.scrollHeight; element.dispatchEvent(new Event("scroll")); });
        await expect(logsRegion.getByRole("button", { name: "Jump to latest", exact: true })).toHaveCount(0);

        const pauseBtn = logsRegion.getByRole("button", { name: "Pause logs", exact: true });
        await pauseBtn.click();
        const resumeBtn = logsRegion.getByRole("button", { name: "Resume logs", exact: true });
        await expect(resumeBtn).toBeVisible();
        await resumeBtn.click();
        await expect(pauseBtn).toBeVisible();

        // Test Filter input
        const currentRetainedCount = await logOutput.locator("[data-log-key]").count();
        const filterInput = logsRegion.getByLabel("Log filter", { exact: true });
        await filterInput.fill("__nonexistent_synthetic_filter_token_88421__");
        if (currentRetainedCount > 0) {
          await expect(logOutput).toContainText("No log lines match this filter.");
        }
        await filterInput.fill("");
        let markCountOnMatch = 0;
        if (currentRetainedCount > 0) {
          markCountOnMatch = await logsRegion.evaluate((sectionEl) => {
            const pre = sectionEl.querySelector("pre");
            const firstSpan = pre?.querySelector<HTMLElement>("[data-log-key]");
            const rawText = firstSpan?.textContent ?? "";
            const tokenMatch = rawText.match(/[a-zA-Z]{3,6}/);
            const token = tokenMatch ? tokenMatch[0] : "a";
            const input = sectionEl.querySelector<HTMLInputElement>(`input[aria-label="Log filter"]`);
            const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")?.set;
            if (input && setter) {
              setter.call(input, token);
              input.dispatchEvent(new Event("input", { bubbles: true }));
            }
            const marks = pre ? pre.querySelectorAll("mark").length : 0;
            if (input && setter) {
              setter.call(input, "");
              input.dispatchEvent(new Event("input", { bubbles: true }));
            }
            return marks;
          });
        }

        // Test Previous logs / Previous container logs finite read
        await page.waitForTimeout(2_200);
        const instance = logsRegion.getByRole("group", { name: "Container instance", exact: true });
        const previousButton = instance.getByRole("button", { name: isWorkload ? "Previous logs" : "Previous container logs", exact: true });
        const currentButton = instance.getByRole("button", { name: "Current", exact: true });
        await expect(currentButton).toHaveAttribute("aria-pressed", "true");
        await expect(previousButton).toHaveAttribute("aria-pressed", "false");
        await previousButton.click();
        await expect(previousButton).toHaveAttribute("aria-pressed", "true");
        await expect(currentButton).toHaveAttribute("aria-pressed", "false");
        // A bounded previous read never follows, and the pill says so.
        await expect(liveStatusPill).toHaveText("Previous");
        await waitForLogsSettled();
        const previousReadState = await logsRegion.evaluate((sectionEl) => {
          const pre = sectionEl.querySelector("pre");
          const spans = pre ? Array.from(pre.querySelectorAll<HTMLElement>("[data-log-key]")) : [];
          return {
            lineSpans: spans.length,
            emptyMessage: spans.length === 0 ? (pre?.textContent?.trim() ?? "") : null,
          };
        });
        await page.waitForTimeout(2_200);
        await currentButton.click();
        await expect(previousButton).toHaveAttribute("aria-pressed", "false");
        await expect(currentButton).toHaveAttribute("aria-pressed", "true");
        await expect(liveStatusPill).toHaveText("Live", { timeout: 30_000 });
        await waitForLogsSettled();

        // Tab 3: Events
        await drawerTablist.getByRole("tab", { name: "Events", exact: true }).click();
        const eventsSection = detail.getByRole("region", { name: "Object-scoped events", exact: true });
        await expect(eventsSection).toBeVisible();
        await expect(eventsSection.getByRole("img", { name: "Event histogram: warnings red, normal green", exact: true })).toBeVisible();
        const eventsSummary = await eventsSection.evaluate((el) => {
          const articles = el.querySelectorAll("article").length;
          const buttons = Array.from(el.querySelectorAll("button")).map((b) => (b.textContent ?? "").trim());
          const text = el.textContent ?? "";
          const hasSecretLeak = /BEGIN [A-Z ]*PRIVATE KEY|Bearer\s+[A-Za-z0-9._-]{20,}/.test(text);
          return { articles, filterButtonsCount: buttons.length, hasSecretLeak };
        });
        expect(eventsSummary.hasSecretLeak).toBe(false);
        if (kind === "Deployment") {
          await captureRedacted("eks-drawer-events", 1440, "populated");
        }

        // Tab 4: YAML
        await drawerTablist.getByRole("tab", { name: "YAML", exact: true }).click();
        const yamlSection = detail.getByRole("region", { name: "Projected YAML", exact: true });
        await expect(yamlSection).toBeVisible();
        await expect(yamlSection.getByText("Projected, read-only", { exact: true })).toBeVisible();
        await expect(yamlSection.getByRole("navigation", { name: "YAML outline", exact: true })).toBeVisible();
        await yamlSection.getByRole("button", { name: "Fold all", exact: true }).click();
        const foldedCount = await yamlSection.evaluate((el) => {
          return Array.from(el.querySelectorAll("code span")).filter((s) => /…\s+\d+\s+lines/.test(s.textContent ?? "")).length;
        });
        expect(foldedCount).toBeGreaterThan(0);
        await yamlSection.getByRole("button", { name: "Unfold all", exact: true }).click();
        await yamlSection.getByLabel("Find in YAML", { exact: true }).fill("metadata");
        await expect(yamlSection.getByText(/\d+ matches/)).toBeVisible();
        const yamlSafety = await yamlSection.evaluate((el) => {
          const text = el.textContent ?? "";
          const lineCount = el.querySelectorAll(`[id^="resource-yaml-"]`).length;
          const outlineButtons = Array.from(el.querySelectorAll(`nav[aria-label="YAML outline"] button`)).map((b) => (b.textContent ?? "").trim());
          const hasSecretLeak = /BEGIN [A-Z ]*PRIVATE KEY|aws_secret_access_key|X-Gateway-Secret/i.test(text);
          return { lineCount, outlineButtons, hasSecretLeak };
        });
        expect(yamlSafety.lineCount).toBeGreaterThan(0);
        expect(yamlSafety.hasSecretLeak).toBe(false);
        await yamlSection.getByLabel("Find in YAML", { exact: true }).fill("");
        if (kind === "Deployment") {
          await captureRedacted("eks-drawer-yaml", 1440, "populated");
        }

        // Tab 5: Related
        await drawerTablist.getByRole("tab", { name: "Related", exact: true }).click();
        const relatedSection = detail.getByRole("region", { name: "Related resources", exact: true });
        await expect(relatedSection).toBeVisible();
        const relatedSummary = await relatedSection.evaluate((el) => {
          const mapButtons = el.querySelectorAll(`[aria-label="Relationship map"] button`).length;
          const headings = Array.from(el.querySelectorAll("section h3")).map((h) => (h.textContent ?? "").trim());
          const secretPills = Array.from(el.querySelectorAll("span.pill")).filter((s) => s.textContent?.trim() === "Secret").length;
          const contentNotShownCount = (el.textContent?.match(/content not shown/g) ?? []).length;
          return {
            mapButtons,
            headings,
            secretPills,
            secretsRedactedProperly: secretPills === 0 || contentNotShownCount >= secretPills,
          };
        });
        expect(relatedSummary.secretsRedactedProperly).toBe(true);
        if (kind === "Deployment") {
          await captureRedacted("eks-drawer-related", 1440, "populated");
        }

        // Tab 6: Timeline
        await drawerTablist.getByRole("tab", { name: "Timeline", exact: true }).click();
        const timelineSection = detail.getByRole("region", { name: "Resource change history", exact: true });
        await expect(timelineSection).toBeVisible();
        for (const h of ["1h", "6h", "24h"]) {
          const btn = timelineSection.getByRole("button", { name: h, exact: true });
          await expect(btn).toBeVisible();
          await btn.click();
          await expect(btn).toHaveAttribute("aria-pressed", "true");
        }
        const drawerTimelineSummary = await timelineSection.evaluate((el) => {
          const items = el.querySelectorAll("ol li").length;
          const text = el.textContent ?? "";
          return {
            historyItems: items,
            hasCaptureStartedNote: /No history before .* \(capture started\)/.test(text),
            hasEmptyNote: text.includes("No recorded changes or crashes in this range."),
          };
        });
        if (kind === "Deployment") {
          await captureRedacted("eks-drawer-timeline", 1440, "populated");
        }

        // Return to Logs tab briefly, then test Close cancellation of active log stream
        await drawerTablist.getByRole("tab", { name: "Logs", exact: true }).click();
        await page.waitForTimeout(600);
        await detail.getByRole("button", { name: "Close panel", exact: true }).click();
        await expect(detail).toHaveCount(0);
        await expect.poll(() => streamLifecycle.active, { timeout: 10_000 }).toBe(0);
        await page.waitForTimeout(2_200);

        drawerVerification[kind] = {
          exercised: true,
          sixTabsVerified: true,
          overview: overviewCheck,
          logs: {
            ...initialLogMetrics,
            arrivedNewLines,
            afterLineSpans,
            podSelectionCheck,
            hasTimestampTextWhenChecked,
            markCountOnMatch,
            previousReadState,
            warningBannersInLogs,
          },
          events: eventsSummary,
          yaml: yamlSafety,
          related: relatedSummary,
          timeline: drawerTimelineSummary,
          closeCancelledActiveStreams: streamLifecycle.active === 0,
        };
      }

      // 5. Probe active log emitter Pod if primary workloads were quiet during their observation window
      const anyArrivedSoFar = Object.values(drawerVerification).some(
        (v) => Boolean((v as { logs?: { arrivedNewLines?: boolean } })?.logs?.arrivedNewLines),
      );
      let activeEmitterProbe: Record<string, unknown> = { alreadyObservedInPrimary: anyArrivedSoFar };
      if (!anyArrivedSoFar && (await openTargetInDrawer("ActivePod"))) {
        const detail = page.getByRole("dialog", { name: "Details panel" });
        await detail.getByRole("tab", { name: "Logs", exact: true }).click();
        const logsRegion = detail.getByRole("region", { name: "Pod logs", exact: true });
        const logOutput = logsRegion.getByLabel("Pod log output", { exact: true });
        await expect
          .poll(async () => !((await logOutput.textContent()) ?? "").includes("Connecting to logs..."), { timeout: 20_000 })
          .toBe(true);
        const before = await logsRegion.evaluate((el) => {
          const spans = Array.from(el.querySelectorAll<HTMLElement>("pre [data-log-key]"));
          return { count: spans.length, lastKey: spans.at(-1)?.dataset.logKey ?? null };
        });
        let arrived = false;
        let after = before.count;
        try {
          await expect
            .poll(
              async () => {
                const cur = await logsRegion.evaluate((el) => {
                  const spans = Array.from(el.querySelectorAll<HTMLElement>("pre [data-log-key]"));
                  return { count: spans.length, lastKey: spans.at(-1)?.dataset.logKey ?? null };
                });
                after = cur.count;
                return cur.count > before.count || (cur.lastKey !== null && cur.lastKey !== before.lastKey);
              },
              { timeout: 20_000, intervals: [1_000] },
            )
            .toBe(true);
          arrived = true;
        } catch {
          arrived = false;
        }
        await detail.getByRole("button", { name: "Close panel", exact: true }).click();
        await expect(detail).toHaveCount(0);
        await expect.poll(() => streamLifecycle.active, { timeout: 10_000 }).toBe(0);
        await page.waitForTimeout(2_200);
        activeEmitterProbe = {
          probedBusyPod: true,
          beforeLines: before.count,
          afterLines: after,
          arrivedNewLines: arrived,
        };
      }

      // 6. Examine naturally existing Issues and Timeline break logs (Read-only, no manufactured faults)
      await page.getByRole("tab", { name: "Issues", exact: true }).click();
      const issuesRegion = page.getByRole("region", { name: "Kubernetes issues", exact: true });
      await expect(issuesRegion).toBeVisible();
      await expect(issuesRegion.getByRole("status", { name: "Loading grouped issues" })).toHaveCount(0, { timeout: 30_000 });

      const breakAndAgentProbe = await page.evaluate(async () => {
        const safeFetch = async (url: string) => {
          const res = await fetch(url, { credentials: "same-origin" });
          let json: Record<string, unknown> | null = null;
          try { json = (await res.json()) as Record<string, unknown>; } catch { json = null; }
          return { status: res.status, json };
        };
        const allIssues = await safeFetch("/api/admin/kubernetes/issues?limit=50");
        const brokenIssues = await safeFetch("/api/admin/kubernetes/issues?broken=true&limit=50");
        const changes = await safeFetch("/api/admin/kubernetes/changes?limit=50");
        const crashes = await safeFetch("/api/admin/kubernetes/crashes?limit=50");
        const agentSettings = await safeFetch("/api/admin/kubernetes/agent-settings");

        const allItems = Array.isArray(allIssues.json?.items) ? (allIssues.json!.items as Array<Record<string, unknown>>) : [];
        const brokenItems = Array.isArray(brokenIssues.json?.items) ? (brokenIssues.json!.items as Array<Record<string, unknown>>) : [];
        const changeItems = Array.isArray(changes.json?.items) ? (changes.json!.items as Array<Record<string, unknown>>) : [];
        const crashItems = Array.isArray(crashes.json?.items) ? (crashes.json!.items as Array<Record<string, unknown>>) : [];

        const settingsBody = agentSettings.json ?? {};
        const detected = (settingsBody.detected as Record<string, unknown> | undefined) ?? {};
        const learning = (settingsBody.learning as Record<string, unknown> | undefined) ?? {};
        const scope = (learning.scope as Record<string, unknown> | undefined) ?? {};
        const breakLogs = (settingsBody.break_logs as Record<string, unknown> | undefined) ?? {};
        const sources = Array.isArray(settingsBody.sources) ? (settingsBody.sources as Array<Record<string, unknown>>) : [];

        const validBroken = brokenItems.filter((i) => {
          const r = (i.root as Record<string, unknown> | undefined) ?? {};
          return Boolean(r.uid && r.namespace && i.broke_at);
        });
        let rootsWithLines = 0;
        const coverageStates: Record<string, number> = {};
        const coverageReasons: Record<string, number> = {};
        const sourceKinds: Record<string, number> = {};
        const win = window as unknown as {
          __populatedBreakRoot?: { uid: string; namespace: string; broke_at: string } | null;
          __brokenRoot?: { uid: string; namespace: string; broke_at: string };
        };
        const firstRoot = validBroken[0];
        if (firstRoot) {
          const root = firstRoot.root as Record<string, unknown>;
          win.__brokenRoot = { uid: String(root.uid), namespace: String(root.namespace), broke_at: String(firstRoot.broke_at) };
        }
        win.__populatedBreakRoot = null;
        for (const item of validBroken.slice(0, 14)) {
          const r = (item.root as Record<string, unknown>) ?? {};
          const brokeMs = Date.parse(String(item.broke_at));
          if (!Number.isFinite(brokeMs)) continue;
          const since = new Date(brokeMs - 5 * 60_000).toISOString();
          const until = new Date(brokeMs + 2 * 60_000).toISOString();
          const bl = await safeFetch(
            `/api/admin/kubernetes/break-logs?namespace=${encodeURIComponent(String(r.namespace))}&root_uid=${encodeURIComponent(String(r.uid))}&since=${encodeURIComponent(since)}&until=${encodeURIComponent(until)}&limit=100`,
          );
          if (bl.status === 200 && bl.json) {
            const cov = (bl.json.coverage as Record<string, unknown> | undefined) ?? {};
            const st = String(cov.state ?? "unknown");
            const rsn = String(cov.reason ?? "none");
            coverageStates[st] = (coverageStates[st] ?? 0) + 1;
            coverageReasons[rsn] = (coverageReasons[rsn] ?? 0) + 1;
            const srcs = Array.isArray(bl.json.sources) ? (bl.json.sources as Array<Record<string, unknown>>) : [];
            for (const s of srcs) {
              const sk = String(s.kind ?? "unknown");
              sourceKinds[sk] = (sourceKinds[sk] ?? 0) + 1;
            }
            const counts = (bl.json.counts as Record<string, number> | undefined) ?? {};
            const linesArr = Array.isArray(bl.json.lines) ? bl.json.lines : [];
            if ((counts.all ?? 0) > 0 || linesArr.length > 0) {
              rootsWithLines += 1;
              if (!win.__populatedBreakRoot) {
                win.__populatedBreakRoot = {
                  uid: String(r.uid),
                  namespace: String(r.namespace),
                  broke_at: String(item.broke_at),
                };
              }
            }
          }
        }

        return {
          issues: {
            status: allIssues.status,
            classification: allIssues.status === 200 ? (allItems.length > 0 ? "populated" : "empty") : `status_${allIssues.status}`,
            totalItems: allItems.length,
            totals: (allIssues.json?.totals as Record<string, number> | undefined) ?? {},
            brokenStatus: brokenIssues.status,
            brokenItemsCount: brokenItems.length,
            validBrokenRootsCount: validBroken.length,
            rootsWithBreakLogLines: rootsWithLines,
            coverageStates,
            coverageReasons,
            sourceKinds,
          },
          timelineData: {
            changesStatus: changes.status,
            changesCount: changeItems.length,
            crashesStatus: crashes.status,
            crashesCount: crashItems.length,
          },
          agentSettings: {
            status: agentSettings.status,
            canWrite: Boolean(settingsBody.can_write),
            detected: {
              workloads: Number(detected.workloads ?? 0),
              namespaces: Number(detected.namespaces ?? 0),
              metricsServer: Boolean(detected.metrics_server),
              tracesKind: (detected.traces as Record<string, unknown> | null)?.kind ?? null,
            },
            learning: {
              logs: Boolean(learning.logs),
              metrics: Boolean(learning.metrics),
              traces: Boolean(learning.traces),
              includeCount: Array.isArray(scope.include) ? scope.include.length : 0,
              excludeCount: Array.isArray(scope.exclude) ? scope.exclude.length : 0,
            },
            sourcesCount: sources.length,
            sourceTypes: sources.map((s) => ({ type: String(s.type ?? ""), health: String(s.health ?? "") })),
            linkedSourceConfigured: Boolean(breakLogs.linked_source),
            linkedSourceMissing: Boolean(settingsBody.linked_source_missing),
            fieldTypes: Object.keys((breakLogs.fields as Record<string, unknown> | undefined) ?? {}).sort(),
          },
        };
      });

      // The unfocused timeline is the retained event list; break logs and the
      // bracket belong to the focused root, not to that list.
      await page.getByRole("tab", { name: "Timeline", exact: true }).click();
      if (breakAndAgentProbe.issues.validBrokenRootsCount > 0) {
        await page.evaluate(() => {
          const root = (window as unknown as { __brokenRoot: { uid: string; namespace: string; broke_at: string } }).__brokenRoot;
          const query = new URLSearchParams({ view: "timeline", root_uid: root.uid, namespace: root.namespace, center: root.broke_at });
          history.pushState(null, "", `?${query}`);
          dispatchEvent(new PopStateEvent("popstate"));
        });
      }
      const timelineRegion = page.getByRole("region", { name: "Kubernetes change timeline", exact: true });
      await expect(timelineRegion).toBeVisible();
      const windowGroup = timelineRegion.getByRole("group", { name: "Timeline window", exact: true });
      await expect(windowGroup).toBeVisible();
      await windowGroup.getByRole("button", { name: "24h", exact: true }).click();
      await expect(timelineRegion.getByRole("status", { name: /Loading/i })).toHaveCount(0, { timeout: 30_000 });

      const breakLogsSection = page.locator("#kubernetes-break-logs");
      // Wait up to 15s for broken issues query on Timeline to mount #kubernetes-break-logs if valid broken roots exist
      if (breakAndAgentProbe.issues.validBrokenRootsCount > 0) {
        await expect(breakLogsSection).toBeVisible({ timeout: 20_000 });
      }
      const breakLogsMounted = (await breakLogsSection.count()) > 0;
      let breakLogsUiCheck: Record<string, unknown> = {
        mounted: breakLogsMounted,
        naturalBrokenIssuesCount: breakAndAgentProbe.issues.brokenItemsCount,
        validBrokenRootsCount: breakAndAgentProbe.issues.validBrokenRootsCount,
        naturalCrashesCount: breakAndAgentProbe.timelineData.crashesCount,
      };

      if (breakLogsMounted) {
        // Wait for break logs query to finish loading
        await expect(breakLogsSection.getByRole("status", { name: "Loading logs at break..." })).toHaveCount(0, { timeout: 25_000 });
        const tipBtn = breakLogsSection.getByRole("button", { name: "Log coverage tip", exact: true });
        const tipVisible = await tipBtn.isVisible();
        let coverageTipDetails: Record<string, unknown> | null = null;
        if (tipVisible) {
          await tipBtn.click();
          const tipPopover = breakLogsSection.getByRole("region", { name: "Log coverage tip", exact: true });
          await expect(tipPopover).toBeVisible();
          coverageTipDetails = await tipPopover.evaluate((el) => ({
            heading: el.querySelector("h4")?.textContent?.trim() ?? null,
            bodyText: el.querySelector("p")?.textContent?.trim() ?? null,
            hasAgentLink: Boolean(el.querySelector(`a[href*="view=agent"]`)),
          }));
          await page.keyboard.press("Escape");
        }
        const panelDetails = await breakLogsSection.evaluate((el) => {
          const podBtns = Array.from(el.querySelectorAll(`[role="group"][aria-label="Log pods"] button`));
          const modeBtns = Array.from(el.querySelectorAll(`[role="group"][aria-label="Container log mode"] button`)).map((b) =>
            (b.textContent ?? "").trim(),
          );
          const logLinesDiv = el.querySelector(`div[aria-label="Break log lines"]`);
          const emptyStatus = logLinesDiv?.querySelector(`p[role="status"]`)?.textContent?.trim() ?? null;
          const footerText = el.querySelector("footer span")?.textContent?.trim() ?? null;
          const asideText = el.querySelector("aside")?.textContent ?? "";
          const coverageMatch = asideText.match(/Coverage:\s*([a-z_]+)/i);
          return {
            podFilterButtonCount: podBtns.length,
            modeButtons: modeBtns,
            emptyStatus,
            footerText,
            coverageState: coverageMatch ? coverageMatch[1] : null,
            hasNoLinkedSourceNote: asideText.includes("No linked-source lines in this window."),
          };
        });
        const bracketVisible = await timelineRegion.getByLabel("Log window", { exact: true }).isVisible();
        const startSliderVisible = await timelineRegion.getByRole("slider", { name: "Log window start", exact: true }).isVisible();
        const endSliderVisible = await timelineRegion.getByRole("slider", { name: "Log window end", exact: true }).isVisible();

        breakLogsUiCheck = {
          ...breakLogsUiCheck,
          exercised: true,
          bracketVisible,
          startSliderVisible,
          endSliderVisible,
          tipVisible,
          coverageTipDetails,
          panelDetails,
          hasAskChatButton: await breakLogsSection.getByRole("button", { name: "Ask chat" }).first().isVisible(),
          hasOpenLiveLogsButton: await breakLogsSection.getByRole("button", { name: "Open live logs" }).first().isVisible(),
        };
      } else {
        breakLogsUiCheck = {
          ...breakLogsUiCheck,
          exercised: false,
          verdict: "NOT_EXERCISED",
          reason: "No naturally broken workloads with valid break timestamps mounted the panel.",
        };
      }

      // Exercise Container log mode buttons and Log window slider keyboard adjustment if visible
      if (breakLogsMounted) {
        const modeGroup = breakLogsSection.getByRole("group", { name: "Container log mode", exact: true });
        if (await modeGroup.isVisible()) {
          for (const modeLabel of ["Previous container", "Current", "Merged"]) {
            const btn = modeGroup.getByRole("button", { name: modeLabel, exact: true });
            await btn.click();
            await expect(btn).toHaveAttribute("aria-pressed", "true");
          }
        }
        const startSlider = timelineRegion.getByRole("slider", { name: "Log window start", exact: true });
        if (await startSlider.isVisible()) {
          await startSlider.focus();
          await page.keyboard.press("ArrowRight");
          await expect.poll(() => new URL(page.url()).searchParams.has("logs_from")).toBe(true);
        }
      }

      // Check Captured crash evidence journal on Timeline
      const crashJournalCheck = await timelineRegion.evaluate((el) => {
        const crashSection = el.querySelector(`section[aria-label="Captured crash evidence"]`);
        const headerSpan = crashSection?.querySelector("header span")?.textContent?.trim() ?? null;
        const rowsCount = crashSection ? crashSection.querySelectorAll("tbody tr, ul li, article").length : 0;
        const chartCrashMarkers = el.querySelectorAll(`[data-marker="crash"]`).length;
        const chartChangeMarkers = el.querySelectorAll(`[data-marker="change"]`).length;
        return {
          present: Boolean(crashSection),
          headerSummary: headerSpan,
          rowsCount,
          chartCrashMarkers,
          chartChangeMarkers,
        };
      });

      // If a crash marker is present on the 24h chart, click it to verify the crash popover ("Logs before crash", "Open pod")
      let crashPopoverCheck: Record<string, unknown> = { exercised: false };
      const crashLabelBtn = timelineRegion.locator(".resource-timeline-crash-label").first();
      if ((await crashLabelBtn.count()) > 0 && (await crashLabelBtn.isVisible())) {
        await crashLabelBtn.click();
        const crashActions = page.getByRole("region", { name: "Crash log actions" }).first();
        if ((await crashActions.count()) > 0 && (await crashActions.isVisible())) {
          crashPopoverCheck = {
            exercised: true,
            hasLogsBeforeCrashBtn: await crashActions.getByRole("button", { name: "Logs before crash", exact: true }).isVisible(),
            hasOpenPodBtn: await crashActions.getByRole("button", { name: "Open pod", exact: true }).isVisible(),
          };
          await page.keyboard.press("Escape");
        }
      }

      // Focus a broken root that has actual captured/Kubernetes break log lines (win.__populatedBreakRoot)
      // so we also verify populated break log lines, source badges,LogWindowBracket sliders, and rate strip!
      let populatedBreakLogsCheck: Record<string, unknown> = { exercised: false };
      const hasPopulatedBreakRoot = await page.evaluate(() => {
        const win = window as unknown as {
          __populatedBreakRoot?: { uid: string; namespace: string; broke_at: string } | null;
        };
        if (!win.__populatedBreakRoot) return false;
        const p = win.__populatedBreakRoot;
        const q = new URLSearchParams({
          view: "timeline",
          root_uid: p.uid,
          namespace: p.namespace,
          center: p.broke_at,
        });
        window.history.pushState(null, "", `/agent/kubernetes?${q}`);
        window.dispatchEvent(new PopStateEvent("popstate"));
        return true;
      });
      if (hasPopulatedBreakRoot) {
        await expect(timelineRegion.getByRole("status", { name: /Loading/i })).toHaveCount(0, { timeout: 30_000 });
        await expect(breakLogsSection).toBeVisible({ timeout: 25_000 });
        await expect(breakLogsSection.getByRole("status", { name: "Loading logs at break..." })).toHaveCount(0, { timeout: 25_000 });
        // Also wait until footer or empty status is rendered inside #kubernetes-break-logs
        await expect
          .poll(
            async () =>
              breakLogsSection.evaluate((el) => Boolean(el.querySelector("footer span") || el.querySelector(`p[role="alert"]`))),
            { timeout: 20_000, intervals: [500, 1000] },
          )
          .toBe(true);

        // Check crash marker popover on focused root timeline
        const focusedCrashBtn = timelineRegion.locator(`.resource-timeline-crash-label, button[data-marker="crash"]`).first();
        if ((await focusedCrashBtn.count()) > 0 && (await focusedCrashBtn.isVisible())) {
          await focusedCrashBtn.click();
          const eventDetails = page.getByRole("region", { name: "Timeline event details" }).first();
          const crashActions = page.getByRole("region", { name: "Crash log actions" }).first();
          const actionsVisible = (await crashActions.count()) > 0 && (await crashActions.isVisible());
          const detailsVisible = (await eventDetails.count()) > 0 && (await eventDetails.isVisible());
          if (actionsVisible || detailsVisible) {
            crashPopoverCheck = {
              exercised: true,
              crashActionsPopoverVisible: actionsVisible,
              eventDetailsVisible: detailsVisible,
              hasLogsBeforeCrashBtn: actionsVisible
                ? await crashActions.getByRole("button", { name: "Logs before crash", exact: true }).isVisible()
                : false,
              hasOpenPodBtn: actionsVisible
                ? await crashActions.getByRole("button", { name: "Open pod", exact: true }).isVisible()
                : false,
            };
            await page.keyboard.press("Escape");
          }
        }

        const startSlider = timelineRegion.getByRole("slider", { name: "Log window start", exact: true });
        const endSlider = timelineRegion.getByRole("slider", { name: "Log window end", exact: true });
        const startVisible = await startSlider.isVisible();
        const endVisible = await endSlider.isVisible();
        populatedBreakLogsCheck = await breakLogsSection.evaluate(
          (el, sliders) => {
            const pills = Array.from(el.querySelectorAll("header .pill")).map((p) => (p.textContent ?? "").trim());
            const footerText = el.querySelector("footer span")?.textContent?.trim() ?? null;
            const patternItems = el.querySelectorAll("aside ul li").length;
            const logRows = el.querySelectorAll(`div[aria-label="Break log lines"] > div`).length;
            const asideText = el.querySelector("aside")?.textContent ?? "";
            return {
              exercised: true,
              startSliderVisible: sliders.startVisible,
              endSliderVisible: sliders.endVisible,
              sliderKeyboardUpdatedUrl: sliders.startVisible,
              sourceBadges: pills,
              logRowsRendered: logRows,
              footerText,
              patternItemsCount: patternItems,
              hasCapturedSource: pills.includes("Captured at break") || asideText.includes("Captured at break"),
              hasKubernetesSource: pills.includes("Kubernetes container logs") || asideText.includes("Kubernetes container logs"),
            };
          },
          { startVisible, endVisible },
        );
        if (startVisible) {
          await startSlider.focus();
          await page.keyboard.press("ArrowRight");
          await expect.poll(() => new URL(page.url()).searchParams.has("logs_from")).toBe(true);
          await page.keyboard.press("ArrowLeft");
          await expect(breakLogsSection.getByRole("status", { name: "Loading logs at break..." })).toHaveCount(0, { timeout: 25_000 });
        }
      }

      expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
      await captureRedacted("eks-timeline", 1440, "observed");

      // 7. Switch to Agent tab (Read-only inspection, zero saves or test-link calls)
      await page.getByRole("tab", { name: "Agent", exact: true }).click();
      const agentSection = page.getByRole("region", { name: "Kubernetes Agent", exact: true });
      await expect(agentSection).toBeVisible();
      const learningSection = agentSection.getByRole("region", { name: "Agent learning", exact: true });
      await expect(learningSection).toBeVisible();
      await expect(learningSection.getByText("Preview", { exact: true })).toBeVisible();

      const captureSection = agentSection.getByRole("region", { name: "Log capture at break", exact: true });
      await expect(captureSection).toBeVisible();
      await expect(captureSection.getByText("Always on", { exact: true })).toBeVisible();

      const agentUiState = await agentSection.evaluate((el) => {
        const switches = Array.from(el.querySelectorAll<HTMLInputElement>(`input[role="switch"]`)).map((s) => ({
          label: s.getAttribute("aria-label"),
          checked: s.checked,
          disabled: s.disabled,
        }));
        const text = el.textContent ?? "";
        const hasNoSourceEmptyState = text.includes("No log data source is configured");
        const saveBtn = Array.from(el.querySelectorAll<HTMLButtonElement>("button")).find((b) =>
          (b.textContent ?? "").includes("Save settings"),
        );
        const discardBtn = Array.from(el.querySelectorAll<HTMLButtonElement>("button")).find((b) =>
          (b.textContent ?? "").includes("Discard changes"),
        );
        return {
          switches,
          hasNoSourceEmptyState,
          saveButtonDisabled: saveBtn ? saveBtn.disabled : null,
          discardButtonDisabled: discardBtn ? discardBtn.disabled : null,
        };
      });
      if (breakAndAgentProbe.agentSettings.canWrite) {
        expect(agentUiState.saveButtonDisabled).toBe(true);
        expect(agentUiState.discardButtonDisabled).toBe(true);
      }

      expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
      await captureRedacted("eks-agent", 1440, "readonly");

      // 8. Mobile viewport (390x844) responsive checks & captures
      await page.setViewportSize({ width: 390, height: 844 });
      await expect(agentSection).toBeVisible();
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
      await captureRedacted("eks-agent", 390, "readonly");

      await page.getByRole("tab", { name: "Timeline", exact: true }).click();
      await expect(timelineRegion).toBeVisible();
      await expect(timelineRegion.getByRole("status", { name: /Loading/i })).toHaveCount(0, { timeout: 30_000 });
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
      await captureRedacted("eks-timeline", 390, "observed");

      await page.getByRole("tab", { name: "Overview", exact: true }).click();
      await expect(page.getByRole("region", { name: "Cluster health", exact: true })).toBeVisible();
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
      await waitForExternalOverview(page);
      await captureRedacted("eks-overview", 390, "populated");

      // Mobile toolbar and native log action-menu controls check
      let mobileDrawerCheck: Record<string, unknown> = { exercised: false };
      if (await openTargetInDrawer("Deployment")) {
        const detail = page.getByRole("dialog", { name: "Details panel" });
        await expect(detail).toBeVisible();
        await expect(detail.getByRole("region", { name: "Live workload metrics", exact: true })).toBeVisible({ timeout: 30_000 });
        expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
        await captureRedacted("eks-drawer-overview", 390, "populated");

        await detail.getByRole("tab", { name: "Logs", exact: true }).click();
        const logsRegion = detail.getByRole("region", { name: "Workload live logs", exact: true });
        await expect(logsRegion).toBeVisible();
        const logOutput = logsRegion.getByLabel("Workload log output", { exact: true });
        await page.waitForTimeout(350);
        await expect
          .poll(async () => !((await logOutput.textContent()) ?? "").includes("Connecting to logs..."), { timeout: 25_000 })
          .toBe(true);
        // Current toolbar keeps its primary controls visible on mobile and
        // puts reload/copy in a native details action menu, not a More expander.
        await expect(logsRegion.getByRole("combobox", { name: "Since window", exact: true })).toBeVisible();
        await expect(logsRegion.getByRole("group", { name: "Container instance", exact: true })).toBeVisible();
        const actionMenu = logsRegion.locator("details.log-action-menu");
        const moreActions = actionMenu.getByLabel("More log actions", { exact: true });
        await expect(moreActions).toBeVisible();
        await moreActions.click();
        await expect(actionMenu).toHaveAttribute("open", "");
        await expect(actionMenu.getByRole("button", { name: "Reload logs", exact: true })).toBeVisible();
        await expect(actionMenu.getByRole("button", { name: "Copy scrubbed logs", exact: true })).toBeVisible();
        await moreActions.click();
        await expect(actionMenu).not.toHaveAttribute("open", "");
        await expect(actionMenu.getByRole("button", { name: "Reload logs", exact: true })).toBeHidden();
        expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
        await captureRedacted("eks-drawer-logs", 390, "populated");

        await page.keyboard.press("Escape");
        await expect(detail).toHaveCount(0);
        await expect.poll(() => streamLifecycle.active, { timeout: 10_000 }).toBe(0);
        mobileDrawerCheck = {
          exercised: true,
          actionMenuToggled: true,
          escapeClosedDrawer: true,
          activeStreamsAfterEscape: streamLifecycle.active,
        };
      }

      expect(mutatingRequests).toEqual([]);
      expect(pageErrors).toEqual([]);

      const sanitizedReceipt = {
        targetUrl: env.baseURL,
        readOnlyGuard: {
          mutatingApiRequestsCount: mutatingRequests.length,
          pageErrorsCount: pageErrors.length,
        },
        streamLifecycle,
        clusterOverview: clusterProbe.overview,
        workloadsInventory: clusterProbe.workloadsInventory,
        deepLinkNormalization: deepLinkNormalized,
        drawerVerification,
        activeEmitterProbe,
        breakAndAgentInspection: {
          ...breakAndAgentProbe,
          breakLogsUiCheck,
          populatedBreakLogsCheck,
          crashPopoverCheck,
          crashJournalCheck,
          agentUiState,
        },
        mobileDrawerCheck,
        capturedImages,
        qualification:
          "Real external EKS read-only viewer acceptance; all persisted screenshots DOM-redacted to generic identities; zero cluster or settings mutations.",
      };

      await testInfo.attach("eks-readonly-acceptance", {
        body: JSON.stringify(sanitizedReceipt, null, 2),
        contentType: "application/json",
      });
      if (process.env.HARNESS_RESULTS_DIR) {
        fs.writeFileSync(
          path.join(process.env.HARNESS_RESULTS_DIR, "eks-readonly-acceptance.json"),
          JSON.stringify(sanitizedReceipt, null, 2),
          { mode: 0o600 },
        );
      }
    } finally {
      await context.close();
    }
  });

  test("verifies read-only namespace recent-center root windows and published restart", async ({ browser }) => {
    test.setTimeout(300_000);
    expect(env.baseURL).toBe("http://127.0.0.1:28080");
    const context = await browser.newContext({ baseURL: env.baseURL, viewport: { width: 1440, height: 900 }, colorScheme: "dark" });
    const page = await context.newPage();
    const images: Array<{ name: string; viewport: string; sha256: string }> = [];
    const reads: Array<{ route: string; status: number; scoped: boolean; root: boolean; enriched: boolean; future: boolean }> = [];
    const mutations: string[] = [];
    const checkpoints: Array<{ state: string; width: number }> = [];
    const geometry: Array<Awaited<ReturnType<typeof measureExternalGeometry>>> = [];
    const rootSelections: Array<{ width: number; selected: boolean; availability: string; candidates: number; listStatuses: number[]; attempts: Array<{ statuses: number[]; issues: number; changes: number; crashes: number }> }> = [];
    const rootWindows: Array<{ width: number; shape: "valid" | "empty"; statuses: number[]; issues: number; changes: number; crashes: number; renderedChanges: number; renderedCrashes: number }> = [];
    const quietSamples: Array<{ preparing: boolean; redInventory: boolean }> = [];
    let quietSamplingErrors = 0;
    let quietTimer: ReturnType<typeof setInterval> | undefined;
    let quietRead: Promise<void> | undefined;
    const pageErrors: string[] = [];
    let restartObserved503 = false;
    let restartStarted = false;
    let restartEnrichedReturned = false;
    let restartCompleted = false;
    let initialPublished = false;
    let firstMarkerAbsent = false;
    let firstPreparationVerified = false;
    let retainedMarker = false;
    const apiPath = "/api/admin/kubernetes/";
    page.on("pageerror", (error) => pageErrors.push(error.name));
    page.on("request", (request) => {
      const url = new URL(request.url());
      if (url.pathname.startsWith("/api/") && !["GET", "HEAD", "OPTIONS"].includes(request.method()) && url.pathname !== "/api/auth/gateway-session") mutations.push(request.method());
    });
    page.on("response", async (response) => {
      const url = new URL(response.url());
      if (!url.pathname.startsWith(apiPath)) return;
      const route = url.pathname.slice(apiPath.length).split("/")[0];
      if (["overview", "issues", "timeline", "changes", "crashes"].includes(route)) {
        reads.push({ route, status: response.status(), scoped: Boolean(url.searchParams.get("namespace")), root: Boolean(url.searchParams.get("root_uid")), enriched: url.searchParams.get("include") === "changes", future: Date.parse(url.searchParams.get("until") ?? "") > Date.now() + 1000 });
        if (restartStarted && response.status() === 503) restartObserved503 = true;
      }
      if (!response.ok()) return;
      try {
        const payload: unknown = await response.json();
        const tokens: string[] = [];
        const walk = (value: unknown): void => {
          if (!value || typeof value !== "object") return;
          for (const [key, item] of Object.entries(value)) {
            if (typeof item === "string" && /^(name|namespace|uid|id|pod|pod_uid|root_uid|root_name|container|cluster_id|resource_id)$/.test(key)) tokens.push(item);
            else if (typeof item === "object") walk(item);
          }
        };
        walk(payload);
        await page.evaluate((values) => {
          const win = window as unknown as { __redactTokens?: Set<string> };
          win.__redactTokens ??= new Set();
          values.forEach((value) => win.__redactTokens!.add(value));
        }, tokens);
      } catch { /* Closing a page can cancel a completed read's redaction collection. */ }
    });
    const settled = async () => {
      await expect(page.getByRole("region", { name: "Kubernetes change timeline", exact: true })).toBeVisible();
      await expect(page.getByRole("status", { name: /Loading projected change history|Loading related root change history|Loading captured crash evidence/ })).toHaveCount(0, { timeout: 45_000 });
      await expect(page.getByText("Root history unavailable.", { exact: true })).toHaveCount(0);
      await expect(page.getByText("Crash journal unavailable.", { exact: true })).toHaveCount(0);
      await expect(page.getByText("Timeline unavailable.", { exact: true })).toHaveCount(0);
    };
    const capture = async (state: string, width: number) => {
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
      const measured = state === "first-preparing" || state === "issues"
        ? await measureExternalGeometry(page, state === "first-preparing" ? "preparing" : "issues") : null;
      if (measured) geometry.push(measured);
      await captureExternalRedacted(page, images, "eks-history", width, state);
      checkpoints.push({ state, width });
      if (measured?.surface === "preparing") {
        expect(measured.rows.length, "Listing row geometry must be measured before redaction").toBe(1);
        const row = measured.rows[0];
        expect(row.measured).toBe(true);
        expect(row.phraseCount).toBeGreaterThan(0);
        expect(row.overlaps, "Essential listing label/count glyphs must not overlap").toBe(0);
        expect(row.overflowedPhrases, "Every essential phrase must fit its own group and row").toBe(0);
        if (width >= 640) expect(row.centerDelta, "Desktop listing remains vertically aligned").toBeLessThanOrEqual(2);
      }
    };
    try {
      await page.addInitScript(() => localStorage.setItem("versus.theme", "dark"));
      await openApp(page, "/agent/kubernetes");
      // The authorized fresh-start handle creates one new run-owned volume;
      // the already attached browser and old volumes are retained.
      await execute("/bin/bash", [path.join(workspace, "plans/harness-run/scripts/eks-readonly.sh"), "--start"], {
        timeout: 180_000,
        env: { ...process.env, HARNESS_EKS_FRESH_DATA: "1", HARNESS_EKS_BROWSER_HANDOFF: "1", HARNESS_EKS_SOURCE_RECEIPT: process.env.HARNESS_EKS_SOURCE_RECEIPT },
      });
      process.env.HARNESS_EKS_VOLUME_OVERRIDE = path.join(process.env.HARNESS_DATA_DIR!, "owned-data-volume.json");
      await openApp(page, "/agent/kubernetes");
      const preparation = page.locator("[data-preparing-panel]");
      await expect(preparation).toBeVisible({ timeout: 15_000 });
      await expect(preparation.locator("[data-step]")).toHaveCount(4);
      await expect(preparation.getByRole("progressbar")).toHaveCount(1);
      await expect(preparation.locator("[data-first-sync-counts]")).toBeVisible();
      const startup = await page.evaluate(async () => {
        const sync = await fetch("/api/admin/kubernetes/sync").then((response) => response.json());
        const alerts = await fetch("/api/admin/kubernetes/issue-alerts").then((response) => response.json());
        return { markerAbsent: !sync.first_ready_at, alertsOff: alerts.enabled === false };
      });
      firstMarkerAbsent = startup.markerAbsent;
      expect(startup.markerAbsent, "Actual first-publication marker must be absent").toBe(true);
      expect(startup.alertsOff, "Read-only acceptance must not enable alerts").toBe(true);
      await expect(page.locator("[role=alert]").filter({ hasText: /inventory_not_ready|503/ })).toHaveCount(0);
      await capture("first-preparing", 1440);
      await page.setViewportSize({ width: 390, height: 844 });
      await expect(preparation).toBeVisible();
      await capture("first-preparing", 390);
      await expect(page.getByRole("region", { name: "Cluster health", exact: true })).toBeVisible({ timeout: 90_000 });
      await expect(preparation).toHaveCount(0);
      initialPublished = await page.evaluate(async () => Boolean((await fetch("/api/admin/kubernetes/sync").then((response) => response.json())).first_ready_at));
      expect(initialPublished, "First publication returns data automatically").toBe(true);
      firstPreparationVerified = true;
      // No action or alert controls are clicked; the request guard below remains authoritative.
      for (const width of [1440, 390]) {
        await page.setViewportSize({ width, height: width === 390 ? 844 : 900 });
        const beforeOverview = reads.length;
        await page.goto("/agent/kubernetes");
        await expect(page.getByRole("region", { name: "Cluster health", exact: true })).toBeVisible({ timeout: 45_000 });
        await waitForExternalOverview(page);
        await expect.poll(() => reads.slice(beforeOverview).some((read) => read.route === "issues" && read.enriched && read.status === 200)).toBe(true);
        await capture("overview", width);
        const beforeIssues = reads.length;
        await page.getByRole("tab", { name: "Issues", exact: true }).click();
        await expect(page.getByRole("region", { name: "Kubernetes issues", exact: true })).toBeVisible();
        await expect(page.getByRole("status", { name: "Loading grouped issues" })).toHaveCount(0, { timeout: 45_000 });
        await expect.poll(() => reads.slice(beforeIssues).some((read) => read.route === "issues" && !read.enriched && !read.root && read.status === 200)).toBe(true);
        await capture("issues", width);
        const globalRead = page.waitForResponse((response) => {
          const url = new URL(response.url());
          return url.pathname.endsWith("/kubernetes/timeline") && !url.searchParams.get("namespace");
        });
        await page.getByRole("tab", { name: "Timeline", exact: true }).click();
        expect((await globalRead).status()).toBe(200);
        await settled();
        await capture("global", width);
        const namespace = page.getByRole("combobox", { name: "Timeline namespace", exact: true });
        await namespace.fill("core-integration");
        const scopedRead = page.waitForResponse((response) => {
          const url = new URL(response.url());
          return url.pathname.endsWith("/kubernetes/timeline") && Boolean(url.searchParams.get("namespace"));
        });
        await page.getByRole("option", { name: "core-integration", exact: true }).click();
        expect((await scopedRead).status()).toBe(200);
        await settled();
        await capture("scoped", width);
        const recent = page.waitForResponse((response) => new URL(response.url()).pathname.endsWith("/kubernetes/timeline"));
        await page.goto(`/agent/kubernetes?view=timeline&namespace=core-integration&center=${encodeURIComponent(new Date(Date.now() - 120_000).toISOString())}`);
        const recentResponse = await recent;
        expect(recentResponse.status()).toBe(200);
        expect(Date.parse(new URL(recentResponse.url()).searchParams.get("until")!)).toBeLessThanOrEqual(Date.now());
        await settled();
        await capture("recent-center", width);
        const selection = await page.evaluate(async () => {
          type Item = { root?: { uid?: string; namespace?: string }; root_uid?: string };
          const read = async (route: string, query: URLSearchParams) => {
            const response = await fetch(`/api/admin/kubernetes/${route}?${query}`);
            const body = await response.json();
            return { status: response.status, items: (Array.isArray(body.items) ? body.items : []) as Item[] };
          };
          // A normal current 24h range, ending now: never move the window to a
          // root's historical firstSeen/break or rewrite a server response.
          const until = new Date().toISOString();
          const since = new Date(Date.parse(until) - 24 * 3600_000).toISOString();
          const scope = new URLSearchParams({ namespace: "core-integration", since, until, limit: "100" });
          const [current, retained] = await Promise.all([
            read("issues", new URLSearchParams({ namespace: "core-integration", limit: "50" })),
            read("crashes", scope),
          ]);
          const attempts: Array<{ statuses: number[]; issues: number; changes: number; crashes: number }> = [];
          const result = { selected: false, availability: "unavailable-actual-data", candidates: 0, listStatuses: [current.status, retained.status], attempts };
          if (result.listStatuses.some((status) => status !== 200)) { result.availability = "read-failed"; return result; }
          const currentRoots = new Map(current.items.filter((item) => item.root?.uid).map((item) => [item.root!.uid!, item.root!]));
          const candidates = [...new Set(retained.items.map((item) => item.root_uid).filter((uid): uid is string => Boolean(uid && currentRoots.has(uid))))];
          result.candidates = candidates.length;
          for (const uid of candidates.slice(0, 5)) {
            const root = currentRoots.get(uid)!;
            const query = new URLSearchParams({ namespace: root.namespace ?? "core-integration", root_uid: uid, since, until, limit: "100" });
            const enriched = new URLSearchParams(query);
            enriched.set("include", "changes");
            const [issues, changes, crashes] = await Promise.all([read("issues", enriched), read("changes", query), read("crashes", query)]);
            const proof = { statuses: [issues.status, changes.status, crashes.status], issues: issues.items.filter((item) => item.root?.uid === uid).length, changes: changes.items.length, crashes: crashes.items.filter((item) => item.root_uid === uid).length };
            attempts.push(proof);
            if (proof.statuses.every((status) => status === 200) && proof.issues > 0 && proof.changes > 0 && proof.crashes > 0) {
              const win = window as unknown as { __acceptRoot?: { uid: string; namespace: string }; __redactTokens?: Set<string> };
              win.__acceptRoot = { uid, namespace: root.namespace ?? "core-integration" };
              win.__redactTokens ??= new Set();
              win.__redactTokens.add(uid);
              win.__redactTokens.add(root.namespace ?? "core-integration");
              result.selected = true;
              result.availability = "populated-current-retained";
              break;
            }
          }
          return result;
        });
        rootSelections.push({ width, ...selection });
        expect(selection.listStatuses).toEqual([200, 200]);
        expect(selection.selected, "Unavailable actual data unless a current authorized root has retained Issues, Changes and crash observations; never a populated PASS").toBe(true);
        const rootResponse = (route: string, hours: number) => page.waitForResponse((response) => {
          const url = new URL(response.url());
          const duration = Date.parse(url.searchParams.get("until") ?? "") - Date.parse(url.searchParams.get("since") ?? "");
          return url.pathname.endsWith(`/kubernetes/${route}`) && Boolean(url.searchParams.get("root_uid")) && Math.abs(duration - hours * 3600_000) < 1000;
        });
        for (const windowShape of ["valid", "empty"] as const) {
          // Install empty-window observers before navigation so a fast real
          // response cannot be missed or require a second historical request.
          const emptyIssues = windowShape === "empty" ? rootResponse("issues", 1) : null;
          const emptyCrashes = windowShape === "empty" ? rootResponse("crashes", 1) : null;
          // The valid view uses the actual current 24h UI control without a
          // center override. The deliberately empty view has its own old range.
          await page.evaluate((shape) => {
            const root = (window as unknown as { __acceptRoot: { uid: string; namespace: string } }).__acceptRoot;
            const query = new URLSearchParams({ view: "timeline", namespace: root.namespace, root_uid: root.uid });
            if (shape === "empty") query.set("center", "2000-01-01T00:00:00.000Z");
            history.pushState(null, "", `?${query}`);
            dispatchEvent(new PopStateEvent("popstate"));
          }, windowShape);
          const timeline = page.getByRole("region", { name: "Kubernetes change timeline", exact: true });
          await expect(timeline).toBeVisible();
          const pendingIssues = emptyIssues ?? rootResponse("issues", 24);
          const pendingCrashes = emptyCrashes ?? rootResponse("crashes", 24);
          const pendingChanges = windowShape === "valid" ? rootResponse("changes", 24) : null;
          if (windowShape === "valid") {
            await timeline.getByRole("group", { name: "Timeline window", exact: true }).getByRole("button", { name: "24h", exact: true }).click();
          }
          const [issueResponse, crashResponse, changeResponse] = await Promise.all([pendingIssues, pendingCrashes, pendingChanges]);
          const statuses = [issueResponse.status(), crashResponse.status(), ...(changeResponse ? [changeResponse.status()] : [])];
          expect(statuses).toEqual(windowShape === "valid" ? [200, 200, 200] : [200, 200]);
          const issues = (await issueResponse.json()).items?.length ?? 0;
          const crashes = (await crashResponse.json()).items?.length ?? 0;
          const changes = changeResponse ? (await changeResponse.json()).items?.length ?? 0 : 0;
          await settled();
          const journal = timeline.getByRole("region", { name: "Captured crash evidence", exact: true });
          await expect(journal).toBeVisible();
          await expect(journal.getByText("Loading captured crash evidence...", { exact: true })).toHaveCount(0, { timeout: 45_000 });
          const changeMarkers = timeline.locator('[data-marker="change"]');
          const crashRows = journal.getByRole("list", { name: "Crash observations", exact: true }).locator(":scope > li");
          if (windowShape === "valid") {
            expect(issues).toBeGreaterThan(0);
            expect(changes).toBeGreaterThan(0);
            expect(crashes).toBeGreaterThan(0);
            await expect(timeline.getByText("Root not found in this namespace.", { exact: true })).toHaveCount(0);
            await expect.poll(() => changeMarkers.count()).toBeGreaterThan(0);
            await expect.poll(() => crashRows.count()).toBeGreaterThan(0);
          } else {
            expect(issues).toBe(0);
            expect(crashes).toBe(0);
            await expect(timeline.getByText("Root not found in this namespace.", { exact: true })).toBeVisible();
            await expect(journal.getByText("No captured observations on this page.", { exact: true })).toBeVisible();
          }
          rootWindows.push({ width, shape: windowShape, statuses, issues, changes, crashes, renderedChanges: await changeMarkers.count(), renderedCrashes: await crashRows.count() });
          await capture(`root-${windowShape}`, width);
        }
      }
      await page.setViewportSize({ width: 1440, height: 900 });
      await page.goto("/agent/kubernetes");
      await expect(page.getByRole("region", { name: "Cluster health", exact: true })).toBeVisible();
      await waitForExternalOverview(page);
      const marker = await page.evaluate(async () => (await fetch("/api/admin/kubernetes/sync").then((response) => response.json())).first_ready_at);
      expect(Boolean(marker)).toBe(true);
      const beforeRestart = reads.length;
      let sampling = false;
      quietTimer = setInterval(() => {
        if (sampling) return;
        sampling = true;
        quietRead = page.evaluate(() => ({ preparing: Boolean(document.querySelector("[data-preparing-panel]")), redInventory: [...document.querySelectorAll("[role=alert], .text-sev-critical, .text-sev-warning")].some((element) => /inventory_not_ready/.test(element.textContent ?? "")) }))
          .then((sample) => { quietSamples.push(sample); })
          .catch(() => { quietSamplingErrors += 1; })
          .finally(() => { sampling = false; });
      }, 250);
      restartStarted = true;
      await execute("/bin/bash", [path.join(workspace, "plans/harness-run/scripts/eks-readonly.sh"), "--restart"], { timeout: 60_000 });
      restartCompleted = true;
      await expect.poll(async () => {
        try { return (await context.request.get("/healthz", { timeout: 3000 })).status(); } catch { return 0; }
      }, { timeout: 60_000, intervals: [500] }).toBe(200);
      // Keep the original page alive: no reload, Retry, refetch, synthetic
      // response or manual cache invalidation may produce this recovery.
      await expect.poll(() => {
        const after = reads.slice(beforeRestart);
        return after.some((read) => read.route === "overview" && read.status === 200)
          && after.some((read) => read.route === "issues" && read.enriched && read.status === 200);
      }, { timeout: 90_000, intervals: [500] }).toBe(true);
      await waitForExternalOverview(page);
      restartEnrichedReturned = true;
      expect(restartObserved503, "The live restart 503 state must actually be observed, not inferred").toBe(true);
      expect(quietSamples.length).toBeGreaterThan(0);
      expect(quietSamples.some((sample) => sample.preparing || sample.redInventory)).toBe(false);
      expect(quietSamplingErrors).toBe(0);
      retainedMarker = await page.evaluate(async (before) => (await fetch("/api/admin/kubernetes/sync").then((response) => response.json())).first_ready_at === before, marker);
      expect(retainedMarker).toBe(true);
      await capture("published-restart", 1440);
      await page.setViewportSize({ width: 390, height: 844 });
      await waitForExternalOverview(page);
      await capture("published-restart", 390);
      expect(reads.filter((read) => [400, 401, 403, 500, 502].includes(read.status))).toEqual([]);
      expect(reads.some((read) => read.future)).toBe(false);
      expect(mutations).toEqual([]);
      expect(pageErrors).toEqual([]);
    } finally {
      if (quietTimer) clearInterval(quietTimer);
      await quietRead;
      if (process.env.HARNESS_RESULTS_DIR) fs.writeFileSync(path.join(process.env.HARNESS_RESULTS_DIR, "eks-focused-history-restart.json"), JSON.stringify({ classification: "live", checkpoints, geometry, rootSelections, rootWindows, quietSamples, quietSamplingErrors, reads, images, mutatingRequests: mutations.length, pageErrors, initialPublished, retainedMarker, restartCompleted, restartObserved503, restartEnrichedReturned, firstMarkerAbsent, firstPreparationVerified, reportPendingPlaceholder: "unverified unless independently captured; no intercepted timing state" }, null, 2), { mode: 0o600 });
      await context.close();
    }
  });
});
