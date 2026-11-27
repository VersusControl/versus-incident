package storage

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

var _ AWSChangeFeedStore = (*memoryTables)(nil)

func (m *memoryTables) RecordAWSChangePoll(_ context.Context, scope tenancy.OrgScope, poll AWSChangePoll, guard *uint64) (AWSChangePollResult, error) {
	cleaned, dropped, err := cleanChangePoll(poll)
	if err != nil {
		return AWSChangePollResult{}, err
	}
	scope = inventoryScope(scope)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.checkGuard(scope.Write, cleaned.Account, guard); err != nil {
		return AWSChangePollResult{}, err
	}
	// Work on copies so a failed poll leaves nothing behind, as a rolled
	// back transaction would.
	ops := &memoryChangePoll{m: m, org: scope.Write, key: memoryPairKey{scope.Write, cleaned.Account, cleaned.Region}}
	ops.begin()
	result, err := recordChangePoll(ops, cleaned, dropped)
	if err != nil {
		return AWSChangePollResult{}, err
	}
	ops.commit()
	return result, nil
}

type memoryChangePoll struct {
	m        *memoryTables
	org      string
	key      memoryPairKey
	changes  map[string]AWSChange
	counts   map[string]AWSChangeCount
	coverage []AWSInterval
	labels   []AWSChangeGapReason
	pair     *AWSChangePairState
}

func (op *memoryChangePoll) begin() {
	op.changes = map[string]AWSChange{}
	for id, change := range op.m.changes[op.org] {
		op.changes[id] = change
	}
	op.counts = map[string]AWSChangeCount{}
	for id, count := range op.m.counts[op.org] {
		op.counts[id] = count
	}
	op.coverage = append([]AWSInterval(nil), op.m.coverage[op.key]...)
	op.labels = append([]AWSChangeGapReason(nil), op.m.gapLabels[op.key]...)
	if state, ok := op.m.pairState[op.key]; ok {
		op.pair = &state
	}
}

func (op *memoryChangePoll) commit() {
	op.m.changes[op.org] = op.changes
	op.m.counts[op.org] = op.counts
	if len(op.coverage) == 0 {
		delete(op.m.coverage, op.key)
	} else {
		op.m.coverage[op.key] = op.coverage
	}
	op.m.gapLabels[op.key] = op.labels
	if op.pair != nil {
		op.m.pairState[op.key] = *op.pair
	}
}

func (op *memoryChangePoll) existingIDs(ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, id := range ids {
		if _, ok := op.changes[id]; ok {
			out[id] = true
		}
	}
	return out, nil
}

func (op *memoryChangePoll) coverageTouching(from, to time.Time) ([]AWSInterval, error) {
	from, to = tableTime(from), tableTime(to)
	var out []AWSInterval
	for _, interval := range op.coverage {
		if !interval.To.Before(from) && !interval.From.After(to) {
			out = append(out, interval)
		}
	}
	return out, nil
}

func (op *memoryChangePoll) failureRows(row AWSChange, from, to time.Time) ([]changeIDTime, error) {
	var out []changeIDTime
	for _, change := range op.changes {
		if change.Account == op.key.account && change.Region == op.key.region && change.EventName == row.EventName && change.ErrorCode == row.ErrorCode &&
			change.Actor == row.Actor && change.SessionClass == row.SessionClass && !change.EventTime.Before(from) && change.EventTime.Before(to) {
			out = append(out, changeIDTime{id: change.EventID, at: change.EventTime})
		}
	}
	return out, nil
}

func (op *memoryChangePoll) deleteChanges(ids []string) error {
	for _, id := range ids {
		delete(op.changes, id)
	}
	return nil
}

func (op *memoryChangePoll) upsertChanges(rows []AWSChange, at time.Time) error {
	for _, row := range rows {
		row.RecordedAt = at
		if existing, ok := op.changes[row.EventID]; ok {
			row.RecordedAt = existing.RecordedAt
		}
		op.changes[row.EventID] = row
	}
	return nil
}

func (op *memoryChangePoll) upsertCounts(counts []AWSChangeCount) error {
	for _, count := range counts {
		key := count.key()
		if existing, ok := op.counts[key]; ok && existing.Events > count.Events {
			count.Events = existing.Events
		}
		op.counts[key] = count
	}
	return nil
}

func (op *memoryChangePoll) replaceCoverage(remove, add []AWSInterval) error {
	kept := make([]AWSInterval, 0, len(op.coverage)+len(add))
	for _, interval := range op.coverage {
		drop := false
		for _, removed := range remove {
			if interval.From.Equal(tableTime(removed.From)) {
				drop = true
				break
			}
		}
		if !drop {
			kept = append(kept, interval)
		}
	}
	for _, interval := range add {
		kept = append(kept, AWSInterval{From: tableTime(interval.From), To: tableTime(interval.To)})
	}
	sort.Slice(kept, func(left, right int) bool { return kept[left].From.Before(kept[right].From) })
	op.coverage = kept
	return nil
}

