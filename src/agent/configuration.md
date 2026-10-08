# Configuration

This page is the reference for every knob the agent exposes. Pair it with
[Getting Started](./getting-started.md) for a hands-on walkthrough.

Everything lives under the top-level
`agent` key. The list of log sources lives in a sibling file
`agent_sources.yaml` so it can be managed independently per environment.

> **Reminder.** The agent is **off by default** (`agent.enable: false`).
> Nothing about the agent runs until that flag flips.

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
  batch_max: 1000
  signal_max_bytes: 8192
  redaction:   { … }
  catalog:     { … }
  miner:       { … }
  regex:       { … }
```

| Key | Type | Default | Description |
|---|---|---|---|
| `enable` | bool | `false` | Master switch. Env: `AGENT_ENABLE`. |
| `mode` | string | `training` | One of `training` \| `shadow` \| `detect`. Env: `AGENT_MODE`. |
| `poll_interval` | duration | `30s` | How often each source is pulled. Lower = more responsive, higher = less load on your log backend. |
| `lookback` | duration | `5m` | Initial backfill window on first start (when there's no cursor yet). |
| `batch_max` | int | `1000` | Safety cap on signals processed per tick per source. |
| `signal_max_bytes` | int | `8192` | Truncates a single signal's `Raw` payload above this size. |

## Modes

| Mode | What it does | When to use |
|---|---|---|
| `training` | Observes only. New patterns are added to the catalog; nothing else. | First few days. Until the catalog stabilizes. |
| `shadow` | Same as training, but logs `agent[shadow]: would alert …` for any signal it would have alerted on. | A release cycle of review before going live. |
| `detect` | Treats unknown / spiking patterns as anomalies, asks the AI SRE to triage them, and emits a real incident through every configured channel. | Production, after you trust the catalog. |

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

## Miner

Drain-style log clusterer. The defaults work for most setups; tune only
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
| `persist_interval` | duration | `30s` | How often the in-memory catalog is flushed to backend storage. |
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
| `name` | string | Unique identifier. Used in cursor keys and admin views. |
| `type` | string | `file`, `elasticsearch`, `loki`, or `cloudwatchlogs`. |
| `enable` | bool | Per-source switch. |

For per-source field reference and troubleshooting, see the
dedicated [Data Sources](./data-sources.md) guide:

- [File](./data-sources/file.md)
- [Elasticsearch](./data-sources/elasticsearch.md)
- [Loki](./data-sources/loki.md)
- [CloudWatch Logs](./data-sources/cloudwatch-logs.md)

**`max_lines_per_pull` vs `agent.batch_max`**

These two caps look similar but live at different layers and protect
against different things. Both apply on every tick, in this order:

1. **`max_lines_per_pull`** (per-source, file source only). The file
   source stops reading after this many non-empty lines and persists
   its byte offset there. Lines past the cap stay in the file and are
   read on the next tick — nothing is lost.
2. **`agent.batch_max`** (worker-wide, every source). After the source
   returns, the worker truncates the slice to `batch_max` and **drops**
   anything beyond it. The source's cursor has already advanced, so
   dropped signals are gone. This is a backstop for runaway sources,
   not a normal flow-control knob.

The practical rule: keep `max_lines_per_pull ≤ agent.batch_max`. If you
flip them around, the worker's hard truncation kicks in and you lose
signals on every tick.

**Worked example**

Suppose you have a 50,000-line backlog, `poll_interval: 30s`,
`max_lines_per_pull: 1000`, and `agent.batch_max: 1000`:

| Tick | Lines read by file source | After `batch_max` | Cursor advances by | Remaining backlog |
|---|---|---|---|---|
| 1 | 1,000 | 1,000 | 1,000 | 49,000 |
| 2 | 1,000 | 1,000 | 1,000 | 48,000 |
| … | … | … | … | … |
| 50 | 1,000 | 1,000 | 1,000 | 0 |

Total drain time: 50 ticks × 30s ≈ **25 minutes**. Nothing is dropped.

To drain faster, raise `max_lines_per_pull` *and* `agent.batch_max`
together, e.g. both to `5000`:

| Tick | Read | After `batch_max` | Cursor | Remaining |
|---|---|---|---|---|
| 1 | 5,000 | 5,000 | 5,000 | 45,000 |
| … | … | … | … | … |
| 10 | 5,000 | 5,000 | 5,000 | 0 |

Drain time: 10 ticks × 30s ≈ **5 minutes**.

What happens if you only raise one of them? With
`max_lines_per_pull: 5000` but `agent.batch_max: 1000`:

| Tick | Read | After `batch_max` | Cursor | Lost |
|---|---|---|---|---|
| 1 | 5,000 | 1,000 | **5,000** | **4,000** |
| 2 | 5,000 | 1,000 | **5,000** | **4,000** |

The cursor jumps 5,000 lines forward on every tick but only 1,000 are
actually mined — the other 4,000 are silently discarded. Always raise
the two caps together.

## AI

The `agent.ai` block powers both the **detect** triage call and the
on-demand **analyze** investigation. Shared keys apply to both; the
`detect:` and `analyze:` overlays tune each task independently.

```yaml
agent:
  ai:
    enable: true
    provider: openai            # openai | deepseek | qwen | ollama | claude | gemini
    api_key: ${AGENT_AI_API_KEY}
    model: gpt-4o-mini          # shared default for detect + analyze
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
| `model` | string | — | Shared default model for both tasks. Env: `AGENT_AI_MODEL`. |
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
a stronger model with bounded, concurrent tool calls for deep dives:

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
      tool_timeout: 10s         # no single lookup may stall the 2-min budget
```

## Where to next

- [Getting Started](./getting-started.md) — Docker walkthrough.
- [Redaction](./redaction.md) · [Regex](./regex.md) ·
  [Miner](./miner.md) · [Catalog](./catalog.md) — component deep-dives.
