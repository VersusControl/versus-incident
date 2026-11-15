# Webhook quickstart

You need Git and Docker with Docker Compose v2, and `curl`. Clone the repository and go into
this example's folder:

```bash
git clone https://github.com/VersusControl/versus-incident.git
cd versus-incident/examples/quickstart/webhook
```

Every command in this README runs from that folder,
`versus-incident/examples/quickstart/webhook`. If you open a new terminal, `cd`
into it first.

Send an alert to Versus over HTTP and get it in Slack, formatted by a template
you control. This stack runs Versus (OSS image) and Redis. An optional profile
adds Prometheus and Alertmanager with a demo rule that fires on its own.

## Services

| Service | URL | Notes |
|---|---|---|
| versus | <http://localhost:3000> | UI and API. Sign in with the gateway secret, `change-me` by default. |
| redis | internal | TLS with a self-signed cert made on first start. |
| prometheus | <http://localhost:9090> | Profile `alertmanager`. Rule `QuickstartDemoAlert` always fires. |
| alertmanager | <http://localhost:9093> | Profile `alertmanager`. Posts to `http://versus:3000/api/incidents`. |

All ports bind to `127.0.0.1`. Open the UI at <http://localhost:3000>, not
`127.0.0.1`: admin actions such as turning on Alert Fatigue fail there with
`CSRF validation failed`.

## 1. Start the stack

Start the stack:

```bash
docker compose up -d
```

## 2. Send a sample Alertmanager alert

```bash
curl -X POST http://localhost:3000/api/incidents \
  -H 'Content-Type: application/json' \
  -d @payloads/alertmanager.json
```

Response: `{"status":"Incident created"}` with HTTP 201.

## 3. See it in the UI and in Slack

- UI: open <http://localhost:3000>, enter `change-me`, then **Incidents**.
  The page opens on the **AI Detected** tab; pick **Webhook / Alerts**.
  Webhook alerts are notifications, so Versus files them as resolved right
  away; pick the **All** or **Resolved** filter to see `HighErrorRate`. The
  **Auto-resolve** checkbox on the Incidents page turns this off.
- API:

  ```bash
  curl -s -H "X-Gateway-Secret: change-me" http://localhost:3000/api/admin/incidents
  ```

- Versus log: `debug_body` is on, so the raw alert is printed.

  ```bash
  docker compose logs versus
  ```

- Slack: export the three variables and recreate Versus, then send the alert
  again. Invite the bot to the channel first (`/invite @your-bot`).

  ```bash
  export SLACK_ENABLE=true
  export SLACK_TOKEN=xoxb-...
  export SLACK_CHANNEL_ID=C0123456789
  docker compose up -d versus
  ```

  With the template in `templates/slack_message.tmpl` the message reads:

  ```
  🔥 *FIRING* · HighErrorRate (1 alert)
  • *CRITICAL* on `checkout-api:8080`: 5xx error rate above 5% for 5 minutes
    _checkout-api is returning 503 on POST /api/orders._
  ```

  With a bad token or channel the curl gets HTTP 500 and the error, for
  example `slack: failed to post standard message: invalid_auth`.

## 4. Edit the template and resend

`templates/` is mounted into the container and the template is read for every
alert, so there is no restart. Change a line in
`templates/slack_message.tmpl`, for example the first line:

```
{{ $icon }} *{{ upper .status }}* · [checkout] {{ .commonLabels.alertname }} ({{ len .alerts }} alert{{ if gt (len .alerts) 1 }}s{{ end }})
```

Send the same curl again and the new line shows up in Slack. The JSON body of
the alert is the template data (`.status`, `.alerts`, `.commonLabels`, ...).

## 5. Let Alertmanager send it (optional)

```bash
docker compose --profile alertmanager up -d
```

Prometheus evaluates `QuickstartDemoAlert` (`vector(1) > 0`) and Alertmanager
posts it to Versus within about 1–2 minutes, then again every 5 minutes while
it fires. Check <http://localhost:9093> or the incident list for
`QuickstartDemoAlert`. Stop it with:

```bash
docker compose --profile alertmanager stop prometheus alertmanager
```

## 6. On-call acknowledgment (optional)

On-call adds an **Acknowledge Alert** button to the Slack message and pages
PagerDuty if nobody presses it within 3 minutes (`oncall.wait_minutes`).

```bash
export ONCALL_ENABLE=true
export PAGERDUTY_ROUTING_KEY=<your Events API v2 routing key>
docker compose up -d versus
```

## 7. Alert Fatigue (Enterprise, optional)

Alert Fatigue quiets an alert that keeps repeating. It needs the Enterprise
image, a license key and Postgres. The overlay file adds all three:

```bash
export LICENSE_KEY=<your license key>      # never commit it
docker compose -f docker-compose.yml -f docker-compose.enterprise.yml up -d
docker compose -f docker-compose.yml -f docker-compose.enterprise.yml logs versus | grep -i password
```

Sign in at <http://localhost:3000> as `admin` with the password from the log,
open **AI → Alert fatigue** and turn it on. Then send the warning alert a few
times:

```bash
for i in 1 2 3 4 5; do
  curl -s -X POST http://localhost:3000/api/incidents \
    -H 'Content-Type: application/json' \
    -d @payloads/alertmanager-warning.json
done
```

The **Fingerprints** table on the Alert fatigue screen shows one row: severity
**WARN**, **Repeats** 5, status **Fatigued**, routed channel **suppressed**.
**Service** and **Source** show `—` and the table does not show the alert
name; open the fingerprint to see the alert content. Critical and high alerts always page and are never
held back. Without `LICENSE_KEY` the Enterprise image runs in community mode
and the screen stays locked.

## Settings

| Variable | Default | What it does |
|---|---|---|
| `VERSUS_IMAGE` | `ghcr.io/versuscontrol/versus-incident:latest` | Versus image |
| `GATEWAY_SECRET` | `change-me` | Secret for the UI and admin API. Set your own outside a laptop demo. |
| `PUBLIC_HOST` | `http://localhost:3000` | Address used by the Acknowledge Alert button |
| `REDIS_PASSWORD` | `versus` | Redis password |
| `SLACK_ENABLE` / `SLACK_TOKEN` / `SLACK_CHANNEL_ID` | off | Slack delivery |
| `ONCALL_ENABLE` / `PAGERDUTY_ROUTING_KEY` | off | On-call through PagerDuty |
| `LICENSE_KEY` | empty | Enterprise overlay only |
| `VERSUS_ENTERPRISE_IMAGE` | `ghcr.io/versuscontrol/versus-enterprise:latest` | Enterprise overlay only |
| `POSTGRES_PASSWORD` | `versus-dev` | Enterprise overlay only; Postgres is not published to the host |

## Clean up

```bash
docker compose --profile alertmanager down -v
# or, after the Enterprise step:
docker compose -f docker-compose.yml -f docker-compose.enterprise.yml --profile alertmanager down -v
```

## Layout

```
webhook/
├── docker-compose.yml
├── docker-compose.enterprise.yml   # Enterprise image + Postgres
├── config/config.yaml
├── templates/slack_message.tmpl    # the custom Slack template
├── payloads/
│   ├── alertmanager.json           # critical HighErrorRate alert
│   └── alertmanager-warning.json   # warning DiskSpaceLow alert, for Alert Fatigue
├── prometheus/{prometheus.yml,rules.yml}
└── alertmanager/alertmanager.yml
```
