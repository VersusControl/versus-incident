// AWS Changes browser acceptance on a real server: the page tells the truth
// about what was read, at every width and theme, and keeps telling it while a
// user leaves it open.
//
// Layout (A11 §9, task C12): a summary card under the scope bar holds one row
// of plain numbers (Changes, By people, Pipeline, AWS service, Failed calls,
// Accounts n/m), the likely-cause row with Show ›, the top actors on their
// own line, and, only when data in the window is really missing, one warning
// line whose popover lists every gap. The Regions status line, the answer
// sentence, the per-row "N read" notes, the gap and footer rows of the list
// and "Show more" are gone; the list loads more with "Load more" inside its
// own scroll, beside Compare at the same height.
//
// The stack is plans/harness-run/tests/aws/aws-changes.sh browser-up: the real
// server binary (harness_awsendpoint build, file storage, the quickstart
// config and the documented multi-account connectors) reading CloudTrail from
// tests/fakeaws through lib/fakeaws-front.mjs. Nothing reaches AWS, and no
// response is intercepted here (one test ages a cursor and says so): every
// assertion is on what the server sent and the page rendered. aws-changes.sh
// real-up boots the same stack on one real account instead (founder-approved
// runs only); set AWS_CHANGES_MASK_FILE to its account-id file there so captures
// mask the account ID.
//
// Captures use capturePath (<surface>-<viewport>-<theme>-<state>.png) into the
// run's HARNESS_IMAGES_DIR; per-test findings go to
// $HARNESS_RESULTS_DIR/aws-changes-live.jsonl. The state label is the scenario
// the stack serves (AWS_CHANGES_BROWSER_DIR/scenario), or AWS_CHANGES_STATE.
// AWS_CHANGES_SURFACE renames the surface (e.g. a task prefix);
// AWS_CHANGES_VIEWPORTS=desktop or mobile narrows the widths.
//
// Run: plans/harness-run/tests/aws/aws-changes.sh browser-specs (sets
// E2E_AWS_CHANGES=1). AWS_CHANGES_WATCH_MIN sets the watch length.
import { expect, test, type Browser, type BrowserContext, type Page, type Route } from "@playwright/test";
import * as fs from "node:fs";
import * as path from "node:path";
import { openApp } from "./helpers";
import { capturePath, harnessResultsDir } from "./support/capture";

const LIVE = process.env.E2E_AWS_CHANGES === "1";
test.skip(!LIVE, "needs the aws-changes browser stack (aws-changes.sh browser-up)");

const ALL_VIEWPORTS = [
  { name: "desktop", width: 1440, height: 900 },
  { name: "mobile", width: 390, height: 844 },
] as const;
const WANT_VIEWPORTS = (process.env.AWS_CHANGES_VIEWPORTS ?? "").split(",").map((v) => v.trim()).filter(Boolean);
const VIEWPORTS = ALL_VIEWPORTS.filter((v) => WANT_VIEWPORTS.length === 0 || WANT_VIEWPORTS.includes(v.name));
const DESKTOP = ALL_VIEWPORTS[0];
const MOBILE = ALL_VIEWPORTS[1];
const MOBILE_ON = VIEWPORTS.some((v) => v.name === "mobile");
const THEMES = ["dark", "light"] as const;
type Theme = (typeof THEMES)[number];
type Viewport = (typeof ALL_VIEWPORTS)[number];
const SURFACE = process.env.AWS_CHANGES_SURFACE || "aws-changes";

function stackScenario(): string {
  const dir = process.env.AWS_CHANGES_BROWSER_DIR ?? "";
  try {
    return fs.readFileSync(path.join(dir, "scenario"), "utf8").trim();
  } catch {
    return "unknown";
  }
}
const STATE = process.env.AWS_CHANGES_STATE || stackScenario();

// Wire keys of the closed vocabularies; none may reach the screen as text.
const RAW_CODES = [
  "cloudtrail_denied", "too_busy", "collector_down", "not_checked", "before_connect", "beyond_retention",
  "region_not_enabled", "credentials_invalid", "account_disconnected", "assume_role_failed", "account_mismatch",
  "global_unknown", "window_locked", "first_read_pending", "new_only", "before_status", "resource_unknown",
  "start_unknown", "no_alarm", "aws_service", "ThrottlingException", "AccessDeniedException", "InvalidClientTokenId",
  "settled_until", "total_is_lower_bound", "[object Object]", "undefined", "NaN",
  // Values that must never render (fixture canaries, email principals), comma-separated.
  ...(process.env.AWS_CHANGES_FORBID ?? "").split(",").map((v) => v.trim()).filter(Boolean),
];

// §9.6: words the redesign removed. None may appear on the page, in an open
// popover or in a dropdown list.
const BANNED: Array<[string, RegExp]> = [
  ["Partial", /\bpartial\b/i],
  ["at least", /\bat least\b/i],
  ["N read", /\b\d[\d,]* read\b/i],
  ["N+", /\b\d[\d,]*\+(?![\d])/],
  ["still arriving", /still arriving/i],
  ["Suspect", /\bsuspect/i],
  ["Read HH:MM", /\bRead \d{1,2}:\d{2}\b/],
  // Amendment 10 (QA-509): the pipeline class reads "Automation".
  ["Pipeline", /\bPipeline\b/],
];

function banned(where: string, text: string): string[] {
  const out: string[] = [];
  for (const [name, re] of BANNED) {
    const m = text.match(re);
    if (m) {
      const at = m.index ?? 0;
      out.push(`banned "${name}" in ${where}: "…${text.slice(Math.max(0, at - 40), at + 40).replace(/\s+/g, " ")}…"`);
    }
  }
  return out;
}

const contexts: BrowserContext[] = [];
test.afterEach(async () => {
  for (const c of contexts.splice(0)) await c.close().catch(() => {});
});

interface Watch {
  console: string[];
  http: string[];
}

async function open(
  browser: Browser,
  viewport: Viewport,
  theme: Theme,
  url: string,
  watch: Watch,
  extra: { timezoneId?: string; hasTouch?: boolean } = {},
): Promise<Page> {
  const context = await browser.newContext({ viewport: { width: viewport.width, height: viewport.height }, colorScheme: theme, ...extra });
  contexts.push(context);
  await context.addInitScript((value) => localStorage.setItem("versus.theme", value), theme);
  const page = await context.newPage();
  page.on("console", (m) => {
    // The shell's SSO deployment probe 404s on every OSS load (QA-447); not this page's.
    if (m.type() === "error" && !(m.location()?.url ?? "").includes("/enterprise/api/sso/deployment")) watch.console.push(m.text().slice(0, 200));
  });
  page.on("response", (r) => {
    if (r.url().includes("/api/") && r.status() >= 400) watch.http.push(`${r.status()} ${new URL(r.url()).pathname}${new URL(r.url()).search}`);
  });
  await page.emulateMedia({ colorScheme: theme });
  await openApp(page, url);
  // Sign-in probes (the shell asks for agent config and SSO before a session
  // exists) are not this page's; record from an authenticated load on.
  watch.console.length = 0;
  watch.http.length = 0;
  await page.goto(url);
  return page;
}

/** The summary card is on screen (it replaced the answer line). */
async function settled(page: Page) {
  await expect(page.locator("[data-summary-card]")).toBeVisible({ timeout: 30_000 });
  await page.waitForLoadState("networkidle").catch(() => {});
}

// AWS_CHANGES_MASK_FILE (real-AWS runs): a file holding the account ID
// (aws-changes.sh real-up's run-owned account-id). Every element whose text
// holds it (an account label, an ARN) is masked in captures.
const MASK_TEXT = (() => {
  const file = process.env.AWS_CHANGES_MASK_FILE ?? "";
  if (!file) return "";
  const id = fs.readFileSync(file, "utf8").trim();
  if (!/^\d{12}$/.test(id)) throw new Error("AWS_CHANGES_MASK_FILE does not hold a 12-digit account ID");
  return id;
})();
const masks = (page: Page) => (MASK_TEXT ? [page.getByText(MASK_TEXT)] : []);
// Playwright's own failure screenshot takes no mask: with a real account ID
// on screen it would keep the ID unmasked in the images and the HTML report.
if (MASK_TEXT) test.use({ screenshot: "off" });

async function shot(page: Page, viewport: Viewport, theme: Theme, state: string) {
  const out = capturePath(SURFACE, viewport.width, theme, state);
  const extra = await page.evaluate(() => {
    const m = document.querySelector("main");
    return m ? Math.max(0, m.scrollHeight - m.clientHeight) : 0;
  });
  if (extra > 0) await page.setViewportSize({ width: viewport.width, height: viewport.height + extra });
  await page.waitForTimeout(250);
  await page.screenshot({ path: out, animations: "disabled", fullPage: true, mask: masks(page) });
  if (extra > 0) await page.setViewportSize({ width: viewport.width, height: viewport.height });
  return out;
}

/** A viewport-only capture, for open popovers and lists (a resize would close or move them). */
async function shotView(page: Page, viewport: Viewport, theme: Theme, state: string) {
  const out = capturePath(SURFACE, viewport.width, theme, state);
  await page.waitForTimeout(200);
  await page.screenshot({ path: out, animations: "disabled", mask: masks(page) });
  return out;
}

function record(name: string, data: Record<string, unknown>) {
  const file = path.join(harnessResultsDir(), "aws-changes-live.jsonl");
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.appendFileSync(file, JSON.stringify({ test: name, state: STATE, at: new Date().toISOString(), ...data }) + "\n");
}

const norm = (s: string) => s.replace(/\s+/g, " ").trim();

/** What the page shows that a user reads as numbers and states. */
async function readPage(page: Page) {
  return page.evaluate(() => {
    const t = (e: Element | null | undefined) => ((e as HTMLElement | null)?.innerText ?? "").replace(/\s+/g, " ").trim();
    const rect = (e: Element | null | undefined) => {
      if (!e) return null;
      const b = e.getBoundingClientRect();
      return { top: Math.round(b.top), bottom: Math.round(b.bottom), left: Math.round(b.left), right: Math.round(b.right), height: Math.round(b.height) };
    };
    const card = document.querySelector<HTMLElement>("[data-summary-card]");
    const numbers = Array.from(card?.querySelectorAll<HTMLElement>("[data-number]") ?? []).map((n) => ({
      key: n.getAttribute("data-number") ?? "",
      label: t(n.querySelector("dt")),
      value: t(n.querySelector("dd")),
      top: Math.round(n.getBoundingClientRect().top),
    }));
    const likely = card?.querySelector<HTMLElement>("[data-likely-cause]") ?? null;
    const topActors = card?.querySelector<HTMLElement>("[data-top-actors]") ?? null;
    const warningLine = card?.querySelector<HTMLElement>("[data-warning-line]") ?? null;
    const grid = document.querySelector<HTMLElement>('section[aria-labelledby="changes-grid-title"]');
    const table = grid?.querySelector<HTMLElement>('[role="table"]') ?? null;
    const header = grid?.firstElementChild as HTMLElement | null;
    // Text in the Activity card outside its header and its table: notes under the grid.
    let gridNotes = "";
    if (grid) {
      const clone = grid.cloneNode(true) as HTMLElement;
      clone.querySelector('[role="table"]')?.remove();
      clone.firstElementChild?.remove();
      gridNotes = (clone.textContent ?? "").replace(/\s+/g, " ").trim();
    }
    return {
      cardText: t(card),
      numbers,
      noChanges: t(card?.querySelector("[data-no-changes]")),
      warning: t(card?.querySelector("[data-warning-text]")),
      warningLine: t(warningLine),
      warningRect: rect(warningLine),
      warningIsLast: Boolean(warningLine && card && card.lastElementChild === warningLine),
      warningTrigger: Boolean(warningLine?.querySelector('button[aria-haspopup="dialog"]')),
      reconnect: warningLine?.querySelector("[data-reconnect]")?.getAttribute("href") ?? null,
      likely: likely
        ? {
            kind: likely.getAttribute("data-likely-cause"),
            text: t(likely),
            show: Array.from(likely.querySelectorAll("button")).some((b) => t(b) === "Show ›"),
            tagInCard: card?.querySelectorAll("[data-likely-tag]").length ?? 0,
            rect: rect(likely),
          }
        : null,
      topActorsText: t(topActors),
      topActors: Array.from(topActors?.querySelectorAll<HTMLElement>("[data-top-actor]") ?? []).map((e) => t(e)),
      topActorsRect: rect(topActors),
      topActorsInsideLikely: Boolean(likely && topActors && likely.contains(topActors)),
      rowLabels: Array.from(grid?.querySelectorAll<HTMLElement>('[role="rowheader"]') ?? []).map((e) => t(e)),
      rowTotals: Array.from(document.querySelectorAll<HTMLElement>("[data-row-total]")).map((e) => ({
        row: ((e.parentElement?.innerText ?? "").split("\n")[0] ?? "").trim(),
        total: e.innerText.trim(),
      })),
      alarmChips: Array.from(document.querySelectorAll<HTMLElement>("[data-alarms-chip]")).map((e) => t(e)),
      alarmLines: document.querySelectorAll("[data-alarm-line]").length,
      headerText: t(header),
      gridNotes,
      gridTableText: t(table).slice(0, 200),
      gapCells: Array.from(document.querySelectorAll<HTMLElement>('[role="cell"][data-state="gap"]')).map((e) => e.getAttribute("title") ?? "").slice(0, 40),
      bands: Array.from(document.querySelectorAll<HTMLElement>("[data-band]")).map((e) => ({ reason: e.getAttribute("data-band"), title: e.getAttribute("title") ?? "" })),
      partialCells: Array.from(document.querySelectorAll<HTMLElement>('[role="cell"][data-state="partial"]')).map((e) => e.innerText.trim()).filter(Boolean).slice(0, 6),
      arrivingOutline: document.querySelectorAll("[data-arriving-outline]").length,
      items: document.querySelectorAll("[data-item]").length,
      likelyTags: Array.from(document.querySelectorAll<HTMLElement>("[data-likely-tag]")).map((e) => `${e.getAttribute("data-likely-tag")}:${t(e)}`),
      // Removed by C12; each must be absent.
      removed: {
        answerLine: document.querySelectorAll("[data-answer-line]").length,
        regionRows: document.querySelectorAll("[data-region-row]").length,
        listFooter: document.querySelectorAll("[data-list-footer]").length,
        gapRows: document.querySelectorAll("[data-gap-row]").length,
        accountGapRows: document.querySelectorAll("[data-account-gap]").length,
        reconnectOutside: document.querySelectorAll("[data-reconnect-outside]").length,
        bandLabels: document.querySelectorAll("[data-band-label]").length,
        arrivingLabels: document.querySelectorAll("[data-arriving-label], [data-arriving-row]").length,
        partialNote: document.querySelectorAll("[data-partial-note]").length,
        readAt: document.querySelectorAll("[data-read-at]").length,
        showMore: Array.from(document.querySelectorAll<HTMLElement>("main button")).filter((b) => t(b) === "Show more").length,
      },
      empty: t(document.querySelector("[data-empty]")),
    };
  });
}
type View = Awaited<ReturnType<typeof readPage>>;

