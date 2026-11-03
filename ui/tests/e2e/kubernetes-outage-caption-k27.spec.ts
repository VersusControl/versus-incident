import { expect, test, type Page } from "@playwright/test";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { openApp } from "./helpers";
import { capturePath } from "./support/capture";

// K27-B4 / plan item F5 / W1 caption delta, deliberately narrower than the
// preserved 600 s retention proof in kubernetes-persisted-history-k27.spec.ts.
// It asserts only the repaired top-bar caption during a SHORT real outage of
// the isolated project-owned kind connector path, plus retained rows and the
// replacement of the caption on recovery without a document reload.
//
// Requires the isolated licensed project + kind network fixture:
//   E2E_K27_CAPTION=1 HARNESS_PROJECT=k27-licensed-... 
const execute = promisify(execFile);

const CAPTION = '[data-sync-state]';
const UNREACHABLE = "Cluster unreachable — showing data from ";

type CaptionLayout = {
  text: string;
  state: string | null;
  viewport: number;
  textLeft: number;
  textRight: number;
  overflowX: number;
  overlaps: string[];
  clippedBy: string[];
};

/** Geometry of the rendered caption text, not just its container box. */
async function captionLayout(page: Page): Promise<CaptionLayout> {
  return page.evaluate(() => {
    const el = document.querySelector<HTMLElement>("[data-sync-state]");
    if (!el) throw new Error("sync state caption is missing");
    const range = document.createRange();
    range.selectNodeContents(el);
    const text = range.getBoundingClientRect();
    const overlaps: string[] = [];
    const siblings = document.querySelectorAll<HTMLElement>(
      '[data-testid="topbar-actions"] button, [data-testid="topbar-actions"] select, [data-testid="topbar-actions"] a, [data-testid="topbar-identity"]',
    );
    for (const node of siblings) {
      if (node === el || el.contains(node) || node.contains(el)) continue;
      const box = node.getBoundingClientRect();
      if (box.width === 0 || box.height === 0) continue;
      const w = Math.min(text.right, box.right) - Math.max(text.left, box.left);
      const h = Math.min(text.bottom, box.bottom) - Math.max(text.top, box.top);
      if (w > 2 && h > 2) {
        overlaps.push((node.getAttribute("aria-label") ?? node.textContent ?? node.tagName).trim().slice(0, 40));
      }
    }
    const clippedBy: string[] = [];
    for (let node = el.parentElement; node && node !== document.body; node = node.parentElement) {
      const style = getComputedStyle(node);
      if (!/hidden|clip|auto|scroll/.test(`${style.overflowX} ${style.overflowY}`)) continue;
      const box = node.getBoundingClientRect();
      if (text.right > box.right + 1 || text.left < box.left - 1) {
        clippedBy.push(`${node.tagName.toLowerCase()}.${String(node.className).slice(0, 30)}`);
      }
    }
    return {
      text: (el.textContent ?? "").trim(),
      state: el.getAttribute("data-sync-state"),
      viewport: innerWidth,
      textLeft: text.left,
      textRight: text.right,
      overflowX: document.documentElement.scrollWidth - innerWidth,
      overlaps,
      clippedBy,
    };
  });
}

function captionTime(text: string): string {
  return text.includes(UNREACHABLE) ? text.slice(text.indexOf(UNREACHABLE) + UNREACHABLE.length).trim() : "";
}

async function setTheme(page: Page, theme: "light" | "dark") {
  const applied = await page.evaluate(() => document.documentElement.dataset.theme ?? "dark");
  if (applied === theme) return;
  await page.getByRole("button", { name: theme === "light" ? "Switch to light theme" : "Switch to dark theme" }).click();
  await expect.poll(() => page.evaluate(() => document.documentElement.dataset.theme ?? "dark")).toBe(theme);
}

async function watchHistorySkeletons(page: Page) {
  await page.evaluate(() => {
    document.documentElement.dataset.k27Skeletons = "0";
    new MutationObserver(() => {
      if (document.querySelector('[aria-label="Loading projected change history"], [aria-label="Loading related root change history"], [aria-label="Loading break timeline"]')) {
        document.documentElement.dataset.k27Skeletons = String(Number(document.documentElement.dataset.k27Skeletons) + 1);
      }
    }).observe(document.body, { childList: true, subtree: true });
  });
}

