package issues

import (
	"errors"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

// EventKind is what happened to an issue.
type EventKind string

// Event kinds. An evaluator reopen of a rejected issue is a regress; a
// user's undo of a rejection is a reopen.
const (
	EventOpened       EventKind = storage.AWSIssueEventOpened
	EventAcknowledged EventKind = storage.AWSIssueEventAcknowledged
	EventRejected     EventKind = storage.AWSIssueEventRejected
	EventReopened     EventKind = storage.AWSIssueEventReopened
	EventResolved     EventKind = storage.AWSIssueEventResolved
	EventRegressed    EventKind = storage.AWSIssueEventRegressed
	EventAlerted      EventKind = storage.AWSIssueEventAlerted
	EventAlertGivenUp EventKind = storage.AWSIssueEventAlertGivenUp
	EventReread       EventKind = storage.AWSIssueEventReread
)

// EvaluatorActor is the actor of every event the evaluator writes.
const EvaluatorActor = "evaluator"

// Event is one thing that happened to an issue, and why.
type Event struct {
	IssueID string `json:"issue_id"`
	// Seq orders a stored issue's events as they were written; their times
	// come from each replica's clock and may disagree.
	Seq   int64     `json:"seq,omitempty"`
	At    time.Time `json:"at"`
	Kind  EventKind `json:"kind"`
	Actor string    `json:"actor,omitempty"`
	// Detail says why. For a rejection it is the user's reason.
	Detail string `json:"detail,omitempty"`
}

// Assessment is what one evaluation could say about one rule on one
// account.
type Assessment string

// Assessments.
const (
	// AssessComplete: every input was read, so a finding that is gone is
	// resolved.
	AssessComplete Assessment = "complete"
	// AssessPartial: an input was capped. What was found is real; what
	// was not found cannot be confirmed gone.
	AssessPartial Assessment = "partial"
	// AssessNotAssessed: an input is off in AWS or not read. The rule
	// opens nothing and resolves nothing.
	AssessNotAssessed Assessment = "not_assessed"
)

// Assess decides what one evaluation of rule can say about the whole
// account from the view's reads, for an account-wide issue, and names the
// inputs that were not read in full.
func Assess(rule Rule, view View) (Assessment, []string) {
	return AssessIn(rule, view, "")
}

// AssessIn decides what one evaluation of rule can say in one Region from
// that Region's reads (or the input's global read). A gap in another Region
// does not count. An empty region assesses the whole account.
func AssessIn(rule Rule, view View, region string) (Assessment, []string) {
	assessment := AssessComplete
	var missing []string
	where := ""
	if region != "" {
		where = " in " + region
	}
	for _, input := range rule.Inputs() {
		switch read := view.ReadIn(input.Service, region); read {
		case ReadOK, ReadNotOffered:
		case ReadCapped:
			missing = append(missing, input.Service+" capped"+where)
			if assessment == AssessComplete {
				assessment = AssessPartial
			}
		case ReadOffInAWS:
			missing = append(missing, input.Service+" off in AWS"+where)
			assessment = AssessNotAssessed
		default:
			missing = append(missing, input.Service+" not read"+where)
			assessment = AssessNotAssessed
		}
	}
	return assessment, missing
}

// Assessed is what one evaluation could say about a rule in one place.
type Assessed struct {
	Assessment Assessment
	// Missing names the inputs that were not read in full.
	Missing []string
	// Baseline marks the rule's first assessment there: what it finds
	// existed when first read.
	Baseline bool
}

// RuleOutcome is one rule's evaluation on one account.
type RuleOutcome struct {
	// Rule is the rule's current ID.
	Rule string
	// StableID is the ID its issues are keyed on; empty is Rule.
	StableID string
	// Names are every ID the rule's issues may carry (Rule and its old
	// IDs); empty is Rule.
	Names []string
	Kind  Kind
	// Assessment, Missing and Baseline are for the whole account: they
	// decide issues with no Region.
	Assessment Assessment
	Missing    []string
	Baseline   bool
	// Regions decide issues in each Region. A Region missing from a
	// non-nil map was not read. A nil map applies the account's
	// assessment everywhere.
	Regions  map[string]Assessed
	Findings []Finding
}

// at is what the outcome says about an issue in region.
func (outcome RuleOutcome) at(region string) Assessed {
	if region == "" || outcome.Regions == nil {
		return Assessed{Assessment: outcome.Assessment, Missing: outcome.Missing, Baseline: outcome.Baseline}
	}
	if assessed, ok := outcome.Regions[region]; ok {
		return assessed
	}
	return Assessed{Assessment: AssessNotAssessed, Missing: []string{region + " not read"}}
}

func (outcome RuleOutcome) stableID() string {
	if outcome.StableID != "" {
		return outcome.StableID
	}
	return outcome.Rule
}

func (outcome RuleOutcome) owns(issue Issue) bool {
	if issue.Rule == outcome.Rule {
		return true
	}
	return slices.Contains(outcome.Names, issue.Rule)
}

// OwnerKey is what an issue ID hashes for an owner on an account: an ARN
// as it is, since it names its account, and any other key scoped to the
// account, so "root" on two accounts is two issues.
func OwnerKey(account, owner string) string {
	if strings.HasPrefix(owner, "arn:") {
		return owner
	}
	return account + "/" + owner
}

// Cycle is the input of one evaluation of one account.
type Cycle struct {
	Account string
	Now     time.Time
	// Removed are Regions the configuration no longer reads. Their open
	// issues resolve as no longer read, their rejections are kept, and
	// nothing opens there.
	Removed map[string]bool
	// Issues are the account's stored issues.
	Issues   []Issue
	Outcomes []RuleOutcome
}

// Change is one issue an evaluation writes, with the events that say why.
// Before is the issue as read, or nil for a new one.
type Change struct {
	Issue  Issue
	Before *Issue
	Events []Event
}

// NotAssessed is a rule that could not be assessed on an account, in one
// Region or (Region empty) account-wide.
type NotAssessed struct {
	Rule    string   `json:"rule"`
	Region  string   `json:"region,omitempty"`
	Missing []string `json:"missing"`
}

// Result is what one evaluation changes.
type Result struct {
	Changes     []Change
	NotAssessed []NotAssessed
}

// Advance applies one evaluation to an account's issues. It is a pure
// function of its input: the same cycle always gives the same result, and
// an issue that does not change is not in it.
//
// A finding with no issue opens one, unless its rule was not assessed
// where it is. An issue whose finding is gone resolves only when the rule
// was assessed in full where the issue is (its Region, or the account for
// an account-wide issue); otherwise it stays and cannot be confirmed. A
// resolved issue found again regresses. A rejected issue stays rejected
// through volatile changes and through a resolve; it regresses when its
// material digest changes, its severity rises above the severity it was
// rejected at, or its review date comes.
func Advance(cycle Cycle) Result {
	now := cycle.Now.UTC()
	byID := make(map[string]Issue, len(cycle.Issues))
	for _, issue := range cycle.Issues {
		byID[issue.ID] = issue
	}
	work := &advance{now: now, before: byID, after: map[string]Issue{}, events: map[string][]Event{}}
	for _, issue := range cycle.Issues {
		if cycle.Removed[issue.Region] && issue.State.Active() {
			issue.State, issue.Confirm, issue.ResolvedAt = StateResolved, Confirmed, now
			work.put(issue, EventResolved, "Region no longer read")
		}
	}
	var result Result
	for _, outcome := range cycle.Outcomes {
		result.NotAssessed = append(result.NotAssessed, notAssessed(outcome)...)
		found := map[string]bool{}
		for _, finding := range strongest(outcome.Findings) {
			where := outcome.at(finding.Region)
			if where.Assessment == AssessNotAssessed || cycle.Removed[finding.Region] {
				continue
			}
			id := IssueID(outcome.stableID(), OwnerKey(cycle.Account, finding.Owner))
			found[id] = true
			issue, ok := work.current(id)
			if !ok {
				work.open(cycle, outcome, where.Baseline, id, finding)
				continue
			}
			work.match(outcome, where.Baseline, issue, finding)
		}
		for _, existing := range cycle.Issues {
			if !outcome.owns(existing) || found[existing.ID] || cycle.Removed[existing.Region] {
				continue
			}
			issue, _ := work.current(existing.ID)
			work.unmatched(outcome.Rule, outcome.at(issue.Region), issue)
		}
	}
	for _, existing := range cycle.Issues {
		issue, _ := work.current(existing.ID)
		if issue.State == StateRejected && !cycle.Removed[issue.Region] && reviewDue(issue.Rejection, now) {
			work.regress(issue, "rejected issue reopened: its review date "+issue.Rejection.ReviewOn+" came")
		}
	}
	result.Changes = work.changes()
	return result
}

// notAssessed lists where an outcome could not be assessed, in Region
// order.
func notAssessed(outcome RuleOutcome) []NotAssessed {
	var out []NotAssessed
	if outcome.Assessment == AssessNotAssessed {
		out = append(out, NotAssessed{Rule: outcome.Rule, Missing: slices.Clone(outcome.Missing)})
	}
	regions := make([]string, 0, len(outcome.Regions))
	for region := range outcome.Regions {
		regions = append(regions, region)
	}
	sort.Strings(regions)
	for _, region := range regions {
		if assessed := outcome.Regions[region]; assessed.Assessment == AssessNotAssessed {
			out = append(out, NotAssessed{Rule: outcome.Rule, Region: region, Missing: slices.Clone(assessed.Missing)})
		}
	}
	return out
}

// Disconnect resolves the issues of an account that is no longer
// connected: open, acknowledged and regressed issues resolve, whether or
// not they could be confirmed. Rejected issues, and resolved ones that keep
// a rejection, stay as they are, so reconnecting the account does not
// raise them again.
func Disconnect(account string, list []Issue, now time.Time) []Change {
	work := &advance{now: now.UTC(), before: map[string]Issue{}, after: map[string]Issue{}, events: map[string][]Event{}}
	for _, issue := range list {
		if issue.Account != account {
			continue
		}
		work.before[issue.ID] = issue
		if !issue.State.Active() {
			continue
		}
		issue.State, issue.Confirm, issue.ResolvedAt = StateResolved, Confirmed, work.now
		work.put(issue, EventResolved, "account no longer connected")
	}
	return work.changes()
}

// strongest keeps one finding per owner, the most severe, so a rule that
// reports an owner twice still raises one issue. The order is stable.
func strongest(findings []Finding) []Finding {
	index := map[string]int{}
	out := make([]Finding, 0, len(findings))
	for _, finding := range findings {
		if finding.Owner == "" || !finding.Severity.Valid() {
			continue
		}
		if at, ok := index[finding.Owner]; ok {
			if finding.Severity.Rank() > out[at].Severity.Rank() {
				out[at] = finding
			}
			continue
		}
		index[finding.Owner] = len(out)
		out = append(out, finding)
	}
	return out
}

// advance is the working set of one Advance call.
type advance struct {
	now    time.Time
	before map[string]Issue
	after  map[string]Issue
	events map[string][]Event
	order  []string
}

func (work *advance) current(id string) (Issue, bool) {
	if issue, ok := work.after[id]; ok {
		return issue, true
	}
	issue, ok := work.before[id]
	return issue, ok
}

func (work *advance) put(issue Issue, kind EventKind, detail string) {
	if _, ok := work.after[issue.ID]; !ok {
		work.order = append(work.order, issue.ID)
	}
	work.after[issue.ID] = issue
	if kind != "" {
		work.events[issue.ID] = append(work.events[issue.ID], Event{IssueID: issue.ID, At: work.now, Kind: kind, Actor: EvaluatorActor, Detail: detail})
	}
}

// apply copies a finding's current facts onto an issue.
func apply(issue *Issue, finding Finding) {
	issue.Severity = finding.Severity
	issue.Tier = finding.Tier
	if issue.Tier == "" {
		issue.Tier = TierFor(finding.Severity)
	}
	issue.Region = finding.Region
	issue.GroupID = finding.GroupID
	issue.Title = finding.Title
	issue.Evidence = slices.Clone(finding.Evidence)
	issue.Steps = slices.Clone(finding.Steps)
	issue.HygieneCount = finding.HygieneCount
	issue.Digest = finding.Material.Digest()
	redactIssueText(issue)
}

func (work *advance) open(cycle Cycle, outcome RuleOutcome, baseline bool, id string, finding Finding) {
	issue := Issue{
		ID: id, Rule: outcome.Rule, Kind: outcome.Kind, Account: cycle.Account, Owner: finding.Owner,
		State: StateOpen, Confirm: Confirmed, FirstSeen: work.now, OpenedAt: work.now,
	}
	apply(&issue, finding)
	detail := "found by rule " + outcome.Rule
	switch {
	case baseline:
		// The rule's first assessment here cannot tell when a problem
		// started.
		detail = "existed when first read"
	case !finding.Since.IsZero():
		issue.Since, issue.SinceKnown = finding.Since.UTC(), true
	default:
		issue.Since, issue.SinceKnown = work.now, true
	}
	work.put(issue, EventOpened, detail)
}

func (work *advance) match(outcome RuleOutcome, baseline bool, issue Issue, finding Finding) {
	issue.Rule = outcome.Rule
	wasCant := issue.Confirm == CantConfirm
	switch {
	case issue.State.Active():
		apply(&issue, finding)
		issue.Confirm = Confirmed
		if wasCant {
			work.put(issue, EventReread, "read again: still found")
			return
		}
		work.put(issue, "", "")
	case issue.State == StateRejected || issue.State == StateResolved && issue.Rejection != nil:
		reasons := reopenReasons(issue.Rejection, finding, work.now)
		wasResolved := issue.State == StateResolved
		apply(&issue, finding)
		issue.Confirm = Confirmed
		issue.ResolvedAt = time.Time{}
		if len(reasons) > 0 {
			issue.State, issue.Rejection = StateRegressed, nil
			issue.Since, issue.SinceKnown = work.now, true
			work.put(issue, EventRegressed, "rejected issue reopened: "+strings.Join(reasons, "; "))
			return
		}
		issue.State = StateRejected
		switch {
		case wasResolved:
			work.put(issue, EventReread, "found again with the same evidence: still rejected")
		case wasCant:
			work.put(issue, EventReread, "read again: still found, still rejected")
		default:
			work.put(issue, "", "")
		}
	case issue.State == StateResolved && baseline:
		// The first assessment after a reconnect, or after a Region is
		// read again, cannot tell a return from a problem that was there
		// all along: it opens again as first read, never as a regress.
		apply(&issue, finding)
		issue.State, issue.Confirm, issue.ResolvedAt = StateOpen, Confirmed, time.Time{}
		issue.OpenedAt, issue.Since, issue.SinceKnown = work.now, time.Time{}, false
		work.put(issue, EventOpened, "existed when first read")
	case issue.State == StateResolved:
		apply(&issue, finding)
		issue.State, issue.Confirm, issue.ResolvedAt = StateRegressed, Confirmed, time.Time{}
		// The problem started again now; FirstSeen keeps its first
		// occurrence.
		issue.Since, issue.SinceKnown = work.now, true
		work.put(issue, EventRegressed, "found again after it resolved")
	}
}

func (work *advance) unmatched(rule string, where Assessed, issue Issue) {
	if !issue.State.Active() && issue.State != StateRejected {
		return
	}
	issue.Rule = rule
	if where.Assessment == AssessComplete {
		issue.State, issue.Confirm, issue.ResolvedAt = StateResolved, Confirmed, work.now
		detail := "not found in a complete read"
		if issue.Rejection != nil {
			detail += "; the rejection is kept"
		}
		work.put(issue, EventResolved, detail)
		return
	}
	if issue.Confirm == CantConfirm {
		return
	}
	issue.Confirm = CantConfirm
	detail := "can't confirm"
	if len(where.Missing) > 0 {
		detail += ": " + strings.Join(where.Missing, ", ")
	}
	work.put(issue, EventReread, detail)
}

func (work *advance) regress(issue Issue, detail string) {
	issue.State, issue.Rejection = StateRegressed, nil
	issue.Since, issue.SinceKnown = work.now, true
	work.put(issue, EventRegressed, detail)
}

// changes returns every issue that changed, in the order first touched.
func (work *advance) changes() []Change {
	out := make([]Change, 0, len(work.order))
	for _, id := range work.order {
		issue := work.after[id]
		var before *Issue
		if previous, ok := work.before[id]; ok {
			copied := previous
			before = &copied
			if len(work.events[id]) == 0 && sameIssue(previous, issue) {
				continue
			}
			issue.Version = previous.Version + 1
		} else {
			issue.Version = 1
		}
		issue.UpdatedAt = work.now
		out = append(out, Change{Issue: issue, Before: before, Events: work.events[id]})
	}
	return out
}

// sameIssue compares two issues apart from their write bookkeeping.
func sameIssue(a, b Issue) bool {
	a.Version, b.Version = 0, 0
	a.UpdatedAt, b.UpdatedAt = time.Time{}, time.Time{}
	a.Org, b.Org = "", ""
	return reflect.DeepEqual(normalized(a), normalized(b))
}

// normalized treats an empty slice and a nil one alike.
func normalized(issue Issue) Issue {
	if len(issue.Evidence) == 0 {
		issue.Evidence = nil
	}
	if len(issue.Steps) == 0 {
		issue.Steps = nil
	}
	return issue
}

// reopenReasons lists why a rejection no longer holds for a finding.
func reopenReasons(rejection *Rejection, finding Finding, now time.Time) []string {
	if rejection == nil {
		return nil
	}
	var reasons []string
	if finding.Material.Digest() != rejection.Digest {
		reasons = append(reasons, "its material evidence changed")
	}
	if finding.Severity.Rank() > rejection.SeverityAtReject.Rank() {
		reasons = append(reasons, "its severity rose from "+string(rejection.SeverityAtReject)+" to "+string(finding.Severity))
	}
	if reviewDue(rejection, now) {
		reasons = append(reasons, "its review date "+rejection.ReviewOn+" came")
	}
	return reasons
}

// reviewDue reports whether a rejection's review date has come: from the
// start of that day in UTC.
func reviewDue(rejection *Rejection, now time.Time) bool {
	if rejection == nil || rejection.ReviewOn == "" {
		return false
	}
	day, err := time.Parse(time.DateOnly, rejection.ReviewOn)
	return err == nil && !now.Before(day)
}

// User action errors.
var (
	// ErrNotFound is an issue ID that is not stored.
	ErrNotFound = errors.New("issues: issue not found")
	// ErrAlreadyRejected is a reject of a rejected issue.
	ErrAlreadyRejected = errors.New("issues: issue is already rejected")
	// ErrNotRejected is a reopen of an issue that is not rejected.
	ErrNotRejected = errors.New("issues: issue is not rejected")
	// ErrNotActive is an acknowledge or reject of an issue that is not
	// open: resolved issues have nothing to act on.
	ErrNotActive = errors.New("issues: issue is not open")
	// ErrInvalidReviewDate is a review date that is not a day after today.
	ErrInvalidReviewDate = errors.New("issues: the review date must be a day after today, YYYY-MM-DD")
	// ErrInvalidReason is a reason that is not plain text of 3 to 500
	// characters.
	ErrInvalidReason = storage.ErrInvalidAWSIssueReason
	// ErrInvalidActor is an action with no actor.
	ErrInvalidActor = errors.New("issues: an action needs the user who made it")
)

// Acknowledge marks an open or regressed issue as seen. Acknowledging an
// acknowledged issue changes nothing.
func Acknowledge(issue Issue, actor string, now time.Time) (Issue, []Event, error) {
	if strings.TrimSpace(actor) == "" {
		return Issue{}, nil, ErrInvalidActor
	}
	switch issue.State {
	case StateAcknowledged:
		return issue, nil, nil
	case StateOpen, StateRegressed:
	default:
		return Issue{}, nil, ErrNotActive
	}
	issue.State = StateAcknowledged
	return issue, []Event{{IssueID: issue.ID, At: now.UTC(), Kind: EventAcknowledged, Actor: actor, Detail: "acknowledged"}}, nil
}

// Reject records a user's decision that an issue does not fit how the
// company works. The reason is required; reviewOn is an optional day after
// today, YYYY-MM-DD. The rejection holds the issue's current material
// digest and severity.
func Reject(issue Issue, actor, reason, reviewOn string, now time.Time) (Issue, []Event, error) {
	if strings.TrimSpace(actor) == "" {
		return Issue{}, nil, ErrInvalidActor
	}
	clean, err := storage.CleanAWSIssueReason(reason)
	if err != nil {
		return Issue{}, nil, err
	}
	if reviewOn != "" {
		day, err := time.Parse(time.DateOnly, reviewOn)
		if err != nil || day.Format(time.DateOnly) != reviewOn || !day.After(now.UTC()) {
			return Issue{}, nil, ErrInvalidReviewDate
		}
	}
	switch {
	case issue.State == StateRejected:
		return Issue{}, nil, ErrAlreadyRejected
	case !issue.State.Active():
		return Issue{}, nil, ErrNotActive
	}
	now = now.UTC()
	issue.State = StateRejected
	issue.Rejection = &Rejection{Reason: clean, By: actor, At: now, ReviewOn: reviewOn, Digest: issue.Digest, SeverityAtReject: issue.Severity}
	return issue, []Event{{IssueID: issue.ID, At: now, Kind: EventRejected, Actor: actor, Detail: clean}}, nil
}

// Reopen undoes a rejection. The reason is optional; when given it is held
// to the same rules as a rejection's.
func Reopen(issue Issue, actor, reason string, now time.Time) (Issue, []Event, error) {
	if strings.TrimSpace(actor) == "" {
		return Issue{}, nil, ErrInvalidActor
	}
	detail := "rejection undone"
	if strings.TrimSpace(reason) != "" {
		clean, err := storage.CleanAWSIssueReason(reason)
		if err != nil {
			return Issue{}, nil, err
		}
		detail = clean
	}
	if issue.State != StateRejected {
		return Issue{}, nil, ErrNotRejected
	}
	issue.State, issue.Rejection = StateOpen, nil
	return issue, []Event{{IssueID: issue.ID, At: now.UTC(), Kind: EventReopened, Actor: actor, Detail: detail}}, nil
}

// sortIssues orders issues by account, then ID.
func sortIssues(list []Issue) {
	sort.Slice(list, func(left, right int) bool {
		if list[left].Account != list[right].Account {
			return list[left].Account < list[right].Account
		}
		return list[left].ID < list[right].ID
	})
}
