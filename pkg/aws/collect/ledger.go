package collect

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/smithy-go/middleware"

	"github.com/VersusControl/versus-incident/pkg/aws/catalog"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// Every AWS request a collector makes is a CloudTrail management event in
// the customer's account, and features the customer chose (GuardDuty, a
// second trail, a log pipeline) price those events. The call count is the
// cost driver, so each unit run counts its requests into the call ledger
// and stops at its per-cycle budget.

// Sources group unit services for budgets and for the cost card.
const (
	SourceHealth           = "health"
	SourceInventory        = "inventory"
	SourceChanges          = "changes"
	SourceIdentity         = "identity"
	SourcePolicies         = "policies"
	SourceCredentialReport = "credential_report"
	SourceFindings         = "findings"
)

// Sources lists every budget source in display order. Credential report and
// findings have a budget before they have a collector.
func Sources() []string {
	return []string{SourceHealth, SourceInventory, SourceChanges, SourceIdentity, SourcePolicies, SourceCredentialReport, SourceFindings}
}

// Per-run budgets are sized from the per-item caps of the source's widest
// unit, so a unit within its caps always reads in full: the budget stops
// a runaway read, never a normal one.
const (
	// healthCallBudget covers the target groups whose health is read, the
	// stacks described and the ECS clusters, each at its cap.
	healthCallBudget = maxTargetGroupHealth + maxStacksDescribed + maxClusters
	// inventoryCallBudget covers the three detail calls of each bucket at
	// its cap, plus the list pages.
	inventoryCallBudget = 3*maxBucketDetails + 100
	// policiesCallBudget covers one policy call for each repository at
	// its cap, plus the list pages.
	policiesCallBudget = maxRepositories + 50
)

// DefaultCallBudgets are the calls one unit run of each source may make
// (pages for findings).
func DefaultCallBudgets() map[string]int {
	return map[string]int{
		SourceHealth:           healthCallBudget,
		SourceInventory:        inventoryCallBudget,
		SourceChanges:          60,
		SourceIdentity:         120,
		SourcePolicies:         policiesCallBudget,
		SourceCredentialReport: 3,
		SourceFindings:         50,
	}
}

// identityService is the unit that reads the account's IAM identities.
const identityService = "iam"

// SourceOf names the budget source a unit service belongs to.
func SourceOf(service string) string {
	switch {
	case service == snapshot.ChangeService:
		return SourceChanges
	case service == identityService:
		return SourceIdentity
	}
	switch KindOf(service) {
	case KindHealth:
		return SourceHealth
	case KindPolicy:
		return SourcePolicies
	}
	return SourceInventory
}

// CodeCapped is the error code of a unit whose run reached its call
// budget: the calls it did not make are named as a capped gap.
const CodeCapped = string(model.GapCapped)

// errCallBudget refuses a request the budget does not allow. The request
// is never sent.
var errCallBudget = errors.New(CodeCapped)

// listCeiling is how many times its budget a unit's own list and
// describe-all calls may run to before the run stops. Lists always run to
// the end below it, so the resource set is exact and a deletion shows;
// above it the estate is extreme and the unit keeps its last good read.
const listCeiling = 5

// rotation is where one per-item detail operation of a unit starts on its
// next run, and how many items its last run had.
type rotation struct {
	start, items int
}

