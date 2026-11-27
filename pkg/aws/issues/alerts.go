package issues

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/VersusControl/versus-incident/pkg/aws/catalog"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/logging"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	"github.com/VersusControl/versus-incident/pkg/utils"
)

// AlertKind is why an alert is sent.
type AlertKind string

// Alert kinds.
const (
	// AlertOpened: a critical or high issue opened.
	AlertOpened AlertKind = "opened"
	// AlertRegressed: a critical or high issue came back after it
	// resolved, or a rejection no longer holds.
	AlertRegressed AlertKind = "regressed"
	// AlertCoverage: an input of an open critical or high issue is no
	// longer read, so the issue can't be confirmed.
	AlertCoverage AlertKind = "coverage"
)

// High issues are batched into one digest when more than DigestOver open
// within DigestWindow.
const (
	DigestWindow = time.Hour
	DigestOver   = 3
)

// Delivery of a recorded alert: a send that fails is tried again on a later
// pump, first after RetryAfter, then twice as long each time up to
// RetryAtMost, and given up GiveUpAfter after it was recorded.
const (
	RetryAfter  = time.Minute
	RetryAtMost = time.Hour
	GiveUpAfter = 24 * time.Hour
)

// Details of the alerted events. The alert path tells a coverage notice
// from an alert on the issue's opening or regress by them.
const (
	alertedOpened    = "alert recorded: opened"
	alertedRegressed = "alert recorded: regressed"
	alertedDigest    = "alert recorded: in a digest of high issues"
	alertedCoverage  = "alert recorded: coverage notice"
)

// AlertMark is what the alert path last recorded on an issue.
type AlertMark struct {
	// Version is the issue version the alert path wrote. While the issue
	// is still at it, nothing has happened since and its events need no
	// read.
	Version int64 `json:"version"`
	// At is when the latest alert on the issue's opening or regress was
	// recorded; zero when none was.
	At time.Time `json:"at,omitzero"`
	// Noticed is when the coverage notice since that opening or regress
	// was recorded; zero when none was.
	Noticed time.Time `json:"noticed,omitzero"`
	// Pending are the alerts recorded and not yet delivered.
	Pending []PendingAlert `json:"pending,omitempty"`
}

// PendingAlert is one alert recorded on an issue and not yet delivered. It
// keeps everything a later send needs, so every attempt sends the same
// message under the same key, and the alert path drops a repeat.
type PendingAlert struct {
	Kind   AlertKind `json:"kind"`
	Key    string    `json:"key"`
	Detail string    `json:"detail,omitempty"`
	// Group names the digest the alert goes out in; empty sends it alone.
	Group      string    `json:"group,omitempty"`
	Unread     []Unread  `json:"unread,omitempty"`
	RecordedAt time.Time `json:"recorded_at"`
	// Attempts counts the sends that failed; NextAt is when the next is
	// due.
	Attempts int       `json:"attempts,omitempty"`
	NextAt   time.Time `json:"next_at"`
	// LastError is the redacted reason the latest send failed.
	LastError string `json:"last_error,omitempty"`
}

// Unread is one input of an issue that is not read.
type Unread struct {
	// Source is the collector unit service; Operation the detail call,
	// empty when the whole unit did not read.
	Source     string          `json:"source"`
	Operation  string          `json:"operation,omitempty"`
	Reason     model.GapReason `json:"reason,omitempty"`
	LastGoodAt time.Time       `json:"last_good_at,omitzero"`
	// Fix lists the IAM actions that allow the read again, for a denial.
	Fix []string `json:"fix,omitempty"`
}

// Alert is one message about one issue.
type Alert struct {
	Kind  AlertKind
	Issue Issue
	// Detail says why a regressed issue came back.
	Detail string
	// Key names the transition the alert is for: every attempt to send
	// the same alert carries the same key.
	Key string
	// AccountName is the account's display name; empty uses its ID.
	AccountName string
	// Unread are the inputs a coverage notice is about.
	Unread []Unread
	// Group names the digest the alert goes out in; every alert of one
	// digest carries it, on every attempt.
	Group string
}

// AlertSink sends alerts through the deployment's alert path.
type AlertSink interface {
	Send(ctx context.Context, alert Alert) error
	// SendDigest sends high issues as one message.
	SendDigest(ctx context.Context, alerts []Alert) error
}

// UnreadFinder names the inputs of an issue that are not read now.
type UnreadFinder func(ctx context.Context, issue Issue) ([]Unread, error)

