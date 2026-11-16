# Metrics and traces sources (Versus Enterprise)

The `prometheus` and `traces` sources are Enterprise features. This example
runs the Enterprise image and needs a license key with the `intelligence`
entitlement. Without a key the image starts in community mode and the sources
stay off.

You need Git and Docker with Docker Compose v2. Clone the repository and go
into this example's folder:

```bash
git clone https://github.com/VersusControl/versus-incident.git
cd versus-incident/examples/metrics-source
```

Every command in this README runs from that folder,
`versus-incident/examples/metrics-source`. If you open a new terminal, `cd`
into it first.

Versus Enterprise reads a local Prometheus (and, with the traces overlay, a
local Tempo), learns what normal looks like for each service, and raises an
incident when a signal stays well outside what it learned.

## What runs

| Service | Address | What it does |
|---|---|---|
| versus | <http://localhost:3000> | Versus Enterprise with the Prometheus source (plus the traces source in the overlay) |
| prometheus | <http://localhost:9090> | scrapes the Pushgateway; Versus reads it |
| pushgateway | `localhost:9091` | receives the demo series |
| postgres, redis | internal | Versus storage |
| tempo | `localhost:3200`, OTLP `localhost:4318` | trace backend (traces overlay only) |
| metrics-generator | none | steady demo traffic (profile `fake-metrics`) |
| metrics-spike | none | one 90-second error and latency spike (profile `metrics-spike`) |

Every port binds to 127.0.0.1. The demo traffic comes from
[scripts/generate_fake_metrics.py](scripts/generate_fake_metrics.py), run
inside a `python:3.12-alpine` container, so you need no Python on your machine.

## How it is configured

[config/agent_sources.yaml](config/agent_sources.yaml) holds the source:

```yaml
sources:
  - name: demo-prom
    type: prometheus
    enable: true
    options:
      address: http://prometheus:9090
      step: 15s                      # one sample every 15s
      filter: '{service!=""}'        # learn only the demo series
      discovery_interval: 1m         # look for new series every minute (default 1h)
```

The traces overlay mounts
[config/agent_sources.traces.yaml](config/agent_sources.traces.yaml) instead,
which adds:

```yaml
  - name: demo-traces
    type: traces
    enable: true
    options:
      backend: tempo
      address: http://tempo:3200
      allow_private_networks: true   # Tempo is on the private Docker network
      query: '{ resource.service.name = "checkout" }'
      page_size: 100
      discovery_interval: 1m
```

Versus reads these files at start. After editing one, run
`docker compose restart versus` (with the overlay files when you use them).

Licensed sources also give Chat and Analyze their read tools
(`discover_metrics`, `read_metric_series`, `discover_trace_fields`,
`read_trace_spans`); no `tools.yaml` entry is needed. Those tools are used when
AI is on:

```bash
export AGENT_AI_ENABLE=true AGENT_AI_API_KEY=<key> AGENT_AI_MODEL=gpt-4o-mini
docker compose up -d versus
```

## Run it

### 1. Start the stack and the demo traffic

```bash
export LICENSE_KEY=<your license key>     # never commit it
docker compose --profile fake-metrics up -d
```

You can also copy `.env.example` to `.env` and put the key there; `.env` is
gitignored. Without `--profile fake-metrics` only the stack starts; run the
same command with the profile later to add the traffic.

The `metrics-generator` service pushes healthy traffic for the `checkout`
service every 2 seconds: about 20 requests per push, a few `500`s, low
latency.

### 2. Sign in

Licensed, Versus uses the built-in admin account. Its password is printed once,
on the first boot against an empty database:

```bash
docker compose logs versus | grep -A6 "DEFAULT ADMIN CREDENTIALS"
```

Open <http://localhost:3000> and sign in as `admin` with that password. Keep
it: recreating the Versus container (for example with the traces overlay)
does not print it again. `docker compose down -v` resets it.

### 3. Let it learn (about 2 minutes)

Within a minute the source finds the `checkout` series:

```bash
docker compose logs versus | grep demo-prom
```

```text
agent: source demo-prom using "metrics" brain
agent: standing metric source "demo-prom" discovered 6 signal(s) across 1 service(s) (label="service" generic=false metrics=0 notes=[])
agent: new service discovered: "checkout"
```

The first discovery right after boot may report `generic=true` with
Prometheus's own metric names; the next pass, a minute later, picks up the
`service` label. The agent pages then list `checkout` with its learned
request rate and error rate. Each baseline shows its sample count and turns
confident after about 2 minutes. The same data is in the API:

```bash
curl -c cookies.txt -H 'Content-Type: application/json' -H 'Origin: http://localhost:3000' \
  -d '{"username":"admin","password":"<from the logs>"}' \
  http://localhost:3000/enterprise/api/auth/local/login
curl -b cookies.txt 'http://localhost:3000/api/agent/baselines?type=metric'
```

```json
{"service":"checkout","signal":"error_rate","kind":"errors","observations":6,"confident":true, ...}
{"service":"checkout","signal":"request_rate","kind":"traffic","observations":7,"confident":true, ...}
```

### 4. Fire a spike

Once the baselines are confident:

