# Installing Versus Incident with Helm

This guide explains how to deploy Versus Incident using Helm, a package manager for Kubernetes.

## Requirements

- Kubernetes 1.19+
- Helm 3.2.0+
- PV provisioner support in the underlying infrastructure (if persistence is required for Redis)

## Installing the Chart

You can install the Versus Incident Helm chart using OCI registry:

```bash
helm install versus-incident oci://ghcr.io/versuscontrol/charts/versus-incident
```

### Install with Custom Values

```bash
# Install with custom configuration from a values file
helm install \
  versus-incident \
  oci://ghcr.io/versuscontrol/charts/versus-incident \
  -f values.yaml
```

### Upgrading an Existing Installation

```bash
# Upgrade an existing installation with the latest version
helm upgrade \
  versus-incident \
  oci://ghcr.io/versuscontrol/charts/versus-incident

# Upgrade with custom values
helm upgrade \
  versus-incident \
  oci://ghcr.io/versuscontrol/charts/versus-incident \
  -f values.yaml
```

## Configuration

### Quick Start Example

Here's a simple example of a custom values file:

```yaml
# values.yaml
replicaCount: 2

alert:
  slack:
    enable: true
    token: "xoxb-your-slack-token"
    channelId: "C12345"
    messageProperties:
      buttonText: "Acknowledge Alert"
      buttonStyle: "primary"
  
  telegram:
    enable: false
  
  email:
    enable: false
  
  msteams:
    enable: false
  
  lark:
    enable: false
```

### Important Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `replicaCount` | Number of replicas for the deployment (set to `1` when `agent.enable=true` or persistence is enabled) | `2` |
| `config.publicHost` | Public URL for acknowledgment links | `""` |
| `gatewaySecret` | Shared secret for `/api/admin/*` and `/api/agent/*`. Empty value leaves admin routes unregistered. | `""` |
| `storage.type` | Storage backend (only `file` is implemented today) | `"file"` |
| `storage.file.dataDir` | Directory for incidents, pattern catalog, detect log | `"/app/data"` |
| `storage.persistence.enabled` | Mount a PVC at `storage.file.dataDir` | `false` |
| `agent.enable` | Enable the AI SRE Agent | `false` |
| `agent.mode` | `training`, `shadow`, or `detect` | `"training"` |
| `agent.ai.enable` | Forward unknown / spike patterns to the LLM | `false` |
| `agent.ai.apiKey` | OpenAI API key (stored in the chart Secret) | `""` |
| `alert.slack.enable` | Enable Slack notifications | `false` |
| `alert.slack.token` | Slack bot token | `""` |
| `alert.slack.channelId` | Slack channel ID | `""` |
| `alert.telegram.enable` | Enable Telegram notifications | `false` |
| `alert.email.enable` | Enable email notifications | `false` |
| `alert.msteams.enable` | Enable Microsoft Teams notifications | `false` |
| `alert.lark.enable` | Enable Lark notifications | `false` |
| `oncall.enable` | Enable on-call functionality | `false` |
| `oncall.provider` | On-call provider ("aws_incident_manager" or "pagerduty") | `"aws_incident_manager"` |
| `redis.enabled` | Enable bundled Redis (required for on-call) | `false` |

## Notification Channel Configuration

### Slack

```yaml
alert:
  slack:
    enable: true
    token: "xoxb-your-slack-token"
    channelId: "C12345"
    messageProperties:
      buttonText: "Acknowledge Alert"
      buttonStyle: "primary" # "primary" (blue), "danger" (red), or empty (default gray)
      disableButton: false
```

### Telegram

```yaml
alert:
  telegram:
    enable: true
    botToken: "your-telegram-bot-token"
    chatId: "your-telegram-chat-id"
```

### Email

```yaml
alert:
  email:
    enable: true
    smtpHost: "smtp.example.com"
    smtpPort: 587
    username: "your-email@example.com"
    password: "your-password"
    to: "alerts@example.com"
    subject: "Incident Alert"
```

### Microsoft Teams

