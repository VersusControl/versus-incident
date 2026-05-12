# Loki source

Pulls log entries from [Grafana Loki](https://grafana.com/oss/loki/)
(self-hosted) or Grafana Cloud Logs via the
`/loki/api/v1/query_range` API.

## Minimal config (self-hosted)

```yaml
sources:
  - name: prod-loki
    type: loki
    enable: true
    loki:
      address: http://loki:3100
      query: '{app="api"} |= "error"'
      page_size: 500
```

## Grafana Cloud

```yaml
sources:
  - name: gcloud-logs
    type: loki
    enable: true
    loki:
      address: https://logs-prod-006.grafana.net
      username: ${GRAFANA_CLOUD_INSTANCE_ID}     # the numeric instance ID
      password: ${GRAFANA_CLOUD_API_TOKEN}       # API token with `logs:read`
      query: '{namespace="prod"} |~ "(?i)error|panic"'
      severity_field: level
      extra_labels:
        - app
        - namespace
      page_size: 500
```

## Multi-tenant Loki

```yaml
loki:
  address: http://loki-gateway:3100
  tenant_id: ${LOKI_TENANT_ID}    # sent as X-Scope-OrgID
  bearer_token: ${LOKI_TOKEN}
  query: '{cluster="prod"} |= ""'
```

## Full reference

```yaml
loki:
  address: http://loki:3100         # REQUIRED. Loki base URL.
  tenant_id: ""                     # X-Scope-OrgID (multi-tenant / Grafana Cloud).

  # Auth — pick at most one. Bearer wins when both are set.
  username: ""
  password: ""
  bearer_token: ""

  insecure_skip_verify: false       # dev only

  query: '{app="api"} |= "error"'   # REQUIRED. LogQL selector + filter.
  severity_field: level             # optional; read from stream LABELS, not log line.
  extra_labels:                     # extra stream labels copied to Signal.Fields.
    - app
    - namespace
  page_size: 500                    # Loki caps around 5000.
```

## Behavior

- **Cursor** — The maximum log entry timestamp seen on the previous
  tick. The next query uses `start = cursor + 1ns` because Loki's
  `start` is **inclusive** — bumping by one nanosecond avoids
  re-reading the boundary entry.
- **Direction** — Always `direction=forward` so the stream is read
  oldest-first.
- **Time range** — `start = max(cursor+1ns, now - lookback)`,
  `end = now`. Both as nanosecond Unix timestamps.
- **Severity** — Read from stream **labels**, not the log line
  itself. Make sure your label set includes `level` (or whatever you
  use in `severity_field`).

## LogQL cheatsheet

| Goal | Query |
|---|---|
| All logs from one app | `{app="api"}` |
| Errors only | `{app="api"} |= "error"` |
| Case-insensitive regex | `{app="api"} |~ "(?i)error|panic"` |
| Multiple namespaces | `{namespace=~"prod|staging"}` |
| JSON-extracted field | `{app="api"} | json | level="error"` |

See the full
[LogQL reference](https://grafana.com/docs/loki/latest/logql/log_queries/).

## Permissions

- **Self-hosted Loki** — no built-in RBAC; place Loki behind a
  gateway (e.g. nginx) and pass `bearer_token` / Basic auth.
- **Grafana Cloud** — create an Access Policy with the `logs:read`
  scope, then issue a token under that policy.

## Try it locally

The [docker-compose example](https://github.com/VersusControl/versus-incident/tree/main/examples/docker-compose)
ships a `loki` + `grafana` stack. Push a few log lines:

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

Browse Loki at <http://localhost:3000/explore> (Grafana) and confirm
the agent catalogs the line at
<http://localhost:3000/api/agent/patterns>.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `loki: 401 Unauthorized` | Wrong instance ID / API token (Grafana Cloud) or missing tenant header. |
| `loki: 400 parse error` | Invalid LogQL — test in Grafana Explore first. |
| No new entries but logs exist | `query` matches nothing for the current `start..end` window — try widening `agent.lookback`. |
| Severity always empty | `severity_field` reads from stream labels, not the line body — promote the field to a label or extract via `\| json \| label_format`. |
