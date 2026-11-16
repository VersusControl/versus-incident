# Splunk-source example

You need Git and Docker with Docker Compose v2; the traffic generator below
also needs Python 3. Clone the repository and go into it:

```bash
git clone https://github.com/VersusControl/versus-incident.git
cd versus-incident
```

Every command in this README runs from that `versus-incident` folder, the
repository root.

Versus + Redis + Splunk Enterprise (single instance). The agent
ingests through the splunkd REST API
(`/services/search/v2/jobs/export`).

> Heads-up: the official `splunk/splunk` image is ~2 GiB and Splunk
> itself wants 1–2 GiB of RAM. First boot can take 60–90 s.
>
> Apple Silicon: Splunk publishes amd64 images only, so the service is
> pinned to `platform: linux/amd64` and runs under emulation. Expect a
> first boot of several minutes and slow indexing; HEC events can take
> a while to become searchable, so give the agent a few extra ticks
> before its first patterns appear.

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
docker compose -f examples/docker-compose/splunk/docker-compose.yml up -d
```

Wait ~90s for Splunk to finish first-time setup, then sign in at
<http://localhost:8000> with `admin` / `Changeme1!` (or whatever
you set in `SPLUNK_PASSWORD`).

> The password must satisfy Splunk's complexity rules
> (>= 8 chars + letter + digit + symbol).

```bash
export SPLUNK_PASSWORD='MyRealPassword!1'
docker compose -f examples/docker-compose/splunk/docker-compose.yml \
  up -d --force-recreate splunk versus
```

## Generate test traffic

Splunk's HTTP Event Collector (HEC) is the easiest way to push test
logs. The example pre-configures a token (set `SPLUNK_HEC_TOKEN` to
override).

Push synthetic logs from your host using the bundled generator:

```bash
export SPLUNK_HEC_TOKEN=${SPLUNK_HEC_TOKEN:-00000000-0000-0000-0000-000000000000}

# Training traffic — 20 lines every 5s, stamped with the current time.
# Leave it running while the agent learns its baseline; Ctrl+C to stop:
scripts/run_noisy_logs.sh --target splunk

# Spike (test the spike detector) — one burst, then exits:
scripts/run_noisy_logs.sh --target splunk --spike db-conn-refused --spike-burst 80

# Curated incident cluster (test detect mode) — one 60-line burst, then exits:
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
  -d '{"event":"db connection refused","sourcetype":"noisy","index":"main","fields":{"level":"error","service":"api"}}'
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
   field, then
   `docker compose -f examples/docker-compose/splunk/docker-compose.yml up -d --force-recreate versus`.

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
docker compose -f examples/docker-compose/splunk/docker-compose.yml down -v
```

## Reference

[Splunk source docs](https://docs.versusincident.com/#/agent/data-sources/splunk)
