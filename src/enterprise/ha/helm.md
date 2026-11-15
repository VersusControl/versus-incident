# HA with Helm

_Enterprise_

## Concept

The Versus Helm chart runs a single replica by default. One values file,
`values-ha.yaml`, turns it into an HA install: three Enterprise replicas in a
StatefulSet, all on one Postgres, with a PodDisruptionBudget, a network policy
and soft anti-affinity. See [High Availability](./overview.md) for how it
works.

| | Default | With `values-ha.yaml` (`ha.enabled: true`) |
|---|---|---|
| Workload | Deployment | StatefulSet |
| Replicas | 1 | `replicaCount` (3 in the file) |
| Storage | `file` or `postgres` | `postgres` (required) |
| License | optional | required |

## Example

`values-ha.yaml` lives in the chart folder,
[`helm/versus-incident/values-ha.yaml`](https://github.com/VersusControl/versus-incident/blob/main/helm/versus-incident/values-ha.yaml):

```yaml
replicaCount: 3

image:
  repository: ghcr.io/versuscontrol/versus-enterprise
  tag: v1.0.0          # used as written; set the Enterprise tag you run

ha:
  enabled: true

storage:
  type: postgres
  postgres:
    dsn: ""                 # pass with --set, or
    dsnExistingSecret: ""   # name a Secret with the key postgres_dsn

enterprise:
  licenseKey: ""      # pass with --set, or
  existingSecret: ""  # name a Secret with the key license_key
```

With `ha.enabled: true` the chart sets `POD_NAME` and `INSTANCE_COUNT` on every
replica for you.

## Install

You need Helm 3, Git, a cluster, and a Postgres the cluster can reach. The
chart comes from the OCI registry; `values-ha.yaml` comes from the repository.
Clone it and go into it:

```bash
git clone https://github.com/VersusControl/versus-incident.git
cd versus-incident
```

Run the commands below from this `versus-incident` folder, the repository root:

```bash
export LICENSE_KEY=<your license key>

helm upgrade --install versus-incident oci://ghcr.io/versuscontrol/charts/versus-incident \
  -f helm/versus-incident/values-ha.yaml \
  --set enterprise.licenseKey="$LICENSE_KEY" \
  --set storage.postgres.dsn="postgres://versus:<password>@<host>:5432/versus?sslmode=require"
```

For GitOps, keep secrets out of your values and reference Secrets you created:

```bash
helm upgrade --install versus-incident oci://ghcr.io/versuscontrol/charts/versus-incident \
  -f helm/versus-incident/values-ha.yaml \
  --set enterprise.existingSecret=versus-license \
  --set storage.postgres.dsnExistingSecret=versus-pg
```

`versus-license` must hold the key `license_key`, and `versus-pg` the key
`postgres_dsn`.

## What you see

```bash
kubectl rollout status statefulset/versus-incident
kubectl get pods -l app.kubernetes.io/name=versus-incident
for i in 0 1 2; do kubectl logs versus-incident-$i | grep "HA mode"; done
```

The pods are `versus-incident-0`, `-1` and `-2`, and each logs its place:
`instance 0 of 3`, `instance 1 of 3`, `instance 2 of 3`. Find the admin
password with:

```bash
kubectl logs -l app.kubernetes.io/name=versus-incident --tail=-1 \
  | grep -A6 "DEFAULT ADMIN CREDENTIALS"
```

Sign in through your Ingress or a port-forward to the `versus-incident`
Service. A sign-in works on every replica.

Every replica needs outbound HTTPS to `versusincident.com` to renew the license
key; allow it if you restrict egress.

To bring your own encryption key, set `VERSUS_ENTERPRISE_SECRET_KEY` to the
same value on every replica (see [High Availability](./overview.md#encryption-key)).

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `ha.enabled=true is an ENTERPRISE capability and requires a license` | Set `enterprise.licenseKey` or `enterprise.existingSecret`. |
| `storage.type=postgres requires a DSN` | Set `storage.postgres.dsn` or `storage.postgres.dsnExistingSecret`. |
| A Deployment is created instead of a StatefulSet | `ha.enabled` is still `false`. Pass `-f helm/versus-incident/values-ha.yaml`. `--set ha.enabled=true` alone keeps the OSS image and one replica; without the file, also set `image.repository=ghcr.io/versuscontrol/versus-enterprise`, `image.tag` and `replicaCount`. |
| `ImagePullBackOff` | Wrong `image.tag`, or the cluster needs `imagePullSecrets` for the registry. |

## See also

- [High Availability](./overview.md)
- [HA on Docker Compose](./docker.md) · [HA on Kubernetes](./kubernetes.md)
- [Helm Chart](../../configuration/helm.md): every chart value.
