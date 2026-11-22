// Browser gate for the Kubernetes list pager, issue order and history warnings.
//
// Everything here is deterministic: the SPA is the real dev-server build of the
// current source, and every `/api/**` body is an intercepted fixture that
// mirrors the server contract (`total`, `limit`, `count_capped`, ordered
// `items`, per-issue `history_truncated`, `partial_failures`). The fixture
// stands in for a cluster read; it is not live-cluster evidence and the
// captures are labelled `fixtures`.
//
// Run with E2E_UI_FIXTURES=1, which makes playwright.config.ts start the Vite
// dev server on 127.0.0.1:4175 instead of pointing at a harness stack.
import { expect, test, type Locator, type Page, type Route } from "@playwright/test";
import { capturePath } from "./support/capture";
import type { KubernetesIssue, KubernetesTimelinePage } from "../../src/lib/api";

const observed = "2026-11-05T16:00:00Z";
const sync = { state: "ready" as const, age_s: 0, partial: false };

type IssueSeed = {
  name: string;
  severity: KubernetesIssue["severity"];
  broken: boolean;
  /** ISO instant for broke_at (broken) or first_seen (not broken). */
  at?: string;
  historyTruncated?: boolean;
};

function issueOf({ name, severity, broken, at, historyTruncated }: IssueSeed): KubernetesIssue {
  return {
    root: { kind: "Deployment", namespace: "shop", name, uid: `uid-${name}` },
    rule: "pod.crash_loop",
    severity,
    count: 2,
    of: 3,
    broken,
    state: "open",
    ...(broken ? { broke_at: at } : { first_seen: at }),
    ...(historyTruncated ? { history_truncated: true } : {}),
  };
}

// Page 1: critical newest first, an unbroken critical with a newer first_seen
// after the broken ones, a critical with no broke_at last in its group, then
// the warning group starts.
const page1: IssueSeed[] = [
  { name: "checkout", severity: "critical", broken: true, at: "2026-11-05T15:38:00Z" },
  { name: "ledger", severity: "critical", broken: true, at: "2026-11-05T11:06:00Z" },
  { name: "payments", severity: "critical", broken: true },
  { name: "search", severity: "critical", broken: false, at: "2026-11-05T16:10:00Z" },
  { name: "email", severity: "warning", broken: true, at: "2026-11-05T14:49:00Z" },
];
// Page 2 continues newest-first inside the warning group, then info.
const page2: IssueSeed[] = [
  { name: "email", severity: "warning", broken: true, at: "2026-11-05T12:00:00Z" },
  { name: "catalog", severity: "warning", broken: true, at: "2026-11-05T09:00:00Z" },
  { name: "notify", severity: "warning", broken: true },
  { name: "payments", severity: "info", broken: true, at: "2026-11-05T13:30:00Z" },
  { name: "ledger", severity: "info", broken: true, at: "2026-11-05T08:00:00Z" },
];
const page3: IssueSeed[] = [
  { name: "search", severity: "info", broken: true, at: "2026-11-05T07:30:00Z" },
  { name: "checkout", severity: "info", broken: true, at: "2026-11-05T07:00:00Z" },
  { name: "email", severity: "info", broken: true, at: "2026-11-05T06:30:00Z" },
  { name: "catalog", severity: "info", broken: true, at: "2026-11-05T06:00:00Z" },
  { name: "notify", severity: "info", broken: false, at: "2026-11-05T05:30:00Z" },
];

type IssuesState = "paged" | "capped" | "critical" | "truncated" | "read-failure" | "budget" | "storage" | "no-match" | "no-match-incomplete" | "history-unavailable";

type Fixture = {
  issues: IssuesState;
  workloadTotals: boolean;
  nodeTotals: boolean;
  timelineTotals: boolean;
};

function fixture(): Fixture {
  return { issues: "paged", workloadTotals: true, nodeTotals: true, timelineTotals: true };
}

// The capped row carries crash evidence so the delta also proves the new
// marker does not overlap that line.
const cappedCheckout: KubernetesIssue = {
  ...issueOf({ name: "checkout", severity: "critical", broken: true, at: "2026-11-05T15:38:00Z", historyTruncated: true }),
  evidence: {
    warning_events: 1,
    terminations: [{ reason: "CrashLoopBackOff", exit_code: 1, count: 2, last: "2026-11-05T15:30:00Z" }],
    from: "2026-11-05T15:00:00Z",
    to: "2026-11-05T15:38:00Z",
  },
};

function cleanRow(name: string, at: string): KubernetesIssue {
  return { ...issueOf({ name, severity: "warning", broken: true, at }), history: "complete" };
}

const cleanLedger = cleanRow("ledger", "2026-11-05T11:06:00Z");

function truncatedIssues(): KubernetesIssue[] {
  return [
    cappedCheckout,
    cleanLedger,
    cleanRow("catalog", "2026-11-05T09:00:00Z"),
    cleanRow("notify", "2026-11-05T07:30:00Z"),
    cleanRow("billing", "2026-11-05T06:15:00Z"),
  ];
}

function issuePage(state: Fixture, url: URL) {
  const cursor = url.searchParams.get("cursor");
  const severity = url.searchParams.get("severity") ?? "";
  const rootUID = url.searchParams.get("root_uid");
  const totals = { critical: 24, warning: 12, info: 6 };
  const base = { broken: { total: 42, by_severity: totals }, truncated: false, sync };
  // An expanded row re-reads its own root with `include=changes`.
  if (rootUID) {
    const name = rootUID.replace(/^uid-/, "");
    if (state.issues === "truncated") {
      const item = truncatedIssues().find(issue => issue.root.uid === rootUID) ?? cleanLedger;
      return { ...base, items: [item], totals: { critical: 1, warning: 5, info: 0 }, total: 6, limit: 5 };
    }
    const seed = [...page1, ...page2, ...page3].find(item => item.name === name) ?? page1[0];
    return {
      ...base,
      items: [issueOf({ ...seed, name })],
      totals,
      total: 42,
      limit: 5,
    };
  }
  // A selected severity that matches nothing. The server only reports the
  // severities it matched, so `totals` has no key; a complete snapshot means
  // zero, an incomplete one leaves the count unknown.
  if ((state.issues === "no-match" || state.issues === "no-match-incomplete") && severity) {
    return {
      ...base,
      broken: { total: 0, by_severity: {} },
      items: [],
      totals: {},
      total: 0,
      limit: 5,
      ...(state.issues === "no-match-incomplete" ? { partial_failures: [{ resource_id: "kubernetes.history", class: "source_error" }] } : {}),
    };
  }
  if (state.issues === "critical" || severity) {
    return {
      ...base,
      items: [
        issueOf(page1[0]),
        issueOf(page1[1]),
        issueOf(page1[2]),
        issueOf(page1[3]),
        issueOf({ name: "billing", severity: "critical", broken: true, at: "2026-11-05T09:45:00Z" }),
      ],
      totals: { critical: 42 },
      total: 42,
      limit: 5,
      next: "issues-critical-2",
    };
  }
  if (state.issues === "capped") {
    return { ...base, items: page1.map(issueOf), totals: { critical: 1000, warning: 1000, info: 1000 }, total: 1001, limit: 5, count_capped: true, next: "issues-capped-2" };
  }
  // Saved break history did not answer: the live items stay, without their
  // saved break time or occurrence, and the page names the history store once.
  if (state.issues === "history-unavailable") {
    return {
      ...base,
      sync: { ...sync, partial: true },
      items: page1.map(issueOf).map(item => ({ ...item, broke_at: undefined, occurrence_id: undefined, partial: true })),
      totals,
      total: 42,
      limit: 5,
      next: "issues-p2",
      partial_failures: [{ resource_id: "kubernetes.history", class: "historical_evidence_unavailable" }],
    };
  }
  if (state.issues === "truncated") {
    return {
      ...base,
      items: truncatedIssues(),
      totals: { critical: 1, warning: 5, info: 0 },
      total: 6,
      limit: 5,
      next: "issues-truncated-p2",
    };
  }
  const failures: Record<string, Array<{ resource_id: string; class: string }>> = {
    "read-failure": [{ resource_id: "kubernetes.history", class: "source_error" }],
    budget: [{ resource_id: "kubernetes.history", class: "budget_exhausted" }],
    storage: [{ resource_id: "kubernetes.history", class: "storage_unavailable" }],
  };
  const partial = failures[state.issues];
  const pages: Record<string, IssueSeed[]> = { "issues-p2": page2, "issues-p3": page3 };
  const items = (cursor ? pages[cursor] : page1) ?? page1;
  return {
    ...base,
    items: items.map(issueOf),
    totals,
    total: 42,
    limit: 5,
    ...(cursor === "issues-p3" ? {} : { next: cursor === "issues-p2" ? "issues-p3" : "issues-p2" }),
    ...(partial ? { partial_failures: partial } : {}),
  };
}

