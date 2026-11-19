# AWS Configuration

Connect Versus to your AWS accounts, choose the regions it reads, and turn on
FinOps. To try Versus against one account in a few minutes, start with the
[Quickstart](/agent/aws/quickstart). This page is the full setup reference.

## Configuration

Configure the AWS connector in a `connectors.yaml` file beside `config.yaml`,
or in a `connectors:` block inside `config.yaml`. When `connectors.yaml` holds
settings, it replaces the inline block. Restart Versus after every change.

The smallest working setup reads one account with the credentials Versus
already has:

```yaml
connectors:
  aws:
    enable: true
    accounts:
      - account_id: "111122223333"
        display_name: Production
        regions: [us-east-1, eu-west-1]
        auth:
          mode: default_chain
```

Quote the account ID so a leading zero is kept.

You can also connect accounts from **AWS > Settings > Connect account**. That
wizard walks you through creating a read-only role; see
[Connect your AWS account](/in-development/aws-harness-connect). Accounts
written in the config file show **From config** in Settings, and you change
them only in the file.

## How AWS reads work

Versus only reads. It never creates, changes or deletes anything in your
accounts.

| What Versus reads | How | Refreshed every |
| --- | --- | --- |
| Inventory: VPCs, Lambda, EKS, DynamoDB, ElastiCache, SQS, SNS, EventBridge, Step Functions, Kinesis, API Gateway, Auto Scaling, S3, CloudFront, Route 53, IAM roles, tags | `Describe*`, `List*` and `Get*` metadata calls | 15 minutes (`collect.intervals.inventory`) |
| Health: CloudWatch alarms, EC2, ECS, load balancers, RDS, CloudFormation stacks, AWS Health events | Status calls | 2 minutes (`collect.intervals.health`) |
| Changes, shown on [AWS > Changes](/agent/aws/changes) | CloudTrail `LookupEvents`, in each selected region | 5 minutes (`collect.intervals.changes`) |
| Cost, forecast, savings and budgets | Cost Explorer and the cost APIs, only when you turn FinOps on | Once a day (`finops.refresh_time`) |
| Account access | Who Versus signs in as, plus one probe per service | Identity every 5 minutes (`health_check_interval`); services every 6 hours |

Before it reads anything from an account, Versus checks which AWS account its
credentials belong to. If that check fails, nothing is read from that account.

The first time, Versus reads the whole account. **AWS > Settings** shows
**First sync**, and each service moves from **Waiting** to **Loading** to
**Ready**.

## Credentials

Versus signs in with an identity from the machine or pod it runs on. It looks
for one in this order and uses the first it finds:

| Order | Source | Set by |
| --- | --- | --- |
| 1 | Access key | `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` (optional `AWS_SESSION_TOKEN`) |
| 2 | IAM roles for service accounts (IRSA) | `AWS_WEB_IDENTITY_TOKEN_FILE` and `AWS_ROLE_ARN`, injected by EKS |
| 3 | Shared profile | `AWS_PROFILE` and the shared credentials file. Static keys only. |
| 4 | Container credentials: ECS task role or EKS Pod Identity | Injected by ECS or EKS |
| 5 | EC2 instance profile | The instance metadata service |

Then, for each account, the `auth.mode` setting decides how that identity is
used:

| `auth.mode` | What Versus does |
| --- | --- |
| `default_chain` (default) | Reads the account directly with the identity above. That identity must belong to this account. |
| `assume_role` | Uses the identity above to assume `role_arn` in the account, with the optional `external_id`. |

`profile` and `web_identity` are also accepted. They behave like
`default_chain`: the source comes from the environment variables in the table
above.

> **Recommended:** run Versus with an IAM role from its platform (IRSA, EKS Pod
> Identity, an ECS task role or an instance profile), and use `assume_role` with
> an external ID for each account it reads. There is no long-lived key to
> rotate, and each account controls its own access.

### Access key

Use an access key only to try Versus or where no role is available. The key
reaches Versus only as environment variables. There is no config key for it.

**Docker Compose:**

```yaml
services:
  versus:
    environment:
      AWS_ACCESS_KEY_ID: ${AWS_ACCESS_KEY_ID}
      AWS_SECRET_ACCESS_KEY: ${AWS_SECRET_ACCESS_KEY}
```

**Helm:** store the key in a Secret, then point the chart at it.

