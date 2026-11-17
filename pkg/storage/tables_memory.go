package storage

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// memoryTables implements AWSInventoryStore and FinOpsStore in process with
// the same contract as the SQL tables: the same keys, temporal rows,
// organization precedence, ordering and aggregation.
type memoryTables struct {
	mu        sync.RWMutex
	epochs    map[string]uint64
	units     map[memoryUnitKey]*memoryUnit
	changes   map[string]map[string]AWSChange
	coverage  map[memoryPairKey][]AWSInterval
	lines     map[string]map[finOpsLineKey]FinOpsCostLine
	daily     map[string]map[finOpsDailyRowKey]finOpsDailyRow
	increases map[string]map[string]FinOpsIncrease
	reports   map[string]map[string]FinOpsReport
	decisions map[string]map[string]FinOpsSavingDecision
}

type memoryUnitKey struct {
	org  string
	unit AWSUnit
}

type memoryPairKey struct {
	org, account, region string
}

type memoryVersion[T any] struct {
	row    T
	digest string
	from   time.Time
	to     time.Time
	closed bool
}

func (version memoryVersion[T]) at(asOf time.Time) bool {
	if asOf.IsZero() {
		return !version.closed
	}
	return !version.from.After(asOf) && (!version.closed || version.to.After(asOf))
}

type memoryUnit struct {
	sync        AWSSyncState
	generations []memoryVersion[AWSGeneration]
	resources   []memoryVersion[AWSResource]
	health      []memoryVersion[AWSHealth]
	flags       []memoryVersion[AWSSecurityFlag]
}

func newMemoryTables() *memoryTables {
	return &memoryTables{
		epochs:    map[string]uint64{},
		units:     map[memoryUnitKey]*memoryUnit{},
		changes:   map[string]map[string]AWSChange{},
		coverage:  map[memoryPairKey][]AWSInterval{},
		lines:     map[string]map[finOpsLineKey]FinOpsCostLine{},
		daily:     map[string]map[finOpsDailyRowKey]finOpsDailyRow{},
		increases: map[string]map[string]FinOpsIncrease{},
		reports:   map[string]map[string]FinOpsReport{},
		decisions: map[string]map[string]FinOpsSavingDecision{},
	}
}

// firstHolder reports whether org is the first organization, in read order,
// for which holds reports true. The first read organization always is.
func firstHolder(orgs []string, org string, holds func(string) bool) bool {
	for _, candidate := range orgs {
		if candidate == org {
			return true
		}
		if holds(candidate) {
			return false
		}
	}
	return false
}

func epochKey(org, account string) string { return org + "\x00" + account }

func (m *memoryTables) checkGuard(org, account string, guard *uint64) error {
	if guard != nil && m.epochs[epochKey(org, account)] != *guard {
		return ErrAWSInventoryFenced
	}
	return nil
}

func (m *memoryTables) AWSAccountEpoch(_ context.Context, scope tenancy.OrgScope, account string) (uint64, error) {
	if !validTableKey(account) {
		return 0, ErrInvalidAWSInventory
	}
	scope = inventoryScope(scope)
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.epochs[epochKey(scope.Write, account)], nil
}

func swapMemoryRows[T any](versions []memoryVersion[T], rows []T, digests []string, at time.Time) []memoryVersion[T] {
	wanted := make(map[string]bool, len(digests))
	for _, digest := range digests {
		wanted[digest] = true
	}
	existing := map[string]bool{}
	for index := range versions {
		if versions[index].closed {
			continue
		}
		if wanted[versions[index].digest] {
			existing[versions[index].digest] = true
			continue
		}
		versions[index].closed, versions[index].to = true, at
	}
	for index, row := range rows {
		if !existing[digests[index]] {
			versions = append(versions, memoryVersion[T]{row: row, digest: digests[index], from: at})
		}
	}
	return versions
}

