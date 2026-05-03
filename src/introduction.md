<h1 align="center" style="border-bottom: none">
  <img alt="Versus" src="docs/images/versus.svg">
</h1>

<p align="center">
  <a href="https://goreportcard.com/report/github.com/VersusControl/versus-incident"><img src="https://goreportcard.com/badge/github.com/VersusControl/versus-incident" alt="Go Report Card"></a>
  <a href="https://opensource.org/licenses/MIT"><img src="https://img.shields.io/badge/License-MIT-yellow.svg" alt="License: MIT"></a>
  <a href="https://github.com/sponsors/versuscontrol"><img src="https://img.shields.io/badge/sponsor-%E2%9D%A4-ff69b4" alt="Sponsor"></a>
</p>

An incident management tool that supports alerting across multiple channels with easy custom messaging and on-call integrations. Compatible with any tool supporting webhook alerts, it's designed for modern DevOps teams to quickly respond to production incidents.

With the built-in **AI SRE Agent**, Versus goes further — continuously observing your logs, metrics, and traces, learning what normal looks like, and alerting you only when something new and unexpected appears.

### Features

- 🚨 **Multi-channel Alerts**: Send incident notifications to Slack, Microsoft Teams, Telegram, and Email (more channels coming!)
- 📝 **Custom Templates**: Define your own alert messages using Go templates
- 🔧 **Easy Configuration**: YAML-based configuration with environment variables support
- 📡 **REST API**: Simple HTTP interface to receive alerts
- 📡 **On-call**: On-call integrations with AWS Incident Manager
- 🤖 **AI Agent** *(Beta)*: An AI SRE agent that reads your logs, metrics and tracing, learns what normal looks like, and only alerts you when something new and unexpected appears.

![versus](docs/images/versus-architecture.svg)

## Roadmap

See [ROADMAP.md](ROADMAP.md) for the full list of shipped features, work
in progress, and planned phases (more log sources, metrics, traces,
cross-signal correlation).

![Versus Control](src/docs/images/road-map.svg)

## Support The Project

[GitHub Sponsors](https://github.com/sponsors/versuscontrol) · see [SPONSORS.md](SPONSORS.md)

## Contributing

Contributions are welcome. Please read [CONTRIBUTING.md](CONTRIBUTING.md)
for development setup, coding standards, and the PR process, and review
the [Code of Conduct](CODE_OF_CONDUCT.md) and [security policy](SECURITY.md)
before reporting vulnerabilities.

Project governance is documented in [GOVERNANCE.md](GOVERNANCE.md).

## License

Distributed under the MIT License. See `LICENSE` for more information.
