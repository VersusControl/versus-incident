package collect

import (
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
	"github.com/VersusControl/versus-incident/pkg/aws/snapshot"
)

// Default cadences. Every call is an event the customer's GuardDuty, extra
// trail copies and log pipeline price, so a unit runs no more often than
// what it reads changes: health often, and faster while something is
// wrong; inventory in full a few times a day, with the change feed driving
// re-reads of what changed in between.
const (
	// DefaultHealthInterval is how often a healthy health unit runs.
	DefaultHealthInterval = 10 * time.Minute
	// DefaultHealthHotInterval is how often a health unit runs while it
	// is unhealthy or its service changed recently.
	DefaultHealthHotInterval = 2 * time.Minute
	// DefaultHotFor is how long a health unit stays on the fast cadence
	// after it was last unhealthy or changed.
	DefaultHotFor = time.Hour
	// MaxHot is the longest a health unit stays on the fast cadence in one
	// spell: a unit that stays unhealthy for longer goes back to the
	// normal cadence, so one long outage does not cost five times the
	// calls all day.
	MaxHot = 6 * time.Hour
	// FeedFallbackInterval is the inventory cadence of a Region whose
	// change feed cannot drive re-reads (denied, capped or more than
	// FeedLagLimit behind): the full read cadence of before the feed.
	FeedFallbackInterval = 15 * time.Minute
	// FeedLagLimit is how far behind a Region's change feed may fall
	// before its inventory falls back to FeedFallbackInterval.
	FeedLagLimit = 2 * time.Hour
	// DefaultInventoryInterval is how often every inventory unit is read
	// in full.
	DefaultInventoryInterval = 6 * time.Hour
	// DefaultIdentityInterval is how often the account's identities are
	// read.
	DefaultIdentityInterval = time.Hour
	// DefaultTrailsInterval is how often the account's trails are read.
	DefaultTrailsInterval = 24 * time.Hour
	// DefaultRegionOffInterval is how often a unit runs in a Region that
	// is not turned on, or for a feature the account's plan lacks.
	DefaultRegionOffInterval = 24 * time.Hour
	// DefaultCredentialReportInterval and DefaultFindingsInterval are the
	// cadences of the credential report and imported findings.
	DefaultCredentialReportInterval = 4 * time.Hour
	DefaultFindingsInterval         = time.Hour
)

// Intervals is how often each kind of unit is re-read. A zero field takes
// its default.
type Intervals struct {
	// Inventory is the full inventory read; the change feed re-reads
	// changed units in between.
	Inventory time.Duration
	// Health is the cadence of a healthy health unit.
	Health time.Duration
	// Changes is the CloudTrail poll.
	Changes time.Duration
	// HealthHot is the cadence of an unhealthy or recently changed health
	// unit; it is never slower than Health.
	HealthHot time.Duration
	// HotFor is how long a unit stays hot once it is clear.
	HotFor time.Duration
	// Identity is the IAM read.
	Identity time.Duration
	// Policies is the resource policy read; never more often than the
	// full inventory.
	Policies time.Duration
	// Trails is the trail and event selector read.
	Trails time.Duration
	// RegionsOff is the cadence of a unit in a Region that is not turned
	// on, or one whose feature is not on the account's plan.
	RegionsOff time.Duration
	// CredentialReport and Findings are the cadences of those sources.
	CredentialReport time.Duration
	Findings         time.Duration
}

func orDefault(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}

// Effective fills every zero field with its default.
func (intervals Intervals) Effective() Intervals {
	out := intervals
	out.Inventory = orDefault(intervals.Inventory, DefaultInventoryInterval)
	out.Health = orDefault(intervals.Health, DefaultHealthInterval)
	out.Changes = orDefault(intervals.Changes, 5*time.Minute)
	out.HealthHot = min(orDefault(intervals.HealthHot, DefaultHealthHotInterval), out.Health)
	out.HotFor = orDefault(intervals.HotFor, DefaultHotFor)
	out.Identity = orDefault(intervals.Identity, DefaultIdentityInterval)
	// A policy interval left unset follows a slower inventory; one that
	// is set is the user's, and configuration refuses one below the
	// inventory interval.
	out.Policies = orDefault(intervals.Policies, max(PolicyInterval, out.Inventory))
	out.Trails = orDefault(intervals.Trails, DefaultTrailsInterval)
	out.RegionsOff = orDefault(intervals.RegionsOff, DefaultRegionOffInterval)
	out.CredentialReport = orDefault(intervals.CredentialReport, DefaultCredentialReportInterval)
	out.Findings = orDefault(intervals.Findings, DefaultFindingsInterval)
	return out
}

