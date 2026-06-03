# Telegram

Send incidents to a Telegram group, channel, or direct chat through a bot.
Supports routing through the global proxy for restricted networks.

## Minimal config

```yaml
# config/config.yaml
alert:
  telegram:
    enable: true
    bot_token: ${TELEGRAM_BOT_TOKEN}
    chat_id: ${TELEGRAM_CHAT_ID}
    template_path: "config/telegram_message.tmpl"
```

Enable from the environment instead of YAML with `TELEGRAM_ENABLE=true`.

## Get the credentials

1. Message [@BotFather](https://t.me/BotFather) → `/newbot`, follow the
   prompts, and copy the **bot token** into `TELEGRAM_BOT_TOKEN`.
2. Add the bot to your group/channel (give it permission to post).
3. Find the **chat ID**:
   - For a group, send any message, then open
     `https://api.telegram.org/bot<TOKEN>/getUpdates` and read
     `result[].message.chat.id` (group IDs are negative).
   - For a channel, use `@channelusername` or the numeric `-100…` ID.
4. Put it in `TELEGRAM_CHAT_ID`.

## Full reference

```yaml
telegram:
  enable: false
  bot_token: ${TELEGRAM_BOT_TOKEN}
  chat_id: ${TELEGRAM_CHAT_ID}
  template_path: "config/telegram_message.tmpl"
  use_proxy: false             # route through the global proxy: block
```

## Using a proxy

Set `use_proxy: true` to send Telegram traffic through the global `proxy:`
block (HTTP/HTTPS/SOCKS5) — useful where `api.telegram.org` is blocked:

```yaml
proxy:
  url: ${PROXY_URL}
  username: ${PROXY_USERNAME}
  password: ${PROXY_PASSWORD}
```

## Per-request override

Route a single incident to a different chat:

```bash
curl -X POST "http://localhost:3000/api/incidents?telegram_chat_id=-1001234567890" \
  -H "Content-Type: application/json" \
  -d '{ "Logs": "API latency spike" }'
```

## Template

Rendered with Go's `text/template` from `config/telegram_message.tmpl`.
Telegram supports a MarkdownV2/HTML subset — keep formatting simple. Agent
detections use `config/agent_telegram.tmpl` when present. See
[Template Syntax](../../webhook/template-syntax.md) for the available fields and
functions.
</content>
