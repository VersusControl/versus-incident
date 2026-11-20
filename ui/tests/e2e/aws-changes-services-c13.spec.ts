// AWS Changes Services filter (A11 §10, task C13) on the real server.
//
// The stack is plans/harness-run/tests/aws/aws-changes.sh browser-up with
// FAKEAWS_CHANGE_SERVICES=1: tests/fakeaws mixes RDS and IAM writes among
// Config and Athena noise (and, on changes-busy, RDS writes among 5,000 Auto
// Scaling ones). Every boot starts from empty file storage, so the first test
// on a fresh boot sees a user who never chose. Nothing is intercepted except
// in the Logs list test, which says so: every other assertion is on what the
// server sent and the page rendered.
//
// Each test runs on the scenario the stack serves (AWS_CHANGES_BROWSER_DIR/
// scenario) and skips on the others; C13_GREP-style narrowing is Playwright's
// own --grep. Desktop 1440 only, light and dark (founder scope for C13).
//
// Run: E2E_AWS_CHANGES=1 AWS_CHANGES_BROWSER_DIR=… harness.sh e2e oss
//      tests/e2e/aws-changes-services-c13.spec.ts --project=chromium
import { expect, test, type Browser, type BrowserContext, type Page } from "@playwright/test";
import * as fs from "node:fs";
import * as path from "node:path";
import { openApp } from "./helpers";
import { capturePath, harnessResultsDir } from "./support/capture";

// Headless Chromium hides every scroll bar (--hide-scrollbars); a user's
// browser does not, and QA-518 is about whether the bar shows. Keep it.
test.use({ launchOptions: { ignoreDefaultArgs: ["--hide-scrollbars"] } });

const LIVE = process.env.E2E_AWS_CHANGES === "1";
test.skip(!LIVE, "needs the aws-changes browser stack (aws-changes.sh browser-up)");

const VIEWPORT = { width: 1440, height: 900 } as const;
const THEMES = ["light", "dark"] as const;
type Theme = (typeof THEMES)[number];
// C13_SURFACE names a recheck pass (e.g. c13b-aws-changes) so its images do not
// overwrite the first pass in local/illustration.
const SURFACE = process.env.C13_SURFACE?.trim() || "c13-aws-changes";
const PREFS = "/api/admin/aws/changes/preferences";

function stackScenario(): string {
  try {
    return fs.readFileSync(path.join(process.env.AWS_CHANGES_BROWSER_DIR ?? "", "scenario"), "utf8").trim();
  } catch {
    return "unknown";
  }
}
const STATE = stackScenario();
const onScenario = (...names: string[]) => test.skip(!names.includes(STATE), `runs on ${names.join(" or ")}; stack serves ${STATE}`);

const BANNED: Array<[string, RegExp]> = [
  ["Partial", /\bpartial\b/i],
  ["at least", /\bat least\b/i],
  ["N read", /\b\d[\d,]* read\b/i],
  ["N+", /\b\d[\d,]*\+(?![\d])/],
  ["still arriving", /still arriving/i],
  ["Suspect", /\bsuspect/i],
  ["Read HH:MM", /\bRead \d{1,2}:\d{2}\b/],
  ["Pipeline", /\bPipeline\b/],
];
const RAW = ["[object Object]", "undefined", "NaN", "cloudtrail_denied", "first_read_pending", "in_filter", "AccessDeniedException"];

function banned(where: string, text: string): string[] {
  const out: string[] = [];
  for (const [name, re] of BANNED) {
    const m = text.match(re);
    if (m) out.push(`banned "${name}" in ${where}: "…${text.slice(Math.max(0, (m.index ?? 0) - 40), (m.index ?? 0) + 40).replace(/\s+/g, " ")}…"`);
  }
  for (const code of RAW) if (text.includes(code)) out.push(`raw "${code}" in ${where}`);
  return out;
}

function record(name: string, data: Record<string, unknown>) {
  const file = path.join(harnessResultsDir(), "aws-changes-services-c13.jsonl");
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.appendFileSync(file, JSON.stringify({ test: name, state: STATE, at: new Date().toISOString(), ...data }) + "\n");
}

interface Watch {
  console: string[];
  http: string[];
  /** Every /api/admin/aws/changes and /compare request the page sent. */
  requests: string[];
  /** Services of every item in every changes answer the page received. */
  itemServices: Set<string>;
  compareServices: Set<string>;
}
const newWatch = (): Watch => ({ console: [], http: [], requests: [], itemServices: new Set(), compareServices: new Set() });

const contexts: BrowserContext[] = [];
test.afterEach(async () => {
  for (const c of contexts.splice(0)) await c.close().catch(() => {});
});

async function newContext(browser: Browser, theme: Theme): Promise<BrowserContext> {
  const context = await browser.newContext({ viewport: VIEWPORT, colorScheme: theme });
  contexts.push(context);
  await context.addInitScript((value) => localStorage.setItem("versus.theme", value), theme);
  return context;
}

function watchPage(page: Page, watch: Watch) {
  page.on("console", (m) => {
    if (m.type() === "error" && !(m.location()?.url ?? "").includes("/enterprise/api/sso/deployment")) watch.console.push(m.text().slice(0, 200));
  });
  page.on("request", (r) => {
    const u = new URL(r.url());
    if (u.pathname === "/api/admin/aws/changes" || u.pathname === "/api/admin/aws/changes/compare") watch.requests.push(u.pathname + u.search);
  });
  page.on("response", async (r) => {
    const u = new URL(r.url());
    if (u.pathname.startsWith("/api/") && r.status() >= 400 && !u.pathname.includes("/enterprise/api/sso/deployment")) watch.http.push(`${r.status()} ${u.pathname}${u.search}`);
    if (r.status() !== 200) return;
    try {
      if (u.pathname === "/api/admin/aws/changes") {
        const body = (await r.json()) as { items?: Array<{ service: string }> };
        for (const it of body.items ?? []) watch.itemServices.add(it.service);
      } else if (u.pathname === "/api/admin/aws/changes/compare") {
        const body = (await r.json()) as Record<string, unknown>;
        for (const k of ["added", "removed", "changed", "changes_between"]) {
          for (const it of (body[k] as Array<{ service?: string }> | null) ?? []) if (it.service) watch.compareServices.add(it.service);
        }
      }
    } catch {
      /* a body already gone is not this test's concern */
    }
  });
}

async function open(browser: Browser, theme: Theme, url: string, watch: Watch): Promise<Page> {
  const context = await newContext(browser, theme);
  const page = await context.newPage();
  watchPage(page, watch);
  await page.emulateMedia({ colorScheme: theme });
  await openApp(page, url);
  watch.console.length = 0;
  watch.http.length = 0;
  await page.goto(url);
  await settled(page);
  return page;
}

async function settled(page: Page) {
  await expect(page.locator("[data-summary-card]")).toBeVisible({ timeout: 30_000 });
  await page.waitForLoadState("networkidle").catch(() => {});
}

async function shot(page: Page, theme: Theme, state: string) {
  const out = capturePath(SURFACE, VIEWPORT.width, theme, state);
  const extra = await page.evaluate(() => {
    const m = document.querySelector("main");
    return m ? Math.max(0, m.scrollHeight - m.clientHeight) : 0;
  });
  if (extra > 0) await page.setViewportSize({ width: VIEWPORT.width, height: VIEWPORT.height + extra });
  await page.waitForTimeout(250);
  await page.screenshot({ path: out, animations: "disabled", fullPage: true });
  if (extra > 0) await page.setViewportSize(VIEWPORT);
  return out;
}

