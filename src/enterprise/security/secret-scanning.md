# Secret scanning

_Versus Security · In development — not yet available_

> **Status: In development.** Secret scanning is on the roadmap and **not built
> yet**. There is nothing to enable today — no config, no UI, no API. This page
> describes the planned capability so you know it's coming.

Services accidentally log secrets — an API key in a debug line, a JWT in a request
dump, a `password=…` in a stack trace, a connection string in a config echo.
**Secret scanning** will surface those leaks from the logs Versus already collects,
so you can **rotate the exposed secret** and **fix the logging** that emitted it.

## What it will do for you

- Watch the logs you already ingest for **exposed secrets** — API keys, tokens,
  passwords, private keys, connection strings, and similar credential classes.
- Group repeat leaks into a **reviewable finding**: the secret *type*, the
  service / log pattern where it appears, how often it occurred, and when it was
  first and last seen.
- Point you at the fix — **rotate the secret, stop logging it** — as a
  human-reviewed action, never an automatic one.

## How it will fit

Secret scanning rides the redactor that
already runs on every log line before anything is stored. That scrubber already
*finds* secrets in order to redact them; secret scanning surfaces the same match as
a finding instead of silently dropping it — so it reuses a pass the agent already
makes rather than adding a new scan of your logs.

**The raw secret value is never captured, stored, or shown.** A finding is derived
*after* the value has already been redacted, so it carries the secret's *type* and
*location* and a redacted snippet — never the plaintext.

## See also

- [Versus Security](./overview.md) — the capability set this belongs to.
- [Fraud & abuse detection](./fraud-detection.md) — the other planned capability.
