# Changelog

All notable changes to Versus Incident are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/).

---

## [Unreleased]

### Added

#### AI SRE Agent — runbook RAG (`find_runbook`, Phase 3)
- **`find_runbook` tool** (`pkg/agent/ai/analyze/tools/find_runbook.go`)
  — read-only runbook-RAG. During an analysis it embeds a redacted query
  and runs a top-K cosine search over the team's runbook corpus, returning
  the best-matching excerpts so the model grounds its finding in real
  remediation docs. Search-only: no writes, no remediation. The query is
  scrubbed through the analyze redactor **before** the embeddings egress
  (same trust boundary as the chat-completion call).
- **Embedding seam** (`pkg/core.Embedder`,
  `pkg/agent/ai/eino/embedder.go`) — `core.Embedder` leaf interface backed
  by the same Eino/OpenAI model path as the chat model. The embeddings
  endpoint is OpenAI-compatible, so pointing `base_url` at a local server
  (Ollama / vLLM / LocalAI) keeps embeddings inside the operator's own
  network with no code change.
- **In-memory vector index** (`pkg/runbook/vectorindex`) — in-process
  cosine top-K index (no external vector DB); `Index` interface is the seam
  an enterprise hosted backend swaps in without forking the tool.
- **Runbook corpus store + ingestion** (`pkg/runbook`) — blob-backed
  corpus persisted via `storage.Provider` (every record carries an `OrgID`
  for per-tenant scoping). The server auto-ingests the runbook source
  directory of Markdown runbooks (optional YAML front-matter for
  title/services/tags) at boot: it embeds new or edited runbooks and
  persists the vectors it loads into the index. Re-ingest is incremental
  (unchanged runbooks reuse their cached vector), so a restart with no
  edits makes no embedding calls. The `runbook-ingest` CLI
  (`cmd/runbook-ingest`) remains for out-of-band pre-baking (CI / image
  builds). The corpus directory is a fixed path under the storage data
  folder (`./data/runbooks`; `/app/data/runbooks` in the container image).
  The write path lives outside the analyze tools package so the read-only
  import-graph guard stays green.
- **`tools.find_runbook` config** (`tools.yaml`) — `embedding_model` and
  `embedding_base_url` (the embeddings call reuses the shared
  `agent.ai.api_key`). The tool registers only when an embedding model is
  configured **and** a storage backend is available, so the community
  single-tenant build is unchanged by default.
- **Helm** — `agent.tools.findRunbook.*` values + configmap wiring.

### Fixed

#### AI SRE Agent — detect-mode emit deduplication
- **Stop re-emitting an incident every tick for a sustained anomaly**
  (`pkg/agent/dedup.go`, `pkg/agent/worker.go`) — a long-running anomaly
  re-clusters into the same pattern on every poll, and the worker re-sent
  (and re-notified) on each tick — even the cached path still called
  `send()`. A new `DedupStore` gates emission per `(service, pattern)` for
  `agent.emit_dedup_window` (default `1h`; `"0"` disables) — Redis
  `SETNX EX` when available (holds across replicas), with an in-memory
  fallback like the cursor store. Suppressed emits record
  `outcome="deduped"` in the detect log; a failed send releases the window
  so the next tick retries. Config triple-touch + Helm
  (`agent.emitDedupWindow`).

---

## [1.4.3] — 2026-05

### Added

#### AI SRE Agent — multi-agent split (Phase 2)
- **Typed task dispatcher** (`pkg/core/ai_task.go`) — new `AIAgent`
  interface with `Name()`, `Kind()`, `Run(ctx, AITask)` and task kinds
  `detect` / `analyze`. Per-kind cache + rate limiter via
  `pkg/agent/ai/router/router.go`.
- **Eino framework adoption** — `pkg/agent/ai/eino/chatmodel.go` wraps
  the `eino-ext/openai` client as the sole LLM path. Two constructors:
  `NewChatModel` (JSON-mode, detect) and `NewToolCallingChatModel`
  (tool-calling, analyze).
