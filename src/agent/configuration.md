# AI Agent — Configuration

This page is the reference for every knob the agent exposes. Pair it with
[Getting Started](./getting-started.md) for a hands-on walkthrough.

The agent reads its configuration from the same `config.yaml` Versus uses
for the rest of its features. Everything lives under the top-level
`agent:` key. The list of log sources lives in a sibling file
`agent_sources.yaml` so it can be managed independently per environment.

> **Reminder.** The agent is **off by default** (`agent.enable: false`).
> Nothing about the agent runs until that flag flips.

---

## Top-level keys

```yaml
# Root-level (NOT under agent:)
gateway_secret: ${GATEWAY_SECRET}     # shared secret for ALL admin endpoints
storage:
  type: file                          # file | redis | database
  file:
    data_dir: ./data                  # patterns.json + shadow.json + incidents.json live here
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

> **Important.** As of the current release, **`gateway_secret` and the
> storage backend live at the root of the config**, not under `agent:`.
> One secret protects every admin endpoint (`/api/admin/*` and
> `/api/agent/*`); one storage block is shared by the agent's catalog,
> the shadow log, and the incident history shown in the UI. The agent's
> previous `data_dir` field has been removed.

| Key | Type | Default | Description |
|---|---|---|---|
| `enable` | bool | `false` | Master switch. Env: `AGENT_ENABLE`. |
| `mode` | string | `training` | One of `training` \| `shadow` \| `detect`. Env: `AGENT_MODE`. |
| `poll_interval` | duration | `30s` | How often each source is pulled. Lower = more responsive, higher = less load on your log backend. |
| `lookback` | duration | `5m` | Initial backfill window on first start (when there's no cursor yet). |
| `batch_max` | int | `1000` | Safety cap on signals processed per tick per source. |
| `signal_max_bytes` | int | `8192` | Truncates a single signal's `Raw` payload above this size. |

### Modes

| Mode | What it does | When to use |
|---|---|---|
| `training` | Observes only. New patterns are added to the catalog; nothing else. | First few days. Until the catalog stabilizes. |
| `shadow` | Same as training, but logs `agent[shadow]: would alert …` for any signal it would have alerted on. | A release cycle of review before going live. |
| `detect` | Treats unknown / spiking patterns as anomalies, asks the AI SRE to triage them, and emits a real incident through every configured channel. | Production, after you trust the catalog. |

### Environment overrides

| Env var | Maps to |
|---|---|
| `GATEWAY_SECRET` | `gateway_secret` (root) |
| `STORAGE_TYPE` | `storage.type` |
| `STORAGE_FILE_DATA_DIR` | `storage.file.data_dir` |
| `AGENT_ENABLE` | `agent.enable` |
| `AGENT_MODE` | `agent.mode` |
| `AGENT_NEW_SERVICE_GRACE` | `agent.new_service_grace` |
| `AGENT_SERVICE_PATTERNS` | `agent.service_patterns` (comma-separated) |
| `AGENT_AI_ENABLE` | `agent.ai.enable` |
| `AGENT_AI_API_KEY` | `agent.ai.api_key` |
| `AGENT_AI_MODEL` | `agent.ai.model` |

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
  persist_interval: 30s
  auto_promote_after: 100
```

| Key | Type | Default | Description |
|---|---|---|---|
| `persist_interval` | duration | `30s` | How often the in-memory catalog is flushed to `<storage.file.data_dir>/patterns.json`. |
| `auto_promote_after` | int | `100` | A pattern seen this many times in `detect` mode is treated as known (won't alert). `0` disables the promotion. |

The storage backend itself is selected at the **root** of `config.yaml`
(`storage.type`), not here. The on-disk filename is fixed
(`patterns.json`); the only configurable part is the storage `data_dir`
(root-level `storage.file.data_dir`).

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

### `max_lines_per_pull` vs `agent.batch_max`

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

#### Worked example

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

---

## `ai`

The `agent.ai` block powers both the **detect** triage call and the
on-demand **analyze** investigation. Shared keys apply to both; the
`detect:` and `analyze:` overlays tune each task independently.

```yaml
agent:
  ai:
    enable: true
    api_key: ${AGENT_AI_API_KEY}
    model: gpt-4o-mini          # shared default for detect + analyze
    temperature: 0.2
    max_tokens: 1024
    base_url: ""                # optional OpenAI-compatible endpoint

    analyze:
      model: gpt-4o             # stronger model for deep dives
```

> The tool-loop knobs `tool_timeout` and `parallel_tools` are **not** set
> here — they live at the root of `tools.yaml` (see *Per-tool config*
> below) because they apply to every analyze tool dispatch.

| Key | Type | Default | Description |
|---|---|---|---|
| `enable` | bool | `false` | Turns on the AI SRE (detect triage + analyze). Env: `AGENT_AI_ENABLE`. |
| `api_key` | string | — | API key for the model provider. Env: `AGENT_AI_API_KEY`. |
| `model` | string | — | Shared default model for both tasks. Env: `AGENT_AI_MODEL`. |
| `analyze.model` | string | inherits `model` | Optional stronger model just for analyze. |

> The tool-loop knobs `tool_timeout` and `parallel_tools` moved to the
> root of `tools.yaml` (see below) — they apply to every analyze tool
> dispatch, not just one task.

> **No tool allow-list.** There is no `analyze.tools` key. Every tool
> wired in `analyzetools.Default(...)` is available to the analyze
> agent. Two tools are conditionally registered: `describe_dependencies`
> only when a service-dependency graph is configured in `tools.yaml`
> (`tools.describe_dependencies.services`), and `recent_changes` only
> when at least one git repository is configured in `tools.yaml`
> (`tools.recent_changes.git.repos`).

### Per-tool config (`tools.yaml`)

Some analyze tools read external data. That **data** configuration lives
in an optional `tools.yaml` file sitting next to your main config — it is
loaded automatically when present and supports `${VAR}` expansion.
`tools.yaml` is per-tool *data* config, **not** a registration allow-list
(tools are still wired in code via `analyzetools.Default`). The root of
`tools.yaml` also carries the shared tool-loop knobs.

```yaml
tools:
  tool_timeout: 20s         # per-tool dispatch cap (applies to every tool)
  parallel_tools: false     # run tool calls in one model turn concurrently

  recent_changes:
    git:
      auth:                 # global default auth; per-repo auth overrides it
        token: ${GIT_TOKEN}   # HTTPS PAT (empty = ambient credentials)
        ssh_key_path: ""      # SSH key path for ssh/scp-like remotes
      repos:                # one or more remote git repositories
        - url: https://github.com/acme/api.git   # remote clone URL (required)
          branch: main                           # optional; empty = default HEAD
          service: api                           # optional; empty = derived from URL
        - url: git@github.com:acme/web.git        # service auto-detected as "web"
          auth:                                  # optional; overrides git.auth
            ssh_key_path: /home/versus/.ssh/web_deploy
  describe_dependencies:
    services:               # optional service-dependency graph
      - name: web
        depends_on:
          - api
      - name: api
        depends_on:
          - database
          - cache
```

| Key | Type | Default | Description |
|---|---|---|---|
| `tool_timeout` | duration | `20s` | Per-tool dispatch cap. Empty, `0`, or an unparseable value inherits the `20s` default. A timeout surfaces as a tool error in the analysis trail, never a hard failure. |
| `parallel_tools` | bool | `false` | When the model emits several tool calls in one turn, run them concurrently instead of sequentially. The audit trail stays deterministically ordered either way. |

### Service-dependency graph (`describe_dependencies`)

The `describe_dependencies` tool returns, for a given service, its
upstream and downstream neighbours, each flagged with whether that
neighbour also has a recent incident — helping the model reason about
cascading failures. Author the graph under
`tools.describe_dependencies.services` in `tools.yaml`: each entry has a
`name` and a `depends_on` list of upstream services. Only `depends_on`
(upstream edges) is authored by hand; the reverse edges
(`depended_on_by`, downstream consumers) are derived automatically. An
empty `services` list leaves the tool unregistered.

### Change feed source (`recent_changes`)

The `recent_changes` tool reads one or more **remote git repositories'
commit histories** — the deploy/change record most teams already keep —
to line an incident up against recent deploys and config changes. It uses
**go-git** (pure Go) — no external `git` binary is required. Each
configured repository is bare-cloned into a local cache on first use and
fetched on subsequent lookups, so the feed stays fresh on its own.

Configure the repositories via `tools.yaml`
(`tools.recent_changes.git.repos`). Each entry has a remote `url` (https
or scp-like `git@host:org/repo`), an optional `branch`, and an optional
`service`. When `service` is omitted it is auto-detected from the
repository name in the URL (e.g. `git@github.com:acme/web.git` → `web`).
With an empty `repos` list the tool is simply not registered.

**Authentication.** Private remotes authenticate via an optional `auth`
block. A global default under `tools.recent_changes.git.auth` applies to
every repo, and any repo may override it field-by-field with its own
`auth` (empty per-repo fields fall back to the global default; both empty
= ambient git credentials / SSH agent). For HTTPS remotes set
`auth.token` (a personal access token) — it is sent as an `Authorization`
header and never written to the local clone. For SSH remotes set
`auth.ssh_key_path` (passed through `GIT_SSH_COMMAND`). Each commit in the
query window maps to a change record:

| Field | Source |
|---|---|
| `timestamp` | Committer date (RFC3339). |
| `service` | The repository's configured `service`, or the name auto-detected from its URL. Used by the optional service filter. |
| `kind` | Always `commit`. |
| `summary` | Commit subject line. |
| `ref` | Short (7-char) commit SHA. |

A missing window or no commits is a clean "nothing found", so the analysis
proceeds without it.

#### Worked example

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
      parallel_tools: true      # fan out multi-tool turns for faster analyses
```

With `parallel_tools: true`, an analyze turn that calls
`recent_incidents`, `describe_service`, and `describe_dependencies`
together runs all three at once; each is still individually capped at
`10s` by `tool_timeout`.

---

## Admin UI

Everything the agent records is reviewed through the admin UI,
served at the agent's address (e.g. `http://localhost:3000`). Sign
in with the gateway secret (`gateway_secret` / `GATEWAY_SECRET`).
With no secret configured the agent refuses to start.

| Page | What it shows |
|---|---|
| **Status** | Catalog size, current mode, and source health at a glance. |
| **Patterns** | Every learned pattern, sorted by sighting count. Open a pattern to set its verdict (`known`) and tags, or remove it. |
| **Shadow** | Would-have-alerted entries to review in shadow mode. |
| **Detect** | The AI's triage calls and outcomes in detect mode. |
| **Services** | Discovered services and their grace state. |
| **Incidents** | Incidents emitted by the agent, with the on-demand analyze action. |

---

## Where to next

- [Getting Started](./getting-started.md) — Docker walkthrough.
- [Redaction](./redaction.md) · [Regex](./regex.md) ·
  [Miner](./miner.md) · [Catalog](./catalog.md) — component deep-dives.
