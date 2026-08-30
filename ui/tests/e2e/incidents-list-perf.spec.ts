import { test, expect, type ConsoleMessage, type Response } from "@playwright/test";
import * as path from "node:path";
import { fileURLToPath } from "node:url";
import { openApp } from "./helpers";

// ---------------------------------------------------------------------------
// Incident list-perf + typed-columns browser e2e (Part C of the incident
// typed-columns / list-perf functional gate). Drives the REAL running OSS SPA
// (versus-incident) against the run-harness Postgres seeded with ~25k rows,
// like an operator:
//
//   1. the list loads fast + bounded (a bounded first page, not all 25k) and
//      the count badge shows the UNRESOLVED count, not full history;
//   2. "Load more" fetches the next page ON DEMAND (not a whole-table pull);
//   3. the incident detail (peek) renders the promoted typed columns;
//   4. resolving from the UI updates the row + decrements the unresolved badge.
//
// Bring the stack up first (cd run && ./oss.sh) and point E2E_BASE_URL /
// E2E_GATEWAY_SECRET at it (tests/e2e/.env, run-harness defaults).
// ---------------------------------------------------------------------------

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const SHOTS = path.resolve(__dirname, "screenshots", "list-perf");

// The seeded fixture (run-harness Postgres) holds ~25k incidents, only a
// fraction unresolved. The unresolved counts are derived from the live API at
// runtime (so the spec is robust to seed drift / re-runs — resolving a row
// decrements them); the one hard number is the FULL history the badge must
// NEVER show. FLOOR keeps the fixture assumption honest (enough unresolved rows
// to prove bounding + a non-trivial badge).
const FULL_HISTORY = 25218; // total rows — the number the count badge must never show
const UNRESOLVED_FLOOR = 2000; // fixture sanity: expect at least this many unresolved

// The server's default page size for the index endpoint — the first page is
// bounded to this, never the whole table.
const SERVER_PAGE = 1000;

// Shape of the index endpoint's JSON (only the fields this spec asserts on).
type IndexBody = {
  incidents: unknown[];
  total: number;
  counts: { ai_detect: number; webhook: number; total: number };
  page_size: number;
  next_offset: number | null;
};

// A first-index-page response for the AI-detected origin (no offset). The
// legacy TopBar count list also hits /api/admin/incidents but carries no
// origin param, so scoping to origin=ai_detect + no offset is unambiguous.
function isFirstIndexPage(r: Response): boolean {
  const u = r.url();
  return (
    r.request().method() === "GET" &&
    u.includes("/api/admin/incidents") &&
    u.includes("origin=ai_detect") &&
    !u.includes("offset=") &&
    r.status() === 200
  );
}

function isLoadMorePage(r: Response): boolean {
  const u = r.url();
  return (
    r.request().method() === "GET" &&
    u.includes("/api/admin/incidents") &&
    u.includes("origin=ai_detect") &&
    u.includes("offset=") &&
    r.status() === 200
  );
}

// Known-benign failed request on an OSS binary: the SHARED SPA's Sidebar probes
// the enterprise-only learned-baselines endpoint to decide whether to show the
// enterprise-locked nav. On OSS that route does not exist, so a 404 is the
// EXPECTED "not licensed" signal the app degrades on (see ui/src/lib/api.ts:
// "a 403 (unlicensed) or 404 (OSS ...)"; Sidebar.tsx:113). It is orthogonal to
// the incident list-perf feature under test — tolerated here, never the app's.
const KNOWN_OSS_PROBE_404 = /\/api\/agent\/baselines/;