```bash
kubectl create secret generic versus-aws \
  --from-literal=AWS_ACCESS_KEY_ID=AKIA... \
  --from-literal=AWS_SECRET_ACCESS_KEY=...
```

```yaml
connectors:
  aws:
    enable: true
    credentials:
      existingSecret: versus-aws
      accessKeyIdKey: AWS_ACCESS_KEY_ID          # key name inside the Secret
      secretAccessKeyKey: AWS_SECRET_ACCESS_KEY  # key name inside the Secret
```

The chart passes the Secret to the container as environment variables, never
through the ConfigMap. Leave `existingSecret` empty when Versus uses a role.

### IRSA on EKS

1. Create an IAM role whose trust policy allows only the Versus
   ServiceAccount:

   ```json
   {
     "Version": "2012-10-17",
     "Statement": [
       {
         "Effect": "Allow",
         "Principal": { "Federated": "arn:aws:iam::111122223333:oidc-provider/oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE" },
         "Action": "sts:AssumeRoleWithWebIdentity",
         "Condition": {
           "StringEquals": {
             "oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE:sub": "system:serviceaccount:versus:versus-incident",
             "oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE:aud": "sts.amazonaws.com"
           }
         }
       }
     ]
   }
   ```

2. Create the annotated ServiceAccount before you install the chart:

   ```yaml
   apiVersion: v1
   kind: ServiceAccount
   metadata:
     name: versus-incident
     namespace: versus
     annotations:
       eks.amazonaws.com/role-arn: arn:aws:iam::111122223333:role/versus-app
   ```

3. Tell the chart to use it:

   ```yaml
   serviceAccount:
     create: false
     name: versus-incident
   ```

For EKS Pod Identity, create a Pod Identity association for the same
ServiceAccount instead of the annotation. Versus picks up the injected
credentials with no other setting.

For a step-by-step EKS setup with IRSA or Pod Identity, several accounts and
FinOps, with copy-ready files, see
[AWS on EKS](/examples/aws-eks-irsa-pod-identity).

### ECS task role and EC2 instance profile

Attach the role to the task or the instance. Versus finds it with no setting.
When Versus runs in Docker on EC2 with IMDSv2, set the instance metadata hop
limit to 2 so the container can reach the instance metadata service.

### Shared profile

Mount a credentials file and set `AWS_PROFILE`. Versus accepts only profiles
with static keys (`aws_access_key_id`, `aws_secret_access_key`, optional
`aws_session_token`). Profiles that use SSO, `role_arn`, `source_profile` or
`credential_process` are refused. The file must not be writable by group or
others.

### What never goes in a config file

| Value | Where it goes instead |
| --- | --- |
| Access key and secret key | Environment variables, or a Kubernetes Secret through `credentials.existingSecret` |
| External ID | An environment variable, referenced as `${VAR}`. A literal value is refused at startup. |
| Session token | Environment variable `AWS_SESSION_TOKEN` |

## Account modes

| Mode | Use when | `auth.mode` |
| --- | --- | --- |
| One account | Versus runs in, or has a key for, the account it reads | `default_chain` |
| Several accounts | Versus reads accounts other than its own | `assume_role`, one role per account |

You can list up to 64 accounts. Each `account_id` appears once.

On EKS, [AWS on EKS](/examples/aws-eks-irsa-pod-identity) walks through both
modes with IRSA or Pod Identity.

### One account

