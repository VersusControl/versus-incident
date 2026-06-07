# Integrate with ServiceNow

This document provides a step-by-step guide to integrate Versus Incident with ServiceNow for on-call management. The integration escalates unacknowledged alerts into ServiceNow by creating records through the ServiceNow [Table API](https://docs.servicenow.com/bundle/washingtondc-api-reference/page/integrate/inbound-rest/concept/c_TableAPI.html).

We'll cover setting up ServiceNow, configuring the integration with Versus, deploying Versus, and testing the integration with practical examples.

## Prerequisites

Before you begin, ensure you have:
+ A ServiceNow instance (a free [Developer instance](https://developer.servicenow.com/) works for testing)
+ Versus Incident deployed (instructions provided later)
+ Prometheus Alert Manager set up to monitor your systems

## Setting Up ServiceNow for On-Call

Let's configure a practical example in ServiceNow with a dedicated integration user and the target table.

### 1. Create an Integration User

First, create a user that Versus will authenticate as when creating records:

1. Log in to your ServiceNow instance as an administrator
2. Navigate to **User Administration** > **Users** > **New**
3. Fill in the user details:
   + User ID (e.g., `versus.integration`)
   + First/Last name (e.g., "Versus Integration")
   + Set a strong password
   + Check **Web service access only** so the account cannot log in to the UI
4. Submit to create the user

### 2. Grant Table Permissions

The integration user needs permission to create records on the target table (the `incident` table by default):

1. Navigate to **User Administration** > **Roles**
2. Assign a role that allows record creation on your target table:
   + For the `incident` table, the `itil` role is the common choice
   + For a custom table, create or assign a role with `create` access
3. Open the integration user and add the role under the **Roles** tab
4. Save the user

> **Note:** Follow least-privilege. Grant only the access needed to create records on the one table Versus writes to — nothing more.

### 3. Identify Your Instance URL and Table

You need two values for the Versus configuration:

1. **Instance URL** — the base URL of your instance, e.g. `https://dev12345.service-now.com`
2. **Table** — the table records are created in. Versus defaults to `incident`. To use a different table (e.g. a custom `u_versus_alert`), note its name.

### 4. Confirm the Table API Endpoint

Versus posts records to the standard Table API path:

```
{instance_url}/api/now/table/{table}
```

For example, with the default table:

```
https://dev12345.service-now.com/api/now/table/incident
```

You can verify access from your terminal before wiring up Versus:

```bash
curl -u "versus.integration:YOUR_PASSWORD" \
  -H "Content-Type: application/json" \
  -X POST "https://dev12345.service-now.com/api/now/table/incident" \
  -d '{"short_description":"Versus connectivity test","correlation_id":"test-001"}'
```

A `201 Created` response confirms the user and permissions are correct.

## How the Mapping Works

When an alert escalates, Versus sends an HTTP `POST` to the Table API with HTTP Basic auth and maps the incident onto two fields:

| ServiceNow field | Value |
|------------------|-------|
| `short_description` | A human-readable summary (`Incident <id>`) |
| `correlation_id` | The incident ID, used by ServiceNow to de-duplicate inbound events |

The request uses the shared HTTPS client with TLS verification enabled. A non-2xx response is treated as a failure and logged. Credentials are never written to logs.

## Deploy Versus Incident

Now let's deploy Versus with ServiceNow integration. You can use Docker or Kubernetes.

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
  provider: servicenow

  servicenow:
    instance_url: ${SERVICENOW_INSTANCE_URL} # e.g. https://dev12345.service-now.com (REQUIRED)
    username: ${SERVICENOW_USERNAME}          # REQUIRED
    password: ${SERVICENOW_PASSWORD}          # REQUIRED
    table: incident                           # ServiceNow table; defaults to "incident"
    other_instance_urls:                      # Optional: per-request instance override
      infra: ${SERVICENOW_OTHER_INSTANCE_URL_INFRA}
      app: ${SERVICENOW_OTHER_INSTANCE_URL_APP}
      db: ${SERVICENOW_OTHER_INSTANCE_URL_DB}

redis: # Required for on-call functionality
  insecure_skip_verify: true # dev only
  host: ${REDIS_HOST}
  port: ${REDIS_PORT}
  password: ${REDIS_PASSWORD}
  db: 0
```

By default, Versus adds an interactive acknowledgment button to Slack notifications when on-call is enabled, using the unified template shipped in `config/slack_message.tmpl`. If the alert is acknowledged before `wait_minutes` elapses, ServiceNow is never called.

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
      - SERVICENOW_INSTANCE_URL=https://dev12345.service-now.com
      - SERVICENOW_USERNAME=versus.integration
      - SERVICENOW_PASSWORD=your_servicenow_password
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

> **Warning:** Always provide `username` and `password` through environment variables. Never commit credentials to `config.yaml`.

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

This waits 5 minutes for acknowledgment before creating a ServiceNow record if the user doesn't click the ACK link in Slack.

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

This creates a ServiceNow record immediately without waiting.

## Override the ServiceNow Instance per Alert

You can route specific alerts to a different ServiceNow instance using the `servicenow_other_instance` query parameter instead of exposing instance URLs directly. The value must match a key under `other_instance_urls`:

```yaml
receivers:
- name: 'versus-servicenow-infra'
  webhook_configs:
  - url: 'http://versus-service:3000/api/incidents?servicenow_other_instance=infra'
    send_resolved: false

route:
  routes:
  - match:
      team: infrastructure
    receiver: 'versus-servicenow-infra'
```

This routes infrastructure team alerts to the instance named `infra`, which is mapped in your configuration file:

```yaml
oncall:
  provider: servicenow
  servicenow:
    instance_url: ${SERVICENOW_INSTANCE_URL}
    username: ${SERVICENOW_USERNAME}
    password: ${SERVICENOW_PASSWORD}
    other_instance_urls:
      infra: ${SERVICENOW_OTHER_INSTANCE_URL_INFRA}
      app: ${SERVICENOW_OTHER_INSTANCE_URL_APP}
      db: ${SERVICENOW_OTHER_INSTANCE_URL_DB}
```

The override applies to that single request only; the global config is never mutated.

> **Warning:** Every URL listed under `other_instance_urls` is reached with the same default `username` and `password`. Only point overrides at instances that share the same credential trust domain.

## Testing the Integration

Let's test the complete workflow:

1. **Trigger an Alert**:
   - Simulate a critical alert in Prometheus to match the Alert Manager rule.

2. **Verify Versus**:
   - Check that Versus receives the alert and sends it to Slack.
   - You should see a message with an acknowledgment link.

3. **Check Escalation**:
   - Option 1: Click the ACK link to acknowledge the incident — ServiceNow should not receive a record.
   - Option 2: Wait for the acknowledgment timeout (e.g., 5 minutes) without clicking the link.
   - In ServiceNow, open the target table (e.g. **Incident** > **All**) and confirm a new record was created with the matching `short_description` and `correlation_id`.

4. **Immediate Trigger Test**:
   - Send an urgent alert and confirm a record is created in ServiceNow instantly.

## How It Works Under the Hood

When Versus integrates with ServiceNow, the following occurs:

1. Versus receives an alert from Alert Manager.
2. If on-call is enabled and the acknowledgment period passes without an ACK, Versus:
   - Builds a Table API payload with `short_description` and `correlation_id`.
   - Sends an authenticated `POST` to `{instance_url}/api/now/table/{table}` using HTTP Basic auth.
3. ServiceNow creates the record on the target table, where it flows into your existing assignment rules, workflows, and notifications.

## Conclusion

You've now integrated Versus Incident with ServiceNow for on-call management. Alerts from Prometheus Alert Manager can create ServiceNow records via Versus when they go unacknowledged.

This integration provides:

+ A delay period for engineers to acknowledge incidents before a record is created
+ Slack notifications with one-click acknowledgment
+ De-duplication through the `correlation_id` field
+ Per-alert routing to different ServiceNow instances

For the full configuration reference, see [ServiceNow](./servicenow.md). Adjust the configuration as needed for your environment and incident response processes.
