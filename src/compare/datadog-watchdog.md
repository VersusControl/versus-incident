# Versus vs Datadog Watchdog

Datadog Watchdog is the AI layer inside Datadog's cloud platform: it flags
anomalies in the logs, traces and infrastructure data you send to Datadog, and
Bits AI and Datadog On-Call build investigation and paging around it. Versus
does the same core job, finding new problems without rules, from a deployment
you run yourself, so your logs never leave your network for analysis.

## Feature comparison

✓ built in · partial: limited or through another tool · ✗ not built in

| Capability | Versus | Datadog (Watchdog, Bits AI, On-Call) |
|---|---|---|
| Detect new errors without writing rules | ✓ The AI SRE Agent opens an incident when a new or unusual error appears, or when a known error spikes. | ✓ Watchdog needs no setup and flags new log patterns and spikes in existing ones. |
| Learns what normal looks like from logs | ✓ Reads File, Loki, Elasticsearch, Graylog, Splunk, SigNoz and CloudWatch Logs where they already live. Metrics and traces are _Enterprise_. | ✓ Learns from the logs you send to Datadog Log Management. |
| Kubernetes issue detection | ✓ Reads your cluster read-only, lists broken workloads and links each one to the changes and logs around it. | ✓ Watchdog shows Kubernetes anomalies in the Kubernetes Explorer. |
| Webhook intake from existing tools | ✓ Alertmanager, Grafana, Sentry, CloudWatch SNS, FluentBit or your own scripts. | ✓ Event Management takes in events and alerts from any source. |
| Notification channels | ✓ Slack, Microsoft Teams, Telegram, Viber, Email and Lark. | ✓ Slack, Microsoft Teams, email, webhooks, Jira, PagerDuty, ServiceNow and more. |
| On-call escalation | ✓ If nobody acknowledges, Versus pages AWS Incident Manager, PagerDuty, ServiceNow or incident.io. Rotations stay in that tool. | ✓ Datadog On-Call has its own schedules, escalation policies and paging. |
| Alert noise reduction | ✓ Known log patterns stay quiet. [Alert Fatigue](../enterprise/alert-fatigue.md) stops repeat alerts from paging (_Enterprise_). | ✓ Event Management correlation groups related alerts. |
| SLOs | ✓ Proposes SLIs and SLOs per service and alerts on error-budget burn (_Enterprise_). | ✓ SLOs with burn-rate tracking. |
| AI investigation and chat | ✓ [AI Analyze](../agent/ai-analyze-mode.md) suggests root causes; [DevOps Agent Chat](../agent/devops-agent.md) answers questions with evidence. Uses the AI provider key you supply. | ✓ Bits AI chat and Bits investigations. Billed in AI credits. |
| Self-hosted, data stays in your network | ✓ Runs in your infrastructure. AI features call only the AI provider you configure. | ✗ SaaS. Your data is sent to Datadog. |
| Single sign-on | ✓ Google Workspace, Microsoft Entra ID or any OpenID Connect provider (_Enterprise_). | ✓ SAML. |
| High availability | ✓ Several replicas on one Postgres (_Enterprise_). | — Run by Datadog as a managed service. |
| Open source | ✓ MIT core, plus a commercial Enterprise edition. | ✗ Proprietary. |

## When to choose Versus

- Your logs cannot leave your network, for data residency, sovereignty or
  compliance reasons.
- Your logs already live in Loki, Elasticsearch, Graylog, Splunk, SigNoz or
  CloudWatch, and you do not want to send them to another platform to watch
  them.
- You already have an on-call tool and want something that feeds it fewer,
  better incidents.
- You want an open-source core you can run and inspect yourself.

## When Datadog Watchdog is the better fit

- You already run Datadog across metrics, traces and logs and want anomalies
  linked across all of them in one place.
- You want APM-grade trace anomalies, real user monitoring and profiling from
  one vendor.
- You prefer a fully managed service and have no data-residency limits.
- You want schedules, escalation and paging in the same product as detection.

## Use them together

Versus can run next to Datadog. Point the AI SRE Agent at the logs you keep in
your own network, and keep Datadog for the data you already send there. Both
can page the same on-call tool, such as PagerDuty.

To bring Datadog monitor alerts into Versus as well, add a Datadog webhook
that points at your Versus webhook URL. Datadog must be able to reach that
URL. The [Webhook Quickstart](../webhook/quickstart.md) shows how Versus
receives, formats and routes an incoming alert.

## Get started

- [AIOps SRE Quickstart](../agent/quickstart.md): let the agent learn your logs
  and catch new errors.
- [AIOps Kubernetes Quickstart](../agent/kubernetes/quickstart.md): find broken
  workloads in your cluster.
- For Enterprise, see [pricing](https://versusincident.com/pricing) or email
  <a href="mailto:admin@versusincident.com?subject=Versus%20vs%20Datadog%20Watchdog">admin@versusincident.com</a>.
