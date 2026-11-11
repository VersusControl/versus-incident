// Package snapshot is the committed store for AWS collector output. Each unit
// (account × region × service) holds a history of generations in the
// storage inventory tables; a sync builds a candidate, validates it and swaps
// it in with one transaction, so a failed, partial or throttled sync never
// blanks or zeros what readers see. Readers query by as-of time and always
// receive the named gaps beside the data.
package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// ChangeService is the unit service that tracks CloudTrail polling. It has a
// sync state and change coverage but no inventory generations.
const ChangeService = "cloudtrail"

// Defaults for the bounded store. A retention sweep stops after
// DefaultRetentionSweepBudget or storage.DefaultAWSRetentionBatch rows and
// resumes after the next sync.
const (
	DefaultMaxGenerations       = 48
	DefaultMaxAge               = 7 * 24 * time.Hour
	DefaultChangeRetention      = 14 * 24 * time.Hour
	DefaultRetentionSweepBudget = time.Second
	// maxResourcesPerUnit rejects a candidate that is implausibly large for
	// one unit.
	maxResourcesPerUnit = 200000
)

var (
	// ErrNoStorage means the store's provider has no inventory tables, so
	// the inventory is not ready.
	ErrNoStorage = errors.New("aws snapshot storage unavailable")
	// ErrInvalidUnit means an account, region or service identifier is
	// malformed.
	ErrInvalidUnit = errors.New("invalid aws snapshot unit")
	// ErrInvalidCandidate means a candidate failed validation and was not
	// committed.
	ErrInvalidCandidate = errors.New("invalid aws snapshot candidate")
	// ErrPurged means the account or region was purged after the writer
	// started; the write was dropped so a purged account cannot come back.
	ErrPurged = errors.New("aws snapshot account purged")
)

var (
	accountPattern = regexp.MustCompile(`^[0-9]{12}$`)
	regionPattern  = regexp.MustCompile(`^([a-z]{2}(-gov|-iso[a-z]*)?-[a-z]+-[0-9]|global)$`)
	servicePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)
)

// ValidUnit reports whether a unit's identifiers are safe to use in a key.
func ValidUnit(unit model.Unit) bool {
	return accountPattern.MatchString(unit.Account) && regionPattern.MatchString(unit.Region) && servicePattern.MatchString(unit.Service)
}

// Options bounds the history and names the application sources.
type Options struct {
	MaxGenerations  int
	MaxAge          time.Duration
	ChangeRetention time.Duration
	// AppSources is the resolver order; any of appregistry, cloudformation,
	// tags. Empty means all three in that order.
	AppSources []string
}

// Candidate is one sync's output for a unit. DetailGaps name parts of the
// unit (one call per bucket, per target group, ...) that could not be read
// while the unit's list itself succeeded.
type Candidate struct {
	Resources  []model.Resource
	Health     []model.Health
	Flags      []model.SecurityFlag
	DetailGaps []model.Gap
}

// Generation is one committed, immutable snapshot of a unit.
type Generation struct {
	Seq         uint64               `json:"seq"`
	Unit        model.Unit           `json:"unit"`
	CommittedAt time.Time            `json:"committed_at"`
	Digest      string               `json:"digest"`
	Resources   []model.Resource     `json:"resources"`
	Health      []model.Health       `json:"health,omitempty"`
	Flags       []model.SecurityFlag `json:"flags,omitempty"`
	DetailGaps  []model.Gap          `json:"detail_gaps,omitempty"`
}

// Store persists snapshots in the provider's inventory tables under the
// deployment's one write organization and reads across the scope's read
// organizations.
type Store struct {
	// allowlist reads the live tag-value allowlist; nil when not wired.
	allowlist func() []string
	provider  storage.Provider
	inventory storage.AWSInventoryStore
	scope     tenancy.OrgScope
	bounded   bool
	now       func() time.Time
	options   func() Options

	sweepMu     sync.Mutex
	sweep       storage.AWSRetentionCursor
	sweepBudget time.Duration
	sweepBatch  int

	mu        sync.Mutex
	views     map[string]cachedView
	viewOrder []string
}

