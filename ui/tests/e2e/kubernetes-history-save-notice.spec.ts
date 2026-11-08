// Browser acceptance for the saved-history notice.
//
// The cluster is read but the latest capture could not be saved, so every
// history surface must say so once: a calm delay notice for a retryable save,
// a warning-tone rejection for a save the store refused for good. Rows that
// were already published stay on screen, and the notice never borrows the
// wording of an access, credential or read failure.
//
// Every capture here is route-intercepted on top of a real authenticated OSS
// session: the page bodies are deterministic fixtures, not a live cluster.
// The captures are labelled `intercepted`.
import { expect, test, type Locator, type Page, type Route } from "@playwright/test";
import { openApp } from "./helpers";
import { capturePath } from "./support/capture";
import type {
  KubernetesCaptureStatus,
  KubernetesChange,
  KubernetesCrashPage,
  KubernetesHistorySave,
  KubernetesIssue,
  KubernetesResource,
  KubernetesSync,
  KubernetesTimelinePage,
} from "../../src/lib/api";

const sync: KubernetesSync = { state: "ready", age_s: 0, partial: false };
const now = Date.now();
const brokeAt = new Date(now - 120_000).toISOString();
const rootUID = "qa-history-root";
const occurrenceID = "qa-history-occurrence";
const resource: KubernetesResource = {
  resource_id: "apps~v1~deployments",
  kind: "Deployment",
  namespace: "shop",
  name: "qa-checkout",
  uid: rootUID,
  created_at: new Date(now - 86_400_000).toISOString(),
  labels: { app: "qa-checkout" },
};
const issue: KubernetesIssue = {
  root: { kind: "Deployment", namespace: "shop", name: "qa-checkout", uid: rootUID },
  rule: "pod.crash_loop",
  severity: "critical",
  count: 2,
  of: 2,
  broken: true,
  state: "open",
  broke_at: brokeAt,
  occurrence_id: occurrenceID,
};
const change: KubernetesChange = {
  id: "qa-history-change",
  cluster: "qa-cluster",
  kind: "Deployment",
  namespace: "shop",
  name: "qa-checkout",
  uid: rootUID,
  type: "image_changed",
  fields: [{ path: "image", from: "qa/checkout:v41", to: "qa/checkout:v42" }],
  at: new Date(now - 120_000 - 60_000).toISOString(),
};

type Kind = "delayed" | "rejected";
type SavedThrough = "yesterday" | "none";
type Config = { kind: Kind; savedThrough: SavedThrough };

function saveFor({ kind, savedThrough }: Config): KubernetesHistorySave {
  const at = new Date(now - 26 * 3_600_000).toISOString();
  return {
    code: kind === "rejected" ? "history_save_rejected" : "history_save_delayed",
    message: kind === "rejected"
      ? "The cluster was read, but the history store rejected the latest capture."
      : "The cluster was read, but the latest capture has not been saved to history yet.",
    action: kind === "rejected"
      ? "Check the Versus server logs; saved history does not advance until the store accepts the capture."
      : "Saved history catches up once the history store accepts writes again.",
    retryable: kind !== "rejected",
    ...(savedThrough === "yesterday" ? { saved_through: at } : {}),
  };
}

function captureFor(config: Config): KubernetesCaptureStatus {
  const save = saveFor(config);
  return { mode: "postgres", behind: config.kind === "delayed", history_save: save };
}

function syncReport() {
  return {
    ...sync,
    age_s: 0,
    first_ready_at: brokeAt,
    kinds: {},
    ready_kinds: 1,
    total_kinds: 1,
  };
}

function overview() {
  return {
    connector: "kubernetes",
    cluster_id: "qa-cluster",
    observed_at: new Date().toISOString(),
    nodes: 1,
    ready_nodes: 1,
    pods: 2,
    running_pods: 2,
    namespaces: 1,
    active_namespaces: 1,
    workloads: 1,
    warnings: 0,
    requested_cpu: "750m",
    limited_cpu: "2",
    allocatable_cpu: "4",
    requested_memory: "1Gi",
    limited_memory: "2Gi",
    allocatable_memory: "8Gi",
    usage_cpu: "500m",
    usage_memory: "3Gi",
    usage_source: "pod_metrics",
    metrics_status: "available",
    metrics_observed_at: new Date().toISOString(),
    metrics_fresh: true,
    truncated: false,
    sync,
  };
}