/** Layout and wording checks every screen must pass. */
async function audit(page: Page): Promise<string[]> {
  const out = await page.evaluate((codes) => {
    const out: string[] = [];
    const overflow = document.documentElement.scrollWidth - window.innerWidth;
    if (overflow > 1) out.push(`page overflows horizontally by ${overflow}px`);
    for (const el of Array.from(document.querySelectorAll<HTMLElement>("main section"))) {
      const box = el.getBoundingClientRect();
      if (box.width > 0 && box.right > window.innerWidth + 1) out.push(`section "${el.getAttribute("aria-labelledby") ?? el.getAttribute("aria-label") ?? ""}" runs ${Math.round(box.right - window.innerWidth)}px past the viewport`);
    }
    for (const el of Array.from(document.querySelectorAll<HTMLElement>("main .truncate"))) {
      if (el.offsetParent !== null && el.scrollWidth - el.clientWidth > 1 && !el.getAttribute("title") && !el.closest("button[aria-haspopup='listbox']")) {
        out.push(`clipped text without a title: "${(el.textContent ?? "").trim().slice(0, 60)}"`);
      }
    }
    const controls = Array.from(document.querySelectorAll<HTMLElement>("main button, main a[href], main [role='button'], main select, main input"))
      .filter((el) => el.offsetParent !== null);
    for (const el of controls) {
      const name = (el.getAttribute("aria-label") || el.innerText || el.getAttribute("title") || (el as HTMLInputElement).placeholder || "").trim();
      if (!name) out.push(`control without an accessible name: <${el.tagName.toLowerCase()} class="${el.className.toString().slice(0, 50)}">`);
      const r = el.getBoundingClientRect();
      if (r.width > 0 && (r.width < 16 || r.height < 16)) out.push(`tiny target ${Math.round(r.width)}x${Math.round(r.height)}: "${name.slice(0, 40)}"`);
    }
    const visible = (document.querySelector("main") as HTMLElement | null)?.innerText ?? "";
    for (const code of codes) if (visible.includes(code)) out.push(`raw code on screen: ${code}`);
    if (/\b\d{12}\b/.test(visible) && !/\(\d{12}\)/.test(visible)) out.push("a bare 12-digit account ID outside an account label");
    return out;
  }, RAW_CODES);
  out.push(...banned("page", await page.locator("main").innerText()));
  return out;
}

type Gap = { account: string; region: string; from: string; to: string; reason: string; missing_permission?: string | null };
type Account = { account: string; name?: string; complete: boolean; reason: string | null; as_of?: string | null; disconnected_at?: string | null };
type Changes = {
  complete: boolean;
  window: { since: string; until: string; settled_until: string };
  summary: {
    total: number;
    total_is_lower_bound: boolean;
    people: number;
    errors: number;
    routine?: number;
    truncated?: boolean;
    by_actor_class: Record<string, number>;
    top_actors?: Array<{ actor: string; actor_class: string; count: number }>;
  };
  accounts: Account[];
  regions?: Array<{ region: string; state: string; complete: boolean }>;
  gaps: Gap[];
  alarms?: Array<{ id: string; name: string; account: string; region: string; start: string | null; start_known: boolean }>;
  sync: { first_read_pending: boolean; dropped?: number };
  buckets?: Array<{ state: string }>;
  suspect: { status: string; change_id?: string | null };
  items?: Array<{ id: string; time: string; account: string; region: string }>;
  next_cursor: string | null;
};

async function api(page: Page, query = "window=24h&limit=50"): Promise<{ status: number; json: Changes }> {
  const r = await page.request.get(`/api/admin/aws/changes?${query}`);
  return { status: r.status(), json: (await r.json().catch(() => ({}))) as Changes };
}

/** Real missing data: a gap that is not the unsettled tail and not a Region left off, or an account short of complete. */
function realGaps(wire: Changes): Gap[] {
  return (wire.gaps ?? []).filter((g) => g.reason !== "arriving" && g.reason !== "region_not_enabled");
}
function wantsWarning(wire: Changes): boolean {
  if (wire.sync?.first_read_pending) return true;
  return (
    realGaps(wire).length > 0 ||
    (wire.accounts ?? []).some((a) => !a.complete || Boolean(a.as_of) || a.reason === "account_disconnected") ||
    Boolean(wire.summary?.truncated) ||
    (wire.sync?.dropped ?? 0) > 0
  );
}

/** The page's nothingRead rule on the wire: the first read is pending (QA-512), or every bucket is a gap. */
function nothingReadWire(wire: Changes): boolean {
  if (wire.sync?.first_read_pending) return true;
  const b = wire.buckets ?? [];
  return b.length > 0 && b.every((x) => x.state === "gap");
}

// AWS_CHANGES_EXPECT_WARNING: the exact warning sentence the served scenario
// must produce (e.g. QA-506 on changes-cloudtrail-denied); unset, not checked.
const EXPECT_WARNING = process.env.AWS_CHANGES_EXPECT_WARNING ?? "";

const NUMBER_ORDER = ["changes", "people", "pipeline", "aws_service", "unknown", "errors", "accounts"];

/**
 * The summary card against the illustration and the API read at the same
 * time: one row of plain numbers in order with Accounts n/m, the likely-cause
 * row, top actors on their own line, and a warning line only on real gaps.
 */
function cardCheck(view: View, wire: Changes): string[] {
  const out: string[] = [];
  const num = (s: string) => Number(s.replace(/,/g, ""));
  for (const [k, n] of Object.entries(view.removed)) if (n > 0) out.push(`removed element still on the page: ${k} (${n})`);
  // One row of numbers, in order.
  const keys = view.numbers.map((n) => n.key);
  const order = keys.map((k) => NUMBER_ORDER.indexOf(k));
  if (order.some((v, i) => v < 0 || (i > 0 && v <= order[i - 1]))) out.push(`numbers out of order: ${keys.join(", ")}`);
  const tops = new Set(view.numbers.map((n) => n.top));
  if (tops.size > 1) out.push(`numbers wrap onto ${tops.size} rows: ${JSON.stringify(view.numbers.map((n) => `${n.label}@${n.top}`))}`);
  // Nothing read in the whole window (every bucket a gap): a zero would claim
  // a count, so every number reads "—" and the warning line says why.
  const blank = nothingReadWire(wire);
  if (blank) {
    for (const n of view.numbers) if (n.value !== "—") out.push(`nothing read, but ${n.label} reads "${n.value}", want "—"`);
    if (view.numbers.length === 0) out.push("nothing read, but the card shows no numbers");
    if (!view.warning) out.push("nothing read, but no warning line says why");
    if (view.noChanges) out.push(`nothing read, but the card says "${view.noChanges}"`);
    for (const r of view.rowTotals) if (r.total !== "—") out.push(`nothing read, but grid row "${r.row}" totals "${r.total}", want "—"`);
    // QA-515: an account that cannot be read is not "Reading": the line names the disconnect.
    // The fix covers every cannot-read reason, not only the disconnect.
    const cut = (wire.accounts ?? []).filter((a) => ["account_disconnected", "credentials_invalid", "assume_role_failed", "account_mismatch"].includes(a.reason ?? ""));
    if (cut.length > 0 && /^Reading /.test(view.warning)) out.push(`${cut.length} account(s) cannot be read (${cut.map((a) => a.reason).join(", ")}), but the card says "${view.warning}"`);
    if (cut.length > 0 && !view.reconnect) out.push(`account cannot be read (${cut.map((a) => a.reason).join(", ")}), but the card has no Reconnect link`);
    if (view.topActors.length > 0) out.push(`nothing read, but top actors show: ${view.topActors.join(" | ")}`);
    return out;
  }
  for (const n of view.numbers) {
    if (n.key === "accounts") {
      if (!/^\d+\/\d+$/.test(n.value)) out.push(`Accounts value "${n.value}" is not n/m`);
    } else if (!/^[\d,]+$/.test(n.value)) out.push(`${n.label} value "${n.value}" is not a plain number`);
  }
  if ((wire.accounts?.length ?? 0) > 0) {
    const want = `${wire.accounts.filter((a) => a.complete).length}/${wire.accounts.length}`;
    const got = view.numbers.find((n) => n.key === "accounts")?.value;
    if (got !== want) out.push(`Accounts ${got ?? "missing"}, API says ${want}`);
  }
  if (wire.sync?.first_read_pending) return out;
  // Numbers agree with the API.
  const pairs: Array<[string, number | undefined]> = [
    ["changes", wire.summary.total],
    // QA-505 (Option A): "By people" counts changes by people, as the grid's Person row does.
    ["people", wire.summary.by_actor_class.person ?? 0],
    ["pipeline", wire.summary.by_actor_class.pipeline],
    ["aws_service", wire.summary.by_actor_class.aws_service],
    ["errors", wire.summary.errors],
  ];
  for (const [k, v] of pairs) {
    const shown = view.numbers.find((n) => n.key === k);
    if (shown && v !== undefined && num(shown.value) !== v) out.push(`${shown.label} ${shown.value} != API ${k} ${v}`);
  }
  // The grid's actor rows add up to Changes, and Errors to Failed calls.
  const actorRows = view.rowTotals.filter((r) => !/^Errors/i.test(r.row));
  const gridSum = actorRows.reduce((a, r) => a + num(r.total), 0);
  if (actorRows.length > 0 && gridSum !== wire.summary.total) out.push(`grid actor rows sum ${gridSum} != API total ${wire.summary.total}`);
  // "By people" sits among change counts; the grid's Person row counts changes by people.
  const byPeople = view.numbers.find((n) => n.key === "people");
  const personRow = view.rowTotals.find((r) => /^Person/i.test(r.row));
  if (byPeople && personRow && num(byPeople.value) !== num(personRow.total)) out.push(`card "By people ${byPeople.value}" and grid "Person ${personRow.total}" disagree`);
  // Warning line: only on real gaps, and then always; one sentence, no "·".
  const want = wantsWarning(wire);
  if (want && !view.warning) out.push("data is missing (API) but the card shows no warning line");
  if (!want && view.warning) out.push(`warning line with no real gap: "${view.warning}" (only the unsettled tail is open)`);
  if (view.warning) {
    if (view.warningLine.includes("·")) out.push(`warning line has "·": ${view.warningLine}`);
    if (!/^Data not complete(?: (?:after|before|from) .+?)?: .+\.$/.test(view.warning) && !/^Reading /.test(view.warning)) out.push(`warning text shape: "${view.warning}"`);
    if (!view.warningIsLast) out.push("warning line is not at the foot of the card");
    if (view.warningRect && view.warningRect.height > 44) out.push(`warning line wraps (${view.warningRect.height}px high)`);
  }
  // "No changes" only without a warning line.
  if (view.noChanges && view.warning) out.push(`"${view.noChanges}" shown with the warning line "${view.warning}"`);
  if (view.noChanges && wire.summary.total > 0) out.push(`"${view.noChanges}" with an API total of ${wire.summary.total}`);
  if (!view.noChanges && !view.warning && wire.summary.total === 0) out.push("total 0 with no warning, but no \"No changes\" line");
  if (view.noChanges && view.numbers.some((n) => n.key === "changes")) out.push("No changes and a Changes number together");
  // Likely cause: present whenever an alarm is in scope; Show › when found; the amber tag only on the list row.
  const alarms = wire.alarms?.length ?? 0;
  if (alarms > 0 && !view.likely) out.push("an alarm is in scope but the likely-cause row is missing");
  if (view.likely) {
    if (!view.likely.text.startsWith("Likely cause")) out.push(`likely-cause row "${view.likely.text}"`);
    if (view.likely.tagInCard > 0) out.push("the card carries the amber Likely cause tag (agreed: list row only)");
    if (wire.suspect.status === "found" && view.likely.kind === "found" && !view.likely.show) out.push("likely cause found but no Show ›");
  }
  // Top actors on their own line, under the likely-cause row; label + number pairs, no mark.
  if ((wire.summary.top_actors?.length ?? 0) > 0) {
    if (!view.topActorsText.startsWith("Top actors")) out.push(`top actors line "${view.topActorsText}"`);
    if (view.topActorsInsideLikely) out.push("top actors inside the likely-cause row");
    if (view.likely?.rect && view.topActorsRect && view.topActorsRect.top < view.likely.rect.bottom) out.push("top actors not on their own line under the likely-cause row");
    for (const a of view.topActors) if (!/^.+ [\d,]+$/.test(a)) out.push(`top actor "${a}" is not name + number`);
    const labels = view.topActors.map((a) => a.replace(/\s+[\d,]+$/, ""));
    if (new Set(labels).size !== labels.length) out.push(`top actors repeat a label: ${view.topActors.join(" | ")}`);
  }
  return out;
}

/** Activity: name + number row labels, one alarms chip, hatched gaps with a reason, no notes under the grid. */
function activityCheck(view: View, wire: Changes): string[] {
  const out: string[] = [];
  // QA-512: while nothing is read the row totals read "—".
  const blank = nothingReadWire(wire);
  for (const l of view.rowLabels) if (!(blank ? /^(Automation|Person|AWS service|Unknown|Errors) (—|[\d,]+)$/ : /^(Automation|Person|AWS service|Unknown|Errors) [\d,]+$/).test(l)) out.push(`row label "${l}" is not name + number`);
  const alarms = wire.alarms ?? [];
  if (alarms.length > 0) {
    if (view.alarmChips.length !== 1) out.push(`${view.alarmChips.length} alarm chips, want one`);
    else if (!/^[\d,]+ alarms? ▾$/.test(view.alarmChips[0])) out.push(`alarm chip "${view.alarmChips[0]}"`);
  } else if (view.alarmChips.length > 0) out.push("alarm chip with no alarm in scope");
  if (view.headerText.replace(view.alarmChips[0] ?? "", "").trim() !== "Activity") out.push(`Activity header carries more than the chip: "${view.headerText}"`);
  if (view.gridNotes && !/^Busiest hour /.test(view.gridNotes)) out.push(`notes under the grid: "${view.gridNotes.slice(0, 120)}"`);
  for (const t of view.gapCells) if (!t || t === "Unknown") out.push(`hatched cell with no reason on hover (title "${t}")`);
  for (const b of view.bands) if (!b.title) out.push(`band ${b.reason} with no reason on hover`);
  for (const c of view.partialCells) if (!/^[\d,]+$/.test(c)) out.push(`partly read cell shows "${c}"`);
  return out;
}

/** The viewer's clock for an instant: "HH:mm" on ref's local day, "MMM d HH:mm" otherwise. */
async function localClock(page: Page, iso: string, ref: string, withDay: boolean): Promise<string> {
  return page.evaluate(
    ([i, r, d]) => {
      const a = new Date(i as string);
      const b = new Date(r as string);
      const p = (n: number) => String(n).padStart(2, "0");
      const hm = `${p(a.getHours())}:${p(a.getMinutes())}`;
      const same = a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
      return same || !d ? hm : `${a.toLocaleString("en-US", { month: "short" })} ${a.getDate()} ${hm}`;
    },
    [iso, ref, withDay] as const,
  );
}

/** A popover row's span by the QA-501 rule: "S–now" to the window end, else "S–E". */
async function stretchSpan(page: Page, from: string, to: string, until: string): Promise<string> {
  const start = await localClock(page, from, until, true);
  if (Date.parse(to) >= Date.parse(until)) return `${start}–now`;
  return `${start}–${await localClock(page, to, from, true)}`;
}

/**
 * Open the warning line's popover and hold its rows against the API: one row
 * per real gap (account, Region, span, reason), the Regions not enabled, the
 * dropped rows. Escape closes it and returns focus to the line.
 */