func (op *memoryChangePoll) coverageCount() (int, error) { return len(op.coverage), nil }

func (op *memoryChangePoll) oldestCoverage(limit int) ([]AWSInterval, error) {
	if limit > len(op.coverage) {
		limit = len(op.coverage)
	}
	return append([]AWSInterval(nil), op.coverage[:limit]...), nil
}

func (op *memoryChangePoll) gapReasons() ([]AWSChangeGapReason, error) {
	return append([]AWSChangeGapReason(nil), op.labels...), nil
}

func (op *memoryChangePoll) replaceGapReasons(list []AWSChangeGapReason) error {
	op.labels = append([]AWSChangeGapReason(nil), list...)
	return nil
}

func (op *memoryChangePoll) upsertPair(poll AWSChangePoll, late, dropped int64) error {
	history := poll.HistoryFrom
	if history.IsZero() {
		history = poll.At
	}
	if op.pair == nil {
		op.pair = &AWSChangePairState{Org: op.org, Account: op.key.account, Region: op.key.region, ConnectedAt: poll.At, HistoryFrom: history}
	} else if history.Before(op.pair.HistoryFrom) {
		op.pair.HistoryFrom = history
	}
	op.pair.LastPollAt = poll.At
	op.pair.LateArrivals += late
	op.pair.Dropped += dropped
	op.pair.NotChanges += poll.NotChanges
	op.pair.ClockSkew = poll.ClockSkew.Truncate(time.Millisecond)
	return nil
}

// firstChangeCopiesLocked lists one copy of each change event across the
// read organizations: the copy of the first organization, in read order,
// that stored it. recordedBefore, when set, counts only copies stored by
// then. It is the memory form of the SQL firstChangeCopy.
func (m *memoryTables) firstChangeCopiesLocked(scope tenancy.OrgScope, recordedBefore time.Time, limit *AWSInheritedLimit) []AWSChange {
	pin := tableTime(recordedBefore)
	seen := map[string]bool{}
	var out []AWSChange
	for _, org := range scope.Read {
		for id, change := range m.changes[org] {
			if seen[id] || !pin.IsZero() && !change.RecordedAt.IsZero() && change.RecordedAt.After(pin) || !limit.keeps(scope.Write, org, change.Account) {
				continue
			}
			seen[id] = true
			out = append(out, change)
		}
	}
	return out
}

// largestCountsLocked keeps, for each minute and count key, the count of
// the organization that counted the most events, the first in read order
// on a tie. It is the memory form of the SQL largestCount.
func (m *memoryTables) largestCountsLocked(scope tenancy.OrgScope, limit *AWSInheritedLimit) []AWSChangeCount {
	best := map[string]AWSChangeCount{}
	for _, org := range scope.Read {
		for key, count := range m.counts[org] {
			if !limit.keeps(scope.Write, org, count.Account) {
				continue
			}
			if existing, ok := best[key]; ok && existing.Events >= count.Events {
				continue
			}
			best[key] = count
		}
	}
	out := make([]AWSChangeCount, 0, len(best))
	for _, count := range best {
		out = append(out, count)
	}
	return out
}

func feedMatches(query AWSChangeFeedQuery, account, region, service, actorClass string) bool {
	return (query.Account == "" || account == query.Account) && (query.Region == "" || region == query.Region) &&
		serviceMatches(query, service) && (query.ActorClass == "" || actorClass == query.ActorClass)
}

func (m *memoryTables) rowMatches(query AWSChangeFeedQuery, change AWSChange) bool {
	if !feedMatches(query, change.Account, change.Region, change.Service, change.ActorClass) {
		return false
	}
	if !query.Since.IsZero() && change.EventTime.Before(tableTime(query.Since)) || !query.Until.IsZero() && !change.EventTime.Before(tableTime(query.Until)) {
		return false
	}
	if query.ResourceARN != "" && !detailListsResource(change.Detail, query.ResourceARN, query.ResourceKey) {
		return false
	}
	if query.Text != "" {
		text := asciiLower(query.Text)
		if !strings.Contains(asciiLower(change.Summary), text) && !strings.Contains(asciiLower(change.EventName), text) &&
			!strings.Contains(asciiLower(apiName(detailSource(change.Detail), change.EventName)), text) {
			return false
		}
	}
	if !query.RecordedBefore.IsZero() && !change.RecordedAt.IsZero() && change.RecordedAt.After(tableTime(query.RecordedBefore)) {
		return false
	}
	return true
}