func (m *memoryTables) CommitAWSUnit(_ context.Context, scope tenancy.OrgScope, unit AWSUnit, candidate AWSCandidate, guard *uint64) (AWSCommit, error) {
	rows, err := prepareAWSCommit(unit, candidate)
	if err != nil {
		return AWSCommit{}, err
	}
	scope = inventoryScope(scope)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.checkGuard(scope.Write, unit.Account, guard); err != nil {
		return AWSCommit{}, err
	}
	key := memoryUnitKey{org: scope.Write, unit: unit}
	entry := m.units[key]
	if entry == nil {
		entry = &memoryUnit{}
		m.units[key] = entry
	}
	commitState := func(seq uint64) {
		entry.sync = AWSSyncState{State: AWSSyncOK, Generation: seq, LastGoodAt: rows.at, AttemptedAt: rows.at, VerifiedAt: rows.at, Gaps: cloneTableJSON(rows.gaps)}
	}
	current := -1
	for index := range entry.generations {
		if !entry.generations[index].closed {
			current = index
		}
	}
	if current >= 0 && entry.generations[current].row.Digest == candidate.Digest {
		entry.generations[current].row.VerifiedAt = rows.at
		commitState(entry.generations[current].row.Seq)
		generation := entry.generations[current].row
		generation.Gaps = cloneTableJSON(generation.Gaps)
		return AWSCommit{Generation: generation, Unchanged: true}, nil
	}
	seq := entry.sync.Generation
	if current >= 0 {
		if entry.generations[current].row.Seq > seq {
			seq = entry.generations[current].row.Seq
		}
		entry.generations[current].closed, entry.generations[current].to = true, rows.at
	}
	seq++
	generation := AWSGeneration{Seq: seq, Digest: candidate.Digest, CommittedAt: rows.at, VerifiedAt: rows.at, Count: len(rows.resources), Gaps: cloneTableJSON(rows.gaps)}
	entry.generations = append(entry.generations, memoryVersion[AWSGeneration]{row: generation, from: rows.at})
	entry.resources = swapMemoryRows(entry.resources, rows.resources, rows.digests[0], rows.at)
	entry.health = swapMemoryRows(entry.health, rows.health, rows.digests[1], rows.at)
	entry.flags = swapMemoryRows(entry.flags, rows.flags, rows.digests[2], rows.at)
	commitState(seq)
	generation.Gaps = cloneTableJSON(generation.Gaps)
	return AWSCommit{Generation: generation}, nil
}

func (m *memoryTables) UpdateAWSSync(_ context.Context, scope tenancy.OrgScope, unit AWSUnit, guard *uint64, update func(state *AWSSyncState, found bool) bool) error {
	if !validAWSUnit(unit) || update == nil {
		return ErrInvalidAWSInventory
	}
	scope = inventoryScope(scope)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.checkGuard(scope.Write, unit.Account, guard); err != nil {
		return err
	}
	key := memoryUnitKey{org: scope.Write, unit: unit}
	entry := m.units[key]
	var state AWSSyncState
	if entry != nil {
		state = entry.sync
		state.Gaps = cloneTableJSON(state.Gaps)
	}
	if !update(&state, entry != nil) {
		return nil
	}
	if !validTableStrings(state.State, state.ErrorCode) || !validTableJSON(state.Gaps) {
		return ErrInvalidAWSInventory
	}
	if entry == nil {
		entry = &memoryUnit{}
		m.units[key] = entry
	}
	state.Generation = entry.sync.Generation
	state.LastGoodAt, state.AttemptedAt, state.VerifiedAt = tableTime(state.LastGoodAt), tableTime(state.AttemptedAt), tableTime(state.VerifiedAt)
	state.Gaps = cloneTableJSON(state.Gaps)
	entry.sync = state
	return nil
}

func (m *memoryTables) AppendAWSChanges(_ context.Context, scope tenancy.OrgScope, account, region string, changes []AWSChange, covered []AWSInterval, guard *uint64) error {
	if !validTableKey(account) || !validTableKey(region) {
		return ErrInvalidAWSInventory
	}
	cleaned := make([]AWSChange, 0, len(changes))
	for _, change := range changes {
		row, err := cleanAWSChange(account, region, change)
		if err != nil {
			return err
		}
		cleaned = append(cleaned, row)
	}
	scope = inventoryScope(scope)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.checkGuard(scope.Write, account, guard); err != nil {
		return err
	}
	stored := m.changes[scope.Write]
	if stored == nil {
		stored = map[string]AWSChange{}
		m.changes[scope.Write] = stored
	}
	for _, change := range cleaned {
		stored[change.EventID] = change
	}
	if len(covered) > 0 {
		key := memoryPairKey{scope.Write, account, region}
		merged := mergeAWSIntervals(append(append([]AWSInterval(nil), m.coverage[key]...), covered...))
		if len(merged) == 0 {
			delete(m.coverage, key)
		} else {
			m.coverage[key] = merged
		}
	}
	return nil
}

func (m *memoryTables) PurgeAWSAccount(_ context.Context, scope tenancy.OrgScope, account, region string) error {
	if !validTableKey(account) || region != "" && !validTableKey(region) {
		return ErrInvalidAWSInventory
	}
	scope = inventoryScope(scope)
	org := scope.Write
	m.mu.Lock()
	defer m.mu.Unlock()
	m.epochs[epochKey(org, account)]++
	matches := func(rowAccount, rowRegion string) bool {
		return rowAccount == account && (region == "" || rowRegion == region)
	}
	for key := range m.units {
		if key.org == org && matches(key.unit.Account, key.unit.Region) {
			delete(m.units, key)
		}
	}
	for id, change := range m.changes[org] {
		if matches(change.Account, change.Region) {
			delete(m.changes[org], id)
		}
	}
	for key := range m.coverage {
		if key.org == org && matches(key.account, key.region) {
			delete(m.coverage, key)
		}
	}
	return nil
}

func pruneMemoryVersions[T any](versions []memoryVersion[T], before time.Time) ([]memoryVersion[T], int64) {
	kept := versions[:0]
	var removed int64
	for _, version := range versions {
		if version.closed && version.to.Before(before) {
			removed++
			continue
		}
		kept = append(kept, version)
	}
	return kept, removed
}

