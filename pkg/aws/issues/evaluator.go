package issues

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/logging"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
	"github.com/VersusControl/versus-incident/pkg/utils"
)

// DefaultMinInterval is the least time between two evaluations of one
// process.
const DefaultMinInterval = time.Minute

// maxConflictRetries bounds how often one account is evaluated again after
// a user action changed one of its issues mid-evaluation.
const maxConflictRetries = 5

// failureLogEvery is how often a failure that continues is logged again.
const failureLogEvery = 10 * time.Minute

// Evaluator errors.
var (
	// ErrLockLost means the database ended the session that held the
	// replica lock during an evaluation; the evaluation stopped writing.
	ErrLockLost = errors.New("issues: the replica lock was lost")
	// ErrConnectedUnknown is a connected-accounts reader that has no list
	// to give (the connector is off). Nothing is resolved as disconnected,
	// and nothing is logged.
	ErrConnectedUnknown = errors.New("issues: the connected accounts are not known")
	// ErrNoStore is an evaluator over a provider without the issue tables.
	ErrNoStore = errors.New("issues: storage has no issue tables")
)

// AccountResult is what one evaluation wrote for one account.
type AccountResult struct {
	Account string
	// Baselines are the rules first assessed in this evaluation, per
	// Region.
	Baselines []storage.AWSIssueBaseline
	Changes   []Change
	// Dropped counts issues the store would refuse (a rule's oversize
	// title, a malformed Region); each is left as it was.
	Dropped     int
	NotAssessed []NotAssessed
	// Disconnected is set when the account is no longer connected and
	// its issues were resolved.
	Disconnected bool
}

// Report is what one evaluation did. Skipped is set when another replica
// held the evaluation lock.
type Report struct {
	At       time.Time
	Skipped  bool
	Accounts []AccountResult
	Pruned   int64
}

// Evaluator runs the rules over every account and writes the issues. One
// replica evaluates at a time, under a session lock where the backend has
// one; memory and file storage serve one process and always evaluate.
// Every account is one transaction, and every issue is written under
// compare-and-swap, so a user's rejection that lands mid-evaluation wins:
// the account is read again and evaluated over it.
type Evaluator struct {
	provider  storage.Provider
	store     storage.AWSIssueStore
	scope     tenancy.OrgScope
	source    Source
	rules     *Registry
	now       func() time.Time
	logf      func(format string, args ...any)
	interval  time.Duration
	retention time.Duration
	after     []func(ctx context.Context, report Report)
	connected func(ctx context.Context) ([]ConnectedAccount, error)
	debugf    func(format string, args ...any)
	// wait is the pace seam of the trigger loop.
	wait func(ctx context.Context, d time.Duration) bool
	// beforeWrite runs before each account's batch is written; tests use it
	// to land a user action mid-evaluation.
	beforeWrite func(ctx context.Context, account string)

	mu      sync.Mutex
	lastRun time.Time

	trigger  chan struct{}
	stopped  chan struct{}
	once     sync.Once
	failures failureLog
}

// Option adjusts an Evaluator.
type Option func(*Evaluator)

// WithClock injects the clock.
func WithClock(now func() time.Time) Option {
	return func(e *Evaluator) {
		if now != nil {
			e.now = now
		}
	}
}

// WithLogger sets the log function for failures a user can act on.
func WithLogger(logf func(string, ...any)) Option {
	return func(e *Evaluator) {
		if logf != nil {
			e.logf = logf
		}
	}
}

// WithRules sets the rules; the default is Default.
func WithRules(rules *Registry) Option {
	return func(e *Evaluator) {
		if rules != nil {
			e.rules = rules
		}
	}
}

// WithMinInterval sets the least time between two evaluations.
func WithMinInterval(interval time.Duration) Option {
	return func(e *Evaluator) {
		if interval >= 0 {
			e.interval = interval
		}
	}
}

// WithRetention sets how long resolved issues and their events are kept.
func WithRetention(retention time.Duration) Option {
	return func(e *Evaluator) {
		if retention > 0 {
			e.retention = retention
		}
	}
}

