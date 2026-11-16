# AWS quickstart

Versus reads one AWS account, its resources and its cost, and shows them on
the AWS and FinOps pages. Storage is file mode, so Docker is all you run.

You need Docker with Docker Compose v2, the AWS CLI, and an access key for an
IAM user with the quickstart policy
([`scripts/aws/versus-quickstart-policy.json`](../../../scripts/aws/versus-quickstart-policy.json)).
The [AWS quickstart guide](../../../src/agent/aws/quickstart.md) creates that
user step by step.

## 1. Export four variables

```bash
export AWS_ACCESS_KEY_ID=AKIA...
export AWS_SECRET_ACCESS_KEY=...
export AWS_REGION=us-east-1
export AWS_ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
```

`config/connectors.yaml` reads the account and region from these, so you never
edit it. Compose stops with a message naming the variable if one is missing.

## 2. Start Versus

From this folder, `examples/quickstart/aws`:

```bash
docker compose up -d
```

## 3. Open the UI

Open <http://localhost:3000> and enter the gateway secret `change-me`. The
account shows as Preparing, then the AWS pages and FinOps fill in. Cost
Explorer must be on in the account; new cost data can take up to 24 hours.

## Settings

| Variable | Default | What it does |
|---|---|---|
| `VERSUS_IMAGE` | `ghcr.io/versuscontrol/versus-incident:latest` | Versus image |
| `GATEWAY_SECRET` | `change-me` | Secret for the UI and admin API |

## Clean up

```bash
docker compose down -v
```

Then delete the access key and the IAM user, as the guide shows.
