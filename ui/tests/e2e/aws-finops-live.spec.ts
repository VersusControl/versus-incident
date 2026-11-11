// AWS FinOps browser acceptance against a REAL AWS account
// (plans/status/aws-harness/a8-finops.md §7.0–7.5, plan principle 11 "Quiet text").
//
// Live only: the harness `oss-aws` stack reads one real account through the
// default credential chain, and this spec drives the page like an operator.
// It asserts what the page must show for whatever the account returns (the
// worst-case states of §7.4 included), so the branches follow the live API.
// The two toast checks that real data cannot trigger without changing AWS or
// spending money (account removal, an error toast) patch one response with
// page.route and are labelled fixture in the test title.
//
// Captures use capturePath (<surface>-<viewport>-<theme>-<state>.png) into the
// run's HARNESS_IMAGES_DIR. Screens hold a real account: they stay in the
// git-ignored run folder.
//
// Run: plans/harness-run/tests/aws/aws-finops-browser-real.sh specs '<grep>'
// (sets E2E_AWS_LIVE=1). A paid drill-down runs only with E2E_AWS_ALLOW_DRILL=1.
import { expect, test, type Browser, type BrowserContext, type Locator, type Page } from "@playwright/test";
import * as fs from "node:fs";
import * as path from "node:path";
import { openApp } from "./helpers";
import { capturePath, captureDir, harnessResultsDir } from "./support/capture";
import type {
  AwsSettings,
  FinOpsBreakdownView,
  FinOpsBridgeView,
  FinOpsDateRange,
  FinOpsIncreaseDetailView,
  FinOpsIncreasesView,
  FinOpsSaving,
  FinOpsSavingsView,
  FinOpsSummaryView,
} from "../../src/lib/api";

const LIVE = process.env.E2E_AWS_LIVE === "1";
const ALLOW_DRILL = process.env.E2E_AWS_ALLOW_DRILL === "1";
test.skip(!LIVE, "needs the oss-aws harness stack reading a real AWS account (E2E_AWS_LIVE=1)");

const VIEWPORTS = [
  { name: "desktop", width: 1440, height: 900 },
  { name: "mobile", width: 390, height: 844 },
] as const;
const THEMES = ["dark", "light"] as const;
type Theme = (typeof THEMES)[number];
type Viewport = (typeof VIEWPORTS)[number];

// Error codes of the closed vocabularies. None may reach the screen as text.
const RAW_CODES = [
  "paid_request_cap_reached", "reserved_for_scheduled_refresh", "store_unavailable", "cost_data_off",
  "cost_period_too_large", "reason_required", "until_invalid", "status_change_not_allowed", "already_frozen",
  "bill_not_closed", "report_not_month", "refresh_in_progress", "no_server_identity", "role_not_found",
  "trust_denied", "external_id_mismatch", "session_duration_invalid", "region_disabled", "ce_denied",
  "ce_unavailable", "ce_data_not_ready", "rate_limited", "invalid_body", "not_found", "cost_by_account",
  "cost_by_usage", "cost_by_team", "cost_by_record", "discount_coverage", "resource_detail", "drill_down",
];

// Principle 11: hints, source captions, disclaimers and footer meta are gone
// from the page (the measure, lag and paid use live behind an ⓘ).
const QUIET_FORBIDDEN: Array<[RegExp, string]> = [
  [/what you actually pay/i, "measure definition outside its ⓘ"],
  [/lags up to 24/i, "data lag outside its ⓘ"],
  [/paid requests today/i, "paid API use outside the freshness popover"],
  [/daily cost from cost explorer/i, "source caption"],
  [/from aws, at the current pace/i, "source caption"],
  [/read only, versus does not run/i, "disclaimer"],
  [/guides only/i, "disclaimer"],
  [/\bclick (a|the|on)\b/i, "instruction hint"],
  [/\b1 of 1\b/, "1 of 1 for a single account"],
];

const contexts: BrowserContext[] = [];
test.afterEach(async () => {
  for (const c of contexts.splice(0)) await c.close().catch(() => {});
});

// ---------------------------------------------------------------------------
// Live data and page helpers
// ---------------------------------------------------------------------------

async function getJson<T>(page: Page, url: string): Promise<T> {
  const res = await page.request.get(url);
  expect(res.status(), `${url} answers 200`).toBe(200);
  return (await res.json()) as T;
}

const MON = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];

/**
 * Independent oracle for a partial comparison month (QA-403): "Oct 1–9", with
 * ", 2025" when its year differs from the period's. "" for a whole month.
 */
function partialLabel(range: FinOpsDateRange | undefined, periodYear: number | undefined): string {
  if (!range?.start || !range.end) return "";
  const [ys, ms, ds] = range.start.split("-").map(Number);
  const [ye, me, de] = range.end.split("-").map(Number);
  if (ys !== ye || ms !== me) return "";
  const last = new Date(Date.UTC(ys, ms, 0)).getUTCDate();
  if (ds === 1 && de === last) return "";
  const days = ds === de ? `${MON[ms - 1]} ${ds}` : `${MON[ms - 1]} ${ds}–${de}`;
  return periodYear && periodYear !== ys ? `${days}, ${ys}` : days;
}

/** A money amount that is zero ("$0", "+$0.00", "−$0"), never a price such as "$0.626/hour" (QA-404, QA-406, QA-408). */
const ZERO_MONEY = /(?:^|[^\d.,])[+−-]?\$0(?:\.0+)?(?![\d.,])/;

/** Visible text of a region minus its charts (an axis tick of $0 is a value, not a stated amount). */
async function textOutsideCharts(loc: Locator): Promise<string> {
  return loc.evaluate((root) => {
    const clone = root.cloneNode(true) as HTMLElement;
    for (const el of Array.from(clone.querySelectorAll('[role="img"], svg'))) el.remove();
    clone.style.cssText = "position:absolute;left:-10000px;top:0;width:" + (root as HTMLElement).clientWidth + "px";
    document.body.appendChild(clone);
    const text = clone.innerText;
    clone.remove();
    return text;
  });
}

/** Account IDs, ARNs and echoed AWS recommendation text must not reach a saving title (QA-406, QA-409). */
const SAVING_TITLE_LEAKS: Array<[RegExp, string]> = [
  [/\b\d{12}\b/, "a 12-digit account ID"],
  [/arn:aws/i, "an ARN"],
  [/AWS account/i, "AWS recommendation text"],
  [/Linked account level/i, "AWS recommendation text"],
  [/AllUpfront|NoUpfront|PartialUpfront|years? term/i, "AWS recommendation text"],
];

async function open(browser: Browser, viewport: Viewport, theme: Theme, url: string): Promise<Page> {
  const context = await browser.newContext({
    viewport: { width: viewport.width, height: viewport.height },
    colorScheme: theme,
  });
  contexts.push(context);
  await context.grantPermissions(["clipboard-read", "clipboard-write"]).catch(() => {});
  await context.addInitScript((value) => localStorage.setItem("versus.theme", value), theme);
  const page = await context.newPage();
  await page.emulateMedia({ colorScheme: theme });
  await openApp(page, url);
  if (!page.url().includes(url.split("?")[0])) await page.goto(url);
  const htmlTheme = await page.locator("html").getAttribute("data-theme");
  if (theme === "light") expect(htmlTheme).toBe("light");
  else expect(htmlTheme).not.toBe("light");
  return page;
}

async function shot(page: Page, surface: string, viewport: Viewport, theme: Theme, state: string, fullPage = true) {
  const out = capturePath(surface, viewport.width, theme, state);
  if (!fullPage) {
    await page.screenshot({ path: out, animations: "disabled" });
    return;
  }
  // The app scrolls inside <main>, so a full-page shot grows the viewport to
  // the scroll height of main, then restores it.
  const extra = await page.evaluate(() => {
    const m = document.querySelector("main");
    return m ? Math.max(0, m.scrollHeight - m.clientHeight) : 0;
  });
  if (extra > 0) await page.setViewportSize({ width: viewport.width, height: viewport.height + extra });
  await page.waitForTimeout(200);
  await page.screenshot({ path: out, animations: "disabled", fullPage: true });
  if (extra > 0) await page.setViewportSize({ width: viewport.width, height: viewport.height });
}

function recordFindings(name: string, findings: string[]) {
  const file = path.join(harnessResultsDir(), "aws-finops-live-findings.jsonl");
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.appendFileSync(file, JSON.stringify({ test: name, findings }) + "\n");
}

