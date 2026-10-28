// K18 fleet-first-sync browser acceptance.
//
// One cluster (prod-eks) is held in its first sync by the fixture, so the
// Clusters page shows four rows: preparing, unreachable, attention, healthy.
// The spec verifies the Preparing pill, the live progress line (separate
// elements, no dot/bullet characters), the placeholders, the header counts,
// row order, the inside-cluster Preparing panel, the catalog refresh cadence
// while preparing and after settling, and that the settled rows and actions
// are unchanged. Captures go through the shared capturePath helper.
import { expect, test, type BrowserContext, type Page } from "@playwright/test";
import * as fs from "node:fs";
import * as https from "node:https";
import * as path from "node:path";
import { openApp } from "./helpers";
import { capturePath, harnessResultsDir } from "./support/capture";

const mode = process.env.E2E_K18_MODE || "fakekube";
const heldURL = process.env.E2E_K18_HELD_FAKEKUBE_URL || "";
const heldCA = process.env.E2E_K18_HELD_FAKEKUBE_CA || "";

const viewports = [
  { name: "desktop", width: 1440, height: 900 },
  { name: "mobile", width: 390, height: 844 },
] as const;
const themes = ["dark", "light"] as const;

const preparingRow = "prod-eks";
const settledRows = [
  { id: "staging-gke", pill: "Unreachable", pillClass: "pill-warn" },
  { id: "prod-gke", pill: "Needs attention", pillClass: "pill-warn" },
  { id: "prod-aks", pill: "Node checks passing", pillClass: "pill-good" },
] as const;

function callFakekube(endpoint: string, body?: unknown): Promise<unknown> {
  if (!heldURL || !heldCA) throw new Error("held fakekube connection env required");
  const ca = fs.readFileSync(heldCA);
  return new Promise((resolve, reject) => {
    const req = https.request(`${heldURL}${endpoint}`, {
      ca,
      method: body === undefined ? "GET" : "POST",
      headers: { "Content-Type": "application/json" },
      timeout: 10000,
    }, (res) => {
      let data = "";
      res.on("data", (chunk) => { data += chunk; });
      res.on("end", () => {
        if (res.statusCode !== 200) return reject(new Error(`fakekube ${endpoint} HTTP ${res.statusCode}: ${data}`));
        try { resolve(JSON.parse(data)); } catch (err) { reject(err); }
      });
    });
    req.on("error", reject);
    req.end(body === undefined ? undefined : JSON.stringify(body));
  });
}

async function applyTheme(page: Page, theme: "light" | "dark") {
  await page.emulateMedia({ colorScheme: theme });
  await page.addInitScript((modeTheme) => localStorage.setItem("versus.theme", modeTheme), theme);
}

