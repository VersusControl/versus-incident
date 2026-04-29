# Scripts

Helper scripts for generating test traffic against the Versus AI agent.

There are two flavours:

| Goal                                         | Script                                                  |
|----------------------------------------------|---------------------------------------------------------|
| Generate a local log **file** the agent tails | [`generate_noisy_logs.py`](generate_noisy_logs.py) + [`run_noisy_logs.sh`](run_noisy_logs.sh) |
| Push synthetic events into **Elasticsearch** | [`ensure_elasticsearch.sh`](ensure_elasticsearch.sh) + [`run_makelogs.sh`](run_makelogs.sh)   |

Pick whichever matches the source you've enabled in `config/agent_sources.yaml`.

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
```

Useful flags: `--lines/-n`, `--output/-o`, `--start-time` (`now` or RFC3339),
`--interval-min/--interval-max`, `--append/-a`, `--seed`.

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
```

Stop with Ctrl+C. The script prints a summary count on exit.

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
rm -f data/patterns.json data/patterns.json.* \
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

## End-to-end flow

```bash
# terminal 1 — start the agent (loads config + tails sources)
./run.sh

# terminal 2 — pick one:
./scripts/run_noisy_logs.sh         # file source
./scripts/run_makelogs.sh           # elasticsearch source

# terminal 3 — peek at what the agent learned
curl -H "X-Gateway-Secret: $AGENT_GATEWAY_SECRET" \
     http://localhost:3000/api/agent/patterns | jq

# bonus: source / cursor status
curl -H "X-Gateway-Secret: $AGENT_GATEWAY_SECRET" \
     http://localhost:3000/api/agent/status | jq
```

Use `AGENT_MODE=training` first to build the catalog, then switch to
`shadow` to see which signals would have alerted, and finally `detect`
to emit incidents for genuinely new patterns.