async function shotView(page: Page, theme: Theme, state: string) {
  const out = capturePath(SURFACE, VIEWPORT.width, theme, state);
  await page.waitForTimeout(200);
  await page.screenshot({ path: out, animations: "disabled" });
  return out;
}

const servicesButton = (page: Page) => page.locator("[data-services-filter]").getByRole("button", { name: /^Services/ });
const servicesDialog = (page: Page) => page.getByRole("dialog", { name: "Services" });

/** The Services panel as a user reads it: each option's label, count and check. */
async function readPanel(page: Page) {
  if (!(await servicesDialog(page).isVisible())) await servicesButton(page).click();
  const dialog = servicesDialog(page);
  await expect(dialog).toBeVisible();
  return dialog.locator("li label").evaluateAll((labels) =>
    labels.map((l) => {
      const spans = l.querySelectorAll("span");
      return {
        label: (spans[0]?.textContent ?? "").trim(),
        count: (spans[1]?.textContent ?? "").trim(),
        checked: (l.querySelector("input[type=checkbox]") as HTMLInputElement | null)?.checked ?? false,
      };
    }),
  );
}

async function closePanel(page: Page) {
  if (await servicesDialog(page).isVisible()) {
    await page.keyboard.press("Escape");
    await expect(servicesDialog(page)).toBeHidden();
  }
}

async function toggle(page: Page, label: string) {
  if (!(await servicesDialog(page).isVisible())) await servicesButton(page).click();
  await servicesDialog(page).locator("li label").filter({ hasText: new RegExp(`^${label}\\s*[\\d,]+$`) }).locator("input").click();
}

async function card(page: Page) {
  return page.evaluate(() => {
    const t = (e: Element | null | undefined) => ((e as HTMLElement | null)?.innerText ?? "").replace(/\s+/g, " ").trim();
    const c = document.querySelector("[data-summary-card]");
    const num = (k: string) => t(c?.querySelector(`[data-number="${k}"] dd`));
    return {
      text: t(c),
      filter: t(c?.querySelector("[data-service-filter]")).replace(/^Service filter:\s*/, ""),
      changes: num("changes"),
      people: num("people"),
      pipeline: num("pipeline"),
      awsService: num("aws_service"),
      noChanges: t(c?.querySelector("[data-no-changes]")),
      warning: t(c?.querySelector("[data-warning-line]")),
      likely: t(c?.querySelector("[data-likely-cause]")),
      outside: t(c?.querySelector("[data-outside-filter]")),
      rowTotals: Array.from(document.querySelectorAll<HTMLElement>("[data-row]")).map((r) => ({
        row: r.getAttribute("data-row"),
        total: t(r.querySelector("[data-row-total]")),
      })),
      items: Array.from(document.querySelectorAll<HTMLElement>("[data-list-scroll] [data-item]")).map((e) => e.getAttribute("data-item") ?? ""),
      events: Array.from(document.querySelectorAll<HTMLElement>("[data-list-scroll] [data-event-name]")).map((e) => t(e)),
      compare: t(document.querySelector('[data-panel="compare"]')),
      chips: Array.from(document.querySelectorAll<HTMLElement>('[aria-label="Applied filters"] .pill')).map((e) => t(e)),
    };
  });
}

type Prefs = { services: string[] };
async function prefs(page: Page): Promise<Prefs> {
  const r = await page.request.get(PREFS);
  expect(r.status()).toBe(200);
  return (await r.json()) as Prefs;
}

type ChangesAnswer = {
  summary: { total: number; by_actor_class: Record<string, number>; errors: number };
  items: Array<{ id: string; service: string; kind: string }>;
  services: Array<{ service: string; label: string; count: number }>;
  buckets: Array<{ by_actor_class?: Record<string, number>; total?: number; count?: number }>;
  suspect: { status: string; in_filter?: boolean; change?: { id: string; service: string } | null };
  gaps: unknown[];
  next_cursor: string | null;
};
/** Saves a preference from the page, the way the app does (same origin, session cookie). Setup only. */
async function putPrefs(page: Page, services: string[]): Promise<number> {
  return page.evaluate(async (body) => {
    const r = await fetch("/api/admin/aws/changes/preferences", { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body), credentials: "same-origin" });
    return r.status;
  }, { services });
}

async function changesApi(page: Page, query: string): Promise<ChangesAnswer> {
  const r = await page.request.get(`/api/admin/aws/changes${query}`);
  expect(r.status(), `GET /api/admin/aws/changes${query}`).toBe(200);
  return (await r.json()) as ChangesAnswer;
}

const fmt = (n: number) => n.toLocaleString("en-US");

// ---------------------------------------------------------------------------
// 1. Fresh install: RDS and IAM checked; every other service listed with its
// count; card, grid, list and Compare narrowed; the card names "RDS, IAM".

test("1 fresh install: RDS and IAM by default", async ({ browser }) => {
  onScenario("bad-deploy");
  test.setTimeout(180_000);
  const problems: string[] = [];
  const results: Record<string, unknown> = {};
  for (const theme of THEMES) {
    const watch = newWatch();
    const page = await open(browser, theme, "/aws/changes", watch);
    const saved = await prefs(page);
    if (JSON.stringify(saved.services) !== JSON.stringify(["rds", "iam"])) problems.push(`${theme}: preferences ${JSON.stringify(saved)} on a fresh install`);
    const all = await changesApi(page, "");
    const filtered = await changesApi(page, "?service=rds,iam");
    const c = await card(page);
    if (c.filter !== "RDS, IAM") problems.push(`${theme}: card names "${c.filter}", want "RDS, IAM"`);
    if (c.changes !== fmt(filtered.summary.total)) problems.push(`${theme}: card Changes ${c.changes} vs API (rds,iam) ${filtered.summary.total}`);
    if (filtered.summary.total >= all.summary.total) problems.push(`${theme}: filtered total ${filtered.summary.total} not below all ${all.summary.total}`);
    for (const r of c.rowTotals) {
      const want = r.row === "errors" ? filtered.summary.errors : filtered.summary.by_actor_class[r.row ?? ""];
      if (want !== undefined && r.total !== fmt(want)) problems.push(`${theme}: grid row ${r.row} total ${r.total} vs filtered ${want}`);
    }
    const bucketSum = filtered.buckets.reduce((s, b) => s + Object.values(b.by_actor_class ?? {}).reduce((a, v) => a + (v as number), 0), 0);
    if (bucketSum !== filtered.summary.total - 0 && bucketSum !== 0) results[`${theme}-bucketSum`] = bucketSum;
    const sent = watch.requests.filter((r) => r.startsWith("/api/admin/aws/changes"));
    const unfiltered = sent.filter((r) => !/[?&]service=rds(%2C|,)iam\b/.test(r));
    if (unfiltered.length) problems.push(`${theme}: page sent requests without service=rds,iam: ${unfiltered.slice(0, 3).join(" | ")}`);
    const stray = [...watch.itemServices].filter((s) => s !== "rds" && s !== "iam");
    if (stray.length) problems.push(`${theme}: list answer carried ${stray.join(", ")}`);
    const strayCompare = [...watch.compareServices].filter((s) => s !== "rds" && s !== "iam");
    if (strayCompare.length) problems.push(`${theme}: compare answer carried ${strayCompare.join(", ")}`);
    if (c.items.length !== filtered.items.length) problems.push(`${theme}: ${c.items.length} rows shown vs ${filtered.items.length} items`);
    // The panel lists every service with changes, counted without the filter.
    const panel = await readPanel(page);
    for (const s of all.services) {
      const opt = panel.find((o) => o.label === s.label);
      if (!opt) problems.push(`${theme}: panel lacks ${s.label}`);
      else {
        if (opt.count !== fmt(s.count)) problems.push(`${theme}: panel ${s.label} ${opt.count} vs API ${s.count}`);
        const want = s.service === "rds" || s.service === "iam";
        if (opt.checked !== want) problems.push(`${theme}: ${s.label} checked=${opt.checked}`);
      }
    }
    if (panel.length !== all.services.length) problems.push(`${theme}: panel lists ${panel.length} services, API ${all.services.length}`);
    const panelText = await servicesDialog(page).innerText();
    problems.push(...banned("Services panel", panelText).map((p) => `${theme}: ${p}`));
    const panelShot = await shotView(page, theme, `${STATE}-fresh-services-open`);
    await closePanel(page);
    const full = await shot(page, theme, `${STATE}-fresh-rds-iam`);
    problems.push(...banned("page", await page.locator("main").innerText()).map((p) => `${theme}: ${p}`));
    if (watch.console.length) problems.push(`${theme}: console errors ${watch.console.join(" | ")}`);
    if (watch.http.length) problems.push(`${theme}: http ${watch.http.join(" | ")}`);
    results[theme] = { saved, card: c, panel, apiAll: all.services, filteredTotal: filtered.summary.total, allTotal: all.summary.total, compareServices: [...watch.compareServices], files: [panelShot, full] };
  }
  record("fresh-install", { results, problems });
  expect(problems).toEqual([]);
});

