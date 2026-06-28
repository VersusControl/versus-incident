# HA with Helm

_Enterprise_

Install the production Helm chart and turn high availability on with **one values
file**. With `ha.enabled: true` the chart renders a **StatefulSet of replicas**
sharing one Postgres; with the default (`ha.enabled: false`) it renders the
single-instance **Deployment** unchanged — same chart, one switch.

> New to enterprise HA? Read the [overview](./overview.md) first for the runtime
> contract (shared Postgres, instance identity, secret convergence).

## What you'll need

| Item | Where it comes from |
|---|---|
| **Helm 3** + a cluster | `helm` ≥ 3 and `kubectl` access |
| The **enterprise image** pullable by the cluster | `ghcr.io/versuscontrol/versus-enterprise` (private — `imagePullSecrets`) |
| A **Versus Enterprise license** (`LICENSE_KEY`) | **required** — HA is license-gated; the chart refuses HA without one |
| A **Postgres** the cluster can reach | one shared database for all replicas (managed Postgres recommended) |

The chart lives at `versus-incident/helm/versus-incident` with an example
overlay `values-ha.yaml`.

## Step 1 — Review `values-ha.yaml`

`values-ha.yaml` is the HA overlay. It turns on the StatefulSet path, points at
the enterprise image, and selects Postgres storage:

```yaml
replicaCount: 3

image:
  repository: ghcr.io/versuscontrol/versus-enterprise
  tag: dev            # used verbatim — set the tag you want to pin

ha:
  enabled: true       # render the StatefulSet HA path (default false = Deployment)

storage:
  type: postgres      # required for >1 replica; the file backend refuses to start
  postgres:
    dsn: ""                 # provide via --set (below) — do NOT commit a real DSN
    dsnExistingSecret: ""   # …or reference your own Secret (key: postgres_dsn)

enterprise:
  licenseKey: ""      # required — provide via --set or an existing Secret
  existingSecret: ""  # …your own Secret (key: license_key)
```

`ha.enabled: true` derives `STORAGE_TYPE=postgres` and wires each replica's
`POD_NAME` (Downward API) + `INSTANCE_COUNT` (from `replicaCount`) for you. The
hardening (securityContext, NetworkPolicy, PodDisruptionBudget, anti-affinity)
and graceful-rollout probes come from the chart defaults.

## Step 2 — Install

Provide the license and the shared Postgres DSN. **Never
commit a real key or DSN** — pass them at install time:

```bash
helm upgrade --install versus oci://ghcr.io/versuscontrol/charts/versus-incident \
  -f versus-incident/helm/versus-incident/values-ha.yaml \
  --set enterprise.licenseKey="$LICENSE_KEY" \
  --set storage.postgres.dsn="postgres://versus:PASS@my-rds:5432/versus?sslmode=require"
```

For GitOps / production, prefer **existing Secrets** over inline `--set` so no
secret material lands in your release values:

```bash
helm upgrade --install versus oci://ghcr.io/versuscontrol/charts/versus-incident \
  -f versus-incident/helm/versus-incident/values-ha.yaml \
  --set enterprise.existingSecret=versus-license \
  --set storage.postgres.dsnExistingSecret=versus-pg
```

`enterprise.existingSecret` must hold the key `license_key`;
`storage.postgres.dsnExistingSecret` must hold the key `postgres_dsn`.

To **bring your own** enterprise encryption key, set
`VERSUS_ENTERPRISE_SECRET_KEY` (the same value on every replica) via the chart's
extra-env mechanism. Leave it unset to let Versus generate and converge the key
automatically.

## Step 3 — What success looks like

The chart renders a StatefulSet plus a headless Service, a PodDisruptionBudget,
a NetworkPolicy, and the license/DSN Secrets. Confirm the replicas are up:

```bash
kubectl rollout status statefulset/versus
kubectl get pods -l app.kubernetes.io/name=versus
# versus-0, versus-1, versus-2 all Running / Ready
```

**Each replica claims a distinct instance index:**

```bash
for i in 0 1 2; do kubectl logs versus-$i | grep -E "HA mode|instance [0-9]+ of"; done
# versus-0 -> "instance 0 of 3", and so on
```

**Sessions and secrets converge** through the shared Postgres, so a login on one
replica is valid on all — the Service can round-robin freely with no sticky
sessions. Log in as the built-in admin `admin`; its password is printed once in
the first-boot log (see [Getting Started](../getting-started.md)). See the
[overview](./overview.md) for why.

## Single-instance vs HA

| | Default (`ha.enabled: false`) | HA (`ha.enabled: true`) |
|---|---|---|
| Workload | Deployment | StatefulSet |
| Replicas | 1 | `replicaCount` (e.g. 3) |
| Storage | `file` or `postgres` | `postgres` (required) |
| License | optional | **required** |

Switching `ha.enabled` is the only change needed to move between the two — the
shared pod spec is identical.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `helm install` / template fails complaining about a license | HA requires a license. Set `enterprise.licenseKey` or `enterprise.existingSecret`. |
| Template fails complaining about storage | HA requires `storage.type: postgres` and a DSN. Set `storage.postgres.dsn` or `storage.postgres.dsnExistingSecret`. |
| Pods `ImagePullBackOff` | The enterprise image is private — add `imagePullSecrets` to the chart values. |
| You expected a StatefulSet but got a Deployment | `ha.enabled` is still `false`. Use `-f values-ha.yaml` (or `--set ha.enabled=true`). |
| Image tag missing the leading `v` | When set, `image.tag` is used **verbatim** (so enterprise tags like `dev` work). For an OSS semver tag, include the leading `v` yourself; leave the tag empty to default to `v<appVersion>`. |

## See also

- [High Availability overview](./overview.md)
- [HA on Docker Compose](./docker.md) · [HA on Kubernetes](./kubernetes.md)
- [Helm Chart](../../configuration/helm.md) — the full chart reference.
