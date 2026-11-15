# Kubernetes examples

Three ways to run Versus in a cluster. Each has a Helm values file and the
same install as one manifest for `kubectl apply`, rendered from the chart.

| Path | What runs | Helm values | One-file manifest |
|---|---|---|---|
| Kubernetes in-cluster | The Kubernetes connector reads the cluster Versus runs in, with a read-only ClusterRole. Agent and channels off. | `values-kubernetes-in-cluster.yaml` | `k8s-connector-in-cluster-enabled.yaml` |
| Agent only | The agent reads one Loki log source in training mode, then detect mode. Connector off. | `values-agent-only.yaml` | `agent-only.yaml` |
| Webhook only | Incoming alerts on `/api/incidents`, a custom Slack template, Slack and Teams channels, on-call ready to turn on. Agent and connector off. | `values-webhook-only.yaml` | `webhook-only.yaml` |

Every path installs into the namespace `versus` and reads the sign-in secret
from a Secret named `versus-incident` that you create first:

```bash
kubectl create namespace versus --save-config
kubectl -n versus create secret generic versus-incident --from-literal=gateway_secret=$(openssl rand -hex 32)
```

`--save-config` lets the one-file manifests, which also declare the
namespace, apply over it without a warning.

## Helm

The chart comes from the OCI registry; the values files are in this folder.
You need Git and Helm 3. Clone the repository and go into this folder, and run
the Helm commands from there:

```bash
git clone https://github.com/VersusControl/versus-incident.git
cd versus-incident/examples/kubernetes
```

```bash
helm install versus-incident oci://ghcr.io/versuscontrol/charts/versus-incident \
  --namespace versus \
  -f values-agent-only.yaml \
  --set gatewaySecretExistingSecret=versus-incident
```

Swap in the values file for your path. Webhook only also needs the channel
credentials at install time; the values file never holds them:

```bash
helm install versus-incident oci://ghcr.io/versuscontrol/charts/versus-incident \
  --namespace versus \
  -f values-webhook-only.yaml \
  --set gatewaySecretExistingSecret=versus-incident \
  --set-string alert.slack.token=$SLACK_BOT_TOKEN \
  --set-string alert.slack.channelId=$SLACK_CHANNEL_ID \
  --set-string alert.msteams.powerAutomateUrl=$TEAMS_POWER_AUTOMATE_URL
```

## kubectl

```bash
kubectl apply -f https://raw.githubusercontent.com/VersusControl/versus-incident/main/examples/kubernetes/k8s-connector-in-cluster-enabled.yaml
```

Use `agent-only.yaml` or `webhook-only.yaml` the same way. `webhook-only.yaml`
leaves out the channel secret; create it before applying:

```bash
kubectl -n versus create secret generic versus-incident-secrets \
  --from-literal=slack_token=$SLACK_BOT_TOKEN \
  --from-literal=slack_channel_id=$SLACK_CHANNEL_ID \
  --from-literal=msteams_power_automate_url=$TEAMS_POWER_AUTOMATE_URL
```

The manifests are generated from the Helm chart and the values files by
`scripts/render_kubernetes_manifest.sh`; do not edit them by hand. CI fails if
they are out of date (`scripts/render_kubernetes_manifest.sh --check`).
