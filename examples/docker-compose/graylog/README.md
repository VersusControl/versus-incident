# Graylog-source example

You need Git and Docker with Docker Compose v2; the traffic generator below
also needs Python 3. Clone the repository and go into it:

```bash
git clone https://github.com/VersusControl/versus-incident.git
cd versus-incident
```

Every command in this README runs from that `versus-incident` folder, the
repository root.

Versus + Redis + Graylog 5 + MongoDB + OpenSearch. The agent ingests
through Graylog's `search/universal/absolute` REST endpoint.

> Heads-up: Graylog + OpenSearch need ~2 GiB of heap combined. Make
> sure Docker has at least 6 GiB of RAM allocated.

## Services

| Service | Port |
|---|---|
| versus-incident | `3000` |
| graylog (web + REST) | `9000` |
| graylog GELF UDP input (created by `graylog-init`) | `12201/udp` |
| mongodb / opensearch | (internal) |
| redis | (internal) |

## Run

```bash
docker compose -f examples/docker-compose/graylog/docker-compose.yml up -d
```

Wait ~60s for Graylog to finish bootstrap, then sign in at
<http://localhost:9000> with `admin` / `admin`.

Graylog's login comes from `GRAYLOG_ROOT_PASSWORD_SHA2` in
`docker-compose.yml`, which ships as the SHA-256 of `admin`.
`GRAYLOG_PASSWORD` only tells Versus and `graylog-init` which password
to use. Setting it alone does not change the login, and both then get
401 from Graylog. To use another password, change both together:

```bash
export GRAYLOG_PASSWORD=my-real-password
# Put this hash in GRAYLOG_ROOT_PASSWORD_SHA2 in
# examples/docker-compose/graylog/docker-compose.yml:
printf '%s' "$GRAYLOG_PASSWORD" | shasum -a 256 | cut -d' ' -f1
docker compose -f examples/docker-compose/graylog/docker-compose.yml \
  up -d --force-recreate graylog graylog-init versus
```

## Generate test traffic

Graylog ships without any inputs enabled. The one-shot `graylog-init`
service creates a global *GELF UDP* input on port `12201` once Graylog
is healthy (and skips it if one already exists). Check it ran:

```bash
docker compose -f examples/docker-compose/graylog/docker-compose.yml logs graylog-init
```

Then push synthetic logs from your host using the bundled generator:

```bash
# Training traffic — 20 lines every 5s, stamped with the current time.
# Leave it running while the agent learns its baseline; Ctrl+C to stop:
scripts/run_noisy_logs.sh --target graylog

# Spike (test the spike detector) — one burst, then exits:
scripts/run_noisy_logs.sh --target graylog --spike db-conn-refused --spike-burst 80

# Curated incident cluster (test detect mode) — one 60-line burst, then exits:
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
docker compose -f examples/docker-compose/graylog/docker-compose.yml down -v
```

## Reference

[Graylog source docs](https://docs.versusincident.com/#/agent/data-sources/graylog)
