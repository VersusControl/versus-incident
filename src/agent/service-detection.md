# AI Agent — Service Detection

Every log line the agent reads gets tagged with a **service** — the name of the app or component that produced it. Service detection is how the agent reads that name out of the raw log text, so it can group signals ("all the errors from `orders-api`") instead of treating your whole system as one undifferentiated stream.

Without it, every signal lands in a single bucket called `_unknown`, and anything that reasons per service — new-service grace, per-service catalogs, the AI's "which service is on fire" summary — has nothing to work with.

> **Note:** A "service" here is just a label pulled out of the log line. It doesn't have to match anything in Kubernetes or your service mesh — it's whatever string your logs call the app.

## What a service is, and why it matters

| Term | One-sentence meaning |
|---|---|
| **Service** | The name of the app/component a log line came from, e.g. `orders-api`. |
| **`_unknown`** | The fallback a signal lands in when no pattern matches — detection is off, or your logs don't fit any rule. |
| **Detection off** | An empty `service_patterns` list. Every signal becomes `_unknown`. |

Grouping by service is what lets the agent say "there's a spike in `orders-api`" instead of "there's a spike somewhere." If detection is off or wrong, every per-service feature collapses back to one global bucket.

## How detection works

For each log line, the agent runs these steps in order:

1. **Strip colour codes.** Console loggers (Spring Boot, Logback) wrap fields in ANSI colour escapes like `\x1b[34morders-api\x1b[m`. The agent removes them first, so a colourised name matches the same as a plain one. (No colour bytes in the line? This step costs nothing.)
2. **Try each pattern in order.** `service_patterns` is an ordered list of regexes, tried top to bottom.
3. **First match wins.** The moment a pattern matches, the agent stops — later patterns are not tried.
4. **Take the captured name.** Each pattern has one capture group `( … )`; the text it captures is the service name.
5. **Skip bare log levels.** If that captured text is exactly a log level (`TRACE`, `DEBUG`, `INFO`, `WARN`, `WARNING`, `ERROR`, `FATAL`), the agent ignores this match and continues to the next pattern — a service is never named `DEBUG`.

If nothing matches (or the list is empty), the service is `_unknown`.

> **Note:** The shipped config includes the default list below, so detection works out of the box. To turn it off, set `service_patterns: []` — every signal then becomes `_unknown`.

## Default patterns

Versus ships a default `service_patterns` list (from `pkg/config/default_config.yaml`) covering the common log shapes. They're tried top to bottom; the first that matches wins.

| # | What it matches | Example log line | Detected |
|---|---|---|---|
| 1 | `service_name=…` (also `servicename:`, `"service_name":`) | `level=error service_name=orders msg="db timeout"` | `orders` |
| 2 | key=value `service=` / `svc=` / `app=` / `component=` | `ts=… svc=orders status=500` | `orders` |
| 3 | JSON `"service":"…"` (also `svc` / `app` / `component`) | `{"service":"orders","level":"error"}` | `orders` |
| 4 | Spring-Boot console: first word after the timestamp, right before the `[thread]` | `2026-06-30 12:00:01.123 orders-api [main] WARN …` | `orders-api` |
| 5 | Logback MDC: first name inside `[ svc , requestID=… ]` | `[ INFO ] [ orders-api , requestID=abc ] …` | `orders-api` |
| 6 | Two brackets — take the second `[ … ] [ svc ]` | `[INFO] [orders-api] request handled` | `orders-api` |
| 7 | Spring `--- [thread] [name]` — take the second bracket | `… --- [main] [orders-api] Started` | `orders-api` |
| 8 | syslog / journald `name[pid]:` | `orders-api[1234]: connection reset` | `orders-api` |
| 9 | Generic single bracket (last resort) | `[orders-api] cache miss` | `orders-api` |

### Colour codes and log levels

Two guards run automatically for **every** pattern, default or custom:

- **ANSI colour codes are stripped first.** A Spring-Boot console line like `2026-06-30 12:00:01 \x1b[34morders-api\x1b[m [main] WARN …` is matched as if it read `2026-06-30 12:00:01 orders-api [main] WARN …`, so pattern 4 detects `orders-api`. Without stripping, the colour bytes sit right against the name and defeat the pattern.
- **A bare log level is never a service.** If a pattern's capture group is exactly `TRACE` / `DEBUG` / `INFO` / `WARN` / `WARNING` / `ERROR` / `FATAL`, the match is skipped and the next pattern is tried. So a line like `[DEBUG] starting up` is never filed under a service called `DEBUG` — it falls through to a later pattern or to `_unknown`. Only a *bare* level is refused; `error-service` is a perfectly valid name.

