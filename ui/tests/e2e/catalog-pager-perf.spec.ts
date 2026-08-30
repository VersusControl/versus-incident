import { test, expect, type Response } from "@playwright/test";
import * as path from "node:path";
import { fileURLToPath } from "node:url";
import { openApp } from "./helpers";

// ---------------------------------------------------------------------------
// Agent catalog server-side pagination browser e2e. Drives the REAL running OSS
// SPA (versus-incident) against a run-harness Postgres holding a large learned
// catalog (>1 server page of patterns AND services), like an operator:
//
//   1. the Patterns / Services page loads a bounded first server page (not the
//      whole catalog) yet the header + "Load more (N total)" show the TRUE
//      whole-set total, not the loaded count;
//   2. "Load more" fetches the next page ON DEMAND via offset (not a
//      whole-table pull), appends rows, and the offset advances with no dupes;
//   3. click-to-sort reorders the LOADED rows;
//   4. no console / page errors surface on a large catalog.
//
// Bring the stack up first (cd run && ./oss.sh with STORAGE_TYPE=postgres), seed
// >1000 patterns + >1000 services, and point E2E_BASE_URL / E2E_GATEWAY_SECRET
// at it (tests/e2e/.env, run-harness defaults).
// ---------------------------------------------------------------------------

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const SHOTS = path.resolve(__dirname, "screenshots", "catalog-pager");

// The server's default page size for the catalog list endpoints — the first
// page is bounded to this, never the whole catalog.
const SERVER_PAGE = 1000;

type PatternsBody = {
  patterns: { id: string }[];
  total: number;
  offset: number;
  page_size: number;
  next_offset: number | null;
};

type ServicesBody = {
  services: Record<string, unknown>;
  total: number;
  offset: number;
  page_size: number;
  next_offset: number | null;
};

// A first index-page response (no offset param) — the initial load.
function isFirstPage(apiPath: string) {
  return (r: Response): boolean => {
    const u = r.url();
    return (
      r.request().method() === "GET" &&
      u.includes(apiPath) &&
      !u.includes("offset=") &&
      r.status() === 200
    );
  };
}

// A load-more response — carries the offset continuation cursor.
function isLoadMorePage(apiPath: string) {
  return (r: Response): boolean => {
    const u = r.url();
    return (
      r.request().method() === "GET" &&
      u.includes(apiPath) &&
      u.includes("offset=") &&
      r.status() === 200
    );
  };
}

// Collects console.error + uncaught page errors so a case can assert the large
// catalog renders clean. Resource 404s (e.g. favicon) surface as separate
// requestfailed events, not console errors, so they do not pollute this.
// Collects genuine JS errors: uncaught page exceptions and console.error
// messages. Chromium logs every failed HTTP resource load ("Failed to load
// resource: … 404") as a console error; in the OSS SPA the shell deliberately
// probes the Enterprise-only /api/agent/baselines endpoint (Sidebar), which is
// 404 in the OSS binary and caught in a try/catch to lock the Metrics/Traces
// nav — pre-existing, pager-unrelated network noise. Filter that generic
// resource-load line so the assertion still bites on real script errors.
function trackErrors(page: import("@playwright/test").Page): string[] {
  const errors: string[] = [];
  page.on("console", (m) => {
    if (m.type() !== "error") return;
    const text = m.text();
    if (/Failed to load resource/i.test(text)) return;
    errors.push(text);
  });
  page.on("pageerror", (e) => errors.push(String(e)));
  return errors;
}

