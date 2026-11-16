<h1 align="center" style="border-bottom: none">
  <img alt="Versus" src="src/docs/images/versus.svg">
</h1>

<p align="center">
  <a href="https://opensource.org/licenses/MIT"><img src="https://img.shields.io/badge/License-MIT-yellow.svg" alt="License: MIT"></a>
  <a href="https://github.com/sponsors/versuscontrol"><img src="https://img.shields.io/badge/sponsor-%E2%9D%A4-ff69b4" alt="Sponsor"></a>
</p>

<p align="center">
  <strong>Your AI SRE Teammate</strong> </br>
</p>

<p align="center">
  You don't have to monitor your systems alone. Versus Incident will keep track of your services, applications, and systems, detect new or unexpected problems, and provide your team with the evidence needed to investigate.
</p>

<p align="center">
  <i>Auto-detecting incidents in your system</i>
  <img alt="Versus Dashboard" src="src/docs/images/versus-dashboard.svg">
</p>

<p align="center">
  <i>Auto-detecting issues in Kubernetes</i>
  </br></br>
  <img alt="The Kubernetes Overview: the tab bar, cluster health with 2 broken and 14 warnings, a timeline with payments/settlement-worker in CrashLoopBackOff and shop/catalog in ImagePullBackOff, and CPU and memory capacity" src="src/docs/images/kubernetes-harness-overview.png">
</p>

## How Versus Works

Versus finds problems in three ways.

1. **AIOps for Kubernetes.** Versus reads your cluster read-only, maps its
   workloads, changes and health, and detects issues on its own. Each issue is
   linked to the logs and changes around it.
2. **AI SRE Agent.** Versus connects to your logs, metrics and
   traces, learns what normal looks like, and raises an incident
   when something new or unexpected appears. No alert rules to write.
3. **Webhook alerts.** The alert tools you already run (Alertmanager, Grafana,
   Sentry, CloudWatch SNS, FluentBit, or your own scripts) POST to Versus. Versus
   applies your template, sends the alert to every channel you enable, and
   escalates to on-call if nobody acknowledges it. Repeated noise is quieted by
   [Alert Fatigue](https://docs.versusincident.com/#/enterprise/alert-fatigue) (_Enterprise_).

## Features

### AIOps for Kubernetes

<p align="center">
  <img alt="AIOps for Kubernetes: Versus reads the cluster read-only, finds broken workloads, and links each issue to its changes and logs" src="src/docs/images/readme-feature-aiops-kubernetes.svg">
</p>

See your cluster's health, capacity, workloads, recent changes, Helm releases
and GitOps status in one place. Versus lists broken workloads as issues, puts
the changes and crashes around each break on a timeline, and can alert your
channels when a new issue appears. It never changes anything in the cluster.

[AIOps Kubernetes Quickstart](https://docs.versusincident.com/#/agent/kubernetes/quickstart)

### AI SRE Agent

<p align="center">
  <img alt="AI SRE Agent: Versus reads your logs, learns the normal patterns, and opens an incident when a new error appears" src="src/docs/images/readme-feature-sre-agent.svg">
</p>

Point the agent at your logs and let it learn your normal patterns in training
mode. In detect mode it opens an incident only when a new or unusual error shows
up, and sends it to your channels and on-call. Metrics and traces sources are _Enterprise_.

[AIOps SRE Quickstart](https://docs.versusincident.com/#/agent/quickstart)

### Webhook alerts

<p align="center">
  <img alt="Webhook alerts: your alert tools POST to Versus, which formats the alert with your template, sends it to your channels, and escalates to on-call" src="src/docs/images/readme-feature-webhook.svg">
</p>

Send alerts from any tool that can POST a webhook. Versus formats each alert
with your own template, fans it out to your channels, and escalates it to
on-call when it is not acknowledged in time.

[Webhook Quickstart](https://docs.versusincident.com/#/webhook/quickstart)

### More features

- **[DevOps Agent Chat](https://docs.versusincident.com/#/agent/devops-agent):** ask questions about your
  systems in the Versus UI; answers cite the evidence from read-only tools.
- **[Notification channels](https://docs.versusincident.com/#/agent/channels):** Slack, Microsoft Teams,
  Telegram, Viber, Email and Lark.
- **[On-call](https://docs.versusincident.com/#/oncall/on-call-introduction):** escalate through AWS Incident
  Manager, PagerDuty, ServiceNow or incident.io.
- **[Custom templates](https://docs.versusincident.com/#/webhook/template-syntax):** write your own alert
  messages with Go templates.
- **[PostgreSQL storage](https://docs.versusincident.com/#/configuration/postgres-storage):** keep incidents and
  agent state in PostgreSQL instead of local files.
- **Enterprise:** [Single Sign-On](https://docs.versusincident.com/#/enterprise/sso/overview),
  [High Availability](https://docs.versusincident.com/#/enterprise/ha/overview) and
  [SLI/SLO auto-define](https://docs.versusincident.com/#/enterprise/slo) (_Enterprise_).

## Get started

Pick the path that matches how you want Versus to find problems:

- [AIOps Kubernetes Quickstart](https://docs.versusincident.com/#/agent/kubernetes/quickstart): watch a cluster and see its issues.
- [AIOps SRE Quickstart](https://docs.versusincident.com/#/agent/quickstart): let the agent learn your logs and open incidents.
- [Webhook Quickstart](https://docs.versusincident.com/#/webhook/quickstart): send alerts from the tools you already run.

The full documentation lives at [docs.versusincident.com](https://docs.versusincident.com/).

## Roadmap

See [ROADMAP.md](ROADMAP.md) for the full list of shipped features, work
in progress, and planned phases (more log sources, metrics, traces,
cross-signal correlation).

## Support The Project

[GitHub Sponsors](https://github.com/sponsors/versuscontrol) · see [SPONSORS.md](SPONSORS.md)

## Contributing

Contributions are welcome. Please read [CONTRIBUTING.md](CONTRIBUTING.md)
for development setup, coding standards, and the PR process, and review
the [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) and [SECURITY.md](SECURITY.md)
before reporting vulnerabilities.

Project governance is documented in [GOVERNANCE.md](GOVERNANCE.md).

## License

Distributed under the MIT License. See [LICENSE](LICENSE) for more information.
