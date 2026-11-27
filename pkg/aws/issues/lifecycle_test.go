package issues

import (
	"errors"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	testAccount = "123400000004"
	testRuleID  = "public-bucket"
	bucketARN   = "arn:aws:s3:::docs-site"
)

// history applies evaluations to an account's issues the way the
// evaluator does, keeping every event.
type history struct {
	t        *testing.T
	now      time.Time
	baseline bool
	issues   map[string]Issue
	events   []Event
}

func newHistory(t *testing.T, at time.Time) *history {
	return &history{t: t, now: at, baseline: true, issues: map[string]Issue{}}
}

func (h *history) stored() []Issue {
	out := make([]Issue, 0, len(h.issues))
	for _, issue := range h.issues {
		out = append(out, issue)
	}
	sort.Slice(out, func(left, right int) bool { return out[left].ID < out[right].ID })
	return out
}

// cycle runs one evaluation of outcome at h.now, then moves the clock a
// minute on.
func (h *history) cycle(outcome RuleOutcome) Result {
	h.t.Helper()
	if h.baseline && outcome.Assessment != AssessNotAssessed {
		outcome.Baseline = true
		h.baseline = false
	}
	result := Advance(Cycle{Account: testAccount, Now: h.now, Issues: h.stored(), Outcomes: []RuleOutcome{outcome}})
	for _, change := range result.Changes {
		before, existed := h.issues[change.Issue.ID]
		if existed && change.Issue.Version != before.Version+1 || !existed && change.Issue.Version != 1 {
			h.t.Fatalf("version %d after %d", change.Issue.Version, before.Version)
		}
		h.issues[change.Issue.ID] = change.Issue
		h.events = append(h.events, change.Events...)
	}
	h.now = h.now.Add(time.Minute)
	return result
}

func (h *history) only() Issue {
	h.t.Helper()
	if len(h.issues) != 1 {
		h.t.Fatalf("issues = %+v, want one", h.issues)
	}
	return h.stored()[0]
}

func (h *history) reject(reason, reviewOn string) {
	h.t.Helper()
	issue := h.only()
	after, events, err := Reject(issue, "ana@example.com", reason, reviewOn, h.now)
	if err != nil {
		h.t.Fatal(err)
	}
	after.Version++
	h.issues[after.ID] = after
	h.events = append(h.events, events...)
}

func (h *history) count(kind EventKind) int {
	n := 0
	for _, event := range h.events {
		if event.Kind == kind {
			n++
		}
	}
	return n
}

func bucketFinding(principals string, severity Severity) Finding {
	return Finding{
		Owner: bucketARN, Region: "eu-west-1", Severity: severity, Title: "Bucket docs-site is public",
		Evidence: []Evidence{{Text: "bucket policy allows *", Status: EvidenceProven, Source: "s3"}},
		Material: Material{"principals": principals},
	}
}

func complete(findings ...Finding) RuleOutcome {
	return RuleOutcome{Rule: testRuleID, Kind: KindSecurity, Assessment: AssessComplete, Findings: findings}
}

func notRead(findings ...Finding) RuleOutcome {
	return RuleOutcome{Rule: testRuleID, Kind: KindSecurity, Assessment: AssessNotAssessed, Missing: []string{"s3 not read"}, Findings: findings}
}

var start = time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)

// The first evaluation of an account cannot tell when a problem started:
// what it finds existed when first read. A finding that appears later
// started at that evaluation.
func TestFirstEvaluationIsABaseline(t *testing.T) {
	h := newHistory(t, start)
	h.cycle(complete(bucketFinding("*", SeverityHigh)))
	first := h.only()
	if first.SinceKnown || !first.Since.IsZero() || first.State != StateOpen || len(h.events) != 1 ||
		h.events[0].Kind != EventOpened || h.events[0].Detail != "existed when first read" {
		t.Fatalf("baseline issue = %+v, events %+v", first, h.events)
	}
	later := bucketFinding("*", SeverityHigh)
	later.Owner = "arn:aws:s3:::uploads"
	h.cycle(complete(bucketFinding("*", SeverityHigh), later))
	second := h.issues[IssueID(testRuleID, later.Owner)]
	if !second.SinceKnown || !second.Since.Equal(start.Add(time.Minute)) || h.events[1].Detail == "existed when first read" {
		t.Fatalf("later issue = %+v, events %+v", second, h.events)
	}
}

