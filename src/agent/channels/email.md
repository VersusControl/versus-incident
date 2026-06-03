# Email

Send incidents over SMTP to one or more recipients. Email is the only
channel that renders with `html/template`, so you get rich HTML formatting.

## Minimal config

```yaml
# config/config.yaml
alert:
  email:
    enable: true
    smtp_host: ${SMTP_HOST}
    smtp_port: ${SMTP_PORT}
    username: ${SMTP_USERNAME}
    password: ${SMTP_PASSWORD}
    to: ${EMAIL_TO}
    subject: ${EMAIL_SUBJECT}
    template_path: "config/email_message.tmpl"
```

Enable from the environment instead of YAML with `EMAIL_ENABLE=true`.

## SMTP settings

| Field | Notes |
|---|---|
| `smtp_host` | e.g. `smtp.gmail.com`, `email-smtp.us-east-1.amazonaws.com` |
| `smtp_port` | `587` (STARTTLS, common) or `465` (implicit TLS) |
| `username` / `password` | SMTP credentials — use an **app password** for Gmail/Microsoft 365, or SES SMTP credentials for AWS |
| `to` | comma-separated list for multiple recipients |
| `subject` | static default; override per request |

## Full reference

```yaml
email:
  enable: false
  smtp_host: ${SMTP_HOST}
  smtp_port: ${SMTP_PORT}
  username: ${SMTP_USERNAME}
  password: ${SMTP_PASSWORD}
  to: ${EMAIL_TO}              # "ops@example.com,sre@example.com"
  subject: ${EMAIL_SUBJECT}
  template_path: "config/email_message.tmpl"
```

## Per-request override

Change the recipient and subject for a single incident:

```bash
curl -X POST "http://localhost:3000/api/incidents?email_to=oncall@example.com&email_subject=DB%20down" \
  -H "Content-Type: application/json" \
  -d '{ "Logs": "PostgreSQL primary unreachable" }'
```

## Template

Rendered with Go's **`html/template`** from `config/email_message.tmpl`, so
values are HTML-escaped automatically — write real HTML in the body. Agent
detections use `config/agent_email.tmpl` when present. See
[Template Syntax](../../webhook/template-syntax.md) for the available fields and
functions.
</content>
