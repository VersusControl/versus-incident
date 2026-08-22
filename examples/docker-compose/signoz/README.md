# SigNoz-source example

Versus + Redis + a self-hosted [SigNoz](https://signoz.io) stack. The agent
ingests log rows via SigNoz's v5 query API (`POST /api/v5/query_range`,
signal `logs`) — the same endpoint and the same `SIGNOZ-API-KEY` header that
SigNoz Cloud uses, so the source config here transfers to Cloud unchanged.

> **Resource cost — read this first.** This is by far the heaviest example in
> this folder. SigNoz self-hosted is ClickHouse + ZooKeeper + a schema migrator
> + an OTel collector + the SigNoz server, and ClickHouse alone wants ~2 GB.
> **Give Docker at least 4 GB of memory** (Docker Desktop → Settings →
> Resources). By comparison the [loki/](../loki/) example next door runs in a
> few hundred MB. If containers die with exit code 137, that is the OOM killer,
> not a bug in the example.

## Services

| Service | Port |
|---|---|
| versus-incident | `3000` |
| signoz (UI + query API) | `8080` |
| otel-collector (OTLP) | `4317` gRPC, `4318` HTTP |
| clickhouse / zookeeper / redis | (internal) |

## Image pins are ours, deliberately

SigNoz **deprecated `install.sh` and its bundled `deploy/` Compose files at
v0.130.0**; `foundryctl` is the supported installer now and it *generates*
compose rather than shipping it. This example therefore **pins its own images**
so it is deterministic and needs no third-party binary at test time:

