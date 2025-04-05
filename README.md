<h1 align="center" style="border-bottom: none">
  <img alt="Versus" src="src/docs/images/versus.svg">
</h1>

[![Go Report Card](https://goreportcard.com/badge/github.com/yourusername/versus)](https://goreportcard.com/report/github.com/yourusername/versus)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

An incident management tool that supports alerting across multiple channels with easy custom messaging and on-call integrations. Compatible with any tool supporting webhook alerts, it’s designed for modern DevOps teams to quickly respond to production incidents.

## Table of Contents
- [Features](#features)
- [Getting Started](#get-started-in-60-seconds)
- [Development](#development)
  - [Docker](#docker)
  - [Kubernetes](#kubernetes)
- [SNS Usage](#sns-usage)
- [On-call](#on-call)
- [Configuration](#complete-configuration)
- [Environment Variables](#environment-variables)
- [Advanced API Usage](#advanced-api-usage)
- [Template Syntax](https://versuscontrol.github.io/versus-incident/template_syntax.html)
- [Template Example](https://versuscontrol.github.io/versus-incident/slack-template-aws-sns.html)
- [Roadmap](#roadmap)
- [Contributing](#contributing)
- [License](#license)
- [Support This Project](#support-this-project)

## Features

- 🚨 **Multi-channel Alerts**: Send incident notifications to Slack, Microsoft Teams, Telegram, and Email (more channels coming!)
- 📝 **Custom Templates**: Define your own alert messages using Go templates
- 🔧 **Easy Configuration**: YAML-based configuration with environment variables support
- 📡 **REST API**: Simple HTTP interface to receive alerts
- 📡 **On-call**: On-call integrations with AWS Incident Manager

![Versus](src/docs/images/versus-architecture.svg)

## Get Started in 60 Seconds

### Easy Installation with Docker

```bash
docker run -p 3000:3000 \
  -e SLACK_ENABLE=true \
  -e SLACK_TOKEN=your_token \
  -e SLACK_CHANNEL_ID=your_channel \
  ghcr.io/versuscontrol/versus-incident
```

### Or Build from source

```bash
# Clone the repository
git clone https://github.com/VersusControl/versus-incident.git
cd versus-incident

# Build with Go
go build -o versus-incident ./cmd/main.go
chmod +x versus-incident
```

Create `run.sh`:
```bash
#!/bin/bash
export SLACK_ENABLE=true
export SLACK_TOKEN=your_token
export SLACK_CHANNEL_ID=your_channel

./versus-incident
```

## Development

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
    template_path: "/app/config/slack_message.tmpl" # For containerized env

  telegram:
    enable: false

  msteams:
    enable: false
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

![Slack Alert](src/docs/images/versus-result.png)

#### More examples

1. [How to Customize Alert Messages from Alertmanager to Slack and Telegram](https://medium.com/@hmquan08011996/how-to-customize-alert-messages-from-alertmanager-to-slack-and-telegram-786525713689)
2. [Configuring Fluent Bit to Send Error Logs to Versus Incident](https://medium.com/@hmquan08011996/configuring-fluent-bit-to-send-error-logs-to-slack-and-telegram-89d11968bc30)
3. [Configuring AWS CloudWatch to Send Alerts to Slack and Telegram](https://medium.com/@hmquan08011996/configuring-aws-cloudwatch-to-send-alerts-to-slack-and-telegram-ae0b8c077fc6)
4. [How to Configure Sentry to Send Alerts to MS Teams](https://medium.com/@hmquan08011996/how-to-configure-sentry-to-send-alerts-to-ms-teams-08e0969f8578)
5. [How to Configure Kibana to Send Alerts to Slack and Telegram](https://medium.com/@hmquan08011996/how-to-configure-kibana-to-send-alerts-to-slack-and-telegram-40e882e29bb4)
6. [How to Configure Grafana to Send Alerts to Slack and Telegram](https://medium.com/@hmquan08011996/how-to-configure-grafana-to-send-alerts-to-slack-and-telegram-b11a784369b8)
7. [How to Configure OpenSearch to Send Alerts to Slack and Telegram](https://medium.com/@hmquan08011996/how-to-configure-opensearch-to-send-alerts-to-slack-and-telegram-43d177d36791)

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

## On-call

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

  aws_incident_manager: # Overrides the default AWS Incident Manager response plan ARN for a specific alert /api/incidents?awsim_response_plan_arn=arn:aws:ssm-incidents::111122223333:response-plan/example-response-plan
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

## Complete Configuration

A sample configuration file is located at `config/config.yaml`:

```yaml
name: versus
host: 0.0.0.0
port: 3000
public_host: https://your-ack-host.example # Required for on-call ack

alert:
  debug_body: true  # Default value, will be overridden by DEBUG_BODY env var

  slack:
    enable: false  # Default value, will be overridden by SLACK_ENABLE env var
    token: ${SLACK_TOKEN}            # From environment
    channel_id: ${SLACK_CHANNEL_ID}  # From environment
    template_path: "config/slack_message.tmpl"

  telegram:
    enable: false  # Default value, will be overridden by TELEGRAM_ENABLE env var
    bot_token: ${TELEGRAM_BOT_TOKEN} # From environment
    chat_id: ${TELEGRAM_CHAT_ID} # From environment
    template_path: "config/telegram_message.tmpl"

  email:
    enable: false # Default value, will be overridden by EMAIL_ENABLE env var
    smtp_host: ${SMTP_HOST} # From environment
    smtp_port: ${SMTP_PORT} # From environment
    username: ${SMTP_USERNAME} # From environment
    password: ${SMTP_PASSWORD} # From environment
    to: ${EMAIL_TO} # From environment
    subject: ${EMAIL_SUBJECT} # From environment
    template_path: "config/email_message.tmpl"

  msteams:
    enable: false # Default value, will be overridden by MSTEAMS_ENABLE env var
    webhook_url: ${MSTEAMS_WEBHOOK_URL}
    template_path: "config/msteams_message.tmpl"

queue:
  enable: true
  debug_body: true

  # AWS SNS
  sns:
    enable: false
    https_endpoint_subscription_path: /sns # URI to receive SNS messages, e.g. ${host}:${port}/sns or ${https_endpoint_subscription}/sns
    # Options If you want to automatically create an sns subscription
    https_endpoint_subscription: ${SNS_HTTPS_ENDPOINT_SUBSCRIPTION} # If the user configures an HTTPS endpoint, then an SNS subscription will be automatically created, e.g. https://your-domain.com
    topic_arn: ${SNS_TOPIC_ARN}

oncall:
  ### Enable overriding using query parameters
  # /api/incidents?oncall_enable=false => Set to `true` or `false` to enable or disable on-call for a specific alert
  # /api/incidents?oncall_wait_minutes=0 => Set the number of minutes to wait for acknowledgment before triggering on-call. Set to `0` to trigger immediately
  enable: false
  wait_minutes: 3 # If you set it to 0, it means there’s no need to check for an acknowledgment, and the on-call will trigger immediately

  aws_incident_manager: # Overrides the default AWS Incident Manager response plan ARN for a specific alert /api/incidents?awsim_response_plan_arn=arn:aws:ssm-incidents::111122223333:response-plan/example-response-plan
    response_plan_arn: ${AWS_INCIDENT_MANAGER_RESPONSE_PLAN_ARN}

redis: # Required for on-call functionality
  insecure_skip_verify: true # dev only
  host: ${REDIS_HOST}
  port: ${REDIS_PORT}
  password: ${REDIS_PASSWORD}
  db: 0
```

## Environment Variables

The application relies on several environment variables to configure alerting services. Below is an explanation of each variable:

### Common
| Variable          | Description |
|------------------|-------------|
| `DEBUG_BODY`   | Set to `true` to enable print body send to Versus Incident. |

### Slack Configuration
| Variable          | Description |
|------------------|-------------|
| `SLACK_ENABLE`   | Set to `true` to enable Slack notifications. |
| `SLACK_TOKEN`    | The authentication token for your Slack bot. |
| `SLACK_CHANNEL_ID` | The ID of the Slack channel where alerts will be sent. |

### Telegram Configuration
| Variable              | Description |
|----------------------|-------------|
| `TELEGRAM_ENABLE`    | Set to `true` to enable Telegram notifications. |
| `TELEGRAM_BOT_TOKEN` | The authentication token for your Telegram bot. |
| `TELEGRAM_CHAT_ID`   | The chat ID where alerts will be sent. |

### Email Configuration
| Variable          | Description |
|------------------|-------------|
| `EMAIL_ENABLE`   | Set to `true` to enable email notifications. |
| `SMTP_HOST`      | The SMTP server hostname (e.g., smtp.gmail.com). |
| `SMTP_PORT`      | The SMTP server port (e.g., 587 for TLS). |
| `SMTP_USERNAME`  | The username/email for SMTP authentication. |
| `SMTP_PASSWORD`  | The password or app-specific password for SMTP authentication. |
| `EMAIL_TO`       | The recipient email address for incident notifications. |
| `EMAIL_SUBJECT`  | The subject line for email notifications. |

### Microsoft Teams Configuration
| Variable          | Description |
|------------------|-------------|
| `MSTEAMS_ENABLE`   | Set to `true` to enable Microsoft Teams notifications. |
| `MSTEAMS_WEBHOOK_URL` | The incoming webhook URL for your Teams channel. |

### AWS SNS Configuration
| Variable                     | Description |
|-----------------------------|-------------|
| `SNS_ENABLE`             | Set to `true` to enable receive Alert Messages from SNS. |
| `SNS_HTTPS_ENDPOINT_SUBSCRIPTION`             | This specifies the HTTPS endpoint to which SNS sends messages. When an HTTPS endpoint is configured, an SNS subscription is automatically created. If no endpoint is configured, you must create the SNS subscription manually using the CLI or AWS Console. E.g. `https://your-domain.com`. |
| `SNS_TOPIC_ARN`             | AWS ARN of the SNS topic to subscribe to. |

### On-Call Configuration
| Variable                          | Description |
|----------------------------------|-------------|
| `ONCALL_ENABLE`             | Set to `true` to enable on-call functionality. |
| `AWS_INCIDENT_MANAGER_RESPONSE_PLAN_ARN` | The ARN of the AWS Incident Manager response plan to use for on-call escalations. Required if on-call is enabled. |

### Redis Configuration
| Variable          | Description |
|------------------|-------------|
| `REDIS_HOST`     | The hostname or IP address of the Redis server. Required if on-call is enabled. |
| `REDIS_PORT`     | The port number of the Redis server. Required if on-call is enabled. |
| `REDIS_PASSWORD` | The password for authenticating with the Redis server. Required if on-call is enabled and Redis requires authentication. |

Ensure these environment variables are properly set before running the application.

## Advanced API Usage
We provide a way to overwrite configuration values using query parameters, allowing you to send alerts to different channel IDs based on the service.

| Query          | Description |
|------------------|-------------|
| `slack_channel_id`   | The ID of the Slack channel where alerts will be sent. Use: `/api/incidents?slack_channel_id=<your_vaule>`. |
| `msteams_other_webhook_url`   | (Optional) Overrides the default Microsoft Teams channel by specifying an alternative webhook key (e.g., qc, ops, dev). Use: `/api/incidents?msteams_other_webhook_url=qc`. |
| `oncall_enable`          | Set to `true` or `false` to enable or disable on-call for a specific alert. Use: `/api/incidents?oncall_enable=false`. |
| `oncall_wait_minutes`    | Set the number of minutes to wait for acknowledgment before triggering on-call. Set to `0` to trigger immediately. Use: `/api/incidents?oncall_wait_minutes=0`. |
| `awsim_response_plan_arn`    | Overrides the default AWS Incident Manager response plan ARN for a specific alert. Use: `/api/incidents?awsim_response_plan_arn=arn:aws:ssm-incidents::111122223333:response-plan/example-response-plan`. |

**Optional: Define additional webhook URLs for multiple MS Teams channels**

```yaml
name: versus
host: 0.0.0.0
port: 3000

alert:
  debug_body: true  # Default value, will be overridden by DEBUG_BODY env var

  slack:
    enable: false  # Default value, will be overridden by SLACK_ENABLE env var

  msteams:
    enable: false # Default value, will be overridden by MSTEAMS_ENABLE env var
    webhook_url: ${MSTEAMS_WEBHOOK_URL} # Default webhook URL for MS Teams
    template_path: "config/msteams_message.tmpl"
    other_webhook_url: # Optional: Define additional webhook URLs for multiple MS Teams channels
      qc: ${MSTEAMS_OTHER_WEBHOOK_URL_QC} # Webhook for QC team
      ops: ${MSTEAMS_OTHER_WEBHOOK_URL_OPS} # Webhook for Ops team
      dev: ${MSTEAMS_OTHER_WEBHOOK_URL_DEV} # Webhook for Dev team
```

**Microsoft Teams Configuration**

| Variable          | Description |
|------------------|-------------|
| `MSTEAMS_WEBHOOK_URL` | The incoming webhook URL for your Teams channel. |
| `MSTEAMS_OTHER_WEBHOOK_URL_QC`  | (Optional) Webhook URL for the QC team channel. |
| `MSTEAMS_OTHER_WEBHOOK_URL_OPS` | (Optional) Webhook URL for the Ops team channel. |
| `MSTEAMS_OTHER_WEBHOOK_URL_DEV` | (Optional) Webhook URL for the Dev team channel. |

*Notes: The `MSTEAMS_WEBHOOK_URL` is the primary webhook URL, while the `MSTEAMS_OTHER_WEBHOOK_URL_*` variables are optional and allow routing alerts to specific Teams channels based on the msteams_other_webhook_url query parameter.*

**Example MS Teams:**

To send an alert to the QC team’s Microsoft Teams channel:

```
curl -X POST http://localhost:3000/api/incidents?msteams_other_webhook_url=qc \
  -H "Content-Type: application/json" \
  -d '{
    "Logs": "[ERROR] Quality check failed.",
    "ServiceName": "qa-service",
    "UserID": "U12345"
  }'
```

**Example On-call:**

To disable on-call for a specific alert:

```bash
curl -X POST http://localhost:3000/api/incidents?oncall_enable=false \
  -H "Content-Type: application/json" \
  -d '{
    "Logs": "[ERROR] This is a test error.",
    "ServiceName": "test-service",
    "UserID": "U12345"
  }'
```

To trigger on-call immediately without waiting:

```bash
curl -X POST http://localhost:3000/api/incidents?oncall_wait_minutes=0 \
  -H "Content-Type: application/json" \
  -d '{
    "Logs": "[ERROR] Urgent issue detected.",
    "ServiceName": "urgent-service",
    "UserID": "U12345"
  }'
```

To use a specific AWS Incident Manager response plan for an alert:

```bash
curl -X POST "http://localhost:3000/api/incidents?awsim_response_plan_arn=arn:aws:ssm-incidents::111122223333:response-plan/example-response-plan" \
  -H "Content-Type: application/json" \
  -d '{
    "Logs": "[ERROR] Critical system failure.",
    "ServiceName": "core-service",
    "UserID": "U12345"
  }'
```

## Roadmap

- [x] Add Telegram support
- [x] Add Email support
- [x] Add SNS subscription
- [x] Add MS Team support
- [ ] Add Viber support
- [ ] Add Lark support
- [ ] Add support error logs for listeners from the queue (AWS SQS, GCP Cloud Pub/Sub, Azure Service Bus)
- [x] Support multiple templates
- [ ] API Server for Incident Management
- [ ] Web UI
- [x] On-call integrations (AWS Incident Manager)
- [ ] Prometheus metrics

Complete Project Diagram

![Versus Control](src/docs/images/road-map.svg)

## Contributing

We welcome contributions! Please follow these steps:

1. Fork the repository
2. Create your feature branch (`git checkout -b feature/amazing-feature`)
3. Commit your changes (`git commit -m 'Add some amazing feature'`)
4. Push to the branch (`git push origin feature/amazing-feature`)
5. Open a Pull Request

## License

Distributed under the MIT License. See `LICENSE` for more information.

## Support This Project

Help us maintain Versus Incident! Your sponsorship:

🔧 Funds critical infrastructure

🚀 Accelerates new features like Viber/Lark integration, Web UI and On-call integrations

[![GitHub Sponsors](https://img.shields.io/github/sponsors/YourUsername?style=for-the-badge)](https://github.com/hoalongnatsu)
