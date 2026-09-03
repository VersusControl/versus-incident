import { expect, test } from "@playwright/test";
import { openApp } from "./helpers";

test.describe("Kubernetes real cluster", () => {
  test.skip(process.env.E2E_KUBERNETES_BACKEND !== "real", "requires the isolated real-cluster harness");

  test("shows the grouped catalog and live operational facts without route interception", async ({ page }) => {
    const pageErrors: Error[] = [];
    page.on("pageerror", (error) => pageErrors.push(error));
    await openApp(page, "/agent/tools");
    await expect(page.locator("main section > div:first-child h2")).toHaveText(["Connectors", "Data Source Tools", "Common"]);
    await expect(page.locator("main article")).toHaveCount(7);
    await expect(page.getByText("get_cluster_overview", { exact: true })).toHaveCount(0);

    const kubernetesCard = page.locator("article").filter({ has: page.getByRole("heading", { name: "Kubernetes" }) });
    await expect(kubernetesCard.getByText("9 tools", { exact: true })).toBeVisible();
    await kubernetesCard.getByRole("link", { name: "Open tool" }).click();

    await expect(page).toHaveURL(/\/agent\/kubernetes$/);
    await expect(page.getByText("docker-desktop-qa", { exact: false })).toBeVisible();
    await expect(page.getByRole("heading", { name: "Workloads" })).toBeVisible();
    await page.getByRole("textbox", { name: "Namespace", exact: true }).fill("versus-dc6-allowed");
    await expect(page.getByText("qa-web", { exact: true }).first()).toBeVisible();
    await expect(page.getByLabel("Kubernetes topology graph")).toBeVisible();

    await page.getByLabel("Resource name").fill("qa-web");
    await page.getByRole("button", { name: "Search" }).click();
    await expect(page.getByRole("heading", { name: "Search results" }).locator("..")).toContainText("qa-web");
    expect(pageErrors).toEqual([]);
  });

  test("keeps the live cluster page usable on mobile", async ({ page }) => {
    const pageErrors: Error[] = [];
    page.on("pageerror", (error) => pageErrors.push(error));
    await page.setViewportSize({ width: 390, height: 844 });
    await openApp(page, "/agent/kubernetes");
    await expect(page.getByText("docker-desktop-qa", { exact: false })).toBeVisible();
    await expect(page.getByLabel("Kubernetes topology graph")).toBeVisible();
    const main = await page.locator("main").boundingBox();
    expect(main?.width).toBeLessThanOrEqual(390);
    const overflows = await page.locator("main button, main input, main [role=listitem]").evaluateAll((elements) =>
      elements.some((element) => {
        const box = element.getBoundingClientRect();
        return box.width > 0 && (box.left < 0 || box.right > document.documentElement.clientWidth + 1);
      }),
    );
    expect(overflows).toBe(false);
    expect(pageErrors).toEqual([]);
  });
});