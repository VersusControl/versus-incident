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
