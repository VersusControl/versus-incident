package collect

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/config"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// Soak shape: one account polled every five minutes for a week, on a fake
// clock.
const (
	soakDays         = 7
	soakStep         = 5 * time.Minute
	soakRetention    = 48 * time.Hour
	soakChangeEvery  = 30 * time.Minute
	soakOverlap      = 15 * time.Minute
	soakRoutinePerMn = 200 // SSM agent updates a minute, counted not stored
)

// soakStore is a bounded snapshot store on the fake clock, with the
// generation caps sized per collection kind as the server sizes them.
func soakStore(now *time.Time, provider storage.Provider, collect config.AWSCollectConfig, retention time.Duration) *snapshot.Store {
	return snapshot.New(provider, tenancy.DefaultOrgScope(), snapshot.WithClock(func() time.Time { return *now }),
		snapshot.WithOptions(func() snapshot.Options {
			return snapshot.Options{
				MaxGenerations: collect.MaxGenerations(),
				GenerationsFor: func(service string) int {
					switch KindOf(service) {
					case KindHealth:
						return collect.HealthMaxGenerations()
					case KindInventory:
						return collect.InventoryMaxGenerations()
					}
					return collect.MaxGenerations()
				},
				MaxAge:          collect.MaxAge(),
				ChangeRetention: retention,
			}
		}))
}

