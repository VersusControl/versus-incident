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
- [x] **Pluggable storage backend** (`storage.Provider`) shared by the agent
  catalog, shadow log, and incident history; root-level `storage:` block
  with `file` backend (production) and `redis` / `database` config stubs
- [x] **Root-level `gateway_secret`** protecting all admin endpoints
  (`/api/admin/*` and `/api/agent/*`) via the `X-Gateway-Secret` header

### Incident management UI
- [x] React + Vite + Tailwind SPA shipped under `ui/`
- [x] Persistent incident history (`POST /api/incidents` writes a record;
  `GET /api/ack/:id` stamps `acked_at`)
- [x] **Admin endpoints** `GET /api/admin/incidents` (list) and
  `GET /api/admin/incidents/:id` (full detail), gated by `X-Gateway-Secret`
- [x] **Incidents page** (search + Open/Acked/Resolved filter) and detail
  page (payload, channels notified, on-call status, timeline)

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
- [x] **AI analyzer config struct** (OpenAI: `api_key`, `model`, rate
  limits, cache TTL)
- [x] **User documentation** (`src/agent/*` mdBook pages: introduction,
  configuration, redaction, regex, miner, catalog, shadow-mode,
  ai-detect-mode)
- [x] **Helm chart support** for agent mode

### AI SRE Agent — detect mode
- [x] **Detect-mode AI pipeline** — unknown / spike patterns forwarded to
  the AI SRE; findings emitted as incidents through the standard
  `services.CreateIncident` pipeline (all channels + on-call)
- [x] **OpenAI HTTP client** (`pkg/agent/ai/openai.go`) — plain `net/http`
  against `/v1/chat/completions`; structured JSON response format;
  no SDK dependency
- [x] **Multi-file system prompt** (`pkg/agent/ai/prompts/`) — SOUL,
  INPUTS, OUTPUT, RULES Markdown files embedded via `go:embed` and
  concatenated at build time; operator-tunable without Go changes
- [x] **AI cost & privacy controls** — per-hour rate limiter
  (`pkg/agent/ai/rate.go`), result cache with TTL eviction
  (`pkg/agent/ai/cache.go`), redaction pipeline runs before any AI call
- [x] **Detect-mode audit log** — bounded ring buffer (`DetectLog`, cap
  500) persisted as `detect` storage blob; captures every AI call with
  prompt, raw response, finding, outcome, latency, model
- [x] **Detect admin endpoints** — `GET /api/agent/detect` (list),
  `/detect/stats`, `/detect/:id`, `DELETE /detect`, `POST /detect/flush`,
  `GET /api/agent/ai/system-prompt`
- [x] **Agent web dashboard** — Detect page (table + outcome filters),
  detail page (prompt + raw response + finding), system-prompt page,
  Dashboard AI Detect tile + chart bar
- [x] **Notification templates for agent incidents** — all 5 channel
  templates (Slack, Telegram, MS Teams, Lark, Viber) detect `Versus Agent`
  source and render verdict, category, frequency, confidence, suggestions,
  sample log in channel-native formatting
- [x] **Frequency spike detection** — flag previously-known patterns whose
  rate exceeds historical EWMA baseline; spikes routed to AI SRE alongside
  unknowns
- [x] **Test scenario scripts** — `--scenario` flag for
  `generate_noisy_logs.py` / `run_noisy_logs.sh` with 7 curated incident
  scenarios (db-outage, cache-meltdown, disk-full, tls-expired,
  oom-cascade, auth-attack, k8s-imagepull)

---

## Planned (v1.4.0 release scope)

- [x] **Security review** — verified no sensitive data leaks into logs or
  AI prompts; admin-endpoint authn/authz audited. Redaction runs before
  every AI call and before persisting to the detect log; all
  `/api/admin/*` and `/api/agent/*` endpoints gated by
  `X-Gateway-Secret`; empty `gateway_secret` leaves admin routes
  unregistered (no silent open surface); Redis enforces TLS 1.2 minimum
  unless `insecure_skip_verify` is explicitly enabled.
- [x] **Release v1.4.0** — Docker image + Helm chart bumped to 1.4.0 +
  [CHANGELOG.md](CHANGELOG.md) + migration notes
  ([`src/migration/migration-v1.4.0.md`](src/migration/migration-v1.4.0.md)).