// Alerter sends each issue alert once. After every evaluation it pumps:
// it finds the critical and high issues whose opening, regress or loss of
// coverage has no alert yet, records the alert on the issue as pending
// under compare-and-swap, and then sends what is pending. A send that
// fails, or that a crash cut off, stays pending and is sent again on a
// later pump under the same key, which the alert path deduplicates; after
// GiveUpAfter it is given up with one warning. A restart or a second
// replica never records one transition twice. Medium and low issues and
// rejected issues are never sent. It also looks up the likely cause of
// every issue that opened or regressed.
type Alerter struct {
	store  storage.AWSIssueStore
	scope  tenancy.OrgScope
	sink   AlertSink
	cause  CauseFinder
	unread UnreadFinder
	names  func(ctx context.Context) map[string]string
	now    func() time.Time
	logf   func(format string, args ...any)
	debugf func(format string, args ...any)

	mu       sync.Mutex
	failures failureLog
}

// AlerterOption adjusts an Alerter.
type AlerterOption func(*Alerter)

// WithCauseFinder sets the likely-cause lookup.
func WithCauseFinder(cause CauseFinder) AlerterOption {
	return func(a *Alerter) { a.cause = cause }
}

// WithUnreadFinder sets how a coverage notice names what is not read. With
// it, a notice goes out only when an input is not read; an input that is
// only capped sends none.
func WithUnreadFinder(unread UnreadFinder) AlerterOption {
	return func(a *Alerter) { a.unread = unread }
}

// WithAccountNames sets the account display names alerts show.
func WithAccountNames(names func(ctx context.Context) map[string]string) AlerterOption {
	return func(a *Alerter) { a.names = names }
}

// WithAlertClock injects the clock.
func WithAlertClock(now func() time.Time) AlerterOption {
	return func(a *Alerter) {
		if now != nil {
			a.now = now
		}
	}
}

// WithAlertLogger sets the log function for failures a user can act on.
func WithAlertLogger(logf func(string, ...any)) AlerterOption {
	return func(a *Alerter) {
		if logf != nil {
			a.logf = logf
		}
	}
}

// NewAlerter builds the alert pump over provider's issue store.
func NewAlerter(provider storage.Provider, scope tenancy.OrgScope, sink AlertSink, options ...AlerterOption) (*Alerter, error) {
	store, ok := storage.AWSIssuesOf(provider)
	if !ok {
		return nil, ErrNoStore
	}
	if sink == nil {
		return nil, errors.New("issues: an alerter needs a sink")
	}
	a := &Alerter{store: store, scope: scope.Normalized(), sink: sink, now: time.Now, logf: func(string, ...any) {}, debugf: logging.Debugf}
	for _, option := range options {
		option(a)
	}
	return a, nil
}

// plan is what one pump does for one issue.
type plan struct {
	issue Issue
	// transition is the issue's latest opened or regressed event.
	transition Event
	alert      bool
	coverage   bool
	cause      bool
	unread     []Unread
	group      string
}

// Pump records the alerts due after an evaluation, then delivers every
// recorded alert whose send is due. report names the issues that opened
// or regressed in it, whose likely cause is looked up whatever their
// severity; every critical and high issue in the store is checked as well,
// so an alert a restart cut off is still recorded. An alert is recorded on
// its issue as pending before it is sent, and stays pending until a send
// succeeds: a failed send or a crash between the record and the send is
// sent again on a later pump under the same key.
func (a *Alerter) Pump(ctx context.Context, report Report) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now().UTC()
	list, err := a.list(ctx)
	if err != nil {
		a.fail("read", fmt.Errorf("%w: %w", errAlertRead, err), now)
		return err
	}
	a.failures.clear("read")
	moved := map[string]bool{}
	for _, account := range report.Accounts {
		for _, change := range account.Changes {
			for _, event := range change.Events {
				if event.Kind == EventOpened || event.Kind == EventRegressed {
					moved[change.Issue.ID] = true
				}
			}
		}
	}
	var plans []*plan
	for _, issue := range list {
		stale := issue.Alerted == nil || issue.Alerted.Version != issue.Version
		// An issue that can't be confirmed and has had no coverage notice
		// is looked at on every pump: an input that was capped may since
		// have stopped being read, which changes nothing on the issue.
		unnoticed := issue.Confirm == CantConfirm && issue.Alerted != nil && issue.Alerted.Noticed.IsZero()
		if !moved[issue.ID] && !(alertable(issue) && (stale || unnoticed)) {
			continue
		}
		records, err := a.store.AWSIssueEvents(ctx, a.scope, issue.ID)
		if err != nil {
			a.fail("read", fmt.Errorf("%w: %w", errAlertRead, err), now)
			return err
		}
		events := make([]Event, 0, len(records))
		for _, record := range records {
			events = append(events, EventFromRecord(record))
		}
		if p := decide(issue, events); p != nil {
			plans = append(plans, p)
		}
	}
	a.findUnread(ctx, plans)
	a.groupHighs(list, plans, now)
	byID := make(map[string]Issue, len(list))
	for _, issue := range list {
		byID[issue.ID] = issue
	}
	for _, recorded := range a.record(ctx, plans, now) {
		byID[recorded.ID] = recorded
	}
	return a.deliver(ctx, byID, now)
}

