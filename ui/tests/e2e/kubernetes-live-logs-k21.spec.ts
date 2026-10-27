// K22 logs-header-and-drawer browser acceptance (contract that replaced K21's
// Follow / chip-row / "Load earlier" header).
//
// Proves S1-S4 at 1440, 1024 and 390 in both themes, plus the K21 checks that
// still hold on the K22 markup: honest first paint, ended and stopped reads,
// the Live dot and reduced motion, Jump to latest only while scrolled up, a
// row Logs paint under 300ms, the drawer Preparing content, zero-crash copy,
// pager rules and the Issues subject (QA-196).
//
// Evidence classes:
//   live    — the real versus + fakekube stack started by
//             tests/kubernetes/k22-visual-seed.mjs.
//   fixture — an in-page SSE stream (quiet/limit/ended/bulk), a route-forced
//             /sync report, a delayed describe, or the controller's exact 403
//             refusal for a caller without infrastructure:view, because fakekube
//             always writes live lines, a real cluster cannot be held in every
//             sync state on demand, and a community gateway session always holds
//             the permission. Fixture captures are labelled in the run summary.
//
// Captures use the shared capturePath helper (HARNESS_IMAGES_DIR). No product
// edit, no /tmp, no one-off Playwright config.
import { expect, test, type APIRequestContext, type Browser, type BrowserContext, type Page } from "@playwright/test";
import * as fs from "node:fs";
import * as path from "node:path";
import { openApp, signInWithGatewaySecret } from "./helpers";
import { capturePath, harnessResultsDir } from "./support/capture";

const cluster = process.env.E2E_K22_CLUSTER || "k22";
const controlURL = process.env.E2E_K22_CONTROL_URL || "";
const longPod = process.env.E2E_K22_LONG_POD || "checkout-orchestrator-canary-rollout-verification-worker-2026-10-26";
const bulkDeployment = process.env.E2E_K22_BULK_DEPLOYMENT || "bulk-app";

const viewports = [
  { name: "desktop", width: 1440, height: 900 },
  { name: "tablet", width: 1024, height: 900 },
  { name: "mobile", width: 390, height: 844 },
] as const;
const themes = ["dark", "light"] as const;
type Theme = (typeof themes)[number];
type Viewport = (typeof viewports)[number];

type SseMode = "quiet" | "limit" | "ended" | "bulk" | "error";

const checksFile = path.join(harnessResultsDir(), "k22-checks.jsonl");
fs.mkdirSync(harnessResultsDir(), { recursive: true });

function record(item: string, viewport: number, theme: string, check: string, pass: boolean, detail: unknown) {
  fs.appendFileSync(checksFile, `${JSON.stringify({ item, viewport, theme, check, pass, detail })}\n`);
}

/** Soft check: records the result and keeps the test running to gather all findings. */
function soft(item: string, viewport: number, theme: string, check: string, pass: boolean, detail: unknown) {
  record(item, viewport, theme, check, pass, detail);
  expect.soft(pass, `${item} / ${check}: ${JSON.stringify(detail)}`).toBe(true);
}

const openContexts: BrowserContext[] = [];
test.afterEach(async () => {
  for (const context of openContexts.splice(0)) await context.close().catch(() => {});
});
test.setTimeout(240_000);

async function newPage(browser: Browser, viewport: Viewport, theme: Theme, fixtures: Record<string, SseMode> = {}): Promise<Page> {
  const context = await browser.newContext({
    viewport: { width: viewport.width, height: viewport.height },
    colorScheme: theme,
  });
  openContexts.push(context);
  const page = await context.newPage();
  await page.emulateMedia({ colorScheme: theme });
  await page.addInitScript((value) => localStorage.setItem("versus.theme", value), theme);
  // Deterministic first-paint states. fakekube always writes live lines, so a
  // quiet, limit or ended container can only be exercised with a controlled
  // SSE body. The patch matches only the named Pod's log stream.
  if (Object.keys(fixtures).length) {
    await page.addInitScript((mapping) => {
      const originalFetch = window.fetch.bind(window);
      window.fetch = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
        const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
        const match = Object.entries(mapping as Record<string, string>).find(([subject]) => url.includes(`/${subject}/logs/stream`));
        if (!match) return originalFetch(input, init);
        const mode = match[1];
        if (mode === "error") return new Response(JSON.stringify({ error: "Log read unavailable in this controlled fixture." }), { status: 403, headers: { "Content-Type": "application/json" } });
        // Counting starts here, not on the network: this fixture answers the
        // stream itself, so a Playwright request listener cannot see it.
        const counter = window as Window & { __k22FixtureStreams?: number };
        counter.__k22FixtureStreams = (counter.__k22FixtureStreams ?? 0) + 1;
        const encoder = new TextEncoder();
        const timers: Array<ReturnType<typeof setTimeout> | ReturnType<typeof setInterval>> = [];
        let closed = false;
        const body = new ReadableStream<Uint8Array>({
          start(controller) {
            const send = (event: string, payload: unknown) => {
              if (closed) return;
              controller.enqueue(encoder.encode(`event: ${event}\ndata: ${JSON.stringify(payload)}\n\n`));
            };
            const heartbeat = () => send("heartbeat", {});
            if (mode === "quiet") {
              heartbeat();
              timers.push(setInterval(heartbeat, 1000));
            } else if (mode === "limit") {
              heartbeat();
              timers.push(setTimeout(() => send("limit", { reason: "bytes", message: "byte safety limit" }), 700));
            } else if (mode === "ended") {
              heartbeat();
              timers.push(setTimeout(() => send("end", { reason: "complete" }), 700));
            } else {
              const now = Date.now();
              for (let index = 0; index < 300; index++) {
                send("line", {
                  text: `fixture scroll line ${String(index).padStart(3, "0")}`,
                  container: "app",
                  timestamp: new Date(now - (300 - index) * 1000).toISOString(),
                  sequence: index + 1,
                  ordinal: index + 1,
                });
              }
              let index = 300;
              timers.push(setInterval(() => {
                index++;
                send("line", {
                  text: `fixture scroll line ${String(index).padStart(3, "0")}`,
                  container: "app",
                  timestamp: new Date().toISOString(),
                  sequence: index,
                  ordinal: index,
                });
              }, 1000));
            }
          },
          cancel() {
            closed = true;
            for (const timer of timers) { clearTimeout(timer); clearInterval(timer); }
          },
        });
        return new Response(body, {
          status: 200,
          headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-store" },
        });
      };
    }, fixtures);
  }
  return page;
}

async function openKubernetes(page: Page, view = "overview") {
  await openApp(page, `/agent/kubernetes?cluster=${encodeURIComponent(cluster)}&view=${view}`);
  await expect(page.getByRole("tablist", { name: "Kubernetes views", exact: true })).toBeVisible({ timeout: 60_000 });
}

async function verifyThemeAndQuietPage(page: Page, theme: Theme) {
  await expect.poll(() => page.locator("html").getAttribute("data-theme")).toBe(theme === "light" ? "light" : null);
  const bodyText = await page.locator("body").innerText();
  expect(bodyText).not.toMatch(/Session expired|Sign in to continue|Unhandled Runtime Error/i);
  expect(bodyText).not.toMatch(/No log lines in the selected time/i);
  expect(bodyText).not.toMatch(/\bundefined\b/);
}

async function saveCapture(page: Page, surface: string, viewport: number, theme: Theme, state: string) {
  await verifyThemeAndQuietPage(page, theme);
  // Acceptance 5: no dot separator in any stat line. Scan the rendered page
  // on every capture, so a stray separator cannot slip through one state.
  const dotLines = (await page.locator("body").innerText())
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line.includes("·"));
  soft("A5", viewport, theme, `no dot separator in ${surface}/${state}`, dotLines.length === 0, dotLines.slice(0, 4));
  const out = capturePath(surface, viewport, theme, state);
  await page.screenshot({ path: out, fullPage: false, animations: "disabled" });
  return out;
}

function viewTab(page: Page, label: string) {
  return page.getByRole("tablist", { name: "Kubernetes views", exact: true }).getByRole("tab", { name: label, exact: true });
}

async function selectView(page: Page, label: string) {
  const tab = viewTab(page, label);
  await tab.click();
  await expect(tab).toHaveAttribute("aria-selected", "true");
  await expect(page.locator("#kubernetes-view-panel")).toBeVisible();
}

function drawer(page: Page) {
  return page.getByRole("dialog", { name: "Details panel" });
}

function fullLogSlot(page: Page) {
  return drawer(page).locator('.resource-log-slot[data-full="true"]');
}

function compactLogSlot(page: Page) {
  return drawer(page).locator('.resource-log-slot[data-full="false"]');
}

function podLogs(page: Page) {
  return fullLogSlot(page).getByRole("region", { name: "Pod logs", exact: true });
}

function workloadLogs(page: Page) {
  return fullLogSlot(page).getByRole("region", { name: "Workload live logs", exact: true });
}

function logOutput(page: Page) {
  return fullLogSlot(page).locator('pre[aria-label="Pod log output"], pre[aria-label="Workload log output"]').first();
}

async function openPodLogs(page: Page, pod: string) {
  await selectView(page, "Nodes");
  const allNodes = page.getByRole("button", { name: "All nodes", exact: true });
  if (await allNodes.isVisible().catch(() => false)) await allNodes.click();
  const nodeButton = page.getByRole("button", { name: "View pods on node-a", exact: true });
  await expect(nodeButton).toBeVisible({ timeout: 30_000 });
  await nodeButton.click();
  const row = page.locator('[role="listitem"]').filter({ has: page.locator(`span[title="${pod}"]`) }).first();
  await expect(row).toBeVisible({ timeout: 30_000 });
  await row.getByRole("button", { name: `Logs for ${pod}`, exact: true }).click();
  await expect(drawer(page)).toBeVisible({ timeout: 10_000 });
  await expect(fullLogSlot(page)).toBeVisible({ timeout: 10_000 });
}

async function openDrawerTab(page: Page, tabId: string, label: string) {
  const panel = drawer(page);
  const direct = panel.locator(`#kubernetes-drawer-tab-${tabId}`);
  if (await direct.isVisible().catch(() => false)) {
    await direct.click();
    return;
  }
  await expect(panel.locator("details.resource-more-tabs > summary")).toBeVisible();
  await panel.locator("details.resource-more-tabs > summary").click();
  await panel.getByRole("menuitem", { name: label, exact: true }).click();
}

async function closeDrawer(page: Page) {
  const panel = drawer(page);
  if (await panel.count()) {
    await panel.getByRole("button", { name: "Close panel", exact: true }).click().catch(() => {});
    await expect(panel).toHaveCount(0, { timeout: 5_000 }).catch(() => {});
  }
}

async function openWorkloadLogs(page: Page, name: string) {
  await selectView(page, "Workloads");
  const search = page.getByRole("textbox", { name: "Resource name", exact: true });
  await search.fill(name);
  const row = page.locator('[role="listitem"]').filter({ hasText: name }).first();
  await expect(row).toBeVisible({ timeout: 30_000 });
  await row.getByRole("button", { name: `Logs for ${name}`, exact: true }).click();
  await expect(drawer(page)).toBeVisible({ timeout: 10_000 });
  await expect(fullLogSlot(page)).toBeVisible({ timeout: 10_000 });
}

function logStatus(page: Page) {
  return fullLogSlot(page).locator(".log-status");
}

async function statusText(page: Page) {
  return (await logStatus(page).innerText()).trim();
}

async function waitStatus(page: Page, value: string, timeout = 20_000) {
  await expect(logStatus(page)).toHaveText(new RegExp(`^${value}$`), { timeout });
}

function sseRequests(page: Page) {
  const seen: Array<{ url: string; at: number }> = [];
  page.on("request", (request) => {
    if (/\/logs\/stream\?/.test(request.url())) seen.push({ url: request.url(), at: Date.now() });
  });
  return seen;
}

type Box = {
  left: number; right: number; top: number; bottom: number;
  width: number; height: number; clientWidth: number; scrollWidth: number;
  display: string; flexWrap: string; whiteSpace: string;
} | null;

/**
 * The S1 measurement the plan asked for before the fix: the header box and the
 * body box, the top row that is actually visible inside the body, and whether
 * the body can escape the panel. The first DOM row is not used because a live
 * read scrolls to the bottom, which legitimately parks earlier rows above the
 * scroll box's viewport.
 */
async function compactGeometry(page: Page) {
  return compactLogSlot(page).locator(".resource-log-viewer").evaluate((viewer) => {
    const box = (element: Element | null) => {
      if (!element) return null;
      const rect = element.getBoundingClientRect();
      return {
        left: rect.left, right: rect.right, top: rect.top, bottom: rect.bottom,
        width: rect.width, height: rect.height,
        clientWidth: (element as HTMLElement).clientWidth, scrollWidth: (element as HTMLElement).scrollWidth,
        display: getComputedStyle(element).display,
        flexWrap: getComputedStyle(element).flexWrap,
        whiteSpace: getComputedStyle(element).whiteSpace,
      };
    };
    const body = viewer.querySelector<HTMLElement>(".log-output");
    const header = viewer.querySelector<HTMLElement>(".log-heading");
    const footer = viewer.querySelector<HTMLElement>(".log-footer");
    const bodyRect = body?.getBoundingClientRect();
    const rows = [...viewer.querySelectorAll<HTMLElement>("[data-log-key]")].map((row) => {
      const rect = row.getBoundingClientRect();
      return { top: rect.top, bottom: rect.bottom, key: row.dataset.logKey ?? "" };
    });
    const visible = bodyRect ? rows.filter((row) => row.bottom > bodyRect.top + 0.5 && row.top < bodyRect.bottom - 0.5) : [];
    return {
      viewer: box(viewer),
      slot: box(viewer.closest(".resource-log-slot")),
      header: box(header),
      body: box(body),
      footer: box(footer),
      topVisible: visible.sort((left, right) => left.top - right.top)[0] ?? null,
      rowCount: rows.length,
      bodyScroll: body ? { clientWidth: body.clientWidth, scrollWidth: body.scrollWidth, clientHeight: body.clientHeight, scrollHeight: body.scrollHeight } : null,
      viewerScroll: { clientWidth: viewer.clientWidth, scrollWidth: viewer.scrollWidth },
      docOverflow: document.documentElement.scrollWidth - window.innerWidth,
    };
  });
}

/** QA-209: the body and the footer must not paint past the compact card. */
async function assertContainment(page: Page, viewport: Viewport, theme: Theme, label: string) {
  const g = await compactGeometry(page);
  const bodyBottom = g.body?.bottom ?? Number.NaN;
  const footerBottom = g.footer?.bottom ?? Number.NaN;
  const cardBottom = g.viewer?.bottom ?? Number.NaN;
  const slotBottom = g.slot?.bottom ?? Number.NaN;
  soft("QA-209", viewport.width, theme, `${label}: body bottom is at or above the card bottom`,
    Number.isFinite(bodyBottom) && bodyBottom <= cardBottom + 0.5,
    { bodyBottom, cardBottom, overflow: bodyBottom - cardBottom });
  soft("QA-209", viewport.width, theme, `${label}: footer bottom is at or above the card bottom`,
    Number.isFinite(footerBottom) && footerBottom <= cardBottom + 0.5,
    { footerBottom, cardBottom, overflow: footerBottom - cardBottom });
  soft("QA-209", viewport.width, theme, `${label}: the card itself stays inside its slot`,
    Number.isFinite(cardBottom) && Number.isFinite(slotBottom) && cardBottom <= slotBottom + 0.5,
    { cardBottom, slotBottom });
  record("QA-209", viewport.width, theme, `${label}: geometry`, true, { bodyBottom, footerBottom, cardBottom, slotBottom });
}