// TestLongUptimeSoakStaysBounded runs a week of polls on a fake clock: a
// real change every 30 minutes, a steady stream of routine agent events
// and a failing job, every poll re-reading its 15-minute overlap, an alarm
// that flips every hour and an inventory that never changes. What is kept
// grows with real change inside retention, never with uptime: change rows,
// coverage intervals, minute counts and generations all stay bounded, and
// the last day still answers complete with every change in it.
func TestLongUptimeSoakStaysBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("simulates a week of polls; runs in the full suite")
	}
	ctx := context.Background()
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	now := start
	provider := storage.NewMemory()
	store := soakStore(&now, provider, config.AWSCollectConfig{}, soakRetention)
	regions := []string{testRegion, model.GlobalRegion}
	alarmARN := model.ARN("aws", "cloudwatch", testRegion, productionAccount, "alarm:checkout-5xx")
	inventoryUnit := model.Unit{Account: productionAccount, Region: testRegion, Service: "lambda"}
	healthUnit := model.Unit{Account: productionAccount, Region: testRegion, Service: "cloudwatch"}
	function := model.Resource{ARN: model.ARN("aws", "lambda", testRegion, productionAccount, "function:api"), Service: "lambda", Type: "function",
		Account: productionAccount, Region: testRegion, Name: "api"}

	realChange := func(at time.Time) model.Change {
		return model.Change{ID: "evt-" + strconv.FormatInt(at.Unix(), 10), Time: at, Account: productionAccount, Region: testRegion,
			Service: "lambda", EventSource: "lambda.amazonaws.com", EventName: "UpdateFunctionConfiguration20150331v2",
			Summary: "Configuration changed for function api", Resources: []string{function.ARN},
			Actor: model.Actor{Class: model.ActorPipeline, PrincipalARN: "arn:aws:iam::111122223333:role/deploy"}}
	}
	covered := map[string]time.Time{testRegion: start.Add(-24 * time.Hour), model.GlobalRegion: start.Add(-24 * time.Hour)}
	for ; !now.After(start.Add(soakDays * 24 * time.Hour)); now = now.Add(soakStep) {
		settled := now.Add(-soakOverlap)
		readFrom := now.Add(-soakStep - soakOverlap)
		for _, region := range regions {
			poll := snapshot.ChangePoll{
				Covered:     []changes.Interval{{From: covered[region], To: settled}},
				Read:        []changes.Interval{{From: readFrom, To: now}},
				HistoryFrom: start.Add(-24 * time.Hour),
			}
			if region == testRegion {
				// The poll re-reads the overlap, so the same events arrive
				// again; each is stored once.
				for at := readFrom.Truncate(soakChangeEvery); at.Before(now); at = at.Add(soakChangeEvery) {
					if !at.Before(readFrom) {
						poll.Found = append(poll.Found, realChange(at))
					}
				}
				for minute := readFrom.Truncate(time.Minute); minute.Before(now); minute = minute.Add(time.Minute) {
					poll.Counts = append(poll.Counts,
						storage.AWSChangeCount{Kind: storage.AWSChangeCountRoutine, EventName: "UpdateInstanceInformation", Minute: minute,
							Account: productionAccount, Region: testRegion, Service: "ssm", EventSource: "ssm.amazonaws.com", ActorClass: string(model.ActorAWSService), Events: soakRoutinePerMn},
						storage.AWSChangeCount{Kind: storage.AWSChangeCountFailure, EventName: "PutParameter", Actor: "arn:aws:iam::111122223333:role/config-sync",
							ErrorCode: "AccessDenied", Minute: minute, Account: productionAccount, Region: testRegion, Service: "ssm",
							EventSource: "ssm.amazonaws.com", ActorClass: string(model.ActorPipeline), Events: 10})
				}
			}
			if err := store.RecordChangePoll(productionAccount, region, poll); err != nil {
				t.Fatalf("poll at %s: %v", now, err)
			}
			covered[region] = settled
		}
		// Inventory every 15 minutes, never changing.
		if now.Sub(start)%(15*time.Minute) == 0 {
			if _, err := store.Commit(inventoryUnit, snapshot.Candidate{Resources: []model.Resource{function}}); err != nil {
				t.Fatal(err)
			}
		}
		// The alarm fires in odd hours and clears in even ones.
		var health []model.Health
		if hour := now.Truncate(time.Hour); (hour.Unix()/3600)%2 == 1 {
			health = []model.Health{{Resource: function.ARN, Account: productionAccount, Region: testRegion, Source: model.SourceAlarm,
				Status: model.HealthCritical, Summary: "Alarm checkout-5xx is in ALARM", Since: hour, Ref: alarmARN}}
		}
		if _, err := store.Commit(healthUnit, snapshot.Candidate{Health: health}); err != nil {
			t.Fatal(err)
		}
		if now.Sub(start)%time.Hour == 0 {
			for {
				result, err := store.SweepRetention(ctx)
				if err != nil {
					t.Fatalf("sweep at %s: %v", now, err)
				}
				if !result.More {
					break
				}
			}
		}
	}

	inventory, ok := storage.AWSInventory(provider)
	if !ok {
		t.Fatal("no inventory store")
	}
	scope := tenancy.DefaultOrgScope()
	rows, err := inventory.AWSChanges(ctx, scope, storage.AWSChangeQuery{Account: productionAccount})
	if err != nil {
		t.Fatal(err)
	}
	// Retention keeps 48 h of real changes: 96 plus the poll in flight.
	if limit := int(soakRetention/soakChangeEvery) + 2; len(rows) > limit || len(rows) < limit-4 {
		t.Errorf("change rows = %d after %d days, want at most %d", len(rows), soakDays, limit)
	}
	coverage, err := inventory.AWSChangeCoverage(ctx, scope, productionAccount, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range coverage {
		if len(pair.Intervals) > 2 {
			t.Errorf("pair %s/%s keeps %d coverage intervals", pair.Account, pair.Region, len(pair.Intervals))
		}
	}
	feeds, ok := storage.AWSChangeFeeds(provider)
	if !ok {
		t.Fatal("no change feed store")
	}
	stored, err := feeds.AWSChangeFeed(ctx, scope, storage.AWSChangeFeedQuery{Account: productionAccount, Since: start.Add(-48 * time.Hour), Until: now})
	if err != nil {
		t.Fatal(err)
	}
	// Two count keys, one row a minute each, inside retention.
	if limit := 2*int(soakRetention/time.Minute) + 2*int((soakStep+soakOverlap)/time.Minute); len(stored.Counts) > limit || len(stored.Counts) < 2*24*60 {
		t.Errorf("minute counts = %d after %d days, want at most %d", len(stored.Counts), soakDays, limit)
	}
	history, err := store.UnitHistory(ctx, snapshot.Query{Account: productionAccount})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, unit := range history {
		seen[unit.Unit.Service] = true
		switch unit.Unit.Service {
		case inventoryUnit.Service:
			if unit.Generations != 1 {
				t.Errorf("unchanged inventory keeps %d generations", unit.Generations)
			}
		case healthUnit.Service:
			// One generation an hour, kept by age (7 days) under the
			// health cap.
			if limit := soakDays*24 + 2; unit.Generations > limit || unit.Generations < 24 {
				t.Errorf("health keeps %d generations, want at most %d", unit.Generations, limit)
			}
		}
	}

	if !seen[inventoryUnit.Service] || !seen[healthUnit.Service] {
		t.Fatalf("unit history = %+v", history)
	}

	// After a week of uptime the last day still answers, complete, with
	// every real change in it.
	answer, err := snapshot.NewChangeFeed(store, snapshot.WithFeedClock(func() time.Time { return now })).Answer(ctx, snapshot.FeedRequest{Window: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if !answer.Complete || answer.Summary.Total < int64(24*time.Hour/soakChangeEvery)-1 || answer.Summary.Routine == 0 || answer.Summary.Errors == 0 {
		t.Fatalf("last day after a week = complete %v summary %+v gaps %+v", answer.Complete, answer.Summary, answer.Gaps)
	}
}
