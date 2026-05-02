# AI Agent — Getting Started

This guide walks you from zero to a running agent in **training mode**,
backed by a local file source and a persisted catalog. By the end you will
have:

- The agent running in Docker on port `3000`.
- A `data/patterns.json` file growing as the agent learns templates.
- A way to inspect the catalog through the admin endpoints.
- (Optional) A script that streams realistic test logs so you can play
  before pointing the agent at real production data.

> **Heads up.** The agent is **off by default**. Nothing in this guide
> happens until you set `AGENT_ENABLE=true`.

---

## Prerequisites

- Docker (or Podman) on your machine.
- A Redis instance the agent can reach. The agent uses Redis to remember
  where it left off in each log source between restarts. For a quick local
  test you can run `docker run -d --name versus-redis -p 6379:6379 redis:7`.
- About 5 minutes.

---

## 1. Prepare a working directory

Create a folder that will hold the agent's config, sources file, and
persistent data. Anything in here is yours — Versus only ever reads from
`/app/config` and writes to `/app/data` inside the container.

```bash
mkdir -p versus-agent/{config,data,logs}
cd versus-agent
```

You should end up with this layout:

```
versus-agent/
├── config/
│   ├── config.yaml          # main config (we'll write this in step 2)
│   └── agent_sources.yaml   # list of log sources (step 3)
├── data/                    # the agent persists patterns.json here
└── logs/                    # the log file the agent will tail
```

---

## 2. Write a minimal `config.yaml`

Copy the snippet below into `config/config.yaml`. Everything not related
to the agent is left disabled so you can focus on training.

```yaml
name: versus
host: 0.0.0.0
port: 3000

alert:
  debug_body: true
  # All channels disabled — you don't need them to learn patterns.

queue:
  enable: false

oncall:
  enable: false

redis:
  host: ${REDIS_HOST}
  port: ${REDIS_PORT}
  password: ${REDIS_PASSWORD}
  db: 0

agent:
  enable: true                # turn the agent on
  mode: training              # observe only, no alerts
  poll_interval: 10s
  lookback: 5m
  data_dir: /app/data         # patterns.json will live here

  gateway_secret: ${AGENT_GATEWAY_SECRET}  # any string you choose

  redaction:
    enable: true
    redact_ips: false

  catalog:
    mode: file                # the only supported backend today
    persist_interval: 30s
    auto_promote_after: 100   # after this many sightings, treat as "known"

  miner:
    similarity_threshold: 0.4
    tree_depth: 4
    max_children: 100

  regex:
    # In training, this is your "what counts as interesting?" knob.
    # ".*" → learn from every line (recommended for the very first run).
    # Leave the rules empty for now; you can add them later.
    default_pattern: ".*"
    rules: []

  sources_path: /app/config/agent_sources.yaml
```

---

## 3. Write `agent_sources.yaml`

This file is loaded separately so you can swap source lists per
environment without touching the main config. For training, point it at a
single local log file.

```yaml
sources:
  - name: my-app
    type: file
    enable: true
    file:
      path: /app/logs/my-app.log
      format: text             # "text" or "json"
      from_beginning: true     # replay the whole file on first start
```

---

## 4. Run with Docker