async function verifyThemeAndQuietPage(page: Page, theme: "light" | "dark") {
  await expect.poll(() => page.locator("html").getAttribute("data-theme")).toBe(theme === "light" ? "light" : null);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  const bodyText = await page.locator("body").innerText();
  expect(bodyText).not.toMatch(/Session expired|Sign in to continue|Unhandled Runtime Error|Couldn't load clusters|connector is not configured/i);
}

async function saveCapture(page: Page, surface: string, viewport: number, theme: "light" | "dark", state: string) {
  await verifyThemeAndQuietPage(page, theme);
  const out = capturePath(surface, viewport, theme, state);
  await page.screenshot({ path: out, animations: "disabled" });
  return out;
}

const row = (page: Page, id: string) => page.locator(`article[aria-label="Cluster ${id}"]`);

// A failing assertion must not leave pages polling the catalog into later
// tests: every context opened here is closed after its test.
const openContexts: BrowserContext[] = [];
test.afterEach(async () => {
  for (const context of openContexts.splice(0)) await context.close().catch(() => {});
});

async function openFleet(page: Page) {
  await openApp(page, "/agent/kubernetes");
  await expect(page.getByRole("heading", { name: "Clusters", exact: true })).toBeVisible();
  await expect(row(page, preparingRow)).toBeVisible();
  await expect(page.locator('[aria-label="Cluster rows"] article')).toHaveCount(4);
}

function fleetHeader(page: Page) {
  // The health strip is a labelled group after the "Clusters" heading; its
  // visible counts are aria-hidden and the group's name is the readable line.
  return page.getByRole("heading", { name: "Clusters", exact: true }).locator("xpath=following-sibling::div[@role='group'][1]");
}

function headerTexts(page: Page) {
  return fleetHeader(page).locator('span[aria-hidden="true"]').allTextContents();
}

async function rowTitles(page: Page) {
  return page.locator('[aria-label="Cluster rows"] article').evaluateAll((nodes) => nodes.map((node) => node.getAttribute("aria-label")));
}

// Inspect the rendered progress line: the step and every count must be their
// own element, stay inside their column without clipping, and overlap nothing.
async function progressLineReport(page: Page) {
  const line = row(page, preparingRow).locator("[data-first-sync]");
  await expect(line).toBeVisible();
  return line.evaluate((node) => {
    const parts = [...node.querySelectorAll("[data-first-sync-step], [data-first-sync-count]")];
    const boxes = parts.map((element) => {
      const rect = element.getBoundingClientRect();
      return {
        text: (element.textContent || "").trim(),
        left: rect.left,
        right: rect.right,
        top: rect.top,
        bottom: rect.bottom,
        scrollWidth: element.scrollWidth,
        clientWidth: element.clientWidth,
      };
    });
    const overlaps: string[][] = [];
    for (let left = 0; left < boxes.length; left += 1) {
      for (let right = left + 1; right < boxes.length; right += 1) {
        const a = boxes[left];
        const b = boxes[right];
        if (a.left < b.right - 0.5 && b.left < a.right - 0.5 && a.top < b.bottom - 0.5 && b.top < a.bottom - 0.5) {
          overlaps.push([a.text, b.text]);
        }
      }
    }
    const rect = node.getBoundingClientRect();
    const clipped = boxes.filter((box) => box.scrollWidth > box.clientWidth + 1);
    const outside = boxes.filter((box) => box.left < rect.left - 0.5 || box.right > rect.right + 0.5 || box.top < rect.top - 0.5 || box.bottom > rect.bottom + 0.5);
    return {
      text: node.textContent || "",
      innerText: (node as HTMLElement).innerText || "",
      scrollWidth: node.scrollWidth,
      clientWidth: node.clientWidth,
      height: rect.height,
      boxes,
      overlaps,
      clipped: clipped.map((box) => box.text),
      outside: outside.map((box) => box.text),
      stepCount: node.querySelectorAll("[data-first-sync-step]").length,
      countCount: node.querySelectorAll("[data-first-sync-count]").length,
      betweenParts: parts.slice(0, -1).map((element) => element.nextSibling?.nodeType === 3 ? String(element.nextSibling.textContent || "") : ""),
    };
  });
}

test.describe("K18 fleet first sync", () => {
  test.skip(mode !== "fakekube", "runs in isolated fakekube mode");
  test.setTimeout(300_000);

  test("Preparing row, progress parts without separators, header counts, order and settled rows at 1440/390 dark/light", async ({ browser }) => {
    const sessions: Array<{ vp: (typeof viewports)[number]; theme: (typeof themes)[number]; context: Awaited<ReturnType<typeof browser.newContext>>; page: Page }> = [];
    for (const vp of viewports) {
      for (const theme of themes) {
        const context = await browser.newContext({ viewport: { width: vp.width, height: vp.height } });
        const page = await context.newPage();
        await applyTheme(page, theme);
        await openFleet(page);
        sessions.push({ vp, theme, context, page });
      }
    }

    for (const { vp, theme, page } of sessions) {
      const preparing = row(page, preparingRow);

      // 3. Header counts include the preparing cluster and never call it unknown.
      const header = (await headerTexts(page)).map((text) => text.trim());
      expect(header).toEqual([
        "4 clusters",
        "1 unreachable",
        "1 preparing",
        "1 attention",
        "0 partial",
        "0 unknown",
        "1 healthy",
      ]);
      // The visible counts are spacing-separated; assistive tech reads one
      // comma-separated line naming each count.
      await expect(fleetHeader(page)).toHaveAccessibleName(
        "4 clusters, 1 unreachable, 1 preparing, 1 attention, 0 partial, 0 unknown, 1 healthy",
      );

      // Row order: unreachable, preparing, attention, healthy.
      expect(await rowTitles(page)).toEqual([
        "Cluster staging-gke",
        "Cluster prod-eks",
        "Cluster prod-gke",
        "Cluster prod-aks",
      ]);

      // 1. Preparing row: neutral pill, no first-sync zeros, placeholders.
      const pill = preparing.locator("span.pill").first();
      await expect(pill).toHaveText("Preparing");
      await expect(pill).toHaveClass(/^pill\s*$/);
      expect((await preparing.innerText()).toLowerCase()).not.toContain("undefined");
      expect(await preparing.innerText()).not.toContain("0 / 0");
      expect(await preparing.innerText()).not.toContain("0 warnings");
      await expect(preparing.getByText("Loading", { exact: true })).toHaveCount(2);
      await expect(preparing.locator("meter")).toHaveCount(0);
      await expect(preparing.getByText("Waiting for first data", { exact: true })).toBeVisible();
      await expect(preparing.getByText("Preparing first data", { exact: true })).toBeVisible();
      expect(await preparing.innerText()).not.toMatch(/Syncing\b/);

      // Whole-row link and Review issues action are unchanged and openable. The
      // row link carries the readable progress phrase, so the visible line can
      // stay a single spacing-only line.
      const open = preparing.locator(`a[aria-label^="Open cluster ${preparingRow}"]`);
      await expect(open).toHaveAttribute("href", `/agent/kubernetes?cluster=${preparingRow}`);
      const visibleParts = await preparing.locator("[data-first-sync-step], [data-first-sync-count]").allTextContents();
      expect(await open.getAttribute("aria-label")).toBe(
        `Open cluster ${preparingRow}: Preparing first data, ${visibleParts.join(", ")}`,
      );
      expect(await open.getAttribute("aria-label")).not.toMatch(/[.·•]/);
      await expect(preparing.getByRole("link", { name: "Review issues" })).toHaveAttribute("href", `/agent/kubernetes?cluster=${preparingRow}&view=issues`);

      // 2. Progress line: separate elements, no separator characters, no clipping.
      const report = await progressLineReport(page);
      expect(report.stepCount).toBe(1);
      expect(report.countCount).toBeGreaterThanOrEqual(3);
      // One full-width line under the row, outside the Health cell, named as a
      // comma-separated phrase for assistive tech.
      await expect(preparing.locator("[data-first-sync]")).toHaveAccessibleName(
        [report.boxes[0].text, ...report.boxes.slice(1).map((box) => box.text)].join(", "),
      );
      expect(await preparing.locator("[data-first-sync]").evaluate((line) => line.parentElement === line.closest("article"))).toBe(true);
      expect(await preparing.locator("[data-health-cell] [data-first-sync]").count()).toBe(0);
      // A single visual line at both widths: it must never wrap or hide a part.
      expect(report.height).toBeLessThanOrEqual(24);
      expect(report.text).not.toMatch(/[.·•]/);
      expect(report.innerText).not.toMatch(/[.·•]/);
      expect(report.betweenParts.filter((between) => between.trim() !== "")).toEqual([]);
      expect(report.boxes[0].text).toBe("Listing resources");
      expect(report.boxes.slice(1).map((box) => box.text.replace(/ [\d,]+$/, ""))).toEqual(["Pods", "Deployments", "Namespaces"]);
      expect(report.overlaps).toEqual([]);
      expect(report.clipped).toEqual([]);
      expect(report.outside).toEqual([]);
      expect(report.scrollWidth).toBeLessThanOrEqual(report.clientWidth + 1);

      await saveCapture(page, "k18-fleet", vp.width, theme, "preparing");
      const linePath = capturePath("k18-progress-line", vp.width, theme, "preparing");
      await preparing.locator("[data-first-sync]").screenshot({ path: linePath, animations: "disabled" });

      if (vp.width === 390) {
        // The fleet table scrolls horizontally on a narrow viewport; bring the
        // whole health column into view and capture the wrapped line again.
        await page.locator('[aria-label="Cluster rows"]').evaluate((region) => { region.scrollLeft = 230; });
        await saveCapture(page, "k18-fleet", vp.width, theme, "preparing-progress");
      }

      // 6. Settled rows and actions unchanged.
      const unreachable = row(page, "staging-gke");
      await expect(unreachable.locator("span.pill").first()).toHaveText("Unreachable");
      await expect(unreachable.locator("span.pill").first()).toHaveClass(/pill-warn/);
      await expect(unreachable.getByText("The cluster did not respond.")).toBeVisible();
      await expect(unreachable.getByText("Unavailable", { exact: true })).toHaveCount(3);
      await expect(unreachable.getByRole("button", { name: "Retry" })).toBeVisible();
      await expect(unreachable.getByRole("link", { name: "Review issues" })).toHaveCount(0);

      const attention = row(page, "prod-gke");
      await expect(attention.locator("span.pill").first()).toHaveText("Needs attention");
      await expect(attention.locator("span.pill").first()).toHaveClass(/pill-warn/);
      expect(await attention.innerText()).toContain("1 / 2");
      expect(await attention.innerText()).toContain("3 / 3");
      await expect(attention.getByRole("link", { name: "Review issues" })).toBeVisible();
      await expect(attention.locator("meter")).toHaveCount(2);

      const healthy = row(page, "prod-aks");
      await expect(healthy.locator("span.pill").first()).toHaveText("Node checks passing");
      await expect(healthy.locator("span.pill").first()).toHaveClass(/pill-good/);
      expect(await healthy.innerText()).toContain("1 / 1");
      expect(await healthy.innerText()).toContain("2 / 2");
      await expect(healthy.getByRole("link", { name: "Review issues" })).toBeVisible();
      await expect(healthy.locator("meter")).toHaveCount(2);

      for (const settled of settledRows) {
        const cell = row(page, settled.id);
        await expect(cell.getByText("Preparing first data")).toHaveCount(0);
        await expect(cell.getByText("Waiting for first data")).toHaveCount(0);
        await expect(cell.getByText("Loading", { exact: true })).toHaveCount(0);
        await expect(cell.locator("[data-first-sync]")).toHaveCount(0);
        expect(await cell.innerText()).toMatch(/\d{1,2}:\d{2}:\d{2}/);
      }

      // Accessibility: every row is a labelled link target, pills have text and
      // the interactive controls are reachable by keyboard in DOM order.
      for (const id of [preparingRow, ...settledRows.map((settled) => settled.id)]) {
        await expect(row(page, id).locator(`a[aria-label^="Open cluster ${id}"]`).first()).toBeVisible();
        expect((await row(page, id).locator("span.pill").first().innerText()).trim()).not.toBe("");
      }
      if (vp.width === 1440 && theme === "dark") {
        await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
        const focusTrail: string[] = [];
        let reachedRowLink = false;
        for (let attempt = 0; attempt < 50; attempt += 1) {
          await page.keyboard.press("Tab");
          const focused = await page.evaluate(() => {
            const element = document.activeElement as HTMLElement | null;
            return element?.getAttribute("aria-label") || (element?.textContent || "").trim().slice(0, 40);
          });
          focusTrail.push(focused);
          if (focused.startsWith(`Open cluster ${preparingRow}`)) {
            reachedRowLink = true;
            break;
          }
        }
        expect(focusTrail.some((focused) => focused.startsWith(`Open cluster ${preparingRow}`))).toBe(true);
        expect(reachedRowLink).toBe(true);
        await page.keyboard.press("Tab");
        const nextFocus = await page.evaluate(() => {
          const element = document.activeElement as HTMLElement | null;
          return element?.getAttribute("aria-label") || (element?.textContent || "").trim().slice(0, 40);
        });
        expect(nextFocus).toBe("Review issues");
        // The row link is a real focus target, not just an invisible overlay.
        await open.focus();
        await expect(open).toBeFocused();
      }
    }

    for (const { context } of sessions) await context.close();
  });

  test("Opening the preparing cluster shows the Preparing panel with separate listing chips and no dots at 1440/390 dark/light", async ({ browser }) => {
    for (const vp of viewports) {
      for (const theme of themes) {
        const context = await browser.newContext({ viewport: { width: vp.width, height: vp.height } });
        openContexts.push(context);
        const page = await context.newPage();
        await applyTheme(page, theme);
        await openApp(page, `/agent/kubernetes?cluster=${preparingRow}`);
        await expect(page.getByRole("heading", { name: "Clusters", exact: true })).toHaveCount(0);
        const panel = page.locator("[data-preparing-panel]");
        await expect(panel).toBeVisible();
        await expect(panel.getByRole("heading", { name: "Preparing your cluster" })).toBeVisible();
        await expect(panel).toContainText(`First sync of ${preparingRow}: every tab opens with data once this finishes.`);
        await expect(panel.getByRole("list", { name: "First sync steps" }).getByRole("listitem")).toHaveCount(4);

        const listing = panel.locator('[data-step="listing"]');
        await expect(listing).toHaveAttribute("data-state", "active");
        const chips = listing.locator("[data-first-sync-counts] [data-first-sync-count]");
        await expect(chips.first()).toBeVisible();
        expect(await chips.count()).toBeGreaterThanOrEqual(3);
        const chipText = await chips.allTextContents();
        for (const text of chipText) expect(text.trim()).toMatch(/^(Pods|Deployments|Namespaces|StatefulSets|DaemonSets|Jobs) [\d,]+$/);

        const chipsContainer = listing.locator("[data-first-sync-counts]");
        const chipsText = (await chipsContainer.textContent()) || "";
        expect(chipsText).not.toMatch(/[.·•]/);
        // No character between the chips: their text is the parts back to back.
        expect(chipsText.replace(/\s/g, "")).toBe(chipText.map((text) => text.replace(/\s/g, "")).join(""));

        const geometry = await chipsContainer.evaluate((node) => {
          const items = [...node.querySelectorAll("[data-first-sync-count]")];
          const boxes = items.map((item) => {
            const rect = item.getBoundingClientRect();
            return { text: (item.textContent || "").trim(), left: rect.left, right: rect.right, top: rect.top, bottom: rect.bottom, scrollWidth: item.scrollWidth, clientWidth: item.clientWidth };
          });
          const overlaps: string[][] = [];
          for (let left = 0; left < boxes.length; left += 1) {
            for (let right = left + 1; right < boxes.length; right += 1) {
              const a = boxes[left];
              const b = boxes[right];
              if (a.left < b.right - 0.5 && b.left < a.right - 0.5 && a.top < b.bottom - 0.5 && b.top < a.bottom - 0.5) overlaps.push([a.text, b.text]);
            }
          }
          const rect = node.getBoundingClientRect();
          return {
            overlaps,
            clipped: boxes.filter((box) => box.scrollWidth > box.clientWidth + 1).map((box) => box.text),
            outside: boxes.filter((box) => box.left < rect.left - 0.5 || box.right > rect.right + 0.5).map((box) => box.text),
            height: rect.height,
          };
        });
        expect(geometry.overlaps).toEqual([]);
        expect(geometry.clipped).toEqual([]);
        expect(geometry.outside).toEqual([]);
        expect((await page.locator("body").innerText())).not.toMatch(/Session expired|Unhandled Runtime Error|Couldn't load Kubernetes overview/i);

        await chipsContainer.scrollIntoViewIfNeeded();
        await saveCapture(page, "k18-preparing-panel", vp.width, theme, "listing");
        await context.close();
        const index = openContexts.indexOf(context);
        if (index >= 0) openContexts.splice(index, 1);
      }
    }
  });

  test("Catalog refreshes about every 4 s while preparing, slows to 30 s after settling, and the row settles without a reload", async ({ browser }) => {
    const context = await browser.newContext({ viewport: { width: 1440, height: 900 } });
    openContexts.push(context);
    const page = await context.newPage();
    await applyTheme(page, "dark");
    const navigations: string[] = [];
    page.on("framenavigated", (frame) => { if (frame === page.mainFrame()) navigations.push(frame.url()); });
    await page.addInitScript(() => {
      const scope = window as unknown as { __k18Loads?: number };
      scope.__k18Loads = (scope.__k18Loads || 0) + 1;
    });
    const samples: Array<{ at: number; syncing: boolean; clusters: number }> = [];
    page.on("response", (response) => {
      if (!response.url().includes("/api/admin/kubernetes/clusters")) return;
      void response.json().then((body: { clusters?: Array<{ health?: string; sync?: { state?: string } }> }) => {
        const clusters = body.clusters || [];
        samples.push({
          at: Date.now(),
          syncing: clusters.some((cluster) => cluster.health === "syncing" || cluster.sync?.state === "syncing"),
          clusters: clusters.length,
        });
      }).catch(() => {});
    });

    await openFleet(page);
    await expect(fleetHeader(page).getByText("1 preparing", { exact: true })).toBeVisible();

    // Observe the live cadence while the cluster is preparing.
    await page.waitForTimeout(16_000);
    samples.sort((left, right) => left.at - right.at);
    const preparing = samples.filter((sample) => sample.syncing);
    const preparingGaps = preparing.slice(1).map((sample, index) => sample.at - preparing[index].at);
    console.log(`[k18] catalog samples: ${samples.map((sample) => `${sample.at}:${sample.syncing ? "preparing" : "settled"}:${sample.clusters}`).join(",")}`);
    console.log(`[k18] preparing catalog timestamps: ${preparing.map((sample) => sample.at).join(",")}`);
    console.log(`[k18] preparing catalog gaps ms: ${preparingGaps.join(",")}`);
    expect(preparing.length).toBeGreaterThanOrEqual(3);
    expect(preparingGaps.length).toBeGreaterThanOrEqual(2);
    for (const gap of preparingGaps) {
      expect(gap).toBeGreaterThanOrEqual(3_200);
      expect(gap).toBeLessThanOrEqual(7_000);
    }

    // Release the held core LIST: the cluster finishes its first sync.
    await callFakekube("/_fake/list-holds", { resource: "services", hold: false, remaining: 0 });
    const releaseAt = Date.now();
    await expect.poll(async () => (await headerTexts(page)).some((text) => text.trim() === "0 preparing"), { timeout: 60_000, intervals: [500, 1_000] }).toBe(true);
    await expect(row(page, preparingRow).locator("span.pill").first()).not.toHaveText("Preparing");
    const settledAt = Date.now();

    // The next catalog fetch after the first settled response must wait ~30 s.
    await expect.poll(() => samples.some((sample) => sample.at >= releaseAt && !sample.syncing), { timeout: 15_000 }).toBe(true);
    samples.sort((left, right) => left.at - right.at);
    const settledIndex = samples.findIndex((sample) => sample.at >= releaseAt && !sample.syncing);
    expect(settledIndex).toBeGreaterThanOrEqual(0);
    const deadline = Date.now() + 45_000;
    while (Date.now() < deadline && samples.length <= settledIndex + 1) await page.waitForTimeout(250);
    expect(samples.length).toBeGreaterThan(settledIndex + 1);
    const settledGap = samples[settledIndex + 1].at - samples[settledIndex].at;
    console.log(`[k18] settled catalog timestamps: ${samples.map((sample) => `${sample.at}:${sample.syncing ? "preparing" : "settled"}`).join(",")}`);
    console.log(`[k18] settled catalog gap ms: ${settledGap} (release ${releaseAt}, settled ${settledAt})`);
    expect(settledGap).toBeGreaterThanOrEqual(25_000);
    expect(settledGap).toBeLessThanOrEqual(36_000);

    const evidence = {
      observed_at: new Date().toISOString(),
      preparing_cadence_ms: preparingGaps,
      preparing_timestamps: preparing.map((sample) => sample.at),
      settled_gap_ms: settledGap,
      settled_index: settledIndex,
      release_at: releaseAt,
      settled_at: settledAt,
      samples,
      navigations,
    };
    const evidenceDir = path.join(harnessResultsDir(), "k18-cadence");
    fs.mkdirSync(evidenceDir, { recursive: true });
    fs.writeFileSync(path.join(evidenceDir, "catalog-cadence.json"), `${JSON.stringify(evidence, null, 2)}\n`);

    // The row settled in place: the document was never reloaded after openFleet.
    const loads = await page.evaluate(() => (window as unknown as { __k18Loads?: number }).__k18Loads);
    expect(loads).toBe(1);
    console.log(`[k18] main-frame navigations: ${navigations.join(",")}`);
    await saveCapture(page, "k18-fleet", 1440, "dark", "settled");
    await expect(page.locator('[aria-label="Cluster rows"] article')).toHaveCount(4);
    await context.close();
    const index = openContexts.indexOf(context);
    if (index >= 0) openContexts.splice(index, 1);
  });
});
