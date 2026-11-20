package fakeaws

import (
	"os"
	"time"
)

// EnvChangeServices set to "1" mixes more services into the CloudTrail
// history, the way a real account reads: a few RDS and IAM writes among
// steady Config and Athena noise. It is off by default, so every test that
// counts the world's events sees the same history as before. The Changes
// page service filter (A11 C13) is checked against it in the browser.
const EnvChangeServices = "FAKEAWS_CHANGE_SERVICES"

// Identifiers and cadences of the service mix, for tests to assert on.
const (
	ServicesDatabase = "orders-db"
	ServicesRole     = "checkout-task"
	// ServicesNoiseStart is how long before the world start the Config and
	// Athena noise begins: outside the bad-deploy alarm's one-hour look-back,
	// so the ECS deploy stays the likely cause.
	ServicesNoiseStart  = 2 * time.Hour
	ServicesConfigEvery = 15 * time.Minute
	ServicesConfigCount = 88
	ServicesAthenaEvery = 40 * time.Minute
	ServicesAthenaCount = 32
	// ServicesBusyRDSEvery spaces the RDS writes changes-busy interleaves
	// with its Auto Scaling events, over the same 24 hours.
	ServicesBusyRDSEvery = 5 * time.Minute
	ServicesBusyRDSCount = 288
	// Offsets after the world start of the writes a Compare from the first
	// snapshot finds.
	ServicesLiveConfig = 1 * time.Minute
	ServicesLiveRDS    = 2 * time.Minute
	ServicesLiveIAM    = 3 * time.Minute
	ServicesLiveEC2    = 3*time.Minute + 30*time.Second
	// servicesGlobalRegion is where CloudTrail records IAM, a global
	// service.
	servicesGlobalRegion = "us-east-1"
)

func changeServicesOn() bool { return os.Getenv(EnvChangeServices) == "1" }

// serviceMix is the extra history EnvChangeServices adds. base is true on
// the world's own trail (bad-deploy, changes-cloudtrail-denied and the
// other collector scenarios); among the change scenarios only changes-busy
// gains events, so quiet and empty states stay quiet.
func (w world) serviceMix(base bool) []trailEvent {
	if !changeServicesOn() {
		return nil
	}
	if base {
		return w.servicesTrail()
	}
	if w.scenario == ScenarioChangesBusy {
		return w.busyDatabaseWrites()
	}
	return nil
}

// servicesTrail anchors every event to the world start, so each read sees
// each event at the same time.
func (w world) servicesTrail() []trailEvent {
	at := func(ago time.Duration) time.Time { return w.start.Add(-ago).Truncate(time.Second) }
	pipeline := assumedRole(w.account, "terraform-apply", "ci-run-5150")
	person := assumedRole(w.account, "AWSReservedSSO_DBA_0123456789abcdef", "jordan.lee")
	database := "arn:aws:rds:" + w.region + ":" + w.account + ":db:" + ServicesDatabase
	var events []trailEvent
	if w.region == servicesGlobalRegion {
		role := "arn:aws:iam::" + w.account + ":role/" + ServicesRole
		events = append(events,
			trailEvent{id: "svc-rds-modify-class", name: "ModifyDBInstance", source: "rds.amazonaws.com", at: at(5 * time.Hour),
				resources: []string{database},
				document: map[string]any{"userIdentity": pipeline, "userAgent": "APN/1.0 HashiCorp/1.0 Terraform/1.9.0",
					"requestParameters": map[string]any{"dBInstanceIdentifier": ServicesDatabase, "dBInstanceClass": "db.r6g.large", "applyImmediately": true},
					"responseElements":  map[string]any{"dBInstanceArn": database, "dBInstanceStatus": "modifying"}}},
			trailEvent{id: "svc-rds-reboot", name: "RebootDBInstance", source: "rds.amazonaws.com", at: at(4*time.Hour + 50*time.Minute),
				resources: []string{database},
				document: map[string]any{"userIdentity": person, "userAgent": "[RDSConsole]",
					"requestParameters": map[string]any{"dBInstanceIdentifier": ServicesDatabase, "forceFailover": false}}},
			trailEvent{id: "svc-rds-modify-backup", name: "ModifyDBInstance", source: "rds.amazonaws.com", at: at(13 * time.Hour),
				resources: []string{database},
				document: map[string]any{"userIdentity": pipeline, "userAgent": "APN/1.0 HashiCorp/1.0 Terraform/1.9.0",
					"requestParameters": map[string]any{"dBInstanceIdentifier": ServicesDatabase, "backupRetentionPeriod": 14}}},
			trailEvent{id: "svc-iam-put-role-policy", name: "PutRolePolicy", source: "iam.amazonaws.com", at: at(8 * time.Hour),
				resources: []string{role},
				document: map[string]any{"userIdentity": pipeline, "userAgent": "APN/1.0 HashiCorp/1.0 Terraform/1.9.0",
					"requestParameters": map[string]any{"roleName": ServicesRole, "policyName": "orders-db-connect",
						"policyDocument": `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"rds-db:connect","Resource":"*"}]}`}}},
			trailEvent{id: "svc-iam-attach-role-policy", name: "AttachRolePolicy", source: "iam.amazonaws.com", at: at(8*time.Hour - 30*time.Second),
				resources: []string{role},
				document: map[string]any{"userIdentity": pipeline, "userAgent": "APN/1.0 HashiCorp/1.0 Terraform/1.9.0",
					"requestParameters": map[string]any{"roleName": ServicesRole, "policyArn": "arn:aws:iam::aws:policy/AmazonRDSReadOnlyAccess"}}},
		)
	}
	evaluator := assumedRole(w.account, "config-rule-evaluator", "config-rules")
	for index := range ServicesConfigCount {
		events = append(events, trailEvent{id: "svc-config-" + pad(index), name: "StartConfigRulesEvaluation", source: "config.amazonaws.com",
			at: at(ServicesNoiseStart + time.Duration(index)*ServicesConfigEvery),
			document: map[string]any{"userIdentity": evaluator, "userAgent": "aws-sdk-go-v2/1.30.0",
				"requestParameters": map[string]any{"configRuleNames": []any{"required-tags", "rds-storage-encrypted"}}}})
	}
	reporting := assumedRole(w.account, "quicksight-reporting", "dashboard-refresh")
	for index := range ServicesAthenaCount {
		events = append(events, trailEvent{id: "svc-athena-" + pad(index), name: "StartQueryExecution", source: "athena.amazonaws.com",
			at: at(ServicesNoiseStart + 10*time.Minute + time.Duration(index)*ServicesAthenaEvery),
			document: map[string]any{"userIdentity": reporting, "userAgent": "aws-sdk-java/2.25.0",
				"requestParameters": map[string]any{"workGroup": "primary"}}})
	}
	return append(events, w.servicesAfterStart(database, pipeline, person, evaluator)...)
}

