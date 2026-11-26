package fakeaws

import (
	"net/http"
	"strconv"
	"time"
)

// State scenarios. Each makes the server reach one Changes or Settings
// state no other scenario reaches: an account whose role session stopped
// working after connect, a Region that refuses every call, CloudTrail
// events delivered after the overlap, malformed events, secret-shaped
// values, routine agent events, e-mail-shaped principals and tied suspect
// candidates. Every one is selected per request (X-FakeAWS-Scenario), so a
// running stack can switch to it without a restart.
const (
	// ScenarioRoleSessionExpired: the reader role of RevokedAccount can no
	// longer be assumed (AccessDenied), and every call signed with a session
	// token of that account answers ExpiredToken. The other accounts read
	// the collector world normally.
	ScenarioRoleSessionExpired = "role-session-expired"
	// ScenarioRegionNotEnabled: NotEnabledRegion is an opt-in Region that is
	// not enabled. DescribeRegions lists it as not-opted-in only when
	// AllRegions is asked for, and every call signed for it fails the way
	// AWS fails it (EC2 AuthFailure, JSON UnrecognizedClientException,
	// query and XML InvalidClientTokenId). Also active for every request
	// when it is the server's own scenario, so a change scenario can be
	// layered on it.
	ScenarioRegionNotEnabled = "region-not-enabled"
	// ScenarioRegionOptInUnrecorded is a not-enabled Region Settings never
	// learns about: DescribeRegions never lists NotEnabledRegion, and every
	// call signed for it answers the explicit opt-in refusal OptInRequired
	// (a simplification: real JSON services answer
	// UnrecognizedClientException, as region-not-enabled does). The
	// Region's state comes only from the polls. Also active for every
	// request as the server's own scenario.
	ScenarioRegionOptInUnrecorded = "region-opt-in-unrecorded"
	// ScenarioChangesLateArrival serves one write a minute over the last
	// ChangesLateSpan, each delivered ChangesLateDelay after it happened:
	// later than the default 15-minute overlap, so a poll finds it inside
	// coverage an earlier poll already claimed.
	ScenarioChangesLateArrival = "changes-late-arrival"
	// ScenarioChangesMalformed serves two valid writes and an unparseable
	// event document (stored, actor unknown) among ChangesMalformedCount
	// malformed events: one without an EventId, one without an EventTime
	// and one whose event name holds a NUL byte.
	ScenarioChangesMalformed = "changes-malformed"
	// ScenarioChangesHiddenValues serves writes that carry secret-shaped
	// values (HiddenCanaries) in request and response fields and a tag
	// value under a key outside the tag allowlist.
	ScenarioChangesHiddenValues = "changes-hidden-values"
	// ScenarioChangesRoutine serves the routine agent events of
	// ChangesRoutineInstances instances (SSM UpdateInstanceInformation every
	// five minutes, ECS SubmitTaskStateChange every ten) over the last
	// ChangesRoutineSpan, plus one real change.
	ScenarioChangesRoutine = "changes-routine"
	// ScenarioChangesEmailPrincipal serves changes by an IAM user and an SSO
	// session named by e-mail addresses (ChangesEmailPrincipals), and a
	// burst of failed calls by both people through one SSO permission set.
	ScenarioChangesEmailPrincipal = "changes-email-principal"
	// ScenarioChangesTiedSuspects is bad-deploy with two UpdateService calls
	// on the checkout service, by two pipelines, 90 seconds apart, before
	// the alarm: two candidates at the same scope.
	ScenarioChangesTiedSuspects = "changes-tied-suspects"
)

// State scenario sizes and identifiers tests assert on.
const (
	RevokedAccount          = "444455556666"
	NotEnabledRegion        = "af-south-1"
	ChangesLateDelay        = 20 * time.Minute
	ChangesLateSpan         = 2 * time.Hour
	ChangesMalformedCount   = 3
	ChangesMalformedValid   = 3
	ChangesRoutineInstances = 12
	ChangesRoutineSpan      = 2 * time.Hour
	ChangesEmailBurstSize   = 6
)

