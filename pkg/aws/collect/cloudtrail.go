package collect

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/aws-sdk-go-v2/service/cloudtrail"
	cloudtrailtypes "github.com/aws/aws-sdk-go-v2/service/cloudtrail/types"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/VersusControl/versus-incident/pkg/aws/catalog"
	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/limit"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/awsauth"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// maxChangePages bounds one window's read when readWindow is called
// directly. A poll sizes its own budget from the CloudTrail rate and the
// poll interval (pageBudget).
var maxChangePages = 20

// changePageSize is the largest page LookupEvents serves.
var changePageSize int32 = 50

// Page budget bounds. The automatic budget spends at most half of the
// CloudTrail rate over one poll interval, and never more than fits in one
// unit's run time.
const (
	minChangePageBudget = 20
	maxChangePageBudget = 480
)

// clockSkewTolerance is how far the local clock may run ahead of AWS
// before coverage is capped at AWS time. The Date header has one-second
// resolution.
const clockSkewTolerance = 2 * time.Second

// DefaultRoutineEvents are counted per minute, not stored one row each:
// agents and schedulers repeat them per instance or task, so they grow
// with fleet size rather than with real change. AWS Config writes one
// PutEvaluations per rule evaluation.
func DefaultRoutineEvents() []string {
	return []string{"ssm:UpdateInstanceInformation", "ecs:SubmitTaskStateChange", "ecs:SubmitContainerStateChange", "logs:CreateLogStream", "config:PutEvaluations"}
}

// globalSources are the services whose events CloudTrail records only in
// us-east-1. They are read once per account as the global stream and stored
// under the global region, whether or not us-east-1 is selected.
var globalSources = map[string]bool{
	"iam.amazonaws.com":            true,
	"sts.amazonaws.com":            true,
	"organizations.amazonaws.com":  true,
	"cloudfront.amazonaws.com":     true,
	"route53.amazonaws.com":        true,
	"route53domains.amazonaws.com": true,
	"waf.amazonaws.com":            true,
	"account.amazonaws.com":        true,
}

// IsGlobalSource reports whether an event source belongs to the global
// stream.
func IsGlobalSource(source string) bool { return globalSources[source] }

// keepEvent reports whether a pair's poll keeps an event: the global pair
// keeps only global sources, and the us-east-1 pair leaves them to it.
func keepEvent(region, source string) bool {
	if region == model.GlobalRegion {
		return globalSources[source]
	}
	return !globalSources[source]
}

func cloudTrailClient(c *call) (*cloudtrail.Client, error) {
	endpoint, client, stack, err := c.env.endpoint("cloudtrail", c.endpointRegion)
	if err != nil {
		return nil, err
	}
	return cloudtrail.NewFromConfig(c.env.config(c.endpointRegion), func(options *cloudtrail.Options) {
		options.BaseEndpoint = &endpoint
		options.HTTPClient = client
		options.RetryMaxAttempts = 1
		options.APIOptions = append(options.APIOptions, stack...)
	}), nil
}

// pageBudget is how many LookupEvents pages one poll may spend.
func pageBudget(options Options) int {
	if options.ChangePageBudget > 0 {
		return options.ChangePageBudget
	}
	interval := options.ChangesInterval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	pages := min(max(int(float64(limit.CloudTrailMaxRPS)*interval.Seconds()/2), minChangePageBudget), maxChangePageBudget)
	// The per-cycle call budget of the change source bounds the automatic
	// size; an explicit page budget is the user's choice and wins.
	if cycle := callBudget(options, snapshot.ChangeService); cycle > 0 {
		pages = min(pages, cycle)
	}
	return pages
}

// routineKey renders an event as service:Action, the form the routine list
// and the change list use.
func routineKey(source, name string) string {
	return strings.TrimSuffix(source, ".amazonaws.com") + ":" + name
}

