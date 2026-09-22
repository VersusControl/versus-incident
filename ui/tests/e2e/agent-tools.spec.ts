import { expect, test, type TestInfo } from "@playwright/test";
import { openApp, primaryNav } from "./helpers";

type AgentKind = "chat" | "analyze";
type ToolSetting = { group: string; name: string; enabled: boolean };
type ToolSettingsSnapshot = { agent: AgentKind; settings: ToolSetting[] };

const REQUIRED_CARD_NAMES = [
  "Kubernetes", "Source control",
  "Logs tools", "File", "Loki", "CloudWatch Logs", "Graylog", "Splunk", "SigNoz Logs", "Elasticsearch",
  "Metrics tools", "Prometheus", "CloudWatch Metrics", "SigNoz Metrics",
  "Trace tools", "Grafana Tempo", "SigNoz Traces",
  "Find runbook", "Describe dependencies", "Describe baseline",
] as const;
const DEVELOPMENT_PROVIDERS = ["File", "Loki", "CloudWatch Logs", "Graylog", "Splunk", "CloudWatch Metrics"] as const;
const PROVIDER_LOGOS = {
  Elasticsearch: "/elasticsearch.svg",
  Loki: "/loki.svg",
  Graylog: "/graylog.svg",
  Splunk: "/splunk.svg",
  "SigNoz Logs": "/signoz.svg",
  "SigNoz Metrics": "/signoz.svg",
  "SigNoz Traces": "/signoz.svg",
  Prometheus: "/prometheus.svg",
  "Grafana Tempo": "/tempo.svg",
} as const;

async function expectStableCatalog(page: import("@playwright/test").Page) {
  const cards = page.locator("main article");
  const versusCore = cards.filter({ has: page.getByRole("heading", { name: "Versus core", exact: true }) });
  const stableCards = cards.filter({ hasNot: page.getByRole("heading", { name: "Versus core", exact: true }) });
  await expect(stableCards).toHaveCount(20);
  expect(await versusCore.count()).toBeLessThanOrEqual(1);

  for (const name of REQUIRED_CARD_NAMES) {
    await expect(page.getByRole("heading", { name, exact: true })).toHaveCount(1);
    await expect(page.getByRole("heading", { name, exact: true })).toBeVisible();
  }
  for (const name of ["Logs tools", "Metrics tools", "Trace tools"]) {
    await expect(page.getByRole("heading", { name, exact: true })).toHaveCount(1);
  }
  for (const provider of DEVELOPMENT_PROVIDERS) {
    const card = cards.filter({ has: page.getByRole("heading", { name: provider, exact: true }) });
    await expect(card.getByText("Development", { exact: true })).toHaveCount(1);
  }
  await expect(page.getByText("Development", { exact: true })).toHaveCount(6);
  await expect(page.getByRole("heading", { name: "Elasticsearch", exact: true })).toHaveCount(1);
  await expect(page.getByRole("heading", { name: "SigNoz", exact: true })).toHaveCount(0);
  for (const name of ["SigNoz Logs", "SigNoz Metrics", "SigNoz Traces"]) {
    await expect(page.getByRole("heading", { name, exact: true })).toHaveCount(1);
  }
  for (const [provider, src] of Object.entries(PROVIDER_LOGOS)) {
    const card = cards.filter({ has: page.getByRole("heading", { name: provider, exact: true }) });
    await expect(card.locator(`img[src="${src}"]`)).toHaveCount(1);
  }
}

async function setToolEnabled(page: import("@playwright/test").Page, agent: AgentKind, name: string, enabled: boolean) {
  return page.evaluate(async ({ agentKind, toolName, target }) => {
    const response = await fetch(`/api/admin/agent/tools/${agentKind}/${encodeURIComponent(toolName)}`, {
      method: "PUT",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ enabled: target }),
    });
    return { ok: response.ok, status: response.status, body: await response.text() };
  }, { agentKind: agent, toolName: name, target: enabled });
}

async function captureVersusSettings(page: import("@playwright/test").Page, agent: AgentKind): Promise<ToolSettingsSnapshot> {
  const result = await page.evaluate(async (agentKind) => {
    const response = await fetch(`/api/admin/agent/tools?agent=${agentKind}`, { credentials: "same-origin" });
    return { ok: response.ok, status: response.status, body: await response.text() };
  }, agent);
  expect(result, `capture ${agent}: ${result.status} ${result.body}`).toMatchObject({ ok: true });
  const rows = JSON.parse(result.body) as ToolSetting[];
  return { agent, settings: rows.filter((row) => row.group === "versus") };
}

