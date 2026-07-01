# AI Agent — Redaction

**Redaction** is scrubbing sensitive text — secrets and personal data — out of a log line before anything else touches it. The agent does this the moment it reads a signal, so the parts you don't want to keep or send anywhere are replaced with a harmless placeholder first.

This matters because everything downstream sees the *scrubbed* copy: the [miner](./miner.md) that groups lines into patterns, the [catalog](./catalog.md) that stores them on disk, the [shadow log](./shadow-mode.md), and — in detect mode — the [AI SRE](./ai-detect-mode.md) you send text to. A password that gets redacted here never gets learned, never gets written to `patterns.json`, and never leaves your box in a prompt.

## What you'll learn

- What the agent redacts out of the box.
- What a redacted line looks like.
- How to add your own patterns and turn on IP redaction.
- Why redaction runs first, and what it deliberately leaves alone.

## Where redaction runs

Redaction is the **first** step of every tick, before filtering, grouping, or storing:

```
read signal → REDACT → filter (regex) → group (miner) → catalog → shadow / detect
```

For each signal the agent scrubs two things:

- **`Message`** — the raw log line text.
- **`Fields`** — structured fields from JSON logs, walked recursively (nested maps and lists included).

Every match is swapped for a token that names the rule that caught it:

```
before:  user alice@corp.com logged in with token=sk_live_9f2ab
after:   user <REDACTED:email> logged in with <REDACTED:password>
```

> **Note:** Redaction is regex-based, not a full parser. The goal is to make it *operationally reasonable* to store log content and send it to an LLM — not to be a perfect data-loss-prevention tool. Treat it as defense-in-depth, and still rotate any credential you know landed in a log.

## What's redacted by default

Redaction is **on by default** when the agent is enabled (`agent.redaction.enable: true`). These built-in rules always run. Order matters — more specific rules run first so, for example, an email isn't half-eaten by another rule.

| Token | What it catches | Example match |
|---|---|---|
| `<REDACTED:jwt>` | JSON Web Tokens (`header.payload.signature`) | `eyJhbGciOi….eyJzdWIi….sig` |
| `<REDACTED:aws_key>` | AWS access key IDs (`AKIA…` / `ASIA…`) | `AKIAIOSFODNN7EXAMPLE` |
| `<REDACTED:openai_key>` | OpenAI API keys (`sk-…`, including project keys `sk-proj-…`) | `sk-proj-A1b2C3d4…` |
| `<REDACTED:slack_token>` | Slack tokens (`xoxb-` bot, `xoxp-` user, `xoxa-`/`xoxr-`/`xoxs-`) | `xoxb-24012345678-…` |
| `<REDACTED:basic_auth>` | Basic-auth credentials in a URL (`user:pass@…`) | `://alice:s3cr3t@` |
| `<REDACTED:bearer>` | `Authorization: Bearer …` headers | `Authorization: Bearer ab12._-` |
| `<REDACTED:password>` | `password` / `passwd` / `pwd` / `secret` / `token` / `api_key` followed by `=` or `:` and a value | `token=sk_live_9f2ab`, `password: hunter2` |
| `<REDACTED:email>` | Email addresses | `alice@corp.com` |
| `<REDACTED:uuid>` | UUIDs | `3f2504e0-4f89-41d3-9a0c-0305e82c3301` |
| `<REDACTED:user_agent>` | Browser User-Agent strings | `Mozilla/5.0 (…) Chrome/…` |

Two more rules cover IP addresses but are **off by default**, because an IP is usually useful context (which host, which client) rather than a secret. Turn them on with `redact_ips: true`:

| Token | What it catches | Enabled by |
|---|---|---|
| `<REDACTED:ipv4>` | IPv4 addresses | `redact_ips: true` |
| `<REDACTED:ipv6>` | IPv6 addresses | `redact_ips: true` |

The shipped config also adds two **extra** patterns on top of the built-ins as belt-and-suspenders (they show up as `<REDACTED:custom0>` / `<REDACTED:custom1>`):

```yaml
agent:
  redaction:
    enable: true
    redact_ips: false
    extra_patterns:
      - "(?i)password=\\S+"                 # any password=…
      - "Authorization:\\s*Bearer\\s+\\S+"  # any Bearer header
```