// pollChanges reads CloudTrail write events for one pair from the end of
// the checkpoint's coverage, plus the most recent gap, and records exactly
// the intervals it read to the end. A failed window is never marked covered
// and the stretch it could not read keeps its own reason. Only one replica
// polls a pair at a time; another replica skips it and writes nothing.
func (collector *Collector) pollChanges(ctx context.Context, c *call, options Options) UnitReport {
	report := UnitReport{Unit: c.unit, Outcome: OutcomeKept}
	unlock, acquired, err := collector.store.LockChangePair(ctx, c.unit.Account, c.unit.Region)
	if err != nil {
		report.ErrorCode = CodeStorage
		return report
	}
	if !acquired {
		report.Outcome = OutcomeSkipped
		return report
	}
	defer unlock()
	// The schedule is per replica; the pair's last poll is shared. Under
	// the lock, a pair any replica polled within half an interval is left
	// alone, so N replicas still poll each pair about once per interval.
	if options.ChangesInterval > 0 {
		polled, err := collector.store.ChangePairPolledAt(ctx, c.unit.Account, c.unit.Region)
		if err != nil {
			report.ErrorCode = CodeStorage
			return report
		}
		if !polled.IsZero() && c.now.Sub(polled) < options.ChangesInterval/2 {
			report.Outcome = OutcomeSkipped
			return report
		}
	}
	if c.env.credentials == nil {
		report.ErrorCode = awsauth.ErrNoServerIdentity.Error()
		collector.failChanges(ctx, c, report.ErrorCode, options)
		return report
	}
	checkpoint, err := collector.store.Checkpoint(c.unit.Account, c.unit.Region)
	if err != nil {
		report.ErrorCode = CodeStorage
		collector.fail(ctx, c, report.ErrorCode, nil)
		return report
	}
	trail := *c
	trail.env.lane = collector.trailLimiter()
	api, err := cloudTrailClient(&trail)
	if err != nil {
		report.ErrorCode = c.env.resolve(errorCode(err))
		collector.failChanges(ctx, c, report.ErrorCode, options)
		return report
	}

	unitCtx, cancel := context.WithTimeout(ctx, unitTimeout)
	defer cancel()
	// Never look further back than the store keeps changes: older reads
	// would be pruned at once and claimed coverage would be a lie.
	lookback := options.ChangeLookback
	if retention := collector.store.ChangeRetention(); retention > 0 && retention < lookback {
		lookback = retention
	}
	poll := snapshot.ChangePoll{FailureKeep: options.FailureKeep}
	events := newPollEvents(c.unit.Account, c.unit.Region, options.RoutineEvents)
	budget := pageBudget(options)
	var skew clockSkew
	code := ""
	type read struct {
		window changes.Interval
		from   time.Time
	}
	var reads []read
	windows := checkpoint.Windows(c.now, lookback, options.ChangeOverlap)
	for index, window := range windows {
		// Windows start on a whole minute, so every minute a read claims
		// is read in full and its counts are exact.
		window.From = window.From.Truncate(time.Minute)
		if index == 0 || poll.HistoryFrom.IsZero() || window.From.Before(poll.HistoryFrom) {
			poll.HistoryFrom = window.From
		}
		result := readWindowPages(unitCtx, api, window, budget, collector.now)
		budget -= result.pages
		skew.merge(result.skew)
		// A malformed event counts on the pair that keeps its source, so
		// the global stream's re-read of us-east-1 never counts it twice.
		for _, source := range result.malformed {
			if keepEvent(c.unit.Region, source) {
				poll.Dropped++
			}
		}
		for _, event := range result.events {
			events.add(event, result.from)
		}
		reads = append(reads, read{window: window, from: result.from})
		switch {
		case result.err != nil:
			code = c.env.resolve(errorCode(result.err))
			poll.Gaps = append(poll.Gaps, snapshot.ChangeGapLabel{From: window.From, To: result.from, Reason: snapshot.ChangeGapReasonForCode(code), Detail: deniedPermission(code)})
		case result.capped:
			poll.Gaps = append(poll.Gaps, snapshot.ChangeGapLabel{From: window.From, To: result.from, Reason: snapshot.GapTooBusy, Detail: eventRate(result)})
		}
		if result.err != nil {
			break
		}
		if budget <= 0 {
			break
		}
	}
	if ctx.Err() != nil {
		// Shutdown: nothing from a cut-short poll is recorded.
		return report
	}
	// CloudTrail delivers events minutes late, so the newest stretch of a
	// window is read but not claimed: it stays a gap until a later poll
	// re-reads it through the overlap. A local clock ahead of AWS never
	// claims coverage past AWS time.
	now := c.now
	if skew.known {
		poll.ClockSkew = skew.value.Round(time.Second)
	}
	if ahead := skew.ahead(); ahead > clockSkewTolerance {
		// The bound, not the estimate: coverage never passes AWS time.
		now = c.now.Add(-ahead)
	}
	settled := now.Add(-options.ChangeOverlap)
	for _, item := range reads {
		end := item.window.To
		if end.After(settled) {
			end = settled
		}
		if item.from.Before(end) {
			poll.Covered = append(poll.Covered, changes.Interval{From: item.from, To: end})
		}
		if item.from.Before(item.window.To) {
			poll.Read = append(poll.Read, changes.Interval{From: item.from, To: item.window.To})
		}
	}
	poll.Found, poll.Counts = events.found, events.countList()
	poll.NotChanges = events.notChangeCount(checkpoint, poll.Covered)
	poll.ErrorCode = code
	if !collector.store.ChangePairLockHeld(ctx, c.unit.Account, c.unit.Region) {
		report.Outcome = OutcomeSkipped
		return report
	}
	if err := c.writer.RecordChangePoll(c.unit.Region, poll); err != nil {
		if errors.Is(err, snapshot.ErrPurged) {
			return report
		}
		report.ErrorCode = CodeStorage
		collector.fail(ctx, c, report.ErrorCode, nil)
		return report
	}
	if code != "" {
		report.ErrorCode = code
		collector.fail(ctx, c, code, nil)
		return report
	}
	if err := c.writer.RecordSuccess(c.unit, nil); err != nil {
		report.ErrorCode = CodeStorage
		return report
	}
	report.Outcome = OutcomeCommitted
	report.Resources = len(poll.Found)
	return report
}

