# Loki-source example

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
docker compose up -d
```

Wait for Loki to become healthy (~10s). All settings have safe
defaults; export the variables in your shell to override — see
[../README.md](../README.md).

## Generate test traffic

Push synthetic logs into the local Loki using the bundled generator
(run from the repo root):

```bash
# 500 mixed lines once:
python3 scripts/generate_noisy_logs.py --target loki --lines 500

# Continuous — 20 lines every 5s, Ctrl+C to stop:
scripts/run_noisy_logs.sh --target loki

# Continuous — 500 lines every 5s, Ctrl+C to stop:
scripts/run_noisy_logs.sh --target loki --batch 500

# Spike (test the spike detector):
scripts/run_noisy_logs.sh --target loki --spike db-conn-refused --spike-burst 80

# Curated incident cluster (test detect mode):
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
docker compose down -v
```

## Reference

[Loki source docs](https://docs.versusincident.com/#/agent/data-sources/loki)
