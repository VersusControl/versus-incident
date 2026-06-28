# AI Agent — Analyze Tools

This page covers the **configuration** required by analyze tools that
read external data, plus a worked Docker example.

Most analyze tools need no configuration — they read state the agent
already keeps (incidents, learned patterns, redacted logs) and work out
of the box.

## Available tools

### `recent_incidents`

Lists incidents recorded in a recent time window so the AI can spot
correlated failures, repeat offenders, or a broader outage in progress.

- **Time window** — defaults to the last 60 minutes, up to a maximum of
  1440 minutes (24 hours).
- **Service filter** — optionally narrows the list to a single service.
- **Limit** — returns up to 20 incidents by default, capped at 100.

Use case: *"Are other services failing at the same time, or is this
incident isolated?"*

### `pattern_history`

Looks up a learned pattern by its id and returns everything the agent
knows about it: the log template, the EWMA frequency baseline, the
operator-set verdict, tags, observation counts, and the associated
service.

Use case: *"Is this a brand-new pattern, or a known issue that has
spiked above its normal baseline?"*

### `describe_service`

Summarises a single service: when it was first seen by the agent and
its top learned patterns ranked by frequency.

Use case: *"What does normal look like for this service, and which
patterns dominate its logs?"*

### `get_related_logs`

Fetches a redacted slice of recent raw log lines so the AI can read the
actual signals around an incident instead of reasoning from summaries
alone. Output is scrubbed by the same redactor used everywhere else, so
secrets and PII never reach the model.

- **Source / service filters** — optionally narrow to a single signal
  source or service.
- **Time window** — defaults to the last 15 minutes, up to a maximum of
  1440 minutes (24 hours).
- **Limit** — returns up to 50 lines by default, capped at 200.

Use case: *"What were the surrounding log lines saying right before this
incident fired?"*

## Configuration tools

Two tools read **external** data and require a config block:

| Tool | Config block | Purpose | Detailed guide |
|---|---|---|---|
| `describe_dependencies` | `tools.describe_dependencies.services` | Service-dependency graph | *(below)* |
| `recent_changes` | `tools.recent_changes.git` | Remote git commit history feed | [recent_changes](./recent-changes.md) |
| `find_runbook` | `tools.find_runbook` | Runbook-RAG vector search over your remediation docs | [find_runbook](./find-runbook.md) |

That configuration lives in an optional **`tools.yaml`** file placed next
to your main `config.yaml`. The root of `tools.yaml` also carries two shared tool-loop knobs that
apply to **every** tool dispatch:

| Knob | Default | Description |
|---|---|---|
| `tool_timeout` | `20s` | Caps a single tool dispatch so one slow lookup can't consume the 2-minute analysis budget. A timeout surfaces as a tool error, never a hard failure. |
| `parallel_tools` | `false` | When the model emits several tool calls in one turn, run them concurrently instead of sequentially. The audit trail stays deterministically ordered either way. |

### `describe_dependencies`

This tool maps service relationships (upstream /
downstream) so the AI can reason about blast radius and root cause
propagation. Example: if `api` depends on `database` and the database is
failing, the AI can infer that API errors are likely downstream
consequences rather than a separate incident.

Author the service-dependency graph under
`tools.describe_dependencies.services`. Each entry has a `name` and a
`depends_on` list of upstream services. Reverse (downstream) edges are
derived automatically. With an empty `services` list the tool is not
registered.

```yaml
tools:
  describe_dependencies:
    services:
      - name: web
        depends_on:
          - api
      - name: api
        depends_on:
          - database
          - cache
      - name: worker
        depends_on:
          - database
          - queue
```

### `recent_changes`

This tool reads commit histories from your deploy repositories so the AI
can correlate an incident with a recent deploy or config change. Example:
a spike in errors appeared 5 minutes after a commit "migrate users table"
landed in the `api` repo — the AI can flag that deploy as the probable
trigger.

Configure your repositories under `tools.recent_changes.git.repos`. With
an empty `repos` list the tool is not registered.

