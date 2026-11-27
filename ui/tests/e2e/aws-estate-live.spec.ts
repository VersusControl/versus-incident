// AWS estate pages on a real server: Overview, Topology (Accounts, Groups and
// Focus with the side panel), Coverage, the Collection cost card in Settings,
// and Changes, at desktop and phone width in both themes.
//
// The stack is plans/harness-run/tests/aws/aws-changes.sh browser-up with
// AWS_CHANGES_ACCOUNTS=estate and the fakeaws estate-hub-spoke scenario: the
// real server binary (harness_awsendpoint build, file storage) reading four
// accounts of one organization from tests/fakeaws. Nothing reaches AWS. One
// test answers the topology API itself to draw the "too many" state the small
// fake estate cannot reach; its captures are named "fixture".
//
// Every capture is taken after the state it shows is asserted, into
// CAPTURE_DIR (or the run's HARNESS_IMAGES_DIR) through capturePath. What the
// page showed (numbers, text contrast below WCAG AA, repeated numbers, focus
// rings) goes to $HARNESS_RESULTS_DIR/aws-estate-live.jsonl for review.
//
// Env: E2E_AWS_ESTATE=1 (required), AWS_ESTATE_SCALE (default 2),
// AWS_ESTATE_VIEWPORTS=desktop,mobile, AWS_ESTATE_STATE (capture state label,
// default "estate"), AWS_ESTATE_FOCUS_ACCOUNT / AWS_ESTATE_FOCUS_REF (the
// resource Focus opens), AWS_ESTATE_GROUPS_ACCOUNT (the account Groups opens),
// AWS_ESTATE_REMOVE (display name of the account the "remove account" test
// removes in Settings).
import { expect, test, type Browser, type BrowserContext, type Page } from "@playwright/test";
import * as fs from "node:fs";
import * as path from "node:path";
import { openApp } from "./helpers";
import { capturePath, harnessResultsDir } from "./support/capture";

const LIVE = process.env.E2E_AWS_ESTATE === "1";
test.skip(!LIVE, "needs the estate browser stack (aws-changes.sh browser-up with AWS_CHANGES_ACCOUNTS=estate)");

const ALL_VIEWPORTS = [
  { name: "desktop", width: 1440, height: 900 },
  { name: "mobile", width: 390, height: 844 },
] as const;
type Viewport = (typeof ALL_VIEWPORTS)[number];
const WANT = (process.env.AWS_ESTATE_VIEWPORTS ?? "").split(",").map((v) => v.trim()).filter(Boolean);
const VIEWPORTS = ALL_VIEWPORTS.filter((v) => WANT.length === 0 || WANT.includes(v.name));
const DESKTOP = ALL_VIEWPORTS[0];
const THEMES = ["dark", "light"] as const;
type Theme = (typeof THEMES)[number];
const SCALE = Number(process.env.AWS_ESTATE_SCALE || "2");
const STATE = process.env.AWS_ESTATE_STATE || "estate";
const FOCUS_ACCOUNT = process.env.AWS_ESTATE_FOCUS_ACCOUNT || "123400000002";
const FOCUS_REF = process.env.AWS_ESTATE_FOCUS_REF || "arn:aws:ecs:eu-south-2:123400000002:service/versus/checkout";
const GROUPS_ACCOUNT = process.env.AWS_ESTATE_GROUPS_ACCOUNT || "123400000002";

// Wire words that must never reach the screen.
const RAW_CODES = [
  "not_read_yet", "not_read", "read_failed", "assume_role_failed", "access_denied", "not_enabled", "too_many",
  "correction_conflict", "same_org", "may_access", "seen_from", "shared_via", "alias_of", "in_subnet", "in_vpc",
  "uses_security_group", "member_of", "[object Object]", "undefined", "NaN",
];

const contexts: BrowserContext[] = [];
test.afterEach(async () => {
  for (const c of contexts.splice(0)) await c.close().catch(() => {});
});

interface Watch {
  console: string[];
  http: string[];
  aws: string[];
}

async function open(browser: Browser, viewport: Viewport, theme: Theme, url: string, watch: Watch): Promise<Page> {
  const context = await browser.newContext({ viewport: { width: viewport.width, height: viewport.height }, deviceScaleFactor: SCALE, colorScheme: theme });
  contexts.push(context);
  await context.addInitScript((value) => localStorage.setItem("versus.theme", value), theme);
  const page = await context.newPage();
  page.on("console", (m) => {
    if (m.type() === "error" && !(m.location()?.url ?? "").includes("/enterprise/api/sso/deployment")) watch.console.push(m.text().slice(0, 200));
  });
  page.on("response", (r) => {
    const u = new URL(r.url());
    if (u.pathname.startsWith("/api/") && r.status() >= 400) watch.http.push(`${r.status()} ${u.pathname}${u.search}`);
    if (u.pathname.startsWith("/api/admin/aws/")) watch.aws.push(`${r.status()} ${u.pathname}${u.search}`);
  });
  await page.emulateMedia({ colorScheme: theme });
  await openApp(page, url);
  watch.console.length = 0;
  watch.http.length = 0;
  watch.aws.length = 0;
  await page.goto(url);
  return page;
}

