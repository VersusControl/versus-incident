# SRE agent quickstart

You need Git and Docker with Docker Compose v2. Clone the repository and go into
this example's folder:

```bash
git clone https://github.com/VersusControl/versus-incident.git
cd versus-incident/examples/quickstart/sre-agent
```

Every command in this README runs from that folder,
`versus-incident/examples/quickstart/sre-agent`. If you open a new terminal, `cd`
into it first.

Versus reads application logs from Loki, learns what normal looks like, then
raises an incident when an error it has never seen shows up. This stack runs
everything on your machine: Versus (OSS image), Redis, Loki and Grafana, plus
an optional demo log stream.

## Services

| Service | URL | Notes |
|---|---|---|
| versus | <http://localhost:3000> | UI and API. Sign in with the gateway secret, `change-me` by default. |
| grafana | <http://localhost:3001> | Anonymous admin, Loki is the default datasource. |
| loki | <http://localhost:3100> | Log store the agent reads from. |
| redis | internal | TLS with a self-signed cert made on first start. |
| log-generator | none | Profile `fake-logs`: 100 mixed log lines into Loki every 5 seconds. |
| error-burst | none | Profile `error-burst`: one-shot, a burst of up to 60 lines of an error the demo stream never produces. |

All ports bind to `127.0.0.1`. The log containers run
[`scripts/generate_noisy_logs.py`](../../../scripts/generate_noisy_logs.py) in a
Python image, so you do not need Python installed.

## 1. Start the stack

Start the stack:

```bash
docker compose up -d
```

The agent starts in **training** mode: it reads logs and learns patterns, and
never raises an incident.

## 2. Open the UI

Open <http://localhost:3000> and enter the gateway secret `change-me`.

## 3. Start the demo logs (optional)

Skip this if you push your own logs into the local Loki.

```bash
docker compose --profile fake-logs up -d
docker compose logs -f log-generator     # Ctrl+C to stop following
```

## 4. Watch training learn patterns

Open **Agent Learning → Logs** in the UI, or ask the API:

```bash
curl -s -H "X-Gateway-Secret: change-me" http://localhost:3000/api/agent/patterns
curl -s -H "X-Gateway-Secret: change-me" http://localhost:3000/api/agent/status
```

New patterns appear within a minute. The agent learns only lines that match
`agent.regex.default_pattern` (`(?i).*error.*` here), so the demo stream of
mixed info, warning and error lines settles at about 20 patterns. A pattern seen 5 times
(`agent.catalog.auto_promote_after` in `config/config.yaml`) counts as
**known**. Give it about 5 minutes so the rare lines in the demo stream are
known too; otherwise they raise incidents of their own once you switch to
detect.

## 5. Switch to detect

The mode comes from `AGENT_MODE`. Recreate the Versus container with the new
value; the learned catalog is kept in the `versus-data` volume.

Slack and the AI key are read when the container is created, so export them
first, in the same terminal. The burst in step 6 raises one incident and the
agent does not send it again while it is open, so variables added after the
burst do not change that message.

Slack (optional). Invite the bot to the channel first (`/invite @your-bot`):

```bash
export SLACK_ENABLE=true
export SLACK_TOKEN=xoxb-...
export SLACK_CHANNEL_ID=C0123456789
```

AI key (optional). Detect raises incidents without a key; with one, the AI SRE
writes the summary, category and suggested next steps:

```bash
export AGENT_AI_ENABLE=true
export AGENT_AI_API_KEY=sk-...
```

That uses OpenAI with `gpt-4o-mini` from `config/config.yaml`. For
`AGENT_AI_PROVIDER` `deepseek`, `qwen`, `claude` or `gemini`, also set
`AGENT_AI_MODEL` to a model of that provider; otherwise `gpt-4o-mini` is sent.

Then switch:

```bash
AGENT_MODE=detect docker compose up -d versus
```

Check it:

```bash
curl -s -H "X-Gateway-Secret: change-me" http://localhost:3000/api/admin/config/agent
```

The response has `"mode":"detect"`.

## 6. Inject a new error burst

Run it once:

```bash
docker compose run --rm error-burst
```

This pushes `checkout error: payment processor acme-pay did not answer within
30s, charge aborted` lines for the `checkout-api` service, back to back. The
demo stream never produces that line, so the agent has never seen it.

## 7. See the incident

Within one or two poll ticks (15 seconds each), the agent raises an incident
titled `Anomaly: checkout-api — service = checkout-api message = " checkout
error: payment processor acme-pay …`:

- UI: **Incidents** lists it; **AI → Decisions** shows the detect decision.
- API:

  ```bash
  curl -s -H "X-Gateway-Secret: change-me" http://localhost:3000/api/admin/incidents
  curl -s -H "X-Gateway-Secret: change-me" http://localhost:3000/api/agent/detect
  ```

- Slack, if you exported the Slack variables in step 5.

Without an AI key the incident is a templated alert: title, severity and
sample lines, with the note
`Heuristic detection — AI enrichment not configured or unavailable.` The
frequency in the summary is how many of the burst's lines the agent counted.

## Settings

| Variable | Default | What it does |
|---|---|---|
| `VERSUS_IMAGE` | `ghcr.io/versuscontrol/versus-incident:latest` | Versus image |
| `GATEWAY_SECRET` | `change-me` | Secret for the UI and admin API. Set your own outside a laptop demo. |
| `AGENT_MODE` | `training` | `training`, `shadow` or `detect` |
| `REDIS_PASSWORD` | `versus` | Redis password |
| `SLACK_ENABLE` / `SLACK_TOKEN` / `SLACK_CHANNEL_ID` | off | Slack delivery |
| `AGENT_AI_ENABLE` / `AGENT_AI_API_KEY` / `AGENT_AI_MODEL` / `AGENT_AI_PROVIDER` | off | AI enrichment |

Variables are read when a container is created, so export them before
`docker compose up -d`.

## Clean up

```bash
docker compose --profile fake-logs --profile error-burst down -v
```

`-v` removes the volumes, including the learned catalog.

## Layout

```
sre-agent/
├── docker-compose.yml
├── config/
│   ├── config.yaml          # agent settings, training by default
│   └── agent_sources.yaml   # the local Loki source
└── grafana/provisioning/datasources/loki.yaml
```
