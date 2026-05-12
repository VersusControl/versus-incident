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
cp .env.example .env
docker compose up -d
```

Wait ~30s for Elasticsearch to become healthy, then verify:

```bash
curl http://localhost:9200/_cluster/health
```

## Test

Index a doc:

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

Then check the agent picked it up:

```bash
SECRET=$(grep GATEWAY_SECRET .env | cut -d= -f2)
curl -H "X-Gateway-Secret: $SECRET" http://localhost:3000/api/agent/patterns | jq
```

Browse the index in Kibana → *Discover* with index pattern `logs-*`
at <http://localhost:5601>.

## Layout

```
elasticsearch/
├── docker-compose.yml
├── .env.example
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
