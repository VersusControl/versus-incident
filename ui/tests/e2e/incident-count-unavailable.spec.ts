import { expect, test, type Browser, type BrowserContext, type Page, type Route } from "@playwright/test";
import * as fs from "node:fs";
import * as path from "node:path";
import { capturePath, harnessResultsDir } from "./support/capture";

// Incident counts served from the server's count cache can be known, stale
// (with counted_at), or unknown (count_unknown: true, numbers null). The header
// badge, the Now tiles and tab badges, and the Incidents tabs must show "—"
// (accessible name "Count unavailable") for an unknown count or a failed first
// request, keep the last number after a failed refetch, and date a stale or
// kept number with "Counted at HH:MM". Never a 0 where the count is unknown.
//
// Deterministic intercepted UI evidence: run with E2E_UI_FIXTURES=1, which
// makes playwright.config.ts start the Vite dev server. Both count endpoints
// (GET /api/admin/incidents/counts and the counts in GET /api/admin/incidents)
// are route-intercepted to force each state.

type Mode = "known" | "stale" | "unknown" | "fail";
type Theme = "light" | "dark";
type State = "known" | "unknown" | "first-failed" | "refetch-failed" | "stale";

const STATES: State[] = ["known", "unknown", "first-failed", "refetch-failed", "stale"];
const VIEWPORTS = [
  { name: "1440", width: 1440, height: 900 },
  { name: "390", width: 390, height: 844 },
] as const;
const THEMES: Theme[] = ["light", "dark"];
const SECRET = "qa-count-secret";
const COOKIE = "versus_gateway_session=count-session";
const OPEN_BADGE = 'header a[href="/incidents?status=open"]';

test.use({ timezoneId: "UTC", locale: "en-US" });

const origin = (ai: number, webhook: number) => ({ ai_detect: ai, webhook, total: ai + webhook });
const BY_STATUS = {
  open: origin(1234, 56),
  acked: origin(8, 4),
  resolved: origin(3400, 2100),
  all: origin(4642, 2160),
};

// HH:MM of an ISO time in UTC (the test's timezone), as the hint shows it.
function hhmm(iso: string): string {
  return iso.slice(11, 16);
}

interface Ctl {
  counts: Mode;
  list: Mode;
  countReqs: number;
  countFails: number;
  listReqs: number;
  countedAt: string;
  staleAt: string;
}

function countsBody(ctl: Ctl, mode: Mode) {
  if (mode === "unknown") {
    return { ai_detect: null, webhook: null, total: null, by_status: null, count_unknown: true };
  }
  const base = { ...BY_STATUS.open, by_status: BY_STATUS };
  if (mode === "stale") return { ...base, counted_at: ctl.staleAt, stale: true };
  return { ...base, counted_at: ctl.countedAt };
}

function rows(n: number, offset: number) {
  const now = Date.now();
  return Array.from({ length: n }, (_, k) => {
    const i = offset + k;
    return {
      id: `incident-count-${i}`,
      title: `Checkout latency above objective ${i}`,
      source: "agent:scripted-detection-source",
      origin: "ai_detect",
      service: i % 2 ? "checkout" : "payments",
      severity: i % 3 ? "high" : "critical",
      resolved: false,
      notify_status: "sent",
      created_at: new Date(now - (i + 1) * 90_000).toISOString(),
    };
  });
}

async function json(route: Route, value: unknown, status = 200) {
  await route.fulfill({ status, contentType: "application/json", body: JSON.stringify(value) });
}