// WithAfterEvaluate runs after every evaluation that wrote, with what it
// wrote. The alert pump and the likely-cause lookup hang here.
func WithAfterEvaluate(after func(ctx context.Context, report Report)) Option {
	return func(e *Evaluator) {
		if after != nil {
			e.after = append(e.after, after)
		}
	}
}

// ConnectedAccount is one account the connector configuration holds, with
// the Regions it reads. Nil Regions are not known, and no Region is then
// treated as removed.
type ConnectedAccount struct {
	Account string
	Regions []string
}

// WithConnectedAccounts reads the accounts and Regions the connector
// configuration holds. On a list read without error, an account that is not
// on it has its open issues resolved as no longer connected and its
// baselines reset, and a Region an account no longer reads is treated the
// same way; rejections are kept. A failed read changes nothing.
func WithConnectedAccounts(connected func(ctx context.Context) ([]ConnectedAccount, error)) Option {
	return func(e *Evaluator) { e.connected = connected }
}

// WithDebug sets the log function for detail: issues dropped because the
// store refuses them.
func WithDebug(debugf func(string, ...any)) Option {
	return func(e *Evaluator) {
		if debugf != nil {
			e.debugf = debugf
		}
	}
}

// NewEvaluator builds an evaluator over provider's issue store. It fails
// with ErrNoStore when the provider has no issue tables.
func NewEvaluator(provider storage.Provider, scope tenancy.OrgScope, source Source, options ...Option) (*Evaluator, error) {
	store, ok := storage.AWSIssuesOf(provider)
	if !ok {
		return nil, ErrNoStore
	}
	e := &Evaluator{
		provider: provider, store: store, scope: scope.Normalized(), source: source, rules: Default,
		now: time.Now, logf: func(string, ...any) {}, debugf: logging.Debugf, interval: DefaultMinInterval, retention: storage.DefaultAWSIssueRetention,
		wait: waitFor, trigger: make(chan struct{}, 1), stopped: make(chan struct{}),
	}
	for _, option := range options {
		option(e)
	}
	return e, nil
}

// Store is the issue store the evaluator writes.
func (e *Evaluator) Store() storage.AWSIssueStore { return e.store }

// Scope is the organization scope the evaluator reads and writes.
func (e *Evaluator) Scope() tenancy.OrgScope { return e.scope }

// Trigger asks for an evaluation. It never blocks; triggers that arrive
// while one is pending fold into it.
func (e *Evaluator) Trigger() {
	if e == nil {
		return
	}
	select {
	case e.trigger <- struct{}{}:
	default:
	}
}

// Due reports whether an evaluation was asked for and has not started.
func (e *Evaluator) Due() bool { return e != nil && len(e.trigger) > 0 }

// Start evaluates once, then after each trigger, at most once per
// interval, until ctx ends. Stopped closes when it has returned.
func (e *Evaluator) Start(ctx context.Context) {
	e.once.Do(func() {
		e.Trigger()
		go func() {
			defer close(e.stopped)
			for {
				select {
				case <-ctx.Done():
					return
				case <-e.trigger:
				}
				if wait := e.untilDue(); wait > 0 && !e.wait(ctx, wait) {
					return
				}
				_, _ = e.Evaluate(ctx)
			}
		}()
	})
}

// Stopped closes when the loop started by Start has returned.
func (e *Evaluator) Stopped() <-chan struct{} { return e.stopped }

// untilDue is how long the next evaluation must wait to keep the interval.
func (e *Evaluator) untilDue() time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lastRun.IsZero() {
		return 0
	}
	return e.interval - e.now().Sub(e.lastRun)
}

func waitFor(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// lockKey is the session lock key of the deployment's evaluation.
func (e *Evaluator) lockKey() int64 {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte("aws-issues\x00" + e.scope.Write))
	return int64(hash.Sum64() & 0x7fffffffffffffff)
}

