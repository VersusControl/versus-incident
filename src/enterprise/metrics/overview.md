# Metrics

_Enterprise_

Versus watches your metrics the same way it watches your logs: it finds the signals worth
watching for each service, learns what "normal" looks like, and pages you only when
something is genuinely wrong and stays wrong. You don't write a single query or set a
single threshold.

There are **two metric sources**, and they share one brain — the same discovery, the same
seasonal baselines, the same **learn → double-check → page** lifecycle:

- **[Prometheus](./prometheus.md)** — point Versus at a Prometheus address and it auto-writes
  the PromQL for each service's golden signals.
- **[CloudWatch Metrics](./cloudwatch-metrics.md)** — point Versus at an AWS region and it
  auto-discovers your CloudWatch metrics.

Both are **Enterprise**. Pick the source that matches where your metrics live (or run both);
the pages above cover the setup, options, and cost controls for each.

## License gate

A standing metric source and its learned baselines require a Versus Enterprise license,
supplied via the `LICENSE_KEY` environment variable. On an **OSS build**, a source with
`type: prometheus` or `type: cloudwatch_metrics` returns **"requires Versus Enterprise"**
and refuses to build.

## OSS vs Enterprise

| Capability | OSS | Enterprise |
|---|---|---|
| On-demand metric correlation during an investigation | ✅ | ✅ |
| A standing metric source that **starts incidents itself** | ❌ | ✅ |
| **Auto-discovered** signals (no PromQL, no metric names) | ❌ | ✅ |
| **Learned** seasonal baseline + sustained-deviation paging | ❌ | ✅ |

The **standing, auto-learned** metric source is the Enterprise wedge — OSS keeps the
on-demand correlation tools, Enterprise adds the source that pages on its own.

## See also

- [Prometheus / Metrics (Enterprise)](./prometheus.md)
- [CloudWatch Metrics (Enterprise)](./cloudwatch-metrics.md)
- OSS on-demand correlation tools: [Analyze Tools](../analyze-tools/tools.md)
