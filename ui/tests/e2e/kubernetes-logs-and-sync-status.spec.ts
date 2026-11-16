// Browser gate for two operator-facing states of the Kubernetes explorer:
//
// 1. The Pod drawer's Live logs card while a row-opened, two-container Pod is
//    still being described. The first read names no container, the cluster
//    refuses it, and the card must wait ("Connecting", "Waiting for logs…")
//    instead of printing the raw refusal class; once the describe lands it
//    streams the application container. A genuine failure afterwards reads as
//    one plain sentence under an "Unavailable" badge.
// 2. A failed or degraded cluster sync. With data already on screen the top bar
//    carries the state and dates the data, and a details popover (hover, focus
//    or press) names each failing resource type with Retry sync. A synced
//    cluster that lost access with nothing on screen, and a cluster whose first
//    sync failed, each get the full in-page failure panel.
//
// Everything is deterministic: the SPA is the Vite dev build of the current
// source and every `/api/**` answer is an intercepted fixture that mirrors the
// server contract. The Pod log stream is answered in the page (a controlled
// SSE body), because a route fulfil cannot hold a stream open. Captures are
// fixture evidence, not live-cluster evidence.
//
// Run with E2E_UI_FIXTURES=1 so playwright.config.ts starts the dev server.
import { expect, test, type Locator, type Page, type Route } from "@playwright/test";
import { capturePath } from "./support/capture";

type Viewport = { name: "desktop" | "mobile"; width: number; height: number };
type Theme = "dark" | "light";
const viewports: Viewport[] = [
  { name: "desktop", width: 1440, height: 900 },
  { name: "mobile", width: 390, height: 844 },
];
const themes: Theme[] = ["dark", "light"];

const minute = 60_000;
// The data on screen was observed seven minutes before the test opens the page,
// so the label's HH:MM is a real, recent wall-clock time.
const observedAt = () => new Date(Date.now() - 7 * minute).toISOString();
const firstReadyAt = "2026-11-10T08:00:00Z";
const zeroTime = "0001-01-01T00:00:00Z";

const pod = { namespace: "shop", name: "checkout-7d9f8c6b5-x2kqp", uid: "pod-checkout-uid" };

type SyncMode =
  | "ready"
  | "failed-with-data"
  | "degraded-with-data"
  | "failed-long-list"
  | "access-lost"
  | "access-lost-overview-ok"
  | "never-synced";

type LogMode = "refuse-then-stream" | "refuse-then-fail";

type Fixture = { sync: SyncMode; observed: string; describeDelayMs: number; overviewWithoutSync?: boolean };

const readyKind = (observed: string) => ({ state: "ready", observed_at: observed, records: 12, partial: false });
const errorKind = (error: string, since: string) => ({ state: "error", observed_at: zeroTime, last_attempt_at: new Date().toISOString(), failing_since: since, records: 0, partial: true, error });

function syncReport(fixture: Fixture) {
  const ago = (minutes: number) => new Date(Date.now() - minutes * minute).toISOString();
  const base = (state: string, kinds: Record<string, unknown>, failing: number) => ({
    state,
    partial: state !== "ready",
    age_s: state === "ready" ? 2 : 420,
    kinds,
    total_kinds: Object.keys(kinds).length,
    ready_kinds: Object.values(kinds).filter((kind) => (kind as { state: string }).state === "ready").length,
    failing_kinds: failing,
    pending_kinds: 0,
    truncated: false,
    failing: [],
    unreadable: [],
  });
  const ready = readyKind(fixture.observed);
  switch (fixture.sync) {
    case "ready":
      return { ...base("ready", { Pod: ready, Node: ready, Deployment: ready }, 0), first_ready_at: firstReadyAt };
    case "failed-with-data":
      return {
        ...base("error", {
          Pod: ready,
          Node: errorKind("forbidden", ago(12)),
          Secret: errorKind("credential_unavailable", ago(9)),
          Deployment: ready,
        }, 2),
        first_ready_at: firstReadyAt,
      };
    case "degraded-with-data":
      return {
        ...base("partial", {
          Pod: ready,
          Node: { ...ready, partial: true, error: "record_rejected", skipped: 2, failing_since: ago(6) },
          Deployment: ready,
        }, 1),
        first_ready_at: firstReadyAt,
      };
    case "failed-long-list": {
      const classes = ["forbidden", "timeout", "credential_unavailable", "unavailable", "budget_exhausted", "record_limit", "incomplete_pages"];
      const names = ["ConfigMap", "CronJob", "DaemonSet", "Endpoints", "HorizontalPodAutoscaler", "Ingress", "Job", "Lease", "NetworkPolicy", "Node", "PersistentVolumeClaim", "ReplicaSet", "Role", "Secret", "Service", "StatefulSet"];
      const kinds: Record<string, unknown> = { Pod: ready, Deployment: ready };
      names.forEach((name, index) => { kinds[name] = errorKind(classes[index % classes.length], ago(3 + index)); });
      return { ...base("error", kinds, names.length), first_ready_at: firstReadyAt };
    }
    case "never-synced":
      return base("error", {
        Pod: errorKind("forbidden", ago(4)),
        Node: errorKind("forbidden", ago(4)),
        Deployment: errorKind("credential_unavailable", ago(4)),
      }, 3);
    case "access-lost":
    case "access-lost-overview-ok":
      return null;
  }
}

