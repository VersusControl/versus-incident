<h1 align="center" style="border-bottom: none">
  <img alt="Versus" src="docs/images/versus.svg">
</h1>

<p align="center">
  <a href="https://goreportcard.com/report/github.com/VersusControl/versus-incident"><img src="https://goreportcard.com/badge/github.com/VersusControl/versus-incident" alt="Go Report Card"></a>
  <a href="https://opensource.org/licenses/MIT"><img src="https://img.shields.io/badge/License-MIT-yellow.svg" alt="License: MIT"></a>
  <a href="https://github.com/sponsors/versuscontrol"><img src="https://img.shields.io/badge/sponsor-%E2%9D%A4-ff69b4" alt="Sponsor"></a>
</p>

Versus is an open-source **incident management tool** built with an **AI SRE agent**. The agent watches your logs, learns what *normal* looks like, and automatically opens an incident the moment something new and unexpected appears — with no alert rules to write.

It also accepts incidents from any webhook-capable tool (Alertmanager, Grafana, Sentry, CloudWatch, FluentBit). Versus supports **multi-channel notifications** to Slack, Microsoft Teams, Telegram, Viber, Email, and Lark with templates you fully control, plus **on-call escalation** to AWS Incident Manager or PagerDuty when an incident goes unacknowledged.

![Versus](docs/images/versus-dashboard-01.png)

## How Versus Creates Incidents

Incidents reach Versus two ways, and both are handled by the same notification, templating, and on-call logic:

- **AI SRE Agent (auto-detect)** — point the agent at your logs and it learns your normal patterns, then automatically raises an incident when a brand-new error or anomaly appears. No alert rules to maintain. See [AI Agent — Introduction](./agent/agent-introduction.md).
- **Webhook alerts (you define)** — any tool that can POST a webhook (Alertmanager, Grafana, Sentry, CloudWatch SNS, FluentBit, or your own scripts) sends incidents straight to Versus, formatted with your own templates. See [Getting Started](./webhook/getting-started.md).

Whichever source raises it, an incident is templated, fanned out to every channel you enable, and escalated to on-call if it isn't acknowledged in time.

## Features

- 🤖 **AI SRE Agent** *(Beta)*: An AI agent that reads your logs, learns what normal looks like, and automatically opens an incident only when something new and unexpected appears.
- 🌐 **Webhook Alerts**: Receive incidents from any tool that can POST a webhook — Alertmanager, Grafana, Sentry, CloudWatch SNS, FluentBit, and more.
- 🚨 **Multi-channel Notifications**: Fan out every incident to Slack, Microsoft Teams, Telegram, Viber, Email, and Lark (more channels coming!)
- 📝 **Custom Templates**: Define your own alert messages using Go templates
- 🔧 **Easy Configuration**: YAML-based configuration with environment variables support
- 📡 **REST API**: Simple HTTP interface to receive alerts
- 📞 **On-Call**: On-Call integrations with AWS Incident Manager and PagerDuty

![versus](docs/images/versus-architecture.png)

## Roadmap

See [ROADMAP.md](ROADMAP.md) for the full list of shipped features, work
in progress, and planned phases (more log sources, metrics, traces,
cross-signal correlation).

## Support The Project

[GitHub Sponsors](https://github.com/sponsors/versuscontrol) · see [SPONSORS.md](https://github.com/VersusControl/versus-incident/blob/main/SPONSORS.md)

## Contributing

Contributions are welcome. Please read [CONTRIBUTING.md](https://github.com/VersusControl/versus-incident/blob/main/CONTRIBUTING.md)
for development setup, coding standards, and the PR process, and review
the [CODE_OF_CONDUCT.md](https://github.com/VersusControl/versus-incident/blob/main/CODE_OF_CONDUCT.md) and [SECURITY.md](https://github.com/VersusControl/versus-incident/blob/main/SECURITY.md)
before reporting vulnerabilities.

Project governance is documented in [GOVERNANCE.md](https://github.com/VersusControl/versus-incident/blob/main/GOVERNANCE.md).

## License

Distributed under the MIT License. See `LICENSE` for more information.