## Define your own service pattern

The defaults cover common shapes, but the surest way to get clean grouping is to tell the agent exactly what your service names look like. Add your own regex to the **front** of `service_patterns`.

### The one rule you can't skip: a capture group

Every pattern must have exactly one capture group — the `( … )` around the part you want as the service name. A pattern with no group is rejected at startup (you'll see `missing capture group` in the logs) and skipped.

So to detect a service literally called `authen-service`:

| Pattern | Works? | Why |
|---|---|---|
| `.*authen-service.*` | No | No capture group — rejected at startup. |
| `.*(authen-service).*` | Yes | Captures `authen-service`, but greedy. |
| `\b(authen-service)\b` | Best | Captures `authen-service` on a word boundary — tighter and faster. |

> **Tip:** `\b` is a "word boundary" — it matches the edge of a word, so `authen-service` isn't caught inside `reauthen-service-v2`. Prefer it over `.*`.

### More worked examples

| Goal | Pattern | Example line | Detects |
|---|---|---|---|
| Allow-list of your exact services | `\b(authen-service\|billing-service\|orders-api)\b` | `… billing-service failed …` | `billing-service` |
| Any name ending `-service` | `\b([a-z]+-service)\b` | `… payment-service timeout` | `payment-service` |
| Kubernetes `app=` label | `app=([a-z0-9-]+)` | `app=orders-api pod=orders-api-7f…` | `orders-api` |

### Set it in YAML

Put your patterns first, then keep whichever defaults you still want as fallbacks:

```yaml
agent:
  service_patterns:
    - '\b(authen-service|billing-service|orders-api)\b'               # your exact names, tried first
    - '\b([a-z]+-service)\b'                                          # any *-service name
    - '(?i)\b(?:service|svc|app|component)\s*=\s*"?([A-Za-z0-9._-]+)' # key=value fallback
```

### Set it via environment variable

`AGENT_SERVICE_PATTERNS` overrides the whole list. It's **comma-separated**, one regex per item, surrounding whitespace trimmed:

```bash
export AGENT_SERVICE_PATTERNS='\b(authen-service|billing-service)\b,\b([a-z]+-service)\b'
```

> **Warning:** The env var splits on commas, so a regex containing a literal comma (like the Logback MDC pattern `[ svc , requestID=… ]`) can't be expressed this way — use `service_patterns` in YAML for those.

### Three rules to remember

| Rule | Why it matters |
|---|---|
| **Order matters — specific first.** | First match wins, so put your exact-name patterns above the generic bracket fallbacks. Otherwise a loose rule grabs a thread or logger name before your rule is reached. |
| **One capture group per pattern.** | The first group is the service name. A pattern with no group is rejected at startup. |
| **Test before you ship.** | Paste a real log line and your regex into a tester (e.g. regex101 with the Go / RE2 flavour) and confirm group 1 is the service name — not the level, thread, or timestamp. |

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| Wrong name detected (a thread, logger, or level, not the app) | A looser/generic pattern matched before a specific one, or a positional token grabbed the wrong field | Add a pattern for your exact layout and put it **first**; tighten it with `\b` or anchors so it targets the service field. |
| Everything is `_unknown` | `service_patterns` is empty (detection off), or no pattern matches your log shape | Add a pattern that matches a real log line, and confirm it has one capture group. |
| Startup log says `missing capture group` and the pattern is ignored | The regex has no `( … )` group | Wrap the service part in parentheses: `.*authen-service.*` → `\b(authen-service)\b`. |
| A colourised console name isn't detected | (Shouldn't happen — colours are stripped automatically) | Confirm the name really is the captured field once colours are removed, then match the plain-text layout. |
| A level like `DEBUG` used to show as the service | A greedy pattern captured the level word | Handled by the built-in level guard; if you wrote the pattern, anchor it to the service field, not the level bracket. |

## See also

- [Configuration](../configuration/configuration.md) — the full `agent.service_patterns` / `AGENT_SERVICE_PATTERNS` reference.
- [AI Detect Mode](./ai-detect-mode.md) — where per-service grouping and `new_service_grace` come into play.
- [Catalog](./catalog.md) — the agent's per-service memory of learned patterns.

---

← Back to [AI SRE Agent overview](./agent-introduction.md)