async function settle(page: Page) {
  await page.waitForLoadState("networkidle").catch(() => {});
  await expect(page.locator('[aria-busy="true"], [data-loading]')).toHaveCount(0, { timeout: 30_000 });
  await page.waitForTimeout(300);
}

/** The whole page: grow the viewport by what the main scroller hides, then shoot. */
// AWS_ESTATE_SURFACE_PREFIX names a capture set (e.g. "a17-" for the final images).
const PREFIX = process.env.AWS_ESTATE_SURFACE_PREFIX ?? "";

async function shot(page: Page, viewport: Viewport, theme: Theme, surface: string, state = STATE) {
  const out = capturePath(PREFIX + surface, viewport.width, theme, state);
  const extra = await page.evaluate(() => {
    const m = document.querySelector("main");
    return m ? Math.max(0, m.scrollHeight - m.clientHeight) : 0;
  });
  if (extra > 0) await page.setViewportSize({ width: viewport.width, height: viewport.height + extra });
  await page.waitForTimeout(300);
  await page.screenshot({ path: out, animations: "disabled", fullPage: true });
  if (extra > 0) await page.setViewportSize({ width: viewport.width, height: viewport.height });
  return out;
}

/** The viewport only, for open popovers, lists and focus rings. */
async function shotView(page: Page, viewport: Viewport, theme: Theme, surface: string, state: string) {
  const out = capturePath(surface, viewport.width, theme, state);
  await page.waitForTimeout(250);
  await page.screenshot({ path: out, animations: "disabled" });
  return out;
}

function record(name: string, data: Record<string, unknown>) {
  const file = path.join(harnessResultsDir(), "aws-estate-live.jsonl");
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.appendFileSync(file, JSON.stringify({ test: name, state: STATE, at: new Date().toISOString(), ...data }) + "\n");
}

/** Visible text below WCAG AA (4.5:1, 3:1 for large text), with the colours it was drawn in. */
async function contrastFailures(page: Page): Promise<string[]> {
  return page.evaluate(() => {
    const parse = (c: string): [number, number, number, number] | null => {
      const m = c.match(/rgba?\(([^)]+)\)/);
      if (!m) return null;
      const p = m[1].split(/[ ,/]+/).filter(Boolean).map(Number);
      return [p[0], p[1], p[2], p.length > 3 ? p[3] : 1];
    };
    const over = (top: number[], bottom: number[]) => {
      const a = top[3];
      return [top[0] * a + bottom[0] * (1 - a), top[1] * a + bottom[1] * (1 - a), top[2] * a + bottom[2] * (1 - a), 1];
    };
    const lum = (c: number[]) => {
      const f = (v: number) => {
        const s = v / 255;
        return s <= 0.03928 ? s / 12.92 : Math.pow((s + 0.055) / 1.055, 2.4);
      };
      return 0.2126 * f(c[0]) + 0.7152 * f(c[1]) + 0.0722 * f(c[2]);
    };
    const bgOf = (el: Element): number[] => {
      const chain: Element[] = [];
      for (let e: Element | null = el; e; e = e.parentElement) chain.push(e);
      let bg = [255, 255, 255, 1];
      const root = getComputedStyle(document.documentElement).backgroundColor;
      const body = getComputedStyle(document.body).backgroundColor;
      for (const c of [root, body]) {
        const p = parse(c);
        if (p && p[3] > 0) bg = over(p, bg);
      }
      for (const e of chain.reverse()) {
        if (e === document.documentElement || e === document.body) continue;
        const p = parse(getComputedStyle(e).backgroundColor);
        if (p && p[3] > 0) bg = over(p, bg);
      }
      return bg;
    };
    const out: string[] = [];
    const seen = new Set<Element>();
    const walker = document.createTreeWalker(document.querySelector("main") ?? document.body, NodeFilter.SHOW_TEXT);
    for (let n = walker.nextNode(); n; n = walker.nextNode()) {
      const text = (n.textContent ?? "").trim();
      const el = n.parentElement;
      if (!text || !el || seen.has(el)) continue;
      seen.add(el);
      const st = getComputedStyle(el);
      const r = el.getBoundingClientRect();
      if (st.visibility === "hidden" || st.display === "none" || r.width === 0 || r.height === 0) continue;
      if (el.closest("[aria-hidden='true'], button:disabled, [disabled], svg")) continue;
      let opacity = 1;
      for (let e: Element | null = el; e; e = e.parentElement) opacity *= Number(getComputedStyle(e).opacity || "1");
      const fg0 = parse(st.color);
      if (!fg0) continue;
      const bg = bgOf(el);
      const fg = over([fg0[0], fg0[1], fg0[2], fg0[3] * opacity], bg);
      const l1 = lum(fg);
      const l2 = lum(bg);
      const ratio = (Math.max(l1, l2) + 0.05) / (Math.min(l1, l2) + 0.05);
      const size = parseFloat(st.fontSize);
      const bold = Number(st.fontWeight) >= 700;
      const large = size >= 24 || (bold && size >= 18.66);
      if (ratio < (large ? 3 : 4.5)) out.push(`${ratio.toFixed(2)} "${text.slice(0, 50)}" ${st.color} on rgb(${bg.slice(0, 3).map(Math.round).join(",")}) ${size}px`);
    }
    return out;
  });
}