function overview(fixture: Fixture) {
  return {
    connector: "kubernetes",
    cluster_id: "fixture",
    observed_at: fixture.observed,
    nodes: 6,
    ready_nodes: 6,
    pods: 42,
    running_pods: 41,
    namespaces: 4,
    active_namespaces: 3,
    workloads: 18,
    warnings: 1,
    truncated: false,
    sync: { state: fixture.sync === "ready" ? "ready" : "error", age_s: 420, partial: fixture.sync !== "ready" },
  };
}

const podRow = { resource_id: "core~v1~pods", kind: "Pod", namespace: pod.namespace, name: pod.name, uid: pod.uid, summary: { phase: "Running", restart_count: 0, node: "node-a" } };

async function installApi(page: Page, fixture: Fixture) {
  const answer = (route: Route, json: unknown, status = 200) => route.fulfill({ json, status });
  const credentialLost = { error: "Cluster credentials unavailable", code: "credential_unavailable", first_ready_at: firstReadyAt };
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    const path = url.pathname;
    if (path === "/api/admin/config/agent") return answer(route, { enable: false, mode: "detect", ai: { enable: false }, sources: [] });
    if (path === "/api/admin/kubernetes/clusters") return answer(route, { multiple: false, clusters: [] });
    if (path === "/api/admin/kubernetes/stream") return route.fulfill({ contentType: "text/event-stream", body: "event: heartbeat\ndata: {}\n\n" });
    if (path === "/api/admin/kubernetes/sync") {
      if (fixture.sync === "access-lost" || fixture.sync === "access-lost-overview-ok") return answer(route, credentialLost, 502);
      return answer(route, syncReport(fixture));
    }
    if (path === "/api/admin/kubernetes/overview") {
      if (fixture.sync === "access-lost") return answer(route, credentialLost, 502);
      if (fixture.sync === "never-synced") return answer(route, { error: "Kubernetes inventory has not finished syncing", code: "inventory_not_ready", retryable: true }, 503);
      const body: Record<string, unknown> = overview(fixture);
      if (fixture.overviewWithoutSync) delete body.sync;
      return answer(route, body);
    }
    if (path === "/api/admin/kubernetes/namespaces") return answer(route, { items: [pod.namespace], sync: overview(fixture).sync });
    if (path === "/api/admin/kubernetes/issue-alerts") return answer(route, { enabled: false, level: "critical", can_write: false, available: true, reason: "", links_ready: true, channels_enabled: true });
    if (path === "/api/admin/kubernetes/workloads") {
      return answer(route, { items: [podRow], counts: { Deployment: 0, StatefulSet: 0, DaemonSet: 0, Job: 0, Pod: 1, CronJob: 0 }, observed_at: fixture.observed, truncated: false, total: 1, limit: 20, sync: overview(fixture).sync });
    }
    if (path.endsWith("/describe")) {
      // A slow describe: the drawer opens on the row identity and the log card
      // starts its read before the Pod's containers are known.
      await new Promise((resolve) => setTimeout(resolve, fixture.describeDelayMs));
      return answer(route, {
        resource: {
          ...podRow,
          summary: {
            phase: "Running",
            node: "node-a",
            restart_count: 0,
            log_containers: [{ name: "app", type: "regular" }, { name: "istio-proxy", type: "regular" }],
            main_container: "app",
          },
        },
      }).catch(() => undefined);
    }
    if (path.includes("/deployment") || path.includes("/sso/")) return answer(route, { error: "Community" }, 403);
    if (path.startsWith("/api/admin/kubernetes/")) return answer(route, { items: [], nodes: [], edges: [], omitted: {}, truncated: false, available: false, gaps: [], sync: overview(fixture).sync });
    return answer(route, { items: [], incidents: [], total: 0, by_origin: {}, enable: false });
  });
}

/**
 * Answers the Pod log stream in the page. A read that names no container is
 * refused the way the cluster refuses it for a multi-container Pod (with the
 * failure class repeated as the message, the worst case for the UI). A read of
 * `app` streams lines, or fails with a real upstream failure.
 */
