# HA on Kubernetes

_Enterprise_

Run the Enterprise SRE Agent as **3 replicas sharing one Postgres** with plain
Kubernetes manifests you can read top to bottom. This is the same topology the
[Helm chart](./helm.md) produces, applied with `kubectl apply -k .` (kustomize).

> New to enterprise HA? Read the [overview](./overview.md) first for the runtime
> contract (shared Postgres, instance identity, secret convergence).

## What you'll need

| Item | Where it comes from |
|---|---|
| A **cluster** + `kubectl` | kind, minikube, or any cluster. Kustomize is built into `kubectl` ≥ 1.14 (`kubectl apply -k`). |
| The **enterprise image** pullable by the cluster | `ghcr.io/versuscontrol/versus-enterprise:dev` (private — use `imagePullSecrets`, or on kind: `kind load docker-image ghcr.io/versuscontrol/versus-enterprise:dev`) |
| A **Versus Enterprise license** (`LICENSE_KEY`) | required to prove session/secret convergence; the topology boots without it |

The example lives at `versus-incident/examples/high-availability/k8s/` and
creates everything in the `versus-ha` namespace:

| File | Resource |
|---|---|
| `00-namespace.yaml` | Namespace |
| `10-postgres.yaml` | Postgres StatefulSet + headless Service (the ONE shared store) |
| `20-versus-config.yaml` | ConfigMap (`config.yaml`, storage = postgres) |
| `30-versus-statefulset.yaml` | **Versus StatefulSet (3 replicas)** — `POD_NAME` via Downward API, `INSTANCE_COUNT=3`, hardened securityContext, anti-affinity, probes |
| `40-versus-service.yaml` | ClusterIP + headless Service |
| `50-poddisruptionbudget.yaml` | PodDisruptionBudget |
| `60-networkpolicy.yaml` | NetworkPolicy ×2 (default-deny ingress; Postgres reachable) |
| `kustomization.yaml` | `secretGenerator` that builds `versus-secrets` from a gitignored `.env` |

## Step 1 — Provide the license and secrets

Kustomize's `secretGenerator` builds the `versus-secrets` Secret from a
**gitignored `.env`** — never commit a real key. Copy the placeholder and fill
it in:

```bash
cd versus-incident/examples/high-availability/k8s
cp .env.example .env
```

Edit `.env`:

```bash
license_key=PASTE_YOUR_ENTERPRISE_LICENSE_JWT_HERE
# Local dev Postgres — postgres_password must match the password in postgres_dsn.
postgres_password=versus
postgres_dsn=postgres://versus:versus@versus-postgres:5432/versus?sslmode=disable
```

These keys become `versus-secrets`; the StatefulSet reads `license_key` and
`postgres_dsn` from it. The dev Postgres password and DSN
already line up out of the box.

## Step 2 — Apply everything

```bash
kubectl apply -k .
```

This creates the Namespace, Postgres, the `versus-secrets` Secret, the Versus
StatefulSet, both Services, the PodDisruptionBudget, and the NetworkPolicies.

## Step 3 — Wait for the replicas

```bash
kubectl -n versus-ha rollout status statefulset/versus
kubectl -n versus-ha get pods -l app.kubernetes.io/name=versus
# versus-0, versus-1, versus-2 all Running / Ready
```

## Step 4 — What success looks like

**Each replica claims a distinct instance index** (from its `POD_NAME` ordinal,
supplied by the StatefulSet via the Downward API):

```bash
for i in 0 1 2; do
  echo "== versus-$i =="
  kubectl -n versus-ha logs versus-$i | grep -E "HA mode|instance [0-9]+ of"
done
# versus-0 -> "instance 0 of 3"
# versus-1 -> "instance 1 of 3"
# versus-2 -> "instance 2 of 3"
```

**Secrets converge** (licensed): the break-glass default-admin password is
generated **once** by the winning replica and printed once — search all three:

```bash
kubectl -n versus-ha logs -l app.kubernetes.io/name=versus --tail=-1 \
  | grep -A3 "DEFAULT ADMIN CREDENTIALS"
```

**Hit the load-balanced endpoint** (round-robins across replicas):

```bash
kubectl -n versus-ha port-forward svc/versus 3000:3000
# in another shell:
curl -fsS http://localhost:3000/healthz && echo OK
```

Log in at <http://localhost:3000> as the built-in admin `admin` with the
password from the log above. A session cookie obtained from one replica is
accepted by any other, because the session key, AI master key, and admin-token
hash converge through Postgres. (The StatefulSet sets the dev-only
`VERSUS_ENTERPRISE_COOKIE_INSECURE=1` so the cookie survives the plain-HTTP
port-forward — never set this in production.)

## Graceful rollout

Replace pods one ordinal at a time without dropping the Service:

```bash
kubectl -n versus-ha rollout restart statefulset/versus
kubectl -n versus-ha rollout status statefulset/versus
```

## Tear down

```bash
kubectl delete -k .
# the Postgres PVC is retained by default; remove it explicitly if desired:
kubectl -n versus-ha delete pvc -l app.kubernetes.io/name=versus-postgres
```

## For production

- Use a **managed Postgres** (e.g. RDS) with `sslmode=require` instead of the
  in-cluster dev Postgres.
- Add `imagePullSecrets` for the private enterprise image.
- Tighten the NetworkPolicy egress to your environment.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| A pod `CrashLoopBackOff`s at boot | `STORAGE_TYPE` isn't `postgres` while `INSTANCE_COUNT > 1`, or `postgres_dsn` is wrong. All replicas must share one Postgres — the file backend is single-node and the binary refuses it. |
| Pods stuck `ImagePullBackOff` | The enterprise image is private. Add `imagePullSecrets`, or on kind `kind load docker-image …` the image into the cluster. |
| Pods stuck `Pending` on a single-node cluster | Soft anti-affinity won't block scheduling, but verify the node has capacity. The example uses `preferredDuringScheduling…` so a one-node kind/minikube cluster still schedules all three. |
| `secret "versus-secrets" not found` | You skipped `cp .env.example .env` (the `secretGenerator` reads `.env`). Create it, then re-run `kubectl apply -k .`. |
| Login on one replica rejected after hitting another | Running unlicensed (no converged session key), or the replicas don't share one `postgres_dsn`. Confirm a license is set and the DSN is identical. |

## See also

- [High Availability overview](./overview.md)
- [HA on Docker Compose](./docker.md) · [HA with Helm](./helm.md)
- [Deploy on Kubernetes](../../configuration/kubernetes.md) — the single-instance
  manifests this builds on.
