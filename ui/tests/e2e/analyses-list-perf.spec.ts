import { test, expect, type Response } from "@playwright/test";
import * as path from "node:path";
import { fileURLToPath } from "node:url";
import { openApp } from "./helpers";

// ---------------------------------------------------------------------------
// Analyses list-perf browser e2e. Drives the REAL running OSS SPA
// (versus-incident) against the run-harness Postgres seeded with ~3k analyses,
// like an operator:
//
//   1. the Analyses page loads fast + bounded (a bounded first page, not all
//      3k rows) and the "Load more (N total)" control shows the TRUE whole-set
//      total, not the loaded count;
//   2. "Load more" fetches the next page ON DEMAND via offset (not a
//      whole-table pull) and appends rows.
//
// Bring the stack up first (cd run && ./oss.sh), seed ~3k vs_analyses rows, and
// point E2E_BASE_URL / E2E_GATEWAY_SECRET at it (tests/e2e/.env, run-harness
// defaults).
// ---------------------------------------------------------------------------

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const SHOTS = path.resolve(__dirname, "screenshots", "analyses-perf");

// The server's default page size for the analyses index endpoint — the first
// page is bounded to this, never the whole table.
const SERVER_PAGE = 1000;

// Shape of the analyses index endpoint's JSON (only the asserted fields).
type AnalysesBody = {
  analyses: unknown[];
  total: number;
  offset: number;
  page: number;
  page_size: number;
  next_offset: number | null;
};

// A first analyses index-page response (no offset param) — the initial load.
function isFirstPage(r: Response): boolean {
  const u = r.url();
  return (
    r.request().method() === "GET" &&
    u.includes("/api/admin/analyses") &&
    !u.includes("offset=") &&
    r.status() === 200
  );
}

// A load-more analyses response — carries the offset continuation cursor.
function isLoadMorePage(r: Response): boolean {
  const u = r.url();
  return (
    r.request().method() === "GET" &&
    u.includes("/api/admin/analyses") &&
    u.includes("offset=") &&
    r.status() === 200
  );
}

test("analyses list loads bounded, shows true total, and loads more on demand", async ({
  page,
}) => {
  // Capture the first-page response BEFORE navigating so we never miss it.
  const firstPageResp = page.waitForResponse(isFirstPage, { timeout: 30_000 });

  await openApp(page, "/analyses");

  // 1) The first page is BOUNDED (<= server page size) yet the payload reports
  //    the true whole-set total, which is strictly larger — proof the endpoint
  //    did not ship the whole table to render the first screen.
  const first = (await (await firstPageResp).json()) as AnalysesBody;
  expect(first.page_size).toBeLessThanOrEqual(SERVER_PAGE);
  expect(first.analyses.length).toBeLessThanOrEqual(SERVER_PAGE);
  expect(first.total).toBeGreaterThan(first.analyses.length);
  expect(first.offset).toBe(0);
  expect(first.next_offset).toBe(first.analyses.length);

  const total = first.total;

  // 2) The "Load more (N total)" control renders the TRUE total, not the number
  //    of rows currently loaded.
  const loadMore = page.getByTestId("analysis-load-more");
  await expect(loadMore).toBeVisible({ timeout: 15_000 });
  await expect(loadMore).toContainText(total.toLocaleString());
  await expect(loadMore).toContainText(/Load more/i);

  await page.screenshot({
    path: path.join(SHOTS, "01-analyses-first-page.png"),
    fullPage: true,
  });

  // 3) Clicking "Load more" fetches the NEXT page on demand via offset (not a
  //    whole-table pull) and appends rows.
  const loadMoreResp = page.waitForResponse(isLoadMorePage, { timeout: 30_000 });
  await loadMore.getByRole("button", { name: /Load more/i }).click();
  const next = (await (await loadMoreResp).json()) as AnalysesBody;
  expect(next.offset).toBe(SERVER_PAGE);
  expect(next.analyses.length).toBeGreaterThan(0);
  expect(next.total).toBe(total);

  await page.screenshot({
    path: path.join(SHOTS, "02-analyses-after-load-more.png"),
    fullPage: true,
  });
});
