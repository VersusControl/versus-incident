import { expect, test, type Locator, type Page } from "@playwright/test";
import * as fs from "node:fs";
import * as https from "node:https";
import { openApp } from "./helpers";
import { capturePath } from "./support/capture";

const mode = process.env.E2E_K16_MODE || "fakekube";
const secondaryBaseURL = process.env.E2E_SECONDARY_BASE_URL || "";
const fakekubeURL = process.env.E2E_FAKEKUBE_URL || "";
const fakekubeCA = process.env.E2E_FAKEKUBE_CA || "";
const kindNamespace = process.env.E2E_KIND_NAMESPACE || "";

const deletedWarningPatterns = [
  /Cluster visibility warning/i,
  /Overview reached its collection bound/i,
  /\bitem_limit\b/i,
  /\bpage_limit\b/i,
  /collection bound/i,
  /\(partial\)/i,
  /\(stale\)/i,
  /Issue evidence is bounded/i,
  /Node inventory is partial/i,
  /Some resource history is unavailable/i,
  /No recorded changes or crashes in this range/i,
  /Partial history; capture gaps or bounded results/i,
  /Partial history · points sum available Pod samples/i,
  /Partial history; crashes may be missing/i,
  /\(not followed\)/i,
];

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
  for (const pattern of deletedWarningPatterns) {
    expect(bodyText, `Forbidden K16 §3 string matched: ${pattern}`).not.toMatch(pattern);
  }
}

async function saveCapture(page: Page, surface: string, viewport: number, theme: "light" | "dark", state: string) {
  await verifyThemeAndQuietPage(page, theme);
  const out = capturePath(surface, viewport, theme, state);
  await page.screenshot({ path: out, animations: "disabled" });
  return out;
}

async function openDrawerTab(page: Page, dialog: Locator, tabId: "overview" | "logs" | "events" | "yaml" | "related" | "timeline", label: string) {
  const directTab = dialog.locator(`#kubernetes-drawer-tab-${tabId}`);
  if (await directTab.isVisible()) {
    await directTab.click();
    return;
  }
  const moreSummary = dialog.locator("details.resource-more-tabs > summary");
  await expect(moreSummary).toBeVisible();
  await moreSummary.click();
  await dialog.getByRole("menuitem", { name: label, exact: true }).click();
}

const viewports = [
  { name: "desktop", width: 1440, height: 900 },
  { name: "mobile", width: 390, height: 844 },
] as const;

const themes = ["dark", "light"] as const;

