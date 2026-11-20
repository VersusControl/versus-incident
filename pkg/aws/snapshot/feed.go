package snapshot

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// Windows the OSS change list serves. Longer windows are an Enterprise
// option; the OSS answer refuses them as locked.
const (
	FeedWindowHour = time.Hour
	FeedWindowDay  = 24 * time.Hour
	// MaxFeedWindow is the longest window OSS serves.
	MaxFeedWindow = FeedWindowDay
	// CursorAllowance is how long past the tier window a listing's later
	// pages may still be read.
	CursorAllowance = time.Hour
)

// Defaults of the change list.
const (
	DefaultFeedLimit   = 50
	MaxFeedLimit       = 200
	DefaultBurstWindow = 10 * time.Minute
	DefaultBurstMin    = 5
	// stackGroupWindow is how long after a stack operation the calls
	// CloudFormation made for the same principal join its group.
	stackGroupWindow = time.Hour
	// staleIntervals is how many poll intervals a pair may miss before its
	// unread tail is collector_down rather than still arriving.
	staleIntervals = 3
)

var (
	// ErrWindowLocked is a window longer than the tier serves.
	ErrWindowLocked = errors.New("aws changes: window locked")
	// ErrBadFeedQuery is a malformed filter or cursor.
	ErrBadFeedQuery = errors.New("aws changes: invalid query")
	// ErrGroupNotFound is an unknown group ID.
	ErrGroupNotFound = errors.New("aws changes: group not found")
	// ErrCursorExpired is a cursor or group token whose window starts
	// before the tier window plus the paging allowance.
	ErrCursorExpired = errors.New("aws changes: cursor expired")
)

// FeedOptions are the live settings the change list reads.
type FeedOptions struct {
	// Lookback is how far back a first poll reads; Overlap is the
	// unsettled tail; Interval is the poll interval.
	Lookback time.Duration
	Overlap  time.Duration
	Interval time.Duration
	// BurstWindow and BurstMin form error bursts: failed attempts by the
	// same principal, error code and service, each within BurstWindow of
	// the previous one, at least BurstMin of them.
	BurstWindow time.Duration
	BurstMin    int
	// ReadCap bounds the rows one answer reads.
	ReadCap int
}

// FeedRequest is one change list query.
type FeedRequest struct {
	Window time.Duration
	// Since, when set, makes the window exactly [Since, Until] and Window
	// is ignored.
	Since      time.Time
	Until      time.Time
	Account    string
	Region     string
	ActorClass string
	Service    string
	// Services narrows the list, its counts and paging to changes in any
	// of these services; empty means every service. The likely cause is
	// still chosen over every service.
	Services []string
	// ServiceFacet asks for the per-service counts and the suspect's
	// in_filter mark the Changes page shows.
	ServiceFacet bool
	Resource     string
	Text         string
	ShowRoutine  bool
	Cursor       string
	Limit        int
}

// ChangeFeed answers the change list from the store, the settings scope
// and the suspect engine.
type ChangeFeed struct {
	store   *Store
	scope   ScopeFunc
	options func() FeedOptions
	limits  func() ChangesLimits
	suspect SuspectEngine
	fields  FieldResolver
	now     func() time.Time
}

// FeedOption adjusts a ChangeFeed.
type FeedOption func(*ChangeFeed)

// WithScope wires the settings scope.
func WithScope(scope ScopeFunc) FeedOption {
	return func(feed *ChangeFeed) { feed.scope = scope }
}

// WithFeedOptions wires the live settings.
func WithFeedOptions(read func() FeedOptions) FeedOption {
	return func(feed *ChangeFeed) {
		if read != nil {
			feed.options = read
		}
	}
}

// ChangesLimits are how far back the change list and the inventory compare
// may reach. The edition decides them: the open-source build serves 24 h
// for both, and a licensed build may pass longer ones through
// WithChangesLimits.
type ChangesLimits struct {
	// MaxWindow is the longest change list window, and how far back its
	// first page may start.
	MaxWindow time.Duration
	// MaxCompareAge is how far back a compare may start.
	MaxCompareAge time.Duration
}

// DefaultChangesLimits are the open-source limits: 24 h for both.
func DefaultChangesLimits() ChangesLimits {
	return ChangesLimits{MaxWindow: MaxFeedWindow, MaxCompareAge: MaxFeedWindow}
}

