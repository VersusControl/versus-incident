# Viber

Send incidents to a Viber **channel** (recommended for incident management)
or to individual users through a **bot**. Pick the mode with `api_type`.

## Minimal config

```yaml
# config/config.yaml
alert:
  viber:
    enable: true
    api_type: channel              # "channel" (default) or "bot"
    bot_token: ${VIBER_BOT_TOKEN}
    channel_id: ${VIBER_CHANNEL_ID}
    template_path: "config/viber_message.tmpl"
```

Enable from the environment instead of YAML with `VIBER_ENABLE=true`.

## Two modes

| `api_type` | Sends to | Requires |
|---|---|---|
| `channel` (default) | a Viber channel — best for incident feeds | `bot_token`, `channel_id` |
| `bot` | a single user (1:1) | `bot_token`, `user_id` |

Get a token by creating a Viber bot/channel in the Viber admin panel and
copying its authentication token into `VIBER_BOT_TOKEN`.

## Full reference

```yaml
viber:
  enable: false
  api_type: ${VIBER_API_TYPE}      # "channel" or "bot"
  bot_token: ${VIBER_BOT_TOKEN}
  # Channel API (recommended)
  channel_id: ${VIBER_CHANNEL_ID}  # required when api_type: channel
  # Bot API (1:1 messages)
  user_id: ${VIBER_USER_ID}        # required when api_type: bot
  template_path: "config/viber_message.tmpl"
  use_proxy: false                 # route through the global proxy: block
```

## Per-request override

Redirect a single incident to a different destination:

```bash
# Channel mode
curl -X POST "http://localhost:3000/api/incidents?viber_channel_id=01234..." \
  -H "Content-Type: application/json" -d '{ "Logs": "Queue backed up" }'

# Bot mode
curl -X POST "http://localhost:3000/api/incidents?viber_user_id=abcd..." \
  -H "Content-Type: application/json" -d '{ "Logs": "Queue backed up" }'
```

## Template

Rendered with Go's `text/template` from `config/viber_message.tmpl`. Agent
detections use `config/agent_viber.tmpl` when present. See
[Template Syntax](../../webhook/template-syntax.md) for the available fields and
functions.
</content>
