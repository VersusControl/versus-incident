## Configuration

## Table of Contents
- [Sample Configuration File](#sample-configuration-file)
- [Environment Variables](#environment-variables)
  - [Common](#common)
  - [Slack Configuration](#slack-configuration)
  - [Telegram Configuration](#telegram-configuration)
  - [Email Configuration](#email-configuration)
  - [Microsoft Teams Configuration](#microsoft-teams-configuration)
  - [Lark Configuration](#lark-configuration)
  - [Queue Services Configuration](#queue-services-configuration)
  - [On-Call Configuration](#on-call-configuration)
  - [Redis Configuration](#redis-configuration)
- [Dynamic Configuration with Query Parameters](#dynamic-configuration-with-query-parameters)
  - [Examples for Each Query Parameter](#examples-for-each-query-parameter)
  - [Combining Multiple Parameters](#combining-multiple-parameters)

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
    message_properties:
      button_text: "Acknowledge Alert" # Custom text for the acknowledgment button
      button_style: "primary" # Button style: "primary" (default blue), "danger" (red), or empty for default gray
      disable_button: false # Set to true to disable the button, if you want to handle acknowledgment differently

  telegram:
    enable: false  # Default value, will be overridden by TELEGRAM_ENABLE env var
    bot_token: ${TELEGRAM_BOT_TOKEN} # From environment
    chat_id: ${TELEGRAM_CHAT_ID} # From environment
    template_path: "config/telegram_message.tmpl"

  viber:
    enable: false  # Default value, will be overridden by VIBER_ENABLE env var
    api_type: ${VIBER_API_TYPE} # From environment - "channel" (default) or "bot"
    bot_token: ${VIBER_BOT_TOKEN} # From environment (token for bot or channel)
    # Channel API (recommended for incident management)
    channel_id: ${VIBER_CHANNEL_ID} # From environment (required for channel API)
    # Bot API (for individual user notifications)
    user_id: ${VIBER_USER_ID} # From environment (required for bot API)
    template_path: "config/viber_message.tmpl"

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
    power_automate_url: ${MSTEAMS_POWER_AUTOMATE_URL} # Power Automate HTTP trigger URL (required)
    template_path: "config/msteams_message.tmpl"
    other_power_urls: # Optional: Define additional Power Automate URLs for multiple MS Teams channels
      qc: ${MSTEAMS_OTHER_POWER_URL_QC} # Power Automate URL for QC team
      ops: ${MSTEAMS_OTHER_POWER_URL_OPS} # Power Automate URL for Ops team
      dev: ${MSTEAMS_OTHER_POWER_URL_DEV} # Power Automate URL for Dev team
      
  lark:
    enable: false # Default value, will be overridden by LARK_ENABLE env var
    webhook_url: ${LARK_WEBHOOK_URL} # Lark webhook URL (required)
    template_path: "config/lark_message.tmpl"
    other_webhook_urls: # Optional: Enable overriding the default webhook URL using query parameters, eg /api/incidents?lark_other_webhook_url=dev
      dev: ${LARK_OTHER_WEBHOOK_URL_DEV}
      prod: ${LARK_OTHER_WEBHOOK_URL_PROD}

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
    
  # AWS SQS
  sqs:
    enable: false
    queue_url: ${SQS_QUEUE_URL}
    
  # GCP Pub Sub
  pubsub:
    enable: false
    
  # Azure Event Bus
  azbus:
    enable: false

oncall:
  ### Enable overriding using query parameters
  # /api/incidents?oncall_enable=false => Set to `true` or `false` to enable or disable on-call for a specific alert
  # /api/incidents?oncall_wait_minutes=0 => Set the number of minutes to wait for acknowledgment before triggering on-call. Set to `0` to trigger immediately
  initialized_only: true  # Initialize on-call feature but don't enable by default; use query param oncall_enable=true to enable for specific requests
  enable: false # Use this to enable or disable on-call for all alerts
  wait_minutes: 3 # If you set it to 0, it means there's no need to check for an acknowledgment, and the on-call will trigger immediately
  provider: aws_incident_manager # Valid values: "aws_incident_manager" or "pagerduty"

  aws_incident_manager: # Used when provider is "aws_incident_manager"
    response_plan_arn: ${AWS_INCIDENT_MANAGER_RESPONSE_PLAN_ARN}
    other_response_plan_arns: # Optional: Enable overriding the default response plan ARN using query parameters, eg /api/incidents?awsim_other_response_plan=prod
      prod: ${AWS_INCIDENT_MANAGER_OTHER_RESPONSE_PLAN_ARN_PROD}
      dev: ${AWS_INCIDENT_MANAGER_OTHER_RESPONSE_PLAN_ARN_DEV}
      staging: ${AWS_INCIDENT_MANAGER_OTHER_RESPONSE_PLAN_ARN_STAGING}

  pagerduty: # Used when provider is "pagerduty"
    routing_key: ${PAGERDUTY_ROUTING_KEY} # Integration/Routing key for Events API v2 (REQUIRED)
    other_routing_keys: # Optional: Enable overriding the default routing key using query parameters, eg /api/incidents?pagerduty_other_routing_key=infra
      infra: ${PAGERDUTY_OTHER_ROUTING_KEY_INFRA}
      app: ${PAGERDUTY_OTHER_ROUTING_KEY_APP}
      db: ${PAGERDUTY_OTHER_ROUTING_KEY_DB}

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
| `SLACK_CHANNEL_ID` | The ID of the Slack channel where alerts will be sent. **Can be overridden per request using the `slack_channel_id` query parameter.** |

Slack also supports interactive acknowledgment buttons that can be configured using the following properties in the `config.yaml` file:

```yaml
alert:
  slack:
    # ...other slack configuration...
    message_properties:
      button_text: "Acknowledge Alert" # Custom text for the acknowledgment button
      button_style: "primary" # Button style: "primary" (default blue), "danger" (red), or empty for default gray
      disable_button: false # Set to true to disable the button, if you want to handle acknowledgment differently
```

These properties allow you to:
- Customize the text of the acknowledgment button (`button_text`)
- Change the style of the button (`button_style`) - options are "primary" (blue), "danger" (red), or leave empty for default gray
- Disable the interactive button entirely (`disable_button`) if you want to handle acknowledgment through other means

### Telegram Configuration
| Variable              | Description |
|----------------------|-------------|
| `TELEGRAM_ENABLE`    | Set to `true` to enable Telegram notifications. |
| `TELEGRAM_BOT_TOKEN` | The authentication token for your Telegram bot. |
| `TELEGRAM_CHAT_ID`   | The chat ID where alerts will be sent. **Can be overridden per request using the `telegram_chat_id` query parameter.** |

### Viber Configuration

Viber supports two types of API integrations:
- **Channel API** (default): Send messages to Viber channels for team notifications
- **Bot API**: Send messages to individual users for personal notifications

**When to use Channel API:**
- ✅ Broadcasting to team channels
- ✅ Public incident notifications
- ✅ Automated system alerts
- ✅ Better for most incident management scenarios
- ✅ No individual user setup required

**When to use Bot API:**
- ✅ Personal notifications to specific users
- ✅ Direct messaging for individual alerts
- ⚠️ Limited to individual users only
- ⚠️ Requires users to interact with bot first
- ⚠️ User IDs can be hard to obtain

| Variable            | Description |
|--------------------|-------------|
| `VIBER_ENABLE`     | Set to `true` to enable Viber notifications. |
| `VIBER_BOT_TOKEN`  | The authentication token for your Viber bot or channel. |
| `VIBER_API_TYPE`   | API type: `"channel"` (default) for team notifications or `"bot"` for individual messaging. |
| `VIBER_CHANNEL_ID` | The channel ID where alerts will be posted (required for channel API). **Can be overridden per request using the `viber_channel_id` query parameter.** |
| `VIBER_USER_ID`    | The user ID where alerts will be sent (required for bot API). **Can be overridden per request using the `viber_user_id` query parameter.** |

### Email Configuration
| Variable          | Description |
|------------------|-------------|
| `EMAIL_ENABLE`   | Set to `true` to enable email notifications. |
| `SMTP_HOST`      | The SMTP server hostname (e.g., smtp.gmail.com). |
| `SMTP_PORT`      | The SMTP server port (e.g., 587 for TLS). |
| `SMTP_USERNAME`  | The username/email for SMTP authentication. |
| `SMTP_PASSWORD`  | The password or app-specific password for SMTP authentication. |
| `EMAIL_TO`       | The recipient email address(es) for incident notifications. Can be multiple addresses separated by commas. **Can be overridden per request using the `email_to` query parameter.** |
| `EMAIL_SUBJECT`  | The subject line for email notifications. **Can be overridden per request using the `email_subject` query parameter.** |

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

| Variable          | Description |
|------------------|-------------|
| `MSTEAMS_ENABLE`   | Set to `true` to enable Microsoft Teams notifications. |
| `MSTEAMS_POWER_AUTOMATE_URL` | The Power Automate HTTP trigger URL for your Teams channel. Automatically works with both Power Automate workflow URLs and legacy Office 365 webhooks. |
| `MSTEAMS_OTHER_POWER_URL_QC`  | (Optional) Power Automate URL for the QC team channel. **Can be selected per request using the `msteams_other_power_url=qc` query parameter.** |
| `MSTEAMS_OTHER_POWER_URL_OPS` | (Optional) Power Automate URL for the Ops team channel. **Can be selected per request using the `msteams_other_power_url=ops` query parameter.** |
| `MSTEAMS_OTHER_POWER_URL_DEV` | (Optional) Power Automate URL for the Dev team channel. **Can be selected per request using the `msteams_other_power_url=dev` query parameter.** |

### Lark Configuration
| Variable                     | Description |
|-----------------------------|-------------|
| `LARK_ENABLE`             | Set to `true` to enable Lark notifications. |
| `LARK_WEBHOOK_URL`        | The webhook URL for your Lark channel. |
| `LARK_OTHER_WEBHOOK_URL_DEV` | (Optional) Webhook URL for the development team. **Can be selected per request using the `lark_other_webhook_url=dev` query parameter.** |
| `LARK_OTHER_WEBHOOK_URL_PROD` | (Optional) Webhook URL for the production team. **Can be selected per request using the `lark_other_webhook_url=prod` query parameter.** |

### Queue Services Configuration
| Variable                     | Description |
|-----------------------------|-------------|
| `SNS_ENABLE`             | Set to `true` to enable receive Alert Messages from SNS. |
| `SNS_HTTPS_ENDPOINT_SUBSCRIPTION`             | This specifies the HTTPS endpoint to which SNS sends messages. When an HTTPS endpoint is configured, an SNS subscription is automatically created. If no endpoint is configured, you must create the SNS subscription manually using the CLI or AWS Console. E.g. `https://your-domain.com`. |
| `SNS_TOPIC_ARN`             | AWS ARN of the SNS topic to subscribe to. |
| `SQS_ENABLE`             | Set to `true` to enable receive Alert Messages from AWS SQS. |
| `SQS_QUEUE_URL`             | URL of the AWS SQS queue to receive messages from. |

### On-Call Configuration
| Variable                          | Description |
|----------------------------------|-------------|
| `ONCALL_ENABLE`             | Set to `true` to enable on-call functionality for all incidents by default. **Can be overridden per request using the `oncall_enable` query parameter.** |
| `ONCALL_INITIALIZED_ONLY`   | Set to `true` to initialize on-call feature but keep it disabled by default. When set to `true`, on-call is triggered only for requests that explicitly include `?oncall_enable=true` in the URL. |
| `ONCALL_WAIT_MINUTES`       | Time in minutes to wait for acknowledgment before escalating (default: 3). **Can be overridden per request using the `oncall_wait_minutes` query parameter.** |
| `ONCALL_PROVIDER`           | Specify the on-call provider to use ("aws_incident_manager" or "pagerduty"). |
| `AWS_INCIDENT_MANAGER_RESPONSE_PLAN_ARN` | The ARN of the AWS Incident Manager response plan to use for on-call escalations. Required if on-call provider is "aws_incident_manager". |
| `AWS_INCIDENT_MANAGER_OTHER_RESPONSE_PLAN_ARN_PROD` | (Optional) AWS Incident Manager response plan ARN for production environment. **Can be selected per request using the `awsim_other_response_plan=prod` query parameter.** |
| `AWS_INCIDENT_MANAGER_OTHER_RESPONSE_PLAN_ARN_DEV` | (Optional) AWS Incident Manager response plan ARN for development environment. **Can be selected per request using the `awsim_other_response_plan=dev` query parameter.** |
| `AWS_INCIDENT_MANAGER_OTHER_RESPONSE_PLAN_ARN_STAGING` | (Optional) AWS Incident Manager response plan ARN for staging environment. **Can be selected per request using the `awsim_other_response_plan=staging` query parameter.** |
| `PAGERDUTY_ROUTING_KEY`     | Integration/Routing key for PagerDuty Events API v2. Required if on-call provider is "pagerduty". |
| `PAGERDUTY_OTHER_ROUTING_KEY_INFRA` | (Optional) PagerDuty routing key for feature team. **Can be selected per request using the `pagerduty_other_routing_key=infra` query parameter.** |
| `PAGERDUTY_OTHER_ROUTING_KEY_APP`   | (Optional) PagerDuty routing key for application team. **Can be selected per request using the `pagerduty_other_routing_key=app` query parameter.** |
| `PAGERDUTY_OTHER_ROUTING_KEY_DB`    | (Optional) PagerDuty routing key for database team. **Can be selected per request using the `pagerduty_other_routing_key=db` query parameter.** |

#### Enabling On-Call for Specific Incidents with initialized_only

When you have `initialized_only: true` in your configuration (rather than `enable: true`), on-call is only triggered for incidents that explicitly request it. This is useful when:

1. You want the on-call feature ready but not active for all alerts
2. You need to selectively enable on-call only for high-priority services or incidents
3. You want to let your monitoring system decide which alerts should trigger on-call

Example configuration:

```yaml
oncall:
  enable: false
  initialized_only: true  # feature ready but not active by default
  wait_minutes: 3
  provider: aws_incident_manager
  # ... provider configuration ...
```

With this configuration, on-call is only triggered when requested via query parameter:

```bash
# This alert will send notifications but NOT trigger on-call escalation
curl -X POST "http://localhost:3000/api/incidents" \
  -H "Content-Type: application/json" \
  -d '{
    "Logs": "[WARNING] Non-critical database latency increase.",
    "ServiceName": "database-monitoring",
    "UserID": "U12345"
  }'

# This alert WILL trigger on-call escalation because of the query parameter
curl -X POST "http://localhost:3000/api/incidents?oncall_enable=true" \
  -H "Content-Type: application/json" \
  -d '{
    "Logs": "[CRITICAL] Production database is down.",
    "ServiceName": "core-database",
    "UserID": "U12345"
  }'
```

**Understanding On-Call Modes:**

| Mode | Configuration | Behavior |
|------|--------------|----------|
| Disabled | `enable: false`<br>`initialized_only: false` | On-call feature is not initialized. No on-call functionality is available. |
| Always Enabled | `enable: true` | On-call is active for all incidents by default. Can be disabled per request with `?oncall_enable=false`. |
| Opt-In Only | `enable: false`<br>`initialized_only: true` | On-call feature is initialized but inactive by default. Must be explicitly enabled per request with `?oncall_enable=true`. |

### Redis Configuration
| Variable          | Description |
|------------------|-------------|
| `REDIS_HOST`     | The hostname or IP address of the Redis server. Required if on-call is enabled. |
| `REDIS_PORT`     | The port number of the Redis server. Required if on-call is enabled. |
| `REDIS_PASSWORD` | The password for authenticating with the Redis server. Required if on-call is enabled and Redis requires authentication. |

Ensure these environment variables are properly set before running the application.

## Dynamic Configuration with Query Parameters
We provide a way to overwrite configuration values using query parameters, allowing you to send alerts to different channels and customize notification behavior on a per-request basis.

| Query Parameter          | Description |
|------------------|-------------|
| `slack_channel_id`   | The ID of the Slack channel where alerts will be sent. Use: `/api/incidents?slack_channel_id=<your_value>`. |
| `telegram_chat_id`   | The chat ID where Telegram alerts will be sent. Use: `/api/incidents?telegram_chat_id=<your_chat_id>`. |
| `viber_channel_id`   | The channel ID where Viber alerts will be posted (for Channel API). Use: `/api/incidents?viber_channel_id=<your_channel_id>`. |
| `viber_user_id`      | The user ID where Viber alerts will be sent (for Bot API). Use: `/api/incidents?viber_user_id=<your_user_id>`. |
| `email_to`   | Overrides the default recipient email address for email notifications. Use: `/api/incidents?email_to=<recipient_email>`. |
| `email_subject`   | Overrides the default subject line for email notifications. Use: `/api/incidents?email_subject=<custom_subject>`. |
| `msteams_other_power_url`   | Overrides the default Microsoft Teams Power Automate flow by specifying an alternative key (e.g., qc, ops, dev). Use: `/api/incidents?msteams_other_power_url=qc`. |
| `lark_other_webhook_url`   | Overrides the default Lark webhook URL by specifying an alternative key (e.g., dev, prod). Use: `/api/incidents?lark_other_webhook_url=dev`. |
| `oncall_enable`          | Set to `true` or `false` to enable or disable on-call for a specific alert. Use: `/api/incidents?oncall_enable=false`. |
| `oncall_wait_minutes`    | Set the number of minutes to wait for acknowledgment before triggering on-call. Set to `0` to trigger immediately. Use: `/api/incidents?oncall_wait_minutes=0`. |
| `awsim_other_response_plan` | Overrides the default AWS Incident Manager response plan ARN by specifying an alternative key (e.g., prod, dev, staging). Use: `/api/incidents?awsim_other_response_plan=prod`. |
| `pagerduty_other_routing_key` | Overrides the default PagerDuty routing key by specifying an alternative key (e.g., infra, app, db). Use: `/api/incidents?pagerduty_other_routing_key=infra`. |

### Examples for Each Query Parameter

#### Slack Channel Override

To send an alert to a specific Slack channel (e.g., a dedicated channel for database issues):

```bash
curl -X POST "http://localhost:3000/api/incidents?slack_channel_id=C01DB2ISSUES" \
  -H "Content-Type: application/json" \
  -d '{
    "Logs": "[ERROR] Database connection pool exhausted.",
    "ServiceName": "database-service",
    "UserID": "U12345"
  }'
```

#### Telegram Chat Override

To send an alert to a different Telegram chat (e.g., for network monitoring):

```bash
curl -X POST "http://localhost:3000/api/incidents?telegram_chat_id=-1001234567890" \
  -H "Content-Type: application/json" \
  -d '{
    "Logs": "[ERROR] Network latency exceeding thresholds.",
    "ServiceName": "network-monitor",
    "UserID": "U12345"
  }'
```

#### Viber Channel Override

To send an alert to a specific Viber channel (recommended for team notifications):

```bash
curl -X POST "http://localhost:3000/api/incidents?viber_channel_id=01234567890A=" \
  -H "Content-Type: application/json" \
  -d '{
    "Logs": "[ERROR] Mobile service experiencing high error rates.",
    "ServiceName": "mobile-api",
    "UserID": "U12345"
  }'
```

#### Viber User Override

To send an alert to a specific Viber user (for individual notifications):

```bash
curl -X POST "http://localhost:3000/api/incidents?viber_user_id=01234567890A=" \
  -H "Content-Type: application/json" \
  -d '{
    "Logs": "[ERROR] Personal alert for mobile service issue.",
    "ServiceName": "mobile-api",
    "UserID": "U12345"
  }'
```

#### Email Recipient Override

To send an email alert to a specific recipient with a custom subject:

```bash
curl -X POST "http://localhost:3000/api/incidents?email_to=network-team@yourdomain.com&email_subject=Urgent%20Network%20Issue" \
  -H "Content-Type: application/json" \
  -d '{
    "Logs": "[ERROR] Load balancer failing health checks.",
    "ServiceName": "load-balancer",
    "UserID": "U12345"
  }'
```

#### Microsoft Teams Channel Override

You can configure multiple Microsoft Teams channels using the `other_power_urls` setting:

```yaml
alert:
  msteams:
    enable: true
    power_automate_url: ${MSTEAMS_POWER_AUTOMATE_URL}
    template_path: "config/msteams_message.tmpl"
    other_power_urls:
      qc: ${MSTEAMS_OTHER_POWER_URL_QC}
      ops: ${MSTEAMS_OTHER_POWER_URL_OPS}
      dev: ${MSTEAMS_OTHER_POWER_URL_DEV}
```

Then, to send an alert to the QC team's Microsoft Teams channel:

```bash
curl -X POST "http://localhost:3000/api/incidents?msteams_other_power_url=qc" \
  -H "Content-Type: application/json" \
  -d '{
    "Logs": "[ERROR] Quality check failed for latest deployment.",
    "ServiceName": "quality-service",
    "UserID": "U12345"
  }'
```

#### Lark Webhook Override

You can configure multiple Lark webhook URLs using the `other_webhook_urls` setting:

```yaml
alert:
  lark:
    enable: true
    webhook_url: ${LARK_WEBHOOK_URL}
    template_path: "config/lark_message.tmpl"
    other_webhook_urls:
      dev: ${LARK_OTHER_WEBHOOK_URL_DEV}
      prod: ${LARK_OTHER_WEBHOOK_URL_PROD}
```

Then, to send an alert to the development team's Lark channel:

```bash
curl -X POST "http://localhost:3000/api/incidents?lark_other_webhook_url=dev" \
  -H "Content-Type: application/json" \
  -d '{
    "Logs": "[ERROR] Development server crash detected.",
    "ServiceName": "dev-server",
    "UserID": "U12345"
  }'
```

#### On-Call Controls

To disable on-call escalation for a non-critical alert:

```bash
curl -X POST "http://localhost:3000/api/incidents?oncall_enable=false" \
  -H "Content-Type: application/json" \
  -d '{
    "Logs": "[WARNING] This is a minor issue that doesn't require on-call response.",
    "ServiceName": "monitoring-service",
    "UserID": "U12345"
  }'
```

To trigger on-call immediately without the normal wait period for a critical issue:

```bash
curl -X POST "http://localhost:3000/api/incidents?oncall_wait_minutes=0" \
  -H "Content-Type: application/json" \
  -d '{
    "Logs": "[CRITICAL] Payment processing system down.",
    "ServiceName": "payment-service",
    "UserID": "U12345"
  }'
```

#### AWS Incident Manager Response Plan Override

You can configure multiple AWS Incident Manager response plans using the `other_response_plan_arns` setting:

```yaml
oncall:
  enable: true
  wait_minutes: 3
  provider: aws_incident_manager
  
  aws_incident_manager:
    response_plan_arn: ${AWS_INCIDENT_MANAGER_RESPONSE_PLAN_ARN}  # Default response plan
    other_response_plan_arns:
      prod: ${AWS_INCIDENT_MANAGER_OTHER_RESPONSE_PLAN_ARN_PROD}  # Production environment
      dev: ${AWS_INCIDENT_MANAGER_OTHER_RESPONSE_PLAN_ARN_DEV}    # Development environment
      staging: ${AWS_INCIDENT_MANAGER_OTHER_RESPONSE_PLAN_ARN_STAGING}  # Staging environment
```

Then, to use a specific AWS Incident Manager response plan for a production environment issue:

```bash
curl -X POST "http://localhost:3000/api/incidents?awsim_other_response_plan=prod" \
  -H "Content-Type: application/json" \
  -d '{
    "Logs": "[CRITICAL] Production database cluster failure.",
    "ServiceName": "prod-database",
    "UserID": "U12345"
  }'
```

#### PagerDuty Routing Key Override

You can configure multiple PagerDuty routing keys using the `other_routing_keys` setting:

```yaml
oncall:
  enable: true
  wait_minutes: 3
  provider: pagerduty
  
  pagerduty:
    routing_key: ${PAGERDUTY_ROUTING_KEY}  # Default routing key
    other_routing_keys:
      infra: ${PAGERDUTY_OTHER_ROUTING_KEY_INFRA}  # Infrastructure team
      app: ${PAGERDUTY_OTHER_ROUTING_KEY_APP}      # Application team
      db: ${PAGERDUTY_OTHER_ROUTING_KEY_DB}        # Database team
```

Then, to use a specific PagerDuty routing key for the infrastructure team:

```bash
curl -X POST "http://localhost:3000/api/incidents?pagerduty_other_routing_key=infra" \
  -H "Content-Type: application/json" \
  -d '{
    "Logs": "[ERROR] Server load balancer failure in us-west-2.",
    "ServiceName": "infrastructure",
    "UserID": "U12345"
  }'
```

### Combining Multiple Parameters

You can combine multiple query parameters to customize exactly how an incident is handled:

```bash
curl -X POST "http://localhost:3000/api/incidents?slack_channel_id=C01PROD&telegram_chat_id=-987654321&oncall_enable=true&oncall_wait_minutes=1" \
  -H "Content-Type: application/json" \
  -d '{
    "Logs": "[CRITICAL] Multiple service failures detected in production environment.",
    "ServiceName": "core-infrastructure",
    "UserID": "U12345",
    "Severity": "CRITICAL"
  }'
```

This will:
1. Send the alert to a specific Slack channel (`C01PROD`)
2. Send the alert to a specific Telegram chat (`-987654321`)
3. Enable on-call escalation with a shortened 1-minute wait time
