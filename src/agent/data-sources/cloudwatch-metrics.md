# CloudWatch Metrics

_Enterprise_

## Concept

Point Versus at an AWS region and it finds the metrics worth watching on its
own: every AWS service metric it can tie to a resource, such as an RDS
instance, an EC2 instance or a load balancer. It learns each metric's normal by
time of week, and raises an incident only when a metric moves far outside its
normal **and stays there**. You name no metrics and create no CloudWatch
alarms.

![An AWS region feeds Versus Enterprise, which connects with an AWS role read only, discovers metrics tied to a resource, learns each metric's normal by time of week and detects a metric that stays far outside normal, keeps learning as traffic changes, and in detect mode opens an incident sent to Slack, Telegram or PagerDuty](../../docs/images/cloudwatch-metrics.svg)

Metrics move through the same modes as logs: [training](../training-mode.md)
learns and never alerts, [shadow](../shadow-mode.md) records what it would have
raised, and [detect](../ai-detect-mode.md) raises incidents.

## Minimal config

```yaml
# config/agent_sources.yaml
sources:
  - name: prod-cloudwatch
    type: cloudwatch_metrics
    enable: true
    options:
      region: us-east-1
```

There are no keys in the file. Versus uses the standard AWS credential chain:
`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` / `AWS_SESSION_TOKEN`, then
`~/.aws/credentials` (`AWS_PROFILE`), then IRSA on EKS, then the ECS task role,
then the EC2 instance profile.

## Options

| Key | Default | What it does |
|---|---|---|
| `region` | required | AWS region to read. |
| `namespaces` | `["AWS/*"]` | Which namespaces to watch. `AWS/*` means every AWS service. Custom namespaces are watched only when listed. |
| `default_statistic` | `Average` | Statistic read for every metric. |
| `statistic_overrides` | none | Statistic per namespace, for example `AWS/SQS: Sum` or `AWS/ApplicationELB: p99`. |
| `period` | `60` | Sample period, in seconds. |
| `query_delay` | `120s` | How far behind now Versus reads, because CloudWatch data arrives late. |
| `max_services` | `200` | Most resources Versus watches. |
| `max_signals` | `2000` | Most metrics Versus watches in total. |
| `discovery_interval` | `1h` | How often Versus looks for new metrics. |

A narrower source for a few services:

```yaml
    options:
      region: us-east-1
      namespaces:
        - AWS/ApplicationELB
        - AWS/RDS
        - AWS/SQS
      statistic_overrides:
        AWS/ApplicationELB: p99    # latency: watch the slow tail
        AWS/SQS: Sum               # queue depth: the total
```

### What it watches

| On the incident | Comes from |
|---|---|
| Service | The metric's main dimension, for example `DBInstanceIdentifier` for RDS, `InstanceId` for EC2, `LoadBalancer` for an ALB. |
| Signal | The metric name and statistic, for example `CPUUtilization:Average`. |

Metrics with no dimensions are skipped, because they belong to no resource.

## What you see in the UI

**Agent Learning → Metrics** lists every metric Versus watches, by service,
with its source and unit. The **Ready** tab shows metrics whose normal is
learned; **Still learning** shows the rest. Incidents appear on the
**Incidents** page and name the source they came from.

## Permissions

The identity Versus runs as needs two read-only actions:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["cloudwatch:ListMetrics", "cloudwatch:GetMetricData"],
      "Resource": "*"
    }
  ]
}
```

CloudWatch does not allow narrowing these actions to a resource, so `Resource`
is `*`. No write or alarm permissions are needed. The source needs the
Enterprise image and a license key with the `intelligence` entitlement.

**Cost.** `GetMetricData` is billed per metric read, so cost grows with the
number of watched metrics. Narrow `namespaces` or lower `max_signals` on a
large account.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `requires Versus Enterprise` | OSS image, or a license without the `intelligence` entitlement. |
| `AccessDenied` on `ListMetrics` or `GetMetricData` | The policy above is missing. |
| Nothing under Metrics | Wrong `region`, or `namespaces` lists services you do not use. |
| A custom namespace is ignored | Custom namespaces must be listed by name in `namespaces`. |
| Values look flat or late | Raise `query_delay` if your metrics arrive later than two minutes. |

## Run it against your AWS account

There is no local CloudWatch, so this source has no Docker Compose example.
Run Versus Enterprise anywhere that can reach AWS, with the source above in
`config/agent_sources.yaml` and these in the environment:

```bash
export LICENSE_KEY=<your license key>
export AWS_ACCESS_KEY_ID=...
export AWS_SECRET_ACCESS_KEY=...
```

On AWS, prefer a role (ECS task role, EC2 instance profile or IRSA) over keys.
Real metrics need time: let Versus learn in training for a few days before you
switch to detect.

For the full walkthrough, see
[CloudWatch Metrics Demo](../../enterprise/metrics/cloudwatch-metrics.md).