## Planned (v1.4.1 release scope)
- [x] **Team & member management** — define teams and members through a
  new admin UI + REST API (gated by `X-Gateway-Secret`) and assign them
  to incidents. Members have a name, an editable alias (auto-derived
  from the name in the UI), and a meta block of per-channel identifiers
  (Slack ID, Telegram ID, email, Viber ID, MS Teams UPN, PagerDuty user
  ID, …). Teams have a name, an alias, an optional description, and an
  ordered member list. Persisted via the existing `storage.Provider`
  (new `teams` + `members` blobs). Incident records gain optional
  `assigned_team_id` and `assigned_member_ids` fields. No automatic
  routing yet — that lands in a later phase.

## Planned (v1.4.2 release scope)
- [ ] **Reliability & load testing** — graceful degradation on log-source
  outages, AI timeouts, and high log volume.

---

## Future phases (not scheduled)

Phases land when the prior phase's success criteria hold up under real
soak. See `local/plans/ai-incident-detection/sre-agent-roadmap.md` for
full descriptions and ADR references.

### AI SRE Agent — Phase 2: Triage with read-only tools
- [ ] **Eino agent framework** — replace plain HTTP analyzer with
  `ChatModelAgent` (Eino); current `openai.go` becomes the `openai-direct`
  opt-out path
- [ ] **`get_related_logs` tool** — query the same SignalSource backwards
  from the alert time for surrounding context
- [ ] **`get_recent_incidents` tool** — read from local `storage.Provider`
  incident history
- [ ] **`get_pattern_history` tool** — return EWMA / count trend for the
  pattern
- [ ] **`describe_service` tool** — operator-curated YAML from
  `config/services.yaml` (owner, runbook URL, tier)
- [ ] **Evidence list in findings** — `AIFinding.Evidence []EvidenceItem`
  surfaced in the incident message ("I looked at X and saw Y")

### AI SRE Agent — Phase 3: Metrics & traces sources
- [ ] **Prometheus signal source** — pull rule-based queries, normalize
  into `Signal`
- [ ] **OTLP / Tempo / Jaeger signal source** — error spans + latency
  outliers
- [ ] **Additional log sources** — Loki, CloudWatch Logs, OpenSearch

### AI SRE Agent — Phase 4: Cross-signal correlation
- [ ] **`VerdictCorrelated`** — log + metric + trace anomalies for the
  same service within a sliding window collapse into a single incident
- [ ] **Windowed dedup** — Redis-backed `service.name + 5m bucket`
  evaluated before AI emission

### AI SRE Agent — Phase 5: Runbook RAG
- [ ] **Local runbook indexing** — Markdown runbooks in `config/runbooks/`,
  embedded with a local model (default `bge-small-en-v1.5` via Ollama)
- [ ] **`find_runbook` tool** — semantic search over runbooks; high-confidence
  match appears directly in the incident

### AI SRE Agent — Phase 6: Suggested remediation
- [ ] **Text-only suggested actions** — for known-good categories (restart,
  scale up), AI suggests the exact command gated by an operator allow-list
- [ ] **No auto-execution** — suggestions are text only; human approval
  required

### AI SRE Agent — Phase 7: Multi-LLM + cost optimization
- [ ] **Model router** — cheaper model for verdict refinement, stronger
  model for final summary
- [ ] **Per-team / per-source budgets**
- [ ] **Self-hosted-only enforcement** (`ai.disallow_external: true`)

### Platform
| Item | Why |
|---|---|
| Prometheus metrics endpoint for Versus itself | Operate the operator |
| Multiple template sets per channel | Different templates for different incident classes |
| GCP Pub/Sub + Azure Service Bus listeners | Parity with AWS SNS/SQS |

---

## How to influence the roadmap

- File an issue with the use case at
  https://github.com/VersusControl/versus-incident/issues
- Sponsors at the Gold tier and above get a monthly roadmap call. See
  [SPONSORS.md](SPONSORS.md).
- Paid integrations and priority features are available — see the
  "For companies" section of [SPONSORS.md](SPONSORS.md).
