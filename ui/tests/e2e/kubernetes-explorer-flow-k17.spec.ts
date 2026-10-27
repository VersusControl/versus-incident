import { expect, test, type Locator, type Page } from "@playwright/test";
import * as fs from "node:fs";
import * as https from "node:https";
import { openApp } from "./helpers";
import { capturePath } from "./support/capture";

const mode = process.env.E2E_K17_MODE || "fakekube";
const secondaryBaseURL = process.env.E2E_SECONDARY_BASE_URL || "";
const fakekubeURL = process.env.E2E_FAKEKUBE_URL || "";
const fakekubeCA = process.env.E2E_FAKEKUBE_CA || "";

function callFakekube(endpoint: string, body?: unknown): Promise<unknown> {
  if (!fakekubeURL || !fakekubeCA) throw new Error("fakekube connection env required");
  const ca = fs.readFileSync(fakekubeCA);
  return new Promise((resolve, reject) => {
    const req = https.request(`${fakekubeURL}${endpoint}`, {
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
  expect(bodyText).not.toMatch(/Session expired|Sign in to continue|Unhandled Runtime Error/i);
}

async function saveCapture(page: Page, surface: string, viewport: number, theme: "light" | "dark", state: string) {
  await verifyThemeAndQuietPage(page, theme);
  const out = capturePath(surface, viewport, theme, state);
  await page.screenshot({ path: out, animations: "disabled" });
  return out;
}

async function openDrawerTab(page: Page, dialog: Locator, tabId: "overview" | "logs" | "events" | "yaml" | "related" | "timeline", label: string) {
  const directTab = dialog.locator(`#kubernetes-drawer-tab-${tabId}`);
  const moreSummary = dialog.locator("details.resource-more-tabs > summary");
  // The drawer re-renders while the resource detail loads and while live events arrive, and on narrow
  // viewports workload tabs collapse behind "More tabs". Retry until one real control is usable.
  for (let attempt = 0; attempt < 6; attempt += 1) {
    if (await directTab.isVisible().catch(() => false)) {
      try {
        await directTab.click({ timeout: 3_000 });
        return;
      } catch {
        // detached mid-click; retry
      }
    } else if (await moreSummary.isVisible().catch(() => false)) {
      try {
        await dialog.locator("details.resource-more-tabs").evaluate((node) => {
          (node as HTMLDetailsElement).open = true;
        }, undefined, { timeout: 3_000 });
        await dialog.getByRole("menuitem", { name: label, exact: true }).click({ timeout: 3_000 });
        return;
      } catch {
        // detached mid-click; retry
      }
    }
    await page.waitForTimeout(250);
  }
  throw new Error(`could not open drawer tab ${tabId}`);
}

function createRouteBarrier() {
  let held = true;
  let releaseResolve!: () => void;
  const releasePromise = new Promise<void>((resolve) => {
    releaseResolve = resolve;
  });
  let hitResolve!: () => void;
  const hit = new Promise<void>((resolve) => {
    hitResolve = resolve;
  });
  const pending: Promise<void>[] = [];
  const handler = async (route: import("@playwright/test").Route) => {
    if (held) {
      hitResolve();
      const p = releasePromise.then(() => route.continue());
      pending.push(p);
      await p;
    } else {
      await route.continue();
    }
  };
  const release = async () => {
    held = false;
    releaseResolve();
    await Promise.all(pending);
  };
  return { handler, hit, release };
}

async function verifyRelationshipMapGeometry(svgLocator: Locator) {
  const report = await svgLocator.evaluate((svg) => {
    const nodeGroups = [...svg.querySelectorAll<SVGGElement>("g.relationship-node")];
    const boxes = nodeGroups.map((g) => {
      const tf = g.getAttribute("transform") || "";
      const match = /translate\(\s*([+-]?\d+(?:\.\d+)?)\s+([+-]?\d+(?:\.\d+)?)\s*\)/.exec(tf);
      const rect = g.querySelector("rect")!;
      const x = match ? Number(match[1]) : 0;
      const y = match ? Number(match[2]) : 0;
      const width = Number(rect.getAttribute("width") || 200);
      const height = Number(rect.getAttribute("height") || 44);
      const label = g.getAttribute("aria-label") || g.textContent || "";
      return { x, y, width, height, label };
    });

    const boxOverlaps: string[] = [];
    for (let i = 0; i < boxes.length; i++) {
      for (let j = i + 1; j < boxes.length; j++) {
        const a = boxes[i];
        const b = boxes[j];
        const overlapX = Math.min(a.x + a.width, b.x + b.width) - Math.max(a.x, b.x);
        const overlapY = Math.min(a.y + a.height, b.y + b.height) - Math.max(a.y, b.y);
        if (overlapX > -8 && overlapY > -8) {
          boxOverlaps.push(`${a.label} overlaps/too close to ${b.label}`);
        }
      }
    }

    const edgePaths = [...svg.querySelectorAll<SVGPathElement>("g[data-relationship-from] > path, g[data-relationship-node-ref] > path")];
    const crossings: string[] = [];

    for (const pathEl of edgePaths) {
      const d = pathEl.getAttribute("d") || "";
      const commands = [...d.matchAll(/([MLHV])\s*([+-]?\d+(?:\.\d+)?)(?:\s+([+-]?\d+(?:\.\d+)?))?/g)];
      const points: Array<[number, number]> = [];
      let cx = 0;
      let cy = 0;
      for (const cmd of commands) {
        const type = cmd[1];
        const v1 = Number(cmd[2]);
        const v2 = cmd[3] !== undefined ? Number(cmd[3]) : undefined;
        if (type === "M" || type === "L") {
          cx = v1;
          cy = v2 ?? cy;
        } else if (type === "H") {
          cx = v1;
        } else if (type === "V") {
          cy = v1;
        }
        points.push([cx, cy]);
      }

      for (let s = 0; s + 1 < points.length; s++) {
        const [x1, y1] = points[s];
        const [x2, y2] = points[s + 1];
        const minX = Math.min(x1, x2);
        const maxX = Math.max(x1, x2);
        const minY = Math.min(y1, y2);
        const maxY = Math.max(y1, y2);

        for (const box of boxes) {
          // Strict interior of the box (inset by 1px so endpoints touching the border are allowed)
          const ix1 = box.x + 1;
          const iy1 = box.y + 1;
          const ix2 = box.x + box.width - 1;
          const iy2 = box.y + box.height - 1;

          if (Math.abs(y1 - y2) < 0.5) {
            // Horizontal segment at y = y1
            if (y1 > iy1 && y1 < iy2 && maxX > ix1 && minX < ix2) {
              crossings.push(`Horizontal segment (${x1},${y1})->(${x2},${y2}) crosses box ${box.label} [${box.x},${box.y},${box.width},${box.height}]`);
            }
          } else if (Math.abs(x1 - x2) < 0.5) {
            // Vertical segment at x = x1
            if (x1 > ix1 && x1 < iy2 && x1 < ix2 && maxY > iy1 && minY < iy2) {
              crossings.push(`Vertical segment (${x1},${y1})->(${x2},${y2}) crosses box ${box.label} [${box.x},${box.y},${box.width},${box.height}]`);
            }
          } else {
            // Diagonal segment sample check
            for (let t = 0.05; t <= 0.95; t += 0.05) {
              const px = x1 + (x2 - x1) * t;
              const py = y1 + (y2 - y1) * t;
              if (px > ix1 && px < ix2 && py > iy1 && py < iy2) {
                crossings.push(`Segment (${x1},${y1})->(${x2},${y2}) crosses box ${box.label}`);
                break;
              }
            }
          }
        }
      }
    }

    return {
      nodeCount: boxes.length,
      edgeCount: edgePaths.length,
      boxOverlaps,
      crossings,
    };
  });

  expect(report.nodeCount).toBeGreaterThanOrEqual(4);
  expect(report.edgeCount).toBeGreaterThanOrEqual(3);
  expect(report.boxOverlaps).toEqual([]);
  expect(report.crossings).toEqual([]);
}

const viewports = [
  { name: "desktop", width: 1440, height: 900 },
  { name: "mobile", width: 390, height: 844 },
] as const;

const themes = ["dark", "light"] as const;

test.describe("Kubernetes explorer flow and first-sync experience", () => {
  test.skip(mode !== "fakekube", "runs in isolated fakekube mode");
  test.setTimeout(300_000);

  test("Cold sync Preparing panel with live steps and ticking counts, core publish with pending resource types note, and full set completion at 1440 and 390 dark and light", async ({ browser }) => {
    const sessions: Array<{ vp: (typeof viewports)[number]; theme: (typeof themes)[number]; context: Awaited<ReturnType<typeof browser.newContext>>; page: Page }> = [];

    for (const vp of viewports) {
      for (const theme of themes) {
        const context = await browser.newContext({ viewport: { width: vp.width, height: vp.height } });
        const page = await context.newPage();
        await applyTheme(page, theme);
        await openApp(page, "/agent/kubernetes");
        sessions.push({ vp, theme, context, page });
      }
    }

    const firstPage = sessions[0].page;

    // 1. Wait for initial cold sync progress before pods list hold is released (Deployments & Namespaces listed, Nodes=12, version=v1.30)
    await expect.poll(async () => {
      const res = await firstPage.request.get("/api/admin/kubernetes/overview");
      if (!res.ok()) return null;
      const body = await res.json();
      const prog = body.sync?.progress;
      if (body.sync?.state !== "syncing" || !prog) return null;
      return prog.counts?.Deployment && prog.counts?.Namespace === 27 && prog.node_count === 12 ? prog : null;
    }, { timeout: 30_000, intervals: [300, 500] }).not.toBeNull();

    // Verify Listing resources step shows initial counts before Pods finish.
    // K18 renders each count as its own chip, so the separator-free chips and
    // the comma-joined group name are asserted instead of a dotted phrase.
    const initialSteps = firstPage.getByRole("list", { name: "First sync steps", exact: true });
    await expect(initialSteps).toBeVisible();
    const initialCounts = initialSteps.locator("[data-first-sync-counts]");
    await expect(initialCounts).toHaveAccessibleName(/^Deployments \d+, Namespaces 27(?:, [A-Za-z]+ [\d,]+)*$/);
    const initialChips = initialCounts.locator("[data-first-sync-count]");
    await expect(initialChips.filter({ hasText: /^Deployments \d+$/ })).toHaveCount(1);
    await expect(initialChips.filter({ hasText: /^Namespaces 27$/ })).toHaveCount(1);
    const initialChipText = (await initialChips.allTextContents()).map((text) => text.trim());
    for (const text of initialChipText) expect(text).not.toMatch(/[.·•]/);
    expect(await initialCounts.textContent()).not.toMatch(/[.·•]/);

    // Release pods hold while keeping statefulsets held so Pods count ticks up live
    await callFakekube("/_fake/list-holds", { resource: "pods", hold: false, remaining: 0 });

    await expect.poll(async () => {
      const res = await firstPage.request.get("/api/admin/kubernetes/overview");
      if (!res.ok()) return null;
      const body = await res.json();
      return body.sync?.state === "syncing" && (body.sync?.progress?.counts?.Pod ?? 0) >= 600 ? body.sync.progress : null;
    }, { timeout: 30_000, intervals: [300, 500] }).not.toBeNull();

    // Assert Preparing panel across all 4 viewport/theme combinations and capture
    for (const { vp, theme, page } of sessions) {
      await expect(page.getByRole("status").filter({ hasText: "Preparing" })).toBeVisible();
      await expect(page.getByText("Syncing cluster", { exact: true })).toHaveCount(0);

      const panel = page.getByRole("region", { name: "Preparing your cluster", exact: true });
      await expect(panel).toBeVisible();
      await expect(panel).toContainText("First sync of prod-eks: every tab opens with data once this finishes.");

      const facts = panel.getByRole("list", { name: "Cluster facts", exact: true });
      await expect(facts).toBeVisible();
      await expect(facts.getByRole("listitem")).toHaveText(["Kubernetes v1.30", "12 nodes"]);

      const steps = panel.getByRole("list", { name: "First sync steps", exact: true });
      await expect(steps).toHaveAttribute("aria-live", "polite");
      const stepItems = steps.getByRole("listitem");
      await expect(stepItems).toHaveCount(4);
      await expect(stepItems.nth(0)).toHaveAttribute("data-step", "connected");
      await expect(stepItems.nth(0)).toHaveAttribute("data-state", "done");
      await expect(stepItems.nth(0)).toContainText("Connected to the cluster");
      await expect(stepItems.nth(0)).toContainText("Kubernetes v1.30");

      await expect(stepItems.nth(1)).toHaveAttribute("data-step", "listing");
      await expect(stepItems.nth(1)).toHaveAttribute("data-state", "active");
      await expect(stepItems.nth(1)).toContainText("Listing resources");
      // Each listed count is its own chip, separated by spacing only; the group
      // name is the readable comma-joined phrase.
      const listingCounts = stepItems.nth(1).locator("[data-first-sync-counts]");
      await expect(listingCounts).toHaveAccessibleName(/^Pods 60\d, Deployments \d+, Namespaces 27(?:, [A-Za-z]+ [\d,]+)*$/);
      const listingChips = listingCounts.locator("[data-first-sync-count]");
      const listingChipText = (await listingChips.allTextContents()).map((text) => text.trim());
      expect(listingChipText.length).toBeGreaterThanOrEqual(3);
      expect(listingChipText.some((text) => /^Pods 60\d$/.test(text))).toBe(true);
      expect(listingChipText.some((text) => /^Deployments \d+$/.test(text))).toBe(true);
      expect(listingChipText).toContain("Namespaces 27");
      for (const text of listingChipText) expect(text).not.toMatch(/[.·•]/);
      expect(await listingCounts.textContent()).not.toMatch(/[.·•]/);

      await expect(stepItems.nth(2)).toHaveAttribute("data-step", "building");
      await expect(stepItems.nth(2)).toHaveAttribute("data-state", "waiting");
      await expect(stepItems.nth(3)).toHaveAttribute("data-step", "metrics");
      await expect(stepItems.nth(3)).toHaveAttribute("data-state", "waiting");

      const bar = panel.getByRole("progressbar", { name: "First sync progress", exact: true });
      await expect(bar).toHaveAttribute("aria-valuetext", "Step 2 of 4");
      await expect(panel.getByText("Usually under a minute for clusters this size", { exact: true })).toBeVisible();

      // Verify Preparing panel also shows on other tabs during cold sync, then return to Overview
      const tabs = page.getByRole("tablist", { name: "Kubernetes views", exact: true }).getByRole("tab");
      await tabs.nth(2).click(); // Issues
      await expect(page.getByRole("region", { name: "Preparing your cluster", exact: true }).first()).toBeVisible();
      await tabs.nth(0).click(); // Overview
      await expect(panel).toBeVisible();

      await saveCapture(page, "k17-preparing-cluster", vp.width, theme, "syncing");
    }

    // 2. Release statefulsets hold (keeping configmaps held) so core generation publishes and Preparing panel closes
    await callFakekube("/_fake/list-holds", { resource: "statefulsets", hold: false, remaining: 0 });

    await expect.poll(async () => {
      const res = await firstPage.request.get("/api/admin/kubernetes/overview");
      if (!res.ok()) return null;
      const body = await res.json();
      return body.sync?.state === "ready" && (body.sync?.pending_kinds ?? 0) > 0 ? body.sync : null;
    }, { timeout: 30_000, intervals: [250, 500] }).not.toBeNull();

    for (const { vp, theme, page } of sessions) {
      await expect(page.getByRole("region", { name: "Preparing your cluster", exact: true })).toHaveCount(0, { timeout: 15_000 });
      await expect(page.getByRole("region", { name: "Cluster health", exact: true })).toBeVisible({ timeout: 15_000 });
      const pendingNote = page.locator("[data-pending-note]").first();
      await expect(pendingNote).toBeVisible();
      await expect(pendingNote).toHaveText(/^Loading \d+ more resource types$/);
      const pendingClass = await pendingNote.getAttribute("class") ?? "";
      expect(pendingClass).toContain("text-ink-400");
      expect(pendingClass).not.toMatch(/warn|critical|sev-/);

      await saveCapture(page, "k17-pending-note", vp.width, theme, "core-published");
    }

    // Verify Issues tab during core-published phase counts pending-only as ready and shows "Evidence loading" note when expanded
    const firstTabs = firstPage.getByRole("tablist", { name: "Kubernetes views", exact: true }).getByRole("tab");
    await firstTabs.nth(2).click();
    const coreIssuesSection = firstPage.getByRole("region", { name: "Kubernetes issues", exact: true });
    await expect(coreIssuesSection).toBeVisible();
    await expect(coreIssuesSection.locator("header").getByText("ready", { exact: true })).toBeVisible({ timeout: 15_000 });
    const coreAppArticle = coreIssuesSection.getByRole("article", { name: "Deployment app-00000", exact: true }).first();
    await expect(coreAppArticle).toBeVisible({ timeout: 15_000 });
    await coreAppArticle.getByRole("button", { name: "Expand app-00000", exact: true }).click();
    await expect(coreAppArticle.locator("[data-pending-note]").first()).toHaveText("Evidence loading", { timeout: 15_000 });
    await firstTabs.nth(0).click();

    // 3. Release configmaps hold so the full set lands and the quiet pending note disappears
    await callFakekube("/_fake/list-holds", { resource: "configmaps", hold: false, remaining: 0 });

    await expect.poll(async () => {
      const res = await firstPage.request.get("/api/admin/kubernetes/overview");
      if (!res.ok()) return null;
      const body = await res.json();
      return body.sync?.state === "ready" && (body.sync?.pending_kinds ?? 0) === 0 ? body : null;
    }, { timeout: 45_000, intervals: [500, 1000] }).not.toBeNull();

    for (const { page, context } of sessions) {
      await page.getByRole("button", { name: "Refresh Kubernetes data", exact: true }).click();
      await expect(page.locator("[data-pending-note]")).toHaveCount(0, { timeout: 15_000 });
      await context.close();
    }
  });

  for (const vp of viewports) {
    for (const theme of themes) {
      test(`Overview capacity, Issues row alignment, Timeline range & Breaks only, Break detail layout & controls, and Related map routing ${vp.width} ${theme}`, async ({ browser }) => {
        await callFakekube("/_fake/list-holds", { resource: "pods", hold: false, remaining: 0 });
        await callFakekube("/_fake/list-holds", { resource: "statefulsets", hold: false, remaining: 0 });
        await callFakekube("/_fake/list-holds", { resource: "configmaps", hold: false, remaining: 0 });

        const context = await browser.newContext({ viewport: { width: vp.width, height: vp.height } });
        const page = await context.newPage();
        const pageErrors: string[] = [];
        page.on("pageerror", (err) => pageErrors.push(err.message));
        await applyTheme(page, theme);

        // --- 1. Overview capacity with allocatable (percent bars) ---
        await openApp(page, "/agent/kubernetes");
        await expect.poll(async () => {
          const res = await page.request.get("/api/admin/kubernetes/overview");
          if (!res.ok()) return null;
          const body = await res.json();
          return body.sync?.state === "ready" && (body.sync?.pending_kinds ?? 0) === 0 && body.allocatable_cpu && body.allocatable_memory ? body : null;
        }, { timeout: 60_000, intervals: [500, 1000] }).not.toBeNull();

        await page.getByRole("button", { name: "Refresh Kubernetes data", exact: true }).click();
        await expect(page.getByRole("region", { name: "Cluster health", exact: true })).toBeVisible({ timeout: 20_000 });

        const cpuInUseMeter = page.getByRole("meter", { name: "CPU in use", exact: true });
        const cpuReservedMeter = page.getByRole("meter", { name: "CPU reserved by requests", exact: true });
        const memInUseMeter = page.getByRole("meter", { name: "Memory in use", exact: true });
        const memReservedMeter = page.getByRole("meter", { name: "Memory reserved by requests", exact: true });

        for (const meter of [cpuInUseMeter, cpuReservedMeter, memInUseMeter, memReservedMeter]) {
          await expect(meter).toBeVisible();
          await expect(meter).toHaveAttribute("aria-valuetext", /^\d+(\.\d+)?% of allocatable$/);
        }
        await expect(page.getByText("Needs allocatable capacity")).toHaveCount(0);
        await expect(page.getByText("48 cores", { exact: true }).first()).toBeVisible();
        await expect(page.getByText("1.5 GiB", { exact: true }).first()).toBeVisible();

        const cpuCard = page.locator("section.card", { has: page.getByRole("heading", { name: "CPU capacity", exact: true }) });
        await cpuCard.scrollIntoViewIfNeeded();
        await saveCapture(page, "k17-overview-capacity", vp.width, theme, "allocatable");

        // --- 2. Secondary cluster: Node-forbidden copy "Needs node read access (list nodes)" including ellipsis/hover at 390 ---
        const secContext = await browser.newContext({ baseURL: secondaryBaseURL, viewport: { width: vp.width, height: vp.height } });
        const secPage = await secContext.newPage();
        await applyTheme(secPage, theme);
        await openApp(secPage, "/agent/kubernetes");
        await expect.poll(async () => {
          const res = await secPage.request.get("/api/admin/kubernetes/overview");
          if (!res.ok()) return null;
          const body = await res.json();
          return body.sync?.state !== "syncing" && (body.partial_failures?.length ?? 0) > 0 ? body : null;
        }, { timeout: 60_000, intervals: [500, 1000] }).not.toBeNull();

        await secPage.getByRole("button", { name: "Refresh Kubernetes data", exact: true }).click();
        await expect(secPage.getByRole("region", { name: "Cluster health", exact: true })).toBeVisible({ timeout: 20_000 });
        await expect(secPage.getByText("Needs allocatable capacity")).toHaveCount(0);

        const secCpuCard = secPage.locator("section.card", { has: secPage.getByRole("heading", { name: "CPU capacity", exact: true }) });
        const secMemCard = secPage.locator("section.card", { has: secPage.getByRole("heading", { name: "Memory capacity", exact: true }) });
        await secCpuCard.scrollIntoViewIfNeeded();

        for (const card of [secCpuCard, secMemCard]) {
          await expect(card.getByText("Needs node read access (list nodes)")).toHaveCount(3); // In use, Reserved by requests, and Allocatable dd
          const allocatableDd = card.locator("dl div", { has: secPage.locator("dt", { hasText: "Allocatable" }) }).locator("dd");
          await expect(allocatableDd).toHaveText("Needs node read access (list nodes)");
          await expect(allocatableDd).toHaveAttribute("title", "Needs node read access (list nodes)");
          await allocatableDd.hover();
          const ddStyles = await allocatableDd.evaluate((el) => {
            const style = getComputedStyle(el);
            const parentBox = el.parentElement!.getBoundingClientRect();
            const box = el.getBoundingClientRect();
            return {
              textOverflow: style.textOverflow,
              overflow: style.overflow,
              whiteSpace: style.whiteSpace,
              truncated: el.scrollWidth > el.clientWidth,
              withinCell: box.right <= parentBox.right + 1,
            };
          });
          expect(ddStyles.textOverflow).toBe("ellipsis");
          expect(ddStyles.overflow).toBe("hidden");
          expect(ddStyles.whiteSpace).toBe("nowrap");
          expect(ddStyles.withinCell).toBe(true);
          if (vp.width === 390) {
            expect(ddStyles.truncated).toBe(true);
          }
        }
        await saveCapture(secPage, "k17-overview-capacity", vp.width, theme, "node-forbidden");
        await secContext.close();

        // --- 3. Issues row: Timeline and Investigate same size, all row text vertically centred ---
        const tabs = page.getByRole("tablist", { name: "Kubernetes views", exact: true }).getByRole("tab");
        await tabs.nth(2).click();
        await expect(page).toHaveURL(/view=issues/);
        const issueSection = page.getByRole("region", { name: "Kubernetes issues", exact: true });
        await expect(issueSection).toBeVisible();

        const appIssueArticle = issueSection.getByRole("article", { name: "Deployment app-00000", exact: true }).first();
        await expect(appIssueArticle).toBeVisible({ timeout: 20_000 });
        const issueRow = appIssueArticle.locator("[data-issue-row]");
        await expect(issueRow).toBeVisible();

        const rowGeometry = await issueRow.evaluate((row, viewportWidth) => {
          const style = getComputedStyle(row);
          const timelineBtn = row.querySelector<HTMLElement>(`button[aria-label="Timeline for app-00000"]`)!;
          const investigateBtn = row.querySelector<HTMLElement>(`button[aria-label="Investigate app-00000"]`)!;
          const tBox = timelineBtn.getBoundingClientRect();
          const iBox = investigateBtn.getBoundingClientRect();
          const tStyle = getComputedStyle(timelineBtn);
          const iStyle = getComputedStyle(investigateBtn);

          const elements = [
            row.querySelector<HTMLElement>(`button[aria-label*="app-00000"]`)!, // chevron
            ...Array.from(row.querySelectorAll<HTMLElement>(".pill")),
            row.querySelector<HTMLElement>(`button[aria-label="Open resource app-00000"]`)!,
            ...Array.from(row.children[2]?.querySelectorAll<HTMLElement>(":scope > span") ?? []),
            timelineBtn,
            investigateBtn,
          ].filter(Boolean);

          const boxes = elements.map((el) => {
            const b = el.getBoundingClientRect();
            return {
              text: (el.textContent || el.getAttribute("aria-label") || "").trim(),
              top: b.top,
              bottom: b.bottom,
              height: b.height,
              centerY: b.top + b.height / 2,
            };
          });

          // Group elements by line (for 390px wrapping) or single line (for 1440px)
          const lines: Array<typeof boxes> = [];
          for (const item of boxes) {
            const line = lines.find((group) => Math.abs(group[0].centerY - item.centerY) < 12);
            if (line) line.push(item);
            else lines.push([item]);
          }

          const maxLineCenterDelta = Math.max(
            0,
            ...lines.map((line) => {
              const avg = line.reduce((s, item) => s + item.centerY, 0) / line.length;
              return Math.max(...line.map((item) => Math.abs(item.centerY - avg)));
            }),
          );

          return {
            display: style.display,
            alignItems: style.alignItems,
            tHeight: tBox.height,
            iHeight: iBox.height,
            tWidth: tBox.width,
            iWidth: iBox.width,
            tCenterY: tBox.top + tBox.height / 2,
            iCenterY: iBox.top + iBox.height / 2,
            tPaddingTop: tStyle.paddingTop,
            iPaddingTop: iStyle.paddingTop,
            tPaddingBottom: tStyle.paddingBottom,
            iPaddingBottom: iStyle.paddingBottom,
            lineCount: lines.length,
            maxLineCenterDelta,
            viewportWidth,
          };
        }, vp.width);

        expect(rowGeometry.display).toBe("flex");
        expect(rowGeometry.alignItems).toBe("center");
        expect(Math.abs(rowGeometry.tHeight - rowGeometry.iHeight)).toBeLessThanOrEqual(1);
        expect(Math.abs(rowGeometry.tCenterY - rowGeometry.iCenterY)).toBeLessThanOrEqual(1);
        expect(rowGeometry.tPaddingTop).toBe(rowGeometry.iPaddingTop);
        expect(rowGeometry.tPaddingBottom).toBe(rowGeometry.iPaddingBottom);
        if (vp.width === 1440) {
          expect(rowGeometry.lineCount).toBe(1);
        } else {
          expect(Math.abs(rowGeometry.tWidth - rowGeometry.iWidth)).toBeLessThanOrEqual(1);
        }
        expect(rowGeometry.maxLineCenterDelta).toBeLessThanOrEqual(2);

        await saveCapture(page, "k17-issues-row", vp.width, theme, "aligned");

        // --- 4. Timeline list: Range 15m/1h/6h/24h keeps previous rows + spinner on range control, and Breaks only toggle (URL breaks=1) ---
        await tabs.nth(3).click();
        await expect(page).toHaveURL(/view=timeline/);
        const timelineSection = page.getByRole("region", { name: "Kubernetes change timeline", exact: true });
        await expect(timelineSection).toBeVisible();
        const timelineList = timelineSection.getByRole("list", { name: "Breaks and changes", exact: true });
        await expect(timelineList.getByRole("listitem").first()).toBeVisible({ timeout: 20_000 });

        // Tag the list element to prove no whole-tab remount across 15m/1h/6h/24h
        await timelineList.evaluate((el) => { (el as HTMLElement).dataset.qaKeepInstance = "timeline-list-1"; });

        const rangeGroup = timelineSection.getByRole("group", { name: "Timeline window", exact: true });
        const timelineRangeGate = createRouteBarrier();
        await page.route("**/api/admin/kubernetes/timeline?*", timelineRangeGate.handler);

        await rangeGroup.getByRole("button", { name: "6h", exact: true }).click();
        await timelineRangeGate.hit;

        // Assert while 6h range is loading: previous rows stay on screen, dimmed, spinner on range control, no skeleton
        await expect(rangeGroup).toHaveAttribute("aria-busy", "true");
        await expect(rangeGroup.getByRole("status", { name: "Loading timeline range", exact: true })).toBeVisible();
        await expect(timelineSection.locator("[data-timeline-list]")).toHaveClass(/opacity-60/);
        await expect(timelineSection.getByLabel("Loading projected change history")).toHaveCount(0);
        expect(await timelineList.getByRole("listitem").count()).toBeGreaterThan(0);
        expect(await timelineList.evaluate((el) => (el as HTMLElement).dataset.qaKeepInstance)).toBe("timeline-list-1");

        await saveCapture(page, "k17-timeline-range", vp.width, theme, "loading");

        await timelineRangeGate.release();
        await page.unroute("**/api/admin/kubernetes/timeline?*", timelineRangeGate.handler);

        await expect(rangeGroup).toHaveAttribute("aria-busy", "false");
        await expect(rangeGroup.getByRole("status", { name: "Loading timeline range", exact: true })).toHaveCount(0);
        await expect(timelineSection.locator("[data-timeline-list]")).not.toHaveClass(/opacity-60/);
        expect(await timelineList.evaluate((el) => (el as HTMLElement).dataset.qaKeepInstance)).toBe("timeline-list-1");

        // Cycle 15m, 24h, and back to 1h confirming the same list instance stays mounted
        for (const label of ["15m", "24h", "1h"] as const) {
          await rangeGroup.getByRole("button", { name: label, exact: true }).click();
          await expect(rangeGroup.getByRole("button", { name: label, exact: true })).toHaveAttribute("aria-pressed", "true");
          await expect(rangeGroup).toHaveAttribute("aria-busy", "false");
          expect(await timelineList.evaluate((el) => (el as HTMLElement).dataset.qaKeepInstance)).toBe("timeline-list-1");
        }

        // Breaks only toggle (URL breaks=1)
        const eventToggle = timelineSection.getByRole("tablist", { name: "Timeline events", exact: true });
        const allTab = eventToggle.getByRole("tab", { name: "All", exact: true });
        const breaksOnlyTab = eventToggle.getByRole("tab", { name: "Breaks only", exact: true });
        await expect(allTab).toHaveAttribute("aria-selected", "true");
        await expect(breaksOnlyTab).toHaveAttribute("aria-selected", "false");

        await breaksOnlyTab.click();
        await expect(page).toHaveURL(/breaks=1/);
        await expect(breaksOnlyTab).toHaveAttribute("aria-selected", "true");
        await expect(timelineSection.getByText(/^\d+ breaks? in 1h$/)).toBeVisible();
        await expect(timelineList.getByRole("link", { name: "Open break details" }).first()).toBeVisible({ timeout: 15_000 });
        await expect(timelineList.getByRole("button", { name: "Open change details" })).toHaveCount(0);
        await expect(timelineList.getByRole("link", { name: "Open crash details" })).toHaveCount(0);

        await saveCapture(page, "k17-timeline-breaks-only", vp.width, theme, "active");

        // --- 5. Break detail first open (no layout jump), filter bar, range change, container dropdown, and side panel ---
        // Hold break-logs request so we can capture & measure before data arrives, then release and compare after data arrives
        const breakLogsGate = createRouteBarrier();
        await page.route("**/api/admin/kubernetes/break-logs?*", breakLogsGate.handler);

        // Filter to load-0000 so app-00000 is the top break row
        const timelineNs = timelineSection.getByRole("combobox", { name: "Timeline namespace", exact: true });
        await timelineNs.focus();
        await timelineNs.fill("load-0000");
        await page.keyboard.press("Enter");
        await expect(timelineNs).toHaveValue("load-0000");

        const appBreakLink = timelineList.getByRole("link", { name: "Open break details" }).first();
        await appBreakLink.click({ force: true });
        await breakLogsGate.hit;

        await expect(page).toHaveURL(/root_uid=uid-app-00000/);

        // Detail filter bar has no namespace or kind selectors
        await expect(timelineSection.getByRole("combobox", { name: "Timeline namespace" })).toHaveCount(0);
        await expect(timelineSection.getByRole("combobox", { name: "Timeline kind" })).toHaveCount(0);
        await expect(timelineSection.getByText("Deployment load-0000/app-00000", { exact: true })).toBeVisible();
        await expect(timelineSection.getByRole("link", { name: "Clear root filter", exact: true })).toBeVisible();
        await expect(timelineSection.getByText(/^centred \d{2}:\d{2}:\d{2}$/)).toBeVisible();

        // Wait for chart lane and Logs at break section to be mounted while break-logs is still in flight
        await expect(timelineSection.getByRole("region", { name: "Timeline lane app-00000", exact: true })).toBeVisible({ timeout: 20_000 });
        const breakLogsSection = timelineSection.getByRole("region", { name: "Logs at break", exact: true });
        await expect(breakLogsSection).toBeVisible();
        await breakLogsSection.scrollIntoViewIfNeeded();

        const logPane = breakLogsSection.locator("[data-break-log-pane]");
        const sidePanel = breakLogsSection.locator("[data-break-log-side]");
        await expect(logPane).toBeVisible();
        await expect(sidePanel).toBeVisible();
        await expect(logPane).toHaveAttribute("aria-busy", "true");
        await expect(sidePanel).toHaveAttribute("aria-busy", "true");

        // Verify spinner sits next to Ask chat and measure bounding boxes BEFORE data arrives
        const beforeBoxes = await page.evaluate(() => {
          const pane = document.querySelector<HTMLElement>("[data-break-log-pane]")!;
          const side = document.querySelector<HTMLElement>("[data-break-log-side]")!;
          const askButtons = [...document.querySelectorAll<HTMLElement>("button")].filter(
            (btn) => btn.textContent?.trim() === "Ask chat" && btn.getBoundingClientRect().width > 0,
          );
          const ask = askButtons[0]!;
          const spinnerWrap = ask.previousElementSibling as HTMLElement | null;
          const spinnerSvg = spinnerWrap?.querySelector("svg") ?? null;
          const pBox = pane.getBoundingClientRect();
          const sBox = side.getBoundingClientRect();
          const aBox = ask.getBoundingClientRect();
          const spBox = spinnerWrap?.getBoundingClientRect() ?? null;
          return {
            pane: { top: pBox.top, bottom: pBox.bottom, left: pBox.left, width: pBox.width, height: pBox.height },
            side: { top: sBox.top, bottom: sBox.bottom, left: sBox.left, width: sBox.width, height: sBox.height },
            ask: { top: aBox.top, bottom: aBox.bottom, left: aBox.left, width: aBox.width, height: aBox.height },
            hasSpinnerAttr: spinnerWrap?.hasAttribute("data-break-log-spinner") ?? false,
            hasSpinningSvg: Boolean(spinnerSvg),
            spinnerLeftOfAsk: spBox ? spBox.right <= aBox.left + 12 && Math.abs((spBox.top + spBox.height / 2) - (aBox.top + aBox.height / 2)) <= 6 : false,
          };
        });

        expect(beforeBoxes.hasSpinnerAttr).toBe(true);
        expect(beforeBoxes.hasSpinningSvg).toBe(true);
        expect(beforeBoxes.spinnerLeftOfAsk).toBe(true);
        expect(Math.abs(beforeBoxes.pane.height - beforeBoxes.side.height)).toBeLessThanOrEqual(1);

        await saveCapture(page, "k17-break-detail", vp.width, theme, "before-data");

        // Release break-logs barrier so data arrives
        await breakLogsGate.release();
        await page.unroute("**/api/admin/kubernetes/break-logs?*", breakLogsGate.handler);

        await expect(logPane).toHaveAttribute("aria-busy", "false", { timeout: 20_000 });
        await expect(sidePanel).toHaveAttribute("aria-busy", "false");
        await expect(logPane).toContainText("upstream payments timeout after 5000 ms");

        // Measure bounding boxes AFTER data arrives and compare against beforeBoxes
        const afterBoxes = await page.evaluate(() => {
          const pane = document.querySelector<HTMLElement>("[data-break-log-pane]")!;
          const side = document.querySelector<HTMLElement>("[data-break-log-side]")!;
          const askButtons = [...document.querySelectorAll<HTMLElement>("button")].filter(
            (btn) => btn.textContent?.trim() === "Ask chat" && btn.getBoundingClientRect().width > 0,
          );
          const ask = askButtons[0]!;
          const spinnerWrap = ask.previousElementSibling as HTMLElement | null;
          const spinnerSvg = spinnerWrap?.querySelector("svg") ?? null;
          const pBox = pane.getBoundingClientRect();
          const sBox = side.getBoundingClientRect();
          const aBox = ask.getBoundingClientRect();
          return {
            pane: { top: pBox.top, bottom: pBox.bottom, left: pBox.left, width: pBox.width, height: pBox.height },
            side: { top: sBox.top, bottom: sBox.bottom, left: sBox.left, width: sBox.width, height: sBox.height },
            ask: { top: aBox.top, bottom: aBox.bottom, left: aBox.left, width: aBox.width, height: aBox.height },
            hasSpinningSvg: Boolean(spinnerSvg),
          };
        });

        expect(afterBoxes.hasSpinningSvg).toBe(false);
        expect(Math.abs(afterBoxes.pane.height - beforeBoxes.pane.height)).toBeLessThanOrEqual(1);
        expect(Math.abs(afterBoxes.side.height - beforeBoxes.side.height)).toBeLessThanOrEqual(1);
        expect(Math.abs(afterBoxes.pane.height - afterBoxes.side.height)).toBeLessThanOrEqual(1);
        expect(Math.abs(afterBoxes.pane.top - beforeBoxes.pane.top), "Log pane top edge must not jump when break-logs data arrives").toBeLessThanOrEqual(1);
        expect(Math.abs(afterBoxes.pane.bottom - beforeBoxes.pane.bottom), "Log pane bottom edge must not jump when break-logs data arrives").toBeLessThanOrEqual(1);
        expect(Math.abs(afterBoxes.side.top - beforeBoxes.side.top), "Side panel top edge must not jump when break-logs data arrives").toBeLessThanOrEqual(1);
        expect(Math.abs(afterBoxes.side.bottom - beforeBoxes.side.bottom), "Side panel bottom edge must not jump when break-logs data arrives").toBeLessThanOrEqual(1);
        expect(Math.abs(afterBoxes.ask.top - beforeBoxes.ask.top)).toBeLessThanOrEqual(1);
        expect(Math.abs(afterBoxes.ask.left - beforeBoxes.ask.left)).toBeLessThanOrEqual(1);
        expect(Math.abs(afterBoxes.ask.width - beforeBoxes.ask.width)).toBeLessThanOrEqual(1);
        expect(Math.abs(afterBoxes.ask.height - beforeBoxes.ask.height)).toBeLessThanOrEqual(1);

        await saveCapture(page, "k17-break-detail", vp.width, theme, "after-data");

        // Break detail range change: keeps chart and logs mounted, dims chart area, shows spinner on range control
        const detailChartArea = timelineSection.locator("[data-timeline-chart-area]");
        await detailChartArea.evaluate((el) => { (el as HTMLElement).dataset.qaDetailChart = "kept"; });
        const detailRangeGroup = timelineSection.getByRole("group", { name: "Timeline window", exact: true });
        const detailChangesGate = createRouteBarrier();
        await page.route("**/api/admin/kubernetes/changes?*", detailChangesGate.handler);

        await detailRangeGroup.getByRole("button", { name: "6h", exact: true }).click();
        await detailChangesGate.hit;

        await expect(detailRangeGroup).toHaveAttribute("aria-busy", "true");
        await expect(detailRangeGroup.getByRole("status", { name: "Loading timeline range", exact: true })).toBeVisible();
        await expect(detailChartArea).toHaveClass(/opacity-60/);
        await expect(timelineSection.getByLabel("Loading related root change history")).toHaveCount(0);
        expect(await detailChartArea.evaluate((el) => (el as HTMLElement).dataset.qaDetailChart)).toBe("kept");

        await saveCapture(page, "k17-break-detail", vp.width, theme, "range-loading");

        await detailChangesGate.release();
        await page.unroute("**/api/admin/kubernetes/changes?*", detailChangesGate.handler);
        await expect(detailRangeGroup).toHaveAttribute("aria-busy", "false");
        await expect(detailChartArea).not.toHaveClass(/opacity-60/);

        // Switch break detail range back to 15m so markers on the chart are spread out
        await detailRangeGroup.getByRole("button", { name: "15m", exact: true }).click();
        await expect(detailRangeGroup.getByRole("button", { name: "15m", exact: true })).toHaveAttribute("aria-pressed", "true");
        await expect(detailRangeGroup).toHaveAttribute("aria-busy", "false");

        // Top messages capped at 5 with 2-line clamp + hover title, Show all scrolling inside the panel, matching log pane height
        const topMessagesRegion = sidePanel.getByRole("region", { name: "Top messages", exact: true });
        const sourcesRegion = sidePanel.getByRole("region", { name: "Sources", exact: true });
        await expect(topMessagesRegion).toBeVisible();
        await expect(sourcesRegion).toBeVisible();
        await expect(topMessagesRegion.getByText("9 unique", { exact: true })).toBeVisible();
        await expect(topMessagesRegion.getByRole("listitem")).toHaveCount(5);

        const firstMsgText = topMessagesRegion.getByRole("listitem").first().locator("span[title]");
        await expect(firstMsgText).toHaveClass(/line-clamp-2/);
        const fullTitle = await firstMsgText.getAttribute("title");
        expect(fullTitle).toContain("upstream payments timeout after");
        await firstMsgText.hover();

        const showAllMessagesBtn = topMessagesRegion.getByRole("button", { name: "Show all (9)", exact: true });
        await expect(showAllMessagesBtn).toBeVisible();
        await showAllMessagesBtn.click();
        await expect(topMessagesRegion.getByRole("listitem")).toHaveCount(9);

        const sideScrollInfo = await page.evaluate(() => {
          const pane = document.querySelector<HTMLElement>("[data-break-log-pane]")!;
          const side = document.querySelector<HTMLElement>("[data-break-log-side]")!;
          return {
            paneHeight: pane.getBoundingClientRect().height,
            sideHeight: side.getBoundingClientRect().height,
            overflowY: getComputedStyle(side).overflowY,
            scrollHeight: side.scrollHeight,
            clientHeight: side.clientHeight,
          };
        });
        expect(Math.abs(sideScrollInfo.paneHeight - sideScrollInfo.sideHeight)).toBeLessThanOrEqual(1);
        expect(sideScrollInfo.overflowY).toBe("auto");
        expect(sideScrollInfo.scrollHeight).toBeGreaterThan(sideScrollInfo.clientHeight);

        await saveCapture(page, "k17-break-detail", vp.width, theme, "side-panel-expanded");

        // Container dropdown defaults to main container ("app") and switches to "istio-proxy" and "All containers"
        const containerSelect = breakLogsSection.getByRole("combobox", { name: "Log container", exact: true });
        await expect(containerSelect).toBeVisible();
        await expect(containerSelect).toHaveValue("app");
        await expect(breakLogsSection.getByText("Main container:", { exact: true })).toBeVisible();
        const containerOptions = await containerSelect.locator("option").allTextContents();
        expect(containerOptions).toEqual(["app (main)", "istio-proxy", "init: migrate", "All containers"]);

        await containerSelect.selectOption("istio-proxy");
        await expect(breakLogsSection.getByText("Container:", { exact: true })).toBeVisible();
        await expect(logPane).toContainText("envoy upstream connect reset before headers from local app container", { timeout: 15_000 });
        await saveCapture(page, "k17-break-detail", vp.width, theme, "container-switched");

        await containerSelect.selectOption("*");
        await expect(logPane).toContainText("upstream payments timeout after 5000 ms", { timeout: 15_000 });
        await expect(logPane).toContainText("envoy upstream connect reset before headers from local app container");

        // Verify "Logs before crash" on sidecar crash marker selects the crashed container (URL logs_container=istio-proxy)
        const proxyCrashMarker = timelineSection.getByRole("button", { name: /istio-proxy: Error/i }).first();
        if (await proxyCrashMarker.isVisible()) {
          await proxyCrashMarker.dispatchEvent("click");
          const eventDetails = page.getByRole("region", { name: "Timeline event details", exact: true });
          await expect(eventDetails).toBeVisible();
          await eventDetails.getByRole("button", { name: "Logs before crash", exact: true }).click();
          await expect(page).toHaveURL(/logs_container=istio-proxy/);
          await expect(containerSelect).toHaveValue("istio-proxy");
        }

        // Also verify Sources capped at 5 with Show all (7) scrolling inside the panel when >5 sources are present
        // Wait for the previous break-logs response before re-registering the
        // route, so no cancelled request is in flight when the augment handler
        // takes over.
        await expect(logPane).toHaveAttribute("aria-busy", "false", { timeout: 20_000 });
        // A container switch can still cancel a break-logs request. Playwright
        // then reports the route as already handled when the augmented response
        // is ready; that response has no client, so it is dropped. A failure on
        // a live request still propagates, and the counter keeps the guard from
        // hiding a total loss of augmentation.
        let augmentedSources = 0;
        const sourcesAugmentHandler = async (route: import("@playwright/test").Route) => {
          try {
            const response = await route.fetch();
            const json = await response.json();
            json.sources = Array.from({ length: 7 }, (_, idx) => ({
              kind: "linked",
              name: `loki-cluster-sink-${idx + 1}`,
              type: "loki",
              at: new Date().toISOString(),
            }));
            await route.fulfill({ response, json });
            augmentedSources += 1;
          } catch (error) {
            if (String(error).includes("Route is already handled")) return;
            throw error;
          }
        };
        await page.route("**/api/admin/kubernetes/break-logs?*", sourcesAugmentHandler);
        await containerSelect.selectOption("app");
        await expect(sourcesRegion.getByText("7 sources", { exact: true })).toBeVisible({ timeout: 15_000 });
        await expect(sourcesRegion.getByRole("listitem")).toHaveCount(5);
        const showAllSourcesBtn = sourcesRegion.getByRole("button", { name: "Show all (7)", exact: true });
        await expect(showAllSourcesBtn).toBeVisible();
        await showAllSourcesBtn.click();
        await expect(sourcesRegion.getByRole("listitem")).toHaveCount(7);
        expect(augmentedSources).toBeGreaterThan(0);
        const sideBoxWithAllSources = await sidePanel.boundingBox();
        const paneBoxWithAllSources = await logPane.boundingBox();
        expect(Math.abs(sideBoxWithAllSources!.height - paneBoxWithAllSources!.height)).toBeLessThanOrEqual(1);
        await page.unroute("**/api/admin/kubernetes/break-logs?*", sourcesAugmentHandler);

        // --- 6. Related map spacing and routing: no line crosses a box ---
        await tabs.nth(1).click(); // Workloads
        await expect(page).toHaveURL(/view=resources/);
        const workloadNs = page.getByRole("combobox", { name: "Workload namespace", exact: true });
        await workloadNs.focus();
        await workloadNs.fill("load-0000");
        await page.keyboard.press("Enter");
        await expect(workloadNs).toHaveValue("load-0000");

        await page.getByRole("button", { name: /Select Deployment load-0000\/app-00000/ }).click();
        const drawer = page.getByRole("dialog", { name: "Details panel" });
        await expect(drawer).toBeVisible();
        await openDrawerTab(page, drawer, "related", "Related");

        const relationshipSvg = drawer.getByRole("img", { name: "Directed resource relationships", exact: true });
        await expect(relationshipSvg).toBeVisible({ timeout: 20_000 });
        await expect(drawer.getByRole("button", { name: "Open Secret common-secrets", exact: true })).toBeVisible();

        await verifyRelationshipMapGeometry(relationshipSvg);
        await saveCapture(page, "k17-related-map", vp.width, theme, "deployment");

        // Also open Pod app-00000-rs-00 (3-node ownership chain + multi-row dependency grid) and verify no line crosses a box
        await drawer.getByRole("button", { name: "Open Pod app-00000-rs-00", exact: true }).first().click();
        await expect(drawer).toContainText("app-00000-rs-00");
        await openDrawerTab(page, drawer, "related", "Related");
        await expect(relationshipSvg).toBeVisible({ timeout: 20_000 });
        await expect(drawer.getByRole("button", { name: "Open Deployment app-00000", exact: true }).first()).toBeVisible();

        await verifyRelationshipMapGeometry(relationshipSvg);
        await saveCapture(page, "k17-related-map", vp.width, theme, "pod");

        await drawer.getByRole("button", { name: "Close panel", exact: true }).click();
        await expect(drawer).toHaveCount(0);

        expect(pageErrors).toEqual([]);
        await context.close();
      });
    }
  }

  test("switching to Timeline via tab bar then clicking Breaks only keeps Timeline tab open", async ({ browser }) => {
    const context = await browser.newContext({ viewport: { width: 1440, height: 900 } });
    const page = await context.newPage();
    await applyTheme(page, "dark");
    await openApp(page, "/agent/kubernetes");
    await expect(page.getByRole("region", { name: "Cluster health", exact: true })).toBeVisible({ timeout: 20_000 });

    const tabs = page.getByRole("tablist", { name: "Kubernetes views", exact: true }).getByRole("tab");
    await tabs.nth(3).click();
    await expect(page).toHaveURL(/view=timeline/);
    const timelineSection = page.getByRole("region", { name: "Kubernetes change timeline", exact: true });
    await expect(timelineSection).toBeVisible();

    const eventToggle = timelineSection.getByRole("tablist", { name: "Timeline events", exact: true });
    await eventToggle.getByRole("tab", { name: "Breaks only", exact: true }).click();
    await expect(page).toHaveURL(/view=timeline/);
    await expect(page).toHaveURL(/breaks=1/);
    await expect(timelineSection).toBeVisible();
    await context.close();
  });
});

test.describe("K17 delta recheck for QA-189 and QA-190", () => {
  test.skip(mode !== "recheck", "runs in isolated recheck mode");
  test.setTimeout(240_000);

  for (const vp of viewports) {
    for (const theme of themes) {
      test(`QA-189 and QA-190 delta recheck at ${vp.width}px (${theme})`, async ({ browser }) => {
        const context = await browser.newContext({ viewport: { width: vp.width, height: vp.height } });
        const page = await context.newPage();
        const pageErrors: string[] = [];
        page.on("pageerror", (err) => pageErrors.push(String(err)));

        await applyTheme(page, theme);

        // --- 1. QA-189: Deep links for Timeline+Breaks only and Workloads+Resource drawer ---
        await openApp(page, "/agent/kubernetes?view=timeline&breaks=1");
        await expect.poll(async () => {
          const res = await page.request.get("/api/admin/kubernetes/overview");
          if (!res.ok()) return null;
          const body = await res.json();
          return body.sync?.state === "ready" && (body.sync?.pending_kinds ?? 0) === 0 ? body : null;
        }, { timeout: 60_000, intervals: [500, 1000] }).not.toBeNull();

        const viewTabs = page.getByRole("tablist", { name: "Kubernetes views", exact: true });
        await expect(viewTabs.getByRole("tab", { name: "Timeline", exact: true })).toHaveAttribute("aria-selected", "true");
        const timelineSection = page.getByRole("region", { name: "Kubernetes change timeline", exact: true });
        await expect(timelineSection).toBeVisible({ timeout: 20_000 });
        await expect(timelineSection.getByRole("tablist", { name: "Timeline events", exact: true }).getByRole("tab", { name: "Breaks only", exact: true })).toHaveAttribute("aria-selected", "true");

        await openApp(page, "/agent/kubernetes?view=resources&r=apps~v1~deployments/load-0000/app-00000&tab=logs");
        await expect(viewTabs.getByRole("tab", { name: "Workloads", exact: true })).toHaveAttribute("aria-selected", "true");
        const deepLinkDrawer = page.getByRole("dialog", { name: "Details panel" });
        await expect(deepLinkDrawer).toBeVisible({ timeout: 20_000 });
        await expect(deepLinkDrawer).toContainText("app-00000");
        await expect(deepLinkDrawer.locator("#kubernetes-drawer-panel")).toHaveAttribute("aria-labelledby", "kubernetes-drawer-tab-logs");
        await deepLinkDrawer.getByRole("button", { name: "Close panel", exact: true }).click();
        await expect(deepLinkDrawer).toHaveCount(0);

        // --- 2. QA-189: Tab-bar navigation, resource drawer open/close, and Back/Forward history ---
        await openApp(page, "/agent/kubernetes");
        await expect(page.getByRole("region", { name: "Cluster health", exact: true })).toBeVisible({ timeout: 20_000 });
        await expect(viewTabs.getByRole("tab", { name: "Overview", exact: true })).toHaveAttribute("aria-selected", "true");

        // Switch Overview -> Workloads via tab bar
        await viewTabs.getByRole("tab", { name: "Workloads", exact: true }).click();
        await expect(page).toHaveURL(/view=resources/);
        await expect(viewTabs.getByRole("tab", { name: "Workloads", exact: true })).toHaveAttribute("aria-selected", "true");

        const workloadNs = page.getByRole("combobox", { name: "Workload namespace", exact: true });
        await workloadNs.focus();
        await workloadNs.fill("load-0000");
        await page.keyboard.press("Enter");
        await expect(workloadNs).toHaveValue("load-0000");

        // Open resource drawer -> switch drawer tab to Logs -> close drawer
        await page.getByRole("button", { name: /Select Deployment load-0000\/app-00000/ }).click();
        const drawer = page.getByRole("dialog", { name: "Details panel" });
        await expect(drawer).toBeVisible();
        expect(new URL(page.url()).searchParams.get("r")).toBe("apps~v1~deployments/load-0000/app-00000");

        await openDrawerTab(page, drawer, "logs", "Logs");
        await expect(page).toHaveURL(/tab=logs/);
        await expect(drawer.locator("#kubernetes-drawer-panel")).toHaveAttribute("aria-labelledby", "kubernetes-drawer-tab-logs");

        await drawer.getByRole("button", { name: "Close panel", exact: true }).click();
        // Closing from a non-overview drawer tab (tab=logs) must close the drawer and drop ?r= and ?tab=.
        // No retry click here: a single close that re-opens the drawer is exactly the QA-189 regression.
        await expect.poll(async () => {
          const params = new URL(page.url()).searchParams;
          return (await drawer.count()) === 0 && !params.has("r") && !params.has("tab");
        }, { timeout: 5_000 }).toBe(true);
        await expect(drawer).toHaveCount(0);
        await expect(page).not.toHaveURL(/[?&]r=/);
        await expect(page).not.toHaveURL(/[?&]tab=/);
        await page.waitForTimeout(200);

        // Browser Back/Forward across drawer close/open and explorer tabs
        await page.goBack();
        await expect(drawer).toBeVisible();
        expect(new URL(page.url()).searchParams.get("r")).toBe("apps~v1~deployments/load-0000/app-00000");

        // Navigate back until drawer closes and we are on Workloads list
        for (let step = 0; step < 3 && new URL(page.url()).searchParams.has("r"); step++) {
          await page.goBack();
          await page.waitForTimeout(150);
        }
        await expect(drawer).toHaveCount(0);
        await expect(viewTabs.getByRole("tab", { name: "Workloads", exact: true })).toHaveAttribute("aria-selected", "true");

        await page.goBack();
        await expect(viewTabs.getByRole("tab", { name: "Overview", exact: true })).toHaveAttribute("aria-selected", "true");
        await expect(page.getByRole("region", { name: "Cluster health", exact: true })).toBeVisible();

        await page.goForward();
        await expect(viewTabs.getByRole("tab", { name: "Workloads", exact: true })).toHaveAttribute("aria-selected", "true");

        await page.goForward();
        await expect(drawer).toBeVisible();
        await drawer.getByRole("button", { name: "Close panel", exact: true }).click();
        await expect(drawer).toHaveCount(0, { timeout: 5_000 });
        await expect(page).not.toHaveURL(/[?&]r=/);

        // --- 3. QA-189: Issues-row Timeline button and Investigate button ---
        await viewTabs.getByRole("tab", { name: "Issues", exact: true }).click();
        await expect(page).toHaveURL(/view=issues/);
        const issueSection = page.getByRole("region", { name: "Kubernetes issues", exact: true });
        await expect(issueSection).toBeVisible();
        const appIssueArticle = issueSection.getByRole("article", { name: "Deployment app-00000", exact: true }).first();
        await expect(appIssueArticle).toBeVisible({ timeout: 20_000 });

        // Click Timeline button on Issues row -> must open Timeline break detail for app-00000
        await appIssueArticle.getByRole("button", { name: "Timeline for app-00000", exact: true }).click();
        await expect(page).toHaveURL(/view=timeline/);
        await expect(page).toHaveURL(/root_uid=uid-app-00000/);
        await expect(viewTabs.getByRole("tab", { name: "Timeline", exact: true })).toHaveAttribute("aria-selected", "true");
        await expect(timelineSection.getByText("Deployment load-0000/app-00000", { exact: true })).toBeVisible();

        // Back returns to Issues tab
        await page.goBack();
        await expect(page).toHaveURL(/view=issues/);
        await expect(viewTabs.getByRole("tab", { name: "Issues", exact: true })).toHaveAttribute("aria-selected", "true");
        await expect(appIssueArticle).toBeVisible();

        // Click Investigate button on Issues row -> must open Chat with right resource params
        await appIssueArticle.getByRole("button", { name: "Investigate app-00000", exact: true }).click();
        await expect(page).toHaveURL(/\/agent\/chat\?/);
        const chatUrl = new URL(page.url());
        expect(chatUrl.searchParams.get("provider")).toBe("kubernetes");
        expect(chatUrl.searchParams.get("cluster")).toBe("prod-eks");
        expect(chatUrl.searchParams.get("resource_id")).toBe("apps~v1~deployments");
        expect(chatUrl.searchParams.get("name")).toBe("app-00000");
        expect(chatUrl.searchParams.get("namespace")).toBe("load-0000");
        expect(chatUrl.searchParams.get("uid")).toBe("uid-app-00000");
        expect(chatUrl.searchParams.get("include_changes")).toBe("true");

        // Go back from Chat to Issues, then switch via tab bar to Timeline, then click Breaks only
        await page.goBack();
        await expect(page).toHaveURL(/view=issues/);
        await expect(viewTabs.getByRole("tab", { name: "Issues", exact: true })).toHaveAttribute("aria-selected", "true");

        await viewTabs.getByRole("tab", { name: "Timeline", exact: true }).click();
        await expect(page).toHaveURL(/view=timeline/);
        await expect(viewTabs.getByRole("tab", { name: "Timeline", exact: true })).toHaveAttribute("aria-selected", "true");
        await expect(timelineSection).toBeVisible();

        const timelineList = timelineSection.getByRole("list", { name: "Breaks and changes", exact: true });
        await expect(timelineList.getByRole("listitem").first()).toBeVisible({ timeout: 20_000 });

        const eventToggle = timelineSection.getByRole("tablist", { name: "Timeline events", exact: true });
        const breaksOnlyTab = eventToggle.getByRole("tab", { name: "Breaks only", exact: true });
        await breaksOnlyTab.click();

        // QA-189 core assertion: URL keeps view=timeline&breaks=1 and Timeline tab stays active
        await expect(page).toHaveURL(/view=timeline/);
        await expect(page).toHaveURL(/breaks=1/);
        const timelineBreaksParams = new URL(page.url()).searchParams;
        expect(timelineBreaksParams.get("view")).toBe("timeline");
        expect(timelineBreaksParams.get("breaks")).toBe("1");
        await expect(viewTabs.getByRole("tab", { name: "Timeline", exact: true })).toHaveAttribute("aria-selected", "true");
        await expect(breaksOnlyTab).toHaveAttribute("aria-selected", "true");
        await expect(timelineSection.getByText(/^\d+ breaks? in 1h$/)).toBeVisible();
        await expect(timelineList.getByRole("link", { name: "Open break details" }).first()).toBeVisible({ timeout: 15_000 });
        await expect(timelineList.getByRole("button", { name: "Open change details" })).toHaveCount(0);

        await saveCapture(page, "k17-timeline-breaks-only", vp.width, theme, "active");

        // --- 4. QA-190: Break detail pane and side-panel edges don't move between loading and loaded; filter row is fixed height and scrolls sideways ---
        const breakLogsGate = createRouteBarrier();
        await page.route("**/api/admin/kubernetes/break-logs?*", breakLogsGate.handler);

        const timelineNs = timelineSection.getByRole("combobox", { name: "Timeline namespace", exact: true });
        await timelineNs.focus();
        await timelineNs.fill("load-0000");
        await page.keyboard.press("Enter");
        await expect(timelineNs).toHaveValue("load-0000");

        const appBreakLink = timelineList.getByRole("link", { name: "Open break details" }).first();
        await appBreakLink.click({ force: true });
        await breakLogsGate.hit;

        await expect(page).toHaveURL(/root_uid=uid-app-00000/);
        await expect(timelineSection.getByRole("region", { name: "Timeline lane app-00000", exact: true })).toBeVisible({ timeout: 20_000 });
        const breakLogsSection = timelineSection.getByRole("region", { name: "Logs at break", exact: true });
        await expect(breakLogsSection).toBeVisible();
        await breakLogsSection.scrollIntoViewIfNeeded();

        const logPane = breakLogsSection.locator("[data-break-log-pane]");
        const sidePanel = breakLogsSection.locator("[data-break-log-side]");
        const filterRow = breakLogsSection.locator("[data-break-log-filters]");
        await expect(logPane).toBeVisible();
        await expect(sidePanel).toBeVisible();
        await expect(filterRow).toBeVisible();
        await expect(logPane).toHaveAttribute("aria-busy", "true");
        await expect(sidePanel).toHaveAttribute("aria-busy", "true");

        const beforeBoxes = await page.evaluate(() => {
          const pane = document.querySelector<HTMLElement>("[data-break-log-pane]")!;
          const side = document.querySelector<HTMLElement>("[data-break-log-side]")!;
          const filters = document.querySelector<HTMLElement>("[data-break-log-filters]")!;
          const askButtons = [...document.querySelectorAll<HTMLElement>("button")].filter(
            (btn) => btn.textContent?.trim() === "Ask chat" && btn.getBoundingClientRect().width > 0,
          );
          const ask = askButtons[0]!;
          const spinnerWrap = ask.previousElementSibling as HTMLElement | null;
          const spinnerSvg = spinnerWrap?.querySelector("svg") ?? null;
          const pBox = pane.getBoundingClientRect();
          const sBox = side.getBoundingClientRect();
          const fBox = filters.getBoundingClientRect();
          const aBox = ask.getBoundingClientRect();
          const spBox = spinnerWrap?.getBoundingClientRect() ?? null;
          const fStyle = getComputedStyle(filters);
          return {
            pane: { top: pBox.top, bottom: pBox.bottom, left: pBox.left, right: pBox.right, width: pBox.width, height: pBox.height },
            side: { top: sBox.top, bottom: sBox.bottom, left: sBox.left, right: sBox.right, width: sBox.width, height: sBox.height },
            filters: { top: fBox.top, bottom: fBox.bottom, height: fBox.height, flexWrap: fStyle.flexWrap, overflowX: fStyle.overflowX, overflowY: fStyle.overflowY },
            ask: { top: aBox.top, bottom: aBox.bottom, left: aBox.left, width: aBox.width, height: aBox.height },
            hasSpinnerAttr: spinnerWrap?.hasAttribute("data-break-log-spinner") ?? false,
            hasSpinningSvg: Boolean(spinnerSvg),
            spinnerLeftOfAsk: spBox ? spBox.right <= aBox.left + 12 && Math.abs((spBox.top + spBox.height / 2) - (aBox.top + aBox.height / 2)) <= 6 : false,
          };
        });

        expect(beforeBoxes.hasSpinnerAttr).toBe(true);
        expect(beforeBoxes.hasSpinningSvg).toBe(true);
        expect(beforeBoxes.spinnerLeftOfAsk).toBe(true);
        expect(beforeBoxes.filters.flexWrap).toBe("nowrap");
        expect(beforeBoxes.filters.overflowX).toBe("auto");
        expect(beforeBoxes.filters.overflowY).toBe("hidden");
        expect(Math.abs(beforeBoxes.filters.height - 56)).toBeLessThanOrEqual(1);
        expect(Math.abs(beforeBoxes.pane.height - beforeBoxes.side.height)).toBeLessThanOrEqual(1);

        await saveCapture(page, "k17-break-detail", vp.width, theme, "before-data");

        await breakLogsGate.release();
        await page.unroute("**/api/admin/kubernetes/break-logs?*", breakLogsGate.handler);

        await expect(logPane).toHaveAttribute("aria-busy", "false", { timeout: 20_000 });
        await expect(sidePanel).toHaveAttribute("aria-busy", "false");
        await expect(logPane).toContainText("upstream payments timeout after 5000 ms");

        const afterBoxes = await page.evaluate(() => {
          const pane = document.querySelector<HTMLElement>("[data-break-log-pane]")!;
          const side = document.querySelector<HTMLElement>("[data-break-log-side]")!;
          const filters = document.querySelector<HTMLElement>("[data-break-log-filters]")!;
          const askButtons = [...document.querySelectorAll<HTMLElement>("button")].filter(
            (btn) => btn.textContent?.trim() === "Ask chat" && btn.getBoundingClientRect().width > 0,
          );
          const ask = askButtons[0]!;
          const spinnerWrap = ask.previousElementSibling as HTMLElement | null;
          const spinnerSvg = spinnerWrap?.querySelector("svg") ?? null;
          const pBox = pane.getBoundingClientRect();
          const sBox = side.getBoundingClientRect();
          const fBox = filters.getBoundingClientRect();
          const aBox = ask.getBoundingClientRect();
          const fStyle = getComputedStyle(filters);

          // Test horizontal scrolling of the filter row
          const initialScrollLeft = filters.scrollLeft;
          filters.scrollLeft = 40;
          const scrolledLeft = filters.scrollLeft;
          filters.scrollLeft = initialScrollLeft;

          return {
            pane: { top: pBox.top, bottom: pBox.bottom, left: pBox.left, right: pBox.right, width: pBox.width, height: pBox.height },
            side: { top: sBox.top, bottom: sBox.bottom, left: sBox.left, right: sBox.right, width: sBox.width, height: sBox.height },
            filters: {
              top: fBox.top,
              bottom: fBox.bottom,
              height: fBox.height,
              flexWrap: fStyle.flexWrap,
              overflowX: fStyle.overflowX,
              overflowY: fStyle.overflowY,
              scrollWidth: filters.scrollWidth,
              clientWidth: filters.clientWidth,
              scrolledLeft,
            },
            ask: { top: aBox.top, bottom: aBox.bottom, left: aBox.left, width: aBox.width, height: aBox.height },
            hasSpinningSvg: Boolean(spinnerSvg),
          };
        });

        expect(afterBoxes.hasSpinningSvg).toBe(false);
        // Filter row fixed height & horizontal scroll
        expect(afterBoxes.filters.flexWrap).toBe("nowrap");
        expect(afterBoxes.filters.overflowX).toBe("auto");
        expect(afterBoxes.filters.overflowY).toBe("hidden");
        expect(Math.abs(afterBoxes.filters.height - beforeBoxes.filters.height)).toBeLessThanOrEqual(1);
        expect(Math.abs(afterBoxes.filters.top - beforeBoxes.filters.top)).toBeLessThanOrEqual(1);
        expect(Math.abs(afterBoxes.filters.bottom - beforeBoxes.filters.bottom)).toBeLessThanOrEqual(1);
        if (vp.width === 390 || afterBoxes.filters.scrollWidth > afterBoxes.filters.clientWidth) {
          expect(afterBoxes.filters.scrollWidth).toBeGreaterThan(afterBoxes.filters.clientWidth);
          expect(afterBoxes.filters.scrolledLeft).toBeGreaterThan(0);
        }

        // Zero edge shift on log pane, side panel, and Ask chat between loading and loaded
        expect(Math.abs(afterBoxes.pane.top - beforeBoxes.pane.top)).toBeLessThanOrEqual(1);
        expect(Math.abs(afterBoxes.pane.bottom - beforeBoxes.pane.bottom)).toBeLessThanOrEqual(1);
        expect(Math.abs(afterBoxes.pane.left - beforeBoxes.pane.left)).toBeLessThanOrEqual(1);
        expect(Math.abs(afterBoxes.pane.right - beforeBoxes.pane.right)).toBeLessThanOrEqual(1);
        expect(Math.abs(afterBoxes.pane.width - beforeBoxes.pane.width)).toBeLessThanOrEqual(1);
        expect(Math.abs(afterBoxes.pane.height - beforeBoxes.pane.height)).toBeLessThanOrEqual(1);

        expect(Math.abs(afterBoxes.side.top - beforeBoxes.side.top)).toBeLessThanOrEqual(1);
        expect(Math.abs(afterBoxes.side.bottom - beforeBoxes.side.bottom)).toBeLessThanOrEqual(1);
        expect(Math.abs(afterBoxes.side.left - beforeBoxes.side.left)).toBeLessThanOrEqual(1);
        expect(Math.abs(afterBoxes.side.right - beforeBoxes.side.right)).toBeLessThanOrEqual(1);
        expect(Math.abs(afterBoxes.side.width - beforeBoxes.side.width)).toBeLessThanOrEqual(1);
        expect(Math.abs(afterBoxes.side.height - beforeBoxes.side.height)).toBeLessThanOrEqual(1);

        expect(Math.abs(afterBoxes.pane.height - afterBoxes.side.height)).toBeLessThanOrEqual(1);
        expect(Math.abs(afterBoxes.ask.top - beforeBoxes.ask.top)).toBeLessThanOrEqual(1);
        expect(Math.abs(afterBoxes.ask.left - beforeBoxes.ask.left)).toBeLessThanOrEqual(1);
        expect(Math.abs(afterBoxes.ask.width - beforeBoxes.ask.width)).toBeLessThanOrEqual(1);
        expect(Math.abs(afterBoxes.ask.height - beforeBoxes.ask.height)).toBeLessThanOrEqual(1);

        await saveCapture(page, "k17-break-detail", vp.width, theme, "after-data");

        // Re-capture the remaining k17-break-detail states so all published break-detail finals reflect the fixed-height filter bar
        const detailChartArea = timelineSection.locator("[data-timeline-chart-area]");
        const detailRangeGroup = timelineSection.getByRole("group", { name: "Timeline window", exact: true });
        const detailChangesGate = createRouteBarrier();
        await page.route("**/api/admin/kubernetes/changes?*", detailChangesGate.handler);

        await detailRangeGroup.getByRole("button", { name: "6h", exact: true }).click();
        await detailChangesGate.hit;
        await expect(detailRangeGroup).toHaveAttribute("aria-busy", "true");
        await expect(detailChartArea).toHaveClass(/opacity-60/);
        await saveCapture(page, "k17-break-detail", vp.width, theme, "range-loading");

        await detailChangesGate.release();
        await page.unroute("**/api/admin/kubernetes/changes?*", detailChangesGate.handler);
        await expect(detailRangeGroup).toHaveAttribute("aria-busy", "false");

        await detailRangeGroup.getByRole("button", { name: "15m", exact: true }).click();
        await expect(detailRangeGroup.getByRole("button", { name: "15m", exact: true })).toHaveAttribute("aria-pressed", "true");
        await expect(detailRangeGroup).toHaveAttribute("aria-busy", "false");

        const topMessagesRegion = sidePanel.getByRole("region", { name: "Top messages", exact: true });
        await topMessagesRegion.getByRole("button", { name: "Show all (9)", exact: true }).click();
        await expect(topMessagesRegion.getByRole("listitem")).toHaveCount(9);
        await saveCapture(page, "k17-break-detail", vp.width, theme, "side-panel-expanded");

        const containerSelect = breakLogsSection.getByRole("combobox", { name: "Log container", exact: true });
        await containerSelect.selectOption("istio-proxy");
        await expect(logPane).toContainText("envoy upstream connect reset before headers from local app container", { timeout: 15_000 });
        await saveCapture(page, "k17-break-detail", vp.width, theme, "container-switched");

        expect(pageErrors).toEqual([]);
        await context.close();
      });
    }
  }
});

test.describe("QA-189 drawer close from every drawer tab and stream stability", () => {
  test.skip(mode !== "fakekube", "runs in isolated fakekube mode");
  test.setTimeout(300_000);

  const drawerTabs = [
    { id: "overview", param: "" },
    { id: "logs", param: "&tab=logs" },
    { id: "events", param: "&tab=events" },
    { id: "yaml", param: "&tab=yaml" },
    { id: "related", param: "&tab=related" },
    { id: "timeline", param: "&tab=timeline" },
  ] as const;

  for (const vp of viewports) {
    for (const theme of themes) {
      test(`closing the drawer from each tab keeps it closed, Back reopens the same tab, and URL changes never restart the live stream ${vp.width} ${theme}`, async ({ browser }) => {
        const context = await browser.newContext({ viewport: { width: vp.width, height: vp.height } });
        const page = await context.newPage();
        const pageErrors: string[] = [];
        page.on("pageerror", (err) => pageErrors.push(String(err)));
        await applyTheme(page, theme);

        const viewTabs = page.getByRole("tablist", { name: "Kubernetes views", exact: true });
        const drawer = page.getByRole("dialog", { name: "Details panel" });

        await openApp(page, "/agent/kubernetes");
        await expect.poll(async () => {
          const res = await page.request.get("/api/admin/kubernetes/overview");
          if (!res.ok()) return null;
          const body = await res.json();
          return body.sync?.state === "ready" && (body.sync?.pending_kinds ?? 0) === 0 ? body : null;
        }, { timeout: 90_000, intervals: [500, 1000] }).not.toBeNull();

        // --- Live-update stream must not restart for each in-app URL change ---
        // The stream reconnects when the server ends it, so compare against an idle control window:
        // six URL changes must not add a connection per change.
        let streamConnects = 0;
        page.on("request", (request) => {
          if (request.url().includes("/api/admin/kubernetes/stream")) streamConnects += 1;
        });
        const streamBaseline = streamConnects;

        await viewTabs.getByRole("tab", { name: "Workloads", exact: true }).click();
        await expect(page).toHaveURL(/view=resources/);
        const workloadNs = page.getByRole("combobox", { name: "Workload namespace", exact: true });
        await workloadNs.focus();
        await workloadNs.fill("load-0000");
        await page.keyboard.press("Enter");
        await page.getByRole("button", { name: /Select Deployment load-0000\/app-00000/ }).click();
        await expect(drawer).toBeVisible();
        await openDrawerTab(page, drawer, "events", "Events");
        await openDrawerTab(page, drawer, "logs", "Logs");
        await drawer.getByRole("button", { name: "Close panel", exact: true }).click();
        await expect(drawer).toHaveCount(0, { timeout: 5_000 });
        await viewTabs.getByRole("tab", { name: "Timeline", exact: true }).click();
        await expect(page).toHaveURL(/view=timeline/);
        const timelineSection = page.getByRole("region", { name: "Kubernetes change timeline", exact: true });
        await expect(timelineSection).toBeVisible({ timeout: 20_000 });
        const breaksOnlyTab = timelineSection.getByRole("tablist", { name: "Timeline events", exact: true }).getByRole("tab", { name: "Breaks only", exact: true });
        await breaksOnlyTab.click();
        await expect(page).toHaveURL(/breaks=1/);
        await timelineSection.getByRole("tablist", { name: "Timeline events", exact: true }).getByRole("tab", { name: "All", exact: true }).click();
        await expect(page).not.toHaveURL(/breaks=1/);
        await viewTabs.getByRole("tab", { name: "Overview", exact: true }).click();
        await expect(page).toHaveURL(/view=overview|agent\/kubernetes$/);
        await page.waitForTimeout(500);

        // Six in-app URL changes must not add a stream connection per change (a single natural
        // reconnect is allowed because the server may end the stream).
        const streamDelta = streamConnects - streamBaseline;
        expect(streamDelta, `live-update stream reopened ${streamDelta} times during six in-app URL changes`).toBeLessThanOrEqual(1);

        // --- Close the drawer from every drawer tab ---
        for (const tab of drawerTabs) {
          await openApp(page, `/agent/kubernetes?view=resources&r=apps~v1~deployments/load-0000/app-00000${tab.param}`);
          await expect(drawer).toBeVisible({ timeout: 20_000 });
          await expect(drawer.locator("#kubernetes-drawer-panel")).toHaveAttribute("aria-labelledby", `kubernetes-drawer-tab-${tab.id}`);
          const historyBeforeClose = await page.evaluate(() => history.length);

          await drawer.getByRole("button", { name: "Close panel", exact: true }).click();
          await expect.poll(async () => {
            const params = new URL(page.url()).searchParams;
            return (await drawer.count()) === 0 && !params.has("r") && !params.has("tab");
          }, { timeout: 5_000 }).toBe(true);
          await expect(drawer).toHaveCount(0);
          await expect(page).not.toHaveURL(/[?&]r=/);
          await expect(viewTabs.getByRole("tab", { name: "Workloads", exact: true })).toHaveAttribute("aria-selected", "true");
          expect(await page.evaluate(() => history.length), `closing from ${tab.id} must add exactly one history entry`).toBe(historyBeforeClose + 1);
          await page.waitForTimeout(200);

          // Back reopens the drawer on the same tab.
          await page.goBack();
          await expect(drawer).toBeVisible({ timeout: 10_000 });
          await expect(drawer.locator("#kubernetes-drawer-panel")).toHaveAttribute("aria-labelledby", `kubernetes-drawer-tab-${tab.id}`);
          expect(new URL(page.url()).searchParams.get("r")).toBe("apps~v1~deployments/load-0000/app-00000");

          await drawer.getByRole("button", { name: "Close panel", exact: true }).click();
          await expect(drawer).toHaveCount(0, { timeout: 5_000 });
        }

        if (vp.width === 1440 && theme === "dark") {
          await openApp(page, "/agent/kubernetes?view=resources&r=apps~v1~deployments/load-0000/app-00000&tab=logs");
          await expect(drawer).toBeVisible({ timeout: 20_000 });
          await saveCapture(page, "k17-drawer-close", vp.width, theme, "open-logs");
          await drawer.getByRole("button", { name: "Close panel", exact: true }).click();
          await expect(drawer).toHaveCount(0, { timeout: 5_000 });
          await saveCapture(page, "k17-drawer-close", vp.width, theme, "closed");
        }

        expect(pageErrors).toEqual([]);
        await context.close();
      });
    }
  }
});
