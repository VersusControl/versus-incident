# Recent Changes Tool

Most incidents trace back to a change. A deploy went out, a config flag
flipped, a migration landed — and minutes later the errors started. The
`recent_changes` tool gives Chat and Analyze one piece of context
that turns a guess into a root cause: *what changed just before this
broke?*

It merges Kubernetes object changes with commit history from configured
deploy repositories and hands the agents a time-ordered list of recent
changes, newest first. Kubernetes-only installations need no GitHub
connection. When a spike
in `api` errors appears five minutes after a commit titled "migrate
users table" landed, the agent can flag that deploy as the probable
trigger instead of speculating.

> **Note:** This tool is read-only. It reads the Kubernetes change index and
> configured Git history; it never changes clusters or repositories.

## What the agent sees

For every commit in the lookback window the tool returns one change
record:

| Field | Source | Example |
|---|---|---|
| `timestamp` | Commit or Kubernetes change time (UTC) | `2026-06-12T11:58:03Z` |
| `service` | Configured or derived service name | `api` |
| `kind` | `commit` or a Kubernetes change kind | `commit` |
| `summary` | Commit subject or bounded Kubernetes change summary | `migrate users table` |
| `ref` | Optional short commit SHA or Kubernetes resource reference | `a1b2c3d` |

The agent can also pass arguments to narrow the view:

| Argument | Type | Default | Notes |
|---|---|---|---|
| `service` | string | *(all)* | Case-insensitive exact match on the service name. |
| `window_minutes` | integer | `120` | Look back this many minutes from now. Capped at `1440` (24 hours). |

## In the Chat and Analyze flows

Here's how the tool shows up during a real investigation. An incident
fires on the `api` service:

> **Incident:** `api` — 500 error rate jumped from 0.1% to 12% at
> 11:58 UTC.

The agent wants to know what changed just before the spike, so it
emits a `recent_changes` call scoped to the affected service:

```json
{
  "tool": "recent_changes",
  "args": {
    "service": "api",
    "window_minutes": 60
  }
}
```

The tool reads available feed records within the window and returns the
standard envelope, newest first. `found` is `true` because a matching change
landed inside the window:

```json
{
  "tool": "recent_changes",
  "found": true,
  "data": {
    "count": 1,
    "window_minutes": 60,
    "service": "api",
    "changes": [
      {
        "timestamp": "2026-06-12T11:53:09Z",
        "service": "api",
        "kind": "commit",
        "summary": "migrate users table: drop legacy index",
        "ref": "a1b2c3d"
      }
    ]
  }
}
```

The deploy landed five minutes before the spike, so the agent flags it as
the probable trigger in its conclusion. The call — its arguments and what
it returned — is recorded in the **Tool calls** section of the analysis
result, so you can audit exactly which change the agent correlated
against.

> **Note:** When no change falls in the window, the tool returns
> `found: false` with no `changes`. The agent still completes without change
> correlation. With neither a Kubernetes feed nor Git repositories configured,
> the capability is unavailable.

## Configuration

The tool lives in [`tools.yaml`](../../configuration/configuration.md), next to
your `config.yaml`. List your deploy repositories under
`tools.recent_changes.git.repos`. An empty `repos` list is valid when a
Kubernetes change feed is available; with neither source configured, the tool
is unavailable to both agents.

```yaml
tools:
  recent_changes:
    git:
      repos:
        - url: https://github.com/acme/api.git
          branch: main          # optional; empty = default HEAD
          service: api           # optional; empty = derived from repo name
```

Each repo entry takes:

| Key | Required | Description |
|---|---|---|
| `url` | yes | Remote clone URL — `https://…` or scp-like `git@host:org/repo`. |
| `branch` | no | Branch to read. Empty falls back to `HEAD`, then `main`, then `master`. |
| `service` | no | Service every commit in this repo maps to. Empty derives it from the repo name in the URL. |
| `auth` | no | Per-repo credential override (see below). |

> **Note:** No external `git` binary is required. The tool uses an
> embedded Git client, so it works the same in a minimal container image
> as on a developer laptop. Each repository is mirror-cloned into a local
> cache under the OS temp directory on first use and fetched on later
> lookups, so reads stay fresh without re-cloning.

## Authentication

Public repositories need no credentials. Private remotes authenticate
through an optional `auth` block. A global `git.auth` default applies to
every repo, and any repo may override it field by field. Empty fields
fall back to the global default; when both are empty the tool uses
ambient credentials.

| Auth field | Mechanism | Notes |
|---|---|---|
| `token` | HTTPS Basic auth (`x-access-token` + token) | Never persisted to the local mirror. |
| `ssh_key_path` | Native SSH key authentication | Must be readable by the container user. |

```yaml
tools:
  recent_changes:
    git:
      auth:                       # global default for every repo
        token: ${GIT_TOKEN}         # HTTPS personal access token
        ssh_key_path: ""            # path to a private SSH key
      repos:
        - url: https://github.com/acme/api.git
          branch: main
          service: api
        - url: git@github.com:acme/web.git   # service derived as "web"
          auth:                   # per-repo override of git.auth
            ssh_key_path: /keys/web_deploy
```

> **Tip:** Pass tokens through environment variables (`${GIT_TOKEN}`)
> rather than hardcoding them in `tools.yaml`. The file is config, not a
> secret store.

## How it behaves under failure

The feed is built to degrade cleanly, never to block an analysis:

- **One broken remote doesn't blind the feed.** If a repository fails to
  clone or fetch (a transient network error, a bad credential), that repo
  is skipped and the others still return. An error surfaces only when
  *every* repository fails.
- **No commits in the window is not an error.** The agent simply learns
  there were no recent changes to correlate against.
- **An unconfigured feed is a clean miss.** With no `repos`, the tool is
  omitted entirely and the analysis runs without it.

## Running with Docker

Mount `tools.yaml` next to `config.yaml`, pass the token as an
environment variable, and mount any SSH key the feed needs:

```bash
docker run -d --name versus-incident \
  -p 3000:3000 \
  -e GATEWAY_SECRET=my-secret \
  -e AGENT_ENABLE=true \
  -e AGENT_AI_ENABLE=true \
  -e AGENT_AI_API_KEY=sk-... \
  -e GIT_TOKEN=ghp_xxxxxxxxxxxx \
  -v ./config:/app/config \
  -v ~/.ssh/web_deploy:/keys/web_deploy:ro \
  ghcr.io/versuscontrol/versus-incident:latest
```

---

Back to the [Tool Reference](./tools.md).
