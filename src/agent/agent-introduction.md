# AI Agent — Introduction

The AI Agent is an SRE agent that watches your systems and points
out anything that looks new or unusual. The plan is to cover the
three signals an SRE cares about — **logs, metrics, and traces** —
and over time take on more of the routine work an on-call engineer
does.

**Logs are the first thing it understands.** It reads your
application logs, learns what your "normal" log lines look like, and
flags lines it has never seen before. The idea is simple: most log
lines repeat themselves. If a brand-new line shows up, it usually
means something new is happening, and you probably want to know.
Metrics and traces will follow in later releases.

![AI Agent](/docs/images/ai-agent.png)

The agent is **off by default** (`agent.enable: false`). It will not
read anything, save any files, or start any background work until
you turn it on.

## Why this is useful

Most logs in production are boring and repeat over and over. A handful
of message shapes (templates) usually account for almost all of the
volume. The agent learns those shapes from your real traffic and then
points out anything that doesn't fit. You don't have to write rules
upfront — you just let it watch for a while.

## How a log line is processed

Each time the agent checks for new logs, every line goes through a
short series of steps. If a step throws the line out, the next steps
are skipped.

![AI Agent](/docs/images/ai-agent-pipeline.png)

In plain words:

1. **Read** a new log line from one of your sources (a file, an
   Elasticsearch index, etc.).
2. **Hide secrets.** Tokens, API keys, passwords, and similar things
   are replaced before anything else looks at the line.
3. **Filter.** Boring lines (`200 OK`, health checks, debug noise) are
   dropped so they don't fill up the catalog.
4. **Group.** Lines that look the same are grouped together. The
   agent doesn't keep every line — only one entry per "shape" of
   message.
5. **Save.** That group is saved, along with how many times it has
   been seen.
6. **Decide what to do** based on the agent's mode: just learn
   (training), pretend to alert (shadow), or actually send an
   incident when the line is brand new (detect).

## Parts of the agent

Each part has its own page with the full settings and examples.

### 1. [Log sources](./configuration.md#signal-sources)
Where the agent reads logs from. Two kinds are supported today: a
local file reader (for testing or simple setups) and an
Elasticsearch reader (for production clusters). Both remember where
they left off, so a restart never replays old lines or skips new
ones.

### 2. [Hiding secrets](./redaction.md)
Before anything else, the agent replaces sensitive values (JWTs,
AWS keys, bearer tokens, emails, UUIDs, user agents, etc.) with a
placeholder. This runs first so secrets never reach the rest of
the agent or any external AI model.

### 3. [Filter rules](./regex.md)
A short list of named rules plus an optional catch-all
(`default_pattern`) decide which lines are worth learning from. Any
line that doesn't match anything is thrown away before grouping. Use
`default_pattern: ".*"` if you want the agent to learn from every
line.

### 4. [Grouping (the miner)](./miner.md)
The grouper looks at the words in each line and puts similar lines
together. The result is a template like
`GET /api/users/<*> 200`, where `<*>` stands for the parts that
change (IDs, timestamps, IPs, etc.). You can tune how strict the
grouping is.

### 5. [Catalog](./catalog.md)
The agent's long-term memory. Every template it learns is saved
with: when it was first seen, when it was last seen, how many times
it has appeared, an average rate, the filter rule that matched it,
and any labels you add. The catalog is saved to
`data/patterns.json`.

### 6. The worker and modes
The worker is the loop that runs everything on a timer. There are
three modes:

- **`training`** — just watch and learn. No alerts.
- **`shadow`** — watch and learn, plus write a "would have alerted"
  log entry every time a line would have triggered an alert. Still
  no real alerts. Good for checking the agent's judgement before
  going live.
- **`detect`** — actually create incidents for lines the agent has
  never seen before. (AI-written summaries for those incidents are
  coming in a later release; today this mode only logs the
  decision.)

### 7. [Admin endpoints](./configuration.md#admin-endpoints)
A small set of HTTP endpoints under `/api/agent/*` that let you
look at the catalog, label patterns as known, and flush state
during reviews. Every endpoint requires the `X-Gateway-Secret`
header.

## Suggested rollout

1. Run in **training** for a few days. Check that the catalog
   stops growing quickly and the templates make sense.
2. Switch to **shadow**. Watch the `agent[shadow]: would alert ...`
   log lines for one release cycle.
3. Switch to **detect**. Triage what the agent reports and keep
   labeling patterns through the admin endpoints.

## Where to next

- [Getting Started](./getting-started.md) — a short walkthrough
  using the file reader and sample logs.
- [Configuration](./configuration.md) — every setting, every
  environment variable.
- Deep dives: [Hiding secrets](./redaction.md) ·
  [Filter rules](./regex.md) · [Grouping](./miner.md) ·
  [Catalog](./catalog.md).
