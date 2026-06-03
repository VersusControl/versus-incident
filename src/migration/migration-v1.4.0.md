# Migrating to v1.4.0

This guide explains the changes introduced in Versus Incident v1.4.0
and how to update your configuration. Most existing deployments need
**no config changes** — the AI agent stays opt-in and defaults to
`agent.enable: false`.

## Key Changes in v1.4.0

### 1. AI Detect Mode is now end-to-end

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

### 2. Removed: `agent.ai.base_url`

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

### 3. New admin endpoints (gated by `gateway_secret`)

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

### 4. Notification templates updated

If you use the **shipped** templates (`config/{slack,telegram,msteams,
lark,viber}_message.tmpl`), no action is required — they now correctly
render AI-emitted incidents with verdict, category, frequency,
confidence, suggestions, and sample log.

If you have **forked** any of these templates, port the new
`Versus Agent` source detection block. Look for the `if eq .Source
"Versus Agent"` branch in the upstream files.

### 5. `core.AISRE.Analyze` signature change (Go integrators only)

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

## How to Migrate from v1.3.x

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

## Upgrading

```bash
# Docker
docker pull ghcr.io/versuscontrol/versus-incident:v1.4.0-beta

# Helm
helm repo update
helm upgrade versus-incident oci://ghcr.io/versuscontrol/charts/versus-incident \
  --version 1.4.0
```

Restart the service to apply the changes. Existing pattern catalogs
and shadow logs are forward-compatible (no schema migration).

## Helm chart changes (1.3.x → 1.4.0)

Chart `1.4.0` adds first-class support for everything that v1.4.0
ships in the binary. Key additions to `values.yaml`:

```yaml
# NEW — required for the dashboard and every /api/admin/* and
# /api/agent/* endpoint. Empty value leaves admin routes unregistered.
gatewaySecret: ""

# NEW — pluggable storage backend (only `file` is implemented today).
# Persist the data dir so incident history and the agent catalog
# survive pod restarts.
storage:
  type: file
  file:
    dataDir: /app/data
    maxIncidents: 1000
  persistence:
    enabled: false
    size: 1Gi
    accessMode: ReadWriteOnce
    # existingClaim: my-pvc

# NEW — opt-in AI SRE Agent (training | shadow | detect).
agent:
  enable: false
  mode: training
  pollInterval: 30s
  newServiceGrace: 30m
  ai:
    enable: false
    apiKey: ""              # stored in the chart Secret
    model: "gpt-4o-mini"
    maxCallsPerHour: 60
    cacheTtl: "1h"
  sources: []               # see helm.md for examples
```

The chart also adds pre-flight validation that **fails the render**
in three previously-silent misconfigurations:

- Unknown `storage.type`.
- `storage.persistence.enabled=true` with `accessMode: ReadWriteOnce`
  and `replicaCount > 1` (the PVC cannot be mounted on multiple pods).
- `agent.enable=true` with `replicaCount > 1` (the agent worker is
  single-writer to the catalog and detect log).

If you previously ran multiple replicas with the agent or persistence
hand-rolled, drop replicas to `1` and disable autoscaling before
upgrading:

```yaml
replicaCount: 1
autoscaling:
  enabled: false
```

If you set `gatewaySecret` to an empty value, the dashboard and admin
endpoints are not registered. Generate a strong value at install:

```bash
helm upgrade --install versus-incident \
  oci://ghcr.io/versuscontrol/charts/versus-incident \
  --version 1.4.0 \
  -f values.yaml \
  --set gatewaySecret="$(openssl rand -hex 32)" \
  --set agent.ai.apiKey="$OPENAI_API_KEY"
```

For the full chart guide including agent setup, see
[`src/configuration/helm.md`](../configuration/helm.md).

For any issues with the migration, please
[open an issue](https://github.com/VersusControl/versus-incident/issues).