// ---------------------------------------------------------------------------
// 1c. "The filter list shows every service with changes": every option, and
// every checked one, is on screen when the panel opens, not behind an inner
// scroll with no visible bar.

/** Geometry of the Services option list as the user sees it. */
async function measureList(page: Page) {
  return servicesDialog(page).evaluate((d) => {
    const ul = d.querySelector("ul") as HTMLElement | null;
    const visible = (el: Element) => {
      const r = el.getBoundingClientRect();
      const b = (ul ?? d).getBoundingClientRect();
      return r.top >= b.top - 1 && r.bottom <= b.bottom + 1;
    };
    const text = (r: Element) => (r as HTMLElement).innerText.replace(/\s+/g, " ").trim();
    const rows = Array.from(d.querySelectorAll("li label"));
    const checked = rows.map((r) => (r.querySelector("input") as HTMLInputElement).checked);
    const firstUnchecked = checked.indexOf(false);
    const fade = d.querySelector("[data-more-below]") as HTMLElement | null;
    const cs = ul ? getComputedStyle(ul) : null;
    return {
      options: rows.length,
      order: rows.map(text),
      checkedFirst: firstUnchecked === -1 || checked.slice(firstUnchecked).every((c) => !c),
      listHeight: ul?.clientHeight ?? 0,
      listMaxHeight: cs?.maxHeight ?? "",
      listScrollHeight: ul?.scrollHeight ?? 0,
      scrollTop: ul?.scrollTop ?? 0,
      overflowY: cs?.overflowY ?? "",
      scrollVisibleClass: ul?.classList.contains("scroll-visible") ?? false,
      hidden: rows.filter((r) => !visible(r)).map(text),
      hiddenChecked: rows.filter((r) => !visible(r) && (r.querySelector("input") as HTMLInputElement).checked).map(text),
      scrollbarWidth: ul ? ul.offsetWidth - ul.clientWidth : 0,
      fade: !!fade && fade.getBoundingClientRect().height > 0,
    };
  });
}

const LIST_CAP_PX = 240;

test("1c panel shows every service without a hidden inner scroll", async ({ browser }) => {
  onScenario("bad-deploy");
  test.setTimeout(120_000);
  const problems: string[] = [];
  const results: Record<string, unknown> = {};
  for (const theme of THEMES) {
    const watch = newWatch();
    // Live: a shared link that checks the last service in the server order.
    const page = await open(browser, theme, "/aws/changes?service=s3,ec2", watch);
    await servicesButton(page).click();
    const m = await measureList(page);
    if (m.hidden.length) problems.push(`${theme}: ${m.hidden.length} of ${m.options} services hidden behind a ${m.listHeight}px inner scroll (${m.hidden.join("; ")})`);
    if (m.hiddenChecked.length) problems.push(`${theme}: a checked service is out of sight: ${m.hiddenChecked.join("; ")}`);
    if (m.listMaxHeight !== `${LIST_CAP_PX}px`) problems.push(`${theme}: list cap ${m.listMaxHeight}, want ${LIST_CAP_PX}px`);
    if (m.listHeight > LIST_CAP_PX) problems.push(`${theme}: list ${m.listHeight}px over the ${LIST_CAP_PX}px cap`);
    if (!m.checkedFirst) problems.push(`${theme}: checked services not first: ${m.order.join("; ")}`);
    if (!/^(S3|EC2) /.test(m.order[0] ?? "") || !/^(S3|EC2) /.test(m.order[1] ?? "")) problems.push(`${theme}: first two rows ${m.order.slice(0, 2).join("; ")}, want S3 and EC2`);
    if (m.fade) problems.push(`${theme}: fade shown with every row in sight`);
    const file = await shotView(page, theme, `${STATE}-link-s3-ec2-panel-open`);
    await closePanel(page);
    results[theme] = { live: m, file };
    await page.context().close();

    // Fixture (labelled): a real account lists about 30 services. The live
    // changes answer is fetched and only its `services` list is padded, so the
    // panel has more rows than the cap; every other field is the server's.
    const fx = newWatch();
    const context = await newContext(browser, theme);
    const pad = ["Auto Scaling", "CloudFormation", "CloudFront", "CloudWatch", "Cognito", "DynamoDB", "EKS", "ElastiCache", "ELB", "EventBridge", "KMS", "Kinesis", "Lambda", "Route 53", "Secrets Manager", "SNS", "SQS", "SSM", "STS", "WAF", "Glue", "Redshift", "Backup"];
    await context.route(/\/api\/admin\/aws\/changes(\?|$)/, async (route) => {
      if (route.request().method() !== "GET") return route.continue();
      const res = await route.fetch();
      const body = (await res.json()) as ChangesAnswer;
      const have = new Set((body.services ?? []).map((s) => s.service));
      const extra = pad
        .map((label, i) => ({ service: label.toLowerCase().replace(/[^a-z0-9]+/g, ""), label, count: 1 + i }))
        .filter((s) => !have.has(s.service));
      await route.fulfill({ response: res, json: { ...body, services: [...(body.services ?? []), ...extra] } });
    });
    const p2 = await context.newPage();
    watchPage(p2, fx);
    await p2.emulateMedia({ colorScheme: theme });
    await openApp(p2, "/aws/changes?service=s3,ec2");
    // The sign-in screen probes the agent config before a session exists (401); not this page's.
    fx.console.length = 0;
    fx.http.length = 0;
    await p2.goto("/aws/changes?service=s3,ec2");
    await settled(p2);
    await servicesButton(p2).click();
    const top = await measureList(p2);
    if (top.options < 25) problems.push(`${theme} fixture: only ${top.options} rows`);
    if (top.listHeight !== LIST_CAP_PX) problems.push(`${theme} fixture: list ${top.listHeight}px, want the ${LIST_CAP_PX}px cap`);
    if (!top.checkedFirst || !/^(S3|EC2) /.test(top.order[0] ?? "") || !/^(S3|EC2) /.test(top.order[1] ?? "")) problems.push(`${theme} fixture: checked not first: ${top.order.slice(0, 4).join("; ")}`);
    if (top.hiddenChecked.length) problems.push(`${theme} fixture: checked out of sight: ${top.hiddenChecked.join("; ")}`);
    if (top.scrollbarWidth < 6) problems.push(`${theme} fixture: no visible scroll bar (gutter ${top.scrollbarWidth}px)`);
    if (!top.fade) problems.push(`${theme} fixture: no fade while rows sit below`);
    if (!top.hidden.length) problems.push(`${theme} fixture: nothing below the cap; the case is not exercised`);
    const topShot = await shotView(p2, theme, `${STATE}-many-services-panel-top`);
    // The user scrolls the list with the wheel until the end.
    const ul = servicesDialog(p2).locator("ul");
    await ul.hover();
    for (let i = 0; i < 12; i++) await p2.mouse.wheel(0, 200);
    await p2.waitForTimeout(300);
    const bottom = await measureList(p2);
    const last = top.order[top.order.length - 1];
    if (bottom.hidden.includes(last)) problems.push(`${theme} fixture: last row "${last}" not reachable by scrolling`);
    if (bottom.fade) problems.push(`${theme} fixture: fade still shown at the end of the list`);
    const bottomShot = await shotView(p2, theme, `${STATE}-many-services-panel-bottom`);
    // Search reaches a low row too.
    await servicesDialog(p2).getByLabel("Filter services values").fill("backup");
    const searched = await measureList(p2);
    if (searched.order.length !== 1 || !/^Backup /.test(searched.order[0] ?? "") || searched.hidden.length) problems.push(`${theme} fixture: search "backup" shows ${searched.order.join("; ")}`);
    await closePanel(p2);
    if (fx.console.length) problems.push(`${theme} fixture: console ${fx.console.join(" | ")}`);
    results[`${theme}-fixture`] = { top, bottom, searched: searched.order, files: [topShot, bottomShot] };
    if (watch.console.length) problems.push(`${theme}: console ${watch.console.join(" | ")}`);
  }
  record("panel-inner-scroll", { results, problems });
  expect(problems).toEqual([]);
});