// lock takes the replica lock. A backend without session locks serves one
// process and always grants it. check answers ErrLockLost once the lock is
// no longer held.
func (e *Evaluator) lock(ctx context.Context) (unlock func(), check func(context.Context) error, acquired bool, err error) {
	none := func(context.Context) error { return nil }
	locker, ok := e.provider.(storage.SessionLocker)
	if !ok {
		return func() {}, none, true, nil
	}
	unlock, acquired, err = locker.TryLockSession(ctx, e.lockKey())
	if err != nil || !acquired {
		return nil, nil, false, err
	}
	verifier, ok := e.provider.(storage.SessionLockVerifier)
	if !ok {
		return unlock, none, true, nil
	}
	key := e.lockKey()
	return unlock, func(ctx context.Context) error {
		held, err := verifier.SessionLockHeld(ctx, key)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			return fmt.Errorf("%w: %v", ErrLockLost, err)
		}
		if !held {
			return ErrLockLost
		}
		return nil
	}, true, nil
}

// Evaluate runs every rule over every account once, under the replica
// lock. It is what Start runs; a caller may also run it directly.
func (e *Evaluator) Evaluate(ctx context.Context) (Report, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	// This evaluation reads after every trigger that came before it, so
	// those need no evaluation of their own.
	select {
	case <-e.trigger:
	default:
	}
	now := e.now().UTC()
	report := Report{At: now}
	// A replica that finds the lock held waits the interval too.
	e.lastRun = now
	unlock, check, acquired, err := e.lock(ctx)
	if err != nil {
		e.failures.fail(e, "lock", err, now)
		return report, err
	}
	e.failures.clear("lock")
	if !acquired {
		report.Skipped = true
		return report, nil
	}
	defer unlock()
	accounts, err := e.source.Accounts(ctx)
	if err != nil {
		err = fmt.Errorf("%w: %w", errInventory, err)
		e.failures.fail(e, "accounts", err, now)
		return report, err
	}
	e.failures.clear("accounts")
	baselines, err := e.store.AWSIssueBaselines(ctx, e.scope, "")
	if err != nil {
		e.failures.fail(e, "accounts", err, now)
		return report, err
	}
	known := baselinesByAccount(baselines)
	var firstErr error
	scope, err := e.connectedScope(ctx, accounts, known)
	switch {
	case errors.Is(err, ErrConnectedUnknown):
		e.failures.clear("connected accounts")
	case err != nil:
		// The configuration could not be read: evaluate what the store
		// holds and resolve nothing as removed.
		e.failures.fail(e, "connected accounts", fmt.Errorf("%w: %w", errConnected, err), now)
		firstErr = err
	default:
		e.failures.clear("connected accounts")
	}
	stop := func(err error) bool {
		return errors.Is(err, ErrLockLost) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
	}
	for _, account := range scope.gone {
		result, err := e.disconnect(ctx, account, now, check)
		if stop(err) {
			return report, err
		}
		if err != nil {
			e.failures.fail(e, "account "+account, err, now)
			firstErr = cmp.Or(firstErr, err)
			continue
		}
		e.failures.clear("account " + account)
		if len(result.Changes) > 0 {
			report.Accounts = append(report.Accounts, result)
		}
	}
	for _, account := range scope.evaluate {
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
		result, err := e.evaluateAccount(ctx, account, scope.regions[account], known[account], now, check)
		if stop(err) {
			// Another replica may hold the lock now; stop without writing.
			return report, err
		}
		if err != nil {
			e.failures.fail(e, "account "+account, err, now)
			firstErr = cmp.Or(firstErr, err)
			continue
		}
		e.failures.clear("account " + account)
		report.Accounts = append(report.Accounts, result)
	}
	if err := check(ctx); err != nil {
		return report, err
	}
	pruned, err := e.store.PruneAWSIssues(ctx, e.scope, now.Add(-e.retention))
	if err != nil {
		firstErr = cmp.Or(firstErr, err)
	}
	report.Pruned = pruned
	for _, after := range e.after {
		after(ctx, report)
	}
	return report, firstErr
}

// baselines are one account's baselines and resets.
type baselines struct {
	taken  map[storage.AWSIssueBaselineKey]time.Time
	resets map[string]time.Time
}