/** Layout, naming and wording checks shared by every screen. */
async function audit(page: Page, label: string, findings: string[], opts: { quiet?: boolean } = {}) {
  const r = await page.evaluate(
    ({ codes }) => {
      const out: string[] = [];
      const overflow = document.documentElement.scrollWidth - window.innerWidth;
      if (overflow > 1) out.push(`page overflows horizontally by ${overflow}px`);
      // Cards may scroll a wide table inside themselves; the card box itself must fit.
      for (const el of Array.from(document.querySelectorAll<HTMLElement>("main section.card, main section[aria-label]"))) {
        const box = el.getBoundingClientRect();
        if (box.width > 0 && box.right > window.innerWidth + 1) out.push(`card "${el.getAttribute("aria-label") ?? el.getAttribute("aria-labelledby") ?? ""}" runs ${Math.round(box.right - window.innerWidth)}px past the viewport`);
      }
      for (const el of Array.from(document.querySelectorAll<HTMLElement>("main .truncate"))) {
        if (el.offsetParent !== null && el.scrollWidth - el.clientWidth > 1 && !el.getAttribute("title")) {
          out.push(`clipped text without a full-text title: "${(el.textContent ?? "").trim().slice(0, 60)}"`);
        }
      }
      const nameOf = (el: Element): string => {
        const aria = el.getAttribute("aria-label");
        if (aria && aria.trim()) return aria.trim();
        const by = el.getAttribute("aria-labelledby");
        if (by) return by.split(/\s+/).map((id) => document.getElementById(id)?.textContent ?? "").join(" ").trim();
        const text = ((el as HTMLElement).innerText || el.textContent || "").trim();
        if (text) return text;
        return el.getAttribute("title")?.trim() ?? "";
      };
      const controls = Array.from(document.querySelectorAll<HTMLElement>("button, a[href], [role='button'], [role='tab']"))
        .filter((el) => el.offsetParent !== null || el.getClientRects().length > 0);
      for (const el of controls) {
        const name = nameOf(el);
        if (!name) out.push(`control without an accessible name: <${el.tagName.toLowerCase()} class="${el.className.toString().slice(0, 60)}">`);
        if (/\b(approve|run)\b/i.test(name)) out.push(`forbidden control "${name}"`);
      }
      // Every ⓘ (a button whose only content is the lucide info icon) is named.
      for (const el of Array.from(document.querySelectorAll<HTMLElement>("button"))) {
        if (el.querySelector("svg.lucide-info") && !(el.innerText ?? "").trim() && !(el.getAttribute("aria-label") ?? "").trim()) {
          out.push("ⓘ button without an accessible name");
        }
      }
      const visible = (document.querySelector("main") as HTMLElement | null)?.innerText ?? document.body.innerText;
      const toasts = Array.from(document.querySelectorAll<HTMLElement>("div.z-toast")).map((t) => t.innerText).join("\n");
      for (const code of codes) {
        const re = new RegExp(`\\b${code}\\b`);
        if (re.test(visible) || re.test(toasts)) out.push(`raw code "${code}" on screen`);
      }
      if (/\b(HTTP )?[45]\d\d\b.*(error|failed)/i.test(toasts)) out.push("toast shows an HTTP status");
      return { out, visible };
    },
    { codes: RAW_CODES },
  );
  for (const f of r.out) findings.push(`${label}: ${f}`);
  if (opts.quiet) {
    for (const [re, why] of QUIET_FORBIDDEN) if (re.test(r.visible)) findings.push(`${label}: quiet text: ${why} (${re})`);
  }
}

/** Principle 11: at most three text sizes per card. */
async function textSizes(page: Page, label: string, findings: string[]) {
  const rows = await page.evaluate(() =>
    Array.from(document.querySelectorAll<HTMLElement>("main section.card")).map((card) => {
      const sizes = new Set<string>();
      const walk = document.createTreeWalker(card, NodeFilter.SHOW_TEXT);
      for (let n = walk.nextNode(); n; n = walk.nextNode()) {
        const parent = n.parentElement;
        if (!parent || !(n.textContent ?? "").trim() || parent.closest(".sr-only, [role='note'], svg")) continue;
        if (parent.offsetParent === null) continue;
        sizes.add(getComputedStyle(parent).fontSize);
      }
      const title = card.querySelector("h2")?.textContent?.trim() ?? card.getAttribute("aria-label") ?? "card";
      return { title, sizes: Array.from(sizes) };
    }),
  );
  for (const r of rows) if (r.sizes.length > 3) findings.push(`${label}: card "${r.title}" uses ${r.sizes.length} text sizes (${r.sizes.join(", ")})`);
}

/**
 * Tab through the page: every stop is visible, shows a focus indicator, and
 * the stops include the ⓘ buttons and the header controls. Returns the names.
 */
async function keyboardWalk(page: Page, label: string, findings: string[], max = 90): Promise<string[]> {
  await page.locator("main").first().evaluate((el) => {
    (el as HTMLElement).setAttribute("tabindex", "-1");
    (el as HTMLElement).focus();
  });
  const names: string[] = [];
  let first = "";
  for (let i = 0; i < max; i += 1) {
    await page.keyboard.press("Tab");
    const stop = await page.evaluate(() => {
      const el = document.activeElement as HTMLElement | null;
      if (!el || el === document.body) return null;
      const r = el.getBoundingClientRect();
      const cs = getComputedStyle(el);
      const ring = (cs.outlineStyle !== "none" && cs.outlineWidth !== "0px") || (cs.boxShadow && cs.boxShadow !== "none");
      const name = (el.getAttribute("aria-label") || el.innerText || el.getAttribute("title") || el.tagName).trim().slice(0, 60);
      const inMain = Boolean(el.closest("main"));
      // A cut-off text toggle (ClippedText) is a stop only while its text is cut off.
      const clipToggle = el.tagName === "BUTTON" && el.classList.contains("truncate") && el.hasAttribute("title");
      return {
        key: `${el.tagName}|${name}|${Math.round(r.top + window.scrollY)}|${Math.round(r.left)}`,
        name,
        inMain,
        visible: r.width > 0 && r.height > 0 && r.bottom > 0 && r.top < window.innerHeight,
        ring,
        uncutToggle: clipToggle && el.getAttribute("aria-expanded") !== "true" && el.scrollWidth <= el.clientWidth,
      };
    });
    if (!stop) continue;
    if (stop.key === first) break;
    if (!first) first = stop.key;
    if (!stop.inMain) continue;
    names.push(stop.name);
    if (!stop.visible) findings.push(`${label}: focus stop "${stop.name}" is not scrolled into view`);
    if (!stop.ring) findings.push(`${label}: focus stop "${stop.name}" shows no focus indicator`);
    if (stop.uncutToggle) findings.push(`${label}: text "${stop.name}" is a tab stop but is not cut off`);
  }
  return names;
}

function toastBox(page: Page): Locator {
  return page.locator("div.z-toast");
}

/** The page's live state, read from the same API the page reads. */
async function liveState(page: Page) {
  const summary = await getJson<FinOpsSummaryView>(page, "/api/admin/aws/finops/summary");
  const on = Boolean(summary.enabled) && summary.reason !== "no_cost_data";
  const bridge = on ? await getJson<FinOpsBridgeView>(page, "/api/admin/aws/finops/bridge") : null;
  const byService = on ? await getJson<FinOpsBreakdownView>(page, "/api/admin/aws/finops/breakdown?group_by=service") : null;
  const savings = on ? await getJson<FinOpsSavingsView>(page, "/api/admin/aws/finops/savings") : null;
  // Every status: resolved increases are what a bridge row links to (QA-404).
  const increases = on ? await getJson<FinOpsIncreasesView>(page, "/api/admin/aws/finops/increases?status=all") : null;
  return { summary, on, bridge, byService, savings, increases };
}

type LiveState = Awaited<ReturnType<typeof liveState>>;

function shape(state: LiveState) {
  const s = state.summary;
  return {
    enabled: s.enabled,
    reason: s.reason ?? null,
    period: s.period ? { open: s.period.open, label: s.period.label, range: s.period.range } : null,
    compare: s.compare ? { available: s.compare.available, short: s.compare.short, range: s.compare.range } : null,
    change: s.change ? { direction: s.change.direction, word: s.change.word } : null,
    forecast: s.forecast ? { source: s.forecast.source, reason: s.forecast.reason ?? null } : null,
    budget: Boolean(s.budget),
    meter: s.meter,
    sync: s.sync
      ? {
          last_complete_day: s.sync.last_complete_day, stale: s.sync.stale, stale_reason: s.sync.stale_reason ?? null,
          learning: s.sync.learning, accounts_total: s.sync.accounts_total, accounts_readable: s.sync.accounts_readable,
          skipped_calls: s.sync.skipped_calls ?? [], missing: (s.sync.missing_accounts ?? []).map((m) => m.reason),
          team_tag_active: s.sync.team_tag?.active ?? null,
        }
      : null,
    bridge: state.bridge
      ? {
          available: state.bridge.available, steady: state.bridge.steady, note: state.bridge.note ?? null,
          rows: (state.bridge.rows ?? []).length, resource_note: Boolean(state.bridge.resource_note),
          too_large_rows: (state.bridge.rows ?? []).filter((r) => r.resources === "too_large").length,
          increase_rows: (state.bridge.rows ?? []).filter((r) => r.increase_id).length,
        }
      : null,
    service_rows: state.byService ? (state.byService.rows ?? []).length : null,
    spend_by_options: state.byService ? (state.byService.options ?? []).map((o) => ({ g: o.group_by, ok: o.available ?? null, reason: o.reason ?? null })) : null,
    savings: state.savings
      ? {
          items: (state.savings.items ?? []).map((i) => ({ kind: i.kind, status: i.status, before_other_savings: Boolean(i.before_other_savings) })),
          total: state.savings.total,
          gaps: (state.savings.gaps ?? []).map((g) => g.reason),
        }
      : null,
    increases: state.increases
      ? (state.increases.items ?? []).map((i) => ({ status: i.status, stated_change_micros: i.stated_change_micros, period_change_micros: i.period_change_micros }))
      : null,
  };
}

