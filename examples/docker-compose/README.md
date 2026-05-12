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
cp .env.example .env
# edit .env (at minimum set GATEWAY_SECRET; CloudWatch needs AWS creds)
docker compose up -d
docker compose logs -f versus
# ... interact ...
docker compose down -v        # cleanup, drops volumes
```

All examples expose Versus on `http://localhost:3000`. The Loki and
Elasticsearch examples additionally expose their respective UIs
(Grafana on `:3001`, Kibana on `:5601`).

## Notification channels

Channel webhooks are off by default in every example. Flip the
matching `*_ENABLE` flag and fill in tokens in `.env`, then restart
just Versus:

```bash
docker compose up -d --force-recreate versus
```

If you turn channels on, copy the corresponding `*_message.tmpl`
files from the repo's [config/](../../config/) directory into the
example's `config/` folder first — `.env` enables the channel, but
the templates still need to be present.

## Per-source field reference

[Data Sources guide](https://versuscontrol.github.io/versus-incident/agent/data-sources.html)
