package fakeaws

import (
	"testing"
	"time"
)

func servicesCount(events []trailEvent) map[string]int {
	counts := map[string]int{}
	for _, event := range events {
		if !event.readOnly {
			counts[event.source]++
		}
	}
	return counts
}

func TestChangeServicesOffKeepsTheTrail(t *testing.T) {
	t.Setenv(EnvChangeServices, "")
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	w := world{account: "111122223333", region: "us-east-1", scenario: ScenarioBadDeploy, now: now, start: now}
	counts := servicesCount(w.trail())
	if counts["rds.amazonaws.com"] != 0 || counts["iam.amazonaws.com"] != 0 || counts["config.amazonaws.com"] != 0 {
		t.Fatalf("service mix leaked with the env unset: %v", counts)
	}
}

func TestChangeServicesMixesRDSIAMAndNoise(t *testing.T) {
	t.Setenv(EnvChangeServices, "1")
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	east := world{account: "111122223333", region: "us-east-1", scenario: ScenarioBadDeploy, now: now, start: now}
	counts := servicesCount(east.trail())
	want := map[string]int{"rds.amazonaws.com": 3, "iam.amazonaws.com": 2, "config.amazonaws.com": ServicesConfigCount,
		"athena.amazonaws.com": ServicesAthenaCount, "ecs.amazonaws.com": 2}
	for source, count := range want {
		if counts[source] != count {
			t.Fatalf("%s = %d, want %d (all %v)", source, counts[source], count, counts)
		}
	}
	// IAM is global: CloudTrail records it in us-east-1 only.
	west := world{account: "111122223333", region: "eu-west-1", scenario: ScenarioBadDeploy, now: now, start: now}
	if counts := servicesCount(west.trail()); counts["iam.amazonaws.com"] != 0 || counts["rds.amazonaws.com"] != 0 || counts["config.amazonaws.com"] != ServicesConfigCount {
		t.Fatalf("eu-west-1 mix = %v", counts)
	}
	// Every noise and mix event is outside the alarm's one-hour look-back
	// and inside the 24-hour window.
	alarm, _ := east.alarmFiring()
	for _, event := range east.serviceMix(true) {
		if event.at.After(alarm.Add(-time.Hour)) || event.at.Before(now.Add(-24*time.Hour)) {
			t.Fatalf("%s at %s: inside the alarm look-back or outside the window", event.id, event.at)
		}
	}
	// Writes after the world start are served once their time has passed.
	later := east
	later.now = now.Add(5 * time.Minute)
	if counts := servicesCount(later.trail()); counts["rds.amazonaws.com"] != 4 || counts["iam.amazonaws.com"] != 3 ||
		counts["ec2.amazonaws.com"] != 2 || counts["config.amazonaws.com"] != ServicesConfigCount+1 {
		t.Fatalf("five minutes after the start = %v", counts)
	}
	// Quiet stays quiet; busy gains interleaved RDS writes.
	quiet := world{account: "111122223333", region: "us-east-1", scenario: ScenarioChangesQuiet, now: now, start: now}
	if counts := servicesCount(quiet.trail()); len(counts) != 0 {
		t.Fatalf("changes-quiet gained writes: %v", counts)
	}
	busy := world{account: "111122223333", region: "us-east-1", scenario: ScenarioChangesBusy, now: now, start: now}
	if counts := servicesCount(busy.trail()); counts["rds.amazonaws.com"] != ServicesBusyRDSCount || counts["autoscaling.amazonaws.com"] != ChangesBusySize {
		t.Fatalf("changes-busy mix = %v", counts)
	}
}
