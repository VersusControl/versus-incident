# Versus Enterprise — High Availability (HA / multi-instance)

Run the Versus Enterprise binary as **multiple replicas behind one endpoint**,
all sharing **one Postgres**. This is the deploy/topology layer for epic **X9**
(HA / multi-instance partitioning); the product feature already ships in the
binary.

Two self-contained, copy-pasteable walkthroughs:

| Dir | Use it to | Entrypoint |
|---|---|---|
| [`docker/`](docker/) | See HA on your laptop in ~2 min: 2 replicas + 1 Postgres + an nginx load balancer, hit one URL. | `docker compose up -d` |
| [`k8s/`](k8s/) | Apply raw Kubernetes manifests (StatefulSet, headless Service, PDB, NetworkPolicy) that mirror the Helm chart. | `kubectl apply -k .` |

For a **Helm** deployment, use the chart at
`versus-incident/helm/versus-incident` with its
[`values-ha.yaml`](../../helm/versus-incident/values-ha.yaml).

## The runtime contract (what makes HA work)

The binary expects exactly this — every artifact here satisfies it:

- **One shared Postgres.** `STORAGE_TYPE=postgres` is **required** with more than
  one replica — the binary refuses (`file` + `INSTANCE_COUNT>1` is fatal). All
  replicas point at the SAME `POSTGRES_DSN`.
- **Instance identity.** `INSTANCE_COUNT` = number of replicas. Each replica's
  index is the **trailing ordinal of `POD_NAME`** (e.g. `versus-0` → 0,
  `versus-1` → 1). On Kubernetes that ordinal comes from the StatefulSet via the
  Downward API; in Compose we set `POD_NAME` explicitly. The binary partitions
  signal sources and the SLO scheduler job by hash-ownership so replicas never
  double-page.
- **Enterprise + license.** HA is an enterprise capability: run the enterprise
  image with a `LICENSE_KEY`. Without a license the stack still boots (community
  mode) so you can see the topology, but the SSO / session-convergence proof
  needs the key.
- **Secrets converge via Postgres.** The session key, AI master key, and
  break-glass admin-token hash are generated **once** and shared across replicas
  through Postgres — so a cookie or admin token minted on one replica validates
  on all. No per-pod secret env is needed beyond `LICENSE_KEY` (+ optional BYOK
  `VERSUS_ENTERPRISE_SECRET_KEY`).

## Secret handling (read before you commit anything)

- **Never commit a real `LICENSE_KEY` or DSN.** Both examples keep them in a
  gitignored `.env` (Compose) or a gitignored `secret.yaml` (k8s), with a
  committed `.example` placeholder.
- The license is mounted as an **env var from a Secret** — it is an offline-
  verified JWT, never phoned home.
- The Postgres password in these examples is a **local dev default** (`versus`),
  clearly marked. For production, use a managed Postgres (e.g. RDS) and a real
  secret, and set `sslmode=require` in the DSN.
