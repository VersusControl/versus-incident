# HA on Docker Compose

_Enterprise_

See high availability work on your laptop in about two minutes: **two enterprise
replicas + one Postgres + an nginx load balancer**. You hit one URL, nginx
round-robins across both replicas, and a session established on one is served by
the other — because sessions and secrets converge through the shared Postgres.

```text
            ┌───────────── http://localhost:8080 ─────────────┐
            │                  nginx (lb)                      │
            │            round-robin, no stickiness            │
            └───────┬─────────────────────────────┬───────────┘
                    │                              │
              ┌─────▼─────┐                  ┌─────▼─────┐
              │ versus-0  │                  │ versus-1  │
              │ index 0/2 │                  │ index 1/2 │
              └─────┬─────┘                  └─────┬─────┘
                    └───────────┬──────────────────┘
                          ┌─────▼─────┐
                          │ postgres  │  ONE shared store
                          └───────────┘  (secrets + sessions converge here)
```

> New to enterprise HA? Read the [overview](./overview.md) first for the runtime
> contract (shared Postgres, instance identity, secret convergence).

## What you'll need

| Item | Where it comes from |
|---|---|
| **Docker** + Docker Compose v2 | runs Postgres, both replicas, and the load balancer |
| The **enterprise image** | `ghcr.io/versuscontrol/versus-enterprise:dev` (private — `docker login ghcr.io` first, or set `VERSUS_ENTERPRISE_IMAGE` to a local build in `.env`) |
| A **Versus Enterprise license** (`LICENSE_KEY`) | optional for the topology demo (empty boots community mode); required to prove session/secret convergence |

The example lives at `versus-incident/examples/high-availability/docker/`.

## Step 1 — Provide your license

The compose file reads secrets from a **gitignored `.env`** — never commit a real
key. Copy the placeholder and fill it in:

```bash
cd versus-incident/examples/high-availability/docker
cp .env.example .env
```

Edit `.env`:

```bash
# Offline JWT — paste your key, or leave empty to boot community mode.
LICENSE_KEY=
```

The Postgres values (`POSTGRES_USER` / `POSTGRES_PASSWORD` / `POSTGRES_DB`) are
local dev defaults and already line up with the shared DSN — leave them as is for
the demo.

## Step 2 — Bring up the stack

```bash
docker compose up -d
docker compose ps
```

Compose starts one Postgres, the two replicas (`versus-0`, `versus-1`), and the
nginx load balancer. Each replica is wired for HA out of the box:

| Env (set in the compose file) | Value | Why |
|---|---|---|
| `STORAGE_TYPE` | `postgres` | the ONE shared store (required for >1 replica) |
| `POSTGRES_DSN` | same on both replicas | points both at the same Postgres |
| `POD_NAME` | `versus-0` / `versus-1` | the trailing ordinal is this replica's index |
| `INSTANCE_COUNT` | `2` | total replicas |
| `LICENSE_KEY` | from `.env` | enterprise features (empty ⇒ community mode) |

## Step 3 — What success looks like

**The single endpoint is healthy:**

```bash
curl -fsS http://localhost:8080/healthz && echo OK
```

**Each replica claims a distinct instance index** (from its `POD_NAME` ordinal):

```bash
docker compose logs versus-0 versus-1 | grep -E "HA mode|instance [0-9]+ of"
# versus-0 -> "instance 0 of 2"
# versus-1 -> "instance 1 of 2"
```

**Secrets converge** (licensed): the break-glass default-admin password is
generated **once** by the winning replica and printed once — search both:

```bash
docker compose logs versus-0 versus-1 | grep -A3 "DEFAULT ADMIN CREDENTIALS"
```

**Sessions work across replicas.** Log in once at <http://localhost:8080> as the
local admin `admin` with the printed password. nginx round-robins your later
requests across `versus-0` and `versus-1` and they all succeed — the session
cookie minted on one replica validates on the other, because the session key
converges through Postgres. (The compose file sets the dev-only
`VERSUS_ENTERPRISE_COOKIE_INSECURE=1` so the cookie survives plain HTTP through
the local load balancer — never set this in production.)

**A rolling restart stays up.** Restart one replica; the load balancer keeps
serving from the other:

```bash
docker compose restart versus-0
curl -fsS http://localhost:8080/healthz && echo "still OK (served by versus-1)"
```

## Step 4 — Tear down

```bash
docker compose down      # keep the Postgres volume
docker compose down -v   # also drop the Postgres volume
```

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| A replica exits immediately at boot | `STORAGE_TYPE` isn't `postgres` while `INSTANCE_COUNT > 1`. The file backend is single-node and the binary refuses it — both replicas must use the shared Postgres DSN. |
| `image pull` fails for the enterprise image | The enterprise image is private. Run `docker login ghcr.io`, or set `VERSUS_ENTERPRISE_IMAGE` in `.env` to a locally-built image. |
| Both replicas report the same index | `POD_NAME` isn't distinct per replica. The compose file sets `versus-0` / `versus-1`; if you copied a service, give it its own `POD_NAME`. |
| No `DEFAULT ADMIN CREDENTIALS` banner appears | You booted without a `LICENSE_KEY` (community mode). Paste a real key into `.env` and `docker compose up -d` again. |
| Login on one replica is rejected after a round-robin to the other | Running unlicensed (no converged session key), or you changed `POSTGRES_DSN` so the replicas no longer share one store. Confirm both use the same DSN and a license is set. |

## See also

- [High Availability overview](./overview.md)
- [HA on Kubernetes](./kubernetes.md) · [HA with Helm](./helm.md)
- [Getting Started](../getting-started.md) — the default-admin bootstrap flow.
