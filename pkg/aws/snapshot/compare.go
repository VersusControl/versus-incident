package snapshot

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/changes"
	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// Compare statuses.
const (
	CompareOK          = "ok"
	CompareUnavailable = "unavailable"
)

// maxChangesBetween is how many change list items a compare answer carries.
const maxChangesBetween = 20

// CompareRequest compares the inventory at From with the inventory at To
// (now when zero).
type CompareRequest struct {
	From    time.Time
	To      time.Time
	Account string
	Region  string
	Service string
	// Services narrows the compare to resources, units and changes in any
	// of these services; empty means every service.
	Services []string
}

// CompareResource names one added or removed resource.
type CompareResource struct {
	ResourceARN string `json:"resource_arn"`
	Type        string `json:"type"`
	Name        string `json:"name"`
	Account     string `json:"account"`
	Region      string `json:"region"`
	// IDHint tells apart items that share a type and name: the resource ID
	// suffix, with the account and region when that repeats too.
	IDHint string `json:"id_hint,omitempty"`
}

// CompareField is one field that differs. A hidden field (a secret-shaped
// name, or a tag whose key is not allowlisted) keeps the fact that it
// changed and never its values.
type CompareField struct {
	Field  string  `json:"field"`
	Before *string `json:"before"`
	After  *string `json:"after"`
	Hidden bool    `json:"hidden"`
}

// CompareChanged is one resource with differing fields.
type CompareChanged struct {
	CompareResource
	Fields []CompareField `json:"fields"`
}

// ComparePartial is one unit without history at From; its resources are
// left out of the diff and named instead.
type ComparePartial struct {
	Account       string     `json:"account"`
	Region        string     `json:"region"`
	Service       string     `json:"service"`
	HistoryStarts *time.Time `json:"history_starts"`
	// Reason is why the unit was not compared: a gap at one of the two
	// times, not_checked for a unit without history at From,
	// service_denied / not_on_plan for a service Settings knows cannot be
	// read, or not_available_in_region for a service AWS does not offer
	// in the Region. It is never empty.
	Reason string `json:"reason"`
	// Excluded marks a service Settings records as denied or not on the
	// account's plan, or one not offered in the Region: it is left out of
	// the compare scope and does not make the answer incomplete.
	Excluded bool `json:"excluded"`
}

// Compare reasons for a service left out of the compare scope.
const (
	CompareServiceDenied = "service_denied"
	CompareNotOnPlan     = "not_on_plan"
)

// CompareSnapshots is the kept history in scope: the oldest kept snapshot
// and the most generations any unit keeps.
type CompareSnapshots struct {
	Oldest *time.Time `json:"oldest"`
	Count  int        `json:"count"`
}

// CompareAnswer is the GET /api/admin/aws/changes/compare answer.
type CompareAnswer struct {
	From                time.Time         `json:"from"`
	To                  time.Time         `json:"to"`
	Status              string            `json:"status"`
	HistoryStarts       *time.Time        `json:"history_starts"`
	Complete            bool              `json:"complete"`
	Added               []CompareResource `json:"added"`
	Removed             []CompareResource `json:"removed"`
	Changed             []CompareChanged  `json:"changed"`
	Partial             []ComparePartial  `json:"partial"`
	Snapshots           CompareSnapshots  `json:"snapshots"`
	ChangesBetween      []FeedItem        `json:"changes_between"`
	ChangesBetweenTotal int64             `json:"changes_between_total"`
	// Limits are the window and compare limits this request was held to.
	Limits ChangesLimitsView `json:"limits"`
}