/** Raw wire words on screen. */
async function rawCodes(page: Page): Promise<string[]> {
  const text = await page.locator("main").innerText();
  return RAW_CODES.filter((c) => text.includes(c));
}

/** Numbers of the summary card that show again elsewhere on the page (10 and up; small numbers repeat by chance). */
async function repeatedNumbers(page: Page): Promise<string[]> {
  return page.evaluate(() => {
    const card = document.querySelector("[data-summary-card]");
    if (!card) return [];
    const values = Array.from(card.querySelectorAll("[data-number] dd")).map((d) => (d.textContent ?? "").replace(/[^0-9,%/]/g, ""));
    const main = document.querySelector("main") as HTMLElement | null;
    const clone = main?.cloneNode(true) as HTMLElement | undefined;
    clone?.querySelector("[data-summary-card]")?.remove();
    const rest = clone?.innerText ?? "";
    const out: string[] = [];
    for (const v of values) {
      const digits = v.replace(/[^0-9]/g, "");
      if (digits.length < 2) continue;
      const re = new RegExp(`(^|[^0-9,])${v.replace(/[/]/g, "\\/")}($|[^0-9,])`);
      if (re.test(rest)) out.push(v);
    }
    return out;
  });
}

/** The map's scroll box: content size, scroll offset, and which account cards are inside the visible part. */
async function canvasProbe(page: Page) {
  return page.evaluate(() => {
    const c = document.querySelector<HTMLElement>("[data-canvas]");
    if (!c) return null;
    const box = c.getBoundingClientRect();
    const cards = Array.from(document.querySelectorAll<HTMLElement>("[data-account-card]")).map((el) => {
      const r = el.getBoundingClientRect();
      const visible = r.right > box.left && r.left < box.right && r.bottom > box.top && r.top < box.bottom;
      const full = r.left >= box.left - 1 && r.right <= box.right + 1 && r.top >= box.top - 1 && r.bottom <= box.bottom + 1;
      return { id: el.getAttribute("data-account-card"), state: el.getAttribute("data-state") ?? el.getAttribute("data-label"), visible, full };
    });
    // Frame headings ("Organization", "Other accounts"): fully inside the visible part or not.
    const headings: Record<string, boolean> = {};
    const walker = document.createTreeWalker(c, NodeFilter.SHOW_TEXT);
    for (let n = walker.nextNode(); n; n = walker.nextNode()) {
      const t = (n.textContent ?? "").trim().toLowerCase();
      if (t !== "organization" && t !== "other accounts") continue;
      const range = document.createRange();
      range.selectNodeContents(n);
      const r = range.getBoundingClientRect();
      headings[t] = r.left >= box.left - 1 && r.right <= box.right + 1 && r.top >= box.top - 1 && r.bottom <= box.bottom + 1;
    }
    // The frame box (the first SVG rect): how far its top-left corner sits from the visible part's top-left.
    const frame = c.querySelector("svg rect")?.getBoundingClientRect();
    const frameTopLeft = frame ? { dx: Math.round(frame.left - box.left), dy: Math.round(frame.top - box.top), inView: frame.left >= box.left - 1 && frame.top >= box.top - 1 } : null;
    return { client: [c.clientWidth, c.clientHeight], content: [c.scrollWidth, c.scrollHeight], scroll: [c.scrollLeft, c.scrollTop], cards, headings, frameTopLeft };
  });
}

async function review(page: Page, name: string, viewport: Viewport, theme: Theme, watch: Watch, extra: Record<string, unknown> = {}) {
  const horizontal = await page.evaluate(() => {
    const m = document.querySelector("main");
    return m ? Math.max(0, m.scrollWidth - m.clientWidth) : 0;
  });
  record(name, {
    viewport: viewport.name,
    theme,
    url: page.url(),
    contrast: await contrastFailures(page),
    raw: await rawCodes(page),
    repeated: await repeatedNumbers(page),
    horizontalOverflow: horizontal,
    console: [...watch.console],
    http: [...watch.http],
    canvas: name.startsWith("aws-topology") ? await canvasProbe(page) : undefined,
    ...extra,
  });
}