// failChanges records a poll that could not start: the sync state, and the
// reason over the stretch it would have read, so that stretch does not take
// a later error's reason.
func (collector *Collector) failChanges(ctx context.Context, c *call, code string, options Options) {
	if ctx.Err() != nil {
		return
	}
	collector.logf("aws collect: account=%s region=%s service=%s failed code=%s", c.unit.Account, c.unit.Region, c.unit.Service, code)
	checkpoint, readErr := collector.store.Checkpoint(c.unit.Account, c.unit.Region)
	poll := snapshot.ChangePoll{ErrorCode: code}
	if readErr == nil {
		lookback := options.ChangeLookback
		if retention := collector.store.ChangeRetention(); retention > 0 && retention < lookback {
			lookback = retention
		}
		for _, window := range checkpoint.Windows(c.now, lookback, options.ChangeOverlap) {
			poll.Gaps = append(poll.Gaps, snapshot.ChangeGapLabel{From: window.From.Truncate(time.Minute), To: window.To,
				Reason: snapshot.ChangeGapReasonForCode(code), Detail: deniedPermission(code)})
		}
	}
	if writeErr := c.writer.RecordChangePoll(c.unit.Region, poll); writeErr != nil && !errors.Is(writeErr, snapshot.ErrPurged) {
		collector.logf("aws collect: account=%s region=%s service=%s state not saved", c.unit.Account, c.unit.Region, c.unit.Service)
		return
	}
	if err := c.writer.RecordFailure(c.unit, code); err != nil && !errors.Is(err, snapshot.ErrPurged) {
		collector.logf("aws collect: account=%s region=%s service=%s state not saved", c.unit.Account, c.unit.Region, c.unit.Service)
	}
}