// effective fills an unset limit with the open-source one, so a seam that
// returns a zero value never widens nor closes the page.
func (limits ChangesLimits) effective() ChangesLimits {
	if limits.MaxWindow <= 0 {
		limits.MaxWindow = MaxFeedWindow
	}
	if limits.MaxCompareAge <= 0 {
		limits.MaxCompareAge = MaxFeedWindow
	}
	return limits
}

// ChangesLimitsView is the limits block of the change list and compare
// answers. The page builds its window switch and compare start picker from
// it, so both durations are whole hours ("24h", "168h").
type ChangesLimitsView struct {
	MaxWindow     string `json:"max_window"`
	MaxCompareAge string `json:"max_compare_age"`
}

// View renders the limits for an answer.
func (limits ChangesLimits) View() ChangesLimitsView {
	limits = limits.effective()
	return ChangesLimitsView{MaxWindow: formatHours(limits.MaxWindow), MaxCompareAge: formatHours(limits.MaxCompareAge)}
}

func formatHours(value time.Duration) string {
	if value%time.Hour == 0 {
		return strconv.FormatInt(int64(value/time.Hour), 10) + "h"
	}
	return value.String()
}

// WithChangesLimits wires the edition's window and compare limits, read on
// every request so a license change applies without a restart. Without it
// the feed serves DefaultChangesLimits.
func WithChangesLimits(read func() ChangesLimits) FeedOption {
	return func(feed *ChangeFeed) {
		if read != nil {
			feed.limits = read
		}
	}
}

// Limits returns the limits this request is held to.
func (feed *ChangeFeed) Limits() ChangesLimits {
	if feed == nil || feed.limits == nil {
		return DefaultChangesLimits()
	}
	return feed.limits().effective()
}

// ParseWindow reads a change list window: 1h and 24h always, and whole
// days (7d, or 168h) up to limits.MaxWindow. locked reports a window
// longer than the limits serve, which callers answer as window_locked
// rather than as a bad request.
func ParseWindow(value string, limits ChangesLimits) (window time.Duration, locked bool, err error) {
	limits = limits.effective()
	value = strings.TrimSpace(value)
	switch value {
	case "", "24h":
		return FeedWindowDay, false, nil
	case "1h":
		return FeedWindowHour, false, nil
	}
	days := strings.HasSuffix(value, "d")
	var span time.Duration
	if days {
		count, convErr := strconv.Atoi(strings.TrimSuffix(value, "d"))
		if convErr != nil || count < 1 {
			return 0, false, errWindowValue
		}
		span = time.Duration(count) * FeedWindowDay
	} else {
		parsed, parseErr := time.ParseDuration(value)
		if parseErr != nil {
			return 0, false, errWindowValue
		}
		span = parsed
	}
	// Longer windows are whole days; 1d is not one of them.
	if span > FeedWindowDay && span%FeedWindowDay == 0 && span <= limits.MaxWindow {
		return span, false, nil
	}
	if span > limits.MaxWindow || days {
		return 0, true, nil
	}
	return 0, false, errWindowValue
}

// FormatWindow names a window the way the API takes it: 1h, 24h, 7d.
func FormatWindow(window time.Duration) string {
	if window > FeedWindowDay && window%FeedWindowDay == 0 {
		return strconv.FormatInt(int64(window/FeedWindowDay), 10) + "d"
	}
	if window%time.Hour == 0 {
		return strconv.FormatInt(int64(window/time.Hour), 10) + "h"
	}
	return window.String()
}

// errWindowValue is a window that is neither 1h, 24h nor a whole number
// of days.
var errWindowValue = errors.New("window must be 1h or 24h")

// WithSuspectEngine wires the suspect engine.
func WithSuspectEngine(engine SuspectEngine) FeedOption {
	return func(feed *ChangeFeed) {
		if engine != nil {
			feed.suspect = engine
		}
	}
}

// WithFieldResolver replaces the before-value resolver; the default reads
// the snapshot just before each change.
func WithFieldResolver(resolver FieldResolver) FeedOption {
	return func(feed *ChangeFeed) {
		if resolver != nil {
			feed.fields = resolver
		}
	}
}

// WithFeedClock injects the clock.
func WithFeedClock(now func() time.Time) FeedOption {
	return func(feed *ChangeFeed) {
		if now != nil {
			feed.now = now
		}
	}
}