async function assertCompact(page: Page, viewport: Viewport, theme: Theme, label: string) {
  const compact = compactLogSlot(page).locator('.resource-log-viewer[data-compact="true"]');
  await expect(compact).toBeVisible({ timeout: 20_000 });
  await expect(compact.getByText("Live logs", { exact: true })).toBeVisible();
  await expect.poll(async () => compact.locator("[data-log-key]").count(), { timeout: 20_000 }).toBeGreaterThan(10);
  await expect.poll(() => compact.locator(".log-output").evaluate((element) => element.scrollHeight > element.clientHeight), { timeout: 10_000 }).toBe(true);
  // A live read follows the bottom, so the topmost row is legitimately clipped
  // by the scroll box. The contract is about the first line of the read: park
  // the body at its start and measure there.
  await compact.locator(".log-output").evaluate((element) => {
    element.scrollTop = 0;
    element.dispatchEvent(new Event("scroll", { bubbles: true }));
  });
  await page.waitForTimeout(300);
  const g = await compactGeometry(page);
  const headerBottom = g.header?.bottom ?? Number.NaN;
  const bodyTop = g.body?.top ?? Number.NaN;
  const bodyBottom = g.body?.bottom ?? Number.NaN;
  soft("S1", viewport.width, theme, `${label}: header and body do not overlap`, headerBottom <= bodyTop + 0.5, { headerBottom, bodyTop });
  soft("S1", viewport.width, theme, `${label}: first visible line is below the header and fully inside the body`,
    Boolean(g.topVisible) && g.topVisible!.top >= headerBottom - 0.5 && g.topVisible!.top >= bodyTop - 0.5 && g.topVisible!.bottom <= bodyBottom + 0.5,
    { headerBottom, bodyTop, bodyBottom, topVisible: g.topVisible });
  soft("S1", viewport.width, theme, `${label}: body scrolls inside its own box`, Boolean(g.bodyScroll) && g.bodyScroll!.scrollHeight > g.bodyScroll!.clientHeight, g.bodyScroll);
  soft("S1", viewport.width, theme, `${label}: body has no horizontal overflow`, Boolean(g.bodyScroll) && g.bodyScroll!.scrollWidth <= g.bodyScroll!.clientWidth + 1, g.bodyScroll);
  soft("S1", viewport.width, theme, `${label}: panel has no horizontal overflow`, Boolean(g.viewer) && g.viewer!.scrollWidth <= g.viewer!.clientWidth + 1 && g.docOverflow <= 1, { viewerScroll: g.viewerScroll, docOverflow: g.docOverflow });
  record("S1", viewport.width, theme, `${label}: geometry`, true, g);
  await assertContainment(page, viewport, theme, label);
}

// ---------------------------------------------------------------------------
// S2: the Logs tab header, one control row, and the read it drives.
// ---------------------------------------------------------------------------

type ToolbarState = {
  viewer: Box;
  primary: Box;
  secondary: Box;
  primaryChildren: Array<{ cls: string; box: Box }>;
  status: Box;
  container: Box;
  instance: Box;
  current: Box;
  previous: Box;
  since: Box;
  sinceSelect: Box;
  load: Box;
  levels: Box;
  levelFirst: Box;
  filter: Box;
  filterWrap: Box;
  panelLeft: number;
  chips: number;
  follow: number;
  followButtons: number;
  containerOptions: Array<{ value: string; text: string }>;
  docOverflow: number;
};

async function toolbarState(page: Page): Promise<ToolbarState> {
  return fullLogSlot(page).locator(".resource-log-viewer").evaluate((viewer) => {
    const box = (element: Element | null): Box => {
      if (!element) return null;
      const rect = element.getBoundingClientRect();
      const style = getComputedStyle(element);
      return {
        left: rect.left, right: rect.right, top: rect.top, bottom: rect.bottom,
        width: rect.width, height: rect.height,
        clientWidth: (element as HTMLElement).clientWidth, scrollWidth: (element as HTMLElement).scrollWidth,
        display: style.display, flexWrap: style.flexWrap, whiteSpace: style.whiteSpace,
      };
    };
    const query = (selector: string) => box(viewer.querySelector(selector));
    const primary = viewer.querySelector<HTMLElement>(".log-primary");
    const secondary = viewer.querySelector<HTMLElement>(".log-secondary");
    const toolbar = viewer.querySelector<HTMLElement>(".log-toolbar");
    const select = viewer.querySelector<HTMLSelectElement>('select[aria-label="Log container"]');
    const buttons = [...viewer.querySelectorAll("button")];
    // The rule is about the panel's own content edge, not another field. The
    // toolbar carries no padding or offset, so its content box is that edge.
    const viewerRect = viewer.getBoundingClientRect();
    const viewerStyle = getComputedStyle(viewer);
    const contentLeft = toolbar
      ? toolbar.getBoundingClientRect().left + parseFloat(getComputedStyle(toolbar).paddingLeft || "0")
      : viewerRect.left + parseFloat(viewerStyle.borderLeftWidth || "0") + parseFloat(viewerStyle.paddingLeft || "0");
    return {
      viewer: box(viewer),
      primary: box(primary),
      secondary: box(secondary),
      primaryChildren: primary ? [...primary.children].map((child) => ({ cls: child.className, box: box(child) })) : [],
      status: query(".log-status"),
      container: box(select),
      instance: query(".log-instance"),
      current: box([...viewer.querySelectorAll<HTMLButtonElement>(".log-instance button")].find((button) => button.textContent?.trim() === "Current") ?? null),
      previous: box([...viewer.querySelectorAll<HTMLButtonElement>(".log-instance button")].find((button) => button.textContent?.trim() === "Previous") ?? null),
      since: query(".log-since"),
      sinceSelect: query('select[aria-label="Since window"]'),
      load: query(".log-load-earlier"),
      levels: query(".log-levels"),
      levelFirst: box(viewer.querySelector(".log-levels button")),
      filter: query(".log-filter"),
      filterWrap: query(".log-filter-wrap"),
      panelLeft: Math.round(contentLeft * 100) / 100,
      chips: viewer.querySelectorAll(".log-container-chips").length,
      follow: viewer.querySelectorAll(".log-follow").length,
      followButtons: buttons.filter((button) => /^follow$/i.test((button.textContent ?? "").trim())).length,
      containerOptions: select ? [...select.options].map((option) => ({ value: option.value, text: option.text.trim() })) : [],
      docOverflow: document.documentElement.scrollWidth - window.innerWidth,
    };
  });
}

function oneLine(children: Array<{ box: Box }>) {
  const boxes = children.map((child) => child.box).filter((box): box is NonNullable<Box> => Boolean(box));
  if (boxes.length === 0) return false;
  const maxTop = Math.max(...boxes.map((box) => box.top));
  const minBottom = Math.min(...boxes.map((box) => box.bottom));
  return maxTop < minBottom;
}

const left = (box: Box) => (box ? Math.round(box.left * 100) / 100 : null);
const delta = (value: number | null, base: number) => (value === null ? null : Math.round((value - base) * 100) / 100);

// ---------------------------------------------------------------------------
// S3: the drawer overlays; the list geometry must be identical.
// ---------------------------------------------------------------------------

async function listGeometry(page: Page, cardName: string) {
  return page.locator(`section[aria-label="${cardName}"]`).evaluate((card) => {
    const rect = (element: Element | null) => {
      if (!element) return null;
      const r = element.getBoundingClientRect();
      return { left: r.left, right: r.right, top: r.top, bottom: r.bottom, width: r.width, height: r.height };
    };
    const tabRow = card.querySelector<HTMLElement>('[role="tablist"]');
    const aside = document.querySelector<HTMLElement>('[role="dialog"] aside');
    const backdrop = document.querySelector<HTMLButtonElement>('[role="dialog"] button[aria-label="Dismiss details"]');
    const asideStyle = aside ? getComputedStyle(aside) : null;
    const backdropStyle = backdrop ? getComputedStyle(backdrop) : null;
    const pane = document.querySelector<HTMLElement>(".kubernetes-explorer-content");
    const paneStyle = pane ? getComputedStyle(pane) : null;
    return {
      card: rect(card),
      cardScroll: { clientWidth: (card as HTMLElement).clientWidth, scrollWidth: (card as HTMLElement).scrollWidth },
      tabRow: tabRow ? { ...rect(tabRow)!, clientWidth: tabRow.clientWidth, scrollWidth: tabRow.scrollWidth } : null,
      rows: [...card.querySelectorAll<HTMLElement>('[role="listitem"]')].slice(0, 6).map((row) => ({
        width: Math.round(row.getBoundingClientRect().width),
        cells: [...row.querySelectorAll<HTMLElement>(".kubernetes-list-row > *")].map((cell) => ({
          text: (cell.textContent ?? "").trim().slice(0, 30),
          display: getComputedStyle(cell).display,
          width: Math.round(cell.getBoundingClientRect().width * 100) / 100,
        })),
      })),
      pane: pane ? { width: pane.getBoundingClientRect().width, paddingRight: paneStyle?.paddingRight ?? null, scrollWidth: pane.scrollWidth, clientWidth: pane.clientWidth } : null,
      docOverflow: document.documentElement.scrollWidth - window.innerWidth,
      aside: aside ? {
        ...rect(aside)!,
        background: asideStyle?.backgroundColor ?? null,
        opacity: asideStyle?.opacity ?? null,
        shadow: asideStyle?.boxShadow ?? null,
      } : null,
      backdrop: backdrop ? { background: backdropStyle?.backgroundColor ?? null, inset: rect(backdrop) } : null,
    };
  });
}

function listShape(g: Awaited<ReturnType<typeof listGeometry>>) {
  return JSON.stringify({
    card: g.card ? { left: Math.round(g.card.left), width: Math.round(g.card.width) } : null,
    tabRow: g.tabRow ? { clientWidth: g.tabRow.clientWidth, scrollWidth: g.tabRow.scrollWidth } : null,
    cells: g.rows.map((row) => ({ width: row.width, cells: row.cells.map((cell) => ({ display: cell.display, width: cell.width })) })),
  });
}

// ---------------------------------------------------------------------------
// S4: the sync report fixture. The stack's real report supplies the durable
// first_ready_at and the kind rows; the forced state is a labelled fixture.
// ---------------------------------------------------------------------------

type SyncReport = Record<string, unknown> & {
  state?: string;
  age_s?: number;
  partial?: boolean;
  pending_kinds?: number;
  total_kinds?: number;
  ready_kinds?: number;
  failing_kinds?: number;
  kinds?: Record<string, { state?: string; partial?: boolean; error?: string; records?: number; observed_at?: string; skipped?: number }>;
  failing?: Array<{ kind: string; class?: string; since?: string; skipped?: number; records?: number }>;
  unreadable?: unknown[];
  first_ready_at?: string;
};

async function realSyncReport(request: APIRequestContext): Promise<SyncReport> {
  const baseURL = process.env.E2E_BASE_URL || "http://127.0.0.1:8080";
  const response = await request.get(`${baseURL}/api/admin/kubernetes/sync?cluster=${encodeURIComponent(cluster)}`, {
    headers: { "X-Gateway-Secret": process.env.E2E_GATEWAY_SECRET ?? "" },
    timeout: 30_000,
  });
  if (!response.ok()) throw new Error(`sync report HTTP ${response.status()}`);
  return response.json() as Promise<SyncReport>;
}

const minutesAgo = (minutes: number) => new Date(Date.now() - minutes * 60_000).toISOString();

function forcedReport(real: SyncReport, state: "syncing" | "partial" | "stale" | "error"): SyncReport {
  const published = real.first_ready_at ?? new Date(Date.now() - 3 * 60_000).toISOString();
  if (state === "syncing") {
    return {
      ...real,
      state: "syncing",
      age_s: 95,
      partial: false,
      pending_kinds: 3,
      ready_kinds: Math.max(1, (real.total_kinds ?? 6) - 3),
      failing_kinds: 0,
      kinds: {},
      failing: [],
      first_ready_at: published,
    };
  }
  if (state === "partial") {
    return {
      ...real,
      state: "partial",
      age_s: 130,
      partial: true,
      pending_kinds: 0,
      failing_kinds: 1,
      kinds: {
        Deployment: { state: "partial", partial: true, error: "record_rejected", records: 12, observed_at: minutesAgo(2) },
      },
      failing: [{ kind: "Deployment", class: "record_rejected", since: minutesAgo(2), skipped: 2, records: 12 }],
      first_ready_at: published,
    };
  }
  if (state === "stale") {
    return {
      ...real,
      state: "stale",
      age_s: 3600,
      partial: false,
      pending_kinds: 0,
      failing_kinds: 0,
      kinds: {},
      failing: [],
      first_ready_at: published,
    };
  }
  return {
    ...real,
    state: "error",
    age_s: 240,
    partial: true,
    pending_kinds: 0,
    failing_kinds: 1,
    kinds: {
      Node: { state: "error", error: "unavailable", records: 0, observed_at: minutesAgo(4) },
    },
    failing: [{ kind: "Node", class: "unavailable", since: minutesAgo(4), records: 0 }],
    first_ready_at: published,
  };
}

function syncRoute(page: Page, state: { report?: SyncReport }) {
  return page.route((url) => url.pathname.endsWith("/api/admin/kubernetes/sync"), async (route) => {
    if (!state.report) { await route.continue(); return; }
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(state.report) });
  });
}

// ===========================================================================
// S1: compact live logs on a Pod Overview and a Deployment Overview.
// ===========================================================================
for (const viewport of viewports) {
  for (const theme of themes) {
    test(`S1 compact live logs ${viewport.width} ${theme}`, async ({ browser }) => {
      const page = await newPage(browser, viewport, theme);
      await openKubernetes(page, "nodes");
      await openPodLogs(page, "multi-container-pod");
      await openDrawerTab(page, "overview", "Overview");
      await expect(compactLogSlot(page).locator('.resource-log-viewer[data-compact="true"]')).toBeVisible({ timeout: 20_000 });
      await saveCapture(page, "k22-compact", viewport.width, theme, "pod");
      await assertCompact(page, viewport, theme, "Pod Overview");
      // The founder's break was at the panel's bottom edge, so capture the card
      // with the drawer scrolled to it as well as the initial paint.
      await drawer(page).locator(".resource-tabpanel").evaluate((panel) => { panel.scrollTop = panel.scrollHeight; });
      await page.waitForTimeout(300);
      await assertContainment(page, viewport, theme, "Pod Overview scrolled to the card bottom");
      await saveCapture(page, "k22-compact", viewport.width, theme, "pod-bottom");
      await closeDrawer(page);

      await openWorkloadLogs(page, bulkDeployment);
      await openDrawerTab(page, "overview", "Overview");
      await expect(compactLogSlot(page).locator('.resource-log-viewer[data-compact="true"]')).toBeVisible({ timeout: 20_000 });
      await saveCapture(page, "k22-compact", viewport.width, theme, "deployment");
      await assertCompact(page, viewport, theme, "Deployment Overview");
      await drawer(page).locator(".resource-tabpanel").evaluate((panel) => { panel.scrollTop = panel.scrollHeight; });
      await page.waitForTimeout(300);
      await assertContainment(page, viewport, theme, "Deployment Overview scrolled to the card bottom");
      await saveCapture(page, "k22-compact", viewport.width, theme, "deployment-bottom");
    });
  }
}

