package suspect

import (
	"context"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

// unfiltered returns every change it holds, whatever window it is asked
// for, so the test sees Find's own window rule rather than the source's.
type unfiltered struct{ changes []model.Change }

func (u unfiltered) Changes(context.Context, string, string, time.Time, time.Time) ([]model.Change, error) {
	return u.changes, nil
}

func (u unfiltered) Settled(string, string, time.Time, time.Time) bool { return true }

// A change made at or after the alarm start, or before the lookback, is
// never a candidate, even when the source hands it back.
func TestFindNeverPicksAChangeOutsideItsWindow(t *testing.T) {
	asg := "arn:aws:autoscaling:eu-west-1:111122223333:autoScalingGroup:*:autoScalingGroupName/web"
	scale := func(id string, at time.Time) model.Change {
		return trail(t, accountA, euWest1, id, "autoscaling.amazonaws.com", "SetDesiredCapacity", at,
			pipeline, map[string]any{"autoScalingGroupName": "web", "desiredCapacity": 9})
	}
	source := unfiltered{changes: []model.Change{
		scale("evt-after", alarmStart.Add(20*time.Minute)),
		scale("evt-at-start", alarmStart),
		scale("evt-too-old", alarmStart.Add(-AlarmLookback-time.Minute)),
	}}
	anchor := Anchor{Kind: AnchorAlarm, ID: "web-5xx", Account: accountA, Regions: []string{euWest1}, Start: alarmStart,
		Targets: map[string]Scope{asg: ScopeResource}}
	result, err := Find(context.Background(), anchor, source, source)
	if err != nil || result.Status != StatusNone || len(result.Candidates) != 0 {
		t.Fatalf("out-of-window change picked: %+v %v", result, err)
	}
}
