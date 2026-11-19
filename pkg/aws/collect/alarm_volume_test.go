package collect

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot/alarmsuspect"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

const (
	flappingAlarms = 200
	flapPeriod     = 20 * time.Minute
	flapFiring     = 4 * time.Minute
)

// firingAt lists the alarms in ALARM at one health pass: alarm i fires for
// four minutes of every twenty, offset by its index, so the set changes on
// every pass.
func firingAt(start, at time.Time) []model.Health {
	var out []model.Health
	for index := range flappingAlarms {
		offset := time.Duration(index%10) * 2 * time.Minute
		phase := (at.Sub(start) + offset) % flapPeriod
		if phase >= flapFiring {
			continue
		}
		name := "flap-" + strconv.Itoa(index)
		out = append(out, model.Health{Resource: model.ARN("aws", "lambda", testRegion, productionAccount, "function:"+name), Account: productionAccount,
			Region: testRegion, Source: model.SourceAlarm, Status: model.HealthCritical, Summary: "Alarm " + name + " is in ALARM",
			Since: at.Add(-phase), Ref: model.ARN("aws", "cloudwatch", testRegion, productionAccount, "alarm:"+name)})
	}
	return out
}

func firingRefs(items []model.Health) []string {
	var refs []string
	for _, item := range items {
		if item.Source == model.SourceAlarm {
			refs = append(refs, item.Ref)
		}
	}
	slices.Sort(refs)
	return slices.Compact(refs)
}

// TestFlappingAlarmsKeepADayOfHistory: 200 alarms flap for 24 hours on the
// 2-minute health pass, so every pass is a new generation. With the health
// cap sized from its interval every episode stays readable and the alarm
// line's history starts at the first pass. With the old flat cap of 48 the
// history is about 96 minutes deep, and the alarm line says where it
// starts instead of showing the older hours as quiet.
func TestFlappingAlarmsKeepADayOfHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("commits 720 health generations of 200 alarms into two stores; runs in the full suite")
	}
	ctx := context.Background()
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	now := start
	sized := soakStore(&now, storage.NewMemory(), config.AWSCollectConfig{}, 14*24*time.Hour)
	flatNow := start
	flat := soakStore(&flatNow, storage.NewMemory(), config.AWSCollectConfig{
		Intervals: config.AWSCollectIntervalsConfig{Health: "24h", Inventory: "24h"}}, 14*24*time.Hour)
	unit := model.Unit{Account: productionAccount, Region: testRegion, Service: "cloudwatch"}
	interval := config.DefaultAWSHealthInterval
	end := start.Add(24 * time.Hour)
	for ; now.Before(end); now = now.Add(interval) {
		flatNow = now
		candidate := snapshot.Candidate{Health: firingAt(start, now)}
		if _, err := sized.Commit(unit, candidate); err != nil {
			t.Fatal(err)
		}
		if _, err := flat.Commit(unit, candidate); err != nil {
			t.Fatal(err)
		}
	}
	last := now.Add(-interval)
	scope := snapshot.AlarmScope{Account: productionAccount, Region: testRegion, Since: end.Add(-24 * time.Hour), Until: end}

	_, starts, err := alarmsuspect.New(sized).AlarmLine(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if starts == nil || !starts.Equal(start) {
		t.Fatalf("sized alarm history starts at %v, want the first pass %v", starts, start)
	}
	// Every episode is readable: each pass's firing set, as of that pass.
	for at := start; at.Before(end); at = at.Add(interval * 7) {
		health, err := sized.Health(ctx, snapshot.Query{Account: productionAccount, Region: testRegion, AsOf: at})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := firingRefs(health.Health), firingRefs(firingAt(start, at)); !slices.Equal(got, want) {
			t.Fatalf("as of %s: %d firing alarms read, want %d", at, len(got), len(want))
		}
	}

	_, flatStarts, err := alarmsuspect.New(flat).AlarmLine(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	depth := time.Duration(config.DefaultAWSHistoryGenerations-1) * interval
	if flatStarts == nil || flatStarts.Before(last.Add(-depth-interval)) || flatStarts.After(last.Add(-depth+interval)) {
		t.Fatalf("flat-cap alarm history starts at %v, want about %v", flatStarts, last.Add(-depth))
	}
}
