# CloudWatch Logs

## Concept

The CloudWatch Logs source reads log events from one AWS CloudWatch Logs log
group: Lambda, ECS, EKS, EC2 or anything else that writes there. You give it a
region and a log group, and optionally a stream prefix and a CloudWatch filter
pattern to read only the events you care about.

## Minimal config

```yaml
# config/agent_sources.yaml
sources:
  - name: lambda-prod
    type: cloudwatchlogs
    enable: true
    cloudwatchlogs:
      region: us-east-1
      log_group_name: /aws/lambda/my-function
```

There are no keys in the file. Versus uses the standard AWS credential chain,
in this order:

1. `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` and `AWS_SESSION_TOKEN`.
2. `~/.aws/credentials`, with the profile from `AWS_PROFILE`.
3. IRSA (IAM Roles for Service Accounts) on EKS.
4. The ECS task role.
5. The EC2 instance profile.

A narrower source for a busy ECS service:

```yaml
    cloudwatchlogs:
      region: us-east-1
      log_group_name: /aws/ecs/api
      log_stream_prefix: "ecs/api/"
      filter_pattern: '?ERROR ?Exception ?panic'
```

| Key | Default | What it does |
|---|---|---|
| `region` | required | AWS region of the log group. |
| `log_group_name` | required | Exact log group name. Case-sensitive. |
| `log_stream_prefix` | none | Read only streams whose name starts with this. |
| `filter_pattern` | none | A [CloudWatch filter pattern](https://docs.aws.amazon.com/AmazonCloudWatch/latest/logs/FilterAndPatternSyntax.html). Not a regex. |
| `page_size` | `500` | Events read per poll, up to `10000`. |
| `reorder_window` | none | Extra time each poll looks back, for events that arrive late, for example `1m`. |

| Filter pattern | Matches |
|---|---|
| `ERROR` | events that contain `ERROR` |
| `?ERROR ?Exception` | events that contain `ERROR` or `Exception` |
| `"connection refused"` | the exact phrase |
| `{ $.level = "error" }` | JSON events whose `level` is `error` |

## What you see in the UI

Patterns learned from the log group appear in **Agent Learning → Logs**,
grouped by service. Incidents show `via cloudwatchlogs:<source name>`.

## Permissions

The identity Versus runs as needs `logs:FilterLogEvents` on the log group:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["logs:FilterLogEvents"],
      "Resource": "arn:aws:logs:us-east-1:123456789012:log-group:/aws/lambda/my-function:*"
    }
  ]
}
```

For several log groups, list each ARN, or use a wildcard such as
`arn:aws:logs:us-east-1:123456789012:log-group:/aws/lambda/*`.

**Cost.** CloudWatch bills `FilterLogEvents` by the data it scans. On a busy
log group, set `filter_pattern` or `log_stream_prefix`, and keep
`agent.poll_interval` at `30s` or more.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `AccessDeniedException` | The policy is missing `logs:FilterLogEvents` for this log group. |
| `ResourceNotFoundException` | Wrong `log_group_name` (case-sensitive) or wrong `region`. |
| `ThrottlingException` | Too many calls in the account and region. Lower `page_size` or raise `agent.poll_interval`. |
| No events, but the console shows them | `filter_pattern` is written as a regex. Use CloudWatch filter syntax. |
| `get identity: get credentials: failed to refresh cached credentials, no EC2 IMDS role found` | No credentials found in the chain above. |

## Run it against your AWS account

There is no local CloudWatch, so this source has no Docker Compose example.
Run Versus anywhere that can reach AWS, with the source above in
`config/agent_sources.yaml` and credentials in the environment:

```bash
export AWS_ACCESS_KEY_ID=...
export AWS_SECRET_ACCESS_KEY=...
```

On AWS, prefer a role (ECS task role, EC2 instance profile or IRSA) over keys.
Then open **Agent Learning → Logs** and watch patterns from the log group
appear.