// alertable is an issue the alert path sends: open and critical or high.
func alertable(issue Issue) bool {
	return issue.State.Active() && issue.Severity.Rank() >= SeverityHigh.Rank()
}

// disconnectDetails are the details of a resolve that only says the
// account or Region is no longer read.
var disconnectDetails = map[string]bool{"account no longer connected": true, "Region no longer read": true}

// decide reads an issue's events in the order they were written: what its
// latest opening or regress still needs. Nil when nothing is to be
// written. An opening after a disconnect resolved an issue that was
// alerted (the account or Region was connected again) needs no alert: the
// problem never went away, the reads did.
func decide(issue Issue, events []Event) *plan {
	events = slices.Clone(events)
	sort.SliceStable(events, func(left, right int) bool { return events[left].Seq < events[right].Seq })
	last := -1
	for index, event := range events {
		if event.Kind == EventOpened || event.Kind == EventRegressed {
			last = index
		}
	}
	p := &plan{issue: issue}
	if last >= 0 {
		p.transition = events[last]
		alerted, noticed := false, false
		for _, event := range events[last+1:] {
			if event.Kind != EventAlerted {
				continue
			}
			if event.Detail == alertedCoverage {
				noticed = true
			} else {
				alerted = true
			}
		}
		if !alerted && p.transition.Kind == EventOpened && alertedBeforeDisconnect(events[:last]) {
			alerted = true
		}
		p.alert = alertable(issue) && !alerted
		p.coverage = alertable(issue) && issue.Confirm == CantConfirm && !noticed
		p.cause = issue.Cause == nil || issue.Cause.Checked.Before(p.transition.At)
	}
	if !p.alert && !p.coverage && !p.cause && issue.Alerted != nil && issue.Alerted.Version == issue.Version {
		return nil
	}
	return p
}

// alertedBeforeDisconnect reports whether the latest resolve in events was
// a disconnect, and the opening or regress before it was alerted.
func alertedBeforeDisconnect(events []Event) bool {
	resolve := -1
	for index, event := range events {
		if event.Kind == EventResolved {
			resolve = index
		}
	}
	if resolve < 0 || !disconnectDetails[events[resolve].Detail] {
		return false
	}
	for index := resolve - 1; index >= 0; index-- {
		switch events[index].Kind {
		case EventAlerted:
			if events[index].Detail != alertedCoverage {
				return true
			}
		case EventOpened, EventRegressed:
			return false
		}
	}
	return false
}

// findUnread names what each coverage notice is about. A notice with no
// input that is not read (only capped) is not sent.
func (a *Alerter) findUnread(ctx context.Context, plans []*plan) {
	if a.unread == nil {
		return
	}
	for _, p := range plans {
		if !p.coverage {
			continue
		}
		unread, err := a.unread(ctx, p.issue)
		if err != nil {
			a.debugf("aws issues: issue %s: inputs not read could not be listed: %v", p.issue.ID, err)
		}
		p.unread = unread
		p.coverage = len(unread) > 0
	}
}

