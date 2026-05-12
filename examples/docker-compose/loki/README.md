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
cp .env.example .env
docker compose up -d
```

Wait for Loki to become healthy (~10s).

## Test

Push a log line into Loki:

```bash
curl -X POST http://localhost:3100/loki/api/v1/push \
  -H 'Content-Type: application/json' \
  -d '{
    "streams":[{
      "stream":{"app":"demo","level":"error"},
      "values":[["'"$(date +%s%N)"'","db connection refused"]]
    }]
  }'
```

Then verify the agent saw it:

```bash
SECRET=$(grep GATEWAY_SECRET .env | cut -d= -f2)
curl -H "X-Gateway-Secret: $SECRET" http://localhost:3000/api/agent/patterns | jq
```

Browse the same logs in Grafana → *Explore* with query `{app="demo"}`
at <http://localhost:3001>.

## Layout

```
loki/
├── docker-compose.yml
├── .env.example
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

[Loki source docs](https://versuscontrol.github.io/versus-incident/agent/data-sources/loki.html)
