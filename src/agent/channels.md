# Notification Channels

Every incident Versus raises — whether the [AI SRE Agent](./agent-introduction.md)
detected it or a [webhook](../webhook/getting-started.md) delivered it — is fanned
out to the **channels** you enable. Each channel is independent: turn on as
many as you like, give each its own message template, and override the
destination per request when you need to.

| Channel | Enable flag | Template engine | Best for |
|---|---|---|---|
| [Slack](./channels/slack.md) | `SLACK_ENABLE` | `text/template` | Team channels with an Acknowledge button |
| [Microsoft Teams](./channels/msteams.md) | `MSTEAMS_ENABLE` | `text/template` | Teams channels via Power Automate |
| [Telegram](./channels/telegram.md) | `TELEGRAM_ENABLE` | `text/template` | Group chats & bots, proxy-friendly |
| [Viber](./channels/viber.md) | `VIBER_ENABLE` | `text/template` | Viber channels or 1:1 bot messages |
| [Email](./channels/email.md) | `EMAIL_ENABLE` | `html/template` | SMTP inboxes, rich HTML formatting |
| [Lark](./channels/lark.md) | `LARK_ENABLE` | `text/template` | Lark / Feishu groups via webhook |

## How channels are configured

Channels live under the `alert:` block in `config/config.yaml`. Each one
follows the same shape — an `enable` flag, the credentials it needs, and a
`template_path` pointing at the message template:

```yaml
alert:
  slack:
    enable: true
    token: ${SLACK_TOKEN}
    channel_id: ${SLACK_CHANNEL_ID}
    template_path: "config/slack_message.tmpl"
```

Every `enable` flag is also overridable from the environment (for example
`SLACK_ENABLE=true`), so you can flip channels on per deployment without
editing YAML. See the [Configuration reference](../configuration/configuration.md) for the
full file.

## One incident, your templates

Versus renders a separate message per channel from that channel's template,
so the same incident can read one way in Slack and another in Email. All
chat channels use Go's `text/template`; Email uses `html/template`. The
template functions available everywhere are documented in
[Template Syntax](../webhook/template-syntax.md).

> **Agent incidents get their own templates.** When the AI SRE agent raises
> an incident, Versus automatically uses the `config/agent_<channel>.tmpl`
> variant if present, so detections can carry agent-specific context
> (pattern, service, confidence) without changing your webhook templates.

## Per-request overrides

Any caller can redirect a single incident to a different destination using
query parameters on `POST /api/incidents` — handy for routing by team or
environment:

```bash
curl -X POST "http://localhost:3000/api/incidents?slack_channel_id=C0123ONCALL" \
  -H "Content-Type: application/json" \
  -d '{ "Logs": "Service down" }'
```

Supported overrides include `slack_channel_id`, `telegram_chat_id`,
`viber_user_id`, `viber_channel_id`, `email_to`, `email_subject`,
`msteams_other_power_url`, and `lark_other_webhook_url`. Each channel page
lists the ones it accepts.
</content>
