import { expect, test, type Page, type Route } from "@playwright/test";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { openApp } from "./helpers";
import { capturePath } from "./support/capture";
import type { KubernetesIssue, KubernetesTimelinePage } from "../../src/lib/api";

const sync = { state: "ready", age_s: 0, partial: false };
const execute = promisify(execFile);
const issue: KubernetesIssue = {
  root: { kind: "Deployment", namespace: "shop", name: "qa-checkout", uid: "k27-checkout" },
  rule: "pod.crash_loop", severity: "critical", count: 2, of: 2, broken: true,
  broke_at: new Date(Date.now() - 120_000).toISOString(), occurrence_id: "k27-occurrence",
};

async function capture(page: Page, width: number, theme: "light" | "dark", state: string) {
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  expect(await page.locator("html").getAttribute("data-theme")).toBe(theme === "light" ? "light" : null);
  await page.screenshot({ path: capturePath("k27-history", width, theme, state), animations: "disabled" });
  if (width === 390) {
    const clipped = await page.getByRole("group", { name: "Timeline window", exact: true }).evaluate(group => {
      const edge = Math.min(innerWidth, group.closest("section")!.getBoundingClientRect().right);
      return [...group.querySelectorAll("button")].filter(button => button.getBoundingClientRect().right > edge + 1).map(button => button.textContent);
    });
    expect(clipped, "K27-B2: compact Timeline window controls must not be clipped on mobile").toEqual([]);
  }
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

test("K27 live authenticated persisted history and commit stream smoke (no interception)", async ({ page }, testInfo) => {
  test.setTimeout(120_000);
  await openApp(page, "/agent/kubernetes?view=timeline");
  if (process.env.E2E_LICENSED === "1") {
    const deploymentResponse = await page.request.get("/enterprise/api/sso/deployment");
    expect(deploymentResponse.status(), "the Enterprise license gate must actually be ON").toBe(200);
    const deployment = await deploymentResponse.json();
    expect(typeof deployment.org).toBe("string");
    expect(deployment.org.length).toBeGreaterThan(0);
    const sessionResponse = await page.request.get(`/enterprise/api/sso/${encodeURIComponent(deployment.org)}/session`);
    expect(sessionResponse.status()).toBe(200);
    expect((await sessionResponse.json()).local).toBe(true);
    expect((await page.context().cookies()).some(cookie => cookie.name === "versus_enterprise_session" && cookie.httpOnly)).toBe(true);
    await testInfo.attach("licensed-local-admin-proof", { body: JSON.stringify({ licensed: true, local_admin: true, http_only_cookie: true }), contentType: "application/json" });
  }
  const statuses: Record<string, number> = {};
  const window = `since=${encodeURIComponent(new Date(Date.now() - 3_600_000).toISOString())}&until=${encodeURIComponent(new Date().toISOString())}`;
  for (const endpoint of [`timeline?${window}`, `issues?include=changes&${window}`, "changes", "crashes"]) {
    const response = await page.request.get(`/api/admin/kubernetes/${endpoint}`);
    statuses[endpoint.split("?")[0]] = response.status();
    expect(response.status(), `live ${endpoint.split("?")[0]}`).toBe(200);
    const body = await response.json();
    expect(Array.isArray(body.items)).toBe(true);
    expect(body.sync).toBeTruthy();
  }
  const stream = await page.evaluate(async () => {
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), 10_000);
    try {
      const response = await fetch("/api/admin/kubernetes/commits/stream", { signal: controller.signal });
      if (!response.ok) return { status: response.status, event: false };
      const reader = response.body!.getReader();
      let text = "";
      while (!text.includes("event: committed")) {
        const chunk = await reader.read();
        if (chunk.done) break;
        text += new TextDecoder().decode(chunk.value);
      }
      return { status: response.status, event: text.includes("event: committed") };
    } finally { clearTimeout(timeout); controller.abort(); }
  });
  expect(stream).toEqual({ status: 200, event: true });
  await expect(page.getByRole("region", { name: "Kubernetes change timeline" })).toBeVisible();
  await expect(page.getByLabel("Loading projected change history")).toHaveCount(0);
  await page.getByRole("tab", { name: "Breaks only", exact: true }).click();
  const liveBreak = page.getByRole("link", { name: "Open break details", exact: true }).first();
  await expect(liveBreak, "kind triage fixtures must publish an actual retained break").toBeVisible();
  const logRequest = page.waitForRequest(request => new URL(request.url()).pathname.endsWith("/break-logs"));
  await liveBreak.click();
  await expect(page.getByRole("region", { name: "Resource timeline chart", exact: true })).toBeVisible();
  await expect(page.getByText("Retained root context is unavailable.", { exact: false })).toHaveCount(0);
  const logURL = await logRequest;
  const logResponse = await page.request.get(logURL.url());
  const logBody = await logResponse.json();
  await testInfo.attach("live-break-log-status", { body: JSON.stringify({ status: logResponse.status(), code: logBody.code, error: logResponse.ok() ? undefined : logBody.error, query: new URL(logURL.url()).search }), contentType: "application/json" });
  expect(logResponse.status(), "live retained break logs must accept the UI's first-page selection").toBe(200);
  const independentWindow = new URL(logURL.url());
  const independentFrom = new Date(Date.parse(independentWindow.searchParams.get("center")!) + 30 * 60_000).toISOString();
  const independentTo = new Date(Date.parse(independentFrom) + 5 * 60_000).toISOString();
  independentWindow.searchParams.set("since", independentFrom);
  independentWindow.searchParams.set("until", independentTo);
  const independentResponse = await page.request.get(independentWindow.href);
  expect(independentResponse.status(), "an occurrence selector must not overwrite an independent bounded log window").toBe(200);
  const independentBody = await independentResponse.json();
  expect(Date.parse(independentBody.window.from)).toBe(Date.parse(independentFrom));
  expect(Date.parse(independentBody.window.to)).toBe(Date.parse(independentTo));
  for (const width of [1440, 390]) for (const theme of ["dark", "light"] as const) {
    await page.setViewportSize({ width, height: width === 390 ? 844 : 1000 });
    await page.emulateMedia({ colorScheme: theme });
    await page.evaluate(value => localStorage.setItem("versus.theme", value), theme);
    const savedLogs = page.waitForResponse(response => new URL(response.url()).pathname.endsWith("/break-logs"));
    await page.reload();
    await expect(page.getByRole("region", { name: "Resource timeline chart", exact: true })).toBeVisible();
    expect((await savedLogs).status()).toBe(200);
    await expect(page.getByText(/\d+ lines from \d+ pods in this window/)).toBeVisible();
    await expect(page.getByText("Loading logs at break...", { exact: true })).toHaveCount(0);
    await capture(page, width, theme, "live-detail");
  }
  await testInfo.attach("live-history-statuses", { body: JSON.stringify({ statuses, stream }), contentType: "application/json" });
});

