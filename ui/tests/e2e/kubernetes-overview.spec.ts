import { expect, test, type Locator, type Page } from "@playwright/test";
import * as fs from "node:fs";
import * as path from "node:path";
import * as https from "node:https";
import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
import { fileURLToPath, pathToFileURL } from "node:url";
import { openApp } from "./helpers";
import type { KubernetesClusters, KubernetesCrashEvent, KubernetesCrashPage, KubernetesGraph, KubernetesIssue, KubernetesOverview, KubernetesResourcePage, KubernetesTopPage, KubernetesTraffic, KubernetesUsage, RbacMe, SSOSession } from "../../src/lib/api";

type StreamCounter = { path: string; container: string; previous: boolean; follow: boolean; requests: number; active: number; cancelled: number; completed: number; disconnected: number; unavailable: number; chunks: number; since_time?: string };

const streamCounters = (cluster?: string) => new Promise<StreamCounter[]>((resolve, reject) => {
  const state = process.env.HARNESS_STATE_DIR || path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../../../plans/harness-run/.state");
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
      const state = process.env.HARNESS_STATE_DIR || path.join(root, "plans/harness-run/.state");
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

const screenshotDir = process.env.E2E_KUBERNETES_CAPTURE_DIR || path.resolve(path.dirname(fileURLToPath(import.meta.url)), "screenshots", "kubernetes", "fakekube");

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

async function captureBreakState(page: Page, filename: string, theme: "dark" | "light") {
  const proof = await page.evaluate((expected) => {
    const background = getComputedStyle(document.body).backgroundColor;
    const storedTheme = localStorage.getItem("versus.theme");
    if (expected === "dark" && storedTheme === "dark" && background === "rgb(13, 17, 24)") document.documentElement.setAttribute("data-theme", "dark");
    return { storedTheme, theme: document.documentElement.getAttribute("data-theme"), background, font: getComputedStyle(document.body).fontFamily, viewport: { width: innerWidth, height: innerHeight }, deviceScaleFactor: devicePixelRatio };
  }, theme);
  expect(proof.storedTheme).toBe(theme);
  expect(proof.theme).toBe(theme);
  if (theme === "dark") expect(proof.background).toBe("rgb(13, 17, 24)");
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  fs.mkdirSync(screenshotDir, { recursive: true });
  const image = await page.screenshot({ path: path.join(screenshotDir, filename), animations: "disabled" });
  expect(image.readUInt32BE(16)).toBe(proof.viewport.width * proof.deviceScaleFactor);
  expect(image.readUInt32BE(20)).toBe(proof.viewport.height * proof.deviceScaleFactor);
  const manifestPath = path.join(screenshotDir, "manifest.json");
  const manifest = fs.existsSync(manifestPath) ? JSON.parse(fs.readFileSync(manifestPath, "utf8")) as { images: Record<string, unknown> } : { images: {} };
  manifest.images[filename] = { classification: "F: synthetic API-intercepted UI fixture, not live-cluster acceptance", spec: "tests/e2e/kubernetes-overview.spec.ts", config: "tests/e2e/playwright.config.ts", sha256: createHash("sha256").update(image).digest("hex"), ...proof };
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
  const state = process.env.HARNESS_STATE_DIR || path.join(root, "plans/harness-run/.state");
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
      await page.getByRole("button", { name: "Select Pod payments/checkout-api-0", exact: true }).click();
      const detail = page.getByRole("dialog", { name: "Details panel" });
      await expect(detail).toBeVisible();
      await expect(detail.getByRole("heading", { name: "Pod payments/checkout-api-0", exact: true })).toBeVisible();
      await detail.getByRole("tab", { name: "Events", exact: true }).click();
      await expect(detail.getByText("No object-scoped events were returned.", { exact: true })).toBeVisible();
      fs.mkdirSync(screenshotDir, { recursive: true });
      await page.screenshot({ path: path.join(screenshotDir, `drawer-events-${viewport.name}.png`), fullPage: true, animations: "disabled" });
      const streamResponse = page.waitForResponse((response) => new URL(response.url()).pathname.endsWith("/logs/stream"));
      await detail.getByRole("tab", { name: "Logs", exact: true }).click();
      const streamed = await streamResponse;
      expect(streamed.status()).toBe(200);
      expect(streamed.headers()["content-type"]).toMatch(/^text\/event-stream(?:;|$)/);
      expect(new URL(streamed.url()).searchParams.get("timestamps")).toBe("false");
      const output = detail.getByLabel("Pod log output");
      await expect(detail.getByLabel("Log connection status")).toHaveText("live");
      await expect(output).toContainText("REDACTED");
      await expect(detail).not.toContainText("super-secret-value");
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
    await openApp(page, "/agent/kubernetes?view=overview");
    await page.getByRole("button", { name: "Select Pod payments/checkout-api-0", exact: true }).click();
    const detail = page.getByRole("dialog", { name: "Details panel", exact: true });
    await detail.getByRole("tab", { name: "Events", exact: true }).click();
    const events = detail.getByRole("region", { name: "Object-scoped events", exact: true });
    await expect(events.getByRole("heading", { name: "BackOff", exact: true })).toBeVisible();
    await expect(events).toContainText("Synthetic checkout container restart delayed");
    await expect(events).toContainText("Count 9; 2026-10-06T12:00:00Z");
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
  const state = process.env.HARNESS_STATE_DIR || path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../../../plans/harness-run/.state");
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
      await detail.getByLabel("Follow", { exact: true }).uncheck();
      await expect(detail.getByLabel("Follow", { exact: true })).not.toBeChecked();
      await output.evaluate((element) => { element.scrollTop = 0; element.dispatchEvent(new Event("scroll")); });
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
      await detail.getByLabel("Follow", { exact: true }).check();
      await detail.getByLabel("Scroll logs to bottom").click();
      await expect.poll(() => output.evaluate((element) => element.scrollHeight - element.scrollTop - element.clientHeight)).toBeLessThanOrEqual(48);
      await output.evaluate((element) => { element.scrollTop = 0; element.dispatchEvent(new Event("scroll")); });
      const scrolledKeys = await lines.count();
      await expect.poll(() => lines.count()).toBeGreaterThan(scrolledKeys + 3);
      expect(await output.evaluate((element) => element.scrollTop)).toBeLessThanOrEqual(1);
      await detail.getByLabel("Scroll logs to bottom").click();
      const beforeChange = (await current())!.cancelled;
      await detail.getByLabel("Log since seconds").selectOption("60");
      await expect.poll(() => requests.at(-1)?.searchParams.get("since_seconds")).toBe("60");
      await expect.poll(async () => (await current())!.cancelled).toBeGreaterThan(beforeChange);
      await detail.getByLabel("Log tail lines").selectOption("100");
      await expect.poll(() => requests.at(-1)?.searchParams.get("tail_lines")).toBe("100");
      const containers = await detail.getByLabel("Log container").locator("option").evaluateAll((options) => options.map((option) => (option as HTMLOptionElement).value));
      expect(containers).toEqual(["api", "sidecar", "setup", "debug"]);
      await expect(detail.getByLabel("Log container").locator("option")).toHaveText(["api (regular)", "sidecar (regular)", "setup (init)", "debug (ephemeral)"]);
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
        await expect(detail.getByRole("alert")).toContainText("previous_unavailable Previous container logs are unavailable. Select current container logs.");
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
      await expect(tabs.getByRole("tab", { name: /^(Logs|Actions)$/ })).toHaveCount(0);
      await expect(page).not.toHaveURL(/tab=actions/);
      await tabs.getByRole("tab", { name: "Events", exact: true }).focus();
      await page.keyboard.press("ArrowRight");
      await expect(tabs.getByRole("tab", { name: "YAML", exact: true })).toBeFocused();
      await page.evaluate(() => { window.history.pushState(null, "", "/agent/kubernetes?r=apps~v1~deployments/payments/checkout-api&tab=logs"); window.dispatchEvent(new PopStateEvent("popstate")); });
      await expect(tabs.getByRole("tab", { name: "Overview", exact: true })).toHaveAttribute("aria-selected", "true");
      await expect(page).not.toHaveURL(/tab=logs/);
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

  test.beforeEach(async ({ page }, testInfo) => {
    const overviewRefresh = testInfo.title.startsWith("Overview refresh");
    const breakTimeline = testInfo.title.startsWith("Break timeline");
    const fullOverview = testInfo.title === "Break timeline full populated dark Overview documentation";
    const overviewCounts = { Namespace: 3, Node: 12, Ingress: 2, Gateway: 1, HTTPRoute: 3, Service: 9, Deployment: 3, ReplicaSet: 6, StatefulSet: 1, DaemonSet: 1, Job: 2, CronJob: 1, Pod: 28, HorizontalPodAutoscaler: 2, ConfigMap: 12, Secret: 8, PersistentVolumeClaim: 4, Event: 26 };
    if (breakTimeline) await page.addInitScript(() => localStorage.setItem("versus.theme", "dark"));
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
    const pod = { resource_id: "core~v1~pods", kind: "Pod", namespace: "payments", name: "checkout-api-0", summary: { phase: "Running", log_containers: [{ name: "app", type: "regular" }, { name: "setup", type: "init" }, { name: "debug", type: "ephemeral" }] } };
    const deployment = { resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "payments", name: "checkout-api", summary: { ready_replicas: 1, desired_replicas: 1 } };
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
        if (url.searchParams.get("previous") === "true") return route.fulfill({ contentType: "text/event-stream", body: 'event: error\ndata: {"code":"previous_unavailable","message":"Previous logs unavailable"}\n\nevent: end\ndata: {"reason":"error"}\n\n' });
        const prefix = url.searchParams.get("timestamps") === "false" ? "" : "2026-10-07T12:00:00Z ";
        const lines = Array.from({ length: 300 }, (_, index) => `event: line\ndata: ${JSON.stringify({ text: prefix + (index % 2 ? "repeated token=[REDACTED] a.b" : "repeated axb"), container: url.searchParams.get("container") || "app", timestamp: "2026-10-07T12:00:00Z", sequence: index + 1, ordinal: index + 1, ...((index + 1) % 32 === 0 ? { cursor: `cursor-${index + 1}` } : {}) })}\n\n`).join("");
        return route.fulfill({ contentType: "text/event-stream", body: `event: heartbeat\ndata: {}\n\n${lines}event: heartbeat\ndata: {"cursor":"cursor-300"}\n\nevent: end\ndata: {"reason":"complete","cursor":"cursor-300"}\n\n` });
      }
      if (pathname === "/api/admin/config/agent") return route.fulfill({ json: { enable: breakTimeline, mode: "shadow", sources: [], ai: { enable: breakTimeline }, catalog: {}, miner: {}, regex: { rules: [] }, redaction: {}, service_patterns: [] } });
      if (multiClusterBreak && pathname === "/api/admin/kubernetes/clusters") return route.fulfill({ json: { multiple: true, clusters: ["east", "west"].map((id) => ({ id, provider: "generic", health: "attention", nodes: 12, ready_nodes: 12, pods: 28, running_pods: 19, warnings: 26, broken: 3, ...(testInfo.title.includes("fleet documentation") ? { last_change_at: id === "east" ? at(9) : "invalid-timestamp" } : {}), sync: { state: "ready" }, observed_at: new Date().toISOString() })) } });
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
      if (breakTimeline && pathname === "/api/admin/kubernetes/issues") {
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
      if (pathname.endsWith("/describe")) return route.fulfill({ json: { resource: pathname.includes("deployments") ? deployment : pod, events: [] } });
      if (pathname.startsWith("/api/admin/kubernetes/workloads/")) return route.fulfill({ json: { ...pod, truncated: false } });
      if (pathname === "/api/admin/kubernetes/resources") return route.fulfill({ json: { items: url.searchParams.get("resource_id") === "core~v1~namespaces" ? [{ resource_id: "core~v1~namespaces", kind: "Namespace", name: "payments" }] : [], truncated: false } });
      if (pathname.includes("/graph")) return route.fulfill({ json: { nodes: [{ id: "deployment", kind: "Deployment", namespace: "payments", name: "checkout-api", group: "payments" }, { id: "pod", kind: "Pod", namespace: "payments", name: "checkout-api-0", group: "payments" }], edges: [{ from: "deployment", to: "pod", type: "manages" }], omitted: {}, sync: { state: "ready", partial: false, age_s: 0 } } });
      if (pathname === "/api/admin/kubernetes/stream") return route.fulfill({ contentType: "text/event-stream", body: "event: heartbeat\ndata: {}\n\n" });
      if (pathname === "/api/admin/kubernetes/traffic") return route.fulfill({ json: { available: false, reason: "No traffic source", window: "15m", unmapped: 0, truncated: false } });
      if (pathname.includes("/enterprise/")) return route.fulfill({ status: 404, json: { error: "Community deployment" } });
      return route.fulfill({ json: { items: [], totals: {}, gaps: [], truncated: false, sync: { state: "ready", partial: false, age_s: 0 } } });
    });
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
      await expect(details).toContainText("41 lifetime restarts");
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
      await page.setViewportSize(viewport);
      await openApp(page, "/agent/kubernetes?view=timeline");
      const chart = page.getByRole("region", { name: "Resource timeline chart", exact: true });
      const marker = chart.locator('[data-marker="change"]').first();
      await marker.focus();
      await page.keyboard.press("Enter");
      const details = page.getByRole("region", { name: "Timeline event details", exact: true });
      await expect(details.getByRole("heading", { name: "Timeline event details", exact: true })).toBeFocused();
      for (const endpoint of ["changes", "issues"]) {
        await page.route(`**/api/admin/kubernetes/${endpoint}*`, (route) => route.fulfill({ json: { items: [], totals: {}, sync: { state: "ready", partial: false, age_s: 0 } } }));
      }
      await page.getByRole("button", { name: "Refresh Kubernetes data", exact: true }).click();
      await expect(details).toHaveCount(0);
      await expect(chart.locator('[data-marker="change"]')).toHaveCount(0);
      await expect(chart).toBeFocused();
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
    });
    test(`Break timeline external controls retain focus across filters and polling ${viewport.name} dark`, async ({ page }) => {
      await page.setViewportSize(viewport);
      await page.clock.install({ time: new Date() });
      await page.route("**/api/admin/kubernetes/issues*", (route) => route.fulfill({ json: { items: [], totals: {}, sync: { state: "ready", partial: false, age_s: 0 } } }));
      await openApp(page, "/agent/kubernetes?view=timeline");
      const chart = page.getByRole("region", { name: "Resource timeline chart", exact: true });
      const marker = chart.locator('[data-marker="change"]').first();
      const details = page.getByRole("region", { name: "Timeline event details", exact: true });
      const namespace = page.getByRole("textbox", { name: "Timeline namespace", exact: true });
      const kind = page.getByRole("combobox", { name: "Timeline kind", exact: true });
      await marker.focus();
      await page.keyboard.press("Enter");
      await expect(details.getByRole("heading", { name: "Timeline event details", exact: true })).toBeFocused();
      await namespace.focus();
      await namespace.pressSequentially("missing");
      await expect(namespace).toHaveValue("missing");
      await expect(namespace).toBeFocused();
      await expect(chart.locator('[data-marker="change"]')).toHaveCount(0);
      await expect(details).toHaveCount(0);
      await namespace.fill("");
      await marker.focus();
      await page.keyboard.press("Enter");
      await kind.focus();
      await kind.selectOption("Deployment");
      await expect(details).toHaveCount(0);
      await expect(kind).toBeFocused();
      await marker.focus();
      await page.keyboard.press("Enter");
      await namespace.focus();
      const previousUntil = await chart.getAttribute("data-until");
      for (const endpoint of ["changes", "issues"]) {
        await page.route(`**/api/admin/kubernetes/${endpoint}*`, (route) => route.fulfill({ json: { items: [], totals: {}, sync: { state: "ready", partial: false, age_s: 0 } } }));
      }
      await page.clock.fastForward(30_000);
      await expect(chart).not.toHaveAttribute("data-until", previousUntil!);
      await expect(chart.locator('[data-marker="change"]')).toHaveCount(0);
      await expect(details).toHaveCount(0);
      await expect(namespace).toBeFocused();
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
        await expect(lane.locator(".resource-timeline-label")).toHaveText(["Deployment checkoutshop", "ConfigMap checkout-cfgshop", "HPA checkout-hpashop"]);
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
      await expect(detail.getByText("300 / 300 lines", { exact: true })).toBeVisible();
      await expect(detail.getByLabel("Log container").locator("option")).toHaveText(["app (regular)", "setup (init)", "debug (ephemeral)"]);
      await detail.getByLabel("Log filter").fill("a.b");
      await expect(output).not.toContainText("axb");
      await expect(detail.getByText("150 / 300 lines", { exact: true })).toBeVisible();
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
      await detail.getByLabel("Previous container logs").check();
      await expect(detail.getByRole("alert")).toContainText("no previous container instance");
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
      await expect(tabs.getByRole("tab", { name: /^(Logs|Actions)$/ })).toHaveCount(0);
      await expect(page).not.toHaveURL(/tab=actions/);
      await tabs.getByRole("tab", { name: "Events", exact: true }).focus();
      await page.keyboard.press("ArrowRight");
      await expect(tabs.getByRole("tab", { name: "YAML", exact: true })).toBeFocused();
      await page.evaluate(() => { window.history.pushState(null, "", "/agent/kubernetes?r=apps~v1~deployments/payments/checkout-api&tab=logs"); window.dispatchEvent(new PopStateEvent("popstate")); });
      await expect(tabs.getByRole("tab", { name: "Overview", exact: true })).toHaveAttribute("aria-selected", "true");
      await expect(page).not.toHaveURL(/tab=logs/);
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
    await expect(resourceDetail.getByText("restart count", { exact: true }).locator("..")).toContainText("9");
    await expect(resourceDetail.getByText("Events", { exact: true }).and(resourceDetail.locator(":not([role=tab])")).locator("..")).toHaveText("Events0");
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