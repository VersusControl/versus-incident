# Helm Chart

Install Versus with `helm install` and one values file. Pick the setup that
matches what you want Versus to do:

| Setup | What runs | Values file |
| --- | --- | --- |
| [Kubernetes in-cluster](#kubernetes-in-cluster) | Versus reads the cluster it runs in, with a read-only role. | `values-kubernetes-in-cluster.yaml` |
| [Agent only](#agent-only) | The SRE Agent reads one Loki log source and learns its patterns. | `values-agent-only.yaml` |
| [Webhook only](#webhook-only) | Your monitoring tools send alerts to Versus, and Versus posts them to Slack and Microsoft Teams. | `values-webhook-only.yaml` |

The values files are in
[`examples/kubernetes/`](https://github.com/VersusControl/versus-incident/tree/main/examples/kubernetes).
To install without Helm, see [Deploy on Kubernetes](/configuration/kubernetes):
the same three setups as one-file manifests.

## Before you start

You need:

- A Kubernetes cluster, with `kubectl` pointed at it.
- Helm 3.8 or newer (the chart is published as an OCI artifact). The upgrade
  commands use `--reset-then-reuse-values`, which needs Helm 3.14; on older
  Helm, pass your values file and `--set` flags again instead.

Every setup needs the namespace and a sign-in secret:

```bash
kubectl create namespace versus --save-config
```

```bash
kubectl -n versus create secret generic versus-incident --from-literal=gateway_secret=$(openssl rand -hex 32)
```

Every command on this page names the release `versus-incident`, so the
Deployment and the Service are both `versus-incident` too. The chart runs the
image `ghcr.io/versuscontrol/versus-incident:v<appVersion>`, the release that
matches the chart version.

## Kubernetes in-cluster

Versus runs in your cluster and reads it with its own ServiceAccount.
`connectors.kubernetes.enabled: true` turns on the Kubernetes connector, and
the chart creates a cluster-wide, read-only ClusterRole for it. Versus does not
read Secrets and never changes anything in the cluster. The agent and the alert
channels stay off.

```bash
curl -LO https://raw.githubusercontent.com/VersusControl/versus-incident/main/examples/kubernetes/values-kubernetes-in-cluster.yaml
```

```bash
helm install versus-incident oci://ghcr.io/versuscontrol/charts/versus-incident \
  --namespace versus \
  -f values-kubernetes-in-cluster.yaml \
  --set gatewaySecretExistingSecret=versus-incident
```

The [AIOps Kubernetes Quickstart](/agent/kubernetes/quickstart) walks through
signing in, the first sync, what to check, and the settings you can add (for
example optional kinds and namespace-only access). The result is the Kubernetes
Overview:

![The Kubernetes Overview after the first sync: Last synced in the top bar, two broken workloads with their changes on the timeline, CPU and memory capacity, and resource counts](../docs/images/kubernetes-quickstart-overview-dark.png)

*The Overview after the first sync.*

## Agent only

The SRE Agent reads your logs, learns which log patterns are normal, and later
raises an incident when a new pattern or a spike shows up. The Kubernetes
connector stays off (no reader role is created) and no alert channel is on.

`values-agent-only.yaml` sets:

| Setting | Value |
| --- | --- |
| `agent.enable`, `agent.mode` | On, in `training` mode: the agent reads logs and builds the pattern catalog, and sends nothing. |
| `agent.sources` | One Loki source named `checkout-logs`: address `http://loki.monitoring:3100`, query `{app="checkout"}`. |
| `agent.ai.enable` | `false`. Detect mode works without a model. |
| `storage.persistence` | A 1Gi PersistentVolumeClaim on the default StorageClass, so the learned patterns survive a restart. |

**1. Download the values file and point it at your Loki.**

```bash
curl -LO https://raw.githubusercontent.com/VersusControl/versus-incident/main/examples/kubernetes/values-agent-only.yaml
```

In `values-agent-only.yaml`, change `address` to your Loki service
(`http://<service>.<namespace>:3100` inside the cluster) and `query` to the
streams to watch.

**2. Install.**

```bash
helm install versus-incident oci://ghcr.io/versuscontrol/charts/versus-incident \
  --namespace versus \
  -f values-agent-only.yaml \
  --set gatewaySecretExistingSecret=versus-incident
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
  `agent.regex.defaultPattern: ".*"` in `values-agent-only.yaml` and apply it
  as in [Loki credentials](#loki-credentials). See [Regex](/agent/regex).
- **Which service.** The service name is read from the log line itself, such
  as `service=checkout` or `app=checkout`, not from the Loki stream label in
  your `query`. Lines without one are grouped under `—`.

![Agent Learning → Logs during training: learned log templates grouped by service, each with its count and a known verdict](../docs/images/quickstart-sre-patterns-dark.png)

### Switch to detect mode

Leave the agent in training until the catalog has seen normal traffic. Then
switch it to `detect`, or to `shadow` first, which only logs what it would
have sent:

```bash
helm upgrade versus-incident oci://ghcr.io/versuscontrol/charts/versus-incident \
  --namespace versus --reset-then-reuse-values --set agent.mode=detect
```

Also change `mode: training` to `mode: detect` in `values-agent-only.yaml`.
A later upgrade with `-f values-agent-only.yaml` applies the file's mode over
the `--set`, and would switch the agent back to training.

Detect-mode incidents reach a channel only when one is on. To send them to
Slack, add to the same command:

```bash
--set alert.slack.enable=true \
--set-string alert.slack.token=$SLACK_BOT_TOKEN \
--set-string alert.slack.channelId=$SLACK_CHANNEL_ID
```

To add AI analysis, add `--set agent.ai.enable=true --set-string agent.ai.apiKey=$OPENAI_API_KEY`.
Pass keys on the command line, never in the values file.

### Loki credentials

The source settings land in a ConfigMap, so write credentials there as `${VAR}`
and supply the value from a Secret with `extraEnv`:

```bash
kubectl -n versus create secret generic loki-credentials --from-literal=token=<token>
```

```yaml
agent:
  sources:
    - name: checkout-logs
      type: loki
      enable: true
      loki:
        address: http://loki.monitoring:3100
        query: '{app="checkout"}'
        severity_field: level
        bearer_token: ${LOKI_TOKEN}

extraEnv:
  - name: LOKI_TOKEN
    valueFrom:
      secretKeyRef:
        name: loki-credentials
        key: token
```

Add both blocks to `values-agent-only.yaml`, then apply the file:

```bash
helm upgrade versus-incident oci://ghcr.io/versuscontrol/charts/versus-incident \
  --namespace versus --reset-then-reuse-values -f values-agent-only.yaml
```

The file's `agent.mode` wins over an earlier `--set agent.mode=detect`. Once
detect is on, keep `mode: detect` in the file, or add
`--set agent.mode=detect` to this command.

All Loki settings are on the [Loki](/agent/data-sources/loki) page. To learn
how the modes work, read [Training Mode](/agent/training-mode),
[Shadow Mode](/agent/shadow-mode) and [AI Detect Mode](/agent/ai-detect-mode).
Other log sources are listed under [Data Sources](/agent/data-sources).

## Webhook only

Your monitoring tools (Alertmanager, Grafana, Sentry, CloudWatch, Fluent Bit)
send alerts to `http://versus-incident.versus:3000/api/incidents`. Versus
formats each alert with a template and posts it to Slack and Microsoft Teams.
The agent and the Kubernetes connector stay off.

`values-webhook-only.yaml` sets:

| Setting | Value |
| --- | --- |
| `alert.slack.enable`, `alert.msteams.enable` | Slack and Microsoft Teams on. |
| `templates.slack` | A custom template for Alertmanager and Grafana alerts: status, alert name, severity and summary. Teams uses the shipped template. |
| `oncall` | Commented out, ready to turn on. |

**1. Install** with the channel credentials on the command line. The chart
keeps them in its own Secret; it has no option to read them from a Secret you
create, so never write them in the values file:

```bash
curl -LO https://raw.githubusercontent.com/VersusControl/versus-incident/main/examples/kubernetes/values-webhook-only.yaml
```

```bash
helm install versus-incident oci://ghcr.io/versuscontrol/charts/versus-incident \
  --namespace versus \
  -f values-webhook-only.yaml \
  --set gatewaySecretExistingSecret=versus-incident \
  --set-string alert.slack.token=$SLACK_BOT_TOKEN \
  --set-string alert.slack.channelId=$SLACK_CHANNEL_ID \
  --set-string alert.msteams.powerAutomateUrl=$TEAMS_POWER_AUTOMATE_URL
```

Using only Slack? Replace the Teams flag with `--set alert.msteams.enable=false`.

```bash
kubectl -n versus rollout status deploy/versus-incident
```

**2. Send a test alert.** Forward the port. This command holds the terminal, so
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

### Turn on on-call

To page PagerDuty when nobody acknowledges an alert within `waitMinutes`:

1. Uncomment the `oncall` and `redis` blocks at the end of
   `values-webhook-only.yaml`. On-call keeps its timers in Redis; the `redis`
   block deploys the bundled Redis. To use your own Redis instead, leave
   `redis` commented and set `externalRedis.host`.
2. Upgrade with the edited file, passing the routing key and a Redis password
   on the command line:

   ```bash
   helm upgrade versus-incident oci://ghcr.io/versuscontrol/charts/versus-incident \
     --namespace versus --reset-then-reuse-values \
     -f values-webhook-only.yaml \
     --set-string oncall.pagerduty.routingKey=$PAGERDUTY_ROUTING_KEY \
     --set-string redis.auth.password=$REDIS_PASSWORD
   ```

   `--reset-then-reuse-values` keeps the channel credentials from the install.
3. Wait until it is ready: `kubectl -n versus rollout status deploy/versus-incident`.

With on-call on, the Slack message also gets an **Acknowledge Alert** button. Other
providers are under [On Call](/oncall/on-call-introduction).

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

### Important parameters

| Parameter | Description | Default |
| --- | --- | --- |
| `replicaCount` | Number of pods. More than one needs `storage.type=postgres`; the install fails otherwise. | `1` |
| `image.tag` | Image tag, used as written. Empty means `v<appVersion>`. | `""` |
| `gatewaySecret` | Sign-in secret for the UI and the admin API. Empty: the chart generates one and keeps it on upgrade. | `""` |
| `gatewaySecretExistingSecret` | Read the sign-in secret from a Secret you create (key `gateway_secret`). | `""` |
| `config.publicHost` | The exact URL users open in the browser. See [Exposing the UI](#exposing-the-ui). | `""` |
| `storage.type` | `file` or `postgres`. | `file` |
| `storage.persistence.enabled` | Mount a PersistentVolumeClaim at `/app/data` (file storage). | `false` |
| `connectors.kubernetes.enabled` | Turn on the Kubernetes connector and its read-only role. | `false` |
| `agent.enable` | Turn on the SRE Agent. | `false` |
| `agent.mode` | `training`, `shadow` or `detect`. | `training` |
| `agent.sources` | Log sources the agent reads. | `[]` |
| `agent.ai.enable` | Let the agent call a model. | `false` |
| `agent.ai.provider` | `openai`, `deepseek`, `qwen`, `ollama`, `claude` or `gemini`. | `openai` |
| `agent.ai.apiKey` | Key for that provider, kept in the chart Secret. Pass it with `--set-string`. | `""` |
| `alert.<channel>.enable` | Turn on `slack`, `telegram`, `viber`, `email`, `msteams` or `lark`. | `false` |
| `templates.<channel>` | Replace the shipped template for `slack`, `telegram`, `email`, `msteams` or `lark`. | `{}` |
| `oncall.enable` | Turn on on-call escalation. Needs Redis. | `false` |
| `oncall.provider` | `aws_incident_manager`, `pagerduty`, `servicenow` or `incident_io`. | `aws_incident_manager` |
| `redis.enabled` | Deploy the bundled Redis. Or set `externalRedis.host`. | `false` |
| `extraEnv`, `extraEnvFrom` | Extra environment variables, for example credentials from your own Secrets. | `[]` |
| `ingress.enabled` | Create an Ingress for the UI. | `false` |

Every value, with comments, is in the chart's
[`values.yaml`](https://github.com/VersusControl/versus-incident/blob/main/helm/versus-incident/values.yaml).
Connector settings, including EKS, AKS and GKE access, are in
[Kubernetes Configuration](/agent/kubernetes/configuration). Each channel's
keys are on its page under [Notification Channels](/agent/channels).

### Storage

With `storage.type=file`, Versus keeps incidents, the pattern catalog and
settings under `/app/data`. Without persistence that is an `emptyDir`, lost on
every restart. To keep it:

```yaml
storage:
  type: file
  persistence:
    enabled: true
    size: 1Gi
    storageClassName: ""   # "" uses the cluster default
    # existingClaim: my-pvc
```

File storage runs one replica.

### PostgreSQL

For durable history, use PostgreSQL. It is also required for more than one
replica. Keep the connection string in a Secret:

```bash
kubectl -n versus create secret generic versus-postgres --from-literal=postgres_dsn='postgres://user:pass@host:5432/versus?sslmode=require'
```

```bash
helm upgrade versus-incident oci://ghcr.io/versuscontrol/charts/versus-incident \
  --namespace versus --reset-then-reuse-values \
  --set storage.type=postgres \
  --set storage.postgres.dsnExistingSecret=versus-postgres
```

See [PostgreSQL Storage](/configuration/postgres-storage) for creating the
database and role.

### Exposing the UI

To reach the UI without port-forward, turn on the Ingress and set
`config.publicHost` to the exact URL users open in the browser:

```yaml
config:
  publicHost: https://versus.example.com

ingress:
  enabled: true
  className: nginx
  hosts:
    - host: versus.example.com
      paths:
        - path: /
          pathType: Prefix
  tls:
    - secretName: versus-incident-tls
      hosts:
        - versus.example.com
```

Versus uses `publicHost` for links in alerts, secure cookies, and its
same-origin checks behind a proxy that rewrites the host or terminates TLS.

### GitOps and `helm template`

Without `gatewaySecret` or `gatewaySecretExistingSecret`, the chart generates
a sign-in secret on the first install and reads it back on every upgrade. Argo
CD, Flux and `helm template` cannot read it back, so they generate a new secret
on every render and the pod restarts on each sync. Use
`gatewaySecretExistingSecret`, as every command on this page does, or set
`gatewaySecret`.

The bundled Redis password works the same way. With `redis.enabled=true`, set
`redis.auth.password` or `redis.auth.existingSecret` (a Secret you create, key
`redis-password`). Otherwise the generated password changes on every sync and
restarts both the Versus and the Redis pods. If you use
`redis.auth.existingSecret` and rotate the password, restart both:

```bash
kubectl -n versus rollout restart deploy/versus-incident-redis deploy/versus-incident
```

### Upgrade

```bash
helm upgrade versus-incident oci://ghcr.io/versuscontrol/charts/versus-incident \
  --namespace versus --reset-then-reuse-values
```

`--reset-then-reuse-values` (Helm 3.14 or newer) keeps your values and `--set`
flags, including the channel credentials, and takes new defaults from the new
chart. On older Helm, pass your values file and `--set` flags again instead.
To pin a release, add `--version <chart version>`.

#### Upgrading from an older chart with the bundled Redis

The chart now runs its own Redis (the official `redis:7.4` image) instead of
the Bitnami subchart. If you used the bundled Redis (`redis.enabled=true`), the
upgrade needs four things:

- **Values.** In your values file, move `redis.master.persistence.*` to
  `redis.persistence.*`, and remove `redis.architecture`; it is no longer
  read. The chart refuses both old keys with a message that names them. The
  values stored in the release can still hold them, so clear them on the
  command line (see the command below). Or pass only `-f <your values file>`,
  with no reuse flag, and give all your values and `--set` flags again.
- **Image.** The bundled Redis runs the official `redis` image with the
  chart's tag. If your old values set `redis.image` (for example
  `redis.image.repository=bitnamilegacy/redis`, often with
  `global.security.allowInsecureImages`), the chart refuses the upgrade with a
  message that names `redis.image`. Remove `redis.image` from your values file
  and add `--set redis.image=null` to the command. Add it only in that case:
  if your old values never set `redis.image`, the flag also removes the
  chart's own image and the upgrade fails with `redis.image.tag is required`.
  `global.security.*` is not read by the chart and can stay.

  ```bash
  helm upgrade versus-incident oci://ghcr.io/versuscontrol/charts/versus-incident \
    --namespace versus --reset-then-reuse-values \
    -f <your values file> \
    --set redis.master=null --set redis.architecture=null
  # Only if your old values set redis.image:
  #   add --set redis.image=null
  ```

- **Data.** The new Redis starts empty on a new volume. Pending on-call
  escalations in the old Redis are not carried over; incidents and their
  history are not kept in Redis.
- **Clean-up.** The old volume is no longer used. Delete it when you no longer
  need it:

  ```bash
  kubectl -n versus delete pvc redis-data-versus-incident-redis-master-0
  ```

### Uninstall

```bash
helm uninstall versus-incident -n versus
```

```bash
kubectl delete namespace versus
```

Deleting the namespace removes the secrets you created and any
PersistentVolumeClaim.

## Next steps

- [Deploy on Kubernetes](/configuration/kubernetes): the same three setups
  with `kubectl apply`.
- [Configuration](/configuration/configuration): every config key and
  environment variable.
- [High Availability with Helm](/enterprise/ha/helm) _Enterprise_: several
  replicas on PostgreSQL.
