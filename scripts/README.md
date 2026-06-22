# Scripts

Helper scripts for generating test traffic against the Versus AI agent.

The main generator is
[`generate_noisy_logs.py`](generate_noisy_logs.py), wrapped by
[`run_noisy_logs.sh`](run_noisy_logs.sh) for live-tail and one-shot
spike / scenario modes. It can write to a **local file** or push the
same generated lines directly into **Loki**, **Elasticsearch**,
**CloudWatch Logs**, **Graylog** (GELF UDP), or **Splunk** (HEC) —
selected via `--target` (or `TARGET=`).

| Target | Flag / env | Default endpoint |
|---|---|---|
| `file` (default) | `-o PATH` / `OUTPUT=` | `local/resource/noisy-app.log` |
| `loki` | `--loki-url` / `LOKI_URL=` | `http://localhost:3100` |
| `elasticsearch` | `--es-url` / `ES_URL=` (+ `--es-index` / `ES_INDEX=`) | `http://localhost:9200`, index `logs-noisy` |
| `cloudwatch` | `--cw-log-group` / `CW_LOG_GROUP_NAME=` (+ `--cw-region`) | — (requires `boto3` + AWS creds) |
| `graylog` | `--graylog-host` / `GRAYLOG_HOST=` (+ `--graylog-port`) | `localhost:12201` (GELF UDP) |
| `splunk` | `--splunk-token` / `SPLUNK_HEC_TOKEN=` (+ `--splunk-url`) | `https://localhost:8088` (HEC) |