/** QA-406 and QA-409: saving titles name resources by ID, with no account IDs or echoed AWS text. */
function savingTitleFindings(savings: FinOpsSavingsView | null, label: string): string[] {
  const out: string[] = [];
  for (const item of savings?.items ?? []) {
    const texts = [item.title, item.line, ...(item.alternatives ?? []).map((a) => a.title)].filter(Boolean);
    for (const text of texts) {
      for (const [re, why] of SAVING_TITLE_LEAKS) if (re.test(text)) out.push(`${label}: saving ${item.kind} text carries ${why}`);
    }
    if (item.kind === "commitment" && item.title.includes(":")) {
      // QA-406: the suggestion clause is Versus wording only ("a 3-year compute commitment of $0.626/h").
      const clause = item.title.slice(item.title.indexOf(":") + 1).trim();
      if (!/^a( \d-year)?( [a-z ]+)? commitment of \$[\d.,]+\/h$/.test(clause)) out.push(`${label}: commitment clause is not in Versus wording`);
    }
    if (item.kind === "rightsize") {
      const id = item.resource_ids?.[0] ?? "";
      // Default allowlist (no Name): the title names the resource by its ID.
      if (!id || !item.title.includes(id)) out.push(`${label}: rightsize title does not name the resource ID`);
    }
  }
  return out;
}

// ---------------------------------------------------------------------------
// Settings: first connect and sync
// ---------------------------------------------------------------------------

for (const viewport of VIEWPORTS) {
  for (const theme of THEMES) {
    test(`settings connected account (${viewport.name}, ${theme})`, async ({ browser }) => {
      const findings: string[] = [];
      const page = await open(browser, viewport, theme, "/aws/settings");
      const accounts = page.locator('section[aria-labelledby="aws-accounts-title"]');
      await expect(accounts).toBeVisible();
      await expect(accounts.getByText("Harness account").first()).toBeVisible();
      await expect(accounts.getByText("From config").first()).toBeVisible();
      // A first sync shows "First sync" with services filling in; a settled one shows the status word.
      const firstSync = page.locator('[role="status"][aria-label*="first sync"]');
      const state = (await firstSync.count()) > 0 ? "first-sync" : "connected";
      test.info().annotations.push({ type: "settings-state", description: state });
      // QA-409: the run keeps the default tag-value allowlist (no Name).
      const settings = await getJson<AwsSettings>(page, "/api/admin/aws/settings");
      const allow = settings.privacy?.tag_value_allowlist ?? [];
      test.info().annotations.push({ type: "tag-value-allowlist", description: `${allow.join(", ")} (${settings.privacy?.tag_value_allowlist_source})` });
      if (allow.includes("Name")) findings.push("QA-409 setup: Name is on the tag-value allowlist; this pass needs the default");
      await page.waitForTimeout(300);
      await shot(page, "aws-settings-live", viewport, theme, state);
      await audit(page, `settings/${viewport.name}/${theme}`, findings);
      recordFindings(test.info().title, findings);
      expect.soft(findings, "settings layout and naming").toEqual([]);
    });
  }
}

// First connect (QA-402): a snapshot of Settings and FinOps as they are right
// now, taken repeatedly while the first check and the first refresh run.
// Live only, opt in with E2E_AWS_FIRST_CONNECT=<label>.
const FIRST_CONNECT = process.env.E2E_AWS_FIRST_CONNECT ?? "";
for (const viewport of VIEWPORTS) {
  test(`first connect snapshot (${viewport.name})`, async ({ browser }) => {
    test.skip(!FIRST_CONNECT, "set E2E_AWS_FIRST_CONNECT=<label> during a first connect");
    const findings: string[] = [];
    const theme: Theme = viewport.name === "desktop" ? "dark" : "light";
    const page = await open(browser, viewport, theme, "/aws/settings");
    await expect(page.locator('section[aria-labelledby="aws-accounts-title"]').getByText("Harness account").first()).toBeVisible();
    await page.waitForTimeout(300);
    const settingsText = await page.locator("main").innerText();
    const pick = (t: string) => t.split("\n").filter((l) => /waiting|first check|first sync|connected|disconnected|ready|readable|Regions|All good|of \d+/i.test(l)).slice(0, 8).join(" | ");
    test.info().annotations.push({ type: "settings", description: pick(settingsText).replace(/[0-9]{12}/g, "<account>") });
    await shot(page, "aws-settings-live", viewport, theme, `first-connect-${FIRST_CONNECT}`);
    await page.goto("/aws/finops");
    await expect(page.locator("main").getByRole("navigation", { name: "Breadcrumb" })).toBeVisible();
    await page.waitForTimeout(500);
    const summary = await getJson<FinOpsSummaryView>(page, "/api/admin/aws/finops/summary");
    const finopsText = await page.locator("main").innerText();
    test.info().annotations.push({ type: "finops", description: `enabled ${summary.enabled}; reason ${summary.reason ?? "-"}; stale ${summary.sync?.stale ?? "-"} ${summary.sync?.stale_reason ?? ""}; meter ${summary.meter?.used ?? "-"}; text: ${finopsText.split("\n").slice(0, 6).join(" | ").slice(0, 200)}` });
    if (/No account ready/.test(finopsText)) findings.push("QA-402: FinOps says \"No account ready\" during a first connect");
    await audit(page, `first-connect/${viewport.name}`, findings);
    await shot(page, "aws-finops", viewport, theme, `first-connect-${FIRST_CONNECT}`);
    recordFindings(test.info().title, findings);
    expect.soft(findings).toEqual([]);
  });
}

// QA-409: set the tag-value allowlist through Settings ▸ Advanced, as an admin
// would. Live only, opt in with E2E_AWS_ALLOWLIST="app, team, …".
const ALLOWLIST = process.env.E2E_AWS_ALLOWLIST ?? "";
test("tag allowlist set through Settings (desktop, dark)", async ({ browser }) => {
  test.skip(!ALLOWLIST, "set E2E_AWS_ALLOWLIST to the keys to save");
  const want = ALLOWLIST.split(",").map((t) => t.trim()).filter(Boolean);
  const page = await open(browser, VIEWPORTS[0], "dark", "/aws/settings");
  await page.locator("details.card summary", { hasText: "Advanced" }).click();
  const input = page.locator("#aws-allowlist");
  await expect(input).toBeEnabled();
  await input.fill(want.join(", "));
  await page.getByRole("button", { name: "Save allowlist" }).click();
  await expect(toastBox(page).getByText("Allowlist saved")).toBeVisible();
  await page.reload();
  const settings = await getJson<AwsSettings>(page, "/api/admin/aws/settings");
  test.info().annotations.push({ type: "allowlist-saved", description: `${settings.privacy.tag_value_allowlist.join(", ")} (${settings.privacy.tag_value_allowlist_source})` });
  expect(settings.privacy.tag_value_allowlist).toEqual(want);
  await shot(page, "aws-settings-live", VIEWPORTS[0], "dark", `allowlist-${want.includes("Name") ? "Name" : want.includes("name") ? "lowercase-name" : "default"}`);
});

// ---------------------------------------------------------------------------
// FinOps page
// ---------------------------------------------------------------------------

