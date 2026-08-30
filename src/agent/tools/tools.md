# Tool Reference

The AI SRE Agent uses read-only tools to gather evidence before it answers.
Tools are organized into the same three groups in Chat, Analyze, and the Tool
catalog: `versus`, `common`, and `k8s`. Chat and Analyze have independent tool
policies, so enabling a tool for one does not enable it for the other.

Every tool call, including its arguments and result, is recorded with the AI
response for audit. A tool reads and ranks data; it never mutates cluster state,
runs remediation, triggers on-call, or sends a notification.

## Versus tools

The `versus` group reads state already held by Versus. These internal tools need
no external integration and are always available to an agent whose policy allows
them. The Tool catalog UI can hide this group because there is nothing for an
operator to connect.

| Tool | What it returns |
|---|---|
| `get_system_overview` | Scoped incident, service, pattern, source-coverage, and detection-health summary |
| `list_services` | Observed services and their incident activity |
| `get_service` | One service and its bounded reliability context |
| `get_incident` | One incident and its bounded, redacted analysis history |
| `search_incidents` | Scoped incident history with bounded pagination |
| `list_patterns` | Learned signal patterns and their current status |
| `get_pattern` | One learned pattern with bounded, redacted samples |
| `list_analyses` | Prior AI analyses for an incident or service |
| `get_alert_decision` | The latest provider-neutral alert decision and its evidence |
| `list_capabilities` | Configured and available Versus capabilities and setup actions |
| `get_detection_health` | Configured signal coverage and dark signal categories |

## Common tools

The `common` group connects AI investigations to operational data. Each tool is
offered only when its requirement is satisfied and its Chat or Analyze policy
allows it.

### `get_related_logs`

Reads a bounded, redacted slice of logs for a service and time window. Configure
a [log data source](../data-sources.md), then use the provider guide for details,
such as [Elasticsearch](../data-sources/elasticsearch.md),
[Loki](../data-sources/loki.md), or
[CloudWatch Logs](../data-sources/cloudwatch-logs.md).

Every returned line is scrubbed by the configured redactor before it reaches
the AI model. The default time window is 15 minutes and the maximum is 1440
minutes (24 hours). The tool returns 50 lines by default and caps the result at
200 lines.

### `query_metrics`

Runs an on-demand PromQL range query during an investigation. This correlation
tool is available in OSS and Enterprise; the standing source that discovers
signals and opens incidents is Enterprise. See [Prometheus](../data-sources/prometheus.md)
for setup and tiering.

### `query_traces`

Reads bounded distributed traces for a service and time window. This on-demand
correlation tool is available in OSS and Enterprise; the standing source is
Enterprise. See [Traces](../data-sources/traces.md) for setup and tiering.

### `find_runbook`

Searches indexed runbooks for operational guidance. It requires an AI embedder
and a runbook index. See [Find Runbook](./find-runbook.md) for corpus setup,
redaction, and air-gapped ingestion.

### `recent_changes`

Reads recent commits from configured source repositories. It requires a GitHub
integration and at least one repository. See
[Recent Changes](./recent-changes.md) for authentication and repository setup.

### `describe_dependencies` :id=describe_dependencies

Reads the operator-configured service dependency graph so the AI can reason
about upstream causes and downstream impact. Configure the graph in
`tools.describe_dependencies.services` as shown below.

## Kubernetes tools

The `k8s` group is planned but not yet implemented. The catalog can show the
group as unavailable, but Chat and Analyze cannot call its tools:
`get_cluster_overview`, `list_workloads`, `get_workload`,
`get_k8s_topology`, and `list_k8s_events`.

Use this section as the documentation target for the planned group until the
Kubernetes guides are published.

## Tool configuration

Configuration for `describe_dependencies`, `recent_changes`, `find_runbook`,
`query_metrics`, and `query_traces` lives in an optional **`tools.yaml`** file
placed next to `config.yaml`. `tools.yaml` provides data and credentials; it is
not a tool allow-list.

The root of `tools.yaml` also carries two shared tool-loop knobs that apply to
every tool dispatch:

| Knob | Default | Description |
|---|---|---|
| `tool_timeout` | `20s` | Caps a single tool dispatch so one slow lookup can't consume the 2-minute analysis budget. A timeout surfaces as a tool error, never a hard failure. |
| `parallel_tools` | `false` | When the model emits several tool calls in one turn, run them concurrently instead of sequentially. The audit trail stays deterministically ordered either way. |

### Configure `query_metrics`

Point the on-demand tool at a Prometheus-compatible endpoint. An empty address
leaves the tool unavailable.

```yaml
tools:
  query_metrics:
    prometheus:
      address: http://prometheus:9090
```

See [Prometheus](../data-sources/prometheus.md) for authentication, TLS, and
the OSS versus Enterprise boundary.

### Configure `query_traces`

Point the on-demand tool at a Tempo-compatible endpoint. An empty address
leaves the tool unavailable.

```yaml
tools:
  query_traces:
    tempo:
      address: http://tempo:3200
```

See [Traces](../data-sources/traces.md) for authentication, TLS, and tiering.

### Configure `describe_dependencies`

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

### Configure `recent_changes`

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

### Configure `find_runbook`

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
```

Then place your `*.md` runbooks in the data folder under `runbooks/`
(`./data/runbooks`). The server auto-ingests them at boot.

> **See the full guide:** [`find_runbook` Tool](./find-runbook.md)
> covers arguments, runbook front-matter, the security/redaction posture,
> pre-baking the corpus with `runbook-ingest`, and managing runbooks from
> the admin UI.

## Complete `tools.yaml` example

A `tools.yaml` combining the common tool integrations and shared knobs:

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

  query_metrics:
    prometheus:
      address: http://prometheus:9090

  query_traces:
    tempo:
      address: http://tempo:3200
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

