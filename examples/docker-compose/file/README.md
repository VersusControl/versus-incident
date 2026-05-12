# File-source example

The smallest possible Versus + AI-agent stack: just `versus` and
`redis`. The agent tails `./logs/app.log` and catalogs whatever you
append to it.

## Run

```bash
cp .env.example .env
# edit .env if you want to enable Slack/Telegram or change the secret
docker compose up -d
```

## Verify

```bash
curl http://localhost:3000/healthz                                 # ok
SECRET=$(grep GATEWAY_SECRET .env | cut -d= -f2)
curl -H "X-Gateway-Secret: $SECRET" http://localhost:3000/api/agent/patterns | jq
```

## Test

Append a line — the agent picks it up within `poll_interval` (15s):

```bash
echo "$(date -u +%FT%TZ) ERROR [api] service=payments db connection refused" \
  >> logs/app.log
```

## Layout

```
file/
├── docker-compose.yml
├── .env.example
├── config/
│   ├── config.yaml
│   └── agent_sources.yaml
└── logs/
    └── app.log               # mounted at /var/log/sample/app.log
```

## Cleanup

```bash
docker compose down -v
```

## Reference

[File source docs](https://versuscontrol.github.io/versus-incident/agent/data-sources/file.html)
