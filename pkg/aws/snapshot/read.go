package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/storage"
)

// Auxiliary units feed other resources rather than listing their own.
const (
	// TagService holds tag sets read through the tagging API. They are merged
	// into the matching resources and never listed on their own.
	TagService = "tagging"
	// AppRegistryService holds AppRegistry applications and their
	// associations.
	AppRegistryService = "appregistry"
	// AlarmService is the unit whose committed snapshot proves which
	// resources have alarms.
	AlarmService = "cloudwatch"
)

// Application sources, in the default resolver order.
const (
	AppSourceAppRegistry    = "appregistry"
	AppSourceCloudFormation = "cloudformation"
	AppSourceTags           = model.AppSourceTags
)

// DefaultAppSources is the resolver order when none is configured.
func DefaultAppSources() []string {
	return []string{AppSourceAppRegistry, AppSourceCloudFormation, AppSourceTags}
}

// Query selects part of the snapshot. Every field is optional. AsOf zero
// means the latest committed generations; otherwise each unit answers with
// the generation that was current at AsOf.
type Query struct {
	Account string
	Region  string
	Service string
	Type    string
	App     string
	ARN     string
	AsOf    time.Time
}

// UnitStatus is one unit's answer to a query: its sync state and the
// generation the query read, if any.
type UnitStatus struct {
	Unit        model.Unit      `json:"unit"`
	Sync        model.SyncState `json:"sync"`
	CommittedAt time.Time       `json:"committed_at,omitzero"`
	VerifiedAt  time.Time       `json:"verified_at,omitzero"`
	Count       int             `json:"count"`
	HasData     bool            `json:"has_data"`
}

// ResourcesResult is the Resources answer: resources plus every unit status
// and gap behind them, so a missing unit is never read as zero.
type ResourcesResult struct {
	AsOf      time.Time        `json:"as_of,omitzero"`
	Resources []model.Resource `json:"resources"`
	Units     []UnitStatus     `json:"units"`
	Gaps      []model.Gap      `json:"gaps"`
}

// HealthResult is the Health answer.
type HealthResult struct {
	AsOf   time.Time      `json:"as_of,omitzero"`
	Health []model.Health `json:"health"`
	Units  []UnitStatus   `json:"units"`
	Gaps   []model.Gap    `json:"gaps"`
}

// FlagsResult is the SecurityFlags answer.
type FlagsResult struct {
	AsOf  time.Time            `json:"as_of,omitzero"`
	Flags []model.SecurityFlag `json:"flags"`
	Units []UnitStatus         `json:"units"`
	Gaps  []model.Gap          `json:"gaps"`
}

type accountView struct {
	resources []model.Resource
	health    []model.Health
	flags     []model.SecurityFlag
	units     []UnitStatus
	gaps      []model.Gap
}

type cachedView struct {
	fingerprint string
	view        accountView
}

// units returns the visible units for an account and region as of asOf,
// each answered by the first read organization that holds it.
func (store *Store) units(account, region, service string, asOf time.Time) ([]storage.AWSUnitStatus, error) {
	if !store.ready() {
		return nil, ErrNoStorage
	}
	return store.inventory.AWSUnits(context.Background(), store.scope, storage.AWSUnitQuery{Account: account, Region: region, Service: service, AsOf: asOf})
}

// statusAt describes a unit as the query sees it. A historical query reports
// the generation it found; it cannot know an attempt's state at that time,
// so a unit without a generation then is not checked.
func statusAt(unit storage.AWSUnitStatus, asOf time.Time) (UnitStatus, []model.Gap) {
	status := UnitStatus{Unit: modelUnit(unit.Unit)}
	generation := unit.Generation
	if generation != nil {
		status.CommittedAt = generation.CommittedAt
		status.VerifiedAt = generation.VerifiedAt
		status.Count = generation.Count
		status.HasData = true
	}
	var gaps []model.Gap
	sync, readable := syncOf(unit.Sync)
	if !readable {
		sync.State = model.SyncFailed
		gaps = append(gaps, unreadableGaps(status.Unit, sync.LastGoodAt)...)
	}
	if asOf.IsZero() {
		status.Sync = sync
		switch sync.State {
		case model.SyncFailed:
			if readable {
				gaps = append(gaps, sync.Gaps...)
			}
		case model.SyncPending, "":
			gaps = append(gaps, model.Gap{Account: status.Unit.Account, Region: status.Unit.Region, Service: status.Unit.Service, Reason: model.GapNotChecked})
		default:
			if readable {
				gaps = append(gaps, sync.Gaps...)
			}
		}
	} else if generation != nil {
		// A generation keeps only its latest verification. When that is
		// after asOf, the last verification known at asOf is the commit
		// itself, so a historical read never reports a time after asOf.
		if generation.VerifiedAt.After(asOf) {
			status.VerifiedAt = generation.CommittedAt
		}
		status.Sync = model.SyncState{State: model.SyncOK, LastGoodAt: status.VerifiedAt}
	} else {
		status.Sync = model.SyncState{State: model.SyncPending}
		gaps = append(gaps, model.Gap{Account: status.Unit.Account, Region: status.Unit.Region, Service: status.Unit.Service, Reason: model.GapNotChecked})
	}
	return status, gaps
}

