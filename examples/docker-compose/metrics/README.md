# Metrics and traces backend example

This Compose example starts **Prometheus** and a **Pushgateway**, with an
optional **Tempo** overlay. It does not start Versus or Redis. Run a separately
configured Versus instance to investigate the synthetic data. The OSS build
can use the sample `file` log source to trigger an incident, but metric and
trace reads require licensed Enterprise sources.

All fake data is produced by the host-run generators in
[`scripts/`](../../../scripts/) — the same convention as the log examples:
[`generate_noisy_logs.py`](../../../scripts/generate_noisy_logs.py) fires the
incident through the `file` log source, and
[`generate_fake_metrics.py`](../../../scripts/generate_fake_metrics.py) pushes
the correlating Prometheus series to the Pushgateway.

The standalone OSS `query_metrics` and `query_traces` tools have been removed.
Licensed `prometheus` and `traces` sources configured in `agent_sources.yaml`
contribute source-bound `discover_metrics` / `read_metric_series` and
`discover_trace_fields` / `read_trace_spans`, respectively. Neither backend
needs a separate tool file. See the
[migration guide](../../../src/migration/migration-metric-trace-tools.md) for
the license requirement and source setup.

## Services

| Service | Port | What |
|---|---|---|
| prometheus | `9090` | scrapes the pushgateway; available to a separately configured licensed source |
| pushgateway | `9091` | receives the synthetic series pushed by `scripts/generate_fake_metrics.py` |

## Run

```bash
docker compose up -d
```

Wait for Prometheus and Pushgateway to become healthy. No Versus container is
started by this Compose file.

## Generate normal metric data

From the **repo root**, push a steady, healthy series to the Pushgateway
(~0.5% errors, low latency — the anomaly rules stay silent):

```bash
# steady traffic for 60s (Ctrl+C to stop early, or --duration 0 to run forever)
python3 scripts/generate_fake_metrics.py
```

Confirm it's flowing:

```bash
curl -s localhost:9091/metrics | grep demo_http        # raw pushed exposition
# or query Prometheus directly once it has scraped twice:
curl -s 'localhost:9090/api/v1/query?query=demo_http_requests_total' | jq '.data.result[0]'
```

See the exact series/labels and sample PromQL the script emits:

```bash
python3 scripts/generate_fake_metrics.py --list
```

## Generate a log trigger and metric spike

Two host-run steps, both using the `scripts/` generators:

```bash
# 1. Append status=503 errors for a separately running agent with the sample
#    file source configured and logs/app.log available at its configured path.
python3 scripts/generate_noisy_logs.py --append --start-time now \
  --spike 5xx --spike-burst 80 \
  --output examples/docker-compose/metrics/logs/app.log

# 2. Push the correlating metric anomaly: ~45% 500s + p95 > 500ms.
python3 scripts/generate_fake_metrics.py --spike --duration 90
```

For a hands-off demo, auto-revert the metric spike to normal after N seconds:

```bash
python3 scripts/generate_fake_metrics.py --spike --spike-duration 60 --duration 180
```

To wipe the pushed series afterwards:

```bash
python3 scripts/generate_fake_metrics.py --clear
```

When a separately running agent is configured to read this log file, the
`status=503` lines can trigger a log incident.

## Inspect the metric spike

Open Prometheus (<http://localhost:9090>) and run
`sum by (service) (rate(demo_http_requests_total{code=~"5.."}[1m]))` in the
*Graph* tab. A separately deployed Versus Enterprise instance with a
configured `prometheus` source and the `intelligence` entitlement can expose
`discover_metrics` and `read_metric_series` to Chat and Analyze. Configure
the source address for that instance's network (for example,
`http://localhost:9090` for a host-run instance). Enable AI and provide its
API key in that instance's configuration to use these tools; generating the
spike alone does not make the agent call them.

## Agent modes

The sample [config/config.yaml](./config/config.yaml) defaults to `detect` for
the [file log source](./config/agent_sources.yaml). `training` learns log
patterns without incidents, `shadow` records would-alert classifications, and
`detect` can open log incidents. Neither OSS mode nor this Compose stack
contributes metric or trace read tools. Licensed Enterprise sources can also
detect metric or trace anomalies independently when configured.

> This example sets `new_service_grace: 0` so the first spike surfaces
> immediately. In production a non-zero grace window suppresses alerts for
> freshly-discovered services while the agent learns their baseline.

## Optional: traces correlation (Tempo)

Traces are kept out of the main path to keep it light. The overlay adds Tempo
only; it does not configure a Versus source or mount a tool file:

```bash
docker compose -f docker-compose.yml -f docker-compose.traces.yml up -d
```

Then point the **same** metrics generator at Tempo's OTLP/HTTP endpoint
(published on `:4318`) so it also emits best-effort spans (error spans during a
`--spike`):

```bash
python3 scripts/generate_fake_metrics.py --spike \
  --otlp http://localhost:4318 --duration 90
```

Tempo's API is on `:3200`, OTLP on `:4318`. To investigate those spans from a
separately deployed, licensed Versus Enterprise instance, configure a `traces`
source pointing to Tempo and use its source-bound `discover_trace_fields` and
`read_trace_spans` tools in Chat or Analyze. See the
[Traces source reference](../../../src/agent/data-sources/traces.md).

## Layout

```
metrics/
├── docker-compose.yml              # prometheus + pushgateway
├── docker-compose.traces.yml       # optional overlay: tempo
├── config/
│   ├── config.yaml                 # sample agent settings (not mounted by Compose)
│   └── agent_sources.yaml          # sample file log source (not mounted by Compose)
├── prometheus/
│   └── prometheus.yml              # scrapes pushgateway (honor_labels) + self
├── tempo/
│   └── tempo.yaml                  # single-binary Tempo (overlay only)
└── logs/
    └── app.log                     # bind-mounted at /var/log/sample/app.log;
                                    # the log generator appends here
```

All fake data comes from the host-run generators in
[`scripts/`](../../../scripts/) — there is no in-compose data generator.

## Cleanup

```bash
docker compose down -v
# or, if you ran the traces overlay:
docker compose -f docker-compose.yml -f docker-compose.traces.yml down -v
```

## Reference

[Migration from standalone metric and trace tools](../../../src/migration/migration-metric-trace-tools.md)
