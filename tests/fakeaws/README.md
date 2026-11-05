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

Per-request overrides (useful when one test needs two fixtures):

- `X-FakeAWS-Scenario` selects a scenario for that request.
- `X-FakeAWS-Account` and `X-FakeAWS-Region` pin the caller.

An empty scenario argument falls back to `FAKEAWS_SCENARIO`, then to
`new-account-connect`.

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
