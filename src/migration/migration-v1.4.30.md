# Migration to v1.4.30

## Breaking change: Kubernetes configuration moves to connectors

Kubernetes is no longer configured under tools. Move the entire Kubernetes
block from inline `agent.tools.kubernetes` or sibling `tools.yaml`'s
`tools.kubernetes` to root `connectors.kubernetes`. Other tool configuration
stays where it is. There is no compatibility alias or automatic migration.

**Before: inline configuration in `config.yaml`**

```yaml
agent:
  tools:
    kubernetes:
      endpoint: https://kubernetes.example.invalid
      ca_file: /app/credentials/cluster-ca.crt
      auth:
        mode: token_file
        token_file: /app/credentials/reader-token
```

**After: root configuration in `config.yaml`**

```yaml
connectors:
  kubernetes:
    endpoint: ${KUBERNETES_ENDPOINT}
    ca_file: /app/credentials/cluster-ca.crt
    auth:
      mode: token_file
      token_file: /app/credentials/reader-token
    actions:
      enable: false
```

For a split configuration, the old sibling `tools.yaml` wrapper was:

```yaml
tools:
  kubernetes:
    endpoint: https://kubernetes.example.invalid
    ca_file: /app/credentials/cluster-ca.crt
    auth:
      mode: token_file
      token_file: /app/credentials/reader-token
```

Move that block into **`connectors.yaml` beside the main config file**, using
the same `connectors:` wrapper as the after example. Remove `kubernetes` from
`tools.yaml`; keep its other tool entries.

- Without a sibling `connectors.yaml`, the inline root block is used.
- When present, the sibling file **replaces the entire inline connectors
  configuration**, rather than merging it. Include every required setting
  there; even an empty `connectors: {}` replaces the inline block.
- Both locations expand `${VAR}` scalar values once from the process
  environment, including credentials and boolean/integer settings. Use explicit
  placeholders; automatic `CONNECTORS_*` environment overrides do not replace
  this connector decoding path.
- The sibling file accepts only the top-level `connectors` key. Unknown connector
  fields, incorrect types, null blocks, and multiple YAML documents are rejected.

Legacy `tools.kubernetes` and `agent.tools.kubernetes` keys are rejected even
when empty or null, including dotted/case-insensitive forms. The application
rejects them in the main config and sibling tools file; Helm rejects the
presence of `agent.tools.kubernetes`. The migration error is:

```text
Kubernetes configuration must use connectors.kubernetes
```

Connector decoding errors use safe messages such as
`invalid connectors configuration`, without echoing submitted credentials.
An invalid inline block still fails before a sibling file can replace it.
Remove old keys from all configuration and values overlays before upgrading,
then restart Versus after updating configuration and mounts.

## Keep reader and actor credentials separate

`connectors.kubernetes.auth` supplies read access for the explorer, logs, and
investigation tools. `connectors.kubernetes.actions.auth` supplies the separate
write identity for Kubernetes actions; it does not inherit reader credentials.
Keep the reader read-only, including `get` on `pods/log` for the Pod log viewer.

Actions default to disabled. If retaining enabled actions, move their settings
with the connector and supply an explicit actor authentication mode and a
separate, least-privilege credential, for example:

```yaml
connectors:
  kubernetes:
    endpoint: ${KUBERNETES_ENDPOINT}
    ca_file: /app/credentials/cluster-ca.crt
    auth:
      mode: token_file
      token_file: /app/credentials/reader-token
    actions:
      enable: true
      auth:
        mode: token_file
        token_file: /app/credentials/actor-token
```

An enabled actor without `auth.mode` is rejected. Actor `in_cluster` mode is
also rejected: do not grant write permissions to the reader's Pod ServiceAccount.
For actor modes using an explicit endpoint, retain the connector endpoint and
CA settings; actor `kubeconfig` mode obtains these from its own kubeconfig.
Existing chat approvals and the Tools Actions setting remain in effect;
moving configuration does not bypass them.

## Helm values and mounts

Move `agent.tools.kubernetes` to **root `connectors.kubernetes`** in Helm values.
Helm uses camelCase field names, unlike the application's snake_case YAML:

```yaml
connectors:
  kubernetes:
    endpoint: https://kubernetes.example.invalid
    caFile: /app/credentials/cluster-ca.crt
    auth:
      mode: token_file
      tokenFile: /app/credentials/reader-token
    actions:
      enable: false
```

Preserve other settings under the new root using their Helm names, including
`clusterID`, `credentialID`, `discoveryTTL`, `allowPrivateNetworks`,
`endpointCIDRs`, and `actions.maxReplicas`. If actions are enabled, configure
their independent identity under `connectors.kubernetes.actions.auth`.

The chart renders `connectors.yaml` into its ConfigMap and mounts it at
`/app/config/connectors.yaml` beside `/app/config/config.yaml`, even when the
agent is disabled. `tools.yaml` remains a separate agent-only mount. For custom
manifests or container deployments, add the connector-file mount explicitly.
Credential file paths must refer to files mounted inside the Versus container;
the connector-file mount does not mount token, CA, or kubeconfig files for you.

For inline Helm credentials, the chart stores reader and actor material in
separate Secret keys and exposes it through `KUBERNETES_*` and
`KUBERNETES_ACTOR_*` environment placeholders in the generated connector file,
not literal credentials in the ConfigMap. Do not commit credential values.

## Kubernetes drawer workflow

The resource drawer has **Logs only for Pods** and no **Actions** tab. Select a
Pod for the live log viewer: choose a container, time window, or tail count;
pause/resume, filter, copy, or download the displayed scrubbed logs. Previous
container logs are a finite read, not a live follow stream. Continue proposing
and approving Kubernetes actions through chat with the existing action controls.
