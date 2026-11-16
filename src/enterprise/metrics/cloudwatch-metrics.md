# CloudWatch Metrics

_Enterprise_

## Concept

Give Versus an AWS region and it finds the CloudWatch metrics worth watching:
every AWS service metric it can tie to a resource, such as an EC2 instance, an
RDS database or a load balancer. It learns each metric's normal by time of
week and raises an incident when a metric moves far outside it and stays
there. You name no metrics and create no CloudWatch alarms.

![An AWS region feeds Versus Enterprise, which connects with an AWS role read only, discovers metrics tied to a resource, learns each metric's normal by time of week and detects a metric that stays far outside normal, keeps learning as traffic changes, and in detect mode opens an incident sent to Slack, Telegram or PagerDuty](../../docs/images/cloudwatch-metrics.svg)

## Example

Your account in `us-east-1` runs an RDS database, `orders-db`, and an
Application Load Balancer. Versus finds `orders-db`'s `CPUUtilization` and
`DatabaseConnections` and the load balancer's `TargetResponseTime`, and learns
each one's weekly rhythm.

On Tuesday afternoon a reporting job starts a full-table scan and `orders-db`'s
CPU sits at 95% instead of its usual 30% for that hour. Versus raises one
incident for `orders-db`, signal `CPUUtilization`. The usual Monday
morning batch peak raises nothing, because it is part of normal.

## What you see in the UI

| Where | What |
|---|---|
| **Agent Learning → Metrics** | Each resource as a service (an `InstanceId`, `DBInstanceIdentifier` or `LoadBalancer`) with its metrics. **Ready** once a metric's normal is learned, **Still learning** before. |
| **Incidents** | One incident per resource and metric that stays out of its normal, naming the source. |
| **AI → Decisions**, **Shadow** tab | What the source would have raised, in shadow mode. |

## Turn it on

The only required option is `region`:

```yaml
# config/agent_sources.yaml
sources:
  - name: prod-cloudwatch
    type: cloudwatch_metrics
    enable: true
    options:
      region: us-east-1
```

To watch only some AWS services, list their namespaces:

```yaml
    options:
      region: us-east-1
      namespaces:
        - AWS/ApplicationELB
        - AWS/RDS
      statistic_overrides:
        AWS/ApplicationELB: p99    # latency: watch the slow tail
```

Versus reads AWS credentials from the standard chain: environment variables,
`~/.aws/credentials`, IRSA on EKS, the ECS task role or the EC2 instance
profile. There are no keys in the file. The identity needs two read-only
actions:

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

Every option, its default and the cost controls are in the
[CloudWatch Metrics data source reference](../../agent/data-sources/cloudwatch-metrics.md).

## Run it against your AWS account

There is no local CloudWatch, so there is no Docker Compose example. Run Versus
Enterprise where it can reach AWS, against a region with resources that report
metrics.

**1. Save the source** above as `agent_sources.yaml` in an empty folder.

**2. Start Versus Enterprise in training** with your license key and AWS
credentials from the environment:

```bash
export LICENSE_KEY=<your license key>
export AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=...   # or use a role on AWS

docker run -d --name versus-enterprise \
  -p 127.0.0.1:3000:3000 \
  -e LICENSE_KEY -e AWS_ACCESS_KEY_ID -e AWS_SECRET_ACCESS_KEY \
  -e VERSUS_ENTERPRISE_COOKIE_INSECURE=true \
  -e AGENT_ENABLE=true -e AGENT_MODE=training \
  -v "$PWD/agent_sources.yaml:/app/config/agent_sources.yaml:ro" \
  -v versus-data:/app/data \
  ghcr.io/versuscontrol/versus-enterprise
```

`VERSUS_ENTERPRISE_COOKIE_INSECURE=true` lets the sign-in cookie work over
plain HTTP on `localhost`; leave it out when you serve Versus over HTTPS.
On AWS, prefer a role (ECS task role, EC2 instance profile or IRSA) over keys.

**3. Sign in.** Get the admin password, open `http://localhost:3000` and sign
in as `admin`:

```bash
docker logs versus-enterprise 2>&1 | grep -A6 "DEFAULT ADMIN CREDENTIALS"
```

**4. Watch it learn.** **Agent Learning → Metrics** fills with your resources.
CloudWatch data arrives a few minutes late, and real traffic needs time: let
Versus learn for a few days.

**5. Shadow, then detect.** In **Admin → Runtime mode**, switch to **Shadow**
and check **AI → Decisions** for a few days. When the results look right, add
an AI key in **Admin → AI provider** and arm **Detect**.

There is no fake spike for CloudWatch. To see an incident sooner, put real
load on a watched resource, such as CPU load on a test EC2 instance, and keep
it there for several minutes.

**Clean up:**

```bash
docker rm -f versus-enterprise
docker volume rm versus-data
```

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `requires Versus Enterprise` in the logs | No `LICENSE_KEY`, or the key lacks the `intelligence` entitlement. |
| `AccessDenied` on `ListMetrics` or `GetMetricData` | The policy above is missing from the identity Versus runs as. |
| Nothing under **Agent Learning → Metrics** | Wrong `region`, or `namespaces` lists services you do not use. |
| Metrics never reach **Ready** | They have no recent datapoints. Pick a region with live resources. |
| A custom namespace is ignored | Custom namespaces are watched only when listed in `namespaces`. |

## See also

- [CloudWatch Metrics data source reference](../../agent/data-sources/cloudwatch-metrics.md)
- [Metrics overview](./overview.md) · [Prometheus Metrics](./prometheus.md)
