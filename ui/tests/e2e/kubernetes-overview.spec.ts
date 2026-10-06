import { expect, test, type Locator } from "@playwright/test";
import * as fs from "node:fs";
import * as path from "node:path";
import { fileURLToPath } from "node:url";
import { openApp } from "./helpers";
import type { KubernetesGraph, KubernetesOverview, KubernetesResourcePage, KubernetesTopPage, KubernetesTraffic, KubernetesUsage } from "../../src/lib/api";

async function expectNoTopologyPagination(panel: Locator) {
  await expect(panel.getByRole("button", { name: /^(Prev|Previous|Next)( page)?$/i })).toHaveCount(0);
  await expect(panel.getByText(/Bounded graph omitted|projected index is partial|Next page/i)).toHaveCount(0);
}

const screenshotDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "screenshots", "kubernetes", "fakekube");
const defaultKinds = ["Ingress", "Service", "Deployment", "Pod"];

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
  await expect(sidebar.getByRole("button", { name: "Refresh topology", exact: true })).toBeVisible();
  await expect(panel.getByRole("button", { name: "Refresh topology", exact: true })).toHaveCount(1);
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
    test(`captures logs ${viewport.name}`, async ({ page }) => {
      await page.setViewportSize({ width: viewport.width, height: viewport.height });
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
      await detail.getByRole("tab", { name: "Logs", exact: true }).click();
      await expect(detail.locator("pre")).toContainText("REDACTED");
      await expect(detail).not.toContainText("super-secret-value");
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