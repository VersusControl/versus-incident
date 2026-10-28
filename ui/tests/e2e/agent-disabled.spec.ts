import { expect, test, type Page, type Route } from "@playwright/test";

async function json(route: Route, body: unknown, status = 200) {
  await route.fulfill({ status, contentType: "application/json", body: JSON.stringify(body) });
}

async function installDisabledAgent(page: Page) {
  await page.route("**/api/**", async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    if (path === "/api/auth/gateway-session" && request.method() === "POST") {
      return route.fulfill({ status: 204, headers: { "Set-Cookie": "versus_gateway_session=disabled-agent-test; Path=/; HttpOnly; SameSite=Strict" }, body: "" });
    }
    if (path === "/api/admin/config/agent") return json(route, { enable: false, mode: "detect", ai: { enable: false }, sources: [] });
    if (path.endsWith("/deployment") || path.includes("/sso/")) return json(route, { error: "community" }, 403);
    if (path === "/api/admin/agent/toolsets") return json(route, []);
    if (path === "/api/admin/agent/tools") return json(route, []);
    return json(route, { error: "not found" }, 404);
  });
}

async function signIn(page: Page, path: string) {
  await page.goto(path);
  const secret = page.getByLabel("Gateway secret");
  if (await secret.isVisible().catch(() => false)) {
    await secret.fill("disabled-agent-test");
    await page.getByRole("button", { name: "Sign in", exact: true }).click();
  }
  await expect(page.getByTestId("app-authenticated")).toBeVisible();
}

test("disabled agent routes show guidance instead of endpoint 404 errors", async ({ page }) => {
  await installDisabledAgent(page);
  await signIn(page, "/agent");

  for (const path of ["/agent", "/agent/chat", "/agent/services", "/agent/logs", "/agent/decisions", "/analyses"]) {
    await page.goto(path);
    await expect(page.getByRole("heading", { name: "AI Agent is disabled" })).toBeVisible();
    await expect(page.getByText(/AGENT_ENABLE=true/)).toBeVisible();
    await expect(page.getByText(/HTTP 404/)).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Check again" })).toBeVisible();
  }

  await page.goto("/agent/tools");
  await expect(page.getByRole("heading", { name: "AI Agent is disabled" })).toHaveCount(0);
  await expect(page.getByRole("heading", { name: /Tool catalog/i })).toBeVisible();
});
