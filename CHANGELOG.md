# Changelog

All notable changes to Versus Incident are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/).

---

## [1.4.1] — Unreleased

### Fixed

#### Correctness & data integrity (regressions in v1.4.0 and earlier)
- **Catalog data race** (`pkg/agent/catalog.go`): `Catalog.Get` returned
  a live `*Pattern` pointer that callers (`worker.classify`) read under
  no lock while a concurrent `Upsert` from another source's tick wrote
  to the same struct. `Get` now returns a deep copy.
- **Alert fan-out short-circuit** (`pkg/core/alert.go`): the legacy
  `Alert.SendAlert` returned on the first provider error and never
  invoked subsequent providers — a flaky Slack silently muted Telegram
  and Email. Added `SendAllAlerts` that tries every configured channel
  and returns per-channel success/failure. `services.CreateIncident`
  now uses it and stores `ChannelsNotified` as the channels that
  actually succeeded (vs. `ChannelsEnabled` for what was configured).
- **On-call status lie** (`pkg/services/incident.go`): `OnCallTriggered`
  was persisted at record-build time, before the workflow was started.
  If `workflow.Start` later failed, the UI still reported "on-call
  triggered" while no one was paged. The flag is now walked back to
  `false` and `OnCallError` is recorded when `Start` returns an error.
- **AI rate-limit drain under breaker-open** (`pkg/agent/worker.go`):
  the rate limiter incremented its hour counter BEFORE the circuit
  breaker check, so an open breaker still consumed quota. Breaker
  check now runs first.

#### Resilience hardening
- **Retry sleep ignored context cancellation** (`pkg/agent/ai/openai.go`):
  `time.Sleep` between retries blocked SIGTERM for up to ~15s at default
  settings. Replaced with `select { ctx.Done(); time.After }`.
- **`rand.Rand` not goroutine-safe** (`pkg/agent/ai/openai.go`):
  per-instance `*rand.Rand` was shared across goroutines that race
  inside `Analyze`. Switched to the package-level `rand.Int63n` (Go
  1.20+ guarantees concurrent safety on the global) and removed the
  field.
- **`rand.Int63n(0)` panic** (`pkg/agent/ai/openai.go`): could panic if
  the operator configured an extremely small `initial_backoff`. Guarded
  the call.
- **Worker died silently on panic** (`pkg/agent/worker.go`): a panic
  in `tickSource` killed its goroutine; the cursor never advanced, no
  log line surfaced, and the agent looked healthy. Added a
  `defer recover()` that logs the stack and records the failure on
  the source health tracker.

#### Security
- **Constant-time gateway secret comparison** (`pkg/controllers/*`):
  the three admin controllers used `got != expected`. Switched to
  `crypto/subtle.ConstantTimeCompare` so prefix-match timing oracles
  are eliminated.

#### Operational limits
- **Unbounded pattern catalog** (`pkg/agent/catalog.go`): added
  `agent.catalog.max_patterns` (default 10000). When the cap is hit,
  the least-frequent NON-"known" pattern is evicted. Operator-curated
  patterns (`verdict: known`) are preserved. 0 disables the cap.
- **Storage atomicity** (`pkg/storage/file.go`): `os.WriteFile` +
  `os.Rename` did not `fsync` between write and rename. A power loss
  in between could replace a previous good `patterns.json` /
  `incidents.json` with a zero-length file. Added `f.Sync()` via a
  new `writeFileAtomicSync` helper used for every persisted blob.

### Added

#### Multi-provider AI support
- **`agent.ai.base_url` config field** (`pkg/agent/ai/openai.go`): override
  the OpenAI chat/completions endpoint to point at any OpenAI-compatible
  provider — e.g. Google Gemini's `https://generativelanguage.googleapis.com/v1beta/openai/chat/completions`,
  LiteLLM proxies, OpenRouter. Wire format remains OpenAI chat/completions.
  Default unchanged (OpenAI), so existing deployments keep working.