func (m *memoryTables) PruneAWSHistory(_ context.Context, scope tenancy.OrgScope, before time.Time) (int64, error) {
	if before.IsZero() {
		return 0, ErrInvalidAWSInventory
	}
	scope = inventoryScope(scope)
	before = tableTime(before)
	m.mu.Lock()
	defer m.mu.Unlock()
	var total, removed int64
	for key, entry := range m.units {
		if key.org != scope.Write {
			continue
		}
		entry.resources, removed = pruneMemoryVersions(entry.resources, before)
		total += removed
		entry.health, removed = pruneMemoryVersions(entry.health, before)
		total += removed
		entry.flags, removed = pruneMemoryVersions(entry.flags, before)
		total += removed
		entry.generations, removed = pruneMemoryVersions(entry.generations, before)
		total += removed
	}
	return total, nil
}

func (m *memoryTables) PruneAWSUnitHistory(_ context.Context, scope tenancy.OrgScope, unit AWSUnit, keep int) (int64, error) {
	if !validAWSUnit(unit) || keep < 1 {
		return 0, ErrInvalidAWSInventory
	}
	scope = inventoryScope(scope)
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.units[memoryUnitKey{org: scope.Write, unit: unit}]
	if entry == nil || len(entry.generations) < keep {
		return 0, nil
	}
	seqs := make([]uint64, 0, len(entry.generations))
	for _, version := range entry.generations {
		seqs = append(seqs, version.row.Seq)
	}
	sort.Slice(seqs, func(left, right int) bool { return seqs[left] > seqs[right] })
	kept := seqs[keep-1]
	var oldest time.Time
	generations := entry.generations[:0]
	var total int64
	for _, version := range entry.generations {
		if version.row.Seq == kept {
			oldest = version.from
		}
		if version.row.Seq < kept {
			total++
			continue
		}
		generations = append(generations, version)
	}
	entry.generations = generations
	var removed int64
	ended := oldest.Add(time.Nanosecond)
	entry.resources, removed = pruneMemoryVersions(entry.resources, ended)
	total += removed
	entry.health, removed = pruneMemoryVersions(entry.health, ended)
	total += removed
	entry.flags, removed = pruneMemoryVersions(entry.flags, ended)
	total += removed
	return total, nil
}

func (m *memoryTables) PruneAWSChanges(_ context.Context, scope tenancy.OrgScope, before time.Time) (int64, error) {
	if before.IsZero() {
		return 0, ErrInvalidAWSInventory
	}
	scope = inventoryScope(scope)
	before = tableTime(before)
	m.mu.Lock()
	defer m.mu.Unlock()
	var total int64
	for id, change := range m.changes[scope.Write] {
		if change.EventTime.Before(before) {
			delete(m.changes[scope.Write], id)
			total++
		}
	}
	for key, intervals := range m.coverage {
		if key.org != scope.Write {
			continue
		}
		trimmed := trimAWSIntervals(intervals, before)
		if len(trimmed) == 0 {
			delete(m.coverage, key)
			continue
		}
		m.coverage[key] = trimmed
	}
	return total, nil
}

// visibleUnits returns the units each read organization answers for, in
// read order of precedence.
func (m *memoryTables) visibleUnits(scope tenancy.OrgScope) []memoryUnitKey {
	held := map[string]map[AWSUnit]bool{}
	for key := range m.units {
		if held[key.org] == nil {
			held[key.org] = map[AWSUnit]bool{}
		}
		held[key.org][key.unit] = true
	}
	out := make([]memoryUnitKey, 0)
	for key := range m.units {
		if firstHolder(scope.Read, key.org, func(org string) bool { return held[org][key.unit] }) {
			out = append(out, key)
		}
	}
	sort.Slice(out, func(left, right int) bool { return lessAWSUnit(out[left].unit, out[right].unit) })
	return out
}

func lessAWSUnit(left, right AWSUnit) bool {
	if left.Account != right.Account {
		return left.Account < right.Account
	}
	if left.Region != right.Region {
		return left.Region < right.Region
	}
	return left.Service < right.Service
}

func matchAWSUnit(unit AWSUnit, account, region, service string) bool {
	return (account == "" || unit.Account == account) && (region == "" || unit.Region == region) && (service == "" || unit.Service == service)
}

func (m *memoryTables) AWSUnits(_ context.Context, scope tenancy.OrgScope, query AWSUnitQuery) ([]AWSUnitStatus, error) {
	scope = inventoryScope(scope)
	asOf := tableTime(query.AsOf)
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]AWSUnitStatus, 0)
	for _, key := range m.visibleUnits(scope) {
		if !matchAWSUnit(key.unit, query.Account, query.Region, query.Service) {
			continue
		}
		entry := m.units[key]
		status := AWSUnitStatus{Org: key.org, Unit: key.unit, Sync: entry.sync}
		status.Sync.Gaps = cloneTableJSON(status.Sync.Gaps)
		for _, version := range entry.generations {
			if version.at(asOf) {
				generation := version.row
				generation.Gaps = cloneTableJSON(generation.Gaps)
				status.Generation = &generation
			}
		}
		out = append(out, status)
	}
	return out, nil
}

