# Versus vs Keep

Keep is an open-source alert management and AIOps platform: it collects the
alerts your monitoring tools already fire, then deduplicates, correlates and
automates them in one place. Versus creates the signal itself, by learning
what normal looks like in your logs and Kubernetes clusters, and raises an
incident only when something new or unusual appears.

## Feature comparison

✓ built in · partial: limited, experimental or paid only · ✗ not built in

| Capability | Versus | Keep |
|---|---|---|
| Detect new errors without writing rules | ✓ The AI SRE Agent opens an incident when a new or unusual error appears, or when a known error spikes. | ✗ Works on alerts that other tools already fire. |
| Learns what normal looks like from logs | ✓ Reads File, Loki, Elasticsearch, Graylog, Splunk, SigNoz and CloudWatch Logs. Metrics and traces are _Enterprise_. | ✗ Works on alerts, not raw logs. |
| Kubernetes issue detection | ✓ Reads your cluster read-only, lists broken workloads and links each one to the changes and logs around it. | — Takes alerts from the tools that watch your cluster. |
| Webhook intake from existing tools | ✓ Alertmanager, Grafana, Sentry, CloudWatch SNS, FluentBit or your own scripts. | ✓ 100+ providers, with two-way sync for many monitoring tools. |
| Notification channels | ✓ Slack, Microsoft Teams, Telegram, Viber, Email and Lark. | ✓ Many chat, ticketing and incident tools through providers and workflows. |
| On-call escalation | ✓ If nobody acknowledges, Versus pages AWS Incident Manager, PagerDuty, ServiceNow or incident.io. Rotations stay in that tool. | partial: Sends to PagerDuty, Opsgenie, Grafana OnCall and others. No schedules of its own. |
| Alert noise reduction | ✓ Known log patterns stay quiet. [Alert Fatigue](../enterprise/alert-fatigue.md) stops repeat alerts from paging (_Enterprise_). | ✓ Deduplication, filtering and rule-based correlation. AI correlation is in Keep Cloud and Enterprise only. |
| SLOs | ✓ Proposes SLIs and SLOs per service and alerts on error-budget burn (_Enterprise_). | — |
| AI investigation and chat | ✓ [AI Analyze](../agent/ai-analyze-mode.md) suggests root causes; [DevOps Agent Chat](../agent/devops-agent.md) answers questions with evidence. Uses the AI provider key you supply. | partial: AI incident assistant in Keep Cloud and Enterprise; experimental in open source. |
| Self-hosted, data stays in your network | ✓ Runs in your infrastructure. AI features call only the AI provider you configure. | ✓ Self-hosted, including air-gapped. A hosted Keep Cloud is also offered. |
| Single sign-on | ✓ Google Workspace, Microsoft Entra ID or any OpenID Connect provider (_Enterprise_). | ✓ Okta, OneLogin and generic OIDC in open source; Auth0, Keycloak and Azure AD in Enterprise. |
| High availability | ✓ Several replicas on one Postgres (_Enterprise_). | ✓ Listed in Keep's README, with horizontal scaling. |
| Open source | ✓ MIT core, plus a commercial Enterprise edition. | ✓ MIT, with a separately licensed enterprise folder. |

## When to choose Versus

- You do not have good alert rules yet, or you are tired of maintaining them,
  and want the agent to catch new errors from your logs.
- You run Kubernetes and want broken workloads found and linked to the changes
  that came before them.
- You want AI root-cause analysis and chat in the open-source edition.
- You want one tool that detects, notifies and escalates to on-call.

## When Keep is the better fit

- You already have many monitoring tools and mature alert rules, and your
  problem is too many alerts from too many places.
- You want one inbox for alerts from 100+ sources, with two-way sync back to
  those tools.
- You want rich workflow automation over alerts, such as opening tickets or
  enriching alerts with data from other systems.

## Use them together

Versus and Keep do different jobs, so they can sit side by side. Versus
detects new problems in your logs and Kubernetes clusters and pages on-call.
Keep collects alerts from the rest of your stack.

If Keep is your alert hub, a Keep workflow can forward selected alerts to your
Versus webhook URL. Versus then formats them with your template, sends them to
your channels, and escalates them to on-call. See the
[Webhook Quickstart](../webhook/quickstart.md) for how Versus receives and
routes an incoming alert.

## Get started

- [AIOps SRE Quickstart](../agent/quickstart.md): let the agent learn your logs
  and catch new errors.
- [Webhook Quickstart](../webhook/quickstart.md): send alerts from your existing
  tools to Versus.
- For Enterprise, see [pricing](https://versusincident.com/pricing) or email
  <a href="mailto:admin@versusincident.com?subject=Versus%20vs%20Keep">admin@versusincident.com</a>.