// ChangesEmailPrincipals are the e-mail addresses the email-principal
// scenario names principals by; none may reach an answer.
func ChangesEmailPrincipals() []string {
	return []string{"jane.doe@example.com", "john.smith@example.com"}
}

// HiddenCanaries are the secret-shaped values the hidden-values scenario
// plants; none may reach an answer.
func HiddenCanaries() []string {
	return []string{
		"vrs-hidden-env-password-0001", "vrs-hidden-tag-owner-0002", "vrs-hidden-parameter-0003",
		"dnJzLWhpZGRlbi11c2VyZGF0YS0wMDA0", "vrs-hidden-secret-string-0005", "vrs-hidden-access-secret-0006",
	}
}

func init() {
	for _, name := range []string{ScenarioRoleSessionExpired, ScenarioRegionNotEnabled, ScenarioRegionOptInUnrecorded, ScenarioChangesLateArrival, ScenarioChangesMalformed,
		ScenarioChangesHiddenValues, ScenarioChangesRoutine, ScenarioChangesEmailPrincipal, ScenarioChangesTiedSuspects} {
		worldScenarios[name] = true
		knownScenarios[name] = true
	}
}

// refuseState answers a request the state scenarios fail, before any other
// handling. It reports whether it answered.
func (s *Server) refuseState(writer http.ResponseWriter, request awsRequest, sessionToken bool) bool {
	if request.scenario == ScenarioRoleSessionExpired {
		if request.service == "sts" && request.action == "AssumeRole" && accountForRoleARN(request.params.Get("RoleArn")).ID == RevokedAccount {
			s.writeSTSError(writer, request.protocol, assumeDeniedWire(AssumeTrustDenied, request))
			return true
		}
		if sessionToken && request.account.ID == RevokedAccount {
			code := "ExpiredToken"
			if request.protocol == protocolJSON || request.protocol == protocolCBOR || (restJSONServices[request.service] && request.protocol == protocolREST) {
				code = "ExpiredTokenException"
			}
			writeErrorFor(writer, request, http.StatusForbidden, code, "The security token included in the request is expired")
			return true
		}
	}
	if s.regionNotEnabled(request) {
		switch {
		case request.scenario == ScenarioRegionOptInUnrecorded || s.scenario == ScenarioRegionOptInUnrecorded:
			writeErrorFor(writer, request, http.StatusUnauthorized, "OptInRequired", "You are not subscribed to this service. Please go to http://aws.amazon.com to subscribe.")
		case request.service == "ec2":
			writeErrorFor(writer, request, http.StatusUnauthorized, "AuthFailure", "AWS was not able to validate the provided access credentials")
		case request.protocol == protocolJSON || request.protocol == protocolCBOR || (restJSONServices[request.service] && request.protocol == protocolREST):
			writeErrorFor(writer, request, http.StatusBadRequest, "UnrecognizedClientException", "The security token included in the request is invalid.")
		default:
			writeErrorFor(writer, request, http.StatusForbidden, "InvalidClientTokenId", "The security token included in the request is invalid.")
		}
		return true
	}
	return false
}

// regionNotEnabled reports a request signed for the not-enabled Region.
func (s *Server) regionNotEnabled(request awsRequest) bool {
	if request.scenario == ScenarioSizingLarge && sizingOff(request.region) && request.service != "sts" {
		return true
	}
	if request.region != NotEnabledRegion {
		return false
	}
	for _, name := range []string{ScenarioRegionNotEnabled, ScenarioRegionOptInUnrecorded} {
		if request.scenario == name || s.scenario == name {
			return true
		}
	}
	return false
}

// notOptedInRegions are the Regions DescribeRegions lists as not enabled
// when the caller asks for every Region.
func (s *Server) notOptedInRegions(request awsRequest) []string {
	if request.params.Get("AllRegions") != "true" {
		return nil
	}
	if request.scenario == ScenarioRegionNotEnabled || s.scenario == ScenarioRegionNotEnabled {
		return []string{NotEnabledRegion}
	}
	return nil
}

