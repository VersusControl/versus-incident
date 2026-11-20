package storage

import (
	"context"
	"sort"
	"time"

	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// Kinds of per-minute change counts. Routine service events and failed
// attempts beyond the kept rows are counted, not stored one row each, so the
// change tables grow with time, not with fleet size or a failing job.
const (
	AWSChangeCountRoutine = "routine"
	AWSChangeCountFailure = "failure"
)

// Bounds of the change feed tables. Each is a per-pair or per-read limit,
// so storage stays flat under steady overload.
const (
	// DefaultAWSChangeIntervalCap is the most coverage intervals one account
	// and region keep. Beyond it the oldest islands are dropped from
	// coverage and their stretch is labelled too_busy.
	DefaultAWSChangeIntervalCap = 96
	// DefaultAWSChangeGapCap is the most labelled gap intervals per pair.
	DefaultAWSChangeGapCap = 64
	// DefaultAWSChangeFailureKeep is how many failed attempts per principal,
	// error code, event and hour are kept as rows; the rest are counted.
	DefaultAWSChangeFailureKeep = 20
	// DefaultAWSChangeReadCap is the most change rows one read returns. A
	// read that hits it is marked truncated.
	DefaultAWSChangeReadCap = 20000
)

// Gap reasons the storage layer writes on its own.
const (
	AWSChangeGapTooBusy = "too_busy"
)

// AWSChangeCount is one minute's count of routine events or failed
// attempts with the same event, principal, session class and error code.
// Counts merge by maximum: a later read of the same minute sees at least
// the events an earlier read saw, so re-reads never double-count.
type AWSChangeCount struct {
	Kind         string    `json:"kind"`
	EventName    string    `json:"event_name"`
	Actor        string    `json:"actor,omitempty"`
	SessionClass string    `json:"session_class,omitempty"`
	ErrorCode    string    `json:"error_code,omitempty"`
	Minute       time.Time `json:"minute"`
	Account      string    `json:"account,omitempty"`
	Region       string    `json:"region,omitempty"`
	Service      string    `json:"service,omitempty"`
	EventSource  string    `json:"event_source,omitempty"`
	ActorClass   string    `json:"actor_class,omitempty"`
	Events       int64     `json:"events"`
}

func (count AWSChangeCount) key() string {
	return count.Account + "\x00" + count.Region + "\x00" + count.Kind + "\x00" + count.EventName + "\x00" + count.Actor + "\x00" +
		count.SessionClass + "\x00" + count.ErrorCode + "\x00" + tableTime(count.Minute).Format(time.RFC3339)
}

// AWSChangeGapReason labels one stretch that was not read with why.
type AWSChangeGapReason struct {
	Account string    `json:"account,omitempty"`
	Region  string    `json:"region,omitempty"`
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
	Reason  string    `json:"reason"`
	Detail  string    `json:"detail,omitempty"`
}

// AWSChangePairState is one account and region's poll bookkeeping.
// HistoryFrom is where the first poll started reading; ConnectedAt is when
// it ran.
type AWSChangePairState struct {
	Org          string        `json:"org"`
	Account      string        `json:"account"`
	Region       string        `json:"region"`
	ConnectedAt  time.Time     `json:"connected_at"`
	HistoryFrom  time.Time     `json:"history_from"`
	LastPollAt   time.Time     `json:"last_poll_at,omitzero"`
	LateArrivals int64         `json:"late_arrivals"`
	Dropped      int64         `json:"dropped"`
	NotChanges   int64         `json:"not_changes"`
	ClockSkew    time.Duration `json:"clock_skew"`
}

// mergeChangePairs folds the states every read organization keeps for one
// account and Region into one, keeping the input's pair order. The input
// lists each pair's states in read order, so a tie goes to the first
// organization. The earliest connect time and history start win, so
// history read before a key was added is never labelled as before the
// connection; the latest poll gives the poll time and clock skew. Each
// counter is the largest any organization kept: both may have read the same
// stretch, so adding them would count its events twice. The state is the
// write organization's when it holds one.
func mergeChangePairs(write string, states []AWSChangePairState) []AWSChangePairState {
	out := make([]AWSChangePairState, 0, len(states))
	index := map[[2]string]int{}
	for _, state := range states {
		key := [2]string{state.Account, state.Region}
		at, seen := index[key]
		if !seen {
			index[key] = len(out)
			out = append(out, state)
			continue
		}
		merged := &out[at]
		if state.Org == write {
			merged.Org = write
		}
		if state.ConnectedAt.Before(merged.ConnectedAt) {
			merged.ConnectedAt = state.ConnectedAt
		}
		if state.HistoryFrom.Before(merged.HistoryFrom) {
			merged.HistoryFrom = state.HistoryFrom
		}
		if state.LastPollAt.After(merged.LastPollAt) {
			merged.LastPollAt, merged.ClockSkew = state.LastPollAt, state.ClockSkew
		}
		merged.LateArrivals = max(merged.LateArrivals, state.LateArrivals)
		merged.Dropped = max(merged.Dropped, state.Dropped)
		merged.NotChanges = max(merged.NotChanges, state.NotChanges)
	}
	return out
}

// addCoverageInterval appends next to intervals sorted by start, folding it
// into the last one when they overlap. Two organizations may have read the
// same stretch; the answer lists it once, so the newest interval is the
// one that ends last.
func addCoverageInterval(intervals []AWSInterval, next AWSInterval) []AWSInterval {
	if last := len(intervals) - 1; last >= 0 && next.From.Before(intervals[last].To) {
		if next.To.After(intervals[last].To) {
			intervals[last].To = next.To
		}
		return intervals
	}
	return append(intervals, next)
}

// AWSChangePoll is one poll of one account and region.
type AWSChangePoll struct {
	Account string
	Region  string
	// At is the poll time; new rows record it as their arrival.
	At time.Time
	// HistoryFrom is where this poll's oldest window started; the pair
	// keeps the first poll's value.
	HistoryFrom time.Time
	Changes     []AWSChange
	Counts      []AWSChangeCount
	Covered     []AWSInterval
	// Read are the stretches read without error, including the unsettled
	// tail that is not yet claimed. They clear older gap labels.
	Read []AWSInterval
	Gaps []AWSChangeGapReason
	// Dropped counts events the caller already dropped as malformed.
	Dropped int64
	// NotChanges counts events the caller left out because they change
	// nothing (sign-ins, tokens, reads).
	NotChanges int64
	ClockSkew  time.Duration
	// Zero bounds take the defaults above.
	FailureKeep int
	IntervalCap int
	GapCap      int

	// strict rejects the whole poll on one invalid row, as the older
	// AppendAWSChanges contract does.
	strict bool
}

// AWSChangePollResult reports what one poll stored.
type AWSChangePollResult struct {
	Stored       int64
	Dropped      int64
	Evicted      int64
	LateArrivals int64
}

// AWSChangeFeedQuery selects one change window. Empty filters match all.
type AWSChangeFeedQuery struct {
	Account string
	Region  string
	Service string
	// Services matches a change in any of these services; empty matches
	// all. It applies together with Service.
	Services   []string
	ActorClass string
	// ResourceARN matches a change whose resources list holds exactly this
	// ARN; ResourceKey, when set, also matches a change whose folded
	// resource keys hold it (two valid ARNs of one resource). Only the
	// resources are searched, never field values or unresolved names.
	ResourceARN string
	ResourceKey string
	// Text matches the summary or event name, ASCII case-insensitive.
	Text  string
	Since time.Time
	Until time.Time
	// RecordedBefore, when set, hides rows recorded after it, so the pages
	// of one listing see one fixed set of rows.
	RecordedBefore time.Time
	// RowLimit caps the rows returned (DefaultAWSChangeReadCap when zero).
	RowLimit int
	// IncludeDetail returns each row's detail document.
	IncludeDetail bool
	// ServiceCounts asks for AWSChangeFeed.ServiceCounts.
	ServiceCounts bool
}

// AWSChangeServiceCount counts one service's successful changes.
type AWSChangeServiceCount struct {
	Service string `json:"service"`
	Events  int64  `json:"events"`
}

// serviceCountQuery is the query a service count uses: every filter but
// the service ones, so the counts show what a service filter hides.
func serviceCountQuery(query AWSChangeFeedQuery) AWSChangeFeedQuery {
	query.Service, query.Services = "", nil
	return query
}

// serviceMatches reports whether a change's service passes the query's
// service filters.
func serviceMatches(query AWSChangeFeedQuery, service string) bool {
	if query.Service != "" && service != query.Service {
		return false
	}
	if len(query.Services) == 0 {
		return true
	}
	for _, want := range query.Services {
		if service == want {
			return true
		}
	}
	return false
}

// sortServiceCounts orders service counts most first, then by service.
func sortServiceCounts(counts []AWSChangeServiceCount) {
	sort.Slice(counts, func(left, right int) bool {
		if counts[left].Events != counts[right].Events {
			return counts[left].Events > counts[right].Events
		}
		return counts[left].Service < counts[right].Service
	})
}

// AWSChangeMinute counts successful changes of one actor class in one
// minute.
type AWSChangeMinute struct {
	Minute     time.Time `json:"minute"`
	ActorClass string    `json:"actor_class"`
	Events     int64     `json:"events"`
}

// AWSChangeActorCount counts one principal's successful changes.
type AWSChangeActorCount struct {
	Actor        string `json:"actor"`
	ActorClass   string `json:"actor_class"`
	SessionClass string `json:"session_class,omitempty"`
	Events       int64  `json:"events"`
}

// topActorLimit is how many principals TopActors returns. It is more than
// the answer shows because the answer merges principals that read alike
// (one role name in several accounts) before it keeps its top few.
const topActorLimit = 50

// AWSChangeFeed is one change window read from a single snapshot: the
// coverage, the gap labels, the pair state, the aggregates and the rows can
// never disagree because a sweep or a poll landed between them.
type AWSChangeFeed struct {
	Coverage []AWSPairCoverage
	Gaps     []AWSChangeGapReason
	Pairs    []AWSChangePairState
	// Minutes are successful changes by minute and actor class.
	Minutes []AWSChangeMinute
	// Counts are routine and failure minute counts. They are empty when a
	// resource filter is set, because counts carry no resource.
	Counts []AWSChangeCount
	// People counts distinct person principals with a successful change.
	People int
	// TopActors are the principals with the most successful changes,
	// most first, at most topActorLimit.
	TopActors []AWSChangeActorCount
	// LastBefore is the newest successful change before Since in scope.
	LastBefore time.Time
	// ServiceCounts are successful changes per service, most first,
	// counted without the service filters. Set only when the query asks.
	ServiceCounts []AWSChangeServiceCount
	// Rows are newest first.
	Rows      []AWSChange
	Truncated bool
}

// AWSChangeFeedStore records and reads the change feed. It is optional
// beside AWSInventoryStore; every backend that has the inventory tables
// implements it with the same contract.
type AWSChangeFeedStore interface {
	// RecordAWSChangePoll stores one poll in one transaction: rows,
	// counts, coverage, gap labels and the pair state. Malformed rows are
	// dropped and counted; the rest commit and coverage advances.
	RecordAWSChangePoll(ctx context.Context, scope tenancy.OrgScope, poll AWSChangePoll, guard *uint64) (AWSChangePollResult, error)
	// AWSChangeFeed reads one window from a single snapshot.
	AWSChangeFeed(ctx context.Context, scope tenancy.OrgScope, query AWSChangeFeedQuery) (AWSChangeFeed, error)
	// AWSChangesByID returns the stored rows with these event IDs, with
	// their detail.
	AWSChangesByID(ctx context.Context, scope tenancy.OrgScope, ids []string) ([]AWSChange, error)
	// AWSChangePairState reads one pair's poll state in the write
	// organization, from the primary, so every replica sees the latest
	// poll.
	AWSChangePairState(ctx context.Context, scope tenancy.OrgScope, account, region string) (AWSChangePairState, bool, error)
}

// ChangeDetailResources and ChangeDetailResourceKeys are the keys of a
// change detail document the resource filter searches.
const (
	ChangeDetailResources    = "resources"
	ChangeDetailResourceKeys = "resource_keys"
)

// AWSChangeFeeds returns the provider's change feed store, if it has one.
func AWSChangeFeeds(provider Provider) (AWSChangeFeedStore, bool) {
	store, ok := provider.(AWSChangeFeedStore)
	return store, ok
}

// changeIDTime is a stored row's identity and time.
type changeIDTime struct {
	id string
	at time.Time
}

// changePollOps are the statements one poll needs inside its transaction.
// The SQL tables and the memory tables implement them, so both run the one
// algorithm in recordChangePoll.
type changePollOps interface {
	existingIDs(ids []string) (map[string]bool, error)
	coverageTouching(from, to time.Time) ([]AWSInterval, error)
	failureRows(row AWSChange, from, to time.Time) ([]changeIDTime, error)
	deleteChanges(ids []string) error
	upsertChanges(rows []AWSChange, at time.Time) error
	upsertCounts(counts []AWSChangeCount) error
	replaceCoverage(remove, add []AWSInterval) error
	coverageCount() (int, error)
	oldestCoverage(limit int) ([]AWSInterval, error)
	gapReasons() ([]AWSChangeGapReason, error)
	replaceGapReasons(list []AWSChangeGapReason) error
	upsertPair(poll AWSChangePoll, late, dropped int64) error
}

// cleanChangePoll validates the poll's keys and rows. Malformed rows are
// dropped and counted unless the poll is strict.
func cleanChangePoll(poll AWSChangePoll) (AWSChangePoll, int64, error) {
	if !validTableKey(poll.Account) || !validTableKey(poll.Region) {
		return poll, 0, ErrInvalidAWSInventory
	}
	poll.At = tableTime(poll.At)
	poll.HistoryFrom = tableTime(poll.HistoryFrom)
	if poll.FailureKeep <= 0 {
		poll.FailureKeep = DefaultAWSChangeFailureKeep
	}
	if poll.IntervalCap <= 0 {
		poll.IntervalCap = DefaultAWSChangeIntervalCap
	}
	if poll.GapCap <= 0 {
		poll.GapCap = DefaultAWSChangeGapCap
	}
	var dropped int64
	byID := make(map[string]AWSChange, len(poll.Changes))
	for _, change := range poll.Changes {
		cleaned, err := cleanAWSChange(poll.Account, poll.Region, change)
		if err != nil {
			if poll.strict {
				return poll, 0, err
			}
			dropped++
			continue
		}
		byID[cleaned.EventID] = cleaned
	}
	poll.Changes = make([]AWSChange, 0, len(byID))
	for _, id := range sortedKeysOf(byID) {
		poll.Changes = append(poll.Changes, byID[id])
	}
	counts := make(map[string]AWSChangeCount, len(poll.Counts))
	for _, count := range poll.Counts {
		count.Account, count.Region, count.Minute = poll.Account, poll.Region, tableTime(count.Minute).Truncate(time.Minute)
		if count.Minute.IsZero() || count.Events <= 0 || (count.Kind != AWSChangeCountRoutine && count.Kind != AWSChangeCountFailure) ||
			!validTableKey(count.EventName) || !validTableStrings(count.Actor, count.SessionClass, count.ErrorCode, count.Service, count.EventSource, count.ActorClass) {
			dropped++
			continue
		}
		key := count.key()
		if existing, ok := counts[key]; ok && existing.Events >= count.Events {
			continue
		}
		counts[key] = count
	}
	poll.Counts = make([]AWSChangeCount, 0, len(counts))
	for _, key := range sortedKeysOf(counts) {
		poll.Counts = append(poll.Counts, counts[key])
	}
	poll.Covered = mergeAWSIntervals(poll.Covered)
	poll.Read = mergeAWSIntervals(append(append([]AWSInterval(nil), poll.Read...), poll.Covered...))
	gaps := make([]AWSChangeGapReason, 0, len(poll.Gaps))
	for _, gap := range poll.Gaps {
		gap.From, gap.To = tableTime(gap.From), tableTime(gap.To)
		if gap.From.IsZero() || !gap.To.After(gap.From) || !validTableKey(gap.Reason) || !validTableText(gap.Detail) {
			continue
		}
		gap.Account, gap.Region = poll.Account, poll.Region
		gaps = append(gaps, gap)
	}
	poll.Gaps = gaps
	return poll, dropped, nil
}

func failureKey(row AWSChange) string {
	return row.EventName + "\x00" + row.Actor + "\x00" + row.SessionClass + "\x00" + row.ErrorCode + "\x00" + row.EventTime.Truncate(time.Hour).Format(time.RFC3339)
}

// recordChangePoll runs one cleaned poll through ops.
func recordChangePoll(ops changePollOps, poll AWSChangePoll, dropped int64) (AWSChangePollResult, error) {
	result := AWSChangePollResult{Dropped: dropped + poll.Dropped}
	ids := make([]string, 0, len(poll.Changes))
	var earliest, latest time.Time
	for _, change := range poll.Changes {
		ids = append(ids, change.EventID)
		if earliest.IsZero() || change.EventTime.Before(earliest) {
			earliest = change.EventTime
		}
		if change.EventTime.After(latest) {
			latest = change.EventTime
		}
	}
	existing, err := ops.existingIDs(ids)
	if err != nil {
		return result, err
	}

	// A new row inside coverage an earlier poll claimed arrived after that
	// poll's overlap: CloudTrail delivered it late.
	if len(poll.Changes) > 0 {
		prior, err := ops.coverageTouching(earliest, latest.Add(time.Microsecond))
		if err != nil {
			return result, err
		}
		for _, change := range poll.Changes {
			if existing[change.EventID] || change.ErrorCode != "" {
				continue
			}
			for _, interval := range prior {
				if !change.EventTime.Before(interval.From) && change.EventTime.Before(interval.To) {
					result.LateArrivals++
					break
				}
			}
		}
	}

	// Failed attempts: per principal, error code, event and hour only the
	// earliest FailureKeep by event time stay rows; the counts hold the rest.
	keep := make([]AWSChange, 0, len(poll.Changes))
	failures := map[string][]AWSChange{}
	for _, change := range poll.Changes {
		if change.ErrorCode == "" {
			keep = append(keep, change)
			continue
		}
		key := failureKey(change)
		failures[key] = append(failures[key], change)
	}
	var evict []string
	for _, key := range sortedKeysOf(failures) {
		group := failures[key]
		hour := group[0].EventTime.Truncate(time.Hour)
		stored, err := ops.failureRows(group[0], hour, hour.Add(time.Hour))
		if err != nil {
			return result, err
		}
		candidates := make([]changeIDTime, 0, len(stored)+len(group))
		seen := map[string]bool{}
		storedIDs := map[string]bool{}
		for _, row := range stored {
			seen[row.id], storedIDs[row.id] = true, true
			candidates = append(candidates, row)
		}
		incoming := map[string]AWSChange{}
		for _, change := range group {
			incoming[change.EventID] = change
			if !seen[change.EventID] {
				seen[change.EventID] = true
				candidates = append(candidates, changeIDTime{id: change.EventID, at: change.EventTime})
			}
		}
		sort.Slice(candidates, func(left, right int) bool {
			if !candidates[left].at.Equal(candidates[right].at) {
				return candidates[left].at.Before(candidates[right].at)
			}
			return candidates[left].id < candidates[right].id
		})
		for index, candidate := range candidates {
			if index < poll.FailureKeep {
				if change, ok := incoming[candidate.id]; ok {
					keep = append(keep, change)
				}
				continue
			}
			if storedIDs[candidate.id] {
				evict = append(evict, candidate.id)
			}
		}
	}
	if len(evict) > 0 {
		if err := ops.deleteChanges(evict); err != nil {
			return result, err
		}
		result.Evicted = int64(len(evict))
	}
	if err := ops.upsertChanges(keep, poll.At); err != nil {
		return result, err
	}
	result.Stored = int64(len(keep))
	if err := ops.upsertCounts(poll.Counts); err != nil {
		return result, err
	}

	// Coverage: only the intervals the new ones touch are rewritten, so a
	// poll's write cost does not grow with the pair's history.
	covered := poll.Covered
	if len(covered) > 0 {
		touching, err := ops.coverageTouching(covered[0].From, covered[len(covered)-1].To)
		if err != nil {
			return result, err
		}
		merged := mergeAWSIntervals(append(append([]AWSInterval(nil), touching...), covered...))
		if err := ops.replaceCoverage(touching, merged); err != nil {
			return result, err
		}
	}
	gaps := poll.Gaps
	count, err := ops.coverageCount()
	if err != nil {
		return result, err
	}
	if excess := count - poll.IntervalCap; excess > 0 {
		// Steady overload leaves one island per poll. Dropping the oldest
		// islands only un-claims coverage, never invents it, and the band
		// they leave is named too_busy.
		oldest, err := ops.oldestCoverage(excess + 1)
		if err != nil {
			return result, err
		}
		if len(oldest) > excess {
			if err := ops.replaceCoverage(oldest[:excess], nil); err != nil {
				return result, err
			}
			gaps = append(gaps, AWSChangeGapReason{Account: poll.Account, Region: poll.Region, From: oldest[0].From, To: oldest[excess].From, Reason: AWSChangeGapTooBusy})
		}
	}
	if len(gaps) > 0 || len(poll.Read) > 0 {
		current, err := ops.gapReasons()
		if err != nil {
			return result, err
		}
		next := applyGapReasons(current, gaps, poll.Read, poll.GapCap)
		if !sameGapReasons(current, next) {
			if err := ops.replaceGapReasons(next); err != nil {
				return result, err
			}
		}
	}
	if err := ops.upsertPair(poll, result.LateArrivals, result.Dropped); err != nil {
		return result, err
	}
	return result, nil
}

// applyGapReasons lays the newest labels over the old ones, removes the
// stretches read since, joins touching labels with the same reason and
// bounds the list: beyond limit the oldest labels fold into one band that
// keeps the newest of their reasons.
func applyGapReasons(current, added []AWSChangeGapReason, covered []AWSInterval, limit int) []AWSChangeGapReason {
	out := append([]AWSChangeGapReason(nil), current...)
	cut := func(list []AWSChangeGapReason, from, to time.Time) []AWSChangeGapReason {
		kept := make([]AWSChangeGapReason, 0, len(list)+1)
		for _, gap := range list {
			if !gap.To.After(from) || !gap.From.Before(to) {
				kept = append(kept, gap)
				continue
			}
			if gap.From.Before(from) {
				left := gap
				left.To = from
				kept = append(kept, left)
			}
			if gap.To.After(to) {
				right := gap
				right.From = to
				kept = append(kept, right)
			}
		}
		return kept
	}
	for _, gap := range added {
		out = append(cut(out, gap.From, gap.To), gap)
	}
	for _, interval := range covered {
		out = cut(out, tableTime(interval.From), tableTime(interval.To))
	}
	sort.Slice(out, func(left, right int) bool { return out[left].From.Before(out[right].From) })
	joined := make([]AWSChangeGapReason, 0, len(out))
	for _, gap := range out {
		if last := len(joined) - 1; last >= 0 && !gap.From.After(joined[last].To) && gap.Reason == joined[last].Reason && gap.Detail == joined[last].Detail {
			if gap.To.After(joined[last].To) {
				joined[last].To = gap.To
			}
			continue
		}
		joined = append(joined, gap)
	}
	if limit > 0 && len(joined) > limit {
		fold := len(joined) - limit + 1
		band := joined[fold-1]
		band.From = joined[0].From
		joined = append([]AWSChangeGapReason{band}, joined[fold:]...)
	}
	return joined
}

func sameGapReasons(left, right []AWSChangeGapReason) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !left[index].From.Equal(right[index].From) || !left[index].To.Equal(right[index].To) ||
			left[index].Reason != right[index].Reason || left[index].Detail != right[index].Detail {
			return false
		}
	}
	return true
}

// minuteOf parses a minute rendered as 2006-01-02T15:04 in UTC.
func minuteOf(text string) (time.Time, bool) {
	if len(text) < 16 {
		return time.Time{}, false
	}
	at, err := time.ParseInLocation("2006-01-02T15:04", text[:16], time.UTC)
	return at, err == nil
}
