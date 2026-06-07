# Integrate with incident.io

This document provides a step-by-step guide to integrate Versus Incident with [incident.io](https://incident.io/) for on-call management. The integration escalates unacknowledged alerts into incident.io through its [HTTP alert source](https://api-docs.incident.io/) events endpoint.

We'll cover setting up incident.io, configuring the integration with Versus, deploying Versus, and testing the integration with practical examples.

## Prerequisites

Before you begin, ensure you have:
+ An incident.io account with permission to manage alert sources
+ Versus Incident deployed (instructions provided later)
+ Prometheus Alert Manager set up to monitor your systems

## Setting Up incident.io for On-Call

Let's configure an HTTP alert source, capture its config ID, and create an API key.

### 1. Create an HTTP Alert Source

The HTTP alert source is the endpoint Versus sends events to:

1. Log in to incident.io
2. Navigate to **Alerts** > **Sources** > **Add alert source**
3. Choose **HTTP** as the source type
4. Name the source (e.g., "Versus Incident")
5. Save the source

### 2. Capture the Alert Source Config ID

Each HTTP alert source has a config ID embedded in its events endpoint:

```
https://api.incident.io/v2/alert_events/http/{alert_source_config_id}
```

1. Open the alert source you just created
2. Copy the **config ID** from the source's settings or its endpoint URL
3. Keep it handy — you'll set it as `alert_source_config_id` in Versus

### 3. Create an API Key

Versus authenticates with a Bearer API key:

1. Navigate to **Settings** > **API keys** > **Add API key**
2. Name the key (e.g., "Versus Integration")
3. Grant the permission required to create alert events
4. Copy the key — it's shown only once. Store it securely; you'll set it as `api_key`.

### 4. Set Up Escalation in incident.io

Decide what should happen when an alert event arrives:

1. Configure an **alert route** that matches events from your Versus alert source
2. Attach the **escalation path** / on-call schedule that should be paged
3. Save the route

This is what turns an inbound alert event into a page for the right on-call engineer.

### 5. Verify the Endpoint

You can confirm connectivity before wiring up Versus:

```bash
curl -H "Authorization: Bearer YOUR_API_KEY" \
  -H "Content-Type: application/json" \
  -X POST "https://api.incident.io/v2/alert_events/http/YOUR_CONFIG_ID" \
  -d '{"title":"Versus connectivity test","deduplication_key":"test-001","status":"firing"}'
```

A 2xx response confirms the API key and config ID are correct.

## How the Mapping Works

When an alert escalates, Versus sends an HTTP `POST` to the alert events endpoint with a Bearer token and maps the incident onto the alert payload:

| incident.io field | Value |
|-------------------|-------|
| `title` | A human-readable summary (`Incident <id>`) |
| `deduplication_key` | The incident ID, so repeated escalations collapse onto the same alert |
| `status` | `firing` |
| `metadata.incident_id` | The incident ID |

The request uses the shared HTTPS client with TLS verification enabled. A non-2xx response is treated as a failure and logged. The API key is never written to logs.

## Deploy Versus Incident

Now let's deploy Versus with incident.io integration. You can use Docker or Kubernetes.

### Docker Deployment

Create a directory for your configuration files:

```bash
mkdir -p ./config
```

Create `config/config.yaml` with the following content:

```yaml
name: versus
host: 0.0.0.0
port: 3000
public_host: https://your-ack-host.example

alert:
  debug_body: true

  slack:
    enable: true
    token: ${SLACK_TOKEN}
    channel_id: ${SLACK_CHANNEL_ID}
    template_path: "config/slack_message.tmpl"

oncall:
  enable: true
  wait_minutes: 3
  provider: incident_io

  incident_io:
    api_key: ${INCIDENTIO_API_KEY}                               # Bearer API key (REQUIRED)
    alert_source_config_id: ${INCIDENTIO_ALERT_SOURCE_CONFIG_ID} # HTTP alert source config ID (REQUIRED)
    other_alert_source_config_ids:                               # Optional: per-request override
      infra: ${INCIDENTIO_OTHER_ALERT_SOURCE_CONFIG_ID_INFRA}
      app: ${INCIDENTIO_OTHER_ALERT_SOURCE_CONFIG_ID_APP}
      db: ${INCIDENTIO_OTHER_ALERT_SOURCE_CONFIG_ID_DB}

redis: # Required for on-call functionality
  insecure_skip_verify: true # dev only
  host: ${REDIS_HOST}
  port: ${REDIS_PORT}
  password: ${REDIS_PASSWORD}
  db: 0
```

By default, Versus adds an interactive acknowledgment button to Slack notifications when on-call is enabled, using the unified template shipped in `config/slack_message.tmpl`. If the alert is acknowledged before `wait_minutes` elapses, incident.io is never called.

Create the `docker-compose.yml` file:

```yaml
version: '3.8'

services:
  versus:
    image: ghcr.io/versuscontrol/versus-incident
    ports:
      - "3000:3000"
    environment:
      - SLACK_TOKEN=your_slack_token
      - SLACK_CHANNEL_ID=your_channel_id
      - INCIDENTIO_API_KEY=your_incidentio_api_key
      - INCIDENTIO_ALERT_SOURCE_CONFIG_ID=your_alert_source_config_id
      - REDIS_HOST=redis
      - REDIS_PORT=6379
      - REDIS_PASSWORD=your_redis_password
    volumes:
      - ./config:/app/config:ro
    depends_on:
      - redis

  redis:
    image: redis:6.2-alpine
    command: redis-server --requirepass your_redis_password
    ports:
      - "6379:6379"
    volumes:
      - redis_data:/data

volumes:
  redis_data:
```

Run Docker Compose:

```bash
docker-compose up -d
```

> **Warning:** Always provide `api_key` through an environment variable. Never commit it to `config.yaml`.

## Alert Manager Routing Configuration

Now, let's configure Alert Manager to route alerts to Versus with different behaviors:

### Send Alert Only (No On-Call)

```yaml
receivers:
- name: 'versus-no-oncall'
  webhook_configs:
  - url: 'http://versus-service:3000/api/incidents?oncall_enable=false'
    send_resolved: false

route:
  receiver: 'versus-no-oncall'
  group_by: ['alertname', 'service']
  routes:
  - match:
      severity: warning
    receiver: 'versus-no-oncall'
```

### Send Alert with Acknowledgment Wait

```yaml
receivers:
- name: 'versus-with-ack'
  webhook_configs:
  - url: 'http://versus-service:3000/api/incidents?oncall_wait_minutes=5'
    send_resolved: false

route:
  routes:
  - match:
      severity: critical
    receiver: 'versus-with-ack'
```

This waits 5 minutes for acknowledgment before sending an alert event to incident.io if the user doesn't click the ACK link in Slack.

### Send Alert with Immediate On-Call Trigger

```yaml
receivers:
- name: 'versus-immediate'
  webhook_configs:
  - url: 'http://versus-service:3000/api/incidents?oncall_wait_minutes=0'
    send_resolved: false

route:
  routes:
  - match:
      severity: urgent
    receiver: 'versus-immediate'
```

This sends the alert event to incident.io immediately without waiting.

## Override the Alert Source per Alert

You can route specific alerts to a different incident.io alert source using the `incidentio_other_alert_source` query parameter instead of exposing config IDs directly. The value must match a key under `other_alert_source_config_ids`:

```yaml
receivers:
- name: 'versus-incidentio-infra'
  webhook_configs:
  - url: 'http://versus-service:3000/api/incidents?incidentio_other_alert_source=infra'
    send_resolved: false

route:
  routes:
  - match:
      team: infrastructure
    receiver: 'versus-incidentio-infra'
```

This routes infrastructure team alerts to the alert source named `infra`, which is mapped in your configuration file:

```yaml
oncall:
  provider: incident_io
  incident_io:
    api_key: ${INCIDENTIO_API_KEY}
    alert_source_config_id: ${INCIDENTIO_ALERT_SOURCE_CONFIG_ID}
    other_alert_source_config_ids:
      infra: ${INCIDENTIO_OTHER_ALERT_SOURCE_CONFIG_ID_INFRA}
      app: ${INCIDENTIO_OTHER_ALERT_SOURCE_CONFIG_ID_APP}
      db: ${INCIDENTIO_OTHER_ALERT_SOURCE_CONFIG_ID_DB}
```

The override applies to that single request only; the global config is never mutated.

## Testing the Integration

Let's test the complete workflow:

1. **Trigger an Alert**:
   - Simulate a critical alert in Prometheus to match the Alert Manager rule.

2. **Verify Versus**:
   - Check that Versus receives the alert and sends it to Slack.
   - You should see a message with an acknowledgment link.

3. **Check Escalation**:
   - Option 1: Click the ACK link to acknowledge the incident — incident.io should not receive an alert event.
   - Option 2: Wait for the acknowledgment timeout (e.g., 5 minutes) without clicking the link.
   - In incident.io, confirm an alert was created from your Versus alert source and that the escalation path paged the on-call engineer.

4. **Immediate Trigger Test**:
   - Send an urgent alert and confirm an alert event reaches incident.io instantly.

## How It Works Under the Hood

When Versus integrates with incident.io, the following occurs:

1. Versus receives an alert from Alert Manager.
2. If on-call is enabled and the acknowledgment period passes without an ACK, Versus:
   - Builds an alert event payload with `title`, `deduplication_key`, `status`, and `metadata`.
   - Sends an authenticated `POST` to the HTTP alert events endpoint using a Bearer token.
3. incident.io ingests the alert event, applies your alert routes, and pages the on-call engineer through the attached escalation path. The `deduplication_key` ensures repeated escalations collapse onto the same alert.

## Conclusion

You've now integrated Versus Incident with incident.io for on-call management. Alerts from Prometheus Alert Manager can page your on-call engineers through incident.io via Versus when they go unacknowledged.

This integration provides:

+ A delay period for engineers to acknowledge incidents before paging
+ Slack notifications with one-click acknowledgment
+ De-duplication through the `deduplication_key` field
+ Per-alert routing to different incident.io alert sources

For the full configuration reference, see [incident.io](./incident-io.md). Adjust the configuration as needed for your environment and incident response processes.
