import { test, expect } from "@playwright/test";
import {
  openApp,
  signInWithGatewaySecret,
  webhookOriginTab,
  autoResolveToggle,
  waitForToast,
} from "./helpers";

// ===========================================================================
// Webhook auto-resolve toggle — Incidents page, webhook origin tab.
//
// The toggle moved off Settings → Alerting (operator feedback): it now lives in
// the Incidents toolbar and is only shown on the WEBHOOK origin tab, labeled
// "Auto-resolve" (the toggle's meaning). It is absent on the AI-detected tab.
//
// Acceptance (unchanged backend):
//   * absent on the default (AI-detected) tab; appears after switching to the
//     Webhook origin tab;
//   * loads default ON (the backend default is auto_resolve_webhook = true);
//   * toggling persists across a reload (save-on-toggle, no explicit Save
//     button).
//
// Selector: the stable `intake-auto-resolve` testid moved with the control onto
// the toolbar checkbox (the old `intake-settings-card` card is gone).
//
// Save-on-toggle mutates a persisted runtime setting; the file runs serially
// (workers:1). The toggle case restores the value to ON at the end so a re-run
// starts from the documented default.
// ===========================================================================

const INCIDENTS = "/incidents";

test.describe("Webhook auto-resolve toggle", () => {
  test("is scoped to the webhook tab and loads default ON", async ({
    page,
  }) => {
    await openApp(page, INCIDENTS);

    // Absent on the default (AI-detected) tab — this is a webhook-only control.
    await expect(autoResolveToggle(page)).toHaveCount(0);

    // Switch to the Webhook origin tab; the toggle appears, default ON.
    await webhookOriginTab(page).click();
    const toggle = autoResolveToggle(page);
    await expect(toggle).toBeVisible();
    await expect(toggle).toBeChecked();
    // Short label only — no "Incident intake" wording.
    await expect(page.getByText("Auto-resolve", { exact: true })).toBeVisible();
  });

  test("toggling persists across a reload", async ({ page }) => {
    await openApp(page, INCIDENTS);
    await webhookOriginTab(page).click();

    // The checkbox is a CONTROLLED input bound to server state with no
    // optimistic update: on click it fires the save and, while the save is in
    // flight, React re-applies the old `checked` value (the input is disabled
    // meanwhile), so the box visually reverts until the round-trip completes.
    // Playwright's .check()/.uncheck() verify the resulting state immediately
    // after the click and would race that revert. So we drive the toggle with a
    // plain .click() (fires onChange, asserts nothing about the transient
    // state) and assert the SETTLED, persisted outcome after the toast + reload.
    const toggle = () => autoResolveToggle(page);

    // Start from the known default (ON). If a prior run left it OFF, restore it
    // and let the save settle back to checked before the real assertions.
    if (!(await toggle().isChecked())) {
      await toggle().click();
      await waitForToast(page, /Intake settings saved/i);
      await expect(toggle()).toBeChecked();
    }

    // Toggle OFF — saves on change — and confirm it persists across a reload.
    // The origin tab is URL-synced (?origin=webhook), so the reload lands back
    // on the webhook tab with the toggle mounted.
    await toggle().click();
    await waitForToast(page, /Intake settings saved/i);
    await expect(toggle()).not.toBeChecked();
    await page.reload();
    await signInWithGatewaySecret(page);
    await expect(toggle()).not.toBeChecked();

    // Toggle back ON — restores the default — and confirm it persists too.
    await toggle().click();
    await waitForToast(page, /Intake settings saved/i);
    await expect(toggle()).toBeChecked();
    await page.reload();
    await signInWithGatewaySecret(page);
    await expect(toggle()).toBeChecked();
  });
});
