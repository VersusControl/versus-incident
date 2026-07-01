# AI Agent — Catalog

The **catalog** is the agent's long-term memory: the set of recurring log *patterns* it has learned, plus the *services* it has discovered, plus whatever verdict you've given each one. A "pattern" here is a reusable log template like `user_id = <*> login ok in <*>` — one shape that stands in for thousands of near-identical lines (see [Miner](./miner.md) for how lines become templates).

When the agent decides whether a new signal is "normal" or "worth a look", it's asking the catalog: *have I seen this shape before, and what did we say about it?* The catalog is what turns a firehose of raw logs into a short, curatable list you can actually reason about.

## What you'll learn

- What the catalog stores.
- How entries get there.
- How you curate it — label, tag, delete, rename services, and reset.
- How it relates to Service Detection and the Miner.

## What the catalog holds

Every catalog entry (one per learned pattern) carries these fields. You see them on the **Patterns** page in the admin UI, sorted most-frequent first:

| Field | What it means |
|---|---|
| **template** | The learned shape, with `<*>` marking the parts that change. |
| **count** | How many raw lines have matched this pattern. |
| **first seen** / **last seen** | When the pattern first and most recently appeared. |
| **baseline frequency** | A running average of how often it fires per tick — the [spike detector](./spike.md) compares against this. |
| **verdict** | `known` once the pattern is baseline (auto-promoted or set by you), otherwise empty. |
| **rule** | Which [regex](./regex.md) rule flagged it (`default` when only the catch-all matched). |
| **source** | The log source it was first seen in — handy for telling prod from staging. |
| **service** | The service name pulled from the line by [service detection](./service-detection.md). Empty when detection is off. |
| **tags** | Any free-form markers you add. |

Alongside patterns, the catalog also tracks each **service** it has discovered — its name and when it was first seen — so per-service features (like [new-service grace](./ai-detect-mode.md)) have something to work with.

The whole catalog lives in one file, `patterns.json`, in the agent's fixed `./data` directory (`/app/data` in the container). It's flushed to disk automatically every `persist_interval` (default `30s`) — there's no manual "save" step.

## How entries get there

The catalog fills itself by **learning**. On every tick, in training, shadow, and detect mode alike:

1. A signal is [redacted](./redaction.md), then checked against the [regex pre-filter](./regex.md).
2. If it passes, the [miner](./miner.md) turns it into a template with a stable `pattern_id`.
3. The catalog records an observation for that `pattern_id` — creating the entry on first sighting, or bumping its `count`, `last seen`, and baseline on every sighting after.

The **service** name is stamped on first sighting only, so a pattern's attribution stays stable even if a later line is ambiguous.

A pattern becomes **known** — meaning the agent stops treating it as new — in one of two ways:

- It's been seen at least `agent.catalog.auto_promote_after` times (**default `100`** in the shipped config). The auto-promotion is saved so you can see which patterns the agent considers baseline.
- You label it `known` by hand (below). This takes effect immediately and sticks until you change it.

## Curate the catalog

The **Patterns** and **Services** pages in the admin UI are where you keep the catalog clean. Everything below is available there — no command line needed.

| Action | Where | What it does |
|---|---|---|
| **Label a pattern `known`** | Patterns page → a pattern → set verdict | Silences it — it won't surface as new in shadow or detect again. |
| **Tag a pattern** | Patterns page → a pattern → tags | Adds free-form markers for your own grouping. |
| **Delete a pattern** | Patterns page → a pattern → delete | Removes one entry — e.g. a false-positive cluster you never want back. |
| **Rename a service** | Services page → a manual service → rename | Renames a service you created by hand (auto-discovered ones are read-only). |
| **Clear all** | Patterns page → **Clear all** | Wipes the whole catalog and starts over (below). |

> **Tip:** Labeling a noisy-but-benign pattern `known` is almost always better than deleting it. A deleted pattern is simply re-learned the next time it appears; a `known` pattern is remembered and stays quiet.

## Clear all — a full reset

**Clear all** is the destructive reset. Use it when your catalog has learned garbage (a bad source, a test flood) and you want a clean baseline. It:

1. Removes **every** learned pattern.
2. Removes **every** discovered service.
3. Resets the [miner](./miner.md) so recurring lines are re-discovered as *new* rather than resumed against the old templates.
4. Persists the now-empty catalog, so a restart doesn't bring the old entries back.

The agent relearns from scratch on the next tick. This is admin-gated like every other destructive action and reports how many patterns and services it cleared.

> **Warning:** Clear all is not undoable. It throws away every `known` verdict and tag you've curated. Reach for **Delete** on a single pattern first; keep Clear all for a genuine "start over".

## How it relates to the rest of the agent

- **[Miner](./miner.md)** produces the `pattern_id` and template that each catalog entry is keyed on. Clearing the catalog also resets the miner — the two are kept in step.
- **[Service Detection](./service-detection.md)** fills the `service` field. Without it, every entry's service is empty and per-service features fall back to one global bucket.
- **[Shadow Mode](./shadow-mode.md)** reads the catalog to decide whether a signal is `known` (quietly recorded) or `unknown` (surfaced for review).
- **[Detect Mode](./ai-detect-mode.md)** uses the same `known` check to decide what reaches the AI SRE.

## See also

- [Miner](./miner.md) — how raw lines become the templates stored here.
- [Service Detection](./service-detection.md) — where the `service` field comes from.
- [Shadow Mode](./shadow-mode.md) — labeling patterns from what the agent would have alerted on.
- [Configuration](./configuration.md) — the `agent.catalog` settings (`persist_interval`, `auto_promote_after`).

---

← Back to [AI SRE Agent overview](./agent-introduction.md)
