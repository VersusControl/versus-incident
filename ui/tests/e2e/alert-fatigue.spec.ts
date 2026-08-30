import { test, expect, type Page } from "@playwright/test";

// alert-fatigue.spec.ts — browser e2e for the Enterprise alert-fatigue operator
// surface, driven like a real admin against a RUNNING enterprise console (the
// run/ harness maps 127.0.0.1:${ENTERPRISE_PORT:-3000}). It does NOT start a
// server — bring one up first (cd run && ./enterprise.sh) and point
// E2E_BASE_URL at it.
//
// Auth model: the enterprise binary authenticates the admin surface with a
// server session minted by the built-in local-admin login form
// (data-testid `local-login-*`). The one-time admin password is PRINTED ONCE to
// the first-boot log; the harness captures it and exports E2E_ADMIN_PASSWORD —
// it is NEVER hardcoded here. Absent that credential the suite skips (it must
// never run against a guessed/blank password).
//
// What it proves:
//   • default-deny in the UI — an unauthenticated visitor never reaches the
//     live controls (the enable/channel/pending card is absent; a sign-in
//     affordance is shown instead);
//   • the live admin controls render and the enable / channel / pending-review
//     switches round-trip and PERSIST across a reload;
//   • the fingerprint review table renders the recorded fingerprints, the
//     detail peek opens, and the page raises no uncaught console errors.

const env = {
  baseURL: process.env.E2E_BASE_URL ?? "http://localhost:3000",
  adminUsername: process.env.E2E_ADMIN_USERNAME ?? "admin",
  // Not defaulted on purpose — a spec that needs it skips when absent.
  adminPassword: process.env.E2E_ADMIN_PASSWORD ?? "",
} as const;

const AF_PATH = "/agent/alert-fatigue";

// signInAsAdmin drives the built-in local-admin login form and waits for the
// authenticated shell marker. Shared precondition for every "as admin" case.
async function signInAsAdmin(page: Page): Promise<void> {
  await page.goto(env.baseURL + "/");
  await page.getByTestId("local-login-username").fill(env.adminUsername);
  await page.getByTestId("local-login-password").fill(env.adminPassword);
  await page.getByTestId("local-login-submit").click();
  await expect(page.getByTestId("app-authenticated")).toBeVisible({
    timeout: 30_000,
  });
}

// collectConsoleErrors attaches a listener that records genuine page errors and
// console.error lines, filtering the benign network noise a live console emits
// (a 4xx a query retries, a favicon miss). Returns a getter for the survivors.
function collectConsoleErrors(page: Page): () => string[] {
  const errors: string[] = [];
  const benign = /Failed to load resource|net::ERR_|favicon/i;
  page.on("console", (m) => {
    if (m.type() === "error" && !benign.test(m.text())) errors.push(m.text());
  });
  page.on("pageerror", (e) => errors.push(String(e)));
  return () => errors;
}

test.describe("alert-fatigue operator surface", () => {
  test.skip(
    env.adminPassword === "",
    "E2E_ADMIN_PASSWORD not provided — capture it from the first-boot log before running.",
  );

  test("unauthenticated visitor is gated out of the live controls", async ({
    page,
  }) => {
    // No login: a fresh context has no session cookie.
    await page.goto(env.baseURL + AF_PATH);

    // Default-deny: the live enable/channel/pending card must NOT render for an
    // unauthenticated visitor.
    await expect(page.getByTestId("alert-fatigue-config")).toHaveCount(0);

    // …and a sign-in affordance is shown instead (the page's own sign-in notice
    // or the app's local-login form — either proves the surface is closed).
    const signInNotice = page.locator(
      '[data-testid="admin-access-notice"][data-reason="sign-in"]',
    );
    const loginForm = page.getByTestId("local-login-form");
    await expect(signInNotice.or(loginForm).first()).toBeVisible({
      timeout: 15_000,
    });
  });

  test("admin sees the live controls and enable/channel/pending round-trip and persist", async ({
    page,
  }) => {
    await signInAsAdmin(page);
    await page.goto(env.baseURL + AF_PATH);

    const configCard = page.getByTestId("alert-fatigue-config");
    await expect(configCard).toBeVisible();

    const enable = page.getByTestId("alert-fatigue-enable-toggle");
    await expect(enable).toBeVisible();

    // Ensure the feature is ON so the channel + pending controls mount.
    if ((await enable.getAttribute("aria-checked")) !== "true") {
      await enable.click();
    }
    await expect(enable).toHaveAttribute("aria-checked", "true");

    // Pick a fatigue channel.
    const channel = page.getByTestId("alert-fatigue-channel-select");
    await expect(channel).toBeVisible();
    await channel.selectOption("slack");
    await expect(channel).toHaveValue("slack");

    // Turn on "require review before spam" and confirm the operator note shows.
    const pending = page.getByTestId("alert-fatigue-pending-toggle");
    if ((await pending.getAttribute("aria-checked")) !== "true") {
      await pending.click();
    }
    await expect(pending).toHaveAttribute("aria-checked", "true");
    await expect(page.getByTestId("alert-fatigue-pending-note")).toBeVisible();

    // Reload and prove the settings persisted server-side (not just local UI).
    await page.reload();
    await expect(page.getByTestId("alert-fatigue-enable-toggle")).toHaveAttribute(
      "aria-checked",
      "true",
    );
    await expect(page.getByTestId("alert-fatigue-channel-select")).toHaveValue(
      "slack",
    );
    await expect(page.getByTestId("alert-fatigue-pending-toggle")).toHaveAttribute(
      "aria-checked",
      "true",
    );
  });

  test("review table renders recorded fingerprints and the detail peek opens", async ({
    page,
  }) => {
    const errors = collectConsoleErrors(page);
    await signInAsAdmin(page);
    await page.goto(env.baseURL + AF_PATH);

    await expect(page.getByTestId("alert-fatigue-config")).toBeVisible();

    // Filter the review list to fatigued fingerprints (the API flow left at
    // least one confirmed-spam fingerprint behind).
    const statusFilter = page.getByTestId("alert-fatigue-status-filter");
    await expect(statusFilter).toBeVisible();
    await statusFilter.selectOption("fatigued");

    // A data row renders (the review table uses the ddt class); open the first
    // row's detail peek (the service-name cell is the peek trigger) and assert
    // the panel surfaces the fingerprint detail.
    const firstRow = page.locator("table.ddt tbody tr").first();
    await expect(firstRow).toBeVisible({ timeout: 15_000 });
    await firstRow.locator("td").first().locator("button").click();
    await expect(
      page.getByText("Fingerprint detail", { exact: false }),
    ).toBeVisible({ timeout: 15_000 });

    // The surface raised no uncaught console errors while operating it.
    expect(errors()).toEqual([]);
  });
});
