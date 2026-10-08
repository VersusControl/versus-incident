import { expect, test, type Locator } from "@playwright/test";
import * as fs from "node:fs";
import * as path from "node:path";
import * as https from "node:https";
import { execFileSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { openApp } from "./helpers";
import type { KubernetesGraph, KubernetesOverview, KubernetesResourcePage, KubernetesTopPage, KubernetesTraffic, KubernetesUsage } from "../../src/lib/api";

async function expectNoTopologyPagination(panel: Locator) {
  await expect(panel.getByRole("button", { name: /^(Prev|Previous|Next)( page)?$/i })).toHaveCount(0);
  await expect(panel.getByText(/Bounded graph omitted|projected index is partial|Next page/i)).toHaveCount(0);
}

const screenshotDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "screenshots", "kubernetes", "fakekube");
const defaultKinds = ["Ingress", "Service", "Deployment", "Pod"];

test.beforeEach(async ({ request }, testInfo) => {
  if (process.env.E2E_KUBERNETES_DOCS !== "1") return;
  const timelineCapture = testInfo.title === "captures populated timeline";
  const trafficCapture = testInfo.title === "captures populated traffic";
  if (!timelineCapture && !trafficCapture) return;
  test.setTimeout(240_000);
  const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../../..");
  const api = (endpoint: string) => JSON.parse(execFileSync(path.join(root, "plans/harness-run/harness.sh"), ["api", "enterprise", "GET", endpoint], { encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] }));
  const fake = async (endpoint: string, body?: unknown): Promise<string> => new Promise((resolve, reject) => {
    const call = https.request(`https://localhost:19443${endpoint}`, { method: body ? "POST" : "GET", ca: fs.readFileSync(path.join(root, "plans/harness-run/.state/fakekube/ca.crt")), headers: body ? { "Content-Type": "application/json" } : {} }, (response) => {
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
      expect((await request.post("http://127.0.0.1:19091/metrics/job/kubernetes-populated", { data: metrics, headers: { "Content-Type": "text/plain" } })).ok()).toBe(true);
      const samples = await request.get("http://127.0.0.1:19090/api/v1/query", { params: { query: 'min(count_over_time(istio_requests_total[15m]))' } });
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
  { id: "resources", label: "Resources" },
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

    for (const view of ["timeline", "releases", "gitops", "traffic", "topology"] as const) {
      test(`captures populated ${view}`, async ({ page }) => {
        if (view === "topology" && viewport.name === "desktop") await page.setViewportSize({ width: 1600, height: 1000 });
        const endpoint = { timeline: "changes", releases: "releases", gitops: "gitops/apps", traffic: "traffic", topology: "resources" }[view];
        const loaded = page.waitForResponse((response) => new URL(response.url()).pathname === `/api/admin/kubernetes/${endpoint}`);
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
          await expect(panel).not.toContainText(/not detected|no projected changes|no Helm release labels|no service flows/i);
          if (view !== "traffic") await expect(panel).not.toContainText(/unavailable/i);
          expect(await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth)).toBe(false);
          await page.locator("main").evaluate((main) => main.scrollTo({ top: 0 }));
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
        if (view === "traffic") {
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
      await page.goto(`${target.origin}/#/agent/tools/kubernetes`);
      await expect(page.getByRole("heading", { name: "Kubernetes Connector and Dashboard", exact: true })).toBeVisible();
      for (const name of ["timeline", "helm", "gitops", "traffic", "namespaces", "topology"]) {
        const image = page.locator(`.markdown-section img[src$="kubernetes-harness-${name}.png"]`);
        await image.scrollIntoViewIfNeeded();
        await expect(image).toBeVisible();
        await expect.poll(() => image.evaluate((element) => {
          const bitmap = element as HTMLImageElement;
          return bitmap.complete && bitmap.naturalWidth > 0 && bitmap.naturalHeight > 0;
        })).toBe(true);
      }
      expect(await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth)).toBe(false);
      fs.mkdirSync(screenshotDir, { recursive: true });
      await page.screenshot({ path: path.join(screenshotDir, `docs-consumer-${viewport.name}.png`), fullPage: true, animations: "disabled" });
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
          await page.getByRole("region", { name: "Nodes", exact: true }).scrollIntoViewIfNeeded();
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
      await page.getByLabel("Workload namespace").fill("payments");
      await page.getByLabel("Resource name").fill("checkout-api-0");
      await page.getByRole("button", { name: "Select Pod payments/checkout-api-0", exact: true }).click();
      const detail = page.getByRole("dialog", { name: "Details panel" });
      await expect(detail).toBeVisible();
      await detail.getByRole("tab", { name: "Events", exact: true }).click();
      await expect(detail.getByText("No object-scoped events were returned.", { exact: true })).toBeVisible();
      fs.mkdirSync(screenshotDir, { recursive: true });
      await page.screenshot({ path: path.join(screenshotDir, `drawer-events-${viewport.name}.png`), fullPage: true, animations: "disabled" });
      const streamRequest = page.waitForRequest((request) => new URL(request.url()).pathname.endsWith("/logs/stream"));
      await detail.getByRole("tab", { name: "Logs", exact: true }).click();
      expect(new URL((await streamRequest).url()).searchParams.get("timestamps")).toBe("false");
      const output = detail.getByLabel("Pod log output");
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
          const response = await page.request.get("/api/admin/kubernetes/resources", { params: { resource_id: resourceId, limit: "200", ...(cursor ? { cursor } : {}) } });
          expect(response.status()).toBe(200);
          const data: KubernetesResourcePage = await response.json();
          expect(data.sync.partial).toBe(false);
          count += data.items.length;
          cursor = data.next || "";
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
      expect(await blocks.getByRole("button").allTextContents()).toEqual([...new Set(inventory.items.map((item) => item.name))].sort());
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
          { from: `${namespace}-deployment`, to: `${namespace}-pod`, type: "manages" },
          ...(fresh ? [{ from: `${namespace}-pod`, to: `${namespace}-config`, type: "uses" }] : []),
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
  type Counter = { path: string; container: string; previous: boolean; follow: boolean; requests: number; active: number; cancelled: number; completed: number; disconnected: number; unavailable: number; chunks: number; since_time?: string };
  const logPath = "/api/v1/namespaces/payments/pods/checkout-api-0/log";
  const ca = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../../../../plans/harness-run/.state/fakekube/ca.crt");
  const counters = () => new Promise<Counter[]>((resolve, reject) => {
    const request = https.get("https://localhost:19443/_fake/log-streams", { ca: fs.readFileSync(ca), timeout: 5000 }, (response) => {
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
  const current = async (container = "api", previous = false) => (await counters()).find((counter) => counter.path === logPath && counter.container === container && counter.previous === previous && counter.follow === !previous);
  const active = async () => (await counters()).filter((counter) => counter.path === logPath).reduce((total, counter) => total + counter.active, 0);
  const control = (container: string, options: { disconnect_after_chunks?: number; remaining?: number; previous_available?: boolean } = {}) => new Promise<void>((resolve, reject) => {
    const body = JSON.stringify({ path: logPath, container, ...options });
    const request = https.request("https://localhost:19443/_fake/log-streams", { method: "POST", ca: fs.readFileSync(ca), timeout: 5000, headers: { "Content-Type": "application/json", "Content-Length": Buffer.byteLength(body) } }, (response) => {
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
      test.setTimeout(90_000);
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
      await expect.poll(active).toBe(0);
      expect((await current())!.cancelled).toBeGreaterThan(beforePause!.cancelled);
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
      const switched = (await current())!.cancelled;
      const alternative = "sidecar";
      const sidecarRequests = (await current(alternative))?.requests ?? 0;
      await detail.getByLabel("Log container").selectOption(alternative);
      await expect.poll(() => requests.at(-1)?.searchParams.get("container")).toBe(alternative);
      await expect.poll(async () => (await current())!.active).toBe(0);
      expect((await current())!.cancelled).toBeGreaterThan(switched);
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
      await detail.getByLabel("Pause logs").click();
      await expect.poll(active).toBe(0);
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
        await detail.getByLabel("Previous container logs").check();
        await expect(detail.getByRole("alert")).toContainText("no previous container instance");
        await expect.poll(async () => (await current(container, true))?.unavailable ?? 0).toBeGreaterThan(unavailable);
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
      await detail.getByLabel("Pause logs").click();
      await expect.poll(active).toBe(0);
      const captured = (await lines.allTextContents()).map((line) => line.replace(/\n$/, "")).join("\n");
      expect(captured).not.toMatch(rawSecrets);
      await detail.getByLabel("Copy scrubbed logs").click();
      expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(captured);
      const capturedDownload = page.waitForEvent("download");
      await detail.getByLabel("Download scrubbed logs").click();
      expect(fs.readFileSync((await (await capturedDownload).path())!, "utf8")).toBe(captured);
      expect(await page.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth)).toBe(false);
      fs.mkdirSync(screenshotDir, { recursive: true });
      await page.screenshot({ path: path.join(screenshotDir, `live-pod-stream-${viewport.name}.png`), fullPage: true, animations: "disabled" });
      await detail.getByLabel("Resume logs").click();
      await expect.poll(active).toBe(1);
      const beforeClose = (await current())!.cancelled;
      await detail.getByRole("button", { name: "Close panel" }).click();
      await expect.poll(active).toBe(0);
      expect((await current())!.cancelled).toBeGreaterThan(beforeClose);
      await openApp(page, "/agent/kubernetes?r=core~v1~pods/payments/checkout-api-0&tab=logs");
      await expect.poll(active).toBe(1);
      const beforeUnmount = (await current())!.cancelled;
      await page.goto("/now");
      await expect.poll(active).toBe(0);
      expect((await current())!.cancelled).toBeGreaterThan(beforeUnmount);
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
  test.beforeEach(async ({ page }, testInfo) => {
    const overviewRefresh = testInfo.title.startsWith("Overview refresh");
    const changeAt = new Date(Date.now() - 60_000).toISOString();
    const pod = { resource_id: "core~v1~pods", kind: "Pod", namespace: "payments", name: "checkout-api-0", summary: { phase: "Running", log_containers: [{ name: "app", type: "regular" }, { name: "setup", type: "init" }, { name: "debug", type: "ephemeral" }] } };
    const deployment = { resource_id: "apps~v1~deployments", kind: "Deployment", namespace: "payments", name: "checkout-api", summary: { ready_replicas: 1, desired_replicas: 1 } };
    await page.addInitScript(() => {
      const signals: Array<{ url: string; aborted: boolean }> = [];
      Object.defineProperty(window, "podStreamSignals", { value: signals });
      const original = window.fetch;
      window.fetch = (input, init) => {
        const url = String(input);
        if (url.includes("/logs/stream")) {
          const entry = { url, aborted: false };
          signals.push(entry);
          init?.signal?.addEventListener("abort", () => { entry.aborted = true; }, { once: true });
        }
        return original(input, init);
      };
    });
    await page.route("**/api/**", async (route) => {
      const url = new URL(route.request().url());
      const pathname = url.pathname;
      if (pathname.endsWith("/logs/stream")) {
        if (url.searchParams.get("previous") === "true") return route.fulfill({ contentType: "text/event-stream", body: 'event: error\ndata: {"code":"previous_unavailable","message":"Previous logs unavailable"}\n\nevent: end\ndata: {"reason":"error"}\n\n' });
        const prefix = url.searchParams.get("timestamps") === "false" ? "" : "2026-10-07T12:00:00Z ";
        const lines = Array.from({ length: 300 }, (_, index) => `event: line\ndata: ${JSON.stringify({ text: prefix + (index % 2 ? "repeated token=[REDACTED] a.b" : "repeated axb"), container: url.searchParams.get("container") || "app", timestamp: "2026-10-07T12:00:00Z", sequence: index + 1, ordinal: index + 1, ...((index + 1) % 32 === 0 ? { cursor: `cursor-${index + 1}` } : {}) })}\n\n`).join("");
        return route.fulfill({ contentType: "text/event-stream", body: `event: heartbeat\ndata: {}\n\n${lines}event: heartbeat\ndata: {"cursor":"cursor-300"}\n\nevent: end\ndata: {"reason":"complete","cursor":"cursor-300"}\n\n` });
      }
      if (pathname === "/api/admin/config/agent") return route.fulfill({ json: { enable: false, mode: "shadow", sources: [], ai: { enable: false }, catalog: {}, miner: {}, regex: { rules: [] }, redaction: {}, service_patterns: [] } });
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

  for (const viewport of [{ name: "desktop", width: 1440, height: 1100 }, { name: "mobile", width: 390, height: 844 }]) {
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