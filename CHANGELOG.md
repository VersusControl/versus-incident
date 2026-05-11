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