const PAGES = [
  { surface: "aws-overview", url: "/aws/overview", ready: "[data-summary-card] [data-number]" },
  { surface: "aws-topology-accounts", url: "/aws/topology?level=accounts", ready: '[data-level="accounts"], [data-list]' },
  { surface: "aws-topology-groups", url: `/aws/topology?level=groups&account=${GROUPS_ACCOUNT}`, ready: '[data-level="groups"], [data-list]', panel: true },
  {
    surface: "aws-topology-focus",
    url: `/aws/topology?level=focus&account=${FOCUS_ACCOUNT}&ref=${encodeURIComponent(FOCUS_REF)}`,
    ready: '[data-level="focus"], [data-list]',
  },
  { surface: "aws-coverage", url: "/aws/coverage", ready: "[data-summary-card] [data-number]" },
  { surface: "aws-settings", url: "/aws/settings", ready: "[data-collection-cost]" },
  { surface: "aws-changes", url: "/aws/changes", ready: "[data-summary-card]" },
] as const;

for (const viewport of VIEWPORTS) {
  for (const theme of THEMES) {
    test(`pages ${viewport.name} ${theme}`, async ({ browser }) => {
      test.setTimeout(240_000);
      const watch: Watch = { console: [], http: [], aws: [] };
      const page = await open(browser, viewport, theme, "/aws/overview", watch);
      for (const p of PAGES) {
        watch.console.length = 0;
        watch.http.length = 0;
        await page.goto(p.url);
        // A Groups view of an account never read shows its not-read card instead of the map.
        const ready = p.surface === "aws-topology-groups" ? `${p.ready}, [data-not-read]` : p.ready;
        await expect(page.locator(ready).first()).toBeVisible({ timeout: 30_000 });
        await settle(page);
        if ("panel" in p && p.panel && (await page.locator("[data-group] button[aria-pressed]").count()) > 0) {
          // Open the side panel on the first named group.
          const first = page.locator("[data-group] button[aria-pressed]").first();
          await first.click();
          await expect(page.locator("[data-group-panel]")).toBeVisible({ timeout: 15_000 });
          await settle(page);
        }
        if (p.surface === "aws-topology-focus" && (await page.locator('[data-level="focus"]').count()) > 0) await expect(page.locator("[data-focus-panel]")).toBeVisible({ timeout: 15_000 });
        if (p.surface === "aws-settings") await page.locator("[data-collection-cost]").scrollIntoViewIfNeeded();
        const file = await shot(page, viewport, theme, p.surface);
        if (p.surface === "aws-settings") {
          // The Collection cost card alone, with the page grown so the whole card is drawn (the main area scrolls).
          const extra = await page.evaluate(() => {
            const m = document.querySelector("main");
            return m ? Math.max(0, m.scrollHeight - m.clientHeight) : 0;
          });
          if (extra > 0) await page.setViewportSize({ width: viewport.width, height: viewport.height + extra });
          await page.locator("[data-collection-cost]").screenshot({ path: capturePath(PREFIX + "aws-settings-cost", viewport.width, theme, STATE), animations: "disabled" });
          if (extra > 0) await page.setViewportSize({ width: viewport.width, height: viewport.height });
        }
        await review(page, p.surface, viewport, theme, watch, { file });
      }
      // Phone width opens Topology in the List view; the map is one click (view=map) away.
      if (viewport.name === "mobile") {
        for (const p of PAGES.filter((x) => x.surface.startsWith("aws-topology-"))) {
          await page.goto(`${p.url}&view=map`);
          await expect(page.locator(`${p.ready.split(",")[0]}, [data-not-read]`).first()).toBeVisible({ timeout: 30_000 });
          await settle(page);
          const file = await shot(page, viewport, theme, `${p.surface}-map`);
          await review(page, `${p.surface} map`, viewport, theme, watch, { file });
        }
      }
    });
  }
}

test("redirects and sidebar", async ({ browser }) => {
  const watch: Watch = { console: [], http: [], aws: [] };
  const page = await open(browser, DESKTOP, "dark", "/aws/overview", watch);
  const landed: Record<string, string> = {};
  const want: Record<string, RegExp> = {
    "/aws": /\/aws\/overview$/,
    "/aws/resources": /\/aws\/topology\?view=list$/,
    "/aws/map": /\/aws\/topology$/,
    "/aws/applications": /\/aws\/topology\?level=groups$/,
    "/aws/security": /\/aws\/issues$/,
  };
  for (const [from, re] of Object.entries(want)) {
    await page.goto(from);
    await page.waitForURL(re, { timeout: 15_000 });
    await settle(page);
    landed[from] = new URL(page.url()).pathname + new URL(page.url()).search;
    await expect(page.getByText(/page not found|404/i)).toHaveCount(0);
  }
  await page.goto("/aws/overview");
  await settle(page);
  const nav = page.locator('nav[aria-label="Primary"]');
  const items = await nav.locator('a[href^="/aws/"]').evaluateAll((as) => as.map((a) => (a.textContent ?? "").trim()));
  record("redirects", { landed, sidebar: items, http: watch.http });
  expect(items).toEqual(["Overview", "Issues", "Topology", "Changes", "FinOps", "Settings"]);
  await shotView(page, DESKTOP, "dark", "aws-sidebar", "redirects");
});

