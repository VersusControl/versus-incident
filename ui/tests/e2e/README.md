# Browser e2e — versus-incident (OSS) admin SPA

> **Review-first.** These [Playwright](https://playwright.dev) specs drive a
> **real running** versus-incident (OSS) instance like an operator. They do
> **not** start a server and are **not** meant to run in CI unattended — bring
> an instance up yourself and run them against it. Read this README + the specs
> before running anything.

## What this is

Specs that open the OSS admin SPA (served embedded by the OSS binary),
authenticate with the single-admin **gateway secret**, and assert on the
recently-shipped UI surfaces:

| File | Surface under test |
|---|---|
| `report-schedule.spec.ts` | Settings → Detection & reports — enabling the schedule reveals/enables `send_time` + timezone; UTC↔Local + send time persist across a reload. |
| `incident-intake.spec.ts` | Incidents page → Webhook origin tab — the "Auto-resolve" toggle is absent on the AI-detected tab, appears on the Webhook tab loading default ON, and persists across a reload. |
| `ui-refresh.spec.ts` | Settings and Admin one-section layouts, and the Logs / Metrics / Traces filter panel and Flat / By service views, with desktop + mobile screenshots. |
| `helpers.ts` | Sign-in (gateway secret or Enterprise local admin) + resilient locators. All env-sourced; nothing secret hardcoded. |
| `playwright.config.ts` | Config — base URL, timeouts, headless/headful. Does **not** start a server. |
| `.env.example` | Required env — copy to `.env` (gitignored) and fill. |

### Selectors + logged testid gaps

None of these surfaces carry a stable `data-testid` today, so the specs
use the best available stable hooks (the primary-nav landmark, visible
labels/roles, and the existing `#rs-send-time` id). QA does **not** edit product
code to add hooks — the exact `data-testid`s requested from the Front-End are
logged in [`../../../../plans/productization/global/qa-defects/QA-043.md`](../../../../plans/productization/global/qa-defects/QA-043.md).
When those land, only `helpers.ts` needs updating; the specs stay put.

## Auth model

`helpers.ts` → `openApp` signs in only when the browser context has no valid
session cookie, so reload tests reuse the session:

- **OSS** shows the gateway-secret form. `E2E_GATEWAY_SECRET` must equal the
  server's `GATEWAY_SECRET`.
- **Licensed Enterprise** shows the local-admin form (`local-login-*`). It uses
  `E2E_ADMIN_USERNAME` / `E2E_ADMIN_PASSWORD`, which harness-run captures from
  the first-boot banner. The gateway secret is retired in Enterprise.

## Bring up an instance and run

Use the QA harness. It builds the app (and its embedded SPA) from local source
and passes the base URL and the right credentials to Playwright:

```sh
harness-run/harness.sh up oss                     # or enterprise-prometheus, enterprise-tempo, …
harness-run/harness.sh e2e ui <spec> --project=chromium --reporter=list
harness-run/harness.sh down
```

Without the harness, build `ui/dist`, run `GATEWAY_SECRET=<secret> go run ./cmd`
from `versus-incident/`, copy `.env.example` to `.env`, and run `npm run e2e`.
First time only: `npx playwright install chromium`.

- Set `E2E_HEADFUL=true` to watch the run locally.

## Notes

- The report-schedule + incident-intake specs **mutate persisted runtime
  settings** and reload to prove persistence, so the file runs serially
  (`workers: 1`). Each mutating case restores the value it changed, so a re-run
  starts clean.
- The unit layer (`npm test`, vitest) covers the same logic hermetically
  (`reportSchedule.test.ts`, `ReportSettingsControl.test.tsx`,
  `IncidentsConfigPage.test.tsx`, `Sidebar.test.tsx`) and can run any time; this
  browser layer proves the surfaces behave end to end against the real binary.