// ---------------------------------------------------------------------------
// 9b (run here, before any save): equal heights with the filter on, and the
// panel and chips by keyboard (8).

test("8 keyboard: open, toggle and close the panel", async ({ browser }) => {
  onScenario("bad-deploy");
  test.setTimeout(120_000);
  const problems: string[] = [];
  const watch = newWatch();
  const theme: Theme = "dark";
  const page = await open(browser, theme, "/aws/changes", watch);
  const before = await prefs(page);
  // Reach the Services button by Tab from the page heading.
  await page.locator("main").focus().catch(() => {});
  await page.locator("main h1, main [data-summary-card]").first().click({ position: { x: 1, y: 1 } }).catch(() => {});
  let reached = false;
  for (let i = 0; i < 40 && !reached; i++) {
    await page.keyboard.press("Tab");
    reached = await servicesButton(page).evaluate((b) => b === document.activeElement);
  }
  if (!reached) problems.push("Services button not reachable by Tab");
  await page.keyboard.press("Enter");
  const dialog = servicesDialog(page);
  if (!(await dialog.isVisible())) problems.push("Enter does not open the panel");
  const expanded = await servicesButton(page).getAttribute("aria-expanded");
  if (expanded !== "true") problems.push(`aria-expanded=${expanded} while open`);
  // Tab into the panel: Select all, search, then the checkboxes.
  const order: string[] = [];
  for (let i = 0; i < 6; i++) {
    await page.keyboard.press("Tab");
    order.push(
      await page.evaluate(() => {
        const a = document.activeElement as HTMLElement | null;
        if (!a) return "none";
        const inDialog = Boolean(a.closest('[role="dialog"]'));
        const name = a.getAttribute("aria-label") || (a as HTMLInputElement).type || a.innerText;
        return `${inDialog ? "in" : "OUT"}:${a.tagName.toLowerCase()}:${(name ?? "").trim().slice(0, 30)}`;
      }),
    );
  }
  // Focus the first checkbox by Tab and toggle it with Space.
  await servicesButton(page).focus();
  await page.keyboard.press("Enter").catch(() => {});
  if (!(await dialog.isVisible())) await servicesButton(page).click();
  let onBox = false;
  for (let i = 0; i < 8 && !onBox; i++) {
    await page.keyboard.press("Tab");
    onBox = await page.evaluate(() => (document.activeElement as HTMLInputElement | null)?.type === "checkbox");
  }
  if (!onBox) problems.push("no checkbox reachable by Tab inside the panel");
  const boxLabel = await page.evaluate(() => (document.activeElement?.closest("label") as HTMLElement | null)?.innerText.replace(/\s+/g, " ").trim() ?? "");
  const wasChecked = await page.evaluate(() => (document.activeElement as HTMLInputElement).checked);
  await page.keyboard.press("Space");
  await page.waitForTimeout(150);
  const nowChecked = await page.evaluate(() => (document.activeElement as HTMLInputElement | null)?.checked);
  if (nowChecked === wasChecked) problems.push(`Space did not toggle "${boxLabel}"`);
  const focusRing = await page.evaluate(() => {
    const a = document.activeElement as HTMLElement | null;
    if (!a) return "";
    const s = getComputedStyle(a);
    return `${s.outlineStyle} ${s.outlineWidth} ${s.boxShadow}`;
  });
  const toggledShot = await shotView(page, theme, `${STATE}-keyboard-toggled`);
  await page.keyboard.press("Escape");
  if (await dialog.isVisible()) problems.push("Escape does not close the panel");
  const focusBack = await servicesButton(page).evaluate((b) => b === document.activeElement);
  if (!focusBack) problems.push("focus does not return to the Services button after Escape");
  // Toggle back so the saved choice is RDS and IAM again.
  await servicesButton(page).press("Enter");
  for (let i = 0; i < 8; i++) {
    await page.keyboard.press("Tab");
    const here = await page.evaluate(() => (document.activeElement?.closest("label") as HTMLElement | null)?.innerText.replace(/\s+/g, " ").trim() ?? "");
    if (here === boxLabel) break;
  }
  await page.keyboard.press("Space");
  await page.keyboard.press("Escape");
  await page.waitForTimeout(900);
  const after = await prefs(page);
  if (JSON.stringify(after.services.slice().sort()) !== JSON.stringify(before.services.slice().sort())) problems.push(`toggle and back left preferences ${JSON.stringify(after)} (was ${JSON.stringify(before)})`);
  record("keyboard", { problems, order, boxLabel, wasChecked, nowChecked, focusRing, before, after, toggledShot });
  expect(problems).toEqual([]);
});

