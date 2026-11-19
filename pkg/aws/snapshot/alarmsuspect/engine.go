// Package alarmsuspect puts the shared suspect engine behind the change
// list: it lists the alarms in scope, says where the alarm history starts,
// and asks the engine which change most likely started each alarm. It never
// guesses: a read error is incomplete, never "none".
package alarmsuspect

import (
	"context"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/aws/suspect"
)

// AlarmService is the unit service whose generations hold the alarms.
const AlarmService = "cloudwatch"

// Engine is the change list's suspect engine over one snapshot store.
type Engine struct {
	store *snapshot.Store
}

var (
	_ snapshot.SuspectEngine = (*Engine)(nil)
	_ snapshot.AlarmLine     = (*Engine)(nil)
)

// New builds the engine over store.
func New(store *snapshot.Store) *Engine { return &Engine{store: store} }

// alarmsIn reads the firing alarms in scope with the inventory they are
// resolved against.
func (engine *Engine) alarmsIn(ctx context.Context, account, region string) ([]suspect.Alarm, []model.Resource, error) {
	if region == model.GlobalRegion {
		return nil, nil, nil
	}
	health, err := engine.store.Health(ctx, snapshot.Query{Account: account, Region: region})
	if err != nil {
		return nil, nil, err
	}
	inventory, err := engine.store.Resources(ctx, snapshot.Query{Account: account})
	if err != nil {
		return nil, nil, err
	}
	return suspect.Alarms(health.Health, inventory.Resources), inventory.Resources, nil
}

// AlarmLine lists the alarms that started before the window ends, and the
// latest of the oldest kept alarm generations in scope: before it, the
// alarm line has no history.
func (engine *Engine) AlarmLine(ctx context.Context, scope snapshot.AlarmScope) ([]snapshot.FeedAlarm, *time.Time, error) {
	alarms, _, err := engine.alarmsIn(ctx, scope.Account, scope.Region)
	if err != nil {
		return nil, nil, err
	}
	out := make([]snapshot.FeedAlarm, 0, len(alarms))
	for _, alarm := range alarms {
		if alarm.StartKnown && !alarm.Start.Before(scope.Until) {
			continue
		}
		out = append(out, feedAlarm(alarm))
	}
	var starts *time.Time
	if scope.Region != model.GlobalRegion {
		history, err := engine.store.UnitHistory(ctx, snapshot.Query{Account: scope.Account, Region: scope.Region, Service: AlarmService})
		if err != nil {
			return nil, nil, err
		}
		oldest := make([]time.Time, 0, len(history))
		for _, unit := range history {
			oldest = append(oldest, unit.Oldest)
		}
		if at := suspect.AlarmHistoryStarts(oldest); !at.IsZero() {
			starts = &at
		}
	}
	return out, starts, nil
}

func feedAlarm(alarm suspect.Alarm) snapshot.FeedAlarm {
	out := snapshot.FeedAlarm{ID: alarm.ID, Name: alarm.Name, Account: alarm.Account, Region: alarm.Region,
		ResourceARNs: append([]string{}, alarm.ResourceARNs...), StartKnown: alarm.StartKnown, End: alarm.End}
	if alarm.StartKnown {
		start := alarm.Start
		out.Start = &start
	}
	return out
}

func engineAlarm(alarm snapshot.FeedAlarm) suspect.Alarm {
	out := suspect.Alarm{ID: alarm.ID, Name: alarm.Name, Account: alarm.Account, Region: alarm.Region,
		ResourceARNs: append([]string{}, alarm.ResourceARNs...), StartKnown: alarm.StartKnown && alarm.Start != nil, End: alarm.End}
	if out.StartKnown {
		out.Start = *alarm.Start
	}
	return out
}

// Suspect asks the engine about each alarm in the answer, in alarm-line
// order, and returns the first found suspect; otherwise the first alarm's
// answer. Coverage is the engine's own snapshot read and the answer's
// settled_until and gaps together, so "none" needs both to agree.
func (engine *Engine) Suspect(ctx context.Context, request snapshot.SuspectRequest) snapshot.FeedSuspect {
	if len(request.Alarms) == 0 {
		return snapshot.FeedSuspect{Status: snapshot.SuspectNoAlarm}
	}
	inventory, err := engine.store.Resources(ctx, snapshot.Query{Account: request.Account})
	if err != nil {
		return snapshot.FeedSuspect{Status: snapshot.SuspectIncomplete, AlarmID: request.Alarms[0].ID}
	}
	var first *snapshot.FeedSuspect
	for _, item := range request.Alarms {
		source := suspect.NewSnapshot(engine.store)
		coverage := suspect.CoverageFunc(func(account, region string, from, to time.Time) bool {
			if !source.Settled(account, region, from, to) {
				return false
			}
			return request.Settled == nil || request.Settled(account, region, from, to)
		})
		result, err := suspect.Find(ctx, engineAlarm(item).Anchor(inventory.Resources), source, coverage)
		answer := mapResult(item.ID, result, err)
		if answer.Status == snapshot.SuspectFound {
			return answer
		}
		if first == nil {
			first = &answer
		}
	}
	return *first
}

// mapResult turns the engine's result into the answer's suspect object.
// An error reads incomplete: the window was not read, so "none" would be a
// guess.
func mapResult(alarmID string, result suspect.Result, err error) snapshot.FeedSuspect {
	if err != nil {
		return snapshot.FeedSuspect{Status: snapshot.SuspectIncomplete, AlarmID: alarmID}
	}
	answer := snapshot.FeedSuspect{AlarmID: alarmID}
	switch result.Status {
	case suspect.StatusFound:
		answer.Status = snapshot.SuspectFound
	case suspect.StatusNone:
		answer.Status = snapshot.SuspectNone
	case suspect.StatusStartUnknown:
		answer.Status = snapshot.SuspectStartUnknown
	case suspect.StatusResourceUnknown:
		answer.Status = snapshot.SuspectResourceUnknown
	default:
		answer.Status = snapshot.SuspectIncomplete
	}
	searched := &snapshot.FeedSearched{Accounts: append([]string{}, result.Searched.Accounts...), Regions: append([]string{}, result.Searched.Regions...)}
	if !result.Searched.From.IsZero() {
		from, to := result.Searched.From, result.Searched.To
		searched.From, searched.To = &from, &to
	}
	answer.Searched = searched
	for _, candidate := range result.Candidates {
		answer.Candidates = append(answer.Candidates, snapshot.FeedSuspectCandidate{ChangeID: candidate.Change.ID, Confidence: candidate.Confidence, Tied: candidate.Tied})
	}
	if len(answer.Candidates) > 0 {
		answer.ChangeID = answer.Candidates[0].ChangeID
	}
	return answer
}
