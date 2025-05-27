# Versus Incident Helm Chart

This Helm chart deploys Versus Incident, a robust incident management tool that supports alerting across multiple channels with easy custom messaging and on-call integrations.

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

```yaml
# values.yaml
replicaCount: 2

# Proxy configuration (for networks that block messaging services)
proxy:
  url: "http://proxy.example.com:8080"
  username: "proxy-user"     # optional
  password: "proxy-pass"     # optional

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
    botToken: "your-telegram-bot-token"
    chatId: "your-chat-id"
    useProxy: false 
  
  viber:
    enable: false
    botToken: "your-viber-token"
    channelId: "your-channel-id"
    useProxy: false
  
  lark:
    enable: false
    webhookUrl: "your-lark-webhook-url"
    useProxy: false
  
  email:
    enable: false
  
  msteams:
    enable: false
```

### Important Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `replicaCount` | Number of replicas for the deployment | `2` |
| `config.publicHost` | Public URL for acknowledgment links | `""` |
| `alert.slack.enable` | Enable Slack notifications | `false` |
| `alert.slack.token` | Slack bot token | `""` |
| `alert.slack.channelId` | Slack channel ID | `""` |
| `alert.telegram.enable` | Enable Telegram notifications | `false` |
| `alert.email.enable` | Enable email notifications | `false` |
| `alert.msteams.enable` | Enable Microsoft Teams notifications | `false` |
| `alert.lark.enable` | Enable Lark notifications | `false` |
| `alert.viber.enable` | Enable Viber notifications | `false` |
| `alert.viber.apiType` | Viber API type ("channel" or "bot") | `"channel"` |
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

### Viber

Viber supports two types of API integrations:

- **Channel API** (default): Send messages to Viber channels for team notifications  
- **Bot API**: Send messages to individual users for personal notifications

**Recommended Configuration (Channel API):**
```yaml
alert:
  viber:
    enable: true
    botToken: "your-viber-channel-token"  # Token for channel or bot
    apiType: "channel"  # Default: "channel" (or "bot" for individual messaging)
    channelId: "your-viber-channel-id"  # Required for Channel API
    templatePath: "/app/config/viber_message.tmpl"
```

**Alternative Configuration (Bot API):**
```yaml
alert:
  viber:
    enable: true
    botToken: "your-viber-bot-token"
    apiType: "bot"  # For individual user notifications
    userId: "your-viber-user-id"  # Required for Bot API
    templatePath: "/app/config/viber_message.tmpl"
```

**When to use each API type:**

- **Channel API** ✅ Better for incident management, team notifications, easier setup
- **Bot API** ⚠️ Limited to individual users, requires user interaction first

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

## Ingress Configuration

The Helm chart supports configuring an Ingress resource for external access:

```yaml
ingress:
  enabled: true
  className: "nginx"  # Specify your ingress controller class
  annotations:
    kubernetes.io/ingress.class: nginx
    kubernetes.io/tls-acme: "true"
    # Add any other annotations needed
  hosts:
    - host: versus-incident.example.com
      paths:
        - path: /
          pathType: Prefix
  tls:
    - secretName: versus-incident-tls
      hosts:
        - versus-incident.example.com
```

When enabling Ingress, make sure to also set the `config.publicHost` value to match your host for proper acknowledgement URL creation:

```yaml
config:
  publicHost: "https://versus-incident.example.com"
```

## Horizontal Pod Autoscaler Configuration

The Helm chart supports configuring a Horizontal Pod Autoscaler (HPA) to automatically scale the number of pods based on CPU and memory utilization:

```yaml
autoscaling:
  enabled: true
  minReplicas: 2
  maxReplicas: 10
  targetCPUUtilizationPercentage: 80
  targetMemoryUtilizationPercentage: 80
```

For more advanced scaling behavior, you can use the `behavior` configuration:

```yaml
autoscaling:
  enabled: true
  minReplicas: 2
  maxReplicas: 10
  targetCPUUtilizationPercentage: 80
  targetMemoryUtilizationPercentage: 80
  behavior:
    scaleDown:
      stabilizationWindowSeconds: 300
      policies:
      - type: Percent
        value: 100
        periodSeconds: 15
    scaleUp:
      stabilizationWindowSeconds: 0
      policies:
      - type: Percent
        value: 100
        periodSeconds: 15
      - type: Pods
        value: 4
        periodSeconds: 15
      selectPolicy: Max
```

Note: When enabling autoscaling, the `replicaCount` value in your values.yaml is only used for the initial deployment before the HPA takes over scaling control.

## Uninstalling the Chart

To uninstall/delete the `versus-incident` deployment:

```bash
helm uninstall versus-incident
```

## Additional Resources

- [Versus Incident Documentation](https://github.com/versuscontrol/versus-incident)
- [Template Syntax Guide](https://versuscontrol.github.io/versus-incident/userguide/template-syntax.html)
- [Configuration Reference](https://versuscontrol.github.io/versus-incident/userguide/configuration.html)