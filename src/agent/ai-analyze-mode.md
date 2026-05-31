# AI Agent — Analyze Mode

Analyze mode provides a **deep-dive investigation** for incidents. While detect mode identifies patterns and sends notifications, analyze mode helps you understand the root cause of an incident by gathering context and presenting structured insights directly in the dashboard.

Analyze mode is designed to assist on-call engineers, post-incident reviewers, and anyone needing a detailed analysis of an incident.

---

## When to Use Analyze Mode

Analyze mode is available whenever `agent.ai.enable: true`. If AI is disabled, the **Analysis** card on the incident detail page will show "coming soon."

Typical scenarios for using analyze mode:

- **On-call troubleshooting**: Quickly generate a starting hypothesis for unfamiliar incidents.
- **Correlating patterns**: Investigate incidents that resemble past patterns.
- **Post-incident review**: Add structured AI insights to human-written notes.

---

## How It Works

1. **Trigger Analysis**: Click **Run analysis** on the incident detail page.
2. **AI Investigation**: The AI gathers context, such as recent incidents, pattern history, and service summaries.
3. **Insights Delivered**: The results are saved and displayed in the dashboard for review.

Analyze mode runs within a 2-minute timeout to ensure responsiveness. Results are always saved, even if the analysis encounters errors.

---

## Configuration

Analyze mode uses the shared `agent.ai` settings. You can optionally point it at a stronger model for deep dives with the `analyze:` block:

```yaml
agent:
  enable: true
  mode: detect            # detect or shadow; analyze works in both
  ai:
    enable: true
    api_key: ${AGENT_AI_API_KEY}
    model: gpt-4o-mini    # shared default for detect + analyze

    analyze:
      model: gpt-4o         # use a stronger model for deep dives
```