// Compare diffs the inventory between two times inside the tier's window.
// Each unit's own oldest kept generation decides whether it has history at
// From: a unit without it is named in partial, never diffed against
// nothing, and a From before every unit's history is unavailable with the
// earliest time that can be picked. Only allowlisted attributes are
// compared; hidden values never appear.
func (feed *ChangeFeed) Compare(ctx context.Context, request CompareRequest) (CompareAnswer, error) {
	feeds, err := feed.feeds()
	if err != nil {
		return CompareAnswer{}, err
	}
	now := feed.now().UTC()
	from, to := request.From.UTC(), request.To.UTC()
	if to.IsZero() || to.After(now) {
		to = now
	}
	if from.IsZero() || !from.Before(to) {
		return CompareAnswer{}, ErrBadFeedQuery
	}
	limits := feed.Limits()
	tierStart := now.Add(-limits.MaxCompareAge)
	if from.Before(tierStart.Add(-time.Minute)) {
		return CompareAnswer{}, ErrWindowLocked
	}
	if request.Services, err = NormalizeServices(request.Services); err != nil {
		return CompareAnswer{}, err
	}
	answer := CompareAnswer{From: from, To: to, Status: CompareOK, Added: []CompareResource{}, Removed: []CompareResource{},
		Changed: []CompareChanged{}, Partial: []ComparePartial{}, ChangesBetween: []FeedItem{}, Limits: limits.View()}

	history, err := feed.store.UnitHistory(ctx, Query{Account: request.Account, Region: request.Region, Service: request.Service})
	if err != nil {
		return CompareAnswer{}, err
	}
	// The configured accounts and regions are the scope (H3): a removed
	// region's leftover units are not compared, and a configured region
	// with no history yet is named, not silently left out.
	inScope, unreadable, scoped, err := feed.compareScope(ctx, request)
	if err != nil {
		return CompareAnswer{}, err
	}
	// partial names one unit not compared. A service Settings knows is
	// denied or not on the plan carries that reason and is excluded, as is
	// a service not offered in the Region or not on the plan whatever
	// Settings last recorded. Every entry has a reason.
	partial := func(entry ComparePartial) ComparePartial {
		if reason := unreadable[unitKey(entry.Account, "", entry.Service)]; entry.Service != "" && reason != "" {
			entry.Reason, entry.Excluded = reason, true
		}
		switch entry.Reason {
		case string(model.GapNotInRegion), CompareNotOnPlan:
			entry.Excluded = true
		case "":
			entry.Reason = string(model.GapNotChecked)
		}
		return entry
	}
	// oldest is the first snapshot of any unit; fullFrom is the first time
	// every unit in scope has one, so a compare from it is full.
	var oldest, fullFrom time.Time
	withHistory := 0
	missing := map[string]bool{}
	seenPair := map[string]bool{}
	for _, unit := range history {
		// Policy entries are not resources, so their unit never limits or
		// is named in a compare.
		if unit.Unit.Service == ChangeService || unit.Unit.Service == model.PolicyEntryService {
			continue
		}
		if scoped && !inScope[pairKey(unit.Unit.Account, unit.Unit.Region)] {
			continue
		}
		seenPair[pairKey(unit.Unit.Account, unit.Unit.Region)] = true
		if !serviceInFilter(request.Services, unit.Unit.Service) {
			continue
		}
		if oldest.IsZero() || unit.Oldest.Before(oldest) {
			oldest = unit.Oldest
		}
		// An excluded service is never compared, so it does not hold back
		// the time a full compare can start.
		if unit.Oldest.After(fullFrom) && unreadable[unitKey(unit.Unit.Account, "", unit.Unit.Service)] == "" {
			fullFrom = unit.Oldest
		}
		answer.Snapshots.Count = max(answer.Snapshots.Count, unit.Generations)
		if unit.Oldest.After(from) {
			starts := unit.Oldest
			answer.Partial = append(answer.Partial, partial(ComparePartial{Account: unit.Unit.Account, Region: unit.Unit.Region, Service: unit.Unit.Service, HistoryStarts: &starts}))
			missing[unitKey(unit.Unit.Account, unit.Unit.Region, unit.Unit.Service)] = true
			continue
		}
		withHistory++
	}
	if !oldest.IsZero() {
		at := oldest
		answer.Snapshots.Oldest = &at
	}
	if !fullFrom.IsZero() {
		starts := fullFrom
		if starts.Before(tierStart) {
			starts = tierStart
		}
		answer.HistoryStarts = &starts
	}
	for _, key := range sortedKeysOf(inScope) {
		if !seenPair[key] {
			account, region, _ := strings.Cut(key, "/")
			answer.Partial = append(answer.Partial, partial(ComparePartial{Account: account, Region: region}))
		}
	}
	if withHistory == 0 {
		answer.Status, answer.Partial = CompareUnavailable, []ComparePartial{}
		return answer, nil
	}

	query := Query{Account: request.Account, Region: request.Region, Service: request.Service, AsOf: from}
	// Tag values are read as stored so a change to an unallowlisted tag
	// is still seen; diffResource hides its values.
	before, err := feed.store.resources(query, true)
	if err != nil {
		return CompareAnswer{}, err
	}
	query.AsOf = time.Time{}
	if to.Before(now) {
		query.AsOf = to
	}
	after, err := feed.store.resources(query, true)
	if err != nil {
		return CompareAnswer{}, err
	}
	skip := func(resource model.Resource) bool {
		return missing[unitKey(resource.Account, resource.Region, resource.Service)] || missing[unitKey(resource.Account, model.GlobalRegion, resource.Service)] ||
			!serviceInFilter(request.Services, resource.Service)
	}
	allowlist := feed.store.readAllowlist()
	was := map[string]model.Resource{}
	for _, resource := range before.Resources {
		if !skip(resource) {
			was[resource.ARN] = resource
		}
	}
	is := map[string]model.Resource{}
	for _, resource := range after.Resources {
		if !skip(resource) {
			is[resource.ARN] = resource
		}
	}
	for _, arn := range sortedKeysOf(is) {
		resource := is[arn]
		old, existed := was[arn]
		if !existed {
			answer.Added = append(answer.Added, compareResource(resource))
			continue
		}
		if fields := diffResource(old, resource, allowlist); len(fields) > 0 {
			answer.Changed = append(answer.Changed, CompareChanged{CompareResource: compareResource(resource), Fields: fields})
		}
	}
	for _, arn := range sortedKeysOf(was) {
		if _, still := is[arn]; !still {
			answer.Removed = append(answer.Removed, compareResource(was[arn]))
		}
	}
	// A unit with a gap at either time is named with its reason, so an
	// incomplete answer always says what was not compared.
	named := map[string]bool{}
	for _, gap := range append(append([]model.Gap(nil), before.Gaps...), after.Gaps...) {
		key := unitKey(gap.Account, gap.Region, gap.Service)
		if gap.Service == model.PolicyEntryService || scoped && !inScope[pairKey(gap.Account, gap.Region)] || named[key] || missing[key] || gap.Service != "" && !serviceInFilter(request.Services, gap.Service) {
			continue
		}
		named[key] = true
		answer.Partial = append(answer.Partial, partial(ComparePartial{Account: gap.Account, Region: gap.Region, Service: gap.Service, Reason: string(gap.Reason)}))
	}
	answer.Complete = true
	for _, entry := range answer.Partial {
		if !entry.Excluded {
			answer.Complete = false
			break
		}
	}
	setIDHints(&answer)

	stored, err := feeds.AWSChangeFeed(ctx, feed.store.scope, storage.AWSChangeFeedQuery{Account: request.Account, Region: request.Region, Service: request.Service,
		Services: request.Services, Since: from, Until: to, RowLimit: maxChangesBetween})
	if err != nil {
		return CompareAnswer{}, err
	}
	for _, minute := range stored.Minutes {
		answer.ChangesBetweenTotal += minute.Events
	}
	for _, row := range stored.Rows {
		if row.ErrorCode == "" {
			answer.ChangesBetween = append(answer.ChangesBetween, itemFromRow(row))
		}
	}
	if err := feed.fillItems(ctx, feeds, answer.ChangesBetween); err != nil {
		return CompareAnswer{}, err
	}
	return answer, nil
}

