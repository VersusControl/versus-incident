package fakeaws

import (
	"net/http"
	"strconv"
	"time"
)

// Change-history scenarios. Each serves the collector world with a
// CloudTrail history shaped to reach one state of the Changes page; the
// inventory, alarms and probes are the world's.
const (
	// ScenarioChangesQuiet serves only read events: a complete, quiet
	// window ("No changes").
	ScenarioChangesQuiet = "changes-quiet"
	// ScenarioChangesServiceOnly serves only changes AWS services made
	// (Auto Scaling, ECS): "No changes by people or pipelines".
	ScenarioChangesServiceOnly = "changes-aws-service-only"
	// ScenarioChangesErrorBurst serves ChangesBurstSize failed
	// ssm:PutParameter calls by one role, one a minute.
	ScenarioChangesErrorBurst = "changes-error-burst"
	// ScenarioChangesBusy serves ChangesBusySize write events over the 24
	// hours before the world start (the first request for the scenario).
	// Their times are absolute: every read sees each event at the same time.
	ScenarioChangesBusy = "changes-busy"
	// ScenarioChangesCloudTrailDenied refuses LookupEvents for the Staging
	// account only; the other accounts read normally.
	ScenarioChangesCloudTrailDenied = "changes-cloudtrail-denied"
	// ScenarioChangesThrottled answers every LookupEvents call with
	// ThrottlingException.
	ScenarioChangesThrottled = "changes-throttled"
	// ScenarioChangesAlarmNoSuspect fires the checkout-5xx alarm forty
	// minutes ago with no write event in the hour before it.
	ScenarioChangesAlarmNoSuspect = "changes-alarm-no-suspect"
	// ScenarioChangesUnknownActor serves a change by an identity of an
	// unknown type through an API no template covers.
	ScenarioChangesUnknownActor = "changes-unknown-actor"
)

// Change scenario sizes and identifiers tests assert on.
const (
	ChangesBurstSize        = 12
	ChangesBusySize         = 5000
	ChangesDeniedAccount    = "444455556666"
	ChangesUnknownEventName = "CreateWidget"
	ChangesUnknownSource    = "widgets.amazonaws.com"
)

func init() {
	for _, name := range []string{ScenarioChangesQuiet, ScenarioChangesServiceOnly, ScenarioChangesErrorBurst, ScenarioChangesBusy,
		ScenarioChangesCloudTrailDenied, ScenarioChangesThrottled, ScenarioChangesAlarmNoSuspect, ScenarioChangesUnknownActor} {
		worldScenarios[name] = true
		knownScenarios[name] = true
	}
}

// alarmFiring reports whether the world's 5xx alarm and its composite are
// in ALARM, and since when.
func (w world) alarmFiring() (time.Time, bool) {
	switch {
	case w.badDeploy():
		return BadDeployStart(w.now).Add(4 * time.Minute), true
	case w.scenario == ScenarioChangesAlarmNoSuspect:
		return w.now.Add(-40 * time.Minute).Truncate(time.Second), true
	}
	return time.Time{}, false
}

// refuseLookup answers LookupEvents with the scenario's error, if any.
func (s *Server) refuseLookup(writer http.ResponseWriter, request awsRequest, w world) bool {
	switch {
	case w.scenario == ScenarioChangesCloudTrailDenied && w.account == ChangesDeniedAccount:
		writeErrorFor(writer, request, http.StatusBadRequest, "AccessDeniedException",
			"User: arn:aws:sts::"+w.account+":assumed-role/versus-reader/versus is not authorized to perform: cloudtrail:LookupEvents")
		return true
	case w.scenario == ScenarioChangesThrottled:
		writeErrorFor(writer, request, http.StatusBadRequest, "ThrottlingException", "Rate exceeded")
		return true
	}
	return false
}

