import { expect, test, type Page } from "@playwright/test";
import * as fs from "node:fs";
import * as path from "node:path";
import { fileURLToPath } from "node:url";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { createHash } from "node:crypto";
import { env, openApp } from "./helpers";
import type { AgentApproval, ChatSession, ChatSessionSummary, KubernetesChangesPage } from "../../src/lib/api";

// Runs against the harness kind cluster:
//   plans/harness-run/harness.sh up oss-kind
//   plans/harness-run/harness.sh e2e ui kubernetes-real-cluster.spec.ts --project=chromium
// Fixtures live in plans/harness-run/config/k8s/fixtures.yaml.

const screenshotDir = process.env.E2E_KUBERNETES_CAPTURE_DIR
  ? path.resolve(process.env.E2E_KUBERNETES_CAPTURE_DIR)
  : path.resolve(path.dirname(fileURLToPath(import.meta.url)), "screenshots", "kubernetes");
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
    const script = path.join(workspace, "plans/harness-run/tests/oss-to-enterprise-upgrade.sh");
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