// meter counts one unit run's requests and enforces its budget. It is
// shared by every client the run builds, retries included: each attempt
// is a request AWS records.
//
// The budget limits per-item detail calls only, the calls a collector
// marks with tried, and counts only them against it. List and
// describe-all calls are counted for the ledger and run to the end, up to
// listCeiling times the budget. A detail call is allowed while the budget
// lasts and its item is inside the run's window: the window starts where
// the last run stopped and is as wide as this run's budget allows for
// each detail operation, so every item is read within
// ceil(items / budget per operation) runs, and a budget raised to cover
// every item reads them all on the next run.
type meter struct {
	mu      sync.Mutex
	budget  int
	calls   int
	metered int
	// listCalls counts the calls that are not per-item detail calls, and
	// detailCalls the ones that are.
	listCalls   int
	detailCalls int
	// refused is how many requests the budget turned away, and stoppedAt
	// the first operation it turned away. ceiling is set when the lists
	// themselves ran past listCeiling times the budget.
	refused   int
	stoppedAt string
	ceiling   bool
	// detail is the operation the next request is a per-item detail call
	// of, set by tried; tried counts each operation's items, allowed the
	// ones read, and rotations are where each operation's window starts.
	detail    string
	tried     map[string]int
	allowed   map[string]int
	rotations map[string]rotation
	// skipped are the items, by operation, whose detail was not read.
	skipped map[string][]string
	item    string
}

func newMeter(budget int, rotations map[string]rotation) *meter {
	return &meter{budget: budget, rotations: rotations}
}

// mark says the next request is a per-item detail call of operation, for
// the resource arn (may be empty).
func (m *meter) mark(operation, arn string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.detail, m.item = operation, arn
}

// take admits one request or refuses it with errCallBudget.
func (m *meter) take(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	operation, item := m.detail, m.item
	m.detail, m.item = "", ""
	if operation == "" {
		if m.budget > 0 && m.listCalls >= listCeiling*m.budget {
			m.refuse(ctx)
			m.ceiling = true
			return errCallBudget
		}
		m.listCalls++
		m.count(ctx)
		return nil
	}
	if m.tried == nil {
		m.tried, m.allowed, m.skipped = map[string]int{}, map[string]int{}, map[string][]string{}
	}
	index := m.tried[operation]
	m.tried[operation]++
	if m.budget > 0 && (m.detailCalls >= m.budget || !m.inWindow(operation, index)) {
		m.refuse(ctx)
		if item != "" {
			m.skipped[operation] = append(m.skipped[operation], item)
		}
		return errCallBudget
	}
	m.allowed[operation]++
	m.detailCalls++
	m.count(ctx)
	return nil
}

// opsKey holds, in a unit's rotations, which detail operation the budget
// serves first when it is too small to serve each of them every run.
const opsKey = ""

// knownOperations are the detail operations the last runs made, sorted.
func (m *meter) knownOperations() []string {
	var out []string
	for operation := range m.rotations {
		if operation != opsKey {
			out = append(out, operation)
		}
	}
	sort.Strings(out)
	return out
}

// inWindow reports whether the index-th item of an operation is inside
// this run's window. With no last run to go by, every item is.
//
// The window is the budget shared by the operations the last runs made
// detail calls of: with S3's three calls a bucket and a budget of 30, ten
// buckets a run. A budget smaller than the number of operations serves
// that many operations a run, one item each, and moves on to the next
// operations on the next run, so every detail read is still made.
func (m *meter) inWindow(operation string, index int) bool {
	operations := m.knownOperations()
	if len(operations) > 0 && m.budget < len(operations) {
		position := slices.Index(operations, operation)
		if position >= 0 {
			offset := m.rotations[opsKey].start
			if ((position-offset)%len(operations)+len(operations))%len(operations) >= m.budget {
				return false
			}
		}
	}
	last, ok := m.rotations[operation]
	if !ok || last.items == 0 {
		return true
	}
	width := max(m.budget/max(len(operations), 1), 1)
	if width >= last.items {
		return true
	}
	offset := ((index-last.start)%last.items + last.items) % last.items
	return offset < width
}

func (m *meter) refuse(ctx context.Context) {
	if m.refused == 0 {
		m.stoppedAt = middleware.GetOperationName(ctx)
	}
	m.refused++
}

func (m *meter) count(ctx context.Context) {
	m.calls++
	if catalog.Metered(middleware.GetServiceID(ctx)) {
		m.metered++
	}
}

