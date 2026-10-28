# Elasticsearch source

Pulls log documents from Elasticsearch / OpenSearch / Elastic Cloud
using the `_search` API with a time-ordered, paginated range scan.

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
  tie_breaker_field: event.id      # optional; unique non-empty keyword field with doc_values
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
- **Pagination (time-only, the default)** — Without `tie_breaker_field`, the
  scan sorts by `time_field` ascending in a short-lived scroll snapshot, so it
  works on indices with no unique field (for example Fluent Bit logs with
  `@timestamp`, `log`, `time`, `kubernetes.*`). Scroll pages can cross 10,000
  equal-timestamp documents without `from` offsets or changing shard-copy
  order. Continuations and cleanup use the address that opened the snapshot;
  a failed continuation fails the tick without advancing its cursor. Known
  scroll IDs are cleared after success, cancellation, or failure. If an opening
  response exceeds the 8 MiB limit before its ID can be decoded, or its ID is
  too large to fit a bounded cleanup request, the server-side context cannot
  be cleared by the source and expires after the one-minute scroll keepalive.
- **Pagination (with `tie_breaker_field`)** — Sorts by
  `(time_field, tie_breaker_field)` ascending and passes both returned sort
  values to `search_after`. The tie breaker must be present, unique across
  matching documents, mapped as a keyword field with `doc_values`, and returned
  as a non-empty string. Configure one when an index sees high-volume bursts
  that share a timestamp (coarse, second-precision timestamps or batch
  shippers), especially when a tick may need to scan 50,000 or more matching
  documents. The field must be mapped in every index the
  pattern matches.
  The source does not sort on `_id`, because `_id` has no doc values by default.
- **Shard failures** — Elasticsearch reports per-shard failures (for example
  sorting on an unmapped field) with HTTP 200 and no hits. The source treats
  any failed shard as a failed tick: it reports the failure count and the first
  reason, and does not advance the cursor.
- **Partial ticks** — A tick emits at most 10,000 documents or 8 MiB, with
  individual search responses capped at 8 MiB. Previously delivered rows do
  not consume the emission limits, so later ticks can scan through a
  same-timestamp prefix and reach unseen rows. Scanning remains bounded at
  50,000 hits per tick. If a time-only scroll reaches that ceiling before
  completion, the source fails the tick rather than advancing a timestamp-only
  cursor across rows it cannot safely track with bounded durable dedup. Configure
  a unique keyword `tie_breaker_field` or narrow the query/reorder window.
  An oversized later scroll page also fails that tick without staging its
  partial results; subsequent ticks reopen from the original cursor with a
  smaller page size until the page fits. A single projected document larger
  than 8 MiB still requires narrowing the query or projected fields.
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
| `search failed on one or more shards … No mapping found for [<field>] in order to sort on` | `tie_breaker_field` names a field that is not mapped in the index. Remove it to use time-only pagination, or point it at a mapped unique keyword field. |
