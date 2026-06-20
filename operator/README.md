# Versus Incident Operator

A Kubernetes operator (kubebuilder/controller-runtime, group `ops.versuscontrol.io`)
that manages Versus Incident deployments declaratively. One `VersusIncident`
custom resource is reconciled into a **ConfigMap + Deployment + Service**, with
owner references so deleting the CR garbage-collects everything.

It is an alternative to the Helm chart: same app, but driven by a CR the
controller continuously reconciles (self-healing, status reporting) instead of
a one-shot `helm install`.

## Layout (standard kubebuilder)

```
operator/
├── api/v1alpha1/           # VersusIncident types + generated DeepCopy
├── internal/controller/    # the reconciler
├── cmd/main.go             # manager entrypoint
├── config/
│   ├── crd/bases/          # generated CRD
│   ├── rbac/               # generated ClusterRole + SA + binding
│   ├── manager/            # operator Deployment + Namespace
│   └── samples/            # sample VersusIncident CR
├── Dockerfile  Makefile  PROJECT
```

## Quick start (minikube)

```bash
# 0) build the operator image into minikube's docker daemon
eval $(minikube docker-env)
make -C operator docker-build          # → versus-incident-operator:local

# 1) install CRD + RBAC + run the manager in-cluster
make -C operator deploy

# 2) create the app namespace + the secret the CR references
kubectl create namespace versus
kubectl -n versus create secret generic versus-operator-secrets \
  --from-literal=gateway_secret="$(openssl rand -hex 32)" \
  --from-literal=telegram_bot_token='<bot-token>' \
  --from-literal=telegram_chat_id='<chat-id>' \
  --from-literal=agent_ai_api_key='<google-api-key>'

# 3) create a VersusIncident — the operator builds the workload
kubectl apply -f operator/config/samples/ops_v1alpha1_versusincident.yaml

# 4) observe
kubectl get versusincident -n versus          # short name: vi
kubectl get deploy,svc,cm -n versus -l app.kubernetes.io/managed-by=versus-incident-operator
```

Deleting the CR removes the Deployment/Service/ConfigMap automatically:

```bash
kubectl delete vi demo -n versus
```

## CRD shape

```yaml
apiVersion: ops.versuscontrol.io/v1alpha1
kind: VersusIncident
spec:
  image: { repository, tag, pullPolicy }
  replicas: 1
  gatewaySecretName: <secret with key gateway_secret>
  telegram:
    enabled: true
    secretName: <secret with telegram_bot_token, telegram_chat_id>
  agent:
    enable: true
    mode: detect              # training | shadow | detect
    pollInterval: 15s
    ai:
      enable: true
      provider: gemini        # openai | gemini (maps to the endpoint internally)
      model: gemini-2.5-flash-lite
      apiKeySecretName: <secret with agent_ai_api_key>
    sources:                  # mirrors agent_sources.yaml
      - name: demo-app
        type: file            # file | loki | elasticsearch
        enable: true
        file: { path: /app/data/app.log, fromBeginning: true }
status:
  readyReplicas: <n>
  conditions: [ { type: Ready, ... } ]
```

Secrets are **referenced, never embedded** in the CR. The rendered in-pod
`config.yaml` includes the full agent detection config (regex / redaction /
miner / catalog / service_patterns) so the agent matches out of the box.

## Regenerate after API changes

```bash
make -C operator manifests generate   # CRD + RBAC + DeepCopy
make -C operator build
```