async function installApi(page: Page, ctl: Ctl) {
  await page.route("**/api/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const p = url.pathname;
    if (p === "/api/auth/gateway-session" && request.method() === "POST") {
      if (request.headers()["x-gateway-secret"] !== SECRET) return json(route, { error: "unauthorized" }, 401);
      return route.fulfill({
        status: 204,
        headers: { "Set-Cookie": `${COOKIE}; Path=/; HttpOnly; SameSite=Strict` },
        body: "",
      });
    }
    if (p === "/api/admin/config/agent") {
      if (!request.headers().cookie?.includes(COOKIE)) return json(route, { error: "unauthorized" }, 401);
      return json(route, {});
    }
    if (p.endsWith("/deployment")) return json(route, { error: "community" }, 403);
    if (p === "/api/admin/incidents/counts") {
      ctl.countReqs++;
      if (ctl.counts === "fail") {
        ctl.countFails++;
        return json(route, { error: "count incidents: timed out" }, 500);
      }
      return json(route, { ...countsBody(ctl, ctl.counts), count_window: "7d" });
    }
    if (p === "/api/admin/incidents") {
      // The Now page's feed asks without an origin; it is not a count surface.
      if (!url.searchParams.has("origin")) {
        return json(route, { incidents: rows(60, 0), counts: countsBody(ctl, "known"), count_window: "7d" });
      }
      ctl.listReqs++;
      if (ctl.list === "fail") return json(route, { error: "list incident counts: timed out" }, 500);
      const offset = Number(url.searchParams.get("offset") ?? "0");
      // 120 rows: more than one 100-row table page, so the page never walks
      // ahead on its own and the "Load more" control stays on screen.
      return json(route, {
        incidents: rows(120, offset),
        counts: countsBody(ctl, ctl.list),
        count_window: "7d",
        total: ctl.list === "unknown" ? null : BY_STATUS.open.ai_detect + BY_STATUS.acked.ai_detect,
        offset,
        next_offset: offset + 120,
        page: 1,
        page_size: 120,
      });
    }
    if (p === "/api/admin/capabilities") return json(route, { search: false, report: false });
    if (p === "/api/admin/teams") return json(route, { teams: [] });
    if (p === "/api/admin/members") return json(route, { members: [] });
    if (p === "/api/agent/baselines") return json(route, { error: "not found" }, 404);
    return json(route, {});
  });
}

function newCtl(): Ctl {
  const now = Date.now();
  return {
    counts: "known",
    list: "known",
    countReqs: 0,
    countFails: 0,
    listReqs: 0,
    countedAt: new Date(now - 20_000).toISOString().replace(/\.\d+Z$/, "Z"),
    staleAt: new Date(now - 5 * 60_000).toISOString().replace(/\.\d+Z$/, "Z"),
  };
}

async function newSession(browser: Browser, vp: (typeof VIEWPORTS)[number], theme: Theme) {
  const context = await browser.newContext({
    viewport: { width: vp.width, height: vp.height },
    deviceScaleFactor: 2,
    colorScheme: theme,
    timezoneId: "UTC",
    locale: "en-US",
  });
  await context.addInitScript((t) => localStorage.setItem("versus.theme", t), theme);
  return context;
}

async function openSignedIn(context: BrowserContext, ctl: Ctl, url: string): Promise<Page> {
  const page = await context.newPage();
  // Natural-flow fake clock: time runs as usual, and the poll can be moved on
  // with fastForward instead of waiting 15-30 s for it.
  await page.clock.install();
  await installApi(page, ctl);
  await page.goto(url);
  const secret = page.getByLabel("Gateway secret");
  const authed = page.getByTestId("app-authenticated");
  await expect(secret.or(authed).first()).toBeVisible();
  if (await secret.isVisible()) {
    await secret.fill(SECRET);
    await page.getByRole("button", { name: "Sign in", exact: true }).click();
  }
  await expect(authed).toBeVisible();
  return page;
}