function workloadPage(state: Fixture) {
  const items = Array.from({ length: 20 }, (_, index) => ({
    resource_id: "apps~v1~deployments",
    kind: "Deployment",
    namespace: "shop",
    name: `workload-${String(index + 1).padStart(2, "0")}`,
    uid: `workload-${index + 1}`,
  }));
  return {
    items,
    counts: { Deployment: 57, StatefulSet: 0, DaemonSet: 0, Job: 0, Pod: 0, CronJob: 0 },
    observed_at: observed,
    truncated: false,
    sync,
    next: "workloads-p2",
    ...(state.workloadTotals ? { total: 57, limit: 20 } : {}),
  };
}

function nodePage(state: Fixture) {
  const items = Array.from({ length: 20 }, (_, index) => ({
    resource_id: "core~v1~nodes",
    kind: "Node",
    name: `node-${String(index + 1).padStart(2, "0")}`,
    uid: `node-${index + 1}`,
    summary: {},
    conditions: [{ type: "Ready", status: "True" }],
  }));
  return {
    items,
    continue: "nodes-p2",
    truncated: false,
    sync,
    ...(state.nodeTotals ? { total: 45, limit: 20 } : {}),
  };
}

function timelinePage(state: Fixture): KubernetesTimelinePage {
  const items = Array.from({ length: 50 }, (_, index) => ({
    type: "change" as const,
    id: `change-${index + 1}`,
    at: new Date(Date.parse(observed) - index * 60_000).toISOString(),
    change: {
      id: `change-${index + 1}`,
      cluster: "fixture",
      kind: "Deployment",
      namespace: "shop",
      name: `checkout-${index + 1}`,
      uid: `uid-checkout-${index + 1}`,
      type: "image_changed" as const,
      fields: [],
      at: new Date(Date.parse(observed) - index * 60_000).toISOString(),
    },
  }));
  return {
    items,
    next: "timeline-p2",
    total: 120,
    crash_count: 0,
    counts: { change: 120, crash: 0, break: 0 },
    gaps: [],
    sync,
    since: "2026-11-05T15:00:00Z",
    until: observed,
    // The fallback case reports a count but no page size, so the pager keeps
    // the plain "Page N" form.
    ...(state.timelineTotals ? { limit: 50 } : {}),
  } as KubernetesTimelinePage;
}

async function installFixture(page: Page, state: Fixture) {
  const answer = (route: Route, json: unknown, status = 200) => route.fulfill({ json, status });
  await page.route("**/api/**", async route => {
    const url = new URL(route.request().url());
    const path = url.pathname;
    if (path === "/api/admin/config/agent") return answer(route, { enable: false, mode: "detect", ai: { enable: false }, sources: [] });
    if (path === "/api/admin/kubernetes/clusters") return answer(route, { multiple: false, clusters: [] });
    if (path === "/api/admin/kubernetes/stream") return route.fulfill({ contentType: "text/event-stream", body: "event: heartbeat\ndata: {}\n\n" });
    if (path === "/api/admin/kubernetes/sync") {
      return answer(route, {
        ...sync,
        first_ready_at: "2026-11-05T10:00:00Z",
        kinds: {},
        total_kinds: 1,
        ready_kinds: 1,
        failing_kinds: 0,
        pending_kinds: 0,
        truncated: false,
        failing: [],
        unreadable: [],
      });
    }
    if (path === "/api/admin/kubernetes/overview") {
      return answer(route, {
        connector: "kubernetes",
        cluster_id: "fixture",
        observed_at: observed,
        nodes: 45,
        ready_nodes: 45,
        pods: 3,
        running_pods: 3,
        namespaces: 1,
        active_namespaces: 1,
        workloads: 57,
        warnings: 0,
        truncated: false,
        sync,
      });
    }
    if (path === "/api/admin/kubernetes/namespaces") return answer(route, { items: ["shop"], sync });
    if (path === "/api/admin/kubernetes/issue-alerts") return answer(route, { enabled: false, level: "critical", can_write: false, available: true, reason: "", links_ready: true, channels_enabled: true });
    if (path === "/api/admin/kubernetes/graph/overview") return answer(route, { nodes: [], edges: [], omitted: {}, sync });
    if (path === "/api/admin/kubernetes/usage") {
      return answer(route, {
        observed_at: observed,
        availability: "available",
        fresh: true,
        pod_metrics: { availability: "available", fresh: true, complete: true, total: 3, cpu: "500m", memory: "3Gi", observed_at: observed },
        node_metrics: { availability: "available", fresh: true, complete: true, total: 45, cpu: "2", memory: "8Gi", observed_at: observed },
        pods: [],
        nodes: [],
        truncated: false,
      });
    }
    if (path === "/api/admin/kubernetes/issues") return answer(route, issuePage(state, url));
    if (path === "/api/admin/kubernetes/workloads") return answer(route, workloadPage(state));
    if (path === "/api/admin/kubernetes/resources") {
      const resourceID = url.searchParams.get("resource_id");
      if (resourceID === "core~v1~nodes") return answer(route, nodePage(state));
      return answer(route, { items: [], continue: "", truncated: false, sync });
    }
    if (path === "/api/admin/kubernetes/timeline") return answer(route, timelinePage(state));
    // An opened Pod drawer reads its own describe; answer with the Pod it names
    // so the drawer renders a whole resource, not a malformed body.
    const describe = path.match(/^\/api\/admin\/kubernetes\/resources\/core~v1~pods\/([^/]+)\/describe$/);
    if (describe) {
      const name = decodeURIComponent(describe[1]);
      return answer(route, {
        resource: {
          resource_id: "core~v1~pods", api_version: "v1", kind: "Pod", namespace: url.searchParams.get("namespace") ?? "shop", name, uid: `uid-${name}`,
          created_at: "2026-11-05T12:00:00Z", owners: [], conditions: [{ type: "Ready", status: "True", reason: "ContainersReady", last_transition_at: "2026-11-05T12:01:00Z" }],
          summary: { phase: "Running", node: "node-01", restart_count: 0, main_container: "api", containers: [{ name: "api", image: "checkout:v42" }] },
        },
        events: [],
        related_resources: [],
        sync,
      });
    }
    if (path.includes("/deployment") || path.includes("/sso/")) return answer(route, { error: "Community" }, 403);
    if (path.startsWith("/api/admin/kubernetes/")) return answer(route, { items: [], nodes: [], edges: [], omitted: {}, truncated: false, available: false, gaps: [], sync });
    return answer(route, { items: [], incidents: [], total: 0, by_origin: {}, enable: false });
  });
}

