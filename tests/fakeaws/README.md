# fakeaws

Deterministic AWS API fixture for tests. Standard library only, and the product
binary never imports it.

It speaks the three protocols the probe client uses:

| Protocol | Shape | Services |
|---|---|---|
| query | form-encoded `Action`, XML answer | STS, EC2, Auto Scaling, CloudFormation, CloudWatch, ElastiCache, ELBv2, IAM, RDS, SNS, SQS |
| JSON | `X-Amz-Target`, JSON answer | Health, CloudTrail, Config, DynamoDB, ECS, EventBridge, Kinesis, Step Functions |
| REST | path-routed | API Gateway, CloudFront, EKS, Lambda, Route 53, S3 |

## Using it from a Go test

```go
server := fakeaws.New(t, "new-account-connect")
httpClient := server.HTTPClient() // inject as the probe client's HTTP seam
baseURL := server.URL()           // or override the client's endpoint
```

`HTTPClient` rewrites every request host to the fixture, so the caller keeps
its normal regional endpoints and signing host while the traffic lands here.
`URL()` is for callers that inject a fixed endpoint instead.

The fixture always serves three accounts:

| Account | ID | Home region | Notes |
|---|---|---|---|
| Production | `111122223333` | us-east-1 | every probe readable |
| Staging | `444455556666` | eu-west-1 | ElastiCache denied, Health not on plan, Config/Kinesis/Step Functions empty |
| Shared services | `777788889999` | ap-southeast-1 | every probe readable |

The calling account is taken from `X-FakeAWS-Account`, else from a 12-digit
account id inside `X-Amz-Security-Token`. The region is taken from the SigV4
credential scope in `Authorization`, else `X-FakeAWS-Region`.

External IDs: `AssumeRole` accepts any `vrs-ext-` value with at least eight
lowercase letters or digits. Pin one with `server.SetExternalID("…")` and the
fixture rejects every other value, including a missing one, with
`AccessDenied` — never with credentials.

## Scenarios

| Scenario | Behaviour |
|---|---|
| `new-account-connect` | The three accounts above; `DescribeRegions` plus the region-presence calls for EC2, Lambda, ECS and RDS |
| `assume-denied` | Every `AssumeRole` failure: `trust_denied`, `external_id_mismatch`, `role_not_found`, `session_duration_invalid`. Pick one with `X-FakeAWS-Assume-Error`, or name the role session after it, or request `DurationSeconds` below 900 |
| `throttled-api` | Every third non-STS request answers `400 ThrottlingException`. Override with `server.SetThrottle(n)`; `0` disables it |
| `region-disabled` | `DescribeRegions` lists `ap-southeast-1`, but EC2, Lambda, ECS and RDS calls there answer `400 AuthFailure` |
| `secret-canary` | Plants canary tag values, Lambda environment strings, an access key and CloudTrail `requestParameters`. `CanarySecrets()` lists them so a redaction test asserts none surfaced |

| `bad-deploy` | Collector world where task definition `checkout:42` is registered (CloudTrail `RegisterTaskDefinition`, then `UpdateService` 20 s later, by a CI role); four minutes later the ALB 5xx alarm `checkout-5xx` (and composite `checkout-health`) is in ALARM and 3 of 4 targets in `versus-web` are unhealthy; the ECS rollout is in progress |
| `health-event` | Collector world plus an upcoming AWS Health event that affects `i-0123456789abcdef0` and a scheduled instance reboot |
| `access-denied-burst` | Collector world where Lambda, SQS, SNS, EventBridge, Step Functions, Kinesis and the tagging API are denied, plus single detail calls (`DescribeTargetHealth`, `ListStackResources`, `GetBucketEncryption`, `ListAssociatedResources`) |
| `load-5k` | Collector world plus 5,000 generated resources (instances, functions, queues, tables, topics) served in pages of at most 100. `SetLoadSize(n)` or `FAKEAWS_LOAD_SIZE=50000` for the large-account run |
| `cost-nat-spike` | Cost Explorer, Budgets, Cost Optimization Hub and Compute Optimizer answer from `awsfix.CostNatSpike` (the NAT step after the checkout-api :41 -> :42 release) |
| `cost-credits` | Billing calls from `awsfix.SingleAccount`: credit, tax, Savings Plan fee, budgets |
| `cost-one-off-fee`, `cost-organization` | Billing calls from `awsfix.Organization`: management account `999999999999` sees every account (incl. cost-only `333333333333`) and the one-off support fee; any other caller sees only its own lines |
| `cost-denied-account` | `organization.json`; every billing call signed by `333333333333` answers `AccessDeniedException` |
| `cost-throttled` | `cost-nat-spike.json`; every Cost Explorer call answers `LimitExceededException` until `SetCostThrottled(false)` |
| `cost-cap-reached` | `awsfix.LargeOrganization`: 60 linked accounts in pages of 50 groups |
| `cost-nat-spike-two-nats` | `awsfix.CostNatSpikeTwoNATs` (a second NAT gateway behind the inventory app); `GetCostAndUsageWithResources` answers `AccessDeniedException`, so the NAT line cannot be narrowed |
| `cost-new-untagged-db` | `awsfix.CostNewUntaggedDB`: a full September, then an untagged database instance from Oct 20 |
| `cost-unavailable` | `cost-nat-spike.json`; every Cost Explorer call answers `AccessDeniedException: User not enabled for cost explorer access` |

