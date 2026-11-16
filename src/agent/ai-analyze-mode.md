# AI Analyze

## Concept

Detect tells you *that* something is wrong. **AI Analyze** helps you work out
*why*. On any incident, click **Run analysis** and the AI investigates: it
looks at the incident, related patterns, recent incidents and, through
read-only tools, your connected data. It then writes root cause hypotheses,
its confidence, and what to check next.

Analyze only reads. It never sends notifications, changes an incident, or runs a
fix. You can run it again at any time; earlier analyses stay on the incident.

## Example

At 14:07 an incident opens: `Anomaly: checkout-api — … acme-pay did not answer
within 30s`. The on-call engineer has not worked on checkout before. They open
the incident and click **Run analysis**.

Seconds later the analysis is titled `checkout-api: calls to acme-pay time
out`, rated high, and points at the dependency rather than `checkout-api`
itself. It ranks three root cause hypotheses: acme-pay is slow or unreachable
(78%), a recent `checkout-api` change lowered its timeout (34%), or the network
path to acme-pay is degraded (21%). The next steps start with "check the
acme-pay status page". The **Tool calls** section lists every lookup the AI
made, so the engineer can see exactly which evidence it used.

## What you see in the UI

Open an incident from **Incidents** and click **Run analysis**. When it
finishes, an **Analysis complete** window shows the result:

| Part | What it shows |
|---|---|
| Header | The analysis title, severity, category, confidence percentage and model. |
| Summary | A short explanation of what is going on. |
| **Root cause hypotheses** | Possible causes, each with its own likelihood and the reason for it. |
| **Evidence** | What each tool lookup found, in one line per tool. |
| **Next steps** | What to check or do first. |
| **Tool calls** | Every lookup, with its arguments, output and duration. |

![The Analysis complete window for checkout-api: title checkout-api: calls to acme-pay time out with HIGH, dependency and 78% conf badges, a summary, three root cause hypotheses at 78%, 34% and 21%, evidence from get_related_logs, list_patterns and the incident snapshot, four next steps, and the start of the expanded Tool calls (2) list](../docs/images/sre-analyze-ui-dark.png)

Expand **Tool calls** to audit the lookups:

![The Tool calls (2) section of the same analysis: get_related_logs with arguments limit 20, service checkout-api and time_range_minutes 30 and its log output from loki:quickstart-loki, and list_patterns with arguments limit 20, query checkout-api and service checkout-api and its output of 2 matching patterns](../docs/images/sre-analyze-tools-ui-dark.png)

Use **View full analysis** to open it on its own page. Every analysis is also
listed under **AI → Analyses**, and earlier analyses stay on the incident.

When AI is not set up, **Run analysis** is disabled and says that AI is not
enabled.

## Turn it on

Analyze uses the same AI settings as detect. It works in any mode and on any
incident, including webhook alerts.

```yaml
agent:
  enable: true
  ai:
    enable: true
    api_key: ${AGENT_AI_API_KEY}
    model: gpt-4o-mini        # shared with detect
    analyze:
      model: gpt-4o           # optional: a stronger model just for analyses
```

Which tools the AI may call during an analysis is set separately from Chat. See
the [Tool Reference](./tools/overview.md).

## Reference

- [Configuration](./configuration.md): the `agent.ai` and `agent.ai.analyze` keys.
- [Tool Reference](./tools/overview.md): the read-only tools and their policies.
- [DevOps Agent Chat](./devops-agent.md): ask follow-up questions in a chat.
