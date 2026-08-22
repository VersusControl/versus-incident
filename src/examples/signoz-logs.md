# Connect SigNoz to the Versus SRE Agent

*Point the agent at the logs already sitting in SigNoz and let it escalate on its own — no alert rules to write.*

![The source config file is loaded by the Versus agent at boot. Its address field points at your SigNoz, which the agent queries and reads log rows back from, raising an incident when a pattern spikes](../docs/images/signoz-flow-h.svg)

If you already run SigNoz, your logs are in there and searchable. What SigNoz won't do is watch them for you.

The usual fix is to write alert rules: pick a pattern you're worried about, guess a threshold, and maintain it forever. This guide takes the other route. Versus reads your SigNoz logs on a timer, learns what normal looks like for each service, and escalates on its own when something breaks that pattern. You don't write the rules — it works them out from your own logs, and you only hear about it when something is actually off.

## Prerequisites

- A running SigNoz, **v0.87.0 or newer**. That's the release with the v5 query API, which this source depends on. Self-hosted and Cloud both work — the endpoint and the auth header are the same.
- Logs actually flowing into it. If the Logs Explorer is empty, fix that first; Versus can only read what SigNoz has.
- A Slack workspace or a Telegram bot, if you want the alert to go somewhere. You can skip this and read incidents from the Versus API instead.
- Docker, for the quickest way to run Versus.

## Step 1: Mint a SigNoz API key

Versus authenticates with the `SIGNOZ-API-KEY` header. You create the key inside SigNoz, as a **service account** — not a personal login.

1. Open SigNoz and go to **Settings → Account Settings**.
2. Find **Service Accounts** and create one. Name it something you'll recognise later, like `versus-reader`.
3. **Grant it the viewer role.** In a stock install that role is named `signoz-viewer`.
4. With the role attached, create a key on that service account. SigNoz generates the value server-side and shows it once. Copy it now.

That third step is the one people skip, and it fails in a way that wastes an afternoon. **A key with no role still authenticates.** You get `200 OK` on every request and an empty result set — so the logs look missing rather than forbidden, and you go hunting through your filter expression, your time window, and your collector config before you think to check the role. If Versus connects but reads nothing, check the role first.

Versus only ever reads from SigNoz. Viewer is all it needs, so don't give the key more.

If you'd rather not click through the UI, the [runnable example](https://github.com/VersusControl/versus-incident/tree/main/examples/docker-compose/signoz) does all four steps against SigNoz's own API in a bootstrap container, and you can read that script to see exactly which calls are involved.

## Step 2: Configure the SigNoz data source

Create `config/agent_sources.yaml`:

```yaml
sources:
  - name: prod-logs
    type: signoz
    enable: true
    signoz:
      address: ${SIGNOZ_ADDRESS}
      api_key: ${SIGNOZ_API_KEY}

      # The same syntax you'd type into the Logs Explorer filter bar. Not
      # LogQL, not PromQL. Leave it empty to read every log in the window;
      # narrow it once you know the volume you're dealing with.
      query: "severity_text = 'ERROR'"

      message_field: body
      severity_field: severity_text

      # SigNoz stores OTLP attributes in nested maps whose KEYS carry the dots,
      # so `service.name` lives inside `resources_string`. Name it either way —
      # bare names are searched across the containers, resource attributes
      # first — and it lands in Signal.Fields under the bare name.
      extra_fields:
        - resources_string.service.name
        - resources_string.deployment.environment
        - attributes_string.http.status_code

      page_size: 500
```

One of those fields is worth a second look.

`query` takes the same expression you'd type into the Logs Explorer filter bar. If you write LogQL or PromQL here it won't error in an obvious way — it'll just match nothing. Build the filter in the SigNoz UI first, confirm it returns the rows you expect, then paste it.

## Step 3: Configure Versus and pick a channel

Create `config/config.yaml`:

```yaml
name: versus
host: 0.0.0.0
port: 3000

alert:
  slack:
    enable: true
    token: ${SLACK_TOKEN}
    channel_id: ${SLACK_CHANNEL_ID}

agent:
  enable: true
  mode: training
  poll_interval: 30s
  sources_file: /app/config/agent_sources.yaml
```

Note `mode: training`. Versus starts by learning your log patterns and their normal rates — it won't alert yet. That's deliberate. A detector with no baseline either pages on everything or on nothing, and neither teaches you anything.

Give it a few hours across a normal working day, including a quiet period and a busy one. Then switch to `mode: detect` and restart. From that point a pattern that spikes well above its learned rate raises an incident.

If you want to see what it *would* have alerted on before committing, `mode: shadow` runs the full detection path and writes findings to the agent's own log without sending anything.

## Step 4: Run it

```bash
docker run -d \
  -p 3000:3000 \
  -v $(pwd)/config:/app/config \
  -e SIGNOZ_ADDRESS=http://signoz:8080 \
  -e SIGNOZ_API_KEY=your-minted-key \
  -e SLACK_TOKEN=xoxb-your-token \
  -e SLACK_CHANNEL_ID=C01234567 \
  ghcr.io/versuscontrol/versus-incident
```

Point `SIGNOZ_ADDRESS` at the SigNoz **query service** — the same host and port that serves the UI, `:8080` on a stock self-hosted install. On Cloud it's your workspace URL.

## Step 5: Check it's actually reading

The boot log tells you whether the source was built at all:

```
agent: starting worker mode=training sources=1
```

`sources=0` means the source didn't build. Versus logs the reason on the line above — a bad address, a missing key, a typo in the type.

Then watch a tick:

```
agent: tick signoz:prod-logs signals=42 matched=42 patterns=7 cursor=2026-08-22T09:14:30Z
```

`signals=0` on every tick means the query is connecting but matching nothing. In rough order of likelihood: the API key has no role (Step 1), the filter expression is wrong, or the window genuinely has no logs. Check them in that order — the first costs nothing to rule out, and it's the one that looks least like the problem.

Or just look at it. Open <http://localhost:3000/> and go to the **Patterns** page — every template the agent has learned, most frequent first, with the service it was attributed to. If templates are showing up with sensible services attached, the source is working and you're only waiting on the baseline.

## When the field mapping looks wrong

The most common surprise is `service` coming through empty while the log body looks fine.

SigNoz nests OTLP attributes in per-type maps, and **the keys keep their dots**. So `service.name` isn't a top-level column — it's a key inside `resources_string`. If you'd assumed a flat schema and mapped `service_name`, you get nothing, silently.

Name it `resources_string.service.name` to be explicit, or just `service.name` and let Versus search the containers for it (resource attributes first). Either way it arrives in `Signal.Fields` under the bare name `service.name`, which is what the rest of the agent keys on.

## What you built

- A read-only SigNoz service account, scoped to viewer, feeding Versus.
- A log source reading your SigNoz logs on a timer, with no change to how those logs get there.
- An agent learning your normal log patterns, ready to switch from `training` to `detect`.
- An escalation path to Slack or Telegram, triggered by the baseline it learned rather than by a rule you wrote.

**Next:** the [SigNoz data source reference](/agent/data-sources/signoz) covers every option in full. If you also want SigNoz **metrics and traces** watched the same way, that's an Enterprise feature — see [SigNoz metrics](/enterprise/metrics/signoz).