function issuePage(config: Config) {
  return {
    items: [issue],
    totals: { critical: 1, warning: 0, info: 0 },
    broken: { total: 1, by_severity: { critical: 1 } },
    truncated: false,
    sync,
    capture: captureFor(config),
  };
}

function timelinePage(config: Config, url: URL): KubernetesTimelinePage {
  return {
    items: [{ type: "break", id: "qa-history-break", at: brokeAt, issue }],
    total: 1,
    crash_count: 0,
    counts: { change: 0, crash: 0, break: 1 },
    gaps: [],
    sync,
    since: url.searchParams.get("since") ?? new Date(now - 3_600_000).toISOString(),
    until: url.searchParams.get("until") ?? new Date(now).toISOString(),
    capture: captureFor(config),
  };
}

function crashPage(config: Config, url: URL): KubernetesCrashPage {
  return {
    items: [],
    crash_count: 0,
    since: url.searchParams.get("since") ?? new Date(now - 3_600_000).toISOString(),
    until: url.searchParams.get("until") ?? new Date(now).toISOString(),
    retention_from: new Date(now - 7 * 86_400_000).toISOString(),
    retention_basis: "configured",
    started_at: new Date(now - 86_400_000).toISOString(),
    gaps: [],
    capture_gaps: [],
    coverage: {},
    limitations: [],
    capture: captureFor(config),
    sync,
  };
}

// Installs the deterministic Kubernetes API fixture on one page. `config` is
// mutable so each navigation can ask for a different save state without
// reinstalling routes.
async function installFixture(page: Page, config: Config) {
  const answer = (route: Route, json: unknown, status = 200) => route.fulfill({ json, status });
  await page.route("**/api/admin/kubernetes/**", async route => {
    const url = new URL(route.request().url());
    const path = url.pathname;
    if (route.request().method() !== "GET") return route.fallback();
    if (path.endsWith("/commits/stream")) {
      const committed = { revision: 7, evidence_sequence: 9, kinds: ["deployments"], cluster_id: "" };
      const event = `id: 7\nevent: committed\ndata: ${JSON.stringify(committed)}\n\n`
        + `event: sync\ndata: ${JSON.stringify({ state: "ready", reason: "", age: 2, cluster_id: "" })}\n\n`;
      return route.fulfill({ contentType: "text/event-stream", body: event });
    }
    if (path.endsWith("/clusters")) return answer(route, { multiple: false, clusters: [] });
    if (path.endsWith("/sync")) return answer(route, syncReport());
    if (path.endsWith("/overview")) return answer(route, overview());
    if (path.endsWith("/namespaces")) return answer(route, { items: ["shop"], sync });
    if (path.endsWith("/top")) return answer(route, { items: [], items_available: true, total: 0, truncated: false, availability: "available", fresh: true, sync });
    if (path.endsWith("/releases")) return answer(route, { items: [], truncated: false });
    if (path.endsWith("/traffic")) return answer(route, { available: false, reason: "not_connected", window: url.searchParams.get("window") ?? "15m", unmapped: 0, truncated: false });
    if (path.endsWith("/usage")) {
      const observed = new Date().toISOString();
      return answer(route, {
        observed_at: observed,
        availability: "available",
        fresh: true,
        pod_metrics: { availability: "available", fresh: true, complete: true, total: 2, cpu: "500m", memory: "3Gi", observed_at: observed },
        node_metrics: { availability: "available", fresh: true, complete: true, total: 1, cpu: "2", memory: "8Gi", observed_at: observed },
        pods: [],
        nodes: [],
        truncated: false,
      });
    }
    if (path.endsWith("/issue-alerts")) {
      return answer(route, { enabled: false, level: "critical", can_write: false, available: true, reason: "", links_ready: true, channels_enabled: true });
    }
    if (path.endsWith("/issues")) {
      if (url.searchParams.get("context") === "persisted") {
        return answer(route, {
          items: [],
          totals: { critical: 1 },
          truncated: false,
          sync,
          capture: captureFor(config),
          selection_status: "resolved",
          selected_occurrence: issue,
          root_context: { root: issue.root, occurrence: issue, retained: true },
        });
      }
      return answer(route, issuePage(config));
    }
    if (path.endsWith("/timeline")) return answer(route, timelinePage(config, url));
    if (path.endsWith("/changes")) {
      return answer(route, { items: [change], total: 1, truncated: false, gaps: [], sync, capture: captureFor(config) });
    }
    if (path.endsWith("/crashes")) return answer(route, crashPage(config, url));
    if (path.endsWith("/break-logs")) {
      return answer(route, {
        occurrence_id: occurrenceID,
        window: { from: url.searchParams.get("since"), to: url.searchParams.get("until") },
        pods: [{ name: "qa-checkout-7d9f", uid: "qa-history-pod", state: "running", short: "running" }],
        containers: [{ name: "app", main: true }],
        main_container: "app",
        lines: [{ at: brokeAt, pod: "qa-checkout-7d9f", pod_uid: "qa-history-pod", container: "app", init: false, slot: "current", level: "info", text: "qa-history line", source: "kubernetes" }],
        markers: [],
        counts: { all: 1, errors: 0, warnings: 0 },
        rate: [],
        patterns: [],
        sources: [{ kind: "kubernetes", name: "Kubernetes container logs", type: "kubernetes", at: brokeAt }],
        coverage: { state: "complete", gaps: [], reason: "", tip: false },
      });
    }
    if (path.endsWith("/describe")) {
      return answer(route, { resource, related_resources: [], events: [], partial_failures: [] });
    }
    if (path.endsWith("/workloads")) {
      return answer(route, { items: [resource], continue: "", truncated: false, sync });
    }
    if (/\/workloads\/Deployment\/qa-checkout$/.test(path)) {
      return answer(route, { ...resource, desired: 2, ready: 2, available: 2, unavailable: 0, truncated: false, containers: [{ name: "app", image: "qa/checkout:v42" }], pods: [] });
    }
    return route.fallback();
  });
}