test("changes warning line opens coverage", async ({ browser }) => {
  const watch: Watch = { console: [], http: [], aws: [] };
  const page = await open(browser, DESKTOP, "dark", "/aws/changes", watch);
  await expect(page.locator("[data-summary-card]")).toBeVisible({ timeout: 30_000 });
  await settle(page);
  const line = page.locator("[data-summary-card] [data-warning-line]");
  const present = (await line.count()) > 0;
  let after = "";
  if (present) {
    await shotView(page, DESKTOP, "dark", "aws-changes", "warning-line");
    await line.getByRole("link", { name: /Details/ }).click();
    await page.waitForURL(/\/aws\/coverage/, { timeout: 15_000 });
    await expect(page.locator("[data-summary-card] [data-number]").first()).toBeVisible({ timeout: 30_000 });
    after = page.url();
    await settle(page);
    await shotView(page, DESKTOP, "dark", "aws-coverage", "from-changes-details");
  }
  record("changes details link", { present, after, http: watch.http });
  expect(present, "the Changes page shows a warning line on this estate").toBe(true);
});

test("search: capped, results, open in focus", async ({ browser }) => {
  for (const viewport of VIEWPORTS) {
    for (const theme of THEMES) {
      const watch: Watch = { console: [], http: [], aws: [] };
      const page = await open(browser, viewport, theme, "/aws/topology?level=accounts&view=map", watch);
      await expect(page.locator('[data-level="accounts"]')).toBeVisible({ timeout: 30_000 });
      await settle(page);
      const box = page.getByRole("searchbox", { name: "Search resources" });
      await box.fill("ch");
      await box.press("Enter");
      await expect(page.locator("[data-search-capped]")).toBeVisible({ timeout: 15_000 });
      await shotView(page, viewport, theme, "aws-topology-search", "capped");
      await box.fill("tgw-0e1f");
      await box.press("Enter");
      const results = page.getByRole("listbox", { name: "Resources found" });
      await expect(results.getByRole("option")).toHaveCount(1, { timeout: 15_000 });
      await expect(page.locator("[data-search-capped]")).toHaveCount(0);
      await shotView(page, viewport, theme, "aws-topology-search", "one-result");
      await results.getByRole("option").first().getByRole("button").click();
      await expect(page.locator('[data-level="focus"], [data-list]').first()).toBeVisible({ timeout: 30_000 });
      // A phone opens the result in the List view; the rest of this test reads the map.
      if ((await page.locator('[data-level="focus"]').count()) === 0) {
        await settle(page);
        await shot(page, viewport, theme, "aws-topology-focus", "from-search-list");
        await page.goto(`${page.url()}&view=map`);
        await expect(page.locator('[data-level="focus"]')).toBeVisible({ timeout: 30_000 });
      }
      await settle(page);
      const file = await shot(page, viewport, theme, "aws-topology-focus", "from-search-transit-gateway");
      await review(page, "focus from search", viewport, theme, watch, { file });
      // Hops 3 from the network hub: bounded, and the page says so when it would cut.
      await page.getByRole("group", { name: "Hops" }).getByRole("button", { name: "3" }).click();
      await settle(page);
      const tooMany = await page.locator("[data-too-many]").count();
      const nodes = await page.locator('[data-level="focus"] [data-node]').count();
      const file3 = await shot(page, viewport, theme, "aws-topology-focus", "hub-hops-3");
      await review(page, "focus hub hops 3", viewport, theme, watch, { file: file3, tooMany, nodes });
      await box.fill("zz-nothing-matches");
      await box.press("Enter");
      await expect(page.getByText("No resource matches")).toBeVisible({ timeout: 15_000 });
      await shotView(page, viewport, theme, "aws-topology-search", "no-match");
    }
  }
});

