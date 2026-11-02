# Kubernetes Configuration

Connect the [Kubernetes dashboard](/agent/connectors/kubernetes) and its
read-only Chat and Analyze tools to your cluster. For per-cluster settings and
fleet navigation, see [Multi-Cluster](/agent/kubernetes/multi-cluster).

## Configuration

Configure `connectors.kubernetes` at the root of `config.yaml`, or in an optional
`connectors.yaml` file beside it. The examples below use the same root
`connectors:` wrapper in either file.

## Authentication

### In-cluster ServiceAccount

```yaml
connectors:
  kubernetes:
    auth:
      mode: in_cluster
```

The projected ServiceAccount token is read for every request, so rotation does
not require a restart. The projected cluster CA is used automatically.

### Rotating token file

```yaml
connectors:
  kubernetes:
    endpoint: https://api.example
    ca_data: ${KUBERNETES_CA_DATA}
    auth:
      mode: token_file
      token_file: /run/secrets/kubernetes-token
```

The bounded token file is read on every request.

### Static token

```yaml
connectors:
  kubernetes:
    endpoint: https://api.example
    ca_data: ${KUBERNETES_CA_DATA}
    auth:
      mode: token
      token: ${KUBERNETES_TOKEN}
```

Keep static tokens in an environment-backed Secret.

### Client certificate

```yaml
connectors:
  kubernetes:
    endpoint: https://api.example
    ca_data: ${KUBERNETES_CA_DATA}
    auth:
      mode: client_certificate
      client_certificate:
        certificate_file: /run/certs/client.crt
        key_file: /run/certs/client.key
```

Certificate files rotate without restart. Bounded base64 `certificate_data`
and `key_data` are also supported; do not combine file and inline forms.

### Safe kubeconfig

```yaml
connectors:
  kubernetes:
    auth:
      mode: kubeconfig
      kubeconfig:
        path: /run/kube/config
        context: production
```

The bounded parser accepts only static token, `tokenFile`, or client
certificate/key users. Relative paths resolve beside the kubeconfig.
Kubeconfig `exec` and legacy `auth-provider` entries are rejected with
instructions to use native `eks`, `aks`, or `gke` mode.

### EKS IAM

```yaml
connectors:
  kubernetes:
    endpoint: https://API_ID.eks.us-east-1.amazonaws.com
    ca_data: ${KUBERNETES_CA_DATA}
    auth:
      mode: eks
      eks:
        cluster_name: production
        region: us-east-1
        role_arn: arn:aws:iam::123456789012:role/versus-kubernetes-reader
        profile: ""
```

Create an EKS access entry (preferred) or compatible `aws-auth` mapping for the
IAM principal, then bind least-privilege Kubernetes RBAC.

For a complete setup showing the IRSA trust, EKS access-entry group mapping,
Kubernetes ClusterRole, and Helm values together, see
[Read EKS with IRSA](/examples/eks-irsa-kubernetes-reader).

### AKS workload identity

```yaml
connectors:
  kubernetes:
    endpoint: https://cluster.example.azmk8s.io
    ca_data: ${KUBERNETES_CA_DATA}
    auth:
      mode: aks
      aks:
        credential_mode: workload_identity
        environment: public
        server_id: api://AKS_SERVER_APP_ID
        tenant_id: TENANT_ID
        client_id: CLIENT_ID
        federated_token_file: /var/run/secrets/azure/tokens/azure-identity-token
```

`environment` is a closed enum: `public`, `government`, or `china`.
The federated token file is reopened, bounded, and permission-checked on every
refresh so projected-file rotation is observed. Projected read-only mode
`0644` is accepted; group/world write bits fail closed.

### AKS client secret

```yaml
connectors:
  kubernetes:
    endpoint: https://cluster.example.azmk8s.io
    ca_data: ${KUBERNETES_CA_DATA}
    auth:
      mode: aks
      aks:
        credential_mode: client_secret
        environment: public
        server_id: api://AKS_SERVER_APP_ID
        tenant_id: TENANT_ID
        client_id: CLIENT_ID
        client_secret: ${KUBERNETES_AKS_CLIENT_SECRET}
```

### AKS managed identity