// servicesAfterStart are writes made after the world start, so a Compare
// from the first snapshot has changes between: one RDS, one IAM, one EC2
// and one Config write. CloudTrail serves each only once its time has
// passed.
func (w world) servicesAfterStart(database string, pipeline, person, evaluator map[string]any) []trailEvent {
	after := func(offset time.Duration) time.Time { return w.start.Add(offset).Truncate(time.Second) }
	events := []trailEvent{
		{id: "svc-config-live", name: "StartConfigRulesEvaluation", source: "config.amazonaws.com", at: after(ServicesLiveConfig),
			document: map[string]any{"userIdentity": evaluator, "userAgent": "aws-sdk-go-v2/1.30.0",
				"requestParameters": map[string]any{"configRuleNames": []any{"required-tags"}}}},
	}
	if w.region == servicesGlobalRegion {
		instance := "i-0123456789abcdef0"
		events = append(events,
			trailEvent{id: "svc-rds-modify-live", name: "ModifyDBInstance", source: "rds.amazonaws.com", at: after(ServicesLiveRDS),
				resources: []string{database},
				document: map[string]any{"userIdentity": person, "userAgent": "[RDSConsole]",
					"requestParameters": map[string]any{"dBInstanceIdentifier": ServicesDatabase, "maxAllocatedStorage": 500}}},
			trailEvent{id: "svc-iam-attach-live", name: "AttachRolePolicy", source: "iam.amazonaws.com", at: after(ServicesLiveIAM),
				resources: []string{"arn:aws:iam::" + w.account + ":role/" + ServicesRole},
				document: map[string]any{"userIdentity": pipeline, "userAgent": "APN/1.0 HashiCorp/1.0 Terraform/1.9.0",
					"requestParameters": map[string]any{"roleName": ServicesRole, "policyArn": "arn:aws:iam::aws:policy/CloudWatchAgentServerPolicy"}}},
			trailEvent{id: "svc-ec2-tags-live", name: "CreateTags", source: "ec2.amazonaws.com", at: after(ServicesLiveEC2),
				resources: []string{instance},
				document: map[string]any{"userIdentity": pipeline, "userAgent": "APN/1.0 HashiCorp/1.0 Terraform/1.9.0",
					"requestParameters": map[string]any{"resourcesSet": map[string]any{"items": []any{map[string]any{"resourceId": instance}}},
						"tagSet": map[string]any{"items": []any{map[string]any{"key": "team", "value": "checkout"}}}}}},
		)
	}
	served := events[:0]
	for _, event := range events {
		if !event.at.After(w.now) {
			served = append(served, event)
		}
	}
	return served
}

// busyDatabaseWrites interleaves RDS writes with changes-busy's Auto
// Scaling events, so a filtered page skips rows of another service.
func (w world) busyDatabaseWrites() []trailEvent {
	database := "arn:aws:rds:" + w.region + ":" + w.account + ":db:" + ServicesDatabase
	pipeline := assumedRole(w.account, "terraform-apply", "ci-run-busy")
	events := make([]trailEvent, 0, ServicesBusyRDSCount)
	for index := range ServicesBusyRDSCount {
		events = append(events, trailEvent{id: "busy-rds-" + pad(index), name: "ModifyDBInstance", source: "rds.amazonaws.com",
			at:        w.start.Add(-time.Duration(index)*ServicesBusyRDSEvery - 7*time.Second).Truncate(time.Second),
			resources: []string{database},
			document: map[string]any{"userIdentity": pipeline, "userAgent": "APN/1.0 HashiCorp/1.0 Terraform/1.9.0",
				"requestParameters": map[string]any{"dBInstanceIdentifier": ServicesDatabase, "allocatedStorage": 100 + index}}})
	}
	return events
}
