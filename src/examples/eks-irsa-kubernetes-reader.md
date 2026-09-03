# Read EKS with IRSA

This example runs Versus in EKS and gives its Kubernetes connector read-only
access to an EKS cluster without storing AWS access keys or installing the AWS
CLI in the container.

IRSA and Kubernetes RBAC solve different parts of the connection:

| Layer | Purpose |
|---|---|
| IRSA | Gives the Versus pod temporary AWS credentials for an IAM role. |
| EKS IAM token | Versus signs `GetCallerIdentity` in Go and sends the resulting `k8s-aws-v1` token to EKS. |
| EKS access entry | Makes the IAM role an authenticated principal in the target cluster. |
| EKS access policy or Kubernetes RBAC | Grants that principal permission to read cluster resources. |
| Versus Enterprise RBAC | Controls which signed-in Versus users may use Kubernetes data through the UI, API, Chat, or Analyze. |

IRSA alone does **not** grant permission to read Pods, Deployments, or logs.
Kubernetes RBAC alone does **not** give the pod an AWS identity accepted by EKS.
Both sides are required.

> If Versus reads the same cluster where it runs, `auth.mode: in_cluster` is
> simpler and does not need IRSA or an EKS access entry. Use this EKS/IRSA flow
> when you specifically want IAM-authenticated access, including access to a
> separate EKS cluster.

## Prerequisites

- An EKS cluster where the Versus pod runs, with an IAM OIDC provider enabled.
- The target EKS cluster endpoint and base64-encoded certificate authority data.
- The target cluster authentication mode set to `API` or `API_AND_CONFIG_MAP`
  when using an EKS access entry.
- Permission to create an IAM role, an EKS access entry, and Kubernetes RBAC.
- AWS CLI v2.12.3 or newer on the administration workstation.
- The Versus Helm chart.

The examples use:

```text
Versus namespace:       versus
Versus ServiceAccount:  versus-incident
IRSA role:              arn:aws:iam::123456789012:role/versus-kubernetes-reader
Target cluster:         production
AWS Region:             us-east-1
Kubernetes group:       versus-kubernetes-readers
```

Replace these values with yours.

## 1. Create the IRSA role

Create an IAM role whose trust policy allows only the Versus ServiceAccount to
call `sts:AssumeRoleWithWebIdentity`.

Use the OIDC provider of the cluster **where Versus runs**. If Versus reads a
different EKS cluster, the source cluster supplies the IRSA identity and the
target cluster supplies the access entry and Kubernetes RBAC.

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "Federated": "arn:aws:iam::123456789012:oidc-provider/oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE"
      },
      "Action": "sts:AssumeRoleWithWebIdentity",
      "Condition": {
        "StringEquals": {
          "oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE:aud": "sts.amazonaws.com",
          "oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE:sub": "system:serviceaccount:versus:versus-incident"
        }
      }
    }
  ]
}
```

The `sub` condition is the important boundary: only the named ServiceAccount in
the `versus` namespace can assume the role.

The role does not need broad EKS administration permissions. Versus uses its
credentials only to create a signed EKS authentication token. If the YAML
configuration sets `auth.eks.role_arn` to a second role, the IRSA role also needs
`sts:AssumeRole` for that exact role.

## 2. Create the annotated ServiceAccount

Create the ServiceAccount before installing the chart:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: versus-incident
  namespace: versus
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/versus-kubernetes-reader
```

For example, from an administrator workstation:

```bash
kubectl create namespace versus
kubectl apply -f versus-serviceaccount.yaml
```

EKS injects these values into the pod automatically:

```text
AWS_ROLE_ARN
AWS_WEB_IDENTITY_TOKEN_FILE
```

## 3. Map the IAM role into EKS

Create an EKS access entry on the **target** cluster, then associate the AWS
managed `AmazonEKSViewPolicy` for quick read access. Run these commands from your
administration workstation; the AWS CLI is not installed or executed inside the
Versus container.

Set the values once:

```bash
export EKS_CLUSTER=production
export AWS_REGION=us-east-1
export VERSUS_ROLE_ARN=arn:aws:iam::123456789012:role/versus-kubernetes-reader
```

Check whether the target cluster accepts access entries:

```bash
aws eks describe-cluster \
  --name "$EKS_CLUSTER" \
  --region "$AWS_REGION" \
  --query 'cluster.accessConfig.authenticationMode' \
  --output text
```

If the result is `CONFIG_MAP`, enable access entries while retaining existing
`aws-auth` mappings:

```bash
aws eks update-cluster-config \
  --name "$EKS_CLUSTER" \
  --region "$AWS_REGION" \
  --access-config authenticationMode=API_AND_CONFIG_MAP

aws eks wait cluster-active \
  --name "$EKS_CLUSTER" \
  --region "$AWS_REGION"
```

Create the access entry:

```bash
aws eks create-access-entry \
  --cluster-name "$EKS_CLUSTER" \
  --region "$AWS_REGION" \
  --principal-arn "$VERSUS_ROLE_ARN" \
  --type STANDARD
```

If that role already has an access entry, do not create a second one. Continue
with the policy association or inspect it first with:

```bash
aws eks describe-access-entry \
  --cluster-name "$EKS_CLUSTER" \
  --region "$AWS_REGION" \
  --principal-arn "$VERSUS_ROLE_ARN"
```

Associate the managed view policy at cluster scope:

```bash
aws eks associate-access-policy \
  --cluster-name "$EKS_CLUSTER" \
  --region "$AWS_REGION" \
  --principal-arn "$VERSUS_ROLE_ARN" \
  --policy-arn arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy \
  --access-scope type=cluster
```