type Surface = {
  id: string;
  label: string;
  url: string;
  section: (page: Page) => Locator;
  rows: (page: Page, section: Locator) => Locator;
};

const surfaces: Surface[] = [
  {
    id: "issues",
    label: "Issues (history)",
    url: "/agent/kubernetes?view=issues",
    section: page => page.locator('section[aria-label="Kubernetes issues"]'),
    rows: (page, section) => section.getByRole("article", { name: "Deployment qa-checkout" }),
  },
  {
    id: "timeline-focused",
    label: "focused Timeline",
    url: `/agent/kubernetes?view=timeline&root_uid=${rootUID}&namespace=shop&kind=Deployment&occurrence_id=${occurrenceID}&center=${encodeURIComponent(brokeAt)}`,
    section: page => page.locator('section[aria-label="Kubernetes change timeline"]'),
    rows: (page, section) => section.getByRole("region", { name: "Resource timeline chart", exact: true }),
  },
  {
    id: "timeline-unfocused",
    label: "unfocused Timeline",
    url: "/agent/kubernetes?view=timeline",
    section: page => page.locator('section[aria-label="Kubernetes change timeline"]'),
    rows: (page, section) => section.getByRole("link", { name: "Open break details", exact: true }),
  },
  {
    id: "overview-timeline-card",
    label: "Overview Timeline card",
    url: "/agent/kubernetes",
    section: page => page.locator('section[aria-label="Break timeline"]'),
    rows: (page, section) => section.getByRole("button", { name: /Changes in 60 min/ }),
  },
  {
    id: "resource-drawer-timeline",
    label: "resource drawer timeline",
    url: "/agent/kubernetes?view=resources&r=apps~v1~deployments/shop/qa-checkout&tab=timeline",
    section: page => page.locator('section[aria-label="Resource change history"]'),
    rows: (page, section) => section.locator('[aria-label="Resource timeline lane"]'),
  },
];

const states: Array<{ id: string; config: Config }> = [
  { id: "delayed-saved-date", config: { kind: "delayed", savedThrough: "yesterday" } },
  { id: "delayed-no-saved-through", config: { kind: "delayed", savedThrough: "none" } },
  { id: "rejected-saved-date", config: { kind: "rejected", savedThrough: "yesterday" } },
  { id: "rejected-no-saved-through", config: { kind: "rejected", savedThrough: "none" } },
];

// The label is formatted in the reader's locale inside the page, so the
// expectation is computed exactly as the product computes it.
async function expectedLabel(page: Page, config: Config): Promise<string> {
  return page.evaluate((save: KubernetesHistorySave) => {
    const lead = save.code === "history_save_rejected" ? "History could not save the latest data" : "History is temporarily not saving";
    if (!save.saved_through) return `${lead} — showing the last saved data`;
    const saved = new Date(save.saved_through);
    const today = new Date();
    const time = saved.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
    if (saved.toDateString() === today.toDateString()) return `${lead} — showing data from ${time}`;
    const date = saved.toLocaleDateString([], { month: "short", day: "numeric", ...(saved.getFullYear() !== today.getFullYear() ? { year: "numeric" } : {}) });
    return `${lead} — showing data from ${date}, ${time}`;
  }, saveFor(config));
}