// Layout facts compared across states: a count changing state must not move
// or resize what surrounds it.
async function measure(page: Page, screen: "now" | "incidents") {
  return page.evaluate((screen) => {
    const box = (el: Element | null) => {
      if (!el) return null;
      const r = el.getBoundingClientRect();
      return { x: Math.round(r.x), y: Math.round(r.y), w: Math.round(r.width), h: Math.round(r.height) };
    };
    const overflow = (el: Element | null) =>
      el ? el.scrollWidth > el.clientWidth + 1 || el.scrollHeight > el.clientHeight + 1 : false;
    const header = document.querySelector("header");
    const badge = document.querySelector('header a[href="/incidents?status=open"]');
    const tabs = [...document.querySelectorAll('[role="tablist"]')].map((t) => ({
      label: t.getAttribute("aria-label"),
      box: box(t),
      scrolls: t.scrollWidth > t.clientWidth + 1,
      tabRights: [...t.querySelectorAll('[role="tab"]')].map((b) => Math.round(b.getBoundingClientRect().right)),
      barRight: Math.round(t.getBoundingClientRect().right),
      badges: [...t.querySelectorAll('[role="tab"] > span')].map((b) => ({
        text: (b.textContent ?? "").trim(),
        box: box(b),
        tabH: Math.round((b.closest('[role="tab"]') as Element).getBoundingClientRect().height),
        overflow: overflow(b),
      })),
    }));
    const tiles =
      screen === "now"
        ? [...document.querySelectorAll(".stat-card")].slice(0, 3).map((t) => ({
            box: box(t),
            value: box(t.querySelector(".stat-value")),
            text: (t.querySelector(".stat-value")?.textContent ?? "").trim(),
            overflow: overflow(t.querySelector(".stat-value")),
          }))
        : [];
    const search = document.querySelector('input[aria-label="Search incidents"]');
    return {
      vw: window.innerWidth,
      docOverflow: document.documentElement.scrollWidth > window.innerWidth,
      search: box(search),
      error: !!document.querySelector('[role="alert"]'),
      header: box(header),
      badge: box(badge),
      badgeText: (badge?.textContent ?? "").trim(),
      badgeOverflow: overflow(badge),
      tabs,
      tiles,
      loadMore: (document.querySelector('[data-testid="incident-load-more"]')?.textContent ?? "").trim(),
    };
  }, screen);
}

async function bring(context: BrowserContext, screen: "now" | "incidents", state: State) {
  const ctl = newCtl();
  const mode: Mode = state === "first-failed" ? "fail" : state === "refetch-failed" ? "known" : state;
  ctl.counts = mode;
  ctl.list = mode;
  const page = await openSignedIn(context, ctl, screen === "now" ? "/now" : "/incidents");
  if (state === "first-failed") {
    // Initial request plus the client's one retry.
    await expect.poll(() => ctl.countFails).toBeGreaterThanOrEqual(2);
  }
  if (state !== "first-failed") {
    await expect(page.locator(OPEN_BADGE)).toContainText(state === "unknown" ? "—" : "AI: 1234");
  }
  if (state === "refetch-failed") {
    ctl.counts = "fail";
    ctl.list = "fail";
    const before = ctl.countFails;
    await page.clock.fastForward(16_000);
    if (screen === "incidents") {
      // The list has no poll; it refetches when the window regains focus.
      await page.evaluate(() => document.dispatchEvent(new Event("visibilitychange", { bubbles: true })));
    }
    await expect.poll(() => ctl.countFails).toBeGreaterThanOrEqual(before + 2);
    await expect(page.locator(OPEN_BADGE)).toHaveAttribute("title", new RegExp(`Counted at ${hhmm(ctl.countedAt)}`));
  }
  return { page, ctl };
}

const findings: Array<Record<string, unknown>> = [];

