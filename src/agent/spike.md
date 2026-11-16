# Spike Detection

## Concept

Some errors are normal in small numbers. A few declined cards an hour is just
business. Fifty a second is an outage. Once a pattern is **known**, the agent
stops reporting it, so spike detection answers the question the known check
cannot: **"This error is normal, but why is it happening so much more than
usual?"**

For every pattern, the agent learns two things while it runs:

- its **normal rate**, for example `1.5/s`;
- how much that rate usually **wobbles**, for example `± 0.3/s`.

When the current rate jumps far above normal, measured in those wobbles, the
pattern is flagged as a **spike**, even though it is known. Because the bar is
measured against each pattern's own wobble, it works the same for a quiet
pattern and a busy one.

![Before and after spike detection: on the left, a known declined-card pattern ticking along at about 1.5 per second and then jumping to 6 per second, which the known check alone would ignore; on the right, the Decisions → Spike row for that pattern, and the Detect tab side panel showing the reason 6.0/s = 15.0σ above 1.5/s ± 0.3](../docs/images/sre-spike-concept.svg)

A few guards keep spikes meaningful:

- a spike needs at least a few matching lines in one read, so two stray lines
  never page;
- a pattern must have been seen enough times before its normal is trusted;
- one spike does not raise the normal, so the next spike is still caught;
- the agent also keeps a separate normal for each hour of the day, so a nightly
  batch job can be normal at 2 a.m. and still raise a spike at 2 p.m.

## Example

`checkout-api` logs `payment declined for order <*> (card_expired)` at about
`1.5/s`, give or take `0.3/s`. The pattern has been known for weeks.

At 14:05 a bad deploy breaks card validation and the rate jumps to `6.0/s`.
That is `(6.0 − 1.5) / 0.3 = 15` wobbles above normal, far past the default bar
of 3. The agent flags a spike.

- In **shadow** mode, the spike is recorded as a would-have-alerted entry.
- In **detect** mode, it becomes an incident, sent through your channels, and
  the decision records the agent's reasoning:

  ```text
  6.0/s = 15.0σ above 1.5/s ± 0.3
  ```

## What you see in the UI

Open **AI → Decisions** and select the **Spike** tab. It lists spikes from both
modes; the **Kind** column says whether a row came from detect (`AI-detect`) or
from shadow (`Shadow`).

To see why a detect spike fired, open it on the **Detect** tab: the side panel
shows the reasoning, such as `6.0/s = 15.0σ above 1.5/s ± 0.3`. Shadow entries
do not store the reasoning.

![AI → Decisions, Spike tab: two jobrunner spike rows for the same pattern from loki:quickstart-loki, one of kind AI-detect with 277 signals and the sample jobrunner: error rate spike, and one of kind Shadow with 303 signals and a failed to enqueue job sample line, each with the time it was seen](../docs/images/sre-spike-ui-dark.png)

Open a pattern from **Agent Learning → Logs** to see **What's normal**: its
normal rate and its wobble, its all-time average, and a grid of 24 hour cells with the normal rate learned for each hour (UTC). An hour reads `—` until the agent has data for it, so the grid fills in over the first day.

![The detail page of the jobrunner failed to enqueue job pattern, What's normal section: default global baseline about 0.1/s ± 0.0/s, cumulative average about 0.1/s, and a 6 by 4 grid of hour cells from 00h to 23h where only 05h shows a rate (0.1) and every other hour reads a dash because the agent has run for less than a day](../docs/images/sre-spike-normal-ui-dark.png)

## Turn it on

Spike detection is on by default in shadow and detect mode. The defaults suit
most setups:

```yaml
agent:
  catalog:
    spike_z: 3.0                 # how many wobbles above normal counts as a spike
    spike_min_frequency: 5       # lines needed in one read before a spike can fire
    spike_min_baseline_count: 20 # sightings needed before the normal is trusted
    spike_sustain_ticks: 1       # reads in a row that must spike (1 = fire at once)
    spike_abs_ceiling: 0         # always fire at this many lines in one read (0 = off)
```

| Setting | Raise it when | Lower it when |
|---|---|---|
| `spike_z` | You get too many spike alerts. | You miss real surges. |
| `spike_min_frequency` | Quiet patterns spike on small bursts. | Low-traffic services never spike. |
| `spike_min_baseline_count` | New patterns spike before their normal settles. | A low-traffic service needs spikes sooner. |
| `spike_sustain_ticks` | One-off bursts raise alerts. | You want the first spiking read to fire. |

`spike_abs_ceiling` is a safety net: set it, for example to `1000`, to always
fire when one read sees that many lines, even for a pattern still warming up.

### Choose which normal to compare against

Under **Settings → Agent → Spike baseline**, choose the default baseline:

| Option | Compares the current rate against |
|---|---|
| **Default (global baseline)** | The pattern's overall normal rate. |
| **Average (cumulative mean)** | A plain running average of the rate. |
| **Time of day (hour-of-day)** | The normal for the current hour (UTC). An hour without enough history falls back to the global baseline. |

All three are always learned, so a change takes effect at once, with no
re-learning and no restart.

## Reference

- [Configuration](./configuration.md): every `agent.catalog` key.
- [Shadow Mode](./shadow-mode.md) and [AI Detect Mode](./ai-detect-mode.md): where
  spikes appear.
- [Catalog](./catalog.md): the learned normal stored for each pattern.
