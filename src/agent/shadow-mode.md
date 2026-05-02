# AI Agent — Shadow Mode

Shadow mode is the **dress rehearsal** between training and detect. The
agent keeps learning, but it also classifies every batch of signals
as if it were going to alert — and records the verdict to a file you
can review at your own pace. **No notifications are sent.**

Think of it as: "show me what you _would_ have woken me up for, so I can
decide if I trust you."

---

## When to switch to shadow

Stay in `training` until the rate of new patterns flattens out — typically
a few days for a small service, longer for a large estate. Then switch
the mode:

```bash
docker stop versus-agent
docker run -d \
  --name versus-agent \
  ... \
  -e AGENT_MODE=shadow \
  ghcr.io/versuscontrol/versus-incident:latest
```

Or edit `agent.mode` in `config.yaml` and restart.

The catalog and shadow log live alongside each other under `data_dir`:

```
data/
├── patterns.json     # learned templates (built in training, kept growing in shadow)
└── shadow.json       # would-have-alerted events (only written in shadow mode)
```

---

## How shadow mode works

Shadow mode reuses the entire training pipeline (read → redact → regex
filter → cluster → catalog) and adds **one extra step at the end**:
classify the result and, if the verdict isn't `known`, write a row to
`shadow.json`. No alert is ever sent.

![AI Agent](/docs/images/shadow-mode.png)

Key things to take away from the diagram:

- **The catalog still grows.** Every signal that survives redaction
  and the regex filter is upserted, just like in training. Switching
  to shadow doesn't pause learning.
- **The verdict is the only new step.** A pattern is `known` when
  either (a) you've labeled it `verdict: known` via the admin API,
  or (b) its `count` has crossed `auto_promote_after`. Everything
  else is `unknown` and ends up in the shadow log.