for (const vp of VIEWPORTS) {
  for (const theme of THEMES) {
    for (const screen of ["now", "incidents"] as const) {
      test(`${screen} counts at ${vp.name} ${theme}: every state renders honestly`, async ({ browser }) => {
        test.skip(process.env.E2E_UI_FIXTURES !== "1", "deterministic intercepted UI evidence, not live collection");
        const context = await newSession(browser, vp, theme);
        const facts: Record<string, Awaited<ReturnType<typeof measure>>> = {};
        try {
          for (const state of STATES) {
            const { page, ctl } = await bring(context, screen, state);
            const badge = page.locator(OPEN_BADGE);
            const unknown = state === "unknown" || state === "first-failed";

            // Header badge.
            if (unknown) {
              await expect(page.getByRole("link", { name: "Open incidents: Count unavailable" })).toBeVisible();
              await expect(badge).toHaveText("—");
              await expect(badge).toHaveAttribute("title", "Count unavailable");
            } else {
              await expect(badge).toContainText("AI: 1234 · Webhook: 56");
              const title = await badge.getAttribute("title");
              if (state === "known") expect(title).not.toContain("Counted at");
              if (state === "stale") expect(title).toContain(`Counted at ${hhmm(ctl.staleAt)}`);
              if (state === "refetch-failed") expect(title).toContain(`Counted at ${hhmm(ctl.countedAt)}`);
            }

            if (screen === "now") {
              const tiles = page.locator(".stat-card");
              for (const [i, label] of ["Open", "Acked", "Resolved"].entries()) {
                const value = tiles.nth(i).locator(".stat-value");
                await expect(tiles.nth(i)).toContainText(label);
                if (unknown) {
                  await expect(value).toHaveText("—Count unavailable");
                  await expect(value.getByText("Count unavailable")).toHaveClass(/sr-only/);
                } else {
                  const n = [1234, 8, 3400][i]; // the active (AI) origin tab
                  await expect(value).toHaveText(n.toLocaleString("en-US"));
                  const hint = await value.getAttribute("title");
                  if (state === "known") expect(hint).toBeNull();
                  if (state === "stale") expect(hint).toBe(`Counted at ${hhmm(ctl.staleAt)}`);
                  if (state === "refetch-failed") expect(hint).toBe(`Counted at ${hhmm(ctl.countedAt)}`);
                }
              }
              const originTabs = page.getByRole("tablist", { name: /Filter the Now view by origin/ });
              if (unknown) {
                await expect(originTabs.getByRole("tab", { name: /Count unavailable/ })).toHaveCount(2);
                await expect(page.getByText("All clear — nothing open")).toHaveCount(0);
                await expect(page.getByRole("region", { name: "Open incidents" })).toHaveCount(0);
              } else {
                await expect(originTabs.getByRole("tab").first()).toContainText("1234");
                await expect(page.getByRole("region", { name: "Open incidents" })).toContainText("1234 open incidents");
              }
            } else {
              const statusTabs = page.getByRole("tablist", { name: "Filter incidents by status" });
              const originTabs = page.getByRole("tablist", { name: "Filter incidents by origin" });
              if (state === "unknown") {
                await expect(statusTabs.getByRole("tab", { name: /Count unavailable/ })).toHaveCount(4);
                await expect(originTabs.getByRole("tab", { name: /Count unavailable/ })).toHaveCount(2);
                await expect(page.getByTestId("incident-load-more").getByRole("button")).toHaveText("Load more");
              } else if (state === "first-failed") {
                await expect(page.getByText("Couldn't load incidents")).toBeVisible();
                // Nothing loaded: the tabs show "—" rather than no badge, so the
                // bars keep their size.
                await expect(statusTabs.getByRole("tab", { name: /Count unavailable/ })).toHaveCount(4);
                await expect(originTabs.getByRole("tab", { name: /Count unavailable/ })).toHaveCount(2);
              } else {
                // Four-digit badges read compact on screen ("1.2k") with the
                // full number in the accessible name and the title.
                const openTab = statusTabs.getByRole("tab", { name: "Open 1234" });
                await expect(openTab).toBeVisible();
                const badgeEl = openTab.locator(":scope > span");
                await expect(badgeEl.locator('[aria-hidden="true"]')).toHaveText("1.2k");
                await expect(statusTabs.getByRole("tab", { name: "All 4642" })).toBeVisible();
                const hint = await badgeEl.getAttribute("title");
                if (state === "known") expect(hint).toBe("1,234");
                if (state === "stale") expect(hint).toBe(`1,234 · Counted at ${hhmm(ctl.staleAt)}`);
                if (state === "refetch-failed") expect(hint).toBe(`1,234 · Counted at ${hhmm(ctl.countedAt)}`);
                if (state !== "refetch-failed") {
                  await expect(page.getByTestId("incident-load-more").getByRole("button")).toHaveText(
                    "Load more (1,242 in last 7d)",
                  );
                }
              }
              // A zero must never stand in for an unknown count.
              if (unknown) {
                for (const tab of await page.getByRole("tab").all()) {
                  expect(await tab.textContent()).not.toMatch(/\b0\b/);
                }
              }
            }
            if (unknown) expect(await badge.textContent()).not.toMatch(/\d/);

            // A failed refresh keeps the page: no error card, only the header's
            // refresh button turns into the "Couldn't refresh · Retry" notice.
            if (state === "refetch-failed") {
              await expect(page.getByRole("button", { name: "Couldn't refresh. Retry" })).toBeVisible();
              await expect(page.getByText("Couldn't load incidents")).toHaveCount(0);
              await expect(page.getByRole("alert")).toHaveCount(0);
              if (screen === "incidents") {
                await expect(page.getByRole("region", { name: /^Incidents table/ })).toBeVisible();
              }
            } else if (state !== "first-failed") {
              await expect(page.getByRole("button", { name: "Refresh now" })).toBeVisible();
            }

            facts[state] = await measure(page, screen);
            await page.screenshot({
              path: capturePath(`incident-count-${screen}`, vp.name, theme, state),
              fullPage: true,
              animations: "disabled",
            });
            await page.locator("header").screenshot({
              path: capturePath("incident-count-header", vp.name, theme, `${screen}-${state}`),
              animations: "disabled",
            });
            await page.close();
          }
        } finally {
          await context.close();
        }

        // No layout shift between states for the surfaces that hold a count.
        const base = facts.known;
        for (const state of STATES) {
          const f = facts[state];
          expect(f.header?.h, `${state}: header height`).toBe(base.header?.h);
          // Nothing runs off the right edge (four-digit counts at 390 px).
          expect(f.docOverflow, `${state}: page wider than the viewport`).toBe(false);
          for (const tl of f.tabs) {
            expect((tl.box?.x ?? 0) + (tl.box?.w ?? 0), `${state}: ${tl.label} right edge`).toBeLessThanOrEqual(f.vw);
            // Every tab fits inside its bar without scrolling, even with the
            // four-digit fixture counts at 390 px.
            expect(tl.scrolls, `${state}: ${tl.label} needs scrolling`).toBe(false);
            for (const [i, r] of tl.tabRights.entries()) {
              expect(r, `${state}: ${tl.label} tab ${i} past the bar's right edge`).toBeLessThanOrEqual(tl.barRight);
            }
          }
          // A failed or stale refresh shows the same page as a good one: the
          // tiles and the search box stay where they were.
          if (state === "refetch-failed" || state === "stale") {
            expect(f.error, `${state}: error card`).toBe(false);
            for (const [i, tile] of f.tiles.entries()) {
              expect(tile.box?.y, `${state}: tile ${i} top`).toBe(base.tiles[i].box?.y);
            }
            if (f.search && base.search) expect(f.search, `${state}: search box`).toEqual(base.search);
          }
          // A failed first load holds the same tab and search geometry as an
          // unknown count (both show "—").
          if (state === "first-failed" && f.search && facts.unknown.search) {
            expect(f.search, "first-failed: search box").toEqual(facts.unknown.search);
            for (const tl of f.tabs) {
              const ref = facts.unknown.tabs.find((t) => t.label === tl.label);
              if (ref) expect(tl.box, `first-failed: ${tl.label} box`).toEqual(ref.box);
            }
          }
          expect(f.badgeOverflow, `${state}: header badge overflow`).toBe(false);
          if (f.badge && base.badge) {
            expect(f.badge.h, `${state}: header badge height`).toBe(base.badge.h);
            expect(f.badge.y, `${state}: header badge top`).toBe(base.badge.y);
          }
          for (const [i, tile] of f.tiles.entries()) {
            expect(tile.box?.h, `${state}: tile ${i} height`).toBe(base.tiles[i].box?.h);
            expect(tile.value?.h, `${state}: tile ${i} value height`).toBe(base.tiles[i].value?.h);
            expect(tile.overflow, `${state}: tile ${i} value overflow`).toBe(false);
          }
          for (const tl of f.tabs) {
            const ref = base.tabs.find((t) => t.label === tl.label);
            if (!ref) continue;
            expect(tl.box?.h, `${state}: ${tl.label} height`).toBe(ref.box?.h);
            for (const [i, b] of tl.badges.entries()) {
              expect(b.overflow, `${state}: ${tl.label} badge ${i} overflow`).toBe(false);
              if (ref.badges[i]) expect(b.box?.h, `${state}: ${tl.label} badge ${i} height`).toBe(ref.badges[i].box?.h);
            }
          }
        }
        findings.push({ screen, viewport: vp.name, theme, facts });
      });
    }
  }
}