// NewChangeFeed builds the change list over store.
func NewChangeFeed(store *Store, options ...FeedOption) *ChangeFeed {
	feed := &ChangeFeed{store: store, options: func() FeedOptions { return FeedOptions{} }, suspect: noSuspect{}, fields: storeFields{store: store}, now: time.Now}
	for _, option := range options {
		option(feed)
	}
	return feed
}

func (feed *ChangeFeed) effectiveOptions() FeedOptions {
	options := feed.options()
	if options.Lookback <= 0 {
		options.Lookback = 24 * time.Hour
	}
	if options.Overlap <= 0 {
		options.Overlap = 15 * time.Minute
	}
	if options.Interval <= 0 {
		options.Interval = 5 * time.Minute
	}
	if options.BurstWindow <= 0 {
		options.BurstWindow = DefaultBurstWindow
	}
	if options.BurstMin <= 0 {
		options.BurstMin = DefaultBurstMin
	}
	return options
}

// pairScope is one expected account and region.
type pairScope struct {
	account string
	region  string
	outage  ChangeGapReason
	since   time.Time
	// notEnabled marks a selected region AWS reports as not enabled: it is
	// listed with region_not_enabled, never expected to be read.
	notEnabled bool
}

// expectedPairs lists the pairs the answer must cover: every configured
// account's readable regions plus its global stream. Without a settings
// scope the stored pairs stand in.
func (feed *ChangeFeed) expectedPairs(ctx context.Context, request FeedRequest, stored storage.AWSChangeFeed) ([]pairScope, []ScopeAccount, error) {
	var accounts []ScopeAccount
	ok := false
	if feed.scope != nil {
		var err error
		accounts, ok, err = feed.scope(ctx)
		if err != nil {
			return nil, nil, err
		}
	}
	if !ok {
		byAccount := map[string]*ScopeAccount{}
		add := func(account, region string) {
			entry := byAccount[account]
			if entry == nil {
				entry = &ScopeAccount{Account: account}
				byAccount[account] = entry
			}
			if region != model.GlobalRegion && !containsString(entry.Regions, region) {
				entry.Regions = append(entry.Regions, region)
			}
		}
		for _, pair := range stored.Coverage {
			add(pair.Account, pair.Region)
		}
		for _, pair := range stored.Pairs {
			add(pair.Account, pair.Region)
		}
		accounts = nil
		for _, account := range sortedKeysOf(byAccount) {
			sort.Strings(byAccount[account].Regions)
			accounts = append(accounts, *byAccount[account])
		}
	}
	var pairs []pairScope
	for _, account := range accounts {
		if request.Account != "" && account.Account != request.Account {
			continue
		}
		// Every account expects its global stream, even before it has a
		// Region: an account nobody has read is never complete and empty.
		regions := append(append([]string(nil), account.Regions...), model.GlobalRegion)
		for _, region := range regions {
			if request.Region != "" && region != request.Region {
				continue
			}
			pairs = append(pairs, pairScope{account: account.Account, region: region, outage: account.Outage, since: account.OutageSince})
		}
		for _, region := range account.NotEnabled {
			if request.Region == "" || region == request.Region {
				pairs = append(pairs, pairScope{account: account.Account, region: region, notEnabled: true})
			}
		}
	}
	return pairs, accounts, nil
}

