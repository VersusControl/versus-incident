# Versus Incident — Admin UI

A Datadog-style admin console for the Versus AI agent. Built with Vite +
React + TypeScript + Tailwind. Talks to the Go backend's `/api/agent/*`
endpoints using an HttpOnly gateway-session cookie.

## Screens

| Path           | What it shows                                                  |
| -------------- | -------------------------------------------------------------- |
| `/status`      | Live tiles: patterns learned, shadow events, signals, services |
| `/patterns`    | Pattern catalog: search, filter by verdict, jump to detail     |
| `/patterns/:id`| Verdict + tags editor, delete, full template, EWMA, timestamps |
| `/shadow`      | Shadow log: filter by `spike` / `unknown`, flush or clear      |
| `/services`    | Discovered services with first-seen + grace control buttons    |

## Run locally

```bash
# 1. start the Go backend in another terminal so /api is reachable
./run.sh                            # in the repo root

# 2. install + start the UI
cd ui
npm install
npm run dev                         # → http://localhost:5173
```

The Vite dev server proxies `/api/*` to `http://localhost:3000` (the agent).
Override the target with `VITE_API_PROXY_TARGET=http://other:3000 npm run dev`.

On first load the app prompts for the gateway secret you configured at the
root of `config.yaml` as `gateway_secret` (env `GATEWAY_SECRET`). The value is
sent once to the gateway-session exchange and is not retained by the UI. The
opaque cookie lasts up to eight hours, so reloads and new tabs on the same
origin remain signed in. Click **Sign out** to revoke it.

## Build for production

```bash
npm run build
npm run preview        # serves the built bundle on :4173
```

The build output is `ui/dist/`. Serve it from any static host, or behind
the Go server with a reverse-proxy rule. For a same-origin deployment, set
`VITE_API_BASE_URL=""` (the default) so requests use relative paths.

## Layout

```
ui/
├── index.html
├── src/
│   ├── main.tsx              # bootstraps QueryClient + Router
│   ├── App.tsx               # routes
│   ├── index.css             # Tailwind + global components (.card, .ddt, .pill)
│   ├── lib/
│   │   ├── api.ts            # typed client for /api/agent/*
│   │   ├── auth.tsx          # AuthGate: secret prompt + verification
│   │   └── format.ts         # date helpers
│   ├── components/
│   │   ├── AppShell.tsx      # sidebar + content layout
│   │   ├── Sidebar.tsx       # dark left rail
│   │   ├── TopBar.tsx        # page title + agent-online dot
│   │   ├── Pill.tsx          # pill / VerdictPill
│   │   └── feedback.tsx      # Spinner / EmptyState / ErrorBox
│   └── pages/
│       ├── StatusPage.tsx
│       ├── PatternsPage.tsx
│       ├── PatternDetailPage.tsx
│       ├── ShadowPage.tsx
│       └── ServicesPage.tsx
├── tailwind.config.js
├── postcss.config.js
├── tsconfig*.json
└── vite.config.ts
```

## Design tokens

The Tailwind config defines a small Datadog-ish palette:

- `ink.50–950` — neutral grayscale (bg + text)
- `accent` — violet (`#7e57ff`)
- `good` / `warn` / `bad` — green / amber / red status colors

Reusable components are declared as `@layer components` in `index.css`:

- `.card` / `.card-header` / `.card-body`
- `.ddt` — dense, sticky-headered admin table
- `.pill`, `.pill-good|warn|bad|accent` — status badges
- `.btn`, `.btn-primary`, `.btn-danger`
- `.stat-card` for metric tiles
- `.input` for compact form controls

## Auth

Browser sign-in sends `X-Gateway-Secret` only to
`POST /api/auth/gateway-session`. Subsequent calls use the opaque
`versus_gateway_session` cookie with `credentials: "same-origin"`; JavaScript
cannot read it. A 401 response re-prompts the user after expiry or gateway
secret rotation. Unsafe cookie-authenticated requests require an exact
same-origin `Origin` or `Referer`; direct API clients using the header remain
compatible and are not subject to this browser-cookie check.

For a TLS-terminating or Host-rewriting proxy, configure the backend's
root `public_host` to the exact browser-visible HTTP(S) origin. It also owns
external links and secure-cookie derivation. The backend does not implicitly
trust `X-Forwarded-Proto` or `X-Forwarded-Host`. Invalid configured origins fail
startup.

> **Heads up:** the agent admin endpoints are admin-level. Host the UI behind
> your usual operator auth (VPN, SSO proxy, etc.) rather than exposing it
> publicly.

### Persisted Kubernetes history

Timeline keys contain filters, range minutes and cursors, not sliding clock
bounds. A page chain pins its bounds; background first-page refreshes advance
them without replacing loaded rows with a skeleton. Failed refreshes retain
last-good rows and tile values with a timestamp. Permission denial, unknown
authority or an identity mismatch clears protected data instead.

Overview, Issues and Timeline pass their stored break into the focused view.
Context confirmation runs in the background independently of window history
reads; transient store failures do not disable seeded detail or break logs.
Unseeded deep links resolve the date-free persisted context.

The explorer consumes `/api/admin/kubernetes/commits/stream` using the existing
authenticated fetch transport. Commit bursts are coalesced, duplicate revisions
are ignored within a connection, and reconnect snapshots invalidate missed
updates even after a server restart. Disconnected streams retry with backoff and
refresh queries every 30 seconds; sync status belongs in the top bar. Existing
Playwright explorer scenarios and `tests/e2e/support/capture.ts` remain the
shared browser/capture fixtures.

History counts may be bounded samples. A capped aggregate displays `1000+`;
sampled subtotals are lower bounds, including an unestablished zero. Continuation
pages retain metadata only from their exact cursor chain, never from an unrelated
first-page refresh.

Focused break links and log selectors use the occurrence's break time, not a
later journal observation. Equivalent URL timestamps retain their raw
nanosecond precision. Compact range controls wrap on narrow screens, and a
failed context refresh with retained evidence shows one refresh note and Retry.

An unestablished persisted proof returns retryable `503 history_not_established`;
the commit feed reconnects without marking independently authorized Overview or
sync-health reads denied. A positive `403 history_authority_denied` clears
protected history and seeds while keeping that live recovery lane reachable.
Only fresh authorized reads restore rows; pre-denial last-good data is fenced.
Application/session denials remain fail-closed.

The current sync-health failure takes precedence over older stream progress.
Cluster timeouts show an unreachable caption anchored to the last-good
observation timestamp, not a stale zero-age value or the current clock.