// ===========================================================================
// S2: row 1 is one line, the container is a select, Current|Previous is one
// segmented control, Follow and the chip row are gone, Since+Load prepends.
// ===========================================================================
for (const viewport of viewports) {
  for (const theme of themes) {
    test(`S2 logs header ${viewport.width} ${theme}`, async ({ browser }) => {
      const page = await newPage(browser, viewport, theme);
      const streams = sseRequests(page);
      await openKubernetes(page, "nodes");
      await openPodLogs(page, "multi-container-pod");
      const logs = podLogs(page);
      await waitStatus(page, "Live");
      // PeekPanel slides in over 200ms; measure the settled toolbar.
      await page.waitForTimeout(500);

      const g = await toolbarState(page);
      // QA-208, revised rule: no control is graded against another field. The
      // panel's content edge, row 1's first control and row 2's first control
      // share one x. Per-control offsets are recorded as evidence only.
      const panelLeft = g.panelLeft;
      const filterLeft = left(g.filter);
      const statusLeft = left(g.status);
      const levelLeft = left(g.levelFirst);
      const row1Delta = delta(statusLeft, panelLeft);
      const row2Delta = delta(levelLeft, panelLeft);
      const rowsDelta = delta(levelLeft, statusLeft ?? 0);
      const primaryDelta = delta(left(g.primary), panelLeft);
      const secondaryDelta = delta(left(g.secondary), panelLeft);
      const units = { status: g.status, container: g.container, instance: g.instance, since: g.since };
      const deltas = Object.fromEntries(Object.entries(units).map(([name, box]) => [name, delta(left(box), panelLeft)]));
      const segmentDeltas = { current: delta(left(g.current), panelLeft), previous: delta(left(g.previous), panelLeft) };
      const loadDelta = delta(left(g.load), panelLeft);

      soft("S2", viewport.width, theme, "row 1 is one visual line at 1440/1024", viewport.width === 390 ? true : oneLine(g.primaryChildren), { primary: g.primary, children: g.primaryChildren });
      soft("S2", viewport.width, theme, "row 2 is one visual line at 1440/1024", viewport.width === 390 ? true : Boolean(g.levels && g.filter && g.levels.top < g.filter.bottom && g.filter.top < g.levels.bottom), { levels: g.levels, filter: g.filter });
      const order = g.primaryChildren
        .map((child) => ["log-status", "log-container", "log-instance", "log-since"].find((name) => child.cls.split(/\s+/).includes(name)))
        .join(",");
      soft("S2", viewport.width, theme, "row 1 order is status, container, Current|Previous, Since+Load",
        order === "log-status,log-container,log-instance,log-since", { order, classes: g.primaryChildren.map((child) => child.cls) });
      soft("S2", viewport.width, theme, "container is a select with one option per container",
        g.containerOptions.length === 2 && g.containerOptions.some((option) => option.text === "app") && g.containerOptions.some((option) => option.text === "metrics-sidecar"),
        g.containerOptions);
      soft("S2", viewport.width, theme, "Current|Previous is exactly one segmented control with two segments",
        Boolean(g.instance) && (await logs.locator(".log-instance").count()) === 1 && (await logs.locator(".log-instance button").count()) === 2,
        { groups: await logs.locator(".log-instance").count(), buttons: await logs.locator(".log-instance button").count() });
      soft("S2", viewport.width, theme, "no Follow control and no chip row in the full panel",
        g.chips === 0 && g.follow === 0 && g.followButtons === 0
          && (await drawer(page).getByRole("button", { name: "Follow", exact: true }).count()) === 0,
        { chips: g.chips, follow: g.follow, followButtons: g.followButtons });

      record("S2", viewport.width, theme, "segments", true, segmentDeltas);
      record("S2", viewport.width, theme, "alignment", true, {
        panelLeft, filterLeft, statusLeft, levelLeft, row1Delta, row2Delta, rowsDelta, primaryDelta, secondaryDelta,
        deltas, segmentDeltas, loadDelta,
      });
      soft("QA-208", viewport.width, theme, "row 1's first control (the Live pill) is flush to the panel content edge",
        row1Delta !== null && Math.abs(row1Delta) <= 1, { panelLeft, statusLeft, row1Delta });
      soft("QA-208", viewport.width, theme, "row 2's first control (the first level pill) is flush to the panel content edge",
        row2Delta !== null && Math.abs(row2Delta) <= 1, { panelLeft, levelLeft, row2Delta });
      soft("QA-208", viewport.width, theme, "both rows share one left edge (0px delta)",
        rowsDelta !== null && Math.abs(rowsDelta) <= 1, { statusLeft, levelLeft, rowsDelta });
      soft("QA-208", viewport.width, theme, "neither row container is indented from the panel content edge",
        primaryDelta !== null && Math.abs(primaryDelta) <= 1 && secondaryDelta !== null && Math.abs(secondaryDelta) <= 1,
        { primaryDelta, secondaryDelta, panelLeft });
      await saveCapture(page, "k22-logs-header", viewport.width, theme, "row1");

      // The container select drives the read.
      const containerSelect = logs.locator('select[aria-label="Log container"]');
      await containerSelect.selectOption("metrics-sidecar");
      await expect(logOutput(page)).toContainText("container=metrics-sidecar", { timeout: 20_000 });
      soft("S2", viewport.width, theme, "switching the container reissues the stream for it",
        streams.some((entry) => /container=metrics-sidecar/.test(entry.url)), streams.slice(-2).map((entry) => entry.url.slice(-120)));
      await saveCapture(page, "k22-logs-header", viewport.width, theme, "container-switched");

      // Previous switches the instance and shows the previous container read.
      await logs.getByRole("button", { name: "Previous container logs", exact: true }).click();
      await waitStatus(page, "Previous");
      await expect(logOutput(page)).toContainText(/fakekube previous .*container=metrics-sidecar/, { timeout: 20_000 });
      soft("S2", viewport.width, theme, "Previous asks the server for the previous instance",
        streams.some((entry) => /previous=true/.test(entry.url) && /container=metrics-sidecar/.test(entry.url)), streams.slice(-2).map((entry) => entry.url.slice(-140)));
      await saveCapture(page, "k22-logs-header", viewport.width, theme, "previous");
      await logs.getByRole("button", { name: "Current", exact: true }).click();
      await waitStatus(page, "Live");
      await expect(logOutput(page)).not.toContainText("fakekube previous", { timeout: 20_000 });
      await closeDrawer(page);

      // Workload read: no chip row, a pod selector is allowed, and Since+Load
      // prepends one bounded history read without restarting the live read.
      const earlierRequests: string[] = [];
      page.on("request", (request) => {
        if (/\/workloads\/[^/]+\/[^/]+\/logs\?/.test(request.url())) earlierRequests.push(request.url());
      });
      await openWorkloadLogs(page, bulkDeployment);
      const workload = workloadLogs(page);
      await waitStatus(page, "Live");
      await page.waitForTimeout(500);
      const wg = await toolbarState(page);
      soft("S2", viewport.width, theme, "workload row 1 is one visual line at 1440/1024", viewport.width === 390 ? true : oneLine(wg.primaryChildren), wg.primaryChildren);
      soft("QA-210", viewport.width, theme, "workload container select lists All containers first",
        wg.containerOptions[0]?.text === "All containers" && wg.containerOptions[0]?.value === "*", wg.containerOptions);
      soft("S2", viewport.width, theme, "workload read has no chip row or Follow", wg.chips === 0 && wg.follow === 0 && wg.followButtons === 0, { chips: wg.chips, follow: wg.follow });
      await saveCapture(page, "k22-logs-header", viewport.width, theme, "workload-row1");

      const output = logOutput(page);
      await expect(output.locator("[data-log-key]").first()).toBeVisible({ timeout: 20_000 });
      await expect.poll(async () => output.locator("[data-log-key]").count(), { timeout: 20_000 }).toBeGreaterThan(50);
      const before = await output.locator("[data-log-key]").count();
      const streamCountBefore = streams.length;
      const load = workload.getByRole("button", { name: "Load logs since that window", exact: true });
      await load.click();
      await expect(load).not.toHaveText("Loading", { timeout: 20_000 });
      await expect.poll(async () => output.locator("[data-log-key]").count(), { timeout: 25_000 }).toBeGreaterThan(before + 50);
      const after = await output.locator("[data-log-key]").count();
      const requestURL = earlierRequests.at(-1) ?? "";
      const tail = Number(new URL(requestURL, "http://x").searchParams.get("tail_lines") ?? "0");
      await page.waitForTimeout(1_500);
      soft("S2", viewport.width, theme, "Since+Load prepends lines", after > before + 50, { before, after });
      soft("S2", viewport.width, theme, "Since+Load is one bounded read", tail > 0 && tail <= 1000, { tail, requestURL: requestURL.replace(/^https?:\/\/[^/]+/, "").slice(0, 260) });
      soft("S2", viewport.width, theme, "Since+Load does not restart the live read", streams.length === streamCountBefore, { before: streamCountBefore, after: streams.length });
      await saveCapture(page, "k22-logs-header", viewport.width, theme, "load");
    });
  }
}