### Collector world

Every collector operation (`CollectorOperations()` lists them) is answered from
one coherent account: VPC, subnet, route table, gateways, two security groups
(one with SSH open to the internet), four web instances behind the `versus-web`
target group and the `versus` ALB, ECS service `checkout` in cluster `versus`,
Lambda `versus-worker` fed by queue `versus-jobs`, stack `versus-stack` and the
AppRegistry application `checkout`, alarms, buckets (one public and
unencrypted), CloudFront, Route 53, IAM roles and a CloudTrail history that
`LookupEvents` filters by `StartTime`/`EndTime`. The four collector scenarios
serve the probe calls from the world too; the probe scenarios keep their
one-item probe pages and use the world only for collector-only calls.
`SetClock` pins the time events and alarms are relative to.

### Pointing the server binary at the fixture

Build the server with `-tags harness_fakeaws` and set
`VERSUS_HARNESS_FAKEAWS_URL` to the fixture's URL (for example the
`tests/fakeaws/cmd/fakeaws` binary). Every AWS call the settings probes and
the collectors make is then rewritten to that URL. A build without the tag
has neither the variable nor the redirect.
The FinOps cost refresh (Cost Explorer, Budgets, Cost Optimization Hub, Compute Optimizer) is redirected the same way.

Per-request overrides (useful when one test needs two fixtures):

- `X-FakeAWS-Scenario` selects a scenario for that request.
- `X-FakeAWS-Account` and `X-FakeAWS-Region` pin the caller.

An empty scenario argument falls back to `FAKEAWS_SCENARIO`, then to
`new-account-connect`.

### Cost scenarios

Data is built in Go by `tests/fixtures/awsfix`, the same builders the cost
package's unit tests use; no data file is read. Amounts are summed in micro-units and
rounded like the product. Every day of the open month, by the fixture clock
(`SetClock`), is marked `Estimated`, like real AWS. `PaidCostRequests()` counts
every Cost Explorer request except `ListCostAllocationTags`; `CostCalls()`
counts billing calls per `service:Action`. Billing calls are answered only in a
cost scenario.

## Budget assertions

```go
server.PathCount("/2015-03-31/functions/") // arrivals on one path
server.OperationCount("ec2:DescribeInstances") // one service:Action
server.TotalRequests() // everything
```

## Adding a scenario

1. Add the name to `knownScenarios` in `server.go`; unknown names fail fast in
   `New`/`NewServer`.
2. Seed any per-server state in `NewServer` — deny lists, throttle intervals or
   disabled regions — the way `throttled-api` and `region-disabled` do.
3. If the scenario changes a response body, gate it on `request.scenario` (see
   `canaryEnabled`) rather than on a package global, so two scenarios can run
   against one server via `X-FakeAWS-Scenario`.
4. Add one table entry to the scenario list above.
5. Prove it in `server_test.go`: assert the status, the protocol-shaped body,
   and the operation counter. Assert both the positive and the negative case —
   what the fixture returns and what it must never return.

Keep fixture data deterministic. Do not import product packages from this
package, and do not put planning identifiers in code or tests.

## Browser harness binary

`cmd/fakeaws` serves the same fixture over a real listener. It compiles only
with the `harness_fakeaws` build tag:

```sh
go run -tags harness_fakeaws ./tests/fakeaws/cmd/fakeaws -scenario secret-canary -listen 127.0.0.1:0
```

The binary prints its URL on stdout. Untagged product builds cannot include it.
