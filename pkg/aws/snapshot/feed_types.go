package snapshot

import (
	"context"
	"time"

	"github.com/VersusControl/versus-incident/pkg/aws/model"
)

// Item kinds of the change list. Bursts, stack groups and routine counts
// are formed before paging, so a page never splits one.
const (
	ItemChange  = "change"
	ItemBurst   = "burst"
	ItemGroup   = "group"
	ItemRoutine = "routine"
)

// Bucket states.
const (
	BucketComplete = "complete"
	BucketGap      = "gap"
	BucketArriving = "arriving"
	// BucketPartial: some pairs in scope were read and some were not; the
	// counts are what was read, a lower bound.
	BucketPartial = "partial"
)

// Region states in the region filter.
const (
	RegionSelected   = "selected"
	RegionNotEnabled = "not_enabled"
	RegionGlobal     = "global"
)

// Suspect statuses.
const (
	SuspectFound        = "found"
	SuspectNone         = "none"
	SuspectStartUnknown = "start_unknown"
	SuspectIncomplete   = "incomplete"
	SuspectNoAlarm      = "no_alarm"
	// SuspectResourceUnknown: the alarm's resources do not resolve to
	// canonical ARNs, so no change can be matched.
	SuspectResourceUnknown = "resource_unknown"
)

// FeedWindow is the window the answer covers. SettledUntil is the earliest
// coverage end across the pairs in scope: after it, changes are still
// arriving and are shown as such, never as none.
type FeedWindow struct {
	Since        time.Time `json:"since"`
	Until        time.Time `json:"until"`
	SettledUntil time.Time `json:"settled_until"`
}

// FeedActorCount is one principal's change count.
type FeedActorCount struct {
	Actor      string `json:"actor"`
	ActorClass string `json:"actor_class"`
	Count      int64  `json:"count"`
}

// FeedSummary is the answer line's numbers.
type FeedSummary struct {
	Total             int64            `json:"total"`
	TotalIsLowerBound bool             `json:"total_is_lower_bound"`
	ByActorClass      map[string]int64 `json:"by_actor_class"`
	People            int              `json:"people"`
	Routine           int64            `json:"routine"`
	Errors            int64            `json:"errors"`
	Truncated         bool             `json:"truncated"`
	TopActors         []FeedActorCount `json:"top_actors"`
	LastChangeBefore  *time.Time       `json:"last_change_before"`
}

// FeedBucket is one column of the activity grid, in UTC.
type FeedBucket struct {
	Start      time.Time `json:"start"`
	Pipeline   int64     `json:"pipeline"`
	Person     int64     `json:"person"`
	AWSService int64     `json:"aws_service"`
	Unknown    int64     `json:"unknown"`
	Errors     int64     `json:"errors"`
	State      string    `json:"state"`
	// GapAccounts are the accounts with a real gap in the bucket.
	GapAccounts []string `json:"gap_accounts"`
}

// FeedAlarm is one alarm on the alarm line.
type FeedAlarm struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Account      string     `json:"account"`
	Region       string     `json:"region"`
	ResourceARNs []string   `json:"resource_arns"`
	Start        *time.Time `json:"start"`
	StartKnown   bool       `json:"start_known"`
	End          *time.Time `json:"end"`
}

// FeedSearched is what a suspect search looked at.
type FeedSearched struct {
	Accounts []string   `json:"accounts"`
	Regions  []string   `json:"regions"`
	From     *time.Time `json:"from"`
	To       *time.Time `json:"to"`
}

// FeedSuspect is the change that most likely started an alarm.
type FeedSuspect struct {
	Status   string        `json:"status"`
	ChangeID string        `json:"change_id,omitempty"`
	AlarmID  string        `json:"alarm_id,omitempty"`
	Searched *FeedSearched `json:"searched,omitempty"`
	Change   *FeedItem     `json:"change"`
	// Candidates are the ranked changes, best first; tied ones are all
	// marked and listed most recent first. ChangeID is the first.
	Candidates []FeedSuspectCandidate `json:"candidates,omitempty"`
	// InFilter is set when the request names a service filter view: false
	// when the suspect change is in a service the filter hides. The
	// suspect is always chosen over every service.
	InFilter *bool `json:"in_filter,omitzero"`
}

