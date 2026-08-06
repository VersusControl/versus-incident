# Versus Security

_In development · on the roadmap — not yet available_

> **Status: In development.** Versus Security is a planned capability set. It is
> **not built yet** and is **not available** in any release today. This page
> describes where the product is heading so you know what is on the roadmap — it
> does not describe features you can turn on now. There is no configuration, UI,
> or API for it yet.

**Versus Security** is a planned security agent that will run on the telemetry
Versus **already collects** for incident detection — your service logs,
API-gateway / access logs, metrics, and traces. Rather than adding new scanners or
new data sources, it turns the signals you already feed the agent into
**security findings** for a human to review and act on.

It opens with **two capabilities**:

| Capability | What it will do |
|---|---|
| [Secret scanning](./secret-scanning.md) | Surface secrets (API keys, tokens, passwords, connection strings) that services accidentally log, so you can rotate them and stop leaking them. |
| [Fraud & abuse detection](./fraud-detection.md) | Flag abusive or anomalous behavior — bots, credential stuffing, scraping, API abuse — from the same access/log telemetry. |

## How it will fit

Both capabilities are **intelligence layers on the telemetry you already ingest**,
not a separate product and not new external data sources. Detection stays
**deterministic and human-in-the-loop**: the findings are reviewable evidence for a
person to decide on, never auto-enforced. Where an LLM is involved, it only
*summarizes* a finding for review — it never *decides* one.

Both are planned as **Enterprise** intelligence with **open-source seams** in the
core agent. Availability, packaging, and timing are not set.

## See also

- [Secret scanning](./secret-scanning.md) — the redactor-backed leak surface.
- [Fraud & abuse detection](./fraud-detection.md) — behavioral findings on access logs.
