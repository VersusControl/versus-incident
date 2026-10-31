import { expect, test, type Locator, type Page } from "@playwright/test";
import * as fs from "node:fs";
import * as path from "node:path";
import * as https from "node:https";
import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
import { fileURLToPath, pathToFileURL } from "node:url";
import { openApp } from "./helpers";
import { captureDir, capturePath, harnessRuntimeStateDir } from "./support/capture";
import type { KubernetesAgentSettings, KubernetesBreakLogPage, KubernetesClusters, KubernetesCrashEvent, KubernetesCrashPage, KubernetesGraph, KubernetesIssue, KubernetesOverview, KubernetesResourcePage, KubernetesTopPage, KubernetesTraffic, KubernetesUsage, RbacMe, SSOSession } from "../../src/lib/api";

type StreamCounter = { path: string; container: string; previous: boolean; follow: boolean; requests: number; active: number; cancelled: number; completed: number; disconnected: number; unavailable: number; chunks: number; since_time?: string };

const streamCounters = (cluster?: string) => new Promise<StreamCounter[]>((resolve, reject) => {
  const state = harnessRuntimeStateDir();
  const port = Number(process.env.HARNESS_FAKEKUBE_PORT || 19443);
  const observerPort = cluster === "west" ? Number(process.env.HARNESS_FAKEKUBE_SECOND_PORT || port + 1) : port;
  const ca = path.join(state, cluster ? `fakekube-multi/${cluster}/ca.crt` : "fakekube/ca.crt");
  const request = https.get(`https://localhost:${observerPort}/_fake/log-streams`, { ca: fs.readFileSync(ca), timeout: 5000 }, (response) => {
    let body = "";
    response.on("data", (chunk) => { body += chunk; });
    response.on("end", () => {
      if (response.statusCode !== 200) return reject(new Error(`observer status ${response.statusCode}`));
      try { resolve(JSON.parse(body)); } catch (failure) { reject(failure); }
    });
    response.on("error", reject);
  });
  request.on("timeout", () => request.destroy(new Error("observer timeout")));
  request.on("error", reject);
});

async function expectStreamCancelled(before: StreamCounter, read: () => Promise<StreamCounter | undefined>) {
  await expect.poll(async () => {
    const after = await read();
    if (!after) return false;
    expect([after.path, after.container, after.previous, after.follow]).toEqual([before.path, before.container, before.previous, before.follow]);
    return after.active === 0 && after.cancelled - before.cancelled >= before.active + after.requests - before.requests;
  }, { timeout: 15_000, intervals: [100, 250, 500, 1000] }).toBe(true);
}

type LicensedIdentity = { id: string; subject: string; role: string; clusters: string[]; expected_mfa: boolean; login_url: string };
type LicensedContract = { configured: boolean; base_url: string; org: string; identities: LicensedIdentity[] };
type LicensedBrowserHelper = {
  loadBrowserFixture: (file: string) => Promise<unknown>;
  loginLicensedIdentity: (page: Page, contract: LicensedContract, fixture: unknown, id: string) => Promise<void>;
};

for (const viewport of [{ name: "desktop", width: 1440, height: 1000 }, { name: "mobile", width: 390, height: 844 }]) {
  test.describe(`Kubernetes licensed SSO acceptance ${viewport.name}`, () => {
    test.skip(process.env.E2E_KUBERNETES_LICENSED !== "1", "requires the real licensed Keycloak browser fixture");
    let contract: LicensedContract;
    let helper: LicensedBrowserHelper;
    let browserFixture: unknown;

    test.beforeAll(async () => {
      const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../../..");
      const state = harnessRuntimeStateDir();
      contract = JSON.parse(fs.readFileSync(path.join(state, "licensed-kubernetes.json"), "utf8"));
      expect(contract.configured, "a completed real Enterprise bootstrap is required").toBe(true);
      expect(contract.org).toBeTruthy();
      expect(contract.identities.map((identity) => identity.id).sort()).toEqual(["all", "deny", "east", "non-mfa", "viewer", "west"]);
      const browserFile = process.env.HARNESS_KUBERNETES_BROWSER_FILE;
      if (!browserFile) throw new Error("HARNESS_KUBERNETES_BROWSER_FILE must point to the private mode-0600 browser fixture");
      helper = await import(pathToFileURL(path.join(root, "plans/harness-run/tests/enterprise/licensed-keycloak-browser.mjs")).href) as LicensedBrowserHelper;
      browserFixture = await helper.loadBrowserFixture(browserFile);
    });

    for (const id of ["east", "west", "all", "deny", "viewer", "non-mfa"]) {
      test(`real ${id} session fleet, dashboard, and Pod stream authorization`, async ({ browser }, testInfo) => {
        test.setTimeout(180_000);
        const identity = contract.identities.find((candidate) => candidate.id === id)!;
        const context = await browser.newContext({ baseURL: contract.base_url, viewport: { width: viewport.width, height: viewport.height } });
        const page = await context.newPage();
        const org = encodeURIComponent(contract.org);
        const kubernetesRequests: URL[] = [];
        page.on("request", (request) => {
          const url = new URL(request.url());
          if (url.pathname.startsWith("/api/admin/kubernetes/")) kubernetesRequests.push(url);
        });
        try {
          await helper.loginLicensedIdentity(page, contract, browserFixture, id);
          const sessionResponse = await context.request.get(`/enterprise/api/sso/${org}/session`);
          expect(sessionResponse.status()).toBe(200);
          const session: SSOSession = await sessionResponse.json();
          expect(session.subject).toBe(identity.subject);
          expect(session.org).toBe(contract.org);
          expect(session.local ?? false).toBe(false);
          expect(session.mfa).toBe(identity.expected_mfa);
          expect([...(session.amr ?? [])].sort()).toEqual(identity.expected_mfa ? ["otp", "pwd"] : ["pwd"]);
          await testInfo.attach(`session-${id}-${viewport.name}`, { body: JSON.stringify({ id, org: session.org, mfa: session.mfa, amr: session.amr }), contentType: "application/json" });
          const meResponse = await context.request.get("/enterprise/api/rbac/me");
          let me: RbacMe | undefined;
          if (id === "non-mfa") {
            expect(meResponse.status()).toBe(403);
            expect(await meResponse.text()).toMatch(/mfa/i);
          } else {
            expect(meResponse.status()).toBe(200);
            me = await meResponse.json();
            expect(me!.role).toBe(identity.role);
            expect([...(me!.clusters ?? [])].sort()).toEqual(identity.role === "viewer" ? [] : [...identity.clusters].sort());
          }
          const cookie = (await context.cookies(contract.base_url)).find((entry) => entry.name === "versus_enterprise_session");
          expect(Boolean(cookie?.value)).toBe(true);
          expect(cookie?.httpOnly).toBe(true);
          const allowed = id === "east" ? ["east"] : id === "west" ? ["west"] : ["east", "west", "unreachable"];
          const noInfrastructure = id === "deny" || id === "viewer";
          const bootstrapResponse = await context.request.get("/api/v1/agent/bootstrap");
          expect(bootstrapResponse.status()).toBe(200);
          const bootstrap = await bootstrapResponse.json();
          if (noInfrastructure || id === "non-mfa") {
            expect(bootstrap.kubernetes).toBeUndefined();
          } else {
            expect(bootstrap.kubernetes.multiple).toBe(true);
            expect(bootstrap.kubernetes.clusters.map((cluster: { id: string }) => cluster.id).sort()).toEqual([...allowed].sort());
          }
          for (const agent of ["chat", "analyze"]) {
            const toolsetsResponse = await context.request.get(`/api/admin/agent/toolsets?agent=${agent}`);
            expect(toolsetsResponse.status()).toBe(200);
            const toolsets: { id: string; state: string }[] = await toolsetsResponse.json();
            const kubernetes = toolsets.find((toolset) => toolset.id === "kubernetes");
            expect(kubernetes).toBeTruthy();
            expect(kubernetes!.state).toBe(noInfrastructure || id === "non-mfa" ? "needs_permission" : "available");
          }
          if (noInfrastructure) {
            expect(me!.role).toBe("viewer");
            expect(me!.permissions).not.toContain("infrastructure:view");
          } else if (me) expect(me.permissions).toContain("infrastructure:view");
          const catalogResponse = await context.request.get("/api/admin/kubernetes/clusters");
          if (noInfrastructure || id === "non-mfa") {
            expect(catalogResponse.status()).toBe(403);
            if (id === "non-mfa") expect(await catalogResponse.text()).toMatch(/mfa/i);
            for (const cluster of ["east", "west"]) {
              expect((await context.request.get(`/api/admin/kubernetes/overview?cluster=${cluster}`)).status()).toBe(403);
              const namespace = cluster === "east" ? "east-shop" : "west-billing";
              const pod = cluster === "east" ? "east-api-0" : "west-worker-0";
              const before = (await streamCounters(cluster)).filter((counter) => counter.path === `/api/v1/namespaces/${namespace}/pods/${pod}/log` && counter.container === "api" && counter.follow && !counter.previous);
              const denied = await context.request.get(`/api/admin/kubernetes/pods/${namespace}/${pod}/logs/stream?cluster=${cluster}&container=api`);
              expect(denied.status()).toBe(403);
              if (id === "non-mfa") expect(await denied.text()).toMatch(/mfa/i);
              expect((await streamCounters(cluster)).filter((counter) => counter.path === `/api/v1/namespaces/${namespace}/pods/${pod}/log` && counter.container === "api" && counter.follow && !counter.previous)).toEqual(before);
            }
            await page.goto("/agent/kubernetes?cluster=east");
            await expect(page.getByRole("article", { name: /^Cluster / })).toHaveCount(0);
            await expect(page.getByRole("region", { name: "Cluster health", exact: true })).toHaveCount(0);
            await expect(page.getByRole("region", { name: "Pod logs", exact: true })).toHaveCount(0);
          } else {
            expect(catalogResponse.status()).toBe(200);
            const catalog: KubernetesClusters = await catalogResponse.json();
            expect(catalog.clusters.map((cluster) => cluster.id).sort()).toEqual([...allowed].sort());
            if (id === "all") expect(catalog.clusters.find((cluster) => cluster.id === "unreachable")?.health).toBe("unreachable");
            await expect(page.getByRole("article", { name: /^Cluster / })).toHaveCount(allowed.length);
            for (const cluster of catalog.clusters) await expect(page.getByRole("article", { name: `Cluster ${cluster.id}`, exact: true })).toBeVisible();
            for (const cluster of ["east", "west"]) {
              if (!allowed.includes(cluster)) {
                expect((await context.request.get(`/api/admin/kubernetes/overview?cluster=${cluster}`)).status()).toBe(404);
                const namespace = cluster === "east" ? "east-shop" : "west-billing";
                const pod = cluster === "east" ? "east-api-0" : "west-worker-0";
                const logPath = `/api/v1/namespaces/${namespace}/pods/${pod}/log`;
                const before = (await streamCounters(cluster)).filter((counter) => counter.path === logPath && counter.container === "api" && counter.follow && !counter.previous);
                expect((await context.request.get(`/api/admin/kubernetes/pods/${namespace}/${pod}/logs/stream?cluster=${cluster}&container=api`)).status()).toBe(404);
                expect((await streamCounters(cluster)).filter((counter) => counter.path === logPath && counter.container === "api" && counter.follow && !counter.previous)).toEqual(before);
                await page.goto(`/agent/kubernetes?cluster=${cluster}`);
                await expect(page.getByText("This cluster is no longer available.", { exact: true })).toBeVisible();
                await expect(page.getByRole("region", { name: "Cluster health", exact: true })).toHaveCount(0);
                await expect(page.getByRole("article", { name: `Cluster ${cluster}`, exact: true })).toHaveCount(0);
                continue;
              }
              const overviewResponse = await context.request.get(`/api/admin/kubernetes/overview?cluster=${cluster}`);
              expect(overviewResponse.status()).toBe(200);
              const overview: KubernetesOverview = await overviewResponse.json();
              expect(overview.cluster_id).toBe(cluster);
              expect(overview.nodes).toBe(cluster === "east" ? 1 : 2);
              expect(overview.pods).toBe(cluster === "east" ? 2 : 3);
              await page.goto(`/agent/kubernetes?cluster=${cluster}`);
              await expect(page.getByRole("combobox", { name: "Cluster", exact: true })).toHaveValue(cluster);
              await expect(page.getByRole("region", { name: "Cluster health", exact: true })).toBeVisible();
              const podsResponse = await context.request.get(`/api/admin/kubernetes/resources?resource_id=core~v1~pods&limit=20&cluster=${cluster}`);
              expect(podsResponse.status()).toBe(200);
              const pods: KubernetesResourcePage = await podsResponse.json();
              const pod = pods.items?.find((item) => item.namespace);
              expect(pod?.namespace).toBe(cluster === "east" ? "east-shop" : "west-billing");
              expect(pod?.name).toMatch(cluster === "east" ? /^east-api/ : /^west-worker/);
              const logPath = `/api/v1/namespaces/${pod!.namespace}/pods/${pod!.name}/log`;
              const query = new URLSearchParams({ cluster, view: "resources", tab: "logs", r: `${pod!.resource_id}/${pod!.namespace}/${pod!.name}` });
              const before = await streamCounters(cluster);
              const streamed = page.waitForResponse((response) => {
                const url = new URL(response.url());
                return url.pathname === `/api/admin/kubernetes/pods/${pod!.namespace}/${pod!.name}/logs/stream` && url.searchParams.get("cluster") === cluster;
              });
              await page.goto(`/agent/kubernetes?${query}`);
              const response = await streamed;
              expect(response.status()).toBe(200);
              expect(response.headers()["content-type"]).toMatch(/^text\/event-stream(?:;|$)/);
              const container = new URL(response.url()).searchParams.get("container");
              expect(container).toBeTruthy();
              const current = async () => (await streamCounters(cluster)).find((counter) => counter.path === logPath && counter.container === container && counter.follow && !counter.previous);
              const baseline = before.find((counter) => counter.path === logPath && counter.container === container && counter.follow && !counter.previous);
              const logs = page.getByRole("region", { name: "Pod logs", exact: true });
              await expect(logs.getByLabel("Log connection status")).toHaveText("live");
              await expect(logs.getByLabel("Pod log output")).toContainText(`container=${container}`);
              await expect(logs.getByLabel("Pod log output")).toContainText("REDACTED");
              await expect(logs.getByLabel("Pod log output")).not.toContainText(/(?:super|split|previous|synthetic-oversized)-secret-value/);
              await expect(logs.getByLabel("Pod log output")).toContainText("fakekube follow line 30");
              await expect.poll(async () => (await current())?.active).toBe(1);
              const opened = (await current())!;
              expect(opened.requests).toBeGreaterThan(baseline?.requests ?? 0);
              if (viewport.name === "mobile") await logs.getByRole("button", { name: "More", exact: true }).click();
              await logs.getByLabel("Pause logs").click();
              await expect(logs.getByLabel("Log connection status")).toHaveText("paused");
              await expectStreamCancelled(opened, current);
              const paused = (await current())!;
              const resumeResponse = page.waitForResponse((candidate) => {
                const url = new URL(candidate.url());
                return url.pathname === `/api/admin/kubernetes/pods/${pod!.namespace}/${pod!.name}/logs/stream` && url.searchParams.get("cluster") === cluster;
              });
              await logs.getByLabel("Resume logs").click();
              const resumedResponse = await resumeResponse;
              await testInfo.attach(`resume-${cluster}-${viewport.name}`, { body: JSON.stringify({ status: resumedResponse.status(), cluster, container, cursor_present: new URL(resumedResponse.url()).searchParams.has("cursor") }), contentType: "application/json" });
              expect(resumedResponse.status()).toBe(200);
              await expect.poll(async () => (await current())?.requests ?? 0).toBeGreaterThan(paused.requests);
              await expect.poll(async () => (await current())?.active).toBe(1);
              const resumed = (await current())!;
              await page.getByRole("dialog", { name: "Details panel" }).getByRole("button", { name: "Close panel" }).click();
              await expect(logs).toHaveCount(0);
              await expectStreamCancelled(resumed, current);
            }
            expect(kubernetesRequests.filter((url) => !url.pathname.endsWith("/clusters")).every((url) => allowed.includes(url.searchParams.get("cluster") ?? ""))).toBe(true);
          }
          expect(await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth)).toBe(false);
          await testInfo.attach(`licensed-${id}-${viewport.name}`, { body: JSON.stringify({ id, org: session.org, role: me?.role, mfa: session.mfa, amr: session.amr, clusters: me?.clusters }), contentType: "application/json" });
        } catch (failure) {
          if (page.url().startsWith(contract.base_url)) {
            await testInfo.attach(`licensed-failure-${id}-${viewport.name}`, { body: await page.screenshot({ fullPage: true }), contentType: "image/png" });
            await testInfo.attach(`licensed-status-${id}-${viewport.name}`, { body: JSON.stringify({ status: await page.getByLabel("Log connection status").allTextContents(), alerts: await page.getByRole("alert").allTextContents() }), contentType: "application/json" });
          }
          throw failure;
        } finally {
          await page.goto("about:blank");
          try { await context.request.post(`/enterprise/api/sso/${org}/logout`); } finally { await context.close(); }
        }
      });
    }
  });
}

for (const viewport of [{ name: "desktop", width: 1440, height: 900 }, { name: "mobile", width: 390, height: 844 }].flatMap((size) =>
  (["light", "dark"] as const).map((theme) => ({ ...size, theme })))) {
  test.describe(`Kubernetes multi-cluster live ${viewport.name} ${viewport.theme}`, () => {
    test.use({ viewport: { width: viewport.width, height: viewport.height } });
    test.skip(process.env.E2E_KUBERNETES_MULTIPLE !== "1", "requires the live multi-cluster harness fixture");
    test.beforeEach(async ({ page }) => {
      await page.emulateMedia({ colorScheme: viewport.theme });
      await page.addInitScript((theme) => localStorage.setItem("versus.theme", theme), viewport.theme);
    });

    test("fleet cards, switching, deep links, and scoped reads", async ({ page }) => {
      const requests: string[] = [];
      page.on("request", (request) => {
        if (new URL(request.url()).pathname.startsWith("/api/admin/kubernetes/")) requests.push(request.url());
      });
      await openApp(page, "/agent/kubernetes");
      await expect.poll(() => page.locator("html").getAttribute("data-theme")).toBe(viewport.theme === "light" ? "light" : null);
      const catalogResponse = await page.request.get("/api/admin/kubernetes/clusters");
      expect(catalogResponse.status()).toBe(200);
      const catalog: KubernetesClusters = await catalogResponse.json();
      expect(catalog.multiple).toBe(true);
      expect(catalog.clusters.length).toBeGreaterThanOrEqual(3);
      const reachable = catalog.clusters.filter((cluster) => cluster.health !== "unreachable");
      expect(reachable.length).toBeGreaterThanOrEqual(2);
      const [first, second] = reachable;
      const offline = catalog.clusters.find((cluster) => cluster.health === "unreachable")!;
      expect(offline).toBeTruthy();
      for (const cluster of catalog.clusters) await expect(page.getByRole("article", { name: `Cluster ${cluster.id}`, exact: true })).toBeVisible();
      await expect(page.getByRole("article", { name: `Cluster ${offline.id}`, exact: true }).getByRole("button", { name: "Retry" })).toBeVisible();
      expect(requests.every((url) => new URL(url).pathname.endsWith("/clusters"))).toBe(true);
      if (process.env.E2E_KUBERNETES_CAPTURE_DIR) {
        expect(await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth)).toBe(false);
        const rows = page.getByRole("region", { name: "Cluster rows", exact: true });
        await expect(rows).toHaveAttribute("tabindex", "0");
        const geometry = await rows.evaluate((region) => {
          const header = region.firstElementChild!.firstElementChild!;
          const columns = Array.from(header.children).map((cell) => cell.getBoundingClientRect());
          const articles = Array.from(region.querySelectorAll("article"));
          return {
            scrollable: region.scrollWidth > region.clientWidth,
            overflow: getComputedStyle(region).overflowX,
            minimumWidth: region.firstElementChild!.getBoundingClientRect().width,
            aligned: articles.every((article) => {
              const bounds = article.getBoundingClientRect();
              const cells = Array.from(article.children).filter((cell) => getComputedStyle(cell).position !== "absolute");
              return cells.length === 7 && cells.every((cell, index) => {
                const rect = cell.getBoundingClientRect();
                return Math.abs(rect.left - columns[index].left) <= 2 && rect.right <= bounds.right && rect.top >= bounds.top && rect.bottom <= bounds.bottom &&
                  Array.from(cell.querySelectorAll("a, button, p, h2, meter")).every((content) => {
                    const contentRect = content.getBoundingClientRect();
                    return contentRect.left >= rect.left - 1 && contentRect.right <= rect.right + 1 && contentRect.top >= bounds.top && contentRect.bottom <= bounds.bottom;
                  });
              });
            }),
            stacked: articles.every((article, index) => {
              const bounds = article.getBoundingClientRect();
              const first = articles[0].getBoundingClientRect();
              return bounds.left === first.left && bounds.width === first.width && (!index || bounds.top >= articles[index - 1].getBoundingClientRect().bottom);
            }),
          };
        });
        expect(geometry.minimumWidth).toBeGreaterThanOrEqual(1160);
        expect(geometry.aligned).toBe(true);
        expect(geometry.stacked).toBe(true);
        expect(geometry.overflow).toBe("auto");
        if (viewport.name === "mobile") {
          expect(geometry.scrollable).toBe(true);
          await rows.focus();
          await expect(rows).toBeFocused();
          await rows.evaluate((region) => { region.scrollLeft = region.scrollWidth; });
          expect(await rows.evaluate((region) => region.scrollLeft)).toBeGreaterThan(0);
          await rows.evaluate((region) => { region.scrollLeft = 0; });
        }
        fs.mkdirSync(screenshotDir, { recursive: true });
        await page.screenshot({ path: path.join(screenshotDir, `fleet-${viewport.name}-${viewport.theme}.png`), fullPage: true, animations: "disabled" });
        if (viewport.name === "desktop") {
          await page.setViewportSize({ width: 1600, height: viewport.height });
          expect(await rows.evaluate((region) => region.scrollWidth > region.clientWidth)).toBe(false);
          await page.screenshot({ path: path.join(screenshotDir, `fleet-rows-wide-desktop-${viewport.theme}.png`), fullPage: true, animations: "disabled" });
          await page.setViewportSize({ width: viewport.width, height: viewport.height });
        }
      }
      await page.getByRole("link", { name: `Open cluster ${first.display_name || first.id}`, exact: true }).click();
      await expect(page.getByRole("combobox", { name: "Cluster", exact: true })).toHaveValue(first.id);
      await expect(page.getByRole("region", { name: "Cluster health", exact: true })).toBeVisible();
      if (process.env.E2E_KUBERNETES_CAPTURE_DIR) {
        await expect(page.getByRole("region", { name: "Break timeline", exact: true }).getByRole("button", { name: /^Broken / })).toBeVisible();
        await expect(page.getByRole("status").filter({ hasText: /Loading/i })).toHaveCount(0);
        expect(await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth)).toBe(false);
        await page.screenshot({ path: path.join(screenshotDir, `cluster-dashboard-${viewport.name}-${viewport.theme}.png`), fullPage: true, animations: "disabled" });
      }
      const diagnostics = page.getByRole("region", { name: "Break timeline", exact: true });
      const firstWarnings = diagnostics.getByRole("button", { name: /^Warnings \(\d+\)$/ });
      await expect(firstWarnings).toHaveAttribute("aria-expanded", "false");
      await firstWarnings.click();
      await expect(firstWarnings).toHaveAttribute("aria-expanded", "true");
      await page.getByRole("combobox", { name: "Cluster", exact: true }).selectOption(second.id);
      await expect(page.getByRole("combobox", { name: "Cluster", exact: true })).toHaveValue(second.id);
      await expect(diagnostics.getByRole("button", { name: /^Warnings \(\d+\)$/ })).toHaveAttribute("aria-expanded", "false");
      await page.getByRole("combobox", { name: "Cluster", exact: true }).selectOption(first.id);
      await expect(page.getByRole("combobox", { name: "Cluster", exact: true })).toHaveValue(first.id);
      await expect(diagnostics.getByRole("button", { name: /^Warnings \(\d+\)$/ })).toHaveAttribute("aria-expanded", "false");
      await page.getByRole("tab", { name: "Nodes", exact: true }).click();
      const switched = page.waitForResponse((response) => new URL(response.url()).pathname.endsWith("/overview") && new URL(response.url()).searchParams.get("cluster") === second.id);
      await page.getByRole("combobox", { name: "Cluster", exact: true }).selectOption(second.id);
      await page.getByRole("button", { name: "Refresh Kubernetes data", exact: true }).click();
      expect((await (await switched).json()).cluster_id).toBe(second.id);
      await expect(page).toHaveURL(new RegExp(`cluster=${second.id}.*view=nodes`));
      await expect(page.getByRole("region", { name: "Nodes", exact: true })).toBeVisible();
      await page.reload();
      await expect(page.getByRole("combobox", { name: "Cluster", exact: true })).toHaveValue(second.id);
      await page.getByRole("navigation", { name: "Cluster breadcrumb" }).getByRole("link", { name: "Clusters", exact: true }).click();
      await expect(page.getByRole("article", { name: `Cluster ${first.id}`, exact: true })).toBeVisible();
      for (const request of requests.filter((url) => !new URL(url).pathname.endsWith("/clusters"))) expect([first.id, second.id]).toContain(new URL(request).searchParams.get("cluster"));
      expect(await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth)).toBe(false);
    });

    test("Pod log streams stay cluster scoped and switch/close cancel upstream", async ({ page }, testInfo) => {
      test.setTimeout(120_000);
      await openApp(page, "/agent/kubernetes");
      const catalog: KubernetesClusters = await (await page.request.get("/api/admin/kubernetes/clusters")).json();
      const reachable = catalog.clusters.filter((entry) => entry.health !== "unreachable");
      expect(reachable.length).toBeGreaterThanOrEqual(2);
      const [first, second] = reachable;
      for (const cluster of [first, second]) {
        expect(["east", "west"]).toContain(cluster.id);
        const response = await page.request.get(`/api/admin/kubernetes/resources?resource_id=core~v1~pods&limit=20&cluster=${encodeURIComponent(cluster.id)}`);
        expect(response.status()).toBe(200);
        const pods: KubernetesResourcePage = await response.json();
        const pod = pods.items?.find((item) => item.namespace);
        expect(pod, "each live cluster must have an attachable Pod").toBeTruthy();
        const query = new URLSearchParams({ cluster: cluster.id, view: "resources", tab: "logs", r: `${pod!.resource_id}/${pod!.namespace}/${pod!.name}` });
        const next = cluster.id === first.id ? second : first;
        const logPath = `/api/v1/namespaces/${pod!.namespace}/pods/${pod!.name}/log`;
        for (const action of ["switch", "close"] as const) {
          const before = await streamCounters(cluster.id);
          const otherBefore = await streamCounters(next.id);
          const active = async () => (await streamCounters(cluster.id)).filter((counter) => counter.path === logPath).reduce((total, counter) => total + counter.active, 0);
          expect(await active()).toBe(0);
          try {
            const stream = page.waitForResponse((response) => {
              const url = new URL(response.url());
              return url.pathname === `/api/admin/kubernetes/pods/${pod!.namespace}/${pod!.name}/logs/stream` && url.searchParams.get("cluster") === cluster.id;
            });
            await page.goto(`/agent/kubernetes?${query}`);
            const streamed = await stream;
            expect(streamed.status()).toBe(200);
            expect(streamed.headers()["content-type"]).toMatch(/^text\/event-stream(?:;|$)/);
            const container = new URL(streamed.url()).searchParams.get("container");
            expect(container).toBeTruthy();
            const current = async () => (await streamCounters(cluster.id)).find((counter) => counter.path === logPath && counter.container === container && counter.follow && !counter.previous);
            const baseline = before.find((counter) => counter.path === logPath && counter.container === container && counter.follow && !counter.previous);
            const logs = page.getByRole("region", { name: "Pod logs", exact: true });
            await expect(logs).toBeVisible();
            await expect(logs.getByLabel("Log connection status")).toHaveText("live");
            const output = logs.getByLabel("Pod log output");
            await expect(output).toContainText("fakekube follow line");
            await expect(output).toContainText(`container=${container}`);
            const lineCount = await output.locator("[data-log-key]").count();
            await expect.poll(() => output.locator("[data-log-key]").count()).toBeGreaterThan(lineCount);
            await expect(output).toContainText("REDACTED");
            await expect(output).not.toContainText(/(?:super|split|previous|synthetic-oversized)-secret-value|synthetic-record-padding/);
            await expect(logs.getByRole("alert")).toHaveCount(0);
            await expect(logs.getByLabel("Log connection status")).toHaveText("live");
            await expect.poll(active).toBe(1);
            const opened = (await current())!;
            expect(opened.requests).toBeGreaterThan(baseline?.requests ?? 0);
            expect(opened.chunks).toBeGreaterThan(baseline?.chunks ?? 0);
            expect(await streamCounters(next.id)).toEqual(otherBefore);
            if (action === "switch") await page.getByRole("combobox", { name: "Cluster", exact: true }).selectOption(next.id);
            else await page.getByRole("dialog", { name: "Details panel" }).getByRole("button", { name: "Close panel" }).click();
            await expect(logs).toHaveCount(0);
            await expect.poll(active).toBe(0);
            await expect.poll(async () => (await current())?.cancelled ?? 0).toBeGreaterThan(opened.cancelled);
            expect(await streamCounters(next.id)).toEqual(otherBefore);
            const location = new URL(page.url());
            expect(location.searchParams.get("cluster")).toBe(action === "switch" ? next.id : cluster.id);
            expect(location.searchParams.has("r")).toBe(false);
            expect(location.searchParams.has("tab")).toBe(false);
            expect(location.searchParams.get("view")).toBe("resources");
            await testInfo.attach(`upstream-${cluster.id}-${action}`, { body: JSON.stringify({ before, after: await streamCounters(cluster.id) }, null, 2), contentType: "application/json" });
          } finally {
            await page.goto("about:blank");
            await expect.poll(active).toBe(0);
          }
        }
      }
    });

    test("unknown clusters return to fleet and raw routes fail closed", async ({ page }) => {
      await openApp(page, "/agent/kubernetes?cluster=unknown-cluster");
      await expect(page.getByText("This cluster is no longer available.", { exact: true })).toBeVisible();
      await expect(page.getByRole("heading", { name: "Clusters", exact: true })).toBeVisible();
      await expect(page.getByRole("combobox", { name: "Cluster", exact: true })).toHaveCount(0);
      expect((await page.request.get("/api/admin/kubernetes/overview")).status()).toBe(400);
      expect((await page.request.get("/api/admin/kubernetes/overview?cluster=unknown-cluster")).status()).toBe(404);
    });

    test("chat selects a cluster before searching and keeps attachment backlinks scoped", async ({ page }) => {
      await openApp(page, "/agent/chat");
      const catalog: KubernetesClusters = await (await page.request.get("/api/admin/kubernetes/clusters")).json();
      const cluster = catalog.clusters.find((entry) => entry.health !== "unreachable")!;
      const resources: KubernetesResourcePage = await (await page.request.get(`/api/admin/kubernetes/workloads?cluster=${encodeURIComponent(cluster.id)}&limit=20`)).json();
      const resource = resources.items?.[0];
      expect(resource, "the live fixture must contain an attachable workload").toBeTruthy();
      await page.getByRole("button", { name: "Attach Kubernetes resource", exact: true }).click();
      const picker = page.getByRole("dialog", { name: "Attach Kubernetes resource", exact: true });
      await picker.getByRole("combobox", { name: "Attachment cluster" }).selectOption(cluster.id);
      const search = page.waitForResponse((response) => new URL(response.url()).pathname.endsWith("/resources/search") && new URL(response.url()).searchParams.get("cluster") === cluster.id);
      await picker.getByRole("combobox", { name: "Search Kubernetes resources" }).fill(resource!.name);
      expect((await search).status()).toBe(200);
      await picker.getByRole("option").filter({ hasText: resource!.name }).first().click();
      const backlink = page.getByRole("link", { name: /Open in topology/ });
      await expect(backlink).toHaveAttribute("href", new RegExp(`cluster=${cluster.id}`));
      await backlink.click();
      await expect(page.getByRole("combobox", { name: "Cluster", exact: true })).toHaveValue(cluster.id);
    });
  });
}

async function expectNoTopologyPagination(panel: Locator) {
  await expect(panel.getByRole("button", { name: /^(Prev|Previous|Next)( page)?$/i })).toHaveCount(0);
  await expect(panel.getByText(/Bounded graph omitted|projected index is partial|Next page/i)).toHaveCount(0);
}

const screenshotDir = process.env.E2E_KUBERNETES_CAPTURE_DIR || captureDir();
// Assertion-only reruns retain inspected captures for unaffected visual inputs.
const captureUnchanged = process.env.E2E_CAPTURE_UNCHANGED !== "0";
if (process.env.E2E_NATIVE_SCROLLBARS === "1") {
  test.use({ launchOptions: { args: ["--disable-features=OverlayScrollbar,FluentOverlayScrollbar"] } });
}

async function fitOverviewCapture(page: Page) {
  const measurements = [];
  while (true) {
    const size = await page.locator("main").evaluate((main) => ({ height: innerHeight, scrollHeight: main.scrollHeight, clientHeight: main.clientHeight, chrome: innerHeight - main.clientHeight }));
    measurements.push(size);
    if (size.scrollHeight <= size.clientHeight + 1) break;
    const requiredHeight = Math.ceil(size.scrollHeight + size.chrome + 2);
    expect(requiredHeight).toBeGreaterThan(size.height);
    await page.setViewportSize({ width: 1440, height: requiredHeight });
  }
  await page.locator("main").evaluate((main) => { main.scrollTop = 0; });
  return measurements;
}

async function captureBreakState(page: Page, filename: string, theme: "dark" | "light", classification = "F: synthetic API-intercepted UI fixture, not live-cluster acceptance") {
  if (process.env.E2E_KUBERNETES_AUTO_DETECT_DOCS === "1") {
    const selected: Record<string, string> = {
      "auto-detect-issues-overview.png": "issues-overview",
      "break-issues-expanded-desktop-dark.png": "issues-details",
      "auto-detect-issue-alerts.png": "issue-alerts",
      "break-timeline-general-desktop-dark.png": "timeline-overview",
      "break-timeline-focus-desktop-dark.png": "timeline-focus",
      "auto-detect-change-compare.png": "change-compare",
      "break-crash-popover-desktop.png": "crash-actions",
      "break-logs-desktop.png": "break-logs",
      "auto-detect-log-filters.png": "log-filters",
      "auto-detect-live-logs.png": "live-logs",
      "break-agent-desktop.png": "agent-link",
    };
    if (!selected[filename]) return;
    filename = `kubernetes-auto-detect-${selected[filename]}.png`;
  }
  const proof = await page.evaluate((expected) => {
    const background = getComputedStyle(document.body).backgroundColor;
    const storedTheme = localStorage.getItem("versus.theme");
    if (expected === "dark" && storedTheme === "dark" && background === "rgb(13, 17, 24)") document.documentElement.setAttribute("data-theme", "dark");
    return { storedTheme, theme: document.documentElement.getAttribute("data-theme"), background, font: getComputedStyle(document.body).fontFamily, viewport: { width: innerWidth, height: innerHeight }, deviceScaleFactor: devicePixelRatio };
  }, theme);
  expect(proof.storedTheme).toBe(theme);
  expect(proof.theme).toBe(theme);
  if (theme === "dark") expect(proof.background).toBe("rgb(13, 17, 24)");
  else expect(proof.background).toBe("rgb(238, 241, 246)");
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  fs.mkdirSync(screenshotDir, { recursive: true });
  const panel = process.env.E2E_KUBERNETES_AUTO_DETECT_DOCS === "1" && filename.endsWith("-break-logs.png") ? page.getByRole("region", { name: "Logs at break", exact: true }) : undefined;
  const image = panel
    ? await panel.screenshot({ path: path.join(screenshotDir, filename), animations: "disabled" })
    : await page.screenshot({ path: path.join(screenshotDir, filename), animations: "disabled" });
  if (!panel) {
    expect(image.readUInt32BE(16)).toBe(proof.viewport.width * proof.deviceScaleFactor);
    expect(image.readUInt32BE(20)).toBe(proof.viewport.height * proof.deviceScaleFactor);
  }
  const manifestPath = path.join(screenshotDir, "manifest.json");
  const manifest = fs.existsSync(manifestPath) ? JSON.parse(fs.readFileSync(manifestPath, "utf8")) as { images: Record<string, unknown> } : { images: {} };
  manifest.images[filename] = { classification, spec: "tests/e2e/kubernetes-overview.spec.ts", config: "tests/e2e/playwright.config.ts", sha256: createHash("sha256").update(image).digest("hex"), ...proof, dimensions: { width: image.readUInt32BE(16), height: image.readUInt32BE(20) } };
  fs.writeFileSync(manifestPath, JSON.stringify(manifest, null, 2));
  return proof;
}
const defaultKinds = ["Ingress", "Service", "Deployment", "Pod"];

test.beforeEach(async ({ request }, testInfo) => {
  if (process.env.E2E_KUBERNETES_DOCS !== "1") return;
  const timelineCapture = testInfo.title === "captures populated timeline";
  const trafficCapture = testInfo.title === "captures populated traffic";
  if (!timelineCapture && !trafficCapture) return;
  test.setTimeout(240_000);
  const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../../..");
  const state = harnessRuntimeStateDir();
  const fakePort = Number(process.env.HARNESS_FAKEKUBE_PORT || 19443);
  const prometheusPort = Number(process.env.HARNESS_PROMETHEUS_PORT || 19090);
  const pushgatewayPort = Number(process.env.HARNESS_PUSHGATEWAY_PORT || 19091);
  const api = (endpoint: string) => JSON.parse(execFileSync(path.join(root, "plans/harness-run/harness.sh"), ["api", "enterprise", "GET", endpoint], { encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }));
  const fake = async (endpoint: string, body?: unknown): Promise<string> => new Promise((resolve, reject) => {
    const call = https.request(`https://localhost:${fakePort}${endpoint}`, { method: body ? "POST" : "GET", ca: fs.readFileSync(path.join(state, "fakekube/ca.crt")), headers: body ? { "Content-Type": "application/json" } : {} }, (response) => {
      let text = "";
      response.on("data", (chunk) => { text += chunk; });
      response.on("end", () => response.statusCode === 200 ? resolve(text) : reject(new Error(`fake API HTTP ${response.statusCode}`)));
    });
    call.on("error", reject);
    call.end(body ? JSON.stringify(body) : undefined);
  });
  const timeline = async () => {
    const creations = JSON.parse(fs.readFileSync(path.join(root, "plans/harness-run/config/k8s/populated-changes.json"), "utf8"));
    expect(creations.map((creation: { name: string }) => creation.name)).toEqual(["checkout-api", "checkout-web", "checkout-worker"]);
    await expect.poll(() => {
      api("/api/admin/kubernetes/overview");
      const changes = api("/api/admin/kubernetes/changes?namespace=shop&limit=100");
      return changes.sync.state === "ready" && !changes.sync.partial;
    }, { timeout: 60_000, intervals: [1000, 3000] }).toBe(true);
    const suffix = Date.now().toString(36);
    for (const creation of creations) {
      const timestamp = new Date().toISOString();
      const name = `${creation.name}-${suffix}`;
      creation.name = name;
      creation.object.metadata.name = name;
      creation.object.metadata.uid = `deployment-${name}-shop`;
      creation.object.metadata.creationTimestamp = timestamp;
      creation.object.metadata.labels.app = name;
      creation.object.metadata.labels["app.kubernetes.io/name"] = name;
      creation.object.spec.selector.matchLabels.app = name;
      creation.object.spec.template.metadata.labels.app = name;
      await fake("/_fake/mutate", creation);
      await expect.poll(() => api("/api/admin/kubernetes/changes?namespace=shop&limit=100").items.some((item: { name: string; type: string; at: string }) => item.name === name && item.type === "created" && Date.parse(item.at) >= Date.parse(timestamp) - 1000), { timeout: 60_000, intervals: [1000, 3000, 5000] }).toBe(true);
      console.log(`Live timeline recorded creation: ${name}`);
    }
  };
  const traffic = async () => {
    await expect.poll(async () => {
      const metrics = await fake("/_fake/traffic-metrics");
      expect((await request.post(`http://127.0.0.1:${pushgatewayPort}/metrics/job/kubernetes-populated`, { data: metrics, headers: { "Content-Type": "text/plain" } })).ok()).toBe(true);
      const samples = await request.get(`http://127.0.0.1:${prometheusPort}/api/v1/query`, { params: { query: 'min(count_over_time(istio_requests_total[15m]))' } });
      const result = (await samples.json()).data.result;
      const data = api("/api/admin/kubernetes/traffic?source=istio&window=15m");
      return Number(result[0]?.value[1] ?? 0) >= 2 && data.available && data.edges.some((edge: { rate_per_sec: number; error_rate: number; p95_ms: number }) => edge.rate_per_sec > 0 && edge.error_rate > 0 && edge.p95_ms > 0);
    }, { timeout: 120_000, intervals: [1000, 3000, 5000] }).toBe(true);
    console.log("Live Istio traffic ready: >=2 Prometheus samples and positive request/error/p95 observations.");
  };
  if (timelineCapture && process.env.E2E_KUBERNETES_TIMELINE_SEED !== "0") await timeline();
  if (trafficCapture) await traffic();
});

async function expectGraphCounts(panel: Locator, data: KubernetesGraph, kinds?: string[]) {
  const nodes = data.nodes.filter((node) => !kinds || kinds.includes(node.kind));
  const ids = new Set(nodes.map((node) => node.id));
  const edges = data.edges.filter((edge) => ids.has(edge.from) && ids.has(edge.to));
  await expect(panel.getByText(`${nodes.length} / ${data.nodes.length} connected resources visible`, { exact: false })).toContainText(`${edges.length} / ${data.edges.length} relationships`);
}

async function expectDefaultFilters(panel: Locator, data: KubernetesGraph) {
  const sidebar = panel.getByRole("complementary", { name: "Topology filters", exact: true });
  for (const group of ["Networking", "Workloads", "Configuration"]) {
    await expect(sidebar.locator("summary", { hasText: group })).toBeVisible();
  }
  await expect(panel.getByRole("group", { name: "Topology namespace controls", exact: true }).getByRole("button", { name: "Refresh namespace topology", exact: true })).toBeVisible();
  for (const kind of new Set([...defaultKinds, ...data.nodes.map((node) => node.kind)])) {
    const visible = defaultKinds.includes(kind);
    const toggle = sidebar.getByRole("button", { name: `${visible ? "Hide" : "Show"} ${kind}`, exact: true });
    await expect(toggle).toHaveAttribute("aria-pressed", String(visible));
    await expect(toggle.locator("svg")).toHaveCount(1);
    await expect(toggle.locator("svg")).toHaveClass(visible ? /lucide-eye(?!-off)/ : /lucide-eye-off/);
    await expect(toggle.locator("..").locator("span").last()).toHaveText(String(data.nodes.filter((node) => node.kind === kind).length));
  }
  for (const name of ["Show all", "Hide all"]) {
    await expect(sidebar.getByRole("button", { name, exact: true }).locator("svg")).toHaveCount(1);
  }
  await expectGraphCounts(panel, data, defaultKinds);
}

const captureViews = [
  { id: "overview", label: "Overview" },
  { id: "issues", label: "Issues" },
  { id: "resources", label: "Workloads" },
  { id: "nodes", label: "Nodes" },
  { id: "timeline", label: "Timeline" },
  { id: "topology", label: "Topology" },
  { id: "traffic", label: "Traffic" },
  { id: "releases", label: "Helm" },
  { id: "gitops", label: "GitOps" },
] as const;

for (const viewport of [
  { name: "desktop", width: 1440, height: 1100, deviceScaleFactor: 2 },
  { name: "mobile", width: 390, height: 844, deviceScaleFactor: 1 },
]) {
  test.describe(`Kubernetes populated documentation ${viewport.name}`, () => {
    test.use({ viewport: { width: viewport.width, height: viewport.height }, deviceScaleFactor: viewport.deviceScaleFactor });
    test.skip(process.env.E2E_KUBERNETES_BACKEND !== "fake", "requires the harness fake Kubernetes backend");
    test.skip(process.env.E2E_KUBERNETES_DOCS !== "1", "opt-in populated documentation fixture contract");

    for (const view of ["overview", "issues", "resources", "nodes", "timeline", "releases", "gitops", "traffic", "topology"] as const) {
      test(`captures populated ${view}`, async ({ page }) => {
        if (view === "topology" && viewport.name === "desktop") await page.setViewportSize({ width: 1600, height: 1000 });
        const endpoint = { overview: "overview", issues: "issues", resources: "workloads", nodes: "resources", timeline: "changes", releases: "releases", gitops: "gitops/apps", traffic: "traffic", topology: "resources" }[view];
        const loaded = page.waitForResponse((response) => {
          const url = new URL(response.url());
          return url.pathname === `/api/admin/kubernetes/${endpoint}` && (view !== "nodes" || url.searchParams.get("resource_id") === "core~v1~nodes");
        });
        const rolloutsLoaded = view === "gitops" ? page.waitForResponse((response) => new URL(response.url()).pathname === "/api/admin/kubernetes/rollouts") : null;
        await openApp(page, `/agent/kubernetes?view=${view}`);
        const response = await loaded;
        const rolloutResponse = rolloutsLoaded ? await rolloutsLoaded : null;
        expect(response.status()).toBe(200);
        const data = await response.json();
        const panel = page.locator("#kubernetes-view-panel");
        await expect(panel).toBeVisible();
        const capture = async (name: string) => {
          await expect(panel.getByText(/^Loading/i)).toHaveCount(0);
          await expect(panel.getByRole("alert")).toHaveCount(0);
          if (view !== "overview") await expect(panel).not.toContainText(/not detected|no projected changes|no Helm release labels|no service flows/i);
          if (!["traffic", "overview", "issues", "nodes"].includes(view)) await expect(panel).not.toContainText(/unavailable/i);
          expect(await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth)).toBe(false);
          await page.locator("main").evaluate((main) => main.scrollTo({ top: 0 }));
          for (let attempt = 0; attempt < 3; attempt++) {
            const extraHeight = await panel.evaluate((element) => Math.max(0, Math.ceil(element.getBoundingClientRect().bottom + 24 - window.innerHeight)));
            if (extraHeight === 0) break;
            await page.setViewportSize({ width: page.viewportSize()!.width, height: page.viewportSize()!.height + extraHeight });
            await page.locator("main").evaluate((main) => main.scrollTo({ top: 0 }));
          }
          await expect(panel).toBeInViewport({ ratio: 1 });
          fs.mkdirSync(screenshotDir, { recursive: true });
          const image = await page.screenshot({ path: path.join(screenshotDir, `docs-${name}-${viewport.name}.png`), animations: "disabled", scale: "device" });
          expect(image.readUInt32BE(16)).toBe(page.viewportSize()!.width * viewport.deviceScaleFactor);
          expect(image.readUInt32BE(20)).toBe(page.viewportSize()!.height * viewport.deviceScaleFactor);
        };
        if (view === "topology") {
          expect(data.items.length, "namespace inventory must be populated").toBeGreaterThan(0);
          const controls = panel.getByRole("group", { name: "Topology namespace controls", exact: true });
          await expect(controls.getByRole("combobox", { name: "Topology namespace", exact: true })).toHaveValue("");
          await expect(controls.getByRole("button", { name: "Refresh namespaces", exact: true })).toBeInViewport();
          const shop = panel.getByRole("button", { name: "Open namespace shop topology", exact: true });
          if (viewport.name === "mobile") await shop.scrollIntoViewIfNeeded();
          await expect(shop).toBeInViewport();
          await capture("namespaces");
          const graphLoaded = page.waitForResponse((result) => new URL(result.url()).pathname === "/api/admin/kubernetes/graph");
          await shop.click();
          const graphResponse = await graphLoaded;
          expect(graphResponse.status()).toBe(200);
          const graph: KubernetesGraph = await graphResponse.json();
          expect(graph.nodes.length).toBeGreaterThan(0);
          expect(graph.edges.length).toBeGreaterThan(0);
          expect(graph.sync.partial).toBe(false);
          expect(graph.truncated ?? false).toBe(false);
          expect(graph.partial_failures ?? []).toEqual([]);
          expect(Object.values(graph.omitted ?? {}).some((count) => count > 0)).toBe(false);
          expect(graph.next ?? "").toBe("");
          await expect(controls.getByRole("combobox", { name: "Topology namespace", exact: true })).toHaveValue("shop");
          await expect(controls.getByRole("button", { name: "Refresh namespace topology", exact: true })).toBeInViewport();
          expect(await controls.evaluate((group) => {
            const boxes = [...group.querySelectorAll("button, select")].map((control) => control.getBoundingClientRect());
            return boxes.every((box, index) => box.left >= 0 && box.right <= window.innerWidth && boxes.slice(index + 1).every((other) => box.right <= other.left || other.right <= box.left || box.bottom <= other.top || other.bottom <= box.top));
          }), "namespace selection, back, and refresh controls must not overlap or overflow").toBe(true);
          await expect(panel.locator("button[data-node-id]")).toHaveCount(graph.nodes.filter((node) => defaultKinds.includes(node.kind)).length);
          await expectDefaultFilters(panel, graph);
          await expectGraphCounts(panel, graph, defaultKinds);
          await expectNoTopologyPagination(panel);
          await expect(panel.getByText(/^Loading/i)).toHaveCount(0);
          await expect(panel.getByRole("alert")).toHaveCount(0);
          const graphViewport = panel.getByLabel("Scrollable topology graph", { exact: true });
          const scale = () => graphViewport.locator("div[style*='transform: scale']").evaluate((element) => new DOMMatrix(getComputedStyle(element).transform).a);
          await panel.getByRole("button", { name: "Fit topology to view", exact: true }).click();
          if (viewport.name === "mobile") {
            for (const control of [controls.getByRole("button", { name: "Namespaces", exact: true }), controls.getByRole("combobox"), controls.getByRole("button", { name: "Refresh namespace topology", exact: true }), ...await panel.getByRole("group", { name: "Topology view controls", exact: true }).getByRole("button").all()]) {
              await control.scrollIntoViewIfNeeded();
              await expect(control).toBeInViewport({ ratio: 1 });
            }
            for (let step = 0; step < 10 && await scale() < 1; step++) await panel.getByRole("button", { name: "Zoom in topology", exact: true }).click();
            expect(await scale(), "mobile graph must use readable internal zoom").toBeGreaterThanOrEqual(1);
            await graphViewport.scrollIntoViewIfNeeded();
            await expect(graphViewport.locator("button[data-node-id]").first()).toBeInViewport({ ratio: 1 });
            await panel.getByRole("button", { name: "Pan topology right", exact: true }).click();
            expect(await graphViewport.evaluate((element) => element.scrollLeft)).toBeGreaterThan(0);
            expect(await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth)).toBe(false);
            await panel.getByRole("button", { name: "Fit topology to view", exact: true }).click();
            await capture("topology");
            for (let step = 0; step < 10 && await scale() < 1; step++) await panel.getByRole("button", { name: "Zoom in topology", exact: true }).click();
            expect(await scale()).toBeGreaterThanOrEqual(1);
            await graphViewport.evaluate((element) => element.scrollTo({ left: 0, top: 0 }));
            await graphViewport.scrollIntoViewIfNeeded();
            await expect(graphViewport.locator("button[data-node-id]").first()).toBeInViewport({ ratio: 1 });
            await graphViewport.screenshot({ path: path.join(screenshotDir, "docs-topology-graph-mobile.png"), animations: "disabled" });
            await graphViewport.evaluate((element) => { element.scrollLeft = element.scrollWidth; });
            await expect(graphViewport.getByRole("button", { name: "Open Pod shop/checkout-0 details", exact: true })).toBeInViewport({ ratio: 1 });
            await graphViewport.screenshot({ path: path.join(screenshotDir, "docs-topology-runtime-mobile.png"), animations: "disabled" });
            return;
          }
          expect(await scale(), "desktop graph must not shrink node names below readable size").toBeGreaterThanOrEqual(0.9);
          const selectedNodes = graph.nodes.filter((node) => defaultKinds.includes(node.kind));
          const selectedIds = new Set(selectedNodes.map((node) => node.id));
          await expect(graphViewport.locator("button[data-node-id]")).toHaveCount(selectedNodes.length);
          await expect(graphViewport.locator("svg g[data-edge-from][data-edge-to]")).toHaveCount(graph.edges.filter((edge) => selectedIds.has(edge.from) && selectedIds.has(edge.to)).length);
          await page.locator("main").evaluate((main) => main.scrollTo({ top: 0 }));
          for (const control of [controls, panel.getByRole("group", { name: "Topology view controls", exact: true }), panel.getByLabel("Relationship key", { exact: true })]) await expect(control).toBeInViewport({ ratio: 1 });
          const geometryFailures = await graphViewport.evaluate((viewport) => {
            const bounds = viewport.getBoundingClientRect();
            const nodes = [...viewport.querySelectorAll<HTMLButtonElement>("button[data-node-id]")];
            const edges = [...viewport.querySelectorAll<SVGGElement>("svg g[data-edge-from][data-edge-to]")];
            const failures: string[] = [];
            for (const node of nodes) {
              const box = node.getBoundingClientRect();
              if (box.left < bounds.left || box.right > bounds.right || box.top < bounds.top || box.bottom > bounds.bottom || box.top < 0 || box.bottom > window.innerHeight) failures.push(`clipped node: ${node.title}`);
              for (const text of node.querySelectorAll<HTMLElement>("span")) if (text.scrollWidth > text.clientWidth) failures.push(`truncated node text: ${node.title}`);
            }
            for (const edge of edges) {
              const label = edge.querySelector("rect")?.getBoundingClientRect();
              if (!label) { failures.push("missing relationship label"); continue; }
              if (label.left < bounds.left || label.right > bounds.right || label.top < bounds.top || label.bottom > bounds.bottom) failures.push(`clipped label: ${edge.textContent}`);
              for (const node of nodes) {
                const box = node.getBoundingClientRect();
                if (label.left < box.right && label.right > box.left && label.top < box.bottom && label.bottom > box.top) failures.push(`label overlaps node: ${edge.textContent} / ${node.title}`);
              }
              for (const other of edges.filter((candidate) => candidate !== edge)) {
                const curve = other.querySelector<SVGPathElement>("path");
                const matrix = curve?.getScreenCTM();
                if (!curve || !matrix) continue;
                const length = curve.getTotalLength();
                for (let distance = 0; distance <= length; distance += 1) {
                  const point = curve.getPointAtLength(distance).matrixTransform(matrix);
                  if (point.x > label.left && point.x < label.right && point.y > label.top && point.y < label.bottom) {
                    failures.push(`edge ${other.textContent} crosses label ${edge.textContent}`);
                    break;
                  }
                }
              }
            }
            return failures;
          });
          expect(geometryFailures, "reject intrinsic routing overlaps; do not conceal them with capture styling").toEqual([]);
          await capture("topology");
          return;
        }
        if (view === "overview") {
          expect(data.nodes, "overview nodes must be populated").toBeGreaterThan(0);
          expect(data.workloads, "overview workloads must be populated").toBeGreaterThan(0);
          await expect(panel).toContainText("Cluster health");
        } else if (view === "issues") {
          expect(data.items.length, "issue inventory must be populated").toBeGreaterThan(0);
          await expect(panel).toContainText(data.items[0].root.name);
        } else if (view === "nodes") {
          expect(data.items.length, "node inventory must be populated").toBeGreaterThan(0);
          await expect(panel).toContainText(data.items[0].name);
        } else if (view === "resources") {
          expect(data.items.length, "workload inventory must be populated").toBeGreaterThan(0);
          await expect(panel).toContainText(data.items[0].name);
        } else if (view === "traffic") {
          expect(data.available, "real traffic contributor must be connected").toBe(true);
          expect(data.edges.length, "observed service flows must be populated").toBeGreaterThan(0);
          const flow = data.edges[0];
          for (const field of ["rate_per_sec", "error_rate", "p95_ms"]) expect(typeof flow[field], `${field} must be observed`).toBe("number");
          expect(data.edges.some((edge: { rate_per_sec: number; error_rate: number; p95_ms: number }) => edge.rate_per_sec > 0 && edge.error_rate > 0 && edge.p95_ms > 0)).toBe(true);
          expect(data.source).toBe("istio");
          expect(data.window).toBe("15m0s");
          console.log(`Observed Istio traffic: ${JSON.stringify(data.edges)}`);
          await expect(panel).toContainText(flow.from.name);
          await expect(panel).toContainText(flow.to.name);
        } else {
          expect(data.items.length, `${view} real fixture inventory must be populated`).toBeGreaterThan(0);
          const item = data.items[0];
          await expect(panel).toContainText(item.name);
          if (view === "timeline") {
            expect(data.items.length, "timeline must contain multiple genuinely recorded changes").toBeGreaterThan(1);
            if (process.env.E2E_KUBERNETES_TIMELINE_SEED !== "0") {
              for (const name of ["checkout-api", "checkout-web", "checkout-worker"]) expect(data.items.some((change: { name: string; type: string }) => change.name.startsWith(`${name}-`) && change.type === "created")).toBe(true);
            }
            for (const change of data.items) {
              expect(change.name).toBeTruthy();
              expect(change.type).toBeTruthy();
              expect(Number.isFinite(Date.parse(change.at))).toBe(true);
            }
            expect(item.type).toBeTruthy();
            expect(item.at).toBeTruthy();
            await expect(panel.getByRole("button", { name: new RegExp(`${item.kind} ${item.name}`) }).first()).toBeVisible();
          }
          if (view === "releases") {
            expect(item.current.revision).toBeGreaterThanOrEqual(2);
            expect(item.current.status).toBeTruthy();
            await panel.locator("article").first().getByRole("button").click();
            await expect(panel).toContainText(`Revision ${item.current.revision}`);
            await expect(panel).toContainText("Revision 1");
            await expect(panel).toContainText(item.current.status);
          }
          if (view === "gitops") {
            expect(data.available).toBe(true);
            expect(data.items.length, "GitOps must expose at least two discovered apps").toBeGreaterThanOrEqual(2);
            expect(item.sync).toBeTruthy();
            expect(item.health).toBeTruthy();
            for (const app of data.items) {
              expect(app.revision).toBeTruthy();
              expect(app.sync).toBeTruthy();
              expect(app.health).toBeTruthy();
              await expect(panel).toContainText(app.revision);
            }
            await expect(panel).toContainText(item.sync);
            await expect(panel).toContainText(item.health);
            expect(rolloutResponse!.status()).toBe(200);
            const rollouts = await rolloutResponse!.json();
            expect(rollouts.available).toBe(true);
            expect(rollouts.items.length, "discovered Rollouts must be populated").toBeGreaterThan(0);
            await expect(panel.getByRole("region", { name: "Argo Rollouts", exact: true })).toContainText(rollouts.items[0].name);
            const rollout = rollouts.items[0];
            expect(rollout.weight).toBe(50);
            expect(rollout.strategy).toBe("canary");
            expect(rollout.stable_rs).toBe("checkout-stable");
            expect(rollout.canary_rs).toBe("checkout-canary");
            const region = panel.getByRole("region", { name: "Argo Rollouts", exact: true });
            for (const value of ["50%", rollout.strategy, rollout.stable_rs, rollout.canary_rs]) await expect(region).toContainText(value);
            const resource = await page.request.get(`/api/admin/kubernetes/resources/argoproj.io~v1alpha1~rollouts/${rollout.name}/describe?namespace=${rollout.namespace}`);
            expect(resource.ok()).toBe(true);
            const actual = await resource.json();
            expect(actual.resource.summary.rollout_stable_rs).toBe(rollout.stable_rs);
            expect(actual.resource.summary.rollout_canary_rs).toBe(rollout.canary_rs);
            expect(actual.resource.summary.replicas).toBe(4);
            expect(actual.resource.summary.updatedReplicas).toBe(2);
            expect(actual.resource.summary.readyReplicas).toBe(4);
            console.log(`Observed GitOps apps and rollout: ${JSON.stringify({ apps: data.items, rollout, replicas: actual.resource.summary })}`);
          }
        }
        await capture(view === "releases" ? "helm" : view);
        if (view === "traffic" && viewport.name === "mobile") {
          const tableScroll = panel.locator("table").locator("..");
          await tableScroll.evaluate((element) => { element.scrollLeft = element.scrollWidth; });
          expect(await tableScroll.evaluate((element) => element.scrollLeft)).toBeGreaterThan(0);
          await expect(panel.getByRole("columnheader", { name: "P95", exact: true })).toBeInViewport({ ratio: 1 });
          await capture("traffic-metrics");
        }
      });
    }
  });
}

test.describe("Kubernetes documentation consumer", () => {
  const docsURL = process.env.E2E_KUBERNETES_DOCS_URL;
  test.skip(!docsURL, "requires an existing local Docsify server");
  for (const viewport of [
    { name: "desktop", width: 1440, height: 1000 },
    { name: "mobile", width: 390, height: 844 },
  ]) {
    test(`loads kind workload live log documentation ${viewport.name}`, async ({ page }) => {
      const target = new URL(docsURL!);
      expect(["localhost", "127.0.0.1", "[::1]"]).toContain(target.hostname);
      await page.setViewportSize({ width: viewport.width, height: viewport.height });
      await page.goto(`${target.origin}/#/agent/connectors/kubernetes`);
      const article = page.locator(".markdown-section");
      await expect(article.getByRole("heading", { name: "Live logs", exact: true })).toBeVisible();
      for (const kind of ["deployment", "statefulset"]) {
        const filename = `kubernetes-${kind}-live-logs.png`;
        const image = article.locator(`img[src$="${filename}"]`);
        await image.scrollIntoViewIfNeeded();
        await expect(image).toBeVisible();
        await expect.poll(() => image.evaluate((element) => {
          const bitmap = element as HTMLImageElement;
          const bounds = bitmap.getBoundingClientRect();
          return bitmap.complete && bitmap.naturalWidth === 1440 && bounds.left >= 0 && bounds.right <= innerWidth;
        })).toBe(true);
        const response = await page.request.get(new URL((await image.getAttribute("src"))!, page.url()).href);
        expect(response.status()).toBe(200);
        expect(response.headers()["content-type"]).toContain("image/png");
        const published = fs.readFileSync(path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../../src/docs/images", filename));
        expect(createHash("sha256").update(await response.body()).digest("hex")).toBe(createHash("sha256").update(published).digest("hex"));
        expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
        await page.screenshot({ path: capturePath(`kubernetes-${kind}-live-logs-docs`, viewport.width, "light", "consumer"), animations: "disabled" });
      }
    });

    test(`verifies refreshed issue-alert documentation assets ${viewport.name}`, async ({ page }) => {
      const target = new URL(docsURL!);
      expect(["localhost", "127.0.0.1", "[::1]"]).toContain(target.hostname);
      await page.setViewportSize({ width: viewport.width, height: viewport.height });
      await page.goto(`${target.origin}/#/agent/kubernetes/auto-detect-issue`);
      await expect(page.getByRole("heading", { name: "Automatically Detect Kubernetes Issues", exact: true })).toBeVisible();
      await expect(page.getByRole("heading", { name: "Alerts", exact: true })).toBeVisible();
      const alerts = page.locator('.markdown-section img[src$="kubernetes-auto-detect-issue-alerts.png"]');
      await expect(alerts).toHaveAttribute("alt", /alerts enabled and Critical selected.*synthetic demonstration data/);
      for (const image of await page.locator(".markdown-section img").all()) {
        const response = await page.request.get(new URL((await image.getAttribute("src"))!, page.url()).href);
        expect(response.status()).toBe(200);
        await image.scrollIntoViewIfNeeded();
        await expect(image).toBeVisible();
        await expect.poll(() => image.evaluate((element) => {
          const bitmap = element as HTMLImageElement;
          const bounds = bitmap.getBoundingClientRect();
          return bitmap.complete && bitmap.naturalWidth > 0 && bitmap.naturalHeight > 0 && bounds.left >= 0 && bounds.right <= innerWidth;
        })).toBe(true);
      }
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
      for (const filename of [
        "kubernetes-auto-detect-issues-overview.png",
        "kubernetes-auto-detect-issues-details.png",
        "kubernetes-auto-detect-issue-alerts.png",
      ]) {
        // Confirm the embedded captures still match the inspected published assets.
        const response = await page.request.get(`${target.origin}/docs/images/${filename}`);
        expect(response.status()).toBe(200);
        expect(response.headers()["content-type"]).toContain("image/png");
        const image = await response.body();
        expect(image.readUInt32BE(16)).toBe(1440);
        expect(image.readUInt32BE(20)).toBe(1000);
        const published = fs.readFileSync(path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../../src/docs/images", filename));
        expect(createHash("sha256").update(image).digest("hex")).toBe(createHash("sha256").update(published).digest("hex"));
      }
    });

    test(`loads published Kubernetes images ${viewport.name}`, async ({ page }) => {
      const target = new URL(docsURL!);
      expect(["localhost", "127.0.0.1", "[::1]"]).toContain(target.hostname);
      await page.setViewportSize({ width: viewport.width, height: viewport.height });
      await page.goto(`${target.origin}/#/agent/connectors/kubernetes`);
      await expect(page.getByRole("heading", { name: "Kubernetes Connector and Dashboard", exact: true })).toBeVisible();
      for (const name of ["overview", "issues", "resources", "timeline", "helm", "gitops", "traffic", "namespaces", "topology"]) {
        const image = page.locator(`.markdown-section img[src$="kubernetes-harness-${name}.png"]`);
        expect((await page.request.get(new URL(await image.getAttribute("src") as string, page.url()).href)).status()).toBe(200);
        await image.scrollIntoViewIfNeeded();
        await expect(image).toBeVisible();
        await expect.poll(() => image.evaluate((element) => {
          const bitmap = element as HTMLImageElement;
          return bitmap.complete && bitmap.naturalWidth > 0 && bitmap.naturalHeight > 0;
        })).toBe(true);
        if (name === "overview") {
          await expect(image).toHaveAttribute("alt", /Full dark Overview.*populated synthetic/);
          const bitmap = await image.evaluate((element) => {
            const image = element as HTMLImageElement;
            const bounds = image.getBoundingClientRect();
            return { width: image.naturalWidth, height: image.naturalHeight, left: bounds.left, right: bounds.right, viewport: innerWidth };
          });
          expect(bitmap.width).toBe(1440);
          expect(bitmap.height).toBeGreaterThan(900);
          expect(bitmap.left).toBeGreaterThanOrEqual(0);
          expect(bitmap.right).toBeLessThanOrEqual(bitmap.viewport);
        }
        if (["overview", "issues", "timeline"].includes(name)) {
          fs.mkdirSync(screenshotDir, { recursive: true });
          await page.screenshot({ path: path.join(screenshotDir, `docs-consumer-${name}-${viewport.name}.png`), animations: "disabled" });
        }
      }
      const nodes = await page.request.get(`${target.origin}/docs/images/kubernetes-harness-nodes.png`);
      expect(nodes.ok(), "new node illustration must be available for the documentation owner").toBe(true);
      expect(await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth)).toBe(false);
      fs.mkdirSync(screenshotDir, { recursive: true });
      await page.screenshot({ path: path.join(screenshotDir, `docs-consumer-${viewport.name}.png`), fullPage: true, animations: "disabled" });
      await page.locator(".markdown-section").getByRole("link", { name: "Configuration", exact: true }).click();
      await expect(page.getByRole("heading", { name: "Kubernetes Configuration", exact: true })).toBeVisible();
      await expect(page).toHaveURL(/agent\/kubernetes\/configuration$/);
      await page.goto(`${target.origin}/#/agent/kubernetes/multi-cluster`);
      await expect(page.getByRole("heading", { name: "Kubernetes Multi-Cluster", exact: true })).toBeVisible();
      const fleet = page.locator('.markdown-section img[src$="kubernetes-multi-cluster.png"]');
      await fleet.scrollIntoViewIfNeeded();
      expect((await page.request.get(new URL(await fleet.getAttribute("src") as string, page.url()).href)).status()).toBe(200);
      await expect.poll(() => fleet.evaluate((element) => (element as HTMLImageElement).complete && (element as HTMLImageElement).naturalWidth > 0)).toBe(true);
      expect(await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth)).toBe(false);
      await page.screenshot({ path: path.join(screenshotDir, `docs-consumer-fleet-${viewport.name}.png`), fullPage: true, animations: "disabled" });
      await page.goto(`${target.origin}/#/agent/kubernetes/configuration?id=change-capture`);
      await expect(page.getByRole("heading", { name: "Kubernetes Configuration", exact: true })).toBeAttached();
      await expect(page.getByRole("heading", { name: "Change capture", exact: true })).toBeInViewport();
      await expect(page.locator(".markdown-section")).toContainText("change_capture");
      await expect(page.locator(".markdown-section")).toContainText("changeCapture");
      expect(await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth)).toBe(false);
      await page.screenshot({ path: path.join(screenshotDir, `docs-consumer-capture-${viewport.name}.png`), animations: "disabled" });
    });

    test(`verifies rewritten auto-detect-issue documentation page ${viewport.name}`, async ({ page }) => {
      const target = new URL(docsURL!);
      expect(["localhost", "127.0.0.1", "[::1]"]).toContain(target.hostname);
      const errors: string[] = [];
      page.on("pageerror", (error) => errors.push(error.message));
      await page.setViewportSize({ width: viewport.width, height: viewport.height });
      await page.goto(`${target.origin}/#/agent/kubernetes/auto-detect-issue`);
      for (const heading of [
        "Automatically Detect Kubernetes Issues",
        "Automatically Detected, Grouped Issues",
        "Timeline Changes and Crashes",
        "Captured Logs",
        "Alerts",
        "Investigate with DevOps Agent",
      ]) {
        await expect(page.getByRole("heading", { name: heading, exact: true })).toBeVisible();
      }
      const expectedImages = [
        "kubernetes-auto-detect-issues-overview.png",
        "kubernetes-auto-detect-break-logs.png",
        "kubernetes-issue-alert-flow.svg",
      ];
      const images = page.locator(".markdown-section img");
      await expect(images).toHaveCount(expectedImages.length);
      for (const filename of expectedImages) {
        const img = page.locator(`.markdown-section img[src$="${filename}"]`);
        const src = await img.getAttribute("src");
        expect(src).toBeTruthy();
        const response = await page.request.get(new URL(src!, page.url()).href);
        expect(response.status()).toBe(200);
        await img.scrollIntoViewIfNeeded();
        await expect(img).toBeVisible();
        await expect.poll(() => img.evaluate((element) => {
          const bitmap = element as HTMLImageElement;
          const box = bitmap.getBoundingClientRect();
          return bitmap.complete && bitmap.naturalWidth > 0 && bitmap.naturalHeight > 0 && box.left >= 0 && box.right <= window.innerWidth;
        })).toBe(true);
      }
      expect(await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth)).toBe(false);
      fs.mkdirSync(screenshotDir, { recursive: true });
      await page.screenshot({ path: path.join(screenshotDir, `docs-consumer-auto-detect-issue-${viewport.name}.png`), fullPage: true, animations: "disabled" });

      await page.locator(".markdown-section").getByRole("link", { name: "Configuration", exact: true }).click();
      await expect(page.getByRole("heading", { name: "Kubernetes Configuration", exact: true })).toBeVisible();
      await expect(page).toHaveURL(/agent\/kubernetes\/configuration$/);

      await page.goto(`${target.origin}/#/agent/kubernetes/auto-detect-issue`);
      await expect(page.getByRole("heading", { name: "Automatically Detect Kubernetes Issues", exact: true })).toBeVisible();
      await page.locator(".markdown-section").getByRole("link", { name: "Kubernetes connector guide", exact: true }).click();
      await expect(page.getByRole("heading", { name: "Kubernetes Connector and Dashboard", exact: true })).toBeVisible();
      await expect(page).toHaveURL(/agent\/connectors\/kubernetes$/);

      await page.goto(`${target.origin}/#/agent/kubernetes/auto-detect-issue?id=alerts`);
      await expect(page.getByRole("heading", { name: "Alerts", exact: true })).toBeInViewport();
      expect(await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth)).toBe(false);
      expect(errors).toEqual([]);
    });

    test(`reveals active documentation item on navigation and reload ${viewport.name}`, async ({ page }) => {
      const target = new URL(docsURL!);
      expect(["localhost", "127.0.0.1", "[::1]"]).toContain(target.hostname);
      await page.setViewportSize({ width: viewport.width, height: viewport.height });
      const sidebar = page.locator(".sidebar-nav");
      const expectCurrentVisible = async (route: string, groups: string[]) => {
        const active = sidebar.locator(`a[href="#${route}"]`);
        for (const group of groups) await expect(sidebar.getByRole("button", { name: group, exact: true })).toHaveAttribute("aria-expanded", "true");
        await expect(active).toHaveClass(/active/);
        await expect(active).toHaveAttribute("aria-current", "page");
        await expect.poll(() => active.evaluate((link) => {
          const menu = link.closest(".sidebar-nav")!;
          const bounds = menu.getBoundingClientRect();
          const item = link.getBoundingClientRect();
          return item.top >= bounds.top - 1 && item.bottom <= bounds.bottom + 1;
        })).toBe(true);
        if (viewport.name === "mobile") {
          await page.locator("#menuToggle").click();
          await expect(active).toBeInViewport({ ratio: 1 });
          await page.locator("#menuToggle").click();
        } else await expect(active).toBeInViewport({ ratio: 1 });
      };
      await page.goto(`${target.origin}/#/introduction`);
      await expect(page.locator(".markdown-section h1")).toBeVisible();
      await page.evaluate(() => { location.hash = "/agent/regex"; });
      await expect(page.getByRole("heading", { name: "Regex", exact: true })).toBeVisible();
      await expectCurrentVisible("/agent/regex", ["Core Concept"]);
      expect(await sidebar.evaluate((menu) => menu.scrollTop)).toBeGreaterThan(0);
      await page.reload();
      await expect(page.getByRole("heading", { name: "Regex", exact: true })).toBeVisible();
      await expectCurrentVisible("/agent/regex", ["Core Concept"]);
      expect(await page.locator(".content").evaluate((content) => content.scrollTop)).toBe(0);
      fs.mkdirSync(screenshotDir, { recursive: true });
      if (viewport.name === "mobile") await page.locator("#menuToggle").click();
      await page.screenshot({ path: path.join(screenshotDir, `docs-active-regex-reload-${viewport.name}.png`), animations: "disabled" });
      await sidebar.getByRole("button", { name: "Core Concept", exact: true }).click();
      await page.evaluate(() => { location.hash = "/agent/regex?id=regex"; });
      await expect(sidebar.getByRole("button", { name: "Core Concept", exact: true })).toHaveAttribute("aria-expanded", "false");
      await page.goto(`${target.origin}/#/enterprise/sso/google`);
      await expect(page.locator(".markdown-section h1")).toBeVisible();
      await expectCurrentVisible("/enterprise/sso/google", ["Enterprise SRE Agent", "Single Sign-On (SSO)"]);
      await page.reload();
      await expect(page.locator(".markdown-section h1")).toBeVisible();
      await expectCurrentVisible("/enterprise/sso/google", ["Enterprise SRE Agent", "Single Sign-On (SSO)"]);
      fs.mkdirSync(screenshotDir, { recursive: true });
      if (viewport.name === "mobile") await page.locator("#menuToggle").click();
      await page.screenshot({ path: path.join(screenshotDir, `docs-active-sidebar-${viewport.name}.png`), animations: "disabled" });
    });
  }
});

test.describe("Live Kubernetes explorer captures", () => {
  test.skip(process.env.E2E_KUBERNETES_BACKEND !== "fake", "requires the harness fake Kubernetes backend");
  for (const viewport of [
    { name: "desktop", width: 1440, height: 1000 },
    { name: "mobile", width: 390, height: 844 },
  ]) {
    for (const view of captureViews) {
      test(`captures ${view.id} ${viewport.name}`, async ({ page }) => {
        await page.setViewportSize({ width: viewport.width, height: viewport.height });
        const graphRequests: URL[] = [];
        const eventRequests: string[] = [];
        page.on("request", (request) => {
          const url = new URL(request.url());
          if (url.pathname === "/api/admin/kubernetes/graph") graphRequests.push(url);
          if (url.pathname === "/api/admin/kubernetes/events") eventRequests.push(url.pathname);
        });
        const namespaceResponse = view.id === "topology"
          ? page.waitForResponse((response) => {
            const url = new URL(response.url());
            return url.pathname === "/api/admin/kubernetes/resources" && url.searchParams.get("resource_id") === "core~v1~namespaces";
          })
          : null;
        const usageResponse = view.id === "overview"
          ? page.waitForResponse((response) => new URL(response.url()).pathname === "/api/admin/kubernetes/usage")
          : null;
        await openApp(page, `/agent/kubernetes?view=${view.id}`);
        await expect(page.getByRole("heading", { name: "Kubernetes", exact: true })).toBeVisible();
        const tab = page.getByRole("tab", { name: view.label, exact: true });
        await tab.click();
        await expect(tab).toHaveAttribute("aria-selected", "true");
        const panel = page.locator("#kubernetes-view-panel");
        await expect(panel).toBeVisible();
        await expect(panel.getByText(/^Loading/i)).toHaveCount(0);
        await expect(panel).not.toContainText(/is unavailable|are unavailable|could not be queried/i);
        if (view.id === "resources") {
          const workloads = panel.getByRole("region", { name: "Workloads", exact: true });
          await expect(workloads).toBeVisible();
          await expect(workloads.getByRole("button", { name: /^Select / }).first()).toBeVisible();
        }
        if (view.id === "nodes") {
          const nodes = panel.getByRole("region", { name: "Nodes", exact: true });
          await expect(nodes).toBeVisible();
          await expect(nodes.getByRole("button", { name: /^View pods on / }).first()).toBeVisible();
        }
        if (usageResponse) {
          const response = await usageResponse;
          expect(response.status()).toBe(200);
          const usage: KubernetesUsage = await response.json();
          expect(usage.node_metrics.availability).toBe("unavailable");
          expect(usage.pod_metrics.availability).toBe("unavailable");
          await expect(panel.getByText("Metrics unavailable", { exact: true })).toBeVisible();
          await expect(panel.getByText("source unavailable", { exact: true })).toBeVisible();
          await expect(panel.getByText("Aggregate Unavailable CPU, Unavailable memory", { exact: true })).toBeVisible();
          await expect(panel.getByRole("meter", { name: /^(CPU|Memory) in use$/ })).toHaveCount(0);
          await expect(panel.getByRole("region", { name: "Recent warnings", exact: true })).toHaveCount(0);
          const health = panel.getByRole("region", { name: "Cluster health", exact: true });
          await expect(health.locator("article")).toHaveCount(4);
          await expect(health.getByText("Warnings", { exact: true })).toHaveCount(0);
          for (const notice of ["Usage ranking is partial.", "Change activity reflects bounded history; view the timeline for more.", "The issue summary is bounded; view all issues for more results.", "Relationship summary is bounded or partially synchronized; view all relationships for details."]) {
            await expect(panel.getByText(notice, { exact: true })).toHaveCount(0);
          }
          expect(eventRequests).toEqual([]);
        }
        if (view.id === "topology") {
          const namespaces: KubernetesResourcePage = await (await namespaceResponse!).json();
          const blocks = panel.getByRole("group", { name: "Namespace blocks", exact: true });
          await expect(blocks.getByRole("button", { name: "Open namespace payments topology", exact: true })).toBeVisible();
          await expect(blocks.getByRole("button", { name: "Open namespace inventory-only topology", exact: true })).toBeVisible();
          expect(await blocks.getByRole("button").evaluateAll((buttons) => buttons.map((button) => button.textContent?.trim())))
            .toEqual([...new Set((namespaces.items ?? []).map((resource) => resource.name))].sort());
          await expect(panel.getByLabel("Scrollable topology graph", { exact: true })).toHaveCount(0);
          expect(graphRequests).toEqual([]);
          fs.mkdirSync(screenshotDir, { recursive: true });
          await page.screenshot({ path: path.join(screenshotDir, `topology-namespace-picker-${viewport.name}.png`), fullPage: true, animations: "disabled" });
          const graphResponse = page.waitForResponse((response) => new URL(response.url()).pathname === "/api/admin/kubernetes/graph");
          await blocks.getByRole("button", { name: "Open namespace payments topology", exact: true }).click();
          const response = await graphResponse;
          expect(response.status()).toBe(200);
          const data: KubernetesGraph = await response.json();
          expect(data.truncated ?? false).toBe(false);
          expect(data.sync.partial).toBe(false);
          expect(data.partial_failures ?? []).toEqual([]);
          expect(Object.values(data.omitted).some((count) => count > 0)).toBe(false);
          expect(data.next ?? "").toBe("");
          for (const kind of ["Deployment", "Pod", "Service"]) {
            expect(data.nodes.some((node) => node.kind === kind && node.namespace === "payments" && node.name === (kind === "Pod" ? "checkout-api-0" : "checkout-api"))).toBe(true);
          }
          expect(data.edges.some((edge) => edge.type === "manages")).toBe(true);
          expect(data.edges.some((edge) => edge.type === "exposes")).toBe(true);
          const connectedIds = new Set(data.edges.flatMap((edge) => [edge.from, edge.to]));
          expect(data.nodes.every((node) => connectedIds.has(node.id))).toBe(true);
          expect(data.nodes.every((node) => !node.namespace || node.namespace === "payments")).toBe(true);
          for (const url of graphRequests) {
            expect(Object.fromEntries(url.searchParams)).toEqual({ namespace: "payments", complete: "true", connected_only: "true" });
          }
          await expect(panel.getByLabel("Scrollable topology graph", { exact: true })).toBeVisible();
          await expectDefaultFilters(panel, data);
          await panel.getByRole("button", { name: "Show all", exact: true }).click();
          await expect(panel.getByText(`${data.nodes.length} / ${data.nodes.length} connected resources visible`, { exact: false })).toContainText(`${data.edges.length} / ${data.edges.length} relationships`);
          await expect(panel.locator("button[data-node-id]")).toHaveCount(data.nodes.length);
          await expect(panel.locator("svg g[data-edge-from][data-edge-to]")).toHaveCount(data.edges.length);
          await expectNoTopologyPagination(panel);
          await expect(panel).not.toContainText(/omitted|graph is partial|namespace index or collection is partial/i);
          const podFilter = panel.getByRole("button", { name: "Hide Pod", exact: true });
          const podCount = data.nodes.filter((node) => node.kind === "Pod").length;
          await expect(podFilter).toHaveAttribute("aria-pressed", "true");
          await podFilter.click();
          await expect(panel.locator("button[data-node-id]")).toHaveCount(data.nodes.length - podCount);
          const filteredIds = new Set(data.nodes.filter((node) => node.kind !== "Pod").map((node) => node.id));
          const filteredEdges = data.edges.filter((edge) => filteredIds.has(edge.from) && filteredIds.has(edge.to));
          await expect(panel.getByText(`${data.nodes.length - podCount} / ${data.nodes.length} connected resources visible`, { exact: false })).toContainText(`${filteredEdges.length} / ${data.edges.length} relationships`);
          await panel.getByRole("button", { name: "Hide all", exact: true }).click();
          await expect(panel.getByText("No resources match these kind filters.", { exact: true })).toBeVisible();
          await panel.getByRole("button", { name: "Show all", exact: true }).click();
          await expect(panel.locator("button[data-node-id]")).toHaveCount(data.nodes.length);
          const graph = panel.getByLabel("Scrollable topology graph", { exact: true });
          const scale = () => graph.locator("div[style*='transform: scale']").evaluate((element) => Number(element.getAttribute("style")?.match(/scale\(([^)]+)\)/)?.[1]));
          const settledFit = await graph.evaluate((viewport) => {
            const content = viewport.querySelector<HTMLElement>("div[style*='transform: scale']")!;
            return Math.max(0.2, Math.min(1, (viewport.clientWidth - 32) / content.offsetWidth, (viewport.clientHeight - 32) / content.offsetHeight));
          });
          await panel.getByRole("button", { name: "Fit topology to view", exact: true }).click();
          await expect.poll(scale).toBeCloseTo(settledFit);
          const fittedScale = await scale();
          await panel.getByRole("button", { name: "Zoom in topology", exact: true }).click();
          expect(await scale()).toBeGreaterThan(fittedScale);
          await panel.getByRole("button", { name: "Zoom out topology", exact: true }).click();
          expect(await scale()).toBeCloseTo(fittedScale);
          for (let step = 0; step < 15; step++) await panel.getByRole("button", { name: "Zoom in topology", exact: true }).click();
          expect(await scale()).toBe(1.5);
          await panel.getByRole("button", { name: "Pan topology right", exact: true }).click();
          expect(await graph.evaluate((element) => element.scrollLeft)).toBeGreaterThan(0);
          await panel.getByRole("button", { name: "Pan topology left", exact: true }).click();
          expect(await graph.evaluate((element) => element.scrollLeft)).toBe(0);
          await panel.getByRole("button", { name: "Pan topology down", exact: true }).click();
          expect(await graph.evaluate((element) => element.scrollTop)).toBe(await graph.evaluate((element) => Math.min(180, element.scrollHeight - element.clientHeight)));
          await panel.getByRole("button", { name: "Pan topology up", exact: true }).click();
          expect(await graph.evaluate((element) => element.scrollTop)).toBe(0);
          await panel.getByRole("button", { name: "Pan topology right", exact: true }).click();
          const expectedFit = await graph.evaluate((viewport) => {
            const content = viewport.querySelector<HTMLElement>("div[style*='transform: scale']")!;
            return Math.max(0.2, Math.min(1, (viewport.clientWidth - 32) / content.offsetWidth, (viewport.clientHeight - 32) / content.offsetHeight));
          });
          await panel.getByRole("button", { name: "Fit topology to view", exact: true }).click();
          expect(await scale()).toBeCloseTo(expectedFit);
          expect(await graph.evaluate((element) => ({ left: element.scrollLeft, top: element.scrollTop }))).toEqual({ left: 0, top: 0 });
        }
        expect(await page.evaluate(() => document.scrollingElement?.scrollHeight ?? 0)).toBeLessThanOrEqual(viewport.height);
        expect(await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth)).toBe(false);
        await page.locator("main").evaluate((main) => main.scrollTo({ top: main.scrollHeight }));
        expect(await page.evaluate(() => document.documentElement.scrollHeight - document.documentElement.clientHeight)).toBeLessThanOrEqual(1);
        await page.locator("main").evaluate((main) => main.scrollTo({ top: 0 }));
        fs.mkdirSync(screenshotDir, { recursive: true });
        await page.screenshot({ path: path.join(screenshotDir, `${view.id}-${viewport.name}.png`), fullPage: true, animations: "disabled" });
        if (view.id === "issues") {
          const rows = panel.locator("article");
          expect(await rows.count()).toBeGreaterThan(0);
          const collisions = await rows.evaluateAll((articles) => articles.flatMap((article) => {
            const identity = article.querySelector("h3")?.parentElement;
            const metadata = article.lastElementChild;
            if (!identity || !metadata) return ["missing issue identity or metadata"];
            const identityBox = identity.getBoundingClientRect();
            const metadataBox = metadata.getBoundingClientRect();
            return identityBox.left < metadataBox.right && identityBox.right > metadataBox.left && identityBox.top < metadataBox.bottom && identityBox.bottom > metadataBox.top
              ? [identity.textContent] : [];
          }));
          expect(collisions).toEqual([]);
          await rows.last().scrollIntoViewIfNeeded();
          await expect(rows.last()).toBeInViewport();
          await page.screenshot({ path: path.join(screenshotDir, `issues-lower-${viewport.name}.png`), fullPage: true, animations: "disabled" });
        }
        if (view.id === "overview") {
          for (const [capture, heading] of [["capacity", "CPU capacity"], ["summary", "Grouped issues"], ["activity", "Recent changes (60m)"], ["traffic", "Service traffic"]] as const) {
            const target = panel.getByRole("heading", { name: heading, exact: true });
            await target.scrollIntoViewIfNeeded();
            await expect(target).toBeInViewport();
            await page.screenshot({ path: path.join(screenshotDir, `overview-${capture}-${viewport.name}.png`), fullPage: true, animations: "disabled" });
          }
          const nodesTab = page.getByRole("tab", { name: "Nodes", exact: true });
          await nodesTab.click();
          await expect(nodesTab).toHaveAttribute("aria-selected", "true");
          const nodes = panel.getByRole("region", { name: "Nodes", exact: true });
          await expect(nodes).toBeVisible();
          await expect(nodes.getByRole("heading", { name: "Nodes", exact: true })).toBeVisible();
          await nodes.scrollIntoViewIfNeeded();
          await page.screenshot({ path: path.join(screenshotDir, `overview-lower-${viewport.name}.png`), fullPage: true, animations: "disabled" });
        }
        if (view.id === "topology") {
          const graph = page.getByLabel("Scrollable topology graph", { exact: true });
          await graph.getByRole("button").first().scrollIntoViewIfNeeded();
          await expect(graph.getByRole("button").first()).toBeInViewport();
          const relationshipLabelOverlaps = await graph.evaluate((element) => {
            const nodes = [...element.querySelectorAll<HTMLButtonElement>("button[data-node-id]")];
            return [...element.querySelectorAll<SVGGElement>("svg g[data-edge-from][data-edge-to]")]
              .filter((edge) => {
                const from = nodes.find((node) => node.dataset.nodeId === edge.dataset.edgeFrom)?.getBoundingClientRect();
                const to = nodes.find((node) => node.dataset.nodeId === edge.dataset.edgeTo)?.getBoundingClientRect();
                return from && to && Math.abs(from.left - to.left) > 292;
              })
              .flatMap((edge) => {
                const label = edge.querySelector("rect")?.getBoundingClientRect();
                if (!label) return ["missing edge label"];
                return nodes.some((node) => {
                  const box = node.getBoundingClientRect();
                  return label.left < box.right && label.right > box.left && label.top < box.bottom && label.bottom > box.top;
                }) ? [edge.dataset.edgeFrom] : [];
              });
          });
          expect(relationshipLabelOverlaps).toEqual([]);
          if (viewport.name === "mobile") {
            for (let step = 0; step < 8; step++) await panel.getByRole("button", { name: "Zoom in topology", exact: true }).click();
          }
          for (const [name, kind, title] of [["workloads", "Deployment", /^Deployment payments\/checkout-api$/], ["runtime", "Pod", /^Pod payments\/checkout-api-0$/], ["service", "Service", /^Service payments\/checkout-api$/]] as const) {
            await panel.getByRole("button", { name: "Fit topology to view", exact: true }).click();
            const target = graph.getByRole("button", { name: `Open ${kind} payments/${kind === "Pod" ? "checkout-api-0" : "checkout-api"} details`, exact: true });
            await expect(target).toHaveAttribute("title", title);
            await target.scrollIntoViewIfNeeded();
            await expect(target).toBeInViewport();
            await page.screenshot({ path: path.join(screenshotDir, `topology-${name}-${viewport.name}.png`), fullPage: true, animations: "disabled" });
            await target.click();
            const detail = page.getByRole("dialog", { name: "Details panel", exact: true });
            await expect(detail.getByRole("heading", { name: `${kind} payments/${kind === "Pod" ? "checkout-api-0" : "checkout-api"}`, exact: true })).toBeVisible();
            await expect(detail).not.toContainText(/secret-token|super-secret-value|must-not-cross|c2VjcmV0LXRva2Vu/);
            await page.keyboard.press("Escape");
            await expect(detail).toHaveCount(0);
          }
          await expect(panel.getByRole("combobox", { name: "Topology namespace", exact: true })).toHaveValue("payments");
        }
      });
    }
    test(`captures logs ${viewport.name}`, async ({ page, context }) => {
      await page.setViewportSize({ width: viewport.width, height: viewport.height });
      await context.grantPermissions(["clipboard-read", "clipboard-write"]);
      await openApp(page, "/agent/kubernetes?view=overview");
      await expect(page.getByText("Nodes ready 0/1")).toBeVisible();
      const workloadsTab = page.getByRole("tab", { name: "Workloads", exact: true });
      await workloadsTab.click();
      await expect(workloadsTab).toHaveAttribute("aria-selected", "true");
      await page.getByLabel("Workload namespace").fill("payments");
      await page.getByLabel("Resource name").fill("checkout-api-0");
      const streamResponse = page.waitForResponse((response) => new URL(response.url()).pathname.endsWith("/logs/stream"));
      await page.getByRole("button", { name: "Select Pod payments/checkout-api-0", exact: true }).click();
      const detail = page.getByRole("dialog", { name: "Details panel" });
      await expect(detail).toBeVisible();
      await expect(detail.getByRole("heading", { name: "Pod payments/checkout-api-0", exact: true })).toBeVisible();
      await detail.getByRole("tab", { name: "Events", exact: true }).click();
      await expect(detail.getByText("No object-scoped events were returned.", { exact: true })).toBeVisible();
      fs.mkdirSync(screenshotDir, { recursive: true });
      await page.screenshot({ path: path.join(screenshotDir, `drawer-events-${viewport.name}.png`), fullPage: true, animations: "disabled" });
      await detail.getByRole("tab", { name: "Logs", exact: true }).click();
      const streamed = await streamResponse;
      expect(streamed.status()).toBe(200);
      expect(streamed.headers()["content-type"]).toMatch(/^text\/event-stream(?:;|$)/);
      expect(new URL(streamed.url()).searchParams.get("timestamps")).toBe("false");
      const output = detail.getByLabel("Pod log output");
      await expect(detail.getByLabel("Log connection status")).toHaveText("live");
      await expect(output).toContainText("REDACTED");
      await expect(detail).not.toContainText("super-secret-value");
      if (viewport.name === "mobile") await detail.getByRole("button", { name: "More", exact: true }).click();
      await detail.getByLabel("Pause logs").click();
      const timestampPattern = /\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z/g;
      for (const enabled of [true, false, true]) {
        await detail.getByLabel("Timestamps").setChecked(enabled);
        const rendered = (await output.locator("[data-log-key]").allTextContents()).map((line) => line.replace(/\n$/, "")).join("\n");
        const lines = rendered.split("\n");
        expect(lines.length).toBeGreaterThan(0);
        for (const line of lines) expect(line.match(timestampPattern) ?? []).toHaveLength(enabled ? 1 : 0);
        await detail.getByLabel("Copy scrubbed logs").click();
        expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(rendered);
        const download = page.waitForEvent("download");
        await detail.getByLabel("Download scrubbed logs").click();
        const file = await download;
        expect(file.suggestedFilename()).toBe("checkout-api-0-logs.txt");
        expect(fs.readFileSync((await file.path())!, "utf8")).toBe(rendered);
      }
      await expect(detail.getByLabel("Log connection status")).toHaveText("paused");
      await expect(detail.getByRole("alert")).toHaveCount(0);
      fs.mkdirSync(screenshotDir, { recursive: true });
      await page.screenshot({ path: path.join(screenshotDir, `logs-${viewport.name}.png`), fullPage: true, animations: "disabled" });
    });
    for (const failure of ["incomplete", "denied"] as const) {
      test(`rejects ${failure} topology ${viewport.name} (isolated response fixture)`, async ({ page }) => {
        await page.setViewportSize({ width: viewport.width, height: viewport.height });
        let graphCalls = 0;
        await page.route("**/api/admin/kubernetes/graph?*", async (route) => {
          graphCalls++;
          const url = new URL(route.request().url());
          expect(Object.fromEntries(url.searchParams)).toEqual({ namespace: "payments", complete: "true", connected_only: "true" });
          if (failure === "denied") {
            await route.fulfill({ status: 403, json: { error: "Synthetic namespace inventory access denied", code: "forbidden" } });
          } else {
            const response = await route.fetch();
            expect(response.status()).toBe(200);
            const data: KubernetesGraph = await response.json();
            await route.fulfill({ response, json: { ...data, sync: { ...data.sync, partial: true }, omitted: { Pod: 1 } } });
          }
        });
        await openApp(page, "/agent/kubernetes?view=topology");
        const panel = page.getByRole("region", { name: "Kubernetes topology", exact: true });
        const namespace = panel.getByRole("button", { name: "Open namespace payments topology", exact: true });
        await expect(namespace).toBeVisible();
        expect(graphCalls).toBe(0);
        await namespace.click();
        if (failure === "denied") {
          await expect(panel.getByRole("alert")).toContainText("Synthetic namespace inventory access denied");
          await expect(panel.getByRole("button", { name: "Retry topology", exact: true })).toHaveCount(0);
        } else {
          await expect(panel.getByRole("alert")).toContainText("The complete connected graph could not be verified");
        }
        expect(graphCalls).toBe(1);
        await expect(panel.getByLabel("Scrollable topology graph", { exact: true })).toHaveCount(0);
        await expect(panel.locator("button[data-node-id]")).toHaveCount(0);
        await expectNoTopologyPagination(panel);
        expect(await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth)).toBe(false);
        fs.mkdirSync(screenshotDir, { recursive: true });
        await page.screenshot({ path: path.join(screenshotDir, `topology-${failure}-fixture-${viewport.name}.png`), fullPage: true, animations: "disabled" });
      });
    }
  }
});

for (const format of [
  { name: "facebook-3x", viewportWidth: 2000, viewportHeight: 1050, pixelRatio: 1.8 },
  { name: "linkedin-3x", viewportWidth: 2000, viewportHeight: 1045, pixelRatio: 1.8 },
  { name: "desktop-3x", viewportWidth: 1440, viewportHeight: 1000, pixelRatio: 3 },
] as const) {
  test.describe(`Kubernetes overview summary social ${format.name}`, () => {
    test.use({ viewport: { width: format.viewportWidth, height: format.viewportHeight }, deviceScaleFactor: format.pixelRatio });
    test.skip(process.env.E2E_KUBERNETES_BACKEND !== "fake", "requires the harness fake Kubernetes backend");

    test("captures overview summary social image", async ({ page }) => {
      const observedAt = new Date().toISOString();
      const pods = [
        { kind: "Pod" as const, namespace: "payments", name: "checkout-api-0", cpu: "850m", memory: "768Mi", window: "30s" },
        { kind: "Pod" as const, namespace: "payments", name: "payment-worker-0", cpu: "620m", memory: "512Mi", window: "30s" },
        { kind: "Pod" as const, namespace: "payments", name: "checkout-api-1", cpu: "410m", memory: "384Mi", window: "30s" },
        { kind: "Pod" as const, namespace: "payments", name: "redis-0", cpu: "240m", memory: "256Mi", window: "30s" },
      ];
      await page.route((url) => url.pathname === "/api/admin/kubernetes/overview", async (route) => {
        const response = await route.fetch();
        expect(response.status()).toBe(200);
        const overview: KubernetesOverview = await response.json();
        await route.fulfill({ response, json: {
          ...overview, pods: pods.length, running_pods: pods.length,
          requested_cpu: "3", limited_cpu: "6", allocatable_cpu: "8",
          requested_memory: "4Gi", limited_memory: "8Gi", allocatable_memory: "16Gi",
        } satisfies KubernetesOverview });
      });
      await page.route((url) => url.pathname === "/api/admin/kubernetes/usage", (route) => route.fulfill({ json: {
        observed_at: observedAt, availability: "available", fresh: true, truncated: false,
        node_metrics: { availability: "available", fresh: true, complete: true, total: 1, cpu: "2120m", memory: "1920Mi", observed_at: observedAt },
        pod_metrics: { availability: "available", fresh: true, complete: true, total: pods.length, cpu: "2120m", memory: "1920Mi", observed_at: observedAt },
        pods, nodes: [],
      } satisfies KubernetesUsage }));
      await page.route((url) => url.pathname === "/api/admin/kubernetes/top", (route) => route.fulfill({ json: {
        items: pods, total: pods.length, truncated: false, availability: "available", fresh: true,
        sync: { state: "ready", age_s: 0, partial: false },
      } satisfies KubernetesTopPage }));
      await page.route((url) => url.pathname === "/api/admin/kubernetes/traffic", (route) => route.fulfill({ json: {
        available: true, source: "synthetic", window: "15m", observed_at: observedAt, unmapped: 0, truncated: false,
        edges: [
          { from: { namespace: "payments", kind: "Service", name: "checkout-api" }, to: { namespace: "payments", kind: "Service", name: "payment-worker" }, rate_per_sec: 128.4, error_rate: 0.012, p95_ms: 84 },
          { from: { namespace: "payments", kind: "Service", name: "payment-worker" }, to: { namespace: "payments", kind: "Service", name: "redis" }, rate_per_sec: 256.8, error_rate: 0.002, p95_ms: 12 },
        ],
      } satisfies KubernetesTraffic }));
      await openApp(page, "/agent/kubernetes?view=overview");
      const summary = page.getByRole("region", { name: "Triage summary", exact: true });
      await expect(summary.getByRole("heading", { name: "Grouped issues", exact: true })).toBeVisible();
      await expect(summary.getByRole("status", { name: /^Loading/ })).toHaveCount(0);
      await expect(summary.getByText("4 total", { exact: true })).toBeVisible();
      await expect(page.getByText("Metrics available", { exact: true })).toBeVisible();
      await expect(page.getByRole("meter", { name: /^(CPU|Memory) in use$/ })).toHaveCount(2);
      await expect(summary.getByText("payments/checkout-api-0", { exact: true })).toBeVisible();
      await expect(summary.getByText("256.8 req/s", { exact: true })).toBeVisible();
      await expect(summary.getByRole("button", { name: "View all traffic", exact: true })).toBeVisible();
      if (format.name === "desktop-3x") {
        await summary.getByRole("heading", { name: "Grouped issues", exact: true }).scrollIntoViewIfNeeded();
      }
      await expect(page.getByRole("region", { name: "Cluster health", exact: true })).toBeInViewport({ ratio: 1 });
      await expect(page.getByRole("heading", { name: "CPU capacity", exact: true })).toBeInViewport();
      expect(await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth)).toBe(false);
      fs.mkdirSync(screenshotDir, { recursive: true });
      const image = await page.screenshot({ path: path.join(screenshotDir, `overview-summary-${format.name}.png`), fullPage: false, scale: "device", animations: "disabled" });
      expect(image.readUInt32BE(16)).toBe(Math.round(format.viewportWidth * format.pixelRatio));
      expect(image.readUInt32BE(20)).toBe(Math.round(format.viewportHeight * format.pixelRatio));
    });

    if (format.name === "desktop-3x") {
      test("captures topology desktop 3x image", async ({ page }) => {
        await openApp(page, "/agent/kubernetes?view=topology");
        const panel = page.getByRole("region", { name: "Kubernetes topology", exact: true });
        const graphResponse = page.waitForResponse((response) => new URL(response.url()).pathname === "/api/admin/kubernetes/graph");
        await panel.getByRole("button", { name: "Open namespace payments topology", exact: true }).click();
        const response = await graphResponse;
        expect(response.status()).toBe(200);
        const data: KubernetesGraph = await response.json();
        expect(data.nodes.length).toBeGreaterThan(0);
        expect(data.edges.length).toBeGreaterThan(0);
        expect(data.sync.partial).toBe(false);
        await expectDefaultFilters(panel, data);
        await panel.getByRole("button", { name: "Show all", exact: true }).click();
        await expectGraphCounts(panel, data);
        await expect(panel.locator("button[data-node-id]")).toHaveCount(data.nodes.length);
        await expect(panel.locator("svg g[data-edge-from][data-edge-to]")).toHaveCount(data.edges.length);
        await panel.getByRole("button", { name: "Fit topology to view", exact: true }).click();
        await expect(panel.getByLabel("Scrollable topology graph", { exact: true })).toBeInViewport();
        expect(await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth)).toBe(false);
        fs.mkdirSync(screenshotDir, { recursive: true });
        const image = await page.screenshot({ path: path.join(screenshotDir, "topology-desktop-3x.png"), fullPage: false, scale: "device", animations: "disabled" });
        expect(image.readUInt32BE(16)).toBe(4320);
        expect(image.readUInt32BE(20)).toBe(3000);
      });
    }
  });
}

test.describe("Inventory overview acceptance", () => {
  test.skip(process.env.E2E_KUBERNETES_BACKEND !== "fake", "requires the real harness fake Kubernetes backend");

  for (const viewport of [{ name: "desktop", width: 1440, height: 1100 }, { name: "mobile", width: 390, height: 844 }]) {
    test(`uses inventory counts and a fixed schematic without graph reads ${viewport.name}`, async ({ page }, testInfo) => {
      await page.setViewportSize({ width: viewport.width, height: viewport.height });
      const graphReads: URL[] = [];
      page.on("request", (request) => {
        const url = new URL(request.url());
        if (/\/kubernetes\/(?:graph|overview\/graph)/.test(url.pathname)) graphReads.push(url);
      });
      await openApp(page, "/agent/kubernetes?view=overview");
      const panel = page.getByRole("region", { name: "Overview topology", exact: true });
      const schematic = panel.getByRole("img", { name: "Kubernetes relationship schematic", exact: true });
      await expect(schematic).toBeVisible();
      await expect(schematic).toHaveAttribute("viewBox", "0 0 700 370");
      await expect(schematic.locator('[data-topology-role="namespace"]')).toHaveCount(2);
      await expect(schematic.locator('[data-topology-role="pod"]')).toHaveCount(4);
      await expect(panel).toContainText("not observed cluster edges");
      await expect(panel.locator("[data-node-id]")).toHaveCount(0);
      const inventoryCounts: Record<string, number> = {};
      for (const [kind, resourceId] of Object.entries({ Namespace: "core~v1~namespaces", Node: "core~v1~nodes", Pod: "core~v1~pods", Service: "core~v1~services", Deployment: "apps~v1~deployments" })) {
        let cursor = "";
        let count = 0;
        do {
          const response = await page.request.get("/api/admin/kubernetes/resources", { params: { resource_id: resourceId, limit: "200", ...(cursor ? { continue: cursor } : {}) } });
          expect(response.status()).toBe(200);
          const data: KubernetesResourcePage = await response.json();
          expect(data.partial_failures ?? []).toEqual([]);
          expect(data.truncated && !data.continue).toBe(false);
          count += (data.items ?? []).length;
          cursor = data.continue || "";
        } while (cursor);
        inventoryCounts[kind] = count;
        const term = panel.getByLabel("Cluster resource counts").locator("dt").filter({ hasText: new RegExp(`^${kind}$`) });
        await expect(term.locator("..").locator("dd")).toHaveText(count.toLocaleString());
      }
      expect(inventoryCounts.Pod).toBeGreaterThan(4);
      await expect(schematic.locator('[data-topology-role="namespace"]')).toHaveCount(2);
      await page.getByRole("button", { name: "Refresh Kubernetes data", exact: true }).click();
      await expect(schematic).toBeVisible();
      expect(graphReads).toEqual([]);
      await panel.scrollIntoViewIfNeeded();
      expect(await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth)).toBe(false);
      fs.mkdirSync(screenshotDir, { recursive: true });
      await page.screenshot({ path: path.join(screenshotDir, `inventory-overview-${viewport.name}.png`), fullPage: true, animations: "disabled" });
      await testInfo.attach("inventory-counts", { body: JSON.stringify(inventoryCounts), contentType: "application/json" });
      await panel.getByRole("button", { name: "View all relationships", exact: true }).click();
      await expect(page.getByRole("tab", { name: "Topology", exact: true })).toHaveAttribute("aria-selected", "true");
      const topology = page.getByRole("region", { name: "Kubernetes topology", exact: true });
      await expect(topology.getByRole("group", { name: "Namespace blocks", exact: true })).toBeVisible();
      expect(graphReads).toEqual([]);
      const namespace = process.env.E2E_KUBERNETES_DOCS === "1" ? "shop" : "payments";
      const loaded = page.waitForResponse((response) => new URL(response.url()).pathname === "/api/admin/kubernetes/graph");
      await topology.getByRole("button", { name: `Open namespace ${namespace} topology`, exact: true }).click();
      const response = await loaded;
      expect(response.status()).toBe(200);
      const graph: KubernetesGraph = await response.json();
      expect(graph.nodes.length).toBeGreaterThan(0);
      expect(graph.nodes.every((node) => !node.namespace || node.namespace === namespace)).toBe(true);
      await expectDefaultFilters(topology, graph);
      for (const url of graphReads) expect(Object.fromEntries(url.searchParams)).toEqual({ namespace, complete: "true", connected_only: "true" });
      await topology.getByRole("button", { name: "Namespaces", exact: true }).click();
      await expect(topology.getByRole("group", { name: "Namespace blocks", exact: true })).toBeVisible();
    });
  }
});

test.describe("Kubernetes repaired snapshot delta", () => {
  test.skip(process.env.E2E_KUBERNETES_BACKEND !== "fake", "requires the harness fake Kubernetes backend");

  for (const viewport of [{ name: "desktop", width: 1440, height: 1000 }, { name: "mobile", width: 390, height: 844 }]) {
    test(`loads independent all-namespace overview before visiting topology ${viewport.name}`, async ({ page }) => {
      await page.setViewportSize({ width: viewport.width, height: viewport.height });
      const overviewRequests: URL[] = [];
      const namespaceRequests: URL[] = [];
      page.on("request", (request) => {
        const url = new URL(request.url());
        if (url.pathname === "/api/admin/kubernetes/graph/overview") overviewRequests.push(url);
        if (url.pathname === "/api/admin/kubernetes/graph") namespaceRequests.push(url);
      });
      const loaded = page.waitForResponse((response) => new URL(response.url()).pathname === "/api/admin/kubernetes/graph/overview");
      await openApp(page, "/agent/kubernetes?view=overview");
      const response = await loaded;
      expect(response.status()).toBe(200);
      const data: KubernetesGraph = await response.json();
      expect(data.sync.partial).toBe(false);
      expect(data.truncated ?? false).toBe(false);
      expect(data.next ?? "").toBe("");
      expect(data.partial_failures ?? []).toEqual([]);
      expect(Object.values(data.omitted).every((count) => count === 0)).toBe(true);
      const ids = new Set(data.nodes.map((node) => node.id));
      const connectedIds = new Set(data.edges.flatMap((edge) => [edge.from, edge.to]));
      expect(ids.size).toBe(data.nodes.length);
      expect(data.nodes.length).toBeGreaterThan(0);
      expect(data.nodes.every((node) => connectedIds.has(node.id))).toBe(true);
      expect(data.edges.every((edge) => ids.has(edge.from) && ids.has(edge.to))).toBe(true);
      expect(namespaceRequests).toEqual([]);
      expect(overviewRequests).toHaveLength(1);
      expect(overviewRequests[0].search).toBe("");
      const overview = page.getByRole("region", { name: "Overview topology", exact: true });
      await expectGraphCounts(overview, data);
      await expectNoTopologyPagination(overview);
      await expect(overview.getByLabel("Scrollable overview topology graph", { exact: true })).toBeVisible();
      const wholeCounts = await overview.getByText(/connected resources visible/).textContent();
      fs.mkdirSync(screenshotDir, { recursive: true });
      await overview.scrollIntoViewIfNeeded();
      await page.screenshot({ path: path.join(screenshotDir, `overview-${viewport.name}.png`), fullPage: true, animations: "disabled" });
      await overview.locator("footer").scrollIntoViewIfNeeded();
      await page.locator("main").evaluate((main) => main.scrollBy({ top: 180 }));
      await expect(overview.locator("footer")).toBeInViewport();
      await page.screenshot({ path: path.join(screenshotDir, `overview-lower-${viewport.name}.png`), fullPage: true, animations: "disabled" });
      const inventoryLoaded = page.waitForResponse((result) => {
        const url = new URL(result.url());
        return url.pathname === "/api/admin/kubernetes/resources" && url.searchParams.get("resource_id") === "core~v1~namespaces";
      });
      await page.getByRole("tab", { name: "Topology", exact: true }).click();
      const topology = page.getByRole("region", { name: "Kubernetes topology", exact: true });
      const inventoryResponse = await inventoryLoaded;
      expect(inventoryResponse.status()).toBe(200);
      const inventory: KubernetesResourcePage = await inventoryResponse.json();
      const blocks = topology.getByRole("group", { name: "Namespace blocks", exact: true });
      expect(await blocks.getByRole("button").allTextContents()).toEqual([...new Set((inventory.items ?? []).map((item) => item.name))].sort());
      const search = topology.getByRole("textbox", { name: "Search topology namespaces", exact: true });
      await search.fill("PAYMENTS");
      await expect(blocks.getByRole("button")).toHaveCount(1);
      await search.fill("");
      const payments = blocks.getByRole("button", { name: "Open namespace payments topology", exact: true });
      await payments.focus();
      await expect(payments).toBeFocused();
      const namespaceLoaded = page.waitForResponse((result) => new URL(result.url()).pathname === "/api/admin/kubernetes/graph");
      await page.keyboard.press("Enter");
      const namespaceData: KubernetesGraph = await (await namespaceLoaded).json();
      expect(namespaceData.nodes.every((node) => !node.namespace || node.namespace === "payments")).toBe(true);
      await expectDefaultFilters(topology, namespaceData);
      const sidebar = topology.getByRole("complementary", { name: "Topology filters", exact: true });
      const podToggle = sidebar.getByRole("button", { name: "Hide Pod", exact: true });
      await podToggle.focus();
      await page.keyboard.press("Space");
      await expect(sidebar.getByRole("button", { name: "Show Pod", exact: true })).toHaveAttribute("aria-pressed", "false");
      await expectGraphCounts(topology, namespaceData, defaultKinds.filter((kind) => kind !== "Pod"));
      await sidebar.getByRole("button", { name: "Show all", exact: true }).click();
      await expectGraphCounts(topology, namespaceData);
      const refreshed = page.waitForResponse((result) => new URL(result.url()).pathname === "/api/admin/kubernetes/graph");
      await sidebar.getByRole("button", { name: "Refresh topology", exact: true }).click();
      expect((await refreshed).status()).toBe(200);
      await expectGraphCounts(topology, namespaceData);
      if (viewport.name === "mobile") {
        for (const group of ["Networking", "Workloads", "Configuration"]) {
          const summary = sidebar.locator("summary", { hasText: group });
          const details = summary.locator("..");
          await expect(details).toHaveAttribute("open", "");
          await summary.click();
          await expect(details).not.toHaveAttribute("open", "");
          await expect(summary).toBeVisible();
          await expect(details.getByRole("button").first()).not.toBeVisible();
        }
      }
      await sidebar.scrollIntoViewIfNeeded();
      await expect(topology.getByLabel("Scrollable topology graph", { exact: true })).toBeInViewport();
      await page.screenshot({ path: path.join(screenshotDir, `topology-${viewport.name}.png`), fullPage: true, animations: "disabled" });
      for (const url of namespaceRequests) expect(Object.fromEntries(url.searchParams)).toEqual({ namespace: "payments", complete: "true", connected_only: "true" });
      await page.getByRole("tab", { name: "Overview", exact: true }).click();
      await expectGraphCounts(overview, data);
      await expect(overview.getByText(/connected resources visible/)).toHaveText(wholeCounts!);
      expect(overviewRequests).toHaveLength(1);
      await expect(overview.getByRole("complementary", { name: "Topology filters", exact: true })).toHaveCount(0);
      expect(await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth)).toBe(false);
      await expect(overview).not.toContainText(/secret-token|super-secret-value|must-not-cross|c2VjcmV0LXRva2Vu/);
      await page.getByRole("tab", { name: "Topology", exact: true }).click();
      await expect(topology.getByRole("combobox", { name: "Topology namespace", exact: true })).toHaveValue("");
      await expect(blocks).toBeVisible();
      await expect(payments).toBeVisible();
      await expect(topology.getByLabel("Scrollable topology graph", { exact: true })).toHaveCount(0);
      await payments.click();
      await expect(topology.getByRole("combobox", { name: "Topology namespace", exact: true })).toHaveValue("payments");
      await expectDefaultFilters(topology, namespaceData);
      expect(overviewRequests).toHaveLength(1);
    });

    test(`refreshes a loaded two-namespace overview independently ${viewport.name}`, async ({ page }) => {
      await page.setViewportSize({ width: viewport.width, height: viewport.height });
      let releaseInitial!: () => void;
      const initialGate = new Promise<void>((resolve) => { releaseInitial = resolve; });
      let calls = 0;
      let refreshed = false;
      const fixture = (fresh: boolean): KubernetesGraph => {
        const nodes = ["payments", "inventory-only"].flatMap((namespace) => [
          { id: `${namespace}-deployment`, kind: "Deployment", namespace, name: `synthetic-${fresh ? "fresh" : "initial"}`, group: namespace },
          { id: `${namespace}-pod`, kind: "Pod", namespace, name: `synthetic-${fresh ? "fresh" : "initial"}-pod`, group: namespace },
          ...(fresh ? [{ id: `${namespace}-config`, kind: "ConfigMap", namespace, name: "synthetic-fresh-config", group: namespace }] : []),
        ]);
        const edges = ["payments", "inventory-only"].flatMap((namespace) => [
          { from: `${namespace}-deployment`, to: `${namespace}-pod`, type: "manages" as const },
          ...(fresh ? [{ from: `${namespace}-pod`, to: `${namespace}-config`, type: "uses" as const }] : []),
        ]);
        return { nodes, edges, omitted: {}, sync: { state: "ready", age_s: 0, partial: false } };
      };
      await page.route((url) => url.pathname === "/api/admin/kubernetes/graph/overview", async (route) => {
        calls++;
        expect(new URL(route.request().url()).search).toBe("");
        if (calls === 1) await initialGate;
        await route.fulfill({ json: fixture(refreshed) });
      });
      await openApp(page, "/agent/kubernetes?view=overview");
      const overview = page.getByRole("region", { name: "Overview topology", exact: true });
      await expect(overview.getByRole("status", { name: "Loading complete topology for all namespaces", exact: true })).toBeVisible();
      await expect(overview.locator("button[data-node-id]")).toHaveCount(0);
      releaseInitial();
      await expectGraphCounts(overview, fixture(false));
      await expect(overview.locator("button[data-node-id]")).toHaveCount(4);
      await expect(overview.locator("svg g[data-edge-from][data-edge-to]")).toHaveCount(2);
      await page.getByRole("tab", { name: "Topology", exact: true }).click();
      const topology = page.getByRole("region", { name: "Kubernetes topology", exact: true });
      await topology.getByRole("button", { name: "Open namespace payments topology", exact: true }).click();
      await topology.getByRole("button", { name: "Hide Pod", exact: true }).click();
      await topology.getByRole("combobox", { name: "Topology namespace", exact: true }).selectOption("inventory-only");
      await expect(topology.getByText("No connected resources were returned for this namespace.", { exact: true })).toBeVisible();
      await page.getByRole("tab", { name: "Overview", exact: true }).click();
      await expectGraphCounts(overview, fixture(false));
      expect(calls).toBe(1);
      refreshed = true;
      await page.getByRole("button", { name: "Refresh Kubernetes data", exact: true }).click();
      await expectGraphCounts(overview, fixture(true));
      expect(calls).toBe(2);
      await expect(overview.locator("button[data-node-id]")).toHaveCount(6);
      await expect(overview.locator("svg g[data-edge-from][data-edge-to]")).toHaveCount(4);
      for (const namespace of ["payments", "inventory-only"]) {
        await expect(overview.getByRole("button", { name: `Open ConfigMap ${namespace}/synthetic-fresh-config details`, exact: true })).toHaveCount(1);
      }
      await expect(overview).not.toContainText("synthetic-initial");
      await expect(overview.getByRole("complementary", { name: "Topology filters", exact: true })).toHaveCount(0);
      await expectNoTopologyPagination(overview);
      await overview.scrollIntoViewIfNeeded();
      fs.mkdirSync(screenshotDir, { recursive: true });
      await page.screenshot({ path: path.join(screenshotDir, `delta-overview-fresh-${viewport.name}.png`), fullPage: true, animations: "disabled" });
    });
  }

  for (const failure of ["incomplete", "missing-sync", "malformed", "denied", "cached-401"] as const) {
    test(`fails closed for ${failure} all-namespace overview`, async ({ page }) => {
      let deny = failure === "denied";
      await page.route((url) => url.pathname === "/api/admin/kubernetes/graph/overview", async (route) => {
        expect(new URL(route.request().url()).search).toBe("");
        if (deny) return route.fulfill({ status: failure === "cached-401" ? 401 : 403, json: { error: "Synthetic overview permission denied", code: "forbidden" } });
        const response = await route.fetch();
        expect(response.status()).toBe(200);
        const data: KubernetesGraph = await response.json();
        await route.fulfill({ response, json: failure === "incomplete" ? { ...data, sync: { ...data.sync, partial: true }, omitted: { Pod: 1 } } : failure === "missing-sync" ? { ...data, sync: undefined } : failure === "malformed" ? { ...data, nodes: null } : data });
      });
      await openApp(page, "/agent/kubernetes?view=overview");
      const overview = page.getByRole("region", { name: "Overview topology", exact: true });
      if (failure === "cached-401") {
        await expect(overview.locator("button[data-node-id]").first()).toBeVisible();
        deny = true;
        await page.getByRole("button", { name: "Refresh Kubernetes data", exact: true }).click();
      }
      await expect(overview.getByRole("alert")).toContainText(failure === "incomplete" || failure === "missing-sync" || failure === "malformed" ? "The complete connected graph could not be verified" : "Check sign-in, license, and Kubernetes graph permissions");
      await expect(overview.locator("button[data-node-id]")).toHaveCount(0);
      await expect(overview.getByLabel("Scrollable overview topology graph", { exact: true })).toHaveCount(0);
      await expect(overview.getByRole("button", { name: "Retry topology", exact: true })).toHaveCount(0);
      await expectNoTopologyPagination(overview);
      fs.mkdirSync(screenshotDir, { recursive: true });
      await overview.scrollIntoViewIfNeeded();
      await page.screenshot({ path: path.join(screenshotDir, `delta-overview-${failure}-desktop.png`), fullPage: true, animations: "disabled" });
    });
  }

  test("keeps fresh complete usage coherent over stale overview and capped samples", async ({ page }) => {
    let usageCalls = 0;
    let freshNodes = false;
    await page.route("**/api/admin/kubernetes/overview", async (route) => {
      const response = await route.fetch();
      await route.fulfill({ response, json: { ...await response.json(), usage_cpu: "99", usage_memory: "999Gi", usage_source: "pod_metrics", metrics_status: "stale", metrics_fresh: false, metrics_observed_at: "2026-08-30T11:00:00Z" } });
    });
    await page.route((url) => url.pathname === "/api/admin/kubernetes/usage", (route) => {
      usageCalls++;
      expect(new URL(route.request().url()).searchParams.has("namespace")).toBe(true);
      return route.fulfill({ json: {
      observed_at: "2026-08-30T12:00:00Z", availability: "available", fresh: !freshNodes,
      node_metrics: { availability: "available", fresh: true, complete: true, total: 5, cpu: "500m", memory: "512Mi", observed_at: "2026-08-30T11:59:00Z" },
      pod_metrics: { availability: "available", fresh: !freshNodes, complete: true, total: 1300, cpu: "750m", memory: "2Gi", observed_at: "2026-08-30T12:00:00Z" },
      pods: [{ kind: "Pod", namespace: "payments", name: "sample-only", cpu: "1", memory: "4Gi" }], nodes: [], truncated: true,
      } satisfies KubernetesUsage });
    });
    await openApp(page, "/agent/kubernetes?view=overview");
    await expect(page.getByText("Metrics available", { exact: true })).toBeVisible();
    expect(usageCalls).toBe(1);
    await expect(page.getByText("source pod metrics", { exact: true })).toBeVisible();
    await expect(page.getByText("Aggregate 750 mCPU CPU, 2 GiB memory", { exact: true })).toBeVisible();
    await expect(page.getByText("Usage fresh; 1300 pod metrics; 5 node metrics; sample list partial", { exact: true })).toBeVisible();
    const layout = await page.getByText(/^Sampled /).evaluate((sampled) => {
      const strip = sampled.parentElement!;
      const metrics = strip.firstElementChild!;
      const workloads = document.querySelector<HTMLElement>('section[aria-label="Workloads"]')!;
      const content = document.querySelector<HTMLElement>("#kubernetes-view-panel")!;
      const sampledBox = sampled.getBoundingClientRect();
      const metricsBox = metrics.getBoundingClientRect();
      const workloadBox = workloads.getBoundingClientRect();
      const contentBox = content.getBoundingClientRect();
      const firstRowItem = [...strip.children].find((item) => Math.abs(item.getBoundingClientRect().top - sampledBox.top) < 2)!;
      return { margin: getComputedStyle(sampled).marginLeft, justify: getComputedStyle(strip).justifyContent, metricsLeft: metricsBox.left, sampledLeft: sampledBox.left, sampledRight: sampledBox.right, rowRight: firstRowItem === sampled ? firstRowItem.getBoundingClientRect().left : sampled.previousElementSibling!.getBoundingClientRect().right + 16, stripLeft: strip.getBoundingClientRect().left, workloadLeft: workloadBox.left, workloadRight: workloadBox.right, contentLeft: contentBox.left, contentRight: contentBox.right };
    });
    expect(layout.margin).toBe("0px");
    expect(layout.justify).not.toBe("space-between");
    expect(layout.sampledLeft).toBeGreaterThanOrEqual(layout.metricsLeft);
    expect(Math.abs(layout.sampledLeft - layout.rowRight)).toBeLessThanOrEqual(1);
    expect(layout.metricsLeft - layout.stripLeft).toBeLessThanOrEqual(17);
    expect(Math.abs(layout.workloadLeft - layout.contentLeft)).toBeLessThanOrEqual(1);
    expect(Math.abs(layout.workloadRight - layout.contentRight)).toBeLessThanOrEqual(1);
    for (const [heading, value] of [["CPU capacity", "750 mCPU"], ["Memory capacity", "2 GiB"]]) {
      const section = page.getByRole("heading", { name: heading, exact: true }).locator("xpath=ancestor::section[1]");
      await expect(section.getByText("Usage", { exact: true }).locator("..")).toContainText(value);
    }
    await expect(page.locator("#kubernetes-view-panel")).not.toContainText(/99 cores|999 GiB|Aggregate 1 core/);
    fs.mkdirSync(screenshotDir, { recursive: true });
    await page.screenshot({ path: path.join(screenshotDir, "delta-usage-complete-desktop.png"), fullPage: true, animations: "disabled" });
    freshNodes = true;
    await page.getByRole("button", { name: "Refresh Kubernetes data", exact: true }).click();
    await expect(page.getByText("source node metrics", { exact: true })).toBeVisible();
    expect(usageCalls).toBe(2);
    await expect(page.getByText("Metrics available", { exact: true })).toBeVisible();
    await expect(page.getByText("Aggregate 500 mCPU CPU, 512 MiB memory", { exact: true })).toBeVisible();
    await expect(page.getByText("Usage fresh; 1300 pod metrics; 5 node metrics; sample list partial", { exact: true })).toBeVisible();
    for (const [heading, value] of [["CPU capacity", "500 mCPU"], ["Memory capacity", "512 MiB"]]) {
      const section = page.getByRole("heading", { name: heading, exact: true }).locator("xpath=ancestor::section[1]");
      await expect(section.getByText("Usage", { exact: true }).locator("..")).toContainText(value);
    }
    await page.screenshot({ path: path.join(screenshotDir, "delta-usage-fresh-nodes-desktop.png"), fullPage: true, animations: "disabled" });
  });

  for (const completeness of ["false", "missing"] as const) {
    test(`rejects ${completeness} completeness in fresh usage aggregates`, async ({ page }) => {
      let usageCalls = 0;
      await page.route((url) => url.pathname === "/api/admin/kubernetes/usage", (route) => {
        usageCalls++;
        expect(new URL(route.request().url()).searchParams.has("namespace")).toBe(true);
        return route.fulfill({ json: {
        observed_at: "2026-08-30T12:00:00Z", availability: "available", fresh: true, truncated: true,
        node_metrics: { availability: "available", fresh: true, ...(completeness === "false" ? { complete: false } : {}), total: 8, cpu: "500m", memory: "512Mi" },
        pod_metrics: { availability: "available", fresh: true, ...(completeness === "false" ? { complete: false } : {}), total: 97, cpu: "2", memory: "4Gi" },
        } });
      });
      await openApp(page, "/agent/kubernetes?view=overview");
      await expect(page.getByText("Aggregate Unavailable CPU, Unavailable memory", { exact: true })).toBeVisible();
      expect(usageCalls).toBe(1);
      await expect(page.getByText(/metrics source incomplete; aggregate unavailable/)).toBeVisible();
      await expect(page.getByText(/Unavailable pod metrics; Unavailable node metrics/)).toBeVisible();
      await expect(page.getByRole("meter", { name: /^(CPU|Memory) in use$/ })).toHaveCount(0);
      fs.mkdirSync(screenshotDir, { recursive: true });
      await page.screenshot({ path: path.join(screenshotDir, `delta-usage-incomplete-${completeness}-desktop.png`), fullPage: true, animations: "disabled" });
    });
  }

  test("validates actual complete graph parameters, namespace isolation and whole counts", async ({ page }) => {
    await openApp(page, "/agent/kubernetes?view=topology");
    const results = await page.evaluate(async () => {
      const base = "/api/admin/kubernetes/graph?complete=true";
      const invalid = ["&namespace=payments&limit=bad", "&namespace=payments&max_nodes=bad", "&namespace=payments&limit=", "&namespace=payments&cursor=unexpected", ""];
      const rejected = await Promise.all(invalid.map(async (query) => {
        const response = await fetch(base + query);
        return { query, status: response.status, body: await response.text() };
      }));
      const graphs = await Promise.all(["payments", "inventory-only"].map(async (namespace) => {
        const response = await fetch(`${base}&namespace=${namespace}`);
        return { namespace, status: response.status, data: await response.json() as KubernetesGraph };
      }));
      const connected = await fetch(`${base}&namespace=payments&connected_only=true`);
      const overviewResponse = await fetch("/api/admin/kubernetes/graph/overview");
      const overview = { status: overviewResponse.status, data: await overviewResponse.json() as KubernetesGraph };
      const overviewRejected = await Promise.all(["namespace=payments", "complete=true", "connected_only=true", "limit=20", "cursor=unexpected", "max_nodes=20"].map(async (query) => {
        const response = await fetch(`/api/admin/kubernetes/graph/overview?${query}`);
        return { query, status: response.status };
      }));
      return { rejected, graphs, connected: { status: connected.status, data: await connected.json() as KubernetesGraph }, overview, overviewRejected };
    });
    expect(results.overview.status).toBe(200);
    expect(results.overview.data.sync.partial).toBe(false);
    expect(results.overview.data.truncated ?? false).toBe(false);
    expect(results.overview.data.next ?? "").toBe("");
    expect(results.overview.data.partial_failures ?? []).toEqual([]);
    expect(Object.values(results.overview.data.omitted).every((count) => count === 0)).toBe(true);
    const overviewConnected = new Set(results.overview.data.edges.flatMap((edge) => [edge.from, edge.to]));
    expect(results.overview.data.nodes.every((node) => overviewConnected.has(node.id))).toBe(true);
    expect(results.connected.data.nodes.every((node) => results.overview.data.nodes.some((whole) => whole.id === node.id))).toBe(true);
    for (const result of results.overviewRejected) expect(result.status, result.query).toBe(400);
    for (const result of results.rejected) {
      expect(result.status, `${result.query || "missing namespace"}: ${result.body}`).toBe(400);
    }
    for (const graph of [...results.graphs, { namespace: "payments", ...results.connected }]) {
      expect(graph.status).toBe(200);
      expect(graph.data.sync.partial).toBe(false);
      expect(graph.data.truncated ?? false).toBe(false);
      expect(graph.data.next ?? "").toBe("");
      expect(graph.data.partial_failures ?? []).toEqual([]);
      expect(Object.values(graph.data.omitted).every((count) => count === 0)).toBe(true);
      expect(graph.data.nodes.every((node) => !node.namespace || node.namespace === graph.namespace)).toBe(true);
      expect(new Set(graph.data.nodes.map((node) => node.id)).size).toBe(graph.data.nodes.length);
      const ids = new Set(graph.data.nodes.map((node) => node.id));
      expect(graph.data.edges.every((edge) => ids.has(edge.from) && ids.has(edge.to))).toBe(true);
    }
    const whole = results.graphs[0].data;
    expect(whole.nodes.length).toBeGreaterThan(0);
    expect(whole.edges.length).toBeGreaterThan(0);
    const connectedIds = new Set(whole.edges.flatMap((edge) => [edge.from, edge.to]));
    expect(results.connected.data.nodes.map((node) => node.id).sort()).toEqual(whole.nodes.filter((node) => connectedIds.has(node.id)).map((node) => node.id).sort());
    expect(results.connected.data.edges).toEqual(whole.edges);
    const panel = page.getByRole("region", { name: "Kubernetes topology", exact: true });
    await panel.getByRole("button", { name: "Open namespace payments topology", exact: true }).click();
    await expectDefaultFilters(panel, results.connected.data);
    await panel.getByRole("button", { name: "Show all", exact: true }).click();
    await expect(panel.getByText(`${connectedIds.size} / ${connectedIds.size} connected resources visible`, { exact: false })).toContainText(`${whole.edges.length} / ${whole.edges.length} relationships`);
    await expectNoTopologyPagination(panel);
  });

  test("renders populated object-scoped drawer events", async ({ page }) => {
    await page.route("**/api/admin/kubernetes/resources/core~v1~pods/checkout-api-0/describe?*", async (route) => {
      const url = new URL(route.request().url());
      expect(url.searchParams.get("namespace")).toBe("payments");
      const response = await route.fetch();
      await route.fulfill({ response, json: { ...await response.json(), events: [{ resource_id: "core~v1~events", kind: "Event", namespace: "payments", name: "checkout-backoff", uid: "synthetic-event", summary: { reason: "BackOff", message: "Synthetic checkout container restart delayed", count: 9, lastTimestamp: "2026-10-06T12:00:00Z" } }] } });
    });
    await openApp(page, "/agent/kubernetes?view=resources");
    await page.getByRole("button", { name: "Select Pod payments/checkout-api-0", exact: true }).click();
    const detail = page.getByRole("dialog", { name: "Details panel", exact: true });
    await detail.getByRole("tab", { name: "Events", exact: true }).click();
    const events = detail.getByRole("region", { name: "Object-scoped events", exact: true });
    await expect(events.getByRole("heading", { name: "BackOff", exact: true })).toBeVisible();
    await expect(events).toContainText("Synthetic checkout container restart delayed");
    await expect(events).toContainText("9");
    await expect(events).toContainText("2026-10-06T12:00:00Z");
    await expect(detail.getByText("No object-scoped events were returned.", { exact: true })).toHaveCount(0);
    fs.mkdirSync(screenshotDir, { recursive: true });
    await page.screenshot({ path: path.join(screenshotDir, "delta-drawer-populated-desktop.png"), fullPage: true, animations: "disabled" });
  });

  test("hides current cached topology after a 401 refetch", async ({ page }) => {
    let deny = false;
    await page.route("**/api/admin/kubernetes/graph?*", async (route) => {
      if (!new URL(route.request().url()).searchParams.has("complete") || !deny) return route.continue();
      await route.fulfill({ status: 401, json: { error: "Synthetic session expired", code: "unauthorized" } });
    });
    await openApp(page, "/agent/kubernetes?view=topology");
    const panel = page.getByRole("region", { name: "Kubernetes topology", exact: true });
    await panel.getByRole("button", { name: "Open namespace payments topology", exact: true }).click();
    await expect(panel.locator("button[data-node-id]").first()).toBeVisible();
    deny = true;
    await panel.getByRole("button", { name: "Refresh topology", exact: true }).click();
    await expect(panel.getByRole("alert")).toContainText("Check sign-in, license, and Kubernetes graph permissions");
    await expect(panel.locator("button[data-node-id]")).toHaveCount(0);
    await expect(panel.getByLabel("Scrollable topology graph", { exact: true })).toHaveCount(0);
    await expect(panel.getByRole("button", { name: "Retry topology", exact: true })).toHaveCount(0);
    await expectNoTopologyPagination(panel);
    fs.mkdirSync(screenshotDir, { recursive: true });
    await page.screenshot({ path: path.join(screenshotDir, "delta-topology-cached-401-desktop.png"), fullPage: true, animations: "disabled" });
  });

  for (const viewport of [{ name: "desktop", width: 1440, height: 1000 }, { name: "mobile", width: 390, height: 844 }]) {
    test(`keeps 1300 resource topology usable and DOM bounded ${viewport.name}`, async ({ page }) => {
      await page.setViewportSize({ width: viewport.width, height: viewport.height });
      const nodes = Array.from({ length: 1300 }, (_, index) => ({ id: `pod-${index}`, kind: index % 2 ? "Pod" : "Deployment", namespace: "payments", name: `synthetic-${index}`, group: "payments" }));
      const edges: KubernetesGraph["edges"] = nodes.slice(1).map((node, index) => ({ from: nodes[index].id, to: node.id, type: "manages" }));
      await page.route((url) => url.pathname === "/api/admin/kubernetes/graph" || url.pathname === "/api/admin/kubernetes/graph/overview", (route) => {
        const url = new URL(route.request().url());
        if (url.pathname.endsWith("/overview")) expect(url.search).toBe("");
        else if (!url.searchParams.has("complete")) return route.continue();
        return route.fulfill({ json: { nodes, edges, omitted: {}, sync: { state: "ready", age_s: 0, partial: false } } satisfies KubernetesGraph });
      });
      await openApp(page, "/agent/kubernetes?view=overview");
      const overview = page.getByRole("region", { name: "Overview topology", exact: true });
      await expectGraphCounts(overview, { nodes, edges } as KubernetesGraph);
      const overviewGraph = overview.getByLabel("Scrollable overview topology graph", { exact: true });
      expect(await overviewGraph.locator("button[data-node-id]").count()).toBeGreaterThan(0);
      expect(await overviewGraph.locator("button[data-node-id]").count()).toBeLessThan(200);
      await page.getByRole("tab", { name: "Topology", exact: true }).click();
      const panel = page.getByRole("region", { name: "Kubernetes topology", exact: true });
      await panel.getByRole("button", { name: "Open namespace payments topology", exact: true }).click();
      const graph = panel.getByLabel("Scrollable topology graph", { exact: true });
      await expect(graph).toBeVisible();
      await expect(panel.getByText("1300 / 1300 connected resources visible", { exact: false })).toContainText("1299 / 1299 relationships");
      await expectNoTopologyPagination(panel);
      const expectBoundedDOM = async () => {
        expect(await graph.locator("button[data-node-id]").count()).toBeGreaterThan(0);
        expect(await graph.locator("button[data-node-id]").count()).toBeLessThan(200);
        expect(await graph.locator("svg g[data-edge-from]").count()).toBeLessThan(400);
        await expect(graph.locator("svg g rect, svg g text")).toHaveCount(0);
      };
      await expectBoundedDOM();
      const geometry = await graph.locator("button[data-node-id]").first().evaluate((node) => ({ id: node.getAttribute("data-node-id"), left: (node as HTMLElement).style.left, top: (node as HTMLElement).style.top }));
      await panel.getByRole("button", { name: "Zoom in topology", exact: true }).click();
      expect(await graph.locator(`button[data-node-id="${geometry.id}"]`).evaluate((node) => ({ id: node.getAttribute("data-node-id"), left: (node as HTMLElement).style.left, top: (node as HTMLElement).style.top }))).toEqual(geometry);
      await graph.evaluate((element) => { element.scrollTop = element.scrollHeight / 2; });
      await expect.poll(() => graph.locator(`button[data-node-id="${geometry.id}"]`).count()).toBe(0);
      await expectBoundedDOM();
      const beforePan = await graph.evaluate((element) => element.scrollTop);
      await panel.getByRole("button", { name: "Pan topology down", exact: true }).click();
      await expect.poll(() => graph.evaluate((element) => element.scrollTop)).toBeGreaterThan(beforePan);
      await panel.getByRole("button", { name: "Hide Pod", exact: true }).click();
      await expect(panel.getByText("650 / 1300 connected resources visible", { exact: false })).toContainText("0 / 1299 relationships");
      await panel.getByRole("button", { name: "Show all", exact: true }).click();
      await expect(panel.getByText("1300 / 1300 connected resources visible", { exact: false })).toContainText("1299 / 1299 relationships");
      await panel.getByRole("button", { name: "Fit topology to view", exact: true }).click();
      await expect.poll(() => graph.evaluate((element) => element.scrollTop)).toBe(0);
      const restoredNode = graph.locator(`button[data-node-id="${geometry.id}"]`);
      await expect(restoredNode).toHaveCount(1);
      for (let step = 0; step < 10; step++) await panel.getByRole("button", { name: "Pan topology down", exact: true }).click();
      await expect(restoredNode).toHaveCount(0);
      await expectBoundedDOM();
      await panel.getByRole("button", { name: "Fit topology to view", exact: true }).click();
      await expect(restoredNode).toHaveCount(1);
      await expectBoundedDOM();
      expect(await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth)).toBe(false);
      fs.mkdirSync(screenshotDir, { recursive: true });
      await page.screenshot({ path: path.join(screenshotDir, `delta-topology-large-${viewport.name}.png`), fullPage: true, animations: "disabled" });
    });
  }
});

test.describe("Live Pod stream acceptance", () => {
  test.skip(process.env.E2E_KUBERNETES_BACKEND !== "fake", "requires the real harness fake Kubernetes backend");
  const logPath = "/api/v1/namespaces/payments/pods/checkout-api-0/log";
  const state = harnessRuntimeStateDir();
  const ca = path.join(state, "fakekube/ca.crt");
  const counters = () => streamCounters();
  const current = async (container = "api", previous = false) => (await counters()).find((counter) => counter.path === logPath && counter.container === container && counter.previous === previous && counter.follow === !previous);
  const active = async () => (await counters()).filter((counter) => counter.path === logPath).reduce((total, counter) => total + counter.active, 0);
  const control = (container: string, options: { disconnect_after_chunks?: number; remaining?: number; previous_available?: boolean } = {}) => new Promise<void>((resolve, reject) => {
    const body = JSON.stringify({ path: logPath, container, ...options });
    const request = https.request(`https://localhost:${Number(process.env.HARNESS_FAKEKUBE_PORT || 19443)}/_fake/log-streams`, { method: "POST", ca: fs.readFileSync(ca), timeout: 5000, headers: { "Content-Type": "application/json", "Content-Length": Buffer.byteLength(body) } }, (response) => {
      response.resume();
      response.on("end", () => response.statusCode === 200 ? resolve() : reject(new Error(`observer control status ${response.statusCode}`)));
      response.on("error", reject);
    });
    request.on("timeout", () => request.destroy(new Error("observer control timeout")));
    request.on("error", reject);
    request.end(body);
  });
  const rawSecrets = /(?:super|split|previous|synthetic-oversized)-secret-value|synthetic-record-padding/;

  test.afterEach(async ({ page }) => {
    await page.goto("about:blank");
    for (const container of ["api", "sidecar", "setup", "debug"]) await control(container);
    await expect.poll(active).toBe(0);
  });

  for (const viewport of [{ name: "desktop", width: 1440, height: 1000 }, { name: "mobile", width: 390, height: 844 }]) {
    test(`live arriving lines, checkpoints, exports, and upstream cancellation ${viewport.name}`, async ({ page, context }, testInfo) => {
      const stageBudgets = { streamAndExport: 120_000, lifecycle: 45_000, cleanup: 15_000 };
      test.setTimeout(stageBudgets.streamAndExport + stageBudgets.lifecycle + stageBudgets.cleanup);
      const caseStarted = Date.now();
      await page.setViewportSize(viewport);
      await context.grantPermissions(["clipboard-read", "clipboard-write"]);
      const requests: URL[] = [];
      page.on("request", (request) => { if (new URL(request.url()).pathname.endsWith("/logs/stream")) requests.push(new URL(request.url())); });
      await openApp(page, "/agent/kubernetes?r=core~v1~pods/payments/checkout-api-0&tab=logs");
      const detail = page.getByRole("dialog", { name: "Details panel" });
      const output = detail.getByLabel("Pod log output");
      const lines = output.locator("[data-log-key]");
      await expect(output).toContainText("REDACTED");
      await expect.poll(() => lines.count()).toBeGreaterThanOrEqual(1);
      expect(await lines.count()).toBeLessThan(32);
      const firstLinePause = await current();
      expect(firstLinePause?.active).toBe(1);
      if (viewport.name === "mobile") await detail.getByRole("button", { name: "More", exact: true }).click();
      await detail.getByLabel("Pause logs").click();
      await expect(detail.getByLabel("Log connection status")).toHaveText("paused");
      await expectStreamCancelled(firstLinePause!, () => current());
      await detail.getByLabel("Resume logs").click();
      await expect.poll(() => requests.at(-1)?.searchParams.has("cursor")).toBe(true);
      await expect(output).toContainText("fakekube follow line 30");
      await expect(output).toContainText("[oversized log line omitted]");
      await expect(output).not.toContainText(rawSecrets);
      const repeated = lines.filter({ hasText: "fakekube repeated identical line" });
      await expect(repeated).toHaveCount(2);
      const beforePause = await current();
      expect(beforePause?.active).toBe(1);
      expect(beforePause!.chunks).toBeGreaterThan(35);
      await detail.getByLabel("Pause logs").click();
      await expect(detail.getByLabel("Log connection status")).toHaveText("paused");
      await expectStreamCancelled(beforePause!, () => current());
      const pausedKeys = await lines.evaluateAll((elements) => elements.map((element) => element.getAttribute("data-log-key")));
      for (const enabled of [false, true, false]) {
        await detail.getByLabel("Timestamps").setChecked(enabled);
        const rendered = (await lines.allTextContents()).map((line) => line.replace(/\n$/, "")).join("\n");
        expect(rendered).not.toMatch(rawSecrets);
        expect(rendered).toContain("[oversized log line omitted]");
        expect(rendered).toContain("fakekube follow line 30");
        for (const line of rendered.split("\n")) expect(line.match(/2026-01-01T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z/g) ?? []).toHaveLength(enabled ? 1 : 0);
        await detail.getByLabel("Copy scrubbed logs").click();
        expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(rendered);
        const downloaded = page.waitForEvent("download");
        await detail.getByLabel("Download scrubbed logs").click();
        const file = await downloaded;
        expect(fs.readFileSync((await file.path())!, "utf8")).toBe(rendered);
      }
      await detail.getByLabel("Resume logs").click();
      await expect.poll(() => requests.at(-1)?.searchParams.has("cursor")).toBe(true);
      expect(requests.at(-1)!.searchParams.has("since_seconds")).toBe(false);
      expect(requests.at(-1)!.searchParams.has("tail_lines")).toBe(false);
      await expect.poll(() => lines.count()).toBeGreaterThan(pausedKeys.length);
      await expect(repeated).toHaveCount(2);
      expect((await lines.evaluateAll((elements) => elements.map((element) => element.getAttribute("data-log-key")))).slice(0, pausedKeys.length)).toEqual(pausedKeys);
      expect((await current())!.since_time).toBeTruthy();
      // Following is automatic: there is no control to toggle, and leaving the
      // newest line is what stops it. The way back is offered instead.
      await expect(detail.getByRole("button", { name: "Jump to latest", exact: true })).toHaveCount(0);
      await output.evaluate((element) => { element.scrollTop = 0; element.dispatchEvent(new Event("scroll")); });
      await expect(detail.getByRole("button", { name: "Jump to latest", exact: true })).toBeVisible();
      const heldKeys = await lines.count();
      await expect.poll(() => lines.count()).toBeGreaterThan(heldKeys + 3);
      expect(await output.evaluate((element) => element.scrollTop)).toBeLessThanOrEqual(1);
      await detail.getByLabel("Wrap lines").uncheck();
      await expect(output).toHaveClass(/whitespace-pre(?!-wrap)/);
      await detail.getByLabel("Wrap lines").check();
      await detail.getByLabel("Log filter").fill("fakekube follow line");
      await expect(output).not.toContainText("repeated identical");
      await detail.getByLabel("Log filter").fill(".");
      await expect(output).toHaveText("No log lines match this filter.");
      await detail.getByLabel("Log filter").fill("");
      await detail.getByRole("button", { name: "Jump to latest", exact: true }).click();
      await expect.poll(() => output.evaluate((element) => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeLessThanOrEqual(48);
      await output.evaluate((element) => { element.scrollTop = 0; element.dispatchEvent(new Event("scroll")); });
      const scrolledKeys = await lines.count();
      await expect.poll(() => lines.count()).toBeGreaterThan(scrolledKeys + 3);
      expect(await output.evaluate((element) => element.scrollTop)).toBeLessThanOrEqual(1);
      await detail.getByRole("button", { name: "Jump to latest", exact: true }).click();
      const beforeChange = (await current())!.cancelled;
      await detail.getByLabel("Log since seconds").selectOption("60");
      await expect.poll(() => requests.at(-1)?.searchParams.get("since_seconds")).toBe("60");
      await expect.poll(async () => (await current())!.cancelled).toBeGreaterThan(beforeChange);
      await detail.getByLabel("Log since seconds").selectOption("86401");
      await expect.poll(() => requests.at(-1)?.searchParams.get("tail_lines")).toBe("5000");
      const containers = await detail.getByLabel("Log container").locator("option").evaluateAll((options) => options.map((option) => (option as HTMLOptionElement).value));
      expect(containers).toEqual(["api", "sidecar", "setup", "debug"]);
      await expect(detail.getByLabel("Log container").locator("option")).toHaveText(["api", "sidecar", "init:setup", "debug (ephemeral)"]);
      await expect.poll(async () => (await current())?.active).toBe(1);
      const switched = (await current())!;
      const alternative = "sidecar";
      const sidecarRequests = (await current(alternative))?.requests ?? 0;
      await detail.getByLabel("Log container").selectOption(alternative);
      await expect.poll(() => requests.at(-1)?.searchParams.get("container")).toBe(alternative);
      await expectStreamCancelled(switched, () => current());
      await expect.poll(async () => (await current(alternative))?.requests ?? 0).toBeGreaterThan(sidecarRequests);
      await expect.poll(async () => (await current(alternative))?.active).toBe(1);
      await expect(output).toContainText("container=sidecar");
      await detail.getByLabel("Previous container logs").check();
      await expect(detail.getByLabel("Log connection status")).toHaveText("ended");
      await expect(output).toContainText("fakekube previous");
      await expect(output).not.toContainText(rawSecrets);
      await expect.poll(active).toBe(0);
      expect((await current(alternative, true))!.completed).toBeGreaterThan(0);
      await detail.getByLabel("Previous container logs").uncheck();
      await expect(output).toContainText("fakekube follow line 30");
      const beforeSidecarPause = (await current(alternative))!;
      await detail.getByLabel("Pause logs").click();
      await expectStreamCancelled(beforeSidecarPause, () => current(alternative));
      const beforeDisconnect = await current(alternative);
      const reconnectStart = requests.length;
      await control(alternative, { disconnect_after_chunks: 40, remaining: 1 });
      await detail.getByLabel("Resume logs").click();
      await expect.poll(async () => (await current(alternative))!.disconnected, { timeout: 20_000 }).toBeGreaterThan(beforeDisconnect!.disconnected);
      await expect.poll(async () => (await current(alternative))!.requests, { timeout: 20_000 }).toBeGreaterThan(beforeDisconnect!.requests + 1);
      const reconnectRequests = requests.slice(reconnectStart);
      expect(reconnectRequests.length).toBeGreaterThanOrEqual(2);
      expect(reconnectRequests.at(-1)!.searchParams.has("cursor")).toBe(true);
      expect(reconnectRequests.at(-1)!.searchParams.has("since_seconds")).toBe(false);
      expect(reconnectRequests.at(-1)!.searchParams.has("tail_lines")).toBe(false);
      expect((await current(alternative))!.since_time).toBeTruthy();
      await expect(output).toContainText("fakekube follow line 64", { timeout: 25_000 });
      await expect.poll(async () => (await current(alternative))!.active).toBe(1);
      expect(await lines.count()).toBeLessThanOrEqual(10_000);
      expect(Buffer.byteLength(await output.innerText())).toBeLessThan(4 * 1024 * 1024);
      await expect(output).not.toContainText(rawSecrets);
      for (const container of ["setup", "debug"]) {
        await detail.getByLabel("Log container").selectOption(container);
        await expect(output).toContainText(`container=${container}`);
        const unavailable = (await current(container, true))?.unavailable ?? 0;
        const previousRequestStart = requests.length;
        await detail.getByLabel("Previous container logs").check();
        await expect(detail.getByLabel("Log connection status")).toHaveText("error");
        await expect(output).toContainText("previous_unavailable Previous container logs are unavailable. Select current container logs.");
        await expect(output).not.toContainText("resumed after reconnect at");
        const previousRequests = requests.slice(previousRequestStart);
        expect(previousRequests).toHaveLength(1);
        expect(previousRequests[0].searchParams.get("container")).toBe(container);
        expect(previousRequests[0].searchParams.get("previous")).toBe("true");
        await expect.poll(async () => (await current(container, true))?.unavailable ?? 0).toBeGreaterThan(unavailable);
        expect(await current(container, true)).toMatchObject({ previous: true, follow: false });
        await expect.poll(active).toBe(0);
        await detail.getByLabel("Previous container logs").uncheck();
        await expect(output).toContainText(`container=${container}`);
      }
      await detail.getByLabel("Log container").selectOption("api");
      await expect(output).toContainText("fakekube follow line");
      const apiPrevious = (await current("api", true))?.completed ?? 0;
      await detail.getByLabel("Previous container logs").check();
      await expect(detail.getByLabel("Log connection status")).toHaveText("ended");
      await expect(output).toContainText("fakekube previous");
      await expect(output).not.toContainText(rawSecrets);
      await expect.poll(async () => (await current("api", true))?.completed ?? 0).toBeGreaterThan(apiPrevious);
      await expect.poll(active).toBe(0);
      await detail.getByLabel("Previous container logs").uncheck();
      await expect(output).toContainText("[oversized log line omitted]");
      await expect(output).toContainText("fakekube follow line 30");
      const beforeExportPause = (await current())!;
      await detail.getByLabel("Pause logs").click();
      await expectStreamCancelled(beforeExportPause, () => current());
      const captured = (await lines.allTextContents()).map((line) => line.replace(/\n$/, "")).join("\n");
      expect(captured).not.toMatch(rawSecrets);
      await detail.getByLabel("Copy scrubbed logs").click();
      expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(captured);
      const capturedDownload = page.waitForEvent("download");
      await detail.getByLabel("Download scrubbed logs").click();
      expect(fs.readFileSync((await (await capturedDownload).path())!, "utf8")).toBe(captured);
      expect(await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth)).toBe(false);
      await expect(detail.getByLabel("Log connection status")).toHaveText("paused");
      await expect(detail.getByRole("alert")).toHaveCount(0);
      fs.mkdirSync(screenshotDir, { recursive: true });
      await page.screenshot({ path: path.join(screenshotDir, `live-pod-stream-${viewport.name}.png`), fullPage: true, animations: "disabled" });
      expect(Date.now() - caseStarted, "stream and export stage must preserve lifecycle and cleanup budgets").toBeLessThanOrEqual(stageBudgets.streamAndExport);
      await detail.getByLabel("Resume logs").click();
      await expect.poll(active).toBe(1);
      const beforeClose = (await current())!;
      await detail.getByRole("button", { name: "Close panel" }).click();
      await expectStreamCancelled(beforeClose, () => current());
      await openApp(page, "/agent/kubernetes?r=core~v1~pods/payments/checkout-api-0&tab=logs");
      await expect.poll(active).toBe(1);
      const beforeUnmount = (await current())!;
      await page.goto("/now");
      await expectStreamCancelled(beforeUnmount, () => current());
      expect(Date.now() - caseStarted, "lifecycle checks must preserve the cleanup budget").toBeLessThanOrEqual(stageBudgets.streamAndExport + stageBudgets.lifecycle);
      await testInfo.attach("upstream-counters", { body: JSON.stringify(await counters(), null, 2), contentType: "application/json" });
    });

    test(`live drawer URL keyboard and aligned topology ${viewport.name}`, async ({ page }) => {
      await page.setViewportSize(viewport);
      await openApp(page, "/agent/kubernetes?r=apps~v1~deployments/payments/checkout-api&tab=actions");
      const detail = page.getByRole("dialog", { name: "Details panel" });
      const tabs = detail.getByRole("tablist", { name: "Resource detail views" });
      await expect(tabs.getByRole("tab", { name: "Overview", exact: true })).toHaveAttribute("aria-selected", "true");
      await expect(tabs.getByRole("tab", { name: /^(Actions|Metrics|Diagnosis)$/ })).toHaveCount(0);
      await expect(tabs.getByRole("tab")).toHaveText(["Overview", "Logs", "Events", "YAML", "Related", "Timeline"]);
      await expect(page).not.toHaveURL(/tab=actions/);
      await tabs.getByRole("tab", { name: "Events", exact: true }).focus();
      await page.keyboard.press("ArrowRight");
      await expect(tabs.getByRole("tab", { name: "YAML", exact: true })).toBeFocused();
      await page.evaluate(() => { window.history.pushState(null, "", "/agent/kubernetes?r=apps~v1~deployments/payments/checkout-api&tab=metrics"); window.dispatchEvent(new PopStateEvent("popstate")); });
      await expect(tabs.getByRole("tab", { name: "Overview", exact: true })).toHaveAttribute("aria-selected", "true");
      await expect(page).not.toHaveURL(/tab=metrics/);
      await page.evaluate(() => { window.history.pushState(null, "", "/agent/kubernetes?r=apps~v1~deployments/payments/checkout-api&tab=logs"); window.dispatchEvent(new PopStateEvent("popstate")); });
      await expect(tabs.getByRole("tab", { name: "Logs", exact: true })).toHaveAttribute("aria-selected", "true");
      await expect(page).toHaveURL(/tab=logs/);
      await page.keyboard.press("Escape");
      await expect(detail).toHaveCount(0);
      await page.getByRole("tab", { name: "Topology", exact: true }).click();
      await page.getByRole("button", { name: "Open namespace payments topology" }).click();
      const controls = page.getByRole("group", { name: "Topology namespace controls" });
      const boxes = await controls.locator("button,select").evaluateAll((elements) => elements.map((element) => { const box = element.getBoundingClientRect(); return { top: box.top, bottom: box.bottom }; }));
      expect(boxes).toHaveLength(3);
      expect(Math.max(...boxes.map((box) => box.top))).toBeLessThan(Math.min(...boxes.map((box) => box.bottom)));
      const refreshed = page.waitForRequest((request) => new URL(request.url()).pathname === "/api/admin/kubernetes/graph");
      await controls.getByLabel("Refresh namespace topology").click();
      expect(new URL((await refreshed).url()).searchParams.get("namespace")).toBe("payments");
      await expect(page.getByLabel("Scrollable topology graph", { exact: true })).toBeVisible();
      expect(await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth)).toBe(false);
      fs.mkdirSync(screenshotDir, { recursive: true });
      await page.screenshot({ path: path.join(screenshotDir, `live-topology-control-row-${viewport.name}.png`), fullPage: true, animations: "disabled" });
    });
  }

  test("live unauthenticated stream denial never reaches upstream", async ({ request }) => {
    const before = await counters();
    const response = await request.get("/api/admin/kubernetes/pods/payments/checkout-api-0/logs/stream?container=api");
    expect(response.status()).toBe(401);
    expect(await counters()).toEqual(before);
  });
});

test.describe("Pod log streaming fixtures", () => {
  test.use({ deviceScaleFactor: Number(process.env.E2E_KUBERNETES_CAPTURE_SCALE || "1") });

  test("full Logs workload filter fills the left edge while Pod levels and mobile actions remain intact", async ({ page }, testInfo) => {
    await page.emulateMedia({ colorScheme: "dark" });
    await page.addInitScript(() => localStorage.setItem("versus.theme", "dark"));
    await page.route("**/api/admin/kubernetes/clusters", (route) => route.fulfill({ json: { multiple: false, clusters: [] } }));
    const measurements = [];
    for (const width of [1440, 1024, 390]) {
      await page.setViewportSize({ width, height: 900 });
      for (const [kind, resourceID] of [["Deployment", "apps~v1~deployments"], ["StatefulSet", "apps~v1~statefulsets"], ["Pod", "core~v1~pods"]] as const) {
        await page.route("**/describe**", (route) => route.fulfill({ json: { resource: { kind, resource_id: resourceID, namespace: "payments", name: "filter-fixture", uid: "filter-fixture", summary: { log_containers: [{ name: "app", type: "regular" }] } }, events: [] } }));
        await openApp(page, `/agent/kubernetes?r=${resourceID}/payments/filter-fixture&tab=logs`);
        const viewer = page.getByRole("dialog", { name: "Details panel" }).locator('.resource-log-viewer[data-compact="false"]');
        await expect(viewer.getByLabel("Log filter")).toBeVisible();
        await expect(viewer.getByLabel("Pause logs")).toBeVisible();
        const geometry = await viewer.evaluate((element) => {
          const box = (selector: string) => {
            const rect = element.querySelector(selector)!.getBoundingClientRect();
            return { left: rect.left, right: rect.right, top: rect.top, bottom: rect.bottom, width: rect.width };
          };
          return { row: box(".log-secondary"), filter: box(".log-filter-wrap"), actions: box(".log-actions"), levels: element.querySelector(".log-levels") ? box(".log-levels") : null, overflow: document.documentElement.scrollWidth > window.innerWidth };
        });
        measurements.push({ width, kind, ...geometry });
        expect(geometry.overflow).toBe(false);
        expect(geometry.actions.right).toBeLessThanOrEqual(geometry.row.right + 1);
        if (kind !== "Pod" || width < 640) expect(Math.abs(geometry.filter.left - geometry.row.left)).toBeLessThanOrEqual(1);
        if (width < 640) {
          expect(Math.abs(geometry.filter.right - geometry.row.right)).toBeLessThanOrEqual(1);
          expect(geometry.actions.top).toBeGreaterThanOrEqual(geometry.filter.bottom);
        } else {
          expect(geometry.actions.left - geometry.filter.right).toBeCloseTo(8, 0);
          expect(Math.abs(geometry.actions.top - geometry.filter.top)).toBeLessThan(8);
          if (kind !== "Pod") expect(geometry.filter.width).toBeGreaterThan(352);
        }
        if (kind === "Pod") {
          expect(geometry.levels).not.toBeNull();
          expect(Math.abs(geometry.levels!.left - geometry.row.left)).toBeLessThanOrEqual(1);
          if (width >= 640) expect(geometry.filter.left).toBeGreaterThanOrEqual(geometry.levels!.right);
        } else expect(geometry.levels).toBeNull();
        await viewer.screenshot({ path: capturePath("log-filter", width, "dark", kind) });
      }
    }
    await testInfo.attach("log-filter-geometry", { body: JSON.stringify(measurements, null, 2), contentType: "application/json" });
  });

  for (const width of [390, 320]) for (const theme of ["dark", "light"] as const) {
    test(`Break timeline log caption bounds ${width} ${theme} theme`, async ({ page }, testInfo) => {
      await page.setViewportSize({ width, height: 844 });
      await openApp(page, `/agent/kubernetes?view=timeline&root_uid=checkout-uid&namespace=shop&center=${new Date(Date.now() - 5 * 60_000).toISOString()}`);
      const chart = page.getByRole("region", { name: "Resource timeline chart", exact: true });
      await expect(page.getByRole("region", { name: "Logs at break", exact: true }).getByRole("log")).toContainText("payment client timeout");
      await expect(chart.locator(".break-log-window-caption")).toBeVisible();
      const start = Date.parse((await chart.getAttribute("data-since"))!);
      const end = Date.parse((await chart.getAttribute("data-until"))!);
      const measurements = [];
      for (const edge of ["left", "right"] as const) for (const duration of [240_000, 30_000]) {
        const from = edge === "left" ? start + 30_000 : end - duration - 30_000;
        const url = new URL(page.url());
        url.searchParams.set("logs_from", new Date(from).toISOString());
        url.searchParams.set("logs_to", new Date(from + duration).toISOString());
        await page.goto(url.href);
        await expect(page.getByRole("region", { name: "Logs at break", exact: true }).getByRole("log")).toContainText("payment client timeout");
        await expect(chart).toHaveAttribute("data-since", new Date(start).toISOString());
        const caption = chart.locator(".break-log-window-caption");
        await expect(caption).toBeVisible();
        await chart.evaluate((element, edge) => { element.scrollLeft = edge === "left" ? 0 : element.scrollWidth - element.clientWidth; }, edge);
        await expect.poll(async () => caption.evaluate((element) => {
          const rect = element.getBoundingClientRect();
          const scroll = element.closest(".resource-timeline-scroll")!.getBoundingClientRect();
          return rect.left >= scroll.left + 132 - 1 && rect.right <= scroll.right + 1;
        })).toBe(true);
        const bounds = await caption.evaluate((element) => {
          const rect = element.getBoundingClientRect();
          const plot = element.closest(".break-log-bracket-plot")!.getBoundingClientRect();
          const scroll = element.closest(".resource-timeline-scroll")!.getBoundingClientRect();
          const range = document.createRange(); range.selectNodeContents(element);
          const text = Array.from(range.getClientRects()).map((line) => ({ left: line.left, right: line.right, top: line.top, bottom: line.bottom }));
          return {
            caption: { left: rect.left, right: rect.right, top: rect.top, bottom: rect.bottom },
            plot: { left: plot.left, right: plot.right }, visible: { left: scroll.left + 132, right: scroll.right },
            text, captionOverflow: element.scrollWidth > element.clientWidth,
            pageOverflow: document.documentElement.scrollWidth > window.innerWidth,
            controlsOverlap: Array.from(document.querySelectorAll(".resource-timeline-break-pill, .resource-timeline-now-label, .break-log-handle")).some((control) => {
              const other = control.getBoundingClientRect();
              return rect.left < other.right && rect.right > other.left && rect.top < other.bottom && rect.bottom > other.top;
            }),
          };
        });
        expect(bounds.caption.left).toBeGreaterThanOrEqual(bounds.plot.left - 1);
        expect(bounds.caption.right).toBeLessThanOrEqual(bounds.plot.right + 1);
        expect(bounds.captionOverflow).toBe(false);
        expect(bounds.pageOverflow).toBe(false);
        expect(bounds.controlsOverlap).toBe(false);
        expect(bounds.text.length).toBeGreaterThan(0);
        for (const line of bounds.text) {
          expect(line.left).toBeGreaterThanOrEqual(bounds.caption.left);
          expect(line.right).toBeLessThanOrEqual(bounds.caption.right);
          expect(line.top).toBeGreaterThanOrEqual(bounds.caption.top);
          expect(line.bottom).toBeLessThanOrEqual(bounds.caption.bottom);
        }
        const handle = chart.getByRole("slider", { name: "Log window end" });
        await expect(handle).toBeInViewport();
        await handle.focus();
        await expect(handle).toBeFocused();
        const previous = await handle.getAttribute("aria-valuenow");
        await page.keyboard.press("ArrowLeft");
        if (duration > 30_000) await expect(handle).not.toHaveAttribute("aria-valuenow", previous!);
        measurements.push({ edge, duration, ...bounds });
      }
      await testInfo.attach("caption-bounds", { body: JSON.stringify({ classification: "synthetic API fixture", width, theme, measurements }), contentType: "application/json" });
      await testInfo.attach("caption-mobile", { body: await page.screenshot(), contentType: "image/png" });
    });
  }

  test.beforeEach(async ({ page }, testInfo) => {
    const overviewRefresh = testInfo.title.startsWith("Overview refresh");
    const breakTimeline = testInfo.title.startsWith("Break timeline");
    const focusRegression = testInfo.title.includes("selected marker removal") || testInfo.title.includes("external controls retain focus");
    const lightTheme = testInfo.title.includes("light theme");
    const fullOverview = testInfo.title === "Break timeline full populated dark Overview documentation";
    const guideAlerts = { enabled: false, level: "critical", enabled_at: "", can_write: true, available: true, reason: "", links_ready: true, channels_enabled: true };
    const overviewCounts = { Namespace: 3, Node: 12, Ingress: 2, Gateway: 1, HTTPRoute: 3, Service: 9, Deployment: 3, ReplicaSet: 6, StatefulSet: 1, DaemonSet: 1, Job: 2, CronJob: 1, Pod: 28, HorizontalPodAutoscaler: 2, ConfigMap: 12, Secret: 8, PersistentVolumeClaim: 4, Event: 26 };
    if (breakTimeline) {
      await page.emulateMedia({ colorScheme: lightTheme ? "light" : "dark" });
      await page.addInitScript((theme) => localStorage.setItem("versus.theme", theme), lightTheme ? "light" : "dark");
    }
    const fullRootHistory = testInfo.title.startsWith("Break timeline full related history");
    const diagnostics = testInfo.title.includes("secondary diagnostics");
    const coldHistory = testInfo.title.includes("collapsed issue uncertainty");
    const denialEdge = testInfo.title.match(/final edge (initial|cached) denial (401|403|404)/);
    const syncEdge = testInfo.title.match(/final edge (overview|enrichment) SourceSync (stale|partial|unknown)/);
    let edgeRootReads = 0;
    const failures = diagnostics ? [{ resource_id: "ConfigMap", class: "forbidden" }, { resource_id: "ConfigMap", class: "forbidden" }] : [];
    const multiClusterBreak = breakTimeline && testInfo.title.includes("multi-cluster");
    const changeAt = new Date(Date.now() - 60_000).toISOString();
    const at = (minutes: number) => new Date((fullOverview ? Date.parse("2026-10-15T12:00:00Z") : Date.now()) - minutes * 60_000).toISOString();
    const roots: KubernetesIssue[] = [
      { root: { kind: "Deployment", namespace: "shop", name: "checkout", uid: "checkout-uid" }, rule: "pod.crash_loop", severity: "critical", count: 7, of: 7, broken: true, broke_at: at(5), history: "complete", changes: [
        { change: { id: "scale", cluster: "fixture", kind: "HorizontalPodAutoscaler", namespace: "shop", name: "checkout-hpa", uid: "hpa-uid", type: "replicas_changed", fields: [{ path: "replicas", from: "6", to: "7" }], at: at(7) }, relation: "scaled_by", lead_seconds: 120 },
        { change: { id: "config", cluster: "fixture", kind: "ConfigMap", namespace: "shop", name: "checkout-cfg", uid: "cfg-uid", type: "config_changed", at: at(8) }, relation: "used_by", lead_seconds: 180 },
        { change: { id: "image", cluster: "fixture", kind: "Deployment", namespace: "shop", name: "checkout", uid: "checkout-uid", type: "image_changed", fields: [{ path: "image", from: "v41", to: "v42" }], at: at(9) }, relation: "same_object", lead_seconds: 240 },
      ], evidence: { terminations: [{ reason: "OOMKilled", exit_code: 137, count: 6, last: at(2) }, { reason: "Error", exit_code: 1, count: 1, last: at(1) }], warning_events: 18, from: at(35), to: at(3) } },
      { root: { kind: "Deployment", namespace: "payments", name: "ledger-api", uid: "ledger-uid" }, rule: "workload.rollout_stalled", severity: "warning", count: 2, of: 4, broken: true, broke_at: at(4), history: "reconstructed", rebuilt: ["image"], gaps: [{ from: at(34), to: at(8) }], changes: [{ change: { id: "ledger-image", cluster: "fixture", kind: "Deployment", namespace: "payments", name: "ledger-api", uid: "ledger-uid", type: "image_changed", fields: [{ path: "image", from: "v17", to: "v18" }], source: "reconstructed", at: at(10) }, relation: "same_object", lead_seconds: 360 }], evidence: { waiting: [{ reason: "ImagePullBackOff", count: 2 }], warning_events: 8, from: at(34), to: at(2) } },
      { root: { kind: "DaemonSet", namespace: "kube-system", name: "node-agent", uid: "node-agent-uid" }, rule: "workload.unavailable", severity: "critical", count: 7, of: 12, broken: true, broke_at: at(3), history: "complete", untracked: ["Secret"], evidence: { warning_events: 0, from: at(33), to: at(1) } },
    ];
    const linkedHistory = Array.from({ length: 24 }, (_, index) => ({ id: `linked-${index}`, cluster: "fixture", kind: "ConfigMap", namespace: "shop", name: `checkout-settings-${index}`, uid: `config-${index}`, type: "config_changed" as const, at: at(7 + index / 10) }));
    if (process.env.E2E_KUBERNETES_AUTO_DETECT_DOCS === "1") roots[0].evidence = { ...roots[0].evidence!, restarts: 44, warning_reasons: [{ reason: "BackOff", count: 18 }] };
    if (testInfo.title.startsWith("Break timeline crash evidence")) {
      roots[0].evidence = { ...roots[0].evidence!, partial: true, restarts: 44,
        terminations: [{ reason: "Error", exit_code: 137, exit_hint: "SIGKILL", count: 2, restarts: 41, last: at(2) }, { reason: "Error", exit_code: 1, init: true, count: 1, last: at(1) }, { reason: "Error", exit_code: 137, signal: "SIGKILL", count: 1, last: at(1) }],
        waiting: [{ reason: "CreateContainerConfigError", count: 1, init: true }],
        pod_reasons: [{ reason: "Evicted", count: 4 }, { reason: "Unschedulable", count: 2 }],
        conditions: [{ type: "Failed", reason: "BackoffLimitExceeded" }],
        nodes: [{ reason: "MemoryPressure", count: 3 }], warning_events: 12,
        warning_reasons: [{ reason: "FailedScheduling", count: 8 }, { reason: "Other", count: 4 }],
      };
    }
    const k12aMatrix = testInfo.title.startsWith("Break timeline K12a crash evidence");
    const journalFixture = testInfo.title.startsWith("Break timeline captured journal fixture");
    let journalContinuationFailed = false;
    if (journalFixture) roots[0].evidence!.partial = true;
    if (k12aMatrix) {
      roots.splice(0, roots.length,
        { ...roots[0], root: { kind: "Deployment", namespace: "nodes", name: "nodes-api", uid: "checkout-uid" }, rule: "pod.crash_loop", severity: "critical", count: 2, of: 3, changes: [{ change: { id: "nodes-cfg", cluster: "fixture", kind: "ConfigMap", namespace: "nodes", name: "nodes-cfg", uid: "nodes-cfg-uid", type: "config_changed", at: at(7) }, relation: "used_by", lead_seconds: 120 }], evidence: {
          partial: true, restarts: 44, from: at(35), to: at(1),
          terminations: [
            { reason: "OOMKilled", exit_code: 137, signal: "SIGKILL", count: 2, restarts: 38, last: at(2) },
            { reason: "Error", exit_code: 137, exit_hint: "SIGKILL", count: 1, restarts: 3, last: at(2) },
            { reason: "Error", exit_code: 1, init: true, signal: "31", exit_hint: "SIGKILL", count: 1, restarts: 3, last: at(1) },
          ],
          waiting: [{ reason: "CrashLoopBackOff", count: 1 }, { reason: "CrashLoopBackOff", count: 1, init: true }],
          nodes: [{ reason: "MemoryPressure", count: 3 }],
          warning_events: 12, warning_reasons: [{ reason: "BackOff", count: 8 }, { reason: "Other", count: 4 }],
        } },
        { root: { kind: "Deployment", namespace: "config", name: "config-api", uid: "config-uid" }, rule: "pod.config_error", severity: "critical", count: 3, of: 3, broken: true, broke_at: at(4), history: "complete", changes: [], evidence: {
          waiting: [{ reason: "CreateContainerConfigError", count: 2 }, { reason: "PostStartHookError", count: 1, init: true }],
          warning_events: 4, warning_reasons: [{ reason: "Failed", count: 4 }], from: at(34), to: at(2),
        } },
        { root: { kind: "Deployment", namespace: "sched", name: "sched-api", uid: "sched-uid" }, rule: "pod.unschedulable", severity: "warning", count: 2, of: 2, broken: true, broke_at: at(4), history: "complete", changes: [], evidence: {
          pod_reasons: [{ reason: "Unschedulable", count: 2 }],
          warning_events: 8, warning_reasons: [{ reason: "FailedScheduling", count: 8 }], from: at(34), to: at(2),
        } },
        { root: { kind: "Deployment", namespace: "evict", name: "evict-api", uid: "evict-uid" }, rule: "pod.evicted", severity: "critical", count: 4, of: 4, broken: true, broke_at: at(3), history: "complete", changes: [], evidence: {
          restarts: 3,
          terminations: [
            { reason: "Error", exit_code: 126, exit_hint: "not_executable", count: 1, restarts: 2, last: at(2) },
            { reason: "Error", exit_code: 127, exit_hint: "not_found", count: 1, init: true, restarts: 1, last: at(1) },
          ],
          pod_reasons: [{ reason: "Evicted", count: 3 }, { reason: "OutOfResource", count: 1 }],
          warning_events: 5, warning_reasons: [{ reason: "Evicted", count: 4 }, { reason: "Other", count: 1 }], from: at(33), to: at(1),
        } },
        { root: { kind: "Job", namespace: "batch", name: "batch-job", uid: "batch-uid" }, rule: "job.failed", severity: "critical", count: 1, of: 1, broken: true, broke_at: at(3), history: "complete", changes: [], evidence: {
          restarts: 3,
          terminations: [
            { reason: "Error", exit_code: 134, exit_hint: "SIGABRT", count: 1, restarts: 1, last: at(3) },
            { reason: "Error", exit_code: 139, exit_hint: "SIGSEGV", count: 1, restarts: 1, last: at(2) },
            { reason: "Error", exit_code: 143, exit_hint: "SIGTERM", count: 1, restarts: 1, last: at(1) },
          ],
          conditions: [{ type: "Failed", reason: "BackoffLimitExceeded" }],
          warning_events: 4, warning_reasons: [{ reason: "BackoffLimitExceeded", count: 4 }], from: at(33), to: at(1),
        } },
      );
    }
    if (testInfo.title.includes("label and stack geometry")) {
      const image = roots[0].changes!.find((item) => item.change.id === "image")!;
      roots[0].changes!.push(...[1, 2].map((index) => ({ ...image, change: { ...image.change, id: `image-stack-${index}` } })), { ...image, change: { ...image.change, id: "rollback", at: at(-11), fields: [{ path: "image", from: "v42", to: "v41" }] } });
    }
    if (denialEdge) {
      roots[0].history = "reconstructed";
      roots[0].rebuilt = ["image"];
      roots[0].untracked = ["Secret"];
    }
    if (syncEdge) roots[0] = { ...roots[0], changes: [], history: "complete", evidence: undefined };
    if (fullRootHistory) {
      roots[0].changes = linkedHistory.slice(0, 3).map((change) => ({ change, relation: "used_by", lead_seconds: 120 }));
      roots[0].more_changes = 21;
    }
    const pod = { resource_id: "core~v1~pods", kind: "Pod", namespace: "payments", name: "checkout-api-0", uid: "pod-checkout-api-0", summary: { phase: "Running", log_containers: [{ name: "app", type: "regular" }, { name: "setup", type: "init" }, { name: "debug", type: "ephemeral" }] } };
    const deployment = { resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "payments", name: "checkout-api", uid: "deploy-checkout-api", summary: { ready_replicas: 1, desired_replicas: 1 } };
    await page.addInitScript((counts) => {
      const signals: Array<{ url: string; aborted: boolean }> = [];
      Object.defineProperty(window, "podStreamSignals", { value: signals });
      const original = window.fetch;
      window.fetch = (input, init) => {
        const url = String(input);
        if (counts && url.includes("/kubernetes/stream?")) {
          const sync = { state: "ready", age_s: 0, partial: false, kinds: Object.fromEntries(Object.entries(counts).map(([kind, records]) => [kind, { state: "ready", observed_at: "2026-10-15T12:00:00Z", records, partial: false }])) };
          return Promise.resolve(new Response(new ReadableStream({ start(controller) {
            controller.enqueue(new TextEncoder().encode(`event: sync\ndata: ${JSON.stringify(sync)}\n\n`));
            init?.signal?.addEventListener("abort", () => controller.close(), { once: true });
          } }), { headers: { "Content-Type": "text/event-stream" } }));
        }
        if (url.includes("/logs/stream")) {
          const entry = { url, aborted: false };
          signals.push(entry);
          init?.signal?.addEventListener("abort", () => { entry.aborted = true; }, { once: true });
        }
        return original(input, init);
      };
    }, fullOverview ? overviewCounts : null);
    await page.route("**/api/**", async (route) => {
      const url = new URL(route.request().url());
      const pathname = url.pathname;
      if (fullOverview) {
        const observedAt = "2026-10-15T12:00:00Z";
        const counts = overviewCounts;
        const sync = { state: "ready", age_s: 0, partial: false, kinds: Object.fromEntries(Object.entries(counts).map(([kind, records]) => [kind, { state: "ready", observed_at: observedAt, records, partial: false }])) };
        const pods = [
          { kind: "Pod" as const, namespace: "shop", name: "checkout-7f8c9-2kx4m", cpu: "1850m", memory: "1536Mi", request_cpu: "2", request_memory: "2Gi", window: "30s" },
          { kind: "Pod" as const, namespace: "payments", name: "ledger-api-6b7d9-p8n2q", cpu: "1240m", memory: "1024Mi", request_cpu: "1500m", request_memory: "2Gi", window: "30s" },
          { kind: "Pod" as const, namespace: "shop", name: "cart-58c9f-v6j3r", cpu: "820m", memory: "768Mi", request_cpu: "1", request_memory: "1Gi", window: "30s" },
          { kind: "Pod" as const, namespace: "kube-system", name: "node-agent-l4w7s", cpu: "430m", memory: "384Mi", request_cpu: "500m", request_memory: "512Mi", window: "30s" },
          { kind: "Pod" as const, namespace: "payments", name: "redis-0", cpu: "260m", memory: "512Mi", request_cpu: "500m", request_memory: "1Gi", window: "30s" },
        ];
        if (pathname === "/api/admin/kubernetes/overview") return route.fulfill({ json: { connector: "kubernetes", cluster_id: "fixture", observed_at: observedAt, nodes: 12, ready_nodes: 12, pods: 28, running_pods: 19, namespaces: 3, active_namespaces: 3, workloads: 9, warnings: 26, requested_cpu: "24", limited_cpu: "48", allocatable_cpu: "46", requested_memory: "64Gi", limited_memory: "128Gi", allocatable_memory: "180Gi", metrics_fresh: true, truncated: false } satisfies KubernetesOverview });
        if (pathname === "/api/admin/kubernetes/usage") return route.fulfill({ json: { observed_at: observedAt, availability: "available", fresh: true, truncated: false, pods, nodes: [], node_metrics: { availability: "available", fresh: true, complete: true, total: 12, cpu: "18400m", memory: "72Gi", observed_at: observedAt }, pod_metrics: { availability: "available", fresh: true, complete: true, total: 28, cpu: "14600m", memory: "48Gi", observed_at: observedAt } } satisfies KubernetesUsage });
        if (pathname === "/api/admin/kubernetes/top") return route.fulfill({ json: { items: pods, total: 28, availability: "available", fresh: true, truncated: false, sync } satisfies KubernetesTopPage });
        if (pathname === "/api/admin/kubernetes/releases") return route.fulfill({ json: { items: [{ namespace: "shop", name: "checkout", health: "degraded", current: { revision: 42, status: "failed" }, history: [] }, { namespace: "payments", name: "ledger-api", health: "degraded", current: { revision: 18, status: "pending-upgrade" }, history: [] }, { namespace: "kube-system", name: "observability", health: "healthy", current: { revision: 12, status: "deployed" }, history: [] }], truncated: false } });
        if (pathname === "/api/admin/kubernetes/traffic") return route.fulfill({ json: { available: true, source: "synthetic-enterprise-istio", window: "15m", observed_at: observedAt, unmapped: 0, truncated: false, edges: [
          { from: { namespace: "shop", kind: "Service", name: "gateway" }, to: { namespace: "shop", kind: "Service", name: "checkout" }, rate_per_sec: 428.6, error_rate: 0.034, p95_ms: 186, bytes_per_sec: 245760 },
          { from: { namespace: "shop", kind: "Service", name: "checkout" }, to: { namespace: "payments", kind: "Service", name: "ledger-api" }, rate_per_sec: 216.4, error_rate: 0.012, p95_ms: 84, bytes_per_sec: 131072 },
          { from: { namespace: "shop", kind: "Service", name: "cart" }, to: { namespace: "payments", kind: "Service", name: "redis" }, rate_per_sec: 128.8, error_rate: 0.002, p95_ms: 12, bytes_per_sec: 65536 },
        ] } satisfies KubernetesTraffic });
        if (pathname === "/api/admin/kubernetes/stream") return route.fulfill({ contentType: "text/event-stream", body: `event: sync\ndata: ${JSON.stringify(sync)}\n\nevent: heartbeat\ndata: {}\n\n` });
        if (pathname === "/api/admin/kubernetes/index/status") return route.fulfill({ json: sync });
      }
      if (pathname.endsWith("/logs/stream")) {
        if (pathname.includes("/workloads/")) return route.fulfill({ contentType: "text/event-stream", body: `event: sources\ndata: ${JSON.stringify({ workload_uid: "checkout-uid", pods: [{ uid: "pod-live", name: "checkout-7d9f8c-p9m1z", containers: ["api"], init_containers: [], main_container: "api", admitted: true }], truncated: false })}\n\nevent: line\ndata: ${JSON.stringify({ workload_uid: "checkout-uid", pod_uid: "pod-live", pod: "checkout-7d9f8c-p9m1z", namespace: "shop", container: "api", incarnation: "1", source_id: "src-live", text: "live workload snapshot line", timestamp: "2026-10-07T12:00:00Z", sequence: 1, ordinal: 1 })}\n\nevent: end\ndata: {"reason":"complete"}\n\n` });
        if (url.searchParams.get("previous") === "true") return route.fulfill({ contentType: "text/event-stream", body: 'event: error\ndata: {"code":"previous_unavailable","message":"Previous logs unavailable"}\n\nevent: end\ndata: {"reason":"error"}\n\n' });
        const prefix = url.searchParams.get("timestamps") === "false" ? "" : "2026-10-07T12:00:00Z ";
        const lines = Array.from({ length: 300 }, (_, index) => `event: line\ndata: ${JSON.stringify({ text: prefix + (index % 2 ? "repeated token=[REDACTED] a.b" : "repeated axb"), container: url.searchParams.get("container") || "app", timestamp: "2026-10-07T12:00:00Z", sequence: index + 1, ordinal: index + 1, ...((index + 1) % 32 === 0 ? { cursor: `cursor-${index + 1}` } : {}) })}\n\n`).join("");
        return route.fulfill({ contentType: "text/event-stream", body: `event: heartbeat\ndata: {}\n\n${lines}event: heartbeat\ndata: {"cursor":"cursor-300"}\n\nevent: end\ndata: {"reason":"complete","cursor":"cursor-300"}\n\n` });
      }
      if (pathname === "/api/admin/config/agent") return route.fulfill({ json: { enable: breakTimeline, mode: "shadow", sources: [], ai: { enable: breakTimeline }, catalog: {}, miner: {}, regex: { rules: [] }, redaction: {}, service_patterns: [] } });
      if (multiClusterBreak && pathname === "/api/admin/kubernetes/clusters") return route.fulfill({ json: { multiple: true, clusters: ["east", "west"].map((id) => ({ id, provider: "generic", health: "attention", nodes: 12, ready_nodes: 12, pods: 28, running_pods: 19, warnings: 26, broken: 3, ...(testInfo.title.includes("fleet documentation") ? { last_change_at: id === "east" ? at(9) : "invalid-timestamp" } : {}), sync: { state: "ready" }, observed_at: new Date().toISOString() })) } });
      if (focusRegression && pathname === "/api/admin/kubernetes/clusters") return route.fulfill({ json: { multiple: false, clusters: [] } });
      if (focusRegression && pathname === "/api/admin/kubernetes/namespaces") return route.fulfill({ json: { items: ["shop", "payments"], sync: { state: "ready", partial: false, age_s: 0 } } });
      if (breakTimeline && pathname === "/api/admin/kubernetes/overview") return route.fulfill({ json: { connector: "kubernetes", cluster_id: "fixture", observed_at: new Date().toISOString(), nodes: 12, ready_nodes: 12, pods: 28, running_pods: 19, namespaces: 3, active_namespaces: 3, workloads: 4, warnings: 26, truncated: diagnostics, omitted_categories: diagnostics ? ["ConfigMap"] : [], partial_failures: failures } });
      if (breakTimeline && pathname === "/api/admin/kubernetes/crashes") {
        const cursor = url.searchParams.get("cursor");
        if (journalFixture && cursor === "journal-50" && !journalContinuationFailed) {
          journalContinuationFailed = true;
          return route.fulfill({ status: 503, json: { error: "Synthetic continuation unavailable" } });
        }
        const base = (root: KubernetesIssue, index: number): KubernetesCrashEvent => ({ id: `${root.root.uid}-${index}`, sequence: index + 1, cluster: url.searchParams.get("cluster") || "fixture", namespace: root.root.namespace || "", pod: `${root.root.name}-removed-pod-${index}`, pod_uid: `removed-pod-uid-${index}`, root_uid: root.root.uid!, root_kind: root.root.kind, root_name: root.root.name, container: "api", type: "termination", termination_slot: "previous", exit_code: 137, reason: "Error", restart_count: 41, at: at(2), time_source: "finished_at", observed_at: at(1) });
        const events: KubernetesCrashEvent[] = journalFixture ? Array.from({ length: 105 }, (_, index) => ({ ...base(roots[0], index), ...(index % 5 === 0 ? { type: "status" as const, waiting_reason: "CrashLoopBackOff", time_source: "observed_at" as const } : {}), ...(index % 7 === 0 ? { init: true, container: "setup", signal: 9 } : {}) })) : roots.flatMap((root) => (root.evidence?.terminations ?? []).map((termination, index) => ({ ...base(root, index), reason: termination.reason, exit_code: termination.exit_code, signal: termination.signal === "SIGKILL" ? 9 : Number(termination.signal) || undefined, restart_count: termination.restarts ?? 0, init: termination.init, at: termination.last })));
        const filtered = events.filter((event) => (!url.searchParams.get("namespace") || event.namespace === url.searchParams.get("namespace")) && (!url.searchParams.get("root_uid") || event.root_uid === url.searchParams.get("root_uid")));
        const offset = Number(cursor?.replace("journal-", "") || 0);
        const limit = Number(url.searchParams.get("limit") || 50);
        return route.fulfill({ json: { items: filtered.slice(offset, offset + limit), ...(offset + limit < filtered.length ? { next: `journal-${offset + limit}` } : {}), since: url.searchParams.get("since")!, until: url.searchParams.get("until")!, retention_from: at(1440), retention_basis: "observed_at", started_at: at(30), gaps: journalFixture ? [{ from: at(50), to: at(45) }] : [], capture_gaps: journalFixture ? [{ from: at(20), to: at(19) }] : [], coverage: { Pod: { from: at(30), to: at(1) } }, limitations: ["polling_observations_only", "journal_not_started_for_full_window", "summary_caps_do_not_limit_journal"], capture: { mode: "continuous", behind: false, last_cycle_at: at(1) }, sync: { state: "ready", age_s: 4, partial: false } } satisfies KubernetesCrashPage });
      }
      if (breakTimeline && pathname === "/api/admin/kubernetes/break-logs") {
        const query = url.searchParams;
        const result: KubernetesBreakLogPage = {
          window: { from: query.get("since")!, to: query.get("until")! },
          pods: [{ name: "checkout-removed-pod-0", uid: "removed-pod-uid-0", short: "pod-0", state: "unknown" }, { name: "checkout-7d9f8c-p9m1z", uid: "pod-live", short: "p9m1z", state: "running" }],
          lines: Array.from({ length: 12 }, (_, index) => ({ at: at(6 - index / 10), pod: index % 2 ? "checkout-7d9f8c-p9m1z" : "checkout-removed-pod-0", pod_uid: index % 2 ? "pod-live" : "removed-pod-uid-0", container: "api", init: false, slot: index > 7 ? "current" : "previous", level: index > 3 && index < 8 ? "error" : "info", text: index > 3 && index < 8 ? "payment client timeout after 5 s (ledger)" : "starting worker pool size=64", source: "captured" })),
          markers: [{ type: "break", at: at(5), label: "Deployment checkout unavailable" }, { type: "crash", at: at(2), label: "api OOMKilled exit 137" }],
          counts: { all: 12, errors: 4, warnings: 0 }, rate: Array.from({ length: 7 }, (_, index) => ({ minute: at(10 - index), total: index + 3, errors: index > 3 ? index : 0 })),
          patterns: [{ template: "payment client timeout", level: "error", count: 4 }, { template: "starting worker pool size=<n>", level: "info", count: 8 }], sources: [{ kind: "captured", name: "fixture", at: at(4) }], coverage: { state: "partial", reason: "restarted_since", tip: true, gaps: [{ from: at(10), to: at(8) }] },
        };
        return route.fulfill({ json: result });
      }
      if (breakTimeline && pathname === "/api/admin/kubernetes/issues") {
        if (focusRegression && url.searchParams.get("context") === "persisted") return route.fulfill({ json: { items: [], totals: {}, truncated: false, sync: { state: "ready", partial: false, age_s: 0 }, root_context: { root: roots[0].root, occurrence: roots[0], retained: true }, selected_occurrence: roots[0], selection_status: "resolved" } });
        const rootLookup = url.searchParams.has("root_uid");
        if (denialEdge && rootLookup && (++edgeRootReads > 1 || denialEdge[1] === "initial")) return route.fulfill({ status: Number(denialEdge[2]), json: { error: "Synthetic root lookup denied" } });
        const cart: KubernetesIssue = { root: { kind: "Deployment", namespace: "shop", name: "cart", uid: "cart-uid" }, rule: "event.warning", severity: "warning", count: 1, first_seen: at(1), history: "gap" };
        let items = url.searchParams.has("root_uid") ? [...roots, cart].filter((issue) => issue.root.uid === url.searchParams.get("root_uid") && issue.root.namespace === url.searchParams.get("namespace")) : url.searchParams.get("broken") === "true" ? roots : [...roots, cart];
        if (url.searchParams.get("severity")) items = items.filter((issue) => issue.severity === url.searchParams.get("severity"));
        if (url.searchParams.get("namespace")) items = items.filter((issue) => issue.root.namespace === url.searchParams.get("namespace"));
        const paged = !url.searchParams.has("root_uid") && !url.searchParams.has("broken") && !url.searchParams.get("severity") && !url.searchParams.get("namespace");
        if (url.searchParams.get("cursor")) items = [cart];
        items = items.map((issue) => ({ ...issue, changes: issue.changes?.map((item) => ({ ...item, change: { ...item.change, cluster: url.searchParams.get("cluster") || "fixture" } })) }));
        if (coldHistory && !url.searchParams.has("root_uid") && !url.searchParams.has("broken")) items = items.map((issue) => issue.root.uid === "checkout-uid" ? { ...issue, history: undefined, changes: undefined, evidence: undefined } : issue);
        if (denialEdge && !rootLookup) items = items.map((issue) => issue.root.uid === "checkout-uid" ? { ...issue, history: undefined, rebuilt: undefined, changes: undefined, evidence: undefined } : issue);
        const adverseSync = syncEdge && (syncEdge[1] === "overview" ? !rootLookup : rootLookup);
        const sync = adverseSync ? { state: syncEdge[2] === "partial" ? "ready" : syncEdge[2], age_s: 4, partial: syncEdge[2] === "partial" } : { state: "ready", age_s: 4, partial: diagnostics };
        return route.fulfill({ json: { items, totals: k12aMatrix ? { critical: 4, warning: 2 } : { critical: 2, warning: 2 }, broken: k12aMatrix ? { total: 5, by_severity: { critical: 4, warning: 1 } } : { total: 3, by_severity: { critical: 2, warning: 1 } }, ...(paged && !url.searchParams.get("cursor") ? { next: "issues-next" } : {}), truncated: false, partial_failures: failures, sync } });
      }
      if (breakTimeline && pathname === "/api/admin/kubernetes/changes") {
        const cursor = url.searchParams.get("cursor");
        const scoped = Boolean(url.searchParams.get("namespace"));
        const unrelated = Array.from({ length: 20 }, (_, index) => ({ ...linkedHistory[0], id: `unrelated-${cursor}-${index}`, uid: `unrelated-${index}`, name: `unrelated-${index}`, at: at(1) }));
        const items = fullRootHistory && scoped ? !cursor || cursor === "unrelated" ? unrelated : cursor === "linked" ? linkedHistory.slice(0, 20) : linkedHistory.slice(20) : roots.flatMap((issue) => issue.changes?.map((item) => item.change) ?? []);
        const next = fullRootHistory && scoped ? !cursor ? "unrelated" : cursor === "unrelated" ? "linked" : cursor === "linked" ? "last" : undefined : undefined;
        return route.fulfill({ json: { items: items.map((change) => ({ ...change, cluster: url.searchParams.get("cluster") || "fixture" })), next, gaps: diagnostics ? [{ from: at(30), to: at(20) }] : [], partial_failures: failures, sync: { state: "ready", age_s: 4, partial: diagnostics } } });
      }
      if (breakTimeline && pathname === "/api/admin/kubernetes/graph") {
        const changes = fullRootHistory ? linkedHistory : roots[0].changes!.map((item) => item.change);
        const primary = roots[0].root;
        return route.fulfill({ json: { nodes: [{ id: primary.uid!, kind: primary.kind, namespace: primary.namespace, name: primary.name, group: primary.namespace ?? "shop" }, ...changes.filter((change) => change.uid !== primary.uid).map((change) => ({ id: change.uid, kind: change.kind, namespace: change.namespace, name: change.name, group: change.namespace ?? "shop" }))], edges: changes.filter((change) => change.uid !== primary.uid).map((change) => ({ from: change.uid, to: primary.uid!, type: change.kind === "HorizontalPodAutoscaler" ? "scales" : "configures" })), omitted: {}, sync: { state: "ready", age_s: 0, partial: false } } });
      }
      if (pathname === "/api/admin/kubernetes/overview") return route.fulfill({ json: { connector: "kubernetes", cluster_id: "fixture", observed_at: "2026-10-07T12:00:00Z", nodes: 1, ready_nodes: 1, pods: 1, running_pods: 1, namespaces: 1, active_namespaces: 1, workloads: 1, warnings: 0, truncated: false } });
      if (pathname === "/api/admin/kubernetes/usage") return route.fulfill({ json: { observed_at: "2026-10-07T12:00:00Z", availability: "unavailable", fresh: false, pods: [], nodes: [], truncated: false } });
      if (pathname === "/api/admin/kubernetes/top") return route.fulfill({ json: { items: [], total: 0, truncated: false, availability: "unavailable", fresh: false, sync: { state: "ready", partial: false, age_s: 0 } } });
      if (pathname === "/api/admin/kubernetes/workloads") return route.fulfill({ json: { items: [pod, deployment], counts: { Pod: 1, Deployment: 1 }, truncated: false } });
      if (overviewRefresh && pathname === "/api/admin/kubernetes/changes") return route.fulfill({ json: { items: [{ id: "synthetic-creation", cluster: "fixture", kind: "Pod", namespace: pod.namespace, name: pod.name, type: "created", at: changeAt }], gaps: [], sync: { state: "ready", partial: false, age_s: 0 } } });
      if (pathname.endsWith("/describe")) {
        if (pathname.includes("checkout-removed-pod-0")) return route.fulfill({ json: { resource: { resource_id: "core~v1~pods", kind: "Pod", namespace: "shop", name: "checkout-removed-pod-0", uid: "removed-pod-uid-0", summary: { phase: "Failed" } }, events: [] } });
        if (pathname.includes("/deployments/checkout/")) return route.fulfill({ json: { resource: { resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "shop", name: "checkout", uid: "checkout-uid", summary: { ready_replicas: 0, desired_replicas: 7 } }, events: [] } });
        return route.fulfill({ json: { resource: pathname.includes("deployments") ? deployment : pod, events: [] } });
      }
      if (pathname.startsWith("/api/admin/kubernetes/workloads/") && pathname.endsWith("/logs")) return route.fulfill({ json: { lines: [{ at: at(1), pod: "checkout-7d9f8c-p9m1z", container: "api", text: "live workload snapshot line" }], truncated: false } });
      if (pathname.startsWith("/api/admin/kubernetes/workloads/")) return route.fulfill({ json: { ...pod, truncated: false } });
      if (pathname === "/api/admin/kubernetes/resources") return route.fulfill({ json: { items: url.searchParams.get("resource_id") === "core~v1~namespaces" ? [{ resource_id: "core~v1~namespaces", kind: "Namespace", name: "payments" }] : [], truncated: false } });
      if (pathname.includes("/graph")) return route.fulfill({ json: { nodes: [{ id: "deployment", kind: "Deployment", namespace: "payments", name: "checkout-api", group: "payments" }, { id: "pod", kind: "Pod", namespace: "payments", name: "checkout-api-0", group: "payments" }], edges: [{ from: "deployment", to: "pod", type: "manages" }], omitted: {}, sync: { state: "ready", partial: false, age_s: 0 } } });
      if (pathname === "/api/admin/kubernetes/stream") return route.fulfill({ contentType: "text/event-stream", body: "event: heartbeat\ndata: {}\n\n" });
      if (pathname === "/api/admin/kubernetes/issue-alerts") {
        if (testInfo.title === "Break timeline auto-detect guide desktop dark" && route.request().method() === "PUT") {
          const body = route.request().postDataJSON() as { enabled: boolean; level: string };
          guideAlerts.enabled = body.enabled;
          guideAlerts.level = body.level;
          guideAlerts.enabled_at = body.enabled ? "2026-10-20T10:00:00Z" : "";
        }
        return route.fulfill({ json: guideAlerts });
      }
      if (pathname === "/api/admin/kubernetes/traffic") return route.fulfill({ json: { available: false, reason: "No traffic source", window: "15m", unmapped: 0, truncated: false } });
      if (pathname.includes("/enterprise/")) return route.fulfill({ status: 404, json: { error: "Community deployment" } });
      return route.fulfill({ json: { items: [], totals: {}, gaps: [], truncated: false, sync: { state: "ready", partial: false, age_s: 0 } } });
    });
  });

  for (const viewport of [{ name: "desktop", width: 1440, height: 900 }, { name: "mobile", width: 390, height: 844 }]) {
    test(`Break timeline logs at break and Agent mappings ${viewport.name}`, async ({ page }) => {
      const errors: string[] = [];
      page.on("pageerror", (error) => errors.push(error.message));
      await page.setViewportSize(viewport);
      const now = Date.now();
      const at = (minutes: number) => new Date(now - minutes * 60_000).toISOString();
      const settings: KubernetesAgentSettings = {
        learning: { logs: true, metrics: true, traces: false, scope: { include: [], exclude: ["sandbox"] } },
        break_logs: { linked_source: "loki-prod", fields: {
          loki: { namespace: "namespace", pod: "pod", container: "container" },
          elasticsearch: { namespace: "kubernetes.namespace_name", pod: "kubernetes.pod_name", container: "kubernetes.container_name", time: "@timestamp", message: "message" },
          cloudwatchlogs: { namespace: "kubernetes.namespace_name", pod: "kubernetes.pod_name", container: "kubernetes.container_name", message: "log" },
          graylog: { namespace: "kubernetes_namespace_name", pod: "kubernetes_pod_name", container: "kubernetes_container_name" },
          splunk: { namespace: "namespace", pod: "pod", container: "container_name" },
          signoz: { namespace: "k8s.namespace.name", pod: "k8s.pod.name", container: "k8s.container.name" },
        } },
        sources: [
          { type: "loki", name: "loki-prod", scope: '{cluster="fixture"}', health: "healthy" },
          { type: "elasticsearch", name: "es-logs", scope: "logs-*", health: "not_checked" },
          { type: "cloudwatchlogs", name: "cw-east", scope: "/kubernetes/fixture", health: "healthy" },
          { type: "graylog", name: "graylog-main", scope: "kubernetes", health: "slow" },
          { type: "splunk", name: "splunk-cloud", scope: "index=prod sourcetype=kube", health: "failing" },
          { type: "signoz", name: "signoz-otel", scope: "k8s.cluster.name=fixture", health: "healthy" },
        ], detected: { workloads: 8, namespaces: 2, metrics_server: true, traces: { kind: "tempo", namespace: "telemetry" } }, can_write: true, linked_source_missing: false,
      };
      await page.route("**/api/admin/kubernetes/agent-settings*", async (route) => {
        if (route.request().method() === "PUT") Object.assign(settings, route.request().postDataJSON());
        await route.fulfill({ json: settings });
      });
      await page.route("**/api/admin/kubernetes/agent-settings/test-link*", async (route) => {
        const source = settings.sources.find((item) => item.name === route.request().postDataJSON().source)!;
        if (source.type === "splunk") return route.fulfill({ status: 502, json: { error: "unauthorized" } });
        return route.fulfill({ json: { lines: source.type === "cloudwatchlogs" ? 0 : 18, namespace: "shop", workload: "checkout", duration_ms: source.type === "graylog" ? 4800 : 220, truncated: source.type === "signoz", preview: JSON.stringify([{ type: source.type, scope: source.scope, query: source.type === "cloudwatchlogs" ? { operation: "FilterLogEvents", filter_pattern: '{ $.cluster = "fixture" }', fields: route.request().postDataJSON().fields } : { bounded_read: true, fields: route.request().postDataJSON().fields }, from: at(15), to: at(0), limit: 500 }], null, 2) } });
      });
      await openApp(page, `/agent/kubernetes?view=timeline&root_uid=checkout-uid&namespace=shop&center=${at(5)}`);
      const lane = page.getByRole("region", { name: "Timeline lane checkout", exact: true });
      await expect(lane).toBeVisible();
      const logs = page.getByRole("region", { name: "Logs at break", exact: true });
      await expect(logs.getByRole("log")).toContainText("payment client timeout");
      const handle = lane.getByRole("slider", { name: "Log window start" });
      await handle.evaluate((element) => { (element as HTMLElement).dataset.handleIdentity = "start"; });
      await handle.focus(); await page.keyboard.press("ArrowRight");
      await expect(page).toHaveURL(/logs_from=/);
      await expect(handle).toBeFocused();
      expect(await handle.evaluate((element) => (element as HTMLElement).dataset.handleIdentity)).toBe("start");
      const endHandle = lane.getByRole("slider", { name: "Log window end" });
      await endHandle.evaluate((element) => { (element as HTMLElement).dataset.handleIdentity = "end"; });
      const beforeDragURL = page.url();
      const dragRead = page.waitForRequest((request) => new URL(request.url()).pathname.endsWith("/break-logs"));
      await endHandle.evaluate((element) => {
        const plot = element.closest(".break-log-bracket-plot")!;
        const rect = plot.getBoundingClientRect();
        const targetX = rect.left + rect.width * 0.6;
        element.dispatchEvent(new PointerEvent("pointerdown", { bubbles: true, pointerId: 1, clientX: rect.left + rect.width * 0.5 }));
        element.dispatchEvent(new PointerEvent("pointermove", { bubbles: true, pointerId: 1, clientX: targetX }));
        element.dispatchEvent(new PointerEvent("pointerup", { bubbles: true, pointerId: 1, clientX: targetX }));
      });
      await dragRead;
      expect(page.url()).not.toBe(beforeDragURL);
      expect(await endHandle.evaluate((element) => (element as HTMLElement).dataset.handleIdentity)).toBe("end");
      await page.locator('[data-marker="crash"]').last().click();
      const crashDetails = page.getByRole("region", { name: "Timeline event details", exact: true });
      await expect(crashDetails.getByRole("button", { name: "Logs before crash" }).first()).toBeVisible();
      await expect(crashDetails.getByRole("button", { name: "Open pod" }).first()).toBeVisible();
      await expect(crashDetails).toBeInViewport();
      await captureBreakState(page, `break-crash-popover-${viewport.name}.png`, "dark");
      await crashDetails.getByRole("button", { name: "Open pod" }).first().click();
      const detail = page.getByRole("dialog", { name: "Details panel" });
      await expect(detail.getByRole("heading", { name: "Pod shop/checkout-removed-pod-0", exact: true })).toBeVisible();
      await detail.getByRole("button", { name: "Close panel" }).click();
      await expect(detail).toHaveCount(0);
      await page.locator('[data-marker="crash"]').last().click();
      const previousRead = page.waitForRequest((request) => { const url = new URL(request.url()); return url.pathname.endsWith("/break-logs") && url.searchParams.get("mode") === "previous" && url.searchParams.get("root_uid") === "checkout-uid"; });
      await crashDetails.getByRole("button", { name: "Logs before crash" }).first().click();
      expect(new URL((await previousRead).url()).searchParams.getAll("pods[]")).toHaveLength(1);
      await expect(page).toHaveURL(/logs_mode=previous/);
      const liveLogsButton = viewport.name === "mobile" ? page.getByRole("toolbar", { name: "Break log actions" }).getByRole("button", { name: "Open live logs" }) : logs.getByRole("button", { name: "Open live logs" });
      await liveLogsButton.click();
      await expect(detail.getByRole("region", { name: "Workload live logs" })).toContainText("live workload snapshot line");
      if (process.env.E2E_KUBERNETES_AUTO_DETECT_DOCS === "1") await captureBreakState(page, "auto-detect-live-logs.png", "dark");
      await detail.getByRole("button", { name: "Close panel" }).click();
      await expect(detail).toHaveCount(0);
      await logs.scrollIntoViewIfNeeded();
      await logs.getByRole("button", { name: "Log coverage tip" }).click();
      await expect(logs.getByRole("link", { name: "Link a source in Agent tab" })).toBeVisible();
      const tip = logs.getByRole("region", { name: "Log coverage tip", exact: true });
      const bounds = await tip.boundingBox();
      expect(bounds!.x).toBeGreaterThanOrEqual(0);
      expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(viewport.width);
      if (viewport.name === "mobile") await expect(page.getByRole("toolbar", { name: "Break log actions" })).toBeInViewport();
      await captureBreakState(page, `break-logs-${viewport.name}.png`, "dark");
      if (process.env.E2E_KUBERNETES_AUTO_DETECT_DOCS === "1" && viewport.name === "desktop") {
        await logs.getByRole("button", { name: "Log coverage tip" }).click();
        await expect(tip).toBeHidden();
        for (const mode of ["Merged", "Previous container", "Current"]) {
          await expect(logs.getByRole("button", { name: mode, exact: true })).toBeVisible();
        }
        await expect(logs.getByRole("group", { name: "Log pods", exact: true })).toBeVisible();
        await expect(logs.getByPlaceholder(/search/i)).toBeVisible();
        await expect(logs.getByRole("button", { name: /errors/i })).toBeVisible();
        await expect(logs.getByRole("button", { name: /warnings/i })).toBeVisible();
        await expect(logs.getByRole("log")).toContainText("payment client timeout");
        await expect(logs.getByRole("heading", { name: "Top messages in window", exact: true })).toBeVisible();
        await captureBreakState(page, "auto-detect-log-filters.png", "dark");
        await logs.getByRole("button", { name: "Log coverage tip" }).click();
      }
      await logs.getByRole("link", { name: "Link a source in Agent tab" }).click();
      const agent = page.getByRole("region", { name: "Kubernetes Agent" });
      await expect(agent.getByText("Not available yet.", { exact: true })).toBeVisible();
      await expect(agent.getByRole("switch", { name: "Logs learning" })).toBeChecked();
      await expect(agent.getByRole("switch", { name: "Logs learning" })).toBeDisabled();
      await expect(agent.getByRole("button", { name: "Edit scope" })).toBeDisabled();
      await expect(agent).toContainText("All namespaces; exclude: sandbox");
      await agent.scrollIntoViewIfNeeded();
      await captureBreakState(page, `break-agent-${viewport.name}.png`, "dark");
      const picker = page.getByLabel("Configured log sources");
      for (const source of settings.sources) {
        await agent.getByRole("button", { name: /^(loki-prod|es-logs|cw-east|graylog-main|splunk-cloud|signoz-otel)$/ }).click();
        await expect(picker).toBeVisible();
        await expect(picker).toBeInViewport();
        expect(await picker.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(true);
        if (source.type === "loki") await captureBreakState(page, `break-source-picker-${viewport.name}.png`, "dark");
        await picker.getByRole("button", { name: new RegExp(`^${source.name}`) }).click();
        const form = agent.getByRole("region", { name: / mapping$/ });
        await form.getByRole("button", { name: "Test link" }).click();
        if (source.type === "splunk") await expect(form.getByRole("alert")).toContainText("Source refused the read");
        else await expect(form).toContainText(source.type === "cloudwatchlogs" ? "Found 0 lines" : "Found 18 lines");
        if (source.type === "cloudwatchlogs") await expect(form.locator("pre")).toContainText("FilterLogEvents");
        if (source.type === "signoz") await expect(form).toContainText("results truncated");
        await form.scrollIntoViewIfNeeded();
        expect(await form.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(true);
        await captureBreakState(page, `break-mapping-${source.type}-${viewport.name}.png`, "dark");
        const saved = page.waitForResponse((response) => response.request().method() === "PUT" && new URL(response.url()).pathname.endsWith("/agent-settings"));
        await form.getByRole("button", { name: /^(Save|Use this source)$/ }).click();
        expect((await saved).status()).toBe(200);
        await expect(form.getByText("Linked", { exact: true })).toBeVisible();
      }
      expect(errors).toEqual([]);
    });

    test(`Break timeline logs at break and Agent light theme, read-only, no-source and polling continuity ${viewport.name}`, async ({ page }) => {
      const errors: string[] = [];
      page.on("pageerror", (error) => errors.push(error.message));
      await page.setViewportSize(viewport);
      const now = Date.now();
      await page.clock.install({ time: new Date(now) });
      const at = (minutes: number) => new Date(now - minutes * 60_000).toISOString();
      let settingsMode: "writable" | "readonly" | "missinglinked" | "nosources" = "writable";
      const settings: KubernetesAgentSettings = {
        learning: { logs: true, metrics: true, traces: false, scope: { include: ["shop"], exclude: ["sandbox"] } },
        break_logs: { linked_source: "loki-prod", fields: {
          loki: { namespace: "namespace", pod: "pod", container: "container" },
          elasticsearch: { namespace: "kubernetes.namespace_name", pod: "kubernetes.pod_name", container: "kubernetes.container_name", time: "@timestamp", message: "message" },
          cloudwatchlogs: { namespace: "kubernetes.namespace_name", pod: "kubernetes.pod_name", container: "kubernetes.container_name", message: "log" },
          graylog: { namespace: "kubernetes_namespace_name", pod: "kubernetes_pod_name", container: "kubernetes_container_name" },
          splunk: { namespace: "namespace", pod: "pod", container: "container_name" },
          signoz: { namespace: "k8s.namespace.name", pod: "k8s.pod.name", container: "k8s.container.name" },
        } },
        sources: [
          { type: "loki", name: "loki-prod", scope: '{cluster="fixture"}', health: "healthy" },
          { type: "elasticsearch", name: "es-logs", scope: "logs-*", health: "not_checked" },
          { type: "cloudwatchlogs", name: "cw-east", scope: "/kubernetes/fixture", health: "healthy" },
          { type: "graylog", name: "graylog-main", scope: "kubernetes", health: "slow" },
          { type: "splunk", name: "splunk-cloud", scope: "index=prod sourcetype=kube", health: "failing" },
          { type: "signoz", name: "signoz-otel", scope: "k8s.cluster.name=fixture", health: "healthy" },
        ], detected: { workloads: 8, namespaces: 2, metrics_server: true, traces: { kind: "tempo", namespace: "telemetry" } }, can_write: true, linked_source_missing: false,
      };
      await page.route("**/api/admin/kubernetes/agent-settings*", async (route) => {
        if (route.request().method() === "PUT") Object.assign(settings, route.request().postDataJSON());
        if (settingsMode === "readonly") return route.fulfill({ json: { ...settings, can_write: false } });
        if (settingsMode === "missinglinked") return route.fulfill({ json: { ...settings, linked_source_missing: true } });
        if (settingsMode === "nosources") return route.fulfill({ json: { ...settings, break_logs: { ...settings.break_logs, linked_source: "" }, sources: [], linked_source_missing: false } });
        return route.fulfill({ json: settings });
      });
      await page.route("**/api/admin/kubernetes/agent-settings/test-link*", async (route) => {
        const source = settings.sources.find((item) => item.name === route.request().postDataJSON().source)!;
        return route.fulfill({ json: { lines: 18, namespace: "shop", workload: "checkout", duration_ms: 220, truncated: false, preview: JSON.stringify([{ type: source.type, scope: source.scope, query: { bounded_read: true, fields: route.request().postDataJSON().fields }, from: at(15), to: at(0), limit: 500 }], null, 2) } });
      });
      let revision = 0;
      let holdRefresh = false;
      const releaseRefresh: { current: (() => void) | null } = { current: null };
      const breakLogQueries: Array<{ cursor: string | null; since: string; until: string }> = [];
      await page.route("**/api/admin/kubernetes/break-logs*", async (route) => {
        const query = new URL(route.request().url()).searchParams;
        const cursor = query.get("cursor");
        breakLogQueries.push({ cursor, since: query.get("since")!, until: query.get("until")! });
        if (!cursor) revision++;
        if (holdRefresh && !cursor) {
          await new Promise<void>((resolve) => { releaseRefresh.current = resolve; });
        }
        const currentRevision = revision;
        const lines = cursor
          ? [{ at: at(4), pod: "checkout-removed-x2k4q", pod_uid: "", container: "api", init: false, slot: "previous" as const, level: "warn" as const, text: `older cursor page ${cursor}`, source: "captured" as const }]
          : Array.from({ length: 30 }, (_, index) => ({ at: at(6 - index / 30), pod: index % 2 ? "checkout-7d9f8c-p9m1z" : "checkout-removed-x2k4q", pod_uid: index % 2 ? "pod-live" : "", container: "api", init: false, slot: (index > 12 ? "current" : "previous") as "current" | "previous", level: (index > 3 && index < 8 ? "error" : "info") as "error" | "info", text: `capture ${currentRevision} line ${index} payment client timeout`, source: "captured" as const }));
        const result: KubernetesBreakLogPage = {
          window: { from: query.get("since")!, to: query.get("until")! },
          pods: [{ name: "checkout-removed-x2k4q", uid: "", short: "x2k4q", state: "unknown" }, { name: "checkout-7d9f8c-p9m1z", uid: "pod-live", short: "p9m1z", state: "running" }],
          lines,
          markers: [{ type: "break", at: at(5), label: "Deployment checkout unavailable" }, { type: "crash", at: at(4.5), label: "api OOMKilled exit 137" }],
          counts: { all: 30, errors: 4, warnings: 1 }, rate: Array.from({ length: 7 }, (_, index) => ({ minute: at(10 - index), total: index + 3, errors: index > 3 ? index : 0 })),
          patterns: [{ template: "payment client timeout", level: "error", count: 4 }],
          sources: [{ kind: "captured", name: "fixture", at: at(4) }],
          coverage: { state: "partial", reason: "restarted_since", tip: true, gaps: [{ from: at(10), to: at(8) }] },
          next: cursor ? undefined : `cursor-${currentRevision}`,
        };
        await route.fulfill({ json: result });
      });
      await openApp(page, `/agent/kubernetes?view=timeline&root_uid=checkout-uid&namespace=shop&center=${at(5)}&logs_from=${at(6)}&logs_to=${at(-2)}`);
      const logs = page.getByRole("region", { name: "Logs at break", exact: true });
      const viewer = logs.getByRole("log");
      await expect(viewer).toContainText("capture 1 line 0 payment client timeout");
      await logs.getByRole("button", { name: "Load more logs" }).click();
      await expect(viewer).toContainText("older cursor page cursor-1");
      expect(breakLogQueries.at(-1)?.cursor).toBe("cursor-1");
      await viewer.evaluate((element) => {
        (element as HTMLElement).dataset.viewerInstance = "preserved";
        element.scrollTop = 64;
      });
      expect(await viewer.evaluate((element) => element.scrollTop)).toBeGreaterThan(0);
      const scrollBeforePoll = await viewer.evaluate((element) => element.scrollTop);
      holdRefresh = true;
      await page.clock.runFor(10_000);
      await expect.poll(() => breakLogQueries.length).toBe(3);
      expect(breakLogQueries[2].cursor).toBeNull();
      await expect(viewer).toContainText("capture 1 line 0 payment client timeout");
      await expect(viewer).not.toContainText("older cursor page cursor-1");
      expect(await viewer.evaluate((element) => ({ id: (element as HTMLElement).dataset.viewerInstance, scrollTop: element.scrollTop }))).toEqual({ id: "preserved", scrollTop: scrollBeforePoll });
      holdRefresh = false;
      releaseRefresh.current?.();
      await expect(viewer).toContainText("capture 2 line 0 payment client timeout");
      expect(await viewer.evaluate((element) => ({ id: (element as HTMLElement).dataset.viewerInstance, scrollTop: element.scrollTop }))).toEqual({ id: "preserved", scrollTop: scrollBeforePoll });
      await logs.getByRole("button", { name: "Load more logs" }).click();
      await expect(viewer).toContainText("older cursor page cursor-2");
      expect(breakLogQueries.at(-1)?.cursor).toBe("cursor-2");
      await page.evaluate(() => {
        Object.defineProperty(document, "visibilityState", { configurable: true, get: () => "hidden" });
        document.dispatchEvent(new Event("visibilitychange"));
      });
      const countWhileHidden = breakLogQueries.length;
      await page.clock.runFor(60_000);
      expect(breakLogQueries.length).toBe(countWhileHidden);
      await page.evaluate(() => {
        Object.defineProperty(document, "visibilityState", { configurable: true, get: () => "visible" });
        document.dispatchEvent(new Event("visibilitychange"));
      });
      await expect(viewer).toContainText("capture 3 line 0 payment client timeout");
      await expect(viewer).not.toContainText("older cursor page cursor-2");
      expect(breakLogQueries.at(-1)?.cursor).toBeNull();
      await page.clock.runFor(240_000);
      const settledLength = breakLogQueries.length;
      await page.clock.runFor(60_000);
      expect(breakLogQueries.length).toBe(settledLength);

      await logs.scrollIntoViewIfNeeded();
      await logs.getByRole("button", { name: "Log coverage tip" }).click();
      await expect(logs.getByRole("link", { name: "Link a source in Agent tab" })).toBeVisible();
      await captureBreakState(page, `break-logs-light-${viewport.name}.png`, "light");
      await logs.getByRole("link", { name: "Link a source in Agent tab" }).click();
      const agent = page.getByRole("region", { name: "Kubernetes Agent" });
      await expect(agent.getByText("Not available yet.", { exact: true })).toBeVisible();
      await agent.scrollIntoViewIfNeeded();
      await captureBreakState(page, `break-agent-light-${viewport.name}.png`, "light");
      await agent.getByRole("button", { name: "loki-prod" }).click();
      const picker = page.getByLabel("Configured log sources");
      await expect(picker).toBeVisible();
      await captureBreakState(page, `break-source-picker-light-${viewport.name}.png`, "light");
      await picker.getByRole("button", { name: /^loki-prod/ }).click();
      const form = agent.getByRole("region", { name: /Loki mapping$/ });
      await form.getByRole("button", { name: "Test link" }).click();
      await expect(form).toContainText("Found 18 lines");
      await form.scrollIntoViewIfNeeded();
      await captureBreakState(page, `break-mapping-loki-light-${viewport.name}.png`, "light");

      settingsMode = "readonly";
      await page.clock.runFor(60_000);
      await page.getByRole("tab", { name: "Timeline", exact: true }).click();
      await page.getByRole("tab", { name: "Agent", exact: true }).click();
      await expect(agent.getByText("Read only. An administrator can change these settings.", { exact: true })).toBeVisible();
      await expect(agent.getByRole("switch", { name: "Logs learning" })).toBeDisabled();
      await expect(agent.getByRole("textbox")).toHaveCount(0);
      await expect(agent.getByRole("button", { name: "Edit scope" })).toBeDisabled();
      await expect(agent.getByRole("button", { name: /^(Test link|Save|Use this source|Save settings)$/ })).toHaveCount(0);
      await agent.scrollIntoViewIfNeeded();
      await captureBreakState(page, `break-agent-readonly-${viewport.name}.png`, "light");

      settingsMode = "missinglinked";
      await page.clock.runFor(60_000);
      await page.getByRole("tab", { name: "Timeline", exact: true }).click();
      await page.getByRole("tab", { name: "Agent", exact: true }).click();
      await expect(agent.getByText("The saved source loki-prod is no longer available. It is not being read.", { exact: true })).toBeVisible();

      settingsMode = "nosources";
      await page.clock.runFor(60_000);
      await page.getByRole("tab", { name: "Timeline", exact: true }).click();
      await page.getByRole("tab", { name: "Agent", exact: true }).click();
      await expect(agent.getByText("No log data source is configured", { exact: true })).toBeVisible();
      await expect(agent.getByRole("button", { name: "None available" })).toBeDisabled();
      await expect(agent.getByRole("link", { name: "Read the data sources guide" })).toHaveAttribute("href", "https://docs.versusincident.com/#/agent/data-sources");
      await expect(agent.getByRole("link", { name: "Data sources settings" })).toHaveAttribute("href", "/settings?tab=agent-sources");
      await agent.scrollIntoViewIfNeeded();
      await captureBreakState(page, `break-agent-nosources-${viewport.name}.png`, "light");

      await page.goto(`/agent/kubernetes?view=timeline&root_uid=checkout-uid&namespace=shop&center=${at(5)}`);
      await expect(logs.getByRole("log")).toBeVisible();
      const askChat = viewport.name === "mobile" ? page.getByRole("toolbar", { name: "Break log actions" }).getByRole("button", { name: "Ask chat" }) : logs.getByRole("button", { name: "Ask chat" });
      await askChat.click();
      await expect(page).toHaveURL(/\/agent\/chat\?.*provider=kubernetes.*resource_id=apps(?:~|%7E)v1(?:~|%7E)deployments.*namespace=shop.*name=checkout.*uid=checkout-uid.*logs_from=.*logs_to=/);
      expect(errors).toEqual([]);
    });

    test(`Break timeline issue alerts control, states, rollback, chat prefill, login return and channel mocks ${viewport.name}`, async ({ page }) => {
      const errors: string[] = [];
      page.on("pageerror", (error) => errors.push(error.message));
      await page.setViewportSize(viewport);
      const alertState = {
        enabled: false,
        level: "critical" as "critical" | "warning",
        enabled_at: "",
        can_write: true,
        available: true,
        reason: "",
        links_ready: true,
        channels_enabled: true,
      };
      let failSave = false;
      let holdSave: Promise<void> | null = null;
      let releaseSave: (() => void) | null = null;
      const puts: Array<{ enabled: boolean; level: string }> = [];

      await page.route("**/api/admin/kubernetes/issue-alerts*", async (route) => {
        if (route.request().method() === "PUT") {
          const body = route.request().postDataJSON() as { enabled: boolean; level: "critical" | "warning" };
          puts.push(body);
          if (holdSave) await holdSave;
          if (failSave) {
            return route.fulfill({ status: 500, json: { error: "Save refused by policy" } });
          }
          alertState.enabled = body.enabled;
          alertState.level = body.level;
          if (body.enabled && !alertState.enabled_at) alertState.enabled_at = "2026-10-19T12:04:00Z";
        }
        return route.fulfill({ json: alertState });
      });

      await openApp(page, "/agent/kubernetes?view=issues");
      const issues = page.getByRole("region", { name: "Kubernetes issues", exact: true });
      await expect(issues).toBeVisible();
      const toggle = issues.getByRole("switch", { name: "Alert on new issues", exact: true });
      await expect(toggle).toBeVisible();
      await expect(toggle).toBeEnabled();
      await expect(toggle).toHaveAttribute("aria-checked", "false");
      const describedBy = await toggle.getAttribute("aria-describedby");
      expect(describedBy).toBeTruthy();
      await expect(issues.locator(`[id="${describedBy}"]`)).toBeAttached();
      await expect(issues.getByRole("group", { name: "Issue alert level", exact: true })).toHaveCount(0);
      await expect(issues.getByText("Sends through your alert channels", { exact: true })).toBeVisible();
      const docsLink = issues.getByRole("link", { name: "Channel settings", exact: true });
      await expect(docsLink).toHaveAttribute("href", "https://docs.versusincident.com/#/agent/channels");
      await expect(docsLink).toHaveAttribute("target", "_blank");
      await expect(docsLink).toHaveAttribute("rel", "noopener noreferrer");
      await expect(issues).not.toContainText(/last alert/i);
      await captureBreakState(page, `issue-alerts-off-${viewport.name}-dark.png`, "dark");

      await toggle.focus();
      await expect(toggle).toBeFocused();
      await page.keyboard.press("Space");
      await expect(toggle).toHaveAttribute("aria-checked", "true");
      expect(puts.at(-1)).toEqual({ enabled: true, level: "critical" });
      const levelGroup = issues.getByRole("group", { name: "Issue alert level", exact: true });
      await expect(levelGroup).toBeVisible();
      const criticalBtn = levelGroup.getByRole("button", { name: "Critical", exact: true });
      const warningBtn = levelGroup.getByRole("button", { name: "Warning", exact: true });
      await expect(criticalBtn).toHaveAttribute("aria-pressed", "true");
      await expect(warningBtn).toHaveAttribute("aria-pressed", "false");
      await expect(issues.locator(`[id="${describedBy}"]`)).toContainText("Critical sends critical issues only");
      await expect(page.getByText("Alerts on. Issues that break from now on will alert.", { exact: true })).toBeVisible();
      const layoutGeometry = await issues.evaluate((_, isMobile) => {
        const switchEl = document.querySelector<HTMLElement>('[role="switch"][aria-label="Alert on new issues"]')!;
        const switchRow = switchEl.parentElement!;
        const groupEl = document.querySelector<HTMLElement>('[role="group"][aria-label="Issue alert level"]')!;
        const buttons = [...groupEl.querySelectorAll<HTMLElement>("button")];
        const rowRect = switchRow.getBoundingClientRect();
        const switchRect = switchEl.getBoundingClientRect();
        const groupRect = groupEl.getBoundingClientRect();
        const btnWidths = buttons.map((b) => b.getBoundingClientRect().width);
        return {
          isMobile,
          sameLineDesktop: Math.abs(rowRect.top - groupRect.top) < 12 && groupRect.left >= switchRect.right,
          wrappedMobile: groupRect.top >= rowRect.bottom - 1 && Math.abs(groupRect.width - rowRect.width) <= 2,
          equalButtons: Math.abs(btnWidths[0] - btnWidths[1]) <= 2,
        };
      }, viewport.name === "mobile");
      if (viewport.name === "mobile") {
        expect(layoutGeometry.wrappedMobile).toBe(true);
        expect(layoutGeometry.equalButtons).toBe(true);
      } else {
        expect(layoutGeometry.sameLineDesktop).toBe(true);
      }
      await captureBreakState(page, `issue-alerts-on-critical-${viewport.name}-dark.png`, "dark");

      await warningBtn.focus();
      await expect(warningBtn).toBeFocused();
      await page.keyboard.press("Enter");
      await expect(warningBtn).toHaveAttribute("aria-pressed", "true");
      await expect(criticalBtn).toHaveAttribute("aria-pressed", "false");
      expect(puts.at(-1)).toEqual({ enabled: true, level: "warning" });
      await expect(issues.locator(`[id="${describedBy}"]`)).toContainText("Warning sends warning and critical issues");
      await expect(page.getByText("Alert level saved.", { exact: true })).toBeVisible();
      await captureBreakState(page, `issue-alerts-on-warning-${viewport.name}-dark.png`, "dark");

      failSave = true;
      holdSave = new Promise<void>((resolve) => { releaseSave = resolve; });
      await criticalBtn.click();
      await expect(criticalBtn).toHaveAttribute("aria-pressed", "true");
      await expect(issues.getByRole("status").filter({ hasText: "Saving…" })).toBeVisible();
      await expect(toggle).toBeDisabled();
      await expect(criticalBtn).toBeDisabled();
      await expect(warningBtn).toBeDisabled();
      holdSave = null;
      releaseSave!();
      await expect(warningBtn).toHaveAttribute("aria-pressed", "true");
      await expect(criticalBtn).toHaveAttribute("aria-pressed", "false");
      await expect(toggle).toHaveAttribute("aria-checked", "true");
      await expect(page.getByText("Couldn't save issue alerts.", { exact: true })).toBeVisible();
      await expect(page.getByText("Save refused by policy", { exact: true })).toBeVisible();
      await captureBreakState(page, `issue-alerts-rollback-${viewport.name}-dark.png`, "dark");
      failSave = false;

      const refreshAlerts = async (patch: Partial<typeof alertState>) => {
        Object.assign(alertState, {
          enabled: true,
          level: "warning",
          can_write: true,
          available: true,
          reason: "",
          links_ready: true,
          channels_enabled: true,
          ...patch,
        });
        await page.reload();
        await expect(issues).toBeVisible();
      };

      await refreshAlerts({ enabled: true, level: "warning", can_write: false });
      await expect(issues.getByText("Ask an admin to change alerts", { exact: true })).toBeVisible();
      await expect(toggle).toBeDisabled();
      await expect(criticalBtn).toBeDisabled();
      await expect(warningBtn).toBeDisabled();
      await captureBreakState(page, `issue-alerts-readonly-${viewport.name}-dark.png`, "dark");

      await refreshAlerts({ enabled: false, level: "critical", available: false, reason: "demand_capture" });
      await expect(issues.getByText("Alerts need continuous change capture", { exact: true })).toBeVisible();
      await expect(toggle).toBeDisabled();
      await expect(issues.getByRole("group", { name: "Issue alert level", exact: true })).toHaveCount(0);
      await captureBreakState(page, `issue-alerts-demand-${viewport.name}-dark.png`, "dark");

      await refreshAlerts({ enabled: true, level: "critical", channels_enabled: false });
      await expect(issues.getByText("No alert channel is on", { exact: true })).toBeVisible();
      await expect(toggle).toBeEnabled();
      await expect(docsLink).toBeVisible();
      await captureBreakState(page, `issue-alerts-no-channel-${viewport.name}-dark.png`, "dark");

      await refreshAlerts({ enabled: true, level: "critical", links_ready: false });
      await expect(issues.getByText("Set public_host to add Timeline and agent links", { exact: true })).toBeVisible();
      await expect(toggle).toBeEnabled();
      await expect(issues).not.toContainText(/last alert/i);
      await captureBreakState(page, `issue-alerts-no-host-${viewport.name}-dark.png`, "dark");

      let enterpriseAuthed = false;
      const chatCreates: unknown[] = [];
      const chatMessages: unknown[] = [];
      await page.route("**/api/admin/config/agent", async (route) => {
        if (!enterpriseAuthed) return route.fulfill({ status: 401, json: { error: "unauthorized" } });
        return route.fulfill({ json: { enable: true, mode: "shadow", sources: [], ai: { enable: true }, catalog: {}, miner: {}, regex: { rules: [] }, redaction: {}, service_patterns: [] } });
      });
      await page.route("**/enterprise/api/**", async (route) => {
        const pathname = new URL(route.request().url()).pathname;
        if (pathname.endsWith("/deployment")) return route.fulfill({ json: { org: "acme" } });
        if (pathname.endsWith("/session")) {
          return enterpriseAuthed
            ? route.fulfill({ json: { org: "acme", subject: "admin@acme.test", role: "owner", mfa: true, amr: ["pwd", "otp"], local: true } })
            : route.fulfill({ status: 401, json: { error: "no active session" } });
        }
        if (pathname.endsWith("/status")) {
          return route.fulfill({
            json: {
              enabled: true,
              connections: [{ id: "work", type: "oidc", display_name: "Okta SSO", login_url: "/enterprise/api/sso/acme/login?connection=work" }],
            },
          });
        }
        if (pathname.endsWith("/auth/local/login") && route.request().method() === "POST") {
          enterpriseAuthed = true;
          return route.fulfill({ status: 204, body: "" });
        }
        if (pathname.endsWith("/rbac/me")) {
          return route.fulfill({ json: { org: "acme", subject: "admin@acme.test", role: "owner", permissions: ["infrastructure:view", "infrastructure:settings:write", "agent:approve"], clusters: null } });
        }
        return route.fulfill({ status: 404, json: { error: "not found" } });
      });
      await page.route("**/api/admin/chat/**", async (route) => {
        const pathname = new URL(route.request().url()).pathname;
        const method = route.request().method();
        if (pathname === "/api/admin/chat/sessions" && method === "GET") return route.fulfill({ json: { sessions: [] } });
        if (pathname === "/api/admin/chat/sessions" && method === "POST") {
          chatCreates.push(route.request().postDataJSON());
          return route.fulfill({ status: 201, json: { id: "session-1", status: "idle", seeded: false, created_at: "2026-10-19T12:05:00Z", updated_at: "2026-10-19T12:05:00Z", turns: [] } });
        }
        if (pathname.endsWith("/messages") && method === "POST") {
          chatMessages.push(route.request().postDataJSON());
          return route.fulfill({ status: 200, contentType: "text/event-stream", body: 'event: run_finished\ndata: {"seq":1,"at":"2026-10-19T12:05:01Z","kind":"run_finished"}\n\n' });
        }
        return route.fulfill({ json: {} });
      });

      const timelineHref = `/agent/kubernetes?cluster=east&view=timeline&root_uid=checkout-uid&namespace=shop&center=${encodeURIComponent(new Date(Date.now() - 5 * 60_000).toISOString())}`;
      const chatTarget = "/agent/chat?provider=kubernetes&cluster=east&resource_id=apps~v1~deployments&name=checkout&namespace=shop&uid=checkout-uid&include_changes=true&issue=pod.crash_loop";
      const ackHref = "/api/incidents/inc-checkout-1/ack?token=ack-token-fixture";

      let ssoRedirectURL = "";
      await page.route("**/enterprise/api/sso/acme/login*", async (route) => {
        ssoRedirectURL = route.request().url();
        enterpriseAuthed = true;
        const returnTo = new URL(ssoRedirectURL).searchParams.get("return_to") || "/";
        await route.fulfill({ status: 302, headers: { Location: returnTo }, body: "" });
      });

      await page.goto(timelineHref);
      await expect(page.getByTestId("local-login-form")).toBeVisible();
      const ssoButton = page.getByRole("button", { name: "Sign in with Okta SSO", exact: true });
      await expect(ssoButton).toBeVisible();
      await ssoButton.click();
      await expect.poll(() => ssoRedirectURL).not.toBe("");
      expect(new URL(ssoRedirectURL).searchParams.get("return_to")).toBe(timelineHref);
      await expect(page.getByRole("region", { name: "Timeline lane checkout", exact: true })).toBeVisible();
      expect(new URL(page.url()).pathname + new URL(page.url()).search).toBe(timelineHref);

      enterpriseAuthed = false;
      ssoRedirectURL = "";
      await page.goto(chatTarget);
      await expect(page.getByTestId("local-login-form")).toBeVisible();
      await expect(ssoButton).toBeVisible();
      await captureBreakState(page, `issue-alerts-login-return-${viewport.name}-dark.png`, "dark");

      await page.getByTestId("local-login-username").fill("admin");
      await page.getByTestId("local-login-password").fill("fixture-admin-password");
      await page.getByTestId("local-login-submit").click();
      await expect(page.getByTestId("app-authenticated")).toBeVisible();
      expect(new URL(page.url()).pathname + new URL(page.url()).search).toBe(chatTarget);
      const composer = page.getByRole("textbox", { name: "Message", exact: true });
      await expect(composer).toHaveValue("Why is Deployment checkout crash looping?");
      await expect(page.getByText("shop/checkout", { exact: true })).toBeVisible();
      await expect(page.getByRole("button", { name: "Send", exact: true })).toBeEnabled();
      expect(chatCreates).toHaveLength(0);
      expect(chatMessages).toHaveLength(0);
      await captureBreakState(page, `issue-alerts-chat-prefill-${viewport.name}-dark.png`, "dark");

      const emailTemplatePath = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../../config/kubernetes_issue_email.tmpl");
      const rawEmailTemplate = fs.readFileSync(emailTemplatePath, "utf8");
      expect(rawEmailTemplate).not.toContain("escapeHTML .url");
      expect(rawEmailTemplate).not.toContain("escapeHTML .AckURL");
      expect(rawEmailTemplate.indexOf("{{ if .AckURL }}")).toBeGreaterThan(0);
      expect(rawEmailTemplate.indexOf("{{ if .AckURL }}")).toBeLessThan(rawEmailTemplate.indexOf("{{ range .Links }}"));
      const renderedEmailBody = rawEmailTemplate
        .replace(/^[\s\S]*?<body[^>]*>/i, "")
        .replace(/<\/body>[\s\S]*$/i, "")
        .replace('{{ if eq .Severity "critical" }}🔴{{ else }}🟡{{ end }}', "🔴")
        .replace('{{ or .AlertName "Detected issue" }}', "Deployment checkout-api is crash looping")
        .replace('{{ or .KubernetesSeverity .Severity "warning" }}', "critical")
        .replace('{{ or .Cluster "" }}', "prod-eu")
        .replace('{{ or .Namespace "" }}', "shop")
        .replace('{{ or .WorkloadKind "" }}', "Deployment")
        .replace('{{ or .Workload "" }}', "checkout-api")
        .replace('{{ or .Affected "" }}', "3 of 3 pods")
        .replace('{{ or .BrokeAt "" }}', "2026-10-19T12:03:00Z")
        .replace('{{ if .BrokeAgo }}, {{ .BrokeAgo }}{{ end }}', ", 1m0s ago")
        .replace("{{ range .Evidence }}<li>{{ . }}</li>{{ end }}", ["Exit 137 OOMKilled 4 times", "12 restarts", "9 warning events"].map((line) => `<li>${line}</li>`).join(""))
        .replace(/\{\{\s*if or \.Changes \.HistoryIncomplete\s*\}\}[\s\S]*?\{\{\s*end\s*\}\}\s*\{\{\s*end\s*\}\}/, "<h3>Changes before the break</h3><ul><li>Image changed 4m0s before break</li></ul>")
        .replace("{{ if .AckURL }}<a href=\"{{ .AckURL }}\">Acknowledge</a>{{ end }}", `<a href="${ackHref}">Acknowledge</a>`)
        .replace(/\{\{\s*range \.Links\s*\}\}<a[^>]*href="\{\{\s*\.url\s*\}\}">\{\{\s*\.label\s*\}\}<\/a>\s*\{\{\s*end\s*\}\}/, [
          { label: "Open timeline", url: timelineHref },
          { label: "Investigate with DevOps Agent", url: chatTarget },
        ].map((link) => `<a href="${link.url}">${link.label}</a>`).join(""));

      await page.route("**/__mock_channel_preview/*", async (route) => {
        const kind = new URL(route.request().url()).pathname.endsWith("/slack") ? "slack" : "email";
        const html = kind === "slack" ? `<!DOCTYPE html>
<html data-theme="dark"><head><meta charset="utf-8"><style>
  body { margin: 0; background: rgb(13, 17, 24); color: #E4E8EF; font-family: Inter, system-ui, sans-serif; padding: 16px; box-sizing: border-box; }
  .shell { max-width: 680px; margin: 0 auto; background: #1A1D21; border: 1px solid #2A3142; border-radius: 12px; padding: 16px; }
  .hdr { display: flex; align-items: center; gap: 8px; padding-bottom: 12px; border-bottom: 1px solid #2A2D31; margin-bottom: 14px; font-weight: 600; font-size: 14px; color: #F5F7FA; }
  .badge { font-size: 10px; background: #2A344A; color: #A3ACBD; padding: 2px 6px; border-radius: 4px; }
  .att { border-left: 4px solid #F87171; padding-left: 12px; margin-bottom: 16px; }
  .att.warn { border-left-color: #FBBF24; margin-bottom: 0; }
  .title { font-weight: 600; font-size: 14px; color: #F5F7FA; margin-bottom: 8px; }
  .facts { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 6px 12px; font-size: 12px; margin-bottom: 10px; }
  .facts b { color: #A3ACBD; }
  .sec { font-weight: 600; font-size: 12px; color: #F5F7FA; margin: 8px 0 4px; }
  ul { margin: 0 0 8px 0; padding-left: 16px; font-size: 12px; }
  .actions { display: flex; flex-wrap: wrap; gap: 8px; margin-top: 10px; }
  .btn { display: inline-flex; align-items: center; padding: 6px 12px; border-radius: 4px; border: 1px solid #565C66; background: #1A1D21; color: #E4E8EF; font-size: 12px; font-weight: 600; text-decoration: none; }
</style></head><body>
  <main class="shell" aria-label="Slack issue alert mock">
    <div class="hdr"># alerts-prod <span class="badge">Versus APP · Synthetic Slack payload fixture</span></div>
    <section class="att" aria-label="Critical Slack alert">
      <div class="title">🔴 Kubernetes issue Deployment checkout-api is crash looping</div>
      <div class="facts">
        <div><b>Severity:</b> critical</div><div><b>Cluster:</b> prod-eu</div>
        <div><b>Namespace:</b> shop</div><div><b>Workload:</b> Deployment checkout-api</div>
        <div><b>Affected:</b> 3 of 3 pods</div><div><b>Broke at:</b> 2026-10-19T12:03:00Z, 1m0s ago</div>
      </div>
      <div class="sec">What we saw</div>
      <ul><li>Exit 137 OOMKilled 4 times</li><li>12 restarts</li><li>9 warning events</li></ul>
      <div class="sec">Changes before the break</div>
      <ul><li>Image changed 4m0s before break</li></ul>
      <div class="actions" role="group" aria-label="Critical Slack actions">
        <a class="btn" href="${ackHref}">Acknowledge</a>
        <a class="btn" href="${timelineHref}">Open timeline</a>
        <a class="btn" href="${chatTarget}">Investigate with DevOps Agent</a>
      </div>
    </section>
    <section class="att warn" aria-label="Warning Slack alert">
      <div class="title">🟡 Kubernetes issue Deployment payments-worker has a stalled rollout</div>
      <div class="facts">
        <div><b>Severity:</b> warning</div><div><b>Cluster:</b> prod-eu</div>
        <div><b>Namespace:</b> shop</div><div><b>Workload:</b> Deployment payments-worker</div>
        <div><b>Affected:</b> 1 of 2 pods</div><div><b>Broke at:</b> 2026-10-19T11:52:00Z, 12m0s ago</div>
      </div>
      <div class="sec">What we saw</div>
      <ul><li>ProgressDeadlineExceeded 1 times</li><li>2 warning events</li></ul>
      <div class="actions" role="group" aria-label="Warning Slack actions">
        <a class="btn" href="${timelineHref}">Open timeline</a>
        <a class="btn" href="${chatTarget}">Investigate with DevOps Agent</a>
      </div>
    </section>
  </main>
</body></html>` : `<!DOCTYPE html>
<html data-theme="dark"><head><meta charset="utf-8"><style>
  body { margin: 0; background: rgb(13, 17, 24); color: #E4E8EF; font-family: Inter, system-ui, sans-serif; padding: 16px; box-sizing: border-box; }
  .card { max-width: 680px; margin: 0 auto; background: #1E2638; border: 1px solid #2A3142; border-radius: 10px; padding: 16px; }
  .meta { font-size: 11px; color: #A3ACBD; text-transform: uppercase; letter-spacing: 0.06em; padding-bottom: 10px; border-bottom: 1px solid #2A3142; margin-bottom: 12px; }
  h2 { font-size: 16px; color: #F5F7FA; margin: 0 0 12px; }
  p { font-size: 13px; line-height: 1.5; margin: 0 0 12px; background: #161C28; padding: 12px; border-radius: 6px; }
  h3 { font-size: 13px; color: #F5F7FA; margin: 12px 0 6px; }
  ul { margin: 0 0 12px; padding-left: 18px; font-size: 13px; }
  .card a { display: inline-block !important; padding: 8px 12px !important; margin: 4px 8px 0 0 !important; background: #262E42 !important; border: 1px solid #4C566E !important; color: #7DB2FC !important; border-radius: 6px !important; font-size: 12px !important; font-weight: 500 !important; text-decoration: none !important; }
</style></head><body>
  <main class="card" aria-label="Email issue alert mock">
    <div class="meta">Email · From Versus Incident · Settled template render fixture</div>
    ${renderedEmailBody}
  </main>
</body></html>`;
        await route.fulfill({ status: 200, contentType: "text/html; charset=utf-8", body: html });
      });

      await page.goto("/__mock_channel_preview/slack");
      const criticalActions = page.getByRole("group", { name: "Critical Slack actions", exact: true }).getByRole("link");
      await expect(criticalActions).toHaveText(["Acknowledge", "Open timeline", "Investigate with DevOps Agent"]);
      const warningActions = page.getByRole("group", { name: "Warning Slack actions", exact: true }).getByRole("link");
      await expect(warningActions).toHaveText(["Open timeline", "Investigate with DevOps Agent"]);
      await captureBreakState(page, `issue-alerts-slack-mock-${viewport.name}-dark.png`, "dark", "F: synthetic Slack attachment/action-block fixture, not live Slack API delivery");

      await page.goto("/__mock_channel_preview/email");
      const emailLinks = page.getByRole("main", { name: "Email issue alert mock", exact: true }).getByRole("link");
      await expect(emailLinks).toHaveText(["Acknowledge", "Open timeline", "Investigate with DevOps Agent"]);
      await expect(emailLinks.nth(0)).toHaveAttribute("href", ackHref);
      await expect(emailLinks.nth(1)).toHaveAttribute("href", timelineHref);
      await expect(emailLinks.nth(2)).toHaveAttribute("href", chatTarget);
      expect(await emailLinks.nth(1).getAttribute("href")).not.toContain("&amp;");
      expect(await emailLinks.nth(2).getAttribute("href")).not.toContain("&amp;");
      await captureBreakState(page, `issue-alerts-email-mock-${viewport.name}-dark.png`, "dark", "F: synthetic Email template fixture, not live SMTP delivery");
      expect(errors).toEqual([]);
    });
  }

  test("Break timeline auto-detect guide desktop dark", async ({ page }) => {
    test.skip(process.env.E2E_KUBERNETES_AUTO_DETECT_DOCS !== "1", "opt-in documentation capture");
    await page.setViewportSize({ width: 1440, height: 1000 });
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    await openApp(page, "/agent/kubernetes?view=issues");
    const issues = page.getByRole("region", { name: "Kubernetes issues", exact: true });
    await expect(issues.getByRole("article")).toHaveCount(4);
    await expect(issues.getByLabel("Issue severity", { exact: true })).toBeVisible();
    const alertToggle = issues.getByRole("switch", { name: "Alert on new issues", exact: true });
    await expect(alertToggle).toBeInViewport({ ratio: 1 });
    await expect(alertToggle).toHaveAttribute("aria-checked", "false");
    await expect(issues.getByRole("group", { name: "Issue alert level", exact: true })).toHaveCount(0);
    await captureBreakState(page, "auto-detect-issues-overview.png", "dark");
    const checkout = issues.getByRole("article", { name: "Deployment checkout", exact: true });
    await checkout.getByRole("button", { name: "Expand checkout", exact: true }).click();
    await expect(checkout).toContainText("44 lifetime restarts");
    await expect(checkout).toContainText("OOMKilled on 6 pods");
    await expect(checkout).toContainText("v41 → v42");
    await expect(alertToggle).toBeInViewport({ ratio: 1 });
    await expect(alertToggle).toHaveAttribute("aria-checked", "false");
    await captureBreakState(page, "break-issues-expanded-desktop-dark.png", "dark");
    if (process.env.E2E_KUBERNETES_ALERT_DOCS === "1") {
      await checkout.getByRole("button", { name: "Collapse checkout", exact: true }).click();
      const saved = page.waitForResponse((response) => new URL(response.url()).pathname === "/api/admin/kubernetes/issue-alerts" && response.request().method() === "PUT");
      await alertToggle.click();
      const response = await saved;
      expect(response.ok()).toBe(true);
      expect(response.request().postDataJSON()).toEqual({ enabled: true, level: "critical" });
      await expect(alertToggle).toHaveAttribute("aria-checked", "true");
      const levels = issues.getByRole("group", { name: "Issue alert level", exact: true });
      await expect(levels.getByRole("button", { name: "Critical", exact: true })).toHaveAttribute("aria-pressed", "true");
      await expect(levels.getByRole("button", { name: "Warning", exact: true })).toHaveAttribute("aria-pressed", "false");
      await expect(levels).toBeInViewport({ ratio: 1 });
      await expect(issues.getByRole("article")).toHaveCount(4);
      await expect(page.getByText("Alerts on. Issues that break from now on will alert.", { exact: true })).toBeVisible();
      // Keep transient save feedback out of the reusable documentation bitmap.
      await expect(page.getByText("Alerts on. Issues that break from now on will alert.", { exact: true })).toBeHidden();
      await captureBreakState(page, "auto-detect-issue-alerts.png", "dark");
      expect(errors).toEqual([]);
      return;
    }
    await checkout.getByRole("button", { name: "Open resource checkout", exact: true }).click();
    await expect(page.getByRole("dialog", { name: "Details panel", exact: true })).toBeVisible();
    await page.getByRole("button", { name: "Close panel", exact: true }).click();
    await issues.getByLabel("Issue severity", { exact: true }).selectOption("critical");
    await expect(issues.getByRole("article")).toHaveCount(2);
    await issues.getByLabel("Issue severity", { exact: true }).selectOption("");
    await checkout.getByRole("button", { name: "Timeline for checkout", exact: true }).click();
    const lane = page.getByRole("region", { name: "Timeline lane checkout", exact: true });
    await expect(lane).toBeVisible();
    await expect(lane.locator('[data-marker="crash"]')).toHaveCount(2);
    await expect(lane.locator('[data-event-id="image"]')).toBeVisible();
    await expect(page.getByRole("region", { name: "Logs at break", exact: true }).getByRole("log")).toContainText("payment client timeout");
    await captureBreakState(page, "break-timeline-focus-desktop-dark.png", "dark");
    await page.getByRole("link", { name: "Clear root filter", exact: true }).click();
    const timeline = page.getByRole("region", { name: "Kubernetes change timeline", exact: true });
    await expect(timeline.getByRole("heading", { name: "Kubernetes changes", exact: true })).toBeVisible();
    await expect(timeline.getByRole("status", { name: "Loading projected change history", exact: true })).toHaveCount(0);
    await expect(timeline.locator('[data-event-id="image"]')).toBeVisible();
    await captureBreakState(page, "break-timeline-general-desktop-dark.png", "dark");
    await timeline.locator('[data-event-id="image"]').click();
    const comparison = timeline.getByRole("region", { name: "Timeline event details", exact: true });
    await expect(comparison).toContainText("v41");
    await expect(comparison).toContainText("v42");
    await captureBreakState(page, "auto-detect-change-compare.png", "dark");
    await page.keyboard.press("Escape");
    await page.getByRole("tab", { name: "Issues", exact: true }).click();
    await expect(checkout).toBeVisible();
    await checkout.getByRole("button", { name: "Investigate checkout", exact: true }).click();
    await expect(page).toHaveURL(/\/agent\/chat\?/);
    expect(new URL(page.url()).searchParams.get("uid")).toBe("checkout-uid");
    expect(errors).toEqual([]);
  });

  test("Break timeline full populated dark Overview documentation", async ({ page }) => {
    await page.setViewportSize({ width: 1440, height: 900 });
    await page.clock.install({ time: new Date("2026-10-15T12:00:00Z") });
    await page.emulateMedia({ colorScheme: "dark" });
    await openApp(page, "/agent/kubernetes?view=overview");
    await page.getByRole("button", { name: "Collapse sidebar", exact: true }).click();
    await expect(page.getByRole("button", { name: "Expand sidebar", exact: true })).toBeVisible();
    const timeline = page.getByRole("region", { name: "Break timeline", exact: true });
    await expect(timeline.getByRole("article")).toHaveCount(3);
    await expect(timeline).toContainText("OOMKilled on 6 pods");
    await expect(timeline).toContainText("v41 → v42");
    await expect(timeline).toContainText("ledger-api");
    await expect(timeline).toContainText("node-agent");
    const names = ["Cluster health", "Break timeline", "CPU capacity", "Memory capacity", "Triage summary", "Top pod usage", "Helm releases", "Overview topology", "Service traffic"];
    const cardFor = (name: string) => ["Cluster health", "Break timeline", "Triage summary", "Overview topology"].includes(name) ? page.getByRole("region", { name, exact: true }) : page.getByRole("heading", { name, exact: true }).locator("..").locator("..");
    const cards = names.map(cardFor);
    for (const card of cards) await expect(card).toBeVisible();
    await expect(page.getByText("Metrics available", { exact: true })).toBeVisible();
    await expect(cardFor("Top pod usage")).toContainText("shop/checkout-7f8c9-2kx4m");
    await expect(cardFor("Helm releases")).toContainText("Revision 42");
    await expect(cardFor("Helm releases").getByRole("listitem")).toHaveCount(3);
    await expect(cardFor("Service traffic")).toContainText("428.6 req/s");
    await expect(cardFor("Service traffic").getByRole("listitem")).toHaveCount(3);
    await expect(cardFor("CPU capacity")).toContainText("18.4 cores");
    await expect(cardFor("Memory capacity")).toContainText("72 GiB");
    await expect(cardFor("Service traffic")).not.toContainText("Stale");
    await expect(cardFor("Overview topology").getByRole("heading", { name: "Not reported", exact: true })).toHaveCount(0);
    await expect(cardFor("Overview topology").getByRole("definition")).toHaveCount(18);
    await expect(cardFor("Overview topology")).not.toContainText(/stale|partial|Workloads \(aggregate\)/i);
    await expect(page.getByRole("region", { name: "Triage summary", exact: true })).not.toContainText(/unavailable|not connected|No Helm|No service flows|Loading/i);
    const measurements = await fitOverviewCapture(page);
    await page.clock.runFor(1000);
    const resourceGeometry = await cardFor("Overview topology").locator("dl > div").evaluateAll((rows) => rows.map((row) => {
      const label = row.querySelector("dt")!;
      const value = row.querySelector("dd")!;
      const text = document.createRange();
      text.selectNodeContents(label);
      return { label: label.textContent, textRight: text.getBoundingClientRect().right, valueLeft: value.getBoundingClientRect().left };
    }));
    for (const resource of resourceGeometry) expect(resource.textRight, resource.label ?? "resource count").toBeLessThanOrEqual(resource.valueLeft - 1);
    const geometry = [];
    for (const [index, card] of cards.entries()) {
      const bounds = await card.boundingBox();
      expect(bounds).not.toBeNull();
      expect(bounds!.y).toBeGreaterThanOrEqual(0);
      expect(bounds!.y + bounds!.height).toBeLessThanOrEqual(page.viewportSize()!.height);
      geometry.push({ name: names[index], ...bounds });
    }
    await expect(page.getByRole("tab", { name: "Overview", exact: true })).toBeInViewport({ ratio: 1 });
    const proof = await captureBreakState(page, "overview-full-page-dark-20261015.png", "dark");
    const bitmap = fs.readFileSync(path.join(screenshotDir, "overview-full-page-dark-20261015.png"));
    expect(bitmap.readUInt32BE(16)).toBe(1440);
    expect(bitmap.readUInt32BE(20)).toBe(page.viewportSize()!.height);
    const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../../..");
    const sourceHashes = Object.fromEntries(["versus-incident/ui/tests/e2e/kubernetes-overview.spec.ts", "versus-incident/ui/tests/e2e/playwright.config.ts", "versus-incident/ui/src/pages/KubernetesPage.tsx", "versus-incident/ui/src/pages/KubernetesExplorerViews.tsx"].map((file) => [file, createHash("sha256").update(fs.readFileSync(path.join(root, file))).digest("hex")]));
    fs.writeFileSync(path.join(screenshotDir, "overview-full-page-dark-20261015.provenance.json"), JSON.stringify({ classification: "F: deterministic synthetic API-intercepted Enterprise-tier UI fixture; not live-cluster or license acceptance", spec: "tests/e2e/kubernetes-overview.spec.ts", config: "tests/e2e/playwright.config.ts", dimensions: page.viewportSize(), ...proof, sidebar: "collapsed using the existing Collapse sidebar control; unchanged CSS and fonts", sha256: createHash("sha256").update(bitmap).digest("hex"), sourceHashes, measurements, geometry, resourceGeometry, data: { brokenWorkloads: 3, checkoutImage: "v41 -> v42", oomKilled: 6, errorExit1: 1, nodes: 12, pods: 28, nodeMetrics: 12, podMetrics: 28, cpuUsage: "18.4 cores", memoryUsage: "72 GiB", topPods: 5, helmReleases: 3, indexedResourceKinds: 18, trafficRows: 3, trafficTier: "Enterprise synthetic fixture" } }, null, 2));
  });

  for (const status of [401, 403, 404]) {
    for (const phase of ["initial", "cached"] as const) {
      test(`Break timeline final edge ${phase} denial ${status} multi-cluster`, async ({ page }, testInfo) => {
        const reads: URL[] = [];
        page.on("request", (request) => { const url = new URL(request.url()); if (url.pathname.endsWith("/kubernetes/issues") && url.searchParams.has("root_uid")) reads.push(url); });
        await openApp(page, "/agent/kubernetes?view=issues&cluster=east");
        const row = page.getByRole("region", { name: "Kubernetes issues", exact: true }).getByRole("article", { name: "Deployment checkout", exact: true });
        const warnings = row.getByRole("button", { name: /^Warnings \(\d+\)$/ });
        await expect(warnings).toHaveText("Warnings (2)");
        await warnings.click();
        await expect(row.getByText("Secrets are not tracked.", { exact: true })).toBeVisible();
        expect(reads).toHaveLength(0);
        await row.getByRole("button", { name: "Expand checkout", exact: true }).click();
        if (phase === "cached") {
          await expect(row.getByText("v41 → v42", { exact: true })).toBeVisible();
          await expect(row.getByText(/History incomplete/)).toBeVisible();
          await page.getByRole("button", { name: "Refresh Kubernetes data", exact: true }).click();
        }
        await expect(row.getByRole("status")).toContainText("Root history unavailable.");
        if (status === 401) {
          await expect(page.getByRole("dialog", { name: "Session expired", exact: true })).toBeVisible();
          await page.keyboard.press("Escape");
          await expect(page.getByRole("dialog", { name: "Session expired", exact: true })).toHaveCount(0);
        }
        await row.getByRole("button", { name: "Collapse checkout", exact: true }).click();
        await expect(warnings).toHaveText("Warnings (3)");
        await expect(warnings).toHaveAttribute("aria-expanded", "false");
        if (status === 403 && phase === "cached") {
          fs.mkdirSync(screenshotDir, { recursive: true });
          await page.screenshot({ path: path.join(screenshotDir, "fixture-edge-cached-403-collapsed.png"), animations: "disabled" });
        }
        await warnings.click();
        await expect(row.getByText("Root history unavailable", { exact: true })).toBeVisible();
        await expect(row.getByText("Secrets are not tracked.", { exact: true })).toBeVisible();
        await expect(row).not.toContainText(/v41|v42|History incomplete|Rebuilt|Synthetic root lookup denied/);
        await expect(row.getByRole("button", { name: "Expand checkout", exact: true })).toHaveAttribute("aria-expanded", "false");
        if (status === 403 && phase === "cached") await page.screenshot({ path: path.join(screenshotDir, "fixture-edge-cached-403-expanded.png"), animations: "disabled" });
        await warnings.click();
        await expect(warnings).toHaveAttribute("aria-expanded", "false");
        expect(reads.every((url) => url.searchParams.get("cluster") === "east")).toBe(true);
        await page.getByRole("combobox", { name: "Cluster", exact: true }).selectOption("west");
        await page.getByRole("tab", { name: "Issues", exact: true }).click();
        await expect(row).not.toContainText(/v41|v42|History incomplete|Rebuilt/);
        await expect(warnings).toHaveAttribute("aria-expanded", "false");
        await row.getByRole("button", { name: "Expand checkout", exact: true }).click();
        await expect(row.getByRole("status")).toContainText("Root history unavailable.");
        if (status === 401) await page.keyboard.press("Escape");
        await expect(row).not.toContainText(/v41|v42|History incomplete|Rebuilt/);
        expect(reads.at(-1)?.searchParams.get("cluster")).toBe("west");
        await page.getByRole("combobox", { name: "Cluster", exact: true }).selectOption("east");
        await page.getByRole("tab", { name: "Issues", exact: true }).click();
        await expect(row).not.toContainText(/v41|v42|History incomplete|Rebuilt/);
        await expect(warnings).toHaveAttribute("aria-expanded", "false");
        await testInfo.attach("synthetic-denial-control", { body: JSON.stringify({ classification: "F", status, phase, root_reads: reads.map((url) => ({ cluster: url.searchParams.get("cluster"), root: url.searchParams.get("root_uid") })) }), contentType: "application/json" });
      });
    }
  }

  for (const scope of ["overview", "enrichment"] as const) {
    for (const state of ["stale", "partial", "unknown"] as const) {
      test(`Break timeline final edge ${scope} SourceSync ${state}`, async ({ page }, testInfo) => {
        await openApp(page, `/agent/kubernetes${scope === "enrichment" ? "?view=issues" : ""}`);
        const region = page.getByRole("region", { name: scope === "overview" ? "Break timeline" : "Kubernetes issues", exact: true });
        const row = region.getByRole("article", { name: "Deployment checkout", exact: true });
        if (scope === "enrichment") await row.getByRole("button", { name: "Expand checkout", exact: true }).click();
        await expect(row.getByText("No recorded change in available evidence", { exact: true })).toBeVisible();
        await expect(row).not.toContainText("No change on it, its config or its nodes");
        expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
        fs.mkdirSync(screenshotDir, { recursive: true });
        await row.scrollIntoViewIfNeeded();
        await page.screenshot({ path: path.join(screenshotDir, `fixture-edge-${scope}-${state}.png`), animations: "disabled" });
        await testInfo.attach("synthetic-source-state", { body: JSON.stringify({ classification: "F", scope, state, history: "complete", changes: 0 }), contentType: "application/json" });
      });
    }
  }

  for (const viewport of [{ name: "desktop", width: 1440, height: 900 }, { name: "mobile", width: 390, height: 844 }]) {
    test(`Break timeline crash evidence responsive disclosure ${viewport.name}`, async ({ page }) => {
      await page.setViewportSize(viewport);
      const errors: string[] = [];
      page.on("pageerror", (error) => errors.push(error.message));
      await openApp(page, "/agent/kubernetes?view=overview");
      const timeline = page.getByRole("region", { name: "Break timeline", exact: true });
      const row = timeline.getByRole("article", { name: "Deployment checkout", exact: true });
      await expect(row.getByText("Error exit 137 (exit hint: SIGKILL) on 2 pods", { exact: true })).toBeVisible();
      await expect(row.getByText("Error exit 137 signal SIGKILL on 1 pod", { exact: true })).toBeVisible();
      await expect(row.getByText("44 lifetime restarts", { exact: true })).toBeVisible();
      await expect(row.getByText("12 warning events: FailedScheduling 8, Other 4", { exact: true })).toBeVisible();
      await expect(row.getByRole("status")).toHaveText("Partial crash evidence");
      const node = row.getByText("Node MemoryPressure on 3 pods", { exact: true });
      if (viewport.name === "mobile") {
        await expect(node).not.toBeVisible();
        const toggle = row.getByRole("button", { name: /^(?:\+5 more|Show less) evidence for checkout$/ });
        await toggle.focus();
        await page.keyboard.press("Enter");
        await expect(toggle).toHaveAttribute("aria-expanded", "true");
        await expect(toggle).toBeFocused();
      }
      await expect(node).toBeVisible();
      await expect(row).not.toContainText("·");
      expect(await row.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(true);
      await captureBreakState(page, `crash-evidence-overview-${viewport.name}.png`, "dark");
      await page.getByRole("tab", { name: "Issues", exact: true }).click();
      const issues = page.getByRole("region", { name: "Kubernetes issues", exact: true });
      const issueRow = issues.getByRole("article", { name: "Deployment checkout", exact: true });
      await expect(issueRow.getByRole("button", { name: "Expand checkout", exact: true })).toHaveAttribute("aria-expanded", "false");
      if (viewport.name === "mobile") {
        const more = issueRow.getByRole("button", { name: /^(?:\+5 more|Show less) evidence for checkout$/ });
        await more.focus();
        await page.keyboard.press("Enter");
        await expect(more).toHaveAttribute("aria-expanded", "true");
        await expect(more).toBeFocused();
      } else await issueRow.getByRole("button", { name: "Expand checkout", exact: true }).click();
      await expect(issueRow.getByRole("button", { name: "Collapse checkout", exact: true })).toHaveAttribute("aria-expanded", "true");
      await expect(issueRow.getByText("Node MemoryPressure on 3 pods", { exact: true })).toBeVisible();
      expect(await issueRow.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(true);
      await captureBreakState(page, `crash-evidence-issues-${viewport.name}.png`, "dark");
      expect(errors).toEqual([]);
    });

    test(`Break timeline captured journal fixture all retained observations ${viewport.name} dark`, async ({ page }, testInfo) => {
      await page.setViewportSize(viewport);
      await page.emulateMedia({ colorScheme: "dark" });
      const reads: URL[] = [];
      const errors: string[] = [];
      page.on("pageerror", (error) => errors.push(error.message));
      page.on("request", (request) => { const url = new URL(request.url()); if (url.pathname.endsWith("/kubernetes/crashes")) reads.push(url); });
      await openApp(page, "/agent/kubernetes?view=timeline&root_uid=checkout-uid&namespace=shop");
      const journal = page.getByRole("region", { name: "Captured crash evidence", exact: true });
      const list = journal.getByRole("list", { name: "Crash observations", exact: true });
      await expect(list.getByRole("listitem")).toHaveCount(50);
      expect(reads).toHaveLength(1);
      expect(Object.fromEntries(reads[0].searchParams)).toMatchObject({ root_uid: "checkout-uid", namespace: "shop", limit: "50" });
      await expect(page.getByText("Partial crash evidence", { exact: true })).toHaveCount(0);
      await expect(journal).toContainText("Events between polls may be unseen");
      await expect(journal).toContainText("Journal retention/capture gap:");
      await expect(journal).toContainText("Capture gap:");
      const chart = page.getByRole("region", { name: "Resource timeline chart", exact: true });
      await expect(chart.locator('[data-marker="crash"]')).toHaveCount(40);
      const marker = chart.locator('[data-marker="crash"]').first();
      await marker.focus();
      await page.keyboard.press("Enter");
      const details = page.getByRole("region", { name: "Timeline event details", exact: true });
      await expect(details).toContainText("removed-pod-49");
      await expect(details).toContainText("exit 137");
      await page.keyboard.press("Escape");
      await expect(marker).toBeFocused();
      const disclosure = list.getByRole("listitem").nth(1).locator("summary");
      await disclosure.focus();
      await page.keyboard.press("Enter");
      await expect(list.getByRole("listitem").nth(1)).toContainText("not proof of SIGKILL");
      await expect(list.getByRole("listitem").nth(1)).toContainText("Pod UID: removed-pod-uid-1");
      await expect(list.getByRole("listitem").nth(1)).not.toContainText(", signal 9");
      await journal.scrollIntoViewIfNeeded();
      await disclosure.scrollIntoViewIfNeeded();
      expect(await journal.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(true);
      await captureBreakState(page, `crash-journal-fixture-details-${viewport.name}-dark.png`, "dark");
      await journal.getByRole("button", { name: "Next crash page", exact: true }).click();
      await expect(journal.getByRole("alert")).toContainText("Crash journal unavailable");
      await expect(list).toHaveCount(0);
      await expect(chart.locator('[data-marker="crash"]')).toHaveCount(0);
      await journal.getByRole("button", { name: "Retry crash journal", exact: true }).click();
      await expect(list.getByRole("listitem")).toHaveCount(50);
      await expect(list).toContainText("removed-pod-99");
      await journal.getByRole("button", { name: "Next crash page", exact: true }).click();
      await expect(list.getByRole("listitem")).toHaveCount(5);
      await expect(list).toContainText("removed-pod-104");
      await expect(journal.getByRole("button", { name: "Next crash page", exact: true })).toBeDisabled();
      await list.getByRole("listitem").last().locator("summary").click();
      await expect(list.getByRole("listitem").last()).toContainText("Root UID: checkout-uid");
      await journal.scrollIntoViewIfNeeded();
      await captureBreakState(page, `crash-journal-fixture-last-page-${viewport.name}-dark.png`, "dark");
      await journal.getByRole("button", { name: "Previous crash page", exact: true }).click();
      await expect(list.getByRole("listitem")).toHaveCount(50);
      await expect(list).toContainText("removed-pod-50");
      await journal.getByRole("button", { name: "Previous crash page", exact: true }).click();
      await expect(list).toContainText("removed-pod-0");
      await journal.getByRole("button", { name: "Next crash page", exact: true }).click();
      await expect(list).toContainText("removed-pod-50");
      await journal.getByRole("button", { name: "Next crash page", exact: true }).click();
      await expect(list.getByRole("listitem")).toHaveCount(5);
      await expect(list).toContainText("removed-pod-104");
      expect(reads.map((url) => url.searchParams.get("cursor"))).toEqual([null, "journal-50", "journal-50", "journal-100"]);
      await page.getByRole("textbox", { name: "Timeline namespace", exact: true }).fill("other");
      await expect(journal).toContainText("No captured observations on this page");
      await expect(journal.getByRole("button", { name: "Previous crash page", exact: true })).toBeDisabled();
      expect(reads.at(-1)!.searchParams.get("cursor")).toBeNull();
      expect(errors).toEqual([]);
      await testInfo.attach("journal-fixture-provenance", { body: JSON.stringify({ classification: "F: synthetic API-intercepted UI fixture; not live backend integration", viewport, pages: [50, 50, 5], continuationRetry: true }), contentType: "application/json" });
    });

    test(`Break timeline K12a crash evidence five-break matrix and markers ${viewport.name}`, async ({ page }) => {
      await page.setViewportSize(viewport);
      await page.emulateMedia({ colorScheme: "dark" });
      const errors: string[] = [];
      page.on("pageerror", (error) => errors.push(error.message));
      await openApp(page, "/agent/kubernetes?view=overview");
      const timeline = page.getByRole("region", { name: "Break timeline", exact: true });
      await expect(timeline.getByRole("article")).toHaveCount(5);
      await expect(timeline.getByRole("button", { name: /^Broken / })).toContainText("5");
      await expect(timeline.getByRole("button", { name: /^Broken / })).toContainText("4 critical");
      await expect(timeline.getByRole("button", { name: /^Broken / })).toContainText("1 warning");
      await expect(timeline).not.toContainText("·");

      const nodesRow = timeline.getByRole("article", { name: "Deployment nodes-api", exact: true });
      const configRow = timeline.getByRole("article", { name: "Deployment config-api", exact: true });
      const schedRow = timeline.getByRole("article", { name: "Deployment sched-api", exact: true });
      const evictRow = timeline.getByRole("article", { name: "Deployment evict-api", exact: true });
      const batchRow = timeline.getByRole("article", { name: "Job batch-job", exact: true });

      await expect(nodesRow.getByLabel("CrashLoopBackOff", { exact: true })).toBeVisible();
      await expect(nodesRow).toContainText("2 of 3 pods affected");
      await expect(nodesRow.getByText("OOMKilled signal SIGKILL on 2 pods", { exact: true })).toBeVisible();
      await expect(nodesRow.getByText("Error exit 137 (exit hint: SIGKILL) on 1 pod", { exact: true })).toBeVisible();
      await expect(nodesRow.getByText("Init Error exit 1 signal 31 on 1 pod", { exact: true })).toBeVisible();
      await expect(nodesRow.getByText("44 lifetime restarts", { exact: true })).toBeVisible();
      await expect(nodesRow.getByText("12 warning events: BackOff 8, Other 4", { exact: true })).toBeVisible();
      await expect(nodesRow.getByRole("status")).toHaveText("Partial crash evidence");

      await expect(configRow.getByLabel("Container config error", { exact: true })).toBeVisible();
      await expect(configRow.getByText("CreateContainerConfigError on 2 pods", { exact: true })).toBeVisible();
      await expect(configRow.getByText("Init PostStartHookError on 1 pod", { exact: true })).toBeVisible();
      await expect(configRow.getByText("4 warning events: Failed 4", { exact: true })).toBeVisible();

      await expect(schedRow.getByLabel("Unschedulable", { exact: true })).toBeVisible();
      const unschedPill = schedRow.getByText("Unschedulable on 2 pods", { exact: true });
      await expect(unschedPill).toBeVisible();
      await expect(unschedPill).toHaveClass(/pill-warn/);
      await expect(schedRow.getByText("8 warning events: FailedScheduling 8", { exact: true })).toBeVisible();

      await expect(evictRow.getByLabel("Evicted", { exact: true })).toBeVisible();
      await expect(evictRow.getByText("Error exit 126 (exit hint: not executable) on 1 pod", { exact: true })).toBeVisible();
      await expect(evictRow.getByText("Init Error exit 127 (exit hint: command not found) on 1 pod", { exact: true })).toBeVisible();
      await expect(evictRow.getByText("Evicted on 3 pods", { exact: true })).toBeVisible();
      await expect(evictRow.getByText("3 lifetime restarts", { exact: true })).toBeVisible();
      await expect(evictRow.getByText("5 warning events: Evicted 4, Other 1", { exact: true })).toBeVisible();

      await expect(batchRow.getByLabel("Job failed", { exact: true })).toBeVisible();
      await expect(batchRow.getByText("Error exit 134 (exit hint: SIGABRT) on 1 pod", { exact: true })).toBeVisible();
      await expect(batchRow.getByText("Error exit 139 (exit hint: SIGSEGV) on 1 pod", { exact: true })).toBeVisible();
      await expect(batchRow.getByText("Error exit 143 (exit hint: SIGTERM) on 1 pod", { exact: true })).toBeVisible();
      await expect(batchRow.getByText("4 warning events: BackoffLimitExceeded 4", { exact: true })).toBeVisible();

      if (viewport.name === "mobile") {
        await expect(nodesRow.getByText("Node MemoryPressure on 3 pods", { exact: true })).not.toBeVisible();
        await expect(evictRow.getByText("OutOfResource on 1 pod", { exact: true })).not.toBeVisible();
        await expect(batchRow.getByText("BackoffLimitExceeded", { exact: true })).not.toBeVisible();
        const toggle = nodesRow.getByRole("button", { name: "+3 more evidence for nodes-api", exact: true });
        await toggle.focus();
        await page.keyboard.press("Enter");
        const less = nodesRow.getByRole("button", { name: "Show less evidence for nodes-api", exact: true });
        await expect(less).toHaveAttribute("aria-expanded", "true");
        await expect(less).toBeFocused();
        await expect(nodesRow.getByText("CrashLoopBackOff on 1 pod", { exact: true })).toBeVisible();
        await expect(nodesRow.getByText("Init CrashLoopBackOff on 1 pod", { exact: true })).toBeVisible();
        await expect(nodesRow.getByText("Node MemoryPressure on 3 pods", { exact: true })).toBeVisible();
        await captureBreakState(page, "k12a-fixture-overview-mobile-expanded.png", "dark");
        await page.keyboard.press("Space");
        await expect(nodesRow.getByRole("button", { name: "+3 more evidence for nodes-api", exact: true })).toHaveAttribute("aria-expanded", "false");
        await expect(nodesRow.getByText("Node MemoryPressure on 3 pods", { exact: true })).not.toBeVisible();
      } else {
        await expect(nodesRow.getByText("CrashLoopBackOff on 1 pod", { exact: true })).toBeVisible();
        await expect(nodesRow.getByText("Init CrashLoopBackOff on 1 pod", { exact: true })).toBeVisible();
        await expect(nodesRow.getByText("Node MemoryPressure on 3 pods", { exact: true })).toBeVisible();
        await expect(evictRow.getByText("OutOfResource on 1 pod", { exact: true })).toBeVisible();
        await expect(batchRow.getByText("BackoffLimitExceeded", { exact: true })).toBeVisible();
      }
      for (const row of [nodesRow, configRow, schedRow, evictRow, batchRow]) {
        expect(await row.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(true);
      }
      await captureBreakState(page, `k12a-fixture-overview-${viewport.name}.png`, "dark");

      await page.getByRole("tab", { name: "Issues", exact: true }).click();
      const issues = page.getByRole("region", { name: "Kubernetes issues", exact: true });
      const issueNodes = issues.getByRole("article", { name: "Deployment nodes-api", exact: true });
      await expect(issueNodes.getByRole("button", { name: "Expand nodes-api", exact: true })).toHaveAttribute("aria-expanded", "false");
      if (viewport.name === "mobile") {
        const more = issueNodes.getByRole("button", { name: "+3 more evidence for nodes-api", exact: true });
        await more.focus();
        await page.keyboard.press("Enter");
        await expect(issueNodes.getByRole("button", { name: "Show less evidence for nodes-api", exact: true })).toHaveAttribute("aria-expanded", "true");
        await expect(issueNodes.getByRole("button", { name: "Show less evidence for nodes-api", exact: true })).toBeFocused();
      } else {
        await issueNodes.getByRole("button", { name: "Expand nodes-api", exact: true }).click();
      }
      await expect(issueNodes.getByRole("button", { name: "Collapse nodes-api", exact: true })).toHaveAttribute("aria-expanded", "true");
      await expect(issueNodes.getByText("Node MemoryPressure on 3 pods", { exact: true })).toBeVisible();
      expect(await issueNodes.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(true);
      await captureBreakState(page, `k12a-fixture-issues-${viewport.name}.png`, "dark");
      if (viewport.name === "mobile") {
        await issues.getByRole("combobox", { name: "Issue severity", exact: true }).selectOption("critical");
        await expect(issueNodes.getByRole("button", { name: "+3 more evidence for nodes-api", exact: true })).toHaveAttribute("aria-expanded", "false");
        await issues.getByRole("combobox", { name: "Issue severity", exact: true }).selectOption("");
      }

      await issueNodes.getByRole("button", { name: "Timeline for nodes-api", exact: true }).click();
      const lane = page.getByRole("region", { name: "Timeline lane nodes-api", exact: true });
      await expect(lane).toBeVisible();
      const chart = lane.getByRole("region", { name: "Resource timeline chart", exact: true });
      await expect(chart.locator('[data-marker="crash"]')).toHaveCount(3);
      const markerIDs = await chart.locator('[data-marker="crash"]').evaluateAll((elements) => elements.map((element) => element.getAttribute("data-event-id")));
      expect(new Set(markerIDs).size).toBe(3);
      await expect(lane.getByText("Partial crash evidence", { exact: true })).toHaveCount(0);
      const crashLabel = chart.locator(".resource-timeline-crash-label").first();
      await expect(crashLabel).toContainText("api: OOMKilled");
      await expect(crashLabel).toHaveAttribute("title", /exit 137, signal 9/);
      await crashLabel.focus();
      await page.keyboard.press("Enter");
      const details = lane.getByRole("region", { name: "Timeline event details", exact: true });
      await expect(details.getByRole("heading", { name: "Timeline event details", exact: true })).toBeFocused();
      await expect(details).toContainText("OOMKilled, exit 137, signal 9");
      await expect(details).toContainText("not proof of SIGKILL");
      await expect(details).toContainText("Init container api: Error, exit 1, signal 31");
      await expect(details).not.toContainText("·");
      await captureBreakState(page, `k12a-fixture-timeline-${viewport.name}.png`, "dark");
      await page.keyboard.press("Escape");
      await expect(details).toHaveCount(0);
      await expect(crashLabel).toBeFocused();
      expect(errors).toEqual([]);
    });

    test(`Break timeline selected marker removal restores chart focus ${viewport.name} dark`, async ({ page }) => {
      const warnings: string[] = [];
      page.on("console", message => { if (message.type() === "error" && message.text().includes("Cannot update a component")) warnings.push(message.text()); });
      await page.setViewportSize(viewport);
      await openApp(page, "/agent/kubernetes?view=timeline&root_uid=checkout-uid&namespace=shop");
      const chart = page.getByRole("region", { name: "Resource timeline chart", exact: true });
      const marker = chart.locator('[data-marker="change"]').first();
      await marker.focus();
      await page.keyboard.press("Enter");
      const details = page.getByRole("region", { name: "Timeline event details", exact: true });
      await expect(details.getByRole("heading", { name: "Timeline event details", exact: true })).toBeFocused();
      await page.route("**/api/admin/kubernetes/changes*", (route) => route.fulfill({ json: { items: [], sync: { state: "ready", partial: false, age_s: 0 } } }));
      await page.getByRole("button", { name: "Refresh Kubernetes data", exact: true }).click();
      await expect(details).toHaveCount(0);
      await expect(chart.locator('[data-marker="change"]')).toHaveCount(0);
      await expect(chart).toBeFocused();
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
      expect(warnings).toEqual([]);
    });
    test(`Break timeline external controls retain focus across filters and polling ${viewport.name} dark`, async ({ page }) => {
      const warnings: string[] = [];
      page.on("console", message => { if (message.type() === "error" && message.text().includes("Cannot update a component")) warnings.push(message.text()); });
      await page.setViewportSize(viewport);
      await page.clock.install({ time: new Date() });
      await openApp(page, "/agent/kubernetes?view=timeline&root_uid=checkout-uid&namespace=shop");
      const chart = page.getByRole("region", { name: "Resource timeline chart", exact: true });
      const marker = chart.locator('[data-marker="change"]').first();
      const details = page.getByRole("region", { name: "Timeline event details", exact: true });
      const range = page.getByRole("group", { name: "Timeline window", exact: true }).getByRole("button", { name: "15m", exact: true });
      await marker.focus();
      await page.keyboard.press("Enter");
      await expect(details.getByRole("heading", { name: "Timeline event details", exact: true })).toBeFocused();
      await range.focus();
      await page.keyboard.press("Enter");
      await expect(range).toHaveAttribute("aria-pressed", "true");
      await expect(range).toBeFocused();
      await expect(details).toHaveCount(0);
      await marker.focus();
      await page.keyboard.press("Enter");
      const refresh = page.getByRole("button", { name: "Refresh Kubernetes data", exact: true });
      await refresh.focus();
      const previousUntil = await chart.getAttribute("data-until");
      await page.route("**/api/admin/kubernetes/changes*", (route) => route.fulfill({ json: { items: [], sync: { state: "ready", partial: false, age_s: 0 } } }));
      await page.keyboard.press("Enter");
      await expect(details).toHaveCount(0);
      await expect(chart.locator('[data-marker="change"]')).toHaveCount(0);
      await expect(refresh).toBeFocused();
      await page.clock.fastForward(30_000);
      // Root investigations pin their range; polling must not move that bracket.
      await expect(chart).toHaveAttribute("data-until", previousUntil!);
      await expect(chart.locator('[data-marker="change"]')).toHaveCount(0);
      await expect(details).toHaveCount(0);
      await expect(refresh).toBeFocused();

      // Namespace/kind filters now belong to the unfocused paginated list.
      let removed = false;
      const reads: URL[] = [];
      await page.route("**/api/admin/kubernetes/timeline*", route => {
        const url = new URL(route.request().url());
        reads.push(url);
        const entry = { type: "change", id: "focus-change", at: new Date().toISOString(), change: { id: "focus-change", at: new Date().toISOString(), cluster: "fixture", kind: "Deployment", namespace: "shop", name: "checkout", uid: "checkout-uid", type: "image_changed" } };
        const items = removed || url.searchParams.get("namespace") === "payments" ? [] : [entry];
        return route.fulfill({ json: { items, total: items.length, counts: { change: items.length, crash: 0, break: 0 }, crash_count: 0, since: url.searchParams.get("since"), until: url.searchParams.get("until"), gaps: [], sync: { state: "ready", partial: false, age_s: 0 } } });
      });
      await page.getByRole("link", { name: "Clear root filter", exact: true }).click();
      const list = page.getByRole("list", { name: "Breaks and changes", exact: true });
      await expect(list.getByRole("listitem")).toHaveCount(1);
      const namespace = page.getByRole("combobox", { name: "Timeline namespace", exact: true });
      await namespace.focus();
      await namespace.fill("payments");
      await expect(page.getByRole("option", { name: "payments", exact: true })).toBeVisible();
      await page.keyboard.press("Enter");
      await expect(list.getByRole("listitem")).toHaveCount(0);
      await expect(namespace).toBeFocused();
      await namespace.click();
      await namespace.fill("shop");
      await expect(page.getByRole("option", { name: "shop", exact: true })).toBeVisible();
      await page.keyboard.press("Enter");
      await expect(list.getByRole("listitem")).toHaveCount(1);
      const kind = page.getByRole("combobox", { name: "Timeline kind", exact: true });
      await kind.focus();
      await kind.selectOption("Deployment");
      await expect(kind).toBeFocused();
      await expect.poll(() => reads.at(-1)?.searchParams.get("kind")).toBe("Deployment");
      await namespace.focus();
      removed = true;
      const listUntil = reads.at(-1)?.searchParams.get("until");
      await page.clock.fastForward(30_000);
      await expect.poll(() => reads.at(-1)?.searchParams.get("until")).not.toBe(listUntil);
      await expect(list.getByRole("listitem")).toHaveCount(0);
      await expect(namespace).toBeFocused();
      expect(warnings).toEqual([]);
    });
    test(`Break timeline label and stack geometry ${viewport.name} dark`, async ({ page }) => {
      await page.setViewportSize(viewport);
      await page.emulateMedia({ colorScheme: "dark" });
      await openApp(page, "/agent/kubernetes");
      await page.getByRole("button", { name: "Timeline for checkout", exact: true }).click();
      const chart = page.getByRole("region", { name: "Resource timeline chart", exact: true });
      await expect(chart.locator('[data-event-id="rollback"]')).toBeAttached();
      const geometry = await chart.evaluate((element) => {
        const row = element.querySelector<HTMLElement>('[data-resource*=checkout-uid]')!;
        const rowBounds = row.getBoundingClientRect();
        const crash = row.querySelector<HTMLElement>(".resource-timeline-crash-label")!;
        const rollback = [...row.querySelectorAll<HTMLElement>(".resource-timeline-event-label")].find((label) => label.textContent?.includes("v42 → v41"))!;
        const crashBounds = crash.getBoundingClientRect();
        const rollbackBounds = rollback.getBoundingClientRect();
        return { crash: { left: crashBounds.left, right: crashBounds.right, width: crashBounds.width, text: crash.textContent, title: crash.title }, rollback: { left: rollbackBounds.left, width: rollbackBounds.width }, markers: [...row.querySelectorAll<HTMLElement>('[data-event-id^="image"]')].map((marker) => { const bounds = marker.getBoundingClientRect(); return { top: bounds.top - rowBounds.top, bottom: bounds.bottom - rowBounds.top, outline: getComputedStyle(marker).outlineOffset }; }), height: rowBounds.height };
      });
      expect(geometry.crash.width).toBeGreaterThan(0);
      expect(geometry.rollback.width).toBeGreaterThan(0);
      expect(geometry.crash.right).toBeLessThanOrEqual(geometry.rollback.left);
      expect(geometry.crash.text).toContain("api: OOMKilled");
      expect(geometry.crash.title).toContain("previous termination");
      expect(geometry.height).toBe(40);
      expect(geometry.markers).toHaveLength(3);
      for (const marker of geometry.markers) { expect(marker.top).toBeGreaterThanOrEqual(0); expect(marker.bottom).toBeLessThanOrEqual(40); }
      const third = chart.locator('[data-event-id="image-stack-2"]');
      await page.keyboard.press("Tab");
      await third.focus();
      await expect(third).toBeFocused();
      expect(await third.evaluate((element) => getComputedStyle(element).outlineOffset)).toBe("-2px");
      await third.click();
      await expect(page.getByRole("region", { name: "Timeline event details", exact: true }).getByRole("button", { name: "Open resource", exact: true })).toHaveCount(3);
      await page.keyboard.press("Escape");
      await expect(third).toBeFocused();
      await captureBreakState(page, `break-label-stack-${viewport.name}-dark.png`, "dark");
    });
    test(`Break timeline fleet documentation ${viewport.name} dark multi-cluster`, async ({ page }) => {
      await page.setViewportSize(viewport);
      await page.addInitScript(() => localStorage.setItem("versus.theme", "dark"));
      await openApp(page, "/agent/kubernetes");
      const rows = page.getByRole("region", { name: "Cluster rows", exact: true });
      await expect(rows.getByRole("article")).toHaveCount(2);
      const east = rows.getByRole("article", { name: "Cluster east", exact: true });
      const west = rows.getByRole("article", { name: "Cluster west", exact: true });
      await expect(east).toContainText("Last change 9 min ago");
      await expect(west).not.toContainText(/Last change|Unknown/);
      await expect(east).toContainText("26 warnings");
      await expect(east).toContainText("3 broken");
      await expect(east.getByRole("meter", { name: "Ready nodes", exact: true })).toHaveAttribute("value", "12");
      await expect(east.getByRole("meter", { name: "Running pods", exact: true })).toHaveAttribute("value", "19");
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
      expect(await rows.evaluate((element) => element.scrollWidth > element.clientWidth)).toBe(viewport.name === "mobile");
      fs.mkdirSync(screenshotDir, { recursive: true });
      await captureBreakState(page, `break-fleet-${viewport.name}-dark.png`, "dark");
      if (viewport.name === "mobile") {
        await rows.evaluate((element) => { element.scrollLeft = element.scrollWidth; });
        await expect(east.getByRole("link", { name: "Review issues", exact: true })).toBeInViewport();
        await captureBreakState(page, "break-fleet-mobile-signals-dark.png", "dark");
      }
    });
    for (const theme of ["dark", "light"] as const) {
      for (const cluster of [undefined, "east"] as const) {
      test(`Break timeline overview and scoped interactions ${viewport.name} ${theme}${cluster ? " multi-cluster" : ""}`, async ({ page }) => {
        await page.setViewportSize(viewport);
        await page.clock.install();
        await page.addInitScript((value) => localStorage.setItem("versus.theme", value), theme);
        const reads: URL[] = [];
        const errors: string[] = [];
        page.on("request", (request) => { const url = new URL(request.url()); if (url.pathname.startsWith("/api/admin/kubernetes/")) reads.push(url); });
        page.on("pageerror", (error) => errors.push(error.message));
        await openApp(page, `/agent/kubernetes${cluster ? `?cluster=${cluster}` : ""}`);
        const timeline = page.getByRole("region", { name: "Break timeline", exact: true });
        await expect(timeline.getByRole("article")).toHaveCount(3);
        await expect(timeline).toContainText("OOMKilled on 6 pods");
        await expect(timeline.getByText(/History incomplete/)).not.toBeVisible();
        await expect(timeline.getByText("Secrets are not tracked.", { exact: true })).not.toBeVisible();
        for (const toggle of await timeline.getByRole("button", { name: /Warnings/ }).all()) await expect(toggle).toHaveAttribute("aria-expanded", "false");
        await expect(page.getByRole("heading", { name: "Recent changes (60m)", exact: true })).toHaveCount(0);
        await expect(page.getByRole("region", { name: "Cluster health", exact: true }).getByRole("article")).toHaveCount(0);
        await expect(timeline).not.toContainText(/same_object|scaled_by|used_by|observed|reconstructed|·/);
        for (const row of await timeline.getByRole("article").all()) {
          const box = await row.boundingBox();
          expect(box).not.toBeNull();
          expect(box!.y + box!.height, "all three complete root rows fit without scrolling").toBeLessThanOrEqual(viewport.height);
        }
        expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
        const checkout = timeline.getByRole("article", { name: "Deployment checkout", exact: true });
        await expect(checkout).toContainText("6 → 7");
        if (viewport.name === "mobile") {
          await expect(checkout.locator("time").first()).toBeVisible();
          await expect(checkout.getByText("v41 → v42", { exact: true })).not.toBeVisible();
          await expect(checkout.getByRole("button", { name: "+2 more changes", exact: true })).toBeVisible();
        } else await expect(checkout.getByText("v41 → v42", { exact: true })).toBeVisible();
        fs.mkdirSync(screenshotDir, { recursive: true });
        await captureBreakState(page, `break-timeline-${viewport.name}-${theme}.png`, theme);
        const ledger = timeline.getByRole("article", { name: "Deployment ledger-api", exact: true });
        const warningToggle = ledger.getByRole("button", { name: "Warnings (1)", exact: true });
        await warningToggle.focus();
        await page.keyboard.press("Enter");
        await expect(warningToggle).toHaveAttribute("aria-expanded", "true");
        await expect(ledger.getByText(/History incomplete/)).toBeVisible();
        await page.keyboard.press("Space");
        await expect(warningToggle).toHaveAttribute("aria-expanded", "false");
        await checkout.getByRole("button", { name: "Timeline for checkout", exact: true }).click();
        await expect(page.getByRole("region", { name: "Timeline lane checkout", exact: true })).toBeVisible();
        await expect(page).toHaveURL(/root_uid=checkout-uid/);
        const focused = new URL(page.url());
        const center = Date.parse(focused.searchParams.get("center")!);
        const lane = page.getByRole("region", { name: "Timeline lane checkout", exact: true });
        await expect(lane.locator(".resource-timeline-label")).toHaveText(["Deployment checkoutshop", "ConfigMap checkout-cfgshop", "HPA checkout-hpashop", "Log linesper minute, errors in red"]);
        await expect(lane.getByRole("button", { name: /HorizontalPodAutoscaler checkout-hpa: 6 → 7 at/ })).toHaveCount(1);
        await expect(lane.getByRole("button", { name: /ConfigMap checkout-cfg:.* at/ })).toHaveCount(1);
        await expect(lane).toContainText("Break: CrashLoopBackOff");
        await expect(lane.locator('[data-marker="crash"]').filter({ hasText: "×" }).first()).toHaveAttribute("aria-label", /container api: OOMKilled, exit 137/);
        await expect(lane.locator('[data-marker="crash"]')).toHaveCount(2);
        const journal = page.getByRole("region", { name: "Captured crash evidence", exact: true });
        await expect(journal.getByRole("list", { name: "Crash observations", exact: true }).getByRole("listitem")).toHaveCount(2);
        await expect(journal.getByRole("button", { name: "Next crash page", exact: true })).toBeDisabled();
        await expect(lane.getByText("Partial crash evidence", { exact: true })).toHaveCount(0);
        await expect.poll(() => reads.some((url) => url.pathname.endsWith("/changes") && Date.parse(url.searchParams.get("since") ?? "") === center - 30 * 60_000 && Date.parse(url.searchParams.get("until") ?? "") === center + 30 * 60_000)).toBe(true);
        const markers = await lane.locator("[data-marker]").evaluateAll((elements) => elements.map((element) => ({ at: element.getAttribute("data-at"), x: Number(element.getAttribute("data-x")) })));
        for (const marker of markers) expect(marker.x).toBeCloseTo((Date.parse(marker.at!) - (center - 30 * 60_000)) / (60 * 60_000) * 100, 6);
        const chart = lane.getByRole("region", { name: "Resource timeline chart", exact: true });
        expect(await chart.evaluate((element) => element.scrollWidth > element.clientWidth)).toBe(viewport.name === "mobile");
        await expect(lane.locator(".resource-timeline-break-pill")).toContainText("break");
        if (viewport.name === "mobile") {
          const framing = await chart.evaluate((element) => {
            const viewport = element.getBoundingClientRect();
            const label = element.querySelector<HTMLElement>(".resource-timeline-label")!.getBoundingClientRect();
            const pill = element.querySelector<HTMLElement>(".resource-timeline-break-pill")!.getBoundingClientRect();
            const line = element.querySelector<HTMLElement>(".resource-timeline-break-line")!.getBoundingClientRect();
            const markers = [...element.querySelectorAll<HTMLElement>('[data-marker="change"]')].map((marker) => marker.getBoundingClientRect());
            return { labelWidth: label.width, breakVisible: line.left >= label.right && line.left <= viewport.right, blueVisible: markers.some((marker) => marker.left >= label.right && marker.right <= viewport.right), pillVisible: pill.left >= label.right && pill.right <= viewport.right, left: element.scrollLeft };
          });
          expect(framing.labelWidth).toBe(132);
          expect(framing.breakVisible).toBe(true);
          expect(framing.blueVisible).toBe(true);
          expect(framing.pillVisible).toBe(true);
          expect(framing.left).toBeGreaterThan(0);
        }
        await captureBreakState(page, `break-timeline-focus-${viewport.name}-${theme}.png`, theme);
        const crash = lane.locator('[data-marker="crash"]').first();
        await crash.focus();
        await page.keyboard.press("Enter");
        const details = lane.getByRole("region", { name: "Timeline event details", exact: true });
        await expect(details.getByRole("heading", { name: "Timeline event details", exact: true })).toBeFocused();
        await expect(details).toContainText("container api: OOMKilled, exit 137");
        await expect(details).toContainText("container api: Error, exit 1");
        await page.keyboard.press("Escape");
        await expect(details).toHaveCount(0);
        await expect(crash).toBeFocused();
        await page.keyboard.press("Space");
        await details.getByRole("button", { name: "Close event details", exact: true }).click();
        await expect(crash).toBeFocused();
        if (viewport.name === "mobile") {
          const panBefore = await chart.evaluate((element) => element.scrollLeft);
          await chart.evaluate((element) => { element.scrollLeft = 0; });
          await expect.poll(() => chart.evaluate((element) => element.scrollLeft)).toBe(0);
          await expect(lane.getByText("ConfigMap checkout-cfg", { exact: true })).toBeInViewport();
          expect(panBefore).toBeGreaterThan(0);
          expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
        }
        await page.getByRole("link", { name: "Clear root filter", exact: true }).click();
        await expect(page).not.toHaveURL(/root_uid=/);
        await expect(lane).toHaveCount(0);
        const unfocused = page.getByRole("region", { name: "Kubernetes change timeline", exact: true });
        await expect(unfocused.getByRole("heading", { name: "Kubernetes changes", exact: true })).toBeVisible();
        await expect(unfocused.getByRole("region", { name: "Resource timeline chart", exact: true })).toHaveCount(1);
        await expect(unfocused.getByRole("status", { name: "Loading projected change history", exact: true })).toHaveCount(0);
        const liveChart = unfocused.getByRole("region", { name: "Resource timeline chart", exact: true });
        if (viewport.name === "mobile") {
          await liveChart.evaluate((element) => { element.scrollLeft = element.scrollWidth; });
          await expect(liveChart.locator(".resource-timeline-break-line")).toBeInViewport();
          await expect(liveChart.locator('[data-marker="change"]').first()).toBeInViewport();
        }
        await captureBreakState(page, `break-timeline-general-${viewport.name}-${theme}.png`, theme);
        const liveMarker = liveChart.locator('[data-marker="change"]').first();
        await liveMarker.focus();
        await liveChart.evaluate((element) => { element.scrollLeft = 80; });
        const livePan = await liveChart.evaluate((element) => element.scrollLeft);
        const initialWindow = await liveChart.getAttribute("data-since");
        await page.clock.fastForward(30_000);
        await expect(liveChart).not.toHaveAttribute("data-since", initialWindow!);
        await expect(liveMarker).toBeFocused();
        expect(await liveChart.evaluate((element) => element.scrollLeft)).toBe(livePan);
        await expect(liveChart.locator(".resource-timeline-now-line")).toHaveCount(1);
        await page.keyboard.press("Enter");
        const liveHeading = unfocused.getByRole("heading", { name: "Timeline event details", exact: true });
        await expect(liveHeading).toBeFocused();
        const selectedWindow = await liveChart.getAttribute("data-since");
        const selectedPan = await liveChart.evaluate((element) => element.scrollLeft);
        await page.clock.fastForward(30_000);
        await expect(liveChart).not.toHaveAttribute("data-since", selectedWindow!);
        await expect(liveHeading).toBeFocused();
        expect(await liveChart.evaluate((element) => element.scrollLeft)).toBe(selectedPan);
        await page.keyboard.press("Escape");
        await expect(liveMarker).toBeFocused();
        await unfocused.getByRole("textbox", { name: "Timeline namespace", exact: true }).fill("shop");
        const filteredRead = page.waitForRequest((request) => {
          const url = new URL(request.url());
          return url.pathname.endsWith("/kubernetes/changes") && url.searchParams.get("kind") === "Deployment" && url.searchParams.get("namespace") === "shop";
        });
        await unfocused.getByRole("combobox", { name: "Timeline kind", exact: true }).selectOption("Deployment");
        await filteredRead;
        await expect(unfocused.getByRole("combobox", { name: "Timeline kind", exact: true })).toHaveValue("Deployment");
        await page.getByRole("tab", { name: "Issues", exact: true }).click();
        const issues = page.getByRole("region", { name: "Kubernetes issues", exact: true });
        const expanded = issues.getByRole("button", { name: "Expand checkout", exact: true });
        await expect(expanded).toHaveAttribute("aria-expanded", "false");
        await expect(issues.getByRole("article", { name: "Deployment checkout", exact: true }).getByRole("button", { name: /Warnings/ })).toHaveCount(0);
        const prior = reads.filter((url) => url.searchParams.get("root_uid") === "checkout-uid").length;
        await expanded.focus();
        await page.keyboard.press("Enter");
        await expect(issues.getByText("OOMKilled on 6 pods", { exact: true })).toBeVisible();
        if (viewport.name === "mobile") await expect(issues.getByText("v41 → v42", { exact: true })).not.toBeVisible();
        else await expect(issues.getByText("v41 → v42", { exact: true })).toBeVisible();
        await expect.poll(() => reads.filter((url) => url.searchParams.get("root_uid") === "checkout-uid").length).toBe(prior + 1);
        const rootRead = reads.filter((url) => url.searchParams.get("root_uid") === "checkout-uid").at(-1)!;
        expect(rootRead.searchParams.get("namespace")).toBe("shop");
        expect(rootRead.searchParams.get("include")).toBe("changes");
        await issues.getByRole("button", { name: "Collapse checkout", exact: true }).click();
        await issues.getByRole("button", { name: "Expand checkout", exact: true }).click();
        await expect(issues.getByText("OOMKilled on 6 pods", { exact: true })).toBeVisible();
        expect(reads.filter((url) => url.searchParams.get("root_uid") === "checkout-uid")).toHaveLength(prior + 1);
        await captureBreakState(page, `break-issues-expanded-${viewport.name}-${theme}.png`, theme);
        await issues.getByRole("button", { name: "Open resource checkout", exact: true }).click();
        await expect(page.getByRole("dialog", { name: "Details panel", exact: true })).toBeVisible();
        await page.getByRole("button", { name: "Close panel", exact: true }).click();
        await issues.getByLabel("Issue severity", { exact: true }).selectOption("critical");
        await expect(issues.getByRole("article")).toHaveCount(2);
        await issues.getByLabel("Issue severity", { exact: true }).selectOption("");
        await issues.getByRole("button", { name: "Next page", exact: true }).click();
        await expect(issues.getByRole("article")).toHaveCount(1);
        await expect(issues).toContainText("cart");
        if (cluster) expect(reads.filter((url) => url.pathname.endsWith("/issues")).every((url) => url.searchParams.get("cluster") === cluster)).toBe(true);
        await issues.getByRole("button", { name: "Investigate cart", exact: true }).click();
        await expect(page).toHaveURL(/\/agent\/chat\?/);
        const attachment = new URL(page.url()).searchParams;
        expect(attachment.get("uid")).toBe("cart-uid");
        expect(attachment.get("include_changes")).toBe("true");
        expect(attachment.get("cluster")).toBe(cluster ?? "fixture");
        expect(errors).toEqual([]);
      });
      }
      test(`Break timeline collapsed issue uncertainty ${viewport.name} ${theme}`, async ({ page }) => {
        await page.setViewportSize(viewport);
        await page.addInitScript((value) => localStorage.setItem("versus.theme", value), theme);
        const rootReads: URL[] = [];
        page.on("request", (request) => { const url = new URL(request.url()); if (url.pathname.endsWith("/kubernetes/issues") && url.searchParams.has("root_uid")) rootReads.push(url); });
        await openApp(page, "/agent/kubernetes?view=issues");
        const issues = page.getByRole("region", { name: "Kubernetes issues", exact: true });
        const checkout = issues.getByRole("article", { name: "Deployment checkout", exact: true });
        const toggle = checkout.getByRole("button", { name: "Warnings (1)", exact: true });
        await expect(toggle).toBeVisible();
        expect(rootReads).toHaveLength(0);
        await toggle.focus();
        await page.keyboard.press("Enter");
        await expect(checkout.getByText("History completeness not yet loaded.", { exact: true })).toBeVisible();
        const warningBounds = await toggle.boundingBox();
        const timestampBounds = await checkout.locator("span.font-mono").filter({ hasText: "Broke" }).boundingBox();
        expect(warningBounds).not.toBeNull();
        expect(timestampBounds).not.toBeNull();
        expect(warningBounds!.y - (timestampBounds!.y + timestampBounds!.height), "warning focus outline clears the timestamp").toBeGreaterThanOrEqual(2);
        await expect(checkout.getByRole("button", { name: "Expand checkout", exact: true })).toHaveAttribute("aria-expanded", "false");
        expect(rootReads).toHaveLength(0);
        const ledger = issues.getByRole("article", { name: "Deployment ledger-api", exact: true });
        await expect(ledger.getByRole("button", { name: "Warnings (1)", exact: true })).toBeVisible();
        await expect(ledger.getByRole("button", { name: "Expand ledger-api", exact: true })).toHaveAttribute("aria-expanded", "false");
        expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
        fs.mkdirSync(screenshotDir, { recursive: true });
        await page.screenshot({ path: path.join(screenshotDir, `break-issues-collapsed-uncertainty-${viewport.name}-${theme}.png`), animations: "disabled" });
        await checkout.getByRole("button", { name: "Expand checkout", exact: true }).click();
        await expect(checkout.getByText("OOMKilled on 6 pods", { exact: true })).toBeVisible();
        await expect(checkout.getByRole("button", { name: /Warnings/ })).toHaveCount(0);
        expect(rootReads).toHaveLength(1);
      });
      test(`Break timeline secondary diagnostics ${viewport.name} ${theme}`, async ({ page }) => {
        await page.setViewportSize(viewport);
        await page.addInitScript((value) => localStorage.setItem("versus.theme", value), theme);
        await openApp(page, "/agent/kubernetes");
        const health = page.getByRole("region", { name: "Cluster health", exact: true });
        const timeline = page.getByRole("region", { name: "Break timeline", exact: true });
        await expect(health).toContainText("Snapshot 4s ago");
        await expect(health).not.toContainText(/Partial visibility|Synced/);
        await expect(health.getByRole("button", { name: "Warnings (2)", exact: true })).toHaveAttribute("aria-expanded", "false");
        const aggregate = timeline.getByRole("button", { name: "Warnings (2)", exact: true });
        await expect(aggregate).toHaveAttribute("aria-expanded", "false");
        await expect(timeline.getByText("OOMKilled on 6 pods", { exact: true })).toBeVisible();
        const controls = await page.getByRole("button", { name: /Warnings \(/ }).evaluateAll((buttons) => buttons.map((button) => button.getAttribute("aria-controls")));
        expect(new Set(controls).size).toBe(controls.length);
        fs.mkdirSync(screenshotDir, { recursive: true });
        await page.screenshot({ path: path.join(screenshotDir, `break-diagnostics-collapsed-${viewport.name}-${theme}.png`), animations: "disabled" });
        await aggregate.click();
        await expect(aggregate).toHaveAttribute("aria-expanded", "true");
        await expect(timeline.getByText("ConfigMap: forbidden", { exact: true })).toBeVisible();
        await expect(timeline.getByText(/Change history gap:/)).toBeVisible();
        await timeline.getByText(/Change history gap:/).scrollIntoViewIfNeeded();
        await page.screenshot({ path: path.join(screenshotDir, `break-diagnostics-expanded-${viewport.name}-${theme}.png`), animations: "disabled" });
        await timeline.getByRole("button", { name: "Timeline for checkout", exact: true }).click();
        const focused = page.getByRole("region", { name: "Kubernetes change timeline", exact: true });
        const toggle = focused.getByRole("button", { name: "Warnings (4)", exact: true });
        await expect(toggle).toHaveAttribute("aria-expanded", "false");
        await toggle.click();
        await expect(focused.getByText("ConfigMap: forbidden", { exact: true })).toBeVisible();
        await focused.getByRole("combobox", { name: "Timeline kind", exact: true }).selectOption("Deployment");
        await expect(focused.getByRole("button", { name: "Warnings (4)", exact: true })).toHaveAttribute("aria-expanded", "false");
        await expect(page.getByRole("region", { name: "Timeline lane checkout", exact: true })).toBeVisible();
        expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
      });
    }
  }

  for (const viewport of [{ name: "desktop", width: 1440, height: 1100 }, { name: "mobile", width: 390, height: 844 }]) {
    test(`Break timeline full related history ${viewport.name} multi-cluster`, async ({ page }) => {
      await page.setViewportSize(viewport);
      const reads: URL[] = [];
      page.on("request", (request) => { const url = new URL(request.url()); if (url.pathname.startsWith("/api/admin/kubernetes/")) reads.push(url); });
      await openApp(page, "/agent/kubernetes?cluster=east");
      const checkout = page.getByRole("region", { name: "Break timeline", exact: true }).getByRole("article", { name: "Deployment checkout", exact: true });
      await checkout.getByRole("button", { name: /more changes/ }).click();
      const lane = page.getByRole("region", { name: "Timeline lane checkout", exact: true });
      await expect(lane.getByRole("button", { name: /ConfigMap checkout-settings-19:.* at/ })).toHaveCount(1);
      await expect(lane).not.toContainText("unrelated-");
      await expect(lane.locator('[data-marker="change"]')).toHaveCount(20);
      const location = new URL(page.url());
      expect(location.searchParams.get("cluster")).toBe("east");
      expect(location.searchParams.get("namespace")).toBe("shop");
      expect(location.searchParams.get("root_uid")).toBe("checkout-uid");
      expect(location.searchParams.has("center")).toBe(true);
      const scopedChanges = reads.filter((url) => url.pathname.endsWith("/changes") && url.searchParams.get("namespace") === "shop");
      const cursors = scopedChanges.map((url) => url.searchParams.get("cursor"));
      expect(cursors.filter((cursor, index) => index === 0 || cursor !== cursors[index - 1])).toEqual([null, "unrelated", "linked"]);
      await page.getByRole("button", { name: "Next page", exact: true }).click();
      await expect(lane.getByRole("button", { name: /ConfigMap checkout-settings-23:.* at/ })).toHaveCount(1);
      await expect(lane.locator('[data-marker="change"]')).toHaveCount(4);
      await expect(page.getByRole("button", { name: "Next page", exact: true })).toBeDisabled();
      expect(reads.filter((url) => /\/(issues|changes|graph)$/.test(url.pathname)).every((url) => url.searchParams.get("cluster") === "east")).toBe(true);
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
      fs.mkdirSync(screenshotDir, { recursive: true });
      await captureBreakState(page, `break-timeline-related-${viewport.name}.png`, "dark");
    });

    test(`Overview refresh keeps paired count cards and inventory tabs ${viewport.name}`, async ({ page }, testInfo) => {
      await page.setViewportSize({ width: viewport.width, height: viewport.height });
      await page.clock.install();
      const endpoints = ["overview", "usage", "workloads", "resources", "issues", "top", "changes", "releases", "traffic"];
      const requests: Array<{ endpoint: string; since: string | null; until: string | null }> = [];
      const graphReads: string[] = [];
      let responseStatus = 200;
      let releasePending = () => {};
      let pending: Promise<void> | undefined;
      await page.route((url) => url.pathname.startsWith("/api/admin/kubernetes/"), async (route) => {
        const url = new URL(route.request().url());
        const endpoint = url.pathname.split("/").at(-1)!;
        if (endpoint.includes("graph")) graphReads.push(url.pathname);
        if (!endpoints.includes(endpoint)) return route.fallback();
        requests.push({ endpoint, since: url.searchParams.get("since"), until: url.searchParams.get("until") });
        if (pending) await pending;
        if (responseStatus !== 200) return route.fulfill({ status: responseStatus, json: { error: responseStatus === 403 ? "Synthetic permission denied" : "Synthetic temporary failure" } });
        return route.fallback();
      });
      await openApp(page, "/agent/kubernetes?view=overview");
      const summary = page.getByRole("region", { name: "Triage summary", exact: true });
      const topology = page.getByRole("region", { name: "Overview topology", exact: true });
      const changes = summary.locator("article").filter({ has: page.getByRole("heading", { name: "Recent changes (60m)", exact: true }) });
      await expect(changes.getByText("Pod payments/checkout-api-0", { exact: true })).toBeVisible();
      await expect(summary.getByRole("status", { name: /^Loading/ })).toHaveCount(0);
      await expect.poll(() => new Set(requests.map(({ endpoint }) => endpoint)).size).toBe(endpoints.length);
      await page.clock.pauseAt(await page.evaluate(() => new Date(Date.now() + 1000).toISOString()));
      await page.clock.runFor(1000);
      await expect(topology.getByRole("img", { name: "Kubernetes relationship schematic", exact: true })).toHaveCount(0);
      await expect(page.getByRole("region", { name: "Workloads", exact: true })).toHaveCount(0);
      await expect(page.getByRole("region", { name: "Nodes", exact: true })).toHaveCount(0);
      const counts = topology.getByLabel("Cluster resource counts");
      await expect(counts.locator('dl[aria-label="Reported resource counts"] dt')).toHaveText(["Namespace", "Node", "Pod", "Workloads (aggregate)"]);
      const unreported = counts.locator('dl[aria-label="Unreported resource counts"] dd');
      expect(await unreported.count()).toBeGreaterThan(0);
      for (const value of await unreported.all()) {
        await expect(value).toHaveText("0");
        await expect(value).toHaveAttribute("title", "Count not reported");
        await expect(value).toHaveAttribute("aria-label", /count not reported, displayed as zero$/);
      }
      const snapshot = async () => ({
        cards: await summary.locator("article").evaluateAll((cards) => cards.map((card) => ({ text: card.textContent, width: card.getBoundingClientRect().width, height: card.getBoundingClientRect().height }))),
        counts: await counts.innerText(),
      });
      const loaded = await snapshot();
      const expectStableSnapshot = async () => {
        const current = await snapshot();
        expect(current.counts).toBe(loaded.counts);
        expect(current.cards).toHaveLength(loaded.cards.length);
        for (const [index, card] of current.cards.entries()) {
          expect(card.text).toBe(loaded.cards[index].text);
          expect(card.width).toBeCloseTo(loaded.cards[index].width, 1);
          expect(card.height).toBeCloseTo(loaded.cards[index].height, 1);
        }
      };
      const historyBox = (await changes.boundingBox())!;
      const topologyBox = (await topology.boundingBox())!;
      expect(historyBox.height).toBeCloseTo(topologyBox.height, 1);
      expect(historyBox.height).toBeCloseTo(416, 1);
      if (viewport.name === "desktop") {
        await expect.poll(async () => Math.abs((await changes.boundingBox())!.y - (await topology.boundingBox())!.y)).toBeLessThan(0.05);
        expect(topologyBox.x).toBeGreaterThanOrEqual(historyBox.x + historyBox.width);
      } else {
        expect(topologyBox.y).toBeGreaterThanOrEqual(historyBox.y + historyBox.height);
      }
      const countScroller = topology.getByRole("region", { name: "Topology resource counts", exact: true });
      await expect(countScroller).toHaveCSS("overflow-y", "auto");
      await expect(changes.getByRole("region", { name: "Recent change history", exact: true })).toHaveCSS("overflow-y", "auto");
      await countScroller.evaluate((element) => { element.scrollTop = element.scrollHeight; });
      if (viewport.name === "mobile") expect(await countScroller.evaluate((element) => element.scrollTop)).toBeGreaterThan(0);
      await countScroller.evaluate((element) => { element.scrollTop = 0; });
      const capture = async (state: string) => {
        expect(await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth)).toBe(false);
        fs.mkdirSync(screenshotDir, { recursive: true });
        for (const [section, target] of [["changes", changes], ["topology", topology]] as const) {
          await target.evaluate((element) => element.scrollIntoView({ block: "start", behavior: "instant" }));
          await expect(target.getByRole("heading", { level: 2 })).toBeInViewport();
          await page.screenshot({ path: path.join(screenshotDir, `overview-refresh-${state}-${section}-${viewport.name}.png`), animations: "disabled" });
        }
      };
      await capture("loaded");
      pending = new Promise<void>((resolve) => { releasePending = resolve; });
      const initialChange = requests.find(({ endpoint }) => endpoint === "changes")!;
      requests.length = 0;
      await page.clock.runFor(30_000);
      await expect.poll(() => requests.some(({ endpoint }) => endpoint === "changes")).toBe(true);
      const timerChange = requests.find(({ endpoint }) => endpoint === "changes")!;
      expect(Date.parse(timerChange.until!) - Date.parse(timerChange.since!)).toBe(3_600_000);
      expect(Date.parse(timerChange.until!)).toBeGreaterThan(Date.parse(initialChange.until!));
      await expectStableSnapshot();
      await expect(summary.getByRole("status", { name: /^Loading/ })).toHaveCount(0);
      await capture("timer-pending");
      pending = undefined;
      releasePending();
      await page.clock.runFor(1000);
      await expect(page.getByRole("button", { name: "Refresh Kubernetes data", exact: true })).toBeEnabled();
      pending = new Promise<void>((resolve) => { releasePending = resolve; });
      requests.length = 0;
      await page.getByRole("button", { name: "Refresh Kubernetes data", exact: true }).click();
      await expect.poll(() => new Set(requests.map(({ endpoint }) => endpoint)).size).toBe(endpoints.length);
      await expectStableSnapshot();
      await expect(page.getByLabel("Loading Kubernetes overview", { exact: true })).toHaveCount(0);
      await expect(summary.getByRole("status", { name: /^Loading/ })).toHaveCount(0);
      await capture("manual-pending");
      responseStatus = 503;
      pending = undefined;
      releasePending();
      await page.clock.runFor(1000);
      await expect(page.getByRole("button", { name: "Refresh Kubernetes data", exact: true })).toBeEnabled();
      await expectStableSnapshot();
      await expect(counts.locator('dl[aria-label="Reported resource counts"]')).toBeVisible();
      expect(graphReads).toEqual([]);
      responseStatus = 200;
      await page.getByRole("tab", { name: "Workloads", exact: true }).click();
      await expect(page.getByRole("region", { name: "Workloads", exact: true })).toBeVisible();
      await expect(page.getByRole("button", { name: "Select Pod payments/checkout-api-0", exact: true })).toBeVisible();
      await expect(page).toHaveURL(/view=resources/);
      await page.getByRole("tab", { name: "Nodes", exact: true }).click();
      await expect(page.getByRole("region", { name: "Nodes", exact: true })).toBeVisible();
      await expect(page.getByRole("region", { name: "Workloads", exact: true })).toHaveCount(0);
      await expect(page).toHaveURL(/view=nodes/);
      await testInfo.attach("refresh-requests", { body: JSON.stringify(requests), contentType: "application/json" });
    });
  }

  for (const viewport of [{ name: "desktop", width: 1440, height: 1000 }, { name: "mobile", width: 390, height: 844 }]) {
    test(`Pod stream controls, exports, cancellation, and unavailable previous ${viewport.name}`, async ({ page, context }) => {
      await page.setViewportSize({ width: viewport.width, height: viewport.height });
      await context.grantPermissions(["clipboard-read", "clipboard-write"]);
      await openApp(page, "/agent/kubernetes?r=core~v1~pods/payments/checkout-api-0&tab=logs");
      const detail = page.getByRole("dialog", { name: "Details panel" });
      await expect(detail.getByRole("tab", { name: "Actions", exact: true })).toHaveCount(0);
      const output = detail.getByLabel("Pod log output");
      await expect(output).toContainText("[REDACTED]");
      await expect(detail.getByLabel("Log connection status")).toHaveText("ended");
      await expect(detail.getByText("300 / 300 retained lines", { exact: true })).toBeVisible();
      await expect(detail.getByLabel("Log container").locator("option")).toHaveText(["app", "init:setup", "debug (ephemeral)"]);
      if (viewport.name === "mobile") await detail.getByRole("button", { name: "More", exact: true }).click();
      await detail.getByLabel("Log filter").fill("a.b");
      await expect(output).not.toContainText("axb");
      await expect(detail.getByText("150 / 300 retained lines", { exact: true })).toBeVisible();
      for (const enabled of [true, false, true]) {
        await detail.getByLabel("Timestamps").setChecked(enabled);
        const rendered = (await output.locator("[data-log-key]").allTextContents()).map((line) => line.replace(/\n$/, "")).join("\n");
        await detail.getByLabel("Copy scrubbed logs").click();
        const copied = await page.evaluate(() => navigator.clipboard.readText());
        expect(copied).toBe(rendered);
        expect(copied.split("\n")).toHaveLength(150);
        for (const line of copied.split("\n")) expect(line.match(/2026-10-07T12:00:00Z/g) ?? []).toHaveLength(enabled ? 1 : 0);
        expect(copied).toContain("[REDACTED]");
        expect(copied).not.toContain("axb");
        const downloaded = page.waitForEvent("download");
        await detail.getByLabel("Download scrubbed logs").click();
        const file = await downloaded;
        expect(file.suggestedFilename()).toBe("checkout-api-0-logs.txt");
        expect(fs.readFileSync((await file.path())!, "utf8")).toBe(copied);
      }
      await detail.getByLabel("Pause logs").click();
      await expect.poll(() => page.evaluate(() => (window as unknown as { podStreamSignals: Array<{ aborted: boolean }> }).podStreamSignals[0].aborted)).toBe(true);
      const resumed = page.waitForRequest((request) => new URL(request.url()).searchParams.has("cursor"));
      await detail.getByLabel("Resume logs").click();
      const resumeQuery = new URL((await resumed).url()).searchParams;
      expect(resumeQuery.get("cursor")).toBe("cursor-300");
      expect(resumeQuery.has("since_seconds")).toBe(false);
      expect(resumeQuery.has("tail_lines")).toBe(false);
      await detail.getByLabel("Log container").selectOption("setup");
      await expect.poll(() => page.evaluate(() => (window as unknown as { podStreamSignals: Array<{ url: string }> }).podStreamSignals.at(-1)?.url)).toContain("container=setup");
      await detail.getByLabel("Log filter").fill("");
      await detail.getByLabel("Previous container logs").check();
      await expect(output).toContainText("previous_unavailable Previous logs unavailable");
      await expect(output).not.toContainText("resumed after reconnect at");
      expect(await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth)).toBe(false);
      fs.mkdirSync(screenshotDir, { recursive: true });
      await page.screenshot({ path: path.join(screenshotDir, `pod-stream-controls-${viewport.name}.png`), fullPage: true, animations: "disabled" });
      await detail.getByRole("button", { name: "Close panel" }).click();
      await expect.poll(() => page.evaluate(() => (window as unknown as { podStreamSignals: Array<{ aborted: boolean }> }).podStreamSignals.every((entry) => entry.aborted))).toBe(true);
    });

    test(`visible drawer tabs, history fallback, and aligned topology controls ${viewport.name}`, async ({ page }) => {
      await page.setViewportSize({ width: viewport.width, height: viewport.height });
      await openApp(page, "/agent/kubernetes?r=apps~v1~deployments/payments/checkout-api&tab=actions");
      const detail = page.getByRole("dialog", { name: "Details panel" });
      const tabs = detail.getByRole("tablist", { name: "Resource detail views" });
      await expect(tabs.getByRole("tab", { name: "Overview", exact: true })).toHaveAttribute("aria-selected", "true");
      await expect(tabs.getByRole("tab", { name: "Actions", exact: true })).toHaveCount(0);
      await expect(tabs.getByRole("tab", { name: "Logs", exact: true })).toHaveCount(1);
      await expect(page).not.toHaveURL(/tab=actions/);
      await tabs.getByRole("tab", { name: "Overview", exact: true }).focus();
      await page.keyboard.press("ArrowRight");
      await expect(tabs.getByRole("tab", { name: "Logs", exact: true })).toBeFocused();
      await page.evaluate(() => { window.history.pushState(null, "", "/agent/kubernetes?r=apps~v1~deployments/payments/checkout-api&tab=logs"); window.dispatchEvent(new PopStateEvent("popstate")); });
      await expect(tabs.getByRole("tab", { name: "Logs", exact: true })).toHaveAttribute("aria-selected", "true");
      await expect(page).toHaveURL(/tab=logs/);
      await detail.getByRole("button", { name: "Close panel" }).click();
      await page.getByRole("tab", { name: "Topology", exact: true }).click();
      await page.getByRole("button", { name: "Open namespace payments topology" }).click();
      const controls = page.getByRole("group", { name: "Topology namespace controls" });
      const boxes = await controls.locator("button,select").evaluateAll((elements) => elements.map((element) => { const box = element.getBoundingClientRect(); return { top: box.top, bottom: box.bottom, left: box.left, right: box.right }; }));
      expect(boxes).toHaveLength(3);
      expect(Math.max(...boxes.map((box) => box.top))).toBeLessThan(Math.min(...boxes.map((box) => box.bottom)));
      const refreshed = page.waitForRequest((request) => new URL(request.url()).pathname === "/api/admin/kubernetes/graph");
      await controls.getByLabel("Refresh namespace topology").click();
      expect(new URL((await refreshed).url()).searchParams.get("namespace")).toBe("payments");
      expect(await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth)).toBe(false);
      fs.mkdirSync(screenshotDir, { recursive: true });
      await page.screenshot({ path: path.join(screenshotDir, `topology-control-row-${viewport.name}.png`), fullPage: true, animations: "disabled" });
    });
  }
});

test.describe("Workloads and focused investigation worst-case browser gate", () => {
  test.skip(process.env.E2E_UI_FIXTURES !== "1", "deterministic intercepted UI evidence, not live collection");

  for (const width of [1440, 390]) {
    test(`published never-listed timeout, last-good count and successful empty zero ${width}`, async ({ page }, testInfo) => {
      await page.setViewportSize({ width, height: 900 });
      await page.addInitScript(() => localStorage.setItem("versus.theme", "dark"));
      const observed = new Date().toISOString();
      let phase: "never-listed" | "known" | "later-timeout" | "empty" = "never-listed";
      const timedOut = () => phase === "never-listed" || phase === "later-timeout";
      const sync = () => ({ state: timedOut() ? "error" : "ready", partial: timedOut(), age_s: 18,
        // The published timeout trace has no pending CronJob.
        pending: null, pending_kinds: 0 });
      const partials = () => [
        ...(timedOut() ? [{ resource_id: "batch~v1~cronjobs", class: "timeout" }] : []),
      ];
      const resources = [
        { resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "shop", name: "only-a", uid: "only-a" },
        { resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "shop", name: "web", uid: "web-v1" },
        { resource_id: "core~v1~pods", kind: "Pod", namespace: "shop", name: "only-a-0", uid: "only-a-pod" },
        { resource_id: "core~v1~pods", kind: "Pod", namespace: "shop", name: "web-0", uid: "web-v1-pod" },
        { resource_id: "apps~v1~statefulsets", kind: "StatefulSet", namespace: "shop", name: "db", uid: "db-v1" },
      ];
      const payloads: unknown[] = [];
      await page.route("**/api/**", async route => {
        const pathname = new URL(route.request().url()).pathname;
        const reply = (json: unknown, status = 200) => route.fulfill({ json, status });
        if (pathname.endsWith("/clusters")) return reply({ multiple: false, clusters: [] });
        if (pathname.endsWith("/sync")) return reply({
          ...sync(), first_ready_at: observed, total_kinds: 20, ready_kinds: timedOut() ? 19 : 20,
          failing_kinds: timedOut() ? 1 : 0, unreadable_kinds: 0,
          failing: timedOut() ? [{ kind: "CronJob", state: "error", class: "timeout", records: phase === "later-timeout" ? 1 : 0 }] : [],
          unreadable: [],
          kinds: { CronJob: { state: timedOut() ? "error" : "ready", error: timedOut() ? "timeout" : "",
            records: phase === "never-listed" || phase === "empty" ? 0 : 1,
            ...(phase === "never-listed" ? {} : { observed_at: observed }) } },
        });
        if (pathname.endsWith("/workloads")) {
          const payload = {
            items: phase === "empty" ? [] : [...resources, ...(phase === "never-listed" ? [] : [
              { resource_id: "batch~v1~cronjobs", kind: "CronJob", namespace: "shop", name: "nightly", uid: "nightly-v1" },
            ])],
            counts: phase === "empty" ? { Deployment: 0, StatefulSet: 0, DaemonSet: 0, Job: 0, Pod: 0, CronJob: 0 }
              : { Deployment: 2, StatefulSet: 1, DaemonSet: 0, Job: 0, Pod: 2, ...(phase === "never-listed" ? {} : { CronJob: 1 }) },
            observed_at: observed, truncated: false, sync: sync(), partial_failures: partials(),
          };
          payloads.push({ phase, ...payload });
          return reply(payload);
        }
        if (pathname.endsWith("/namespaces")) return reply({ items: ["shop"], sync: sync() });
        if (pathname.endsWith("/overview")) return reply({ connector: "kubernetes", observed_at: observed, nodes: 1, pods: 2, namespaces: 1, workloads: 3, sync: sync() });
        if (pathname.includes("/deployment") || pathname.includes("/sso/")) return reply({ error: "Community" }, 403);
        return reply({ items: [], nodes: [], edges: [], omitted: {}, sources: [], totals: {}, broken: { total: 0 }, sync: sync(), enable: false });
      });
      const panel = page.getByRole("tabpanel", { name: "Workloads", exact: true });
      const expectCount = async (kind: string, count?: number, soft = false) => {
        const assertion = soft ? expect.soft : expect;
        if (width === 390) await assertion(panel.getByRole("combobox", { name: "Workload kind" }).locator(`option[value="${kind}"]`)).toHaveText(count === undefined ? kind : `${kind} (${count})`, { timeout: 1000 });
        else await assertion(panel.getByRole("tab", { name: kind, exact: true })).toHaveText(count === undefined ? kind : `${kind}${count}`, { timeout: 1000 });
      };
      const shot = async () => {
        await expect(panel).not.toContainText("Preparing your cluster");
        expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
        await page.screenshot({ path: capturePath("workload-count-provenance", width, "dark", phase), fullPage: true, animations: "disabled" });
      };
      await page.goto("/agent/kubernetes?view=resources");
      await expect(page.locator('[data-sync-problem="CronJob"]')).toContainText("the cluster is slow or rate-limiting reads");
      // Keep the acceptance failure while collecting independent later phases.
      await expect.soft(panel).toContainText("web", { timeout: 1000 });
      await expect.soft(panel).toContainText("db", { timeout: 1000 });
      await expect.soft(panel).toContainText("Count unavailable", { timeout: 1000 });
      await expect(panel).not.toContainText("total");
      await expectCount("CronJob");
      await expectCount("All");
      await expectCount("Deployment", 2, true);
      await expectCount("StatefulSet", 1, true);
      await expectCount("Pod", 2, true);
      await expectCount("Job", 0, true);
      await shot();
      for (phase of ["known", "later-timeout"] as const) {
        await page.getByRole("button", { name: "Refresh Kubernetes data", exact: true }).click();
        await expect(panel).toContainText("nightly");
        await expectCount("CronJob", 1);
        await expect(panel).toContainText("6 total");
        if (phase === "later-timeout") await expect(page.locator('[data-sync-problem="CronJob"]')).toContainText("the cluster is slow or rate-limiting reads");
        await shot();
      }
      phase = "empty";
      await page.getByRole("button", { name: "Refresh Kubernetes data", exact: true }).click();
      await expect(panel).toContainText("No workloads in this scope.");
      await expect(panel).not.toContainText("nightly");
      await expect(panel).not.toContainText("web");
      await expect(panel).toContainText("0 total");
      await expectCount("CronJob", 0);
      await expectCount("Deployment", 0);
      await shot();
      await testInfo.attach("count-provenance-responses", { body: JSON.stringify(payloads), contentType: "application/json" });
    });
  }

  for (const width of [1440, 390]) for (const health of ["degraded", "failed"] as const) {
    test(`pending counts and bounded post-publication ${health} recovery ${width}`, async ({ page }, testInfo) => {
      await page.setViewportSize({ width, height: 900 });
      await page.addInitScript(() => localStorage.setItem("versus.theme", "dark"));
      const observed = new Date().toISOString();
      let phase: "cold" | "published" | "pending" | "warming" | "empty" = "cold";
      const coldPending = ["apps~v1~deployments", "apps~v1~statefulsets", "apps~v1~daemonsets", "batch~v1~jobs", "batch~v1~cronjobs", "core~v1~pods"];
      let pending = "cronjobs", scopedReads = 0;
      let release!: () => void;
      const barrier = new Promise<void>(resolve => { release = resolve; });
      const sync = () => ({ state: phase === "cold" ? "syncing" : "ready", age_s: 0, partial: false,
        ...(phase === "cold" ? { pending: coldPending, pending_kinds: 20 } : phase === "pending" ? { pending: [pending] } : {}) });
      await page.route("**/api/**", async route => {
        const url = new URL(route.request().url());
        const reply = (json: unknown, status = 200) => route.fulfill({ json, status });
        if (url.pathname.endsWith("/clusters")) return reply({ multiple: false, clusters: [] });
        if (url.pathname.endsWith("/sync") && phase === "cold") return reply({
          ...sync(), first_ready: false, total_kinds: 20, ready_kinds: 0,
          kinds: Object.fromEntries(["Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob", "Pod"].map(kind => [kind, { state: "pending", first_ready: false }])),
        });
        if (url.pathname.endsWith("/sync")) return reply({
          state: health === "failed" ? "error" : "ready", partial: true, age_s: 0, first_ready_at: observed,
          kinds: { Pod: { state: health === "failed" ? "error" : "ready", partial: true, observed_at: observed, records: 1, error: health === "failed" ? "unavailable" : "timeout" } },
          total_kinds: 1, ready_kinds: health === "failed" ? 0 : 1, failing_kinds: 1, pending_kinds: 0,
        });
        if (url.pathname.endsWith("/workloads")) {
          // The real cold response omits counts entirely; pending is not zero.
          if (phase === "cold") return reply({ items: [], truncated: false, sync: sync() });
          if (phase === "warming") {
            if (++scopedReads === 1) await barrier;
            return reply({ code: "inventory_not_ready", error: "Scoped inventory warming", retryable: true }, 503);
          }
          return reply({ items: phase === "empty" ? [] : [{ resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "shop", name: "committed-api", uid: "committed-uid" }],
            counts: { Deployment: phase === "empty" ? 0 : 2, ...(phase === "pending" ? {} : { CronJob: 0 }) }, observed_at: observed, truncated: false, sync: sync() });
        }
        if (url.pathname.endsWith("/namespaces")) return reply({ items: ["shop", "payments"], sync: sync() });
        if (url.pathname.endsWith("/overview")) return reply({ connector: "kubernetes", observed_at: observed, nodes: 1, pods: 1, namespaces: 2, workloads: 2, sync: sync() });
        if (url.pathname.includes("/deployment") || url.pathname.includes("/sso/")) return reply({ error: "Community" }, 403);
        return reply({ items: [], nodes: [], edges: [], omitted: {}, sources: [], totals: {}, broken: { total: 0 }, sync: sync(), enable: false });
      });
      const panel = page.getByRole("tabpanel", { name: "Workloads", exact: true });
      const shot = async (state: string) => {
        expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
        await page.screenshot({ path: capturePath("workload-health", width, "dark", `${health}-${state}`), fullPage: true, animations: "disabled" });
      };
      try {
        await page.goto("/agent/kubernetes?view=resources");
        await page.getByRole("tab", { name: "Workloads", exact: true }).click();
        await expect(panel).toContainText("Preparing your cluster");
        await expect(panel).not.toContainText("0 total");
        await expect(panel.getByRole("combobox", { name: "Workload kind" })).toHaveCount(0);
        await expect(panel.getByRole("tab")).toHaveCount(0);
        await shot("cold-pending");
        phase = "published";
        await page.getByRole("button", { name: "Refresh Kubernetes data", exact: true }).click();
        await expect(panel).toContainText("committed-api");
        await expect(page.locator(`[data-sync-strip="${health}"]`)).toBeVisible();
        phase = "pending";
        for (pending of ["cronjobs", "batch~v1~cronjobs"]) {
          await page.getByRole("button", { name: "Refresh Kubernetes data", exact: true }).click();
          await expect(panel).toContainText("Count unavailable");
          if (width === 390) {
            const kinds = panel.getByRole("combobox", { name: "Workload kind" });
            await expect(kinds.locator('option[value="CronJob"]')).toHaveText("CronJob");
            await expect(kinds.locator('option[value="Deployment"]')).toHaveText("Deployment (2)");
            await expect(kinds.locator('option[value="All"]')).toHaveText("All");
          } else {
            await expect(panel.getByRole("tab", { name: "CronJob", exact: true })).toHaveText("CronJob");
            await expect(panel.getByRole("tab", { name: "Deployment", exact: true })).toContainText("2");
            await expect(panel.getByRole("tab", { name: "All", exact: true })).toHaveText("All");
          }
          await expect(panel).not.toContainText("2 total");
        }
        await shot("pending");
        await page.clock.install({ time: new Date() });
        phase = "warming";
        await panel.getByRole("combobox", { name: "Workload namespace", exact: true }).fill("payments");
        await page.getByRole("option", { name: "payments", exact: true }).click();
        await expect.poll(() => scopedReads).toBe(1);
        await expect(panel).not.toContainText("committed-api");
        await shot("loading");
        release();
        for (let retry = 0; retry < 6; retry++) {
          await page.clock.fastForward(1050);
          await page.waitForTimeout(50);
        }
        await expect(panel.getByRole("button", { name: "Retry inventory", exact: true })).toBeVisible();
        expect(scopedReads).toBe(6);
        await expect(panel).toContainText("Inventory unavailable");
        await expect(panel).not.toContainText("0 total");
        await shot("exhausted");
        await page.clock.fastForward(10_000);
        expect(scopedReads).toBe(6);
        phase = "empty";
        await panel.getByRole("button", { name: "Retry inventory", exact: true }).click();
        await expect(panel).toContainText("No workloads in this namespace.");
        await expect(panel).toContainText("0 total");
        if (width === 390) await expect(panel.getByRole("combobox", { name: "Workload kind" }).locator('option[value="CronJob"]')).toHaveText("CronJob (0)");
        else await expect(panel.getByRole("tab", { name: "CronJob", exact: true })).toContainText("0");
        await shot("explicit-retry-empty");
        await testInfo.attach("bounded-health", { body: JSON.stringify({ classification: "fixture", health, scopedReads, automaticRetries: scopedReads - 1 }), contentType: "application/json" });
      } finally { release(); }
    });
  }

  for (const width of [1440, 390]) for (const theme of ["dark", "light"] as const) {
    test(`scope resync late warmup cursor recovery and denial callback fence ${width} ${theme}`, async ({ page }, testInfo) => {
      test.setTimeout(120_000);
      await page.setViewportSize({ width, height: 900 });
      await page.emulateMedia({ colorScheme: theme });
      await page.addInitScript(value => localStorage.setItem("versus.theme", value), theme);
      const observed = new Date(Date.now() - 60_000).toISOString();
      let phase = "published", warmingReads = 0;
      let releaseWarm!: () => void, releaseLate!: () => void;
      const warm = new Promise<void>(resolve => { releaseWarm = resolve; });
      const late = new Promise<void>(resolve => { releaseLate = resolve; });
      const requests: Array<{ phase: string; path: string; query: Record<string, string> }> = [];
      const sync = () => ({ state: phase === "resync" || phase === "late-warmup" ? "syncing" : "ready", age_s: 0, partial: false });
      await page.route("**/api/**", async route => {
        const url = new URL(route.request().url()), query = Object.fromEntries(url.searchParams);
        requests.push({ phase, path: url.pathname, query });
        const reply = (json: unknown, status = 200) => route.fulfill({ json, status });
        if (url.pathname.endsWith("/clusters")) return reply({ multiple: false, clusters: [] });
        if (url.pathname.endsWith("/sync")) return phase === "denied"
          ? reply({ code: "forbidden", error: "Infrastructure access denied" }, 403)
          : reply({ ...sync(), first_ready_at: observed, total_kinds: 2, ready_kinds: phase === "resync" ? 1 : 2, pending_kinds: phase === "resync" ? 1 : 0, kinds: {} });
        if (url.pathname.endsWith("/workloads")) {
          const namespace = query.namespace || "shop";
          const item = { resource_id: "apps~v1~deployments", kind: "Deployment", namespace, uid: `${namespace}-uid`, name: `${namespace}-committed`, ready: 1, desired: 1 };
          if (phase === "late-warmup" && namespace === "payments" && warmingReads++ === 0) {
            await warm;
            return reply({ code: "inventory_not_ready", error: "Scoped inventory warming", retryable: true }, 503);
          }
          if (phase === "hold-positive") { await late; return reply({ items: [item], counts: { Deployment: 1 }, observed_at: observed, sync: sync() }).catch(() => {}); }
          if (phase === "denied") return reply({ code: "forbidden", error: "Infrastructure access denied" }, 403);
          if (query.cursor) return reply({ code: "cursor_expired", error: "Inventory generation changed" }, 400);
          if (phase === "resync") return reply({ code: "inventory_not_ready", error: "Replacement warming", retryable: true }, 503);
          return reply({ items: phase === "empty" ? [] : [item], counts: { Deployment: phase === "empty" ? 0 : 1 }, observed_at: observed, sync: sync(), ...(phase === "cursor" ? { next: "old-generation" } : {}) });
        }
        if (url.pathname.endsWith("/namespaces")) return reply({ items: ["shop", "payments"], sync: sync() });
        if (url.pathname.endsWith("/overview")) return reply({ connector: "kubernetes", observed_at: observed, nodes: 1, pods: 1, namespaces: 2, workloads: 1, sync: sync() });
        if (url.pathname.includes("/deployment") || url.pathname.includes("/sso/")) return reply({ error: "Community" }, 403);
        return reply({ items: [], nodes: [], edges: [], omitted: {}, sources: [], totals: {}, broken: { total: 0 }, sync: sync(), enable: false });
      });
      const rows = page.locator(".kubernetes-workload-shell[role=listitem]");
      const panel = page.getByRole("tabpanel", { name: "Workloads", exact: true });
      const capture = async (state: string) => {
        expect.soft(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)).toBe(false);
        if (captureUnchanged) await page.screenshot({ path: capturePath("workload-recovery", width, theme, state), fullPage: true, animations: "disabled" });
      };
      try {
        await page.goto("/agent/kubernetes");
        await page.getByRole("tab", { name: "Workloads", exact: true }).click();
        await expect(rows).toContainText("shop-committed");
        phase = "resync";
        await page.getByRole("button", { name: "Refresh Kubernetes data" }).click();
        await expect(page.locator('[data-sync-state="preparing"]')).toHaveText("Syncing");
        await expect(rows).toContainText("shop-committed");
        await expect(panel).not.toContainText("Resource inventory is unavailable.");
        if (width === 390) await expect(panel.getByRole("combobox", { name: "Workload kind" }).locator('option[value="Deployment"]')).toHaveText("Deployment (1)");
        else await expect(panel.getByRole("tab", { name: "Deployment", exact: true })).toContainText("1");
        await capture("same-scope-sync");
        await page.reload();
        await page.getByRole("tab", { name: "Workloads", exact: true }).click();
        await expect(rows).toHaveCount(0);
        await expect(panel).not.toContainText(/All 0|Deployment 0/);
        await capture("reload-unknown");
        phase = "late-warmup";
        await panel.getByRole("combobox", { name: "Workload namespace" }).fill("payments");
        await page.getByRole("option", { name: "payments", exact: true }).click();
        await expect.poll(() => warmingReads).toBe(1);
        await expect(rows).toHaveCount(0);
        await capture("scope-loading");
        phase = "ready";
        // The held warmup completes after the ready status has been observed.
        await page.getByRole("button", { name: "Refresh Kubernetes data" }).click();
        releaseWarm();
        await expect(rows).toContainText("payments-committed", { timeout: 45_000 });
        await expect(rows).not.toContainText("shop-committed");
        await capture("late-warmup-recovered");
        phase = "cursor";
        await page.getByRole("button", { name: "Refresh Kubernetes data" }).click();
        await panel.getByRole("button", { name: "Next page", exact: true }).click();
        await expect(panel).toContainText("This inventory page expired");
        await expect(rows).toHaveCount(0);
        await capture("cursor-expired");
        await panel.getByRole("button", { name: "Restart inventory paging" }).click();
        await expect(rows).toContainText("payments-committed");
        phase = "empty";
        await page.getByRole("button", { name: "Refresh Kubernetes data" }).click();
        await expect(rows).toHaveCount(0);
        await capture("complete-empty");
        phase = "ready";
        await page.getByRole("button", { name: "Refresh Kubernetes data" }).click();
        await expect(rows).toHaveCount(1);
        phase = "hold-positive";
        await page.getByRole("button", { name: "Refresh Kubernetes data" }).click();
        await expect.poll(() => requests.filter(request => request.phase === "hold-positive" && request.path.endsWith("/workloads")).length).toBeGreaterThan(0);
        phase = "denied";
        await page.getByRole("button", { name: "Refresh Kubernetes data" }).click();
        await expect(rows).toHaveCount(0);
        releaseLate();
        await page.waitForTimeout(1000);
        await expect(rows).toHaveCount(0);
        await expect(panel).not.toContainText("payments-committed");
        await capture("denied-late-positive");
      } finally {
        releaseWarm(); releaseLate();
        await testInfo.attach("scope-recovery-requests", { body: JSON.stringify(requests), contentType: "application/json" });
      }
    });
  }

  for (const width of [1440, 390]) for (const theme of ["dark", "light"] as const) {
    test(`pinned recurrence detail consumes empty continuations and explains missing capture ${width} ${theme}`, async ({ page }, testInfo) => {
      await page.setViewportSize({ width, height: 1000 });
      await page.emulateMedia({ colorScheme: theme });
      await page.addInitScript(value => localStorage.setItem("versus.theme", value), theme);
      const at = new Date(Date.now() - 300_000).toISOString().replace(/\.\d{3}Z$/, ".123456789Z");
      const later = new Date(Date.now() - 60_000).toISOString();
      const sync = { state: "ready", age_s: 0, partial: false };
      const root = { kind: "Deployment", namespace: "shop", name: "checkout", uid: "checkout-uid" };
      let state = "recurring";
      let denyDetail = false, holdDetail = false, heldDetail = 0;
      let releaseDetail!: () => void;
      const detailBarrier = new Promise<void>(resolve => { releaseDetail = resolve; });
      const reads: Array<{ state: string; path: string; query: Record<string, string> }> = [];
      const selected = () => ({ root, occurrence_id: "pinned-old", state: state === "resolved" || state === "deleted" ? "resolved" : "open", rule: "pod.crash_loop", count: 1, severity: "critical", broken: false, ...(state === "missing-time" ? {} : { broke_at: at }) });
      await page.route("**/api/**", async route => {
        const url = new URL(route.request().url()), query = Object.fromEntries(url.searchParams);
        reads.push({ state, path: url.pathname, query });
        const reply = (json: unknown, status = 200) => route.fulfill({ json, status });
        if (url.pathname.endsWith("/clusters")) return reply({ multiple: false, clusters: [] });
        if (url.pathname.endsWith("/sync")) return reply({ ...sync, first_ready_at: at, total_kinds: 1, ready_kinds: 1, kinds: {} });
        if (url.pathname.endsWith("/issues") && denyDetail) return reply({ code: "forbidden", error: "Root access denied" }, 403);
        if (url.pathname.endsWith("/issues")) return reply({
          items: [query.context === "persisted" ? { ...selected(), occurrence_id: "wrong-new", broke_at: later } : selected()],
          ...(query.context === "persisted" ? { root_context: { root, occurrence: { ...selected(), occurrence_id: "wrong-new", broke_at: later }, retained: true }, selected_occurrence: selected(), selection_status: "resolved" } : {}),
          totals: { critical: 1 }, broken: { total: 1 }, sync,
        });
        if (url.pathname.endsWith("/timeline")) return reply({
          items: [{ type: "break", id: "pinned-old", at, issue: selected() }],
          total: 1, crash_count: 0, counts: { change: 0, crash: 0, break: 1 }, gaps: [], sync,
        });
        if (url.pathname.endsWith("/changes")) {
          const cursor = query.cursor;
          const item = (id: string) => ({ id, cluster: "fixture", ...root, type: "image_changed", fields: [{ path: "image", from: "v1", to: id }], at });
          return reply({ items: cursor === "empty-changes" ? [] : cursor ? [item("first-marker"), item("last-marker")] : [item("first-marker")],
            ...(!cursor ? { next: "empty-changes" } : cursor === "empty-changes" ? { next: "last-changes" } : {}), gaps: [], sync });
        }
        if (url.pathname.endsWith("/crashes")) {
          const item = (id: string) => ({ id, sequence: id === "crash-first" ? 1 : 2, cluster: "fixture", namespace: "shop", pod: id, pod_uid: id, root_uid: root.uid, root_kind: root.kind, root_name: root.name, container: "api", type: "termination", reason: "OOMKilled", exit_code: 137, at, observed_at: at, time_source: "finished_at" });
          return reply({ items: query.cursor === "empty-crashes" ? [] : query.cursor ? [item("crash-first"), item("crash-last")] : [item("crash-first")],
            ...(!query.cursor ? { next: "empty-crashes" } : query.cursor === "empty-crashes" ? { next: "last-crashes" } : {}),
            since: query.since, until: query.until, gaps: [], capture_gaps: [], coverage: {}, limitations: [], capture: { mode: "continuous", behind: false }, sync });
        }
        if (url.pathname.endsWith("/break-logs")) {
          if (holdDetail) { heldDetail++; await detailBarrier; }
          return reply({
          window: { from: query.since, to: query.until }, pods: [{ name: "retained-pod", uid: "retained-pod", state: "deleted", short: "retained" }],
          main_container: "api", containers: [{ name: "api", main: true }], markers: [],
          lines: state === "no-logs" || state === "deleted" ? [] : [{ at, pod: "retained-pod", pod_uid: "retained-pod", container: "api", init: false, slot: "previous", level: "error", text: "synthetic-filter pinned retained line", source: "captured" }], rate: [], patterns: [], sources: [], counts: { all: state === "no-logs" || state === "deleted" ? 0 : 1, errors: state === "no-logs" || state === "deleted" ? 0 : 1, warnings: 0 },
          coverage: { state: "partial", reason: "capture_not_found", tip: true, gaps: [{ from: query.since, to: query.until }] },
        }).catch(() => {});
        }
        if (url.pathname.endsWith("/overview")) return reply({ connector: "kubernetes", observed_at: at, nodes: 1, pods: 1, namespaces: 1, workloads: 1, sync });
        if (url.pathname.includes("/deployment") || url.pathname.includes("/sso/")) return reply({ error: "Community" }, 403);
        return reply({ items: [], nodes: [], edges: [], omitted: {}, sources: [], totals: {}, broken: { total: 0 }, sync, enable: false });
      });
      const renderWarnings: string[] = [];
      page.on("console", message => {
        if (message.type() === "error" && message.text().includes("Cannot update a component")) renderWarnings.push(message.text());
      });
      for (state of ["overview-entry", "issues-entry", "timeline-entry", "recurring", "resolved", "deleted", "no-logs", "missing-time"]) {
        if (state.endsWith("-entry")) {
          await page.goto("/agent/kubernetes");
          if (state === "timeline-entry") {
            await page.getByRole("tab", { name: "Timeline", exact: true }).click();
            await page.getByRole("link", { name: "Open break details", exact: true }).click();
          } else {
            if (state === "issues-entry") await page.getByRole("tab", { name: "Issues", exact: true }).click();
            await page.getByRole("button", { name: "Timeline for checkout", exact: true }).click();
          }
          await expect(page.getByRole("tab", { name: "Timeline", exact: true })).toHaveAttribute("aria-selected", "true");
          expect(new URL(page.url()).searchParams.get("center")).toBe(at);
          expect(new URL(page.url()).searchParams.get("occurrence_id")).toBe("pinned-old");
        } else {
          await page.goto(`/agent/kubernetes?view=timeline&root_uid=${root.uid}&namespace=shop&occurrence_id=pinned-old&center=${at}`);
          await page.getByRole("tab", { name: "Timeline", exact: true }).click();
        }
        const lane = page.getByRole("region", { name: "Timeline lane checkout", exact: true });
        await expect(lane).toBeVisible();
        const logs = page.getByRole("region", { name: "Logs at break", exact: true });
        await expect(logs).toBeVisible();
        await expect.poll(() => reads.filter(read => read.state === state && read.path.endsWith("/changes") && read.query.cursor === "last-changes").length).toBeGreaterThanOrEqual(1);
        await expect.poll(() => reads.filter(read => read.state === state && read.path.endsWith("/crashes") && read.query.cursor === "last-crashes").length).toBeGreaterThanOrEqual(1);
        expect(reads.filter(read => read.state === state && read.path.endsWith("/issues") && read.query.context === "persisted")
          .every(read => read.query.center === at && read.query.occurrence_id === "pinned-old")).toBe(true);
        expect.soft(await page.getByRole("button", { name: /^(Prev|Previous|Next)( page| crash page)?$/ }).count()).toBe(0);
        expect.soft(await page.getByText(/^Page \d/).count()).toBe(0);
        if (state === "missing-time") {
          await expect(logs).toContainText(/time range.*unavailable|break time.*unresolved/i);
          expect.soft(await lane.getByRole("slider").count()).toBe(0);
          expect.soft(reads.filter(read => read.state === state && read.path.endsWith("/break-logs"))).toHaveLength(0);
        } else {
          const handle = lane.getByRole("slider", { name: "Log window start", exact: true });
          await expect(handle).toBeVisible();
          await handle.focus();
          await expect(handle).toBeFocused();
          const bracket = await handle.getAttribute("aria-valuenow");
          const chart = page.getByRole("region", { name: "Resource timeline chart", exact: true });
          const markers = chart.locator('[data-marker="change"]');
          await expect(markers).toHaveCount(2);
          await markers.first().focus();
          await page.keyboard.press("Enter");
          await expect(page.getByRole("region", { name: "Timeline event details", exact: true })).toBeVisible();
          const filter = logs.getByRole("textbox", { name: "Search in log window" });
          await expect(logs.getByRole("combobox", { name: "Log container", exact: true })).toBeEnabled();
          await expect(logs.getByRole("combobox", { name: "Log container", exact: true })).not.toContainText("Loading");
          await filter.fill("synthetic-filter");
          const scrollBeforeRefresh = await chart.evaluate(element => {
            element.scrollLeft = Math.min(64, element.scrollWidth - element.clientWidth);
            return element.scrollLeft;
          });
          if (width === 390) expect(scrollBeforeRefresh).toBeGreaterThan(0);
          await page.getByRole("button", { name: "Refresh Kubernetes data" }).click();
          await expect(filter).toHaveValue("synthetic-filter");
          await expect(handle).toHaveAttribute("aria-valuenow", bracket!);
          await expect(markers).toHaveCount(2);
          await expect(page.getByRole("region", { name: "Timeline event details", exact: true })).toBeVisible();
          await expect.poll(() => chart.evaluate(element => element.scrollLeft)).toBe(scrollBeforeRefresh);
          await chart.evaluate(element => { element.scrollLeft = 0; });
          expect.soft(reads.filter(read => read.state === state && read.path.endsWith("/break-logs")).every(read =>
            Date.parse(read.query.since) <= Date.parse(at) && Date.parse(read.query.until) >= Date.parse(at) && Date.parse(read.query.until) < Date.parse(later))).toBe(true);
        }
        expect.soft(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)).toBe(false);
        if (captureUnchanged || state.endsWith("-entry")) await page.screenshot({ path: capturePath("focused-investigation", width, theme, state), fullPage: true, animations: "disabled" });
      }
      try {
        state = "denial-fence";
        await page.goto(`/agent/kubernetes?view=timeline&root_uid=${root.uid}&namespace=shop&occurrence_id=pinned-old&center=${at}`);
        await page.getByRole("tab", { name: "Timeline", exact: true }).click();
        await expect(page.getByRole("log", { name: "Break log lines" })).toContainText("pinned retained line");
        holdDetail = true;
        await page.getByRole("region", { name: "Logs at break", exact: true }).getByRole("button", { name: "Previous container", exact: true }).click();
        await expect.poll(() => heldDetail).toBeGreaterThan(0);
        denyDetail = true;
        await page.getByRole("button", { name: "Refresh Kubernetes data" }).click();
        await expect(page.getByRole("slider", { name: "Log window start" })).toHaveCount(0);
        releaseDetail();
        await page.waitForTimeout(1000);
        await expect(page.getByRole("slider", { name: "Log window start" })).toHaveCount(0);
        await expect(page.getByRole("log", { name: "Break log lines" })).toHaveCount(0);
        if (captureUnchanged) await page.screenshot({ path: capturePath("focused-investigation", width, theme, "denied-late-logs"), fullPage: true, animations: "disabled" });
      } finally { releaseDetail(); }
      expect(renderWarnings).toEqual([]);
      fs.writeFileSync(path.join(process.env.HARNESS_RESULTS_DIR!, `focused-investigation-${width}-${theme}.json`), JSON.stringify({ classification: "fixture; deleted is retained identity without live resource, not proof of server deletion", reads }, null, 2));
      await testInfo.attach("pinned-requests", { body: JSON.stringify(reads), contentType: "application/json" });
    });
  }

  for (const width of [1440, 1024, 390]) for (const theme of ["dark", "light"] as const) {
    test(`mixed workload column edges and keyboard controls ${width} ${theme}`, async ({ page }, testInfo) => {
      await page.setViewportSize({ width, height: 900 });
      await page.addInitScript(value => localStorage.setItem("versus.theme", value), theme);
      await page.emulateMedia({ colorScheme: theme });
      const sync = { state: "ready", age_s: 0, partial: false };
      let crowded = false;
      const base = [
        { kind: "CronJob", resource_id: "batch~v1~cronjobs" },
        { kind: "Deployment", resource_id: "apps~v1~deployments" },
        { kind: "Pod", resource_id: "core~v1~pods" },
        { kind: "ConfigMap", resource_id: "core~v1~configmaps" },
      ];
      await page.route("**/api/**", route => {
        const url = new URL(route.request().url());
        if (url.pathname === "/api/admin/kubernetes/workloads") {
          const items = Array.from({ length: crowded ? 32 : 4 }, (_, index) => ({
            ...base[index % 4], namespace: "namespace-with-a-long-but-valid-name",
            name: `${base[index % 4].kind.toLowerCase()}-${index}-${"long-name-".repeat(8)}`,
            uid: `resource-${index}`, summary: { ready: 1, desired: 1 }, conditions: [],
          }));
          return route.fulfill({ json: { items, counts: { CronJob: crowded ? 8 : 1, Deployment: crowded ? 8 : 1, Pod: crowded ? 8 : 1, ConfigMap: crowded ? 8 : 1 }, observed_at: new Date().toISOString(), sync } });
        }
        if (url.pathname.endsWith("/sync")) return route.fulfill({ json: { ...sync, first_ready_at: "2026-10-30T00:00:00Z", kinds: {}, total_kinds: 4, ready_kinds: 4 } });
        if (url.pathname.endsWith("/clusters")) return route.fulfill({ json: { multiple: false, clusters: [] } });
        if (url.pathname.endsWith("/namespaces")) return route.fulfill({ json: { items: ["namespace-with-a-long-but-valid-name"], sync } });
        if (url.pathname.endsWith("/overview")) return route.fulfill({ json: { connector: "kubernetes", nodes: 1, pods: 1, namespaces: 1, workloads: 4, observed_at: new Date().toISOString(), sync } });
        if (url.pathname.includes("/deployment") || url.pathname.includes("/sso/")) return route.fulfill({ status: 403, json: { error: "Community" } });
        return route.fulfill({ json: { items: [], sources: [], nodes: [], edges: [], omitted: {}, counts: {}, totals: {}, broken: { total: 0 }, sync, enable: false } });
      });
      await page.goto("/agent/kubernetes?view=workloads");
      await page.getByRole("tab", { name: "Workloads", exact: true }).click();
      const report = [];
      for (const state of ["no-scrollbar", "scrollbar"] as const) {
        if (state === "scrollbar") {
          crowded = true;
          await page.getByRole("button", { name: "Refresh Kubernetes data" }).click();
        }
        const rows = page.locator(".kubernetes-workload-shell[role=listitem]");
        await expect(rows).toHaveCount(crowded ? 32 : 4);
        const measurement = await rows.evaluateAll(elements => {
          const head = document.querySelector(".kubernetes-workload-head")!;
          const headers = Array.from(head.querySelector(".kubernetes-list-head")!.children);
          const scroller = head.parentElement!;
          return {
            desktop: getComputedStyle(head).display !== "none",
            scrollbar: scroller.scrollHeight > scroller.clientHeight,
            gutter: scroller.offsetWidth - scroller.clientWidth,
            overflow: document.documentElement.scrollWidth > innerWidth,
            rows: elements.map(row => {
              const button = row.querySelector(".kubernetes-list-row--workload")!;
              const cells = Array.from(button.children);
              return { kind: cells[2].textContent, deltas: [1, 2, 3].map(index => ({
                column: headers[index].textContent,
                headerX: headers[index].getBoundingClientRect().x,
                cellX: cells[index].getBoundingClientRect().x,
                delta: Math.abs(headers[index].getBoundingClientRect().x - cells[index].getBoundingClientRect().x),
              })), nestedButtons: button.querySelectorAll("button").length,
              actionOutside: Array.from(row.querySelectorAll("button")).some(control => control.getBoundingClientRect().right > row.getBoundingClientRect().right + 1) };
            }),
          };
        });
        report.push({ width, theme, state, ...measurement });
        expect.soft(measurement.scrollbar).toBe(crowded);
        expect.soft(measurement.overflow).toBe(false);
        if (width === 1440) expect.soft(measurement.desktop).toBe(true);
        for (const row of measurement.rows) {
          expect.soft(row.nestedButtons).toBe(0);
          expect.soft(row.actionOutside).toBe(false);
          if (measurement.desktop) for (const edge of row.deltas) expect.soft(edge.delta, `${row.kind} ${edge.column}`).toBeLessThanOrEqual(1);
        }
        const first = rows.first().getByRole("button").first();
        await first.focus();
        await expect(first).toBeFocused();
        const unsupported = rows.filter({ has: page.locator(".pill", { hasText: "CronJob" }) });
        expect.soft(await unsupported.getByRole("button").count()).toBe(crowded ? 8 : 1);
        if (captureUnchanged || process.env.E2E_NATIVE_SCROLLBARS === "1" && width === 1440 && theme === "dark") {
          await page.screenshot({ path: capturePath("workload-alignment", width, theme, state), fullPage: true, animations: "disabled" });
        }
      }
      fs.writeFileSync(path.join(process.env.HARNESS_RESULTS_DIR!, `workload-alignment-${width}-${theme}.json`), JSON.stringify({ classification: "fixture", thresholdCSSPixels: 1, report }, null, 2));
      await testInfo.attach("exact-column-edges", { body: JSON.stringify(report), contentType: "application/json" });
    });
  }
});

test.describe("Kubernetes overview", () => {
  test.skip(process.env.E2E_KUBERNETES_BACKEND !== "fake", "requires a fresh Versus backend connected to the deterministic fake Kubernetes API");

  test("shows bounded partial cluster evidence and workload filters", async ({ page }) => {
    await openApp(page, "/agent/tools");
    const kubernetesCard = page.locator("article").filter({ has: page.getByRole("heading", { name: "Kubernetes", exact: true }) });
    await expect(kubernetesCard.getByRole("link", { name: "Open Kubernetes", exact: true })).toHaveCount(1);
    await kubernetesCard.getByRole("link", { name: "Open Kubernetes", exact: true }).click();
    await expect(page).toHaveURL(/\/agent\/kubernetes$/);
    await expect(page.getByRole("heading", { name: "Kubernetes", exact: true, level: 1 })).toBeVisible();
    await expect(page.getByText(/limited-cluster/)).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Refresh Kubernetes data" })).toBeVisible();
    await expect(page.getByText("Nodes ready 0/1")).toBeVisible();
    await expect(page.getByText(/^Metrics (available|partial|unavailable)$/, { exact: true })).toBeVisible();
    await expect(page.getByRole("heading", { name: "Workloads" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Select Pod payments/checkout-api-0", exact: true })).toBeVisible();
    await page.getByRole("button", { name: "Select Deployment payments/checkout-api", exact: true }).click();
    const deploymentDetail = page.getByRole("dialog", { name: "Details panel", exact: true });
    await expect(deploymentDetail.getByRole("heading", { name: "Deployment payments/checkout-api", exact: true })).toBeVisible();
    await expect(deploymentDetail).toContainText("ProgressDeadlineExceeded");
    await page.keyboard.press("Escape");
    await expect(deploymentDetail).toHaveCount(0);
    const nodes = page.getByRole("region", { name: "Nodes" });
    await nodes.getByRole("button", { name: "View pods on node-a" }).click();
    await expect(nodes.getByRole("list", { name: "Pods on node-a" })).toContainText("payments");
    await expect(nodes.getByRole("list", { name: "Pods on node-a" })).toContainText("api");
    await nodes.getByRole("button", { name: "All nodes" }).click();
    await expect(page.getByRole("tab", { name: "Topology" })).toBeVisible();
    await page.getByLabel("Workload namespace").fill("payments");
    await page.getByLabel("Resource name").fill("api");
    const workloads = page.getByRole("region", { name: "Workloads" });
    const podResult = workloads.getByRole("button", { name: "Select Pod payments/checkout-api-0", exact: true });
    await expect(podResult).toBeVisible();
    await podResult.click();
    const resourceDetail = page.getByRole("dialog", { name: "Details panel" });
    await expect(resourceDetail).toBeVisible();
    await expect(resourceDetail.getByRole("heading", { name: "Pod payments/checkout-api-0" })).toBeVisible();
    await expect(resourceDetail.getByText(/^\d+ restarts$/)).toBeVisible();
    await expect(resourceDetail.getByRole("heading", { name: "Restarts", exact: true })).toBeVisible();
    await expect(page.getByText(/secret-token/)).toHaveCount(0);

    const logs = await page.evaluate(async () => {
      const response = await fetch("/api/admin/kubernetes/pods/payments/checkout-api-0/logs?container=api&tail_lines=20");
      return response.json() as Promise<{ text: string }>;
    });
    expect(logs.text).not.toContain("super-secret-value");
    expect(logs.text).toContain("REDACTED");

    const projections = await page.evaluate(async () => {
      const [secret, configMap] = await Promise.all([
        fetch("/api/admin/kubernetes/resources/core~v1~secrets/api-secret/describe?namespace=payments").then((response) => response.text()),
        fetch("/api/admin/kubernetes/resources/core~v1~configmaps/api-config/describe?namespace=payments").then((response) => response.text()),
      ]);
      return { secret, configMap };
    });
    expect(projections.secret).not.toContain("c2VjcmV0LXRva2Vu");
    expect(projections.secret).toContain("token");
    expect(projections.configMap).not.toContain("must-not-cross");
    expect(projections.configMap).toContain("config.yaml");
    fs.mkdirSync(screenshotDir, { recursive: true });
    await page.screenshot({ path: path.join(screenshotDir, "bounded-resource-desktop.png"), fullPage: true, animations: "disabled" });
  });

  test("shows the timeline and opens a resource from the palette", async ({ page }) => {
    await openApp(page, "/agent/kubernetes");
    await expect(page.getByRole("heading", { name: "Kubernetes" })).toBeVisible();
    await page.getByRole("tab", { name: "Timeline" }).click();
    await expect(page.getByRole("heading", { name: "Kubernetes changes" })).toBeVisible();
    await page.getByRole("tab", { name: "Overview" }).click();
    await page.keyboard.press("Control+k");
    const palette = page.getByRole("dialog", { name: "Kubernetes command palette" });
    await palette.getByRole("combobox", { name: "Search Kubernetes resources and views" }).fill("api");
    const result = palette.getByRole("option", { name: /^checkout-api-0 Pod payments/ });
    await expect(result).toBeVisible({ timeout: 30_000 });
    await result.click();
    await expect(page.getByRole("dialog", { name: "Details panel" }).getByRole("heading", { name: "Pod payments/checkout-api-0" })).toBeVisible();
    fs.mkdirSync(screenshotDir, { recursive: true });
    await page.screenshot({ path: path.join(screenshotDir, "palette-resource-desktop.png"), fullPage: true, animations: "disabled" });
  });

  test("remains coherent on mobile", async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await openApp(page, "/agent/kubernetes");
    await expect(page.getByRole("heading", { name: "Kubernetes" })).toBeVisible();
    const main = await page.locator("main").boundingBox();
    expect(main?.width).toBeLessThanOrEqual(390);
    await expect(page.getByRole("region", { name: "Nodes" })).toBeVisible();
    const viewTabs = page.getByRole("tablist", { name: "Kubernetes views", exact: true });
    for (const tab of await viewTabs.getByRole("tab").all()) {
      await tab.scrollIntoViewIfNeeded();
      const box = await tab.boundingBox();
      expect(box).not.toBeNull();
      expect(box!.x).toBeGreaterThanOrEqual(0);
      expect(box!.x + box!.width).toBeLessThanOrEqual(390);
    }
    const overlaps = await page.locator("main button, main input, main select, main [role=listitem]").evaluateAll((elements) => {
      return elements.flatMap((element) => {
        if (element.getAttribute("role") === "tab" && element.parentElement?.getAttribute("aria-label") === "Kubernetes views" && getComputedStyle(element.parentElement).overflowX === "auto") return [];
        const box = element.getBoundingClientRect();
        return box.width > 0 && box.height > 0 && (box.left < 0 || box.right > document.documentElement.clientWidth + 1)
          ? [{ name: element.getAttribute("aria-label") ?? element.textContent, left: box.left, right: box.right, scrollParent: element.parentElement?.getAttribute("aria-label"), overflowX: element.parentElement ? getComputedStyle(element.parentElement).overflowX : null }]
          : [];
      });
    });
    expect(overlaps).toEqual([]);
    const layout = await page.evaluate(() => {
      const main = document.querySelector<HTMLElement>("#main")!;
      main.focus();
      return { overflow: document.documentElement.scrollWidth > document.documentElement.clientWidth, outline: getComputedStyle(main).outlineStyle };
    });
    expect(layout).toEqual({ overflow: false, outline: "none" });
    fs.mkdirSync(screenshotDir, { recursive: true });
    await page.screenshot({ path: path.join(screenshotDir, "coherence-mobile.png"), fullPage: true, animations: "disabled" });
  });
});

test.describe("Live K12a crash evidence acceptance", () => {
  test.skip(process.env.E2E_CRASH_EVIDENCE !== "1", "requires the live crash-evidence fakekube scenario");

  for (const viewport of [{ name: "desktop", width: 1440, height: 900 }, { name: "mobile", width: 390, height: 844 }]) {
    test(`renders six-namespace crash evidence, mobile disclosure, and timeline markers ${viewport.name}`, async ({ page }, testInfo) => {
      await page.setViewportSize(viewport);
      await page.emulateMedia({ colorScheme: "dark" });
      await page.addInitScript(() => localStorage.setItem("versus.theme", "dark"));
      const errors: string[] = [];
      page.on("pageerror", (error) => errors.push(error.message));
      const captureLive = async (filename: string) => {
        const proof = await page.evaluate(() => {
          const background = getComputedStyle(document.body).backgroundColor;
          const storedTheme = localStorage.getItem("versus.theme");
          if (storedTheme === "dark" && background === "rgb(13, 17, 24)") document.documentElement.setAttribute("data-theme", "dark");
          return { storedTheme, theme: document.documentElement.getAttribute("data-theme"), background, viewport: { width: innerWidth, height: innerHeight } };
        });
        expect(proof.storedTheme).toBe("dark");
        expect(proof.theme).toBe("dark");
        expect(proof.background).toBe("rgb(13, 17, 24)");
        expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
        fs.mkdirSync(screenshotDir, { recursive: true });
        const image = await page.screenshot({ path: path.join(screenshotDir, filename), animations: "disabled" });
        const manifestPath = path.join(screenshotDir, "manifest-live.json");
        const manifest = fs.existsSync(manifestPath) ? JSON.parse(fs.readFileSync(manifestPath, "utf8")) as { images: Record<string, unknown> } : { images: {} };
        manifest.images[filename] = { classification: "L: built OSS app against live TLS fakekube crash-evidence scenario", spec: "tests/e2e/kubernetes-overview.spec.ts", config: "tests/e2e/playwright.config.ts", sha256: createHash("sha256").update(image).digest("hex"), ...proof };
        fs.writeFileSync(manifestPath, JSON.stringify(manifest, null, 2));
      };

      await openApp(page, "/agent/kubernetes?view=overview");
      const apiBroken = await page.evaluate(async () => {
        const response = await fetch("/api/admin/kubernetes/issues?broken=true&include=changes&limit=20");
        return { status: response.status, text: await response.text() };
      });
      expect(apiBroken.status).toBe(200);
      expect(apiBroken.text).not.toContain("CANARY_");
      const brokenPage = JSON.parse(apiBroken.text) as { items: KubernetesIssue[]; broken: { total: number; by_severity: Record<string, number> } };
      expect(brokenPage.broken.total).toBe(6);
      expect(brokenPage.items.map((item) => `${item.root.namespace}/${item.root.name}`)).toEqual([
        "nodes/nodes-api", "batch/batch-job", "config/config-api", "evict/evict-api", "probe/probe-api", "sched/sched-api",
      ]);
      expect(brokenPage.items.some((item) => item.root.name === "healthy-api")).toBe(false);

      const timeline = page.getByRole("region", { name: "Break timeline", exact: true });
      await expect(timeline.getByRole("article")).toHaveCount(5);
      await expect(timeline.getByRole("button", { name: /^Broken / })).toContainText("6");
      await expect(timeline.getByRole("button", { name: /^Broken / })).toContainText("6 critical");
      await expect(timeline.getByRole("button", { name: /1 more/ })).toBeVisible();
      await expect(timeline).not.toContainText("·");
      await expect(timeline).not.toContainText("CANARY_");

      const nodesRow = timeline.getByRole("article", { name: "Deployment nodes-api", exact: true });
      const batchRow = timeline.getByRole("article", { name: "Job batch-job", exact: true });
      const configRow = timeline.getByRole("article", { name: "Deployment config-api", exact: true });
      const evictRow = timeline.getByRole("article", { name: "Deployment evict-api", exact: true });
      const probeRow = timeline.getByRole("article", { name: "Deployment probe-api", exact: true });

      await expect(nodesRow.getByLabel("CrashLoopBackOff", { exact: true })).toBeVisible();
      await expect(nodesRow).toContainText("2 of 3 pods affected");
      await expect(nodesRow.getByText("OOMKilled signal SIGKILL on 2 pods", { exact: true })).toBeVisible();
      await expect(nodesRow.getByText("Init Error exit 1 on 1 pod", { exact: true })).toBeVisible();
      await expect(nodesRow.getByText("CrashLoopBackOff on 1 pod", { exact: true })).toBeVisible();
      await expect(nodesRow.getByText("17 lifetime restarts", { exact: true })).toBeVisible();
      await expect(nodesRow.getByText("4 warning events: BackOff 4", { exact: true })).toBeVisible();

      await expect(batchRow.getByLabel("Pod failed", { exact: true })).toBeVisible();
      await expect(batchRow.getByText("Error exit 1 on 1 pod", { exact: true })).toBeVisible();
      await expect(batchRow.getByText("BackoffLimitExceeded", { exact: true })).toBeVisible();
      await expect(batchRow.getByText("4 warning events: BackoffLimitExceeded 4", { exact: true })).toBeVisible();

      await expect(configRow.getByLabel("Container config error", { exact: true })).toBeVisible();
      await expect(configRow.getByText("CreateContainerConfigError on 1 pod", { exact: true })).toBeVisible();
      await expect(configRow.getByText("4 warning events: Failed 4", { exact: true })).toBeVisible();

      await expect(evictRow.getByLabel("Evicted", { exact: true })).toBeVisible();
      await expect(evictRow.getByText("Error exit 1 on 1 pod", { exact: true })).toBeVisible();
      await expect(evictRow.getByText("Evicted on 1 pod", { exact: true })).toBeVisible();
      await expect(evictRow.getByText("4 warning events: Evicted 4", { exact: true })).toBeVisible();

      await expect(probeRow.getByLabel("Pods unavailable", { exact: true })).toBeVisible();
      await expect(probeRow.getByText("4 warning events: Unhealthy 4", { exact: true })).toBeVisible();

      if (viewport.name === "mobile") {
        await expect(nodesRow.getByText("Init CrashLoopBackOff on 1 pod", { exact: true })).not.toBeVisible();
        await expect(nodesRow.getByText("Node MemoryPressure on 3 pods", { exact: true })).not.toBeVisible();
        const toggle = nodesRow.getByRole("button", { name: "+2 more evidence for nodes-api", exact: true });
        await toggle.focus();
        await page.keyboard.press("Enter");
        const less = nodesRow.getByRole("button", { name: "Show less evidence for nodes-api", exact: true });
        await expect(less).toHaveAttribute("aria-expanded", "true");
        await expect(less).toBeFocused();
        await expect(nodesRow.getByText("Init CrashLoopBackOff on 1 pod", { exact: true })).toBeVisible();
        await expect(nodesRow.getByText("Node MemoryPressure on 3 pods", { exact: true })).toBeVisible();
      } else {
        await expect(nodesRow.getByText("Init CrashLoopBackOff on 1 pod", { exact: true })).toBeVisible();
        await expect(nodesRow.getByText("Node MemoryPressure on 3 pods", { exact: true })).toBeVisible();
      }
      for (const row of [nodesRow, batchRow, configRow, evictRow, probeRow]) {
        expect(await row.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(true);
      }
      await captureLive(`live-k12a-overview-${viewport.name}.png`);

      await timeline.getByRole("button", { name: /1 more/ }).click();
      const issues = page.getByRole("region", { name: "Kubernetes issues", exact: true });
      await expect(issues).toBeVisible();
      await expect(issues.getByRole("article", { name: "Deployment healthy-api", exact: true })).toHaveCount(0);
      const issueNodes = issues.getByRole("article", { name: "Deployment nodes-api", exact: true });
      const issueSched = issues.getByRole("article", { name: "Deployment sched-api", exact: true });
      await expect(issueSched.getByLabel("Unschedulable", { exact: true })).toBeVisible();
      if (viewport.name === "mobile") {
        const more = issueNodes.getByRole("button", { name: "+2 more evidence for nodes-api", exact: true });
        await more.focus();
        await page.keyboard.press("Enter");
        await expect(issueNodes.getByRole("button", { name: "Show less evidence for nodes-api", exact: true })).toHaveAttribute("aria-expanded", "true");
        await expect(issueNodes.getByRole("button", { name: "Show less evidence for nodes-api", exact: true })).toBeFocused();
      } else {
        await issueNodes.getByRole("button", { name: "Expand nodes-api", exact: true }).click();
      }
      await expect(issueNodes.getByRole("button", { name: "Collapse nodes-api", exact: true })).toHaveAttribute("aria-expanded", "true");
      await expect(issueNodes.getByText("Node MemoryPressure on 3 pods", { exact: true })).toBeVisible();
      await issueSched.getByRole("button", { name: "Expand sched-api", exact: true }).click();
      await expect(issueSched.getByText("Unschedulable on 1 pod", { exact: true })).toBeVisible();
      await expect(issueSched.getByText("Unschedulable on 1 pod", { exact: true })).toHaveClass(/pill-warn/);
      await expect(issueSched.getByText("4 warning events: FailedScheduling 4", { exact: true })).toBeVisible();
      await expect(issues).not.toContainText("·");
      expect(await issueNodes.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(true);
      expect(await issueSched.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(true);
      await captureLive(`live-k12a-issues-${viewport.name}.png`);

      const journalResponse = page.waitForResponse((response) => {
        const url = new URL(response.url());
        return url.pathname === "/api/admin/kubernetes/crashes" && url.searchParams.has("root_uid");
      });
      await issueNodes.getByRole("button", { name: "Timeline for nodes-api", exact: true }).click();
      const response = await journalResponse;
      expect(response.status()).toBe(200);
      const journalPage: KubernetesCrashPage = await response.json();
      const terminations = journalPage.items.filter((event) => event.type === "termination");
      expect(terminations.length).toBeGreaterThan(0);
      const lane = page.getByRole("region", { name: "Timeline lane nodes-api", exact: true });
      await expect(lane).toBeVisible();
      const chart = lane.getByRole("region", { name: "Resource timeline chart", exact: true });
      await expect(chart.locator('[data-marker="crash"]')).toHaveCount(terminations.length);
      expect((await chart.locator('[data-marker="crash"]').evaluateAll((elements) => elements.map((element) => element.getAttribute("data-event-id")))).sort()).toEqual(terminations.map((event) => event.id).sort());
      await expect(lane.getByText("Partial crash evidence", { exact: true })).toHaveCount(0);
      const journal = page.getByRole("region", { name: "Captured crash evidence", exact: true });
      await expect(journal.getByRole("list", { name: "Crash observations", exact: true }).getByRole("listitem")).toHaveCount(journalPage.items.length);
      for (const event of [terminations.find((item) => item.reason === "OOMKilled"), terminations.find((item) => item.init)]) {
        expect(event).toBeTruthy();
        const marker = chart.locator(`[data-event-id="${event!.id}"]`);
        await marker.focus();
        await page.keyboard.press("Enter");
        const details = lane.getByRole("region", { name: "Timeline event details", exact: true });
        await expect(details.getByRole("heading", { name: "Timeline event details", exact: true })).toBeFocused();
        await expect(details).toContainText(`Pod ${event!.pod} / ${event!.init ? "Init " : ""}container ${event!.container}: ${event!.reason}, exit ${event!.exit_code}`);
        if (event!.signal) await expect(details).toContainText(`signal ${event!.signal}`);
        await expect(details).toContainText(`${event!.restart_count} lifetime restarts`);
        await expect(details).not.toContainText("·");
        await captureLive(`live-k12a-timeline-${event!.init ? "init" : "oom"}-${viewport.name}.png`);
        await page.keyboard.press("Escape");
        await expect(details).toHaveCount(0);
        await expect(marker).toBeFocused();
      }
      expect(errors).toEqual([]);
      await testInfo.attach("k12a-live-broken-roots", { body: JSON.stringify(brokenPage.items.map((item) => ({ root: `${item.root.namespace}/${item.root.name}`, rule: item.rule, count: item.count, of: item.of, broke_at_source: item.broke_at_source, evidence: item.evidence }))), contentType: "application/json" });
    });
  }
});

test.describe("Live Break timeline logs and Agent tab acceptance", () => {
  test.skip(process.env.E2E_KUBERNETES_BREAK_LOGS_LIVE !== "1", "requires the live break-logs harness stack");
  const namespace = "k13vis";
  const rootUID = "k13vis-checkout";
  const rsUID = "k13vis-checkout-rs";
  const podUID = "k13vis-checkout-pod";
  const podName = "vis-checkout-6c8d4-x2k4q";
  const logPath = `/api/v1/namespaces/${namespace}/pods/${podName}/log`;
  let brokeAt = "";
  let windowFrom = "";
  let windowTo = "";

  test.beforeAll(async () => {
    const state = harnessRuntimeStateDir();
    const fakePort = Number(process.env.HARNESS_FAKEKUBE_PORT || 19443);
    const lokiPort = Number(process.env.HARNESS_LOKI_PORT || 13100);
    const baseURL = process.env.E2E_BASE_URL || "http://127.0.0.1:18080";
    const secret = process.env.E2E_GATEWAY_SECRET || "";
    const ca = fs.readFileSync(path.join(state, "fakekube/ca.crt"));
    const upstream = (endpoint: string, body?: unknown) => new Promise<unknown>((resolve, reject) => {
      const encoded = body === undefined ? undefined : JSON.stringify(body);
      const call = https.request(`https://localhost:${fakePort}${endpoint}`, { ca, method: encoded ? "POST" : "GET", headers: encoded ? { "Content-Type": "application/json" } : {} }, (response) => {
        let text = "";
        response.on("data", (chunk) => { text += chunk; });
        response.on("end", () => response.statusCode === 200 ? resolve(JSON.parse(text)) : reject(new Error(`fakekube ${endpoint} HTTP ${response.statusCode} ${text}`)));
      });
      call.on("error", reject);
      call.end(encoded);
    });
    const backend = async (endpoint: string, method = "GET", body?: unknown) => {
      const response = await fetch(`${baseURL}${endpoint}`, {
        method,
        headers: { "X-Gateway-Secret": secret, Origin: baseURL, ...(body ? { "Content-Type": "application/json" } : {}) },
        body: body ? JSON.stringify(body) : undefined,
      });
      return { status: response.status, json: await response.json() };
    };
    const second = (ms: number) => Math.floor(ms / 1000) * 1000;
    const iso = (ms: number) => new Date(ms).toISOString().replace(".000Z", "Z");
    const start = second(Date.now());
    const brokeMs = start - 240_000;
    const firstTermMs = start - 40_000;
    brokeAt = iso(brokeMs);
    windowFrom = iso(firstTermMs - 200_000);
    const firstLines = Array.from({ length: 12 }, (_, index) => ({
      at: iso(firstTermMs - 200_000 + index * 10_000),
      text: index === 2 ? "ERROR payment client timeout password=k13-capture-canary-value" : index % 3 === 0 ? `ERROR payment client timeout attempt ${index}` : `INFO starting worker pool ${index}`,
    }));
    await upstream("/_fake/mutate", { operation: "upsert", resource: "namespaces", namespace: "", name: namespace, object: { kind: "Namespace", metadata: {} } });
    await upstream("/_fake/mutate", {
      operation: "upsert", resource: "deployments", namespace, name: "vis-checkout",
      object: { apiVersion: "apps/v1", kind: "Deployment", metadata: { name: "vis-checkout", namespace, uid: rootUID, generation: 1, creationTimestamp: iso(start - 1_800_000), labels: { app: "vis-checkout" } }, spec: { replicas: 1, selector: { matchLabels: { app: "vis-checkout" } }, template: { spec: { containers: [{ name: "api", image: "example.invalid/k13:v1" }] } } }, status: { observedGeneration: 1, replicas: 1, readyReplicas: 1, availableReplicas: 1, updatedReplicas: 1 } },
    });
    await upstream("/_fake/mutate", {
      operation: "upsert", resource: "replicasets", namespace, name: "vis-checkout-6c8d4",
      object: { apiVersion: "apps/v1", kind: "ReplicaSet", metadata: { name: "vis-checkout-6c8d4", namespace, uid: rsUID, creationTimestamp: iso(start - 1_800_000), labels: { app: "vis-checkout" }, annotations: { "deployment.kubernetes.io/revision": "1" }, ownerReferences: [{ apiVersion: "apps/v1", kind: "Deployment", name: "vis-checkout", uid: rootUID, controller: true }] }, spec: { replicas: 1, template: { spec: { containers: [{ name: "api", image: "example.invalid/k13:v1" }] } } }, status: { replicas: 1, readyReplicas: 1 } },
    });
    const podObject = (restarts: number, terminatedAt: number | null) => ({
      apiVersion: "v1", kind: "Pod", metadata: { name: podName, namespace, uid: podUID, creationTimestamp: iso(start - 1_800_000), labels: { app: "vis-checkout" }, ownerReferences: [{ apiVersion: "apps/v1", kind: "ReplicaSet", name: "vis-checkout-6c8d4", uid: rsUID, controller: true }] },
      spec: { nodeName: "k13-node", containers: [{ name: "api", image: "example.invalid/k13:v1" }] },
      status: { phase: "Running", conditions: [{ type: "Ready", status: "False", lastTransitionTime: brokeAt }], containerStatuses: [{ name: "api", ready: false, restartCount: restarts, state: { waiting: { reason: "CrashLoopBackOff" } }, ...(terminatedAt ? { lastState: { terminated: { reason: "OOMKilled", exitCode: 137, finishedAt: iso(terminatedAt) } } } : {}) }] },
    });
    await upstream("/_fake/mutate", { operation: "upsert", resource: "pods", namespace, name: podName, object: podObject(0, null) });
    await upstream("/_fake/log-fixtures", { path: logPath, container: "api", previous: true, lines: firstLines });
    await upstream("/_fake/mutate", { operation: "upsert", resource: "pods", namespace, name: podName, object: podObject(1, firstTermMs) });
    await expect.poll(async () => {
      const res = await backend(`/api/admin/kubernetes/break-logs?namespace=${namespace}&root_uid=${rootUID}&since=${encodeURIComponent(windowFrom)}&until=${encodeURIComponent(iso(firstTermMs))}&limit=500`);
      return res.status === 200 && (res.json.lines ?? []).filter((line: { source: string }) => line.source === "captured").length === firstLines.length;
    }, { timeout: 90_000, intervals: [1000, 2000] }).toBe(true);

    const secondTermMs = second(Date.now()) - 5000;
    windowTo = iso(secondTermMs);
    const secondLines = Array.from({ length: 6 }, (_, index) => ({ at: iso(secondTermMs - 25_000 + index * 4000), text: `INFO second restart line ${index}` }));
    await upstream("/_fake/log-fixtures", { path: logPath, container: "api", previous: true, lines: secondLines });
    await upstream("/_fake/mutate", { operation: "upsert", resource: "pods", namespace, name: podName, object: podObject(3, secondTermMs) });
    await expect.poll(async () => {
      const res = await backend(`/api/admin/kubernetes/break-logs?namespace=${namespace}&root_uid=${rootUID}&since=${encodeURIComponent(windowFrom)}&until=${encodeURIComponent(windowTo)}&limit=500`);
      return res.status === 200 && res.json.coverage?.state === "partial" && res.json.coverage?.reason === "restarted_since" && res.json.coverage?.tip === true;
    }, { timeout: 90_000, intervals: [1000, 2000] }).toBe(true);

    const lokiEntries = [
      ...Array.from({ length: 6 }, (_, index) => ({ at: iso(firstTermMs + 3000 + index * 3000), text: `WARN loki between restarts ${index}` })),
      ...Array.from({ length: 4 }, (_, index) => ({ at: iso(second(Date.now()) - 30_000 + index * 5000), text: `INFO loki recent test link ${index}` })),
    ];
    const pushRes = await fetch(`http://127.0.0.1:${lokiPort}/loki/api/v1/push`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ streams: [{ stream: { job: "k8s-break-logs", namespace, pod: podName, container: "api" }, values: lokiEntries.map((entry) => [`${Date.parse(entry.at)}000000`, entry.text]) }] }),
    });
    expect(pushRes.status).toBeGreaterThanOrEqual(200);
    expect(pushRes.status).toBeLessThan(300);
    await expect.poll(async () => {
      const query = encodeURIComponent(`{job="k8s-break-logs",pod="${podName}"}`);
      const res = await fetch(`http://127.0.0.1:${lokiPort}/loki/api/v1/query_range?query=${query}&start=${(firstTermMs - 600_000) * 1e6}&end=${Date.now() * 1e6}&limit=100`);
      const data = await res.json();
      return data.data.result.reduce((sum: number, stream: { values: unknown[] }) => sum + stream.values.length, 0) === lokiEntries.length;
    }, { timeout: 60_000, intervals: [1000, 2000] }).toBe(true);
  });

  for (const viewport of [{ name: "desktop", width: 1440, height: 900 }, { name: "mobile", width: 390, height: 844 }]) {
    test(`drives live break logs, coverage tip, Agent test-link and Loki gap fill ${viewport.name}`, async ({ page }) => {
      test.setTimeout(120_000);
      const errors: string[] = [];
      page.on("pageerror", (error) => errors.push(error.message));
      await page.setViewportSize(viewport);
      await page.emulateMedia({ colorScheme: "dark" });
      await page.addInitScript(() => localStorage.setItem("versus.theme", "dark"));
      const baseURL = process.env.E2E_BASE_URL || "http://127.0.0.1:18080";
      const secret = process.env.E2E_GATEWAY_SECRET || "";
      const resetRes = await fetch(`${baseURL}/api/admin/kubernetes/agent-settings`, {
        method: "PUT",
        headers: { "X-Gateway-Secret": secret, Origin: baseURL, "Content-Type": "application/json" },
        body: JSON.stringify({ learning: { logs: true, metrics: false, traces: true, scope: { include: [namespace], exclude: [] } }, break_logs: { linked_source: "", fields: {} } }),
      });
      expect(resetRes.status).toBe(200);

      const timelineURL = `/agent/kubernetes?view=timeline&root_uid=${rootUID}&namespace=${namespace}&center=${encodeURIComponent(brokeAt)}&logs_from=${encodeURIComponent(windowFrom)}&logs_to=${encodeURIComponent(windowTo)}`;
      await openApp(page, timelineURL);
      const logs = page.getByRole("region", { name: "Logs at break", exact: true });
      const viewer = logs.getByRole("log");
      await expect(viewer).toContainText("payment client timeout");
      await expect(viewer).toContainText("<REDACTED:password>");
      await expect(viewer).not.toContainText("k13-capture-canary-value");
      await expect(logs.getByText("Captured at break").first()).toBeVisible();
      await logs.scrollIntoViewIfNeeded();
      await logs.getByRole("button", { name: "Log coverage tip" }).click();
      await expect(logs.getByText(/The container restarted more than once since the break/)).toBeVisible();
      await expect(logs.getByRole("link", { name: "Link a source in Agent tab" })).toBeVisible();
      await captureBreakState(page, `live-break-logs-${viewport.name}.png`, "dark", "L/F: live OSS + Postgres + Loki + Elasticsearch stack with fakekube cluster");

      await logs.getByRole("link", { name: "Link a source in Agent tab" }).click();
      const agent = page.getByRole("region", { name: "Kubernetes Agent" });
      await expect(agent.getByText("Not available yet.", { exact: true })).toBeVisible();
      await agent.getByRole("button", { name: "None" }).click();
      const picker = page.getByLabel("Configured log sources");
      await expect(picker).toBeVisible();
      await picker.getByRole("button", { name: /^qa-break-elasticsearch-missing/ }).click();
      const esForm = agent.getByRole("region", { name: /Elasticsearch mapping$/ });
      await esForm.getByRole("button", { name: "Test link" }).click();
      await expect(esForm.getByRole("alert")).toContainText("Source or workload not found. Check the source and cluster.");
      await expect(esForm.getByRole("button", { name: "Retry" })).toBeVisible();

      await agent.getByRole("button", { name: "qa-break-elasticsearch-missing" }).click();
      await picker.getByRole("button", { name: /^qa-break-loki/ }).click();
      const lokiForm = agent.getByRole("region", { name: /Loki mapping$/ });
      await lokiForm.getByRole("button", { name: "Test link" }).click();
      await expect(lokiForm).toContainText(/Found \d+ lines/);
      await expect(lokiForm.locator("pre")).toContainText("vis-checkout-6c8d4-x2k4q");
      await lokiForm.scrollIntoViewIfNeeded();
      await captureBreakState(page, `live-break-agent-${viewport.name}.png`, "dark", "L/F: live OSS + Postgres + Loki + Elasticsearch stack with fakekube cluster");
      const saved = page.waitForResponse((response) => response.request().method() === "PUT" && new URL(response.url()).pathname.endsWith("/agent-settings"));
      await lokiForm.getByRole("button", { name: "Use this source" }).click();
      expect((await saved).status()).toBe(200);
      await expect(lokiForm.getByText("Linked", { exact: true })).toBeVisible();

      await page.goto(timelineURL);
      await expect(viewer).toContainText("loki between restarts 0");
      await expect(logs.getByText("qa-break-loki").first()).toBeVisible();
      await expect(logs.getByText("Coverage: complete")).toBeVisible();
      await expect(logs.getByRole("button", { name: "Log coverage tip" })).toHaveCount(0);
      expect(errors).toEqual([]);
    });
  }
});

test.describe("Live K13 and K15 worst-case user acceptance", () => {
  test.skip(process.env.E2E_K13_K15_LIVE !== "1", "requires the live K13/K15 user-acceptance stack and local receiver");

  test("executes 100% real-user UI journey: default off, Warning enable, HA handoff, crash/restarts/deleted Pod/gaps, Timeline/Agent failure & Loki fill, incident/Ack/partial/on-call outcomes, deep links, disable, reload and mobile", async ({ page }, testInfo) => {
    test.setTimeout(480_000);
    const errors: string[] = [];
    const chatPostRequests: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    page.on("request", (req) => {
      const url = new URL(req.url());
      if (req.method() === "POST" && url.pathname.startsWith("/api/admin/chat/")) {
        chatPostRequests.push(`${req.method()} ${url.pathname}`);
      }
    });

    await page.emulateMedia({ colorScheme: "dark" });
    await page.addInitScript(() => localStorage.setItem("versus.theme", "dark"));

    const state = harnessRuntimeStateDir();
    const fakePort = Number(process.env.HARNESS_FAKEKUBE_PORT || 28643);
    const lokiPort = Number(process.env.HARNESS_LOKI_PORT || 28610);
    const receiverPort = Number(process.env.HARNESS_RECEIVER_HTTP_PORT || 28690);
    const haScript = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../../../plans/harness-run/tests/kubernetes/kubernetes-ha.sh");
    const ca = fs.readFileSync(path.join(state, "fakekube/ca.crt"));

    const upstream = (endpoint: string, body?: unknown) => new Promise<unknown>((resolve, reject) => {
      const encoded = body === undefined ? undefined : JSON.stringify(body);
      const call = https.request(`https://localhost:${fakePort}${endpoint}`, {
        ca,
        method: encoded ? "POST" : "GET",
        headers: encoded ? { "Content-Type": "application/json" } : {},
      }, (response) => {
        let text = "";
        response.on("data", (chunk) => { text += chunk; });
        response.on("end", () => response.statusCode === 200 ? resolve(JSON.parse(text)) : reject(new Error(`fakekube ${endpoint} HTTP ${response.statusCode} ${text}`)));
      });
      call.on("error", reject);
      call.end(encoded);
    });

    type ReceiverState = {
      emails: Array<{ mailFrom: string; rcptTo: string[]; headers: string; body: string; links: Array<{ label: string; url: string }> }>;
      lark: Array<{ text: string; links: Array<{ label: string; url: string }> }>;
      divertLark: Array<{ text: string; links: Array<{ label: string; url: string }> }>;
      msteams: Array<{ text: string; links: Array<{ label: string; url: string }> }>;
      servicenow: Array<{ table: string; hasBasicAuth: boolean; payload: { short_description?: string; correlation_id?: string } }>;
    };

    const receiver = async (endpoint = "/_receipts", method = "GET", body?: unknown): Promise<ReceiverState> => {
      const res = await fetch(`http://127.0.0.1:${receiverPort}${endpoint}`, {
        method,
        headers: body ? { "Content-Type": "application/json" } : {},
        body: body ? JSON.stringify(body) : undefined,
      });
      expect(res.status).toBe(200);
      return res.json() as Promise<ReceiverState>;
    };

    const haControl = (op: string, svc?: string) => {
      const args = [haScript, "--control", op, ...(svc ? [svc] : [])];
      return execFileSync("bash", args, { env: process.env as NodeJS.ProcessEnv, encoding: "utf8", timeout: 90_000 }).trim();
    };

    const second = (ms: number) => Math.floor(ms / 1000) * 1000;
    const iso = (ms: number) => new Date(ms).toISOString().replace(".000Z", "Z");
    const ns = "k13k15";

    await receiver("/_clear", "POST");
    await upstream("/_fake/mutate", { operation: "upsert", resource: "namespaces", namespace: "", name: ns, object: { kind: "Namespace", metadata: {} } });

    // Seed a pre-existing broken workload BEFORE alerts are enabled to verify no-backfill.
    const preBreakMs = second(Date.now()) - 120_000;
    await upstream("/_fake/mutate", {
      operation: "upsert", resource: "deployments", namespace: ns, name: "preexisting-api",
      object: {
        apiVersion: "apps/v1", kind: "Deployment",
        metadata: { name: "preexisting-api", namespace: ns, uid: "preexisting-root", generation: 1, creationTimestamp: iso(preBreakMs - 600_000), labels: { app: "preexisting-api" } },
        spec: { replicas: 2, selector: { matchLabels: { app: "preexisting-api" } }, template: { spec: { containers: [{ name: "api", image: "example.invalid/pre:v1" }] } } },
        status: { observedGeneration: 1, replicas: 2, readyReplicas: 1, availableReplicas: 1, updatedReplicas: 2 },
      },
    });
    await upstream("/_fake/mutate", {
      operation: "upsert", resource: "pods", namespace: ns, name: "preexisting-api-pod-0",
      object: {
        apiVersion: "v1", kind: "Pod",
        metadata: { name: "preexisting-api-pod-0", namespace: ns, uid: "preexisting-pod-0", creationTimestamp: iso(preBreakMs - 600_000), ownerReferences: [{ apiVersion: "apps/v1", kind: "Deployment", name: "preexisting-api", uid: "preexisting-root", controller: true }] },
        spec: { nodeName: "k13-node", containers: [{ name: "api", image: "example.invalid/pre:v1" }] },
        status: { phase: "Running", conditions: [{ type: "Ready", status: "False", lastTransitionTime: iso(preBreakMs) }], containerStatuses: [{ name: "api", ready: false, restartCount: 4, state: { waiting: { reason: "CrashLoopBackOff", message: "preexisting-canary-msg" } } }] },
      },
    });

    // 1. Open Issues tab as a real user at desktop 1440x1000 and verify alerts default off.
    await page.setViewportSize({ width: 1440, height: 1000 });
    await openApp(page, "/agent/kubernetes?view=issues");
    const issues = page.getByRole("region", { name: "Kubernetes issues", exact: true });
    await expect(issues).toBeVisible();
    await expect(issues.getByRole("article", { name: "Deployment preexisting-api", exact: true }).first()).toBeVisible({ timeout: 60_000 });

    const alertSwitch = issues.getByRole("switch", { name: "Alert on new issues", exact: true });
    await expect(alertSwitch).toBeVisible();
    await expect(alertSwitch).toBeEnabled();
    await expect(alertSwitch).toHaveAttribute("aria-checked", "false");
    await expect(issues.getByRole("group", { name: "Issue alert level", exact: true })).toHaveCount(0);
    await expect(issues.getByText("Sends through your alert channels", { exact: true })).toBeVisible();
    const channelDocs = issues.getByRole("link", { name: "Channel settings", exact: true });
    await expect(channelDocs).toHaveAttribute("href", "https://docs.versusincident.com/#/agent/channels");
    await expect(channelDocs).toHaveAttribute("target", "_blank");
    await expect(channelDocs).toHaveAttribute("rel", "noopener noreferrer");
    await expect(issues.getByText("No alert channel is on", { exact: true })).toHaveCount(0);
    await expect(issues.getByText("Set public_host to add Timeline and agent links", { exact: true })).toHaveCount(0);
    await captureBreakState(page, "live-k15-alerts-off-desktop.png", "dark", "L: live OSS + fakekube + local SMTP/Lark/MSTeams/ServiceNow stack");

    await page.setViewportSize({ width: 390, height: 844 });
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
    await captureBreakState(page, "live-k15-alerts-off-mobile.png", "dark", "L: live OSS + fakekube + local SMTP/Lark/MSTeams/ServiceNow stack");
    await page.setViewportSize({ width: 1440, height: 1000 });

    // Confirm zero notifications while off.
    const initialReceipts = await receiver();
    expect(initialReceipts.emails).toHaveLength(0);
    expect(initialReceipts.lark).toHaveLength(0);
    expect(initialReceipts.msteams).toHaveLength(0);
    expect(initialReceipts.servicenow).toHaveLength(0);

    // 2. Enable alerts in the UI -> defaults to Critical -> switch to Warning -> reload & verify persistence.
    await alertSwitch.click();
    await expect(page.getByText("Alerts on. Issues that break from now on will alert.", { exact: true })).toBeVisible();
    await expect(alertSwitch).toHaveAttribute("aria-checked", "true");
    const levelGroup = issues.getByRole("group", { name: "Issue alert level", exact: true });
    await expect(levelGroup).toBeVisible();
    const criticalBtn = levelGroup.getByRole("button", { name: "Critical", exact: true });
    const warningBtn = levelGroup.getByRole("button", { name: "Warning", exact: true });
    await expect(criticalBtn).toHaveAttribute("aria-pressed", "true");
    await expect(warningBtn).toHaveAttribute("aria-pressed", "false");
    await expect(issues.getByText("Critical sends critical issues only", { exact: true })).toBeVisible();
    await captureBreakState(page, "live-k15-alerts-critical-desktop.png", "dark", "L: live OSS + fakekube + local SMTP/Lark/MSTeams/ServiceNow stack");

    await warningBtn.click();
    await expect(page.getByText("Alert level saved.", { exact: true })).toBeVisible();
    await expect(warningBtn).toHaveAttribute("aria-pressed", "true");
    await expect(criticalBtn).toHaveAttribute("aria-pressed", "false");
    await expect(issues.getByText("Warning sends warning and critical issues", { exact: true })).toBeVisible();
    await captureBreakState(page, "live-k15-alerts-warning-desktop.png", "dark", "L: live OSS + fakekube + local SMTP/Lark/MSTeams/ServiceNow stack");

    await page.setViewportSize({ width: 390, height: 844 });
    const mobileAlertGeometry = await issues.evaluate(() => {
      const sw = document.querySelector<HTMLElement>('[role="switch"][aria-label="Alert on new issues"]')!;
      const row = sw.parentElement!;
      const grp = document.querySelector<HTMLElement>('[role="group"][aria-label="Issue alert level"]')!;
      const btns = [...grp.querySelectorAll<HTMLElement>("button")];
      const rBox = row.getBoundingClientRect();
      const gBox = grp.getBoundingClientRect();
      return {
        wrapped: gBox.top >= rBox.bottom - 1 && Math.abs(gBox.width - rBox.width) <= 2,
        equalButtons: Math.abs(btns[0].getBoundingClientRect().width - btns[1].getBoundingClientRect().width) <= 2,
        noOverflow: document.documentElement.scrollWidth <= window.innerWidth,
      };
    });
    expect(mobileAlertGeometry).toEqual({ wrapped: true, equalButtons: true, noOverflow: true });
    await captureBreakState(page, "live-k15-alerts-warning-mobile.png", "dark", "L: live OSS + fakekube + local SMTP/Lark/MSTeams/ServiceNow stack");
    await page.setViewportSize({ width: 1440, height: 1000 });

    // Reload to prove persistence.
    await page.reload();
    await expect(issues).toBeVisible();
    await expect(alertSwitch).toHaveAttribute("aria-checked", "true");
    await expect(warningBtn).toHaveAttribute("aria-pressed", "true");

    // 3. Edit Agent tab settings in UI and verify saving Agent settings does NOT clobber issue alerts.
    await page.getByRole("tab", { name: "Agent", exact: true }).click();
    const agentRegion = page.getByRole("region", { name: "Kubernetes Agent", exact: true });
    await expect(agentRegion.getByText("Not available yet.", { exact: true })).toBeVisible();
    await expect(agentRegion.getByRole("button", { name: "Edit scope", exact: true })).toBeDisabled();

    await page.getByRole("tab", { name: "Issues", exact: true }).click();
    await page.reload();
    await expect(alertSwitch).toHaveAttribute("aria-checked", "true");
    await expect(warningBtn).toHaveAttribute("aria-pressed", "true");

    // Seed healthy Deployment checkout + ReplicaSets + Pods BEFORE enabling alerts so their owner chain is indexed.
    const rootUID = "k13k15-checkout";
    const rsV41UID = "k13k15-checkout-rs-v41";
    const rsV42UID = "k13k15-checkout-rs-v42";
    const crashPodName = "checkout-6c8d4-x2k4q";
    const crashPodUID = "k13k15-checkout-crash-pod";
    const delPodName = "checkout-6c8d4-d3e4f";
    const delPodUID = "k13k15-checkout-del-pod";
    const crashLogPath = `/api/v1/namespaces/${ns}/pods/${crashPodName}/log`;
    const delLogPath = `/api/v1/namespaces/${ns}/pods/${delPodName}/log`;
    const seedMs = second(Date.now());
    const checkoutDeployment = (availableReplicas: number, createdMs = seedMs - 1_800_000) => ({
      apiVersion: "apps/v1", kind: "Deployment",
      metadata: { name: "checkout", namespace: ns, uid: rootUID, generation: 2, creationTimestamp: iso(createdMs), labels: { app: "checkout" } },
      spec: { replicas: 4, selector: { matchLabels: { app: "checkout" } }, template: { metadata: { labels: { app: "checkout" } }, spec: { containers: [{ name: "api", image: "example.invalid/checkout:v42" }] } } },
      status: { observedGeneration: 2, replicas: 4, readyReplicas: availableReplicas, availableReplicas, updatedReplicas: 4 },
    });
    const makeCheckoutPod = (name: string, uid: string, restarts: number, termMs: number | null, brokeAtStr: string, reason = "OOMKilled", exitCode = 137) => ({
      apiVersion: "v1", kind: "Pod",
      metadata: { name, namespace: ns, uid, creationTimestamp: iso(seedMs - 240_000), labels: { app: "checkout" }, ownerReferences: [{ apiVersion: "apps/v1", kind: "ReplicaSet", name: "checkout-6c8d4", uid: rsV42UID, controller: true }] },
      spec: { nodeName: "k13-node", containers: [{ name: "api", image: "example.invalid/checkout:v42" }] },
      status: {
        phase: "Running",
        conditions: [{ type: "Ready", status: restarts > 0 ? "False" : "True", lastTransitionTime: brokeAtStr }],
        containerStatuses: [{
          name: "api", ready: restarts === 0, restartCount: restarts,
          state: restarts > 0 ? { waiting: { reason: "CrashLoopBackOff", message: "raw-termination-canary-msg" } } : { running: { startedAt: brokeAtStr } },
          ...(termMs ? { lastState: { terminated: { reason, exitCode, finishedAt: iso(termMs), message: "raw-termination-canary-msg" } } } : {}),
        }],
      },
    });

    await upstream("/_fake/mutate", { operation: "upsert", resource: "deployments", namespace: ns, name: "checkout", object: checkoutDeployment(4) });
    await upstream("/_fake/mutate", {
      operation: "upsert", resource: "replicasets", namespace: ns, name: "checkout-v41",
      object: {
        apiVersion: "apps/v1", kind: "ReplicaSet",
        metadata: { name: "checkout-v41", namespace: ns, uid: rsV41UID, creationTimestamp: iso(seedMs - 900_000), labels: { app: "checkout" }, annotations: { "deployment.kubernetes.io/revision": "1" }, ownerReferences: [{ apiVersion: "apps/v1", kind: "Deployment", name: "checkout", uid: rootUID, controller: true }] },
        spec: { replicas: 0, template: { spec: { containers: [{ name: "api", image: "example.invalid/checkout:v41" }] } } },
        status: { replicas: 0, readyReplicas: 0 },
      },
    });
    await upstream("/_fake/mutate", {
      operation: "upsert", resource: "replicasets", namespace: ns, name: "checkout-6c8d4",
      object: {
        apiVersion: "apps/v1", kind: "ReplicaSet",
        metadata: { name: "checkout-6c8d4", namespace: ns, uid: rsV42UID, creationTimestamp: iso(seedMs - 240_000), labels: { app: "checkout" }, annotations: { "deployment.kubernetes.io/revision": "2" }, ownerReferences: [{ apiVersion: "apps/v1", kind: "Deployment", name: "checkout", uid: rootUID, controller: true }] },
        spec: { replicas: 4, template: { spec: { containers: [{ name: "api", image: "example.invalid/checkout:v42" }] } } },
        status: { replicas: 4, readyReplicas: 4 },
      },
    });
    await upstream("/_fake/mutate", { operation: "upsert", resource: "pods", namespace: ns, name: crashPodName, object: makeCheckoutPod(crashPodName, crashPodUID, 0, null, iso(seedMs - 240_000)) });
    await upstream("/_fake/mutate", { operation: "upsert", resource: "pods", namespace: ns, name: delPodName, object: makeCheckoutPod(delPodName, delPodUID, 0, null, iso(seedMs - 240_000)) });
    await upstream("/_fake/mutate", {
      operation: "upsert", resource: "deployments", namespace: ns, name: "payments-worker",
      object: {
        apiVersion: "apps/v1", kind: "Deployment",
        metadata: { name: "payments-worker", namespace: ns, uid: "k13k15-payments-worker", generation: 1, creationTimestamp: iso(seedMs - 600_000), labels: { app: "payments-worker" } },
        spec: { replicas: 2, selector: { matchLabels: { app: "payments-worker" } }, template: { spec: { containers: [{ name: "worker", image: "example.invalid/payments:v1" }] } } },
        status: { observedGeneration: 1, replicas: 2, readyReplicas: 2, availableReplicas: 2, updatedReplicas: 2 },
      },
    });
    await upstream("/_fake/mutate", {
      operation: "upsert", resource: "pods", namespace: ns, name: "payments-worker-pod-0",
      object: {
        apiVersion: "v1", kind: "Pod",
        metadata: { name: "payments-worker-pod-0", namespace: ns, uid: "k13k15-payments-pod-0", creationTimestamp: iso(seedMs - 600_000), ownerReferences: [{ apiVersion: "apps/v1", kind: "Deployment", name: "payments-worker", uid: "k13k15-payments-worker", controller: true }] },
        spec: { nodeName: "k13-node", containers: [{ name: "worker", image: "example.invalid/payments:v1" }] },
        status: { phase: "Running", conditions: [{ type: "Ready", status: "True", lastTransitionTime: iso(seedMs - 600_000) }], containerStatuses: [{ name: "worker", ready: true, restartCount: 0, state: { running: { startedAt: iso(seedMs - 600_000) } } }] },
      },
    });

    // Wait until the healthy owner chain is indexed in the continuous capture store.
    const indexedAfter = new Date().toISOString();
    await expect.poll(() => {
      const rows = JSON.parse(haControl("rows")) as Array<{ snapshot: { coverage?: { Pod?: { to?: string }; Deployment?: { to?: string }; ReplicaSet?: { to?: string } } } }>;
      return rows.some((r) => (r.snapshot.coverage?.Pod?.to ?? "") > indexedAfter && (r.snapshot.coverage?.Deployment?.to ?? "") > indexedAfter && (r.snapshot.coverage?.ReplicaSet?.to ?? "") > indexedAfter);
    }, { timeout: 90_000, intervals: [1000, 2000] }).toBe(true);

    // Wait a moment so any new break timestamp is strictly after enabled_at and <= server now.
    await page.waitForTimeout(3000);
    const afterEnableReceipts = await receiver();
    expect(afterEnableReceipts.emails).toHaveLength(0);

    const nowMs = second(Date.now());
    const brokeMs = nowMs - 2000;
    const firstTermMs = nowMs - 1000;
    const brokeAtISO = iso(brokeMs);
    const windowFromISO = iso(brokeMs - 200_000);

    const crashFirstLines = Array.from({ length: 12 }, (_, index) => ({
      at: iso(firstTermMs - 180_000 + index * 10_000),
      text: index === 2
        ? "ERROR payment client timeout Authorization: Bearer k13-live-secret-canary"
        : index % 3 === 0
          ? `ERROR payment client timeout attempt ${index}`
          : `INFO starting checkout worker pool ${index}`,
    }));
    const delLines = Array.from({ length: 8 }, (_, index) => ({
      at: iso(firstTermMs - 140_000 + index * 10_000),
      text: index % 2 === 0 ? `WARN deleted replica retry ${index}` : `INFO deleted pod request ${index}`,
    }));

    await upstream("/_fake/log-fixtures", { path: crashLogPath, container: "api", previous: true, lines: crashFirstLines });
    await upstream("/_fake/log-fixtures", { path: delLogPath, container: "api", previous: true, lines: delLines });
    await upstream("/_fake/mutate", { operation: "upsert", resource: "pods", namespace: ns, name: crashPodName, object: makeCheckoutPod(crashPodName, crashPodUID, 1, firstTermMs, brokeAtISO, "OOMKilled", 137) });
    await upstream("/_fake/mutate", { operation: "upsert", resource: "pods", namespace: ns, name: delPodName, object: makeCheckoutPod(delPodName, delPodUID, 1, firstTermMs, brokeAtISO, "Error", 1) });
    await upstream("/_fake/mutate", { operation: "upsert", resource: "deployments", namespace: ns, name: "checkout", object: checkoutDeployment(3) });

    // Wait for continuous capture to emit the Warning alert to Email (SMTP), Lark, and MS Teams AND capture break logs.
    await expect.poll(async () => {
      const r = await receiver();
      return r.emails.length === 1 && r.lark.length === 1 && r.msteams.length === 1;
    }, { timeout: 90_000, intervals: [1000, 2000] }).toBe(true);

    const warningReceipts = await receiver();
    expect(warningReceipts.servicenow).toHaveLength(0);
    const warningEmail = warningReceipts.emails[0];
    expect(warningEmail.body).toContain("🟡");
    expect(warningEmail.body).toContain("Kubernetes issue");
    expect(warningEmail.body).toContain("Deployment checkout is crash looping");
    expect(warningEmail.body).toContain("<b>Severity:</b> warning");
    expect(warningEmail.body).toContain("<b>Cluster:</b> fakekube");
    expect(warningEmail.body).toContain(`<b>Namespace:</b> ${ns}`);
    expect(warningEmail.body).toContain("What we saw");
    expect(warningEmail.body).toContain("Exit 137 OOMKilled");
    expect(warningEmail.body).toContain("Changes before the break");
    expect(warningEmail.body).toContain("Image changed");
    expect(warningEmail.body).not.toContain("k13-live-secret-canary");
    expect(warningEmail.body).not.toContain("raw-termination-canary-msg");
    expect(warningEmail.body).not.toContain("payment client timeout");
    expect(warningEmail.body).not.toContain("&amp;amp;");
    expect(warningEmail.links.map((l) => l.label)).toEqual(["Open timeline", "Investigate with DevOps Agent"]);
    expect(warningReceipts.lark[0].links.map((l) => l.label)).toEqual(["Open timeline", "Investigate with DevOps Agent"]);
    expect(warningReceipts.msteams[0].links.map((l) => l.label)).toEqual(["Open timeline", "Investigate with DevOps Agent"]);

    // 5. Forced two-replica capture-owner handoff: verify zero duplicate notifications across handoff.
    haControl("recreate", "oss-peer");
    const rowsBeforeHandoff = JSON.parse(haControl("rows")) as Array<{ key: string; snapshot: { lease?: { owner?: string }; coverage?: { Pod?: { to?: string } } } }>;
    const leaseBefore = rowsBeforeHandoff.find((r) => r.snapshot.lease?.owner);
    expect(leaseBefore?.snapshot.lease?.owner).toBeTruthy();
    const ownerBefore = leaseBefore!.snapshot.lease!.owner!;

    // Stop oss-peer first; if oss-peer was owner, oss takes over. If oss was owner, stop oss so oss-peer takes over, then start oss back.
    haControl("stop", "oss-peer");
    await page.waitForTimeout(2000);
    const currentRows = JSON.parse(haControl("rows")) as typeof rowsBeforeHandoff;
    const currentOwner = currentRows.find((r) => r.key === leaseBefore!.key)?.snapshot.lease?.owner;
    if (currentOwner === ownerBefore) {
      // oss owned the lease; start oss-peer, stop oss so oss-peer takes over and advances coverage, then start oss.
      haControl("start", "oss-peer");
      haControl("stop", "oss");
      await expect.poll(() => {
        const rows = JSON.parse(haControl("rows")) as typeof rowsBeforeHandoff;
        const row = rows.find((r) => r.key === leaseBefore!.key);
        const owner = row?.snapshot.lease?.owner;
        const podTo = row?.snapshot.coverage?.Pod?.to ?? "";
        return Boolean(owner && owner !== ownerBefore && podTo > (leaseBefore!.snapshot.coverage?.Pod?.to ?? ""));
      }, { timeout: 90_000, intervals: [1000, 2000] }).toBe(true);
      haControl("start", "oss");
    } else {
      await expect.poll(() => {
        const rows = JSON.parse(haControl("rows")) as typeof rowsBeforeHandoff;
        const row = rows.find((r) => r.key === leaseBefore!.key);
        const owner = row?.snapshot.lease?.owner;
        const podTo = row?.snapshot.coverage?.Pod?.to ?? "";
        return Boolean(owner && owner !== ownerBefore && podTo > (leaseBefore!.snapshot.coverage?.Pod?.to ?? ""));
      }, { timeout: 90_000, intervals: [1000, 2000] }).toBe(true);
      haControl("start", "oss-peer");
    }
    await expect.poll(async () => {
      try {
        const res = await fetch(`${process.env.E2E_BASE_URL || "http://127.0.0.1:28680"}/healthz`);
        return res.status === 200;
      } catch { return false; }
    }, { timeout: 30_000, intervals: [500, 1000] }).toBe(true);

    const handoffReceipts = await receiver();
    expect(handoffReceipts.emails).toHaveLength(1);
    expect(handoffReceipts.lark).toHaveLength(1);
    expect(handoffReceipts.msteams).toHaveLength(1);

    // 6. Delete checkout-del-pod, trigger second restart on checkout-crash-pod, and escalate checkout to Critical (0 of 3 ready).
    // Wait until initial break-log snapshots for both Pods are persisted before deleting/restarting.
    await expect.poll(() => {
      const rows = JSON.parse(haControl("rows")) as Array<{ snapshot: { break_logs?: { snapshots?: Array<{ pod_uid: string }> } } }>;
      const snaps = rows.flatMap((r) => r.snapshot.break_logs?.snapshots ?? []);
      return snaps.some((s) => s.pod_uid === crashPodUID) && snaps.some((s) => s.pod_uid === delPodUID);
    }, { timeout: 90_000, intervals: [1000, 2000] }).toBe(true);

    await upstream("/_fake/mutate", { operation: "delete", resource: "pods", namespace: ns, name: delPodName });
    const secondTermMs = second(Date.now()) - 2000;
    const windowToISO = iso(secondTermMs);
    const secondLines = Array.from({ length: 6 }, (_, index) => ({
      at: iso(secondTermMs - 25_000 + index * 4000),
      text: `INFO second restart line ${index}`,
    }));
    await upstream("/_fake/log-fixtures", { path: crashLogPath, container: "api", previous: true, lines: secondLines });
    await upstream("/_fake/mutate", { operation: "upsert", resource: "pods", namespace: ns, name: crashPodName, object: makeCheckoutPod(crashPodName, crashPodUID, 3, secondTermMs, brokeAtISO, "OOMKilled", 137) });
    await upstream("/_fake/mutate", { operation: "upsert", resource: "deployments", namespace: ns, name: "checkout", object: checkoutDeployment(0) });

    // Wait for Critical escalation notification on the same episode AND second termination break-log snapshot.
    await expect.poll(async () => {
      const r = await receiver();
      return r.emails.length === 2 && r.lark.length === 2 && r.msteams.length === 2;
    }, { timeout: 90_000, intervals: [1000, 2000] }).toBe(true);

    const criticalReceipts = await receiver();
    const criticalEmail = criticalReceipts.emails[1];
    expect(criticalEmail.body).toContain("🔴");
    expect(criticalEmail.body).toContain("<b>Severity:</b> critical");
    expect(criticalEmail.links.map((l) => l.label)).toEqual(["Acknowledge", "Open timeline", "Investigate with DevOps Agent"]);
    expect(criticalReceipts.lark[1].links.map((l) => l.label)).toEqual(["Acknowledge", "Open timeline", "Investigate with DevOps Agent"]);
    expect(criticalReceipts.msteams[1].links.map((l) => l.label)).toEqual(["Acknowledge", "Open timeline", "Investigate with DevOps Agent"]);

    const ackURL = criticalEmail.links[0].url;
    const timelineURL = criticalEmail.links[1].url;
    const agentURL = criticalEmail.links[2].url;
    expect(ackURL).toContain("/api/ack/");
    expect(timelineURL).toContain("view=timeline");
    expect(agentURL).toContain("/agent/chat?");
    expect(agentURL).toContain("issue=pod.crash_loop");

    // Visit AckURL in the real browser to acknowledge the critical incident before the 1-minute on-call wait expires.
    await page.goto(ackURL);
    await expect(page.locator("body")).toContainText('"status":"success"');

    // Open /incidents?status=acked in the real UI and inspect the persisted, acknowledged incident.
    await openApp(page, "/incidents?status=acked");
    const checkoutIncidentLink = page.getByRole("link", { name: /Deployment checkout is crash looping/i }).first();
    await expect(checkoutIncidentLink).toBeVisible();
    await checkoutIncidentLink.click();
    await expect(page.getByText("acknowledged", { exact: true }).first()).toBeVisible();
    await expect(page.getByText("notified email", { exact: true })).toBeVisible();
    await expect(page.getByText("notified lark", { exact: true })).toBeVisible();
    await expect(page.getByText("notified msteams", { exact: true })).toBeVisible();
    await expect(page.getByText("No summary provided.", { exact: true })).toHaveCount(0);
    await expect(page.getByText("checkout", { exact: true })).toBeVisible();
    await expect(page.getByRole("region", { name: "Evidence", exact: true })).toContainText("Exit 137 OOMKilled");
    await expect(page.getByRole("navigation", { name: "Kubernetes issue links", exact: true }).getByRole("link", { name: "Timeline", exact: true })).toBeVisible();
    await expect(page.getByText("kubernetes:pod.crash_loop", { exact: true })).toBeVisible();
    await expect(page.locator('a[href^="/agent/logs/"]')).toHaveCount(0);

    // 7. Worst-case partial channel failure (MS Teams 500) + unacknowledged critical on-call paging to ServiceNow.
    await receiver("/_control/fault", "POST", { target: "msteams", status: 500 });
    await page.waitForTimeout(2000);
    const partialBreakMs = second(Date.now()) - 1000;
    await upstream("/_fake/mutate", {
      operation: "upsert", resource: "pods", namespace: ns, name: "payments-worker-pod-0",
      object: {
        apiVersion: "v1", kind: "Pod",
        metadata: { name: "payments-worker-pod-0", namespace: ns, uid: "k13k15-payments-pod-0", creationTimestamp: iso(partialBreakMs - 600_000), ownerReferences: [{ apiVersion: "apps/v1", kind: "Deployment", name: "payments-worker", uid: "k13k15-payments-worker", controller: true }] },
        spec: { nodeName: "k13-node", containers: [{ name: "worker", image: "example.invalid/payments:v1" }] },
        status: { phase: "Running", conditions: [{ type: "Ready", status: "False", lastTransitionTime: iso(partialBreakMs) }], containerStatuses: [{ name: "worker", ready: false, restartCount: 5, state: { waiting: { reason: "CrashLoopBackOff" } }, lastState: { terminated: { reason: "Error", exitCode: 1, finishedAt: iso(partialBreakMs) } } }] },
      },
    });
    await upstream("/_fake/mutate", {
      operation: "upsert", resource: "deployments", namespace: ns, name: "payments-worker",
      object: {
        apiVersion: "apps/v1", kind: "Deployment",
        metadata: { name: "payments-worker", namespace: ns, uid: "k13k15-payments-worker", generation: 1, creationTimestamp: iso(partialBreakMs - 600_000), labels: { app: "payments-worker" } },
        spec: { replicas: 2, selector: { matchLabels: { app: "payments-worker" } }, template: { spec: { containers: [{ name: "worker", image: "example.invalid/payments:v1" }] } } },
        status: { observedGeneration: 1, replicas: 2, readyReplicas: 0, availableReplicas: 0, updatedReplicas: 2 },
      },
    });

    await expect.poll(async () => {
      const r = await receiver();
      return r.emails.length === 3 && r.lark.length === 3;
    }, { timeout: 90_000, intervals: [1000, 2000] }).toBe(true);
    const partialReceipts = await receiver();
    expect(partialReceipts.msteams).toHaveLength(2);
    await receiver("/_control/fault", "POST", { target: "msteams", status: 0 });

    await page.goto("/incidents?status=open");
    const paymentsIncidentLink = page.getByRole("link", { name: /Deployment payments-worker is crash looping/i }).first();
    await expect(paymentsIncidentLink).toBeVisible();
    await page.getByRole("button", { name: "Show notification failure detail", exact: true }).first().click();
    await expect(page.getByText(/Partial delivery:.*MS Teams API returned 500/i)).toBeVisible();
    await paymentsIncidentLink.click();
    await expect(page.getByText("partial", { exact: true }).first()).toBeVisible();
    await expect(page.getByText("notified email", { exact: true })).toBeVisible();
    await expect(page.getByText("notified lark", { exact: true })).toBeVisible();
    await expect(page.getByText(/MS Teams API returned 500/i).first()).toBeVisible();
    const paymentsIncidentID = new URL(page.url()).pathname.split("/").pop()!;
    const paymentsDetailRes = await page.request.get(`/api/admin/incidents/${paymentsIncidentID}`);
    expect(paymentsDetailRes.status()).toBe(200);
    const paymentsDetail = await paymentsDetailRes.json();
    expect(paymentsDetail.notify_status).toBe("partial");
    expect(paymentsDetail.notify_error).toMatch(/MS Teams API returned 500/i);

    // 8. Navigate via the delivered TimelineURL deep link and drive K13 Timeline + Logs at break + Agent tab in the UI.
    await expect.poll(() => {
      const rows = JSON.parse(haControl("rows")) as Array<{ snapshot: { break_logs?: { snapshots?: Array<{ pod_uid: string }> } } }>;
      const snaps = rows.flatMap((r) => r.snapshot.break_logs?.snapshots ?? []);
      return snaps.filter((s) => s.pod_uid === crashPodUID).length;
    }, { timeout: 90_000, intervals: [1000, 2000] }).toBe(2);

    const timelineTarget = new URL(timelineURL);
    timelineTarget.searchParams.set("logs_from", windowFromISO);
    timelineTarget.searchParams.set("logs_to", windowToISO);
    await page.goto(timelineTarget.pathname + timelineTarget.search);

    const lane = page.getByRole("region", { name: "Timeline lane checkout", exact: true });
    await expect(lane).toBeVisible();
    const chart = lane.getByRole("region", { name: "Resource timeline chart", exact: true });
    await expect(chart.locator('[data-marker="change"]').first()).toBeVisible();
    await expect(chart.locator('[data-marker="crash"]').first()).toBeVisible();

    const logs = page.getByRole("region", { name: "Logs at break", exact: true });
    const viewer = logs.getByRole("log");
    await expect(viewer).toContainText("payment client timeout");
    await expect(viewer).toContainText("deleted replica retry");
    await expect(viewer).toContainText("<REDACTED");
    await expect(viewer).not.toContainText("k13-live-secret-canary");
    await expect(logs.getByText("Captured at break").first()).toBeVisible();

    await logs.scrollIntoViewIfNeeded();
    await logs.getByRole("button", { name: "Log coverage tip" }).click();
    await expect(logs.getByText(/The container restarted more than once since the break/)).toBeVisible();
    await expect(logs.getByRole("link", { name: "Link a source in Agent tab" })).toBeVisible();
    await captureBreakState(page, "live-k13-k15-timeline-desktop.png", "dark", "L: live OSS + fakekube + Postgres + Loki + Elasticsearch stack");

    await page.setViewportSize({ width: 390, height: 844 });
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
    await expect(page.getByRole("toolbar", { name: "Break log actions" })).toBeInViewport();
    await captureBreakState(page, "live-k13-k15-timeline-mobile.png", "dark", "L: live OSS + fakekube + Postgres + Loki + Elasticsearch stack");
    await page.setViewportSize({ width: 1440, height: 1000 });

    // Exercise crash marker popover -> Logs before crash & Open live logs drawer in UI.
    await chart.locator('[data-marker="crash"]').last().click();
    const crashPopover = page.getByRole("region", { name: "Timeline event details", exact: true });
    await expect(crashPopover.getByRole("button", { name: "Logs before crash" }).first()).toBeVisible();
    await crashPopover.getByRole("button", { name: "Logs before crash" }).first().click();
    await expect(page).toHaveURL(/logs_mode=previous/);
    await logs.getByRole("button", { name: "Open live logs" }).click();
    const drawer = page.getByRole("dialog", { name: "Details panel" });
    await expect(drawer.getByRole("region", { name: "Workload live logs" })).toBeVisible();
    await drawer.getByRole("button", { name: "Close panel" }).click();
    await expect(drawer).toHaveCount(0);

    // Return to full break window and follow "Link a source in Agent tab".
    await page.goto(timelineTarget.pathname + timelineTarget.search);
    await logs.getByRole("button", { name: "Log coverage tip" }).click();
    await logs.getByRole("link", { name: "Link a source in Agent tab" }).click();

    const agent = page.getByRole("region", { name: "Kubernetes Agent", exact: true });
    await expect(agent).toBeVisible();
    await agent.getByRole("button", { name: "None" }).click();
    const picker = page.getByLabel("Configured log sources");
    await expect(picker).toBeVisible();
    await picker.getByRole("button", { name: /^qa-break-elasticsearch-missing/ }).click();
    const esForm = agent.getByRole("region", { name: /Elasticsearch mapping$/ });
    await esForm.getByRole("button", { name: "Test link" }).click();
    await expect(esForm.getByRole("alert")).toContainText("Source or workload not found. Check the source and cluster.");

    // Test invalid mapping field validation in UI.
    const nsFieldInput = esForm.getByLabel(/Namespace field/i);
    await nsFieldInput.fill("bad field!");
    await expect(esForm.getByRole("alert").first()).toContainText("Use distinct namespace, pod and container names");
    await expect(esForm.getByRole("button", { name: "Use this source" })).toBeDisabled();
    await nsFieldInput.fill("kubernetes.namespace_name");

    // Save the failing Elasticsearch source and verify Timeline keeps partial coverage and tip.
    await esForm.getByRole("button", { name: "Use this source" }).click();
    await expect(esForm.getByText("Linked", { exact: true })).toBeVisible();
    await page.goto(timelineTarget.pathname + timelineTarget.search);
    await expect(logs.getByText("Coverage: partial")).toBeVisible();
    await expect(logs.getByRole("button", { name: "Log coverage tip" })).toBeVisible();

    // Seed between-restarts gap lines into real Loki, link qa-break-loki in UI, and verify gap closes.
    const lokiEntries = [
      ...Array.from({ length: 6 }, (_, index) => ({ at: iso(firstTermMs + 3000 + index * 3000), text: `WARN loki between restarts ${index}` })),
      ...Array.from({ length: 4 }, (_, index) => ({ at: iso(second(Date.now()) - 20_000 + index * 3000), text: `INFO loki recent test link ${index}` })),
    ];
    const pushRes = await fetch(`http://127.0.0.1:${lokiPort}/loki/api/v1/push`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ streams: [{ stream: { job: "k8s-break-logs", namespace: ns, pod: crashPodName, container: "api" }, values: lokiEntries.map((entry) => [`${Date.parse(entry.at)}000000`, entry.text]) }] }),
    });
    expect(pushRes.status).toBeGreaterThanOrEqual(200);
    expect(pushRes.status).toBeLessThan(300);
    await expect.poll(async () => {
      const q = encodeURIComponent(`{job="k8s-break-logs",pod="${crashPodName}"}`);
      const res = await fetch(`http://127.0.0.1:${lokiPort}/loki/api/v1/query_range?query=${q}&start=${(firstTermMs - 600_000) * 1e6}&end=${Date.now() * 1e6}&limit=100`);
      const data = await res.json();
      return data.data.result.reduce((sum: number, s: { values: unknown[] }) => sum + s.values.length, 0) === lokiEntries.length;
    }, { timeout: 60_000, intervals: [1000, 2000] }).toBe(true);

    await logs.getByRole("button", { name: "Log coverage tip" }).click();
    await logs.getByRole("link", { name: "Link a source in Agent tab" }).click();
    await agent.getByRole("button", { name: "qa-break-elasticsearch-missing" }).click();
    await expect(picker.getByRole("button", { name: /^qa-break-elasticsearch-missing/ })).toContainText("failing");
    await picker.getByRole("button", { name: /^qa-break-loki/ }).click();
    const lokiForm = agent.getByRole("region", { name: /Loki mapping$/ });
    await lokiForm.getByRole("button", { name: "Test link" }).click();
    await expect(lokiForm).toContainText(/Found \d+ lines/);
    await lokiForm.getByRole("button", { name: "Use this source" }).click();
    await expect(lokiForm.getByText("Linked", { exact: true })).toBeVisible();

    await page.goto(timelineTarget.pathname + timelineTarget.search);
    await expect(viewer).toContainText("loki between restarts 0");
    await expect(viewer).toContainText("deleted replica retry");
    await expect(logs.getByText("qa-break-loki").first()).toBeVisible();
    await expect(logs.getByText("Coverage: complete")).toBeVisible();
    await expect(logs.getByRole("button", { name: "Log coverage tip" })).toHaveCount(0);

    // 9. Follow delivered AgentURL deep link into Chat (verify prefill and NO auto-send).
    const agentTarget = new URL(agentURL);
    await page.goto(agentTarget.pathname + agentTarget.search);
    const composer = page.getByRole("textbox", { name: "Message", exact: true });
    await expect(composer).toHaveValue("Why is Deployment checkout crash looping?");
    await expect(page.getByText(`${ns}/checkout`, { exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: "Send", exact: true })).toBeEnabled();
    expect(chatPostRequests).toEqual([]);

    // 10. Disable alerts in UI, reload/persistence, and verify new break does not alert.
    await page.goto("/agent/kubernetes?view=issues");
    await expect(alertSwitch).toHaveAttribute("aria-checked", "true");
    await alertSwitch.click();
    await expect(page.getByText("Alerts off.", { exact: true })).toBeVisible();
    await expect(alertSwitch).toHaveAttribute("aria-checked", "false");
    await page.reload();
    await expect(alertSwitch).toHaveAttribute("aria-checked", "false");
    await expect(issues.getByRole("group", { name: "Issue alert level", exact: true })).toHaveCount(0);

    const postDisableMs = second(Date.now()) - 1000;
    await upstream("/_fake/mutate", {
      operation: "upsert", resource: "deployments", namespace: ns, name: "post-disable-api",
      object: {
        apiVersion: "apps/v1", kind: "Deployment",
        metadata: { name: "post-disable-api", namespace: ns, uid: "k13k15-post-disable", generation: 1, creationTimestamp: iso(postDisableMs - 600_000), labels: { app: "post-disable-api" } },
        spec: { replicas: 2, selector: { matchLabels: { app: "post-disable-api" } }, template: { spec: { containers: [{ name: "api", image: "example.invalid/post:v1" }] } } },
        status: { observedGeneration: 1, replicas: 2, readyReplicas: 0, availableReplicas: 0, updatedReplicas: 2 },
      },
    });
    await upstream("/_fake/mutate", {
      operation: "upsert", resource: "pods", namespace: ns, name: "post-disable-pod-0",
      object: {
        apiVersion: "v1", kind: "Pod",
        metadata: { name: "post-disable-pod-0", namespace: ns, uid: "k13k15-post-disable-pod-0", creationTimestamp: iso(postDisableMs - 600_000), ownerReferences: [{ apiVersion: "apps/v1", kind: "Deployment", name: "post-disable-api", uid: "k13k15-post-disable", controller: true }] },
        spec: { nodeName: "k13-node", containers: [{ name: "api", image: "example.invalid/post:v1" }] },
        status: { phase: "Running", conditions: [{ type: "Ready", status: "False", lastTransitionTime: iso(postDisableMs) }], containerStatuses: [{ name: "api", ready: false, restartCount: 3, state: { waiting: { reason: "CrashLoopBackOff" } }, lastState: { terminated: { reason: "Error", exitCode: 1, finishedAt: iso(postDisableMs) } } }] },
      },
    });
    await expect(issues.getByRole("article", { name: "Deployment post-disable-api", exact: true }).first()).toBeVisible({ timeout: 60_000 });

    // Also wait for the 1-minute on-call wait window on the unacknowledged payments-worker critical incident to page ServiceNow.
    await expect.poll(async () => {
      const r = await receiver();
      return r.servicenow.length;
    }, { timeout: 90_000, intervals: [1000, 2000] }).toBe(1);

    const finalReceipts = await receiver();
    expect(finalReceipts.emails).toHaveLength(3);
    expect(finalReceipts.lark).toHaveLength(3);
    expect(finalReceipts.msteams).toHaveLength(2);
    expect(finalReceipts.servicenow).toHaveLength(1);
    expect(finalReceipts.servicenow[0].hasBasicAuth).toBe(true);
    expect(finalReceipts.servicenow[0].table).toBe("incident");
    expect(errors).toEqual([]);
    await testInfo.attach("live-k13-k15-receipts", { body: JSON.stringify(finalReceipts, null, 2), contentType: "application/json" });
  });

  test("verifies concurrent partial and stale-ready owner chain, partial channel delivery UI, Kubernetes vs log-pattern incident detail, and mobile timeline caption and handles", async ({ page }, testInfo) => {
    test.setTimeout(360_000);
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));

    await page.emulateMedia({ colorScheme: "dark" });
    await page.addInitScript(() => localStorage.setItem("versus.theme", "dark"));

    const state = harnessRuntimeStateDir();
    const fakePort = Number(process.env.HARNESS_FAKEKUBE_PORT || 28643);
    const receiverPort = Number(process.env.HARNESS_RECEIVER_HTTP_PORT || 28690);
    const haScript = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../../../plans/harness-run/tests/kubernetes/kubernetes-ha.sh");
    const ca = fs.readFileSync(path.join(state, "fakekube/ca.crt"));

    const upstream = (endpoint: string, body?: unknown) => new Promise<unknown>((resolve, reject) => {
      const encoded = body === undefined ? undefined : JSON.stringify(body);
      const call = https.request(`https://localhost:${fakePort}${endpoint}`, {
        ca,
        method: encoded ? "POST" : "GET",
        headers: encoded ? { "Content-Type": "application/json" } : {},
      }, (response) => {
        let text = "";
        response.on("data", (chunk) => { text += chunk; });
        response.on("end", () => response.statusCode === 200 ? resolve(JSON.parse(text)) : reject(new Error(`fakekube ${endpoint} HTTP ${response.statusCode} ${text}`)));
      });
      call.on("error", reject);
      call.end(encoded);
    });

    type ReceiverState = {
      emails: Array<{ mailFrom: string; rcptTo: string[]; headers: string; body: string; links: Array<{ label: string; url: string }> }>;
      lark: Array<{ text: string; links: Array<{ label: string; url: string }> }>;
      msteams: Array<{ text: string; links: Array<{ label: string; url: string }> }>;
      servicenow: Array<{ table: string; hasBasicAuth: boolean; payload: { short_description?: string; correlation_id?: string } }>;
    };

    const receiver = async (endpoint = "/_receipts", method = "GET", body?: unknown): Promise<ReceiverState> => {
      const res = await fetch(`http://127.0.0.1:${receiverPort}${endpoint}`, {
        method,
        headers: body ? { "Content-Type": "application/json" } : {},
        body: body ? JSON.stringify(body) : undefined,
      });
      expect(res.status).toBe(200);
      return res.json() as Promise<ReceiverState>;
    };

    const haControl = (op: string, svc?: string) => {
      const args = [haScript, "--control", op, ...(svc ? [svc] : [])];
      return execFileSync("bash", args, { env: process.env as NodeJS.ProcessEnv, encoding: "utf8", timeout: 90_000 }).trim();
    };

    const second = (ms: number) => Math.floor(ms / 1000) * 1000;
    const iso = (ms: number) => new Date(ms).toISOString().replace(".000Z", "Z");
    const ns = "k13k15";

    await receiver("/_clear", "POST");
    await upstream("/_fake/mutate", { operation: "upsert", resource: "namespaces", namespace: "", name: ns, object: { kind: "Namespace", metadata: {} } });

    const preBreakMs = second(Date.now()) - 120_000;
    await upstream("/_fake/mutate", {
      operation: "upsert", resource: "deployments", namespace: ns, name: "preexisting-api",
      object: {
        apiVersion: "apps/v1", kind: "Deployment",
        metadata: { name: "preexisting-api", namespace: ns, uid: "preexisting-root", generation: 1, creationTimestamp: iso(preBreakMs - 600_000), labels: { app: "preexisting-api" } },
        spec: { replicas: 2, selector: { matchLabels: { app: "preexisting-api" } }, template: { spec: { containers: [{ name: "api", image: "example.invalid/pre:v1" }] } } },
        status: { observedGeneration: 1, replicas: 2, readyReplicas: 1, availableReplicas: 1, updatedReplicas: 2 },
      },
    });
    await upstream("/_fake/mutate", {
      operation: "upsert", resource: "pods", namespace: ns, name: "preexisting-api-pod-0",
      object: {
        apiVersion: "v1", kind: "Pod",
        metadata: { name: "preexisting-api-pod-0", namespace: ns, uid: "preexisting-pod-0", creationTimestamp: iso(preBreakMs - 600_000), ownerReferences: [{ apiVersion: "apps/v1", kind: "Deployment", name: "preexisting-api", uid: "preexisting-root", controller: true }] },
        spec: { nodeName: "k13-node", containers: [{ name: "api", image: "example.invalid/pre:v1" }] },
        status: { phase: "Running", conditions: [{ type: "Ready", status: "False", lastTransitionTime: iso(preBreakMs) }], containerStatuses: [{ name: "api", ready: false, restartCount: 4, state: { waiting: { reason: "CrashLoopBackOff", message: "preexisting-canary-msg" } } }] },
      },
    });

    // 1. Open Issues tab in the real browser, ensure Pod/ReplicaSet/Deployment are indexed (ready+complete), and enable Warning alerts.
    await page.setViewportSize({ width: 1440, height: 1000 });
    await openApp(page, "/agent/kubernetes?view=issues");
    const issues = page.getByRole("region", { name: "Kubernetes issues", exact: true });
    await expect(issues).toBeVisible();
    await expect(issues.getByRole("article", { name: "Deployment preexisting-api", exact: true }).first()).toBeVisible({ timeout: 60_000 });

    const alertSwitch = issues.getByRole("switch", { name: "Alert on new issues", exact: true });
    await expect(alertSwitch).toHaveAttribute("aria-checked", "false");
    await alertSwitch.click();
    await expect(page.getByText("Alerts on. Issues that break from now on will alert.", { exact: true })).toBeVisible();
    const warningBtn = issues.getByRole("group", { name: "Issue alert level", exact: true }).getByRole("button", { name: "Warning", exact: true });
    await warningBtn.click();
    await expect(page.getByText("Alert level saved.", { exact: true })).toBeVisible();

    // Ensure at least one complete continuous capture cycle has established the owned baseline and Pod/ReplicaSet/Deployment coverage.
    await expect.poll(() => {
      const rows = JSON.parse(haControl("rows")) as Array<{ snapshot: { coverage?: { Pod?: { to?: string }; Deployment?: { to?: string }; ReplicaSet?: { to?: string } } } }>;
      return rows.some((r) => Boolean(r.snapshot.coverage?.Pod?.to && r.snapshot.coverage?.Deployment?.to && r.snapshot.coverage?.ReplicaSet?.to));
    }, { timeout: 90_000, intervals: [1000, 2000] }).toBe(true);

    // Wait past the 15s index refresh interval so Pod can refresh immediately while ReplicaSet remains in its older ready+complete state.
    await page.waitForTimeout(16_000);

    // 2. Concurrent partial owner chain + ready-but-stale owner snapshot:
    // Hold ReplicaSet list reads in fakekube while creating Deployment checkout, ReplicaSets, and crashing Pods (ownerReferences -> ReplicaSet checkout-6c8d4).
    const rootUID = "k13k15-checkout";
    const rsV41UID = "k13k15-checkout-rs-v41";
    const rsV42UID = "k13k15-checkout-rs-v42";
    const crashPodName = "checkout-6c8d4-x2k4q";
    const crashPodUID = "k13k15-checkout-crash-pod";
    const delPodName = "checkout-6c8d4-d3e4f";
    const delPodUID = "k13k15-checkout-del-pod";
    const crashLogPath = `/api/v1/namespaces/${ns}/pods/${crashPodName}/log`;
    const delLogPath = `/api/v1/namespaces/${ns}/pods/${delPodName}/log`;

    const nowMs = second(Date.now());
    const seedMs = nowMs;
    const brokeMs = nowMs - 2000;
    const firstTermMs = nowMs - 1000;
    const brokeAtISO = iso(brokeMs);
    const windowFromISO = iso(brokeMs - 200_000);

    const checkoutDeployment = (availableReplicas: number, createdMs = seedMs - 1_800_000) => ({
      apiVersion: "apps/v1", kind: "Deployment",
      metadata: { name: "checkout", namespace: ns, uid: rootUID, generation: 2, creationTimestamp: iso(createdMs), labels: { app: "checkout" } },
      spec: { replicas: 4, selector: { matchLabels: { app: "checkout" } }, template: { metadata: { labels: { app: "checkout" } }, spec: { containers: [{ name: "api", image: "example.invalid/checkout:v42" }] } } },
      status: { observedGeneration: 2, replicas: 4, readyReplicas: availableReplicas, availableReplicas, updatedReplicas: 4 },
    });
    const makeCheckoutPod = (name: string, uid: string, restarts: number, termMs: number | null, brokeAtStr: string, reason = "OOMKilled", exitCode = 137) => ({
      apiVersion: "v1", kind: "Pod",
      metadata: { name, namespace: ns, uid, creationTimestamp: iso(seedMs - 240_000), labels: { app: "checkout" }, ownerReferences: [{ apiVersion: "apps/v1", kind: "ReplicaSet", name: "checkout-6c8d4", uid: rsV42UID, controller: true }] },
      spec: { nodeName: "k13-node", containers: [{ name: "api", image: "example.invalid/checkout:v42" }] },
      status: {
        phase: "Running",
        conditions: [{ type: "Ready", status: restarts > 0 ? "False" : "True", lastTransitionTime: brokeAtStr }],
        containerStatuses: [{
          name: "api", ready: restarts === 0, restartCount: restarts,
          state: restarts > 0 ? { waiting: { reason: "CrashLoopBackOff", message: "raw-termination-canary-msg" } } : { running: { startedAt: brokeAtStr } },
          ...(termMs ? { lastState: { terminated: { reason, exitCode, finishedAt: iso(termMs), message: "raw-termination-canary-msg" } } } : {}),
        }],
      },
    });

    const crashFirstLines = Array.from({ length: 12 }, (_, index) => ({
      at: iso(firstTermMs - 180_000 + index * 10_000),
      text: index === 2
        ? "ERROR payment client timeout Authorization: ******"
        : index % 3 === 0
          ? `ERROR payment client timeout attempt ${index}`
          : `INFO starting checkout worker pool ${index}`,
    }));
    const delLines = Array.from({ length: 8 }, (_, index) => ({
      at: iso(firstTermMs - 140_000 + index * 10_000),
      text: index % 2 === 0 ? `WARN deleted replica retry ${index}` : `INFO deleted pod request ${index}`,
    }));

    await upstream("/_fake/list-holds", { resource: "replicasets", hold: true, remaining: 2 });
    await upstream("/_fake/list-holds", { resource: "deployments", hold: true, remaining: 2 });
    await upstream("/_fake/mutate", { operation: "upsert", resource: "deployments", namespace: ns, name: "checkout", object: checkoutDeployment(3) });
    await upstream("/_fake/mutate", {
      operation: "upsert", resource: "replicasets", namespace: ns, name: "checkout-v41",
      object: {
        apiVersion: "apps/v1", kind: "ReplicaSet",
        metadata: { name: "checkout-v41", namespace: ns, uid: rsV41UID, creationTimestamp: iso(seedMs - 900_000), labels: { app: "checkout" }, annotations: { "deployment.kubernetes.io/revision": "1" }, ownerReferences: [{ apiVersion: "apps/v1", kind: "Deployment", name: "checkout", uid: rootUID, controller: true }] },
        spec: { replicas: 0, template: { spec: { containers: [{ name: "api", image: "example.invalid/checkout:v41" }] } } },
        status: { replicas: 0, readyReplicas: 0 },
      },
    });
    await upstream("/_fake/mutate", {
      operation: "upsert", resource: "replicasets", namespace: ns, name: "checkout-6c8d4",
      object: {
        apiVersion: "apps/v1", kind: "ReplicaSet",
        metadata: { name: "checkout-6c8d4", namespace: ns, uid: rsV42UID, creationTimestamp: iso(seedMs - 240_000), labels: { app: "checkout" }, annotations: { "deployment.kubernetes.io/revision": "2" }, ownerReferences: [{ apiVersion: "apps/v1", kind: "Deployment", name: "checkout", uid: rootUID, controller: true }] },
        spec: { replicas: 4, template: { spec: { containers: [{ name: "api", image: "example.invalid/checkout:v42" }] } } },
        status: { replicas: 4, readyReplicas: 3 },
      },
    });
    await upstream("/_fake/mutate", { operation: "upsert", resource: "pods", namespace: ns, name: crashPodName, object: makeCheckoutPod(crashPodName, crashPodUID, 1, firstTermMs, brokeAtISO, "OOMKilled", 137) });
    await upstream("/_fake/mutate", { operation: "upsert", resource: "pods", namespace: ns, name: delPodName, object: makeCheckoutPod(delPodName, delPodUID, 1, firstTermMs, brokeAtISO, "Error", 1) });
    await upstream("/_fake/log-fixtures", { path: crashLogPath, container: "api", previous: true, lines: crashFirstLines });
    await upstream("/_fake/log-fixtures", { path: delLogPath, container: "api", previous: true, lines: delLines });
    await upstream("/_fake/mutate", {
      operation: "upsert", resource: "pods", namespace: ns, name: "orphan-rs-pod-0",
      object: {
        apiVersion: "v1", kind: "Pod",
        metadata: { name: "orphan-rs-pod-0", namespace: ns, uid: "k13k15-orphan-rs-pod-0", creationTimestamp: iso(seedMs - 240_000), labels: { app: "orphan-rs" }, ownerReferences: [{ apiVersion: "apps/v1", kind: "ReplicaSet", name: "missing-rs", uid: "k13k15-missing-rs", controller: true }] },
        spec: { nodeName: "k13-node", containers: [{ name: "api", image: "example.invalid/orphan:v1" }] },
        status: {
          phase: "Running",
          conditions: [{ type: "Ready", status: "False", lastTransitionTime: brokeAtISO }],
          containerStatuses: [{ name: "api", ready: false, restartCount: 2, state: { waiting: { reason: "CrashLoopBackOff" } }, lastState: { terminated: { reason: "Error", exitCode: 1, finishedAt: iso(firstTermMs) } } }],
        },
      },
    });

    // Trigger a Pod-only refresh while ReplicaSet and Deployment remain in their older ready+complete snapshot (and held if any concurrent list starts).
    const podOnlyRes = await page.request.get("/api/admin/kubernetes/resources?resource_id=core~v1~pods&limit=200");
    expect(podOnlyRes.status()).toBe(200);
    const podOnlyData = await podOnlyRes.json() as { items?: Array<{ name: string }> };
    expect((podOnlyData.items ?? []).some((item) => item.name === crashPodName)).toBe(true);
    await page.waitForTimeout(4000);

    // Assert zero transient ReplicaSet 2-of-0 alert (and zero premature missing-owner alert) was emitted during the ready-but-stale-owner snapshot.
    const staleOwnerReceipts = await receiver();
    expect(staleOwnerReceipts.emails).toHaveLength(0);
    expect(staleOwnerReceipts.lark).toHaveLength(0);
    expect(staleOwnerReceipts.msteams).toHaveLength(0);

    // Wait past the 15s refresh interval so Pod, ReplicaSet, and Deployment all re-list in the same refresh cycle (testing stable initial OwnerObservedAt boundary).
    await page.waitForTimeout(16_000);
    await upstream("/_fake/list-holds", { resource: "replicasets", hold: false, remaining: 0 });
    await upstream("/_fake/list-holds", { resource: "deployments", hold: false, remaining: 0 });
    await page.getByRole("button", { name: "Refresh Kubernetes data", exact: true }).click();

    await expect.poll(async () => {
      const r = await receiver();
      return r.emails.length === 2 && r.lark.length === 2 && r.msteams.length === 2;
    }, { timeout: 90_000, intervals: [1000, 2000] }).toBe(true);

    const warningReceipts = await receiver();
    for (const msg of [...warningReceipts.emails.map((e) => e.body), ...warningReceipts.lark.map((l) => l.text), ...warningReceipts.msteams.map((m) => m.text)]) {
      expect(msg).not.toContain("ReplicaSet checkout-6c8d4");
      expect(msg).not.toContain("2 of 0 pods");
    }
    const warningEmail = warningReceipts.emails.find((e) => e.body.includes("Deployment checkout is crash looping"))!;
    expect(warningEmail).toBeTruthy();
    expect(warningEmail.body).toContain("🟡");
    expect(warningEmail.body).toContain("<b>Affected:</b> 2 of 4 pods");
    expect(warningEmail.body).toContain("Exit 137 OOMKilled");
    expect(warningEmail.body).toContain("Image changed");
    expect(warningEmail.links.map((l) => l.label)).toEqual(["Open timeline", "Investigate with DevOps Agent"]);
    const orphanEmail = warningReceipts.emails.find((e) => e.body.includes("ReplicaSet missing-rs is crash looping"))!;
    expect(orphanEmail).toBeTruthy();
    expect(orphanEmail.body).toContain("<b>Affected:</b> 1 of 0 pods");
    const timelineURL = warningEmail.links[0].url;

    // Wait until initial break-log snapshots for both Pods are persisted before deleting/restarting for the Timeline view.
    await expect.poll(() => {
      const rows = JSON.parse(haControl("rows")) as Array<{ snapshot: { break_logs?: { snapshots?: Array<{ pod_uid: string }> } } }>;
      const snaps = rows.flatMap((r) => r.snapshot.break_logs?.snapshots ?? []);
      return snaps.some((s) => s.pod_uid === crashPodUID) && snaps.some((s) => s.pod_uid === delPodUID);
    }, { timeout: 90_000, intervals: [1000, 2000] }).toBe(true);

    // 3. Inspect Kubernetes issue incident detail in real UI (facts, evidence, changes, deep links, and no broken /agent/logs/ link).
    await page.goto("/incidents?status=open");
    const checkoutLink = page.getByRole("link", { name: /Deployment checkout is crash looping/i }).first();
    await expect(checkoutLink).toBeVisible();
    await checkoutLink.click();

    await expect(page.getByText("No summary provided.", { exact: true })).toHaveCount(0);
    await expect(page.getByText("fakekube", { exact: true })).toBeVisible();
    await expect(page.getByText(ns, { exact: true }).first()).toBeVisible();
    await expect(page.getByText("checkout", { exact: true })).toBeVisible();
    await expect(page.getByText("2 of 4 pods", { exact: true })).toBeVisible();
    const evidenceSection = page.getByRole("region", { name: "Evidence", exact: true });
    await expect(evidenceSection).toContainText("Exit 137 OOMKilled");
    await expect(evidenceSection).toContainText("Exit 1 Error");
    const changesSection = page.getByRole("region", { name: "Changes", exact: true });
    await expect(changesSection).toContainText("Image changed");
    const k8sLinksNav = page.getByRole("navigation", { name: "Kubernetes issue links", exact: true });
    const detailTimelineLink = k8sLinksNav.getByRole("link", { name: "Timeline", exact: true });
    const detailAgentLink = k8sLinksNav.getByRole("link", { name: "Agent", exact: true });
    await expect(detailTimelineLink).toHaveAttribute("href", /\/agent\/kubernetes\?.*root_uid=k13k15-checkout.*view=timeline/);
    await expect(detailAgentLink).toHaveAttribute("href", /\/agent\/chat\?.*issue=pod\.crash_loop.*provider=kubernetes/);
    await expect(page.getByText("Rule", { exact: true })).toBeVisible();
    await expect(page.getByText("kubernetes:pod.crash_loop", { exact: true })).toBeVisible();
    await expect(page.locator('a[href^="/agent/logs/"]')).toHaveCount(0);
    await captureBreakState(page, "live-repaired-k8s-incident-detail-desktop.png", "dark", "L: live OSS Kubernetes issue incident detail");

    await page.setViewportSize({ width: 390, height: 844 });
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
    await captureBreakState(page, "live-repaired-k8s-incident-detail-mobile.png", "dark", "L: live OSS Kubernetes issue incident detail mobile");
    await page.setViewportSize({ width: 1440, height: 1000 });

    // 4. Critical partial channel failure: MS Teams HTTP 500 while Email + Lark succeed -> list, peek drawer, and detail UI.
    await receiver("/_control/fault", "POST", { target: "msteams", status: 500 });
    await page.waitForTimeout(16_000);
    const partialBreakMs = second(Date.now()) - 1000;
    await upstream("/_fake/mutate", {
      operation: "upsert", resource: "deployments", namespace: ns, name: "payments-worker",
      object: {
        apiVersion: "apps/v1", kind: "Deployment",
        metadata: { name: "payments-worker", namespace: ns, uid: "k13k15-payments-worker", generation: 1, creationTimestamp: iso(partialBreakMs - 600_000), labels: { app: "payments-worker" } },
        spec: { replicas: 2, selector: { matchLabels: { app: "payments-worker" } }, template: { spec: { containers: [{ name: "worker", image: "example.invalid/payments:v1" }] } } },
        status: { observedGeneration: 1, replicas: 2, readyReplicas: 0, availableReplicas: 0, updatedReplicas: 2 },
      },
    });
    await upstream("/_fake/mutate", {
      operation: "upsert", resource: "pods", namespace: ns, name: "payments-worker-pod-0",
      object: {
        apiVersion: "v1", kind: "Pod",
        metadata: { name: "payments-worker-pod-0", namespace: ns, uid: "k13k15-payments-pod-0", creationTimestamp: iso(partialBreakMs - 600_000), ownerReferences: [{ apiVersion: "apps/v1", kind: "Deployment", name: "payments-worker", uid: "k13k15-payments-worker", controller: true }] },
        spec: { nodeName: "k13-node", containers: [{ name: "worker", image: "example.invalid/payments:v1" }] },
        status: { phase: "Running", conditions: [{ type: "Ready", status: "False", lastTransitionTime: iso(partialBreakMs) }], containerStatuses: [{ name: "worker", ready: false, restartCount: 5, state: { waiting: { reason: "CrashLoopBackOff" } }, lastState: { terminated: { reason: "Error", exitCode: 1, finishedAt: iso(partialBreakMs) } } }] },
      },
    });

    await page.goto("/agent/kubernetes?view=issues");
    await page.getByRole("button", { name: "Refresh Kubernetes data", exact: true }).click();
    await expect.poll(async () => {
      const r = await receiver();
      return r.emails.length === 3 && r.lark.length === 3;
    }, { timeout: 90_000, intervals: [1000, 2000] }).toBe(true);
    expect((await receiver()).msteams).toHaveLength(2);
    await receiver("/_control/fault", "POST", { target: "msteams", status: 0 });

    await page.goto("/incidents?status=open");
    const paymentsLink = page.getByRole("link", { name: /Deployment payments-worker is crash looping/i }).first();
    await expect(paymentsLink).toBeVisible();
    const paymentsRow = paymentsLink.locator("xpath=ancestor::tr[1]");

    // Expand inline partial-delivery failure detail in the table row.
    const notifyExpandBtn = paymentsRow.getByRole("button", { name: "Show notification failure detail", exact: true });
    await expect(notifyExpandBtn).toBeVisible();
    await expect(notifyExpandBtn).toContainText("partial");
    await notifyExpandBtn.click();
    await expect(page.getByText(/Partial delivery:.*MS Teams API returned 500 status code/i)).toBeVisible();

    // Open Peek drawer on the same row and verify Partial delivery error banner is visible.
    await paymentsRow.getByRole("button", { name: /^View incident / }).click();
    const peekDialog = page.getByRole("dialog");
    await expect(peekDialog).toBeVisible();
    await expect(peekDialog).toContainText("partial");
    await expect(peekDialog.getByText(/Partial delivery:.*MS Teams API returned 500 status code/i)).toBeVisible();
    await captureBreakState(page, "live-repaired-incidents-list-peek-partial-desktop.png", "dark", "L: live OSS incidents list + peek drawer partial channel failure");

    await page.keyboard.press("Escape");
    await page.setViewportSize({ width: 390, height: 844 });
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
    await captureBreakState(page, "live-repaired-incidents-list-partial-mobile.png", "dark", "L: live OSS incidents list partial channel failure mobile");
    await page.setViewportSize({ width: 1440, height: 1000 });

    // Open IncidentDetailPage for the partial-delivery incident and verify sent badges on succeeded channels + inline error + Timeline row.
    await paymentsLink.click();
    const notifiedCard = page.locator(".card", { has: page.locator(".card-title", { hasText: "Notified" }) });
    await expect(notifiedCard.locator("li", { hasText: "email" })).toContainText("sent");
    await expect(notifiedCard.locator("li", { hasText: "lark" })).toContainText("sent");
    await expect(notifiedCard.getByText(/MS Teams API returned 500 status code/i)).toBeVisible();
    const timelineCard = page.locator(".card", { has: page.locator(".card-title", { hasText: "Timeline" }) });
    await expect(timelineCard).toContainText("notified email");
    await expect(timelineCard).toContainText("notified lark");
    await expect(timelineCard).toContainText("Partial delivery");
    await expect(timelineCard.getByText(/MS Teams API returned 500 status code/i)).toBeVisible();
    await captureBreakState(page, "live-repaired-incident-detail-partial-desktop.png", "dark", "L: live OSS incident detail partial channel failure");

    await page.setViewportSize({ width: 390, height: 844 });
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
    await captureBreakState(page, "live-repaired-incident-detail-partial-mobile.png", "dark", "L: live OSS incident detail partial channel failure mobile");
    await page.setViewportSize({ width: 1440, height: 1000 });

    // 5. Ordinary log-pattern incident UI preserved.
    const createOrdinaryRes = await page.request.post("/api/incidents", {
      headers: { Origin: new URL(page.url()).origin },
      data: {
        AlertName: "Spike in payment gateway timeouts",
        Severity: "warning",
        ServiceName: "payments-api",
        Summary: "Ordinary log-miner pattern summary preserved.",
        PatternID: "pat-ordinary-log-01",
        PatternTemplate: "ERROR timeout waiting for upstream <*> after <*>ms",
        Logs: "2026-10-20T12:00:00Z ERROR timeout waiting for upstream ledger after 5000ms",
      },
    });
    expect(createOrdinaryRes.status()).toBe(201);

    await page.goto("/incidents?status=open");
    const ordinaryLink = page.getByRole("link", { name: "Spike in payment gateway timeouts", exact: true }).first();
    await expect(ordinaryLink).toBeVisible();
    await ordinaryLink.click();
    await expect(page.getByText("Ordinary log-miner pattern summary preserved.", { exact: true })).toBeVisible();
    await expect(page.getByRole("navigation", { name: "Kubernetes issue links", exact: true })).toHaveCount(0);
    await expect(page.getByText("Pattern", { exact: true }).first()).toBeVisible();
    const patternLink = page.getByRole("link", { name: "pat-ordinary", exact: true });
    await expect(patternLink).toHaveAttribute("href", "/agent/logs/pat-ordinary-log-01");
    await captureBreakState(page, "live-repaired-ordinary-log-incident-desktop.png", "dark", "L: live OSS ordinary log-pattern incident detail preserved");

    // 6. Trigger 2nd restart + deleted Pod on checkout and verify mobile 390 Timeline log-window caption & handles (including right edge).
    await upstream("/_fake/mutate", { operation: "delete", resource: "pods", namespace: ns, name: delPodName });
    const secondTermMs = second(Date.now()) - 2000;
    const windowToISO = iso(secondTermMs);
    const secondLines = Array.from({ length: 6 }, (_, index) => ({
      at: iso(secondTermMs - 25_000 + index * 4000),
      text: `INFO second restart line ${index}`,
    }));
    await upstream("/_fake/log-fixtures", { path: crashLogPath, container: "api", previous: true, lines: secondLines });
    await upstream("/_fake/mutate", { operation: "upsert", resource: "pods", namespace: ns, name: crashPodName, object: makeCheckoutPod(crashPodName, crashPodUID, 3, secondTermMs, brokeAtISO, "OOMKilled", 137) });
    await upstream("/_fake/mutate", { operation: "upsert", resource: "deployments", namespace: ns, name: "checkout", object: checkoutDeployment(0) });

    await page.waitForTimeout(16_000);
    await page.goto("/agent/kubernetes?view=issues");
    await page.getByRole("button", { name: "Refresh Kubernetes data", exact: true }).click();
    await expect.poll(() => {
      const rows = JSON.parse(haControl("rows")) as Array<{ snapshot: { break_logs?: { snapshots?: Array<{ pod_uid: string }> } } }>;
      const snaps = rows.flatMap((r) => r.snapshot.break_logs?.snapshots ?? []);
      return snaps.filter((s) => s.pod_uid === crashPodUID).length;
    }, { timeout: 90_000, intervals: [1000, 2000] }).toBe(2);

    const timelineTarget = new URL(timelineURL);
    timelineTarget.searchParams.set("logs_from", windowFromISO);
    timelineTarget.searchParams.set("logs_to", windowToISO);
    await page.goto(timelineTarget.pathname + timelineTarget.search);

    const lane = page.getByRole("region", { name: "Timeline lane checkout", exact: true });
    await expect(lane).toBeVisible();
    const chart = lane.getByRole("region", { name: "Resource timeline chart", exact: true });
    const logs = page.getByRole("region", { name: "Logs at break", exact: true });
    const viewer = logs.getByRole("log");
    await expect(viewer).toContainText("payment client timeout");
    await expect(viewer).toContainText("deleted replica retry");
    await expect(logs.getByText("Captured at break").first()).toBeVisible();

    await logs.scrollIntoViewIfNeeded();
    await logs.getByRole("button", { name: "Log coverage tip" }).click();
    await expect(logs.getByText(/The container restarted more than once since the break/)).toBeVisible();
    await captureBreakState(page, "live-k13-k15-timeline-desktop.png", "dark", "L: live OSS + fakekube + Postgres + Loki + Elasticsearch stack");

    await page.setViewportSize({ width: 390, height: 844 });
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
    await expect(page.getByRole("toolbar", { name: "Break log actions" })).toBeInViewport();
    const caption = chart.locator(".break-log-window-caption");
    await expect(caption).toBeVisible();
    await expect(caption).toHaveText(/^log window [+-]?\d+(?:\.\d+)? m \.\.\. [+-]?\d+(?:\.\d+)? m$/);

    const verifyMobileCaptionBounds = async () => {
      const bounds = await caption.evaluate((element) => {
        const rect = element.getBoundingClientRect();
        const plot = element.closest(".break-log-bracket-plot")!.getBoundingClientRect();
        const scroll = element.closest(".resource-timeline-scroll")!.getBoundingClientRect();
        const range = document.createRange();
        range.selectNodeContents(element);
        const text = Array.from(range.getClientRects()).map((line) => ({ left: line.left, right: line.right, top: line.top, bottom: line.bottom }));
        return {
          textContent: element.textContent,
          caption: { left: rect.left, right: rect.right, top: rect.top, bottom: rect.bottom },
          plot: { left: plot.left, right: plot.right },
          visible: { left: scroll.left + 132, right: scroll.right },
          text,
          captionOverflow: element.scrollWidth > element.clientWidth,
          pageOverflow: document.documentElement.scrollWidth > window.innerWidth,
          controlsOverlap: Array.from(document.querySelectorAll(".resource-timeline-break-pill, .resource-timeline-now-label, .break-log-handle")).some((control) => {
            const other = control.getBoundingClientRect();
            return rect.left < other.right && rect.right > other.left && rect.top < other.bottom && rect.bottom > other.top;
          }),
        };
      });
      expect(bounds.caption.left).toBeGreaterThanOrEqual(bounds.plot.left - 1);
      expect(bounds.caption.right).toBeLessThanOrEqual(bounds.plot.right + 1);
      expect(bounds.caption.left).toBeGreaterThanOrEqual(bounds.visible.left - 1);
      expect(bounds.caption.right).toBeLessThanOrEqual(bounds.visible.right + 1);
      expect(bounds.captionOverflow).toBe(false);
      expect(bounds.pageOverflow).toBe(false);
      expect(bounds.controlsOverlap).toBe(false);
      expect(bounds.text.length).toBeGreaterThan(0);
      for (const line of bounds.text) {
        expect(line.left).toBeGreaterThanOrEqual(bounds.caption.left - 1);
        expect(line.right).toBeLessThanOrEqual(bounds.caption.right + 1);
        expect(line.top).toBeGreaterThanOrEqual(bounds.caption.top - 1);
        expect(line.bottom).toBeLessThanOrEqual(bounds.caption.bottom + 1);
      }
      return bounds;
    };

    const centeredBounds = await verifyMobileCaptionBounds();
    await captureBreakState(page, "live-k13-k15-timeline-mobile.png", "dark", "L: live OSS + fakekube + Postgres + Loki + Elasticsearch stack");

    // Push log window bracket to the right edge of the chart on mobile 390x844 and verify caption + both slider handles remain accessible.
    const chartUntil = Date.parse((await chart.getAttribute("data-until"))!);
    const rightEdgeURL = new URL(page.url());
    rightEdgeURL.searchParams.set("logs_from", new Date(chartUntil - 180_000).toISOString());
    rightEdgeURL.searchParams.set("logs_to", new Date(chartUntil - 30_000).toISOString());
    await page.goto(rightEdgeURL.pathname + rightEdgeURL.search);
    await expect(caption).toBeVisible();
    await chart.evaluate((element) => { element.scrollLeft = element.scrollWidth - element.clientWidth; });
    await expect.poll(async () => {
      const b = await verifyMobileCaptionBounds();
      return b.captionOverflow === false;
    }).toBe(true);
    const rightEdgeBounds = await verifyMobileCaptionBounds();

    const startHandle = chart.getByRole("slider", { name: "Log window start", exact: true });
    const endHandle = chart.getByRole("slider", { name: "Log window end", exact: true });
    await expect(startHandle).toBeInViewport();
    await expect(endHandle).toBeInViewport();
    await endHandle.focus();
    await expect(endHandle).toBeFocused();
    const prevEndVal = await endHandle.getAttribute("aria-valuenow");
    await page.keyboard.press("ArrowLeft");
    await expect(endHandle).not.toHaveAttribute("aria-valuenow", prevEndVal!);
    await startHandle.focus();
    await expect(startHandle).toBeFocused();
    const prevStartVal = await startHandle.getAttribute("aria-valuenow");
    await page.keyboard.press("ArrowRight");
    await expect(startHandle).not.toHaveAttribute("aria-valuenow", prevStartVal!);
    await captureBreakState(page, "live-repaired-timeline-right-edge-mobile.png", "dark", "L: live OSS mobile 390 timeline right-edge caption and handles");

    expect(errors).toEqual([]);
    await testInfo.attach("repaired-flows-evidence", {
      body: JSON.stringify({ centeredBounds, rightEdgeBounds, receipts: await receiver() }, null, 2),
      contentType: "application/json",
    });
  });
});

test.describe("Live Enterprise K15 user and receiver acceptance", () => {
  test.skip(process.env.E2E_K15_ENTERPRISE_LIVE !== "1", "requires the live licensed Enterprise fakekube stack and local receiver");

  test("verifies unauthenticated deep-link login return, UI alert enable, custom-channel diversion, critical priority floor and on-call paging", async ({ page }, testInfo) => {
    test.setTimeout(480_000);
    const errors: string[] = [];
    const chatPostRequests: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    page.on("request", (req) => {
      const url = new URL(req.url());
      if (req.method() === "POST" && url.pathname.startsWith("/api/admin/chat/")) {
        chatPostRequests.push(`${req.method()} ${url.pathname}`);
      }
    });

    await page.emulateMedia({ colorScheme: "dark" });
    await page.addInitScript(() => localStorage.setItem("versus.theme", "dark"));

    const state = harnessRuntimeStateDir();
    const fakePort = Number(process.env.HARNESS_FAKEKUBE_PORT || 28743);
    const receiverPort = Number(process.env.HARNESS_RECEIVER_HTTP_PORT || 28690);
    const ca = fs.readFileSync(path.join(state, "fakekube/ca.crt"));

    const upstream = (endpoint: string, body?: unknown) => new Promise<unknown>((resolve, reject) => {
      const encoded = body === undefined ? undefined : JSON.stringify(body);
      const call = https.request(`https://localhost:${fakePort}${endpoint}`, {
        ca,
        method: encoded ? "POST" : "GET",
        headers: encoded ? { "Content-Type": "application/json" } : {},
      }, (response) => {
        let text = "";
        response.on("data", (chunk) => { text += chunk; });
        response.on("end", () => response.statusCode === 200 ? resolve(JSON.parse(text)) : reject(new Error(`fakekube ${endpoint} HTTP ${response.statusCode} ${text}`)));
      });
      call.on("error", reject);
      call.end(encoded);
    });

    type ReceiverState = {
      emails: Array<{ body: string; links: Array<{ label: string; url: string }> }>;
      lark: Array<{ text: string; links: Array<{ label: string; url: string }> }>;
      divertLark: Array<{ text: string; links: Array<{ label: string; url: string }> }>;
      msteams: Array<{ text: string; links: Array<{ label: string; url: string }> }>;
      servicenow: Array<{ table: string; hasBasicAuth: boolean; payload: { correlation_id?: string } }>;
    };

    const receiver = async (endpoint = "/_receipts", method = "GET", body?: unknown): Promise<ReceiverState> => {
      const res = await fetch(`http://127.0.0.1:${receiverPort}${endpoint}`, {
        method,
        headers: body ? { "Content-Type": "application/json" } : {},
        body: body ? JSON.stringify(body) : undefined,
      });
      expect(res.status).toBe(200);
      return res.json() as Promise<ReceiverState>;
    };

    await receiver("/_clear", "POST");

    // 1. Unauthenticated deep link to Chat -> real local-admin login in UI -> preserves deep link and prefills without auto-send.
    const deepChatPath = "/agent/chat?provider=kubernetes&cluster=fakekube&resource_id=apps~v1~deployments&name=ent-checkout&namespace=shop&uid=ent-checkout-root&include_changes=true&issue=pod.crash_loop";
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.goto(deepChatPath);
    await expect(page.getByTestId("local-login-form")).toBeVisible();
    await page.getByTestId("local-login-username").fill(process.env.E2E_ADMIN_USERNAME || "admin");
    await page.getByTestId("local-login-password").fill(process.env.E2E_ADMIN_PASSWORD || "");
    await page.getByTestId("local-login-submit").click();
    await expect(page.getByTestId("app-authenticated")).toBeVisible();
    expect(new URL(page.url()).pathname + new URL(page.url()).search).toBe(deepChatPath);
    await expect(page.getByRole("textbox", { name: "Message", exact: true })).toHaveValue("Why is Deployment ent-checkout crash looping?");
    await expect(page.getByText("shop/ent-checkout", { exact: true })).toBeVisible();
    expect(chatPostRequests).toEqual([]);

    // 2. Enable issue alerts at Warning in the real Enterprise UI.
    await page.goto("/agent/kubernetes?view=issues");
    const issues = page.getByRole("region", { name: "Kubernetes issues", exact: true });
    await expect(issues).toBeVisible();
    const alertSwitch = issues.getByRole("switch", { name: "Alert on new issues", exact: true });
    await expect(alertSwitch).toHaveAttribute("aria-checked", "false");
    await alertSwitch.click();
    await expect(alertSwitch).toHaveAttribute("aria-checked", "true");
    const warningBtn = issues.getByRole("group", { name: "Issue alert level", exact: true }).getByRole("button", { name: "Warning", exact: true });
    await warningBtn.click();
    await expect(warningBtn).toHaveAttribute("aria-pressed", "true");

    // Configure custom fatigue channel to /divert-lark through the authenticated session.
    const origin = new URL(page.url()).origin;
    const setCustomRes = await page.request.put("/enterprise/api/alert-fatigue/custom-channel", {
      headers: { Origin: origin },
      data: {
        channel_type: "lark",
        enabled: true,
        config: { webhook_url: `http://host.docker.internal:${receiverPort}/divert-lark`, use_proxy: "false" },
      },
    });
    expect(setCustomRes.status()).toBe(200);

    // Ensure alert fatigue starts OFF by default.
    const fatigueCfgRes = await page.request.get("/enterprise/api/alert-fatigue/config");
    expect(fatigueCfgRes.status()).toBe(200);
    expect((await fatigueCfgRes.json()).enabled).toBe(false);

    const second = (ms: number) => Math.floor(ms / 1000) * 1000;
    const iso = (ms: number) => new Date(ms).toISOString().replace(".000Z", "Z");
    const ns = "shop";
    const wName = "ent-checkout";
    const rootUID = "ent-checkout-root";
    const podName = "ent-checkout-pod-0";

    const deployObj = (avail: number) => ({
      apiVersion: "apps/v1", kind: "Deployment",
      metadata: { name: wName, namespace: ns, uid: rootUID, generation: 1, creationTimestamp: iso(Date.now() - 600_000), labels: { app: wName } },
      spec: { replicas: 3, selector: { matchLabels: { app: wName } }, template: { metadata: { labels: { app: wName } }, spec: { containers: [{ name: "api", image: "example.invalid/ent:v1" }] } } },
      status: { observedGeneration: 1, replicas: 3, readyReplicas: avail, availableReplicas: avail, updatedReplicas: 3 },
    });
    const podObj = (broken: boolean, brokeMs: number) => ({
      apiVersion: "v1", kind: "Pod",
      metadata: { name: podName, namespace: ns, uid: "ent-checkout-pod-uid", creationTimestamp: iso(Date.now() - 600_000), ownerReferences: [{ apiVersion: "apps/v1", kind: "Deployment", name: wName, uid: rootUID, controller: true }] },
      spec: { nodeName: "k13-node", containers: [{ name: "api", image: "example.invalid/ent:v1" }] },
      status: {
        phase: "Running",
        conditions: [{ type: "Ready", status: broken ? "False" : "True", lastTransitionTime: iso(brokeMs) }],
        containerStatuses: [{
          name: "api", ready: !broken, restartCount: broken ? 6 : 0,
          state: broken ? { waiting: { reason: "CrashLoopBackOff" } } : { running: { startedAt: iso(brokeMs) } },
          ...(broken ? { lastState: { terminated: { reason: "Error", exitCode: 1, finishedAt: iso(brokeMs) } } } : {}),
        }],
      },
    });

    const waitHealthy = async () => {
      const t = second(Date.now());
      await upstream("/_fake/mutate", { operation: "upsert", resource: "pods", namespace: ns, name: podName, object: podObj(false, t) });
      await upstream("/_fake/mutate", { operation: "upsert", resource: "deployments", namespace: ns, name: wName, object: deployObj(3) });
      await expect.poll(async () => {
        const res = await page.request.get(`/api/admin/kubernetes/issues?cluster=fakekube&broken=true&limit=200`);
        const body = await res.json();
        return !(body.items ?? []).some((item: { root?: { name?: string } }) => item.root?.name === wName);
      }, { timeout: 90_000, intervals: [1000, 2000] }).toBe(true);
    };

    const resolveOpenIncidents = async () => {
      const res = await page.request.get("/api/admin/incidents?status=open&limit=50");
      const list = (await res.json()).incidents ?? [];
      for (const inc of list) {
        if ((inc.service ?? "").includes(wName)) {
          const r = await page.request.post(`/api/admin/incidents/${inc.id}/resolve`, { headers: { Origin: origin }, data: {} });
          expect(r.status()).toBe(200);
        }
      }
    };

    // Enable Alert Fatigue (enabled: true, pending_review: false) via API/UI.
    const enableFatigueRes = await page.request.put("/enterprise/api/alert-fatigue/config", {
      headers: { Origin: origin },
      data: { enabled: true, pending_review: false },
    });
    expect(enableFatigueRes.status()).toBe(200);

    // 1st Warning episode under Alert Fatigue ON -> proceeds to primary /lark.
    await waitHealthy();
    await page.waitForTimeout(3000);
    const firstBrokeMs = second(Date.now()) - 1000;
    await upstream("/_fake/mutate", { operation: "upsert", resource: "pods", namespace: ns, name: podName, object: podObj(true, firstBrokeMs) });
    await upstream("/_fake/mutate", { operation: "upsert", resource: "deployments", namespace: ns, name: wName, object: deployObj(2) });
    await expect.poll(async () => (await receiver()).lark.length, { timeout: 90_000, intervals: [1000, 2000] }).toBe(1);
    expect((await receiver()).divertLark).toHaveLength(0);

    // Recover and resolve 1st episode, then trigger 2nd Warning episode -> DIVERTED to /divert-lark!
    await waitHealthy();
    await resolveOpenIncidents();
    await page.waitForTimeout(3000);
    const secondBrokeMs = second(Date.now()) - 1000;
    await upstream("/_fake/mutate", { operation: "upsert", resource: "pods", namespace: ns, name: podName, object: podObj(true, secondBrokeMs) });
    await upstream("/_fake/mutate", { operation: "upsert", resource: "deployments", namespace: ns, name: wName, object: deployObj(2) });
    await expect.poll(async () => (await receiver()).divertLark.length, { timeout: 90_000, intervals: [1000, 2000] }).toBe(1);
    const afterDivert = await receiver();
    expect(afterDivert.lark).toHaveLength(1);
    expect(afterDivert.divertLark[0].text).toContain("Deployment ent-checkout is crash looping");
    expect(afterDivert.divertLark[0].links.map((l) => l.label)).toEqual(["Open timeline", "Investigate with DevOps Agent"]);

    // Verify fingerprint review table stamps status="fatigued" and routed_channel="custom:lark".
    const fpRes = await page.request.get("/enterprise/api/alert-fatigue/fingerprints");
    expect(fpRes.status()).toBe(200);
    const fpItems = (await fpRes.json()).fingerprints ?? [];
    expect(fpItems.some((item: { status: string; routed_channel: string }) => item.status === "fatigued" && item.routed_channel === "custom:lark")).toBe(true);

    // Escalate 2nd episode to Critical (0 of 3 ready) -> priority floor overrides fatigue and delivers to primary /lark + pages ServiceNow!
    await upstream("/_fake/mutate", { operation: "upsert", resource: "deployments", namespace: ns, name: wName, object: deployObj(0) });
    await expect.poll(async () => (await receiver()).lark.length, { timeout: 90_000, intervals: [1000, 2000] }).toBe(2);
    expect((await receiver()).divertLark).toHaveLength(1);

    // Wait for the 1-minute on-call wait window to page ServiceNow on the critical escalation.
    await expect.poll(async () => (await receiver()).servicenow.length, { timeout: 90_000, intervals: [1000, 2000] }).toBe(1);
    const entFinal = await receiver();
    expect(entFinal.servicenow[0].hasBasicAuth).toBe(true);

    // Cleanup Enterprise settings.
    await page.request.delete("/enterprise/api/alert-fatigue/custom-channel", { headers: { Origin: origin } });
    await page.request.put("/enterprise/api/alert-fatigue/config", { headers: { Origin: origin }, data: { enabled: false, pending_review: false } });
    await page.request.put("/api/admin/kubernetes/issue-alerts?cluster=fakekube", { headers: { Origin: origin }, data: { enabled: false, level: "critical" } });
    expect(errors).toEqual([]);
    await testInfo.attach("live-enterprise-k15-receipts", { body: JSON.stringify(entFinal, null, 2), contentType: "application/json" });
  });
});

test.describe("Resource detail drawer and workload log streaming acceptance", () => {
  test("fixture thresholds, usage history, quiet rotation, and crash handoff desktop and mobile", async ({ page, context }) => {
    test.setTimeout(180_000);
    await context.grantPermissions(["clipboard-read", "clipboard-write"]);
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));
    const nowMs = Date.now();
    const iso = (offsetMinutes: number) => new Date(nowMs - offsetMinutes * 60_000).toISOString();

    let podStreamReadCount = 0;
    const workloadStreamRequests: URL[] = [];

    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.emulateMedia({ colorScheme: "dark" });
    await page.addInitScript(() => localStorage.setItem("versus.theme", "dark"));
    await openApp(page, "/now");

    await page.route("**/api/admin/kubernetes/**", (route) =>
      route.fulfill({ json: { items: [], totals: {}, counts: {}, pods: [], nodes: [], edges: [], available: false, truncated: false, sync: { state: "ready", age_s: 1, partial: false } } }),
    );
    await page.route("**/api/admin/config/agent", (route) =>
      route.fulfill({ json: { enable: true, mode: "shadow", sources: [], ai: { enable: true }, catalog: {}, miner: {}, regex: { rules: [] }, redaction: {}, service_patterns: [] } }),
    );
    await page.route("**/api/admin/kubernetes/clusters", (route) =>
      route.fulfill({ json: { multiple: false, clusters: [{ id: "default", provider: "generic", health: "healthy", nodes: 2, ready_nodes: 2, pods: 6, running_pods: 6, warnings: 2 }] } }),
    );
    await page.route("**/api/admin/kubernetes/stream**", (route) =>
      route.fulfill({ contentType: "text/event-stream", body: `event: sync\ndata: ${JSON.stringify({ state: "ready", age_s: 1, partial: false })}\n\nevent: heartbeat\ndata: {}\n\n` }),
    );
    await page.route("**/api/admin/kubernetes/overview**", (route) =>
      route.fulfill({ json: { connector: "kubernetes", cluster_id: "default", observed_at: iso(0), nodes: 2, ready_nodes: 2, pods: 6, running_pods: 6, namespaces: 2, active_namespaces: 2, workloads: 3, warnings: 2, truncated: false } }),
    );
    await page.route("**/api/admin/kubernetes/usage-history**", (route) => {
      const url = new URL(route.request().url());
      const pod = url.searchParams.get("pod");
      if (pod === "checkout-7d9f8c-x2k4q") {
        return route.fulfill({
          json: {
            observed_at: iso(0),
            source: "capture",
            availability: "available",
            live_since: iso(60),
            retention_seconds: 21600,
            samples: [
              { timestamp: iso(20), cpu_milli: 410, memory_bytes: 360 * 1024 * 1024 },
              { timestamp: iso(10), cpu_milli: 780, memory_bytes: 400 * 1024 * 1024 },
              { timestamp: iso(2), cpu_milli: 820, memory_bytes: 480 * 1024 * 1024 },
            ],
          },
        });
      }
      return route.fulfill({
        json: { observed_at: iso(0), source: "capture", availability: "unavailable", live_since: iso(5), retention_seconds: 21600, samples: [] },
      });
    });
    await page.route("**/api/admin/kubernetes/usage**", (route) => {
      const url = new URL(route.request().url());
      if (url.pathname.endsWith("/usage-history")) return route.fallback();
      return route.fulfill({
        json: {
          observed_at: iso(0),
          availability: "available",
          fresh: true,
          truncated: false,
          pods: [{ kind: "Pod", namespace: "shop", name: "checkout-7d9f8c-x2k4q", cpu: "820m", memory: "480Mi", window: "30s" }],
          nodes: [],
        },
      });
    });
    await page.route("**/api/admin/kubernetes/crashes**", (route) =>
      route.fulfill({
        json: {
          items: [
            {
              id: "crash-oom-1",
              sequence: 1,
              cluster: "default",
              namespace: "shop",
              pod: "checkout-7d9f8c-x2k4q",
              pod_uid: "pod-x2k4q",
              root_uid: "deploy-checkout",
              root_kind: "Deployment",
              root_name: "checkout",
              container: "api",
              type: "termination",
              termination_slot: "previous",
              exit_code: 137,
              reason: "OOMKilled",
              restart_count: 6,
              at: iso(8),
              time_source: "finished_at",
              observed_at: iso(8),
            },
          ],
          since: iso(1440),
          until: iso(0),
          started_at: iso(120),
          gaps: [{ from: iso(90), to: iso(85) }],
          capture: { mode: "continuous", behind: false, last_cycle_at: iso(1) },
          coverage: { Pod: { from: iso(120), to: iso(0) } },
          sync: { state: "ready", age_s: 1, partial: false },
        },
      }),
    );
    await page.route("**/api/admin/kubernetes/changes**", (route) =>
      route.fulfill({
        json: {
          items: [
            {
              id: "chg-image-42",
              cluster: "default",
              kind: "Pod",
              namespace: "shop",
              name: "checkout-7d9f8c-x2k4q",
              uid: "pod-x2k4q",
              type: "image_changed",
              at: iso(12),
              fields: [{ path: "image", from: "checkout:v41", to: "checkout:v42" }],
            },
          ],
          gaps: [],
          sync: { state: "ready", age_s: 1, partial: false },
        },
      }),
    );
    await page.route("**/api/admin/kubernetes/issues**", (route) =>
      route.fulfill({
        json: {
          items: [
            {
              root: { kind: "Pod", namespace: "shop", name: "checkout-7d9f8c-x2k4q", uid: "pod-x2k4q" },
              rule: "pod.crash_loop",
              severity: "critical",
              count: 1,
              of: 1,
              broken: true,
              broke_at: iso(9),
              history: "complete",
            },
          ],
          totals: { critical: 1, warning: 0 },
          broken: { total: 1, by_severity: { critical: 1 } },
          truncated: false,
          sync: { state: "ready", age_s: 1, partial: false },
        },
      }),
    );
    await page.route("**/api/admin/kubernetes/resources/core~v1~pods/checkout-7d9f8c-x2k4q/describe**", (route) =>
      route.fulfill({
        json: {
          resource: {
            resource_id: "core~v1~pods",
            api_version: "v1",
            kind: "Pod",
            namespace: "shop",
            name: "checkout-7d9f8c-x2k4q",
            uid: "pod-x2k4q",
            created_at: iso(180),
            owners: [{ api_version: "apps/v1", kind: "ReplicaSet", namespace: "shop", name: "checkout-7d9f8c", uid: "rs-7d9f8c" }],
            conditions: [
              { type: "Ready", status: "True", reason: "ContainersReady", last_transition_at: iso(134) },
              { type: "Initialized", status: "True", reason: "PodCompleted", last_transition_at: "0001-01-01T00:00:00Z" },
            ],
            summary: {
              phase: "Running",
              node: "node-a",
              pod_ip: "10.42.1.17",
              qos_class: "Burstable",
              restart_count: 6,
              main_container: "api",
              containers: [
                { name: "api", image: "checkout:v42", requests: { cpu: "500m", memory: "256Mi" }, limits: { cpu: "1000m", memory: "512Mi" } },
              ],
              log_containers: [
                { name: "api", type: "regular" },
                { name: "istio-proxy", type: "regular" },
                { name: "migrate", type: "init" },
                { name: "certs", type: "init" },
                { name: "debug", type: "ephemeral" },
              ],
              log_container_states: [
                {
                  name: "api",
                  ready: true,
                  running: true,
                  started_at: iso(134),
                  restart_count: 6,
                  last_terminated_reason: "OOMKilled",
                  last_terminated_exit_code: 137,
                  last_terminated_at: iso(8),
                },
                { name: "istio-proxy", ready: true, running: true, started_at: iso(180), restart_count: 0 },
                { name: "migrate", init: true, ready: true, running: false, restart_count: 0, last_terminated_at: "0001-01-01T00:00:00Z" },
                { name: "certs", init: true, ready: true, running: false, restart_count: 0 },
                { name: "debug", ready: true, running: true, restart_count: 0 },
              ],
              config_maps: ["checkout-cfg"],
              secrets: ["checkout-secret"],
              env: [{ name: "DB_PASSWORD", value: "raw-secret-never-shown" }],
            },
          },
          related_resources: [
            { kind: "ReplicaSet", namespace: "shop", name: "checkout-7d9f8c", uid: "rs-7d9f8c" },
            { kind: "Service", namespace: "shop", name: "checkout" },
            { kind: "ConfigMap", namespace: "shop", name: "checkout-cfg" },
            { kind: "Secret", namespace: "shop", name: "checkout-secret" },
          ],
          events: [
            {
              resource_id: "core~v1~events",
              kind: "Event",
              namespace: "shop",
              name: "checkout-backoff",
              uid: "evt-1",
              summary: {
                type: "Warning",
                reason: "BackOff",
                count: 9,
                message: "Back-off restarting failed container api",
                first_timestamp: iso(40),
                last_timestamp: iso(3),
                source_component: "kubelet",
                source_host: "node-a",
              },
            },
            {
              resource_id: "core~v1~events",
              kind: "Event",
              namespace: "shop",
              name: "checkout-pulled",
              uid: "evt-2",
              summary: {
                type: "Normal",
                reason: "Pulled",
                count: 4,
                message: "Container image checkout:v42 already present on node",
                first_timestamp: iso(50),
                last_timestamp: iso(10),
                source_component: "kubelet",
                source_host: "node-a",
              },
            },
          ],
        },
      }),
    );
    await page.route("**/api/admin/kubernetes/workloads/Pod/checkout-7d9f8c-x2k4q**", (route) =>
      route.fulfill({
        json: {
          uid: "pod-x2k4q",
          resource_id: "core~v1~pods",
          kind: "Pod",
          namespace: "shop",
          name: "checkout-7d9f8c-x2k4q",
          containers: [
            { name: "api", image: "checkout:v42", requests: { cpu: "500m", memory: "256Mi" }, limits: { cpu: "1000m", memory: "512Mi" } },
          ],
          pods: [
            { name: "checkout-7d9f8c-x2k4q", uid: "pod-x2k4q", phase: "Running", node: "node-a", restart_count: 6, started_at: iso(134), created_at: "0001-01-01T00:00:00Z" },
          ],
          nodes: ["node-a"],
          usage: [{ kind: "Pod", namespace: "shop", name: "checkout-7d9f8c-x2k4q", cpu: "820m", memory: "480Mi" }],
          truncated: false,
        },
      }),
    );
    await page.route("**/api/admin/kubernetes/graph/neighborhood**", (route) => {
      const url = new URL(route.request().url());
      if (url.searchParams.get("kind") === "Deployment") {
        return route.fulfill({
          json: {
            nodes: [
              { id: "hpa-checkout", kind: "HorizontalPodAutoscaler", namespace: "shop", name: "checkout-hpa", health: "healthy" },
              { id: "deploy-checkout", kind: "Deployment", namespace: "shop", name: "checkout", health: "degraded" },
              { id: "rs-7d9f8c", kind: "ReplicaSet", namespace: "shop", name: "checkout-7d9f8c", health: "healthy" },
              { id: "pod-x2k4q", kind: "Pod", namespace: "shop", name: "checkout-7d9f8c-x2k4q", health: "degraded" },
              { id: "svc-checkout", kind: "Service", namespace: "shop", name: "checkout", health: "healthy" },
            ],
            edges: [
              { from: "hpa-checkout", to: "deploy-checkout", type: "scales" },
              { from: "deploy-checkout", to: "rs-7d9f8c", type: "manages" },
              { from: "rs-7d9f8c", to: "pod-x2k4q", type: "manages" },
              { from: "svc-checkout", to: "pod-x2k4q", type: "exposes" },
            ],
            omitted: {},
            sync: { state: "ready", age_s: 1, partial: false },
          },
        });
      }
      return route.fulfill({
        json: {
          nodes: [
            { id: "deploy-checkout", kind: "Deployment", namespace: "shop", name: "checkout", health: "degraded" },
            { id: "rs-7d9f8c", kind: "ReplicaSet", namespace: "shop", name: "checkout-7d9f8c", health: "healthy" },
            { id: "pod-x2k4q", kind: "Pod", namespace: "shop", name: "checkout-7d9f8c-x2k4q", health: "degraded" },
            { id: "svc-checkout", kind: "Service", namespace: "shop", name: "checkout", health: "healthy" },
            { id: "cm-checkout", kind: "ConfigMap", namespace: "shop", name: "checkout-cfg", health: "healthy" },
            { id: "sec-checkout", kind: "Secret", namespace: "shop", name: "checkout-secret", health: "healthy" },
            { id: "node-a", kind: "Node", name: "node-a", health: "healthy" },
          ],
          edges: [
            { from: "deploy-checkout", to: "rs-7d9f8c", type: "manages" },
            { from: "rs-7d9f8c", to: "pod-x2k4q", type: "manages" },
            { from: "svc-checkout", to: "pod-x2k4q", type: "exposes" },
            { from: "pod-x2k4q", to: "cm-checkout", type: "uses" },
            { from: "pod-x2k4q", to: "sec-checkout", type: "uses" },
            { from: "pod-x2k4q", to: "node-a", type: "runs_on" },
          ],
          omitted: {},
          sync: { state: "ready", age_s: 1, partial: false },
        },
      });
    });
    await page.route("**/api/admin/kubernetes/pods/shop/checkout-7d9f8c-x2k4q/logs/stream**", (route) => {
      const url = new URL(route.request().url());
      if (url.searchParams.get("previous") === "true") {
        return route.fulfill({
          contentType: "text/event-stream",
          body: [
            `event: line\ndata: ${JSON.stringify({ text: "ERROR out of memory: cannot allocate 64 MiB", container: "api", timestamp: iso(9), sequence: 1, ordinal: 1 })}\n\n`,
            `event: end\ndata: {"reason":"complete"}\n\n`,
          ].join(""),
        });
      }
      podStreamReadCount++;
      if (podStreamReadCount === 1) {
        // First read rotates quietly on limit with cursor-rot-1
        return route.fulfill({
          contentType: "text/event-stream",
          body: [
            `event: line\ndata: ${JSON.stringify({ text: "INFO GET /cart 200 18 ms", container: "api", timestamp: iso(2), sequence: 1, ordinal: 1, cursor: "cursor-rot-1" })}\n\n`,
            `event: limit\ndata: ${JSON.stringify({ reason: "duration", cursor: "cursor-rot-1" })}\n\n`,
          ].join(""),
        });
      }
      return route.fulfill({
        contentType: "text/event-stream",
        body: [
          `event: line\ndata: ${JSON.stringify({ text: "WARN slow query orders.find 812 ms", container: "api", timestamp: iso(1), sequence: 2, ordinal: 2, cursor: "cursor-rot-2", replay_uncertain: true })}\n\n`,
          `event: line\ndata: ${JSON.stringify({ text: "ERROR payment client timeout after 5 s (ledger)", container: "api", timestamp: iso(0), sequence: 3, ordinal: 3, cursor: "cursor-rot-3" })}\n\n`,
          `event: heartbeat\ndata: {"cursor":"cursor-rot-3"}\n\n`,
          `event: end\ndata: {"reason":"complete","cursor":"cursor-rot-3"}\n\n`,
        ].join(""),
      });
    });

    // 25-Pod Deployment fixture for over-limit dropdown and focused Pod capacity preservation
    const overLimitPods = Array.from({ length: 25 }, (_, i) => ({
      uid: `pod-uid-${i}`,
      name: `checkout-pod-${String(i).padStart(2, "0")}`,
      containers: ["api", "istio-proxy"],
      init_containers: ["migrate"],
      main_container: "api",
      admitted: i < 20,
      ...(i >= 20 ? { reason: "source_limit" } : {}),
      all_pods_admitted: i < 20,
      ...(i >= 20 ? { all_pods_reason: "source_limit" } : {}),
    }));
    await page.route("**/api/admin/kubernetes/resources/apps~v1~deployments/checkout/describe**", (route) =>
      route.fulfill({
        json: {
          resource: {
            resource_id: "apps~v1~deployments",
            api_version: "apps/v1",
            kind: "Deployment",
            namespace: "shop",
            name: "checkout",
            uid: "deploy-checkout",
            created_at: "0001-01-01T00:00:00Z",
            conditions: [{ type: "Available", status: "True", reason: "MinimumReplicasAvailable", last_transition_at: "0001-01-01T00:00:00Z" }],
            summary: { ready_replicas: 3, desired_replicas: 4, containers: [{ name: "api", image: "checkout:v42" }] },
          },
          related_resources: [{ kind: "ReplicaSet", namespace: "shop", name: "checkout-7d9f8c", uid: "rs-7d9f8c" }],
          events: [],
        },
      }),
    );
    await page.route("**/api/admin/kubernetes/workloads/Deployment/checkout**", (route) => {
      const url = new URL(route.request().url());
      if (url.pathname.endsWith("/logs/stream")) {
        workloadStreamRequests.push(url);
        const focusedUID = url.searchParams.get("pod_uid");
        const isPrevious = url.searchParams.get("previous") === "true";
        const isAllContainers = url.searchParams.get("container") === "*";
        const podsPayload = overLimitPods.map((p, idx) => ({
          ...p,
          admitted: focusedUID ? p.uid === focusedUID : idx < 20,
          reason: focusedUID ? (p.uid === focusedUID ? "" : "not_selected") : idx >= 20 ? "source_limit" : "",
        }));
        if (isPrevious) {
          return route.fulfill({
            contentType: "text/event-stream",
            body: [
              `event: sources\ndata: ${JSON.stringify({ workload_uid: "deploy-checkout", pods: podsPayload, truncated: false })}\n\n`,
              `event: line\ndata: ${JSON.stringify({ workload_uid: "deploy-checkout", pod_uid: "pod-uid-0", pod: "checkout-pod-00", namespace: "shop", container: "api", incarnation: "1", source_id: "src-0", text: "previous crash panic: nil pointer", timestamp: iso(5), sequence: 1, ordinal: 1 })}\n\n`,
              `event: end\ndata: {"reason":"complete"}\n\n`,
            ].join(""),
          });
        }
        return route.fulfill({
          contentType: "text/event-stream",
          body: [
            `event: sources\ndata: ${JSON.stringify({ workload_uid: "deploy-checkout", pods: podsPayload, truncated: false })}\n\n`,
            `event: line\ndata: ${JSON.stringify({ workload_uid: "deploy-checkout", pod_uid: "pod-uid-0", pod: "checkout-pod-00", namespace: "shop", container: "api", incarnation: "1", source_id: "src-0", text: "GET /cart 200 18 ms", timestamp: iso(2), sequence: 1, ordinal: 1, cursor: "wl-cur-1" })}\n\n`,
            ...(focusedUID
              ? [`event: line\ndata: ${JSON.stringify({ workload_uid: "deploy-checkout", pod_uid: focusedUID, pod: podsPayload.find((p) => p.uid === focusedUID)?.name ?? "checkout-pod-24", namespace: "shop", container: "api", incarnation: "1", source_id: "src-focus", text: "focused pod exclusive line", timestamp: iso(1), sequence: 2, ordinal: 2, cursor: "wl-cur-2" })}\n\n`]
              : [
                  `event: line\ndata: ${JSON.stringify({ workload_uid: "deploy-checkout", pod_uid: "pod-uid-1", pod: "checkout-pod-01", namespace: "shop", container: isAllContainers ? "istio-proxy" : "api", incarnation: "1", source_id: "src-1", text: "POST /checkout 201 311 ms", timestamp: iso(1), sequence: 2, ordinal: 2, cursor: "wl-cur-2" })}\n\n`,
                  `event: source_status\ndata: ${JSON.stringify({ workload_uid: "deploy-checkout", pod_uid: "pod-uid-2", pod: "checkout-pod-02", namespace: "shop", container: "api", source_id: "src-2", code: "forbidden", message: "forbidden" })}\n\n`,
                  `event: line\ndata: ${JSON.stringify({ workload_uid: "deploy-checkout", pod_uid: "pod-uid-0", pod: "checkout-pod-00", namespace: "shop", container: "api", incarnation: "1", source_id: "src-0", text: "GET /checkout/quote 200 42 ms", timestamp: iso(0), sequence: 3, ordinal: 3, cursor: "wl-cur-3", replay_uncertain: true })}\n\n`,
                ]),
            `event: heartbeat\ndata: {"cursor":"wl-cur-3"}\n\n`,
            `event: end\ndata: {"reason":"complete","cursor":"wl-cur-3"}\n\n`,
          ].join(""),
        });
      }
      return route.fulfill({
        json: {
          uid: "deploy-checkout",
          resource_id: "apps~v1~deployments",
          kind: "Deployment",
          namespace: "shop",
          name: "checkout",
          desired: 4,
          ready: 3,
          available: 3,
          containers: [{ name: "api", image: "checkout:v42", requests: { cpu: "250m", memory: "256Mi" }, limits: { cpu: "500m", memory: "512Mi" } }],
          pods: [
            { name: "checkout-7d9f8c-x2k4q", uid: "pod-x2k4q", phase: "Running", node: "node-a", restart_count: 6, started_at: iso(134), created_at: "0001-01-01T00:00:00Z" },
            { name: "checkout-8b6c9d-a6v2n", uid: "pod-a6v2n", phase: "Running", node: "node-b", restart_count: 0, started_at: iso(90), created_at: "0001-01-01T00:00:00Z" },
          ],
          nodes: ["node-a", "node-b"],
          truncated: false,
        },
      });
    });

    // 1. Open Pod drawer via legacy tab=diagnosis -> normalizes to Overview
    await page.goto("/agent/kubernetes?r=core~v1~pods/shop/checkout-7d9f8c-x2k4q&tab=diagnosis");
    const detail = page.getByRole("dialog", { name: "Details panel" });
    await expect(detail).toBeVisible();
    await expect(page).not.toHaveURL(/tab=diagnosis/);
    const tabs = detail.getByRole("tablist", { name: "Resource detail views" });
    await expect(tabs.getByRole("tab")).toHaveText(["Overview", "Logs", "Events", "YAML", "Related", "Timeline"]);
    await expect(tabs.getByRole("tab", { name: "Overview", exact: true })).toHaveAttribute("aria-selected", "true");

    // Verify CPU (>75% amber) and Memory (>90% critical red) thresholds, Restarts (6, OOMKilled), Uptime (2h 14m)
    const cpuTile = detail.getByLabel("CPU metric tile", { exact: true });
    const memTile = detail.getByLabel("Memory metric tile", { exact: true });
    await expect(cpuTile).toContainText("820m");
    await expect(cpuTile).toContainText("82%");
    await expect(cpuTile.locator("p.text-sev-warning")).toBeVisible();
    await expect(memTile).toContainText("480 MiB");
    await expect(memTile).toContainText("94%");
    await expect(memTile.locator("p.text-sev-critical")).toBeVisible();
    await expect(detail.getByLabel("24 hour crash ticks").getByRole("button")).toHaveCount(1);
    await expect(detail).toContainText("2h 14m");

    // Verify Details & Conditions zero-time protection (Initialized has 0001-01-01T00:00:00Z -> no 1/1/1)
    await expect(detail).toContainText("10.42.1.17");
    await expect(detail).toContainText("Burstable");
    await expect(detail).not.toContainText("739910d");
    await expect(detail).not.toContainText("1/1/1");
    await expect(detail).toContainText("Last state api: OOMKilled exit 137");

    // Verify compact Pod container chips (+1 button for 5th container) and quiet rotation + neutral reconnect divider
    const containerGroup = detail.getByRole("group", { name: "Log containers", exact: true });
    await expect(containerGroup.getByRole("button", { name: "+1", exact: true })).toBeVisible();
    await containerGroup.getByRole("button", { name: "+1", exact: true }).click();
    await expect(containerGroup.getByRole("button", { name: "debug", exact: true })).toBeVisible();
    const podLogOutput = detail.getByLabel("Pod log output");
    await expect(podLogOutput).toContainText("GET /cart 200 18 ms");
    await expect(podLogOutput).toContainText("resumed after reconnect at");
    await expect(podLogOutput).toContainText("payment client timeout after 5 s (ledger)");
    await expect(detail.getByRole("alert")).toHaveCount(0);
    await captureBreakState(page, "k14-fixture-pod-overview-desktop-dark.png", "dark");

    // Mobile 390x844 Pod Overview: verify 3x2 non-overflowing tab grid and visible container chips outside More
    await page.setViewportSize({ width: 390, height: 844 });
    await tabs.scrollIntoViewIfNeeded();
    const mobileTabOverflow = await tabs.evaluate((el) => el.scrollWidth > el.clientWidth);
    expect(mobileTabOverflow).toBe(false);
    await expect(tabs.getByRole("tab", { name: "Timeline", exact: true })).toBeInViewport({ ratio: 1 });
    await containerGroup.scrollIntoViewIfNeeded();
    await expect(containerGroup).toBeVisible();
    await tabs.scrollIntoViewIfNeeded();
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
    await captureBreakState(page, "k14-fixture-pod-overview-mobile-dark.png", "dark");
    await page.setViewportSize({ width: 1440, height: 1000 });

    // Switch to Logs tab via "Open full logs →": verify shared session (no extra stream read)
    const readsBeforeFullLogs = podStreamReadCount;
    await detail.getByRole("button", { name: "Open full logs", exact: true }).click();
    await expect(tabs.getByRole("tab", { name: "Logs", exact: true })).toHaveAttribute("aria-selected", "true");
    expect(podStreamReadCount).toBe(readsBeforeFullLogs);
    await expect(podLogOutput).toContainText("GET /cart 200 18 ms");
    await captureBreakState(page, "k14-fixture-pod-logs-desktop-dark.png", "dark");

    // Events tab: occurrence counts (All 13, Warnings 9, Normal 4), histogram, reason groups, source component/host
    await tabs.getByRole("tab", { name: "Events", exact: true }).click();
    const eventsRegion = detail.getByRole("region", { name: "Object-scoped events", exact: true });
    await expect(eventsRegion.getByRole("button", { name: "All 13", exact: true })).toBeVisible();
    await expect(eventsRegion.getByRole("button", { name: "Warnings 9", exact: true })).toBeVisible();
    await expect(eventsRegion.getByRole("button", { name: "Normal 4", exact: true })).toBeVisible();
    await expect(eventsRegion.getByRole("img", { name: "Event histogram: warnings red, normal gray", exact: true })).toBeVisible();
    await expect(eventsRegion).toContainText("kubelet node-a");
    await captureBreakState(page, "k14-fixture-pod-events-desktop-dark.png", "dark");

    // YAML tab: Projected, read-only badge, Outline, Fold all / Unfold all, Find in YAML unfolding ancestor, redaction, zero-time stripped
    await tabs.getByRole("tab", { name: "YAML", exact: true }).click();
    const yamlRegion = detail.getByRole("region", { name: "Projected YAML", exact: true });
    await expect(yamlRegion.getByText("Projected, read-only", { exact: true })).toBeVisible();
    await yamlRegion.getByRole("button", { name: "Unfold all", exact: true }).click();
    await expect(yamlRegion).toContainText("‹redacted›");
    await expect(yamlRegion).not.toContainText("raw-secret-never-shown");
    await expect(yamlRegion).not.toContainText("0001-01-01T00:00:00Z");
    await expect(yamlRegion.getByRole("navigation", { name: "YAML outline", exact: true })).toContainText("metadata");
    await expect(yamlRegion.getByRole("navigation", { name: "YAML outline", exact: true })).toContainText("spec");
    await yamlRegion.getByRole("button", { name: "Fold all", exact: true }).click();
    await expect(yamlRegion).toContainText(/▸ .+ \(\d+ lines\)/);
    await yamlRegion.getByLabel("Find in YAML", { exact: true }).fill("10.42.1.17");
    await yamlRegion.getByRole("button", { name: "Next match", exact: true }).click();
    await expect(yamlRegion).toContainText("10.42.1.17");
    await yamlRegion.getByRole("button", { name: "Unfold all", exact: true }).click();
    await captureBreakState(page, "k14-fixture-pod-yaml-desktop-dark.png", "dark");

    // Timeline tab: 1h/6h/24h, lane, field diff tokens (checkout:v41 → checkout:v42), crash item -> View logs before crash
    await tabs.getByRole("tab", { name: "Timeline", exact: true }).click();
    const timelineRegion = detail.getByRole("region", { name: "Resource change history", exact: true });
    await expect(timelineRegion.getByLabel("Resource timeline lane", { exact: true })).toBeVisible();
    await expect(timelineRegion).toContainText("checkout:v41");
    await expect(timelineRegion).toContainText("checkout:v42");
    await expect(timelineRegion).toContainText("Crash history starts");
    await expect(timelineRegion).toContainText("Partial history; capture gaps or bounded results may omit events in this range.");
    await captureBreakState(page, "k14-fixture-pod-timeline-desktop-dark.png", "dark");

    await timelineRegion.getByRole("button", { name: "View logs before crash", exact: true }).click();
    await expect(tabs.getByRole("tab", { name: "Logs", exact: true })).toHaveAttribute("aria-selected", "true");
    await expect(detail.getByLabel("Previous container logs")).toHaveAttribute("aria-pressed", "true");
    await expect(podLogOutput).toContainText("out of memory: cannot allocate 64 MiB");

    // Related tab: owner chain, Secret "content not shown", and Open navigation to Deployment checkout
    await tabs.getByRole("tab", { name: "Related", exact: true }).click();
    const relatedRegion = detail.getByRole("region", { name: "Related resources", exact: true });
    await expect(relatedRegion.getByLabel("Relationship map", { exact: true })).toContainText("DEPLOYMENT");
    await expect(relatedRegion.getByLabel("Relationship map", { exact: true })).toContainText("REPLICASET");
    await expect(relatedRegion).toContainText("content not shown");
    await captureBreakState(page, "k14-fixture-pod-related-desktop-dark.png", "dark");

    // Navigate to Deployment checkout via Related -> Map button
    await relatedRegion.getByLabel("Relationship map", { exact: true }).getByRole("button", { name: "Open Deployment checkout", exact: true }).click();
    await expect(detail.getByRole("heading", { name: "Deployment shop/checkout", exact: true })).toBeVisible();

    // Verify Deployment Overview: partial usage history ("Available samples · 1/2 tracked Pods", no fabricated 100%/0%), muted Unavailable Memory when incomplete
    await expect(detail.getByLabel("CPU metric tile", { exact: true })).toContainText("Available samples · 1/2 tracked Pods");
    await expect(detail).not.toContainText("739910d");
    await expect(detail).not.toContainText("1/1/1");

    // Verify Deployment Related tab classifiesReplicaSet/Pod under "Owns" and HPA under "Selected by"
    await tabs.getByRole("tab", { name: "Related", exact: true }).click();
    await expect(relatedRegion.getByRole("heading", { name: "Owns", exact: true })).toBeVisible();
    await expect(relatedRegion.getByRole("heading", { name: "Selected by", exact: true })).toBeVisible();

    // Verify Deployment Logs tab: 25-Pod over-limit dropdown ("All pods (first 20)") preserved when focusing pod-uid-24
    await tabs.getByRole("tab", { name: "Logs", exact: true }).click();
    const wlOutput = detail.getByLabel("Workload log output");
    await expect(wlOutput).toContainText("checkout-pod-00");
    await expect(wlOutput).toContainText("logs not available: forbidden");
    await expect(wlOutput).toContainText("reconnected at");
    const podSelect = detail.getByLabel("Log pod", { exact: true });
    await expect(podSelect.locator("option").first()).toHaveText("All pods (first 20)");
    await expect(podSelect.locator('option[value="pod-uid-24"]')).toHaveText("checkout-pod-24 (not in all pods)");
    await captureBreakState(page, "k14-fixture-deployment-logs-desktop-dark.png", "dark");

    // Select extra Pod #24 and verify "All pods (first 20)" and "(not in all pods)" remain intact!
    await podSelect.selectOption("pod-uid-24");
    await expect(wlOutput).toContainText("focused pod exclusive line");
    await expect(podSelect.locator("option").first()).toHaveText("All pods (first 20)");
    await expect(podSelect.locator('option[value="pod-uid-24"]')).toHaveText("checkout-pod-24 (not in all pods)");
    expect(errors).toEqual([]);
  });


  test("captures and verifies all 12 K14 SVG-comparable frames plus CrashJournal R1 and Agent learning bottom disabled", async ({ page, context }, testInfo) => {
    test.setTimeout(180_000);
    await context.grantPermissions(["clipboard-read", "clipboard-write"]);
    const pageErrors: string[] = [];
    const consoleErrors: string[] = [];
    page.on("pageerror", (error) => pageErrors.push(error.message));
    page.on("console", (msg) => {
      if (msg.type() === "error" && !msg.text().includes("404 (Not Found)")) consoleErrors.push(msg.text());
    });

    const nowMs = Date.now();
    const iso = (offsetMinutes: number) => new Date(nowMs - offsetMinutes * 60_000).toISOString();

    await page.setViewportSize({ width: 1440, height: 900 });
    await page.emulateMedia({ colorScheme: "dark" });
    await page.addInitScript(() => localStorage.setItem("versus.theme", "dark"));

    // Authenticate properly via openApp so the browser context holds a valid gateway session cookie and never triggers 401 Session expired.
    await openApp(page, "/now");
    await expect(page.getByRole("dialog", { name: "Session expired" })).toHaveCount(0);
    await page.route("**/api/agent/baselines**", (route) => route.fulfill({ json: { baselines: [] } }));
    consoleErrors.length = 0;
    page.on("response", (res) => {
      if (res.status() >= 400 && !res.url().includes("/enterprise/api/sso/deployment")) {
        consoleErrors.push(`HTTP ${res.status()} ${res.url()}`);
      }
    });

    const podsPayload = [
      { uid: "p1", name: "checkout-7d9f8c-x2k4q", containers: ["api", "istio-proxy"], init_containers: ["migrate"], main_container: "api", admitted: true },
      { uid: "p2", name: "checkout-7d9f8c-p9m1z", containers: ["api", "istio-proxy"], init_containers: ["migrate"], main_container: "api", admitted: true },
      { uid: "p3", name: "checkout-8b6c9d-a6v2n", containers: ["api", "istio-proxy"], init_containers: ["migrate"], main_container: "api", admitted: true },
      { uid: "p4", name: "checkout-8b6c9d-b8r3s", containers: ["api", "istio-proxy"], init_containers: ["migrate"], main_container: "api", admitted: true },
    ];
    const stsPods = ["ledger-0", "ledger-1", "ledger-2"].map((name, i) => ({
      uid: `sts-${i}`, name, containers: ["api"], init_containers: [], main_container: "api", admitted: true,
    }));

    await page.route("**/api/admin/kubernetes/**", (route) => {
      const url = new URL(route.request().url());
      const pathname = url.pathname;
      if (pathname === "/api/admin/kubernetes/clusters") {
        return route.fulfill({ json: { multiple: false, clusters: [{ id: "default", provider: "generic", health: "healthy", nodes: 2, ready_nodes: 2, pods: 6, running_pods: 6, warnings: 2 }] } });
      }
      if (pathname === "/api/admin/kubernetes/stream") {
        return route.fulfill({ contentType: "text/event-stream", body: `event: sync\ndata: ${JSON.stringify({ state: "ready", age_s: 1, partial: false })}\n\nevent: heartbeat\ndata: {}\n\n` });
      }
      if (pathname === "/api/admin/kubernetes/overview") {
        return route.fulfill({ json: { connector: "kubernetes", cluster_id: "default", observed_at: iso(0), nodes: 2, ready_nodes: 2, pods: 6, running_pods: 6, namespaces: 2, active_namespaces: 2, workloads: 3, warnings: 2, truncated: false } });
      }
      if (pathname === "/api/admin/kubernetes/usage-history") {
        return route.fulfill({
          json: {
            observed_at: iso(0), source: "capture", availability: "available", live_since: iso(60), retention_seconds: 21600,
            samples: [
              { timestamp: iso(50), cpu_milli: 320, memory_bytes: 280 * 1024 * 1024 },
              { timestamp: iso(35), cpu_milli: 360, memory_bytes: 320 * 1024 * 1024 },
              { timestamp: iso(20), cpu_milli: 390, memory_bytes: 360 * 1024 * 1024 },
              { timestamp: iso(10), cpu_milli: 450, memory_bytes: 490 * 1024 * 1024 },
              { timestamp: iso(5), cpu_milli: 380, memory_bytes: 350 * 1024 * 1024 },
              { timestamp: iso(1), cpu_milli: 412, memory_bytes: 389 * 1024 * 1024 },
            ],
          },
        });
      }
      if (pathname === "/api/admin/kubernetes/usage") {
        return route.fulfill({ json: { observed_at: iso(0), availability: "available", fresh: true, truncated: false, pods: [{ kind: "Pod", namespace: "shop", name: "checkout-7d9f8c-x2k4q", cpu: "412m", memory: "389Mi", window: "30s" }], nodes: [] } });
      }
      if (pathname === "/api/admin/kubernetes/crashes") {
        return route.fulfill({
          json: {
            items: [
              { id: "c1", sequence: 1, cluster: "default", namespace: "shop", pod: "checkout-7d9f8c-x2k4q", pod_uid: "pod-x2k4q", root_uid: "deploy-checkout", root_kind: "Deployment", root_name: "checkout", container: "api", type: "termination", termination_slot: "previous", exit_code: 137, reason: "OOMKilled", restart_count: 6, at: iso(5), time_source: "finished_at", observed_at: iso(5) },
              { id: "c2", sequence: 2, cluster: "default", namespace: "shop", pod: "checkout-7d9f8c-x2k4q", pod_uid: "pod-x2k4q", root_uid: "deploy-checkout", root_kind: "Deployment", root_name: "checkout", container: "api", type: "termination", termination_slot: "previous", exit_code: 137, reason: "OOMKilled", restart_count: 5, at: iso(134), time_source: "finished_at", observed_at: iso(134) },
            ],
            since: url.searchParams.get("since") || iso(1440),
            until: url.searchParams.get("until") || iso(0),
            retention_from: iso(1440),
            retention_basis: "observed_at",
            started_at: iso(170),
            gaps: [],
            capture_gaps: [],
            limitations: ["polling_observations_only", "summary_caps_do_not_limit_journal", "unknown_termination_identity_is_ambiguous"],
            sync: { state: "ready", age_s: 1, partial: false },
            capture: { mode: "continuous", behind: false, last_cycle_at: iso(0) },
            coverage: { Pod: { from: url.searchParams.get("since") || iso(1440), to: url.searchParams.get("until") || iso(0) } },
          },
        });
      }
      if (pathname === "/api/admin/kubernetes/changes") {
        return route.fulfill({
          json: {
            items: [
              { id: "chg-1", cluster: "default", kind: "Pod", namespace: "shop", name: "checkout-7d9f8c-x2k4q", uid: "pod-x2k4q", type: "image_changed", at: iso(12), fields: [{ path: "image", from: "checkout:v41", to: "checkout:v42" }] },
              { id: "chg-2", cluster: "default", kind: "Pod", namespace: "shop", name: "checkout-7d9f8c-x2k4q", uid: "pod-x2k4q", type: "referenced_config_changed", at: iso(30), fields: [{ path: "checkout-cfg revision", from: "11", to: "12" }] },
            ],
            gaps: [],
            sync: { state: "ready", age_s: 1, partial: false },
          },
        });
      }
      if (pathname === "/api/admin/kubernetes/issues") {
        return route.fulfill({
          json: {
            items: [{ root: { kind: "Pod", namespace: "shop", name: "checkout-7d9f8c-x2k4q", uid: "pod-x2k4q" }, rule: "pod.crash_loop", severity: "critical", count: 1, of: 1, broken: true, broke_at: iso(6), history: "complete" }],
            totals: { critical: 1, warning: 0 },
            broken: { total: 1, by_severity: { critical: 1 } },
            truncated: false,
            sync: { state: "ready", age_s: 1, partial: false },
          },
        });
      }
      if (pathname === "/api/admin/kubernetes/break-logs") {
        return route.fulfill({
          json: {
            window: { from: url.searchParams.get("since") || iso(15), to: url.searchParams.get("until") || iso(0) },
            pods: [{ name: "checkout-7d9f8c-x2k4q", uid: "pod-x2k4q", short: "x2k4q", state: "running" }],
            lines: [{ at: iso(5), pod: "checkout-7d9f8c-x2k4q", pod_uid: "pod-x2k4q", container: "api", init: false, slot: "previous", level: "error", text: "payment client timeout after 5 s (ledger)", source: "captured" }],
            markers: [{ type: "break", at: iso(6), label: "Pod checkout-7d9f8c-x2k4q crash looping" }],
            counts: { all: 1, errors: 1, warnings: 0 },
            rate: [{ minute: iso(5), total: 1, errors: 1 }],
            patterns: [{ template: "payment client timeout", level: "error", count: 1 }],
            sources: [{ kind: "captured", name: "default", at: iso(5) }],
            coverage: { state: "complete", reason: "", tip: false, gaps: [] },
          },
        });
      }
      if (pathname === "/api/admin/kubernetes/resources/core~v1~pods/checkout-7d9f8c-x2k4q/describe") {
        return route.fulfill({
          json: {
            resource: {
              resource_id: "core~v1~pods", api_version: "v1", kind: "Pod", namespace: "shop", name: "checkout-7d9f8c-x2k4q", uid: "pod-x2k4q", created_at: iso(4320),
              owners: [{ api_version: "apps/v1", kind: "ReplicaSet", namespace: "shop", name: "checkout-7d9f8c", uid: "rs-7d9f8c" }],
              conditions: [
                { type: "Ready", status: "True", reason: "ContainersReady", last_transition_at: iso(5) },
                { type: "ContainersReady", status: "True", reason: "ContainersReady", last_transition_at: iso(5) },
                { type: "Initialized", status: "True", reason: "PodCompleted", last_transition_at: iso(4320) },
                { type: "PodScheduled", status: "True", reason: "Scheduled", last_transition_at: iso(4320) },
              ],
              summary: {
                phase: "Running", node: "node-a", pod_ip: "10.42.1.17", qos_class: "Burstable", restart_count: 6, main_container: "api",
                containers: [{ name: "api", image: "checkout:v42", requests: { cpu: "500m", memory: "256Mi" }, limits: { cpu: "1000m", memory: "512Mi" } }],
                log_containers: [{ name: "api", type: "regular" }, { name: "istio-proxy", type: "regular" }, { name: "migrate", type: "init" }],
                log_container_states: [
                  { name: "api", ready: true, running: true, started_at: iso(134), restart_count: 6, last_terminated_reason: "OOMKilled", last_terminated_exit_code: 137, last_terminated_at: iso(5) },
                  { name: "istio-proxy", ready: true, running: true, started_at: iso(180), restart_count: 0 },
                  { name: "migrate", init: true, ready: true, running: false, restart_count: 0 },
                ],
                config_maps: ["checkout-cfg"], secrets: ["checkout-db"],
                env: [{ name: "LOG_LEVEL", value: "info" }, { name: "DB_PASSWORD", value: "secret" }, { name: "TRAILING_COLON", value: "prefix:" }],
              },
            },
            related_resources: [
              { kind: "ReplicaSet", namespace: "shop", name: "checkout-7d9f8c", uid: "rs-7d9f8c" },
              { kind: "Service", namespace: "shop", name: "checkout" },
              { kind: "ConfigMap", namespace: "shop", name: "checkout-cfg" },
              { kind: "Secret", namespace: "shop", name: "checkout-db" },
            ],
            events: [
              { resource_id: "core~v1~events", kind: "Event", namespace: "shop", name: "e1", uid: "e1", summary: { type: "Warning", reason: "BackOff", count: 12, message: "Back-off restarting failed container api", first_timestamp: iso(40), last_timestamp: iso(1), source_component: "kubelet", source_host: "node-a" } },
              { resource_id: "core~v1~events", kind: "Event", namespace: "shop", name: "e2", uid: "e2", summary: { type: "Warning", reason: "OOMKilling", count: 6, message: "Container api was stopped for going over its 512 MiB memory limit", first_timestamp: iso(134), last_timestamp: iso(5), source_component: "kubelet", source_host: "node-a" } },
              { resource_id: "core~v1~events", kind: "Event", namespace: "shop", name: "e3", uid: "e3", summary: { type: "Warning", reason: "Unhealthy", count: 4, message: "Readiness probe failed: HTTP 503 on /healthz", first_timestamp: iso(8), last_timestamp: iso(5), source_component: "kubelet", source_host: "node-a" } },
              { resource_id: "core~v1~events", kind: "Event", namespace: "shop", name: "e4", uid: "e4", summary: { type: "Normal", reason: "Pulled", count: 6, message: "Container image checkout:v42 already present on node", first_timestamp: iso(170), last_timestamp: iso(5), source_component: "kubelet", source_host: "node-a" } },
              { resource_id: "core~v1~events", kind: "Event", namespace: "shop", name: "e5", uid: "e5", summary: { type: "Normal", reason: "Started", count: 7, message: "Started container api", first_timestamp: iso(170), last_timestamp: iso(5), source_component: "kubelet", source_host: "node-a" } },
              { resource_id: "core~v1~events", kind: "Event", namespace: "shop", name: "e6", uid: "e6", summary: { type: "Normal", reason: "Scheduled", count: 1, message: "Assigned shop/checkout-7d9f8c-x2k4q to node-a", first_timestamp: iso(170), last_timestamp: iso(170), source_component: "default-scheduler", source_host: "" } },
            ],
          },
        });
      }
      if (pathname === "/api/admin/kubernetes/workloads/Pod/checkout-7d9f8c-x2k4q") {
        return route.fulfill({
          json: {
            uid: "pod-x2k4q", resource_id: "core~v1~pods", kind: "Pod", namespace: "shop", name: "checkout-7d9f8c-x2k4q",
            containers: [{ name: "api", image: "checkout:v42", requests: { cpu: "500m", memory: "256Mi" }, limits: { cpu: "1000m", memory: "512Mi" } }],
            pods: [{ name: "checkout-7d9f8c-x2k4q", uid: "pod-x2k4q", phase: "Running", node: "node-a", restart_count: 6, started_at: iso(134), created_at: iso(4320) }],
            nodes: ["node-a"], usage: [{ kind: "Pod", namespace: "shop", name: "checkout-7d9f8c-x2k4q", cpu: "412m", memory: "389Mi" }], truncated: false,
          },
        });
      }
      if (pathname === "/api/admin/kubernetes/graph/neighborhood") {
        return route.fulfill({
          json: {
            nodes: [
              { id: "hpa-checkout", kind: "HorizontalPodAutoscaler", namespace: "shop", name: "checkout-hpa", health: "healthy" },
              { id: "deploy-checkout", kind: "Deployment", namespace: "shop", name: "checkout", health: "degraded" },
              { id: "rs-7d9f8c", kind: "ReplicaSet", namespace: "shop", name: "checkout-7d9f8c", health: "healthy" },
              { id: "pod-x2k4q", kind: "Pod", namespace: "shop", name: "checkout-7d9f8c-x2k4q", health: "degraded" },
              { id: "svc-checkout", kind: "Service", namespace: "shop", name: "checkout", health: "healthy" },
              { id: "cm-checkout", kind: "ConfigMap", namespace: "shop", name: "checkout-cfg", health: "unknown" },
              { id: "sec-checkout", kind: "Secret", namespace: "shop", name: "checkout-db", health: "unknown" },
              { id: "node-a", kind: "Node", name: "node-a", health: "healthy" },
            ],
            edges: [
              { from: "hpa-checkout", to: "deploy-checkout", type: "scales" },
              { from: "deploy-checkout", to: "rs-7d9f8c", type: "manages" },
              { from: "rs-7d9f8c", to: "pod-x2k4q", type: "manages" },
              { from: "svc-checkout", to: "pod-x2k4q", type: "exposes" },
              { from: "pod-x2k4q", to: "cm-checkout", type: "uses" },
              { from: "pod-x2k4q", to: "sec-checkout", type: "uses" },
              { from: "pod-x2k4q", to: "node-a", type: "runs_on" },
            ],
            omitted: {}, sync: { state: "ready", age_s: 1, partial: false },
          },
        });
      }
      if (pathname === "/api/admin/kubernetes/pods/shop/checkout-7d9f8c-x2k4q/logs/stream") {
        return route.fulfill({
          contentType: "text/event-stream",
          body: [
            `event: line\ndata: ${JSON.stringify({ text: "INFO GET /cart 200 18 ms", container: "api", timestamp: iso(0.2), sequence: 1, ordinal: 1, cursor: "c1" })}\n\n`,
            `event: line\ndata: ${JSON.stringify({ text: "INFO GET /checkout/quote 200 42 ms", container: "api", timestamp: iso(0.18), sequence: 2, ordinal: 2, cursor: "c2" })}\n\n`,
            `event: line\ndata: ${JSON.stringify({ text: "WARN slow query orders.find 812 ms", container: "api", timestamp: iso(0.15), sequence: 3, ordinal: 3, cursor: "c3" })}\n\n`,
            `event: line\ndata: ${JSON.stringify({ text: "INFO GET /cart 200 16 ms", container: "api", timestamp: iso(0.12), sequence: 4, ordinal: 4, cursor: "c4", replay_uncertain: true })}\n\n`,
            `event: line\ndata: ${JSON.stringify({ text: "ERROR payment client timeout after 5 s (ledger)", container: "api", timestamp: iso(0.08), sequence: 5, ordinal: 5, cursor: "c5" })}\n\n`,
            `event: line\ndata: ${JSON.stringify({ text: "INFO retry 1 payment ok 230 ms", container: "api", timestamp: iso(0.05), sequence: 6, ordinal: 6, cursor: "c6" })}\n\n`,
            `event: line\ndata: ${JSON.stringify({ text: "INFO POST /checkout 201 311 ms", container: "api", timestamp: iso(0.02), sequence: 7, ordinal: 7, cursor: "c7" })}\n\n`,
            `event: line\ndata: ${JSON.stringify({ text: "INFO GET /cart 200 17 ms", container: "api", timestamp: iso(0), sequence: 8, ordinal: 8, cursor: "c8" })}\n\n`,
            `event: heartbeat\ndata: {"cursor":"c8"}\n\n`,
            `event: end\ndata: {"reason":"complete","cursor":"c8"}\n\n`,
          ].join(""),
        });
      }
      if (pathname === "/api/admin/kubernetes/resources/apps~v1~deployments/checkout/describe") {
        return route.fulfill({
          json: {
            resource: {
              resource_id: "apps~v1~deployments", api_version: "apps/v1", kind: "Deployment", namespace: "shop", name: "checkout", uid: "deploy-checkout", created_at: iso(4320),
              conditions: [{ type: "Available", status: "True", reason: "MinimumReplicasAvailable", last_transition_at: iso(134) }],
              summary: { ready_replicas: 3, desired_replicas: 4, main_container: "api", containers: [{ name: "api", image: "checkout:v42" }] },
            },
            related_resources: [], events: [],
          },
        });
      }
      if (pathname === "/api/admin/kubernetes/workloads/Deployment/checkout/logs/stream") {
        return route.fulfill({
          contentType: "text/event-stream",
          body: [
            `event: sources\ndata: ${JSON.stringify({ workload_uid: "deploy-checkout", pods: podsPayload, truncated: false })}\n\n`,
            `event: line\ndata: ${JSON.stringify({ workload_uid: "deploy-checkout", pod_uid: "p1", pod: "checkout-7d9f8c-x2k4q", namespace: "shop", container: "api", incarnation: "1", source_id: "s1", text: "GET /cart 200 18 ms", timestamp: iso(0.2), sequence: 1, ordinal: 1, cursor: "wc1" })}\n\n`,
            `event: line\ndata: ${JSON.stringify({ workload_uid: "deploy-checkout", pod_uid: "p3", pod: "checkout-8b6c9d-a6v2n", namespace: "shop", container: "api", incarnation: "1", source_id: "s3", text: "GET /checkout/quote 200 42 ms", timestamp: iso(0.18), sequence: 2, ordinal: 2, cursor: "wc2" })}\n\n`,
            `event: line\ndata: ${JSON.stringify({ workload_uid: "deploy-checkout", pod_uid: "p4", pod: "checkout-8b6c9d-b8r3s", namespace: "shop", container: "api", incarnation: "1", source_id: "s4", text: "cache hit ratio 0.94", timestamp: iso(0.15), sequence: 3, ordinal: 3, cursor: "wc3" })}\n\n`,
            `event: source_status\ndata: ${JSON.stringify({ workload_uid: "deploy-checkout", pod_uid: "p2", pod: "checkout-7d9f8c-p9m1z", namespace: "shop", container: "api", source_id: "s2", code: "forbidden", message: "forbidden" })}\n\n`,
            `event: line\ndata: ${JSON.stringify({ workload_uid: "deploy-checkout", pod_uid: "p1", pod: "checkout-7d9f8c-x2k4q", namespace: "shop", container: "api", incarnation: "1", source_id: "s1", text: "GET /cart 200 17 ms", timestamp: iso(0.1), sequence: 4, ordinal: 4, cursor: "wc4", replay_uncertain: true })}\n\n`,
            `event: heartbeat\ndata: {"cursor":"wc4"}\n\n`,
            `event: end\ndata: {"reason":"complete","cursor":"wc4"}\n\n`,
          ].join(""),
        });
      }
      if (pathname === "/api/admin/kubernetes/workloads/Deployment/checkout") {
        return route.fulfill({
          json: {
            uid: "deploy-checkout", resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "shop", name: "checkout", desired: 4, ready: 3, available: 3,
            containers: [{ name: "api", image: "checkout:v42", requests: { cpu: "250m", memory: "256Mi" }, limits: { cpu: "500m", memory: "512Mi" } }],
            pods: podsPayload.map((p) => ({ name: p.name, uid: p.uid, phase: "Running", node: "node-a", restart_count: 1, started_at: iso(134), created_at: iso(4320) })),
            nodes: ["node-a"], truncated: false,
          },
        });
      }
      if (pathname === "/api/admin/kubernetes/resources/apps~v1~statefulsets/ledger/describe") {
        return route.fulfill({
          json: {
            resource: {
              resource_id: "apps~v1~statefulsets", api_version: "apps/v1", kind: "StatefulSet", namespace: "shop", name: "ledger", uid: "sts-ledger", created_at: iso(4320),
              conditions: [], summary: { ready_replicas: 3, desired_replicas: 3, main_container: "api", containers: [{ name: "api", image: "ledger:v1" }] },
            },
            related_resources: [], events: [],
          },
        });
      }
      if (pathname === "/api/admin/kubernetes/workloads/StatefulSet/ledger/logs/stream") {
        return route.fulfill({
          contentType: "text/event-stream",
          body: [
            `event: sources\ndata: ${JSON.stringify({ workload_uid: "sts-ledger", pods: stsPods, truncated: false })}\n\n`,
            `event: line\ndata: ${JSON.stringify({ workload_uid: "sts-ledger", pod_uid: "sts-0", pod: "ledger-0", namespace: "shop", container: "api", incarnation: "1", source_id: "l0", text: "replaying wal segment 41", timestamp: iso(5), sequence: 1, ordinal: 1 })}\n\n`,
            `event: line\ndata: ${JSON.stringify({ workload_uid: "sts-ledger", pod_uid: "sts-1", pod: "ledger-1", namespace: "shop", container: "api", incarnation: "1", source_id: "l1", text: "commit accepted 18 ms", timestamp: iso(4), sequence: 2, ordinal: 2 })}\n\n`,
            `event: line\ndata: ${JSON.stringify({ workload_uid: "sts-ledger", pod_uid: "sts-2", pod: "ledger-2", namespace: "shop", container: "api", incarnation: "1", source_id: "l2", text: "replica caught up", timestamp: iso(3), sequence: 3, ordinal: 3 })}\n\n`,
            `event: line\ndata: ${JSON.stringify({ workload_uid: "sts-ledger", pod_uid: "sts-1", pod: "ledger-1", namespace: "shop", container: "api", incarnation: "1", source_id: "l1", text: "out of memory: cannot allocate 64 MiB", timestamp: iso(2), sequence: 4, ordinal: 4 })}\n\n`,
            `event: end\ndata: {"reason":"complete"}\n\n`,
          ].join(""),
        });
      }
      if (pathname === "/api/admin/kubernetes/workloads/StatefulSet/ledger") {
        return route.fulfill({
          json: {
            uid: "sts-ledger", resource_id: "apps~v1~statefulsets", kind: "StatefulSet", namespace: "shop", name: "ledger", desired: 3, ready: 3, available: 3,
            containers: [{ name: "api", image: "ledger:v1" }],
            pods: stsPods.map((p) => ({ name: p.name, uid: p.uid, phase: "Running", node: "node-a", restart_count: 1, started_at: iso(134), created_at: iso(4320) })),
            nodes: ["node-a"], truncated: false,
          },
        });
      }
      if (pathname === "/api/admin/kubernetes/agent-settings") {
        return route.fulfill({
          json: {
            learning: { logs: true, metrics: true, traces: false, scope: { include: ["shop"], exclude: ["sandbox"] } },
            break_logs: { linked_source: "loki-prod", fields: { loki: { namespace: "namespace", pod: "pod", container: "container" }, elasticsearch: { namespace: "kubernetes.namespace_name", pod: "kubernetes.pod_name", container: "kubernetes.container_name" }, cloudwatchlogs: { namespace: "kubernetes.namespace_name", pod: "kubernetes.pod_name", container: "kubernetes.container_name" }, graylog: { namespace: "kubernetes_namespace_name", pod: "kubernetes_pod_name", container: "kubernetes_container_name" }, splunk: { namespace: "namespace", pod: "pod", container: "container_name" }, signoz: { namespace: "k8s.namespace.name", pod: "k8s.pod.name", container: "k8s.container.name" } } },
            sources: [{ type: "loki", name: "loki-prod", scope: "{cluster=\"fixture\"}", health: "healthy" }],
            detected: { workloads: 8, namespaces: 2, metrics_server: true, traces: { kind: "tempo", namespace: "telemetry" } },
            can_write: true, linked_source_missing: false,
          },
        });
      }
      return route.fulfill({ json: { items: [], totals: {}, counts: {}, pods: [], nodes: [], edges: [], available: false, truncated: false, sync: { state: "ready", age_s: 1, partial: false } } });
    });

    const captures: Array<{ name: string; path: string; sha256: string; width: number; height: number }> = [];
    const assertCleanAndSave = async (name: string, clip?: { x: number; y: number; width: number; height: number }) => {
      await expect(page.getByRole("dialog", { name: "Session expired" })).toHaveCount(0);
      await expect(page.getByText(/Couldn.t render this page/i)).toHaveCount(0);
      await expect(page.getByText(/Loading/i)).toHaveCount(0);
      expect(pageErrors, `pageErrors before ${name}`).toEqual([]);
      expect(consoleErrors, `consoleErrors before ${name}`).toEqual([]);
      fs.mkdirSync(screenshotDir, { recursive: true });
      const file = path.join(screenshotDir, name);
      const buf = clip
        ? await page.screenshot({ path: file, clip, animations: "disabled" })
        : await page.screenshot({ path: file, animations: "disabled" });
      captures.push({
        name,
        path: file,
        sha256: createHash("sha256").update(buf).digest("hex"),
        width: buf.readUInt32BE(16),
        height: buf.readUInt32BE(20),
      });
    };

    // 1. Pod Overview & 5 Tabs at 1440x900 (and 840x900 drawer crop)
    await page.goto("/agent/kubernetes?r=core~v1~pods/shop/checkout-7d9f8c-x2k4q");
    const detail = page.getByRole("dialog", { name: "Details panel" });
    await expect(detail).toBeVisible();
    const tabs = detail.getByRole("tablist", { name: "Resource detail views" });
    await expect(detail.getByLabel("CPU metric tile", { exact: true })).toContainText("412m");
    await expect(detail.getByLabel("Memory metric tile", { exact: true })).toContainText("389 MiB");
    await expect(detail.getByLabel("Pod log output")).toContainText("GET /cart 200 18 ms");
    await assertCleanAndSave("k14-pod-overview-1440-dark-full.png");
    await assertCleanAndSave("k14-pod-overview-840-dark-drawer.png", { x: 600, y: 0, width: 840, height: 900 });

    await tabs.getByRole("tab", { name: "Logs", exact: true }).click();
    await expect(detail.getByRole("group", { name: "Container instance" })).toBeVisible();
    await expect(detail.getByRole("button", { name: "Errors 1", exact: true })).toBeVisible();
    await assertCleanAndSave("k14-pod-logs-840-dark-drawer.png", { x: 600, y: 0, width: 840, height: 900 });

    await tabs.getByRole("tab", { name: "Events", exact: true }).click();
    await expect(detail.getByRole("region", { name: "Object-scoped events", exact: true })).toContainText("BackOff");
    await assertCleanAndSave("k14-pod-events-840-dark-drawer.png", { x: 600, y: 0, width: 840, height: 900 });

    await tabs.getByRole("tab", { name: "YAML", exact: true }).click();
    await expect(detail.getByRole("region", { name: "Projected YAML", exact: true })).toContainText("apiVersion: v1");
    await assertCleanAndSave("k14-pod-yaml-840-dark-drawer.png", { x: 600, y: 0, width: 840, height: 900 });

    await tabs.getByRole("tab", { name: "Related", exact: true }).click();
    await expect(detail.getByRole("region", { name: "Related resources", exact: true })).toContainText("Scales Deployment checkout");
    await assertCleanAndSave("k14-pod-related-840-dark-drawer.png", { x: 600, y: 0, width: 840, height: 900 });

    await tabs.getByRole("tab", { name: "Timeline", exact: true }).click();
    await expect(detail.getByRole("region", { name: "Resource change history", exact: true })).toContainText("checkout:v41");
    await assertCleanAndSave("k14-pod-timeline-840-dark-drawer.png", { x: 600, y: 0, width: 840, height: 900 });

    // Light sanity on Pod Overview
    await tabs.getByRole("tab", { name: "Overview", exact: true }).click();
    await page.emulateMedia({ colorScheme: "light" });
    await page.evaluate(() => {
      localStorage.setItem("versus.theme", "light");
      document.documentElement.setAttribute("data-theme", "light");
    });
    await assertCleanAndSave("k14-pod-overview-840-light-drawer.png", { x: 600, y: 0, width: 840, height: 900 });
    await page.emulateMedia({ colorScheme: "dark" });
    await page.evaluate(() => {
      localStorage.setItem("versus.theme", "dark");
      document.documentElement.setAttribute("data-theme", "dark");
    });

    // Mobile 390x844 Pod Overview
    await page.setViewportSize({ width: 390, height: 844 });
    await expect(detail.getByRole("button", { name: "Pause logs", exact: true })).toBeVisible();
    await expect(detail.getByRole("button", { name: "Search log lines", exact: true })).toBeVisible();
    await expect(detail.getByRole("button", { name: "Expand logs", exact: true })).toBeVisible();
    await assertCleanAndSave("k14-pod-overview-390-dark-mobile.png");

    // 2. Deployment Overview & Logs at 1440x900 (840x900 drawer) and 390x844
    await page.setViewportSize({ width: 1440, height: 900 });
    await page.goto("/agent/kubernetes?r=apps~v1~deployments/shop/checkout");
    await expect(detail.getByRole("heading", { name: "Deployment shop/checkout", exact: true })).toBeVisible();
    await expect(detail.getByLabel("Workload log output")).toContainText("checkout-7d9f8c-x2k4q");
    await assertCleanAndSave("k14-deployment-overview-840-dark-drawer.png", { x: 600, y: 0, width: 840, height: 900 });

    await tabs.getByRole("tab", { name: "Logs", exact: true }).click();
    await expect(detail.getByLabel("Workload log output")).toContainText("logs not available: forbidden");
    await assertCleanAndSave("k14-deployment-logs-840-dark-drawer.png", { x: 600, y: 0, width: 840, height: 900 });

    await page.setViewportSize({ width: 390, height: 844 });
    await tabs.getByRole("tab", { name: "Overview", exact: true }).click();
    await expect(detail.getByLabel("Workload log output")).toContainText("checkout-7d9f8c-x2k4q");
    await assertCleanAndSave("k14-deployment-overview-390-dark-mobile.png");

    await tabs.getByRole("tab", { name: "Logs", exact: true }).click();
    await expect(detail.getByLabel("Workload log output")).toContainText("checkout-7d9f8c-x2k4q");
    await assertCleanAndSave("k14-deployment-logs-390-dark-mobile.png");

    await detail.getByRole("button", { name: "More", exact: true }).click();
    await expect(detail.getByRole("group", { name: "Additional log controls", exact: true })).toBeVisible();
    await assertCleanAndSave("k14-deployment-logs-390-dark-more-open.png");

    // 3. StatefulSet Previous logs at 1440x900 (840x900 drawer)
    await page.setViewportSize({ width: 1440, height: 900 });
    await page.goto("/agent/kubernetes?r=apps~v1~statefulsets/shop/ledger&tab=logs");
    await expect(detail.getByRole("heading", { name: "StatefulSet shop/ledger", exact: true })).toBeVisible();
    await detail.getByLabel("Previous logs", { exact: true }).check();
    await expect(detail.getByText("Previous logs (not followed)", { exact: true })).toBeVisible();
    await expect(detail.getByLabel("Workload log output")).toContainText("replaying wal segment 41");
    await assertCleanAndSave("k14-statefulset-logs-840-dark-previous.png", { x: 600, y: 0, width: 840, height: 900 });

    // 4. CrashJournal on Timeline tab (verifying R1 neutral summary when baseline limitations present)
    await page.goto("/agent/kubernetes?view=timeline");
    const crashJournal = page.getByRole("region", { name: "Captured crash evidence", exact: true });
    await expect(crashJournal).toBeVisible();
    await expect(crashJournal).toContainText("2 observations · Polling may miss crashes between checks.");
    await expect(crashJournal).not.toContainText("Partial history; crashes may be missing.");
    await crashJournal.scrollIntoViewIfNeeded();
    await assertCleanAndSave("k14-crash-journal-1440-dark-timeline.png");

    // 5. Agent tab (verifying log capture without byte/line limits + Agent learning at bottom with disabled switches and disabled Edit scope)
    await page.goto("/agent/kubernetes?view=agent");
    const agentRegion = page.getByRole("region", { name: "Kubernetes Agent", exact: true });
    await expect(agentRegion).toBeVisible();
    const learningRegion = agentRegion.getByRole("region", { name: "Agent learning", exact: true });
    await expect(learningRegion).toBeVisible();
    await expect(learningRegion.getByText("Not available yet.", { exact: true })).toBeVisible();
    await expect(learningRegion.getByRole("switch", { name: "Logs learning", exact: true })).toBeDisabled();
    await expect(learningRegion.getByRole("switch", { name: "Metrics learning", exact: true })).toBeDisabled();
    await expect(learningRegion.getByRole("switch", { name: "Traces learning", exact: true })).toBeDisabled();
    await expect(learningRegion.getByRole("button", { name: "Edit scope", exact: true })).toBeDisabled();
    await learningRegion.scrollIntoViewIfNeeded();
    await assertCleanAndSave("k14-agent-tab-1440-dark-settings.png");

    if (process.env.HARNESS_RESULTS_DIR) {
      fs.writeFileSync(
        path.join(process.env.HARNESS_RESULTS_DIR, "visual-delta-captures.json"),
        JSON.stringify(captures, null, 2),
      );
    }
    await testInfo.attach("k14-svg-captures", { body: JSON.stringify(captures, null, 2), contentType: "application/json" });
  });

  test("live stack Deployment and StatefulSet All pods streaming, tabs, and V1-V8 regressions desktop and mobile", async ({ page, context }, testInfo) => {
    test.skip(process.env.E2E_KUBERNETES_BACKEND !== "fake", "requires the live fakekube backend");
    test.setTimeout(300_000);
    await context.grantPermissions(["clipboard-read", "clipboard-write"]);
    const errors: string[] = [];
    page.on("pageerror", (error) => errors.push(error.message));

    const state = harnessRuntimeStateDir();
    const fakePort = Number(process.env.HARNESS_FAKEKUBE_PORT || 19443);
    const ca = fs.readFileSync(path.join(state, "fakekube/ca.crt"));

    const upstream = (endpoint: string, body?: unknown) =>
      new Promise<unknown>((resolve, reject) => {
        const encoded = body === undefined ? undefined : JSON.stringify(body);
        const call = https.request(
          `https://localhost:${fakePort}${endpoint}`,
          {
            ca,
            method: encoded ? "POST" : "GET",
            headers: encoded ? { "Content-Type": "application/json" } : {},
          },
          (response) => {
            let text = "";
            response.on("data", (chunk) => {
              text += chunk;
            });
            response.on("end", () =>
              response.statusCode === 200
                ? resolve(text ? JSON.parse(text) : {})
                : reject(new Error(`fakekube ${endpoint} HTTP ${response.statusCode} ${text}`)),
            );
          },
        );
        call.on("error", reject);
        call.end(encoded);
      });

    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.emulateMedia({ colorScheme: "dark" });
    await page.addInitScript(() => localStorage.setItem("versus.theme", "dark"));

    // 1. Verify default triage Deployment payments/checkout-api streams live logs via repaired ReplicaSet -> Pod chain
    await openApp(page, "/agent/kubernetes?r=apps~v1~deployments/payments/checkout-api");
    const detail = page.getByRole("dialog", { name: "Details panel" });
    await expect(detail).toBeVisible();
    await expect(detail.getByRole("heading", { name: "Deployment payments/checkout-api", exact: true })).toBeVisible();
    await expect(detail).not.toContainText("739910d");
    await expect(detail).not.toContainText("1/1/1");
    await expect(detail.getByLabel("CPU metric tile", { exact: true }).locator("p.text-ink-400").first()).toHaveText("Unavailable");
    await expect(detail.getByLabel("Memory metric tile", { exact: true }).locator("p.text-ink-400").first()).toHaveText("Unavailable");

    const triageWlLogs = detail.getByLabel("Workload log output");
    await expect(triageWlLogs).toContainText("checkout-api-0", { timeout: 30_000 });
    await expect(triageWlLogs).toContainText("fakekube follow line");
    await expect(triageWlLogs).toContainText("REDACTED");
    await expect(triageWlLogs).not.toContainText("super-secret-value");

    // 2. Seed shop namespace with multi-ReplicaSet Deployment checkout (4 owned Pods + 1 unowned same-label Pod) and StatefulSet ledger (3 ordinals)
    const nowMs = Math.floor(Date.now() / 1000) * 1000;
    const iso = (ms: number) => new Date(ms).toISOString().replace(".000Z", "Z");
    const startedISO = iso(nowMs - 8040_000);
    const createdISO = iso(nowMs - 86400_000 * 3);

    await upstream("/_fake/mutate", {
      operation: "upsert",
      resource: "namespaces",
      namespace: "",
      name: "shop",
      object: { kind: "Namespace", metadata: { name: "shop" } },
    });
    await upstream("/_fake/mutate", {
      operation: "upsert",
      resource: "deployments",
      namespace: "shop",
      name: "checkout",
      object: {
        apiVersion: "apps/v1",
        kind: "Deployment",
        metadata: { name: "checkout", namespace: "shop", uid: "live-deploy-checkout", creationTimestamp: createdISO, generation: 2, labels: { app: "checkout" } },
        spec: {
          replicas: 4,
          selector: { matchLabels: { app: "checkout" } },
          template: {
            metadata: { labels: { app: "checkout" } },
            spec: {
              containers: [
                { name: "api", image: "checkout:v42", resources: { requests: { cpu: "250m", memory: "256Mi" }, limits: { cpu: "500m", memory: "512Mi" } } },
                { name: "sidecar", image: "istio-proxy:v1" },
              ],
              initContainers: [{ name: "setup", image: "setup:v1" }],
            },
          },
        },
        status: {
          observedGeneration: 2,
          replicas: 4,
          readyReplicas: 3,
          availableReplicas: 3,
          updatedReplicas: 2,
          conditions: [{ type: "Available", status: "True", reason: "MinimumReplicasAvailable", lastTransitionTime: startedISO }],
        },
      },
    });
    for (const rs of [
      { name: "checkout-7d9f8c", uid: "live-rs-old", replicas: 2 },
      { name: "checkout-8b6c9d", uid: "live-rs-new", replicas: 2 },
    ]) {
      await upstream("/_fake/mutate", {
        operation: "upsert",
        resource: "replicasets",
        namespace: "shop",
        name: rs.name,
        object: {
          apiVersion: "apps/v1",
          kind: "ReplicaSet",
          metadata: {
            name: rs.name,
            namespace: "shop",
            uid: rs.uid,
            creationTimestamp: createdISO,
            labels: { app: "checkout" },
            ownerReferences: [{ apiVersion: "apps/v1", kind: "Deployment", name: "checkout", uid: "live-deploy-checkout", controller: true }],
          },
          spec: { replicas: rs.replicas, selector: { matchLabels: { app: "checkout" } } },
          status: { replicas: rs.replicas, readyReplicas: rs.replicas },
        },
      });
    }

    const makeShopPod = (name: string, uid: string, ownerKind: "ReplicaSet" | "StatefulSet", ownerName: string, ownerUID: string, appLabel: string, withOwner = true) => ({
      apiVersion: "v1",
      kind: "Pod",
      metadata: {
        name,
        namespace: "shop",
        uid,
        creationTimestamp: createdISO,
        labels: { app: appLabel },
        ...(withOwner
          ? { ownerReferences: [{ apiVersion: "apps/v1", kind: ownerKind, name: ownerName, uid: ownerUID, controller: true }] }
          : {}),
      },
      spec: {
        nodeName: "node-a",
        containers: [
          { name: "api", image: "checkout:v42", resources: { requests: { cpu: "250m", memory: "256Mi" }, limits: { cpu: "500m", memory: "512Mi" } } },
          { name: "sidecar", image: "istio-proxy:v1" },
        ],
        initContainers: [{ name: "setup", image: "setup:v1" }],
      },
      status: {
        phase: "Running",
        podIP: "10.42.1.17",
        qosClass: "Burstable",
        conditions: [{ type: "Ready", status: "True", reason: "ContainersReady", lastTransitionTime: startedISO }],
        containerStatuses: [
          { name: "api", ready: true, restartCount: 2, state: { running: { startedAt: startedISO } }, lastState: { terminated: { reason: "OOMKilled", exitCode: 137, finishedAt: iso(nowMs - 600_000) } } },
          { name: "sidecar", ready: true, restartCount: 1, state: { running: { startedAt: startedISO } } },
        ],
        initContainerStatuses: [{ name: "setup", ready: true, restartCount: 0, state: { terminated: { exitCode: 0, reason: "Completed" } } }],
      },
    });

    const checkoutPods = [
      { name: "checkout-7d9f8c-x2k4q", uid: "live-pod-x2k4q", rsName: "checkout-7d9f8c", rsUID: "live-rs-old" },
      { name: "checkout-7d9f8c-p9m1z", uid: "live-pod-p9m1z", rsName: "checkout-7d9f8c", rsUID: "live-rs-old" },
      { name: "checkout-8b6c9d-a6v2n", uid: "live-pod-a6v2n", rsName: "checkout-8b6c9d", rsUID: "live-rs-new" },
      { name: "checkout-8b6c9d-b8r3s", uid: "live-pod-b8r3s", rsName: "checkout-8b6c9d", rsUID: "live-rs-new" },
    ];
    for (const p of checkoutPods) {
      await upstream("/_fake/mutate", {
        operation: "upsert",
        resource: "pods",
        namespace: "shop",
        name: p.name,
        object: makeShopPod(p.name, p.uid, "ReplicaSet", p.rsName, p.rsUID, "checkout", true),
      });
    }
    await upstream("/_fake/mutate", {
      operation: "delete",
      resource: "pods",
      namespace: "shop",
      name: "checkout-8b6c9d-c9z4k",
    }).catch(() => {});
    // Unowned same-label Pod in shop namespace that must NEVER be admitted to Deployment checkout stream
    await upstream("/_fake/mutate", {
      operation: "upsert",
      resource: "pods",
      namespace: "shop",
      name: "checkout-unowned-same-label",
      object: makeShopPod("checkout-unowned-same-label", "live-pod-unowned", "ReplicaSet", "other-rs", "other-rs-uid", "checkout", false),
    });
    // Configure checkout-7d9f8c-p9m1z api container to return HTTP 403 Forbidden on finite Previous logs read
    await upstream("/_fake/log-fixtures", {
      path: "/api/v1/namespaces/shop/pods/checkout-7d9f8c-p9m1z/log",
      container: "api",
      previous: true,
      status: 403,
    });

    // Seed StatefulSet shop/ledger with 3 ordinals ledger-0, ledger-1, ledger-2
    await upstream("/_fake/mutate", {
      operation: "upsert",
      resource: "statefulsets",
      namespace: "shop",
      name: "ledger",
      object: {
        apiVersion: "apps/v1",
        kind: "StatefulSet",
        metadata: { name: "ledger", namespace: "shop", uid: "live-sts-ledger", creationTimestamp: createdISO, generation: 1, labels: { app: "ledger" } },
        spec: {
          serviceName: "ledger",
          replicas: 3,
          selector: { matchLabels: { app: "ledger" } },
          template: { metadata: { labels: { app: "ledger" } }, spec: { containers: [{ name: "api", image: "ledger:v1" }] } },
        },
        status: { observedGeneration: 1, replicas: 3, readyReplicas: 3, currentReplicas: 3, updatedReplicas: 3 },
      },
    });
    for (let ord = 0; ord < 3; ord++) {
      await upstream("/_fake/mutate", {
        operation: "upsert",
        resource: "pods",
        namespace: "shop",
        name: `ledger-${ord}`,
        object: makeShopPod(`ledger-${ord}`, `live-pod-ledger-${ord}`, "StatefulSet", "ledger", "live-sts-ledger", "ledger", true),
      });
    }

    // Refresh index and wait until Deployment shop/checkout and StatefulSet shop/ledger are indexed
    await detail.getByRole("button", { name: "Close panel" }).click();
    await expect.poll(async () => {
      await page.getByRole("button", { name: "Refresh Kubernetes data", exact: true }).click();
      const res = await page.request.get("/api/admin/kubernetes/workloads?namespace=shop&limit=50");
      if (res.status() !== 200) return false;
      const body = await res.json();
      const names = (body.items ?? []).map((item: { kind: string; name: string }) => `${item.kind}/${item.name}`);
      return names.includes("Deployment/checkout") && names.includes("StatefulSet/ledger") && names.includes("Pod/checkout-8b6c9d-b8r3s");
    }, { timeout: 45_000, intervals: [1000, 2000] }).toBe(true);

    // 3. Open Deployment shop/checkout on Overview and verify shared Overview/Logs workload stream
    const checkoutStreamRequests: URL[] = [];
    page.on("request", (req) => {
      const url = new URL(req.url());
      if (url.pathname === "/api/admin/kubernetes/workloads/Deployment/checkout/logs/stream") {
        checkoutStreamRequests.push(url);
      }
    });

    await page.goto("/agent/kubernetes?r=apps~v1~deployments/shop/checkout");
    await expect(detail.getByRole("heading", { name: "Deployment shop/checkout", exact: true })).toBeVisible();
    await expect(detail).toContainText("Ready 3/4");
    await expect(detail).not.toContainText("Partial coverage");
    await expect(detail).not.toContainText(/Streaming \d+ of \d+/);
    await expect(detail.getByRole("alert")).toHaveCount(0);

    const wlOutput = detail.getByLabel("Workload log output");
    for (const podName of ["checkout-7d9f8c-x2k4q", "checkout-7d9f8c-p9m1z", "checkout-8b6c9d-a6v2n", "checkout-8b6c9d-b8r3s"]) {
      await expect(wlOutput).toContainText(podName, { timeout: 30_000 });
    }
    await expect(wlOutput).not.toContainText("checkout-unowned-same-label");
    expect(checkoutStreamRequests).toHaveLength(1);
    await captureBreakState(page, "k14-live-deployment-overview-desktop-dark.png", "dark", "L: live OSS + fakekube Deployment Overview compact All pods stream");

    // Mobile 390x844 Deployment Overview compact card
    await page.setViewportSize({ width: 390, height: 844 });
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
    await captureBreakState(page, "k14-live-deployment-overview-mobile-dark.png", "dark", "L: live OSS + fakekube Deployment Overview mobile 390x844");
    await page.setViewportSize({ width: 1440, height: 1000 });

    // Switch to Logs tab via "Open full logs →" and verify NO second stream request is opened!
    await detail.getByRole("button", { name: "Open full logs", exact: true }).click();
    const tabs = detail.getByRole("tablist", { name: "Resource detail views" });
    await expect(tabs.getByRole("tab", { name: "Logs", exact: true })).toHaveAttribute("aria-selected", "true");
    expect(checkoutStreamRequests).toHaveLength(1);

    // Switch to Events and back to Logs -> still the same single stream session!
    await tabs.getByRole("tab", { name: "Events", exact: true }).click();
    await tabs.getByRole("tab", { name: "Logs", exact: true }).click();
    expect(checkoutStreamRequests).toHaveLength(1);

    const podSelect = detail.getByLabel("Log pod", { exact: true });
    await expect(podSelect.locator("option")).toHaveText([
      "All pods",
      "checkout-7d9f8c-p9m1z",
      "checkout-7d9f8c-x2k4q",
      "checkout-8b6c9d-a6v2n",
      "checkout-8b6c9d-b8r3s",
    ]);
    await captureBreakState(page, "k14-live-deployment-logs-all-pods-desktop-dark.png", "dark", "L: live OSS + fakekube Deployment Logs All pods across old and new ReplicaSets");

    // Mobile 390x844 Deployment Logs with More menu collapsed and expanded
    await page.setViewportSize({ width: 390, height: 844 });
    const moreBtn = detail.getByRole("button", { name: "More", exact: true });
    await expect(moreBtn).toBeVisible();
    await expect(moreBtn).toHaveAttribute("aria-expanded", "false");
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
    await captureBreakState(page, "k14-live-deployment-logs-mobile-dark.png", "dark", "L: live OSS + fakekube Deployment Logs mobile 390x844");
    await moreBtn.click();
    await expect(moreBtn).toHaveAttribute("aria-expanded", "true");
    await expect(detail.getByRole("group", { name: "Additional log controls", exact: true })).toBeVisible();
    await captureBreakState(page, "k14-live-deployment-logs-more-mobile-dark.png", "dark", "L: live OSS + fakekube Deployment Logs mobile More menu expanded");
    await moreBtn.click();
    await page.setViewportSize({ width: 1440, height: 1000 });

    // 4. Click a Pod-name prefix in a log line to narrow the stream to that Pod, verify upstream cancellation of other Pods, and verify "All pods" is not mislabeled
    await wlOutput.getByRole("button", { name: "checkout-8b6c9d-a6v2n" }).first().click();
    await expect(podSelect).toHaveValue("live-pod-a6v2n");
    await expect(podSelect.locator("option").first()).toHaveText("All pods");
    await expect(podSelect.locator("option")).toHaveText([
      "All pods",
      "checkout-7d9f8c-p9m1z",
      "checkout-7d9f8c-x2k4q",
      "checkout-8b6c9d-a6v2n",
      "checkout-8b6c9d-b8r3s",
    ]);
    await expect.poll(async () => {
      const counters = await streamCounters();
      const a6Active = counters.find((c) => c.path === "/api/v1/namespaces/shop/pods/checkout-8b6c9d-a6v2n/log" && c.container === "api" && c.follow)?.active ?? 0;
      const x2Active = counters.find((c) => c.path === "/api/v1/namespaces/shop/pods/checkout-7d9f8c-x2k4q/log" && c.container === "api" && c.follow)?.active ?? 0;
      return a6Active === 1 && x2Active === 0;
    }, { timeout: 15_000 }).toBe(true);

    // Switch back to All pods
    await podSelect.selectOption("");
    await expect.poll(async () => {
      const counters = await streamCounters();
      const a6Active = counters.find((c) => c.path === "/api/v1/namespaces/shop/pods/checkout-8b6c9d-a6v2n/log" && c.container === "api" && c.follow)?.active ?? 0;
      const x2Active = counters.find((c) => c.path === "/api/v1/namespaces/shop/pods/checkout-7d9f8c-x2k4q/log" && c.container === "api" && c.follow)?.active ?? 0;
      return a6Active === 1 && x2Active === 1;
    }, { timeout: 15_000 }).toBe(true);

    // 5. Container picker: All containers ("*") is first and renders <pod>/<container> prefixes
    const containerSelect = detail.getByLabel("Log container", { exact: true });
    await expect(containerSelect.locator("option")).toHaveText(["All containers", "Main container", "api", "sidecar", "init:setup"]);
    await containerSelect.selectOption("*");
    await expect(wlOutput).toContainText("checkout-7d9f8c-x2k4q/api", { timeout: 20_000 });
    await expect(wlOutput).toContainText("checkout-7d9f8c-x2k4q/sidecar", { timeout: 20_000 });
    await containerSelect.selectOption("");
    await expect(wlOutput).toContainText("checkout-7d9f8c-x2k4q");

    // 6. Scroll position vs network pause: scrolling away from the newest line
    // stops the follow while lines keep arriving over the network, then Pause
    // logs cancels upstream
    await wlOutput.evaluate((el) => {
      el.scrollTop = 0;
      el.dispatchEvent(new Event("scroll"));
    });
    const countBeforeFollowWait = await wlOutput.locator("[data-log-key]").count();
    await expect.poll(() => wlOutput.locator("[data-log-key]").count(), { timeout: 15_000 }).toBeGreaterThan(countBeforeFollowWait + 2);
    expect(await wlOutput.evaluate((el) => el.scrollTop)).toBeLessThanOrEqual(1);
    await detail.getByRole("button", { name: "Jump to latest", exact: true }).click();
    // At the newest line the control is gone: following is the state, not a toggle.
    await expect(detail.getByRole("button", { name: "Jump to latest", exact: true })).toHaveCount(0);
    await expect.poll(() => wlOutput.locator("[data-log-key]").count(), { timeout: 25_000 }).toBeGreaterThanOrEqual(4);
    expect(await wlOutput.locator("[data-log-key]").count()).toBeLessThan(32);

    await detail.getByLabel("Pause logs").click();
    await expect.poll(async () => {
      const counters = await streamCounters();
      return counters.filter((c) => c.path.startsWith("/api/v1/namespaces/shop/pods/checkout-")).reduce((sum, c) => sum + c.active, 0);
    }, { timeout: 15_000 }).toBe(0);
    await detail.getByLabel("Resume logs").click();
    await expect.poll(() => checkoutStreamRequests.at(-1)?.searchParams.has("cursor")).toBe(true);

    // Check Previous logs on Deployment checkout: checkout-7d9f8c-p9m1z returns 403 Forbidden (one dim inline line, no spurious reconnect divider spam) while the other 3 Pods stream previous lines!
    await detail.getByLabel("Previous logs").check();
    await expect(wlOutput).toContainText(/logs not available: (?:forbidden|Kubernetes denied access)/i, { timeout: 15_000 });
    await expect(wlOutput).toContainText("checkout-7d9f8c-x2k4q");
    await expect(wlOutput).toContainText("fakekube previous");
    await page.waitForTimeout(6200);
    await expect(wlOutput.locator("[data-log-key]").filter({ hasText: "logs not available:" })).toHaveCount(1);
    await expect(wlOutput).not.toContainText("reconnected at");
    await captureBreakState(page, "k14-live-deployment-previous-forbidden-inline-desktop-dark.png", "dark", "L: live OSS + fakekube Deployment Previous logs with one forbidden Pod inline row");
    await detail.getByLabel("Previous logs").uncheck();
    await expect(wlOutput).toContainText("fakekube follow line", { timeout: 20_000 });

    // 7. Dynamic rollout join & deletion on live stream
    await upstream("/_fake/mutate", {
      operation: "upsert",
      resource: "pods",
      namespace: "shop",
      name: "checkout-8b6c9d-c9z4k",
      object: makeShopPod("checkout-8b6c9d-c9z4k", "live-pod-c9z4k", "ReplicaSet", "checkout-8b6c9d", "live-rs-new", "checkout", true),
    });
    expect((await page.request.get("/api/admin/kubernetes/workloads?namespace=shop&limit=50")).status()).toBe(200);
    await expect(wlOutput).toContainText("checkout-8b6c9d-c9z4k", { timeout: 30_000 });
    await upstream("/_fake/mutate", {
      operation: "delete",
      resource: "pods",
      namespace: "shop",
      name: "checkout-8b6c9d-c9z4k",
    });
    expect((await page.request.get("/api/admin/kubernetes/workloads?namespace=shop&limit=50")).status()).toBe(200);
    await expect.poll(async () => {
      const counters = await streamCounters();
      return counters.find((c) => c.path === "/api/v1/namespaces/shop/pods/checkout-8b6c9d-c9z4k/log" && c.follow)?.active ?? 0;
    }, { timeout: 20_000 }).toBe(0);
    // Retained lines from deleted Pod remain visible in buffer
    await expect(wlOutput).toContainText("checkout-8b6c9d-c9z4k");

    // 8. StatefulSet shop/ledger live logs and Previous logs finite read
    await page.goto("/agent/kubernetes?r=apps~v1~statefulsets/shop/ledger&tab=logs");
    await expect(detail.getByRole("heading", { name: "StatefulSet shop/ledger", exact: true })).toBeVisible();
    for (const ord of ["ledger-0", "ledger-1", "ledger-2"]) {
      await expect(wlOutput).toContainText(ord, { timeout: 30_000 });
    }
    await detail.getByLabel("Previous logs").check();
    // Previous logs is one bounded read: it never follows, and the status pill
    // says which read is on screen.
    await expect(detail.getByLabel("Log connection status")).toHaveText("Previous");
    await expect(wlOutput).toContainText("fakekube previous");
    await expect(wlOutput).toContainText("REDACTED");
    await expect(wlOutput).not.toContainText("previous-secret-value");
    await expect.poll(async () => {
      const counters = await streamCounters();
      return counters.filter((c) => c.path.startsWith("/api/v1/namespaces/shop/pods/ledger-")).reduce((sum, c) => sum + c.active, 0);
    }, { timeout: 15_000 }).toBe(0);
    await captureBreakState(page, "k14-live-statefulset-previous-logs-desktop-dark.png", "dark", "L: live OSS + fakekube StatefulSet Previous logs finite read");

    // Light theme visual parity check on StatefulSet drawer
    await page.emulateMedia({ colorScheme: "light" });
    await page.evaluate(() => {
      localStorage.setItem("versus.theme", "light");
      document.documentElement.setAttribute("data-theme", "light");
    });
    await captureBreakState(page, "k14-live-statefulset-previous-logs-desktop-light.png", "light", "L: live OSS + fakekube StatefulSet Previous logs light theme");
    await page.emulateMedia({ colorScheme: "dark" });
    await page.evaluate(() => {
      localStorage.setItem("versus.theme", "dark");
      document.documentElement.setAttribute("data-theme", "dark");
    });

    // Uncheck Previous logs -> resumes live follow, then test StatefulSet ordinal UID replacement (stale index before refresh vs fresh identity after refresh)
    await detail.getByLabel("Previous logs").uncheck();
    await expect(wlOutput).toContainText("fakekube follow line", { timeout: 20_000 });

    // Close drawer so active streams stop, then replace ledger-2 in fakekube with a new UID BEFORE refreshing the shared index
    await detail.getByRole("button", { name: "Close panel" }).click();
    await expect(detail).toHaveCount(0);
    await expect.poll(async () => {
      const counters = await streamCounters();
      return counters.reduce((sum, c) => sum + c.active, 0);
    }, { timeout: 15_000 }).toBe(0);

    const ledger2BeforeReplace = (await streamCounters()).find((c) => c.path === "/api/v1/namespaces/shop/pods/ledger-2/log" && c.container === "api" && c.follow)?.requests ?? 0;
    await upstream("/_fake/mutate", {
      operation: "upsert",
      resource: "pods",
      namespace: "shop",
      name: "ledger-2",
      object: makeShopPod("ledger-2", "live-pod-ledger-2-replaced", "StatefulSet", "ledger", "live-sts-ledger", "ledger", true),
    });

    // Open fresh StatefulSet stream while shared index still has old UID "live-pod-ledger-2": D3 live Pod GET rejects stale UID without opening a log read on ledger-2!
    await page.goto("/agent/kubernetes?r=apps~v1~statefulsets/shop/ledger&tab=logs");
    await expect(detail.getByRole("heading", { name: "StatefulSet shop/ledger", exact: true })).toBeVisible();
    await expect(wlOutput).toContainText("ledger-0", { timeout: 20_000 });
    await expect(wlOutput).toContainText("ledger-1", { timeout: 20_000 });
    const ledger2DuringStaleIndex = (await streamCounters()).find((c) => c.path === "/api/v1/namespaces/shop/pods/ledger-2/log" && c.container === "api" && c.follow)?.requests ?? 0;
    expect(ledger2DuringStaleIndex).toBe(ledger2BeforeReplace);

    // Refresh shared index so new UID "live-pod-ledger-2-replaced" is discovered -> 5s reconcile admits ledger-2 with its new identity!
    expect((await page.request.get("/api/admin/kubernetes/workloads?namespace=shop&limit=50")).status()).toBe(200);
    await expect(wlOutput).toContainText("ledger-2", { timeout: 25_000 });
    await expect(detail.getByLabel("Log pod", { exact: true }).locator('option[value="live-pod-ledger-2-replaced"]')).toHaveText("ledger-2");

    await detail.getByRole("button", { name: "Close panel" }).click();
    await expect(detail).toHaveCount(0);
    await expect.poll(async () => {
      const counters = await streamCounters();
      return counters.reduce((sum, c) => sum + c.active, 0);
    }, { timeout: 15_000 }).toBe(0);
    expect(errors).toEqual([]);
    await testInfo.attach("k14-live-upstream-counters", { body: JSON.stringify(await streamCounters(), null, 2), contentType: "application/json" });
  });
});

for (const width of [1440, 390]) for (const theme of ["dark", "light"] as const) {
  test(`published snapshots stay authorized through warming, recollection and later credential failure at ${width} ${theme}`, async ({ page }, testInfo) => {
    test.skip(process.env.E2E_UI_FIXTURES !== "1", "credential-free intercepted UI fixture only");
    await page.setViewportSize({ width, height: width === 390 ? 844 : 900 });
    await page.emulateMedia({ colorScheme: theme });
    await page.addInitScript((value) => localStorage.setItem("versus.theme", value), theme);
    const observed = "2026-10-28T00:00:00Z";
    const recollected = "2026-10-28T00:14:00Z";
    await page.clock.install({ time: new Date(observed) });
    let phase: "published" | "warming" | "recollected" | "fault" | "empty" | "denied" = "published";
    const replies: Array<{ phase: string; path: string; query: string; payload: unknown }> = [];
    let logAttempts = 0;
    const resource = { resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "shop", name: "checkout", uid: "checkout", summary: { ready: 2, desired: 3 } };
    const sync = () => ({ state: phase === "warming" ? "syncing" : "ready", age_s: phase === "warming" ? 780 : 0, partial: false });
    const issue = { root: { kind: "Deployment", namespace: "shop", name: "checkout", uid: "checkout" }, rule: "pod.crash_loop", count: 3, severity: "critical", broken: true, broke_at: observed };
    await page.route("**/api/**", async (route) => {
      const url = new URL(route.request().url());
      const path = url.pathname;
      const answer = (payload: unknown, status = 200) => {
        replies.push({ phase, path, query: url.search, payload });
        return route.fulfill({ json: payload, status });
      };
      if (phase === "denied" && path.startsWith("/api/admin/kubernetes/")) return answer({ code: "forbidden", error: "infrastructure:view permission is required" }, 403);
      if (path === "/api/admin/config/agent") return answer({ enable: false, mode: "detect", ai: { enable: false }, sources: [] });
      if (path.includes("/deployment") || path.includes("/sso/")) return answer({ error: "Community" }, 403);
      if (path === "/api/admin/kubernetes/clusters") return answer({ multiple: false, clusters: [] });
      if (path === "/api/admin/kubernetes/sync") return phase === "fault"
        ? answer({ code: "credential_unavailable", error: "Cluster credentials unavailable", first_ready_at: observed }, 502)
        : answer({ ...sync(), first_ready_at: observed, kinds: {}, total_kinds: 1, ready_kinds: phase === "warming" ? 0 : 1, failing_kinds: 0, pending_kinds: phase === "warming" ? 1 : 0, truncated: false });
      if (path === "/api/admin/kubernetes/overview") return answer({ connector: "kubernetes", cluster_id: "fixture", observed_at: phase === "warming" || phase === "fault" ? new Date().toISOString() : phase === "recollected" ? recollected : observed, nodes: phase === "warming" || phase === "empty" ? 0 : 2, ready_nodes: 2, pods: 3, running_pods: 0, namespaces: 1, active_namespaces: 1, workloads: 1, warnings: 1, ...(phase === "warming" ? {} : { allocatable_cpu: "8", allocatable_memory: "16Gi", requested_cpu: "2", requested_memory: "4Gi" }), truncated: false, sync: sync() });
      if (path === "/api/admin/kubernetes/issues") return answer({ items: phase === "warming" || phase === "empty" ? [] : [issue], broken: { total: phase === "warming" || phase === "empty" ? 0 : 3, by_severity: { critical: phase === "empty" ? 0 : 3 } }, totals: { critical: phase === "empty" ? 0 : 3 }, truncated: false, sync: sync() });
      if (path === "/api/admin/kubernetes/stream") return route.fulfill({ contentType: "text/event-stream", body: `event: heartbeat\ndata: {}\n\n` });
      if (path === "/api/admin/kubernetes/namespaces") return answer({ items: ["shop"], sync: sync() });
      if (path === "/api/admin/kubernetes/graph/overview") return answer({ nodes: [], edges: [], omitted: {}, sync: sync() });
      if (path === "/api/admin/kubernetes/usage") {
        const available = phase !== "warming";
        const metrics = { availability: available ? "available" : "unavailable", fresh: available, complete: available, total: available ? 2 : 0, ...(available ? { cpu: "1", memory: "2Gi", observed_at: observed } : {}) };
        return answer({ observed_at: available ? observed : new Date().toISOString(), availability: metrics.availability, fresh: available, pod_metrics: metrics, node_metrics: metrics, pods: [], nodes: [], truncated: false });
      }
      if (path === "/api/admin/kubernetes/issue-alerts") return answer({ enabled: false, level: "critical", available: false, can_write: false, reason: "read_only" });
      if (path.endsWith("/checkout/describe")) return answer({ resource, events: [], related_resources: [], sync: sync() });
      if (path === "/api/admin/kubernetes/workloads/Deployment/checkout") return answer({ ...resource, ready: 2, desired: 3, pods: [], containers: [{ name: "app", image: "checkout:demo" }], conditions: [], truncated: false, sync: sync() });
      if (path === "/api/admin/kubernetes/graph/related") return answer({ nodes: [{ id: "checkout", ...resource, group: "workload", health: "degraded" }], edges: [], omitted: {}, sync: sync() });
      if (path === "/api/admin/kubernetes/timeline") {
        if (!url.searchParams.has("resource_uid")) {
          const cursor = url.searchParams.get("cursor");
          const last = cursor === "long+/last";
          const change = { id: last ? "later-change" : "first-change", cluster: "fixture", kind: "Deployment", namespace: "shop", name: "checkout", uid: "checkout", type: last ? "replicas_changed" : "image_changed", fields: [], at: observed };
          return answer({ items: cursor === "long+/empty" ? [] : [{ type: "change", id: change.id, at: observed, change }], ...(last ? {} : { next: cursor ? "long+/last" : "long+/empty" }), total: 90000, crash_count: 0, counts: { change: 90000, crash: 0, break: 0 }, gaps: [], sync: sync() });
        }
        if (!url.searchParams.has("cursor")) return answer({ items: [], next: "retained+/position", total: 90000, crash_count: 0, counts: { change: 90000, crash: 0, break: 0 }, gaps: [{ from: "2026-10-27T23:00:00Z", to: observed }], sync: sync() });
        return answer({ items: [{ type: "change", id: "captured-change", at: observed, change: { id: "captured-change", cluster: "fixture", kind: "Deployment", namespace: "shop", name: "checkout", uid: "checkout", type: "image_changed", fields: [{ path: "image", from: "old", to: "demo" }], at: observed } }], total: 90000, crash_count: 0, counts: { change: 90000, crash: 0, break: 0 }, gaps: [], sync: sync() });
      }
      if (path.endsWith("/logs/stream")) {
        logAttempts++;
        if (phase === "fault") return answer({ code: "credential_unavailable", error: "Cluster credentials unavailable", retryable: false }, 502);
        if (logAttempts === 1) return answer({ code: "credential_unavailable", error: "Renewal pending", retryable: true }, 502);
        const line = { text: "authorized fixture output", container: "app", pod: "checkout-0", pod_uid: "pod-checkout", source_id: "checkout-app", timestamp: observed, ordinal: 1 };
        return route.fulfill({ contentType: "text/event-stream", body: `event: line\ndata: ${JSON.stringify(line)}\n\nevent: end\ndata: {}\n\n` });
      }
      if (path.startsWith("/api/admin/kubernetes/")) return answer({ items: [], nodes: [], edges: [], omitted: {}, truncated: false, available: false, gaps: [], sync: sync() });
      return answer({ items: [], incidents: [], total: 0, by_origin: {}, enable: false });
    });
    const capture = async (surface: string, state: string) => {
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
      expect(await page.locator("html").getAttribute("data-theme")).toBe(theme === "light" ? "light" : null);
      if (captureUnchanged) await page.screenshot({ path: capturePath(surface, width, theme, state), fullPage: true, animations: "disabled" });
    };
    await page.goto("/agent/kubernetes");
    const health = page.getByRole("region", { name: "Cluster health", exact: true });
    await expect(health.getByRole("button", { name: "3 broken", exact: true })).toBeVisible();
    const snapshot = await health.getByText(/^Snapshot \d\d:/).innerText();
    const tabLabels = await page.getByRole("tablist", { name: "Kubernetes views" }).getByRole("tab").allTextContents();
    const cpu = page.getByRole("heading", { name: "CPU capacity", exact: true }).locator("..").locator("..");
    const memory = page.getByRole("heading", { name: "Memory capacity", exact: true }).locator("..").locator("..");
    await expect(cpu).toContainText("8");
    await expect(cpu).toContainText("1 core");
    await expect(memory).toContainText("2 GiB");
    await capture("published-continuity", "published");
    await page.getByRole("button", { name: "Open resource checkout", exact: true }).click();
    const drawer = page.getByRole("dialog", { name: "Details panel" });
    const drawerTab = async (label: string) => {
      const tab = drawer.getByRole("tab", { name: label, exact: true });
      if (width >= 640 || ["Overview", "Logs", "Events"].includes(label)) await tab.click();
      else {
        await drawer.getByLabel("More resource tabs", { exact: true }).click();
        await drawer.getByRole("menuitem", { name: label, exact: true }).click();
      }
      if (width >= 640 || ["Overview", "Logs", "Events"].includes(label)) await expect(tab).toHaveAttribute("aria-selected", "true");
      await expect(drawer.locator(".resource-tabpanel")).toHaveAttribute("data-view", label.toLowerCase());
    };
    await expect(drawer).toBeVisible();
    await expect(drawer.getByText("Ready 2/3", { exact: true })).toBeVisible();
    await capture("resource-detail-retained", "identity");
    await page.clock.runFor(1100);
    await drawerTab("Logs");
    await expect(drawer.getByLabel("Workload log output")).toContainText("authorized fixture output");
    expect(logAttempts).toBe(2);
    await capture("resource-detail-retained", "logs");
    await drawerTab("YAML");
    await expect(drawer.getByRole("region", { name: "Projected YAML" })).toContainText("Projected, read-only");
    await capture("resource-detail-retained", "yaml");
    await drawerTab("Events");
    await expect(drawer.getByRole("region", { name: "Object-scoped events" })).toBeVisible();
    await capture("resource-detail-retained", "events");
    await drawerTab("Related");
    await expect(drawer.locator('[data-relationship-node="checkout"]')).toBeVisible();
    await capture("resource-detail-retained", "related");
    await drawerTab("Timeline");
    await drawer.getByRole("button", { name: "All retained", exact: true }).click();
    await expect(drawer.getByText("Image changed", { exact: true })).toBeVisible();
    const retained = replies.filter((reply) => reply.path.endsWith("/timeline") && new URLSearchParams(reply.query).get("since") === "1970-01-01T00:00:00.000Z");
    expect(retained).toHaveLength(2);
    const firstQuery = new URLSearchParams(retained[0].query), nextQuery = new URLSearchParams(retained[1].query);
    expect(nextQuery.get("cursor")).toBe("retained+/position");
    nextQuery.delete("cursor");
    expect(nextQuery.toString()).toBe(firstQuery.toString());
    expect(firstQuery.get("resource_uid")).toBe("checkout");
    expect(firstQuery.get("limit")).toBe("100");
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
    if (captureUnchanged) await page.screenshot({ path: capturePath("resource-detail-retained", width, theme, "timeline"), fullPage: true, animations: "disabled" });
    await page.getByRole("button", { name: "Close panel", exact: true }).click();
    await expect(health.getByRole("button", { name: "3 broken", exact: true })).toBeVisible();
    phase = "warming";
    await page.clock.setSystemTime(new Date("2026-10-28T00:13:00Z"));
    await page.getByRole("button", { name: "Refresh Kubernetes data" }).click();
    await expect(page.locator('[data-sync-state="preparing"]')).toHaveText("Syncing");
    await page.clock.runFor(60_000);
    await expect(health.getByRole("button", { name: "3 broken", exact: true })).toBeVisible();
    await expect(health.getByText(snapshot, { exact: true })).toBeVisible();
    await expect(cpu).toContainText("8");
    await expect(cpu).toContainText("1 core");
    await expect(memory).toContainText("2 GiB");
    await expect(page.getByText("No broken workloads", { exact: true })).toHaveCount(0);
    await expect(page.locator("[data-preparing-panel], [data-sync-strip]")).toHaveCount(0);
    expect(await page.getByRole("tablist", { name: "Kubernetes views" }).getByRole("tab").allTextContents()).toEqual(tabLabels);
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
    if (captureUnchanged) await page.screenshot({ path: capturePath("published-continuity", width, theme, "warming"), fullPage: true, animations: "disabled" });
    expect(replies.some((reply) => reply.phase === "warming" && reply.path.endsWith("/issues") && (reply.payload as { broken?: { total: number } }).broken?.total === 0)).toBe(true);
    phase = "recollected";
    await page.getByRole("button", { name: "Refresh Kubernetes data" }).click();
    await expect(health.getByText(`Snapshot ${new Date(recollected).toLocaleTimeString("en-US", { hour12: false })}`, { exact: true })).toBeVisible();
    await page.getByRole("button", { name: "Open resource checkout", exact: true }).click();
    await drawerTab("Logs");
    await expect(drawer.getByLabel("Workload log output")).toContainText("authorized fixture output");
    phase = "fault";
    await drawer.getByLabel("More log actions", { exact: true }).click();
    await drawer.getByRole("button", { name: "Reload logs", exact: true }).click();
    await expect(drawer.getByLabel("Workload log output")).toContainText("credential_unavailable");
    await expect(drawer.getByLabel("Workload log output")).not.toContainText("authorized fixture output");
    const permanentAttempts = logAttempts;
    await page.clock.runFor(7000);
    expect(logAttempts).toBe(permanentAttempts);
    await page.getByRole("button", { name: "Close panel", exact: true }).click();
    await page.getByRole("button", { name: "Refresh Kubernetes data" }).click();
    await expect(page.locator('[data-sync-state="failed"]')).toHaveText("Failed");
    await expect(health).toHaveCount(0);
    await expect(page.locator("[data-preparing-panel]")).toHaveCount(0);
    await expect(page.getByText("No broken workloads", { exact: true })).toHaveCount(0);
    await expect(page.locator("[data-sync-strip]")).toContainText("credentials are unavailable or rejected");
    if (captureUnchanged) await page.screenshot({ path: capturePath("published-continuity", width, theme, "credential-failure"), fullPage: true, animations: "disabled" });
    await page.reload();
    await expect(page.locator('[data-sync-state="failed"]')).toHaveText("Failed");
    await expect(page.locator("[data-preparing-panel]")).toHaveCount(0);
    // Prove an authoritative empty publication replaces authorized current data,
    // not merely the already-cleared credential-failure surface.
    phase = "recollected";
    await page.getByRole("button", { name: "Refresh Kubernetes data" }).click();
    await expect(health.getByRole("button", { name: "3 broken", exact: true })).toBeVisible();
    phase = "empty";
    await page.getByRole("button", { name: "Refresh Kubernetes data" }).click();
    await expect(health.getByRole("button", { name: "0 broken", exact: true })).toBeVisible();
    await expect(health.getByText("3 critical", { exact: true })).toHaveCount(0);
    await expect(page.getByText("No broken workloads", { exact: true })).toBeVisible();
    await capture("published-continuity", "complete-empty");
    await page.getByRole("tablist", { name: "Kubernetes views" }).getByRole("tab", { name: "Timeline", exact: true }).click();
    await expect(page.getByText("image changed", { exact: true })).toBeVisible();
    await expect(page.getByRole("tablist", { name: "Kubernetes views" }).getByRole("tab", { name: /Issues/ })).not.toContainText("90000");
    await page.getByRole("group", { name: "Timeline window" }).getByRole("button", { name: "All retained", exact: true }).click();
    await page.getByRole("combobox", { name: "Timeline kind", exact: true }).selectOption("Deployment");
    await page.getByRole("combobox", { name: "Timeline namespace", exact: true }).click();
    await page.getByRole("option", { name: "shop", exact: true }).click();
    const timeline = page.getByRole("region", { name: "Kubernetes change timeline" });
    await expect(timeline).toContainText("90000 events");
    await timeline.getByRole("navigation", { name: "Timeline pages" }).getByRole("button", { name: /Next/ }).click();
    await expect(timeline.getByText("replicas changed", { exact: true })).toBeVisible();
    await expect(page.getByRole("combobox", { name: "Timeline kind" })).toHaveValue("Deployment");
    const continued = replies.filter((reply) => reply.path.endsWith("/timeline") && new URLSearchParams(reply.query).get("cursor")?.startsWith("long+/"));
    expect(continued.length).toBeGreaterThanOrEqual(2);
    for (const reply of continued) {
      const query = new URLSearchParams(reply.query);
      expect(query.get("kind")).toBe("Deployment");
      expect(query.get("namespace")).toBe("shop");
      expect(query.get("since")).toBe("1970-01-01T00:00:00.000Z");
    }
    await capture("published-continuity", "empty-current-retained-history");
    phase = "denied";
    await page.getByRole("button", { name: "Refresh Kubernetes data" }).click();
    await expect(page.getByRole("region", { name: "Kubernetes change timeline" })).toHaveCount(0);
    await expect(page.locator("#kubernetes-view-panel")).not.toContainText("checkout");
    await expect(page.locator("body")).not.toContainText("authorized fixture output");
    await expect(page.locator('[data-sync-state="failed"]')).toHaveText("Failed");
    await expect(page.getByText("Cluster permission denied", { exact: true })).toBeVisible();
    await capture("published-continuity", "permission-denied");
    await testInfo.attach("sanitized-response-bodies", { body: JSON.stringify(replies), contentType: "application/json" });
  });
}