# DevOps Agent Chat

## Concept

**Chat** lets you ask the DevOps Agent questions about your system in plain
words: "what changed before the latest incident?", "which services have the
highest error rate?". The agent answers using read-only tools over your
incidents, learned patterns and connected data, and shows the evidence behind
each answer so you can check it.

Chat only reads. It does not run fixes or change your systems or clusters.

## Example

It is Monday morning and you are taking over on-call. You open Chat and ask:

> Summarize unresolved incidents from the last 24 hours.

The agent searches your incident history and answers: two unresolved
incidents, a `jobrunner` error rate spike and `checkout-api` acme-pay timeouts.
It marks what it confirmed and what is only a guess, and cites its evidence as
**[1] Searching incident history**. You follow up:

> Which services have the highest error rate?

Each answer cites the tool results it used. Open one to see the raw evidence.
Your thread is saved, so you can return to it later in the shift.

## What you see in the UI

Open **Respond → Chat**. A new thread offers suggested questions; type your own
in **Ask about your system**.

![The DevOps Agent chat page answering Summarize unresolved incidents from the last 24 hours: the search_incidents tool call expanded with its arguments (limit 20) and output, then the answer 2 unresolved incidents, jobrunner error rate spike and checkout-api acme-pay timeouts abort checkouts, a speculative note, the evidence chip [1] Searching incident history, and the message box at the bottom](../docs/images/sre-chat-ui-dark.png)

| Control | What it does |
|---|---|
| **Stop** | Cancels a running answer. |
| History icon | Opens your saved threads. Start a **New thread** or delete one you no longer need. |
| Tool icon | Opens **Connectors & Tools**, the list of tools Chat may use. |

## Turn it on

Chat needs the agent and AI turned on:

```yaml
agent:
  enable: true
  ai:
    enable: true
    api_key: ${AGENT_AI_API_KEY}
    model: gpt-4o-mini
```

What Chat can see depends on your data sources, connected tools, and the Chat
tool policy. The policy is separate from [AI Analyze](./ai-analyze-mode.md):
enabling a tool for Chat does not enable it for Analyze.

> **Don't paste secrets into Chat.** Tool results are bounded and some fields
> are scrubbed, but not every tool response is fully masked. Review what each
> connector can read before you enable it.

## Reference

- [Tool Reference](./tools/overview.md): every read-only tool, what it needs, and
  the Chat and Analyze policies.
- [Kubernetes Configuration](./kubernetes/configuration.md): access for the
  Kubernetes tools.
- [Configuration](./configuration.md): the `agent.ai` keys.