test.describe("Kubernetes UI polish K16 fakekube scale and states", () => {
  test.skip(mode !== "fakekube", "runs in fakekube scale mode");
  test.setTimeout(240_000);

  test("first-load syncing state shows skeleton and Syncing cluster note without partial warnings", async ({ browser }) => {
    for (const vp of viewports) {
      for (const theme of themes) {
        const context = await browser.newContext({ viewport: { width: vp.width, height: vp.height } });
        const page = await context.newPage();
        await applyTheme(page, theme);
        await openApp(page, "/agent/kubernetes");
        await expect(page.getByText("Syncing cluster", { exact: true })).toBeVisible({ timeout: 15_000 });
        await expect(page.getByLabel("Loading Kubernetes overview", { exact: true })).toBeVisible();
        await expect(page.getByRole("button", { name: "Resource coverage", exact: true })).toHaveCount(0);
        await saveCapture(page, "k16-first-load", vp.width, theme, "syncing");
        await context.close();
      }
    }
    // Release hold so background sync completes for the 600 Pods / 800 ReplicaSets / 27 Namespaces matrix
    await callFakekube("/_fake/list-holds", { resource: "pods", hold: false, remaining: 0 });
  });

  for (const vp of viewports) {
    for (const theme of themes) {
      test(`U1-U6b, U8-U13, V1-V3 acceptance ${vp.width} ${theme}`, async ({ browser }) => {
        await callFakekube("/_fake/list-holds", { resource: "pods", hold: false, remaining: 0 });
        const context = await browser.newContext({ viewport: { width: vp.width, height: vp.height } });
        const page = await context.newPage();
        const pageErrors: string[] = [];
        page.on("pageerror", (err) => pageErrors.push(err.message));
        await applyTheme(page, theme);

        // Wait for backend 600 Pods / 800 ReplicaSets / 27 Namespaces sync to be ready
        await openApp(page, "/agent/kubernetes");
        await expect.poll(async () => {
          const res = await page.request.get("/api/admin/kubernetes/overview");
          if (!res.ok()) return null;
          const body = await res.json();
          return body.sync?.state === "ready" && body.pods === 600 && body.namespaces === 27 && !body.sync?.partial && !body.truncated ? body : null;
        }, { timeout: 90_000, intervals: [500, 1000, 2000] }).not.toBeNull();

        // Reload or refresh so UI reflects ready state
        await page.getByRole("button", { name: "Refresh Kubernetes data", exact: true }).click();
        await expect(page.getByRole("region", { name: "Cluster health", exact: true })).toBeVisible({ timeout: 30_000 });
        await expect(page.getByText("Syncing cluster", { exact: true })).toHaveCount(0);

        // U11: Tab order has Workloads second
        const tabs = page.getByRole("tablist", { name: "Kubernetes views", exact: true }).getByRole("tab");
        await expect(tabs).toHaveText([
          "Overview",
          "Workloads",
          "Issues",
          "Timeline",
          "Topology",
          "Nodes",
          "Helm",
          "GitOps",
          "Traffic",
          "Agent",
        ]);

        // U1/U2/V3: At most 1 CoverageMarker on Overview (aggregated into TopBar, never duplicated in TimelineCard or BreakRow)
        expect(await page.getByRole("button", { name: "Resource coverage", exact: true }).count()).toBeLessThanOrEqual(1);
        await expect(page.getByRole("region", { name: "Break timeline", exact: true }).getByRole("button", { name: "Resource coverage", exact: true })).toHaveCount(0);

        // U5: Overview Topology card two-column grid, 27 Namespaces, 800 ReplicaSets, 600 Pods, +N kinds not reported, no overlap
        const overviewTopo = page.getByRole("region", { name: "Overview topology", exact: true });
        await overviewTopo.scrollIntoViewIfNeeded();
        await expect(overviewTopo).toBeVisible();
        const reportedList = overviewTopo.locator(`dl[aria-label="Reported resource counts"]`);
        await expect(reportedList).toBeVisible();
        await expect.poll(async () => {
          const text = await reportedList.innerText();
          return text.includes("600") && text.includes("27");
        }, { timeout: 30_000, intervals: [500, 1000] }).toBe(true);
        // Verify all 800 ReplicaSets are cached with zero size truncation
        let rsTotal = 0;
        let rsCursor = "";
        for (let pageIdx = 0; pageIdx < 15; pageIdx++) {
          const rsRes = await page.request.get(`/api/admin/kubernetes/resources?resource_id=apps~v1~replicasets&limit=100${rsCursor ? `&continue=${encodeURIComponent(rsCursor)}` : ""}`);
          expect(rsRes.ok()).toBe(true);
          const rsPage = await rsRes.json();
          expect(rsPage.partial_failures ?? []).toEqual([]);
          rsTotal += (rsPage.items ?? []).length;
          if (!rsPage.continue) break;
          rsCursor = rsPage.continue;
        }
        expect(rsTotal).toBe(800);
        await expect(overviewTopo.getByText(/^\+\d+ kinds not reported$/)).toBeVisible();
        await expect(overviewTopo.getByText("Not reported", { exact: true })).toHaveCount(0);

        const topoLayout = await reportedList.evaluate((dl) => {
          const rows = [...dl.children] as HTMLElement[];
          const values: Record<string, string> = {};
          const overlaps: string[] = [];
          for (const row of rows) {
            const dt = row.querySelector("dt")!;
            const dd = row.querySelector("dd")!;
            const label = dt.textContent?.trim() ?? "";
            const val = dd.textContent?.trim() ?? "";
            values[label] = val;
            const dtBox = dt.getBoundingClientRect();
            const ddBox = dd.getBoundingClientRect();
            if (dtBox.right > ddBox.left + 1) {
              overlaps.push(`${label}: dt.right(${dtBox.right}) > dd.left(${ddBox.left})`);
            }
          }
          return { values, overlaps };
        });
        expect(topoLayout.overlaps).toEqual([]);
        expect(topoLayout.values["Namespace"]).toBe("27");
        expect(topoLayout.values["Pod"]).toBe("600");
        await saveCapture(page, "k16-overview", vp.width, theme, "complete-scale");

        // U8 & U11: Navigate to Workloads (second tab), verify NamespaceSelect keyboard/search & free-text rejection
        await tabs.nth(1).click();
        await expect(page).toHaveURL(/view=resources/);
        const workloadNs = page.getByRole("combobox", { name: "Workload namespace", exact: true });
        await expect(workloadNs).toBeVisible();
        await workloadNs.focus();
        const workloadListbox = page.getByRole("listbox", { name: "Workload namespace options", exact: true });
        await expect(workloadListbox).toBeVisible();
        await expect(workloadListbox.getByRole("option")).toHaveCount(28); // All namespaces + 27 load-0000..load-0026
        const allOptions = await workloadListbox.getByRole("option").allTextContents();
        expect(allOptions[0]).toBe("All namespaces");
        await saveCapture(page, "k16-namespace-select", vp.width, theme, "open");

        // Search and select via keyboard
        await workloadNs.fill("load-0001");
        await page.keyboard.press("Enter");
        await expect(workloadNs).toHaveValue("load-0001");
        // Reject free text
        await workloadNs.focus();
        await workloadNs.fill("non-existent-free-text-ns");
        await expect(page.getByText("No matching namespaces", { exact: true })).toBeVisible();
        await page.keyboard.press("Enter");
        await page.keyboard.press("Escape");
        await expect(workloadNs).toHaveValue("load-0001");

        // U6, U6b, V1, U13: Open long-named workload in load-0001
        const longName = "vikki-mobile-dev-checkout-orchestrator-canary-long-workload-v2";
        await page.getByRole("textbox", { name: "Resource name", exact: true }).fill("vikki-mobile-dev");
        await page.getByRole("button", { name: new RegExp(longName) }).first().click();
        const drawer = page.getByRole("dialog", { name: "Details panel" });
        await expect(drawer).toBeVisible();
        await expect(drawer).toContainText(longName);

        // U6b: Workload Uptime tile omits container badge
        const workloadUptime = drawer.getByLabel("Uptime metric tile", { exact: true });
        await expect(workloadUptime).toBeVisible();
        await expect(workloadUptime.locator(".metric-container-label")).toHaveCount(0);

        // U6 & V1: CPU and memory usage chart has two-row header (row 1: title + legend, row 2: range buttons + since opened) with no overlap
        if (vp.name === "mobile") {
          await drawer.getByRole("button", { name: /CPU usage history/i }).click();
        }
        const usageChart = drawer.getByRole("figure", { name: "CPU and memory usage chart", exact: true });
        await expect(usageChart).toBeVisible();
        const chartHeaderGeometry = await usageChart.evaluate((fig) => {
          const caption = fig.querySelector("figcaption")!;
          const rangeGroup = fig.querySelector<HTMLElement>(`[aria-label="Usage history range"]`)!;
          const captionBox = caption.getBoundingClientRect();
          const rangeBox = rangeGroup.getBoundingClientRect();
          return {
            captionPosition: getComputedStyle(caption).position,
            captionBottom: captionBox.bottom,
            rangeTop: rangeBox.top,
            rangeText: rangeGroup.textContent ?? "",
            overlapsVertically: rangeBox.top < captionBox.bottom - 2,
          };
        });
        expect(chartHeaderGeometry.captionPosition).toBe("static");
        expect(chartHeaderGeometry.overlapsVertically).toBe(false);
        expect(chartHeaderGeometry.rangeText).toContain("since opened");
        await saveCapture(page, "k16-workload-metrics", vp.width, theme, "long-name");

        // U13: Resource detail Timeline tab for empty history shows single short line "No changes or crashes in this range." and zero coverage markers
        await openDrawerTab(page, drawer, "timeline", "Timeline");
        const resourceHistory = drawer.getByRole("region", { name: "Resource change history", exact: true });
        await expect(resourceHistory).toBeVisible();
        await expect(resourceHistory.getByText("No changes or crashes in this range.", { exact: true })).toBeVisible();
        expect(await resourceHistory.getByRole("button", { name: "Resource coverage", exact: true }).count()).toBeLessThanOrEqual(1);
        await saveCapture(page, "k16-resource-timeline", vp.width, theme, "empty");

        await drawer.getByRole("button", { name: "Close panel", exact: true }).click();
        await expect(drawer).toHaveCount(0);

        // Switch Workload namespace to load-0000 to inspect app-00000 (U12 Related & Pod U6b/V2)
        await page.getByRole("textbox", { name: "Resource name", exact: true }).fill("");
        await workloadNs.focus();
        await workloadNs.fill("load-0000");
        await page.keyboard.press("Enter");
        await expect(workloadNs).toHaveValue("load-0000");
        await page.getByRole("button", { name: /app-00000/ }).first().click();
        const appDrawer = page.getByRole("dialog", { name: "Details panel" });
        await expect(appDrawer).toBeVisible();
        await expect(appDrawer).toContainText("app-00000");

        // U12: Related tab shows actual connected children/owners/Service/Ingress/HPA/ConfigMap/Secret/PVC/SA/Node and excludes all 10 sibling Deployments sharing common-secrets
        await openDrawerTab(page, appDrawer, "related", "Related");
        await expect(appDrawer.getByRole("button", { name: "Open Secret common-secrets", exact: true })).toBeVisible({ timeout: 20_000 });
        for (const expectedBtn of [
          "Open ReplicaSet app-00000-rs",
          "Open Pod app-00000-rs-00",
          "Open Service app-00000-svc",
          "Open HorizontalPodAutoscaler app-00000-hpa",
          "Open ConfigMap app-00000-config",
          "Open PersistentVolumeClaim app-00000-pvc",
          "Open Node node-000",
        ]) {
          await expect(appDrawer.getByRole("button", { name: expectedBtn, exact: true })).toBeVisible();
        }
        const relatedText = await appDrawer.innerText();
        expect(relatedText).toContain("app-00000-sa");
        expect(relatedText).not.toContain("sibling-shared-secret-");
        await saveCapture(page, "k16-resource-related", vp.width, theme, "directed");

        // U6b & V2: Open Pod app-00000-rs-00 from Related tab and verify Pod Uptime container badge ellipsis + no clipping at 1440 & 390
        await appDrawer.getByRole("button", { name: "Open Pod app-00000-rs-00", exact: true }).click();
        const podDrawer = page.getByRole("dialog", { name: "Details panel" });
        await expect(podDrawer).toBeVisible();
        await expect(podDrawer).toContainText("app-00000-rs-00");
        await openDrawerTab(page, podDrawer, "overview", "Overview");
        const podUptime = podDrawer.getByLabel("Uptime metric tile", { exact: true });
        await expect(podUptime).toBeVisible();
        const containerBadge = podUptime.locator(".metric-container-label");
        await expect(containerBadge).toBeVisible();
        await expect(containerBadge).toHaveAttribute("title", "vikki-mobile-dev-extremely-long-primary-container-runtime-v2");
        const uptimeGeometry = await podUptime.evaluate((tile) => {
          const tileBox = tile.getBoundingClientRect();
          const h3Box = tile.querySelector("h3")!.getBoundingClientRect();
          const badge = tile.querySelector<HTMLElement>(".metric-container-label")!;
          const badgeBox = badge.getBoundingClientRect();
          const sinceP = tile.querySelectorAll("p")[1]!;
          const sinceBox = sinceP.getBoundingClientRect();
          return {
            badgeOverlapsH3: badgeBox.top < h3Box.bottom - 1 && badgeBox.left < h3Box.right,
            badgeOverflowX: getComputedStyle(badge).textOverflow,
            sinceClipped: sinceBox.bottom > tileBox.bottom + 1,
          };
        });
        expect(uptimeGeometry.badgeOverlapsH3).toBe(false);
        expect(uptimeGeometry.badgeOverflowX).toBe("ellipsis");
        expect(uptimeGeometry.sinceClipped).toBe(false);
        await saveCapture(page, "k16-pod-uptime", vp.width, theme, "long-container");
        await podDrawer.getByRole("button", { name: "Close panel", exact: true }).click();

        // U8 & U9: Issues tab -> NamespaceSelect + Alert on new issues disabled when no channel is enabled
        await tabs.nth(2).click();
        await expect(page).toHaveURL(/view=issues/);
        const issueSection = page.getByRole("region", { name: "Kubernetes issues", exact: true });
        await expect(issueSection).toBeVisible();
        const alertSwitch = issueSection.getByRole("switch", { name: "Alert on new issues", exact: true });
        await expect(alertSwitch).toBeDisabled();
        await expect(alertSwitch).toHaveAttribute("aria-checked", "false");
        await expect(issueSection.getByText("No alert channel is on", { exact: true })).toBeVisible();
        await expect(issueSection.getByRole("link", { name: "Channel settings" })).toHaveAttribute("href", "https://docs.versusincident.com/#/agent/channels");
        await expect(issueSection.getByRole("combobox", { name: "Issue namespace", exact: true })).toBeVisible();
        await saveCapture(page, "k16-issues-alerts", vp.width, theme, "no-channel");

        // U4 & U10: Timeline tab -> virtualised list of breaks and changes, smooth paging, selection -> detail chart & Logs at break & CrashJournal
        await tabs.nth(3).click();
        await expect(page).toHaveURL(/view=timeline/);
        const timelineSection = page.getByRole("region", { name: "Kubernetes change timeline", exact: true });
        await expect(timelineSection).toBeVisible();
        const timelineList = timelineSection.getByRole("list", { name: "Breaks and changes", exact: true });
        await expect(timelineList).toBeVisible();
        await expect(timelineList.getByRole("listitem").first()).toBeVisible({ timeout: 30_000 });
        const renderedItems = await timelineList.getByRole("listitem").count();
        expect(renderedItems).toBeGreaterThan(0);
        expect(renderedItems).toBeLessThan(30); // Virtualised list
        await expect(page.getByRole("region", { name: "Resource timeline chart", exact: true })).toHaveCount(0);
        await expect(page.getByRole("region", { name: "Logs at break", exact: true })).toHaveCount(0);
        const timelineNs = timelineSection.getByRole("combobox", { name: "Timeline namespace", exact: true });
        await expect(timelineNs).toBeVisible();
        await timelineNs.focus();
        await timelineNs.fill("load-0000");
        await page.keyboard.press("Enter");
        await expect(timelineNs).toHaveValue("load-0000");
        await expect(timelineList.getByRole("listitem").first()).toBeVisible({ timeout: 30_000 });
        await saveCapture(page, "k16-timeline-list", vp.width, theme, "populated");

        // U4 paging: click Next page on Timeline pages and verify smooth transition
        const changePager = timelineSection.getByRole("navigation", { name: "Timeline pages", exact: true });
        const nextBtn = changePager.getByRole("button", { name: "Next page", exact: true });
        await expect(nextBtn).toBeEnabled();
        await nextBtn.click();
        await expect(changePager.getByText("Page 2", { exact: true })).toBeVisible();
        await expect(timelineList.getByRole("listitem").first()).toBeVisible();
        await changePager.getByRole("button", { name: "Previous page", exact: true }).click();
        await expect(changePager.getByText("Page 1", { exact: true })).toBeVisible();

        // U4 detail & deep link + U10 Captured crash evidence: select break/crash for app-00000
        await timelineList.evaluate((el) => { el.scrollTop = 0; });
        const breakOrCrashLink = timelineList.getByRole("link", { name: /Open (break|crash) details/ }).first();
        await breakOrCrashLink.click({ force: true });
        await expect(page).toHaveURL(/root_uid=uid-app-00000/);
        await expect(timelineSection.getByRole("link", { name: "Clear root filter", exact: true })).toBeVisible();
        await expect(timelineSection.getByRole("region", { name: "Resource timeline chart", exact: true })).toBeVisible({ timeout: 30_000 });
        await expect(timelineSection.getByRole("region", { name: "Logs at break", exact: true })).toBeVisible();
        await expect(timelineSection.getByRole("group", { name: "Container log mode", exact: true }).getByRole("button")).toHaveText(["Merged", "Previous container", "Current"]);

        // U10: Captured crash evidence header "N crashes" + 2-line Capture details
        const crashJournal = timelineSection.getByRole("region", { name: "Captured crash evidence", exact: true });
        await expect(crashJournal).toBeVisible();
        await expect(crashJournal.locator("header")).toContainText(/\d+ crashes\b/);
        await crashJournal.getByText("Capture details", { exact: true }).click();
        const captureCoverage = crashJournal.getByLabel("Crash capture coverage", { exact: true });
        await expect(captureCoverage).toBeVisible();
        const coverageLines = await captureCoverage.locator("p").allTextContents();
        expect(coverageLines).toHaveLength(2);
        expect(coverageLines[0]).toMatch(/^Window:/);
        expect(coverageLines[1]).toMatch(/^Capture started:/);
        await saveCapture(page, "k16-timeline-detail", vp.width, theme, "selected");

        // U8 Traffic namespace dropdown
        await tabs.nth(8).click();
        await expect(page).toHaveURL(/view=traffic/);
        await expect(page.getByRole("combobox", { name: "Traffic namespace", exact: true })).toBeVisible();

        // Secondary instance checks (U1, U2, V3 CoverageMarker on refused kinds + U9 missing public_host with channel enabled + U13 incomplete marker)
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
        await expect(secPage.getByRole("region", { name: "Cluster health", exact: true })).toBeVisible();

        // V3 & U2: At most ONE CoverageMarker on Overview
        const overviewMarkers = secPage.getByRole("button", { name: "Resource coverage", exact: true });
        await expect(overviewMarkers).toHaveCount(1);

        // U1: Hover/focus CoverageMarker -> popover below tab bar, max-width 320, plain copy "Some resources could not be read" + affected kinds
        await overviewMarkers.first().click();
        const coveragePopover = secPage.getByRole("region", { name: "Resource coverage details", exact: true });
        await expect(coveragePopover).toBeVisible();
        await expect(coveragePopover).toContainText("Some resources could not be read");
        await expect(coveragePopover).toContainText(/nodes: permission denied|secrets: permission denied/);
        const popoverPlacement = await secPage.evaluate(() => {
          const tabs = document.querySelector(`[aria-label="Kubernetes views"]`)!.getBoundingClientRect();
          const pop = document.querySelector(`[aria-label="Resource coverage details"]`)!.getBoundingClientRect();
          return {
            tabsBottom: tabs.bottom,
            popTop: pop.top,
            popWidth: pop.width,
            popRight: pop.right,
            viewportWidth: window.innerWidth,
          };
        });
        expect(popoverPlacement.popTop).toBeGreaterThanOrEqual(popoverPlacement.tabsBottom);
        expect(popoverPlacement.popWidth).toBeLessThanOrEqual(321);
        expect(popoverPlacement.popRight).toBeLessThanOrEqual(popoverPlacement.viewportWidth);
        await saveCapture(secPage, "k16-coverage-popover", vp.width, theme, "open");
        await secPage.keyboard.press("Escape");
        await expect(coveragePopover).toHaveCount(0);

        // U9 (D3) & R4 on Secondary Issues tab: missing public_host shows tip, channel enabled allows turning ON and OFF
        const secTabs = secPage.getByRole("tablist", { name: "Kubernetes views", exact: true }).getByRole("tab");
        await secTabs.nth(2).click();
        const secIssues = secPage.getByRole("region", { name: "Kubernetes issues", exact: true });
        await expect(secIssues).toBeVisible();
        await expect(secPage.getByRole("button", { name: "Resource coverage", exact: true })).toHaveCount(1);
        await expect(secIssues.getByText("Set public_host to add Timeline and DevOps Agent links on Alerts", { exact: true })).toBeVisible();
        const secSwitch = secIssues.getByRole("switch", { name: "Alert on new issues", exact: true });
        await expect(secSwitch).toBeEnabled();
        await secSwitch.click();
        await expect(secSwitch).toHaveAttribute("aria-checked", "true");
        await expect(secIssues.getByRole("group", { name: "Issue alert level", exact: true })).toBeVisible();
        await saveCapture(secPage, "k16-issues-alerts", vp.width, theme, "missing-public-host-enabled");
        await secSwitch.click();
        await expect(secSwitch).toHaveAttribute("aria-checked", "false");

        expect(pageErrors).toEqual([]);
        await secContext.close();
        await context.close();
      });
    }
  }
});