test("patterns list: whole-set total, load-more appends next page, sort works, no console errors", async ({
  page,
}) => {
  const errors = trackErrors(page);

  const firstPageResp = page.waitForResponse(isFirstPage("/api/agent/patterns"), {
    timeout: 30_000,
  });
  await openApp(page, "/agent/logs");

  // 1) Bounded first server page, yet the payload reports the true whole-set
  //    total (strictly larger) — proof the endpoint did not ship the whole
  //    catalog to render the first screen.
  const first = (await (await firstPageResp).json()) as PatternsBody;
  expect(first.patterns.length).toBeLessThanOrEqual(SERVER_PAGE);
  expect(first.total).toBeGreaterThan(first.patterns.length);
  expect(first.offset).toBe(0);
  expect(first.next_offset).toBe(first.patterns.length);
  const total = first.total;

  // 2) The header shows the TRUE whole-set count, not the loaded count.
  await expect(
    page.getByText(`${total.toLocaleString()} log templates learned`),
  ).toBeVisible({ timeout: 15_000 });

  // 3) The "Load more (N total)" control renders the TRUE total.
  const loadMore = page.getByTestId("pattern-load-more");
  await expect(loadMore).toBeVisible({ timeout: 15_000 });
  await expect(loadMore).toContainText(total.toLocaleString());

  // Capture the top rendered row id before appending / sorting.
  const topBtn = () =>
    page
      .locator("table.ddt tbody tr")
      .first()
      .getByRole("button", { name: /View pattern/ });
  const topBefore = await topBtn().getAttribute("aria-label");

  await page.screenshot({
    path: path.join(SHOTS, "01-patterns-first-page.png"),
    fullPage: true,
  });

  // 4) Clicking "Load more" fetches the NEXT page on demand via offset (not a
  //    whole-table pull), the offset advances, and the last (underfull) page
  //    closes the cursor.
  const loadMoreResp = page.waitForResponse(isLoadMorePage("/api/agent/patterns"), {
    timeout: 30_000,
  });
  await loadMore.getByRole("button", { name: /Load more/i }).click();
  const more = (await (await loadMoreResp).json()) as PatternsBody;
  expect(more.offset).toBe(SERVER_PAGE);
  expect(more.patterns.length).toBeGreaterThan(0);
  expect(more.total).toBe(total);
  expect(more.next_offset).toBeNull();

  // Whole catalog now loaded ⇒ the load-more control retires.
  await expect(page.getByTestId("pattern-load-more")).toHaveCount(0, {
    timeout: 15_000,
  });

  await page.screenshot({
    path: path.join(SHOTS, "02-patterns-after-load-more.png"),
    fullPage: true,
  });

  // 5) Click-to-sort reorders the LOADED rows. Count sorts descending on the
  //    first click (already the incoming order), ascending on the second — so
  //    two clicks flip the top row to the lowest loaded count.
  const countHeader = page.getByRole("columnheader", { name: /Count/ });
  const countSort = countHeader.getByRole("button").first();
  await countSort.click();
  await countSort.click();
  await expect(countHeader).toHaveAttribute("aria-sort", "ascending");
  const topAfter = await topBtn().getAttribute("aria-label");
  expect(topAfter).not.toBe(topBefore);

  await page.screenshot({
    path: path.join(SHOTS, "03-patterns-sorted-asc.png"),
    fullPage: true,
  });

  expect(errors, `console/page errors: ${errors.join("\n")}`).toEqual([]);
});

test("services list: whole-set total, load-more appends next page, sort works, no console errors", async ({
  page,
}) => {
  const errors = trackErrors(page);

  const firstPageResp = page.waitForResponse(isFirstPage("/api/agent/services"), {
    timeout: 30_000,
  });
  await openApp(page, "/agent/services");

  const first = (await (await firstPageResp).json()) as ServicesBody;
  const loadedCount = Object.keys(first.services).length;
  expect(loadedCount).toBeLessThanOrEqual(SERVER_PAGE);
  expect(first.total).toBeGreaterThan(loadedCount);
  expect(first.offset).toBe(0);
  expect(first.next_offset).toBe(loadedCount);
  const total = first.total;

  // Header total is the whole estate, not the loaded count.
  await expect(
    page.getByText(`${total.toLocaleString()} discovered`),
  ).toBeVisible({ timeout: 15_000 });

  const loadMore = page.getByTestId("service-load-more");
  await expect(loadMore).toBeVisible({ timeout: 15_000 });
  await expect(loadMore).toContainText(total.toLocaleString());

  const topBtn = () =>
    page
      .locator("table.ddt tbody tr")
      .first()
      .getByRole("button", { name: /View service/ });
  const topBefore = await topBtn().getAttribute("aria-label");

  await page.screenshot({
    path: path.join(SHOTS, "04-services-first-page.png"),
    fullPage: true,
  });

  const loadMoreResp = page.waitForResponse(isLoadMorePage("/api/agent/services"), {
    timeout: 30_000,
  });
  await loadMore.getByRole("button", { name: /Load more/i }).click();
  const more = (await (await loadMoreResp).json()) as ServicesBody;
  expect(more.offset).toBe(SERVER_PAGE);
  expect(Object.keys(more.services).length).toBeGreaterThan(0);
  expect(more.total).toBe(total);
  expect(more.next_offset).toBeNull();

  await expect(page.getByTestId("service-load-more")).toHaveCount(0, {
    timeout: 15_000,
  });

  await page.screenshot({
    path: path.join(SHOTS, "05-services-after-load-more.png"),
    fullPage: true,
  });

  // First-seen sort reorders loaded rows: incoming order is first_seen asc, so
  // one descending click flips the top row.
  const seenHeader = page.getByRole("columnheader", { name: /First seen/ });
  const seenSort = seenHeader.getByRole("button").first();
  await seenSort.click();
  await expect(seenHeader).not.toHaveAttribute("aria-sort", "none");
  let topAfter = await topBtn().getAttribute("aria-label");
  if (topAfter === topBefore) {
    await seenSort.click();
    topAfter = await topBtn().getAttribute("aria-label");
  }
  expect(topAfter).not.toBe(topBefore);

  await page.screenshot({
    path: path.join(SHOTS, "06-services-sorted.png"),
    fullPage: true,
  });

  expect(errors, `console/page errors: ${errors.join("\n")}`).toEqual([]);
});