// compareScope lists the configured account and region pairs (global
// included) the compare covers, and the services Settings records as
// denied or not on the plan, keyed by account and service with the compare
// reason; scoped is false without a settings scope.
func (feed *ChangeFeed) compareScope(ctx context.Context, request CompareRequest) (map[string]bool, map[string]string, bool, error) {
	if feed.scope == nil {
		return nil, nil, false, nil
	}
	accounts, ok, err := feed.scope(ctx)
	if err != nil || !ok {
		return nil, nil, false, err
	}
	pairs := map[string]bool{}
	unreadable := map[string]string{}
	for _, account := range accounts {
		for _, service := range account.DeniedServices {
			unreadable[unitKey(account.Account, "", service)] = CompareServiceDenied
		}
		for _, service := range account.NotOnPlanServices {
			unreadable[unitKey(account.Account, "", service)] = CompareNotOnPlan
		}
		if request.Account != "" && account.Account != request.Account {
			continue
		}
		for _, region := range append(append([]string(nil), account.Regions...), model.GlobalRegion) {
			if request.Region == "" || region == request.Region {
				pairs[pairKey(account.Account, region)] = true
			}
		}
	}
	return pairs, unreadable, true, nil
}

// setIDHints gives every item whose type and name another item shares a
// short identifier, so look-alike lines (an Auto Scaling group's
// instances, one stack in two regions) can be told apart.
func setIDHints(answer *CompareAnswer) {
	var items []*CompareResource
	for index := range answer.Added {
		items = append(items, &answer.Added[index])
	}
	for index := range answer.Removed {
		items = append(items, &answer.Removed[index])
	}
	for index := range answer.Changed {
		items = append(items, &answer.Changed[index].CompareResource)
	}
	byName := map[string][]*CompareResource{}
	for _, item := range items {
		key := item.Type + "\x00" + item.Name
		byName[key] = append(byName[key], item)
	}
	for _, group := range byName {
		if len(group) < 2 {
			continue
		}
		hints := map[string]int{}
		for _, item := range group {
			item.IDHint = model.ResourceID(item.ResourceARN)
			hints[item.IDHint]++
		}
		for _, item := range group {
			if hints[item.IDHint] > 1 || item.IDHint == "" {
				item.IDHint = item.Account + " " + item.Region + " " + item.IDHint
			}
		}
	}
}