test.describe("Kubernetes UI polish K16 real kind live logs (U7) and Related (U12)", () => {
  test.skip(mode !== "kind", "runs in real kind mode");
  test.setTimeout(240_000);

  for (const vp of viewports) {
    for (const theme of themes) {
      test(`U7 Current -> Previous -> Current live kind logs ${vp.width} ${theme}`, async ({ browser }) => {
        const context = await browser.newContext({ viewport: { width: vp.width, height: vp.height } });
        const page = await context.newPage();
        const pageErrors: string[] = [];
        page.on("pageerror", (err) => pageErrors.push(err.message));
        await applyTheme(page, theme);

        // Wait for our per-run kind namespace and deploy-multi to be indexed
        await openApp(page, "/agent/kubernetes?view=resources");
        await expect.poll(async () => {
          const res = await page.request.get(`/api/admin/kubernetes/workloads?q=deploy-multi`);
          if (!res.ok()) return false;
          const body = await res.json();
          return (body.items || []).some((item: { namespace?: string; name: string }) => item.namespace === kindNamespace && item.name === "deploy-multi");
        }, { timeout: 90_000, intervals: [1000, 2000] }).toBe(true);

        await page.getByRole("button", { name: "Refresh Kubernetes data", exact: true }).click();
        const workloadNs = page.getByRole("combobox", { name: "Workload namespace", exact: true });
        await workloadNs.focus();
        await workloadNs.fill(kindNamespace);
        await page.keyboard.press("Enter");
        await expect(workloadNs).toHaveValue(kindNamespace);

        // Open Deployment deploy-multi -> Logs tab
        await page.getByRole("button", { name: /deploy-multi/ }).first().click();
        const drawer = page.getByRole("dialog", { name: "Details panel" });
        await expect(drawer).toBeVisible();
        await expect(drawer).toContainText("deploy-multi");
        await openDrawerTab(page, drawer, "logs", "Logs");

        const logsRegion = drawer.getByRole("region", { name: "Workload live logs", exact: true });
        await expect(logsRegion).toBeVisible();
        const logOutput = logsRegion.getByLabel("Workload log output", { exact: true });
        await expect(logOutput).toContainText("current-live-stream", { timeout: 45_000 });

        if (vp.name === "mobile") {
          const moreBtn = logsRegion.getByRole("button", { name: "More", exact: true });
          if (await moreBtn.isVisible() && (await moreBtn.getAttribute("aria-expanded")) !== "true") {
            await moreBtn.click();
          }
        }

        const instanceGroup = logsRegion.getByRole("group", { name: "Container instance", exact: true });
        const currentBtn = instanceGroup.getByRole("button", { name: "Current", exact: true });
        const previousBtn = instanceGroup.getByRole("button", { name: "Previous logs", exact: true });
        await expect(currentBtn).toHaveAttribute("aria-pressed", "true");
        await expect(previousBtn).toHaveAttribute("aria-pressed", "false");
        // Following is automatic in K22; the status pill names the live read.
        const statusPill = logsRegion.getByLabel("Log connection status", { exact: true });
        await expect(statusPill).toHaveText("Live");
        await saveCapture(page, "k16-kind-logs", vp.width, theme, "current");

        // Switch to Previous logs on real kind cluster
        await previousBtn.click();
        await expect(previousBtn).toHaveAttribute("aria-pressed", "true");
        await expect(currentBtn).toHaveAttribute("aria-pressed", "false");
        await expect(statusPill).toHaveText("Previous");
        await expect(logOutput).toContainText("previous-container-fatal", { timeout: 30_000 });
        await saveCapture(page, "k16-kind-logs", vp.width, theme, "previous");

        // Switch back to Current logs with one click and confirm live stream resumes
        await currentBtn.click();
        await expect(currentBtn).toHaveAttribute("aria-pressed", "true");
        await expect(previousBtn).toHaveAttribute("aria-pressed", "false");
        await expect(statusPill).toHaveText("Live", { timeout: 30_000 });
        await expect(logOutput).toContainText("current-live-stream", { timeout: 30_000 });
        await saveCapture(page, "k16-kind-logs", vp.width, theme, "returned-current");

        expect(pageErrors).toEqual([]);
        await context.close();
      });
    }
  }
});