| Image | Tag |
|---|---|
| `signoz/signoz` | `v0.129.0` (last release that shipped a reference compose) |
| `signoz/signoz-otel-collector` | `v0.144.5` (collector versions are independent of SigNoz's) |
| `clickhouse/clickhouse-server` | `25.5.6` |
| `signoz/zookeeper` | `3.7.1` |

**SigNoz does not support this shape.** If it breaks against a newer SigNoz,
bump `SIGNOZ_TAG` / `SIGNOZ_OTELCOL_TAG` here — do not report it upstream.

**Compatibility floor: SigNoz v0.87.0.** `/api/v5/query_range` first appears at
v0.87.0 (present in the tagged sources there, absent at v0.86.0). Anything
older cannot serve this source at all. Pin at or above the floor.

One thing in the stack is fetched at runtime rather than pinned: the
`init-clickhouse` one-shot downloads SigNoz's `histogramQuantile` executable UDF
from GitHub releases. It is the same fetch SigNoz's own compose does, and the
stack will not answer quantile queries without it.

One deliberate deviation from SigNoz's own compose: the collector runs on its
**static config only**, without `--manager-config`. With OpAMP enabled the
collector pulls config from the SigNoz server and restarts its inner service
twice during the first minute, dropping every OTLP connection in that window —
so the first `generate_noisy_logs.py --target signoz` after `up -d` would fail.
The config here is fixed, so OpAMP buys nothing. The cost is that SigNoz's UI
cannot push logs-pipeline changes to this collector.

## Run

```bash
docker compose up -d
```

Everything is `${VAR:-default}`, so this needs zero configuration. First boot
takes ~1 minute: the ClickHouse schema migration runs before SigNoz starts.

### The API key

The SigNoz query key is the one credential that **cannot** be a compose default,
because SigNoz generates its value server-side. The `signoz-bootstrap` one-shot
therefore does what you would otherwise click through in the UI — registers the
first admin, creates a read-only service account, grants it the `signoz-viewer`
role, and mints a key — then writes it to a private volume that the versus
container reads at startup. Nothing is committed and nothing is printed.

Dev defaults (override by exporting them before `up`):

| Variable | Default | What |
|---|---|---|
| `SIGNOZ_ADMIN_EMAIL` | `admin@versus.local` | SigNoz UI login |
| `SIGNOZ_ADMIN_PASSWORD` | `Versus-Dev-12345` | SigNoz UI password (SigNoz enforces ≥12 chars with upper/lower/digit/symbol) |
| `SIGNOZ_API_KEY` | *(empty → minted)* | set it to skip minting and use your own key |
| `SIGNOZ_QUERY` | *(empty → match all)* | a v5 filter expression, e.g. `severity_text = 'ERROR'` |

Log in to the SigNoz UI at <http://localhost:8080> with those credentials to
browse the same logs the agent is reading.

## Generate test traffic

The bundled generator pushes over **OTLP/HTTP** to the collector, so the real
ingest path is exercised — not a shortcut into ClickHouse. Run from the repo
root:

```bash
# 500 mixed lines once:
python3 scripts/generate_noisy_logs.py --target signoz --lines 500

# Continuous — 20 lines every 5s, Ctrl+C to stop:
scripts/run_noisy_logs.sh --target signoz

# Spike (test the spike detector):
scripts/run_noisy_logs.sh --target signoz --spike panic --spike-burst 80

# Curated incident cluster (test detect mode):
scripts/run_noisy_logs.sh --target signoz --scenario db-outage
```

The generator is stdlib-only and encodes OTLP as **JSON**, which the collector's
OTLP/HTTP receiver accepts alongside protobuf.

Pick a `--spike` template whose *message* trips the agent's regex — the default
rule is `(?i).*error.*` matched against the log **body**, and OTLP keeps the
level in `severity_text` rather than in the body. `panic` works; `db-conn-refused`
writes `connection refused ...` and will be filtered out. (This is the same
behaviour as the Loki example, which also keeps the level out of the line.)

Or push a single line by hand:

```bash
TS=$(python3 -c 'import time; print(time.time_ns())')
curl -X POST http://localhost:4318/v1/logs \
  -H 'Content-Type: application/json' \
  -d "{\"resourceLogs\":[{
        \"resource\":{\"attributes\":[
          {\"key\":\"service.name\",\"value\":{\"stringValue\":\"demo\"}}]},
        \"scopeLogs\":[{\"logRecords\":[{
          \"timeUnixNano\":\"${TS}\",
          \"severityText\":\"ERROR\",
          \"severityNumber\":17,
          \"body\":{\"stringValue\":\"db connection refused error\"}}]}]}]}"
```

## Verify

```bash
SECRET=${GATEWAY_SECRET:-change-me}
curl -H "X-Gateway-Secret: $SECRET" http://localhost:3000/api/agent/patterns | jq
docker compose logs -f versus     # watch the per-tick line
```

A healthy tick looks like:

```
agent: tick signoz:demo-signoz signals=300 matched=16 patterns=10 skipped_no_match=284 verdicts=map[learned:10] cursor=2026-08-20T08:37:02Z
```

## Field names

SigNoz rows are not flat. `body`, `severity_text`, `id` and `timestamp` sit at
the top level, but OTLP resource and log attributes live in **nested maps whose
keys still contain the dots**:

```json
{ "body": "...", "severity_text": "ERROR",
  "resources_string": { "service.name": "checkout" },
  "attributes_string": { "level": "error" } }
```

So a dotted name is resolved against those containers rather than as a nested
JSON path. Write it **container-qualified** (`resources_string.service.name`) to
pin exactly which map you mean, or **bare** (`service.name`) to have it searched
across the containers in a fixed order — `resources_string` first, then
`attributes_string`, `attributes_number`, `attributes_bool`, `scope_string`.
Both spellings land in `Signal.Fields` under the bare attribute name. Naming a
container on its own (`resources_string`) still copies the whole map.

The service on an incident comes from the agent's `service_patterns` reading the
message, the same as every other log source.

## Layout

```
signoz/
├── docker-compose.yml
├── config/
│   ├── config.yaml
│   └── agent_sources.yaml
└── signoz/
    ├── bootstrap.py                    # mints the query API key on first boot
    ├── otel-collector-config.yaml
    └── clickhouse/
        ├── cluster.xml                 # zookeeper + the single-shard `cluster`
        └── histogram_quantile_function.xml
```

## Cleanup

```bash
docker compose down -v
```

That drops every volume, including the minted API key and SigNoz's SQLite state,
so the next `up` bootstraps from scratch.

## Reference

[SigNoz source docs](https://docs.versusincident.com/#/agent/data-sources/signoz)
