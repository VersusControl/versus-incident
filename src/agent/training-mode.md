# Training Mode

## Concept

Training is where the agent learns what **normal** looks like. It reads your
logs, groups lines that look alike into **patterns**, and counts how often each
pattern happens. It never raises an incident and never sends anything.

A pattern is a log line with the changing parts blanked out. These three lines:

```text
checkout-api payment declined for order 81723 (card_expired)
checkout-api payment declined for order 81731 (card_expired)
checkout-api payment declined for order 81802 (card_expired)
```

become one pattern:

```text
checkout-api payment declined for order <*> (card_expired)
```

Each pattern starts as **still learning**. Once the agent has seen it often
enough (100 times by default), it becomes **known**: part of your normal. You
can also mark a pattern known yourself.

![Before and after training: on the left, a raw stream of checkout-api log lines that differ only by order ID and timestamp; on the right, the Logs page rows the agent built from them, each pattern with its count, normal rate and a still learning or known verdict](../docs/images/sre-training-concept.svg)

## Example

Your `checkout-api` writes thousands of log lines an hour. On day one of
training, the agent sees a few dozen different shapes: declined cards,
retried payments, slow cart reads. Each new shape shows up as a new pattern.

After a day or two, almost every line matches a pattern the agent already has.
New patterns arrive only now and then, and most patterns have moved to
**known**. That flat line is your signal that training has done its job and you
can move to [Shadow Mode](./shadow-mode.md).

> **What gets learned.** By default the agent learns lines that contain `error`,
> plus a few named rules (out of memory, panics, HTTP 5xx). To learn every line,
> set `agent.regex.default_pattern: ".*"`. See [Regex](./regex.md).

## What you see in the UI

Open **Agent Learning → Logs**. The page lists every pattern the agent has
learned, grouped by service.

![Agent Learning → Logs during training: 19 log templates learned and grouped by service, each with its count, normal rate and a known verdict; the Known tab shows 19 and Still learning shows 0](../docs/images/quickstart-sre-patterns-dark.png)

| What you see | What it means |
|---|---|
| **Still learning** / **Known** tabs | How many patterns are still being learned and how many are part of normal. |
| **Count** | How many times the pattern has been seen. |
| **Normal** | The pattern's usual rate, for example `≈ 0.1/s`. |
| **Verdict** | `known` once the pattern is part of normal. |
| **Mark known** | Marks a pattern as normal right away, without waiting for the count. |

Open a pattern to see its full template, first and last seen times, and an
example line. Secrets in that example are already masked
([Redaction](./redaction.md)).

## Turn it on

Training is the default mode. Turn the agent on and point it at a log source:

```yaml
# config/config.yaml
agent:
  enable: true
  mode: training
```

```yaml
# config/agent_sources.yaml
sources:
  - name: my-app
    type: file
    enable: true
    file:
      path: /app/logs/my-app.log
      format: text
```

Or with environment variables: `AGENT_ENABLE=true` and `AGENT_MODE=training`.
Every source type is described in [Data Sources](./data-sources.md).

To see training end to end with demo logs, follow the
[AIOps SRE Quickstart](./quickstart.md).

## Tips

- **How long to train?** Until new patterns stop appearing often: a few days for
  a small service, a week or two for a large setup.
- **Too many patterns you don't care about?** Narrow what is learned with
  [Regex](./regex.md) rules. To start over, use **Clear all logs** on the
  **Logs** page and train again.
- **Related lines not merging into one pattern?** Tune grouping in
  [Miner](./miner.md).

## Reference

- [Configuration](./configuration.md): `agent.mode`, `agent.catalog.auto_promote_after`
  and every other setting.
- [Catalog](./catalog.md): how learned patterns are stored and labelled.
- Next step: [Shadow Mode](./shadow-mode.md).
