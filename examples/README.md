# Examples

Runnable examples for Versus Incident. For prose docs, see the
[user guide](https://docs.versusincident.com).

## quickstart

[quickstart/sre-agent/](./quickstart/sre-agent/) (detect a new log error from Loki) and [quickstart/webhook/](./quickstart/webhook/) (Alertmanager alert to Slack with a custom template) — one `docker compose up -d` each, no edits.

## docker-compose

One folder per AI-agent data source. Each is self-contained and
runs only the services required for that source.

| Example | Backing services |
|---|---|
| [docker-compose/file/](./docker-compose/file/) | (none — local file) |
| [docker-compose/loki/](./docker-compose/loki/) | Loki + Grafana |
| [metrics-source/](./metrics-source/) (_Enterprise_) | Versus Enterprise + Postgres + Prometheus + Pushgateway; learns metric baselines and raises an incident on a spike (Tempo or SigNoz overlay optional; needs `LICENSE_KEY`) |
| [docker-compose/elasticsearch/](./docker-compose/elasticsearch/) | Elasticsearch + Kibana |
| [docker-compose/signoz/](./docker-compose/signoz/) | SigNoz + ClickHouse + ZooKeeper + OTel collector (**≥4 GB Docker memory**) |

See [docker-compose/README.md](./docker-compose/README.md) for the
shared workflow.

## kubernetes

[kubernetes/](./kubernetes/) — `k8s-connector-in-cluster-enabled.yaml`, one
`kubectl apply` that runs Versus in an existing cluster with the in-cluster
Kubernetes connector and a read-only reader role.

## Notification channels

Channel webhook setup lives in the main docs:
[docs.versusincident.com](https://docs.versusincident.com).

## Data source reference

Per-source field reference and troubleshooting:
[Data Sources guide](https://docs.versusincident.com/#/agent/data-sources).
