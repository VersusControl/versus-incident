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
- [x] Redaction before anything leaves the box
- [x] Detection audit log and agent dashboard

### AI SRE Agent — investigation
- [x] On-demand incident analysis
- [x] Read-only investigation tools — recent incidents, pattern history, service
      description, related logs, recent changes, service dependencies
- [x] Metric and trace lookups during an investigation
- [x] Runbook search over your own runbooks

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

### Service readiness
- [ ] Tools reporting a service's signal coverage, objectives, alert coverage
      and runbook coverage
- [ ] Infrastructure-aware tools — Kubernetes workloads and autoscalers, and IaC
      manifests — so the agent can reason about capacity, scaling and resilience

### Agent quality
- [ ] Evaluation harness — score the agent against recorded incidents whose
      answers are already known, so improvement and regression are measurable
- [ ] A starter pack of scored scenarios shipped with the harness

### Investigation depth
- [ ] Investigate a service on demand, by name, without waiting for an alert
- [ ] Investigation loop guardrails — skip repeat tool calls, stop when the agent
      stops making progress, and open with the most useful evidence
- [ ] More read-only investigation tools, added where users actually need them

### Suggested remediation
- [ ] Suggested actions for known-good categories, gated by an operator
      allow-list
- [ ] Text only — no auto-execution, human approval always required

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
