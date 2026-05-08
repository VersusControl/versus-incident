# AI Agent — Getting Started

This guide takes you from nothing to a running agent in **training
mode**, reading from a local file and saving what it learns to disk.

![Training Mode](/docs/images/training-mode.png)

By the end you'll have:

- The agent running in Docker on port `3000`.
- A `data/patterns.json` file that grows as the agent learns.
- A way to look at what it learned through the admin endpoints.
- (Optional) A script that writes realistic test logs so you can
  play with it before pointing the agent at real data.

> **Reminder.** The agent is **off by default**. Nothing in this
> guide happens until you set `AGENT_ENABLE=true`.

---

## Before you start

- Docker (or Podman) on your machine.
- A Redis instance the agent can reach. The agent uses Redis to
  remember where it left off in each log source between restarts.
  For a quick local test:
  `docker run -d --name versus-redis -p 6379:6379 redis:7`.
- About 5 minutes.

---

## 1. Make a working folder

Create a folder to hold the agent's settings, source list, and saved
data. Anything in here is yours — the agent only reads from
`/app/config` and writes to `/app/data` inside the container.

```bash
mkdir -p versus-agent/{config,data,logs}
cd versus-agent
```

You should end up with:

```
versus-agent/
├── config/
│   ├── config.yaml          # main settings (step 2)
│   └── agent_sources.yaml   # list of log sources (step 3)
├── data/                    # patterns.json is saved here
└── logs/                    # the file the agent will read
```

---

## 2. Write a small `config.yaml`

Copy this into `config/config.yaml`. Everything not related to the
agent is turned off so you can focus on training.