type Fault = "ok" | "hang" | "timeout" | "denied" | "mismatch";

async function historyFixture(page: Page, selected: KubernetesIssue = issue, rowAt = selected.broke_at!) {
  let fault: Fault = "ok";
  let historyFails = false;
  let historyHangs = false;
  let capped = false;
  let unreachable = false;
  let initialized = true;
  let authorityDenied = false;
  let streamConnections = 0;
  const reconnectHints: Array<string | undefined> = [];
  const reads: URL[] = [];
  let release!: () => void;
  let barrier = new Promise<void>(resolve => { release = resolve; });
  const answer = (route: Route, json: unknown, status = 200) => route.fulfill({ json, status });
  await page.route("**/api/admin/kubernetes/**", async route => {
    const url = new URL(route.request().url());
    const path = url.pathname;
    reads.push(url);
    if (path.endsWith("/commits/stream")) {
      streamConnections++;
      reconnectHints.push(route.request().headers()["last-event-id"]);
      if (authorityDenied) return answer(route, { code: "history_authority_denied", error: "Persisted source authority was revoked" }, 403);
      if (!initialized) return answer(route, { code: "history_not_established", error: "First collection has not committed", retryable: true }, 503);
      const committed = { revision: 7, evidence_sequence: 9, kinds: ["deployments"], cluster_id: "" };
      const event = `id: 7\nevent: committed\ndata: ${JSON.stringify(committed)}\n\n`;
      return route.fulfill({ contentType: "text/event-stream", body: event + event + `event: sync\ndata: ${JSON.stringify({ state: unreachable ? "stale" : "ready", reason: unreachable ? "cluster_unreachable" : "", age: unreachable ? 90 : 2, cluster_id: "" })}\n\n` });
    }
    if (path.endsWith("/clusters")) return answer(route, { multiple: false, clusters: [] });
    if (path.endsWith("/sync")) return answer(route, { ...sync, state: unreachable ? "error" : initialized ? "ready" : "syncing", age_s: unreachable ? 90 : 0, first_ready_at: initialized ? selected.broke_at : undefined, kinds: {}, ready_kinds: initialized ? 1 : 0, total_kinds: 1 });
    if (path.endsWith("/overview")) return answer(route, { connector: "kubernetes", observed_at: new Date().toISOString(), nodes: initialized ? 1 : 0, ready_nodes: initialized ? 1 : 0, pods: initialized ? 2 : 0, running_pods: 0, namespaces: 1, active_namespaces: 1, workloads: initialized ? 1 : 0, warnings: 1, truncated: false, sync: initialized ? sync : { ...sync, state: "syncing" } });
    if (path.endsWith("/namespaces")) return answer(route, { items: ["shop", "other"], sync });
    if (authorityDenied && /\/(issues|timeline|changes|crashes|break-logs)$/.test(path)) {
      return answer(route, { code: "history_authority_denied", error: "Persisted source authority was revoked" }, 403);
    }
    if (!initialized && /\/(issues|timeline|changes|crashes|break-logs)$/.test(path)) {
      return answer(route, { code: "history_not_established", error: "First collection has not committed", retryable: true }, 503);
    }
    if (path.endsWith("/issues")) {
      if (url.searchParams.get("context") === "persisted") {
        if (fault === "hang") await barrier;
        if (fault === "timeout") return answer(route, { code: "history_store_timeout", error: "QA lookup timeout" }, 504);
        if (fault === "denied" || fault === "mismatch") return answer(route, { code: fault === "denied" ? "history_authority_denied" : "authority_mismatch", error: "QA authority denied" }, 403);
        return answer(route, { items: [], totals: {}, truncated: false, sync, selection_status: "resolved", selected_occurrence: selected, root_context: { root: selected.root, occurrence: selected, retained: true } });
      }
      if (historyFails) return answer(route, { code: "history_store_timeout", error: "QA refresh unavailable" }, 503);
      return answer(route, { items: [selected], totals: { critical: 1 }, broken: { total: 1, by_severity: { critical: 1 } }, truncated: false, sync });
    }
    if (path.endsWith("/timeline")) {
      if (historyHangs) await barrier;
      if (historyFails) return answer(route, { code: "history_store_timeout", error: "QA refresh unavailable" }, 503);
      const cursor = url.searchParams.get("cursor");
      const payload: KubernetesTimelinePage = {
        items: cursor ? [] : [{ type: "break", id: "qa-break", at: rowAt, issue: selected }],
        total: capped ? 1000 : 1, crash_count: 0, counts: { change: 0, crash: 0, break: capped ? 1000 : 1 },
        gaps: [], sync, since: url.searchParams.get("since")!, until: url.searchParams.get("until")!,
        ...(capped && !cursor ? { count_capped: true, next: "k27+/cursor" } : {}),
      };
      return answer(route, payload);
    }
    if (path.endsWith("/changes")) return historyFails ? answer(route, { code: "history_store_timeout", error: "QA refresh unavailable" }, 503) : answer(route, { items: [], total: 17, sync });
    if (path.endsWith("/crashes")) return answer(route, { items: [], sync, coverage: {}, limitations: [], capture: { mode: "journal", behind: false } });
    if (path.endsWith("/break-logs")) {
      return answer(route, {
        occurrence_id: selected.occurrence_id,
        window: { from: url.searchParams.get("since"), to: url.searchParams.get("until") },
        pods: [{ name: "qa-retained-pod", uid: "qa-pod", state: "terminated", short: "retained" }],
        containers: [{ name: "app", main: true }], main_container: "app",
        lines: [{ at: selected.broke_at, pod: "qa-retained-pod", pod_uid: "qa-pod", container: "app", init: false, slot: "previous", level: "error", text: "K27 saved evidence", source: "native" }],
        markers: [], counts: { all: 1, errors: 1, warnings: 0 }, rate: [], patterns: [], sources: [],
        coverage: { state: "complete", gaps: [], reason: "", tip: false },
      });
    }
    return route.fallback();
  });
  return {
    reads, reconnectHints, connections: () => streamConnections,
    setFault(value: Fault) { fault = value; },
    holdLookup() { fault = "hang"; barrier = new Promise<void>(resolve => { release = resolve; }); },
    holdHistory() { historyHangs = true; barrier = new Promise<void>(resolve => { release = resolve; }); },
    failHistory() { historyFails = true; },
    cap() { capped = true; },
    startBeforeFirstCommit() { initialized = false; },
    publishFirstCommit() { initialized = true; },
    denyHistoryAuthority() { authorityDenied = true; },
    restoreHistoryAuthority(value: KubernetesIssue) { selected = value; authorityDenied = false; },
    disconnectCluster() { unreachable = true; },
    release() { release(); barrier = Promise.resolve(); },
  };
}

