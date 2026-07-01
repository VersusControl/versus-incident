# AI Agent — Miner

The **miner** is the part of the agent that takes noisy, never-quite-identical log lines and groups them into a small set of reusable **templates**. A template is a log line with its changing bits blanked out — `user_id = <*> login ok in <*>` stands in for `user_id=42 login ok in 12ms`, `user_id=99 login ok in 8ms`, and every other line of that shape.

Raw logs are almost all unique — every line has a different timestamp, ID, or duration. If the agent treated each one as its own thing, it would drown. The miner collapses millions of lines into dozens of stable patterns, and the ID of each pattern (`p-…`) becomes the unit of work for everything downstream: the [catalog](./catalog.md) stores it, the [shadow log](./shadow-mode.md) flags it, and [detect mode](./ai-detect-mode.md) triages it.

## What you'll learn

- How a raw line turns into a template.
- What the agent treats as a "variable".
- The three tuning knobs and when to touch them.
- Why "Clear all" resets the miner too.

## How grouping works

The miner is a small, dependency-free version of the classic **Drain** algorithm (a fixed-depth tree that buckets similar lines). For each incoming line it does this, in order:

1. **Tokenize.** Split the line on whitespace and common glue characters (`=`, `,`, `()`, `[]`, `{}`, `"`).
2. **Blank out variables.** Any token that *looks* like a value — a number, an ID, an IP — is replaced with the wildcard `<*>`. So `user_id=42` and `user_id=99` end up with the same shape.
3. **Find the right bucket.** Walk a tree keyed first by how many tokens the line has (lines of different length never collide), then by its leading fixed tokens.
4. **Compare to what's there.** In that bucket, score the line against each existing template token-by-token. If the best score clears `similarity_threshold`, it's a match.
5. **Merge or create.** On a match, merge — any position where the two disagree becomes `<*>`, so the template keeps generalizing. On no match, register a **new** template with a fresh `pattern_id`.

Every template gets a stable id of the form `p-<12-hex>` derived from its first token list, so the same shape reads the same across restarts.

### A worked example

Feed these three lines in:

```
user_id=42 login ok in 12ms
user_id=99 login ok in 8ms
user_id=7  login ok in 21ms
```

The miner produces **one** template:

```
user_id = <*> login ok in <*>
```

`42` / `99` / `7` are numbers and `12ms` / `8ms` / `21ms` mix letters and digits, so all become `<*>`. The fixed words (`user_id`, `login`, `ok`, `in`) hold the shape together.

## What counts as a variable

During tokenization the miner replaces these token shapes with `<*>`:

| Looks like | Example |
|---|---|
| A number (int or float, with sign) | `42`, `-3.14` |
| Hex with `0x` | `0x1f4a` |
| A UUID | `3f2504e0-4f89-41d3-9a0c-0305e82c3301` |
| An IPv4 (optionally `:port`) | `10.0.0.4`, `10.0.0.4:8080` |
| A long hex string (16+ chars — hashes, IDs) | `9f2ab3c4d5e6f7a8` |
| A token with **both** letters and digits | `12ms`, `req7f3` |
| A redacted secret | `<REDACTED:email>` |

That last row matters: because a [redacted](./redaction.md) token is treated as a variable, two lines that differ *only* by a scrubbed secret still land in the same template instead of fragmenting your [catalog](./catalog.md).

> **Note:** A token that's plain letters (`login`) or plain punctuation stays fixed. Only value-shaped tokens get blanked, which is what keeps templates meaningful instead of collapsing everything into `<*> <*> <*>`.

## Tuning

The defaults work for most setups. Tune only if you see related lines failing to merge, or unrelated lines collapsing together.

```yaml
agent:
  miner:
    similarity_threshold: 0.4   # how alike two lines must be to share a template
    tree_depth: 4               # how many leading tokens bucket a template
    max_children: 100           # per-bucket cap on distinct templates
```

| Key | Type | Default | What it does |
|---|---|---|---|
| `similarity_threshold` | float 0–1 | `0.4` | The fraction of positions two lines must share to merge. **Lower** = merge more aggressively (fewer, broader templates); **raise** = split more (more, tighter templates). |
| `tree_depth` | int | `4` | How many leading tokens are used to bucket templates. Deeper = finer buckets. |
| `max_children` | int | `100` | Cap on distinct templates per bucket. When full, the least-seen template is evicted to make room. |

| Symptom | Fix |
|---|---|
| Lines that are clearly the same show up as several patterns | **Lower** `similarity_threshold` (e.g. `0.3`). |
| Unrelated lines get merged into one pattern | **Raise** `similarity_threshold` (e.g. `0.5`). |

## Why "Clear all" resets the miner

The miner keeps its learned templates **in memory**, separate from the on-disk [catalog](./catalog.md). That split matters when you reset.

The catalog's **Clear all** action wipes every stored pattern *and* calls the miner's reset, which throws away its whole template tree and starts empty. Both are needed for a true fresh start: if only the catalog were cleared, the miner would still recognize recurring lines as already-seen (reporting them as *not new*), and the catalog would never truly relearn from zero. Clearing both means the next line the agent reads is genuinely treated as brand new.

Your tuning (`similarity_threshold`, `tree_depth`, `max_children`) is preserved across a reset — only the learned templates are dropped.

<!-- Visual Designer prompt: a "before/after" grouping graphic — on the left, five slightly-different raw log lines (varying IDs/durations highlighted); an arrow labelled "Miner" pointing right to a single template card reading "user_id = <*> login ok in <*>" with a small "p-9c2f01…" id chip. Keep it clean and monospace, matching the docsify Warp-style theme. -->

## See also

- [Catalog](./catalog.md) — where the miner's templates are stored and curated.
- [Regex](./regex.md) — the pre-filter that decides which lines reach the miner.
- [Redaction](./redaction.md) — why a scrubbed secret becomes a `<*>` wildcard here.
- [Configuration](./configuration.md) — the full `agent.miner` reference.

---

← Back to [AI SRE Agent overview](./agent-introduction.md)