test("9 regression: equal heights and banned words with the filter on", async ({ browser }) => {
  onScenario("bad-deploy");
  test.setTimeout(120_000);
  const problems: string[] = [];
  const results: Record<string, unknown> = {};
  for (const theme of THEMES) {
    const watch = newWatch();
    const page = await open(browser, theme, "/aws/changes", watch);
    const m = await page.evaluate(() => {
      const box = (sel: string) => {
        const e = document.querySelector<HTMLElement>(sel);
        if (!e) return null;
        const b = e.getBoundingClientRect();
        return { top: Math.round(b.top), height: Math.round(b.height) };
      };
      return {
        changes: box('[data-panel="changes"]'),
        compare: box('[data-panel="compare"]'),
        changesCard: box('[data-panel="changes"] > section'),
        compareCard: box('[data-panel="compare"] > section'),
      };
    });
    if (!m.changes || !m.compare) problems.push(`${theme}: panels missing`);
    else {
      if (Math.abs(m.changes.height - m.compare.height) > 1) problems.push(`${theme}: Changes ${m.changes.height}px vs Compare ${m.compare.height}px`);
      if (m.changes.top !== m.compare.top) problems.push(`${theme}: panels not side by side`);
    }
    if (m.changesCard && m.changes && Math.abs(m.changesCard.height - m.changes.height) > 1) problems.push(`${theme}: Changes card does not fill its slot`);
    if (m.compareCard && m.compare && Math.abs(m.compareCard.height - m.compare.height) > 1) problems.push(`${theme}: Compare card does not fill its slot`);
    problems.push(...banned("page", await page.locator("main").innerText()).map((p) => `${theme}: ${p}`));
    // Popovers: the warning line's, if any, and the Services panel.
    const warn = page.locator("[data-warning-line] button[aria-haspopup='dialog']");
    if (await warn.count()) {
      await warn.first().click();
      problems.push(...banned("warning popover", await page.getByRole("dialog").last().innerText()).map((p) => `${theme}: ${p}`));
      await page.keyboard.press("Escape");
    }
    await servicesButton(page).click();
    problems.push(...banned("Services panel", await servicesDialog(page).innerText()).map((p) => `${theme}: ${p}`));
    await closePanel(page);
    if (watch.console.length) problems.push(`${theme}: console ${watch.console.join(" | ")}`);
    results[theme] = m;
  }
  record("regression-heights-words", { results, problems });
  expect(problems).toEqual([]);
});

// ---------------------------------------------------------------------------
// 2. Change and persistence: reload, sign-out and sign-in, another browser,
// and the API.

test("2 change persists: reload, sign out and in, second browser, API", async ({ browser }) => {
  onScenario("bad-deploy");
  test.setTimeout(180_000);
  const problems: string[] = [];
  const theme: Theme = "light";
  const watch = newWatch();
  const page = await open(browser, theme, "/aws/changes", watch);
  const start = await prefs(page);
  if (start.services.slice().sort().join(",") !== "iam,rds") {
    // Rerun on a used stack: back to the default first (setup, not under test).
    expect(await putPrefs(page, ["rds", "iam"])).toBe(200);
    await page.reload();
    await settled(page);
  }
  // A user hides IAM and adds Config and ECS.
  await toggle(page, "IAM");
  await toggle(page, "Config");
  await toggle(page, "ECS");
  await closePanel(page);
  await page.waitForLoadState("networkidle").catch(() => {});
  await page.waitForTimeout(800);
  const want = ["rds", "config", "ecs"];
  const sorted = (a: string[]) => a.slice().sort().join(",");
  const api1 = await prefs(page);
  if (sorted(api1.services) !== sorted(want)) problems.push(`API after change ${JSON.stringify(api1)}`);
  const changed = await card(page);
  if (changed.filter !== "RDS, Config, ECS") problems.push(`card names "${changed.filter}" after change`);
  const changedShot = await shot(page, theme, `${STATE}-changed-rds-config-ecs`);
  const checkedLabels = async (p: Page) => (await readPanel(p)).filter((o) => o.checked).map((o) => o.label).sort().join(",");
  const wantLabels = ["Config", "ECS", "RDS"].join(",");
  // Reload.
  await page.reload();
  await settled(page);
  const afterReload = await checkedLabels(page);
  await closePanel(page);
  if (afterReload !== wantLabels) problems.push(`after reload checked ${afterReload}`);
  if ((await card(page)).filter !== "RDS, Config, ECS") problems.push("card filter after reload");
  // Sign out and in.
  await page.getByTestId("sign-out").click();
  await expect(page.getByLabel("Gateway secret")).toBeVisible({ timeout: 30_000 });
  await openApp(page, "/aws/changes");
  // The sign-in screen probes the agent config before a session exists (401); not this page's.
  watch.console.length = 0;
  watch.http.length = 0;
  await page.goto("/aws/changes");
  await settled(page);
  const afterSignIn = await checkedLabels(page);
  await closePanel(page);
  if (afterSignIn !== wantLabels) problems.push(`after sign-out/in checked ${afterSignIn}`);
  // A second browser (fresh context, no shared storage).
  const watch2 = newWatch();
  const other = await open(browser, "dark", "/aws/changes", watch2);
  const second = await checkedLabels(other);
  await closePanel(other);
  if (second !== wantLabels) problems.push(`second browser checked ${second}`);
  const secondCard = await card(other);
  if (secondCard.filter !== "RDS, Config, ECS") problems.push(`second browser card names "${secondCard.filter}"`);
  const secondShot = await shot(other, "dark", `${STATE}-second-browser-rds-config-ecs`);
  const stray = [...watch2.itemServices].filter((s) => !want.includes(s));
  if (stray.length) problems.push(`second browser list carried ${stray.join(", ")}`);
  // Worst condition (QA-517): a change and an immediate reload, inside the
  // 400 ms save debounce. Every PUT the browser sends is counted at the
  // context, so a request from the unloading document is seen too.
  const puts: string[] = [];
  const countPuts = (r: import("@playwright/test").Request) => {
    if (r.method() === "PUT" && new URL(r.url()).pathname === PREFS) puts.push(r.postData() ?? "");
  };
  const ctx = page.context();
  ctx.on("request", countPuts);
  await toggle(page, "Athena");
  await page.reload();
  await settled(page);
  await page.waitForTimeout(1_000);
  const quick = await prefs(page);
  const quickKept = quick.services.includes("athena");
  const quickPuts = puts.splice(0);
  const quickChecked = await checkedLabels(page);
  await closePanel(page);
  if (!quickKept) problems.push(`tick then reload lost: ${JSON.stringify(quick)}`);
  if (quickPuts.length !== 1) problems.push(`tick then reload sent ${quickPuts.length} PUTs: ${quickPuts.join(" | ")}`);
  if (quickChecked !== ["Athena", "Config", "ECS", "RDS"].join(",")) problems.push(`after quick reload checked ${quickChecked}`);
  // The same with the tab closed at once (Cmd-W): a second tab in the same
  // browser ticks Athena off and closes.
  // Playwright reports no request from a closed page, so the tab also counts
  // the PUTs the app hands to fetch, in localStorage (written synchronously,
  // before the page goes; the network-level count above covers the reload).
  const tab = await ctx.newPage();
  await tab.addInitScript((prefsPath) => {
    const orig = window.fetch.bind(window);
    window.fetch = (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      const method = (init?.method ?? (input instanceof Request ? input.method : "GET")).toUpperCase();
      if (method === "PUT" && new URL(url, location.href).pathname === prefsPath) {
        const log = JSON.parse(localStorage.getItem("c13b.puts") ?? "[]") as Array<{ body: string; keepalive: boolean }>;
        log.push({ body: String(init?.body ?? ""), keepalive: !!init?.keepalive });
        localStorage.setItem("c13b.puts", JSON.stringify(log));
      }
      return orig(input, init);
    };
  }, PREFS);
  await tab.goto("/aws/changes");
  await settled(tab);
  await tab.evaluate(() => localStorage.removeItem("c13b.puts"));
  puts.length = 0;
  await toggle(tab, "Athena");
  await tab.close();
  await page.waitForTimeout(1_500);
  const closed = await prefs(page);
  const closeKept = !closed.services.includes("athena");
  const closeNet = puts.splice(0);
  const closePuts = (await page.evaluate(() => {
    const v = localStorage.getItem("c13b.puts");
    localStorage.removeItem("c13b.puts");
    return JSON.parse(v ?? "[]");
  })) as Array<{ body: string; keepalive: boolean }>;
  if (closePuts.length === 1 && !closePuts[0].keepalive) problems.push("tick then tab close: the PUT was not a keepalive request");
  if (!closeKept) problems.push(`tick then tab close lost: ${JSON.stringify(closed)}`);
  if (closePuts.length !== 1) problems.push(`tick then tab close sent ${closePuts.length} PUTs: ${JSON.stringify(closePuts)}`);
  // A normal pause sends one PUT as before (debounce kept).
  await page.reload();
  await settled(page);
  const afterClose = await checkedLabels(page);
  await closePanel(page);
  if (afterClose !== wantLabels) problems.push(`after tab close, reload shows ${afterClose}`);
  ctx.off("request", countPuts);
  const final = await prefs(page);
  if (watch.console.length || watch2.console.length) problems.push(`console ${[...watch.console, ...watch2.console].join(" | ")}`);
  if (watch.http.length || watch2.http.length) problems.push(`http ${[...watch.http, ...watch2.http].join(" | ")}`);
  if (sorted(final.services) !== sorted(want)) problems.push(`final preferences ${JSON.stringify(final)}`);
  record("persistence", { problems, api1, afterReload, afterSignIn, second, quickKept, quick, quickPuts, quickChecked, closeKept, closed, closePuts, closeNet, afterClose, final, files: [changedShot, secondShot] });
  expect(problems).toEqual([]);
});

