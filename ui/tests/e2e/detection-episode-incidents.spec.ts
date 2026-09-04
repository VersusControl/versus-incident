import { expect, test, type Page, type Route } from "@playwright/test";

const incidentID = "incident-episode-500";
const lastSeen = "2026-09-03T14:05:06Z";

const originCounts = { ai_detect: 1, webhook: 0, total: 1 };
const zeroCounts = { ai_detect: 0, webhook: 0, total: 0 };
const incident = {
  id: incidentID,
  title: "Checkout failures",
  source: "agent:scripted-detection-source",
  origin: "ai_detect",
  service: "checkout",
  resolved: false,
  notify_status: "sent",
  created_at: "2026-09-03T14:00:00Z",
  detection_fingerprint: "v1:synthetic-fingerprint",
  detection_episode_id: "episode-synthetic-500",
  occurrence_count: 500,
  detection_first_seen: "2026-09-03T14:00:00Z",
  detection_last_seen: lastSeen,
  highest_observed_severity: "high",
  highest_notified_severity: "high",
  content: {
    AlertName: "Checkout failures",
    Summary: "Checkout requests are failing",
    Severity: "high",
    PatternID: "checkout-failure-pattern",
    PatternTemplate: "service=checkout request failed status=<*>",
    Frequency: 1,
    Verdict: "unknown",
    ServiceName: "checkout",
  },
};

async function json(route: Route, value: unknown, status = 200) {
  await route.fulfill({ status, contentType: "application/json", body: JSON.stringify(value) });
}

async function installEpisodeApi(page: Page) {
  await page.route("**/api/**", async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;

    if (path === "/api/auth/gateway-session" && request.method() === "POST") {
      if (request.headers()["x-gateway-secret"] !== "qa-episode-secret") {
        return json(route, { error: "unauthorized" }, 401);
      }
      return route.fulfill({
        status: 204,
        headers: { "Set-Cookie": "versus_gateway_session=episode-session; Path=/; HttpOnly; SameSite=Strict" },
        body: "",
      });
    }
    if (path === "/api/admin/config/agent") {
      if (!request.headers().cookie?.includes("versus_gateway_session=episode-session")) {
        return json(route, { error: "unauthorized" }, 401);
      }
      return json(route, {});
    }
    if (path.endsWith("/deployment")) return json(route, { error: "community" }, 403);
    if (path === "/api/admin/incidents/counts") {
      return json(route, {
        ...originCounts,
        count_window: { mode: "all" },
        by_status: { open: originCounts, acked: zeroCounts, resolved: zeroCounts, all: originCounts },
      });
    }
    if (path === "/api/admin/incidents") {
      return json(route, {
        incidents: [incident],
        counts: {
          ...originCounts,
          by_status: { open: originCounts, acked: zeroCounts, resolved: zeroCounts, all: originCounts },
        },
        count_window: { mode: "all" },
        total: 1,
        offset: 0,
        next_offset: null,
        page: 1,
        page_size: 1000,
      });
    }
    if (path === `/api/admin/incidents/${incidentID}`) return json(route, incident);
    if (path === `/api/admin/incidents/${incidentID}/analyses`) return json(route, { analyses: [] });
    if (path === "/api/admin/capabilities") return json(route, { search: false, report: false });
    if (path === "/api/admin/teams") return json(route, { teams: [] });
    if (path === "/api/admin/members") return json(route, { members: [] });
    if (path === "/api/agent/baselines") return json(route, { error: "not found" }, 404);
    return json(route, {});
  });
}

async function signIn(page: Page) {
  await page.getByLabel("Gateway secret").fill("qa-episode-secret");
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page.getByTestId("app-authenticated")).toBeVisible();
}

test("incident list and detail show cumulative occurrences and last seen", async ({ page }) => {
  await installEpisodeApi(page);
  await page.goto("/incidents");
  await signIn(page);

  await expect(page.getByText(/500 occurrences · last seen/i)).toBeVisible();
  const listLastSeen = page.getByText(/500 occurrences · last seen/i);
  await expect(listLastSeen).toContainText("500 occurrences");

  await page.getByRole("link", { name: "Checkout failures" }).click();
  await expect(page).toHaveURL(new RegExp(`/incidents/${incidentID}$`));

  const agentContext = page.locator(".card").filter({ hasText: "Agent context" });
  await expect(agentContext).toContainText("Frequency");
  await expect(agentContext.getByText("500", { exact: true })).toBeVisible();

  const lastSeenLabel = page.locator("dt", { hasText: "Last seen" });
  await expect(lastSeenLabel).toBeVisible();
  const lastSeenValue = lastSeenLabel.locator("xpath=following-sibling::dd[1]").locator("span");
  await expect(lastSeenValue).toHaveAttribute("title", /^2026-09-03 \d{2}:05:06$/);
});