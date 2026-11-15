# Kubernetes: one-file install

`k8s-connector-in-cluster-enabled.yaml` runs Versus in an existing cluster, in the namespace
`versus`, reading the cluster with a read-only role. Create the namespace and
the sign-in secret first, then apply it:

```bash
kubectl create namespace versus
kubectl -n versus create secret generic versus-incident --from-literal=gateway_secret=$(openssl rand -hex 32)
kubectl apply -f https://raw.githubusercontent.com/VersusControl/versus-incident/v1.4.36/examples/kubernetes/k8s-connector-in-cluster-enabled.yaml
```

The file is generated from the Helm chart by `scripts/render_kubernetes_manifest.sh`; do not edit it by hand.