// deniedPermission names the permission a denied poll lacked.
func deniedPermission(code string) string {
	if code == CodeDenied {
		return "cloudtrail:LookupEvents"
	}
	return ""
}

// eventRate describes a window the page budget could not finish, as the
// event rate over the stretch that was read.
func eventRate(result windowRead) string {
	span := result.window.To.Sub(result.from)
	if span <= 0 || len(result.events) == 0 {
		return ""
	}
	perHour := int64(float64(len(result.events)) / span.Hours())
	return "events_per_hour=" + strconv.FormatInt(perHour, 10)
}

// pollEvents collects one poll's events across its windows. Each EventId
// is taken once: the forward window and a back-fill, or a repeated page,
// can return the same event, and counts must never double it.
type pollEvents struct {
	account, region string
	routine         map[string]bool
	// counted and stored hold the EventIds taken into the counts and into
	// the rows. An event is marked only when it is taken, so one that a
	// cut-short window could not count is still counted when a later
	// window of the same poll reads its minute in full.
	counted map[string]bool
	stored  map[string]bool
	counts  map[string]*storage.AWSChangeCount
	found   []model.Change
	// notChanges holds the time of each event, by EventId, that CloudTrail
	// flagged as a write but changes nothing (sign-ins, tokens, reads).
	notChanges map[string]time.Time
}

func newPollEvents(account, region string, routineEvents []string) *pollEvents {
	routine := make(map[string]bool, len(routineEvents))
	for _, name := range routineEvents {
		routine[name] = true
	}
	return &pollEvents{account: account, region: region, routine: routine, counted: map[string]bool{}, stored: map[string]bool{},
		counts: map[string]*storage.AWSChangeCount{}, notChanges: map[string]time.Time{}}
}

// add takes one event. Routine events and failed attempts are counted, but
// only in minutes the window read in full (from on); the rest of a cut-short
// window is counted when it is read again. Routine events are not stored.
func (events *pollEvents) add(event changes.Event, from time.Time) {
	if !keepEvent(events.region, event.Source) || events.counted[event.ID] && events.stored[event.ID] {
		return
	}
	if changes.IsNotChange(event.Source, event.Name) {
		events.notChanges[event.ID] = event.Time
		return
	}
	change, ok := changes.Normalize(events.account, events.region, event)
	if !ok {
		return
	}
	routine := events.routine[routineKey(change.EventSource, change.EventName)]
	if (routine || change.ErrorCode != "") && !events.counted[event.ID] && !event.Time.Before(from) {
		addCount(events.counts, change, routine)
		events.counted[event.ID] = true
	}
	if routine || events.stored[event.ID] {
		return
	}
	events.stored[event.ID] = true
	events.found = append(events.found, change)
}

// notChangeCount counts the non-change events in the stretch this poll
// newly claims: inside its covered intervals and outside what an earlier
// poll already covered. An overlap re-read or an unsettled tail is counted
// once, by the poll that settles it.
func (events *pollEvents) notChangeCount(checkpoint changes.Checkpoint, covered []changes.Interval) int64 {
	var count int64
	for _, at := range events.notChanges {
		if within(at, covered) && !within(at, checkpoint.Covered) {
			count++
		}
	}
	return count
}

func within(at time.Time, intervals []changes.Interval) bool {
	for _, interval := range intervals {
		if !at.Before(interval.From) && at.Before(interval.To) {
			return true
		}
	}
	return false
}

func (events *pollEvents) countList() []storage.AWSChangeCount {
	out := make([]storage.AWSChangeCount, 0, len(events.counts))
	for _, count := range events.counts {
		out = append(out, *count)
	}
	return out
}

