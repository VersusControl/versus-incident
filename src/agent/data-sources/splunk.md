# Splunk

## Concept

The Splunk source reads events from Splunk Enterprise or Splunk Cloud through
the Splunk REST API on the management port (usually `8089`). You give it an SPL
search; Versus runs it for the new time range on every poll and takes the log
text from `_raw` unless you name another field.

## Minimal config

```yaml
# config/agent_sources.yaml
sources:
  - name: prod-splunk
    type: splunk
    enable: true
    splunk:
      address: https://splunk:8089
      token: ${SPLUNK_TOKEN}
      search: 'index=main sourcetype=api level=error'
```

| Key | Default | What it does |
|---|---|---|
| `address` | required | Splunk REST URL on the management port, usually `8089`. |
| `search` | required | SPL search. `search` is added at the front when missing. |
| `token` | none | Splunk authentication token. Used instead of `username`/`password` when both are set. |
| `username` / `password` | none | Basic auth with a Splunk user. |
| `time_field` | `_time` | Field that holds the event time. |
| `message_field` | `_raw` | Field that holds the log text. |
| `severity_field` | none | Field that holds the level, for example `level`. |
| `extra_fields` | none | Fields kept with each line, for example `host`, `sourcetype`. |
| `owner` / `app` | none | Splunk namespace for AI Chat and Analyze searches. Set both or neither. |
| `page_size` | `500` | Events read per poll. |
| `insecure_skip_verify` | `false` | Skip TLS verification, for a self-signed `splunkd` certificate. |

Test the search in **Search & Reporting** first.

## What you see in the UI

Patterns learned from Splunk appear in **Agent Learning → Logs**, grouped by
service, the same view as for any log source (see [Training Mode](../training-mode.md)).
Incidents show `via splunk:<source name>`.

## Permissions

Use a Splunk user whose role can search the indexes in `search`, and create a
token for it under **Settings → Tokens**. Token authentication must be turned on
in Splunk. Versus only reads.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `401 Unauthorized` | Wrong token or password, or token authentication is off in Splunk. |
| `400 Bad Request` | Invalid SPL. Test it in **Search & Reporting**. |
| Connection refused | `address` uses the web port (`8000`) instead of the management port (`8089`). |
| No events, but Splunk has them | The search matches nothing, or the role cannot read that index. |
| Severity is always empty | `severity_field` names a field Splunk does not extract. |

## Try it with Docker Compose

You need Git, Docker with Docker Compose v2, and Python 3 for the demo
traffic script. Clone the repository and go into it:

```bash
git clone https://github.com/VersusControl/versus-incident.git
cd versus-incident
```

Every command below runs from this `versus-incident` folder, the repository
root.

[`examples/docker-compose/splunk/`](https://github.com/VersusControl/versus-incident/tree/main/examples/docker-compose/splunk)
runs Versus (OSS image), Redis and a single Splunk Enterprise instance. The
Splunk image is about 2 GB, and its first boot takes about 2 minutes.

```yaml
# examples/docker-compose/splunk/config/agent_sources.yaml
sources:
  - name: demo-splunk
    type: splunk
    enable: true
    splunk:
      address: https://splunk:8089
      username: admin
      password: ${SPLUNK_PASSWORD}        # Changeme1! unless you export another
      insecure_skip_verify: true   # self-signed splunkd cert
      search: 'search index=main'
      time_field: _time
      message_field: _raw
      extra_fields:
        - host
        - source
        - sourcetype
      page_size: 500
```

Start it and wait about 2 minutes:

```bash
docker compose -f examples/docker-compose/splunk/docker-compose.yml up -d
```

The demo traffic goes through Splunk's HTTP Event Collector. Export the token
the example presets, in each terminal you send traffic from:

```bash
export SPLUNK_HEC_TOKEN=00000000-0000-0000-0000-000000000000
```

Start the training traffic: 20 lines every 5 seconds, stamped with the
current time, with one line of each ordinary error in every batch. The script
needs Python 3 on your machine. Leave it running while the agent learns; press
`Ctrl+C` to stop it.

```bash
scripts/run_noisy_logs.sh --target splunk
```

Open `http://localhost:3000`, enter the gateway secret `change-me`, and go to
**Agent Learning → Logs**. Browse the same events in Splunk at
`http://localhost:8000` (`admin` / `Changeme1!`, search `index=main`).

Within a minute or two all 6 patterns show **Known**.

To see an incident, keep the training traffic running for 3 minutes. Then open
a second terminal in the `versus-incident` folder, export `SPLUNK_HEC_TOKEN`
there too, switch the agent to detect, and send one incident burst:

```bash
AGENT_MODE=detect docker compose -f examples/docker-compose/splunk/docker-compose.yml \
  up -d --force-recreate versus
scripts/run_noisy_logs.sh --target splunk --scenario db-outage
```

The burst sends 60 lines and exits. Within about 30 seconds of the burst, often sooner, two
`db-pool` incidents appear on the **Incidents** page, and the normal traffic
raises none.

Clean up:

```bash
docker compose -f examples/docker-compose/splunk/docker-compose.yml down -v
```