// An issue that flaps opens, resolves and regresses with exactly one event
// per transition, and nothing on a cycle that changes nothing.
func TestFlappingGivesOneEventPerTransition(t *testing.T) {
	h := newHistory(t, start)
	random := rand.New(rand.NewSource(7))
	transitions := 0
	present := false
	for cycle := 0; cycle < 100; cycle++ {
		next := random.Intn(3) == 0
		if cycle == 0 {
			next = true
		}
		if next != present {
			transitions++
		}
		present = next
		before := len(h.events)
		if present {
			h.cycle(complete(bucketFinding("*", SeverityCritical)))
		} else {
			h.cycle(complete())
		}
		added := len(h.events) - before
		if added > 1 {
			t.Fatalf("cycle %d wrote %d events: %+v", cycle, added, h.events[before:])
		}
	}
	if transitions < 20 {
		t.Fatalf("the flapping pattern changed only %d times", transitions)
	}
	if len(h.events) != transitions {
		t.Fatalf("%d transitions gave %d events", transitions, len(h.events))
	}
	if h.count(EventOpened) != 1 || h.count(EventResolved)+h.count(EventRegressed) != transitions-1 {
		t.Fatalf("event kinds: opened %d, resolved %d, regressed %d of %d transitions",
			h.count(EventOpened), h.count(EventResolved), h.count(EventRegressed), transitions)
	}
}

// When a permission is revoked mid-run the data behind every open issue is
// no longer read: each turns to can't confirm, none resolves, and the rule
// opens nothing new. A capped read can't confirm either. Once the read
// works again a finding that is gone resolves.
func TestRevokedPermissionTurnsIssuesCantConfirm(t *testing.T) {
	h := newHistory(t, start)
	other := bucketFinding("*", SeverityHigh)
	other.Owner = "arn:aws:s3:::uploads"
	h.cycle(complete(bucketFinding("*", SeverityHigh), other))
	if len(h.issues) != 2 {
		t.Fatalf("issues = %+v", h.issues)
	}
	newcomer := bucketFinding("*", SeverityHigh)
	newcomer.Owner = "arn:aws:s3:::new"
	for range 3 {
		h.cycle(notRead(newcomer))
	}
	for _, issue := range h.issues {
		if issue.State != StateOpen || issue.Confirm != CantConfirm {
			t.Fatalf("after the permission was revoked: %+v", issue)
		}
	}
	if h.count(EventResolved) != 0 || h.count(EventOpened) != 2 || h.count(EventReread) != 2 {
		t.Fatalf("events while not read = %+v", h.events)
	}
	if !strings.Contains(h.events[len(h.events)-1].Detail, "s3 not read") {
		t.Fatalf("the can't-confirm event does not say why: %+v", h.events[len(h.events)-1])
	}

	capped := RuleOutcome{Rule: testRuleID, Kind: KindSecurity, Assessment: AssessPartial, Missing: []string{"s3 capped"}, Findings: []Finding{other}}
	h.cycle(capped)
	if h.issues[IssueID(testRuleID, bucketARN)].State != StateOpen || h.issues[IssueID(testRuleID, other.Owner)].Confirm != Confirmed {
		t.Fatalf("after a capped read: %+v", h.issues)
	}
	h.cycle(complete(other))
	if gone := h.issues[IssueID(testRuleID, bucketARN)]; gone.State != StateResolved || gone.Confirm != Confirmed {
		t.Fatalf("after a complete read: %+v", gone)
	}
}

// A rejection with a review date reopens when that day comes, on the
// evaluator's clock, and says so. One without a date holds.
func TestReviewDateReopensARejection(t *testing.T) {
	h := newHistory(t, start)
	h.cycle(complete(bucketFinding("*", SeverityHigh)))
	h.reject("public on purpose: docs site", "2026-12-01")
	h.now = time.Date(2026, 11, 30, 23, 59, 0, 0, time.UTC)
	h.cycle(complete(bucketFinding("*", SeverityHigh)))
	if h.only().State != StateRejected || h.count(EventRegressed) != 0 {
		t.Fatalf("before the review date: %+v", h.only())
	}
	h.cycle(complete(bucketFinding("*", SeverityHigh)))
	issue := h.only()
	if issue.State != StateRegressed || issue.Rejection != nil || h.count(EventRegressed) != 1 ||
		!strings.Contains(h.events[len(h.events)-1].Detail, "review date 2026-12-01") {
		t.Fatalf("on the review date: %+v, events %+v", issue, h.events)
	}

	// The review date also comes for an issue whose rule is not assessed.
	h = newHistory(t, start)
	h.cycle(complete(bucketFinding("*", SeverityHigh)))
	h.reject("public on purpose: docs site", "2026-11-26")
	h.now = time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	h.cycle(notRead())
	if h.only().State != StateRegressed {
		t.Fatalf("review date while not read: %+v", h.only())
	}

	h = newHistory(t, start)
	h.cycle(complete(bucketFinding("*", SeverityHigh)))
	h.reject("public on purpose: docs site", "")
	h.now = start.AddDate(3, 0, 0)
	h.cycle(complete(bucketFinding("*", SeverityHigh)))
	if h.only().State != StateRejected {
		t.Fatalf("a rejection with no review date lapsed: %+v", h.only())
	}
}

