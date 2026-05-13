# Changelog

All notable changes to Versus Incident are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/).

---

## [1.4.0] — 2026-05

### Added

#### AI SRE Agent — detect mode (end-to-end)
- Detect-mode AI pipeline: unknown / spike patterns are forwarded to the
  AI SRE; findings emit incidents through the standard
  `services.CreateIncident` pipeline so all configured channels and the
  on-call workflow trigger unchanged.
- OpenAI HTTP client (`pkg/agent/ai/openai.go`) using plain `net/http`
  against `/v1/chat/completions` with `response_format: json_object`.
  No SDK dependency.
- Multi-file system prompt embedded via `go:embed` from
  `pkg/agent/ai/prompts/{SOUL,INPUTS,OUTPUT,RULES}.md`. Operators tune
  the prompt by editing Markdown — no Go changes required.
- AI cost & privacy controls: per-hour `RateLimiter`, `ResultCache` with
  TTL eviction, redaction pipeline runs before any AI call.
- **Detect-mode audit log** — bounded ring buffer (cap 500) persisted as
  the `detect` storage blob; captures every AI call with prompt, raw
  response, finding, outcome, latency, and model.
- New admin endpoints (gated by `X-Gateway-Secret`):
  - `GET /api/agent/detect`, `/detect/stats`, `/detect/:id`
  - `DELETE /api/agent/detect`, `POST /api/agent/detect/flush`
  - `GET /api/agent/ai/system-prompt`
- Frequency spike detection: known patterns whose rate exceeds the EWMA
  baseline are routed to the AI SRE alongside unknowns.

#### Data sources (AI agent)
- **Loki** signal source (`pkg/signalsources/loki.go`) — polls
  `/loki/api/v1/query_range` with stream-label filtering, configurable
  `query` + `step`, basic-auth and `X-Scope-OrgID` for multi-tenant
  deployments. Full test coverage.
- **CloudWatch Logs** signal source
  (`pkg/signalsources/cloudwatchlogs.go`) — pulls events via the AWS
  SDK v2 with `--log-group-name` and optional `--log-stream-name-prefix`
  / `--filter-pattern`. Full test coverage.
- Agent now supports `type: loki` and `type: cloudwatchlogs` in
  `agent_sources.yaml` alongside the existing `file` and
  `elasticsearch` sources.

#### UI
- New **Detect** page (table + outcome filters) and detail page (prompt,
  raw response, finding).
- New **System Prompt** page rendering the assembled system prompt.
- Dashboard **AI Detect** tile and chart bar (replaces the prior
  Services tile).
- **Incident detail page** redesigned: structured Summary / Suggestions
  / Sample log / Raw payload column on the left; Facts / Channels
  notified / Status / Agent context column on the right. Same layout
  for every incident (manual, webhook, or agent-emitted).
- Two AI action cards at the top of the detail page — **Analysis** and
  **Auto Post Mortem** — each with an explanation and a
  `coming soon` pill. Reserved for future AI features.
- **Status page** now a proper agent dashboard: runtime banner (agent
  on/off, mode, source counts, AI model, poll interval), two tile rows
  (patterns/services/shadow + detect/emitted/cache/errors), four
  breakdown tables (shadow verdicts, detect outcomes, detect verdicts,
  AI severity), and a signal-sources table read from
  `agent_sources.yaml`.

#### Notification templates
- All 5 channel templates (Slack, Telegram, MS Teams, Lark, Viber) now
  detect `Versus Agent` source via `.PatternID` and render an
  agent-native block (verdict, category, frequency, baseline,
  confidence, pattern, suggestions, sample log) in channel-native
  formatting.
- Split the prior shared `agent_message.tmpl` into six per-channel
  templates (`agent_slack_message.tmpl`, `agent_telegram_message.tmpl`,
  `agent_msteams_message.tmpl`, `agent_lark_message.tmpl`,
  `agent_viber_message.tmpl`, `agent_email_message.tmpl`) so each
  channel renders in its native formatting.
