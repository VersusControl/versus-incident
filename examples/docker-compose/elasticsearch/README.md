# Elasticsearch-source example

Versus + Redis + Elasticsearch 8 + Kibana. The agent ingests via the
ES `_search` API with `search_after` pagination; Kibana is provided
so you can browse the same indices.

> Heads-up: Elasticsearch needs ~512 MiB of heap. Make sure Docker
> has at least 4 GiB of RAM allocated.

## Services

| Service | Port |
|---|---|
| versus-incident | `3000` |
| elasticsearch | `9200` |
| kibana | `5601` |
| redis | (internal) |

## Run

```bash
docker compose up -d
```

Wait ~30s for Elasticsearch to become healthy, then verify:

```bash
curl http://localhost:9200/_cluster/health
```

All settings have safe defaults; export the variables in your
shell to override — see [../README.md](../README.md).

## Generate test traffic

Push synthetic logs into the local Elasticsearch using the bundled
generator (run from the repo root):

```bash
# 500 mixed lines once into the logs-noisy index:
python3 scripts/generate_noisy_logs.py --target elasticsearch --lines 500

# Continuous — 20 lines every 5s, Ctrl+C to stop:
scripts/run_noisy_logs.sh --target elasticsearch

# Spike (test the spike detector):
scripts/run_noisy_logs.sh --target elasticsearch --spike db-conn-refused --spike-burst 80

# Curated incident cluster (test detect mode):
scripts/run_noisy_logs.sh --target elasticsearch --scenario db-outage
```

The index `logs-noisy` matches the default `logs-*` pattern in
[config/agent_sources.yaml](config/agent_sources.yaml). Change with
`--es-index NAME` (or `ES_INDEX=NAME`) if you customize the source.

Or index a single doc by hand:

```bash
curl -X POST http://localhost:9200/logs-demo/_doc \
  -H 'Content-Type: application/json' \
  -d '{
    "@timestamp":"'"$(date -u +%FT%TZ)"'",
    "message":"db connection refused",
    "level":"error",
    "service":"api"
  }'
```

## Verify

```bash
SECRET=${GATEWAY_SECRET:-change-me}
curl -H "X-Gateway-Secret: $SECRET" http://localhost:3000/api/agent/patterns | jq
```

Browse the index in Kibana → *Discover* with index pattern `logs-*`
at <http://localhost:5601>.

## Layout

```
elasticsearch/
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

[Elasticsearch source docs](https://versuscontrol.github.io/versus-incident/agent/data-sources/elasticsearch.html)
