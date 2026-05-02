# AI Agent — Introduction

The AI Agent is an opt-in subsystem that turns raw application logs into a
small, curated catalog of recurring patterns. Once a service has been
running long enough for the catalog to stabilize, anything that doesn't fit
a known pattern is — by definition — something new, and worth a closer look.

![AI Agent](/docs/images/ai-agent.png)

It is **off by default** (`agent.enable: false`). Nothing extra runs, no
goroutines start, and no files are created until you explicitly opt in.

## Why pattern learning?

Most production logs are repetitive: a few hundred templates account for
99% of the volume. The agent solves the problem by building its own picture of "normal" from your traffic, then flagging departures from that baseline.

## The pipeline

Every time the agent checks for new logs, each line travels through a
short assembly line. Each station does one job; if a station rejects the
line, the rest is skipped.

![AI Agent](/docs/images/ai-agent-pipeline.png)

In plain English:

1. **Read** a fresh log line from one of your sources.
2. **Hide** sensitive parts so secrets never leave your machine.
3. **Decide** if the line is interesting. Boring lines (200 OK, health
   checks, debug noise) are dropped here so they don't clutter the
   catalog.
4. **Group** the line with others that look the same. The agent doesn't
   store every line it has ever seen — it stores one entry per "shape"
   of message.
5. **Remember** that group, including how often it shows up.
6. **React** based on the agent's mode: just learn (training), pretend
   to alert (shadow), or send a real incident if the line is something
   the agent has never seen before (detect).

## Components

Each component has its own page with the full configuration reference,
trade-offs, and examples.

### 1. [Data Sources](./configuration.md#data-sources)
What the agent reads from. Two source types ship today: a file tailer for
local logs and an Elasticsearch reader for production clusters. Sources
are cursor-aware, so the agent always picks up where it left off after a
restart.

### 2. [Redaction](./redaction.md)
Pattern-based scrubbing of secrets and PII (JWTs, AWS keys, bearer tokens,
emails, UUIDs, user agents, …). Runs first so no other component — and no
external AI model — ever sees the raw values.

### 3. [Regex pre-filter](./regex.md)
A small set of named rules plus an optional `default_pattern` that decide
which signals are worth learning from. Lines that match nothing are
dropped before the miner sees them, keeping the catalog focused. Set
`default_pattern: ".*"` to learn from every line.

### 4. [Miner](./miner.md)
A Drain-style log clusterer that turns a stream of similar lines into a
single template with `<*>` placeholders for variable parts (timestamps,
IDs, IPs, etc.). Configurable similarity threshold and tree depth.

### 5. [Catalog](./catalog.md)
Long-term memory. Every template the miner produces is stored with a
first-seen timestamp, sighting count, EWMA frequency, the regex
`rule_name` that flagged it, and an operator-curated verdict / tags.
Persisted as `data/patterns.json` (atomic
writes, rotated backups).

### 6. Worker & modes
The worker glues the components together and runs them on a polling
ticker. Three modes:

- **`training`** — observe only. Learn templates and persist them. No
  alerts of any kind.
- **`shadow`** — same as training, but log a verdict every time a signal
  *would* have alerted in detect mode. Useful for reviewing the agent's
  judgement before going live.
- **`detect`** — emit incidents for genuinely novel patterns. (AI
  summarization is a follow-up milestone; today this mode logs the
  verdict only.)

### 7. [Admin endpoints](./configuration.md#admin-endpoints)
A small REST surface (`/api/agent/*`, gated by `X-Gateway-Secret`) for
inspecting the catalog, labeling patterns, and flushing state during
training reviews.

## Recommended rollout

1. Start in **training** mode for a few days. Confirm the catalog
   stabilizes and the templates make sense.
2. Switch to **shadow** mode. Watch the `agent[shadow]: would alert ...`
   log lines for a release cycle.
3. Promote to **detect**. Triage incidents the agent emits and keep
   curating the catalog through the admin endpoints.

## Where to next

- [Getting Started](./getting-started.md) — a five-minute walkthrough
  using the included file source and sample data.
- [Configuration](./configuration.md) — every config knob, every env
  override, every per-request query parameter.
- Component deep-dives: [Redaction](./redaction.md) ·
  [Regex](./regex.md) · [Miner](./miner.md) · [Catalog](./catalog.md).