// viewCacheSize is how many assembled account views the store keeps. Views
// are keyed by what they were built from, so many as-of reads that land on
// the same generations share one entry.
const viewCacheSize = 8

// buildAccount assembles one account's view at asOf: resources with merged
// tags, stack membership and resolved applications; health; collector and
// derived security flags; unit statuses and gaps. The answer is always a
// copy the caller may change freely.
func (store *Store) buildAccount(account string, asOf time.Time) (accountView, error) {
	if !store.ready() {
		return accountView{}, ErrNoStorage
	}
	// Units and rows come from one consistent read, so a concurrent commit
	// or purge can never pair one generation's status with another's rows.
	snapshot, err := store.inventory.AWSAccountView(context.Background(), store.scope, account, asOf)
	if err != nil {
		return accountView{}, err
	}
	units := snapshot.Units
	statuses := make([]UnitStatus, 0, len(units))
	gaps := make([]model.Gap, 0)
	sequences := make(map[string]uint64, len(units))
	held := make(map[string]storage.AWSUnitStatus, len(units))
	for _, unit := range units {
		status, unitGaps := statusAt(unit, asOf)
		statuses = append(statuses, status)
		gaps = append(gaps, unitGaps...)
		if unit.Generation == nil {
			continue
		}
		held[status.Unit.Key()] = unit
		sequences[unit.Org+"|"+status.Unit.Key()] = unit.Generation.Seq
		// A historical read reports the gaps the generation was committed
		// with; the latest read already has them in the unit's sync state.
		if !asOf.IsZero() {
			gaps = append(gaps, generationGaps(unit)...)
		}
	}
	options := store.effectiveOptions()
	encoded, err := json.Marshal(struct {
		Latest    bool              `json:"l"`
		Units     []UnitStatus      `json:"u"`
		Gaps      []model.Gap       `json:"g"`
		Sequences map[string]uint64 `json:"s"`
		Sources   []string          `json:"a"`
	}{asOf.IsZero(), statuses, gaps, sequences, options.AppSources})
	if err != nil {
		return accountView{}, err
	}
	sum := sha256.Sum256(encoded)
	viewKey := account + "|" + hex.EncodeToString(sum[:])
	store.mu.Lock()
	if cached, ok := store.views[viewKey]; ok {
		store.touchViewLocked(viewKey)
		store.mu.Unlock()
		return copyView(cached.view), nil
	}
	store.mu.Unlock()

	generations := accountGenerations(snapshot, held)
	view := assemble(generations, options.AppSources)
	view.units = statuses
	view.gaps = gaps

	store.mu.Lock()
	if _, ok := store.views[viewKey]; !ok {
		store.views[viewKey] = cachedView{fingerprint: viewKey, view: view}
		store.viewOrder = append(store.viewOrder, viewKey)
		for len(store.viewOrder) > viewCacheSize {
			delete(store.views, store.viewOrder[0])
			store.viewOrder = store.viewOrder[1:]
		}
	}
	store.mu.Unlock()
	return copyView(view), nil
}

// accountGenerations groups one consistent snapshot's rows into the units
// that held a generation at the query time. Rows of a unit without one
// (history pruned past asOf) are never served.
func accountGenerations(snapshot storage.AWSSnapshotView, held map[string]storage.AWSUnitStatus) map[string]*Generation {
	generations := make(map[string]*Generation, len(held))
	for key, unit := range held {
		generations[key] = &Generation{Seq: unit.Generation.Seq, Unit: modelUnit(unit.Unit), CommittedAt: unit.Generation.CommittedAt, DetailGaps: generationGaps(unit)}
	}
	if len(generations) == 0 {
		return generations
	}
	for _, row := range snapshot.Resources {
		if generation := generations[modelUnit(storage.AWSUnit{Account: row.Account, Region: row.Region, Service: row.Service}).Key()]; generation != nil {
			generation.Resources = append(generation.Resources, resourceOf(row))
		}
	}
	for _, row := range snapshot.Health {
		if generation := generations[modelUnit(storage.AWSUnit{Account: row.Account, Region: row.Region, Service: row.Service}).Key()]; generation != nil {
			generation.Health = append(generation.Health, healthOf(row))
		}
	}
	for _, row := range snapshot.Flags {
		if generation := generations[modelUnit(storage.AWSUnit{Account: row.Account, Region: row.Region, Service: row.Service}).Key()]; generation != nil {
			generation.Flags = append(generation.Flags, flagOf(row))
		}
	}
	return generations
}