// ===========================================================================
// S3: opening the drawer overlays; the list is untouched and opaque-covered.
// ===========================================================================
for (const viewport of viewports) {
  for (const theme of themes) {
    test(`S3 drawer overlay ${viewport.width} ${theme}`, async ({ browser }) => {
      const page = await newPage(browser, viewport, theme);
      await openKubernetes(page, "resources");
      await selectView(page, "Workloads");
      const workloads = page.getByRole("region", { name: "Workloads", exact: true });
      await expect.poll(async () => workloads.locator('[role="listitem"]').count(), { timeout: 45_000 }).toBeGreaterThan(5);
      await page.waitForTimeout(500);

      const closed = await listGeometry(page, "Workloads");
      await saveCapture(page, "k22-drawer", viewport.width, theme, "workloads-closed");
      const logButton = page.getByRole("button", { name: `Logs for ${bulkDeployment}`, exact: true });
      await expect(logButton).toBeVisible({ timeout: 30_000 });
      await logButton.click();
      await expect(drawer(page)).toBeVisible({ timeout: 10_000 });
      await page.waitForTimeout(600);
      const open = await listGeometry(page, "Workloads");
      await saveCapture(page, "k22-drawer", viewport.width, theme, "workloads-open");

      soft("S3", viewport.width, theme, "Workloads list geometry is identical while the drawer is open",
        listShape(closed) === listShape(open), { closed: listShape(closed), open: listShape(open) });
      soft("S3", viewport.width, theme, "Workloads tab row has no horizontal scrollbar",
        Boolean(open.tabRow) && open.tabRow!.scrollWidth <= open.tabRow!.clientWidth + 1 && Boolean(closed.tabRow) && closed.tabRow!.scrollWidth <= closed.tabRow!.clientWidth + 1,
        { open: open.tabRow, closed: closed.tabRow });
      soft("S3", viewport.width, theme, "no Workloads column folds under the drawer",
        open.rows.length > 0 && open.rows.every((row) => row.cells.filter((cell) => cell.display !== "none" && cell.text !== "").length >= 3),
        open.rows.map((row) => row.cells));
      soft("S3", viewport.width, theme, "drawer surface is opaque with a shadow",
        Boolean(open.aside) && open.aside!.opacity === "1" && /^rgb\(/.test(open.aside!.background ?? "") && (open.aside!.shadow ?? "none") !== "none",
        open.aside);
      soft("S3", viewport.width, theme, "a dimming backdrop covers the list",
        Boolean(open.backdrop) && /rgba\([^)]*,\s*0?\.[0-9]+\)/.test(open.backdrop!.background ?? ""),
        open.backdrop);
      soft("S3", viewport.width, theme, "drawer opening adds no horizontal overflow",
        open.docOverflow <= 1 && Boolean(open.cardScroll) && open.cardScroll!.scrollWidth <= open.cardScroll!.clientWidth + 1,
        { docOverflow: open.docOverflow, cardScroll: open.cardScroll });

      await closeDrawer(page);
      await page.waitForTimeout(500);
      const reclosed = await listGeometry(page, "Workloads");
      soft("S3", viewport.width, theme, "closing restores the identical Workloads layout",
        listShape(reclosed) === listShape(closed), { closed: listShape(closed), reclosed: listShape(reclosed) });
      await saveCapture(page, "k22-drawer", viewport.width, theme, "workloads-closed-again");

      // Node Pods, same contract.
      await selectView(page, "Nodes");
      const allNodes = page.getByRole("button", { name: "All nodes", exact: true });
      if (await allNodes.isVisible().catch(() => false)) await allNodes.click();
      const nodeButton = page.getByRole("button", { name: "View pods on node-a", exact: true });
      await expect(nodeButton).toBeVisible({ timeout: 30_000 });
      await nodeButton.click();
      const nodes = page.getByRole("region", { name: "Nodes", exact: true });
      await expect.poll(async () => nodes.locator('[role="listitem"]').count(), { timeout: 30_000 }).toBeGreaterThan(2);
      await page.waitForTimeout(400);
      const nodeClosed = await listGeometry(page, "Nodes");
      await saveCapture(page, "k22-drawer", viewport.width, theme, "nodes-closed");
      await nodes.locator('[role="listitem"]').filter({ hasText: "multi-container-pod" }).first()
        .getByRole("button", { name: "Logs for multi-container-pod", exact: true }).click();
      await expect(drawer(page)).toBeVisible({ timeout: 10_000 });
      await page.waitForTimeout(600);
      const nodeOpen = await listGeometry(page, "Nodes");
      await saveCapture(page, "k22-drawer", viewport.width, theme, "nodes-open");
      soft("S3", viewport.width, theme, "Node Pods geometry is identical while the drawer is open",
        listShape(nodeClosed) === listShape(nodeOpen), { closed: listShape(nodeClosed), open: listShape(nodeOpen) });
      soft("S3", viewport.width, theme, "Node Pods tab row has no horizontal scrollbar",
        !nodeOpen.tabRow || nodeOpen.tabRow!.scrollWidth <= nodeOpen.tabRow!.clientWidth + 1,
        nodeOpen.tabRow);
      soft("S3", viewport.width, theme, "drawer surface stays opaque over Node Pods",
        Boolean(nodeOpen.aside) && nodeOpen.aside!.opacity === "1" && /^rgb\(/.test(nodeOpen.aside!.background ?? ""),
        nodeOpen.aside);
      await closeDrawer(page);
      await page.waitForTimeout(400);
      const nodeReclosed = await listGeometry(page, "Nodes");
      soft("S3", viewport.width, theme, "closing restores the identical Node Pods layout",
        listShape(nodeReclosed) === listShape(nodeClosed), { closed: listShape(nodeClosed), reclosed: listShape(nodeReclosed) });
    });
  }
}

// ===========================================================================
// Preparing belongs to first publication; routine refresh uses the top bar.
// ===========================================================================
for (const viewport of viewports) {
  for (const theme of themes) {
    test(`S4 sync states ${viewport.width} ${theme}`, async ({ browser, request }) => {
      const real = await realSyncReport(request);
      expect(real.first_ready_at, "the stack must have published before the strip proof").toBeTruthy();
      const forced: { report?: SyncReport } = {};
      const page = await newPage(browser, viewport, theme);
      await syncRoute(page, forced);

      // Never published: Preparing with progress, unchanged.
      forced.report = {
        ...forcedReport(real, "syncing"),
        state: "syncing",
        age_s: 0,
        pending_kinds: 4,
        total_kinds: 12,
        ready_kinds: 8,
        first_ready_at: "0001-01-01T00:00:00Z",
      };
      // A snapshot taken before the route exists: fulfilling from a live
      // route.fetch() races the page's own refetch and can dispose the body.
      const baseURL = process.env.E2E_BASE_URL || "http://127.0.0.1:8080";
      const overviewResponse = await request.get(`${baseURL}/api/admin/kubernetes/overview?cluster=${encodeURIComponent(cluster)}`, {
        headers: { "X-Gateway-Secret": process.env.E2E_GATEWAY_SECRET ?? "" },
        timeout: 30_000,
      });
      expect(overviewResponse.ok(), `overview snapshot HTTP ${overviewResponse.status()}`).toBe(true);
      const overviewSnapshot = await overviewResponse.json();
      const preparingOverview = {
        ...overviewSnapshot,
        sync: {
          state: "syncing",
          age_s: 0,
          partial: false,
          progress: {
            step: "listing",
            step_index: 2,
            step_total: 4,
            steps_done: ["connected"],
            counts: { Pod: 12, Deployment: 4 },
            kinds_done: 12,
            kinds_total: 20,
            cluster_version: "v1.31.0",
            node_count: 2,
            started_at: minutesAgo(1),
            updated_at: new Date().toISOString(),
          },
        },
      };
      let detailedProgress = true;
      await page.route((url) => url.pathname.endsWith("/api/admin/kubernetes/overview"), (route) =>
        route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ ...preparingOverview, sync: { ...preparingOverview.sync, progress: detailedProgress ? preparingOverview.sync.progress : undefined } }) }));
      await openKubernetes(page, "overview");
      await expect(page.locator("[data-preparing-panel]")).toBeVisible({ timeout: 60_000 });
      soft("S4", viewport.width, theme, "never-published cluster shows Preparing", (await page.locator("body").innerText()).includes("Preparing") || (await page.locator("[data-preparing-panel]").isVisible()), "preparing");
      soft("S4", viewport.width, theme, "Preparing shows progress", await page.getByRole("progressbar", { name: "Listing resource types: 12 / 20", exact: true }).isVisible(), "progressbar");
      soft("S4", viewport.width, theme, "never-published cluster has no data view", (await page.getByRole("region", { name: "Workloads", exact: true }).count()) === 0, "workloads card");
      await saveCapture(page, "k22-sync", viewport.width, theme, "preparing");
      await closeDrawer(page);

      await expect(page.locator("[data-preparing-panel] [data-step]")).toHaveCount(4);
      await expect(page.locator("[data-preparing-panel] [role=progressbar]")).toHaveCount(1);
      const listingBar = page.getByRole("progressbar", { name: "Listing resource types: 12 / 20", exact: true });
      await expect(listingBar).toHaveAttribute("aria-valuenow", "12");
      await expect(listingBar).toHaveAttribute("aria-valuemax", "20");
      await expect(page.locator("[data-preparing-panel]")).not.toContainText(/\d+\s*%|ETA|usually under|estimated/i);
      const positions = await page.locator("[data-preparing-panel]").evaluate((panel) => ({
        rowsBottom: Math.max(...Array.from(panel.querySelectorAll("[data-step]")).map((row) => row.getBoundingClientRect().bottom)),
        barTop: panel.querySelector("[role=progressbar]")!.getBoundingClientRect().top,
        overflow: panel.scrollWidth > panel.clientWidth,
      }));
      expect(positions.barTop).toBeGreaterThanOrEqual(positions.rowsBottom);
      expect(positions.overflow).toBe(false);
      detailedProgress = false;
      await page.reload();
      await expect(page.locator("[data-preparing-panel] [data-state=unknown]")).toHaveCount(4);
      const unknownBar = page.getByRole("progressbar", { name: "Waiting for preparation progress", exact: true });
      await expect(unknownBar).toBeVisible();
      await expect(unknownBar).not.toHaveAttribute("aria-valuenow");
      await expect(page.locator("[data-preparing-panel] [data-state=done], [data-preparing-panel] [data-state=active]")).toHaveCount(0);
      await expect(page.locator("[data-preparing-panel]")).not.toContainText(/\d+\s*%|ETA|usually under|estimated/i);
      await page.emulateMedia({ reducedMotion: "reduce" });
      expect(await page.locator("[data-preparing-panel] [data-preparing-motion]").evaluateAll((nodes) => nodes.every((node) => getComputedStyle(node).display === "none" || getComputedStyle(node).animationName === "none"))).toBe(true);
      await expect(page.locator("[data-preparing-panel] [aria-live], [data-preparing-panel] [role=status]")).toHaveCount(0);
      await expect(page.locator("[data-preparing-panel] [role=progressbar]")).toHaveCount(1);
      await saveCapture(page, "k22-sync", viewport.width, theme, "unknown-progress");

      // Published failures retain their warning; routine syncing stays quiet.
      const states = [
        { state: "syncing" as const, strip: "preparing", headline: "Cluster sync running" },
        { state: "partial" as const, strip: "degraded", headline: "Cluster sync degraded" },
        { state: "stale" as const, strip: "degraded", headline: "Cluster sync degraded" },
        { state: "error" as const, strip: "failed", headline: "Cluster sync failed" },
      ];
      for (const item of states) {
        forced.report = forcedReport(real, item.state);
        await openKubernetes(page, "resources");
        await selectView(page, "Workloads");
        const rows = page.getByRole("region", { name: "Workloads", exact: true }).locator('[role="listitem"]');
        await expect.poll(async () => rows.count(), { timeout: 45_000 }).toBeGreaterThan(0);
        if (item.state === "syncing") {
          await expect(page.locator("[data-sync-state]")).toHaveText("Syncing");
          await expect(page.locator("[data-sync-strip], [data-preparing-panel]")).toHaveCount(0);
          await saveCapture(page, "k22-sync", viewport.width, theme, "silent-syncing");
          continue;
        }
        const strip = page.locator(`[data-sync-strip="${item.strip}"]`);
        await expect(strip).toBeVisible({ timeout: 20_000 });
        const stripText = (await strip.innerText()).replace(/\s+/g, " ");
        soft("S4", viewport.width, theme, `${item.state}: Preparing never renders`, (await page.locator("[data-sync-health]").count()) === 0 && !(await page.locator("body").innerText()).includes("Preparing the cluster"), stripText.slice(0, 160));
        soft("S4", viewport.width, theme, `${item.state}: strip names the state`, stripText.includes(item.headline), stripText.slice(0, 160));
        soft("S4", viewport.width, theme, `${item.state}: strip carries Retry`, (await strip.locator("[data-sync-retry]").count()) === 1, "retry");
        soft("S4", viewport.width, theme, `${item.state}: data age is visible`, (await strip.locator("[data-sync-age]").count()) === 1 && /Showing data from \d/.test(stripText), stripText.slice(0, 200));
        soft("S4", viewport.width, theme, `${item.state}: the view keeps its data`, (await rows.count()) > 0, await rows.count());
        if (item.state === "partial" || item.state === "error") {
          soft("S4", viewport.width, theme, `${item.state}: the strip names the failing kind`, (await strip.locator("[data-sync-problem]").count()) >= 1, stripText.slice(0, 240));
        }
        await saveCapture(page, "k22-sync", viewport.width, theme, item.state);
      }

      // Reload mid-resync: data and the existing indicator, never Preparing.
      forced.report = forcedReport(real, "syncing");
      await watchPreparationFlashes(page);
      await page.reload();
      await expect(page.getByRole("tablist", { name: "Kubernetes views", exact: true })).toBeVisible({ timeout: 60_000 });
      await selectView(page, "Workloads");
      await expect.poll(async () => page.getByRole("region", { name: "Workloads", exact: true }).locator('[role="listitem"]').count(), { timeout: 45_000 }).toBeGreaterThan(0);
      await expect(page.locator("[data-sync-state]")).toHaveText("Syncing", { timeout: 20_000 });
      await expect(page.locator("[data-sync-strip], [data-preparing-panel]")).toHaveCount(0);
      soft("S4", viewport.width, theme, "reload mid-resync shows data with no extra loading surface",
        (await page.locator("[data-sync-health]").count()) === 0 && !(await page.locator("body").innerText()).includes("Preparing the cluster"),
        "reload");
      expect(await page.evaluate(() => sessionStorage.getItem("unexpected-preparation"))).toBeNull();
      await saveCapture(page, "k22-sync", viewport.width, theme, "reload-mid-resync");

      // Retry still reaches the server.
      const retry = page.getByRole("button", { name: "Refresh Kubernetes data", exact: true });
      const syncCalls = { count: 0 };
      page.on("request", (request) => { if (request.url().includes("/api/admin/kubernetes/sync")) syncCalls.count++; });
      const before = syncCalls.count;
      await retry.focus();
      await expect(retry).toBeFocused();
      await page.keyboard.press("Enter");
      await expect.poll(() => syncCalls.count, { timeout: 15_000 }).toBeGreaterThan(before);
      soft("S4", viewport.width, theme, "Retry re-reads the sync report", syncCalls.count > before, { before, after: syncCalls.count });
    });
  }
}

// Observe transient gates as well as the settled screen across real reloads.
async function watchPreparationFlashes(page: Page) {
  await page.addInitScript(() => {
    const watch = () => {
      if (document.querySelector("[data-preparing-panel]")) sessionStorage.setItem("unexpected-preparation", "true");
    };
    new MutationObserver(watch).observe(document, { childList: true, subtree: true });
  });
}

for (const viewport of viewports.filter((entry) => entry.width !== 1024)) {
  for (const theme of themes) {
    test(`Published restart warming snapshot recovers ${viewport.width} ${theme}`, async ({ browser, request }) => {
      const before = await realSyncReport(request);
      expect(before.first_ready_at).toBeTruthy();
      const page = await newPage(browser, viewport, theme);
      await watchPreparationFlashes(page);
      await openKubernetes(page, "resources");
      const renewal = await page.request.post(`${controlURL}/renew-credential`, { timeout: 120_000 });
      expect(renewal.ok()).toBe(true);
      expect((await renewal.json()).first_ready_at).toBe(before.first_ready_at);
      const restart = await page.request.post(`${controlURL}/restart`, { timeout: 120_000 });
      expect(restart.ok()).toBe(true);
      expect((await restart.json()).first_ready_at).toBe(before.first_ready_at);

      // A controlled empty warming envelope over a real restarted process/store.
      // Only the empty-to-ready timing is a route fixture, not persistence.
      let warming = true;
      let emptyReads = 0;
      let restoredReads = 0;
      await page.route((url) => url.pathname.endsWith("/api/admin/kubernetes/sync"), async (route) => {
        await page.waitForTimeout(350);
        await route.fulfill({ json: warming ? forcedReport(before, "syncing") : { ...before, state: "ready", pending_kinds: 0, failing_kinds: 0 } });
      });
      await page.route((url) => url.pathname.endsWith("/api/admin/kubernetes/workloads"), async (route) => {
        if (warming) {
          emptyReads++;
          await route.fulfill({ json: { items: [], truncated: false, sync: { state: "syncing", age_s: 0, partial: false } } });
        } else {
          restoredReads++;
          await route.continue();
        }
      });
      await page.reload();
      await selectView(page, "Workloads");
      await expect(page.getByText("No workloads in this scope.", { exact: true })).toBeVisible();
      expect(emptyReads).toBeGreaterThan(0);
      await expect(page.locator("[data-sync-state]")).toHaveText("Syncing");
      await expect(page.locator("[data-preparing-panel], [data-sync-strip]")).toHaveCount(0);
      await saveCapture(page, "k22-warming", viewport.width, theme, "empty-after-restart");
      // Do not forge readiness before the restarted worker actually publishes.
      await expect.poll(async () => (await realSyncReport(request)).state, { timeout: 60_000 }).toBe("ready");
      warming = false;
      // No Refresh click or navigation: report readiness must invalidate the
      // scoped empty snapshot itself, while the selection stays Workloads.
      await expect.poll(() => restoredReads, { timeout: 45_000 }).toBeGreaterThan(0);
      const rows = page.getByRole("region", { name: "Workloads", exact: true }).locator('[role="listitem"]');
      await expect.poll(() => rows.count(), { timeout: 45_000 }).toBeGreaterThan(0);
      await expect(viewTab(page, "Workloads")).toHaveAttribute("aria-selected", "true");
      await expect(page.locator("[data-preparing-panel], [data-sync-strip]")).toHaveCount(0);
      expect(await page.evaluate(() => sessionStorage.getItem("unexpected-preparation"))).toBeNull();
      expect((await realSyncReport(request)).first_ready_at).toBe(before.first_ready_at);
      await saveCapture(page, "k22-warming", viewport.width, theme, "recovered-without-refresh");
    });

    test(`Independent cluster publication classifications ${viewport.width} ${theme}`, async ({ browser, request }) => {
      const real = await realSyncReport(request);
      expect(real.first_ready_at).toBeTruthy();
      const page = await newPage(browser, viewport, theme);
      const fresh = "new-cluster";
      const summary = (id: string) => ({ id, display_name: id, provider: "Kubernetes", version: "v1.31", health: "syncing", nodes: 2, ready_nodes: 2, pods: 12, running_pods: 12, warnings: 0, observed_at: new Date().toISOString(), sync: { state: "syncing", age_s: 0, partial: false } });
      await page.route((url) => url.pathname.endsWith("/api/admin/kubernetes/clusters"), (route) => route.fulfill({ json: { multiple: true, clusters: [summary(cluster), summary(fresh)] } }));
      await page.route((url) => url.pathname.endsWith("/api/admin/kubernetes/sync"), (route) => route.fulfill({ json: { ...forcedReport(real, "syncing"), first_ready_at: new URL(route.request().url()).searchParams.get("cluster") === fresh ? undefined : real.first_ready_at } }));
      await openApp(page, "/agent/kubernetes");
      const publishedRow = page.getByRole("article", { name: `Cluster ${cluster}`, exact: true });
      const freshRow = page.getByRole("article", { name: `Cluster ${fresh}`, exact: true });
      await expect(publishedRow.locator("[data-sync-state-label]")).toHaveText("Syncing");
      await expect(publishedRow.locator("[data-first-sync]")).toHaveCount(0);
      await expect(freshRow.locator("[data-sync-state-label]")).toHaveText("Preparing");
      await expect(freshRow.locator("[data-first-sync]")).toHaveCount(1);
      await verifyThemeAndQuietPage(page, theme);
      await page.screenshot({ path: capturePath("k22-isolation", viewport.width, theme, "fleet"), animations: "disabled" });
      await openKubernetes(page, "resources");
      await selectView(page, "Workloads");
      await expect.poll(() => page.getByRole("region", { name: "Workloads", exact: true }).locator('[role="listitem"]').count(), { timeout: 45_000 }).toBeGreaterThan(0);
      await expect(page.locator("[data-sync-state]")).toHaveText("Syncing");
      await expect(page.locator("[data-preparing-panel], [data-sync-strip]")).toHaveCount(0);
      await saveCapture(page, "k22-isolation", viewport.width, theme, "published-usable");
      // This fixture proves independent presentation, not authorization for a
      // server-unknown cluster. The real server must still refuse that cluster.
      await openApp(page, "/agent/kubernetes");
      await expect(publishedRow.locator("[data-sync-state-label]")).toHaveText("Syncing");
      await expect(freshRow.locator("[data-sync-state-label]")).toHaveText("Preparing");
      await expect(publishedRow.locator("[data-first-sync]")).toHaveCount(0);
      await expect(freshRow.locator("[data-first-sync]")).toHaveCount(1);
    });
  }
}