// counts returns the requests made, the metered part and whether any were
// refused.
func (m *meter) counts() (calls, metered int, capped bool, stoppedAt string) {
	if m == nil {
		return 0, 0, false, ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls, m.metered, m.refused > 0, m.stoppedAt
}

// overCeiling reports whether the unit's lists ran past the list ceiling.
func (m *meter) overCeiling() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ceiling
}

// nextRotations is where each detail operation starts on the next run:
// past the items this run read, or at the first item when it read them
// all. Every operation is kept, so the next run knows how many share its
// budget.
func (m *meter) nextRotations() map[string]rotation {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]rotation{}
	for operation, items := range m.tried {
		if items == 0 {
			continue
		}
		read := m.allowed[operation]
		start := 0
		if read < items {
			if last, ok := m.rotations[operation]; ok && last.items > 0 {
				start = last.start
			}
			start = (start + read) % items
		}
		out[operation] = rotation{start: start, items: items}
	}
	// An operation this run did not reach (its items come from a call
	// the budget skipped) is still one of the unit's operations.
	for operation, last := range m.rotations {
		if _, ok := out[operation]; !ok && operation != opsKey {
			out[operation] = last
		}
	}
	if operations := len(out); operations > 0 && m.budget > 0 && m.budget < operations {
		out[opsKey] = rotation{start: (m.rotations[opsKey].start + m.budget) % operations}
	}
	return out
}

// skippedItems are the resources whose detail the budget skipped, by
// operation.
func (m *meter) skippedItems() map[string][]string {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string][]string, len(m.skipped))
	for operation, items := range m.skipped {
		out[operation] = append([]string(nil), items...)
	}
	return out
}

// budgetGap names a unit run whose own lists ran past the list ceiling:
// Seen is the calls it made, and nothing says how many more were needed.
func (c *call) budgetGap() model.Gap {
	calls, _, _, operation := c.env.meter.counts()
	return model.Gap{Account: c.unit.Account, Region: c.unit.Region, Service: c.unit.Service, Reason: model.GapCapped,
		Operation: operation, Seen: calls}
}

// notReadSources are the health sources a per-item detail operation reads.
// An item the budget skipped gets a not-read signal from that source, so a
// reader sees "not read", never healthy and never the old signal. Every
// other signal comes from a list (alarms, instance status, stacks, RDS
// events), which always runs to the end.
var notReadSources = map[string]string{
	"DescribeTargetHealth": model.SourceTargetHealth,
}

// notRead are the not-read signals of the items whose detail the budget
// skipped.
func (c *call) notRead() []model.Health {
	var out []model.Health
	for operation, items := range c.env.meter.skippedItems() {
		source, ok := notReadSources[operation]
		if !ok {
			continue
		}
		for _, arn := range items {
			out = append(out, c.healthItem(arn, source, model.HealthUnknown, "Not read: over the call budget for this cycle", unknownSince, ""))
		}
	}
	return out
}

// callBudget is the per-run budget of a unit service; zero means none.
func callBudget(options Options, service string) int {
	if options.CallBudgets == nil {
		return 0
	}
	return max(options.CallBudgets[SourceOf(service)], 0)
}

// unitBudget is the hard request cap of one unit run. The change poller
// stops at its own page budget, which the change source's budget already
// bounds, and finishes its windows cleanly there, so it gets no hard cap.
func unitBudget(options Options, service string) int {
	if service == snapshot.ChangeService {
		return 0
	}
	return callBudget(options, service)
}

// recordCalls writes one unit run's requests to the call ledger, once. A
// write that fails keeps its counts in memory and adds them to the next
// write, so an outage of the ledger loses no calls; LedgerFailedSince
// says when the ledger may be short meanwhile.
func (collector *Collector) recordCalls(ctx context.Context, c *call) {
	if collector.ledger == nil {
		return
	}
	calls, metered, _, _ := c.env.meter.counts()
	if calls == 0 {
		return
	}
	count := storage.AWSCallCount{AccountID: c.unit.Account, Region: c.unit.Region, Source: c.unit.Service,
		Hour: c.now.Truncate(time.Hour), Calls: int64(calls), Metered: int64(metered), Runs: 1, Peak: int64(calls)}
	collector.ledgerMu.Lock()
	defer collector.ledgerMu.Unlock()
	collector.holdLocked(count)
	collector.flushLocked(ctx)
}