func unitKey(account, region, service string) string { return account + "/" + region + "/" + service }

func compareResource(resource model.Resource) CompareResource {
	return CompareResource{ResourceARN: resource.ARN, Type: resource.Type, Name: resource.Name, Account: resource.Account, Region: resource.Region}
}

// diffResource lists the differing fields of one resource: name, state,
// allowlisted attributes and tags. Hidden fields keep only the fact.
func diffResource(old, current model.Resource, allowlist map[string]bool) []CompareField {
	var out []CompareField
	add := func(field, before, after string, hidden bool) {
		if before == after {
			return
		}
		item := CompareField{Field: field, Hidden: hidden}
		if !hidden {
			if before != "" {
				item.Before = &before
			}
			if after != "" {
				item.After = &after
			}
		}
		out = append(out, item)
	}
	add("name", old.Name, current.Name, false)
	add("state", old.State, current.State, false)
	keys := map[string]bool{}
	for key := range old.Attributes {
		keys[key] = true
	}
	for key := range current.Attributes {
		keys[key] = true
	}
	for _, key := range sortedKeysOf(keys) {
		if !model.AllowedAttribute(key) {
			continue
		}
		add(key, old.Attributes[key], current.Attributes[key], hiddenField(key))
	}
	tags := map[string]bool{}
	for key := range old.Tags {
		tags[key] = true
	}
	for key := range current.Tags {
		tags[key] = true
	}
	names := make([]string, 0, len(tags))
	for key := range tags {
		names = append(names, key)
	}
	sort.Strings(names)
	for _, key := range names {
		// Snapshot tags keep allowlisted values, so compare hides by the
		// live allowlist.
		add(changes.TagFieldPrefix+key, old.Tags[key], current.Tags[key], !allowlist[key])
	}
	return out
}