// ===========================================================================
// S4 durability: first_ready_at survives a versus restart.
// ===========================================================================
test("S4 first_ready_at survives a versus restart", async ({ browser, request }) => {
  test.setTimeout(240_000);
  const viewport = viewports[0];
  const theme: Theme = "dark";
  const page = await newPage(browser, viewport, theme);
  await openKubernetes(page, "resources");
  const before = await realSyncReport(request);
  expect(before.first_ready_at, "first_ready_at before restart").toBeTruthy();
  if (!controlURL) throw new Error("E2E_K22_CONTROL_URL is required for the restart proof");
  const renewal = await page.request.post(`${controlURL}/renew-credential`, { timeout: 120_000 });
  expect(renewal.ok()).toBe(true);
  const renewed = await renewal.json() as { first_ready_at?: string };
  expect(renewed.first_ready_at).toBe(before.first_ready_at);
  const restart = await page.request.post(`${controlURL}/restart`, { timeout: 120_000 });
  expect(restart.ok(), `restart HTTP ${restart.status()}`).toBe(true);
  const after = await restart.json() as { restarted?: boolean; first_ready_at?: string | null };
  soft("S4", viewport.width, theme, "first_ready_at survives the versus restart", Boolean(after.restarted) && after.first_ready_at === before.first_ready_at, { before: before.first_ready_at, after });
  await openKubernetes(page, "resources");
  await selectView(page, "Workloads");
  await expect.poll(async () => page.getByRole("region", { name: "Workloads", exact: true }).locator('[role="listitem"]').count(), { timeout: 60_000 }).toBeGreaterThan(0);
  soft("S4", viewport.width, theme, "Preparing does not come back after the restart",
    (await page.locator("[data-sync-health]").count()) === 0 && !(await page.locator("body").innerText()).includes("Preparing the cluster"),
    "after restart");
  await saveCapture(page, "k22-sync", viewport.width, theme, "after-restart");
});

// ===========================================================================
// F2: a background resync keeps the rows and the Related tab on screen.
// The per-view `syncing` envelope is a labelled fixture layered over the real
// rows; first_ready_at is the stack's real value, which is the condition the
// founder complained about.
// ===========================================================================
for (const viewport of viewports) {
  for (const theme of themes) {
    test(`F2 background resync ${viewport.width} ${theme}`, async ({ browser }) => {
      test.setTimeout(180_000);
      const page = await newPage(browser, viewport, theme);
      await openKubernetes(page, "resources");
      await selectView(page, "Workloads");
      const rows = page.getByRole("region", { name: "Workloads", exact: true }).locator('[role="listitem"]');
      await expect.poll(async () => rows.count(), { timeout: 45_000 }).toBeGreaterThan(5);
      const rowsBefore = await rows.count();

      // Related has real content before anything is forced.
      await openWorkloadLogs(page, bulkDeployment);
      await openDrawerTab(page, "related", "Related");
      const related = drawer(page).locator('section[aria-label="Related resources"]');
      await expect(related).toBeVisible({ timeout: 30_000 });
      await expect.poll(async () => (await related.innerText()).trim().length, { timeout: 30_000 }).toBeGreaterThan(20);
      const relatedBefore = (await related.innerText()).replace(/\s+/g, " ").trim();
      // The drawer's backdrop covers the top bar, so close it before pressing
      // the page's own Refresh control.
      await closeDrawer(page);

      let forcedWorkloads = 0;
      let forcedRelated = 0;
      await page.route((url) => url.pathname === "/api/admin/kubernetes/workloads", async (route) => {
        const response = await route.fetch();
        const body = await response.json() as Record<string, unknown>;
        forcedWorkloads++;
        await route.fulfill({ response, json: { ...body, sync: { state: "syncing", age_s: 2, partial: false, pending: ["Pod"] } } });
      });
      await page.route((url) => url.pathname === "/api/admin/kubernetes/graph/related", async (route) => {
        const response = await route.fetch();
        const body = await response.json() as Record<string, unknown>;
        forcedRelated++;
        await route.fulfill({ response, json: { ...body, sync: { state: "syncing", age_s: 2, partial: false } } });
      });

      await page.getByRole("button", { name: "Refresh Kubernetes data", exact: true }).click();
      await expect.poll(() => forcedWorkloads, { timeout: 20_000 }).toBeGreaterThan(0);
      await page.waitForTimeout(800);
      const rowsAfter = await rows.count();
      const skeletons = await page.getByRole("region", { name: "Workloads", exact: true }).locator(".sk").count();
      soft("F2", viewport.width, theme, "Workloads rows stay during a forced per-view syncing",
        rowsAfter > 0 && rowsAfter >= Math.min(5, rowsBefore) && skeletons === 0,
        { rowsBefore, rowsAfter, skeletons });
      soft("F2", viewport.width, theme, "no Preparing panel replaces the Workloads view",
        (await page.locator("[data-sync-health]").count()) === 0 && !(await page.locator("body").innerText()).includes("Preparing the cluster"),
        "preparing");
      await saveCapture(page, "k22-resync", viewport.width, theme, "workloads-syncing");

      // Re-open Related: the same rows are already in hand and must come back
      // unchanged, with no skeleton while the view settles.
      await openWorkloadLogs(page, bulkDeployment);
      await openDrawerTab(page, "related", "Related");
      const relatedAgain = drawer(page).locator('section[aria-label="Related resources"]');
      await expect(relatedAgain).toBeVisible({ timeout: 20_000 });
      await expect.poll(async () => (await relatedAgain.innerText()).trim().length, { timeout: 20_000 }).toBeGreaterThan(20);
      const relatedAfter = (await relatedAgain.innerText()).replace(/\s+/g, " ").trim();
      soft("F2", viewport.width, theme, "Related keeps its content during the forced syncing",
        relatedAfter.length > 20 && relatedAfter === relatedBefore,
        { before: relatedBefore.slice(0, 200), after: relatedAfter.slice(0, 200) });
      soft("F2", viewport.width, theme, "Related shows no loading skeleton or Preparing panel",
        (await relatedAgain.locator('[aria-label="Loading related resources"]').count()) === 0
          && (await relatedAgain.locator(".sk").count()) === 0
          && (await relatedAgain.locator("[data-sync-health]").count()) === 0,
        "related");
      await saveCapture(page, "k22-resync", viewport.width, theme, "related-syncing");
      await closeDrawer(page);

      // A fresh related query with the per-view envelope forced to syncing:
      // first_ready_at is set, so the tab renders the related content and never
      // a skeleton or the Preparing panel.
      await openWorkloadLogs(page, "long-container-app");
      await openDrawerTab(page, "related", "Related");
      await expect.poll(() => forcedRelated, { timeout: 20_000 }).toBeGreaterThan(0);
      const freshRelated = drawer(page).locator('section[aria-label="Related resources"]');
      await expect(freshRelated).toBeVisible({ timeout: 20_000 });
      await expect.poll(async () => (await freshRelated.innerText()).trim().length, { timeout: 20_000 }).toBeGreaterThan(20);
      soft("F2", viewport.width, theme, "a fresh Related read with a syncing envelope still renders its content",
        (await freshRelated.locator('[aria-label="Loading related resources"]').count()) === 0
          && (await freshRelated.locator(".sk").count()) === 0
          && (await freshRelated.locator("[data-sync-health]").count()) === 0,
        "fresh related");
      await saveCapture(page, "k22-resync", viewport.width, theme, "related-fresh-syncing");
    });
  }
}

// ===========================================================================
// F3: returning to the newest line by scrolling resumes following, with no
// Jump press and no lost tail.
// ===========================================================================
for (const viewport of viewports) {
  for (const theme of themes) {
    test(`F3 scroll down resumes follow ${viewport.width} ${theme}`, async ({ browser }) => {
      test.setTimeout(120_000);
      const page = await newPage(browser, viewport, theme, { "scroll-pod": "bulk" });
      await openKubernetes(page, "nodes");
      await openPodLogs(page, "scroll-pod");
      const logs = podLogs(page);
      const output = logOutput(page);
      await waitStatus(page, "Live");
      await expect.poll(async () => output.locator("[data-log-key]").count(), { timeout: 20_000 }).toBeGreaterThanOrEqual(300);
      await output.evaluate((element) => { element.scrollTop = element.scrollHeight; element.dispatchEvent(new Event("scroll", { bubbles: true })); });
      await page.waitForTimeout(300);
      soft("F3", viewport.width, theme, "at the newest line there is no Jump", await logs.getByRole("button", { name: "Jump to latest", exact: true }).count() === 0, "count");

      await output.hover();
      await page.mouse.wheel(0, -6000);
      await expect(logs.getByRole("button", { name: "Jump to latest", exact: true })).toBeVisible({ timeout: 10_000 });
      soft("F3", viewport.width, theme, "scrolling away from the newest line offers Jump", true, "count");
      await saveCapture(page, "k22-follow", viewport.width, theme, "scrolled-up");

      await output.hover();
      await page.mouse.wheel(0, 60000);
      await expect(logs.getByRole("button", { name: "Jump to latest", exact: true })).toHaveCount(0, { timeout: 10_000 });
      const gap = await output.evaluate((element) => element.scrollHeight - element.scrollTop - element.clientHeight);
      soft("F3", viewport.width, theme, "scrolling down to the newest line resumes following without a click",
        await logs.getByRole("button", { name: "Jump to latest", exact: true }).count() === 0 && gap <= 48,
        { gap });
      const lastKey = () => output.evaluate((element) => {
        const lines = [...element.querySelectorAll<HTMLElement>("[data-log-key]")];
        return lines.at(-1)?.dataset.logKey ?? "";
      });
      const beforeKey = await lastKey();
      await page.waitForTimeout(2_500);
      const afterKey = await lastKey();
      const settledGap = await output.evaluate((element) => element.scrollHeight - element.scrollTop - element.clientHeight);
      soft("F3", viewport.width, theme, "the resumed read keeps following new lines",
        afterKey !== beforeKey && settledGap <= 48 && await logs.getByRole("button", { name: "Jump to latest", exact: true }).count() === 0,
        { beforeKey, afterKey, settledGap });
      await saveCapture(page, "k22-follow", viewport.width, theme, "resumed");
    });
  }
}

// ===========================================================================
// F4: a rollout removes the selected container; the select moves to one that
// still exists and the read follows it.
// ===========================================================================
for (const viewport of viewports) {
  for (const theme of themes) {
    test(`F4 rollout container fallback ${viewport.width} ${theme}`, async ({ browser, request }) => {
      test.setTimeout(180_000);
      if (!controlURL) throw new Error("E2E_K22_CONTROL_URL is required for the rollout fixture");
      const ready = await request.post(`${controlURL}/rollout/two`, { timeout: 30_000 });
      expect(ready.ok(), `rollout/two HTTP ${ready.status()}`).toBe(true);
      const page = await newPage(browser, viewport, theme);
      const streams = sseRequests(page);
      await openKubernetes(page, "resources");
      await openWorkloadLogs(page, "rollout-app");
      const workload = workloadLogs(page);
      await waitStatus(page, "Live");
      const select = workload.locator('select[aria-label="Log container"]');
      await expect.poll(async () => select.locator("option").evaluateAll((options) => options.map((option) => (option as HTMLOptionElement).value)), { timeout: 30_000 })
        .toContain("metrics-sidecar");
      await page.waitForTimeout(500);

      await select.selectOption("metrics-sidecar");
      await expect(logOutput(page)).toContainText("metrics-sidecar", { timeout: 20_000 });
      soft("F4", viewport.width, theme, "the selected container is the one being read",
        streams.some((entry) => /container=metrics-sidecar/.test(entry.url)), streams.slice(-2).map((entry) => entry.url.slice(-120)));
      await saveCapture(page, "k22-rollout", viewport.width, theme, "selected-sidecar");

      const breakAt = Date.now();
      const removed = await request.post(`${controlURL}/rollout/one`, { timeout: 30_000 });
      expect(removed.ok(), `rollout/one HTTP ${removed.status()}`).toBe(true);
      await expect.poll(async () => select.inputValue(), { timeout: 30_000 }).not.toBe("metrics-sidecar");
      const selected = await select.inputValue();
      const options = await select.locator("option").evaluateAll((items) => items.map((item) => (item as HTMLOptionElement).value));
      soft("F4", viewport.width, theme, "the select moves to a container that still exists",
        options.includes(selected) && selected !== "metrics-sidecar", { selected, options });
      await expect.poll(() => streams.some((entry) => entry.at >= breakAt && !/container=metrics-sidecar/.test(entry.url)), { timeout: 20_000 }).toBe(true);
      soft("F4", viewport.width, theme, "the live read follows the container that remains",
        streams.some((entry) => entry.at >= breakAt && !/container=metrics-sidecar/.test(entry.url)) && (await statusText(page)) === "Live",
        streams.filter((entry) => entry.at >= breakAt).slice(-2).map((entry) => entry.url.slice(-140)));
      await saveCapture(page, "k22-rollout", viewport.width, theme, "fallback-container");
    });
  }
}

// ===========================================================================
// F5: a safety-limit-stopped compact read can start a new read from the card.
// ===========================================================================
for (const viewport of viewports) {
  for (const theme of themes) {
    test(`F5 compact limited reload ${viewport.width} ${theme}`, async ({ browser }) => {
      test.setTimeout(120_000);
      const page = await newPage(browser, viewport, theme, { "limit-pod": "limit" });
      const fixtureStreams = () => page.evaluate(() => (window as Window & { __k22FixtureStreams?: number }).__k22FixtureStreams ?? 0);
      await openKubernetes(page, "nodes");
      await openPodLogs(page, "limit-pod");
      await openDrawerTab(page, "overview", "Overview");
      const compact = compactLogSlot(page).locator('.resource-log-viewer[data-compact="true"]');
      await expect(compact).toBeVisible({ timeout: 20_000 });
      await expect.poll(async () => (await compact.locator(".log-status").innerText()).trim(), { timeout: 20_000 }).toBe("Stopped");
      const reload = compact.getByRole("button", { name: "Reload logs", exact: true });
      await expect(reload).toBeVisible();
      soft("F5", viewport.width, theme, "a limited compact read offers Reload in the card", await reload.isVisible(), "reload");
      await saveCapture(page, "k22-compact", viewport.width, theme, "limited");
      const before = await fixtureStreams();
      await reload.click();
      await expect.poll(fixtureStreams, { timeout: 20_000 }).toBeGreaterThan(before);
      soft("F5", viewport.width, theme, "Reload starts a new read", (await fixtureStreams()) > before, { before, after: await fixtureStreams() });
      await expect.poll(async () => (await compact.locator(".log-status").innerText()).trim(), { timeout: 20_000 }).toBe("Stopped");
      await saveCapture(page, "k22-compact", viewport.width, theme, "limited-reloaded");
    });
  }
}