func copyAWSResource(row AWSResource) AWSResource {
	row.Tags, row.Attributes, row.Detail = cloneTableMap(row.Tags), cloneTableMap(row.Attributes), cloneTableJSON(row.Detail)
	if row.Relations != nil {
		row.Relations = append([]AWSRelation(nil), row.Relations...)
	}
	return row
}

func (m *memoryTables) AWSResources(_ context.Context, scope tenancy.OrgScope, query AWSResourceQuery) ([]AWSResource, error) {
	scope = inventoryScope(scope)
	asOf := tableTime(query.AsOf)
	prefix := asciiLower(query.NamePrefix)
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]AWSResource, 0)
	for _, key := range m.visibleUnits(scope) {
		if !matchAWSUnit(key.unit, query.Account, query.Region, query.Service) {
			continue
		}
		for _, version := range m.units[key].resources {
			row := version.row
			if !version.at(asOf) || query.Type != "" && row.Type != query.Type || query.App != "" && row.App != query.App || query.ARN != "" && row.ARN != query.ARN {
				continue
			}
			if prefix != "" {
				name := asciiLower(row.Name)
				if name < prefix || name >= prefixUpperBound(prefix) {
					continue
				}
			}
			out = append(out, copyAWSResource(row))
		}
	}
	sort.SliceStable(out, func(left, right int) bool {
		if out[left].Account != out[right].Account || out[left].Region != out[right].Region || out[left].Service != out[right].Service {
			return lessAWSUnit(AWSUnit{out[left].Account, out[left].Region, out[left].Service}, AWSUnit{out[right].Account, out[right].Region, out[right].Service})
		}
		return out[left].ARN < out[right].ARN
	})
	if query.Limit > 0 && len(out) > query.Limit {
		out = out[:query.Limit]
	}
	return out, nil
}

func (m *memoryTables) AWSHealth(_ context.Context, scope tenancy.OrgScope, query AWSHealthQuery) ([]AWSHealth, error) {
	scope = inventoryScope(scope)
	asOf := tableTime(query.AsOf)
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]AWSHealth, 0)
	for _, key := range m.visibleUnits(scope) {
		if !matchAWSUnit(key.unit, query.Account, query.Region, query.Service) {
			continue
		}
		for _, version := range m.units[key].health {
			row := version.row
			if !version.at(asOf) || query.State != "" && row.State != query.State || query.ARN != "" && row.ARN != query.ARN && row.Ref != query.ARN {
				continue
			}
			if row.Children != nil {
				row.Children = append([]string(nil), row.Children...)
			}
			row.Detail = cloneTableJSON(row.Detail)
			out = append(out, row)
		}
	}
	sort.SliceStable(out, func(left, right int) bool {
		a, b := out[left], out[right]
		for _, pair := range [][2]string{{a.Account, b.Account}, {a.Region, b.Region}, {a.Service, b.Service}, {a.ARN, b.ARN}, {a.Kind, b.Kind}, {a.Ref, b.Ref}} {
			if pair[0] != pair[1] {
				return pair[0] < pair[1]
			}
		}
		return false
	})
	return out, nil
}

func (m *memoryTables) AWSSecurityFlags(_ context.Context, scope tenancy.OrgScope, query AWSFlagQuery) ([]AWSSecurityFlag, error) {
	scope = inventoryScope(scope)
	asOf := tableTime(query.AsOf)
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]AWSSecurityFlag, 0)
	for _, key := range m.visibleUnits(scope) {
		if !matchAWSUnit(key.unit, query.Account, query.Region, query.Service) {
			continue
		}
		for _, version := range m.units[key].flags {
			row := version.row
			if !version.at(asOf) || query.Flag != "" && row.Flag != query.Flag || query.ARN != "" && row.ARN != query.ARN {
				continue
			}
			row.Detail = cloneTableJSON(row.Detail)
			out = append(out, row)
		}
	}
	sort.SliceStable(out, func(left, right int) bool {
		a, b := out[left], out[right]
		for _, pair := range [][2]string{{a.Account, b.Account}, {a.Region, b.Region}, {a.Service, b.Service}, {a.ARN, b.ARN}, {a.Flag, b.Flag}} {
			if pair[0] != pair[1] {
				return pair[0] < pair[1]
			}
		}
		return false
	})
	return out, nil
}

func (m *memoryTables) coverageHolder(account, region string) func(string) bool {
	return func(org string) bool { return len(m.coverage[memoryPairKey{org, account, region}]) > 0 }
}

