import { expect, test } from "@playwright/test";
import { openApp } from "./helpers";

test.describe("Agent tool catalog", () => {
  test("renders authoritative groups, actions, and per-agent controls", async ({ page }) => {
    await openApp(page, "/agent/tools");

    await expect(page.getByRole("heading", { name: "Agent tools" })).toBeVisible();
    const groups = page.locator("main section > div:first-child h2");
    await expect(groups).toHaveText(["Versus", "Common", "Kubernetes"]);
    await expect(page.getByText("get_cluster_overview", { exact: true })).toBeVisible();
    await expect(page.getByText("needs integration", { exact: true }).first()).toBeVisible();
    await expect(page.getByRole("link", { name: "Connect Kubernetes" }).first()).toBeVisible();

    const incidentToggle = page.getByLabel("Enable Incident details for chat");
    await expect(incidentToggle).toBeEnabled();
    await expect(page.getByLabel("Enable Cluster overview for chat")).toBeDisabled();

    await page.getByRole("button", { name: "analyze" }).click();
    await expect(page.getByLabel("Enable Incident details for analyze")).toBeEnabled();
  });

  test("stays readable at a mobile viewport", async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await openApp(page, "/agent/tools");
    await expect(page.getByRole("heading", { name: "Agent tools" })).toBeVisible();
    const cards = page.locator("main article");
    await expect(cards).toHaveCount(22);
    const first = await cards.first().boundingBox();
    const second = await cards.nth(1).boundingBox();
    expect(first).not.toBeNull();
    expect(second).not.toBeNull();
    expect(Math.abs((first?.x ?? 0) - (second?.x ?? 0))).toBeLessThan(2);
  });
});