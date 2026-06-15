# Metrics & traces *correlation* example (OSS)

Versus + Redis + **Prometheus** + a **Pushgateway** — demonstrating what the
**open-source** build does with metrics and traces: **on-demand correlation**
during AI analysis via the `query_metrics` and `query_traces` tools. It is the
analogue of the [loki/](../loki/) example, but instead of standing up a detector
it shows the agent *pull* metric/trace context while investigating an incident.

All fake data is produced by the host-run generators in
[`scripts/`](../../../scripts/) — the same convention as the log examples:
[`generate_noisy_logs.py`](../../../scripts/generate_noisy_logs.py) fires the
incident through the `file` log source, and
[`generate_fake_metrics.py`](../../../scripts/generate_fake_metrics.py) pushes
the correlating Prometheus series to the Pushgateway.

> **OSS vs Enterprise — read this first.**
> In the open-source build, metrics and traces are **investigation tools**, not
> a detector. The `query_metrics` / `query_traces` analyze tools run on-demand
> PromQL / TraceQL **while the AI investigates an incident** and cite what they
> find.
>
> The standing **metric/trace _data source_** — a PromQL/TraceQL rule that
> *starts* an incident on its own (`type: prometheus` / `type: traces` in
> `agent_sources.yaml`) — is a **Versus Enterprise** feature. On the OSS image
> those source types return a "requires Versus Enterprise" error, so this
> example does **not** configure one. For the standing detect path see the
> Enterprise metrics data-source example and the
> [Data Sources docs](https://docs.versusincident.com/#/agent/data-sources)
> (Enterprise).
>
> To still show something end-to-end on OSS, this example **triggers incidents
> through a plain `file` log source** (an OSS source). The host-run log
> generator appends synthetic app logs; a `--spike 5xx` burst makes it emit
> `level=error … status=503` lines that start an incident — and the AI-analyze
> loop then pulls `query_metrics` / `query_traces` from the (fake) Prometheus /
> Tempo data to correlate.

## Services

| Service | Port | What |
|---|---|---|
| versus-incident | `3000` | the agent (tails `./logs/app.log`) |
| prometheus | `9090` | scrapes the pushgateway; queried on-demand by `query_metrics` |
| pushgateway | `9091` | receives the synthetic series pushed by `scripts/generate_fake_metrics.py` |
| redis | `6379` | state — **TLS-only** (self-signed cert) behind a `requirepass` password (`REDIS_PASSWORD`, default `versus`) |

## Run

```bash
docker compose up -d
```

Wait ~15s for Prometheus and Versus to go healthy. All settings have safe
defaults (see [../README.md](../README.md) to override `GATEWAY_SECRET`,
enable Slack, etc.).

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

## Trigger an incident

Two host-run steps, both using the `scripts/` generators:

```bash
# 1. Fire the OSS incident: append a burst of status=503 error lines to the
#    file the `file` source tails (this is what STARTS the incident on OSS).
python3 scripts/generate_noisy_logs.py --append --start-time now \
  --spike 5xx --spike-burst 80 \
  --output examples/docker-compose/metrics/logs/app.log

# 2. Push the correlating metric anomaly: ~45% 500s + p95 > 500ms, so
#    query_metrics has a real spike to cite during the investigation.
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

Within one agent tick (`poll_interval: 15s`) the `status=503` lines start an
incident.

## What you'll see

Watch the agent:

```bash
docker compose logs -f versus
```

On the next tick after the log spike, the `file` source picks up the error lines
and the worker classifies them (e.g. the `http-5xx` / `slow-request` patterns
from `config/config.yaml`). Inspect what the agent learned:

```bash
SECRET=${GATEWAY_SECRET:-change-me}
curl -H "X-Gateway-Secret: $SECRET" http://localhost:3000/api/agent/patterns | jq
```

You can cross-check the matching metric spike in Prometheus
(<http://localhost:9090>) — e.g. run
`sum by (service) (rate(demo_http_requests_total{code=~"5.."}[1m]))` in the
*Graph* tab and watch it climb after the metric spike. That series is exactly
what `query_metrics` reads during the investigation below.

### See `query_metrics` correlation during investigation (needs an API key)

The detect path above works with **no API key**. To see the agent pull
`query_metrics` while *investigating* the incident, enable the AI analyzer:

```bash
AGENT_AI_ENABLE=true AGENT_AI_API_KEY=sk-... AGENT_AI_MODEL=gpt-4o-mini \
  docker compose up -d --force-recreate versus
# fire the trigger + metric spike again (the two commands above)
docker compose logs -f versus      # watch for query_metrics tool calls
```

With AI enabled, the error-log incident triggers the analyze agent, which calls
`query_metrics` (configured in [config/tools.yaml](./config/tools.yaml)) to pull
the relevant Prometheus series before writing its finding — turning a raw "503s
in the logs" signal into "5xx rate on `checkout` crossed 0.5 req/s at HH:MM".

## What each mode does (OSS, no overclaiming)

`AGENT_MODE` (default `detect` here) governs the **log** source — metrics and
traces enter **only** through the `query_metrics` / `query_traces` analyze tools
during an AI investigation, never as a standing detector on OSS.

| Mode | What the `file` (log) source does | Metrics / traces |
|---|---|---|
| `training` | observes & learns log patterns only — no verdict, no incident | not consulted |
| `shadow` | classifies log lines; logs a "would alert" + records to the shadow file; **no** incident | not consulted |
| `detect` | classifies log lines; with AI enabled, the analyzer runs **and** pulls `query_metrics` / `query_traces` to correlate, then an incident is emitted; with AI off, it logs a "dry detect" | pulled on-demand by the analyze tools |

> **The honest framing:** in OSS, *detection* is driven by the log source +
> your patterns. The metric/trace value is in **investigation and
> correlation** — the `query_metrics` / `query_traces` tools pulling related
> series/spans during an incident — not in deciding whether a metric is
> anomalous. A standing metric/trace **detector** is a Versus Enterprise
> feature.

> This example sets `new_service_grace: 0` so the first spike surfaces
> immediately. In production a non-zero grace window suppresses alerts for
> freshly-discovered services while the agent learns their baseline.

## Optional: traces correlation (Tempo)

Traces are kept out of the main path to keep it light. To also exercise the
`query_traces` **tool**, bring up the overlay, which adds Tempo and swaps in
`tools.traces.yaml` (which adds `query_traces` alongside `query_metrics`):

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

With AI enabled `query_traces` pulls redacted span summaries during the
investigation. Tempo's API is on `:3200`, OTLP on `:4318`. (As above, there is
**no** standing `traces` *source* — that's Enterprise; the overlay only adds the
OSS correlation tool plus a Tempo backend for it to read.)

## Layout

```
metrics/
├── docker-compose.yml              # versus + redis + prometheus + pushgateway
├── docker-compose.traces.yml       # optional overlay: + tempo + query_traces tool
├── config/
│   ├── config.yaml                 # mode=detect, new_service_grace=0
│   ├── agent_sources.yaml          # file (log) source — the OSS trigger
│   ├── tools.yaml                  # query_metrics tool
│   └── tools.traces.yaml           # + query_traces tool (overlay only)
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

[AI Agent — Analyze Tools (`query_metrics` / `query_traces`)](https://docs.versusincident.com/#/agent/analyze-tools/tools)