// FeedServiceCount is one service's successful changes in the window and
// scope, counted without the service filter.
type FeedServiceCount struct {
	Service string `json:"service"`
	Label   string `json:"label"`
	Count   int64  `json:"count"`
}

// FeedSuspectCandidate is one ranked suspect change.
type FeedSuspectCandidate struct {
	ChangeID   string `json:"change_id"`
	Confidence string `json:"confidence"`
	Tied       bool   `json:"tied"`
}

// FeedGap is one stretch of one pair that was not read, with why.
type FeedGap struct {
	Account           string    `json:"account"`
	Region            string    `json:"region"`
	From              time.Time `json:"from"`
	To                time.Time `json:"to"`
	Reason            string    `json:"reason"`
	MissingPermission *string   `json:"missing_permission"`
	// EventsPerHour is the event rate of a too_busy stretch, when known.
	EventsPerHour *int64 `json:"events_per_hour,omitempty"`

	// confirmedOff marks a stretch of a region Settings recorded as not
	// enabled: listed, never a real gap. A region_not_enabled reason from
	// a failed poll is a real gap.
	confirmedOff bool
}

// FeedAccount is one account's completeness.
type FeedAccount struct {
	Account  string     `json:"account"`
	Name     string     `json:"name"`
	Complete bool       `json:"complete"`
	Reason   *string    `json:"reason"`
	AsOf     *time.Time `json:"as_of"`
	// DisconnectedAt is when Settings recorded the account disconnected;
	// nil while it is connected.
	DisconnectedAt *time.Time `json:"disconnected_at"`
}

// FeedRegion is one region in scope.
type FeedRegion struct {
	Region   string `json:"region"`
	State    string `json:"state"`
	Complete bool   `json:"complete"`
}

// FeedSync is the poller's state for the scope.
type FeedSync struct {
	LastPollAt       *time.Time `json:"last_poll_at"`
	LateArrivals     int64      `json:"late_arrivals"`
	ClockSkewSeconds int64      `json:"clock_skew_seconds"`
	FirstReadPending bool       `json:"first_read_pending"`
	// Dropped counts malformed events skipped at ingest.
	Dropped int64 `json:"dropped"`
	// NotChanges counts events CloudTrail flagged as writes that change
	// nothing (sign-ins, tokens, reads); they are not stored.
	NotChanges int64 `json:"not_changes"`
}

// FeedField is one field of a change, with its before value when known.
type FeedField struct {
	Field             string  `json:"field"`
	Before            *string `json:"before"`
	After             *string `json:"after"`
	BeforeStatus      string  `json:"before_status"`
	MissingPermission *string `json:"missing_permission"`
}

// Before statuses of a field.
const (
	BeforeKnown     = "known"
	BeforeNewOnly   = "new_only"
	BeforeHidden    = "hidden"
	BeforeAmbiguous = "ambiguous"
)

// FeedItem is one row of the change list.
type FeedItem struct {
	Kind       string      `json:"kind"`
	ID         string      `json:"id"`
	Time       time.Time   `json:"time"`
	EndTime    *time.Time  `json:"end_time"`
	Account    string      `json:"account"`
	Region     string      `json:"region"`
	Service    string      `json:"service"`
	EventName  string      `json:"event_name"`
	ActorClass string      `json:"actor_class"`
	Actor      string      `json:"actor"`
	Summary    string      `json:"summary"`
	Resources  []string    `json:"resources"`
	Fields     []FeedField `json:"fields"`
	ErrorCode  *string     `json:"error_code"`
	Count      *int64      `json:"count"`

	// anchor orders the list; it is a group's first time, so a member that
	// arrives later never moves the group across a page boundary.
	anchor time.Time
}

