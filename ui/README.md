# Versus Incident — Admin UI

A Datadog-style admin console for the Versus AI agent. Built with Vite +
React + TypeScript + Tailwind. Talks to the Go backend's `/api/agent/*`
endpoints using the `X-Gateway-Secret` header.

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
root of `config.yaml` as `gateway_secret` (env `GATEWAY_SECRET`). The value is stored in
`localStorage` under `versus.gatewaySecret`. Click **Sign out** in the
sidebar to clear it.

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

Every API call attaches `X-Gateway-Secret: <stored value>`. A 401 response
clears the gate and re-prompts the user.

> **Heads up:** the secret is stored in `localStorage`. Anyone with browser
> access on this machine can read it. The agent admin endpoints are admin-
> level — host the UI behind your usual operator auth (VPN, SSO proxy,
> etc.) rather than exposing it to the public internet.
