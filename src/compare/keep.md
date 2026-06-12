# Versus vs Keep

Keep is a popular open-source **alert management and AIOps platform** — a strong
aggregation and workflow layer for alerts you already have rules for. **Versus
Incident is the self-hosted AI SRE agent**: it *generates* the signal in the
first place by learning what your logs normally look like, then routes only what
is new or unexpected issues.

Both are open source and both self-host. Keep is the closest OSS competitor, so
this comparison leads with the two things that actually differ: **the AI agent**
and **how the two products think about alerts.**

## The wedge: an AI agent that writes the alerts for you

Keep is excellent at *managing* alerts — deduplicating, correlating, and running
workflows over alerts that **your existing rules already fire.** You still have to
author and maintain those rules.

Versus inverts that. Point the AI SRE agent at your logs and it **learns your
normal patterns** (drain miner + EWMA + grace periods) and opens an incident the
moment a brand-new error or anomaly appears — **with no alert rules to write.**
The detection is deterministic where it can be and AI-assisted where it helps, and
every decision is recorded in `detect.json` so it stays auditable.

You can run both together: let Versus detect, and let any downstream tool manage.
But if your goal is *fewer rules and less noise at the source*, that is the Versus
job.

## Side-by-side

| | **Versus Incident** | **Keep** |
|---|---|---|
| Primary job | **Detect** new/anomalous behaviour from logs | **Manage** alerts you already fire |
| Needs you to write alert rules | No — learns normal automatically | Yes — ingests existing alerts/rules |
| AI approach | Drain miner + EWMA + LLM detect/analyze, auditable | AI correlation/enrichment over alerts |
| Self-hosted | Yes (or air-gapped) | Yes |
| License | MIT core + commercial Enterprise | Open source (AGPL) + commercial |
| Bring your own LLM | Yes (per-org BYO key, Enterprise) | Varies |
| Data residency | Data stays in your infrastructure | Self-hostable |
| On-call relationship | Feeds PagerDuty / Opsgenie / incident.io | Routes to many tools |
| Notification channels | Slack, Teams, Telegram, Viber, Email, Lark | Many integrations |

## Where Keep is the better fit

- You already have a mature set of alert rules and want a powerful place to
  **aggregate, deduplicate, and run workflows** over them.
- Your problem is *too many alerting sources to wrangle*, not *too few useful
  signals*.

## Where Versus wins

- **No rules to maintain.** The AI agent learns normal and raises only what is
  new — the noise-reduction happens at detection time.
- **Auditable AI.** Deterministic patterns plus recorded AI decisions, which
  regulated buyers can actually approve.
- **Self-hosted, data stays put.** The MIT core runs in your infrastructure or air-gapped;
  logs never leave for analysis.
- **Permissive licensing.** The core is MIT, not copyleft — fewer questions from
  legal about embedding it in your stack.

## Use them together

Versus is **upstream** of alert management. A clean pattern is: Versus detects
anomalies from logs → fans out to your channels → escalates to on-call. If you
already run Keep, point Versus's webhook output at it and let Keep manage the
incident lifecycle.

## Try it

Run the MIT core in under 30 minutes:
[AI Agent — Getting Started](../agent/getting-started.md). Email
<a href="mailto:supports@devopsvn.tech?subject=Versus%20vs%20Keep">supports@devopsvn.tech</a> for enterprise pricing.