// ---------------------------------------------------------------------------
// 3. A shared `service=` link shows that view without saving; an explicit
// change on that view saves.

test("3 shared link: shows without saving; an explicit change saves", async ({ browser }) => {
  onScenario("bad-deploy");
  test.setTimeout(120_000);
  const problems: string[] = [];
  const theme: Theme = "dark";
  const watch = newWatch();
  const page = await open(browser, theme, "/aws/changes", watch);
  const before = await prefs(page);
  await page.goto("/aws/changes?service=s3,ec2");
  await settled(page);
  const c = await card(page);
  if (c.filter !== "S3, EC2") problems.push(`link view names "${c.filter}"`);
  const panel = await readPanel(page);
  const checked = panel.filter((o) => o.checked).map((o) => o.label).sort().join(",");
  if (checked !== "EC2,S3") problems.push(`link view checked ${checked}`);
  const linkShot = await shotView(page, theme, `${STATE}-link-s3-ec2-open`);
  await closePanel(page);
  const linkFull = await shot(page, theme, `${STATE}-link-s3-ec2`);
  await page.waitForTimeout(800);
  const during = await prefs(page);
  if (JSON.stringify(during) !== JSON.stringify(before)) problems.push(`opening the link saved ${JSON.stringify(during)}`);
  await page.reload();
  await settled(page);
  if ((await card(page)).filter !== "S3, EC2") problems.push("link view lost on reload");
  // Without the link: the saved choice.
  await page.goto("/aws/changes");
  await settled(page);
  const savedView = (await card(page)).filter;
  // The link again, then an explicit change: add RDS.
  await page.goto("/aws/changes?service=s3,ec2");
  await settled(page);
  await toggle(page, "RDS");
  await closePanel(page);
  await page.waitForTimeout(900);
  const url = new URL(page.url());
  if (url.searchParams.has("service")) problems.push(`URL keeps service= after an explicit change: ${url.search}`);
  const after = await prefs(page);
  if (after.services.slice().sort().join(",") !== "ec2,rds,s3") problems.push(`explicit change saved ${JSON.stringify(after)}`);
  // Put back RDS and IAM for the likely-cause test, by the API (setup only).
  const put = await putPrefs(page, ["rds", "iam"]);
  if (put !== 200) problems.push(`PUT preferences ${put}`);
  record("shared-link", { problems, before, during, savedView, after, files: [linkShot, linkFull] });
  expect(problems).toEqual([]);
});

// ---------------------------------------------------------------------------
// 4. The ECS deploy is the likely cause; with RDS and IAM chosen the card
// marks it "Not in your service filter", and Show › adds ECS for this view
// and scrolls to the row.

test("4 likely cause outside the filter", async ({ browser }) => {
  onScenario("bad-deploy");
  test.setTimeout(120_000);
  const problems: string[] = [];
  const results: Record<string, unknown> = {};
  for (const theme of THEMES) {
    const watch = newWatch();
    const page = await open(browser, theme, "/aws/changes", watch);
    const saved = await prefs(page);
    if (saved.services.slice().sort().join(",") !== "iam,rds") problems.push(`${theme}: setup preferences ${JSON.stringify(saved)}`);
    const api = await changesApi(page, "?service=rds,iam");
    if (api.suspect.status !== "found" || api.suspect.change?.service !== "ecs" || api.suspect.in_filter !== false) {
      problems.push(`${theme}: API suspect ${JSON.stringify({ status: api.suspect.status, service: api.suspect.change?.service, in_filter: api.suspect.in_filter })}`);
    }
    const c = await card(page);
    if (c.outside !== "Not in your service filter") problems.push(`${theme}: marker "${c.outside}"`);
    if (!/UpdateService|deploy|checkout|Service/i.test(c.likely)) problems.push(`${theme}: likely cause reads "${c.likely}"`);
    const markedShot = await shot(page, theme, `${STATE}-likely-outside-filter`);
    await page.locator("[data-likely-cause]").getByRole("button", { name: "Show ›" }).click();
    await page.waitForLoadState("networkidle").catch(() => {});
    await page.waitForTimeout(1200);
    const url = new URL(page.url());
    const svc = url.searchParams.get("service") ?? "";
    if (svc.split(",").sort().join(",") !== "ecs,iam,rds") problems.push(`${theme}: Show › URL service=${svc}`);
    const id = api.suspect.change?.id ?? "";
    const row = page.locator(`[data-list-scroll] [data-item="${id}"]`);
    if ((await row.count()) === 0) problems.push(`${theme}: suspect row ${id} not in the list after Show ›`);
    else {
      const inView = await row.evaluate((r) => {
        const s = r.closest("[data-list-scroll]") as HTMLElement;
        const a = r.getBoundingClientRect();
        const b = s.getBoundingClientRect();
        return a.top >= b.top - 1 && a.bottom <= b.bottom + 1 && a.top >= 0 && a.bottom <= window.innerHeight;
      });
      if (!inView) problems.push(`${theme}: suspect row not scrolled into view`);
      if ((await row.getAttribute("data-shown")) === null) problems.push(`${theme}: suspect row not outlined (data-shown)`);
    }
    const after = await card(page);
    if (after.outside) problems.push(`${theme}: still marked outside after Show ›`);
    if (after.filter !== "RDS, IAM, ECS") problems.push(`${theme}: card names "${after.filter}" after Show ›`);
    const shownShot = await shotView(page, theme, `${STATE}-likely-show-added-ecs`);
    const savedAfter = await prefs(page);
    if (savedAfter.services.slice().sort().join(",") !== "iam,rds") problems.push(`${theme}: Show › saved ${JSON.stringify(savedAfter)}`);
    if (watch.console.length) problems.push(`${theme}: console ${watch.console.join(" | ")}`);
    if (watch.http.length) problems.push(`${theme}: http ${watch.http.join(" | ")}`);
    results[theme] = { likely: c.likely, outside: c.outside, svc, after: after.filter, savedAfter, files: [markedShot, shownShot] };
  }
  record("likely-outside", { results, problems });
  expect(problems).toEqual([]);
});