// groupHighs batches the high alerts of this pump into one digest when
// two or more are due and, with the high alerts recorded in the last
// DigestWindow, more than DigestOver opened. A lone high alert always goes
// out on its own. The digest's group key names its alerts, so every
// attempt to send it carries the same key.
func (a *Alerter) groupHighs(list []Issue, plans []*plan, now time.Time) {
	pending := map[string]bool{}
	var highs []*plan
	for _, p := range plans {
		if p.alert && p.issue.Severity == SeverityHigh {
			pending[p.issue.ID] = true
			highs = append(highs, p)
		}
	}
	recent := 0
	for _, issue := range list {
		if !pending[issue.ID] && issue.Severity == SeverityHigh && issue.State.Active() && issue.Alerted != nil &&
			!issue.Alerted.At.IsZero() && now.Sub(issue.Alerted.At) < DigestWindow {
			recent++
		}
	}
	if len(highs) < 2 || len(highs)+recent <= DigestOver {
		return
	}
	keys := make([]string, 0, len(highs))
	for _, p := range highs {
		keys = append(keys, p.issue.ID+":"+transitionKey(p.transition))
	}
	sort.Strings(keys)
	sum := sha256.Sum256([]byte(strings.Join(keys, "\n")))
	group := "digest:" + hex.EncodeToString(sum[:16])
	for _, p := range highs {
		p.group = group
	}
}

// transitionKey names an opening or regress: every alert about it carries
// the same key.
func transitionKey(transition Event) string {
	return string(transition.Kind) + ":" + strconv.FormatInt(transition.At.UTC().UnixNano(), 10)
}

// record writes each plan on its issue under compare-and-swap: the likely
// cause, the alerted events, and each alert as pending. It returns the
// issues as written. An issue that changed since it was read is left for
// the next pump.
func (a *Alerter) record(ctx context.Context, plans []*plan, now time.Time) []Issue {
	var out []Issue
	for _, p := range plans {
		before := p.issue
		after := before
		looked := false
		if p.cause && a.cause != nil {
			cause, err := a.cause(ctx, before)
			if err != nil {
				a.debugf("aws issues: issue %s: likely cause not looked up: %v", before.ID, err)
			} else {
				after.Cause, looked = cause, true
			}
		}
		current := before.Alerted != nil && before.Alerted.Version == before.Version
		if !p.alert && !p.coverage && !looked && current {
			// Nothing to record, and the mark is up to date.
			continue
		}
		after.Version, after.UpdatedAt = before.Version+1, now
		mark := AlertMark{Version: after.Version}
		if before.Alerted != nil {
			mark.At, mark.Noticed = before.Alerted.At, before.Alerted.Noticed
			mark.Pending = slices.Clone(before.Alerted.Pending)
		}
		var events []Event
		if p.alert {
			mark.At, mark.Noticed = now, time.Time{}
			detail := alertedOpened
			switch {
			case p.group != "":
				detail = alertedDigest
			case p.transition.Kind == EventRegressed:
				detail = alertedRegressed
			}
			events = append(events, Event{IssueID: before.ID, At: now, Kind: EventAlerted, Actor: EvaluatorActor, Detail: detail})
			entry := PendingAlert{Kind: AlertOpened, Key: transitionKey(p.transition), Group: p.group, RecordedAt: now, NextAt: now}
			if p.transition.Kind == EventRegressed {
				entry.Kind, entry.Detail = AlertRegressed, p.transition.Detail
			}
			mark.Pending = append(mark.Pending, entry)
		}
		if p.coverage {
			mark.Noticed = now
			events = append(events, Event{IssueID: before.ID, At: now, Kind: EventAlerted, Actor: EvaluatorActor, Detail: alertedCoverage})
			mark.Pending = append(mark.Pending, PendingAlert{Kind: AlertCoverage, Key: "coverage:" + strconv.FormatInt(p.transition.At.UTC().UnixNano(), 10),
				Unread: p.unread, RecordedAt: now, NextAt: now})
		}
		after.Alerted = &mark
		if err := a.write(ctx, before, after, events); err != nil {
			if !errors.Is(err, storage.ErrAWSIssueConflict) {
				a.fail("record", fmt.Errorf("%w: %w", errAlertRecord, err), now)
			}
			continue
		}
		a.failures.clear("record")
		out = append(out, after)
	}
	return out
}

func (a *Alerter) write(ctx context.Context, before, after Issue, events []Event) error {
	write, err := writeOf(Change{Issue: after, Before: &before, Events: events})
	if err != nil {
		return err
	}
	return a.store.WriteAWSIssues(ctx, a.scope, storage.AWSIssueBatch{AccountID: before.Account, Writes: []storage.AWSIssueWrite{write}})
}

// sending is one pending alert being delivered in this pump.
type sending struct {
	issue string
	key   string
	alert Alert
	err   error
}

