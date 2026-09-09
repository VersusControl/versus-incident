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
      # Required when an address resolves to localhost or another loopback IP.
      allow_loopback: false
      index: "logs-demo-*"
      time_field: "@timestamp"
      tie_breaker_field: event.id
      query: '*'
      message_field: message
      reorder_window: 1m
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
  allow_loopback: false            # set true explicitly for localhost / loopback
  insecure_skip_verify: false      # unauthenticated dev only; forbidden with credentials

  index: "logs-app-*"              # REQUIRED. Wildcards supported.
  time_field: "@timestamp"         # REQUIRED. Used for sort + range filter.
  tie_breaker_field: event.id       # unique non-empty keyword field with doc_values
  query: 'log.level:(error OR warn)'  # Lucene-style; "*" = match all.

  message_field: message           # field copied to Signal.Message
  severity_field: log.level        # optional; copied to Signal.Severity
  extra_fields:                    # extra fields copied to Signal.Fields
    - service.name
    - host.name
    - error.stack_trace

  page_size: 500                   # _search size; capped at 1000
  reorder_window: 1m               # inclusive re-scan below the time cursor
```

**Fluent Bit users — check which field holds your log body.**

When you ship container logs to Elasticsearch with Fluent Bit, the real log line is usually
stored in the **`log`** field (the CRI/Docker parsers also add `stream`,
`logtag`, `time`), and `message` is often empty or absent. If `query` and
`message_field` point at `message` but your text is in `log`, the source
connects fine and matches **zero** documents — no error, no alert, nothing
on the dashboard. Point both at the field that actually carries the text:
```yaml
      time_field: "@timestamp"
      tie_breaker_field: event.id
      query: 'log:error'      # was message:error
      message_field: log      # was message
      page_size: 500
```
Not sure which field it is? Query the index and inspect one document:
`GET your-index-*/_search { "size": 1, "_source": ["@timestamp","message","log"] }`.

## Behavior

- **Cursor** — Stores the maximum emitted `time_field` timestamp. Each tick
  re-reads from `cursor - reorder_window` with an inclusive `gte` lower bound;
  the durable delivered-ID set suppresses rows already emitted from that
  overlap. This lets a successful partial tick resume within a timestamp even
  though the persisted cursor itself is time-only.
- **Pagination** — Within each inclusive `gte` reorder-window scan, sorts by
  `(time_field, tie_breaker_field)` ascending and passes both returned sort
  values to `search_after`. The tie breaker must be present, unique across
  matching documents, mapped as a keyword field with `doc_values`, and returned
  as a non-empty string. `event.id` is the default for ECS-compatible indices.
  The source does not sort on `_id`, because `_id` has no doc values by default.
- **Partial ticks** — A tick emits at most 10,000 documents or 8 MiB. Previously
  delivered rows do not consume either limit, so later ticks scan through a
  same-timestamp prefix and reach unseen rows. Total scanning remains bounded
  at 50,000 hits per tick, matching the durable delivered-ID capacity;
  exhausting that bound without an unseen row reports
  an unhealthy source with guidance to narrow the query/window or fix tie-break
  selectivity.
- **Auth precedence** — `api_key` wins over `username`/`password`
  when both are set.
- **Credential transport** — Basic auth and API keys require verified HTTPS.
  Credentialed HTTP and credentialed `insecure_skip_verify: true` are rejected.
- **URL credentials** — Userinfo in an address, such as
  `https://user:password@host`, is rejected. Use the dedicated credential
  fields so validation and error reporting cannot expose secrets.
- **Security migration** — Existing configurations that put credentials in
  `addresses` must move them to `username`/`password` or `api_key`, use verified
  HTTPS, and remove `insecure_skip_verify`. Local unauthenticated HTTP also
  requires the explicit `allow_loopback: true` opt-in.
- **Loopback opt-in** — `localhost`, `*.localhost`, `127.0.0.0/8`, and `::1`
  require `allow_loopback: true`. Resolved destinations are checked again when
  dialing, so DNS cannot redirect a permitted hostname to a blocked address.
- **Redirects** — HTTP redirects are rejected. Configure every cluster address
  as the final permitted endpoint instead of relying on a redirect target.

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
curl -X POST http://localhost:9200/logs-demo-000001/_doc \
  -H 'Content-Type: application/json' \
  -d '{"@timestamp":"2026-05-12T10:00:00Z","event":{"id":"demo-000001"},"message":"db connection refused","level":"error","service":"api"}'
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
