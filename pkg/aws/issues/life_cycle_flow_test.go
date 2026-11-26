package issues

import (
	"strings"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
)

const (
	docsBucket    = "arn:aws:s3:::flow-docs"
	uploadsBucket = "arn:aws:s3:::flow-uploads"
)

func bucketRule() *scriptedRule {
	return &scriptedRule{id: "flow-public-bucket", inputs: []Input{{Service: "s3"}}}
}

// One issue goes through the whole life cycle through the evaluator and
// the user actions on every backend: a baseline open, an acknowledge, a
// reject, a resolve that keeps the rejection, a match with the same
// evidence that goes back to rejected without an alert, volatile cycles
// that change nothing, and one material change that reopens it as a
// regress whose event says why.
func TestLifeCycleFromBaselineToMaterialRegress(t *testing.T) {
	for _, backend := range flowBackends(t) {
		t.Run(backend.name, func(t *testing.T) {
			rule := bucketRule()
			source := newScriptedSource(flowAccount)
			source.read(flowAccount, "s3", "eu-west-1", ReadOK)
			f := newFlow(t, backend.provider, newScope("life-cycle"), source, nil, rule)
			id := IssueID(rule.id, docsBucket)

			rule.set(publicFinding(docsBucket, "eu-west-1", SeverityHigh, "*", "1"))
			f.evaluate()
			issue, events := f.get(id)
			if issue.State != StateOpen || issue.SinceKnown || len(events) != 1 || events[0].Kind != EventOpened ||
				events[0].Detail != "existed when first read" {
				t.Fatalf("baseline: %+v, %+v", issue, events)
			}

			f.clock.Add(time.Minute)
			if issue, err := f.actions.Acknowledge(f.ctx, id, "ana@example.com"); err != nil || issue.State != StateAcknowledged {
				t.Fatalf("acknowledge: %+v, %v", issue, err)
			}
			f.clock.Add(time.Minute)
			if issue, err := f.actions.Reject(f.ctx, id, "ana@example.com", "public on purpose: it serves the docs site", ""); err != nil ||
				issue.State != StateRejected || issue.Rejection.SeverityAtReject != SeverityHigh {
				t.Fatalf("reject from acknowledged: %+v, %v", issue, err)
			}
			if summary := Summarize(f.list(storage.AWSIssueQuery{}), Coverage{}); summary.Open != 0 || summary.High != 0 || summary.RejectedCount != 1 {
				t.Fatalf("summary while rejected: %+v", summary)
			}
			rejectedAt := len(f.mustEvents(id))

			// The bucket is gone in a complete read: resolved, rejection kept,
			// out of the Rejected tab and out of every count.
			f.clock.Add(time.Minute)
			rule.set()
			f.evaluate()
			issue, events = f.get(id)
			if issue.State != StateResolved || issue.Rejection == nil || !strings.Contains(events[len(events)-1].Detail, "rejection is kept") {
				t.Fatalf("resolve while rejected: %+v, %+v", issue, events)
			}
			if tab := f.list(storage.AWSIssueQuery{State: string(StateRejected)}); len(tab) != 0 {
				t.Fatalf("a resolved issue is in the Rejected tab: %+v", tab)
			}
			if summary := Summarize(f.list(storage.AWSIssueQuery{}), Coverage{}); summary != (Summary{}) {
				t.Fatalf("summary while resolved: %+v", summary)
			}

			// Found again with the same material and a new volatile fact.
			f.clock.Add(time.Minute)
			rule.set(publicFinding(docsBucket, "eu-west-1", SeverityHigh, "*", "2"))
			f.evaluate()
			issue, events = f.get(id)
			if issue.State != StateRejected || issue.Rejection == nil || alertWorthy(events[rejectedAt:]) != 0 {
				t.Fatalf("same-digest match: %+v, %+v", issue, events[rejectedAt:])
			}
			if tab := f.list(storage.AWSIssueQuery{State: string(StateRejected)}); len(tab) != 1 {
				t.Fatalf("Rejected tab after the match: %+v", tab)
			}

			// Volatile cycles write no event and no version.
			version := issue.Version
			eventCount := len(events)
			for range 20 {
				f.clock.Add(time.Minute)
				rule.set(publicFinding(docsBucket, "eu-west-1", SeverityHigh, "*", "2"))
				f.evaluate()
			}
			issue, events = f.get(id)
			if issue.Version != version || len(events) != eventCount {
				t.Fatalf("unchanged cycles wrote: version %d -> %d, events %d -> %d", version, issue.Version, eventCount, len(events))
			}

			// One material change reopens it once, and the event says why.
			f.clock.Add(time.Minute)
			rule.set(publicFinding(docsBucket, "eu-west-1", SeverityHigh, "*, arn:aws:iam::999900000000:root", "3"))
			f.evaluate()
			for range 5 {
				f.clock.Add(time.Minute)
				f.evaluate()
			}
			issue, events = f.get(id)
			regressed := 0
			for _, event := range events {
				if event.Kind == EventRegressed {
					regressed++
					if !strings.Contains(event.Detail, "material evidence changed") || event.Actor != EvaluatorActor {
						t.Fatalf("regress event does not say why: %+v", event)
					}
				}
			}
			if issue.State != StateRegressed || issue.Rejection != nil || regressed != 1 {
				t.Fatalf("material change: %+v, %+v", issue, events)
			}
			if summary := Summarize(f.list(storage.AWSIssueQuery{}), Coverage{}); summary.Open != 1 || summary.High != 1 || summary.RejectedCount != 0 {
				t.Fatalf("summary after the regress: %+v", summary)
			}
		})
	}
}

