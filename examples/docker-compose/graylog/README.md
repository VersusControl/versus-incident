# Graylog-source example

Versus + Redis + Graylog 5 + MongoDB + OpenSearch. The agent ingests
through Graylog's `search/universal/absolute` REST endpoint.

> Heads-up: Graylog + OpenSearch need ~2 GiB of heap combined. Make
> sure Docker has at least 6 GiB of RAM allocated.

## Services

| Service | Port |
|---|---|
| versus-incident | `3000` |
| graylog (web + REST) | `9000` |
| graylog GELF UDP input (after setup) | `12201/udp` |
| mongodb / opensearch | (internal) |
| redis | (internal) |

## Run

```bash
docker compose up -d
```

Wait ~60s for Graylog to finish bootstrap, then sign in at
<http://localhost:9000> with `admin` / `admin` (or whatever you set
in `GRAYLOG_PASSWORD`).

The default password ships with this example as the SHA-256 of
`admin`. Override both values before any non-throwaway use:

```bash
export GRAYLOG_PASSWORD=my-real-password
# regenerate the hash and edit docker-compose.yml:
echo -n my-real-password | shasum -a 256
docker compose up -d --force-recreate graylog versus
```

## Generate test traffic

Graylog ships without any inputs enabled. From the web UI:

1. *System* → *Inputs* → select *GELF UDP* → *Launch new input* →
   pick the `graylog` node → save with defaults (UDP port `12201`).

Then push synthetic logs from your host using the bundled generator
(run from the repo root):

```bash
# 500 mixed lines once into Graylog:
python3 scripts/generate_noisy_logs.py --target graylog --lines 500

# Continuous — 20 lines every 5s, Ctrl+C to stop:
scripts/run_noisy_logs.sh --target graylog

# Spike (test the spike detector):
scripts/run_noisy_logs.sh --target graylog --spike db-conn-refused --spike-burst 80

# Curated incident cluster (test detect mode):
scripts/run_noisy_logs.sh --target graylog --scenario db-outage
```

Override the endpoint with `GRAYLOG_HOST` / `GRAYLOG_PORT` if you've
mapped the GELF UDP input to a different port. The same template /
`--spike` / `--scenario` flags work across all targets; see
[scripts/README.md](../../../scripts/README.md) for the full reference.

Or push a single GELF message by hand:

```bash
echo '{"version":"1.1","host":"laptop","short_message":"db connection refused","level":3,"_service":"api"}' \
  | nc -u -w1 localhost 12201
```

Messages appear in *Search* immediately. The Versus agent picks them
up on the next poll tick (`agent.poll_interval`, default `15s`).

## Verify

```bash
SECRET=${GATEWAY_SECRET:-change-me}
curl -H "X-Gateway-Secret: $SECRET" http://localhost:3000/api/agent/patterns | jq
```

## Layout

```
graylog/
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

[Graylog source docs](https://versuscontrol.github.io/versus-incident/agent/data-sources/graylog.html)
