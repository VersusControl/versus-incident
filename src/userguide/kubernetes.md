# Deploy on Kubernetes

This page covers running Versus Incident as plain Kubernetes manifests.
For the packaged distribution see [Helm Chart](./helm.md).

> **TL;DR for production:** mount a `PersistentVolumeClaim` at
> `/app/data` and set `GATEWAY_SECRET`. Without those two, the admin
> dashboard is unavailable and incident history disappears on every
> pod restart.

## Quick deploy

### 1. Create the secrets

```bash
kubectl create secret generic versus-secrets \
  --from-literal=gateway_secret=$GATEWAY_SECRET \
  --from-literal=slack_token=$SLACK_TOKEN \
  --from-literal=slack_channel_id=$SLACK_CHANNEL_ID
```

### 2. ConfigMap with config + templates

```yaml
# versus-config.yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: versus-config
data:
  config.yaml: |
    name: versus
    host: 0.0.0.0
    port: 3000
    public_host: https://versus.example.com  # external URL the dashboard uses

    gateway_secret: ${GATEWAY_SECRET}

    storage:
      type: file
      file:
        data_dir: /app/data         # mount a PVC here (see below)
        max_incidents: 1000

    alert:
      slack:
        enable: true
        token: ${SLACK_TOKEN}
        channel_id: ${SLACK_CHANNEL_ID}
        template_path: /app/config/slack_message.tmpl

  slack_message.tmpl: |
    *Critical Error in {{.ServiceName}}*
    ----------
    ```{{.Logs}}```
    Owner <@{{.UserID}}> please investigate
```

```bash
kubectl apply -f versus-config.yaml
```

## Persistent data store

Versus persists three things to disk via the `file` storage backend:

| File | Purpose |
|------|---------|
| `incidents.json` | Every incident received (rolling cap = `max_incidents`). |
| `patterns.json`  | AI-agent pattern catalog + services map. |
| `shadow.json`    | Append-only NDJSON log of shadow events. |

If you don't mount a volume, all three are written to the container's
ephemeral filesystem and **disappear on every pod restart, redeploy, or
rescheduling event**. The admin dashboard's incident history will look
like it resets.

### PersistentVolumeClaim

```yaml
# versus-pvc.yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: versus-data
spec:
  accessModes: ["ReadWriteOnce"]
  resources:
    requests:
      storage: 5Gi
  # storageClassName: gp3        # uncomment / set to your cluster's class
```

```bash
kubectl apply -f versus-pvc.yaml
```

> **Replicas vs. RWO.** A `ReadWriteOnce` volume binds to a single
> node. If you need `replicas > 1` either (a) switch to a
> `ReadWriteMany` class (EFS, Filestore, Azure Files) so every pod
> writes to the same directory, or (b) keep `replicas: 1` and use a
> `Recreate` deployment strategy. Sharing one RWO PVC across multiple
> pods will cause file corruption.

## Deployment

```yaml
# versus-deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: versus-incident
spec:
  replicas: 1                    # see PVC note above before bumping this
  strategy:
    type: Recreate
  selector:
    matchLabels:
      app: versus-incident
  template:
    metadata:
      labels:
        app: versus-incident
    spec:
      containers:
        - name: versus-incident
          image: ghcr.io/versuscontrol/versus-incident
          ports:
            - containerPort: 3000
          livenessProbe:
            httpGet:
              path: /healthz
              port: 3000
          readinessProbe:
            httpGet:
              path: /healthz
              port: 3000
          env:
            - name: GATEWAY_SECRET
              valueFrom:
                secretKeyRef:
                  name: versus-secrets
                  key: gateway_secret
            - name: SLACK_TOKEN
              valueFrom:
                secretKeyRef:
                  name: versus-secrets
                  key: slack_token
            - name: SLACK_CHANNEL_ID
              valueFrom:
                secretKeyRef:
                  name: versus-secrets
                  key: slack_channel_id
          volumeMounts:
            - name: versus-config
              mountPath: /app/config/config.yaml
              subPath: config.yaml
            - name: versus-config
              mountPath: /app/config/slack_message.tmpl
              subPath: slack_message.tmpl
            - name: versus-data
              mountPath: /app/data
      volumes:
        - name: versus-config
          configMap:
            name: versus-config
        - name: versus-data
          persistentVolumeClaim:
            claimName: versus-data
---
apiVersion: v1
kind: Service
metadata:
  name: versus-service
spec:
  selector:
    app: versus-incident
  ports:
    - protocol: TCP
      port: 3000
      targetPort: 3000
```

```bash
kubectl apply -f versus-deployment.yaml
```

## Exposing the dashboard

Set `public_host` in the config to the external URL clients (and the
admin dashboard's banner) should use. Then expose the Service via your
preferred path — Ingress, LoadBalancer, or `kubectl port-forward` for
local testing:

```bash
kubectl port-forward svc/versus-service 3000:3000
# open http://localhost:3000/
```

## Next steps

- [Admin Dashboard](./admin-ui.md) — what the UI surfaces and how to
  rebuild the bundled assets.
- [Configuration](./configuration.md) — every config key, env var, and
  per-request query parameter.
- [Helm Chart](./helm.md) — packaged install.