test("too many to draw (fixture answer)", async ({ browser }) => {
  for (const viewport of VIEWPORTS) {
    for (const theme of THEMES) {
      const watch: Watch = { console: [], http: [], aws: [] };
      const page = await open(browser, viewport, theme, "/aws/overview", watch);
      // The fake estate is too small to pass 300 nodes; answer the topology
      // read with the server's too_many shape (the wire form of the Go type).
      await page.route("**/api/admin/aws/topology?*", async (route) => {
        const u = new URL(route.request().url());
        if (u.searchParams.get("level") !== "focus") return route.continue();
        await route.fulfill({
          json: {
            enabled: true, level: "focus", account: FOCUS_ACCOUNT, ref: FOCUS_REF, accounts: [FOCUS_ACCOUNT], lenses: ["network", "identity", "data"],
            nodes: [], links: [], node_count: 2140, max_nodes: 300, too_many: true, fan_out_capped: true,
            hint: { text: "Pick fewer hops, one Region, or one lens", hops: 1 },
          },
        });
      });
      await page.goto(`/aws/topology?level=focus&account=${FOCUS_ACCOUNT}&ref=${encodeURIComponent(FOCUS_REF)}&hops=3`);
      await expect(page.locator("[data-too-many]")).toBeVisible({ timeout: 30_000 });
      await settle(page);
      const file = await shot(page, viewport, theme, "aws-topology-focus", "too-many-fixture");
      await review(page, "too many (fixture)", viewport, theme, watch, { file, text: await page.locator("[data-too-many]").innerText() });
    }
  }
});

test("side panel: rename, merge, move, changed by someone else", async ({ browser }) => {
  test.setTimeout(240_000);
  const watch: Watch = { console: [], http: [], aws: [] };
  const theme: Theme = "dark";
  const page = await open(browser, DESKTOP, theme, `/aws/topology?level=groups&account=${GROUPS_ACCOUNT}`, watch);
  await expect(page.locator('[data-level="groups"]')).toBeVisible({ timeout: 30_000 });
  await settle(page);
  const panel = page.locator("[data-group-panel]");
  const pick = async (group: string) => {
    // Ungrouped opens its panel from "Move resources ›"; a named group from its title.
    const opener = group === "ungrouped" ? page.locator(`[data-group="ungrouped"] button`).filter({ hasText: /Move resources/ }) : page.locator(`[data-group="${group}"] button[aria-pressed]`);
    await opener.first().click();
    await expect(page.locator(`[data-group-panel="${group}"]`)).toBeVisible({ timeout: 15_000 });
    await settle(page);
  };
  const toastText = async () => (await page.locator('[role="status"], [role="alert"]').allInnerTexts()).join(" | ");
  const notes: Record<string, unknown> = {};

  const only = process.env.AWS_ESTATE_PANEL_STEP ?? "";
  if (only !== "move") {
  // Rename "versus" (a name-prefix group) to a long name.
  await pick("prefix:versus-");
  await panel.getByRole("button", { name: "Rename" }).click();
  const nameBox = panel.getByRole("textbox", { name: "Group name" });
  await expect(nameBox).toBeFocused();
  await shotView(page, DESKTOP, theme, "aws-topology-panel", "rename-open");
  await nameBox.fill("checkout-platform-shared-services-and-batch-workers");
  await nameBox.press("Enter");
  await expect(page.getByText("Saved. It applies on every read")).toBeVisible({ timeout: 15_000 });
  await settle(page);
  await expect(page.locator('[data-group="prefix:versus-"]')).toContainText("checkout-platform-shared", { timeout: 15_000 });
  notes.rename = await toastText();
  await shot(page, DESKTOP, theme, "aws-topology-panel", "renamed-long-name");

  // Changed by someone else: start a rename, let a second tab rename the
  // same group, then save the first.
  await pick("prefix:versus-");
  await panel.getByRole("button", { name: "Rename" }).click();
  const other = await page.context().newPage();
  await other.goto(`/aws/topology?level=groups&account=${GROUPS_ACCOUNT}`);
  await expect(other.locator('[data-level="groups"]')).toBeVisible({ timeout: 30_000 });
  await other.locator('[data-group="prefix:versus-"] button[aria-pressed]').first().click();
  const otherPanel = other.locator('[data-group-panel="prefix:versus-"]');
  await expect(otherPanel).toBeVisible({ timeout: 15_000 });
  await otherPanel.getByRole("button", { name: "Rename" }).click();
  await otherPanel.getByRole("textbox", { name: "Group name" }).fill("batch");
  await otherPanel.getByRole("textbox", { name: "Group name" }).press("Enter");
  await expect(other.getByText("Saved. It applies on every read")).toBeVisible({ timeout: 15_000 });
  await other.close();
  await panel.getByRole("textbox", { name: "Group name" }).fill("checkout-web");
  await panel.getByRole("textbox", { name: "Group name" }).press("Enter");
  const conflict = page.locator("[data-conflict]");
  await expect(conflict).toBeVisible({ timeout: 15_000 });
  notes.conflict = await conflict.innerText();
  await shotView(page, DESKTOP, theme, "aws-topology-panel", "changed-by-someone-else");
  await conflict.getByRole("button").click();
  await settle(page);

  // Merge "wkl" into "checkout".
  await pick("prefix:wkl-");
  await panel.getByRole("button", { name: "Merge with" }).click();
  await page.getByRole("option", { name: /checkout/ }).first().click();
  await panel.getByRole("button", { name: "Merge", exact: true }).click();
  await expect(page.getByText("Saved. It applies on every read")).toBeVisible({ timeout: 15_000 });
  await settle(page);
  notes.mergedGone = await page.locator('[data-group="prefix:wkl-"]').count();
  await shot(page, DESKTOP, theme, "aws-topology-panel", "merged");
  }

  // Move a resource from Ungrouped into a group.
  await pick("ungrouped");
  await panel.getByRole("button", { name: "Resource to move" }).click();
  await page.getByRole("option", { name: /lambda-exec|image-resize|orders-export/ }).first().click();
  await panel.getByRole("button", { name: "Group to move to" }).click();
  await page.getByRole("option", { name: /checkout/ }).first().click();
  await shotView(page, DESKTOP, theme, "aws-topology-panel", "move-ready");
  await panel.getByRole("button", { name: "Move", exact: true }).click();
  await expect(page.getByText("Saved. It applies on every read")).toBeVisible({ timeout: 15_000 });
  await settle(page);
  await shot(page, DESKTOP, theme, "aws-topology-panel", "moved");
  // The same panel in light, and on a phone.
  for (const [vp, th] of [[DESKTOP, "light"], [ALL_VIEWPORTS[1], "dark"], [ALL_VIEWPORTS[1], "light"]] as const) {
    const w: Watch = { console: [], http: [], aws: [] };
    const p2 = await open(browser, vp, th, `/aws/topology?level=groups&account=${GROUPS_ACCOUNT}&view=map`, w);
    await expect(p2.locator('[data-level="groups"]')).toBeVisible({ timeout: 30_000 });
    await settle(p2);
    await p2.locator('[data-group="tag:app=checkout"] button[aria-pressed]').first().click();
    await expect(p2.locator("[data-group-panel]")).toBeVisible({ timeout: 15_000 });
    await settle(p2);
    const file = await shot(p2, vp, th, "aws-topology-panel", "after-corrections");
    await review(p2, "panel after corrections", vp, th, w, { file });
  }
  record("side panel", { notes, http: watch.http, console: watch.console });
});