func (intervals Intervals) of(kind Kind) time.Duration {
	effective := intervals.Effective()
	switch kind {
	case KindHealth:
		return effective.Health
	case KindChanges:
		return effective.Changes
	case KindPolicy:
		return effective.Policies
	default:
		return effective.Inventory
	}
}

// unitState is what the scheduler knows of one unit from its own runs.
type unitState struct {
	// last is when the unit last ran.
	last time.Time
	// hotUntil keeps a health unit on the fast cadence: an hour after it
	// was last unhealthy or its service last changed. hotSince is when the
	// current spell started.
	hotUntil time.Time
	hotSince time.Time
	// off is set while the unit's Region is not turned on, or its feature
	// is not on the account's plan.
	off bool
	// changed is set when the change feed showed a change the unit has
	// not read yet. It clears only when a run commits.
	changed bool
	// failures counts the runs in a row that did not commit, and retryAt
	// is when the next is due: a short, growing backoff, never the full
	// interval.
	failures int
	retryAt  time.Time
}

// heat puts a health unit on the fast cadence until until, starting a new
// spell when it was not hot at now.
func (state *unitState) heat(now, until time.Time) {
	if !now.Before(state.hotUntil) {
		state.hotSince = now
	}
	if until.After(state.hotUntil) {
		state.hotUntil = until
	}
}

// hot reports whether a health unit is on the fast cadence at now: inside
// its spell, and for no longer than MaxHot.
func (state *unitState) hot(now time.Time) bool {
	return state != nil && now.Before(state.hotUntil) && now.Sub(state.hotSince) < MaxHot
}

// retryBackoff is how long a unit waits after failures runs in a row did
// not commit: health from 2 minutes, inventory from 15, doubling, capped
// at the health interval and at an hour, and never longer than the unit's
// own interval.
func retryBackoff(unit model.Unit, failures int, intervals Intervals, interval time.Duration) time.Duration {
	effective := intervals.Effective()
	base, ceiling := FeedFallbackInterval, time.Hour
	switch {
	case unit.Service == snapshot.ChangeService:
		base, ceiling = effective.Changes, effective.Changes
	case KindOf(unit.Service) == KindHealth:
		base, ceiling = effective.HealthHot, effective.Health
	}
	wait := base
	for step := 1; step < failures && wait < ceiling; step++ {
		wait *= 2
	}
	return min(wait, ceiling, interval)
}

// UnitCadence is how often one unit runs at a moment: the interval in
// force and why.
type UnitCadence struct {
	Interval time.Duration
	// Hot is set when a health unit is on the fast cadence.
	Hot bool
	// Stretched is set when the daily call target slowed the unit down.
	Stretched bool
	// FeedDown is set when the Region's change feed cannot drive re-reads
	// and the full read runs on the fallback cadence.
	FeedDown bool
}

// cadenceOf is the interval a unit runs at. factor stretches the inventory
// and policy cadences when the account is over its daily call target;
// health, changes and identity are never stretched.
//
// feedDown is set when the change feed of the unit's Region cannot drive
// re-reads: inventory then falls back to FeedFallbackInterval.
func cadenceOf(unit model.Unit, state *unitState, intervals Intervals, factor int, now time.Time, feedDown ...bool) UnitCadence {
	effective := intervals.Effective()
	factor = max(factor, 1)
	if state != nil && state.off {
		return UnitCadence{Interval: effective.RegionsOff}
	}
	switch {
	case unit.Service == snapshot.ChangeService:
		return UnitCadence{Interval: effective.Changes}
	case unit.Service == identityService:
		return UnitCadence{Interval: effective.Identity}
	case unit.Service == serviceTrails:
		return UnitCadence{Interval: effective.Trails}
	}
	switch KindOf(unit.Service) {
	case KindHealth:
		if state.hot(now) && effective.HealthHot < effective.Health {
			return UnitCadence{Interval: effective.HealthHot, Hot: true}
		}
		return UnitCadence{Interval: effective.Health}
	case KindPolicy:
		return UnitCadence{Interval: effective.Policies * time.Duration(factor), Stretched: factor > 1}
	}
	interval := effective.Inventory * time.Duration(factor)
	if len(feedDown) > 0 && feedDown[0] && interval > FeedFallbackInterval {
		return UnitCadence{Interval: FeedFallbackInterval, FeedDown: true}
	}
	return UnitCadence{Interval: interval, Stretched: factor > 1}
}