```yaml
name: versus
host: 0.0.0.0
port: 3000

alert:
  debug_body: true
  # All channels off — you don't need them for training.

queue:
  enable: false

oncall:
  enable: false

redis:
  host: ${REDIS_HOST}
  port: ${REDIS_PORT}
  password: ${REDIS_PASSWORD}
  db: 0

# Shared secret for ALL admin endpoints (`/api/admin/*` and `/api/agent/*`).
gateway_secret: ${GATEWAY_SECRET}      # any string you choose

# Storage backend for the agent catalog, shadow log, and incident history.
storage:
  type: file
  file:
    data_dir: /app/data                # patterns.json + shadow.json + incidents.json live here
    max_incidents: 1000

agent:
  enable: true                # turn the agent on
  mode: training              # just watch and learn — no alerts
  poll_interval: 10s
  lookback: 5m

  redaction:
    enable: true
    redact_ips: false

  catalog:
    persist_interval: 30s
    auto_promote_after: 100   # after this many sightings, treat as known

  miner:
    similarity_threshold: 0.4
    tree_depth: 4
    max_children: 100

  regex:
    # In training, this controls "what counts as interesting?".
    # ".*" → learn from every line (good for the first run).
    # Leave the rules empty for now; you can add them later.
    default_pattern: ".*"
    rules: []

  sources_path: /app/config/agent_sources.yaml
```

---

## 3. Write `agent_sources.yaml`

The list of log sources is in a separate file so you can swap it
between environments without touching the main settings. For
training, point it at a single local log file.

```yaml
sources:
  - name: my-app
    type: file
    enable: true
    file:
      path: /app/logs/my-app.log
      format: text             # "text" or "json"
      from_beginning: true     # read the whole file on first start
```

---

## 4. Run with Docker

Mount four things from your machine: the two config files, the data
folder (so `patterns.json` survives restarts), and the logs folder
(so the agent can read your application's log file).

```bash
docker run -d \
  --name versus-agent \
  -p 3000:3000 \
  -v "$PWD/config:/app/config:ro" \
  -v "$PWD/data:/app/data" \
  -v "$PWD/logs:/app/logs:ro" \
  -e AGENT_ENABLE=true \
  -e AGENT_MODE=training \
  -e GATEWAY_SECRET=change-me \
  -e REDIS_HOST=host.docker.internal \
  -e REDIS_PORT=6379 \
  -e REDIS_PASSWORD= \
  ghcr.io/versuscontrol/versus-incident:latest
```

> On Linux, replace `host.docker.internal` with your host IP, or
> run Redis in the same Docker network and use its container name.

Watch the logs:

```bash
docker logs -f versus-agent
```

You should see:

```
agent: starting worker mode=training sources=1 poll=10s catalog=/app/data/patterns.json
```

If there are no log lines yet, the agent just waits and checks
again every `poll_interval`. To give it something to read, jump to
the next section.

---

## 5. (Optional) Generate fake logs to test

Before pointing the agent at real production data, it's worth
checking the whole thing works end to end on your machine. The repo
has two scripts in `scripts/`:

- `generate_noisy_logs.py` — writes one batch of realistic INFO /
  WARN / ERROR lines.
- `run_noisy_logs.sh` — keeps appending fresh batches at a fixed
  interval, so the agent (which is reading the file) sees live
  traffic.

Copy the scripts folder, then:

```bash
# one-shot: write 2000 lines into the file the agent is reading
python3 scripts/generate_noisy_logs.py \
  --output ./logs/my-app.log \
  --lines 2000 --seed 42

# OR live: append 20 lines every 5 seconds, forever (Ctrl+C to stop)
./scripts/run_noisy_logs.sh \
  --output ./logs/my-app.log \
  --interval 5 --batch 20
```

Within a few seconds you should see lines like:

```
agent: new pattern p-abc123 (source=my-app tag=default) → service=api-gateway method=GET path=<*> status=200 …
agent: tick my-app signals=20 matched=20 patterns=8 skipped_no_match=0 verdicts=map[learned:8] cursor=…
```

Each "new pattern" line is a brand-new template the agent just
added to the catalog. After a minute or two the rate of new
patterns drops sharply — that's the agent reaching steady state.

For the full reference on the helper scripts (Elasticsearch source,
makelogs, Docker auto-start, etc.) see [`scripts/README.md`][scripts]
in the repo.

[scripts]: https://github.com/VersusControl/versus-incident/tree/main/scripts

---

## 6. Look at what the agent learned

The admin endpoints need the `X-Gateway-Secret` header you set in
step 4 (`GATEWAY_SECRET`).

```bash
# Catalog summary
curl -H "X-Gateway-Secret: change-me" \
  http://localhost:3000/api/agent/status | jq

# Every learned pattern, sorted by how often it has been seen
curl -H "X-Gateway-Secret: change-me" \
  http://localhost:3000/api/agent/patterns | jq

# Look at one pattern in detail
curl -H "X-Gateway-Secret: change-me" \
  http://localhost:3000/api/agent/patterns/p-abc123 | jq
```

The `patterns.json` file in `./data/` is updated every
`catalog.persist_interval` (default 30s). It survives restarts and
can be copied between environments.

---

## 7. Switch to your real log file

Once you trust the catalog, replace the test file with the real
one:

1. Stop the agent: `docker stop versus-agent`.
2. Edit `config/agent_sources.yaml` so `file.path` points at your
   application's log file (mount it read-only into `/app/logs`).
3. (Optional) Delete the old test catalog so the agent starts
   fresh: `rm -f data/patterns.json`.
4. Start the agent again.

After a few days in training, when the new-pattern rate has
flattened, switch `AGENT_MODE` to `shadow` and watch the
"would-have-alerted" entries collect at `GET /api/agent/shadow`
for one release cycle before going to `detect`. See
[Shadow Mode](./shadow-mode.md) for the review steps.

---

## Common questions

**Q: How long should I leave the agent in training?**
Until new patterns stop showing up often. For a small service it's
usually a few days; for a large setup it can be a week or two.
Watch the `agent: new pattern …` lines in stdout — when they slow
to a trickle over a full release cycle, you're ready for shadow
mode.

**Q: Does training mode send any alerts?**
No. Training only watches. The agent learns templates and saves
them to `patterns.json`. No Slack, no email, no on-call.

**Q: Where does `patterns.json` live and what's in it?**
At `<storage.file.data_dir>/patterns.json` (default `data/patterns.json`). Each
entry is one pattern: ID, the template the agent learned, when it
was first and last seen, how many times it has been seen, an
average rate, the filter rule that matched it, and any labels you
add.

**Q: What if my log file rotates?**
The file reader notices rotation by comparing the file's current
size to the saved position. When the file shrinks (a fresh log
after rotation), it starts again from the beginning. No special
setup needed.

**Q: Do I need Redis?**
Yes, when the agent is on. Redis stores per-source bookmarks so
the agent picks up exactly where it left off across restarts.
Without Redis, every restart would either replay your `lookback`
window or miss entries written while the agent was down. The file
reader also writes a small bookmark file as a fallback for local
testing, but Elasticsearch and other sources rely on Redis.

**Q: My catalog is full of patterns I don't care about — how do I
clean up?**
Three ways, easiest first:

1. Make `agent.regex.default_pattern` stricter (or set it to empty
   and only use named rules) so noisy lines never reach the
   grouper.
2. Delete bad patterns one by one:
   `DELETE /api/agent/patterns/<id>`.
3. Wipe and start over: stop the agent, delete `data/patterns.json*`,
   start it again. You'll lose your training history.

**Q: Can I run multiple agents against the same Redis?**
Yes, as long as the source `name`s are different (bookmark keys
include the source name). Each agent should have its own
`storage.file.data_dir` so they don't fight over `patterns.json`.

**Q: What's the smallest config I can run with?**
Roughly: root-level `gateway_secret=…`, `agent.enable=true`,
`agent.sources_path=…`, plus a `redis` block. Everything else has
sensible defaults.

---

## What's next

- [Shadow Mode](./shadow-mode.md) — the next step after training:
  review what the agent _would_ have alerted on without actually
  sending anything.
- [Configuration](./configuration.md) — every setting and
  environment variable.
- [Hiding secrets](./redaction.md), [Filter rules](./regex.md),
  [Grouping](./miner.md), [Catalog](./catalog.md) — deep dives.
- [Introduction](./agent-introduction.md) — overview and rollout
  plan.