for (const viewport of VIEWPORTS) {
  for (const theme of THEMES) {
    test(`finops page (${viewport.name}, ${theme})`, async ({ browser }) => {
      const findings: string[] = [];
      const page = await open(browser, viewport, theme, "/aws/finops");
      const live = await liveState(page);
      fs.writeFileSync(path.join(harnessResultsDir(), `finops-shape-${viewport.name}-${theme}.json`), JSON.stringify(shape(live), null, 2));
      const s = live.summary;
      const main = page.locator("main");
      await expect(main.getByRole("navigation", { name: "Breadcrumb" })).toBeVisible();

      if (!s.enabled && s.reason === "cost_data_off") {
        // 09e-01: cost reads off; the page explains and offers Settings.
        await expect(main.getByText(/Cost Explorer reads: off/i)).toBeVisible();
        await shot(page, "aws-finops", viewport, theme, "cost-off");
      } else if (!s.enabled) {
        // Cost on, but no account can be read yet (for example before the first health check).
        test.info().annotations.push({ type: "finops-off-reason", description: String(s.reason) });
        await expect(main.locator('section[aria-label="Cost data"]')).toBeVisible();
        await shot(page, "aws-finops", viewport, theme, `off-${String(s.reason)}`);
      } else if (!live.on) {
        // 09e-17: Cost Explorer not available in the account: no totals at all.
        await expect(page.locator("#ce-unavailable")).toBeVisible();
        await expect(main.getByText("No cost data")).toBeVisible();
        await expect(main.locator('section[aria-label$="cost"], section[aria-label$="to date"]')).toHaveCount(0);
        await shot(page, "aws-finops", viewport, theme, "ce-unavailable");
      } else {
        // Three summary cards, no answer sentence (§7.0).
        const period = s.period!;
        const costLabel = period.open ? `${period.short} to date` : `${period.short} cost`;
        await expect(main.locator(`section[aria-label="${costLabel}"]`)).toBeVisible();
        const changeTile = main.locator(`section[aria-label^="Change"]`);
        await expect(changeTile).toBeVisible();
        if (s.change) {
          await expect(changeTile.getByText(s.change.word, { exact: true })).toBeVisible();
        } else {
          await expect(changeTile.getByText(/No full|No previous month/)).toBeVisible();
        }
        const fcTile = main.locator('section[aria-label^="Forecast"]');
        await expect(fcTile).toBeVisible();
        if (s.forecast?.source === "none" || !s.forecast?.amount_micros) {
          await expect(fcTile.getByText(/Not enough history|Skipped today/)).toBeVisible();
        }
        if (!s.budget) await expect(fcTile.getByText(/Budget|Over budget/)).toHaveCount(0);
        await expect(main.getByText(/\bwent (up|down)\b/)).toHaveCount(0);

        // Scope bar: one account is a plain label, never "1 of 1".
        if ((s.sync?.accounts_total ?? 0) <= 1) {
          await expect(main.getByText("Account:", { exact: false }).first()).toBeVisible();
          await expect(main.getByRole("combobox", { name: "Accounts" })).toHaveCount(0);
        }

        // QA-411: lost access keeps the stored cost, marked stale, with the
        // cause behind an ⓘ and "AWS settings ›" on the same line.
        await expect(main.getByText(/No account ready/)).toHaveCount(0);
        const staleGap = main.locator('[data-gap="stale-access"]');
        if (s.sync?.stale) {
          test.info().annotations.push({ type: "stale", description: `reason ${s.sync.stale_reason}; since ${s.sync.stale_detail?.since ?? "-"}` });
          if (["credentials_invalid", "no_server_identity", "account_disconnected", "denied", "disconnected"].includes(s.sync.stale_reason ?? "")) {
            await expect(staleGap).toBeVisible();
            await expect(staleGap.getByText(/^Stale since /)).toBeVisible();
            await expect(staleGap.getByRole("link", { name: "AWS settings ›" })).toHaveAttribute("href", "/aws/settings");
            const tip = staleGap.getByRole("button", { name: "About stale cost" });
            await tip.click();
            const noteText = await page.locator('[role="note"]').first().innerText();
            test.info().annotations.push({ type: "stale-note", description: noteText });
            if (!/credentials|identity|disconnected|denied/i.test(noteText)) findings.push(`QA-411: stale ⓘ does not name the cause: "${noteText}"`);
            await shot(page, "aws-finops", viewport, theme, "stale-note-open", false);
            await page.keyboard.press("Escape");
            const freshBtn = page.getByRole("button", { name: /^Stale since/ });
            await freshBtn.first().click();
            const freshNote = await page.locator('[role="note"]').first().innerText();
            test.info().annotations.push({ type: "stale-freshness-note", description: freshNote.replace(/\n/g, " | ") });
            if (!/AWS settings/.test(freshNote)) findings.push(`QA-411: freshness note does not point to AWS settings: "${freshNote}"`);
            await shot(page, "aws-finops", viewport, theme, "stale-freshness-open", false);
            await freshBtn.first().click();
          }
        } else {
          await expect(staleGap).toHaveCount(0);
        }

        // What moved the cost.
        const bridgeCard = main.locator('section[aria-labelledby="finops-bridge-title"]');
        await expect(bridgeCard.getByRole("heading", { name: "What moved the cost" })).toBeVisible();
        const b = live.bridge!;
        let bridgeState = "bridge";
        if (!b.available) {
          bridgeState = "bridge-unavailable";
          if (b.note) await expect(bridgeCard.getByText(b.note)).toBeVisible();
        } else if (b.steady || (b.rows ?? []).length === 0) {
          bridgeState = "bridge-steady";
          await expect(bridgeCard.getByText(/No driver above|steady|offset/i).first()).toBeVisible();
        } else if (viewport.name === "mobile") {
          await expect(bridgeCard.locator('ol[aria-label="What moved the cost"]')).toBeVisible();
          await expect(bridgeCard.locator("table")).toHaveCount(0);
        } else {
          const table = bridgeCard.locator("table");
          await expect(table).toBeVisible();
          await expect(table.locator('tr[data-row-kind="base"]')).toHaveCount(1);
          await expect(table.locator('tr[data-row-kind="total"]')).toHaveCount(1);
          await expect(table.getByRole("columnheader", { name: "Service" })).toBeVisible();
          const owner = table.getByRole("columnheader", { name: "Owner" });
          if (!s.sync?.team_tag?.active) {
            await expect(owner).toHaveCount(0);
            await expect(bridgeCard.getByText(/Owner not shown|All cost has/)).toBeVisible();
          }
          // Owner column is never a column of Unallocated.
          if ((await owner.count()) > 0) {
            const cells = await table.locator("tbody tr[data-row-kind='driver'] td:nth-child(6)").allInnerTexts();
            if (cells.length > 0 && cells.every((c) => /Unallocated/.test(c))) findings.push("Owner column holds only Unallocated");
          }
        }
        if (b.available && b.resource_note) {
          await expect(bridgeCard.getByText("Resources not named").first()).toBeVisible();
          await expect(bridgeCard.getByRole("button", { name: "About resource data" })).toBeVisible();
          test.info().annotations.push({ type: "resources-not-named", description: "shown (real data)" });
        } else {
          test.info().annotations.push({ type: "resources-not-named", description: "not triggered by this account" });
        }
        test.info().annotations.push({ type: "bridge-state", description: bridgeState });

        // QA-403: a month-to-date compared with the same days of last month names
        // its days ("Oct 1–9"), never "October total".
        const partial = s.compare?.available ? partialLabel(s.compare.range, Number(period.range.start.slice(0, 4))) : "";
        test.info().annotations.push({ type: "compare-base", description: partial ? `partial: ${partial}` : "whole month" });
        if (partial) {
          if ((await main.getByText(`${s.compare!.short} total`).count()) > 0) findings.push(`QA-403: "${s.compare!.short} total" shown for the partial base ${partial}`);
          if (bridgeState === "bridge") {
            const base = viewport.name === "mobile" ? bridgeCard.locator('ol[aria-label="What moved the cost"] > li').first() : bridgeCard.locator('tr[data-row-kind="base"]');
            const baseText = await base.innerText();
            if (!baseText.includes(partial)) findings.push(`QA-403: bridge base row reads "${baseText.split("\n")[0]}", want "${partial}"`);
          }
        }

        // QA-404/QA-408: no zero amounts on the bridge or Spend by service.
        for (const [name, loc] of [
          ["What moved the cost", bridgeCard],
          ["Spend by service", main.locator("section").filter({ has: page.getByRole("heading", { name: "Spend by service" }) }).last()],
        ] as const) {
          const text = await loc.innerText();
          const line = text.split("\n").find((l) => ZERO_MONEY.test(l));
          if (line) findings.push(`zero amount in "${name}": "${line.trim().slice(0, 80)}"`);
        }

        // Spend by service, Spend by (or its removal), Savings found.
        await expect(main.getByRole("heading", { name: "Spend by service" })).toBeVisible();
        const opts = live.byService?.options ?? [];
        const qualifying = opts.filter((o) => !o.reason);
        const spendBy = main.getByRole("heading", { name: /^Spend by (account|region|team|account group)$/i });
        if (opts.length > 0 && qualifying.length === 0) {
          await expect(main.getByRole("group", { name: "Spend by" })).toHaveCount(0);
        }
        test.info().annotations.push({ type: "spend-by", description: `${await spendBy.count()} titled card(s); options ${JSON.stringify(opts.map((o) => [o.group_by, o.reason ?? "ok"]))}` });
        await expect(main.getByRole("heading", { name: "Savings found" })).toBeVisible();

        // QA-406: no green $0 total; commitment-only line when that is all; titles in Versus words.
        const sv = live.savings!;
        const savingsCard = main.locator("section").filter({ has: page.getByRole("heading", { name: "Savings found" }) }).last();
        const savingsText = await savingsCard.innerText();
        if (ZERO_MONEY.test(savingsText)) findings.push(`QA-406: Savings found shows a zero amount: "${savingsText.split("\n").find((l) => ZERO_MONEY.test(l))}"`);
        const openItems = (sv.items ?? []).filter((i) => i.status !== "dismissed");
        let savingsState = openItems.length > 0 ? `items ${openItems.length} (counted ${sv.total.count})` : "none";
        if (openItems.length === 0 && sv.total.commitment_count > 0 && sv.total.commitment_monthly_micros > 0) {
          savingsState = "commitment-only";
          await expect(savingsCard.getByText(/^Up to \$[\d,]+ a month, before other savings$/)).toBeVisible();
        }
        if (sv.total.count > 0) await expect(savingsCard.getByText(/\/mo$/).first()).toBeVisible();
        test.info().annotations.push({ type: "savings", description: savingsState });
        for (const f of savingTitleFindings(sv, "savings")) findings.push(f);
        for (const [re, why] of SAVING_TITLE_LEAKS) if (re.test(savingsText)) findings.push(`savings card shows ${why}`);

        if (viewport.name === "mobile") {
          // 390 px: the three cards stack in one column.
          const lefts = await main.locator('section[aria-label^="Change"], section[aria-label^="Forecast"]').evaluateAll((els) =>
            els.map((e) => Math.round(e.getBoundingClientRect().left)),
          );
          if (new Set(lefts).size > 1) findings.push(`mobile: summary cards do not stack (${lefts.join(",")})`);
        }
        await page.waitForTimeout(400);
        await shot(page, "aws-finops", viewport, theme, "overview");
        await textSizes(page, `finops/${viewport.name}/${theme}`, findings);

        // Every ⓘ opens by keyboard and closes with Escape.
        const tips = main.locator("button[aria-expanded]").filter({ has: page.locator("svg.lucide-info") });
        const tipCount = await tips.count();
        test.info().annotations.push({ type: "info-buttons", description: String(tipCount) });
        const pageUrl = page.url();
        for (let i = 0; i < tipCount; i += 1) {
          const tip = tips.nth(i);
          const tipName = (await tip.getAttribute("aria-label")) ?? "";
          await expect(tip).toHaveAttribute("aria-label", /\S/);
          await tip.focus();
          await page.keyboard.press("Enter");
          await page.waitForTimeout(300);
          if (page.url() !== pageUrl) {
            findings.push(`ⓘ "${tipName}" sits inside a row link: Enter navigates to ${new URL(page.url()).pathname} instead of opening the note`);
            await shot(page, "aws-finops", viewport, theme, `info-${i}-navigated`, false);
            await page.goto(pageUrl);
            await expect(main.getByRole("heading", { name: "What moved the cost" })).toBeVisible();
            continue;
          }
          const note = page.locator('[role="note"]').first();
          await expect(note).toBeVisible();
          const noteText = await note.innerText();
          if (partial && noteText.includes(`${s.compare!.short} total`)) findings.push(`QA-403: ⓘ "${tipName}" says "${s.compare!.short} total" for ${partial}`);
          const vbox = await note.boundingBox();
          if (vbox && (vbox.y < 0 || vbox.y + vbox.height > page.viewportSize()!.height + 1)) findings.push(`ⓘ "${tipName}" note runs off screen vertically`);
          const box = await note.boundingBox();
          if (box && (box.x < 0 || box.x + box.width > viewport.width + 1)) {
            findings.push(`ⓘ "${tipName}" popover runs off screen (${Math.round(box.x)}..${Math.round(box.x + box.width)})`);
            await shot(page, "aws-finops", viewport, theme, `info-${i}-offscreen`, false);
          }
          if (i === 0) await shot(page, "aws-finops", viewport, theme, "info-open", false);
          if (/^About Change/.test(tipName)) await shot(page, "aws-finops", viewport, theme, "info-change-open", false);
          await page.keyboard.press("Escape");
          await expect(page.locator('[role="note"]')).toHaveCount(0);
        }

        // Freshness popover holds the lag and the paid use.
        const fresh = page.getByRole("button", { name: /^(Updated|Stale since)/ });
        if ((await fresh.count()) > 0) {
          await fresh.first().click();
          await expect(page.locator('[role="note"]').getByText(/Cost data to/)).toBeVisible();
          const fbox = await page.locator('[role="note"]').first().boundingBox();
          if (fbox && (fbox.x < 0 || fbox.x + fbox.width > viewport.width + 1)) {
            findings.push(`freshness note runs off screen (${Math.round(fbox.x)}..${Math.round(fbox.x + fbox.width)})`);
          }
          await shot(page, "aws-finops", viewport, theme, "freshness-open", false);
          await fresh.first().click();
        }

        // Keyboard: every stop visible and ringed; ⓘ and header controls reachable.
        const stops = await keyboardWalk(page, `finops/${viewport.name}/${theme}`, findings);
        fs.writeFileSync(path.join(harnessResultsDir(), `finops-tab-order-${viewport.name}-${theme}.json`), JSON.stringify(stops, null, 2));
        if (tipCount > 0 && !stops.some((n) => /^About /.test(n))) findings.push("keyboard: no ⓘ reached by Tab");
        if (!stops.some((n) => /^Export/.test(n))) findings.push("keyboard: Export not reached by Tab");

        // Details depth: labels may change, numbers do not.
        if (viewport.name === "desktop") {
          const before = await main.locator('section[aria-label^="Change"], section[aria-label^="Forecast"]').allInnerTexts();
          await page.getByRole("group", { name: "Depth" }).getByRole("button", { name: "Details", exact: true }).click();
          await expect(page).toHaveURL(/depth=details/);
          await page.waitForLoadState("networkidle");
          await page
            .getByRole("group", { name: "Depth" })
            .locator('button[aria-pressed="true"]', { hasText: "Details" })
            .waitFor({ timeout: 5_000 })
            .catch(() => {});
          const pressed = await page.getByRole("group", { name: "Depth" }).getByRole("button", { name: "Details", exact: true }).getAttribute("aria-pressed");
          if (pressed !== "true") {
            const pressedNow = await page.getByRole("group", { name: "Depth" }).locator('button[aria-pressed="true"]').allInnerTexts();
            findings.push(`Details depth: after the click the toggle shows ${JSON.stringify(pressedNow)} pressed; URL ${new URL(page.url()).search}`);
            await page.reload();
            await expect(main.getByRole("heading", { name: "What moved the cost" })).toBeVisible();
            const afterReload = await page.getByRole("group", { name: "Depth" }).locator('button[aria-pressed="true"]').allInnerTexts();
            findings.push(`Details depth: after a reload of ${new URL(page.url()).search} the toggle shows ${JSON.stringify(afterReload)} pressed`);
          }
          const after = await main.locator('section[aria-label^="Change"], section[aria-label^="Forecast"]').allInnerTexts();
          const nums = (xs: string[]) => xs.join(" ").match(/[-+−]?\$[\d,.]+/g) ?? [];
          if (JSON.stringify(nums(before)) !== JSON.stringify(nums(after))) findings.push(`Details depth changed card numbers: ${nums(before)} -> ${nums(after)}`);
          await shot(page, "aws-finops", viewport, theme, "details");
          await page.getByRole("group", { name: "Depth" }).getByRole("button", { name: "Summary", exact: true }).click();
        }

        // Export menu: PDF, CSV, Copy summary (+ Save, disabled with a reason for an open month).
        const exportBtn = page.getByRole("button", { name: "Export" });
        await exportBtn.click();
        const menu = page.getByRole("menu");
        await expect(menu.getByRole("menuitem", { name: /PDF/ })).toBeVisible();
        await expect(menu.getByRole("menuitem", { name: /CSV/ })).toBeVisible();
        await expect(menu.getByRole("menuitem", { name: /Copy summary/ })).toBeVisible();
        await shot(page, "aws-finops", viewport, theme, "export-menu", false);
        if (viewport.name === "desktop" && theme === "dark") {
          await menu.getByRole("menuitem", { name: /Copy summary/ }).click();
          await expect(toastBox(page).getByText(/Summary copied|Copy failed/)).toBeVisible();
          test.info().annotations.push({ type: "copy-summary-toast", description: (await toastBox(page).innerText()).split("\n")[0] });
          await shot(page, "aws-finops", viewport, theme, "toast-copy-summary", false);
        } else {
          await page.keyboard.press("Escape");
        }
      }
      await audit(page, `finops/${viewport.name}/${theme}`, findings, { quiet: true });
      recordFindings(test.info().title, findings);
      expect.soft(findings, "FinOps page layout, naming and quiet text").toEqual([]);
    });
  }
}