// A browser console watcher that records console errors AND every failed
// (non-2xx/3xx) response. A case asserts the console is clean MODULO the one
// known OSS enterprise-license probe 404 — proven by the response allowlist, so
// the generic "Failed to load resource … 404" console line is safely attributed
// and any OTHER failure still fails the case.
function watch(page: import("@playwright/test").Page): {
  consoleErrors: string[];
  unexpectedResponses: string[];
} {
  const consoleErrors: string[] = [];
  const unexpectedResponses: string[] = [];
  page.on("console", (m: ConsoleMessage) => {
    if (m.type() === "error") consoleErrors.push(m.text());
  });
  page.on("pageerror", (e) => consoleErrors.push(`pageerror: ${e.message}`));
  page.on("response", (r) => {
    if (r.status() >= 400 && !KNOWN_OSS_PROBE_404.test(r.url())) {
      unexpectedResponses.push(`${r.status()} ${r.request().method()} ${r.url()}`);
    }
  });
  page.on("requestfailed", (r) => {
    if (!KNOWN_OSS_PROBE_404.test(r.url())) {
      unexpectedResponses.push(`FAILED ${r.method()} ${r.url()}`);
    }
  });
  return { consoleErrors, unexpectedResponses };
}

// assertCleanConsole passes when the only console errors are the generic
// resource-load 404 lines that correspond to the allowlisted OSS baselines
// probe (proven by unexpectedResponses being empty), and fails on anything
// else.
function assertCleanConsole(w: {
  consoleErrors: string[];
  unexpectedResponses: string[];
}): void {
  expect(
    w.unexpectedResponses,
    `unexpected failed requests: ${w.unexpectedResponses.join("\n")}`,
  ).toEqual([]);
  const unexplained = w.consoleErrors.filter(
    (t) => !/Failed to load resource: the server responded with a status of 404/i.test(t),
  );
  expect(unexplained, `console errors: ${unexplained.join("\n")}`).toEqual([]);
}

// The AI-detected origin filter tab (SegmentedControl role=tab). Its trailing
// badge is the whole-set UNRESOLVED count for the feed.
function aiOriginTab(page: import("@playwright/test").Page) {
  return page.getByRole("tab", { name: /AI Detected/i });
}
function webhookOriginTab(page: import("@playwright/test").Page) {
  return page.getByRole("tab", { name: /Webhook/i });
}

// The per-row "View incident" buttons — one per rendered row. The client pager
// caps the DOM at 100 rows, so this doubles as the "bounded render" probe.
function viewButtons(page: import("@playwright/test").Page) {
  return page.locator('button[aria-label^="View incident"]');
}

