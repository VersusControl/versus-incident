# HA on Kubernetes

_Enterprise_

## Concept

Run three Versus Enterprise replicas on Kubernetes with plain manifests you can
read top to bottom. A StatefulSet names the pods `versus-0`, `versus-1` and
`versus-2` and passes each its name, so every replica knows its place in the
set. All three share one Postgres. The [Helm chart](./helm.md) builds the same
setup. See [High Availability](./overview.md) for how it works.

## Example

The example is
[`examples/high-availability/k8s/`](https://github.com/VersusControl/versus-incident/tree/main/examples/high-availability/k8s).
It creates everything in the `versus-ha` namespace:

| File | Creates |
|---|---|
| `00-namespace.yaml` | The `versus-ha` namespace |
| `10-postgres.yaml` | A Postgres StatefulSet and Service: the shared store |
| `20-versus-config.yaml` | The Versus `config.yaml` (Postgres storage) |
| `30-versus-statefulset.yaml` | The Versus StatefulSet: 3 replicas, `INSTANCE_COUNT=3`, `POD_NAME` from the pod name |
| `40-versus-service.yaml` | The `versus` Service and a headless Service |
| `50-poddisruptionbudget.yaml` | A PodDisruptionBudget |
| `60-networkpolicy.yaml` | Network policies: deny other ingress, allow Postgres from Versus |
| `kustomization.yaml` | Builds the `versus-secrets` Secret from a local `.env` file |

## Run it

You need a cluster (kind, minikube or any other), `kubectl` and Git. The cluster
must be able to pull `ghcr.io/versuscontrol/versus-enterprise`. Clone the repository and go into it:

```bash
git clone https://github.com/VersusControl/versus-incident.git
cd versus-incident
```

Every command below runs from this `versus-incident` folder, the repository
root.

**1. Create the secrets file.** Kustomize builds the Secret from `.env`, which
git ignores:

```bash
cp examples/high-availability/k8s/.env.example examples/high-availability/k8s/.env
```

In `examples/high-availability/k8s/.env`, replace `PASTE_YOUR_ENTERPRISE_LICENSE_JWT_HERE` with your license
key. The Postgres values already match each other; leave them for the demo.

```bash
license_key=<your license key>
postgres_password=versus
postgres_dsn=postgres://versus:versus@versus-postgres:5432/versus?sslmode=disable
```

**2. Apply everything:**

```bash
kubectl apply -k examples/high-availability/k8s
kubectl -n versus-ha rollout status statefulset/versus
kubectl -n versus-ha get pods -l app.kubernetes.io/name=versus
```

`versus-0`, `versus-1` and `versus-2` are `Running` and ready.

**3. Check each replica's place in the set:**

```bash
for i in 0 1 2; do kubectl -n versus-ha logs versus-$i | grep "HA mode"; done
```

You see `instance 0 of 3`, `instance 1 of 3` and `instance 2 of 3`.

**4. Sign in.** One replica printed the admin password:

```bash
kubectl -n versus-ha logs -l app.kubernetes.io/name=versus --tail=-1 \
  | grep -A6 "DEFAULT ADMIN CREDENTIALS"
kubectl -n versus-ha port-forward svc/versus 3000:3000
```

The port-forward keeps running: leave it open while you use the UI, and run
the next steps in a second terminal in the `versus-incident` folder.

Open `http://localhost:3000` and sign in as `admin`. The StatefulSet sets
`VERSUS_ENTERPRISE_COOKIE_INSECURE=1` so sign-in works over the plain-HTTP
port-forward; remove it in production.

**5. Roll the replicas** one at a time:

```bash
kubectl -n versus-ha rollout restart statefulset/versus
kubectl -n versus-ha rollout status statefulset/versus
```

**Clean up:**

```bash
kubectl delete -k examples/high-availability/k8s
kubectl delete namespace versus-ha --ignore-not-found
```

## For production

- Use a managed Postgres with `sslmode=require` instead of the in-cluster one.
- Add `imagePullSecrets` if your cluster needs credentials for the registry.
- Tighten the network policies to your environment, but keep outbound HTTPS to
  `versusincident.com` from every replica so the license key can renew.
- Remove `VERSUS_ENTERPRISE_COOKIE_INSECURE` and serve Versus over HTTPS.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| A pod restarts at start | `postgres_dsn` is wrong, or storage is not `postgres`. File storage is refused with more than one replica. |
| `ImagePullBackOff` | The cluster cannot pull the image. Add `imagePullSecrets`, or on kind run `kind load docker-image ghcr.io/versuscontrol/versus-enterprise`. |
| `secret "versus-secrets" not found` | `examples/high-availability/k8s/.env` is missing. Run `cp examples/high-availability/k8s/.env.example examples/high-availability/k8s/.env` and apply again. |
| No `DEFAULT ADMIN CREDENTIALS` in the logs | The license key is still the placeholder, or Postgres already holds an admin. |
| Pods `Pending` on a one-node cluster | Check node capacity. The example's anti-affinity is a preference, so one node can run all three. |

## See also

- [High Availability](./overview.md)
- [HA on Docker Compose](./docker.md) · [HA with Helm](./helm.md)
- [Deploy on Kubernetes](../../configuration/kubernetes.md): the single-instance setup.
