import { expect, test, type TestInfo } from "@playwright/test";
import { openApp, primaryNav } from "./helpers";

type AgentKind = "chat" | "analyze";
type ToolSetting = { group: string; name: string; enabled: boolean };
type ToolSettingsSnapshot = { agent: AgentKind; settings: ToolSetting[] };

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
      await expect(page.getByRole("heading", { name: "Agent tools" })).toBeVisible();
      const groups = page.locator("main section > div:first-child h2");
      await expect(groups).toHaveText(["Connectors", "Data Source Tools", "Common"]);
      await expect(page.locator("main article")).toHaveCount(7);
      await expect(page.getByText("get_incident", { exact: true })).toHaveCount(0);
      await expect(page.getByText("get_cluster_overview", { exact: true })).toHaveCount(0);
      await expect(page.getByRole("heading", { name: "Kubernetes" })).toBeVisible();
      await expect(page.getByText("9 tools", { exact: true })).toBeVisible();
      await expect(page.getByText("needs permission", { exact: true })).toBeVisible();
      await expect(page.getByText("Kubernetes is unavailable: infrastructure:view permission is required", { exact: true })).toBeVisible();

      const unavailableToggle = page.getByLabel("Enable Kubernetes for chat");
      await expect(unavailableToggle).toBeVisible();
      await expect(unavailableToggle).not.toBeChecked();
      await expect(unavailableToggle).toBeDisabled();

      const cards = page.locator("main article");
      await expect(page.getByRole("link", { name: "Documentation" })).toHaveCount(7);
      const docs = page.getByRole("link", { name: "Documentation" });
      for (let index = 0; index < await docs.count(); index++) {
        await expect(docs.nth(index)).toHaveAttribute("target", "_blank");
        await expect(docs.nth(index)).toHaveAttribute("rel", "noopener noreferrer");
      }
      const runbookCard = cards.filter({ hasText: "Find runbook" });
      await expect(runbookCard.getByRole("link", { name: "Open tool" })).toHaveAttribute("href", "/agent/runbooks");
      const licensedCard = cards.filter({ hasText: "Metrics" });
      await expect(licensedCard.getByRole("link", { name: "Open tool" })).toHaveCount(0);
      const permissionCard = cards.filter({ hasText: "Kubernetes" });
      await expect(permissionCard.getByRole("link", { name: "Open tool" })).toHaveCount(0);
      await expect(permissionCard.getByRole("link", { name: "Documentation" })).toBeVisible();

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

  test("surfaces a disabled Versus recovery card", async ({ page }, testInfo) => {
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
      await expect(page.getByRole("heading", { name: "Versus core" })).toBeVisible();
      await expect(page.getByLabel("Enable Versus core for chat")).toBeEnabled();
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
      await expect(page.getByRole("heading", { name: "Agent tools" })).toBeVisible();
      const cards = page.locator("main article");
      await expect(cards).toHaveCount(7);
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