function noticeOf(section: Locator): Locator {
  return section.locator("[data-history-save-notice]");
}

async function contrastRatio(notice: Locator): Promise<number> {
  return notice.evaluate(el => {
    const parse = (value: string): number[] => {
      const parts = (value.match(/[\d.]+/g) ?? []).slice(0, 3).map(Number);
      return parts.map(channel => {
        const c = channel / 255;
        return c <= 0.03928 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4);
      });
    };
    const luminance = (rgb: number[]) => 0.2126 * rgb[0] + 0.7152 * rgb[1] + 0.0722 * rgb[2];
    const transparent = (value: string) => !value || value === "transparent" || /rgba\(0, 0, 0, 0\)/.test(value);
    const text = getComputedStyle(el.querySelector("p")!).color;
    let background = "";
    for (let node: HTMLElement | null = el as HTMLElement; node; node = node.parentElement) {
      const value = getComputedStyle(node).backgroundColor;
      if (!transparent(value)) { background = value; break; }
    }
    if (transparent(background)) background = getComputedStyle(document.body).backgroundColor;
    const a = luminance(parse(text));
    const b = luminance(parse(background));
    return (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);
  });
}

// Every assertion the rendered notice must pass before its capture is saved.
async function verifyNotice(page: Page, section: Locator, config: Config) {
  const notice = noticeOf(section);
  await expect(notice).toHaveCount(1);
  const rejected = config.kind === "rejected";
  await expect(notice).toHaveAttribute("data-history-save-notice", rejected ? "rejected" : "delayed");
  await expect(notice).toHaveAttribute("role", "status");
  await expect(notice.locator("svg").first(), "the notice icon must be decorative").toHaveAttribute("aria-hidden", "true");
  expect(await notice.locator("button, a, input, select, textarea, [tabindex]").count(), "the notice must not add a focus stop").toBe(0);
  await expect(notice.locator("p").first()).toHaveText(await expectedLabel(page, config));
  if (rejected) {
    await expect(notice.locator("p").nth(1)).toContainText("Check the Versus server logs");
  } else {
    await expect(notice.locator("p")).toHaveCount(1);
  }

  // The data on screen stays the real, published history.
  const body = await section.innerText();
  expect(body, "a save delay must not borrow read, credential or access wording").not.toMatch(
    /credential|could not be resolved|unavailable|Couldn't refresh|Some history could not be read|does not cover the whole range/i,
  );

  // Readable in dark mode at body-text size.
  expect(await contrastRatio(notice)).toBeGreaterThanOrEqual(4.5);

  const geometry = await notice.evaluate(el => {
    const label = el.querySelector("p")!;
    const range = document.createRange();
    range.selectNodeContents(label);
    const text = range.getBoundingClientRect();
    const box = label.getBoundingClientRect();
    const section = el.closest("section")!;
    const panel = section.closest(".resource-history") ?? section;
    return {
      scrollWidth: el.scrollWidth,
      clientWidth: el.clientWidth,
      textRight: text.right,
      labelRight: box.right,
      noticeRight: el.getBoundingClientRect().right,
      sectionScroll: section.scrollWidth,
      sectionClient: section.clientWidth,
      panelScroll: panel.scrollWidth,
      panelClient: panel.clientWidth,
      innerWidth: window.innerWidth,
      documentWidth: document.documentElement.scrollWidth,
    };
  });
  expect(geometry.scrollWidth, "the notice must wrap, never clip").toBeLessThanOrEqual(geometry.clientWidth + 1);
  expect(geometry.textRight, `the notice copy must stay inside the viewport ${JSON.stringify(geometry)}`).toBeLessThanOrEqual(geometry.innerWidth + 1);
  expect(geometry.documentWidth, "no horizontal overflow").toBeLessThanOrEqual(geometry.innerWidth);

  const overlapping = await section.evaluate(root => {
    const noticeRect = root.querySelector("[data-history-save-notice]")!.getBoundingClientRect();
    const hits: string[] = [];
    for (const control of root.querySelectorAll<HTMLElement>("button, a, select, input, textarea, [role=tab]")) {
      if (control.closest("[data-history-save-notice]")) continue;
      const rect = control.getBoundingClientRect();
      if (rect.width === 0 || rect.height === 0) continue;
      const horizontal = Math.min(noticeRect.right, rect.right) - Math.max(noticeRect.left, rect.left);
      const vertical = Math.min(noticeRect.bottom, rect.bottom) - Math.max(noticeRect.top, rect.top);
      if (horizontal > 1 && vertical > 1) hits.push(control.getAttribute("aria-label") || control.textContent?.trim().slice(0, 40) || control.tagName);
    }
    return hits;
  });
  expect(overlapping, "the notice must not cover controls").toEqual([]);
}