func addCount(counts map[string]*storage.AWSChangeCount, change model.Change, routine bool) {
	kind := storage.AWSChangeCountFailure
	if routine {
		kind = storage.AWSChangeCountRoutine
	}
	minute := change.Time.UTC().Truncate(time.Minute)
	key := kind + "\x00" + change.EventName + "\x00" + change.Actor.PrincipalARN + "\x00" + change.Actor.SessionClass + "\x00" + change.ErrorCode + "\x00" + minute.Format(time.RFC3339)
	if existing := counts[key]; existing != nil {
		existing.Events++
		return
	}
	counts[key] = &storage.AWSChangeCount{
		Kind: kind, EventName: change.EventName, Actor: change.Actor.PrincipalARN, SessionClass: change.Actor.SessionClass, ErrorCode: change.ErrorCode,
		Minute: minute, Service: change.Service, EventSource: change.EventSource, ActorClass: string(change.Actor.Class), Events: 1,
	}
}

// windowRead is one window's read.
type windowRead struct {
	window changes.Interval
	events []changes.Event
	// from is the earliest time the read can vouch for: window.From when
	// every page was read, otherwise the minute after the oldest event
	// seen, or window.To when nothing could be read.
	from   time.Time
	err    error
	pages  int
	capped bool
	// malformed holds the event source of each event skipped for a missing
	// ID or time.
	malformed []string
	// skew is the read's clock skew estimate.
	skew clockSkew
}

// readWindow pages LookupEvents for write events in window, newest first,
// with the package page cap.
func readWindow(ctx context.Context, api *cloudtrail.Client, window changes.Interval) ([]changes.Event, time.Time, error) {
	result := readWindowPages(ctx, api, window, maxChangePages, time.Now)
	return result.events, result.from, result.err
}

func readWindowPages(ctx context.Context, api *cloudtrail.Client, window changes.Interval, pages int, clock func() time.Time) windowRead {
	from, to := window.From, window.To
	input := &cloudtrail.LookupEventsInput{
		StartTime:        &from,
		EndTime:          &to,
		MaxResults:       aws.Int32(changePageSize),
		LookupAttributes: []cloudtrailtypes.LookupAttribute{{AttributeKey: cloudtrailtypes.LookupAttributeKeyReadOnly, AttributeValue: aws.String("false")}},
	}
	result := windowRead{window: window, from: window.To}
	oldest := window.To
	// partial is the coverage of a read that stopped early. The minute of
	// the oldest event seen is left to the next read, so a claimed minute
	// is always read in full.
	partial := func() time.Time {
		if !oldest.Before(window.To) {
			return window.To
		}
		readFrom := oldest.Truncate(time.Minute).Add(time.Minute)
		if readFrom.Before(window.From) {
			return window.From
		}
		if readFrom.After(window.To) {
			return window.To
		}
		return readFrom
	}
	if pages <= 0 {
		result.capped = true
		return result
	}
	for page := 0; page < pages; page++ {
		sent := clock()
		output, err := api.LookupEvents(ctx, input)
		received := clock()
		result.pages++
		if err != nil {
			result.err, result.from = err, partial()
			return result
		}
		result.skew.sample(sent, received, responseTime(output.ResultMetadata))
		for _, item := range output.Events {
			event := changes.Event{
				ID:     deref(item.EventId),
				Name:   deref(item.EventName),
				Source: deref(item.EventSource),
				Time:   derefTime(item.EventTime),
				Raw:    deref(item.CloudTrailEvent),
			}
			event.ReadOnly = deref(item.ReadOnly)
			for _, resource := range item.Resources {
				if name := deref(resource.ResourceName); name != "" {
					event.Resources = append(event.Resources, name)
				}
			}
			if event.ID == "" || event.Time.IsZero() {
				// A malformed event is counted and skipped; it never
				// stops the rest of the page from being stored.
				result.malformed = append(result.malformed, event.Source)
				continue
			}
			result.events = append(result.events, event)
			if event.Time.Before(oldest) {
				oldest = event.Time
			}
		}
		if deref(output.NextToken) == "" {
			result.from = window.From
			return result
		}
		input.NextToken = output.NextToken
	}
	// The page budget stopped the read: the newest stretch down to the
	// oldest event seen is covered; the rest stays a too_busy gap.
	result.capped, result.from = true, partial()
	return result
}