```yaml
tools:
  recent_changes:
    git:
      repos:
        - url: https://github.com/acme/api.git
          branch: main
          service: api
```

> **See the full guide:** [`recent_changes` Tool](./recent-changes.md)
> covers arguments, the change-record shape, authentication (HTTPS tokens
> and SSH keys), failure behavior, and a Docker example.

### `find_runbook`

This tool grounds the analysis in **your team's own runbooks**. During an
investigation it embeds a short query derived from the incident, runs a
top-K similarity search over a corpus of Markdown runbooks the server
ingests for you, and returns the best-matching excerpts so the model can
cite real remediation steps instead of inventing them. It is
**search-only** — it never executes any remediation, triggers on-call, or
sends a notification.

The tool registers only when an embedding model is configured **and** a
storage backend is available, so the default community build is
unaffected.

```yaml
tools:
  find_runbook:
    embedding_model: text-embedding-3-small
    embedding_base_url: ""     # empty = OpenAI default; set a local server to keep embeddings in-network
```

Then place your `*.md` runbooks in the data folder under `runbooks/`
(`./data/runbooks`). The server auto-ingests them at boot.

> **See the full guide:** [`find_runbook` Tool](./find-runbook.md)
> covers arguments, runbook front-matter, the security/redaction posture,
> pre-baking the corpus with `runbook-ingest`, and managing runbooks from
> the admin UI.

## Complete `tools.yaml` example

A production-ready `tools.yaml` combining both external tools and the
shared knobs:

```yaml
tools:
  tool_timeout: 15s
  parallel_tools: true

  recent_changes:
    git:
      auth:
        token: ${GIT_TOKEN}
      repos:
        - url: https://github.com/acme/api.git
          branch: main
          service: api
        - url: https://github.com/acme/web.git
          service: web
        - url: git@github.com:acme/infra.git
          service: infra
          auth:
            ssh_key_path: /keys/infra_deploy

  describe_dependencies:
    services:
      - name: web
        depends_on: [api]
      - name: api
        depends_on: [database, cache]
      - name: worker
        depends_on: [database, queue]

  find_runbook:
    embedding_model: text-embedding-3-small
    embedding_base_url: ""
```

## Running with Docker

Mount `tools.yaml` next to your `config.yaml` and pass secrets via
environment variables. If using SSH keys for `recent_changes`, also mount
the key file.

```bash
docker run -d --name versus-incident \
  -p 3000:3000 \
  -e GATEWAY_SECRET=my-secret \
  -e AGENT_ENABLE=true \
  -e AGENT_MODE=detect \
  -e AGENT_AI_ENABLE=true \
  -e AGENT_AI_API_KEY=sk-... \
  -e GIT_TOKEN=ghp_xxxxxxxxxxxx \
  -v ./config:/app/config \
  -v ./data:/app/data \
  -v ~/.ssh/web_deploy:/keys/web_deploy:ro \
  ghcr.io/versuscontrol/versus-incident:latest
```

Your `./config/` directory should contain:

```
config/
├── config.yaml
├── agent_sources.yaml
└── tools.yaml              # ← analyze tool config
```

### Docker Compose

```yaml
services:
  versus:
    image: ghcr.io/versuscontrol/versus-incident:latest
    ports:
      - "3000:3000"
    environment:
      GATEWAY_SECRET: ${GATEWAY_SECRET}
      AGENT_ENABLE: "true"
      AGENT_MODE: detect
      AGENT_AI_ENABLE: "true"
      AGENT_AI_API_KEY: ${AGENT_AI_API_KEY}
      GIT_TOKEN: ${GIT_TOKEN}
    volumes:
      - ./config:/app/config
      - ./data:/app/data
      - ./keys/web_deploy:/keys/web_deploy:ro
```

## More tools

The tool interface is intentionally pluggable. Areas we're exploring for
future tools include:

- **Metric & dashboard lookups** — pull the relevant time-series
  (error rate, latency, saturation) around the incident window.

As new tools ship they will appear automatically in the **Tool calls**
audit trail, and no configuration change is required to benefit from
them.
