# Slack

Post incidents to a Slack channel with an optional **Acknowledge** button
that calls back into Versus to silence on-call escalation.

## Minimal config

```yaml
# config/config.yaml
alert:
  slack:
    enable: true
    token: ${SLACK_TOKEN}            # Bot User OAuth token (xoxb-…)
    channel_id: ${SLACK_CHANNEL_ID}  # e.g. C0123456789
    template_path: "config/slack_message.tmpl"
```

Enable from the environment instead of YAML with `SLACK_ENABLE=true`.

## Get the credentials

1. Create a Slack app at <https://api.slack.com/apps> → **From scratch**.
2. Under **OAuth & Permissions**, add the `chat:write` bot scope (add
   `chat:write.public` if you post to channels the bot hasn't joined).
3. **Install to Workspace** and copy the **Bot User OAuth Token**
   (`xoxb-…`) into `SLACK_TOKEN`.
4. Invite the bot to the target channel and copy its **Channel ID** (the
   `C…` value in the channel details) into `SLACK_CHANNEL_ID`.

## Full reference

```yaml
slack:
  enable: false
  token: ${SLACK_TOKEN}
  channel_id: ${SLACK_CHANNEL_ID}
  template_path: "config/slack_message.tmpl"
  message_properties:
    button_text: "Acknowledge Alert"  # label on the ack button
    button_style: "primary"           # "primary" (blue), "danger" (red), or "" (gray)
    disable_button: false             # true = no ack button
```

## The Acknowledge button

When on-call is enabled, Versus appends an **Acknowledge** button to the
Slack message. Clicking it hits `GET /api/ack/:incidentID`, marks the
incident acknowledged, and stops escalation. The button needs
`public_host` set so Slack can reach your instance. Set
`disable_button: true` if you acknowledge incidents elsewhere.

## Per-request override

Route a single incident to a different channel:

```bash
curl -X POST "http://localhost:3000/api/incidents?slack_channel_id=C0123ONCALL" \
  -H "Content-Type: application/json" \
  -d '{ "Logs": "PostgreSQL down" }'
```

## Template

Rendered with Go's `text/template` from `config/slack_message.tmpl`. Agent
detections use `config/agent_slack.tmpl` when present. See
[Template Syntax](../../webhook/template-syntax.md) for the available fields and
functions.
</content>
