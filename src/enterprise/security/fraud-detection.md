# Fraud & abuse detection

_Versus Security · In development — not yet available_

> **Status: In development.** Fraud & abuse detection (also called abuse / threat
> detection) is on the roadmap and **not built yet**. There is nothing to enable
> today — no config, no UI, no API. This page describes the planned capability so
> you know it's coming.

Versus already learns what "normal" looks like for your telemetry to catch
operational anomalies. The **same telemetry** — especially your API-gateway /
access logs — carries the signals needed to spot **abuse**: a burst of requests
from one IP, a spray of auth failures across accounts, one API key hammering an
endpoint far above its usual rate, sequential scraping of resources.

## What it will do for you

- Profile behavior **per entity** — per client IP, per API key, per user — from the
  access and service logs you already ingest.
- Flag abusive or anomalous patterns as **reviewable findings**: bot / scraper
  traffic, credential stuffing, API-rate abuse, and off-baseline access.
- Hand you the **evidence and the entity list** — the offending IPs, the abused
  keys, the credential-stuffing sources — so you can acknowledge, dismiss, or
  **export a blocklist** to enforce in **your own** WAF or gateway.

## How it will fit

Fraud & abuse detection is an **intelligence layer on the telemetry you already
have**, not a new pipeline or a new external data source. Detection is
**deterministic** — counters, ratios, and baseline deviation with explicit reason
codes — and **human-in-the-loop**: Versus surfaces the finding, you decide and
enforce. Versus does not block traffic inline; it produces findings for review.

## See also

- [Versus Security](./overview.md) — the capability set this belongs to.
- [Secret scanning](./secret-scanning.md) — the other planned capability.
