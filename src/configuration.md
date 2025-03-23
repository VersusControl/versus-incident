## Configuration

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

  aws_incident_manager:
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