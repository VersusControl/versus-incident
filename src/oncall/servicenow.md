# ServiceNow

Versus Incident can escalate unacknowledged alerts to [ServiceNow](https://www.servicenow.com/) by creating records through the ServiceNow [Table API](https://docs.servicenow.com/bundle/washingtondc-api-reference/page/integrate/inbound-rest/concept/c_TableAPI.html). When on-call is enabled with the `servicenow` provider, Versus posts a new record (by default to the `incident` table) for every incident that is not acknowledged within the configured wait period.

## How it works

When an incident escalates, Versus sends an HTTP `POST` to:

```
{instance_url}/api/now/table/{table}
```

- Authentication uses **HTTP Basic auth** with the configured `username` and `password`.
- The incident ID is mapped onto two fields:
  - `short_description` — a human-readable summary (`Incident <id>`).
  - `correlation_id` — used by ServiceNow to de-duplicate inbound events.
- The request uses the shared HTTPS client with TLS verification enabled.

A non-2xx response from ServiceNow is treated as a failure and logged.

## Configuration

Add the `servicenow` block under `oncall` in your `config.yaml` and set `provider: servicenow`:

```yaml
oncall:
  enable: true
  wait_minutes: 3
  provider: servicenow

  servicenow:
    instance_url: ${SERVICENOW_INSTANCE_URL} # eg https://dev12345.service-now.com (REQUIRED)
    username: ${SERVICENOW_USERNAME}          # REQUIRED
    password: ${SERVICENOW_PASSWORD}          # REQUIRED
    table: incident                           # ServiceNow table; defaults to "incident"
    other_instance_urls:                      # Optional: per-request instance override
      infra: ${SERVICENOW_OTHER_INSTANCE_URL_INFRA}
      app: ${SERVICENOW_OTHER_INSTANCE_URL_APP}
      db: ${SERVICENOW_OTHER_INSTANCE_URL_DB}

redis: # Required for on-call functionality
  host: ${REDIS_HOST}
  port: ${REDIS_PORT}
  password: ${REDIS_PASSWORD}
  db: 0
```

| Field | Required | Description |
|-------|----------|-------------|
| `instance_url` | Yes | Base URL of your ServiceNow instance, e.g. `https://dev12345.service-now.com`. |
| `username` | Yes | ServiceNow user with permission to create records on the target table. |
| `password` | Yes | Password for the user. Provide via an environment variable; never commit it. |
| `table` | No | Table to create records in. Defaults to `incident`. |
| `other_instance_urls` | No | Map of named instance URLs selectable per request. |

### Credentials

Provide `username` and `password` through environment variables (`${SERVICENOW_USERNAME}`, `${SERVICENOW_PASSWORD}`) — never hard-code them in `config.yaml`. Versus never logs credentials.

## Per-request override

You can route a specific alert to a different ServiceNow instance using the `servicenow_other_instance` query parameter. The value must match a key under `other_instance_urls`:

```
POST /api/incidents?servicenow_other_instance=infra
```

This overrides `instance_url` for that single request only; the global config is never mutated.

## Environment variables

| Variable | Description |
|----------|-------------|
| `SERVICENOW_INSTANCE_URL` | Default instance URL. |
| `SERVICENOW_USERNAME` | Basic auth username. |
| `SERVICENOW_PASSWORD` | Basic auth password. |
| `SERVICENOW_OTHER_INSTANCE_URL_<NAME>` | Named alternate instance URLs (e.g. `SERVICENOW_OTHER_INSTANCE_URL_INFRA`). |
