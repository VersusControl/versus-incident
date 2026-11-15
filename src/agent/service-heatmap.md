# Service Heatmap

## Concept

The **Service Heatmap** shows every service's recent health on one screen. It
uses what the agent already collects: log events, new and spiking patterns, and
open incidents for each service. It does not start any extra collection.

Each service gets a tile with its state, its recent log events and its active
incidents:

| State | When |
|---|---|
| **Degraded** | The service has an active incident. |
| **Warning** | No incident, but recent logs show a new pattern, a spike, or error or warning lines. |
| **Normal** | Nothing unusual in the window. |

Services that need attention stand out, so you can see where to look first.

In OSS the heatmap is built from logs and incidents, and that view is complete
on its own. _Enterprise_ adds latency, error rate, throughput and regression
evidence from connected metric and trace sources.

## Example

A release goes out at 14:00. At 14:06 you open the heatmap: most tiles are
**Normal**, but `checkout-api` shows **Degraded** with one active incident, and
`jobrunner` shows **Warning** with no incident yet. You open the `checkout-api`
tile. The **Logs** view shows the new pattern behind the incident. Then you
check `jobrunner`: a known error is spiking. You know which services to look
at and what changed, before reading a single log line.

## What you see in the UI

Open **Agent Learning → Overview**. The Service Heatmap sits at the top of the
page. On a phone, select the **Services** tab.

![Agent Learning → Overview with the Service Heatmap: 12 services, 2 need attention; checkout-api is Degraded with about 130 log events and 1 active incident, jobrunner is Warning with about 472 log events and no incident, the other services are Normal; above the tiles are the search box, the services and data filters, and grid and list views](../docs/images/sre-heatmap-ui-dark.png)

| Control | What it does |
|---|---|
| **Search services** | Find a service by name. |
| State filter | Show only the services that need attention, or all of them. |
| Data filter | Switch between all data, logs, incidents and any _Enterprise_ measures available to you. |
| **Grid** / **List** | Change the layout without changing the result. |

Select a service to open its details. **Overview** sums up impact, logs and
incidents. **Logs** shows estimated event and pattern counts, new or
unrecognized patterns, spikes, and the latest log time. Counts marked with `~`
are estimates for the current window, not lifetime totals.

A missing measure is shown as missing, never as a healthy zero.

### _Enterprise_ measures

With Versus Enterprise and a connected metric or trace source, the details also
show:

| Measure | What it shows |
|---|---|
| **Latency P99** | The learned 99th-percentile latency in milliseconds. |
| **Request error rate** | The latest learned request error ratio. |
| **Throughput** | The latest learned request rate per second. |
| **Regression** | Whether latency or error rate has moved against its learned normal. |

![Agent Learning → Overview with the Frequent log patterns block above the Service Heatmap, and the details panel open for the checkout service: Critical, latency P99 284.5 ms, regression assessment 82/100 driven by Latency P99 (POST /checkout), confidence 91%](../docs/images/service-health-enterprise-detail-dark.png)

## Turn it on

The heatmap is always on while the agent runs. It needs at least one log source;
see [Data Sources](./data-sources.md).

```yaml
agent:
  enable: true
```

To change how often it refreshes and how much recent data it looks at, open
**Settings → General → Service health**. Changes apply on the next refresh, with
no restart.

| Setting | Default | Allowed |
|---|---:|---:|
| **Service Health Interval** | 60 seconds | 30–900 seconds |
| **Service Health Window** | 300 seconds | 60–3600 seconds |

The window must be at least as long as the interval.

## Troubleshooting

| You see | What to do |
|---|---|
| **Not connected** | Connect a log source in [Data Sources](./data-sources.md). |
| **No data** | Check the source has traffic and the service name is detected correctly ([Service Detection](./service-detection.md)); widen the window if traffic is sparse. |
| **Stale** | Check the source connection and refresh. |
| A measure is missing from the data filter | Enterprise measures need a license and a connected metric or trace source. |

## Reference

- [Service Detection](./service-detection.md): how lines get a service name.
- [Spike Detection](./spike.md): what counts as a spike.
- [Configuration](./configuration.md): agent settings.