// ===========================================================================
// F6: no dot separator anywhere, including the events and crashes counts.
// ===========================================================================
for (const viewport of viewports) {
  for (const theme of themes) {
    test(`F6 no dot separator ${viewport.width} ${theme}`, async ({ browser }) => {
      test.setTimeout(120_000);
      const page = await newPage(browser, viewport, theme);
      await openKubernetes(page, "nodes");
      await openPodLogs(page, longPod);
      const scans: Array<{ surface: string; text: string }> = [];
      const scan = async (surface: string) => scans.push({ surface, text: await drawer(page).innerText() });
      await scan("logs");
      await openDrawerTab(page, "events", "Events");
      await expect(drawer(page).locator('section[aria-label="Object-scoped events"]')).toBeVisible({ timeout: 20_000 });
      await scan("events");
      await saveCapture(page, "k22-no-dot", viewport.width, theme, "events");
      await openDrawerTab(page, "timeline", "Timeline");
      await expect(drawer(page).locator('section[aria-label="Resource change history"]')).toBeVisible({ timeout: 30_000 });
      await page.waitForTimeout(800);
      await scan("timeline");
      await saveCapture(page, "k22-no-dot", viewport.width, theme, "timeline");
      await openDrawerTab(page, "overview", "Overview");
      await scan("overview");
      const dotted = scans.filter((entry) => /[·•]/.test(entry.text)).map((entry) => entry.surface);
      soft("F6", viewport.width, theme, "no dot separator in logs, events, timeline or overview", dotted.length === 0, dotted);
      const timelineText = (scans.find((entry) => entry.surface === "timeline")?.text ?? "").replace(/\s+/g, " ");
      const countLine = timelineText.match(/(\d+)\s+events?\s+and\s+(\d+)\s+crash(?:es)?/);
      soft("F6", viewport.width, theme, "the events and crashes count is separated by words",
        Boolean(countLine) || !/\d+\s*[·•]\s*\d+/.test(timelineText), { countLine: countLine?.[0] ?? null, sample: timelineText.slice(0, 240) });
    });
  }
}

// ===========================================================================
// Still-valid K21 checks on the K22 markup.
// ===========================================================================
for (const viewport of [viewports[0], viewports[2]] as const) {
  for (const theme of themes) {
    test(`K21 first paint ${viewport.width} ${theme}`, async ({ browser }) => {
      const page = await newPage(browser, viewport, theme, { "quiet-pod": "quiet" });
      await openKubernetes(page, "nodes");
      await openPodLogs(page, "quiet-pod");
      const logs = podLogs(page);
      await expect(logs.getByText("Waiting for the first log line…", { exact: true })).toBeVisible({ timeout: 8_000 });
      await waitStatus(page, "Live");
      const waitingText = await logs.innerText();
      soft("A2", viewport.width, theme, "first paint says waiting", waitingText.includes("Waiting for the first log line…"), waitingText.slice(0, 200));
      soft("A2", viewport.width, theme, "never says selected time", !waitingText.includes("No log lines in the selected time"), waitingText.slice(0, 200));
      soft("QA-207", viewport.width, theme, "quiet sentence absent before the heartbeat window", !waitingText.includes("No new log lines yet. The container is quiet."), waitingText.slice(0, 200));
      await saveCapture(page, "k21-logs-first-paint", viewport.width, theme, "waiting");
      await expect(logs.getByText("No new log lines yet. The container is quiet.", { exact: true })).toBeVisible({ timeout: 15_000 });
      soft("QA-207", viewport.width, theme, "quiet sentence on a live heartbeat-only read", (await statusText(page)) === "Live", await statusText(page));
      await saveCapture(page, "k21-logs-first-paint", viewport.width, theme, "quiet");
    });

    test(`K21 ended and stopped ${viewport.width} ${theme}`, async ({ browser }) => {
      const page = await newPage(browser, viewport, theme, { "ended-pod": "ended", "limit-pod": "limit" });
      await openKubernetes(page, "nodes");
      await openPodLogs(page, "ended-pod");
      await waitStatus(page, "Ended");
      const endedOutput = await logOutput(page).innerText();
      soft("QA-207", viewport.width, theme, "ended empty state uses ended wording", /ended/i.test(endedOutput) && /No further lines will arrive/i.test(endedOutput), endedOutput.slice(0, 240));
      soft("QA-207", viewport.width, theme, "ended empty state is not the quiet sentence", !/container is quiet/i.test(endedOutput), endedOutput.slice(0, 240));
      soft("A4", viewport.width, theme, "ended Pod has no Follow", await drawer(page).getByRole("button", { name: "Follow", exact: true }).count() === 0, await statusText(page));
      soft("A4", viewport.width, theme, "ended Pod is not Unavailable", (await statusText(page)) !== "Unavailable", await statusText(page));
      await saveCapture(page, "k21-logs-ended", viewport.width, theme, "ended");
      await closeDrawer(page);

      await openPodLogs(page, "limit-pod");
      await waitStatus(page, "Stopped");
      soft("A4", viewport.width, theme, "limit-stopped reads Stopped", (await statusText(page)) === "Stopped", await statusText(page));
      soft("A4", viewport.width, theme, "limit-stopped is not Ended", (await statusText(page)) !== "Ended", await statusText(page));
      soft("A4", viewport.width, theme, "limit-stopped has no Follow", await drawer(page).getByRole("button", { name: "Follow", exact: true }).count() === 0, await statusText(page));
      await saveCapture(page, "k21-logs-ended", viewport.width, theme, "stopped");
    });

    test(`K21 live dot and jump ${viewport.width} ${theme}`, async ({ browser }) => {
      const page = await newPage(browser, viewport, theme, { "scroll-pod": "bulk" });
      await openKubernetes(page, "nodes");
      await openPodLogs(page, "checkout-api-0");
      const logs = podLogs(page);
      await waitStatus(page, "Live");
      const dot = logs.locator('.log-status[data-log-status="is-live"] .log-dot');
      await expect(dot).toBeVisible();
      const moving = await dot.evaluate((element) => {
        const style = getComputedStyle(element);
        return { name: style.animationName, duration: style.animationDuration };
      });
      soft("A5", viewport.width, theme, "Live dot animates", moving.name === "log-dot-pulse", moving);
      await saveCapture(page, "k21-logs-live-dot", viewport.width, theme, "animated");
      await page.emulateMedia({ reducedMotion: "reduce", colorScheme: theme });
      const still = await dot.evaluate((element) => {
        const style = getComputedStyle(element);
        return { name: style.animationName, duration: style.animationDuration };
      });
      soft("A5", viewport.width, theme, "reduced motion stops the dot", still.name === "none", still);
      await saveCapture(page, "k21-logs-live-dot", viewport.width, theme, "reduced-motion");
      await page.emulateMedia({ reducedMotion: "no-preference", colorScheme: theme });
      soft("A5", viewport.width, theme, "no retained/evicted stats", !/\bretained\b|\bevicted\b/i.test(await fullLogSlot(page).innerText()), "stats");
      await closeDrawer(page);

      await openPodLogs(page, "scroll-pod");
      const scrolling = podLogs(page);
      const output = logOutput(page);
      await expect.poll(async () => output.locator("[data-log-key]").count(), { timeout: 15_000 }).toBeGreaterThanOrEqual(300);
      soft("A5", viewport.width, theme, "Jump absent at bottom", await scrolling.getByRole("button", { name: "Jump to latest", exact: true }).count() === 0, "count");
      await output.evaluate((element) => { element.scrollTop = 0; element.dispatchEvent(new Event("scroll", { bubbles: true })); });
      await expect(scrolling.getByRole("button", { name: "Jump to latest", exact: true })).toBeVisible({ timeout: 5_000 });
      await scrolling.locator('summary[aria-label="More log actions"]').click();
      const menuLatest = scrolling.getByRole("button", { name: "Scroll to latest", exact: true });
      soft("A5", viewport.width, theme, "menu Scroll to latest appears when scrolled up", await menuLatest.count() === 1, "count");
      await saveCapture(page, "k21-logs-jump", viewport.width, theme, "scrolled-up");
      await scrolling.getByRole("button", { name: "Jump to latest", exact: true }).click();
      await expect(scrolling.getByRole("button", { name: "Jump to latest", exact: true })).toHaveCount(0, { timeout: 5_000 });
      soft("A5", viewport.width, theme, "menu Scroll to latest gone at bottom", await scrolling.getByRole("button", { name: "Scroll to latest", exact: true }).count() === 0, "count");
      await saveCapture(page, "k21-logs-jump", viewport.width, theme, "at-bottom");
    });

    test(`K21 row logs under 300ms ${viewport.width} ${theme}`, async ({ browser }) => {
      const page = await newPage(browser, viewport, theme);
      await openKubernetes(page, "resources");
      let describeStarted = false;
      let describeFinished = false;
      await page.route((url) => url.pathname.endsWith("/describe"), async (route) => {
        describeStarted = true;
        await new Promise((resolve) => setTimeout(resolve, 5_000));
        describeFinished = true;
        await route.continue().catch(() => {});
      });
      const search = page.getByRole("textbox", { name: "Resource name", exact: true });
      await search.fill("checkout-api");
      const row = page.locator('[role="listitem"]').filter({ hasText: "checkout-api" }).first();
      await expect(row).toBeVisible({ timeout: 30_000 });
      const startedAt = Date.now();
      await row.getByRole("button", { name: "Logs for checkout-api", exact: true }).click();
      await expect(fullLogSlot(page)).toBeVisible({ timeout: 5_000 });
      const elapsed = Date.now() - startedAt;
      soft("A7", viewport.width, theme, "Logs tab paints under 300ms", elapsed < 300, { elapsed, describeStarted, describeFinished });
      soft("A7", viewport.width, theme, "Logs paint before describe resolves", !describeFinished, { elapsed, describeStarted, describeFinished });
      await expect(page.locator("[data-resource-preparing]")).toBeVisible({ timeout: 3_000 });
      const preparing = await page.locator("[data-resource-preparing]").innerText();
      soft("A7", viewport.width, theme, "Preparing names the resource", preparing.includes("checkout-api"), preparing.slice(0, 200));
      soft("A7", viewport.width, theme, "Preparing names the kind", preparing.includes("Deployment"), preparing.slice(0, 200));
      soft("A7", viewport.width, theme, "Preparing is not a bare skeleton", await drawer(page).locator(".sk").count() === 0, "skeletons");
      await saveCapture(page, "k21-detail-preparing", viewport.width, theme, "logs-open");
      await expect.poll(() => describeFinished, { timeout: 15_000 }).toBe(true);
      await expect(page.locator("[data-resource-preparing]")).toHaveCount(0, { timeout: 10_000 });
      await closeDrawer(page);

      await selectView(page, "Workloads");
      for (const [name, kind] of [["settlement", "Job"], ["edge-agent", "DaemonSet"]] as const) {
        await search.fill(name);
        const kindRow = page.locator('[role="listitem"]').filter({ hasText: name }).first();
        await expect(kindRow).toBeVisible({ timeout: 20_000 });
        const actions = await kindRow.locator('[data-row-action="logs"]').count();
        soft("A7", viewport.width, theme, `${kind} row has no Logs action`, actions === 0, actions);
      }
      await search.fill(bulkDeployment);
      const streamable = page.locator('[role="listitem"]').filter({ hasText: bulkDeployment }).first();
      await expect(streamable).toBeVisible({ timeout: 20_000 });
      soft("A7", viewport.width, theme, "Deployment row has a Logs action", await streamable.locator('[data-row-action="logs"]').count() === 1, "count");
      await saveCapture(page, "k21-workloads-actions", viewport.width, theme, "non-stream-kinds");
    });

    test(`K21 zero crashes ${viewport.width} ${theme}`, async ({ browser }) => {
      const page = await newPage(browser, viewport, theme);
      await openKubernetes(page, "nodes");
      await openPodLogs(page, "long-container-pod");
      await drawer(page).locator("#kubernetes-drawer-tab-overview").click();
      await expect(drawer(page).locator(".resource-overview")).toBeVisible({ timeout: 30_000 });
      const panels = await drawer(page).innerText();
      soft("A8", viewport.width, theme, "no crash section at zero crashes", !/captured crashes/i.test(panels), panels.slice(0, 200));
      soft("A8", viewport.width, theme, "no crash pager on a single page", await drawer(page).getByRole("navigation", { name: "Captured crash pages", exact: true }).count() === 0, "count");
      await saveCapture(page, "k21-detail-crashes", viewport.width, theme, "zero");
    });

    test(`QA-196 issue subject ${viewport.width} ${theme}`, async ({ browser }) => {
      const page = await newPage(browser, viewport, theme);
      await openKubernetes(page, "issues");
      const namespace = page.getByRole("combobox", { name: "Issue namespace", exact: true });
      await namespace.click();
      await namespace.fill("payments");
      await page.keyboard.press("Enter");
      await expect(namespace).toHaveValue("payments");
      const subject = page.locator("[data-issue-subject]").filter({ hasText: longPod }).first();
      await expect(subject).toBeVisible({ timeout: 45_000 });
      const geometry = await subject.evaluate((element) => {
        const style = getComputedStyle(element);
        const rect = element.getBoundingClientRect();
        const lineHeight = parseFloat(style.lineHeight || "0");
        return {
          text: element.textContent, title: element.getAttribute("title"), overflow: style.textOverflow,
          whiteSpace: style.whiteSpace, clientWidth: element.clientWidth, scrollWidth: element.scrollWidth,
          height: rect.height, lineHeight, right: rect.right, viewport: window.innerWidth,
          docOverflow: document.documentElement.scrollWidth - window.innerWidth,
        };
      });
      soft("QA-196", viewport.width, theme, "subject shows the full name in title", geometry.title === `payments/${longPod}`, geometry.title);
      soft("QA-196", viewport.width, theme, "subject is one nowrap ellipsis line", geometry.overflow === "ellipsis" && geometry.whiteSpace === "nowrap" && geometry.height <= Math.max(geometry.lineHeight * 1.6, 20), geometry);
      if (viewport.width === 390) {
        soft("QA-196", viewport.width, theme, "subject is clipped, not wrapped mid-word", geometry.scrollWidth > geometry.clientWidth, { clientWidth: geometry.clientWidth, scrollWidth: geometry.scrollWidth });
      }
      soft("QA-196", viewport.width, theme, "subject stays inside the viewport", geometry.right <= geometry.viewport + 1 && geometry.docOverflow <= 1, { right: geometry.right, viewport: geometry.viewport, docOverflow: geometry.docOverflow });
      await saveCapture(page, "k21-issues-subject", viewport.width, theme, "long-subject");
    });
  }
}