// touchViewLocked marks a cached view as most recently used.
func (store *Store) touchViewLocked(key string) {
	for position, existing := range store.viewOrder {
		if existing == key {
			store.viewOrder = append(append(store.viewOrder[:position:position], store.viewOrder[position+1:]...), key)
			return
		}
	}
}

// copyView deep-copies a view so no caller shares the cache's maps or
// slices.
func copyView(view accountView) accountView {
	out := accountView{
		resources: make([]model.Resource, len(view.resources)),
		health:    make([]model.Health, len(view.health)),
		flags:     append([]model.SecurityFlag(nil), view.flags...),
		units:     make([]UnitStatus, len(view.units)),
		gaps:      append([]model.Gap(nil), view.gaps...),
	}
	for index, resource := range view.resources {
		out.resources[index] = cloneResource(resource)
	}
	for index, item := range view.health {
		item.Children = append([]string(nil), item.Children...)
		out.health[index] = item
	}
	for index, status := range view.units {
		status.Sync.Gaps = append([]model.Gap(nil), status.Sync.Gaps...)
		out.units[index] = status
	}
	return out
}

// accounts lists every account visible to the store.
func (store *Store) accounts() ([]string, error) {
	units, err := store.units("", "", "", time.Time{})
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	accounts := make([]string, 0)
	for _, unit := range units {
		if !seen[unit.Unit.Account] {
			seen[unit.Unit.Account] = true
			accounts = append(accounts, unit.Unit.Account)
		}
	}
	sort.Strings(accounts)
	return accounts, nil
}

func (store *Store) accountViews(query Query) ([]accountView, error) {
	accounts := []string{query.Account}
	if query.Account == "" {
		listed, err := store.accounts()
		if err != nil {
			return nil, err
		}
		accounts = listed
	}
	views := make([]accountView, 0, len(accounts))
	allowlist := store.readAllowlist()
	for _, account := range accounts {
		view, err := store.buildAccount(account, query.AsOf)
		if err != nil {
			return nil, err
		}
		if allowlist != nil {
			// The view is the caller's copy; the cache keeps what was stored.
			for index := range view.resources {
				model.RedactName(&view.resources[index], allowlist)
			}
		}
		views = append(views, view)
	}
	return views, nil
}

// readAllowlist is the live tag-value allowlist as a set; nil when the store
// has none wired.
func (store *Store) readAllowlist() map[string]bool {
	if store.allowlist == nil {
		return nil
	}
	keys := store.allowlist()
	set := make(map[string]bool, len(keys))
	for _, key := range keys {
		set[key] = true
	}
	return set
}

func matchUnit(query Query, unit model.Unit) bool {
	if query.Region != "" && unit.Region != query.Region {
		return false
	}
	if query.Service != "" && unit.Service != query.Service {
		return false
	}
	return true
}

func matchResource(query Query, resource model.Resource) bool {
	if query.Region != "" && resource.Region != query.Region {
		return false
	}
	if query.Service != "" && resource.Service != query.Service {
		return false
	}
	if query.Type != "" && resource.Type != query.Type {
		return false
	}
	if query.App != "" && resource.App != query.App {
		return false
	}
	if query.ARN != "" && resource.ARN != query.ARN {
		return false
	}
	return true
}

func dataUnit(service string) bool {
	return service != ChangeService
}

func filterUnits(query Query, statuses []UnitStatus, gaps []model.Gap) ([]UnitStatus, []model.Gap) {
	units := make([]UnitStatus, 0)
	for _, status := range statuses {
		if dataUnit(status.Unit.Service) && matchUnit(query, status.Unit) {
			units = append(units, status)
		}
	}
	kept := make([]model.Gap, 0)
	for _, gap := range gaps {
		if dataUnit(gap.Service) && matchUnit(query, model.Unit{Account: gap.Account, Region: gap.Region, Service: gap.Service}) {
			kept = append(kept, gap)
		}
	}
	return units, kept
}

// Resources lists resources matching the query as of query.AsOf, with the
// status and gaps of every unit behind them.
func (store *Store) Resources(_ context.Context, query Query) (ResourcesResult, error) {
	views, err := store.accountViews(query)
	if err != nil {
		return ResourcesResult{}, err
	}
	result := ResourcesResult{AsOf: query.AsOf, Resources: []model.Resource{}}
	for _, view := range views {
		for _, resource := range view.resources {
			if matchResource(query, resource) {
				result.Resources = append(result.Resources, resource)
			}
		}
		units, gaps := filterUnits(query, view.units, view.gaps)
		result.Units = append(result.Units, units...)
		result.Gaps = append(result.Gaps, gaps...)
	}
	if result.Units == nil {
		result.Units = []UnitStatus{}
	}
	if result.Gaps == nil {
		result.Gaps = []model.Gap{}
	}
	return result, nil
}

