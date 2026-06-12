# Admin Dashboard

Versus Incident ships with a built-in **admin dashboard** — a single-page
React app embedded directly into the Go binary. There is no separate UI
process to run; once the server is up, the dashboard is available at the
root path.

![Versus](../docs/images/versus-dashboard-02.png)

## Quick start

```bash
docker run -p 3000:3000 \
  -e GATEWAY_SECRET=change-me \
  -e SLACK_ENABLE=true \
  -e SLACK_TOKEN=$SLACK_TOKEN \
  -e SLACK_CHANNEL_ID=$SLACK_CHANNEL_ID \
  ghcr.io/versuscontrol/versus-incident
```

Then open <http://localhost:3000/> in your browser.

> **Public URL.** When running behind a reverse proxy or in
> Kubernetes, set `public_host` (e.g.
> `public_host: https://versus.example.com`) in `config.yaml` so the
> startup banner and acknowledgement links use the externally-reachable
> address. With `public_host` empty, Versus falls back to
> `http://<host>:<port>`.

> **`GATEWAY_SECRET` is required for the dashboard to function.** All
> admin endpoints (`/api/admin/*` and `/api/agent/*`) are gated by the
> `X-Gateway-Secret` HTTP header. The dashboard prompts you for this
> value the first time you load it and stores it in your browser's
> `localStorage`. With no secret configured the admin endpoints are not
> registered at all.

## What you can do

The dashboard surfaces every persisted incident plus, when the AI agent
is enabled, the full agent-runtime state. It is meant for day-to-day
operations: triaging fresh alerts, acknowledging on-call pages, and
curating the agent's pattern catalog.

### Pages

| Page | Path | What it shows |
|------|------|----------------|
| Dashboard | `/dashboard` | At-a-glance metrics + Agent runtime bar chart, recent incidents, top patterns, recent shadow events. |
| Incidents | `/incidents` | Full incident history (newest first) with filters for open / acked / resolved and a free-text search. |
| Incident detail | `/incidents/:id` | Single incident: title, service, channels notified, on-call status, notify outcome, raw payload. |
| Agent status | `/status` | Worker mode, source count, catalog size, dirty flag. |
| Patterns | `/patterns` | Every pattern the miner has learned (count, verdict, service, rule, last seen). |
| Pattern detail | `/patterns/:id` | One pattern: full template, sample message, edit verdict / tags, delete. |
| Shadow | `/shadow` | NDJSON log of "would-have-alerted" events recorded in shadow mode. |
| Shadow detail | `/shadow/:patternId` | Drill into one shadow event with the matching catalog entry side-by-side. |
| Services | `/services` | Every service the agent has discovered, with first-seen timestamps and grace controls. |
| Runbooks | `/runbooks` | The runbook corpus that backs the `find_runbook` tool. Upload `.md` files, view a runbook's contents, or delete one. |

### Incident lifecycle

Every incident received via `POST /api/incidents` (or the SNS / SQS
listeners) is persisted to the configured storage backend immediately —
**before** the alert fan-out — so a downstream channel failure never
loses the record. Each incident carries:

- `notify_status` — `pending`, `sent`, or `failed` (with `notify_error`
  on failure). Visible as a coloured pill in the incidents table.
- `acked_at` — set when an operator clicks the acknowledge button in
  Slack/Telegram or hits `GET /api/ack/:incidentID`. The dashboard
  reflects the new state on the next poll.
- `resolved` — true when the original payload's `status` / `state` /
  `alertState` field equals `"resolved"`. Resolved alerts skip on-call
  escalation and the `AckURL` injection.

### Agent management

When `agent.enable: true`, the dashboard exposes the agent's full admin
surface without you needing `curl`:

- Browse the **pattern catalog** and assign verdicts (`known`, `spike`,
  custom) or tags so detect-mode emissions stay quiet for the things
  you've already triaged.
- Inspect every **shadow event** — one click takes you from the recent
  feed to a detail page that shows the exact log line, the cluster
  template, and the catalog entry it would have matched.
- Force the worker to **flush** the catalog or shadow log to disk for
  immediate persistence (the worker also flushes periodically — see
  `agent.catalog.persist_interval`).
- See **services** the agent has discovered; end or restart a service's
  grace period without restarting the binary.
- Manage the **runbook corpus** that powers the `find_runbook` tool:
  upload one or more `.md` files at once, view a runbook's contents, or
  delete it. Uploads share the same corpus as the `runbooks/` source
  folder, and re-uploading a file with the same name replaces it. When
  an embedding model is configured (`tools.find_runbook.embedding_model`)
  uploads are embedded and searchable immediately; otherwise they are
  stored but flagged as not yet searchable.

## Where the data lives

Everything the dashboard reads is durable. The default backend writes
JSON to a directory on disk:

```yaml
storage:
  type: file              # file | redis | database (env: STORAGE_TYPE)
  file:
    max_incidents: 1000   # rolling cap on persisted incidents
```

Files inside the `./data` directory (`/app/data` in the container image):

| File | Purpose |
|------|---------|
| `incidents.json` | All persisted incidents (most recent `max_incidents`). |
| `patterns.json` | The agent's pattern catalog and the services map. |
| `shadow.json` | Append-only NDJSON log of shadow events. |

> **Heads-up.** Setting `storage.type: redis` or `database` is currently
> a **config stub** — the provider returns `storage: backend not
> implemented`. Stick with `file` (the default) in production until
> these land.

## Running without the UI

If you only need the API surface (for example, in a tightly-scoped CI
fixture), simply leave `GATEWAY_SECRET` unset. The admin endpoints stay
unregistered and the root path serves a small "UI not built" landing
page that links to `/api/incidents` and `/healthz`. The notification
fan-out is unaffected.