// maxSkewRoundTrip is the longest request whose Date still estimates the
// clock: a longer one (a slow page, or retries after throttling inside
// the call) leaves too much doubt about when AWS stamped it.
const maxSkewRoundTrip = 5 * time.Second

// clockSkew estimates the local-minus-AWS clock offset from successful
// answers. Each answer's Date, whole seconds, was stamped somewhere inside
// the local round trip:
//   - value is the shown estimate: the Date against the middle of the
//     round trip, smallest absolute value first, from round trips of at
//     most maxSkewRoundTrip; only when there are none, from the slower
//     ones, so a busy account still gets a measure;
//   - upper is the most the local clock can be ahead (the arrival time
//     minus the Date, tightest over all answers). Coverage is capped with
//     it, so no estimate can claim coverage past AWS time.
type clockSkew struct {
	value   time.Duration
	known   bool
	precise bool
	upper   time.Duration
	bounded bool
}

func (skew *clockSkew) sample(sent, received, date time.Time) {
	if date.IsZero() || received.Before(sent) {
		return
	}
	if upper := received.Sub(date); !skew.bounded || upper < skew.upper {
		skew.upper, skew.bounded = upper, true
	}
	middle := sent.Add(received.Sub(sent) / 2)
	// Date truncates AWS time to the second: its middle is half a second on.
	value := middle.Sub(date.Add(500 * time.Millisecond))
	skew.add(value, received.Sub(sent) <= maxSkewRoundTrip)
}

func (skew *clockSkew) add(value time.Duration, precise bool) {
	switch {
	case precise && !skew.precise, !skew.known:
	case precise != skew.precise:
		return
	case absDuration(value) >= absDuration(skew.value):
		return
	}
	skew.value, skew.known, skew.precise = value, true, precise
}

func (skew *clockSkew) merge(other clockSkew) {
	if other.known {
		skew.add(other.value, other.precise)
	}
	if other.bounded && (!skew.bounded || other.upper < skew.upper) {
		skew.upper, skew.bounded = other.upper, true
	}
}

// ahead is how far the local clock may be ahead of AWS at most, for
// capping coverage; zero when nothing bounds it.
func (skew clockSkew) ahead() time.Duration {
	if !skew.bounded || skew.upper < 0 {
		return 0
	}
	return skew.upper
}

func absDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}

// responseTime reads the Date header of the raw AWS response.
func responseTime(metadata middleware.Metadata) time.Time {
	raw, ok := awsmiddleware.GetRawResponse(metadata).(*smithyhttp.Response)
	if !ok || raw == nil || raw.Response == nil {
		return time.Time{}
	}
	at, err := http.ParseTime(raw.Header.Get("Date"))
	if err != nil {
		return time.Time{}
	}
	return at.UTC()
}

// trailLimiter is the CloudTrail lane of the shared limiter: LookupEvents
// never runs above the rate AWS allows, and a CloudTrail throttle pauses
// only CloudTrail.
func (collector *Collector) trailLimiter() *limit.Manager {
	if collector.limiter == nil {
		return nil
	}
	return collector.limiter.Lane(limit.CloudTrailLane, limit.CloudTrailMaxRPS)
}

// globalChangeUnit is the account's global stream: read from us-east-1 and
// stored under the global region.
func globalChangeUnit(accountID string) plannedUnit {
	return plannedUnit{
		spec:           spec{service: snapshot.ChangeService, kind: KindChanges},
		unit:           model.Unit{Account: accountID, Region: model.GlobalRegion, Service: snapshot.ChangeService},
		limiterRegion:  catalog.GlobalHomeRegion,
		endpointRegion: catalog.GlobalHomeRegion,
	}
}