- **Two sinks, never an alert.** A would-have-alerted event lands in
  `shadow.json` (durable, reviewed via API) and prints a green stdout
  line (ephemeral, easy to spot when you're tailing the container).
  Slack, email, on-call — none of them are touched.

---

## What gets recorded

Every poll tick the worker walks each source and, for every signal
that survived the redactor and the regex pre-filter, decides one of
three outcomes:

1. **The cluster is `known`** — silently update the catalog and move
   on. This is what most signals do once the agent has trained.
2. **The cluster is `unknown` (verdict = `unknown`)** — write a row
   to the shadow log. This is the case for a brand-new template, or
   for a rare template whose `count` hasn't crossed
   `auto_promote_after` yet.
3. **The cluster is a frequency `spike` (verdict = `spike`)** —
   reserved for a future milestone. The verdict exists in the
   schema today but is never produced; you'll see `verdict_spike: 0`
   in the stats endpoint.

A signal is `known` when **either** of these is true:

- An operator has explicitly set the pattern's verdict to `known` via
  `POST /api/agent/patterns/<id>` with body `{"verdict":"known"}`
  (this takes effect immediately and is permanent until you change
  it).
- The pattern's `Count` is `≥ agent.catalog.auto_promote_after`
  (default `100`). The threshold is per-pattern, not per-source. The
  agent persists this auto-promotion to `patterns.json` so you can
  audit which patterns it considers baseline.

### Coalescing: one row per pattern, not per signal

A naive log of every flagged signal would explode on a busy cluster.
Instead the shadow log stores **one row per `(source, pattern_id)`
pair**. Every time the same pattern hits again, the existing row is
updated:

- `Count` += signals seen this tick (the raw firehose count).
- `Occurrences` += 1 (one tick = one occurrence, regardless of how
  many signals fired in that tick).
- `Template` is refreshed in case the miner refined it.
- `LastSeen` is set to now (UTC).
- `RuleName` is upgraded if the regex matcher now has a more
  specific tag than before.

So if 200 NTP-skew lines arrive across 4 ticks, you don't get 200
rows — you get **one** row with `count: 200, occurrences: 4`. That
one row is what shows up in the API.

### Every recorded field

| Field | Meaning | Example |
|---|---|---|
| `pattern_id` | Stable ID assigned by the miner. Same as in the catalog, so you can cross-reference. | `p-9c2f01` |
| `template` | Latest mined template. `<*>` marks variable parts. | `kernel: Out of memory: Killed process <*> (<*>) score 999 …` |
| `source` | The source name from `agent_sources.yaml`. Lets you tell prod-app from staging-app at a glance. | `my-app` |
| `rule_name` | Regex tag that fired. `default` means `default_pattern` matched but no named rule did. Empty when the regex pre-filter is disabled entirely. | `oom-killer` / `default` |
| `verdict` | `unknown` today; `spike` reserved for future use. | `unknown` |
| `sample_message` | One representative line, **post-redaction**, truncated to 512 bytes. The agent picks the first signal in the bucket; secrets are already gone. | `kernel: Out of memory: Killed process 1842 (versus-worker) score 999 …` |
| `count` | Total raw signals across every coalesced tick. The "firehose volume" for this pattern. | `17` |
| `occurrences` | How many distinct ticks flagged this pair. Always `≤ count`. | `3` |
| `first_seen` | UTC timestamp of the very first time this pair was added. | `2026-04-30T18:21:04Z` |
| `last_seen` | UTC timestamp of the most recent hit. Used for sorting and for eviction. | `2026-04-30T18:31:42Z` |

### Bounded size and eviction

The store is capped at **1000 distinct `(source, pattern_id)` pairs**
(currently hard-coded). When you cross that cap, the row with the
oldest `last_seen` is evicted to make room for the newcomer. In
practice you should never hit this on a normally-sized service: a
catalog of 1000 distinct anomalies in a single review window means
something is *very* wrong (or the regex pre-filter is too permissive
— see [Regex](./regex.md)).

`shadow.json` itself is written atomically (`tmp` + `rename`), so
you can `cat` it from disk while the agent is running without
risking a partial read.

### Stdout mirror

You'll also see a green line in the agent's logs every time a row is
recorded. It's the same data, formatted for humans:

```
agent[shadow]: would alert pattern=p-abc123 tag=default verdict=unknown freq=4
```

`freq` here is the **per-tick** count — the same number that gets
added to `Count` in the JSON. Use stdout for live debugging while
you're iterating on the regex rules; use the API for review.

---

## Try it locally with the noisy-logs script

If you went through [Getting Started](./getting-started.md), you already
have the agent running against `./logs/my-app.log` with `from_beginning:
true`. The repo ships a [`scripts/generate_noisy_logs.py`][gen] generator
that mixes ~30 common templates with a handful of low-weight,
production-style anomalies (kernel OOM with score, segfaults, expired
TLS certs, NTP clock skew, lost Raft quorum, replication lag,
unexpected `SIGTERM`, …). Those rare lines are exactly what shadow mode
is meant to surface.

[gen]: https://github.com/VersusControl/versus-incident/blob/main/scripts/generate_noisy_logs.py

**Step 1 — train the agent on the boring baseline.** While the agent is
running in `training` mode, point the live-tail script at the log file
and let it run for a few minutes:

```bash
./scripts/run_noisy_logs.sh \
  --output ./logs/my-app.log \
  --interval 1 --batch 50
```

Watch the agent logs: the rate of `agent: new pattern` lines should
drop sharply within a minute or two. That's your baseline.

**Step 2 — flip the agent to shadow mode.** Stop the container, start it
again with `AGENT_MODE=shadow`. The catalog you just built is preserved
on disk (`data/patterns.json`), so the agent already knows what "normal"
looks like.

**Step 3 — keep generating logs.** Restart the live-tail script (or just
leave it running). Most of what comes through is now the noisy baseline,
which the agent classifies as `known` and ignores. The rare anomaly
lines, however, hit the catalog with `Count` well below
`auto_promote_after` and end up in the shadow log:

```
agent[shadow]: would alert pattern=p-9c2f01 tag=default verdict=unknown freq=1
agent[shadow]: would alert pattern=p-7e1a44 tag=default verdict=unknown freq=2
```

**Step 4 — review.** After a minute or two, hit the admin endpoint:

```bash
curl -H "X-Gateway-Secret: $SECRET" \
  http://localhost:3000/api/agent/shadow | jq '.events[] | {template, count, occurrences}'
```

You'll see entries like:

```json
{ "template": "service=notifier message=\"x509 certificate expired\" host=<*> expired_at=<*> chain_position=<*>",  "count": 1, "occurrences": 1 }
```

This is the dry-run version of the on-call page you _would_ have
received in detect mode. Use the loop in [A typical review
loop](#a-typical-review-loop) below to triage them.

> **Tip.** Want a forced demo? Generate a single batch of mostly
> baseline + anomalies into the file the agent is tailing while in
> shadow mode:
>
> ```bash
> python3 scripts/generate_noisy_logs.py \
>   --append --start-time now \
>   --output ./logs/my-app.log --lines 500
> ```
>
> 500 lines at default weights yields ~25 anomaly lines spread across
> ~10 unique templates — a tidy worked example for review.

---

## Reviewing the shadow log

The `/api/agent/shadow*` endpoints are how you actually consume the
log. They're admin-only — every request needs the `X-Gateway-Secret`
header (set via the `AGENT_GATEWAY_SECRET` env var). The examples
below assume you've exported that value into a shell variable for
brevity:

```bash
export SECRET=change-me   # whatever you set as AGENT_GATEWAY_SECRET
```

All responses are JSON; piping to `jq` (or `python -m json.tool`)
makes them easier to read.

### List every event (most recent first)

Start here. The endpoint returns every distinct `(source, pattern_id)`
the agent flagged in the current window, sorted by `last_seen`
descending so the freshest noise floats to the top:

```bash
curl -H "X-Gateway-Secret: $SECRET" \
  http://localhost:3000/api/agent/shadow | jq
```

Sample output:

```json
{
  "events": [
    {
      "pattern_id": "p-abc123",
      "template": "GET /api/users/<*> 500",
      "source": "my-app",
      "rule_name": "default",
      "verdict": "unknown",
      "sample_message": "GET /api/users/42 500",
      "count": 17,
      "occurrences": 3,
      "first_seen": "2026-04-30T18:21:04Z",
      "last_seen": "2026-04-30T18:31:42Z"
    }
  ]
}
```

The two numbers to watch are:

- **`count`** — how many raw signals matched the pattern in total.
  High `count` + low `occurrences` means a brief flurry; high in both
  means a steady drip you should care about.
- **`occurrences`** — how many distinct ticks the pattern fired in.
  Each tick is one poll cycle (`agent.poll_interval`).

If you only want to see something specific, pipe through `jq`:

```bash
# templates and counts only
curl -s -H "X-Gateway-Secret: $SECRET" http://localhost:3000/api/agent/shadow \
  | jq '.events[] | {template, count, rule_name}'

# everything tagged "oom"
curl -s -H "X-Gateway-Secret: $SECRET" http://localhost:3000/api/agent/shadow \
  | jq '.events[] | select(.rule_name == "oom")'
```

### Aggregate stats

Useful for dashboards or a quick "is this getting better?" check
between review windows:

```bash
curl -H "X-Gateway-Secret: $SECRET" \
  http://localhost:3000/api/agent/shadow/stats | jq
```

```json
{
  "events": 12,
  "total_signals": 248,
  "total_occurrences": 41,
  "verdict_unknown": 12,
  "verdict_spike": 0
}
```

What each number means:

- **`events`** — distinct `(source, pattern_id)` pairs in the log.
- **`total_signals`** — sum of `count` across every event. This is
  the raw firehose volume.
- **`total_occurrences`** — sum of `occurrences`. Roughly: "how many
  ticks would have paged me?".
- **`verdict_unknown`** / **`verdict_spike`** — breakdown by verdict.
  Spike is reserved for a future milestone; today this is always 0.

A healthy review cycle drives `events` and `total_occurrences` down
over time, even as `total_signals` stays flat (because you're
labelling the boring patterns as known).

### Force-flush to disk

The worker debounces writes to `shadow.json` at
`catalog.persist_interval` (default 30s) so we don't thrash the disk.
If you need a snapshot **right now** — to copy the file out of a
container, attach to a bug report, or just check what would land on
disk — ask the agent to flush:

```bash
curl -X POST -H "X-Gateway-Secret: $SECRET" \
  http://localhost:3000/api/agent/shadow/flush
```

This is a no-op when the log is already clean (`shadow_dirty: false`).

### Clear the log

Once you've reviewed a batch of events and either curated the catalog
(label-as-known) or fixed the underlying bug, drop the log so the
next review window starts from zero:

```bash
curl -X DELETE -H "X-Gateway-Secret: $SECRET" \
  http://localhost:3000/api/agent/shadow
```

This also persists the empty file so a restart doesn't resurrect the
old entries. **The catalog is left alone** — every learned pattern
stays exactly where it was. You're only wiping the "would have
alerted" inbox.

### Status endpoint

A cheap health check that tells you both stores at a glance:

```bash
curl -H "X-Gateway-Secret: $SECRET" \
  http://localhost:3000/api/agent/status | jq
```

```json
{
  "patterns": 87,
  "dirty": false,
  "shadow_events": 12,
  "shadow_dirty": true
}
```

- **`patterns`** — catalog size.
- **`dirty`** — catalog has un-flushed changes since the last persist.
- **`shadow_events`** — distinct events currently in the shadow log.
- **`shadow_dirty`** — same idea for the shadow log.

If you ever see `shadow_events` plateau and `shadow_dirty` stay
`false` for many ticks, the agent has nothing new to flag — a good
sign you're approaching readiness for `detect` mode.

---

## A typical review loop

This is the workflow you'll repeat for as long as the agent stays in
shadow mode. Each pass should make the next one quieter.

1. **Run shadow mode for ~24 hours.** Long enough to capture at least
   one full traffic cycle (peak hours, off-peak, any nightly cron
   jobs).
2. **Pull the events.** `GET /api/agent/shadow` and skim them. Sort
   in your head into three buckets: real anomalies you'd page on,
   noise that should have been silenced, and "new but legitimate"
   patterns (fresh deploys, new endpoints, etc.).
3. **For events you _would_ want to be paged about:**
   - Add or refine a rule under `agent.regex.rules` in `config.yaml`
     so the pattern gets the right `name` next time.
   - Example: a `quorum lost` line that landed with `rule_name:
     default` deserves its own rule:

     ```yaml
     agent:
       regex:
         rules:
           - name: quorum-lost
             pattern: "(?i)quorum lost"
     ```
4. **For events that are just noise:**
   - Either bump `agent.catalog.auto_promote_after` (default 100) so
     the pattern auto-graduates to known sooner, **or** mark it
     `known` manually:

     ```bash
     curl -X POST -H "X-Gateway-Secret: $SECRET" \
       -H "Content-Type: application/json" \
       -d '{"verdict":"known","tags":["benign-validation-error"]}' \
       http://localhost:3000/api/agent/patterns/p-abc123
     ```

     Once `verdict == "known"`, that pattern will never appear in the
     shadow log again, regardless of frequency.
5. **Clear the log:** `DELETE /api/agent/shadow`. The next review
   window starts clean.
6. **Repeat** until the shadow log is mostly empty over a full release
   cycle (one or two weeks).
7. **Promote to detect.** Set `AGENT_MODE=detect` and you're live.

---

## Common questions

**Q: Will shadow mode send any alerts?**
No. Not Slack, Telegram, email, on-call — nothing. It only writes to
`shadow.json` and stdout.

**Q: Does shadow mode keep adding patterns to the catalog?**
Yes. Every signal that survives the redactor and regex pre-filter is
clustered and upserted, exactly like in training. This is intentional:
shadow mode is "training plus a verdict" so you don't lose ground while
reviewing.

**Q: What happens if I switch back to training?**
The shadow log is preserved on disk and kept in memory; the worker just
stops appending to it. Switch back to `shadow` later and it picks up
where it left off.

**Q: Can the shadow log fill up forever?**
No. It's capped at 1000 distinct `(source, pattern_id)` pairs. When full,
the oldest-by-`last_seen` is evicted to make room. The cap is currently
hard-coded.

**Q: Is `shadow.json` safe to delete by hand?**
Yes — but only while the agent is stopped, and it'll come back on the
next tick anyway.

---

## What's next

- [Configuration](./configuration.md) — every knob, env override, and
  per-request parameter.
- [Catalog](./catalog.md) — labeling and curating patterns from the
  shadow log.
- [Regex](./regex.md) — fine-tuning what reaches the miner so shadow
  noise stays manageable.
