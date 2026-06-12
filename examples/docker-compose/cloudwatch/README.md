# CloudWatch-Logs-source example

Versus + Redis only — CloudWatch Logs lives in AWS, so there is no
local backend to spin up. You bring AWS credentials and a log-group
name; the agent uses `FilterLogEvents` to pull events.

## Prerequisites

- An AWS account and a CloudWatch log group with some recent events.
- IAM user / role with at least:

  ```json
  {
    "Effect": "Allow",
    "Action": ["logs:FilterLogEvents", "logs:DescribeLogStreams"],
    "Resource": "arn:aws:logs:us-east-1:123456789012:log-group:/aws/lambda/my-function:*"
  }
  ```

## Run

CloudWatch lives in AWS, so a few values can't sensibly default —
export at minimum `CW_LOG_GROUP_NAME` and AWS credentials in your
shell before running `docker compose up`:

```bash
# AWS credentials (or use ~/.aws — see below)
export AWS_REGION=us-east-1
export AWS_ACCESS_KEY_ID=...
export AWS_SECRET_ACCESS_KEY=...
# export AWS_SESSION_TOKEN=...           # only when using STS / SSO

# CloudWatch source
export CW_REGION=us-east-1
export CW_LOG_GROUP_NAME=/aws/lambda/my-function
# export CW_LOG_STREAM_PREFIX=...        # optional, narrows scan
# export CW_FILTER_PATTERN='?ERROR ?Exception'   # optional but recommended

docker compose up -d
```

### Using `~/.aws/credentials` instead

Open `docker-compose.yml`, uncomment the `~/.aws` bind in
`versus.volumes`, and leave `AWS_ACCESS_KEY_ID` /
`AWS_SECRET_ACCESS_KEY` unset. The SDK falls back to the shared
credentials file automatically.

## Generate test traffic

Push synthetic logs directly into your CloudWatch log group (run
from the repo root). Requires `boto3` and AWS credentials with
`logs:PutLogEvents` on the target group:

```bash
pip install boto3        # one-time

# 500 mixed lines once into the configured log group:
python3 scripts/generate_noisy_logs.py --target cloudwatch \
  --cw-log-group "$CW_LOG_GROUP_NAME" \
  --cw-region "$CW_REGION" --lines 500

# Continuous — 20 lines every 5s, Ctrl+C to stop:
TARGET=cloudwatch scripts/run_noisy_logs.sh

# Spike / scenario flags work identically:
TARGET=cloudwatch scripts/run_noisy_logs.sh --spike db-conn-refused
TARGET=cloudwatch scripts/run_noisy_logs.sh --scenario db-outage
```

The log group **must already exist**; the log stream
(`noisy-app` by default, override via `CW_LOG_STREAM`) is
created automatically.

## Verify

```bash
SECRET=${GATEWAY_SECRET:-change-me}
curl -H "X-Gateway-Secret: $SECRET" http://localhost:3000/api/agent/status | jq
docker compose logs versus | grep cwlogs
```

## Layout

```
cloudwatch/
├── docker-compose.yml
└── config/
    ├── config.yaml
    └── agent_sources.yaml
```

## Cleanup

```bash
docker compose down -v
```

## Reference

[CloudWatch Logs source docs](https://docs.versusincident.com/#/agent/data-sources/cloudwatch-logs)