func (m *memoryTables) AWSChanges(_ context.Context, scope tenancy.OrgScope, query AWSChangeQuery) ([]AWSChange, error) {
	scope = inventoryScope(scope)
	since, until := tableTime(query.Since), tableTime(query.Until)
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]AWSChange, 0)
	for _, org := range scope.Read {
		for _, change := range m.changes[org] {
			switch {
			case query.Account != "" && change.Account != query.Account,
				query.Region != "" && change.Region != query.Region,
				query.Service != "" && change.Service != query.Service,
				query.ResourceARN != "" && change.ResourceARN != query.ResourceARN,
				query.App != "" && change.App != query.App,
				!since.IsZero() && change.EventTime.Before(since),
				!until.IsZero() && !change.EventTime.Before(until):
				continue
			}
			if !firstHolder(scope.Read, org, m.coverageHolder(change.Account, change.Region)) {
				continue
			}
			change.Fields, change.Detail = cloneTableMap(change.Fields), cloneTableJSON(change.Detail)
			out = append(out, change)
		}
	}
	sort.Slice(out, func(left, right int) bool {
		if !out[left].EventTime.Equal(out[right].EventTime) {
			return out[left].EventTime.Before(out[right].EventTime)
		}
		return out[left].EventID < out[right].EventID
	})
	if query.Limit > 0 && len(out) > query.Limit {
		out = out[:query.Limit]
	}
	return out, nil
}

func (m *memoryTables) AWSChangeCoverage(_ context.Context, scope tenancy.OrgScope, account, region string) ([]AWSPairCoverage, error) {
	scope = inventoryScope(scope)
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]AWSPairCoverage, 0)
	for key, intervals := range m.coverage {
		if !scope.Contains(key.org) || account != "" && key.account != account || region != "" && key.region != region {
			continue
		}
		if !firstHolder(scope.Read, key.org, m.coverageHolder(key.account, key.region)) {
			continue
		}
		out = append(out, AWSPairCoverage{Org: key.org, Account: key.account, Region: key.region, Intervals: append([]AWSInterval(nil), intervals...)})
	}
	sort.Slice(out, func(left, right int) bool {
		if out[left].Account != out[right].Account {
			return out[left].Account < out[right].Account
		}
		return out[left].Region < out[right].Region
	})
	return out, nil
}

func (m *memoryTables) AWSPairs(_ context.Context, scope tenancy.OrgScope) ([]AWSPair, error) {
	scope = inventoryScope(scope)
	m.mu.RLock()
	defer m.mu.RUnlock()
	seen := map[AWSPair]bool{}
	for key := range m.units {
		if key.org == scope.Write {
			seen[AWSPair{key.unit.Account, key.unit.Region}] = true
		}
	}
	for key := range m.coverage {
		if key.org == scope.Write {
			seen[AWSPair{key.account, key.region}] = true
		}
	}
	out := make([]AWSPair, 0, len(seen))
	for pair := range seen {
		out = append(out, pair)
	}
	sort.Slice(out, func(left, right int) bool {
		if out[left].Account != out[right].Account {
			return out[left].Account < out[right].Account
		}
		return out[left].Region < out[right].Region
	})
	return out, nil
}

