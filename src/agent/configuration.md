# AI Agent — Configuration

This page is the reference for every knob the agent exposes. Pair it with
[Getting Started](./getting-started.md) for a hands-on walkthrough.

The agent reads its configuration from the same `config.yaml` Versus uses
for the rest of its features. Everything lives under the top-level
`agent:` key. The list of log sources lives in a separate file (default
`agent_sources.yaml`) so it can be managed independently.

> **Reminder.** The agent is **off by default** (`agent.enable: false`).
> Nothing about the agent runs until that flag flips.

---

## Top-level keys

```yaml
agent:
  enable: false
  mode: training
  poll_interval: 30s
  lookback: 5m
  batch_max: 1000
  signal_max_bytes: 8192
  gateway_secret: ${AGENT_GATEWAY_SECRET}
  data_dir: ./data
  sources_path: ./agent_sources.yaml
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
| `gateway_secret` | string | (empty) | Shared secret required on `X-Gateway-Secret` header for `/api/agent/*`. **Empty disables the admin endpoints entirely**, which means the agent cannot start. Env: `AGENT_GATEWAY_SECRET`. |
| `data_dir` | path | `./data` | Where the agent persists its catalog and source cursors. |
| `sources_path` | path | (empty) | External YAML file containing the `sources:` list. Resolved relative to the main config. Env: `AGENT_SOURCES_PATH`. |

### Modes

| Mode | What it does | When to use |
|---|---|---|
| `training` | Observes only. New patterns are added to the catalog; nothing else. | First few days. Until the catalog stabilizes. |
| `shadow` | Same as training, but logs `agent[shadow]: would alert …` for any signal it would have alerted on. | A release cycle of review before going live. |
| `detect` | Treats unknown signals as anomalies. AI summarization + incident emission ships in a follow-up milestone — today this mode logs the verdict only. | Production, after you trust the catalog. |

### Environment overrides

| Env var | Maps to |
|---|---|
| `AGENT_ENABLE` | `agent.enable` |
| `AGENT_MODE` | `agent.mode` |
| `AGENT_GATEWAY_SECRET` | `agent.gateway_secret` |
| `AGENT_SOURCES_PATH` | `agent.sources_path` |

---

## `redaction`

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

---

## `catalog`

Long-term storage for learned patterns. See [Catalog](./catalog.md) for
the schema and admin workflows.

```yaml
catalog:
  mode: file
  persist_interval: 30s
  auto_promote_after: 100
```

| Key | Type | Default | Description |
|---|---|---|---|
| `mode` | string | `file` | Storage backend. Only `file` is supported today (planned: `redis`, `database`). |
| `persist_interval` | duration | `30s` | How often the in-memory catalog is flushed to `data_dir/patterns.json`. |
| `auto_promote_after` | int | `100` | A pattern seen this many times in `detect` mode is treated as known (won't alert). `0` disables the promotion. |

The on-disk filename is fixed (`patterns.json`); the only configurable
part is `data_dir`.

---

## `miner`

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

---

## `regex`

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

---

## Signal sources

The list of log sources lives in a separate file referenced by
`agent.sources_path`. Versus reads it at startup, expands `${ENV_VAR}`
references inside it, and replaces `agent.sources` in memory. Keeping
sources separate makes it easy to swap fixtures (local file ↔ ES) and
manage per-environment lists without touching the rest of the config.

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

  - name: prod-app
    type: elasticsearch
    enable: false
    elasticsearch:
      addresses:
        - https://es.prod.example:9200
      username: ${ES_USERNAME}
      password: ${ES_PASSWORD}
      index: "logs-app-*"
      time_field: "@timestamp"
      query: 'log.level:(error OR warn)'
      message_field: message
      page_size: 500
```

Common keys for every source:

| Key | Type | Description |
|---|---|---|
| `name` | string | Unique identifier. Used in cursor keys and admin views. |
| `type` | string | `file` or `elasticsearch`. |
| `enable` | bool | Per-source switch. |

### `file` source

Cheapest way to test the agent end-to-end. One source = one file (no
globs). Tracks position via a sidecar cursor file, so it survives
restarts and handles log rotation (a shrinking file resets to offset 0).

```yaml
file:
  path: /app/logs/my-app.log
  format: text                # "text" or "json"
  from_beginning: true        # replay-like; false tails from EOF
  cursor_path: ""             # default: <data_dir>/cursors/file-<name>.cursor
  max_line_bytes: 65536
  max_lines_per_pull: 1000    # cap signals returned per tick; the rest carries to the next Pull
  timestamp_layout: ""        # Go time layout; empty = auto
  # JSON-mode only:
  message_field: message
  timestamp_field: "@timestamp"
  severity_field: level
```

> When you start the agent against an existing log file with
> `from_beginning: true`, the source paginates the backlog
> `max_lines_per_pull` lines at a time. Each tick advances the byte
> offset only over what was actually consumed, so a multi-million-line
> file is drained safely across many ticks.

#### `max_lines_per_pull` vs `agent.batch_max`

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

##### Worked example

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

### `elasticsearch` source

Reads through the `_search` API with a `range` filter on `time_field`.
Uses `sort` + `search_after` for stable pagination. Authenticates with
HTTP basic auth or an API key.

```yaml
elasticsearch:
  addresses:                  # any number; first that responds wins
    - https://es.prod.example:9200
  username: ${ES_USERNAME}
  password: ${ES_PASSWORD}
  api_key: ""                 # alternative to user/pass
  insecure_skip_verify: false
  index: "logs-app-*"         # supports wildcards
  time_field: "@timestamp"
  query: 'log.level:(error OR warn)'   # Lucene-style query string; "*" = match all
  message_field: message
  severity_field: log.level
  extra_fields:
    - service.name
    - host.name
    - error.stack_trace
  page_size: 500
```

> **Tip on `lookback`.** The agent's first poll uses
> `since = now - lookback`. ES queries with very large historical
> windows may hit a lot of data on the first tick — start with the
> default `5m` and only increase if you need to backfill.

---

## Admin endpoints

All `/api/agent/*` endpoints require the `X-Gateway-Secret` header to
match `agent.gateway_secret`. With no secret configured the endpoints
are not registered and the agent refuses to start.

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/agent/status` | Catalog size, dirty flag, persist-interval, mode. |
| `GET` | `/api/agent/patterns` | All patterns, sorted by sighting count desc. |
| `GET` | `/api/agent/patterns/:id` | One pattern. |
| `POST` | `/api/agent/patterns/:id` | Update `verdict` and/or `tags`. |
| `DELETE` | `/api/agent/patterns/:id` | Remove a pattern from the catalog. |
| `POST` | `/api/agent/flush` | Force-flush the in-memory catalog to disk. |

Example:

```bash
curl -H "X-Gateway-Secret: $AGENT_GATEWAY_SECRET" \
  -H 'Content-Type: application/json' \
  -d '{"verdict":"known","tags":["deploy-rollout","benign"]}' \
  http://localhost:3000/api/agent/patterns/p-abc123
```

---

## Where to next

- [Getting Started](./getting-started.md) — Docker walkthrough.
- [Redaction](./redaction.md) · [Regex](./regex.md) ·
  [Miner](./miner.md) · [Catalog](./catalog.md) — component deep-dives.