// maxHeldCounts bounds the counts kept while the ledger cannot be written:
// one per unit and hour, so days of a ledger outage.
const maxHeldCounts = 20000

// holdLocked adds a count to the unsent ones, by row. The caller holds
// ledgerMu.
func (collector *Collector) holdLocked(count storage.AWSCallCount) {
	key := storage.AWSCallCountKey{AccountID: count.AccountID, Region: count.Region, Source: count.Source, Hour: count.Hour}
	held, ok := collector.held[key]
	if !ok && len(collector.held) >= maxHeldCounts {
		collector.ledgerFailedAt = collector.now()
		return
	}
	held.AccountID, held.Region, held.Source, held.Hour = count.AccountID, count.Region, count.Source, count.Hour
	held.Calls += count.Calls
	held.Metered += count.Metered
	held.Runs += count.Runs
	held.Peak = max(held.Peak, count.Peak)
	if collector.held == nil {
		collector.held = map[storage.AWSCallCountKey]storage.AWSCallCount{}
	}
	collector.held[key] = held
}

// flushLocked writes every unsent count at once. The caller holds
// ledgerMu.
func (collector *Collector) flushLocked(ctx context.Context) error {
	if len(collector.held) == 0 {
		return nil
	}
	counts := make([]storage.AWSCallCount, 0, len(collector.held))
	for _, count := range collector.held {
		counts = append(counts, count)
	}
	if err := collector.ledger(ctx, counts); err != nil {
		collector.ledgerFailedAt = collector.now()
		return err
	}
	clear(collector.held)
	return nil
}

// FlushLedger writes the counts a failed ledger write kept, best effort:
// the scheduler calls it when it stops.
func (collector *Collector) FlushLedger(ctx context.Context) error {
	if collector == nil || collector.ledger == nil {
		return nil
	}
	collector.ledgerMu.Lock()
	defer collector.ledgerMu.Unlock()
	return collector.flushLocked(ctx)
}

// LedgerFailedSince reports whether a ledger write failed at or after
// since, or counts are still unsent: the ledger may be short for that
// stretch.
func (collector *Collector) LedgerFailedSince(since time.Time) bool {
	if collector == nil {
		return false
	}
	collector.ledgerMu.Lock()
	defer collector.ledgerMu.Unlock()
	return len(collector.held) > 0 || !collector.ledgerFailedAt.IsZero() && !collector.ledgerFailedAt.Before(since)
}

// WithCallLedger receives the counts of the unit runs that made requests,
// each unit's run added to its hour's row; a write that fails is retried
// with the next one.
func WithCallLedger(record func(context.Context, []storage.AWSCallCount) error) Option {
	return func(collector *Collector) { collector.ledger = record }
}

// rotationsOf returns where a unit's detail reads start on this run.
func (collector *Collector) rotationsOf(unit model.Unit) map[string]rotation {
	collector.rotationMu.Lock()
	defer collector.rotationMu.Unlock()
	return collector.rotations[unit.Key()]
}

// keepRotations records where a unit's detail reads start on its next run.
func (collector *Collector) keepRotations(unit model.Unit, next map[string]rotation) {
	collector.rotationMu.Lock()
	defer collector.rotationMu.Unlock()
	if collector.rotations == nil {
		collector.rotations = map[string]map[string]rotation{}
	}
	if len(next) == 0 {
		delete(collector.rotations, unit.Key())
		return
	}
	collector.rotations[unit.Key()] = next
}