async function installLogStream(page: Page, mode: LogMode) {
  await page.addInitScript((logMode) => {
    const original = window.fetch.bind(window);
    const reads: string[] = [];
    (window as Window & { __logReads?: string[] }).__logReads = reads;
    window.fetch = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (!url.includes("/logs/stream") || !url.includes("/pods/")) return original(input, init);
      const container = new URL(url, location.origin).searchParams.get("container") ?? "";
      reads.push(container);
      const encoder = new TextEncoder();
      const timers: Array<ReturnType<typeof setTimeout>> = [];
      let closed = false;
      const body = new ReadableStream<Uint8Array>({
        start(controller) {
          const send = (event: string, payload: unknown) => {
            if (!closed) controller.enqueue(encoder.encode(`event: ${event}\ndata: ${JSON.stringify(payload)}\n\n`));
          };
          const finish = () => { if (!closed) { closed = true; controller.close(); } };
          send("heartbeat", {});
          if (!container) {
            timers.push(setTimeout(() => { send("error", { code: "invalid_arguments", message: "invalid_arguments", reason: "error", retryable: false }); finish(); }, 250));
            return;
          }
          if (logMode === "refuse-then-fail") {
            timers.push(setTimeout(() => { send("error", { code: "upstream_unavailable", message: "upstream_unavailable", reason: "error", retryable: false }); finish(); }, 250));
            return;
          }
          const now = Date.now();
          const messages = ["GET /api/cart 200 12ms", "POST /api/checkout 201 48ms", "cache warm for catalog shard 3", "GET /healthz 200 1ms", "warn: payment provider latency 820ms"];
          let sequence = 0;
          const line = (at: number) => {
            sequence++;
            send("line", { text: `${new Date(at).toISOString()} ${messages[sequence % messages.length]}`, container: "app", timestamp: new Date(at).toISOString(), sequence, ordinal: sequence, cursor: `c${sequence}` });
          };
          for (let index = 0; index < 24; index++) line(now - (24 - index) * 1000);
          const tick = () => { line(Date.now()); timers.push(setTimeout(tick, 1000)); };
          timers.push(setTimeout(tick, 1000));
        },
        cancel() {
          closed = true;
          timers.forEach(clearTimeout);
        },
      });
      return new Response(body, { status: 200, headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-store" } });
    };
  }, mode);
}

async function open(page: Page, viewport: Viewport, theme: Theme, fixture: Fixture, url: string) {
  await page.setViewportSize({ width: viewport.width, height: viewport.height });
  await page.emulateMedia({ colorScheme: theme });
  await page.addInitScript((value) => localStorage.setItem("versus.theme", value), theme);
  await installApi(page, fixture);
  await page.goto(url);
  await expect(page.getByTestId("app-authenticated")).toBeVisible({ timeout: 30_000 });
  if (theme === "light") await expect(page.locator("html")).toHaveAttribute("data-theme", "light");
  else await expect(page.locator("html")).not.toHaveAttribute("data-theme", "light");
}

async function noHorizontalOverflow(page: Page, where: string) {
  const widths = await page.evaluate(() => ({ scroll: document.documentElement.scrollWidth, inner: innerWidth }));
  expect(widths.scroll, `${where} must not scroll sideways`).toBeLessThanOrEqual(widths.inner);
}

/**
 * WCAG contrast of an element's text against the first opaque background
 * behind it. Colours are resolved through a canvas so any CSS colour syntax
 * (oklch, color-mix) is measured as rendered sRGB.
 */
async function contrast(locator: Locator): Promise<number> {
  return locator.evaluate((element) => {
    const canvas = document.createElement("canvas");
    canvas.width = canvas.height = 1;
    const context = canvas.getContext("2d", { willReadFrequently: true })!;
    const rgba = (color: string) => {
      context.clearRect(0, 0, 1, 1);
      context.fillStyle = "#000";
      context.fillStyle = color;
      context.fillRect(0, 0, 1, 1);
      const [r, g, b, a] = context.getImageData(0, 0, 1, 1).data;
      return { r, g, b, a: a / 255 };
    };
    const layers: Array<{ r: number; g: number; b: number; a: number }> = [];
    for (let node: Element | null = element; node; node = node.parentElement) {
      const background = getComputedStyle(node).backgroundColor;
      if (!background || background === "transparent" || background === "rgba(0, 0, 0, 0)") continue;
      const layer = rgba(background);
      layers.push(layer);
      if (layer.a >= 1) break;
    }
    if (!layers.length || layers[layers.length - 1].a < 1) layers.push(rgba(getComputedStyle(document.body).backgroundColor || "#fff"));
    let base = layers.pop()!;
    while (layers.length) {
      const top = layers.pop()!;
      base = { r: top.r * top.a + base.r * (1 - top.a), g: top.g * top.a + base.g * (1 - top.a), b: top.b * top.a + base.b * (1 - top.a), a: 1 };
    }
    const ink = rgba(getComputedStyle(element).color);
    const fg = { r: ink.r * ink.a + base.r * (1 - ink.a), g: ink.g * ink.a + base.g * (1 - ink.a), b: ink.b * ink.a + base.b * (1 - ink.a) };
    const luminance = ({ r, g, b }: { r: number; g: number; b: number }) => {
      const channel = (value: number) => { const v = value / 255; return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4; };
      return 0.2126 * channel(r) + 0.7152 * channel(g) + 0.0722 * channel(b);
    };
    const [light, dark] = [luminance(fg), luminance(base)].sort((left, right) => right - left);
    return (light + 0.05) / (dark + 0.05);
  });
}

async function expectContrast(locator: Locator, what: string) {
  const ratio = await contrast(locator);
  expect(ratio, `${what} contrast ${ratio.toFixed(2)}:1 must be at least 4.5:1`).toBeGreaterThanOrEqual(4.5);
  return ratio;
}

/** The popover sits fully inside the viewport and below its trigger. */
async function expectInsideViewport(page: Page, locator: Locator, what: string) {
  const box = await locator.boundingBox();
  const size = page.viewportSize()!;
  expect(box, `${what} must render`).not.toBeNull();
  expect(box!.x, `${what} left edge`).toBeGreaterThanOrEqual(0);
  expect(box!.y, `${what} top edge`).toBeGreaterThanOrEqual(0);
  expect(box!.x + box!.width, `${what} right edge`).toBeLessThanOrEqual(size.width + 0.5);
  expect(box!.y + box!.height, `${what} bottom edge`).toBeLessThanOrEqual(size.height + 0.5);
}

/** Waits for finite entrance animations so a capture shows the settled state. */
async function settle(page: Page) {
  await page.evaluate(() => Promise.all(document.getAnimations()
    .filter((animation) => animation.effect?.getComputedTiming().iterations !== Infinity)
    .map((animation) => animation.finished.catch(() => undefined))));
}

// The label uses a 24-hour clock, like the snapshot time beside it.
/** The panel's state badge has the shared pill shape, not tinted plain text. */
async function expectPillBadge(panel: Locator, label: string) {
  const badge = panel.locator("span", { hasText: new RegExp(`^${label}$`) }).first();
  await expect(badge).toHaveClass(/(^|\s)pill(\s|$)/);
  const shape = await badge.evaluate((el) => ({ radius: parseFloat(getComputedStyle(el).borderTopLeftRadius), padding: parseFloat(getComputedStyle(el).paddingLeft), size: parseFloat(getComputedStyle(el).fontSize) }));
  expect(shape.radius, "rounded pill").toBeGreaterThan(4);
  expect(shape.padding, "padded pill").toBeGreaterThan(0);
  expect(shape.size, "pill text is smaller than the 14px heading").toBeLessThan(14);
}

const hhmm = (page: Page, iso: string) => page.evaluate((value) => new Date(value).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hourCycle: "h23" }), iso);