The `elasticsearch`-via-`makelogs` flavour is also kept around (see
[§2](#2-elasticsearch-source--makelogs)) — useful when you want
Elastic's own canned HTTP-traffic fixtures rather than the Versus
templates.

Pick whichever matches the source you've enabled in
`config/agent_sources.yaml`.

For **metrics** (rather than logs) there is a separate generator,
[`generate_fake_metrics.py`](generate_fake_metrics.py), which pushes
synthetic Prometheus series to a Pushgateway — see
[§3](#3-metrics-source--fake-prometheus-series).

---

## 1. File source — local noisy logs

Generates an application-style log file with a mix of common INFO/WARN lines
and rare ERRORs (panics, OOM, deadlocks, 5xx, etc.) so the agent can cluster
the noise and surface the anomalies.

### One-shot generation

```bash
# 2000 lines, default output path local/resource/noisy-app.log
python3 scripts/generate_noisy_logs.py

# custom size + reproducible
python3 scripts/generate_noisy_logs.py --lines 5000 --seed 42

# write somewhere else
python3 scripts/generate_noisy_logs.py -o /tmp/test.log -n 500

# push the same content into other backends (see "Targets" above):
python3 scripts/generate_noisy_logs.py --target loki --lines 500
python3 scripts/generate_noisy_logs.py --target elasticsearch --lines 500
python3 scripts/generate_noisy_logs.py --target cloudwatch \
  --cw-log-group /aws/lambda/my-fn --cw-region us-east-1 --lines 500
python3 scripts/generate_noisy_logs.py --target graylog --lines 500
python3 scripts/generate_noisy_logs.py --target splunk \
  --splunk-token "$SPLUNK_HEC_TOKEN" --lines 500
```

Useful flags: `--lines/-n`, `--output/-o`, `--start-time` (`now` or RFC3339),
`--interval-min/--interval-max`, `--append/-a`, `--seed`, `--target`,
`--batch-size`.

### Spike mode

Emit a tight burst of one specific error template so the agent's spike
detector fires. Use this after training to verify the spike settings work.

**Typical workflow:**

```bash
# 1. Train — build a baseline (2000 mixed lines)
python3 scripts/generate_noisy_logs.py --lines 2000

# 2. Switch the agent to shadow mode (AGENT_MODE=shadow) and restart

# 3. Inject a spike — 80 db-conn-refused errors packed into ~16 seconds
python3 scripts/generate_noisy_logs.py --append --start-time now \
  --spike db-conn-refused --spike-burst 80

# 4. Check the shadow log for spike verdicts
curl -H "X-Gateway-Secret: $GATEWAY_SECRET" \
  http://localhost:3000/api/agent/shadow | jq '.[] | select(.verdict=="spike")'
```

When `--spike` is set, `--lines` is ignored. The script emits exactly
`--spike-burst` lines of the chosen template with tight spacing (default
0.0–0.2 s between lines) so they all land in one or two poll ticks.

> **Why your spike might not fire.** A spike compares the current tick
> to a *prior* baseline, so the pattern must already exist in
> `data/patterns.json` with enough history. Specifically:
>
> - The pattern's `count` must be `≥ spike_min_baseline_count`
>   (default `20`).
> - The pattern's service must NOT be inside its `new_service_grace`
>   window (env `AGENT_NEW_SERVICE_GRACE`).
>
> If you delete `data/patterns.json` and immediately inject a burst,
> you'll see `verdicts=map[grace:1]` or `map[unknown:1]` and no
> `SPIKE` log line. Either run the training step first and wait out
> the grace period, or end grace early with
> `POST /api/agent/services/<name>/grace` with body `{"action":"end"}`.

**Spike flags:**

| Flag | Default | Description |
|---|---|---|
| `--spike NAME` | — | Template to burst. Use `auto` for a random pick. |
| `--spike-burst N` | `50` | Number of lines in the burst. |
| `--spike-interval-min S` | `0.0` | Min seconds between burst lines. |
| `--spike-interval-max S` | `0.2` | Max seconds between burst lines. |
| `--spike-context N` | `0` | Regular noisy lines to emit before the burst. |
| `--list-templates` | — | Print all template names and exit. |

```bash
# see all available template names
python3 scripts/generate_noisy_logs.py --list-templates

# random template, 60 lines
python3 scripts/generate_noisy_logs.py --append --start-time now \
  --spike auto --spike-burst 60

# add 20 normal lines before the burst for context
python3 scripts/generate_noisy_logs.py --append --start-time now \
  --spike panic --spike-burst 40 --spike-context 20
```

### Live tail (interval mode)

Append fresh batches forever so the agent (running in another terminal via
`./run.sh`) sees new traffic continuously:

```bash
# defaults: 20 lines every 5s into local/resource/noisy-app.log
./scripts/run_noisy_logs.sh

# faster + bigger batches
INTERVAL=2 BATCH=50 ./scripts/run_noisy_logs.sh

# stop after 30 batches
./scripts/run_noisy_logs.sh --interval 1 --batch 10 --iter 30

# push to a different backend instead of a file:
./scripts/run_noisy_logs.sh --target loki
./scripts/run_noisy_logs.sh --target elasticsearch
TARGET=cloudwatch CW_LOG_GROUP_NAME=/aws/lambda/foo ./scripts/run_noisy_logs.sh
./scripts/run_noisy_logs.sh --target graylog
SPLUNK_HEC_TOKEN=... ./scripts/run_noisy_logs.sh --target splunk
```

Stop with Ctrl+C. The script prints a summary count on exit.

### One-shot spike via `run_noisy_logs.sh`

The wrapper also has a spike shortcut. When `--spike` (or `SPIKE=`) is set
the live-tail loop is skipped — the script does one burst and exits, so the
lines all land in a single poll tick.

```bash
# 80 db-conn-refused lines into the default output path
./scripts/run_noisy_logs.sh --spike db-conn-refused

# environment-variable form, larger burst, 20 normal lines first
SPIKE=panic SPIKE_BURST=120 SPIKE_CONTEXT=20 ./scripts/run_noisy_logs.sh

# discover available template names
./scripts/run_noisy_logs.sh --list-templates
```

| Flag | Env | Default | Description |
|---|---|---|---|
| `--spike NAME` | `SPIKE` | — | Template to burst (use `auto` for random). |
| `--spike-burst N` | `SPIKE_BURST` | `80` | Number of lines in the burst. |
| `--spike-context N` | `SPIKE_CONTEXT` | `0` | Regular lines emitted before the burst. |
| `--list-templates` | — | — | Print all template names and exit. |

The same baseline + grace prerequisites apply — see the warning in the
spike-mode section above.

### Make sure the agent is reading from this file

In [`config/agent_sources.yaml`](../config/agent_sources.yaml):

```yaml
sources:
  - name: noisy-app
    type: file
    enable: true
    file:
      path: ./local/resource/noisy-app.log
      format: text
      from_beginning: true
```

If you want a clean run, clear the agent's stored state first:

```bash
rm -f data/patterns.json \
      local/resource/.versus-cursor-noisy-app
```

---

## 2. Elasticsearch source — makelogs

Uses [`@elastic/makelogs`](https://github.com/elastic/makelogs) to push fake
HTTP-traffic events into Elasticsearch indexes (default `logstash-YYYY.MM.DD`).

### Start Elasticsearch (Docker)

[`ensure_elasticsearch.sh`](ensure_elasticsearch.sh) is idempotent — it probes
`http://localhost:9200` first and only spins up a container if needed.

```bash
# default: docker.elastic.co/elasticsearch/elasticsearch:8.13.4 on :9200
./scripts/ensure_elasticsearch.sh

# pin a different version / port / name
ES_VERSION=8.14.0 ES_PORT=9201 ES_NAME=my-es ./scripts/ensure_elasticsearch.sh
```

The container runs single-node with security disabled (local dev only — do
not expose this beyond `localhost`). Tear down when finished:

```bash
docker rm -f versus-es
```

### Push events with makelogs

[`run_makelogs.sh`](run_makelogs.sh) auto-runs `ensure_elasticsearch.sh`
first, then invokes makelogs (via `npx` if `makelogs` isn't installed
globally — Node.js 16+ required).

```bash
# 10k events spread over the last 1 day
./scripts/run_makelogs.sh

# bigger backfill + reset existing indices
COUNT=50000 DAYS=7 ./scripts/run_makelogs.sh --reset

# loop mode: 5k events every 60s, 10 batches total — keeps fresh data
# landing inside the agent's `lookback` window
INTERVAL=60 ITER=10 COUNT=5000 ./scripts/run_makelogs.sh
```

Verify the data landed:

```bash
curl -s 'http://localhost:9200/_cat/indices/logstash-*?v'
```

You should see a `logstash-YYYY.MM.DD` index with a non-zero `docs.count`.
If the table is empty, makelogs failed to push — re-check the host and
auth flags above.

Useful flags: `--host`, `--auth`, `--count/-c`, `--days/-d`, `--index-prefix`,
`--index-interval`, `--reset`, `--insecure`, `--interval`, `--iter`,
`--no-ensure-es`.

> **Index naming.** The script defaults to `--index-prefix logstash-` and
> `--index-interval daily`, which produces `logstash-YYYY.MM.DD` indices
> matching what `agent_sources.yaml` queries (`logstash-*`). makelogs'
> own default (`--indexInterval` numeric) lumps everything into a single
> `logstash0` index, which won't match the wildcard.

> **About the "replace existing indices?" prompt.** makelogs prompts before
> overwriting indices on every run. The script handles this for you: when
> `--reset` is set, makelogs auto-confirms; otherwise the script answers
> "no" once so subsequent runs append to the existing index instead of
> wiping it. Just add `--reset` if you actually want to start over.

### Make sure the agent is reading from ES

In [`config/agent_sources.yaml`](../config/agent_sources.yaml):

```yaml
sources:
  - name: prod-app
    type: elasticsearch
    enable: true
    elasticsearch:
      addresses:
        - http://localhost:9200
      index: "logstash-*"
      time_field: "@timestamp"
      query: '*'
      message_field: "@message"
      page_size: 500
```

(Adjust `addresses`, `index`, and `username`/`password` for your environment.)

> **Tip on `lookback`.** The agent only queries documents newer than
> `now - agent.lookback` (default `5m`). makelogs spreads events over
> `--days N` ending at "now", so a one-shot `--days 1` push is mostly
> outside that window. Either bump `agent.lookback` in
> `config/config.yaml` (e.g. `1h` / `24h`) or use loop mode so fresh
> events keep landing inside the window.

---

## 3. Metrics source — fake Prometheus series

[`generate_fake_metrics.py`](generate_fake_metrics.py) is the metrics
analogue of `generate_noisy_logs.py`. Because Prometheus *scrapes* rather
than receiving pushes, the script pushes a realistic, increasing
time-series to a **Prometheus Pushgateway** that Prometheus then scrapes —
so the `query_metrics` analyze tool (and, on Enterprise, a standing
`prometheus` source) has real series to range-query. Used by the
[`metrics/`](../examples/docker-compose/metrics/) example.

It emits exactly the metric names that example's PromQL uses:

```
demo_http_requests_total{service,code}                 (counter)
demo_http_request_duration_seconds_bucket{service,le}  (histogram)
```

### Normal traffic

```bash
# steady, healthy traffic for 60s (~0.5% errors, low latency) to the
# default pushgateway at http://localhost:9091
python3 scripts/generate_fake_metrics.py

# point at a different pushgateway / service (the enterprise metrics-source
# example reuses this script with its own service name)
python3 scripts/generate_fake_metrics.py \
  --target http://localhost:9091 --service checkout

# see the exact series/labels emitted + sample PromQL
python3 scripts/generate_fake_metrics.py --list
```

Useful flags: `--target/-t` (env `PUSHGATEWAY_URL`), `--service/-s`,
`--job/-j`, `--duration/-d` (0 = until Ctrl+C), `--interval/-i`, `--rate/-r`,
`--seed`.

### Spike mode

Mirror the log generator's `--spike` UX — drive a 5xx + latency anomaly
(~45% 500s, p95 > 500ms) so the PromQL anomaly rules cross their thresholds:

```bash
# 90s of anomaly
python3 scripts/generate_fake_metrics.py --spike --duration 90

# hands-off demo: spike for 60s, then auto-revert to normal for the rest of
# the run (the analogue of /spike?seconds=60 then /calm)
python3 scripts/generate_fake_metrics.py --spike --spike-duration 60 --duration 180

# wipe the pushed series afterwards (cleanup / the analogue of /calm)
python3 scripts/generate_fake_metrics.py --clear
```

| Flag | Default | Description |
|---|---|---|
| `--spike` | off | Engage the 5xx + latency anomaly. |
| `--spike-duration S` | `0` | Stay anomalous for S seconds then auto-revert (0 = whole run). |
| `--clear` | — | DELETE the pushed group from the pushgateway and exit. |
| `--otlp URL` | — | Also POST best-effort OTLP spans to a Tempo backend (traces overlay). |

### Optional: traces (Tempo)

When the `metrics/` example's traces overlay is up, point the same script at
Tempo's OTLP/HTTP endpoint so it also emits best-effort spans (error spans
during a `--spike`):

```bash
python3 scripts/generate_fake_metrics.py --spike \
  --otlp http://localhost:4318 --duration 90
```

### How it fits the `metrics/` example (OSS)

On the OSS image the standing metric/trace **source** is Enterprise-only, so
the example **triggers** incidents through the `file` log source and uses
metrics only to **correlate**. Both fake-data steps are `scripts/`-based:

```bash
# 1. trigger the incident (status=503 error log lines)
python3 scripts/generate_noisy_logs.py --append --start-time now \
  --spike 5xx --spike-burst 80 \
  --output examples/docker-compose/metrics/logs/app.log

# 2. push the correlating metric anomaly
python3 scripts/generate_fake_metrics.py --spike --duration 90
```

---

## End-to-end flow

```bash
# terminal 1 — start the agent (loads config + tails sources)
./run.sh

# terminal 2 — pick one:
./scripts/run_noisy_logs.sh         # file source
./scripts/run_makelogs.sh           # elasticsearch source

# terminal 3 — peek at what the agent learned
curl -H "X-Gateway-Secret: $GATEWAY_SECRET" \
     http://localhost:3000/api/agent/patterns | jq

# bonus: source / cursor status
curl -H "X-Gateway-Secret: $GATEWAY_SECRET" \
     http://localhost:3000/api/agent/status | jq
```

Use `AGENT_MODE=training` first to build the catalog, then switch to
`shadow` to see which signals would have alerted, and finally `detect`
to emit incidents for new or unexpected issues patterns.
