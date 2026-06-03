# Microsoft Teams

Post incidents to a Microsoft Teams channel through a **Power Automate**
HTTP trigger flow. Versus sends an Adaptive Card payload that Power
Automate forwards into the channel.

## Minimal config

```yaml
# config/config.yaml
alert:
  msteams:
    enable: true
    power_automate_url: ${MSTEAMS_POWER_AUTOMATE_URL}
    template_path: "config/msteams_message.tmpl"
```

Enable from the environment instead of YAML with `MSTEAMS_ENABLE=true`.

## Get the Power Automate URL

1. In **Power Automate**, create a flow with the trigger **When a Teams
   webhook request is received** (or **When an HTTP request is received**).
2. Add the action **Post card in a chat or channel** → *Post as Flow bot*
   → choose your Team and channel.
3. Save the flow and copy the generated **HTTP POST URL** into
   `MSTEAMS_POWER_AUTOMATE_URL`.

> Microsoft is retiring legacy Office 365 connector webhooks, so Versus
> targets Power Automate flows rather than the old `webhook.office.com`
> connector URLs.

## Full reference

```yaml
msteams:
  enable: false
  power_automate_url: ${MSTEAMS_POWER_AUTOMATE_URL}  # required
  template_path: "config/msteams_message.tmpl"
  other_power_urls:            # optional: extra channels, selectable per request
    qc: ${MSTEAMS_OTHER_POWER_URL_QC}
    ops: ${MSTEAMS_OTHER_POWER_URL_OPS}
    dev: ${MSTEAMS_OTHER_POWER_URL_DEV}
```

## Multiple channels

Define named URLs under `other_power_urls` and pick one per incident with
the `msteams_other_power_url` query parameter:

```bash
curl -X POST "http://localhost:3000/api/incidents?msteams_other_power_url=ops" \
  -H "Content-Type: application/json" \
  -d '{ "Logs": "Disk almost full" }'
```

The value (`ops`) must match a key under `other_power_urls`; otherwise the
default `power_automate_url` is used.

## Template

Rendered with Go's `text/template` from `config/msteams_message.tmpl`. Agent
detections use `config/agent_msteams.tmpl` when present. See
[Template Syntax](../../webhook/template-syntax.md) for the available fields and
functions.
</content>
