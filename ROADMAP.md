# Roadmap

What is **done** and what is **next**. Feature level only — no implementation
detail.

The live tracker is the public GitHub Project board
[Versus Incident Roadmap](https://github.com/orgs/VersusControl/projects/2).
This file is the human-readable mirror.

> **Open-core note:** this roadmap covers the OSS (MIT) product. Enterprise tier
> capabilities are tracked separately — see [GOVERNANCE.md](GOVERNANCE.md).

---

## Done

### Alerting
- [x] Slack, Telegram, Microsoft Teams, Viber, Email and Lark notifications
- [x] Interactive acknowledgment
- [x] Custom templates per channel, plus a universal default template
- [x] Multiple destinations per channel per request
- [x] Per-channel proxy support

### On-call
- [x] AWS Incident Manager and PagerDuty integrations
- [x] Acknowledge-or-escalate workflow
- [x] Per-request on-call overrides

### Queue listeners
- [x] AWS SNS and SQS

### Incident management
- [x] Persistent incident history with search and filtering
- [x] Web UI — incident list, detail, timeline and payload
- [x] Team and member management, with incident assignment
- [x] Incident analytics report delivered to a channel, on demand or daily

### AI SRE Agent — detection
- [x] Training / shadow / detect modes
- [x] Log pattern mining with a persisted pattern catalog
- [x] Frequency spike detection
- [x] Service detection, with per-service attribution overrides
- [x] New-service grace period
- [x] Redaction of ingested log samples and selected tool output
- [x] Detection audit log and agent dashboard

### AI SRE Agent — investigation
- [x] On-demand incident analysis
- [x] Read-only investigation tools — recent incidents, pattern history, service
      description, related logs, recent changes, service dependencies
- [x] Runbook search over your own runbooks
- [x] DevOps chat website interface for open-ended, on-demand service investigation
- [x] Conversation history, streaming tool activity, and investigation loop
      guardrails for repeated calls and stalled progress
- [x] Kubernetes workload, resource, event, and pod-log investigation tools

### Service health
- [x] Service Heatmap with log and incident evidence, source coverage, and
      explicit missing or stale data states
- [x] Configurable collection interval and assessment window

### Signal sources
- [x] Elasticsearch, File, Graylog, Splunk, Loki and CloudWatch Logs

### Platform
- [x] Multi-provider AI — OpenAI, Gemini, Ollama and OpenAI-compatible endpoints
- [x] Optional AI — detection and alerting keep working with AI turned off
- [x] Pluggable storage, including Postgres with server-side search
- [x] YAML configuration with environment expansion
- [x] Docker images and a published Helm chart

### Agent stream analysis
- [x] Watch the agent investigate live, step by step, instead of waiting on a
      spinner
- [x] A Tools page showing every tool the agent can use, whether it is active,
      and what to configure to switch it on

---

## Next

In the order we intend to build.

### Agent quality
- [ ] Evaluation harness — score the agent against recorded incidents whose
      answers are already known, so improvement and regression are measurable
- [ ] A starter pack of scored scenarios shipped with the harness
- [ ] End-to-end model egress guard that checks chat input and every tool result
      for secrets before data reaches an AI provider

### DevOps agent CLI
- [ ] Separate `versus-devops` CLI that connects to a Versus server, shares web
      sessions, and uses server-managed tools, skills, MCP, and policy

### Investigation depth
- [ ] Release-qualified native log read tools for configured providers
- [ ] More read-only investigation tools, added where users actually need them

### Suggested remediation
- [ ] Approved, scoped, non-destructive actions from an operator allow-list
- [ ] Manual commands and guidance for delete/remove operations, never executed
      by the agent

### Cost control
- [ ] Model routing — cheaper model for refinement, stronger model for the final
      summary
- [ ] Per-team and per-source budgets
- [ ] Self-hosted-only enforcement

### Ecosystem
- [ ] GCP Pub/Sub and Azure Service Bus listeners
- [ ] Prometheus metrics endpoint for Versus itself
- [ ] Multiple template sets per channel

---

## How to influence the roadmap

- File an issue with your use case at
  [github.com/VersusControl/versus-incident/issues](https://github.com/VersusControl/versus-incident/issues)
- Sponsors at the Gold tier and above get a monthly roadmap call — see
  [SPONSORS.md](SPONSORS.md)
