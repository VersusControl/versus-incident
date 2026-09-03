# SigNoz source

Pulls log entries from [SigNoz](https://signoz.io/) (self-hosted) or
SigNoz Cloud via the v5 query API.

Requires **SigNoz v0.87.0 or later**: that is where v5 query API first appears (verified at source level against the tagged releases — the route is registered at `v0.87.0` and absent at `v0.86.0`).

## Minimal config (self-hosted)

```yaml
sources:
  - name: prod-signoz
    type: signoz
    enable: true
    signoz:
      address: http://signoz:8080
      api_key: ${SIGNOZ_API_KEY}
      query: "severity_text = 'ERROR' AND service.name = 'api'"
      page_size: 500
```

## SigNoz Cloud

Same endpoint, same header — only the address changes.

```yaml
sources:
  - name: cloud-signoz
    type: signoz
    enable: true
    signoz:
      address: https://<region>.signoz.cloud   # your Cloud region host
      api_key: ${SIGNOZ_API_KEY}               # Settings → API Keys (query key)
      query: "severity_text = 'ERROR' AND k8s.namespace.name = 'prod'"
      severity_field: severity_text
      extra_fields:
        - resources_string.service.name
        - k8s.namespace.name
      page_size: 500
```

## Full reference

```yaml
signoz:
  address: http://signoz:8080       # REQUIRED. SigNoz base URL — the UI/API port
                                    # self-hosted, or https://<region>.signoz.cloud.
  api_key: ${SIGNOZ_API_KEY}        # REQUIRED. Sent as the SIGNOZ-API-KEY header.
                                    # The QUERY key from Settings → API Keys —
                                    # NOT the signoz-ingestion-key.

  insecure_skip_verify: false       # dev only — disables TLS verification.

  query: "severity_text = 'ERROR'"  # v5 filter EXPRESSION (same syntax as the
                                    # Logs Explorer filter bar). Empty matches
                                    # every log in the window.

  message_field: body               # row attribute copied to Signal.Message.
  severity_field: severity_text     # row attribute copied to Signal.Severity.
  extra_fields:                     # extra row attributes copied to Signal.Fields.
    - resources_string.service.name # container-qualified — unambiguous.
    - k8s.namespace.name            # bare — searched across the containers.

  page_size: 500                    # per-request `limit`; clamped to 1000.
  reorder_window: 2m                # how far below the cursor each tick re-scans.
```

### Addressing attributes

SigNoz does **not** flatten OTLP attributes. It nests them in per-type
maps whose **keys carry the dots**:

```json
{
  "body": "upstream returned 500",
  "severity_text": "ERROR",
  "resources_string":  { "service.name": "checkout", "k8s.namespace.name": "shop" },
  "attributes_string": { "http.status_code": "500" }
}
```

`extra_fields`, `message_field` and `severity_field` all resolve a name
the same way, first hit wins:

1. a **top-level column** — `body`, `id`, `severity_text`, or a whole
   container map such as `resources_string`;
2. a **container-qualified** name — `resources_string.service.name` —
   where the first segment names the container;
3. a **bare** attribute name searched across the attribute containers in
   this fixed order: `resources_string`, `attributes_string`,
   `attributes_number`, `attributes_bool`, `scope_string`. Resource
   attributes win because `service.name` and the `k8s.*` set describe the
   emitter's stable identity;
4. a nested JSON path, for payloads that really do nest.

Both spellings of one attribute land in `Signal.Fields` under the **bare**
name — the container is SigNoz's storage column, not part of the
attribute's identity, so `resources_string.service.name` is emitted as
`service.name`. Qualify the name when you need to pin which container you
mean; leave it bare otherwise.

## Behavior

- **Cursor** — SigNoz has no server-side tail cursor, so the source
  uses the Elasticsearch tailing model rather than the Loki one. Each
  tick queries `[cursor - reorder_window, now]` with an **inclusive**
  lower bound, ordered `timestamp asc, id asc` (SigNoz needs the `id`
  tiebreak for a stable order over `timestamp`).
- **Pagination** — `offset` is walked **within a tick only** (up to 20
  pages), never carried across ticks, and stops on the first short
  page. The window moves between ticks, so a carried offset would point
  at a different row set.
- **De-duplication** — rows already delivered are tracked by the log
  row `id` and skipped when the overlapping re-scan pulls them back, so
  each row is learned once. A row without an `id` falls back to a
  composite of its timestamp and the first 256 characters of its
  message.
- **Restarts** — both halves of the position are durable: the timestamp
  in the poll cursor, the `id` set alongside it. A restart resumes with
  **no duplicates and no dropped rows**. Without Redis both halves are
  in-process, and a restart re-reads one reorder window once.
- **Timestamps** — read from the row envelope, falling back to the
  row's own `timestamp` attribute. SigNoz stamps log timestamps in
  nanoseconds; a bare epoch number is interpreted by magnitude
  (seconds / milliseconds / nanoseconds).
- **Transport** — exactly one path is ever requested
  (`/api/v5/query_range`), the API key travels as a header and never as
  a query parameter, transient rejections (429 / 5xx / transport
  errors) are retried up to 3 attempts, and a response body is capped
  at 16 MiB.

### What `reorder_window` is for

SigNoz publishes **no ingest-to-queryable ordering guarantee** — a log
line can become queryable *after* the poll cursor has already passed
its timestamp (OTel collector batching, ClickHouse insert lag, clock
skew). A bare forward cursor would step over it permanently.

`reorder_window` is the fix: every tick re-scans that far **below** the
cursor, so a log that arrives late but *inside* the window is still
picked up on the next tick, and de-duplication keeps it from being
emitted twice.

The `2m` default is a deliberately conservative inference from the
ingest path's shape, not a measured delay. Tune it:

| Change | Effect |
|---|---|
| **Lower** (e.g. `30s`) | Cheaper ticks and a smaller dedup set. Do this once you have measured your own pipeline. |
| **Raise** (e.g. `5m`) | Catches later arrivals, at the cost of re-scanning more rows every tick. |

### The span actually scanned

The agent scans `reorder_window + agent.catalog.persist_interval` below the
cursor, and logs that effective span once at boot.

The persist interval is added because delivered ids are only made durable after
the flush that stored the rows they describe: the re-read span is therefore also
what a restarted process replays to recover everything a killed one read but
never stored, and those rows span up to one persist interval. Lowering
`reorder_window` narrows the lateness tolerance and nothing else — it cannot
switch off crash recovery.

A log arriving **more than** `reorder_window` late is not recovered.
That bound is what keeps the per-tick re-scan and the dedup set small.

## Filter expression cheatsheet

`query` takes a SigNoz **v5 filter expression** — the same syntax as
the Logs Explorer filter bar. It is not LogQL and not SQL.

| Goal | Query |
|---|---|
| Errors only | `severity_text = 'ERROR'` |
| One service | `service.name = 'api'` |
| Errors from one service | `severity_text = 'ERROR' AND service.name = 'api'` |
| One namespace | `k8s.namespace.name = 'prod'` |
| Everything in the window | *(leave `query` empty)* |

Attribute names are **dotted** (`service.name`, `k8s.namespace.name`) —
that is SigNoz's convention, not ours.

Build the expression in the SigNoz **Logs Explorer** and paste it in:
the source passes it through verbatim, so whatever the Explorer accepts
is what this field accepts, and whatever it rejects fails the tick.

## Permissions

- Create the key under **Settings → API Keys**. Creating a service
  account / API key requires the **Admin** role.
- API keys are available on **SigNoz Cloud** and on **self-hosted
  community** — you do not need Cloud for this source.
- The key only needs to read; the source has **no write path** to
  SigNoz.

## Limitations

- **No analyze auto-wire.** Configuring a SigNoz source does **not**
  populate the `query_metrics` / `query_traces`
  [analyze tools](../tools/tools.md) — those still point at
  Prometheus / Tempo and are configured by hand. Planned, not shipped.
- **SigNoz alerts are not ingested.** This is a data source that tails
  logs, not an alert receiver.

## Try it locally

A runnable stack lives at `examples/docker-compose/signoz/`
([on GitHub](https://github.com/VersusControl/versus-incident/tree/main/examples/docker-compose/signoz)) —
SigNoz plus Versus, every value defaulted so `docker compose up -d`
works with no configuration.

> **It is a heavy stack.** SigNoz brings ClickHouse, ClickHouse Keeper,
> Postgres and an OTel collector — budget **≥4 GB** of Docker memory.
> That is far more than the `loki/` example beside it.

Then browse your logs in the SigNoz UI and confirm the agent catalogs
them at <http://localhost:3000/api/agent/patterns>.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `401` / `403` on every tick | Wrong key type — an **ingestion** key was used instead of a **query** API key, or the key was revoked. |
| `404` on `/api/v5/query_range` | SigNoz is older than **v0.87.0**; the v5 endpoint does not exist yet. Upgrade. |
| `400` on every tick | Invalid `query` — test the filter expression in the Logs Explorer first. |
| No new entries but logs exist in SigNoz | `query` matches nothing in the current window, or `address` points at the collector (4317/4318) instead of the SigNoz UI/API port. |
| Severity always empty | `severity_field` (default `severity_text`) is not populated on your rows — point it at the attribute your shipper actually sets. |
| A burst of repeated signals after a restart | The `id` set is persisted only when Redis is configured. Without it, one `reorder_window` is re-read once per restart. |
| Logs that arrive very late are missed | They landed outside `reorder_window` — raise it. |

## See also

- Source list and cursor model: [Data Sources](../data-sources.md)
- Metrics from the same backend (Enterprise): [SigNoz Metrics](../../enterprise/metrics/signoz.md)
- Traces from the same backend (Enterprise): [Traces](./traces.md#signoz-backend)