for (const width of [1440, 390] as const) {
  test(`QA-206 pager ${width} light`, async ({ browser }) => {
    const viewport = viewports.find((item) => item.width === width)!;
    const theme: Theme = "light";
    const page = await newPage(browser, viewport, theme);
    await openKubernetes(page, "resources");
    await selectView(page, "Workloads");
    const rows = page.getByRole("region", { name: "Workloads", exact: true }).locator('[role="listitem"]');
    await expect.poll(async () => rows.count(), { timeout: 45_000 }).toBeGreaterThan(1);
    const pager = page.getByRole("navigation", { name: "Page navigation", exact: true });
    await expect(pager).toBeVisible({ timeout: 10_000 });
    const next = page.getByRole("button", { name: "Next page", exact: true });
    soft("QA-206", width, theme, "multi-page list keeps a pager", await pager.isVisible(), true);
    soft("QA-206", width, theme, "multi-page pager can move forward", await next.isEnabled(), await next.isDisabled());
    await saveCapture(page, "k21-pager", width, theme, "multi-page");
    await next.click();
    await expect(pager.getByText("Page 2", { exact: true })).toBeVisible({ timeout: 20_000 });
    soft("QA-206", width, theme, "second page renders", (await rows.count()) > 0, await rows.count());
    await pager.getByRole("button", { name: "Previous page", exact: true }).click();
    await expect(pager.getByText("Page 1", { exact: true })).toBeVisible({ timeout: 20_000 });

    const search = page.getByRole("textbox", { name: "Resource name", exact: true });
    await search.fill("bulk-app");
    await expect.poll(async () => rows.count(), { timeout: 30_000 }).toBeGreaterThan(0);
    await expect.poll(async () => pager.count(), { timeout: 30_000 }).toBe(0);
    soft("QA-206", width, theme, "single-page Workloads list has no pager", await pager.count() === 0, { rows: await rows.count() });
    await saveCapture(page, "k21-pager", width, theme, "single-page");
  });
}

// ===========================================================================
// F1: the real revoked-credential path. No /sync route fixture: fakekube
// revokes the token at the API server (every call answers 401) while the token
// file stays readable, so the connector still resolves and the server itself
// answers 200 with state=error and the durable first_ready_at. Declared last.
// ===========================================================================
for (const viewport of viewports) {
  for (const theme of themes) {
    test(`F1 revoked credential ${viewport.width} ${theme}`, async ({ browser, request }) => {
      test.setTimeout(300_000);
      if (!controlURL) throw new Error("E2E_K22_CONTROL_URL is required for the credential proof");
      const baseURL = process.env.E2E_BASE_URL || "http://127.0.0.1:8080";
      const headers = { "X-Gateway-Secret": process.env.E2E_GATEWAY_SECRET ?? "" };
      const syncURL = `${baseURL}/api/admin/kubernetes/sync?cluster=${encodeURIComponent(cluster)}`;

      // Restore the credential file and wait for the next healthy cycle, so
      // every rectangle starts from a published cluster.
      const restored = await request.post(`${controlURL}/restore-credential`, { timeout: 60_000 });
      expect(restored.ok(), `restore-credential HTTP ${restored.status()}`).toBe(true);
      await expect.poll(async () => (await request.get(`${controlURL}/sync`, { timeout: 20_000 })).json().then((body) => body.state), { timeout: 120_000 }).toBe("ready");
      const before = await realSyncReport(request);
      expect(before.first_ready_at, "the cluster must have published once").toBeTruthy();

      const page = await newPage(browser, viewport, theme);
      await openKubernetes(page, "resources");
      await selectView(page, "Workloads");
      const rows = page.getByRole("region", { name: "Workloads", exact: true }).locator('[role="listitem"]');
      await expect.poll(async () => rows.count(), { timeout: 60_000 }).toBeGreaterThan(5);
      await saveCapture(page, "k22-credential", viewport.width, theme, "before-break");

      const broken = await request.post(`${controlURL}/break-credential`, { timeout: 30_000 });
      expect(broken.ok(), `break-credential HTTP ${broken.status()}`).toBe(true);
      // The server's own report after the credential is gone. This is the
      // durable answer the UI must render, not a route fixture.
      await expect.poll(async () => (await request.get(syncURL, { headers, timeout: 20_000 })).json().then((body) => body.state), { timeout: 60_000 }).toBe("error");
      const liveSync = await request.get(syncURL, { headers, timeout: 30_000 });
      const liveBody = await liveSync.json() as { state?: string; first_ready_at?: string };
      soft("F1", viewport.width, theme, "the server answers 200 with state error and first_ready_at",
        liveSync.status() === 200 && liveBody.state === "error" && liveBody.first_ready_at === before.first_ready_at,
        { status: liveSync.status(), state: liveBody.state, first_ready_at: liveBody.first_ready_at, before: before.first_ready_at });

      // Wait until the stored snapshot is old enough to be a real measurement:
      // the strip only labels an age it can stand behind, and a value under five
      // seconds is deliberately left unsaid.
      await expect.poll(async () => (await request.get(syncURL, { headers, timeout: 20_000 })).json().then((body) => body.age_s), { timeout: 60_000 }).toBeGreaterThanOrEqual(6);

      // The live page asks the server again through its own Refresh control.
      await page.getByRole("button", { name: "Refresh Kubernetes data", exact: true }).click();
      await page.waitForTimeout(1_500);
      const strip = page.locator('[data-sync-strip="failed"]');
      const indicator = page.locator('[data-sync-state="failed"]');
      const stripVisible = await strip.isVisible().catch(() => false);
      const indicatorVisible = await indicator.isVisible().catch(() => false);
      const indicatorText = indicatorVisible ? (await indicator.innerText()).trim() : "";
      const dotClass = indicatorVisible ? (await indicator.locator('[aria-hidden="true"]').getAttribute("class") ?? "") : "";
      soft("F1", viewport.width, theme, "the sync indicator says Failed and is not green",
        indicatorVisible && indicatorText === "Failed" && /bg-sev-critical/.test(dotClass) && !/bg-sev-ok/.test(dotClass),
        { indicatorVisible, indicatorText, dotClass });
      const stripText = stripVisible ? (await strip.innerText()).replace(/\s+/g, " ") : "";
      soft("F1", viewport.width, theme, "the strip names the failure", stripVisible && stripText.includes("Cluster sync failed"), { stripVisible, stripText: stripText.slice(0, 220) });
      soft("F1", viewport.width, theme, "the data stays on screen", (await rows.count()) > 5, await rows.count());
      const retry = strip.getByRole("button", { name: "Retry sync", exact: true });
      soft("QA-211", viewport.width, theme, "the failed strip carries Retry",
        (await retry.count()) === 1 && await retry.isVisible() && await retry.isEnabled(),
        { count: await retry.count() });
      const ageNode = strip.locator("[data-sync-age]");
      const ageText = (await ageNode.count()) ? (await ageNode.innerText()).trim() : "";
      soft("QA-211", viewport.width, theme, "the data kept on screen carries a real age",
        /^Showing data from \d+ (second|seconds|minute|minutes|hour|hours) ago\.$/.test(ageText), ageText);
      soft("QA-211", viewport.width, theme, "the strip never calls the retained data just now",
        !/just now|loaded just now|0 seconds ago/i.test(stripText), stripText.slice(0, 220));
      const liveCard = page.locator("article[aria-labelledby='kubernetes-connector-title']");
      soft("F1", viewport.width, theme, "the live page does not replace the cluster with a connector-missing card",
        (await liveCard.count()) === 0, { connectorCard: (await liveCard.count()) > 0 ? (await liveCard.innerText()).replace(/\s+/g, " ").slice(0, 200) : null });
      soft("F1", viewport.width, theme, "Preparing does not reappear after the failure",
        (await page.locator("[data-sync-health]").count()) === 0 && !(await page.locator("body").innerText()).includes("Preparing the cluster"),
        "preparing");
      await saveCapture(page, "k22-credential", viewport.width, theme, "live-error");

      // Cold reload: the same failure must be named on a non-Overview view.
      await page.reload();
      await page.waitForLoadState("domcontentloaded");
      await page.waitForTimeout(5_000);
      const tabs = page.getByRole("tablist", { name: "Kubernetes views", exact: true });
      const tabsVisible = await tabs.isVisible().catch(() => false);
      if (tabsVisible) {
        const workloadsTab = viewTab(page, "Workloads");
        if (await workloadsTab.isVisible().catch(() => false)) {
          await workloadsTab.click();
          await expect(page.locator("#kubernetes-view-panel")).toBeVisible({ timeout: 20_000 });
          await page.waitForTimeout(2_000);
        }
      }
      const coldStrip = page.locator('[data-sync-strip="failed"]');
      const coldStripVisible = await coldStrip.isVisible().catch(() => false);
      const mainText = (await page.locator("main").innerText()).replace(/\s+/g, " ");
      const coldCard = page.locator("article[aria-labelledby='kubernetes-connector-title']");
      const coldCardText = (await coldCard.count()) ? (await coldCard.innerText()).replace(/\s+/g, " ") : "";
      const panelText = (await page.locator("#kubernetes-view-panel").count())
        ? (await page.locator("#kubernetes-view-panel").innerText()).replace(/\s+/g, " ")
        : "";
      soft("F1", viewport.width, theme, "a cold reload names the failure on a non-Overview view instead of rendering blank",
        tabsVisible && coldStripVisible && (panelText.includes("Cluster sync failed") || mainText.includes("Cluster sync failed")),
        { tabsVisible, coldStripVisible, main: mainText.slice(0, 260) });
      soft("F1", viewport.width, theme, "a cold reload does not replace the sync failure with a connector-missing card",
        (await coldCard.count()) === 0, { connectorCard: coldCardText.slice(0, 200) });
      soft("F1", viewport.width, theme, "a cold reload does not bring Preparing back",
        (await page.locator("[data-sync-health]").count()) === 0 && !mainText.includes("Preparing the cluster"),
        "preparing");
      const coldRetry = coldStrip.getByRole("button", { name: "Retry sync", exact: true });
      soft("QA-211", viewport.width, theme, "the cold failed strip carries Retry",
        (await coldStrip.locator("[data-sync-retry]").count()) === 1 && await coldRetry.isVisible(),
        { count: await coldStrip.locator("[data-sync-retry]").count() });
      soft("QA-211", viewport.width, theme, "a cold reload claims no age for data it does not have",
        (await page.locator("[data-sync-age]").count()) === 0
          && !/Showing data from/i.test(mainText)
          && !/just now|loaded just now|0 seconds ago/i.test(mainText),
        { main: mainText.slice(0, 260) });
      const coldSync = await request.get(syncURL, { headers, timeout: 30_000 });
      const coldBody = await coldSync.json() as { state?: string; first_ready_at?: string };
      soft("F1", viewport.width, theme, "first_ready_at still arrives on the cold path",
        coldSync.status() === 200 && coldBody.state === "error" && coldBody.first_ready_at === before.first_ready_at,
        { status: coldSync.status(), state: coldBody.state, first_ready_at: coldBody.first_ready_at });
      await saveCapture(page, "k22-credential", viewport.width, theme, "cold-reload-error");
    });
  }
}

// ===========================================================================
// QA-211, the other half of the distinction: a deployment that truly has no
// connector must still get the setup card, and a configured cluster whose
// credential never resolved has never published, so it owes no age at all.
// Both are separate real versus processes on their own ports and state dirs,
// started by k22-visual-seed.mjs. No route fixtures.
// ===========================================================================
for (const width of [1440, 390] as const) {
  for (const theme of themes) {
    test(`QA-211 genuine not configured ${width} ${theme}`, async ({ browser }) => {
      const setupURL = process.env.E2E_K22_SETUP_URL;
      if (!setupURL) throw new Error("E2E_K22_SETUP_URL is required for the connector distinction");
      const viewport = viewports.find((item) => item.width === width)!;
      const page = await newPage(browser, viewport, theme);
      await page.goto(`${setupURL}/agent/kubernetes?view=overview`);
      await signInWithGatewaySecret(page);
      const card = page.locator("article[aria-labelledby='kubernetes-connector-title']");
      await expect(card).toBeVisible({ timeout: 60_000 });
      const cardText = (await card.innerText()).replace(/\s+/g, " ");
      soft("QA-211", width, theme, "the genuine not-configured cluster shows the setup card",
        /Kubernetes connector/.test(cardText) && /Kubernetes connector is not configured\./.test(cardText), cardText.slice(0, 240));
      soft("QA-211", width, theme, "setup is not dressed as a sync failure",
        (await page.locator("[data-sync-strip]").count()) === 0 && (await page.locator("[data-sync-health]").count()) === 0,
        { strip: await page.locator("[data-sync-strip]").count(), health: await page.locator("[data-sync-health]").count() });
      soft("QA-211", width, theme, "setup shows no view tabs and claims no age",
        (await page.getByRole("tablist", { name: "Kubernetes views", exact: true }).count()) === 0
          && (await page.locator("[data-sync-age]").count()) === 0
          && !/Showing data from/i.test(cardText),
        { tabs: await page.getByRole("tablist", { name: "Kubernetes views", exact: true }).count() });
      await saveCapture(page, "k22-connector-distinction", width, theme, "not-configured");
    });

    test(`QA-211 never-published dead credential ${width} ${theme}`, async ({ browser, request }) => {
      const neverURL = process.env.E2E_K22_NEVER_PUBLISHED_URL;
      if (!neverURL) throw new Error("E2E_K22_NEVER_PUBLISHED_URL is required for the age-honesty proof");
      const headers = { "X-Gateway-Secret": process.env.E2E_GATEWAY_SECRET ?? "" };
      const viewport = viewports.find((item) => item.width === width)!;

      // The server's own answer: a failure report for a cluster that never
      // published a snapshot carries no first_ready_at and no age_s.
      const answer = await request.get(`${neverURL}/api/admin/kubernetes/sync?cluster=k22-never`, { headers, timeout: 30_000 });
      const report = await answer.json() as { state?: string; first_ready_at?: string; age_s?: number };
      soft("QA-211", width, theme, "a never-published cluster's report claims no age",
        answer.status() === 200 && report.state === "error" && report.first_ready_at == null && report.age_s == null,
        { status: answer.status(), state: report.state, first_ready_at: report.first_ready_at ?? null, age_s: report.age_s ?? null });

      const page = await newPage(browser, viewport, theme);
      await page.goto(`${neverURL}/agent/kubernetes?cluster=k22-never&view=nodes`);
      await signInWithGatewaySecret(page);
      const surface = page.locator('[data-sync-health="failed"]');
      await expect(surface).toBeVisible({ timeout: 60_000 });
      const mainText = (await page.locator("main").innerText()).replace(/\s+/g, " ");
      soft("QA-211", width, theme, "a never-published failure names itself and blocks the views",
        /Cluster sync failed/.test(mainText) && (await page.locator("[data-sync-strip]").count()) === 0,
        { main: mainText.slice(0, 260), strip: await page.locator("[data-sync-strip]").count() });
      soft("QA-211", width, theme, "no age is claimed when nothing was ever published",
        (await page.locator("[data-sync-age]").count()) === 0
          && !/Showing data from/i.test(mainText)
          && !/loaded just now|just now|0 seconds ago/i.test(mainText),
        { main: mainText.slice(0, 260), ageNodes: await page.locator("[data-sync-age]").count() });
      soft("QA-211", width, theme, "the never-published failure offers the same Retry",
        (await surface.locator("[data-sync-retry]").count()) === 1 && await surface.getByRole("button", { name: "Retry sync", exact: true }).isVisible(),
        { retry: await surface.locator("[data-sync-retry]").count() });
      await saveCapture(page, "k22-connector-distinction", width, theme, "never-published");
    });
  }
}