- **DetectAgent relocation** — detect logic moved to
  `pkg/agent/ai/detect/` with its own embedded
  `prompts/{SOUL,INPUTS,OUTPUT,RULES}.md`. Compile-time tool-free guard
  enforces no tools are registered.
- **Shared prompt loader** (`pkg/agent/ai/prompt/loader.go`) —
  content-free `Assemble` / `MustAssemble` used by both agents so
  prompt assembly stays uniform.
- **AnalyzeAgent** (`pkg/agent/ai/analyze/`) — on-demand triage agent
  triggered via `POST /api/admin/incidents/:id/analyze`. Tool-calling
  with read-only tools: `recent_incidents`, `pattern_history`,
  `describe_service`. Own `prompts/` set (triage analyst identity,
  never re-notifies). Max 3 tool iterations (configurable via
  `agent.ai.analyze.max_tool_iterations`). Compile-time Emitter-free
  guard.
- **Analyses storage** — `storage.Provider` extended with
  `SaveAnalysis`, `GetAnalysis`, `ListAnalyses`, `DeleteAnalysis`
  (file + memory backends). Capped at 500 entries (FIFO eviction).
- **Admin endpoints** (gated by `X-Gateway-Secret`):
  - `POST /api/admin/incidents/:id/analyze`
  - `GET /api/admin/incidents/:id/analyses`
  - `GET /api/admin/analyses/:analysis_id`
  - `DELETE /api/admin/analyses/:analysis_id`
- **Per-agent system-prompt endpoint** — `GET /api/agent/ai/system-prompt`
  now accepts `?kind=detect|analyze` (defaults to `detect`). Response
  includes source file list and assembly order.
- **Per-task AI config** — `agent.ai.detect.*` and `agent.ai.analyze.*`
  sub-blocks for model, temperature, max_tokens, max_calls_per_hour,
  cache_ttl overrides.

#### UI
- **Run Analysis** button on the incident detail page (replaces the
  `coming soon` pill on the Analysis card when `ai.enable` is true).
- **AnalysisCard** rendering root-cause hypotheses, evidence list
  (collapsible, source-tagged), next steps, related pattern links,
  and tool-call audit trail.
- **Past analyses** collapsible section listing prior runs (newest
  first) with timestamp, model, and duration; click to expand.

#### Documentation
- New `src/agent/ai-analyze-mode.md` covering on-demand analysis
  configuration, pipeline, admin endpoints, and worked example.
- New data-source pages: `src/agent/data-sources/graylog.md`,
  `src/agent/data-sources/splunk.md`.

#### AI SRE Agent — analyze tools expansion (Phase 2.5)
- **`get_related_logs` tool** — pulls a redacted raw-log slice from
  configured signal sources around the incident window. Bridge via
  `SignalReader` in `pkg/agent/analyze_adapter.go` (no import cycle).
  Window default 15m (cap 1440m), limit default 50 (cap 200).
- **`recent_changes` tool** — reads one or more remote git repositories'
  commit histories to correlate incidents with recent deploys. Repos
  configured in `tools.yaml` (`tools.recent_changes.git.repos[]`). Each
  remote is mirror-cloned into a local cache on first use and fetched on
  later lookups. Global + per-repo auth: HTTPS token via
  `http.extraHeader` (never persisted to the mirror), SSH key via
  `GIT_SSH_COMMAND`. Window default 120m (cap 1440m), newest first.
- **`describe_dependencies` tool** — surfaces upstream/downstream
  service neighbours from the service-dependency graph in `tools.yaml`
  (`tools.describe_dependencies.services`), with a
  `has_recent_incident` flag per neighbour. Reverse edges derived
  automatically from `depends_on`.
- **`tools.yaml` sibling config** — new optional per-tool DATA
  configuration file (same directory as `config.yaml`). Supports
  `${VAR}` expansion. Not a tool allow-list — tools are wired in code.