// deliver sends every pending alert whose send is due: alone, or with the
// rest of its digest. A send that succeeds takes the alert off the issue;
// one that fails is tried again after a backoff; one recorded more than
// GiveUpAfter ago is given up with one warning. A rejected issue's pending
// alerts are dropped unsent.
func (a *Alerter) deliver(ctx context.Context, byID map[string]Issue, now time.Time) error {
	var names map[string]string
	var due []*sending
	dropped := map[string]bool{}
	gaveUp := 0
	var gaveUpError string
	ids := make([]string, 0, len(byID))
	for id, issue := range byID {
		if issue.Alerted != nil && len(issue.Alerted.Pending) > 0 {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		issue := byID[id]
		if issue.State == StateRejected {
			dropped[id] = true
			continue
		}
		for _, entry := range issue.Alerted.Pending {
			switch {
			case now.Sub(entry.RecordedAt) >= GiveUpAfter:
				dropped[id] = true
				gaveUp++
				gaveUpError = cmpOr(entry.LastError, gaveUpError)
			case !entry.NextAt.After(now):
				if names == nil && a.names != nil {
					names = a.names(ctx)
				}
				alert := Alert{Kind: entry.Kind, Issue: issue, Detail: entry.Detail, Key: entry.Key, AccountName: names[issue.Account],
					Unread: entry.Unread, Group: entry.Group}
				due = append(due, &sending{issue: id, key: entry.Key, alert: alert})
			}
		}
	}
	due = a.claim(ctx, byID, due, now)
	var firstErr error
	groups := map[string][]*sending{}
	var order []string
	for _, item := range due {
		if item.alert.Group == "" {
			item.err = a.sink.Send(ctx, item.alert)
			firstErr = cmp.Or(firstErr, item.err)
			continue
		}
		if _, ok := groups[item.alert.Group]; !ok {
			order = append(order, item.alert.Group)
		}
		groups[item.alert.Group] = append(groups[item.alert.Group], item)
	}
	for _, group := range order {
		members := groups[group]
		alerts := make([]Alert, 0, len(members))
		for _, item := range members {
			alerts = append(alerts, item.alert)
		}
		sort.Slice(alerts, func(left, right int) bool { return alerts[left].Issue.ID < alerts[right].Issue.ID })
		err := a.sink.SendDigest(ctx, alerts)
		firstErr = cmp.Or(firstErr, err)
		for _, item := range members {
			item.err = err
		}
	}
	outcomes := map[string]map[string]error{}
	for _, item := range due {
		if outcomes[item.issue] == nil {
			outcomes[item.issue] = map[string]error{}
		}
		outcomes[item.issue][item.key] = item.err
	}
	for _, id := range ids {
		if outcomes[id] == nil && !dropped[id] {
			continue
		}
		a.settle(ctx, byID[id], outcomes[id], now)
	}
	if gaveUp > 0 {
		a.logf("aws issues: %d issue alerts given up after %s of failed sends (last error: %s); they will not be sent; check the alert channel settings and that their hosts are reachable",
			gaveUp, GiveUpAfter, cmpOr(gaveUpError, "unknown"))
	}
	if firstErr != nil {
		a.fail("send", fmt.Errorf("%w: %w", errAlertSend, firstErr), now)
		return firstErr
	}
	if len(due) > 0 {
		a.failures.clear("send")
	}
	return nil
}

// claim takes the due alerts of each issue for this pump before any is
// sent: it moves their next try a RetryAfter ahead under
// compare-and-swap. Only the writer that wins sends, so two pumps that
// see the same pending alert never both send it; a crash after the claim
// leaves it pending for the next try.
func (a *Alerter) claim(ctx context.Context, byID map[string]Issue, due []*sending, now time.Time) []*sending {
	keys := map[string]map[string]bool{}
	for _, item := range due {
		if keys[item.issue] == nil {
			keys[item.issue] = map[string]bool{}
		}
		keys[item.issue][item.key] = true
	}
	won := map[string]bool{}
	for id, claimed := range keys {
		before := byID[id]
		after := before
		mark := *before.Alerted
		mark.Pending = slices.Clone(before.Alerted.Pending)
		for index := range mark.Pending {
			if claimed[mark.Pending[index].Key] {
				mark.Pending[index].NextAt = now.Add(RetryAfter)
			}
		}
		after.Version, after.UpdatedAt = before.Version+1, now
		mark.Version = after.Version
		after.Alerted = &mark
		err := a.write(ctx, before, after, nil)
		if err != nil {
			if !errors.Is(err, storage.ErrAWSIssueConflict) {
				a.fail("record", fmt.Errorf("%w: %w", errAlertRecord, err), now)
			}
			continue
		}
		won[id] = true
		byID[id] = after
	}
	out := due[:0]
	for _, item := range due {
		if won[item.issue] {
			item.alert.Issue = byID[item.issue]
			out = append(out, item)
		}
	}
	return out
}

// settle writes the outcome of this pump's sends on an issue: delivered
// and given-up alerts leave it, a given-up one with an alert_given_up
// event in the same write, failed ones wait longer. A write that
// loses to another writer reads the issue again and applies the outcome
// to it, by key; if it keeps losing, the alerts stay pending and the
// alert path drops the repeat by its key.
func (a *Alerter) settle(ctx context.Context, issue Issue, outcomes map[string]error, now time.Time) {
	for range maxActionRetries {
		after := issue
		mark := *issue.Alerted
		mark.Pending = nil
		var events []Event
		for _, entry := range issue.Alerted.Pending {
			err, sent := outcomes[entry.Key]
			switch {
			case issue.State == StateRejected:
				continue
			case now.Sub(entry.RecordedAt) >= GiveUpAfter:
				// Given up: the issue says so in the same write that drops
				// the alert, so its events never claim it was sent.
				events = append(events, Event{IssueID: issue.ID, At: now, Kind: EventAlertGivenUp, Actor: EvaluatorActor, Detail: givenUpDetail(entry)})
				continue
			case sent && err == nil:
				continue
			case sent:
				entry.Attempts++
				entry.NextAt = now.Add(retryDelay(entry.Attempts))
				entry.LastError = cutText(utils.RedactSecrets(utils.SafeErrorText(err)), 256)
			}
			mark.Pending = append(mark.Pending, entry)
		}
		after.Version, after.UpdatedAt = issue.Version+1, now
		mark.Version = after.Version
		after.Alerted = &mark
		err := a.write(ctx, issue, after, events)
		if err == nil {
			return
		}
		if !errors.Is(err, storage.ErrAWSIssueConflict) {
			a.fail("record", fmt.Errorf("%w: %w", errAlertRecord, err), now)
			return
		}
		records, err := a.store.AWSIssues(ctx, a.scope, storage.AWSIssueQuery{IDs: []string{issue.ID}})
		if err != nil || len(records) != 1 {
			return
		}
		if issue, err = FromRecord(records[0]); err != nil || issue.Alerted == nil {
			return
		}
	}
}

// givenUpDetail says which alert was given up, after how many failed
// sends, and the last channel error, redacted.
func givenUpDetail(entry PendingAlert) string {
	detail := fmt.Sprintf("%s alert not sent: given up after %.0f hours and %d failed sends", entry.Kind, GiveUpAfter.Hours(), entry.Attempts)
	if entry.LastError != "" {
		detail += "; last error: " + cutText(utils.RedactSecrets(entry.LastError), 512)
	}
	return detail
}

// retryDelay is how long to wait after the given number of failed sends.
func retryDelay(attempts int) time.Duration {
	delay := RetryAfter
	for range attempts - 1 {
		if delay >= RetryAtMost {
			break
		}
		delay *= 2
	}
	return min(delay, RetryAtMost)
}

// cutText bounds text to limit bytes on a rune boundary.
func cutText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}