// changeScenarioTrail is the scenario's CloudTrail history, newest first
// once sorted by the caller. ok is false outside the change scenarios.
func (w world) changeScenarioTrail() ([]trailEvent, bool) {
	read := trailEvent{id: EventDescribeInstances, name: "DescribeInstances", source: "ec2.amazonaws.com",
		at: w.now.Add(-30 * time.Minute).Truncate(time.Second), readOnly: true,
		document: map[string]any{"userIdentity": assumedRole(w.account, "versus-reader", "versus")}}
	switch w.scenario {
	case ScenarioChangesQuiet:
		return []trailEvent{read}, true
	case ScenarioChangesServiceOnly:
		service := func(id, invokedBy, source, name string, ago time.Duration, request map[string]any) trailEvent {
			return trailEvent{id: id, name: name, source: source, at: w.now.Add(-ago).Truncate(time.Second),
				document: map[string]any{"userIdentity": map[string]any{"type": "AWSService", "invokedBy": invokedBy},
					"requestParameters": request}}
		}
		return []trailEvent{read,
			service("svc-0001", "autoscaling.amazonaws.com", "autoscaling.amazonaws.com", "SetDesiredCapacity", 2*time.Hour,
				map[string]any{"autoScalingGroupName": "versus-web", "desiredCapacity": 6}),
			service("svc-0002", "ecs.amazonaws.com", "ecs.amazonaws.com", "UpdateService", 90*time.Minute,
				map[string]any{"cluster": WorldCluster, "service": WorldService, "desiredCount": 5}),
		}, true
	case ScenarioChangesErrorBurst:
		events := []trailEvent{read}
		start := w.now.Add(-2 * time.Hour).Truncate(time.Minute)
		for index := range ChangesBurstSize {
			events = append(events, trailEvent{id: "burst-" + pad(index), name: "PutParameter", source: "ssm.amazonaws.com",
				at: start.Add(time.Duration(index) * time.Minute),
				document: map[string]any{"userIdentity": assumedRole(w.account, "config-sync", "job-7"),
					"errorCode": "AccessDenied", "errorMessage": "not authorized",
					"requestParameters": map[string]any{"name": "/checkout/feature-flags", "type": "String"}}})
		}
		return events, true
	case ScenarioChangesBusy:
		events := make([]trailEvent, 0, ChangesBusySize+1)
		events = append(events, read)
		step := 24 * time.Hour / ChangesBusySize
		for index := range ChangesBusySize {
			events = append(events, trailEvent{id: "busy-" + pad(index), name: "SetDesiredCapacity", source: "autoscaling.amazonaws.com",
				at: w.start.Add(-time.Duration(index+1) * step).Truncate(time.Second),
				document: map[string]any{"userIdentity": assumedRole(w.account, "terraform-apply", "ci-run-"+strconv.Itoa(index)),
					"userAgent":         "APN/1.0 HashiCorp/1.0 Terraform/1.9.0",
					"requestParameters": map[string]any{"autoScalingGroupName": "fleet-" + strconv.Itoa(index%50), "desiredCapacity": index % 9}}})
		}
		return events, true
	case ScenarioChangesAlarmNoSuspect:
		// The only write is three hours old, far outside the alarm's hour.
		return []trailEvent{read, {id: EventRunInstances, name: "RunInstances", source: "ec2.amazonaws.com", at: w.now.Add(-3 * time.Hour).Truncate(time.Second),
			resources: []string{"i-0123456789abcdef0"},
			document: map[string]any{"userIdentity": assumedRole(w.account, "terraform-apply", "ci-run-1234"),
				"userAgent":         "APN/1.0 HashiCorp/1.0 Terraform/1.9.0",
				"requestParameters": map[string]any{"instanceType": "t3.micro"},
				"responseElements":  map[string]any{"instancesSet": map[string]any{"items": []any{map[string]any{"instanceId": "i-0123456789abcdef0"}}}}}}}, true
	case ScenarioChangesUnknownActor:
		return []trailEvent{read, {id: "unknown-0001", name: ChangesUnknownEventName, source: ChangesUnknownSource,
			at:       w.now.Add(-45 * time.Minute).Truncate(time.Second),
			document: map[string]any{"userIdentity": map[string]any{"type": "Unknown"}}}}, true
	}
	return w.stateScenarioTrail()
}

func pad(index int) string {
	text := strconv.Itoa(index)
	for len(text) < 5 {
		text = "0" + text
	}
	return text
}