// ---------------------------------------------------------------------------
// Compare narrowed: once the writes after the first snapshot are read, the
// Compare card's "changes between" lists only the chosen services.

test("1b compare shows only the chosen services", async ({ browser }) => {
  onScenario("bad-deploy");
  test.setTimeout(120_000);
  const problems: string[] = [];
  const results: Record<string, unknown> = {};
  for (const theme of THEMES) {
    const watch = newWatch();
    const page = await open(browser, theme, "/aws/changes", watch);
    // Compare from the earliest full compare, as a user picks it ("Use HH:MM").
    const panel = page.locator('[data-panel="compare"]');
    const use = panel.getByRole("button", { name: /^Use \d{1,2}:\d{2}$/ });
    if (await use.count()) {
      await use.first().click();
      await page.waitForLoadState("networkidle").catch(() => {});
      await page.waitForTimeout(600);
    }
    const between = page.locator("[data-changes-between]");
    const compareText = await panel.innerText();
    const req = watch.requests.filter((r) => r.startsWith("/api/admin/aws/changes/compare"));
    const from = new URLSearchParams(req[req.length - 1]?.split("?")[1] ?? "").get("from") ?? "";
    const all = from ? await (await page.request.get(`/api/admin/aws/changes/compare?from=${encodeURIComponent(from)}`)).json() : null;
    const shown = from ? await (await page.request.get(`/api/admin/aws/changes/compare?from=${encodeURIComponent(from)}&service=rds,iam`)).json() : null;
    const svcOf = (d: Record<string, Array<{ service?: string }>> | null) =>
      d ? [...new Set(["added", "removed", "changed", "changes_between"].flatMap((k) => (d[k] ?? []).map((i) => i.service ?? "?")))] : [];
    const stray = [...watch.compareServices].filter((s) => s !== "rds" && s !== "iam");
    if (stray.length) problems.push(`${theme}: compare answer carried ${stray.join(", ")}`);
    if (!req.every((r) => /[?&]service=rds(%2C|,)iam\b/.test(r))) problems.push(`${theme}: compare request without the filter: ${req.join(" | ")}`);
    const allSvc = svcOf(all);
    const shownSvc = svcOf(shown);
    if (!allSvc.some((s) => s !== "rds" && s !== "iam")) problems.push(`${theme}: unfiltered compare holds no other service (${allSvc.join(",")}); nothing to narrow`);
    if (!shownSvc.length) problems.push(`${theme}: filtered compare is empty; the post-boot RDS/IAM writes are not in it`);
    if (shownSvc.some((s) => s !== "rds" && s !== "iam")) problems.push(`${theme}: filtered compare API carried ${shownSvc.join(",")}`);
    for (const word of ["Config", "EC2", "Athena", "ECS", "CreateTags", "tags"]) {
      if (new RegExp(`\\b${word}\\b`).test(compareText)) problems.push(`${theme}: Compare card mentions ${word}`);
    }
    if ((await between.count()) === 0 && shownSvc.length) problems.push(`${theme}: Compare card shows no changes between`);
    const file = await shot(page, theme, `${STATE}-compare-rds-iam`);
    results[theme] = { compareText: compareText.slice(0, 600), requests: req, allServices: allSvc, shownServices: shownSvc, between: await between.count(), file };
  }
  record("compare", { results, problems });
  expect(problems).toEqual([]);
});

// ---------------------------------------------------------------------------
// 5 and 6 on changes-cloudtrail-denied: the warning line is the same with any
// filter, and "No … changes" never shows with it.

test("5 warning line is the same with any filter", async ({ browser }) => {
  onScenario("changes-cloudtrail-denied");
  test.setTimeout(150_000);
  const problems: string[] = [];
  const results: Record<string, unknown> = {};
  for (const theme of THEMES) {
    const watch = newWatch();
    const page = await open(browser, theme, "/aws/changes", watch);
    const views: Record<string, { warning: string; noChanges: string; filter: string; changes: string }> = {};
    for (const [name, url] of [
      ["default", "/aws/changes"],
      ["all", "/aws/changes?service="],
      ["ecs", "/aws/changes?service=ecs"],
      ["lambda-none", "/aws/changes?service=lambda"],
    ] as const) {
      await page.goto(url);
      await settled(page);
      const c = await card(page);
      views[name] = { warning: c.warning, noChanges: c.noChanges, filter: c.filter, changes: c.changes };
      if (name === "default" || name === "lambda-none") results[`${theme}-${name}-file`] = await shot(page, theme, `${STATE}-warning-${name}`);
    }
    const base = views.default.warning;
    if (!base) problems.push(`${theme}: no warning line on cloudtrail-denied`);
    for (const [name, v] of Object.entries(views)) {
      if (v.warning !== base) problems.push(`${theme}: warning with ${name} "${v.warning}" vs default "${base}"`);
      if (v.noChanges) problems.push(`${theme}: "${v.noChanges}" shown beside a warning line (${name})`);
    }
    // The popover lists the same gaps either way.
    const pop = async (url: string) => {
      await page.goto(url);
      await settled(page);
      await page.locator("[data-warning-line] button[aria-haspopup='dialog']").first().click();
      const text = (await page.getByRole("dialog").last().innerText()).replace(/\s+/g, " ").trim();
      await page.keyboard.press("Escape");
      return text;
    };
    const popDefault = await pop("/aws/changes");
    const popLambda = await pop("/aws/changes?service=lambda");
    if (popDefault !== popLambda) problems.push(`${theme}: popover differs: "${popDefault.slice(0, 120)}" vs "${popLambda.slice(0, 120)}"`);
    problems.push(...banned("warning popover", popDefault).map((p) => `${theme}: ${p}`));
    if (watch.console.length) problems.push(`${theme}: console ${watch.console.join(" | ")}`);
    results[theme] = { views, popDefault: popDefault.slice(0, 400) };
  }
  record("warning-any-filter", { results, problems });
  expect(problems).toEqual([]);
});

test("6 no changes names the services", async ({ browser }) => {
  onScenario("changes-quiet");
  test.setTimeout(120_000);
  const problems: string[] = [];
  const results: Record<string, unknown> = {};
  for (const theme of THEMES) {
    const watch = newWatch();
    const page = await open(browser, theme, "/aws/changes", watch);
    const c = await card(page);
    if (c.warning) problems.push(`${theme}: warning line on changes-quiet: "${c.warning}"`);
    if (c.noChanges !== "No RDS or IAM changes in the last 24 h") problems.push(`${theme}: no-changes line "${c.noChanges}"`);
    const file = await shot(page, theme, `${STATE}-no-rds-iam-changes`);
    await page.goto("/aws/changes?service=rds,iam,s3");
    await settled(page);
    const three = (await card(page)).noChanges;
    if (three !== "No RDS, IAM or S3 changes in the last 24 h") problems.push(`${theme}: three services read "${three}"`);
    await page.goto("/aws/changes?service=");
    await settled(page);
    const allText = (await card(page)).noChanges;
    if (allText !== "No changes in the last 24 h") problems.push(`${theme}: all services read "${allText}"`);
    problems.push(...banned("page", await page.locator("main").innerText()).map((p) => `${theme}: ${p}`));
    results[theme] = { noChanges: c.noChanges, three, allText, filter: c.filter, file };
  }
  record("no-changes", { results, problems });
  expect(problems).toEqual([]);
});

