# File-source example

The smallest possible Versus + AI-agent stack: just `versus` and
`redis`. The agent tails `./logs/app.log` and catalogs whatever you
append to it.

## Run

```bash
docker compose up -d
```

Everything defaults to safe local values. To override (e.g. real
`GATEWAY_SECRET`, enable Slack/Telegram), export the variables in
your shell before running `docker compose up` — see
[../README.md](../README.md).

## Verify

```bash
curl http://localhost:3000/healthz                                 # ok
SECRET=${GATEWAY_SECRET:-change-me}
curl -H "X-Gateway-Secret: $SECRET" http://localhost:3000/api/agent/patterns | jq
```

## Generate test traffic

Either append a single line manually:

```bash
echo "$(date -u +%FT%TZ) ERROR [api] service=payments db connection refused" \
  >> logs/app.log
```

…or use the bundled generator to produce realistic noisy traffic
that the agent can cluster (run from the repo root):

```bash
# 500 mixed lines (INFO/WARN/ERROR) appended once:
python3 scripts/generate_noisy_logs.py --append \
  --output examples/docker-compose/file/logs/app.log --lines 500

# Continuous live tail — 20 lines every 5s, Ctrl+C to stop:
OUTPUT=examples/docker-compose/file/logs/app.log \
  scripts/run_noisy_logs.sh

# Inject a spike burst (test the spike detector):
OUTPUT=examples/docker-compose/file/logs/app.log \
  scripts/run_noisy_logs.sh --spike db-conn-refused --spike-burst 80

# Inject a curated incident cluster (test detect mode):
OUTPUT=examples/docker-compose/file/logs/app.log \
  scripts/run_noisy_logs.sh --scenario db-outage
```

The agent picks up appended lines within `poll_interval` (15s). See
[scripts/README.md](../../../scripts/README.md) for the full flag
list including `--list-templates` and `--list-scenarios`.

## Layout

```
file/
├── docker-compose.yml
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