// markNotRead names, on every resource whose detail the budget skipped,
// the detail reads it lacks. A bucket whose policy was skipped gets a
// policy entry that says so, so the graph shows "policy not read" rather
// than "no grant".
func (c *call) markNotRead(out *result) {
	skipped := c.env.meter.skippedItems()
	if len(skipped) == 0 {
		return
	}
	byARN := map[string][]string{}
	for operation, items := range skipped {
		for _, arn := range items {
			byARN[arn] = append(byARN[arn], operation)
		}
	}
	found := map[string]bool{}
	for index := range out.resources {
		resource := &out.resources[index]
		operations, ok := byARN[resource.ARN]
		if !ok {
			continue
		}
		found[resource.ARN] = true
		if resource.Attributes == nil {
			resource.Attributes = map[string]string{}
		}
		resource.Attributes[model.AttrNotRead] = joinOperations(model.NotReadOperations(*resource), operations)
	}
	if c.unit.Service != model.PolicyEntryService {
		return
	}
	for arn, operations := range byARN {
		if found[arn] {
			continue
		}
		name := arn[strings.LastIndex(arn, ":")+1:]
		entry := c.newResource(arn+"#policy", model.ResourcePolicyType, name, "")
		entry.Attributes = map[string]string{model.AttrSubject: arn, model.AttrNotRead: joinOperations(nil, operations)}
		out.resources = append(out.resources, entry)
	}
}

func joinOperations(have, add []string) string {
	seen := map[string]bool{}
	var out []string
	for _, operation := range append(append([]string(nil), have...), add...) {
		if operation != "" && !seen[operation] {
			seen[operation] = true
			out = append(out, operation)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// linkReads are the detail reads that say which resources belong together:
// an application's or a stack's members. Grouping places resources by
// them, so a skipped one keeps the members its resource last read, still
// marked not read, and nothing moves group because a read was skipped.
// Health, flags and grants are never carried over.
var linkReads = map[string]bool{"ListAssociatedResources": true, "ListStackResources": true}

// keepLinks gives each resource whose link read the budget skipped the
// links it had when last read.
func (collector *Collector) keepLinks(ctx context.Context, c *call, out *result) {
	for index := range out.resources {
		resource := &out.resources[index]
		skipped := false
		for _, operation := range model.NotReadOperations(*resource) {
			skipped = skipped || linkReads[operation]
		}
		if !skipped || hasRelationType(resource.Relations, model.RelationAssociated) {
			continue
		}
		last, _, found, err := collector.store.Resource(ctx, resource.ARN, time.Time{})
		if err != nil || !found {
			continue
		}
		for _, relation := range last.Relations {
			if relation.Type == model.RelationAssociated {
				resource.Relations = append(resource.Relations, relation)
			}
		}
	}
}

func hasRelationType(relations []model.Relation, kind model.RelationType) bool {
	for _, relation := range relations {
		if relation.Type == kind {
			return true
		}
	}
	return false
}

// keepIdentity gives a resource the budget kept from being described the
// ARN it was last read under, when the list alone cannot name it (a node
// group's ARN ends in an ID only its describe returns): the same name in
// the same parent is the same resource, so a skipped describe never reads
// as one resource removed and another added.
func (collector *Collector) keepIdentity(ctx context.Context, c *call, out *result) {
	var stored []model.Resource
	loaded := false
	for index := range out.resources {
		resource := &out.resources[index]
		if len(model.NotReadOperations(*resource)) == 0 || resource.Type != "node-group" {
			continue
		}
		if !loaded {
			result, err := collector.store.Resources(ctx, snapshot.Query{Account: c.unit.Account, Region: c.unit.Region, Service: c.unit.Service, Type: resource.Type})
			if err != nil {
				return
			}
			stored, loaded = result.Resources, true
		}
		for _, last := range stored {
			if last.Name == resource.Name && sameParent(last.Relations, resource.Relations) {
				resource.ARN = last.ARN
				break
			}
		}
	}
}

func sameParent(left, right []model.Relation) bool {
	for _, a := range left {
		for _, b := range right {
			if a.Type == model.RelationMemberOf && b.Type == model.RelationMemberOf && a.Target == b.Target {
				return true
			}
		}
	}
	return false
}
