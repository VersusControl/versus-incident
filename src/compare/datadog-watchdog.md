# Versus vs Datadog Watchdog

**Versus Incident is the self-hosted AI SRE agent.** Datadog Watchdog is the
AIOps layer inside Datadog's cloud platform. Both use machine intelligence to
surface anomalies — the difference that matters is **where your data lives** and
**what you pay to keep it watched.**

This is an honest comparison. Watchdog is a strong product inside a mature
platform. If you are already all-in on Datadog and your compliance team is
comfortable shipping logs to a US SaaS, Watchdog is convenient. The reasons to
choose Versus are below.

## The wedge: your data never leaves your infrastructure

Datadog Watchdog runs in Datadog's cloud. To get anomaly detection, your logs and
metrics are ingested into Datadog's platform first. For teams in EU fintech,
health, and government — or anyone with data-residency, sovereignty, or air-gap
requirements — that is the blocker.

Versus is **self-hosted by default.** You run the same Go binary in your own
infrastructure (or fully air-gapped). The AI agent reads your logs in place and only ever emits
an *incident notification* outward — never the raw data. Nothing is shipped to a
vendor for analysis.

## Side-by-side

| | **Versus Incident** | **Datadog Watchdog** |
|---|---|---|
| Deployment | Self-hosted (your infrastructure) or air-gapped | Datadog cloud (SaaS) |
| Where your logs live | In your infrastructure | Ingested into Datadog |
| Data residency / sovereignty | You control it entirely | Datadog regions only |
| Licensing | MIT core, free forever + commercial Enterprise | Proprietary, per-host + ingest |
| Pricing model | Monitored services, not per-seat | Per-host + per-GB ingest + indexing |
| AI detection | Drain miner + EWMA + LLM detect, **auditable** (`detect.json`) | Proprietary, opaque |
| Explainability | Deterministic patterns + recorded AI decisions | Black-box scoring |
| Bring your own LLM | Yes (per-org model gateway, BYO key) | No |
| On-call relationship | Feeds PagerDuty / Opsgenie / incident.io | Datadog On-Call (its own stack) |
| Lock-in | None — open core, your data, your infra | Platform + billing lock-in |

## Where Datadog Watchdog is the better fit

- You already run the full Datadog platform and want correlation across metrics,
  traces, and logs in one pane.
- You have no data-residency constraint and prefer a fully managed SaaS.
- You want APM-grade trace anomaly detection today (Versus is log-first; metrics
  and traces are on the [roadmap](https://github.com/VersusControl/versus-incident/blob/main/ROADMAP.md)).

## Where Versus wins

- **Data residency is non-negotiable.** Regulated buyers can run Versus where the
  data already is. Nothing leaves your infrastructure.
- **Auditability.** Every AI decision is explainable and recorded — you can buy
  what you can audit.
- **Cost shape.** You pay on monitored services, not on ingest volume and
  per-host agents. No surprise overage on a noisy day.
- **No lock-in.** The core is MIT. If you stop paying for Enterprise, the binary
  keeps running in full community mode.

## Try it

Run the MIT core in under 30 minutes:
[AI Agent — Getting Started](../agent/getting-started.md). Need data-residency
guarantees or a self-hosted enterprise quote? Email
<a href="mailto:admin@versusincident.com?subject=Versus%20vs%20Datadog%20Watchdog">admin@versusincident.com</a>.