test("finops depth toggle on a fresh page (desktop, dark)", async ({ browser }) => {
  const page = await open(browser, VIEWPORTS[0], "dark", "/aws/finops");
  const live = await liveState(page);
  test.skip(!live.on, "no cost data");
  const depth = page.getByRole("group", { name: "Depth" });
  await expect(depth.getByRole("button", { name: "Summary", exact: true })).toHaveAttribute("aria-pressed", "true");
  await depth.getByRole("button", { name: "Details", exact: true }).click();
  await expect(page).toHaveURL(/depth=details/);
  await expect.soft(depth.getByRole("button", { name: "Details", exact: true }), "Details pressed after one click").toHaveAttribute("aria-pressed", "true", { timeout: 5_000 });
  await shot(page, "aws-finops", VIEWPORTS[0], "dark", "depth-click-fresh", false);
  await depth.getByRole("button", { name: "Summary", exact: true }).click();
  await expect.soft(page, "Summary click drops depth from the URL").not.toHaveURL(/depth=details/);
  await expect.soft(depth.getByRole("button", { name: "Summary", exact: true }), "Summary pressed after one click").toHaveAttribute("aria-pressed", "true", { timeout: 5_000 });
});

// ---------------------------------------------------------------------------
// Increase detail (only when the account has one) and the paid drill-down
// ---------------------------------------------------------------------------