- `pkg/utils/agent_template.go` exposes `IsAgentIncident()` and the
  six template-path constants; alert dispatch picks the agent template
  automatically when `.PatternID` is set or `.Source` starts with
  `agent:`.

#### Examples
- Four self-contained docker-compose stacks under
  `examples/docker-compose/`:
  - `file/` — tails a host log file
  - `loki/` — Grafana + Loki + Promtail + Versus
  - `elasticsearch/` — single-node ES + Versus
  - `cloudwatch/` — CloudWatch Logs poller + Versus
- Each stack ships its own `docker-compose.yml`,
  `config/{config.yaml,agent_sources.yaml}`, and `README.md` with
  test-traffic instructions. Compose files use `${VAR:-default}` so
  `docker compose up` works zero-config.
- All stacks run **Redis with TLS** via a one-shot
  `redis-tls-init` container that mints a self-signed cert into a
  shared volume — matches the app's unconditional TLS Redis config.

#### Test scripts
- `scripts/generate_noisy_logs.py` and `scripts/run_noisy_logs.sh` now
  support `--scenario` with 7 curated incident scenarios:
  `db-outage`, `cache-meltdown`, `disk-full`, `tls-expired`,
  `oom-cascade`, `auth-attack`, `k8s-imagepull`.
- `scripts/generate_noisy_logs.py` rewritten with pluggable sinks:
  `--target {stdout,loki,elasticsearch,cloudwatch}`. Stdlib only;
  `boto3` is lazy-imported for the CloudWatch target.
- `scripts/run_noisy_logs.sh` accepts the same `--target` flag with
  per-backend env vars (`LOKI_URL`, `ES_URL`, `CW_LOG_GROUP_NAME`, …).

#### Documentation
- New `src/agent/ai-detect-mode.md` covering configuration, pipeline
  outcomes, admin endpoints, system prompt anatomy, worked example, and
  cost knobs.
- New **Data Sources** mdBook section
  (`src/agent/data-sources.md`) covering every source type with
  configuration, polling semantics, and end-to-end examples.
- `src/userguide/admin-ui.md` updated for the new detail/status pages.
- `SUMMARY.md` updated with the new entries.

#### CI / release
- `release.yaml` now builds **multi-platform** images
  (`linux/amd64` + `linux/arm64`) using QEMU + Buildx.
- CI guards `go vet` against an empty `ui/dist/` by re-creating
  `ui/dist/.gitkeep` before compilation (the `go:embed all:dist`
  directive needs at least one entry).

### Changed
- `core.AISRE.Analyze` now returns `*AICallResult` (finding + user
  prompt + raw response + latency + model) instead of `*AIFinding`.
  External implementations of `AISRE` must be updated.
- OpenAI endpoint is now hardcoded to
  `https://api.openai.com/v1/chat/completions`.
- `.env.example` files removed from the docker-compose examples —
  the `${VAR:-default}` defaults in compose files cover every knob
  and keep the quick-start to a single command.

### Fixed
- Channel notifications for AI-emitted incidents previously rendered as
  "Unknown Alert (Unknown) / INFO". All 5 templates now correctly
  display the agent verdict, severity, and metadata.
- **Pattern catalog** — race / correctness regressions in the catalog,
  alert fan-out, and on-call status handling.
- **Storage** — fsync the staged file before the atomic rename in the
  file-backed provider so a crash mid-write can no longer leave a
  zero-byte `incidents.json`.

### Security
- Detect-mode audit log redacts samples through the same redactor used
  before AI calls; raw payloads never reach the on-disk log unredacted.
- `gateway_secret` continues to gate every `/api/agent/*` endpoint;
  empty secret means the admin endpoints are not registered at all
  (no silent open admin surface).
- **Constant-time gateway-secret comparison** in
  `pkg/controllers/agent.go` — replaces the previous `==` check, which
  was vulnerable to timing-based secret discovery on the
  `/api/agent/*` and `/api/admin/*` endpoints.

---

## [1.4.x]

See git history and previous Helm chart releases. Migration notes:
[`src/migration/migration-v1.4.0.md`](src/migration/migration-v1.4.0.md).