```yaml
alert:
  msteams:
    enable: true
    powerAutomateUrl: "your-power-automate-flow-url"
    otherPowerUrls:
      dev: "dev-team-power-automate-url"
      ops: "ops-team-power-automate-url"
```

### Lark

```yaml
alert:
  lark:
    enable: true
    webhookUrl: "your-lark-webhook-url"
    otherWebhookUrls:
      dev: "dev-team-webhook-url"
      prod: "prod-team-webhook-url"
```

## On-Call Configurations

### AWS Incident Manager

```yaml
oncall:
  enable: true
  waitMinutes: 3
  provider: "aws_incident_manager"
  
  awsIncidentManager:
    responsePlanArn: "arn:aws:ssm-incidents::111122223333:response-plan/YourPlan"
    otherResponsePlanArns:
      prod: "arn:aws:ssm-incidents::111122223333:response-plan/ProdPlan"
      dev: "arn:aws:ssm-incidents::111122223333:response-plan/DevPlan"

redis:
  enabled: true
  auth:
    enabled: true
    password: "your-redis-password"
  architecture: standalone
  master:
    persistence:
      enabled: true
      size: 8Gi
```

### PagerDuty

```yaml
oncall:
  enable: true
  waitMinutes: 5
  provider: "pagerduty"
  
  pagerduty:
    routingKey: "your-pagerduty-routing-key"
    otherRoutingKeys:
      infra: "infrastructure-team-routing-key"
      app: "application-team-routing-key"
      db: "database-team-routing-key"

redis:
  enabled: true
  auth:
    enabled: true
    password: "your-redis-password"
  architecture: standalone
  master:
    persistence:
      enabled: true
      size: 8Gi
```

## Redis Configuration

Redis is required for on-call functionality. The chart can either deploy its own Redis instance or connect to an external one.

### External Redis

```yaml
redis:
  enabled: false

externalRedis:
  host: "redis.example.com"
  port: 6379
  password: "your-redis-password"
  insecureSkipVerify: false
  db: 0
```

## Custom Alert Templates

You can provide custom templates for each notification channel:

```yaml
templates:
  slack: |
    *Critical Error in {{.ServiceName}}*
    ----------
    Error Details:
    ```
    {{.Logs}}
    ```
    ----------
    Owner <@{{.UserID}}> please investigate

  telegram: |
    🚨 <b>Critical Error Detected!</b> 🚨
    📌 <b>Service:</b> {{.ServiceName}}
    ⚠️ <b>Error Details:</b>
    {{.Logs}}
```

## AWS Integrations

Versus Incident can receive alerts from aws sns systems:

### AWS SNS

```yaml
alert:
  sns:
    enable: true
    httpsEndpointSubscriptionPath: "/sns"
```

## Uninstalling the Chart

To uninstall/delete the `versus-incident` deployment:

```bash
helm uninstall versus-incident
```

## Admin Dashboard & Storage

The embedded admin dashboard (see [Admin Dashboard](./admin-ui.md)) and
the persistent incident store are first-class chart values from
v1.4.0+.

```yaml
# Required for the dashboard and every /api/admin/* and /api/agent/*
# endpoint. When empty the admin routes are not registered at all
# (no silent open surface). Generate with `openssl rand -hex 32`.
gatewaySecret: "my-strong-secret"

storage:
  type: file                  # only `file` is implemented today
  file:
    dataDir: /app/data        # holds incidents.json, patterns.json, etc.
    maxIncidents: 1000        # rolling cap

  # Persist the data dir so incident history and the agent catalog
  # survive pod restarts. When disabled an emptyDir is used.
  persistence:
    enabled: true
    size: 2Gi
    accessMode: ReadWriteOnce
    storageClassName: ""      # "" → cluster default
    # existingClaim: my-pvc   # bind to an existing PVC instead
```

> ⚠️ **Single-writer.** The file storage backend writes JSON files
> directly to disk, and the AI agent worker is single-writer to the
> pattern catalog and detect log. When you enable persistence or the
> agent, set `replicaCount: 1` and `autoscaling.enabled: false`. The
> chart's pre-flight validation will refuse to render if you violate
> this.

## AI SRE Agent

