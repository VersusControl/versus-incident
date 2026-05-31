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

### AI SRE Agent — Phase 1: Detector
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

### AI SRE Agent — Phase 2: Multi-agent split (Detector + Analyzer)
- [x] **E1 — Typed task dispatcher** (`core.AIAgent`): replace the
  one-method `core.AISRE` with a task-kind dispatcher (`detect` /
  `analyze`), per-kind cache + rate limiter via a new `Router`
- [x] **E2 — Eino framework adoption**: add Eino chat-model layer behind
  `core.AIAgent` as the sole LLM path (no `framework` knob — Eino is the
  implementation); per-task sub-configs `agent.ai.detect.*` and
  `agent.ai.analyze.*`
- [x] **E3 — DetectAgent relocation**: move detect logic into
  `pkg/agent/ai/detect/`; compile-time tool-free guard; delete legacy
  `pkg/agent/ai/openai.go`
- [x] **E4 — AnalyzeAgent (backend)**: new on-demand agent + read-only
  Eino tools (`recent_incidents`, `pattern_history`,
  `describe_service`); compile-time
  Emitter-free guard; new `analyses` storage blob; admin endpoints
  `POST /api/admin/incidents/:id/analyze`,
  `GET /api/admin/incidents/:id/analyses`,
  `GET /api/admin/analyses/:id`, `DELETE /api/admin/analyses/:id`
- [x] **E5 — AnalyzeAgent UI wiring**: wire the `Analysis` `AiActionCard`
  on the incident detail page to the new endpoint; render root-cause
  hypotheses, evidence list, next steps; collapsible past-analyses
  history
- [x] **E6 — Docs & ADRs**: ADR 0009 (multi-agent split);
  `architecture.md` AI-subsystem diagram update;
  `config-schema.md` for the new per-task keys; user-guide page for
  on-demand analysis

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
- [x] **Graylog signal source** (`pkg/signalsources/graylog.go`) — polls
  `/api/search/universal/absolute` (synchronous, sorted ascending) with
  optional `stream_id`, configurable `query`, `message_field`, and extra
  fields. Auth supports HTTP Basic and the Graylog API-token convention
  (`<token>:token`). Cursor advances on the max message timestamp seen;
  inclusive-`from` duplicates filtered client-side.
- [x] **Splunk signal source** (`pkg/signalsources/splunk.go`) — streams
  results from `/services/search/v2/jobs/export` (NDJSON). Auth via
  bearer token (preferred) or HTTP Basic. Sub-second epoch
  `earliest_time` / `latest_time`; cursor is the max `_time` seen;
  search auto-prefixed with `search` when missing.
- [x] **Docker-compose examples** — fully wired stacks under
  `examples/docker-compose/{graylog,splunk}/` (Graylog + MongoDB +
  OpenSearch; Splunk Enterprise with HEC) plus ready-to-use
  `agent_sources.yaml`.
- [x] **Noisy-log generator extensions** — `scripts/generate_noisy_logs.py`
  gains `GraylogSink` (GELF UDP) and `SplunkSink` (HEC) with
  `--graylog-*` / `--splunk-*` flags; `scripts/run_noisy_logs.sh` adds
  `graylog` and `splunk` targets.

## Planned (v1.4.3 release scope)
- [x] **Multi-agent split (Phase 2)** — restructured the AI subsystem
  from one tool-using analyzer into two specialised agents coordinated
  by a typed task dispatcher:
  - **Typed task dispatcher** — `core.AIAgent` interface + `Router`
    with per-task cache and rate limiter
  - **Eino framework** — sole LLM path via `eino-ext/openai`; per-task
    sub-configs `agent.ai.detect.*` / `agent.ai.analyze.*`
  - **DetectAgent** relocated to `pkg/agent/ai/detect/` with own prompt
    fragments; compile-time tool-free guard
  - **AnalyzeAgent** (`pkg/agent/ai/analyze/`) — on-demand triage with
    read-only tools (`recent_incidents`, `pattern_history`,
    `describe_service`); own prompt fragments; Emitter-free guard
  - **Analyses storage** — CRUD via `storage.Provider` (file + memory)
  - **Admin endpoints** — `POST /analyze`, `GET /analyses`,
    `GET /analyses/:id`, `DELETE /analyses/:id`
  - **Per-agent system-prompt endpoint** — `?kind=detect|analyze`
  - **Shared prompt loader** — `pkg/agent/ai/prompt/loader.go`
  - **UI** — Run Analysis button, AnalysisCard, past analyses list
  - Legacy `pkg/agent/ai/openai.go` deleted
- [x] **Analyze toolset expansion (Phase 2.5)** — grew the AnalyzeAgent
  from 3 foundational tools to 6, plus tool-loop infrastructure:
  - `get_related_logs` — redacted raw-log slice via `SignalReader` bridge
  - `recent_changes` — remote git commit history feed with per-repo +
    global auth (HTTPS token / SSH key); configured in `tools.yaml`
  - `describe_dependencies` — upstream/downstream service graph from
    `tools.yaml` with `has_recent_incident` flag
  - `tools.yaml` sibling config file (per-tool DATA, not allow-list)
  - `tool_timeout` (default 20s) + `parallel_tools` knobs at
    `tools.yaml` root
  - Docs: `ai-analyze-mode.md` per-tool YAML, `configuration.md`
    worked examples

---

## Future phases

Phases land when the prior phase's success criteria hold up under real
soak. Track Phase 2 work on the
[GitHub Project](https://github.com/orgs/VersusControl/projects/2).

> The `Auto Post Mortem` UI card stays a `coming soon` stub until
> Phase 3 lands.

> Metrics & traces are **out of scope** for Phase 2.5 — Versus has no
> metric ingestion path today. A metric-lookup analyze tool waits on
> Phase 3 (metrics/traces sources).

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