async function warningPopover(page: Page, wire: Changes, problems: string[], capture?: (state: string) => Promise<string>) {
  const trigger = page.locator('[data-warning-line] button[aria-haspopup="dialog"]').first();
  if (!(await trigger.count())) return null;
  await trigger.click();
  const panel = page.locator("[data-warning-popover]");
  await expect(panel).toBeVisible({ timeout: 5_000 });
  const rows = await panel.locator("[data-stretch]").evaluateAll((els) =>
    els.map((e) => ({ reason: e.getAttribute("data-stretch"), text: (e as HTMLElement).innerText.replace(/\s+/g, " ").trim() })),
  );
  const notEnabled = await panel.locator("[data-not-enabled-region]").evaluateAll((els) => els.map((e) => e.getAttribute("data-not-enabled-region")));
  const dropped = await panel.locator("[data-dropped]").allInnerTexts();
  const text = await panel.innerText();
  const box = await panel.boundingBox();
  const vp = page.viewportSize();
  problems.push(...banned("warning popover", text));
  if (text.includes("·")) problems.push(`warning popover has "·"`);
  if (box && vp && (box.x < 0 || box.x + box.width > vp.width + 1)) problems.push(`warning popover off screen ${JSON.stringify(box)}`);
  const nameOf = (account: string) => wire.accounts.find((a) => a.account === account)?.name ?? account;
  const disconnected = new Set(wire.accounts.filter((a) => a.reason === "account_disconnected").map((a) => a.account));
  const expected: string[] = [];
  for (const g of realGaps(wire)) {
    const span = await stretchSpan(page, g.from, g.to, wire.window.until);
    const head = `${nameOf(g.account)}${g.region ? ` ${g.region}` : ""} ${span}`;
    expected.push(head);
    const reason = disconnected.has(g.account) ? "account_disconnected" : g.reason;
    const row = rows.find((r) => r.text.startsWith(head) && r.reason === reason);
    if (!row) problems.push(`popover has no row "${head}" (${reason}); rows: ${rows.map((r) => r.text).join(" | ")}`);
    if (g.missing_permission && row && !row.text.includes(`missing ${g.missing_permission}`)) problems.push(`popover row "${row.text}" does not name ${g.missing_permission}`);
  }
  if (rows.length < realGaps(wire).length) problems.push(`popover lists ${rows.length} gaps, API has ${realGaps(wire).length}`);
  const wantNotEnabled = [
    ...new Set([
      ...(wire.regions ?? []).filter((r) => r.state === "not_enabled").map((r) => r.region),
      ...(wire.gaps ?? []).filter((g) => g.reason === "region_not_enabled").map((g) => g.region),
    ]),
  ];
  for (const r of wantNotEnabled) if (!notEnabled.includes(r)) problems.push(`popover does not list not-enabled ${r}`);
  if ((wire.sync?.dropped ?? 0) > 0 && dropped.length === 0) problems.push("popover does not list dropped rows");
  const file = capture ? await capture("warning-popover") : null;
  await page.keyboard.press("Escape");
  await page.waitForTimeout(150);
  const closed = (await panel.count()) === 0;
  const focusBack = await trigger.evaluate((el) => el === document.activeElement);
  if (!closed) problems.push("Escape does not close the warning popover");
  if (!focusBack) problems.push("Escape does not return focus to the warning line");
  return { rows, notEnabled, dropped, expected, text: norm(text).slice(0, 800), file };
}

/** The alarms chip opens a list of each alarm with account, Region and start. */
async function alarmsPopover(page: Page, wire: Changes, problems: string[], capture?: (state: string) => Promise<string>) {
  const chip = page.locator("[data-alarms-chip]").first();
  if (!(await chip.count())) return null;
  await chip.scrollIntoViewIfNeeded();
  await chip.click();
  const panel = page.locator("[data-alarms-popover]");
  await expect(panel).toBeVisible({ timeout: 5_000 });
  const entries = await panel.locator("[data-alarm-entry]").allInnerTexts();
  const history = await panel.locator("[data-alarm-history]").allInnerTexts();
  const text = await panel.innerText();
  problems.push(...banned("alarms popover", text));
  const chipN = Number(((await chip.innerText()).match(/[\d,]+/) ?? ["0"])[0].replace(/,/g, ""));
  if (entries.length !== chipN) problems.push(`alarms chip says ${chipN}, popover lists ${entries.length}`);
  for (const e of entries) if (!/(from \S+|start unknown)/.test(e)) problems.push(`alarm entry without a start: "${norm(e)}"`);
  const file = capture ? await capture("alarms-popover") : null;
  await page.keyboard.press("Escape");
  await page.waitForTimeout(150);
  if (await panel.count()) problems.push("Escape does not close the alarms popover");
  return { chipN, apiAlarms: wire.alarms?.length ?? 0, entries: entries.map(norm), history, file };
}

// ---------------------------------------------------------------------------
// Every state at each width and theme: the card, the activity grid, the
// warning popover, the alarms popover, Regions options, Show ›.

for (const viewport of VIEWPORTS) {
  for (const theme of THEMES) {
    test(`state capture (${viewport.name}, ${theme})`, async ({ browser }) => {
      test.setTimeout(90_000);
      const watch: Watch = { console: [], http: [] };
      const page = await open(browser, viewport, theme, "/aws/changes", watch);
      // The answer the page rendered from (the collector writes every 30 s).
      let seen: Changes | null = null;
      page.on("response", async (r) => {
        const u = new URL(r.url());
        if (u.pathname === "/api/admin/aws/changes" && !u.searchParams.get("cursor") && r.status() === 200) seen = ((await r.json().catch(() => null)) as Changes | null) ?? seen;
      });
      await settled(page);
      await page.waitForTimeout(500);
      const view = await readPage(page);
      const wire = seen ?? (await api(page)).json;
      const problems = [...cardCheck(view, wire), ...activityCheck(view, wire), ...(await audit(page))];
      if (EXPECT_WARNING && view.warning !== EXPECT_WARNING) problems.push(`warning line "${view.warning}", want "${EXPECT_WARNING}"`);
      const file = await shot(page, viewport, theme, STATE);
      const capture = (s: string) => shotView(page, viewport, theme, `${STATE}-${s}`);
      // Hover a hatched or partly read cell, a band and an alarm line: the
      // hover text is the title of whatever sits under the pointer, as a user gets it.
      const hover: Record<string, string | null> = {};
      const underPointer = async (sel: string) => {
        const target = page.locator(sel).first();
        if (!(await target.count())) return null;
        await target.scrollIntoViewIfNeeded();
        const b = await target.boundingBox();
        if (!b) return null;
        await page.mouse.move(b.x + b.width / 2, b.y + b.height / 2);
        await page.waitForTimeout(100);
        return page.evaluate(([x, y]) => {
          const at = document.elementFromPoint(x, y) as HTMLElement | null;
          return at?.closest<HTMLElement>("[title]")?.getAttribute("title") ?? "";
        }, [b.x + b.width / 2, b.y + b.height / 2] as const);
      };
      hover.gapCell = await underPointer('[role="cell"][data-state="gap"]');
      hover.partialCell = await underPointer('[role="cell"][data-state="partial"]');
      hover.band = await underPointer("[data-band]");
      hover.alarmLine = await underPointer("[data-alarm-line]");
      for (const [k, v] of Object.entries(hover)) if (v === "") problems.push(`no hover text over the ${k}`);
      if (hover.gapCell && !/: /.test(hover.gapCell)) problems.push(`hatched cell hover "${hover.gapCell}" names no reason`);
      // QA-512: during the first read a hatched cell names what is not checked yet.
      if (wire.sync?.first_read_pending && (wire.gaps ?? []).some((g) => g.reason === "not_checked") && hover.gapCell && !/not checked yet/.test(hover.gapCell)) problems.push(`first read: hatched cell hover "${hover.gapCell}" does not say "not checked yet"`);
      await page.mouse.move(2, 2);
      const warning = await warningPopover(page, wire, problems, capture);
      const alarms = await alarmsPopover(page, wire, problems, capture);
      // Regions: the status line is gone; each option names its state.
      let regionOptions: string[] = [];
      const regions = page.getByRole("button", { name: "Regions", exact: true });
      if (await regions.count()) {
        await regions.click();
        regionOptions = (await page.getByRole("listbox", { name: "Regions" }).getByRole("option").allInnerTexts()).map(norm);
        problems.push(...banned("Regions list", regionOptions.join("\n")));
        for (const o of regionOptions.slice(1)) if (!/ — (complete|partly unknown|not enabled)$/.test(o)) problems.push(`Regions option "${o}" names no state`);
        // The Region's state lives only here now (§9.1 #1): it must be readable, not cut by an ellipsis.
        const clipped = await page.getByRole("listbox", { name: "Regions" }).getByRole("option").evaluateAll((els) =>
          els
            .filter((e) => Array.from(e.querySelectorAll<HTMLElement>("*")).concat(e as HTMLElement).some((c) => c.scrollWidth - c.clientWidth > 1 && !c.getAttribute("title")))
            .map((e) => (e as HTMLElement).innerText.trim()),
        );
        for (const c of clipped) problems.push(`Regions option clipped on screen: "${c}"`);
        for (const r of wire.regions ?? []) {
          const o = regionOptions.find((x) => x.startsWith(r.region === "global" ? "Global" : r.region));
          const want = r.state === "not_enabled" ? "not enabled" : r.complete ? "complete" : "partly unknown";
          if (!o) problems.push(`Regions list has no ${r.region}`);
          else if (!o.endsWith(`— ${want}`)) problems.push(`Regions option "${o}", API says ${want}`);
        }
        await capture("regions-open");
        await page.keyboard.press("Escape");
      }
      let accountOptions: string[] = [];
      const accounts = page.getByRole("button", { name: "Accounts", exact: true });
      if (await accounts.count()) {
        await accounts.click();
        accountOptions = (await page.getByRole("listbox", { name: "Accounts" }).getByRole("option").allInnerTexts()).map(norm);
        problems.push(...banned("Accounts list", accountOptions.join("\n")));
        await capture("accounts-open");
        await page.keyboard.press("Escape");
      }
      // Show ›: the list scrolls to the likely-cause row and the grid cell is outlined.
      let showResult: Record<string, unknown> | null = null;
      const show = page.locator("[data-likely-cause] button", { hasText: "Show ›" });
      if (await show.count()) {
        await show.click();
        await page.waitForTimeout(900);
        showResult = await page.evaluate(() => {
          const row = document.querySelector<HTMLElement>("[data-list-scroll] [data-item][data-shown]");
          const scroller = document.querySelector<HTMLElement>("[data-list-scroll]");
          const r = row?.getBoundingClientRect();
          const s = scroller?.getBoundingClientRect();
          return {
            row: row?.getAttribute("data-item") ?? null,
            tag: row?.querySelector("[data-likely-tag]")?.textContent?.trim() ?? null,
            rowInView: Boolean(r && s && r.top >= s.top - 1 && r.bottom <= s.bottom + 1),
            cellShown: document.querySelectorAll('[role="cell"][data-shown]').length,
          };
        });
        if (!showResult.row) problems.push("Show › marks no list row");
        else if (!showResult.rowInView) problems.push("Show › did not scroll the row into the list's view");
        if (showResult.tag !== "Likely cause") problems.push(`the shown row's tag is "${showResult.tag}", want "Likely cause"`);
        if (showResult.cellShown !== 1) problems.push(`Show › outlines ${showResult.cellShown} grid cells, want 1`);
        await capture("show-likely-cause");
      }
      record(`state ${viewport.name} ${theme}`, { file, view, api: { complete: wire.complete, summary: wire.summary, accounts: wire.accounts, gaps: wire.gaps, regions: wire.regions, suspect: wire.suspect, sync: wire.sync }, hover, warning, alarms, regionOptions, accountOptions, showResult, problems, console: watch.console, http: watch.http });
      expect.soft(watch.console, "no console errors").toEqual([]);
      expect.soft(watch.http.filter((h) => !h.includes("/enterprise/api/sso/deployment")), "no unexplained 4xx/5xx").toEqual([]);
      expect(problems).toEqual([]);
    });
  }
}

// ---------------------------------------------------------------------------
// The warning line over a gap (A11 C9, C12): at 1h and 24h, the numbers stay
// plain, the warning line is there whenever data is missing, and every span it
// or its popover names follows QA-501's one-reference-day rule: the start
// reads HH:mm on window.until's local day and "MMM d HH:mm" otherwise; the end
// carries its date when its local day differs from the start's.
// AWS_CHANGES_VIEW_TZ sets the browser zone.

const VIEW_TZ = process.env.AWS_CHANGES_VIEW_TZ || undefined;
for (const viewport of VIEWPORTS) {
  for (const theme of THEMES) {
    test(`warning line spans and words at 1h and 24h (${viewport.name}, ${theme}${VIEW_TZ ? `, ${VIEW_TZ}` : ""})`, async ({ browser }) => {
      test.setTimeout(90_000);
      const watch: Watch = { console: [], http: [] };
      const problems: string[] = [];
      const page = await open(browser, viewport, theme, "/aws/changes", watch, VIEW_TZ ? { timezoneId: VIEW_TZ } : {});
      const seen = new Map<string, Changes>();
      page.on("response", async (r) => {
        const u = new URL(r.url());
        if (u.pathname === "/api/admin/aws/changes" && !u.searchParams.get("cursor") && r.status() === 200) {
          const j = (await r.json().catch(() => null)) as Changes | null;
          if (j) seen.set(u.searchParams.get("window") ?? "24h", j);
        }
      });
      await settled(page);
      const readings: Record<string, unknown> = {};
      for (const w of ["1h", "24h"] as const) {
        await page.getByRole("button", { name: w, exact: true }).first().click();
        await page.waitForTimeout(1200);
        await settled(page);
        await page.waitForTimeout(300);
        const wire = seen.get(w) ?? (await api(page, `window=${w}&limit=50`)).json;
        const view = await readPage(page);
        const local: string[] = [...cardCheck(view, wire)];
        if (EXPECT_WARNING && view.warning !== EXPECT_WARNING) local.push(`${w}: warning line "${view.warning}", want "${EXPECT_WARNING}"`);
        // The warning line's "after S" / "before E" / "from S to E" names a real gap span by the rule.
        const m = view.warning.match(/^Data not complete (after|before|from) (.+?): /);
        if (m) {
          const spans: string[] = [];
          for (const g of realGaps(wire)) {
            const s = await localClock(page, g.from, wire.window.until, true);
            const e = await localClock(page, g.to, wire.window.until, true);
            const e2 = await localClock(page, g.to, g.from, true);
            spans.push(`after ${s}`, `before ${e}`, `from ${s} to ${e2}`);
          }
          for (const a of wire.accounts) if (a.as_of) spans.push(`after ${await localClock(page, a.as_of, wire.window.until, true)}`);
          if (!spans.includes(`${m[1]} ${m[2]}`)) local.push(`${w}: warning "${m[1]} ${m[2]}" matches no gap by the rule ${JSON.stringify([...new Set(spans)].slice(0, 12))}`);
        }
        const zoneTag = VIEW_TZ ? `-${VIEW_TZ.replace(/[^A-Za-z0-9]+/g, "-").toLowerCase()}` : "";
        const pop = await warningPopover(page, wire, local, (s) => shotView(page, viewport, theme, `${STATE}-${w}-${s}${zoneTag}`));
        readings[w] = { warning: view.warning, numbers: view.numbers.map((n) => `${n.label} ${n.value}`), noChanges: view.noChanges, complete: wire.complete, lower: wire.summary.total_is_lower_bound, gaps: realGaps(wire).map((g) => `${g.account}/${g.region} ${g.from}..${g.to} ${g.reason}`), popover: pop };
        problems.push(...local.map((p) => `${w}: ${p}`));
        await shot(page, viewport, theme, `${STATE}-window-${w}${zoneTag}`);
      }
      record(`warning line ${viewport.name} ${theme}`, { zone: VIEW_TZ ?? Intl.DateTimeFormat().resolvedOptions().timeZone, readings, problems, console: watch.console, http: watch.http });
      expect(problems).toEqual([]);
    });
  }
}

