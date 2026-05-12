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

```bash
cp .env.example .env
# edit .env:
#   - AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY (or use ~/.aws — see below)
#   - CW_REGION
#   - CW_LOG_GROUP_NAME
#   - CW_FILTER_PATTERN (optional, recommended for noisy groups)
docker compose up -d
```

### Using `~/.aws/credentials` instead

Open `docker-compose.yml`, uncomment the `~/.aws` bind in `versus.volumes`,
and unset `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` in `.env`.
The SDK falls back to the shared credentials file automatically.

## Verify

```bash
SECRET=$(grep GATEWAY_SECRET .env | cut -d= -f2)
curl -H "X-Gateway-Secret: $SECRET" http://localhost:3000/api/agent/status | jq
docker compose logs versus | grep cwlogs
```

## Layout

```
cloudwatch/
├── docker-compose.yml
├── .env.example
└── config/
    ├── config.yaml
    └── agent_sources.yaml
```

## Cleanup

```bash
docker compose down -v
```

## Reference

[CloudWatch Logs source docs](https://versuscontrol.github.io/versus-incident/agent/data-sources/cloudwatch-logs.html)
