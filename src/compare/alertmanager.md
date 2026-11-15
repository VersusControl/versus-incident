# Versus vs Alertmanager

Prometheus Alertmanager takes the alerts your Prometheus rules fire, then
groups, silences and routes them to the right receiver. Versus finds problems
nobody wrote a rule for, in your logs and your Kubernetes clusters, and also
takes in alerts from the tools you already run, Alertmanager included.

## Feature comparison

✓ built in · partial: limited or through another tool · ✗ not built in

| Capability | Versus | Alertmanager |
|---|---|---|
| Detect new errors without writing rules | ✓ The AI SRE Agent opens an incident when a new or unusual error appears, or when a known error spikes. | ✗ Routes only the alerts your Prometheus rules fire. |
| Learns what normal looks like from logs | ✓ Reads File, Loki, Elasticsearch, Graylog, Splunk, SigNoz and CloudWatch Logs. Metrics and traces are _Enterprise_. | ✗ Does not read logs. |
| Kubernetes issue detection | ✓ Reads your cluster read-only, lists broken workloads and links each one to the changes and logs around it. | ✗ Not built in. |
| Webhook intake from existing tools | ✓ Alertmanager, Grafana, Sentry, CloudWatch SNS, FluentBit or your own scripts. | partial: Takes alerts from Prometheus and other clients through its API. |
| Notification channels | ✓ Slack, Microsoft Teams, Telegram, Viber, Email and Lark, each with your own message template. | ✓ Email, Slack, Microsoft Teams, Telegram, Discord, Mattermost, Webex, WeChat, Pushover, Amazon SNS, Jira, webhooks and more. |
| On-call escalation | ✓ If nobody acknowledges, Versus pages AWS Incident Manager, PagerDuty, ServiceNow or incident.io. Rotations stay in that tool. | partial: Sends to PagerDuty, Opsgenie, incident.io and VictorOps. No acknowledgment wait or escalation of its own. |
| Alert noise reduction | ✓ Known log patterns stay quiet. [Alert Fatigue](../enterprise/alert-fatigue.md) stops repeat alerts from paging (_Enterprise_). | ✓ Grouping, inhibition and silences. |
| SLOs | ✓ Proposes SLIs and SLOs per service and alerts on error-budget burn (_Enterprise_). | ✗ Not built in. |
| AI investigation and chat | ✓ [AI Analyze](../agent/ai-analyze-mode.md) suggests root causes; [DevOps Agent Chat](../agent/devops-agent.md) answers questions with evidence. Uses the AI provider key you supply. | ✗ Not built in. |
| Self-hosted, data stays in your network | ✓ Runs in your infrastructure. AI features call only the AI provider you configure. | ✓ Runs in your infrastructure. |
| Single sign-on | ✓ Google Workspace, Microsoft Entra ID or any OpenID Connect provider (_Enterprise_). | ✗ Not built in. TLS and basic auth only. |
| High availability | ✓ Several replicas on one Postgres (_Enterprise_). | ✓ Built-in clustering, on by default. |
| Open source | ✓ MIT core, plus a commercial Enterprise edition. | ✓ Apache 2.0. |

## When to choose Versus

- You want to catch errors nobody predicted, without writing and tuning a rule
  for each one.
- Your signal lives in logs or in Kubernetes, not only in Prometheus metrics.
- You want alerts acknowledged in Slack and escalated to your on-call tool when
  nobody responds.
- You want AI help to find the cause, with the evidence it used.

## When Alertmanager is the better fit

- All your alerting is Prometheus metric thresholds, and you are happy writing
  and maintaining those rules.
- You need a receiver Versus does not offer, such as Discord, Mattermost,
  Webex or Jira.
- You want a small, proven router with no AI and no extra database.

## Use them together

You do not have to choose. Keep Alertmanager for your metric thresholds and
send its alerts to Versus. Versus formats them with your template, sends them
to your channels, and escalates them to on-call. Its AI SRE Agent covers the
errors your rules cannot see.

1. Start Versus and open the UI.
2. In Alertmanager, add a webhook receiver that points at your Versus webhook
   URL, and route your alerts to it.
3. Fire an alert. It appears in the Versus UI under **Incidents**, on the
   **Webhook / Alerts** tab, and in every channel you turned on.
4. Optionally turn on on-call. Slack messages then get an **Acknowledge Alert**
   button, and Versus pages your on-call tool if nobody presses it in time.

The [Webhook Quickstart](../webhook/quickstart.md) walks through these steps
with a demo Alertmanager.

## Get started

- [Webhook Quickstart](../webhook/quickstart.md): send Alertmanager alerts to
  Versus.
- [AIOps SRE Quickstart](../agent/quickstart.md): let the agent learn your logs
  and catch new errors.
- For Enterprise, see [pricing](https://versusincident.com/pricing) or email
  <a href="mailto:admin@versusincident.com?subject=Versus%20vs%20Alertmanager">admin@versusincident.com</a>.