for (const width of [1440, 390]) for (const theme of ["dark", "light"] as const) {
  test(`K27 intercepted seeded hang timeout denial and deep link ${width} ${theme}`, async ({ page }) => {
    test.setTimeout(90_000);
    await page.setViewportSize({ width, height: width === 390 ? 844 : 1000 });
    await page.emulateMedia({ colorScheme: theme });
    await page.addInitScript(value => localStorage.setItem("versus.theme", value), theme);
    await openApp(page, "/now");
    const fixture = await historyFixture(page);
    try {
      fixture.setFault("hang");
      await page.goto("/agent/kubernetes?view=timeline");
      await page.getByRole("link", { name: "Open break details", exact: true }).click();
      const chart = page.getByRole("region", { name: "Resource timeline chart", exact: true });
      await expect(chart).toBeVisible();
      await expect.poll(() => fixture.reads.filter(url => url.searchParams.get("context") === "persisted").length).toBeGreaterThan(0);
      await expect.poll(() => fixture.reads.filter(url => url.pathname.endsWith("/changes") && url.searchParams.has("root_uid")).length).toBeGreaterThan(0);
      await expect.poll(() => fixture.reads.filter(url => url.pathname.endsWith("/crashes") && url.searchParams.has("root_uid")).length).toBeGreaterThan(0);
      await expect.poll(() => fixture.reads.filter(url => url.pathname.endsWith("/break-logs")).length).toBeGreaterThan(0);
      await expect(page.getByText("K27 saved evidence", { exact: true })).toBeVisible();
      const windowsBefore = fixture.reads.filter(url => url.pathname.endsWith("/changes")).length;
      await page.getByRole("group", { name: "Timeline window", exact: true }).getByRole("button", { name: "15m", exact: true }).click();
      await expect.poll(() => fixture.reads.filter(url => url.pathname.endsWith("/changes")).length).toBeGreaterThan(windowsBefore);
      await expect(chart).toBeVisible();
      await expect(page.getByText("K27 saved evidence", { exact: true })).toBeVisible();
      await capture(page, width, theme, "seeded-hung-intercepted");
      fixture.setFault("timeout");
      fixture.release();
      await expect(page.getByText(/Couldn't refresh — showing stored break evidence/)).toBeVisible();
      await expect(chart).toBeVisible();
      await capture(page, width, theme, "seeded-504-intercepted");
      await expect(page.getByText("Retained root context is unavailable.", { exact: false }), "K27-B3: a transient confirmation failure must not contradict the retained seeded evidence").toHaveCount(0);
      const deepLink = page.url();
      fixture.setFault("denied");
      await page.getByRole("button", { name: "Retry root context", exact: true }).click();
      await expect(chart).toHaveCount(0);
      await expect(page.getByText(/Couldn't refresh — showing stored break evidence/)).toHaveCount(0);
      const unseededBefore = fixture.reads.filter(url => url.pathname.endsWith("/changes")).length;
      fixture.holdLookup();
      await page.reload();
      await expect.poll(() => fixture.reads.filter(url => url.pathname.endsWith("/changes")).length).toBeGreaterThan(unseededBefore);
      await expect(chart).toHaveCount(0);
      fixture.setFault("ok");
      fixture.release();
      await expect(chart).toBeVisible();
      expect(page.url()).toBe(deepLink);
      fixture.setFault("mismatch");
      await page.reload();
      await expect(page.getByText("Retained root context is unavailable.", { exact: false })).toBeVisible();
      await expect(chart).toHaveCount(0);
    } finally { fixture.release(); await page.unrouteAll({ behavior: "ignoreErrors" }); }
  });
}

test("K27 intercepted last-good window slide capped cursor and commit reconnect", async ({ page }) => {
  await openApp(page, "/now");
  const fixture = await historyFixture(page);
  fixture.cap();
  try {
    await page.goto("/agent/kubernetes?view=timeline");
    const timeline = page.getByRole("region", { name: "Kubernetes change timeline" });
    await expect(timeline).toContainText("1000+");
    await expect(timeline).toContainText("Counts are lower bounds from a bounded sample.");
    await timeline.getByRole("navigation", { name: "Timeline pages" }).getByRole("button", { name: /Next/ }).click();
    await expect.poll(() => fixture.reads.some(url => url.searchParams.get("cursor") === "k27+/cursor")).toBe(true);
    await expect(timeline).toContainText("1000+");
    await page.goto("/agent/kubernetes?view=timeline");
    await expect(page.getByRole("link", { name: "Open break details" })).toBeVisible();
    await watchHistorySkeletons(page);
    await page.clock.install();
    const before = fixture.reads.filter(url => url.pathname.endsWith("/timeline")).length;
    fixture.failHistory();
    await page.clock.fastForward(31_000);
    await expect.poll(() => fixture.reads.filter(url => url.pathname.endsWith("/timeline")).length).toBeGreaterThan(before);
    await expect(page.getByRole("link", { name: "Open break details" })).toBeVisible();
    await expect(page.getByLabel("Loading projected change history")).toHaveCount(0);
    await expect(page.locator("html")).toHaveAttribute("data-k27-skeletons", "0");
    await expect.poll(() => fixture.connections()).toBeGreaterThan(1);
    await expect.poll(() => fixture.reconnectHints.slice(1)).toContain("7");
    const historyReads = fixture.reads.filter(url => url.pathname.endsWith("/timeline")).length;
    expect(historyReads).toBeLessThanOrEqual(fixture.connections() * 2 + 5);
    await expect(page.locator("[data-sync-state]")).toContainText(/Last synced|Updates disconnected/);
    fixture.disconnectCluster();
    await page.clock.fastForward(2_000);
    await expect(page.locator("[data-sync-state]")).toContainText("Cluster unreachable — showing data from");
  } finally { fixture.release(); await page.unrouteAll({ behavior: "ignoreErrors" }); }
});

test("K27 intercepted Overview and Issues seed every retained break shape during lookup hang", async ({ page }) => {
  test.setTimeout(120_000);
  await openApp(page, "/now");
  const samples = [
    { kind: "Deployment", view: "overview", broken: true },
    { kind: "CronJob", view: "issues", broken: true },
    { kind: "Job", view: "overview", broken: true },
    { kind: "TestRun", view: "issues", broken: true },
    { kind: "Deployment", view: "issues", broken: false },
    { kind: "Pod", view: "overview", broken: false },
  ];
  for (const [index, sample] of samples.entries()) {
    const selected = { ...issue, root: { ...issue.root, kind: sample.kind, name: `qa-${sample.kind.toLowerCase()}-${index}`, uid: `k27-root-${index}` }, broken: sample.broken, history: "complete" as const };
    const fixture = await historyFixture(page, selected);
    fixture.setFault("hang");
    try {
      await page.goto(`/agent/kubernetes?view=${sample.view}`);
      await page.getByRole("button", { name: `Timeline for ${selected.root.name}`, exact: true }).click();
      await expect(page.getByRole("region", { name: "Resource timeline chart", exact: true })).toBeVisible();
      await expect(page.getByLabel("Loading related root change history")).toHaveCount(0);
      await expect.poll(() => fixture.reads.some(url => url.pathname.endsWith("/break-logs"))).toBe(true);
      await expect(page.getByText("K27 saved evidence", { exact: true })).toBeVisible();
    } finally { fixture.release(); await page.unrouteAll({ behavior: "ignoreErrors" }); }
  }
});

test("K27 intercepted Overview retains last-good Changes tile on failed refresh", async ({ page }) => {
  await openApp(page, "/now");
  const fixture = await historyFixture(page);
  try {
    await page.goto("/agent/kubernetes");
    const overview = page.getByRole("region", { name: "Break timeline", exact: true });
    const tile = overview.getByRole("button", { name: /Changes in 60 min/ });
    await expect(tile).toContainText("17");
    await page.clock.install();
    fixture.failHistory();
    await page.clock.fastForward(31_000);
    await expect(tile).toContainText("17");
    await expect(tile).not.toContainText("Unknown");
    await expect(overview).toContainText(/Couldn't refresh|earlier evidence/);
    await expect(page.getByLabel("Loading break timeline", { exact: true })).toHaveCount(0);
  } finally { fixture.release(); await page.unrouteAll({ behavior: "ignoreErrors" }); }
});

test("K27 intercepted pending refresh preserves rows on range change but isolates a new filter", async ({ page }) => {
  await openApp(page, "/now");
  const fixture = await historyFixture(page);
  try {
    await page.goto("/agent/kubernetes?view=timeline");
    const row = page.getByRole("link", { name: "Open break details", exact: true });
    await expect(row).toBeVisible();
    await watchHistorySkeletons(page);
    fixture.holdHistory();
    await page.clock.install();
    const before = fixture.reads.filter(url => url.pathname.endsWith("/timeline")).length;
    await page.clock.fastForward(31_000);
    await expect.poll(() => fixture.reads.filter(url => url.pathname.endsWith("/timeline")).length).toBeGreaterThan(before);
    await expect(row).toBeVisible();
    await page.getByRole("group", { name: "Timeline window", exact: true }).getByRole("button", { name: "6h", exact: true }).click();
    await expect(row).toBeVisible();
    await expect(page.getByLabel("Loading projected change history")).toHaveCount(0);
    await expect(page.locator("html")).toHaveAttribute("data-k27-skeletons", "0");
    await page.getByRole("combobox", { name: "Timeline kind", exact: true }).selectOption("Pod");
    await expect(row).toHaveCount(0);
    await expect(page.getByLabel("Loading projected change history")).toBeVisible();
    fixture.release();
    await expect(page.getByLabel("Loading projected change history")).toHaveCount(0);
  } finally { fixture.release(); await page.unrouteAll({ behavior: "ignoreErrors" }); }
});

test("K27 intercepted older occurrence keeps log selection independent of latest row observation (K27-B1)", async ({ page }) => {
  await openApp(page, "/now");
  const fixture = await historyFixture(page, { ...issue, broke_at: new Date(Date.now() - 3_600_000).toISOString() }, new Date().toISOString());
  try {
    await page.goto("/agent/kubernetes?view=timeline");
    await page.getByRole("link", { name: "Open break details", exact: true }).click();
    await expect.poll(() => fixture.reads.some(url => url.pathname.endsWith("/break-logs"))).toBe(true);
    const request = fixture.reads.find(url => url.pathname.endsWith("/break-logs"))!;
    const center = Date.parse(request.searchParams.get("center")!);
    expect(center, "K27-B1: a first-page saved-log selection must lie inside its own requested window").toBeGreaterThanOrEqual(Date.parse(request.searchParams.get("since")!));
    expect(center, "K27-B1: latest observation must not replace the occurrence's saved-log anchor").toBeLessThanOrEqual(Date.parse(request.searchParams.get("until")!));
    await expect(page.getByText("K27 saved evidence", { exact: true })).toBeVisible();
  } finally { fixture.release(); await page.unrouteAll({ behavior: "ignoreErrors" }); }
});

test("K27 intercepted first open before first commit recovers without reload (R1)", async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 });
  await page.emulateMedia({ colorScheme: "dark" });
  await page.addInitScript(() => localStorage.setItem("versus.theme", "dark"));
  await openApp(page, "/now");
  const fixture = await historyFixture(page);
  fixture.startBeforeFirstCommit();
  const documentMarker = `k27-r1-${Date.now()}`;
  try {
    await page.goto("/agent/kubernetes?view=timeline");
    await page.locator("html").evaluate((html, marker) => { html.setAttribute("data-k27-document", marker); }, documentMarker);
    await expect.poll(() => fixture.connections()).toBeGreaterThanOrEqual(1);
    await expect(page.getByRole("link", { name: "Open break details", exact: true })).toHaveCount(0);
    fixture.publishFirstCommit();
    await expect(page.getByRole("link", { name: "Open break details", exact: true })).toBeVisible();
    await expect.poll(() => fixture.connections()).toBeGreaterThan(1);
    await expect(page.getByLabel("Loading projected change history")).toHaveCount(0);
    await capture(page, 1440, "dark", "r1-first-commit-recovered-intercepted");
    await page.getByRole("tab", { name: "Overview", exact: true }).click();
    await expect(page.getByRole("button", { name: "Timeline for qa-checkout", exact: true })).toBeVisible();
    await expect(page.locator("[data-sync-state]")).toContainText(/Last synced|Updates disconnected/);
    await expect(page.locator("html"), "first-commit recovery must not depend on a document reload").toHaveAttribute("data-k27-document", documentMarker);
  } finally { fixture.release(); await page.unrouteAll({ behavior: "ignoreErrors" }); }
});

test("K27 intercepted typed stream denial wipes history but preserves current recovery lane (R1)", async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1000 });
  await page.emulateMedia({ colorScheme: "dark" });
  await page.addInitScript(() => localStorage.setItem("versus.theme", "dark"));
  await openApp(page, "/now");
  const fixture = await historyFixture(page);
  const documentMarker = `k27-r1-${Date.now()}`;
  try {
    await page.goto("/agent/kubernetes?view=timeline");
    await page.locator("html").evaluate((html, marker) => { html.setAttribute("data-k27-document", marker); }, documentMarker);
    await page.getByRole("link", { name: "Open break details", exact: true }).click();
    const chart = page.getByRole("region", { name: "Resource timeline chart", exact: true });
    await expect(chart).toBeVisible();
    await expect(page.getByText("K27 saved evidence", { exact: true })).toBeVisible();
    fixture.denyHistoryAuthority();
    await expect(chart).toHaveCount(0);
    await expect(page.getByText("K27 saved evidence", { exact: true })).toHaveCount(0);
    await page.getByRole("tab", { name: "Overview", exact: true }).click();
    await expect(page.getByRole("region", { name: "Cluster health", exact: true })).toBeVisible();
    await expect(page.locator("[data-sync-state]")).not.toContainText(/Failed|Preparing/);
    await expect(page.getByRole("button", { name: "Timeline for qa-checkout", exact: true })).toHaveCount(0);
    await capture(page, 1440, "dark", "r1-denied-history-current-lane-intercepted");
    fixture.restoreHistoryAuthority({ ...issue, root: { ...issue.root, name: "qa-checkout-renewed" } });
    await expect(page.getByRole("button", { name: "Timeline for qa-checkout-renewed", exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: "Timeline for qa-checkout", exact: true })).toHaveCount(0);
    await capture(page, 1440, "dark", "r1-reauthorized-fresh-history-intercepted");
    await expect(page.locator("html"), "typed history authority recovery must not require a document reload").toHaveAttribute("data-k27-document", documentMarker);
  } finally { fixture.release(); await page.unrouteAll({ behavior: "ignoreErrors" }); }
});