func baselinesByAccount(rows []storage.AWSIssueBaseline) map[string]*baselines {
	out := map[string]*baselines{}
	for _, row := range rows {
		account := out[row.AccountID]
		if account == nil {
			account = &baselines{taken: map[storage.AWSIssueBaselineKey]time.Time{}, resets: map[string]time.Time{}}
			out[row.AccountID] = account
		}
		if row.Rule == "" {
			account.resets[row.Region] = row.FirstAssessedAt
			continue
		}
		account.taken[row.Key()] = row.FirstAssessedAt
	}
	return out
}

// valid reports whether a baseline was taken for key and no reset of the
// account, or of its Region, came after it.
func (b *baselines) valid(key storage.AWSIssueBaselineKey) bool {
	if b == nil {
		return false
	}
	at, ok := b.taken[key]
	if !ok || !at.After(b.resets[""]) {
		return false
	}
	return key.Region == "" || at.After(b.resets[key.Region])
}

// regionsWithBaselines lists the Regions that hold a baseline still in
// force.
func (b *baselines) regionsWithBaselines() map[string]bool {
	out := map[string]bool{}
	if b == nil {
		return out
	}
	for key := range b.taken {
		if b.valid(key) {
			out[key.Region] = true
		}
	}
	return out
}

// connectedScope is what the connector configuration says to evaluate:
// the accounts to evaluate with the Regions each reads (nil when not
// known), and the accounts no longer connected that still hold an open
// issue or a baseline in force.
type connectedScope struct {
	evaluate []string
	regions  map[string]map[string]bool
	gone     []string
}

// connectedScope reads the configuration. Without a reader, or when it
// fails, every account the store holds is evaluated and nothing is
// removed.
func (e *Evaluator) connectedScope(ctx context.Context, accounts []string, known map[string]*baselines) (connectedScope, error) {
	scope := connectedScope{evaluate: accounts, regions: map[string]map[string]bool{}}
	if e.connected == nil {
		return scope, nil
	}
	connected, err := e.connected(ctx)
	if err != nil {
		return scope, err
	}
	keep := map[string]bool{}
	for _, account := range connected {
		keep[account.Account] = true
		if account.Regions != nil {
			regions := map[string]bool{}
			for _, region := range account.Regions {
				regions[region] = true
			}
			scope.regions[account.Account] = regions
		}
	}
	records, err := e.store.AWSIssues(ctx, e.scope, storage.AWSIssueQuery{})
	if err != nil {
		return scope, err
	}
	scope.evaluate = scope.evaluate[:0:0]
	for _, account := range accounts {
		if keep[account] {
			scope.evaluate = append(scope.evaluate, account)
		}
	}
	gone := map[string]bool{}
	for _, record := range records {
		if !keep[record.AccountID] && State(record.State).Active() {
			gone[record.AccountID] = true
		}
	}
	for account, held := range known {
		if !keep[account] && len(held.regionsWithBaselines()) > 0 {
			gone[account] = true
		}
	}
	for account := range gone {
		scope.gone = append(scope.gone, account)
	}
	sort.Strings(scope.gone)
	return scope, nil
}

// readIssues reads an account's stored issues.
func (e *Evaluator) readIssues(ctx context.Context, account string) ([]Issue, error) {
	records, err := e.store.AWSIssues(ctx, e.scope, storage.AWSIssueQuery{AccountID: account})
	if err != nil {
		return nil, err
	}
	stored := make([]Issue, 0, len(records))
	for _, record := range records {
		issue, err := FromRecord(record)
		if err != nil {
			return nil, err
		}
		stored = append(stored, issue)
	}
	return stored, nil
}

// disconnect resolves the issues of an account that is no longer
// connected and resets its baselines, in one transaction, reading again on
// a conflict. Rejections are kept.
func (e *Evaluator) disconnect(ctx context.Context, account string, now time.Time, check func(context.Context) error) (AccountResult, error) {
	reset := []storage.AWSIssueBaseline{{AccountID: account, FirstAssessedAt: now}}
	for attempt := 0; ; attempt++ {
		stored, err := e.readIssues(ctx, account)
		if err != nil {
			return AccountResult{}, err
		}
		changes := Disconnect(account, stored, now)
		batch, kept, _, err := e.batchOf(account, changes, reset)
		if err != nil {
			return AccountResult{}, err
		}
		err = e.write(ctx, batch, check)
		if errors.Is(err, storage.ErrAWSIssueConflict) && attempt < maxConflictRetries {
			continue
		}
		if err != nil {
			return AccountResult{}, err
		}
		return AccountResult{Account: account, Changes: kept, Disconnected: true}, nil
	}
}