func (m *memoryTables) ReplaceFinOpsCostDays(_ context.Context, scope tenancy.OrgScope, payer string, days []string, lines []FinOpsCostLine) error {
	days, lines, err := prepareFinOpsDays(payer, days, lines)
	if err != nil {
		return err
	}
	scope = inventoryScope(scope)
	replaced := make(map[string]bool, len(days))
	for _, day := range days {
		replaced[day] = true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	stored := m.lines[scope.Write]
	if stored == nil {
		stored = map[finOpsLineKey]FinOpsCostLine{}
		m.lines[scope.Write] = stored
	}
	for key := range stored {
		if key.payer == payer && replaced[key.day] {
			delete(stored, key)
		}
	}
	for _, line := range lines {
		stored[line.key()] = line
	}
	daily := m.daily[scope.Write]
	if daily == nil {
		daily = map[finOpsDailyRowKey]finOpsDailyRow{}
		m.daily[scope.Write] = daily
	}
	for key := range daily {
		if key.payer == payer && replaced[key.day] {
			delete(daily, key)
		}
	}
	for _, row := range rollUpFinOpsLines(lines) {
		daily[finOpsDailyKeyOf(row.line)] = row
	}
	return nil
}

func (m *memoryTables) DeleteFinOpsPayer(_ context.Context, scope tenancy.OrgScope, payer string) error {
	if !validTableKey(payer) {
		return ErrInvalidFinOps
	}
	scope = inventoryScope(scope)
	m.mu.Lock()
	defer m.mu.Unlock()
	for key := range m.lines[scope.Write] {
		if key.payer == payer {
			delete(m.lines[scope.Write], key)
		}
	}
	for key := range m.daily[scope.Write] {
		if key.payer == payer {
			delete(m.daily[scope.Write], key)
		}
	}
	return nil
}

func (m *memoryTables) DeleteFinOpsIncreasesForPayer(_ context.Context, scope tenancy.OrgScope, payer string) (int64, error) {
	if !validTableKey(payer) {
		return 0, ErrInvalidFinOps
	}
	scope = inventoryScope(scope)
	m.mu.Lock()
	defer m.mu.Unlock()
	var count int64
	for id, increase := range m.increases[scope.Write] {
		if increase.Payer == payer {
			delete(m.increases[scope.Write], id)
			count++
		}
	}
	return count, nil
}

func (m *memoryTables) PruneFinOpsCostLines(_ context.Context, scope tenancy.OrgScope, before string) (int64, error) {
	if !validFinOpsDay(before) {
		return 0, ErrInvalidFinOps
	}
	scope = inventoryScope(scope)
	m.mu.Lock()
	defer m.mu.Unlock()
	var count int64
	for key := range m.lines[scope.Write] {
		if key.day < before {
			delete(m.lines[scope.Write], key)
			count++
		}
	}
	for key := range m.daily[scope.Write] {
		if key.day < before {
			delete(m.daily[scope.Write], key)
		}
	}
	return count, nil
}

// visibleLines returns the matching lines each read organization answers
// for, ordered like the SQL tables.
func (m *memoryTables) visibleLines(scope tenancy.OrgScope, query FinOpsCostQuery) []FinOpsCostLine {
	held := map[string]map[string]bool{}
	for _, org := range scope.Read {
		held[org] = map[string]bool{}
		for key := range m.lines[org] {
			held[org][key.payer+"\x00"+key.day] = true
		}
	}
	out := make([]FinOpsCostLine, 0)
	for _, org := range scope.Read {
		for key, line := range m.lines[org] {
			if !matchFinOpsLine(query, line) {
				continue
			}
			day := key.payer + "\x00" + key.day
			if firstHolder(scope.Read, org, func(candidate string) bool { return held[candidate][day] }) {
				out = append(out, line)
			}
		}
	}
	sort.Slice(out, func(left, right int) bool { return lessFinOpsLine(out[left], out[right]) })
	return out
}

func (m *memoryTables) FinOpsCostLines(_ context.Context, scope tenancy.OrgScope, query FinOpsCostQuery) ([]FinOpsCostLine, error) {
	if !validFinOpsQuery(query) {
		return nil, ErrInvalidFinOps
	}
	scope = inventoryScope(scope)
	m.mu.RLock()
	defer m.mu.RUnlock()
	lines := m.visibleLines(scope, query)
	if query.Limit > 0 && len(lines) > query.Limit {
		lines = lines[:query.Limit]
	}
	return lines, nil
}

// addAmount adds two amounts, reporting the closed overflow error instead of
// a wrapped total.
func addAmount(total, amount int64) (int64, error) {
	if amount > 0 && total > math.MaxInt64-amount || amount < 0 && total < math.MinInt64-amount {
		return 0, ErrAmountOverflow
	}
	return total + amount, nil
}

// visibleDaily returns the matching rollup rows each read organization
// answers for, with the same precedence as visibleLines: a payer's day
// belongs to the first organization holding cost lines for it.
func (m *memoryTables) visibleDaily(scope tenancy.OrgScope, query FinOpsCostQuery) []finOpsDailyRow {
	held := map[string]map[string]bool{}
	for _, org := range scope.Read {
		held[org] = map[string]bool{}
		for key := range m.lines[org] {
			held[org][key.payer+"\x00"+key.day] = true
		}
	}
	query = finOpsDailyQuery(query)
	out := make([]finOpsDailyRow, 0)
	for _, org := range scope.Read {
		for key, row := range m.daily[org] {
			if !matchFinOpsLine(query, row.line) {
				continue
			}
			day := key.payer + "\x00" + key.day
			if firstHolder(scope.Read, org, func(candidate string) bool { return held[candidate][day] }) {
				out = append(out, row)
			}
		}
	}
	sort.Slice(out, func(left, right int) bool {
		return lessFinOpsLine(out[left].line, out[right].line) || lessFinOpsUsageGroup(out[left].line, out[right].line)
	})
	return out
}

// FinOpsCostTotals sums the daily rollup when it answers the query, like the
// SQL tables, and the visible lines otherwise or when a rollup row it would
// sum overflowed.
func (m *memoryTables) FinOpsCostTotals(_ context.Context, scope tenancy.OrgScope, query FinOpsCostQuery, groupBy ...FinOpsDimension) ([]FinOpsCostTotal, error) {
	if !validFinOpsQuery(query) || !validFinOpsDimensions(groupBy) {
		return nil, ErrInvalidFinOps
	}
	scope = inventoryScope(scope)
	if finOpsDailyAnswers(query, groupBy) {
		m.mu.RLock()
		daily := m.visibleDaily(scope, query)
		m.mu.RUnlock()
		overflowed := false
		for _, row := range daily {
			overflowed = overflowed || row.overflowed
		}
		if !overflowed {
			return sumFinOpsRows(daily, groupBy)
		}
	}
	m.mu.RLock()
	lines := m.visibleLines(scope, query)
	m.mu.RUnlock()
	rows := make([]finOpsDailyRow, len(lines))
	for index, line := range lines {
		rows[index] = finOpsDailyRow{line: line, lines: 1}
	}
	return sumFinOpsRows(rows, groupBy)
}

// sumFinOpsRows groups rows by the dimensions, each row counting for its
// lines, and orders the groups by them.
func sumFinOpsRows(rows []finOpsDailyRow, groupBy []FinOpsDimension) ([]FinOpsCostTotal, error) {
	groups := map[string]*FinOpsCostTotal{}
	keys := map[string][]string{}
	for _, row := range rows {
		line := row.line
		values := make([]string, len(groupBy))
		for index, dimension := range groupBy {
			values[index] = finOpsDimensionValue(line, dimension)
		}
		key := strings.Join(values, "\x00")
		total := groups[key]
		if total == nil {
			total = &FinOpsCostTotal{}
			for index, dimension := range groupBy {
				setFinOpsDimension(total, dimension, values[index])
			}
			groups[key] = total
			keys[key] = values
		}
		net, err := addAmount(total.NetAmortizedMicros, line.NetAmortizedMicros)
		if err != nil {
			return nil, err
		}
		quantity, err := addAmount(total.QuantityMicros, line.QuantityMicros)
		if err != nil {
			return nil, err
		}
		total.NetAmortizedMicros, total.QuantityMicros = net, quantity
		total.Lines += row.lines
		if total.MinDay == "" || line.Day < total.MinDay {
			total.MinDay = line.Day
		}
		total.EstimatedTeam = total.EstimatedTeam || line.EstimatedTeam
		total.EstimatedRegion = total.EstimatedRegion || line.EstimatedRegion
	}
	order := make([]string, 0, len(groups))
	for key := range groups {
		order = append(order, key)
	}
	sort.Slice(order, func(left, right int) bool {
		a, b := keys[order[left]], keys[order[right]]
		for index := range a {
			if a[index] != b[index] {
				return a[index] < b[index]
			}
		}
		return false
	})
	out := make([]FinOpsCostTotal, 0, len(order))
	for _, key := range order {
		out = append(out, *groups[key])
	}
	return out, nil
}

func copyFinOpsIncrease(increase FinOpsIncrease) FinOpsIncrease {
	increase.Suspects, increase.Detail = cloneTableJSON(increase.Suspects), cloneTableJSON(increase.Detail)
	if increase.Events != nil {
		increase.Events = append([]FinOpsIncreaseEvent(nil), increase.Events...)
	}
	return increase
}

func (m *memoryTables) PutFinOpsIncrease(_ context.Context, scope tenancy.OrgScope, increase FinOpsIncrease) error {
	increase, err := cleanFinOpsIncrease(increase)
	if err != nil {
		return err
	}
	scope = inventoryScope(scope)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.increases[scope.Write] == nil {
		m.increases[scope.Write] = map[string]FinOpsIncrease{}
	}
	m.increases[scope.Write][increase.ID] = increase
	return nil
}

func containsTableString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func (m *memoryTables) FinOpsIncreases(_ context.Context, scope tenancy.OrgScope, query FinOpsIncreaseQuery) ([]FinOpsIncrease, error) {
	scope = inventoryScope(scope)
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]FinOpsIncrease, 0)
	for _, org := range scope.Read {
		for id, increase := range m.increases[org] {
			if !firstHolder(scope.Read, org, func(candidate string) bool { _, ok := m.increases[candidate][id]; return ok }) {
				continue
			}
			if len(query.IDs) > 0 && !containsTableString(query.IDs, id) || len(query.Statuses) > 0 && !containsTableString(query.Statuses, increase.Status) ||
				query.OwnerTeam != "" && increase.OwnerTeam != query.OwnerTeam || query.Service != "" && increase.Service != query.Service {
				continue
			}
			out = append(out, copyFinOpsIncrease(increase))
		}
	}
	sort.Slice(out, func(left, right int) bool {
		if out[left].StartedOn != out[right].StartedOn {
			return out[left].StartedOn < out[right].StartedOn
		}
		return out[left].ID < out[right].ID
	})
	return out, nil
}