func iamUser(account, name string) map[string]any {
	return map[string]any{"type": "IAMUser", "arn": "arn:aws:iam::" + account + ":user/" + name, "userName": name, "accountId": account}
}

// stateScenarioTrail is the CloudTrail history of the state scenarios.
func (w world) stateScenarioTrail() ([]trailEvent, bool) {
	read := trailEvent{id: EventDescribeInstances, name: "DescribeInstances", source: "ec2.amazonaws.com",
		at: w.now.Add(-30 * time.Minute).Truncate(time.Second), readOnly: true,
		document: map[string]any{"userIdentity": assumedRole(w.account, "versus-reader", "versus")}}
	pipeline := func(session string) map[string]any { return assumedRole(w.account, "terraform-apply", session) }
	switch w.scenario {
	case ScenarioChangesLateArrival:
		events := []trailEvent{read}
		// Only events already delivered: one at each whole minute up to
		// now − delay. Each keeps its time and ID on every read.
		newest := w.now.Add(-ChangesLateDelay).Truncate(time.Minute)
		for at := newest; !at.Before(w.now.Add(-ChangesLateSpan)); at = at.Add(-time.Minute) {
			stamp := strconv.FormatInt(at.Unix(), 10)
			events = append(events, trailEvent{id: "late-" + stamp, name: "PutRule", source: "events.amazonaws.com", at: at,
				document: map[string]any{"userIdentity": pipeline("ci-late-" + stamp),
					"userAgent":         "APN/1.0 HashiCorp/1.0 Terraform/1.9.0",
					"requestParameters": map[string]any{"name": "late-rule-" + at.UTC().Format("1504"), "eventBusName": "default"}}})
		}
		return events, true
	case ScenarioChangesMalformed:
		at := func(ago time.Duration) time.Time { return w.now.Add(-ago).Truncate(time.Second) }
		return []trailEvent{read,
			{id: "malformed-ok-1", name: "RunInstances", source: "ec2.amazonaws.com", at: at(90 * time.Minute), resources: []string{"i-0123456789abcdef0"},
				document: map[string]any{"userIdentity": pipeline("ci-run-77"), "requestParameters": map[string]any{"instanceType": "t3.small"},
					"responseElements": map[string]any{"instancesSet": map[string]any{"items": []any{map[string]any{"instanceId": "i-0123456789abcdef0"}}}}}},
			{id: "malformed-ok-2", name: "PutRule", source: "events.amazonaws.com", at: at(50 * time.Minute),
				document: map[string]any{"userIdentity": pipeline("ci-run-78"), "requestParameters": map[string]any{"name": "nightly-report"}}},
			// Stored with the actor unknown: the document cannot be parsed,
			// but the event itself is well formed.
			{id: "malformed-raw-doc", name: "PutRule", source: "events.amazonaws.com", at: at(70 * time.Minute), rawDocument: "{not json"},
			// Malformed: no EventId, no EventTime, a NUL in the event name.
			{id: "", name: "StopInstances", source: "ec2.amazonaws.com", at: at(2 * time.Hour), exactID: true,
				document: map[string]any{"userIdentity": pipeline("ci-run-79")}},
			{id: "malformed-no-time", name: "StartInstances", source: "ec2.amazonaws.com", at: at(100 * time.Minute), noTime: true,
				document: map[string]any{"userIdentity": pipeline("ci-run-80")}},
			{id: "malformed-nul-name", name: "Put\x00Rule", source: "events.amazonaws.com", at: at(80 * time.Minute),
				document: map[string]any{"userIdentity": pipeline("ci-run-81")}},
		}, true
	case ScenarioChangesHiddenValues:
		canary := HiddenCanaries()
		at := func(ago time.Duration) time.Time { return w.now.Add(-ago).Truncate(time.Second) }
		return []trailEvent{read,
			{id: "hidden-env", name: "UpdateFunctionConfiguration20150331v2", source: "lambda.amazonaws.com", at: at(110 * time.Minute),
				document: map[string]any{"userIdentity": pipeline("ci-run-90"), "requestParameters": map[string]any{"functionName": WorldFunction,
					"environment": map[string]any{"variables": map[string]any{"DB_PASSWORD": canary[0]}}}}},
			{id: "hidden-tag", name: "CreateTags", source: "ec2.amazonaws.com", at: at(100 * time.Minute), resources: []string{"i-0123456789abcdef1"},
				document: map[string]any{"userIdentity": pipeline("ci-run-91"), "requestParameters": map[string]any{
					"resourcesSet": map[string]any{"items": []any{map[string]any{"resourceId": "i-0123456789abcdef1"}}},
					"tagSet":       map[string]any{"items": []any{map[string]any{"key": "owner", "value": canary[1]}, map[string]any{"key": "team", "value": "payments"}}}}}},
			{id: "hidden-parameter", name: "PutParameter", source: "ssm.amazonaws.com", at: at(90 * time.Minute),
				document: map[string]any{"userIdentity": pipeline("ci-run-92"), "requestParameters": map[string]any{
					"name": "/checkout/db-password", "type": "SecureString", "value": canary[2], "overwrite": true}}},
			{id: "hidden-userdata", name: "RunInstances", source: "ec2.amazonaws.com", at: at(80 * time.Minute), resources: []string{"i-0123456789abcdef2"},
				document: map[string]any{"userIdentity": pipeline("ci-run-93"), "requestParameters": map[string]any{"instanceType": "t3.micro", "userData": canary[3]},
					"responseElements": map[string]any{"instancesSet": map[string]any{"items": []any{map[string]any{"instanceId": "i-0123456789abcdef2"}}}}}},
			{id: "hidden-secret", name: "PutSecretValue", source: "secretsmanager.amazonaws.com", at: at(70 * time.Minute),
				document: map[string]any{"userIdentity": pipeline("ci-run-94"), "requestParameters": map[string]any{"secretId": "checkout/db", "secretString": canary[4]}}},
			{id: "hidden-access-key", name: "CreateAccessKey", source: "iam.amazonaws.com", at: at(60 * time.Minute),
				document: map[string]any{"userIdentity": pipeline("ci-run-95"), "requestParameters": map[string]any{"userName": "deploy-bot"},
					"responseElements": map[string]any{"accessKey": map[string]any{"accessKeyId": "AKIAVERSUSHIDDEN0006", "secretAccessKey": canary[5], "userName": "deploy-bot", "status": "Active"}}}},
		}, true
	case ScenarioChangesRoutine:
		events := []trailEvent{read, {id: "routine-real-change", name: "RunInstances", source: "ec2.amazonaws.com",
			at: w.now.Add(-150 * time.Minute).Truncate(time.Second), resources: []string{"i-0123456789abcdef3"},
			document: map[string]any{"userIdentity": pipeline("ci-run-60"), "requestParameters": map[string]any{"instanceType": "t3.micro"},
				"responseElements": map[string]any{"instancesSet": map[string]any{"items": []any{map[string]any{"instanceId": "i-0123456789abcdef3"}}}}}}}
		for _, event := range routineEvents(w.account, w.now.Add(-ChangesRoutineSpan), w.now) {
			events = append(events, event)
		}
		return events, true
	case ScenarioChangesEmailPrincipal:
		people := ChangesEmailPrincipals()
		sso := func(person string) map[string]any {
			return assumedRole(w.account, "AWSReservedSSO_DeployAccess_0123456789abcdef", person)
		}
		at := func(ago time.Duration) time.Time { return w.now.Add(-ago).Truncate(time.Second) }
		events := []trailEvent{read,
			{id: "email-iam-user", name: "StopInstances", source: "ec2.amazonaws.com", at: at(50 * time.Minute), resources: []string{"i-0123456789abcdef0"},
				document: map[string]any{"userIdentity": iamUser(w.account, people[0]), "userAgent": "aws-cli/2.17.0",
					"requestParameters": map[string]any{"instancesSet": map[string]any{"items": []any{map[string]any{"instanceId": "i-0123456789abcdef0"}}}}}},
			{id: "email-sso-session", name: "UpdateService", source: "ecs.amazonaws.com", at: at(40 * time.Minute), resources: []string{w.serviceARN()},
				document: map[string]any{"userIdentity": sso(people[1]), "userAgent": "[ECSConsole]",
					"requestParameters": map[string]any{"cluster": WorldCluster, "service": WorldService, "desiredCount": 5},
					"responseElements":  map[string]any{"service": map[string]any{"serviceArn": w.serviceARN()}}}},
		}
		// Both people fail the same call through one permission set, one a
		// minute, in one burst window.
		start := w.now.Add(-100 * time.Minute).Truncate(time.Minute)
		for index := range ChangesEmailBurstSize {
			events = append(events, trailEvent{id: "email-burst-" + pad(index), name: "PutParameter", source: "ssm.amazonaws.com",
				at: start.Add(time.Duration(index) * time.Minute),
				document: map[string]any{"userIdentity": sso(people[index%2]), "errorCode": "AccessDenied", "errorMessage": "not authorized",
					"requestParameters": map[string]any{"name": "/checkout/feature-flags", "type": "String"}}})
		}
		return events, true
	case ScenarioChangesTiedSuspects:
		start := BadDeployStart(w.now)
		update := func(id, role, session string, at time.Time, desired int) trailEvent {
			return trailEvent{id: id, name: "UpdateService", source: "ecs.amazonaws.com", at: at, resources: []string{w.serviceARN()},
				document: map[string]any{"userIdentity": assumedRole(w.account, role, session), "userAgent": "aws-cli/2.17.0 md/command#ecs.update-service",
					"requestParameters": map[string]any{"cluster": WorldCluster, "service": WorldService, "desiredCount": desired},
					"responseElements":  map[string]any{"service": map[string]any{"serviceArn": w.serviceARN()}}}}
		}
		return []trailEvent{read,
			update("tied-update-a", "github-actions-deploy", "GitHubActions", start, 4),
			update("tied-update-b", "gitlab-ci-deploy", "GitLabRunner", start.Add(90*time.Second), 6),
		}, true
	}
	return nil, false
}