// Volatile fields never reopen a rejection; one change of material
// evidence reopens it exactly once.
func TestOnlyMaterialChangesReopenARejection(t *testing.T) {
	h := newHistory(t, start)
	h.cycle(complete(bucketFinding("*", SeverityHigh)))
	h.reject("public on purpose: docs site", "")
	rejectedEvents := len(h.events)
	for cycle := 0; cycle < 100; cycle++ {
		finding := bucketFinding("*", SeverityHigh)
		finding.Title = "Bucket docs-site is public (" + string(rune('a'+cycle%26)) + ")"
		finding.HygieneCount = cycle
		finding.Evidence[0].AsOf = h.now
		finding.Evidence = append(finding.Evidence, Evidence{Text: "objects: " + strings.Repeat("x", cycle%5), Status: EvidenceProven})
		h.cycle(complete(finding))
	}
	if h.count(EventRegressed) != 0 || len(h.events) != rejectedEvents || h.only().State != StateRejected {
		t.Fatalf("volatile cycles reopened it: %+v", h.events[rejectedEvents:])
	}
	h.cycle(complete(bucketFinding("*, arn:aws:iam::999900000000:root", SeverityHigh)))
	for range 10 {
		h.cycle(complete(bucketFinding("*, arn:aws:iam::999900000000:root", SeverityHigh)))
	}
	if h.count(EventRegressed) != 1 || h.only().State != StateRegressed ||
		!strings.Contains(h.events[len(h.events)-1].Detail, "material evidence changed") {
		t.Fatalf("after one material change: %+v", h.events[rejectedEvents:])
	}
}

// A rejection reopens when the severity rises above the one it was
// rejected at, not when it falls.
func TestSeverityRiseReopensARejection(t *testing.T) {
	h := newHistory(t, start)
	h.cycle(complete(bucketFinding("*", SeverityHigh)))
	h.reject("public on purpose: docs site", "")
	h.cycle(complete(bucketFinding("*", SeverityMedium)))
	h.cycle(complete(bucketFinding("*", SeverityHigh)))
	if h.only().State != StateRejected {
		t.Fatalf("a lower or equal severity reopened it: %+v", h.only())
	}
	h.cycle(complete(bucketFinding("*", SeverityCritical)))
	if h.only().State != StateRegressed || !strings.Contains(h.events[len(h.events)-1].Detail, "severity rose from high to critical") {
		t.Fatalf("a higher severity: %+v, %+v", h.only(), h.events)
	}
}

// A rejection survives a resolve and a later match with the same
// evidence; a match with other evidence regresses.
func TestRejectionSurvivesResolve(t *testing.T) {
	h := newHistory(t, start)
	h.cycle(complete(bucketFinding("*", SeverityHigh)))
	h.reject("public on purpose: docs site", "")
	h.cycle(complete())
	if issue := h.only(); issue.State != StateResolved || issue.Rejection == nil {
		t.Fatalf("resolved: %+v", issue)
	}
	h.cycle(complete(bucketFinding("*", SeverityHigh)))
	if issue := h.only(); issue.State != StateRejected || issue.Rejection == nil || h.count(EventRegressed) != 0 {
		t.Fatalf("matched again with the same evidence: %+v", issue)
	}
	h.cycle(complete())
	h.cycle(complete(bucketFinding("arn:aws:iam::999900000000:root", SeverityHigh)))
	if issue := h.only(); issue.State != StateRegressed || issue.Rejection != nil || h.count(EventRegressed) != 1 {
		t.Fatalf("matched again with other evidence: %+v", issue)
	}
}