1. **Give Versus an identity in the account.** Use one of the
   [credentials](#credentials) above.
2. **Grant it read access.** Attach the read-only policy from
   [Permissions](#permissions) to that role or user.
3. **Write the config:**

   ```yaml
   connectors:
     aws:
       enable: true
       accounts:
         - account_id: "111122223333"
           display_name: Production
           regions: [us-east-1]
           auth:
             mode: default_chain
   ```

4. **Restart Versus and verify.** Open **AWS > Settings**. The account shows
   **Connected**, with a count such as `22 of 24 readable`. Use **Recheck** to
   run the check again now.

Settings shows times in your browser's local time, with no `UTC` suffix, for
example `last run 14:21`. Times from an earlier day carry the date, for
example `Nov 18 14:21`.

One identity belongs to one account, so `default_chain` can read only one
account. Use `assume_role` for the others.

### Several accounts

Versus assumes a read-only role in each account. Each role trusts the Versus
identity, and only when the caller presents the external ID.

1. **Find the Versus identity.** It is the role from
   [Credentials](#credentials), for example
   `arn:aws:iam::999988887777:role/versus-app`. **AWS > Settings > Connect
   account** shows it as **Versus principal ARN**.

2. **Allow it to assume the reader roles.** Attach this to the Versus identity:

   ```json
   {
     "Version": "2012-10-17",
     "Statement": [
       {
         "Effect": "Allow",
         "Action": "sts:AssumeRole",
         "Resource": "arn:aws:iam::*:role/versus-reader"
       }
     ]
   }
   ```

3. **Create an external ID.** One per deployment or one per account:

   ```bash
   openssl rand -hex 16
   ```

   Treat it like a password. Use at least 16 characters and no spaces.

4. **Create the reader role in each account.** The role is named
   `versus-reader`. Its trust policy allows only the Versus role ARN, and only
   with the external ID:

   ```json
   {
     "Version": "2012-10-17",
     "Statement": [
       {
         "Effect": "Allow",
         "Principal": { "AWS": "arn:aws:iam::999988887777:role/versus-app" },
         "Action": "sts:AssumeRole",
         "Condition": { "StringEquals": { "sts:ExternalId": "<external-id>" } }
       }
     ]
   }
   ```

   The repository ships the same role three ways, in
   [`scripts/aws`](https://github.com/VersusControl/versus-incident/tree/dev/scripts/aws):
   a CloudFormation template (`versus-reader.yaml`), a Terraform module
   (`terraform/versus-reader`) and a CLI script. With the CLI, run it in each
   account:

   ```bash
   cd scripts/aws/cli
   ./create-versus-reader.sh arn:aws:iam::999988887777:role/versus-app -
   ```

   The `-` makes the script prompt for the external ID, so it stays out of
   your shell history. Add `--optional-data` to include the cost reads for
   FinOps. The script refuses an account root or an IAM user as the
   principal: trust an exact role ARN.

5. **Write the config.** Reference the external ID through an environment
   variable:

   ```yaml
   connectors:
     aws:
       enable: true
       accounts:
         - account_id: "111122223333"
           display_name: Production
           regions: [us-east-1, eu-west-1]
           auth:
             mode: assume_role
             role_arn: arn:aws:iam::111122223333:role/versus-reader
             external_id: ${VERSUS_AWS_EXTERNAL_ID}
         - account_id: "444455556666"
           display_name: Staging
           regions: [us-east-1]
           auth:
             mode: assume_role
             role_arn: arn:aws:iam::444455556666:role/versus-reader
             external_id: ${VERSUS_AWS_EXTERNAL_ID}
   ```

   With Docker Compose, add `VERSUS_AWS_EXTERNAL_ID` to the service
   `environment`. With Helm, see [Helm values](#helm-values).

6. **Restart Versus and verify.** In **AWS > Settings**, every account shows
   **Connected**. If a role cannot be assumed, the account shows the reason;
   see [Troubleshooting](#troubleshooting).

Versus asks for 15-minute sessions. Its session names start with `versus`, so
you can find its calls in CloudTrail.

### The account ID check

At every identity check, Versus asks AWS which account its credentials belong
to and compares it with `account_id`.

- **They match:** Versus reads the account.
- **They don't match:** Versus reads nothing through those credentials, so no
  data from one account is filed under another. The account is marked
  **Disconnected** at once, without waiting for a second failure. Settings
  shows **The AWS credentials belong to another account**.

To fix it, set `account_id` to the account of the credentials
(`aws sts get-caller-identity`), or use credentials of the configured account,
then restart Versus.

## Regions

| `regions` value | What Versus reads |
| --- | --- |
| A list, for example `[us-east-1, eu-west-1]` | Only those regions |
| `auto`, or the key left out | Every region enabled in the account, found at the first identity check |

- **Choose regions:** list the regions where you run workloads. Each region
  adds API calls, and the request rate applies per account and region.
- **Opt-in regions:** a region your account has not opted in to is not read.
  With `auto`, it is skipped. If you list it, Settings shows
  `1 not enabled` on the account, and the summary line says
  **Production has 1 region not enabled**. Opt in to the region in the AWS
  console, or remove it from the list.
- **Opt-in status not read:** when Versus could not re-read a region's opt-in
  status recently, the account's Regions cell shows a line such as
  `af-south-1: opt-in status not read since 14:32`. Until the status is read
  again, [AWS Changes](/agent/aws/changes#gaps-and-their-reasons) treats that
  region as a real gap, not as **Not enabled in AWS**.
- **Global services:** S3, CloudFront, Route 53, IAM and AWS Health are read
  once per account through `us-east-1`, whatever regions you list. Cost
  Explorer is global too.
- **Changes to global services:** CloudTrail records them in `us-east-1`.
  Versus reads them from there whatever regions you list. See
  [AWS Changes](/agent/aws/changes#iam-permissions).
- **Sign-in region:** Versus checks the identity and assumes roles in the
  first listed region, or `us-east-1` with `auto`. Put an enabled region first.

Region names from the commercial, AWS GovCloud (US) and China partitions are
accepted. The role ARN's partition must match the regions.

## FinOps setup

The FinOps page shows this month's spend, what moved it, the forecast and
where you can save. It is off by default because Cost Explorer charges for
each request.

### 1. Turn on Cost Explorer in AWS

In the account that pays for the cost, open the
[Billing and Cost Management console](https://console.aws.amazon.com/costmanagement/home),
then **Cost Explorer**, and turn it on once. In an Organization, member
accounts can read their own cost only if the management account allows it
(**Cost Explorer preferences > Member account access**).

### 2. Grant the cost reads

Add the cost statement from [Permissions](#permissions) to each reader role,
or use `versus-cost-reader` in the management account
([Cost of every account](#cost-of-every-account-through-the-management-account)).

### 3. Turn on cost reads in Versus

```yaml
connectors:
  aws:
    optional_data:
      cost_explorer: true
```

Or set `VERSUS_CONNECTORS_AWS_OPTIONAL_DATA_COST_EXPLORER=true`. The
environment variable wins over the file. With Helm, set
`connectors.aws.optionalData.costExplorer=true`.

### 4. Minimal config

The smallest config that shows cost on the FinOps page. Everything else uses
its default.

```yaml
connectors:
  aws:
    enable: true
    optional_data:
      cost_explorer: true        # without it, FinOps shows no cost
    accounts:
      - account_id: "111122223333"
        regions: [us-east-1]
        auth:
          mode: default_chain
```

With these defaults, spend is grouped by the `team` tag. Versus keeps the
values of `app`, `team`, `env`, `owner` and `cost-center`. Cost refreshes daily
at 09:00 UTC, with at most 20 paid Cost Explorer requests a day. The page has
no budgets until you add them in step 5.

### 5. Full config: ownership and budgets

The same config with every FinOps option set: the tag allowlist, an owner per
account, the refresh time, the report timezone, budgets and account groups.
Copy it whole, then change the values.

```yaml
connectors:
  aws:
    enable: true
    optional_data:
      cost_explorer: true        # step 3: without it, FinOps shows no cost
    tags:
      value_allowlist: [Name, app, team, env]   # must include team_tag_key
    accounts:
      - account_id: "111122223333"
        display_name: Production
        regions: [us-east-1]
        auth:
          mode: default_chain
        owner: platform          # owner when no team tag applies
    finops:
      team_tag_key: team
      max_paid_requests_per_day: 20
      refresh_time: "09:00"
      report:
        timezone: Europe/Berlin
      budgets:
        - { name: AWS total, scope: deployment, monthly_usd: 42000 }
        - { name: Checkout, scope: team, value: checkout, monthly_usd: 5000 }
        - { name: Production, scope: account, value: "111122223333", monthly_usd: 30000 }
      account_groups:
        - { name: Production, accounts: ["111122223333"] }
```

For spend by team:

1. Tag resources with one key, `team` by default (`finops.team_tag_key`).
2. **Activate the key as a cost allocation tag** in **Billing > Cost
   allocation tags**. Until then the team breakdown says **Team tag not active
   in billing**, and spend shows as Unallocated.
3. Add the key to `tags.value_allowlist`, with the same case. Otherwise
   Versus drops the team values and says **No team tag on this cost**.

A budget is **over budget** when this month's spend passes it, and **at risk**
when the forecast passes 90% of it. Versus raises an alert the first time a
budget reaches each status in a month. More on tags:
[Tag your AWS cost for ownership](/in-development/aws-finops-tagging).

### Cost of every account through the management account

In an AWS Organization, read the cost of every member account through the
management (payer) account:


1. **Create a cost role in the management account.** Use
   `versus-cost-reader` from
   [`scripts/aws`](https://github.com/VersusControl/versus-incident/tree/dev/scripts/aws)
   (CloudFormation `versus-cost-reader.yaml` or Terraform
   `terraform/versus-cost-reader`). It allows only the cost reads and denies
   everything else. Its trust policy is the same as `versus-reader`.

2. **List the management account, and name it as the cost source:**

   ```yaml
   connectors:
     aws:
       enable: true
       optional_data:
         cost_explorer: true
       accounts:
         - account_id: "123456789012"
           display_name: Management
           auth:
             mode: assume_role
             role_arn: arn:aws:iam::123456789012:role/versus-cost-reader
             external_id: ${VERSUS_AWS_EXTERNAL_ID}
         # member accounts with versus-reader, as above
       finops:
         cost_source_account: "123456789012"
   ```

3. **Verify.** The FinOps page shows the cost of every linked account. With
   10 or more accounts, it adds an **Accounts** tab.

The cost role reads nothing else, so **AWS > Settings** lists the management
account's other services as **Denied**. That is expected. Connect member
accounts with `versus-reader` to see their resources and changes.

Without `cost_source_account`, each connected account reads only its own
cost. A member account sees only its own cost.

### What it costs

AWS charges $0.01 for each Cost Explorer request. Versus spends at most
`max_paid_requests_per_day` paid requests a day for the whole deployment: 20
by default, so at most $0.20 a day. The daily refresh comes first. A manual
refresh can run once a day, and a drill-down cannot use the requests the
daily refresh still needs.

### When data appears

| Step | Wait |
| --- | --- |
| Cost Explorer turned on for the first time | AWS can take up to 24 hours to fill in cost data |
| First read by Versus | As soon as the account's identity check passes. After that, once a day after `refresh_time`, in `report.timezone`. |
| A day counts as complete | 24 hours after the day ends (UTC). Until the first complete day the page says **Waiting for the first complete day of cost data**. |

### What the page shows when data is missing

| What you see | Meaning |
| --- | --- |
| **What FinOps answers** and **What it reads**, with **Turn on** | Cost reads are off. Turn them on as in step 3. |
| **Waiting for the first complete day of cost data** | Cost reads are on; no full day has settled yet. |
| **No account ready to read cost yet** | No account has passed its identity check. Check **AWS > Settings**. |
| **Cost source account not connected** | `cost_source_account` is not listed in `accounts`, or its check has not passed. |
| **Cost Explorer is not available for** *account* | Cost Explorer was never opened in that account, or the Organization admin turned off member access. |
| **No account can read cost** | Cost was read before, and no account can read it now. Versus keeps showing the cost it already has. |
| **2 of 3 accounts** | One account's cost could not be read; its reason shows next to it, for example `Staging: access denied`. |

## Permissions

The read-only policy is
[`scripts/aws/versus-quickstart-policy.json`](https://github.com/VersusControl/versus-incident/blob/dev/scripts/aws/versus-quickstart-policy.json).
It has four statements:

| Statement | Effect | What it covers |
| --- | --- | --- |
| `MetadataReads` | Allow | Metadata reads for the 24 services Versus reads, `cloudtrail:LookupEvents`, `tag:GetResources` and Resource Explorer search |
| `CostReads` | Allow | FinOps only. Drop it if you don't use FinOps. |
| `DenyDataPlaneAndSecretReads` | Deny | `s3:GetObject`, `secretsmanager:GetSecretValue`, `ssm:GetParameter*`, `kms:Decrypt`, DynamoDB item reads, `lambda:GetFunction`, `ec2:GetConsoleOutput`, `logs:GetLogEvents` |
| `DenyBillingAccountSurfaces` | Deny | Invoices, payments, tax and account settings |

The cost statement:

```json
{
  "Sid": "CostReads",
  "Effect": "Allow",
  "Resource": "*",
  "Action": [
    "ce:GetCostAndUsage",
    "ce:GetCostAndUsageWithResources",
    "ce:GetCostForecast",
    "ce:GetSavingsPlansCoverage",
    "ce:ListCostAllocationTags",
    "budgets:ViewBudget",
    "cost-optimization-hub:ListRecommendations",
    "compute-optimizer:GetEC2InstanceRecommendations"
  ]
}
```

The `versus-reader` role from `scripts/aws` holds the same reads, with the
cost statement only when you ask for optional data. The deny statements win
over any allow, so a broader policy attached later cannot add them back.

## Connector options

All keys sit under `connectors.aws`.

| YAML key | Default | Purpose |
| --- | --- | --- |
| `enable` | `false` | Turns the connector on. Env: `VERSUS_CONNECTORS_AWS_ENABLE`. |
| `accounts[].account_id` | | 12-digit account ID. Required and unique. |
| `accounts[].display_name` | the account ID | Name shown in the UI, up to 128 bytes. |
| `accounts[].regions` | `auto` | Regions to read. See [Regions](#regions). |
| `accounts[].auth.mode` | `default_chain` | `default_chain` or `assume_role`. |
| `accounts[].auth.role_arn` | | Role to assume. Only with `assume_role`. |
| `accounts[].auth.external_id` | | `${VAR}` reference to the external ID. Only with `assume_role`. |
| `accounts[].owner` | | Who owns the account's cost when no team tag applies. Up to 128 characters. |
| `tags.value_allowlist` | see [Security](#security) | Tag keys whose values Versus keeps. Up to 32 keys. |
| `budgets.max_requests_per_second` | `2` | Request rate per account and region, 1 to 1000. Also editable in **Settings > Advanced** when the config leaves the default. |
| `health_check_interval` | `5m` | How often the identity check runs, 30s to 24h. |
| `optional_data.cost_explorer` | `false` | Turns on cost reads for FinOps. |
| `collect.enable` | `true` | Runs the background readers. |
| `collect.intervals.inventory` | `15m` | Inventory refresh, 30s to 24h. |
| `collect.intervals.health` | `2m` | Health and alarm refresh, 30s to 24h. |
| `collect.intervals.changes` | `5m` | CloudTrail polling, 30s to 24h. |
| `collect.history.max_generations` | `48` | Snapshots kept per service and region, 1 to 10000. |
| `collect.history.max_age` | `168h` | How long older snapshots are kept, 1h to 8784h. |
| `collect.history.change_retention` | `336h` | How long CloudTrail changes are kept, 1h to 8784h. |
| `collect.changes.*` | see [Changes settings](/agent/aws/changes#settings) | The CloudTrail reader: lookback, overlap, page budget, routine events, bursts and read cap. |
| `applications.sources` | `[appregistry, cloudformation, tags]` | Where Versus looks for a resource's application, in order. |
| `applications.tag_keys` | `[app, service]` | Tag keys read for the application, up to 16. Only allowlisted keys are used. |
| `finops.team_tag_key` | `team` | The one tag key for team ownership. |
| `finops.max_paid_requests_per_day` | `20` | Paid Cost Explorer requests per day for the deployment, 1 to 1000. |
| `finops.refresh_time` | `"09:00"` | Daily refresh time, `HH:MM`, in `report.timezone`. |
| `finops.report.timezone` | `UTC` | Time zone for "today", the refresh and reports. |
| `finops.cost_source_account` | empty | Management account that reads cost for the whole Organization. Must also be in `accounts`. |
| `finops.min_increase_usd_per_month` | `100` | Smallest projected monthly increase reported. |
| `finops.min_driver_usd` | `50` | Smaller cost changes roll into "Everything else". |
| `finops.target_coverage` | `0.8` | Savings Plans and reservation coverage to aim for, above 0 and at most 1. |
| `finops.budgets[]` | `[]` | Up to 50 monthly budgets: `name`, `scope` (`deployment`, `account` or `team`), `value`, `monthly_usd`. |
| `finops.account_groups[]` | `[]` | Up to 64 named groups of account IDs. An account belongs to one group. |

Environment overrides, read at startup:

- `VERSUS_CONNECTORS_AWS_COLLECT_` + `ENABLE`, `INVENTORY_INTERVAL`,
  `HEALTH_INTERVAL`, `CHANGES_INTERVAL`, `HISTORY_MAX_GENERATIONS`,
  `HISTORY_MAX_AGE`, `HISTORY_CHANGE_RETENTION`.
- `VERSUS_CONNECTORS_AWS_FINOPS_` + `TEAM_TAG_KEY`,
  `MIN_INCREASE_USD_PER_MONTH`, `MIN_DRIVER_USD`, `TARGET_COVERAGE`,
  `MAX_PAID_REQUESTS_PER_DAY`, `REFRESH_TIME`, `COST_SOURCE_ACCOUNT`,
  `REPORT_TIMEZONE`.

## Helm values

Helm values use camelCase where the YAML above uses snake_case, for example
`accountId`, `displayName`, `optionalData.costExplorer`,
`healthCheckInterval` and `finops.maxPaidRequestsPerDay`. Two keys exist only
in the chart: `credentials` and `auth.externalIdEnv`.

```yaml
connectors:
  aws:
    enable: true
    accounts:
      - accountId: "111122223333"
        displayName: Production
        regions: [us-east-1, eu-west-1]
        auth:
          mode: assume_role
          roleArn: arn:aws:iam::111122223333:role/versus-reader
          externalIdEnv: VERSUS_AWS_EXTERNAL_ID   # name of the env var, not the value
    credentials:
      existingSecret: ""        # empty with IRSA, Pod Identity or a node role
    optionalData:
      costExplorer: true
    finops:
      teamTagKey: team

extraEnv:
  - name: VERSUS_AWS_EXTERNAL_ID
    valueFrom:
      secretKeyRef:
        name: versus-aws-external-id
        key: external_id
```

| Value | Default | What it does |
| --- | --- | --- |
| `connectors.aws.enable` | `false` | Turns the connector on. |
| `connectors.aws.accounts[].accountId` | | Must be 12 digits; the install fails otherwise. Quote it in a values file. |
| `connectors.aws.accounts[].auth.externalIdEnv` | | Name of the environment variable that holds the external ID. Supply the variable through `extraEnv` or `extraEnvFrom`. |
| `connectors.aws.credentials.existingSecret` | empty | Secret with the access key. Requires `connectors.aws.enable: true`. |
| `connectors.aws.credentials.accessKeyIdKey` | `AWS_ACCESS_KEY_ID` | Key name inside the Secret. |
| `connectors.aws.credentials.secretAccessKeyKey` | `AWS_SECRET_ACCESS_KEY` | Key name inside the Secret. |
| `connectors.aws.tags.valueAllowlist` | `[Name, app, service, team, env]` | Tag keys whose values Versus keeps. |

With `--set`, quote arguments that contain square brackets:
`--set "connectors.aws.accounts[0].accountId=111122223333"`. See
[Installing with Helm](/configuration/helm).

## Security

- **Read-only by design.** Versus makes only read calls. The policy allows no
  `Put`, `Create`, `Update`, `Delete`, `Run` or `Invoke` action, and denies
  object, secret, parameter, item, function code, console output and log
  reads.
- **Tag values are redacted** unless their key is allowlisted. Versus keeps
  every tag key and replaces each other value with `[redacted]` before it
  stores anything. Keys match exactly and are case-sensitive, like AWS tag
  keys: `team` does not allow `Team`.

  | Allowlist in effect | When |
  | --- | --- |
  | `tags.value_allowlist` | Set in the config. Shown as **From config** in **Settings > Advanced**. |
  | The list saved in **Settings > Advanced > Tag value allowlist** | The config sets none |
  | `app`, `team`, `env`, `owner`, `cost-center` | Neither is set |

  When `Name` is not allowlisted, resources named by their `Name` tag show
  their resource ID instead. The **Privacy** card in Settings lists what is
  sent to the AI model and runs a leak self-test.
- **Secrets stay out of files.** Access keys come only from the environment.
  An `external_id` must be a `${VAR}` reference; a literal value stops
  Versus at startup. Error messages name the field, never the value.
- **Least privilege.** Grant only the reads above, and drop `CostReads` if you
  don't use FinOps. Trust an exact role ARN in each reader role, never an
  account root.
- **External ID.** It stops anyone who learns the Versus role ARN from using
  your reader role through another Versus deployment. Rotate it by changing the
  environment variable and the trust policy together.
- **Network.** Versus calls AWS endpoints directly over HTTPS. It ignores
  proxy settings for AWS calls, and it refuses `AWS_ENDPOINT_URL*` and
  `AWS_EC2_METADATA_SERVICE_ENDPOINT` overrides.

## Troubleshooting

Messages appear on **AWS > Settings** or on the FinOps page. Configuration
errors stop Versus at startup and appear in its log.

| Symptom | Cause | Fix |
| --- | --- | --- |
| **The Versus server has no AWS identity** | No credentials found, AWS rejected them, a shared profile uses SSO or a role, a credentials file is writable by others, or an `AWS_ENDPOINT_URL*` variable is set | Check `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`, or attach an instance role, task role or IRSA role, then restart Versus. |
| **The AWS credentials belong to another account** | The credentials sign in to a different account than `account_id` | Set `account_id` to the output of `aws sts get-caller-identity`, or use credentials of the configured account, then restart. |
| **Role not found** | No role at `role_arn`, or it is in another account | Check the account ID and role name in the ARN. |
| **The role trust policy refused Versus** | The trust policy does not name the Versus role ARN | Put the exact Versus principal ARN in the trust policy, and allow `sts:AssumeRole` on the Versus identity. |
| **Trust policy does not allow this ExternalId** | The external ID in the environment differs from the trust condition | Set the same value in both places. |
| *Account* **is missing** *service* **access**; the row shows *service*`: Denied` | The role lacks that service's read | Add the actions Settings names, for example `Add elasticache:Describe* to versus-reader`. |
| *Account* **is missing CloudTrail access** | `cloudtrail:LookupEvents` is not allowed, so changes are not read | Add `cloudtrail:LookupEvents` to the reader role. For other gaps on the Changes page, see [Changes troubleshooting](/agent/aws/changes#troubleshooting). |
| A service shows **Not on plan** | The service needs a higher AWS Support plan, for example AWS Health | Expected. It is not counted as denied. |
| **Region not enabled for this account**, or *account* **has 1 region not enabled** | A listed region is not opted in | Opt in to the region, or remove it from `regions`. |
| **AWS throttled the request**, or a service shows **AWS throttled, retrying** | AWS asked Versus to slow down | Nothing to do; Versus backs off and retries. If it persists, lower `budgets.max_requests_per_second` or read fewer regions. |
| **AWS did not answer in time** or **Could not reach AWS** | Network path to AWS | Check egress, DNS and firewalls. AWS calls do not use a proxy. |
| *Account* **disconnected since** *time*, for example `Staging disconnected since 14:21` | Two identity checks in a row failed, or the account ID did not match | Fix the cause shown on the account, then press **Recheck**. |
| **Set in the config file** | You tried to rename or remove an account defined in the config | Change it in the config file. |
| FinOps shows **Turn on** | Cost reads are off | Set `optional_data.cost_explorer: true`, then restart. |
| **Waiting for the first complete day of cost data** | No full day has settled yet | Wait. See [When data appears](#when-data-appears). |
| **Cost Explorer is not available for** *account* | Cost Explorer never opened, or member access is off in the Organization | Open Cost Explorer once in that account, or ask the management account admin to allow member access. |
| **Daily AWS request limit reached** | `max_paid_requests_per_day` is used up | Try again tomorrow, or raise the limit. |
| **Request cap reached:** *forecast and coverage* **skipped** | The day's paid requests ran out during the refresh | Raise `max_paid_requests_per_day`. |
| **Team tag not active in billing** | The team tag is not a cost allocation tag | Activate it in **Billing > Cost allocation tags**. |
| **No team tag on this cost** | Resources lack the tag, or `team_tag_key` is not in `tags.value_allowlist` | Tag the resources, and allowlist the key with the same case. |
| Startup error `connectors.aws.accounts[0]: external_id must reference an environment variable` | A literal external ID in the file | Use `external_id: ${VAR}` and set the variable. |
| Startup error `connectors.aws.accounts[0]: role fields require assume_role mode` | `role_arn` or `external_id` with another mode | Set `auth.mode: assume_role`. |
| Startup error `connectors.aws.accounts[0]: invalid or duplicate account ID` | Not 12 digits, or listed twice | Quote the ID and list each account once. |
| Startup error `connectors.aws.accounts[0]: invalid region` | A misspelled or unsupported region | Use region names such as `eu-west-1`. |

## Limits

| Data | Kept |
| --- | --- |
| Inventory snapshots | 48 per service and region, for up to 7 days (`collect.history`) |
| CloudTrail changes | 14 days (`collect.history.change_retention`) |
| Cost data | 13 months |
| Accounts in the config | 64 |

Changes come from CloudTrail event history, so only management events are
seen, and only within what CloudTrail keeps.