```yaml
connectors:
  kubernetes:
    endpoint: https://cluster.example.azmk8s.io
    ca_data: ${KUBERNETES_CA_DATA}
    auth:
      mode: aks
      aks:
        credential_mode: managed_identity
        environment: public
        server_id: api://AKS_SERVER_APP_ID
        client_id: OPTIONAL_USER_ASSIGNED_IDENTITY_CLIENT_ID
```

Managed identity calls only Azure's fixed link-local IMDS endpoint. There is no
configurable token URL, device code, browser, refresh-token persistence, or
Azure CLI flow. The cluster must use Entra integration with the configured
server audience. Grant the identity cluster access and Kubernetes RBAC.

### GKE credentials

```yaml
connectors:
  kubernetes:
    endpoint: https://api.gke.example
    ca_data: ${KUBERNETES_CA_DATA}
    auth:
      mode: gke
      gke:
        credentials_file: /run/secrets/google/credentials.json
```

Leave `credentials_file` empty for Google Workload Identity through the fixed
metadata IP only. Versus does not read the well-known gcloud ADC file;
`GOOGLE_APPLICATION_CREDENTIALS` and operator-controlled `GCE_METADATA_HOST`
are rejected even when an explicit credentials file is configured.
Explicit credential files accept only Google `service_account` JSON or a
file-backed `external_account` using Google STS. Executable, URL, AWS, nested,
authorized-user, and impersonated-service-account credential sources are
rejected. External-account service account impersonation, when present, must
use the fixed `https://iamcredentials.googleapis.com/` API.
The provider requests only the `https://www.googleapis.com/auth/cloud-platform`
scope. Credential and external subject-token files are reopened, bounded, and
permission-checked on every refresh so projected-file rotation is observed;
projected read-only mode `0644` is accepted and group/world write bits fail
closed. Provide endpoint and CA separately;
this mode does not perform cluster discovery. Grant only the Google IAM needed
to access the cluster endpoint, then bind the principal through Kubernetes
RBAC.

### Change capture

`change_capture` defaults to `continuous`: Versus records supported changes
while running, even when nobody has the dashboard open. To opt out, set
`change_capture: demand` to collect on reads instead; idle periods and a cold
start can leave history gaps.

Add the setting to your existing single-cluster configuration:

```yaml
connectors:
  kubernetes:
    change_capture: continuous
```