async function capture(page: Page, surface: Surface, width: number, state: string) {
  const dir = capturePath(surface.id, width, "dark", `${state}-intercepted`);
  await page.screenshot({ path: dir, animations: "disabled" });
  return dir;
}

const viewports = [
  { name: "desktop", width: 1440, height: 1000 },
  { name: "mobile", width: 390, height: 844 },
] as const;

for (const surface of surfaces) {
  test(`${surface.label} shows the saved-history notice without hiding its rows`, async ({ page }) => {
    test.setTimeout(180_000);
    await page.setViewportSize({ width: viewports[0].width, height: viewports[0].height });
    await page.emulateMedia({ colorScheme: "dark" });
    await page.addInitScript(() => localStorage.setItem("versus.theme", "dark"));
    const config: Config = { kind: "delayed", savedThrough: "yesterday" };
    await installFixture(page, config);
    await openApp(page, "/now");

    for (const viewport of viewports) {
      for (const state of states) {
        config.kind = state.config.kind;
        config.savedThrough = state.config.savedThrough;
        await page.setViewportSize({ width: viewport.width, height: viewport.height });
        await page.goto(surface.url);
        const section = surface.section(page);
        await expect(section, `${surface.label} must render`).toBeVisible({ timeout: 30_000 });
        await expect(surface.rows(page, section).first(), `${surface.label} must keep its rows`).toBeVisible({ timeout: 30_000 });
        // A sliding drawer (or any entrance transition) must settle before the
        // geometry checks; otherwise the panel is measured mid-animation.
        await expect.poll(
          () => section.evaluate(el => { const rect = el.getBoundingClientRect(); return rect.left >= -1 && rect.right <= window.innerWidth + 1; }),
          { message: `${surface.label} must settle inside the viewport` },
        ).toBe(true);
        await verifyNotice(page, section, state.config);
        await noticeOf(section).scrollIntoViewIfNeeded();
        await expect(page.locator("html")).not.toHaveAttribute("data-theme", "light");
        await capture(page, surface, viewport.width, state.id);
      }
    }
  });
}

test("every history surface words the same save state identically", async ({ page }) => {
  test.setTimeout(180_000);
  await page.setViewportSize({ width: 1440, height: 1000 });
  await page.emulateMedia({ colorScheme: "dark" });
  await page.addInitScript(() => localStorage.setItem("versus.theme", "dark"));
  const config: Config = { kind: "delayed", savedThrough: "yesterday" };
  await installFixture(page, config);
  await openApp(page, "/now");

  for (const kind of ["delayed", "rejected"] as const) {
    config.kind = kind;
    config.savedThrough = "yesterday";
    const seen: Array<{ surface: string; label: string }> = [];
    for (const surface of surfaces) {
      await page.goto(surface.url);
      const section = surface.section(page);
      await expect(section).toBeVisible({ timeout: 30_000 });
      await expect.poll(
        () => section.evaluate(el => { const rect = el.getBoundingClientRect(); return rect.left >= -1 && rect.right <= window.innerWidth + 1; }),
        { message: `${surface.label} must settle inside the viewport` },
      ).toBe(true);
      const notice = noticeOf(section);
      await expect(notice).toHaveCount(1);
      await expect(notice.locator("p").first()).toHaveText(await expectedLabel(page, config));
      // The surface's own controls stay keyboard reachable while the notice
      // is on screen; the notice never becomes a focus stop.
      await section.evaluate(el => { (el as HTMLElement).dataset.a11yProbe = "1"; });
      let reached = false;
      for (let press = 0; press < 80 && !reached; press++) {
        await page.keyboard.press("Tab");
        reached = await page.evaluate(() => Boolean(document.activeElement?.closest('[data-a11y-probe="1"]')));
      }
      expect(reached, `${surface.label} controls must be keyboard reachable`).toBe(true);
      seen.push({ surface: surface.label, label: (await notice.locator("p").first().innerText()).trim() });
    }
    expect(new Set(seen.map(entry => entry.label)).size, JSON.stringify(seen)).toBe(1);
  }
});
