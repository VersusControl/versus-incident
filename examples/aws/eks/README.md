# AWS on EKS: IRSA, Pod Identity and FinOps

Run Versus on EKS and let it read AWS with the pod's own IAM role, no access
key. Start with one account, add more accounts through assume role, then turn
on FinOps. The guide that walks through every step, with the expected output,
is [AWS on EKS](../../../src/examples/aws-eks-irsa-pod-identity.md).

```text
examples/aws/eks/
├── README.md
├── setup.env.example                     # account IDs, cluster, region, namespace; no secrets
├── iam/
│   ├── irsa-trust-policy.json            # Path A: versus-app trusts the cluster's OIDC provider
│   ├── pod-identity-trust-policy.json    # Path B: versus-app trusts pods.eks.amazonaws.com
│   ├── versus-reader-policy.json         # read-only access: on versus-app, and on versus-reader
│   ├── assume-reader-policy.json         # lets versus-app assume versus-reader in each account
│   ├── reader-trust-policy.json          # versus-reader trusts versus-app, with the external ID
│   ├── cost-reads-policy.json            # FinOps: cost reads, for an account that reads its own cost
│   ├── versus-cost-reader-policy.json    # FinOps: versus-cost-reader in the management account
│   └── assume-cost-reader-policy.json    # FinOps: lets versus-app assume versus-cost-reader
├── k8s/
│   ├── serviceaccount-irsa.yaml          # Path A: ServiceAccount with the role annotation
│   └── serviceaccount-pod-identity.yaml  # Path B: plain ServiceAccount
└── helm/
    ├── values-single-account.yaml        # one account, IRSA or Pod Identity
    ├── values-multi-account.yaml         # + Production and Staging through versus-reader
    ├── values-finops.yaml                # + Cost Explorer reads, team tag, budgets
    └── values-finops-organization.yaml   # + the Organization's cost through the management account
```

The files use `${VAR}` placeholders. `envsubst` fills them from
`setup.env`, so you never edit a policy or a values file by hand.

## Order

| Step | What you do | Files |
| --- | --- | --- |
| 1 | Copy this folder, fill in `setup.env`, render the Kubernetes and Helm files | `setup.env.example` |
| 2a | **IRSA:** create `versus-app`, the annotated ServiceAccount, install | `iam/irsa-trust-policy.json`, `iam/versus-reader-policy.json`, `k8s/serviceaccount-irsa.yaml`, `helm/values-single-account.yaml` |
| 2b | **Pod Identity:** install the agent, create `versus-app`, the association, install | `iam/pod-identity-trust-policy.json`, `iam/versus-reader-policy.json`, `k8s/serviceaccount-pod-identity.yaml`, `helm/values-single-account.yaml` |
| 3 | **More accounts:** external ID Secret, `versus-reader` in each account, upgrade | `iam/assume-reader-policy.json`, `iam/reader-trust-policy.json`, `iam/versus-reader-policy.json`, `helm/values-multi-account.yaml` |
| 4 | **FinOps:** cost reads, then upgrade | `iam/cost-reads-policy.json` or `iam/versus-cost-reader-policy.json` + `iam/assume-cost-reader-policy.json`, `helm/values-finops.yaml`, `helm/values-finops-organization.yaml` |

Do 2a or 2b, not both.

## Quick start

You need the AWS CLI v2, `kubectl`, Helm 3.8 or newer, `envsubst` (part of
GNU gettext; `brew install gettext` on macOS), and `eksctl` for IRSA. The
chart comes from your copy of the repository, on the `dev` branch.

```bash
git clone -b dev https://github.com/VersusControl/versus-incident.git
cp -R versus-incident/examples/aws/eks ~/versus-aws-eks
cd ~/versus-aws-eks
cp setup.env.example setup.env      # then replace every <...> value
set -a; . ./setup.env; set +a
mkdir -p rendered
for f in k8s/*.yaml helm/*.yaml; do envsubst < "$f" > "rendered/$(basename "$f")"; done
```

Then follow the guide from **Path A** or **Path B**. Run every command from
`~/versus-aws-eks`, and load `setup.env` again in each new terminal.

Helm values that still hold a `<...>` placeholder stop the install with
`accountId="<...>" is not a 12-digit AWS account ID`.