With `multiple: true`, only `multiple` and `clusters` belong directly under
`kubernetes`. Each cluster entry owns its `change_capture`; there is no inherited
top-level setting. See the [per-cluster example](/agent/kubernetes/multi-cluster#setup)
for continuous capture in east and demand capture in west. Helm values use
`connectors.kubernetes.changeCapture` for a single cluster and `changeCapture`
inside each `connectors.kubernetes.clusters` entry for multiple clusters.
Restart Versus after configuration changes.

### Linked break-log source

In the cluster's **Agent** tab, link an already configured Loki, Elasticsearch,
SigNoz, Graylog, CloudWatch Logs or Splunk source. Verify its Kubernetes field
mapping and workload scope with **Test link**, then save. This extends
[Logs at break](auto-detect-issue.md#linked-source-and-ingestion-delays);
it does not change current/live logs.

| Saved cluster setting | Value |
| --- | --- |
| `break_logs.linked_source` | Name of the configured log source selected in the Agent tab. |
| `break_logs.fields` | Existing per-source Kubernetes field mappings. |
| `break_logs.ingest_delay` | Optional integer seconds, **0–3600** inclusive. Blank/absent uses the source default; `0` is an explicit zero-delay estimate. |

These are saved Agent settings, not `connectors.kubernetes` YAML keys.
Use **Ingestion delay override (seconds)** only when needed. Built-in estimates
are **30s** for Loki, **60s** for Elasticsearch/SigNoz/Graylog, and **120s** for
CloudWatch Logs/Splunk. Readiness is the selected window end plus this delay:
there is no ingestion watermark or guaranteed completion time. Background
settling is automatic when a source is linked; it has no separate enable setting.

### Break-log evidence limits

Raw break-log excerpts and identity metadata are bounded on both file and
PostgreSQL storage, separately from general Timeline history:

| Evidence | Limit |
| --- | --- |
| Raw snapshots and their metadata | **24 hours**, **2,000 snapshots**, **8 MiB serialized state including metadata**. |
| Identity history | **1 MiB** total reservation per evidence scope; **40 Pods** and **64 KiB** per namespace/root UID. |
| All identity metadata | **2 MiB** within the overall 8 MiB cap. |
| Break identity manifest | Up to **20 Pods per root occurrence**, retained within the same 24-hour horizon and metadata budget. |

Sampling or eviction of healthy and retired Pod identities leaves explicit
coverage gaps scoped to the affected root/namespace; loss markers are retained
for the 24-hour horizon. Under pressure, root markers may be coarsened to the
affected namespace. These budgets do not guarantee all-fleet identity coverage
or retention of every excerpt for a full day. Settled excerpts may outlast source
retention only within the same limits; they are not a long-term archive.

Each self-hosted deployment has one organization: OSS writes and reads `default`;
a licensed deployment writes its licensed organization and reads
`[licensed organization, default]`. The two concurrent background-settle slots
are shared across the deployment, not multiplied by organization or cluster.
Current Kubernetes namespace authority still applies to reads and settling.

### History storage

| Backend | Kubernetes evidence policy |
| --- | --- |
| File | Per evidence scope, up to **24 hours**, **5,000 history records**, and a **4 MiB serialized evidence-state budget**. Older history is pruned to fit; active issue state is protected. If protected state still cannot fit, the commit fails rather than deleting active state. Persist the data directory to survive restarts. |
| [PostgreSQL](/configuration/postgres-storage) | No application-imposed history age, total-record or total-byte retention limit, and no automatic Kubernetes-history pruning. Operators manage capacity and any explicit deletion/lifecycle policy. |
| Memory | Ephemeral: evidence is lost when the process stops. |

Bounded pages, query windows and per-operation payload limits constrain work,
not PostgreSQL history retention. Choosing a longer time range cannot recover
uncaptured or file-pruned evidence.

| Symptom | What to check |
| --- | --- |
| Configured continuous capture reports effective Demand | Check the reported reason. Projected ServiceAccount tokens must be verifiable against the cluster API server. Unverified rotating or cloud identities, and unsupported storage, fall back to demand; selecting continuous alone does not guarantee coverage. |
| History has gaps or is unavailable | Check capture mode, runtime uptime, current reader permissions, and the backend's history policy above. Revoked credentials stop capture; missing permissions leave incomplete history. Expand the history warnings rather than assuming nothing changed. |
| Multiple replicas need continuous history | Use shared PostgreSQL storage and the same connector configuration on current versions. Duplicate-free capture is verified for static readers and verified projected ServiceAccount readers, not every cloud identity. Confirm effective continuous capture on each replica. Stop all old capture instances before upgrading; do not run old and new capture together. |
| A large cluster responds slowly | Large inventories, including around 50,000 resources, can still be slow. Retry the read. Temporarily switching to demand may help reduce background work, but is not a guaranteed fix and can leave idle history gaps. |

## Kubernetes RBAC

Versus Enterprise `infrastructure:view` controls access to the Versus UI/API.
It does not grant Kubernetes permissions. Bind the connector identity inside
Kubernetes separately. For `in_cluster`, that identity is the pod
ServiceAccount. For `eks`, it is normally the Kubernetes group assigned by the
EKS access entry. This cluster-wide example covers discovery, resources, logs,
and metrics with read verbs only:

When `auth.mode: eks` is used with an IAM role, the binding subject is normally
the Kubernetes group configured on the EKS access entry, not the pod
ServiceAccount. The [EKS with IRSA example](/examples/eks-irsa-kubernetes-reader)
shows that mapping. The ServiceAccount binding below is for `in_cluster` mode.

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: versus-kubernetes-reader}
rules:
- apiGroups: [""]
  resources: [nodes, namespaces, pods, services, events, configmaps, serviceaccounts, secrets, persistentvolumeclaims, persistentvolumes]
  verbs: [get, list]
- apiGroups: [""]
  resources: [pods/log]
  verbs: [get]
- apiGroups: [apps]
  resources: [deployments, statefulsets, daemonsets, replicasets, controllerrevisions]
  verbs: [get, list]
- apiGroups: [batch]
  resources: [jobs, cronjobs]
  verbs: [get, list]
- apiGroups: [discovery.k8s.io]
  resources: [endpointslices]
  verbs: [get, list]
- apiGroups: [networking.k8s.io]
  resources: [ingresses, networkpolicies]
  verbs: [get, list]
- apiGroups: [autoscaling]
  resources: [horizontalpodautoscalers]
  verbs: [get, list]
- apiGroups: [policy]
  resources: [poddisruptionbudgets]
  verbs: [get, list]
- apiGroups: [storage.k8s.io]
  resources: [storageclasses]
  verbs: [get, list]
- apiGroups: [apiextensions.k8s.io]
  resources: [customresourcedefinitions]
  verbs: [get, list]
- apiGroups: [metrics.k8s.io]
  resources: [nodes, pods]
  verbs: [get, list]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: versus-kubernetes-reader
subjects:
  - kind: ServiceAccount
    name: versus-incident
    namespace: versus
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: versus-kubernetes-reader
```

The shipped role reads Secret metadata only. Resource reports project Secret
names, labels, and referenced key names; Secret values are never projected. The
grant is part of the role above, so remove `secrets` from your own role or
binding if you want Secrets invisible to Versus. Gateway API reads are also
optional: add `get,list` for `gateway.networking.k8s.io` `gateways` and
`httproutes`, or set `kubernetesReaderRBAC.gatewayAPI=true` in the chart, only on
clusters where those CRDs are installed.

For least privilege, replace `ClusterRole`/`ClusterRoleBinding` with a `Role`
and `RoleBinding` in each observed namespace, remove nodes/namespaces and node
metrics, and accept a partial cluster overview. API discovery itself may
still be visible, while forbidden resource reads are reported as partial.

### Actor credentials and RBAC

Dashboard, Chat, and Analyze reads use the reader identity above. Optional
actions use a separate identity; enabling actions does not give the reader
write permissions. Actions require an explicit `actions.auth.mode` and reject
`in_cluster` mode, which would reuse the pod ServiceAccount.

```yaml
connectors:
  kubernetes:
    endpoint: https://api.example
    ca_data: ${KUBERNETES_CA_DATA}
    auth:
      mode: token_file
      token_file: /run/secrets/kubernetes-reader-token
    actions:
      enable: true
      max_replicas: 20
      timeout: 15s
      auth:
        mode: token_file
        token_file: /run/secrets/kubernetes-actor-token
```

Bind the actor separately and grant only the verbs and resource types needed
for the operations you enable. The chart's `kubernetesActionsRBAC` settings
provide optional, separate actor ServiceAccount grants; mount that identity's
token for `actions.auth.token_file`. Reader RBAC and a successful dashboard
connection do not authorize action execution; action policy and approval
requirements still apply.

The optional chart actor grants are:

| Scope | Resources | Verbs |
| --- | --- | --- |
| Selected namespace | `apps`: deployments, deployments/scale, statefulsets, statefulsets/scale, daemonsets | `get`, `patch` |
| Selected namespace | `batch`: cronjobs | `get`, `patch` |
| Selected namespace | `batch`: jobs | `get`, `create` |
| Cluster-wide | Core nodes | `get`, `patch` |

Set `kubernetesActionsRBAC.enabled: true` only when these grants are appropriate.
`namespace` defaults to the release namespace; `serviceAccountName` defaults
to a separate chart-generated actor name. Use separately managed, narrower
bindings if you do not need every chart grant. These bindings do not grant
Pod exec or Secret reads.

## Connection options

These keys apply to the single connector or to each multi-cluster entry.

| YAML key | Purpose |
| --- | --- |
| `endpoint` | Kubernetes API base URL; supply it explicitly for native cloud authentication. |
| `cluster_id` | Stable cluster identity; single-cluster mode falls back to `default` when empty. Multi-cluster mode requires a unique ID. |
| `credential_id` | Credential identity used to separate cached cluster evidence. |
| `ca_file` / `ca_data` | Trusted CA file or base64 CA data; choose one form. |
| `server_name` | Trusted TLS certificate hostname when different from the endpoint. |
| `timeout` | Request duration, such as `10s`. |
| `discovery_ttl` | Discovery cache duration, such as `5m`. |
| `allow_private_networks` | Permit private API-server addresses. |
| `endpoint_cidrs` | Explicit allowed endpoint network ranges. |
| `allow_loopback` | Test-only loopback access; not for production. |
| `actions.enable` | Enable separately authenticated action clients; disabled by default. |
| `actions.max_replicas` | Replica ceiling for supported scale operations. |
| `actions.timeout` | Action request duration; when omitted, uses the connector timeout. |

## Helm values

Helm values use camelCase where the runtime YAML uses snake_case. For example,
`caData`, `clusterID`, `credentialID`, `discoveryTTL`, `allowPrivateNetworks`,
`endpointCIDRs`, `auth.tokenFile`, and `actions.maxReplicas` map to the keys
above. Native auth fields also use camelCase, such as `eks.clusterName`,
`eks.roleARN`, `aks.credentialMode`, and `gke.credentialsFile`.

```yaml
connectors:
  kubernetes:
    multiple: false
    auth:
      mode: in_cluster
kubernetesReaderRBAC:
  enabled: true
```

The chart defaults include `clusterID: default`, `timeout: 10s`,
`discoveryTTL: 5m`, `actions.enable: false`, `actions.maxReplicas: 20`, and
`actions.timeout: 15s`. Secret reads are metadata-only and already part of the
shipped reader role; remove the `secrets` grant to exclude them. Gateway API
grants use `kubernetesReaderRBAC.gatewayAPI: true` and must be enabled
deliberately. Keep credential values in Secrets or mounted files, not committed
values files. See
[Installing with Helm](/configuration/helm) and the complete
[EKS with IRSA example](/examples/eks-irsa-kubernetes-reader).

## Network and TLS policy

Production endpoints must use HTTPS. Public addresses are allowed by default.
Private managed APIs require `allow_private_networks: true` or narrowly scoped
`endpoint_cidrs`. DNS is resolved and every address checked at connect time;
loopback is test-only, while metadata, link-local, multicast, unspecified,
redirect, proxy, userinfo, query, and non-root base endpoints are rejected.
Use the cluster CA. TLS verification defaults to the endpoint hostname; set
`server_name` only when the certificate uses a different trusted DNS name.

## Security and troubleshooting

Kubeconfig supports server, CA file/data, token/tokenFile, client certificate/key
file/data, and context selection. It rejects insecure TLS, proxy URLs, basic
auth, impersonation, every exec command, and legacy auth-provider credentials.
Cloud credential HTTP clients have deadlines, bounded bodies, no redirects or
proxy inheritance, TLS verification, and fixed production endpoints.

The admin API and Kubernetes page return a safe error code, explanation, and
next action:

Pod-log diagnostics use the same safe codes, including
`tls_verification_failed`, `dns_resolution_failed`, and
`cluster_permission_denied`, without exposing raw upstream responses or causes.

| Code | What to check |
| --- | --- |
| `credential_unavailable` | Configure the selected auth mode. For EKS, provide IRSA, Pod Identity, environment credentials, or an explicitly selected static profile. |
| `cluster_authentication_failed` | Verify cloud identity, cluster access mapping, token audience, and credential expiry. |
| `cluster_permission_denied` | Grant the connector identity the required read-only Kubernetes RBAC. |
| `tls_verification_failed` | Verify `ca_data` or `ca_file` and `server_name` against the API-server certificate. |
| `dns_resolution_failed` | Resolve the configured endpoint from the Versus container. |
| `connection_failed` | Check endpoint CIDRs, private-network access, routes, firewall rules, and security groups. |
| `request_timeout` | Check API-server reachability and control-plane latency before increasing the timeout. |
| `response_too_large` or `operation_budget_exhausted` | Narrow the namespace, resource category, selectors, search query, or result limit. |
| `connector_configuration_invalid` | Check the endpoint, network policy, CA selection, and auth-mode fields, then restart Versus. |