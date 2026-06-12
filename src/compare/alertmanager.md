# Versus vs Alertmanager

Prometheus **Alertmanager** is the de-facto open-source router for Prometheus
alerts: it deduplicates, groups, silences, and dispatches alerts that **your
PromQL rules** fire. **Versus Incident is the self-hosted AI SRE agent** — it
learns what your logs normally look like and raises an incident when something
new or unexpected issues appears, with no rules to write.

The good news: you do **not** have to choose. Versus is a **drop-in webhook
target** for Alertmanager. Keep Alertmanager for your metric thresholds, and add
Versus for everything your thresholds can't see — the novel log error nobody
wrote a rule for yet.

## Drop-in: point Alertmanager at Versus in one block

Versus exposes a simple `POST /api/incidents` endpoint. Add it as a receiver and
Alertmanager forwards alerts straight into Versus's templating, multi-channel
notification, and on-call escalation pipeline:

```yaml
route:
  receiver: 'versus-incident'
  group_wait: 10s

receivers:
- name: 'versus-incident'
  webhook_configs:
  - url: 'http://versus-host:3000/api/incidents'
    send_resolved: false
```

Full walkthrough with custom templates:
[Use Alertmanager](../examples/alertmanager.md).

## The wedge: rules you wrote vs anomalies you didn't

Alertmanager can only route what your **PromQL rules already detect.** Every
incident it dispatches started life as a threshold a human predicted and authored.
That misses the failure mode nobody anticipated.

Versus adds the missing half: an **AI agent that learns normal from your logs**
(drain miner + EWMA + grace periods) and opens an incident for brand-new errors
and anomalies **without a rule.** Together they cover both *known* threshold
breaches and *unknown* novel failures.

## Side-by-side

| | **Versus Incident** | **Alertmanager** |
|---|---|---|
| What it does | Detects new/anomalous behaviour from **logs** | Routes alerts your **rules** fire |
| Needs alert rules | No — learns normal automatically | Yes — PromQL rules required |
| Signal source | Logs (Elasticsearch, Loki, Graylog, Splunk, file, …) | Prometheus metrics |
| AI detection | Drain miner + EWMA + LLM detect, auditable | None — pure routing |
| Notification channels | Slack, Teams, Telegram, Viber, Email, Lark | Slack, email, PagerDuty, webhook |
| On-call escalation | Built in (feeds PagerDuty / Opsgenie / incident.io) | Via receivers |
| Templating | Full Go templates you control | Go templates |
| Self-hosted | Yes (or air-gapped) | Yes |
| License | MIT core + commercial Enterprise | Apache 2.0 |
| Works with the other | **Yes — drop-in webhook target** | Yes — Versus is a receiver |

## Where Alertmanager alone is enough

- Your alerting need is entirely **metric thresholds** in Prometheus, and you are
  happy authoring and maintaining PromQL rules.
- You don't need log-based anomaly detection or AI triage.

## Where Versus adds value

- **Catch the unknowns.** Anomalies nobody wrote a rule for — surfaced from logs.
- **Less rule maintenance.** The agent learns normal instead of you encoding it.
- **Richer routing out of the box** — multi-channel notifications plus on-call
  escalation, all templated.
- **Self-hosted and auditable** — data stays in your infrastructure; every AI decision is
  recorded.

## Try it

Wire Versus behind Alertmanager in minutes:
[Use Alertmanager](../examples/alertmanager.md), or start with the AI agent in
[AI Agent — Getting Started](../agent/getting-started.md). Email
<a href="mailto:admin@versusincident.com?subject=Versus%20vs%20Alertmanager">admin@versusincident.com</a> for enterprise pricing.
