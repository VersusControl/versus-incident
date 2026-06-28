# HA on Kubernetes (raw manifests)

Self-contained Kubernetes manifests that run **Versus Enterprise as 3 replicas
sharing one Postgres** — the same topology the Helm chart produces, but plain
YAML you can read top-to-bottom. Applied with `kubectl apply -k .` (kustomize).

## What gets created (namespace `versus-ha`)

| File | Resource | Why |
|---|---|---|
| `00-namespace.yaml` | Namespace | isolation |
| `10-postgres.yaml` | Postgres StatefulSet + headless Service | the ONE shared store (swap for managed Postgres in prod) |
| `20-versus-config.yaml` | ConfigMap | `config.yaml` (storage=postgres, on-call/agent off) |
| `30-versus-statefulset.yaml` | **Versus StatefulSet (3 replicas)** | `POD_NAME` Downward API → instance index, `INSTANCE_COUNT=3`, enterprise image, probes, hardened securityContext, anti-affinity |
| `40-versus-service.yaml` | ClusterIP + headless Service | the load-balanced entrypoint + stable per-pod DNS |
| `50-poddisruptionbudget.yaml` | PodDisruptionBudget | keep the LB backed during node drains |
| `60-networkpolicy.yaml` | NetworkPolicy ×2 | default-deny ingress; Postgres reachable |
| `kustomization.yaml` | secretGenerator | builds `versus-secrets` from a gitignored `.env` |

## Prerequisites

- A cluster (kind / minikube / any). `kubectl` + kustomize (built into
  `kubectl` ≥ 1.14, i.e. `kubectl apply -k`).
- The enterprise image `ghcr.io/versuscontrol/versus-enterprise` pullable by
  the cluster (it is private — `imagePullSecrets` or a local load may be needed;
  on kind: `kind load docker-image ghcr.io/versuscontrol/versus-enterprise`).

## Run it

```bash
cd versus-incident/examples/high-availability/k8s

# 1. Provide the license + secrets (NEVER commit .env).
cp .env.example .env
#   edit .env: paste LICENSE_KEY. The data plane / admin UI is authenticated by
#   a live session (the built-in default admin or SSO) — no gateway secret.
#   The dev Postgres password/DSN already line up out of the box.

# 2. Apply everything (Namespace, Postgres, Secret, StatefulSet, Services, PDB, NetworkPolicy).
kubectl apply -k .

# 3. Wait for the 3 replicas to be ready.
kubectl -n versus-ha rollout status statefulset/versus
kubectl -n versus-ha get pods -l app.kubernetes.io/name=versus
#   versus-0, versus-1, versus-2 all Running/Ready.
```

## Prove HA

**Each replica claims a distinct instance index** (from its POD_NAME ordinal):

```bash
for i in 0 1 2; do
  echo "== versus-$i =="
  kubectl -n versus-ha logs versus-$i | grep -E "HA mode|instance [0-9]+ of"
done
# versus-0 -> "instance 0 of 3", versus-1 -> "instance 1 of 3", versus-2 -> "instance 2 of 3"
```

**One shared Postgres, secrets converge** (licensed): the break-glass default
admin password is generated ONCE by the winning replica and printed once —
search all three:

```bash
kubectl -n versus-ha logs -l app.kubernetes.io/name=versus --tail=-1 \
  | grep -A4 "DEFAULT ADMIN"
```

**Hit the load-balanced endpoint** (round-robins across replicas):

```bash
kubectl -n versus-ha port-forward svc/versus 3000:3000
# in another shell:
curl -fsS http://localhost:3000/healthz && echo OK
```

A session cookie obtained from one replica is accepted by any other, because the
session key, AI master key, and admin-token hash converge through Postgres.

## Graceful rollout

```bash
# Trigger a rolling update (e.g. after changing the image tag) and watch it
# replace pods one ordinal at a time without dropping the Service.
kubectl -n versus-ha rollout restart statefulset/versus
kubectl -n versus-ha rollout status statefulset/versus
```

## Tear down

```bash
kubectl delete -k .
# PVC for Postgres is retained by default; remove it explicitly if desired:
kubectl -n versus-ha delete pvc -l app.kubernetes.io/name=versus-postgres
```

## Secret handling notes (for the docs)

- `LICENSE_KEY` and `POSTGRES_DSN` live ONLY in the gitignored
  `.env`; kustomize's `secretGenerator` turns them into the `versus-secrets`
  Secret at apply time. Nothing secret is committed.
- The license is mounted as an **env var from the Secret** (`LICENSE_KEY`) — it
  is an offline JWT, never phoned home.
- **Managed secrets converge via Postgres**, not env: the session key, AI master
  key, and admin-token hash are generated once and shared across replicas, so no
  per-pod secret is needed beyond `LICENSE_KEY` (+ optional BYOK
  `VERSUS_ENTERPRISE_SECRET_KEY`).
- **Auth is session-only:** the data plane / admin UI is authenticated by a live
  session — the built-in default admin (password printed once on first boot) or
  SSO. There is no gateway secret. `VERSUS_ENTERPRISE_COOKIE_INSECURE=1` is set
  (dev-only) so the session cookie survives plain-HTTP `port-forward`; remove it
  behind TLS/ingress.
- For production: managed Postgres (RDS) with `sslmode=require`, TLS/ingress in
  front (drop the cookie-insecure shim), tightened NetworkPolicy egress, and
  `imagePullSecrets` for the private enterprise image.
