# Template Syntax

A template turns the JSON body of an alert into a channel message. Templates
use Go template syntax: text is copied as-is, and anything inside `{{ }}` is
filled in from the alert.

The examples on this page use this Alertmanager alert:

```json
{
  "status": "firing",
  "alerts": [
    {
      "status": "firing",
      "labels": {
        "alertname": "HighErrorRate",
        "severity": "critical",
        "instance": "checkout-api:8080",
        "app.kubernetes.io/name": "checkout-api"
      },
      "annotations": {
        "summary": "5xx error rate above 5% for 5 minutes",
        "error_rate": 7.5
      },
      "startsAt": "2026-11-15T09:00:00Z"
    }
  ],
  "commonLabels": { "alertname": "HighErrorRate", "severity": "critical" }
}
```

## Read a field

Start with a dot and follow the JSON keys:

```text
{{ .status }} · {{ .commonLabels.alertname }}
```

Renders: `firing · HighErrorRate`

| To read | Write | Renders |
| --- | --- | --- |
| A nested field | `{{ .commonLabels.severity }}` | `critical` |
| An item in a list | `{{ (index .alerts 0).labels.instance }}` | `checkout-api:8080` |
| A key with `-`, `.` or `/` in it | `{{ index (index .alerts 0).labels "app.kubernetes.io/name" }}` | `checkout-api` |
| A field that may be missing | `{{ default .commonLabels.team "unassigned" }}` | `unassigned` |

A field that is not in the alert renders as `<no value>`. Use `default` or an
`if` to show something better.

## Variables

Store a value once and reuse it:

```text
{{ $first := index .alerts 0 }}
{{ $first.labels.alertname }} on {{ $first.labels.instance }}
```

Renders: `HighErrorRate on checkout-api:8080`

A variable can be changed later with `=`:

```text
{{ $icon := "🔥" }}{{ if eq .status "resolved" }}{{ $icon = "✅" }}{{ end }}{{ $icon }} {{ upper .status }}
```

Renders: `🔥 FIRING`

## Conditions

```text
{{ if eq .commonLabels.severity "critical" }}🔴 Page now
{{ else if eq .commonLabels.severity "warning" }}🟡 Check today
{{ else }}ℹ️ FYI
{{ end }}
```

Renders: `🔴 Page now`

| Test | Example | True when |
| --- | --- | --- |
| `eq`, `ne` | `eq .status "firing"` | The two values are equal (`ne`: not equal). |
| `and` | `and .status .commonLabels.alertname` | Every value is set. |
| `or` | `or .commonLabels.team "unassigned"` | Any value is set. As a value, `or` returns the first one that is set. |
| `not` | `not .commonLabels.team` | The value is empty or missing. |
| `contains` | `contains .status "fir"` | The first string contains the second. |
| `regexMatch` | `regexMatch "(?i)5xx" (index .alerts 0).annotations.summary` | The regular expression matches the string. |
| `gt`, `lt` | `gt (len .alerts) 1` | Greater or less than, for whole numbers such as `len` results, and for strings. |
| `ge`, `le` | `ge (index .alerts 0).annotations.error_rate 5.0` | Greater or equal, less or equal, for numbers from the JSON. |

Numbers in the alert JSON are decimals. Compare them with `ge` and `le`, and
write the other side with a decimal point (`5.0`, not `5`). `gt`, `lt` and
`eq` do not match a JSON number against a literal like `5`. To test a number
for equality, compare it as text: `eq (toString .count) "5"`.

## Loops

`range` repeats its body for each item in a list. Inside, `.` is the current
item:

```text
{{ range .alerts }}
• {{ .labels.alertname }} on {{ .labels.instance }}: {{ .annotations.summary }}
{{ end }}
```

Renders: `• HighErrorRate on checkout-api:8080: 5xx error rate above 5% for 5 minutes`

To get the position too, name both: `{{ range $i, $alert := .alerts }}`.
`range` over an object gives each key and value:

```text
{{ range $key, $value := .commonLabels }}{{ $key }}={{ $value }} {{ end }}
```

Renders: `alertname=HighErrorRate severity=critical`

## Whitespace and comments

`{{-` trims the spaces and newlines before a tag, and `-}}` trims those after
it. Use them to keep logic lines out of the message:

```text
{{- if eq .status "firing" -}}
FIRING
{{- end }}
```

Renders `FIRING` with no blank lines around it.

`{{/* … */}}` is a comment and renders nothing:

```text
{{/* Owner: platform team. Used by the checkout alerts. */}}
```

## Functions

Call a function with its arguments after it, separated by spaces:

```text
{{ replace .commonLabels.alertname "Rate" " rate" }}
```

Renders: `HighError rate`

A pipe `|` passes a value as the **last** argument. It reads well with
one-argument functions, and with `format`:

```text
{{ .status | upper }} at {{ (index .alerts 0).startsAt | format "15:04" }}
```

Renders: `FIRING at 09:00`