// Option adjusts a Store.
type Option func(*Store)

// WithClock injects the clock used for commit times and pruning.
func WithClock(now func() time.Time) Option {
	return func(store *Store) {
		if now != nil {
			store.now = now
		}
	}
}

// WithOptions wires a live reader for the history bounds and app sources.
func WithOptions(read func() Options) Option {
	return func(store *Store) {
		if read != nil {
			store.options = read
		}
	}
}

// WithTagAllowlist wires the live tag-value allowlist. Every read applies
// it again (model.RedactName), so a name or tag value stored while the
// allowlist was wider is hidden as soon as it narrows. Without it reads
// serve what the collectors stored, already redacted at write.
func WithTagAllowlist(read func() []string) Option {
	return func(store *Store) { store.allowlist = read }
}

// WithUnbounded turns retention off: every generation and change is kept.
// It is meant for tests and archival deployments, not for the default.
func WithUnbounded() Option {
	return func(store *Store) { store.bounded = false }
}

// WithRetentionSweep bounds one retention pass. A pass stops at budget or
// after batch rows and resumes on the next sync, so the configured retention
// never turns into one long delete. Zero values keep the defaults; a negative
// budget removes the time limit.
func WithRetentionSweep(budget time.Duration, batch int) Option {
	return func(store *Store) {
		store.sweepBudget = budget
		store.sweepBatch = batch
	}
}

// New builds a store over provider. Every backend keeps the configured
// history and change retention: providers with a bounded sweep (the SQL
// tables behind Postgres and the file index) delete expired rows in small
// batches, and memory providers prune inline. A provider without inventory
// tables answers ErrNoStorage.
func New(provider storage.Provider, scope tenancy.OrgScope, options ...Option) *Store {
	inventory, _ := storage.AWSInventory(provider)
	store := &Store{
		provider:    provider,
		inventory:   inventory,
		scope:       scope.Normalized(),
		bounded:     true,
		now:         time.Now,
		options:     func() Options { return Options{} },
		sweepBudget: DefaultRetentionSweepBudget,
		sweepBatch:  storage.DefaultAWSRetentionBatch,
		views:       make(map[string]cachedView),
	}
	for _, option := range options {
		option(store)
	}
	return store
}

// Scope reports the tenancy scope the store is pinned to.
func (store *Store) Scope() tenancy.OrgScope { return store.scope }

// writeScope reads and writes only the write organization.
func (store *Store) writeScope() tenancy.OrgScope { return tenancy.NewOrgScope(store.scope.Write) }

func (store *Store) ready() bool { return store != nil && store.inventory != nil }

func (store *Store) effectiveOptions() Options {
	options := store.options()
	if options.MaxGenerations <= 0 {
		options.MaxGenerations = DefaultMaxGenerations
	}
	if options.MaxAge <= 0 {
		options.MaxAge = DefaultMaxAge
	}
	if options.ChangeRetention <= 0 {
		options.ChangeRetention = DefaultChangeRetention
	}
	return options
}

func pairKey(account, region string) string { return account + "/" + region }

// invalidateViews drops the cached views of one account.
func (store *Store) invalidateViews(account string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	kept := store.viewOrder[:0]
	for _, key := range store.viewOrder {
		if len(key) > len(account) && key[:len(account)+1] == account+"|" {
			delete(store.views, key)
			continue
		}
		kept = append(kept, key)
	}
	store.viewOrder = kept
}

// Commit validates a candidate and makes it the unit's current snapshot in
// one transaction. An identical candidate only refreshes the verification
// time.
func (store *Store) Commit(unit model.Unit, candidate Candidate) (Generation, error) {
	return store.commit(unit, candidate, nil)
}