// A reject needs an actor, a plain-text reason of 3 to 500 characters and
// a review date after today; a secret in the reason is redacted; a
// rejected or resolved issue cannot be rejected; reopen undoes it.
func TestRejectValidatesItsInput(t *testing.T) {
	open := Issue{ID: IssueID(testRuleID, bucketARN), State: StateOpen, Severity: SeverityHigh, Digest: "d"}
	for name, call := range map[string]func() error{
		"short reason": func() error { _, _, err := Reject(open, "ana", "ok", "", start); return err },
		"long reason":  func() error { _, _, err := Reject(open, "ana", strings.Repeat("a", 501), "", start); return err },
		"control":      func() error { _, _, err := Reject(open, "ana", "fine\x1b[31m", "", start); return err },
		"no actor":     func() error { _, _, err := Reject(open, " ", "public on purpose", "", start); return err },
		"past date":    func() error { _, _, err := Reject(open, "ana", "public on purpose", "2026-11-25", start); return err },
		"bad date":     func() error { _, _, err := Reject(open, "ana", "public on purpose", "2026-13-01", start); return err },
		"loose date":   func() error { _, _, err := Reject(open, "ana", "public on purpose", "2026-12-1", start); return err },
		"resolved": func() error {
			_, _, err := Reject(Issue{State: StateResolved}, "ana", "public on purpose", "", start)
			return err
		},
		"twice": func() error {
			_, _, err := Reject(Issue{State: StateRejected}, "ana", "public on purpose", "", start)
			return err
		},
		"reopen open":   func() error { _, _, err := Reopen(open, "ana", "", start); return err },
		"ack resolved":  func() error { _, _, err := Acknowledge(Issue{State: StateResolved}, "ana", start); return err },
		"reopen reason": func() error { _, _, err := Reopen(Issue{State: StateRejected}, "ana", "‮", start); return err },
	} {
		if call() == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	rejected, events, err := Reject(open, "ana", "  docs site, see https://wiki.example.com/x?token=abc  ", "2026-12-01", start)
	if err != nil {
		t.Fatal(err)
	}
	if rejected.State != StateRejected || rejected.Rejection.Reason != "docs site, see https://wiki.example.com" ||
		rejected.Rejection.Digest != "d" || rejected.Rejection.SeverityAtReject != SeverityHigh || rejected.Rejection.ReviewOn != "2026-12-01" ||
		len(events) != 1 || events[0].Detail != rejected.Rejection.Reason || events[0].Actor != "ana" {
		t.Fatalf("rejected = %+v, events %+v", rejected, events)
	}
	reopened, events, err := Reopen(rejected, "bo", "", start)
	if err != nil || reopened.State != StateOpen || reopened.Rejection != nil || len(events) != 1 || events[0].Kind != EventReopened {
		t.Fatalf("reopened = %+v, %+v, %v", reopened, events, err)
	}
	if _, _, err := Reject(Issue{State: StateRejected}, "ana", "public on purpose", "", start); !errors.Is(err, ErrAlreadyRejected) {
		t.Fatalf("second reject = %v", err)
	}
}

// An issue in a Region resolves on a complete read of that Region, whatever
// another Region's reads; one whose Region was not read can't be confirmed
// and nothing opens there. An account-wide issue follows the account's
// reads, so any gap keeps it.
func TestRegionGapDoesNotBlockResolve(t *testing.T) {
	inEU := bucketFinding("*", SeverityHigh)
	inUS := bucketFinding("*", SeverityHigh)
	inUS.Owner, inUS.Region = "arn:aws:s3:::us-bucket", "us-east-1"
	root := Finding{Owner: "root", Severity: SeverityCritical, Title: "Root has an access key", Material: Material{"key": "1"}}
	h := newHistory(t, start)
	h.cycle(complete(inEU, inUS, root))
	if len(h.issues) != 3 {
		t.Fatalf("issues = %+v", h.issues)
	}
	usNotRead := RuleOutcome{Rule: testRuleID, Kind: KindSecurity, Assessment: AssessNotAssessed, Missing: []string{"s3 not read in us-east-1"},
		Regions: map[string]Assessed{
			"eu-west-1": {Assessment: AssessComplete},
			"us-east-1": {Assessment: AssessNotAssessed, Missing: []string{"s3 not read in us-east-1"}},
		}}
	newcomer := bucketFinding("*", SeverityHigh)
	newcomer.Owner, newcomer.Region = "arn:aws:s3:::new-us", "us-east-1"
	usNotRead.Findings = []Finding{newcomer}
	h.cycle(usNotRead)
	if got := h.issues[IssueID(testRuleID, bucketARN)]; got.State != StateResolved {
		t.Fatalf("the eu-west-1 issue did not resolve on its complete read: %+v", got)
	}
	if got := h.issues[IssueID(testRuleID, inUS.Owner)]; got.State != StateOpen || got.Confirm != CantConfirm {
		t.Fatalf("the us-east-1 issue = %+v", got)
	}
	if got := h.issues[IssueID(testRuleID, OwnerKey(testAccount, "root"))]; got.State != StateOpen || got.Confirm != CantConfirm {
		t.Fatalf("the account-wide issue = %+v", got)
	}
	if _, ok := h.issues[IssueID(testRuleID, newcomer.Owner)]; ok {
		t.Fatal("an issue opened in a Region that was not read")
	}
	result := Advance(Cycle{Account: testAccount, Now: h.now, Issues: h.stored(), Outcomes: []RuleOutcome{usNotRead}})
	if len(result.NotAssessed) != 2 || result.NotAssessed[1].Region != "us-east-1" {
		t.Fatalf("not assessed = %+v", result.NotAssessed)
	}
}

// An owner that is not an ARN is scoped to its account, so "root" on two
// accounts is two issues.
func TestNonARNOwnersAreScopedToTheAccount(t *testing.T) {
	root := Finding{Owner: "root", Severity: SeverityCritical, Title: "Root has an access key", Material: Material{"key": "1"}}
	ids := map[string]bool{}
	for _, account := range []string{"123400000007", "123400000008"} {
		result := Advance(Cycle{Account: account, Now: start, Outcomes: []RuleOutcome{complete(root)}})
		if len(result.Changes) != 1 || result.Changes[0].Issue.ID != IssueID(testRuleID, account+"/root") || result.Changes[0].Issue.Owner != "root" {
			t.Fatalf("account %s: %+v", account, result.Changes)
		}
		ids[result.Changes[0].Issue.ID] = true
	}
	if len(ids) != 2 {
		t.Fatal("two accounts' root findings share one issue")
	}
	if OwnerKey("123400000007", bucketARN) != bucketARN {
		t.Fatal("an ARN owner was rescoped")
	}
}

// A regress starts the issue's Since again at the regression, so the
// likely cause anchors there; FirstSeen keeps the first occurrence.
func TestRegressRestartsSince(t *testing.T) {
	h := newHistory(t, start)
	h.cycle(complete(bucketFinding("*", SeverityHigh)))
	h.cycle(complete())
	regressedAt := h.now
	h.cycle(complete(bucketFinding("*", SeverityHigh)))
	issue := h.only()
	if issue.State != StateRegressed || !issue.SinceKnown || !issue.Since.Equal(regressedAt) || !issue.FirstSeen.Equal(start) {
		t.Fatalf("after a regress from resolved: %+v", issue)
	}
	h.reject("public on purpose: docs site", "")
	reopenedAt := h.now
	h.cycle(complete(bucketFinding("*, arn:aws:iam::999900000000:root", SeverityHigh)))
	issue = h.only()
	if issue.State != StateRegressed || !issue.Since.Equal(reopenedAt) || !issue.FirstSeen.Equal(start) {
		t.Fatalf("after a regress from rejected: %+v", issue)
	}
}

// An account that is no longer connected has its open issues resolved,
// can't confirm or not; its rejected issues stay rejected.
func TestDisconnectResolvesOpenIssuesOnly(t *testing.T) {
	list := []Issue{
		{ID: "a", Account: testAccount, State: StateOpen, Confirm: CantConfirm, Version: 1},
		{ID: "b", Account: testAccount, State: StateAcknowledged, Confirm: Confirmed, Version: 3},
		{ID: "c", Account: testAccount, State: StateRejected, Rejection: &Rejection{Reason: "on purpose"}, Version: 2},
		{ID: "d", Account: testAccount, State: StateResolved, Version: 2},
	}
	changes := Disconnect(testAccount, list, start)
	if len(changes) != 2 {
		t.Fatalf("changes = %+v", changes)
	}
	for _, change := range changes {
		if change.Issue.State != StateResolved || change.Issue.Version != change.Before.Version+1 || len(change.Events) != 1 ||
			change.Events[0].Detail != "account no longer connected" {
			t.Fatalf("change = %+v", change)
		}
	}
}