Mount the four pieces from your host: the two config files, the data
directory (so `patterns.json` survives restarts), and the logs directory
(so the agent can tail your application's log file).

```bash
docker run -d \
  --name versus-agent \
  -p 3000:3000 \
  -v "$PWD/config:/app/config:ro" \
  -v "$PWD/data:/app/data" \
  -v "$PWD/logs:/app/logs:ro" \
  -e AGENT_ENABLE=true \
  -e AGENT_MODE=training \
  -e AGENT_GATEWAY_SECRET=change-me \
  -e REDIS_HOST=host.docker.internal \
  -e REDIS_PORT=6379 \
  -e REDIS_PASSWORD= \
  ghcr.io/versuscontrol/versus-incident:latest
```

> On Linux, replace `host.docker.internal` with your host IP, or run
> Redis in the same Docker network and use its container name.

Watch the logs:

```bash
docker logs -f versus-agent
```

You should see:

```
agent: starting worker mode=training sources=1 poll=10s catalog=/app/data/patterns.json
```

If you don't have any log lines yet, the agent simply waits and polls every
`poll_interval`. To give it something to chew on, jump to the next section.

---

## 5. (Optional) Generate fake logs to test

Before pointing the agent at real production data, it's worth confirming
end-to-end that the file source, redactor, miner, and catalog all work on
your machine. The repo ships two scripts for this in `scripts/`:

- `generate_noisy_logs.py` — writes a one-shot file of realistic INFO /
  WARN / ERROR lines.
- `run_noisy_logs.sh` — appends fresh batches at a fixed interval, so the
  agent (which is tailing the file) sees live traffic.

Clone (or copy) the scripts folder, then:

```bash
# one-shot: write 2000 lines into the file the agent is tailing
python3 scripts/generate_noisy_logs.py \
  --output ./logs/my-app.log \
  --lines 2000 --seed 42

# OR live-tail: append 20 lines every 5 seconds, forever (Ctrl+C to stop)
./scripts/run_noisy_logs.sh \
  --output ./logs/my-app.log \
  --interval 5 --batch 20
```

Within a few seconds you should see lines like this in the agent's log:

```
agent: new pattern p-abc123 (source=my-app tag=default) → service=api-gateway method=GET path=<*> status=200 …
agent: tick my-app signals=20 matched=20 patterns=8 skipped_no_match=0 verdicts=map[learned:8] cursor=…
```

Each "new pattern" line is a previously-unseen template the agent has
just added to the catalog. After the noisy script has been running for a
minute or two, the rate of new patterns drops sharply — that's the
agent reaching steady state.

For the full reference on the helper scripts (Elasticsearch source,
makelogs, Docker auto-start, etc.) see [`scripts/README.md`][scripts] in
the repo.

[scripts]: https://github.com/VersusControl/versus-incident/tree/main/scripts

---

## 6. Inspect what the agent has learned

The admin endpoints are gated by the `X-Gateway-Secret` header you set in
step 4 (`AGENT_GATEWAY_SECRET`).

```bash
# Catalog summary
curl -H "X-Gateway-Secret: change-me" \
  http://localhost:3000/api/agent/status | jq

# Every learned pattern, sorted by sighting count
curl -H "X-Gateway-Secret: change-me" \
  http://localhost:3000/api/agent/patterns | jq

# Drill into one pattern
curl -H "X-Gateway-Secret: change-me" \
  http://localhost:3000/api/agent/patterns/p-abc123 | jq
```

The `patterns.json` file in `./data/` is updated every
`catalog.persist_interval` (default 30s). It survives restarts and can be
copied between environments.

---

## 7. Switch to your real log file

Once you trust the catalog, replace the test file with the real one:

1. Stop the agent: `docker stop versus-agent`.
2. Edit `config/agent_sources.yaml` so `file.path` points at your
   application's log file (mount it read-only into `/app/logs`).
3. (Optional) Drop the old test catalog so the agent starts fresh:
   `rm -f data/patterns.json`.
4. Start the agent again.

After a few days of training mode, when the new-pattern rate flattens
out, switch `AGENT_MODE` to `shadow` and watch the would-have-alerted
events accumulate at `GET /api/agent/shadow` for a release cycle before
promoting to `detect`. See [Shadow Mode](./shadow-mode.md) for the full
review workflow.

---

## Common questions

**Q: How long should I leave the agent in training mode?**
Until the rate of new patterns flattens out. For a small service this is
usually a few days; for a large estate it can be a week or two. Watch
the `agent: new pattern …` lines in stdout — when they slow to a
trickle over a full release cycle, you're ready for shadow mode.

**Q: Does training mode send any alerts?**
No. Training is observation-only. The agent learns templates and writes
them to `patterns.json`, nothing else. No Slack, no email, no on-call.

**Q: Where does `patterns.json` live and what's in it?**
At `<data_dir>/patterns.json` (default `data/patterns.json`). Each entry
is one pattern: ID, mined template, first/last seen timestamps, total
sighting count, EWMA frequency baseline, the regex `rule_name` that
flagged it, and any operator-assigned `verdict` / `tags`.

**Q: What if my log file rotates?**
The file source detects rotation by comparing the file's current size
to the saved cursor offset. When the file shrinks (a fresh log after
rotation), it restarts from offset 0. No special configuration needed.

**Q: Do I need Redis?**
Yes, when the agent is enabled. Redis stores per-source cursors so the
agent picks up exactly where it left off across restarts. Without
Redis, every restart would either replay your `lookback` window or
miss entries written while the agent was down. For local testing the
file source also writes a sidecar cursor file as a fallback, but
Elasticsearch and other backends rely on Redis.

**Q: My catalog is full of patterns I don't care about — how do I clean
up?**
Three options, in order of effort:

1. Tighten `agent.regex.default_pattern` (or set it to empty and rely on
   named rules) so noisy lines never reach the miner.
2. Delete bad patterns one by one:
   `DELETE /api/agent/patterns/<id>`.
3. Wipe and start over: stop the agent, delete `data/patterns.json*`,
   start it again. You'll lose your training history.

**Q: Can I run multiple agents against the same Redis?**
Yes, as long as their source `name`s are distinct (cursor keys are
namespaced by source name). Each agent should have its own
`data_dir` so they don't fight over `patterns.json`.

**Q: What's the smallest config I can run with?**
Effectively: `agent.enable=true`, `agent.gateway_secret=…`,
`agent.sources_path=…`, plus a `redis` block. Everything else has
sensible defaults.

---

## What's next

- [Shadow Mode](./shadow-mode.md) — the next step after training: review
  what the agent _would_ have alerted on without sending anything.
- [Configuration](./configuration.md) — every knob, env override, and
  per-request parameter.
- [Redaction](./redaction.md), [Regex](./regex.md), [Miner](./miner.md),
  [Catalog](./catalog.md) — component deep-dives.
- [Introduction](./agent-introduction.md) — pipeline overview and
  rollout strategy.