// ===========================================================================
// SEC-PERM: a caller without infrastructure:view is answered about the
// permission, never about the connector.
//
// The controller now answers the permission before it looks at whether a
// connector exists or whether its credential still resolves, so an unauthorized
// caller gets 403 and learns nothing about the deployment (Go test:
// TestKubernetesPermissionIsAnsweredBeforeConnectorState). This spec
// proves the surface half: when every Kubernetes route answers the controller's
// exact 403, the page renders that refusal and never the setup card, a sync
// failure, view tabs, or an age.
//
// Fixture: the 403 body is the controller's exact refusal word for word. A
// community gateway session always holds infrastructure:view
// (grantCommunityPermissions), so this denial cannot be produced by the seeded
// stack; the server ordering itself is Go-test evidence.
const permissionDenied = { status: 403, error: "infrastructure:view permission is required" };

for (const width of [1440, 390] as const) {
  for (const theme of themes) {
    test(`SEC-PERM permission answers before the connector ${width} ${theme}`, async ({ browser }) => {
      const viewport = viewports.find((item) => item.width === width)!;
      const page = await newPage(browser, viewport, theme);
      await page.route(
        (url) => url.pathname === "/api/admin/kubernetes" || url.pathname.startsWith("/api/admin/kubernetes/"),
        async (route) => {
          await route.fulfill({ status: permissionDenied.status, contentType: "application/json", body: JSON.stringify({ error: permissionDenied.error }) });
        },
      );
      await page.goto(`/agent/kubernetes?cluster=${encodeURIComponent(cluster)}&view=overview`);
      await signInWithGatewaySecret(page);

      const alert = page.getByRole("alert").filter({ hasText: "infrastructure:view permission is required" });
      await expect(alert).toBeVisible({ timeout: 60_000 });
      const mainText = (await page.locator("main").innerText()).replace(/\s+/g, " ");
      soft("SEC-PERM", width, theme, "the page names the missing permission",
        /infrastructure:view permission is required/.test(mainText),
        { main: mainText.slice(0, 260) });
      soft("SEC-PERM", width, theme, "the answer is not the connector setup card",
        (await page.locator("article[aria-labelledby='kubernetes-connector-title']").count()) === 0
          && !/Kubernetes connector is not configured\./.test(mainText),
        { main: mainText.slice(0, 260) });
      soft("SEC-PERM", width, theme, "the answer carries no connector state or data",
        (await page.getByRole("tablist", { name: "Kubernetes views", exact: true }).count()) === 0
          && (await page.locator("[data-sync-health]").count()) === 0
          && (await page.locator("[data-sync-strip]").count()) === 0
          && (await page.locator("[data-sync-age]").count()) === 0
          && !/Cluster sync failed|Showing data from/i.test(mainText),
        { tabs: await page.getByRole("tablist", { name: "Kubernetes views", exact: true }).count(), main: mainText.slice(0, 260) });
      record("SEC-PERM", width, theme, "evidence class", true, {
        class: "fixture", reason: "403 body is the controller's exact refusal; a community gateway session always holds infrastructure:view",
      });
      await saveCapture(page, "k22-permission", width, theme, "denied");
    });
  }
}

// Application-level delta proof; only held sync envelopes and quiet/terminal
// streams are fixtures. Populated logs and same-store restart use the stack.
for (const viewport of [viewports[0], viewports[2]] as const) {
  for (const theme of themes) {
    test(`Preparation continuity delta ${viewport.width} ${theme}`, async ({ browser, request }) => {
      const real = await realSyncReport(request);
      const baseURL = process.env.E2E_BASE_URL!;
      const response = await request.get(`${baseURL}/api/admin/kubernetes/overview?cluster=${cluster}`, { headers: { "X-Gateway-Secret": process.env.E2E_GATEWAY_SECRET ?? "" } });
      expect(response.ok()).toBe(true);
      const overview = await response.json();
      const remaining = ["ControllerRevision", "Ingress", "PersistentVolumeClaim", "ConfigMap", "Secret", "ServiceAccount", "HTTPRoute", "Gateway"];
      const progress = {
        step: "listing", step_index: 2, step_total: 4, steps_done: ["connected"],
        counts: { Pod: 12, Deployment: 4, Namespace: 3, DaemonSet: 0 },
        kinds_done: 8, kinds_total: 20, cluster_version: "v1.31.0", node_count: 2,
        started_at: minutesAgo(1), updated_at: new Date().toISOString(),
        remaining_kinds: ["Service", "ReplicaSet", "CronJob", "HorizontalPodAutoscaler", ...remaining],
        kind_status: Object.fromEntries(["Service", "ReplicaSet", "CronJob", "HorizontalPodAutoscaler", ...remaining].map((kind) => [kind, "pending"])), 
      };
      let publishedCore = false;
      const forced: { report?: SyncReport } = { report: { ...real, state: "syncing", first_ready_at: undefined, age_s: 0, pending_kinds: 12, ready_kinds: 8, total_kinds: 20, failing_kinds: 0, failing: [], kinds: {}, progress } };
      const page = await newPage(browser, viewport, theme);
      await syncRoute(page, forced);
      const overviewRouteMatch = (url: URL) => url.pathname.endsWith("/api/admin/kubernetes/overview");
      await page.route(overviewRouteMatch, (route) => route.fulfill({ json: { ...overview, sync: publishedCore ? { state: "ready", age_s: 0, partial: false } : { state: "syncing", age_s: 0, partial: false, progress } } }));
      await openKubernetes(page);
      const panel = page.locator("[data-preparing-panel]");
      const bar = panel.getByRole("progressbar");
      await expect(bar).toHaveAttribute("aria-valuenow", "8");
      const assertProgress = async (done: number) => {
        await expect(panel).toBeVisible();
        await expect(panel.locator("[data-step]")).toHaveCount(4);
        await expect(bar).toHaveCount(1);
        await expect(bar).toHaveAttribute("aria-valuenow", String(done));
        await expect(bar).toHaveAttribute("aria-valuemax", "20");
        await expect(panel.locator('[data-state="unknown"]')).toHaveCount(0);
        await expect(panel).not.toContainText(/unknown|Synced|\d+\s*%|ETA/i);
        await expect(panel.locator("[data-first-sync-counts]")).toHaveAttribute("aria-label", "Pods 12, Deployments 4, Namespaces 3, DaemonSets 0");
        await expect(panel.locator('[data-step="connected"]')).toHaveAttribute("data-state", "done");
        for (const step of ["building", "metrics"]) await expect(panel.locator(`[data-step="${step}"]`)).toHaveAttribute("data-state", "waiting");
        const geometry = await panel.evaluate((element) => ({
          rowsBottom: Math.max(...Array.from(element.querySelectorAll("[data-step]")).map((row) => row.getBoundingClientRect().bottom)),
          barTop: element.querySelector('[role="progressbar"]')!.getBoundingClientRect().top,
          overflow: element.scrollWidth > element.clientWidth,
        }));
        expect(geometry.barTop).toBeGreaterThanOrEqual(geometry.rowsBottom);
        expect(geometry.overflow).toBe(false);
        record("preparation-delta", viewport.width, theme, "fixture worker progress", true, { done, geometry, counts: progress.counts });
      };
      await assertProgress(8);
      await saveCapture(page, "prepare-delta", viewport.width, theme, "listing-fixture");
      publishedCore = true;
      progress.kinds_done = 12;
      progress.remaining_kinds = remaining;
      forced.report = { ...forced.report!, progress, pending_kinds: 8, ready_kinds: 12 };
      await page.reload();
      await assertProgress(12);
      await expect(panel).toContainText("Remaining: ControllerRevision (pending)");
      await saveCapture(page, "prepare-delta", viewport.width, theme, "core-published-eight-pending-fixture");
      progress.kinds_done = 13;
      progress.remaining_kinds = remaining.slice(1);
      forced.report = { ...forced.report!, progress, pending_kinds: 7, ready_kinds: 13 };
      await page.getByRole("button", { name: "Refresh Kubernetes data", exact: true }).click();
      await expect(bar).toHaveAttribute("aria-valuenow", "13", { timeout: 30_000 });
      await assertProgress(13);
      await saveCapture(page, "prepare-delta", viewport.width, theme, "advancing-without-reset-fixture");

      // Return to the actual already-ready cluster; watch transient gates too.
      forced.report = undefined;
      await page.unroute(overviewRouteMatch);
      await watchPreparationFlashes(page);
      await page.reload();
      await expect(page.locator("[data-sync-state]")).toHaveAttribute("data-sync-state", "ready", { timeout: 60_000 });
      await expect(panel).toHaveCount(0);
      const restart = await page.request.post(`${controlURL}/restart`, { timeout: 120_000 });
      expect(restart.ok()).toBe(true);
      expect((await restart.json()).first_ready_at).toBe(real.first_ready_at);
      await page.reload();
      await expect(page.locator("[data-sync-state]")).toHaveAttribute("data-sync-state", "ready", { timeout: 60_000 });
      expect(await page.evaluate(() => sessionStorage.getItem("unexpected-preparation"))).toBeNull();
      await expect(panel).toHaveCount(0);
      await expect(page.locator("body")).toContainText(/Snapshot \d+s ago/);
      await expect(page.locator("body")).not.toContainText(/Synced \d+s/);
      await saveCapture(page, "prepare-delta", viewport.width, theme, "same-store-restart-live");
      forced.report = forcedReport(real, "syncing");
      await page.reload();
      await expect(page.locator("[data-sync-state]")).toHaveText("Syncing");
      await expect(page.locator("[data-preparing-panel], [data-sync-strip]")).toHaveCount(0);
      await saveCapture(page, "prepare-delta", viewport.width, theme, "routine-topbar-only-fixture");
      forced.report = forcedReport(real, "error");
      await page.reload();
      await expect(page.locator('[data-sync-strip="failed"]')).toBeVisible();
      await expect(page.locator("[data-preparing-panel]")).toHaveCount(0);
      await expect(page.locator('[data-sync-strip="failed"]')).toContainText("Cluster sync failed");
      await saveCapture(page, "prepare-delta", viewport.width, theme, "error-visible-fixture");
    });

    test(`Overview log card application delta ${viewport.width} ${theme}`, async ({ browser }) => {
      for (const subject of ["pod", "deployment"] as const) {
        for (const state of ["waiting", "quiet", "populated", "error", "ended"] as const) {
          const key = subject === "pod" ? "quiet-pod" : bulkDeployment;
          const fixtures: Record<string, SseMode> = state === "populated" ? {} : { [key]: state === "waiting" || state === "quiet" ? "quiet" : state };
          const page = await newPage(browser, viewport, theme, fixtures);
          await openKubernetes(page, "nodes");
          if (subject === "pod") await openPodLogs(page, key); else await openWorkloadLogs(page, key);
          await openDrawerTab(page, "overview", "Overview");
          const card = compactLogSlot(page).locator('.resource-log-viewer[data-compact="true"]');
          await expect(card).toBeVisible();
          if (state === "waiting") await expect(card).toContainText("Waiting for the first log line…");
          if (state === "quiet") await expect(card).toContainText("No new log lines yet. The container is quiet.", { timeout: 20_000 });
          if (state === "populated") await expect.poll(() => card.locator("[data-log-lines] > span").count(), { timeout: 20_000 }).toBeGreaterThan(0);
          if (state === "error") await expect(card.locator(".log-status")).toHaveText("Unavailable");
          if (state === "ended") await expect(card.locator(".log-status")).toHaveText("Ended");
          const factsToggle = drawer(page).getByRole("button", { name: /Details and conditions/ });
          if (await factsToggle.isVisible() && await factsToggle.getAttribute("aria-expanded") === "false") await factsToggle.click();
          await card.scrollIntoViewIfNeeded();
          const geometry = await card.evaluate((element) => {
            const rect = (node: Element) => { const box = node.getBoundingClientRect(); return { left: box.left, right: box.right, width: box.width }; };
            const panel = element.closest(".resource-tabpanel")!;
            return { card: rect(element), slot: rect(element.parentElement!), panel: rect(panel), facts: rect(panel.querySelector(".resource-facts")!), overflow: panel.scrollWidth > panel.clientWidth };
          });
          record("overview-delta", viewport.width, theme, `${subject} ${state} edges`, true, geometry);
          for (const reference of [geometry.slot, geometry.panel, geometry.facts]) {
            expect(Math.abs(geometry.card.left - reference.left)).toBeLessThan(2);
            expect(Math.abs(geometry.card.right - reference.right)).toBeLessThan(2);
          }
          expect(geometry.overflow).toBe(false);
          const qualification = state === "populated" ? "live" : "fixture";
          await saveCapture(page, `overview-${subject}-delta`, viewport.width, theme, `${state}-${qualification}`);
          if (state === "populated") {
            const openFull = card.getByRole("button", { name: /Open full logs/ });
            await openFull.focus();
            await expect(openFull).toBeFocused();
            await page.keyboard.press("Enter");
            await expect(fullLogSlot(page)).toBeVisible();
            await expect(fullLogSlot(page).locator('.resource-log-viewer[data-compact="false"]')).toBeVisible();
            await expect(fullLogSlot(page).getByRole("combobox", { name: "Log container", exact: true })).toBeVisible();
            const fullBounds = await fullLogSlot(page).evaluate((slot) => {
              const card = slot.querySelector(".resource-log-viewer")!.getBoundingClientRect();
              const parent = slot.getBoundingClientRect();
              const footer = slot.querySelector(".log-footer")!.getBoundingClientRect();
              return { leftDelta: card.left - parent.left, rightDelta: card.right - parent.right, footerOverflow: footer.bottom - card.bottom, overflow: slot.scrollWidth > slot.clientWidth };
            });
            expect(Math.abs(fullBounds.leftDelta)).toBeLessThan(2);
            expect(Math.abs(fullBounds.rightDelta)).toBeLessThan(2);
            expect(fullBounds.footerOverflow).toBeLessThanOrEqual(1);
            expect(fullBounds.overflow).toBe(false);
            record("overview-delta", viewport.width, theme, `${subject} full Logs unaffected`, true, fullBounds);
            await saveCapture(page, `full-${subject}-delta`, viewport.width, theme, "populated-live");
          }
          await page.context().close();
          openContexts.splice(openContexts.indexOf(page.context()), 1);
        }
      }
    });
  }
}

// Isolated layout proof: no app request, login, stream, or runtime mutation.
test("isolated compact log slot fills its available width in every output state", async ({ page }) => {
 const css = fs.readFileSync(path.resolve("src/pages/KubernetesResourceDetail.css"), "utf8");
 for (const width of [1440, 1024, 390]) {
  await page.setViewportSize({ width, height: 900 });
  for (const state of ["waiting", "quiet", "populated", "ended", "error"]) {
   await page.setContent(`<style>${css}</style><div data-resource-detail><div class="resource-tabpanel" data-view="overview"><div class="resource-overview"><section class="resource-facts">Facts</section></div><div class="resource-log-slot" data-full="false"><section class="resource-log-viewer" data-compact="true"><header class="log-heading">Live logs</header><div class="log-output">${state === "populated" ? "INFO served" : state}</div><footer class="log-footer">${state}</footer></section></div></div></div>`);
   const bounds = await page.locator(".resource-log-slot").evaluate((slot) => {
    const card = slot.querySelector(".resource-log-viewer")!;
    const parent = slot.parentElement!.getBoundingClientRect();
    return { available: parent.width, slot: slot.getBoundingClientRect().width, card: card.getBoundingClientRect().width };
   });
   expect(Math.abs(bounds.available - bounds.slot), `${width} ${state} slot`).toBeLessThan(2);
   expect(Math.abs(bounds.slot - bounds.card), `${width} ${state} card`).toBeLessThan(2);
  }
 }
});