func (store *Store) commit(unit model.Unit, candidate Candidate, guard *uint64) (Generation, error) {
	if !ValidUnit(unit) || unit.Service == ChangeService {
		return Generation{}, ErrInvalidUnit
	}
	if !store.ready() {
		return Generation{}, ErrNoStorage
	}
	generation, err := normalizeCandidate(unit, candidate)
	if err != nil {
		return Generation{}, err
	}
	now := store.now().UTC()
	rows := storage.AWSCandidate{At: now, Digest: generation.Digest, Gaps: encodeGaps(generation.DetailGaps)}
	for _, resource := range generation.Resources {
		rows.Resources = append(rows.Resources, resourceRow(resource))
	}
	for _, item := range generation.Health {
		rows.Health = append(rows.Health, healthRow(item))
	}
	for _, flag := range generation.Flags {
		rows.Flags = append(rows.Flags, flagRow(flag))
	}
	ctx := context.Background()
	committed, err := store.inventory.CommitAWSUnit(ctx, store.scope, storageUnit(unit), rows, guard)
	if err != nil {
		return Generation{}, storeError(err)
	}
	generation.Seq, generation.CommittedAt = committed.Generation.Seq, committed.Generation.CommittedAt
	if store.bounded && !committed.Unchanged {
		store.retain(ctx, unit)
	}
	store.invalidateViews(unit.Account)
	return generation, nil
}

// retain runs one bounded retention pass without ever failing the write it
// follows: a retention failure leaves more history, never less. A pass that
// is already running is skipped; it resumes on the next sync.
func (store *Store) retain(ctx context.Context, unit model.Unit) {
	if !store.sweepMu.TryLock() {
		return
	}
	defer store.sweepMu.Unlock()
	store.sweepOnce(ctx, unit)
}

// SweepRetention runs one bounded retention pass and returns what it
// removed. Sync paths call it after a commit; a driver may also run it on a
// timer, repeating it while More is true.
func (store *Store) SweepRetention(ctx context.Context) (storage.AWSRetentionResult, error) {
	if store == nil || !store.bounded || !store.ready() {
		return storage.AWSRetentionResult{}, nil
	}
	store.sweepMu.Lock()
	defer store.sweepMu.Unlock()
	return store.sweepOnce(ctx, model.Unit{})
}

// sweepOnce selects the provider's batched sweep when it has one, and prunes
// inline otherwise. The caller holds sweepMu.
func (store *Store) sweepOnce(ctx context.Context, unit model.Unit) (storage.AWSRetentionResult, error) {
	options := store.effectiveOptions()
	now := store.clock()
	if sweeper, ok := store.inventory.(storage.AWSRetentionSweeper); ok {
		sweep := storage.AWSRetentionSweep{
			Scope:           store.scope,
			HistoryBefore:   now.Add(-options.MaxAge),
			ChangeBefore:    now.Add(-options.ChangeRetention),
			KeepGenerations: options.MaxGenerations,
			Batch:           store.sweepBatch,
			Cursor:          store.sweep,
		}
		budget := store.sweepBudget
		if budget >= 0 {
			if budget == 0 {
				budget = DefaultRetentionSweepBudget
			}
			sweep.Deadline = time.Now().Add(budget)
		}
		result, err := sweeper.SweepAWSRetention(ctx, sweep)
		if err != nil {
			log.Printf("aws retention sweep failed, history kept: %v", err)
			return result, err
		}
		store.sweep = result.Cursor
		logRetention(result)
		return result, nil
	}
	result := storage.AWSRetentionResult{Cursor: storage.AWSRetentionCursor{}}
	if unit.Service != "" {
		removed, err := store.inventory.PruneAWSUnitHistory(ctx, store.scope, storageUnit(unit), options.MaxGenerations)
		if err != nil {
			log.Printf("aws retention unit prune failed, history kept: %v", err)
			return result, err
		}
		result.Generations = removed
		result.Units = 1
	}
	removed, err := store.inventory.PruneAWSHistory(ctx, store.scope, now.Add(-options.MaxAge))
	if err != nil {
		log.Printf("aws retention history prune failed, history kept: %v", err)
		return result, err
	}
	result.History = removed
	if unit.Service == ChangeService || unit.Service == "" {
		removed, err = store.inventory.PruneAWSChanges(ctx, store.scope, now.Add(-options.ChangeRetention))
		if err != nil {
			log.Printf("aws retention change prune failed, history kept: %v", err)
			return result, err
		}
		result.Changes = removed
	}
	logRetention(result)
	return result, nil
}