test("K27 real SSE lease bounds a revoked licensed session", async ({ page }, testInfo) => {
  test.skip(process.env.E2E_K27_SESSION_LEASE !== "1", "requires the isolated licensed session-revocation fixture");
  test.setTimeout(75_000);
  await openApp(page, "/now");
  const session = (await page.context().cookies()).find(cookie => cookie.name === "versus_enterprise_session");
  expect(Boolean(session?.httpOnly)).toBe(true);
  const url = new URL("/api/admin/kubernetes/commits/stream", page.url()).href;
  const headers = { Cookie: `${session!.name}=${session!.value}` };
  const controller = new AbortController();
  const deadline = setTimeout(() => controller.abort(), 50_000);
  const started = performance.now();
  try {
    const response = await fetch(url, { headers, signal: controller.signal });
    expect(response.status).toBe(200);
    const reader = response.body!.getReader();
    let text = "";
    while (!text.includes("event: committed")) {
      const chunk = await reader.read();
      expect(chunk.done, "the real lease must publish its initial committed snapshot").toBe(false);
      text += new TextDecoder().decode(chunk.value);
    }
    const logout = await page.request.post("/enterprise/api/auth/local/logout", { headers: { Origin: new URL(page.url()).origin } });
    expect([200, 204]).toContain(logout.status());
    let termination = "eof";
    try { while (!(await reader.read()).done) { /* Read until the server closes this real lease. */ } }
    catch (error) { termination = error instanceof Error ? error.name : "stream_closed"; }
    const elapsed = performance.now() - started;
    expect(elapsed, "a revoked session must not retain an unbounded pre-authenticated stream").toBeLessThan(40_000);
    const denied = await fetch(url, { headers, signal: controller.signal });
    expect(denied.status, "a new lease must reject the same now-revoked cookie, not merely an absent cookie").toBe(401);
    await denied.body?.cancel();
    await testInfo.attach("real-session-lease", { body: JSON.stringify({ elapsed_ms: elapsed, initial_committed: true, termination, revoked_session_next_status: denied.status, clock: "real", client: "independent of browser navigation" }), contentType: "application/json" });
  } finally { clearTimeout(deadline); controller.abort(); }
});