test("keyboard focus on pickers, search and side panel", async ({ browser }) => {
  for (const theme of THEMES) {
    const watch: Watch = { console: [], http: [], aws: [] };
    const page = await open(browser, DESKTOP, theme, `/aws/topology?level=groups&account=${GROUPS_ACCOUNT}`, watch);
    await expect(page.locator('[data-level="groups"]')).toBeVisible({ timeout: 30_000 });
    await settle(page);
    const ring = async (label: string) =>
      page.evaluate((l) => {
        const el = document.activeElement as HTMLElement | null;
        if (!el) return { label: l, tag: "none" };
        const st = getComputedStyle(el);
        return { label: l, tag: el.tagName, name: el.getAttribute("aria-label") ?? (el.textContent ?? "").trim().slice(0, 40), outline: `${st.outlineStyle} ${st.outlineWidth} ${st.outlineColor}`, shadow: st.boxShadow };
      }, label);
    const seen: unknown[] = [];
    // Tab from the breadcrumb through the scope bar.
    await page.locator("[data-scope-bar]").getByRole("button", { name: "Accounts" }).focus();
    await page.keyboard.press("Tab");
    await page.keyboard.press("Shift+Tab");
    seen.push(await ring("accounts picker"));
    await shotView(page, DESKTOP, theme, "aws-topology-focus-ring", "accounts-picker");
    await page.keyboard.press("Enter");
    await expect(page.getByRole("listbox", { name: "Accounts" })).toBeVisible();
    await page.keyboard.press("ArrowDown");
    await shotView(page, DESKTOP, theme, "aws-topology-focus-ring", "accounts-picker-open");
    await page.keyboard.press("Escape");
    await page.getByRole("searchbox", { name: "Search resources" }).focus();
    seen.push(await ring("search"));
    await shotView(page, DESKTOP, theme, "aws-topology-focus-ring", "search");
    // Reach the first group by keyboard and open its panel.
    const first = page.locator("[data-group] button[aria-pressed]").first();
    await first.focus();
    seen.push(await ring("group card"));
    await page.keyboard.press("Enter");
    await expect(page.locator("[data-group-panel]")).toBeVisible({ timeout: 15_000 });
    await page.locator("[data-group-panel]").getByRole("button", { name: "Rename" }).focus();
    seen.push(await ring("panel rename"));
    await shotView(page, DESKTOP, theme, "aws-topology-focus-ring", "panel-rename");
    await page.keyboard.press("Tab");
    seen.push(await ring("panel merge"));
    await shotView(page, DESKTOP, theme, "aws-topology-focus-ring", "panel-merge");
    record("keyboard focus", { theme, seen });
  }
});