The `analyze:` block only overrides the model. The tool-loop knobs
(`tool_timeout`, `parallel_tools`) and all per-tool data live in a
separate `tools.yaml` file — see [Tool configuration](#tool-configuration-toolsyaml)
below.

There is no `analyze.tools` allow-list: every tool wired in
`analyzetools.Default(...)` is available to the agent.

---

## Investigation Tools

During an analysis the AI doesn't just read the incident in isolation —
it can call **read-only tools** to pull supporting context from your
own data before forming a conclusion. Every tool is strictly
observational: tools only *read* state, they never send notifications,
mutate incidents, or touch external systems.

Each tool returns the same envelope so the AI can reason about results
uniformly:

- `found` — whether the lookup returned anything.
- `data` — the tool-specific payload (only present on a hit).

The full tool-call trail (which tools ran, their arguments, and what
they returned) is recorded with every analysis and shown in the
**Tool calls** section of the analysis result, so you can audit exactly
what the AI looked at.

### Tool configuration

Most tools need no configuration — they read state the agent already
keeps (incidents, learned patterns, redacted logs) and work out of the
box. A few tools read **external** data and need a small config block.
That configuration lives in an optional `tools.yaml` file placed next to
your main `config.yaml`; it is loaded automatically when present and
supports `${VAR}` environment expansion.

`tools.yaml` is per-tool **data** config, not a registration allow-list:
tools are wired in code, so a missing block just means the tool has no
data to read and degrades to a clean "nothing found". The root of the
file carries the shared tool-loop knobs that apply to **every** tool:

```yaml
tools:
  # Shared tool-loop knobs (apply to every analyze tool dispatch).
  tool_timeout: 20s         # per-tool dispatch cap; empty/"0"/invalid = 20s
  parallel_tools: false     # run tool calls in one model turn concurrently
```

| Knob | Default | Description |
|---|---|---|
| `tool_timeout` | `20s` | Caps a single tool dispatch so one slow lookup can't consume the 2-minute analysis budget. A timeout surfaces as a tool error in the **Tool calls** trail, never a hard failure. Empty, `0`, or unparseable inherits `20s`. |
| `parallel_tools` | `false` | When the model emits several tool calls in one turn, run them concurrently instead of sequentially. The audit trail stays deterministically ordered either way. |

Tools that read external data have their own block under `tools:`. Only
two tools currently need one — see their per-tool YAML in
[`describe_dependencies`](#describe_dependencies) and
[`recent_changes`](#recent_changes) below.

### Available tools

#### `recent_incidents`

Lists incidents recorded in a recent time window so the AI can spot
correlated failures, repeat offenders, or a broader outage in progress.

- **Time window** — defaults to the last 60 minutes, up to a maximum of
  1440 minutes (24 hours).
- **Service filter** — optionally narrows the list to a single service.
- **Limit** — returns up to 20 incidents by default, capped at 100.

Use case: *"Are other services failing at the same time, or is this
incident isolated?"*

#### `pattern_history`

Looks up a learned pattern by its id and returns everything the agent
knows about it: the log template, the EWMA frequency baseline, the
operator-set verdict, tags, observation counts, and the associated
service.

Use case: *"Is this a brand-new pattern, or a known issue that has
spiked above its normal baseline?"*

#### `describe_service`

Summarises a single service: when it was first seen by the agent and
its top learned patterns ranked by frequency.

Use case: *"What does normal look like for this service, and which
patterns dominate its logs?"*

#### `get_related_logs`

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

#### `describe_dependencies`

Surfaces the upstream and downstream services of a given service from
the service-dependency graph configured in `tools.yaml` and flags which
of them have a recent incident, so the AI can reason about a cascading
failure.

- **Service** — the service whose dependencies to describe.
- **Time window** — how far back to check each dependency for a recent
  incident (`has_recent_incident`).

**Configuration** — author the graph under
`tools.describe_dependencies.services`. Each entry has a `name` and a
`depends_on` list of upstream services; the reverse (downstream) edges
are derived automatically. With an empty `services` list the tool is not
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
```

Use case: *"Is this failure originating here, or is an upstream
dependency the real culprit?"*

#### `recent_changes`

Lists recent deploys and config changes from one or more **remote git
repositories' commit histories** within a time window, newest first, so
the AI can correlate an incident with what changed just before it. Each
repository is mirror-cloned into a local cache on first use and fetched
on later lookups; it shells out to the `git` binary, so no extra
dependency or change pipeline is needed.

- **Service filter** — optionally narrows to a single service
  (case-insensitive exact match against the commit's repository service).
- **Time window** — defaults to the last 120 minutes, up to a maximum
  of 1440 minutes (24 hours).

**Configuration** — list the repositories under
`tools.recent_changes.git.repos`. Each entry has a `url` (https or
scp-like `git@host:org/repo`), an optional `branch`, and an optional
`service` (auto-detected from the repo name when omitted). Private
remotes authenticate via an `auth` block (HTTPS `token` or
`ssh_key_path`): a global `git.auth` default applies to every repo and
any repo may override it, with empty fields falling back to the global
default. With an empty `repos` list the tool is not registered.

```yaml
tools:
  recent_changes:
    git:
      auth:                 # global default auth; per-repo auth overrides it
        token: ${GIT_TOKEN}   # HTTPS PAT (empty = ambient git credentials)
        ssh_key_path: ""      # SSH key path for ssh/scp-like remotes
      repos:
        - url: https://github.com/acme/api.git   # inherits global auth.token
          branch: main                           # optional; empty = default HEAD
          service: api                           # optional; empty = derived from URL
        - url: git@github.com:acme/web.git        # service auto-detected as "web"
          auth:                                  # optional; overrides git.auth
            ssh_key_path: /home/versus/.ssh/web_deploy
```

Each commit in the window maps to a change record (`timestamp`, `service`,
`kind: commit`, `summary`, short SHA `ref`). A window with no commits is a
clean "nothing found" rather than an error.

Use case: *"Did a deploy or config change in the last hour line up with
when this incident started?"*

---

## Roadmap — more tools for deeper analysis

The tools above form the foundation of analyze mode. The tool interface
is intentionally pluggable, and we plan to expand the catalog so the AI
can investigate further without manual digging. Areas we're exploring
for future tools include:

- **Metric & dashboard lookups** — pull the relevant time-series
  (error rate, latency, saturation) around the incident window. Metric
  retrieval remains a later-phase item.

As new tools ship they will appear automatically in the **Tool calls**
audit trail, and no configuration change is required to benefit from
them.

---

## Key Features

- **Non-intrusive**: Analyze mode never sends notifications or modifies systems.
- **Read-only tools**: The AI uses tools like `recent_incidents`, `pattern_history`, `describe_service`, `get_related_logs`, `describe_dependencies`, and `recent_changes` to gather context.
- **Customizable**: Fine-tune the AI's behavior with optional settings.

Analyze mode empowers you to make informed decisions by providing structured insights when you need them most.
