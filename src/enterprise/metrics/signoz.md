# SigNoz Metrics Guide

_Enterprise_

A hands-on walkthrough: point the `signoz_metrics` data source at
[SigNoz](https://signoz.io/) — Cloud or self-hosted — let Versus
**discover your services from recent spans**, **probe a metric catalog**
against each one, **learn each signal's baseline**, and open a real
incident when one clearly deviates and stays deviated.

Unlike the [Prometheus](./prometheus.md), SigNoz exposes no way to
*enumerate* metric names over its query API, so discovery works
differently here — read [What it discovers](#3-what-it-discovers) before
you decide this source sees everything you expect.

## Prerequisites

| Need | Why |
|---|---|
| **SigNoz v0.87.0 or later** (Cloud or self-hosted) | the source reads `POST /api/v5/query_range`, which first appears in `v0.87.0` (verified at source level against the tagged releases) |
| **Traces flowing into SigNoz** | services are discovered from recent spans, and SigNoz's default span metrics are derived from traces — without traces this source degrades to a global scope (see [limitations](#limitations)) |
| A SigNoz **query API key** | authentication; created under **Settings → API Keys**, which needs the **Admin** role. Available on Cloud **and** self-hosted community |
| A **Versus Enterprise license** with the **`intelligence`** entitlement, supplied via the `LICENSE_KEY` environment variable | the standing `signoz_metrics` source is gated on this feature |
| An **AI API key** (e.g. OpenAI) | the detect AI that triages the anomaly and writes the incident summary |

> **First time running Enterprise?** Start with
> [Getting Started — Running the Enterprise Agent](../getting-started.md). It
> covers signing in as the default admin, turning on AI, and switching modes
> from the UI — the controls this walkthrough uses.

> **Use a query API key, not an ingestion key.** The source
> authenticates with the **`SIGNOZ-API-KEY`** header, identical on Cloud
> and self-hosted. The Cloud-only `signoz-ingestion-key` that shippers
> use to *write* telemetry will **not** work for queries.

## 1. Bring up SigNoz

If you do not already run SigNoz, the OSS example at
`examples/docker-compose/signoz/`
([on GitHub](https://github.com/VersusControl/versus-incident/tree/main/examples/docker-compose/signoz))
brings up a full stack with `docker compose up -d`.

> **It is a heavy stack** — ClickHouse, ClickHouse Keeper, Postgres and
> an OTel collector. Budget **≥4 GB** of Docker memory.

Then create the query key in the SigNoz UI under **Settings → API Keys**
and export it:

```bash
export SIGNOZ_API_KEY=...    # never commit this
```

## 2. Configure the source

Declare the enterprise `signoz_metrics` source in
[config/agent_sources.yaml](../../../config/agent_sources.yaml) — the file
Versus loads from next to `config.yaml`. The documented path is
**connection-only**: an address and a key. You author no queries.

```yaml
sources:
  - name: demo-signoz-metrics
    type: signoz_metrics
    enable: true
    options:
      address: http://signoz:8080          # self-hosted UI/API port
      # address: https://<region>.signoz.cloud
      api_key: ${SIGNOZ_API_KEY}
```

## 3. What it discovers

This is the part that differs from Prometheus, and it is worth
understanding before you trust the watch-set.

**Services come from spans.** SigNoz's metrics signal has no list view,
so the source enumerates services from a bounded sample of **raw spans**
— the discovery lookback is split into 6 equal buckets and one small
span query is issued per bucket, so the sample is spread across the
window instead of pinned to the last few seconds of the busiest service.
It reads the first attribute it finds of `service.name`,
`resource.service.name`, `serviceName`, `service`.

**Metric names come from a catalog.** The v5 query API cannot enumerate
metric names at all, so there is no honest way to find them
automatically. The default catalog is SigNoz's **own span metrics**:

| Metric | Why it is the default |
|---|---|
| `signoz_calls_total` | SigNoz derives it from ingested traces, so it exists wherever this source has anything to watch |
| `signoz_latency.bucket` | same — the latency histogram behind SigNoz's own charts |

**Mind the spelling: a DOT before a histogram component.** SigNoz stores
the parts of a histogram family — `bucket`, `sum`, `count` — as **dotted**
suffixes (`signoz_latency.bucket`,
`http_server_duration.sum`), while a base counter keeps whatever the
producer named it, which for Prometheus-style counters is `_total`
(`signoz_calls_total`). Both spellings therefore live side by side on the
same deployment. Naming the underscored form of a histogram component
(`signoz_latency_bucket`) is not an error you will see: the v5 API answers
an unknown metric with a **successful, empty** result, so the metric is
simply never watched. Copy the name exactly as SigNoz stores it.

**Every (service, metric) pair is probed once per discovery pass.** A
pair that answers with no finite sample is never watched, so nothing is
guessed onto the watch-set — every entry was observed. Probing is
bounded at **500 probes per pass**; a large deployment stops there with
a partial watch-set (and says so in the discovery notes) rather than
turning a discovery pass into a request burst.

**Anything outside the catalog is invisible** unless you name it in
`metrics:`. Host metrics, custom application metrics and OTel receiver
metrics are all in that category:

```yaml
options:
  address: http://signoz:8080
  api_key: ${SIGNOZ_API_KEY}
  metrics:                                  # ADDED to the default catalog
    - http_server_duration.bucket
    - system_memory_usage
```

`metrics:` **adds to** the default catalog — it does not replace it, so
naming one custom metric cannot silently switch off the span metrics the
source relies on. Order is preserved and duplicates are dropped, so the
probe order (and therefore which metrics survive the per-service cap) is
deterministic.

### Scoping with `filter:`

`filter:` takes a **SigNoz v5 filter expression** and is ANDed into every
probe and every sample, so one line narrows the whole source. Attribute
names are **dotted** — this is not PromQL:

```yaml
options:
  address: http://signoz:8080
  api_key: ${SIGNOZ_API_KEY}
  filter: "deployment.environment = 'prod' AND k8s.namespace.name = 'payments'"
```

Your expression is parenthesised before the generated `service.name`
clause is appended, so a top-level `OR` scopes only what you wrote and
cannot widen the watch-set. A malformed expression (unterminated quote,
unbalanced parentheses) is refused **at boot** rather than at query time,
because SigNoz answers a bad-but-parseable filter with an empty
*successful* result — which would otherwise look like a source that
quietly watches nothing.

Use `filter:` to narrow by environment, namespace or tenant, and
`metrics:` to add metrics the catalog does not already probe.

**How each metric is read** is decided by its name suffix — the same
shape heuristic the Prometheus path uses. It matters: reading a
monotonic counter with the backend's default reducer would teach the
brain a baseline that only ever climbs.

| Name ends with | Read as | Golden signal |
|---|---|---|
| `.bucket` or `_bucket` | `p99` across series | latency |
| `.sum` / `.count`, or `_total` / `_count` / `_sum` | `rate` over time, `sum` across series | traffic |
| anything else | `avg` / `avg` | saturation |

Both spellings are recognised because both occur: SigNoz writes histogram
family components dotted, and base counters keep the producer's `_total`.

## 4. Understand the three modes

Versus Enterprise runs in one of three **agent modes**. You pick the mode
via `AGENT_MODE`. SigNoz is a **standing baseline** source, so the
sequence matters — always learn first:

| Mode | What it does | Use when |
|---|---|---|
| `training` | Discovers signals and **learns their baseline** (normal behavior). No alerts, no incidents — pure observation. | First run — let it watch healthy traffic and build a model of "normal." |
| `shadow` | Scores every signal against the learned baseline. Writes **"would have alerted"** verdicts to the UI — but **pages no one**. | Validating that the learned baseline is accurate before going live. |
| `detect` | Opens a **real incident automatically** when a signal deviates from its baseline and stays deviated. A lightweight **AI classification** writes the incident's title, severity, and summary. | Production — the payoff mode. |

Think of the agent like a new on-call engineer: first it *watches* and
learns each service's normal rhythm, then it *double-checks* quietly,
then it *acts*.

## 5. Run the agent

Start the Enterprise image in `training` mode. From the repo root:

```bash
docker run --rm --name versus-enterprise \
  --network host \
  -v "$PWD/config:/app/config" \
  -v "$PWD/data:/app/data" \
  -e REDIS_HOST=localhost -e REDIS_PORT=6379 -e REDIS_PASSWORD=versus \
  -e LICENSE_KEY=... \
  -e SIGNOZ_API_KEY=... \
  -e AGENT_ENABLE=true \
  -e AGENT_AI_ENABLE=true \
  -e AGENT_AI_API_KEY=sk-... \
  -e AGENT_AI_MODEL=gpt-4o-mini \
  -e AGENT_MODE=training \
  ghcr.io/versuscontrol/versus-enterprise:latest
```

On boot you should see a discovery line and a clean start:

```text
agent: standing metric source "demo-signoz-metrics" discovered 12 signal(s) across 6 service(s) (label="service.name" generic=false metrics=2 notes=[])
enterprise: agent started (mode=training, sources=1)
```

`sources=1` and **no** `requires Versus Enterprise` line confirm the
license unlocked the source. `generic=false` confirms services were
attributed; `generic=true` means it fell back to a global scope (see
[limitations](#limitations)).

There is **no** `auto-wired query_metrics tool` line — that is expected,
not a failure. See [limitations](#limitations).

Then work through the modes exactly as in the
[Prometheus guide](./prometheus.md#3-understand-the-three-modes):
learn in `training`, review on the **Shadow** page, then switch to
`detect` and let a real deviation page you. Baselines are **persisted**
and reload across restarts — switching mode never starts the learning
over.

## Options

All options live under `options:`. Only `address` and `api_key` are
required; every other field defaults, so the documented path is
connection-only.

| Key | Default | Meaning |
|---|---|---|
| `address` | — (required) | SigNoz base URL — the UI/API port self-hosted, or `https://<region>.signoz.cloud`. |
| `api_key` | — (required) | Sent as the `SIGNOZ-API-KEY` header. The **query** key from Settings → API Keys. |
| `insecure_skip_verify` | `false` | Skip TLS verification — **local dev only**, never production. |
| `step` | `60s` | Sampling resolution (the v5 `stepInterval`). |
| `metrics` | built-in catalog | Metric names **added to** the default catalog. Histogram components are dotted; base counters keep the producer's `_total`. |
| `filter` | unset | SigNoz v5 filter expression ANDed into every read, e.g. `deployment.environment = 'prod'`. Dotted attribute names; not PromQL. |
| `max_services` | `50` | Service-enumeration cap. |
| `max_signals` | `200` | Per-tenant signal cap. |
| `max_per_service` | `6` | Per-service signal cap. |
| `discovery_interval` | `1h` | How often the watched set is rebuilt (cadence ceiling). |
| `discovery_lookback` | `1h` | Window discovery samples over. Floored at `5m`. |
| `discovery_samples` | `200` | Span rows per discovery bucket (6 buckets). Capped at `1000`. |
| `org` | unset | Tenant scope for learned baselines. |

`address` and `api_key` are the documented surface; the caps and
discovery knobs are optional bounds you reach for only when a large
deployment needs them.

## Cost and scale

One discovery pass costs **6 span queries + (services × metrics)
probes**, hard-bounded at 500 probes, and runs at most once per
`discovery_interval`. Sampling costs **one request per watched signal
per tick** — deliberately never a batched composite query, because a
single rejected metric in a batch would blank the whole tick.

Narrow the scope with `max_services` / `max_per_service` / `max_signals`
before you reach for a longer `discovery_interval`.

## Limitations

These are real gaps in the first release, not configuration mistakes.

- **No analyze auto-wire.** A configured `signoz_metrics` source does
  **not** populate the `query_metrics`
  [analyze tool](../../agent/tools/tools.md) — unlike the
  `prometheus` source, which does. Configure `tools.query_metrics` by
  hand, or keep pointing it at Prometheus. Planned, not shipped.
- **Metric discovery is catalog-based.** Metrics outside the default
  catalog are invisible unless you name them in `metrics:`. The v5 query
  API cannot enumerate metric names, so there is no automatic
  alternative today.
- **Per-service attribution needs traces.** Services are discovered from
  spans. With no `service.name` observed, discovery reports
  `generic=true` and watches each catalog metric **globally** under an
  `_all_` pseudo-service rather than going dark — useful, but you lose
  per-service isolation.

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `requires Versus Enterprise`, `sources=0` | The license is missing the **`intelligence`** feature (or you are on an OSS build). Mint a key that includes `intelligence`. This is the open-core line: OSS keeps only the on-demand `query_metrics` tool, not the standing source. |
| `401` / `403` on every read | An **ingestion** key was used instead of a **query** API key, or the key was revoked. |
| `404` on `/api/v5/query_range` | SigNoz is older than **v0.87.0**. Upgrade. |
| `discovered 0 signal(s)` | No spans in the discovery lookback, or none of the catalog metrics answered with a finite sample for any service. Confirm traces are flowing, then widen `discovery_lookback`. |
| `generic=true` in the discovery line | No `service.name` was observed on sampled spans — the source degraded to a global scope. Check your instrumentation sets the resource attribute. |
| A metric you care about is never watched | It is outside the default catalog — add it with `metrics:`. |
| A metric you do not want learned | Add it to the org **Disable-Learn** policy (`metrics`, exact names or globs). Narrowing `filter:` stops it being read at all. |
| `probe budget (500) spent; watch-set is partial` in the notes | The deployment is larger than one discovery pass can probe. Lower `max_services`, or narrow with `filter:` so the budget covers what matters. |
| No `auto-wired query_metrics tool` line at boot | Expected — see [limitations](#limitations). |

## See also

- The Prometheus twin of this guide: [Prometheus Guide](./prometheus.md)
- Enterprise metrics overview & licensing: [Overview](./overview.md)
- SigNoz traces (Enterprise): [Traces](../../agent/data-sources/traces.md#signoz-backend)
- SigNoz logs (OSS): [SigNoz source](../../agent/data-sources/signoz.md)