- **`tool_timeout`** knob (root of `tools.yaml`, default `20s`) — caps
  each tool dispatch; a timeout surfaces as a tool error, never a hard
  failure.
- **`parallel_tools`** knob (root of `tools.yaml`, default `false`) —
  run multiple tool calls in one model turn concurrently while
  preserving deterministic trace ordering.

### Changed
- **Legacy `pkg/agent/ai/openai.go` deleted** — Eino is the only LLM
  path going forward. The `core.AISRE` adapter is removed; all callers
  use `core.AIAgent` via the router.
- **Prompt fragments relocated** — moved from `pkg/agent/ai/prompts/`
  to `pkg/agent/ai/detect/prompts/`. Each agent owns its own fragments.
- `BuildAI` → `BuildAIs` in `pkg/agent/factory_ai.go` — returns
  `AIBundle{Router, Detect, Analyze, Cache, Rate, AnalyzeRate}`.
- `agent.ai.analyze` config block added; `analyze.enable` defaults to
  `true` when `ai.enable` is true (no separate opt-in flag).
- `tool_timeout` and `parallel_tools` moved from `agent.ai.analyze`
  to the root of `tools.yaml` — they apply to every tool dispatch.
- `analyzetools.Default` signature extended to accept `SignalReader`,
  `DependencyGraph`, and `ChangeFeed` dependencies.

### Fixed
- Incident detail page no longer shows stale analysis status after
  triggering a new run.

---

## [1.4.2] — 2026-05

### Added

#### Data sources (AI agent)
- **Graylog** signal source (`pkg/signalsources/graylog.go`) — polls
  `/api/search/universal/absolute` (synchronous, sorted ascending) with
  optional `stream_id`, configurable `query`, `message_field`, and
  extra fields. Auth supports HTTP Basic and the Graylog API-token
  convention (`<token>:token`). Cursor advances on the max message
  timestamp seen; inclusive-`from` duplicates are filtered client-side.
- **Splunk** signal source (`pkg/signalsources/splunk.go`) — streams
  results from `/services/search/v2/jobs/export` (NDJSON). Auth via
  bearer token (preferred) or HTTP Basic. Sub-second epoch
  `earliest_time` / `latest_time`; cursor is the max `_time` seen.
  Search string is auto-prefixed with `search` when missing.
- Agent supports `type: graylog` and `type: splunk` in
  `agent_sources.yaml` alongside the existing sources.

#### Examples & tooling
- New docker-compose examples under
  `examples/docker-compose/{graylog,splunk}/` — fully wired stacks
  (Graylog + MongoDB + OpenSearch; Splunk Enterprise with HEC) plus
  ready-to-use `agent_sources.yaml`.
- `scripts/generate_noisy_logs.py` gains `GraylogSink` (GELF UDP) and
  `SplunkSink` (HEC) plus `--graylog-*` / `--splunk-*` CLI flags
  (env-var aware: `GRAYLOG_HOST`, `SPLUNK_HEC_TOKEN`, …).
- `scripts/run_noisy_logs.sh` adds `graylog` and `splunk` targets.

## [1.4.1] — 2026-05

### Added

#### Team & member management
- Define teams and members through a
  new admin UI + REST API (gated by `X-Gateway-Secret`) and assign them
  to incidents. Members have a name, an editable alias (auto-derived
  from the name in the UI), and a meta block of per-channel identifiers
  (Slack ID, Telegram ID, email, Viber ID, MS Teams UPN, PagerDuty user
  ID, …). Teams have a name, an alias, an optional description, and an
  ordered member list. Persisted via the existing `storage.Provider`
  (new `teams` + `members` blobs). Incident records gain optional
  `assigned_team_id` and `assigned_member_ids` fields. No automatic
  routing yet — that lands in a later phase.
- Enable AI SRE detect make on-call

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