This is the quickest way to verify authentication and basic resource reads. It
does not change the IAM role and does not grant AWS administration permissions.
The access policy is evaluated by EKS for Kubernetes API requests made by that
principal.

`AmazonEKSViewPolicy` is intentionally general-purpose. Depending on the
cluster and installed APIs, Versus may report pod logs or some cluster-scoped,
metrics, storage, Gateway API, or custom-resource reads as partial or forbidden.
That is acceptable for a connection check. The Security section below replaces
the managed policy with a connector-specific Kubernetes role.

Check the association:

```bash
aws eks list-associated-access-policies \
  --cluster-name "$EKS_CLUSTER" \
  --region "$AWS_REGION" \
  --principal-arn "$VERSUS_ROLE_ARN"
```

For older clusters that do not use EKS access entries, use an `aws-auth` role
mapping and the custom Kubernetes group/RBAC flow in the Security section.

## 4. Configure the Helm release

Because this example uses EKS IAM authentication, do not enable the chart's
`kubernetesReaderRBAC`. That option binds permissions directly to the pod
ServiceAccount and is intended for `auth.mode: in_cluster`. The quick path uses
the EKS managed access policy; the hardened path below uses an access-entry group.

Create `values-eks-irsa.yaml`:

```yaml
serviceAccount:
  create: false
  name: versus-incident

kubernetesReaderRBAC:
  enabled: false

agent:
  enable: true
  ai:
    enable: true
  tools:
    kubernetes:
      clusterID: production
      endpoint: https://API_ID.eks.us-east-1.amazonaws.com
      caData: LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0t...
      endpointCIDRs:
        - 10.20.0.0/16
      auth:
        mode: eks
        eks:
          clusterName: production
          region: us-east-1
          roleARN: ""
          profile: ""
```

`caData` is the target cluster's base64-encoded certificate-authority data. It is
stored in the chart Secret and expanded into `tools.yaml`; it is not written as
plaintext into the ConfigMap.

Leave `roleARN` empty when the IRSA role itself is the principal in the EKS access
entry. Set it only for an intentional second-hop role, and grant the IRSA role
`sts:AssumeRole` for that exact ARN.

For a public EKS endpoint, remove `endpointCIDRs`. For a private endpoint, set
`endpointCIDRs` to the actual control-plane address range. Use
`allowPrivateNetworks: true` only when maintaining a narrow CIDR list is not
possible, because it permits any private destination that the endpoint hostname
resolves to.

Install Versus:

```bash
helm upgrade --install versus-incident ./helm/versus-incident \
  --namespace versus \
  --values values-eks-irsa.yaml
```

The Helm command runs on your administration workstation or deployment system.
No AWS CLI or cloud credential plugin is installed or executed inside the Versus
container.

## 5. Security: replace the managed policy with custom read-only RBAC

After the connector is working, replace `AmazonEKSViewPolicy` with a Kubernetes
group and a role containing only the resources Versus reads. This makes the
authorization reviewable in your cluster and lets you omit optional resource
families you do not use.

First remove the quick-access policy:

```bash
aws eks disassociate-access-policy \
  --cluster-name "$EKS_CLUSTER" \
  --region "$AWS_REGION" \
  --principal-arn "$VERSUS_ROLE_ARN" \
  --policy-arn arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy
```

Map the same access entry to a Kubernetes group:

```bash
aws eks update-access-entry \
  --cluster-name "$EKS_CLUSTER" \
  --region "$AWS_REGION" \
  --principal-arn "$VERSUS_ROLE_ARN" \
  --kubernetes-groups versus-kubernetes-readers
```

Apply the connector-specific role and bind it to that group:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: versus-kubernetes-reader
rules:
  - apiGroups: [""]
    resources: [nodes, namespaces, pods, services, events, configmaps, persistentvolumeclaims, persistentvolumes]
    verbs: [get, list]
  - apiGroups: [""]
    resources: [pods/log]
    verbs: [get]
  - apiGroups: [apps]
    resources: [deployments, statefulsets, daemonsets, replicasets]
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
  - kind: Group
    name: versus-kubernetes-readers
    apiGroup: rbac.authorization.k8s.io
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: versus-kubernetes-reader
```

Save that manifest as `versus-kubernetes-rbac.yaml`, then apply it:

```bash
kubectl apply -f versus-kubernetes-rbac.yaml
```

The role grants only `get` and `list`, plus `get` on `pods/log`. It grants no
create, update, patch, delete, exec, proxy, rollout, or Helm permission.

Secrets are deliberately omitted. ConfigMaps are readable, but Versus projects
only key names and metadata, never values. Add Secret reads only if referenced
Secret key names are required in topology. Add Gateway API `gateways` and
`httproutes` only when those CRDs are installed.

For namespace-only access, replace the ClusterRoleBinding with RoleBindings in
approved namespaces and remove cluster-scoped resources. Versus reports denied
cluster-wide reads as partial evidence instead of treating them as an empty
healthy result.

Verify the custom binding by refreshing `/agent/kubernetes`. Normal resources
should remain visible; anything intentionally excluded should appear as partial
or forbidden evidence.

## Same-cluster alternative

When Versus reads only the cluster where it runs, prefer:

```yaml
agent:
  tools:
    kubernetes:
      auth:
        mode: in_cluster

kubernetesReaderRBAC:
  enabled: true
```

In this simpler mode, the chart binds Kubernetes read permissions directly to
the Versus ServiceAccount. IRSA and an EKS access entry are unnecessary.

## Next

- [Kubernetes connector reference](/agent/tools/kubernetes)
- [Helm configuration](/configuration/helm)