func (m *memoryTables) AWSChangeFeed(_ context.Context, scope tenancy.OrgScope, query AWSChangeFeedQuery) (AWSChangeFeed, error) {
	scope = inventoryScope(scope)
	m.mu.RLock()
	defer m.mu.RUnlock()
	feed := AWSChangeFeed{Coverage: m.changeCoverageLocked(scope, query.Account, query.Region, query.Inherited), Gaps: []AWSChangeGapReason{}, Pairs: []AWSChangePairState{}, Minutes: []AWSChangeMinute{}, Counts: []AWSChangeCount{}, Rows: []AWSChange{}}
	since, until := tableTime(query.Since), tableTime(query.Until)
	var ranks []int
	for key, labels := range m.gapLabels {
		if !scope.Contains(key.org) || !query.Inherited.keeps(scope.Write, key.org, key.account) ||
			!feedMatches(AWSChangeFeedQuery{Account: query.Account, Region: query.Region}, key.account, key.region, "", "") {
			continue
		}
		for _, label := range labels {
			if !since.IsZero() && !label.To.After(since) || !until.IsZero() && !label.From.Before(until) {
				continue
			}
			label.Account, label.Region = key.account, key.region
			feed.Gaps = append(feed.Gaps, label)
			ranks = append(ranks, slices.Index(scope.Read, key.org))
		}
	}
	// Labels of both organizations starting together keep read order, as
	// the SQL read orders them.
	order := make([]int, len(feed.Gaps))
	for index := range order {
		order[index] = index
	}
	sort.SliceStable(order, func(left, right int) bool {
		a, b := feed.Gaps[order[left]], feed.Gaps[order[right]]
		if a.Account != b.Account {
			return a.Account < b.Account
		}
		if a.Region != b.Region {
			return a.Region < b.Region
		}
		if !a.From.Equal(b.From) {
			return a.From.Before(b.From)
		}
		return ranks[order[left]] < ranks[order[right]]
	})
	gaps := make([]AWSChangeGapReason, 0, len(order))
	for _, index := range order {
		gaps = append(gaps, feed.Gaps[index])
	}
	feed.Gaps = gaps
	feed.Pairs = m.pairStatesLocked(scope, query.Account, query.Region, query.Inherited)

	minutes := map[AWSChangeMinute]int64{}
	people := map[string]bool{}
	actors := map[AWSChangeActorCount]int64{}
	services := map[string]int64{}
	serviceQuery := serviceCountQuery(query)
	var rows []AWSChange
	before := query
	before.Since, before.Until = time.Time{}, query.Since
	for _, change := range m.firstChangeCopiesLocked(scope, query.RecordedBefore, query.Inherited) {
		if !query.Since.IsZero() && change.ErrorCode == "" && m.rowMatches(before, change) && change.EventTime.After(feed.LastBefore) {
			feed.LastBefore = change.EventTime
		}
		if query.ServiceCounts && change.ErrorCode == "" && m.rowMatches(serviceQuery, change) {
			services[change.Service]++
		}
		if !m.rowMatches(query, change) {
			continue
		}
		if change.ErrorCode == "" {
			minutes[AWSChangeMinute{Minute: change.EventTime.Truncate(time.Minute), ActorClass: change.ActorClass}]++
			actors[AWSChangeActorCount{Actor: change.Actor, ActorClass: change.ActorClass, SessionClass: change.SessionClass}]++
			if change.ActorClass == "person" {
				people[change.Actor] = true
			}
		}
		change.Fields = cloneTableMap(change.Fields)
		if query.IncludeDetail {
			change.Detail = cloneTableJSON(change.Detail)
		} else {
			change.Detail = nil
		}
		rows = append(rows, change)
	}
	if query.ResourceARN == "" {
		for _, count := range m.largestCountsLocked(scope, query.Inherited) {
			if !feedMatches(query, count.Account, count.Region, count.Service, count.ActorClass) ||
				!since.IsZero() && count.Minute.Before(since) || !until.IsZero() && !count.Minute.Before(until) ||
				query.Text != "" && !strings.Contains(asciiLower(count.EventName), asciiLower(query.Text)) &&
					!strings.Contains(asciiLower(apiName(count.EventSource, count.EventName)), asciiLower(query.Text)) {
				continue
			}
			feed.Counts = append(feed.Counts, count)
		}
	}
	for item, events := range minutes {
		item.Events = events
		feed.Minutes = append(feed.Minutes, item)
	}
	sort.Slice(feed.Minutes, func(left, right int) bool {
		if !feed.Minutes[left].Minute.Equal(feed.Minutes[right].Minute) {
			return feed.Minutes[left].Minute.Before(feed.Minutes[right].Minute)
		}
		return feed.Minutes[left].ActorClass < feed.Minutes[right].ActorClass
	})
	sort.Slice(feed.Counts, func(left, right int) bool {
		if !feed.Counts[left].Minute.Equal(feed.Counts[right].Minute) {
			return feed.Counts[left].Minute.Before(feed.Counts[right].Minute)
		}
		return feed.Counts[left].key() < feed.Counts[right].key()
	})
	feed.People = len(people)
	if query.ServiceCounts {
		feed.ServiceCounts = make([]AWSChangeServiceCount, 0, len(services))
		for service, events := range services {
			feed.ServiceCounts = append(feed.ServiceCounts, AWSChangeServiceCount{Service: service, Events: events})
		}
		sortServiceCounts(feed.ServiceCounts)
	}
	for item, events := range actors {
		item.Events = events
		feed.TopActors = append(feed.TopActors, item)
	}
	sort.Slice(feed.TopActors, func(left, right int) bool {
		a, b := feed.TopActors[left], feed.TopActors[right]
		if a.Events != b.Events {
			return a.Events > b.Events
		}
		if a.Actor != b.Actor {
			return a.Actor < b.Actor
		}
		if a.ActorClass != b.ActorClass {
			return a.ActorClass < b.ActorClass
		}
		return a.SessionClass < b.SessionClass
	})
	if len(feed.TopActors) > topActorLimit {
		feed.TopActors = feed.TopActors[:topActorLimit]
	}
	if feed.TopActors == nil {
		feed.TopActors = []AWSChangeActorCount{}
	}
	sortFeedRows(rows)
	limit := query.RowLimit
	if limit <= 0 {
		limit = DefaultAWSChangeReadCap
	}
	if len(rows) > limit {
		rows, feed.Truncated = rows[:limit], true
	}
	if rows != nil {
		feed.Rows = rows
	}
	return feed, nil
}