for (const viewport of VIEWPORTS) {
  for (const theme of THEMES) {
    test(`increase detail (${viewport.name}, ${theme})`, async ({ browser }) => {
      const findings: string[] = [];
      const page = await open(browser, viewport, theme, "/aws/finops");
      const live = await liveState(page);
      const fromBridge = (live.bridge?.rows ?? []).find((r) => r.increase_id)?.increase_id;
      const id = fromBridge ?? live.increases?.items?.[0]?.id;
      test.skip(!id, "no cost increase in this account (Learning, or nothing over the threshold): not triggered by real data");
      await page.goto(`/aws/finops/increases/${encodeURIComponent(id!)}`);
      const main = page.locator("main");
      const detailJson = await getJson<FinOpsIncreaseDetailView>(page, `/api/admin/aws/finops/increases/${encodeURIComponent(id!)}`);
      const inc = detailJson.increase;
      const h1 = main.locator("h1").filter({ hasText: /went (up|down)|increase/i });
      await expect(h1).toBeVisible();
      const heading = await h1.innerText();
      test.info().annotations.push({ type: "increase", description: `status ${inc.status}; heading says ${/\$0\b/.test(heading) ? "$0" : "an amount"}` });
      if (/went (up|down) \$0\b/.test(heading)) findings.push(`heading states a zero change: "${heading.replace(/[0-9]{12}/g, "<account>").slice(0, 80)}"`);
      await expect(main.getByText(/^Started /).first()).toBeVisible();
      await expect(main.getByText(/^Owner: /).first()).toBeVisible();
      await expect(main.getByRole("heading", { name: "Why" })).toBeVisible();
      await expect(main.getByRole("heading", { name: "Who" })).toBeVisible();
      await expect(main.getByRole("heading", { name: "Where the increase came from" })).toBeVisible();
      if (inc.status === "open") await expect(main.getByRole("button", { name: /Mark expected/ })).toBeVisible();
      if (inc.status !== "open" && (await main.getByText(/\/mo from now/).count()) > 0) {
        findings.push(`a ${inc.status} increase still shows "${await main.getByText(/\/mo from now/).first().innerText()}"`);
      }
      const gapLines = await main.getByText("Resource data off").count();
      if (gapLines > 1) findings.push(`quiet text: "Resource data off" repeated on ${gapLines} cause rows`);
      // QA-410: the whole stated amount is highlighted, as the sentence writes it.
      const hl = h1.locator("span.font-mono");
      if ((await hl.count()) > 0) {
        const hlText = (await hl.first().innerText()).trim();
        const token = heading.match(/[+−-]?\$[\d,]+(?:\.\d+)?/)?.[0] ?? "";
        test.info().annotations.push({ type: "heading-highlight", description: `${hlText} (sentence token ${token})` });
        if (hlText !== token) findings.push(`QA-410: heading highlights "${hlText}" but the sentence states "${token}"`);
      }
      // QA-410: chart axis labels inside the chart box.
      const clipped = await main.locator("svg").evaluateAll((svgs) =>
        svgs.flatMap((svg) => {
          const box = svg.getBoundingClientRect();
          return Array.from(svg.querySelectorAll("text"))
            .filter((t) => (t.textContent ?? "").trim())
            .filter((t) => {
              const r = t.getBoundingClientRect();
              return r.width > 0 && (r.right > box.right + 0.5 || r.left < box.left - 0.5);
            })
            .map((t) => (t.textContent ?? "").trim());
        }),
      );
      if (clipped.length > 0) findings.push(`QA-410: chart labels run outside the chart: ${clipped.join(", ")}`);
      // QA-404/QA-408: no zero amount anywhere on the detail (heading, aside, cause rows).
      const detailText = await textOutsideCharts(main);
      const zeroLine = detailText.split("\n").find((l) => ZERO_MONEY.test(l));
      if (zeroLine) findings.push(`zero amount on the increase detail: "${zeroLine.replace(/[0-9]{12}/g, "<account>").trim().slice(0, 80)}"`);
      test.info().annotations.push({ type: "usage-signal", description: detailJson.usage_signal ? `${detailJson.usage_signal.label} [${detailJson.usage_signal.unit}]` : "none" });
      // QA-408: chart text legible at 390 px (rendered glyph height).
      const tiny = await main.locator("svg text").evaluateAll((els) =>
        els.filter((e) => (e.textContent ?? "").trim() && e.getBoundingClientRect().height > 0 && e.getBoundingClientRect().height < 9).map((e) => `${(e.textContent ?? "").trim()} (${e.getBoundingClientRect().height.toFixed(1)}px)`),
      );
      if (tiny.length > 0) findings.push(`QA-408: chart text under 9 px tall: ${tiny.slice(0, 4).join(", ")}`);
      await page.waitForTimeout(400);
      await shot(page, "aws-finops-increase", viewport, theme, "detail");
      if (ALLOW_DRILL && viewport.name === "desktop" && theme === "dark") {
        const drill = main.getByRole("button", { name: /Read exact detail from AWS/ });
        if ((await drill.count()) > 0) {
          await drill.click();
          await expect(page.getByRole("dialog").getByText(/1 paid Cost Explorer request/)).toBeVisible();
          await shot(page, "aws-finops-increase", viewport, theme, "drill-confirm", false);
          await page.getByRole("button", { name: /Read for about \$0\.01/ }).click();
          await expect(toastBox(page).locator("div.text-xs.font-medium").first()).toBeVisible({ timeout: 60_000 });
          test.info().annotations.push({ type: "drill-toast", description: await toastBox(page).innerText() });
          await shot(page, "aws-finops-increase", viewport, theme, "drill-toast", false);
        }
      }
      await audit(page, `increase/${viewport.name}/${theme}`, findings, { quiet: true });
      recordFindings(test.info().title, findings);
      expect.soft(findings, "increase detail layout and naming").toEqual([]);
    });
  }
}

// QA-404: every increase the account has, not only the one the bridge links.
test("every increase states no zero and no forward impact once resolved (desktop, dark)", async ({ browser }) => {
  const findings: string[] = [];
  const page = await open(browser, VIEWPORTS[0], "dark", "/aws/finops");
  const live = await liveState(page);
  const items = live.increases?.items ?? [];
  test.skip(items.length === 0, "no cost increase in this account");
  for (const it of items) {
    for (const [what, text] of [["title", it.title], ["sentence", it.sentence]] as const) {
      if (ZERO_MONEY.test(text ?? "")) findings.push(`increase ${it.status}: API ${what} states a zero amount`);
      if (/went up/.test(text ?? "") && it.stated_change_micros < 0) findings.push(`increase ${it.status}: ${what} says "went up" with a negative stated change`);
    }
    await page.goto(`/aws/finops/increases/${encodeURIComponent(it.id)}`);
    const main = page.locator("main");
    await expect(main.getByRole("heading", { name: "Why" })).toBeVisible();
    const text = await textOutsideCharts(main);
    const zero = text.split("\n").find((l) => ZERO_MONEY.test(l));
    if (zero) findings.push(`increase ${it.status} (${it.id}): zero amount "${zero.replace(/[0-9]{12}/g, "<account>").trim().slice(0, 80)}"`);
    if (it.status !== "open" && /\/mo from now/.test(text)) findings.push(`increase ${it.status} (${it.id}): shows "/mo from now"`);
    const crumb = await main.getByRole("navigation", { name: "Breadcrumb" }).innerText();
    if (ZERO_MONEY.test(crumb)) findings.push(`increase ${it.id}: breadcrumb states a zero amount`);
  }
  test.info().annotations.push({ type: "increases-checked", description: `${items.length} (${items.map((i) => i.status).join(", ")})` });
  await audit(page, "increases/all", findings, { quiet: true });
  recordFindings(test.info().title, findings);
  expect.soft(findings, "every increase").toEqual([]);
});

// ---------------------------------------------------------------------------
// Print / PDF report route
// ---------------------------------------------------------------------------

for (const viewport of VIEWPORTS) {
  test(`report print route (${viewport.name})`, async ({ browser }) => {
    const findings: string[] = [];
    const theme: Theme = "dark"; // the report forces light itself
    const page = await open(browser, viewport, theme, "/aws/finops");
    const live = await liveState(page);
    test.skip(!live.on, "no cost data in the account: the report has nothing to print");
    const key = live.summary.period?.key ?? "mtd";
    await page.goto(`/aws/finops/report?period=${encodeURIComponent(key)}`);
    const report = page.locator(".finops-report");
    await expect(report).toHaveAttribute("data-theme", "light");
    await expect(report.locator("h1")).toBeVisible();
    await expect(report.locator('section[data-page="1"]')).toBeVisible();
    await expect(report.locator('section[data-page="2"]')).toBeVisible();
    await expect(report.locator('section[aria-label="What moved the cost"]')).toBeVisible();
    await expect(report.locator('section[aria-label="About this report"]')).toBeVisible();
    // Real text, not images.
    await expect(report.locator("img, canvas")).toHaveCount(0);
    if (live.bridge?.resource_note) {
      await expect(report.getByText(/Resources not named\*/).first()).toBeVisible();
    }
    await page.waitForTimeout(300);
    const reportHeight = await page.evaluate(() => {
      const r = document.querySelector(".finops-report");
      let h = document.documentElement.scrollHeight;
      for (let el: Element | null = r; el; el = el.parentElement) h = Math.max(h, (el as HTMLElement).scrollHeight);
      return h + 40;
    });
    // QA-403/404/406/409 on the report text.
    const reportText = await report.innerText();
    const s = live.summary;
    const partial = s.compare?.available ? partialLabel(s.compare.range, Number(s.period!.range.start.slice(0, 4))) : "";
    if (partial && reportText.includes(`${s.compare!.short} total`)) findings.push(`QA-403: report says "${s.compare!.short} total" for ${partial}`);
    if (partial && !reportText.includes(partial)) findings.push(`QA-403: report never names the base days ${partial}`);
    const zero = reportText.split("\n").find((l) => ZERO_MONEY.test(l));
    if (zero) findings.push(`report states a zero amount: "${zero.replace(/[0-9]{12}/g, "<account>").trim().slice(0, 80)}"`);
    for (const [re, why] of SAVING_TITLE_LEAKS) if (re.test(reportText)) findings.push(`report shows ${why}`);
    // QA-407: a window taller than the sheet stays light below it.
    await page.setViewportSize({ width: viewport.width, height: Math.max(viewport.height, reportHeight) + 300 });
    await page.waitForTimeout(200);
    const below = await page.evaluate(() => {
      const el = document.elementFromPoint(window.innerWidth / 2, window.innerHeight - 4);
      for (let e: Element | null = el; e; e = e.parentElement) {
        const bg = getComputedStyle(e).backgroundColor;
        const m = bg.match(/rgba?\((\d+), (\d+), (\d+)(?:, ([\d.]+))?\)/);
        if (m && (m[4] === undefined || Number(m[4]) > 0.5)) return { bg, lum: (0.2126 * +m[1] + 0.7152 * +m[2] + 0.0722 * +m[3]) / 255 };
      }
      const html = getComputedStyle(document.documentElement).backgroundColor;
      return { bg: html, lum: -1 };
    });
    test.info().annotations.push({ type: "report-below-sheet", description: `${below.bg} (lum ${below.lum.toFixed(2)})` });
    if (below.lum >= 0 && below.lum < 0.5) findings.push(`QA-407: area below the light sheet is dark (${below.bg})`);
    await shot(page, "aws-finops-report", viewport, "light", "screen", false);
    await page.setViewportSize({ width: viewport.width, height: viewport.height });
    if (viewport.name === "desktop") {
      await page.emulateMedia({ media: "print" });
      const pdfPath = path.join(captureDir(), "aws-finops-report-a4.pdf");
      const pdf = await page.pdf({ path: pdfPath, format: "A4", printBackground: true });
      const raw = pdf.toString("latin1");
      const pages = (raw.match(/\/Type\s*\/Page(?!s)/g) ?? []).length;
      const fonts = (raw.match(/\/Font\b/g) ?? []).length;
      test.info().annotations.push({ type: "pdf", description: `${pages} pages, ${pdf.length} bytes, font refs ${fonts}` });
      if (pages !== 2) findings.push(`PDF has ${pages} pages, want 2`);
      if (fonts === 0) findings.push("PDF has no font (text is not real text)");
      await page.emulateMedia({ media: "screen" });
    }
    await audit(page, `report/${viewport.name}`, findings);
    recordFindings(test.info().title, findings);
    expect.soft(findings, "report layout and PDF").toEqual([]);
  });
}

