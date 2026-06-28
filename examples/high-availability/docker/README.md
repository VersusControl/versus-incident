# HA on Docker Compose

See HA on your laptop in ~2 minutes: **two enterprise replicas + one Postgres +
an nginx load balancer**. Hit one URL and watch sessions/identity work across
both replicas.

```
            ┌───────────── http://localhost:8080 ─────────────┐
            │                  nginx (lb)                      │
            │            round-robin, no stickiness            │
            └───────┬─────────────────────────────┬───────────┘
                    │                              │
              ┌─────▼─────┐                  ┌─────▼─────┐
              │ versus-0  │  POD_NAME=versus-0│ versus-1  │ POD_NAME=versus-1
              │ index 0/2 │                  │ index 1/2 │
              └─────┬─────┘                  └─────┬─────┘
                    └───────────┬──────────────────┘
                          ┌─────▼─────┐
                          │ postgres  │  ONE shared store
                          └───────────┘  (secrets + sessions converge here)
```

## Prerequisites

- Docker + Docker Compose v2.
- The enterprise image `ghcr.io/versuscontrol/versus-enterprise:dev` (private —
  `docker login ghcr.io` first, or set `VERSUS_ENTERPRISE_IMAGE` to a local build
  in `.env`).

## Run it

```bash
cd versus-incident/examples/high-availability/docker

# 1. Provide your license (NEVER commit .env).
cp .env.example .env
#   edit .env: paste LICENSE_KEY (optional — empty boots community mode).
#   The data plane / admin UI is authenticated by a live session (the built-in
#   default admin or SSO) — there is no gateway secret to set.

# 2. Bring up Postgres + both replicas + the load balancer.
docker compose up -d

# 3. Wait for health, then hit the single endpoint.
docker compose ps
curl -fsS http://localhost:8080/healthz && echo OK
```

## Prove HA

**Each replica claims a distinct instance index** (from its POD_NAME ordinal):

```bash
docker compose logs versus-0 versus-1 | grep -E "HA mode|instance [0-9]+ of"
# versus-0 -> "instance 0 of 2", versus-1 -> "instance 1 of 2"
```

**Both replicas share ONE Postgres, secrets converge** (licensed): the
break-glass default admin password is generated ONCE by the winning replica and
printed once — search both:

```bash
docker compose logs versus-0 versus-1 | grep -A4 "DEFAULT ADMIN"
```

**Sessions work across replicas through the load balancer.** Log in once at
http://localhost:8080 (local admin `admin` + the printed password). nginx
round-robins your subsequent requests across versus-0 and versus-1, and they all
succeed — the session cookie minted on one replica validates on the other,
because the session key converges through Postgres. (The dev-only
`VERSUS_ENTERPRISE_COOKIE_INSECURE=1` is set so the cookie survives plain HTTP.)

**Rolling restart stays up.** Restart one replica and the LB keeps serving from
the other:

```bash
docker compose restart versus-0
curl -fsS http://localhost:8080/healthz && echo "still OK (served by versus-1)"
```

## Tear down

```bash
docker compose down            # keep data
docker compose down -v         # also drop the Postgres volume
```

## Notes (for the docs)

- `LICENSE_KEY` lives ONLY in the gitignored `.env`; it is injected as an env var
  and is an offline JWT (never phoned home).
- Both replicas use the SAME `POSTGRES_DSN` — that single shared store is what
  makes `>1` replica legal (the binary refuses `file` + `INSTANCE_COUNT>1`) and
  is where the session key / AI master key / admin-token hash converge.
- `POD_NAME` is set explicitly here (`versus-0`, `versus-1`); on Kubernetes the
  StatefulSet supplies the same ordinal via the Downward API.
- This example is intentionally Postgres-only (on-call + agent are off). To
  exercise signal-source partitioning, add a metrics backend + shared Redis as in
  the `metrics-source` example and enable the agent on both replicas.