## Configuration

```yaml
agent:
  redaction:
    enable: true          # master switch (default true when the agent is on)
    redact_ips: false     # opt in to IPv4/IPv6 redaction
    extra_patterns:       # your own Go regexes, added to the built-ins
      - "(?i)password=\\S+"
      - "Authorization:\\s*Bearer\\s+\\S+"
```

| Key | Type | Default | Description |
|---|---|---|---|
| `enable` | bool | `true` | Turn redaction on or off. Leave it on in production. |
| `redact_ips` | bool | `false` | Also redact IPv4/IPv6 addresses. Off because IPs are usually useful context. |
| `extra_patterns` | string list | two shipped patterns (above) | Extra Go regexes to scrub. Each becomes a `custom<n>` rule. |

See [Configuration](./configuration.md) for how the `redaction` block sits inside the rest of the agent config.

## Add your own patterns

The built-ins cover common secret shapes, but your logs may carry something specific — an internal API key format, a customer ID, a session cookie. Add a Go regex to `extra_patterns`:

1. **Write the regex** for the shape you want gone. Match the whole secret, not just its label — the matched text is what gets replaced.

   ```yaml
   agent:
     redaction:
       extra_patterns:
         - "cust_[A-Za-z0-9]{16}"                  # internal customer IDs
         - "(?i)x-session-cookie:\\s*\\S+"          # a custom auth header
   ```

2. **Restart the agent.** Patterns are compiled at startup.

3. **Confirm it took.** Send a test line containing the secret through a source and check the sample on the [Patterns](./catalog.md) page — the secret should read `<REDACTED:custom0>`.

> **Tip:** The agent uses Go's RE2 regex flavor (no backreferences or lookaround). If you're testing at regex101, pick the **Go** flavor. See [Regex](./regex.md) for the mechanics and a test workflow.

## Behavior and edge cases

- **A bad pattern can't disable redaction.** Each `extra_patterns` entry is compiled independently. An invalid regex is skipped and logged at startup — the built-ins and every other valid pattern keep working, so one typo never opens the floodgates.
- **`Raw` is left untouched.** The agent scrubs `Message` and `Fields`, but keeps the original `Raw` payload intact for admin debugging only. Operators never see `Raw` outside the admin surface, and it's never sent to the model.
- **Redacted tokens become wildcards.** The [miner](./miner.md) treats a `<REDACTED:…>` token as a variable (`<*>`), so two lines that differ *only* by a secret still collapse into one pattern instead of fragmenting your catalog.
- **The AI only ever sees scrubbed text.** In [detect mode](./ai-detect-mode.md), the sample line, template, and fields sent to the model are already redacted — the secret is gone before the prompt is built.

## Why it matters

| Concern | How redaction helps |
|---|---|
| **Privacy** | Emails, UUIDs, and (optionally) IPs are stripped before they're stored or grouped. |
| **Secret hygiene** | Tokens, passwords, AWS keys, JWTs, Bearer headers, OpenAI (`sk-…`) and Slack (`xoxb-…`) tokens, and basic-auth URL credentials don't get persisted to `patterns.json`. |
| **Sending logs to an LLM** | Detect mode sends samples to an external model; redaction is what makes that operationally acceptable. |
| **Compliance** | You control exactly what's scrubbed via `extra_patterns`, and can prove it from the config. |

<!-- Visual Designer prompt: a simple left-to-right pipeline strip — "raw log line" box containing a red-highlighted email + token, an arrow through a "Redact" gate, and a "scrubbed line" box where those two are replaced by grey <REDACTED:email> / <REDACTED:password> chips; then arrows fanning to three small labelled boxes: Miner, Catalog, AI SRE. Matches the docsify Warp-style theme. -->

## See also

- [Regex](./regex.md) — the RE2 mechanics behind `extra_patterns` (and every other pattern in the agent).
- [Miner](./miner.md) — how a redacted line becomes a template.
- [Catalog](./catalog.md) — where the scrubbed samples are stored.
- [Configuration](./configuration.md) — the full `agent.redaction` reference.
- [AI Detect Mode](./ai-detect-mode.md) — where scrubbed samples are sent to the model.

---

← Back to [AI SRE Agent overview](./agent-introduction.md)