```bash
docker compose run --rm metrics-spike
```

For 90 seconds `checkout` returns about 45% `500`s and its latency goes above
500 ms. The spike is pushed as its own job, so the steady traffic keeps
running. After three agent ticks above the learned band (`poll_interval: 15s`)
Versus raises an incident, about a minute into the spike:

```text
Anomaly: checkout — error_rate (... above baseline ...)
```

It appears in the Incidents list with source `demo-prom`. A spike fired
before the baselines are confident is learned as normal and raises nothing.

## Add traces (Tempo)

The overlay adds Tempo and a second source that reads its spans. Use both
files on every command:

```bash
export LICENSE_KEY=<your license key>
docker compose -f docker-compose.yml -f docker-compose.traces.yml --profile fake-metrics up -d
docker compose -f docker-compose.yml -f docker-compose.traces.yml run --rm metrics-spike
```

With the overlay both generators also send one span to Tempo per push
(`POST /checkout` and `GET /orders`), about half of them errors during the
spike. The `demo-traces` source reads the spans its `query` selects and learns
their error rate and p99 latency. These baselines are listed under the service
`_custom_` with the operation `service request`, and the logs show
`agent: tick demo-traces signals=2 ...` every 15 seconds:

```bash
curl -b cookies.txt 'http://localhost:3000/api/agent/baselines?type=trace'
```

If you started the base stack first, this recreates Versus with the traces
source; the learned metric baselines stay in Postgres. Tempo's search API is
on <http://localhost:3200>.

## Settings

| Variable | Default | Purpose |
|---|---|---|
| `LICENSE_KEY` | empty | Enterprise license; turns the sources on |
| `VERSUS_ENTERPRISE_IMAGE` | `ghcr.io/versuscontrol/versus-enterprise` | image to run |
| `GATEWAY_SECRET` | `change-me` | admin API secret, used only in community mode (no license) |
| `AGENT_MODE` | `detect` | `training`, `shadow` or `detect` |
| `POSTGRES_PASSWORD`, `REDIS_PASSWORD` | `versus-dev`, `versus` | local storage passwords |
| `AGENT_AI_ENABLE`, `AGENT_AI_API_KEY`, `AGENT_AI_MODEL` | off | optional AI analysis |
| `SLACK_*`, `TELEGRAM_*` | off | optional notifications |

Compose reads these from your shell or from `.env`. Variables already set in
your shell, such as `AGENT_MODE`, override the defaults.

## Generator options

Run the generator by hand for other traffic shapes:

```bash
docker compose run --rm metrics-spike python3 /scripts/generate_fake_metrics.py --list
docker compose run --rm metrics-spike python3 /scripts/generate_fake_metrics.py \
  --job demo-spike --spike --spike-duration 60 --duration 120
docker compose run --rm metrics-spike python3 /scripts/generate_fake_metrics.py --clear
```

With Python 3 on your machine you can also run it directly against the
published Pushgateway: `python3 scripts/generate_fake_metrics.py --spike
--duration 90` (add `--otlp http://localhost:4318` for spans).

## Optional: SigNoz

`docker-compose.signoz.yml` swaps Prometheus and Tempo for a self-hosted SigNoz
and the `signoz_metrics` and `signoz_traces` sources
([config/agent_sources.signoz.yaml](config/agent_sources.signoz.yaml)). It is
heavy (ClickHouse, ZooKeeper, a collector and the SigNoz server; budget at
least 4 GB of Docker memory) and must not be combined with the traces overlay,
since both publish OTLP on `:4318`.

```bash
docker compose -f docker-compose.yml -f docker-compose.signoz.yml up -d
python3 scripts/generate_fake_metrics.py --backend signoz --spike --duration 240
docker compose -f docker-compose.yml -f docker-compose.signoz.yml down -v
```

A `signoz-bootstrap` one-shot creates the SigNoz admin
(`admin@versus.local` / `Versus-Dev-12345` by default) and a read-only API key
for Versus. The SigNoz UI is on <http://localhost:8080>.

## Clean up

```bash
docker compose --profile fake-metrics down -v
# with the traces overlay:
docker compose -f docker-compose.yml -f docker-compose.traces.yml --profile fake-metrics down -v
```

## Files

```text
metrics-source/
├── docker-compose.yml              Versus Enterprise, Prometheus, Pushgateway, Postgres, Redis, generators
├── docker-compose.traces.yml       overlay: Tempo and the traces source
├── docker-compose.signoz.yml       overlay: SigNoz instead of Prometheus and Tempo
├── .env.example                    optional settings; copy to .env
├── config/
│   ├── config.yaml                 Versus config (detect mode, 15s poll)
│   ├── agent_sources.yaml          Prometheus source
│   ├── agent_sources.traces.yaml   Prometheus and traces sources (traces overlay)
│   └── agent_sources.signoz.yaml   SigNoz sources (SigNoz overlay)
├── prometheus/prometheus.yml       scrapes the Pushgateway
├── tempo/tempo.yaml                single-binary Tempo
├── signoz/                         SigNoz overlay support files
└── scripts/generate_fake_metrics.py  demo traffic generator
```
