## How to Customize Alert Messages from Alertmanager to Slack and Telegram

## Table of Contents
- [Configure Alertmanager Webhook](#configure-alertmanager-webhook)
- [Launch Versus with Slack/Telegram](#launch-versus-with-slacktelegram)
- [Test](#test)
- [Advanced: Dynamic Channel Routing](#advanced-dynamic-channel-routing)
- [Troubleshooting Tips](#troubleshooting-tips)

![Diagram](/docs/images/alertmanager.png)

In this guide, you'll learn how to route Prometheus Alertmanager alerts to Slack and Telegram using the Versus Incident, while fully customizing alert messages.

### Configure Alertmanager Webhook

Update your `alertmanager.yml` to forward alerts to Versus:

```yaml
route:
  receiver: 'versus-incident'
  group_wait: 10s

receivers:
- name: 'versus-incident'
  webhook_configs:
  - url: 'http://versus-host:3000/api/incidents' # Versus API endpoint
    send_resolved: false
    # Additional settings (if needed):
    # http_config:
    #   tls_config:
    #     insecure_skip_verify: true  # For self-signed certificates
```

For example, alert rules:

```yaml
groups:
  - name: cluster
    rules:
      - alert: PostgresqlDown
        expr: pg_up == 0
        for: 0m
        labels:
            severity: critical
        annotations:
            summary: Postgresql down (instance {{ $labels.instance }})
            description: "Postgresql instance is down."
```

Alertmanager sends alerts to the webhook in JSON format. Here’s an example of the payload:

```json
{
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
}
```

Next, we will deploy Versus Incident and configure it with a custom template to send alerts to both Slack and Telegram for this payload.

### Launch Versus with Slack/Telegram

Create a configuration file `config/config.yaml`:

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
    enable: true
    bot_token: ${TELEGRAM_BOT_TOKEN}
    chat_id: ${TELEGRAM_CHAT_ID}
    template_path: "/app/config/telegram_message.tmpl"
```

Create Slack and Telegram templates.

`config/slack_message.tmpl`:
```
🔥 *{{ .commonLabels.severity | upper }} Alert: {{ .commonLabels.alertname }}*

🌐 *Instance*: `{{ .commonLabels.instance }}`  
🚨 *Status*: `{{ .status }}`

{{ range .alerts }}
📝 {{ .annotations.description }}  
⏰ *Firing since*: {{ .startsAt | formatTime }}
{{ end }}

🔗 *Dashboard*: <{{ .externalURL }}|Investigate>
```

`telegram_message.tmpl`:
```
🚩 <b>{{ .commonLabels.alertname }}</b>

{{ range .alerts }}
🕒 {{ .startsAt | formatTime }}
{{ .annotations.summary }}
{{ end }}

<pre>
Status: {{ .status }}
Severity: {{ .commonLabels.severity }}
</pre>
```

Run Versus:

```bash
docker run -d -p 3000:3000 \
  -e SLACK_ENABLE=true \
  -e SLACK_TOKEN=xoxb-your-token \
  -e SLACK_CHANNEL_ID=C12345 \
  -e TELEGRAM_ENABLE=true \
  -e TELEGRAM_BOT_TOKEN=123:ABC \
  -e TELEGRAM_CHAT_ID=-456789 \
  -v ./config:/app/config \
  ghcr.io/versuscontrol/versus-incident
```

### Test

Trigger a test alert using `curl`:

```bash
curl -X POST http://localhost:3000/api/incidents \
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

Final Result:

![Slack Alert](/docs/images/versus-result-02.png)

### Advanced: Dynamic Channel Routing
Override Slack channels per alert using query parameters:

```bash
POST http://versus-host:3000/api/incidents?slack_channel_id=EMERGENCY-CHANNEL
```

### Troubleshooting Tips
1. Enable debug mode: `DEBUG_BODY=true`
2. Check Versus logs: `docker logs versus`

If you encounter any issues or have further questions, feel free to reach out!
