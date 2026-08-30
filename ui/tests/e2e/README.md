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
| `helpers.ts` | Gateway-secret seeding + resilient locators. All env-sourced; nothing secret hardcoded. |
| `playwright.config.ts` | Config — base URL, timeouts, headless/headful. Does **not** start a server. |
| `.env.example` | Required env — copy to `.env` (gitignored) and fill. |

### Selectors + logged testid gaps

None of these surfaces carry a stable `data-testid` today, so the specs
use the best available stable hooks (the primary-nav landmark, visible
labels/roles, and the existing `#rs-send-time` id). QA does **not** edit product
code to add hooks — the exact `data-testid`s requested from the Front-End are
logged in [`../../../../plans/productization/global/qa-defects/QA-043.md`](../../../../plans/productization/global/qa-defects/QA-043.md).
When those land, only `helpers.ts` needs updating; the specs stay put.

## Auth model (why a gateway secret)

The OSS SPA exchanges `X-Gateway-Secret` once for an HttpOnly session cookie
(`ui/src/lib/api.ts`). The harness fills the visible AuthGate form only when the
browser context has no valid cookie (`helpers.ts` → `openApp`); reload tests
reuse the existing session without re-entering the secret. The value comes from
`E2E_GATEWAY_SECRET` and **must equal**
the running server's `GATEWAY_SECRET` (config
`gateway_secret: ${GATEWAY_SECRET}`) — never hardcoded.

## Bring up an OSS instance

### Option A — the run/ harness (rebuilds the SPA embed)

```sh
cd run && ./oss.sh        # builds the image (rebuilds ui/dist + //go:embed), starts it
# OSS up on http://localhost:8080  (GATEWAY_SECRET = dev-gateway-secret, run/env/shared.env)
```

Tear it down when done (never leave a stack running after a review):

```sh
docker compose -f run/docker-compose.yml --profile oss down --remove-orphans
```

### Option B — local `go run` (build the SPA first)

The binary `//go:embed`s `ui/dist`, so build the SPA before running the server,
or the embedded bundle is stale/empty:

```sh
cd versus-incident/ui && npm install && npm run build   # produces ui/dist
cd .. && GATEWAY_SECRET=dev-gateway-secret go run ./cmd  # serves the embed on :8080
```

## Run the specs (once you have an instance)

```sh
cd versus-incident/ui

# First time only — install Playwright's browser:
npm install && npx playwright install chromium

# Configure + run:
cp tests/e2e/.env.example tests/e2e/.env   # set E2E_BASE_URL + E2E_GATEWAY_SECRET
npm run e2e
```

- `E2E_BASE_URL` **must** be the running instance's origin (default
  `http://localhost:8080`).
- `E2E_GATEWAY_SECRET` **must** equal the server's `GATEWAY_SECRET`
  (run/ harness default `dev-gateway-secret`) — the settings pages 401 without
  it and the specs fail fast with a clear message.
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