// ---------------------------------------------------------------------------
// Toasts real data cannot trigger safely (fixture-patched single responses)
// ---------------------------------------------------------------------------

// QA-401 and QA-402: states the real account cannot reach again without
// wiping its data (cost reads off; before the first account check). One
// patched response each; everything else on the page is live.
for (const viewport of VIEWPORTS) {
  for (const theme of THEMES) {
    test(`cost-off and first-check states (fixture: summary/settings patched) (${viewport.name}, ${theme})`, async ({ browser }) => {
      const findings: string[] = [];
      const page = await open(browser, viewport, theme, "/now");
      let reason = "cost_data_off";
      const summaryUrl = (url: URL) => url.pathname === "/api/admin/aws/finops/summary";
      await page.route(
        summaryUrl,
        (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ enabled: false, reason, links: { ui: "/aws/finops" } }) }),
      );
      // QA-401: cost reads off at 390 px; both cards inside the gutters.
      await page.goto("/aws/finops");
      const main = page.locator("main");
      await expect(main.getByText(/Cost Explorer reads: off/i)).toBeVisible();
      await page.waitForTimeout(300);
      await shot(page, "aws-finops", viewport, theme, "cost-off-fixture");
      await audit(page, `cost-off/${viewport.name}/${theme}`, findings);
      // QA-402: no account ready yet: the page names the cause and the fix.
      reason = "no_connected_accounts";
      await page.goto("/aws/finops");
      await expect(main.getByText("No account ready to read cost yet")).toBeVisible();
      await expect(main.getByRole("link", { name: /AWS settings/ })).toBeVisible();
      await shot(page, "aws-finops", viewport, theme, "off-no-connected-accounts-fixture");
      await audit(page, `no-connected/${viewport.name}/${theme}`, findings);
      // QA-402: Settings before the first check: waiting words, never zero counts or "All good".
      await page.route(
        (url) => url.pathname === "/api/admin/aws/settings",
        async (route) => {
          const res = await route.fetch();
          const body = (await res.json()) as AwsSettings;
          body.accounts = (body.accounts ?? []).map((a) => ({
            ...a,
            // As seen live on 2026-11-11 before the first check: connected, no check yet, nothing counted.
            status: "connected" as const,
            disconnected_since: null,
            error_code: null,
            checked_at: null,
            services: { ...a.services, readable: 0 },
            regions: { selected: [], enabled: 0, not_enabled: [] },
            denied_services: [],
            sync: a.sync ? { ...a.sync, state: "pending" as const, synced_at: null, services: [] } : a.sync,
          }));
          await route.fulfill({ response: res, json: body });
        },
      );
      await page.goto("/aws/settings");
      const accounts = page.locator('section[aria-labelledby="aws-accounts-title"]');
      await expect(accounts.getByText("Harness account").first()).toBeVisible();
      await page.waitForTimeout(300);
      const text = await page.locator("main").innerText();
      test.info().annotations.push({ type: "settings-first-check", description: text.split("\n").filter((l) => /waiting|All good|of \d+|ready/i.test(l)).slice(0, 6).join(" | ") });
      // The API-use meter ("0 of 36 req/s") is a measured rate, not a count.
      const zeroCount = text.split("\n").find((l) => /\b0 of \d+\b(?! req\/s)/.test(l));
      if (zeroCount) findings.push(`QA-402: zero count before the first check: "${zeroCount}"`);
      if (/All good/.test(text)) findings.push("QA-402: \"All good\" before the first check");
      if (!/waiting/i.test(text)) findings.push("QA-402: no waiting words before the first check");
      await shot(page, "aws-settings-live", viewport, theme, "first-check-fixture");
      await audit(page, `first-check/${viewport.name}/${theme}`, findings);
      // QA-406: only a commitment found (as seen live right after the first
      // sync): no total, never a green $0, and the commitment line is shown.
      await page.unroute(summaryUrl);
      const liveSummary = await getJson<FinOpsSummaryView>(page, "/api/admin/aws/finops/summary");
      if (!liveSummary.enabled) {
        test.info().annotations.push({ type: "commitment-only", description: `not run: live summary is off (${liveSummary.reason})` });
      } else {
        await page.route(
          (url) => url.pathname === "/api/admin/aws/finops/savings",
          async (route) => {
            const res = await route.fetch();
            const body = (await res.json()) as FinOpsSavingsView;
            body.items = (body.items ?? []).filter((i) => i.kind === "commitment");
            body.total = { ...body.total, monthly_micros: 0, count: 0 };
            await route.fulfill({ response: res, json: body });
          },
        );
        await page.goto("/aws/finops");
        const savingsCard = page.locator("main section").filter({ has: page.getByRole("heading", { name: "Savings found" }) }).last();
        await expect(savingsCard).toBeVisible();
        const savingsText = await savingsCard.innerText();
        if (ZERO_MONEY.test(savingsText)) findings.push(`QA-406: commitment-only savings show a zero amount`);
        if (!/up to \$[\d,]+ a month, before other savings/i.test(savingsText)) findings.push("QA-406: commitment-only savings miss the commitment line");
        await shot(page, "aws-finops", viewport, theme, "savings-commitment-only-fixture");
      }
      recordFindings(test.info().title, findings);
      expect.soft(findings, "cost-off and first-check states").toEqual([]);
    });
  }
}

// B123: codes that used to fall back to "The AWS request failed" are worded.
// The drill-down POST is answered in the browser, so nothing reaches AWS.
test("drill-down errors are worded (fixture: the POST is answered in the browser)", async ({ browser }) => {
  const findings: string[] = [];
  const viewport = VIEWPORTS[0];
  const theme: Theme = "dark";
  const page = await open(browser, viewport, theme, "/aws/finops");
  const live = await liveState(page);
  const id = (live.bridge?.rows ?? []).find((r) => r.increase_id)?.increase_id ?? live.increases?.items?.[0]?.id;
  test.skip(!id, "no increase to open");
  let posts = 0;
  let code = "account_not_covered";
  await page.route(
    (url) => url.pathname === "/api/admin/aws/finops/drilldown",
    async (route) => {
      posts += 1;
      await route.fulfill({ status: 409, contentType: "application/json", body: JSON.stringify({ error: code }) });
    },
  );
  const cases: Array<[string, RegExp]> = [
    ["account_not_covered", /This account isn't in cost data/],
    ["paid_request_cap_reached", /\S/],
  ];
  for (const [c, want] of cases) {
    code = c;
    await page.goto(`/aws/finops/increases/${encodeURIComponent(id!)}`);
    await expect(page.locator("main").getByRole("heading", { name: "Where the increase came from" })).toBeVisible();
    const drill = page.locator("main").getByRole("button", { name: /Read exact detail from AWS/ });
    if ((await drill.count()) === 0) {
      test.info().annotations.push({ type: "drill", description: "no drill button on this increase" });
      break;
    }
    await drill.click();
    await page.getByRole("button", { name: /Read for about \$0\.01/ }).click();
    const toast = toastBox(page);
    await expect(toast.getByText(want).first()).toBeVisible();
    const text = await toast.innerText();
    test.info().annotations.push({ type: `toast-${c}`, description: text.replace(/\n/g, " | ") });
    if (/The AWS request failed/.test(text)) findings.push(`B123: ${c} still reads "The AWS request failed"`);
    if (new RegExp(`\\b${c}\\b`).test(text)) findings.push(`B123: raw code ${c} in the toast`);
    await shot(page, "aws-finops-increase", viewport, theme, `toast-${c}-fixture`, false);
  }
  expect(posts, "every drill POST was answered in the browser").toBeGreaterThan(0);
  recordFindings(test.info().title, findings);
  expect.soft(findings).toEqual([]);
});

test("account removal toast (fixture: the DELETE is answered in the browser)", async ({ browser }) => {
  const findings: string[] = [];
  const viewport = VIEWPORTS[0];
  const theme: Theme = "dark";
  const page = await open(browser, viewport, theme, "/now");
  // Show the real account as wizard-created so it offers Remove; answer the
  // DELETE in the browser so nothing is removed on the server.
  await page.route(
    (url) => url.pathname === "/api/admin/aws/settings",
    async (route) => {
      const res = await route.fetch();
      const body = (await res.json()) as AwsSettings;
      body.accounts = (body.accounts ?? []).map((a) => ({ ...a, source: "wizard" as const }));
      await route.fulfill({ response: res, json: body });
    },
  );
  let deletes = 0;
  await page.route(
    (url) => /^\/api\/admin\/aws\/accounts\/[^/]+$/.test(url.pathname),
    async (route) => {
      if (route.request().method() !== "DELETE") return route.fallback();
      deletes += 1;
      await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(deletes === 1 ? { purge: "pending" } : {}) });
    },
  );
  await page.goto("/aws/settings");
  const remove = page.getByRole("button", { name: /^Remove Harness account/ });
  await expect(remove).toBeVisible();
  await remove.click();
  await expect(page.getByRole("dialog").getByText(/Remove account\?/)).toBeVisible();
  await page.getByRole("dialog").getByRole("button", { name: "Remove", exact: true }).click();
  const toast = toastBox(page);
  await expect(toast.getByText("Account removed")).toBeVisible();
  await expect(toast.getByText("Cost data clears at the next refresh.")).toBeVisible();
  await shot(page, "aws-settings-live", viewport, theme, "toast-account-removed-fixture", false);
  expect(deletes).toBe(1);
  await audit(page, "removal-toast", findings);
  recordFindings(test.info().title, findings);
  expect.soft(findings).toEqual([]);
});

