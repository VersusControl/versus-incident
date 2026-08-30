import { test, expect } from "@playwright/test";
import { openApp, reportCard, signInWithGatewaySecret, waitForToast } from "./helpers";

// ===========================================================================
// Report schedule control — Settings → Detection & reports (?tab=tuning),
// the "Incidents report" card's "Scheduled delivery" group.
//
// Acceptance (from the shipped change):
//   * enabling the schedule reveals/enables `send_time` + the timezone control
//     (both are disabled while the schedule is off);
//   * switching UTC ↔ Local updates the STORED value (UTC → "UTC";
//     Local → the browser IANA zone) — proven by the selected radio surviving
//     a save + reload;
//   * Save persists across a reload.
//
// Selectors: stable data-testids landed (QA-043) — `report-schedule-enabled`,
// `report-send-time`, `report-timezone-utc`, `report-timezone-local`,
// `report-settings-save`. The scoped "Incidents report" card is kept only for
// the card-visibility check.
//
// These cases MUTATE a persisted runtime setting; the file runs serially
// (workers:1) so they don't race the intake spec.
// ===========================================================================

const TUNING = "/settings?tab=tuning";

test.describe("Report schedule control", () => {
  test("enabling the schedule reveals/enables send time + timezone", async ({
    page,
  }) => {
    await openApp(page, TUNING);

    const card = reportCard(page);
    await expect(card).toBeVisible();

    const scheduleToggle = page.getByTestId("report-schedule-enabled");
    const sendTime = page.getByTestId("report-send-time");
    const utc = page.getByTestId("report-timezone-utc");
    const local = page.getByTestId("report-timezone-local");

    // Get to the OFF baseline (client-side only — no save here).
    if (await scheduleToggle.isChecked()) {
      await scheduleToggle.uncheck();
    }
    // Off: the time input and both timezone radios are disabled.
    await expect(sendTime).toBeDisabled();
    await expect(utc).toBeDisabled();
    await expect(local).toBeDisabled();

    // Enabling reveals/enables the controls.
    await scheduleToggle.check();
    await expect(sendTime).toBeEnabled();
    await expect(utc).toBeEnabled();
    await expect(local).toBeEnabled();
  });

  test("UTC↔Local selection + send time persist across a reload", async ({
    page,
  }) => {
    await openApp(page, TUNING);

    const scheduleToggle = page.getByTestId("report-schedule-enabled");
    const sendTime = page.getByTestId("report-send-time");
    const utc = page.getByTestId("report-timezone-utc");
    const local = page.getByTestId("report-timezone-local");
    const save = page.getByTestId("report-settings-save");

    // Enable the schedule so the fields are editable.
    if (!(await scheduleToggle.isChecked())) {
      await scheduleToggle.check();
    }

    // Set a known send time and select UTC, then save.
    await sendTime.fill("09:30");
    await utc.check();
    await save.click();
    await waitForToast(page, /Report settings saved/i);

    // Reload: the stored UTC selection + send time survive.
    await page.reload();
    await signInWithGatewaySecret(page);
    await expect(scheduleToggle).toBeChecked();
    await expect(utc).toBeChecked();
    await expect(sendTime).toHaveValue("09:30");

    // Switch to Local, save, reload — the stored value flips to Local.
    await local.check();
    await save.click();
    await waitForToast(page, /Report settings saved/i);

    await page.reload();
  await signInWithGatewaySecret(page);
    await expect(local).toBeChecked();
    await expect(sendTime).toHaveValue("09:30");
  });
});
