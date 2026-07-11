# Migrating to v1.5.0

This guide covers the changes introduced in Versus Incident v1.5.0.
Most deployments need **no config changes** — but v1.5.0 ships one
**default-on behavior change** for webhook intake that operators should
read before upgrading.

## Key Changes in v1.5.0

### 1. Webhook auto-resolve is ON by default (behavior change)

> **Operator behavior-change notice.** Starting in v1.5.0, incidents
> created through the webhook intake (`POST /api/incidents`) are
> **stored as resolved** by default. This is the new intake setting
> `auto_resolve_webhook`, which defaults to **ON**. If your workflow
> expects every webhook incident to remain in the open list until it is
> acknowledged, opt out (see below).

**What this changes:** a webhook incident no longer sits in the
open-incidents list out of the box — it's recorded as resolved as soon
as it's created. The goal is to stop noisy or spammy webhook sources
from accumulating as open incidents you then have to close by hand.

**What it does *not* change:** alerting, the acknowledge link (AckURL),
and on-call escalation all still fire exactly as they do for a normal
incident. Auto-resolve only affects the **stored state** —
`resolved` / `resolved_at` — nothing else. An auto-resolved webhook
incident is identical to a normal one in every other respect.

**Scope:** only the public webhook origin is affected. SNS/SQS-transported
incidents and incidents emitted by the AI SRE Agent are never
auto-resolved, and a payload that is already resolved is left untouched.

**Action — opt out** if you want webhook incidents to stay open and
escalate until acknowledged:

- **Admin UI** — turn off the **Incident intake** toggle at the top of
  **Settings → Alerting**.
- **API** — `PUT /api/admin/incidents/intake-settings` with body
  `{"auto_resolve_webhook": false}`. On the **OSS/community** binary this
  call is authenticated by the `X-Gateway-Secret` header, like the other
  admin settings endpoints. On the **Enterprise** binary the gateway
  secret is retired — sign in as an **admin or owner** instead, and the
  request is authorized by your admin session plus the `runtime:manage`
  permission (the change is written to the audit log), with no
  `X-Gateway-Secret` involved.

See [Incident intake: webhook auto-resolve](../webhook/getting-started.md#incident-intake-webhook-auto-resolve)
for the full behavior table.

### 2. Scheduled daily incident report + timezone (new, opt-in)

The [Incidents Report](../agent/incident-report.md) can now be delivered
automatically on a daily schedule, and the report is timezone-aware.
Both additions are **opt-in and default to off / UTC**, so an existing
deployment renders and behaves exactly as before until you enable them.

New report settings:

| Setting | Default | What it does |
|---|---|---|
| `schedule_enabled` | `false` | Sends the report automatically once a day. |
| `send_time` | `"09:00"` | The 24-hour `"HH:MM"` wall-clock send time. |
| `timezone` | `"UTC"` | An `"UTC"` or IANA name (e.g. `"Asia/Ho_Chi_Minh"`) that sets both the send time and the report's printed timestamps. |

When the schedule is on, the report is sent every day at `send_time` in
`timezone`, reusing the existing default window and default channel. The
schedule only runs while the report itself is enabled. In a
multi-instance deployment, exactly one replica sends the daily digest.

Leaving `timezone` at `UTC` keeps the rendered report byte-for-byte
identical to earlier releases. See
[Scheduled daily delivery](../agent/incident-report.md#scheduled-daily-delivery).

## Upgrade

```bash
# Docker
docker pull ghcr.io/versuscontrol/versus-incident

# Helm
helm repo update
helm upgrade versus-incident \
  oci://ghcr.io/versuscontrol/charts/versus-incident
```

No schema migration is required. If you rely on webhook incidents
staying open, apply the opt-out above before or immediately after the
upgrade.

For any issues with the migration, please
[open an issue](https://github.com/VersusControl/versus-incident/issues).