// ChangesAnswer is the GET /api/admin/aws/changes answer.
type ChangesAnswer struct {
	Window   FeedWindow   `json:"window"`
	Complete bool         `json:"complete"`
	Summary  FeedSummary  `json:"summary"`
	Buckets  []FeedBucket `json:"buckets"`
	// BucketSeconds is the width of each bucket, so the page never infers
	// the step from the window.
	BucketSeconds      int64         `json:"bucket_seconds"`
	Alarms             []FeedAlarm   `json:"alarms"`
	AlarmHistoryStarts *time.Time    `json:"alarm_history_starts"`
	Suspect            FeedSuspect   `json:"suspect"`
	Gaps               []FeedGap     `json:"gaps"`
	Accounts           []FeedAccount `json:"accounts"`
	Regions            []FeedRegion  `json:"regions"`
	Sync               FeedSync      `json:"sync"`
	Items              []FeedItem    `json:"items"`
	NextCursor         *string       `json:"next_cursor"`
	// Services is set when the request asks for the service facet.
	Services []FeedServiceCount `json:"services,omitzero"`
	// Limits are the window and compare limits this request was held to.
	Limits ChangesLimitsView `json:"limits"`
}

// GroupAnswer is the GET /api/admin/aws/changes/groups/{id} answer.
type GroupAnswer struct {
	Items      []FeedItem `json:"items"`
	NextCursor *string    `json:"next_cursor"`
}

// ScopeAccount is one connected account as settings know it now. The
// expected scope comes from here, not from stored rows, so a region added a
// moment ago is a gap until its first poll.
type ScopeAccount struct {
	Account string
	Name    string
	// Regions are the selected regions that can be read.
	Regions []string
	// NotEnabled are selected regions AWS reports as not enabled.
	NotEnabled []string
	// Outage, when set, is why the account cannot be read now
	// (account_disconnected, credentials_invalid, assume_role_failed,
	// account_mismatch); OutageSince is when it started.
	Outage      ChangeGapReason
	OutageSince time.Time
	// Disconnected is set when Settings records the account disconnected:
	// the account reads account_disconnected with its disconnect time,
	// whatever the role failure behind it.
	Disconnected bool
	// DeniedServices and NotOnPlanServices are the services Settings found
	// the reader role cannot read, or the account's plan does not offer.
	// Compare names them with that reason and leaves them out of its scope.
	DeniedServices    []string
	NotOnPlanServices []string
	// LastReadAt is when the account was last checked successfully; the
	// as-of time of an outage when no change coverage says more.
	LastReadAt time.Time
}

// ScopeFunc lists the connected accounts. A nil ScopeFunc, or one that
// reports ok=false, falls back to the pairs the store holds.
type ScopeFunc func(ctx context.Context) (accounts []ScopeAccount, ok bool, err error)

// SuspectRequest is what the suspect engine needs: the window's changes
// (without their fields; Load returns the full changes for IDs), the
// alarms, and the coverage the answer reports.
type SuspectRequest struct {
	// Account and Region are the answer's filters; empty means all.
	Account      string
	Region       string
	Changes      []model.Change
	Load         func(ctx context.Context, ids []string) ([]model.Change, error)
	Alarms       []FeedAlarm
	Gaps         []FeedGap
	SettledUntil time.Time
	Complete     bool
	Since        time.Time
	Until        time.Time
	// Settled reports whether [from, to) of one account and region (empty:
	// every region) is read and settled as far as this answer knows: it
	// ends by SettledUntil and no real gap of the answer overlaps it. The
	// part before Since is the engine's own read to judge.
	Settled func(account, region string, from, to time.Time) bool
}

// AlarmScope is the scope of the alarm line.
type AlarmScope struct {
	Account string
	Region  string
	Since   time.Time
	Until   time.Time
}

// AlarmLine is the optional half of a suspect engine that also lists the
// alarms in scope and where the alarm history starts (nil when unknown).
type AlarmLine interface {
	AlarmLine(ctx context.Context, scope AlarmScope) (alarms []FeedAlarm, historyStarts *time.Time, err error)
}

// SuspectEngine picks the change that most likely started an alarm. The
// answer's Change is filled by the feed from ChangeID.
type SuspectEngine interface {
	Suspect(ctx context.Context, request SuspectRequest) FeedSuspect
}

// noSuspect is the engine until one is wired.
type noSuspect struct{}

func (noSuspect) Suspect(context.Context, SuspectRequest) FeedSuspect {
	return FeedSuspect{Status: SuspectNoAlarm}
}

// FieldResolver fills a change's before values. Until one is wired, every
// field shows its new value only.
type FieldResolver interface {
	Fields(ctx context.Context, change model.Change) []FeedField
}
