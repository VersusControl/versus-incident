# Lark

Send incidents to a Lark (Feishu) group through a **custom bot webhook**.
Supports multiple named webhooks and routing through the global proxy.

## Minimal config

```yaml
# config/config.yaml
alert:
  lark:
    enable: true
    webhook_url: ${LARK_WEBHOOK_URL}
    template_path: "config/lark_message.tmpl"
```

Enable from the environment instead of YAML with `LARK_ENABLE=true`.

## Get the webhook URL

1. In the Lark group, open **Settings → Group Bots → Add Bot → Custom Bot**.
2. Name the bot and copy the generated **Webhook URL** into
   `LARK_WEBHOOK_URL`.
3. (Optional) If you enable signature verification or keyword filtering on
   the bot, make sure your template content matches the configured keyword.

## Full reference

```yaml
lark:
  enable: false
  webhook_url: ${LARK_WEBHOOK_URL}   # required
  template_path: "config/lark_message.tmpl"
  use_proxy: false                   # route through the global proxy: block
  other_webhook_urls:                # optional: extra groups, selectable per request
    dev: ${LARK_OTHER_WEBHOOK_URL_DEV}
    prod: ${LARK_OTHER_WEBHOOK_URL_PROD}
```

## Multiple groups

Define named webhooks under `other_webhook_urls` and select one per incident
with the `lark_other_webhook_url` query parameter:

```bash
curl -X POST "http://localhost:3000/api/incidents?lark_other_webhook_url=dev" \
  -H "Content-Type: application/json" \
  -d '{ "Logs": "Build pipeline failed" }'
```

The value (`dev`) must match a key under `other_webhook_urls`; otherwise the
default `webhook_url` is used.

## Template

Rendered with Go's `text/template` from `config/lark_message.tmpl`. Lark
expects a JSON message card payload — keep the template valid JSON. Agent
detections use `config/agent_lark.tmpl` when present. See
[Template Syntax](../../webhook/template-syntax.md) for the available fields and
functions.
</content>
