# Analyze Tools Overview

When the AI Analyze agent investigates an incident, it doesn't reason from
the alert text alone. It reaches for a small toolbox of **read-only** tools
to gather context — recent incidents, a pattern's history, related log
lines, recent deploys, service dependencies, and your own runbooks — and
grounds its conclusion in what it finds.

This page is the map of that toolbox. For configuration and a full Docker
walkthrough see [Analyze Tools](./tools.md); the two external tools with
their own detailed guides are linked below.

## Read-only by design

Every analyze tool is **search-only**. A tool reads and ranks; it never
mutates cluster state, never runs a remediation, never triggers on-call, and
never sends a notification. The agent reads the evidence; a human still acts
on it.

Every tool call — its arguments and what it returned — is recorded in the
**Tool calls** section of the analysis result, so you can audit exactly what
the agent looked at and which sources grounded a finding.

## The tools

### Built-in — no configuration

These read state the agent already keeps, so they work out of the box on
every build:

| Tool | What it answers |
|---|---|
| `recent_incidents` | Are other services failing at the same time, or is this isolated? |
| `pattern_history` | Is this pattern brand-new, or a known one spiking above its baseline? |
| `describe_service` | What does normal look like for this service, and which patterns dominate? |
| `get_related_logs` | What were the surrounding (redacted) log lines saying when this fired? |

### External — need a `tools.yaml` block

These reach outside the agent, so each registers only when you configure it:

| Tool | What it adds | Guide |
|---|---|---|
| `describe_dependencies` | Upstream/downstream service graph for blast-radius reasoning | [Analyze Tools](./tools.md#describe_dependencies) |
| `recent_changes` | Recent commits from your deploy repos, to correlate an incident with a deploy | [Recent Changes](./recent-changes.md) |
| `find_runbook` | Similarity search over your own Markdown runbooks, so findings cite real remediation steps | [Find Runbook](./find-runbook.md) |

> On an **Enterprise** build with a metric/trace data source, two more tools —
> `query_metrics` and `query_traces` — auto-wire in from that source (no
> `tools.yaml` needed) so the agent can pull the relevant time series and
> spans during an investigation.

## Next

- [Analyze Tools](./tools.md) — configuration, the complete `tools.yaml`, and Docker / Compose examples.
- [Recent Changes Tool](./recent-changes.md) — git-history feed setup.
- [Find Runbook Tool](./find-runbook.md) — runbook-RAG setup, corpus directory, and security posture.
