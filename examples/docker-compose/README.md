# Docker Compose examples

Each subfolder is a self-contained example for one AI-agent data
source or backend. Most start Versus and Redis with a local
`agent_sources.yaml`; the metrics example starts only backing services for a
separately configured Enterprise agent.

| Example | Brings up | When to pick |
|---|---|---|
| [file/](./file/) | versus + redis | Quickest start; tail a local log file |
| [loki/](./loki/) | versus + redis + **loki** + **grafana** | Test the `loki` source against a real Loki |
| [metrics/](./metrics/) | prometheus + **pushgateway** (optional Tempo overlay; no Versus container) | Host-run generators push synthetic series and optional traces. A separately configured, licensed Enterprise `prometheus` / `traces` source can detect anomalies and provide source-bound `discover_metrics` / `read_metric_series` and `discover_trace_fields` / `read_trace_spans` to Chat and Analyze. OSS has no metric/trace read tools. |
| [elasticsearch/](./elasticsearch/) | versus + redis + **elasticsearch** + **kibana** | Test the `elasticsearch` source against a real ES |
| [cloudwatch/](./cloudwatch/) | versus + redis | Test the `cloudwatchlogs` source against your AWS account |
| [graylog/](./graylog/) | versus + redis + **graylog** + mongodb + opensearch | Test the `graylog` source against a real Graylog |
| [splunk/](./splunk/) | versus + redis + **splunk** | Test the `splunk` source against a real Splunk Enterprise |
| [signoz/](./signoz/) | versus + redis + **signoz** + clickhouse + zookeeper + otel-collector | Test the `signoz` source against a real SigNoz. **Heavy — needs ≥4 GB of Docker memory.** |

## Workflow per example

For the metrics backend-only example, follow its
[own run instructions](./metrics/README.md) instead; there is no `versus`
service to tail or recreate in that Compose stack.

```bash
cd <example>
docker compose up -d              # all settings have sane defaults
docker compose logs -f versus
# ... interact ...
docker compose down -v            # cleanup, drops volumes
```

Every value is set via `${VAR:-default}` in the compose file, so
zero configuration is required to start. To override (e.g. enable
Slack, set a real `GATEWAY_SECRET`, change the Redis password),
export the variables in your shell before running `docker compose
up`:

```bash
export GATEWAY_SECRET=my-real-secret
export SLACK_ENABLE=true
export SLACK_TOKEN=xoxb-...
export SLACK_CHANNEL_ID=C01234567
docker compose up -d --force-recreate versus
```

CloudWatch additionally requires `CW_LOG_GROUP_NAME` and AWS
credentials — see [cloudwatch/README.md](./cloudwatch/) for the
list.

Examples that start Versus expose it on `http://localhost:3000`. The Loki and
Elasticsearch examples additionally expose their respective UIs
(Grafana on `:3001`, Kibana on `:5601`). Graylog exposes its web UI
on `:9000`; Splunk on `:8000`; SigNoz on `:8080` (and its OTLP
receiver on `:4317`/`:4318`).

## Generate test traffic

Each example's README has a one-liner that uses
[`scripts/generate_noisy_logs.py`](../../scripts/generate_noisy_logs.py)
(or the [`scripts/run_noisy_logs.sh`](../../scripts/run_noisy_logs.sh)
wrapper) to push synthetic application logs into the matching
backend. The same template / `--spike` / `--scenario` flags work
across all four targets — only the destination flag changes.

```bash
# from the repo root, with the file stack running:
scripts/run_noisy_logs.sh                                # tail to logs/app.log
scripts/run_noisy_logs.sh --target loki                  # push to local Loki
scripts/run_noisy_logs.sh --target elasticsearch         # push to local ES
TARGET=cloudwatch CW_LOG_GROUP_NAME=/aws/lambda/foo \
  scripts/run_noisy_logs.sh                              # push to AWS
scripts/run_noisy_logs.sh --target graylog               # push GELF UDP to local Graylog
SPLUNK_HEC_TOKEN=... \
  scripts/run_noisy_logs.sh --target splunk              # push HEC to local Splunk
scripts/run_noisy_logs.sh --target signoz                # push OTLP/HTTP to local SigNoz
```

See [scripts/README.md](../../scripts/README.md) for the full
flag reference (spike / scenario modes, template lists, batching).

## Notification channels

Channel webhooks are off by default. Flip the matching `*_ENABLE`
variable and fill in tokens, then restart just Versus:

```bash
SLACK_ENABLE=true SLACK_TOKEN=xoxb-... SLACK_CHANNEL_ID=C0123 \
  docker compose up -d --force-recreate versus
```

If you turn channels on, copy the corresponding `*_message.tmpl`
files from the repo's [config/](../../config/) directory into the
example's `config/` folder first — the env var enables the channel,
but the templates still need to be present.

## Per-source field reference

[Data Sources guide](https://docs.versusincident.com/#/agent/data-sources)