// ---------------------------------------------------------------------------
// 7. changes-busy with a filter: Load more to the end, no duplicates or skips.

test("7 paging with a filter: no duplicates or skips", async ({ browser }) => {
  onScenario("changes-busy");
  test.setTimeout(600_000);
  const problems: string[] = [];
  const theme: Theme = "dark";
  const watch = newWatch();
  const page = await open(browser, theme, "/aws/changes", watch);
  // Default RDS and IAM: only the interleaved RDS writes among the Auto Scaling ones.
  const api = await changesApi(page, "?service=rds,iam");
  // Walk the API's own pages for the expected IDs.
  const want: string[] = [];
  let cursor: string | null = null;
  let first: ChangesAnswer | null = null;
  for (let i = 0; i < 40; i++) {
    const page1: ChangesAnswer = await changesApi(page, `?service=rds,iam${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`);
    first ??= page1;
    want.push(...page1.items.map((x) => x.id));
    cursor = page1.next_cursor;
    if (!cursor) break;
  }
  const loadMore = page.locator("[data-list-scroll]").getByRole("button", { name: "Load more", exact: true });
  let clicks = 0;
  while ((await loadMore.count()) > 0 && clicks < 40) {
    const n = await page.locator("[data-list-scroll] [data-item]").count();
    await loadMore.scrollIntoViewIfNeeded();
    await loadMore.click();
    await expect.poll(() => page.locator("[data-list-scroll] [data-item]").count(), { timeout: 20_000 }).toBeGreaterThan(n);
    clicks++;
  }
  const ids = await page.locator("[data-list-scroll] [data-item]").evaluateAll((els) => els.map((e) => e.getAttribute("data-item") ?? ""));
  const dupes = ids.length - new Set(ids).size;
  const missing = want.filter((id) => !ids.includes(id));
  const extra = ids.filter((id) => !want.includes(id));
  if (dupes) problems.push(`${dupes} duplicate rows`);
  if (missing.length) problems.push(`${missing.length} rows the API pages hold are missing (first ${missing.slice(0, 3).join(", ")})`);
  if (extra.length) problems.push(`${extra.length} rows not in the API pages`);
  const stray = [...watch.itemServices].filter((s) => s !== "rds" && s !== "iam");
  if (stray.length) problems.push(`list answers carried ${stray.join(", ")}`);
  const c = await card(page);
  if (c.changes !== fmt(api.summary.total)) problems.push(`card Changes ${c.changes} vs API ${api.summary.total}`);
  const sumCounts = (await changesApi(page, "?service=rds,iam")).items.length;
  await page.locator("[data-list-scroll]").evaluate((e) => (e.scrollTop = e.scrollHeight));
  const endShot = await shotView(page, theme, `${STATE}-rds-iam-list-end`);
  const top = await shot(page, theme, `${STATE}-rds-iam`);
  if (watch.console.length) problems.push(`console ${watch.console.join(" | ")}`);
  if (watch.http.length) problems.push(`http ${watch.http.join(" | ")}`);
  record("paging", { problems, total: api.summary.total, apiIds: want.length, rows: ids.length, clicks, dupes, sumCounts, cardChanges: c.changes, services: first?.services, files: [endShot, top] });
  expect(problems).toEqual([]);
});

// ---------------------------------------------------------------------------
// 9c. The Logs list page filter (FilterPanel was extended). Fixture-assisted:
// this stack has no log patterns, so the pattern index answer is a fixture
// with counts in the thousands; the agent config is the server's own.

test("9c logs list filter still works (fixture patterns)", async ({ browser }) => {
  test.skip(process.env.C13_LOGS !== "1", "set C13_LOGS=1 on a stack with the agent on");
  test.setTimeout(120_000);
  const problems: string[] = [];
  const results: Record<string, unknown> = {};
  for (const theme of THEMES) {
    const context = await newContext(browser, theme);
    const page = await context.newPage();
    const watch = newWatch();
    watchPage(page, watch);
    await page.route(/\/api\/agent\/patterns(\?|$)/, async (route) => {
      if (route.request().method() !== "GET") return route.continue();
      const now = Date.now();
      const mk = (i: number, service: string, rule: string) => ({
        id: `fixture-${i}`,
        template: `connection reset by peer <*> service=${service}`,
        first_seen: new Date(now - 3_600_000 * ((i % 48) + 2)).toISOString(),
        last_seen: new Date(now - 60_000 * (i % 600)).toISOString(),
        count: 10 + i,
        baseline_frequency: 0.2,
        verdict: "",
        rule_name: rule,
        source: "loki",
        service,
        tags: i % 5 === 0 ? ["edge"] : [],
        readiness: { ready: true, seen: 200, needed: 100, rate_per_min: 4 },
      });
      // Facet counts are patterns per value: 1,500 checkout, 900 payments, 600 orders.
      const patterns = Array.from({ length: 3000 }, (_, i) =>
        mk(i, i % 2 === 0 ? "checkout" : i % 10 < 6 ? "payments" : "orders", i % 3 === 0 ? "http-5xx" : ""),
      );
      await route.fulfill({ json: { patterns, total: patterns.length, offset: 0, next_offset: null } });
    });
    await page.emulateMedia({ colorScheme: theme });
    await openApp(page, "/agent/logs");
    await page.goto("/agent/logs");
    await page.waitForLoadState("networkidle").catch(() => {});
    const btn = page.getByRole("button", { name: /^Filters/ });
    const visible = await btn.isVisible().catch(() => false);
    if (!visible) {
      problems.push(`${theme}: Filters button missing on the Logs list page`);
      results[theme] = { file: await shot(page, theme, "logs-no-filters") };
      continue;
    }
    await btn.click();
    const dialog = page.getByRole("dialog", { name: "Filters" });
    await expect(dialog).toBeVisible();
    const counts = await dialog.locator("li label span:last-child").allInnerTexts();
    const thousands = counts.filter((c) => /^\d{1,3}(,\d{3})+$/.test(c.trim()));
    const panelShot = await shotView(page, theme, "logs-filters-open");
    // Toggle the first option and check the chips and the list react.
    const firstLabel = (await dialog.locator("li label").first().innerText()).replace(/\s+/g, " ").trim();
    await dialog.locator("li input").first().click();
    await page.keyboard.press("Escape");
    await expect(page.locator('[aria-label="Applied filters"] .pill').first()).toBeVisible({ timeout: 10_000 }).catch(() => {});
    const chips = await page.locator('[aria-label="Applied filters"] .pill').allInnerTexts();
    if (thousands.length === 0) problems.push(`${theme}: no count over 999 shows a thousands separator (${counts.slice(0, 6).join(", ")})`);
    if (chips.length === 0) problems.push(`${theme}: no chip after choosing "${firstLabel}"`);
    const chipShot = await shot(page, theme, "logs-filter-applied");
    problems.push(...banned("logs page", await page.locator("main").innerText()).filter((p) => p.includes("raw")).map((p) => `${theme}: ${p}`));
    results[theme] = { counts: counts.slice(0, 12), thousands: thousands.length, firstLabel, chips, files: [panelShot, chipShot] };
    await context.close();
  }
  record("logs-filter", { results, problems });
  expect(problems).toEqual([]);
});