The chart can deploy the agent introduced in
[AI Detect Mode](../agent/ai-detect-mode.md). It is fully opt-in:
when `agent.enable: false` (the default) no extra resources are
created and no AI calls are made.

### Minimum config — training mode

Run the agent in observe-only mode against a log file mounted into
the pod:

```yaml
replicaCount: 1                     # required while agent.enable=true
gatewaySecret: "my-strong-secret"

storage:
  type: file
  file:
    dataDir: /app/data
  persistence:
    enabled: true
    size: 2Gi

agent:
  enable: true
  mode: training                    # observe + build catalog only
  pollInterval: 30s
  newServiceGrace: 30m
  sources:
    - name: app-logs
      type: file
      enable: true
      file:
        path: /var/log/app.log
        from_beginning: false
```

Inspect the catalog after a few minutes:

```bash
kubectl exec -it deploy/versus-incident -- \
  curl -H "X-Gateway-Secret: my-strong-secret" \
       http://localhost:3000/api/agent/patterns
```

### Detect mode (forward unknowns to the LLM)

Switch `mode: detect` and enable the AI analyzer. The API key is
written to the chart Secret and exposed as `AGENT_AI_API_KEY`:

```yaml
agent:
  enable: true
  mode: detect
  ai:
    enable: true
    apiKey: "${OPENAI_API_KEY}"     # use --set or external secret in prod
    model: "gpt-4o-mini"
    temperature: 0.2
    maxTokens: 512
    maxCallsPerHour: 30             # 0 = unlimited
    cacheTtl: "1h"
```

Install with the secret on the command line so it never lands in a
checked-in `values.yaml`:

```bash
helm upgrade --install versus-incident \
  oci://ghcr.io/versuscontrol/charts/versus-incident \
  --version 1.4.1 \
  -f values.yaml \
  --set gatewaySecret="$(openssl rand -hex 32)" \
  --set agent.ai.apiKey="$OPENAI_API_KEY"
```

Every AI call is recorded in the detect log
(`/app/data/detect.json`, capped at 500 events) and viewable via
the API or the UI:

```bash
curl -H "X-Gateway-Secret: $SECRET" \
     http://versus-incident.local/api/agent/detect/stats
```

### Mounting log files into the pod

The file source needs the log file accessible inside the container.
Common patterns:

| Source | How to mount |
|--------|--------------|
| App in the same pod | sidecar emits to a shared `emptyDir`, agent reads it |
| Node logs (e.g. journald) | `hostPath` volume + `securityContext.fsGroup` |
| Cloud log service | use the `elasticsearch` source instead of `file` |

For Elasticsearch, replace the source block:

```yaml
agent:
  sources:
    - name: prod-logs
      type: elasticsearch
      enable: true
      elasticsearch:
        addresses: ["https://es.internal:9200"]
        api_key: "${ES_API_KEY}"
        index: "logs-prod-*"
        time_field: "@timestamp"
        message_field: "message"
        page_size: 500
```

Always pass `apiKey` / credentials via `--set` or an external Secret;
inline secrets in `values.yaml` end up in `helm get values` output.

### Important agent parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `agent.enable` | Master switch (requires `replicaCount: 1`) | `false` |
| `agent.mode` | `training`, `shadow`, or `detect` | `"training"` |
| `agent.pollInterval` | How often each source is pulled | `"30s"` |
| `agent.lookback` | Initial backfill window on startup | `"5m"` |
| `agent.newServiceGrace` | Implicit training window per new service | `"30m"` |
| `agent.ai.enable` | Call the LLM (detect mode dry-runs without this) | `false` |
| `agent.ai.apiKey` | OpenAI API key | `""` |
| `agent.ai.model` | Model identifier | `"gpt-4o-mini"` |
| `agent.ai.maxCallsPerHour` | Per-hour rate limit (`0` = unlimited) | `60` |
| `agent.ai.cacheTtl` | TTL for the per-pattern AI result cache | `"1h"` |
| `agent.sources` | Inline list of signal sources | `[]` |

## Additional Resources

- [Template Syntax Guide](../userguide/template-syntax.html)
- [Configuration Reference](../userguide/configuration.html)