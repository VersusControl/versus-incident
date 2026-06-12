# Splunk-source example

Versus + Redis + Splunk Enterprise (single instance). The agent
ingests through the splunkd REST API
(`/services/search/v2/jobs/export`).

> Heads-up: the official `splunk/splunk` image is ~2 GiB and Splunk
> itself wants 1–2 GiB of RAM. First boot can take 60–90 s.

## Services

| Service | Port |
|---|---|
| versus-incident | `3000` |
| splunk Web UI | `8000` |
| splunk HEC (log ingest) | `8088` |
| splunkd REST | `8089` |
| redis | (internal) |

## Run

```bash
docker compose up -d
```

Wait ~90s for Splunk to finish first-time setup, then sign in at
<http://localhost:8000> with `admin` / `Changeme1!` (or whatever
you set in `SPLUNK_PASSWORD`).

> The password must satisfy Splunk's complexity rules
> (>= 8 chars + letter + digit + symbol).

```bash
export SPLUNK_PASSWORD='MyRealPassword!1'
docker compose up -d --force-recreate splunk versus
```

## Generate test traffic

Splunk's HTTP Event Collector (HEC) is the easiest way to push test
logs. The example pre-configures a token (set `SPLUNK_HEC_TOKEN` to
override).

Push synthetic logs from your host using the bundled generator (run
from the repo root):

```bash
export SPLUNK_HEC_TOKEN=${SPLUNK_HEC_TOKEN:-00000000-0000-0000-0000-000000000000}

# 500 mixed lines once into Splunk:
python3 scripts/generate_noisy_logs.py --target splunk \
  --splunk-token "$SPLUNK_HEC_TOKEN" --lines 500

# Continuous — 20 lines every 5s, Ctrl+C to stop:
scripts/run_noisy_logs.sh --target splunk

# Spike (test the spike detector):
scripts/run_noisy_logs.sh --target splunk --spike db-conn-refused --spike-burst 80

# Curated incident cluster (test detect mode):
scripts/run_noisy_logs.sh --target splunk --scenario db-outage
```

Override `SPLUNK_URL` / `SPLUNK_INDEX` to target a different endpoint
or index. The same template / `--spike` / `--scenario` flags work
across all targets; see [scripts/README.md](../../../scripts/README.md)
for the full reference.

Or push a single event by hand:

```bash
TOKEN=$SPLUNK_HEC_TOKEN
curl -k -H "Authorization: Splunk $TOKEN" \
  https://localhost:8088/services/collector/event \
  -d '{"event":"db connection refused","sourcetype":"_json","index":"main","fields":{"level":"error","service":"api"}}'
```

The Versus agent picks them up on the next poll tick
(`agent.poll_interval`, default `15s`).

## Verify

```bash
SECRET=${GATEWAY_SECRET:-change-me}
curl -H "X-Gateway-Secret: $SECRET" http://localhost:3000/api/agent/patterns | jq
```

Browse the same events in Splunk → *Search & Reporting*:
`index=main`.

## Switching to bearer-token auth

The example uses HTTP Basic for simplicity. To use an auth token
instead (recommended for production):

1. In Splunk Web → *Settings* → *Tokens* → *New Token* → generate
   one for `admin` with no expiry.
2. Edit [config/agent_sources.yaml](config/agent_sources.yaml):
   replace the `username` / `password` keys with a single `token:`
   field, then `docker compose up -d --force-recreate versus`.

## Layout

```
splunk/
├── docker-compose.yml
└── config/
    ├── config.yaml
    └── agent_sources.yaml
```

## Cleanup

```bash
docker compose down -v
```

## Reference

[Splunk source docs](https://docs.versusincident.com/#/agent/data-sources/splunk)
