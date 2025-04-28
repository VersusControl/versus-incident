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
    power_automate_url: ${MSTEAMS_POWER_AUTOMATE_URL} # Default Power Automate URL for MS Teams
    template_path: "config/msteams_message.tmpl"
    other_power_url: # Optional: Define additional Power Automate URLs for multiple MS Teams channels
      qc: ${MSTEAMS_OTHER_POWER_URL_QC} # Power Automate URL for QC team
      ops: ${MSTEAMS_OTHER_POWER_URL_OPS} # Power Automate URL for Ops team
      dev: ${MSTEAMS_OTHER_POWER_URL_DEV} # Power Automate URL for Dev team

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
| `EMAIL_TO`       | The recipient email address(es) for incident notifications. Can be multiple addresses separated by commas. |
| `EMAIL_SUBJECT`  | The subject line for email notifications. |

### Microsoft Teams Configuration

The Microsoft Teams integration now supports both legacy Office 365 webhooks and modern Power Automate workflows with a single configuration option:

```yaml
alert:
  msteams:
    enable: true
    power_automate_url: ${MSTEAMS_POWER_AUTOMATE_URL}
    template_path: "config/msteams_message.tmpl"
```

#### Automatic URL Detection (April 2025 Update)

As of the April 2025 update, Versus Incident automatically detects the type of URL provided in the `power_automate_url` setting:

- **Legacy Office 365 Webhook URLs**: If the URL contains "webhook.office.com" (e.g., `https://yourcompany.webhook.office.com/...`), the system will use the legacy format with a simple "text" field containing your rendered Markdown.

- **Power Automate Workflow URLs**: For newer Power Automate HTTP trigger URLs, the system converts your Markdown template to an Adaptive Card with rich formatting features.

This automatic detection provides backward compatibility while supporting newer features, eliminating the need for separate configuration options.

#### Multiple Teams Channels

You can configure multiple Microsoft Teams channels using the `other_power_url` setting:

```yaml
alert:
  msteams:
    enable: true
    power_automate_url: ${MSTEAMS_POWER_AUTOMATE_URL}
    template_path: "config/msteams_message.tmpl"
    other_power_url:
      qc: ${MSTEAMS_OTHER_POWER_URL_QC}
      ops: ${MSTEAMS_OTHER_POWER_URL_OPS}
      dev: ${MSTEAMS_OTHER_POWER_URL_DEV}
```

To send alerts to specific channels, use the `msteams_other_power_url` query parameter:

```bash
curl -X POST "http://localhost:3000/api/incidents?msteams_other_power_url=qc" \
  -H "Content-Type: application/json" \
  -d '{
    "Logs": "[ERROR] Quality check failed.",
    "ServiceName": "qa-service"
  }'
```

| Variable          | Description |
|------------------|-------------|
| `MSTEAMS_ENABLE`   | Set to `true` to enable Microsoft Teams notifications. |
| `MSTEAMS_POWER_AUTOMATE_URL` | The Power Automate HTTP trigger URL for your Teams channel. |
| `MSTEAMS_OTHER_POWER_URL_QC`  | (Optional) Power Automate URL for the QC team channel. |
| `MSTEAMS_OTHER_POWER_URL_OPS` | (Optional) Power Automate URL for the Ops team channel. |
| `MSTEAMS_OTHER_POWER_URL_DEV` | (Optional) Power Automate URL for the Dev team channel. |

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
| `email_to`   | Overrides the default recipient email address for email notifications. Use: `/api/incidents?email_to=<recipient_email>`. |
| `email_subject`   | Overrides the default subject line for email notifications. Use: `/api/incidents?email_subject=<custom_subject>`. |
| `msteams_other_power_url`   | (Optional) Overrides the default Microsoft Teams Power Automate URL by specifying an alternative key (e.g., qc, ops, dev). Use: `/api/incidents?msteams_other_power_url=qc`. |
| `oncall_enable`          | Set to `true` or `false` to enable or disable on-call for a specific alert. Use: `/api/incidents?oncall_enable=false`. |
| `oncall_wait_minutes`    | Set the number of minutes to wait for acknowledgment before triggering on-call. Set to `0` to trigger immediately. Use: `/api/incidents?oncall_wait_minutes=0`. |
| `awsim_response_plan_arn`    | Overrides the default AWS Incident Manager response plan ARN for a specific alert. Use: `/api/incidents?awsim_response_plan_arn=arn:aws:ssm-incidents::111122223333:response-plan/example-response-plan`. |

**Optional: Define additional Power Automate URLs for multiple MS Teams channels**

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
    power_automate_url: ${MSTEAMS_POWER_AUTOMATE_URL} # Default Power Automate URL for MS Teams
    template_path: "config/msteams_message.tmpl"
    other_power_url: # Optional: Define additional Power Automate URLs for multiple MS Teams channels
      qc: ${MSTEAMS_OTHER_POWER_URL_QC} # Power Automate URL for QC team
      ops: ${MSTEAMS_OTHER_POWER_URL_OPS} # Power Automate URL for Ops team
      dev: ${MSTEAMS_OTHER_POWER_URL_DEV} # Power Automate URL for Dev team
```

**Microsoft Teams Configuration**

| Variable          | Description |
|------------------|-------------|
| `MSTEAMS_POWER_AUTOMATE_URL` | The Power Automate HTTP trigger URL for your Teams channel. |
| `MSTEAMS_OTHER_POWER_URL_QC`  | (Optional) Power Automate URL for the QC team channel. |
| `MSTEAMS_OTHER_POWER_URL_OPS` | (Optional) Power Automate URL for the Ops team channel. |
| `MSTEAMS_OTHER_POWER_URL_DEV` | (Optional) Power Automate URL for the Dev team channel. |

*Notes: The `MSTEAMS_POWER_AUTOMATE_URL` is the primary Power Automate URL, while the `MSTEAMS_OTHER_POWER_URL_*` variables are optional and allow routing alerts to specific Teams channels based on the msteams_other_power_url query parameter.*

**Example MS Teams:**

To send an alert to the QC team’s Microsoft Teams channel:

```
curl -X POST http://localhost:3000/api/incidents?msteams_other_power_url=qc \
  -H "Content-Type: application/json" \
  -d '{
    "Logs": "[ERROR] Quality check failed.",
    "ServiceName": "qa-service",
    "UserID": "U12345"
  }'
```

**Example Email:**

To send an email alert to a specific recipient with a custom subject:

```bash
curl -X POST "http://localhost:3000/api/incidents?email_to=alerts@yourdomain.com&email_subject=Urgent%20Incident%20Notification" \
  -H "Content-Type: application/json" \
  -d '{
    "Logs": "[ERROR] Critical system failure.",
    "ServiceName": "core-service",
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