// ---------------------------------------------------------------------------
// QA-503: every Dropdown on the page by keyboard only. Open with Enter, focus
// moves into the list, ArrowDown/ArrowUp move the highlighted option, Enter
// chooses and returns focus to the button, Escape closes without choosing and
// returns focus to the button (after a keyboard or a mouse open).

async function focusInfo(page: Page) {
  return page.evaluate(() => {
    const a = document.activeElement as HTMLElement | null;
    return { tag: a?.tagName.toLowerCase() ?? "none", role: a?.getAttribute("role") ?? "", label: a?.getAttribute("aria-label") ?? "", active: a?.getAttribute("aria-activedescendant") ?? "" };
  });
}

async function keyboardDropdown(page: Page, name: string, capture?: (state: string) => Promise<string>) {
  const steps: Array<Record<string, unknown>> = [];
  const problems: string[] = [];
  const button = page.getByRole("button", { name, exact: true }).first();
  if (!(await button.count())) return { name, skipped: true, steps, problems };
  const slug = name.toLowerCase().replace(/[^a-z]+/g, "-");
  await button.scrollIntoViewIfNeeded();
  await button.focus();
  const before = { url: page.url(), label: norm(await button.innerText()) };
  await page.keyboard.press("Enter");
  await page.waitForTimeout(250);
  let f = await focusInfo(page);
  const expanded = await button.getAttribute("aria-expanded");
  steps.push({ key: "Enter (open)", expanded, focus: f });
  if (expanded !== "true") problems.push(`${name}: Enter does not open the list`);
  if (f.role !== "listbox") problems.push(`${name}: focus is not in the list after opening (${f.tag}${f.role ? `[${f.role}]` : ""})`);
  const options = (await page.getByRole("listbox", { name }).getByRole("option").allInnerTexts()).map(norm);
  const a0 = f.active;
  await page.keyboard.press("ArrowDown");
  await page.waitForTimeout(120);
  const a1 = (await focusInfo(page)).active;
  await page.keyboard.press("ArrowUp");
  await page.waitForTimeout(120);
  const a2 = (await focusInfo(page)).active;
  steps.push({ key: "ArrowDown, ArrowUp", a0, a1, a2 });
  if (options.length > 1 && a1 === a0) problems.push(`${name}: ArrowDown does not move the highlight`);
  if (options.length > 1 && a2 !== a0) problems.push(`${name}: ArrowUp does not move back`);
  // Choose the option after the current one (or before it, at the end).
  const current = options.findIndex((o) => o === before.label);
  await page.keyboard.press(current >= 0 && current === options.length - 1 ? "ArrowUp" : "ArrowDown");
  await page.waitForTimeout(120);
  const chosenId = (await focusInfo(page)).active;
  const chosen = chosenId ? norm(await page.locator(`[id="${chosenId}"]`).innerText().catch(() => "")) : "";
  if (capture) await capture(`${slug}-keyboard-open`);
  await page.keyboard.press("Enter");
  await page.waitForTimeout(700);
  f = await focusInfo(page);
  const after = { url: page.url(), label: norm(await button.innerText().catch(() => "")), expanded: await button.getAttribute("aria-expanded").catch(() => null) };
  steps.push({ key: "Arrow, Enter (choose)", chosen, after, focus: f });
  if (after.expanded === "true") problems.push(`${name}: Enter does not close the list`);
  if (after.label !== chosen) problems.push(`${name}: Enter chose "${after.label}", highlighted "${chosen}"`);
  if (after.label === before.label && after.url === before.url) problems.push(`${name}: Enter chose nothing`);
  if (f.label !== name) problems.push(`${name}: focus not back on the button after choosing (${f.tag}${f.role ? `[${f.role}]` : ""} ${f.label})`);
  // A scope picker must survive its own choice, or the user cannot widen the scope again.
  if (!(await button.count())) {
    const scopeText = norm(await page.locator("main").innerText()).match(/(Accounts?:? .{0,40}?Regions?:? \S+(?: \S+)?)/)?.[1] ?? "";
    problems.push(`${name}: the picker is gone after choosing "${chosen}" (scope bar reads "${scopeText}"); no way back to all on the page`);
    if (capture) await capture(`${slug}-keyboard-chosen-picker-gone`);
    steps.push({ key: "picker gone", scopeText, url: page.url() });
    await page.goto(before.url);
    await settled(page);
  } else if (capture) await capture(`${slug}-keyboard-chosen`);
  // Escape after a keyboard open.
  const urlPreEsc = page.url();
  await button.focus();
  await page.keyboard.press("ArrowDown");
  await page.waitForTimeout(200);
  const opened = await button.getAttribute("aria-expanded");
  await page.keyboard.press("Escape");
  await page.waitForTimeout(200);
  f = await focusInfo(page);
  const urlEsc = page.url();
  steps.push({ key: "ArrowDown (open), Escape", opened, expanded: await button.getAttribute("aria-expanded"), focus: f });
  if (opened !== "true") problems.push(`${name}: ArrowDown on the button does not open the list`);
  if ((await button.getAttribute("aria-expanded")) === "true") problems.push(`${name}: Escape does not close the list opened from the keyboard`);
  if (f.label !== name) problems.push(`${name}: Escape does not return focus to the button (${f.tag} ${f.label})`);
  if (urlEsc !== urlPreEsc) problems.push(`${name}: Escape changed the choice`);
  // Escape after a mouse open.
  await button.click();
  await page.waitForTimeout(200);
  const mouseFocus = await focusInfo(page);
  await page.keyboard.press("Escape");
  await page.waitForTimeout(200);
  f = await focusInfo(page);
  steps.push({ key: "click (open), Escape", focusOnOpen: mouseFocus, expanded: await button.getAttribute("aria-expanded"), focus: f });
  if (mouseFocus.role !== "listbox") problems.push(`${name}: focus not in the list after a mouse open`);
  if ((await button.getAttribute("aria-expanded")) === "true") problems.push(`${name}: Escape does not close the list opened with a click`);
  if (f.label !== name) problems.push(`${name}: Escape after a click does not return focus to the button`);
  // Home + Enter restores the first option (All / default), by keyboard too.
  await button.focus();
  await page.keyboard.press("Enter");
  await page.waitForTimeout(200);
  await page.keyboard.press(name === "Compare from" ? "End" : "Home");
  await page.keyboard.press("Enter");
  await page.waitForTimeout(700);
  steps.push({ key: "Enter, Home/End, Enter (restore)", label: norm(await button.innerText().catch(() => "")), url: page.url() });
  if (!(await button.count()) || page.url() !== before.url) {
    await page.goto(before.url);
    await settled(page);
  }
  return { name, options, before, after, steps, problems };
}

test("QA-503: Accounts, Regions and Compare from/to by keyboard", async ({ browser }) => {
  test.setTimeout(120_000);
  const viewport = DESKTOP;
  const watch: Watch = { console: [], http: [] };
  const page = await open(browser, viewport, "dark", "/aws/changes", watch);
  await settled(page);
  const results = [];
  for (const name of ["Accounts", "Regions", "Compare from", "Compare to"]) {
    results.push(await keyboardDropdown(page, name, (s) => shotView(page, viewport, "dark", `${STATE}-${s}`)));
    await settled(page);
  }
  const problems = results.flatMap((r) => r.problems);
  for (const r of results) if (r.skipped) problems.push(`${r.name}: not on the page`);
  record("QA-503 keyboard", { results, problems, console: watch.console, http: watch.http });
  expect(problems).toEqual([]);
});

// ---------------------------------------------------------------------------
// QA-504: an account, then a Region. Both pickers stay after each choice and
// keep listing every configured account and Region; "All N" and "N regions"
// each clear only their own filter; focus returns to the picker's button.
// By mouse and by keyboard, light and dark. A frame sampler records whether
// the pickers or the card ever leave the screen, and how often each picker's
// label changes, while the filtered answer and the extra unfiltered picker
// read land (flicker).

const ident = (o: string) => o.split(" — ")[0].trim();

async function pickerOptions(page: Page, name: string): Promise<string[]> {
  const button = page.getByRole("button", { name, exact: true });
  await button.click();
  const opts = (await page.getByRole("listbox", { name }).getByRole("option").allInnerTexts()).map(norm);
  await page.keyboard.press("Escape");
  await page.waitForTimeout(150);
  return opts;
}

async function choose(page: Page, name: string, mode: "mouse" | "keyboard", index: number) {
  const button = page.getByRole("button", { name, exact: true });
  if (mode === "mouse") {
    await button.click();
    await page.getByRole("listbox", { name }).getByRole("option").nth(index).click();
  } else {
    await button.focus();
    await page.keyboard.press("Enter");
    await page.waitForTimeout(150);
    await page.keyboard.press("Home");
    for (let i = 0; i < index; i++) await page.keyboard.press("ArrowDown");
    await page.keyboard.press("Enter");
  }
  await page.waitForTimeout(1500);
  await page.waitForLoadState("networkidle").catch(() => {});
}

for (const theme of THEMES) {
  for (const mode of ["mouse", "keyboard"] as const) {
    test(`QA-504: account then Region, pickers stay, All clears (${mode}, ${theme})`, async ({ browser }) => {
      test.setTimeout(120_000);
      const viewport = DESKTOP;
      const watch: Watch = { console: [], http: [] };
      const page = await open(browser, viewport, theme, "/aws/changes", watch);
      const reads: string[] = [];
      page.on("request", (r) => {
        const u = new URL(r.url());
        if (u.pathname === "/api/admin/aws/changes" && !u.searchParams.get("cursor")) reads.push(`${new Date().toISOString().slice(11, 23)} ${u.search}`);
      });
      await settled(page);
      const problems: string[] = [];
      const steps: Array<Record<string, unknown>> = [];
      const accAll = await pickerOptions(page, "Accounts");
      const regAll = await pickerOptions(page, "Regions");
      const accLabel0 = norm(await page.getByRole("button", { name: "Accounts", exact: true }).innerText());
      const regLabel0 = norm(await page.getByRole("button", { name: "Regions", exact: true }).innerText());
      if (!/^All \d+$/.test(accLabel0)) problems.push(`Accounts starts at "${accLabel0}", want "All N"`);
      if (!/^\d+ regions?$/.test(regLabel0)) problems.push(`Regions starts at "${regLabel0}", want "N regions"`);
      // Frame sampler: one entry each time what the user sees in the scope bar changes.
      await page.evaluate(() => {
        const w = window as unknown as { __frames: Array<Record<string, unknown>>; __stop: boolean };
        w.__frames = [];
        w.__stop = false;
        let last = "";
        const t0 = performance.now();
        const tick = () => {
          const btn = (n: string) => document.querySelector<HTMLElement>(`button[aria-haspopup="listbox"][aria-label="${n}"]`);
          const a = btn("Accounts");
          const r = btn("Regions");
          const f = {
            acc: a ? (a.innerText ?? "").trim() : null,
            reg: r ? (r.innerText ?? "").trim() : null,
            card: Boolean(document.querySelector("[data-summary-card]")),
            items: document.querySelectorAll("[data-item]").length > 0,
            numbers: Array.from(document.querySelectorAll("[data-summary-card] [data-number] dd")).map((e) => (e as HTMLElement).innerText.trim()).join(" "),
          };
          const key = JSON.stringify(f);
          if (key !== last) {
            w.__frames.push({ ms: Math.round(performance.now() - t0), ...f });
            last = key;
          }
          if (!w.__stop) requestAnimationFrame(tick);
        };
        requestAnimationFrame(tick);
      });
      const accIndex = accAll.findIndex((o) => o.startsWith("Production"));
      const regIndex = regAll.findIndex((o) => o.startsWith("eu-west-1"));
      if (accIndex < 1 || regIndex < 1) problems.push(`no Production / eu-west-1 option: ${accAll.join(" | ")} ; ${regAll.join(" | ")}`);
      const check = async (label: string, want: { account: boolean; region: boolean }, focusOn: string) => {
        const url = new URL(page.url());
        const f = await focusInfo(page);
        const acc = page.getByRole("button", { name: "Accounts", exact: true });
        const reg = page.getByRole("button", { name: "Regions", exact: true });
        const present = { accounts: await acc.count(), regions: await reg.count() };
        const s: Record<string, unknown> = { step: label, url: url.search, focus: f, present };
        if (present.accounts !== 1 || present.regions !== 1) problems.push(`${label}: pickers on screen ${JSON.stringify(present)}, want both`);
        if (Boolean(url.searchParams.get("account")) !== want.account) problems.push(`${label}: account filter in URL is ${url.searchParams.get("account")}, want ${want.account ? "set" : "none"}`);
        if (Boolean(url.searchParams.get("region")) !== want.region) problems.push(`${label}: region filter in URL is ${url.searchParams.get("region")}, want ${want.region ? "set" : "none"}`);
        if (f.label !== focusOn) problems.push(`${label}: focus on ${f.tag}${f.role ? `[${f.role}]` : ""} "${f.label}", want the ${focusOn} button`);
        if (present.accounts && present.regions) {
          s.accLabel = norm(await acc.innerText());
          s.regLabel = norm(await reg.innerText());
          // The selected label in the button reads in full too.
          s.regLabelClipped = await reg.evaluate((b) => Array.from(b.querySelectorAll<HTMLElement>("span")).some((x) => x.scrollWidth - x.clientWidth > 1));
          if (s.regLabelClipped) problems.push(`${label}: Regions button label "${s.regLabel}" is clipped`);
          const accNow = await pickerOptions(page, "Accounts");
          const regNow = await pickerOptions(page, "Regions");
          s.accOptions = accNow;
          s.regOptions = regNow;
          if (JSON.stringify(accNow.map(ident)) !== JSON.stringify(accAll.map(ident))) problems.push(`${label}: Accounts lists ${accNow.map(ident).join(", ")}, want ${accAll.map(ident).join(", ")}`);
          if (JSON.stringify(regNow.map(ident)) !== JSON.stringify(regAll.map(ident))) problems.push(`${label}: Regions lists ${regNow.map(ident).join(", ")}, want ${regAll.map(ident).join(", ")}`);
        }
        steps.push(s);
      };
      await choose(page, "Accounts", mode, accIndex);
      await check("account chosen", { account: true, region: false }, "Accounts");
      await choose(page, "Regions", mode, regIndex);
      await check("then Region chosen", { account: true, region: true }, "Regions");
      // The filtered page with both pickers, and the Regions list open over it.
      const file = await shot(page, viewport, theme, `${STATE}-qa504-${mode}-both-chosen`);
      await page.getByRole("button", { name: "Regions", exact: true }).click();
      const listFile = await shotView(page, viewport, theme, `${STATE}-qa504-${mode}-both-chosen-regions-open`);
      await page.keyboard.press("Escape");
      await page.waitForTimeout(150);
      await choose(page, "Accounts", mode, 0);
      await check(`"${accLabel0}" chosen`, { account: false, region: true }, "Accounts");
      const accAfter = norm(await page.getByRole("button", { name: "Accounts", exact: true }).innerText());
      if (accAfter !== accLabel0) problems.push(`Accounts reads "${accAfter}" after All, want "${accLabel0}"`);
      await choose(page, "Regions", mode, 0);
      await check(`"${regLabel0}" chosen`, { account: false, region: false }, "Regions");
      const regAfter = norm(await page.getByRole("button", { name: "Regions", exact: true }).innerText());
      if (regAfter !== regLabel0) problems.push(`Regions reads "${regAfter}" after clearing, want "${regLabel0}"`);
      const frames = (await page.evaluate(() => {
        const w = window as unknown as { __frames: Array<Record<string, unknown>>; __stop: boolean };
        w.__stop = true;
        return w.__frames;
      })) as Array<{ ms: number; acc: string | null; reg: string | null; card: boolean; items: boolean; numbers: string }>;
      // Flicker: a picker or the card leaving the screen, or a label bouncing (A → B → A).
      const gone = frames.filter((f) => f.acc === null || f.reg === null || !f.card);
      for (const f of gone.slice(0, 3)) problems.push(`flicker at ${f.ms} ms: Accounts ${f.acc === null ? "gone" : "on"}, Regions ${f.reg === null ? "gone" : "on"}, card ${f.card ? "on" : "gone"}`);
      const seq = (k: "acc" | "reg") => frames.map((f) => f[k]).filter((v, i, a) => i === 0 || v !== a[i - 1]);
      const bounces = (xs: Array<string | null>) => xs.filter((v, i) => i >= 2 && v === xs[i - 2]).length;
      const flicker = { accLabels: seq("acc"), regLabels: seq("reg"), accBounces: bounces(seq("acc")), regBounces: bounces(seq("reg")), framesRecorded: frames.length, numberChanges: frames.map((f) => f.numbers).filter((v, i, a) => i === 0 || v !== a[i - 1]) };
      // Each picker's label goes start → chosen → start once; anything more is a visible flicker.
      if (flicker.accBounces > 1) problems.push(`Accounts label bounces: ${flicker.accLabels.join(" → ")}`);
      if (flicker.regBounces > 1) problems.push(`Regions label bounces: ${flicker.regLabels.join(" → ")}`);
      record(`QA-504 ${mode} ${theme}`, { file, listFile, accAll, regAll, steps, flicker, frames: frames.slice(0, 80), reads, problems, console: watch.console, http: watch.http });
      expect.soft(watch.console, "no console errors").toEqual([]);
      expect(problems).toEqual([]);
    });
  }
}