func sortedKeysOf[T any](values map[string]T) []string {
	out := make([]string, 0, len(values))
	for key := range values {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func containsString(values []string, value string) bool {
	for _, existing := range values {
		if existing == value {
			return true
		}
	}
	return false
}

// pairCoverage indexes the stored coverage, labels and state by pair.
type pairCoverage struct {
	covered []changes.Interval
	labels  []storage.AWSChangeGapReason
	state   *storage.AWSChangePairState
}

func indexPairs(stored storage.AWSChangeFeed) map[string]*pairCoverage {
	out := map[string]*pairCoverage{}
	get := func(account, region string) *pairCoverage {
		key := pairKey(account, region)
		if out[key] == nil {
			out[key] = &pairCoverage{}
		}
		return out[key]
	}
	for _, pair := range stored.Coverage {
		entry := get(pair.Account, pair.Region)
		for _, interval := range pair.Intervals {
			entry.covered = append(entry.covered, changes.Interval{From: interval.From, To: interval.To})
		}
	}
	for _, label := range stored.Gaps {
		entry := get(label.Account, label.Region)
		entry.labels = append(entry.labels, label)
	}
	for index := range stored.Pairs {
		state := stored.Pairs[index]
		get(state.Account, state.Region).state = &state
	}
	return out
}

// coverageEnd is the end of a pair's newest covered interval.
func (entry *pairCoverage) coverageEnd() time.Time {
	if entry == nil || len(entry.covered) == 0 {
		return time.Time{}
	}
	return (changes.Checkpoint{Covered: entry.covered}).CoveredUntil()
}

// polling reports whether the collector is still attempting the pair: an
// attempt, failed or not, within a few poll intervals. Only a pair that
// stopped being attempted is collector_down; one whose reads fail keeps its
// failure's reason, and a clock-skewed coverage end changes neither.
func (entry *pairCoverage) polling(now time.Time, options FeedOptions) bool {
	if entry == nil || entry.state == nil {
		return false
	}
	limit := time.Duration(staleIntervals)*options.Interval + options.Overlap
	return !entry.state.LastPollAt.Before(now.Add(-limit))
}

// caughtUp reports whether the pair's coverage reaches within the normal
// delivery lag of now (the overlap plus a few poll intervals). Only then is
// its unread tail still arriving; an older unread stretch of a pair being
// polled is a real gap.
func (entry *pairCoverage) caughtUp(now time.Time, options FeedOptions) bool {
	limit := time.Duration(staleIntervals)*options.Interval + options.Overlap
	end := entry.coverageEnd()
	return !end.IsZero() && !end.Before(now.Add(-limit))
}

// newestLabel is the pair's latest failure label when it reaches past the
// pair's coverage: the latest attempt failed, so the pair is failing.
func (entry *pairCoverage) newestLabel() *storage.AWSChangeGapReason {
	if entry == nil || len(entry.labels) == 0 {
		return nil
	}
	newest := entry.labels[0]
	for _, label := range entry.labels[1:] {
		if label.To.After(newest.To) {
			newest = label
		}
	}
	if newest.To.Before(entry.coverageEnd()) {
		return nil
	}
	return &newest
}

// gapContext is what labelling one pair's gaps needs.
type gapContext struct {
	pair            pairScope
	entry           *pairCoverage
	polling         bool
	caughtUp        bool
	failing         *storage.AWSChangeGapReason
	settledUntil    time.Time
	retentionCutoff time.Time
}

// labelGaps splits a pair's uncovered stretches at every boundary that
// changes their reason and names each piece. Pieces with the same reason
// are joined, so the list stays short under steady overload.
func labelGaps(ctx gapContext, since, until time.Time) []FeedGap {
	var covered []changes.Interval
	if ctx.entry != nil {
		covered = ctx.entry.covered
	}
	raw := (changes.Checkpoint{Covered: covered}).Gaps(since, until)
	var out []FeedGap
	for _, gap := range raw {
		cuts := []time.Time{gap.From, gap.To}
		addCut := func(at time.Time) {
			if at.After(gap.From) && at.Before(gap.To) {
				cuts = append(cuts, at)
			}
		}
		addCut(ctx.retentionCutoff)
		addCut(ctx.settledUntil)
		addCut(ctx.pair.since)
		if ctx.entry != nil {
			if ctx.entry.state != nil {
				addCut(ctx.entry.state.HistoryFrom)
			}
			for _, label := range ctx.entry.labels {
				addCut(label.From)
				addCut(label.To)
			}
		}
		sort.Slice(cuts, func(left, right int) bool { return cuts[left].Before(cuts[right]) })
		for index := 0; index+1 < len(cuts); index++ {
			from, to := cuts[index], cuts[index+1]
			if !to.After(from) {
				continue
			}
			reason, detail := ctx.reason(from)
			piece := FeedGap{Account: ctx.pair.account, Region: ctx.pair.region, From: from, To: to, Reason: string(reason)}
			if reason == GapCloudTrailDenied && detail != "" {
				permission := detail
				piece.MissingPermission = &permission
			}
			if reason == GapTooBusy {
				if rate, ok := strings.CutPrefix(detail, "events_per_hour="); ok {
					if value, err := strconv.ParseInt(rate, 10, 64); err == nil {
						piece.EventsPerHour = &value
					}
				}
			}
			if last := len(out) - 1; last >= 0 && out[last].Reason == piece.Reason && out[last].To.Equal(piece.From) &&
				equalPointer(out[last].MissingPermission, piece.MissingPermission) {
				out[last].To = piece.To
				continue
			}
			out = append(out, piece)
		}
	}
	return out
}

func equalPointer(left, right *string) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

// reason names the gap piece starting at from. Position decides first:
// past retention, before the first poll's reach; then an account-level
// outage; then the reason stored when the latest read of that stretch
// failed; then the unsettled tail of a pair being polled.
func (ctx gapContext) reason(from time.Time) (ChangeGapReason, string) {
	switch {
	case !ctx.retentionCutoff.IsZero() && from.Before(ctx.retentionCutoff):
		return GapBeyondRetention, ""
	case ctx.entry != nil && ctx.entry.state != nil && from.Before(ctx.entry.state.HistoryFrom):
		return GapBeforeConnect, ""
	case ctx.pair.outage != "" && !from.Before(ctx.pair.since):
		return ctx.pair.outage, ""
	}
	if ctx.entry != nil {
		for _, label := range ctx.entry.labels {
			if !from.Before(label.From) && from.Before(label.To) {
				reason := ChangeGapReason(label.Reason)
				if ctx.pair.region == model.GlobalRegion && reason == GapCloudTrailDenied {
					return GapGlobalUnknown, label.Detail
				}
				return reason, label.Detail
			}
		}
	}
	switch {
	case ctx.polling && ctx.failing != nil && !from.Before(ctx.failing.To):
		// Still attempted and still failing: the failure's reason runs to
		// now, the same every cycle.
		reason := ChangeGapReason(ctx.failing.Reason)
		if ctx.pair.region == model.GlobalRegion && reason == GapCloudTrailDenied {
			return GapGlobalUnknown, ctx.failing.Detail
		}
		return reason, ctx.failing.Detail
	case ctx.polling && ctx.caughtUp && ctx.failing == nil && !from.Before(ctx.settledUntil):
		return GapArriving, ""
	case ctx.pair.outage != "":
		return ctx.pair.outage, ""
	case ctx.entry == nil || ctx.entry.state == nil:
		return GapChangesNotChecked, ""
	case !ctx.polling:
		return GapCollectorDown, ""
	}
	return GapChangesNotChecked, ""
}

// coverageView is the gap and completeness half of an answer.
type coverageView struct {
	since, until, settled time.Time
	gaps                  []FeedGap
	accounts              []FeedAccount
	regions               []FeedRegion
	complete              bool
	pending               bool
	sync                  FeedSync
	// pairs is how many account and region pairs the answer expects.
	pairs int
}

// settledFunc answers whether a window is read and settled as far as this
// answer knows: it ends by settled_until and no real gap of the pairs it
// names overlaps it. The part before the answer's window is not judged
// here; the caller's own read covers it.
func (view coverageView) settledFunc() func(account, region string, from, to time.Time) bool {
	return func(account, region string, from, to time.Time) bool {
		if to.After(view.settled) {
			return false
		}
		if from.Before(view.since) {
			from = view.since
		}
		for _, gap := range view.gaps {
			if !realGap(gap) || gap.Account != account || region != "" && gap.Region != region {
				continue
			}
			if gap.From.Before(to) && gap.To.After(from) {
				return false
			}
		}
		return true
	}
}

// realGap reports a gap that makes the answer incomplete; the arriving
// tail does not.
func realGap(gap FeedGap) bool {
	return gap.Reason != string(GapArriving) && !gap.confirmedOff
}

func (feed *ChangeFeed) coverage(pairs []pairScope, accounts []ScopeAccount, stored storage.AWSChangeFeed, since, until, now time.Time, options FeedOptions) coverageView {
	// A not-enabled region has nothing to read: it is listed as such for
	// the whole window, and it neither makes the answer incomplete nor
	// counts as a pair that should have been read.
	index := indexPairs(stored)
	var notEnabled []FeedGap
	read := make([]pairScope, 0, len(pairs))
	for _, pair := range pairs {
		if pair.notEnabled {
			entry := index[pairKey(pair.account, pair.region)]
			if entry == nil {
				notEnabled = append(notEnabled, FeedGap{Account: pair.account, Region: pair.region, From: since, To: until, Reason: string(GapRegionNotEnabled), confirmedOff: true})
				continue
			}
			// It was polled before Settings recorded it as off: what was
			// read counts, and a stretch whose read failed for another
			// reason keeps that real reason. An opt-in refusal and the
			// unread rest are now the confirmed region_not_enabled.
			ctx := gapContext{pair: pair, entry: entry, settledUntil: until, retentionCutoff: time.Time{}}
			for _, gap := range labelGaps(ctx, since, until) {
				if !labelled(entry, gap.From) || gap.Reason == string(GapRegionNotEnabled) {
					gap.Reason, gap.MissingPermission, gap.EventsPerHour, gap.confirmedOff = string(GapRegionNotEnabled), nil, nil, true
				}
				notEnabled = append(notEnabled, gap)
			}
			continue
		}
		read = append(read, pair)
	}
	pairs = read
	view := coverageView{since: since, until: until, gaps: []FeedGap{}, accounts: []FeedAccount{}, regions: []FeedRegion{}, pairs: len(pairs)}
	retention := feed.store.ChangeRetention()
	var retentionCutoff time.Time
	if retention > 0 {
		retentionCutoff = now.Add(-retention)
	}

	settled := time.Time{}
	for _, pair := range pairs {
		entry := index[pairKey(pair.account, pair.region)]
		// A failing pair's tail is a named gap; it does not hold back the
		// settled point of the pairs that read.
		end := entry.coverageEnd()
		if !entry.polling(now, options) || !entry.caughtUp(now, options) || entry.newestLabel() != nil || end.IsZero() {
			continue
		}
		if settled.IsZero() || end.Before(settled) {
			settled = end
		}
	}
	if settled.IsZero() {
		settled = now.Add(-options.Overlap)
	}
	if settled.After(until) {
		settled = until
	}
	if settled.Before(since) {
		settled = since
	}
	view.settled = settled

	incomplete := map[string]string{}
	regionIncomplete := map[string]bool{}
	for _, pair := range pairs {
		entry := index[pairKey(pair.account, pair.region)]
		ctx := gapContext{pair: pair, entry: entry, polling: entry.polling(now, options), caughtUp: entry.caughtUp(now, options), failing: entry.newestLabel(),
			settledUntil: settled, retentionCutoff: retentionCutoff}
		for _, gap := range labelGaps(ctx, since, until) {
			view.gaps = append(view.gaps, gap)
			if realGap(gap) {
				if _, ok := incomplete[pair.account]; !ok {
					incomplete[pair.account] = gap.Reason
				}
				regionIncomplete[pair.region] = true
			}
		}
		if entry == nil || entry.state == nil {
			view.pending = true
			continue
		}
		state := entry.state
		if view.sync.LastPollAt == nil || state.LastPollAt.After(*view.sync.LastPollAt) {
			at := state.LastPollAt
			view.sync.LastPollAt = &at
		}
		view.sync.LateArrivals += state.LateArrivals
		view.sync.Dropped += state.Dropped
		view.sync.NotChanges += state.NotChanges
		if skew := int64(state.ClockSkew / time.Second); abs(skew) > abs(view.sync.ClockSkewSeconds) {
			view.sync.ClockSkewSeconds = skew
		}
	}
	view.sync.FirstReadPending = view.pending
	view.complete = len(pairs) > 0
	for _, gap := range view.gaps {
		if realGap(gap) {
			view.complete = false
			break
		}
	}

	regions := map[string]string{}
	for _, account := range accounts {
		if feed.hasPairFor(pairs, account.Account) {
			reason, bad := incomplete[account.Account]
			entry := FeedAccount{Account: account.Account, Name: account.Name, Complete: !bad}
			if bad {
				entry.Reason = &reason
			}
			if account.Outage != "" {
				entry.AsOf = outageAsOf(account, pairs, index)
			}
			if account.Disconnected {
				// Settings has recorded the account disconnected: that is
				// its one reason, whatever role failure lies behind it.
				reason := string(GapAccountDisconnected)
				entry.Complete, entry.Reason = false, &reason
				if !account.OutageSince.IsZero() {
					at := account.OutageSince
					entry.DisconnectedAt = &at
				}
			}
			view.accounts = append(view.accounts, entry)
		}
		for _, region := range account.NotEnabled {
			if regions[region] == "" {
				regions[region] = RegionNotEnabled
			}
		}
	}
	for _, pair := range pairs {
		state := RegionSelected
		if pair.region == model.GlobalRegion {
			state = RegionGlobal
		}
		regions[pair.region] = state
	}
	for _, region := range sortedKeysOf(regions) {
		state := regions[region]
		view.regions = append(view.regions, FeedRegion{Region: region, State: state, Complete: state != RegionNotEnabled && !regionIncomplete[region]})
	}
	view.gaps = append(view.gaps, notEnabled...)
	// A failed read of a region Settings later recorded as off is still a
	// real gap: the answer and its account are incomplete.
	for _, gap := range notEnabled {
		if !realGap(gap) {
			continue
		}
		view.complete = false
		for index := range view.accounts {
			if view.accounts[index].Account == gap.Account && view.accounts[index].Complete {
				reason := gap.Reason
				view.accounts[index].Complete, view.accounts[index].Reason = false, &reason
			}
		}
		for index := range view.regions {
			if view.regions[index].Region == gap.Region {
				view.regions[index].Complete = false
			}
		}
	}
	return view
}

// settleDisconnected gives each account Settings records as disconnected
// one story. Each enabled Region's settled coverage ends where its trailing
// run of gaps begins; the account's as-of time is the earliest of those
// ends. The account will not be read again, so its not-yet-settled tail is
// a real account_disconnected gap rather than still arriving. Gaps are
// never trimmed to fit stored rows: a row newer than the as-of time was
// read, but late events around it may be missing. Every gap in the trailing
// run reads account_disconnected, whatever the collector recorded there (a
// role failure, say); a gap before the run keeps its own reason, and a
// Region confirmed off stays as it is.
func (view *coverageView) settleDisconnected(accounts []ScopeAccount, pairs []pairScope) {
	for _, account := range accounts {
		if !account.Disconnected {
			continue
		}
		var asOf time.Time
		trailing := map[string]time.Time{}
		for _, pair := range pairs {
			if pair.account != account.Account || pair.notEnabled {
				continue
			}
			end := view.until
			if start := trailingGapStart(view.gaps, pair.account, pair.region, view.until); !start.IsZero() {
				end = start
				trailing[pair.region] = start
			}
			if asOf.IsZero() || end.Before(asOf) {
				asOf = end
			}
		}
		if asOf.IsZero() {
			continue
		}
		for index := range view.gaps {
			gap := &view.gaps[index]
			if gap.Account != account.Account || gap.confirmedOff {
				continue
			}
			start, ok := trailing[gap.Region]
			inTrailing := ok && !gap.From.Before(start)
			if gap.Reason != string(GapArriving) && !inTrailing {
				continue
			}
			gap.Reason = string(GapAccountDisconnected)
			view.complete = false
			for region := range view.regions {
				if view.regions[region].Region == gap.Region {
					view.regions[region].Complete = false
				}
			}
		}
		for index := range view.accounts {
			if view.accounts[index].Account == account.Account {
				at := asOf
				view.accounts[index].AsOf = &at
			}
		}
	}
}

// trailingGapStart is where the pair's run of gaps, real or still arriving,
// reaching the end of the window begins; zero when the pair's window does
// not end in a gap.
func trailingGapStart(gaps []FeedGap, account, region string, until time.Time) time.Time {
	var pair []FeedGap
	for _, gap := range gaps {
		if gap.Account == account && gap.Region == region && !gap.confirmedOff {
			pair = append(pair, gap)
		}
	}
	sort.Slice(pair, func(left, right int) bool { return pair[left].From.Before(pair[right].From) })
	var start time.Time
	reach := until
	for index := len(pair) - 1; index >= 0; index-- {
		if pair[index].To.Before(reach) {
			break
		}
		start, reach = pair[index].From, pair[index].From
	}
	return start
}

// outageAsOf is the time an unreadable account's changes are known up to:
// the end of its newest change coverage, else its last successful check,
// else when the outage began. Every account-level outage carries it.
func outageAsOf(account ScopeAccount, pairs []pairScope, index map[string]*pairCoverage) *time.Time {
	var at time.Time
	for _, pair := range pairs {
		if pair.account != account.Account {
			continue
		}
		if end := index[pairKey(pair.account, pair.region)].coverageEnd(); end.After(at) {
			at = end
		}
	}
	if at.IsZero() {
		at = account.LastReadAt
	}
	if at.IsZero() {
		at = account.OutageSince
	}
	if at.IsZero() {
		return nil
	}
	return &at
}

// labelled reports whether a stored failure label covers the time.
func labelled(entry *pairCoverage, at time.Time) bool {
	for _, label := range entry.labels {
		if !at.Before(label.From) && at.Before(label.To) {
			return true
		}
	}
	return false
}

func (feed *ChangeFeed) hasPairFor(pairs []pairScope, account string) bool {
	for _, pair := range pairs {
		if pair.account == account {
			return true
		}
	}
	return false
}

func abs(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

// bucketWidth is five minutes up to an hour, an hour up to a day and six
// hours beyond, so the grid has 12, 24 or (for 7 days) 28 columns and a
// long window still fits on the page.
func bucketWidth(window time.Duration) time.Duration {
	switch {
	case window <= FeedWindowHour:
		return 5 * time.Minute
	case window <= FeedWindowDay:
		return time.Hour
	}
	return 6 * time.Hour
}

// buckets fills the activity grid from minute counts. Buckets are UTC
// steps from since, so a daylight-saving change never adds or drops one.
func buckets(since, until time.Time, width time.Duration, stored storage.AWSChangeFeed, failureRows []storage.AWSChange, view coverageView) []FeedBucket {
	count := int(until.Sub(since) / width)
	if until.Sub(since)%width != 0 {
		count++
	}
	out := make([]FeedBucket, count)
	for index := range out {
		out[index] = FeedBucket{Start: since.Add(time.Duration(index) * width).UTC(), State: BucketComplete}
	}
	slot := func(at time.Time) int {
		if at.Before(since) || !at.Before(until) {
			return -1
		}
		return int(at.Sub(since) / width)
	}
	// The store groups changes by their truncated minute, so when since is
	// not on a minute boundary the window's first minute row starts before
	// since. The summary counts that row, so it belongs in the first bucket.
	minuteSlot := func(minute time.Time) int {
		if minute.Before(since) && minute.Add(time.Minute).After(since) {
			return slot(since)
		}
		return slot(minute)
	}
	for _, minute := range stored.Minutes {
		position := minuteSlot(minute.Minute)
		if position < 0 {
			continue
		}
		switch model.ActorClass(minute.ActorClass) {
		case model.ActorPipeline:
			out[position].Pipeline += minute.Events
		case model.ActorPerson:
			out[position].Person += minute.Events
		case model.ActorAWSService:
			out[position].AWSService += minute.Events
		default:
			out[position].Unknown += minute.Events
		}
	}
	if failureRows != nil {
		for _, row := range failureRows {
			if position := slot(row.EventTime); position >= 0 {
				out[position].Errors++
			}
		}
	} else {
		for _, count := range stored.Counts {
			if count.Kind != storage.AWSChangeCountFailure {
				continue
			}
			if position := minuteSlot(count.Minute); position >= 0 {
				out[position].Errors += count.Events
			}
		}
	}
	for index := range out {
		start := out[index].Start
		end := start.Add(width)
		if end.After(until) {
			end = until
		}
		out[index].State, out[index].GapAccounts = bucketState(start, end, view)
	}
	return out
}

// bucketState names one bucket's state from the real gaps in it. A bucket
// is gap only when no pair in scope was read for any of it; when some pairs
// were read it is partial: its counts are what was read, a lower bound,
// and gap_accounts names the accounts not read. Gaps of one account never
// hide what another account read.
func bucketState(start, end time.Time, view coverageView) (string, []string) {
	unread := map[string]time.Duration{}
	accounts := map[string]bool{}
	for _, gap := range view.gaps {
		if !realGap(gap) || !gap.From.Before(end) || !gap.To.After(start) {
			continue
		}
		from, to := gap.From, gap.To
		if from.Before(start) {
			from = start
		}
		if to.After(end) {
			to = end
		}
		unread[pairKey(gap.Account, gap.Region)] += to.Sub(from)
		accounts[gap.Account] = true
	}
	gapAccounts := sortedKeysOf(accounts)
	if len(unread) == 0 {
		if end.After(view.settled) {
			return BucketArriving, []string{}
		}
		return BucketComplete, []string{}
	}
	whole := 0
	for _, missing := range unread {
		if missing >= end.Sub(start) {
			whole++
		}
	}
	if view.pairs > 0 && whole >= view.pairs {
		return BucketGap, gapAccounts
	}
	return BucketPartial, gapAccounts
}
