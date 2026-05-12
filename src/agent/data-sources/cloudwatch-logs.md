# CloudWatch Logs source

Pulls events from AWS CloudWatch Logs using the `FilterLogEvents` API.
Cheap, real-time, no async query lifecycle.

## Minimal config

```yaml
sources:
  - name: lambda-prod
    type: cloudwatchlogs
    enable: true
    cloudwatchlogs:
      region: us-east-1
      log_group_name: /aws/lambda/my-function
      page_size: 500
```

## Narrowed query

```yaml
cloudwatchlogs:
  region: us-east-1
  log_group_name: /aws/ecs/api
  log_stream_prefix: "ecs/api/"          # only recent task streams
  filter_pattern: '?ERROR ?Exception ?panic'
  page_size: 1000
```

## Full reference

```yaml
cloudwatchlogs:
  region: us-east-1                       # REQUIRED.
  log_group_name: /aws/lambda/my-fn       # REQUIRED. Exact log group name.
  log_stream_prefix: ""                   # restrict to streams starting with this string
  filter_pattern: ""                      # CloudWatch filter syntax — NOT regex
  page_size: 500                          # max 10000
```

## Authentication

The source uses the **standard AWS SDK credential chain** in this
order:

1. Environment variables: `AWS_ACCESS_KEY_ID`,
   `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`.
2. Shared credentials file: `~/.aws/credentials` (profile from
   `AWS_PROFILE`, default `"default"`).
3. ECS task role (when running in ECS / Fargate).
4. EC2 instance profile / EKS IRSA (IAM Roles for Service Accounts).

Required IAM policy:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["logs:FilterLogEvents", "logs:DescribeLogStreams"],
      "Resource": "arn:aws:logs:us-east-1:123456789012:log-group:/aws/lambda/my-function:*"
    }
  ]
}
```

For multiple log groups, list each `Resource` ARN — wildcards work
(`arn:aws:logs:us-east-1:123456789012:log-group:/aws/lambda/*`).

## Behavior

- **Cursor** — The maximum event timestamp seen on the previous
  tick. CloudWatch event timestamps are **milliseconds** since epoch.
  The next call uses `startTime = cursor + 1ms` to avoid re-reading
  the boundary event.
- **Pagination** — Walks `NextToken` up to **20 pages** OR until
  `page_size` events are collected, whichever happens first. Anything
  beyond that is read on the next tick.
- **Filter pattern syntax** — CloudWatch filter syntax, NOT regex:

  | Pattern | Matches |
  |---|---|
  | `ERROR` | events containing the word `ERROR` |
  | `?ERROR ?Exception` | events containing `ERROR` OR `Exception` |
  | `"connection refused"` | exact phrase |
  | `[time, level=ERROR, ...]` | space-delimited fields where `level == "ERROR"` |
  | `{$.level = "error"}` | JSON event with `level: "error"` |

  See the [AWS docs](https://docs.aws.amazon.com/AmazonCloudWatch/latest/logs/FilterAndPatternSyntax.html)
  for the full grammar.

## Cost notes

- `FilterLogEvents` is billed per GB scanned. **Always** set
  `filter_pattern` and/or `log_stream_prefix` on chatty groups.
- `page_size` controls the per-call cap; `agent.poll_interval`
  controls the call rate. A noisy Lambda group with `poll_interval:
  30s` makes ~120 calls/hour per source.
- Use `log_stream_prefix` for ECS/EKS where each task creates a new
  stream — you don't need to scan retired streams.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `AccessDeniedException` | IAM policy missing `logs:FilterLogEvents` for the group ARN. |
| `ResourceNotFoundException` | Wrong `log_group_name` (case-sensitive) or wrong `region`. |
| `ThrottlingException` | Lower `page_size` or raise `poll_interval`; CloudWatch quotas are per-account-per-region. |
| Empty results despite events in console | `filter_pattern` is regex-style instead of CloudWatch syntax. |

## Local testing without AWS

The integration tests in
[`pkg/signalsources/cloudwatchlogs_test.go`](https://github.com/VersusControl/versus-incident/blob/main/pkg/signalsources/cloudwatchlogs_test.go)
use `httptest` + the SDK's `BaseEndpoint` option to point at a local
mock — useful as a template if you want to wire your own integration
tests.