// ---------------------------------------------------------------------------
// A user's path: from the sidebar, keyboard through the controls, the 7d lock,
// search, Regions, every expandable row, and Load more.

test("user path: nav, keyboard, 7d lock, search, regions, expand, load more", async ({ browser }) => {
  test.setTimeout(180_000);
  const viewport = DESKTOP;
  const watch: Watch = { console: [], http: [] };
  const page = await open(browser, viewport, "dark", "/now", watch);
  const nav = page.locator('nav[aria-label="Primary"]');
  const link = nav.getByRole("link", { name: "Changes", exact: true });
  if (!(await link.isVisible())) await nav.getByRole("button", { name: /^AWS/ }).first().click();
  await link.click();
  await expect(page).toHaveURL(/\/aws\/changes/);
  await settled(page);
  const notes: Record<string, unknown> = {};

  // Keyboard: the window switch, the search box and the compare pickers are reachable by Tab.
  const reached: string[] = [];
  await page.getByRole("searchbox", { name: "Search changes" }).focus();
  for (let i = 0; i < 90; i++) {
    await page.keyboard.press("Tab");
    const name = await page.evaluate(() => {
      const el = document.activeElement as HTMLElement | null;
      if (!el || !el.closest("main")) return "";
      return (el.getAttribute("aria-label") || el.innerText || (el as HTMLInputElement).placeholder || el.tagName).trim().slice(0, 40);
    });
    if (name) reached.push(name);
  }
  notes.tabOrder = reached;
  for (const want of ["1h", "24h", "7d, Enterprise", "Compare from", "Compare to"]) {
    expect.soft(reached.some((n) => n === want || n.startsWith(want)), `Tab reaches "${want}"`).toBe(true);
  }

  // 7d: a quiet Enterprise note, not an error; the API answers 403 window_locked.
  await page.getByRole("button", { name: "7d, Enterprise" }).click({ force: true });
  await expect(page.locator("[data-lock-note]").first()).toBeVisible();
  await shot(page, viewport, "dark", `${STATE}-7d-locked`);
  const locked = await page.request.get("/api/admin/aws/changes?window=7d");
  notes.lockedStatus = locked.status();
  expect.soft(locked.status()).toBe(403);
  expect.soft(await page.locator("main").innerText()).not.toContain("Couldn't load changes");
  await page.goto("/aws/changes?window=7d");
  await expect(page.locator("[data-lock-note]").first()).toBeVisible();
  notes.deepLink7d = (await page.locator("[data-lock-note]").first().innerText()).trim();
  await page.goto("/aws/changes");
  await settled(page);

  // Window 1h.
  await page.getByRole("button", { name: "1h", exact: true }).click();
  await settled(page);
  notes.oneHour = await readPage(page);
  await shot(page, viewport, "dark", `${STATE}-1h`);
  await page.getByRole("button", { name: "24h", exact: true }).click();
  await settled(page);

  // Expand every burst and group; walk its "Show more in this group".
  const expanders = page.getByRole("button", { name: /Expand ›/ });
  const groups = await expanders.count();
  notes.groups = groups;
  for (let i = 0; i < Math.min(groups, 2); i++) {
    await expanders.nth(0).click();
    const members = page.getByRole("list", { name: "Group members" }).first();
    await expect(members).toBeVisible();
    let more = 0;
    while ((await page.getByRole("button", { name: "Show more in this group" }).count()) > 0 && more < 5) {
      await page.getByRole("button", { name: "Show more in this group" }).first().click();
      await page.waitForTimeout(400);
      more++;
    }
    const ids = await members.locator("li").allInnerTexts();
    notes[`group${i}`] = { memberCount: ids.length, showMoreClicks: more, duplicates: ids.length - new Set(ids).size };
    await page.getByRole("button", { name: "Collapse" }).first().click();
  }
  const details = page.getByRole("button", { name: /Details ›/ });
  if (await details.count()) {
    await details.first().click();
    await expect(page.locator("[data-item-detail]").first()).toBeVisible();
    notes.detail = (await page.locator("[data-item-detail]").first().innerText()).slice(0, 400);
    await page.getByRole("button", { name: "Collapse" }).first().click();
  }

  // Load more inside the list's own scroll, without a repeated row.
  let pages = 0;
  const loadMore = page.locator("[data-list-scroll]").getByRole("button", { name: "Load more", exact: true });
  while ((await loadMore.count()) > 0 && pages < 4) {
    await loadMore.scrollIntoViewIfNeeded();
    await loadMore.click();
    await page.waitForTimeout(800);
    pages++;
  }
  const rowIds = await page.locator("[data-item]").evaluateAll((els) => els.map((e) => e.getAttribute("data-item")));
  notes.paging = { pages, rows: rowIds.length, duplicates: rowIds.length - new Set(rowIds).size };
  expect.soft(rowIds.length - new Set(rowIds).size, "no row repeats across pages").toBe(0);

  // Search by q: the raw API name of the first row, then a miss.
  await page.getByRole("button", { name: /Details ›/ }).first().click().catch(() => {});
  const firstEvent = await page.locator("[data-event-name]").first().innerText().catch(() => "");
  const term = (firstEvent.trim() || "PutParameter").split(/\s/)[0];
  await page.getByLabel("Search changes").fill(term);
  const hitAnswer = page.waitForResponse((r) => r.url().includes("/api/admin/aws/changes?") && r.url().includes("q="));
  await page.getByLabel("Search changes").press("Enter");
  notes.searchHitApi = await (await hitAnswer).json().then((j: Changes) => ({ total: j.summary.total, complete: j.complete })).catch(() => null);
  await page.waitForTimeout(500);
  await settled(page);
  notes.searchHit = { term, view: await readPage(page) };
  await shot(page, viewport, "dark", `${STATE}-search-hit`);
  await page.getByLabel("Search changes").fill("zz-no-such-change");
  await page.getByLabel("Search changes").press("Enter");
  await page.waitForTimeout(800);
  await settled(page);
  const miss = await readPage(page);
  notes.searchMiss = miss;
  await shot(page, viewport, "dark", `${STATE}-search-miss`);
  const missWire = await api(page, "window=24h&q=zz-no-such-change");
  const problems = cardCheck(miss, missWire.json);
  record("user path", { notes, problems, console: watch.console, http: watch.http });
  expect.soft(problems).toEqual([]);
  // The SSO deployment probe on every OSS load is the shell's (QA-447), not this page's.
  expect(watch.http.filter((h) => !h.includes("window=7d") && !h.includes("/enterprise/api/sso/deployment")), "no unexplained 4xx/5xx").toEqual([]);
});

// ---------------------------------------------------------------------------
// Changes and Compare: the same height (480–720 px), each scrolls inside, and
// the page does not grow as Load more brings the list to 200 rows. Run on a
// busy scenario (changes-busy) for the 200 rows.

test("changes and compare: equal height, inner scroll, load more to 200", async ({ browser }) => {
  test.setTimeout(150_000);
  const viewport = DESKTOP;
  const results: Record<string, unknown> = {};
  const problems: string[] = [];
  for (const theme of THEMES) {
    const watch: Watch = { console: [], http: [] };
    const page = await open(browser, viewport, theme, "/aws/changes", watch);
    await settled(page);
    const measure = () =>
      page.evaluate(() => {
        const box = (sel: string) => {
          const e = document.querySelector<HTMLElement>(sel);
          if (!e) return null;
          const b = e.getBoundingClientRect();
          return { top: Math.round(b.top), height: Math.round(b.height), scrollH: e.scrollHeight, clientH: e.clientHeight, overflowY: getComputedStyle(e).overflowY };
        };
        const main = document.querySelector<HTMLElement>("main");
        return {
          changes: box('[data-panel="changes"]'),
          compare: box('[data-panel="compare"]'),
          changesCard: box('[data-panel="changes"] > section'),
          compareCard: box('[data-panel="compare"] > section'),
          listScroll: box("[data-list-scroll]"),
          compareScroll: box("[data-compare-scroll]"),
          mainScrollH: main?.scrollHeight ?? 0,
          rows: document.querySelectorAll("[data-list-scroll] [data-item]").length,
        };
      });
    const before = await measure();
    const loadMore = page.locator("[data-list-scroll]").getByRole("button", { name: "Load more", exact: true });
    let clicks = 0;
    const seenIds: string[] = [];
    while ((await loadMore.count()) > 0 && (await page.locator("[data-list-scroll] [data-item]").count()) < 200 && clicks < 8) {
      await loadMore.scrollIntoViewIfNeeded();
      await loadMore.click();
      await expect(loadMore.or(page.locator("[data-list-scroll] [data-item]").nth(Math.min(199, (clicks + 2) * 50 - 1)))).toBeVisible({ timeout: 15_000 }).catch(() => {});
      await page.waitForTimeout(700);
      clicks++;
    }
    seenIds.push(...(await page.locator("[data-list-scroll] [data-item]").evaluateAll((els) => els.map((e) => e.getAttribute("data-item") ?? ""))));
    const after = await measure();
    const dupes = seenIds.length - new Set(seenIds).size;
    const local: string[] = [];
    if (!before.changes || !before.compare) local.push("panels missing");
    else {
      if (Math.abs(before.changes.height - before.compare.height) > 1) local.push(`Changes ${before.changes.height}px and Compare ${before.compare.height}px differ`);
      if (before.changes.height < 480 || before.changes.height > 720) local.push(`panel height ${before.changes.height}px outside 480–720`);
      if (before.changes.top !== before.compare.top) local.push("panels not side by side");
    }
    if (before.changesCard && before.changes && Math.abs(before.changesCard.height - before.changes.height) > 1) local.push(`Changes card ${before.changesCard.height}px does not fill its ${before.changes.height}px slot`);
    if (before.compareCard && before.compare && Math.abs(before.compareCard.height - before.compare.height) > 1) local.push(`Compare card ${before.compareCard.height}px does not fill its ${before.compare.height}px slot`);
    if (before.listScroll?.overflowY !== "auto" || before.compareScroll?.overflowY !== "auto") local.push("a card does not scroll inside");
    if (after.rows < 200) local.push(`only ${after.rows} rows after ${clicks} Load more clicks`);
    if (dupes) local.push(`${dupes} duplicate rows after Load more`);
    if (after.mainScrollH !== before.mainScrollH) local.push(`page grew from ${before.mainScrollH}px to ${after.mainScrollH}px with ${after.rows} rows`);
    if (after.changes && before.changes && after.changes.height !== before.changes.height) local.push(`Changes card height moved ${before.changes.height} → ${after.changes.height}`);
    if (after.listScroll && after.listScroll.scrollH <= after.listScroll.clientH) local.push("list does not overflow its card with 200 rows");
    const file = await shot(page, viewport, theme, `${STATE}-200-rows`);
    // The end of the list, scrolled inside the card.
    await page.locator("[data-list-scroll]").evaluate((e) => (e.scrollTop = e.scrollHeight));
    await page.locator('[data-panel="changes"]').scrollIntoViewIfNeeded();
    const fileEnd = await shotView(page, viewport, theme, `${STATE}-200-rows-list-end`);
    results[theme] = { before, after, clicks, rows: after.rows, dupes, file, fileEnd, http: watch.http };
    problems.push(...local.map((p) => `${theme}: ${p}`));
    expect.soft(watch.http.filter((h) => !h.includes("/enterprise/api/sso/deployment")), "no unexplained 4xx/5xx").toEqual([]);
  }
  record("panels", { results, problems });
  expect(problems).toEqual([]);
});

// ---------------------------------------------------------------------------
// Compare: every "Compare from" choice, the *to* picker (a time moves the diff
// and the URL), "N groups not compared ▾", "Use HH:MM" and "and N more".

