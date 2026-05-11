## Migration Guide to v1.4.0

## Table of Contents
- [Key Changes in v1.4.0](#key-changes-in-v140)
  - [1. AI Detect Mode is now end-to-end](#1-ai-detect-mode-is-now-end-to-end)
  - [2. Removed: `agent.ai.base_url`](#2-removed-agentaibase_url)
  - [3. New admin endpoints (gated by `gateway_secret`)](#3-new-admin-endpoints-gated-by-gateway_secret)
  - [4. Notification templates updated](#4-notification-templates-updated)
  - [5. `core.AISRE.Analyze` signature change (Go integrators only)](#5-coreaisreanalyze-signature-change-go-integrators-only)
- [How to Migrate from v1.3.x](#how-to-migrate-from-v13x)
- [Upgrading](#upgrading)

This guide explains the changes introduced in Versus Incident v1.4.0
and how to update your configuration. Most existing deployments need
**no config changes** — the AI agent stays opt-in and defaults to
`agent.enable: false`.

### Key Changes in v1.4.0

#### 1. AI Detect Mode is now end-to-end

In v1.3.x, the agent could observe and shadow-log unknown patterns but
could not emit incidents from AI verdicts. In v1.4.0, switching
`agent.mode: detect` now:

1. Forwards unknown / spike patterns to the configured LLM
   (`agent.ai.enable: true`).
2. Caches and rate-limits the AI calls.
3. Emits the resulting finding as a normal incident through
   `services.CreateIncident` — meaning **all your existing channels
   (Slack, Telegram, MS Teams, Lark, Viber, Email) and the on-call
   workflow trigger unchanged**. No new fan-out logic.

Every AI call is also persisted to a bounded audit log
(`<storage.data_dir>/detect.json`, capped at 500 events) and viewable
in the UI under **Agent → Detect**.

See the new guide: [AI Detect Mode](../agent/ai-detect-mode.md).

#### 2. Removed: `agent.ai.base_url`

The OpenAI endpoint is now hardcoded to
`https://api.openai.com/v1/chat/completions`. The `base_url` field and
`AGENT_AI_BASE_URL` env var are no longer recognised.

**Action:** if your `config.yaml` contains `agent.ai.base_url`, remove
the line. The server logs an unused-key warning at startup but does not
fail. A future minor release may treat unknown keys as fatal.

```diff
 agent:
   ai:
     enable: true
-    base_url: https://api.openai.com/v1
     api_key: ${OPENAI_API_KEY}
     model: gpt-4o-mini
```

Multi-LLM support (Anthropic, Bedrock, Ollama, OpenAI-compatible
gateways) is on the [roadmap](../../ROADMAP.md) under Phase 7.

#### 3. New admin endpoints (gated by `gateway_secret`)

| Method | Path | Purpose |
|--------|------|---------|
| GET    | `/api/agent/detect`              | List detect-mode AI calls |
| GET    | `/api/agent/detect/stats`        | Aggregate counts |
| GET    | `/api/agent/detect/:id`          | Single call: prompts + raw response + finding |
| DELETE | `/api/agent/detect`              | Clear detect log |
| POST   | `/api/agent/detect/flush`        | Force-persist detect log |
| GET    | `/api/agent/ai/system-prompt`    | Read assembled system prompt |

All require the `X-Gateway-Secret` header to match the root-level
`gateway_secret`. **An empty secret means the admin endpoints are not
registered at all** — no silent open admin surface.

#### 4. Notification templates updated

If you use the **shipped** templates (`config/{slack,telegram,msteams,
lark,viber}_message.tmpl`), no action is required — they now correctly
render AI-emitted incidents with verdict, category, frequency,
confidence, suggestions, and sample log.

If you have **forked** any of these templates, port the new
`Versus Agent` source detection block. Look for the `if eq .Source
"Versus Agent"` branch in the upstream files.

#### 5. `core.AISRE.Analyze` signature change (Go integrators only)

This only matters if you have a custom implementation of the
`core.AISRE` interface in your fork.

```diff
 type AISRE interface {
-    Analyze(ctx context.Context, r AgentResult) (*AIFinding, error)
+    Analyze(ctx context.Context, r AgentResult) (*AICallResult, error)
 }
```

`AICallResult` wraps the finding plus the prompt sent, the raw response
received, the model name, and the call duration. The detect-mode audit
log uses these fields. Update your implementation to populate them
(empty strings are tolerated for non-OpenAI backends).

### How to Migrate from v1.3.x

Most users do not need to change anything. The agent is opt-in and
disabled by default.

If you were already running the agent in `training` or `shadow` mode:

1. Keep your existing `agent.*` config and `data/patterns.json`.
2. Remove `agent.ai.base_url` if present (see §2).
3. To enable detect mode, set `agent.mode: detect` and provide an
   OpenAI API key:

   ```yaml
   agent:
     enable: true
     mode: detect
     ai:
       enable: true
       api_key: ${OPENAI_API_KEY}
       model: gpt-4o-mini
       max_calls_per_hour: 30
       cache_ttl: 1h
   gateway_secret: ${GATEWAY_SECRET}
   ```

4. Verify the audit trail at
   `GET /api/agent/detect` (with `X-Gateway-Secret` header) or in the
   UI under **Agent → Detect**.

If you fork the channel templates, see §4.

### Upgrading

```bash
# Docker
docker pull ghcr.io/versuscontrol/versus-incident:1.4.0

# Helm
helm repo update
helm upgrade versus-incident oci://ghcr.io/versuscontrol/charts/versus-incident \
  --version 1.4.0
```

Restart the service to apply the changes. Existing pattern catalogs
and shadow logs are forward-compatible (no schema migration).

For any issues with the migration, please
[open an issue](https://github.com/VersusControl/versus-incident/issues).
