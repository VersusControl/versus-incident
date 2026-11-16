# Configuration

This page is the reference for every knob the agent exposes. Pair it with
the [AIOps SRE Quickstart](./quickstart.md) for a hands-on walkthrough.

Everything lives under the top-level
`agent` key. The list of log sources lives in a sibling file
`agent_sources.yaml` so it can be managed independently per environment.

## Top Level

```yaml
# Root-level (NOT under agent:)
gateway_secret: ${GATEWAY_SECRET}     # shared secret for ALL admin endpoints
storage:
  type: file                          # file | redis | database
  file:
    max_incidents: 1000

agent:
  enable: false
  mode: training
  poll_interval: 30s
  lookback: 5m
  batch_max: 5000
  new_service_grace: 24h
  service_patterns: [ … ]
  redaction:   { … }
  catalog:     { … }
  miner:       { … }
  regex:       { … }
```

| Key | Type | Default | Description |
|---|---|---|---|
| `enable` | bool | `false` | Master switch. Env: `AGENT_ENABLE`. |
| `mode` | string | `training` | One of `training` \| `shadow` \| `detect`. Env: `AGENT_MODE`. |
| `poll_interval` | duration | `30s` | How often each source is read. Lower = more responsive, higher = less load on your log backend. |
| `lookback` | duration | `5m` | How far back a source is read the first time the agent starts it. |
| `batch_max` | int | `5000` | How many lines the agent processes at a time. Every line read is processed; a larger read is handled in several chunks of this size. |
| `new_service_grace` | duration | `24h` (`30m` in the Helm chart) | How long a service seen for the first time is learned quietly before it can raise incidents or shadow entries. Env: `AGENT_NEW_SERVICE_GRACE`. |
| `service_patterns` | regex list | 10 built-in patterns | How the service name is read from a log line; the first match wins. Leave the key out to use the built-in patterns; `[]` turns detection off and every line lands in `_unknown`. Env: `AGENT_SERVICE_PATTERNS` (comma-separated). See [Service Detection](./service-detection.md). |

## Modes

| Mode | What it does | When to use |
|---|---|---|
| `training` | Learns your normal patterns. Sends nothing. | The first few days, until new patterns stop appearing often. See [Training Mode](./training-mode.md). |
| `shadow` | Keeps learning and records what it would have alerted on in **AI → Decisions → Shadow**. Sends nothing. | A release cycle of review before going live. See [Shadow Mode](./shadow-mode.md). |
| `detect` | Raises a real incident for each new pattern or spike. Works without an AI key; with `agent.ai` set up, the AI writes the summary, severity and next steps. | Production, after you trust what it learned. See [AI Detect Mode](./ai-detect-mode.md). |

## Environment overrides

| Env var | Maps to |
|---|---|
| `GATEWAY_SECRET` | `gateway_secret` (root) |
| `STORAGE_TYPE` | `storage.type` |
| `AGENT_ENABLE` | `agent.enable` |
| `AGENT_MODE` | `agent.mode` |
| `AGENT_NEW_SERVICE_GRACE` | `agent.new_service_grace` |
| `AGENT_SERVICE_PATTERNS` | `agent.service_patterns` (comma-separated) |
| `AGENT_AI_ENABLE` | `agent.ai.enable` |
| `AGENT_AI_API_KEY` | `agent.ai.api_key` |
| `AGENT_AI_MODEL` | `agent.ai.model` |
| `AGENT_AI_PROVIDER` | `agent.ai.provider` |
| `AGENT_AI_BASE_URL` | `agent.ai.base_url` |

## Miner

Groups similar log lines into patterns. The defaults work for most setups; tune only
if you see related lines failing to merge into one template (lower
`similarity_threshold`) or unrelated lines collapsing together (raise
it). See [Miner](./miner.md).

```yaml
miner:
  similarity_threshold: 0.4
  tree_depth: 4
  max_children: 100
```

| Key | Type | Default | Description |
|---|---|---|---|
| `similarity_threshold` | float (0–1) | `0.4` | Token-overlap ratio required to consider two messages part of the same template. |
| `tree_depth` | int | `4` | Depth of the prefix tree used to bucket templates by length and leading tokens. |
| `max_children` | int | `100` | Per-node fan-out cap to keep the tree bounded. |