test("one failed refetch never flickers the counts to a dash across two polls", async ({ browser }) => {
  test.skip(process.env.E2E_UI_FIXTURES !== "1", "deterministic intercepted UI evidence, not live collection");
  const vp = VIEWPORTS[0];
  const context = await newSession(browser, vp, "dark");
  try {
    const ctl = newCtl();
    const page = await openSignedIn(context, ctl, "/now");
    const badge = page.locator(OPEN_BADGE);
    await expect(badge).toContainText("AI: 1234 · Webhook: 56");
    await expect(page.locator(".stat-card").first().locator(".stat-value")).toHaveText("1,234");

    // Record every DOM change to the header badge and the count tiles.
    await page.evaluate(() => {
      const w = window as unknown as { __countSamples: string[] };
      w.__countSamples = [];
      const sample = () => {
        const b = document.querySelector('header a[href="/incidents?status=open"]');
        const tiles = [...document.querySelectorAll(".stat-card .stat-value")].slice(0, 3);
        const tabs = document.querySelector('[role="tablist"]');
        w.__countSamples.push(
          [
            b ? (b.textContent ?? "").trim() : "<no badge>",
            ...tiles.map((t) => (t.textContent ?? "").trim()),
            (tabs?.textContent ?? "").trim(),
          ].join(" | "),
        );
      };
      sample();
      new MutationObserver(sample).observe(document.body, { subtree: true, childList: true, characterData: true });
    });

    const tileTops = () =>
      page.evaluate(() =>
        [...document.querySelectorAll(".stat-card")].slice(0, 3).map((t) => Math.round(t.getBoundingClientRect().y)),
      );
    const topsBefore = await tileTops();

    // Poll 1 fails (the request and the client's retry).
    ctl.counts = "fail";
    const reqs0 = ctl.countReqs;
    await page.clock.fastForward(15_500);
    await expect.poll(() => ctl.countFails).toBeGreaterThanOrEqual(2);
    await expect(badge).toHaveAttribute("title", new RegExp(`Counted at ${hhmm(ctl.countedAt)}`));
    await expect(badge).toContainText("AI: 1234 · Webhook: 56");
    await expect(page.locator(".stat-card").first().locator(".stat-value")).toHaveAttribute(
      "title",
      `Counted at ${hhmm(ctl.countedAt)}`,
    );
    // The failed poll moves nothing: no error card, the tiles stay put, and
    // only the refresh button changes.
    await expect(page.getByRole("button", { name: "Couldn't refresh. Retry" })).toBeVisible();
    await expect(page.getByRole("alert")).toHaveCount(0);
    expect(await tileTops(), "tile tops after a failed poll").toEqual(topsBefore);
    await page.screenshot({ path: capturePath("incident-count-poll", vp.name, "dark", "after-failed-poll"), animations: "disabled" });

    // Poll 2 succeeds; the hint goes away and the number stays.
    ctl.counts = "known";
    const okBefore = ctl.countReqs;
    await page.clock.fastForward(15_500);
    await expect.poll(() => ctl.countReqs).toBeGreaterThan(okBefore);
    await expect(badge).not.toHaveAttribute("title", /Counted at/);
    await expect(page.locator(".stat-card").first().locator(".stat-value")).not.toHaveAttribute("title", /Counted at/);
    await expect(page.getByRole("button", { name: "Refresh now" })).toBeVisible();
    expect(await tileTops(), "tile tops after the recovered poll").toEqual(topsBefore);
    await page.screenshot({ path: capturePath("incident-count-poll", vp.name, "dark", "after-recovered-poll"), animations: "disabled" });

    const samples = await page.evaluate(() => (window as unknown as { __countSamples: string[] }).__countSamples);
    expect(ctl.countReqs - reqs0, "requests across the two polls").toBeGreaterThanOrEqual(3);
    for (const s of samples) {
      expect(s, "a sample showed a dash, a missing badge or a zero").not.toMatch(/—|Count unavailable|<no badge>|AI: 0/);
    }
    fs.mkdirSync(harnessResultsDir(), { recursive: true });
    fs.writeFileSync(
      path.join(harnessResultsDir(), "count-poll-samples.json"),
      JSON.stringify({ requests: ctl.countReqs - reqs0, failures: ctl.countFails, distinct: [...new Set(samples)], total: samples.length }, null, 2),
    );
  } finally {
    await context.close();
  }
});