func (f *flow) mustEvents(id string) []Event {
	f.t.Helper()
	_, events := f.get(id)
	return events
}

// A review date that has passed reopens the rejection on the next
// evaluation; a rejection with no review date holds through a year of
// daily evaluations on the test clock and writes nothing.
func TestReviewDateOnTheEvaluatorClock(t *testing.T) {
	for _, backend := range flowBackends(t) {
		t.Run(backend.name, func(t *testing.T) {
			rule := bucketRule()
			source := newScriptedSource(flowAccount)
			source.read(flowAccount, "s3", "eu-west-1", ReadOK)
			f := newFlow(t, backend.provider, newScope("review"), source, nil, rule)
			docs, uploads := IssueID(rule.id, docsBucket), IssueID(rule.id, uploadsBucket)
			rule.set(publicFinding(docsBucket, "eu-west-1", SeverityHigh, "*", "1"),
				publicFinding(uploadsBucket, "eu-west-1", SeverityHigh, "*", "1"))
			f.evaluate()
			if _, err := f.actions.Reject(f.ctx, docs, "ana@example.com", "public on purpose: docs site", ""); err != nil {
				t.Fatal(err)
			}
			tomorrow := day(f.clock.Now().AddDate(0, 0, 1))
			if _, err := f.actions.Reject(f.ctx, uploads, "ana@example.com", "live with it until the migration", tomorrow); err != nil {
				t.Fatal(err)
			}
			docsBefore, docsEvents := f.get(docs)

			// Two days on, the review date was yesterday.
			f.clock.Add(48 * time.Hour)
			f.evaluate()
			issue, events := f.get(uploads)
			last := events[len(events)-1]
			if issue.State != StateRegressed || issue.Rejection != nil || last.Kind != EventRegressed ||
				!strings.Contains(last.Detail, "review date "+tomorrow) {
				t.Fatalf("review date yesterday: %+v, last event %+v", issue, last)
			}

			// A year of daily evaluations: the undated rejection holds and
			// nothing is written for it; the lapsed one regressed once.
			for range 365 {
				f.clock.Add(24 * time.Hour)
				f.evaluate()
			}
			issue, events = f.get(docs)
			if issue.State != StateRejected || issue.Version != docsBefore.Version || len(events) != len(docsEvents) {
				t.Fatalf("undated rejection after 365 days: %+v, events %+v", issue, events)
			}
			if _, events := f.get(uploads); alertWorthy(events) != 2 {
				t.Fatalf("the lapsed rejection reopened more than once: %+v", events)
			}
		})
	}
}

// A rejection reopens when the severity rises above the severity it was
// rejected at, also when it rises while the issue is resolved; a fall, and
// a rise back to the rejected severity, never reopen it.
func TestSeverityAboveRejectReopensThroughTheEvaluator(t *testing.T) {
	rule := bucketRule()
	source := newScriptedSource(flowAccount)
	source.read(flowAccount, "s3", "eu-west-1", ReadOK)
	f := newFlow(t, storage.NewMemory(), newScope("severity"), source, nil, rule)
	id := IssueID(rule.id, docsBucket)
	rule.set(publicFinding(docsBucket, "eu-west-1", SeverityHigh, "*", "1"))
	f.evaluate()
	if _, err := f.actions.Reject(f.ctx, id, "ana@example.com", "public on purpose: docs site", ""); err != nil {
		t.Fatal(err)
	}
	for _, severity := range []Severity{SeverityMedium, SeverityLow, SeverityHigh, SeverityMedium, SeverityHigh} {
		f.clock.Add(time.Minute)
		rule.set(publicFinding(docsBucket, "eu-west-1", severity, "*", "1"))
		f.evaluate()
		if issue, events := f.get(id); issue.State != StateRejected || alertWorthy(events) != 1 {
			t.Fatalf("severity %s reopened it: %+v, %+v", severity, issue, events)
		}
	}
	f.clock.Add(time.Minute)
	rule.set(publicFinding(docsBucket, "eu-west-1", SeverityCritical, "*", "1"))
	f.evaluate()
	issue, events := f.get(id)
	if issue.State != StateRegressed || !strings.Contains(events[len(events)-1].Detail, "severity rose from high to critical") {
		t.Fatalf("rise to critical: %+v, %+v", issue, events)
	}

	// Rejected at medium, resolved, then found again at high.
	f.clock.Add(time.Minute)
	rule.set(publicFinding(uploadsBucket, "eu-west-1", SeverityMedium, "*", "1"), publicFinding(docsBucket, "eu-west-1", SeverityCritical, "*", "1"))
	f.evaluate()
	uploads := IssueID(rule.id, uploadsBucket)
	if _, err := f.actions.Reject(f.ctx, uploads, "ana@example.com", "uploads are public on purpose", ""); err != nil {
		t.Fatal(err)
	}
	f.clock.Add(time.Minute)
	rule.set(publicFinding(docsBucket, "eu-west-1", SeverityCritical, "*", "1"))
	f.evaluate()
	if issue, _ := f.get(uploads); issue.State != StateResolved || issue.Rejection == nil {
		t.Fatalf("resolved while rejected: %+v", issue)
	}
	f.clock.Add(time.Minute)
	rule.set(publicFinding(uploadsBucket, "eu-west-1", SeverityHigh, "*", "1"), publicFinding(docsBucket, "eu-west-1", SeverityCritical, "*", "1"))
	f.evaluate()
	issue, events = f.get(uploads)
	if issue.State != StateRegressed || !strings.Contains(events[len(events)-1].Detail, "severity rose from medium to high") {
		t.Fatalf("rise while resolved: %+v, %+v", issue, events)
	}
}