func (a *Alerter) list(ctx context.Context) ([]Issue, error) {
	records, err := a.store.AWSIssues(ctx, a.scope, storage.AWSIssueQuery{})
	if err != nil {
		return nil, err
	}
	out := make([]Issue, 0, len(records))
	for _, record := range records {
		issue, err := FromRecord(record)
		if err != nil {
			return nil, err
		}
		out = append(out, issue)
	}
	sortIssues(out)
	return out, nil
}

// Alert failure classes.
var (
	errAlertRead   = errors.New("issues could not be read")
	errAlertRecord = errors.New("alerts could not be recorded")
	errAlertSend   = errors.New("alerts could not be sent")
)

// fail logs an alert failure when it starts or its cause changes, then at
// most once per failureLogEvery while it continues.
func (a *Alerter) fail(key string, err error, now time.Time) {
	cause, line := alertFailureLine(err)
	a.failures.report(a.logf, "alerts "+key, cause, now, line)
}

// alertFailureLine is the default-level line for an alert failure: what
// failed, why, and what to check.
func alertFailureLine(err error) (string, string) {
	text := utils.RedactSecrets(utils.SafeErrorText(err))
	if errors.Is(err, errAlertSend) {
		return "alert channels failed", "aws issues: issue alerts not delivered (alert channels failed): " + text +
			"; each is sent again with a growing wait for 24 h; check the alert channel settings and that their hosts are reachable"
	}
	return "issue storage unavailable", "aws issues: issue alerts not recorded (issue storage unavailable): " + text +
		"; they are sent after the next evaluation; check the database connection"
}

