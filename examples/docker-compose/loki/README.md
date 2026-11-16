# Loki-source example

You need Git and Docker with Docker Compose v2; the traffic generator below
also needs Python 3. Clone the repository and go into it:

```bash
git clone https://github.com/VersusControl/versus-incident.git
cd versus-incident
```

Every command in this README runs from that `versus-incident` folder, the
repository root.

Versus + Redis + Grafana Loki + Grafana. The agent ingests log
entries via Loki's `query_range` API; Grafana is provisioned with
Loki as the default datasource so you can browse the same logs.

## Services

| Service | Port |
|---|---|
| versus-incident | `3000` |
| grafana | `3001` (anonymous admin) |
| loki | `3100` |
| redis | (internal) |

## Run

```bash
docker compose -f examples/docker-compose/loki/docker-compose.yml up -d
```

Wait for Loki to become healthy (~10s). All settings have safe
defaults; export the variables in your shell to override — see
[../README.md](../README.md).

## Generate test traffic

Push synthetic logs into the local Loki using the bundled generator:

```bash
# Training traffic — 20 lines every 5s, stamped with the current time.
# Leave it running while the agent learns its baseline; Ctrl+C to stop:
scripts/run_noisy_logs.sh --target loki

# Heavier training traffic — 500 lines every 5s, Ctrl+C to stop:
scripts/run_noisy_logs.sh --target loki --batch 500

# Spike (test the spike detector) — one burst, then exits:
scripts/run_noisy_logs.sh --target loki --spike db-conn-refused --spike-burst 80

# Curated incident cluster (test detect mode) — one 60-line burst, then exits:
scripts/run_noisy_logs.sh --target loki --scenario db-outage
```

Or push a single line by hand (uses Python for a portable nanosecond
timestamp — `date +%s%N` doesn't work on macOS):

```bash
TS=$(python3 -c 'import time; print(time.time_ns())')
curl -X POST http://localhost:3100/loki/api/v1/push \
  -H 'Content-Type: application/json' \
  -d "{
    \"streams\":[{
      \"stream\":{\"app\":\"demo\",\"level\":\"error\"},
      \"values\":[[\"${TS}\",\"db connection refused\"]]
    }]
  }"
```

## Verify

```bash
SECRET=${GATEWAY_SECRET:-change-me}
curl -H "X-Gateway-Secret: $SECRET" http://localhost:3000/api/agent/patterns | jq
```

Browse the same logs in Grafana → *Explore* with query
`{app=~".+"}` at <http://localhost:3001>.

## Layout

```
loki/
├── docker-compose.yml
├── config/
│   ├── config.yaml
│   └── agent_sources.yaml
└── grafana/
    └── provisioning/
        └── datasources/loki.yaml
```

## Cleanup

```bash
docker compose -f examples/docker-compose/loki/docker-compose.yml down -v
```

## Reference

[Loki source docs](https://docs.versusincident.com/#/agent/data-sources/loki)
