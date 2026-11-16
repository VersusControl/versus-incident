# Advanced Template Tips

Patterns for templates that go beyond one tool and one field. Each tip shows
a template and what it renders. The functions are listed in
[Template Syntax](./template-syntax.md).

## Start from a built-in template

The image ships one template per channel in `config/`, such as
`config/slack_message.tmpl` and `config/telegram_message.tmpl`. They already
recognise Alertmanager, Grafana, Sentry, Fluent Bit and CloudWatch. Copy one
into your templates folder, point `template_path` at the copy, and change only
what you need.

The built-in templates add a runbook link to every alert when you set
`DEFAULT_RUNBOOK_URL` on the Versus container. An Alertmanager alert with a
`runbook_url` annotation uses that link instead.

## One template for several tools

All tools send to the same webhook URL, so one template can serve them all.
Tell the tools apart by a field only one of them sends:

```text
{{- if .receiver -}}
🔥 *{{ upper .status }}* · {{ .commonLabels.alertname }} (Alertmanager)
{{- else if .AlarmName -}}
☁️ *{{ .NewStateValue }}* · {{ .AlarmName }} (CloudWatch)
{{- else if .data -}}
🐞 *{{ .data.issue.title }}* (Sentry)
{{- else if .log -}}
📄 *Error in {{ default .kubernetes.labels.app "unknown app" }}* (Fluent Bit)
{{- else -}}
ℹ️ Alert received
{{- end }}
```

For a CloudWatch alarm with `"AlarmName": "checkout-api-5xx"` and
`"NewStateValue": "ALARM"`, this renders:

```text
☁️ *ALARM* · checkout-api-5xx (CloudWatch)
```

A field that a tool does not send is empty, so `if` on it is safe. Passing a
missing field to a text function is not: `{{ contains .source "aws" }}` fails
when `.source` is missing. Wrap it first, for example
`{{ contains (toString .source) "aws" }}`.

## Map severity to an icon

Look the severity up in a `dict` instead of chaining `if`s, and fall back with
`or` for values you did not list:

```text
{{- $icons := dict "critical" "🔴" "high" "🟠" "warning" "🟡" -}}
{{- $severity := lower (toString (default .commonLabels.severity "info")) -}}
{{ or (index $icons $severity) "ℹ️" }} *{{ upper $severity }}* {{ .commonLabels.alertname }}
```

For `"severity": "critical"` and `"alertname": "HighErrorRate"`, this renders:

```text
🔴 *CRITICAL* HighErrorRate
```

## Format by a number in the alert

Numbers in the alert JSON are decimals. Compare them with `ge` or `le`, and
write the threshold with a decimal point:

```text
{{- $rate := (index .alerts 0).annotations.error_rate -}}
{{ if ge $rate 10.0 }}🚨 *Error rate {{ printf "%.1f" $rate }}%*: page on-call
{{- else if ge $rate 5.0 }}⚠️ *Error rate {{ printf "%.1f" $rate }}%*: watch it
{{- end }}
```

For `"error_rate": 7.5`, this renders:

```text
⚠️ *Error rate 7.5%*: watch it
```

`gt` and `lt` do not compare these numbers; they return false.

## Keys with dashes or dots

A key such as `instance-id` or `app.kubernetes.io/name` cannot follow a dot.
Read it with `index`:

```text
🖥 Instance: {{ index .detail "instance-id" }}
```

For `"detail": {"instance-id": "i-0abc123"}`, this renders
`🖥 Instance: i-0abc123`.

## Many alerts in one message

Alertmanager groups alerts, so one webhook can carry several. Show a count, one
line per alert, and keep long text short:

```text
{{- $n := len .alerts -}}
🔥 *{{ .commonLabels.alertname }}*: {{ $n }} alert{{ if gt $n 1 }}s{{ end }}
{{ range .alerts -}}
• `{{ .labels.instance }}` {{ truncate (toString .annotations.summary) 60 }}
{{ end -}}
```

For two `HighErrorRate` alerts, this renders:

```text
🔥 *HighErrorRate*: 2 alerts
• `checkout-api:8080` 5xx error rate above 5% for 5 minutes
• `checkout-api:8081` 5xx error rate above 5% for 5 minutes
```

## Test a template

1. Turn on `debug_body` (or `DEBUG_BODY=true`) and send one real alert from
   your tool. Copy the body from the Versus log into a file.
2. Send that file to the webhook URL with `curl -d @alert.json` after each
   edit. Versus reads the template for every alert, so no restart is needed.
3. Read the answer. A template error does not lose the alert: the incident is
   stored, and `notify_status` is `failed` with the error in `notify_error`,
   for example `slack: failed to parse template: …`. The same error shows on
   the incident's **Notified** card in the UI.

Repeated test alerts are new incidents each time. To keep a noisy alert from
paging on-call in production, see [Alert Fatigue](../enterprise/alert-fatigue.md)
(_Enterprise_).