## Redaction

Pattern-based scrubbing of secrets and PII before any other component
sees them. Always enable this in production. See [Redaction](./redaction.md)
for the full default rule list and how to extend it.

```yaml
redaction:
  enable: true
  redact_ips: false
  extra_patterns:
    - "(?i)password=\\S+"
    - "Authorization:\\s*Bearer\\s+\\S+"
```

| Key | Type | Default | Description |
|---|---|---|---|
| `enable` | bool | `true` (when `agent.enable: true`) | Master switch for redaction. |
| `redact_ips` | bool | `false` | Opt-in IPv4/IPv6 redaction. Off by default because IPs are usually useful context. |
| `extra_patterns` | string list | `[]` | Additional Go regexes. Invalid patterns are skipped (logged at startup), so one typo can't disable redaction. |

## Catalog

Long-term storage for learned patterns. See [Catalog](./catalog.md) for
the schema and admin workflows.

```yaml
catalog:
  persist_interval: 30s
  auto_promote_after: 100
```

| Key | Type | Default | Description |
|---|---|---|---|
| `persist_interval` | duration | `30s` | How often learned patterns are saved to storage. |
| `auto_promote_after` | int | `100` | Number of sightings before a log pattern is auto-promoted to *known*. Default `100`. Any value `<= 0` (or a blank/unset key) is treated as the default `100`. To keep a pattern out of *known*, label/curate it instead. |

The storage backend itself is selected at the **root** of `config.yaml`
(`storage.type`), not here. The on-disk filename is fixed
(`patterns.json`), written to the fixed `./data` directory
(`/app/data` in the container image).

## Regex

Pre-filter and tagger. Only signals whose message matches at least one
rule (named or default) are forwarded to the miner — everything else is
dropped before clustering. See [Regex](./regex.md) for cookbook recipes.

```yaml
regex:
  default_pattern: "(?i).*error.*"
  rules:
    - name: oom-killer
      pattern: "Out of memory: Killed process"
    - name: panic
      pattern: "(?i)panic:"
    - name: 5xx-burst
      pattern: "HTTP/[0-9.]+\\s+5\\d\\d"
```

| Key | Type | Default | Description |
|---|---|---|---|
| `default_pattern` | regex | `""` | Catch-all tried after every named rule misses. Empty = nothing matches by default → strict mode. `".*"` = learn from every line. |
| `rules` | list | `[]` | Named rules. First match wins. Each rule has `name` and `pattern`. The matched `name` is stored on the pattern as `rule_name` so you can cross-reference shadow events back to the rule that flagged them. |

Common recipes:

| Goal | Setting |
|---|---|
| Learn everything (training, broad scope) | `default_pattern: ".*"` |
| Only learn explicit rule matches (strict) | `default_pattern: ""` plus full `rules:` list |
| Learn only error-ish lines (default) | `default_pattern: "(?i).*error.*"` |

## Signal sources

The list of log sources lives in a sibling file `agent_sources.yaml`
sitting next to your main `config.yaml`. The file is optional and,
when present, REPLACES any inline `agent.sources` from the main
config. Versus expands `${ENV_VAR}` references inside the file at
load time.

```yaml
# config/agent_sources.yaml
sources:
  - name: my-app
    type: file
    enable: true
    file:
      path: /var/log/my-app/app.log
      format: text
      from_beginning: false   # tail-like behavior
```

Common keys for every source:

| Key | Type | Description |
|---|---|---|
| `name` | string | Unique name for the source, shown in the UI. Keep it stable: the agent uses it to remember where it stopped reading. |
| `type` | string | `file`, `elasticsearch`, `loki`, `cloudwatchlogs`, `graylog`, `splunk` or `signoz`. _Enterprise_ adds metric and trace sources. |
| `enable` | bool | Per-source switch. |

For per-source field reference and troubleshooting, see the
dedicated [Data Sources](./data-sources.md) guide:

- [File](./data-sources/file.md): lines longer than `file.max_line_bytes` (default 64 KiB) are cut to that length.
- [Elasticsearch](./data-sources/elasticsearch.md)
- [Loki](./data-sources/loki.md)
- [CloudWatch Logs](./data-sources/cloudwatch-logs.md)
- [Graylog](./data-sources/graylog.md)
- [Splunk](./data-sources/splunk.md)
- [SigNoz](./data-sources/signoz.md)

**`max_lines_per_pull`** (file source only, default `1000`)

The file source stops reading after this many lines on each read. The rest
stay in the file and are read next time; nothing is lost. Raise it to work
through a backlog faster: with a 50,000-line backlog and `poll_interval: 30s`,
`1000` takes about 25 minutes and `5000` about 5 minutes.

## AI

The `agent.ai` block powers both the **detect** triage call and the
on-demand **analyze** investigation. Shared keys apply to both; the
`detect:` and `analyze:` overlays tune each task independently.

Example (the defaults are in the table below):

```yaml
agent:
  ai:
    enable: true
    provider: openai            # openai | deepseek | qwen | ollama | claude | gemini
    api_key: ${AGENT_AI_API_KEY}
    model: gpt-4o-mini          # shared model for detect + analyze
    temperature: 0.2
    max_tokens: 1024
    base_url: ""                # optional OpenAI-compatible endpoint

    analyze:
      model: gpt-4o             # stronger model for deep dives
```

**Note**: For beta-limited / reasoning models (`gpt-5.*`, o-series), set
`temperature: -1` to omit the parameter entirely; those providers fix
temperature at `1` and reject any explicit value.

| Key | Type | Default | Description |
|---|---|---|---|
| `enable` | bool | `false` | Turns on the AI SRE (detect triage + analyze). Env: `AGENT_AI_ENABLE`. |
| `provider` | string | `openai` | Model backend: `openai`, `deepseek`, `qwen`, `ollama`, `claude`, or `gemini`. An unknown value fails fast (no silent fallback). Env: `AGENT_AI_PROVIDER`. |
| `base_url` | string | `""` | Custom OpenAI-compatible chat-completions base URL. When nonempty, takes precedence over `provider` for Detect, Chat, and Analyze. Env: `AGENT_AI_BASE_URL`; an empty env value restores provider selection. |
| `api_key` | string | — | API key for the model provider. Env: `AGENT_AI_API_KEY`. |
| `model` | string | `gpt-5.4-mini` | Shared default model for both tasks. Env: `AGENT_AI_MODEL`. |
| `max_tokens` | int | `4096` | Longest response the model may write. |
| `max_calls_per_hour` | int | `60` | Cap on detect AI calls. Past the cap, incidents are still raised with the templated summary. |
| `cache_ttl` | duration | `1h` | The same pattern firing again within this time reuses the last AI finding. |
| `temperature` | float | `0.2` | Randomness control. Set `-1` to omit the field for beta-limited / reasoning models that reject explicit temperature values. |
| `analyze.model` | string | inherits `model` | Optional stronger model just for analyze. |

For a custom server, configure its API base including any required prefix:

```yaml
agent:
  ai:
    enable: true
    base_url: https://models.example.com/v1
    api_key: ${AGENT_AI_API_KEY}
    model: custom-model
```

**Worked example**

A production setup using a cheap model for high-volume detect triage and
a stronger model for deep dives:

```yaml
agent:
  enable: true
  mode: detect
  ai:
    enable: true
    api_key: ${AGENT_AI_API_KEY}
    model: gpt-4o-mini          # cheap, fast — used for every detect call
    analyze:
      model: gpt-4o             # reserved for the on-demand analyze action
```

How long a single tool lookup may run during an analysis (`tool_timeout`) is
set in `tools.yaml`, not here. See the [Tool Reference](./tools/overview.md).

## Where to next

- [AIOps SRE Quickstart](./quickstart.md) — Docker Compose walkthrough.
- [Training Mode](./training-mode.md) · [Shadow Mode](./shadow-mode.md) ·
  [AI Detect Mode](./ai-detect-mode.md) · [Spike Detection](./spike.md) — the modes.
- [Redaction](./redaction.md) · [Regex](./regex.md) ·
  [Miner](./miner.md) · [Catalog](./catalog.md) — component deep-dives.
