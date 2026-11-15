# Shadow Mode

## Concept

Shadow mode is a **practice run** for detect. The agent keeps learning exactly
as in training, and on top of that it writes down every problem it *would* have
alerted on. It sends nothing: no Slack, no email, no on-call.

Think of it as: "show me what you would have alerted me about, so I can decide
whether to trust you."

The agent records two kinds of entries:

| Verdict | Meaning |
|---|---|
| `unknown` | A pattern the agent has never seen, or has not seen often enough to call normal. |
| `spike` | A known pattern firing far more than usual. See [Spike Detection](./spike.md). |

Known patterns at their usual rate stay quiet; a known pattern only comes back
if it spikes. Each source and pattern pair gets **one entry**, updated each
time it comes back, so a busy problem does not flood the list.

![Before and after shadow mode: on the left, the same log stream a detect agent would alert on, with a new checkout timeout and a burst of a known error; on the right, the Decisions → Shadow list with one would-have-alerted row per source and pattern, marked unknown and spike, and no notification sent](../docs/images/sre-shadow-concept.svg)

## Example

You trained the agent on `checkout-api` for three days, then switched to
shadow. On Friday a release starts throwing:

```text
checkout-api payment processor acme-pay did not answer within 30s
```

The agent has never seen this line. In detect mode it would raise an incident
and send it to your channels. In shadow mode it adds one `unknown` entry
instead, and the entry's signal count grows as the error repeats. Nothing is
sent.

On Monday you review the list:

- The `acme-pay` timeout is a real problem you want to hear about. Leave it:
  detect will raise an incident for it.
- A `cache warmed for region <*>` line from a new nightly job is harmless. Mark
  its pattern **known** on the **Logs** page. It stops showing up unless it
  spikes.

## What you see in the UI

Open **AI → Decisions** and select the **Shadow** tab.

![AI → Decisions, Shadow tab in shadow mode: two would-have-alerted rows from loki:quickstart-loki, checkout-api with verdict unknown and 60 signals for the acme-pay timeout, and jobrunner with verdict spike and 143 signals for the failed to enqueue job line, each with pattern, rule, ticks, sample line and last seen time, and a Clear button](../docs/images/sre-shadow-ui-dark.png)

| Column | What it shows |
|---|---|
| **Service** | The service the line came from. See [Service Detection](./service-detection.md). |
| **Verdict** | `unknown` for a new pattern, `spike` for a known pattern firing far above normal. |
| **Pattern** | The learned pattern. Open it to see the full template. |
| **Source** | The log source it came from, for example prod or staging. |
| **Rule** | The [Regex](./regex.md) rule that matched the line. |
| **Signals** / **Ticks** | Total matching lines, and how many separate reads they arrived in. |
| **Sample** | One example line, with secrets masked. |
| **Last seen** | When the pattern last appeared. |

Filter by verdict to look at spikes on their own. When a review round is done,
use **Clear** to start the next one with an empty list. The list keeps up to
1000 patterns; the oldest entry is dropped when it is full.

## Turn it on

Switch the mode and restart. What the agent learned in training is kept.

```yaml
agent:
  enable: true
  mode: shadow
```

Or set `AGENT_MODE=shadow`. _Enterprise_ admins can also switch from
**Admin → Runtime mode** in the UI.

> **New services are quiet for a while.** A service the agent sees for the first
> time is learned but not reported for `agent.new_service_grace` (`24h` in the
> shipped `config.yaml`, `30m` in the Helm chart). Shadow and detect use the same grace, so shadow is a faithful preview
> of detect.

## A review loop

1. Run shadow for about a day, so it covers peak hours and nightly jobs.
2. Sort each entry: a real problem, harmless noise, or a new but expected line.
3. Mark harmless patterns **known** on the **Logs** page.
4. For a real problem that matched only the catch-all rule, give it its own
   [Regex](./regex.md) rule so incidents carry a clear name.
5. **Clear** the list and repeat until it holds mostly real problems over a full
   release cycle.
6. Switch to [AI Detect Mode](./ai-detect-mode.md).

## Reference

- [Configuration](./configuration.md): `agent.mode`, `agent.new_service_grace`,
  `agent.catalog.auto_promote_after`.
- [Catalog](./catalog.md): labelling patterns as known.
- [Spike Detection](./spike.md): when a known pattern still shows up.