test("K27 real ten-minute collector blackhole preserves history and recovers (W1)", async ({ page }, testInfo) => {
  test.skip(process.env.E2E_K27_BLACKHOLE !== "1", "requires the isolated project-owned kind network fixture");
  test.setTimeout(900_000);
  const project = process.env.HARNESS_PROJECT ?? "";
  expect(project.startsWith("k27-licensed-"), "never disconnect a shared harness network").toBe(true);
  const network = `${project}_default`;
  const node = `${process.env.E2E_KIND_CLUSTER ?? "versus-harness"}-control-plane`;
  const overview = await page.context().newPage();
  const detail = await page.context().newPage();
  const pages = [page, overview, detail];
  const failures: Array<{ path: string; query: string; status: number }> = [];
  const currentFailures: Array<{ path: string; status: number }> = [];
  const uiFailures: Array<{ elapsed_ms: number; timeline: boolean; detail: boolean; tile: string | null; skeletons: Array<string | null> }> = [];
  const syncLabels = new Set<string>();
  const durations: number[] = [];
  let completedHistoryReads = 0;
  for (const surface of pages) {
    await surface.setViewportSize({ width: 1440, height: 1000 });
    await surface.emulateMedia({ colorScheme: "dark" });
    await surface.addInitScript(() => localStorage.setItem("versus.theme", "dark"));
    surface.on("response", async response => {
      const url = new URL(response.url());
      const path = url.pathname;
      if (!/\/api\/admin\/kubernetes\/(timeline|issues|changes|crashes|break-logs)$/.test(path)) return;
      if (path.endsWith("/issues") && url.searchParams.get("context") !== "persisted"
        && !["since", "until", "cursor"].some(param => url.searchParams.has(param))) {
        if (response.status() >= 400) currentFailures.push({ path, status: response.status() });
        return;
      }
      if (response.status() >= 400) failures.push({ path, query: url.search, status: response.status() });
      if (await response.finished().catch(() => new Error("cancelled")) !== null) return;
      completedHistoryReads++;
      const timing = response.request().timing();
      if (timing.responseEnd >= timing.requestStart && timing.requestStart >= 0) durations.push(timing.responseEnd - timing.requestStart);
    });
  }
  let disconnected = false;
  let failureStarted = 0;
  let failureDuration = 0;
  try {
    await openApp(page, "/agent/kubernetes?view=timeline");
    await page.getByRole("tab", { name: "Breaks only", exact: true }).click();
    const breakLink = page.getByRole("link", { name: "Open break details", exact: true }).first();
    await expect(breakLink).toBeVisible();
    const target = await breakLink.getAttribute("href");
    expect(target).toBeTruthy();
    await openApp(overview, "/agent/kubernetes");
    const tile = overview.getByRole("region", { name: "Break timeline", exact: true }).getByRole("button", { name: /Changes in 60 min/ });
    await expect(tile).not.toContainText("Unknown");
    await openApp(detail, new URL(target!, page.url()).href);
    const chart = detail.getByRole("region", { name: "Resource timeline chart", exact: true });
    await expect(chart).toBeVisible();
    await expect(detail.getByText(/\d+ lines from \d+ pods in this window/)).toBeVisible();
    for (const surface of pages) await watchHistorySkeletons(surface);
    const readsBeforeFailure = completedHistoryReads;
    await execute("docker", ["network", "disconnect", network, node]);
    disconnected = true;
    failureStarted = performance.now();
    await expect.soft(overview.locator("[data-sync-state]")).toContainText("Cluster unreachable", { timeout: 120_000 });
    await capture(page, 1440, "dark", "w1-blackhole-timeline-live");
    await capture(overview, 1440, "dark", "w1-blackhole-overview-live");
    await capture(detail, 1440, "dark", "w1-blackhole-detail-live");
    while (performance.now() - failureStarted < 600_000) {
      await new Promise(resolve => setTimeout(resolve, Math.min(15_000, 600_000 - (performance.now() - failureStarted))));
      const sample = {
        elapsed_ms: performance.now() - failureStarted,
        timeline: await breakLink.isVisible(),
        detail: await chart.isVisible(),
        tile: await tile.textContent({ timeout: 1000 }).catch(() => null),
        skeletons: await Promise.all(pages.map(surface => surface.locator("html").getAttribute("data-k27-skeletons"))),
      };
      syncLabels.add((await overview.locator("[data-sync-state]").textContent()) ?? "");
      if (!sample.timeline || !sample.detail || sample.tile === null || sample.tile.includes("Unknown") || sample.skeletons.some(count => count !== "0")) uiFailures.push(sample);
    }
    failureDuration = performance.now() - failureStarted;
    expect(failureDuration).toBeGreaterThanOrEqual(600_000);
    expect(completedHistoryReads - readsBeforeFailure, "history must actually refresh during the outage").toBeGreaterThanOrEqual(10);
    await execute("docker", ["network", "connect", network, node]);
    disconnected = false;
    await expect.soft(overview.locator("[data-sync-state]")).toContainText(/Last synced|Updates disconnected/, { timeout: 120_000 });
    await capture(overview, 1440, "dark", "w1-recovered-overview-live");
    const sorted = [...durations].sort((a, b) => a - b);
    await testInfo.attach("real-ten-minute-blackhole", { body: JSON.stringify({ clock: "real", outage_ms: failureDuration, completed_history_reads: completedHistoryReads, reads_during_outage: completedHistoryReads - readsBeforeFailure, history_failures: failures, current_issue_failures: currentFailures, ui_failures: uiFailures, observed_sync_labels: [...syncLabels], observed_history_p95_ms: sorted[Math.max(0, Math.ceil(sorted.length * 0.95) - 1)], snapshots: 3 }), contentType: "application/json" });
    expect(uiFailures).toEqual([]);
    expect(failures).toEqual([]);
  } finally {
    if (disconnected) await execute("docker", ["network", "connect", network, node]);
    await Promise.all([overview.close(), detail.close()]);
  }
});