func (m *memoryTables) PutFinOpsReport(_ context.Context, scope tenancy.OrgScope, report FinOpsReport) error {
	report, err := cleanFinOpsReport(report)
	if err != nil {
		return err
	}
	scope = inventoryScope(scope)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reports[scope.Write] == nil {
		m.reports[scope.Write] = map[string]FinOpsReport{}
	}
	m.reports[scope.Write][report.Month] = report
	return nil
}

func (m *memoryTables) FinOpsReport(_ context.Context, scope tenancy.OrgScope, month string) (FinOpsReport, bool, error) {
	if !validFinOpsMonth(month) {
		return FinOpsReport{}, false, ErrInvalidFinOps
	}
	scope = inventoryScope(scope)
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, org := range scope.Read {
		if report, ok := m.reports[org][month]; ok {
			report.Model = cloneTableJSON(report.Model)
			return report, true, nil
		}
	}
	return FinOpsReport{}, false, nil
}

func (m *memoryTables) FinOpsReports(_ context.Context, scope tenancy.OrgScope) ([]FinOpsReport, error) {
	scope = inventoryScope(scope)
	m.mu.RLock()
	defer m.mu.RUnlock()
	seen := map[string]bool{}
	out := make([]FinOpsReport, 0)
	for _, org := range scope.Read {
		for month, report := range m.reports[org] {
			if seen[month] {
				continue
			}
			seen[month] = true
			report.Model = nil
			out = append(out, report)
		}
	}
	sort.Slice(out, func(left, right int) bool { return out[left].Month < out[right].Month })
	return out, nil
}