- **`finish_reason` handling + truncation auto-retry**
  (`pkg/agent/ai/openai.go`): the analyzer now reads each choice's
  `finish_reason`. On `length` (model hit max_tokens mid-JSON) it
  auto-retries once with max_tokens doubled (capped at 4096), which
  recovers Gemini-2.5-flash's verbose output without operator
  intervention. On `content_filter` it surfaces a clear error without
  retrying (more tokens won't unblock a safety filter). Caught
  end-to-end during Gemini live tests where the original 512-token
  budget kept truncating JSON.
- **Raw response in parse-error message** (`pkg/agent/ai/openai.go`):
  when ParseFinding fails, the first 300 chars of the model's actual
  reply are now included in the error string, so the detect audit
  log shows operators *why* parsing failed instead of just
  "no JSON object found".

#### Reliability & graceful degradation
- **Per-source health tracker** (`pkg/agent/health.go`): consecutive
  failures, last error, last success, cooldown end, lifetime pull /
  signal / drop counters, last pull duration. Exposed under
  `GET /api/agent/status` as `sources: [...]`.
- **Exponential backoff cooldown** for failing sources. After the first
  consecutive failure the source enters a `source_backoff_initial`
  cooldown that doubles each subsequent failure up to
  `source_backoff_max`. Pulls during cooldown are skipped (logged) so a
  flapping backend cannot stall the worker.
- **Per-pull context deadline** (`reliability.pull_timeout`, default
  20s) so a hung backend can't run past the tick budget.
- **Silent-truncation counter**: when `batch_max` truncates a tick,
  the dropped count is recorded and surfaced under
  `total_signals_dropped`. Previously the truncation only logged.
- **OpenAI retry with jitter** (`pkg/agent/ai/openai.go`): HTTP 429 and
  5xx now retry up to `ai_retry.max_attempts` (default 3) with
  exponential backoff. 4xx other than 429 surface immediately.
- **Circuit breaker for AI calls** (`pkg/agent/ai/breaker.go`):
  consecutive failures above `ai_breaker.failure_threshold` (default 5)
  flip the breaker open; calls short-circuit for `ai_breaker.cooldown`
  (default 2m), then one probe is allowed; success closes, failure
  re-opens with a fresh cooldown. Surfaced in the detect audit log as
  outcome `breaker_open`.
- **AI health under `/api/agent/status`** as `ai: {state,
  consecutive_failures, total_success, total_failure, total_opens,
  total_probes, last_error, last_error_at, last_success_at, opened_at,
  latency_p50_ms, latency_p95_ms}`. Latencies are computed over a
  rolling window of the last 100 successful calls.

### Configuration
- New `agent.reliability` block (see `config/config.yaml`).
  All fields have sensible defaults; the block is optional.

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

#### UI
- New **Detect** page (table + outcome filters) and detail page (prompt,
  raw response, finding).
- New **System Prompt** page rendering the assembled system prompt.
- Dashboard **AI Detect** tile and chart bar (replaces the prior
  Services tile).

#### Notification templates
- All 5 channel templates (Slack, Telegram, MS Teams, Lark, Viber) now
  detect `Versus Agent` source via `.PatternID` and render an
  agent-native block (verdict, category, frequency, baseline,
  confidence, pattern, suggestions, sample log) in channel-native
  formatting.

#### Test scripts
- `scripts/generate_noisy_logs.py` and `scripts/run_noisy_logs.sh` now
  support `--scenario` with 7 curated incident scenarios:
  `db-outage`, `cache-meltdown`, `disk-full`, `tls-expired`,
  `oom-cascade`, `auth-attack`, `k8s-imagepull`.

#### Documentation
- New `src/agent/ai-detect-mode.md` covering configuration, pipeline
  outcomes, admin endpoints, system prompt anatomy, worked example, and
  cost knobs.
- `SUMMARY.md` updated with the new entry.

### Changed
- `core.AISRE.Analyze` now returns `*AICallResult` (finding + user
  prompt + raw response + latency + model) instead of `*AIFinding`.
  External implementations of `AISRE` must be updated.
- OpenAI endpoint is now hardcoded to
  `https://api.openai.com/v1/chat/completions`.

### Fixed
- Channel notifications for AI-emitted incidents previously rendered as
  "Unknown Alert (Unknown) / INFO". All 5 templates now correctly
  display the agent verdict, severity, and metadata.

### Security
- Detect-mode audit log redacts samples through the same redactor used
  before AI calls; raw payloads never reach the on-disk log unredacted.
- `gateway_secret` continues to gate every `/api/agent/*` endpoint;
  empty secret means the admin endpoints are not registered at all
  (no silent open admin surface).

---

## [1.4.x]

See git history and previous Helm chart releases. Migration notes:
[`src/migration/migration-v1.4.0.md`](src/migration/migration-v1.4.0.md).