// The "Couldn't refresh" notice: the amber icon keeps 3:1 against the header
// in both themes (WCAG 1.4.11, non-text contrast), the subtitle note truncates
// inside the title row at mid widths instead of wrapping or pushing the
// controls, and the header keeps its height.
const NOTE_WIDTHS = [
  { name: "1440", width: 1440, height: 900 },
  { name: "1024", width: 1024, height: 768 },
  { name: "680", width: 680, height: 800 },
  { name: "390", width: 390, height: 844 },
] as const;
const noticeFacts: Array<Record<string, unknown>> = [];

for (const theme of THEMES) {
  for (const screen of ["now", "incidents"] as const) {
    test(`${screen} refresh notice in ${theme}: contrast, truncation, no shift`, async ({ browser }) => {
      test.skip(process.env.E2E_UI_FIXTURES !== "1", "deterministic intercepted UI evidence, not live collection");
      for (const vp of NOTE_WIDTHS) {
        const context = await newSession(browser, vp as unknown as (typeof VIEWPORTS)[number], theme);
        try {
          const header = async (page: Page) =>
            page.evaluate(() => {
              const h = document.querySelector("header") as HTMLElement;
              const sub = h.querySelector("h1")?.nextElementSibling as HTMLElement | null;
              const btn = h.querySelector('button[aria-label="Couldn\'t refresh. Retry"], button[aria-label="Refresh now"]') as HTMLElement | null;
              const r = (el: Element | null) => {
                if (!el) return null;
                const b = el.getBoundingClientRect();
                return { x: Math.round(b.x), y: Math.round(b.y), w: Math.round(b.width), h: Math.round(b.height) };
              };
              const parse = (c: string) => {
                const m = c.match(/[\d.]+/g)!.map(Number);
                return { r: m[0], g: m[1], b: m[2], a: m.length > 3 ? m[3] : 1 };
              };
              // Composite translucent backgrounds up to the first opaque one.
              const bgOf = (el: HTMLElement | null) => {
                const layers: Array<ReturnType<typeof parse>> = [];
                for (let e: HTMLElement | null = el; e; e = e.parentElement) {
                  const c = parse(getComputedStyle(e).backgroundColor);
                  if (c.a > 0) layers.push(c);
                  if (c.a >= 1) break;
                }
                let out = { r: 255, g: 255, b: 255 };
                for (const l of layers.reverse()) {
                  out = { r: l.r * l.a + out.r * (1 - l.a), g: l.g * l.a + out.g * (1 - l.a), b: l.b * l.a + out.b * (1 - l.a) };
                }
                return out;
              };
              const lum = (c: { r: number; g: number; b: number }) => {
                const f = (v: number) => {
                  const s = v / 255;
                  return s <= 0.03928 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
                };
                return 0.2126 * f(c.r) + 0.7152 * f(c.g) + 0.0722 * f(c.b);
              };
              const fg = btn ? parse(getComputedStyle(btn).color) : null;
              const bg = bgOf(btn);
              const ratio = fg ? (Math.max(lum(fg), lum(bg)) + 0.05) / (Math.min(lum(fg), lum(bg)) + 0.05) : 0;
              return {
                header: r(h),
                button: r(btn),
                buttonLabel: btn?.getAttribute("aria-label") ?? null,
                contrast: Math.round(ratio * 100) / 100,
                fg: fg && `rgb(${fg.r},${fg.g},${fg.b})`,
                bg: `rgb(${Math.round(bg.r)},${Math.round(bg.g)},${Math.round(bg.b)})`,
                note: sub
                  ? {
                      text: (sub.textContent ?? "").trim(),
                      visible: getComputedStyle(sub).display !== "none",
                      box: r(sub),
                      truncated: sub.scrollWidth > sub.clientWidth + 1,
                      ellipsis: getComputedStyle(sub).textOverflow,
                      whiteSpace: getComputedStyle(sub).whiteSpace,
                    }
                  : null,
                docOverflow: document.documentElement.scrollWidth > window.innerWidth,
                search: r(document.querySelector('input[aria-label="Search incidents"]')),
              };
            });

          const good = await bring(context, screen, "known");
          const goodFacts = await header(good.page);
          await good.page.close();
          const bad = await bring(context, screen, "refetch-failed");
          const notice = bad.page.getByRole("button", { name: "Couldn't refresh. Retry" });
          await expect(notice).toBeVisible();
          await expect(notice).toHaveAttribute("title", "Couldn't refresh · showing the last loaded data. Retry");
          const badFacts = await header(bad.page);
          await bad.page.locator("header").screenshot({
            path: capturePath(`incident-count-notice-${screen}`, vp.name, theme, "refetch-failed"),
            animations: "disabled",
          });
          if (vp.name === "680" || vp.name === "1024") {
            await bad.page.screenshot({
              path: capturePath(`incident-count-${screen}`, vp.name, theme, "refetch-failed"),
              animations: "disabled",
            });
          }
          await bad.page.close();

          noticeFacts.push({ screen, theme, viewport: vp.name, good: goodFacts, failed: badFacts });
          expect(badFacts.contrast, `${vp.name}: amber icon contrast ${badFacts.fg} on ${badFacts.bg}`).toBeGreaterThanOrEqual(3);
          expect(badFacts.header?.h, `${vp.name}: header height`).toBe(goodFacts.header?.h);
          expect(badFacts.button, `${vp.name}: refresh button box`).toEqual(goodFacts.button);
          expect(badFacts.docOverflow, `${vp.name}: page wider than the viewport`).toBe(false);
          if (screen === "incidents") {
            expect(badFacts.search, `${vp.name}: search box measured`).not.toBeNull();
            expect(badFacts.search, `${vp.name}: search box`).toEqual(goodFacts.search);
          }
          const note = badFacts.note;
          if (note?.visible) {
            expect(note.text).toBe("Couldn't refresh · showing the last loaded data");
            expect(note.whiteSpace, `${vp.name}: note wraps`).toBe("nowrap");
            if (note.truncated) expect(note.ellipsis, `${vp.name}: note cut without ellipsis`).toBe("ellipsis");
            // The note stays left of the controls.
            const sameRow = note.box && badFacts.button && badFacts.button.y < note.box.y + note.box.h && note.box.y < badFacts.button.y + badFacts.button.h;
            if (sameRow && note.box && badFacts.button) {
              expect(note.box.x + note.box.w, `${vp.name}: note runs under the controls`).toBeLessThanOrEqual(badFacts.button.x);
            }
          }
        } finally {
          await context.close();
        }
      }
    });
  }
}

test.afterAll(() => {
  if (noticeFacts.length > 0) {
    fs.mkdirSync(harnessResultsDir(), { recursive: true });
    fs.writeFileSync(path.join(harnessResultsDir(), "refresh-notice-facts.json"), JSON.stringify(noticeFacts, null, 2));
  }
  if (findings.length === 0) return;
  fs.mkdirSync(harnessResultsDir(), { recursive: true });
  fs.writeFileSync(path.join(harnessResultsDir(), "count-layout-facts.json"), JSON.stringify(findings, null, 2));
});