func (m *memoryTables) PutFinOpsSavingDecision(_ context.Context, scope tenancy.OrgScope, decision FinOpsSavingDecision) error {
	decision, err := cleanFinOpsDecision(decision)
	if err != nil {
		return err
	}
	scope = inventoryScope(scope)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.decisions[scope.Write] == nil {
		m.decisions[scope.Write] = map[string]FinOpsSavingDecision{}
	}
	m.decisions[scope.Write][decision.SavingID] = decision
	return nil
}

func (m *memoryTables) SyncFinOpsSavingDecisions(_ context.Context, scope tenancy.OrgScope, present []string, at time.Time) (int64, int64, error) {
	keep, err := presentSavings(present, at)
	if err != nil {
		return 0, 0, err
	}
	at = tableTime(at)
	scope = inventoryScope(scope)
	m.mu.Lock()
	defer m.mu.Unlock()
	var orphaned, restored int64
	for id, decision := range m.decisions[scope.Write] {
		switch {
		case !keep[id] && decision.OrphanedAt.IsZero():
			decision.OrphanedAt = at
			orphaned++
		case keep[id] && !decision.OrphanedAt.IsZero():
			decision.OrphanedAt = time.Time{}
			restored++
		default:
			continue
		}
		m.decisions[scope.Write][id] = decision
	}
	return orphaned, restored, nil
}

func (m *memoryTables) FillFinOpsSavingDecisionSubjects(_ context.Context, scope tenancy.OrgScope, subjects []FinOpsSavingDecision) (int64, error) {
	fills, err := decisionSubjects(subjects)
	if err != nil {
		return 0, err
	}
	scope = inventoryScope(scope)
	m.mu.Lock()
	defer m.mu.Unlock()
	var count int64
	for id, subject := range fills {
		decision, ok := m.decisions[scope.Write][id]
		if !ok || !fillDecisionSubject(&decision, subject) {
			continue
		}
		m.decisions[scope.Write][id] = decision
		count++
	}
	return count, nil
}

func (m *memoryTables) DeleteFinOpsSavingDecisionsForPayer(_ context.Context, scope tenancy.OrgScope, payer string) (int64, error) {
	if !validTableKey(payer) {
		return 0, ErrInvalidFinOps
	}
	scope = inventoryScope(scope)
	m.mu.Lock()
	defer m.mu.Unlock()
	var count int64
	for id, decision := range m.decisions[scope.Write] {
		if decision.Payer == payer {
			delete(m.decisions[scope.Write], id)
			count++
		}
	}
	return count, nil
}

func (m *memoryTables) DeleteFinOpsSavingDecisionsWithoutPayer(_ context.Context, scope tenancy.OrgScope, accounts []string) (int64, error) {
	named, err := decisionAccounts(accounts)
	if err != nil {
		return 0, err
	}
	scope = inventoryScope(scope)
	m.mu.Lock()
	defer m.mu.Unlock()
	var count int64
	for id, decision := range m.decisions[scope.Write] {
		if decision.Payer == "" && named[decision.Account] {
			delete(m.decisions[scope.Write], id)
			count++
		}
	}
	return count, nil
}

func (m *memoryTables) PruneFinOpsSavingDecisions(_ context.Context, scope tenancy.OrgScope, orphanedBefore time.Time) (int64, error) {
	if orphanedBefore.IsZero() {
		return 0, ErrInvalidFinOps
	}
	orphanedBefore = tableTime(orphanedBefore)
	scope = inventoryScope(scope)
	m.mu.Lock()
	defer m.mu.Unlock()
	var count int64
	for id, decision := range m.decisions[scope.Write] {
		if !decision.OrphanedAt.IsZero() && decision.OrphanedAt.Before(orphanedBefore) {
			delete(m.decisions[scope.Write], id)
			count++
		}
	}
	return count, nil
}

func (m *memoryTables) FinOpsSavingDecisions(_ context.Context, scope tenancy.OrgScope) ([]FinOpsSavingDecision, error) {
	scope = inventoryScope(scope)
	m.mu.RLock()
	defer m.mu.RUnlock()
	seen := map[string]bool{}
	out := make([]FinOpsSavingDecision, 0)
	for _, org := range scope.Read {
		for id, decision := range m.decisions[org] {
			if seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, decision)
		}
	}
	sort.Slice(out, func(left, right int) bool { return out[left].SavingID < out[right].SavingID })
	return out, nil
}