async function enableVersusDefaults(page: import("@playwright/test").Page, snapshot: ToolSettingsSnapshot) {
  for (const setting of snapshot.settings.filter((row) => !row.enabled)) {
    const result = await setToolEnabled(page, snapshot.agent, setting.name, true);
    expect(result, `enable ${snapshot.agent}/${setting.name}: ${result.status} ${result.body}`).toMatchObject({ ok: true });
  }
}

async function restoreVersusSettings(
  page: import("@playwright/test").Page,
  testInfo: TestInfo,
  primaryError: unknown,
  ...snapshots: ToolSettingsSnapshot[]
) {
  const failures: string[] = [];
  for (const snapshot of snapshots) {
    for (const setting of snapshot.settings) {
      try {
        const result = await setToolEnabled(page, snapshot.agent, setting.name, setting.enabled);
        if (!result.ok) failures.push(`${snapshot.agent}/${setting.name}: ${result.status} ${result.body}`);
      } catch (error) {
        failures.push(`${snapshot.agent}/${setting.name}: ${String(error)}`);
      }
    }
  }
  if (failures.length === 0) return;

  const message = `failed to restore Versus settings:\n${failures.join("\n")}`;
  if (primaryError !== undefined) {
    testInfo.annotations.push({ type: "cleanup failure", description: message });
    await testInfo.attach("Versus settings restoration failures", {
      body: message,
      contentType: "text/plain",
    }).catch((error) => {
      testInfo.annotations.push({ type: "cleanup attachment failure", description: String(error) });
    });
    return;
  }
  throw new Error(message);
}

async function overrideToolStates(page: import("@playwright/test").Page, agent: AgentKind, overrides: Record<string, { state: string; reason?: string }>) {
  await page.route(`**/api/admin/agent/toolsets?agent=${agent}`, async (route) => {
    const response = await route.fetch();
    if (!response.ok()) {
      await route.fulfill({ response });
      return;
    }
    const rows = await response.json() as Array<Record<string, unknown>>;
    await route.fulfill({ response, json: rows.map((row) => {
      const override = overrides[String(row.id)];
      return override ? { ...row, ...override } : row;
    }) });
  });
}

