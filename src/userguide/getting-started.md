## Getting Started

## Table of Contents
- [Prerequisites](#prerequisites)
- [Easy Installation with Docker](#easy-installation-with-docker)
- [Universal Alert Template Support](#universal-alert-template-support)
  - [Example: Send an Alertmanager alert](#example-send-an-alertmanager-alert)
  - [Example: Send a Sentry alert](#example-send-a-sentry-alert)
- [Development Custom Templates](#development-custom-templates)
  - [Docker](#docker)
  - [Kubernetes](#kubernetes)
- [SNS Usage](#sns-usage)
- [On-call](#on-call)

### Prerequisites

- Docker 20.10+ (optional)
- Slack workspace (for Slack notifications)

### Easy Installation with Docker

```bash
docker run -p 3000:3000 \
  -e SLACK_ENABLE=true \
  -e SLACK_TOKEN=your_token \
  -e SLACK_CHANNEL_ID=your_channel \
  ghcr.io/versuscontrol/versus-incident
```

Versus listens on port 3000 by default and exposes the `/api/incidents` endpoint, which you can configure as a webhook URL in your monitoring tools. This endpoint accepts JSON payloads from various monitoring systems and forwards the alerts to your configured notification channels.

### Universal Alert Template Support

Our default template automatically handles alerts from multiple sources, including:
- Alertmanager (Prometheus)
- Grafana Alerts
- Sentry
- CloudWatch SNS
- FluentBit

#### Example: Send an Alertmanager alert

```bash
curl -X POST "http://localhost:3000/api/incidents" \
  -H "Content-Type: application/json" \
  -d '{
    "receiver": "webhook-incident",
    "status": "firing",
    "alerts": [ 
      {                
        "status": "firing",                         
        "labels": {                                   
          "alertname": "PostgresqlDown",
          "instance": "postgresql-prod-01",
          "severity": "critical"
        },                        
        "annotations": {                                                
          "summary": "Postgresql down (instance postgresql-prod-01)",
          "description": "Postgresql instance is down."
        },                                     
        "startsAt": "2023-10-01T12:34:56.789Z",                         
        "endsAt": "2023-10-01T12:44:56.789Z",                
        "generatorURL": ""
      }                                  
    ],                                 
    "groupLabels": {                                                                  
      "alertname": "PostgresqlDown"     
    },                                                                    
    "commonLabels": {                                                           
      "alertname": "PostgresqlDown",                                                       
      "severity": "critical",
      "instance": "postgresql-prod-01"
    },  
    "commonAnnotations": {                                                                                  
      "summary": "Postgresql down (instance postgresql-prod-01)",
      "description": "Postgresql instance is down."
    },            
    "externalURL": ""            
  }'
```

#### Example: Send a Sentry alert

```bash
curl -X POST "http://localhost:3000/api/incidents" \
  -H "Content-Type: application/json" \
  -d '{
    "action": "created",
    "data": {
      "issue": {
        "id": "123456",
        "title": "Example Issue",
        "culprit": "example_function in example_module",
        "shortId": "PROJECT-1",
        "project": {
          "id": "1",
          "name": "Example Project",
          "slug": "example-project"
        },
        "metadata": {
          "type": "ExampleError",
          "value": "This is an example error"
        },
        "status": "unresolved",
        "level": "error",
        "firstSeen": "2023-10-01T12:00:00Z",
        "lastSeen": "2023-10-01T12:05:00Z",
        "count": 5,
        "userCount": 3
      }
    },
    "installation": {
      "uuid": "installation-uuid"
    },
    "actor": {
      "type": "user",
      "id": "789",
      "name": "John Doe"
    }
  }'
```

**Result:**

![Versus Result](/docs/images/versus-result-01.png)

## Development Custom Templates

### Docker

Create a configuration file:

```
mkdir -p ./config && touch config.yaml
```

`config.yaml`:
```yaml
name: versus
host: 0.0.0.0
port: 3000

alert:
  slack:
    enable: true
    token: ${SLACK_TOKEN}
    channel_id: ${SLACK_CHANNEL_ID}
    template_path: "/app/config/slack_message.tmpl"

  telegram:
    enable: false

  msteams:
    enable: false
```

**Configuration Notes**

Ensure `template_path` in `config.yaml` matches container path:
```yaml
alert:
  slack:
    template_path: "/app/config/slack_message.tmpl" # For containerized env
```

**Slack Template**

Create your Slack message template, for example `config/slack_message.tmpl`:

```
🔥 *Critical Error in {{.ServiceName}}*

❌ Error Details:
```{{.Logs}}```

Owner <@{{.UserID}}> please investigate
```

**Run with volume mount:**

```bash
docker run -d \
  -p 3000:3000 \
  -v $(pwd)/config:/app/config \
  -e SLACK_ENABLE=true \
  -e SLACK_TOKEN=your_slack_token \
  -e SLACK_CHANNEL_ID=your_channel_id \
  --name versus \
  ghcr.io/versuscontrol/versus-incident
```

To test, simply send an incident to Versus:

```bash
curl -X POST http://localhost:3000/api/incidents \
  -H "Content-Type: application/json" \
  -d '{
    "Logs": "[ERROR] This is an error log from User Service that we can obtain using Fluent Bit.",
    "ServiceName": "order-service",
    "UserID": "SLACK_USER_ID"
  }'
```

Response:

```json
{
    "status":"Incident created"
}
```

**Result:**

![Versus Result](/docs/images/versus-result-02.png)

#### Other Templates

**Telegram Template**

For Telegram, you can use HTML formatting. Create your Telegram message template, for example `config/telegram_message.tmpl`:
```
🚨 <b>Critical Error Detected!</b> 🚨
📌 <b>Service:</b> {{.ServiceName}}
⚠️ <b>Error Details:</b>
{{.Logs}}
```

This template will be parsed with HTML tags when sending the alert to Telegram.

**Email Template**

Create your email message template, for example `config/email_message.tmpl`:

```
Subject: Critical Error Alert - {{.ServiceName}}

Critical Error Detected in {{.ServiceName}}
----------------------------------------

Error Details:
{{.Logs}}

Please investigate this issue immediately.

Best regards,
Versus Incident Management System
```

This template supports both plain text and HTML formatting for email notifications.

**Microsoft Teams Template**

Create your Teams message template, for example `config/msteams_message.tmpl`:

```
**Critical Error in {{.ServiceName}}**
 
**Error Details:**

```{{.Logs}}```

Please investigate immediately
```

### Kubernetes

1. Create a secret for Slack:
```bash
# Create secret
kubectl create secret generic versus-secrets \
  --from-literal=slack_token=$SLACK_TOKEN \
  --from-literal=slack_channel_id=$SLACK_CHANNEL_ID
```

2. Create ConfigMap for config and template file, for example `versus-config.yaml`:
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: versus-config
data:
  config.yaml: |
    name: versus
    host: 0.0.0.0
    port: 3000

    alert:
      slack:
        enable: true
        token: ${SLACK_TOKEN}
        channel_id: ${SLACK_CHANNEL_ID}
        template_path: "/app/config/slack_message.tmpl"

      telegram:
        enable: false

  slack_message.tmpl: |
    *Critical Error in {{.ServiceName}}*
    ----------
    Error Details:
    ```
    {{.Logs}}
    ```
    ----------
    Owner <@{{.UserID}}> please investigate

```

```bash
kubectl apply -f versus-config.yaml
```

3. Create `versus-deployment.yaml`:
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: versus-incident
spec:
  replicas: 2
  selector:
    matchLabels:
      app: versus-incident
  template:
    metadata:
      labels:
        app: versus-incident
    spec:
      containers:
      - name: versus-incident
        image: ghcr.io/versuscontrol/versus-incident
        ports:
        - containerPort: 3000
        livenessProbe:
          httpGet:
            path: /healthz
            port: 3000
        env:
          - name: SLACK_CHANNEL_ID
            valueFrom:
              secretKeyRef:
                name: versus-secrets
                key: slack_channel_id
          - name: SLACK_TOKEN
            valueFrom:
              secretKeyRef:
                name: versus-secrets
                key: slack_token
        volumeMounts:
        - name: versus-config
          mountPath: /app/config/config.yaml
          subPath: config.yaml
        - name: versus-config
          mountPath: /app/config/slack_message.tmpl
          subPath: slack_message.tmpl
      volumes:
      - name: versus-config
        configMap:
          name: versus-config

---
apiVersion: v1
kind: Service
metadata:
  name: versus-service
spec:
  selector:
    app: versus
  ports:
    - protocol: TCP
      port: 3000
      targetPort: 3000
```

4. Apply:
```bash
kubectl apply -f versus-deployment.yaml
```

## SNS Usage
```bash
docker run -d \
  -p 3000:3000 \
  -e SLACK_ENABLE=true \
  -e SLACK_TOKEN=your_slack_token \
  -e SLACK_CHANNEL_ID=your_channel_id \
  -e SNS_ENABLE=true \
  -e SNS_TOPIC_ARN=$SNS_TOPIC_ARN \
  -e SNS_HTTPS_ENDPOINT_SUBSCRIPTION=https://your-domain.com \
  -e AWS_ACCESS_KEY_ID=$AWS_ACCESS_KEY \
  -e AWS_SECRET_ACCESS_KEY=$AWS_SECRET_KEY \
  --name versus \
  ghcr.io/versuscontrol/versus-incident
```

Send test message using AWS CLI:
```
aws sns publish \
  --topic-arn $SNS_TOPIC_ARN \
  --message '{"ServiceName":"test-service","Logs":"[ERROR] Test error","UserID":"U12345"}' \
  --region $AWS_REGION
```

**A key real-world application of Amazon SNS** involves integrating it with CloudWatch Alarms. This allows CloudWatch to publish messages to an SNS topic when an alarm state changes (e.g., from OK to ALARM), which can then trigger notifications to Slack, Telegram, or Email via Versus Incident with a custom template.

### On-call

Currently, Versus support On-call integrations with AWS Incident Manager. Updated configuration example with on-call features:

```yaml
name: versus
host: 0.0.0.0
port: 3000
public_host: https://your-ack-host.example # Required for on-call ack

# ... existing alert configurations ...

oncall:
  ### Enable overriding using query parameters
  # /api/incidents?oncall_enable=false => Set to `true` or `false` to enable or disable on-call for a specific alert
  # /api/incidents?oncall_wait_minutes=0 => Set the number of minutes to wait for acknowledgment before triggering on-call. Set to `0` to trigger immediately
  enable: false
  wait_minutes: 3 # If you set it to 0, it means there’s no need to check for an acknowledgment, and the on-call will trigger immediately

  aws_incident_manager:
    response_plan_arn: ${AWS_INCIDENT_MANAGER_RESPONSE_PLAN_ARN}

redis: # Required for on-call functionality
  insecure_skip_verify: true # dev only
  host: ${REDIS_HOST}
  port: ${REDIS_PORT}
  password: ${REDIS_PASSWORD}
  db: 0
```

**Explanation:**

The `oncall` section includes:
1. `enable`: A boolean to toggle on-call functionality (default: `false`).
2. `wait_minutes`: Time in minutes to wait for an acknowledgment before escalating (default: `3`). Setting it to `0` triggers the on-call immediately.
3. `aws_incident_manager`: Contains the `response_plan_arn`, which links to an AWS Incident Manager response plan via an environment variable.

The `redis` section is required when `oncall.enable` is `true`. It configures the Redis instance used for state management or queuing, with settings like `host`, `port`, `password`, and `db`.

For detailed information on integration, please refer to the document here: [On-call setup with Versus](https://versuscontrol.github.io/versus-incident/on-call-introduction.html).