// logRetention reports what one pass removed, so an operator can see the
// sweep keeping up or falling behind.
func logRetention(result storage.AWSRetentionResult) {
	if result.Generations == 0 && result.History == 0 && result.Changes == 0 {
		return
	}
	log.Printf("aws retention: removed %d generations, %d history rows and %d changes across %d units; more pending: %t",
		result.Generations, result.History, result.Changes, result.Units, result.More)
}

// updateSync applies change to the unit's sync state under the store's row
// lock.
func (store *Store) updateSync(unit model.Unit, guard *uint64, change func(state *storage.AWSSyncState, found bool) bool) error {
	if !ValidUnit(unit) {
		return ErrInvalidUnit
	}
	if !store.ready() {
		return ErrNoStorage
	}
	err := store.inventory.UpdateAWSSync(context.Background(), store.scope, storageUnit(unit), guard, change)
	if err == nil {
		store.invalidateViews(unit.Account)
	}
	return storeError(err)
}

// RecordFailure marks a unit's last attempt as failed with a closed error
// code. Its committed generations are untouched and keep serving reads. The
// gaps name what is missing; without them the whole unit is one gap. Every
// gap carries the time the unit last committed good data.
func (store *Store) RecordFailure(unit model.Unit, code string, gaps ...model.Gap) error {
	return store.recordFailure(unit, code, gaps, nil)
}

func (store *Store) recordFailure(unit model.Unit, code string, gaps []model.Gap, guard *uint64) error {
	now := store.clock()
	return store.updateSync(unit, guard, func(state *storage.AWSSyncState, _ bool) bool {
		state.State = string(model.SyncFailed)
		state.AttemptedAt = now
		state.ErrorCode = code
		named := make([]model.Gap, 0, len(gaps)+1)
		for _, gap := range gaps {
			gap.Account, gap.Region, gap.Service = unit.Account, unit.Region, unit.Service
			gap.LastGoodAt = state.LastGoodAt
			named = append(named, gap)
		}
		if len(named) == 0 {
			named = append(named, model.Gap{
				Account: unit.Account, Region: unit.Region, Service: unit.Service,
				Reason: model.GapReasonForCode(code), LastGoodAt: state.LastGoodAt,
			})
		}
		state.Gaps = encodeGaps(named)
		return true
	})
}

func (store *Store) clock() time.Time {
	if store == nil {
		return time.Time{}
	}
	return store.now().UTC()
}

// HasGeneration reports whether the write organization holds a committed
// generation for unit.
func (store *Store) HasGeneration(unit model.Unit) bool {
	if !store.ready() {
		return false
	}
	units, err := store.inventory.AWSUnits(context.Background(), store.writeScope(), storage.AWSUnitQuery{Account: unit.Account, Region: unit.Region, Service: unit.Service})
	return err == nil && len(units) == 1 && units[0].Generation != nil
}

// RecordSuccess marks a unit without generations (the change poller) as
// read successfully, with any detail gaps.
func (store *Store) RecordSuccess(unit model.Unit, detailGaps []model.Gap) error {
	return store.recordSuccess(unit, detailGaps, nil)
}

func (store *Store) recordSuccess(unit model.Unit, detailGaps []model.Gap, guard *uint64) error {
	now := store.clock()
	return store.updateSync(unit, guard, func(state *storage.AWSSyncState, _ bool) bool {
		state.State, state.LastGoodAt, state.AttemptedAt, state.ErrorCode = string(model.SyncOK), now, now, ""
		state.Gaps = encodeGaps(detailGaps)
		return true
	})
}

