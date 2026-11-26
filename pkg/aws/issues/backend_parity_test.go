package issues

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/VersusControl/versus-incident/pkg/storage"
	"github.com/VersusControl/versus-incident/pkg/tenancy"
)

// The same evaluations and user actions on memory, file and Postgres
// storage leave identical issues, events and baselines at every step,
// through retention: a resolved issue is kept at 179 days and deleted with
// its events at 181, while rejected and acknowledged issues, and a resolved
// one that keeps its rejection, stay with every event.
func TestBackendsAgreeOnIssuesEventsAndRetention(t *testing.T) {
	backends := flowBackends(t)
	org := uniqueOrg("parity")
	transcripts := make([][]string, len(backends))
	for index, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			rule := bucketRule()
			source := newScriptedSource(flowAccount)
			source.read(flowAccount, "s3", "eu-west-1", ReadOK)
			f := newFlow(t, backend.provider, tenancy.NewOrgScope(org), source, nil, rule)
			bucket := func(name string) Finding {
				return publicFinding("arn:aws:s3:::parity-"+name, "eu-west-1", SeverityHigh, "*", "1")
			}
			id := func(name string) string { return IssueID(rule.id, "arn:aws:s3:::parity-"+name) }
			names := []string{"docs", "logs", "temp", "uploads"}
			record := func(step string, report *Report) {
				t.Helper()
				line := step
				if report != nil {
					line = fmt.Sprintf("%s pruned=%d", step, report.Pruned)
				}
				transcripts[index] = append(transcripts[index], line)
				for _, issue := range f.list(storage.AWSIssueQuery{}) {
					issue.Org = ""
					transcripts[index] = append(transcripts[index], "issue "+canonical(t, issue))
				}
				for _, name := range names {
					events, err := f.evaluator.Store().AWSIssueEvents(f.ctx, f.scope, id(name))
					if err != nil {
						t.Fatal(err)
					}
					transcripts[index] = append(transcripts[index], "events "+name+" "+canonical(t, events))
				}
				baselines, err := f.evaluator.Store().AWSIssueBaselines(f.ctx, f.scope, flowAccount)
				if err != nil {
					t.Fatal(err)
				}
				transcripts[index] = append(transcripts[index], "baselines "+canonical(t, baselines))
			}

			rule.set(bucket("docs"), bucket("logs"), bucket("temp"), bucket("uploads"))
			report := f.evaluate()
			record("baseline", &report)

			f.clock.Add(time.Minute)
			if _, err := f.actions.Acknowledge(f.ctx, id("logs"), "ana@example.com"); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"docs", "uploads"} {
				if _, err := f.actions.Reject(f.ctx, id(name), "ana@example.com", "public on purpose: <b>"+name+"</b> 日本語", ""); err != nil {
					t.Fatal(err)
				}
			}
			record("actions", nil)

			f.clock.Add(time.Hour)
			resolvedAt := f.clock.Now()
			rule.set(bucket("docs"), bucket("logs"))
			report = f.evaluate()
			record("resolve", &report)

			f.clock.Add(resolvedAt.Add(179 * 24 * time.Hour).Sub(f.clock.Now()))
			report = f.evaluate()
			record("day 179", &report)
			if _, _, err := f.actions.Get(f.ctx, id("temp")); err != nil {
				t.Fatalf("a resolved issue was pruned at 179 days: %v", err)
			}

			f.clock.Add(2 * 24 * time.Hour)
			report = f.evaluate()
			record("day 181", &report)
			if _, _, err := f.actions.Get(f.ctx, id("temp")); !errors.Is(err, ErrNotFound) {
				t.Fatalf("a resolved issue outlived retention: %v", err)
			}
			if events, _ := f.evaluator.Store().AWSIssueEvents(f.ctx, f.scope, id("temp")); len(events) != 0 {
				t.Fatalf("events of a pruned issue are kept: %+v", events)
			}
			if report.Pruned != 3 {
				t.Fatalf("pruned %d rows, want the issue and its 2 events", report.Pruned)
			}
			for _, name := range []string{"docs", "logs", "uploads"} {
				issue, events := f.get(id(name))
				if len(events) < 2 {
					t.Fatalf("%s lost events to retention: %+v", name, events)
				}
				if name == "uploads" && (issue.State != StateResolved || issue.Rejection == nil) {
					t.Fatalf("resolved issue with a rejection: %+v", issue)
				}
			}
		})
	}
	for index := 1; index < len(backends); index++ {
		want, got := transcripts[0], transcripts[index]
		for line := range max(len(want), len(got)) {
			var left, right string
			if line < len(want) {
				left = want[line]
			}
			if line < len(got) {
				right = got[line]
			}
			if left != right {
				t.Fatalf("%s differs from %s at line %d:\n%s: %s\n%s: %s", backends[index].name, backends[0].name, line,
					backends[0].name, left, backends[index].name, right)
			}
		}
	}
}

// canonical renders a value as JSON with sorted keys.
func canonical(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
