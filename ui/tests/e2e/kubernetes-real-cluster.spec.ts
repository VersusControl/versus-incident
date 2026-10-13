import { expect, test, type Page } from "@playwright/test";
import * as fs from "node:fs";
import * as path from "node:path";
import { fileURLToPath } from "node:url";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { env, openApp } from "./helpers";
import type { AgentApproval, ChatSession, ChatSessionSummary, KubernetesChangesPage } from "../../src/lib/api";

// Runs against the harness kind cluster:
//   plans/harness-run/harness.sh up oss-kind
//   plans/harness-run/harness.sh e2e ui kubernetes-real-cluster.spec.ts --project=chromium
// Fixtures live in plans/harness-run/config/k8s/fixtures.yaml.

const screenshotDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "screenshots", "kubernetes");
const canary = "harness-canary-7f3a";
const cluster = process.env.E2E_KIND_CLUSTER ?? "versus-harness";
const workerNode = `${cluster}-worker`;
const backend = process.env.E2E_KUBERNETES_BACKEND;
const execute = promisify(execFile);
const workspace = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../../..");

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