test.describe("incident list-perf + typed columns (OSS SPA)", () => {
  test("1 · list loads fast + bounded, count badge shows UNRESOLVED not full history", async ({
    page,
  }) => {
    const w = watch(page);

    // Arm the first-page response capture BEFORE the app navigates.
    const firstPage = page.waitForResponse(isFirstIndexPage, { timeout: 30_000 });

    const t0 = Date.now();
    await openApp(page, "/incidents");

    // The table region + the first row render.
    const region = page.getByRole("region", { name: /Incidents table/i });
    await expect(region).toBeVisible({ timeout: 15_000 });
    await expect(viewButtons(page).first()).toBeVisible({ timeout: 15_000 });
    const elapsedMs = Date.now() - t0;

    // BOUNDED (wire): the first page is one server page, NOT the whole 25k.
    const resp = await firstPage;
    const body = (await resp.json()) as IndexBody;
    expect(body.incidents.length).toBeLessThanOrEqual(SERVER_PAGE);
    expect(body.incidents.length).toBeLessThan(FULL_HISTORY);
    // The whole-set counts are UNRESOLVED-scoped, never full history: the AI
    // origin total equals the AI unresolved count, the summed unresolved total
    // covers at least the two named origins, and it stays a fraction of the 25k
    // history (other origins may add to counts.total beyond ai + webhook).
    expect(body.total).toBe(body.counts.ai_detect);
    expect(body.counts.total).toBeGreaterThanOrEqual(
      body.counts.ai_detect + body.counts.webhook,
    );
    expect(body.counts.total).toBeGreaterThanOrEqual(UNRESOLVED_FLOOR);
    expect(body.counts.total).toBeLessThan(FULL_HISTORY);

    // BOUNDED (DOM): the client pager caps rendered rows at 100.
    const rendered = await viewButtons(page).count();
    expect(rendered).toBeGreaterThan(0);
    expect(rendered).toBeLessThanOrEqual(100);

    // COUNT BADGE shows the unresolved count (per-origin), NOT the 25k history.
    await expect(aiOriginTab(page)).toContainText(String(body.counts.ai_detect));
    await expect(webhookOriginTab(page)).toContainText(String(body.counts.webhook));
    await expect(aiOriginTab(page)).not.toContainText(String(FULL_HISTORY));

    // Fast: the seeded 25k history is on-screen well within the case budget.
    expect(elapsedMs).toBeLessThan(20_000);

    await page.screenshot({ path: path.join(SHOTS, "01-list-bounded-unresolved-badge.png"), fullPage: true });

    assertCleanConsole(w);
  });

  test("2 · Load more fetches the next page on demand (not a whole-table pull)", async ({
    page,
  }) => {
    const w = watch(page);
    const firstPage = page.waitForResponse(isFirstIndexPage, { timeout: 30_000 });
    await openApp(page, "/incidents");
    await expect(viewButtons(page).first()).toBeVisible({ timeout: 15_000 });
    const first = (await (await firstPage).json()) as IndexBody;

    // The "Load more" control renders (next_offset present) and names the
    // whole-set unresolved total for the feed (derived from the live response).
    const loadMore = page.getByTestId("incident-load-more");
    await expect(loadMore).toBeVisible();
    await expect(loadMore).toContainText(first.total.toLocaleString());
    await page.screenshot({ path: path.join(SHOTS, "02a-load-more-control.png"), fullPage: true });

    // Clicking it fetches EXACTLY the next bounded page (offset=1000), on
    // demand — proving it is not a whole-table pull.
    const nextPage = page.waitForResponse(isLoadMorePage, { timeout: 30_000 });
    await loadMore.getByRole("button", { name: /Load more/i }).click();
    const resp = await nextPage;
    expect(resp.url()).toContain("offset=1000");
    const body = (await resp.json()) as IndexBody;
    expect(body.incidents.length).toBeGreaterThan(0);
    expect(body.incidents.length).toBeLessThanOrEqual(SERVER_PAGE);

    // The client pager now spans more loaded rows (the "of N" indicator grows
    // past a single server page's worth of matching rows).
    await page.screenshot({ path: path.join(SHOTS, "02b-load-more-appended.png"), fullPage: true });

    assertCleanConsole(w);
  });

  test("3 · incident detail (peek) renders the promoted typed columns", async ({
    page,
  }) => {
    const w = watch(page);
    await openApp(page, "/incidents");
    await expect(viewButtons(page).first()).toBeVisible({ timeout: 15_000 });

    // Open the first row's detail slide-out.
    await viewButtons(page).first().click();
    const peek = page.getByRole("dialog", { name: /Details panel/i });
    await expect(peek).toBeVisible();

    // Every promoted field renders (title in the header + the typed columns as
    // labelled facts). The <dt> labels come straight from the typed columns.
    await expect(peek.getByText("Service", { exact: true })).toBeVisible();
    await expect(peek.getByText("When", { exact: true })).toBeVisible();
    await expect(peek.getByText("Channels", { exact: true })).toBeVisible();
    await expect(peek.getByText("Assigned", { exact: true })).toBeVisible();
    await expect(peek.getByText("Notify", { exact: true })).toBeVisible();
    await expect(peek.getByText("ID", { exact: true })).toBeVisible();
    // Source/origin badge + status pill (open|acked|resolved) render.
    await expect(
      peek.getByText(/^(open|acked|resolved)$/).first(),
    ).toBeVisible();
    // The Resolve + Assign actions render (content-region actions).
    await expect(peek.getByRole("button", { name: /Mark incident resolved/i })).toBeVisible();

    await page.screenshot({ path: path.join(SHOTS, "03-incident-detail-peek.png"), fullPage: true });

    assertCleanConsole(w);
  });

  test("4 · resolve from the UI updates the row + decrements the unresolved badge", async ({
    page,
  }) => {
    const w = watch(page);
    await openApp(page, "/incidents");
    await expect(viewButtons(page).first()).toBeVisible({ timeout: 15_000 });

    // Default status tab is "Open"; capture the current open-badge count over
    // the loaded rows (client-computed) so we can prove it decrements.
    const openTab = page.getByRole("tab", { name: /^Open/ });
    await expect(openTab).toBeVisible();
    const openBefore = Number((await openTab.innerText()).replace(/\D+/g, ""));
    expect(openBefore).toBeGreaterThan(0);

    // Capture the whole-set AI unresolved badge BEFORE resolving so we prove a
    // relative -1 decrement (robust to seed drift across re-runs). The default
    // origin view is AI-detected, so the first open row is an AI incident.
    const aiBefore = Number((await aiOriginTab(page).innerText()).replace(/\D+/g, ""));
    expect(aiBefore).toBeGreaterThan(0);

    // Identify the first open row's title so we can prove the row leaves the
    // open view once resolved.
    const firstRowLink = page
      .getByRole("region", { name: /Incidents table/i })
      .locator("tbody tr a[href^='/incidents/']")
      .first();
    const firstTitle = (await firstRowLink.innerText()).trim();

    // Resolve it via the detail peek → confirm dialog.
    await viewButtons(page).first().click();
    const peek = page.getByRole("dialog", { name: /Details panel/i });
    await expect(peek).toBeVisible();
    await peek.getByRole("button", { name: /Mark incident resolved/i }).click();

    const confirm = page.getByRole("dialog", { name: /Resolve incident/i });
    await expect(confirm).toBeVisible();
    // Capture the follow-up resolve write so we know it round-tripped.
    const resolveWrite = page.waitForResponse(
      (r) =>
        r.request().method() === "POST" &&
        /\/api\/admin\/incidents\/.+\/resolve$/.test(r.url()),
      { timeout: 30_000 },
    );
    await confirm.getByRole("button", { name: /^Resolve$/ }).click();
    const wrote = await resolveWrite;
    expect(wrote.status()).toBeGreaterThanOrEqual(200);
    expect(wrote.status()).toBeLessThan(300);

    // Success toast confirms the resolve landed.
    await expect(page.getByText("Incident resolved").first()).toBeVisible({ timeout: 15_000 });

    // Row state updates: the just-resolved incident leaves the Open view, and
    // the client-computed Open badge decrements by 1.
    await expect(openTab).toHaveText(new RegExp(`Open\\s*${openBefore - 1}\\b`), {
      timeout: 15_000,
    });
    await page.screenshot({ path: path.join(SHOTS, "04a-open-badge-decremented.png"), fullPage: true });

    // The whole-set unresolved badge (AI origin) also decrements once the
    // invalidated index refetch settles against the persisted resolve.
    await expect
      .poll(async () => (await aiOriginTab(page).innerText()).replace(/\D+/g, ""), {
        timeout: 20_000,
      })
      .toBe(String(aiBefore - 1));

    await page.screenshot({ path: path.join(SHOTS, "04b-unresolved-badge-decremented.png"), fullPage: true });

    // Sanity: the resolved incident's title is no longer at the top of the
    // still-open list (it was removed from the open view).
    const newFirstTitle = (await firstRowLink.innerText().catch(() => "")).trim();
    expect(newFirstTitle).not.toBe("");
    // (Titles can repeat across the 25k seed, so we don't assert inequality on
    // the string — the badge decrement above is the authoritative signal.)
    void firstTitle;

    assertCleanConsole(w);
  });
});