// routineEvents are the routine agent events of one account and Region in
// [from, to): each instance's SSM heartbeat on every five-minute boundary
// (offset by the instance number in seconds) and an ECS task state change
// on every ten-minute boundary. Times and IDs are absolute, so every read
// sees the same events.
func routineEvents(account string, from, to time.Time) []trailEvent {
	var events []trailEvent
	for boundary := from.Truncate(5 * time.Minute); boundary.Before(to); boundary = boundary.Add(5 * time.Minute) {
		stamp := strconv.FormatInt(boundary.Unix(), 10)
		for instance := range ChangesRoutineInstances {
			at := boundary.Add(time.Duration(instance) * time.Second)
			if at.Before(from) || !at.Before(to) {
				continue
			}
			id := "i-0routine" + strconv.Itoa(1000+instance)
			events = append(events, trailEvent{id: "routine-ssm-" + stamp + "-" + strconv.Itoa(instance), name: "UpdateInstanceInformation",
				source: "ssm.amazonaws.com", at: at,
				document: map[string]any{"userIdentity": assumedRole(account, "ssm-instance-role", id),
					"userAgent":         "aws-sdk-go/1.44.0 (amazon-ssm-agent/3.3.0)",
					"requestParameters": map[string]any{"instanceId": id, "agentVersion": "3.3.0"}}})
		}
		if boundary.Minute()%10 == 0 && !boundary.Before(from) {
			events = append(events, trailEvent{id: "routine-ecs-" + stamp, name: "SubmitTaskStateChange", source: "ecs.amazonaws.com", at: boundary,
				document: map[string]any{"userIdentity": assumedRole(account, "ecsInstanceRole", "i-0routine1000"),
					"requestParameters": map[string]any{"cluster": WorldCluster, "status": "RUNNING"}}})
		}
	}
	return events
}

// ChangesRoutineCount is how many routine events the routine scenario
// serves one account and Region with times in [from, to).
func ChangesRoutineCount(from, to time.Time) int {
	return len(routineEvents("", from.UTC(), to.UTC()))
}
