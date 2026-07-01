# AI Agent — Regex

A **regex** (regular expression) is a small pattern language for matching text. The agent uses regexes in three different places, and this page is the one-stop guide to writing them: what flavor to use, how capture groups and ordering work, and how to test a pattern before you ship it.

The three surfaces share the same engine but do different jobs:

| Surface | Config key | What it does |
|---|---|---|
| **Pre-filter** | `agent.regex` | Decides which log lines are worth learning, and tags each with a rule name. |
| **Service detection** | `agent.service_patterns` | Pulls the service name out of a line via a capture group. |
| **Redaction** | `agent.redaction.extra_patterns` | Scrubs extra secret shapes before anything else sees them. |

> **Note:** The agent uses Go's **RE2** regex engine (the standard-library `regexp` package). RE2 is fast and safe but has no backreferences or lookaround. When testing at [regex101](https://regex101.com), pick the **Go / RE2** flavor so what you test is what the agent runs.

## The pre-filter — `agent.regex`

The pre-filter is the agent's first gate: only lines whose message matches at least one rule (a named rule or the catch-all `default_pattern`) are forwarded to the [miner](./miner.md). Everything else is dropped before grouping, so noise never reaches your [catalog](./catalog.md).

```yaml
agent:
  regex:
    default_pattern: "(?i).*error.*"   # catch-all; "" = strict, ".*" = learn everything
    rules:                             # named rules, first match wins
      - name: oom-killer
        pattern: "Out of memory: Killed process"
      - name: panic
        pattern: "(?i)panic:"
      - name: 5xx-burst
        pattern: "HTTP/[0-9.]+\\s+5\\d\\d"
```

- Named `rules` run **first, in order — first match wins**. The matching rule's `name` is stored on the pattern (as `rule`) so you can trace a [shadow](./shadow-mode.md) event back to the rule that flagged it.
- `default_pattern` runs only if no named rule matched. Set it to `".*"` to learn from every line, or `""` for strict mode (only explicit rules learn).

| Goal | Setting |
|---|---|
| Learn everything | `default_pattern: ".*"` |
| Only learn explicit matches | `default_pattern: ""` + a full `rules:` list |
| Learn error-ish lines (default) | `default_pattern: "(?i).*error.*"` |

See [Configuration](./configuration.md#regex) for the full field reference.

## Service patterns — `agent.service_patterns`

This is where regex work gets most exacting, because you're not just matching a line — you're **extracting** a piece of it. Each `service_patterns` entry is a regex with a **capture group**, and the text that group captures becomes the service name. The full feature (the nine shipped patterns, the log-shape table, troubleshooting) lives on the [Service Detection](./service-detection.md) page; this section is the regex mechanics behind it.

Four rules govern how these patterns behave:

1. **First match wins.** Patterns are tried top to bottom; the first that matches stops the search. So put your specific patterns *above* the generic fallbacks.
2. **The first capture group is the service name.** A pattern must have exactly one `( … )` group. A pattern with **no** group is rejected at startup (`missing capture group` in the logs) and skipped.
3. **Bare log levels are refused.** If a group captures exactly `TRACE` / `DEBUG` / `INFO` / `WARN` / `WARNING` / `ERROR` / `FATAL`, that match is skipped and the next pattern is tried — a service is never named `DEBUG`.
4. **Colour codes are stripped first.** ANSI colour escapes from console loggers are removed before matching, so a colourised name matches the same as a plain one.

If nothing matches (or the list is empty), the service is **`_unknown`**.

### The one rule you can't skip: a capture group

To detect a service literally called `authen-service`:

| Pattern | Works? | Why |
|---|---|---|
| `.*authen-service.*` | No | No capture group — rejected at startup. |
| `.*(authen-service).*` | Yes | Captures `authen-service`, but greedy. |
| `\b(authen-service)\b` | Best | Captures it on a word boundary — tighter and faster. |

> **Tip:** `\b` is a "word boundary" — the edge of a word — so `authen-service` isn't caught inside `reauthen-service-v2`. Prefer it over `.*`.

### Copy-paste examples

```yaml
agent:
  service_patterns:
    - '\b(authen-service|billing-service|orders-api)\b'   # your exact names, tried first
    - '\b([a-z]+-service)\b'                              # any *-service name
    - 'app=([a-z0-9-]+)'                                  # a Kubernetes app= label
```

| Goal | Pattern | Detects from `… billing-service failed …` |
|---|---|---|
| Allow-list of exact services | `\b(authen-service\|billing-service\|orders-api)\b` | `billing-service` |
| Any `-service` name | `\b([a-z]+-service)\b` | `billing-service` |

You can also override the whole list from the environment with `AGENT_SERVICE_PATTERNS` (comma-separated, one regex per item). See [Service Detection](./service-detection.md#set-it-via-environment-variable) for the caveat about regexes that contain commas.

## Redaction patterns — `agent.redaction.extra_patterns`

The third regex surface is [redaction](./redaction.md). Every entry in `extra_patterns` is a Go regex; whatever it matches is replaced with `<REDACTED:custom<n>>` before the line reaches anything else. Unlike service patterns, these need **no** capture group — the whole match is scrubbed.

```yaml
agent:
  redaction:
    extra_patterns:
      - "(?i)password=\\S+"          # scrub any password=…
      - "cust_[A-Za-z0-9]{16}"       # scrub internal customer IDs
```

A pattern that fails to compile is skipped and logged at startup, so one typo can't disable redaction.

## Test and verify a pattern

Before you ship any regex, confirm it does what you think:

1. **Paste a real log line and your regex into [regex101](https://regex101.com)** with the **Go** flavor selected.
2. **For service patterns, check group 1** is the service name — not the timestamp, thread, or log level.
3. **Restart the agent and read the startup logs.** A compile error or `missing capture group` means the pattern was skipped — fix it and restart.
4. **Watch the [Patterns](./catalog.md) page.** For service patterns, confirm the `service` column shows the name you expect. For pre-filter rules, confirm the `rule` column shows your rule name. For redaction, confirm the sample reads `<REDACTED:…>`.

| Symptom | Likely cause | Fix |
|---|---|---|
| Pattern ignored at startup | Invalid regex, or a service pattern with no `( … )` group | Fix the syntax / add one capture group. |
| Wrong service captured | A looser pattern matched first, or the group grabbed the wrong field | Move your specific pattern **above** the generic ones; anchor it with `\b`. |
| Everything is `_unknown` | No `service_patterns` matches your log shape | Add a pattern that matches a real line, with one capture group. |
| A line you wanted isn't learned | The pre-filter dropped it | Add a rule, or broaden `default_pattern`. |

## See also

- [Service Detection](./service-detection.md) — the full feature, the nine shipped patterns, and the troubleshooting table.
- [Redaction](./redaction.md) — the built-in secret rules and how `extra_patterns` fits in.
- [Miner](./miner.md) — what happens to a line after the pre-filter lets it through.
- [Shadow Mode](./shadow-mode.md) — tuning pre-filter rules so shadow noise stays manageable.
- [Configuration](./configuration.md) — the `agent.regex`, `agent.service_patterns`, and `agent.redaction` references.

---

← Back to [AI SRE Agent overview](./agent-introduction.md)
