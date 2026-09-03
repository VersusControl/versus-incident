import { expect, test } from "@playwright/test";
import { openApp } from "./helpers";

test.describe("Kubernetes overview", () => {
  test.skip(process.env.E2E_KUBERNETES_BACKEND !== "fake", "requires a fresh Versus backend connected to the deterministic fake Kubernetes API");

  test("shows bounded partial cluster evidence and resource search", async ({ page }) => {
    await openApp(page, "/agent/tools");
    const kubernetesCard = page.locator("article").filter({ has: page.getByRole("heading", { name: "Kubernetes" }) });
    await expect(kubernetesCard.getByRole("link", { name: "Open tool" })).toHaveCount(1);
    await kubernetesCard.getByRole("link", { name: "Open tool" }).click();
    await expect(page).toHaveURL(/\/agent\/kubernetes$/);
    await expect(page.getByRole("heading", { name: "Kubernetes" }).last()).toBeVisible();
    await expect(page.getByText(/limited-cluster/)).toBeVisible();
    await expect(page.getByText("Nodes ready 2/3")).toBeVisible();
    await expect(page.getByText(/Metrics available/)).toBeVisible();
    await expect(page.getByRole("heading", { name: "Workloads" })).toBeVisible();
    await expect(page.getByText("api", { exact: true }).first()).toBeVisible();
    await expect(page.getByText("Unhealthy", { exact: true }).first()).toBeVisible();
    await expect(page.getByLabel("Kubernetes topology graph")).toHaveAttribute("tabindex", "0");
    await expect(page.getByRole("listitem", { name: /scheduled_on.*observed/ }).first()).toBeVisible();
    await page.getByRole("textbox", { name: "Namespace", exact: true }).fill("payments");
    await page.getByLabel("Resource name").fill("api");
    await page.getByRole("button", { name: "Search" }).click();
    const searchResults = page.getByRole("heading", { name: "Search results" }).locator("..");
    await expect(searchResults).toBeVisible();
    const podResult = searchResults.getByRole("button", { name: "api Pod payments", exact: true });
    await expect(podResult).toBeVisible();
    await expect(searchResults.getByRole("status")).toContainText(/forbidden/);
    await podResult.click();
    const resourceDetail = page.getByRole("heading", { name: "Resource detail" }).locator("..");
    await expect(resourceDetail).toBeVisible();
    await expect(resourceDetail.getByText("Pod payments/api", { exact: true })).toBeVisible();
    await expect(resourceDetail.getByText(/restarts 1/)).toBeVisible();
    await expect(resourceDetail.getByText("Diagnostic events: 2", { exact: true })).toBeVisible();
    await expect(page.getByText(/secret-token/)).toHaveCount(0);

    const logs = await page.evaluate(async () => {
      const response = await fetch("/api/admin/kubernetes/pods/payments/api/logs?container=api&tail_lines=20");
      return response.json() as Promise<{ text: string }>;
    });
    expect(logs.text).not.toContain("super-secret-value");
    expect(logs.text).toContain("REDACTED");

    const projections = await page.evaluate(async () => {
      const [secret, configMap] = await Promise.all([
        fetch("/api/admin/kubernetes/resources/core~v1~secrets/api-secret/describe?namespace=payments").then((response) => response.text()),
        fetch("/api/admin/kubernetes/resources/core~v1~configmaps/api-config/describe?namespace=payments").then((response) => response.text()),
      ]);
      return { secret, configMap };
    });
    expect(projections.secret).not.toContain("c2VjcmV0LXRva2Vu");
    expect(projections.secret).toContain("token");
    expect(projections.configMap).not.toContain("must-not-cross");
    expect(projections.configMap).toContain("config.yaml");

  });

  test("remains coherent on mobile", async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await openApp(page, "/agent/kubernetes");
    await expect(page.getByText(/limited-cluster/)).toBeVisible();
    const main = await page.locator("main").boundingBox();
    expect(main?.width).toBeLessThanOrEqual(390);
    await expect(page.getByLabel("Kubernetes topology graph")).toBeVisible();
    const overlaps = await page.locator("main button, main input, main [role=listitem]").evaluateAll((elements) => {
      const boxes = elements.map((element) => element.getBoundingClientRect()).filter((box) => box.width > 0 && box.height > 0);
      return boxes.some((box) => box.left < 0 || box.right > document.documentElement.clientWidth + 1);
    });
    expect(overlaps).toBe(false);
  });
});