// RecordPending registers units a sync is expected to produce, so a reader
// sees them as not checked rather than absent. Units that already have a
// state are left alone.
func (store *Store) RecordPending(units []model.Unit) error {
	return store.recordPending(units, nil)
}

func (store *Store) recordPending(units []model.Unit, guard *uint64) error {
	if !store.ready() {
		return ErrNoStorage
	}
	for _, unit := range units {
		if !ValidUnit(unit) {
			return ErrInvalidUnit
		}
	}
	sorted := append([]model.Unit(nil), units...)
	sort.Slice(sorted, func(left, right int) bool { return sorted[left].Key() < sorted[right].Key() })
	pending := make([]storage.AWSUnit, len(sorted))
	for index, unit := range sorted {
		pending[index] = storageUnit(unit)
	}
	// The whole plan is registered in one store transaction, so a reader
	// during a first sync never sees the total grow unit by unit.
	if err := store.inventory.RegisterAWSPending(context.Background(), store.scope, pending, guard); err != nil {
		return storeError(err)
	}
	seen := make(map[string]bool, len(sorted))
	for _, unit := range sorted {
		if seen[unit.Account] {
			continue
		}
		seen[unit.Account] = true
		store.invalidateViews(unit.Account)
	}
	return nil
}

func normalizeCandidate(unit model.Unit, candidate Candidate) (Generation, error) {
	if len(candidate.Resources) > maxResourcesPerUnit {
		return Generation{}, ErrInvalidCandidate
	}
	resources := make([]model.Resource, 0, len(candidate.Resources))
	seen := make(map[string]bool, len(candidate.Resources))
	for _, resource := range candidate.Resources {
		if resource.ARN == "" || resource.Account != unit.Account || resource.Service == "" || resource.Type == "" {
			return Generation{}, ErrInvalidCandidate
		}
		if seen[resource.ARN] {
			continue
		}
		seen[resource.ARN] = true
		resources = append(resources, model.Sanitize(resource))
	}
	sort.Slice(resources, func(left, right int) bool { return resources[left].ARN < resources[right].ARN })

	health := make([]model.Health, 0, len(candidate.Health))
	for _, item := range candidate.Health {
		if item.Account != unit.Account || item.Source == "" || item.Status == "" {
			return Generation{}, ErrInvalidCandidate
		}
		health = append(health, item)
	}
	sort.Slice(health, func(left, right int) bool {
		if health[left].Resource != health[right].Resource {
			return health[left].Resource < health[right].Resource
		}
		if health[left].Source != health[right].Source {
			return health[left].Source < health[right].Source
		}
		return health[left].Ref < health[right].Ref
	})

	flags := make([]model.SecurityFlag, 0, len(candidate.Flags))
	for _, flag := range candidate.Flags {
		if flag.Account != unit.Account || flag.Resource == "" || flag.Kind == "" {
			return Generation{}, ErrInvalidCandidate
		}
		flags = append(flags, flag)
	}
	sort.Slice(flags, func(left, right int) bool {
		if flags[left].Resource != flags[right].Resource {
			return flags[left].Resource < flags[right].Resource
		}
		return flags[left].Kind < flags[right].Kind
	})

	gaps := append([]model.Gap(nil), candidate.DetailGaps...)
	sort.Slice(gaps, func(left, right int) bool { return gaps[left].Operation < gaps[right].Operation })

	generation := Generation{Unit: unit, Resources: resources, Health: health, Flags: flags, DetailGaps: gaps}
	digest, err := digestOf(generation)
	if err != nil {
		return Generation{}, ErrInvalidCandidate
	}
	generation.Digest = digest
	return generation, nil
}

func digestOf(generation Generation) (string, error) {
	payload, err := json.Marshal(struct {
		Resources []model.Resource     `json:"r"`
		Health    []model.Health       `json:"h"`
		Flags     []model.SecurityFlag `json:"f"`
		Gaps      []model.Gap          `json:"g"`
	}{generation.Resources, generation.Health, generation.Flags, generation.DetailGaps})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}