// Resource returns one resource by ARN as of asOf, with the status of the
// unit that holds it.
func (store *Store) Resource(ctx context.Context, arn string, asOf time.Time) (model.Resource, UnitStatus, bool, error) {
	account := accountFromARN(arn)
	result, err := store.Resources(ctx, Query{Account: account, ARN: arn, AsOf: asOf})
	if err != nil || len(result.Resources) == 0 {
		return model.Resource{}, UnitStatus{}, false, err
	}
	resource := result.Resources[0]
	for _, status := range result.Units {
		if status.Unit.Service == resource.Service && (status.Unit.Region == resource.Region || status.Unit.Region == model.GlobalRegion) {
			return resource, status, true, nil
		}
	}
	return resource, UnitStatus{}, true, nil
}

// Health lists health signals matching the query. Service filters on the
// unit that produced the signal; ARN matches the affected resource or the
// alarm or event itself; App matches the affected resource's application.
func (store *Store) Health(_ context.Context, query Query) (HealthResult, error) {
	views, err := store.accountViews(query)
	if err != nil {
		return HealthResult{}, err
	}
	result := HealthResult{AsOf: query.AsOf, Health: []model.Health{}}
	for _, view := range views {
		apps := make(map[string]string, len(view.resources))
		for _, resource := range view.resources {
			apps[resource.ARN] = resource.App
		}
		for _, item := range view.health {
			if query.Region != "" && item.Region != query.Region {
				continue
			}
			if query.Service != "" && healthService(item) != query.Service {
				continue
			}
			if query.ARN != "" && item.Resource != query.ARN && item.Ref != query.ARN {
				continue
			}
			if query.App != "" && apps[item.Resource] != query.App {
				continue
			}
			result.Health = append(result.Health, item)
		}
		units, gaps := filterUnits(query, view.units, view.gaps)
		result.Units = append(result.Units, units...)
		result.Gaps = append(result.Gaps, gaps...)
	}
	if result.Units == nil {
		result.Units = []UnitStatus{}
	}
	if result.Gaps == nil {
		result.Gaps = []model.Gap{}
	}
	return result, nil
}

// SecurityFlags lists collector and derived flags matching the query.
func (store *Store) SecurityFlags(_ context.Context, query Query) (FlagsResult, error) {
	views, err := store.accountViews(query)
	if err != nil {
		return FlagsResult{}, err
	}
	result := FlagsResult{AsOf: query.AsOf, Flags: []model.SecurityFlag{}}
	for _, view := range views {
		byARN := make(map[string]model.Resource, len(view.resources))
		for _, resource := range view.resources {
			byARN[resource.ARN] = resource
		}
		for _, flag := range view.flags {
			resource := byARN[flag.Resource]
			if query.Region != "" && flag.Region != query.Region {
				continue
			}
			if query.Service != "" && resource.Service != query.Service {
				continue
			}
			if query.ARN != "" && flag.Resource != query.ARN {
				continue
			}
			if query.App != "" && resource.App != query.App {
				continue
			}
			result.Flags = append(result.Flags, flag)
		}
		units, gaps := filterUnits(query, view.units, view.gaps)
		result.Units = append(result.Units, units...)
		result.Gaps = append(result.Gaps, gaps...)
	}
	if result.Units == nil {
		result.Units = []UnitStatus{}
	}
	if result.Gaps == nil {
		result.Gaps = []model.Gap{}
	}
	return result, nil
}

// Sync returns the status of every unit matching the query, including the
// change poller.
func (store *Store) Sync(_ context.Context, query Query) ([]UnitStatus, error) {
	units, err := store.units(query.Account, query.Region, query.Service, query.AsOf)
	if err != nil {
		return nil, err
	}
	statuses := make([]UnitStatus, 0, len(units))
	for _, unit := range units {
		status, _ := statusAt(unit, query.AsOf)
		statuses = append(statuses, status)
	}
	return statuses, nil
}

func healthService(item model.Health) string {
	switch item.Source {
	case model.SourceAlarm:
		return "cloudwatch"
	case model.SourceInstanceStatus:
		return "ec2"
	case model.SourceTargetHealth:
		return "elbv2"
	case model.SourceRollout:
		return "ecs"
	case model.SourceDatabaseEvent:
		return "rds"
	case model.SourceAWSHealth:
		return "aws-health"
	case model.SourceStackStatus:
		return "cloudformation"
	}
	return ""
}

func accountFromARN(arn string) string {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) < 6 {
		return ""
	}
	return parts[4]
}