async function capture(page: Page, rows: ReturnType<Page["getByRole"]>, width: number, theme: "light" | "dark", state: string) {
  await page.setViewportSize({ width, height: width === 390 ? 844 : 1000 });
  await page.emulateMedia({ colorScheme: theme });
  await setTheme(page, theme);
  await expect(rows.first(), "retained break rows must stay rendered in the capture").toBeVisible();
  const layout = await captionLayout(page);
  expect(layout.overflowX, `${state}: the page must not overflow horizontally`).toBeLessThanOrEqual(0);
  expect(layout.textLeft, `${state}: caption must start inside the viewport`).toBeGreaterThanOrEqual(-1);
  expect(layout.textRight, `${state}: caption text must not run past the viewport — "${layout.text}"`).toBeLessThanOrEqual(layout.viewport + 1);
  expect(layout.overlaps, `${state}: caption must not overlap top-bar controls`).toEqual([]);
  expect(layout.clippedBy, `${state}: caption must not be clipped by an ancestor`).toEqual([]);
  await page.screenshot({ path: capturePath("k27-caption", width, theme, state), animations: "disabled" });
  return layout;
}

test("K27 real short outage captions the actual last-good time (K27-B4/F5/W1)", async ({ page }, testInfo) => {
  test.skip(process.env.E2E_K27_CAPTION !== "1", "requires the isolated project-owned kind network fixture");
  test.setTimeout(900_000);
  const project = process.env.HARNESS_PROJECT ?? "";
  expect(project.startsWith("k27-licensed-"), "never disconnect a shared harness network").toBe(true);
  const network = `${project}_default`;
  const node = `${process.env.E2E_KIND_CLUSTER ?? "versus-harness"}-control-plane`;

  await page.setViewportSize({ width: 1440, height: 1000 });
  await page.emulateMedia({ colorScheme: "dark" });
  await page.addInitScript(() => localStorage.setItem("versus.theme", "dark"));

  const syncReads: Array<{ status: number; state?: string; failure?: string; at: number }> = [];
  const observed: Array<{ at: number; observed_at: string; cluster_id?: string }> = [];
  const watch = (surface: Page) => surface.on("response", (response) => {
    const path = new URL(response.url()).pathname;
    if (path.endsWith("/api/admin/kubernetes/sync")) {
      const entry: { status: number; state?: string; failure?: string; at: number } = { status: response.status(), at: Date.now() };
      syncReads.push(entry);
      if (response.ok()) {
        void response.json()
          .then((body: { state?: string; class?: string }) => { entry.state = body?.state; entry.failure = body?.class; })
          .catch(() => undefined);
      }
    }
    if (path.endsWith("/api/admin/kubernetes/overview") && response.ok()) {
      void response.json()
        .then((body: { observed_at?: string; cluster_id?: string }) => {
          if (body?.observed_at) observed.push({ at: Date.now(), observed_at: body.observed_at, cluster_id: body.cluster_id });
        })
        .catch(() => undefined);
    }
  });
  watch(page);

  let disconnected = false;
  try {
    await openApp(page, "/agent/kubernetes?view=timeline");
    // Licensed tier proof: this must be the real Enterprise build with a local
    // admin session, not community mode. The session route is checked after
    // sign-in because the browser context holds the verified cookie then.
    const deploymentResponse = await page.request.get("/enterprise/api/sso/deployment");
    expect(deploymentResponse.status(), "the Enterprise license gate must actually be ON").toBe(200);
    const deployment = await deploymentResponse.json();
    const sessionResponse = await page.request.get(`/enterprise/api/sso/${encodeURIComponent(deployment.org)}/session`);
    expect(sessionResponse.status()).toBe(200);
    expect((await sessionResponse.json()).local).toBe(true);
    expect((await page.context().cookies()).some(cookie => cookie.name === "versus_enterprise_session" && cookie.httpOnly)).toBe(true);
    await page.getByRole("tab", { name: "Breaks only", exact: true }).click();
    const rows = page.getByRole("link", { name: "Open break details", exact: true });
    await expect(rows.first()).toBeVisible({ timeout: 60_000 });
    const baselineRows = await rows.count();
    expect(baselineRows, "the fixture must hold retained break rows before the outage").toBeGreaterThan(0);
    await expect(page.locator(CAPTION)).toContainText(/Last synced|Updates disconnected/, { timeout: 60_000 });
    await expect.poll(() => observed.length, { timeout: 60_000 }).toBeGreaterThan(0);
    await watchHistorySkeletons(page);
    await page.evaluate(() => { document.documentElement.dataset.k27Doc = "k27-caption"; });

    // The same browser context, a second surface: the Overview tile header
    // states the observed_at of the data actually on screen, so the caption can
    // be checked against what the page itself renders, not a re-fetch.
    const overviewPage = await page.context().newPage();
    watch(overviewPage);
    await overviewPage.setViewportSize({ width: 1440, height: 1000 });
    await overviewPage.emulateMedia({ colorScheme: "dark" });
    await overviewPage.addInitScript(() => localStorage.setItem("versus.theme", "dark"));
    await openApp(overviewPage, "/agent/kubernetes");
    await expect(overviewPage.locator(CAPTION)).toContainText(/Last synced|Updates disconnected/, { timeout: 60_000 });
    const snapshotLine = overviewPage.getByText(/^Snapshot \d{1,2}:\d{2}:\d{2}$/).first();
    await expect(snapshotLine, "the Overview view must state its own snapshot time").toBeVisible({ timeout: 60_000 });
    const snapshotBefore = (await snapshotLine.textContent()) ?? "";
    await watchHistorySkeletons(overviewPage);
    const observedBefore = [...observed];

    const failureStarted = Date.now();
    await execute("docker", ["network", "disconnect", network, node]);
    disconnected = true;

    await expect(page.locator(CAPTION), "the repaired caption must name the outage").toContainText(UNREACHABLE, { timeout: 120_000 });
    const firstText = (await page.locator(CAPTION).textContent()) ?? "";
    const shownTime = captionTime(firstText);
    expect(shownTime, "caption must carry an absolute time").toMatch(/\d{1,2}:\d{2}/);
    const expectedTimes = await page.evaluate(
      list => [...new Set(list.map(iso => new Date(iso).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })))],
      observedBefore.map(entry => entry.observed_at),
    );
    expect(expectedTimes, "the fixture must have a real last-good observation").not.toHaveLength(0);
    expect(expectedTimes, `caption time ${shownTime} must be an actual last-good observation time, not a synthesized one`).toContain(shownTime);
    expect(firstText, "the top bar must not keep a stale Syncing label").not.toMatch(/Syncing/);
    await expect(page.getByTestId("topbar-actions").getByText(/^Syncing$/), "no Syncing text may remain in the top bar").toHaveCount(0);
    await expect(page.locator('[data-sync-strip="failed"]')).toBeVisible();
    await expect(page.locator('[data-sync-strip="failed"]')).toContainText("Cluster sync failed");
    expect(await rows.count(), "retained rows must stay rendered").toBeGreaterThanOrEqual(baselineRows);
    expect(await page.evaluate(() => document.documentElement.dataset.k27Skeletons)).toBe("0");

    await expect(overviewPage.locator(CAPTION), "the Overview surface must carry the same outage caption").toContainText(UNREACHABLE, { timeout: 120_000 });
    await expect(overviewPage.locator(CAPTION)).not.toContainText(/Syncing/);
    const snapshotDuring = (await snapshotLine.textContent()) ?? "";
    const snapshotTime = snapshotDuring.replace("Snapshot ", "").trim();
    const snapshotAsCaption = await overviewPage.evaluate((time) => {
      const [hour, minute] = time.split(":").map(Number);
      const clock = new Date();
      clock.setHours(hour, minute, 0, 0);
      return clock.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
    }, snapshotTime);
    expect(snapshotTime, "the Overview view must keep stating its snapshot time during the outage").toMatch(/\d{1,2}:\d{2}:\d{2}/);
    const overviewCaption = (await overviewPage.locator(CAPTION).textContent()) ?? "";
    expect(overviewCaption, "the Overview surface's top bar must name its outage too").toContain(UNREACHABLE);
    expect(
      snapshotAsCaption,
      `the Overview caption (${captionTime(overviewCaption)}) must match the Overview data actually on screen (${snapshotDuring})`,
    ).toBe(captionTime(overviewCaption));
    await overviewPage.screenshot({ path: capturePath("k27-caption-overview", 1440, "dark", "outage-live"), animations: "disabled" });

    // Let the outage age past a whole minute so "not the current clock" is a
    // real assertion rather than a same-minute coincidence.
    await page.waitForTimeout(60_000);
    const clock = await page.evaluate(() => new Date().toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }));
    const aged = (await page.locator(CAPTION).textContent()) ?? "";
    expect(captionTime(aged), "the caption must still name the last-good time").toBe(shownTime);
    expect(captionTime(aged), `caption must not be the current clock (${clock})`).not.toBe(clock);
    const lastGoodAge = await page.evaluate(iso => Date.now() - Date.parse(iso), observedBefore[observedBefore.length - 1].observed_at);
    expect(lastGoodAge, "the outage must really be older than the shown last-good data").toBeGreaterThan(60_000);
    await expect(page.locator(CAPTION)).not.toContainText(/Syncing/);
    await expect(page.getByTestId("topbar-actions").getByText(/^Syncing$/)).toHaveCount(0);

    await capture(page, rows, 1440, "dark", "outage-live");
    await capture(page, rows, 1440, "light", "outage-live");
    await capture(page, rows, 390, "dark", "outage-live");
    await capture(page, rows, 390, "light", "outage-live");
    // Restore the desktop dark state for the recovery proof, without a reload.
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.emulateMedia({ colorScheme: "dark" });
    await setTheme(page, "dark");

    // Keep the outage real for a few minutes while sampling the caption and
    // retained rows; no clock mocking anywhere in this test.
    const samples: Array<{ elapsed_ms: number; text: string; state: string | null; rows: number; skeletons: string | null }> = [];
    const violations: string[] = [];
    while (Date.now() - failureStarted < 180_000) {
      await page.waitForTimeout(15_000);
      const layout = await captionLayout(page);
      const sample = {
        elapsed_ms: Date.now() - failureStarted,
        text: layout.text,
        state: layout.state,
        rows: await rows.count(),
        skeletons: await page.evaluate(() => document.documentElement.dataset.k27Skeletons ?? null),
      };
      samples.push(sample);
      if (!sample.text.includes(UNREACHABLE) || captionTime(sample.text) !== shownTime) violations.push(`caption changed at ${sample.elapsed_ms}ms: ${sample.text}`);
      if (/Syncing/.test(sample.text)) violations.push(`Syncing returned at ${sample.elapsed_ms}ms`);
      if (sample.rows < baselineRows) violations.push(`rows dropped to ${sample.rows} at ${sample.elapsed_ms}ms`);
      if (sample.skeletons !== "0") violations.push(`skeleton re-entry ${sample.skeletons} at ${sample.elapsed_ms}ms`);
    }
    expect(violations, "caption/rows must hold for the whole real outage").toEqual([]);
    expect(
      syncReads.some(entry => entry.status >= 500 || entry.state === "error"),
      "the outage must be real: a sync-health read failed upstream",
    ).toBe(true);

    await execute("docker", ["network", "connect", network, node]);
    disconnected = false;
    await expect(page.locator(CAPTION), "recovery must replace the outage caption").not.toContainText(UNREACHABLE, { timeout: 180_000 });
    await expect(page.locator(CAPTION)).toContainText(/Last synced|Updates disconnected/, { timeout: 120_000 });
    await expect(page.locator(CAPTION)).not.toContainText(/Syncing/);
    await expect(rows.first(), "retained rows must survive recovery").toBeVisible();
    expect(await page.evaluate(() => document.documentElement.dataset.k27Doc), "recovery must not reload the document").toBe("k27-caption");
    await capture(page, rows, 1440, "dark", "recovered-live");
    await expect(overviewPage.locator(CAPTION), "the Overview surface must recover without a reload").not.toContainText(UNREACHABLE, { timeout: 180_000 });
    await expect(overviewPage.locator(CAPTION)).toContainText(/Last synced|Updates disconnected/, { timeout: 120_000 });

    await testInfo.attach("real-short-outage-caption", {
      body: JSON.stringify({
        clock: "real",
        outage_ms: Date.now() - failureStarted,
        network_disconnected: network,
        last_good_observed_at: observedBefore[observedBefore.length - 1]?.observed_at,
        overview_observations: observedBefore,
        overview_snapshot_before: snapshotBefore,
        overview_snapshot_during: snapshotDuring,
        caption_text: aged,
        caption_time: shownTime,
        current_clock_at_check: clock,
        outage_samples: samples,
        sync_reads: syncReads,
        rows_baseline: baselineRows,
        document_reloaded: false,
      }),
      contentType: "application/json",
    });
  } finally {
    if (disconnected) await execute("docker", ["network", "connect", network, node]).catch(() => undefined);
  }
});