test("removed account", async ({ browser }) => {
  const name = process.env.AWS_ESTATE_REMOVE ?? "";
  test.skip(!name, "needs AWS_ESTATE_REMOVE");
  test.setTimeout(240_000);
  const watch: Watch = { console: [], http: [], aws: [] };
  const page = await open(browser, DESKTOP, "dark", "/aws/settings", watch);
  const row = page.getByRole("list", { name: "AWS accounts" }).getByRole("listitem").filter({ hasText: name });
  await expect(row).toBeVisible({ timeout: 30_000 });
  await row.getByRole("button", { name: /remove/i }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Remove", exact: true }).click();
  await expect(row).toHaveCount(0, { timeout: 30_000 });
  for (const viewport of VIEWPORTS) {
    for (const theme of THEMES) {
      const w: Watch = { console: [], http: [], aws: [] };
      const p = await open(browser, viewport, theme, "/aws/topology?level=accounts", w);
      for (const pg of [PAGES[1], PAGES[0], PAGES[2]]) {
        await p.goto(pg.url);
        await expect(p.locator(pg.ready).first()).toBeVisible({ timeout: 30_000 });
        await settle(p);
        const file = await shot(p, viewport, theme, pg.surface, `removed-${name}`);
        await review(p, `${pg.surface} after remove`, viewport, theme, w, { file });
      }
    }
  }
});

/** The Accounts number a page shows ("4/5"), from its summary card or, on Topology, its account cards. */
test("accounts agree across pages", async ({ browser }) => {
  const watch: Watch = { console: [], http: [], aws: [] };
  const page = await open(browser, DESKTOP, "dark", "/aws/overview", watch);
  const fromCard = async (url: string) => {
    await page.goto(url);
    await expect(page.locator("[data-summary-card] [data-number]").first()).toBeVisible({ timeout: 30_000 });
    await settle(page);
    return page.evaluate(() => {
      for (const n of Array.from(document.querySelectorAll("[data-summary-card] [data-number]"))) {
        const label = (n.querySelector("dt")?.textContent ?? "").trim();
        if (label === "Accounts") return (n.querySelector("dd")?.textContent ?? "").replace(/\s+/g, " ").trim();
      }
      return "";
    });
  };
  const seen: Record<string, string> = {};
  seen.overview = await fromCard("/aws/overview");
  seen.coverage = await fromCard("/aws/coverage");
  seen.changes = await fromCard("/aws/changes");
  await page.goto("/aws/topology?level=accounts");
  await expect(page.locator('[data-level="accounts"]')).toBeVisible({ timeout: 30_000 });
  await settle(page);
  const cards = page.locator("[data-account-card][data-state]");
  const total = await cards.count();
  const read = await page.locator('[data-account-card][data-state="read"], [data-account-card][data-state="partial"]').count();
  seen.topology = `${read}/${total}`;
  seen.warnings = JSON.stringify({
    overview: await (async () => { await page.goto("/aws/overview"); await settle(page); return (await page.locator("[data-warning-line]").allInnerTexts()).join(" | "); })(),
    changes: await (async () => { await page.goto("/aws/changes"); await settle(page); return (await page.locator("[data-warning-line]").allInnerTexts()).join(" | "); })(),
  });
  record("accounts agree", { seen });
  expect(new Set([seen.overview, seen.coverage, seen.changes, seen.topology]).size, JSON.stringify(seen)).toBe(1);
});

test("map zoom buttons by keyboard", async ({ browser }) => {
  for (const theme of THEMES) {
    const watch: Watch = { console: [], http: [], aws: [] };
    const page = await open(browser, DESKTOP, theme, "/aws/topology?level=accounts", watch);
    await expect(page.locator('[data-level="accounts"]')).toBeVisible({ timeout: 30_000 });
    await settle(page);
    const zoom = page.getByRole("group", { name: "Zoom" });
    const steps: Record<string, unknown> = { open: await canvasProbe(page) };
    await zoom.getByRole("button", { name: "Zoom in" }).focus();
    await shotView(page, DESKTOP, theme, "aws-topology-zoom", "zoom-in-focused");
    await page.keyboard.press("Enter");
    await page.keyboard.press("Enter");
    await settle(page);
    steps.zoomedIn = await canvasProbe(page);
    await shotView(page, DESKTOP, theme, "aws-topology-zoom", "zoomed-in");
    // Zoom out, Zoom in, Fit: one Tab from Zoom in reaches Fit.
    await page.keyboard.press("Tab");
    const fitName = await page.evaluate(() => document.activeElement?.getAttribute("aria-label") ?? "");
    await shotView(page, DESKTOP, theme, "aws-topology-zoom", "fit-focused");
    await page.keyboard.press("Enter");
    await settle(page);
    steps.fit = await canvasProbe(page);
    steps.fitName = fitName;
    await shotView(page, DESKTOP, theme, "aws-topology-zoom", "after-fit");
    record("zoom", { theme, steps });
    expect(fitName).toBe("Fit the map to the view");
  }
});
