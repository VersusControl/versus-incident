# Configuration

Centralised reference for `config/config.yaml`, environment variables, and per-request overrides used by Versus Incident.

A sample configuration file is located at `config/config.yaml`:

```yaml
name: versus
host: 0.0.0.0
port: 3000
public_host: https://your-ack-host.example # Required for on-call ack & dashboard links

# Shared secret required for ALL admin endpoints (`/api/admin/*` and
# `/api/agent/*`) and the embedded admin dashboard. Sent by clients
# (and the dashboard) in the `X-Gateway-Secret` header. When empty,
# admin endpoints are not registered and the agent refuses to start.
gateway_secret: ${GATEWAY_SECRET}

# Storage backend used by BOTH the agent (catalog, shadow log, services)
# and the incident service (history shown in the UI). Only `file` is
# implemented today; `redis` and `database` are config stubs.
storage:
  type: file              # file | redis | database (env: STORAGE_TYPE)
  file:
    data_dir: ./data
    max_incidents: 1000   # rolling cap on persisted incidents

# Optional global proxy applied per-channel via `use_proxy: true` below
# (Telegram, Viber, Lark). Unset to disable.
proxy:
  url: ${PROXY_URL}           # HTTP/HTTPS/SOCKS5, e.g. http://proxy.example.com:8080
  username: ${PROXY_USERNAME}
  password: ${PROXY_PASSWORD}

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
    use_proxy: false # Set to true to use the global proxy block above

  viber:
    enable: false  # Default value, will be overridden by VIBER_ENABLE env var
    api_type: ${VIBER_API_TYPE} # From environment - "channel" (default) or "bot"
    bot_token: ${VIBER_BOT_TOKEN} # From environment (token for bot or channel)
    # Channel API (recommended for incident management)
    channel_id: ${VIBER_CHANNEL_ID} # From environment (required for channel API)
    # Bot API (for individual user notifications)
    user_id: ${VIBER_USER_ID} # From environment (required for bot API)
    template_path: "config/viber_message.tmpl"
    use_proxy: false

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
    use_proxy: false
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

  # GCP Pub Sub (config stub — not yet implemented)
  pubsub:
    enable: false

  # Azure Service Bus (config stub — not yet implemented)
  azbus:
    enable: false

oncall:
  ### Enable overriding using query parameters
  # /api/incidents?oncall_enable=false => Set to `true` or `false` to enable or disable on-call for a specific alert
  # /api/incidents?oncall_wait_minutes=0 => Set the number of minutes to wait for acknowledgment before triggering on-call. Set to `0` to trigger immediately
  initialized_only: true  # Initialize on-call feature but don't enable by default; use query param oncall_enable=true to enable for specific requests
  enable: false # Use this to enable or disable on-call for all alerts
  wait_minutes: 3 # If you set it to 0, it means there's no need to check for an acknowledgment, and the on-call will trigger immediately
  provider: aws_incident_manager # Valid values: "aws_incident_manager", "pagerduty", "servicenow" or "incident_io"

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

  servicenow: # Used when provider is "servicenow"
    instance_url: ${SERVICENOW_INSTANCE_URL} # eg https://dev12345.service-now.com (REQUIRED)
    username: ${SERVICENOW_USERNAME} # REQUIRED
    password: ${SERVICENOW_PASSWORD} # REQUIRED
    table: incident # ServiceNow table to create records in; defaults to "incident"
    other_instance_urls: # Optional: Override the default instance using query parameters, eg /api/incidents?servicenow_other_instance=infra
      infra: ${SERVICENOW_OTHER_INSTANCE_URL_INFRA}
      app: ${SERVICENOW_OTHER_INSTANCE_URL_APP}
      db: ${SERVICENOW_OTHER_INSTANCE_URL_DB}

  incident_io: # Used when provider is "incident_io"
    api_key: ${INCIDENTIO_API_KEY} # Bearer API key for the HTTP alert source (REQUIRED)
    alert_source_config_id: ${INCIDENTIO_ALERT_SOURCE_CONFIG_ID} # HTTP alert source config ID (REQUIRED)
    other_alert_source_config_ids: # Optional: Override the default alert source using query parameters, eg /api/incidents?incidentio_other_alert_source=infra
      infra: ${INCIDENTIO_OTHER_ALERT_SOURCE_CONFIG_ID_INFRA}
      app: ${INCIDENTIO_OTHER_ALERT_SOURCE_CONFIG_ID_APP}
      db: ${INCIDENTIO_OTHER_ALERT_SOURCE_CONFIG_ID_DB}

redis: # Required for on-call functionality and the AI agent
  insecure_skip_verify: true # dev only
  host: ${REDIS_HOST}
  port: ${REDIS_PORT}
  password: ${REDIS_PASSWORD}
  db: 0

# -----------------------------------------------------------------------------
# AI agent (training | shadow | detect) — opt-in.
# When agent.enable=false (the default) nothing extra runs.
# Source list lives in a separate file (sources_path).
# -----------------------------------------------------------------------------
agent:
  enable: false              # master switch (env: AGENT_ENABLE)
  mode: training             # training | shadow | detect (env: AGENT_MODE)
  poll_interval: 30s         # how often each source is pulled
  lookback: 5m               # initial backfill window on startup
  batch_max: 5000            # safety cap per tick
  signal_max_bytes: 65536    # cap on Signal.Raw

  # Path to the YAML file listing log sources (resolved relative to this
  # config file). Override via env: AGENT_SOURCES_PATH.
  sources_path: ./agent_sources.yaml

  # Grace period for newly discovered services in shadow/detect mode.
  # During grace, signals are observed and clustered but never surfaced
  # as would-have-alerted (shadow) or sent to the AI analyzer (detect).
  # Set to "0" to disable.
  new_service_grace: 30m

  # Regexes used to extract a service name from each log message. The
  # first capture group of the first matching pattern wins. Empty list
  # disables service detection (everything attributed to "_unknown").
  service_patterns:
    - '(?i)\bservice[._-]?name["\s:=]+"?([A-Za-z0-9._-]+)'
    - '(?i)\b(?:service|svc|app|component)\s*=\s*"?([A-Za-z0-9._-]+)'
    - '(?i)"(?:service|svc|app|component)"\s*:\s*"([A-Za-z0-9._-]+)"'
    - '\[([A-Za-z0-9._-]+)\]'

  redaction:
    enable: true
    redact_ips: false        # IPs are usually useful context; opt-in
    extra_patterns:
      - "(?i)password=\\S+"
      - "Authorization:\\s*Bearer\\s+\\S+"

  catalog:
    persist_interval: 30s
    auto_promote_after: 50    # in detect mode, this many sightings = "known"
    # Spike detection: a known pattern is re-flagged when its tick-level
    # frequency exceeds the EWMA baseline by `spike_multiplier`.
    spike_multiplier: 5.0
    spike_min_frequency: 5
    spike_min_baseline_count: 20

  miner:
    similarity_threshold: 0.4
    tree_depth: 4
    max_children: 100

  regex:
    # Pre-filter: only signals matching at least one rule (named or
    # default) are forwarded to the miner. Set to ".*" to train on
    # every line, or leave empty to require an explicit named match.
    default_pattern: "(?i).*error.*"
    rules:
      - name: oom-killer
        pattern: "Out of memory: Killed process"
      - name: panic
        pattern: "(?i)panic:"
      - name: 5xx-burst
        pattern: "HTTP/[0-9.]+\\s+5\\d\\d"

  # AI analyzer — used in detect mode to assess unknown/spiking patterns.
  ai:
    enable: false                     # master switch (env: AGENT_AI_ENABLE)
    base_url: ${AGENT_AI_BASE_URL}    # OpenAI-compatible chat/completions endpoint
    api_key: ${AGENT_AI_API_KEY}
    model: "gpt-4o-mini"
    temperature: 0.2
    max_tokens: 512
    max_calls_per_hour: 60            # 0 = unlimited
    cache_ttl: "1h"
```

The runtime list of agent sources lives in the file referenced by
`agent.sources_path` (default `./agent_sources.yaml`):

```yaml
sources:
  - name: prod-app
    type: elasticsearch
    enable: true
    elasticsearch:
      addresses:
        - https://es.example.internal:9200
      username: ${ES_USERNAME}
      password: ${ES_PASSWORD}
      index: "logs-app-*"
      time_field: "@timestamp"
      query: 'log.level:(error OR warn)'
      message_field: message
      page_size: 500

  - name: sample-app
    type: file
    enable: true
    file:
      path: ./local/resource/sample-app.log
      format: text
      from_beginning: true
```

## Environment Variables

The application relies on several environment variables to configure alerting services. Below is an explanation of each variable:

### Common
| Variable          | Description |
|------------------|-------------|
| `DEBUG_BODY`   | Set to `true` to enable print body send to Versus Incident. |

### Admin & Gateway
| Variable          | Description |
|------------------|-------------|
| `GATEWAY_SECRET` | Shared secret required to access the admin dashboard and every `/api/admin/*` and `/api/agent/*` endpoint. Sent by clients in the `X-Gateway-Secret` header. **When unset the admin endpoints are not registered at all.** |

### Storage
| Variable                 | Description |
|--------------------------|-------------|
| `STORAGE_TYPE`           | Storage backend for incidents and agent state. One of `file` (default and the only implemented backend today), `redis`, `database`. |
| `STORAGE_FILE_DATA_DIR`  | Directory for the `file` backend. Default `./data`. Files written: `incidents.json`, `patterns.json`, `shadow.json`. |

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
| `ONCALL_PROVIDER`           | Specify the on-call provider to use ("aws_incident_manager", "pagerduty", "servicenow" or "incident_io"). |
| `AWS_INCIDENT_MANAGER_RESPONSE_PLAN_ARN` | The ARN of the AWS Incident Manager response plan to use for on-call escalations. Required if on-call provider is "aws_incident_manager". |
| `AWS_INCIDENT_MANAGER_OTHER_RESPONSE_PLAN_ARN_PROD` | (Optional) AWS Incident Manager response plan ARN for production environment. **Can be selected per request using the `awsim_other_response_plan=prod` query parameter.** |
| `AWS_INCIDENT_MANAGER_OTHER_RESPONSE_PLAN_ARN_DEV` | (Optional) AWS Incident Manager response plan ARN for development environment. **Can be selected per request using the `awsim_other_response_plan=dev` query parameter.** |
| `AWS_INCIDENT_MANAGER_OTHER_RESPONSE_PLAN_ARN_STAGING` | (Optional) AWS Incident Manager response plan ARN for staging environment. **Can be selected per request using the `awsim_other_response_plan=staging` query parameter.** |
| `PAGERDUTY_ROUTING_KEY`     | Integration/Routing key for PagerDuty Events API v2. Required if on-call provider is "pagerduty". |
| `PAGERDUTY_OTHER_ROUTING_KEY_INFRA` | (Optional) PagerDuty routing key for feature team. **Can be selected per request using the `pagerduty_other_routing_key=infra` query parameter.** |
| `PAGERDUTY_OTHER_ROUTING_KEY_APP`   | (Optional) PagerDuty routing key for application team. **Can be selected per request using the `pagerduty_other_routing_key=app` query parameter.** |
| `PAGERDUTY_OTHER_ROUTING_KEY_DB`    | (Optional) PagerDuty routing key for database team. **Can be selected per request using the `pagerduty_other_routing_key=db` query parameter.** |
| `SERVICENOW_INSTANCE_URL`   | Base URL of your ServiceNow instance (e.g. `https://dev12345.service-now.com`). Required if on-call provider is "servicenow". |
| `SERVICENOW_USERNAME`       | ServiceNow Basic auth username. Required if on-call provider is "servicenow". |
| `SERVICENOW_PASSWORD`       | ServiceNow Basic auth password. Required if on-call provider is "servicenow". |
| `SERVICENOW_OTHER_INSTANCE_URL_INFRA` | (Optional) Alternate ServiceNow instance URL. **Can be selected per request using the `servicenow_other_instance=infra` query parameter.** |
| `INCIDENTIO_API_KEY`        | Bearer API key for the incident.io HTTP alert source. Required if on-call provider is "incident_io". |
| `INCIDENTIO_ALERT_SOURCE_CONFIG_ID` | incident.io HTTP alert source config ID. Required if on-call provider is "incident_io". |
| `INCIDENTIO_OTHER_ALERT_SOURCE_CONFIG_ID_INFRA` | (Optional) Alternate incident.io alert source config ID. **Can be selected per request using the `incidentio_other_alert_source=infra` query parameter.** |

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

### AI Agent Configuration
| Variable                  | Description |
|---------------------------|-------------|
| `AGENT_ENABLE`            | Set to `true` to start the AI agent worker. When `false` (default) no agent goroutines, files, or Redis keys are created. |
| `AGENT_MODE`              | Worker mode: `training` (observe and learn only), `shadow` (classify and log "would-have-alerted" events), or `detect` (classify + emit). |
| `AGENT_SOURCES_PATH`      | Path to the YAML file listing the agent's log sources. Resolved relative to the main config file. Default `./agent_sources.yaml`. |
| `AGENT_NEW_SERVICE_GRACE` | Duration a newly discovered service stays in implicit training before detect-mode AI analysis begins (e.g. `30m`). `0` disables the grace window. |
| `AGENT_SERVICE_PATTERNS`  | Comma-separated list of regexes used to extract the service name from each log line. Each pattern must contain at least one capture group. Overrides the YAML list when set. |
| `AGENT_AI_ENABLE`         | Set to `true` to call the configured LLM in detect mode. When `false`, detect mode classifies but never calls the model (dry-run). |
| `AGENT_AI_BASE_URL`       | OpenAI-compatible chat/completions endpoint, e.g. `https://api.openai.com/v1`. |
| `AGENT_AI_API_KEY`        | Bearer token sent in the `Authorization` header when calling the LLM. |
| `AGENT_AI_MODEL`          | Model identifier, e.g. `gpt-4o-mini`. |

> The agent also requires the **root-level** `GATEWAY_SECRET` (see
> [Admin & Gateway](#admin--gateway)) and the **root-level** `redis`
> block — Redis is used to remember per-source cursors so the agent
> resumes from where it left off after a restart.

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

## SNS Listener

Versus can subscribe to an SNS topic and treat each message as an incoming
incident. This is useful for CloudWatch Alarms which can publish to SNS on
state changes.

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

Test with the AWS CLI:

```bash
aws sns publish \
  --topic-arn $SNS_TOPIC_ARN \
  --message '{"ServiceName":"test-service","Logs":"[ERROR] Test error","UserID":"U12345"}' \
  --region $AWS_REGION
```

A common real-world setup: CloudWatch Alarms → SNS topic → Versus →
Slack/Telegram/Email with a custom CloudWatch-aware template.

## AI Agent

Versus supports an opt-in **AI SRE agent** that reads your logs, metrics,
and traces, learns what normal looks like, and only alerts you when
something new and unexpected appears.

Configuration example with agent features:

```yaml
name: versus
host: 0.0.0.0
port: 3000

# ... existing alert configurations ...

# Shared secret required for ALL admin endpoints (`/api/admin/*` and
# `/api/agent/*`). Sent by clients in the `X-Gateway-Secret` header.
gateway_secret: ${GATEWAY_SECRET}

# Storage backend for the pattern catalog, shadow log, and incident
# history. Only `file` is implemented today; `redis` and `database`
# are config stubs.
storage:
  type: file              # file | redis | database (env: STORAGE_TYPE)
  file:
    data_dir: ./data
    max_incidents: 1000   # rolling cap on persisted incidents

agent:
  enable: false # Use this to enable or disable the agent for all sources
  mode: training # Valid values: "training", "shadow", or "detect"
  poll_interval: 30s

  # Sources are kept in a separate file so they can be managed independently
  # (e.g. swap fixtures, per-environment lists). Path is resolved relative to
  # this config file. Override via env: AGENT_SOURCES_PATH.
  sources_path: ./agent_sources.yaml

  catalog:
    persist_interval: 30s
    auto_promote_after: 100 # In detect mode, this many sightings = "known"

  redaction:
    enable: true
    redact_ips: false
    extra_patterns: # Optional: extra regex rules to scrub before clustering
      - "(?i)password=\\S+"
      - "Authorization:\\s*Bearer\\s+\\S+"

  miner:
    similarity_threshold: 0.4
    tree_depth: 4
    max_children: 100

  regex:
    # Optional: tag any signal whose message matches this pattern
    # if none of the named rules below hit. Leave empty to disable.
    default_pattern: "(?i)error|exception|fatal|panic"
    # Named rules are tried first, in order. The first match wins.
    rules:
      - name: oom
        pattern: "(?i)out of memory|OOMKilled|java\\.lang\\.OutOfMemoryError"
      - name: db-timeout
        pattern: "(?i)(connection|query) timeout|deadlock detected"
      - name: auth-failure
        pattern: "(?i)401 unauthorized|invalid credentials|permission denied"

redis: # Required for the agent to persist source cursors across restarts
  host: ${REDIS_HOST}
  port: ${REDIS_PORT}
  password: ${REDIS_PASSWORD}
  db: 0
```

**Explanation:**

The `agent` section includes:

1. `enable`: Turn the agent on or off (default: `false`). When disabled, nothing extra runs.
2. `mode`: How the agent behaves after it has learned your log patterns:
   - `training`: observation only — the agent learns patterns and saves them, but sends no alerts.
   - `shadow`: same as training, but also logs a note every time it would have sent an alert. Good for reviewing before going live.
   - `detect`: the agent actively sends alerts for any pattern it has never seen before.
3. `poll_interval`: How often the agent checks your log sources for new entries.
4. `catalog`: Where the agent stores the list of known patterns and how often to write updates. Storage is selected by the root `storage:` block.

> **Admin secret.** All admin endpoints (`/api/admin/*` and
> `/api/agent/*`) are protected by the **root-level** `gateway_secret`
> (env `GATEWAY_SECRET`). Set it to any value you choose; clients send
> the same value in the `X-Gateway-Secret` header. When no secret is
> configured the admin endpoints are not registered and the agent
> refuses to start.

5. `redaction`: Rules for automatically removing sensitive information (passwords, tokens, emails, etc.) from logs before the agent processes them.
6. `miner`: Controls how aggressively the agent groups similar log lines together. The defaults work well for most setups.
7. `regex`: Acts as a **pre-filter** for the agent. Only signals whose message matches at least one rule (a named entry under `rules` or `default_pattern`) are forwarded to the pattern miner and stored in the catalog.

   - Named `rules` are tried in order; the first match wins and tags the signal with that `name` (stored as `rule_name` on the pattern).
   - If no named rule hits, `default_pattern` is tried. Matches there are tagged with `name=default`.
   - **To learn from every line, set `default_pattern: ".*"`.**
   - **To filter aggressively, set `default_pattern: ""` (empty)** and rely on your named rules.

8. `sources_path`: Path to a separate YAML file that lists the log sources the agent should read from. Resolved relative to the main config file. Override via `AGENT_SOURCES_PATH`.

The sources file (default `./agent_sources.yaml`) has a single top-level `sources:` list. Each entry needs `name`, `type` (`file` or `elasticsearch`), `enable`, plus a matching `file:` or `elasticsearch:` block:

```yaml
sources:
  - name: prod-app
    type: elasticsearch
    enable: true
    elasticsearch:
      addresses:
        - https://es.example.internal:9200
      username: ${ES_USERNAME}
      password: ${ES_PASSWORD}
      index: "logs-app-*"
      time_field: "@timestamp"
      query: 'log.level:(error OR warn)'
      message_field: message
      page_size: 500

  - name: sample-app
    type: file
    enable: true
    file:
      path: ./local/resource/sample-app.log
      format: text
      from_beginning: true
```

The `redis` section is required when `agent.enable` is `true`. Redis stores the per-source cursor so the agent picks up where it left off after a restart.

For full integration walkthroughs see [Enable AI Agent](https://versuscontrol.github.io/versus-incident/agent/agent-introduction.html).

## On-Call

Versus supports On-Call integrations with **AWS Incident Manager** and
**PagerDuty**. Configuration example with on-call features:

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
  wait_minutes: 3 # If you set it to 0, on-call triggers immediately without checking for an acknowledgment

  aws_incident_manager:
    response_plan_arn: ${AWS_INCIDENT_MANAGER_RESPONSE_PLAN_ARN}

redis: # Required for on-call functionality
  insecure_skip_verify: true # dev only
  host: ${REDIS_HOST}
  port: ${REDIS_PORT}
  password: ${REDIS_PASSWORD}
  db: 0
```

The `oncall` section includes:

1. `enable`: A boolean to toggle on-call functionality for all incidents (default: `false`).
2. `initialized_only`: Initialize the on-call subsystem but keep it disabled by default. With `true`, on-call is triggered only for requests that explicitly include `?oncall_enable=true`.
3. `wait_minutes`: Time in minutes to wait for an acknowledgment before escalating (default: `3`). Set to `0` to trigger immediately.
4. `provider`: Which on-call provider to use (`"aws_incident_manager"` or `"pagerduty"`).
5. `aws_incident_manager`: Configuration for AWS Incident Manager when selected, including `response_plan_arn` and `other_response_plan_arns`.
6. `pagerduty`: Configuration for PagerDuty when selected, including `routing_key` and `other_routing_keys`.

The `redis` section is required when `oncall.enable` or `oncall.initialized_only` is `true`. It stores the open-incident state needed for ack-or-escalate.

For provider-specific walkthroughs see [On-Call setup with Versus](https://versuscontrol.github.io/versus-incident/oncall/on-call-introduction.html).