test("compare panel: from, to, groups not compared", async ({ browser }) => {
  test.setTimeout(150_000);
  const viewport = DESKTOP;
  const results: Record<string, unknown> = {};
  const problems: string[] = [];
  for (const theme of THEMES) {
    const watch: Watch = { console: [], http: [] };
    const page = await open(browser, viewport, theme, "/aws/changes", watch);
    const compares: Array<{ url: string; status: number; body: Record<string, unknown> }> = [];
    page.on("response", async (r) => {
      if (r.url().includes("/api/admin/aws/changes/compare")) compares.push({ url: r.url(), status: r.status(), body: await r.json().catch(() => ({})) });
    });
    await settled(page);
    const panel = page.locator('section[aria-labelledby="changes-compare-title"]');
    await expect(panel.locator("[data-compare]").first()).toBeVisible({ timeout: 15_000 });
    const notes: Record<string, unknown> = { default: norm(await panel.innerText()).slice(0, 500) };
    const capture = (s: string) => shotView(page, viewport, theme, `${STATE}-compare-${s}`);
    await panel.scrollIntoViewIfNeeded();
    await capture("default");
    const fromPicker = page.getByRole("button", { name: "Compare from", exact: true });
    await fromPicker.click();
    const fromOptions = (await page.getByRole("listbox", { name: "Compare from" }).getByRole("option").allInnerTexts()).map(norm);
    notes.fromOptions = fromOptions;
    await page.keyboard.press("Escape");
    for (const [index, label] of fromOptions.entries()) {
      await fromPicker.click();
      await page.getByRole("listbox", { name: "Compare from" }).getByRole("option").nth(index).click();
      await page.waitForTimeout(900);
      notes[`from ${label}`] = norm(await panel.innerText()).slice(0, 400);
    }
    // From the earliest full compare when one is offered ("Use HH:MM"), else
    // the oldest preset, so the *to* picker has hours between from and now.
    const useFirst = panel.getByRole("button", { name: /^Use / });
    if (await useFirst.count()) {
      notes.useLabel = norm(await useFirst.first().innerText());
      await useFirst.first().click();
      await page.waitForTimeout(900);
      notes.afterUse = norm(await panel.innerText()).slice(0, 400);
    } else {
      await fromPicker.click();
      await page.getByRole("listbox", { name: "Compare from" }).getByRole("option").last().click();
      await page.waitForTimeout(900);
    }
    notes.fromForTo = new URL(page.url()).searchParams.get("from");
    const toPicker = page.getByRole("button", { name: "Compare to", exact: true });
    const local: string[] = [];
    if (!(await toPicker.count())) local.push("no Compare to picker");
    else {
      if (norm(await toPicker.innerText()) !== "Now") local.push(`Compare to starts at "${await toPicker.innerText()}", want Now`);
      await toPicker.click();
      const toOptions = (await page.getByRole("listbox", { name: "Compare to" }).getByRole("option").allInnerTexts()).map(norm);
      notes.toOptions = toOptions;
      if (toOptions[0] !== "Now") local.push(`first to option "${toOptions[0]}"`);
      await capture("to-open");
      if (toOptions.length < 2) {
        notes.toBlocked = "no whole hour between from and now: the to picker offers only Now (time-bound; rerun once snapshot history spans an hour)";
        await page.keyboard.press("Escape");
      } else {
        const pickIndex = Math.min(toOptions.length - 1, Math.max(1, Math.floor(toOptions.length / 2)));
        const pick = toOptions[pickIndex];
        const beforeText = norm(await panel.innerText());
        const n = compares.length;
        await page.getByRole("listbox", { name: "Compare to" }).getByRole("option").nth(pickIndex).click();
        await expect.poll(() => compares.length, { timeout: 10_000 }).toBeGreaterThan(n).catch(() => {});
        await page.waitForTimeout(800);
        const url = new URL(page.url());
        const req = compares.slice(n).pop();
        const reqTo = req ? new URL(req.url).searchParams.get("to") : null;
        const afterText = norm(await panel.innerText());
        notes.to = { pick, url: page.url(), urlTo: url.searchParams.get("to"), reqTo, reqStatus: req?.status, body: req ? { status: req.body.status, from: req.body.from, to: req.body.to, added: (req.body.added as unknown[] | undefined)?.length, removed: (req.body.removed as unknown[] | undefined)?.length, changed: (req.body.changed as unknown[] | undefined)?.length, between: req.body.changes_between_total } : null, afterText: afterText.slice(0, 500) };
        if (!url.searchParams.get("to")) local.push(`picking to "${pick}" left no to= in the URL`);
        if (!reqTo) local.push(`picking to "${pick}" sent no compare request with to=`);
        if (reqTo && url.searchParams.get("to") && Date.parse(reqTo) !== Date.parse(url.searchParams.get("to") as string)) local.push(`URL to ${url.searchParams.get("to")} != request to ${reqTo}`);
        if (req && req.status !== 200) local.push(`compare with to answered ${req.status}`);
        if (req?.body.to && reqTo && Date.parse(String(req.body.to)) !== Date.parse(reqTo)) local.push(`server compared to ${req.body.to}, asked ${reqTo}`);
        if (norm(await toPicker.innerText()) !== pick) local.push(`to picker reads "${await toPicker.innerText()}" after choosing "${pick}"`);
        if (afterText === beforeText) local.push("the diff did not change after picking a to time");
        if (/and now\b/.test(afterText) && afterText.includes("Changes between")) local.push(`"Changes between … and now" after picking to ${pick}`);
        const between = afterText.match(/Changes between (\S+(?: \S+ \S+)?) and (\S+(?: \S+ \S+)?) \(/);
        if (between && between[2] !== pick) local.push(`"Changes between" ends at "${between[2]}", picked "${pick}"`);
        await capture("to-picked");
        // A reload keeps the picked to time.
        await page.reload();
        await settled(page);
        await page.waitForTimeout(800);
        if (norm(await toPicker.innerText()) !== pick) local.push(`after reload the to picker reads "${await toPicker.innerText()}"`);
        // Back to Now clears it.
        await toPicker.click();
        await page.getByRole("listbox", { name: "Compare to" }).getByRole("option").first().click();
        await page.waitForTimeout(800);
        if (new URL(page.url()).searchParams.get("to")) local.push("choosing Now kept to= in the URL");
      }
    }
    // "N groups not compared ▾".
    for (const idx of [0, ...fromOptions.keys()]) {
      if (await page.locator("[data-not-compared]").count()) break;
      await fromPicker.click();
      await page.getByRole("listbox", { name: "Compare from" }).getByRole("option").nth(idx).click();
      await page.waitForTimeout(900);
    }
    const nc = page.locator("[data-not-compared]").first();
    if (await nc.count()) {
      const label = norm(await nc.innerText());
      if (!/^[\d,]+ groups? not compared ▾$/.test(label)) local.push(`not-compared trigger "${label}"`);
      await nc.click();
      const pop = page.locator("[data-compare-partial]");
      await expect(pop).toBeVisible({ timeout: 5_000 }).catch(() => local.push("groups not compared popover did not open"));
      const rows = (await pop.locator("[data-gap-text]").allInnerTexts()).map(norm);
      const n = Number((label.match(/[\d,]+/) ?? ["0"])[0].replace(/,/g, ""));
      if (rows.length !== n) local.push(`"${label}" lists ${rows.length} rows`);
      local.push(...banned("groups not compared popover", await pop.innerText().catch(() => "")));
      notes.notCompared = { label, rows };
      await capture("not-compared-open");
      await page.keyboard.press("Escape");
      await page.waitForTimeout(150);
      if (await pop.count()) local.push("Escape does not close the groups not compared popover");
    } else notes.notCompared = "no group left out in any from choice";
    local.push(...banned("Compare card", await panel.innerText()));
    // QA-514: every partial entry has a reason; a service not offered in a
    // Region or off the support plan is excluded with that reason.
    const partials = new Map<string, { account: string; region: string; service: string; reason: string; excluded: boolean }>();
    for (const c of compares) {
      for (const e of ((c.body.partial as Array<Record<string, unknown>> | undefined) ?? [])) {
        const row = { account: String(e.account ?? ""), region: String(e.region ?? ""), service: String(e.service ?? ""), reason: String(e.reason ?? ""), excluded: Boolean(e.excluded) };
        partials.set(`${row.region}/${row.service}/${row.reason}/${row.excluded}`, row);
        if (!e.reason) local.push(`compare partial ${row.region} ${row.service} has an empty reason (${new URL(c.url).search})`);
        if ((row.reason === "not_available_in_region" || row.reason === "not_on_plan") && !row.excluded) local.push(`compare partial ${row.region} ${row.service} ${row.reason} is not excluded`);
        if (row.service === "appregistry" && row.region === "ap-southeast-5" && row.reason !== "not_available_in_region") local.push(`compare: AppRegistry ap-southeast-5 reason "${row.reason}", want not_available_in_region`);
        if (row.service === "aws-health" && row.reason !== "not_on_plan") local.push(`compare: AWS Health reason "${row.reason}", want not_on_plan`);
      }
    }
    notes.partials = [...partials.values()].map((r) => ({ ...r, account: r.account ? "<account>" : "" }));
    const more = page.locator("[data-changes-between-more]");
    if (await more.count()) {
      const href = (await more.first().getAttribute("href")) ?? "";
      const want = new URL(href, page.url());
      const since = want.searchParams.get("since");
      const until = want.searchParams.get("until");
      const chipBefore = await page.locator("[data-span]").innerText().catch(() => "");
      // QA-513: hold the span's own live answer 3 s (delayed, not replaced) so
      // the frame between the click and the answer can be read.
      let held = 0;
      const match = (u: URL) => u.pathname === "/api/admin/aws/changes" && u.searchParams.get("since") === since && !u.searchParams.get("cursor");
      const hold = async (route: Route) => {
        held++;
        await new Promise((r) => setTimeout(r, 3_000));
        await route.continue().catch(() => {});
      };
      await page.route(match, hold);
      await more.first().click();
      await expect(page.locator("[data-span]")).toBeVisible({ timeout: 5_000 }).catch(() => {});
      await page.waitForTimeout(300);
      const firstFrame = { at: new Date().toISOString(), span: norm(await page.locator("[data-span]").innerText().catch(() => "")), view: await readPage(page) };
      const firstFile = await capture("and-more-first-frame");
      await page.unroute(match, hold);
      await settled(page);
      await expect.poll(async () => (await readPage(page)).numbers.find((n) => n.key === "changes")?.value ?? "", { timeout: 15_000 }).not.toBe("—").catch(() => {});
      const landed = await readPage(page);
      const landedSpan = norm(await page.locator("[data-span]").innerText().catch(() => ""));
      const wantStart = since ? await localClock(page, since, until ?? since, false) : "";
      const wantEnd = until ? await localClock(page, until, until, false) : "";
      if (held === 0) local.push("and N more: the span's answer was never requested with its since (could not hold it)");
      if (wantStart && !firstFrame.span.includes(wantStart)) local.push(`and N more first frame: chip "${firstFrame.span}" does not start at ${wantStart}`);
      if (wantEnd && !firstFrame.span.includes(wantEnd)) local.push(`and N more first frame: chip "${firstFrame.span}" does not end at ${wantEnd}`);
      if (firstFrame.span === norm(chipBefore) && chipBefore) local.push(`and N more first frame: chip still reads the previous span "${chipBefore}"`);
      for (const n of firstFrame.view.numbers) if (n.value !== "—") local.push(`and N more first frame: ${n.label} reads "${n.value}" before the span's answer, want "—"`);
      for (const r of firstFrame.view.rowTotals) if (r.total !== "—") local.push(`and N more first frame: grid "${r.row}" totals "${r.total}", want "—"`);
      if (firstFrame.view.topActors.length) local.push("and N more first frame: top actors of the previous span still shown");
      if (landed.numbers.find((n) => n.key === "changes")?.value === "—") local.push("and N more: numbers still read — 15 s after the answer was released");
      if (landedSpan !== firstFrame.span) local.push(`and N more: chip moved from "${firstFrame.span}" to "${landedSpan}" when the answer landed`);
      const landedFile = await capture("and-more-landed");
      notes.andMore = { href, url: page.url(), chipBefore, held, firstFrame: { span: firstFrame.span, numbers: firstFrame.view.numbers.map((n) => `${n.label} ${n.value}`), rowTotals: firstFrame.view.rowTotals, file: firstFile }, landed: { span: landedSpan, numbers: landed.numbers.map((n) => `${n.label} ${n.value}`), file: landedFile }, span: landedSpan };
    }
    results[theme] = { notes, compares: compares.map((c) => ({ url: new URL(c.url).search, status: c.status })), http: watch.http };
    problems.push(...local.map((p) => `${theme}: ${p}`));
    expect.soft(watch.http.filter((h) => !h.includes("/enterprise/api/sso/deployment"))).toEqual([]);
  }
  record("compare panel", { results, problems });
  expect(problems).toEqual([]);
});

// ---------------------------------------------------------------------------
// Compare on a phone (A11 C9, QA-501/QA-502): the "Changes between …" time
// column fits a date and no row runs into its summary. Mobile only.

const COMPARE_TZ = process.env.AWS_CHANGES_COMPARE_TZ || undefined;
for (const theme of THEMES) {
  test(`compare panel on a phone (${theme}${COMPARE_TZ ? `, ${COMPARE_TZ}` : ""})`, async ({ browser }) => {
    test.skip(!MOBILE_ON, "mobile width not in AWS_CHANGES_VIEWPORTS");
    test.setTimeout(150_000);
    const viewport = MOBILE;
    const tag = COMPARE_TZ ? `-${COMPARE_TZ.replace(/[^A-Za-z0-9]+/g, "-").toLowerCase()}` : "";
    const watch: Watch = { console: [], http: [] };
    const problems: string[] = [];
    const page = await open(browser, viewport, theme, "/aws/changes", watch, { timezoneId: COMPARE_TZ, hasTouch: true });
    await settled(page);
    const panel = page.locator("[data-compare]").first();
    await expect(panel).toBeVisible();
    const measure = async (choice: string) => {
      await panel.scrollIntoViewIfNeeded();
      const m = await page.evaluate(() => {
        const doc = document.documentElement;
        const rows = Array.from(document.querySelectorAll<HTMLElement>("[data-changes-between] li")).map((li) => {
          const [time, body] = Array.from(li.children) as HTMLElement[];
          const t = time.getBoundingClientRect();
          const b = body?.getBoundingClientRect();
          return { time: time.innerText.trim(), clipped: time.scrollWidth > time.clientWidth + 1, overlaps: Boolean(b && t.right > b.left + 0.5), right: Math.round(t.right), summaryLeft: b ? Math.round(b.left) : null };
        });
        return { pageOverflow: Math.max(doc.scrollWidth, document.body.scrollWidth) - window.innerWidth, rows, viewport: window.innerWidth };
      });
      if (m.pageOverflow > 0) problems.push(`${choice}: page scrolls sideways by ${m.pageOverflow}px`);
      const lefts = [...new Set(m.rows.map((r) => r.summaryLeft).filter((v): v is number => v !== null))];
      if (lefts.length > 1) problems.push(`${choice}: summaries start at ${lefts.join(", ")} px`);
      for (const r of m.rows) {
        if (r.clipped) problems.push(`${choice}: time "${r.time}" clipped`);
        if (r.overlaps) problems.push(`${choice}: time "${r.time}" runs into its summary`);
        if (r.right > m.viewport) problems.push(`${choice}: time "${r.time}" past the screen edge`);
      }
      return m;
    };
    const notes: Record<string, unknown> = { default: await measure("default") };
    const files = [await shot(page, viewport, theme, `${STATE}-compare-phone${tag}`)];
    const picker = page.getByRole("button", { name: "Compare from" });
    await picker.click();
    const options = await page.getByRole("option").allInnerTexts();
    await page.keyboard.press("Escape");
    for (const [index, label] of options.entries()) {
      if ((await picker.getAttribute("aria-expanded")) !== "true") await picker.click();
      await page.getByRole("listbox", { name: "Compare from" }).getByRole("option").nth(index).click();
      await page.waitForTimeout(800);
      notes[`from ${label}`] = await measure(`from ${label}`);
    }
    record(`compare phone ${theme}`, { zone: COMPARE_TZ ?? Intl.DateTimeFormat().resolvedOptions().timeZone, options, notes, files, problems, console: watch.console, http: watch.http });
    expect(problems).toEqual([]);
  });
}

// ---------------------------------------------------------------------------
// Watch over time: the page left open, sampled every 10 s, with a screenshot
// every 30 s. The warning line (presence and words) and the Accounts figure
// must hold steady while the served scenario does not change; a 5xx, or
// "No changes" over a gap, fails the run.

test("watch over time", async ({ browser }) => {
  const minutes = Number(process.env.AWS_CHANGES_WATCH_MIN || "3");
  test.setTimeout((minutes * 60 + 120) * 1000);
  const viewport = DESKTOP;
  const theme: Theme = (process.env.AWS_CHANGES_WATCH_THEME as Theme) || "dark";
  const watch: Watch = { console: [], http: [] };
  const page = await open(browser, viewport, theme, "/aws/changes", watch);
  await settled(page);
  const samples: Array<Record<string, unknown>> = [];
  const problems: string[] = [];
  const end = Date.now() + minutes * 60_000;
  let tick = 0;
  let lastShot = 0;
  while (Date.now() < end) {
    const wire = await api(page);
    if (wire.status !== 200) problems.push(`${new Date().toISOString()} API status ${wire.status}`);
    // AWS_CHANGES_WATCH_RELOAD=0: never reload after the first load, so the
    // page must follow the API on its own.
    const reloaded = process.env.AWS_CHANGES_WATCH_RELOAD === "0" ? false : tick % 3 === 0;
    if (reloaded) {
      await page.reload();
      await settled(page);
    }
    const view = await readPage(page);
    const at = new Date().toISOString();
    if (reloaded) for (const p of cardCheck(view, (await api(page)).json)) problems.push(`${at} ${p}`);
    for (const p of banned("page", view.cardText)) problems.push(`${at} ${p}`);
    samples.push({
      at,
      tick,
      reloaded,
      warning: view.warning,
      reconnect: view.reconnect,
      noChanges: view.noChanges,
      numbers: view.numbers.map((n) => `${n.key}=${n.value}`).join(" "),
      api: {
        complete: wire.json.complete,
        total: wire.json.summary?.total,
        accounts: wire.json.accounts?.map((a) => `${a.account}:${a.complete}:${a.reason}:${a.as_of ?? "-"}`),
        gaps: realGaps(wire.json).map((g) => `${g.account}/${g.region}:${g.reason}:${g.from}`),
        suspect: wire.json.suspect?.status,
      },
    });
    if (Date.now() - lastShot >= 30_000) {
      await shot(page, viewport, theme, `${STATE}-watch-${String(tick).padStart(3, "0")}`);
      lastShot = Date.now();
    }
    tick++;
    await page.waitForTimeout(10_000);
  }
  // Flap checks: the warning text, its presence, and the API's account/gap state never alternate.
  const flaps = (key: (s: Record<string, unknown>) => string, what: string) => {
    const keys = samples.map(key);
    const transitions = keys.filter((k, i) => i > 0 && k !== keys[i - 1]).length;
    const distinct = new Set(keys).size;
    if (distinct > 1 && transitions > distinct - 1) problems.push(`${what} flapped: ${distinct} distinct values, ${transitions} transitions`);
    return { distinct, transitions, values: [...new Set(keys)] };
  };
  const warningFlap = flaps((s) => String(s.warning), "warning line");
  const presenceFlap = flaps((s) => String(Boolean(s.warning)), "warning presence");
  const accountsFlap = flaps((s) => String(s.numbers).replace(/.*(accounts=\S+).*/, "$1"), "Accounts figure");
  const apiFlap = flaps((s) => JSON.stringify((s.api as { accounts: unknown }).accounts) + JSON.stringify((s.api as { gaps: unknown }).gaps), "API account/gap state");
  if (process.env.AWS_CHANGES_WATCH_WANT_WARNING === "1") {
    const missing = samples.filter((s) => !s.warning);
    if (missing.length) problems.push(`warning line missing in ${missing.length} of ${samples.length} samples (first ${missing[0].at})`);
  }
  record("watch over time", { minutes, samples, warningFlap, presenceFlap, accountsFlap, apiFlap, problems, console: watch.console, http: watch.http });
  expect.soft(watch.http.filter((h) => !h.includes("/enterprise/api/sso/deployment"))).toEqual([]);
  expect(problems).toEqual([]);
});

// ---------------------------------------------------------------------------
// The agent reads the same numbers: Ask AI from the page, a question that names
// get_aws_changes (the harness mock model calls the tool it is asked for and
// states its summary total), compared with the API and the page at that time.
// Needs the stack booted with AWS_CHANGES_AGENT=mock.

test("ask AI agrees with the page", async ({ browser }) => {
  test.skip(process.env.AWS_CHANGES_AGENT !== "mock", "needs AWS_CHANGES_AGENT=mock");
  test.setTimeout(120_000);
  const viewport = DESKTOP;
  const watch: Watch = { console: [], http: [] };
  const page = await open(browser, viewport, "dark", "/aws/changes", watch);
  await settled(page);
  const view = await readPage(page);
  const wire = await api(page);
  await page.getByRole("link", { name: "Ask AI" }).click();
  const composer = page.getByLabel("Message");
  await expect(composer).toBeVisible();
  await composer.fill("What changed in AWS in the last 24 h? Use get_aws_changes.");
  await composer.press("Enter");
  const answer = page.getByText(/Aws changes: total/i).last();
  await expect(answer).toBeVisible({ timeout: 60_000 });
  const said = (await answer.innerText()).trim();
  const m = said.match(/total (at least )?(\d+), errors (\d+), complete (yes|no)/);
  const problems: string[] = [];
  if (!m) problems.push(`agent answer has no total: ${said}`);
  else {
    if (Number(m[2]) !== wire.json.summary.total) problems.push(`agent total ${m[2]} != API total ${wire.json.summary.total}`);
    if (Number(m[3]) !== wire.json.summary.errors) problems.push(`agent errors ${m[3]} != API errors ${wire.json.summary.errors}`);
    if ((m[4] === "yes") !== wire.json.complete) problems.push(`agent complete ${m[4]} != API complete ${wire.json.complete}`);
    const shown = view.numbers.find((n) => n.key === "changes")?.value;
    if (shown && Number(shown.replace(/,/g, "")) !== Number(m[2])) problems.push(`agent total ${m[2]} != page Changes ${shown}`);
  }
  await shot(page, viewport, "dark", `${STATE}-ask-ai`);
  record("ask AI", { said, page: view.numbers, api: wire.json.summary, problems, http: watch.http });
  expect(problems).toEqual([]);
});

// ---------------------------------------------------------------------------
// First read, page left alone: right after connect the card's line reads
// "Reading the last 24 h…". A user who opens it then and waits must see the
// answer arrive without pressing refresh.

test("first read, left open without refresh", async ({ browser }) => {
  test.setTimeout(240_000);
  const viewport = DESKTOP;
  const watch: Watch = { console: [], http: [] };
  const page = await open(browser, viewport, "dark", "/aws/changes", watch);
  await settled(page);
  const first = (await readPage(page)).warning;
  await shot(page, viewport, "dark", `${STATE}-first-read`);
  const timeline: Array<{ at: string; warning: string; apiPending: boolean }> = [];
  const end = Date.now() + 150_000;
  while (Date.now() < end) {
    const wire = await api(page);
    timeline.push({ at: new Date().toISOString(), warning: (await readPage(page)).warning, apiPending: wire.json.sync.first_read_pending });
    await page.waitForTimeout(10_000);
  }
  await shot(page, viewport, "dark", `${STATE}-first-read-after-150s`);
  const apiDone = timeline.find((t) => !t.apiPending);
  const stale = apiDone ? timeline.filter((t) => t.at >= apiDone.at && t.warning.startsWith("Reading")) : [];
  record("first read left open", { first, timeline, staleSamples: stale.length });
  expect(stale, "page still says Reading after the API finished the first read").toEqual([]);
});

// ---------------------------------------------------------------------------
// Compare from an exact time (the value "Use HH:MM" sets), then "and N more ›".

test("compare from an exact time", async ({ browser }) => {
  const from = process.env.AWS_CHANGES_COMPARE_FROM ?? "";
  test.skip(!from, "needs AWS_CHANGES_COMPARE_FROM");
  test.setTimeout(90_000);
  const scope = new URLSearchParams();
  if (process.env.AWS_CHANGES_COMPARE_ACCOUNT) scope.set("account", process.env.AWS_CHANGES_COMPARE_ACCOUNT);
  if (process.env.AWS_CHANGES_COMPARE_REGION) scope.set("region", process.env.AWS_CHANGES_COMPARE_REGION);
  const scoped = scope.toString() ? `&${scope.toString()}` : "";
  const viewport = DESKTOP;
  const watch: Watch = { console: [], http: [] };
  const page = await open(browser, viewport, "dark", `/aws/changes?from=${encodeURIComponent(from)}${scoped}`, watch);
  await settled(page);
  const panel = page.locator("[data-compare]").first();
  await expect(panel).toBeVisible();
  await page.waitForTimeout(800);
  if (process.env.AWS_CHANGES_COMPARE_ALL === "1") {
    const showMore = panel.getByRole("button", { name: /^Show \d+ more/ });
    if (await showMore.count()) await showMore.first().click();
  }
  const notes: Record<string, unknown> = { panel: (await panel.innerText()).slice(0, 1200) };
  const wire = await (await page.request.get(`/api/admin/aws/changes/compare?from=${encodeURIComponent(from)}${scoped}`)).json();
  notes.api = { status: wire.status, complete: wire.complete, added: wire.added?.length, removed: wire.removed?.length, changed: wire.changed?.length, partial: wire.partial?.length, between: wire.changes_between_total, from: wire.from, to: wire.to };
  await shot(page, viewport, "dark", `${STATE}-compare-exact`);
  const more = page.locator("[data-changes-between-more]");
  if (await more.count()) {
    await more.first().click();
    await settled(page);
    await page.waitForTimeout(800);
    notes.andMore = { url: page.url(), span: await page.locator("[data-span]").innerText().catch(() => ""), numbers: (await readPage(page)).numbers };
    await shot(page, viewport, "dark", `${STATE}-compare-and-more`);
    await page.getByRole("button", { name: "Clear span" }).click();
    await settled(page);
    notes.cleared = page.url();
  }
  record("compare exact", { notes, http: watch.http });
  expect(watch.http.filter((h) => !h.includes("/enterprise/api/sso/deployment"))).toEqual([]);
});

// ---------------------------------------------------------------------------
// The list moved on: a later page whose cursor window left the 24 h tier.
// FIXTURE-ASSISTED: the real first page is fetched from the server and only
// its next_cursor is aged by 26 h (cursors are unsigned base64 JSON); the 400
// cursor_expired that follows is the real server's answer.

test("expired cursor reloads quietly (fixture-assisted cursor)", async ({ browser }) => {
  test.setTimeout(90_000);
  const viewport = DESKTOP;
  const watch: Watch = { console: [], http: [] };
  const context = await browser.newContext({ viewport: { width: viewport.width, height: viewport.height }, colorScheme: "dark" });
  contexts.push(context);
  await context.addInitScript(() => localStorage.setItem("versus.theme", "dark"));
  const page = await context.newPage();
  page.on("response", (r) => {
    if (r.url().includes("/api/") && r.status() >= 400) watch.http.push(`${r.status()} ${new URL(r.url()).pathname}`);
  });
  await openApp(page, "/aws/changes");
  let aged = 0;
  await page.route(/\/api\/admin\/aws\/changes\?(?!.*cursor=)/, async (route) => {
    const response = await route.fetch();
    const body = await response.json();
    if (body.next_cursor && aged === 0) {
      const c = JSON.parse(Buffer.from(body.next_cursor, "base64url").toString("utf8"));
      const back = (iso: string) => new Date(Date.parse(iso) - 26 * 3600_000).toISOString();
      c.s = back(c.s);
      c.u = back(c.u);
      body.next_cursor = Buffer.from(JSON.stringify(c), "utf8").toString("base64url");
      aged++;
    }
    await route.fulfill({ response, json: body });
  });
  await page.goto("/aws/changes");
  await settled(page);
  const more = page.locator("[data-list-scroll]").getByRole("button", { name: "Load more", exact: true });
  await expect(more).toBeVisible();
  const before = await page.locator("[data-item]").count();
  await more.scrollIntoViewIfNeeded();
  await more.click();
  await expect(page.locator("[data-moved-on]")).toBeVisible({ timeout: 15_000 });
  const note = await page.locator("[data-moved-on]").innerText();
  await shot(page, viewport, "dark", `${STATE}-moved-on`);
  const after = await page.locator("[data-item]").count();
  const errorShown = await page.getByText("Couldn't load changes").count();
  record("moved on", { aged, note, before, after, errorShown, http: watch.http });
  expect(errorShown).toBe(0);
  expect(watch.http.some((h) => h.startsWith("400 /api/admin/aws/changes"))).toBe(true);
});

// ---------------------------------------------------------------------------
// AWS > Settings: each account's state and Region notes, as a user reads them.

test("settings page states", async ({ browser }) => {
  test.setTimeout(60_000);
  const problems: string[] = [];
  for (const viewport of VIEWPORTS) {
    for (const theme of THEMES) {
      const watch: Watch = { console: [], http: [] };
      const page = await open(browser, viewport, theme, "/aws/settings", watch);
      await page.waitForLoadState("networkidle").catch(() => {});
      await page.waitForTimeout(1000);
      const full = await page.locator("main").innerText();
      const text = full.slice(0, 6000);
      const file = await shot(page, viewport, theme, `${STATE}-settings`);
      // QA-514: no service reads "Not enabled" (a Region left off is named
      // once, at the Region); AWS Health and AppRegistry name their real reason.
      const local: string[] = [];
      const lines = full.split("\n").map((l) => l.trim()).filter(Boolean);
      for (const [i, l] of lines.entries()) {
        if (/\bNot enabled\b/.test(l)) local.push(`"Not enabled" on screen: "${lines.slice(Math.max(0, i - 1), i + 1).join(" / ")}"`);
        if (/^AWS Health\b/.test(l) && /not|denied/i.test(lines.slice(i, i + 2).join(" ")) && !/Not on this support plan/.test(lines.slice(i, i + 2).join(" "))) local.push(`AWS Health reads "${lines.slice(i, i + 2).join(" / ")}"`);
        if (/AppRegistry/.test(l) && /not|denied/i.test(lines.slice(i, i + 2).join(" ")) && !/Not offered in this Region/.test(lines.slice(i, i + 2).join(" "))) local.push(`AppRegistry reads "${lines.slice(i, i + 2).join(" / ")}"`);
      }
      local.push(...banned("settings", full));
      const sync = await page.request.get("/api/admin/aws/settings").then((r) => r.json()).catch(() => ({}));
      const acct = (sync.accounts ?? [])[0] ?? {};
      const syncServices = ((acct.sync?.services ?? []) as Array<Record<string, unknown>>).map((x) => ({ service: x.service, status: x.status, gaps: x.gaps }));
      record(`settings ${viewport.name} ${theme}`, { file, text, checking: /checking/i.test(text), optIn: text.match(/opt-in status[^\n]*/gi) ?? [], syncLabel: acct.sync?.label ?? null, syncServices, notOnPlan: acct.services?.not_on_plan ?? null, problems: local, http: watch.http });
      problems.push(...local.map((p) => `${theme}: ${p}`));
    }
  }
  expect(problems).toEqual([]);
});

// ---------------------------------------------------------------------------
// Routine agent updates: counted, hidden by default, shown on request.

test("routine counts toggle", async ({ browser }) => {
  const viewport = DESKTOP;
  const results: Record<string, unknown> = {};
  const problems: string[] = [];
  for (const theme of THEMES) {
    const watch: Watch = { console: [], http: [] };
    const page = await open(browser, viewport, theme, `/aws/changes${process.env.AWS_CHANGES_ROUTINE_QUERY ?? ""}`, watch);
    await settled(page);
    const wire = await api(page);
    const toggle = page.getByRole("button", { name: /routine/i }).first();
    const notes: Record<string, unknown> = { routine: wire.json.summary.routine, toggle: await toggle.innerText().catch(() => "") };
    await shot(page, viewport, theme, `${STATE}-routine-hidden`);
    if ((wire.json.summary.routine ?? 0) > 0 && !(await toggle.count())) problems.push(`${theme}: ${wire.json.summary.routine} routine updates but no toggle`);
    if (await toggle.count()) {
      const before = await page.locator('[data-kind="routine"]').count();
      if (before > 0) problems.push(`${theme}: routine rows listed before asking`);
      await toggle.click();
      await settled(page);
      await page.waitForTimeout(800);
      notes.rows = await page.locator('[data-kind="routine"]').allInnerTexts();
      if ((notes.rows as string[]).length === 0) problems.push(`${theme}: Show routine lists none`);
      const plural = (notes.rows as string[]).filter((t) => /(^|\n)1 routine updates\b/.test(t));
      if (plural.length) problems.push(`${theme}: "1 routine updates"`);
      notes.url = page.url();
      notes.toggleAfter = await toggle.innerText().catch(() => "");
      problems.push(...(await audit(page)).map((p) => `${theme}: ${p}`));
      await shot(page, viewport, theme, `${STATE}-routine-shown`);
    }
    results[theme] = { notes, http: watch.http };
  }
  record("routine", { results, problems });
  expect(problems).toEqual([]);
});

// ---------------------------------------------------------------------------
// Readings at each width and theme: the Accounts filter holds to the screen
// and its options read in full; rows carry a Region chip when more than one
// Region is in scope; the "<account>, as of" chip only on rows newer than that
// account's as_of; no account rows under the grid (that state lives in the
// summary card now).

type Item = { id: string; time: string; account: string; region: string };

for (const viewport of VIEWPORTS) {
  for (const theme of THEMES) {
    test(`readings (${viewport.name}, ${theme})`, async ({ browser }) => {
      test.setTimeout(90_000);
      const watch: Watch = { console: [], http: [] };
      const page = await open(browser, viewport, theme, "/aws/changes", watch);
      await settled(page);
      const problems: string[] = [];
      const wire = (await (await page.request.get("/api/admin/aws/changes?window=24h&limit=50")).json()) as Omit<Changes, "items"> & { items: Item[] };
      const dom = await page.evaluate(() => {
        const rect = (el: Element | null) => {
          if (!el) return null;
          const b = el.getBoundingClientRect();
          return { left: Math.round(b.left), right: Math.round(b.right), top: Math.round(b.top), bottom: Math.round(b.bottom) };
        };
        return {
          accountRows: document.querySelectorAll("[data-account-gap], [data-reconnect-outside]").length,
          rows: Array.from(document.querySelectorAll<HTMLElement>("[data-item]")).map((e) => ({
            id: e.getAttribute("data-item"),
            kind: e.getAttribute("data-kind"),
            region: e.querySelector("[data-region-chip]")?.textContent?.trim() ?? null,
            asOf: e.querySelector("[data-as-of-chip]")?.textContent?.trim() ?? null,
          })),
          filter: rect(document.querySelector("[data-accounts-filter]")),
          filterButton: rect(document.querySelector("[data-accounts-filter] button")),
          viewport: window.innerWidth,
        };
      });
      if (dom.accountRows) problems.push(`${dom.accountRows} account rows under the grid`);
      for (const [n, b] of [["filter", dom.filter], ["filter button", dom.filterButton]] as const) {
        if (b && (b.right > dom.viewport + 1 || b.left < -1)) problems.push(`Accounts ${n} runs past the screen (${b.left}..${b.right} of ${dom.viewport})`);
      }
      const regionsInScope = new Set(wire.items.map((i) => i.region)).size;
      const byId = new Map(wire.items.map((i) => [i.id, i]));
      const acct = new Map(wire.accounts.map((a) => [a.account, a]));
      for (const row of dom.rows) {
        const it = row.id ? byId.get(row.id) : undefined;
        if (!it) continue;
        const asOf = acct.get(it.account)?.as_of;
        const newer = Boolean(asOf) && Date.parse(it.time) > Date.parse(asOf as string);
        if (newer !== Boolean(row.asOf)) problems.push(`row ${it.time} ${it.account}: as-of chip ${row.asOf ? "shown" : "missing"} (as_of ${asOf ?? "none"})`);
        if (regionsInScope > 1 && row.kind !== "routine" && !row.region && it.region) problems.push(`row ${it.time} has no Region chip with ${regionsInScope} Regions in scope`);
      }
      let options: Array<{ text: string; clipped: boolean; right: number }> = [];
      const picker = page.getByRole("button", { name: "Accounts", exact: true });
      if (await picker.count()) {
        await picker.first().click();
        await page.waitForTimeout(250);
        options = await page.getByRole("option").evaluateAll((els) =>
          els.map((e) => {
            const h = e as HTMLElement;
            const b = h.getBoundingClientRect();
            const inner = Array.from(h.querySelectorAll<HTMLElement>("*")).some((c) => c.scrollWidth - c.clientWidth > 1);
            return { text: h.innerText.trim(), clipped: h.scrollWidth - h.clientWidth > 1 || inner, right: Math.round(b.right) };
          }),
        );
        for (const o of options) {
          if (o.clipped) problems.push(`Accounts option clipped: ${o.text}`);
          if (o.right > dom.viewport + 1) problems.push(`Accounts option runs past the screen: ${o.text}`);
        }
        await page.keyboard.press("Escape");
      }
      const file = await shot(page, viewport, theme, `${STATE}-readings`);
      record(`readings ${viewport.name} ${theme}`, { file, dom: { ...dom, rows: dom.rows.slice(0, 20) }, options, regionsInScope, accounts: wire.accounts, problems, console: watch.console, http: watch.http });
      expect.soft(watch.http.filter((h) => !h.includes("/enterprise/api/sso/deployment")), "no unexplained 4xx/5xx").toEqual([]);
      expect(problems).toEqual([]);
    });
  }
}

// ---------------------------------------------------------------------------
// A disconnected account (A11 C9 QA-499 / QA-501, C12): after
// role-session-expired and two Recheck presses, Changes and Settings tell one
// story, in the viewer's local time with no "UTC". The card's warning line
// reads "Data not complete after <as_of>: <name> was disconnected at
// <disconnected_at>." with Reconnect ›, its popover lists each of the
// account's stretches as "account disconnected", Accounts reads 1/2, and the
// Accounts option reads "<name> (<id>) — as of <as_of>".
// AWS_CHANGES_DISCONNECTED names the account ID; AWS_CHANGES_TZ adds
// comma-separated IANA zones (desktop, light) besides the host zone.

const DISCONNECTED = process.env.AWS_CHANGES_DISCONNECTED ?? "";
const EXTRA_ZONES = (process.env.AWS_CHANGES_TZ ?? "").split(",").map((v) => v.trim()).filter(Boolean);

const DISCONNECTED_RUNS: Array<{ viewport: Viewport; theme: Theme; zone?: string }> = [
  ...VIEWPORTS.flatMap((viewport) => THEMES.map((theme) => ({ viewport, theme }))),
  ...EXTRA_ZONES.map((zone) => ({ viewport: DESKTOP as Viewport, theme: "light" as Theme, zone })),
];

for (const run of DISCONNECTED_RUNS) {
  const label = `${run.viewport.name}, ${run.theme}${run.zone ? `, ${run.zone}` : ""}`;
  test(`disconnected account, one story in local time (${label})`, async ({ browser }) => {
    test.skip(!DISCONNECTED, "needs AWS_CHANGES_DISCONNECTED (account ID) after role-session-expired and two Recheck presses");
    test.setTimeout(90_000);
    const { viewport, theme, zone } = run;
    const tag = zone ? `-${zone.replace(/[^A-Za-z0-9]+/g, "-").toLowerCase()}` : "";
    const watch: Watch = { console: [], http: [] };
    const problems: string[] = [];
    const page = await open(browser, viewport, theme, "/aws/changes", watch, { timezoneId: zone, hasTouch: viewport.width < 768 });
    await settled(page);
    await page.waitForTimeout(500);
    const wire = (await api(page)).json;
    const settingsWire = (await (await page.request.get("/api/admin/aws/settings")).json()) as {
      accounts: Array<{ account_id: string; display_name: string; status: string; disconnected_since: string | null }>;
    };
    const acct = wire.accounts.find((a) => a.account === DISCONNECTED);
    const setting = settingsWire.accounts.find((a) => a.account_id === DISCONNECTED);
    const name = setting?.display_name ?? "";
    if (acct?.reason !== "account_disconnected") problems.push(`API reason ${acct?.reason}`);
    if (!acct?.as_of || !acct?.disconnected_at) problems.push(`API as_of ${acct?.as_of} disconnected_at ${acct?.disconnected_at}`);
    if (setting?.status !== "disconnected") problems.push(`Settings API status ${setting?.status}`);
    const now = new Date().toISOString();
    const until = wire.window?.until ?? now;
    const expDisc = await localClock(page, acct?.disconnected_at ?? now, until, true);
    const expAsOf = await localClock(page, acct?.as_of ?? now, until, true);
    const view = await readPage(page);
    problems.push(...cardCheck(view, wire));
    const others = wire.accounts.filter((a) => a.account !== DISCONNECTED && !a.complete).length;
    const warnWant = `Data not complete after ${expAsOf}: ${name} was disconnected at ${expDisc}${others ? "" : "."}`;
    if (!view.warning.startsWith(warnWant)) problems.push(`warning "${view.warning}", want "${warnWant}"`);
    if (view.reconnect !== "/aws/settings") problems.push(`Reconnect › in the warning line: ${view.reconnect}`);
    const reconnectHit = await page.evaluate(() => {
      const el = document.querySelector<HTMLElement>("[data-warning-line] [data-reconnect]");
      if (!el) return { hit: false, w: 0, h: 0 };
      el.scrollIntoView({ block: "center" });
      const b = el.getBoundingClientRect();
      const at = document.elementFromPoint(b.left + b.width / 2, b.top + b.height / 2);
      return { hit: Boolean(at && (at === el || el.contains(at))), w: Math.round(b.width), h: Math.round(b.height), right: Math.round(b.right), vw: window.innerWidth };
    });
    if (!reconnectHit.hit) problems.push(`Reconnect › not hit-testable ${JSON.stringify(reconnectHit)}`);
    const chips = await page.locator("[data-as-of-chip]").allInnerTexts();
    for (const c of chips.map(norm).filter((t) => t.startsWith(`${name},`))) if (c !== `${name}, as of ${expAsOf}`) problems.push(`chip "${c}", want "${name}, as of ${expAsOf}"`);
    if (/\bUTC\b/.test(await page.locator("main").innerText())) problems.push('Changes page prints "UTC"');
    const capture = (s: string) => shotView(page, viewport, theme, `${STATE}-${s}${tag}`);
    const pop = await warningPopover(page, wire, problems, capture);
    const mine = (pop?.rows ?? []).filter((r) => r.text.startsWith(name));
    if (!mine.length) problems.push(`popover has no ${name} row`);
    for (const r of mine) {
      if (r.reason !== "account_disconnected" || !r.text.includes("account disconnected")) problems.push(`popover row "${r.text}" is not "account disconnected"`);
      if (!r.text.includes("Reconnect ›")) problems.push(`popover row "${r.text}" has no Reconnect ›`);
    }
    // Accounts option.
    const accounts = page.getByRole("button", { name: "Accounts", exact: true });
    let option = "";
    if (await accounts.count()) {
      await accounts.click();
      option = norm((await page.getByRole("option").allInnerTexts()).find((o) => o.includes(`(${DISCONNECTED})`)) ?? "");
      if (option !== `${name} (${DISCONNECTED}) — as of ${expAsOf}`) problems.push(`Accounts option "${option}", want "${name} (${DISCONNECTED}) — as of ${expAsOf}"`);
      await page.keyboard.press("Escape");
    }
    const changesShot = await shot(page, viewport, theme, `disconnected${tag}`);

    // Settings: the same instant in the same words.
    await page.goto("/aws/settings");
    await page.waitForLoadState("networkidle").catch(() => {});
    await expect(page.locator("main")).toContainText(`${name} disconnected`, { timeout: 20_000 });
    const text = await page.locator("main").innerText();
    const settingsNow = new Date().toISOString();
    const expSince = await localClock(page, setting?.disconnected_since ?? settingsNow, settingsNow, true);
    const banner = text.match(new RegExp(`${name} disconnected since ([A-Z][a-z]{2} \\d{1,2} )?\\d{2}:\\d{2}`))?.[0] ?? "";
    if (banner !== `${name} disconnected since ${expSince}`) problems.push(`Settings banner "${banner}", want "${name} disconnected since ${expSince}"`);
    if (/\bUTC\b/.test(text)) problems.push(`Settings prints "UTC": ${text.match(/[^\n]*\bUTC\b[^\n]*/)?.[0]}`);
    const settingsDisc = banner.replace(`${name} disconnected since `, "");
    if (settingsDisc !== expDisc) problems.push(`same instant reads "${settingsDisc}" in Settings and "${expDisc}" in Changes`);
    const settingsShot = await shot(page, viewport, theme, `disconnected-settings${tag}`);

    // Click Reconnect where a user would: the card's warning line.
    await page.goto("/aws/changes");
    await settled(page);
    await page.locator("[data-warning-line] [data-reconnect]").click();
    await page.waitForURL(/\/aws\/settings/, { timeout: 10_000 }).catch(() => problems.push(`Reconnect did not open Settings (at ${page.url()})`));

    record(`disconnected ${label}`, {
      zone: zone ?? Intl.DateTimeFormat().resolvedOptions().timeZone,
      api: { account: acct, settings: setting, gaps: wire.gaps.filter((g) => g.account === DISCONNECTED) },
      view: { warning: view.warning, numbers: view.numbers, reconnect: view.reconnect },
      reconnectHit,
      popover: pop,
      option,
      chips,
      expected: { warnWant, expDisc, expAsOf, expSince },
      files: [changesShot, settingsShot],
      problems,
      console: watch.console,
      http: watch.http,
    });
    expect.soft(watch.http.filter((h) => !h.includes("/enterprise/api/sso/deployment")), "no unexplained 4xx/5xx").toEqual([]);
    expect(problems).toEqual([]);
  });
}
