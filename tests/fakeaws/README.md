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
| `changes-quiet` | Collector world whose CloudTrail holds only a read event: a complete, quiet change window |
| `changes-aws-service-only` | Only changes AWS services made (Auto Scaling `SetDesiredCapacity`, ECS `UpdateService` by `AWSService`) |
| `changes-error-burst` | 12 failed `ssm:PutParameter` calls (`AccessDenied`) by role `config-sync`, one a minute |
| `changes-busy` | 5,000 write events spread over the 24 hours before the scenario's first request; their times are absolute, so every read sees the same history |
| `changes-cloudtrail-denied` | `LookupEvents` answers `AccessDeniedException` for the Staging account only |
| `changes-throttled` | Every `LookupEvents` call answers `ThrottlingException` |
| `changes-alarm-no-suspect` | The `checkout-5xx` alarm fired forty minutes ago; the only write event is three hours old |
| `changes-unknown-actor` | One change by an identity of type `Unknown` through an API no template covers (`widgets:CreateWidget`) |
| `role-session-expired` | Staging (`444455556666`, `RevokedAccount`) lost its reader role after connect: `AssumeRole` for it answers `AccessDenied` (not authorized to perform `sts:AssumeRole`) and every call signed with a Staging session token, `GetCallerIdentity` included, answers `ExpiredToken` (`ExpiredTokenException` on JSON APIs). Other accounts read the collector world. Switch to it after connect; two failed identity checks (two Rechecks) disconnect the account |
| `region-not-enabled` | `af-south-1` (`NotEnabledRegion`) is an opt-in Region that is not enabled. `DescribeRegions` with `AllRegions=true` lists it as `not-opted-in` (without, it is absent). Every call signed for it fails as AWS fails it: EC2 `401 AuthFailure`, JSON and REST-JSON `UnrecognizedClientException`, query and XML `InvalidClientTokenId` (STS too). Holds for every request when it is the server's own scenario, so a change scenario can be layered on it per request |
| `region-opt-in-unrecorded` | `region-not-enabled` for a Region Settings never learns about: `DescribeRegions` never lists `af-south-1`, every call there answers `401 OptInRequired` (a simplification: real JSON services answer `UnrecognizedClientException`, as `region-not-enabled` does). Also active for every request as the server's own scenario |
| `changes-late-arrival` | One `events:PutRule` write a minute over the last 2 h, each delivered 20 min after it happened (`ChangesLateDelay`), later than the default 15-min overlap. Times and IDs are absolute minutes, so a poll finds about one late event per minute of polling inside coverage an earlier poll claimed |
| `changes-malformed` | Two valid writes, one event whose `CloudTrailEvent` document is not JSON (well formed otherwise: stored, actor unknown), and three malformed events (`ChangesMalformedCount`): no `EventId`, no `EventTime`, a NUL byte in the event name. All are older than the overlap, so none is re-read |
| `changes-hidden-values` | Writes carrying secret-shaped values (`HiddenCanaries()`): Lambda environment `DB_PASSWORD`, an EC2 tag `owner` (outside the tag allowlist), an SSM `SecureString` value, EC2 `userData`, a Secrets Manager `secretString` and an IAM `CreateAccessKey` secret in the response |
| `changes-routine` | Routine agent events of 12 instances over the last 2 h on absolute five-minute boundaries (SSM `UpdateInstanceInformation`; ECS `SubmitTaskStateChange` every ten minutes) plus one real `RunInstances`. `ChangesRoutineCount(from, to)` is how many it serves one pair |
| `changes-email-principal` | A change by IAM user `jane.doe@example.com`, one by SSO session `john.smith@example.com` (`ChangesEmailPrincipals()`), and 6 failed `ssm:PutParameter` calls by both through one permission set |
| `changes-tied-suspects` | `bad-deploy`'s world and alarm with two `UpdateService` calls on `checkout`, by two pipelines 90 s apart, before the alarm: two candidates at the same scope |
| `cost-nat-spike` | Cost Explorer, Budgets, Cost Optimization Hub and Compute Optimizer answer from `awsfix.CostNatSpike` (the NAT step after the checkout-api :41 -> :42 release) |
| `cost-credits` | Billing calls from `awsfix.SingleAccount`: credit, tax, Savings Plan fee, budgets |
| `cost-one-off-fee`, `cost-organization` | Billing calls from `awsfix.Organization`: management account `999999999999` sees every account (incl. cost-only `333333333333`) and the one-off support fee; any other caller sees only its own lines |
| `cost-denied-account` | `organization.json`; every billing call signed by `333333333333` answers `AccessDeniedException` |
| `cost-throttled` | `cost-nat-spike.json`; every Cost Explorer call answers `LimitExceededException` until `SetCostThrottled(false)` |
| `cost-cap-reached` | `awsfix.LargeOrganization`: 60 linked accounts in pages of 50 groups |
| `cost-nat-spike-two-nats` | `awsfix.CostNatSpikeTwoNATs` (a second NAT gateway behind the inventory app); `GetCostAndUsageWithResources` answers `AccessDeniedException`, so the NAT line cannot be narrowed |
| `cost-new-untagged-db` | `awsfix.CostNewUntaggedDB`: a full September, then an untagged database instance from Oct 20 |
| `cost-unavailable` | `cost-nat-spike.json`; every Cost Explorer call answers `AccessDeniedException: User not enabled for cost explorer access` |

CloudTrail `EventId`s are unique per account and Region (the fixture ID plus `-<account>-<region>`), as real ones are, so two accounts or Regions never share a stored row. The global stream reads us-east-1 and sees the us-east-1 IDs. Events added with `AddLateEvent` keep their ID as given.

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

Build the server with `-tags harness_awsendpoint` and set
`VERSUS_HARNESS_AWS_ENDPOINT_URL` to the fixture's URL (for example the
`tests/fakeaws/cmd/fakeaws` binary). Every AWS call the settings probes and
the collectors make is then rewritten to that URL. The request keeps its
SigV4 signature and the real AWS `Host` header. The URL must be a bare
`http(s)://host[:port]` whose host is loopback, a private address or a local
name; anything else makes every AWS call fail. For an HTTPS endpoint with its
own CA (an AWS API emulator), set `VERSUS_HARNESS_AWS_CA_BUNDLE` to the PEM
file; only the redirected transport trusts it. The older `harness_fakeaws`
tag and `VERSUS_HARNESS_FAKEAWS_URL` variable still work the same way. A
build without either tag has none of these variables and no redirect.
The redirect is the `pkg/aws/harnessendpoint` package: any server binary built
with the tag, OSS or Enterprise, passes `harnessendpoint.HTTPClient()` as its
AWS transport.
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

A single-account data set (every scenario above except the Organization ones)
is the calling account's own cost, as Cost Explorer answers a standalone
account: its lines, account names, budget `LINKED_ACCOUNT` filters and
recommendations carry the caller's account ID, so the default caller
`111122223333` sees the same cost as a test signing as `111111111111`.
Organization scenarios keep their fixed IDs.

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