// apiName renders an event as service:Action, as the SQL read does.
func apiName(source, name string) string {
	return strings.ReplaceAll(source, ".amazonaws.com", "") + ":" + name
}

// detailSource reads the event source from a change detail document.
func detailSource(detail json.RawMessage) string {
	var document struct {
		EventSource string `json:"event_source"`
	}
	_ = json.Unmarshal(detail, &document)
	return document.EventSource
}

// detailListsResource reports whether the detail document's resources hold
// arn, or its folded resource keys hold key.
func detailListsResource(detail json.RawMessage, arn, key string) bool {
	if key == "" {
		key = arn
	}
	var document map[string]json.RawMessage
	if json.Unmarshal(detail, &document) != nil {
		return false
	}
	for name, want := range map[string]string{ChangeDetailResources: arn, ChangeDetailResourceKeys: key} {
		var list []string
		if json.Unmarshal(document[name], &list) != nil {
			continue
		}
		for _, value := range list {
			if value == want {
				return true
			}
		}
	}
	return false
}

func (m *memoryTables) AWSChangePairState(_ context.Context, scope tenancy.OrgScope, account, region string) (AWSChangePairState, bool, error) {
	scope = inventoryScope(scope)
	m.mu.RLock()
	defer m.mu.RUnlock()
	pairs := m.pairStatesLocked(scope, account, region, nil)
	if len(pairs) == 0 {
		return AWSChangePairState{}, false, nil
	}
	return pairs[0], true, nil
}

// pairStatesLocked is every read organization's pair state matching the
// account and Region (empty matches all), merged per pair and sorted.
func (m *memoryTables) pairStatesLocked(scope tenancy.OrgScope, account, region string, limit *AWSInheritedLimit) []AWSChangePairState {
	var states []AWSChangePairState
	for _, org := range scope.Read {
		for key, state := range m.pairState {
			if key.org == org && limit.keeps(scope.Write, org, key.account) && feedMatches(AWSChangeFeedQuery{Account: account, Region: region}, key.account, key.region, "", "") {
				states = append(states, state)
			}
		}
	}
	sort.SliceStable(states, func(left, right int) bool {
		if states[left].Account != states[right].Account {
			return states[left].Account < states[right].Account
		}
		return states[left].Region < states[right].Region
	})
	return mergeChangePairs(scope.Write, states)
}

// sortFeedRows orders rows newest first, then by event ID descending, as
// the SQL read does.
func sortFeedRows(rows []AWSChange) {
	sort.Slice(rows, func(left, right int) bool {
		if !rows[left].EventTime.Equal(rows[right].EventTime) {
			return rows[left].EventTime.After(rows[right].EventTime)
		}
		return rows[left].EventID > rows[right].EventID
	})
}

func (m *memoryTables) AWSChangesByID(_ context.Context, scope tenancy.OrgScope, ids []string) ([]AWSChange, error) {
	scope = inventoryScope(scope)
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]AWSChange, 0, len(ids))
	for _, id := range ids {
		for _, org := range scope.Read {
			change, ok := m.changes[org][id]
			if !ok {
				continue
			}
			change.Fields, change.Detail = cloneTableMap(change.Fields), cloneTableJSON(change.Detail)
			out = append(out, change)
			break
		}
	}
	sortFeedRows(out)
	return out, nil
}