test.describe("Agent tool catalog", () => {
  test("renders authoritative groups, actions, and per-agent controls", async ({ page }, testInfo) => {
    await openApp(page, "/agent/tools");
    const chatSettings = await captureVersusSettings(page, "chat");
    const analyzeSettings = await captureVersusSettings(page, "analyze");
    let primaryError: unknown;

    try {
      await overrideToolStates(page, "chat", {
        metrics: { state: "needs_license" },
        kubernetes: { state: "needs_permission", reason: "infrastructure:view permission is required" },
      });
      await enableVersusDefaults(page, chatSettings);
      await enableVersusDefaults(page, analyzeSettings);
      await page.reload();
      await expect(page.getByRole("heading", { name: "Tool catalog" })).toBeVisible();
      await expect(page.getByRole("heading", { level: 2 })).toHaveText(["Connectors", "Data Source Tools", "Common"]);
      await expectStableCatalog(page);
      await expect(page.getByRole("textbox", { name: "Search tools" })).toHaveCount(0);
      await expect(page.getByRole("tablist")).toHaveCount(0);
      await expect(page.getByText("get_incident", { exact: true })).toHaveCount(0);
      await expect(page.getByText("get_cluster_overview", { exact: true })).toHaveCount(0);
      await expect(page.getByRole("heading", { name: "Kubernetes" })).toBeVisible();
      await expect(page.getByText("9 tools", { exact: true })).toHaveCount(0);
      await expect(page.getByText("No access", { exact: true })).toBeVisible();
      await expect(page.getByText("infrastructure:view permission is required", { exact: false })).toHaveCount(0);
      await expect(page.getByText("Inspect Kubernetes.", { exact: true })).toHaveCount(0);
      await expect(page.getByRole("checkbox")).toHaveCount(0);

      const rows = page.locator("main article");
      const runbookRow = rows.filter({ hasText: "Find runbook" });
      await expect(runbookRow.getByRole("link", { name: "Open Find runbook" })).toHaveAttribute("href", "/agent/runbooks");
      const licensedRow = rows.filter({ hasText: "Prometheus" });
      await expect(licensedRow.getByRole("link", { name: /^Open / })).toHaveCount(0);
      const permissionRow = rows.filter({ hasText: "Kubernetes" });
      await expect(permissionRow.getByRole("link", { name: /^Open / })).toHaveCount(0);
      await permissionRow.getByRole("button", { name: "Kubernetes settings" }).click();
      const details = page.getByRole("dialog", { name: "Kubernetes" });
      await expect(details).toContainText("No access");
      await expect(details).toContainText("infrastructure:view permission is required");
      await expect(details.getByRole("link", { name: "Documentation" })).toBeVisible();
      await details.getByRole("button", { name: "Close dialog" }).click();

      const nav = primaryNav(page);
      await expect(nav.getByRole("link", { name: "Runbooks", exact: true })).toHaveCount(0);
      const chatIsInRespond = await nav.getByText("Respond", { exact: true }).evaluate((heading) => {
        let sibling = heading.nextElementSibling;
        while (sibling?.tagName === "A") {
          if (sibling.textContent?.trim() === "Chat") return true;
          sibling = sibling.nextElementSibling;
        }
        return false;
      });
      expect(chatIsInRespond).toBe(true);

      await page.getByRole("button", { name: "analyze" }).click();
      await expect(page.getByText("get_incident", { exact: true })).toHaveCount(0);
    } catch (error) {
      primaryError = error;
      throw error;
    } finally {
      await restoreVersusSettings(page, testInfo, primaryError, analyzeSettings, chatSettings);
    }
  });

  test("surfaces disabled Versus recovery details in settings", async ({ page }, testInfo) => {
    await openApp(page, "/agent/tools");
    const chatSettings = await captureVersusSettings(page, "chat");
    let primaryError: unknown;

    try {
      const priorEnabled = chatSettings.settings.find((row) => row.name === "get_incident")?.enabled;
      expect(priorEnabled).not.toBeUndefined();
      const enable = await setToolEnabled(page, "chat", "get_incident", true);
      expect(enable, `${enable.status} ${enable.body}`).toMatchObject({ ok: true });
      await page.reload();
      await expect(page.getByText("get_incident", { exact: true })).toHaveCount(0);

      const disable = await setToolEnabled(page, "chat", "get_incident", false);
      expect(disable, `${disable.status} ${disable.body}`).toMatchObject({ ok: true });
      await page.reload();
      await expect(page.getByText("get_incident", { exact: true })).toHaveCount(0);
      await expect(page.getByRole("heading", { name: "Describe dependencies" })).toBeVisible();
      await page.getByRole("button", { name: "Describe dependencies settings" }).click();
      const details = page.getByRole("dialog", { name: "Describe dependencies" });
      await expect(details.getByRole("checkbox")).toBeDisabled();
      await expect(details).toContainText(/Setup required|not configured|unavailable/i);
    } catch (error) {
      primaryError = error;
      throw error;
    } finally {
      await restoreVersusSettings(page, testInfo, primaryError, chatSettings);
    }
  });

  test("stays readable at a mobile viewport", async ({ page }, testInfo) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await openApp(page, "/agent/tools");
    const chatSettings = await captureVersusSettings(page, "chat");
    const analyzeSettings = await captureVersusSettings(page, "analyze");
    let primaryError: unknown;

    try {
      await enableVersusDefaults(page, chatSettings);
      await enableVersusDefaults(page, analyzeSettings);
      await page.reload();
      await expect(page.getByRole("heading", { name: "Tool catalog" })).toBeVisible();
      const cards = page.locator("main article");
      await expectStableCatalog(page);
      const first = await cards.first().boundingBox();
      const second = await cards.nth(1).boundingBox();
      expect(first).not.toBeNull();
      expect(second).not.toBeNull();
      expect(Math.abs((first?.x ?? 0) - (second?.x ?? 0))).toBeLessThan(2);

      await page.getByRole("button", { name: "analyze" }).click();
      await expect(page.getByText("get_incident", { exact: true })).toHaveCount(0);
    } catch (error) {
      primaryError = error;
      throw error;
    } finally {
      await restoreVersusSettings(page, testInfo, primaryError, analyzeSettings, chatSettings);
    }
  });
});