type Viewport = { name: "desktop" | "mobile"; width: number; height: number };
const viewports: Viewport[] = [
  { name: "desktop", width: 1440, height: 900 },
  { name: "mobile", width: 390, height: 844 },
];

async function open(page: Page, fixtureState: Fixture, viewport: Viewport, theme: "dark" | "light", url: string) {
  await page.setViewportSize({ width: viewport.width, height: viewport.height });
  await page.emulateMedia({ colorScheme: theme });
  await page.addInitScript(value => localStorage.setItem("versus.theme", value), theme);
  await installFixture(page, fixtureState);
  await page.goto(url);
  await expect(page.getByTestId("app-authenticated")).toBeVisible({ timeout: 30_000 });
  if (theme === "light") await expect(page.locator("html")).toHaveAttribute("data-theme", "light");
  else await expect(page.locator("html")).not.toHaveAttribute("data-theme", "light");
}

async function noHorizontalOverflow(page: Page, where: string) {
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), `${where} must not scroll sideways`).toBe(true);
}

function issuesSection(page: Page) {
  return page.locator('section[aria-label="Kubernetes issues"]');
}

async function rowNames(page: Page): Promise<string[]> {
  return issuesSection(page).locator("article[aria-label]").evaluateAll(nodes => nodes.map(node => node.getAttribute("aria-label") ?? ""));
}

/** Waits for the page's own rows, not the previous page's still-rendered ones. */
async function expectRows(page: Page, expected: string[]) {
  await expect.poll(() => rowNames(page), { message: `issue rows must read ${expected.join(", ")}` }).toEqual(expected);
}

/**
 * The pager must fit its column: no sideways scroll, no overlap between the
 * range and the Prev/Page N/Next group, and no part outside the viewport.
 */
async function assertPagerFits(page: Page, scope = issuesSection(page)) {
  const pager = scope.getByRole("navigation", { name: "Page navigation" });
  await expect(pager).toBeVisible();
  await pager.scrollIntoViewIfNeeded();
  const geometry = await pager.evaluate(el => {
    const rect = el.getBoundingClientRect();
    const box = (node: Element | null) => node ? { left: node.getBoundingClientRect().left, right: node.getBoundingClientRect().right } : null;
    return {
      scrollWidth: el.scrollWidth,
      clientWidth: el.clientWidth,
      outer: { left: rect.left, right: rect.right, top: rect.top, bottom: rect.bottom },
      innerWidth: window.innerWidth,
      innerHeight: window.innerHeight,
      range: box(el.querySelector(":scope > span")),
      controls: box(el.querySelector(":scope > div")),
    };
  });
  expect(geometry.scrollWidth, `pager content must not be clipped ${JSON.stringify(geometry)}`).toBeLessThanOrEqual(geometry.clientWidth + 1);
  expect(geometry.outer.left).toBeGreaterThanOrEqual(-1);
  expect(geometry.outer.right).toBeLessThanOrEqual(geometry.innerWidth + 1);
  expect(geometry.outer.top).toBeGreaterThanOrEqual(-1);
  expect(geometry.outer.bottom).toBeLessThanOrEqual(geometry.innerHeight + 1);
  if (geometry.range && geometry.controls) {
    expect(geometry.range.right, "the range and the page controls must not overlap").toBeLessThanOrEqual(geometry.controls.left + 1);
  }
}


/**
 * The per-row coverage marker must be fully inside the viewport, must not make
 * the row scroll sideways, and must not cover the crash-evidence line or the
 * pager.
 */
async function assertMarkerLayout(page: Page, row: Locator, section: Locator) {
  const marker = row.getByRole("button", { name: "Resource coverage" });
  await expect(marker).toHaveCount(1);
  const box = await marker.evaluate(el => {
    const rect = el.getBoundingClientRect();
    return {
      left: rect.left,
      right: rect.right,
      top: rect.top,
      bottom: rect.bottom,
      width: rect.width,
      height: rect.height,
      scrollWidth: el.scrollWidth,
      clientWidth: el.clientWidth,
      innerWidth: window.innerWidth,
    };
  });
  expect(box.width, "the marker must render a hit target").toBeGreaterThan(0);
  expect(box.height, "the marker must render a hit target").toBeGreaterThan(0);
  expect(box.scrollWidth, `the marker must not clip ${JSON.stringify(box)}`).toBeLessThanOrEqual(box.clientWidth + 1);
  expect(box.left).toBeGreaterThanOrEqual(-1);
  expect(box.right).toBeLessThanOrEqual(box.innerWidth + 1);

  const rowBox = await row.evaluate(el => ({ scrollWidth: el.scrollWidth, clientWidth: el.clientWidth }));
  expect(rowBox.scrollWidth, "the marker must not push its row into sideways scroll").toBeLessThanOrEqual(rowBox.clientWidth + 1);

  const overlaps = async (other: Locator) => {
    const a = await marker.boundingBox();
    const b = await other.boundingBox();
    if (!a || !b) return false;
    const horizontal = Math.min(a.x + a.width, b.x + b.width) - Math.max(a.x, b.x);
    const vertical = Math.min(a.y + a.height, b.y + b.height) - Math.max(a.y, b.y);
    return horizontal > 1 && vertical > 1;
  };
  expect(await overlaps(row.getByLabel("Crash evidence")), "the marker must not cover the crash-evidence line").toBe(false);
  expect(await overlaps(section.getByRole("navigation", { name: "Page navigation" })), "the marker must not cover the pager").toBe(false);
  await noHorizontalOverflow(page, "the Issues list with a row marker");
}

/** The pager line reads exactly, so a missing range or page count fails. */
async function pagerText(page: Page, scope = issuesSection(page)) {
  const pager = scope.getByRole("navigation", { name: "Page navigation" });
  await expect(pager).toHaveCount(1);
  return (await pager.innerText()).replace(/\s+/g, " ").trim();
}