// UnreadOf names the inputs of an issue's rule that are not read now, in
// the issue's Region (or the account, for an account-wide issue), with
// the gap that says why and the IAM actions that fix a denial.
func UnreadOf(view View, rule Rule, region string) []Unread {
	var out []Unread
	gaps := view.Gaps()
	for _, input := range rule.Inputs() {
		if view.ReadIn(input.Service, region) != ReadNotRead {
			continue
		}
		unread := Unread{Source: input.Service}
		for _, gap := range gaps {
			if gap.Service != input.Service || region != "" && gap.Region != region && gap.Region != model.GlobalRegion {
				continue
			}
			if gap.Outcome() != model.OutcomeNotRead {
				continue
			}
			unread.Operation, unread.Reason, unread.LastGoodAt = gap.Operation, gap.Reason, gap.LastGoodAt.UTC()
			break
		}
		if unread.Reason == model.GapDenied {
			unread.Fix = catalog.FixFor(unread.Source, unread.Operation)
		}
		out = append(out, unread)
	}
	return out
}

// NewUnreadFinder names an issue's inputs that are not read from the same
// views and rules the evaluator reads.
func NewUnreadFinder(source Source, rules *Registry) UnreadFinder {
	if rules == nil {
		rules = Default
	}
	return func(ctx context.Context, issue Issue) ([]Unread, error) {
		rule, ok := rules.Lookup(issue.Rule)
		if !ok {
			return nil, ErrUnknownRule
		}
		view, err := source.View(ctx, issue.Account)
		if err != nil {
			return nil, err
		}
		return UnreadOf(view, rule, issue.Region), nil
	}
}

// AlertWhere is the alert's "where" line: account, Region, owner and
// since.
func AlertWhere(alert Alert) string {
	issue := alert.Issue
	parts := []string{cmpOr(alert.AccountName, issue.Account)}
	if issue.Region != "" {
		parts = append(parts, issue.Region)
	}
	if owner := OwnerLabel(issue.Owner); owner != "" {
		parts = append(parts, owner)
	}
	if issue.SinceKnown && !issue.Since.IsZero() {
		parts = append(parts, "since "+issue.Since.UTC().Format("2006-01-02 15:04 UTC"))
	} else {
		parts = append(parts, "existed when first read")
	}
	return strings.Join(parts, " · ")
}

// OwnerLabel names an owner the way the pages do: the resource type and
// name of an ARN ("cluster ci-eks"), or the key itself.
func OwnerLabel(owner string) string {
	if !strings.HasPrefix(owner, "arn:") {
		return owner
	}
	parts := strings.SplitN(owner, ":", 6)
	if len(parts) < 6 {
		return owner
	}
	resource := parts[5]
	if kind, name, found := strings.Cut(resource, "/"); found {
		return kind + " " + name
	}
	if kind, name, found := strings.Cut(resource, ":"); found {
		return kind + " " + name
	}
	return resource
}

// EvidenceWord is how an evidence status reads on an alert and the
// report.
func EvidenceWord(status EvidenceStatus) string {
	switch status {
	case EvidenceProven:
		return "Proven"
	case EvidenceAssumed:
		return "Assumed"
	case EvidenceNotRead:
		return "Not read"
	}
	return "Assumed"
}

// ReasonWords say why a source is not read, for a coverage notice.
func ReasonWords(reason model.GapReason) string {
	switch reason {
	case model.GapDenied:
		return "permission removed"
	case model.GapThrottled:
		return "AWS throttled the reads"
	case model.GapNotOnPlan:
		return "not on the account's support plan"
	case "":
		return "not read"
	}
	return strings.ReplaceAll(string(reason), "_", " ")
}

func cmpOr(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
