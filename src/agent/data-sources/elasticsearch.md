# Elasticsearch source

Pulls log documents from Elasticsearch / OpenSearch / Elastic Cloud
using the `_search` API with `search_after` pagination.

## Minimal config

```yaml
sources:
  - name: prod-app
    type: elasticsearch
    enable: true
    elasticsearch:
      addresses:
        - http://elasticsearch:9200
      index: "logs-app-*"
      time_field: "@timestamp"
      query: '*'
      message_field: message
      page_size: 500
```

## Full reference

```yaml
elasticsearch:
  addresses:                       # REQUIRED. List of cluster nodes.
    - https://es.prod.example:9200
  username: ${ES_USERNAME}         # HTTP Basic auth
  password: ${ES_PASSWORD}
  api_key: ""                      # alternative to user/pass
  insecure_skip_verify: false      # set true only for dev / self-signed certs

  index: "logs-app-*"              # REQUIRED. Wildcards supported.
  time_field: "@timestamp"         # REQUIRED. Used for sort + range filter.
  query: 'log.level:(error OR warn)'  # Lucene-style; "*" = match all.

  message_field: message           # field copied to Signal.Message
  severity_field: log.level        # optional; copied to Signal.Severity
  extra_fields:                    # extra fields copied to Signal.Fields
    - service.name
    - host.name
    - error.stack_trace

  page_size: 500                   # _search size; capped at 10000
```

> **Fluent Bit users — check which field holds your log body.** When you ship
> container logs to Elasticsearch with Fluent Bit, the real log line is usually
> stored in the **`log`** field (the CRI/Docker parsers also add `stream`,
> `logtag`, `time`), and `message` is often empty or absent. If `query` and
> `message_field` point at `message` but your text is in `log`, the source
> connects fine and matches **zero** documents — no error, no alert, nothing
> on the dashboard. Point both at the field that actually carries the text:
>
> ```yaml
>       time_field: "@timestamp"
>       query: 'log:error'      # was message:error
>       message_field: log      # was message
>       page_size: 500
> ```
>
> Not sure which field it is? Query the index and inspect one document:
> `GET your-index-*/_search { "size": 1, "_source": ["@timestamp","message","log"] }`.

## Behavior

- **Cursor** — Stored as the maximum `time_field` timestamp returned
  on the previous tick. Documents with `_source[time_field] <= since`
  are filtered out (`range: gt`).
- **Pagination** — Uses `sort: [{time_field: asc}, {_id: asc}]` plus
  `search_after`, so the cursor is stable even when many documents
  share the same timestamp.
- **Auth precedence** — `api_key` wins over `username`/`password`
  when both are set.

## IAM / role requirements

The user / API key needs `read` on the configured `index` pattern.
Minimal Elasticsearch role:

```json
{
  "indices": [
    { "names": ["logs-app-*"], "privileges": ["read", "view_index_metadata"] }
  ]
}
```

For **Elastic Cloud**, create an API key under
*Stack Management → Security → API keys* and pass it via `api_key`.

## Tips

- For very busy indices, **always** set a `query` filter (e.g.
  `log.level:(error OR warn)`). The miner is fast but ingesting every
  INFO line of every service is rarely useful.
- Pair the agent's `agent.lookback` (default `5m`) with `page_size`
  so the first poll completes in one round-trip when possible.
- If your time field is in epoch milliseconds, the source converts
  numeric values to `time.Unix(0, ms*1e6)` automatically.

## Try it locally

The [docker-compose example](https://github.com/VersusControl/versus-incident/tree/main/examples/docker-compose)
ships an `elasticsearch` + `kibana` stack. Send some test logs:

```bash
curl -X POST http://localhost:9200/logs-demo/_doc \
  -H 'Content-Type: application/json' \
  -d '{"@timestamp":"2026-05-12T10:00:00Z","message":"db connection refused","level":"error","service":"api"}'
```

Then enable the sample ES source in `agent_sources.yaml` and watch
the catalog pick it up on the **Patterns** page in the admin UI.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `failed to query elasticsearch: 401` | Wrong creds / API key expired. |
| `failed to query elasticsearch: 403` | Role missing `read` on `index`. |
| Cursor never advances | `time_field` not present in returned docs, or `query` matches nothing. |
| Only old data, then silence | `time_field` doesn't match the field your docs actually use. |
| Connects, no error, nothing ingested | Your log body is in a different field than `query`/`message_field` target — Fluent Bit typically stores it in `log`, not `message`. Point both at the real field (see the Fluent Bit note above). |
