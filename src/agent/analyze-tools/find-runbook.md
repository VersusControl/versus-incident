# Find Runbook Tool

Your team already wrote down how to fix things. The runbooks live in a
repo, a wiki, a folder somewhere — and during an incident, nobody can
find the right one fast enough. The `find_runbook` tool puts those
runbooks in front of the analyze agent so its findings cite *your* real
remediation steps instead of generic advice the model invented.

During an investigation the agent embeds a short query derived from the
incident, runs a similarity search over a corpus of Markdown runbooks you
provide, and returns the best-matching excerpts. When an incident looks
like connection-pool exhaustion on `api`, the agent can pull your
"Postgres connection pool exhausted" runbook and ground its analysis in
the exact `SELECT` and `pg_terminate_backend` steps your team trusts.

> **Warning:** This tool is search-only. It reads and ranks runbooks. It
> never executes a remediation, triggers on-call, or sends a
> notification. The agent reads your runbook; a human still runs it.

## When the tool is available

`find_runbook` registers only when **both** conditions hold:

1. An embedding model is configured (`tools.find_runbook.embedding_model`).
2. A storage backend is available to persist the embedded corpus.

With no embedding model the tool is omitted and analyses proceed without
runbook grounding — so the default community build is unaffected. You can
still upload and manage runbooks from the admin UI before configuring
embeddings; they become searchable once an embedding model is set and the
corpus is re-ingested.

> **Note:** `find_runbook` also requires AI to be enabled
> (`AGENT_AI_ENABLE=true`) — it runs inside the
> [AI Analyze mode](../ai-analyze-mode.md). The same
> `agent.ai.api_key` is reused for the embeddings call.

## What the agent sees

The agent calls the tool with a natural-language query and gets back
ranked runbook matches:

| Argument | Type | Default | Notes |
|---|---|---|---|
| `query` | string | *(required)* | Natural-language description of the problem, e.g. `"postgres connection pool exhausted on the api service"`. |
| `service` | string | *(all)* | Restrict matches to one service (case-insensitive exact match). |
| `limit` | integer | `5` | Cap the number of matches returned. Capped at `20`. |

Each match carries the runbook's `id`, `title`, `service`, a similarity
`score`, and a bounded `excerpt` of the body so the agent can read the
relevant steps without pulling the whole document.

## In the analysis flow

Here's how the tool shows up during a real investigation. An incident
fires on the `api` service:

> **Incident:** `api` — `FATAL: remaining connection slots are reserved
> for non-replication superuser connections`, error rate climbing.

The analyze agent decides it needs your team's remediation steps and
emits a `find_runbook` call. The arguments are derived from the incident
— and the query is scrubbed through the redactor **before** it is
embedded:

```json
{
  "tool": "find_runbook",
  "args": {
    "query": "postgres connection slots reserved, pool exhausted on api",
    "service": "api",
    "limit": 3
  }
}
```

The tool embeds the query, runs the similarity search, and returns the
standard envelope. `found` is `true` because at least one runbook
matched:

```json
{
  "tool": "find_runbook",
  "found": true,
  "data": {
    "count": 1,
    "service": "api",
    "matches": [
      {
        "id": "postgres-pool-exhausted.md",
        "title": "Postgres connection pool exhausted",
        "service": "api",
        "score": 0.89,
        "excerpt": "1. Check active connections: SELECT count(*) FROM pg_stat_activity; 2. Terminate stuck idle-in-transaction sessions. 3. Roll back the most recent api deploy if the leak is app-side.",
        "source": "postgres-pool-exhausted.md"
      }
    ]
  }
}
```

The agent folds those steps into its conclusion, and the call — its
arguments and what it returned — is recorded in the **Tool calls** section
of the analysis result so you can audit exactly which runbook grounded
the finding.

> **Note:** When nothing matches (or the corpus is empty), the tool
> returns `found: false` with an empty `matches` list. The analysis still
> completes — it just proceeds without runbook grounding.

## Setup

There are two steps: configure the embedding model, then drop your
runbooks in the corpus directory.

### 1. Configure the embedding model

In [`tools.yaml`](../../configuration/configuration.md):

```yaml
tools:
  find_runbook:
    embedding_model: text-embedding-3-small   # empty = tool omitted
```

### 2. Add your runbooks

Place your `*.md` runbooks in the data folder under `runbooks/`
(`./data/runbooks`; `/app/data/runbooks` in the container image). The
server auto-ingests them at boot — it scans `*.md` files, embeds any that
are new or edited, and persists the vectors to the same storage backend
it reads at boot.

> **Tip:** Ingestion is incremental. A runbook whose content is unchanged
> since the last boot reuses its cached embedding, so a restart with no
> edits makes zero embedding calls. Only new or edited runbooks are
> re-embedded.

On a successful boot you'll see both lines in the log:

```text
agent: find_runbook: ingested 6 runbook(s) from ./data/runbooks
agent: find_runbook enabled model=text-embedding-3-small runbooks=6
```

## Runbook format

A runbook is just a Markdown file. Optional YAML front-matter enriches the
record; without it, the title is derived from the first `# ` heading or
the filename.

```markdown
---
title: Postgres connection pool exhausted
service: api
tags: [database, postgres]
---
# Postgres connection pool exhausted

1. Check active connections: `SELECT count(*) FROM pg_stat_activity;`
2. Terminate stuck idle-in-transaction sessions.
3. Roll back the most recent api deploy if the leak is app-side.
```

Front-matter fields, all optional:

| Field | Type | Purpose |
|---|---|---|
| `title` | string | Display title. Falls back to the first `# ` heading, then the filename. |
| `service` | string | Single service this runbook applies to. Feeds the `service` filter. |
| `services` | list | Multiple services, when one runbook covers several. |
| `tags` | list | Free-form labels for organization. |

> **Tip:** Set `service` (or `services`) so the agent can scope matches.
> When an incident is on `api`, a `service: api` filter keeps the search
> focused on the runbooks that actually apply.

## Security posture

Embedding a query is an external trust boundary — identical to the
chat-completion call. The query the agent builds may carry
incident-derived text, so it is scrubbed through the same redactor
(`redaction.*` in `config.yaml`) **before** it is embedded. Returned
excerpts are scrubbed on the way out too. Incident-derived text never
egresses raw.

To keep embeddings fully inside your own network, set `agent.ai.provider`
to `ollama` (or `gemini`) — the embedder selects its backend from the same
provider as the chat path. No code change is required.

## Pre-baking the corpus (optional)

You usually don't need this — the server auto-ingests at boot. But for
CI pipelines or air-gapped image builds, the `runbook-ingest` command
builds the corpus out-of-band so the server starts with it already
populated:

```bash
runbook-ingest -config config/config.yaml
```

It reads the same `data/runbooks` directory, the same
`tools.find_runbook.embedding_model`, and the same
`agent.ai.api_key` the server uses.

## Managing runbooks from the admin UI

Beyond the corpus directory, you can upload, view, and delete runbooks
from the **Runbooks** page in the [admin UI](../../configuration/admin-ui.md)
(`/runbooks`). Uploads atomically rebuild the search index, so newly
uploaded runbooks become searchable without a restart.

---

Back to the [Analyze Tools overview](./tools.md).
