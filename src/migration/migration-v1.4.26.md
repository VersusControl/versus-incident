# Migrating to v1.4.26

This release adds bounded, read-only Elasticsearch tools and strengthens the
existing Elasticsearch log source. Deployments without an enabled
Elasticsearch source need no migration steps.

Elasticsearch users must verify pagination and connection settings before
restarting Versus. There is no database migration.

## Upgrading

```bash
# Docker
docker pull ghcr.io/versuscontrol/versus-incident:v1.4.26

# Helm
helm repo update
helm upgrade versus-incident oci://ghcr.io/versuscontrol/charts/versus-incident \
  --version 1.4.26
```

## Who Needs to Act

| Your setup | What to do |
|---|---|
| No enabled Elasticsearch source | Nothing. Upgrade normally. |
| Elasticsearch documents have a unique keyword `event.id` | You may omit `tie_breaker_field`; Versus uses `event.id` by default. |
| Documents do not have `event.id`, or it is missing or duplicated | Configure `tie_breaker_field` to another unique keyword field with `doc_values` and ensure every matching document has a value. |
| Elasticsearch uses Basic auth or an API key | Use verified HTTPS. Plain HTTP and `insecure_skip_verify: true` are rejected when credentials are configured. |
| Elasticsearch runs on localhost | Set `allow_loopback: true`. |
| Credentials are embedded in an address URL | Move them to `username`/`password` or `api_key`; URL userinfo is rejected. |

## Tie-breaker Pagination

Elasticsearch pagination now sorts each source by two fields:

1. `time_field`, ascending;
2. `tie_breaker_field`, ascending.

The second field prevents documents with the same timestamp from being skipped
between pages or bounded polling ticks.

### What happens when the field is omitted?

When `tie_breaker_field` is empty or absent, Versus sets its effective value to:

```yaml
tie_breaker_field: event.id
```

This default is also visible in the redacted admin configuration response. It is
not a fallback chain: Versus does not fall back to Elasticsearch `_id` because
`_id` does not provide doc values for sorting.

Every matching document must therefore contain a unique, non-empty string in
`event.id`. The field must be mapped as `keyword` with `doc_values` enabled. If
your documents already follow ECS and satisfy those requirements, no config
change is needed.

If the effective tie-breaker is missing, null, duplicated, not a string, or not
returned consistently by Elasticsearch sorting, the source fails that pull
without emitting the page or advancing its cursor. The server log identifies
the configured `tie_breaker_field`. This fail-closed behavior prevents silent
log loss.

### Using a different field

Configure a different field when `event.id` is unavailable:

```yaml
sources:
  - name: prod-app
    type: elasticsearch
    enable: true
    elasticsearch:
      addresses:
        - https://es.prod.example:9200
      index: "logs-app-*"
      time_field: "@timestamp"
      tie_breaker_field: versus.event_id
      message_field: message
```

The replacement field must:

- be present on every document matched by `index` and `query`;
- contain a unique, non-empty string;
- be mapped as `keyword` with `doc_values` enabled;
- differ from `time_field`;
- not be `_id` or another Elasticsearch metadata field.

Check the mapping before upgrading:

```http
GET logs-app-*/_mapping/field/event.id
```

Check for missing values:

```http
POST logs-app-*/_count
{
  "query": {
    "bool": {
      "must_not": {
        "exists": { "field": "event.id" }
      }
    }
  }
}
```

If documents do not satisfy the contract, update the producer or backfill a
suitable keyword field before enabling the source on v1.4.26.

## Connection Hardening

The shared Elasticsearch client now applies the same connection policy to log
ingestion and read-only agent tools:

- Basic authentication and API keys require verified HTTPS.
- Credentialed `http://` endpoints are rejected.
- `insecure_skip_verify: true` is rejected when credentials are configured.
- Credentials embedded in an address URL are rejected.
- Redirects and ambient HTTP proxies are not used.
- Localhost and other loopback destinations require `allow_loopback: true`.
- Link-local, metadata, multicast, and unspecified destinations remain blocked.

A local, unauthenticated development source therefore needs an explicit opt-in:

```yaml
elasticsearch:
  addresses:
    - http://localhost:9200
  allow_loopback: true
  index: "logs-demo-*"
  tie_breaker_field: event.id
```

## Bounds

`page_size` is now capped at `1000`. Larger configured values are reduced to
that limit. Pulls and agent queries also enforce response, item, scan, and time
bounds. Large backlogs are processed in bounded batches and resume from the last
accepted pagination position.

For full configuration and troubleshooting guidance, see
[Elasticsearch source](../agent/data-sources/elasticsearch.md).
