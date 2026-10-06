import { expect, test, type Page } from "@playwright/test";
import * as fs from "node:fs";
import * as path from "node:path";
import { fileURLToPath } from "node:url";
import { openApp } from "./helpers";

// Runs against the harness kind cluster:
//   plans/harness-run/harness.sh up oss-kind
//   plans/harness-run/harness.sh e2e ui kubernetes-real-cluster.spec.ts --project=chromium
// Fixtures live in plans/harness-run/config/k8s/fixtures.yaml.

const screenshotDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "screenshots", "kubernetes");
const canary = "harness-canary-7f3a";
const cluster = process.env.E2E_KIND_CLUSTER ?? "versus-harness";
const workerNode = `${cluster}-worker`;
const backend = process.env.E2E_KUBERNETES_BACKEND;

async function shot(page: Page, name: string) {
  fs.mkdirSync(screenshotDir, { recursive: true });
  await page.screenshot({ path: path.join(screenshotDir, name), fullPage: true, animations: "disabled" });
}

async function openKubernetes(page: Page) {
  await openApp(page, "/agent/kubernetes");
  await expect(page.getByRole("heading", { name: "Kubernetes" })).toBeVisible();
  await expect(page.getByText(/Nodes ready 3\/3/)).toBeVisible({ timeout: 60_000 });
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
    await expect(page.getByText(/Nodes ready 3\/3/)).toBeVisible({ timeout: 60_000 });
    await expect(page.getByRole("region", { name: "Cluster health" })).toBeVisible();
    await expect(page.getByRole("heading", { name: "Workloads" })).toBeVisible();
    await shot(page, "overview-desktop.png");
    expect(pageErrors).toEqual([]);
  });

  test("healthy workload detail and node drill-down", async ({ page }) => {
    await openKubernetes(page);
    await page.getByLabel("Workload namespace").selectOption("shop");
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

    const nodes = page.getByRole("region", { name: "Nodes" });
    await nodes.getByRole("button", { name: `View pods on ${workerNode}`, exact: true }).click();
    await expect(nodes.getByRole("list", { name: `Pods on ${workerNode}`, exact: true })).toContainText("node-agent");
    await shot(page, "node-pods-desktop.png");
  });

  test("unhealthy fixtures stay visible and secrets never cross the API", async ({ page }) => {
    await openKubernetes(page);
    await page.getByLabel("Workload namespace").selectOption("payments");
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
    await expect(page.getByRole("region", { name: "Nodes" })).toBeVisible();
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth);
    expect(overflow).toBe(false);
    await shot(page, "overview-mobile.png");
  });
});