test.describe("Kubernetes list paging, order and history warnings", () => {
  test.skip(process.env.E2E_UI_FIXTURES !== "1", "deterministic intercepted UI evidence, not live collection");
  // Captures are reviewed and published at twice their displayed size.
  test.use({ deviceScaleFactor: 2 });

  test("the dev server serves the current pager and warning copy", async ({ page }) => {
    await page.goto("/");
    const pager = await page.request.get("/src/components/Pagination.tsx");
    expect(pager.ok()).toBe(true);
    const pagerSource = await pager.text();
    const warnings = await page.request.get("/src/pages/kubernetesWarningItems.ts");
    expect(warnings.ok()).toBe(true);
    const warningsSource = await warnings.text();
    // Proof the served bundle is the current source, not a stale build.
    expect(pagerSource).toContain("Page ");
    expect(pagerSource).toContain("capped");
    expect(warningsSource).toContain("Some changes before this break were not loaded");
    expect(warningsSource).toContain("kubernetes.history");
  });

  test("issues show a range, page count and newest-first order on pages 1-3", async ({ page }) => {
    test.setTimeout(240_000);
    for (const viewport of viewports) {
      const state = fixture();
      await open(page, state, viewport, "dark", "/agent/kubernetes?view=issues");
      const section = issuesSection(page);
      await expect(section).toBeVisible({ timeout: 30_000 });
      await expect(section.locator("article[aria-label]").first()).toBeVisible({ timeout: 30_000 });

      // Page 1: range and page count from `total` + `limit`.
      await expectRows(page, ["Deployment checkout", "Deployment ledger", "Deployment payments", "Deployment search", "Deployment email"]);
      expect(await pagerText(page)).toContain("1–5 of 42");
      expect(await pagerText(page)).toContain("Page 1 / 9");
      const firstRow = section.locator("article[aria-label]").first();
      // Newest broke_at first inside the critical group, and the unbroken row
      // (newer first_seen) stays after the broken ones.
      // "Broke" and its <time> are separate flex items, so innerText puts a
      // line break between them; compare the words, not the layout.
      expect((await firstRow.innerText()).replace(/\s+/g, " ")).toContain("Broke " + await page.evaluate(iso => new Date(iso).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false }), "2026-11-05T15:38:00Z"));
      expect((await rowNames(page)).indexOf("Deployment search")).toBeGreaterThan((await rowNames(page)).indexOf("Deployment ledger"));
      await expect(section.locator('article[aria-label="Deployment search"]')).toContainText("First seen");
      // A nil broke_at sorts last in its severity group and reads as unknown.
      await expect(section.locator('article[aria-label="Deployment payments"]')).toContainText("Break time unknown");
      await expect(section.locator('article[aria-label="Deployment payments"]')).not.toContainText("Time unavailable");
      await assertPagerFits(page);
      await noHorizontalOverflow(page, `issues page 1 ${viewport.name}`);
      await page.screenshot({ path: capturePath("kubernetes-list-paging-issues-pager", viewport.width, "dark", "page-1"), fullPage: true, animations: "disabled" });

      // Page 2 continues the order and keeps the range.
      await section.getByRole("button", { name: "Next page" }).click();
      await expect.poll(() => pagerText(page)).toContain("Page 2 / 9");
      await expect.poll(() => pagerText(page)).toContain("6–10 of 42");
      await expectRows(page, ["Deployment email", "Deployment catalog", "Deployment notify", "Deployment payments", "Deployment ledger"]);
      await expect(section.getByLabel("Issue severity")).toBeVisible();
      await assertPagerFits(page);
      await noHorizontalOverflow(page, `issues page 2 ${viewport.name}`);
      await page.screenshot({ path: capturePath("kubernetes-list-paging-issues-pager", viewport.width, "dark", "page-2"), fullPage: true, animations: "disabled" });

      // Page 3 keeps its own range and order.
      await section.getByRole("button", { name: "Next page" }).click();
      await expect.poll(() => pagerText(page)).toContain("Page 3 / 9");
      await expect.poll(() => pagerText(page)).toContain("11–15 of 42");
      await expectRows(page, ["Deployment search", "Deployment checkout", "Deployment email", "Deployment catalog", "Deployment notify"]);
      const thirdRow = section.locator('article[aria-label="Deployment notify"]');
      await expect(thirdRow).toContainText("First seen");
      await assertPagerFits(page);
      await noHorizontalOverflow(page, `issues page 3 ${viewport.name}`);
      await page.screenshot({ path: capturePath("kubernetes-list-paging-issues-pager", viewport.width, "dark", "page-3"), fullPage: true, animations: "disabled" });

      // Prev returns with the same counts, proving the first page's total is kept.
      await section.getByRole("button", { name: "Previous page" }).click();
      await expect.poll(() => pagerText(page)).toContain("Page 2 / 9");
      await expect.poll(() => pagerText(page)).toContain("6–10 of 42");

      // A severity filter shows only its own count, never "Unknown".
      await section.getByLabel("Issue severity").selectOption("critical");
      await expect(section.getByLabel("critical count")).toHaveText("42");
      await expect(section.getByLabel("warning count")).toHaveCount(0);
      await expect(section.getByLabel("info count")).toHaveCount(0);
      await expect(section).not.toContainText("Unknown");
      await expect.poll(() => pagerText(page)).toContain("1–5 of 42");
      await expect.poll(() => pagerText(page)).toContain("Page 1 / 9");
      await assertPagerFits(page);
      await noHorizontalOverflow(page, `issues critical ${viewport.name}`);
      await page.screenshot({ path: capturePath("kubernetes-list-paging-issues-filter", viewport.width, "dark", "critical-only"), fullPage: true, animations: "disabled" });
    }
  });

  test("a selected severity with no matches shows zero, and Unknown only for an incomplete snapshot", async ({ page }) => {
    test.setTimeout(240_000);
    for (const viewport of viewports) {
      for (const stateName of ["no-match", "no-match-incomplete"] as const) {
        const state = fixture();
        state.issues = stateName;
        await open(page, state, viewport, "dark", "/agent/kubernetes?view=issues");
        const section = issuesSection(page);
        await expect(section.locator("article[aria-label]").first()).toBeVisible({ timeout: 30_000 });
        await section.getByLabel("Issue severity").selectOption("critical");
        await expect(section.locator("article[aria-label]")).toHaveCount(0);
        const count = section.getByLabel("critical count");
        const badge = count.locator("xpath=..");
        await expect(section.getByLabel("warning count")).toHaveCount(0);
        await expect(section.getByLabel("info count")).toHaveCount(0);
        if (stateName === "no-match") {
          await expect(count).toHaveText("0");
          expect((await badge.innerText()).replace(/\s+/g, " ").trim()).toMatch(/^critical ?0$/);
          await expect(section).toContainText("No issues match these filters.");
          await expect(section).not.toContainText("Unknown");
        } else {
          await expect(count).toHaveText("Unknown");
          await expect(section).not.toContainText("No issues match these filters.");
        }
        // The badge must sit inside the viewport and not clip.
        const fits = await badge.evaluate(el => {
          const rect = el.getBoundingClientRect();
          return el.scrollWidth <= el.clientWidth + 1 && rect.left >= -1 && rect.right <= innerWidth + 1;
        });
        expect(fits, "the severity badge must fit").toBe(true);
        await noHorizontalOverflow(page, `issues ${stateName} ${viewport.name}`);
        await page.screenshot({ path: capturePath("kubernetes-list-paging-issues-filter", viewport.width, "dark", stateName === "no-match" ? "critical-zero" : "critical-unknown-incomplete"), fullPage: true, animations: "disabled" });
      }
    }
  });

  test("issues show a capped total and only the per-issue history warning", async ({ page }, testInfo) => {
    test.setTimeout(240_000);
    for (const viewport of viewports) {
      const state = fixture();
      state.issues = "capped";
      await open(page, state, viewport, "dark", "/agent/kubernetes?view=issues");
      const section = issuesSection(page);
      await expect(section.locator("article[aria-label]").first()).toBeVisible({ timeout: 30_000 });
      expect(await pagerText(page)).toContain("1–5 of 1,000+");
      expect(await pagerText(page)).toContain("Page 1 / 200+");
      await expect(section).toContainText("Counts are lower bounds from a bounded sample.");
      await assertPagerFits(page);
      await noHorizontalOverflow(page, `capped issues ${viewport.name}`);
      await page.screenshot({ path: capturePath("kubernetes-list-paging-issues-pager", viewport.width, "dark", "capped-total"), fullPage: true, animations: "disabled" });
    }

    // A capped history window marks the issue, not the list. The same row
    // keeps exactly one marker collapsed and expanded; a clean row stays bare.
    const capCopy = "Some changes before this break were not loaded";
    for (const viewport of viewports) {
      const state = fixture();
      state.issues = "truncated";
      await open(page, state, viewport, "dark", "/agent/kubernetes?view=issues");
      const section = issuesSection(page);
      const capped = section.locator('article[aria-label="Deployment checkout"]');
      const clean = section.locator('article[aria-label="Deployment ledger"]');
      await expect(capped).toBeVisible({ timeout: 30_000 });
      await expect(clean).toBeVisible();
      await expect(section).not.toContainText("Change history");
      await expect(section.locator(':scope > button[aria-label="Resource coverage"]'), "no list-level marker for a cap").toHaveCount(0);

      const markers = capped.getByRole("button", { name: "Resource coverage" });
      const panel = page.getByRole("region", { name: "Resource coverage details" });
      const panelReasons = panel.getByRole("list", { name: "Coverage reasons" }).getByRole("listitem");

      // Collapsed: the marker travels with the row itself.
      await expect(markers, "the capped issue must carry one coverage marker while collapsed").toHaveCount(1);
      await expect(clean.getByRole("button", { name: "Resource coverage" }), "a clean row must stay marker-free").toHaveCount(0);
      await markers.click();
      await expect(panelReasons).toHaveText([capCopy]);
      // A cap is not a read failure, so the headline must not claim one.
      await expect(panel.getByText("Some data is incomplete", { exact: true })).toBeVisible();
      await expect(panel).not.toContainText("could not be read");
      await noHorizontalOverflow(page, `truncated issues marker panel ${viewport.name}`);
      await page.screenshot({ path: capturePath("kubernetes-list-paging-issues-warning", viewport.width, "dark", "capped-history-marker-panel"), fullPage: true, animations: "disabled" });
      await page.keyboard.press("Escape");
      await expect(panel).toHaveCount(0);
      await noHorizontalOverflow(page, `truncated issues collapsed ${viewport.name}`);
      await page.screenshot({ path: capturePath("kubernetes-list-paging-issues-warning", viewport.width, "dark", "capped-history-collapsed"), fullPage: true, animations: "disabled" });

      // Expanded: still exactly one marker, and the empty body names the cap.
      await capped.getByRole("button", { name: "Expand checkout" }).click();
      await expect(markers, "expanding must not duplicate the marker").toHaveCount(1);
      await expect(capped).toContainText(capCopy);
      await expect(capped).not.toContainText("No recorded change in available evidence");
      await markers.click();
      await expect(panelReasons).toHaveText([capCopy]);
      await page.keyboard.press("Escape");
      await assertMarkerLayout(page, capped, section);
      await testInfo.attach(`capped-history-row-${viewport.name}`, { body: JSON.stringify({ row: await capped.innerText(), section: await section.innerText() }, null, 2), contentType: "application/json" });
      await page.screenshot({ path: capturePath("kubernetes-list-paging-issues-warning", viewport.width, "dark", "capped-history-expanded"), fullPage: true, animations: "disabled" });

      // A genuinely empty, complete row keeps the old copy and gains no marker.
      await clean.getByRole("button", { name: "Expand ledger" }).click();
      await expect(clean.getByRole("button", { name: "Resource coverage" })).toHaveCount(0);
      await expect(clean).toContainText("No change on it, its config or its nodes");
      await expect(clean).not.toContainText(capCopy);
      await noHorizontalOverflow(page, `truncated issues expanded ${viewport.name}`);
      await page.screenshot({ path: capturePath("kubernetes-list-paging-issues-warning", viewport.width, "dark", "clean-row-expanded"), fullPage: true, animations: "disabled" });
    }
  });

  test("live issues listed while saved break history is unavailable read as partial with a reason", async ({ page }, testInfo) => {
    test.setTimeout(240_000);
    const sentence = "Issues are live, but saved break history is temporarily unavailable, so break times and occurrence links are hidden. It is retried on the next refresh.";
    for (const viewport of viewports) {
      for (const theme of ["dark", "light"] as const) {
        const state = fixture();
        state.issues = "history-unavailable";
        await open(page, state, viewport, theme, "/agent/kubernetes?view=issues");
        const section = issuesSection(page);
        await expectRows(page, ["Deployment checkout", "Deployment ledger", "Deployment payments", "Deployment search", "Deployment email"]);
        // One visible sentence for the page, never the change-capture wording.
        await expect(section.getByRole("status").filter({ hasText: sentence })).toHaveCount(1);
        await expect(section).not.toContainText("Change history");
        await expect(section.locator(':scope > button[aria-label="Resource coverage"]'), "the reason is shown, not hidden behind the list marker").toHaveCount(0);
        // The header badge says partial, not ready.
        const badge = section.locator(":scope > header .pill");
        await expect(badge).toHaveText("partial");
        await expect(badge).toHaveClass(/pill-warn/);
        // Every broken row reads as unknown and gives its reason.
        const payments = section.locator('article[aria-label="Deployment payments"]');
        await expect(payments).toContainText("Break time unknown");
        await expect(payments).toContainText("Saved history unavailable");
        await expect(payments).not.toContainText("Time unavailable");
        await expect(section.locator('article[aria-label="Deployment checkout"]')).not.toContainText("Broke ");
        await expect(section.getByLabel("critical count")).toHaveText("24");
        await assertPagerFits(page);
        await noHorizontalOverflow(page, `history-unavailable issues ${viewport.name} ${theme}`);
        await testInfo.attach(`history-unavailable-${viewport.name}-${theme}`, { body: JSON.stringify({ section: await section.innerText() }, null, 2), contentType: "application/json" });
        await page.screenshot({ path: capturePath("kubernetes-issues-saved-history-unavailable", viewport.width, theme, "partial-page"), fullPage: true, animations: "disabled" });
        // The header badge, in view at every width, and the text contrast of
        // the new copy against what it sits on.
        await badge.scrollIntoViewIfNeeded();
        await section.locator(":scope > header").screenshot({ path: capturePath("kubernetes-issues-saved-history-unavailable-header", viewport.width, theme, "partial-badge"), animations: "disabled" });
        const contrast = await page.evaluate(() => {
          const rgb = (value: string) => (value.match(/[\d.]+/g) ?? []).map(Number);
          const lum = ([r, g, b]: number[]) => { const c = [r, g, b].map(v => { const x = v / 255; return x <= 0.03928 ? x / 12.92 : ((x + 0.055) / 1.055) ** 2.4; }); return 0.2126 * c[0] + 0.7152 * c[1] + 0.0722 * c[2]; };
          const background = (el: Element | null): number[] => { for (let node = el; node; node = node.parentElement) { const c = rgb(getComputedStyle(node).backgroundColor); if (c.length >= 3 && (c[3] === undefined || c[3] > 0.5)) return c; } return [255, 255, 255]; };
          const ratio = (el: Element | null) => { if (!el) return null; const a = lum(rgb(getComputedStyle(el).color)), b = lum(background(el)); return Math.round(((Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05)) * 100) / 100; };
          const section = document.querySelector('section[aria-label="Kubernetes issues"]');
          return {
            sentence: ratio([...(section?.querySelectorAll('[role="status"]') ?? [])].find(el => el.textContent?.includes("saved break history")) ?? null),
            badge: ratio(section?.querySelector(":scope > header .pill") ?? null),
            rowReason: ratio(section?.querySelector("[data-row-reason]") ?? null),
            breakTimeUnknown: ratio([...(section?.querySelectorAll("article span") ?? [])].find(el => el.textContent === "Break time unknown") ?? null),
          };
        });
        await testInfo.attach(`history-unavailable-contrast-${viewport.name}-${theme}`, { body: JSON.stringify(contrast, null, 2), contentType: "application/json" });
      }
    }
  });

  test("on a phone the view tab rail keeps the active tab in view and the drawer title clears its actions", async ({ page }, testInfo) => {
    test.setTimeout(240_000);
    const phone = viewports.find(viewport => viewport.name === "mobile")!;
    const longName = "checkout-payments-reconciliation-worker-7f9c6d5b8-x2k4q";
    for (const theme of ["dark", "light"] as const) {
      // A late tab is active on arrival: the rail scrolls to it, and both ends
      // fade because tabs are hidden on either side.
      await open(page, fixture(), phone, theme, "/agent/kubernetes?view=releases");
      const rail = page.getByRole("tablist", { name: "Kubernetes views", exact: true });
      const active = rail.getByRole("tab", { name: "Helm", exact: true });
      await expect(active).toHaveAttribute("aria-selected", "true");
      await expect.poll(() => active.evaluate(el => {
        const tab = el.getBoundingClientRect();
        const box = el.parentElement!.getBoundingClientRect();
        return tab.left >= box.left - 1 && tab.right <= box.right + 1;
      }), { message: "the active tab must be fully inside the rail" }).toBe(true);
      await expect(rail).toHaveAttribute("data-overflow-start", "true");
      await expect(rail).toHaveAttribute("data-overflow-end", "true");
      expect(await rail.evaluate(el => getComputedStyle(el).maskImage || getComputedStyle(el).webkitMaskImage), "a rail hiding tabs fades its edges").toContain("gradient");
      // Every tab cut by the rail edge sits under a fade: a clipped label is
      // only allowed on a side that fades.
      const clipped = await rail.evaluate(el => {
        const box = el.getBoundingClientRect();
        return [...el.querySelectorAll('[role="tab"]')].map(tab => tab.getBoundingClientRect()).filter(tab => tab.right > box.left && tab.left < box.right).map(tab => ({ start: tab.left < box.left - 1, end: tab.right > box.right + 1 }));
      });
      for (const edge of clipped) {
        if (edge.start) await expect(rail).toHaveAttribute("data-overflow-start", "true");
        if (edge.end) await expect(rail).toHaveAttribute("data-overflow-end", "true");
      }
      await noHorizontalOverflow(page, `phone tab rail ${theme}`);
      await page.screenshot({ path: capturePath("kubernetes-tab-rail", phone.width, theme, "late-tab-active"), fullPage: false, animations: "disabled" });

      // The first tab shows the rail start: no start fade, end fade kept.
      await rail.getByRole("tab", { name: "Overview", exact: true }).click();
      await expect(rail).not.toHaveAttribute("data-overflow-start", "true");
      await expect(rail).toHaveAttribute("data-overflow-end", "true");

      // A long resource name wraps clear of Investigate and Close, and the full
      // name is on the title.
      await open(page, fixture(), phone, theme, `/agent/kubernetes?view=issues&r=${encodeURIComponent(`core~v1~pods/shop/${longName}`)}`);
      const drawer = page.locator("[data-resource-detail]");
      const name = drawer.locator(".resource-name");
      await expect(name).toBeVisible({ timeout: 30_000 });
      await expect(name).toHaveAttribute("title", longName);
      await expect(name).toHaveText(longName);
      // Measure after the drawer has slid in and its detail has loaded.
      await expect(drawer.getByRole("heading", { name: "Restarts", exact: true })).toBeVisible({ timeout: 30_000 });
      let lastBox = "";
      await expect.poll(async () => { const box = JSON.stringify(await drawer.boundingBox()); const settled = box === lastBox; lastBox = box; return settled; }, { intervals: [150] }).toBe(true);
      await testInfo.attach(`drawer-title-boxes-${theme}`, { body: JSON.stringify({ drawer: await drawer.boundingBox(), name: await name.boundingBox(), investigate: await drawer.getByRole("button", { name: "Investigate", exact: true }).boundingBox(), close: await drawer.getByRole("button", { name: "Close panel", exact: true }).boundingBox() }, null, 2), contentType: "application/json" });
      const overlap = async (other: Locator) => {
        const a = await name.boundingBox();
        const b = await other.boundingBox();
        if (!a || !b) return false;
        return Math.min(a.x + a.width, b.x + b.width) - Math.max(a.x, b.x) > 1 && Math.min(a.y + a.height, b.y + b.height) - Math.max(a.y, b.y) > 1;
      };
      expect(await overlap(drawer.getByRole("button", { name: "Investigate", exact: true })), "the name must not run under Investigate").toBe(false);
      expect(await overlap(drawer.getByRole("button", { name: "Close panel", exact: true })), "the name must not run under Close").toBe(false);
      await testInfo.attach(`drawer-title-${theme}`, { body: JSON.stringify({ name: await name.boundingBox(), investigate: await drawer.getByRole("button", { name: "Investigate", exact: true }).boundingBox() }, null, 2), contentType: "application/json" });
      await page.screenshot({ path: capturePath("kubernetes-drawer-title", phone.width, theme, "long-name"), fullPage: false, animations: "disabled" });
    }
  });

  test("a later crash page that cannot load offers Restart crashes and returns focus to Restarts", async ({ page }, testInfo) => {
    test.setTimeout(240_000);
    const podName = "checkout-7d9f8c-x2k4q";
    const podUID = "pod-x2k4q";
    const iso = (minutesAgo: number) => new Date(Date.now() - minutesAgo * 60_000).toISOString();
    const crashOf = (id: string, minutesAgo: number) => ({
      id, sequence: 1, cluster: "default", namespace: "shop", pod: podName, pod_uid: podUID,
      root_uid: "deploy-checkout", root_kind: "Deployment", root_name: "checkout", container: "api",
      type: "termination", termination_slot: "previous", exit_code: 137, reason: "OOMKilled",
      restart_count: 6, at: iso(minutesAgo), time_source: "finished_at", observed_at: iso(minutesAgo),
    });
    // A later crash page that fails for a reason other than an expired link:
    // the page stays an error and offers Restart crashes. An expired link is
    // covered by the next test, where the pager returns to page 1 instead.
    for (const [viewport, theme] of [[viewports[0], "dark"], [viewports[1], "dark"], [viewports[1], "light"]] as const) {
      const cursors: Array<string | null> = [];
      await open(page, fixture(), viewport, theme, "/agent/kubernetes?view=issues");
      await page.route("**/api/admin/kubernetes/resources/core~v1~pods/**/describe**", route => route.fulfill({
        json: {
          resource: {
            resource_id: "core~v1~pods", api_version: "v1", kind: "Pod", namespace: "shop", name: podName, uid: podUID,
            created_at: iso(180), owners: [], conditions: [{ type: "Ready", status: "True", reason: "ContainersReady", last_transition_at: iso(134) }],
            summary: { phase: "Running", node: "node-01", restart_count: 6, main_container: "api", containers: [{ name: "api", image: "checkout:v42" }] },
          },
          events: [], related_resources: [], sync,
        },
      }));
      await page.route("**/api/admin/kubernetes/crashes**", route => {
        const url = new URL(route.request().url());
        const cursor = url.searchParams.get("cursor");
        cursors.push(cursor);
        if (cursor) return route.fulfill({ status: 503, json: { error: "History store did not answer.", code: "history_unavailable" } });
        return route.fulfill({
          json: {
            items: [crashOf("c1", 8), crashOf("c2", 134)], crash_count: 120, next: "crash-page-2",
            since: url.searchParams.get("since") ?? iso(1440), until: url.searchParams.get("until") ?? iso(0),
            started_at: iso(170), retention_from: iso(1440), retention_basis: "observed_at", gaps: [], capture_gaps: [], limitations: [],
            coverage: { Pod: { from: iso(1440), to: iso(0) } }, capture: { mode: "continuous", behind: false, last_cycle_at: iso(0) }, sync,
          },
        });
      });
      await page.goto(`/agent/kubernetes?view=issues&r=${encodeURIComponent(`core~v1~pods/shop/${podName}`)}`);
      const drawer = page.locator("[data-resource-detail]");
      const pager = drawer.getByRole("navigation", { name: "Captured crash pages" });
      await expect(pager).toBeVisible({ timeout: 30_000 });
      await expect(pager).toContainText("120 captured crashes in the last 24h");
      await expect(drawer.getByRole("button", { name: "Restart crashes" })).toHaveCount(0);
      await pager.getByRole("button", { name: "Next page" }).click();
      const restart = drawer.getByRole("button", { name: "Restart crashes" });
      await expect(restart).toBeVisible({ timeout: 30_000 });
      const notice = drawer.getByRole("status").filter({ hasText: "This crash page could not be loaded; its link may have expired." });
      await expect(notice).toHaveCount(1);
      await expect(notice.getByRole("button", { name: "Restart crashes" })).toBeVisible();
      await expect(drawer.locator(".metric-restart-description")).toContainText("Crash evidence unavailable");
      await noHorizontalOverflow(page, `crash restart notice ${viewport.name} ${theme}`);
      // Keyboard path: the button is reachable and shows its focus ring.
      await restart.focus();
      await page.keyboard.press("Shift+Tab");
      await page.keyboard.press("Tab");
      await expect(restart).toBeFocused();
      const ring = await restart.evaluate(el => { const s = getComputedStyle(el); return { outline: `${s.outlineStyle} ${s.outlineWidth} ${s.outlineColor}`, shadow: s.boxShadow }; });
      await notice.scrollIntoViewIfNeeded();
      await page.screenshot({ path: capturePath("kubernetes-resource-crashes", viewport.width, theme, "page-expired-restart-focused"), fullPage: false, animations: "disabled" });
      await page.keyboard.press("Enter");
      const heading = drawer.getByRole("heading", { name: "Restarts", exact: true });
      await expect(heading).toBeFocused();
      await expect(restart).toHaveCount(0);
      await expect(pager).toBeVisible();
      await expect(pager).toContainText("120 captured crashes in the last 24h");
      await expect(pager.getByRole("button", { name: "Previous page" })).toBeDisabled();
      // The first page comes back (from cache or a fresh read), never the
      // failed cursor again.
      const failedReads = cursors.filter(Boolean).length;
      expect(failedReads, "the failed page was asked for").toBeGreaterThan(0);
      await expect(drawer.locator(".metric-restart-description")).toContainText("shown OOMKilled");
      await page.waitForTimeout(500);
      expect(cursors.filter(Boolean).length, "the restart does not ask for the failed page again").toBe(failedReads);
      await noHorizontalOverflow(page, `crash restart done ${viewport.name} ${theme}`);
      await testInfo.attach(`crash-restart-${viewport.name}-${theme}`, { body: JSON.stringify({ cursors, ring, active: await page.evaluate(() => document.activeElement?.textContent) }, null, 2), contentType: "application/json" });
      await page.screenshot({ path: capturePath("kubernetes-resource-crashes", viewport.width, theme, "restarted-first-page"), fullPage: false, animations: "disabled" });
      await page.unrouteAll({ behavior: "ignoreErrors" });
    }
  });

  test("a crash page link the server reports as expired never labels the first page as a later one", async ({ page }, testInfo) => {
    test.setTimeout(120_000);
    // The server answers an expired crash cursor with 400 cursor_expired. The
    // user must then either be offered Restart crashes or see the pager back
    // on its first page; the first page's crashes under "Page 2" is wrong.
    const podName = "checkout-7d9f8c-x2k4q";
    const podUID = "pod-x2k4q";
    const iso = (minutesAgo: number) => new Date(Date.now() - minutesAgo * 60_000).toISOString();
    const cursors: Array<string | null> = [];
    await open(page, fixture(), viewports[0], "dark", "/agent/kubernetes?view=issues");
    await page.route("**/api/admin/kubernetes/resources/core~v1~pods/**/describe**", route => route.fulfill({
      json: {
        resource: {
          resource_id: "core~v1~pods", api_version: "v1", kind: "Pod", namespace: "shop", name: podName, uid: podUID,
          created_at: iso(180), owners: [], conditions: [],
          summary: { phase: "Running", node: "node-01", restart_count: 6, main_container: "api", containers: [{ name: "api", image: "checkout:v42" }] },
        },
        events: [], related_resources: [], sync,
      },
    }));
    await page.route("**/api/admin/kubernetes/crashes**", route => {
      const url = new URL(route.request().url());
      const cursor = url.searchParams.get("cursor");
      cursors.push(cursor);
      if (cursor) return route.fulfill({ status: 400, json: { error: "The list changed since this page was loaded.", code: "cursor_expired" } });
      return route.fulfill({
        json: {
          items: [{ id: "c1", sequence: 1, cluster: "default", namespace: "shop", pod: podName, pod_uid: podUID, root_uid: "deploy-checkout", root_kind: "Deployment", root_name: "checkout", container: "api", type: "termination", termination_slot: "previous", exit_code: 137, reason: "OOMKilled", restart_count: 6, at: iso(8), time_source: "finished_at", observed_at: iso(8) }],
          crash_count: 120, next: "crash-page-2",
          since: url.searchParams.get("since") ?? iso(1440), until: url.searchParams.get("until") ?? iso(0),
          started_at: iso(170), retention_from: iso(1440), retention_basis: "observed_at", gaps: [], capture_gaps: [], limitations: [],
          coverage: { Pod: { from: iso(1440), to: iso(0) } }, capture: { mode: "continuous", behind: false, last_cycle_at: iso(0) }, sync,
        },
      });
    });
    await page.goto(`/agent/kubernetes?view=issues&r=${encodeURIComponent(`core~v1~pods/shop/${podName}`)}`);
    const drawer = page.locator("[data-resource-detail]");
    const pager = drawer.getByRole("navigation", { name: "Captured crash pages" });
    await expect(pager).toBeVisible({ timeout: 30_000 });
    await pager.getByRole("button", { name: "Next page" }).click();
    await expect.poll(() => cursors.filter(Boolean).length).toBeGreaterThan(0);
    await page.waitForTimeout(1_500);
    const seen = {
      cursors,
      pager: (await pager.innerText().catch(() => "")).replace(/\s+/g, " ").trim(),
      restartButton: await drawer.getByRole("button", { name: "Restart crashes" }).count(),
      restartsTile: (await drawer.locator(".metric-restart-description").innerText().catch(() => "")).trim(),
    };
    await testInfo.attach("crash-cursor-expired-400", { body: JSON.stringify(seen, null, 2), contentType: "application/json" });
    await pager.scrollIntoViewIfNeeded();
    await page.screenshot({ path: capturePath("kubernetes-resource-crashes", 1440, "dark", "cursor-expired-400"), fullPage: false, animations: "disabled" });
    const backOnFirstPage = !/\bPage 2\b/.test(seen.pager) && await pager.getByRole("button", { name: "Previous page" }).isDisabled().catch(() => true);
    expect(seen.restartButton === 1 || backOnFirstPage, `after an expired crash link the pager read "${seen.pager}" with ${seen.restartButton} Restart crashes button(s)`).toBe(true);
  });

  test("a drawer whose resource is gone says so instead of breaking the page", async ({ page }) => {
    test.setTimeout(120_000);
    const podName = "checkout-7d9f8c-gone1";
    for (const [viewport, theme] of [[viewports[0], "dark"], [viewports[1], "light"]] as const) {
      await open(page, fixture(), viewport, theme, "/agent/kubernetes?view=issues");
      await page.route("**/api/admin/kubernetes/resources/core~v1~pods/**/describe**", route => route.fulfill({ status: 404, json: { error: "The Kubernetes object was not found.", code: "not_found" } }));
      await page.goto(`/agent/kubernetes?view=issues&r=${encodeURIComponent(`core~v1~pods/shop/${podName}`)}`);
      const drawer = page.locator("[data-resource-detail]");
      await expect(drawer.getByRole("status").filter({ hasText: "This Pod no longer exists in the cluster. It may have been deleted or replaced." })).toBeVisible({ timeout: 30_000 });
      await expect(page.getByRole("heading", { name: "Couldn't render this page" })).toHaveCount(0);
      await expect(drawer.getByText("Couldn't load resource detail")).toHaveCount(0);
      await expect(issuesSection(page)).toBeVisible();
      await noHorizontalOverflow(page, `gone drawer ${viewport.name} ${theme}`);
      await page.screenshot({ path: capturePath("kubernetes-drawer-resource-gone", viewport.width, theme, "not-found"), fullPage: false, animations: "disabled" });
      await page.unrouteAll({ behavior: "ignoreErrors" });
    }
  });

  test("a real history read failure keeps the list-level Change history wording", async ({ page }) => {
    test.setTimeout(240_000);
    // The coverage panel words each class the way the server's own sentences
    // do; a source or storage fault is retried by the next sync.
    const expected: Record<string, string> = {
      "read-failure": "Change history could not be read; the next sync tries again.",
      budget: "Change history was read only in part because it is larger than one read allows.",
      storage: "Change history could not be read; the next sync tries again.",
    };
    for (const viewport of viewports) {
      for (const stateName of ["read-failure", "budget", "storage"] as const) {
        const state = fixture();
        state.issues = stateName;
        await open(page, state, viewport, "dark", "/agent/kubernetes?view=issues");
        const section = issuesSection(page);
        await expect(section.locator("article[aria-label]").first()).toBeVisible({ timeout: 30_000 });
        // The list-level marker is a direct child of the Issues section; row
        // markers live inside their articles.
        const listMarker = section.locator(':scope > button[aria-label="Resource coverage"]');
        await expect(listMarker).toHaveCount(1);
        await listMarker.click();
        const detail = page.getByRole("region", { name: "Resource coverage details" });
        // The neutral headline holds for every reason; the reason row says why.
        await expect(detail.getByText("Some data is incomplete", { exact: true })).toBeVisible();
        await expect(detail).not.toContainText("Some resources could not be read");
        const reason = detail.getByRole("list", { name: "Coverage reasons" }).getByRole("listitem").first();
        await expect(reason).toHaveText(expected[stateName]);
        if (stateName === "budget") {
          await expect(reason).not.toContainText("could not be read");
          await expect(reason).toHaveText(expected[stateName]);
        }
        await noHorizontalOverflow(page, `${stateName} issues ${viewport.name}`);
        await page.screenshot({ path: capturePath("kubernetes-list-paging-issues-warning", viewport.width, "dark", `${stateName}-list-level`), fullPage: true, animations: "disabled" });
        await page.keyboard.press("Escape");
      }
    }
  });

  test("workload, node and timeline pagers show totals and fall back without them", async ({ page }) => {
    test.setTimeout(240_000);
    for (const viewport of viewports) {
      // Workloads with total + limit.
      const state = fixture();
      await open(page, state, viewport, "dark", "/agent/kubernetes?view=resources");
      const workloads = page.locator('section[aria-label="Workloads"]');
      await expect(workloads.getByText("workload-01").first()).toBeVisible({ timeout: 30_000 });
      expect(await pagerText(page, workloads)).toContain("1–20 of 57");
      expect(await pagerText(page, workloads)).toContain("Page 1 / 3");
      await assertPagerFits(page, workloads);
      await noHorizontalOverflow(page, `workloads totals ${viewport.name}`);
      await page.screenshot({ path: capturePath("kubernetes-list-paging-workloads-pager", viewport.width, "dark", "totals"), fullPage: true, animations: "disabled" });

      // Workloads without a total keep the plain "Page N" form.
      state.workloadTotals = false;
      await page.reload();
      await expect(workloads.getByText("workload-01").first()).toBeVisible({ timeout: 30_000 });
      const workloadFallback = await pagerText(page, workloads);
      expect(workloadFallback).toContain("Page 1");
      expect(workloadFallback).not.toContain(" of ");
      expect(workloadFallback).not.toMatch(/Page 1 \/ /);
      await assertPagerFits(page, workloads);
      await noHorizontalOverflow(page, `workloads fallback ${viewport.name}`);
      await page.screenshot({ path: capturePath("kubernetes-list-paging-workloads-pager", viewport.width, "dark", "no-total"), fullPage: true, animations: "disabled" });

      // Nodes with total + limit.
      const nodeState = fixture();
      await open(page, nodeState, viewport, "dark", "/agent/kubernetes?view=nodes");
      const nodes = page.locator('section[aria-label="Nodes"]');
      await expect(nodes.getByRole("button", { name: "View pods on node-01" })).toBeVisible({ timeout: 30_000 });
      expect(await pagerText(page, nodes)).toContain("1–20 of 45");
      expect(await pagerText(page, nodes)).toContain("Page 1 / 3");
      await assertPagerFits(page, nodes);
      await noHorizontalOverflow(page, `nodes totals ${viewport.name}`);
      await page.screenshot({ path: capturePath("kubernetes-list-paging-nodes-pager", viewport.width, "dark", "totals"), fullPage: true, animations: "disabled" });

      // Nodes without a total.
      nodeState.nodeTotals = false;
      await page.reload();
      await expect(nodes.getByRole("button", { name: "View pods on node-01" })).toBeVisible({ timeout: 30_000 });
      const nodeFallback = await pagerText(page, nodes);
      expect(nodeFallback).toContain("Page 1");
      expect(nodeFallback).not.toContain(" of ");
      expect(nodeFallback).not.toMatch(/Page 1 \/ /);
      await assertPagerFits(page, nodes);
      await noHorizontalOverflow(page, `nodes fallback ${viewport.name}`);
      await page.screenshot({ path: capturePath("kubernetes-list-paging-nodes-pager", viewport.width, "dark", "no-total"), fullPage: true, animations: "disabled" });

      // Timeline with total + limit.
      const timelineState = fixture();
      await open(page, timelineState, viewport, "dark", "/agent/kubernetes?view=timeline");
      const timeline = page.locator('section[aria-label="Kubernetes change timeline"]');
      await expect(timeline.getByRole("navigation", { name: "Timeline pages" })).toBeVisible({ timeout: 30_000 });
      expect(await pagerText(page, timeline)).toContain("1–50 of 120");
      expect(await pagerText(page, timeline)).toContain("Page 1 / 3");
      await assertPagerFits(page, timeline);
      await noHorizontalOverflow(page, `timeline totals ${viewport.name}`);
      await page.screenshot({ path: capturePath("kubernetes-list-paging-timeline-pager", viewport.width, "dark", "totals"), fullPage: true, animations: "disabled" });

      // Timeline without a limit keeps "Page N".
      timelineState.timelineTotals = false;
      await page.reload();
      await expect(timeline.getByRole("navigation", { name: "Timeline pages" })).toBeVisible({ timeout: 30_000 });
      const timelineFallback = await pagerText(page, timeline);
      expect(timelineFallback).toContain("Page 1");
      expect(timelineFallback).not.toContain(" of ");
      expect(timelineFallback).not.toMatch(/Page 1 \/ /);
      await assertPagerFits(page, timeline);
      await noHorizontalOverflow(page, `timeline fallback ${viewport.name}`);
      await page.screenshot({ path: capturePath("kubernetes-list-paging-timeline-pager", viewport.width, "dark", "no-total"), fullPage: true, animations: "disabled" });
    }
  });

  test("light theme pager parity at both widths", async ({ page }) => {
    test.setTimeout(120_000);
    for (const viewport of viewports) {
      const state = fixture();
      await open(page, state, viewport, "light", "/agent/kubernetes?view=issues");
      const section = issuesSection(page);
      await expect(section.locator("article[aria-label]").first()).toBeVisible({ timeout: 30_000 });
      expect(await pagerText(page)).toContain("1–5 of 42");
      expect(await page.locator("html")).toHaveAttribute("data-theme", "light");
      await assertPagerFits(page);
      await noHorizontalOverflow(page, `light issues ${viewport.name}`);
      await page.screenshot({ path: capturePath("kubernetes-list-paging-issues-pager", viewport.width, "light", "page-1"), fullPage: true, animations: "disabled" });
    }
  });
});
