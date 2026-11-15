# Elasticsearch

## Concept

The Elasticsearch source reads log documents from Elasticsearch, OpenSearch or
Elastic Cloud. You give it an index pattern, the time field and an optional
query; Versus reads matching documents in time order on every poll and takes
the log text from one field.

## Minimal config

```yaml
# config/agent_sources.yaml
sources:
  - name: prod-app
    type: elasticsearch
    enable: true
    elasticsearch:
      addresses:
        - https://es.prod.example:9200
      api_key: ${ES_API_KEY}
      index: "logs-app-*"
      query: 'log.level:(error OR warn)'
```

| Key | Default | What it does |
|---|---|---|
| `addresses` | required | One or more cluster URLs. |
| `index` | required | Index name or pattern, for example `logs-*`. |
| `time_field` | `@timestamp` | The document time. Used to read in order. |
| `query` | match all | Lucene query string, for example `level:error`. |
| `message_field` | `message` | Field that holds the log text. |
| `severity_field` | none | Field that holds the level, for example `log.level`. |
| `extra_fields` | none | Fields kept with each line, for example `service.name`. |
| `api_key` | none | API key auth. Used instead of `username`/`password` when both are set. |
| `username` / `password` | none | Basic auth. |
| `allow_loopback` | `false` | Must be `true` to reach `localhost` or `127.0.0.1`. |
| `insecure_skip_verify` | `false` | Skip TLS verification. Only without credentials, for local testing. |
| `page_size` | `500` | Documents per request, up to `1000`. |
| `reorder_window` | `1m` | How far back each poll looks again, to catch documents that arrive late. |
| `tie_breaker_field` | none | A unique keyword field, for busy indices where many documents share a timestamp. |

Credentials need **HTTPS with a valid certificate**. Versus refuses a username,
password or API key over plain HTTP or with `insecure_skip_verify`, and refuses
credentials written inside an address (`https://user:pass@host`).

> **Fluent Bit and container logs.** Fluent Bit often stores the log line in
> `log`, not `message`. If `message_field` points at an empty field, Versus
> connects without error and learns nothing. Set `message_field: log` (and use
> `log:` in `query`).

## What you see in the UI

Patterns learned from the index appear in **Agent Learning → Logs**, grouped by
service. Incidents show `via elasticsearch:<source name>`.

![Agent Learning → Logs for the Elasticsearch example: 18 templates learned from the logs-* indices, grouped by service, each with its count and all still learning](../../docs/images/ds-elasticsearch-ui-dark.png)

## Permissions

The user or API key needs `read` and `view_index_metadata` on the index pattern:

```json
{
  "indices": [
    { "names": ["logs-app-*"], "privileges": ["read", "view_index_metadata"] }
  ]
}
```

On Elastic Cloud, create an API key under **Stack Management → Security → API
keys** and pass it as `api_key`.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `401` | Wrong credentials, or the API key has expired. |
| `403` | The role is missing `read` on the index. |
| Connects, no error, nothing learned | The log text is in a different field from `message_field` (often `log`). |
| Only old data, then nothing | `time_field` is not the field your documents use. |
| `No mapping found for [<field>] in order to sort on` | `tie_breaker_field` is not mapped in every matching index. Remove it, or pick a mapped keyword field. |
| Refused at startup | Credentials over HTTP, credentials in the URL, or a loopback address without `allow_loopback: true`. |

## Try it with Docker Compose

You need Git, Docker with Docker Compose v2, and Python 3 for the demo
traffic script. Clone the repository and go into it:

```bash
git clone https://github.com/VersusControl/versus-incident.git
cd versus-incident
```

Every command below runs from this `versus-incident` folder, the repository
root.

[`examples/docker-compose/elasticsearch/`](https://github.com/VersusControl/versus-incident/tree/main/examples/docker-compose/elasticsearch)
runs Versus (OSS image), Redis, Elasticsearch 8 and Kibana. Give Docker at least
4 GB of memory.

```yaml
# examples/docker-compose/elasticsearch/config/agent_sources.yaml
sources:
  - name: demo-es
    type: elasticsearch
    enable: true
    elasticsearch:
      addresses:
        - http://elasticsearch:9200
      index: "logs-*"
      time_field: "@timestamp"
      query: '*'
      message_field: message
      severity_field: level
      extra_fields:
        - service
      page_size: 500
```

Start it and wait about 30 seconds for Elasticsearch:

```bash
docker compose -f examples/docker-compose/elasticsearch/docker-compose.yml up -d
curl http://localhost:9200/_cluster/health
```

Start the training traffic: 20 lines every 5 seconds, stamped with the
current time, with one line of each ordinary error in every batch. The script
needs Python 3 on your machine. Leave it running while the agent learns; press
`Ctrl+C` to stop it.

```bash
scripts/run_noisy_logs.sh --target elasticsearch
```

The lines go to the `logs-noisy` index, which matches `logs-*`.

Open `http://localhost:3000`, enter the gateway secret `change-me`, and go to
**Agent Learning → Logs**. Browse the same documents in Kibana at
`http://localhost:5601` (**Discover**, index pattern `logs-*`).

Within about a minute all 6 patterns show **Known**.

To see an incident, keep the training traffic running for 3 minutes. Then open
a second terminal in the `versus-incident` folder, switch the agent to
detect, and send one incident burst:

```bash
AGENT_MODE=detect docker compose -f examples/docker-compose/elasticsearch/docker-compose.yml \
  up -d --force-recreate versus
scripts/run_noisy_logs.sh --target elasticsearch --scenario db-outage
```

The burst sends 60 lines and exits. Within about 30 seconds of the burst, often sooner, two
`db-pool` incidents appear on the **Incidents** page, and the normal traffic
raises none.

Clean up:

```bash
docker compose -f examples/docker-compose/elasticsearch/docker-compose.yml down -v
```
