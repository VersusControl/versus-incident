# Examples

Runnable examples for Versus Incident. For prose docs, see the
[user guide](https://docs.versusincident.com).

## docker-compose

One folder per AI-agent data source. Each is self-contained and
runs only the services required for that source.

| Example | Backing services |
|---|---|
| [docker-compose/file/](./docker-compose/file/) | (none — local file) |
| [docker-compose/loki/](./docker-compose/loki/) | Loki + Grafana |
| [docker-compose/elasticsearch/](./docker-compose/elasticsearch/) | Elasticsearch + Kibana |
| [docker-compose/cloudwatch/](./docker-compose/cloudwatch/) | (your AWS account) |

See [docker-compose/README.md](./docker-compose/README.md) for the
shared workflow.

## Notification channels

Channel webhook setup lives in the main docs:
[docs.versusincident.com](https://docs.versusincident.com).

## Data source reference

Per-source field reference and troubleshooting:
[Data Sources guide](https://docs.versusincident.com/#/agent/data-sources).
