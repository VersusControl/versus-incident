# SigNoz Metrics

_Enterprise_

## Concept

Give Versus your [SigNoz](https://signoz.io/) address and a query API key, and
it finds your services and the metrics worth watching for each. It learns each
signal's normal by time of week and raises an incident when a signal moves far
outside it and stays there. It works with SigNoz Cloud and self-hosted SigNoz,
version 0.87.0 or later.

SigNoz cannot list its metric names, so Versus finds services from your recent
traces and then checks a short list of metrics for each one. By default that
list is SigNoz's own span metrics, which exist wherever traces flow. Add your
own metrics to the list with `metrics:`.

## Example

Your services send traces to SigNoz, so SigNoz keeps `signoz_calls_total` and
`signoz_latency.bucket` for each. Versus finds `checkout` and `payments` in the
traces, checks both metrics for each, and learns their traffic and p99 latency.
When `checkout`'s latency stays far above its usual band, Versus raises one
incident for `checkout`. `payments`, still normal, stays quiet.

## What you see in the UI

| Where | What |
|---|---|
| **Agent Learning → Metrics** | Each service with its SigNoz metrics. **Ready** once a signal's normal is learned, **Still learning** before. |
| **Incidents** | One incident per service and signal that stays out of its normal, naming the source. |
| **AI → Decisions**, **Shadow** tab | What the source would have raised, in shadow mode. |

## Turn it on

```yaml
# config/agent_sources.yaml
sources:
  - name: prod-signoz-metrics
    type: signoz_metrics
    enable: true
    options:
      address: https://<region>.signoz.cloud   # or your self-hosted SigNoz
      api_key: ${SIGNOZ_API_KEY}
```

Create the key in SigNoz under **Settings → API Keys** (needs the Admin role).
Use a **query** API key, not an ingestion key. For a self-hosted SigNoz on a
private address, also set `allow_private_networks: true`.

For SigNoz traces, add a `signoz_traces` source: see
[Traces](../../agent/data-sources/traces.md#signoz-backend).

## Try it with Docker Compose

You need Git, Docker with Docker Compose v2, Python 3, and a Versus Enterprise
license key. Clone the repository and go into it:

```bash
git clone https://github.com/VersusControl/versus-incident.git
cd versus-incident
```

Every command below runs from this `versus-incident` folder, the repository
root.

[`examples/metrics-source/`](https://github.com/VersusControl/versus-incident/tree/main/examples/metrics-source)
has a SigNoz overlay, `docker-compose.signoz.yml`. It runs Versus Enterprise
with a self-hosted SigNoz and both `signoz_metrics` and `signoz_traces` sources.
A one-shot service creates the SigNoz admin and a query API key for Versus, so
you set no key yourself.

The stack is heavy (ClickHouse, ZooKeeper, a collector and SigNoz): give
Docker at least 4 GB of memory. The demo traffic script needs Python 3 on your
machine.

```bash
export LICENSE_KEY=<your license key>
docker compose -f examples/metrics-source/docker-compose.yml \
  -f examples/metrics-source/docker-compose.signoz.yml up -d
docker compose -f examples/metrics-source/docker-compose.yml \
  -f examples/metrics-source/docker-compose.signoz.yml \
  logs versus | grep -A6 "DEFAULT ADMIN CREDENTIALS"
python3 examples/metrics-source/scripts/generate_fake_metrics.py \
  --backend signoz --spike --duration 240
```

Open `http://localhost:3000` and sign in as `admin`. **Agent Learning →
Metrics** lists the demo services from SigNoz. The SigNoz UI is on
`http://localhost:8080` (`admin@versus.local` / `Versus-Dev-12345`).

Clean up:

```bash
docker compose -f examples/metrics-source/docker-compose.yml \
  -f examples/metrics-source/docker-compose.signoz.yml down -v
```

Do not combine this overlay with the Tempo overlay: both use port `4318`.

## How it chooses what to watch

- **Services** come from recent spans. Versus reads the first of
  `service.name`, `resource.service.name`, `serviceName` and `service`. With no
  service attribute at all, it watches each metric across everything, under the
  service `_all_`.
- **Metrics** come from the list: `signoz_calls_total` and
  `signoz_latency.bucket`, plus anything in `metrics:`. Each service and metric
  pair is checked once per discovery; only pairs that return data are watched.
- **Spelling matters.** SigNoz writes histogram parts with a dot
  (`signoz_latency.bucket`, `http_server_duration.sum`) and counters as named
  (`signoz_calls_total`). A misspelled name returns no data and is never
  watched, without an error.
- **How a metric is read** follows its name:

  | Name ends with | Read as | Signal |
  |---|---|---|
  | `.bucket` or `_bucket` | p99 | latency |
  | `.sum`, `.count`, `_total`, `_count` or `_sum` | rate | traffic |
  | anything else | average | saturation |

Add metrics and narrow the scope like this:

```yaml
    options:
      address: https://signoz.example.internal
      api_key: ${SIGNOZ_API_KEY}
      allow_private_networks: true
      metrics:                       # added to the default list
        - http_server_duration.bucket
        - system_memory_usage
      filter: "deployment.environment = 'prod'"
```

`filter` is a SigNoz filter expression, not PromQL, with dotted attribute
names. It applies to every read. A malformed one stops the source at start.

## Options

Only `address` and `api_key` are required.

| Key | Default | What it does |
|---|---|---|
| `address` | required | SigNoz URL, or `https://<region>.signoz.cloud`. Use HTTPS in production: over HTTP the key is sent in plain text. |
| `api_key` | required | SigNoz query API key. |
| `insecure_skip_verify` | `false` | Skip TLS verification. |
| `allow_private_networks` | `false` | Allow a private address, such as a self-hosted SigNoz. |
| `allow_loopback` | `false` | Allow `localhost`. |
| `step` | `60s` | Sampling resolution. |
| `metrics` | none | Metric names added to the default list. |
| `filter` | none | SigNoz filter expression applied to every read. |
| `max_services` | `50` | Most services watched. |
| `max_signals` | `200` | Most signals watched in total. |
| `max_per_service` | `6` | Most signals per service. |
| `discovery_interval` | `1h` | How often Versus looks for services and metrics. |
| `discovery_lookback` | `1h` | Time range of spans read to find services (at least `5m`). |
| `discovery_samples` | `200` | Spans read per search, up to `1000`. Raise it on a quiet backend. |

Each discovery checks at most 500 service and metric pairs. On a large
deployment, lower `max_services` or add a `filter` so the checks cover what
matters.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `requires Versus Enterprise` | No `LICENSE_KEY`, or the key lacks the `intelligence` entitlement. |
| `401` or `403` on every read | An ingestion key instead of a query API key, or a revoked key. |
| `404` from SigNoz | SigNoz is older than 0.87.0. |
| Nothing under **Agent Learning → Metrics** | No spans in the last `discovery_lookback`, or none of the listed metrics returned data. Check that traces reach SigNoz. |
| Only a service called `_all_` | Your spans have no `service.name`. Set it in your instrumentation. |
| A metric you want is never watched | It is not in the list. Add it under `metrics:`, spelled as SigNoz stores it. |

## See also

- [Metrics overview](./overview.md) · [Prometheus Metrics](./prometheus.md)
- [Traces](../traces.md), including SigNoz traces
- [SigNoz logs source](../../agent/data-sources/signoz.md) (OSS)