Most other functions take the text first, so call them directly instead of
piping into them: `{{ trimPrefix .x "prod-" }}`, not `{{ .x | trimPrefix "prod-" }}`.

### Text

| Function | Example | Renders |
| --- | --- | --- |
| `upper` | `{{ upper .status }}` | `FIRING` |
| `lower` | `{{ lower "CRITICAL" }}` | `critical` |
| `title` | `{{ title "checkout api" }}` | `Checkout Api` |
| `default` | `{{ default .commonLabels.team "unassigned" }}` | `unassigned` when `team` is missing or empty |
| `toString` | `{{ toString (index .alerts 0).annotations.error_rate }}` | `7.5` |
| `replace` (also `replaceAll`) | `{{ replace "aws.ec2" "aws." "" }}` | `ec2` |
| `trimPrefix` | `{{ trimPrefix "prod-checkout" "prod-" }}` | `checkout` |
| `trimSuffix` | `{{ trimSuffix "checkout-service" "-service" }}` | `checkout` |
| `truncate` | `{{ truncate (index .alerts 0).annotations.summary 14 }}` | `5xx error rate` |
| `slice` | `{{ slice .status 0 4 }}` | `firi` (characters 0 to 3) |
| `split` | `{{ index (split "checkout-api:8080" ":") 0 }}` | `checkout-api` |
| `join` | `{{ join (split "a,b,c" ",") " / " }}` | `a / b / c` |
| `printf` | `{{ printf "%.1f%%" (index .alerts 0).annotations.error_rate }}` | `7.5%` |
| `len` | `{{ len .alerts }}` | `1` (works on text, lists and objects) |
| `urlquery` | `https://grafana.example.com/search?query={{ urlquery .commonLabels.alertname }}` | the name, escaped for a URL |
| `escapeHTML` | `{{ escapeHTML "<5xx>" }}` | `&lt;5xx&gt;`, safe in a Telegram HTML message |

`truncate` and `slice` count bytes, so they can cut an emoji or accented
letter in half. If `slice` is given positions outside the text, it returns the
whole text.

### Time

| Function | Example | Renders |
| --- | --- | --- |
| `formatTime` | `{{ formatTime (index .alerts 0).startsAt }}` | `2026-11-15 09:00:00` |
| `format` | `{{ (index .alerts 0).startsAt \| format "Jan 2, 15:04 MST" }}` | `Nov 15, 09:00 UTC` |
| `now` | `{{ now \| format "2006-01-02" }}` | today's date |

`formatTime` reads RFC 3339 times like `2026-11-15T09:00:00Z`. `format` also
reads the common forms `2006-01-02 15:04:05` and `2006-01-02`. Layouts are
written with the reference time `Mon Jan 2 15:04:05 MST 2006`.

### Lists, objects and numbers

| Function | Example | Renders |
| --- | --- | --- |
| `index` | `{{ index .commonLabels "severity" }}` | `critical` |
| `list`, `append` | `{{ $names := list }}{{ $names = append $names "api" }}` | builds a list |
| `stringSlice` | `{{ join (stringSlice $names) ", " }}` | joins a built list: `api` |
| `dict` | `{{ $icons := dict "critical" "🔴" "warning" "🟡" }}{{ index $icons .commonLabels.severity }}` | `🔴` |
| `add` | `{{ add (len .alerts) 1 }}` | `2` |

### Environment

`{{ env "DEFAULT_RUNBOOK_URL" }}` reads an environment variable of the Versus
container. The built-in templates use it for a default runbook link. Any
variable is readable, so never print one that holds a secret.

## Microsoft Teams templates

Write Teams templates in Markdown. Versus turns the message into an Adaptive
Card and sends it through your Power Automate workflow. Teams shows:

| Markdown | Shows as |
| --- | --- |
| `#`, `##`, `###` at the start of a line | Headings |
| A line that is all `**bold**` | A bold line |
| `-` or `*` items, and `1.` items | Lists |
| `[text](url)` | A link. The first link on a line opens when the line is clicked. |
| A line with only ```` ``` ```` above and below the text | A code block |

Put the code fences on their own lines. A fence on the same line as text
drops that text.

The first heading (or the first line, if there is no heading) becomes the
notification summary, and a plain-text copy is sent for clients that cannot
show cards. You do not add these yourself.

````markdown
# {{ .commonLabels.alertname }} is {{ upper .status }}

**Severity**: {{ .commonLabels.severity }}

{{ range .alerts -}}
- {{ .labels.instance }}: {{ .annotations.summary }}
{{ end }}
```
{{ (index .alerts 0).annotations.summary }}
```

[Open the dashboard](https://grafana.example.com/d/checkout)
````

## Next steps

- [Advanced Template Tips](./advanced-template-tips.md): one template for
  several tools, and severity-based formatting.
- [Send Alerts from Your Tools](./send-alerts.md): where templates are set and
  how to see the payload your tool sends.
