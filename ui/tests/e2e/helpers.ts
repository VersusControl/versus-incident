import { expect, type Locator, type Page } from "@playwright/test";

// ---------------------------------------------------------------------------
// Shared helpers for the versus-incident SPA browser e2e. Everything
// environment-specific (the base URL, credentials) comes from env, loaded by
// playwright.config.ts or set by `plans/harness-run/harness.sh e2e ui` — never
// hardcoded here.
//
// Auth model: OSS exchanges `X-Gateway-Secret` once for an HttpOnly session
// cookie; a licensed Enterprise build shows the local-admin login form instead.
// Reloads in the same browser context reuse the session cookie.
// ---------------------------------------------------------------------------

export const env = {
  baseURL: process.env.E2E_BASE_URL ?? "http://localhost:8080",
  // The OSS single-admin gateway secret (== the server's GATEWAY_SECRET).
  gatewaySecret: process.env.E2E_GATEWAY_SECRET ?? "",
  // Enterprise built-in admin, captured by harness-run from the boot banner.
  adminUsername: process.env.E2E_ADMIN_USERNAME ?? "admin",
  adminPassword: process.env.E2E_ADMIN_PASSWORD ?? "",
  licensed: process.env.E2E_LICENSED === "1",
};

export async function signInWithGatewaySecret(page: Page): Promise<void> {
  const secretField = page.getByLabel("Gateway secret");
  const localLogin = page.getByTestId("local-login-form");
  const surface = await Promise.race([
    secretField.waitFor({ state: "visible", timeout: 30_000 }).then(() => "secret" as const),
    localLogin.waitFor({ state: "visible", timeout: 30_000 }).then(() => "local" as const),
    page.getByTestId("app-authenticated").waitFor({ state: "visible", timeout: 30_000 }).then(() => "authed" as const),
  ]);
  if (surface === "authed") return;
  if (surface === "local") {
    if (!env.adminPassword) {
      throw new Error(
        "E2E_ADMIN_PASSWORD is required for an Enterprise build — run the suite " +
          "through `plans/harness-run/harness.sh e2e ui`, which sets it from the boot banner.",
      );
    }
    await page.getByTestId("local-login-username").fill(env.adminUsername);
    await page.getByTestId("local-login-password").fill(env.adminPassword);
    await page.getByTestId("local-login-submit").click();
  } else {
    if (!env.gatewaySecret) {
      throw new Error(
        "E2E_GATEWAY_SECRET is required — run the suite through " +
          "`plans/harness-run/harness.sh e2e ui`, or set it in tests/e2e/.env.",
      );
    }
    await secretField.fill(env.gatewaySecret);
    await page.getByRole("button", { name: "Sign in", exact: true }).click();
  }
  await expect(page.getByTestId("app-authenticated")).toBeVisible({
    timeout: 30_000,
  });
}

// openApp navigates to `path` and signs in only when this context has no valid
// session cookie.
export async function openApp(page: Page, path = "/now"): Promise<void> {
  await page.goto(path);
  await signInWithGatewaySecret(page);
}

// ---------------------------------------------------------------------------
// Locators for the three surfaces under test. The Front-End landed the stable
// `data-testid` hooks requested in qa-defects/QA-043.md, so these locators now
// anchor to durable testids instead of copy/DOM-coupled selectors. Centralised
// here so a future testid rename is a one-line change and the specs stay put.
// ---------------------------------------------------------------------------

// primaryNav is the sidebar navigation landmark (aria-label="Primary"). On a
// desktop viewport only the rail renders it (the mobile drawer is not in the
// DOM), so scoping here is unambiguous. Still used to assert the ABSENCE of a
// navigable <a> for the in-development placeholders.
export function primaryNav(page: Page): Locator {
  return page.locator('nav[aria-label="Primary"]');
}

// reportCard scopes to the "Incidents report" settings card (Settings →
// Detection & reports). The card has no card-level testid, but its controls do
// (`report-*`); the "Incidents report" heading scopes the card for visibility
// checks and its individual controls are resolved by testid (see report spec).
export function reportCard(page: Page): Locator {
  return page
    .locator(".card")
    .filter({ has: page.getByRole("heading", { name: /Incidents report/i }) });
}

// webhookOriginTab is the Incidents-page origin filter tab that scopes the
// list (and the auto-resolve toggle) to inbound webhook incidents. The
// SegmentedControl renders each option as role="tab"; the webhook label is
// "Webhook / Alerts" (originLabel).
export function webhookOriginTab(page: Page): Locator {
  return page.getByRole("tab", { name: /Webhook/i });
}

// autoResolveToggle is the "Auto-resolve" checkbox in the Incidents toolbar. It
// only mounts on the webhook origin tab; its stable testid is `intake-auto-resolve`.
export function autoResolveToggle(page: Page): Locator {
  return page.getByTestId("intake-auto-resolve");
}

// waitForToast waits for a Toast with the given title text to appear — used to
// confirm a settings save round-tripped to the server before we reload to prove
// persistence.
export async function waitForToast(page: Page, title: RegExp): Promise<void> {
  await expect(page.getByText(title).first()).toBeVisible({ timeout: 15_000 });
}