// write writes one batch unless the evaluation was cancelled or lost its
// lock.
func (e *Evaluator) write(ctx context.Context, batch storage.AWSIssueBatch, check func(context.Context) error) error {
	if e.beforeWrite != nil {
		e.beforeWrite(ctx, batch.AccountID)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := check(ctx); err != nil {
		return err
	}
	return e.store.WriteAWSIssues(ctx, e.scope, batch)
}

// removedRegions lists the Regions an account no longer reads that still
// hold an issue or a baseline in force. Nil configured Regions remove
// nothing.
func removedRegions(configured map[string]bool, issues []Issue, held *baselines) map[string]bool {
	removed := map[string]bool{}
	if configured == nil {
		return removed
	}
	consider := func(region string) {
		if region != "" && region != model.GlobalRegion && !configured[region] {
			removed[region] = true
		}
	}
	for _, issue := range issues {
		consider(issue.Region)
	}
	for region := range held.regionsWithBaselines() {
		consider(region)
	}
	return removed
}

// evaluateAccount evaluates one account and writes it in one transaction,
// reading the account again when an issue changed since it was read.
func (e *Evaluator) evaluateAccount(ctx context.Context, account string, configured map[string]bool, held *baselines, now time.Time, check func(context.Context) error) (AccountResult, error) {
	view, err := e.source.View(ctx, account)
	if err != nil {
		return AccountResult{}, fmt.Errorf("%w: %w", errInventory, err)
	}
	for attempt := 0; ; attempt++ {
		issues, err := e.readIssues(ctx, account)
		if err != nil {
			return AccountResult{}, err
		}
		removed := removedRegions(configured, issues, held)
		outcomes, taken := e.outcomes(view, held, removed, now)
		result := Advance(Cycle{Account: account, Now: now, Removed: removed, Issues: issues, Outcomes: outcomes})
		resets := make([]storage.AWSIssueBaseline, 0, len(removed))
		for region := range held.regionsWithBaselines() {
			if removed[region] {
				resets = append(resets, storage.AWSIssueBaseline{AccountID: account, Region: region, FirstAssessedAt: now})
			}
		}
		batch, kept, dropped, err := e.batchOf(account, result.Changes, append(taken, resets...))
		if err != nil {
			return AccountResult{}, err
		}
		err = e.write(ctx, batch, check)
		if errors.Is(err, storage.ErrAWSIssueConflict) && attempt < maxConflictRetries {
			continue
		}
		if err != nil {
			return AccountResult{}, err
		}
		e.reportDrops(account, dropped, now)
		var baselined []storage.AWSIssueBaseline
		for _, baseline := range batch.Baselines {
			if baseline.Rule != "" {
				baselined = append(baselined, baseline)
			}
		}
		return AccountResult{Account: account, Baselines: baselined, Changes: kept, Dropped: len(result.Changes) - len(kept),
			NotAssessed: result.NotAssessed}, nil
	}
}

// outcomes runs every rule over the view and assesses it for the whole
// account and in each Region the view read or a finding names, except the
// Regions no longer read. A rule is run even where it cannot be assessed:
// Advance opens and resolves only where it can. A place is a baseline
// when it is assessed and no baseline in force is held for it; those are
// returned to be written.
func (e *Evaluator) outcomes(view View, held *baselines, removed map[string]bool, now time.Time) ([]RuleOutcome, []storage.AWSIssueBaseline) {
	rules := e.rules.Rules()
	out := make([]RuleOutcome, 0, len(rules))
	var taken []storage.AWSIssueBaseline
	first := func(stable, region string, assessment Assessment) bool {
		key := storage.AWSIssueBaselineKey{AccountID: view.Account(), Rule: stable, Region: region}
		if assessment == AssessNotAssessed || held.valid(key) {
			return false
		}
		taken = append(taken, storage.AWSIssueBaseline{AccountID: view.Account(), Rule: stable, Region: region, FirstAssessedAt: now})
		return true
	}
	for _, rule := range rules {
		stable := e.rules.StableID(rule.ID())
		assessment, missing := Assess(rule, view)
		outcome := RuleOutcome{
			Rule: rule.ID(), StableID: stable, Names: e.rules.names(rule.ID()),
			Kind: rule.Kind(), Assessment: assessment, Missing: missing, Regions: map[string]Assessed{},
			Findings: rule.Evaluate(view),
		}
		outcome.Baseline = first(stable, "", assessment)
		var regions []string
		for _, region := range view.Regions() {
			if !removed[region] {
				regions = append(regions, region)
			}
		}
		for _, finding := range outcome.Findings {
			if finding.Region != "" && !removed[finding.Region] && !slices.Contains(regions, finding.Region) {
				regions = append(regions, finding.Region)
			}
		}
		sort.Strings(regions)
		for _, region := range regions {
			assessed, missing := AssessIn(rule, view, region)
			outcome.Regions[region] = Assessed{Assessment: assessed, Missing: missing, Baseline: first(stable, region, assessed)}
		}
		out = append(out, outcome)
	}
	return out, taken
}

// drop is one issue left out of a batch because the store refuses it.
type drop struct {
	rule, owner, why string
}

// batchOf encodes one account's changes as one transaction. A change the
// store would refuse is dropped, its issue left as it was, so one bad
// finding never stalls the rest of the account. No baseline is written for
// the rule and Region of a dropped change, so a finding that shrinks into
// the store later still reads as existed when first read.
func (e *Evaluator) batchOf(account string, changes []Change, baselines []storage.AWSIssueBaseline) (storage.AWSIssueBatch, []Change, []drop, error) {
	batch := storage.AWSIssueBatch{AccountID: account}
	kept := make([]Change, 0, len(changes))
	var dropped []drop
	held := map[storage.AWSIssueBaselineKey]bool{}
	for _, change := range changes {
		write, err := writeOf(change)
		if err == nil {
			err = storage.CheckAWSIssueRecord(write.Issue)
		}
		if errors.Is(err, storage.ErrInvalidAWSIssue) {
			why := strings.TrimPrefix(err.Error(), storage.ErrInvalidAWSIssue.Error()+": ")
			e.debugf("aws issues: account %s: rule %s on %s left out: %s", account, change.Issue.Rule, change.Issue.Owner, why)
			dropped = append(dropped, drop{rule: change.Issue.Rule, owner: change.Issue.Owner, why: why})
			held[storage.AWSIssueBaselineKey{AccountID: account, Rule: e.rules.StableID(change.Issue.Rule), Region: change.Issue.Region}] = true
			continue
		}
		if err != nil {
			return storage.AWSIssueBatch{}, nil, nil, err
		}
		batch.Writes = append(batch.Writes, write)
		kept = append(kept, change)
	}
	for _, baseline := range baselines {
		if baseline.Rule != "" && held[baseline.Key()] {
			continue
		}
		batch.Baselines = append(batch.Baselines, baseline)
	}
	return batch, kept, dropped, nil
}

// reportDrops warns, per account and rule, about issues left out because
// the store refuses them, throttled like any continuing failure, and
// clears the warning of a rule that no longer drops any.
func (e *Evaluator) reportDrops(account string, dropped []drop, now time.Time) {
	byRule := map[string][]drop{}
	for _, one := range dropped {
		byRule[one.rule] = append(byRule[one.rule], one)
	}
	for _, rule := range e.rules.Rules() {
		key := "account " + account + " rule " + rule.ID()
		drops := byRule[rule.ID()]
		if len(drops) == 0 {
			e.failures.clear(key)
			continue
		}
		e.failures.report(e.logf, key, "dropped "+drops[0].why, now, fmt.Sprintf(
			"aws issues: account %s, rule %s: %d findings left out (%s); the rest of the account is up to date; check the rule's evidence size and fields, LOG_LEVEL=debug names each owner",
			account, rule.ID(), len(drops), drops[0].why))
	}
}

func writeOf(change Change) (storage.AWSIssueWrite, error) {
	record, err := Record(change.Issue)
	if err != nil {
		return storage.AWSIssueWrite{}, err
	}
	write := storage.AWSIssueWrite{Issue: record, Events: eventRecords(change.Events)}
	if change.Before != nil {
		write.Read, write.ReadOrg = change.Before.Version, change.Before.Org
	}
	return write, nil
}

// failureLog keeps a continuing failure to one line when it starts or its
// cause changes, then one line per failureLogEvery with a count.
type failureLog struct {
	mu   sync.Mutex
	seen map[string]*failureState
}

type failureState struct {
	cause   string
	since   time.Time
	logged  time.Time
	repeats int
}

// errInventory marks a failure to read the snapshot store.
var errInventory = errors.New("inventory unreadable")

// errConnected marks a failure to read the connector's account list.
var errConnected = errors.New("connector account list unreadable")

// failureLine is the default-level line for a failure: what failed, the
// cause class, the error, and what to check.
func failureLine(key string, err error) (cause, line string) {
	text := utils.RedactSecrets(utils.SafeErrorText(err))
	for _, marker := range []error{errInventory, errConnected} {
		text = strings.TrimPrefix(text, marker.Error()+": ")
	}
	switch {
	case errors.Is(err, errConnected):
		return "connector account list unreadable", "aws issues: connector account list unreadable: " + text +
			"; evaluation continues, but accounts and Regions removed from the connector are not resolved until it is read; check the AWS connector settings and their storage"
	case errors.Is(err, storage.ErrAWSIssueConflict):
		cause = "issues kept changing during the evaluation"
		return cause, "aws issues: cannot evaluate " + key + " (" + cause + "): " + text +
			"; issues keep their last state; it is retried on the next evaluation; check for a script or replica writing issues in a loop"
	case errors.Is(err, storage.ErrInvalidAWSIssue):
		cause = "the store refused the issue data"
		return cause, "aws issues: cannot evaluate " + key + " (" + cause + "): " + text +
			"; issues keep their last state; report it with the rule named in the debug log (LOG_LEVEL=debug)"
	case errors.Is(err, errInventory):
		cause = "inventory unreadable"
		return cause, "aws issues: cannot evaluate " + key + " (" + cause + "): " + text +
			"; issues keep their last state; check the AWS inventory storage"
	case errors.Is(err, ErrLockLost):
		cause = "replica lock lost"
		return cause, "aws issues: cannot evaluate " + key + " (" + cause + "): " + text +
			"; issues keep their last state; check the database connection"
	}
	cause = "storage unavailable"
	return cause, "aws issues: cannot evaluate " + key + " (" + cause + "): " + text +
		"; issues keep their last state; check the database connection and the storage settings"
}

// fail reports a failure of one step under key.
func (log *failureLog) fail(e *Evaluator, key string, err error, now time.Time) {
	cause, line := failureLine(key, err)
	log.report(e.logf, key, cause, now, line)
}

// report logs line when the failure under key starts or its cause
// changes, then at most once per failureLogEvery while it continues, with
// a count and the same text, so the repeat keeps the error and what to
// check.
func (log *failureLog) report(logf func(string, ...any), key, cause string, now time.Time, line string) {
	log.mu.Lock()
	defer log.mu.Unlock()
	if log.seen == nil {
		log.seen = map[string]*failureState{}
	}
	state, ok := log.seen[key]
	switch {
	case !ok || state.cause != cause:
		log.seen[key] = &failureState{cause: cause, since: now, logged: now, repeats: 1}
		logf("%s", line)
	case now.Sub(state.logged) >= failureLogEvery:
		state.repeats++
		state.logged = now
		logf("%s (still failing, %d times since %s)", line, state.repeats, state.since.Format(time.RFC3339))
	default:
		state.repeats++
	}
}

// clear forgets the failure under key once its step succeeds, so the next
// failure logs at once.
func (log *failureLog) clear(key string) {
	log.mu.Lock()
	defer log.mu.Unlock()
	delete(log.seen, key)
}
