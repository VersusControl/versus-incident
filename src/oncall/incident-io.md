# incident.io

Versus Incident can escalate unacknowledged alerts to [incident.io](https://incident.io/) through its [HTTP alert source](https://api-docs.incident.io/) events endpoint. When on-call is enabled with the `incident_io` provider, Versus sends an alert event for every incident that is not acknowledged within the configured wait period.

## How it works

When an incident escalates, Versus sends an HTTP `POST` to the incident.io HTTP alert events endpoint:

```
https://api.incident.io/v2/alert_events/http/{alert_source_config_id}
```

- Authentication uses an `Authorization: Bearer {api_key}` header.
- The incident fields are mapped onto the alert payload:
  - `title` — `Incident <id>`.
  - `deduplication_key` — the incident ID, so repeated escalations collapse onto the same alert.
  - `status` — `firing`.
  - `metadata.incident_id` — the incident ID.
- The request uses the shared HTTPS client with TLS verification enabled.

A non-2xx response from incident.io is treated as a failure and logged.

## Configuration

Add the `incident_io` block under `oncall` in your `config.yaml` and set `provider: incident_io`:

```yaml
oncall:
  enable: true
  wait_minutes: 3
  provider: incident_io

  incident_io:
    api_key: ${INCIDENTIO_API_KEY}                       # Bearer API key (REQUIRED)
    alert_source_config_id: ${INCIDENTIO_ALERT_SOURCE_CONFIG_ID} # HTTP alert source config ID (REQUIRED)
    other_alert_source_config_ids:                       # Optional: per-request override
      infra: ${INCIDENTIO_OTHER_ALERT_SOURCE_CONFIG_ID_INFRA}
      app: ${INCIDENTIO_OTHER_ALERT_SOURCE_CONFIG_ID_APP}
      db: ${INCIDENTIO_OTHER_ALERT_SOURCE_CONFIG_ID_DB}

redis: # Required for on-call functionality
  host: ${REDIS_HOST}
  port: ${REDIS_PORT}
  password: ${REDIS_PASSWORD}
  db: 0
```

| Field | Required | Description |
|-------|----------|-------------|
| `api_key` | Yes | Bearer API key for the HTTP alert source. Provide via an environment variable; never commit it. |
| `alert_source_config_id` | Yes | The HTTP alert source config ID created in incident.io. |
| `other_alert_source_config_ids` | No | Map of named alert source config IDs selectable per request. |

### Getting the alert source config ID and API key

1. In incident.io, create an **HTTP alert source** under **Alerts → Sources**.
2. Copy the alert source config ID from the source's URL/settings.
3. Create an API key with permission to create alert events and use it as `api_key`.

### Credentials

Provide `api_key` through an environment variable (`${INCIDENTIO_API_KEY}`) — never hard-code it in `config.yaml`. Versus never logs the API key.

## Per-request override

You can route a specific alert to a different incident.io alert source using the `incidentio_other_alert_source` query parameter. The value must match a key under `other_alert_source_config_ids`:

```
POST /api/incidents?incidentio_other_alert_source=infra
```

This overrides `alert_source_config_id` for that single request only; the global config is never mutated.

## Environment variables

| Variable | Description |
|----------|-------------|
| `INCIDENTIO_API_KEY` | Bearer API key. |
| `INCIDENTIO_ALERT_SOURCE_CONFIG_ID` | Default HTTP alert source config ID. |
| `INCIDENTIO_OTHER_ALERT_SOURCE_CONFIG_ID_<NAME>` | Named alternate alert source config IDs (e.g. `INCIDENTIO_OTHER_ALERT_SOURCE_CONFIG_ID_INFRA`). |
