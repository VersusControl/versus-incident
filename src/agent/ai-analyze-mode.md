# AI Agent — Analyze Mode

Analyze mode provides a **deep-dive investigation** for incidents. While detect mode identifies patterns and sends notifications, analyze mode helps you understand the root cause of an incident by gathering context and presenting structured insights directly in the dashboard.

Analyze mode is designed to assist on-call engineers, post-incident reviewers, and anyone needing a detailed analysis of an incident.

---

## When to Use Analyze Mode

Analyze mode is available whenever `agent.ai.enable: true`. If AI is disabled, the **Analysis** card on the incident detail page will show "coming soon."

Typical scenarios for using analyze mode:

- **On-call troubleshooting**: Quickly generate a starting hypothesis for unfamiliar incidents.
- **Correlating patterns**: Investigate incidents that resemble past patterns.
- **Post-incident review**: Add structured AI insights to human-written notes.

---

## How It Works

1. **Trigger Analysis**: Click **Run analysis** on the incident detail page.
2. **AI Investigation**: The AI gathers context, such as recent incidents, pattern history, and service summaries.
3. **Insights Delivered**: The results are saved and displayed in the dashboard for review.

Analyze mode runs within a 2-minute timeout to ensure responsiveness. Results are always saved, even if the analysis encounters errors.

---

## Configuration

Analyze mode uses the shared `agent.ai` settings. You can optionally point it at a stronger model for deep dives with the `analyze:` block:

```yaml
agent:
  enable: true
  mode: detect            # detect or shadow; analyze works in both
  ai:
    enable: true
    api_key: ${AGENT_AI_API_KEY}
    model: gpt-4o-mini    # shared default for detect + analyze

    analyze:
      model: gpt-4o       # use a stronger model for deep dives
```

---

## Investigation Tools

During an analysis the AI doesn't just read the incident in isolation —
it can call **read-only tools** to pull supporting context from your
own data before forming a conclusion. Every tool is strictly
observational: tools only *read* state, they never send notifications,
mutate incidents, or touch external systems.

Each tool returns the same envelope so the AI can reason about results
uniformly:

- `found` — whether the lookup returned anything.
- `data` — the tool-specific payload (only present on a hit).

The full tool-call trail (which tools ran, their arguments, and what
they returned) is recorded with every analysis and shown in the
**Tool calls** section of the analysis result, so you can audit exactly
what the AI looked at.

### Available tools

#### `recent_incidents`

Lists incidents recorded in a recent time window so the AI can spot
correlated failures, repeat offenders, or a broader outage in progress.

- **Time window** — defaults to the last 60 minutes, up to a maximum of
  1440 minutes (24 hours).
- **Service filter** — optionally narrows the list to a single service.
- **Limit** — returns up to 20 incidents by default, capped at 100.

Use case: *"Are other services failing at the same time, or is this
incident isolated?"*

#### `pattern_history`

Looks up a learned pattern by its id and returns everything the agent
knows about it: the log template, the EWMA frequency baseline, the
operator-set verdict, tags, observation counts, and the associated
service.

Use case: *"Is this a brand-new pattern, or a known issue that has
spiked above its normal baseline?"*

#### `describe_service`

Summarises a single service: when it was first seen by the agent and
its top learned patterns ranked by frequency.

Use case: *"What does normal look like for this service, and which
patterns dominate its logs?"*

---

## Roadmap — more tools for deeper analysis

The three tools above form the foundation of analyze mode. The tool
interface is intentionally pluggable, and we plan to expand the
catalog so the AI can investigate further without manual digging.
Areas we're exploring for future tools include:

- **Deploy & change correlation** — line up an incident with recent
  deployments, config changes, or feature-flag flips.
- **Metric & dashboard lookups** — pull the relevant time-series
  (error rate, latency, saturation) around the incident window.
- **Cross-source log retrieval** — fetch a wider slice of raw logs
  from the originating data source on demand.
- **Dependency awareness** — surface upstream/downstream services that
  could explain a cascading failure.

As new tools ship they will appear automatically in the **Tool calls**
audit trail, and no configuration change is required to benefit from
them.

---

## Key Features

- **Non-intrusive**: Analyze mode never sends notifications or modifies systems.
- **Read-only tools**: The AI uses tools like `recent_incidents`, `pattern_history`, and `describe_service` to gather context.
- **Customizable**: Fine-tune the AI's behavior with optional settings.

Analyze mode empowers you to make informed decisions by providing structured insights when you need them most.
