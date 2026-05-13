# Docker Compose examples

Each subfolder is a self-contained example for one AI-agent data
source. They share the same minimal `config/config.yaml`; the only
difference is `agent_sources.yaml` and which backing services are
spun up alongside Versus + Redis.

| Example | Brings up | When to pick |
|---|---|---|
| [file/](./file/) | versus + redis | Quickest start; tail a local log file |
| [loki/](./loki/) | versus + redis + **loki** + **grafana** | Test the `loki` source against a real Loki |
| [elasticsearch/](./elasticsearch/) | versus + redis + **elasticsearch** + **kibana** | Test the `elasticsearch` source against a real ES |
| [cloudwatch/](./cloudwatch/) | versus + redis | Test the `cloudwatchlogs` source against your AWS account |

## Workflow per example

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

All examples expose Versus on `http://localhost:3000`. The Loki and
Elasticsearch examples additionally expose their respective UIs
(Grafana on `:3001`, Kibana on `:5601`).

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

[Data Sources guide](https://versuscontrol.github.io/versus-incident/agent/data-sources.html)
