# Roadmap

The roadmap is split into **shipped**, **in progress**, and **planned**.
Done items are checked. For finer-grained tracking of the AI agent work
see [GitHub Project](https://github.com/orgs/VersusControl/projects/2).

---

## Shipped (v1.x)

### Multi-channel alerting
- [x] Slack notifications with interactive acknowledgment buttons
- [x] Telegram notifications
- [x] Microsoft Teams (Power Automate workflow + legacy Office 365 webhooks)
- [x] Viber (Channel API + Bot API)
- [x] Email (SMTP + Outlook)
- [x] Lark notifications
- [x] Per-channel proxy support for restricted networks
- [x] Custom Go templates per channel
- [x] Default universal template covering Alertmanager, Grafana, Sentry, CloudWatch, FluentBit
- [x] Multiple destinations per channel via query-parameter overrides
  (`slack_channel_id`, `telegram_chat_id`, `viber_channel_id`,
  `msteams_other_power_url`, `lark_other_webhook_url`, `email_to`, …)

### Queue listeners
- [x] AWS SNS HTTPS endpoint subscription (auto-subscribe)
- [x] AWS SQS polling

### On-call
- [x] AWS Incident Manager integration
- [x] PagerDuty integration (Events API v2)
- [x] Acknowledgment-or-escalate workflow with Redis-backed wait timer
- [x] Per-request on-call overrides (`oncall_enable`, `oncall_wait_minutes`,
  `awsim_other_response_plan`, `pagerduty_other_routing_key`)
- [x] `initialized_only` mode (on-call ready but disabled by default)

### Configuration & deployment
- [x] YAML config with `${ENV_VAR}` expansion
- [x] Per-request config cloning (`GetConfigWitParamsOverwrite`)
- [x] Docker images on GHCR
- [x] Helm chart published to OCI registry
- [x] `/healthz` endpoint
- [x] Resolved-alert short-circuit (skip on-call when `status=resolved`)

### AI SRE Agent — foundations
- [x] Agent enable/disable master switch (`agent.enable`)
- [x] Training / shadow / detect mode selector
- [x] Per-source poll loop with Redis-backed cursors
- [x] **Elasticsearch log source** with `search_after` pagination
- [x] **File log source** with byte-offset cursor sidecar (testing/dev)
- [x] **Redaction pipeline** — JWTs, AWS keys, bearer tokens, emails, UUIDs,
  user agents, custom regex rules; runs before any external AI call
- [x] **User-defined regex rules** (named rules + `default_pattern` catch-all)
- [x] **Drain-style pattern miner** with configurable similarity threshold
  and tree depth
- [x] **Pattern catalog** persisted atomically to `data/patterns.json` with
  EWMA frequency tracking and auto-promote-after-N-sightings
- [x] **Service detection** from log content via configurable patterns
  (`agent.service_patterns`) covering ECS/OTel, logfmt, JSON access logs,
  Java/Spring, syslog/journald
- [x] **New-service grace period** — implicit training window for
  newly-discovered services in shadow/detect modes; admin-controllable
- [x] **Shadow mode** — append-only NDJSON log of would-have-alerted events
- [x] **Pattern management API** — `/api/agent/{status, patterns,
  patterns/:id, flush, shadow, shadow/stats, services,
  services/:name/grace}`, gated by `X-Gateway-Secret`
- [x] **AI analyzer config struct** (OpenAI-compatible: `base_url`,
  `api_key`, `model`, rate limits, cache TTL)
- [x] **User documentation** (`src/agent/*` mdBook pages: introduction,
  configuration, redaction, regex, miner, catalog, shadow-mode)
- [x] **Helm chart support** for agent mode

---

## In progress

These are partially landed; the surfaces exist but the end-to-end behavior
isn't wired.

- [ ] **Unknown error detection (detect mode)** — classifier and shadow
  recording are done; forwarding unknowns to the AI analyzer is the
  remaining piece.
- [ ] **AI error analysis HTTP client** — config struct is in;
  prompt builder + OpenAI-compatible HTTP client land alongside detect-mode
  emission.
- [ ] **AI cost & privacy controls** — redaction is done; per-hour call
  limit and cache TTL enforcement land with the HTTP client.
- [ ] **Incidents from detected issues** — emit AI verdicts through the
  existing alert + on-call pipeline so no extra channel config is needed.
- [ ] **Frequency spike detection** — flag previously-known patterns whose
  rate exceeds historical EWMA baseline.

---

## Planned (v1.4.0 release scope)

- [ ] **Agent web dashboard** — lightweight SPA at `/ui/agent/`
  (status, sortable catalog, shadow viewer, service grace control).
  Same `X-Gateway-Secret` auth.
- [ ] **Reliability & load testing** — graceful degradation on log-source
  outages, AI timeouts, and high log volume.
- [ ] **Security review** — verify no sensitive data leaks into logs or AI
  prompts; admin-endpoint authn/authz audit.
- [ ] **Release v1.4.0** — Docker image + Helm chart + changelog +
  migration notes for existing users.

---

## Future phases (not scheduled)

| Phase | Theme | Why |
|---|---|---|
| 2 | More log sources (Loki, CloudWatch Logs, OpenSearch) | Reach teams not on Elasticsearch |
| 3 | Metrics analysis (Prometheus) | Detect anomalies in numeric time-series |
| 4 | Tracing analysis (Jaeger, Tempo) | Latency outliers and error-rate spikes |
| 5 | Cross-signal correlation | Combine logs + metrics + traces into one root-cause incident |
| 6 | Multiple AI providers (Anthropic, Bedrock, Ollama) + cost optimization | Model choice + spend control |
| — | Prometheus metrics endpoint for Versus itself | Operate the operator |
| — | Multiple template sets per channel | Different templates for different incident classes |
| — | Web UI for incident management (beyond agent dashboard) | Inbox/triage view |
| — | GCP Pub/Sub + Azure Service Bus listeners | Parity with AWS SNS/SQS |

---

## How to influence the roadmap

- File an issue with the use case at
  https://github.com/VersusControl/versus-incident/issues
- Sponsors at the Gold tier and above get a monthly roadmap call. See
  [SPONSORS.md](SPONSORS.md).
- Paid integrations and priority features are available — see the
  "For companies" section of [SPONSORS.md](SPONSORS.md).