// StretchFactor is how much slower the inventory and policy reads run for
// an account that made calls24h requests in the last 24 hours against a
// daily target. At or under the target nothing is stretched.
func StretchFactor(calls24h int64, target int) int {
	if target <= 0 || calls24h <= int64(target) {
		return 1
	}
	ratio := float64(calls24h) / float64(target)
	switch {
	case ratio <= 1.5:
		return 2
	case ratio <= 3:
		return 4
	}
	return 8
}

// StretchedSources are the sources the daily target slows down first;
// health is protected.
func StretchedSources() []string { return []string{SourceInventory, SourcePolicies} }

// derivedUnit is a unit a change to another service's API can make stale,
// for the events that can.
type derivedUnit struct {
	service string
	events  func(name string) bool
}

// changeUnits are the units a change re-reads beside the unit named after
// its service: the EC2 API also answers the network and transit gateway
// reads, and S3 the bucket policy read. Instance churn (an autoscaler
// launching nodes) is an EC2 change too, so only network events re-read
// the network units.
var changeUnits = map[string][]derivedUnit{
	"ec2": {
		{service: "vpc", events: networkEvent},
		{service: serviceTransitGateway, events: func(name string) bool { return strings.Contains(name, "TransitGateway") }},
	},
	"s3": {{service: serviceS3Policy, events: policyEvent}},
}

// networkWords name the EC2 events that change a network: VPCs, subnets,
// routes, gateways, security groups, network ACLs, endpoints and peering.
var networkWords = []string{"Vpc", "Subnet", "Route", "Gateway", "SecurityGroup", "NetworkAcl", "Peering", "Endpoint", "Address"}

func networkEvent(name string) bool {
	for _, word := range networkWords {
		if strings.Contains(name, word) {
			return true
		}
	}
	return false
}

// policyEvent reports whether an event can change a resource policy or the
// set of resources that have one. Policy reads are metered, so other
// changes to the service wait for the policy cadence.
func policyEvent(name string) bool {
	return strings.Contains(name, "Policy") || strings.HasPrefix(name, "Create") || strings.HasPrefix(name, "Delete") ||
		strings.HasPrefix(name, "ScheduleKeyDeletion") || strings.HasPrefix(name, "CancelKeyDeletion")
}

// serviceAliases are services whose changes the change list names after
// the API, where the collector reads them under another unit.
var serviceAliases = map[string]string{
	"servicecatalog":       snapshot.AppRegistryService,
	"cloudtrail":           serviceTrails,
	"elasticloadbalancing": "elbv2",
	"monitoring":           "cloudwatch",
	"autoscaling":          "auto-scaling",
	"events":               "eventbridge",
	"states":               "step-functions",
	"apigateway":           "api-gateway",
	"health":               "aws-health",
}

// unitsForChange lists the units a change makes stale, in the account.
// The collector re-reads a whole unit, so a change it cannot pin to one
// resource still refreshes everything that resource could be.
func unitsForChange(change model.Change) []model.Unit {
	service := change.Service
	if alias, ok := serviceAliases[service]; ok {
		service = alias
	}
	services := []string{service}
	for _, derived := range changeUnits[service] {
		if derived.events(change.EventName) {
			services = append(services, derived.service)
		}
	}
	if strings.Contains(change.EventName, "Tag") {
		services = append(services, snapshot.TagService)
	}
	var out []model.Unit
	for _, service := range services {
		for _, entry := range specs {
			if entry.service != service {
				continue
			}
			if entry.kind == KindPolicy && !policyEvent(change.EventName) {
				continue
			}
			region := change.Region
			if entry.global {
				region = model.GlobalRegion
			}
			out = append(out, model.Unit{Account: change.Account, Region: region, Service: service})
		}
	}
	return out
}