test.describe("Kubernetes Pod logs while describing, and the sync failure status", () => {
  test.skip(process.env.E2E_UI_FIXTURES !== "1", "deterministic intercepted UI evidence, not live collection");

  test("the dev server serves the current log and sync status source", async ({ page }) => {
    await page.goto("/");
    const viewer = await (await page.request.get("/src/components/PodLogViewer.tsx")).text();
    const indicator = await (await page.request.get("/src/components/KubernetesSyncStateIndicator.tsx")).text();
    const surface = await (await page.request.get("/src/components/KubernetesSyncHealth.tsx")).text();
    expect(viewer).toContain("containerlessRefusal");
    expect(indicator).toContain("Sync status details");
    expect(surface).toContain("Cannot read this cluster right now");
  });

  for (const mode of ["refuse-then-stream", "refuse-then-fail"] as const) {
    test(`a row-opened two-container Pod waits for its describe, then ${mode === "refuse-then-stream" ? "streams the app container" : "names a real failure plainly"}`, async ({ page }) => {
      test.setTimeout(300_000);
      for (const viewport of viewports) {
        for (const theme of themes) {
          await page.unrouteAll({ behavior: "ignoreErrors" });
          const fixture: Fixture = { sync: "ready", observed: observedAt(), describeDelayMs: 4_500 };
          await installLogStream(page, mode);
          await open(page, viewport, theme, fixture, "/agent/kubernetes?view=resources");
          const row = page.getByRole("button", { name: `Select Pod ${pod.namespace}/${pod.name}` });
          await row.click();
          const card = page.getByRole("region", { name: "Pod logs" }).or(page.locator('[aria-label="Pod logs"]')).first();
          await expect(card).toBeVisible();
          const status = card.getByLabel("Log connection status");
          const output = card.getByLabel("Pod log output");
          // While the describe is pending the refused containerless read is a wait.
          await expect.poll(() => page.evaluate(() => (window as Window & { __logReads?: string[] }).__logReads ?? [])).toContain("");
          await expect(status).toHaveText("Connecting");
          await expect(output).toContainText("Waiting for logs…");
          // The waiting selector names the default without clipping it at 390.
          const waitingSelect = card.getByLabel("Log container");
          await expect(waitingSelect.locator("option:checked")).toHaveText("Default");
          await page.waitForTimeout(600);
          await expect(status).toHaveText("Connecting");
          await expect(output).toContainText("Waiting for logs…");
          expect(await page.locator("body").innerText()).not.toMatch(/invalid_arguments|did not name a container/);
          await expectContrast(status, "log status badge");
          await expectContrast(output, "waiting copy");
          await noHorizontalOverflow(page, "drawer while describing");
          if (mode === "refuse-then-stream") {
            await settle(page); await card.screenshot({ path: capturePath("pod-live-logs", viewport.name, theme, "describing-waiting") });
            if (viewport.name === "desktop" && theme === "dark") await settle(page); await page.screenshot({ path: capturePath("pod-drawer", viewport.name, theme, "describing-waiting") });
          }

          // The describe lands: the card adopts `app` and reads it.
          await expect.poll(() => page.evaluate(() => (window as Window & { __logReads?: string[] }).__logReads ?? []), { timeout: 15_000 }).toContain("app");
          const select = card.getByLabel("Log container");
          if (mode === "refuse-then-stream") {
            await expect(status).toHaveText("Live", { timeout: 15_000 });
            await expect(output).toContainText("POST /api/checkout 201");
            await expect(select).toHaveValue("app");
            expect(await select.locator("option").allTextContents()).toEqual(expect.arrayContaining(["app", "istio-proxy"]));
            expect(await page.locator("body").innerText()).not.toMatch(/invalid_arguments|did not name a container/);
            await expectContrast(status, "live badge");
            await noHorizontalOverflow(page, "drawer while streaming");
            await settle(page); await card.screenshot({ path: capturePath("pod-live-logs", viewport.name, theme, "streaming") });
            await settle(page); await page.screenshot({ path: capturePath("pod-drawer", viewport.name, theme, "streaming") });
          } else {
            await expect(status).toHaveText("Unavailable", { timeout: 15_000 });
            await expect(output).toContainText("The node serving these logs could not be reached.");
            const text = await page.locator("body").innerText();
            expect(text).not.toMatch(/upstream_unavailable|invalid_arguments/);
            await expectContrast(output, "failure sentence");
            await noHorizontalOverflow(page, "drawer after a log failure");
            await settle(page); await card.screenshot({ path: capturePath("pod-live-logs", viewport.name, theme, "failure-unavailable") });
            if (viewport.name === "mobile") await settle(page); await page.screenshot({ path: capturePath("pod-drawer", viewport.name, theme, "failure-unavailable") });
          }
          const reads = await page.evaluate(() => (window as Window & { __logReads?: string[] }).__logReads ?? []);
          expect(reads[0], "the first read went out before the describe").toBe("");
          expect(reads).not.toContain("istio-proxy");
          await page.goto("about:blank");
        }
      }
    });
  }

  for (const sync of ["failed-with-data", "degraded-with-data"] as const) {
    test(`${sync}: the top bar dates the data and its details popover opens on hover and focus`, async ({ page }) => {
      test.setTimeout(300_000);
      const failed = sync === "failed-with-data";
      const head = failed ? "Sync failed" : "Sync degraded";
      for (const viewport of viewports) {
        for (const theme of themes) {
          await page.unrouteAll({ behavior: "ignoreErrors" });
          const fixture: Fixture = { sync, observed: observedAt(), describeDelayMs: 0 };
          await open(page, viewport, theme, fixture, "/agent/kubernetes");
          const time = await hhmm(page, fixture.observed);
          const indicator = page.locator(`[data-sync-state="${failed ? "failed" : "degraded"}"]`);
          await expect(indicator).toHaveText(`${head} — showing data from ${time}`);
          // One clock on the page: a 24-hour HH:MM like the snapshot time.
          await expect(indicator).toHaveText(/ \d{2}:\d{2}$/);
          // The views stay open with their data; no in-page failure section.
          await expect(page.getByRole("region", { name: "Cluster health" })).toBeVisible();
          await expect(page.locator("[data-sync-health]")).toHaveCount(0);
          await expect(page.getByRole("button", { name: "Retry sync" })).toHaveCount(0);
          const trigger = indicator.locator("[data-sync-details-trigger]");
          // Colour: red for failed, amber for degraded, and readable either way.
          const tone = await indicator.evaluate((el) => el.className);
          expect(tone).toContain(failed ? "text-sev-critical" : "text-sev-warning");
          // Rest state: the pointer is parked away from the top bar.
          await page.mouse.move(1, viewport.height - 1);
          await expect(page.getByRole("region", { name: "Sync status details" })).toBeHidden();
          const restRatio = await contrast(trigger);
          test.info().annotations.push({ type: "contrast", description: `${sync} ${viewport.name}/${theme} label at rest ${restRatio.toFixed(2)}:1` });
          expect.soft(restRatio, `${head} label at rest ${restRatio.toFixed(2)}:1 must be at least 4.5:1`).toBeGreaterThanOrEqual(4.5);
          const truncated = await trigger.locator("span.truncate").evaluate((el) => ({ scroll: el.scrollWidth, client: el.clientWidth, text: el.textContent }));
          test.info().annotations.push({ type: "label-fit", description: `${viewport.name}/${theme}/${sync}: scrollWidth=${truncated.scroll} clientWidth=${truncated.client} text="${truncated.text}"` });
          await noHorizontalOverflow(page, "top bar with a failure label");
          await settle(page); await page.screenshot({ path: capturePath("sync-status", viewport.name, theme, `${sync}-closed`) });

          // Hover opens the details.
          const details = page.getByRole("region", { name: "Sync status details" });
          await trigger.hover();
          await expect(details).toBeVisible();
          await expect(details.locator("p").first()).toHaveText(head);
          const problems = details.locator("[data-sync-problem]");
          if (failed) {
            await expect(problems).toHaveCount(2);
            await expect(details.locator('[data-sync-problem="Node"]')).toContainText("permission denied");
            await expect(details.locator('[data-sync-problem="Node"]')).toContainText(/since 1[12] minutes ago/);
            await expect(details.locator('[data-sync-problem="Secret"]')).toContainText("cluster credentials are unavailable or rejected");
          } else {
            await expect(problems).toHaveCount(1);
            await expect(details.locator('[data-sync-problem="Node"]')).toContainText("some objects could not be indexed");
            await expect(details.locator('[data-sync-problem="Node"]')).toContainText("2 objects skipped");
          }
          await expect(details.getByRole("button", { name: "Retry sync" })).toBeVisible();
          const hoverRatio = await contrast(trigger);
          test.info().annotations.push({ type: "contrast", description: `${sync} ${viewport.name}/${theme} label on hover ${hoverRatio.toFixed(2)}:1` });
          expect.soft(hoverRatio, `${head} label on hover ${hoverRatio.toFixed(2)}:1 must be at least 4.5:1`).toBeGreaterThanOrEqual(4.5);
          await expectInsideViewport(page, details, "details popover");
          const panelBox = await details.boundingBox(); const triggerBox = await trigger.boundingBox();
          test.info().annotations.push({ type: "popover-box", description: `${sync} ${viewport.name}/${theme} popover ${JSON.stringify(panelBox)} trigger ${JSON.stringify(triggerBox)}` });
          await expectContrast(details.locator("p").first(), "popover title");
          await expectContrast(problems.first(), "popover reason");
          await noHorizontalOverflow(page, "open details popover");
          await settle(page); await page.screenshot({ path: capturePath("sync-status", viewport.name, theme, `${sync}-hover`) });

          // The pointer can travel into the panel; leaving the panel closes it.
          await details.hover();
          await expect(details).toBeVisible();
          await page.mouse.move(viewport.width / 2, viewport.height - 4);
          await expect(details).toBeHidden();
          // Keyboard: reach the trigger with a real Tab so :focus-visible applies.
          await trigger.focus();
          await page.keyboard.press("Shift+Tab");
          await expect(details).toBeHidden();
          await page.keyboard.press("Tab");
          await expect(trigger).toBeFocused();
          await expect(details).toBeVisible();
          const outline = await trigger.evaluate((el) => ({ style: getComputedStyle(el).outlineStyle, width: getComputedStyle(el).outlineWidth, visible: el.matches(":focus-visible") }));
          expect(outline.visible, "keyboard focus matches :focus-visible").toBe(true);
          expect(outline.style, "the focused trigger shows a visible outline").not.toBe("none");
          expect(parseFloat(outline.width)).toBeGreaterThanOrEqual(2);
          const focusRatio = await contrast(trigger);
          test.info().annotations.push({ type: "contrast", description: `${sync} ${viewport.name}/${theme} label on keyboard focus ${focusRatio.toFixed(2)}:1` });
          if (theme === "dark") await settle(page); await page.screenshot({ path: capturePath("sync-status", viewport.name, theme, `${sync}-focus-trigger`) });
          // Tab reaches Retry sync inside the popover.
          let reached = false;
          for (let step = 0; step < 4 && !reached; step++) {
            await page.keyboard.press("Tab");
            reached = await page.evaluate(() => document.activeElement?.hasAttribute("data-sync-retry") ?? false);
          }
          expect(reached, "Tab reaches Retry sync").toBe(true);
          await expect(details).toBeVisible();
          if (theme === "dark" || viewport.name === "mobile") await settle(page); await page.screenshot({ path: capturePath("sync-status", viewport.name, theme, `${sync}-focus-retry`) });
          // Escape closes it and returns focus to the trigger.
          await page.keyboard.press("Escape");
          await expect(details).toBeHidden();
          await expect(trigger).toBeFocused();
          // A press outside closes a hover-opened panel too.
          await trigger.hover();
          await expect(details).toBeVisible();
          await page.getByRole("heading", { name: "Kubernetes", exact: true }).first().click();
          await expect(details).toBeHidden();
          await page.goto("about:blank");
        }
      }
    });
  }

  // A cluster that synced before keeps its data on screen through a failed or
  // degraded sync. The in-page panel must not appear even for a moment while
  // the first reads settle on a cold load: a watcher records every time a
  // `[data-sync-health]` panel enters the page from navigation onward.
  test("a synced cluster with data never flashes the in-page failure panel while loading", async ({ page }) => {
    test.setTimeout(240_000);
    await page.addInitScript(() => {
      const seen: Array<{ at: number; state: string | null; text: string; label: string }> = [];
      (window as Window & { __panelSeen?: typeof seen }).__panelSeen = seen;
      const check = () => {
        const panel = document.querySelector("[data-sync-health]");
        if (panel && (!seen.length || seen[seen.length - 1].state === "gone")) {
          seen.push({ at: Math.round(performance.now()), state: panel.getAttribute("data-sync-health"), text: (panel.querySelector("h2")?.textContent ?? "").trim(), label: document.querySelector("[data-sync-state]")?.textContent ?? "" });
        } else if (!panel && seen.length && seen[seen.length - 1].state !== "gone") {
          seen.push({ at: Math.round(performance.now()), state: "gone", text: "", label: document.querySelector("[data-sync-state]")?.textContent ?? "" });
        }
      };
      new MutationObserver(check).observe(document, { subtree: true, childList: true, attributes: true });
    });
    const flashes: string[] = [];
    // The last case answers the overview without its own `sync` envelope, to
    // tell whether that envelope or the order of the two reads drops the data.
    for (const [sync, overviewWithoutSync] of [["failed-with-data", false], ["degraded-with-data", false], ["failed-long-list", false], ["failed-with-data", true]] as const) {
      for (let attempt = 1; attempt <= (overviewWithoutSync ? 2 : 4); attempt++) {
        await page.unrouteAll({ behavior: "ignoreErrors" });
        const fixture: Fixture = { sync, observed: observedAt(), describeDelayMs: 0, overviewWithoutSync };
        await open(page, viewports[0], "dark", fixture, "/agent/kubernetes");
        await expect(page.getByRole("region", { name: "Cluster health" })).toBeVisible();
        await page.waitForTimeout(1_000);
        const seen = await page.evaluate(() => (window as Window & { __panelSeen?: unknown[] }).__panelSeen ?? []);
        // When the overview and sync reads went out and answered, in page time.
        const reads = await page.evaluate(() => performance.getEntriesByType("resource")
          .filter((entry) => /\/api\/admin\/kubernetes\/(overview|sync)$/.test(new URL(entry.name).pathname))
          .map((entry) => `${new URL(entry.name).pathname.split("/").pop()}@${Math.round(entry.startTime)}-${Math.round((entry as PerformanceResourceTiming).responseEnd)}`));
        if (seen.length) flashes.push(`${sync}${overviewWithoutSync ? " (overview without sync)" : ""} #${attempt}: ${JSON.stringify(seen)} reads=${reads.join(",")}`);
        await page.goto("about:blank");
      }
    }
    test.info().annotations.push({ type: "panel-flashes", description: flashes.length ? flashes.join(" | ") : "none in 14 loads" });
    expect(flashes, "no in-page failure panel over a synced cluster with data").toEqual([]);
  });

  test("a long list of failing kinds scrolls inside the popover, and keyboard reaches Retry", async ({ page }) => {
    test.setTimeout(240_000);
    for (const viewport of viewports) {
      for (const theme of themes) {
        await page.unrouteAll({ behavior: "ignoreErrors" });
        const fixture: Fixture = { sync: "failed-long-list", observed: observedAt(), describeDelayMs: 0 };
        const answers: string[] = [];
        const record = (response: import("@playwright/test").Response) => {
          const path = new URL(response.url()).pathname;
          if (path.startsWith("/api/admin/kubernetes/")) answers.push(`${response.status()} ${path}`);
        };
        page.on("response", record);
        await open(page, viewport, theme, fixture, "/agent/kubernetes");
        const trigger = page.locator('[data-sync-state="failed"] [data-sync-details-trigger]');
        await expect(trigger).toBeVisible();
        // Same cluster state as the two-kind failure, only more kinds: the data
        // stays on screen and the top bar dates it.
        await page.waitForTimeout(1_500);
        const label = await page.locator('[data-sync-state="failed"]').innerText();
        const panelShown = await page.locator("[data-sync-health]").count();
        const healthShown = await page.getByRole("region", { name: "Cluster health" }).count();
        test.info().annotations.push({ type: "long-list-state", description: `${viewport.name}/${theme}: label="${label}" panel=${panelShown} clusterHealth=${healthShown} answers=${[...new Set(answers)].join(", ")}` });
        page.off("response", record);
        await expect.soft(page.locator('[data-sync-state="failed"]'), "a synced cluster with data keeps it on screen and dates it").toHaveText(/^Sync failed — showing data from /);
        await expect.soft(page.locator("[data-sync-health]"), "no in-page failure section while data is on screen").toHaveCount(0);
        await trigger.click();
        const details = page.getByRole("region", { name: "Sync status details" });
        await expect(details).toBeVisible();
        const list = details.getByRole("list", { name: "Resource types that could not sync" });
        await expect(list.locator("li")).toHaveCount(16);
        const scroll = await list.evaluate((el) => ({ scroll: el.scrollHeight, client: el.clientHeight, tabIndex: el.tabIndex }));
        expect(scroll.scroll, "the long list scrolls").toBeGreaterThan(scroll.client);
        expect(scroll.tabIndex, "the scrolling list is keyboard focusable").toBe(0);
        await expectInsideViewport(page, details, "long details popover");
        await expect(details.getByRole("button", { name: "Retry sync" })).toBeInViewport();
        await noHorizontalOverflow(page, "long details popover");
        await settle(page); await page.screenshot({ path: capturePath("sync-status", viewport.name, theme, "failed-long-list-top") });
        await list.evaluate((el) => { el.scrollTop = el.scrollHeight; });
        await expect(list.locator('[data-sync-problem="StatefulSet"]')).toBeInViewport();
        if (theme === "dark") await settle(page); await page.screenshot({ path: capturePath("sync-status", viewport.name, theme, "failed-long-list-scrolled") });
        await page.goto("about:blank");
      }
    }
  });

  for (const sync of ["access-lost", "access-lost-overview-ok"] as const) test(`${sync}: a synced cluster that lost access with no data on screen shows the read failure panel`, async ({ page }) => {
    test.setTimeout(240_000);
    for (const viewport of viewports) {
      for (const theme of themes) {
        await page.unrouteAll({ behavior: "ignoreErrors" });
        const fixture: Fixture = { sync, observed: observedAt(), describeDelayMs: 0 };
        await open(page, viewport, theme, fixture, "/agent/kubernetes");
        const heading = page.getByRole("heading", { name: "Cannot read this cluster right now" });
        await expect(heading).toBeVisible();
        const panel = page.locator('[data-sync-health="failed"]');
        await expect(panel).toContainText("This cluster synced before, but Versus cannot read it now.");
        await expectPillBadge(panel, "Failed");
        await expect(panel.getByRole("button", { name: "Retry sync" })).toBeVisible();
        // Nothing to date: the top bar names only the state.
        await expect(page.locator('[data-sync-state="failed"]')).toHaveText("Sync failed");
        // No endless skeleton: wait past the poll interval and look again.
        await page.waitForTimeout(3_000);
        await expect(page.getByLabel("Loading Kubernetes overview")).toHaveCount(0);
        await expect(heading).toBeVisible();
        await expectContrast(heading, "panel heading");
        await expectContrast(panel.locator("p").first(), "panel body");
        await noHorizontalOverflow(page, "access-lost panel");
        // One failure surface with one Retry: a second overview error card
        // under the panel would repeat the same failure.
        const alerts = await page.getByRole("alert").allInnerTexts();
        test.info().annotations.push({ type: "panel-text", description: `${sync} ${viewport.name}/${theme}: panel="${(await panel.innerText()).replace(/\s+/g, " ")}" alerts=${JSON.stringify(alerts)}` });
        await expect.soft(page.getByText("Couldn't load Kubernetes overview"), "the panel is the only failure surface").toHaveCount(0);
        await expect.soft(page.getByRole("button", { name: /^Retry/ }), "one Retry for one failure").toHaveCount(1);
        await settle(page); await page.screenshot({ path: capturePath("sync-panel", viewport.name, theme, sync), fullPage: false });
        await page.goto("about:blank");
      }
    }
  });

  test("a cluster whose first sync failed shows the connect failure panel", async ({ page }) => {
    test.setTimeout(240_000);
    for (const viewport of viewports) {
      for (const theme of themes) {
        await page.unrouteAll({ behavior: "ignoreErrors" });
        const fixture: Fixture = { sync: "never-synced", observed: observedAt(), describeDelayMs: 0 };
        await open(page, viewport, theme, fixture, "/agent/kubernetes");
        const heading = page.getByRole("heading", { name: "Cannot connect to this cluster" });
        await expect(heading).toBeVisible();
        const panel = page.locator('[data-sync-health="failed"]');
        await expect(panel).toContainText("Versus could not read this cluster yet.");
        await expectPillBadge(panel, "Failed");
        await expect(panel.locator("[data-sync-problem]")).toHaveCount(3);
        await expect(panel.locator('[data-sync-problem="Node"]')).toContainText("permission denied");
        await expect(panel.getByRole("button", { name: "Retry sync" })).toBeVisible();
        await expect(page.getByRole("region", { name: "Cluster health" })).toHaveCount(0);
        await expectContrast(heading, "panel heading");
        await expectContrast(panel.locator('[data-sync-problem="Node"] span').nth(1), "panel reason");
        await noHorizontalOverflow(page, "never-synced panel");
        await settle(page); await page.screenshot({ path: capturePath("sync-panel", viewport.name, theme, "never-synced") });
        await page.goto("about:blank");
      }
    }
  });
});