test("error toasts are worded (fixture: one failing response per action)", async ({ browser }) => {
  const findings: string[] = [];
  const viewport = VIEWPORTS[0];
  const theme: Theme = "dark";
  const page = await open(browser, viewport, theme, "/aws/settings");
  // Recheck: a refresh-in-progress conflict.
  await page.route(
    (url) => /\/api\/admin\/aws\/accounts\/[^/]+\/recheck$/.test(url.pathname),
    (route) => route.fulfill({ status: 409, contentType: "application/json", body: JSON.stringify({ error: "refresh_in_progress" }) }),
  );
  await page.getByRole("button", { name: /^Recheck Harness account/ }).click();
  await expect(toastBox(page).getByText("A cost refresh is running")).toBeVisible();
  await shot(page, "aws-settings-live", viewport, theme, "toast-refresh-running-fixture", false);
  // Copy summary: the report read fails with a FinOps code.
  await page.route(
    (url) => url.pathname === "/api/admin/aws/finops/report",
    (route) => route.fulfill({ status: 503, contentType: "application/json", body: JSON.stringify({ error: "store_unavailable" }) }),
  );
  await page.goto("/aws/finops");
  await expect(page.locator("main").getByRole("heading", { name: "What moved the cost" })).toBeVisible();
  const exportBtn = page.getByRole("button", { name: "Export" });
  {
    await exportBtn.click();
    await page.getByRole("menuitem", { name: /Copy summary/ }).click();
    await expect(toastBox(page).getByText("Copy failed")).toBeVisible();
    await expect(toastBox(page).getByText("Storage is unavailable")).toBeVisible();
  }
  await shot(page, "aws-finops", viewport, theme, "toast-error-fixture", false);
  // An unlisted code falls back to plain words.
  await page.route(
    (url) => /\/api\/admin\/aws\/accounts\/[^/]+\/recheck$/.test(url.pathname),
    (route) => route.fulfill({ status: 500, contentType: "application/json", body: JSON.stringify({ error: "weird_new_code" }) }),
  );
  await page.goto("/aws/settings");
  await page.getByRole("button", { name: /^Recheck Harness account/ }).click();
  await expect(toastBox(page).getByText("The AWS request failed")).toBeVisible();
  const text = await toastBox(page).innerText();
  if (/weird_new_code|refresh_in_progress|store_unavailable|\b[45]\d\d\b/.test(text)) findings.push(`raw code or status in toast: ${text}`);
  await shot(page, "aws-settings-live", viewport, theme, "toast-error-fixture", false);
  await audit(page, "error-toasts", findings);
  recordFindings(test.info().title, findings);
  expect.soft(findings).toEqual([]);
});

// QA-413: Dismiss with reason on an open saving. The savings list and the
// dismiss POST are answered in the browser, so no saving changes on the server.
for (const viewport of VIEWPORTS) {
  for (const theme of THEMES) {
    test(`dismiss a saving with a reason (fixture: savings list and dismiss POST answered in the browser) (${viewport.name}, ${theme})`, async ({ browser }) => {
      const findings: string[] = [];
    const page = await open(browser, viewport, theme, "/aws/finops");
    const liveSummary = await getJson<FinOpsSummaryView>(page, "/api/admin/aws/finops/summary");
    test.skip(!liveSummary.enabled, `live summary is off (${liveSummary.reason})`);
    const item: FinOpsSaving = {
      id: "fixture-idle-volume",
      kind: "idle",
      title: "Delete 2 unattached volumes",
      monthly_micros: 48_000_000,
      effort: "minutes",
      risk: "low",
      basis: "list_price",
      count: 2,
      line: "",
      owner: { name: "Unallocated", rule: "unallocated", rule_text: "" },
      status: "open",
      // A guide link sits beside Dismiss, as on real items (WCAG 2.5.8 spacing).
      guide: { doc_url: "https://docs.aws.amazon.com/ebs/latest/userguide/ebs-deleting-volume.html" },
    };
    let dismissed = false;
    await page.route(
      (url) => url.pathname === "/api/admin/aws/finops/savings",
      async (route) => {
        const res = await route.fetch();
        const body = (await res.json()) as FinOpsSavingsView;
        body.items = dismissed ? [] : [item];
        body.total = { ...body.total, monthly_micros: dismissed ? 0 : item.monthly_micros, count: dismissed ? 0 : 1 };
        await route.fulfill({ response: res, json: body });
      },
    );
    const posts: Array<{ reason?: string }> = [];
    await page.route(
      (url) => /^\/api\/admin\/aws\/finops\/savings\/[^/]+\/dismiss$/.test(url.pathname),
      async (route) => {
        posts.push(JSON.parse(route.request().postData() ?? "{}") as { reason?: string });
        if (posts.length === 1) {
          await route.fulfill({ status: 503, contentType: "application/json", body: JSON.stringify({ error: "store_unavailable" }) });
          return;
        }
        dismissed = true;
        await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ dismissed: true }) });
      },
    );
    await page.goto("/aws/finops");
    const card = page.locator("main section").filter({ has: page.getByRole("heading", { name: "Savings found" }) }).last();
    await expect(card.getByText(item.title)).toBeVisible();
    const dismiss = card.getByRole("button", { name: `Dismiss ${item.title}` });
    await expect(dismiss).toBeVisible();
    const target = await dismiss.boundingBox();
    test.info().annotations.push({ type: "dismiss-target", description: target ? `${Math.round(target.width)}x${Math.round(target.height)}` : "none" });
    // WCAG 2.5.8: an undersized target passes when a 24 px circle on its centre meets no other target.
    if (target && (target.width < 24 || target.height < 24)) {
      const crowded = await dismiss.evaluate((el) => {
        const r = el.getBoundingClientRect();
        const cx = r.left + r.width / 2;
        const cy = r.top + r.height / 2;
        const hits: string[] = [];
        for (const o of Array.from(document.querySelectorAll<HTMLElement>("a[href], button, input, [role='button']"))) {
          if (o === el || o.offsetParent === null) continue;
          const b = o.getBoundingClientRect();
          const dx = Math.max(b.left - cx, 0, cx - b.right);
          const dy = Math.max(b.top - cy, 0, cy - b.bottom);
          if (Math.hypot(dx, dy) < 12) hits.push((o.getAttribute("aria-label") || o.innerText || o.tagName).trim().slice(0, 40));
        }
        return hits;
      });
      if (crowded.length > 0) findings.push(`Dismiss target under 24x24 with a neighbour inside its 24 px circle: ${crowded.join(", ")}`);
    }

    // Escape closes the form and focus goes back to Dismiss.
    await dismiss.click();
    const dialog = page.getByRole("dialog", { name: "Dismiss saving" });
    await expect(dialog).toBeVisible();
    await page.keyboard.press("Escape");
    await expect(dialog).toBeHidden();
    await expect(dismiss).toBeFocused();

    // The reason is required: the submit stays disabled until one is typed.
    await dismiss.click();
    const submit = dialog.getByRole("button", { name: "Dismiss", exact: true });
    await expect(submit).toBeDisabled();
    await dialog.getByLabel("Reason").fill("   ");
    await expect(submit).toBeDisabled();
    await shot(page, "aws-finops", viewport, theme, "dismiss-saving-form-fixture", false);
    expect(posts.length).toBe(0);

    // A refused dismissal is worded and the form stays open.
    await dialog.getByLabel("Reason").fill("Volumes are kept for a restore test");
    await submit.click();
    await expect(toastBox(page).getByText("Storage is unavailable")).toBeVisible();
    await expect(dialog).toBeVisible();
    await shot(page, "aws-finops", viewport, theme, "dismiss-saving-error-fixture", false);

    // Success: the item leaves the list and the total.
    await submit.click();
    await expect(toastBox(page).getByText("Saving dismissed")).toBeVisible();
    await expect(dialog).toBeHidden();
    await expect(card.getByText(item.title)).toHaveCount(0);
    const cardText = await card.innerText();
    if (/\$48\/mo/.test(cardText)) findings.push("dismissed saving still counted in the total");
    if (ZERO_MONEY.test(cardText)) findings.push("Savings found shows a zero amount after the dismissal");
    expect(posts.map((p) => p.reason)).toEqual(["Volumes are kept for a restore test", "Volumes are kept for a restore test"]);
    await shot(page, "aws-finops", viewport, theme, "dismiss-saving-done-fixture", false);
    await audit(page, "dismiss-saving", findings);
    recordFindings(test.info().title, findings);
    expect.soft(findings).toEqual([]);
    });
  }
}
