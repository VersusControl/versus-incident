# SRE Agent

The SRE Agent learns what normal looks like from your **logs, metrics and
traces**. It stays quiet about what is normal and tells you only when something
**new or unusual** happens. When it does, it raises one clear incident: what
happened, which service, how bad, and what to check first.

![Three inputs flow into the SRE Agent: Logs, marked start here, plus Metrics and Traces, both marked Enterprise. The agent learns what normal looks like for each one and sends out a single incident when something new or unusual happens](../docs/images/sre-agent-overview.svg)

| Signal | What the agent learns | Read more |
|---|---|---|
| **Logs** | Groups log lines into patterns and flags a new pattern or a sudden spike. This is where you start, and it works in OSS. | [Data Sources](./data-sources.md), [Quickstart](./quickstart.md) |
| **Metrics** (_Enterprise_) | Finds each service's request rate, error rate, latency and resource use, learns their normal by time of week, and flags a signal that stays far outside it. | [Metrics](../enterprise/metrics/overview.md) |
| **Traces** (_Enterprise_) | Learns each operation's p99 latency and error rate by time of week, and flags an operation that stays clearly slower or fails more. | [Traces](../enterprise/traces.md) |

**Install and configure:** [Deploy on Kubernetes](../configuration/kubernetes.md),
or use [Helm](../configuration/helm.md). Every agent setting is in the
[Configuration](./configuration.md) reference.

## What it does

The rest of this page covers logs, the easiest place to start. For instance, the SRE Agent monitors your logs automatically. In the image below, on the left, a noisy log stream of thousands of repeating lines with one new checkout timeout hidden among them; on the right, the same stream after the agent has learned normal, with the repeating lines grouped as known and a single incident raised for the new checkout timeout.

![Before and after: on the left, a noisy log stream of thousands of repeating lines with one new checkout timeout hidden among them; on the right, the same stream after the agent has learned normal, with the repeating lines grouped as known and a single incident raised for the new checkout timeout](../docs/images/sre-agent-concept.svg)

| Step | In plain words |
|---|---|
| **Watch** | Reads new lines from your log sources: a file, Loki, Elasticsearch, CloudWatch Logs, Graylog, Splunk or SigNoz. See [Data Sources](./data-sources.md). |
| **Learn normal** | Groups lines that look alike into patterns and counts how often each one happens. A pattern seen often enough becomes **known**. |
| **Tell you about the unusual** | Raises an incident for a pattern it has never seen, or for a known pattern that suddenly fires far more than usual (a [spike](./spike.md)). |
| **Explain** | Writes the incident summary, severity and next steps. With an AI key, the AI writes them; without one, the agent still raises the incident with a clear templated summary. |

Secrets in your logs (tokens, keys, emails) are masked before the agent stores
or sends anything. See [Redaction](./redaction.md).

The agent is **off by default** (`agent.enable: false`). It reads nothing
until you turn it on.

## The mode journey: Training → Shadow → Detect

You move the agent through three modes as you build trust in it.

| Mode | What it does | Sends alerts? | Page |
|---|---|---|---|
| `training` | Learns your normal patterns. | No | [Training Mode](./training-mode.md) |
| `shadow` | Keeps learning, and records what it *would* have alerted on so you can review it. | No | [Shadow Mode](./shadow-mode.md) |
| `detect` | Raises real incidents for new patterns and spikes. Incidents go to your channels; high and critical ones also go to on-call when it is enabled. | Yes | [AI Detect Mode](./ai-detect-mode.md) |

A typical rollout:

1. **Training** for a few days, until new patterns stop appearing often.
2. **Shadow** for a release cycle. Review what it would have flagged and mark
   the harmless ones as known.
3. **Detect** when the shadow list is mostly real problems.

In OSS you set the mode with `agent.mode` in `config.yaml` or the `AGENT_MODE`
environment variable, then restart. What the agent has learned is kept across
the switch. _Enterprise_ admins can also switch the mode from **Admin →
Runtime mode** in the UI.

```yaml
agent:
  enable: true
  mode: training     # training | shadow | detect
```

## Try it in minutes

The [AIOps SRE Quickstart](./quickstart.md) runs Versus next to a local Loki
with Docker Compose. You watch training learn the demo logs, switch to detect,
send an error the agent has never seen, and see the incident in the UI and in
Slack.

## More of the agent

| Page | What it covers |
|---|---|
| [Spike Detection](./spike.md) | Paging on a known error that suddenly fires far more than usual. |
| [AI Analyze](./ai-analyze-mode.md) | A deeper AI investigation of one incident, on demand. |
| [DevOps Agent Chat](./devops-agent.md) | Ask questions about your system in a chat, with cited evidence. |
| [Service Detection](./service-detection.md) | How each log line gets a service name. |
| [Service Heatmap](./service-heatmap.md) | Every service's recent health on one screen. |
| [Incident Reports](./incident-report.md) | A shareable summary card of your incidents. |
| [Configuration](./configuration.md) | Every agent setting and environment variable. |

How the agent masks, filters, groups and remembers lines is explained in the
Core Concept pages: [Redaction](./redaction.md), [Regex](./regex.md),
[Miner](./miner.md) and [Catalog](./catalog.md).
