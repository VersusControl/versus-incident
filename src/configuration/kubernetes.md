# Deploy on Kubernetes

Install Versus with one `kubectl apply`. Each setup below is a single manifest
file, generated from the [Helm Chart](/configuration/helm). Pick the one that
matches what you want Versus to do:

| Setup | What runs | Manifest |
| --- | --- | --- |
| [Kubernetes in-cluster](#kubernetes-in-cluster) | Versus reads the cluster it runs in, with a read-only role. | `k8s-connector-in-cluster-enabled.yaml` |
| [Agent only](#agent-only) | The SRE Agent reads one Loki log source and learns its patterns. | `agent-only.yaml` |
| [Webhook only](#webhook-only) | Your monitoring tools send alerts to Versus, and Versus posts them to Slack and Microsoft Teams. | `webhook-only.yaml` |

The files are in
[`examples/kubernetes/`](https://github.com/VersusControl/versus-incident/tree/main/examples/kubernetes).
Each installs into the namespace `versus`, with one Deployment and one Service
named `versus-incident`.

## Before you start

You need a Kubernetes cluster, with `kubectl` pointed at it:

```bash
kubectl get nodes
```

Every setup needs the namespace and a sign-in secret. The manifests do not
contain the secret, so create it first:

```bash
kubectl create namespace versus --save-config
```

```bash
kubectl -n versus create secret generic versus-incident --from-literal=gateway_secret=$(openssl rand -hex 32)
```

Expected output: `namespace/versus created`, then
`secret/versus-incident created`.

## Kubernetes in-cluster

Versus runs in your cluster and reads it with its own ServiceAccount. The
manifest turns on the Kubernetes connector and creates a cluster-wide,
read-only ClusterRole for it. Versus does not read Secrets and never changes
anything in the cluster. The agent and the alert channels stay off.

```bash
kubectl apply -f https://raw.githubusercontent.com/VersusControl/versus-incident/main/examples/kubernetes/k8s-connector-in-cluster-enabled.yaml
```

The [AIOps Kubernetes Quickstart](/agent/kubernetes/quickstart) walks through
this install step by step: the permissions you need, signing in, the first
sync, and what to check. The result is the Kubernetes Overview:

![The Kubernetes Overview after the first sync: Last synced in the top bar, two broken workloads with their changes on the timeline, CPU and memory capacity, and resource counts](../docs/images/kubernetes-quickstart-overview-dark.png)

*The Overview after the first sync.*

## Agent only

The SRE Agent reads your logs, learns which log patterns are normal, and later
raises an incident when a new pattern or a spike shows up. The Kubernetes
connector stays off (no reader role is created) and no alert channel is on.

`agent-only.yaml` sets:

| Setting | Value |
| --- | --- |
| Agent | On, in `training` mode: it reads logs and builds the pattern catalog, and sends nothing. |
| Log source | One Loki source named `checkout-logs`: address `http://loki.monitoring:3100`, query `{app="checkout"}`. |
| AI | Off. Detect mode works without a model. |
| Storage | A 1Gi PersistentVolumeClaim on the default StorageClass, so the learned patterns survive a restart. |

**1. Download the manifest and point it at your Loki.**

```bash
curl -LO https://raw.githubusercontent.com/VersusControl/versus-incident/main/examples/kubernetes/agent-only.yaml
```

Open `agent-only.yaml`, find the `agent_sources.yaml` key of the ConfigMap, and
change `address` to your Loki service (`http://<service>.<namespace>:3100`
inside the cluster) and `query` to the streams to watch.

**2. Apply it and wait until it is ready.**

```bash
kubectl apply -f agent-only.yaml
```

```bash
kubectl -n versus rollout status deploy/versus-incident
```

**3. Check that the agent started.**

```bash
kubectl -n versus logs deploy/versus-incident | grep "starting worker"
```

Expected output: `agent: starting worker mode=training sources=1 poll=30s`.

### What you'll see

[Open the UI](#open-the-ui) and go to **Agent Learning → Logs**. Within a
minute of the first matching lines, learned log patterns appear, grouped by
service, each with its count.

- **Which lines are learned.** Training learns only the lines that match the
  learning pattern, plus a few named rules (out of memory, panics, HTTP 5xx).
  By default that is lines containing `error`. While your service writes only
  `info` and `warn` lines, the page stays empty. To learn every line, set
  `default_pattern: ".*"` under `agent.regex` in the `config.yaml` key of the
  ConfigMap in your copy of `agent-only.yaml`, apply it again, then restart
  with `kubectl -n versus rollout restart deploy/versus-incident`. See
  [Regex](/agent/regex).
- **Which service.** The service name is read from the log line itself, such
  as `service=checkout` or `app=checkout`, not from the Loki stream label in
  your `query`. Lines without one are grouped under `—`.

![Agent Learning → Logs during training: learned log templates grouped by service, each with its count and a known verdict](../docs/images/quickstart-sre-patterns-dark.png)

### Switch to detect mode

Leave the agent in training until the catalog has seen normal traffic. Then
switch it to `detect`, or to `shadow` first, which only logs what it would
have sent:

```bash
kubectl -n versus set env deploy/versus-incident AGENT_MODE=detect
```

This restarts the pod. Applying `agent-only.yaml` again sets the mode back to
`training`, so change the `AGENT_MODE` value in your copy of the file as well.

Detect-mode incidents reach Slack or another channel only when that channel is
on. See [Notification Channels](/agent/channels).

### Loki credentials

For a multi-tenant Loki or a token-protected gateway, keep the token in a
Secret and reference it from the source as `${VAR}`:

```bash
kubectl -n versus create secret generic loki-credentials --from-literal=LOKI_TOKEN=<token>
```

In your copy of `agent-only.yaml`, add `bearer_token: ${LOKI_TOKEN}` under
`loki:` in the `agent_sources.yaml` key, apply the file again, then pass the
Secret to the pod:

```bash
kubectl -n versus set env deploy/versus-incident --from=secret/loki-credentials
```

All Loki settings are on the [Loki](/agent/data-sources/loki) page. To learn
how the modes work, read [Training Mode](/agent/training-mode),
[Shadow Mode](/agent/shadow-mode) and [AI Detect Mode](/agent/ai-detect-mode).
Other log sources are listed under [Data Sources](/agent/data-sources).

## Webhook only

Your monitoring tools (Alertmanager, Grafana, Sentry, CloudWatch, Fluent Bit)
send alerts to `http://versus-incident.versus:3000/api/incidents`. Versus
formats each alert with a template and posts it to Slack and Microsoft Teams.
The agent and the Kubernetes connector stay off.

`webhook-only.yaml` sets:

| Setting | Value |
| --- | --- |
| Channels | Slack and Microsoft Teams on. |
| Slack template | A custom template for Alertmanager and Grafana alerts: status, alert name, severity and summary. Teams uses the shipped template. |
| On-call | Off. |

**1. Create the channel secret.** The manifest reads the Slack and Teams
credentials from a Secret named `versus-incident-secrets`, and does not contain
it:

```bash
kubectl -n versus create secret generic versus-incident-secrets \
  --from-literal=slack_token=$SLACK_BOT_TOKEN \
  --from-literal=slack_channel_id=$SLACK_CHANNEL_ID \
  --from-literal=msteams_power_automate_url=$TEAMS_POWER_AUTOMATE_URL
```

Using only Slack? Download the manifest, set `MSTEAMS_ENABLE` to `"false"`, and
delete the `MSTEAMS_POWER_AUTOMATE_URL` entry. Then apply your copy in step 2.

**2. Apply the manifest and wait until it is ready.**

```bash
kubectl apply -f https://raw.githubusercontent.com/VersusControl/versus-incident/main/examples/kubernetes/webhook-only.yaml
```

```bash
kubectl -n versus rollout status deploy/versus-incident
```

**3. Send a test alert.** Forward the port. This command holds the terminal, so
open a second one for the `curl`:

```bash
kubectl -n versus port-forward svc/versus-incident 3000:3000
```

```bash
curl -X POST http://localhost:3000/api/incidents \
  -H 'Content-Type: application/json' \
  -d '{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"HighErrorRate","severity":"critical"},"annotations":{"summary":"5xx rate above 5% on checkout"}}]}'
```

Expected output: `{"incident_id":"<id>","notify_status":"sent","status":"Incident created"}`,
with HTTP status 201. If a token or channel is wrong, the alert is still stored
and the response shows `"notify_status":"failed"` (or `partial` when the other
channel worked) with the channel's error in `notify_error`.

### What you'll see

In Slack, formatted by the custom template:

```text
*FIRING: HighErrorRate* (critical)
5xx rate above 5% on checkout
```

In the UI ([Open the UI](#open-the-ui)), go to **Incidents**, pick the
**Webhook / Alerts** tab, then the **All** filter. The alert's row shows the
title `HighErrorRate` and severity **CRITICAL**.

To turn on on-call escalation (PagerDuty, AWS Incident Manager and others),
install with Helm: on-call needs Redis. See
[Webhook only](/configuration/helm#webhook-only) on the Helm page.

For the same flow on Docker, read the [Webhook Quickstart](/webhook/quickstart).
To write your own templates, read [Template Syntax](/webhook/template-syntax)
and [Advanced Template Tips](/webhook/advanced-template-tips).

## Open the UI

This works for every setup. The command holds the terminal; leave it running:

```bash
kubectl -n versus port-forward svc/versus-incident 3000:3000
```

Browse to `http://localhost:3000`. In a second terminal, print the sign-in
secret and paste it into **Gateway secret**:

```bash
kubectl -n versus get secret versus-incident -o jsonpath='{.data.gateway_secret}' | base64 -d; echo
```

## Reference

### Storage

Versus keeps incidents, the pattern catalog and settings under `/app/data`.
Only `agent-only.yaml` mounts a PersistentVolumeClaim there
(`versus-incident-data`, 1Gi, `ReadWriteOnce`). The other two use an
`emptyDir`, so their data is lost when the pod restarts. To keep it, use
PostgreSQL, or install with Helm and set `storage.persistence.enabled=true`.

Run one replica with file storage. Two pods on the same files disagree with
each other.

### PostgreSQL

For durable history, store everything in PostgreSQL. Put the connection string
in a Secret:

```bash
kubectl -n versus create secret generic versus-postgres --from-literal=POSTGRES_DSN='postgres://user:pass@host:5432/versus?sslmode=require'
```

Then, in your copy of the manifest, change the `STORAGE_TYPE` value of the
`versus-incident` container to `"postgres"` and add the connection string next
to it:

```yaml
            - name: STORAGE_TYPE
              value: "postgres"
            - name: POSTGRES_DSN
              valueFrom:
                secretKeyRef:
                  name: versus-postgres
                  key: POSTGRES_DSN
```

Apply your copy. A quick switch with
`kubectl -n versus set env deploy/versus-incident STORAGE_TYPE=postgres --from=secret/versus-postgres`
also works, but the next apply of the file sets `STORAGE_TYPE` back to `file`.

See [PostgreSQL Storage](/configuration/postgres-storage) for creating the
database and role.

### Exposing the UI

The Service `versus-incident` is a `ClusterIP` on port 3000. To reach the UI
without port-forward, put an Ingress or a LoadBalancer in front of it, then set
`public_host` to the exact URL users open in the browser, for example
`public_host: https://versus.example.com`. Add it to the `config.yaml` key of
the `versus-incident-config` ConfigMap and restart the pod:

```bash
kubectl -n versus rollout restart deploy/versus-incident
```

Versus uses `public_host` for links in alerts, secure cookies, and its
same-origin checks behind a proxy that rewrites the host or terminates TLS.

### Upgrade

The manifests use the image `ghcr.io/versuscontrol/versus-incident` with no tag,
and a node keeps the copy it pulled first (`imagePullPolicy: IfNotPresent`).
To run a given release, set the tag in your copy of the file, for example
`ghcr.io/versuscontrol/versus-incident:v1.4.36`, and apply it again.

Applying a file again resets every setting the file contains, so a value you
changed with `kubectl set env` or `kubectl edit` (such as `AGENT_MODE` or
`STORAGE_TYPE`) goes back to the file's value. Variables you added that are not
in the file, such as `LOKI_TOKEN`, stay. To keep changes across upgrades, make
them in your copy of the file, or install with [Helm](/configuration/helm).

### Uninstall

Delete with the same file you applied, for example:

```bash
kubectl delete -f https://raw.githubusercontent.com/VersusControl/versus-incident/main/examples/kubernetes/k8s-connector-in-cluster-enabled.yaml
```

This deletes the `versus` namespace too, with the secrets and any
PersistentVolumeClaim in it. For the in-cluster setup it also deletes the
reader ClusterRole and its binding.

## Next steps

- [Helm Chart](/configuration/helm): the same three setups with `helm install`.
- [Configuration](/configuration/configuration): every config key and
  environment variable.
- [Kubernetes Configuration](/agent/kubernetes/configuration): connector
  settings, optional kinds and namespace-only access.
