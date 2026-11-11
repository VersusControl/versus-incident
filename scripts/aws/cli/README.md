# versus-reader role from the AWS CLI

This directory creates the same `versus-reader` role as the CloudFormation
template in `../versus-reader.yaml` and the Terraform module in
`../terraform/versus-reader`, using only the AWS CLI and three JSON policy
files. Use it when the CLI, not a stack or a pipeline, is how you change IAM in
this account.

The role is identical in effect:

- `trust-policy.json` allows `sts:AssumeRole` only for the exact Versus role ARN
  and only when the caller presents the matching External ID. The principal and
  the External ID are `__VERSUS_PRINCIPAL_ARN__` and `__EXTERNAL_ID__`
  placeholders; nothing in the files is a credential.
- `permissions-policy.json` is the template's inline policy:
  `versus-reader-metadata-reads` holds the explicit allow list plus both deny
  statements (`DenyDataPlaneAndSecretReads`, `DenyBillingAccountSurfaces`).
  Optional-data actions are not in it.
- `optional-data-policy.json` is the `OptionalDataReads` allow list (cost
  reads from Cost Explorer, AWS Budgets, Cost Optimization Hub and Compute
  Optimizer, plus Security Hub findings, AWS Config advanced queries and
  Trusted Advisor), attached only when you ask for it.

## The six commands

Run these from this directory. Set `AWS_PROFILE` and `AWS_REGION` for the
account that will hold the role. The caller needs `iam:CreateRole` or
`iam:UpdateAssumeRolePolicy`, `iam:PutRolePolicy`, `iam:GetRole` and
`iam:GetRolePolicy`.

1. Confirm you are pointed at the right account:

   ```bash
   aws sts get-caller-identity
   ```

2. Create the role. Pass the exact Versus **role** ARN from the connect wizard
   and the External ID it shows. The account root ARN is refused, and so is any
   principal that is not an IAM role:

   ```bash
   ./create-versus-reader.sh \
     arn:aws:iam::123456789012:role/versus-app \
     '<external-id>'
   ```

   To keep the External ID out of your shell history, pass `-` instead of the
   value and the script prompts for it without echo:

   ```bash
   ./create-versus-reader.sh arn:aws:iam::123456789012:role/versus-app -
   ```

   The script fills the placeholders into a temporary trust policy, creates
   `versus-reader` with `max-session-duration 3600`, and attaches
   `versus-reader-metadata-reads`. If the role already exists it updates the
   trust policy with `update-assume-role-policy` instead of failing, so
   rotating the principal or the External ID is the same command again.

3. Or create the role with the optional policy attached, for cost and posture
   data. Cost Explorer is billed per request by AWS:

   ```bash
   ./create-versus-reader.sh \
     arn:aws:iam::123456789012:role/versus-app \
     '<external-id>' \
     --optional-data
   ```

   Running without `--optional-data` later does not detach the optional policy;
   the script prints the `delete-role-policy` command if it finds one.

4. Read the role ARN to paste into the Versus connect wizard:

   ```bash
   aws iam get-role --role-name versus-reader --query 'Role.Arn' --output text
   ```

5. Test access from the Versus principal, not from this account. The script
   prints this command with the real role ARN; run it where the Versus role
   credentials are available:

   ```bash
   aws sts assume-role \
     --role-arn arn:aws:iam::123456789012:role/versus-reader \
     --role-session-name versus-verify \
     --external-id '<external-id>'
   ```

   Success is `Credentials` in the response. `AccessDenied` means the ARN or
   the External ID does not match the trust policy; the role name and the
   condition are the only moving parts.

6. Remove the role when access is revoked. Delete the inline policies first;
   IAM refuses to delete a role that still has them:

   ```bash
   aws iam delete-role-policy --role-name versus-reader --policy-name versus-reader-metadata-reads
   aws iam delete-role-policy --role-name versus-reader --policy-name versus-reader-optional-data
   aws iam delete-role --role-name versus-reader
   ```

   The second `delete-role-policy` fails if you never enabled optional data;
   that is safe to ignore.

## What the script refuses

| Input | Result |
|---|---|
| `arn:aws:iam::123456789012:root` | Refused: trusting an account root delegates the role to every principal in the Versus account |
| `arn:aws:iam::123456789012:user/versus` | Refused: the principal must be an exact IAM role ARN |
| An External ID shorter than 16 characters, longer than 1224, or containing a space | Refused before any AWS call |
| No AWS CLI, or no caller identity | Refused before any AWS call |

The External ID is never printed. It is written only to the temporary trust
policy file, which a trap removes when the script exits. Backslashes and double
quotes are escaped for JSON, so any printable ASCII External ID round-trips
unchanged.

The script has no `--force`, no wildcard principal and no delete or
`iam:PassRole` call. The only writes it makes are `CreateRole`,
`UpdateAssumeRolePolicy` and `PutRolePolicy` on `versus-reader`.

## Drift check

`validate_test.go` in the parent directory parses the template, the Terraform
module and these JSON files, and fails if the allow lists, the deny groups
(including the billing surfaces) or the trust condition differ in any way:

```bash
cd versus-incident/scripts/aws
go test ./... -count=1
bash -n cli/create-versus-reader.sh
```

The CLI policy files are inert until the script substitutes placeholders, so
they are safe to read, diff and review.
