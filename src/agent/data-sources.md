# AI Agent — Data Sources

The AI agent ingests log signals via pluggable **sources**. Every source
implements the same contract — pull new signals since a cursor, return
the next cursor — so they can be mixed freely in one deployment.

| Source | Type string | Best for |
|---|---|---|
| [File](./data-sources/file.md) | `file` | Local files, container stdout via volume, fixtures |
| [Elasticsearch](./data-sources/elasticsearch.md) | `elasticsearch` | ELK, Elastic Cloud, OpenSearch |
| [Loki](./data-sources/loki.md) | `loki` | Grafana Loki self-hosted, Grafana Cloud Logs |
| [CloudWatch Logs](./data-sources/cloudwatch-logs.md) | `cloudwatchlogs` | AWS Lambda, ECS, EKS, EC2 |

## How sources are configured

Sources live in a separate file, **`agent_sources.yaml`**, sitting next
to your main `config.yaml`. The file is optional. When present, it
REPLACES any inline `agent.sources` from the main config.

```yaml
# agent_sources.yaml
sources:
  - name: my-source        # unique, used in cursor keys & admin views
    type: file             # one of: file | elasticsearch | loki | cloudwatchlogs
    enable: true
    file:                  # block name MUST match `type`
      path: /var/log/app.log
```

Multiple sources are supported — each runs on its own goroutine with
an independent cursor.

## Cursor & ordering

Every source is cursor-based:

- `Pull(ctx, since)` returns signals strictly **after** `since` plus
  the new cursor the worker should pass back next tick.
- The worker stores the cursor in Redis under
  `versus:agent:cursor:<source>` (RFC3339Nano timestamp) and falls
  back to in-memory state when Redis is unavailable. The `file`
  source uses a sidecar `.cursor` file with the byte offset instead.
- On first start (no cursor), the agent backfills `agent.lookback`
  worth of history (default `5m`).

This means restarts are safe: the agent picks up exactly where it left
off, and no signal is processed twice or skipped.

## Try it locally

The runnable [docker-compose example](https://github.com/VersusControl/versus-incident/tree/main/examples/docker-compose)
ships with **Versus + Redis + Loki + Elasticsearch + Grafana + Kibana**
so you can experiment with all source types in a single
`docker compose up`.

## Adding a new source type

The codebase is designed so a new backend takes ~150 lines of Go. See
the **Adding a new SignalSource** section in
[AGENTS.md](https://github.com/VersusControl/versus-incident/blob/main/AGENTS.md)
and the existing implementations under `pkg/signalsources/`.
