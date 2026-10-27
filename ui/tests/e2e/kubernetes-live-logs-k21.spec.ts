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
//             /sync report, or a delayed describe, because fakekube always
//             writes live lines and a real cluster cannot be held in every
//             sync state on demand. Fixture captures are labelled in the run
//             summary.
//
// Captures use the shared capturePath helper (HARNESS_IMAGES_DIR). No product
// edit, no /tmp, no one-off Playwright config.
import { expect, test, type APIRequestContext, type Browser, type BrowserContext, type Page } from "@playwright/test";
import * as fs from "node:fs";
import * as path from "node:path";
import { openApp } from "./helpers";
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

type SseMode = "quiet" | "limit" | "ended" | "bulk";

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
        const match = Object.entries(mapping as Record<string, string>).find(([pod]) => url.includes(`/${pod}/logs/stream`));
        if (!match) return originalFetch(input, init);
        const mode = match[1];
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
  filter: Box;
  filterWrap: Box;
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
    const select = viewer.querySelector<HTMLSelectElement>('select[aria-label="Log container"]');
    const buttons = [...viewer.querySelectorAll("button")];
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
      filter: query(".log-filter"),
      filterWrap: query(".log-filter-wrap"),
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
      await saveCapture(page, "k22-compact", viewport.width, theme, "pod-bottom");
      await closeDrawer(page);

      await openWorkloadLogs(page, bulkDeployment);
      await openDrawerTab(page, "overview", "Overview");
      await expect(compactLogSlot(page).locator('.resource-log-viewer[data-compact="true"]')).toBeVisible({ timeout: 20_000 });
      await saveCapture(page, "k22-compact", viewport.width, theme, "deployment");
      await assertCompact(page, viewport, theme, "Deployment Overview");
      await drawer(page).locator(".resource-tabpanel").evaluate((panel) => { panel.scrollTop = panel.scrollHeight; });
      await page.waitForTimeout(300);
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
      const filterLeft = left(g.filter);
      const units = { status: g.status, container: g.container, instance: g.instance, since: g.since };
      const deltas = Object.fromEntries(Object.entries(units).map(([name, box]) => [name, delta(left(box), filterLeft ?? 0)]));
      const segmentDeltas = { current: delta(left(g.current), filterLeft ?? 0), previous: delta(left(g.previous), filterLeft ?? 0) };
      const loadDelta = delta(left(g.load), filterLeft ?? 0);
      const groupDelta = delta(left(g.primary), left(g.levels ?? g.secondary) ?? 0);

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

      // Alignment contract: every row-1 control unit starts at the Filter lines
      // field's left edge. Recorded per control so a single offset is visible.
      for (const [name, value] of Object.entries(deltas)) {
        soft("S2", viewport.width, theme, `row-1 ${name} left edge equals the Filter lines field`, value !== null && Math.abs(value) <= 1, { [name]: value, filterLeft });
      }
      // The segmented control is one control: its group left edge is graded
      // above; the individual segments are recorded, not graded.
      record("S2", viewport.width, theme, "segments", true, segmentDeltas);
      record("S2", viewport.width, theme, "alignment", true, { filterLeft, deltas, segmentDeltas, loadDelta, groupDelta, containerVsFilter: deltas.container });
      soft("S2", viewport.width, theme, "row 1 and row 2 share the same content left edge", groupDelta !== null && Math.abs(groupDelta) <= 1, { groupDelta });
      soft("S2", viewport.width, theme, "illustration's 0px guide: container select meets the Filter lines field",
        deltas.container !== null && Math.abs(deltas.container) <= 1, { containerVsFilter: deltas.container });
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
      soft("S2", viewport.width, theme, "workload container select lists All containers first",
        wg.containerOptions[0]?.text === "All containers", wg.containerOptions);
      soft("S2", viewport.width, theme, "workload read has no chip row or Follow", wg.chips === 0 && wg.follow === 0 && wg.followButtons === 0, { chips: wg.chips, follow: wg.follow });

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
// S4: Preparing only before the first publication; a strip afterwards.
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
            step_index: 1,
            step_total: 4,
            steps_done: ["connected"],
            counts: { Pod: 12, Deployment: 4 },
            kinds_done: 6,
            kinds_total: 12,
            cluster_version: "v1.31.0",
            node_count: 2,
            started_at: minutesAgo(1),
            updated_at: new Date().toISOString(),
          },
        },
      };
      await page.route((url) => url.pathname.endsWith("/api/admin/kubernetes/overview"), (route) =>
        route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(preparingOverview) }));
      await openKubernetes(page, "overview");
      await expect(page.locator("[data-preparing-panel]")).toBeVisible({ timeout: 60_000 });
      soft("S4", viewport.width, theme, "never-published cluster shows Preparing", (await page.locator("body").innerText()).includes("Preparing") || (await page.locator("[data-preparing-panel]").isVisible()), "preparing");
      soft("S4", viewport.width, theme, "Preparing shows progress", await page.getByRole("progressbar", { name: "First sync progress", exact: true }).isVisible(), "progressbar");
      soft("S4", viewport.width, theme, "never-published cluster has no data view", (await page.getByRole("region", { name: "Workloads", exact: true }).count()) === 0, "workloads card");
      await saveCapture(page, "k22-sync", viewport.width, theme, "preparing");
      await closeDrawer(page);

      // Published: each forced state keeps data and shows the strip.
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
        const strip = page.locator(`[data-sync-strip="${item.strip}"]`);
        await expect(strip).toBeVisible({ timeout: 20_000 });
        const stripText = (await strip.innerText()).replace(/\s+/g, " ");
        soft("S4", viewport.width, theme, `${item.state}: Preparing never renders`, (await page.locator("[data-sync-health]").count()) === 0 && !(await page.locator("body").innerText()).includes("Preparing the cluster"), stripText.slice(0, 160));
        soft("S4", viewport.width, theme, `${item.state}: strip names the state`, stripText.includes(item.headline), stripText.slice(0, 160));
        soft("S4", viewport.width, theme, `${item.state}: strip carries Retry`, (await strip.locator("[data-sync-retry]").count()) === 1, "retry");
        soft("S4", viewport.width, theme, `${item.state}: data age is visible`, (await strip.locator("[data-sync-age]").count()) === 1 && /Showing data (from|loaded)/.test(stripText), stripText.slice(0, 200));
        soft("S4", viewport.width, theme, `${item.state}: the view keeps its data`, (await rows.count()) > 0, await rows.count());
        if (item.state === "partial" || item.state === "error") {
          soft("S4", viewport.width, theme, `${item.state}: the strip names the failing kind`, (await strip.locator("[data-sync-problem]").count()) >= 1, stripText.slice(0, 240));
        }
        await saveCapture(page, "k22-sync", viewport.width, theme, item.state);
      }

      // Reload mid-resync: data plus the strip, never Preparing.
      forced.report = forcedReport(real, "syncing");
      await page.reload();
      await expect(page.getByRole("tablist", { name: "Kubernetes views", exact: true })).toBeVisible({ timeout: 60_000 });
      await selectView(page, "Workloads");
      await expect.poll(async () => page.getByRole("region", { name: "Workloads", exact: true }).locator('[role="listitem"]').count(), { timeout: 45_000 }).toBeGreaterThan(0);
      await expect(page.locator('[data-sync-strip="preparing"]')).toBeVisible({ timeout: 20_000 });
      soft("S4", viewport.width, theme, "reload mid-resync shows data plus the strip, never Preparing",
        (await page.locator("[data-sync-health]").count()) === 0 && !(await page.locator("body").innerText()).includes("Preparing the cluster"),
        "reload");
      await saveCapture(page, "k22-sync", viewport.width, theme, "reload-mid-resync");

      // Retry still reaches the server.
      const retry = page.locator('[data-sync-strip="preparing"] [data-sync-retry]');
      const syncCalls = { count: 0 };
      page.on("request", (request) => { if (request.url().includes("/api/admin/kubernetes/sync")) syncCalls.count++; });
      const before = syncCalls.count;
      await retry.click();
      await expect.poll(() => syncCalls.count, { timeout: 15_000 }).toBeGreaterThan(before);
      soft("S4", viewport.width, theme, "Retry re-reads the sync report", syncCalls.count > before, { before, after: syncCalls.count });
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
